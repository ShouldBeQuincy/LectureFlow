// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"slices"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Review R18 suggestion 3 (T4): a stream still coming at its time limit
// (llm.CodeTimeLimit) is split down to single sentences; when the single
// sentence runs into it too, the stage fails with that error as on the
// single path: batches 0–3 and 0–1, sentence 0 once, nothing written and
// nothing left pending.
func TestTranslateBatchTimeLimitDownToOneSentence(t *testing.T) {
	timeLimit := sched.Permanent(model.Errorf(llm.CodeTimeLimit, map[string]any{"role": config.RoleTranslate, "retries": 0, "seconds": 240.0,
		"provider": "Claude", "model": "claude-sonnet-5-5"}, errors.New("the attempt timeout of 4m0s ran out while the answer was still streaming")))
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", timeLimit }}
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if jobErrorCode(r.job) != llm.CodeTimeLimit || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
	}
	batch, single := splitCalls(fake)
	var got [][]int
	for _, c := range batch {
		got = append(got, llmtest.TargetIdxs(c.User))
	}
	if want := [][]int{idxRange(0, 3), {0, 1}}; !slices.EqualFunc(got, want, slices.Equal) || len(single) != 1 || llmtest.TargetIdx(single[0].User) != 0 {
		t.Errorf("batches %v and %d single requests, want %v and sentence 0 once", got, len(single), want)
	}
	if r.stats.PendingSentences != 0 {
		t.Errorf("pending %d", r.stats.PendingSentences)
	}
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhNone {
			t.Errorf("sentence %d is %s", s.Idx, s.ZhStatus)
		}
	}
}
