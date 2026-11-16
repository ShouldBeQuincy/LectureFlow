// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"lectureflow/internal/model"
)

// Anthropic Messages API adapter (SPEC-v1.1 A6). Transport, retries and
// error classification are shared with the OpenAI-compatible path (http.go);
// only the request body, the auth headers and the response differ.

const (
	// AnthropicVersion is the anthropic-version header of A6.
	AnthropicVersion = "2023-06-01"
	// DefaultAnthropicMaxTokens is max_tokens of an Anthropic request for
	// every role without its own value (SPEC-v2 appendix D2: 4000). The
	// Messages API requires the field; thinking counts against it.
	DefaultAnthropicMaxTokens = 4000
	// JanitorAnthropicMaxTokens is max_tokens of the glossary_janitor role
	// (appendix D2): its answer lists actions for a whole glossary.
	JanitorAnthropicMaxTokens = 16000
	// stopMaxTokens is the stop_reason of an answer cut off at max_tokens;
	// appendix D2 counts it as a failure (see HTTPClient.Complete).
	stopMaxTokens = "max_tokens"
	// stopRefusal is the stop_reason of a request the provider's safety
	// classifiers declined (HTTP 200; stop_details.category says which).
	stopRefusal = "refusal"
	// JSONInstruction is appended to the user message in JSON mode: the
	// Messages API has no response_format (A6). The answer still goes
	// through the tolerant ParseJSON of SPEC §12.
	JSONInstruction = "\n\nRespond with JSON only."
	// StatusOverloaded is Anthropic's 529 overloaded_error, retried like 5xx (A6).
	StatusOverloaded = 529
)

// AnthropicMaxTokensFor returns max_tokens of an Anthropic request of role
// (appendix D2): glossary_janitor 16000; chapters, organizer and every
// other role 4000. A truncated answer is retried once with the limit doubled.
func AnthropicMaxTokensFor(role string) int {
	switch role {
	case model.RoleGlossaryJanitor:
		return JanitorAnthropicMaxTokens
	case model.RoleChapters, model.RoleOrganizer:
		return 4000
	}
	return DefaultAnthropicMaxTokens
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicTextBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string               `json:"model"`
	MaxTokens int                  `json:"max_tokens"`
	System    []anthropicTextBlock `json:"system,omitempty"`
	Messages  []anthropicMessage   `json:"messages"`
	// Thinking and OutputConfig: the role's "深度思考" (thinking.go,
	// anthropicThinkingFields) and the JSON Schema of the answer.
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	// Stream: the answer comes as server-sent events (stream.go; current
	// Claude models only). Last and omitted when false, so a request that is
	// not streamed is byte for byte what it was.
	Stream bool `json:"stream,omitempty"`
}

// Structured output (v3.2.x P1, Q3a). A JSON request to a Claude model of an
// Anthropic provider carries the JSON Schema of its answer:
//
//	"output_config": {"format": {"type": "json_schema", "schema": {…}}}
//
// in the same output_config object as the effort. The stage attaches the
// schema with WithJSONSchema (ctxbuild has one per JSON role); without one,
// or for any other provider or model name, the request is what it was. The
// answer is then one JSON value of that schema, so the model cannot append
// second thoughts to it. Limits of the schema language: every object needs
// "additionalProperties": false and lists all its properties as required;
// no length or range constraints (the stages' checks keep those).
//
// Models that do not know the field answer HTTP 400 (Sonnet 4.6, Opus 4.6
// and 4.7 are not listed as supporting it): the call is sent once more
// without the schema and, when that works, the client remembers for its
// lifetime that (provider, model) takes none. JSONInstruction and the
// tolerant ParseJSON stay in every case.
type anthropicFormat struct {
	Type   string          `json:"type"` // "json_schema"
	Schema json.RawMessage `json:"schema"`
}

type jsonSchemaKey struct{}

// WithJSONSchema returns ctx whose JSON-mode requests ask for an answer of
// schema (a JSON Schema object) where the provider can enforce one.
func WithJSONSchema(ctx context.Context, schema string) context.Context {
	return context.WithValue(ctx, jsonSchemaKey{}, json.RawMessage(schema))
}

func jsonSchemaFrom(ctx context.Context) json.RawMessage {
	s, _ := ctx.Value(jsonSchemaKey{}).(json.RawMessage)
	return s
}

// structuredModel reports whether a model name of an Anthropic provider
// may be sent a schema: Claude models only (DeepSeek's Anthropic-compatible
// endpoint and the like are left alone).
func structuredModel(modelName string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelName)), "claude-")
}

// anthropicBody builds the request of A6: the fixed prefix as the single
// system block, marked cache_control ephemeral (prompt caching; the prefix
// is byte-identical for one lecture and role, SPEC §12), the variable part
// as the user message.
//
// No temperature is sent: current Claude models reject sampling
// parameters other than the defaults (and API version 2026-10-21 removes
// them on every model), so a role's temperature applies to
// OpenAI-compatible providers only (NOTES.md, v1.1 settings-builder).
func anthropicBody(tg target, maxTokens int, system, user string, jsonMode bool, schema json.RawMessage) ([]byte, error) {
	if maxTokens <= 0 {
		maxTokens = DefaultAnthropicMaxTokens
	}
	// Thinking off: no thinking parameter, except on the Claude models that
	// think without one (thinking.go, anthropicOffFields); also not for
	// DeepSeek's Anthropic-compatible endpoint: its documentation gives no
	// reliable way to switch thinking off in this format (appendix D2;
	// NOTES.md, v2 llm-builder). Thinking on: thinking.go (maxTokens already
	// includes the budget).
	if jsonMode {
		user += JSONInstruction
	}
	req := anthropicRequest{
		Model:     tg.model,
		MaxTokens: maxTokens,
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
	}
	req.Thinking, req.OutputConfig = anthropicThinkingFields(tg.model, tg.thinking)
	if jsonMode && len(schema) > 0 {
		if req.OutputConfig == nil {
			req.OutputConfig = &anthropicOutputConfig{}
		}
		req.OutputConfig.Format = &anthropicFormat{Type: "json_schema", Schema: schema}
	}
	if system != "" { // the API rejects empty text blocks
		req.System = []anthropicTextBlock{{Type: "text", Text: system, CacheControl: &anthropicCacheControl{Type: "ephemeral"}}}
	}
	req.Stream = streamed(tg)
	return json.Marshal(req)
}

type anthropicResponse struct {
	Type    string `json:"type"`
	Content *[]struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason  string `json:"stop_reason"`
	StopDetails *struct {
		Category string `json:"category"`
	} `json:"stop_details"`
	Usage *struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// parseAnthropic reads a 200 response of /v1/messages. The text is the
// concatenation of the text blocks (thinking blocks are skipped).
//
// Usage is mapped to the DeepSeek semantics used everywhere else (job.stats,
// usage_daily, cost estimates: prompt tokens INCLUDE cache hits):
// prompt = input_tokens + cache_read_input_tokens + cache_creation_input_tokens,
// cache_hit = cache_read_input_tokens, completion = output_tokens.
//
// stop_reason "max_tokens" is a failTruncated error that carries the
// response's usage (the tokens were spent and billed). stop_reason
// "refusal" is a failRefusal error with the usage and the category: what
// such an answer holds is not the answer asked for (and need not follow the
// schema), so it is never handed to a stage.
func parseAnthropic(body []byte, key string) (Result, *attemptErr) {
	var ar anthropicResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return Result{}, &attemptErr{kind: failEnvelope, body: redactSnippet(body, key), err: err}
	}
	if ar.Content == nil && ar.StopReason != stopRefusal {
		return Result{}, &attemptErr{kind: failEnvelope, body: redactSnippet(body, key), err: errors.New("no content in response")}
	}
	var sb strings.Builder
	if ar.Content != nil {
		for _, b := range *ar.Content {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
	}
	res := Result{Text: sb.String()}
	if u := ar.Usage; u != nil {
		res.PromptTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		res.CacheHitTokens = u.CacheReadInputTokens
		res.CompletionTokens = u.OutputTokens
	}
	switch ar.StopReason {
	case stopMaxTokens:
		return Result{}, &attemptErr{kind: failTruncated, res: res, err: errors.New("stop_reason max_tokens")}
	case stopRefusal:
		ae := &attemptErr{kind: failRefusal, res: res, err: errors.New("stop_reason refusal")}
		if ar.StopDetails != nil {
			ae.category = CleanRefusalCategory(ar.StopDetails.Category)
		}
		return Result{}, ae
	}
	return res, nil
}

// MaxRefusalCategoryLen is the longest stop_details.category kept as sent.
const MaxRefusalCategoryLen = 32

// RefusalCategoryOther stands for a category that is not a short
// identifier (CleanRefusalCategory).
const RefusalCategoryOther = "other"

// CleanRefusalCategory returns the refusal category of an answer as this
// app passes it on (job error, qa_error, logs, calls file): the provider's
// value when it is a short identifier of lower-case letters, digits and
// underscores (Anthropic sends enums such as "cyber"), "" when none came,
// RefusalCategoryOther for anything else. The provider may be any
// Anthropic-compatible endpoint the user entered, so the value is untrusted
// input: it must not carry text of any length, or an echoed key, into the
// database and the service log (SEC1 L1).
func CleanRefusalCategory(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > MaxRefusalCategoryLen {
		return RefusalCategoryOther
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return RefusalCategoryOther
		}
	}
	return s
}
