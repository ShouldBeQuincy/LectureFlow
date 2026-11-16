// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// Desktop mode and the application endpoints of SPEC-v3 V1.1, V1.2, V2
// (.claude/v3-brief.md section 2): the access token and Host check of the
// desktop build, the security headers of both modes, and /api/health,
// /api/app, /api/app/settings, /api/app/activity, /api/app/shutdown,
// /api/onboarding.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"lectureflow/internal/fetch"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// DesktopOptions are set by cmd in desktop mode (SPEC-v3 V1.2); nil = web mode.
type DesktopOptions struct {
	Token        string // every /api/ request must carry it in X-LF-Token
	Port         int    // Host header check: 127.0.0.1:Port or localhost:Port
	ResourcesDir string
	Shutdown     func() // POST /api/app/shutdown: start the graceful stop (returns at once)
	Restart      func() // graceful stop, then exit code 3 (step 5: data dir change / import)
	// LocationFile is {default data dir}/location.json ({"data_dir": "..."},
	// v3-brief §3): written when the user moves the data dir (V1.3); ""
	// when the data dir was given by LECTUREFLOW_DATA_DIR (no move then).
	LocationFile string
	// InstallDir is the installation directory the desktop shell names
	// (-install-dir: the Windows installation set up by the installer; ""
	// = none: development, the portable build, macOS). The diagnostics'
	// install_dir row says whether other accounts can change it
	// (system_installdir.go).
	InstallDir string
}

// TokenHeader is the request header carrying the desktop access token.
const TokenHeader = "X-LF-Token"

// contentSecurityPolicy is sent on every response in both modes (SPEC-v3 V2).
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// kv keys of the application settings (V1.1, V11) and the onboarding state (V4).
const (
	kvAppCloseBehavior = model.KVAppCloseBehavior
	kvAppPreventSleep  = model.KVAppPreventSleep
	kvAppUpdateChannel = model.KVAppUpdateChannel
	kvOnboardingState  = model.KVOnboardingState
)

// Values of the application settings.
var (
	closeBehaviors = []string{"ask", "background", "quit"}
	updateChannels = []string{"stable", "beta"}
)

const maxOnboardingBytes = 64 << 10

// ---- middleware ----

// secure adds the security headers to every response and checks the Host
// header of every request (DNS rebinding): in desktop mode against the
// configured port, in web mode against the port the request came in on
// (security review L6). In desktop mode it checks the token of every /api/
// request; the static front end needs no token: the window loads it before
// any script runs, and the main process injects the header into the API
// requests (fetch, EventSource, <video>). In both modes an /api/ request
// the browser marks as sent by another site's page is refused whatever
// its method (crossSiteFetch; final security audit AUD1 L-3: web mode has
// no token, and some GET requests change something — the event stream,
// /api/colab, the media request, cite.json), and so is a request that
// changes something (not GET, HEAD or OPTIONS) another site's page sent
// according to its Origin when the browser sends no Fetch Metadata
// (crossOrigin: a form post or a no-cors fetch needs no CORS preflight;
// security review L6). The static front end stays open to a link from
// another site.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", contentSecurityPolicy)
		hd.Set("X-Content-Type-Options", "nosniff")
		if d := s.desktop; d != nil {
			if !d.hostAllowed(r.Host) {
				writeError(w, http.StatusForbidden, "auth.bad_host", nil)
				return
			}
			if (r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")) && !d.tokenOK(r.Header.Get(TokenHeader)) {
				writeError(w, http.StatusUnauthorized, "auth.token_required", nil)
				return
			}
		} else if !webHostAllowed(r) {
			writeError(w, http.StatusForbidden, "auth.bad_host", nil)
			return
		}
		if (r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")) && crossSiteFetch(r) {
			writeError(w, http.StatusForbidden, "app.cross_site", nil)
			return
		}
		if !safeMethod(r.Method) && s.crossOrigin(r) {
			writeError(w, http.StatusForbidden, "app.cross_site", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed: 127.0.0.1:Port or localhost:Port (DNS rebinding, V1.2).
func (d *DesktopOptions) hostAllowed(host string) bool { return loopbackHost(host, d.Port) }

// loopbackHost: host is 127.0.0.1:port or localhost:port.
func loopbackHost(host string, port int) bool {
	p := ":" + strconv.Itoa(port)
	h := strings.ToLower(host)
	return h == "127.0.0.1"+p || h == "localhost"+p
}

// webHostAllowed is web mode's Host check: 127.0.0.1 or localhost with the
// port of the connection (the server listens on 127.0.0.1 only, config
// "listen"; a page of a site whose name was made to resolve to 127.0.0.1
// sends its own name). A request that came through no listener (a handler
// called directly) has no port to compare and is not checked.
func webHostAllowed(r *http.Request) bool {
	la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return true
	}
	ta, ok := la.(*net.TCPAddr)
	if !ok {
		return true // (not TCP: a test's pipe)
	}
	return loopbackHost(r.Host, ta.Port)
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// crossSiteFetch reports a request the browser's Fetch Metadata marks as
// sent by another site's page: Sec-Fetch-Site cross-site, same-site
// (another port of localhost) or a value this server does not know.
// LectureFlow's own pages send same-origin (fetch, EventSource, <video>
// and <audio>, a download link), an address typed or opened by the
// desktop shell or a bookmark sends none; a client that is no browser
// (the desktop shell's main process, curl) and an older browser send no
// such header, and the Origin and Host checks apply to them as before.
func crossSiteFetch(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return false
	}
	return true
}

// crossOrigin reports a request another page than LectureFlow's own sent.
// Fetch Metadata first: anything but same-origin, or none (typed by the
// user, a bookmark), is refused, same-site too (another port of
// localhost); a client that is no browser sends none of it. Without it (an
// older browser), the Origin header, which browsers send with every
// request that changes something, must name this server; desktop mode
// needs no fallback, its token stays inside LectureFlow's own pages.
func (s *Server) crossOrigin(r *http.Request) bool {
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "same-origin", "none":
		return false
	case "":
	default: // cross-site, same-site
		return true
	}
	if s.desktop != nil {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err != nil || !strings.EqualFold(u.Host, r.Host) || (u.Scheme != "http" && u.Scheme != "https")
}

// tokenOK compares in constant time; an empty configured token accepts nothing.
func (d *DesktopOptions) tokenOK(got string) bool {
	if d.Token == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(d.Token)) == 1
}

func (s *Server) mode() string {
	if s.desktop != nil {
		return "desktop"
	}
	return "web"
}

func (s *Server) appVersion() string {
	if s.version == "" {
		return "dev"
	}
	return s.version
}

// ---- GET /api/health, GET /api/app ----

// Health is GET /api/health.
type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	Mode    string `json:"mode"` // desktop | web
}

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Health{OK: true, Version: s.appVersion(), Mode: s.mode()})
}

// AppInfo is GET /api/app.
type AppInfo struct {
	Version        string `json:"version"`
	Mode           string `json:"mode"`     // desktop | web
	Platform       string `json:"platform"` // windows | darwin | linux (runtime.GOOS)
	Arch           string `json:"arch"`     // runtime.GOARCH
	DataDir        string `json:"data_dir"`
	ResourcesDir   string `json:"resources_dir"` // "" in web mode
	OnboardingDone bool   `json:"onboarding_done"`
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	ob, err := s.onboarding(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	dir := s.dataDir
	if a, err := filepath.Abs(dir); err == nil && dir != "" {
		dir = a
	}
	info := AppInfo{Version: s.appVersion(), Mode: s.mode(), Platform: runtime.GOOS, Arch: runtime.GOARCH,
		DataDir: dir, OnboardingDone: ob.Done}
	if s.desktop != nil {
		info.ResourcesDir = s.desktop.ResourcesDir
	}
	writeJSON(w, http.StatusOK, info)
}

// ---- /api/app/settings ----

// AppSettings is GET/PATCH /api/app/settings (V1.1, V11; the download
// queue's two numbers of SPEC-ext, both modes).
type AppSettings struct {
	CloseBehavior string `json:"close_behavior"` // ask | background | quit
	PreventSleep  bool   `json:"prevent_sleep"`
	UpdateChannel string `json:"update_channel"` // stable | beta
	// DownloadConcurrency is how many lectures download at once (1–6,
	// default 3); DownloadConnections the connections per file (1–16,
	// default 4; a platform's own limit may lower it).
	DownloadConcurrency int `json:"download_concurrency"`
	DownloadConnections int `json:"download_connections"`
}

// AppSettingsPatch is the body of PATCH /api/app/settings (omitted = unchanged).
type AppSettingsPatch struct {
	CloseBehavior       *string `json:"close_behavior"`
	PreventSleep        *bool   `json:"prevent_sleep"`
	UpdateChannel       *string `json:"update_channel"`
	DownloadConcurrency *int    `json:"download_concurrency"`
	DownloadConnections *int    `json:"download_connections"`
}

func oneOf(v string, list []string) bool {
	for _, x := range list {
		if v == x {
			return true
		}
	}
	return false
}

// kvGet returns the value of a kv key ("" and false when missing).
func (s *Server) kvGet(ctx context.Context, key string) (string, bool, error) {
	v, ok, err := s.st.GetKV(ctx, key)
	if err != nil {
		return "", false, err
	}
	return v, ok, nil
}

// appSettings reads the settings; a missing or unknown stored value is the default.
func (s *Server) appSettings(ctx context.Context) (AppSettings, error) {
	out := AppSettings{CloseBehavior: "ask", PreventSleep: true, UpdateChannel: "stable"}
	v, ok, err := s.kvGet(ctx, kvAppCloseBehavior)
	if err != nil {
		return out, err
	}
	if ok && oneOf(v, closeBehaviors) {
		out.CloseBehavior = v
	}
	if v, ok, err = s.kvGet(ctx, kvAppPreventSleep); err != nil {
		return out, err
	}
	if ok && v == "0" {
		out.PreventSleep = false
	}
	if v, ok, err = s.kvGet(ctx, kvAppUpdateChannel); err != nil {
		return out, err
	}
	if ok && oneOf(v, updateChannels) {
		out.UpdateChannel = v
	}
	if out.DownloadConcurrency, out.DownloadConnections, err = fetch.ReadLimits(ctx, s.st); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Server) getAppSettings(w http.ResponseWriter, r *http.Request) {
	out, err := s.appSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// PATCH /api/app/settings: every given field is validated before any is
// written; 400 app_settings.close_behavior_invalid / update_channel_invalid
// {value}, request.nothing_to_change for an empty patch.
func (s *Server) patchAppSettings(w http.ResponseWriter, r *http.Request) {
	var in AppSettingsPatch
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.CloseBehavior == nil && in.PreventSleep == nil && in.UpdateChannel == nil &&
		in.DownloadConcurrency == nil && in.DownloadConnections == nil {
		writeError(w, http.StatusBadRequest, "request.nothing_to_change",
			map[string]any{"fields": "close_behavior, prevent_sleep, update_channel, download_concurrency, download_connections"})
		return
	}
	if in.CloseBehavior != nil && !oneOf(*in.CloseBehavior, closeBehaviors) {
		writeError(w, http.StatusBadRequest, "app_settings.close_behavior_invalid", map[string]any{"value": *in.CloseBehavior})
		return
	}
	if in.UpdateChannel != nil && !oneOf(*in.UpdateChannel, updateChannels) {
		writeError(w, http.StatusBadRequest, "app_settings.update_channel_invalid", map[string]any{"value": *in.UpdateChannel})
		return
	}
	if v := in.DownloadConcurrency; v != nil && (*v < fetch.MinConcurrency || *v > fetch.MaxConcurrency) {
		writeError(w, http.StatusBadRequest, fetch.CodeConcurrencyInvalid,
			map[string]any{"value": *v, "min": fetch.MinConcurrency, "max": fetch.MaxConcurrency})
		return
	}
	if v := in.DownloadConnections; v != nil && (*v < fetch.MinConnections || *v > fetch.MaxConnections) {
		writeError(w, http.StatusBadRequest, fetch.CodeConnectionsInvalid,
			map[string]any{"value": *v, "min": fetch.MinConnections, "max": fetch.MaxConnections})
		return
	}
	ctx := r.Context()
	var writes [][2]string
	if in.CloseBehavior != nil {
		writes = append(writes, [2]string{kvAppCloseBehavior, *in.CloseBehavior})
	}
	if in.PreventSleep != nil {
		v := "0"
		if *in.PreventSleep {
			v = "1"
		}
		writes = append(writes, [2]string{kvAppPreventSleep, v})
	}
	if in.UpdateChannel != nil {
		writes = append(writes, [2]string{kvAppUpdateChannel, *in.UpdateChannel})
	}
	if in.DownloadConcurrency != nil {
		writes = append(writes, [2]string{fetch.KVConcurrency, strconv.Itoa(*in.DownloadConcurrency)})
	}
	if in.DownloadConnections != nil {
		writes = append(writes, [2]string{fetch.KVConnections, strconv.Itoa(*in.DownloadConnections)})
	}
	for _, kv := range writes {
		if err := s.st.SetKV(ctx, kv[0], kv[1]); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if s.dl != nil && (in.DownloadConcurrency != nil || in.DownloadConnections != nil) {
		s.dl.SettingsChanged() // a higher limit starts waiting downloads now
	}
	s.hub.Changed("settings", "")
	s.getAppSettings(w, r)
}

// ---- GET /api/app/activity ----

// AppActivity is GET /api/app/activity, polled by the main process every 5
// seconds (the close-window question and the sleep blocker, V1.1). The
// browser extension's downloads count as activity too (SPEC-ext): one in
// progress is part of running, a waiting one of queued, so the shell's
// close question and sleep blocker cover them without a change of its own.
type AppActivity struct {
	Running      int  `json:"running"`      // current jobs in status running + downloads in progress
	Queued       int  `json:"queued"`       // current jobs in status queued + waiting downloads
	Transcribing bool `json:"transcribing"` // an asr job or a device self-test runs
	APIRunning   bool `json:"api_running"`  // a job calling a model (an API stage) runs
	Downloads    int  `json:"downloads"`    // downloads in progress or waiting (included above)
}

func (s *Server) getActivity(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.st.ListCurrentJobsByStatus(r.Context(), model.StatusRunning, model.StatusQueued)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var out AppActivity
	for _, j := range jobs {
		if j.Status == model.StatusQueued {
			out.Queued++
			continue
		}
		out.Running++
		if j.Stage == model.StageASR {
			out.Transcribing = true
		}
		if balanceStages[j.Stage] {
			out.APIRunning = true
		}
	}
	if s.dl != nil {
		a := s.dl.Activity()
		out.Running += a.Active
		out.Queued += a.Queued
		out.Downloads = a.Active + a.Queued
	}
	if s.dsc != nil && !out.Transcribing {
		for _, d := range s.dsc.DeviceStatuses() {
			if d.SelfTest != nil && d.SelfTest.State == sched.DeviceTestRunning {
				out.Transcribing = true
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- POST /api/app/shutdown ----

// POST /api/app/shutdown: desktop mode only (404 api.not_found in web
// mode); starts the graceful stop and answers 202 at once.
func (s *Server) postShutdown(w http.ResponseWriter, r *http.Request) {
	if s.desktop == nil || s.desktop.Shutdown == nil {
		writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.desktop.Shutdown()
}

// ---- /api/onboarding ----

// OnboardingState is GET/PUT /api/onboarding (V4; the steps are step 6's).
// Stored as sent in kv onboarding.state; data is free-form for the wizard.
type OnboardingState struct {
	Step string          `json:"step"`
	Done bool            `json:"done"`
	Data json.RawMessage `json:"data"`
}

func defaultOnboarding() OnboardingState {
	return OnboardingState{Step: "welcome", Data: json.RawMessage("{}")}
}

// parseOnboarding validates a state: a JSON object whose step is a
// non-empty string, done a boolean and data (optional) an object.
func parseOnboarding(b []byte) (OnboardingState, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return OnboardingState{}, apiErr(http.StatusBadRequest, "request.bad_json", nil)
	}
	bad := func(field string) error {
		return apiErr(http.StatusBadRequest, "onboarding_state.invalid", map[string]any{"field": field})
	}
	out := defaultOnboarding()
	if err := json.Unmarshal(raw["step"], &out.Step); err != nil || raw["step"] == nil || strings.TrimSpace(out.Step) == "" || len(out.Step) > 64 {
		return out, bad("step")
	}
	v, ok := raw["done"]
	if !ok || (string(v) != "true" && string(v) != "false") {
		return out, bad("done")
	}
	out.Done = string(v) == "true"
	if d, ok := raw["data"]; ok && string(d) != "null" {
		if t := bytes.TrimSpace(d); len(t) == 0 || t[0] != '{' {
			return out, bad("data")
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, d); err != nil {
			return out, bad("data")
		}
		out.Data = buf.Bytes()
	}
	return out, nil
}

// onboarding reads the stored state; a missing or unreadable value is the
// initial state (not done).
func (s *Server) onboarding(ctx context.Context) (OnboardingState, error) {
	v, ok, err := s.kvGet(ctx, kvOnboardingState)
	if err != nil {
		return defaultOnboarding(), err
	}
	if !ok {
		return defaultOnboarding(), nil
	}
	st, err := parseOnboarding([]byte(v))
	if err != nil {
		return defaultOnboarding(), nil
	}
	return st, nil
}

func (s *Server) getOnboarding(w http.ResponseWriter, r *http.Request) {
	st, err := s.onboarding(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// PUT /api/onboarding OnboardingState: replaces the state; 400
// onboarding_state.invalid {field} (step, done, data), onboarding_state.too_large.
func (s *Server) putOnboarding(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxOnboardingBytes+1))
	if err != nil {
		s.fail(w, r, apiErr(http.StatusBadRequest, "request.bad_json", nil))
		return
	}
	if len(b) > maxOnboardingBytes {
		writeError(w, http.StatusBadRequest, "onboarding_state.too_large", map[string]any{"max_bytes": maxOnboardingBytes})
		return
	}
	st, err := parseOnboarding(b)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	enc, err := json.Marshal(st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.st.SetKV(r.Context(), kvOnboardingState, string(enc)); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
