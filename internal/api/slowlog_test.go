// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer: handlers_envkeys_test.go.

const testSlow = 40 * time.Millisecond

// slowClock is the slow-request log's clock in these tests (P6 T1): it
// moves only when a handler pauses, so a busy machine adds nothing to a
// request's time and the threshold is compared exactly.
type slowClock struct {
	mu sync.Mutex
	t  time.Time
}

func newSlowClock() *slowClock {
	return &slowClock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
}

func (c *slowClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *slowClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// slowMux registers fake handlers under real route patterns.
func slowMux(t *testing.T) (http.Handler, *syncBuffer) {
	t.Helper()
	mux := http.NewServeMux()
	clock := newSlowClock()
	pause := clock.advance
	mux.HandleFunc("GET /api/lectures/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slow") != "" {
			pause(2 * testSlow)
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/lectures/{id}/launch", func(w http.ResponseWriter, r *http.Request) {
		pause(2 * testSlow)
		writeError(w, http.StatusConflict, "lecture.busy", nil)
	})
	// SSE: answers at once, then stays open well past the threshold.
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through the wrapper: %v", err)
		}
		pause(3 * testSlow)
		_, _ = w.Write([]byte("data: x\n\n"))
	})
	// media: the first byte comes late (a hang before answering)
	mux.HandleFunc("GET /api/lectures/{id}/media", func(w http.ResponseWriter, r *http.Request) {
		pause(2 * testSlow)
		_, _ = w.Write([]byte("bytes"))
	})
	// an upload: slow because of its body, never reported
	mux.HandleFunc("POST /api/courses/{id}/lectures", func(w http.ResponseWriter, r *http.Request) {
		pause(2 * testSlow)
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	})
	// routes whose other wildcards may name a lecture or a server (D2)
	slowFirstByte := func(w http.ResponseWriter, r *http.Request) {
		pause(2 * testSlow)
		_, _ = w.Write([]byte("bytes"))
	}
	mux.HandleFunc("GET /api/lectures/{id}/export/{file}", slowFirstByte)
	mux.HandleFunc("GET /api/lectures/{id}/angles/{name}", slowFirstByte)
	mux.HandleFunc("GET /api/courses/{id}/export/cite.json", slowFirstByte)
	mux.HandleFunc("PATCH /api/remote/servers/{id}", func(w http.ResponseWriter, r *http.Request) {
		pause(2 * testSlow)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, nil))
	return slowRequestLogClock(mux, testSlow, log, clock.now), buf
}

// The threshold, exactly: a request that takes it is not logged, one that
// takes a nanosecond more is; for a streamed route the time to the first
// byte counts, however long the body takes after it.
func TestSlowRequestLogThreshold(t *testing.T) {
	clock := newSlowClock()
	mux := http.NewServeMux()
	var took, firstByteAfter, bodyAfter time.Duration
	mux.HandleFunc("GET /api/lectures/{id}", func(w http.ResponseWriter, r *http.Request) {
		clock.advance(took)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/lectures/{id}/media", func(w http.ResponseWriter, r *http.Request) {
		clock.advance(firstByteAfter)
		_, _ = w.Write([]byte("a"))
		clock.advance(bodyAfter)
		_, _ = w.Write([]byte("b"))
	})
	for _, c := range []struct {
		name              string
		path              string
		took, first, body time.Duration
		logged            bool
	}{
		{"at the threshold", "/api/lectures/L1", testSlow, 0, 0, false},
		{"just over", "/api/lectures/L2", testSlow + time.Nanosecond, 0, 0, true},
		{"far under", "/api/lectures/L3", time.Millisecond, 0, 0, false},
		{"streamed: long body after a quick first byte", "/api/lectures/L4/media", 0, testSlow, 100 * testSlow, false},
		{"streamed: first byte just over", "/api/lectures/L5/media", 0, testSlow + time.Nanosecond, 0, true},
	} {
		buf := &syncBuffer{}
		h := slowRequestLogClock(mux, testSlow, slog.New(slog.NewTextHandler(buf, nil)), clock.now)
		took, firstByteAfter, bodyAfter = c.took, c.first, c.body
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.path, nil))
		out := buf.String()
		if got := strings.Contains(out, `msg="slow request"`); got != c.logged {
			t.Errorf("%s: logged=%v, want %v:\n%s", c.name, got, c.logged, out)
		}
		if c.logged && !strings.Contains(out, "path="+c.path+" ") {
			t.Errorf("%s: no path=%s in:\n%s", c.name, c.path, out)
		}
	}
}

func TestSlowRequestLog(t *testing.T) {
	h, buf := slowMux(t)
	ts := httptest.NewServer(h)
	defer ts.Close()
	do := func(method, path string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		req.Header.Set(TokenHeader, "header-secret-123")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	do("GET", "/api/lectures/fast1?token=query-secret-456")
	do("GET", "/api/events?token=query-secret-456")
	do("POST", "/api/courses/c1/lectures?x=1")
	if out := buf.String(); out != "" {
		t.Fatalf("fast / streamed / upload requests were logged:\n%s", out)
	}

	do("GET", "/api/lectures/L7?slow=1&token=query-secret-456")
	do("POST", "/api/lectures/L8/launch")
	do("GET", "/api/lectures/L9/media?token=query-secret-456")
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 slow lines, got:\n%s", out)
	}
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`level=WARN msg="slow request" method=GET route="GET /api/lectures/\{id\}" path=/api/lectures/L7 status=200 duration_ms=\d+ measured=total`),
		regexp.MustCompile(`method=POST route="POST /api/lectures/\{id\}/launch" path=/api/lectures/L8/launch status=409 duration_ms=\d+ measured=total`),
		regexp.MustCompile(`method=GET route="GET /api/lectures/\{id\}/media" path=/api/lectures/L9/media status=200 duration_ms=\d+ measured=first_byte`),
	} {
		if !want.MatchString(out) {
			t.Errorf("no line matching %s in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"?", "token", "query-secret", "header-secret", "slow=1", "lecture.busy", `"ok"`} {
		if strings.Contains(out, bad) {
			t.Errorf("log contains %q:\n%s", bad, out)
		}
	}
}

// P6 D2: the slow-request log records the route's path with a lecture's or
// course's id, never the file name of an export, an angle's name or a
// server's slug from the request's path.
func TestSlowRequestLogPathWithoutNames(t *testing.T) {
	h, buf := slowMux(t)
	ts := httptest.NewServer(h)
	defer ts.Close()
	do := func(method, path string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	do("GET", "/api/lectures/01J9ZQ4M7R8T2V6X0B3C5D7E9F/export/Quantum%20Chromodynamics%20Lecture%203.srt")
	do("GET", "/api/lectures/L11/angles/Whiteboard%20of%20Prof.%20Placeholder.mp4")
	do("GET", "/api/courses/C12/export/cite.json")
	do("PATCH", "/api/remote/servers/secret-lab-box")
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 slow lines, got:\n%s", out)
	}
	for _, want := range []string{
		`route="GET /api/lectures/{id}/export/{file}" path=/api/lectures/01J9ZQ4M7R8T2V6X0B3C5D7E9F/export/{file} status=200`,
		`route="GET /api/lectures/{id}/angles/{name}" path=/api/lectures/L11/angles/{name} status=200`,
		`route="GET /api/courses/{id}/export/cite.json" path=/api/courses/C12/export/cite.json status=200`,
		`route="PATCH /api/remote/servers/{id}" path=/api/remote/servers/{id} status=200`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no line with %s in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"Quantum", "Chromodynamics", "Lecture", ".srt", "Whiteboard", "Placeholder", ".mp4", "secret-lab-box"} {
		if strings.Contains(out, bad) {
			t.Errorf("log contains %q:\n%s", bad, out)
		}
	}
}

// SEC32 M-1: through Handler's own middleware in desktop mode, where
// grantScope passes a new *Request to the router for every method but
// GET, HEAD and OPTIONS, a slow request still names its route, an upload
// route is still left out, and an {id} that is no id (a title typed into
// the path) is not filled in (SEC32 M7).
func TestSlowRequestLogDesktopChain(t *testing.T) {
	const token = "slow-test-token-0001"
	s := &Server{desktop: &DesktopOptions{Token: token, Port: 8765}}
	clock := newSlowClock()
	mux := http.NewServeMux()
	slow := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			clock.advance(2 * testSlow)
			w.WriteHeader(status)
		}
	}
	mux.HandleFunc("PATCH /api/lectures/{id}", slow(http.StatusOK))
	mux.HandleFunc("DELETE /api/remote/servers/{id}", slow(http.StatusNoContent))
	mux.HandleFunc("POST /api/lectures/{id}/launch", slow(http.StatusConflict))
	mux.HandleFunc("POST /api/courses/{id}/lectures", slow(http.StatusCreated)) // an upload
	mux.HandleFunc("GET /api/lectures/{id}/export/{file}", slow(http.StatusOK))
	do := func(method, path string) string {
		t.Helper()
		buf := &syncBuffer{}
		h := s.handlerChain(mux, testSlow, slog.New(slog.NewTextHandler(buf, nil)), clock.now)
		req := httptest.NewRequest(method, path, nil)
		req.Host = "127.0.0.1:8765"
		req.Header.Set(TokenHeader, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 400 && rec.Code != http.StatusConflict {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		return buf.String()
	}
	for _, c := range []struct{ method, path, want string }{
		{"PATCH", "/api/lectures/L21", `method=PATCH route="PATCH /api/lectures/{id}" path=/api/lectures/L21 status=200`},
		{"DELETE", "/api/remote/servers/secret-lab-box", `method=DELETE route="DELETE /api/remote/servers/{id}" path=/api/remote/servers/{id} status=204`},
		{"POST", "/api/lectures/01J9ZQ4M7R8T2V6X0B3C5D7E9F/launch", `route="POST /api/lectures/{id}/launch" path=/api/lectures/01J9ZQ4M7R8T2V6X0B3C5D7E9F/launch status=409`},
		// not an id: the title is not filled in
		{"POST", "/api/lectures/Quantum%20Title%20(draft)/launch", `route="POST /api/lectures/{id}/launch" path=/api/lectures/{id}/launch status=409`},
		{"GET", "/api/lectures/Quantum%20Title/export/x.srt", `route="GET /api/lectures/{id}/export/{file}" path=/api/lectures/{id}/export/{file} status=200`},
	} {
		out := do(c.method, c.path)
		if !strings.Contains(out, c.want) {
			t.Errorf("%s %s: no %s in:\n%s", c.method, c.path, c.want, out)
		}
		for _, bad := range []string{"(unmatched)", "Quantum", "Title", "secret-lab-box", "x.srt"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s %s: the log has %q:\n%s", c.method, c.path, bad, out)
			}
		}
	}
	if out := do("POST", "/api/courses/C12/lectures"); out != "" {
		t.Errorf("an upload route was logged:\n%s", out)
	}
}

// Every pattern the slow-request log singles out is a route of the real
// router (a renamed route would silently be timed in full again).
func TestSlowRequestRoutesExist(t *testing.T) {
	e := newEnv(t)
	mux := e.srv.routes()
	param := regexp.MustCompile(`\{[^}]+\}`)
	for _, m := range []map[string]bool{streamedRoutes, uploadRoutes} {
		for pattern := range m {
			method, p, _ := strings.Cut(pattern, " ")
			req := httptest.NewRequest(method, param.ReplaceAllString(p, "x"), nil)
			if _, got := mux.Handler(req); got != pattern {
				t.Errorf("%s routes to %q", pattern, got)
			}
		}
	}
}
