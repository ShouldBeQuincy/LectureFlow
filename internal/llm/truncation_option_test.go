// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// T-PERF-07: a request marked WithNoTruncationRetry is sent once; its
// truncation is the permanent ErrTruncated at once (retries 0), neither one
// thinking level lower (an answer without text) nor with a doubled limit,
// and the billed attempt is counted. Without the mark the same answers take
// the ladder as before (TestTruncationLadder).
func TestNoTruncationRetry(t *testing.T) {
	const r = model.RoleGlossaryJanitor
	for _, tc := range []struct {
		name, model, level string
		empty              bool // the answer has no text
		ladder             string
	}{
		{"text written", "claude-sonnet-5-5", model.ThinkingOff, false, "between_tools//16000 between_tools//32000 between_tools//64000"},
		{"no text, thinking high", "claude-opus-5-5", model.ThinkingHigh, true, "adaptive/high/40000 adaptive/low/40000 adaptive/low/80000"},
		{"no text, thinking off", "claude-opus-5-5", model.ThinkingOff, true, "/low/28000 /low/56000 /low/112000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncated := anthropicTruncated(`{"actions": [`)
			if tc.empty {
				truncated = anthropicTruncated("")
			}
			for _, mark := range []bool{true, false} {
				srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
					if n > 3 { // more than the ladder sends: the test is wrong, answer at once
						io.WriteString(w, anthropicOK(`{"actions": []}`))
						return
					}
					io.WriteString(w, truncated)
				})
				c, set, p, _, rec, usage := anthropicClient(t, srv.URL)
				setRole(t, set, r, p.ID, tc.model, model.RoleParams{Thinking: sp(tc.level)})
				ctx := context.Background()
				if mark {
					ctx = WithNoTruncationRetry(ctx)
				}
				_, err := c.Complete(ctx, r, "SYS", "USR", true)
				if !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) {
					t.Fatalf("mark %v: err %v", mark, err)
				}
				caps := got()
				want, retries := tc.ladder, 2
				if mark {
					first, _, _ := strings.Cut(tc.ladder, " ")
					want, retries = first, 0
				}
				if g := rungs(t, caps); g != want {
					t.Errorf("mark %v: requests %s, want %s", mark, g, want)
				}
				if !isCode(err, CodeTruncated, "retries", retries) {
					t.Errorf("mark %v: err %+v", mark, infoOf(err))
				}
				if len(rec.waits) != 0 {
					t.Errorf("mark %v: waited %v", mark, rec.waits)
				}
				// Every truncated attempt was billed and is counted.
				if calls := usage.all(); len(calls) != len(caps) || calls[0].completion != 4000 {
					t.Errorf("mark %v: usage %+v for %d requests", mark, calls, len(caps))
				}
			}
		})
	}
}
