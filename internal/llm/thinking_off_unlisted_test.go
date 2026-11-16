// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"lectureflow/internal/model"
)

// R6-C (the user's USER-18 item 4; review R35 S2, backlog U2): a current
// Claude model (anthropicAdaptive) that is in none of the lists of
// thinking.go — a model released after them, such as claude-opus-6 — may
// think when the request has no thinking field, as every Claude 5 model
// does. Its level off still sends no field (nothing it might refuse with a
// 400) but holds the headroom of the models whose thinking cannot be
// switched off, so thinking at the model's default effort does not fill
// max_tokens and get the request cut off and sent again (T9). The models
// the lists know keep what they had.

// unlistedClaudeIDs are model ids that look like Claude models (anthropicAdaptive)
// and are in none of the lists: new families and versions, and ids that
// only start like a listed one (the lists match strictly).
var unlistedClaudeIDs = []string{
	"claude-opus-6", "claude-opus-6-20270101", "claude-opus-6@20270101", "claude-sonnet-6", "claude-haiku-6", "claude-opus-6-1",
	"claude-opus-5-1", "claude-sonnet-5-1", "claude-haiku-5", "claude-opus-5-v1", "claude-opus-5-2026030", "claude-opus-5.5",
	"claude-sonnet-5-50", "claude-haiku-5-5-preview", "claude-future-9", " Claude-Opus-6 ",
	"claude-opus-4-9", "claude-opus-4-10", "claude-sonnet-4-7", "claude-haiku-4-6", "claude-opus-4-6-1", "claude-opus-4-60",
	"claude-opus-5[1m]", "claude-opus-5-5[1m]",
}

func TestThinkingOffHeadroomOfUnlistedClaude(t *testing.T) {
	if ThinkingHeadroomOffUnlisted != ThinkingHeadroomOffAlwaysThinking {
		t.Errorf("ThinkingHeadroomOffUnlisted = %d, want the headroom of the models that always think (%d)",
			ThinkingHeadroomOffUnlisted, ThinkingHeadroomOffAlwaysThinking)
	}
	for _, tc := range []struct {
		class    string
		headroom int
		ids      []string
	}{
		{"unlisted current Claude: may think without a field", ThinkingHeadroomOffUnlisted, unlistedClaudeIDs},
		// The lists of thinking.go: unchanged.
		{"thinking cannot be switched off (C91)", ThinkingHeadroomOffAlwaysThinking, []string{
			"claude-opus-5-5", "claude-opus-5-5-20260901", "claude-fable-5", "claude-fable-5-2", "claude-mythos-5-2", "claude-mythos-preview",
		}},
		{"between_tools: no extended thinking", 0, []string{"claude-sonnet-5-5", "claude-sonnet-5-5-20260901", "claude-sonnet-5-5@20261101"}},
		{"disabled: thinking switched off", 0, []string{
			"claude-opus-5", "claude-opus-5-20260301", "claude-sonnet-5", "claude-sonnet-5@20260301", "claude-sonnet-5-latest",
			"claude-haiku-5-5", "claude-haiku-5-5-20261001",
		}},
		{"no field already means no thinking", 0, []string{
			"claude-opus-4-6", "claude-opus-4-6@20260101", "claude-opus-4-6-20260205", "claude-opus-4-7", "claude-opus-4-8",
			"claude-opus-4-8-latest", "claude-sonnet-4-6", " Claude-Sonnet-4-6 ",
		}},
		// Not current Claude models: the budget form, nothing for off.
		{"older Claude and non-Claude names", 0, []string{
			"claude-haiku-4-5", "claude-opus-4-1", "claude-3-7-sonnet-latest", "claude-2.1", "deepseek-flash", "claude", "opus-6",
		}},
	} {
		for _, id := range tc.ids {
			if h, capped := thinkingHeadroom(model.ProviderAnthropic, id, model.ThinkingOff, true); h != tc.headroom || capped {
				t.Errorf("%s: headroom of %q at off = %d (capped %v), want %d", tc.class, id, h, capped, tc.headroom)
			}
			if got := anthropicMayThinkWhenOff(id); got != (tc.headroom > 0) {
				t.Errorf("%s: anthropicMayThinkWhenOff(%q) = %v", tc.class, id, got)
			}
			// A Claude name on an OpenAI-compatible provider: as before.
			if h, _ := thinkingHeadroom(model.ProviderOpenAI, id, model.ThinkingOff, true); h != 0 {
				t.Errorf("%s: OpenAI-compatible headroom of %q at off = %d", tc.class, id, h)
			}
		}
	}
}

// The request of an unlisted model with thinking off: no thinking field and
// no effort, as before (a field the model does not know would be a 400);
// max_tokens is the role's base + the headroom, for every role. The other
// levels are unchanged: adaptive thinking with the level's effort and the
// headroom of every current Claude model.
func TestThinkingOffRequestOfUnlistedClaude(t *testing.T) {
	type sent struct {
		MaxTokens    int             `json:"max_tokens"`
		Thinking     json.RawMessage `json:"thinking"`
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	for _, id := range unlistedClaudeIDs {
		for _, lv := range []struct {
			level    string
			thinking string
			effort   string
			headroom int
		}{
			{model.ThinkingOff, "", "", ThinkingHeadroomOffUnlisted},
			{model.ThinkingLow, `{"type":"adaptive","display":"summarized"}`, "low", ThinkingHeadroomLow},
			{model.ThinkingHigh, `{"type":"adaptive","display":"summarized"}`, "high", ThinkingHeadroomHigh},
			{model.ThinkingMax, `{"type":"adaptive","display":"summarized"}`, "max", ThinkingHeadroomMax},
		} {
			for _, role := range model.Roles {
				tg := target{ptype: model.ProviderAnthropic, model: id, thinking: lv.level}
				if !streamed(tg) {
					t.Fatalf("%q: not streamed", id)
				}
				c := &HTTPClient{}
				mt, headroom := c.outputLimit(tg, role)
				body, err := anthropicBody(tg, mt, "SYS", "USR", true, nil)
				if err != nil {
					t.Fatal(err)
				}
				var got sent
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatal(err)
				}
				effort := ""
				if got.OutputConfig != nil {
					effort = got.OutputConfig.Effort
				}
				want := BaseMaxTokens(role, 0) + lv.headroom
				if string(got.Thinking) != lv.thinking || effort != lv.effort || got.MaxTokens != want || headroom != lv.headroom {
					t.Errorf("%q %s %s: thinking %s effort %q max_tokens %d headroom %d, want %s %q %d %d",
						id, lv.level, role, got.Thinking, effort, got.MaxTokens, headroom, lv.thinking, lv.effort, want, lv.headroom)
				}
			}
		}
		// The user's max_tokens is the whole limit, as for every current
		// Claude model.
		tg := target{ptype: model.ProviderAnthropic, model: id, thinking: model.ThinkingOff, maxTokens: 3000}
		if mt, _ := (&HTTPClient{}).outputLimit(tg, model.RoleRepair); mt != 3000 {
			t.Errorf("%q off, user's 3000: max_tokens %d", id, mt)
		}
	}
}

// T9 replayed on a model the lists do not know: with thinking off it
// thinks 9,965 tokens (no field: the model's default) before an answer of
// 400. The first request holds both, so it is sent once and not cut off;
// with no headroom (6000) it was cut off and sent again at 12000.
func TestThinkingOffUnlistedClaudeNotCutOff(t *testing.T) {
	const thought, answer = 9965, 400
	var e *thinkingEnv
	e = newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) {
		caps := e.got()
		if sentMaxTokens(t, caps[n-1].body) < thought+answer {
			io.WriteString(w, anthropicTruncated(""))
			return
		}
		io.WriteString(w, anthropicOK(`{"patches": []}`))
	})
	claude := e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
	snap := model.LLMSnapshot{ProviderID: claude.ID, ProviderName: claude.Name, Model: "claude-opus-6"}
	if _, err := e.c.Complete(WithTarget(context.Background(), snap), model.RoleRepair, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	caps := e.got()
	if len(caps) != 1 {
		t.Fatalf("%d requests, want 1", len(caps))
	}
	var b map[string]json.RawMessage
	if err := json.Unmarshal(caps[0].body, &b); err != nil {
		t.Fatal(err)
	}
	if mt := sentMaxTokens(t, caps[0].body); mt != BaseMaxTokensRepair+ThinkingHeadroomOffUnlisted {
		t.Errorf("max_tokens %d", mt)
	}
	for _, k := range []string{"thinking", "output_config", "temperature"} {
		if _, has := b[k]; has {
			t.Errorf("field %q sent: %s", k, caps[0].body)
		}
	}
	if string(b["stream"]) != "true" {
		t.Errorf("not streamed: %s", caps[0].body)
	}
}
