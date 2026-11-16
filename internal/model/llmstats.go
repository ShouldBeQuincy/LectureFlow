// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

// LLMStats is JobStats.LLM (v3.2.x P1-c, lead freeze): what the LLM
// requests of a run are doing, for the stage card ("第 2 次尝试 · model ·
// max_tokens", the tokens of each attempt, "N 秒后进行第 3 次尝试"). The
// scheduler builds it from the client's request events (sched.RequestEvent).
type LLMStats struct {
	// InFlight: requests being sent or awaited right now.
	InFlight int `json:"in_flight,omitempty"`
	// Current: the request in flight with the highest attempt number (the
	// one a single-request stage such as summary is waiting for); nil when
	// none is in flight. With RetryAt set it waits for its next attempt.
	Current *LLMAttempt `json:"current,omitempty"`
	// Attempts: the last LLMAttemptsKept finished attempts worth showing,
	// oldest first: every attempt that did not end ok, every attempt after
	// the first of its call, and the first attempt of such a call.
	Attempts []LLMAttempt `json:"attempts,omitempty"`
	// Retries: attempts after the first of their call, over the whole job.
	Retries int `json:"retries,omitempty"`
}

// LLMAttemptsKept is the length limit of LLMStats.Attempts.
const LLMAttemptsKept = 8

// LLMAttempt is one HTTP attempt of an LLM request.
type LLMAttempt struct {
	CallID string `json:"call_id"` // one Complete call (the call log's "call")
	N      int    `json:"n"`       // attempt within the call, 1 = first
	// Try: the stage's request for the same answer, 1 = first (0: unknown).
	Try        int    `json:"try,omitempty"`
	Role       string `json:"role,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	Model      string `json:"model"`
	MaxTokens  int    `json:"max_tokens,omitempty"` // 0 = not sent
	Thinking   string `json:"thinking,omitempty"`   // level sent (Thinking*), "" = off
	StartedAt  string `json:"started_at,omitempty"` // RFC 3339 UTC
	EndedAt    string `json:"ended_at,omitempty"`
	TimeoutMS  int64  `json:"timeout_ms,omitempty"`
	// Phase (streamed requests, Current only): LLMPhaseThinking or
	// LLMPhaseWriting; "" while nothing arrived or without streaming.
	Phase string `json:"phase,omitempty"`
	// RetryAt (Current only): when the next attempt of the call is sent,
	// while the client waits out a backoff.
	RetryAt string `json:"retry_at,omitempty"`
	// Outcome of a finished attempt: LLMOutcome*.
	Outcome          string `json:"outcome,omitempty"`
	Status           int    `json:"status,omitempty"` // HTTP status of an http_error
	EmptyText        bool   `json:"empty_text,omitempty"`
	PromptTokens     int64  `json:"prompt_tokens,omitempty"`
	CompletionTokens int64  `json:"completion_tokens,omitempty"`
	CacheHitTokens   int64  `json:"cache_hit_tokens,omitempty"`
	ReasoningTokens  *int64 `json:"reasoning_tokens,omitempty"`
}

// Outcomes of an attempt: the "result" values of the call log (llm, calllog.go).
const (
	LLMOutcomeOK          = "ok"
	LLMOutcomeTruncated   = "truncated"
	LLMOutcomeHTTPError   = "http_error"
	LLMOutcomeNetwork     = "network"
	LLMOutcomeTimeout     = "timeout"
	LLMOutcomeIdle        = "idle" // streamed request without events for too long (P1-c)
	LLMOutcomeBadResponse = "bad_response"
	LLMOutcomeCanceled    = "canceled"
	// LLMOutcomeRefused: the provider declined to answer (Anthropic
	// stop_reason "refusal" or OpenAI-compatible finish_reason
	// "content_filter"; v3.2.x, lead freeze 2026-11-11).
	LLMOutcomeRefused = "refused"
)

// Phases of a streamed request.
const (
	LLMPhaseThinking = "thinking"
	LLMPhaseWriting  = "writing"
)
