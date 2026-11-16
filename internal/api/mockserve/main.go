// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command mockserve is a development-only server for previewing the front end
// without the real back end (SPEC §16.2 "mock 模式", v1.1 front-end work).
//
//	go run ./internal/api/mockserve            # then open http://127.0.0.1:8421/
//	go run ./internal/api/mockserve -addr 127.0.0.1:9000
//	go run ./internal/api/mockserve -media sample/clip.mp4   # the player's video (player.go)
//
// It serves web/ from disk (edit and reload, no restart) and answers the
// HTTP API from an in-memory store loaded from testdata/api/v11_*.json (plus
// the v1 mock_sentences / mock_patches / terms examples). Write operations
// (launch, update, cancel, delete, rename, review, …) change that store and
// push SSE "changed" events, so the front end only ever renders what the API
// returned. Restart the server to get the fixtures back.
//
// The page passes its ?mock=… value on every request; scenarios:
//
//	1      default data
//	empty  no courses (dashboard empty state)
//	nogpu  nvidia-smi unavailable, balance query failed (resource "--" states)
//	error  dashboard, resources and lecture lists answer 500 (card error states)
//	slow   every GET waits 1.5 s (skeletons)
//	diag   diagnostics contain an error (first-run redirect to #/system)
//	many   450 sentences in the transcript (paging)
//	unset  settings with an unconfigured role and a deleted provider (settings.go);
//	       no glossary janitor, so 整理术语表 applies the common-word rule (janitor.go)
//	oldmodel  settings whose DeepSeek model list is the current one (deepseek-flash,
//	       deepseek-v4-pro) while five roles still use deepseek-chat: the settings
//	       page's row markers and the diagnostics' check.role_model_unlisted
//	renumbered  COMP3311 with a term start and an exported citation JSON, where
//	       the title task's kind (a tutorial) renumbered another lecture: the
//	       course page's notice (Course.renumber_notices, kinds.go)
//	courseux  week overrides: a material hung under week 3, two make-ups
//	       ("补"); batch rerun and PUT/DELETE .../week (course_ux.go)
//	peak, offpeak  the default data with the tariff clock moved ahead into that
//	       period (P12.4 tags; course_p2.go); set the browser clock to the
//	       resources' sampled_at for consistent countdowns
//	battery, noov, cpuonly  transcription devices (devices.go)
//	lowvram, lowvram_fp16, lowvram_turbo  a 4 GB NVIDIA card (cuda:0 on
//	       large-v2, the same at float16, or after switching to
//	       large-v3-turbo): cuda:0's VRAM hint on the diagnostics page
//	       (none for large-v3-turbo) (vram_hint.go)
//	lowvram_missing, lowvram_missing_install  the same 4 GB card where
//	       cuda:0's large-v2 is not installed: the diagnostics hint that
//	       says so and suggests the card's recommended large-v3-turbo,
//	       installed / still to install (vram_hint.go)
//	vram_raised  the per-task estimate raised after running out of GPU
//	       memory: the GPU card's note (vram_hint.go)
//	diag_intel, diag_mac, diag_cloud  the diagnostics by hardware and
//	       transcription mode: a pure Intel laptop, an Apple silicon Mac,
//	       only Colab (ext_diag.go; GET / PUT /api/asr-mode)
//	angles  CSC3631 L01 has a second camera angle (ext_web3.go; -angle2 <file>)
//	ext_ok, ext_unregistered, ext_broken, ext_unmanaged, ext_nmoff
//	       the diagnostics rows aria2c and browser_ext and GET /api/nmhost
//	       (ext_web3.go)
//	datadir_shared, datadir_readable, datadir_fat
//	       other accounts can change / read the data folder (the data_dir
//	       row's warning and "收紧权限"; the last one cannot be restricted)
//	       (datadir.go)
//	installdir_ok, installdir_shared, installdir_unconfirmed, installdir_above,
//	installdir_root, installdir_profile
//	       the installation folder's row: private, other accounts can
//	       change it, cannot be confirmed, a folder above it, the drive's
//	       root above it, the default place with a foreign group
//	       (installdir.go)
//	power_ac_dgpu, power_ac_intel, power_battery_npu, power_battery_suspended,
//	power_ac_switch, power_all_suspended
//	       the power policy's resource bar (power.go): AC + NVIDIA card, AC +
//	       Intel only, battery + NPU, battery with a suspended NVIDIA job,
//	       plugged in while the NPU runs (switch pending), battery without
//	       any qualifying device (everything suspended)
//	old_failures  the default data with the Deadlocks translate failure three
//	       days old: out of the dashboard's 24-hour failures, counted in
//	       earlier_failed_total and in COMP2120's failed_count (r7.go)
//
// It also streams fake SSE (job progress, "resources" every tick, one
// "lecture finished" and one "final failure" event), synthetic audio clips
// and discarded multipart uploads (a title or file name containing "fail"
// simulates a server error). It only listens on a loopback address and never
// touches the network.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8421", "listen address (loopback only)")
	root := flag.String("root", "", "project root (default: nearest directory upwards containing go.mod)")
	media := flag.String("media", "", "MP4 served as every lecture's GET /media (none: the player uses audio mode)")
	flag.Parse()

	if err := checkLoopback(*addr); err != nil {
		log.Fatal(err)
	}
	dir := *root
	if dir == "" {
		var err error
		if dir, err = findRoot(); err != nil {
			log.Fatal(err)
		}
	}
	h := newHandler(filepath.Join(dir, "web"), filepath.Join(dir, "testdata", "api"), 2*time.Second)
	if m, ok := h.(*handler); ok && m.m.loadErr != nil {
		log.Fatal(m.m.loadErr)
	}
	if *media != "" {
		if _, err := os.Stat(*media); err != nil {
			log.Fatal(err)
		}
		h.(*handler).m.mediaFile = *media
	}
	log.Printf("LectureFlow mock UI: http://%s/?mock=1   (also ?mock=empty, nogpu, error, slow, diag, many; &theme=dark|light)", *addr)
	log.Fatal(http.ListenAndServe(*addr, h))
}

func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("mockserve only listens on a loopback address, got %q", addr)
}

func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found; run from the project or pass -root")
		}
		dir = parent
	}
}

type handler struct {
	http.Handler
	m *mock
}

type mock struct {
	dataDir  string
	main     *store
	empty    *store
	renum    *store    // scenario "renumbered" (kinds.go)
	courseUX lazyStore // scenario "courseux" (course_ux.go)
	battery  lazyStore // scenario "battery" (devices.go)
	noov     lazyStore // scenario "noov" (devices.go)
	cpuonly  lazyStore // scenario "cpuonly" (devices.go)
	// scenario "transcribe_only" (v31_transcribe_only.go)
	transcribeOnly lazyStore
	// the power scenarios (power.go)
	powerACDGPU           lazyStore
	powerACIntel          lazyStore
	powerBatteryNPU       lazyStore
	powerBatterySuspended lazyStore
	powerACSwitch         lazyStore
	powerAllSuspended     lazyStore
	oldFailures           lazyStore // scenario "old_failures" (r7.go)
	resNoGPU              resources.Snapshot
	loadErr               error
	mediaFile             string                 // -media: the file of GET /media (player.go)
	player                map[*store]*playerData // player.go: resume positions and chapters
}

func newHandler(webDir, dataDir string, tick time.Duration) http.Handler {
	m := &mock{dataDir: dataDir}
	now := time.Now()
	if m.main, m.loadErr = loadStore(dataDir, now, false); m.loadErr == nil {
		m.empty, m.loadErr = loadStore(dataDir, now, true)
	}
	if m.loadErr == nil {
		if m.renum, m.loadErr = loadStore(dataDir, now, false); m.loadErr == nil {
			setupRenumbered(m.renum, now)
		}
	}
	if m.loadErr == nil {
		m.loadErr = readShifted(dataDir, "v11_resources_nogpu.json", now.Sub(epoch()).Truncate(time.Second), &m.resNoGPU)
	}
	mux := http.NewServeMux()
	a := m.api
	for _, reg := range mockV3Routes { // v3_routes.go: v3 mock files register themselves
		reg(m, mux)
	}
	mux.HandleFunc("GET /api/events", m.events(tick))
	// perf-fix: the page reports whether it is shown; the mock does not sample.
	mux.HandleFunc("POST /api/events/visibility", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /api/system", a(m.getSystem))
	mux.HandleFunc("POST /api/system/recheck", a(m.getSystem))
	mux.HandleFunc("POST /api/system/gpu", a(m.setGPU))
	mux.HandleFunc("GET /api/resources", a(m.getResources))
	mux.HandleFunc("GET /api/dashboard", a(m.getDashboard))
	m.registerSettings(mux) // settings.go (A5.3)
	m.registerSyllabus(mux) // syllabus.go (A8, course materials and glossary)
	m.registerJanitor(mux)  // janitor.go (v2 G3: 整理术语表, report, undo)
	m.registerPlayer(mux)   // player.go (v2 P8.2: media, cues, chapters, resume)
	m.registerCourseP2(mux) // course_p2.go (v2 P12, P13: calendars, file-name rules, titles, holidays)
	m.registerKinds(mux)    // kinds.go (lecture kinds, numbering, slides by week)
	m.registerDevices(mux)  // devices.go (SPEC-v2 I6-I10: transcription devices, self-test)

	mux.HandleFunc("GET /api/courses", a(m.listCourses))
	mux.HandleFunc("GET /api/courses/{id}", a(m.getCourse))
	mux.HandleFunc("GET /api/courses/{id}/lectures", a(m.courseLectures))
	mux.HandleFunc("POST /api/courses/{id}/launch-drafts", a(m.launchDrafts))
	mux.HandleFunc("POST /api/courses/{id}/update-stale", a(m.updateStale))
	mux.HandleFunc("GET /api/courses/{id}/export/cite.json", a(m.courseCite))
	mux.HandleFunc("POST /api/courses/{id}/lectures", a(m.uploadLecture))

	mux.HandleFunc("GET /api/lectures/{id}", a(m.getLecture))
	mux.HandleFunc("PATCH /api/lectures/{id}", a(m.patchLecture))
	mux.HandleFunc("DELETE /api/lectures/{id}", a(m.deleteLecture))
	mux.HandleFunc("POST /api/lectures/{id}/launch", a(m.launch))
	mux.HandleFunc("POST /api/lectures/{id}/update", a(m.update))
	mux.HandleFunc("POST /api/lectures/{id}/stages/{stage}/{action}", a(m.stageAction))
	mux.HandleFunc("POST /api/lectures/launch", a(m.batch("launch")))
	mux.HandleFunc("POST /api/lectures/cancel", a(m.batch("cancel")))
	mux.HandleFunc("POST /api/lectures/update", a(m.batch("update")))
	mux.HandleFunc("POST /api/lectures/delete", a(m.batch("delete")))
	mux.HandleFunc("GET /api/lectures/{id}/sentences", a(m.sentences))
	mux.HandleFunc("GET /api/lectures/{id}/patches", a(m.patches))
	mux.HandleFunc("POST /api/lectures/{id}/sentences/{idx}/{op}", a(m.sentenceOp))
	mux.HandleFunc("POST /api/patches/{id}/review", a(m.review))
	mux.HandleFunc("GET /api/lectures/{id}/audio", m.audio)
	mux.HandleFunc("GET /api/lectures/{id}/export/{file}", m.exportFile)

	mux.HandleFunc("/api/", notImplemented)
	mux.Handle("GET /testdata/api/", http.StripPrefix("/testdata/api/", static(dataDir)))
	mux.Handle("/", rootHandler(static(webDir)))
	return &handler{Handler: mux, m: m}
}

// ---------- scenarios ----------

func scenario(r *http.Request) string {
	if s := r.URL.Query().Get("mock"); s != "" {
		return s
	}
	return "1"
}

func (m *mock) storeFor(r *http.Request) *store {
	if s, ok := m.diagStore(r); ok { // ext-step1 (ext_diag.go)
		return s
	}
	if s, ok := m.sshStore(r); ok { // ssh-1b (ssh.go): the SSH servers' device rows
		return s
	}
	switch scenario(r) {
	case "empty":
		return m.empty
	case "renumbered":
		return m.renum
	case "courseux": // course_ux.go
		return m.courseUX.get(m, setupCourseUX)
	case "battery":
		return m.battery.get(m, func(s *store) {
			pct := 64
			s.dev.power = powerOf(true, true, false, &pct)
		})
	case "noov":
		return m.noov.get(m, func(s *store) { s.dev.noOV = true })
	case "cpuonly":
		return m.cpuonly.get(m, setupCPUOnly)
	case "transcribe_only":
		return m.transcribeOnly.get(m, setupTranscribeOnly)
	case "power_ac_dgpu":
		return m.powerACDGPU.get(m, setupPowerACDGPU)
	case "power_ac_intel":
		return m.powerACIntel.get(m, setupPowerACIntel)
	case "power_battery_npu":
		return m.powerBatteryNPU.get(m, setupPowerBatteryNPU)
	case "power_battery_suspended":
		return m.powerBatterySuspended.get(m, setupPowerBatterySuspended)
	case "power_ac_switch":
		return m.powerACSwitch.get(m, setupPowerACSwitch)
	case "power_all_suspended":
		return m.powerAllSuspended.get(m, setupPowerAllSuspended)
	case "old_failures":
		return m.oldFailures.get(m, setupOldFailures)
	}
	return m.main
}

// lazyStore is a scenario's own store, loaded on first use.
type lazyStore struct {
	once sync.Once
	s    *store
}

func (l *lazyStore) get(m *mock, setup func(*store)) *store {
	l.once.Do(func() {
		s, err := loadStore(m.dataDir, time.Now(), false)
		if err != nil {
			l.s = m.main
			return
		}
		setup(s)
		l.s = s
	})
	return l.s
}

// failingInErrorScenario: the reads whose cards show an error state.
func failingInErrorScenario(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	return p == "/api/dashboard" || p == "/api/resources" || (strings.HasPrefix(p, "/api/courses/") && strings.HasSuffix(p, "/lectures"))
}

func (m *mock) api(fn func(http.ResponseWriter, *http.Request, *store)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.loadErr != nil {
			writeError(w, http.StatusInternalServerError, "mock：无法读取示例数据："+m.loadErr.Error())
			return
		}
		switch sc := scenario(r); {
		case sc == "slow" && r.Method == http.MethodGet:
			select {
			case <-time.After(1500 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		case sc == "error" && failingInErrorScenario(r):
			writeError(w, http.StatusInternalServerError, "服务器内部错误：mock 模拟的失败（?mock=error）")
			return
		}
		fn(w, r, m.storeFor(r))
	}
}

// ---------- static files ----------

// Explicit types: on Windows the registry can map .js to text/plain, which
// browsers refuse for module scripts.
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".svg":  "image/svg+xml",
}

func static(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ext := path.Ext(r.URL.Path)
		if r.URL.Path == "/" || r.URL.Path == "" {
			ext = ".html"
		}
		if ct, ok := contentTypes[ext]; ok {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", "no-store")
		fs.ServeHTTP(w, r)
	})
}

// "/" without ?mock redirects to "/?mock=1" so api.js starts in mock mode.
func rootHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "mock 服务器只支持 GET 静态文件")
			return
		}
		if r.URL.Path == "/" && !r.URL.Query().Has("mock") {
			http.Redirect(w, r, "/?mock=1", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, fmt.Sprintf("mock 服务器没有实现 %s %s", r.Method, r.URL.Path))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError answers with the v2 error body (P14.3). The mock's own errors
// have no code: the front end shows their text (detail), as it does for any
// code it has no translation for.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, api.ErrorBody{Error: model.ErrorInfo{Detail: msg}})
}

func writeErr(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		if he.body != nil {
			writeJSON(w, he.status, he.body)
			return
		}
		writeError(w, he.status, he.msg)
		return
	}
	writeError(w, http.StatusInternalServerError, "服务器内部错误："+err.Error())
}

// decodeBody decodes an optional JSON body (an empty body leaves v unchanged).
func decodeBody(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return errBad("无法读取请求体")
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errBad("请求体不是有效的 JSON：" + err.Error())
	}
	return nil
}

func (m *mock) readExample(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(m.dataDir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ---------- system and resources ----------

func (m *mock) system(r *http.Request, s *store) (api.System, error) {
	name := "mock_system_ok.json"
	if scenario(r) == "diag" {
		name = "system.json"
	}
	var sys api.System
	if err := m.readExample(name, &sys); err != nil {
		return sys, err
	}
	sys.GPU.Paused = s.paused
	sys.GPU.SlotsUsed = 0
	for _, l := range s.lectures {
		if stageStatus(&l, model.StageASR) == model.StatusRunning {
			sys.GPU.SlotsUsed++
		}
	}
	if s.dev.cpuOnly { // no NVIDIA card (devices.go)
		sys.GPU.TotalMB, sys.GPU.FreeMB, sys.GPU.SlotsUsed = 0, 0, 0
	}
	// SPEC-v2 I10 (devices.go)
	now := time.Now()
	sys.Devices = s.deviceViews(now)
	p := s.powerView()
	sys.Power = &p
	sys.OpenVINO = s.openVINOInfo(now)
	sys.Checks = s.withDeviceChecks(sys.Checks)
	vramSystem(r, &sys)                     // r2 N15 (vram_hint.go)
	obDesktopChecks(r, &sys)                // ob_desktop (v3_accfix.go)
	sys.Config = mockConfigView(r, s, &sys) // 3.1.0 (v31_config.go)
	m.diagSystem(r, s, &sys)                // ext-step1 (ext_diag.go)
	m.web3System(r, &sys)                   // ext web3: aria2c, browser_ext (ext_web3.go)
	m.dataDirSystem(r, &sys)                // datadir-ui: who else can reach the data folder (datadir.go)
	m.installDirSystem(r, &sys)             // T-SEC-05: who else can change the installation (installdir.go)
	return sys, nil
}

func (m *mock) getSystem(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	sys, err := m.system(r, s)
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	sys.Checks = api.WithRoleModelChecks(sys.Checks, m.roleModelChecks(r, s))
	writeJSON(w, http.StatusOK, sys)
}

// roleModelChecks are the diagnostics rows of the real API for role models
// missing from their provider's probed list (api.RoleModelChecks), from
// the request's settings (scenario "oldmodel" has some; in the default data
// they appear once DeepSeek's models are probed, the mock's probe answering
// the current names). Called without s.mu held.
func (m *mock) roleModelChecks(r *http.Request, s *store) []api.SystemCheck {
	st, err := m.settingsStore(r, s)
	if err != nil {
		return nil
	}
	st.mu.Lock()
	v := st.settingsView()
	st.mu.Unlock()
	return api.RoleModelChecks(v.Roles, v.Providers)
}

func (m *mock) setGPU(w http.ResponseWriter, r *http.Request, s *store) {
	var in struct {
		Paused        bool `json:"paused"`
		CancelRunning bool `json:"cancel_running"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	s.paused = in.Paused
	if in.Paused && in.CancelRunning {
		for i := range s.lectures {
			l := &s.lectures[i]
			if j := stageIndex(l, model.StageASR); j >= 0 && l.Stages[j].Status == model.StatusRunning {
				l.Stages[j].Status = model.StatusCancelled
				s.changed(l)
			}
		}
	}
	s.notify(api.ChangedEvent{Kind: "system"})
	sys, err := m.system(r, s)
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	sys.Checks = api.WithRoleModelChecks(sys.Checks, m.roleModelChecks(r, s))
	writeJSON(w, http.StatusOK, sys)
}

func (m *mock) resourcesFor(r *http.Request, s *store) resources.Snapshot {
	base := s.res
	if scenario(r) == "nogpu" || s.dev.cpuOnly {
		base = m.resNoGPU
	} else if s.dev.noCUDA { // no NVIDIA card; the balances as usual
		base.GPU = m.resNoGPU.GPU
	}
	now := time.Now()
	s.advanceDevices(now)
	running := s.runningByDevice(now)
	res := s.withTariffStates(s.resources(base, now, len(running[model.DeviceCUDA0])), now, scenario(r)) // v2 P12.4 (course_p2.go)
	// power management (power.go): the policy, every device row, the
	// upper area's readings
	res.Policy = s.resourcesPolicy(now)
	res.Devices = s.deviceCards(res, now, res.Policy) // SPEC-v2 I10 (devices.go)
	p := s.powerView()
	res.Power = &p
	busy := false
	for _, ts := range running {
		busy = busy || len(ts) > 0
	}
	res.System = s.systemReadings(now, busy)
	return res
}

func (m *mock) getResources(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	res := m.resourcesFor(r, s)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, res)
}

func (m *mock) getDashboard(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	now := time.Now()
	s.advance(now)
	d := buildDashboard(s.courses, s.lectures, s.usage, now)
	d.DevicePrompts = s.devicePrompts() // SPEC-v2 I9 (devices.go)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, d)
}

// ---------- courses ----------

func (m *mock) listCourses(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]api.Course, 0, len(s.courses))
	for _, c := range s.courses {
		out = append(out, m.courseOut(r, s, c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *mock) getCourse(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.findCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m.courseOut(r, s, *c))
}

func (m *mock) courseLectures(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	if _, err := s.findCourse(id); err != nil {
		writeErr(w, err)
		return
	}
	s.advance(time.Now())
	writeJSON(w, http.StatusOK, s.courseLectures(id))
}

func modelChoice(r *http.Request) (string, error) {
	var in api.ModelChoiceRequest
	if err := decodeBody(r, &in); err != nil {
		return "", err
	}
	return in.ModelChoice, nil
}

func (m *mock) launchDrafts(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.findCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var ids []string
	for _, l := range s.courseLectures(c.ID) {
		if l.Draft {
			ids = append(ids, l.ID)
		}
	}
	now := time.Now()
	res := s.batch(ids, func(id string) (*api.Lecture, error) { return s.launch(id, now) })
	c, _ = s.findCourse(c.ID)
	writeJSON(w, http.StatusOK, api.CourseOpResult{Results: res.Results, Course: *c})
}

// updateStale asks once for the whole course (A2.3): the 409 lists the
// differences of every stale lecture; the chosen answer applies to all.
func (m *mock) updateStale(w http.ResponseWriter, r *http.Request, s *store) {
	choice, err := modelChoice(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.findCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var ids []string
	var diffs []api.ModelDiffView
	for _, l := range s.courseLectures(c.ID) {
		if l.StaleText != nil {
			ids = append(ids, l.ID)
			diffs = append(diffs, s.diffs(&l, earliestStale(&l), false)...)
		}
	}
	if choice == "" && len(diffs) > 0 {
		writeErr(w, needsChoice(diffs))
		return
	}
	res := s.batch(ids, func(id string) (*api.Lecture, error) { return s.update(id, choice) })
	c, _ = s.findCourse(c.ID)
	writeJSON(w, http.StatusOK, api.CourseOpResult{Results: res.Results, Course: *c})
}

func (m *mock) courseCite(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.findCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	code := ""
	if c.Code != nil {
		code = *c.Code
	}
	type citeSentence struct {
		ID      string  `json:"id"`
		Ref     string  `json:"ref"`
		StartMS int64   `json:"start_ms"`
		EndMS   int64   `json:"end_ms"`
		En      string  `json:"en"`
		Zh      *string `json:"zh"`
		Edited  bool    `json:"edited"`
	}
	type citeLecture struct {
		ID         string         `json:"id"`
		Seq        int            `json:"seq"`
		Label      string         `json:"label"`
		Title      string         `json:"title"`
		DurationMS *int64         `json:"duration_ms"`
		Summary    string         `json:"summary"`
		Sentences  []citeSentence `json:"sentences"`
	}
	var lecs []citeLecture
	for _, l := range s.courseLectures(c.ID) {
		if stageStatus(&l, model.StageExport) != model.StatusDone {
			continue
		}
		cl := citeLecture{ID: l.ID, Seq: l.Seq, Label: l.Label, Title: l.Title, DurationMS: l.DurationMS, Summary: "（mock 摘要）"}
		for i, sn := range s.sentences[:min(3, len(s.sentences))] {
			sec := sn.StartMS / 1000
			cl.Sentences = append(cl.Sentences, citeSentence{
				ID: fmt.Sprintf("%s.%s.s%04d", code, l.Label, i), Ref: fmt.Sprintf("%s %s@%02d:%02d", code, l.Label, sec/60, sec%60),
				StartMS: sn.StartMS, EndMS: sn.EndMS, En: sn.Text, Zh: sn.Zh, Edited: sn.HumanText != nil,
			})
		}
		lecs = append(lecs, cl)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.cite.json\"", strings.ReplaceAll(c.Name, "\"", "")))
	writeJSON(w, http.StatusOK, map[string]any{
		"schema":       "lectureflow.cite/v1",
		"how_to_cite":  "Cite statements with the ref field in square brackets, e.g. [CSC1003 L03@12:34]. Use the sentence id when an exact reference is needed.",
		"course":       map[string]any{"id": c.ID, "code": code, "name": c.Name},
		"lectures":     lecs,
		"glossary":     []map[string]string{{"en": "block cipher", "zh": "分组密码"}},
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// ---------- lectures ----------

func (m *mock) getLecture(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance(time.Now())
	l, err := s.findLecture(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m.lectureJSON(r, s, l)) // ext web3: the second angle (ext_web3.go)
}

func (m *mock) patchLecture(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.LecturePatch
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.patchLecture(r.PathValue("id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (m *mock) deleteLecture(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.deleteLecture(r.PathValue("id"), r.URL.Query().Get("force") == "1"); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *mock) launch(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.launch(r.PathValue("id"), time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (m *mock) update(w http.ResponseWriter, r *http.Request, s *store) {
	choice, err := modelChoice(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.update(r.PathValue("id"), choice)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (m *mock) stageAction(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.ModelChoiceRequest
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.stageActionWith(r.PathValue("id"), r.PathValue("stage"), r.PathValue("action"), in, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (m *mock) batch(op string) func(http.ResponseWriter, *http.Request, *store) {
	return func(w http.ResponseWriter, r *http.Request, s *store) {
		var in api.BatchRequest
		if err := decodeBody(r, &in); err != nil {
			writeErr(w, err)
			return
		}
		if len(in.IDs) == 0 {
			writeError(w, http.StatusBadRequest, "ids 不能为空")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now()
		if op == "update" && in.ModelChoice == "" {
			var diffs []api.ModelDiffView
			for _, id := range in.IDs {
				if l, err := s.findLecture(id); err == nil && earliestStale(l) >= 0 {
					diffs = append(diffs, s.diffs(l, earliestStale(l), false)...)
				}
			}
			if len(diffs) > 0 {
				writeErr(w, needsChoice(diffs))
				return
			}
		}
		res := s.batch(in.IDs, func(id string) (*api.Lecture, error) {
			switch op {
			case "launch":
				return s.launch(id, now)
			case "cancel":
				return s.cancel(id, now)
			case "update":
				return s.update(id, in.ModelChoice)
			}
			return nil, s.deleteLecture(id, in.Force)
		})
		writeJSON(w, http.StatusOK, res)
	}
}

// exportFile serves a stand-in for an export file. The Markdown transcript
// and the VTT files are built from the mock's sentences in the formats of
// the export stage (SPEC §6.6, P4.6; one cue per sentence here), so a
// converted download (P14.6 "下载（繁体）") has Chinese to convert; the
// other files are a one-line note.
func (m *mock) exportFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	s := m.main
	s.mu.Lock()
	ss := append([]api.Sentence(nil), s.sentences...)
	s.mu.Unlock()
	zh := func(x api.Sentence) string {
		if x.Zh == nil {
			return ""
		}
		return *x.Zh
	}
	clock := func(ms int64) string {
		return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
	}
	switch {
	case strings.HasSuffix(name, ".bilingual.md"):
		for _, x := range ss {
			fmt.Fprintf(w, "**[%02d:%02d]** %s\n\n%s\n\n", x.StartMS/60000, x.StartMS/1000%60, x.Text, zh(x))
		}
	case strings.HasSuffix(name, ".vtt"):
		fmt.Fprint(w, "WEBVTT\n\n")
		for _, x := range ss {
			text := zh(x)
			if strings.HasSuffix(name, ".bilingual.vtt") {
				text = x.Text + "\n" + text
			}
			fmt.Fprintf(w, "%s --> %s\n%s\n\n", clock(x.StartMS), clock(x.EndMS), text)
		}
	default:
		fmt.Fprintf(w, "mock 导出文件：%s\n（mock 服务器不生成真实内容）\n", name)
	}
}

// ---------- sentences, patches, review (SPEC §6.4, §17.1) ----------

func (s *store) sentenceList(r *http.Request) []api.Sentence {
	if scenario(r) != "many" {
		return s.sentences
	}
	base := s.sentences
	span := base[len(base)-1].EndMS
	out := make([]api.Sentence, 450)
	for i := range out {
		c := base[i%len(base)]
		shift := int64(i/len(base)) * span
		c.Idx, c.StartMS, c.EndMS = i, c.StartMS+shift, c.EndMS+shift
		if c.Patch != nil {
			p := *c.Patch
			p.ID += "-" + strconv.Itoa(i)
			c.Patch = &p
		}
		out[i] = c
	}
	return out
}

func (m *mock) sentences(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.sentenceList(r)
	off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if lim <= 0 {
		lim = 200
	}
	off = max(0, min(off, len(all)))
	end := min(len(all), off+lim)
	writeJSON(w, http.StatusOK, api.SentencePage{Total: len(all), Offset: off, Sentences: all[off:end]})
}

func (m *mock) patches(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	review := r.URL.Query().Get("review")
	out := []api.Patch{}
	for _, p := range s.patches {
		if review == "" || p.Review == review {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *mock) review(w http.ResponseWriter, r *http.Request, s *store) {
	var in struct {
		Action string  `json:"action"`
		Text   *string `json:"text"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.patches {
		p := &s.patches[i]
		if p.ID != r.PathValue("id") {
			continue
		}
		review := map[string]string{"accept": model.ReviewAccepted, "reject": model.ReviewRejected, "edit": model.ReviewEdited, "reset": model.ReviewPending}[in.Action]
		if review == "" {
			writeError(w, http.StatusBadRequest, "未知的审阅操作："+in.Action)
			return
		}
		p.EditedText = nil
		if in.Action == "edit" {
			if in.Text == nil || strings.TrimSpace(*in.Text) == "" {
				writeError(w, http.StatusBadRequest, "编辑后的文本不能为空")
				return
			}
			t := strings.TrimSpace(*in.Text)
			p.EditedText = &t
		}
		p.Review = review
		for j := range s.sentences {
			if s.sentences[j].Patch != nil && s.sentences[j].Patch.ID == p.ID {
				s.sentences[j].Patch.Review = review
			}
		}
		n := 3
		out := *p
		out.Retranslated = &n
		s.notify(api.ChangedEvent{Kind: "patches", ID: p.LectureID}) // lecture id, as the real hub sends
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeErr(w, errNotFound("修改"))
}

func (m *mock) sentenceOp(w http.ResponseWriter, r *http.Request, s *store) {
	var in struct {
		Text       string `json:"text"`
		UseCurrent bool   `json:"use_current"` // retranslate, v3.2.x (manual.go)
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "句子序号无效")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < 0 || idx >= len(s.sentences) {
		writeErr(w, errNotFound("句子"))
		return
	}
	sn := &s.sentences[idx]
	switch r.PathValue("op") {
	case "edit":
		t := strings.TrimSpace(in.Text)
		if t == "" {
			writeError(w, http.StatusBadRequest, "原文不能为空")
			return
		}
		sn.HumanText, sn.Text = &t, t
	case "reset":
		sn.HumanText, sn.Text = nil, sn.RepairedText
	case "retranslate":
		if in.UseCurrent {
			mixed, err := s.useCurrentModel(r.PathValue("id"), sn)
			if err != nil {
				writeErr(w, err)
				return
			}
			sn.ZhStatus = model.ZhStale
			writeJSON(w, http.StatusOK, api.RetranslateResult{Sentence: *sn, Mixed: &mixed})
			return
		}
	default:
		notImplemented(w, r)
		return
	}
	sn.ZhStatus = model.ZhStale
	writeJSON(w, http.StatusOK, sn)
}

// ---------- SSE (SPEC §8.2, v1.1 A10) ----------

const (
	mockLectureDone   = "01JD3311000000000000000003" // COMP3311 L03, all done
	mockLectureFailed = "01JD3121000000000000000006" // CSC1002 L06, prep failed for good
)

// events pushes: progress for every running stage and a "resources"
// snapshot each tick, the store's "changed" events as they happen, a
// "lecture finished" and a "stage failed" event early on (to exercise
// notifications), `changed system` every 5 ticks and a ping every 8 ticks.
func (m *mock) events(tick time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		if m.loadErr != nil {
			writeError(w, http.StatusInternalServerError, m.loadErr.Error())
			return
		}
		s := m.storeFor(r)
		changes, unsubscribe := s.subscribe()
		defer unsubscribe()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ": connected\n\n")
		fl.Flush()
		send := func(event string, v any) error {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			fl.Flush()
			return err
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for n := 0; ; n++ {
			if err := m.step(n, r, s, send, w, fl); err != nil {
				return
			}
		wait:
			for {
				select {
				case <-r.Context().Done():
					return
				case ev := <-changes:
					if send("changed", ev) != nil {
						return
					}
				case <-t.C:
					break wait
				}
			}
		}
	}
}

func (m *mock) step(n int, r *http.Request, s *store, send func(string, any) error, w io.Writer, fl http.Flusher) error {
	s.mu.Lock()
	s.advance(time.Now())
	var jobs []api.JobEvent
	for _, l := range s.lectures {
		for _, st := range l.Stages {
			if st.Status == model.StatusRunning {
				ev := api.JobEvent{Scope: model.ScopeLecture, ScopeID: l.ID, Stage: st.Stage, Status: st.Status, Progress: st.Progress}
				if st.LLM != nil || st.LastActivityAt != nil {
					// v3.2.x P1-c: a running job's stats as the stage view has them
					u := st.Stats
					ev.Stats, ev.LLM, ev.LastActivityAt = &u, st.LLM, st.LastActivityAt
				}
				jobs = append(jobs, ev)
			}
		}
	}
	res := m.resourcesFor(r, s)
	s.mu.Unlock()
	for _, ev := range jobs {
		if err := send("job", ev); err != nil {
			return err
		}
	}
	if scenario(r) != "error" { // the inspector shows its error state there
		if err := send("resources", res); err != nil {
			return err
		}
	}
	if scenario(r) != "empty" {
		switch n {
		case 3:
			if err := send("job", api.JobEvent{Scope: model.ScopeLecture, ScopeID: mockLectureDone, Stage: model.StageExport, Status: model.StatusDone, Progress: 1}); err != nil {
				return err
			}
		case 4:
			if err := send("job", api.JobEvent{Scope: model.ScopeLecture, ScopeID: mockLectureFailed, Stage: model.StagePrep, Status: model.StatusFailed}); err != nil {
				return err
			}
		}
	}
	if n > 0 && n%5 == 0 {
		if err := send("changed", api.ChangedEvent{Kind: "system"}); err != nil {
			return err
		}
	}
	if n > 0 && n%8 == 0 {
		if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
			return err
		}
		fl.Flush()
	}
	return nil
}

// ---------- audio clips (SPEC §8: the interval plus 1 second on each side; v1.1 A13.3 headers) ----------

const sampleRate = 16000

// audio returns a quiet tone as 16 kHz mono 16-bit WAV, as long as the real
// clip would be; the pitch depends on start_ms so neighbouring sentences
// sound different. The X-Clip-* headers give the absolute clip and sentence
// bounds (A13.3), so the front end never hard-codes the 1 s padding.
func (m *mock) audio(w http.ResponseWriter, r *http.Request) {
	start, err1 := strconv.ParseInt(r.URL.Query().Get("start_ms"), 10, 64)
	end, err2 := strconv.ParseInt(r.URL.Query().Get("end_ms"), 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		writeError(w, http.StatusBadRequest, "start_ms / end_ms 无效")
		return
	}
	if scenario(r) == "slow" { // like every other GET: preview the bar's loading state
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	clipStart, clipEnd := max(start-1000, 0), end+1000
	ms := min(max(clipEnd-clipStart, 500), 60000)
	n := int(ms * sampleRate / 1000)
	freq := 330 + float64((start/1000)%12)*30
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.Itoa(44+2*n))
	w.Header().Set("X-Clip-Start-Ms", strconv.FormatInt(clipStart, 10))
	w.Header().Set("X-Clip-Sentence-Start-Ms", strconv.FormatInt(start, 10))
	w.Header().Set("X-Clip-Sentence-End-Ms", strconv.FormatInt(end, 10))
	w.Header().Set("X-Clip-End-Ms", strconv.FormatInt(clipStart+ms, 10))
	_, _ = w.Write(wavHeader(n))
	buf := make([]byte, 0, 2*sampleRate)
	for i := 0; i < n; i++ {
		t := float64(i) / sampleRate
		env := math.Min(1, math.Min(t/0.05, float64(n-i)/sampleRate/0.05))
		v := int16(2500 * env * math.Sin(2*math.Pi*freq*t))
		buf = binary.LittleEndian.AppendUint16(buf, uint16(v))
		if len(buf) == cap(buf) {
			if _, err := w.Write(buf); err != nil {
				return
			}
			buf = buf[:0]
		}
	}
	_, _ = w.Write(buf)
}

func wavHeader(samples int) []byte {
	data := uint32(samples * 2)
	b := make([]byte, 0, 44)
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, 36+data)
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)           // fmt chunk size
	b = binary.LittleEndian.AppendUint16(b, 1)            // PCM
	b = binary.LittleEndian.AppendUint16(b, 1)            // mono
	b = binary.LittleEndian.AppendUint32(b, sampleRate)   // sample rate
	b = binary.LittleEndian.AppendUint32(b, sampleRate*2) // byte rate
	b = binary.LittleEndian.AppendUint16(b, 2)            // block align
	b = binary.LittleEndian.AppendUint16(b, 16)           // bits per sample
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, data)
	return b
}

// ---------- uploads ----------

// drain reads a multipart body part by part without buffering files
// (the same streaming rule as SPEC §17.4) and returns the text fields and
// the uploaded file's name and size.
//
// A JSON body {"path": ..., fields...} is the desktop app's native file
// handling (SPEC-v3 V1.4): the mock takes the file name from the path and
// reads nothing.
func drain(r *http.Request) (fields map[string]string, filename string, size int64, err error) {
	if name, f, ok, err := pathBody(r); ok || err != nil {
		return f, name, 0, err
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", 0, err
	}
	fields = map[string]string{}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", 0, err
		}
		if p.FileName() != "" {
			filename = p.FileName()
			n, err := io.Copy(io.Discard, p)
			if err != nil {
				return nil, "", 0, err
			}
			size += n
			continue
		}
		b, err := io.ReadAll(io.LimitReader(p, 64<<10))
		if err != nil {
			return nil, "", 0, err
		}
		fields[p.FormName()] = string(b)
	}
	if filename == "" {
		return nil, "", 0, errors.New("缺少文件")
	}
	return fields, filename, size, nil
}

// pathBody decodes a desktop path upload: ok is false for other bodies.
func pathBody(r *http.Request) (filename string, fields map[string]string, ok bool, err error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "application/json" {
		return "", nil, false, nil
	}
	var raw map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&raw); err != nil {
		return "", nil, false, err
	}
	p, _ := raw["path"].(string)
	if p == "" {
		return "", nil, false, errors.New("缺少文件路径")
	}
	fields = map[string]string{}
	for k, v := range raw {
		if s, isStr := v.(string); isStr && k != "path" {
			fields[k] = s
		}
	}
	return pathBase(p), fields, true, nil
}

// pathBase is the file name of a Windows or POSIX path.
func pathBase(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func simulatedFailure(s string) bool {
	return strings.Contains(strings.ToLower(s), "fail")
}

// uploadLecture creates a draft (v1.1 A3: an upload no longer starts the pipeline).
func (m *mock) uploadLecture(w http.ResponseWriter, r *http.Request, s *store) {
	fields, filename, _, err := drain(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无法解析上传内容："+err.Error())
		return
	}
	title := strings.TrimSpace(fields["title"])
	if title == "" {
		writeError(w, http.StatusBadRequest, "标题不能为空")
		return
	}
	if simulatedFailure(title) || simulatedFailure(filename) {
		writeError(w, http.StatusInternalServerError, "mock：模拟的上传失败（标题或文件名包含 fail）")
		return
	}
	s.mu.Lock()
	lec := s.addDraft(r.PathValue("id"), title, time.Now())
	// P13.3: the course's file-name rule gives the recording time and
	// (lecture kinds, kinds.go) the kind of the new lecture
	if l, err := s.findLecture(lec.ID); err == nil && filename != "" {
		if c, err := s.findCourse(l.CourseID); err == nil {
			l.OriginalFilename = filename
			l.RecordedAt, l.RecordedAtSource = s.automaticRecordedAt(c, l)
			s.finalize(l)
			s.fnChanged(c)
			lec = *l
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, lec)
}
