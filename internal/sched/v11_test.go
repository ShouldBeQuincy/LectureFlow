// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Tests of the v1.1 scheduler operations (SPEC-v1.1 A2.3, A3, A4.3, A7) and
// the settings wiring (A5). Written before the implementation (user
// guardrail: test first).

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// ---------- helpers ----------

// provider creates a provider with a fake key (never a real one).
func (e *env) provider(name string, maxConc int, prices ...float64) model.Provider {
	e.t.Helper()
	typ, url, key := model.ProviderOpenAI, "https://"+name+".example.test/v1", "sk-test-"+name
	in := settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key, MaxConcurrency: &maxConc}
	if len(prices) == 3 {
		in.PriceInput, in.PriceCacheHit, in.PriceOutput = &prices[0], &prices[1], &prices[2]
	}
	p, err := e.s.set.CreateProvider(e.ctx, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// assign sets the role of every listed stage to providerID/mdl.
func (e *env) assign(providerID, mdl string, stages ...string) {
	e.t.Helper()
	for _, s := range stages {
		if err := e.s.set.SetRole(e.ctx, model.RoleOfStage(s), providerID, mdl, model.RoleParams{}); err != nil {
			e.t.Fatal(err)
		}
	}
}

var llmStages = []string{model.StageSummary, model.StageRepair, model.StageTranslate}

func (e *env) snaps(lectureID string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, s := range model.LectureStages {
		out[s] = e.lj(lectureID, s).ConfigSnapshot
	}
	return out
}

func (e *env) jobIDs(lectureID string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, s := range model.LectureStages {
		out[s] = e.lj(lectureID, s).ID
	}
	return out
}

func (e *env) current() map[string]string {
	e.t.Helper()
	cur, err := e.s.set.CurrentSnapshots()
	if err != nil {
		e.t.Fatal(err)
	}
	return cur
}

func providerOfSnap(raw string) string {
	s, _ := model.ParseLLMSnapshot(raw)
	return s.ProviderID
}

func (e *env) hasChanged(kind, id string) bool {
	e.ev.mu.Lock()
	defer e.ev.mu.Unlock()
	for _, c := range e.ev.changed {
		if c == kind+":"+id {
			return true
		}
	}
	return false
}

func needsChoice(t *testing.T, err error) []state.ModelDiff {
	t.Helper()
	var nm *state.NeedsModelChoiceError
	if !errors.As(err, &nm) || !errors.Is(err, state.ErrConflict) {
		t.Fatalf("want *state.NeedsModelChoiceError (409), got %v", err)
	}
	return nm.Diffs
}

// ---------- A3 launch ----------

func TestDraftNeverLeavesPendingUntilLaunch(t *testing.T) {
	e := newEnv(t, nil, allInstant()...)
	p2 := e.provider("p2", 4)
	d := e.draft()
	e.start()
	time.Sleep(80 * time.Millisecond) // many scans
	for _, s := range model.LectureStages {
		j := e.lj(d.ID, s)
		if j.Status != model.StatusPending || e.s.WaitingFor(j) != model.WaitNotStarted {
			t.Fatalf("draft %s: %s waiting_for=%q", s, j.Status, e.s.WaitingFor(j))
		}
	}
	// The snapshots are the settings at launch time, not at upload time.
	e.assign(p2.ID, "model-x", model.StageTranslate)
	if err := e.s.Launch(e.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(d.ID, model.StageExport, model.StatusDone)
	want := e.current()
	for s, snap := range e.snaps(d.ID) {
		if snap != want[s] {
			t.Errorf("%s snapshot %q, want %q", s, snap, want[s])
		}
	}
	if providerOfSnap(e.lj(d.ID, model.StageTranslate).ConfigSnapshot) != p2.ID {
		t.Fatal("translate snapshot does not name the provider chosen before launch")
	}
	if l, _ := e.st.GetLecture(e.ctx, d.ID); l.LaunchedAt == nil {
		t.Fatal("launched_at not set")
	}
	if !e.hasChanged("course", e.course.ID) || !e.hasChanged("lecture", d.ID) {
		t.Fatalf("launch events: %v", e.ev.changed)
	}
	if err := e.s.Launch(e.ctx, d.ID); !errors.Is(err, state.ErrAlreadyLaunched) {
		t.Fatalf("second launch: %v", err)
	}
	if err := e.s.Launch(e.ctx, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown lecture: %v", err)
	}
}

// Some but not all lecture roles configured: the launch is refused. (With
// none configured it is "只转写", SPEC-v3 V4.2 / 3.1.0: see
// TestTranscribeOnlyLaunch in transcribe_only_test.go.)
func TestLaunchNeedsConfiguredRoles(t *testing.T) {
	e := newEnvOpts(t, nil, func(o *Options) {
		// Settings without the legacy import: no provider, no role.
		s := settings.New(o.Store, o.Config)
		if err := s.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		o.Settings = s
	}, allInstant()...)
	e.assign(e.provider("p1", 4).ID, "model-a", model.StageSummary) // repair, translate: none
	d := e.draft()
	if err := e.s.Launch(e.ctx, d.ID); !errors.Is(err, settings.ErrRoleNotConfigured) {
		t.Fatalf("launch with some roles: %v", err)
	}
	if l, _ := e.st.GetLecture(e.ctx, d.ID); l.LaunchedAt != nil {
		t.Fatal("failed launch must leave a draft")
	}
	for s, snap := range e.snaps(d.ID) {
		if snap != model.EmptySnapshot {
			t.Fatalf("%s snapshot %q after a failed launch", s, snap)
		}
	}
}

// ---------- A4.3 rerun decision ----------

func TestRerunModelDecision(t *testing.T) {
	e := newEnv(t, nil, allInstant()...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	old := e.snaps(l.ID)

	// The translate model changes in the settings.
	e.assign(p2.ID, "model-b", model.StageTranslate)
	before := e.jobIDs(l.ID)
	diffs := needsChoice(t, e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""))
	cur := e.current()
	if len(diffs) != 1 {
		t.Fatalf("diffs = %+v", diffs)
	}
	d := diffs[0]
	if d.Stage != model.StageTranslate || d.LectureID != l.ID || d.Snapshot != old[model.StageTranslate] ||
		d.Current != cur[model.StageTranslate] || !d.SnapshotUsable || d.Reason != "" {
		t.Fatalf("diff = %+v", d)
	}
	// DoAction(rerun) is Rerun(..., ""): same question.
	needsChoice(t, e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ActionRerun))
	for s, id := range e.jobIDs(l.ID) {
		if id != before[s] || e.lj(l.ID, s).Status != model.StatusDone {
			t.Fatalf("the question must not change anything (%s)", s)
		}
	}

	// 沿用原模型: every new job keeps the snapshot of the job it replaces.
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	after := e.jobIDs(l.ID)
	for _, s := range model.LectureStages {
		if e.lj(l.ID, s).ConfigSnapshot != old[s] {
			t.Errorf("snapshot choice: %s snapshot changed", s)
		}
		renewed := s != model.StagePrep && s != model.StageASR
		if (after[s] != before[s]) != renewed {
			t.Errorf("snapshot choice: %s renewed=%v", s, after[s] != before[s])
		}
	}

	// 使用当前设置: the rerun stage and all its downstream stages.
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageRepair, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	for _, s := range []string{model.StageRepair, model.StageTranslate, model.StageExport} {
		if got := e.lj(l.ID, s).ConfigSnapshot; got != cur[s] {
			t.Errorf("current choice: %s snapshot %q, want %q", s, got, cur[s])
		}
	}
	if providerOfSnap(e.lj(l.ID, model.StageTranslate).ConfigSnapshot) != p2.ID {
		t.Fatal("translate must now use p2")
	}
	if e.lj(l.ID, model.StageSummary).ConfigSnapshot != old[model.StageSummary] {
		t.Fatal("summary is upstream of the rerun stage and must keep its snapshot")
	}

	// Everything matches now: no question.
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""); err != nil {
		t.Fatalf("rerun with matching snapshots: %v", err)
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)

	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, "maybe"); !errors.Is(err, state.ErrBadInput) {
		t.Fatalf("unknown choice: %v", err)
	}
	dr := e.draft()
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, dr.ID, model.StagePrep, ""); !errors.Is(err, state.ErrDraft) {
		t.Fatalf("rerun of a draft: %v", err)
	}
}

func TestRerunUnusableSnapshot(t *testing.T) {
	e := newEnv(t, nil, allInstant()...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	e.assign(p2.ID, "model-b", model.StageTranslate)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	// translate moves back to p1 and p2 is deleted: l's translate snapshot
	// can no longer be used.
	e.assign(p1.ID, "model-a", model.StageTranslate)
	if _, err := e.s.set.DeleteProvider(e.ctx, p2.ID); err != nil {
		t.Fatal(err)
	}
	diffs := needsChoice(t, e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""))
	if len(diffs) != 1 || diffs[0].Stage != model.StageTranslate || diffs[0].SnapshotUsable || diffs[0].ReasonInfo == nil ||
		diffs[0].ReasonInfo.Code != settings.CodeSnapshotProviderDeleted || diffs[0].Reason != diffs[0].ReasonInfo.Detail {
		t.Fatalf("diffs = %+v", diffs)
	}
	before := e.jobIDs(l.ID)
	err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceSnapshot)
	if !errors.Is(err, state.ErrSnapshotUnusable) || !errors.Is(err, state.ErrConflict) {
		t.Fatalf("snapshot choice with an unusable snapshot: %v", err)
	}
	if e.jobIDs(l.ID)[model.StageSummary] != before[model.StageSummary] {
		t.Fatal("refused rerun changed jobs")
	}
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if providerOfSnap(e.lj(l.ID, model.StageTranslate).ConfigSnapshot) != p1.ID {
		t.Fatal("translate must use p1 after the current choice")
	}
}

// ---------- retry with an unusable snapshot (lead extension of A4.3) ----------

func TestRetryUnusableSnapshotNeedsChoice(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	tr := &fakeStage{name: model.StageTranslate, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		if fail.Load() {
			return Permanent(errors.New("provider rejected the request"))
		}
		return nil
	}}
	e := newEnv(t, nil, allInstant(tr)...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	e.assign(p2.ID, "model-b", model.StageTranslate)
	l := e.lecture()
	e.start()
	failed := e.waitStatus(l.ID, model.StageTranslate, model.StatusFailed)

	// Usable snapshot: retry asks nothing and keeps it (A4.2).
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageTranslate, ""); err != nil {
		t.Fatal(err)
	}
	e.waitFor("second failure", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageTranslate)
		return j.Status == model.StatusFailed && j.Attempt == failed.Attempt+1
	})
	if got := e.lj(l.ID, model.StageTranslate).ConfigSnapshot; got != failed.ConfigSnapshot {
		t.Fatalf("retry changed the snapshot: %q", got)
	}

	// p2 deleted: retrying with the old snapshot cannot work.
	e.assign(p1.ID, "model-a", model.StageTranslate)
	if _, err := e.s.set.DeleteProvider(e.ctx, p2.ID); err != nil {
		t.Fatal(err)
	}
	diffs := needsChoice(t, e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageTranslate, ""))
	if len(diffs) != 1 || diffs[0].Stage != model.StageTranslate || diffs[0].SnapshotUsable || diffs[0].LectureID != l.ID {
		t.Fatalf("diffs = %+v", diffs)
	}
	needsChoice(t, e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageTranslate, model.ActionRetry))
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageTranslate, model.ModelChoiceSnapshot); !errors.Is(err, state.ErrSnapshotUnusable) {
		t.Fatalf("snapshot choice: %v", err)
	}
	if j := e.lj(l.ID, model.StageTranslate); j.Status != model.StatusFailed {
		t.Fatalf("refused retry changed the job: %s", j.Status)
	}
	// A stage that is not failed/cancelled: 409 before anything else.
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceCurrent); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("retry of a done stage: %v", err)
	}

	fail.Store(false)
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageTranslate, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	cur := e.current()
	j := e.lj(l.ID, model.StageTranslate)
	if j.ID != failed.ID || j.ConfigSnapshot != cur[model.StageTranslate] {
		t.Fatalf("current choice: id %s→%s snapshot %q", failed.ID, j.ID, j.ConfigSnapshot)
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
}

// ---------- A2.3 update, asked once for several lectures ----------

func TestUpdateLecturesAsksOnce(t *testing.T) {
	e := newEnv(t, nil, allInstant()...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	l1, l2, l3 := e.lecture(), e.lecture(), e.lecture()
	e.start()
	for _, l := range []model.Lecture{l1, l2, l3} {
		e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	}
	reason := "术语表已更新（v1 → v2）"
	markStale := func(l model.Lecture) {
		t.Helper()
		if err := state.TransitionJob(e.ctx, e.st, e.lj(l.ID, model.StageRepair).ID, model.StatusDone, model.StatusStale,
			db.JobFinish{StaleReason: &reason}); err != nil {
			t.Fatal(err)
		}
	}
	markStale(l1)
	markStale(l2)
	e.assign(p2.ID, "model-b", model.StageTranslate)
	d := e.draft()
	ids := []string{l1.ID, l2.ID, l3.ID, d.ID}
	before1, before2 := e.jobIDs(l1.ID), e.jobIDs(l2.ID)

	res, err := e.s.UpdateLectures(e.ctx, ids, "")
	diffs := needsChoice(t, err)
	if res != nil || len(diffs) != 2 {
		t.Fatalf("one aggregated question expected: res=%v diffs=%+v", res, diffs)
	}
	got := map[string]string{}
	for _, x := range diffs {
		got[x.LectureID] = x.Stage
	}
	if got[l1.ID] != model.StageTranslate || got[l2.ID] != model.StageTranslate {
		t.Fatalf("diffs by lecture = %v", got)
	}
	for s, id := range e.jobIDs(l1.ID) {
		if id != before1[s] || e.jobIDs(l2.ID)[s] != before2[s] {
			t.Fatal("the question must not change anything")
		}
	}

	res, err = e.s.UpdateLectures(e.ctx, ids, model.ModelChoiceCurrent)
	if err != nil || len(res) != 4 {
		t.Fatalf("update with a choice: %v %+v", err, res)
	}
	if res[0].LectureID != l1.ID || res[0].Stage != model.StageRepair || res[0].Err != nil ||
		res[1].LectureID != l2.ID || res[1].Stage != model.StageRepair || res[1].Err != nil ||
		res[2].LectureID != l3.ID || res[2].Stage != "" || res[2].Err != nil ||
		res[3].LectureID != d.ID || !errors.Is(res[3].Err, state.ErrDraft) {
		t.Fatalf("results = %+v", res)
	}
	for _, l := range []model.Lecture{l1, l2} {
		e.waitStatus(l.ID, model.StageExport, model.StatusDone)
		if providerOfSnap(e.lj(l.ID, model.StageTranslate).ConfigSnapshot) != p2.ID {
			t.Fatalf("%s translate not switched to p2", l.ID)
		}
	}
	if providerOfSnap(e.lj(l3.ID, model.StageTranslate).ConfigSnapshot) != p1.ID {
		t.Fatal("l3 had nothing stale and must be untouched")
	}

	// UpdateLecture: nothing stale; matching snapshots update without asking.
	if st, err := e.s.UpdateLecture(e.ctx, l3.ID, ""); st != "" || err != nil {
		t.Fatalf("nothing stale: %q %v", st, err)
	}
	markStale(l1)
	if st, err := e.s.UpdateLecture(e.ctx, l1.ID, ""); st != model.StageRepair || err != nil {
		t.Fatalf("update l1: %q %v", st, err)
	}
	e.waitStatus(l1.ID, model.StageExport, model.StatusDone)
	if _, err := e.s.UpdateLecture(e.ctx, d.ID, ""); !errors.Is(err, state.ErrDraft) {
		t.Fatalf("update draft: %v", err)
	}
	if _, err := e.s.UpdateLecture(e.ctx, "nope", ""); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("update unknown: %v", err)
	}
}

// ---------- A7 cancel and delete ----------

func TestCancelLecture(t *testing.T) {
	g := newGate()
	e := newEnv(t, nil, allInstant(&fakeStage{name: model.StageSummary, res: ResourceAPI, run: g.run})...)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageSummary, model.StatusRunning)
	if n, err := e.s.CancelLecture(e.ctx, l.ID); n != 1 || err != nil {
		t.Fatalf("cancel running: %d %v", n, err)
	}
	if s := e.lj(l.ID, model.StageSummary).Status; s != model.StatusCancelled {
		t.Fatalf("summary = %s", s)
	}
	if s := e.lj(l.ID, model.StageRepair).Status; s != model.StatusPending {
		t.Fatalf("repair = %s", s)
	}
	if n, err := e.s.CancelLecture(e.ctx, l.ID); n != 0 || err != nil {
		t.Fatalf("nothing to cancel: %d %v", n, err)
	}
	// Queued jobs are cancelled too.
	e.s.SetGPUPaused(true, false)
	l2 := e.lecture()
	e.waitStatus(l2.ID, model.StageASR, model.StatusQueued)
	if n, err := e.s.CancelLecture(e.ctx, l2.ID); n != 1 || err != nil {
		t.Fatalf("cancel queued: %d %v", n, err)
	}
	if s := e.lj(l2.ID, model.StageASR).Status; s != model.StatusCancelled {
		t.Fatalf("asr = %s", s)
	}
	if _, err := e.s.CancelLecture(e.ctx, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown lecture: %v", err)
	}
}

func TestDeleteLectureForce(t *testing.T) {
	var stopped atomic.Bool
	entered := make(chan struct{})
	sum := &fakeStage{name: model.StageSummary, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		close(entered)
		<-ctx.Done()
		stopped.Store(true)
		return ctx.Err()
	}}
	e := newEnvOpts(t, nil, func(o *Options) { o.DeleteWait = 3 * time.Second }, allInstant(sum)...)
	l := e.lecture()
	ext := filepath.Join(t.TempDir(), "original.mp4") // outside data_dir
	os.WriteFile(ext, []byte("media"), 0o644)
	if err := e.st.SetLectureMedia(e.ctx, l.ID, ext); err != nil {
		t.Fatal(err)
	}
	ldir := model.LectureDir(e.dataDir, l.CourseID, l.ID)
	os.MkdirAll(ldir, 0o755)
	os.WriteFile(filepath.Join(ldir, model.AudioFile), []byte("wav"), 0o644)
	e.start()
	e.waitStatus(l.ID, model.StageSummary, model.StatusRunning)
	<-entered // "running" is committed before the stage goroutine enters Run
	// Materials (created after asr, so they do not block it): one bound to
	// the lecture, one course-wide.
	bound := model.Material{ID: db.NewID(), CourseID: e.course.ID, LectureID: &l.ID, Filename: "w1.pdf", Path: "p"}
	wide := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "all.pdf", Path: "p"}
	for _, m := range []model.Material{bound, wide} {
		e.st.CreateMaterial(e.ctx, m)
		os.MkdirAll(model.MaterialDir(e.dataDir, m.CourseID, m.ID), 0o755)
	}

	// Without force: 409, nothing deleted.
	if err := e.s.DeleteLecture(e.ctx, l.ID, false); !errors.Is(err, state.ErrBusy) || !errors.Is(err, state.ErrConflict) {
		t.Fatalf("delete running without force: %v", err)
	}
	if _, err := e.st.GetLecture(e.ctx, l.ID); err != nil || !exists(ldir) {
		t.Fatalf("lecture must survive: %v", err)
	}

	// With force: cancel, wait, delete.
	if err := e.s.DeleteLecture(e.ctx, l.ID, true); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() {
		t.Fatal("running stage was not cancelled")
	}
	if _, err := e.st.GetLecture(e.ctx, l.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("lecture row: %v", err)
	}
	if js, _ := e.st.ListCurrentJobs(e.ctx, model.ScopeLecture, l.ID); len(js) != 0 {
		t.Fatalf("jobs left: %d", len(js))
	}
	if _, err := e.st.GetMaterial(e.ctx, bound.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("bound material: %v", err)
	}
	if exists(ldir) || exists(model.MaterialDir(e.dataDir, bound.CourseID, bound.ID)) {
		t.Fatal("lecture or bound material directory left behind")
	}
	if _, err := e.st.GetMaterial(e.ctx, wide.ID); err != nil || !exists(model.MaterialDir(e.dataDir, wide.CourseID, wide.ID)) {
		t.Fatalf("course-wide material must stay: %v", err)
	}
	if !exists(ext) {
		t.Fatal("the original media outside data_dir must never be touched")
	}
	if !e.hasChanged("course", e.course.ID) {
		t.Fatalf("no course event: %v", e.ev.changed)
	}
	if err := e.s.DeleteLecture(e.ctx, l.ID, true); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// An idle lecture needs no force.
	d := e.draft()
	if err := e.s.DeleteLecture(e.ctx, d.ID, false); err != nil {
		t.Fatalf("delete idle draft: %v", err)
	}
}

func TestDeleteLectureTimeout(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{})
	stubborn := &fakeStage{name: model.StageSummary, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		close(entered)
		<-release // ignores the cancel
		return nil
	}}
	e := newEnvOpts(t, nil, func(o *Options) { o.DeleteWait = 150 * time.Millisecond }, allInstant(stubborn)...)
	t.Cleanup(func() { close(release) }) // runs before the env's Stop (LIFO)
	l := e.lecture()
	ldir := model.LectureDir(e.dataDir, l.CourseID, l.ID)
	os.MkdirAll(ldir, 0o755)
	e.start()
	e.waitStatus(l.ID, model.StageSummary, model.StatusRunning)
	<-entered
	t0 := time.Now()
	err := e.s.DeleteLecture(e.ctx, l.ID, true)
	if !errors.Is(err, ErrDeleteTimeout) || !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stubborn stage: %v", err)
	}
	if took := time.Since(t0); took < 150*time.Millisecond || took > 3*time.Second {
		t.Fatalf("waited %v", took)
	}
	if _, err := e.st.GetLecture(e.ctx, l.ID); err != nil || !exists(ldir) {
		t.Fatalf("nothing may be deleted after a timeout: %v", err)
	}
	if js, _ := e.st.ListCurrentJobs(e.ctx, model.ScopeLecture, l.ID); len(js) != 6 {
		t.Fatalf("jobs = %d", len(js))
	}
}

// A job that finishes while the delete waits must not let the next stage of
// the lecture start: the delete holds the scan lock until the rows are gone.
func TestDeleteLectureStartsNothingMeanwhile(t *testing.T) {
	var repairRuns atomic.Int32
	entered := make(chan struct{})
	sum := &fakeStage{name: model.StageSummary, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		close(entered)
		<-ctx.Done()
		time.Sleep(30 * time.Millisecond)
		return nil // finishes as done despite the cancel
	}}
	rep := &fakeStage{name: model.StageRepair, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		repairRuns.Add(1)
		return nil
	}}
	e := newEnvOpts(t, nil, func(o *Options) { o.DeleteWait = 3 * time.Second }, allInstant(sum, rep)...)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageSummary, model.StatusRunning)
	<-entered
	if err := e.s.DeleteLecture(e.ctx, l.ID, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // many scans
	if n := repairRuns.Load(); n != 0 {
		t.Fatalf("repair started %d times during/after the delete", n)
	}
	if js, _ := e.st.ListCurrentJobs(e.ctx, model.ScopeLecture, l.ID); len(js) != 0 {
		t.Fatalf("jobs left: %+v", js)
	}
}

// ---------- A2.2 invalidation with a cause ----------

func TestInvalidateCourseWithWritesReasonAndEmits(t *testing.T) {
	var hash atomic.Value
	hash.Store("h1")
	repair := &fakeStage{name: model.StageRepair, res: ResourceAPI,
		hash: func(ctx context.Context, id string) (string, error) { return hash.Load().(string), nil }}
	e := newEnv(t, nil, allInstant(repair)...)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if _, err := e.st.BumpGlossaryVersion(e.ctx, e.course.ID); err != nil {
		t.Fatal(err)
	}
	hash.Store("h2")
	e.ev.mu.Lock()
	nEvents := len(e.ev.jobs)
	e.ev.mu.Unlock()
	if err := e.s.InvalidateCourseWith(e.ctx, e.course.ID, state.StaleCause{Kind: state.StaleGlossary}); err != nil {
		t.Fatal(err)
	}
	j := e.lj(l.ID, model.StageRepair)
	// P14.3: stored as JSON ("术语表已更新（现为 v1）" before v2 phase 2).
	if j.Status != model.StatusStale || j.StaleReason == nil || *j.StaleReason != model.StaleReasonJSON(model.StaleCodeGlossary, map[string]any{"to": 1}) {
		t.Fatalf("repair = %s %v", j.Status, j.StaleReason)
	}
	e.ev.mu.Lock()
	var emitted bool
	for _, x := range e.ev.jobs[nEvents:] {
		if x.ID == j.ID && x.Status == model.StatusStale {
			emitted = true
		}
	}
	e.ev.mu.Unlock()
	if !emitted {
		t.Fatal("no job event for the stale job")
	}
	time.Sleep(40 * time.Millisecond)
	if s := e.lj(l.ID, model.StageRepair).Status; s != model.StatusStale {
		t.Fatalf("stale must not rerun automatically: %s", s)
	}
}

// ---------- settings wiring (A5) ----------

func TestGPUSlotsAndASRSettingsAtScanTime(t *testing.T) {
	g := newGate()
	e := newEnv(t, nil, gpuStages(g)...) // max_parallel 1 (imported)
	l1, l2 := e.lecture(), e.lecture()
	e.prepDone(l1, l2)
	e.start()
	e.waitFor("one asr running", 5*time.Second, func() bool { c, _, _ := g.stats(); return c == 1 })
	e.waitFor("gpu_slot", 5*time.Second, func() bool { return e.s.WaitingFor(e.lj(l2.ID, model.StageASR)) == model.WaitGPUSlot })
	if used, max, paused := e.s.GPUSlots(); used != 1 || max != 1 || paused {
		t.Fatalf("GPUSlots = %d %d %v", used, max, paused)
	}
	a := e.s.set.ASR()
	a.MaxParallel = 2
	if err := e.s.set.SetASR(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	e.waitFor("two asr running", 5*time.Second, func() bool { c, _, _ := g.stats(); return c == 2 })
	if used, max, _ := e.s.GPUSlots(); used != 2 || max != 2 {
		t.Fatalf("GPUSlots = %d %d", used, max)
	}
	e.s.SetGPUPaused(true, false)
	if _, _, paused := e.s.GPUSlots(); !paused {
		t.Fatal("paused not reported")
	}
	close(g.release)
	e.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	e.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

func TestVRAMReserveFromSettings(t *testing.T) {
	e := newEnv(t, nil, gpuStages(newGate())...)
	a := e.s.set.ASR()
	a.VRAMReserveMB = 19000 // free 20000 - 19000 < 5000 per job
	if err := e.s.set.SetASR(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	l := e.lecture()
	e.start()
	e.waitFor("vram", 5*time.Second, func() bool { return e.s.WaitingFor(e.lj(l.ID, model.StageASR)) == model.WaitVRAM })
	a.VRAMReserveMB = 1000
	if err := e.s.set.SetASR(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(l.ID, model.StageASR, model.StatusRunning)
}

func TestVRAMCalibrationKeyUsesSnapshotModel(t *testing.T) {
	var e *env
	asr := &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(ctx context.Context, id string, p func(float64)) error {
		e.gpu.set(14000, 24000, 7000) // +6000 MB over the 1000 MB baseline
		err := e.waitSampledPeak(id, 6000)
		e.gpu.set(20000, 24000, 1000)
		return err
	}}
	e = newEnv(t, nil, instant(model.StagePrep, ResourceCPU), asr)
	a := e.s.set.ASR()
	a.Model = "large-v2"
	if err := e.s.set.SetASR(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	l := e.lecture() // asr snapshot: large-v2
	a.Model = "large-v3-turbo"
	if err := e.s.set.SetASR(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitStatus(l.ID, model.StageASR, model.StatusDone)
	if v, ok, _ := e.st.GetKV(e.ctx, model.KVVRAMObservedPrefix+"large-v2:float16"); !ok || v != "6000" {
		t.Fatalf("calibration of the snapshot model = %q (ok=%v)", v, ok)
	}
	if _, ok, _ := e.st.GetKV(e.ctx, model.KVVRAMObservedPrefix+"large-v3-turbo:float16"); ok {
		t.Fatal("calibrated the current setting instead of the model the job ran with")
	}
}

func TestAPIProviderOfJob(t *testing.T) {
	e := newEnv(t, nil, append(allInstant(), instant(model.StageMaterialExtract, ResourceCPU), instant(model.StageMaterialTerms, ResourceAPI))...)
	p1, p2 := e.provider("p1", 1), e.provider("p2", 1)
	e.assign(p1.ID, "model-a", llmStages...)
	release, err := e.s.Pool().Acquire(e.ctx, p1.ID) // p1's only slot
	if err != nil {
		t.Fatal(err)
	}
	l := e.lecture() // summary snapshot: p1
	// The current setting moves to p2 (which is free): the lecture job must
	// still wait for the provider of its snapshot.
	e.assign(p2.ID, "model-b", llmStages...)
	e.start()
	e.waitFor("api_slot", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageSummary)
		return j.Status == model.StatusQueued && e.s.WaitingFor(j) == model.WaitAPISlot
	})
	release()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)

	// Course/material jobs have no snapshot: the role's current provider.
	if err := e.s.set.SetRole(e.ctx, model.RoleGlossary, p2.ID, "model-b", model.RoleParams{}); err != nil {
		t.Fatal(err)
	}
	release, err = e.s.Pool().Acquire(e.ctx, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "a.pdf", Path: "p"}
	e.st.CreateMaterial(e.ctx, m)
	e.waitFor("material_terms api_slot", 5*time.Second, func() bool {
		j := e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms)
		return j.Status == model.StatusQueued && e.s.WaitingFor(j) == model.WaitAPISlot
	})
	release()
	e.waitFor("material_terms done", 5*time.Second, func() bool {
		return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms).Status == model.StatusDone
	})
}

// job.stats.estimated_cost uses the prices of the provider the job runs
// with (its snapshot), not of the role's current provider.
func TestJobCostUsesSnapshotProviderPrices(t *testing.T) {
	sum := &fakeStage{name: model.StageSummary, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		ReportUsage(ctx, 1_000_000, 200_000, 400_000)
		return nil
	}}
	e := newEnv(t, nil, allInstant(sum)...)
	p1 := e.provider("p1", 4, 1, 0.5, 2)
	p2 := e.provider("p2", 4, 100, 100, 100)
	e.assign(p1.ID, "model-a", llmStages...)
	l := e.lecture()
	e.assign(p2.ID, "model-b", llmStages...)
	e.start()
	j := e.waitStatus(l.ID, model.StageSummary, model.StatusDone)
	// (1M - 0.4M) × 1 + 0.4M × 0.5 + 0.2M × 2 = 0.6 + 0.2 + 0.4 = 1.2
	if math.Abs(j.Stats.EstimatedCost-1.2) > 1e-9 {
		t.Fatalf("estimated_cost = %v, want 1.2 (p1 prices)", j.Stats.EstimatedCost)
	}
}

// DeleteLecture hands stopRuns the jobs that were running when it listed
// them; one may have finished before stopRuns looks (no longer tracked):
// re-read it, skip it when it is no longer running, cancel it when it went
// back to queued (automatic retry), and refuse only a job that is still
// running but not tracked. The scheduler is not started here, so the
// statuses are driven directly.
func TestStopRunsRereadsJobsThatChangedMeanwhile(t *testing.T) {
	e := newEnv(t, nil, allInstant()...)
	l, l2 := e.lecture(), e.lecture()
	drive := func(id, stage string, path ...string) {
		t.Helper()
		from := e.lj(id, stage).Status
		for _, to := range path {
			if err := state.TransitionJob(e.ctx, e.st, e.lj(id, stage).ID, from, to, db.JobFinish{}); err != nil {
				t.Fatal(err)
			}
			from = to
		}
	}
	asListed := func(id, stage string) model.Job { // the caller's copy: running
		j := e.lj(id, stage)
		j.Status = model.StatusRunning
		return j
	}
	drive(l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)                      // finished meanwhile
	drive(l.ID, model.StageASR, model.StatusQueued, model.StatusRunning, model.StatusFailed, model.StatusQueued) // auto retry
	if err := e.s.stopRuns(e.ctx, []model.Job{asListed(l.ID, model.StagePrep), asListed(l.ID, model.StageASR)}); err != nil {
		t.Fatalf("jobs that stopped meanwhile: %v", err)
	}
	if s := e.lj(l.ID, model.StagePrep).Status; s != model.StatusDone {
		t.Fatalf("prep = %s", s)
	}
	if s := e.lj(l.ID, model.StageASR).Status; s != model.StatusCancelled {
		t.Fatalf("requeued asr must be cancelled, got %s", s)
	}
	// Still running in the database, but no run is tracked: refused.
	drive(l2.ID, model.StagePrep, model.StatusQueued, model.StatusRunning)
	if err := e.s.stopRuns(e.ctx, []model.Job{asListed(l2.ID, model.StagePrep)}); !errors.Is(err, state.ErrBusy) {
		t.Fatalf("untracked running job: %v", err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
