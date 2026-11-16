// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"time"

	"lectureflow/internal/sched"
)

// RateFeedback is implemented by an API pool that adapts a provider's
// concurrency to its answers (perf-llm; sched.Pool does). The HTTP client
// reports every attempt the pool granted: Throttled after HTTP 429, 503 or
// 529 (sent is when the request went out, so the throttles of one burst
// count once), Succeeded after an answer.
type RateFeedback interface {
	Throttled(provider string, sent time.Time)
	Succeeded(provider string)
}

type sentHookKey struct{}

// WithSentHook returns ctx whose HTTP attempts call fn right before the
// request is sent, after the API pool granted it (every attempt, retries
// included). The stages use it to time the prompt-cache warm-up of a run
// (perf-llm): the first request's prefix should reach the provider before
// the others are sent.
func WithSentHook(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, sentHookKey{}, fn)
}

func sentHookFrom(ctx context.Context) func() {
	fn, _ := ctx.Value(sentHookKey{}).(func())
	return fn
}

type firstEventHookKey struct{}

// WithFirstEventHook returns ctx whose HTTP attempts call fn when the answer
// of a streamed request begins: on its message_start event, the first event
// of a Messages API stream, which the server sends once it has read the
// prompt (stream.go). The stages use it to time the prompt-cache warm-up on
// Anthropic (v3.2.x r2 N46), whose cache becomes readable for other
// requests only once the first request's answer has begun.
//
// fn is called at most once per attempt, from the goroutine reading the
// stream, before the rest of the answer is read; a retried request may call
// it again. Comment lines and pings ahead of message_start (a proxy keeping
// the connection open) do not call it. It is not called for an attempt
// that gets no message_start: a request that is not streamed (an
// OpenAI-compatible provider, an older Claude model, a server that ignored
// "stream" and sent the whole message), an error status, a stream that
// fails or ends before message_start, an idle timeout, a time-out or a
// cancel before it. An error event or a failure after message_start does
// not undo the call. A caller waiting for it must therefore have its own
// limit.
func WithFirstEventHook(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, firstEventHookKey{}, fn)
}

func firstEventHookFrom(ctx context.Context) func() {
	fn, _ := ctx.Value(firstEventHookKey{}).(func())
	return fn
}

var _ RateFeedback = (*sched.Pool)(nil)
