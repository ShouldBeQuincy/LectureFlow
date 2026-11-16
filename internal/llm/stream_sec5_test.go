// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// SEC5: the recheck of the streamed Anthropic requests (N1, N2).

// sec5Env is a client of the fake server, every role on claude-opus-5-5
// (streamed), with its backoff recorder, usage recorder and data dir. The
// idle timeout stays at its default and runs on a virtual clock that is never
// advanced: it never fires.
func sec5Env(t *testing.T) (*llmtest.Server, *HTTPClient, *recorder, *fakeRecorder, string) {
	t.Helper()
	s := llmtest.New(t)
	c, set, p, _, rec, usage := anthropicClient(t, s.URL)
	for _, role := range model.Roles {
		setRole(t, set, role, p.ID, "claude-opus-5-5", model.RoleParams{})
	}
	c.newIdleTimer = (&virtualIdle{}).new
	if got := c.idleTimeout(); got != time.Duration(model.DefaultLLMIdleTimeoutS)*time.Second {
		t.Fatalf("idle timeout %v", got)
	}
	return s, c, rec, usage, set.Config().DataDir
}

// endOutcomes lists the outcomes of the RequestEnd events; starts counts the
// RequestStart events.
func endOutcomes(evs []sched.RequestEvent) (outcomes []string, starts int) {
	for _, ev := range evs {
		switch ev.Phase {
		case sched.RequestStart:
			starts++
		case sched.RequestEnd:
			outcomes = append(outcomes, ev.Outcome)
		}
	}
	return outcomes, starts
}

// N1 end to end: an attempt timeout shorter than the idle timeout — 120 s
// against 180 s (the default until 2026-11-11, now 90 s) for thinking-off
// translations and summaries; here 300 ms against the default, the idle
// timer never firing. The stream stalls
// right after message_start (StreamHang): when the attempt timeout runs out
// the last event is long before the end, so it is an ordinary timeout,
// retried after the backoff, not a permanent time_limit. The retry heals it;
// a stall on every attempt ends as a timeout that is not permanent.
func TestSEC5StalledShortRequestRetried(t *testing.T) {
	defer func(v time.Duration) { MaxAttemptTimeout = v }(MaxAttemptTimeout)
	MaxAttemptTimeout = 300 * time.Millisecond

	t.Run("healed by the retry", func(t *testing.T) {
		s, c, rec, usage, dir := sec5Env(t)
		s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHang)
		st := &sched.StatsRecorder{}
		res, err := c.Complete(sched.WithStatsSink(context.Background(), st), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
		if err != nil || res.Text == "" {
			t.Fatalf("res %+v err %v", res, err)
		}
		if s.Count(llmtest.RoleSummary) != 2 || len(rec.waits) != 1 || rec.waits[0] != c.Backoff[0] {
			t.Fatalf("%d requests, waits %v", s.Count(llmtest.RoleSummary), rec.waits)
		}
		outcomes, starts := endOutcomes(st.Events)
		if starts != 2 || strings.Join(outcomes, " ") != model.LLMOutcomeTimeout+" "+model.LLMOutcomeOK {
			t.Errorf("events %s", streamEventLines(st.Events))
		}
		// The stall came after message_start: the first attempt counts its
		// input (SEC4 M1), so stillStreaming saw a started stream.
		if calls := usage.all(); len(calls) != 2 || calls[0].prompt <= 0 || calls[0].prompt != calls[1].prompt {
			t.Errorf("usage %+v", calls)
		}
		_, recs := readCalls(t, dir)
		if events(recs) != "start end start end" || recs[1].Result != "timeout" || recs[3].Result != "ok" {
			t.Errorf("calls %+v", recs)
		}
	})

	t.Run("stalled every time", func(t *testing.T) {
		s, c, rec, _, _ := sec5Env(t)
		c.Backoff = c.Backoff[:1]
		s.StreamNext(llmtest.RoleSummary, 2, llmtest.StreamHang)
		_, err := c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
		if err == nil || sched.IsPermanent(err) || !isCode(err, CodeTimeout, "retries", 1, "seconds", 0.3) {
			t.Fatalf("err %v %+v", err, infoOf(err))
		}
		if d := infoOf(err).Detail; strings.Contains(d, "单次请求的时限") || strings.Contains(d, "总时限") || strings.Contains(d, "思考档位") {
			t.Errorf("detail %q", d)
		}
		if s.Count(llmtest.RoleSummary) != 2 || len(rec.waits) != 1 {
			t.Errorf("%d requests, waits %v", s.Count(llmtest.RoleSummary), rec.waits)
		}
	})
}

// N1: the window within which the last event must lie for a timed-out
// attempt to count as still streaming — one idle timeout (its default when
// off), at most a quarter of the attempt timeout — and what answer makes of
// a timed-out attempt with the numbers of a short request (attempt timeout
// 120 s, idle timeout 180 s), at the edges of the window and with the cap.
func TestSEC5TimeLimitWindow(t *testing.T) {
	const s, m = time.Second, time.Minute
	for _, tc := range []struct {
		idle, timeout, want time.Duration
	}{
		{180 * s, 120 * s, 30 * s},
		{0, 120 * s, 30 * s},
		{180 * s, 200 * s, 50 * s},
		{180 * s, 30 * m, 180 * s},
		{0, 30 * m, 90 * s}, // off: the default idle timeout
		{60 * s, 30 * m, 60 * s},
		{180 * s, 0, 0},
	} {
		if got := stillStreamingWindow(callInfo{idle: tc.idle, timeout: tc.timeout}); got != tc.want {
			t.Errorf("idle %v, timeout %v: window %v, want %v", tc.idle, tc.timeout, got, tc.want)
		}
	}

	c := &HTTPClient{}
	for _, tc := range []struct {
		name               string
		idle, timeout      time.Duration
		lastEvent, endedAt time.Duration
		want               failKind
	}{
		// the stall of SEC5 N1: message_start at 2 s, then nothing
		{"stalled after 2 s", 180 * s, 120 * s, 2 * s, 120 * s, failTimeout},
		{"stalled after 2 s, idle off", 0, 120 * s, 2 * s, 120 * s, failTimeout},
		// the window of 30 s: the last event just inside it, right at its
		// edge (outside), and before it
		{"last event just inside the window", 180 * s, 120 * s, 90*s + time.Millisecond, 120 * s, failTimeLimit},
		{"last event at the edge of the window", 180 * s, 120 * s, 90 * s, 120 * s, failTimeout},
		{"last event before the window", 180 * s, 120 * s, 89 * s, 120 * s, failTimeout},
		{"events until the end", 180 * s, 120 * s, 119 * s, 120 * s, failTimeLimit},
		// the cap of 30 minutes: the window is the idle timeout
		{"cap, events until the end", 180 * s, 30 * m, 29*m + 55*s, 30 * m, failTimeLimit},
		{"cap, last event just inside", 180 * s, 30 * m, 27*m + time.Millisecond, 30 * m, failTimeLimit},
		{"cap, last event at the edge", 180 * s, 30 * m, 27 * m, 30 * m, failTimeout},
	} {
		call := callInfo{stream: true, idle: tc.idle, timeout: tc.timeout}
		out := attemptOutcome{status: 200, duration: tc.endedAt, stream: &streamOutcome{started: true, lastEvent: tc.lastEvent,
			firstToken: -1, firstText: -1, usage: Result{PromptTokens: 10}, hasUsage: true}}
		_, aerr := c.answer(context.Background(), call, out, context.DeadlineExceeded)
		if aerr == nil || aerr.kind != tc.want || aerr.retryable() != (tc.want == failTimeout) || aerr.permanent != (tc.want == failTimeLimit) ||
			!aerr.usage || aerr.res.PromptTokens != 10 {
			t.Errorf("%s: %+v", tc.name, aerr)
		}
	}
}

// sec5End ends a stream: stop_reason end_turn, output_tokens 9, message_stop.
const sec5End = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// N2: a known event whose field the answer is read from has another type
// than the API's — the stream fails as unreadable (retried), with the field's
// name and nothing of the data, instead of losing part of the answer or its
// stop reason; the usage reported so far is kept (SEC4 M1). End to end the
// attempt is sent again and the retry's answer is the result; the lost text
// is nowhere in the call log or the service log.
func TestSEC5WrongTypedFieldsFail(t *testing.T) {
	const lost = "LOST-PART-OF-THE-ANSWER"
	for _, tc := range []struct {
		name, event, data, field string
		completion               int64 // output tokens reported by the bad event itself (message_delta), else message_start's 1
	}{
		{"delta a string", "content_block_delta", `{"type":"content_block_delta","index":0,"delta":"` + lost + `"}`, "delta", 1},
		{"delta an array", "content_block_delta", `{"type":"content_block_delta","index":0,"delta":[{"type":"text_delta","text":"` + lost + `"}]}`, "delta", 1},
		{"delta.type a number", "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":7,"text":"` + lost + `"}}`, "delta.type", 1},
		{"message_delta.delta a string", "message_delta", `{"type":"message_delta","delta":"max_tokens","usage":{"output_tokens":4000}}`, "delta", 4000},
		{"message_delta.delta a number", "message_delta", `{"type":"message_delta","delta":5,"usage":{"output_tokens":4000}}`, "delta", 4000},
		{"stop_reason not a string", "message_delta", `{"type":"message_delta","delta":{"stop_reason":["max_tokens"]},"usage":{"output_tokens":4000}}`, "delta.stop_reason", 4000},
		{"content_block a string", "content_block_start", `{"type":"content_block_start","index":1,"content_block":"` + lost + `"}`, "content_block", 1},
		{"content_block.type an object", "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":{"is":"text"},"text":"` + lost + `"}}`, "content_block.type", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sse := sec4Start + sec4Text("Hel") + "event: " + tc.event + "\ndata: " + tc.data + "\n\n" + sec4Text("lo") + sec5End
			out, err := readAnthropicStream(strings.NewReader(sse), testKey, streamHooks{since: func() time.Duration { return 0 }})
			if err != nil || out.aerr == nil || out.aerr.kind != failEnvelope || !out.aerr.retryable() || out.aerr.permanent {
				t.Fatalf("err %v aerr %+v", err, out.aerr)
			}
			if want := fmt.Sprintf("event %s, %d bytes of data, unexpected type of %s", tc.event, len(tc.data), tc.field); out.aerr.body != want {
				t.Errorf("body %q, want %q", out.aerr.body, want)
			}
			if out.res.Text != "" || out.meta.parsed || out.aerr.res.Text != "" {
				t.Errorf("an answer from a failed stream: res %+v aerr res %+v parsed %v", out.res, out.aerr.res, out.meta.parsed)
			}
			if !out.aerr.usage || out.aerr.res.PromptTokens != 140 || out.aerr.res.CompletionTokens != tc.completion {
				t.Errorf("usage %+v", out.aerr)
			}

			logs := captureLog(t)
			ok := sec4OK(t, `{"ok": true}`)
			s := newSEC4Server(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n == 1 {
					writeSSE(w, sse)
					return
				}
				writeSSE(w, ok)
			})
			c, rec, _, dir := sec4Client(t, s.URL)
			res, cerr := c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
			if cerr != nil || res.Text != `{"ok": true}` || s.count() != 2 || len(rec.waits) != 1 {
				t.Fatalf("res %+v err %v, %d requests, waits %v", res, cerr, s.count(), rec.waits)
			}
			raw, recs := readCalls(t, dir)
			if events(recs) != "start end start end" || recs[1].Result != "bad_response" {
				t.Errorf("calls %+v", recs)
			}
			for where, text := range map[string]string{"aerr body": out.aerr.body, "aerr err": out.aerr.err.Error(),
				"call log": raw, "service log": logs.String()} {
				if strings.Contains(text, lost) {
					t.Errorf("%s holds the answer: %s", where, text)
				}
			}
		})
	}
}

// N2 does not undo SEC4 M2: absent or null fields, deltas and blocks of new
// kinds, fields of new kinds, and the fields this client does not read the
// answer from (id, usage, stop_details, signature, thinking) with another
// type are ignored; unknown events too, whatever their fields. The answer is
// read as usual.
func TestSEC5LenientFieldsTolerated(t *testing.T) {
	ev := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":42,\"usage\":{\"input_tokens\":10,\"output_tokens\":1},\"stop_reason\":7}}\n\n" +
		ev("content_block_start", `{"type":"content_block_start","index":0}`) +
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":null}`) +
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":null}}`) +
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":null,"citations":7}}`) +
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","input":"x","id":5}}`) +
		ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":5,"signature":[]}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":null}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":null}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":"c","text":5}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":7}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"signature_delta","signature":5}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"thinking_delta","thinking":["x"]}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":null}}`) +
		sec4Text("Hel") +
		ev("future", `{"type":"future","delta":"x","content_block":7}`) +
		ev("future_delta", `{"type":"future_delta","delta":{"type":7}}`) +
		"data: {\"type\":\"unnamed_future\",\"delta\":5}\n\n" +
		sec4Text("lo") +
		ev("message_delta", `{"type":"message_delta","usage":{"output_tokens":3}}`) +
		ev("message_delta", `{"type":"message_delta","delta":null,"usage":"u"}`) +
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":null,"stop_details":[1],"stop_sequence":5}}`) +
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_details":"weird"},"usage":{"output_tokens":9}}`) +
		ev("message_stop", `{"type":"message_stop"}`)
	out, err := readAnthropicStream(strings.NewReader(sse), testKey, streamHooks{since: func() time.Duration { return 0 }})
	if err != nil || out.aerr != nil {
		t.Fatalf("err %v %+v", err, out.aerr)
	}
	if out.res != (Result{Text: "Hello", PromptTokens: 10, CompletionTokens: 9}) || out.meta.stopReason != "end_turn" || !out.meta.parsed {
		t.Errorf("result %+v meta %+v", out.res, out.meta)
	}
}
