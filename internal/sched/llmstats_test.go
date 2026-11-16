// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// v3.2.x P1-c: the run turns the llm client's request events into
// job.stats.llm and job.stats.last_activity_at.

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

func bareRun(c *testClock) *run { return &run{now: c.now} }

func reqStart(at time.Time, call string, n int) RequestEvent {
	return RequestEvent{Phase: RequestStart, At: at, CallID: call, N: n, Try: 1, Role: "summary", ProviderID: "p1",
		Model: "m-large", MaxTokens: 4000 * n, Thinking: "high", Timeout: 120 * time.Second}
}

func reqEnd(at time.Time, call string, n int, outcome string, prompt, completion int64) RequestEvent {
	ev := reqStart(at, call, n)
	ev.Phase, ev.Outcome, ev.PromptTokens, ev.CompletionTokens = RequestEnd, outcome, prompt, completion
	return ev
}

func llmOf(t *testing.T, r *run) model.LLMStats {
	t.Helper()
	st, _ := r.statsSnapshot()
	if st.LLM == nil {
		return model.LLMStats{}
	}
	return *st.LLM
}

func TestLLMStatsTruncatedThenOK(t *testing.T) {
	c := newTestClock()
	r := bareRun(c)
	t0 := c.now()
	r.ReportRequest(reqStart(t0, "c1", 1))
	l := llmOf(t, r)
	if l.InFlight != 1 || l.Current == nil || l.Current.N != 1 || l.Current.Model != "m-large" || l.Current.MaxTokens != 4000 ||
		l.Current.Thinking != "high" || l.Current.TimeoutMS != 120000 || l.Current.StartedAt != stamp(t0) || len(l.Attempts) != 0 {
		t.Fatalf("after the first start: %+v current=%+v", l, l.Current)
	}
	r.ReportRequest(RequestEvent{Phase: RequestPhase, At: c.advance(time.Second), CallID: "c1", N: 1, StreamPhase: model.LLMPhaseThinking})
	if l = llmOf(t, r); l.Current == nil || l.Current.Phase != model.LLMPhaseThinking {
		t.Fatalf("phase: %+v", l.Current)
	}
	t1 := c.advance(30 * time.Second)
	r.ReportRequest(reqEnd(t1, "c1", 1, model.LLMOutcomeTruncated, 1000, 4000))
	l = llmOf(t, r)
	if l.InFlight != 0 || l.Current != nil || len(l.Attempts) != 1 || l.Retries != 0 {
		t.Fatalf("after the truncated end: %+v", l)
	}
	t2 := c.advance(time.Second)
	r.ReportRequest(reqStart(t2, "c1", 2))
	l = llmOf(t, r)
	if l.InFlight != 1 || l.Current == nil || l.Current.N != 2 || l.Current.MaxTokens != 8000 || l.Current.Phase != "" || l.Retries != 1 {
		t.Fatalf("after the second start: %+v current=%+v", l, l.Current)
	}
	t3 := c.advance(40 * time.Second)
	reason := int64(1500)
	end := reqEnd(t3, "c1", 2, model.LLMOutcomeOK, 1000, 6000)
	end.ReasoningTokens = &reason
	r.ReportRequest(end)
	l = llmOf(t, r)
	if l.InFlight != 0 || l.Current != nil || l.Retries != 1 || len(l.Attempts) != 2 {
		t.Fatalf("after the ok end: %+v", l)
	}
	a1, a2 := l.Attempts[0], l.Attempts[1]
	if a1.N != 1 || a1.Outcome != model.LLMOutcomeTruncated || a1.CompletionTokens != 4000 || a1.MaxTokens != 4000 ||
		a1.StartedAt != stamp(t0) || a1.EndedAt != stamp(t1) || a1.Phase != "" || a1.ReasoningTokens != nil {
		t.Fatalf("first attempt: %+v", a1)
	}
	if a2.N != 2 || a2.Outcome != model.LLMOutcomeOK || a2.CompletionTokens != 6000 || a2.MaxTokens != 8000 ||
		a2.StartedAt != stamp(t2) || a2.EndedAt != stamp(t3) || a2.ReasoningTokens == nil || *a2.ReasoningTokens != 1500 {
		t.Fatalf("second attempt: %+v", a2)
	}
	if st, _ := r.statsSnapshot(); st.LastActivityAt != stamp(t3) {
		t.Fatalf("last activity = %q, want %q", st.LastActivityAt, stamp(t3))
	}
	// The job ends: nothing in flight, the attempts stay.
	if s := settledLLM(&l); s == nil || s.InFlight != 0 || s.Current != nil || len(s.Attempts) != 2 || s.Retries != 1 {
		t.Fatalf("settled: %+v", s)
	}
}

func TestLLMStatsBackoffAfter429(t *testing.T) {
	c := newTestClock()
	r := bareRun(c)
	r.ReportRequest(reqStart(c.now(), "c1", 1))
	end := reqEnd(c.advance(2*time.Second), "c1", 1, model.LLMOutcomeHTTPError, 0, 0)
	end.Status = 429
	r.ReportRequest(end)
	tb := c.advance(10 * time.Millisecond)
	r.ReportRequest(RequestEvent{Phase: RequestBackoff, At: tb, CallID: "c1", N: 1, RetryIn: 12 * time.Second})
	l := llmOf(t, r)
	if l.InFlight != 0 || l.Current == nil || l.Current.N != 1 || l.Current.RetryAt != stamp(tb.Add(12*time.Second)) {
		t.Fatalf("waiting out the backoff: %+v current=%+v", l, l.Current)
	}
	if len(l.Attempts) != 1 || l.Attempts[0].Status != 429 || l.Attempts[0].Outcome != model.LLMOutcomeHTTPError || l.Attempts[0].RetryAt != "" {
		t.Fatalf("attempts: %+v", l.Attempts)
	}
	r.ReportRequest(reqStart(c.advance(12*time.Second), "c1", 2))
	l = llmOf(t, r)
	if l.InFlight != 1 || l.Current == nil || l.Current.N != 2 || l.Current.RetryAt != "" || l.Retries != 1 {
		t.Fatalf("second attempt: %+v current=%+v", l, l.Current)
	}
	// A cancel during a backoff: no next start; once well past its retry
	// time the waiting call is no longer shown as current.
	r.ReportRequest(reqEnd(c.advance(time.Second), "c1", 2, model.LLMOutcomeOK, 10, 10))
	r.ReportRequest(reqStart(c.now(), "c2", 1))
	e2 := reqEnd(c.advance(time.Second), "c2", 1, model.LLMOutcomeNetwork, 0, 0)
	r.ReportRequest(e2)
	r.ReportRequest(RequestEvent{Phase: RequestBackoff, At: c.now(), CallID: "c2", N: 1, RetryIn: 10 * time.Second})
	if l = llmOf(t, r); l.Current == nil || l.Current.CallID != "c2" {
		t.Fatalf("c2 backoff: %+v", l.Current)
	}
	c.advance(10*time.Second + llmBackoffGrace + time.Second)
	r.ReportRequest(RequestEvent{Phase: RequestPhase, At: c.now(), CallID: "zz", StreamPhase: model.LLMPhaseWriting})
	if l = llmOf(t, r); l.Current != nil {
		t.Fatalf("abandoned backoff still current: %+v", l.Current)
	}
}

func TestLLMStatsManyConcurrentRequests(t *testing.T) {
	c := newTestClock()
	r := bareRun(c)
	const n = 60
	for i := 0; i < n; i++ {
		r.ReportRequest(reqStart(c.advance(time.Millisecond), fmt.Sprintf("c%02d", i), 1))
	}
	if l := llmOf(t, r); l.InFlight != n || l.Current == nil || l.Current.N != 1 || len(l.Attempts) != 0 {
		t.Fatalf("all in flight: in_flight=%d current=%+v attempts=%d", l.InFlight, l.Current, len(l.Attempts))
	}
	// Every fifth call times out and is tried again; the others end ok.
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%02d", i)
		if i%5 == 0 {
			r.ReportRequest(reqEnd(c.advance(time.Millisecond), id, 1, model.LLMOutcomeTimeout, 0, 0))
			r.ReportRequest(reqStart(c.advance(time.Millisecond), id, 2))
		} else {
			r.ReportRequest(reqEnd(c.advance(time.Millisecond), id, 1, model.LLMOutcomeOK, 100, 50))
		}
	}
	l := llmOf(t, r)
	if l.InFlight != n/5 || l.Current == nil || l.Current.N != 2 || l.Retries != n/5 {
		t.Fatalf("retries in flight: in_flight=%d current=%+v retries=%d", l.InFlight, l.Current, l.Retries)
	}
	for i := 0; i < n; i += 5 {
		r.ReportRequest(reqEnd(c.advance(time.Millisecond), fmt.Sprintf("c%02d", i), 2, model.LLMOutcomeOK, 100, 50))
	}
	l = llmOf(t, r)
	if l.InFlight != 0 || l.Current != nil || len(l.Attempts) != model.LLMAttemptsKept {
		t.Fatalf("after all: in_flight=%d current=%+v attempts=%d", l.InFlight, l.Current, len(l.Attempts))
	}
	for _, a := range l.Attempts {
		if a.Outcome == model.LLMOutcomeOK && a.N < 2 {
			t.Fatalf("an unremarkable attempt was kept: %+v", a)
		}
	}
	// The newest are kept: the last one is the final retry's success.
	if last := l.Attempts[len(l.Attempts)-1]; last.CallID != fmt.Sprintf("c%02d", n-5) || last.N != 2 {
		t.Fatalf("last kept attempt: %+v", last)
	}
	if len(r.calls) > llmCallsPruneAt+n/5 {
		t.Fatalf("%d calls still tracked", len(r.calls))
	}
}

func TestLLMStatsNotChangedByUpdateStats(t *testing.T) {
	c := newTestClock()
	r := bareRun(c)
	r.ReportRequest(reqStart(c.now(), "c1", 1))
	r.ReportRequest(reqEnd(c.advance(time.Second), "c1", 1, model.LLMOutcomeTruncated, 1, 2))
	r.ReportRequest(reqStart(c.now(), "c1", 2))
	before := llmOf(t, r)
	tu := c.advance(5 * time.Second)
	r.Update(func(st *model.JobStats) {
		st.Note = "x"
		st.LLM.Attempts[0].Model = "changed"
		st.LLM.Current.Model = "changed"
		st.LLM.Retries = 99
		st.LastActivityAt = "2000-01-01T00:00:00Z"
	})
	st, _ := r.statsSnapshot()
	if st.Note != "x" || st.LLM == nil || st.LLM.Retries != before.Retries || st.LLM.Attempts[0].Model != "m-large" ||
		st.LLM.Current.Model != "m-large" {
		t.Fatalf("stats after Update: %+v llm=%+v", st, st.LLM)
	}
	if st.LastActivityAt != stamp(tu) {
		t.Fatalf("an Update is a sign of life: %q, want %q", st.LastActivityAt, stamp(tu))
	}
	r.Update(func(st *model.JobStats) { st.LLM = nil })
	if st, _ = r.statsSnapshot(); st.LLM == nil || st.LLM.InFlight != 1 {
		t.Fatalf("Update cleared stats.llm: %+v", st.LLM)
	}
	// Usage and progress also refresh the last activity.
	ta := c.advance(3 * time.Second)
	r.AddUsage(1, 1, 0)
	if st, _ = r.statsSnapshot(); st.LastActivityAt != stamp(ta) {
		t.Fatalf("usage: %q", st.LastActivityAt)
	}
	tp := c.advance(3 * time.Second)
	r.progress(0.5)
	if st, _ = r.statsSnapshot(); st.LastActivityAt != stamp(tp) {
		t.Fatalf("progress: %q", st.LastActivityAt)
	}
}

// Through the scheduler: the attempts are persisted and outlive a restart;
// a run that starts clears what a crashed run left in flight; a job whose
// stats change without progress is announced.
func TestLLMStatsPersistedAcrossRestart(t *testing.T) {
	inFlight := make(chan struct{})
	terms := &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		now := time.Now()
		ReportRequest(ctx, reqStart(now, "c1", 1))
		ReportRequest(ctx, reqEnd(now, "c1", 1, model.LLMOutcomeTruncated, 100, 400))
		ReportRequest(ctx, reqStart(now, "c1", 2))
		close(inFlight)
		<-ctx.Done()
		return ctx.Err()
	}}
	e := newEnv(t, nil, instant(model.StageMaterialExtract, ResourceCPU), terms)
	m := e.material()
	tj := func() model.Job { return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms) }
	e.start()
	<-inFlight
	// Flushed while running, and announced although the progress never moved.
	e.waitFor("stats.llm flushed", 5*time.Second, func() bool {
		l := tj().Stats.LLM
		return l != nil && l.InFlight == 1 && l.Current != nil && l.Current.N == 2 && len(l.Attempts) == 1
	})
	e.waitFor("job event with stats.llm", 5*time.Second, func() bool {
		e.ev.mu.Lock()
		defer e.ev.mu.Unlock()
		for _, j := range e.ev.jobs {
			if j.ID == tj().ID && j.Status == model.StatusRunning && j.Stats.LLM != nil && j.Stats.LLM.InFlight == 1 && j.Stats.LastActivityAt != "" {
				return true
			}
		}
		return false
	})
	e.s.Stop()
	j := tj()
	if j.Status != model.StatusRunning || j.Stats.LLM == nil || j.Stats.LLM.InFlight != 0 || j.Stats.LLM.Current != nil ||
		len(j.Stats.LLM.Attempts) != 1 || j.Stats.LLM.Retries != 1 {
		t.Fatalf("after Stop: %s %+v", j.Status, j.Stats.LLM)
	}
	// A crash would have left the attempt in flight in the database.
	st := j.Stats
	st.LLM = &model.LLMStats{InFlight: 1, Current: &model.LLMAttempt{CallID: "c1", N: 2, Model: "m"}, Attempts: st.LLM.Attempts, Retries: 1}
	if err := e.st.SetJobStats(e.ctx, j.ID, st); err != nil {
		t.Fatal(err)
	}
	if err := state.Recover(e.ctx, e.st, e.dataDir); err != nil {
		t.Fatal(err)
	}
	seen := make(chan model.LLMStats, 1)
	terms2 := &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		cur, _ := JobFrom(ctx)
		if cur.Stats.LLM != nil {
			seen <- *cur.Stats.LLM
		} else {
			seen <- model.LLMStats{}
		}
		ReportRequest(ctx, reqStart(time.Now(), "c9", 1))
		ReportRequest(ctx, reqEnd(time.Now(), "c9", 1, model.LLMOutcomeOK, 1, 1))
		return nil
	}}
	e.s = e.newScheduler(instant(model.StageMaterialExtract, ResourceCPU), terms2)
	e.start()
	j = e.waitStatusScope(model.ScopeMaterial, m.ID, model.StageMaterialTerms, model.StatusDone)
	got := <-seen
	if len(got.Attempts) != 1 || got.Retries != 1 {
		t.Fatalf("the restarted run saw %+v", got)
	}
	l := j.Stats.LLM
	if l == nil || l.InFlight != 0 || l.Current != nil || len(l.Attempts) != 1 || l.Attempts[0].Outcome != model.LLMOutcomeTruncated || l.Retries != 1 {
		t.Fatalf("done: %+v", l)
	}
	if j.Stats.LastActivityAt == "" {
		t.Fatal("no last activity")
	}
}

func TestLLMStatsJobEventWithoutProgress(t *testing.T) {
	reported := make(chan struct{})
	release := make(chan struct{})
	terms := &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		p(0.25)
		<-release
		ReportUsage(ctx, 700, 70, 0) // only the stats change
		close(reported)
		<-ctx.Done()
		return errors.New("stopped")
	}}
	e := newEnv(t, nil, instant(model.StageMaterialExtract, ResourceCPU), terms)
	m := e.material()
	tj := func() model.Job { return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms) }
	e.start()
	e.waitFor("progress flushed", 5*time.Second, func() bool { return tj().Progress == 0.25 })
	close(release)
	<-reported
	e.waitFor("job event with the new tokens", 5*time.Second, func() bool {
		e.ev.mu.Lock()
		defer e.ev.mu.Unlock()
		for _, j := range e.ev.jobs {
			if j.ID == tj().ID && j.Status == model.StatusRunning && j.Progress == 0.25 && j.Stats.PromptTokens == 700 {
				return true
			}
		}
		return false
	})
}

func (e *env) material() model.Material {
	e.t.Helper()
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "a.pdf", Path: "p"}
	if err := e.st.CreateMaterial(e.ctx, m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) waitStatusScope(scope, id, stage, status string) model.Job {
	e.t.Helper()
	var j model.Job
	e.waitFor(fmt.Sprintf("%s %s=%s", id, stage, status), 5*time.Second, func() bool {
		j = e.job(scope, id, stage)
		return j.Status == status
	})
	return j
}

// R8 BC-01 (T25 S4): requests in flight when the window closes (or the job
// is canceled) end "canceled" without a token. They are not attempts worth
// showing: closing the app is no failure, and a restart must not show a row
// of "1st attempt · canceled". A canceled attempt that was billed, and every
// other outcome that is not ok, are still kept.
func TestLLMStatsCanceledWithoutTokensNotKept(t *testing.T) {
	c := newTestClock()
	r := bareRun(c)
	for _, id := range []string{"c1", "c2", "c3"} {
		r.ReportRequest(reqStart(c.advance(time.Millisecond), id, 1))
	}
	if l := llmOf(t, r); l.InFlight != 3 {
		t.Fatalf("in flight: %+v", l)
	}
	// R84 L-2: dropping a canceled call leaves the others as they are.
	r.ReportRequest(reqEnd(c.advance(time.Millisecond), "c1", 1, model.LLMOutcomeCanceled, 0, 0))
	if l := llmOf(t, r); l.InFlight != 2 || l.Current == nil || l.Current.CallID != "c3" || len(r.calls) != 2 || len(l.Attempts) != 0 {
		t.Fatalf("after canceling c1: %+v current=%+v calls=%d", l, l.Current, len(r.calls))
	}
	r.ReportRequest(reqEnd(c.advance(time.Millisecond), "c3", 1, model.LLMOutcomeCanceled, 0, 0))
	if l := llmOf(t, r); l.InFlight != 1 || l.Current == nil || l.Current.CallID != "c2" || len(r.calls) != 1 {
		t.Fatalf("after canceling c3: %+v current=%+v calls=%d", l, l.Current, len(r.calls))
	}
	r.ReportRequest(reqEnd(c.advance(time.Millisecond), "c2", 1, model.LLMOutcomeCanceled, 0, 0))
	l := llmOf(t, r)
	if len(l.Attempts) != 0 || l.InFlight != 0 || l.Current != nil || l.Retries != 0 {
		t.Fatalf("canceled without tokens kept: %+v current=%+v", l, l.Current)
	}
	if len(r.calls) != 0 {
		t.Fatalf("%d canceled calls still tracked", len(r.calls))
	}
	if st, _ := r.statsSnapshot(); st.LLM != nil {
		t.Fatalf("stats.llm = %+v, want none", st.LLM)
	}

	// A retry canceled before it got a token: the earlier failure stays,
	// the canceled retry is not added; the retry was sent, so it counts.
	r.ReportRequest(reqStart(c.advance(time.Millisecond), "c4", 1))
	r.ReportRequest(reqEnd(c.advance(time.Millisecond), "c4", 1, model.LLMOutcomeTimeout, 0, 0))
	r.ReportRequest(reqStart(c.advance(time.Millisecond), "c4", 2))
	r.ReportRequest(reqEnd(c.advance(time.Millisecond), "c4", 2, model.LLMOutcomeCanceled, 0, 0))
	l = llmOf(t, r)
	if len(l.Attempts) != 1 || l.Attempts[0].Outcome != model.LLMOutcomeTimeout || l.Retries != 1 || l.InFlight != 0 || l.Current != nil {
		t.Fatalf("canceled retry: %+v current=%+v", l, l.Current)
	}

	// Canceled after tokens arrived (the provider bills them): kept, with
	// its tokens, whichever kind they are.
	billed := []func(*RequestEvent){
		func(ev *RequestEvent) { ev.PromptTokens = 900 },
		func(ev *RequestEvent) { ev.CompletionTokens = 40 },
		func(ev *RequestEvent) { ev.CacheHitTokens = 512 },
		func(ev *RequestEvent) { v := int64(64); ev.ReasoningTokens = &v },
	}
	for i, set := range billed {
		id := fmt.Sprintf("b%d", i)
		r.ReportRequest(reqStart(c.advance(time.Millisecond), id, 1))
		end := reqEnd(c.advance(time.Millisecond), id, 1, model.LLMOutcomeCanceled, 0, 0)
		set(&end)
		r.ReportRequest(end)
		l = llmOf(t, r)
		last := l.Attempts[len(l.Attempts)-1]
		if len(l.Attempts) != 2+i || last.CallID != id || last.Outcome != model.LLMOutcomeCanceled {
			t.Fatalf("billed cancel %d not kept: %+v", i, l.Attempts)
		}
	}
	if a := l.Attempts[1]; a.PromptTokens != 900 {
		t.Fatalf("billed cancel lost its tokens: %+v", a)
	}

	// Failures are kept as before, and so is a later attempt that ended ok.
	r.ReportRequest(reqStart(c.advance(time.Millisecond), "f1", 1))
	r.ReportRequest(reqEnd(c.advance(time.Millisecond), "f1", 1, model.LLMOutcomeHTTPError, 0, 0))
	r.ReportRequest(reqStart(c.advance(time.Millisecond), "f1", 2))
	r.ReportRequest(reqEnd(c.advance(time.Millisecond), "f1", 2, model.LLMOutcomeOK, 10, 10))
	l = llmOf(t, r)
	n := len(l.Attempts)
	if n != 7 || l.Attempts[n-2].Outcome != model.LLMOutcomeHTTPError || l.Attempts[n-1].N != 2 || l.Attempts[n-1].Outcome != model.LLMOutcomeOK ||
		l.Retries != 2 {
		t.Fatalf("failure and ok retry: %+v", l)
	}
}

// R8 BC-01 through the scheduler: the window closes (Stop) while a stage
// has requests in flight. What is persisted and what the next run starts
// with holds no canceled attempt; the usage counters are unchanged.
func TestLLMStatsStopDoesNotPersistCanceled(t *testing.T) {
	inFlight := make(chan struct{})
	terms := &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		ReportUsage(ctx, 300, 30, 0)
		for _, call := range []string{"c1", "c2", "c3"} {
			ReportRequest(ctx, reqStart(time.Now(), call, 1))
		}
		close(inFlight)
		<-ctx.Done()
		// what the llm client reports for each request the cancel cut short
		for _, call := range []string{"c1", "c2", "c3"} {
			ReportRequest(ctx, reqEnd(time.Now(), call, 1, model.LLMOutcomeCanceled, 0, 0))
		}
		return ctx.Err()
	}}
	e := newEnv(t, nil, instant(model.StageMaterialExtract, ResourceCPU), terms)
	m := e.material()
	tj := func() model.Job { return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms) }
	e.start()
	<-inFlight
	e.waitFor("stats.llm flushed", 5*time.Second, func() bool {
		l := tj().Stats.LLM
		return l != nil && l.InFlight == 3
	})
	e.s.Stop()
	j := tj()
	if j.Stats.LLM != nil {
		t.Fatalf("after Stop: stats.llm = %+v attempts=%+v", j.Stats.LLM, j.Stats.LLM.Attempts)
	}
	if j.Stats.PromptTokens != 300 || j.Stats.CompletionTokens != 30 {
		t.Fatalf("usage after Stop: %+v", j.Stats)
	}
}

// An activity event of a streamed attempt (SEC4 L7) moves the run's last
// activity and nothing of job.stats.llm.
func TestLLMStatsActivityOnlyTouches(t *testing.T) {
	c := newTestClock()
	r := bareRun(c)
	t0 := c.now()
	r.ReportRequest(reqStart(t0, "c1", 1))
	before := llmOf(t, r)
	t1 := c.advance(5 * time.Minute)
	r.ReportRequest(RequestEvent{Phase: RequestActivity, At: t1, CallID: "c1", N: 1})
	st, _ := r.statsSnapshot()
	if st.LastActivityAt != stamp(t1) {
		t.Fatalf("last activity = %q, want %q", st.LastActivityAt, stamp(t1))
	}
	after := llmOf(t, r)
	if after.InFlight != before.InFlight || after.Current == nil || *after.Current != *before.Current || len(after.Attempts) != 0 || after.Retries != 0 {
		t.Fatalf("llm stats changed: %+v current=%+v", after, after.Current)
	}
	// an activity event of a call never started does not create one
	r.ReportRequest(RequestEvent{Phase: RequestActivity, At: c.advance(time.Second), CallID: "other", N: 1})
	if l := llmOf(t, r); l.InFlight != 1 || len(r.calls) != 1 {
		t.Fatalf("calls %d, %+v", len(r.calls), l)
	}
}
