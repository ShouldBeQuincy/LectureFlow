// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/colab"
	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/dirperm"
	"lectureflow/internal/fetch"
	"lectureflow/internal/glossary"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/selfupdate"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// App is the fully wired application: store, scheduler with all stages, and
// the HTTP server. main and the end-to-end tests build it the same way.
type App struct {
	Cfg       *config.Config
	Store     db.Store
	Settings  *settings.Service
	Resources *resources.Service
	Sched     *sched.Scheduler
	Server    *api.Server
	Hub       *api.Hub
	// Devices is the transcription device registry (SPEC-v2 I6): the
	// DeviceSource of the settings and of the scheduler.
	Devices *devices.Registry
	// keepAwake is the power request held while transcription runs (power
	// management, lead ruling 9.5); nil in test mode or when it could not
	// be created.
	keepAwake *devices.KeepAwake
	// Components is the component manager (SPEC-v3 V3); nil when the
	// built-in manifest is missing or the manager could not open (the
	// service runs without it; the component endpoints answer 503).
	Components *components.Manager
	// Downloads is the browser extension's download queue (SPEC-ext,
	// downloads.go); nil when it could not be built.
	Downloads *fetch.Queue
	// SelfUpdate is the in-app update (SPEC-ext 4, internal/selfupdate);
	// it reports "not configured" until the public key is compiled in.
	SelfUpdate *selfupdate.Updater

	// redetect asks the detection goroutine for another device detection
	// (a component changed the backend environments); compsUnsub ends that
	// subscription.
	redetect   chan struct{}
	compsUnsub func()

	// Colab is the Colab remote transcription backend (SPEC-v3 V14); nil
	// when it could not be built. colabCancel / colabDone end its loop.
	Colab       *colabRemote
	colabCancel context.CancelFunc
	colabDone   chan struct{}

	detectCancel context.CancelFunc
	detectDone   chan struct{}
	// manifestDone is closed when the start's manifest check ended (nil:
	// none was started).
	manifestDone chan struct{}
	closeOnce    sync.Once
	// llm is the LLM client; its call log keeps a file open while requests
	// run (T-PERF-08), closed on shutdown.
	llm *llm.HTTPClient
}

// AppOptions lets tests adjust the scheduler (short intervals), the device
// registry (fake probes) and the HTTP client used for LLM requests.
type AppOptions struct {
	Sched      func(*sched.Options)
	Devices    func(*devices.RegistryOptions)
	HTTPClient *http.Client
	// Desktop is non-nil in desktop mode (SPEC-v3 V1.2): handed to the HTTP
	// server, and the legacy config.toml settings are not imported (the
	// onboarding sets up the providers; NOTES v3 v1-go).
	Desktop *api.DesktopOptions
	// Version is the application version (api.Deps.Version).
	Version string
	// Stages are registered after the real stages and replace those of the
	// same name (tests: a stage that blocks until cancelled).
	Stages []sched.Stage
	// Components adjusts the component manager's options (tests: platform,
	// fake HTTP); the manager is opened only when cfg.ComponentsManifest is
	// set.
	Components func(*components.Options)
	// Colab adjusts the Colab backend's options (tests: fake services).
	Colab func(*colab.Options)
	// Portable is the Windows portable build (-portable): the in-app update
	// replaces the executable instead of running an installer.
	Portable bool
	// NoSelfUpdate (-no-self-update): this copy of the desktop app cannot
	// replace itself; the in-app update installs nothing here
	// (selfupdate.Options.NoInstall).
	NoSelfUpdate bool
	// SelfUpdate adjusts the in-app update's options (tests: fake sources).
	SelfUpdate func(*selfupdate.Options)
}

// buildApp opens the database, runs startup recovery (SPEC §4.7) and wires
// every component. It does not start the scheduler (Start does, and runs
// the first device detection).
func buildApp(ctx context.Context, cfg *config.Config, o AppOptions) (*App, error) {
	state.SetDataDir(cfg.DataDir) // netpath-datadir: stored paths inside it are opened even on a share
	// private from the start when created here (sec-update M2)
	if _, err := dirperm.MkdirPrivate(cfg.DataDir); err != nil {
		return nil, err
	}
	st, err := db.Open(filepath.Join(cfg.DataDir, "lectureflow.db"))
	if err != nil {
		return nil, dbFailure(cfg.DataDir, "open db", err) // F5: coded when the cause is known
	}
	if err := state.Recover(ctx, st, cfg.DataDir); err != nil {
		st.Close()
		return nil, dbFailure(cfg.DataDir, "startup recovery", err)
	}
	// After a data directory move: no Colab credential stays in the old
	// directory's database (v3 Gate round 2).
	scrubOldDataDir(ctx, st, cfg.DataDir)
	scanNetworkPaths(ctx, st) // security review 2, M3: paths naming another computer are never opened

	// Settings live in the database since v1.1 (A5.1): the first start
	// imports the legacy config.toml fields once. Not in desktop mode
	// (v3-brief §1): the defaults of a missing config.toml would import a
	// DeepSeek provider keyed from the environment; the onboarding sets up
	// the providers instead.
	var set *settings.Service
	if o.Desktop != nil {
		set = settings.New(st, cfg)
		err = set.Load(ctx)
	} else {
		set, err = settings.Open(ctx, st, cfg)
	}
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("settings: %w", err)
	}
	// SPEC-v2 I6 (phase 3b): the device rows, the one-time migration of the
	// legacy ASR settings into cuda:0 / fw:cpu. The first detection runs in
	// the background once the scheduler has started (Start).
	var regOpts devices.RegistryOptions
	if o.Devices != nil {
		o.Devices(&regOpts)
	}
	reg := devices.NewRegistry(st, cfg, cfg.DataDir, regOpts)
	if err := reg.Load(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("asr devices: %w", err)
	}
	if migrated, err := reg.MigrateLegacy(ctx, set.ASR()); err != nil {
		slog.Warn("asr devices: legacy settings not migrated (tried again on the next start)", "err", err)
	} else if migrated {
		slog.Info("asr devices: legacy ASR settings migrated", "devices", len(reg.List()))
	}
	// Lectures started by v1 get the snapshot of the settings they ran with
	// (one-time, NOTES.md "v1.1 冻结与裁决" 3): still the legacy form, the
	// device source is installed only after it.
	if cur, err := set.CurrentSnapshots(); err != nil {
		slog.Warn("snapshot backfill skipped: roles not configured", "err", err)
	} else if n, err := state.BackfillSnapshots(ctx, st, cur); err != nil {
		st.Close()
		return nil, fmt.Errorf("snapshot backfill: %w", err)
	} else if n > 0 {
		slog.Info("v1.1 snapshot backfill", "jobs", n)
	}
	set.SetDeviceSource(reg) // launches write device snapshots from now on
	if n, err := migrateV1Usage(ctx, st, set); err != nil {
		slog.Warn("v1 usage import into usage_daily failed", "err", err)
	} else if n > 0 {
		slog.Info("imported v1 token usage into usage_daily", "rows", n)
	}
	applyV2Defaults(ctx, st, set)

	hub := api.NewHub()
	pool := sched.NewPoolWithSettings(st, set, sched.PoolOptions{})
	// The stages' self-test (stages.Deps.DeviceTest) exists only once the
	// stages are built below, after the scheduler; set before Start.
	var deviceTest sched.DeviceTestFunc
	// The power reading, with the development simulation
	// (LECTUREFLOW_DEV_POWER, power-brief §3.1) when the variable is set.
	power := devices.NewPowerReader(os.Getenv)
	if cfg.ASR.FakeResult != "" && os.Getenv(devices.DevPowerEnv) == "" {
		// Test mode never reads the machine's power state: on a laptop on
		// battery the fake CUDA jobs would wait for the adapter. The
		// development simulation still applies (end-to-end tests drive it).
		power = func() devices.Power { return devices.Power{} }
	}
	// Keep the machine out of idle standby while transcription runs (lead
	// addition after I12): not in test mode (the fake runs take
	// milliseconds and no test may hold a real power request).
	var keepAwake *devices.KeepAwake
	if cfg.ASR.FakeResult == "" {
		ka, err := devices.NewKeepAwake(devices.KeepAwakeReason)
		if err != nil {
			slog.Warn("power request not available: the machine may sleep while transcribing", "err", err)
		} else {
			keepAwake = ka
		}
	}
	opts := sched.Options{
		Store:      st,
		Config:     cfg,
		Settings:   set,
		Stages:     map[string]sched.Stage{},
		Events:     hub,
		DataDir:    cfg.DataDir,
		Pool:       pool,
		Devices:    reg,
		Power:      power,
		MemAvailMB: devices.AvailableMemoryMB,
		DeviceTest: func(ctx context.Context, dev model.ASRDevice, env sched.DeviceTestEnv) (model.DeviceBenchmark, error) {
			return deviceTest(ctx, dev, env)
		},
	}
	// The Colab backend (SPEC-v3 V14): colab:remote of the device table and
	// the scheduler's remote device.
	cb := newColab(cfg, st, func() { hub.Changed("colab", "") }, o.Colab)
	soon := newDownloadSoon() // colab-batch: downloads about to become lectures (the queue comes below)
	if cb != nil {
		opts.Remote = cb
		opts.RemoteSoon = soon.eta
		opts.OnRemote = func(ctx context.Context, present bool, row model.ASRDevice) {
			if err := reg.SetRemote(ctx, present, row); err != nil && ctx.Err() == nil {
				slog.Warn("colab: updating the device table", "err", err)
			}
		}
	}
	if keepAwake != nil {
		opts.KeepAwake = keepAwakeHook(ctx, st, keepAwake.Set)
	}
	if o.Sched != nil {
		o.Sched(&opts)
	}
	s, err := sched.New(opts)
	if err != nil {
		if keepAwake != nil {
			keepAwake.Close()
		}
		st.Close()
		return nil, err
	}
	// A device row, its presence or the detection changed: SSE and a scan.
	reg.OnChange(func() {
		hub.Changed("devices", "")
		s.Kick()
	})
	wireSSH(ctx, cfg, st, reg, s, hub, o.Version) // ssh-mvp: the SSH inference servers (ssh.go)

	// The component manager (SPEC-v3 V3, step 5): its environments and
	// models take precedence over the configured paths from now on
	// (cfg.Components, read by the stages, the device detection and the
	// memory estimates; set before anything runs). Its update self-test
	// (V8) runs the stages' worker check; stageDeps is set below.
	var stageDeps atomic.Pointer[stages.Deps]
	// What uses a component (v3 Gate round 1 item 5, round 2): the
	// scheduler's asr runs, device self-tests and material extraction, the
	// update self-tests, the device detection's probes, the diagnostics
	// checks and the syllabus PDF extraction (added once the server exists).
	use := newComponentUse(cfg)
	use.setWork(s.WorkerUses)
	if cb != nil {
		use.setLogin(cb.LoginActive)
	}
	use.addPythons(probePythons(reg.RunningPythons))
	comps := openComponents(ctx, cfg, st, o, use.wrapSelfTest(func(ctx context.Context, req components.SelfTestRequest) error {
		d := stageDeps.Load()
		if d == nil {
			return errors.New("the stages are not ready")
		}
		return d.ComponentSelfTest(ctx, componentCheck(req, reg.LastDetection().CUDAName != ""))
	}), use.InUse)
	redetect := make(chan struct{}, 1)
	var compsUnsub func()
	if comps != nil {
		use.setRoot(comps.Root())
		cfg.Components = comps
		compsUnsub = subscribeRedetect(comps, redetect)
	}

	// One llm client for the stages and the syllabus formatter: the pool is
	// both the API pool and the usage_daily recorder (A10.3).
	llmc := llm.NewWithSettings(set, s.Pool(), s.Pool(), "", o.HTTPClient)
	d := stages.Deps{
		Store:    st,
		Cfg:      cfg,
		LLM:      llmc,
		DataDir:  cfg.DataDir,
		Events:   hub,
		Settings: set,
		Invalidate: func(ctx context.Context, courseID string, cause state.StaleCause) error {
			return s.InvalidateCourseWith(ctx, courseID, cause)
		},
		// perf-fix: the total VRAM does not change while the program runs;
		// nvidia-smi runs only while no reading has succeeded yet
		GPUTotalMB: s.GPUTotalMB,
	}
	for _, stg := range stages.All(d) {
		s.Register(stg)
	}
	for _, stg := range o.Stages {
		s.Register(stg)
	}
	deviceTest = d.DeviceTest
	stageDeps.Store(&d)

	// Resource watcher (A10): samples only while an SSE client is connected;
	// the server reports the client count and balance refreshes.
	resDeps := resources.Deps{
		Store:     st,
		Providers: set.Providers,
		ASR:       set.ASR,
		Slots:     s.GPUSlots,
		Inflight: func(id string) (int, int) {
			in, mx, _ := s.Pool().ProviderStatus(id)
			return in, mx
		},
		NvidiaSMI: cfg.NvidiaSMI,
		// T046: no nvidia-smi runs without an NVIDIA GPU
		HasNVIDIA: hasNVIDIAFunc(st, reg.LastDetection),
		Holidays:  set.HolidaySet, // v2 P12.2: the holiday data in effect
		OnSample:  hub.Resources,
		// SPEC-v2 I10 device cards
		ASRDevices:     reg.List,
		DeviceStatuses: s.DeviceStatuses,
		Detection:      reg.LastDetection,
		Power:          s.Power,
		// Power management: the policy's state (resource bar); SystemStats
		// stays nil, so the resource service reads the machine (task P4).
		PowerStatus: s.PowerStatus,
	}
	if cb != nil {
		resDeps.Remote = api.RemoteSnapshot(cb.Backend, s) // SPEC-v3 V14.5: the remote area
	}
	resDeps.RemoteCards = sshCards(s) // ssh-1b: the SSH servers' cards (ssh.go; nil while they are off)
	res := resources.New(resDeps)
	res.Start(ctx)
	if cb != nil {
		cb.SetViewers(res.Clients) // colab-obs-core: the runtime's resources only while a front end watches
	}

	// The browser extension's download queue (SPEC-ext, downloads.go).
	dl, err := newDownloadQueue(cfg, st, hub)
	if err != nil {
		slog.Warn("download queue unavailable", "err", err)
	}
	soon.setQueue(dl)
	deps := api.Deps{
		Store:     st,
		Sched:     s,
		Cfg:       cfg,
		DataDir:   cfg.DataDir,
		Hub:       hub,
		Settings:  set,
		Resources: res,
		LLM:       llmc,
		Devices:   reg,
		Desktop:   o.Desktop,
		Version:   o.Version,
	}
	if comps != nil {
		deps.Components = comps // a nil *Manager must not become a non-nil field value
	}
	if cb != nil {
		deps.Colab = cb.Backend
	}
	if dl != nil {
		deps.Downloads = dl // a nil *Queue must not become a non-nil interface
	}
	// The in-app update (SPEC-ext 4): checks GitHub Releases at most once a
	// day, only in desktop mode, and only once the public key is set.
	suOpts := selfupdate.Options{DataDir: cfg.DataDir, Version: o.Version, KV: st,
		Desktop: o.Desktop != nil, Portable: o.Portable, NoInstall: o.NoSelfUpdate, Log: slog.Default()}
	if o.Desktop != nil {
		suOpts.InstallDir = o.Desktop.InstallDir // security audit M-4
	}
	if dl != nil {
		suOpts.Downloader = updateDownloader(dl)
	}
	if o.SelfUpdate != nil {
		o.SelfUpdate(&suOpts)
	}
	upd := selfupdate.New(suOpts)
	upd.Startup(ctx)
	go upd.Run(ctx)
	deps.SelfUpdate = upd
	srv := api.New(deps)
	if dl != nil {
		// Before the extension's endpoint serves (runMain starts it later):
		// tasks left unfinished by the last run become needs_refresh.
		if err := startDownloads(ctx, dl, srv); err != nil {
			slog.Warn("download queue did not start", "err", err)
		}
	}
	use.addPythons(serverPythons(srv.RunningPythons))
	if o.Desktop != nil {
		// The data directory move (SPEC-v3 V1.3): no new dispatches while
		// it copies, and the copied component environments get their
		// absolute paths fixed (SetRelocateHooks replaces both at once).
		srv.SetRelocateHooks(api.RelocateHooks{
			FixComponents: func(oldRoot, newRoot string) error { return components.Relocated(oldRoot, newRoot) },
			PauseDispatch: func() func() {
				resume := s.PauseDispatch()
				// The call log's open file would keep the old folder busy
				// on Windows (T-PERF-08); the next request opens it again.
				if err := llmc.CloseLogs(); err != nil {
					slog.Warn("llm logs not closed", "err", err)
				}
				return resume
			},
		})
	}
	return &App{Cfg: cfg, Store: st, Settings: set, Sched: s, Server: srv, Hub: hub, Resources: res, Devices: reg,
		keepAwake: keepAwake, Components: comps, redetect: redetect, compsUnsub: compsUnsub, Colab: cb, Downloads: dl, SelfUpdate: upd,
		llm: llmc}, nil
}

// openComponents opens the component manager of cfg.DataDir (both modes),
// or returns nil — logged, never fatal — when cfg.ComponentsManifest is
// empty (no built-in manifest: tests, a build without it) or the manager
// cannot open.
func openComponents(ctx context.Context, cfg *config.Config, st db.Store, o AppOptions,
	selfTest func(context.Context, components.SelfTestRequest) error, inUse func(string) (bool, string)) *components.Manager {
	if cfg.ComponentsManifest == "" {
		slog.Info("components: no built-in manifest; running without the component manager")
		return nil
	}
	co := componentOptions(ctx, cfg, st, o.Version)
	co.SelfTest = selfTest
	co.InUse = inUse
	if o.Components != nil {
		o.Components(&co)
	}
	m, err := components.New(co)
	if err != nil {
		slog.Warn("components: the component manager is not available", "manifest", co.BuiltinManifest, "err", err)
		return nil
	}
	if m.StateRecovered() {
		slog.Warn("components: state.json was damaged and has been rebuilt from the installation receipts")
	}
	slog.Info("components: manager ready", "dir", m.Root(), "platform", m.Platform(), "manifest", co.BuiltinManifest,
		"uv", co.UV, "update_check", co.ManifestURL != "")
	return m
}

// Start starts the scheduler, then the first device detection in the
// background (nvidia-smi and the OpenVINO probe can take seconds; jobs of
// devices seen before start meanwhile, SPEC-v2 I6). Close waits for it.
func (a *App) Start(ctx context.Context) error {
	if err := a.Sched.Start(ctx); err != nil {
		return err
	}
	if a.Colab != nil {
		// SPEC-v3 V14.3 step 5: leftovers of the last run are cleaned up in
		// the background; then the backend's 30-second Tick loop.
		cctx, ccancel := context.WithCancel(ctx)
		a.colabCancel, a.colabDone = ccancel, make(chan struct{})
		a.Colab.start(cctx, a.Sched, a.colabDone)
	}
	dctx, cancel := context.WithCancel(ctx)
	a.detectCancel, a.detectDone = cancel, make(chan struct{})
	go func() {
		defer close(a.detectDone)
		a.detect(dctx)
		// A component installed, updated or removed a backend environment
		// (subscribeRedetect): detect again, so the device table follows.
		for {
			select {
			case <-dctx.Done():
				return
			case <-a.redetect:
				a.detect(dctx)
			}
		}
	}()
	if a.Components != nil && a.Cfg.ComponentsManifestURL != "" {
		// A newer component manifest (SPEC-v3 V3.2, V8): only when a URL
		// is configured (the project has no release address yet).
		a.manifestDone = make(chan struct{})
		go func() {
			defer close(a.manifestDone)
			mctx, mcancel := context.WithTimeout(dctx, time.Minute)
			defer mcancel()
			if newer, err := a.Components.CheckManifest(mctx); err != nil {
				if dctx.Err() == nil {
					slog.Warn("components: manifest check failed", "err", err)
				}
			} else if newer {
				slog.Info("components: a newer component manifest was adopted")
			}
		}()
	}
	return nil
}

// detect runs one device detection and logs its outcome.
func (a *App) detect(ctx context.Context) {
	d, newIDs, err := a.Devices.Detect(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("asr device detection failed", "err", err)
		}
		return
	}
	slog.Info("asr devices detected", "cuda", d.CUDAName, "openvino", d.OV != nil, "mlx", d.MLX != "",
		"new", newIDs, "devices", len(a.Devices.List()))
}

// applyV2Defaults runs the one-time v2 startup steps after the migrations
// and before the scheduler starts. None may stop the application: a
// failure is logged and the step is tried again on the next start.
//   - appendix D1 (lead ruling 12, phase 2 ruling 11): default the roles
//     that are not configured yet (glossary_janitor; chapters, organizer);
//   - P12.2/D3, P12.5 (llm-p2): the DeepSeek tariff preset for existing
//     providers and the estimated cost of usage rows from before v2;
//   - G2 (lead ruling 13): common_word of the terms that existed before
//     migration 005 (kv-guarded, no tier or version change);
//   - P14.3 (phase 2 ruling 5): stale_reason texts become JSON (kv-guarded;
//     until then every reader also understands the texts);
//   - P13.3.1: original_filename of the lectures from before phase 2;
//   - lecture kinds (review item 3): a renumbering notice for the courses
//     whose exported citations the past-lecture rule renumbered (kv-guarded;
//     after the migrations, before the scheduler can set any kind).
func applyV2Defaults(ctx context.Context, st db.Store, set *settings.Service) {
	if filled, err := set.ApplyRoleDefaults(ctx); err != nil {
		slog.Warn("v2 role defaults not applied", "err", err)
	} else if len(filled) > 0 {
		slog.Info("v2 role defaults applied", "roles", filled)
	}
	if names, rows, err := set.ApplyPhase2Defaults(ctx); err != nil {
		slog.Warn("v2 phase 2 settings data not applied", "err", err)
	} else if len(names) > 0 || rows > 0 {
		slog.Info("v2 phase 2 settings data", "tariff_preset", names, "usage_rows_priced", rows)
	}
	if n, err := glossary.BackfillCommonWords(ctx, st); err != nil {
		slog.Warn("v2 common_word backfill failed", "err", err)
	} else if n > 0 {
		slog.Info("v2 common_word backfill", "terms", n)
	}
	if n, err := state.ConvertStaleReasons(ctx, st); err != nil {
		slog.Warn("v2 stale_reason conversion failed", "err", err)
	} else if n > 0 {
		slog.Info("v2 stale_reason converted to codes", "jobs", n)
	}
	if n, err := backfillOriginalFilenames(ctx, st); err != nil {
		slog.Warn("v2 original_filename backfill failed", "err", err)
	} else if n > 0 {
		slog.Info("v2 original_filename backfill", "lectures", n)
	}
	if n, err := checkPastRuleRenumbering(ctx, st, time.Now()); err != nil {
		slog.Warn("past-lecture renumbering check failed", "err", err)
	} else if n > 0 {
		slog.Info("past-lecture rule renumbered exported citations: notices recorded", "courses", n)
	}
}

// Close stops the scheduler and releases resources (running jobs are left
// running in the DB; the next start recovers them, SPEC §4.7). Without a
// time limit; Close after Shutdown does nothing.
func (a *App) Close() {
	a.Shutdown(time.Time{}, nil)
}

// shutdownReserve is the part of the graceful stop kept for closing HTTP
// and the database after the scheduler.
const shutdownReserve = 2 * time.Second

// Shutdown is the graceful stop (SPEC-v3 V1.1, v3-brief §1), in order:
// stop dispatching and cancel the running jobs (their workers are killed)
// and the device detection (and the start's manifest check), waiting for
// them, while the download queue stops its downloads and aria2c at the
// same time; stop the component manager (its running task stops where it is);
// release the power request;
// wait for the pending invalidations; cancel the startup checks and wait
// for them; stop the resource watcher; close the
// SSE hub, then the HTTP servers srvs (nil entries skipped), which waits for
// the handlers in progress and so for their database transactions; close the
// database (which waits for the statements in progress). deadline (zero:
// none) bounds the waits for the scheduler and HTTP; the caller's
// watchdog bounds the whole. Interrupted jobs stay running in the
// database: startup recovery (§4.7) handles them on the next start.
// Only the first call does anything.
func (a *App) Shutdown(deadline time.Time, srvs ...*http.Server) {
	a.closeOnce.Do(func() { a.shutdown(deadline, srvs) })
}

func (a *App) shutdown(deadline time.Time, srvs []*http.Server) {
	remain := func(reserve time.Duration) time.Duration {
		if deadline.IsZero() {
			return 24 * time.Hour
		}
		return max(time.Until(deadline)-reserve, 100*time.Millisecond)
	}
	if a.detectCancel != nil {
		a.detectCancel()
	}
	if a.compsUnsub != nil {
		a.compsUnsub()
	}
	// Downloads stop where they are, alongside the scheduler (M1
	// acceptance: one after the other could pass the 10 s of V1.1); the
	// next start marks them needs_refresh (the cookies were only in
	// memory).
	var stopDL func(time.Duration) bool
	if a.Downloads != nil {
		stopDL = alongside(func() { stopDownloads(a.Downloads, remain(shutdownReserve)) })
	}
	if !a.Sched.Shutdown(remain(shutdownReserve)) {
		slog.Warn("graceful stop: running jobs did not end in time; startup recovery handles them")
	}
	closeSSH(a.Sched) // ssh-mvp: after the runs ended (ssh.go)
	// The call log's open file (T-PERF-08; CloseLogs is nil-safe).
	if err := a.llm.CloseLogs(); err != nil {
		slog.Warn("graceful stop: llm logs not closed", "err", err)
	}
	if a.SelfUpdate != nil {
		a.SelfUpdate.Close() // a running update download stops where it is
	}
	if stopDL != nil && !stopDL(remain(shutdownReserve)) {
		slog.Warn("graceful stop: did not end in time", "what", "downloads")
	}
	for _, w := range []struct {
		done chan struct{}
		what string
	}{{a.detectDone, "device detection"}, {a.manifestDone, "component manifest check"}} {
		if w.done == nil {
			continue
		}
		select {
		case <-w.done:
		case <-time.After(remain(shutdownReserve)):
			slog.Warn("graceful stop: did not end in time", "what", w.what)
		}
	}
	if a.Colab != nil {
		// SPEC-v3 V14: the remote runtime is released and a "session"
		// credential deleted (the lecture that ran there was interrupted with
		// the scheduler and is recovered on the next start).
		a.Colab.stop(a.colabCancel, a.colabDone, remain(shutdownReserve))
	}
	if a.Components != nil {
		// The component manager: a running download or uv step stops where
		// it is (partial downloads stay for the next start).
		done := make(chan error, 1)
		go func() { done <- a.Components.Close() }()
		select {
		case err := <-done:
			if err != nil {
				slog.Warn("graceful stop: component manager", "err", err)
			}
		case <-time.After(remain(shutdownReserve)):
			slog.Warn("graceful stop: the component manager did not stop in time")
		}
	}
	if a.keepAwake != nil {
		if err := a.keepAwake.Close(); err != nil {
			slog.Warn("power request close", "err", err)
		}
	}
	a.Server.WaitInvalidations()
	if !a.Server.StopStartupChecks(remain(shutdownReserve)) {
		slog.Warn("graceful stop: did not end in time", "what", "startup checks")
	}
	a.Resources.Stop()
	a.Hub.Close() // ends SSE streams so the HTTP shutdown does not wait for them
	var active []*http.Server
	for _, srv := range srvs {
		if srv != nil {
			active = append(active, srv)
		}
	}
	if len(active) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), remain(0))
		var wg sync.WaitGroup
		for _, srv := range active {
			wg.Add(1)
			go func(s *http.Server) {
				defer wg.Done()
				if err := s.Shutdown(ctx); err != nil {
					slog.Warn("graceful stop: HTTP requests still in progress; closing", "err", err)
					s.Close()
				}
			}(srv)
		}
		wg.Wait()
		cancel()
	}
	a.Store.Close()
}

// alongside runs fn in the background; the returned wait reports whether
// it ended within d (graceful-stop steps that may run at the same time).
func alongside(fn func()) (wait func(d time.Duration) bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return func(d time.Duration) bool {
		select {
		case <-done:
			return true
		case <-time.After(d):
			return false
		}
	}
}

// keepAwakeHook is sched.Options.KeepAwake over set (devices.KeepAwake.Set):
// the scheduler calls it for the pipeline's asr runs and the device
// self-tests alike (V12.2 acceptance issue 6). app.prevent_sleep off
// (SPEC-v3 V1.1, v3-brief §2) means the request is not taken: the setting
// governs this request as well as the shell's. A release is always passed
// on.
func keepAwakeHook(ctx context.Context, st db.Store, set func(on bool) error) func(on bool) {
	return func(on bool) {
		if on && !preventSleep(ctx, st) {
			slog.Info("power request not taken: prevent_sleep is off")
			return
		}
		if err := set(on); err != nil {
			slog.Warn("power request", "on", on, "err", err)
		} else {
			slog.Info("power request", "held", on)
		}
	}
}

// preventSleep reports whether the power request may be taken (SPEC-v3
// V1.1, V11): true unless kv model.KVAppPreventSleep is "0" ("false" is
// accepted too). A read error keeps the previous
// behaviour (on). Called by the scheduler outside any Store transaction.
func preventSleep(ctx context.Context, st db.Store) bool {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	v, ok, err := st.GetKV(rctx, model.KVAppPreventSleep)
	if err != nil || !ok {
		return true
	}
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "0", "false":
		return false
	}
	return true
}
