// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

// Each CourseReader method gives what the per-course Store method gives,
// course by course (also across id chunks), and BatchCurrentJobs the
// current jobs of ListCurrentJobsByStatus in the same order within a scope.
func TestCourseReaderMatchesStore(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	r, ok := st.(CourseReader)
	if !ok {
		t.Fatal("the SQLite store does not implement CourseReader")
	}
	var ids []string
	var lecIDs, matIDs []string
	for i := 0; i < 4; i++ {
		c := mkCourse(t, st)
		ids = append(ids, c.ID)
		if i == 0 {
			continue // a course without anything
		}
		for k := 0; k < i; k++ {
			l := mkLecture(t, st, c.ID)
			lecIDs = append(lecIDs, l.ID)
		}
		lid := lecIDs[len(lecIDs)-1]
		for k, m := range []model.Material{
			{Filename: "a.pdf", PagesMD: []string{"p1", ""}},
			{Filename: "b.pdf", LectureID: &lid},
			{Filename: "c.pdf", PagesMD: []string{}},
		} {
			m.ID, m.CourseID, m.Path, m.CreatedAt = NewID(), c.ID, m.Filename, fmt.Sprintf("2026-11-0%dT00:00:0%dZ", i, k)
			if err := st.CreateMaterial(ctx, m); err != nil {
				t.Fatal(err)
			}
			matIDs = append(matIDs, m.ID)
		}
		if i%2 == 1 {
			text := fmt.Sprintf("syllabus %d — 大纲", i)
			if err := st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c.ID, Filename: "s.pdf", Path: "s.pdf", Text: &text}); err != nil {
				t.Fatal(err)
			}
		}
		if i == 2 {
			if err := st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c.ID, Filename: "untext.pdf", Path: "u.pdf"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.InsertJob(ctx, model.Job{ID: NewID(), Scope: model.ScopeCourse, ScopeID: c.ID, Stage: model.StageGlossarySeed,
			Status: model.StatusDone, IsCurrent: true}); err != nil {
			t.Fatal(err)
		}
	}
	// a retired job is not current
	j, _ := st.GetCurrentJob(ctx, model.ScopeLecture, lecIDs[0], model.StagePrep)
	if err := st.RetireJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, "k:1", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, "k:2", ""); err != nil {
		t.Fatal(err)
	}

	lecs, err := r.BatchLectures(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	mats, err := r.BatchMaterials(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	syl, err := r.BatchSyllabi(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		want, _ := st.ListLectures(ctx, id)
		if !reflect.DeepEqual(lecs[id], want) {
			t.Errorf("course %s lectures: batch %+v, store %+v", id, lecs[id], want)
		}
		wm, _ := st.ListMaterials(ctx, id)
		if !reflect.DeepEqual(mats[id], wm) {
			t.Errorf("course %s materials: batch %+v, store %+v", id, mats[id], wm)
		}
		ws, err := st.GetSyllabus(ctx, id)
		gs, has := syl[id]
		if (err == nil) != has || (has && !reflect.DeepEqual(gs, ws)) {
			t.Errorf("course %s syllabus: batch %+v (%v), store %+v (%v)", id, gs, has, ws, err)
		}
	}
	kv, err := r.BatchKV(ctx, []string{"k:1", "k:2", "k:missing"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k:1", "k:2", "k:missing"} {
		v, ok, _ := st.GetKV(ctx, k)
		gv, gok := kv[k]
		if ok != gok || v != gv {
			t.Errorf("kv %s: batch %q %v, store %q %v", k, gv, gok, v, ok)
		}
	}

	all, err := st.ListCurrentJobsByStatus(ctx, model.StatusPending, model.StatusQueued, model.StatusRunning,
		model.StatusDone, model.StatusFailed, model.StatusCancelled, model.StatusStale)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		scope string
		ids   []string
	}{{model.ScopeLecture, lecIDs}, {model.ScopeMaterial, matIDs}, {model.ScopeCourse, ids}} {
		got, err := r.BatchCurrentJobs(ctx, tc.scope, tc.ids)
		if err != nil {
			t.Fatal(err)
		}
		byScope := func(jobs []model.Job) map[string][]model.Job {
			m := map[string][]model.Job{}
			for _, j := range jobs {
				if j.Scope == tc.scope {
					m[j.ScopeID] = append(m[j.ScopeID], j)
				}
			}
			return m
		}
		if g, w := byScope(got), byScope(all); !reflect.DeepEqual(g, w) {
			t.Errorf("%s jobs differ:\nbatch %+v\nall   %+v", tc.scope, g, w)
		}
		if len(got) == 0 {
			t.Errorf("%s: no jobs", tc.scope)
		}
	}

	// More ids than one chunk: the results are joined.
	many := make([]string, 0, countChunk+5)
	for i := 0; i < countChunk+5; i++ {
		many = append(many, fmt.Sprintf("none-%d", i))
	}
	many = append(many, ids...)
	if l2, err := r.BatchLectures(ctx, many); err != nil || !reflect.DeepEqual(l2, lecs) {
		t.Errorf("chunked lectures: %v %v", len(l2), err)
	}
	if j2, err := r.BatchCurrentJobs(ctx, model.ScopeLecture, append(many, lecIDs...)); err != nil || len(j2) == 0 {
		t.Errorf("chunked jobs: %d %v", len(j2), err)
	}
}

// ListJobHistory returns every job of one scope, the retired ones included,
// oldest first, with their stats; other scopes' jobs are not included. It
// works on a transaction's store too.
func TestListJobHistory(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	l1 := mkLecture(t, st, c.ID)
	l2 := mkLecture(t, st, c.ID)

	h, ok := st.(JobHistory)
	if !ok {
		t.Fatal("the SQLite store does not implement JobHistory")
	}
	old, err := st.GetCurrentJob(ctx, model.ScopeLecture, l1.ID, model.StageTranslate)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobStats(ctx, old.ID, model.JobStats{PromptTokens: 1000, CompletionTokens: 200, EstimatedCost: 0.05}); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireJob(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	repl := model.Job{ID: NewID(), Scope: model.ScopeLecture, ScopeID: l1.ID, Stage: model.StageTranslate, Status: model.StatusPending,
		IsCurrent: true, Stats: model.JobStats{PromptTokens: 10}, CreatedAt: "2999-01-01T00:00:00Z"}
	if err := st.InsertJob(ctx, repl); err != nil {
		t.Fatal(err)
	}
	// another lecture's jobs, current and retired, are not part of l1's history
	other, _ := st.GetCurrentJob(ctx, model.ScopeLecture, l2.ID, model.StageTranslate)
	if err := st.RetireJob(ctx, other.ID); err != nil {
		t.Fatal(err)
	}

	got, err := h.ListJobHistory(ctx, model.ScopeLecture, l1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(model.LectureStages)+1 {
		t.Fatalf("history has %d jobs, want %d (the current ones and the retired translate)", len(got), len(model.LectureStages)+1)
	}
	var sawOld, sawRepl bool
	for i, j := range got {
		if j.Scope != model.ScopeLecture || j.ScopeID != l1.ID {
			t.Fatalf("job %d belongs to %s/%s", i, j.Scope, j.ScopeID)
		}
		if i > 0 && (got[i-1].CreatedAt > j.CreatedAt || (got[i-1].CreatedAt == j.CreatedAt && got[i-1].ID > j.ID)) {
			t.Fatalf("not oldest first at %d: %s %s after %s %s", i, j.CreatedAt, j.ID, got[i-1].CreatedAt, got[i-1].ID)
		}
		switch j.ID {
		case old.ID:
			sawOld = !j.IsCurrent && j.Stats.PromptTokens == 1000 && j.Stats.CompletionTokens == 200 && j.Stats.EstimatedCost == 0.05
		case repl.ID:
			sawRepl = j.IsCurrent && j.Stats.PromptTokens == 10
		}
	}
	if !sawOld || !sawRepl {
		t.Fatalf("retired job read %v, replacement read %v: %+v", sawOld, sawRepl, got)
	}
	if got[len(got)-1].ID != repl.ID {
		t.Fatalf("the newest job is %s, want the replacement", got[len(got)-1].ID)
	}

	none, err := h.ListJobHistory(ctx, model.ScopeLecture, "nope")
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown scope id: %v %v", none, err)
	}

	if err := st.Tx(ctx, func(tx Store) error {
		th, ok := tx.(JobHistory)
		if !ok {
			t.Fatal("a transaction's store does not implement JobHistory")
		}
		in, err := th.ListJobHistory(ctx, model.ScopeLecture, l1.ID)
		if err != nil || len(in) != len(got) {
			t.Fatalf("in a transaction: %d jobs, %v", len(in), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
