// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// refusalErr is the error the LLM client returns for a request the provider
// declined (llm refusalError): permanent, coded llm.refusal.
func refusalErr() error {
	params := map[string]any{"role": "translate", "retries": 0, "provider": "Fake", "model": "fake-model", "category": "cyber"}
	return sched.Permanent(model.Errorf(llm.CodeRefusal, params, fmt.Errorf("declined: %w", llm.ErrRefusal)))
}

// Review R1 suggestion 8 (r4 D12): the provider declined every sentence of
// the run. The stage fails with the client's llm.refusal (permanent, as the
// code says: asking again gives the same), not with "unusable output"; the
// sentences go back to stale as before, so that a later run sends them.
func TestTranslateEverySentenceRefusedFailsWithRefusal(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	ss := e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", refusalErr() }}
	r := e.run(e.stage(e.deps(fake), model.StageTranslate), l.ID)
	var ce *model.CodedError
	if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != llm.CodeRefusal || !errors.Is(r.err, llm.ErrRefusal) ||
		!sched.IsPermanent(r.err) || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	if !paramsMatch(&ce.Info, "role", "translate", "provider", "Fake", "model", "fake-model") {
		t.Errorf("params %+v", ce.Info.Params)
	}
	if n := len(fake.Calls()); n != len(ss) {
		t.Errorf("%d requests, want %d (a declined request is not asked again)", n, len(ss))
	}
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhStale {
			t.Errorf("sentence %d status %s, want stale", s.Idx, s.ZhStatus)
		}
	}
}

// Some sentences declined, the others unusable: not a refusal of the whole
// run, so the error stays translate.sentences_failed (retryable).
func TestTranslateRefusedAndUnusableFailsAsBefore(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	ss := e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	fake := &scriptLLM{fn: func(_ context.Context, n int, _ llmCall) (string, error) {
		if n == 0 {
			return "", refusalErr()
		}
		return "I cannot do that.", nil
	}}
	r := e.run(e.stage(e.deps(fake), model.StageTranslate), l.ID)
	var ce *model.CodedError
	if r.err == nil || sched.IsPermanent(r.err) || !errors.As(r.err, &ce) || ce.Info.Code != CodeTranslateFailed ||
		!paramsMatch(&ce.Info, "n", len(ss)) || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
}

// Review R51 (r4 D12) on the batch path (v3.2.x P2): every batch and every
// single request is declined. The chunks are split down to single
// sentences (translateOne), each declined once; the stage fails with the
// permanent llm.refusal and every sentence goes back to stale.
func TestTranslateBatchEverySentenceRefusedFailsWithRefusal(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	d := e.claudeDeps(l.ID, "claude-sonnet-5-5")
	e.setTranslateBatch(l.ID, 4)
	ss := e.sentences(l.ID)
	e.fake.FailNextBatch(200, llmtest.FailRefusal)
	e.fake.FailNext(llmtest.RoleTranslate, 200, llmtest.FailRefusal)
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	var ce *model.CodedError
	if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != llm.CodeRefusal || !errors.Is(r.err, llm.ErrRefusal) ||
		!sched.IsPermanent(r.err) || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	if len(e.fake.BatchRequests()) == 0 {
		t.Fatal("no batch request: not the batch path")
	}
	singles := map[int]int{}
	for _, q := range e.fake.Requests(llmtest.RoleTranslate) {
		if !q.Batch {
			singles[q.TargetIdx]++
		}
	}
	if len(singles) != len(ss) {
		t.Errorf("single requests %v, want each of the %d sentences", singles, len(ss))
	}
	for idx, n := range singles {
		if n != 1 {
			t.Errorf("sentence %d asked %d times alone (a declined request is not asked again)", idx, n)
		}
	}
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhStale {
			t.Errorf("sentence %d status %s, want stale", s.Idx, s.ZhStatus)
		}
	}
}
