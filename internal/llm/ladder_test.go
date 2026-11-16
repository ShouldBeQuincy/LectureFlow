// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// v3.2.x P1-c (client): the truncation ladder, request events, the cap of
// the attempt timeout.

// sentThinking returns thinking.type, output_config.effort and max_tokens of
// a captured Anthropic request ("" = absent).
func sentThinking(t *testing.T, body map[string]any) (thinking, effort string, maxTokens int) {
	t.Helper()
	if th, ok := body["thinking"].(map[string]any); ok {
		thinking, _ = th["type"].(string)
	}
	if oc, ok := body["output_config"].(map[string]any); ok {
		effort, _ = oc["effort"].(string)
	}
	mt, _ := body["max_tokens"].(float64)
	return thinking, effort, int(mt)
}

func rungs(t *testing.T, caps []capture) string {
	t.Helper()
	var out []string
	for _, c := range caps {
		th, ef, mt := sentThinking(t, c.body)
		out = append(out, fmt.Sprintf("%s/%s/%d", th, ef, mt))
	}
	return strings.Join(out, " ")
}

// The ladder on the two models whose "off" has fields of its own, and on a
// plain current Claude model. answers: per request, true = truncated with
// no text, false = truncated with text; past the list: ok.
func TestTruncationLadder(t *testing.T) {
	const r = model.RoleRepair // base 6000
	for _, tc := range []struct {
		name, model, level string
		answers            []bool
		want               string // thinking/effort/max_tokens of each request
		fail               bool
		tried              string
	}{
		// Empty text with thinking on: one level down, the same limit.
		{"opus high, empty then ok", "claude-opus-5-5", model.ThinkingHigh, []bool{true},
			"adaptive/high/30000 adaptive/low/30000", false, ""},
		{"opus max, empty twice then ok", "claude-opus-5-5", model.ThinkingMax, []bool{true, true},
			"adaptive/max/66000 adaptive/high/66000 adaptive/low/66000", false, ""}, // streamed: max holds 60000
		// Text written: doubled, the level stays.
		{"sonnet high, text then ok", "claude-sonnet-5-5", model.ThinkingHigh, []bool{false},
			"adaptive/high/30000 adaptive/high/60000", false, ""},
		// Already at low: doubled.
		{"sonnet high, always empty", "claude-sonnet-5-5", model.ThinkingHigh, []bool{true, true, true},
			"adaptive/high/30000 adaptive/low/30000 adaptive/low/60000", true, "high/30000, low/30000, low/60000"},
		// Off is no rung: opus-5-5 (effort low, thinking runs) and
		// sonnet-5-5 (between_tools) double.
		{"opus off, empty twice then ok", "claude-opus-5-5", model.ThinkingOff, []bool{true, true},
			"/low/18000 /low/36000 /low/72000", false, ""}, // C91: 6000 + 12000
		{"sonnet off, always empty", "claude-sonnet-5-5", model.ThinkingOff, []bool{true, true, true},
			"between_tools//6000 between_tools//12000 between_tools//24000", true, "off/6000, off/12000, off/24000"},
		{"other model low, empty", "claude-test-model", model.ThinkingLow, []bool{true},
			"adaptive/low/14000 adaptive/low/28000", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
				switch {
				case n > len(tc.answers):
					io.WriteString(w, anthropicOK(`{"patches": []}`))
				case tc.answers[n-1]:
					io.WriteString(w, anthropicTruncated(""))
				default:
					io.WriteString(w, anthropicTruncated(`{"patches": [`))
				}
			})
			c, set, p, _, rec, _ := anthropicClient(t, srv.URL)
			setRole(t, set, r, p.ID, tc.model, model.RoleParams{Thinking: sp(tc.level)})
			_, err := c.Complete(context.Background(), r, "SYS", "USR", true)
			if got := rungs(t, got()); got != tc.want {
				t.Errorf("requests %s, want %s", got, tc.want)
			}
			if len(rec.waits) != 0 {
				t.Errorf("waited %v", rec.waits)
			}
			if !tc.fail {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) || !isCode(err, CodeTruncated, "tried", tc.tried, "retries", 2) {
				t.Fatalf("err %v %+v", err, infoOf(err))
			}
			if d := infoOf(err).Detail; !strings.Contains(d, "各次尝试") || !strings.Contains(d, "思考 high") && tc.level == model.ThinkingHigh {
				t.Errorf("detail %q", d)
			}
		})
	}
}

// At the model's output limit an empty answer still goes one level down
// (the limit cannot grow, the thinking can shrink).
func TestTruncationLadderLowersAtOutputLimit(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			io.WriteString(w, anthropicTruncated(""))
			return
		}
		io.WriteString(w, anthropicOK(`{"patches": []}`))
	})
	c, set, p, _, _, _ := anthropicClient(t, srv.URL)
	setRole(t, set, model.RoleRepair, p.ID, "claude-opus-5-5", model.RoleParams{Thinking: sp(model.ThinkingMax), MaxTokens: ip(ClaudeMaxOutputTokens)})
	if _, err := c.Complete(context.Background(), model.RoleRepair, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	if r := rungs(t, got()); r != "adaptive/max/128000 adaptive/high/128000" {
		t.Errorf("requests %s", r)
	}
}

// eventLine is the comparable part of a request event.
func eventLine(ev sched.RequestEvent) string {
	s := fmt.Sprintf("%s(%d)", ev.Phase, ev.N)
	switch ev.Phase {
	case sched.RequestEnd:
		s += ":" + ev.Outcome
	case sched.RequestBackoff:
		s += ":" + ev.RetryIn.String()
	}
	return s
}

func eventLines(evs []sched.RequestEvent) string {
	var out []string
	for _, ev := range evs {
		out = append(out, eventLine(ev))
	}
	return strings.Join(out, " ")
}

// Request events of a call truncated once without text, then answered:
// start(1) → end(truncated) → start(2) → end(ok), with the attempts'
// parameters and tokens, the same call id, a lower level on the second.
func TestRequestEventsTruncatedThenOK(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			io.WriteString(w, anthropicTruncated(""))
			return
		}
		io.WriteString(w, anthropicOK(`{"patches": []}`))
	})
	c, set, p, _, _, _ := anthropicClient(t, srv.URL)
	setRole(t, set, model.RoleRepair, p.ID, "claude-opus-5-5", model.RoleParams{Thinking: sp(model.ThinkingHigh)})
	rec := &sched.StatsRecorder{}
	ctx := sched.WithStatsSink(WithCallTrace(context.Background()), rec)
	if _, err := c.Complete(ctx, model.RoleRepair, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	evs := rec.Events
	if got := eventLines(evs); got != "start(1) end(1):truncated start(2) end(2):ok" {
		t.Fatalf("events %s", got)
	}
	for _, ev := range evs {
		if ev.CallID == "" || ev.CallID != evs[0].CallID || ev.Try != 1 || ev.Role != model.RoleRepair || ev.ProviderID != p.ID ||
			ev.Model != "claude-opus-5-5" || ev.MaxTokens != 30000 || ev.Timeout <= 0 || ev.At.IsZero() {
			t.Errorf("event %+v", ev)
		}
	}
	if evs[0].Thinking != model.ThinkingHigh || evs[2].Thinking != model.ThinkingLow || evs[3].Thinking != model.ThinkingLow {
		t.Errorf("levels %q %q %q", evs[0].Thinking, evs[2].Thinking, evs[3].Thinking)
	}
	// anthropicTruncated: 100 in, 4000 out; anthropicOK: 450 in (300 cached), 20 out
	if e := evs[1]; !e.EmptyText || e.CompletionTokens != 4000 || e.PromptTokens != 100 || e.Status != 0 {
		t.Errorf("end 1 %+v", e)
	}
	if e := evs[3]; e.EmptyText || e.CompletionTokens != 20 || e.PromptTokens != 450 || e.CacheHitTokens != 300 {
		t.Errorf("end 2 %+v", e)
	}
	if evs[1].At.Before(evs[0].At) || evs[2].At.Before(evs[1].At) {
		t.Errorf("times %v %v %v", evs[0].At, evs[1].At, evs[2].At)
	}
	// usage still goes through AddUsage (two billed answers)
	if st := rec.Snapshot(); st.Requests != 2 || st.CompletionTokens != 4020 {
		t.Errorf("stats %+v", st)
	}
}

// 429 with Retry-After: end(http_error, 429) and a backoff with the wait.
// The thinking level off is reported as "".
func TestRequestEventsBackoff(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error": {"message": "slow down"}}`)
			return
		}
		io.WriteString(w, okBody(`{"zh": "缓存"}`))
	})
	c, _, rs := newClient(t, srv.URL)
	rec := &sched.StatsRecorder{}
	ctx := sched.WithStatsSink(context.Background(), rec)
	if _, err := c.Complete(ctx, "translate", "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	if got := eventLines(rec.Events); got != "start(1) end(1):http_error backoff(1):3s start(2) end(2):ok" {
		t.Fatalf("events %s", got)
	}
	if e := rec.Events[1]; e.Status != http.StatusTooManyRequests || e.PromptTokens != 0 || e.Thinking != "" || e.MaxTokens != 0 {
		t.Errorf("end %+v", e)
	}
	if len(rs.waits) != 1 || rs.waits[0] != 3*time.Second {
		t.Errorf("waits %v", rs.waits)
	}
}

// A cancel while the request is in flight ends the attempt with an end
// event (canceled): no start is left without its end.
func TestRequestEventsCanceled(t *testing.T) {
	arrived := make(chan struct{})
	srv := newBlockingServer(t, arrived)
	c, _, _ := newClient(t, srv)
	rec := &sched.StatsRecorder{}
	ctx, cancel := context.WithCancel(sched.WithStatsSink(context.Background(), rec))
	go func() {
		<-arrived
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, err := c.Complete(ctx, "translate", "SYS", "USR", true)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Complete did not return after the cancel")
	}
	if got := eventLines(rec.Events); got != "start(1) end(1):canceled" {
		t.Fatalf("events %s", got)
	}
}

// newBlockingServer answers nothing until the request is cancelled; it
// closes arrived when the first request comes in.
func newBlockingServer(t *testing.T, arrived chan struct{}) string {
	t.Helper()
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // the server notices a closed connection once the body is read
		once.Do(func() { close(arrived) })
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// DeepSeek reports reasoning tokens; the end event carries them.
func TestRequestEventsReasoningTokens(t *testing.T) {
	answer := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"hm","content":"{}"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":900,"prompt_cache_hit_tokens":640,"completion_tokens_details":{"reasoning_tokens":850}}}`
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, answer) })
	c, _, _ := newClient(t, srv.URL)
	rec := &sched.StatsRecorder{}
	if _, err := c.Complete(sched.WithStatsSink(context.Background(), rec), "translate", "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	if len(rec.Events) != 2 {
		t.Fatalf("events %s", eventLines(rec.Events))
	}
	if e := rec.Events[1]; e.ReasoningTokens == nil || *e.ReasoningTokens != 850 || e.CompletionTokens != 900 || e.CacheHitTokens != 640 {
		t.Errorf("end %+v", e)
	}
}

// SEC1 L3: one attempt's timeout is capped at MaxAttemptTimeout (a user
// limit of 128000 tokens would give 84 minutes); the cap is logged, the
// call log and the request event carry the capped value.
func TestAttemptTimeoutCapped(t *testing.T) {
	log := captureLog(t)
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, anthropicOK(`{"patches": []}`)) })
	c, set, p, _, _, _ := anthropicClient(t, srv.URL)
	setRole(t, set, model.RoleRepair, p.ID, "claude-sonnet-5-5", model.RoleParams{MaxTokens: ip(ClaudeMaxOutputTokens)})
	rec := &sched.StatsRecorder{}
	if _, err := c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleRepair, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	if MaxAttemptTimeout != 30*time.Minute {
		t.Fatalf("MaxAttemptTimeout %v", MaxAttemptTimeout)
	}
	if len(rec.Events) == 0 || rec.Events[0].Timeout != MaxAttemptTimeout {
		t.Fatalf("events %+v", rec.Events)
	}
	_, recs := readCalls(t, set.Config().DataDir)
	if recs[0].TimeoutS != int(MaxAttemptTimeout/time.Second) {
		t.Errorf("call log timeout %d s", recs[0].TimeoutS)
	}
	if s := log.String(); !strings.Contains(s, "timeout of one attempt is capped") || !strings.Contains(s, "cap=30m0s") {
		t.Errorf("service log:\n%s", s)
	}
	// Below the cap nothing changes.
	setRole(t, set, model.RoleRepair, p.ID, "claude-sonnet-5-5", model.RoleParams{})
	rec2 := &sched.StatsRecorder{}
	if _, err := c.Complete(sched.WithStatsSink(context.Background(), rec2), model.RoleRepair, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	if want := DefaultTimeout + (BaseMaxTokensRepair-TimeoutFreeTokens)*ThinkingTimeoutPerToken; rec2.Events[0].Timeout != want {
		t.Errorf("timeout %v, want %v", rec2.Events[0].Timeout, want)
	}
}
