// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"net"
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

// Review R14: a request marked WithNoDeadlineRetry is not sent again when
// its attempt timeout runs out; the caller (the translate stage) asks for
// less instead. Every other failure is retried as before, and an unmarked
// request is retried exactly as before.

// shortAttempts caps every attempt at d for the test.
func shortAttempts(t *testing.T, d time.Duration) {
	t.Helper()
	prev := MaxAttemptTimeout
	MaxAttemptTimeout = d
	t.Cleanup(func() { MaxAttemptTimeout = prev })
}

// The attempt timeout of a request that is not streamed (OpenAI-compatible):
// marked, sent once and CodeTimeout (not permanent, no backoff wait);
// unmarked, sent five times as before.
func TestNoDeadlineRetryNotStreamed(t *testing.T) {
	shortAttempts(t, 200*time.Millisecond)
	for _, tc := range []struct {
		name     string
		mark     bool
		requests int
		retries  int
	}{
		{"unmarked", false, 5, 4},
		{"marked", true, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := llmtest.New(t)
			c, _, rec := newClient(t, s.URL)
			s.FailNext(llmtest.RoleSummary, 5, llmtest.FailHang)
			ctx := context.Background()
			if tc.mark {
				ctx = WithNoDeadlineRetry(ctx)
			}
			_, err := c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, secretUser, false)
			if !isCode(err, CodeTimeout, "retries", tc.retries) || sched.IsPermanent(err) {
				t.Fatalf("err %v %+v", err, infoOf(err))
			}
			if got := s.Count(llmtest.RoleSummary); got != tc.requests || len(rec.waits) != tc.retries {
				t.Errorf("%d requests, waits %v; want %d requests", got, rec.waits, tc.requests)
			}
			if strings.Contains(err.Error(), secretUser) || strings.Contains(infoOf(err).Detail, secretUser) {
				t.Errorf("the error holds the prompt: %v", err)
			}
		})
	}
}

// The attempt timeout of a streamed request (a current Claude model) before
// the stream delivers: no headers, or message_start and then nothing for
// longer than a quarter of the attempt (not still delivering, SEC5 N1).
// Marked: sent once; unmarked: five times.
func TestNoDeadlineRetryStreamed(t *testing.T) {
	shortAttempts(t, 200*time.Millisecond)
	for _, mode := range []struct {
		name string
		mode llmtest.StreamMode
	}{{"no headers", llmtest.StreamHangHeaders}, {"stalled after message_start", llmtest.StreamHang}} {
		for _, tc := range []struct {
			name     string
			mark     bool
			requests int
		}{{"unmarked", false, 5}, {"marked", true, 1}} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				s, c, _, _, _ := sec4Env(t) // virtual idle timer: the idle timeout never runs out here
				s.StreamNext(llmtest.RoleSummary, 5, mode.mode)
				ctx := context.Background()
				if tc.mark {
					ctx = WithNoDeadlineRetry(ctx)
				}
				_, err := c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
				if !isCode(err, CodeTimeout, "retries", tc.requests-1) || sched.IsPermanent(err) {
					t.Fatalf("err %v %+v", err, infoOf(err))
				}
				if got := s.Count(llmtest.RoleSummary); got != tc.requests {
					t.Errorf("%d requests, want %d", got, tc.requests)
				}
			})
		}
	}
}

// Review R18 suggestion 2 (T4): a marked stream given up after message_start
// is accounted like any failed stream (SEC4 M1): its usage is recorded, the
// call log has its start, its end as a timeout and the call's failed
// record, and the scheduler gets one RequestStart and one RequestEnd.
func TestNoDeadlineRetryStreamedMarkedRecords(t *testing.T) {
	shortAttempts(t, 200*time.Millisecond)
	s, c, usage, _, dir := sec4Env(t) // virtual idle timer: the idle timeout never runs out here
	s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHang)
	st := &sched.StatsRecorder{}
	ctx := sched.WithStatsSink(WithNoDeadlineRetry(context.Background()), st)
	_, err := c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if !isCode(err, CodeTimeout, "retries", 0) || sched.IsPermanent(err) {
		t.Fatalf("err %v %+v", err, infoOf(err))
	}
	var starts, ends []sched.RequestEvent
	for _, ev := range st.Events {
		switch ev.Phase {
		case sched.RequestStart:
			starts = append(starts, ev)
		case sched.RequestEnd:
			ends = append(ends, ev)
		}
	}
	if len(starts) != 1 || len(ends) != 1 || ends[0].Outcome != model.LLMOutcomeTimeout || ends[0].PromptTokens <= 0 {
		t.Errorf("events %s", streamEventLines(st.Events))
	}
	if snap := st.Snapshot(); snap.Requests != 1 || snap.PromptTokens <= 0 {
		t.Errorf("stats %+v", snap)
	}
	if calls := usage.all(); len(calls) != 1 || calls[0].prompt <= 0 {
		t.Errorf("usage %+v", calls)
	}
	_, recs := readCalls(t, dir)
	var results []string
	for _, r := range recs {
		if r.Event == "end" {
			results = append(results, r.Result)
		}
	}
	if got := events(recs); got != "start end failed" || len(results) != 1 || results[0] != "timeout" {
		t.Errorf("call log %q, end results %v", got, results)
	}
}

// The idle timeout of a stream is retried as before, marked or not: nothing
// arrived for a while, which a smaller request does not fix.
func TestNoDeadlineRetryKeepsIdleRetries(t *testing.T) {
	s, c, _, vt, _ := sec4Env(t)
	s.StreamNext(llmtest.RoleSummary, 5, llmtest.StreamHang)
	s.SetStreamHook(func(role, event string) {
		if event == "hang" {
			waitUntil(t, "message_start to be read", func() bool { return vt.resetsOfLatest() >= 2 })
			vt.advance(time.Duration(model.DefaultLLMIdleTimeoutS) * time.Second)
		}
	})
	_, err := c.Complete(WithNoDeadlineRetry(context.Background()), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if !isCode(err, CodeIdleTimeout, "retries", 4) {
		t.Fatalf("err %v %+v", err, infoOf(err))
	}
	if got := s.Count(llmtest.RoleSummary); got != 5 {
		t.Errorf("%d requests, want 5", got)
	}
}

// dialTimeout is a connect timeout (net.Error with Timeout() true).
type dialTimeout struct{}

func (dialTimeout) Error() string   { return "i/o timeout" }
func (dialTimeout) Timeout() bool   { return true }
func (dialTimeout) Temporary() bool { return true }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A connect (or TLS handshake) timeout comes before the attempt's own
// timeout runs out: retried as before, marked or not (CodeTimeout after
// five attempts). Nothing leaves the process: the transport fails at once.
func TestNoDeadlineRetryKeepsConnectTimeoutRetries(t *testing.T) {
	var n atomic.Int32
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		n.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: dialTimeout{}}
	})}
	rec := &recorder{}
	c := New(testConfig(t, "http://127.0.0.1:9"), &fakePool{}, "", hc)
	c.Sleep = rec.sleep
	_, err := c.Complete(WithNoDeadlineRetry(context.Background()), model.RoleTranslate, "s", "u", true)
	if !isCode(err, CodeTimeout, "retries", 4) || sched.IsPermanent(err) {
		t.Fatalf("err %v %+v", err, infoOf(err))
	}
	if n.Load() != 5 || len(rec.waits) != 4 {
		t.Errorf("%d attempts, waits %v; want 5 attempts", n.Load(), rec.waits)
	}
}

// Review R14: the retries of the truncation ladder keep at least the
// batch's timeout (batchTimeout). DeepSeek, batch 20, max_tokens 3000: the
// first attempt has 360 s (the batch's), the doubled retry (6000) would
// have had 200 s alone, the next one (12000) 440 s.
func TestTruncationLadderKeepsBatchTimeout(t *testing.T) {
	per := ThinkingTimeoutPerToken
	e := newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"items\":["},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	deepseek := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone) // redirected to the test server
	snap := model.LLMSnapshot{ProviderID: deepseek.ID, ProviderName: deepseek.Name, Model: "deepseek-flash",
		Params: model.RoleParams{Batch: ip(20), MaxTokens: ip(3000)}}
	st := &sched.StatsRecorder{}
	ctx := sched.WithStatsSink(WithTarget(context.Background(), snap), st)
	if _, err := e.c.Complete(ctx, model.RoleTranslate, "s", "u", true); !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) {
		t.Fatalf("err %v", err)
	}
	var got []time.Duration
	for _, ev := range st.Events {
		if ev.Phase == sched.RequestStart {
			got = append(got, ev.Timeout)
		}
	}
	want := []time.Duration{DefaultTimeout + 6000*per, DefaultTimeout + 6000*per, DefaultTimeout + 8000*per}
	if len(got) != len(want) {
		t.Fatalf("timeouts %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attempt %d: timeout %v, want %v", i, got[i], want[i])
		}
	}
}

// Review R18 suggestion 4 (T4): the same ladder without a batch keeps the
// timeouts it had before batchTimeout existed: 3000 → 120 s, the doubled
// 6000 → 120 s + 2000 × 40 ms, 12000 → 120 s + 8000 × 40 ms.
func TestTruncationLadderTimeoutWithoutBatch(t *testing.T) {
	per := ThinkingTimeoutPerToken
	e := newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"items\":["},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	deepseek := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone) // redirected to the test server
	snap := model.LLMSnapshot{ProviderID: deepseek.ID, ProviderName: deepseek.Name, Model: "deepseek-flash",
		Params: model.RoleParams{MaxTokens: ip(3000)}}
	st := &sched.StatsRecorder{}
	ctx := sched.WithStatsSink(WithTarget(context.Background(), snap), st)
	if _, err := e.c.Complete(ctx, model.RoleTranslate, "s", "u", true); !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) {
		t.Fatalf("err %v", err)
	}
	var got []time.Duration
	for _, ev := range st.Events {
		if ev.Phase == sched.RequestStart {
			got = append(got, ev.Timeout)
		}
	}
	want := []time.Duration{DefaultTimeout, DefaultTimeout + 2000*per, DefaultTimeout + 8000*per}
	if len(got) != len(want) {
		t.Fatalf("timeouts %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attempt %d: timeout %v, want %v", i, got[i], want[i])
		}
	}
}
