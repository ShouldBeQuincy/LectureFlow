// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// SPEC-v3 V1.1, V1.2, V2 (.claude/v3-brief.md section 2).

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// newDesktopEnv is an env in desktop mode; the Host check uses the port of
// the test server.
func newDesktopEnv(t *testing.T, d *DesktopOptions) *env {
	t.Helper()
	e := newEnvWith(t, envConfig{server: func(s *Server) {
		s.desktop = d
		s.version = "3.0.0-test"
	}})
	u, err := url.Parse(e.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if d.Port, err = strconv.Atoi(u.Port()); err != nil {
		t.Fatal(err)
	}
	return e
}

// doWith sends a request with optional token and Host override.
func (e *env) doWith(method, path string, body []byte, token, host string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	if host != "" {
		req.Host = host
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func errCode(t *testing.T, b []byte) string {
	t.Helper()
	var eb struct {
		Error model.ErrorInfo `json:"error"`
	}
	if err := json.Unmarshal(b, &eb); err != nil {
		t.Fatalf("error body %s: %v", b, err)
	}
	return eb.Error.Code
}

func TestDesktopTokenRequired(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	for _, p := range []string{"/api/health", "/api/courses", "/api/app", "/api/nope", "/api/events"} {
		res, b := e.doWith("GET", p, nil, "", "")
		if res.StatusCode != http.StatusUnauthorized || errCode(t, b) != "auth.token_required" {
			t.Errorf("%s without token: %d %s", p, res.StatusCode, b)
		}
		res, b = e.doWith("GET", p, nil, testToken[:63]+"0", "")
		if res.StatusCode != http.StatusUnauthorized || errCode(t, b) != "auth.token_required" {
			t.Errorf("%s with a wrong token: %d %s", p, res.StatusCode, b)
		}
		res, b = e.doWith("GET", p, nil, testToken+"0", "")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a longer token: %d %s", p, res.StatusCode, b)
		}
	}
	// a POST is refused before its handler runs
	res, b := e.doWith("POST", "/api/courses", []byte(`{"name":"X"}`), "", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST without token: %d %s", res.StatusCode, b)
	}
	res, b = e.doWith("GET", "/api/courses", nil, testToken, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/courses with token: %d %s", res.StatusCode, b)
	}
	if len(b) == 0 || b[0] != '[' {
		t.Fatalf("courses body %s", b)
	}
	// a missing route with the token is the usual 404
	res, b = e.doWith("GET", "/api/nope", nil, testToken, "")
	if res.StatusCode != http.StatusNotFound || errCode(t, b) != "api.not_found" {
		t.Fatalf("/api/nope with token: %d %s", res.StatusCode, b)
	}
}

func TestDesktopEmptyTokenAcceptsNothing(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{})
	res, b := e.doWith("GET", "/api/health", nil, "", "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
}

func TestDesktopHostCheck(t *testing.T) {
	d := &DesktopOptions{Token: testToken}
	e := newDesktopEnv(t, d)
	port := strconv.Itoa(d.Port)
	for _, h := range []string{"127.0.0.1:" + port, "localhost:" + port, "LocalHost:" + port} {
		res, b := e.doWith("GET", "/api/health", nil, testToken, h)
		if res.StatusCode != http.StatusOK {
			t.Errorf("host %s: %d %s", h, res.StatusCode, b)
		}
	}
	for _, h := range []string{"evil.example:" + port, "127.0.0.1", "127.0.0.1:1", "localhost", "[::1]:" + port, "127.0.0.1.evil.example:" + port} {
		for _, p := range []string{"/api/health", "/"} {
			res, b := e.doWith("GET", p, nil, testToken, h)
			if res.StatusCode != http.StatusForbidden || errCode(t, b) != "auth.bad_host" {
				t.Errorf("host %s %s: %d %s", h, p, res.StatusCode, b)
			}
		}
	}
}

func TestDesktopStaticNeedsNoToken(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	for _, p := range []string{"/", "/index.html", "/boot.js", "/app.js", "/style.css", "/favicon.svg", "/i18n/zh-CN.json"} {
		res, b := e.doWith("GET", p, nil, "", "")
		if res.StatusCode != http.StatusOK || len(b) == 0 {
			t.Errorf("%s: %d", p, res.StatusCode)
		}
	}
}

func TestWebModeNeedsNoToken(t *testing.T) {
	e := newEnv(t)
	var h Health
	e.call("GET", "/api/health", nil, http.StatusOK, &h)
	if !h.OK || h.Mode != "web" || h.Version != "dev" {
		t.Fatalf("%+v", h)
	}
	res, b := e.doWith("GET", "/api/courses", nil, "", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
}

// Web mode checks the Host header too (DNS rebinding; security review L6):
// 127.0.0.1 or localhost with the port the request came in on.
func TestWebModeHostCheck(t *testing.T) {
	e := newEnv(t)
	u, _ := url.Parse(e.ts.URL)
	port := u.Port()
	for _, h := range []string{"127.0.0.1:" + port, "localhost:" + port, "LOCALHOST:" + port} {
		if res, b := e.doWith("GET", "/api/courses", nil, "", h); res.StatusCode != http.StatusOK {
			t.Errorf("host %s: %d %s", h, res.StatusCode, b)
		}
	}
	for _, h := range []string{"lectureflow.test:80", "evil.example:" + port, "127.0.0.1", "127.0.0.1:1", "localhost",
		"[::1]:" + port, "127.0.0.1.evil.example:" + port, "localhost.evil.example:" + port} {
		for _, p := range []string{"/api/courses", "/"} {
			res, b := e.doWith("GET", p, nil, "", h)
			if res.StatusCode != http.StatusForbidden || errCode(t, b) != "auth.bad_host" {
				t.Errorf("host %s %s: %d %s", h, p, res.StatusCode, b)
			}
		}
	}
}

// doHeaders sends a request with the given headers (no token).
func (e *env) doHeaders(method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// Security review L6: every request that changes something is refused when
// another site's page sent it, in both modes; reading is not.
func TestCrossSiteWritesRefused(t *testing.T) {
	e := newEnv(t)
	own := e.ts.URL
	refused := []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"}, // another port of localhost
		{"Sec-Fetch-Site": "cross-site", "Origin": own},
		{"Origin": "http://evil.example"}, // a browser without Fetch Metadata
		{"Origin": "null"},
		{"Origin": strings.Replace(own, "127.0.0.1", "localhost", 1)},
	}
	allowed := []map[string]string{
		{"Sec-Fetch-Site": "same-origin"},
		{"Sec-Fetch-Site": "none"},
		{"Sec-Fetch-Site": "same-origin", "Origin": own},
		{"Origin": own},
		{}, // no browser (curl, the desktop shell's main process)
	}
	body := []byte(`{"prevent_sleep":true}`)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, h := range refused {
			res, b := e.doHeaders(m, "/api/app/settings", body, h)
			if res.StatusCode != http.StatusForbidden || errCode(t, b) != "app.cross_site" {
				t.Errorf("%s %v: %d %s", m, h, res.StatusCode, b)
			}
		}
	}
	for _, h := range allowed {
		if res, b := e.doHeaders("PATCH", "/api/app/settings", body, h); res.StatusCode != http.StatusOK {
			t.Errorf("PATCH %v: %d %s", h, res.StatusCode, b)
		}
	}
	for _, h := range refused {
		// AUD1 L-3: a read the browser marks as another site's is refused too
		// (some reads change something); a browser without Fetch Metadata
		// may still read (its Origin is checked for writes only)
		want := http.StatusOK
		if h["Sec-Fetch-Site"] != "" {
			want = http.StatusForbidden
		}
		if res, b := e.doHeaders("GET", "/api/app/settings", nil, h); res.StatusCode != want {
			t.Errorf("GET %v: %d %s, want %d", h, res.StatusCode, b, want)
		}
	}
	// desktop mode: the same Fetch Metadata check besides the token
	desk := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	for h, want := range map[string]int{"cross-site": http.StatusForbidden, "same-site": http.StatusForbidden,
		"same-origin": http.StatusOK, "": http.StatusOK} {
		hd := map[string]string{TokenHeader: testToken, "Origin": "http://evil.example"}
		if h != "" {
			hd["Sec-Fetch-Site"] = h
		}
		if res, b := desk.doHeaders("PATCH", "/api/app/settings", body, hd); res.StatusCode != want {
			t.Errorf("desktop %q: %d %s", h, res.StatusCode, b)
		}
	}
}

func TestSecurityHeadersBothModes(t *testing.T) {
	web := newEnv(t)
	desk := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
	for _, c := range []struct {
		e   *env
		tok string
	}{{web, ""}, {desk, testToken}, {desk, ""}} {
		for _, p := range []string{"/", "/boot.js", "/api/health", "/api/nope"} {
			res, _ := c.e.doWith("GET", p, nil, c.tok, "")
			if got := res.Header.Get("Content-Security-Policy"); got != csp {
				t.Errorf("%s (token %v): CSP %q", p, c.tok != "", got)
			}
			if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("%s: nosniff %q", p, got)
			}
		}
	}
}

// The page has no inline script left (CSP script-src 'self'): index.html
// loads boot.js synchronously before the module.
func TestIndexHasNoInlineScript(t *testing.T) {
	e := newEnv(t)
	_, b := e.doWith("GET", "/", nil, "", "")
	html := string(b)
	if strings.Contains(html, "<script>") || strings.Contains(html, "<script type=\"module\">") {
		t.Fatal("index.html has an inline script")
	}
	boot := strings.Index(html, `<script src="boot.js"></script>`)
	app := strings.Index(html, `<script type="module" src="app.js"></script>`)
	if boot < 0 || app < 0 || boot > app || boot > strings.Index(html, "</head>") {
		t.Fatalf("boot.js must be a classic script in <head> before app.js: %d %d", boot, app)
	}
	res, js := e.doWith("GET", "/boot.js", nil, "", "")
	if res.Header.Get("Content-Type") != "text/javascript; charset=utf-8" || !strings.Contains(string(js), "dataset.nav") {
		t.Fatalf("boot.js: %s", res.Header.Get("Content-Type"))
	}
}

func TestHealthAndAppDesktop(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken, ResourcesDir: `C:\res`})
	res, b := e.doWith("GET", "/api/health", nil, testToken, "")
	var h Health
	if res.StatusCode != http.StatusOK || json.Unmarshal(b, &h) != nil || !h.OK || h.Mode != "desktop" || h.Version != "3.0.0-test" {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	res, b = e.doWith("GET", "/api/app", nil, testToken, "")
	var a AppInfo
	if res.StatusCode != http.StatusOK || json.Unmarshal(b, &a) != nil {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	if a.Mode != "desktop" || a.Version != "3.0.0-test" || a.Platform != runtime.GOOS || a.Arch != runtime.GOARCH ||
		a.ResourcesDir != `C:\res` || a.OnboardingDone || !filepath.IsAbs(a.DataDir) {
		t.Fatalf("%+v", a)
	}
}

func TestAppWebMode(t *testing.T) {
	e := newEnv(t)
	var a AppInfo
	e.call("GET", "/api/app", nil, http.StatusOK, &a)
	if a.Mode != "web" || a.ResourcesDir != "" || a.OnboardingDone || a.DataDir == "" {
		t.Fatalf("%+v", a)
	}
	e.call("PUT", "/api/onboarding", map[string]any{"step": "done", "done": true}, http.StatusOK, nil)
	e.call("GET", "/api/app", nil, http.StatusOK, &a)
	if !a.OnboardingDone {
		t.Fatal("onboarding_done must follow onboarding.state")
	}
}

func TestAppSettings(t *testing.T) {
	e := newEnv(t)
	var s AppSettings
	e.call("GET", "/api/app/settings", nil, http.StatusOK, &s)
	if s != (AppSettings{CloseBehavior: "ask", PreventSleep: true, UpdateChannel: "stable", DownloadConcurrency: 3, DownloadConnections: 4}) {
		t.Fatalf("defaults %+v", s)
	}
	e.call("PATCH", "/api/app/settings", map[string]any{"close_behavior": "background"}, http.StatusOK, &s)
	if s != (AppSettings{CloseBehavior: "background", PreventSleep: true, UpdateChannel: "stable", DownloadConcurrency: 3, DownloadConnections: 4}) {
		t.Fatalf("%+v", s)
	}
	e.call("PATCH", "/api/app/settings", map[string]any{"prevent_sleep": false, "update_channel": "beta", "close_behavior": "quit"}, http.StatusOK, &s)
	want := AppSettings{CloseBehavior: "quit", PreventSleep: false, UpdateChannel: "beta", DownloadConcurrency: 3, DownloadConnections: 4}
	if s != want {
		t.Fatalf("%+v", s)
	}
	ctx := context.Background()
	for k, v := range map[string]string{kvAppCloseBehavior: "quit", kvAppPreventSleep: "0", kvAppUpdateChannel: "beta"} {
		got, ok, err := e.st.GetKV(ctx, k)
		if err != nil || !ok || got != v {
			t.Errorf("kv %s = %q %v %v", k, got, ok, err)
		}
	}
	e.call("GET", "/api/app/settings", nil, http.StatusOK, &s)
	if s != want {
		t.Fatalf("%+v", s)
	}
	// invalid values: 400, nothing written (also not the valid field of the same patch)
	for _, c := range []struct {
		body any
		code string
	}{
		{map[string]any{"close_behavior": "minimize", "update_channel": "stable"}, "app_settings.close_behavior_invalid"},
		{map[string]any{"close_behavior": ""}, "app_settings.close_behavior_invalid"},
		{map[string]any{"update_channel": "nightly", "prevent_sleep": true}, "app_settings.update_channel_invalid"},
		{map[string]any{"prevent_sleep": "yes"}, "request.bad_json"},
		{map[string]any{"close_behavior": 1}, "request.bad_json"},
		{map[string]any{}, "request.nothing_to_change"},
	} {
		if got := e.callErrInfo("PATCH", "/api/app/settings", c.body, http.StatusBadRequest).Code; got != c.code {
			t.Errorf("%v: %s, want %s", c.body, got, c.code)
		}
	}
	e.call("GET", "/api/app/settings", nil, http.StatusOK, &s)
	if s != want {
		t.Fatalf("after invalid patches %+v", s)
	}
	// unknown stored values read as the defaults
	_ = e.st.SetKV(ctx, kvAppCloseBehavior, "weird")
	_ = e.st.SetKV(ctx, kvAppPreventSleep, "maybe")
	_ = e.st.SetKV(ctx, kvAppUpdateChannel, "")
	e.call("GET", "/api/app/settings", nil, http.StatusOK, &s)
	if s != (AppSettings{CloseBehavior: "ask", PreventSleep: true, UpdateChannel: "stable", DownloadConcurrency: 3, DownloadConnections: 4}) {
		t.Fatalf("%+v", s)
	}
}

func TestShutdownDesktopOnly(t *testing.T) {
	web := newEnv(t)
	if got := web.callErrInfo("POST", "/api/app/shutdown", nil, http.StatusNotFound).Code; got != "api.not_found" {
		t.Fatalf("web mode: %s", got)
	}
	var calls atomic.Int32
	d := &DesktopOptions{Token: testToken, Shutdown: func() { calls.Add(1) }}
	e := newDesktopEnv(t, d)
	res, b := e.doWith("POST", "/api/app/shutdown", nil, "", "")
	if res.StatusCode != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("without token: %d %s, %d calls", res.StatusCode, b, calls.Load())
	}
	res, b = e.doWith("GET", "/api/app/shutdown", nil, testToken, "")
	if res.StatusCode == http.StatusAccepted || calls.Load() != 0 {
		t.Fatalf("GET: %d %s", res.StatusCode, b)
	}
	res, b = e.doWith("POST", "/api/app/shutdown", nil, testToken, "")
	if res.StatusCode != http.StatusAccepted || calls.Load() != 1 {
		t.Fatalf("with token: %d %s, %d calls", res.StatusCode, b, calls.Load())
	}
}

func TestOnboarding(t *testing.T) {
	e := newEnv(t)
	var o OnboardingState
	e.call("GET", "/api/onboarding", nil, http.StatusOK, &o)
	if o.Step != "welcome" || o.Done || string(o.Data) != "{}" {
		t.Fatalf("initial %+v %s", o, o.Data)
	}
	body := map[string]any{"step": "components", "done": false, "data": map[string]any{"preset": "balanced", "ids": []string{"core", "python"}}}
	e.call("PUT", "/api/onboarding", body, http.StatusOK, &o)
	e.call("GET", "/api/onboarding", nil, http.StatusOK, &o)
	if o.Step != "components" || o.Done || string(o.Data) != `{"ids":["core","python"],"preset":"balanced"}` {
		t.Fatalf("%+v %s", o, o.Data)
	}
	// data may be left out (an empty object)
	e.call("PUT", "/api/onboarding", map[string]any{"step": "finish", "done": true}, http.StatusOK, &o)
	e.call("GET", "/api/onboarding", nil, http.StatusOK, &o)
	if o.Step != "finish" || !o.Done || string(o.Data) != "{}" {
		t.Fatalf("%+v %s", o, o.Data)
	}
	for _, c := range []struct {
		body string
		code string
	}{
		{`{"done":false}`, "onboarding_state.invalid"},
		{`{"step":3,"done":false}`, "onboarding_state.invalid"},
		{`{"step":"","done":false}`, "onboarding_state.invalid"},
		{`{"step":"a"}`, "onboarding_state.invalid"},
		{`{"step":"a","done":"true"}`, "onboarding_state.invalid"},
		{`{"step":"a","done":1}`, "onboarding_state.invalid"},
		{`{"step":"a","done":true,"data":[1]}`, "onboarding_state.invalid"},
		{`{"step":"a","done":true,"data":"x"}`, "onboarding_state.invalid"},
		{`[1,2]`, "request.bad_json"},
		{`null`, "request.bad_json"},
		{`not json`, "request.bad_json"},
		{`{"step":"a","done":true,"data":{"x":"` + strings.Repeat("y", maxOnboardingBytes) + `"}}`, "onboarding_state.too_large"},
	} {
		res, b := e.doWith("PUT", "/api/onboarding", []byte(c.body), "", "")
		if res.StatusCode != http.StatusBadRequest || errCode(t, b) != c.code {
			t.Errorf("%.40s: %d %s, want %s", c.body, res.StatusCode, b, c.code)
		}
	}
	e.call("GET", "/api/onboarding", nil, http.StatusOK, &o)
	if o.Step != "finish" || !o.Done {
		t.Fatalf("invalid PUTs must not change the state: %+v", o)
	}
	// a corrupt stored value reads as the initial state
	_ = e.st.SetKV(context.Background(), kvOnboardingState, "{broken")
	e.call("GET", "/api/onboarding", nil, http.StatusOK, &o)
	if o.Step != "welcome" || o.Done {
		t.Fatalf("%+v", o)
	}
}

// fakeTestSched reports device self-tests for GET /api/app/activity.
type fakeTestSched struct {
	deviceScheduler
	statuses []sched.DeviceStatus
}

func (f *fakeTestSched) DeviceStatuses() []sched.DeviceStatus { return f.statuses }

func TestActivity(t *testing.T) {
	ft := &fakeTestSched{}
	e := newEnvWith(t, envConfig{server: func(s *Server) { s.dsc = ft }})
	get := func() AppActivity {
		var a AppActivity
		e.call("GET", "/api/app/activity", nil, http.StatusOK, &a)
		return a
	}
	if a := get(); a != (AppActivity{}) {
		t.Fatalf("idle: %+v", a)
	}
	c := e.course("C")
	l1 := e.lecture(c.ID, "L1")
	l2 := e.lecture(c.ID, "L2")
	ctx := context.Background()
	count := func(status string) int {
		js, err := e.st.ListCurrentJobsByStatus(ctx, status)
		if err != nil {
			t.Fatal(err)
		}
		return len(js)
	}
	// the scheduler is not started: queue the prep jobs by hand
	toQueued := func(lectureID, stage string) {
		if j := e.moveJob(model.ScopeLecture, lectureID, stage); j.Status != model.StatusQueued {
			e.moveJob(model.ScopeLecture, lectureID, stage, model.StatusQueued)
		}
	}
	toQueued(l1.ID, model.StagePrep)
	toQueued(l2.ID, model.StagePrep)
	base := get()
	if base.Running != 0 || base.Queued != count(model.StatusQueued) || base.Queued < 2 || base.Transcribing || base.APIRunning {
		t.Fatalf("queued: %+v (queued %d)", base, count(model.StatusQueued))
	}
	// prep of L1 runs: one running, one queued fewer, neither asr nor API
	e.moveJob(model.ScopeLecture, l1.ID, model.StagePrep, model.StatusRunning)
	if a := get(); a.Running != 1 || a.Queued != base.Queued-1 || a.Transcribing || a.APIRunning {
		t.Fatalf("prep running: %+v", a)
	}
	// L1 asr runs → transcribing
	e.moveJob(model.ScopeLecture, l1.ID, model.StagePrep, model.StatusDone)
	toQueued(l1.ID, model.StageASR)
	e.moveJob(model.ScopeLecture, l1.ID, model.StageASR, model.StatusRunning)
	if a := get(); a.Running != 1 || !a.Transcribing || a.APIRunning {
		t.Fatalf("asr running: %+v", a)
	}
	// L1 summary (an API stage) runs, L2 prep as well → api_running, 2 running
	e.moveJob(model.ScopeLecture, l1.ID, model.StageASR, model.StatusDone)
	toQueued(l1.ID, model.StageSummary)
	e.moveJob(model.ScopeLecture, l1.ID, model.StageSummary, model.StatusRunning)
	e.moveJob(model.ScopeLecture, l2.ID, model.StagePrep, model.StatusRunning)
	if a := get(); a.Running != 2 || a.Transcribing || !a.APIRunning || a.Running != count(model.StatusRunning) || a.Queued != count(model.StatusQueued) {
		t.Fatalf("summary running: %+v", a)
	}
	// a device self-test running counts as transcribing; a waiting one does not
	ft.statuses = []sched.DeviceStatus{{ID: "cuda:0", SelfTest: &sched.DeviceTestState{State: sched.DeviceTestWaiting}}}
	if a := get(); a.Transcribing {
		t.Fatalf("waiting self-test: %+v", a)
	}
	ft.statuses = append(ft.statuses, sched.DeviceStatus{ID: "ov:GPU", SelfTest: &sched.DeviceTestState{State: sched.DeviceTestRunning}})
	if a := get(); !a.Transcribing || !a.APIRunning {
		t.Fatalf("running self-test: %+v", a)
	}
}
