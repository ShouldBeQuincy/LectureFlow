// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Review D7: a request that is not streamed and needs more than
// LongAttemptTimeout for one attempt runs into MaxAttemptTimeout; when it
// times out there it is not sent again (it was sent five times, each
// holding a pool slot for the whole cap). The cap is lowered for the test;
// what counts is the time the request would have needed. One that needs
// less (its timeout cut only by the test's low cap) is retried as before.
func TestLongAttemptTimeoutNotRetried(t *testing.T) {
	shortAttempts(t, 200*time.Millisecond)
	for _, tc := range []struct {
		name      string
		maxTokens int // DeepSeek, the user's value: 120 s + (max_tokens − 4000) × 40 ms
		requests  int
		permanent bool
	}{
		{"needs 66 minutes", 100000, 1, true},
		{"needs 6 minutes", 10000, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			e := newThinkingEnv(t, func(_ int, _ string, w http.ResponseWriter) {
				n.Add(1)
				// hangs until the client gives up (bounded: the test's cap is 200 ms)
				if cn, ok := w.(http.CloseNotifier); ok {
					select {
					case <-cn.CloseNotify():
					case <-time.After(10 * time.Second):
					}
				}
			})
			deepseek := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone) // redirected to the test server
			snap := model.LLMSnapshot{ProviderID: deepseek.ID, ProviderName: deepseek.Name, Model: "deepseek-flash",
				Params: model.RoleParams{MaxTokens: ip(tc.maxTokens)}}
			_, err := e.c.Complete(WithTarget(context.Background(), snap), model.RoleSummary, "s", "u", false)
			if !isCode(err, CodeTimeout, "retries", tc.requests-1) || sched.IsPermanent(err) != tc.permanent {
				t.Fatalf("err %v %+v", err, infoOf(err))
			}
			if got := int(n.Load()); got != tc.requests || len(e.rec.waits) != tc.requests-1 {
				t.Errorf("%d requests, waits %v; want %d requests", got, e.rec.waits, tc.requests)
			}
			if tc.permanent && !strings.Contains(infoOf(err).Detail, "上限") {
				t.Errorf("detail %q", infoOf(err).Detail)
			}
		})
	}
}

// A streamed request is not affected: its attempt timeout runs out before
// the stream delivers (no headers) and it is sent again as before, however
// long it would need (the idle timeout and the stream's time limit are
// its own rules).
func TestLongAttemptTimeoutStreamedRetried(t *testing.T) {
	shortAttempts(t, 200*time.Millisecond)
	s := llmtest.New(t)
	c, set, p, _, _, _ := anthropicClient(t, s.URL)
	setRole(t, set, model.RoleSummary, p.ID, "claude-opus-5-5", model.RoleParams{MaxTokens: ip(ClaudeMaxOutputTokens)})
	c.newIdleTimer = (&virtualIdle{}).new // the idle timeout never runs out here
	s.StreamNext(llmtest.RoleSummary, 5, llmtest.StreamHangHeaders)
	_, err := c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if !isCode(err, CodeTimeout, "retries", 4) || sched.IsPermanent(err) {
		t.Fatalf("err %v %+v", err, infoOf(err))
	}
	if got := s.Count(llmtest.RoleSummary); got != 5 {
		t.Errorf("%d requests, want 5", got)
	}
}
