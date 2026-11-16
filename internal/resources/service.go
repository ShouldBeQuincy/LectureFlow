// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/tariff"
)

// Window and HistoryMax are the sparkline history of A10.1: the last 60 s,
// one point every 2 s.
const (
	Window     = 60 * time.Second
	HistoryMax = 30
)

// TempWarnC is the A10.1 warning threshold.
const TempWarnC = 85

// DefaultInterval is the A10 sampling interval.
const DefaultInterval = 2 * time.Second

// NVIDIARecheck is how long the answer of Deps.HasNVIDIA is kept before it
// is asked again (T046); a new device detection (Deps.Detection's At)
// asks again at once.
const NVIDIARecheck = 5 * time.Minute

// nvidiaAskTimeout bounds one Deps.HasNVIDIA call (a kv read).
const nvidiaAskTimeout = 5 * time.Second

// balanceTimeout bounds one balance query even with a client without timeout.
const balanceTimeout = 15 * time.Second

// Deps are the collaborators of the resource watcher, injected so that the
// package does not depend on the scheduler, the settings service or the API
// (it uses the pure price helpers of package settings only).
type Deps struct {
	Store db.Store // usage_daily
	// Providers returns settings.Providers (with keys, for the balance
	// query; a key is never logged or returned).
	Providers func() []model.Provider
	ASR       func() model.ASRSettings // the Backend text
	// Slots reports the scheduler's ASR slots and the GPU pause switch.
	Slots func() (used, max int, paused bool)
	// Inflight reports a provider's API pool usage; max 0 falls back to
	// provider.max_concurrency.
	Inflight func(providerID string) (inflight, max int)
	// Holidays returns the holiday data in effect (settings.HolidaySet) for
	// the tariff states (v2 P12.4). nil: kv tariff.holidays_cn from Store
	// when set and valid, else the embedded file.
	Holidays  func() tariff.HolidaySet
	NvidiaSMI string // executable; "" = "nvidia-smi"
	// HasNVIDIA reports whether this computer has an NVIDIA GPU (T046,
	// SPEC-ext: what the machine cannot have is neither shown nor
	// checked). false: sampling does not run nvidia-smi and the GPU area
	// stays unavailable without an error. It is asked at the first sample,
	// again after NVIDIARecheck and whenever Detection's At changes (a
	// driver installed later is picked up). nil = always true (as before).
	// An error means the answer could not be had (r4 D10): it is not kept,
	// the last answer stands (nothing known yet: false for this sample) and the next sample
	// asks again. It is asked with a context of its own (not cancelled with
	// the request that triggered the sample), bounded by nvidiaAskTimeout.
	HasNVIDIA func(ctx context.Context) (bool, error)
	// RunCmd runs a command and returns its stdout; nil = os/exec.
	RunCmd func(ctx context.Context, name string, args ...string) (stdout string, err error)
	HTTP   *http.Client // balance queries; nil = a client with a 10 s timeout
	Now    func() time.Time
	// Interval is the sampling interval; 0 = DefaultInterval (2 s).
	Interval time.Duration
	// OnSample is called from the sampling goroutine after every background
	// sample (the API broadcasts it as SSE "event: resources"). It is not
	// called for the synchronous sample taken by Snapshot. Nothing is locked
	// while it runs, so it may call back into the Service.
	OnSample func(Snapshot)

	// SPEC-v2 I10 device cards (phase 3b freeze; bodies: task E, wiring in
	// cmd: task C). All nil = no device cards (Snapshot.Devices and Power
	// omitted), as before 3b.
	ASRDevices     func() []model.ASRDevice    // devices.Registry.List
	DeviceStatuses func() []sched.DeviceStatus // sched.Scheduler.DeviceStatuses
	Detection      func() devices.Detection    // devices.Registry.LastDetection (LUIDs for the PDH counters)
	Power          func() devices.Power        // sched.Scheduler.Power

	// Power management (v2-step20 freeze; bodies: task P4, wiring in cmd:
	// task P1). PowerStatus is sched.Scheduler.PowerStatus (Snapshot.Policy,
	// the Active / HoldsSuspended flags of the device rows); nil = no
	// Policy. SystemStats reads the upper area's values (Snapshot.System);
	// nil = the Windows readers (PDH, GlobalMemoryStatusEx; unknown
	// elsewhere) — tests inject it.
	PowerStatus func() sched.PowerStatus
	SystemStats func() System

	// Remote reports the Colab backend's state (SPEC-v3 V14.5); ok false
	// = not configured or switched off (Snapshot.Remote omitted). nil = no
	// remote area. It never contacts Colab.
	Remote func(ctx context.Context) (colab.Status, bool)
	// RemoteCards are the SSH inference servers' cards (ssh-1b:
	// sshremote.Manager.Cards; nil = none, the SSH servers are off). They
	// follow Colab's card in Snapshot.Remotes; a running lecture without a
	// title gets the one the device cards show.
	RemoteCards func(ctx context.Context) []remote.Card
}

// Service samples the GPU while front ends are connected (A10), caches
// provider balances in memory (A10.3) and builds Snapshots.
type Service struct {
	d        Deps
	interval time.Duration
	run      func(ctx context.Context, name string, args ...string) (string, error)
	client   *http.Client
	exe      string
	// newTicker is replaced in tests.
	newTicker func(time.Duration) (<-chan time.Time, func())
	// newEngines opens the PDH reader of the Intel device cards (replaced in
	// tests); engines, engErr and engRetryAt are guarded by sampleMu.
	newEngines func() (engineReader, error)
	engines    engineReader
	engErr     string
	engRetryAt time.Time
	// newSystem opens the PDH reader of the upper area and memStatus reads
	// GlobalMemoryStatusEx (both replaced in tests); sysR, sysErr, memErr
	// and sysRetryAt are guarded by sampleMu.
	newSystem  func() (systemReader, error)
	memStatus  memStatusFunc
	sysR       systemReader
	sysErr     string
	memErr     string
	sysRetryAt time.Time
	// platform names the upper area's readings this system cannot give and
	// whether its memory is unified (System.Unsupported, UnifiedMemory;
	// replaced in tests). appleErr (sampleMu) is the last ioreg failure
	// logged (apple.go).
	platform func() (unsupported []string, unified bool)
	appleErr string
	// nvKnown, nvHas, nvAt and nvDetAt cache Deps.HasNVIDIA (sampleMu):
	// the answer, when it was asked and the detection time it saw.
	nvKnown bool
	nvHas   bool
	nvAt    time.Time
	nvDetAt time.Time

	ctx     context.Context // cancelled by Stop (or Start's context)
	cancel  context.CancelFunc
	wake    chan struct{}
	loopWG  sync.WaitGroup
	fetchWG sync.WaitGroup

	sampleMu sync.Mutex // serialises nvidia-smi runs

	mu          sync.Mutex
	started     bool
	stopped     bool
	clients     int
	sampled     bool      // at least one sample was taken
	lastAt      time.Time // when the last sample was taken
	gpuOK       bool
	gpu         Sample
	gpuErr      string
	lastTotalMB int // last known total VRAM, for the auto compute type
	hist        []histPoint
	bal         map[string]*balanceEntry
	usageErr    string
	// engUtil is the last PDH reading per LUID key (nil value: unknown);
	// engHist its 60 s history.
	engUtil map[string]*int
	engHist map[string][]histPoint
	// engMem is the last shared GPU memory reading per LUID key in MB (nil
	// value or no entry: unknown); sys the upper area's last reading and
	// memTotalMB the last known physical memory (the Intel cards' total).
	engMem     map[string]*int
	sys        System
	memTotalMB *int
	// appleUtil, appleMem (MB) and appleHist are the Apple GPU card's last
	// ioreg reading and utilization history (mac-opti M6, apple.go).
	appleUtil, appleMem *int
	appleHist           []histPoint
}

type histPoint struct {
	at         time.Time
	util, temp *int
}

// New builds a Service. Nothing runs until Start; Snapshot works without it.
func New(d Deps) *Service {
	s := &Service{
		d:          d,
		interval:   d.Interval,
		run:        d.RunCmd,
		client:     d.HTTP,
		exe:        d.NvidiaSMI,
		newTicker:  realTicker,
		newEngines: newEngineReader,
		newSystem:  newSystemReader,
		memStatus:  readMemStatus,
		platform:   platformMarks,
		wake:       make(chan struct{}, 1),
		bal:        map[string]*balanceEntry{},
		engHist:    map[string][]histPoint{},
	}
	if s.interval <= 0 {
		s.interval = DefaultInterval
	}
	if s.run == nil {
		s.run = execRun
	}
	// The balance query carries the provider's key (Authorization: Bearer):
	// a redirect is followed only within the origin, as with the LLM
	// clients (llm.CheckRedirect; AUD1 I-2). net/http's own policy keeps
	// Authorization on a redirect to the same host name, from https down
	// to http or to another port included. A client given without a
	// policy gets this one (a copy; the caller's is left as it is).
	if s.client == nil {
		s.client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: llm.CheckRedirect}
	} else if s.client.CheckRedirect == nil {
		cp := *s.client
		cp.CheckRedirect = llm.CheckRedirect
		s.client = &cp
	}
	if s.exe == "" {
		s.exe = "nvidia-smi"
	}
	if s.d.Now == nil {
		s.d.Now = time.Now
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Start launches the background loop. It samples only while SetClients
// reports at least one client. Cancelling ctx has the effect of Stop except
// for waiting; call Stop to wait for the goroutines.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.loopWG.Add(1)
	s.mu.Unlock()
	if ctx != nil {
		context.AfterFunc(ctx, s.cancel)
	}
	go s.loop()
}

// Stop ends the loop and any balance query and waits for them.
func (s *Service) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.cancel()
	s.loopWG.Wait()
	s.fetchWG.Wait()
	s.closeEngines()
}

// SetClients reports the number of front ends looking at the data: the
// connected SSE clients whose page is shown (perf-fix: a window hidden to
// the tray or a background tab keeps its stream but does not count,
// internal/api/visibility.go). 0 stops sampling (A10: no sampling without a
// front end); a rise samples at once, so a page shown again gets a fresh
// snapshot, and then every Interval.
func (s *Service) SetClients(n int) {
	if n < 0 {
		n = 0
	}
	s.mu.Lock()
	s.clients = n
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// RefreshBalance asks for a new balance query of the provider (A10.2: after
// one of its jobs ends). The query runs at the next sample, so calls within
// one interval are coalesced; without clients it runs when one connects.
func (s *Service) RefreshBalance(providerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.bal[providerID]; e != nil {
		e.dirty = true
	}
}

// Snapshot returns the current data (GET /api/resources). It samples
// synchronously first when nothing was sampled yet or the last sample is
// older than Interval.
func (s *Service) Snapshot(ctx context.Context) Snapshot {
	if s.needSample() {
		s.sampleMu.Lock()
		if s.needSample() { // a concurrent sampler may have just finished
			s.sampleLocked(ctx)
		}
		s.sampleMu.Unlock()
	}
	return s.build(ctx)
}

func (s *Service) needSample() bool {
	now := s.d.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.sampled || now.Sub(s.lastAt) > s.interval
}

func (s *Service) loop() {
	defer s.loopWG.Done()
	var tickC <-chan time.Time
	var stopTick func()
	defer func() {
		if stopTick != nil {
			stopTick()
		}
	}()
	last := 0
	for {
		s.mu.Lock()
		n := s.clients
		s.mu.Unlock()
		switch {
		case n > 0 && (tickC == nil || n > last):
			if stopTick != nil {
				stopTick()
			}
			s.tick()
			tickC, stopTick = s.newTicker(s.interval)
		case n == 0 && tickC != nil:
			stopTick()
			tickC, stopTick = nil, nil
		}
		last = n
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case <-tickC:
			s.mu.Lock()
			active := s.clients > 0
			s.mu.Unlock()
			if active {
				s.tick()
			}
		}
	}
}

// tick takes one background sample and hands the snapshot to OnSample.
func (s *Service) tick() {
	if s.ctx.Err() != nil {
		return
	}
	s.sampleMu.Lock()
	s.sampleLocked(s.ctx)
	s.sampleMu.Unlock()
	if s.d.OnSample != nil && s.ctx.Err() == nil {
		s.d.OnSample(s.build(s.ctx))
	}
}

// sampleLocked runs nvidia-smi (with an NVIDIA GPU, T046), reads the Intel devices' PDH counters and
// the upper area's readings (system.go) and starts due balance queries.
// sampleMu held.
func (s *Service) sampleLocked(ctx context.Context) {
	s.sampleGPU(ctx)
	if ctx.Err() == nil {
		s.sampleEngines()
		s.sampleSystem()
		s.sampleApple(ctx)
	}
	s.kickBalances()
}

func (s *Service) sampleGPU(ctx context.Context) {
	if !s.nvidiaPresent(ctx) {
		// T046: no NVIDIA GPU, no nvidia-smi process; the card is simply
		// unavailable (no error to show).
		now := s.d.Now()
		s.mu.Lock()
		s.sampled, s.lastAt = true, now
		s.gpuOK, s.gpu, s.gpuErr, s.hist = false, Sample{}, "", nil
		s.mu.Unlock()
		return
	}
	cctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	out, err := s.run(cctx, s.exe, QueryArgs...)
	cancel()
	if ctx.Err() != nil {
		return // caller gone or stopping: keep the previous sample
	}
	var smp Sample
	if err != nil {
		err = fmt.Errorf("nvidia-smi: %w", err)
	} else {
		smp, err = ParseQuery(out)
	}
	now := s.d.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sampled, s.lastAt = true, now
	if err != nil {
		s.gpuOK, s.gpu, s.gpuErr = false, Sample{}, err.Error()
	} else {
		s.gpuOK, s.gpu, s.gpuErr = true, smp, ""
		if smp.MemTotalMB != nil {
			s.lastTotalMB = *smp.MemTotalMB
		}
		if smp.UtilPct != nil || smp.TempC != nil {
			s.hist = append(s.hist, histPoint{at: now, util: smp.UtilPct, temp: smp.TempC})
		}
	}
	s.hist = pruneHistory(s.hist, now)
}

// nvidiaPresent returns the cached answer of Deps.HasNVIDIA, asking again
// on the first call, after NVIDIARecheck (or a clock step back) and when
// the device detection ran again. sampleMu held.
func (s *Service) nvidiaPresent(ctx context.Context) bool {
	if s.d.HasNVIDIA == nil {
		return true
	}
	now := s.d.Now()
	var detAt time.Time
	if s.d.Detection != nil {
		detAt = s.d.Detection().At
	}
	if s.nvKnown && detAt.Equal(s.nvDetAt) &&
		now.Sub(s.nvAt) < NVIDIARecheck && !now.Before(s.nvAt) {
		return s.nvHas
	}
	// r4 D10: GET /api/resources samples with the request's context; a
	// page closed meanwhile must not turn the read into "no NVIDIA" kept
	// for NVIDIARecheck. The answer is about the computer, not the request.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nvidiaAskTimeout)
	has, err := s.d.HasNVIDIA(actx)
	cancel()
	if err != nil {
		slog.Debug("resources: NVIDIA GPU check failed; asking again at the next sample", "err", err)
		if s.nvKnown {
			return s.nvHas
		}
		// Nothing known yet: skip nvidia-smi for this sample, keep nothing.
		// The alternative, "has one" (the first-start rule of cmd
		// hasNVIDIAFunc), would run nvidia-smi on a computer without one and
		// show its error in the resource bar; skipping only leaves the GPU
		// area empty for one sample period, and a computer that does have
		// one is sampled as soon as the check answers (review R57 D-1). The
		// first-start rule itself stays in hasNVIDIAFunc's answer.
		return false
	}
	if !s.nvKnown || has != s.nvHas {
		slog.Info("resources: NVIDIA GPU sampling", "enabled", has)
	}
	s.nvKnown, s.nvHas, s.nvAt, s.nvDetAt = true, has, now, detAt
	return has
}

// pruneHistory keeps the points of the last Window, at most HistoryMax.
func pruneHistory(h []histPoint, now time.Time) []histPoint {
	i := 0
	for i < len(h) && now.Sub(h[i].at) >= Window {
		i++
	}
	if len(h)-i > HistoryMax {
		i = len(h) - HistoryMax
	}
	if i == 0 {
		return h
	}
	return append(h[:0:0], h[i:]...)
}

// kickBalances starts the balance queries that are due: never fetched, older
// than BalanceTTL, provider settings changed, or RefreshBalance was called.
func (s *Service) kickBalances() {
	if s.d.Providers == nil {
		return
	}
	ps := s.d.Providers()
	now := s.d.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	keep := map[string]bool{}
	for _, p := range ps {
		if p.BalanceAPI != model.BalanceDeepSeek || p.APIKey == "" {
			continue
		}
		keep[p.ID] = true
		fp := fingerprint(p)
		e := s.bal[p.ID]
		if e == nil || e.fp != fp {
			// New provider or changed base_url/key: forget the old balance.
			// A query still running for the old settings is discarded.
			e = &balanceEntry{fp: fp}
			s.bal[p.ID] = e
		}
		if e.fetching || (!e.at.IsZero() && !e.dirty && now.Sub(e.at) < BalanceTTL) {
			continue
		}
		e.fetching, e.dirty = true, false
		s.fetchWG.Add(1)
		go s.fetchBalance(p, fp)
	}
	for id, e := range s.bal {
		if !keep[id] && !e.fetching {
			delete(s.bal, id)
		}
	}
}

func (s *Service) fetchBalance(p model.Provider, fp string) {
	defer s.fetchWG.Done()
	ctx, cancel := context.WithTimeout(s.ctx, balanceTimeout)
	res := fetchDeepSeekBalance(ctx, s.client, p)
	cancel()
	now := s.d.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.bal[p.ID]
	if e == nil || e.fp != fp {
		return // the provider changed while the query ran
	}
	e.fetching = false
	if s.ctx.Err() != nil {
		return // stopping: do not record "context canceled"
	}
	e.at = now
	if res.err != nil {
		msg := res.err.Error()
		if msg != e.err {
			slog.Warn("balance query failed", "provider", p.ID, "err", msg) // redacted, no key
		}
		e.err, e.errInfo, e.balance, e.currency = msg, res.errInfo, nil, nil
		return
	}
	b := res.balance
	e.err, e.errInfo, e.balance, e.currency = "", nil, &b, nil
	if res.currency != "" {
		c := res.currency
		e.currency = &c
	}
}

// build assembles a Snapshot from the last sample, the live scheduler and
// settings state, the balance cache and usage_daily.
func (s *Service) build(ctx context.Context) Snapshot {
	var asr model.ASRSettings
	if s.d.ASR != nil {
		asr = s.d.ASR()
	}
	var slotsUsed, slotsMax int
	var paused bool
	if s.d.Slots != nil {
		slotsUsed, slotsMax, paused = s.d.Slots()
	}
	var ps []model.Provider
	if s.d.Providers != nil {
		ps = s.d.Providers()
	}
	type pool struct{ inflight, max int }
	pools := make([]pool, len(ps))
	for i, p := range ps {
		pools[i] = pool{0, p.MaxConcurrency}
		if s.d.Inflight != nil {
			in, max := s.d.Inflight(p.ID)
			pools[i].inflight = in
			if max > 0 {
				pools[i].max = max
			}
		}
	}
	now := s.d.Now()
	usage := s.usage(ctx, dayKey(now))
	holidays := s.holidays(ctx)
	var devIn deviceInput
	withDevices := s.devicesEnabled()
	if withDevices {
		devIn = s.readDevices(ctx)
	}
	var remote *colab.Status
	if s.d.Remote != nil {
		if st, ok := s.d.Remote(ctx); ok {
			remote = &st
		}
	}
	var sshCards []remoteCard
	if s.d.RemoteCards != nil {
		sshCards = s.titledCards(ctx, s.d.RemoteCards(ctx), devIn.titles)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	g := GPU{
		Available:    s.gpuOK,
		Name:         s.gpu.Name,
		UtilPct:      s.gpu.UtilPct,
		MemUsedMB:    s.gpu.MemUsedMB,
		MemTotalMB:   s.gpu.MemTotalMB,
		TempC:        s.gpu.TempC,
		PowerW:       s.gpu.PowerW,
		PowerLimitW:  s.gpu.PowerLimitW,
		UtilHistory:  []int{},
		TempHistory:  []int{},
		ASRSlotsUsed: slotsUsed,
		ASRSlotsMax:  slotsMax,
		Paused:       paused,
		Error:        s.gpuErr,
	}
	if b, ok := cudaBackend(devIn.rows, s.lastTotalMB); ok {
		g.Backend = b // SPEC-v2 I6: the cuda:0 row's precision
	} else if asr.Device == "cpu" {
		g.Backend = "CPU"
	} else {
		g.Backend = "CUDA · " + resolveComputeType(asr.ComputeType, s.lastTotalMB)
	}
	g.TempWarn = g.TempC != nil && *g.TempC >= TempWarnC
	for _, h := range pruneHistory(s.hist, now) {
		if h.util != nil {
			g.UtilHistory = append(g.UtilHistory, *h.util)
		}
		if h.temp != nil {
			g.TempHistory = append(g.TempHistory, *h.temp)
		}
	}
	snap := Snapshot{GPU: g, Providers: make([]Provider, 0, len(ps)), Remote: remote}
	snap.Remotes = append(remoteCards(remote, devIn), sshCards...)
	if withDevices {
		snap.Devices, snap.Power, snap.Policy = s.deviceCards(devIn, g, asr)
	}
	if s.systemEnabled() {
		snap.System = s.systemOf()
	}
	if s.sampled {
		snap.SampledAt = s.lastAt.UTC().Format(time.RFC3339)
	}
	for i, p := range ps {
		u := usage[p.ID]
		row := Provider{
			ID: p.ID, Name: p.Name,
			Today: u.today, Total: u.total,
			CacheHitRateToday: hitRate(u.today),
			CacheHitRateTotal: hitRate(u.total),
			Inflight:          pools[i].inflight,
			Max:               pools[i].max,
			TariffState:       TariffStateOf(p, now, holidays),
		}
		// v2 P12.5: the accumulated per-request estimates, not today's tokens
		// × a price (that would mix peak and off-peak usage); D3: null when
		// every request of today was unpriced, the unpriced ones counted.
		p := p
		row.CostToday, row.CostUnpricedRequests = settings.DisplayCost(&p, u.today.Requests, u.unpricedToday, u.costToday)
		row.Currency = settings.ProviderCurrency(p)
		s.fillBalance(&row, p)
		snap.Providers = append(snap.Providers, row)
	}
	return snap
}

// fillBalance copies the cached balance of p into row. s.mu held.
func (s *Service) fillBalance(row *Provider, p model.Provider) {
	if p.BalanceAPI != model.BalanceDeepSeek {
		return // balance_api = none: "--" without an explanation
	}
	if p.APIKey == "" {
		row.BalanceError = "未设置 API key，无法查询余额"
		row.BalanceErrorInfo = &model.ErrorInfo{Code: CodeBalanceNoAPIKey, Detail: row.BalanceError}
		return
	}
	e := s.bal[p.ID]
	if e == nil || e.at.IsZero() || e.fp != fingerprint(p) {
		return // not fetched yet
	}
	row.Balance, row.BalanceCurrency, row.BalanceError = e.balance, e.currency, e.err
	if e.errInfo != nil {
		info := *e.errInfo
		row.BalanceErrorInfo = &info
	}
	row.BalanceAt = e.at.UTC().Format(time.RFC3339)
}

type usagePair struct {
	today, total  Tokens
	costToday     float64 // sum of today's usage_daily.estimated_cost
	unpricedToday int64   // today's requests without a price (settings.UnpricedRequests)
}

// usage sums usage_daily per provider: today (local date) and all days.
func (s *Service) usage(ctx context.Context, today string) map[string]usagePair {
	out := map[string]usagePair{}
	if s.d.Store == nil {
		return out
	}
	rows, err := s.d.Store.ListUsageDaily(ctx, "")
	s.mu.Lock()
	prev := s.usageErr
	if err != nil {
		s.usageErr = err.Error()
	} else {
		s.usageErr = ""
	}
	s.mu.Unlock()
	if err != nil {
		if err.Error() != prev && ctx.Err() == nil {
			slog.Error("resources: read usage_daily", "err", err)
		}
		return out
	}
	for _, r := range rows {
		u := out[r.ProviderID]
		addUsage(&u.total, r)
		if r.Day == today {
			addUsage(&u.today, r)
			u.costToday += r.EstimatedCost
		}
		out[r.ProviderID] = u
	}
	for id, u := range out {
		if u.today.Requests == 0 {
			continue
		}
		n, err := settings.UnpricedRequests(ctx, s.d.Store, id, today)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("resources: read unpriced requests", "provider", id, "err", err)
			}
			continue
		}
		u.unpricedToday = n
		out[id] = u
	}
	return out
}

func addUsage(t *Tokens, r model.UsageDaily) {
	t.Requests += r.Requests
	t.PromptTokens += r.PromptTokens
	t.CacheHitTokens += r.CacheHitTokens
	t.CompletionTokens += r.CompletionTokens
}

func hitRate(t Tokens) *float64 {
	if t.PromptTokens <= 0 {
		return nil
	}
	r := float64(t.CacheHitTokens) / float64(t.PromptTokens)
	return &r
}

// holidays returns the holiday data for the tariff states (Deps.Holidays).
func (s *Service) holidays(ctx context.Context) tariff.HolidaySet {
	if s.d.Holidays != nil {
		return s.d.Holidays()
	}
	if s.d.Store != nil {
		if v, ok, err := s.d.Store.GetKV(ctx, model.KVHolidaysCN); err == nil && ok && v != "" {
			if h, err := tariff.ParseHolidays([]byte(v)); err == nil {
				return h
			}
		}
	}
	return tariff.HolidaysCN()
}

// TariffStateOf is P12.4 for one provider at now: nil when the provider has
// no tariff, an invalid one, or one without peak windows (no time-of-day
// pricing, never "空闲 · 半价"). TodayPeak lists the peak intervals of the
// server's local day (time.Local), clipped to it. The API uses it for
// ProviderView.TariffState as well.
func TariffStateOf(p model.Provider, now time.Time, holidays tariff.HolidaySet) *TariffState {
	if p.Tariff == nil {
		return nil
	}
	tf, err := tariff.Parse(*p.Tariff)
	if err != nil || tf == nil || len(tf.PeakWindows) == 0 {
		return nil
	}
	peak, next := tariff.State(now, *tf, holidays)
	ts := &TariffState{
		Peak:            peak,
		Timezone:        tf.Timezone,
		MissingHolidays: tariff.MissingHolidayData(now, *tf, holidays),
		TodayPeak:       [][2]string{},
		OffpeakOnly:     p.OffpeakOnly,
		Multiplier:      tf.OffpeakMultiplier,
	}
	if !next.IsZero() {
		ts.NextChange = next.UTC().Format(time.RFC3339)
	}
	for _, iv := range tariff.PeakIntervals(now, time.Local, *tf, holidays) {
		ts.TodayPeak = append(ts.TodayPeak, [2]string{iv[0].UTC().Format(time.RFC3339), iv[1].UTC().Format(time.RFC3339)})
	}
	return ts
}

// dayKey is the local date of usage_daily.day (same as sched.Pool).
func dayKey(now time.Time) string { return now.In(time.Local).Format("2006-01-02") }

// resolveComputeType is SPEC §17.2 for the Backend text: auto → float16 at
// ≥ 10 GB total VRAM, else int8_float16 (also when the total is unknown, as
// config.ResolveComputeType does for the asr stage).
func resolveComputeType(ct string, totalMB int) string {
	if ct != "" && ct != "auto" {
		return ct
	}
	if totalMB >= 10*1024 {
		return "float16"
	}
	return "int8_float16"
}
