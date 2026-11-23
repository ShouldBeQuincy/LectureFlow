// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testWebAccessKey = "abcdefgh234567abcdefgh234567abcdefgh234567abcdefgh23"

type waTestEnv struct {
	t      *testing.T
	env    *env
	wa     *httptest.Server
	waPort int
	host   string
	key    string
	nowMu  sync.Mutex
	now    time.Time
}

func newWebAccessEnv(t *testing.T) *waTestEnv {
	t.Helper()
	d := &DesktopOptions{Token: testToken, ResourcesDir: "/fake/resources"}
	e := newDesktopEnv(t, d)

	w := &waTestEnv{
		t:    t,
		env:  e,
		host: "mymac.example-tailnet.ts.net:8443",
		key:  testWebAccessKey,
		now:  time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC),
	}
	opts := WebAccessOptions{
		Key:       w.key,
		Hosts:     []string{w.host},
		Now:       func() time.Time { w.nowMu.Lock(); defer w.nowMu.Unlock(); return w.now },
		FailDelay: -1, // disable 500ms sleep in unit tests unless explicitly tested
	}
	// Wrap in a lazy handler so opts.Port can be filled from ts.Listener.Addr().
	var h http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(rw, r)
	}))
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	w.waPort, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	opts.Port = w.waPort
	h = checkErrorBodies(t, e.srv.WebAccessHandler(opts))
	w.wa = ts
	return w
}

func (w *waTestEnv) advance(d time.Duration) {
	w.nowMu.Lock()
	w.now = w.now.Add(d)
	w.nowMu.Unlock()
}

func (w *waTestEnv) do(method, path string, body io.Reader, headers map[string]string, cookies ...*http.Cookie) (*http.Response, []byte) {
	w.t.Helper()
	req, err := http.NewRequest(method, w.wa.URL+path, body)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Host = w.host
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	res, err := client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func (w *waTestEnv) login(key string) (*http.Response, []byte, *http.Cookie) {
	w.t.Helper()
	form := url.Values{"key": {key}}.Encode()
	res, b := w.do(http.MethodPost, "/web-access/login", strings.NewReader(form), map[string]string{
		"Content-Type":   "application/x-www-form-urlencoded",
		"Sec-Fetch-Site": "same-origin",
	})
	var session *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == webAccessCookieName {
			session = c
		}
	}
	return res, b, session
}

func TestLoadOrCreateWebAccessKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	k1, p1, created1, err := LoadOrCreateWebAccessKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created1 {
		t.Fatal("expected created=true on first call")
	}
	if p1 != filepath.Join(dir, WebAccessKeyFileName) {
		t.Fatalf("unexpected path: %s", p1)
	}
	if len(k1) != 52 { // 32 bytes base32 no padding = 52 chars
		t.Fatalf("unexpected key length %d (%q)", len(k1), k1)
	}
	if k1 != strings.ToLower(k1) || strings.Contains(k1, "=") {
		t.Fatalf("key must be lowercase base32 without padding: %q", k1)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p1)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %#o, want 0600", fi.Mode().Perm())
		}
	}

	// Second call reuses the exact same key.
	k2, p2, created2, err := LoadOrCreateWebAccessKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if created2 || k2 != k1 || p2 != p1 {
		t.Fatalf("expected reuse: created=%v k2=%q p2=%q", created2, k2, p2)
	}

	// Deleting the file rotates the key on next call.
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	k3, _, created3, err := LoadOrCreateWebAccessKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created3 || k3 == k1 {
		t.Fatalf("expected rotated key: created=%v same=%v", created3, k3 == k1)
	}
}

func TestWebAccessHostCheck(t *testing.T) {
	w := newWebAccessEnv(t)
	portStr := strconv.Itoa(w.waPort)
	allowed := []string{
		w.host,
		strings.ToUpper(w.host),
		"127.0.0.1:" + portStr,
		"localhost:" + portStr,
		"LOCALHOST:" + portStr,
	}
	for _, h := range allowed {
		res, b := w.do(http.MethodGet, "/web-access/login", nil, map[string]string{"Host": h})
		if res.StatusCode != http.StatusOK {
			t.Errorf("allowed host %q: got %d %s", h, res.StatusCode, b)
		}
		if res.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("missing Referrer-Policy header")
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("missing X-Content-Type-Options header")
		}
		if res.Header.Get("Content-Security-Policy") != contentSecurityPolicy {
			t.Errorf("unexpected CSP header: %q", res.Header.Get("Content-Security-Policy"))
		}
	}

	denied := []string{
		"evil.example:8443",
		"mymac.example-tailnet.ts.net", // wrong port (missing :8443)
		"127.0.0.1:1",
		"127.0.0.1",
		"localhost",
		"mymac.example-tailnet.ts.net:8443.evil.example",
	}
	for _, h := range denied {
		for _, p := range []string{"/web-access/login", "/api/health", "/"} {
			res, b := w.do(http.MethodGet, p, nil, map[string]string{"Host": h})
			if res.StatusCode != http.StatusForbidden || errCode(t, b) != "auth.bad_host" {
				t.Errorf("denied host %q %s: got %d %s", h, p, res.StatusCode, b)
			}
		}
	}
}

func TestWebAccessUnauthenticated(t *testing.T) {
	w := newWebAccessEnv(t)

	// /api/... without cookie -> 401 auth.login_required
	for _, p := range []string{"/api/health", "/api/app", "/api/courses"} {
		res, b := w.do(http.MethodGet, p, nil, nil)
		if res.StatusCode != http.StatusUnauthorized || errCode(t, b) != "auth.login_required" {
			t.Errorf("GET %s without cookie: %d %s", p, res.StatusCode, b)
		}
	}

	// HTML GET / HEAD without cookie -> 303 to /web-access/login
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		res, _ := w.do(m, "/", nil, nil)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/web-access/login" {
			t.Errorf("%s / without cookie: %d loc=%q", m, res.StatusCode, res.Header.Get("Location"))
		}
	}

	// Non-GET/HEAD non-API without cookie -> 401 auth.login_required
	res, b := w.do(http.MethodPost, "/", nil, nil)
	if res.StatusCode != http.StatusUnauthorized || errCode(t, b) != "auth.login_required" {
		t.Errorf("POST / without cookie: %d %s", res.StatusCode, b)
	}

	// Login page renders cleanly without inline script and displays error on ?failed=1.
	res, b = w.do(http.MethodGet, "/web-access/login", nil, nil)
	if res.StatusCode != http.StatusOK || strings.Contains(string(b), "<script") {
		t.Fatalf("login page status=%d contains script=%v", res.StatusCode, strings.Contains(string(b), "<script"))
	}
	res, b = w.do(http.MethodGet, "/web-access/login?failed=1", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(b), "Wrong key") {
		t.Fatalf("login failed page status=%d body=%s", res.StatusCode, b)
	}
}

func TestWebAccessLoginCookieAndLogout(t *testing.T) {
	w := newWebAccessEnv(t)
	res, _, cookie := w.login(w.key)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("login success: status=%d loc=%q", res.StatusCode, res.Header.Get("Location"))
	}
	if cookie == nil {
		t.Fatal("expected lf_web_access cookie")
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.MaxAge != 30*24*3600 {
		t.Fatalf("unexpected cookie attributes: %+v", cookie)
	}

	// Authenticated request succeeds without X-LF-Token.
	res, b := w.do(http.MethodGet, "/api/health", nil, nil, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/health with cookie: %d %s", res.StatusCode, b)
	}
	res, b = w.do(http.MethodGet, "/", nil, nil, cookie)
	if res.StatusCode != http.StatusOK || len(b) == 0 {
		t.Fatalf("GET / with cookie: %d", res.StatusCode)
	}

	// Logout clears cookie and redirects to login.
	res, _ = w.do(http.MethodPost, "/web-access/logout", nil, map[string]string{
		"Sec-Fetch-Site": "same-origin",
	}, cookie)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/web-access/login" {
		t.Fatalf("logout: status=%d loc=%q", res.StatusCode, res.Header.Get("Location"))
	}
	var cleared *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == webAccessCookieName {
			cleared = c
		}
	}
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("expected cleared cookie, got %+v", cleared)
	}
}

func TestWebAccessWrongKeyAndRateLimit(t *testing.T) {
	w := newWebAccessEnv(t)

	for i := 0; i < webAccessMaxFailsPerMin; i++ {
		res, _, cookie := w.login("wrong-key")
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/web-access/login?failed=1" {
			t.Fatalf("attempt %d: status=%d loc=%q", i+1, res.StatusCode, res.Header.Get("Location"))
		}
		if cookie != nil {
			t.Fatalf("attempt %d: must not set cookie on failure", i+1)
		}
	}

	// 11th attempt within the minute is rate-limited with 429 (even with right key).
	res, _, _ := w.login(w.key)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after %d failures, got %d", webAccessMaxFailsPerMin, res.StatusCode)
	}

	// After a minute passes, login works again.
	w.advance(time.Minute + time.Second)
	res, _, cookie := w.login(w.key)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" || cookie == nil {
		t.Fatalf("after window expiry: status=%d loc=%q cookie=%v", res.StatusCode, res.Header.Get("Location"), cookie)
	}
}

func TestWebAccessCookieValidation(t *testing.T) {
	w := newWebAccessEnv(t)
	_, _, good := w.login(w.key)
	if good == nil {
		t.Fatal("missing cookie")
	}

	// Tampered signature rejected.
	tampered := &http.Cookie{Name: webAccessCookieName, Value: good.Value[:len(good.Value)-1] + "X"}
	if tampered.Value == good.Value {
		tampered.Value = good.Value[:len(good.Value)-1] + "Y"
	}
	res, b := w.do(http.MethodGet, "/api/health", nil, nil, tampered)
	if res.StatusCode != http.StatusUnauthorized || errCode(t, b) != "auth.login_required" {
		t.Fatalf("tampered cookie: %d %s", res.StatusCode, b)
	}

	// Cookie signed with a different key rejected.
	otherKeyCookie := &http.Cookie{
		Name:  webAccessCookieName,
		Value: signWebAccessCookie("different-key-after-rotation", w.now.Add(time.Hour).Unix()),
	}
	res, b = w.do(http.MethodGet, "/api/health", nil, nil, otherKeyCookie)
	if res.StatusCode != http.StatusUnauthorized || errCode(t, b) != "auth.login_required" {
		t.Fatalf("other-key cookie: %d %s", res.StatusCode, b)
	}

	// Expired cookie rejected.
	w.advance(webAccessSessionTTL + time.Second)
	res, b = w.do(http.MethodGet, "/api/health", nil, nil, good)
	if res.StatusCode != http.StatusUnauthorized || errCode(t, b) != "auth.login_required" {
		t.Fatalf("expired cookie: %d %s", res.StatusCode, b)
	}
}

func TestWebAccessDenyList(t *testing.T) {
	w := newWebAccessEnv(t)
	_, _, cookie := w.login(w.key)

	denied := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/app/shutdown"},
		{http.MethodPost, "/api/app/path-grants"},
		{http.MethodPost, "/api/app/data-dir"},
		{http.MethodPost, "/api/app/data-dir/check"},
		{http.MethodPost, "/api/app/import"},
		{http.MethodDelete, "/api/app/old-data-dir"},
		{http.MethodDelete, "/api/app/folder-pick"},
		{http.MethodPost, "/api/app/open-logs"},
		{http.MethodPost, "/api/app/update/download"},
		{http.MethodPost, "/api/app/update/install-on-quit"},
		{http.MethodPost, "/api/selfupdate/check"},
		{http.MethodDelete, "/api/selfupdate/pending"},
		{http.MethodPost, "/api/system/data-dir/restrict"},
		{http.MethodPost, "/api/nmhost/register"},
		{http.MethodPost, "/api/components/import"},
	}
	for _, tc := range denied {
		res, b := w.do(tc.method, tc.path, strings.NewReader("{}"), map[string]string{
			"Content-Type":   "application/json",
			"Sec-Fetch-Site": "same-origin",
		}, cookie)
		if res.StatusCode != http.StatusForbidden || errCode(t, b) != "web_access.not_available" {
			t.Errorf("%s %s: got %d %s, want 403 web_access.not_available", tc.method, tc.path, res.StatusCode, b)
		}
	}

	// Safe methods (GET) on read endpoints remain allowed.
	res, b := w.do(http.MethodGet, "/api/app/settings", nil, nil, cookie)
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /api/app/settings: %d %s", res.StatusCode, b)
	}
	// Normal non-host mutating operations (creating a course) remain allowed.
	res, b = w.do(http.MethodPost, "/api/courses", strings.NewReader(`{"name":"CS101"}`), map[string]string{
		"Content-Type":   "application/json",
		"Sec-Fetch-Site": "same-origin",
	}, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Errorf("POST /api/courses: %d %s", res.StatusCode, b)
	}
}

func TestWebAccessCrossSiteChecks(t *testing.T) {
	w := newWebAccessEnv(t)
	_, _, cookie := w.login(w.key)

	// Cross-site GET to /api/ refused.
	res, b := w.do(http.MethodGet, "/api/courses", nil, map[string]string{
		"Sec-Fetch-Site": "cross-site",
	}, cookie)
	if res.StatusCode != http.StatusForbidden || errCode(t, b) != "app.cross_site" {
		t.Fatalf("cross-site GET /api/courses: %d %s", res.StatusCode, b)
	}

	// Cross-site POST to /web-access/login refused (Sec-Fetch-Site).
	form := url.Values{"key": {w.key}}.Encode()
	res, b = w.do(http.MethodPost, "/web-access/login", strings.NewReader(form), map[string]string{
		"Content-Type":   "application/x-www-form-urlencoded",
		"Sec-Fetch-Site": "cross-site",
	})
	if res.StatusCode != http.StatusForbidden || errCode(t, b) != "app.cross_site" {
		t.Fatalf("cross-site POST login: %d %s", res.StatusCode, b)
	}

	// Without Sec-Fetch-Site, mismatched Origin must be refused (must NOT take desktop shortcut).
	res, b = w.do(http.MethodPost, "/web-access/login", strings.NewReader(form), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       "https://evil.example",
	})
	if res.StatusCode != http.StatusForbidden || errCode(t, b) != "app.cross_site" {
		t.Fatalf("cross-origin POST login without Sec-Fetch-Site: %d %s", res.StatusCode, b)
	}

	res, b = w.do(http.MethodPost, "/api/courses", strings.NewReader(`{"name":"Bad"}`), map[string]string{
		"Content-Type": "application/json",
		"Origin":       "https://evil.example",
	}, cookie)
	if res.StatusCode != http.StatusForbidden || errCode(t, b) != "app.cross_site" {
		t.Fatalf("cross-origin POST /api/courses without Sec-Fetch-Site: %d %s", res.StatusCode, b)
	}

	// Matching Origin without Sec-Fetch-Site is allowed.
	res, b = w.do(http.MethodPost, "/api/courses", strings.NewReader(`{"name":"Good"}`), map[string]string{
		"Content-Type": "application/json",
		"Origin":       "https://" + w.host,
	}, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("same-origin POST /api/courses without Sec-Fetch-Site: %d %s", res.StatusCode, b)
	}
}

func TestWebAccessGetAppMode(t *testing.T) {
	w := newWebAccessEnv(t)
	_, _, cookie := w.login(w.key)

	// Via web access: mode is "web", web_access is true, resources_dir is "".
	res, b := w.do(http.MethodGet, "/api/app", nil, nil, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("web-access GET /api/app: %d %s", res.StatusCode, b)
	}
	var waInfo AppInfo
	if err := json.Unmarshal(b, &waInfo); err != nil {
		t.Fatal(err)
	}
	if waInfo.Mode != "web" || !waInfo.WebAccess || waInfo.ResourcesDir != "" {
		t.Fatalf("web-access /api/app = %+v", waInfo)
	}

	// Via desktop listener on the same Server: mode stays "desktop", web_access is false, resources_dir set.
	res, b = w.env.doWith(http.MethodGet, "/api/app", nil, testToken, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("desktop GET /api/app: %d %s", res.StatusCode, b)
	}
	var deskInfo AppInfo
	if err := json.Unmarshal(b, &deskInfo); err != nil {
		t.Fatal(err)
	}
	if deskInfo.Mode != "desktop" || deskInfo.WebAccess || deskInfo.ResourcesDir != "/fake/resources" {
		t.Fatalf("desktop /api/app = %+v", deskInfo)
	}
}

// A browser on another device never names a path on this computer: the
// web-access listener answers a JSON path upload as web mode does
// (upload.not_multipart), even though the server is in desktop mode.
func TestWebAccessNoPathUpload(t *testing.T) {
	w := newWebAccessEnv(t)
	_, _, cookie := w.login(w.key)
	src := filepath.Join(t.TempDir(), "secret.m4a")
	if err := os.WriteFile(src, []byte("not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, b := w.do(http.MethodPost, "/api/courses", strings.NewReader(`{"name":"CS101"}`), map[string]string{
		"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin",
	}, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/courses: %d %s", res.StatusCode, b)
	}
	var c struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" {
		t.Fatalf("course id: %v %s", err, b)
	}
	body, _ := json.Marshal(map[string]string{"path": src})
	for _, p := range []string{"/api/courses/" + c.ID + "/lectures", "/api/courses/" + c.ID + "/materials"} {
		res, b = w.do(http.MethodPost, p, strings.NewReader(string(body)), map[string]string{
			"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin",
		}, cookie)
		if res.StatusCode != http.StatusBadRequest || errCode(t, b) != "upload.not_multipart" {
			t.Errorf("POST %s with a path: %d %s, want 400 upload.not_multipart", p, res.StatusCode, b)
		}
	}
}
