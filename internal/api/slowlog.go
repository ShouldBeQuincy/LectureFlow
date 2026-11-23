// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Slow-request log (T063, test-upgrade F8): a request whose handling takes
// longer than slowRequestThreshold gets one WARN line, so a request that
// hangs leaves a trace. Not a setting.
const slowRequestThreshold = 10 * time.Second

// streamedRoutes answer with a long or open-ended body (the SSE stream,
// media with Range, file downloads): their time is measured up to the
// first byte of the response (headers included), not to the end of the
// body, so a stream that is merely long is not reported but one that never
// starts is.
var streamedRoutes = map[string]bool{
	"GET /api/events":                         true,
	"GET /api/lectures/{id}/media":            true,
	"GET /api/lectures/{id}/audio":            true,
	"GET /api/lectures/{id}/angles/{name}":    true,
	"GET /api/lectures/{id}/export/{file}":    true,
	"GET /api/lectures/{id}/english/{format}": true,
	"GET /api/courses/{id}/export/cite.json":  true,
}

// uploadRoutes read a whole file from the request body (multipart, or a
// desktop-mode path copy) before answering: their time depends on the file
// size, so they are not reported at all.
var uploadRoutes = map[string]bool{
	"POST /api/courses/{id}/lectures":     true,
	"PUT /api/lectures/{id}/media":        true,
	"POST /api/lectures/{id}/angles":      true,
	"POST /api/courses/{id}/materials":    true,
	"POST /api/courses/{id}/syllabus":     true,
	"POST /api/courses/{id}/syllabus/raw": true,
	"POST /api/components/import":         true,
	"POST /api/selfupdate/import":         true,
}

// slowRequestLogClock wraps next with the slow-request log, timed by now
// (Handler: time.Now; tests: a clock they move themselves, P6 T1). log nil
// means slog.Default() at the time of the request. Only the method, the
// route pattern, the route's path with a lecture's or course's id filled
// in (slowLogPath), the status and the duration are logged: never the
// request's own path, the query string, a header or a body. The route
// comes from slowRouteRecorder next to the router (handlerChain).
func slowRequestLogClock(next http.Handler, threshold time.Duration, log *slog.Logger, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := now()
		sw := &statusWriter{ResponseWriter: w, now: now}
		rt := &slowRoute{}
		r = r.WithContext(context.WithValue(r.Context(), slowRouteKey{}, rt))
		next.ServeHTTP(sw, r)
		end := now()
		// The route comes from the request the ServeMux matched:
		// slowRouteRecorder fills rt in (a middleware between, such as
		// grantScope, may pass on another *Request; SEC32 M-1). Without a
		// recorder (a bare mux): r itself, when nothing in between
		// replaced it.
		pattern, id := rt.get()
		if pattern == "" {
			pattern, id = r.Pattern, r.PathValue("id")
		}
		if uploadRoutes[pattern] {
			return
		}
		measured := "total"
		if streamedRoutes[pattern] {
			measured = "first_byte"
			if fb := sw.firstByteAt(); !fb.IsZero() {
				end = fb
			}
		}
		d := end.Sub(start)
		if d <= threshold {
			return
		}
		l := log
		if l == nil {
			l = slog.Default()
		}
		route := pattern
		if route == "" {
			route = "(unmatched)"
		}
		l.Warn("slow request", "method", r.Method, "route", route, "path", slowLogPath(pattern, id),
			"status", sw.statusCode(), "duration_ms", d.Milliseconds(), "measured", measured)
	})
}

// slowRoute is the route a request matched, filled in by
// slowRouteRecorder for the slow-request log around it.
type slowRoute struct {
	mu      sync.Mutex
	pattern string
	id      string // the {id} wildcard's value ("" = none)
}

type slowRouteKey struct{}

func (rt *slowRoute) get() (pattern, id string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.pattern, rt.id
}

// slowRouteRecorder wraps the router: once it has served a request, the
// pattern it matched and the {id} go to the slow-request log's slowRoute
// (in the context, which every r.WithContext in between keeps).
func slowRouteRecorder(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if rt, ok := r.Context().Value(slowRouteKey{}).(*slowRoute); ok {
			rt.mu.Lock()
			rt.pattern, rt.id = r.Pattern, r.PathValue("id")
			rt.mu.Unlock()
		}
	})
}

// handlerChain is Handler's middleware around routes: the slow-request
// log outermost, the security checks and the grants' scope, and the
// recorder of the matched route next to the router.
func (s *Server) handlerChain(routes http.Handler, threshold time.Duration, log *slog.Logger, now func() time.Time) http.Handler {
	return s.handlerChainWith(s.secure, routes, threshold, log, now)
}

// handlerChainWith builds the same middleware chain as handlerChain with a
// custom security gate (s.secure for the primary listener, s.webAccess for the
// opt-in web-access listener).
func (s *Server) handlerChainWith(gate func(http.Handler) http.Handler, routes http.Handler, threshold time.Duration, log *slog.Logger, now func() time.Time) http.Handler {
	return slowRequestLogClock(gate(s.grantScope(slowRouteRecorder(routes))), threshold, log, now)
}

// slowLogIDRoutes are the routes whose {id} is a lecture's or a course's
// (a generated ULID): the slow-request log names it.
var slowLogIDRoutes = []string{"/api/lectures/{id}", "/api/courses/{id}"}

var slowLogIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// slowLogPath is the path the slow-request log records (P6 D2): the
// route's own path, its wildcards kept as "{name}" except the id of a
// lecture or course. The request's path may carry a lecture's title (an
// export's file name, an angle's name), a server's name (its slug) or
// whatever was typed; an unmatched request's path is not recorded.
func slowLogPath(pattern, id string) string {
	if pattern == "" {
		return "(unmatched)"
	}
	p := pattern
	if _, rest, ok := strings.Cut(pattern, " "); ok {
		p = rest
	}
	if i := strings.IndexByte(p, '/'); i > 0 { // a host name in the pattern
		p = p[i:]
	}
	for _, prefix := range slowLogIDRoutes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			if slowLogIDRe.MatchString(id) {
				p = strings.Replace(p, "{id}", id, 1)
			}
			break
		}
	}
	return p
}

// statusWriter records the status and the time of the first byte. It keeps
// Flush and Unwrap so http.ResponseController (the SSE hub) and Flusher
// assertions still reach the real writer.
type statusWriter struct {
	http.ResponseWriter
	now    func() time.Time // nil: time.Now
	mu     sync.Mutex
	status int
	first  time.Time
}

func (s *statusWriter) mark(status int) {
	s.mu.Lock()
	if s.status == 0 {
		s.status = status
	}
	if s.first.IsZero() {
		if s.now != nil {
			s.first = s.now()
		} else {
			s.first = time.Now()
		}
	}
	s.mu.Unlock()
}

func (s *statusWriter) firstByteAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.first
}

func (s *statusWriter) statusCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == 0 {
		return http.StatusOK // what net/http sends for a handler that wrote nothing
	}
	return s.status
}

func (s *statusWriter) WriteHeader(code int) {
	// 1xx informational headers are not the response's status.
	if code >= 200 {
		s.mark(code)
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	s.mark(http.StatusOK)
	return s.ResponseWriter.Write(p)
}

// ReadFrom keeps the underlying writer's fast path (io.Copy from a file).
func (s *statusWriter) ReadFrom(src io.Reader) (int64, error) {
	s.mark(http.StatusOK)
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(s.ResponseWriter, src)
}

func (s *statusWriter) Flush() {
	s.mark(http.StatusOK)
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
