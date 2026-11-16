// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Review P3, R56: a truncation retry one thinking level down, where the
// first attempt's timeout follows the thinking budget only (DeepSeek, older
// Claude models), keeps the first attempt's timeout: recomputed from
// max_tokens − 4000 it grew (DeepSeek max 18m23s → high 21m10s), from the
// lower level's budget it shrank (→ 7m27s), although the level is lowered
// because thinking filled max_tokens. A doubled retry, and every retry
// where the timeout follows max_tokens (current Claude models), is
// recomputed as before.
func TestLoweredLevelTimeout(t *testing.T) {
	per := ThinkingTimeoutPerToken
	at := func(tokens int) time.Duration { return DefaultTimeout + time.Duration(tokens)*per }
	for _, tc := range []struct {
		name, model, ptype, level, role string
		want                            []time.Duration // the timeout of each attempt
		maxTokens                       []int
	}{
		// max → high → low, max_tokens 8192 + 24576: all three the first's
		{"DeepSeek max", "deepseek-pro", model.ProviderOpenAI, model.ThinkingMax, model.RoleRepair,
			[]time.Duration{at(ThinkingBudgetMax), at(ThinkingBudgetMax), at(ThinkingBudgetMax)},
			[]int{DeepSeekDefaultMaxTokens + ThinkingBudgetMax, DeepSeekDefaultMaxTokens + ThinkingBudgetMax, DeepSeekDefaultMaxTokens + ThinkingBudgetMax}},
		// high → low (the first's), then doubled: 24384 − 4000
		{"older Claude high", "claude-sonnet-4-5", model.ProviderAnthropic, model.ThinkingHigh, model.RoleRepair,
			[]time.Duration{at(ThinkingBudgetHigh), at(ThinkingBudgetHigh), at(2*(DefaultAnthropicMaxTokens+ThinkingBudgetHigh) - TimeoutFreeTokens)},
			[]int{DefaultAnthropicMaxTokens + ThinkingBudgetHigh, DefaultAnthropicMaxTokens + ThinkingBudgetHigh, 2 * (DefaultAnthropicMaxTokens + ThinkingBudgetHigh)}},
		// translate 2000 + 24000: the first follows the headroom (24000),
		// the lowered one max_tokens − 4000 (22000), the doubled one is
		// capped (52000 − 4000 → 34m)
		{"current Claude high", "claude-sonnet-5-5", model.ProviderAnthropic, model.ThinkingHigh, model.RoleTranslate,
			[]time.Duration{at(ThinkingHeadroomHigh), at(BaseMaxTokensTranslate + ThinkingHeadroomHigh - TimeoutFreeTokens), MaxAttemptTimeout},
			[]int{26000, 26000, 52000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) {
				if strings.HasSuffix(path, "/v1/messages") {
					io.WriteString(w, anthropicTruncated(""))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
			})
			base := ""
			if tc.ptype == model.ProviderOpenAI {
				base = "https://api.deepseek.com" // redirected to the test server
			}
			p := e.provider("P", tc.ptype, base, model.BalanceNone)
			snap := model.LLMSnapshot{ProviderID: p.ID, ProviderName: p.Name, Model: tc.model, Params: model.RoleParams{Thinking: sp(tc.level)}}
			st := &sched.StatsRecorder{}
			ctx := sched.WithStatsSink(WithTarget(context.Background(), snap), st)
			if _, err := e.c.Complete(ctx, tc.role, "s", "u", true); !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) {
				t.Fatalf("err %v", err)
			}
			var got []time.Duration
			for _, ev := range st.Events {
				if ev.Phase == sched.RequestStart {
					got = append(got, ev.Timeout)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("timeouts %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("attempt %d: timeout %v, want %v", i, got[i], tc.want[i])
				}
			}
			if tc.ptype == model.ProviderOpenAI && got[1] != got[0] {
				t.Errorf("DeepSeek: the lowered retry's timeout %v is not the first attempt's %v", got[1], got[0])
			}
			caps := e.got()
			if len(caps) != len(tc.maxTokens) {
				t.Fatalf("%d requests, want %d", len(caps), len(tc.maxTokens))
			}
			for i, c := range caps {
				var body map[string]any
				if err := json.Unmarshal(c.body, &body); err != nil {
					t.Fatal(err)
				}
				if mt, _ := body["max_tokens"].(float64); int(mt) != tc.maxTokens[i] {
					t.Errorf("request %d: max_tokens %v, want %d", i, body["max_tokens"], tc.maxTokens[i])
				}
			}
		})
	}
}
