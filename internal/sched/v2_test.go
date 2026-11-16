// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Scheduler tests of the v2 glossary_review task (SPEC-v2 G3.1, appendix
// D2; lead rulings 2–4 in .claude/v2-brief.md). Fake stages only. Written
// before the implementation.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// reviewStage is a fake glossary_review stage: every run reports itself on
// started and returns the next value sent on results (or ctx.Err()).
type reviewStage struct {
	*fakeStage
	runs    atomic.Int32
	started chan string
	results chan error
}

func newReviewStage() *reviewStage {
	r := &reviewStage{started: make(chan string, 16), results: make(chan error)}
	r.fakeStage = &fakeStage{name: model.StageGlossaryReview, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		r.runs.Add(1)
		r.started <- id
		select {
		case err := <-r.results:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	return r
}

func (r *reviewStage) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("glossary_review did not start")
	}
}

func (r *reviewStage) finish(t *testing.T, err error) {
	t.Helper()
	select {
	case r.results <- err:
	case <-time.After(5 * time.Second):
		t.Fatal("glossary_review run is not waiting for a result")
	}
}

// configureJanitor assigns the glossary_janitor role to a new provider
// (fake key) and returns it.
func (e *env) configureJanitor() model.Provider {
	e.t.Helper()
	p := e.provider("opus", 4)
	e.assign(p.ID, "claude-opus-5-5", model.StageGlossaryReview)
	return p
}

func (e *env) review() model.Job {
	e.t.Helper()
	return e.job(model.ScopeCourse, e.course.ID, model.StageGlossaryReview)
}

func (e *env) enqueueReview() {
	e.t.Helper()
	queued, err := state.EnqueueGlossaryReview(e.ctx, e.st, e.course.ID)
	if err != nil || !queued {
		e.t.Fatalf("EnqueueGlossaryReview: queued=%v err=%v", queued, err)
	}
}

func (e *env) dirty() string {
	e.t.Helper()
	v, _, err := e.st.GetKV(e.ctx, model.KVGlossaryReviewDirtyPrefix+e.course.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

func (e *env) waitReview(what string, cond func(j model.Job) bool) model.Job {
	e.t.Helper()
	var j model.Job
	e.waitFor(what, 5*time.Second, func() bool {
		j = e.review()
		return cond(j)
	})
	return j
}

// New terms while the review runs: dirty, then exactly one more run after
// it is done (several triggers merge into that one run).
func TestGlossaryReviewRunsOnceMoreWhenDirty(t *testing.T) {
	rs := newReviewStage()
	e := newEnv(t, nil, rs)
	e.configureJanitor()
	e.enqueueReview()
	e.start()
	rs.waitStarted(t)
	first := e.review()
	if first.Status != model.StatusRunning {
		t.Fatalf("first run: %+v", first)
	}
	e.enqueueReview()
	e.enqueueReview()
	if e.dirty() != "1" {
		t.Fatalf("dirty = %q", e.dirty())
	}
	if j := e.review(); j.ID != first.ID {
		t.Fatal("a running review was replaced")
	}
	rs.finish(t, nil)
	rs.waitStarted(t) // the second run
	second := e.review()
	if second.ID == first.ID {
		t.Fatal("no new job for the second run")
	}
	if old, err := e.st.GetJob(e.ctx, first.ID); err != nil || old.Status != model.StatusDone || old.IsCurrent {
		t.Fatalf("first job: %+v %v", old, err)
	}
	if e.dirty() != "" {
		t.Fatalf("dirty not cleared: %q", e.dirty())
	}
	rs.finish(t, nil)
	e.waitReview("second run done", func(j model.Job) bool { return j.Status == model.StatusDone })
	time.Sleep(60 * time.Millisecond) // many scans
	if n := rs.runs.Load(); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
	if j := e.review(); j.ID != second.ID || j.Status != model.StatusDone {
		t.Fatalf("after the second run: %+v", j)
	}
}

// Without new terms during the run nothing more happens.
func TestGlossaryReviewNoDirtyNoRerun(t *testing.T) {
	rs := newReviewStage()
	e := newEnv(t, nil, rs)
	e.configureJanitor()
	e.enqueueReview()
	e.start()
	rs.waitStarted(t)
	rs.finish(t, nil)
	done := e.waitReview("done", func(j model.Job) bool { return j.Status == model.StatusDone })
	time.Sleep(60 * time.Millisecond)
	if n := rs.runs.Load(); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	// The course view's stage actions work on it: rerun, then cancel.
	if err := e.s.DoAction(e.ctx, model.ScopeCourse, e.course.ID, model.StageGlossaryReview, model.ActionRerun); err != nil {
		t.Fatal(err)
	}
	rs.waitStarted(t)
	j := e.review()
	if j.ID == done.ID || j.Status != model.StatusRunning {
		t.Fatalf("after rerun: %+v", j)
	}
	if err := e.s.DoAction(e.ctx, model.ScopeCourse, e.course.ID, model.StageGlossaryReview, model.ActionCancel); err != nil {
		t.Fatal(err)
	}
	if j := e.review(); j.Status != model.StatusCancelled {
		t.Fatalf("after cancel: %+v", j)
	}
}

// A final failure (e.g. Opus unavailable: Permanent) is an end like done.
func TestGlossaryReviewDirtyAfterFinalFailure(t *testing.T) {
	rs := newReviewStage()
	e := newEnv(t, nil, rs)
	e.configureJanitor()
	e.enqueueReview()
	e.start()
	rs.waitStarted(t)
	first := e.review()
	e.enqueueReview()
	rs.finish(t, Permanent(errors.New("no anthropic key")))
	rs.waitStarted(t)
	if old, err := e.st.GetJob(e.ctx, first.ID); err != nil || old.Status != model.StatusFailed || old.IsCurrent {
		t.Fatalf("failed job: %+v %v", old, err)
	}
	if e.dirty() != "" {
		t.Fatalf("dirty = %q", e.dirty())
	}
	rs.finish(t, nil)
	e.waitReview("second run done", func(j model.Job) bool { return j.Status == model.StatusDone })
	if n := rs.runs.Load(); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

// The automatic retry's backoff is not an end: the flag waits for the
// retried run, which is followed by one more run.
func TestGlossaryReviewDirtyKeptDuringRetryBackoff(t *testing.T) {
	rs := newReviewStage()
	// The scheduler's clock stands still until the test moves it: the
	// backoff lasts until the test has looked at it (a 400 ms state polled
	// on a loaded machine can be over before it is seen).
	clk := &gpuTestClock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	e := newEnvOpts(t, nil, func(o *Options) {
		o.RetryBackoff = []time.Duration{400 * time.Millisecond}
		o.Now = clk.Now
	}, rs)
	e.configureJanitor()
	e.enqueueReview()
	e.start()
	rs.waitStarted(t)
	first := e.review()
	e.enqueueReview()
	rs.finish(t, errors.New("HTTP 503"))
	// The waiting reason is the scan's: wait for the scan that saw the job.
	j := e.waitReview("requeued in backoff", func(j model.Job) bool {
		return j.Status == model.StatusQueued && strings.HasPrefix(e.s.WaitingFor(j), model.WaitRetryBackoffPrefix)
	})
	if j.ID != first.ID || j.Attempt != 1 {
		t.Fatalf("auto retry: %+v", j)
	}
	if e.dirty() != "1" {
		t.Fatalf("dirty consumed during the backoff: %q", e.dirty())
	}
	if n := rs.runs.Load(); n != 1 {
		t.Fatalf("runs during the backoff = %d", n)
	}
	clk.Advance(400 * time.Millisecond) // the backoff ends
	rs.waitStarted(t)                   // the retry
	if j := e.review(); j.ID != first.ID {
		t.Fatal("the retry must reuse the job")
	}
	rs.finish(t, nil)
	rs.waitStarted(t) // one more run for the dirty flag
	if j := e.review(); j.ID == first.ID {
		t.Fatal("no new job after the retried run")
	}
	rs.finish(t, nil)
	e.waitReview("done", func(j model.Job) bool { return j.Status == model.StatusDone })
	if n := rs.runs.Load(); n != 3 {
		t.Fatalf("runs = %d, want 3", n)
	}
}

// A user cancel drops the flag and queues nothing (lead answer to
// core-builder, NOTES.md): the cancel must stick, and retrying the job
// reviews the whole glossary anyway.
func TestGlossaryReviewUserCancelDropsDirty(t *testing.T) {
	rs := newReviewStage()
	e := newEnvOpts(t, nil, func(o *Options) { o.RetryBackoff = []time.Duration{time.Hour} }, rs)
	e.configureJanitor()
	e.enqueueReview()
	e.start()

	// running
	rs.waitStarted(t)
	first := e.review()
	e.enqueueReview()
	if err := e.s.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	if j := e.review(); j.ID != first.ID || j.Status != model.StatusCancelled {
		t.Fatalf("after cancel: %+v", j)
	}
	if e.dirty() != "" {
		t.Fatalf("dirty = %q", e.dirty())
	}
	time.Sleep(60 * time.Millisecond)
	if j := e.review(); j.ID != first.ID || j.Status != model.StatusCancelled {
		t.Fatalf("a cancelled review came back: %+v", j)
	}

	// queued in the retry backoff
	if err := e.s.DoAction(e.ctx, model.ScopeCourse, e.course.ID, model.StageGlossaryReview, model.ActionRetry); err != nil {
		t.Fatal(err)
	}
	rs.waitStarted(t)
	e.enqueueReview()
	rs.finish(t, errors.New("HTTP 503"))
	j := e.waitReview("in backoff", func(j model.Job) bool { return j.Status == model.StatusQueued })
	if e.dirty() != "1" {
		t.Fatalf("dirty = %q", e.dirty())
	}
	if err := e.s.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if e.dirty() != "" {
		t.Fatalf("dirty after cancelling the queued retry: %q", e.dirty())
	}
	time.Sleep(60 * time.Millisecond)
	if j2 := e.review(); j2.ID != j.ID || j2.Status != model.StatusCancelled {
		t.Fatalf("a cancelled review came back: %+v", j2)
	}
	if n := rs.runs.Load(); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

// asr waits for the review while it is pending/queued/running and shows
// waiting_for "glossary_review"; afterwards it runs.
func TestASRWaitsForGlossaryReview(t *testing.T) {
	rs := newReviewStage()
	asr := newGate()
	close(asr.release)
	e := newEnv(t, nil, append(gpuStages(asr), rs)...)
	e.configureJanitor()
	l := e.lecture()
	e.prepDone(l)
	e.enqueueReview()
	e.start()
	rs.waitStarted(t)
	time.Sleep(40 * time.Millisecond) // many scans
	j := e.lj(l.ID, model.StageASR)
	if j.Status != model.StatusPending {
		t.Fatalf("asr while the review runs: %s", j.Status)
	}
	if w := e.s.WaitingFor(j); w != model.WaitGlossaryReview {
		t.Fatalf("waiting_for = %q", w)
	}
	rs.finish(t, nil)
	e.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

// A failed review never blocks asr (appendix D2).
func TestASRNotBlockedByFailedReview(t *testing.T) {
	rs := newReviewStage()
	asr := newGate()
	close(asr.release)
	e := newEnv(t, nil, append(gpuStages(asr), rs)...)
	e.configureJanitor()
	l := e.lecture()
	e.prepDone(l)
	e.enqueueReview()
	e.start()
	rs.waitStarted(t)
	rs.finish(t, Permanent(errors.New("HTTP 401")))
	e.waitStatus(l.ID, model.StageASR, model.StatusDone)
	if j := e.review(); j.Status != model.StatusFailed {
		t.Fatalf("review: %+v", j)
	}
}

// The review is an API stage whose provider is the glossary_janitor role's
// current setting (course jobs have no snapshot, like glossary_seed).
func TestGlossaryReviewProviderIsJanitorRole(t *testing.T) {
	rs := newReviewStage()
	e := newEnv(t, nil, rs)
	p := e.provider("opus1", 1)
	e.assign(p.ID, "claude-opus-5-5", model.StageGlossaryReview)
	e.enqueueReview()
	if got := e.s.providerOf(rs, e.review()); got != p.ID {
		t.Fatalf("providerOf = %q, want the janitor provider %q", got, p.ID)
	}
	if rs.Provider() == p.ID {
		t.Fatal("test setup: the stage's own provider must differ")
	}
	// The pool of that provider (one slot, taken) gates the start.
	release, err := e.s.pool.Acquire(e.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.start()
	j := e.waitReview("queued", func(j model.Job) bool { return j.Status == model.StatusQueued })
	e.waitFor("api_slot", 5*time.Second, func() bool { return e.s.WaitingFor(j) == model.WaitAPISlot })
	release()
	rs.waitStarted(t)
	rs.finish(t, nil)
}

// A flag left over from a run that ended without the scheduler seeing it
// (startup recovery failed it for good, or a crash) is handled at Start.
func TestStartConsumesLeftoverDirty(t *testing.T) {
	rs := newReviewStage()
	e := newEnv(t, nil, rs)
	e.configureJanitor()
	e.enqueueReview()
	failed := e.review()
	for _, step := range [][2]string{{model.StatusPending, model.StatusQueued}, {model.StatusQueued, model.StatusRunning}, {model.StatusRunning, model.StatusFailed}} {
		if err := state.TransitionJob(e.ctx, e.st, failed.ID, step[0], step[1], db.JobFinish{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.st.SetKV(e.ctx, model.KVGlossaryReviewDirtyPrefix+e.course.ID, "1"); err != nil {
		t.Fatal(err)
	}
	e.start()
	rs.waitStarted(t)
	if j := e.review(); j.ID == failed.ID {
		t.Fatal("no new job for the leftover flag")
	}
	if e.dirty() != "" {
		t.Fatalf("dirty = %q", e.dirty())
	}
	rs.finish(t, nil)
	e.waitReview("done", func(j model.Job) bool { return j.Status == model.StatusDone })
}

// A flag whose review is still waiting (pending/queued) is kept for the end
// of that run.
func TestStartKeepsDirtyOfWaitingReview(t *testing.T) {
	rs := newReviewStage()
	e := newEnv(t, nil, rs)
	e.configureJanitor()
	e.enqueueReview() // pending
	if err := e.st.SetKV(e.ctx, model.KVGlossaryReviewDirtyPrefix+e.course.ID, "1"); err != nil {
		t.Fatal(err)
	}
	pending := e.review()
	e.start()
	rs.waitStarted(t)
	if j := e.review(); j.ID != pending.ID {
		t.Fatal("the waiting review was replaced")
	}
	if e.dirty() != "1" {
		t.Fatalf("dirty = %q", e.dirty())
	}
	rs.finish(t, nil)
	rs.waitStarted(t)
	rs.finish(t, nil)
	e.waitReview("done", func(j model.Job) bool { return j.Status == model.StatusDone && j.ID != pending.ID })
}
