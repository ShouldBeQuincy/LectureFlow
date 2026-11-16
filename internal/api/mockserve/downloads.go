// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Recording downloads sent by the browser extension, in the mock
// (SPEC-ext 用户流程 4–6, .claude/ext-brief.md §6.2 "接口（冻结…）";
// web-builder, ext-step6):
//
//	GET    /api/downloads[?course_id=]  {"items": [Download]}, newest first
//	POST   /api/downloads/{id}/cancel   200 Download | 409 downloads.not_cancellable {state}
//	POST   /api/downloads/{id}/retry    200 Download | 409 downloads.needs_refresh,
//	                                    downloads.not_retryable {state}, or the
//	                                    task's own error {code, params} when no
//	                                    retry is offered (fetch.Retryable)
//	DELETE /api/downloads/{id}          204 | 409 downloads.active {state}
//	                                    unknown id: 404 downloads.not_found
//	GET/PATCH /api/app/settings         + download_concurrency (1–6, default 3) and
//	                                    download_connections (1–16, default 4);
//	                                    400 downloads.concurrency_invalid /
//	                                    connections_invalid {value, min, max}
//
// The sample tasks exist only in the scenario "downloads" (?mock=downloads):
// every state once on CSC1003 (downloading, queued twice, merging,
// importing, needs_refresh, failed, cancelled and a done one the page
// hides), and one download on COMP2120 that finishes: it downloads for
// about 45 s, merges, creates its lecture draft and is done. The other
// scenarios have no downloads, so their pages look as before. A change
// sends SSE "changed {kind: downloads, id}" (progress about once a
// second). The allowed actions are internal/fetch's (fetch.AllowedActions):
// cancel while queued, downloading, merging or needing a refresh; retry and
// remove when failed, remove only when a retry would fail again
// (fetch.Retryable); remove when done or cancelled; nothing while importing. A
// retried task waits in the queue for a moment, then downloads to the end.
//
// The scenario "download_errors" (?mock=download_errors, download_errors.go)
// has one failed or needs_refresh task for every task error code of
// internal/fetch instead, so that each reason's text can be seen.
//
// The download settings are per mock instance, like the App settings
// (v3_settings.go); this file serves GET/PATCH /api/app/settings before
// v3_settings.go does (it registers first, see init) and hands every other
// field to v3_settings.go's handlers.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/fetch"
	"lectureflow/internal/model"
)

const dlScenario = "downloads"

// The mock's timing (tests shorten it).
var (
	dlTickEvery  = time.Second             // SSE progress events
	dlMergeDur   = 4 * time.Second         // merging of the finishing task
	dlImportDur  = 3 * time.Second         // creating its lecture
	dlStartDelay = 1500 * time.Millisecond // a retried task waits in the queue
	dlRetryRate  = 5e6                     // bytes per second of a retried task
)

// Limits of the settings (internal/fetch).
const (
	dlMinConcurrency, dlMaxConcurrency, dlDefConcurrency = 1, 6, 3
	dlMinConnections, dlMaxConnections, dlDefConnections = 1, 16, 4
)

// The JSON is the server's (api.Download, api.DownloadList: the frozen
// Download of ext-brief §6.2).
type mockDownload = api.Download

// dlTask is a sample task and how it moves on.
type dlTask struct {
	mockDownload
	rate     float64   // bytes per second while downloading
	finishes bool      // downloads to the end, merges and imports (else stops at 99 %)
	last     time.Time // last advance
	phaseEnd time.Time // end of merging / importing; zero = stays
	startAt  time.Time // a retried task starts downloading then
	seq      int       // queue order
}

// dlState is the scenario's tasks; guarded by the store's mutex.
type dlState struct {
	tasks   []*dlTask
	seq     int
	ticking bool
}

var dlStates sync.Map // dlKey → *dlState

type dlSettings struct {
	mu          sync.Mutex
	concurrency int
	connections int
}

var dlSettingsOf sync.Map // *mock → *dlSettings

func init() {
	// First in the list: GET/PATCH /api/app/settings must be ours before
	// v3_settings.go registers them (its helper skips registered patterns).
	mockV3Routes = append([]func(m *mock, mux *http.ServeMux){registerDownloads}, mockV3Routes...)
}

func registerDownloads(m *mock, mux *http.ServeMux) {
	a := m.api
	mux.HandleFunc("GET /api/downloads", a(m.dlList))
	mux.HandleFunc("POST /api/downloads/{id}/cancel", a(m.dlCancel))
	mux.HandleFunc("POST /api/downloads/{id}/retry", a(m.dlRetry))
	mux.HandleFunc("DELETE /api/downloads/{id}", a(m.dlRemove))
	mux.HandleFunc("GET /api/app/settings", a(m.dlGetAppSettings))
	mux.HandleFunc("PATCH /api/app/settings", a(m.dlPatchAppSettings))
}

// ---------- the sample tasks ----------

const (
	dlCourseCrypto = "01JC0000000000000000003631" // CSC1003 Cryptography
	dlCourseOS     = "01JC0000000000000000002120" // COMP2120 Operating Systems
)

// dlKey: the two scenarios share the main store but not their tasks.
type dlKey struct {
	s  *store
	sc string
}

// dlStateFor returns the scenario's tasks moved on to now (nil outside the
// scenarios "downloads" and "download_errors"); s.mu held.
func (m *mock) dlStateFor(r *http.Request, s *store) *dlState {
	sc := scenario(r)
	if sc != dlScenario && sc != dlErrScenario {
		return nil
	}
	var st *dlState
	if v, ok := dlStates.Load(dlKey{s, sc}); ok {
		st = v.(*dlState)
	} else {
		if sc == dlErrScenario {
			st = newDLErrorState(time.Now())
		} else {
			st = newDLState(s, time.Now())
		}
		dlStates.Store(dlKey{s, sc}, st)
	}
	s.dlNotify(st.advance(s, time.Now()))
	m.dlTick(s, st)
	return st
}

func newDLState(s *store, now time.Time) *dlState {
	const mb = int64(1_000_000)
	av := []api.DownloadTrack{{Kind: "audio", Format: "hls", Role: "primary"}, {Kind: "video", Format: "hls", Role: "screen"}}
	audio := []api.DownloadTrack{{Kind: "audio", Format: "file", Role: "primary", Label: "Podcast"}}
	video := []api.DownloadTrack{{Kind: "video", Format: "file", Role: "primary", Label: "Podcast"}}
	rec := func(daysAgo int, hm string) string {
		return now.AddDate(0, 0, -daysAgo).Format("2006-01-02") + "T" + hm
	}
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	doneLecture := ""
	for _, l := range s.lectures {
		if l.CourseID == dlCourseCrypto {
			doneLecture = l.ID
			break
		}
	}
	mk := func(id int64, course, title, recorded, state string, created time.Duration, done, total int64, tracks []api.DownloadTrack) *dlTask {
		return &dlTask{mockDownload: mockDownload{
			ID: id, Platform: "panopto", SessionID: fmt.Sprintf("c0ffee%02d-0000-4000-8000-%012d", id, id), CourseID: course,
			Title: title, RecordedAt: recorded, State: state, BytesDone: done, BytesTotal: total, Tracks: tracks,
			CreatedAt: at(created), UpdatedAt: at(created / 2),
		}, last: now}
	}
	st := &dlState{}
	add := func(t *dlTask) *dlTask {
		st.seq++
		t.seq = st.seq
		st.tasks = append(st.tasks, t)
		return t
	}
	// CSC1003: every state once
	t := add(mk(41, dlCourseCrypto, "Week 8 · Elliptic curves", rec(5, "09:35"), "downloading", 20*time.Minute, 488*mb, 1284*mb, av))
	t.rate = 6 * float64(mb)
	add(mk(42, dlCourseCrypto, "Week 8 · Pairings", rec(4, "14:00"), "queued", 6*time.Minute, 0, -1, av))
	t = add(mk(43, dlCourseCrypto, "Week 9 · Zero-knowledge proofs", rec(1, "09:35"), "queued", 5*time.Minute, 0, -1, audio))
	t.OverrideDisabled = true
	t = add(mk(44, dlCourseCrypto, "Week 3 · Block ciphers", rec(40, "09:35"), "merging", 35*time.Minute, 1104*mb, 1104*mb, av))
	t = add(mk(45, dlCourseCrypto, "Week 5 · Public-key encryption", rec(26, "09:35"), "importing", 40*time.Minute, 655*mb, 655*mb, video))
	t = add(mk(46, dlCourseCrypto, "Week 7 · Key exchange", rec(12, "09:35"), "needs_refresh", 50*time.Minute, 312*mb, 980*mb, av))
	t.Error = &model.ErrorInfo{Code: "downloads.restarted"}
	t = add(mk(47, dlCourseCrypto, "Week 6 · Digital signatures", rec(19, "09:35"), "failed", 2*time.Hour, 96*mb, 712*mb, av))
	t.Error = &model.ErrorInfo{Code: "downloads.http_status", Params: map[string]any{"status": 503}, Detail: "aria2c: errorCode=22 the response status is not successful. status=503"}
	t = add(mk(48, dlCourseCrypto, "Week 4 · Hash functions", rec(33, "09:35"), "cancelled", 3*time.Hour, 0, 870*mb, av))
	t.Platform = "generic"
	t = add(mk(49, dlCourseCrypto, "Week 2 · Classical ciphers", rec(47, "09:35"), "done", 26*time.Hour, 640*mb, 640*mb, audio))
	t.LectureID = doneLecture
	// COMP2120: one that finishes and creates its lecture
	t = add(mk(51, dlCourseOS, "Lecture 7 · Virtual memory", rec(2, "11:00"), "downloading", 8*time.Minute, 231*mb, 420*mb, audio))
	t.rate, t.finishes = 4*float64(mb), true
	st.settle()
	return st
}

// settle sets the positions and the allowed actions.
func (st *dlState) settle() {
	var queued []*dlTask
	for _, t := range st.tasks {
		t.Position = 0
		if t.State == "queued" {
			queued = append(queued, t)
		}
		t.AllowedActions = dlActions(t)
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].seq < queued[j].seq })
	for i, t := range queued {
		t.Position = i + 1
	}
}

// dlActions: internal/fetch's own (a failure a retry cannot fix offers
// remove only: fetch.AllowedActions, T18 N2 and M1).
func dlActions(t *dlTask) []string {
	code := ""
	if t.Error != nil {
		code = t.Error.Code
	}
	return fetch.AllowedActions(t.State, code)
}

func dlEnded(state string) bool { return state == "done" || state == "failed" || state == "cancelled" }

// advance moves the tasks on to now and returns the ids that changed; s.mu
// held. The finishing task's lecture is created here (store.addDraft).
func (st *dlState) advance(s *store, now time.Time) []int64 {
	var changed []int64
	for _, t := range st.tasks {
		before := t.mockDownload.State + "/" + strconv.FormatInt(t.BytesDone, 10)
		switch t.State {
		case "queued":
			if !t.startAt.IsZero() && !now.Before(t.startAt) {
				t.State, t.startAt = "downloading", time.Time{}
			}
		case "downloading":
			if t.rate > 0 && t.BytesTotal > 0 {
				limit := t.BytesTotal * 99 / 100
				if t.finishes {
					limit = t.BytesTotal
				}
				if t.BytesDone < limit {
					t.BytesDone = min(limit, t.BytesDone+int64(t.rate*now.Sub(t.last).Seconds()))
				}
				if t.finishes && t.BytesDone >= t.BytesTotal {
					t.State, t.phaseEnd = "merging", now.Add(dlMergeDur)
				}
			}
		case "merging":
			if !t.phaseEnd.IsZero() && !now.Before(t.phaseEnd) {
				t.State, t.phaseEnd = "importing", now.Add(dlImportDur)
			}
		case "importing":
			if !t.phaseEnd.IsZero() && !now.Before(t.phaseEnd) {
				lec := s.addDraft(t.CourseID, t.Title, now)
				t.State, t.LectureID, t.phaseEnd = "done", lec.ID, time.Time{}
			}
		}
		t.last = now
		if t.mockDownload.State+"/"+strconv.FormatInt(t.BytesDone, 10) != before {
			t.UpdatedAt = now.UTC().Format(time.RFC3339)
			changed = append(changed, t.ID)
		}
	}
	if len(changed) > 0 {
		st.settle()
	}
	return changed
}

// moving: something will still change by itself.
func (st *dlState) moving() bool {
	for _, t := range st.tasks {
		switch t.State {
		case "queued":
			if !t.startAt.IsZero() {
				return true
			}
		case "downloading":
			if t.rate > 0 && t.BytesTotal > 0 && (t.finishes || t.BytesDone < t.BytesTotal*99/100) {
				return true
			}
		case "merging", "importing":
			if !t.phaseEnd.IsZero() {
				return true
			}
		}
	}
	return false
}

// dlTick sends the progress events while something moves and an event
// stream listens (one goroutine at most; it ends when nothing moves or
// nobody listens, and the next request starts it again). The tasks move
// with the wall clock either way: every request advances them. s.mu held.
func (m *mock) dlTick(s *store, st *dlState) {
	if st.ticking || !st.moving() {
		return
	}
	st.ticking = true
	go func() {
		tk := time.NewTicker(dlTickEvery)
		defer tk.Stop()
		for range tk.C {
			s.mu.Lock()
			s.dlNotify(st.advance(s, time.Now()))
			more := st.moving() && len(s.subs) > 0
			if !more {
				st.ticking = false
			}
			s.mu.Unlock()
			if !more {
				return
			}
		}
	}()
}

// dlNotify sends "changed downloads" for each id; s.mu held.
func (s *store) dlNotify(ids []int64) {
	for _, id := range ids {
		s.notify(api.ChangedEvent{Kind: "downloads", ID: strconv.FormatInt(id, 10)})
	}
}

func (st *dlState) find(r *http.Request) *dlTask {
	if st == nil {
		return nil
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil
	}
	for _, t := range st.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// ---------- handlers ----------

func (m *mock) dlList(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.DownloadList{Items: []api.Download{}}
	st := m.dlStateFor(r, s)
	if st != nil {
		course := r.URL.Query().Get("course_id")
		for _, t := range st.tasks {
			if course == "" || t.CourseID == course {
				out.Items = append(out.Items, t.mockDownload)
			}
		}
		sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].CreatedAt > out.Items[j].CreatedAt })
	}
	writeJSON(w, http.StatusOK, out)
}

func dlNotFound(w http.ResponseWriter) {
	codedError(w, http.StatusNotFound, "downloads.not_found", nil)
}

func (m *mock) dlCancel(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := m.dlStateFor(r, s)
	t := st.find(r)
	if t == nil {
		dlNotFound(w)
		return
	}
	if dlEnded(t.State) || t.State == "importing" {
		codedError(w, http.StatusConflict, "downloads.not_cancellable", map[string]any{"state": t.State})
		return
	}
	t.State, t.Error, t.startAt, t.phaseEnd = "cancelled", nil, time.Time{}, time.Time{}
	t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	st.settle()
	s.dlNotify([]int64{t.ID})
	writeJSON(w, http.StatusOK, t.mockDownload)
}

func (m *mock) dlRetry(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := m.dlStateFor(r, s)
	t := st.find(r)
	switch {
	case t == nil:
		dlNotFound(w)
		return
	case t.State == "needs_refresh":
		codedError(w, http.StatusConflict, "downloads.needs_refresh", nil)
		return
	case t.State != "failed":
		codedError(w, http.StatusConflict, "downloads.not_retryable", map[string]any{"state": t.State})
		return
	case t.Error != nil && !fetch.Retryable(t.Error.Code):
		// as fetch.Queue.Retry: the task's own reason, 409
		codedError(w, http.StatusConflict, t.Error.Code, t.Error.Params)
		return
	}
	now := time.Now()
	st.seq++
	t.seq = st.seq
	t.State, t.Error = "queued", nil
	t.startAt, t.last = now.Add(dlStartDelay), now
	t.rate, t.finishes = dlRetryRate, true
	if t.BytesTotal <= 0 {
		t.BytesTotal = 500_000_000
	}
	t.UpdatedAt = now.UTC().Format(time.RFC3339)
	st.settle()
	s.dlNotify([]int64{t.ID})
	m.dlTick(s, st)
	writeJSON(w, http.StatusOK, t.mockDownload)
}

func (m *mock) dlRemove(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := m.dlStateFor(r, s)
	t := st.find(r)
	if t == nil {
		dlNotFound(w)
		return
	}
	if !dlEnded(t.State) {
		codedError(w, http.StatusConflict, "downloads.active", map[string]any{"state": t.State})
		return
	}
	for i, x := range st.tasks {
		if x == t {
			st.tasks = append(st.tasks[:i], st.tasks[i+1:]...)
			break
		}
	}
	st.settle()
	s.dlNotify([]int64{t.ID})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- the download settings in /api/app/settings ----------

func (m *mock) dlSet() *dlSettings {
	if v, ok := dlSettingsOf.Load(m); ok {
		return v.(*dlSettings)
	}
	v, _ := dlSettingsOf.LoadOrStore(m, &dlSettings{concurrency: dlDefConcurrency, connections: dlDefConnections})
	return v.(*dlSettings)
}

// dlWithSettings answers like v3_settings.go's handler and adds the two
// download fields to its JSON object.
func (m *mock) dlWithSettings(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	if rec.Code != http.StatusOK {
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		writeError(w, http.StatusInternalServerError, "mock：应用设置不是 JSON 对象")
		return
	}
	ds := m.dlSet()
	ds.mu.Lock()
	out["download_concurrency"], out["download_connections"] = ds.concurrency, ds.connections
	ds.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (m *mock) dlGetAppSettings(w http.ResponseWriter, r *http.Request, s *store) {
	rec := httptest.NewRecorder()
	m.setGetAppSettings(rec, r, s)
	m.dlWithSettings(w, rec)
}

func (m *mock) dlPatchAppSettings(w http.ResponseWriter, r *http.Request, s *store) {
	var in map[string]json.RawMessage
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	type field struct {
		key, code string
		min, max  int
		set       func(*dlSettings, int)
		val       *int
	}
	fields := []*field{
		{key: "download_concurrency", code: "downloads.concurrency_invalid", min: dlMinConcurrency, max: dlMaxConcurrency, set: func(d *dlSettings, v int) { d.concurrency = v }},
		{key: "download_connections", code: "downloads.connections_invalid", min: dlMinConnections, max: dlMaxConnections, set: func(d *dlSettings, v int) { d.connections = v }},
	}
	ours := false
	for _, f := range fields {
		raw, ok := in[f.key]
		if !ok {
			continue
		}
		delete(in, f.key)
		ours = true
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n != float64(int(n)) || int(n) < f.min || int(n) > f.max {
			codedError(w, http.StatusBadRequest, f.code, map[string]any{"value": string(raw), "min": f.min, "max": f.max})
			return
		}
		v := int(n)
		f.val = &v
	}
	rec := httptest.NewRecorder()
	if len(in) > 0 || !ours {
		// the other fields (or an empty patch: nothing_to_change) go to v3_settings.go
		body, _ := json.Marshal(in)
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(bytes.NewReader(body))
		m.setPatchAppSettings(rec, r2, s)
		if rec.Code != http.StatusOK {
			m.dlWithSettings(w, rec)
			return
		}
	} else {
		time.Sleep(250 * time.Millisecond) // the "saving" state is visible, as in v3_settings.go
		m.setGetAppSettings(rec, r, s)
	}
	ds := m.dlSet()
	ds.mu.Lock()
	for _, f := range fields {
		if f.val != nil {
			f.set(ds, *f.val)
		}
	}
	ds.mu.Unlock()
	m.dlWithSettings(w, rec)
}
