// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// v3.2.x D1: max_tokens = the role's override, else the role's base + the
// thinking headroom, at most the model's output limit. Every role × level ×
// kind of provider and model, with and without an override.

// sentMaxTokens returns max_tokens of a captured request (0 = absent).
func sentMaxTokens(t *testing.T, body []byte) int {
	t.Helper()
	var b struct {
		MaxTokens *int `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if b.MaxTokens == nil {
		return 0
	}
	return *b.MaxTokens
}

func TestMaxTokensTable(t *testing.T) {
	e := newThinkingEnv(t, okAnswers)
	claude := e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
	dsAnth := e.provider("DeepSeek (Anthropic)", model.ProviderAnthropic, "https://api.deepseek.com/anthropic", model.BalanceNone)
	ds := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone)
	other := e.provider("Other", model.ProviderOpenAI, "", model.BalanceNone)

	// The bases of a current Claude model (design note 3.2) and the limits
	// of appendix D2 that every other model keeps.
	base := map[string]int{
		model.RoleSyllabusFormat: 16000, model.RoleGlossary: 8000, model.RoleGlossaryJanitor: 16000, model.RoleSummary: 4000,
		model.RoleRepair: 6000, model.RoleTranslate: 2000, model.RoleChapters: 4000, model.RoleOrganizer: 4000,
	}
	legacyBase := func(role string) int {
		if role == model.RoleGlossaryJanitor {
			return 16000
		}
		return 4000
	}
	levels := []string{model.ThinkingOff, model.ThinkingLow, model.ThinkingHigh, model.ThinkingMax}
	// Headroom by level (off, low, high, max).
	adaptive := []int{0, 8000, 24000, 60000}    // max: the full headroom, the requests are streamed (v3.2.x P1-c)
	opus55 := []int{12000, 12000, 24000, 60000} // off and low: thinking cannot be switched off, effort low (C91)
	budgets := []int{0, 2048, 8192, 24576}

	type kind struct {
		name  string
		p     model.Provider
		model string
		// auto returns max_tokens without an override (0 = not sent).
		auto func(role string, level int) int
		// cap is the model's output limit (0 = none).
		cap int
	}
	kinds := []kind{
		{"sonnet-5-5", claude, "claude-sonnet-5-5", func(r string, l int) int { return base[r] + adaptive[l] }, 128000},
		{"opus-5-5", claude, "claude-opus-5-5", func(r string, l int) int { return base[r] + opus55[l] }, 128000},
		{"opus-5-5 dated", claude, "claude-opus-5-5-20260901", func(r string, l int) int { return base[r] + opus55[l] }, 128000},
		{"another current Claude", claude, "claude-sonnet-5", func(r string, l int) int { return base[r] + adaptive[l] }, 128000},
		{"Fable (cannot switch thinking off)", claude, "claude-fable-5", func(r string, l int) int { return base[r] + opus55[l] }, 128000},
		{"older Claude", claude, "claude-haiku-4-5", func(r string, l int) int { return legacyBase(r) + budgets[l] }, 0},
		{"Anthropic endpoint, not Claude", dsAnth, "deepseek-flash", func(r string, l int) int { return legacyBase(r) + budgets[l] }, 0},
		{"DeepSeek", ds, "deepseek-flash", func(r string, l int) int {
			n := 0
			if r == model.RoleGlossaryJanitor {
				n = DeepSeekJanitorMaxTokens
			}
			if l > 0 {
				if n == 0 {
					n = DeepSeekDefaultMaxTokens
				}
				n += budgets[l]
			}
			return n
		}, 0},
		// No thinking on this provider: every level is sent as off.
		{"OpenAI-compatible", other, "gpt-test", func(string, int) int { return 0 }, 0},
	}

	ctx := context.Background()
	for _, k := range kinds {
		for _, role := range model.Roles {
			for li, level := range levels {
				for _, override := range []int{0, 5000, 200000} {
					params := model.RoleParams{Thinking: sp(level)}
					want := k.auto(role, li)
					if override > 0 {
						params.MaxTokens = ip(override)
						want = override
						// The budget form needs max_tokens above budget_tokens.
						if k.p.Type == model.ProviderAnthropic && k.cap == 0 && budgets[li] >= override {
							want = override + budgets[li]
						}
						if k.cap > 0 && want > k.cap {
							want = k.cap
						}
					}
					snap := model.LLMSnapshot{ProviderID: k.p.ID, ProviderName: k.p.Name, Model: k.model, Params: params}
					name := fmt.Sprintf("%s/%s/%s/override %d", k.name, role, level, override)
					if _, err := e.c.Complete(WithTarget(ctx, snap), role, "SYS", "USR", true); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if got := sentMaxTokens(t, e.last().body); got != want {
						t.Errorf("%s: max_tokens %d, want %d", name, got, want)
					}
				}
			}
		}
	}

	// The translate role with batches (v3.2.x P2): the batch base on
	// current Claude models, at least the batch base on older Claude
	// models; a non-Claude model on an Anthropic endpoint and the
	// OpenAI-compatible providers keep their limits; batch ≤ 1 is as
	// before; the user's value wins.
	for _, tc := range []struct {
		p     model.Provider
		model string
		batch *int
		level string
		mt    *int
		want  int
	}{
		{claude, "claude-sonnet-5-5", ip(10), "", nil, 6000},
		{claude, "claude-sonnet-5-5", ip(20), model.ThinkingLow, nil, 6000 + 8000},
		{claude, "claude-sonnet-5-5", ip(1), "", nil, 2000},
		{claude, "claude-sonnet-5-5", ip(0), "", nil, 2000},
		{claude, "claude-sonnet-5-5", nil, "", nil, 2000},
		{claude, "claude-opus-5-5", ip(10), "", nil, 6000 + 12000},
		{claude, "claude-sonnet-5-5", ip(10), "", ip(3000), 3000},
		{claude, "claude-haiku-4-5", ip(10), "", nil, 6000},
		{claude, "claude-haiku-4-5", ip(10), model.ThinkingLow, nil, 6000 + 2048},
		{claude, "claude-haiku-4-5", ip(1), "", nil, 4000},
		{claude, "claude-haiku-4-5", nil, "", nil, 4000},
		{claude, "claude-haiku-4-5", ip(10), "", ip(2500), 2500},
		// older Claude models whose output limit is below the batch base:
		// the floor stops at the limit (more would be an HTTP 400)
		{claude, "claude-3-haiku-20240307", ip(10), "", nil, 4096},
		{claude, "claude-3-opus-latest", ip(10), "", nil, 4096},
		{claude, "claude-3-haiku-20240307", ip(1), "", nil, 4000},
		{claude, "claude-3-5-sonnet-20241022", ip(10), "", nil, 6000},
		{claude, "claude-3-5-haiku@20241022", ip(20), "", nil, 6000},
		{claude, "claude-3-7-sonnet-20250219", ip(10), "", nil, 6000},
		{claude, "claude-3-7-sonnet-20250219", ip(10), model.ThinkingLow, nil, 6000 + 2048},
		{claude, "claude-2.1", ip(10), "", nil, 4096},
		{claude, "claude-opus-4-1", ip(10), "", nil, 6000},
		{dsAnth, "deepseek-flash", ip(10), "", nil, 4000},
		{ds, "deepseek-flash", ip(10), "", nil, 0},
		{ds, "deepseek-flash", ip(10), model.ThinkingLow, nil, DeepSeekDefaultMaxTokens + 2048},
		{ds, "deepseek-flash", ip(10), "", ip(5000), 5000},
		{other, "gpt-test", ip(10), "", nil, 0},
	} {
		params := model.RoleParams{Batch: tc.batch, MaxTokens: tc.mt}
		if tc.level != "" {
			params.Thinking = sp(tc.level)
		}
		snap := model.LLMSnapshot{ProviderID: tc.p.ID, ProviderName: tc.p.Name, Model: tc.model, Params: params}
		if _, err := e.c.Complete(WithTarget(ctx, snap), model.RoleTranslate, "SYS", "USR", true); err != nil {
			t.Fatal(err)
		}
		if got := sentMaxTokens(t, e.last().body); got != tc.want {
			t.Errorf("%s %s batch %v level %q override %v: max_tokens %d, want %d", tc.p.Name, tc.model, tc.batch, tc.level, tc.mt, got, tc.want)
		}
		// Only the translate role looks at the batch size.
		if tc.mt == nil && tc.level == "" {
			if _, err := e.c.Complete(WithTarget(ctx, snap), model.RoleSummary, "SYS", "USR", true); err != nil {
				t.Fatal(err)
			}
			snap.Params.Batch = nil
			if _, err := e.c.Complete(WithTarget(ctx, snap), model.RoleSummary, "SYS", "USR", true); err != nil {
				t.Fatal(err)
			}
			caps := e.got()
			if a, b := sentMaxTokens(t, caps[len(caps)-2].body), sentMaxTokens(t, caps[len(caps)-1].body); a != b {
				t.Errorf("%s %s: summary max_tokens %d with batch, %d without", tc.p.Name, tc.model, a, b)
			}
		}
	}
}

// A role's own max_tokens in the job's snapshot (WithTarget) applies, and
// a value that is not positive means automatic. (The path from the
// settings, SetRole to the request, waits for line B's settings.ResolveParams.)
func TestMaxTokensOverrideFromSnapshot(t *testing.T) {
	e := newThinkingEnv(t, okAnswers)
	claude := e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
	for _, tc := range []struct {
		mt   *int
		want int
	}{{ip(3000), 3000}, {ip(0), 6000}, {ip(-5), 6000}, {nil, 6000}} {
		snap := model.LLMSnapshot{ProviderID: claude.ID, ProviderName: claude.Name, Model: "claude-sonnet-5-5", Params: model.RoleParams{MaxTokens: tc.mt}}
		if _, err := e.c.Complete(WithTarget(context.Background(), snap), model.RoleRepair, "SYS", "USR", true); err != nil {
			t.Fatal(err)
		}
		if got := sentMaxTokens(t, e.last().body); got != tc.want {
			t.Errorf("max_tokens %v: sent %d, want %d", tc.mt, got, tc.want)
		}
	}
}

// The level max holds 28000 of headroom for a request that is not
// streamed; requests to current Claude models are streamed (v3.2.x P1-c)
// and get the full 60000, with no note about the cap; the timeout grows with
// the headroom.
func TestMaxTokensMaxLevelNote(t *testing.T) {
	log := captureLog(t)
	e := newThinkingEnv(t, okAnswers)
	claude := e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
	setRole(t, e.set, model.RoleSummary, claude.ID, "claude-opus-5-5", model.RoleParams{Thinking: sp(model.ThinkingMax)})
	for range 2 {
		if _, err := e.c.Complete(context.Background(), model.RoleSummary, "SYS", "USR", false); err != nil {
			t.Fatal(err)
		}
	}
	if got := sentMaxTokens(t, e.last().body); got != BaseMaxTokensSummary+ThinkingHeadroomMax {
		t.Errorf("max_tokens %d", got)
	}
	if s := log.String(); strings.Contains(s, "not streamed") {
		t.Errorf("service log:\n%s", s)
	}
	if h, capped := thinkingHeadroom(model.ProviderAnthropic, "claude-opus-5-5", model.ThinkingMax, false); h != ThinkingHeadroomMaxUnstreamed || !capped {
		t.Errorf("unstreamed headroom %d %v", h, capped)
	}
	if h, _ := thinkingHeadroom(model.ProviderAnthropic, "claude-opus-5-5", model.ThinkingMax, true); h != ThinkingHeadroomMax {
		t.Errorf("streamed headroom %d", h)
	}
	if h, _ := thinkingHeadroom(model.ProviderOpenAI, "deepseek-flash", model.ThinkingMax, true); h != ThinkingBudgetMax {
		t.Errorf("DeepSeek headroom %d", h)
	}
	// Timeouts: the headroom of a current Claude model, the budget elsewhere.
	if got := thinkingTimeout(DefaultTimeout, ThinkingHeadroomMaxUnstreamed); got != DefaultTimeout+28000*ThinkingTimeoutPerToken {
		t.Errorf("timeout %v", got)
	}
}

// A truncated answer is retried with a doubled limit up to the model's
// output limit; at the limit it is not retried.
func TestMaxTokensTruncationStopsAtModelLimit(t *testing.T) {
	for _, tc := range []struct {
		override int
		sent     []int
	}{
		{100000, []int{100000, 128000}},
		{128000, []int{128000}},
		{300000, []int{128000}},
	} {
		e := newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) {
			io.WriteString(w, anthropicTruncated(`{"patches": [`))
		})
		claude := e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
		snap := model.LLMSnapshot{ProviderID: claude.ID, ProviderName: claude.Name, Model: "claude-opus-5-5", Params: model.RoleParams{MaxTokens: ip(tc.override)}}
		_, err := e.c.Complete(WithTarget(context.Background(), snap), model.RoleRepair, "SYS", "USR", true)
		if !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) || !isCode(err, CodeTruncated, "max_tokens", tc.sent[len(tc.sent)-1]) {
			t.Errorf("override %d: %v %+v", tc.override, err, infoOf(err))
		}
		caps := e.got()
		var sent []int
		for _, c := range caps {
			sent = append(sent, sentMaxTokens(t, c.body))
		}
		if fmt.Sprint(sent) != fmt.Sprint(tc.sent) {
			t.Errorf("override %d: sent %v, want %v", tc.override, sent, tc.sent)
		}
	}
	// No limit known (a model that is not Claude on an Anthropic endpoint):
	// doubled as before.
	e := newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) {
		io.WriteString(w, anthropicTruncated(`{"patches": [`))
	})
	claude := e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
	snap := model.LLMSnapshot{ProviderID: claude.ID, ProviderName: claude.Name, Model: "deepseek-chat", Params: model.RoleParams{MaxTokens: ip(100000)}}
	if _, err := e.c.Complete(WithTarget(context.Background(), snap), model.RoleRepair, "SYS", "USR", true); !errors.Is(err, ErrTruncated) {
		t.Fatal(err)
	}
	// v3.2.x P1-c: two truncation retries
	if caps := e.got(); len(caps) != 3 || sentMaxTokens(t, caps[1].body) != 200000 || sentMaxTokens(t, caps[2].body) != 400000 {
		t.Errorf("model without a known limit: %d requests", len(caps))
	}
	// An older Claude model (review D6): not doubled past its output limit
	// (claude-haiku-4-5: 64000); the user's value above it is sent as it
	// was, and its truncation is the call's error.
	e = newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) {
		io.WriteString(w, anthropicTruncated(`{"patches": [`))
	})
	claude = e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
	snap = model.LLMSnapshot{ProviderID: claude.ID, ProviderName: claude.Name, Model: "claude-haiku-4-5", Params: model.RoleParams{MaxTokens: ip(100000)}}
	if _, err := e.c.Complete(WithTarget(context.Background(), snap), model.RoleRepair, "SYS", "USR", true); !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) {
		t.Fatal(err)
	}
	if caps := e.got(); len(caps) != 1 || sentMaxTokens(t, caps[0].body) != 100000 {
		t.Errorf("older model: %d requests", len(caps))
	}
}
