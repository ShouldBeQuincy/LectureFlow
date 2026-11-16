// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"log/slog"
	"strings"
	"time"

	"lectureflow/internal/model"
)

// Per-role "深度思考" (.claude/thinking-brief.md section 2; T1). A role's
// level (model.RoleParams.Thinking: off, low, high, max) reaches the
// request through its target (snapshot or current setting). Off — the
// default, also every stray value — leaves every request body byte for
// byte as it was before the option existed (thinking_off_bodies.golden).
//
// DeepSeek (OpenAI-compatible, settings.IsDeepSeek):
//
//	off: "thinking": {"type": "disabled"} (appendix D2, unchanged)
//	on:  "thinking": {"type": "enabled"}, "reasoning_effort": "<level>",
//	     no temperature (ignored in thinking mode), max_tokens = the role's
//	     limit (DeepSeekDefaultMaxTokens when it sends none) + ThinkingBudget.
//	     Only message.content is read; reasoning_content is ignored.
//
// Anthropic (Messages API), max_tokens = the role's limit + ThinkingBudget
// (current Claude models: the role's base + the level's headroom, v3.2.x
// D1, maxtokens.go), no temperature either way:
//
//	off: no thinking field (unchanged), except the models that think when
//	     the field is absent (v3.2.x P1, Q4a; C91; anthropicOffFields):
//	     claude-sonnet-5-5: "thinking": {"type": "between_tools"}, no effort
//	     claude-opus-5, claude-sonnet-5, claude-haiku-5-5:
//	                        "thinking": {"type": "disabled"}, no effort
//	     claude-opus-5-5, claude-fable-*, claude-mythos-*:
//	                        "output_config": {"effort": "low"}, no thinking
//	                        field (their thinking cannot be switched off;
//	                        max_tokens holds ThinkingHeadroomOffAlwaysThinking)
//	     a Claude model in none of these lists (anthropicMayThinkWhenOff):
//	                        no field either, but max_tokens holds
//	                        ThinkingHeadroomOffUnlisted (R6-C)
//	on, current Claude models (anthropicAdaptive; the request is streamed,
//	     stream.go, and asks for a summary of the thinking):
//	     "thinking": {"type": "adaptive", "display": "summarized"},
//	     "output_config": {"effort": "<level>"}
//	on, older Claude models and non-Claude model names (DeepSeek's
//	     Anthropic-compatible endpoint and the like):
//	     "thinking": {"type": "enabled", "budget_tokens": ThinkingBudget}
//	Thinking blocks of the answer are skipped (parseAnthropic).
//
// Other OpenAI-compatible providers do not support it (settings.SupportsThinking);
// a level stored for one anyway is sent as off and logged once.
//
// A truncated answer is retried on the truncation ladder (http.go,
// TruncationRetries: an answer without text one level lower, else
// max_tokens doubled). Each attempt's timeout grows by
// ThinkingTimeoutPerToken per token max_tokens holds for thinking (the
// budget; on a current Claude model the headroom of maxtokens.go).

// Thinking budgets (tokens) of the levels: Anthropic's budget_tokens and,
// for every provider, the amount max_tokens is raised by.
const (
	ThinkingBudgetLow  = 2048
	ThinkingBudgetHigh = 8192
	ThinkingBudgetMax  = 24576
)

// ThinkingBudget returns the budget of a thinking level (0 for off and for
// any value that is not a level).
func ThinkingBudget(level string) int {
	switch level {
	case model.ThinkingLow:
		return ThinkingBudgetLow
	case model.ThinkingHigh:
		return ThinkingBudgetHigh
	case model.ThinkingMax:
		return ThinkingBudgetMax
	}
	return 0
}

// DeepSeekDefaultMaxTokens is DeepSeek's documented output limit in
// non-thinking mode when a request sends no max_tokens (8K; NOTES.md v2
// llm-builder): the "original limit" of the roles that send none, raised by
// the budget when thinking is on (DeepSeek's own default in thinking mode
// is far larger, but a request without max_tokens cannot be retried with a
// doubled limit when it is cut off).
const DeepSeekDefaultMaxTokens = 8192

// ThinkingTimeoutPerToken is how much longer one attempt may take per
// budget token when thinking is on (assumes at least 25 tokens/s): low
// adds about 82 s, high 5.5 min, max 16.4 min to DefaultTimeout. Without it
// a long reasoning answer would hit the 120 s timeout of SPEC §12 and be
// sent (and billed) again. Cancellation is unaffected.
const ThinkingTimeoutPerToken = 40 * time.Millisecond

// thinkingTimeout is the timeout of one attempt with the given budget.
func thinkingTimeout(base time.Duration, budget int) time.Duration {
	return base + time.Duration(budget)*ThinkingTimeoutPerToken
}

// TimeoutFreeTokens is the part of max_tokens the base timeout (SPEC §12,
// 120 s) already covers: the appendix D2 limit of an answer (4000). Tokens
// above it add ThinkingTimeoutPerToken each (review fix of v3.2.x D1).
const TimeoutFreeTokens = 4000

// attemptTimeout is the timeout of one attempt of a request with maxTokens
// (0 = not sent) of which budget is held for thinking. Without scaled it is
// thinkingTimeout (only the budget counts: the default limits of older
// Claude models and DeepSeek, unchanged). With scaled (max_tokens chosen
// for the answer: a current Claude model's base + headroom, the user's
// value, a doubled retry) every token above TimeoutFreeTokens counts too, at
// least the budget: a 16000-token syllabus with thinking off gets 120 s +
// 12000 × 40 ms = 600 s.
func attemptTimeout(base time.Duration, maxTokens, budget int, scaled bool) time.Duration {
	extra := budget
	if scaled && maxTokens-TimeoutFreeTokens > extra {
		extra = maxTokens - TimeoutFreeTokens
	}
	return thinkingTimeout(base, max(extra, 0))
}

// anthropicLegacyModels are the Claude models that take thinking as
// {"type": "enabled", "budget_tokens": N}: the models before Claude Opus
// 4.6 / Sonnet 4.6. An entry matches the id itself and every id that
// continues it with "-", "@" or "." (dated snapshots, "-latest", "2.1",
// Vertex's "@date"). Every later Claude model takes {"type": "adaptive"} with
// output_config.effort; Opus 4.7 and later (Opus 5.5, Sonnet 5.5, Haiku
// 5.5, Fable, ...) reject budget_tokens with a 400, and on Opus/Sonnet 4.6
// it is deprecated (claude-api documentation, 2026-10; NOTES.md,
// 决定（深度思考 T1）).
var anthropicLegacyModels = []string{
	// Claude 1/2 and every Claude 3 model (3, 3.5, 3.7)
	"claude-instant", "claude-2", "claude-3",
	// Opus 4 and Sonnet 4 (aliases and dated ids)
	"claude-opus-4-0", "claude-opus-4-20250514", "claude-sonnet-4-0", "claude-sonnet-4-20250514",
	// Opus 4.1 and the 4.5 family
	"claude-opus-4-1", "claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5",
}

// anthropicAdaptive reports whether modelName takes adaptive thinking with
// an effort level: a Claude model id ("claude-…", any case) that is not
// one of anthropicLegacyModels. Non-Claude names (an Anthropic-compatible
// endpoint of another vendor, such as DeepSeek's) get the classic budget
// form, which those endpoints implement.
func anthropicAdaptive(modelName string) bool {
	id := strings.ToLower(strings.TrimSpace(modelName))
	if !strings.HasPrefix(id, "claude-") {
		return false
	}
	for _, p := range anthropicLegacyModels {
		if id == p || strings.HasPrefix(id, p) && strings.ContainsRune("-@.", rune(id[len(p)])) {
			return false
		}
	}
	return true
}

// anthropicThinking is the thinking field of a Messages API request.
type anthropicThinking struct {
	Type         string `json:"type"`                    // "adaptive" | "enabled" | "between_tools" | "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // "enabled" only
	// Display: "summarized" with "adaptive" (v3.2.x P1-c): the current Claude
	// models omit the thinking text by default, so a streamed request would
	// be silent while the model thinks; a summary streams thinking_delta
	// events (activity for the idle timeout, the phase "thinking"). Never
	// with "between_tools", which takes no other field, nor with "disabled".
	Display string `json:"display,omitempty"`
}

// thinkingDisplaySummarized is the display of adaptive thinking (stream.go).
const thinkingDisplaySummarized = "summarized"

// anthropicOutputConfig carries the effort of adaptive thinking and the
// JSON Schema of a structured answer (anthropic.go).
type anthropicOutputConfig struct {
	Effort string           `json:"effort,omitempty"`
	Format *anthropicFormat `json:"format,omitempty"`
}

// The models whose thinking runs when the request has no thinking field:
// the level off needs fields of their own. Anthropic's "Thinking" page
// (platform.claude.com/docs/en/build-with-claude/thinking, configuration
// table, read 2026-11-11):
//
//	no field = thinking off    Opus 4.8, 4.7, 4.6, Sonnet 4.6 and older
//	no field = adaptive, and
//	  "disabled" turns it off  Opus 5 and Haiku 5.5 (effort high or below),
//	                           Sonnet 5
//	  "between_tools"          Sonnet 5.5 (effort high or below; "disabled"
//	                           is a 400)
//	  cannot be turned off     Opus 5.5, Fable 5 / 5.1 / 5.2, Mythos (every
//	                           other value is a 400): effort is the only
//	                           control
//
// The level off sends no effort where thinking is switched off: the model's
// default (high on Opus 5 and Sonnet 5, medium on Haiku 5.5) is within what
// "disabled" and "between_tools" accept.
const (
	modelSonnet55 = "claude-sonnet-5-5"
	modelOpus55   = "claude-opus-5-5"
	modelOpus5    = "claude-opus-5"
	modelSonnet5  = "claude-sonnet-5"
	modelHaiku55  = "claude-haiku-5-5"
	// thinkingBetweenTools is claude-sonnet-5-5's lowest setting: no extended
	// thinking. It takes no other field, is refused with an effort above
	// high, and no other model accepts it.
	thinkingBetweenTools = "between_tools"
	// thinkingTypeDisabled switches thinking off (anthropicDisablesThinking).
	thinkingTypeDisabled = "disabled"
	// EffortOffAlwaysThinking is the effort the models whose thinking cannot
	// be switched off (anthropicAlwaysThinks) get for the level off: the
	// lowest there is (claude-opus-5-5's default is medium, Fable's high).
	EffortOffAlwaysThinking = "low"
)

// anthropicAlwaysThinkingFamilies are the id prefixes of the model families
// whose thinking cannot be switched off (every Fable and Mythos model).
var anthropicAlwaysThinkingFamilies = []string{"claude-fable-", "claude-mythos-"}

// anthropicModelIs reports whether modelName is the Claude model id: the id
// itself or one that continues it with "-", "@" or "." (a dated snapshot).
func anthropicModelIs(modelName, id string) bool {
	m := strings.ToLower(strings.TrimSpace(modelName))
	return m == id || strings.HasPrefix(m, id) && strings.ContainsRune("-@.", rune(m[len(id)]))
}

// anthropicSnapshotIs reports whether modelName is the Claude model id
// itself, a dated snapshot of it ("-" and at least eight digits), its
// "-latest" alias or a platform id ("@…"). Stricter than anthropicModelIs:
// "claude-opus-5-5" is not a snapshot of "claude-opus-5", and a model id
// that only starts alike is not sent a field it may refuse with a 400.
func anthropicSnapshotIs(modelName, id string) bool {
	m := strings.ToLower(strings.TrimSpace(modelName))
	if m == id {
		return true
	}
	if !strings.HasPrefix(m, id) {
		return false
	}
	rest := m[len(id):]
	switch {
	case len(rest) > 1 && rest[0] == '@', rest == "-latest":
		return true
	case len(rest) >= 9 && rest[0] == '-':
		return strings.Trim(rest[1:], "0123456789") == ""
	}
	return false
}

// anthropicAlwaysThinks reports whether modelName is a Claude model whose
// thinking cannot be switched off (claude-opus-5-5, Fable, Mythos).
func anthropicAlwaysThinks(modelName string) bool {
	if anthropicModelIs(modelName, modelOpus55) {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(modelName))
	for _, p := range anthropicAlwaysThinkingFamilies {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// anthropicOffWithoutFieldModels are the current Claude models
// (anthropicAdaptive) that do not think when the request has no thinking
// field (the table above: "no field = thinking off", Opus 4.6 to 4.8 and
// Sonnet 4.6; the older ones take the budget form, anthropicLegacyModels).
// Matched strictly (anthropicSnapshotIs): an id that only starts alike is
// not known to stay silent (anthropicMayThinkWhenOff).
var anthropicOffWithoutFieldModels = []string{"claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-sonnet-4-6"}

// anthropicMayThinkWhenOff reports whether a request with thinking off to
// modelName may still think, so its max_tokens holds room for the thinking
// (maxtokens.go, thinkingHeadroom): a current Claude model (anthropicAdaptive)
// whose level off neither sends a field that switches thinking off
// (anthropicOffFields: "between_tools", "disabled") nor is a model known to
// think only when asked (anthropicOffWithoutFieldModels). These are the
// models whose thinking cannot be switched off (anthropicAlwaysThinks) and
// every Claude model id that is in none of the lists (R6-C, the user's
// USER-18 item 4; review R35 S2): a model released after them (such as
// claude-opus-6), or an id that only starts like a listed one. Such a model
// is still sent no field (one it does not know would be a 400), but since
// Claude 5 a request without one thinks at the model's default effort, and
// thinking counts toward max_tokens: without room, the answer is cut off
// and sent again (T9). Room it does not use is not billed, so the
// uncertain case gets it. Older Claude models and non-Claude names
// (anthropicAdaptive false) are not among them: their requests with
// thinking off stay what they were.
func anthropicMayThinkWhenOff(modelName string) bool {
	if !anthropicAdaptive(modelName) {
		return false
	}
	if th, _ := anthropicOffFields(modelName); th != nil {
		return false // "between_tools" or "disabled"
	}
	for _, id := range anthropicOffWithoutFieldModels {
		if anthropicSnapshotIs(modelName, id) {
			return false
		}
	}
	return true
}

// anthropicDisablesThinking reports whether modelName is a Claude model
// that thinks when the request has no thinking field and takes
// {"type": "disabled"} (claude-opus-5, claude-sonnet-5, claude-haiku-5-5).
func anthropicDisablesThinking(modelName string) bool {
	return anthropicSnapshotIs(modelName, modelOpus5) || anthropicSnapshotIs(modelName, modelSonnet5) ||
		anthropicSnapshotIs(modelName, modelHaiku55)
}

// anthropicOffFields returns the thinking fields of a request with thinking
// off (v3.2.x P1, Q4a; C91): nothing, except for the models above.
func anthropicOffFields(modelName string) (*anthropicThinking, *anthropicOutputConfig) {
	switch {
	case anthropicModelIs(modelName, modelSonnet55):
		return &anthropicThinking{Type: thinkingBetweenTools}, nil
	case anthropicAlwaysThinks(modelName):
		return nil, &anthropicOutputConfig{Effort: EffortOffAlwaysThinking}
	case anthropicDisablesThinking(modelName):
		return &anthropicThinking{Type: thinkingTypeDisabled}, nil
	}
	return nil, nil
}

// anthropicThinkingFields returns the thinking fields of a request with
// thinking level (off: anthropicOffFields).
func anthropicThinkingFields(modelName, level string) (*anthropicThinking, *anthropicOutputConfig) {
	budget := ThinkingBudget(level)
	if budget == 0 {
		return anthropicOffFields(modelName)
	}
	if anthropicAdaptive(modelName) {
		return &anthropicThinking{Type: "adaptive", Display: thinkingDisplaySummarized}, &anthropicOutputConfig{Effort: level}
	}
	return &anthropicThinking{Type: "enabled", BudgetTokens: budget}, nil
}

// supportsThinking is settings.SupportsThinking for a resolved target.
func supportsThinking(ptype string, deepseek bool) bool {
	return ptype == model.ProviderAnthropic || (ptype == model.ProviderOpenAI && deepseek)
}

// thinkingFor returns the level a request of role to the target sends:
// the params' level when it is valid and the provider supports thinking,
// otherwise off. A level that cannot be sent (should not occur: SetRole
// refuses it) is logged once per client, provider, role and value.
func (c *HTTPClient) thinkingFor(role, providerName, ptype string, deepseek bool, p model.RoleParams) string {
	raw := model.ThinkingOf(p)
	lvl := model.ThinkingLevel(p)
	why := ""
	switch {
	case raw != lvl:
		why = "not a thinking level"
	case lvl != model.ThinkingOff && !supportsThinking(ptype, deepseek):
		why = "the provider does not support thinking"
		lvl = model.ThinkingOff
	}
	if why != "" {
		if _, seen := c.thinkingWarned.LoadOrStore(providerName+"\x00"+role+"\x00"+raw, true); !seen {
			slog.Warn("llm: thinking sent as off", "role", role, "provider", providerName, "thinking", raw, "reason", why)
		}
	}
	return lvl
}
