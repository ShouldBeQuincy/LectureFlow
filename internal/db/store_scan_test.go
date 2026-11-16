// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"slices"
	"testing"

	"lectureflow/internal/model"
)

// ListScanJobs is ListCurrentJobsByStatus(pending, queued) in the same
// order, without exactly the pending jobs of draft lectures whose stage is
// in draftHeld (performance audit F08).
func TestListScanJobsLeavesOutDraftPendingJobs(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	c := mkCourse(t, st)
	draft1 := mkLecture(t, st, c.ID)
	launched := mkLecture(t, st, c.ID)
	if ok, err := st.SetLectureLaunched(ctx, launched.ID, Now()); err != nil || !ok {
		t.Fatalf("launch: %v %v", ok, err)
	}
	draft2 := mkLecture(t, st, c.ID)

	ins := func(scope, scopeID, stage string) model.Job {
		t.Helper()
		j := model.Job{ID: NewID(), Scope: scope, ScopeID: scopeID, Stage: stage, Status: model.StatusPending,
			IsCurrent: true, CreatedAt: Now()}
		if err := st.InsertJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	// An auxiliary job of a draft, a course job, a lecture job whose
	// lecture row is missing.
	title := ins(model.ScopeLecture, draft1.ID, model.StageTitle)
	seed := ins(model.ScopeCourse, c.ID, model.StageGlossarySeed)
	orphan := ins(model.ScopeLecture, NewID(), model.StagePrep)
	// A queued job of a draft (not reachable through the actions, but the
	// listing must not depend on it): always listed.
	prep2, err := st.GetCurrentJob(ctx, model.ScopeLecture, draft2.ID, model.StagePrep)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.CASJobStatus(ctx, prep2.ID, model.StatusPending, model.StatusQueued, JobFinish{}); err != nil || !ok {
		t.Fatalf("cas: %v %v", ok, err)
	}
	// A finished job of the launched lecture: not pending/queued, never listed.
	prepL, err := st.GetCurrentJob(ctx, model.ScopeLecture, launched.ID, model.StagePrep)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{model.StatusPending, model.StatusQueued}, {model.StatusQueued, model.StatusRunning}, {model.StatusRunning, model.StatusDone}} {
		if ok, err := st.CASJobStatus(ctx, prepL.ID, step[0], step[1], JobFinish{}); err != nil || !ok {
			t.Fatalf("cas %v: %v %v", step, ok, err)
		}
	}

	all, err := st.ListCurrentJobsByStatus(ctx, model.StatusPending, model.StatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(js []model.Job) []string {
		out := []string{}
		for _, j := range js {
			out = append(out, j.ID)
		}
		return out
	}
	drafts := map[string]bool{draft1.ID: true, draft2.ID: true}
	for _, tc := range []struct {
		name string
		held []string
	}{
		{"none", nil},
		{"prep only", []string{model.StagePrep}},
		{"pipeline and aux", append(slices.Clone(model.LectureStages), model.StageTitle, model.StageChapters, model.StageStream)},
	} {
		got, err := st.ListScanJobs(ctx, tc.held)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{}
		for _, j := range all {
			if j.Status == model.StatusPending && j.Scope == model.ScopeLecture && drafts[j.ScopeID] && slices.Contains(tc.held, j.Stage) {
				continue
			}
			want = append(want, j.ID)
		}
		if !slices.Equal(ids(got), want) {
			t.Errorf("%s: got %v, want %v", tc.name, ids(got), want)
		}
		for _, id := range []string{seed.ID, orphan.ID, prep2.ID} {
			if !slices.Contains(ids(got), id) {
				t.Errorf("%s: %s not listed", tc.name, id)
			}
		}
		if slices.Contains(tc.held, model.StageTitle) == slices.Contains(ids(got), title.ID) {
			t.Errorf("%s: the draft's title job listed=%v", tc.name, slices.Contains(ids(got), title.ID))
		}
	}
	// With every stage held: the launched lecture's five pending jobs, the
	// draft's queued prep, the course job and the orphan, nothing else.
	got, _ := st.ListScanJobs(ctx, append(slices.Clone(model.LectureStages), model.StageTitle, model.StageChapters, model.StageStream))
	if len(got) != 5+1+1+1 {
		t.Fatalf("got %d jobs: %+v", len(got), got)
	}
	for _, j := range got {
		if j.Scope == model.ScopeLecture && drafts[j.ScopeID] && j.Status == model.StatusPending {
			t.Errorf("draft pending job listed: %+v", j)
		}
	}
}
