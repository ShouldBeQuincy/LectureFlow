// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// Opt-in "web access" listener for desktop mode: lets a browser on another
// device reach the LectureFlow UI through a reverse proxy (such as
// `tailscale serve`) using a stateless HMAC-signed session cookie backed by
// {data_dir}/web-access.key.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/dirperm"
)

const (
	// WebAccessKeyFileName is the access key file inside the data directory.
	// Deleting it and restarting LectureFlow rotates the key and invalidates
	// all existing web-access sessions.
	WebAccessKeyFileName = "web-access.key"

	webAccessCookieName = "lf_web_access"
	webAccessHMACPrefix = "lectureflow-web-access-v1|"
	webAccessSessionTTL = 30 * 24 * time.Hour

	webAccessDefaultFailDelay = 500 * time.Millisecond
	webAccessFailWindow       = time.Minute
	webAccessMaxFailsPerMin   = 10
	webAccessMaxFormBytes     = 4 << 10

	webAccessLoginPath  = "/web-access/login"
	webAccessLogoutPath = "/web-access/logout"
)

// WebAccessOptions configures the web-access middleware.
type WebAccessOptions struct {
	Key   string   // access key read from {data_dir}/web-access.key
	Port  int      // actual TCP port of the web-access listener (allows local loopback testing)
	Hosts []string // Host header values forwarded by the reverse proxy (exact, case-insensitive)

	// Now returns the current time; nil uses time.Now (tests inject a clock).
	Now func() time.Time
	// FailDelay is the pause before redirecting after a wrong login key.
	// Zero uses webAccessDefaultFailDelay (~500ms); negative disables the pause (tests).
	FailDelay time.Duration
}

type webAccessContextKey struct{}

func isWebAccess(ctx context.Context) bool {
	v, _ := ctx.Value(webAccessContextKey{}).(bool)
	return v
}

// webAccessDeniedPrefixes lists API prefixes blocked for non-safe HTTP methods
// over web access. These endpoints control the host computer itself — stopping
// the desktop process, granting local filesystem paths, moving or importing
// data directories, opening desktop folders, updating or replacing binaries,
// changing host filesystem ACLs, registering native-messaging hosts, or
// importing component bundles — and must only be triggered from the local
// desktop app, never from a remote browser session.
var webAccessDeniedPrefixes = []string{
	"/api/app/shutdown",
	"/api/app/path-grants",
	"/api/app/data-dir",
	"/api/app/import",
	"/api/app/old-data-dir",
	"/api/app/folder-pick",
	"/api/app/open-logs",
	"/api/app/update/",
	"/api/selfupdate/",
	"/api/system/data-dir/",
	"/api/nmhost/",
	"/api/components/import",
}

func isWebAccessDeniedPath(rawPath string) bool {
	clean := path.Clean(rawPath)
	for _, prefix := range webAccessDeniedPrefixes {
		if strings.HasSuffix(prefix, "/") {
			if strings.HasPrefix(clean, prefix) {
				return true
			}
			continue
		}
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return true
		}
	}
	return false
}

// LoadOrCreateWebAccessKey loads {dataDir}/web-access.key if it exists, or
// creates a new 32-byte random key (lowercase base32 without padding, mode
// 0600) on first start. Never log the returned key.
func LoadOrCreateWebAccessKey(dataDir string) (key, keyPath string, created bool, err error) {
	if dataDir == "" {
		return "", "", false, errors.New("web access: empty data directory")
	}
	if _, err := dirperm.MkdirPrivate(dataDir); err != nil {
		return "", "", false, fmt.Errorf("web access data dir: %w", err)
	}
	keyPath = filepath.Join(dataDir, WebAccessKeyFileName)
	b, err := os.ReadFile(keyPath)
	if err == nil {
		key = strings.TrimSpace(string(b))
		if key == "" {
			return "", keyPath, false, fmt.Errorf("web access key file %s is empty", keyPath)
		}
		if runtime.GOOS != "windows" {
			if fi, serr := os.Stat(keyPath); serr == nil && fi.Mode().Perm()&0o077 != 0 {
				_ = os.Chmod(keyPath, 0o600)
			}
		}
		return key, keyPath, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", keyPath, false, fmt.Errorf("read web access key %s: %w", keyPath, err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", keyPath, false, fmt.Errorf("generate web access key: %w", err)
	}
	key = strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))

	if err := writePrivateKeyFile(keyPath, key+"\n"); err != nil {
		return "", keyPath, false, fmt.Errorf("write web access key %s: %w", keyPath, err)
	}
	return key, keyPath, true, nil
}

func writePrivateKeyFile(dst, content string) error {
	f, err := os.CreateTemp(filepath.Dir(dst), WebAccessKeyFileName+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if runtime.GOOS != "windows" {
		if err := f.Chmod(0o600); err != nil {
			return err
		}
	}
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	ok = true
	return nil
}

// webAccessFailLimiter enforces a global sliding-window limit on failed logins.
type webAccessFailLimiter struct {
	mu       sync.Mutex
	failures []time.Time
}

func (l *webAccessFailLimiter) pruneLocked(now time.Time) {
	cutoff := now.Add(-webAccessFailWindow)
	n := 0
	for _, ts := range l.failures {
		if ts.After(cutoff) {
			l.failures[n] = ts
			n++
		}
	}
	l.failures = l.failures[:n]
}

// limited reports whether the failure limit has been reached and returns how
// many seconds until the oldest failure leaves the window.
func (l *webAccessFailLimiter) limited(now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	if len(l.failures) < webAccessMaxFailsPerMin {
		return false, 0
	}
	retry := int(l.failures[0].Add(webAccessFailWindow).Sub(now).Seconds()) + 1
	if retry < 1 {
		retry = 1
	}
	return true, retry
}

func (l *webAccessFailLimiter) recordFailure(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	l.failures = append(l.failures, now)
}

func signWebAccessCookie(key string, expiry int64) string {
	expStr := strconv.FormatInt(expiry, 10)
	return expStr + "." + webAccessMAC(key, expStr)
}

func webAccessMAC(key, expStr string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(webAccessHMACPrefix))
	m.Write([]byte(expStr))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func verifyWebAccessCookie(key, value string, now time.Time) bool {
	if key == "" || value == "" {
		return false
	}
	expStr, sig, ok := strings.Cut(value, ".")
	if !ok || expStr == "" || sig == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || exp <= now.Unix() {
		return false
	}
	want := webAccessMAC(key, expStr)
	return hmac.Equal([]byte(sig), []byte(want))
}

func webAccessHostAllowed(host string, port int, hosts []string) bool {
	if port > 0 && loopbackHost(host, port) {
		return true
	}
	for _, allowed := range hosts {
		if strings.EqualFold(host, allowed) {
			return true
		}
	}
	return false
}

func (s *Server) webAccess(opts WebAccessOptions) func(http.Handler) http.Handler {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	failDelay := opts.FailDelay
	if failDelay == 0 {
		failDelay = webAccessDefaultFailDelay
	} else if failDelay < 0 {
		failDelay = 0
	}
	hosts := append([]string(nil), opts.Hosts...)
	limiter := &webAccessFailLimiter{}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. Security headers (same as secure + Referrer-Policy: no-referrer).
			hd := w.Header()
			hd.Set("Content-Security-Policy", contentSecurityPolicy)
			hd.Set("X-Content-Type-Options", "nosniff")
			hd.Set("Referrer-Policy", "no-referrer")

			// 2. Host check.
			if !webAccessHostAllowed(r.Host, opts.Port, hosts) {
				writeError(w, http.StatusForbidden, "auth.bad_host", nil)
				return
			}

			// 3. Cross-site checks (web-mode semantics: never take the desktop shortcut).
			isAPI := r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")
			if isAPI && crossSiteFetch(r) {
				writeError(w, http.StatusForbidden, "app.cross_site", nil)
				return
			}
			if !safeMethod(r.Method) && crossOriginCheck(r, false) {
				writeError(w, http.StatusForbidden, "app.cross_site", nil)
				return
			}

			// 4. Login and logout endpoints (no cookie required).
			switch r.URL.Path {
			case webAccessLoginPath:
				serveWebAccessLogin(w, r, opts.Key, nowFn, failDelay, limiter)
				return
			case webAccessLogoutPath:
				serveWebAccessLogout(w, r)
				return
			}

			// 5. Require a valid session cookie for everything else.
			now := nowFn()
			cookieOK := false
			if c, err := r.Cookie(webAccessCookieName); err == nil {
				cookieOK = verifyWebAccessCookie(opts.Key, c.Value, now)
			}
			if !cookieOK {
				if isAPI {
					writeError(w, http.StatusUnauthorized, "auth.login_required", nil)
					return
				}
				if r.Method == http.MethodGet || r.Method == http.MethodHead {
					http.Redirect(w, r, webAccessLoginPath, http.StatusSeeOther)
					return
				}
				writeError(w, http.StatusUnauthorized, "auth.login_required", nil)
				return
			}

			// 6. Deny host-machine-only operations on non-safe methods.
			if !safeMethod(r.Method) && isWebAccessDeniedPath(r.URL.Path) {
				writeError(w, http.StatusForbidden, "web_access.not_available", nil)
				return
			}

			// 7. Mark request context as web-access and continue into the inner chain.
			ctx := context.WithValue(r.Context(), webAccessContextKey{}, true)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func serveWebAccessLogin(w http.ResponseWriter, r *http.Request, key string, nowFn func() time.Time, failDelay time.Duration, limiter *webAccessFailLimiter) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write([]byte(renderWebAccessLoginHTML(r.URL.Query().Get("failed") == "1")))
	case http.MethodPost:
		now := nowFn()
		if limited, retryAfter := limiter.limited(now); limited {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("Too many failed attempts. Please wait a minute and try again.\n"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, webAccessMaxFormBytes)
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, webAccessLoginPath+"?failed=1", http.StatusSeeOther)
			return
		}
		got := strings.TrimSpace(r.FormValue("key"))
		ok := key != "" && got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
		if !ok {
			limiter.recordFailure(now)
			if failDelay > 0 {
				timer := time.NewTimer(failDelay)
				select {
				case <-timer.C:
				case <-r.Context().Done():
					timer.Stop()
					return
				}
			}
			http.Redirect(w, r, webAccessLoginPath+"?failed=1", http.StatusSeeOther)
			return
		}
		exp := now.Add(webAccessSessionTTL).Unix()
		http.SetCookie(w, &http.Cookie{
			Name:     webAccessCookieName,
			Value:    signWebAccessCookie(key, exp),
			Path:     "/",
			MaxAge:   int(webAccessSessionTTL / time.Second),
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
}

func serveWebAccessLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     webAccessCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, webAccessLoginPath, http.StatusSeeOther)
}

func renderWebAccessLoginHTML(failed bool) string {
	errBlock := ""
	if failed {
		errBlock = `<p class="err" role="alert">Wrong key. Please try again. / 访问密钥不正确，请重试。</p>`
	}
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LectureFlow — Web Access</title>
<style>
  :root { color-scheme: light dark; }
  body {
    margin: 0;
    min-height: 100vh;
    display: grid;
    place-items: center;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: Canvas;
    color: CanvasText;
    padding: 16px;
  }
  main {
    width: 100%;
    max-width: 360px;
    padding: 24px;
    border: 1px solid color-mix(in srgb, CanvasText 18%, transparent);
    border-radius: 12px;
  }
  h1 { margin: 0 0 8px; font-size: 1.25rem; }
  p { margin: 0 0 16px; font-size: 0.92rem; opacity: 0.85; line-height: 1.45; }
  label { display: block; font-size: 0.85rem; margin-bottom: 6px; font-weight: 600; }
  input[type="password"] {
    width: 100%;
    box-sizing: border-box;
    padding: 10px 12px;
    font-size: 1rem;
    border-radius: 8px;
    border: 1px solid color-mix(in srgb, CanvasText 28%, transparent);
    background: Field;
    color: FieldText;
    margin-bottom: 14px;
  }
  button {
    width: 100%;
    padding: 10px 14px;
    font-size: 1rem;
    font-weight: 600;
    border: 0;
    border-radius: 8px;
    cursor: pointer;
    background: AccentColor;
    color: AccentColorText;
  }
  .err {
    color: #c62828;
    opacity: 1;
    font-weight: 500;
  }
  @media (prefers-color-scheme: dark) {
    .err { color: #ff8a80; }
  }
</style>
</head>
<body>
<main>
  <h1>LectureFlow</h1>
  <p>Enter the access key from <code>web-access.key</code>.<br>请输入 <code>web-access.key</code> 中的访问密钥。</p>
  ` + errBlock + `
  <form method="post" action="/web-access/login">
    <label for="key">Access key / 访问密钥</label>
    <input id="key" name="key" type="password" autocomplete="current-password" required autofocus>
    <button type="submit">Sign in / 登录</button>
  </form>
</main>
</body>
</html>
`
}
