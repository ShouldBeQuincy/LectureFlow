// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// ext web3 (web-builder, 2026-10-27): the mock side of the second camera
// angle and of the diagnostics rows aria2c and browser_ext. The interfaces
// were frozen by the lead; the real API is core3's (handlers_angles.go,
// system_ext.go), whose codes the mock uses.
//
// Second angle (the player's picture in picture, the lecture page's
// "添加第二视角"):
//
//	GET    /api/lectures/{id}                 the lecture with Lecture.angles
//	                                          ([{"name": "angle2"}]; absent when none)
//	GET    /api/lectures/{id}/angles/{name}   the angle's video, Range requests, no
//	                                          audio: the -angle2 file, else the -media
//	                                          file; 404 angle.not_found {name}
//	POST   /api/lectures/{id}/angles          {"path": "<local file>"} or a multipart
//	                                          "file" → 200 the lecture, after
//	                                          angleCopyDelay; replaces one that is
//	                                          there. Errors: no file upload.no_file, a
//	                                          relative path upload.path_invalid; a name
//	                                          containing "missing" upload.path_not_found,
//	                                          "locked" upload.path_unreadable, "audio"
//	                                          angle.no_video, "fail" angle.remux_failed
//	DELETE /api/lectures/{id}/angles/{name}   → 200 the lecture; 404 angle.not_found
//
// Scenario "angles": CSC3631 L01 (video, chapters) has angle2. Angles added
// or removed belong to the store, so every scenario sharing it (1, angles,
// ext_*) sees them until the server restarts. Only GET /api/lectures/{id}
// and these routes carry Lecture.angles (the real lists do as well).
//
// Diagnostics: GET /api/system has the rows aria2c (after ffmpeg) and
// browser_ext (before the providers) in the scenarios ext_*, built like
// internal/api/system_ext.go builds them; GET /api/nmhost and
// POST /api/nmhost/register answer in every scenario (ext_ok's state
// outside ext_*):
//
//	ext_ok           aria2c 1.37.0 matches the manifest; Chrome and Edge
//	                 registered, Firefox not installed; Chrome's extension
//	                 connected 4 minutes ago
//	ext_unregistered Chrome and Edge not registered (warn), no extension has
//	                 connected; "重新登记" registers them (the row turns ok)
//	ext_broken       aria2c's SHA-256 differs from the manifest (error); the
//	                 manifests point to a host program that is missing,
//	                 Firefox is not registered, registering again fails
//	ext_unmanaged    an isolated instance on its own endpoint: managed false,
//	                 POST /api/nmhost/register 503
//	ext_nmoff        the service has not started: /api/nmhost 503
//
// A scenario whose own data already has these rows gets them replaced, not
// doubled. The other scenarios keep their rows.

import (
	"errors"
	"flag"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/nmhost"
)

var angle2Flag = flag.String("angle2", "", "MP4 served as every second angle's video (GET /api/lectures/{id}/angles/{name}; default: the -media file) (ext_web3.go)")

// angleCopyDelay is how long POST /angles takes (tests shorten it).
var angleCopyDelay = 1200 * time.Millisecond

// The lecture with a second angle in scenario "angles" (CSC3631 L01).
const angleLecture = mockLectureChapters

const angleName = "angle2"

// web3State is a mock's state of this file (main.go's mock has no field
// for it: this file only adds).
type web3State struct {
	mu sync.Mutex
	// angles changed with POST / DELETE: store → lecture ID → present
	// (true) or removed (false; overrides scenario "angles")
	angles map[*store]map[string]bool
	nm     map[string]*nmhost.Status // scenario → its Native Messaging status
}

var web3States sync.Map // *mock → *web3State

func (m *mock) web3() *web3State {
	v, _ := web3States.LoadOrStore(m, &web3State{angles: map[*store]map[string]bool{}, nm: map[string]*nmhost.Status{}})
	return v.(*web3State)
}

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/lectures/{id}/angles/{name}", m.serveAngle)
		mux.HandleFunc("POST /api/lectures/{id}/angles", m.api(m.addAngle))
		mux.HandleFunc("DELETE /api/lectures/{id}/angles/{name}", m.api(m.deleteAngle))
		mux.HandleFunc("GET /api/nmhost", m.api(m.getNMHost))
		mux.HandleFunc("POST /api/nmhost/register", m.api(m.registerNMHost))
	})
}

// ---------- angles ----------

// angles are the angle names of a lecture in this request's scenario.
func (m *mock) angles(r *http.Request, s *store, id string) []string {
	st := m.web3()
	st.mu.Lock()
	v, set := st.angles[s][id]
	st.mu.Unlock()
	if set && v || !set && scenario(r) == "angles" && id == angleLecture {
		return []string{angleName}
	}
	return nil
}

func (m *mock) setAngle(s *store, id string, present bool) {
	st := m.web3()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.angles[s] == nil {
		st.angles[s] = map[string]bool{}
	}
	st.angles[s][id] = present
}

// lectureJSON is a lecture as GET /api/lectures/{id} answers it: a copy
// with its angles.
func (m *mock) lectureJSON(r *http.Request, s *store, l *api.Lecture) *api.Lecture {
	out := *l
	if r.Method == http.MethodGet && r.URL.Query().Get("usage_total") == "1" {
		out = *s.withUsageTotal(l) // asked for by the lecture page (r7.go)
	}
	out.Angles = nil
	for _, n := range m.angles(r, s, l.ID) {
		out.Angles = append(out.Angles, api.Angle{Name: n})
	}
	return &out
}

func angleNotFound(name string) error {
	return codedErr(http.StatusNotFound, "angle.not_found", map[string]any{"name": name}, "")
}

func (m *mock) serveAngle(w http.ResponseWriter, r *http.Request) {
	if m.loadErr != nil {
		writeError(w, http.StatusInternalServerError, "mock：无法读取示例数据："+m.loadErr.Error())
		return
	}
	s := m.storeFor(r)
	s.mu.Lock()
	_, err := s.findLecture(r.PathValue("id"))
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	name := r.PathValue("name")
	if !slices.Contains(m.angles(r, s, r.PathValue("id")), name) {
		writeErr(w, angleNotFound(name))
		return
	}
	file := *angle2Flag
	if file == "" {
		file = m.mediaFile
	}
	if file == "" {
		writeError(w, http.StatusNotFound, "mock：没有用 -angle2 或 -media 指定视频文件")
		return
	}
	f, err := os.Open(file)
	if err != nil {
		writeError(w, http.StatusNotFound, "mock：无法打开视角的视频文件："+err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, filepath.Base(file), info.ModTime(), f)
}

// angleSource reads POST /angles: the path of a JSON body, or the file
// name of a multipart "file" (its bytes are discarded).
func angleSource(r *http.Request) (name string, path bool, err error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		var in struct {
			Path string `json:"path"`
		}
		if err := decodeBody(r, &in); err != nil {
			return "", true, codedErr(http.StatusBadRequest, "request.bad_json", nil, "")
		}
		return strings.TrimSpace(in.Path), true, nil
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return "", false, codedErr(http.StatusBadRequest, "upload.not_multipart", nil, "")
	}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return name, false, nil
		}
		if err != nil {
			return "", false, codedErr(http.StatusBadRequest, "upload.interrupted", nil, "")
		}
		if p.FormName() == "file" && p.FileName() != "" {
			name = p.FileName()
		}
		if _, err := io.Copy(io.Discard, p); err != nil {
			return "", false, codedErr(http.StatusBadRequest, "upload.interrupted", nil, "")
		}
	}
}

// absPath: an absolute path on Windows or Unix, whatever the mock runs on.
func absPath(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\\`) || len(p) > 2 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

func (m *mock) addAngle(w http.ResponseWriter, r *http.Request, s *store) {
	id := r.PathValue("id")
	s.mu.Lock()
	_, err := s.findLecture(id)
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	src, isPath, err := angleSource(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	low := strings.ToLower(src)
	switch {
	case src == "":
		writeErr(w, codedErr(http.StatusBadRequest, "upload.no_file", nil, ""))
		return
	case isPath && !absPath(src):
		writeErr(w, codedErr(http.StatusBadRequest, "upload.path_invalid", map[string]any{"path": src}, ""))
		return
	case isPath && strings.Contains(low, "missing"):
		writeErr(w, codedErr(http.StatusNotFound, "upload.path_not_found", map[string]any{"path": src}, ""))
		return
	case isPath && strings.Contains(low, "locked"):
		writeErr(w, codedErr(http.StatusForbidden, "upload.path_unreadable", map[string]any{"path": src}, "The process cannot access the file because it is being used by another process."))
		return
	}
	select { // ffmpeg remuxes the video stream
	case <-time.After(angleCopyDelay):
	case <-r.Context().Done():
		return
	}
	switch {
	case strings.Contains(low, "audio"):
		writeErr(w, codedErr(http.StatusBadRequest, "angle.no_video", nil, ""))
		return
	case strings.Contains(low, "fail"):
		writeErr(w, codedErr(http.StatusBadRequest, "angle.remux_failed", nil, "mock: Could not find tag for codec in stream #0, codec not currently supported in container"))
		return
	}
	m.setAngle(s, id, true)
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.findLecture(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.changed(l)
	writeJSON(w, http.StatusOK, m.lectureJSON(r, s, l))
}

func (m *mock) deleteAngle(w http.ResponseWriter, r *http.Request, s *store) {
	id, name := r.PathValue("id"), r.PathValue("name")
	s.mu.Lock()
	_, err := s.findLecture(id)
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	if !slices.Contains(m.angles(r, s, id), name) {
		writeErr(w, angleNotFound(name))
		return
	}
	m.setAngle(s, id, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.findLecture(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.changed(l)
	writeJSON(w, http.StatusOK, m.lectureJSON(r, s, l))
}

// ---------- diagnostics: aria2c, browser_ext ----------

var web3Scenarios = []string{"ext_ok", "ext_unregistered", "ext_broken", "ext_unmanaged", "ext_nmoff", "ext_mac", "ext_dmg"}

const (
	mockHostPath    = `C:\Program Files\LectureFlow\resources\bin\lectureflow-nmhost.exe`
	mockAria2cSHA   = "be2099c214f63a3cb4954b09a0becd6e2e34660b886d4c898d260febfe9d70c2"
	mockAria2cBad   = "0f5e7cd1b2a1c4cd5e2e1a9bd2d4c3b8a1e9f7d6c5b4a39281706f5e4d3c2b1a"
	mockAria2cVer   = "aria2 version 1.37.0"
	mockExtVersion  = "0.3.0"
	mockManifestDir = `C:\Users\student\AppData\Roaming\LectureFlow\nmhost\`
)

// The macOS scenarios (mac-fix): ext_mac has Chrome blocked by the system
// (Full Disk Access), Edge installed but never opened (not_ready) and
// Firefox registered, and "重新登记" changes none of it; ext_dmg runs from
// the disk image (launcher_blocked), everything else fine.
const (
	mockMacHost         = "/Users/student/Library/Application Support/LectureFlow/nmhost/bin/lectureflow-nmhost"
	mockMacFirefoxNMH   = "/Users/student/Library/Application Support/Mozilla/NativeMessagingHosts/org.lectureflow.nmhost.json"
	mockMacChromeDenied = "chrome: open /Users/student/Library/Application Support/Google/Chrome/NativeMessagingHosts/.org.lectureflow.nmhost.json.123.tmp: operation not permitted"
)

func mockMacRegister(at time.Time) *nmhost.RegisterResult {
	return &nmhost.RegisterResult{At: at, Notes: []string{mockMacChromeDenied},
		Blocked: []string{nmhost.BrowserChrome}, NotReady: []string{nmhost.BrowserEdge}}
}

func mockManifest(browser string) string {
	if browser == nmhost.BrowserFirefox {
		return mockManifestDir + "org.lectureflow.nmhost.firefox.json"
	}
	return mockManifestDir + "org.lectureflow.nmhost.chromium.json"
}

// nmStatus is the scenario's Native Messaging status (created on first use,
// changed by "重新登记"). Must hold st.mu.
func (st *web3State) nmStatus(sc string) *nmhost.Status {
	if v := st.nm[sc]; v != nil {
		return v
	}
	now := time.Now().UTC()
	v := &nmhost.Status{
		Managed: true, Host: mockHostPath, HostPresent: true, AutoStart: true, Listening: true,
		Browsers: []nmhost.BrowserStatus{
			{Browser: nmhost.BrowserChrome, State: nmhost.StateRegistered, Manifest: mockManifest(nmhost.BrowserChrome)},
			{Browser: nmhost.BrowserEdge, State: nmhost.StateRegistered, Manifest: mockManifest(nmhost.BrowserEdge)},
			{Browser: nmhost.BrowserFirefox, State: nmhost.StateNotInstalled},
		},
		LastConnection: &nmhost.HelloInfo{Browser: nmhost.BrowserChrome, ExtensionVersion: mockExtVersion, Protocol: 1, Accepted: true, At: now.Add(-4 * time.Minute)},
		LastRegister:   &nmhost.RegisterResult{At: now.Add(-2 * time.Hour)},
	}
	switch sc {
	case "ext_unregistered":
		v.Browsers[0].State, v.Browsers[0].Manifest = nmhost.StateNotRegistered, ""
		v.Browsers[1].State, v.Browsers[1].Manifest = nmhost.StateNotRegistered, ""
		v.LastConnection = nil
		v.LastRegister = nil
	case "ext_broken":
		v.Browsers[0].State = nmhost.StateHostMissing
		v.Browsers[1].State = nmhost.StateHostMissing
		v.Browsers[2] = nmhost.BrowserStatus{Browser: nmhost.BrowserFirefox, State: nmhost.StateNotRegistered}
		v.HostPresent = false
		v.LastConnection = &nmhost.HelloInfo{Browser: nmhost.BrowserEdge, ExtensionVersion: mockExtVersion, Protocol: 1, Accepted: true, At: now.Add(-26 * time.Minute)}
		v.LastRegister = &nmhost.RegisterResult{At: now.Add(-3 * time.Minute), Error: "host program not found: " + mockHostPath}
	case "ext_unmanaged":
		v.Managed = false
		v.Browsers = []nmhost.BrowserStatus{}
		v.LastRegister = nil
		v.LastConnection = nil
	case "ext_mac":
		v.Host = mockMacHost
		v.Browsers = []nmhost.BrowserStatus{
			{Browser: nmhost.BrowserChrome, State: nmhost.StateBlocked},
			{Browser: nmhost.BrowserEdge, State: nmhost.StateNotReady},
			{Browser: nmhost.BrowserFirefox, State: nmhost.StateRegistered, Manifest: mockMacFirefoxNMH},
		}
		v.LastConnection = &nmhost.HelloInfo{Browser: nmhost.BrowserFirefox, ExtensionVersion: mockExtVersion, Protocol: 1, Accepted: true, At: now.Add(-9 * time.Minute)}
		v.LastRegister = mockMacRegister(now.Add(-time.Hour))
	case "ext_dmg":
		v.Host = mockMacHost
		v.AutoStart = false
		v.LauncherBlocked = nmhost.LauncherDiskImage
		v.Browsers[2] = nmhost.BrowserStatus{Browser: nmhost.BrowserFirefox, State: nmhost.StateRegistered, Manifest: mockMacFirefoxNMH}
	}
	st.nm[sc] = v
	return v
}

// nmScenario is the scenario whose status a request reads (ext_ok's outside ext_*).
func nmScenario(r *http.Request) string {
	if sc := scenario(r); slices.Contains(web3Scenarios, sc) {
		return sc
	}
	return "ext_ok"
}

func (m *mock) getNMHost(w http.ResponseWriter, r *http.Request, _ *store) {
	sc := nmScenario(r)
	if sc == "ext_nmoff" {
		writeErr(w, codedErr(http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "nmhost"}, ""))
		return
	}
	st := m.web3()
	st.mu.Lock()
	v := *st.nmStatus(sc)
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

func (m *mock) registerNMHost(w http.ResponseWriter, r *http.Request, s *store) {
	sc := nmScenario(r)
	if sc == "ext_nmoff" || sc == "ext_unmanaged" {
		writeErr(w, codedErr(http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "nmhost"}, ""))
		return
	}
	select { // writing the manifests and the registry keys
	case <-time.After(angleCopyDelay / 2):
	case <-r.Context().Done():
		return
	}
	st := m.web3()
	st.mu.Lock()
	v := st.nmStatus(sc)
	now := time.Now().UTC()
	if sc == "ext_broken" {
		v.LastRegister = &nmhost.RegisterResult{At: now, Error: "host program not found: " + mockHostPath}
	} else if sc == "ext_mac" {
		v.LastRegister = mockMacRegister(now) // the user has not acted yet
	} else {
		for i := range v.Browsers {
			if v.Browsers[i].State != nmhost.StateNotInstalled {
				v.Browsers[i].State = nmhost.StateRegistered
				v.Browsers[i].Manifest = mockManifest(v.Browsers[i].Browser)
			}
		}
		v.LastRegister = &nmhost.RegisterResult{At: now}
	}
	out := *v
	st.mu.Unlock()
	s.mu.Lock()
	s.notify(api.ChangedEvent{Kind: "system"})
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func codedCheck(name, status, code string, params map[string]any) api.SystemCheck {
	return api.SystemCheck{Name: name, Status: status, DetailInfo: &model.ErrorInfo{Code: code, Params: params}}
}

func mockBrowserName(b string) string {
	switch b {
	case nmhost.BrowserChrome:
		return "Chrome"
	case nmhost.BrowserEdge:
		return "Edge"
	case nmhost.BrowserFirefox:
		return "Firefox"
	}
	return b
}

// browserExtRow follows internal/api/system_ext.go checkBrowserExt with
// the desktop hints (nil v: the service has not started).
func browserExtRow(v *nmhost.Status) api.SystemCheck {
	if v == nil {
		c := codedCheck("browser_ext", "warn", "check.browser_ext_unavailable", nil)
		c.HintInfo = &model.ErrorInfo{Code: "check.hint_browser_ext_restart"}
		return c
	}
	if !v.Managed {
		return codedCheck("browser_ext", "ok", "check.browser_ext_unmanaged", nil)
	}
	if l := v.LastConnection; l != nil && !l.Accepted {
		c := codedCheck("browser_ext", "warn", "check.browser_ext_protocol", map[string]any{"browser": mockBrowserName(l.Browser), "version": l.ExtensionVersion})
		c.HintInfo = &model.ErrorInfo{Code: "check.hint_browser_ext_update"}
		return c
	}
	var installed, bad, missing, blocked, notReady []string
	for _, b := range v.Browsers {
		switch b.State {
		case nmhost.StateNotInstalled:
			continue
		case nmhost.StateHostMissing:
			missing = append(missing, mockBrowserName(b.Browser))
		case nmhost.StateNotRegistered:
			bad = append(bad, mockBrowserName(b.Browser))
		case nmhost.StateBlocked:
			blocked = append(blocked, mockBrowserName(b.Browser))
		case nmhost.StateNotReady:
			notReady = append(notReady, mockBrowserName(b.Browser))
		}
		installed = append(installed, mockBrowserName(b.Browser))
	}
	if len(installed) == 0 {
		return codedCheck("browser_ext", "ok", "check.browser_ext_no_browser", nil)
	}
	var c api.SystemCheck
	switch {
	case len(blocked) > 0:
		c = codedCheck("browser_ext", "warn", "check.browser_ext_blocked", map[string]any{"browsers": strings.Join(blocked, ", ")})
		c.HintInfo = &model.ErrorInfo{Code: "check.hint_browser_ext_full_disk"}
	case len(missing) > 0 || !v.HostPresent:
		c = codedCheck("browser_ext", "warn", "check.browser_ext_host_missing", map[string]any{"path": v.Host})
		c.HintInfo = &model.ErrorInfo{Code: "check.hint_browser_ext_register"}
	case len(bad) > 0:
		c = codedCheck("browser_ext", "warn", "check.browser_ext_not_registered", map[string]any{"browsers": strings.Join(bad, ", ")})
		c.HintInfo = &model.ErrorInfo{Code: "check.hint_browser_ext_register"}
	case len(notReady) > 0:
		names := strings.Join(notReady, ", ")
		c = codedCheck("browser_ext", "warn", "check.browser_ext_not_ready", map[string]any{"browsers": names})
		c.HintInfo = &model.ErrorInfo{Code: "check.hint_browser_ext_open_browser", Params: map[string]any{"browsers": names}}
	case v.LauncherBlocked != "":
		c = codedCheck("browser_ext", "warn", "check.browser_ext_app_location", nil)
		c.HintInfo = &model.ErrorInfo{Code: "check.hint_browser_ext_move_app"}
		c.DetailInfo.Detail = "launch.json not written: the app runs from a read-only disk image"
	case v.LastConnection != nil:
		c = codedCheck("browser_ext", "ok", "check.browser_ext_connected", map[string]any{"browsers": strings.Join(installed, ", "),
			"browser": mockBrowserName(v.LastConnection.Browser), "version": v.LastConnection.ExtensionVersion, "at": v.LastConnection.At.Local().Format("2006-01-02 15:04")})
	default:
		c = codedCheck("browser_ext", "ok", "check.browser_ext_registered", map[string]any{"browsers": strings.Join(installed, ", ")})
	}
	if c.Status != "ok" && v.LastRegister != nil && v.LastRegister.Error != "" {
		c.DetailInfo.Detail = v.LastRegister.Error
	}
	return c
}

// web3System puts the rows aria2c and browser_ext into the scenarios ext_*
// (replacing rows of the same name). Called with s.mu held.
func (m *mock) web3System(r *http.Request, sys *api.System) {
	sc := scenario(r)
	if !slices.Contains(web3Scenarios, sc) {
		return
	}
	aria := codedCheck("aria2c", "ok", "check.aria2c_verified", map[string]any{"version": mockAria2cVer, "sha256": mockAria2cSHA})
	if sc == "ext_broken" {
		aria = codedCheck("aria2c", "error", "check.aria2c_mismatch", map[string]any{"sha256": mockAria2cBad, "expected": mockAria2cSHA})
		aria.HintInfo = &model.ErrorInfo{Code: "check.hint_aria2c_desktop"}
	}
	var ext api.SystemCheck
	if sc == "ext_nmoff" {
		ext = browserExtRow(nil)
	} else {
		st := m.web3()
		st.mu.Lock()
		ext = browserExtRow(st.nmStatus(sc))
		st.mu.Unlock()
	}
	out := make([]api.SystemCheck, 0, len(sys.Checks)+2)
	placedAria, placedExt := false, false
	for _, c := range sys.Checks {
		if c.Name == "aria2c" || c.Name == "browser_ext" {
			continue // replaced
		}
		provider := strings.HasPrefix(c.Name, "api_key:") || strings.HasPrefix(c.Name, "api:") || strings.HasPrefix(c.Name, "role_model:") || c.Name == "disk"
		if provider && !placedExt {
			if !placedAria {
				out = append(out, aria)
				placedAria = true
			}
			out = append(out, ext)
			placedExt = true
		}
		out = append(out, c)
		if c.Name == "ffmpeg" && !placedAria {
			out = append(out, aria)
			placedAria = true
		}
	}
	if !placedAria {
		out = append(out, aria)
	}
	if !placedExt {
		out = append(out, ext)
	}
	sys.Checks = out
}
