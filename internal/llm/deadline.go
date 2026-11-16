// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import "context"

type noDeadlineRetryKey struct{}

// WithNoDeadlineRetry returns ctx whose requests are not sent again when an
// attempt runs out of its own timeout (v3.2.x P2, review R14): Complete
// returns the timeout (CodeTimeout, its retries 0) at once, and the caller
// asks for less instead — the translate stage splits a batch of more than
// one sentence. Every other failure is retried as before: a connect or TLS
// handshake timeout (the attempt's timeout has not run out), the idle
// timeout of a stream, 429, 5xx and network errors. A stream still coming
// when the attempt's timeout runs out is never sent again anyway
// (CodeTimeLimit).
func WithNoDeadlineRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, noDeadlineRetryKey{}, true)
}

// noDeadlineRetry reports whether ctx was made by WithNoDeadlineRetry.
func noDeadlineRetry(ctx context.Context) bool {
	on, _ := ctx.Value(noDeadlineRetryKey{}).(bool)
	return on
}

type noTruncationRetryKey struct{}

// WithNoTruncationRetry returns ctx whose requests skip the truncation
// ladder (TruncationRetries; Q5a): an answer cut off at max_tokens is
// returned at once as the permanent ErrTruncated (CodeTruncated, retries 0,
// the billed attempt counted as usual), neither at a lower thinking level
// nor with a doubled limit, and the caller asks for less instead — the
// glossary_review stage splits its batch (T-PERF-07, approved by the user
// 2026-11-11). Every other failure is retried as before.
func WithNoTruncationRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, noTruncationRetryKey{}, true)
}

// noTruncationRetry reports whether ctx was made by WithNoTruncationRetry.
func noTruncationRetry(ctx context.Context) bool {
	on, _ := ctx.Value(noTruncationRetryKey{}).(bool)
	return on
}
