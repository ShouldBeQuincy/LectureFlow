// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/glossary"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
)

// v3.2.x r7 (P1 H3): the course list and the dashboard read the per-course
// data (lectures, materials, syllabus, kv entries, term and patch counts) for
// all courses at once instead of course by course.

// perCourseStore hides the store's batch reads (db.CourseReader, and with
// them db.Counter): a server over it reads course by course, as before.
type perCourseStore struct{ db.Store }

// countingStore counts the per-course reads and the batch reads of the
// course views; it forwards db.Counter and db.CourseReader to the store.
type countingStore struct {
	db.Store
	mu sync.Mutex
	n  map[string]int
}

func (c *countingStore) count(name string) {
	c.mu.Lock()
	c.n[name]++
	c.mu.Unlock()
}

func (c *countingStore) reset() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.n
	c.n = map[string]int{}
	return out
}

func (c *countingStore) ListLectures(ctx context.Context, id string) ([]model.Lecture, error) {
	c.count("ListLectures")
	return c.Store.ListLectures(ctx, id)
}
func (c *countingStore) ListMaterials(ctx context.Context, id string) ([]model.Material, error) {
	c.count("ListMaterials")
	return c.Store.ListMaterials(ctx, id)
}
func (c *countingStore) GetSyllabus(ctx context.Context, id string) (model.Syllabus, error) {
	c.count("GetSyllabus")
	return c.Store.GetSyllabus(ctx, id)
}
func (c *countingStore) GetKV(ctx context.Context, key string) (string, bool, error) {
	c.count("GetKV")
	return c.Store.GetKV(ctx, key)
}
func (c *countingStore) ListLectureKinds(ctx context.Context) ([]model.LectureKind, error) {
	c.count("ListLectureKinds")
	return c.Store.ListLectureKinds(ctx)
}
func (c *countingStore) LectureCounts(ctx context.Context, ids []string) (map[string]db.LectureCounts, error) {
	c.count("LectureCounts")
	return c.Store.(db.Counter).LectureCounts(ctx, ids)
}
func (c *countingStore) PendingPatchCounts(ctx context.Context, ids []string) (map[string]int, error) {
	c.count("PendingPatchCounts")
	return c.Store.(db.Counter).PendingPatchCounts(ctx, ids)
}
func (c *countingStore) TermCounts(ctx context.Context, ids []string) (map[string]int, error) {
	c.count("TermCounts")
	return c.Store.(db.Counter).TermCounts(ctx, ids)
}
func (c *countingStore) BatchLectures(ctx context.Context, ids []string) (map[string][]model.Lecture, error) {
	c.count("BatchLectures")
	return c.Store.(db.CourseReader).BatchLectures(ctx, ids)
}
func (c *countingStore) BatchMaterials(ctx context.Context, ids []string) (map[string][]model.Material, error) {
	c.count("BatchMaterials")
	return c.Store.(db.CourseReader).BatchMaterials(ctx, ids)
}
func (c *countingStore) BatchSyllabi(ctx context.Context, ids []string) (map[string]model.Syllabus, error) {
	c.count("BatchSyllabi")
	return c.Store.(db.CourseReader).BatchSyllabi(ctx, ids)
}
func (c *countingStore) BatchKV(ctx context.Context, keys []string) (map[string]string, error) {
	c.count("BatchKV")
	return c.Store.(db.CourseReader).BatchKV(ctx, keys)
}
func (c *countingStore) BatchCurrentJobs(ctx context.Context, scope string, ids []string) ([]model.Job, error) {
	c.count("BatchCurrentJobs")
	return c.Store.(db.CourseReader).BatchCurrentJobs(ctx, scope, ids)
}
func (c *countingStore) ListCurrentJobsByStatus(ctx context.Context, statuses ...string) ([]model.Job, error) {
	c.count("ListCurrentJobsByStatus")
	return c.Store.ListCurrentJobsByStatus(ctx, statuses...)
}

// batchFixtures builds courses covering the course views' cases: an empty
// course, one with drafts only, one with failed, cancelled, running and done
// lectures plus a syllabus, materials (one with mostly empty pages), terms,
// pending patches, a janitor run and every per-course kv entry, and a second
// course with a janitor report but no recorded run.
func batchFixtures(t *testing.T, e *env) []Course {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	empty := e.course("Empty course")
	drafts := e.course("Drafts only")
	e.draft(drafts.ID, "Draft 1")
	e.draft(drafts.ID, "Draft 2")

	mixed := e.course("Operating Systems")
	e.call("PATCH", "/api/courses/"+mixed.ID, CoursePatch{TermStart: ptr("2026-09-28")}, http.StatusOK, nil)
	failed := e.lecture(mixed.ID, "Failed")
	e.failStageAt(failed.ID, model.StagePrep, now.Add(-30*time.Hour))
	cancelled := e.lecture(mixed.ID, "Cancelled")
	e.moveJob(model.ScopeLecture, cancelled.ID, model.StagePrep, "queued", "cancelled")
	running := e.lecture(mixed.ID, "Running")
	e.moveJob(model.ScopeLecture, running.ID, model.StagePrep, "queued", "running")
	done := e.lecture(mixed.ID, "Done")
	e.allDone(done.ID)
	e.call("PATCH", "/api/lectures/"+done.ID, LecturePatch{RecordedAt: ptr("2026-10-06T10:00")}, http.StatusOK, nil)
	e.draft(mixed.ID, "A draft too")
	if err := e.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.InsertSentences(ctx, done.ID, []model.Sentence{
			{Idx: 0, EndMS: 1000, ASRText: "a", RepairedText: "a", ZhStatus: model.ZhNone},
			{Idx: 1, StartMS: 1000, EndMS: 2000, ASRText: "b", RepairedText: "b", ZhStatus: model.ZhNone}}); err != nil {
			return err
		}
		return tx.InsertPatches(ctx, []model.Patch{
			{ID: db.NewID(), LectureID: done.ID, SentenceIdx: 0, Before: "a", After: "A", Class: "decide", Review: "pending", CreatedAt: db.Now()},
			{ID: db.NewID(), LectureID: done.ID, SentenceIdx: 1, Before: "b", After: "B", Class: "decide", Review: "pending", CreatedAt: db.Now()},
		})
	}); err != nil {
		t.Fatal(err)
	}
	text := "课程大纲 Operating Systems — week 1: processes"
	if err := e.st.UpsertSyllabus(ctx, model.Syllabus{CourseID: mixed.ID, Filename: "syllabus.pdf", Path: "syllabus.pdf", Text: &text}); err != nil {
		t.Fatal(err)
	}
	lid := done.ID
	for i, m := range []model.Material{
		{Filename: "week1.pdf", PagesMD: []string{"Intro", "Processes", "Threads"}},
		{Filename: "Week3-scans.pdf", PagesMD: []string{"", " ", "Figure", "\n"}, LectureID: &lid},
		{Filename: "no-text.pdf"},
	} {
		m.ID, m.CourseID, m.Path = db.NewID(), mixed.ID, m.Filename
		m.CreatedAt = now.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339)
		if err := e.st.CreateMaterial(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, en := range []string{"deadlock", "semaphore"} {
		e.call("POST", "/api/courses/"+mixed.ID+"/terms", map[string]string{"en": en, "zh": en + "-zh"}, http.StatusCreated, nil)
	}
	if err := glossary.SaveRunReport(ctx, e.st, model.JanitorReport{CourseID: mixed.ID, FromVersion: 1, Version: 2, CreatedAt: db.Now(),
		ProviderName: "DeepSeek", Model: "deepseek-chat",
		Items: []model.JanitorItem{{TermID: "t1", En: "deadlock", Action: "disable", Reason: "common"}, {TermID: "t2", En: "x", Action: "disable", Undone: true}}}); err != nil {
		t.Fatal(err)
	}
	notices, _ := json.Marshal([]stages.RenumberNotice{{ErrorInfo: model.ErrorInfo{Code: "calendar.renumbered_auto"}, At: db.Now()}})
	for k, v := range map[string]string{
		model.KVFilenameProposal + mixed.ID:      `{"regex":"x"}`,
		model.KVFilenameUndo + mixed.ID:          `{"rule":null}`,
		model.KVCiteExportPrefix + mixed.ID:      db.Now(),
		stages.KVRenumberedAutoPrefix + mixed.ID: string(notices),
		model.KVFilenameProposal + drafts.ID:     "  ", // cleared: absent
	} {
		if err := e.st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}

	second := e.course("Networks")
	e.allDone(e.lecture(second.ID, "TCP").ID)
	e.call("POST", "/api/courses/"+second.ID+"/terms", map[string]string{"en": "socket", "zh": "套接字"}, http.StatusCreated, nil)
	if err := glossary.SaveReport(ctx, e.st, model.JanitorReport{CourseID: second.ID, FromVersion: 0, Version: 1, CreatedAt: db.Now(),
		Fallback: true, Items: []model.JanitorItem{{TermID: "t3", En: "socket", Action: "disable", Reason: "common"}}}); err != nil {
		t.Fatal(err)
	}
	return []Course{empty, drafts, mixed, second}
}

// The batch reads give exactly the responses of the course-by-course reads:
// the course list, every course and the dashboard, byte for byte.
func TestCourseBatchEqualsPerCourse(t *testing.T) {
	fixed := time.Now()
	e := newEnv(t, func(s *Server) { s.now = func() time.Time { return fixed } })
	courses := batchFixtures(t, e)
	old := newServer(Deps{Store: perCourseStore{e.st}, Sched: e.sc, Cfg: e.cfg, DataDir: e.cfg.DataDir, Hub: NewHub(), Settings: e.set},
		func(s *Server) {
			s.runCmd = fakeCmd
			s.freeSpace = func(string) (int64, error) { return 100 << 30, nil }
			s.now = func() time.Time { return fixed }
		})
	t.Cleanup(old.diag.waitStartup)
	oldTS := httptest.NewServer(old.Handler())
	t.Cleanup(oldTS.Close)

	get := func(base, path string) []byte {
		t.Helper()
		res, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return append([]byte(res.Status+"\n"), b...)
	}
	// the course page's lecture list and the citation export read the jobs
	// of their course only (R77 (f)); compared like the rest
	paths := []string{"/api/courses", "/api/dashboard"}
	for _, c := range courses {
		paths = append(paths, "/api/courses/"+c.ID, "/api/courses/"+c.ID+"/lectures", "/api/courses/"+c.ID+"/export/cite.json")
	}
	for _, p := range paths {
		got, want := get(e.ts.URL, p), get(oldTS.URL, p)
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs:\nbatch      %s\nper course %s", p, got, want)
		}
		if !bytes.HasPrefix(got, []byte("200 ")) && !bytes.HasSuffix([]byte(p), []byte("cite.json")) {
			t.Errorf("%s: %s", p, got)
		}
	}
	// The fixtures do reach the views (the comparison is not vacuous).
	var list []Course
	e.call("GET", "/api/courses", nil, http.StatusOK, &list)
	byName := map[string]Course{}
	for _, c := range list {
		byName[c.Name] = c
	}
	m := byName["Operating Systems"]
	if m.Syllabus == nil || len(m.Materials) != 3 || m.Materials[1].TextWarning != TextWarningIncomplete || m.TermCount != 2 ||
		m.PatchesPending != 2 || m.FailedCount != 1 || m.DraftCount != 1 || m.RunningCount != 1 || !m.FilenameProposal || !m.FilenameUndo ||
		!m.CiteExported || len(m.RenumberNotices) != 1 || m.Janitor.LastChanges != 1 || m.Janitor.LastModel == "" {
		t.Errorf("Operating Systems view lacks a fixture: %+v", m)
	}
	if n := byName["Networks"]; n.Janitor.LastAt == "" || n.Janitor.LastChanges != 1 || n.TermCount != 1 {
		t.Errorf("Networks janitor / terms: %+v", n)
	}
	if d := byName["Drafts only"]; d.DraftCount != 2 || d.FilenameProposal {
		t.Errorf("Drafts only: %+v", d)
	}
}

// The course list reads each kind of per-course data once for all courses:
// adding courses adds no statement (P1 H3: about 11 per course before).
func TestCourseListReadsInBatches(t *testing.T) {
	e := newEnv(t)
	courses := batchFixtures(t, e)
	cs := &countingStore{Store: e.st, n: map[string]int{}}
	srv := newServer(Deps{Store: cs, Sched: e.sc, Cfg: e.cfg, DataDir: e.cfg.DataDir, Hub: NewHub(), Settings: e.set},
		func(s *Server) {
			s.runCmd = fakeCmd
			s.freeSpace = func(string) (int64, error) { return 100 << 30, nil }
		})
	// The counts are the requests' own reads: no other goroutine may read
	// through cs while one is counted. The startup diagnostics run in the
	// background over the same store (environment reads kv asr.mode and
	// onboarding.state, then the checks run); a run scheduled late landed
	// in the first count (T29: 10 reads with 4 courses, 8 with 7, the
	// extra two GetKV from diagnostics.run). Wait for them before counting.
	// With no component manager nothing schedules another run (rerunAfter),
	// and the scheduler and the fixtures read e.st, not cs.
	srv.diag.waitStartup()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	get := func(p string) map[string]int {
		t.Helper()
		cs.reset()
		res, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
		return cs.reset()
	}
	before := get("/api/courses")
	for _, name := range []string{"ListLectures", "ListMaterials", "GetSyllabus"} {
		if before[name] != 0 {
			t.Errorf("/api/courses: %s called %d times, want 0 (read in batches)", name, before[name])
		}
	}
	for _, name := range []string{"BatchLectures", "BatchMaterials", "BatchSyllabi", "TermCounts", "PendingPatchCounts"} {
		if before[name] != 1 {
			t.Errorf("/api/courses: %s called %d times, want 1", name, before[name])
		}
	}
	// One course reads only its own jobs (through job_current), not every
	// current job of the library.
	one := get("/api/courses/" + courses[2].ID)
	if one["ListCurrentJobsByStatus"] != 0 || one["BatchCurrentJobs"] == 0 || one["ListLectures"] != 0 {
		t.Errorf("/api/courses/{id} reads: %v; want BatchCurrentJobs, no ListCurrentJobsByStatus", one)
	}
	// so do the course page's lecture list and the citation export (R77 (f))
	for _, p := range []string{"/api/courses/" + courses[2].ID + "/lectures", "/api/courses/" + courses[2].ID + "/export/cite.json"} {
		if n := get(p); n["ListCurrentJobsByStatus"] != 0 || n["BatchCurrentJobs"] == 0 {
			t.Errorf("%s reads: %v; want BatchCurrentJobs, no ListCurrentJobsByStatus", p, n)
		}
	}
	cs.reset()
	res, err := http.Post(ts.URL+"/api/courses/"+courses[2].ID+"/update-stale", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	// the course view in the response reads its own jobs as well
	if n := cs.reset(); res.StatusCode != http.StatusOK || n["ListCurrentJobsByStatus"] != 0 {
		t.Errorf("update-stale: %d, reads %v; want no ListCurrentJobsByStatus", res.StatusCode, n)
	}
	dash := get("/api/dashboard")
	if dash["ListLectures"] != 0 || dash["BatchLectures"] != 1 || dash["ListLectureKinds"] != 1 || dash["PendingPatchCounts"] != 1 || dash["TermCounts"] != 1 {
		t.Errorf("/api/dashboard reads: %v; want one batch each, no ListLectures", dash)
	}
	// Three more courses with a draft each (not listed on the dashboard, so
	// only the per-course reads could grow): no additional store read.
	for _, name := range []string{"Extra 1", "Extra 2", "Extra 3"} {
		c := e.course(name)
		e.draft(c.ID, "L1")
	}
	after := get("/api/courses")
	dashAfter := get("/api/dashboard")
	total := func(m map[string]int) int {
		n := 0
		for _, v := range m {
			n += v
		}
		return n
	}
	if total(after) != total(before) {
		t.Errorf("/api/courses: %d store reads with 4 courses, %d with 7: %v → %v", total(before), total(after), before, after)
	}
	if total(dashAfter) != total(dash) {
		t.Errorf("/api/dashboard: %d store reads with 4 courses, %d with 7: %v → %v", total(dash), total(dashAfter), dash, dashAfter)
	}
}
