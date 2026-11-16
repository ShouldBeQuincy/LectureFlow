// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// failStageAt walks a lecture stage to running and fails it with finished_at
// at (through state.TransitionJob, like any failure).
func (e *env) failStageAt(lectureID, stage string, at time.Time) {
	e.t.Helper()
	j := e.moveJob(model.ScopeLecture, lectureID, stage, "queued", "running")
	msg := "boom"
	fin := at.UTC().Format(time.RFC3339)
	if err := state.TransitionJob(context.Background(), e.st, j.ID, model.StatusRunning, model.StatusFailed,
		db.JobFinish{Error: &msg, FinishedAt: &fin}); err != nil {
		e.t.Fatal(err)
	}
}

// jsonHasKey reports whether the JSON object b has the top-level key k.
func jsonHasKey(t *testing.T, b []byte, k string) bool {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("not a JSON object: %v; %s", err, b)
	}
	_, ok := top[k]
	return ok
}

// T24 S2: the course counts include the lectures that failed, whenever they
// failed (the dashboard's failure group keeps its 24-hour window). A lecture
// counts once however many of its stages failed; cancelled stages and drafts
// do not count.
func TestCourseFailedCount(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	c := e.course("Operating Systems")
	other := e.course("Networks")

	recent := e.lecture(c.ID, "Failed recently")
	e.failStageAt(recent.ID, model.StagePrep, now.Add(-2*time.Hour))
	old := e.lecture(c.ID, "Failed long ago")
	e.failStageAt(old.ID, model.StagePrep, now.Add(-72*time.Hour))
	twice := e.lecture(c.ID, "Two failed stages")
	e.moveJob(model.ScopeLecture, twice.ID, model.StagePrep, "queued", "running", "done")
	e.moveJob(model.ScopeLecture, twice.ID, model.StageASR, "queued", "running", "done")
	e.failStageAt(twice.ID, model.StageSummary, now.Add(-30*time.Hour))
	e.failStageAt(twice.ID, model.StageRepair, now.Add(-time.Hour))
	cancelled := e.lecture(c.ID, "Cancelled")
	e.moveJob(model.ScopeLecture, cancelled.ID, model.StagePrep, "queued", "cancelled")
	done := e.lecture(c.ID, "Done")
	e.allDone(done.ID)
	e.draft(c.ID, "Draft")
	e.lecture(other.ID, "Other course, running")

	var list []Course
	e.call("GET", "/api/courses", nil, http.StatusOK, &list)
	byID := map[string]Course{}
	for _, x := range list {
		byID[x.ID] = x
	}
	if got := byID[c.ID].FailedCount; got != 3 {
		t.Errorf("GET /api/courses: failed_count = %d, want 3", got)
	}
	if got := byID[other.ID].FailedCount; got != 0 {
		t.Errorf("GET /api/courses: other course failed_count = %d, want 0", got)
	}
	var one Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &one)
	if one.FailedCount != 3 {
		t.Errorf("GET /api/courses/{id}: failed_count = %d, want 3", one.FailedCount)
	}
	var d Dashboard
	e.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	cards := map[string]CourseCard{}
	for _, x := range d.Courses {
		cards[x.ID] = x
	}
	if cards[c.ID].FailedCount != 3 || cards[other.ID].FailedCount != 0 {
		t.Errorf("dashboard course cards: failed_count = %d / %d, want 3 / 0", cards[c.ID].FailedCount, cards[other.ID].FailedCount)
	}
	// The field is always present (0 included), so the page can rely on it.
	_, b := e.do("GET", "/api/courses/"+other.ID, nil)
	if !jsonHasKey(t, b, "failed_count") {
		t.Errorf("failed_count missing at 0: %s", b)
	}
}

// T24 S2: the dashboard keeps its 24-hour failure group and says how many
// lectures failed earlier and are still failed (earlier_failed_total): the
// launched lectures with a failed stage none of which is listed.
func TestDashboardEarlierFailedTotal(t *testing.T) {
	e := newEnv(t)
	var d Dashboard
	_, b := e.do("GET", "/api/dashboard", nil)
	if !jsonHasKey(t, b, "earlier_failed_total") {
		t.Fatalf("earlier_failed_total missing at 0: %s", b)
	}
	now := time.Now()
	c := e.course("Operating Systems")
	other := e.course("Networks")

	recent := e.lecture(c.ID, "Failed recently")
	e.failStageAt(recent.ID, model.StagePrep, now.Add(-2*time.Hour))
	old := e.lecture(c.ID, "Failed long ago")
	e.failStageAt(old.ID, model.StagePrep, now.Add(-72*time.Hour))
	mixed := e.lecture(c.ID, "One old, one recent failure")
	e.moveJob(model.ScopeLecture, mixed.ID, model.StagePrep, "queued", "running", "done")
	e.moveJob(model.ScopeLecture, mixed.ID, model.StageASR, "queued", "running", "done")
	e.failStageAt(mixed.ID, model.StageSummary, now.Add(-30*time.Hour))
	e.failStageAt(mixed.ID, model.StageRepair, now.Add(-time.Hour))
	oldTwice := e.lecture(c.ID, "Two old failures")
	e.moveJob(model.ScopeLecture, oldTwice.ID, model.StagePrep, "queued", "running", "done")
	e.moveJob(model.ScopeLecture, oldTwice.ID, model.StageASR, "queued", "running", "done")
	e.failStageAt(oldTwice.ID, model.StageSummary, now.Add(-30*time.Hour))
	e.failStageAt(oldTwice.ID, model.StageRepair, now.Add(-40*time.Hour))
	cancelled := e.lecture(c.ID, "Cancelled")
	e.moveJob(model.ScopeLecture, cancelled.ID, model.StagePrep, "queued", "cancelled")
	otherOld := e.lecture(other.ID, "Other course, failed long ago")
	e.failStageAt(otherOld.ID, model.StagePrep, now.Add(-50*time.Hour))

	e.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	listed := map[string]bool{}
	lectures := map[string]bool{}
	for _, f := range d.Failures {
		listed[f.LectureID+"/"+f.Stage] = true
		lectures[f.LectureID] = true
	}
	if len(d.Failures) != 2 || !listed[recent.ID+"/"+model.StagePrep] || !listed[mixed.ID+"/"+model.StageRepair] {
		t.Errorf("failures (24 h) = %+v, want the recent prep and the mixed lecture's repair", d.Failures)
	}
	if d.EarlierFailedTotal != 3 {
		t.Errorf("earlier_failed_total = %d, want 3 (old, two old failures, other course)", d.EarlierFailedTotal)
	}
	// Every failed lecture is either listed or counted as earlier.
	failed := 0
	for _, cc := range d.Courses {
		failed += cc.FailedCount
	}
	if failed != len(lectures)+d.EarlierFailedTotal {
		t.Errorf("failed_count sum %d != listed lectures %d + earlier %d", failed, len(lectures), d.EarlierFailedTotal)
	}
}
