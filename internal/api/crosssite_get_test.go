// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// Final security audit AUD1 L-3: web mode has no token, and some GET
// requests change something (the event stream counts a visible front end,
// /api/colab may query Google, /api/app/storage walks the data directory,
// the media request may queue a transcode, cite.json records an export).
// Another site's page can make the browser send them (<img>, <video>, a
// no-cors fetch), so every /api/ request the browser marks as sent by
// another site (Sec-Fetch-Site cross-site or same-site, or a value it
// does not know) is refused, whatever its method. Each way LectureFlow's
// own pages reach the API stays open, one test per way.

// fetchHeaders are the Fetch Metadata headers of one kind of request.
func fetchHeaders(site, mode, dest string, extra ...string) map[string]string {
	h := map[string]string{}
	if site != "" {
		h["Sec-Fetch-Site"] = site
	}
	if mode != "" {
		h["Sec-Fetch-Mode"] = mode
	}
	if dest != "" {
		h["Sec-Fetch-Dest"] = dest
	}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

// streamStatus opens path as an event stream with headers and returns the
// status and content type without waiting for the stream to end.
func (e *env) streamStatus(path string, headers map[string]string) (int, string) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", e.ts.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode, res.Header.Get("Content-Type")
}

// lectureWithFiles is a lecture whose directory has a playback file, a
// wave file and an export.
func (e *env) lectureWithFiles() (Lecture, string) {
	e.t.Helper()
	c := e.course("Networks")
	l := e.draft(c.ID, "Week 1")
	dir := model.LectureDir(e.srv.dataDir, l.CourseID, l.ID)
	files := map[string]string{
		model.StreamFile: "fake mp4 bytes",
		model.AudioFile:  "fake wave bytes",
		filepath.Join(model.ExportDirName, "Week 1.bilingual.md"): "# Week 1\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	return l, "Week 1.bilingual.md"
}

// Web mode: a GET (or HEAD, OPTIONS) another site's page sent is refused
// on every /api/ path, before the handler runs: the citation export is
// not recorded.
func TestCrossSiteReadsRefused(t *testing.T) {
	e := newEnv(t)
	l, export := e.lectureWithFiles()
	paths := []string{
		"/api/health", "/api/courses", "/api/app/storage", "/api/hardware", "/api/resources", "/api/colab",
		"/api/courses/" + l.CourseID + "/export/cite.json",
		"/api/lectures/" + l.ID + "/media", "/api/lectures/" + l.ID + "/media?audio=1",
		"/api/lectures/" + l.ID + "/export/" + export, "/api", "/api/no-such-route",
	}
	sent := []map[string]string{
		fetchHeaders("cross-site", "no-cors", "image", "Origin", "https://evil.example", "Referer", "https://evil.example/"),
		fetchHeaders("cross-site", "no-cors", "video"),
		fetchHeaders("cross-site", "navigate", "document"), // a link on another site straight to the API
		fetchHeaders("same-site", "cors", "empty"),         // another port of localhost
		fetchHeaders("same-site", "no-cors", "audio"),
		fetchHeaders("cross-origin", "", ""), // a value the server does not know
	}
	for _, p := range paths {
		for _, h := range sent {
			for _, m := range []string{"GET", "HEAD", "OPTIONS"} {
				res, b := e.doHeaders(m, p, nil, h)
				if res.StatusCode != http.StatusForbidden || (m != "HEAD" && errCode(t, b) != "app.cross_site") {
					t.Errorf("%s %s %v: %d %s", m, p, h, res.StatusCode, b)
				}
			}
		}
	}
	if st, _ := e.streamStatus("/api/events", fetchHeaders("cross-site", "cors", "empty", "Accept", "text/event-stream")); st != http.StatusForbidden {
		t.Errorf("cross-site event stream: %d", st)
	}
	if n := e.srv.hub.Clients(); n != 0 {
		t.Errorf("%d event stream clients after the refused requests", n)
	}
	if v, ok, _ := e.st.GetKV(context.Background(), model.KVCiteExportPrefix+l.CourseID); ok {
		t.Errorf("citation export recorded by a refused request: %q", v)
	}
}

// LectureFlow's own pages, one way at a time.
func TestOwnPagesReachTheAPI(t *testing.T) {
	e := newEnv(t)
	l, export := e.lectureWithFiles()
	own := e.ts.URL
	for _, site := range []string{"same-origin", ""} { // "": a browser without Fetch Metadata
		// a fetch of the page's script
		if res, b := e.doHeaders("GET", "/api/courses", nil, fetchHeaders(site, "cors", "empty", "Referer", own+"/")); res.StatusCode != http.StatusOK {
			t.Errorf("%q fetch: %d %s", site, res.StatusCode, b)
		}
		// the event stream (EventSource)
		if st, ct := e.streamStatus("/api/events", fetchHeaders(site, "cors", "empty", "Accept", "text/event-stream")); st != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("%q event stream: %d %s", site, st, ct)
		}
		// <video> and <audio> with a range
		for _, c := range []struct{ path, dest string }{
			{"/api/lectures/" + l.ID + "/media", "video"},
			{"/api/lectures/" + l.ID + "/media?audio=1", "audio"},
		} {
			res, b := e.doHeaders("GET", c.path, nil, fetchHeaders(site, "no-cors", c.dest, "Range", "bytes=0-3"))
			if res.StatusCode != http.StatusPartialContent || len(b) != 4 {
				t.Errorf("%q <%s> %s: %d %q", site, c.dest, c.path, res.StatusCode, b)
			}
		}
		// a download link clicked on the page
		if res, b := e.doHeaders("GET", "/api/lectures/"+l.ID+"/export/"+export, nil, fetchHeaders(site, "navigate", "document", "Sec-Fetch-User", "?1")); res.StatusCode != http.StatusOK || string(b) != "# Week 1\n" {
			t.Errorf("%q download link: %d %q", site, res.StatusCode, b)
		}
		// the citation export the page downloads
		if res, b := e.doHeaders("GET", "/api/courses/"+l.CourseID+"/export/cite.json", nil, fetchHeaders(site, "cors", "empty")); res.StatusCode != http.StatusOK {
			t.Errorf("%q cite.json: %d %s", site, res.StatusCode, b)
		}
	}
	// a download address typed into the address bar or opened from a bookmark
	if res, b := e.doHeaders("GET", "/api/lectures/"+l.ID+"/export/"+export, nil, fetchHeaders("none", "navigate", "document", "Sec-Fetch-User", "?1")); res.StatusCode != http.StatusOK {
		t.Errorf("address bar download: %d %s", res.StatusCode, b)
	}
	// a client that is no browser (curl, a script)
	if res, b := e.doHeaders("GET", "/api/health", nil, nil); res.StatusCode != http.StatusOK {
		t.Errorf("no browser: %d %s", res.StatusCode, b)
	}
	// the front page opened from a link on another site: not under /api/
	if res, b := e.doHeaders("GET", "/", nil, fetchHeaders("cross-site", "navigate", "document", "Sec-Fetch-User", "?1")); res.StatusCode != http.StatusOK {
		t.Errorf("front page from another site: %d %s", res.StatusCode, b)
	}
	// a browser without Fetch Metadata keeps the Origin check for writes only (as before)
	if res, b := e.doHeaders("GET", "/api/app/settings", nil, map[string]string{"Origin": "http://evil.example"}); res.StatusCode != http.StatusOK {
		t.Errorf("old browser read: %d %s", res.StatusCode, b)
	}
}

// Desktop mode: the window loads the page without a token (an address the
// shell opens: none), the page's requests carry the token the main process
// injects (same-origin), the main process's own calls (Node's fetch sends
// no Sec-Fetch-Site) pass; another site's page is refused even with the
// token.
func TestDesktopCrossSiteReads(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	l, export := e.lectureWithFiles2()
	tok := func(h map[string]string) map[string]string {
		h[TokenHeader] = testToken
		return h
	}
	if res, b := e.doHeaders("GET", "/", nil, fetchHeaders("none", "navigate", "document")); res.StatusCode != http.StatusOK {
		t.Errorf("window load: %d %s", res.StatusCode, b)
	}
	for _, c := range []struct {
		name string
		path string
		h    map[string]string
		want int
	}{
		{"page fetch", "/api/app", tok(fetchHeaders("same-origin", "cors", "empty")), http.StatusOK},
		{"page <video>", "/api/lectures/" + l.ID + "/media", tok(fetchHeaders("same-origin", "no-cors", "video", "Range", "bytes=0-3")), http.StatusPartialContent},
		{"page download", "/api/lectures/" + l.ID + "/export/" + export, tok(fetchHeaders("same-origin", "navigate", "document")), http.StatusOK},
		{"main process", "/api/app/activity", tok(fetchHeaders("", "cors", "", "User-Agent", "node")), http.StatusOK},
		{"cross-site with the token", "/api/app", tok(fetchHeaders("cross-site", "no-cors", "image")), http.StatusForbidden},
		{"same-site with the token", "/api/app/activity", tok(fetchHeaders("same-site", "cors", "empty")), http.StatusForbidden},
	} {
		if res, b := e.doHeaders("GET", c.path, nil, c.h); res.StatusCode != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, res.StatusCode, b, c.want)
		}
	}
	if st, ct := e.streamStatus("/api/events", tok(fetchHeaders("same-origin", "cors", "empty", "Accept", "text/event-stream"))); st != http.StatusOK || !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("page event stream: %d %s", st, ct)
	}
}

// lectureWithFiles2 is lectureWithFiles for a desktop env, whose requests
// need the token (e.course and e.draft go through the API).
func (e *env) lectureWithFiles2() (Lecture, string) {
	e.t.Helper()
	ctx := context.Background()
	c := model.Course{ID: "C1", Name: "Networks", CreatedAt: "2026-01-01T00:00:00Z"}
	if err := e.st.CreateCourse(ctx, c); err != nil {
		e.t.Fatal(err)
	}
	ml := model.Lecture{ID: "L1", CourseID: c.ID, Title: "Week 1", CreatedAt: "2026-01-02T00:00:00Z"}
	if err := e.st.CreateLecture(ctx, ml); err != nil {
		e.t.Fatal(err)
	}
	dir := model.LectureDir(e.srv.dataDir, c.ID, ml.ID)
	for name, body := range map[string]string{
		model.StreamFile: "fake mp4 bytes",
		filepath.Join(model.ExportDirName, "Week 1.bilingual.md"): "# Week 1\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	return Lecture{ID: ml.ID, CourseID: c.ID}, "Week 1.bilingual.md"
}
