// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

func openTest(t *testing.T) Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sp(s string) *string { return &s }

func mkCourse(t *testing.T, st Store) model.Course {
	t.Helper()
	c := model.Course{ID: NewID(), Name: "CS101"}
	if err := st.CreateCourse(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	c, err := st.GetCourse(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mkLecture(t *testing.T, st Store, courseID string) model.Lecture {
	t.Helper()
	l := model.Lecture{ID: NewID(), CourseID: courseID, Title: "Week 1", MediaPath: "media.mp4"}
	if err := st.CreateLecture(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	l, err := st.GetLecture(context.Background(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestCreateLectureInsertsSixPendingJobs(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	l := mkLecture(t, st, c.ID)
	if l.CreatedAt == "" {
		t.Fatal("created_at not filled")
	}
	jobs, err := st.ListCurrentJobs(ctx, model.ScopeLecture, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 6 {
		t.Fatalf("want 6 jobs, got %d", len(jobs))
	}
	for i, j := range jobs {
		if j.Stage != model.LectureStages[i] || j.Status != model.StatusPending || !j.IsCurrent || j.Attempt != 0 {
			t.Fatalf("job %d = %+v", i, j)
		}
	}
	// The partial unique index allows only one current job per stage.
	err = st.InsertJob(ctx, model.Job{ID: NewID(), Scope: model.ScopeLecture, ScopeID: l.ID, Stage: model.StagePrep,
		Status: model.StatusPending, IsCurrent: true})
	if err == nil {
		t.Fatal("second current job for the same stage must violate job_current")
	}
}

func TestCreateLectureIsAtomic(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	// Unknown course id: the FK fails, so no job may be left behind.
	l := model.Lecture{ID: NewID(), CourseID: "nope", Title: "x", MediaPath: "m"}
	if err := st.CreateLecture(ctx, l); err == nil {
		t.Fatal("expected FK error")
	}
	jobs, err := st.ListCurrentJobs(ctx, model.ScopeLecture, l.ID)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("jobs left behind: %v %v", jobs, err)
	}
}

func TestMaterialJobsAndDelete(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	l := mkLecture(t, st, c.ID)
	bound := model.Material{ID: NewID(), CourseID: c.ID, LectureID: &l.ID, Filename: "w1.pdf", Path: "p"}
	wide := model.Material{ID: NewID(), CourseID: c.ID, Filename: "all.pdf", Path: "p2"}
	for _, m := range []model.Material{bound, wide} {
		if err := st.CreateMaterial(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := st.ListCurrentJobs(ctx, model.ScopeMaterial, bound.ID)
	if len(jobs) != 2 || jobs[0].Stage != model.StageMaterialExtract || jobs[1].Stage != model.StageMaterialTerms {
		t.Fatalf("material jobs = %+v", jobs)
	}
	m, err := st.GetMaterial(ctx, bound.ID)
	if err != nil || m.PagesMD != nil || m.LectureID == nil || *m.LectureID != l.ID {
		t.Fatalf("material = %+v err=%v", m, err)
	}
	if err := st.SetMaterialPages(ctx, bound.ID, []string{"# p1", ""}); err != nil {
		t.Fatal(err)
	}
	m, _ = st.GetMaterial(ctx, bound.ID)
	if !reflect.DeepEqual(m.PagesMD, []string{"# p1", ""}) {
		t.Fatalf("pages = %#v", m.PagesMD)
	}
	if err := st.SetMaterialLecture(ctx, wide.ID, nil); err != nil {
		t.Fatal(err)
	}
	ms, _ := st.ListMaterials(ctx, c.ID)
	if len(ms) != 2 || ms[0].ID != bound.ID {
		t.Fatalf("list materials = %+v", ms)
	}

	if err := st.DeleteMaterial(ctx, wide.ID); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := st.ListCurrentJobs(ctx, model.ScopeMaterial, wide.ID); len(jobs) != 0 {
		t.Fatal("material jobs not deleted")
	}
	if _, err := st.GetMaterial(ctx, wide.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// Deleting the lecture removes its jobs and the jobs of bound materials.
	if err := st.DeleteLecture(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := st.ListCurrentJobs(ctx, model.ScopeLecture, l.ID); len(jobs) != 0 {
		t.Fatal("lecture jobs not deleted")
	}
	if jobs, _ := st.ListCurrentJobs(ctx, model.ScopeMaterial, bound.ID); len(jobs) != 0 {
		t.Fatal("bound material jobs not deleted")
	}
	if _, err := st.GetMaterial(ctx, bound.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("bound material must cascade")
	}
	if err := st.DeleteLecture(ctx, l.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestSentencesAndRecomputeText(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	l := mkLecture(t, st, c.ID)
	ss := []model.Sentence{
		{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "hello cash", RepairedText: "hello cash",
			LowConfWords: []model.LowConfWord{{I: 1, W: "cash"}}},
		{Idx: 1, StartMS: 1000, EndMS: 2000, ASRText: "second", RepairedText: "second"},
		{Idx: 2, StartMS: 2000, EndMS: 3000, ASRText: "third", RepairedText: "third"},
	}
	if err := st.InsertSentences(ctx, l.ID, ss); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListSentences(ctx, l.ID, 0, 0)
	if err != nil || len(got) != 3 {
		t.Fatalf("list: %v %v", got, err)
	}
	if got[0].Text != "hello cash" || !reflect.DeepEqual(got[0].LowConfWords, []model.LowConfWord{{I: 1, W: "cash"}}) ||
		got[0].ZhStatus != model.ZhNone || got[1].LowConfWords == nil {
		t.Fatalf("sentence 0 = %+v", got[0])
	}
	page, _ := st.ListSentences(ctx, l.ID, 1, 1)
	if len(page) != 1 || page[0].Idx != 1 {
		t.Fatalf("page = %+v", page)
	}

	// repaired / human texts only show up in text via RecomputeText.
	st.SetRepairedText(ctx, l.ID, 0, "hello cache")
	st.SetHumanText(ctx, l.ID, 1, sp("human"))
	s0, _ := st.GetSentence(ctx, l.ID, 0)
	if s0.Text != "hello cash" {
		t.Fatal("text must not change without RecomputeText")
	}
	if err := st.RecomputeText(ctx, l.ID, []int{}); err != nil {
		t.Fatal(err)
	}
	s0, _ = st.GetSentence(ctx, l.ID, 0)
	if s0.Text != "hello cash" {
		t.Fatal("empty idx list must change nothing")
	}
	if err := st.RecomputeText(ctx, l.ID, []int{0}); err != nil {
		t.Fatal(err)
	}
	s0, _ = st.GetSentence(ctx, l.ID, 0)
	s1, _ := st.GetSentence(ctx, l.ID, 1)
	if s0.Text != "hello cache" || s1.Text != "second" {
		t.Fatalf("partial recompute: %q %q", s0.Text, s1.Text)
	}
	st.RecomputeText(ctx, l.ID, nil)
	s1, _ = st.GetSentence(ctx, l.ID, 1)
	if s1.Text != "human" {
		t.Fatalf("human_text must win: %q", s1.Text)
	}

	st.ResetRepairedText(ctx, l.ID)
	st.RecomputeText(ctx, l.ID, nil)
	s0, _ = st.GetSentence(ctx, l.ID, 0)
	if s0.RepairedText != s0.ASRText || s0.Text != "hello cash" {
		t.Fatalf("reset repaired: %+v", s0)
	}

	if err := st.SetTranslation(ctx, l.ID, 0, SentenceTr{Zh: sp("你好"), ZhStatus: model.ZhOK, ZhHash: sp("h")}); err != nil {
		t.Fatal(err)
	}
	st.SetTranslation(ctx, l.ID, 1, SentenceTr{Zh: sp("x"), ZhStatus: model.ZhQAFailed, QAError: sp("numbers")})
	if err := st.SetZhStatus(ctx, l.ID, []int{0}, model.ZhStale); err != nil {
		t.Fatal(err)
	}
	counts, _ := st.CountZh(ctx, l.ID)
	if counts[model.ZhStale] != 1 || counts[model.ZhQAFailed] != 1 || counts[model.ZhNone] != 1 || counts[model.ZhOK] != 0 {
		t.Fatalf("counts = %v", counts)
	}
	st.ClearTranslations(ctx, l.ID)
	s1, _ = st.GetSentence(ctx, l.ID, 1)
	if s1.Zh != nil || s1.ZhStatus != model.ZhNone || s1.ZhHash != nil || s1.QAError != nil {
		t.Fatalf("clear translations: %+v", s1)
	}
	if n, _ := st.CountSentences(ctx, l.ID); n != 3 {
		t.Fatal("count")
	}
	if _, err := st.GetSentence(ctx, l.ID, 9); !errors.Is(err, ErrNotFound) {
		t.Fatal("want ErrNotFound")
	}
	st.DeleteSentences(ctx, l.ID)
	if n, _ := st.CountSentences(ctx, l.ID); n != 0 {
		t.Fatal("delete sentences")
	}
}

func TestPatchesSummaryTermsKV(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	l := mkLecture(t, st, c.ID)

	ps := []model.Patch{
		{LectureID: l.ID, SentenceIdx: 3, Before: "a", After: "b", Class: model.ClassDecide, Review: model.ReviewPending},
		{LectureID: l.ID, SentenceIdx: 1, Before: "c", After: "d", Class: model.ClassAuto, Review: model.ReviewNone},
	}
	if err := st.InsertPatches(ctx, ps); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ListPatches(ctx, l.ID, "")
	if len(all) != 2 || all[0].SentenceIdx != 1 || all[0].ID == "" {
		t.Fatalf("patches = %+v", all)
	}
	pend, _ := st.ListPatches(ctx, l.ID, model.ReviewPending)
	if len(pend) != 1 || pend[0].SentenceIdx != 3 {
		t.Fatalf("pending = %+v", pend)
	}
	if err := st.SetPatchReview(ctx, pend[0].ID, model.ReviewEdited, sp("e")); err != nil {
		t.Fatal(err)
	}
	p, _ := st.PatchForSentence(ctx, l.ID, 3)
	if p.Review != model.ReviewEdited || p.EditedText == nil || *p.EditedText != "e" {
		t.Fatalf("patch = %+v", p)
	}
	if _, err := st.PatchForSentence(ctx, l.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("want ErrNotFound")
	}
	if n, _ := st.CountPatches(ctx, l.ID, model.ReviewPending); n != 0 {
		t.Fatal("count pending")
	}
	st.DeletePatches(ctx, l.ID)
	if n, _ := st.CountPatches(ctx, l.ID, ""); n != 0 {
		t.Fatal("delete patches")
	}

	st.UpsertSummary(ctx, model.Summary{LectureID: l.ID, Text: "s1", InputHash: "h1"})
	st.UpsertSummary(ctx, model.Summary{LectureID: l.ID, Text: "s2", InputHash: "h2"})
	sm, err := st.GetSummary(ctx, l.ID)
	if err != nil || sm.Text != "s2" {
		t.Fatalf("summary %+v %v", sm, err)
	}
	st.DeleteSummary(ctx, l.ID)
	if _, err := st.GetSummary(ctx, l.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("summary not deleted")
	}

	tm := model.Term{ID: NewID(), CourseID: c.ID, En: "  Cache   Line ", Source: model.SourceManual, Enabled: true}
	if err := st.InsertTerm(ctx, tm); err != nil {
		t.Fatal(err)
	}
	dup := model.Term{ID: NewID(), CourseID: c.ID, En: "cache line", Source: model.SourceMaterial}
	if err := st.InsertTerm(ctx, dup); err == nil {
		t.Fatal("en_key must be unique per course")
	}
	got, _ := st.GetTerm(ctx, tm.ID)
	if got.EnKey != "cache line" || !got.Enabled {
		t.Fatalf("term = %+v", got)
	}
	got.Zh, got.Enabled = "缓存行", false
	st.UpdateTerm(ctx, got)
	st.SetTermFreq(ctx, got.ID, 7)
	terms, _ := st.ListTerms(ctx, c.ID)
	if len(terms) != 1 || terms[0].Zh != "缓存行" || terms[0].Enabled || terms[0].Freq != 7 {
		t.Fatalf("terms = %+v", terms)
	}
	st.DeleteTerm(ctx, got.ID)
	if _, err := st.GetTerm(ctx, got.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("term not deleted")
	}

	v, err := st.BumpGlossaryVersion(ctx, c.ID)
	if err != nil || v != 1 {
		t.Fatalf("bump: %d %v", v, err)
	}
	v, _ = st.BumpGlossaryVersion(ctx, c.ID)
	if v != 2 {
		t.Fatal("bump 2")
	}
	if _, err := st.BumpGlossaryVersion(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal("bump unknown course")
	}

	if _, ok, _ := st.GetKV(ctx, "k"); ok {
		t.Fatal("kv should be empty")
	}
	st.SetKV(ctx, "k", "1")
	st.SetKV(ctx, "k", "2")
	if v, ok, _ := st.GetKV(ctx, "k"); !ok || v != "2" {
		t.Fatal("kv upsert")
	}

	if err := st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c.ID, Filename: "s.pdf", Path: "p"}); err != nil {
		t.Fatal(err)
	}
	st.SetSyllabusText(ctx, c.ID, "syllabus text")
	sy, _ := st.GetSyllabus(ctx, c.ID)
	if sy.Text == nil || *sy.Text != "syllabus text" {
		t.Fatalf("syllabus %+v", sy)
	}
	st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c.ID, Filename: "s2.md", Path: "p2"})
	sy, _ = st.GetSyllabus(ctx, c.ID)
	if sy.Text != nil || sy.Filename != "s2.md" {
		t.Fatalf("upsert must reset text: %+v", sy)
	}
}

func TestJobCASAndQueries(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	l1 := mkLecture(t, st, c.ID)
	l2 := mkLecture(t, st, c.ID)

	prep1, err := st.GetCurrentJob(ctx, model.ScopeLecture, l1.ID, model.StagePrep)
	if err != nil {
		t.Fatal(err)
	}
	attempt := 1
	ok, err := st.CASJobStatus(ctx, prep1.ID, model.StatusPending, model.StatusQueued, JobFinish{Attempt: &attempt, Error: sp("boom")})
	if err != nil || !ok {
		t.Fatalf("cas: %v %v", ok, err)
	}
	ok, _ = st.CASJobStatus(ctx, prep1.ID, model.StatusPending, model.StatusQueued, JobFinish{})
	if ok {
		t.Fatal("CAS from a stale status must fail")
	}
	j, _ := st.GetJob(ctx, prep1.ID)
	if j.Status != model.StatusQueued || j.Attempt != 1 || j.Error == nil || *j.Error != "boom" {
		t.Fatalf("job = %+v", j)
	}
	now := Now()
	ok, _ = st.CASJobStatus(ctx, prep1.ID, model.StatusQueued, model.StatusRunning, JobFinish{Error: sp(""), StartedAt: &now, FinishedAt: sp("")})
	j, _ = st.GetJob(ctx, prep1.ID)
	if !ok || j.Error != nil || j.StartedAt == nil || j.FinishedAt != nil {
		t.Fatalf("clear error: %+v", j)
	}

	// Retired jobs cannot be CASed.
	prep2, _ := st.GetCurrentJob(ctx, model.ScopeLecture, l2.ID, model.StagePrep)
	if err := st.RetireJob(ctx, prep2.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.CASJobStatus(ctx, prep2.ID, model.StatusPending, model.StatusQueued, JobFinish{}); ok {
		t.Fatal("retired job CASed")
	}
	if _, err := st.GetCurrentJob(ctx, model.ScopeLecture, l2.ID, model.StagePrep); !errors.Is(err, ErrNotFound) {
		t.Fatal("retired job still current")
	}

	// FIFO by lecture creation.
	pend, err := st.ListCurrentJobsByStatus(ctx, model.StatusPending, model.StatusRunning)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 6+5 {
		t.Fatalf("want 11 jobs, got %d", len(pend))
	}
	if pend[0].ScopeID != l1.ID || pend[0].Stage != model.StagePrep || pend[len(pend)-1].ScopeID != l2.ID {
		t.Fatalf("order: first=%+v last=%+v", pend[0], pend[len(pend)-1])
	}

	stats := model.JobStats{PromptTokens: 10, CompletionTokens: 5, Note: "chunk 2 failed"}
	if err := st.SetJobStats(ctx, prep1.ID, stats); err != nil {
		t.Fatal(err)
	}
	st.SetJobProgress(ctx, prep1.ID, 0.5)
	st.SetJobInputHash(ctx, prep1.ID, "abc")
	st.SetJobError(ctx, prep1.ID, sp("err"))
	j, _ = st.GetJob(ctx, prep1.ID)
	if !reflect.DeepEqual(j.Stats, stats) || j.Progress != 0.5 || j.InputHash == nil || *j.InputHash != "abc" || *j.Error != "err" {
		t.Fatalf("job = %+v", j)
	}

	fin := Now()
	st.CASJobStatus(ctx, prep1.ID, model.StatusRunning, model.StatusDone, JobFinish{FinishedAt: &fin})
	got, _ := st.ListJobsFinishedSince(ctx, "2000-01-01T00:00:00Z")
	if len(got) != 1 || got[0].ID != prep1.ID {
		t.Fatalf("finished since = %+v", got)
	}
	got, _ = st.ListJobsFinishedSince(ctx, "2999-01-01T00:00:00Z")
	if len(got) != 0 {
		t.Fatal("future since")
	}

	if err := st.DeleteJobs(ctx, model.ScopeLecture, l2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetJob(ctx, prep2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("DeleteJobs must delete retired jobs too")
	}
}

func TestTxNestingAndRollback(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	boom := errors.New("boom")
	err := st.Tx(ctx, func(tx Store) error {
		if err := tx.CreateCourse(ctx, model.Course{ID: "c1", Name: "a"}); err != nil {
			return err
		}
		// Nested Tx reuses the outer transaction (no deadlock with MaxOpenConns=1).
		if err := tx.Tx(ctx, func(tx2 Store) error {
			return tx2.CreateCourse(ctx, model.Course{ID: "c2", Name: "b"})
		}); err != nil {
			return err
		}
		if _, err := tx.GetCourse(ctx, "c2"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	cs, _ := st.ListCourses(ctx)
	if len(cs) != 0 {
		t.Fatalf("rollback failed: %+v", cs)
	}
	if err := st.Tx(ctx, func(tx Store) error {
		return tx.CreateCourse(ctx, model.Course{ID: "c3", Name: "c"})
	}); err != nil {
		t.Fatal(err)
	}
	cs, _ = st.ListCourses(ctx)
	if len(cs) != 1 || cs[0].ID != "c3" {
		t.Fatalf("commit failed: %+v", cs)
	}

	func() {
		defer func() { recover() }()
		st.Tx(ctx, func(tx Store) error {
			tx.CreateCourse(ctx, model.Course{ID: "c4", Name: "d"})
			panic("x")
		})
	}()
	if _, err := st.GetCourse(ctx, "c4"); !errors.Is(err, ErrNotFound) {
		t.Fatal("panic must roll back")
	}
	// The connection is usable after the panic.
	if _, err := st.ListCourses(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEnKeyAndIDs(t *testing.T) {
	if EnKey("  Translation  Lookaside\tBuffer ") != "translation lookaside buffer" {
		t.Fatal("EnKey")
	}
	a, b := NewID(), NewID()
	if len(a) != 26 || a >= b {
		t.Fatalf("ids not monotonic: %s %s", a, b)
	}
}
