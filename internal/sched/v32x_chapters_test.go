// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// v3.2.x P3 (Q16a): an export that completes queues the chapters task when
// auto_chapters is on, the chapters role is configured and the lecture has
// no chapters yet.

// chaptersJob is the id of the lecture's current chapters job ("" = none).
func (e *env) chaptersJob(lectureID string) string {
	e.t.Helper()
	if !e.hasJob(model.ScopeLecture, lectureID, model.StageChapters) {
		return ""
	}
	return e.lj(lectureID, model.StageChapters).ID
}

func (e *env) setAutoChapters(on bool) {
	e.t.Helper()
	g := e.s.set.General()
	g.AutoChapters = &on
	if err := e.s.set.SetGeneral(e.ctx, g); err != nil {
		e.t.Fatal(err)
	}
}

func TestExportDoneQueuesChapters(t *testing.T) {
	var chapters atomic.Int32
	chap := &fakeStage{name: model.StageChapters, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		chapters.Add(1)
		return nil
	}}
	e := newEnv(t, nil, instant(model.StageExport, ResourceCPU), chap)
	p := e.provider("chapters", 2)
	e.assign(p.ID, "chapters-model", model.StageChapters)
	l := e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate)
	e.start()
	export1 := e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	e.waitFor("chapters queued and run", 5*time.Second, func() bool {
		return e.hasJob(model.ScopeLecture, l.ID, model.StageChapters) && e.lj(l.ID, model.StageChapters).Status == model.StatusDone
	})
	first := e.lj(l.ID, model.StageChapters)
	if chapters.Load() != 1 {
		t.Fatalf("chapters runs = %d", chapters.Load())
	}
	// A later export (e.g. after an incremental retranslation) queues none:
	// the lecture already had its chapters task (this fake writes no kv).
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageExport, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	e.waitFor("second export", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageExport)
		return j.ID != export1.ID && j.Status == model.StatusDone
	})
	e.s.Stop() // waits for the second export's completion, including the queueing
	if got := e.chaptersJob(l.ID); got != first.ID || chapters.Load() != 1 {
		t.Fatalf("a second export queued chapters again: %s (first %s), runs %d", got, first.ID, chapters.Load())
	}
}

func TestExportDoneChaptersNotQueued(t *testing.T) {
	e := newEnv(t, nil)
	queue := func(l model.Lecture) string {
		e.s.queueChaptersAfterExport(l.ID, e.lj(l.ID, model.StageExport).ID)
		return e.chaptersJob(l.ID)
	}
	// The chapters role is not configured.
	if id := queue(e.lecture()); id != "" {
		t.Fatal("queued without a chapters role")
	}
	p := e.provider("chapters", 2)
	e.assign(p.ID, "chapters-model", model.StageChapters)
	l := e.lecture()
	first := queue(l)
	if first == "" {
		t.Fatal("configured, on by default: not queued")
	}
	if id := queue(l); id != first { // already waiting
		t.Fatalf("queued twice: %s, %s", first, id)
	}
	// Chapters already there, also when out of date.
	l = e.lecture()
	if err := e.st.SetKV(e.ctx, model.KVChaptersPrefix+l.ID, `{"chapters":[],"input_hash":"old"}`); err != nil {
		t.Fatal(err)
	}
	if id := queue(l); id != "" {
		t.Fatal("queued although the lecture has chapters")
	}
	// A chapters task that failed earlier: not queued again automatically.
	l = e.lecture()
	first = queue(l)
	e.drive(first, db.JobFinish{}, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	if id := queue(l); id != first || e.lj(l.ID, model.StageChapters).Status != model.StatusFailed {
		t.Fatalf("queued again after a failed chapters task: %s (first %s)", id, first)
	}
	// Switched off.
	e.setAutoChapters(false)
	if id := queue(e.lecture()); id != "" {
		t.Fatal("queued with auto_chapters off")
	}
	e.setAutoChapters(true)
	if id := queue(e.lecture()); id == "" {
		t.Fatal("not queued with auto_chapters on again")
	}
}

// reexport reruns the lecture's export and waits until the new export job
// is done, then stops the scheduler: Stop waits for the run, so whatever
// its completion queued is in the database (no sleep).
func (e *env) reexport(lectureID string) {
	e.t.Helper()
	prev := e.lj(lectureID, model.StageExport).ID
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, lectureID, model.StageExport, model.ModelChoiceSnapshot); err != nil {
		e.t.Fatal(err)
	}
	e.waitFor("the export again", 5*time.Second, func() bool {
		j := e.lj(lectureID, model.StageExport)
		return j.ID != prev && j.Status == model.StatusDone
	})
	e.s.Stop()
}

// A lecture exported before the upgrade, without chapters or a chapters
// task, is not backfilled when it is exported again (review R2 issue 1).
func TestOldLectureExportAgainNoChapters(t *testing.T) {
	e := newEnv(t, nil, instant(model.StageExport, ResourceCPU), instant(model.StageChapters, ResourceAPI))
	p := e.provider("chapters", 2)
	e.assign(p.ID, "chapters-model", model.StageChapters)
	l := e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport)
	e.start()
	e.reexport(l.ID)
	if id := e.chaptersJob(l.ID); id != "" {
		t.Fatalf("an old lecture got chapters on its export again: %s", id)
	}
	// Also when the earlier export had gone stale before the rerun.
	l = e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport)
	e.drive(e.lj(l.ID, model.StageExport).ID, db.JobFinish{}, model.StatusStale)
	e.s = e.newScheduler(instant(model.StageExport, ResourceCPU), instant(model.StageChapters, ResourceAPI))
	e.start()
	e.reexport(l.ID)
	if id := e.chaptersJob(l.ID); id != "" {
		t.Fatalf("queued after a stale earlier export: %s", id)
	}
}

// Only the first export counts: when it completed while the chapters role
// was not configured, configuring the role later does not queue chapters
// on the next export.
func TestChaptersOnlyAfterFirstExport(t *testing.T) {
	stages := func() []Stage {
		return []Stage{instant(model.StageExport, ResourceCPU), instant(model.StageChapters, ResourceAPI)}
	}
	e := newEnv(t, nil, stages()...)
	l := e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate)
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	e.s.Stop()
	if id := e.chaptersJob(l.ID); id != "" {
		t.Fatalf("queued without a chapters role: %s", id)
	}
	p := e.provider("chapters", 2)
	e.assign(p.ID, "chapters-model", model.StageChapters)
	e.s = e.newScheduler(stages()...)
	e.start()
	e.reexport(l.ID)
	if id := e.chaptersJob(l.ID); id != "" {
		t.Fatalf("queued on a later export: %s", id)
	}
	// A new lecture's first export, with the role configured: queued once.
	l = e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate)
	e.s = e.newScheduler(stages()...)
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	e.s.Stop()
	if id := e.chaptersJob(l.ID); id == "" {
		t.Fatal("a new lecture's first export did not queue chapters")
	}
}

// ---------- round 2 N39: out-of-date chapters are refreshed ----------

// writeChapters stores chapters for the lecture with the current input
// hash (as the chapters stage does), or with hash when it is not "".
func (e *env) writeChapters(lectureID, hash string) {
	e.t.Helper()
	if hash == "" {
		h, err := chaptersInputHash(e.ctx, e.st, lectureID)
		if err != nil {
			e.t.Fatal(err)
		}
		hash = h
	}
	b, err := json.Marshal(model.Chapters{InputHash: hash})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.SetKV(e.ctx, model.KVChaptersPrefix+lectureID, string(b)); err != nil {
		e.t.Fatal(err)
	}
}

// doneAt drives the job to done with started_at and finished_at at.
func (e *env) doneAt(jobID string, at time.Time) {
	e.t.Helper()
	e.ranAt(jobID, at, at)
}

// ranAt drives the job to done with started_at start and finished_at end.
func (e *env) ranAt(jobID string, start, end time.Time) {
	e.t.Helper()
	s, f := db.FormatTime(start), db.FormatTime(end)
	e.drive(jobID, db.JobFinish{StartedAt: &s, FinishedAt: &f}, model.StatusQueued, model.StatusRunning, model.StatusDone)
}

// After a summary rerun changed the chapters' input, the next export
// queues the chapters task again, once. Chapters that are up to date, and
// chapters out of date only because sentences were edited by hand, are not
// regenerated (review R26 A-1).
func TestOutdatedChaptersRequeuedOnExport(t *testing.T) {
	var runs atomic.Int32
	var e *env
	stages := func() []Stage {
		return []Stage{instant(model.StageSummary, ResourceAPI), instant(model.StageRepair, ResourceAPI),
			instant(model.StageTranslate, ResourceAPI), instant(model.StageExport, ResourceCPU),
			&fakeStage{name: model.StageChapters, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
				runs.Add(1)
				e.writeChapters(id, "")
				return nil
			}}}
	}
	e = newEnv(t, nil, stages()...)
	p := e.provider("chapters", 2)
	e.assign(p.ID, "chapters-model", model.StageChapters)
	l := e.lecture()
	zh := "中文"
	var ss []model.Sentence
	for i := 0; i < 3; i++ {
		ss = append(ss, model.Sentence{Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i+1) * 1000, ASRText: "s", RepairedText: "s",
			Zh: &zh, ZhStatus: model.ZhOK})
	}
	if err := e.st.InsertSentences(e.ctx, l.ID, ss); err != nil {
		t.Fatal(err)
	}
	if err := e.st.RecomputeText(e.ctx, l.ID, []int{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertSummary(e.ctx, model.Summary{LectureID: l.ID, Text: "the summary"}); err != nil {
		t.Fatal(err)
	}
	// The pipeline finished two hours ago, the chapters one hour ago.
	now := time.Now()
	for _, st := range []string{model.StagePrep, model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate} {
		e.doneAt(e.lj(l.ID, st).ID, now.Add(-2*time.Hour))
	}
	if _, err := state.EnqueueAux(e.ctx, e.st, model.ScopeLecture, l.ID, model.StageChapters); err != nil {
		t.Fatal(err)
	}
	first := e.chaptersJob(l.ID)
	e.doneAt(first, now.Add(-time.Hour))
	e.writeChapters(l.ID, "")
	// Up to date: the export queues none.
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	e.s.Stop()
	if got := e.chaptersJob(l.ID); got != first {
		t.Fatalf("up-to-date chapters queued again: %s (first %s)", got, first)
	}
	// A sentence edited by hand: out of date, but the summary was not
	// regenerated, so the export queues none.
	human := "edited by hand"
	if err := e.st.SetHumanText(e.ctx, l.ID, 1, &human); err != nil {
		t.Fatal(err)
	}
	if err := e.st.RecomputeText(e.ctx, l.ID, []int{1}); err != nil {
		t.Fatal(err)
	}
	// Its translation is out of date: the incremental retranslation and its
	// export run when the scheduler starts.
	if err := e.st.SetZhStatus(e.ctx, l.ID, []int{1}, model.ZhStale); err != nil {
		t.Fatal(err)
	}
	prevExport := e.lj(l.ID, model.StageExport).ID
	e.s = e.newScheduler(stages()...)
	e.start()
	e.waitFor("the export after the hand edit", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageExport)
		return j.ID != prevExport && j.Status == model.StatusDone
	})
	e.s.Stop()
	if got := e.chaptersJob(l.ID); got != first || runs.Load() != 0 {
		t.Fatalf("a hand edit queued the chapters: %s (first %s), runs %d", got, first, runs.Load())
	}
	// The summary task reran (as after a rerun of the transcription) and
	// the export after it completed: queued again.
	// (The fake translate left the translation stale; mark it translated so
	// that no incremental retranslation starts with the scheduler.)
	if err := e.st.SetZhStatus(e.ctx, l.ID, []int{1}, model.ZhOK); err != nil {
		t.Fatal(err)
	}
	e.s = e.newScheduler(stages()...)
	e.start()
	prevExport = e.lj(l.ID, model.StageExport).ID
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	// The scheduler keeps running: the new chapters task is picked up as
	// soon as it is queued, so wait for it to be done (review R26 A-5).
	e.waitFor("the export after the summary rerun", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageExport)
		return j.ID != prevExport && j.Status == model.StatusDone
	})
	e.waitFor("the refreshed chapters", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageChapters)
		return j.ID != first && j.Status == model.StatusDone
	})
	e.s.Stop() // waits for any run still going, with what its completion queues
	second := e.lj(l.ID, model.StageChapters)
	if runs.Load() != 1 {
		t.Fatalf("chapters runs after the summary rerun: %d", runs.Load())
	}
	summaryJob := e.lj(l.ID, model.StageSummary).ID
	// Chapters refreshed after the summary: the same export completing
	// again queues none (their hash is current).
	e.s.queueChaptersAfterExport(l.ID, e.lj(l.ID, model.StageExport).ID)
	if got := e.chaptersJob(l.ID); got != second.ID {
		t.Fatalf("queued twice: %s, %s", second.ID, got)
	}
	// The next export queues none either.
	// (The repair rerun reset the translations, which the fake translate
	// does not fill: mark them translated, as above, and let any
	// incremental export that started meanwhile finish first.)
	if err := e.st.SetZhStatus(e.ctx, l.ID, []int{0, 1, 2}, model.ZhOK); err != nil {
		t.Fatal(err)
	}
	e.s = e.newScheduler(stages()...)
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	e.reexport(l.ID)
	if got := e.chaptersJob(l.ID); got != second.ID || runs.Load() != 1 {
		t.Fatalf("after the refresh: %s (refresh %s), runs %d", got, second.ID, runs.Load())
	}
	// The refresh queued no other stage again.
	if got := e.lj(l.ID, model.StageSummary).ID; got != summaryJob {
		t.Fatalf("summary job %s, was %s", got, summaryJob)
	}
	for _, st := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		if s := e.lj(l.ID, st).Status; s != model.StatusDone {
			t.Fatalf("%s is %s", st, s)
		}
	}
}

func TestOutdatedChaptersNotRequeued(t *testing.T) {
	e := newEnv(t, nil)
	queue := func(l model.Lecture) string {
		e.s.queueChaptersAfterExport(l.ID, e.lj(l.ID, model.StageExport).ID)
		return e.chaptersJob(l.ID)
	}
	p := e.provider("chapters", 2)
	e.assign(p.ID, "chapters-model", model.StageChapters)
	now := time.Now()
	// lecture is a lecture whose summary finished at summaryAt (left
	// pending when zero) and whose chapters task was queued.
	lecture := func(summaryAt time.Time) (model.Lecture, string) {
		l := e.lecture()
		if !summaryAt.IsZero() {
			e.doneAt(e.lj(l.ID, model.StageSummary).ID, summaryAt)
		}
		return l, queue(l)
	}
	// A chapters task in each blocking status, with out-of-date chapters
	// and a summary that finished later.
	for _, path := range [][]string{
		{},
		{model.StatusQueued},
		{model.StatusQueued, model.StatusRunning},
		{model.StatusQueued, model.StatusRunning, model.StatusFailed},
		{model.StatusQueued, model.StatusCancelled},
		{model.StatusQueued, model.StatusRunning, model.StatusCancelled},
	} {
		l, first := lecture(now)
		e.drive(first, db.JobFinish{}, path...)
		want := e.lj(l.ID, model.StageChapters).Status
		e.writeChapters(l.ID, "old")
		if id := queue(l); id != first || e.lj(l.ID, model.StageChapters).Status != want {
			t.Fatalf("%s: queued again: %s (first %s)", want, id, first)
		}
	}
	// Done an hour ago, out of date, the summary finished since: not with
	// auto_chapters off, queued again with it on (the control case).
	l, first := lecture(now)
	e.doneAt(first, now.Add(-time.Hour))
	e.writeChapters(l.ID, "old")
	e.setAutoChapters(false)
	if id := queue(l); id != first {
		t.Fatal("refreshed with auto_chapters off")
	}
	e.setAutoChapters(true)
	if id := queue(l); id == first || id == "" {
		t.Fatal("out-of-date chapters after a newer summary not queued again")
	}
	// Out of date; the chapters ran from an hour ago until now. Compared is
	// the summary's finished_at with the chapters' started_at, not strictly
	// (review R26 A-6): a summary that finished while the chapters ran, or
	// in the second they started, may not have been seen by them, so the
	// chapters are queued again (the hash decides whether they saw it). A
	// summary that finished before they started, or has not finished, does
	// not queue them.
	for _, c := range []struct {
		what      string
		summaryAt time.Time
		queued    bool
	}{
		{"summary before the chapters started", now.Add(-2 * time.Hour), false},
		{"summary in the second the chapters started", now.Add(-time.Hour), true},
		{"summary while the chapters ran", now.Add(-30 * time.Minute), true},
		{"summary not done", time.Time{}, false},
	} {
		l, first := lecture(c.summaryAt)
		e.ranAt(first, now.Add(-time.Hour), now)
		e.writeChapters(l.ID, "old")
		if id := queue(l); (id != first) != c.queued {
			t.Fatalf("%s: queued again %v, want %v", c.what, id != first, c.queued)
		}
	}
	// Done without chapters, with unreadable chapters or with chapters
	// without a hash (the summary finished later): not out of date.
	for _, kv := range []string{"", "not json", `{"chapters":[]}`} {
		l, first := lecture(now)
		e.doneAt(first, now.Add(-time.Hour))
		if kv != "" {
			if err := e.st.SetKV(e.ctx, model.KVChaptersPrefix+l.ID, kv); err != nil {
				t.Fatal(err)
			}
		}
		if id := queue(l); id != first {
			t.Fatalf("kv %q: queued again", kv)
		}
	}
	// An old lecture with out-of-date chapters but no chapters task, after
	// an earlier export: not backfilled.
	l = e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport)
	e.writeChapters(l.ID, "old")
	if id := queue(l); id != "" {
		t.Fatalf("an old lecture got chapters: %s", id)
	}
}
