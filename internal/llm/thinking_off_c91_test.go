// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// C91: the thinking fields and max_tokens of an Anthropic request, per model
// and level, as Anthropic's thinking page documents them (thinking.go):
// "disabled" where a model thinks without a thinking field and accepts it,
// "between_tools" on claude-sonnet-5-5, effort low with room for the
// thinking where thinking cannot be switched off, nothing where no field
// already means no thinking (and nothing, with room for the thinking, on a
// Claude model in none of the lists: R6-C). Role repair: base 6000 on a
// current Claude model, 4000 on an older one or a non-Claude name.
func TestAnthropicRequestThinkingByModelAndLevel(t *testing.T) {
	const adaptiveSum = `{"type":"adaptive","display":"summarized"}`
	type want struct {
		thinking string // the thinking field as sent ("" = absent)
		effort   string // output_config.effort ("" = absent)
		max      int
	}
	off := func(th, ef string, mt int) want { return want{th, ef, mt} }
	for _, tc := range []struct {
		model string
		off   want
		low   want // nil thinking: same rule as every current Claude model
	}{
		// Thinking cannot be switched off: effort low, headroom 12000 (off and low).
		{"claude-opus-5-5", off("", "low", 18000), want{adaptiveSum, "low", 18000}},
		{"claude-opus-5-5-20260901", off("", "low", 18000), want{adaptiveSum, "low", 18000}},
		{"claude-fable-5-2", off("", "low", 18000), want{adaptiveSum, "low", 18000}},
		{"claude-fable-5", off("", "low", 18000), want{adaptiveSum, "low", 18000}},
		{"claude-mythos-5-2", off("", "low", 18000), want{adaptiveSum, "low", 18000}},
		// Lowest setting of its own.
		{"claude-sonnet-5-5", off(`{"type":"between_tools"}`, "", 6000), want{adaptiveSum, "low", 14000}},
		// Adaptive without a field, "disabled" accepted at the default effort.
		{"claude-haiku-5-5", off(`{"type":"disabled"}`, "", 6000), want{adaptiveSum, "low", 14000}},
		{"claude-opus-5", off(`{"type":"disabled"}`, "", 6000), want{adaptiveSum, "low", 14000}},
		{"claude-opus-5-20260301", off(`{"type":"disabled"}`, "", 6000), want{adaptiveSum, "low", 14000}},
		{"claude-sonnet-5", off(`{"type":"disabled"}`, "", 6000), want{adaptiveSum, "low", 14000}},
		{"claude-sonnet-5@20260301", off(`{"type":"disabled"}`, "", 6000), want{adaptiveSum, "low", 14000}},
		{"claude-sonnet-5-latest", off(`{"type":"disabled"}`, "", 6000), want{adaptiveSum, "low", 14000}},
		// No field already means no thinking: unchanged.
		{"claude-opus-4-8", off("", "", 6000), want{adaptiveSum, "low", 14000}},
		{"claude-opus-4-6", off("", "", 6000), want{adaptiveSum, "low", 14000}},
		{"claude-sonnet-4-6", off("", "", 6000), want{adaptiveSum, "low", 14000}},
		// Ids that only start alike are not sent a field they may refuse;
		// being in none of the lists, they may think without one: the
		// headroom of ThinkingHeadroomOffUnlisted at off (R6-C).
		{"claude-opus-5-1", off("", "", 18000), want{adaptiveSum, "low", 14000}},
		{"claude-sonnet-5-50", off("", "", 18000), want{adaptiveSum, "low", 14000}},
		{"claude-haiku-5-5-preview", off("", "", 18000), want{adaptiveSum, "low", 14000}},
		// Older Claude models and non-Claude names: as before.
		{"claude-haiku-4-5", off("", "", 4000), want{`{"type":"enabled","budget_tokens":2048}`, "", 4000 + 2048}},
		{"deepseek-v4-pro", off("", "", 4000), want{`{"type":"enabled","budget_tokens":2048}`, "", 4000 + 2048}},
	} {
		for _, lv := range []struct {
			level string
			want  want
		}{{model.ThinkingOff, tc.off}, {model.ThinkingLow, tc.low}} {
			tg := target{ptype: model.ProviderAnthropic, model: tc.model, thinking: lv.level}
			c := &HTTPClient{}
			mt, _ := c.outputLimit(tg, model.RoleRepair)
			body, err := anthropicBody(tg, mt, "SYS", "USR", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				MaxTokens    int             `json:"max_tokens"`
				Thinking     json.RawMessage `json:"thinking"`
				OutputConfig *struct {
					Effort string `json:"effort"`
				} `json:"output_config"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			effort := ""
			if got.OutputConfig != nil {
				effort = got.OutputConfig.Effort
			}
			if string(got.Thinking) != lv.want.thinking || effort != lv.want.effort || got.MaxTokens != lv.want.max {
				t.Errorf("%s %s: thinking %s effort %q max_tokens %d, want %s %q %d",
					tc.model, lv.level, got.Thinking, effort, got.MaxTokens, lv.want.thinking, lv.want.effort, lv.want.max)
			}
		}
	}
}

// The thinking fields depend on the model and the level only: every
// request of a role (the repair warm-up chunk and the chunks after it, any
// role, JSON or not) carries the same ones, so the cached prefix of a
// lecture stays the same (C91; prompt caching renders the thinking
// configuration and the effort into the prompt).
func TestAnthropicThinkingFieldsSameForEveryRequestOfARole(t *testing.T) {
	for _, m := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-opus-5", "claude-fable-5-2", "claude-opus-4-8"} {
		for _, level := range []string{model.ThinkingOff, model.ThinkingLow, model.ThinkingHigh, model.ThinkingMax} {
			var first string
			for i, role := range model.Roles {
				for _, jsonMode := range []bool{true, false} {
					tg := target{ptype: model.ProviderAnthropic, model: m, thinking: level}
					body, err := anthropicBody(tg, 1000, "SYS", "USR", jsonMode, nil)
					if err != nil {
						t.Fatal(err)
					}
					var got struct {
						Thinking     json.RawMessage `json:"thinking"`
						OutputConfig json.RawMessage `json:"output_config"`
					}
					if err := json.Unmarshal(body, &got); err != nil {
						t.Fatal(err)
					}
					fields := string(got.Thinking) + "|" + string(got.OutputConfig)
					if i == 0 && jsonMode {
						first = fields
					} else if fields != first {
						t.Errorf("%s %s %s json=%v: %s, first request %s", m, level, role, jsonMode, fields, first)
					}
				}
			}
		}
	}
}

// The thinking tokens of an Anthropic answer reach the call log's
// reasoning_tokens (usage.output_tokens_details.thinking_tokens), for a
// plain and a streamed answer; absent when the usage has none.
func TestAnthropicThinkingTokensInCallLog(t *testing.T) {
	plain := `{"id":"msg_1","type":"message","stop_reason":"max_tokens","content":[{"type":"thinking","thinking":""}],` +
		`"usage":{"input_tokens":10,"output_tokens":10000,"output_tokens_details":{"thinking_tokens":9965}}}`
	if m := answerMeta(model.ProviderAnthropic, []byte(plain)); m.reasoningTokens == nil || *m.reasoningTokens != 9965 {
		t.Errorf("plain answer: %v", m.reasoningTokens)
	}
	none := `{"id":"msg_2","type":"message","stop_reason":"end_turn","content":[],"usage":{"input_tokens":10,"output_tokens":5}}`
	if m := answerMeta(model.ProviderAnthropic, []byte(none)); m.reasoningTokens != nil {
		t.Errorf("no details: %v", *m.reasoningTokens)
	}
	if n := anthropicThinkingTokens(json.RawMessage(`{"output_tokens":3,"output_tokens_details":{"thinking_tokens":2}}`)); n == nil || *n != 2 {
		t.Errorf("helper: %v", n)
	}
	// Streamed with the thinking text omitted (the default display): one
	// empty thinking delta and the signature, pings while the model thinks,
	// cut off at max_tokens (T9).
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
		"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"c2ln\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":10000,\"output_tokens_details\":{\"thinking_tokens\":9965}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out, err := readAnthropicStream(strings.NewReader(sse), testKey, streamHooks{since: func() time.Duration { return 0 }})
	if err != nil || out.aerr == nil || out.aerr.kind != failTruncated {
		t.Fatalf("err %v aerr %+v", err, out.aerr)
	}
	if n := out.meta.reasoningTokens; n == nil || *n != 9965 {
		t.Errorf("streamed answer: %v", n)
	}
}
