// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Review D6: on an older Claude model the doubled max_tokens of the
// truncation ladder stops at the model's output limit (legacyClaudeOutputCap):
// beyond it the request was an HTTP 400. At the limit the truncation is the
// call's error (permanent, CodeTruncated). Models with a larger limit and
// non-Claude models on an Anthropic endpoint double as before.
func TestTruncationLadderStopsAtLegacyClaudeLimit(t *testing.T) {
	for _, tc := range []struct {
		model   string
		want    string // max_tokens of each request
		tried   string
		retries int
	}{
		{"claude-3-5-sonnet-20241022", "//4000 //8000 //8192", "off/4000, off/8000, off/8192", 2},
		{"claude-3-haiku-20240307", "//4000 //4096", "off/4000, off/4096", 1},
		{"claude-sonnet-4-5", "//4000 //8000 //16000", "off/4000, off/8000, off/16000", 2},
		{"deepseek-chat", "//4000 //8000 //16000", "off/4000, off/8000, off/16000", 2},
	} {
		t.Run(tc.model, func(t *testing.T) {
			srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
				if n > 5 {
					http.Error(w, "too many requests in this test", http.StatusBadRequest)
					return
				}
				io.WriteString(w, anthropicTruncated(`{"patches": [`))
			})
			c, set, p, _, rec, _ := anthropicClient(t, srv.URL)
			setRole(t, set, model.RoleRepair, p.ID, tc.model, model.RoleParams{})
			_, err := c.Complete(context.Background(), model.RoleRepair, "SYS", "USR", true)
			if r := rungs(t, got()); r != tc.want {
				t.Errorf("requests %s, want %s", r, tc.want)
			}
			if !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) || !isCode(err, CodeTruncated, "tried", tc.tried, "retries", tc.retries) {
				t.Fatalf("err %v %+v", err, infoOf(err))
			}
			if len(rec.waits) != 0 {
				t.Errorf("waited %v", rec.waits)
			}
		})
	}
}
