// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Every call-log result maps to its model.LLMOutcome*; a refusal is
// "refused" (R8 E-2), not bad_response.
func TestOutcomeOfEveryResult(t *testing.T) {
	for result, want := range map[string]string{
		"ok":           model.LLMOutcomeOK,
		"truncated":    model.LLMOutcomeTruncated,
		"refused":      model.LLMOutcomeRefused,
		"http_error":   model.LLMOutcomeHTTPError,
		"timeout":      model.LLMOutcomeTimeout,
		"time_limit":   model.LLMOutcomeTimeout,
		"bad_response": model.LLMOutcomeBadResponse,
		"idle":         model.LLMOutcomeIdle,
		"canceled":     model.LLMOutcomeCanceled,
		"network":      model.LLMOutcomeNetwork,

		"insufficient_resource": model.LLMOutcomeBadResponse,
	} {
		if got := outcomeOf(result); got != want {
			t.Errorf("outcomeOf(%q) = %q, want %q", result, got, want)
		}
	}
	if got := attemptResult(context.Background(), &attemptErr{kind: failRefusal}); outcomeOf(got) != model.LLMOutcomeRefused {
		t.Errorf("failRefusal: result %q, outcome %q", got, outcomeOf(got))
	}
}

// A refused streamed attempt ends with outcome refused (the stage card's
// "被拒答"; sched keeps any outcome but ok as an attempt worth showing).
func TestStreamRefusalOutcome(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.FailNext(llmtest.RoleSummary, 1, llmtest.FailRefusal)
	rec := &sched.StatsRecorder{}
	_, err := e.c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if !errors.Is(err, ErrRefusal) {
		t.Fatalf("err %v", err)
	}
	if got := eventLines(rec.Events); got != "start(1) end(1):"+model.LLMOutcomeRefused {
		t.Fatalf("events %s", got)
	}
}
