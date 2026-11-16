// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
)

// Lectures of the fixtures used below (see fixtures_test.go).
var (
	lecDraft      = lecID(c3631, 8) // draft
	lecStaleGloss = lecID(c3631, 3) // repair+translate stale, export outdated, snapshot differs from settings
	lecTranslRun  = lecID(c3631, 6) // translate running
	lecASRRun     = lecID(c3631, 7) // asr running
	lecDone       = lecID(c3631, 1) // all done
	lecFailedOR   = lecID(c2120, 5) // translate failed, snapshot provider has no key
	lecCancelled  = lecID(c2120, 6)
)

func doJSON(t *testing.T, method, url string, body any, want int, out any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, url, res.StatusCode, want, b)
	}
	if out != nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(out); err != nil {
			t.Fatalf("%s %s: %v\n%s", method, url, err, b)
		}
	}
}

func TestV11FilesDecodeStrict(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "testdata", "api")
	cases := map[string]any{
		"v11_courses.json":            &[]api.Course{},
		"v11_lectures.json":           &[]api.Lecture{},
		"v11_dashboard.json":          &api.Dashboard{},
		"v11_resources.json":          &resources.Snapshot{},
		"v11_resources_nogpu.json":    &resources.Snapshot{},
		"v11_settings.json":           &api.Settings{},
		"v11_usage_today.json":        &[]api.ProviderUsage{},
		"v11_needs_model_choice.json": &api.NeedsModelChoice{},
	}
	files, _ := filepath.Glob(filepath.Join(dir, "v11_*.json"))
	for _, f := range files {
		if _, ok := cases[filepath.Base(f)]; !ok {
			t.Errorf("%s is not checked by this test", filepath.Base(f))
		}
	}
	for name, v := range cases {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The course page mock must show every state combination (task brief).
func TestV11LecturesCoverAllStates(t *testing.T) {
	var lectures []api.Lecture
	readJSON(t, filepath.Join("..", "..", "..", "testdata", "api", "v11_lectures.json"), &lectures)
	seen := map[string]bool{}
	for _, l := range lectures {
		if l.Draft {
			seen["draft"] = true
		}
		if l.StaleText != nil {
			seen["stale_text"] = true
		}
		if l.Counts.PatchesPending > 0 {
			seen["patches"] = true
		}
		for _, st := range l.Stages {
			seen[st.Status] = true
			seen["display:"+st.DisplayStatus] = true
			if st.WaitingFor != nil {
				w := *st.WaitingFor
				if i := strings.Index(w, ":"); i > 0 {
					w = w[:i+1]
				}
				seen["wait:"+w] = true
			}
			if st.Status == model.StatusRunning && st.Progress > 0 && st.Progress < 1 {
				seen["progress"] = true
			}
		}
	}
	for _, k := range []string{
		"draft", "stale_text", "patches", "progress",
		"pending", "queued", "running", "done", "failed", "cancelled", "stale", "display:outdated",
		"wait:not_started", "wait:upstream:", "wait:gpu_slot", "wait:material_terms:", "wait:retry_backoff:",
	} {
		if !seen[k] {
			t.Errorf("fixtures do not cover %q", k)
		}
	}
}

func TestDashboardAndEmptyScenario(t *testing.T) {
	srv := testServer(t)
	var d api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard?mock=1", nil, 200, &d)
	if len(d.InProgress) == 0 || d.DraftTotal == 0 || d.ReviewTotal == 0 || d.StaleTotal == 0 || len(d.Failures) == 0 || len(d.RecentDone) == 0 || len(d.Courses) < 4 || len(d.UsageToday) == 0 {
		t.Errorf("dashboard is missing sections: %+v", d)
	}
	if len(d.RecentDone) > 10 {
		t.Errorf("recent_done has %d items", len(d.RecentDone))
	}
	// Failures are shifted to "the last 24 hours" relative to the server start.
	for _, f := range d.Failures {
		if ft, _ := time.Parse(time.RFC3339, deref(f.FinishedAt)); time.Since(ft) > 24*time.Hour {
			t.Errorf("failure %s finished %v ago", f.LectureID, time.Since(ft))
		}
	}
	var e api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard?mock=empty", nil, 200, &e)
	if len(e.Courses) != 0 || len(e.InProgress) != 0 || e.DraftTotal != 0 || e.Courses == nil {
		t.Errorf("empty scenario: %+v", e)
	}
	var courses []api.Course
	doJSON(t, "GET", srv.URL+"/api/courses?mock=empty", nil, 200, &courses)
	if len(courses) != 0 {
		t.Errorf("empty scenario has %d courses", len(courses))
	}
}

func TestResourcesEndpoint(t *testing.T) {
	srv := testServer(t)
	var r resources.Snapshot
	doJSON(t, "GET", srv.URL+"/api/resources?mock=1", nil, 200, &r)
	if !r.GPU.Available || r.GPU.UtilPct == nil || len(r.GPU.UtilHistory) != 30 || len(r.GPU.TempHistory) != 30 || r.GPU.ASRSlotsUsed != 1 || len(r.Providers) != 3 {
		t.Errorf("unexpected resources %+v", r)
	}
	var n resources.Snapshot
	doJSON(t, "GET", srv.URL+"/api/resources?mock=nogpu", nil, 200, &n)
	if n.GPU.Available || n.GPU.UtilPct != nil || n.GPU.Error == "" || n.Providers[0].Balance != nil || n.Providers[0].BalanceError == "" {
		t.Errorf("nogpu scenario %+v", n)
	}
}

func TestErrorAndSlowScenarios(t *testing.T) {
	srv := testServer(t)
	var e api.ErrorBody
	doJSON(t, "GET", srv.URL+"/api/dashboard?mock=error", nil, 500, &e)
	if e.Error.Detail == "" {
		t.Error("error scenario without message")
	}
	doJSON(t, "GET", srv.URL+"/api/courses?mock=error", nil, 200, nil) // the nav still loads
}

func TestLaunchDraft(t *testing.T) {
	srv := testServer(t)
	var l api.Lecture
	doJSON(t, "POST", srv.URL+"/api/lectures/"+lecDraft+"/launch", nil, 200, &l)
	if l.Draft || l.LaunchedAt == nil || l.Stages[0].Status != model.StatusQueued || l.Stages[0].WaitingFor == nil || l.Stages[1].Model == nil {
		t.Errorf("launch result %+v", l)
	}
	if strings.Join(l.LectureActions, ",") != "cancel,delete" {
		t.Errorf("lecture_actions %v", l.LectureActions)
	}
	doJSON(t, "POST", srv.URL+"/api/lectures/"+lecDraft+"/launch", nil, 409, &api.ErrorBody{})
	var d api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard", nil, 200, &d)
	if d.DraftTotal != 3 {
		t.Errorf("draft_total %d after launching one of 4", d.DraftTotal)
	}
	var res api.CourseOpResult
	doJSON(t, "POST", srv.URL+"/api/courses/"+courseID(c3121)+"/launch-drafts", nil, 200, &res)
	if len(res.Results) != 3 || !res.Results[0].OK || res.Course.DraftCount != 0 {
		t.Errorf("launch-drafts %+v", res)
	}
}

func TestUpdateNeedsModelChoice(t *testing.T) {
	srv := testServer(t)
	var nc api.NeedsModelChoice
	doJSON(t, "POST", srv.URL+"/api/lectures/"+lecStaleGloss+"/update", nil, 409, &nc)
	if nc.Error.Code != model.ErrNeedsModelChoice || len(nc.Diff) != 1 || nc.Diff[0].Stage != model.StageTranslate || !nc.Diff[0].SnapshotUsable {
		t.Fatalf("409 body %+v", nc)
	}
	var l api.Lecture
	doJSON(t, "POST", srv.URL+"/api/lectures/"+lecStaleGloss+"/update", api.ModelChoiceRequest{ModelChoice: model.ModelChoiceCurrent}, 200, &l)
	if l.StaleText != nil || l.Stages[3].Status != model.StatusQueued || l.Stages[4].Status != model.StatusPending || !strings.HasPrefix(l.Stages[4].Model.Label, "Anthropic") {
		t.Errorf("update result %+v", l.Stages)
	}
	// Course-level update asks once for every stale lecture of the course.
	var cnc api.NeedsModelChoice
	doJSON(t, "POST", srv.URL+"/api/courses/"+courseID(c2120)+"/update-stale", nil, 409, &cnc)
	if len(cnc.Diff) == 0 || cnc.Diff[0].LectureID == "" || cnc.Diff[0].LectureTitle == "" {
		t.Fatalf("course 409 %+v", cnc)
	}
	var res api.CourseOpResult
	doJSON(t, "POST", srv.URL+"/api/courses/"+courseID(c2120)+"/update-stale", api.ModelChoiceRequest{ModelChoice: model.ModelChoiceSnapshot}, 200, &res)
	if len(res.Results) != 1 || !res.Results[0].OK || res.Course.StaleLectureCount != 0 {
		t.Errorf("update-stale %+v", res)
	}
}

func TestRetryWithUnusableSnapshot(t *testing.T) {
	srv := testServer(t)
	url := srv.URL + "/api/lectures/" + lecFailedOR + "/stages/translate/retry"
	var nc api.NeedsModelChoice
	doJSON(t, "POST", url, nil, 409, &nc)
	if len(nc.Diff) != 1 || nc.Diff[0].SnapshotUsable || nc.Diff[0].Reason == "" {
		t.Fatalf("409 body %+v", nc)
	}
	doJSON(t, "POST", url, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceSnapshot}, 409, &api.ErrorBody{})
	var l api.Lecture
	doJSON(t, "POST", url, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceCurrent}, 200, &l)
	if l.Stages[4].Status != model.StatusQueued || l.Stages[4].Model.ProviderName != "Anthropic" {
		t.Errorf("retry result %+v", l.Stages[4])
	}
	// v3.2.x P1-b (Q9a, revising A4.2): a plain retry of an LLM stage asks
	// when its snapshot or a pending downstream one differs from the
	// current settings, even when every snapshot is usable (here: translate
	// after repair, DeepSeek → Anthropic) ...
	retry := srv.URL + "/api/lectures/" + lecCancelled + "/stages/repair/retry"
	nc = api.NeedsModelChoice{}
	doJSON(t, "POST", retry, nil, 409, &nc)
	if len(nc.Diff) != 1 || nc.Diff[0].Stage != model.StageTranslate || !nc.Diff[0].SnapshotUsable {
		t.Fatalf("409 body %+v", nc)
	}
	doJSON(t, "POST", retry, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceSnapshot}, 200, &l)
	// ... and the retry of any other stage still only when one is unusable.
	doJSON(t, "POST", srv.URL+"/api/lectures/01JD3121000000000000000006/stages/prep/retry", nil, 200, &l)
}

func TestDeleteForceAndBatch(t *testing.T) {
	srv := testServer(t)
	doJSON(t, "DELETE", srv.URL+"/api/lectures/"+lecASRRun, nil, 409, &api.ErrorBody{})
	doJSON(t, "DELETE", srv.URL+"/api/lectures/"+lecASRRun+"?force=1", nil, 204, nil)
	doJSON(t, "GET", srv.URL+"/api/lectures/"+lecASRRun, nil, 404, &api.ErrorBody{})

	var br api.BatchResponse
	doJSON(t, "POST", srv.URL+"/api/lectures/cancel", api.BatchRequest{IDs: []string{lecTranslRun, lecDone}}, 200, &br)
	if len(br.Results) != 2 || !br.Results[0].OK || br.Results[0].Lecture == nil || br.Results[1].OK || br.Results[1].Error == "" {
		t.Errorf("batch cancel %+v", br)
	}
	doJSON(t, "POST", srv.URL+"/api/lectures/delete", api.BatchRequest{IDs: []string{lecDone, "nope"}, Force: true}, 200, &br)
	if !br.Results[0].OK || br.Results[1].OK {
		t.Errorf("batch delete %+v", br)
	}
	doJSON(t, "POST", srv.URL+"/api/lectures/launch", api.BatchRequest{}, 400, &api.ErrorBody{})
}

func TestPatchLecture(t *testing.T) {
	srv := testServer(t)
	var l api.Lecture
	seq := 12
	title := "Week 1 - Intro (renamed)"
	// v2 P13.2: seq is computed from the recording times and cannot be set
	var e api.ErrorBody
	doJSON(t, "PATCH", srv.URL+"/api/lectures/"+lecDone, api.LecturePatch{Title: &title, Seq: &seq}, 400, &e)
	if e.Error.Code != "lecture.seq_readonly" {
		t.Errorf("seq: %+v", e)
	}
	doJSON(t, "PATCH", srv.URL+"/api/lectures/"+lecDone, api.LecturePatch{Title: &title}, 200, &l)
	// (seq 2: last year's recording comes first by date)
	if l.Title != title || l.TitleSource != model.TitleSourceUser || l.Seq != 2 || l.Label != "W01L1" {
		t.Errorf("patched %q %s %d %q", l.Title, l.TitleSource, l.Seq, l.Label)
	}
}

func TestAudioClipHeaders(t *testing.T) {
	srv := testServer(t)
	res, err := http.Get(srv.URL + "/api/lectures/L1/audio?start_ms=5000&end_ms=7400")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	h := res.Header
	if h.Get("X-Clip-Start-Ms") != "4000" || h.Get("X-Clip-Sentence-Start-Ms") != "5000" || h.Get("X-Clip-Sentence-End-Ms") != "7400" || h.Get("X-Clip-End-Ms") != "8400" {
		t.Errorf("clip headers %v", h)
	}
}

// A write operation pushes "changed" events to open SSE streams.
func TestChangedEventAfterMutation(t *testing.T) {
	srv := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	sc.Scan() // ": connected"
	go func() {
		if r, err := http.Post(srv.URL+"/api/lectures/"+lecDraft+"/launch", "application/json", nil); err == nil {
			r.Body.Close()
		}
	}()
	var event string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if event == "changed" && strings.HasPrefix(line, "data: ") {
			var ev api.ChangedEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Kind == "lecture" && ev.ID == lecDraft {
				return
			}
		}
	}
	t.Fatalf("no changed event for the launched lecture (scan err %v)", sc.Err())
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
