// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"log/slog"
	"strings"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// max_tokens of a request (v3.2.x D1; design note 3.2, the user's Q6a,
// amending the table of SPEC-v2 appendix D2):
//
//	max_tokens = the user's value (RoleParams.MaxTokens), else
//	             the role's base for the answer + the thinking headroom,
//	             at most the model's output limit.
//
// The new values apply to the current Claude models (anthropicAdaptive:
// adaptive thinking, which has no budget parameter: the model does not know
// max_tokens, so an answer that thinks longer is simply cut off). Older
// Claude models, non-Claude names on an Anthropic endpoint, DeepSeek and
// other OpenAI-compatible providers keep the limits of appendix D2 and the
// thinking budgets (thinking.go): their requests with thinking off stay
// byte for byte what they were (thinking_off_bodies.golden). Every provider
// takes the user's value; an OpenAI-compatible provider that sends no
// max_tokens by default sends it only then.
//
// Base for the answer (current Claude models):
//
//	translate                     2000  (one sentence: 50–150 tokens)
//	translate, batch > 1          6000  (≤ 20 sentences, v3.2.x P2; also
//	                                     the floor of older Claude models,
//	                                     within their output limit)
//	summary, chapters, organizer  4000
//	repair                        6000  (40 sentences, all changed: ~2600)
//	glossary (and material terms) 8000  (150 terms)
//	syllabus_format              16000  (the whole syllabus again)
//	glossary_janitor             16000
//
// Thinking headroom (current Claude models): off 0, low 8000 (the models
// whose thinking cannot be switched off and runs at effort low for both
// levels — claude-opus-5-5, Fable, Mythos, anthropicAlwaysThinks — 12000
// for off and low, C91; a Claude model in none of the lists of thinking.go,
// which is sent no thinking field and may think at its default effort —
// anthropicMayThinkWhenOff — 12000 for off, R6-C), high 24000, max 60000 —
// capped at 28000 for a request that is not
// streamed (an answer of 60000 tokens takes longer than one silent request
// should). Since v3.2.x P1-c every request to a current Claude model is
// streamed (stream.go), so the cap applies to none of them now; it stays for
// a path that is not. The headroom costs nothing when unused: only
// generated tokens are billed.
const (
	BaseMaxTokensTranslate      = 2000
	BaseMaxTokensTranslateBatch = 6000
	BaseMaxTokensSummary        = 4000
	BaseMaxTokensRepair         = 6000
	BaseMaxTokensGlossary       = 8000
	BaseMaxTokensSyllabus       = 16000
	BaseMaxTokensJanitor        = 16000
	BaseMaxTokensOther          = 4000

	// ThinkingHeadroomOffAlwaysThinking is the headroom of the levels off and
	// low on a model whose thinking cannot be switched off
	// (anthropicAlwaysThinks; C91): both send effort low. Effort low is a behavioural signal, not a budget: Anthropic's
	// effort page says the model "still thinks on sufficiently difficult
	// problems", and thinking counts toward max_tokens. The real acceptance
	// run T9 saw claude-opus-5-5 at effort low think 9,965 tokens on one
	// repair chunk (6000 + 4000 = 10000: cut off and sent again at 20000).
	// 12000 holds that with a margin of about a fifth on top of every
	// role's base, the smallest (translate, 2000) included. Unused headroom
	// is not billed.
	ThinkingHeadroomOffAlwaysThinking = 12000
	// ThinkingHeadroomOffUnlisted is the headroom of the level off on a
	// current Claude model that is in none of the lists of thinking.go
	// (anthropicMayThinkWhenOff; R6-C, the user's USER-18 item 4): it is sent
	// no thinking field and may think at its default effort, as the models
	// above may at effort low. The same room, on the side of caution: unused
	// headroom is not billed, a cut-off answer is sent (and billed) again.
	ThinkingHeadroomOffUnlisted = ThinkingHeadroomOffAlwaysThinking
	ThinkingHeadroomLow         = 8000
	ThinkingHeadroomHigh        = 24000
	ThinkingHeadroomMax         = 60000
	// ThinkingHeadroomMaxUnstreamed is the headroom of the level max while
	// requests are not streamed.
	ThinkingHeadroomMaxUnstreamed = 28000

	// ClaudeMaxOutputTokens is the output limit of the current Claude
	// models (128K). Other models have no limit here: the server says so.
	ClaudeMaxOutputTokens = 128000
)

// BaseMaxTokens returns the base for the answer of a request of role to a
// current Claude model; batch is the translate role's sentences per request
// (RoleParams.Batch; ≤ 1 = one).
func BaseMaxTokens(role string, batch int) int {
	switch role {
	case model.RoleTranslate:
		if batch > 1 {
			return BaseMaxTokensTranslateBatch
		}
		return BaseMaxTokensTranslate
	case model.RoleSummary, model.RoleChapters, model.RoleOrganizer:
		return BaseMaxTokensSummary
	case model.RoleRepair:
		return BaseMaxTokensRepair
	case model.RoleGlossary:
		return BaseMaxTokensGlossary
	case model.RoleSyllabusFormat:
		return BaseMaxTokensSyllabus
	case model.RoleGlossaryJanitor:
		return BaseMaxTokensJanitor
	}
	return BaseMaxTokensOther
}

// thinkingHeadroom returns how many tokens max_tokens holds for thinking at
// level: on a current Claude model the table above (streamed: whether the
// request is streamed, which lifts the cap of the level max); elsewhere the
// level's budget (ThinkingBudget). capped reports the level max cut to
// ThinkingHeadroomMaxUnstreamed.
func thinkingHeadroom(ptype, modelName, level string, streamed bool) (headroom int, capped bool) {
	if ptype != model.ProviderAnthropic || !anthropicAdaptive(modelName) {
		return ThinkingBudget(level), false
	}
	switch level {
	case model.ThinkingLow:
		if anthropicAlwaysThinks(modelName) {
			// The same effort low as the level off: the same headroom.
			return ThinkingHeadroomOffAlwaysThinking, false
		}
		return ThinkingHeadroomLow, false
	case model.ThinkingHigh:
		return ThinkingHeadroomHigh, false
	case model.ThinkingMax:
		if !streamed {
			return ThinkingHeadroomMaxUnstreamed, true
		}
		return ThinkingHeadroomMax, false
	}
	switch {
	case anthropicAlwaysThinks(modelName):
		return ThinkingHeadroomOffAlwaysThinking, false
	case anthropicMayThinkWhenOff(modelName):
		// A model the lists do not know: sent no field, it may think (R6-C).
		return ThinkingHeadroomOffUnlisted, false
	}
	return 0, false
}

// outputCap returns the output limit of a target's model (0 = none known).
func outputCap(tg target) int {
	if tg.ptype == model.ProviderAnthropic && anthropicAdaptive(tg.model) {
		return ClaudeMaxOutputTokens
	}
	return 0
}

// truncationCap is the limit a doubled max_tokens stops at on the
// truncation ladder (0 = none known): outputCap, and for an older Claude
// model on an Anthropic endpoint its documented output limit
// (legacyClaudeOutputCap; review D6: doubled past it, the request was an
// HTTP 400, now the truncation is the call's error). The first request's
// max_tokens is not capped by it (outputLimit, as before).
func truncationCap(tg target) int {
	if limit := outputCap(tg); limit > 0 {
		return limit
	}
	if tg.ptype == model.ProviderAnthropic {
		return legacyClaudeOutputCap(tg.model)
	}
	return 0
}

// legacyClaudeOutputLimits are the output limits of the older Claude
// models (anthropicLegacyModels; claude-api documentation): the first entry
// whose prefix matches the id (the id itself, or the prefix followed by
// "-", "@" or "."), so the more specific prefixes come first. The batch
// base of the translate role reads them (outputLimit), and so does the
// truncation ladder (truncationCap).
var legacyClaudeOutputLimits = []struct {
	prefix string
	limit  int
}{
	{"claude-3-7", 64000},
	{"claude-3-5", 8192},
	{"claude-3", 4096},
	{"claude-2", 4096},
	{"claude-instant", 4096},
	{"claude-opus-4-0", 32000},
	{"claude-opus-4-20250514", 32000},
	{"claude-opus-4-1", 32000},
	{"claude-sonnet-4-0", 64000},
	{"claude-sonnet-4-20250514", 64000},
	{"claude-opus-4-5", 64000},
	{"claude-sonnet-4-5", 64000},
	{"claude-haiku-4-5", 64000},
}

// legacyClaudeOutputCap returns the output limit of an older Claude model
// (0 = not one of legacyClaudeOutputLimits).
func legacyClaudeOutputCap(modelName string) int {
	id := strings.ToLower(strings.TrimSpace(modelName))
	for _, e := range legacyClaudeOutputLimits {
		if id == e.prefix || strings.HasPrefix(id, e.prefix) && strings.ContainsRune("-@.", rune(id[len(e.prefix)])) {
			return e.limit
		}
	}
	return 0
}

// outputLimit returns max_tokens of a request of role to tg (0 = the field
// is not sent) and the tokens it holds for thinking (headroom: the timeout
// grows with it, thinking.go).
func (c *HTTPClient) outputLimit(tg target, role string) (maxTokens, headroom int) {
	headroom, capped := thinkingHeadroom(tg.ptype, tg.model, tg.thinking, streamed(tg))
	adaptive := tg.ptype == model.ProviderAnthropic && anthropicAdaptive(tg.model)
	switch {
	case tg.maxTokens > 0:
		// The user's value is the whole limit, thinking included.
		maxTokens = tg.maxTokens
		if tg.ptype == model.ProviderAnthropic && !adaptive && headroom > 0 && maxTokens <= headroom {
			// budget_tokens must be below max_tokens (the API refuses the
			// request otherwise): the budget is added to the user's value.
			maxTokens += headroom
			c.warnLimit("override_below_budget", tg, role, "llm: the role's max_tokens is not above the thinking budget; the budget is added to it",
				"max_tokens", tg.maxTokens, "budget", headroom)
		}
	case tg.ptype == model.ProviderAnthropic:
		base := c.AnthropicMaxTokens
		if base <= 0 {
			if adaptive {
				base = BaseMaxTokens(role, tg.batch)
			} else {
				base = AnthropicMaxTokensFor(role)
				if role == model.RoleTranslate && tg.batch > 1 && structuredModel(tg.model) {
					// An older Claude model answering a batch: the batch
					// base, at most what the model's output limit leaves
					// beside the thinking budget (legacyClaudeOutputCap:
					// Claude 3 answers at most 4096 tokens, 3.5 8192; more
					// is an HTTP 400), never below the limit of appendix
					// D2. Non-Claude models on an Anthropic endpoint
					// (DeepSeek, 8K output) keep the limit of appendix D2:
					// its doubled retry (8000) still fits their output limit.
					floor := BaseMaxTokensTranslateBatch
					if limit := legacyClaudeOutputCap(tg.model); limit > 0 {
						floor = min(floor, limit-headroom)
					}
					base = max(base, floor)
				}
			}
		}
		maxTokens = base + headroom
	default:
		// OpenAI-compatible: no max_tokens unless thinking is on (DeepSeek's
		// default limit plus the budget) or the role is DeepSeek's janitor.
		// Also not for translate batches (v3.2.x P2): DeepSeek's default
		// (DeepSeekDefaultMaxTokens, 8K) already holds a batch of 20
		// (~6000), and a sent limit would be doubled past it on truncation
		// (HTTP 400 instead of ErrTruncated, which the stage splits on).
		maxTokens = openAIMaxTokens(role, tg.deepseek)
		if headroom > 0 {
			if maxTokens <= 0 {
				maxTokens = DeepSeekDefaultMaxTokens
			}
			maxTokens += headroom
		}
	}
	if capped && tg.maxTokens <= 0 {
		c.warnLimit("max_unstreamed", tg, role, "llm: thinking level max holds 28000 tokens for thinking while requests are not streamed (60000 once they are)",
			"max_tokens", maxTokens)
	}
	if limit := outputCap(tg); limit > 0 && maxTokens > limit {
		if tg.maxTokens > 0 {
			c.warnLimit("override_above_cap", tg, role, "llm: the role's max_tokens is above the model's output limit and is sent as that limit",
				"max_tokens", tg.maxTokens, "limit", limit)
		}
		maxTokens = limit
	}
	return maxTokens, headroom
}

// warnLimit logs a note about the output limit of (provider, model, role)
// once per process (INFO: nothing failed).
func (c *HTTPClient) warnLimit(what string, tg target, role, msg string, args ...any) {
	if _, seen := c.limitWarned.LoadOrStore(what+"\x00"+tg.providerID+"\x00"+tg.model+"\x00"+role, true); seen {
		return
	}
	slog.Info(msg, append([]any{"provider", tg.providerName, "model", tg.model, "role", role}, args...)...)
}

// BatchTimeoutTokensPerSentence is the answer a translate batch is given
// time for per target sentence: the estimate behind the batch base
// (BaseMaxTokensTranslateBatch = 20 × 300).
const BatchTimeoutTokensPerSentence = 300

// batchTimeout is the least timeout of one attempt of a translate request
// with more than one target sentence (v3.2.x P2, review R11 B1); 0 for any
// other request, whose timeout stays exactly what it was:
//
//	base + (budget + min(batch, settings.MaxBatch) × 300) × ThinkingTimeoutPerToken
//
// i.e. 12 s per sentence on top of the base timeout (120 s) and the
// thinking budget: batch 2 → 144 s, 10 → 240 s, 20 (the most) → 360 s with
// thinking off. It holds for every provider, also where the timeout does
// not follow max_tokens (DeepSeek, older Claude models). The batch is
// capped at settings.MaxBatch, the whole attempt at MaxAttemptTimeout
// (capTimeout). A batch that still times out is split by the stage.
func (c *HTTPClient) batchTimeout(tg target, role string, budget int) time.Duration {
	if role != model.RoleTranslate || tg.batch <= 1 {
		return 0
	}
	return thinkingTimeout(c.baseTimeout(), budget+min(tg.batch, settings.MaxBatch)*BatchTimeoutTokensPerSentence)
}
