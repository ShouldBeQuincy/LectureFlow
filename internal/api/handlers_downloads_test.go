// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/fetch"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// fakeDownloads is a download queue with fixed tasks.
type fakeDownloads struct {
	mu       sync.Mutex
	tasks    map[int64]fetch.TaskInfo
	errs     map[int64]error
	listed   []string // course_id of every List
	changed  int
	activity fetch.Activity
}

func (f *fakeDownloads) List(_ context.Context, courseID string) ([]fetch.TaskInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, courseID)
	var out []fetch.TaskInfo
	for _, t := range f.tasks {
		if courseID == "" || t.CourseID == courseID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeDownloads) Get(_ context.Context, id int64) (fetch.TaskInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return t, model.Errorf(fetch.CodeNotFound, nil, fetch.ErrNotFound)
	}
	return t, nil
}

func (f *fakeDownloads) act(id int64, state string) (fetch.TaskInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[id]; err != nil {
		return fetch.TaskInfo{}, err
	}
	t, ok := f.tasks[id]
	if !ok {
		return t, model.Errorf(fetch.CodeNotFound, nil, fetch.ErrNotFound)
	}
	t.State = state
	f.tasks[id] = t
	return t, nil
}

func (f *fakeDownloads) Cancel(_ context.Context, id int64) (fetch.TaskInfo, error) {
	return f.act(id, "cancelled")
}
func (f *fakeDownloads) Retry(_ context.Context, id int64) (fetch.TaskInfo, error) {
	return f.act(id, "queued")
}
func (f *fakeDownloads) Remove(_ context.Context, id int64) error {
	_, err := f.act(id, "")
	if err == nil {
		f.mu.Lock()
		delete(f.tasks, id)
		f.mu.Unlock()
	}
	return err
}
func (f *fakeDownloads) Activity() fetch.Activity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activity
}
func (f *fakeDownloads) SettingsChanged() {
	f.mu.Lock()
	f.changed++
	f.mu.Unlock()
}

func newDownloadsEnv(t *testing.T, f *fakeDownloads) *env {
	return newEnvWith(t, envConfig{server: func(s *Server) { s.dl = f }})
}

func TestDownloadsUnavailable(t *testing.T) {
	e := newEnv(t)
	for _, c := range [][2]string{{"GET", "/api/downloads"}, {"POST", "/api/downloads/1/cancel"},
		{"POST", "/api/downloads/1/retry"}, {"DELETE", "/api/downloads/1"}} {
		info := e.callErrInfo(c[0], c[1], nil, http.StatusServiceUnavailable)
		if info.Code != "server.unavailable" || info.Params["service"] != "downloads" {
			t.Errorf("%v: %+v", c, info)
		}
	}
	// the settings work without a queue
	var s AppSettings
	e.call("PATCH", "/api/app/settings", map[string]any{"download_concurrency": 2}, http.StatusOK, &s)
	if s.DownloadConcurrency != 2 || s.DownloadConnections != 4 {
		t.Fatalf("%+v", s)
	}
}

func TestDownloadsEndpoints(t *testing.T) {
	f := &fakeDownloads{tasks: map[int64]fetch.TaskInfo{
		12: {ID: 12, Platform: "panopto", SessionID: "s12", CourseID: "C1", Title: "Lecture 3", RecordedAt: "2026-10-21T09:35",
			State: "downloading", BytesDone: 10, BytesTotal: -1, AllowedActions: []string{"cancel"},
			Tracks:    []fetch.TrackInfo{{Kind: "audio", Format: "hls", Role: "primary-audio", Label: "Lecture audio"}},
			CreatedAt: "2026-10-26T10:00:00Z", UpdatedAt: "2026-10-26T10:01:00Z"},
		13: {ID: 13, Platform: "generic", SessionID: "s13", CourseID: "C2", State: "failed", BytesTotal: -1,
			Error: &model.ErrorInfo{Code: fetch.CodeHTTPStatus, Params: map[string]any{"status": 500}}, AllowedActions: []string{"retry", "remove"}},
	}, errs: map[int64]error{}}
	e := newDownloadsEnv(t, f)

	var list DownloadList
	e.call("GET", "/api/downloads?course_id=C1", nil, http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].ID != 12 || list.Items[0].Tracks[0].Role != "primary-audio" || f.listed[0] != "C1" {
		t.Fatalf("list %+v", list)
	}
	// the wire shape of the frozen interface
	_, raw := e.do("GET", "/api/downloads", nil)
	for _, want := range []string{`"items":[`, `"session_id":"s12"`, `"course_id":"C1"`, `"recorded_at":"2026-10-21T09:35"`,
		`"recorded_at":""`, `"lecture_id":""`, `"error":null`, `"position":0`, `"bytes_total":-1`, `"override_disabled":false`,
		`"allowed_actions":["cancel"]`, `"allowed_actions":["retry","remove"]`, `"tracks":[]`,
		`"error":{"code":"downloads.http_status","params":{"status":500}}`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("list lacks %s: %s", want, raw)
		}
	}

	var d Download
	e.call("POST", "/api/downloads/12/cancel", nil, http.StatusOK, &d)
	if d.ID != 12 || d.State != "cancelled" {
		t.Fatalf("cancel %+v", d)
	}
	e.call("POST", "/api/downloads/13/retry", nil, http.StatusOK, &d)
	if d.State != "queued" {
		t.Fatalf("retry %+v", d)
	}
	f.errs[13] = model.Errorf(fetch.CodeNeedsRefresh, nil, fetch.ErrConflict)
	if info := e.callErrInfo("POST", "/api/downloads/13/retry", nil, http.StatusConflict); info.Code != fetch.CodeNeedsRefresh {
		t.Fatalf("needs refresh %+v", info)
	}
	f.errs[13] = model.Errorf(fetch.CodeActive, map[string]any{"state": "downloading"}, fetch.ErrConflict)
	if info := e.callErrInfo("DELETE", "/api/downloads/13", nil, http.StatusConflict); info.Code != fetch.CodeActive || info.Params["state"] != "downloading" {
		t.Fatalf("remove active %+v", info)
	}
	delete(f.errs, 13)
	if res, b := e.do("DELETE", "/api/downloads/13", nil); res.StatusCode != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("remove: %d %s", res.StatusCode, b)
	}
	for _, p := range []string{"/api/downloads/13/retry", "/api/downloads/abc/cancel", "/api/downloads/0/retry"} {
		if info := e.callErrInfo("POST", p, nil, http.StatusNotFound); info.Code != fetch.CodeNotFound {
			t.Errorf("%s: %+v", p, info)
		}
	}
	if info := e.callErrInfo("DELETE", "/api/downloads/-4", nil, http.StatusNotFound); info.Code != fetch.CodeNotFound {
		t.Fatalf("bad id %+v", info)
	}
}

// Security review L6: cancel, retry and remove of a download are refused
// when another site's page sends them (web mode has no token); the task is
// left alone.
func TestDownloadActionsCrossSite(t *testing.T) {
	f := &fakeDownloads{tasks: map[int64]fetch.TaskInfo{
		12: {ID: 12, CourseID: "C1", State: "downloading", AllowedActions: []string{"cancel"}},
		13: {ID: 13, CourseID: "C1", State: "failed", AllowedActions: []string{"retry", "remove"}},
	}, errs: map[int64]error{}}
	e := newDownloadsEnv(t, f)
	for _, c := range [][2]string{{"POST", "/api/downloads/12/cancel"}, {"POST", "/api/downloads/13/retry"}, {"DELETE", "/api/downloads/13"}} {
		for _, h := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Origin": "https://evil.example"}} {
			res, b := e.doHeaders(c[0], c[1], nil, h)
			if res.StatusCode != http.StatusForbidden || errCode(t, b) != "app.cross_site" {
				t.Errorf("%v %v: %d %s", c, h, res.StatusCode, b)
			}
		}
	}
	f.mu.Lock()
	unchanged := f.tasks[12].State == "downloading" && f.tasks[13].State == "failed"
	f.mu.Unlock()
	if !unchanged {
		t.Fatalf("a refused request changed a task: %+v", f.tasks)
	}
	if res, b := e.doHeaders("POST", "/api/downloads/12/cancel", nil, map[string]string{"Sec-Fetch-Site": "same-origin"}); res.StatusCode != http.StatusOK {
		t.Fatalf("own page: %d %s", res.StatusCode, b)
	}
}

func TestDownloadSettings(t *testing.T) {
	f := &fakeDownloads{tasks: map[int64]fetch.TaskInfo{}}
	e := newDownloadsEnv(t, f)
	ctx := context.Background()
	var s AppSettings
	e.call("GET", "/api/app/settings", nil, http.StatusOK, &s)
	if s.DownloadConcurrency != 3 || s.DownloadConnections != 4 {
		t.Fatalf("defaults %+v", s)
	}
	e.call("PATCH", "/api/app/settings", map[string]any{"download_concurrency": 6, "download_connections": 16}, http.StatusOK, &s)
	if s.DownloadConcurrency != 6 || s.DownloadConnections != 16 || f.changed != 1 {
		t.Fatalf("patched %+v, changed %d", s, f.changed)
	}
	if v, _, _ := e.st.GetKV(ctx, fetch.KVConcurrency); v != "6" {
		t.Fatalf("kv %q", v)
	}
	e.call("PATCH", "/api/app/settings", map[string]any{"close_behavior": "quit"}, http.StatusOK, &s)
	if f.changed != 1 {
		t.Fatal("an unrelated setting re-read the download limits")
	}
	for _, c := range []struct {
		body  map[string]any
		code  string
		value float64
	}{
		{map[string]any{"download_concurrency": 0}, fetch.CodeConcurrencyInvalid, 0},
		{map[string]any{"download_concurrency": 7, "close_behavior": "ask"}, fetch.CodeConcurrencyInvalid, 7},
		{map[string]any{"download_connections": 17}, fetch.CodeConnectionsInvalid, 17},
		{map[string]any{"download_connections": 0, "download_concurrency": 2}, fetch.CodeConnectionsInvalid, 0},
	} {
		info := e.callErrInfo("PATCH", "/api/app/settings", c.body, http.StatusBadRequest)
		if info.Code != c.code || info.Params["value"] != c.value || info.Params["min"] == nil || info.Params["max"] == nil {
			t.Errorf("%v: %+v", c.body, info)
		}
	}
	if info := e.callErrInfo("PATCH", "/api/app/settings", map[string]any{"download_connections": 2.5}, http.StatusBadRequest); info.Code != "request.bad_json" {
		t.Fatalf("non-integer: %+v", info)
	}
	e.call("GET", "/api/app/settings", nil, http.StatusOK, &s)
	if s.DownloadConcurrency != 6 || s.DownloadConnections != 16 || s.CloseBehavior != "quit" {
		t.Fatalf("after invalid patches %+v", s)
	}
}

func TestActivityCountsDownloads(t *testing.T) {
	f := &fakeDownloads{tasks: map[int64]fetch.TaskInfo{}, activity: fetch.Activity{Active: 2, Queued: 1}}
	e := newDownloadsEnv(t, f)
	var a AppActivity
	e.call("GET", "/api/app/activity", nil, http.StatusOK, &a)
	if a.Running != 2 || a.Queued != 1 || a.Downloads != 3 || a.Transcribing {
		t.Fatalf("%+v", a)
	}
}

// writeDownload puts a finished download where the queue keeps it.
func writeDownload(t *testing.T, e *env, content string) string {
	t.Helper()
	dir := filepath.Join(e.cfg.DataDir, "downloads", "7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "media.mp4")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportDownload(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("CSC1003")
	src := writeDownload(t, e, "media bytes")
	rec := "2026-10-21T09:35"
	res, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{TaskID: 7, CourseID: c.ID, Path: src,
		Filename: "Lecture 3 Graphs.mp4", Title: "Lecture 3: Graphs", RecordedAt: &rec})
	if err != nil || res.LaunchError != nil || res.LectureID == "" {
		t.Fatalf("import %+v %v", res, err)
	}
	l, err := e.st.GetLecture(ctx, res.LectureID)
	if err != nil {
		t.Fatal(err)
	}
	if l.Title != "Lecture 3: Graphs" || l.TitleSource != model.TitleSourceFilename || l.OriginalFilename != "Lecture 3 Graphs.mp4" ||
		l.RecordedAt == nil || *l.RecordedAt != rec || l.RecordedAtSource != model.RecordedAtUser || l.LaunchedAt == nil {
		t.Fatalf("lecture %+v", l)
	}
	if b, err := os.ReadFile(l.MediaPath); err != nil || string(b) != "media bytes" ||
		!strings.HasPrefix(l.MediaPath, filepath.Join(e.cfg.DataDir, "courses")) || filepath.Base(l.MediaPath) != "media.mp4" {
		t.Fatalf("media %s %q %v", l.MediaPath, b, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("the download was copied, not moved: %v", err)
	}
	// no file-name-rule task for a synthetic name
	jobs, err := e.st.ListCurrentJobs(ctx, model.ScopeCourse, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Stage == model.StageFilenameRule {
			t.Fatalf("a filename_rule job was created: %+v", j)
		}
	}

	// unknown course: coded error, the file stays for a retry
	src = writeDownload(t, e, "again")
	if _, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{CourseID: "01NOSUCH", Path: src, Filename: "x.mp4", Title: "x"}); err == nil {
		t.Fatal("import into a missing course")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("the download is gone after a failed import: %v", err)
	}
}

// A launch the settings refuse leaves the draft and says why.
func TestImportDownloadLaunchRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.clearRoles()
	e.configureRole(model.RoleSummary) // some roles but not all: launches are refused
	c := e.course("CSC1003")
	src := writeDownload(t, e, "x")
	res, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{CourseID: c.ID, Path: src, Filename: "Week 1.mp4", Title: ""})
	if err != nil || res.LaunchError == nil || res.LaunchError.Code != settings.CodeRoleNotConfigured {
		t.Fatalf("%+v %v", res, err)
	}
	l, _ := e.st.GetLecture(ctx, res.LectureID)
	if l.LaunchedAt != nil || l.Title != "Week 1" || l.RecordedAt != nil || l.RecordedAtSource != model.RecordedAtNone {
		t.Fatalf("draft %+v", l)
	}
}

// The path upload still behaves as before the import was shared.
func TestImportLocalUserPathChecks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// a user's path inside the data directory is refused (the app's own
	// files only go through ImportDownload)
	inside := writeDownload(t, e, "x")
	e.grant(inside) // as if the user had picked it (T-SEC-08)
	_, err := e.srv.copyFromPath(ctx, pathRequest{path: inside}, filepath.Join(e.cfg.DataDir, "tmpdir"), "media", nil)
	if he, ok := err.(*httpError); !ok || he.info.Code != "upload.path_in_data_dir" {
		t.Fatalf("inside: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "Lecture.MP4")
	if err := os.WriteFile(outside, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.grant(outside)
	res, err := e.srv.copyFromPath(ctx, pathRequest{path: outside, fields: map[string]string{"title": "T"}}, filepath.Join(e.root(), "dst"), "media", nil)
	if err != nil || res.filename != "Lecture.MP4" || filepath.Base(res.path) != "media.mp4" || res.size != 3 || res.fields["title"] != "T" {
		t.Fatalf("copy %+v %v", res, err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("the user's file was moved")
	}
	_ = json.Valid
}

func (e *env) root() string { return e.dir }
