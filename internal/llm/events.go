// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Request events (v3.2.x P1-c, client side; design note 3.2 "界面要的数据").
// The scheduler turns them into job.stats.llm (sched/llmstats.go). They are
// reported at the moments the call log writes (calllog.go), from the same
// data:
//
//	start    an HTTP attempt is sent (after the API pool granted it), with
//	         the call log's start record (N = attempt + 1)
//	end      the attempt is over, every time a start was reported — also on
//	         cancel (outcome canceled) — with the outcome, the HTTP status
//	         of an http_error, the attempt's usage (the Result the parsers
//	         return, so DeepSeek semantics: prompt includes cache hits),
//	         reasoning tokens when the provider reports them, empty_text
//	         from the call log's end record
//	phase    a streamed attempt (stream.go) got its first thinking delta
//	         (StreamPhase thinking) or its first text delta (writing); each
//	         at most once per attempt
//	backoff  the call waits RetryIn before its next attempt (a transport
//	         retry; N is the attempt that ended). Truncation retries and the
//	         resend without a schema go out at once and have none.
//	activity events of a streamed attempt keep arriving; at most once per
//	         streamActivityEvery (stream.go), only the job's last activity
//	         follows it (SEC4 L7)
//
// An attempt the API pool did not grant (cancelled while queued) was never
// sent: no start, no end.

// requestEvent returns the fields every event of an attempt of call has.
func requestEvent(phase string, call callInfo, n int, at time.Time) sched.RequestEvent {
	lvl := call.effort
	if lvl == model.ThinkingOff {
		lvl = "" // LLMAttempt.Thinking: "" = off
	}
	return sched.RequestEvent{
		Phase: phase, At: at, CallID: call.id, N: n + 1, Try: call.try,
		Role: call.role, ProviderID: call.provider, Model: call.model,
		MaxTokens: max(call.maxTokens, 0), Thinking: lvl, Timeout: call.timeout,
	}
}

// reportStart reports that attempt n (0 = the first) of call was sent at start.
func reportStart(ctx context.Context, call callInfo, n int, start time.Time) {
	sched.ReportRequest(ctx, requestEvent(sched.RequestStart, call, n, start))
}

// reportEnd reports the end of attempt n of call at at; rec is its end
// record of the call log, res and aerr what the attempt gave.
func reportEnd(ctx context.Context, call callInfo, n int, at time.Time, rec callRecord, res Result, aerr *attemptErr) {
	ev := requestEvent(sched.RequestEnd, call, n, at)
	ev.Outcome = outcomeOf(rec.Result)
	if ev.Outcome == model.LLMOutcomeHTTPError {
		ev.Status = rec.Status
		if aerr != nil && aerr.kind == failStatus && aerr.status != 0 {
			// An error event of a stream (HTTP 200) counts as the status it
			// stands for (529 overloaded, 429, else 500; stream.go).
			ev.Status = aerr.status
		}
	}
	ev.EmptyText = rec.EmptyText
	usage := res
	if aerr != nil {
		usage = aerr.res // the billed answer of a truncation or refusal; zero otherwise
	}
	ev.PromptTokens, ev.CompletionTokens, ev.CacheHitTokens = usage.PromptTokens, usage.CompletionTokens, usage.CacheHitTokens
	if rec.ReasoningTokens != nil {
		v := *rec.ReasoningTokens
		ev.ReasoningTokens = &v
	}
	sched.ReportRequest(ctx, ev)
}

// reportPhase reports that streamed attempt n of call entered phase
// (model.LLMPhase*: its first thinking or text delta arrived).
func reportPhase(ctx context.Context, call callInfo, n int, phase string) {
	ev := requestEvent(sched.RequestPhase, call, n, time.Now())
	ev.StreamPhase = phase
	sched.ReportRequest(ctx, ev)
}

// reportActivity reports that events of streamed attempt n of call keep
// arriving (the job's last activity; throttled by attempt, SEC4 L7).
func reportActivity(ctx context.Context, call callInfo, n int, at time.Time) {
	sched.ReportRequest(ctx, requestEvent(sched.RequestActivity, call, n, at))
}

// reportBackoff reports that call waits wait after attempt n (0 = the first).
func reportBackoff(ctx context.Context, call callInfo, n int, wait time.Duration) {
	ev := requestEvent(sched.RequestBackoff, call, n, time.Now())
	ev.RetryIn = wait
	sched.ReportRequest(ctx, ev)
}

// outcomeOf maps the call log's result to model.LLMOutcome*. "refused" (a
// refusal: HTTP 200 whose answer is not the one asked for, failRefusal) is
// model.LLMOutcomeRefused (R8 E-2).
func outcomeOf(result string) string {
	switch result {
	case "ok":
		return model.LLMOutcomeOK
	case "truncated":
		return model.LLMOutcomeTruncated
	case "http_error":
		return model.LLMOutcomeHTTPError
	case "timeout":
		return model.LLMOutcomeTimeout
	case "canceled":
		return model.LLMOutcomeCanceled
	case "network":
		return model.LLMOutcomeNetwork
	case "idle":
		return model.LLMOutcomeIdle
	case "refused":
		return model.LLMOutcomeRefused
	case "time_limit":
		// The frozen list has no outcome of its own for it: the attempt
		// timeout ran out (the call's error, llm.time_limit, tells it apart).
		return model.LLMOutcomeTimeout
	case "insufficient_resource":
		// The frozen list has no outcome of its own for it either: HTTP 200
		// whose answer was cut short by the server (N50b), not usable.
		return model.LLMOutcomeBadResponse
	}
	return model.LLMOutcomeBadResponse // bad_response
}
