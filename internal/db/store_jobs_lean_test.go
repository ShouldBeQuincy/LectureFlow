// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// r7 perf (P1 report H1): the lean job reads give exactly what their
// callers used to derive from ListCurrentJobsByStatus.

var leanAllStatuses = []string{model.StatusPending, model.StatusQueued, model.StatusRunning,
	model.StatusDone, model.StatusFailed, model.StatusCancelled, model.StatusStale}

// refDoneByFinish is DoneLectureJobsByFinish the way devices.FindClipSource
// derived it before: every done current job, the lecture jobs of stage,
// sorted by finished_at descending (nil as ""), then scope id.
func refDoneByFinish(t *testing.T, st Store, stage string) []FinishedJob {
	t.Helper()
	jobs, err := st.ListCurrentJobsByStatus(context.Background(), model.StatusDone)
	if err != nil {
		t.Fatal(err)
	}
	out := []FinishedJob{}
	for _, j := range jobs {
		if j.Scope != model.ScopeLecture || j.Stage != stage {
			continue
		}
		f := ""
		if j.FinishedAt != nil {
			f = *j.FinishedAt
		}
		out = append(out, FinishedJob{ScopeID: j.ScopeID, FinishedAt: f})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].FinishedAt != out[b].FinishedAt {
			return out[a].FinishedAt > out[b].FinishedAt
		}
		return out[a].ScopeID < out[b].ScopeID
	})
	return out
}

// refSnapshotCounts is LectureSnapshotCounts the way the API's snapshotRefs
// read it before: every current job, the lecture jobs of stages, counted by
// their raw snapshot.
func refSnapshotCounts(t *testing.T, st Store, stages ...string) map[string]int {
	t.Helper()
	jobs, err := st.ListCurrentJobsByStatus(context.Background(), leanAllStatuses...)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, j := range jobs {
		if j.Scope == model.ScopeLecture && leanContains(stages, j.Stage) {
			out[j.ConfigSnapshot]++
		}
	}
	return out
}

func leanContains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func checkLean(t *testing.T, st Store, what string) {
	t.Helper()
	ctx := context.Background()
	jr, ok := st.(JobReader)
	if !ok {
		t.Fatal("the SQLite store does not implement JobReader")
	}
	for _, stage := range []string{model.StageRepair, model.StageTranslate, model.StagePrep} {
		got, err := jr.DoneLectureJobsByFinish(ctx, stage)
		if err != nil {
			t.Fatal(err)
		}
		if want := refDoneByFinish(t, st, stage); !reflect.DeepEqual(leanNonNil(got), want) {
			t.Errorf("%s: DoneLectureJobsByFinish(%s)\n got %v\nwant %v", what, stage, got, want)
		}
	}
	for _, stages := range [][]string{
		{model.StageSummary, model.StageRepair, model.StageTranslate},
		{model.StageRepair},
		{model.StageASR, model.StageExport},
	} {
		got, err := jr.LectureSnapshotCounts(ctx, stages...)
		if err != nil {
			t.Fatal(err)
		}
		if want := refSnapshotCounts(t, st, stages...); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: LectureSnapshotCounts(%v)\n got %v\nwant %v", what, stages, got, want)
		}
	}
}

func leanNonNil(x []FinishedJob) []FinishedJob {
	if x == nil {
		return []FinishedJob{}
	}
	return x
}

func TestLeanJobReadsMatchFullListing(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	checkLean(t, st, "empty database")

	c := mkCourse(t, st)
	// Lectures come with six pending current jobs (CreateLecture); retire
	// them all so only superseded jobs are left.
	var lecs []model.Lecture
	for i := 0; i < 6; i++ {
		lecs = append(lecs, mkLecture(t, st, c.ID))
	}
	retireAll := func() {
		t.Helper()
		for _, l := range lecs {
			js, err := st.ListCurrentJobs(ctx, model.ScopeLecture, l.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, j := range js {
				if err := st.RetireJob(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	retireAll()
	// superseded done jobs with snapshots: never counted
	snap := model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "deepseek", Model: "deepseek-chat"})
	for _, l := range lecs[:3] {
		fin := "2026-10-20T10:00:00Z"
		for _, stage := range []string{model.StageRepair, model.StageSummary} {
			if err := st.InsertJob(ctx, model.Job{ID: NewID(), Scope: model.ScopeLecture, ScopeID: l.ID, Stage: stage,
				Status: model.StatusDone, FinishedAt: &fin, ConfigSnapshot: snap, CreatedAt: Now()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	checkLean(t, st, "only superseded jobs")

	// The order's edge cases, spelled out: NULL and "" finish times tie
	// (both ""), equal times go by lecture id.
	byID := append([]model.Lecture(nil), lecs[:5]...)
	sort.Slice(byID, func(a, b int) bool { return byID[a].ID < byID[b].ID })
	edge := []*string{nil, sp(""), sp("2026-10-15T10:00:00Z"), sp("2026-10-15T10:00:00Z"), sp("2026-10-16T08:00:00Z")}
	for i, l := range byID {
		if err := st.InsertJob(ctx, model.Job{ID: NewID(), Scope: model.ScopeLecture, ScopeID: l.ID, Stage: model.StageRepair,
			Status: model.StatusDone, FinishedAt: edge[i], ConfigSnapshot: snap, IsCurrent: true, CreatedAt: Now()}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.(JobReader).DoneLectureJobsByFinish(ctx, model.StageRepair)
	if err != nil {
		t.Fatal(err)
	}
	want := []FinishedJob{{byID[4].ID, "2026-10-16T08:00:00Z"}, {byID[2].ID, "2026-10-15T10:00:00Z"},
		{byID[3].ID, "2026-10-15T10:00:00Z"}, {byID[0].ID, ""}, {byID[1].ID, ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("edge order\n got %v\nwant %v", got, want)
	}
	checkLean(t, st, "edge cases")

	// Mixed: random statuses, stages, snapshots and finish times (NULL, "",
	// equal values), course and material jobs, a lecture job whose lecture
	// row is missing; several rounds of superseding.
	rng := rand.New(rand.NewSource(7))
	snaps := []string{
		model.EmptySnapshot, "",
		snap,
		model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "claude", Model: "claude-sonnet-4-5"}),
		model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "claude", Model: "claude-sonnet-4-5", PromptVersion: 2}),
		`{"provider_id":"x"}`,           // no model: not usable
		`{"provider_id":"x","model":1}`, // invalid
		`not json`,
	}
	fins := []*string{nil, sp(""), sp("2026-10-01T10:00:00Z"), sp("2026-10-15T10:00:00Z"), sp("2026-10-15T10:00:00Z"),
		sp("2026-10-15T10:00:01Z"), sp("2026-11-01T00:00:00Z")}
	scopes := []string{}
	for _, l := range lecs {
		scopes = append(scopes, l.ID)
	}
	scopes = append(scopes, "zz-missing-lecture", "00-missing-lecture")
	stages := []string{model.StageRepair, model.StageSummary, model.StageTranslate, model.StagePrep, model.StageASR, model.StageExport}
	for round := 0; round < 4; round++ {
		retireAll()
		for _, id := range scopes {
			for _, stage := range stages {
				if rng.Intn(5) == 0 {
					continue // no current job of this stage
				}
				// a missing lecture's jobs stay current across rounds: insert once
				if strings.Contains(id, "missing") && round > 0 {
					continue
				}
				if err := st.InsertJob(ctx, model.Job{ID: NewID(), Scope: model.ScopeLecture, ScopeID: id, Stage: stage,
					Status: leanAllStatuses[rng.Intn(len(leanAllStatuses))], FinishedAt: fins[rng.Intn(len(fins))],
					ConfigSnapshot: snaps[rng.Intn(len(snaps))], IsCurrent: true, CreatedAt: Now()}); err != nil {
					t.Fatal(err)
				}
			}
		}
		checkLean(t, st, fmt.Sprintf("round %d", round))
	}
	// Course and material jobs with done repair-like rows and snapshots
	// never count as lecture jobs.
	fin := "2026-12-01T00:00:00Z"
	for _, scope := range []string{model.ScopeCourse, model.ScopeMaterial} {
		if err := st.InsertJob(ctx, model.Job{ID: NewID(), Scope: scope, ScopeID: c.ID, Stage: model.StageRepair,
			Status: model.StatusDone, FinishedAt: &fin, ConfigSnapshot: snap, IsCurrent: true, CreatedAt: Now()}); err != nil {
			t.Fatal(err)
		}
	}
	checkLean(t, st, "with course and material jobs")
	// Inside a transaction too.
	if err := st.Tx(ctx, func(tx Store) error {
		checkLean(t, tx, "transaction")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The lean reads go through the job_current index (EXPLAIN QUERY PLAN), not
// a scan of every job ever run.
func TestLeanJobReadsUseCurrentJobIndex(t *testing.T) {
	st := openTest(t).(*sqlStore)
	for name, q := range map[string]struct {
		sql  string
		args []any
	}{
		"DoneLectureJobsByFinish": {doneByFinishSQL, []any{model.ScopeLecture, model.StageRepair, model.StatusDone}},
		"LectureSnapshotCounts": {snapshotCountsSQL(3),
			[]any{model.ScopeLecture, model.StageSummary, model.StageRepair, model.StageTranslate}},
	} {
		rows, err := st.q.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q.sql, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		p := strings.Join(plan, " | ")
		t.Logf("%s: %s", name, p)
		if !strings.Contains(p, "USING INDEX job_current") || strings.Contains(p, "SCAN job ") || strings.HasSuffix(p, "SCAN job") {
			t.Errorf("%s: plan %q does not use the job_current index", name, p)
		}
	}
}
