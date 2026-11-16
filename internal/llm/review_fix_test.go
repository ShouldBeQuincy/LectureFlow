// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// Review fixes of v3.2.x P1: an object answer wrapped in "[...]" is used
// again (as before P1), whether or not text comes first; "{ }" in the tail
// is not a second answer, like "{}".
func TestParseJSONArrayWrappedObjectAndEmptyTail(t *testing.T) {
	type out struct {
		Idx int    `json:"idx"`
		Zh  string `json:"zh"`
	}
	first := `{"idx": 3, "zh": "好"}`
	for _, c := range []struct {
		name, text string
		want       error // nil = {3, 好} is used
	}{
		{"wrapped in an array", "[" + first + "]", nil},
		{"wrapped, compact", `[{"idx":3,"zh":"好"}]`, nil},
		{"wrapped, fenced", "```json\n[" + first + "]\n```", nil},
		{"wrapped, text before", "Here: [" + first + "]", nil},
		{"wrapped, text after", "[" + first + "]\nDone.", nil},
		{"wrapped, two equal objects", "[" + first + ", " + first + "]", nil},
		// Two different objects are two answers, with or without the "[".
		{"wrapped, two different objects", "[" + first + `, {"idx": 4, "zh": "坏"}]`, ErrSecondJSON},
		{"empty object with spaces in the tail", first + " { }", nil},
		{"empty object with a newline in the tail", first + "\n{\n}\n", nil},
	} {
		var v out
		err := ParseJSON(c.text, &v)
		if c.want == nil {
			if err != nil || v != (out{3, "好"}) {
				t.Errorf("%s: %+v, err %v", c.name, v, err)
			}
			continue
		}
		if !errors.Is(err, c.want) || !errors.Is(err, ErrBadJSON) {
			t.Errorf("%s: err %v, want %v", c.name, err, c.want)
		}
	}
	// An answer that is an array keeps its meaning where an array is wanted,
	// and "[ ]" after it is not a second answer.
	var xs []out
	if err := ParseJSON("["+first+"]\n[ ]", &xs); err != nil || len(xs) != 1 || xs[0] != (out{3, "好"}) {
		t.Errorf("array answer: %+v, err %v", xs, err)
	}
	var anyV any
	if err := ParseJSON("["+first+"] trailing words", &anyV); err != nil {
		t.Errorf("array into any: %v", err)
	} else if a, ok := anyV.([]any); !ok || len(a) != 1 {
		t.Errorf("array into any: %#v", anyV)
	}
}

// Review fix of v3.2.x D1: the timeout of one attempt follows the whole
// max_tokens on a current Claude model and for the user's value (not only
// the thinking headroom); the default limits elsewhere keep the old
// timeout (base + budget).
func TestAttemptTimeoutFollowsMaxTokens(t *testing.T) {
	per := ThinkingTimeoutPerToken
	if got := attemptTimeout(DefaultTimeout, 16000, 0, true); got != DefaultTimeout+12000*per {
		t.Errorf("syllabus, thinking off: %v", got)
	}
	if got := attemptTimeout(DefaultTimeout, 2000+24000, 24000, true); got != DefaultTimeout+24000*per {
		t.Errorf("translate, high: %v", got)
	}
	if got := attemptTimeout(DefaultTimeout, 2000, 0, true); got != DefaultTimeout {
		t.Errorf("translate, off: %v", got)
	}
	if got := attemptTimeout(DefaultTimeout, DeepSeekDefaultMaxTokens+ThinkingBudgetLow, ThinkingBudgetLow, false); got != thinkingTimeout(DefaultTimeout, ThinkingBudgetLow) {
		t.Errorf("not scaled: %v", got)
	}

	e := newThinkingEnv(t, okAnswers)
	claude := e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
	deepseek := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone) // redirected to the test server
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		provider model.Provider
		model    string
		role     string
		params   model.RoleParams
		want     time.Duration
	}{
		{"sonnet off, syllabus", claude, "claude-sonnet-5-5", model.RoleSyllabusFormat, model.RoleParams{}, DefaultTimeout + 12000*per},
		{"sonnet off, glossary", claude, "claude-sonnet-5-5", model.RoleGlossary, model.RoleParams{}, DefaultTimeout + 4000*per},
		{"sonnet off, translate", claude, "claude-sonnet-5-5", model.RoleTranslate, model.RoleParams{}, DefaultTimeout},
		{"sonnet high, summary", claude, "claude-sonnet-5-5", model.RoleSummary, model.RoleParams{Thinking: sp(model.ThinkingHigh)}, DefaultTimeout + 24000*per},
		{"sonnet off, override 64000", claude, "claude-sonnet-5-5", model.RoleRepair, model.RoleParams{MaxTokens: ip(64000)}, DefaultTimeout + 60000*per},
		{"older Claude, default", claude, "claude-haiku-4-5", model.RoleRepair, model.RoleParams{}, DefaultTimeout},
		{"older Claude, override 20000", claude, "claude-haiku-4-5", model.RoleRepair, model.RoleParams{MaxTokens: ip(20000)}, DefaultTimeout + 16000*per},
		{"DeepSeek low, default", deepseek, "deepseek-flash", model.RoleSummary, model.RoleParams{Thinking: sp(model.ThinkingLow)}, DefaultTimeout + ThinkingBudgetLow*per},
		{"DeepSeek off, override 12000", deepseek, "deepseek-flash", model.RoleSummary, model.RoleParams{MaxTokens: ip(12000)}, DefaultTimeout + 8000*per},
	} {
		snap := model.LLMSnapshot{ProviderID: tc.provider.ID, ProviderName: tc.provider.Name, Model: tc.model, Params: tc.params}
		tg, err := e.c.resolve(WithTarget(ctx, snap), tc.role)
		if err != nil {
			t.Fatal(err)
		}
		maxTokens, budget := e.c.outputLimit(tg, tc.role)
		if got := e.c.requestTimeout(tg, tc.role, maxTokens, budget); got != tc.want {
			t.Errorf("%s: timeout %v (max_tokens %d, budget %d), want %v", tc.name, got, maxTokens, budget, tc.want)
		}
	}
}

// Review R11 B1 (v3.2.x P2): a translate request with more than one target
// sentence gets at least base + (budget + batch × 300) × 40 ms, the batch
// capped at settings.MaxBatch, on every provider; batch ≤ 1 and every other
// role keep exactly the timeout they had.
func TestBatchTimeout(t *testing.T) {
	per := ThinkingTimeoutPerToken
	e := newThinkingEnv(t, okAnswers)
	claude := e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
	deepseek := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone) // redirected to the test server
	ctx := context.Background()
	timeout := func(p model.Provider, mod, role string, params model.RoleParams) time.Duration {
		t.Helper()
		snap := model.LLMSnapshot{ProviderID: p.ID, ProviderName: p.Name, Model: mod, Params: params}
		tg, err := e.c.resolve(WithTarget(ctx, snap), role)
		if err != nil {
			t.Fatal(err)
		}
		maxTokens, budget := e.c.outputLimit(tg, role)
		return e.c.requestTimeout(tg, role, maxTokens, budget)
	}
	for _, tc := range []struct {
		name     string
		provider model.Provider
		model    string
		params   model.RoleParams
		want     time.Duration
	}{
		{"DeepSeek off, batch 2", deepseek, "deepseek-flash", model.RoleParams{Batch: ip(2)}, DefaultTimeout + 600*per},
		{"DeepSeek off, batch 10", deepseek, "deepseek-flash", model.RoleParams{Batch: ip(10)}, DefaultTimeout + 3000*per},
		{"DeepSeek off, batch 20", deepseek, "deepseek-flash", model.RoleParams{Batch: ip(20)}, DefaultTimeout + 6000*per},
		{"DeepSeek off, batch 50 (capped at 20)", deepseek, "deepseek-flash", model.RoleParams{Batch: ip(50)}, DefaultTimeout + 6000*per},
		{"DeepSeek low, batch 20", deepseek, "deepseek-flash", model.RoleParams{Batch: ip(20), Thinking: sp(model.ThinkingLow)}, DefaultTimeout + (ThinkingBudgetLow+6000)*per},
		{"older Claude, batch 20", claude, "claude-haiku-4-5", model.RoleParams{Batch: ip(20)}, DefaultTimeout + 6000*per},
		{"sonnet off, batch 20", claude, "claude-sonnet-5-5", model.RoleParams{Batch: ip(20)}, DefaultTimeout + 6000*per},
		// the larger of the scaled timeout and the batch's: here the batch's
		// (120 s + 27000 × 40 ms against 120 s + 26000 × 40 ms)
		{"sonnet high, batch 10", claude, "claude-sonnet-5-5", model.RoleParams{Batch: ip(10), Thinking: sp(model.ThinkingHigh)}, DefaultTimeout + (24000+3000)*per},
		// and here the scaled one (16000 above the free 4000)
		{"DeepSeek off, batch 20, override 20000", deepseek, "deepseek-flash", model.RoleParams{Batch: ip(20), MaxTokens: ip(20000)}, DefaultTimeout + 16000*per},
	} {
		if got := timeout(tc.provider, tc.model, model.RoleTranslate, tc.params); got != tc.want {
			t.Errorf("%s: timeout %v, want %v", tc.name, got, tc.want)
		}
	}
	// batch ≤ 1, and every role other than translate: unchanged
	for _, p := range []struct {
		provider model.Provider
		model    string
	}{{deepseek, "deepseek-flash"}, {claude, "claude-haiku-4-5"}, {claude, "claude-sonnet-5-5"}} {
		for _, level := range []string{"", model.ThinkingLow} {
			base := model.RoleParams{}
			if level != "" {
				base.Thinking = sp(level)
			}
			want := timeout(p.provider, p.model, model.RoleTranslate, base)
			for _, b := range []*int{ip(1), ip(0), ip(-3)} {
				params := base
				params.Batch = b
				if got := timeout(p.provider, p.model, model.RoleTranslate, params); got != want {
					t.Errorf("%s level %q batch %d: timeout %v, want %v as without a batch", p.model, level, *b, got, want)
				}
			}
			for _, role := range []string{model.RoleSummary, model.RoleRepair} {
				params := base
				params.Batch = ip(20)
				if got, w := timeout(p.provider, p.model, role, params), timeout(p.provider, p.model, role, base); got != w {
					t.Errorf("%s level %q %s batch 20: timeout %v, want %v as without a batch", p.model, level, role, got, w)
				}
			}
		}
	}
	// the batch of one is the old timeout exactly (SPEC §12 base)
	if got := timeout(deepseek, "deepseek-flash", model.RoleTranslate, model.RoleParams{Batch: ip(1)}); got != DefaultTimeout {
		t.Errorf("DeepSeek batch 1: timeout %v, want %v", got, DefaultTimeout)
	}
}

// Review fix of v3.2.x P1: an answer without any JSON is rejected with an
// error that quotes its start; the service log leaves that out (lecture
// content) and keeps the reason.
func TestReportRejectedLeavesAnswerOutOfServiceLog(t *testing.T) {
	log := captureLog(t)
	text := "Sorry, I will not translate SECRET-LECTURE-WORDS."
	var v map[string]any
	err := ParseJSON(text, &v)
	if !errors.Is(err, ErrBadJSON) || !strings.Contains(err.Error(), "SECRET-LECTURE-WORDS") {
		t.Fatalf("parse error %v", err)
	}
	ReportRejected(context.Background(), config.RoleTranslate, text, err)
	if s := log.String(); strings.Contains(s, "SECRET-LECTURE-WORDS") || !strings.Contains(s, "no JSON object") || !strings.Contains(s, "kind=not_json") {
		t.Errorf("service log:\n%s", s)
	}
}
