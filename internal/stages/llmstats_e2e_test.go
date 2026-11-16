// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"net/http"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// v3.2.x P1-c end to end: the HTTP client's request events reach the real
// scheduler's run (its RequestSink), which keeps job.stats.llm. A summary
// request answered HTTP 500 once and then 200 leaves, on the finished job,
// one retry and both attempts (the failed one with its status, the second
// ok with its tokens); nothing is in flight.
func TestRequestEventsReachJobStats(t *testing.T) {
	e := newEnv(t)
	var s *sched.Scheduler
	d := e.deps(nil)
	d.Invalidate = func(ctx context.Context, courseID string, cause state.StaleCause) error {
		return s.InvalidateCourseWith(ctx, courseID, cause)
	}
	d.GPUTotalMB = func(ctx context.Context) int { return s.GPUStatus(ctx).TotalMB }
	s, err := sched.New(sched.Options{
		Store: e.st, Config: e.cfg, Events: e.ev, DataDir: e.cfg.DataDir,
		GPUQuery: func(context.Context) (sched.GPUMem, error) {
			return sched.GPUMem{FreeMB: 20000, TotalMB: 24000, UsedMB: 1000}, nil
		},
		ScanInterval: 5 * time.Millisecond, ProgressMinInterval: 5 * time.Millisecond, IncrementalDebounce: 20 * time.Millisecond,
		RetryBackoff: []time.Duration{20 * time.Millisecond, 40 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := llm.New(e.cfg, s.Pool(), "", nil)
	client.Backoff = []time.Duration{time.Millisecond}
	d.LLM = client
	for _, stg := range All(d) {
		s.Register(stg)
	}

	l := e.lecture("Lecture 1", "m.mp4")
	e.insertSample(l.ID)
	e.markDone(model.ScopeLecture, l.ID, model.StagePrep)
	e.markDone(model.ScopeLecture, l.ID, model.StageASR)
	if j := e.job(model.ScopeLecture, l.ID, model.StageSummary); j.Status == model.StatusPending {
		e.must(state.TransitionJob(e.ctx, e.st, j.ID, model.StatusPending, model.StatusQueued, db.JobFinish{}))
	}
	e.fake.FailNext(llmtest.RoleSummary, 1, llmtest.FailHTTP500)

	e.must(s.Start(e.ctx))
	t.Cleanup(s.Stop)
	waitUntil(t, e, l.ID, 60*time.Second, func() bool {
		st := e.job(model.ScopeLecture, l.ID, model.StageSummary).Status
		return st == model.StatusDone || st == model.StatusFailed
	})
	j := e.job(model.ScopeLecture, l.ID, model.StageSummary)
	if j.Status != model.StatusDone {
		t.Fatalf("summary %s: %v", j.Status, j.Error)
	}
	ls := j.Stats.LLM
	if ls == nil {
		t.Fatalf("no stats.llm: %+v", j.Stats)
	}
	if ls.InFlight != 0 || ls.Current != nil || ls.Retries != 1 || len(ls.Attempts) != 2 {
		t.Fatalf("stats.llm %+v", ls)
	}
	a1, a2 := ls.Attempts[0], ls.Attempts[1]
	if a1.N != 1 || a1.Outcome != model.LLMOutcomeHTTPError || a1.Status != http.StatusInternalServerError || a1.CallID == "" ||
		a1.Role != model.RoleSummary || a1.StartedAt == "" || a1.EndedAt == "" || a1.TimeoutMS <= 0 {
		t.Errorf("attempt 1 %+v", a1)
	}
	if a2.N != 2 || a2.Outcome != model.LLMOutcomeOK || a2.CallID != a1.CallID || a2.PromptTokens == 0 || a2.CompletionTokens == 0 {
		t.Errorf("attempt 2 %+v", a2)
	}
	if j.Stats.LastActivityAt == "" || j.Stats.PromptTokens != a2.PromptTokens {
		t.Errorf("stats %+v", j.Stats)
	}
}
