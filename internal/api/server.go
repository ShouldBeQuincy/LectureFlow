// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/pyenv"
	"lectureflow/internal/relocate"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
	"lectureflow/web"
)

// Deps are the collaborators of the HTTP server.
type Deps struct {
	Store   db.Store
	Sched   *sched.Scheduler
	Cfg     *config.Config
	DataDir string
	// Hub receives the scheduler's events and serves /api/events. Create it
	// with NewHub before the scheduler and pass the same value to
	// sched.Options.Events. nil = a new Hub (then the scheduler's events are
	// not seen by SSE clients).
	Hub *Hub
	// Settings is the settings service of v1.1 A5 — the SAME instance the
	// scheduler and the llm client use (sched.Options.Settings), so a change
	// made here applies to the next launch and the next request. nil (old
	// tests only): the settings endpoints answer 503 and the views fall back
	// to the legacy config.toml fields.
	Settings *settings.Service
	// Resources is the resource watcher of A10 (GET /api/resources, SSE
	// "event: resources"). nil = /api/resources answers 503. Wire its
	// Deps.OnSample to Hub.Resources; the server reports the SSE client
	// count to it (SetClients) and asks for a balance refresh when an API
	// job of a provider ends (RefreshBalance).
	Resources *resources.Service
	// LLM is the client used for "整理大纲" (A8.3, role syllabus_format).
	// nil = POST /api/courses/{id}/syllabus/format answers 503.
	LLM llm.Client
	// LLMCheck sends a minimal request to one provider for the diagnostics
	// "API 连通性" row (SPEC §17.1) in legacy mode (Settings == nil) only;
	// with Settings the check uses llm.TestConnection. nil = not run (warn).
	LLMCheck func(ctx context.Context, provider string) error
	// Devices is the transcription device registry of SPEC-v2 I6 (phase 3b
	// freeze; the same instance as sched.Options.Devices and
	// settings.SetDeviceSource). nil = the device endpoints answer 503 and
	// the views show no devices (tests and builds before 3b).
	Devices *devices.Registry
	// Desktop is non-nil in desktop mode (SPEC-v3 V1.2).
	Desktop *DesktopOptions
	// Version is the application version (both modes).
	Version string
	// Components is the component manager of SPEC-v3 V3 (both modes; nil =
	// the component endpoints answer 503). Wired in cmd (.claude/v3-brief.md §7.2).
	Components *components.Manager
	// Colab is the Colab remote transcription backend (SPEC-v3 V14,
	// .claude/v3-brief.md §8; nil = the /api/colab endpoints answer 503).
	// The same instance as sched.Options.Remote.
	Colab *colab.Backend
	// Downloads is the browser extension's download queue (SPEC-ext,
	// internal/fetch; handlers_downloads.go). nil = /api/downloads answers
	// 503; the download settings are still read and written.
	Downloads DownloadQueue
	// SelfUpdate is the in-app update of SPEC-ext (internal/selfupdate,
	// handlers_selfupdate.go). nil = /api/selfupdate answers 503.
	SelfUpdate SelfUpdater
}

// Server is the HTTP JSON API (SPEC §8, §17.1, SPEC-v1.1) plus the embedded front end.
type Server struct {
	st      db.Store
	sc      *sched.Scheduler
	cfg     *config.Config
	dataDir string
	hub     *Hub
	set     *settings.Service
	res     *resources.Service
	llmc    llm.Client
	llm     func(ctx context.Context, provider string) error
	// dev and dsc are the transcription devices (SPEC-v2 I6-I10): Deps.Devices
	// and the device part of Deps.Sched; both nil before 3b (no device
	// endpoints, views without devices). Interfaces so tests can fake them.
	dev deviceRegistry
	dsc deviceScheduler
	// desktop is Deps.Desktop (nil = web mode); version is Deps.Version
	// (SPEC-v3 V1, desktop.go).
	desktop *DesktopOptions
	version string
	// comps is Deps.Components (nil = no component manager).
	comps *components.Manager
	// colab is Deps.Colab (nil = no Colab backend; handlers_colab.go).
	colab *colab.Backend
	// nm is the browser extension's Native Messaging service, set by cmd
	// once it listens (SetNMHost; handlers_nmhost.go).
	nm nmHostHolder
	// dl is Deps.Downloads (nil = no download queue).
	dl DownloadQueue
	// su is Deps.SelfUpdate (nil = no in-app update; handlers_selfupdate.go).
	su SelfUpdater

	// Injected in tests.
	maxUpload  int64                           // bytes per uploaded file (SPEC §17.4: 4 GB)
	minFree    int64                           // refuse uploads below this free space (SPEC §17.5: 2 GB)
	freeSpace  func(dir string) (int64, error) // bytes available on the partition of dir
	runCmd     func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
	staticFS   fs.FS
	now        func() time.Time
	testConn   func(ctx context.Context, p model.Provider, modelName string) llm.TestResult
	probe      func(ctx context.Context, p model.Provider) ([]string, error)
	extractPDF func(ctx context.Context, dir, pdf string) ([]string, error)
	// findClip is devices.FindClipSource, else the bundled sample (the
	// self-test's audio, I9, SPEC-v3 V4.3); LectureID is empty for the sample.
	findClip func(ctx context.Context) (devices.ClipSource, bool, error)
	// colabReach is the diagnostics' Colab reachability call (reachColab,
	// system_env.go). Per server, not a package variable: a test with two
	// servers must not have one server's late check call the other's fake.
	colabReach func(ctx context.Context) (supported bool, err error)

	inval *invalidator
	diag  *diagnostics

	balMu   sync.Mutex
	balSeen map[string]string // job id → "status|finished_at" already handled by jobEvent

	// trOffers caches whether a lecture's asr stage offers "修复断句"
	// (textrepair.go).
	trOffers textRepairOffers

	// grants are the desktop path uploads' one-time grants (pathgrant.go).
	grants *pathGrants
	// evalSymlinks and statSource are checkSourcePath's file system calls
	// (filepath.EvalSymlinks, os.Stat); tests replace them to see that a
	// refused path is never touched.
	evalSymlinks func(string) (string, error)
	statSource   func(string) (fs.FileInfo, error)
	// statDir, checkMove and checkImportSource are the data directory
	// endpoints' first file system calls on a page's path (os.Stat in
	// volumeFree, relocate.CheckMove, relocate.CheckImportSource); tests
	// replace them to see that a refused path (one the folder dialog did not
	// pick, network or local: folderRefused) is never touched.
	statDir           func(string) (fs.FileInfo, error)
	checkMove         func(ctx context.Context, src, dst, locationFile string) (relocate.MovePlan, error)
	checkImportSource func(ctx context.Context, dir string) (relocate.SourceInfo, error)
}

// New builds the server and starts the startup diagnostics run (SPEC §17.1,
// without the CUDA self-test, which only runs on "重新检查").
func New(d Deps) *Server { return newServer(d, nil) }

// newServer lets tests replace the injectable fields before the startup
// diagnostics begin.
func newServer(d Deps, tweak func(*Server)) *Server {
	s := &Server{
		st: d.Store, sc: d.Sched, cfg: d.Cfg, dataDir: d.DataDir, hub: d.Hub, llm: d.LLMCheck,
		set: d.Settings, res: d.Resources, llmc: d.LLM, desktop: d.Desktop, version: d.Version, comps: d.Components,
		maxUpload: 4 << 30,
		minFree:   2 << 30,
		freeSpace: diskFree,
		staticFS:  web.FS,
		now:       time.Now,
		testConn:  llm.TestConnection,
		probe:     llm.ProbeModels,
		balSeen:   map[string]string{},
	}
	s.runCmd = s.runDiagnostic // (tests replace it)
	s.extractPDF = s.runExtractPDF
	s.grants = newPathGrants()
	s.evalSymlinks = filepath.EvalSymlinks
	s.statSource = os.Stat
	s.statDir = os.Stat
	s.checkMove = relocate.CheckMove
	s.checkImportSource = relocate.CheckImportSource
	s.colabReach = s.reachColab
	s.findClip = func(ctx context.Context) (devices.ClipSource, bool, error) {
		src, ok, err := devices.FindClipSource(ctx, s.st, s.dataDir)
		if err != nil || ok {
			return src, ok, err
		}
		// No lecture: the bundled sample (SPEC-v3 V4.3) makes the test
		// available too (stages.Deps.DeviceTest uses it).
		if s.cfg != nil {
			if audio, _, ok := devices.BundledClip(s.cfg.SampleDir); ok {
				return devices.ClipSource{Title: devices.BundledAudioFile, Audio: audio}, true, nil
			}
		}
		return devices.ClipSource{}, false, nil
	}
	// A nil *devices.Registry / *sched.Scheduler must stay a nil interface.
	if d.Devices != nil {
		s.dev = d.Devices
	}
	if d.Sched != nil && d.Devices != nil {
		s.dsc = d.Sched
	}
	s.colab = d.Colab
	s.dl = d.Downloads
	s.su = d.SelfUpdate
	if s.hub == nil {
		s.hub = NewHub()
	}
	if s.dataDir == "" && s.cfg != nil {
		s.dataDir = s.cfg.DataDir
	}
	s.inval = newInvalidator(s.invalidateCourse)
	if tweak != nil {
		tweak(s)
	}
	if s.res != nil {
		s.hub.setClientsHook(s.res.SetClients)
	}
	s.hub.setJobHook(s.jobEvent)
	s.hub.setJobStats(s.jobEventStats)
	if s.set != nil {
		// Every settings or provider write (A5): the settings page and the
		// resource watcher refetch.
		s.set.OnChange(func() { s.hub.Changed("settings", "") })
	}
	s.wireSelfUpdate() // handlers_selfupdate.go
	s.diag = newDiagnostics(s)
	s.diag.start()
	return s
}

// Hub returns the SSE hub (it implements sched.Events).
func (s *Server) Hub() *Hub { return s.hub }

// StopStartupChecks cancels the startup checks of the diagnostics page
// that are still running and waits for them, at most timeout (orderly
// shutdown: the data directory check writes a file there, and the checks
// read the database). It reports whether they ended in time.
func (s *Server) StopStartupChecks(timeout time.Duration) bool {
	s.diag.stop()
	select {
	case <-s.diag.ready:
		return true
	case <-time.After(timeout):
		return false
	}
}

// WaitInvalidations blocks until the background invalidation runs started
// by term/material changes have finished (tests and orderly shutdown).
func (s *Server) WaitInvalidations() { s.inval.wait() }

// Handler returns the router: the API under /api/ and the front end at /,
// behind the security headers and, in desktop mode, the Host and token
// checks (desktop.go), all behind the slow-request log (slowlog.go).
func (s *Server) Handler() http.Handler {
	return s.handlerChain(s.routes(), slowRequestThreshold, nil, time.Now)
}

// routes is the router of Handler without the middleware.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	h := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, fn) }

	// courses, syllabus, materials, terms (SPEC §8, NOTES.md B–E, v1.1 A8, A11)
	h("GET /api/courses", s.listCourses)
	h("POST /api/courses", s.createCourse)
	h("GET /api/courses/{id}", s.getCourse)
	h("PATCH /api/courses/{id}", s.patchCourse)
	h("POST /api/courses/{id}/launch-drafts", s.launchDrafts)
	h("POST /api/courses/{id}/update-stale", s.updateStale)
	h("GET /api/courses/{id}/export/cite.json", s.courseCite)
	h("POST /api/courses/{id}/syllabus", s.uploadSyllabus)
	h("GET /api/courses/{id}/syllabus", s.getSyllabus)
	h("PUT /api/courses/{id}/syllabus", s.saveSyllabus)
	h("POST /api/courses/{id}/syllabus/raw", s.syllabusRaw)
	h("POST /api/courses/{id}/syllabus/format", s.formatSyllabus)
	h("POST /api/courses/{id}/materials", s.uploadMaterial)
	h("POST /api/courses/{id}/stages/{stage}/{action}", s.courseStageAction)
	h("GET /api/courses/{id}/terms", s.listTerms)
	h("POST /api/courses/{id}/terms", s.createTerm)
	h("PATCH /api/terms/{id}", s.patchTerm)
	h("DELETE /api/terms/{id}", s.deleteTerm)
	h("POST /api/courses/{id}/glossary/review", s.reviewGlossary)                   // v2 G3.1
	h("GET /api/courses/{id}/glossary/report", s.glossaryReport)                    // v2 G3.3
	h("POST /api/courses/{id}/glossary/report/undo", s.undoGlossaryReport)          // v2 G3.5
	h("GET /api/courses/{id}/filename-rule", s.getFilenameRule)                     // v2 P13.3.3
	h("PUT /api/courses/{id}/filename-rule", s.putFilenameRule)                     // v2 P13.3.3 应用 / 手动编辑
	h("POST /api/courses/{id}/filename-rule/generate", s.generateFilenameRule)      // v2 P13.3.2 重新识别
	h("POST /api/courses/{id}/filename-rule/preview", s.previewFilenameRule)        // v2 P13.3.3 live preview
	h("DELETE /api/courses/{id}/filename-rule/proposal", s.dismissFilenameProposal) // v2 P13.3.3 忽略
	h("POST /api/courses/{id}/filename-rule/undo", s.undoFilenameRule)              // v2 P13.3.3 撤销
	h("DELETE /api/courses/{id}/renumber-notices", s.dismissRenumberNotices)        // lecture kinds: automatic renumbering
	h("GET /api/lecture-kinds", s.listKinds)                                        // lecture kinds (.claude/kinds-brief.md)
	h("POST /api/lecture-kinds", s.createKind)
	h("PATCH /api/lecture-kinds/{id}", s.patchKind)
	h("DELETE /api/lecture-kinds/{id}", s.deleteKind)
	h("PATCH /api/materials/{id}", s.patchMaterial)
	h("DELETE /api/materials/{id}", s.deleteMaterial)
	h("POST /api/materials/{id}/stages/{stage}/{action}", s.materialStageAction)

	// lectures (SPEC §8, NOTES.md A, v1.1 A2.3, A3, A7)
	h("GET /api/courses/{id}/lectures", s.listLectures)
	h("POST /api/courses/{id}/lectures", s.uploadLecture)
	h("POST /api/lectures/launch", s.batchLaunch)
	h("POST /api/lectures/update", s.batchUpdate)
	h("POST /api/lectures/cancel", s.batchCancel)
	h("POST /api/lectures/delete", s.batchDelete)
	h("GET /api/lectures/{id}", s.getLecture)
	h("PATCH /api/lectures/{id}", s.patchLecture)
	h("PUT /api/lectures/{id}/kind", s.setLectureKind) // lecture kinds
	h("DELETE /api/lectures/{id}", s.deleteLecture)
	h("PUT /api/lectures/{id}/media", s.replaceMedia)
	h("POST /api/lectures/{id}/launch", s.launchLecture)
	h("POST /api/lectures/{id}/update", s.updateLecture)
	h("POST /api/lectures/{id}/stages/{stage}/{action}", s.lectureStageAction)
	h("GET /api/lectures/{id}/sentences", s.listSentences)
	h("POST /api/lectures/{id}/sentences/{idx}/{op}", s.sentenceOp)
	h("GET /api/lectures/{id}/patches", s.listPatches)
	h("POST /api/patches/{id}/review", s.reviewPatch)
	h("GET /api/lectures/{id}/audio", s.audio)
	h("GET /api/lectures/{id}/export/{file}", s.exportFile)

	// the player and titles (SPEC-v2 P6.4, P7, P8.2, P13.5)
	h("GET /api/lectures/{id}/media", s.media)
	h("GET /api/lectures/{id}/cues", s.cues)
	h("GET /api/lectures/{id}/chapters", s.getChapters)
	h("POST /api/lectures/{id}/chapters", s.postChapters)
	h("GET /api/lectures/{id}/resume", s.getResume)
	h("PUT /api/lectures/{id}/resume", s.putResume)
	h("POST /api/lectures/{id}/title", s.regenerateTitle)

	// settings and providers (v1.1 A5)
	h("GET /api/settings", s.getSettings)
	h("PUT /api/settings/roles/{role}", s.putRole)
	h("PUT /api/settings/asr", s.putASR)
	h("PUT /api/settings/general", s.putGeneral)
	h("GET /api/settings/holidays", s.getHolidays) // v2 P12.2
	h("PUT /api/settings/holidays", s.putHolidays)
	h("GET /api/settings/ui", s.getUI) // v2 P14.6, P14.7
	h("PUT /api/settings/ui", s.putUI)
	h("GET /api/providers", s.listProviders)
	h("POST /api/providers", s.createProvider)
	h("PATCH /api/providers/{id}", s.patchProvider)
	h("DELETE /api/providers/{id}", s.deleteProvider)
	h("POST /api/providers/{id}/test", s.testProvider)
	h("POST /api/providers/{id}/models", s.probeProvider)

	// dashboard and resources (v1.1 A9, A10)
	h("GET /api/dashboard", s.getDashboard)
	h("GET /api/resources", s.getResources)

	// system (SPEC §17.1) and SSE (SPEC §8.2)
	h("GET /api/system", s.getSystem)
	h("POST /api/system/recheck", s.recheck)
	h("POST /api/system/gpu", s.setGPU)

	// transcription devices (SPEC-v2 I6-I10, types_intel.go)
	h("GET /api/asr/devices", s.listDevices)
	h("POST /api/asr/devices/detect", s.postDetectDevices)
	h("PUT /api/asr/devices/order", s.putDeviceOrder)
	h("PATCH /api/asr/devices/{id}", s.patchDevice)
	h("POST /api/asr/devices/{id}/pause", s.pauseDevice)
	h("POST /api/asr/devices/{id}/selftest", s.startSelfTest)
	h("GET /api/asr/devices/{id}/selftest", s.getSelfTest)
	h("DELETE /api/asr/devices/{id}/selftest", s.cancelSelfTest)
	h("POST /api/asr/devices/{id}/selftest/apply", s.applySelfTest)
	h("POST /api/asr/devices/{id}/prompt/dismiss", s.dismissDevicePrompt)
	mux.Handle("GET /api/events", s.hub)
	h("POST /api/events/visibility", s.postEventsVisibility) // perf-fix: visible front ends only (visibility.go)

	// the application (SPEC-v3 V1.1, V1.2, V4, V11; desktop.go)
	h("GET /api/health", s.getHealth)
	h("GET /api/app", s.getApp)
	h("GET /api/app/settings", s.getAppSettings)
	h("PATCH /api/app/settings", s.patchAppSettings)
	h("GET /api/app/activity", s.getActivity)
	h("POST /api/app/shutdown", s.postShutdown)
	h("GET /api/onboarding", s.getOnboarding)
	h("PUT /api/onboarding", s.putOnboarding)
	s.registerV3Routes(h) // routes_v3.go: v3 handlers register themselves from their own files

	h("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
	})
	mux.Handle("/", s.static())
	return mux
}

// ---- static front end ----

// Explicit content types: on Windows the registry may map .js to
// text/plain, which browsers refuse for module scripts.
var staticTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
}

func (s *Server) static() http.Handler {
	fsrv := http.FileServerFS(s.staticFS)
	etags := staticETags(s.staticFS) // static_etag.go
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "request.method_not_allowed", nil)
			return
		}
		ext := path.Ext(r.URL.Path)
		if r.URL.Path == "/" {
			ext = ".html"
		}
		if ct, ok := staticTypes[ext]; ok {
			w.Header().Set("Content-Type", ct)
		}
		// The assets are embedded in the binary; revalidate so an upgrade
		// is picked up without a hard reload. The ETag (the content's hash)
		// makes that revalidation a 304 (http.ServeContent checks
		// If-None-Match against it).
		w.Header().Set("Cache-Control", "no-cache")
		if tag, ok := etags()[staticName(r.URL.Path)]; ok {
			w.Header().Set("ETag", tag)
		}
		fsrv.ServeHTTP(w, r)
	})
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("write response", "err", err)
	}
}

// decodeJSON reads a small JSON request body.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return apiErr(http.StatusBadRequest, "request.bad_json", nil)
	}
	return nil
}

// decodeOptionalJSON is decodeJSON for an optional body: an empty body
// leaves v unchanged.
func decodeOptionalJSON(r *http.Request, v any) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return apiErr(http.StatusBadRequest, "request.bad_json", nil)
	}
	return nil
}

// ---- background invalidation (SPEC §4.4, §4.6, v1.1 A2.2) ----

// invalidateCourse is the work of one invalidation run: recompute the
// input hashes of every lecture/material/course job of the course and mark
// changed done jobs stale with the reason of cause (the scheduler emits
// their events).
func (s *Server) invalidateCourse(courseID string, cause state.StaleCause) {
	if s.sc == nil {
		return
	}
	if err := s.sc.InvalidateCourseWith(context.Background(), courseID, cause); err != nil {
		slog.Error("invalidate course", "course", courseID, "err", err)
	}
	s.sc.Kick()
}

// invalidator runs invalidation in the background, one run per course at a
// time. Requests arriving during a run are merged per cause kind (the last
// request of each kind wins: an input hash does not tell which of several
// changes of one kind altered it; NOTES.md v1.1 api-builder) and cause one
// more run per kind, the glossary first: a glossary change merged with a
// material upload still refreshes the glossary reasons (NOTES lead 第五批).
type invalidator struct {
	mu      sync.Mutex
	running map[string]bool
	dirty   map[string]map[string]state.StaleCause // course → kind → cause
	wg      sync.WaitGroup
	work    func(courseID string, cause state.StaleCause)
}

// causeOrder is the order of the merged runs: glossary first, then the
// janitor (v2 G3.1: a term edit merged with a janitor run or undo refreshes
// both reasons; a kind missing here would be dropped silently). The
// material-set comparison (lecture kinds, state.StaleMaterials) runs after
// a material's own change, so an extraction finished at the same time keeps
// its reason material_changed {filename}.
var causeOrder = []string{state.StaleGlossary, state.StaleJanitor, state.StaleMaterial, state.StaleMaterials, state.StaleSummary, ""}

func newInvalidator(work func(string, state.StaleCause)) *invalidator {
	return &invalidator{running: map[string]bool{}, dirty: map[string]map[string]state.StaleCause{}, work: work}
}

func (iv *invalidator) request(courseID string, cause state.StaleCause) {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	if iv.running[courseID] {
		if iv.dirty[courseID] == nil {
			iv.dirty[courseID] = map[string]state.StaleCause{}
		}
		iv.dirty[courseID][cause.Kind] = cause
		return
	}
	iv.running[courseID] = true
	iv.wg.Add(1)
	go func() {
		defer iv.wg.Done()
		next := []state.StaleCause{cause}
		for {
			for _, c := range next {
				iv.work(courseID, c)
			}
			iv.mu.Lock()
			merged := iv.dirty[courseID]
			if len(merged) == 0 {
				delete(iv.running, courseID)
				iv.mu.Unlock()
				return
			}
			delete(iv.dirty, courseID)
			next = next[:0]
			for _, k := range causeOrder {
				if c, ok := merged[k]; ok {
					next = append(next, c)
				}
			}
			iv.mu.Unlock()
		}
	}()
}

func (iv *invalidator) wait() { iv.wg.Wait() }

// ---- balance refresh after API jobs (A10.2) ----

// balanceStages are the API stages whose end refreshes the provider balance.
var balanceStages = map[string]bool{
	model.StageSummary: true, model.StageRepair: true, model.StageTranslate: true,
	model.StageGlossarySeed: true, model.StageMaterialTerms: true, model.StageGlossaryReview: true,
	// v2 auxiliary tasks calling a model (the role's current provider)
	model.StageTitle: true, model.StageChapters: true, model.StageFilenameRule: true,
}

// jobEvent is called by the hub for every job event when a job ends (done,
// failed, cancelled):
//   - glossary_review (v2 G3.5): the janitor changed terms, or appendix D2
//     applied the fallback rule, so the glossary and the course's Janitor
//     status are announced as changed;
//   - API jobs: the balance of the provider is refreshed — lecture jobs: the
//     provider of the job's snapshot; course/material jobs: the role's
//     current provider.
//
// Each end is handled once (the scheduler re-emits unchanged jobs when it
// emits a whole scope).
func (s *Server) jobEvent(j model.Job) {
	switch j.Status {
	case model.StatusDone, model.StatusFailed, model.StatusCancelled:
	default:
		return
	}
	janitor := j.Stage == model.StageGlossaryReview
	balance := s.res != nil && balanceStages[j.Stage]
	if !janitor && !balance {
		return
	}
	key := j.Status + "|"
	if j.FinishedAt != nil {
		key += *j.FinishedAt
	}
	s.balMu.Lock()
	if s.balSeen[j.ID] == key {
		s.balMu.Unlock()
		return
	}
	if len(s.balSeen) > 4096 {
		clear(s.balSeen)
	}
	s.balSeen[j.ID] = key
	s.balMu.Unlock()
	if janitor {
		s.hub.Changed("terms", j.ScopeID)
		s.hub.Changed("course", j.ScopeID)
	}
	if !balance {
		return
	}
	provider := ""
	if j.Scope == model.ScopeLecture {
		if snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot); ok {
			provider = snap.ProviderID
		}
	}
	if provider == "" && s.set != nil {
		if t, err := s.set.RoleTarget(model.RoleOfStage(j.Stage)); err == nil {
			provider = t.ProviderID
		}
	}
	if provider != "" {
		s.res.RefreshBalance(provider)
	}
}

// ---- misc ----

func (s *Server) logWarn(msg, id string, err error) {
	slog.Warn(msg, "id", id, "err", err)
}

func (s *Server) kick() {
	if s.sc != nil {
		s.sc.Kick()
	}
}

// runDiagnostic is the server's runCmd: runCommandIn with the worker
// directory as the Python calls' working directory.
func (s *Server) runDiagnostic(ctx context.Context, name string, args ...string) (string, string, error) {
	dir := ""
	if s.cfg != nil {
		dir = s.cfg.WorkerDir
	}
	return runCommandIn(ctx, dir, name, args...)
}

// runCommand is runCommandIn without a working directory for Python calls.
func runCommand(ctx context.Context, name string, args ...string) (string, string, error) {
	return runCommandIn(ctx, "", name, args...)
}

// runCommandIn runs a short diagnostic command and returns its output. The
// Python checks and self-tests run through it, so it has pyenv's
// environment (no usage reporting); so does the upload's ffprobe
// (media.Check). Like every helper program it ends with LectureFlow, also
// on a crash (procutil.Tied; test-chaos 1). A Python call runs in
// workerDir (diagCommand).
func runCommandIn(ctx context.Context, workerDir, name string, args ...string) (string, string, error) {
	cmd := diagCommand(ctx, workerDir, name, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	cmd.WaitDelay = 2 * time.Second
	err := procutil.Run(cmd, procutil.Tied)
	return out.String(), errb.String(), err
}

// diagCommand is runCommandIn's command. A Python call (its first argument
// is one of pyenv's isolation switches: -I for the import checks' code, -s
// for a worker script, as every check, self-test and cuDNN check of the
// diagnostics starts) runs in workerDir when that is an absolute path, not
// in the service's current directory: the folder the user started
// LectureFlow from in web mode, where Windows would look for a DLL loaded
// by name (-I keeps it off sys.path already; SEC47 info-5). The worker
// directory, rather than an empty private one: it is LectureFlow's own and
// holds only the worker scripts, a script's directory is sys.path[0]
// anyway (whoever can write there changes the code that runs), and it
// exists in every mode without a temporary directory to make and clean
// up. Other commands (ffprobe, ffmpeg, `python --version`, which runs no
// Python code) keep the current directory: their arguments may hold
// relative paths. (nvidia-smi does not run here: the scheduler's, the
// device detection's and the resource sampler's run in nvidia-smi's own
// directory, procutil.CommandInExeDir.)
func diagCommand(ctx context.Context, workerDir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = pyenv.Environ()
	if len(args) > 0 && (args[0] == "-I" || args[0] == "-s") && filepath.IsAbs(workerDir) {
		cmd.Dir = workerDir
	}
	return cmd
}

// stagesOf returns the lecture/material/course stage list of a scope.
func stagesOf(scope string) []string { return state.StagesOf(scope) }

// validStage: a stage of the scope's chain; for a course also the v2
// glossary_review, which is not in the chain (lead ruling 2) but takes the
// same stage actions; and the scope's auxiliary tasks (phase-2 ruling 1:
// title, chapters, stream for a lecture, filename_rule for a course).
func validStage(scope, stage string) bool {
	if scope == model.ScopeCourse && stage == model.StageGlossaryReview {
		return true
	}
	if model.IsAuxStage(stage) {
		return model.AuxScope(stage) == scope
	}
	for _, s := range stagesOf(scope) {
		if s == stage {
			return true
		}
	}
	return false
}

// validAction: cancel, retry or rerun; an auxiliary task is only retried or
// cancelled (it is regenerated by its own endpoint, e.g. POST /chapters).
func validAction(stage, a string) bool {
	if model.IsAuxStage(stage) {
		return a == model.ActionCancel || a == model.ActionRetry
	}
	return a == model.ActionCancel || a == model.ActionRetry || a == model.ActionRerun
}

// requireSettings answers 503 when the server runs without the settings
// service (old wiring); true when the handler may continue.
func (s *Server) requireSettings(w http.ResponseWriter) bool {
	if s.set == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "settings"})
		return false
	}
	return true
}
