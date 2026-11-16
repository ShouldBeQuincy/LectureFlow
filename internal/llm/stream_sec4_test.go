// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// SEC4: the security review of the streamed Anthropic requests (M1–M5, L1–L4,
// L7, L9, L10).

const sec4Start = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":40,\"output_tokens\":1}}}\n\n"

func sec4Text(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	return "event: content_block_delta\ndata: " + string(b) + "\n\n"
}

func sec4ErrorEvent(typ, msg string) string {
	return fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":%q,\"message\":%q}}\n\n", typ, msg)
}

// sec4OK is the whole stream of an answer with text.
func sec4OK(t *testing.T, text string) string {
	t.Helper()
	b, err := llmtest.MessageSSE([]byte(anthropicOK(text)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeSSE(w http.ResponseWriter, raw string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, raw)
}

// sec4Server answers request n (from 1) with respond(n, w, r).
type sec4Server struct {
	*httptest.Server
	mu     sync.Mutex
	n      int
	bodies []map[string]any
}

func newSEC4Server(t *testing.T, respond func(n int, w http.ResponseWriter, r *http.Request)) *sec4Server {
	t.Helper()
	s := &sec4Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.n++
		n := s.n
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		respond(n, w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sec4Server) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// sec4Client is a settings-mode client of s, every role on claude-opus-5-5
// (streamed).
func sec4Client(t *testing.T, url string) (*HTTPClient, *recorder, *fakeRecorder, string) {
	t.Helper()
	c, set, p, _, rec, usage := anthropicClient(t, url)
	for _, role := range model.Roles {
		setRole(t, set, role, p.ID, "claude-opus-5-5", model.RoleParams{})
	}
	return c, rec, usage, set.Config().DataDir
}

// M2: unknown event types, unknown fields and new fields whose names are
// those of other events' fields (with another type) are ignored; the answer
// is read as usual. (A content_block_delta whose delta is a string is no new
// field but the known one with another type: it fails the stream since SEC5
// N2, TestSEC5WrongTypedFieldsFail.)
func TestSEC4UnknownEventsAndFieldsTolerated(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"usage\":\"top-level string\",\"delta\":5,\"message\":{\"id\":\"msg_t\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1},\"content\":\"new\"}}\n\n" +
		"event: future_notice\ndata: {\"type\":\"future_notice\",\"message\":\"heads up\"}\n\n" +
		"event: future\ndata: {\"type\":\"future\",\"delta\":\"x\",\"usage\":\"y\",\"error\":\"z\",\"content_block\":7}\n\n" +
		"event: weird\ndata: not json at all\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"message\":\"m\",\"content_block\":{\"type\":\"text\",\"text\":\"\",\"citations\":[1]}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":\"zero\",\"message\":\"m\",\"usage\":\"u\",\"error\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\",\"extra\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0,\"delta\":[]}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"message\":\"x\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_details\":\"weird\",\"stop_sequence\":5},\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\",\"usage\":\"x\"}\n\n"
	out, err := readAnthropicStream(strings.NewReader(sse), testKey, streamHooks{since: func() time.Duration { return 0 }})
	if err != nil || out.aerr != nil {
		t.Fatalf("err %v %+v", err, out.aerr)
	}
	if out.res != (Result{Text: "Hello", PromptTokens: 10, CompletionTokens: 9}) || out.meta.id != "msg_t" || out.meta.stopReason != "end_turn" {
		t.Errorf("result %+v meta %+v", out.res, out.meta)
	}
}

// M2 end to end: a new event with a field named like another event's field
// does not fail the request, which is sent once.
func TestSEC4UnknownEventRequest(t *testing.T) {
	ok := sec4OK(t, `{"ok": true}`)
	s := newSEC4Server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		writeSSE(w, strings.Replace(ok, "event: ping\n", "event: future_notice\ndata: {\"type\":\"future_notice\",\"message\":\"heads up\"}\n\nevent: ping\n", 1))
	})
	c, rec, _, _ := sec4Client(t, s.URL)
	res, err := c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if err != nil || res.Text != `{"ok": true}` || s.count() != 1 || len(rec.waits) != 0 {
		t.Fatalf("res %+v err %v, %d requests", res, err, s.count())
	}
}

// M2: an error event whose error is a string (a gateway's) is a server
// error, retried; its text is kept, cut.
func TestSEC4GatewayErrorEvent(t *testing.T) {
	out, err := readAnthropicStream(strings.NewReader(sec4Start+"event: error\ndata: {\"type\":\"error\",\"error\":\"upstream failed\"}\n\n"),
		testKey, streamHooks{since: func() time.Duration { return 0 }})
	if err != nil || out.aerr == nil || out.aerr.kind != failStatus || out.aerr.status != 500 || !out.aerr.retryable() ||
		!strings.Contains(out.aerr.body, "upstream failed") || out.errorType != "" {
		t.Fatalf("err %v aerr %+v", err, out.aerr)
	}
}

// M3: an event that cannot be read leaves nothing of its data — the answer —
// in the error, the call log, the service log or the job's error; only its
// name, its size and the kind of error.
func TestSEC4UnreadableEventKeepsNoContent(t *testing.T) {
	const secret1, secret2 = "SECRET-ANSWER-ONE", "SECRET-ANSWER-TWO"
	for name, tc := range map[string]struct {
		bad, kind string
	}{
		"bad escape": {"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + secret2 + " \\x\"}}\n\n", "syntax error"},
		"text type":  {"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":[\"" + secret2 + "\"]}}\n\n", "unexpected type"},
		"cut json":   {"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"" + secret2 + "\n\n", "syntax error"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLog(t)
			sse := sec4Start + sec4Text(secret1) + tc.bad
			out, err := readAnthropicStream(strings.NewReader(sse), testKey, streamHooks{since: func() time.Duration { return 0 }})
			if err != nil || out.aerr == nil || out.aerr.kind != failEnvelope || !out.aerr.retryable() {
				t.Fatalf("err %v aerr %+v", err, out.aerr)
			}
			if !strings.Contains(out.aerr.body, "content_block_delta") || !strings.Contains(out.aerr.body, tc.kind) || !strings.Contains(out.aerr.body, "bytes") {
				t.Errorf("body %q", out.aerr.body)
			}
			s := newSEC4Server(t, func(n int, w http.ResponseWriter, r *http.Request) { writeSSE(w, sse) })
			c, _, _, dir := sec4Client(t, s.URL)
			ctx := sched.WithJob(context.Background(), model.Job{ID: "01SEC4M3"})
			_, cerr := c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
			if cerr == nil || !isCode(cerr, CodeBadResponse) || s.count() != 5 {
				t.Fatalf("err %v, %d requests", cerr, s.count())
			}
			raw, _ := readCalls(t, dir)
			for where, text := range map[string]string{"aerr body": out.aerr.body, "aerr err": out.aerr.err.Error(),
				"call log": raw, "service log": logs.String(), "job error": cerr.Error(), "detail": infoOf(cerr).Detail} {
				for _, secret := range []string{secret1, secret2} {
					if strings.Contains(text, secret) {
						t.Errorf("%s holds %q: %s", where, secret, text)
					}
				}
			}
		})
	}
}

// sec4Env is a client of the fake server on claude-opus-5-5 with a virtual
// idle timer, its usage recorder and request events.
func sec4Env(t *testing.T) (*llmtest.Server, *HTTPClient, *fakeRecorder, *virtualIdle, string) {
	t.Helper()
	s := llmtest.New(t)
	c, set, p, _, _, usage := anthropicClient(t, s.URL)
	for _, role := range model.Roles {
		setRole(t, set, role, p.ID, "claude-opus-5-5", model.RoleParams{})
	}
	vt := &virtualIdle{}
	c.newIdleTimer = vt.new
	return s, c, usage, vt, set.Config().DataDir
}

// M1: a stream that fails after message_start (error event, dropped
// connection, idle timeout, missing message_stop, cancel) counts the usage
// it reported — in the job's stats, the request events and the usage
// recorder alike; one that never started counts nothing. L4: none of them is
// an empty answer in the call log.
func TestSEC4FailedStreamUsageCounted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    llmtest.StreamMode
		billed  bool
		outcome string
	}{
		{"overloaded", llmtest.StreamOverloaded, true, model.LLMOutcomeHTTPError},
		{"disconnect", llmtest.StreamDisconnect, true, model.LLMOutcomeNetwork},
		{"no stop", llmtest.StreamNoStop, true, model.LLMOutcomeBadResponse},
		{"idle", llmtest.StreamHang, true, model.LLMOutcomeIdle},
		{"idle before headers", llmtest.StreamHangHeaders, false, model.LLMOutcomeIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, usage, vt, dir := sec4Env(t)
			s.StreamNext(llmtest.RoleSummary, 5, tc.mode)
			s.SetStreamHook(func(role, event string) {
				if event == "hang" {
					if tc.mode == llmtest.StreamHang {
						waitUntil(t, "message_start to be read", func() bool { return vt.resetsOfLatest() >= 2 })
					}
					vt.advance(time.Duration(model.DefaultLLMIdleTimeoutS) * time.Second)
				}
			})
			st := &sched.StatsRecorder{}
			_, err := c.Complete(sched.WithStatsSink(context.Background(), st), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
			if err == nil {
				t.Fatal("no error")
			}
			var ends []sched.RequestEvent
			for _, ev := range st.Events {
				if ev.Phase == sched.RequestEnd {
					ends = append(ends, ev)
				}
			}
			if len(ends) != 5 {
				t.Fatalf("events %s", streamEventLines(st.Events))
			}
			var p, h, comp int64
			for _, ev := range ends {
				if ev.Outcome != tc.outcome || ev.EmptyText {
					t.Errorf("end %+v", ev)
				}
				if tc.billed && ev.PromptTokens <= 0 {
					t.Errorf("end without the usage: %+v", ev)
				}
				p, h, comp = p+ev.PromptTokens, h+ev.CacheHitTokens, comp+ev.CompletionTokens
			}
			snap := st.Snapshot()
			calls := usage.all()
			var up, uh, uc int64
			for _, u := range calls {
				up, uh, uc = up+u.prompt, uh+u.hit, uc+u.completion
			}
			wantRequests := 0
			if tc.billed {
				wantRequests = 5
			}
			if snap.Requests != int64(wantRequests) || len(calls) != wantRequests || snap.PromptTokens != p || up != p ||
				snap.CacheHitTokens != h || uh != h || snap.CompletionTokens != comp || uc != comp {
				t.Errorf("stats %+v, usage calls %+v, events %d/%d/%d", snap, calls, p, h, comp)
			}
			if !tc.billed && p != 0 {
				t.Errorf("usage made up: %d", p)
			}
			_, recs := readCalls(t, dir)
			for _, r := range recs {
				if r.Event == "end" && (r.EmptyText || r.TextChars != nil) {
					t.Errorf("a failed stream logged as an answer: %+v", r)
				}
			}
		})
	}
}

// M1: a cancel after message_start counts the usage too (recorded without
// the cancelled context), and nothing is retried.
func TestSEC4CanceledStreamUsageCounted(t *testing.T) {
	s, c, usage, vt, _ := sec4Env(t)
	s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHang)
	st := &sched.StatsRecorder{}
	ctx, cancel := context.WithCancel(sched.WithStatsSink(context.Background(), st))
	defer cancel()
	s.SetStreamHook(func(role, event string) {
		if event == "hang" {
			waitUntil(t, "message_start to be read", func() bool { return vt.resetsOfLatest() >= 2 })
			cancel()
		}
	})
	_, err := c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	calls := usage.all()
	if len(calls) != 1 || calls[0].prompt <= 0 || calls[0].ctxErr != nil || st.Snapshot().Requests != 1 || st.Snapshot().PromptTokens != calls[0].prompt {
		t.Fatalf("usage %+v stats %+v", calls, st.Snapshot())
	}
	if got := streamEventLines(st.Events); got != "start(1) end(1):canceled" {
		t.Errorf("events %s", got)
	}
}

// M4: the attempt timeout runs out while events keep coming: the answer is
// longer than one attempt may take. Permanent, sent once, its own code (not
// a network failure), the usage counted. (1.5 s, a still-streaming window
// of 375 ms and an event every 50 ms: room for a slow, loaded machine;
// SEC6 O3.)
func TestSEC4TimeLimitWhileStreaming(t *testing.T) {
	defer func(v time.Duration) { MaxAttemptTimeout = v }(MaxAttemptTimeout)
	MaxAttemptTimeout = 1500 * time.Millisecond
	s := newSEC4Server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sec4Start)
		rc := http.NewResponseController(w)
		for {
			if _, err := io.WriteString(w, sec4Text("more ")); err != nil || rc.Flush() != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
	c, rec, usage, dir := sec4Client(t, s.URL)
	_, err := c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if err == nil || !sched.IsPermanent(err) || !isCode(err, CodeTimeLimit, "retries", 0, "model", "claude-opus-5-5") {
		t.Fatalf("err %v %+v", err, infoOf(err))
	}
	if d := infoOf(err).Detail; !strings.Contains(d, "单次请求的时限") || strings.Contains(d, "总时限") || strings.Contains(d, "网络不可用") {
		t.Errorf("detail %q", d)
	}
	if s.count() != 1 || len(rec.waits) != 0 {
		t.Errorf("%d requests, waits %v", s.count(), rec.waits)
	}
	if calls := usage.all(); len(calls) != 1 || calls[0].prompt != 140 || calls[0].hit != 40 {
		t.Errorf("usage %+v", calls)
	}
	_, recs := readCalls(t, dir)
	if events(recs) != "start end failed" || recs[1].Result != "time_limit" {
		t.Errorf("calls %+v", recs)
	}
}

// M4: which timed-out attempts count as still streaming — the last event
// less than one idle timeout (its default when off) before the end; with an
// attempt timeout of 30 minutes the quarter bound of SEC5 N1 does not bite —
// and which stay a timeout, retried as before.
func TestSEC4TimeLimitOrTimeout(t *testing.T) {
	c := &HTTPClient{}
	for _, tc := range []struct {
		name      string
		stream    bool
		started   bool
		idle      time.Duration
		lastEvent time.Duration
		want      failKind
	}{
		{"events until the end", true, true, 3 * time.Minute, 29*time.Minute + 50*time.Second, failTimeLimit},
		{"stalled", true, true, 3 * time.Minute, 20 * time.Minute, failTimeout},
		{"idle off, events", true, true, 0, 29 * time.Minute, failTimeLimit},
		{"idle off, stalled", true, true, 0, 20 * time.Minute, failTimeout},
		{"no message_start", true, false, 3 * time.Minute, 29*time.Minute + 50*time.Second, failTimeout},
		{"not streamed", false, false, 0, 0, failTimeout},
	} {
		call := callInfo{stream: tc.stream, idle: tc.idle, timeout: 30 * time.Minute}
		out := attemptOutcome{status: 200, duration: 30 * time.Minute}
		if tc.stream {
			out.stream = &streamOutcome{started: tc.started, lastEvent: tc.lastEvent, firstToken: -1, firstText: -1,
				usage: Result{PromptTokens: 10}, hasUsage: tc.started}
		}
		_, aerr := c.answer(context.Background(), call, out, context.DeadlineExceeded)
		if aerr.kind != tc.want || aerr.retryable() != (tc.want == failTimeout) || aerr.usage != tc.started {
			t.Errorf("%s: %+v", tc.name, aerr)
		}
	}
}

// L1: error events map to the statuses of the same errors without
// streaming: billing, authentication and invalid requests are permanent
// (and a 400 in a stream says nothing about the schema), overloaded, rate
// limit, api and timeout errors are retried.
func TestSEC4ErrorEventTypes(t *testing.T) {
	for typ, want := range map[string]int{
		"invalid_request_error": 400, "authentication_error": 401, "billing_error": 402, "permission_error": 403,
		"not_found_error": 404, "request_too_large": 413, "rate_limit_error": 429, "api_error": 500,
		"timeout_error": 504, "overloaded_error": StatusOverloaded, "brand_new_error": 500, "": 500,
	} {
		if got := streamErrorStatus(typ); got != want {
			t.Errorf("%q: %d, want %d", typ, got, want)
		}
	}
	for _, tc := range []struct {
		typ      string
		code     string
		requests int
		perm     bool
	}{
		{"billing_error", CodeNoBalance, 1, true},
		{"authentication_error", CodeRejected, 1, true},
		{"invalid_request_error", CodeRejected, 1, true},
		{"timeout_error", "", 2, false},
		{"api_error", "", 2, false},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			ok := sec4OK(t, `{"patches": []}`)
			s := newSEC4Server(t, func(n int, w http.ResponseWriter, r *http.Request) {
				if n == 1 {
					writeSSE(w, sec4Start+sec4ErrorEvent(tc.typ, "boom"))
					return
				}
				writeSSE(w, ok)
			})
			c, _, _, _ := sec4Client(t, s.URL)
			schema := `{"type":"object","additionalProperties":false,"properties":{"patches":{"type":"array","items":{"type":"string"}}},"required":["patches"]}`
			_, err := c.Complete(WithJSONSchema(context.Background(), schema), model.RoleRepair, ctxbuild.RepairSystemTemplate, "t", true)
			if s.count() != tc.requests {
				t.Fatalf("%d requests (err %v)", s.count(), err)
			}
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || sched.IsPermanent(err) != tc.perm || !isCode(err, tc.code) {
				t.Fatalf("err %v %+v", err, infoOf(err))
			}
			if oc, _ := s.bodies[0]["output_config"].(map[string]any); oc["format"] == nil {
				t.Errorf("the request had no schema: %v", s.bodies[0])
			}
		})
	}
}

// L2: a line or the data of an event longer than maxStreamLine fails the
// attempt for good (sent once); a long event name is cut and matches no
// event; names are cleaned and cut before they reach a message.
func TestSEC4LineAndNameLimits(t *testing.T) {
	long := strings.Repeat("A", maxStreamLine+1)
	manyLines := strings.Repeat("data: "+strings.Repeat("B", 4096)+"\n", maxStreamLine/4096+1)
	for name, sse := range map[string]string{
		"long line": sec4Start + "event: content_block_delta\ndata: " + long + "\n\n",
		"long data": sec4Start + "event: content_block_delta\n" + manyLines + "\n",
	} {
		out, err := readAnthropicStream(strings.NewReader(sse), testKey, streamHooks{since: func() time.Duration { return 0 }})
		if err != nil || out.aerr == nil || out.aerr.kind != failEnvelope || out.aerr.retryable() || len(out.aerr.err.Error()) > 200 ||
			strings.Contains(out.aerr.err.Error(), "AAAA") || strings.Contains(out.aerr.err.Error(), "BBBB") {
			t.Errorf("%s: err %v aerr %+v", name, err, out.aerr)
		}
		s := newSEC4Server(t, func(n int, w http.ResponseWriter, r *http.Request) { writeSSE(w, sse) })
		c, rec, _, _ := sec4Client(t, s.URL)
		_, cerr := c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
		if !sched.IsPermanent(cerr) || !isCode(cerr, CodeBadResponse) || s.count() != 1 || len(rec.waits) != 0 {
			t.Errorf("%s: err %v, %d requests", name, cerr, s.count())
		}
	}

	// A 200 KB event name: cut when read, it is no known event and its data
	// is not looked at; the answer is read as usual.
	bigName := strings.Repeat("n", 200<<10)
	sse := sec4OK(t, `{"ok": true}`)
	sse = strings.Replace(sse, "event: ping\n", "event: "+bigName+"\ndata: {\"type\":\n\nevent: ping\n", 1)
	out, err := readAnthropicStream(strings.NewReader(sse), testKey, streamHooks{since: func() time.Duration { return 0 }})
	if err != nil || out.aerr != nil || out.res.Text != `{"ok": true}` {
		t.Errorf("big name: err %v aerr %+v", err, out.aerr)
	}
	for in, want := range map[string]string{
		"":                       "(unnamed)",
		"content_block_delta":    "content_block_delta",
		"a b\x00<script>":        "a?b??script?",
		strings.Repeat("x", 500): strings.Repeat("x", 40) + "…",
	} {
		if got := cleanEventName(in); got != want {
			t.Errorf("cleanEventName(%.20q) = %q, want %q", in, got, want)
		}
	}
}

// padReader yields a stream start, then comment lines without end.
type padReader struct {
	head []byte
	line []byte
	pos  int
}

func (p *padReader) Read(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		if len(p.head) > 0 {
			c := copy(b[n:], p.head)
			p.head, n = p.head[c:], n+c
			continue
		}
		c := copy(b[n:], p.line[p.pos:])
		n += c
		p.pos = (p.pos + c) % len(p.line)
	}
	return n, nil
}

// L3: a stream longer than maxResponseBytes fails for good, unlike one that
// merely ends before message_stop (retried).
func TestSEC4ResponseCap(t *testing.T) {
	pr := &padReader{head: []byte(sec4Start), line: []byte(": " + strings.Repeat("p", 1000) + "\n")}
	out, err := readAnthropicStream(pr, testKey, streamHooks{since: func() time.Duration { return 0 }})
	if err != nil || out.aerr == nil || out.aerr.kind != failEnvelope || out.aerr.retryable() || out.bytes != maxResponseBytes+1 ||
		!strings.Contains(out.aerr.err.Error(), "stream is longer") {
		t.Fatalf("err %v aerr %+v bytes %d", err, out.aerr, out.bytes)
	}
	if !out.aerr.usage || out.aerr.res.PromptTokens != 140 {
		t.Errorf("usage %+v", out.aerr)
	}
	out, err = readAnthropicStream(strings.NewReader(sec4Start), testKey, streamHooks{since: func() time.Duration { return 0 }})
	if err != nil || out.aerr == nil || out.aerr.kind != failEnvelope || !out.aerr.retryable() {
		t.Fatalf("no stop: err %v aerr %+v", err, out.aerr)
	}
}

// L9: the stream breaks after message_delta brought stop_reason max_tokens
// or refusal: the answer takes the truncation ladder / the refusal path, not
// a transport retry with the same parameters.
func TestSEC4StopReasonBeforeBreak(t *testing.T) {
	delta := func(reason string) string {
		return fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":4000}}\n\n", reason)
	}
	out, err := readAnthropicStream(strings.NewReader(sec4Start+sec4Text("partial")+delta("max_tokens")), testKey,
		streamHooks{since: func() time.Duration { return 0 }})
	if err != nil || out.aerr == nil || out.aerr.kind != failTruncated || out.aerr.res.Text != "partial" || out.aerr.res.CompletionTokens != 4000 || !out.meta.parsed {
		t.Fatalf("EOF: err %v aerr %+v", err, out.aerr)
	}
	// a read error after a refusal: the refusal, whatever the transport did
	broken := io.MultiReader(strings.NewReader(sec4Start+delta("refusal")), errReader{io.ErrUnexpectedEOF})
	out, err = readAnthropicStream(broken, testKey, streamHooks{since: func() time.Duration { return 0 }})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err %v", err)
	}
	_, aerr := (&HTTPClient{}).answer(context.Background(), callInfo{stream: true}, attemptOutcome{status: 200, stream: &out}, err)
	if aerr == nil || aerr.kind != failRefusal {
		t.Fatalf("aerr %+v", aerr)
	}

	// end to end: truncated without text, the stream ends without
	// message_stop; the next attempt is the ladder's (one level down), at once
	thinking := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n"
	ok := sec4OK(t, `{"patches": []}`)
	s := newSEC4Server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			writeSSE(w, sec4Start+thinking+delta("max_tokens"))
			return
		}
		writeSSE(w, ok)
	})
	c, set, p, _, rec, _ := anthropicClient(t, s.URL)
	setRole(t, set, model.RoleRepair, p.ID, "claude-opus-5-5", model.RoleParams{Thinking: sp(model.ThinkingMax)})
	if _, err := c.Complete(context.Background(), model.RoleRepair, ctxbuild.RepairSystemTemplate, "t", true); err != nil {
		t.Fatal(err)
	}
	effort := func(b map[string]any) any { oc, _ := b["output_config"].(map[string]any); return oc["effort"] }
	if s.count() != 2 || len(rec.waits) != 0 || effort(s.bodies[0]) != model.ThinkingMax || effort(s.bodies[1]) != model.ThinkingHigh {
		t.Fatalf("%d requests, waits %v, bodies %v", s.count(), rec.waits, s.bodies)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// L7: while events keep coming, the attempt reports activity (at most every
// streamActivityEvery; every event here) for the job's last activity; the
// request event lines are otherwise unchanged.
func TestSEC4StreamActivityReported(t *testing.T) {
	defer func(v time.Duration) { streamActivityEvery = v }(streamActivityEvery)
	streamActivityEvery = 0
	s, c, _, _, _ := sec4Env(t)
	s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamPings)
	st := &sched.StatsRecorder{}
	if _, err := c.Complete(sched.WithStatsSink(context.Background(), st), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	var rest []sched.RequestEvent
	activity := 0
	for _, ev := range st.Events {
		if ev.Phase == sched.RequestActivity {
			activity++
			if ev.CallID == "" || ev.N != 1 || ev.At.IsZero() {
				t.Errorf("activity %+v", ev)
			}
			continue
		}
		rest = append(rest, ev)
	}
	if activity < llmtest.StreamPingCount || streamEventLines(rest) != "start(1) phase(1):writing end(1):ok" {
		t.Errorf("%d activity events, others %s", activity, streamEventLines(rest))
	}
	// at the default interval a quick stream reports none
	streamActivityEvery = 15 * time.Second
	st = &sched.StatsRecorder{}
	if _, err := c.Complete(sched.WithStatsSink(context.Background(), st), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	for _, ev := range st.Events {
		if ev.Phase == sched.RequestActivity {
			t.Errorf("activity within 15 s: %+v", ev)
		}
	}
}

// L10: a set idle timeout below MinIdleTimeout is raised to it; 0 stays off.
// (The settings check refuses such a value since SEC5 N4; the test lowers
// its floor to reach the client's backstop.)
// M5: the idle failure says how to switch the timeout off behind a
// buffering proxy.
func TestSEC4IdleTimeoutFloorAndHint(t *testing.T) {
	defer func(v int) { settings.MinLLMIdleTimeoutS = v }(settings.MinLLMIdleTimeoutS)
	settings.MinLLMIdleTimeoutS = 0
	for set, want := range map[int]int{5: 30, 29: 30, 30: 30, 100: 100, 0: 0} {
		e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
		v := set
		g := e.set.General()
		g.LLMIdleTimeoutS = &v
		if err := e.set.SetGeneral(context.Background(), g); err != nil {
			t.Fatal(err)
		}
		if got := e.c.idleTimeout(); got != time.Duration(want)*time.Second {
			t.Errorf("setting %d: idle timeout %v", set, got)
		}
	}
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.c.Backoff = nil
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHangHeaders)
	e.s.SetStreamHook(func(role, event string) {
		if event == "hang" {
			e.vt.advance(time.Duration(model.DefaultLLMIdleTimeoutS) * time.Second)
		}
	})
	_, err := e.c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if !isCode(err, CodeIdleTimeout, "seconds", float64(model.DefaultLLMIdleTimeoutS)) || !strings.Contains(infoOf(err).Detail, "空闲超时设为 0") {
		t.Fatalf("err %v", err)
	}
}
