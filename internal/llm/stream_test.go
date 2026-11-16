// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// v3.2.x P1-c: streamed Anthropic requests with an idle timeout. The idle
// timeout runs on a virtual clock (virtualIdle): no test waits for it.

// virtualIdle is an idle timer on a virtual clock: it fires when advance
// moves the clock past the deadline its last Reset set.
type virtualIdle struct {
	mu       sync.Mutex
	now      time.Duration
	deadline time.Duration
	armed    bool
	f        func()
	created  int
	resets   int
	fired    int
	// resetsAtNew: resets when the latest timer was created
	resetsAtNew int
}

func (v *virtualIdle) new(d time.Duration, f func()) idleTimer {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.created++
	v.resetsAtNew = v.resets
	v.f, v.deadline, v.armed = f, v.now+d, true
	return v
}

// resetsOfLatest counts the resets of the latest timer (one per attempt).
func (v *virtualIdle) resetsOfLatest() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.resets - v.resetsAtNew
}

func (v *virtualIdle) Reset(d time.Duration) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.resets++
	was := v.armed
	v.deadline, v.armed = v.now+d, true
	return was
}

func (v *virtualIdle) Stop() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	was := v.armed
	v.armed = false
	return was
}

// advance moves the clock by d and fires the timer when it ran out.
func (v *virtualIdle) advance(d time.Duration) {
	v.mu.Lock()
	v.now += d
	fire := v.armed && v.now >= v.deadline
	f := v.f
	if fire {
		v.armed = false
		v.fired++
	}
	v.mu.Unlock()
	if fire {
		f()
	}
}

func (v *virtualIdle) counts() (created, resets, fired int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.created, v.resets, v.fired
}

// waitUntil polls cond for up to 10 s.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Errorf("timed out waiting for %s", what)
}

// streamEnv is a settings-mode client against the fake server, every role
// on modelName, with a virtual idle timer.
type streamEnv struct {
	s   *llmtest.Server
	c   *HTTPClient
	set *settings.Service
	p   model.Provider
	rec *recorder
	vt  *virtualIdle
}

func newStreamEnv(t *testing.T, modelName string, params model.RoleParams) *streamEnv {
	t.Helper()
	s := llmtest.New(t)
	c, set, p, _, rec, _ := anthropicClient(t, s.URL)
	for _, role := range model.Roles {
		setRole(t, set, role, p.ID, modelName, params)
	}
	vt := &virtualIdle{}
	c.newIdleTimer = vt.new
	return &streamEnv{s: s, c: c, set: set, p: p, rec: rec, vt: vt}
}

// streamEventLine is eventLine with the phase of a RequestPhase event and
// the status of an http_error.
func streamEventLines(evs []sched.RequestEvent) string {
	var out []string
	for _, ev := range evs {
		s := eventLine(ev)
		switch {
		case ev.Phase == sched.RequestPhase:
			s += ":" + ev.StreamPhase
		case ev.Phase == sched.RequestEnd && ev.Outcome == model.LLMOutcomeHTTPError:
			s += fmt.Sprintf("/%d", ev.Status)
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func summaryFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(llmtest.FixtureDir(), "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Summary
}

// Which requests are streamed: current Claude models of an Anthropic
// provider only.
func TestStreamedTargets(t *testing.T) {
	for _, tc := range []struct {
		ptype, model string
		want         bool
	}{
		{model.ProviderAnthropic, "claude-opus-5-5", true},
		{model.ProviderAnthropic, "claude-sonnet-5-5-20261001", true},
		{model.ProviderAnthropic, "claude-test-model", true},
		{model.ProviderAnthropic, "claude-haiku-4-5", false},
		{model.ProviderAnthropic, "claude-sonnet-4-5-20250929", false},
		{model.ProviderAnthropic, "deepseek-flash", false},
		{model.ProviderOpenAI, "claude-opus-5-5", false},
		{model.ProviderOpenAI, "deepseek-flash", false},
	} {
		if got := streamed(target{ptype: tc.ptype, model: tc.model}); got != tc.want {
			t.Errorf("%s %s: streamed %v", tc.ptype, tc.model, got)
		}
	}
}

// The stream of a message gives what the whole message gives: text, usage,
// stop reason, refusal category, the call log's meta data.
func TestStreamMatchesWholeMessage(t *testing.T) {
	refusal, _ := json.Marshal(map[string]any{"id": "msg_r", "type": "message", "role": "assistant", "stop_reason": "refusal",
		"content": []any{}, "stop_details": map[string]any{"type": "refusal", "category": "cyber", "explanation": "x"},
		"usage": map[string]any{"input_tokens": 50, "output_tokens": 3}})
	for name, msg := range map[string]string{
		"ok":                 anthropicOK(`{"patches": [{"idx": 1, "text": "中文 and more"}]}`),
		"no cache fields":    `{"id":"m","type":"message","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`,
		"truncated, text":    anthropicTruncated(`{"patches": [`),
		"truncated, no text": anthropicTruncated(""),
		"refusal":            string(refusal),
	} {
		t.Run(name, func(t *testing.T) {
			wantRes, wantErr := parseAnthropic([]byte(msg), testKey)
			wantMeta := answerMeta(model.ProviderAnthropic, []byte(msg))
			sse, err := llmtest.MessageSSE([]byte(msg))
			if err != nil {
				t.Fatal(err)
			}
			var phases []string
			out, rerr := readAnthropicStream(bytes.NewReader(sse), testKey, streamHooks{
				since: func() time.Duration { return time.Millisecond }, phase: func(p string) { phases = append(phases, p) }})
			if rerr != nil {
				t.Fatal(rerr)
			}
			if out.res != wantRes {
				t.Errorf("result %+v, want %+v", out.res, wantRes)
			}
			switch {
			case (out.aerr == nil) != (wantErr == nil):
				t.Fatalf("err %+v, want %+v", out.aerr, wantErr)
			case wantErr != nil && (out.aerr.kind != wantErr.kind || out.aerr.res != wantErr.res || out.aerr.category != wantErr.category):
				t.Errorf("err %+v, want %+v", out.aerr, wantErr)
			}
			got := out.meta
			if got.id != wantMeta.id || got.stopReason != wantMeta.stopReason || got.stopCategory != wantMeta.stopCategory ||
				got.textChars != wantMeta.textChars || got.thinkingChars != wantMeta.thinkingChars || !reflect.DeepEqual(got.blocks, wantMeta.blocks) || !got.parsed {
				t.Errorf("meta %+v, want %+v", got, wantMeta)
			}
			var gu, wu map[string]any
			json.Unmarshal(got.usage, &gu)
			json.Unmarshal(wantMeta.usage, &wu)
			if !reflect.DeepEqual(gu, wu) {
				t.Errorf("usage %s, want %s", got.usage, wantMeta.usage)
			}
			if out.bytes != len(sse) || out.lastEvent < 0 {
				t.Errorf("bytes %d of %d, last event %v", out.bytes, len(sse), out.lastEvent)
			}
			if wantRes.Text != "" || wantErr != nil && wantErr.res.Text != "" {
				if len(phases) != 1 || phases[0] != model.LLMPhaseWriting || out.firstText < 0 || out.firstToken < 0 {
					t.Errorf("phases %v first text %v", phases, out.firstText)
				}
			}
		})
	}
}

// message_delta may carry updated input-side usage; it wins over
// message_start's. Comments, unknown events and pings are ignored.
func TestStreamUsageMergedAndUnknownEvents(t *testing.T) {
	sse := ": keep-alive comment\n\n" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_u\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: future_event\ndata: {\"type\":\"future_event\",\"x\":1}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\r\n\r\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":12,\"cache_creation_input_tokens\":3,\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	touched := 0
	out, err := readAnthropicStream(strings.NewReader(sse), testKey, streamHooks{since: func() time.Duration { return 0 }, touch: func() { touched++ }})
	if err != nil || out.aerr != nil {
		t.Fatalf("err %v %+v", err, out.aerr)
	}
	if out.res != (Result{Text: "Hello", PromptTokens: 12 + 5 + 3, CacheHitTokens: 5, CompletionTokens: 9}) {
		t.Errorf("result %+v", out.res)
	}
	if touched != 11 { // the comment and every event
		t.Errorf("touched %d", touched)
	}
}

// Broken streams: unreadable JSON, no message_start, no message_stop are
// bad responses; an error event is the status it stands for.
func TestStreamBroken(t *testing.T) {
	start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n"
	for name, tc := range map[string]struct {
		sse    string
		kind   failKind
		status int
	}{
		"bad json":    {start + "event: content_block_delta\ndata: {\"type\":\n\n", failEnvelope, 0},
		"no start":    {"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", failEnvelope, 0},
		"no stop":     {start + "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n", failEnvelope, 0},
		"overloaded":  {start + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n", failStatus, StatusOverloaded},
		"rate limit":  {start + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"x\"}}\n\n", failStatus, 429},
		"api error":   {start + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"x " + testKey + "\"}}\n\n", failStatus, 500},
		"unterminate": {start + "event: message_stop\ndata: {\"type\":\"message_stop\"}", failEnvelope, 0},
	} {
		out, err := readAnthropicStream(strings.NewReader(tc.sse), testKey, streamHooks{since: func() time.Duration { return 0 }})
		if err != nil || out.aerr == nil || out.aerr.kind != tc.kind || out.aerr.status != tc.status || !out.aerr.retryable() {
			t.Errorf("%s: err %v aerr %+v", name, err, out.aerr)
			continue
		}
		if strings.Contains(out.aerr.body, testKey) {
			t.Errorf("%s: the key is in %q", name, out.aerr.body)
		}
	}
}

// A streamed request through the fake server: the answer, the usage, one
// phase event of each kind (the thinking summary and the text arrive in
// several deltas), the call log's stream fields, nothing of the answer or
// the thinking in the call log.
func TestStreamedRequest(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{Thinking: sp(model.ThinkingHigh)})
	rec := &sched.StatsRecorder{}
	ctx := sched.WithStatsSink(context.Background(), rec)
	res, err := e.c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "the transcript", false)
	if err != nil {
		t.Fatal(err)
	}
	want := summaryFixture(t)
	if res.Text != want || res.PromptTokens <= 0 || res.CompletionTokens <= 0 {
		t.Fatalf("result %+v", res)
	}
	reqs := e.s.Requests(llmtest.RoleSummary)
	if len(reqs) != 1 || !reqs[0].Stream || reqs[0].Thinking != "adaptive" || reqs[0].Display != "summarized" || reqs[0].Effort != model.ThinkingHigh {
		t.Fatalf("requests %+v", reqs)
	}
	if got := streamEventLines(rec.Events); got != "start(1) phase(1):thinking phase(1):writing end(1):ok" {
		t.Fatalf("events %s", got)
	}
	if created, resets, fired := e.vt.counts(); created != 1 || resets < 5 || fired != 0 {
		t.Errorf("idle timer created %d resets %d fired %d", created, resets, fired)
	}
	raw, recs := readCalls(t, e.set.Config().DataDir)
	if events(recs) != "start end" {
		t.Fatalf("calls %s", raw)
	}
	s, end := recs[0], recs[1]
	if !s.Stream || s.IdleTimeoutS == nil || *s.IdleTimeoutS != model.DefaultLLMIdleTimeoutS {
		t.Errorf("start %+v", s)
	}
	if !end.Stream || end.Result != "ok" || end.FirstTokenMS == nil || end.FirstTextMS == nil || end.LastEventMS == nil ||
		*end.FirstTokenMS > *end.FirstTextMS || *end.FirstTextMS > *end.LastEventMS || end.RequestID != "req_llmtest_stream" ||
		end.StopReason != "end_turn" || end.Blocks["thinking"] != 1 || end.Blocks["text"] != 1 || end.ThinkingChars == 0 ||
		end.TextChars == nil || *end.TextChars == 0 || end.ResponseBytes == 0 || !strings.Contains(string(end.Usage), "output_tokens") {
		t.Errorf("end %+v", end)
	}
	for _, secret := range []string{want[:20], llmtest.ThinkingSummary[:20], "the transcript"} {
		if strings.Contains(raw, secret) {
			t.Errorf("the call log holds %q", secret)
		}
	}
}

// With thinking off (claude-opus-5-5: effort low, no thinking field, so the
// thinking text is omitted and only pings arrive while the model thinks) the
// request is streamed without a summary: no thinking phase.
func TestStreamedRequestThinkingOff(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	rec := &sched.StatsRecorder{}
	if _, err := e.c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	if r := e.s.Requests(llmtest.RoleSummary)[0]; !r.Stream || r.Thinking != "" || r.Display != "" || r.Effort != EffortOffAlwaysThinking {
		t.Fatalf("request %+v", r)
	}
	if got := streamEventLines(rec.Events); got != "start(1) phase(1):writing end(1):ok" {
		t.Fatalf("events %s", got)
	}
}

// claude-sonnet-5-5 off: between_tools without display; older Claude models
// and non-Claude names are not streamed and get no display.
func TestStreamedThinkingFields(t *testing.T) {
	for _, tc := range []struct {
		model, level      string
		stream            bool
		thinking, display string
	}{
		{"claude-sonnet-5-5", model.ThinkingOff, true, "between_tools", ""},
		{"claude-sonnet-5-5", model.ThinkingLow, true, "adaptive", "summarized"},
		{"claude-haiku-4-5", model.ThinkingHigh, false, "enabled", ""},
		{"deepseek-flash", model.ThinkingHigh, false, "enabled", ""},
	} {
		e := newStreamEnv(t, tc.model, model.RoleParams{Thinking: sp(tc.level)})
		if _, err := e.c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
			t.Fatalf("%s %s: %v", tc.model, tc.level, err)
		}
		r := e.s.Requests(llmtest.RoleSummary)[0]
		if r.Stream != tc.stream || r.Thinking != tc.thinking || r.Display != tc.display {
			t.Errorf("%s %s: request %+v", tc.model, tc.level, r)
		}
		if created, _, _ := e.vt.counts(); (created == 1) != tc.stream {
			t.Errorf("%s %s: idle timers %d", tc.model, tc.level, created)
		}
	}
}

// No event for the idle timeout after message_start: the attempt ends idle,
// is retried with the backoff, and the retry succeeds.
func TestStreamIdleTimeoutRetried(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{Thinking: sp(model.ThinkingHigh)})
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHang)
	e.s.SetStreamHook(func(role, event string) {
		if event == "hang" {
			// the client read the headers and message_start: the timer was
			// restarted twice; one idle timeout later it runs out
			waitUntil(t, "message_start to be read", func() bool { _, r, _ := e.vt.counts(); return r >= 2 })
			e.vt.advance(time.Duration(model.DefaultLLMIdleTimeoutS)*time.Second - time.Nanosecond)
			if _, _, fired := e.vt.counts(); fired != 0 {
				t.Errorf("fired before the idle timeout")
			}
			e.vt.advance(time.Nanosecond)
		}
	})
	rec := &sched.StatsRecorder{}
	res, err := e.c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if err != nil || res.Text != summaryFixture(t) {
		t.Fatalf("res %+v err %v", res, err)
	}
	if got := streamEventLines(rec.Events); got != "start(1) end(1):idle backoff(1):2s start(2) phase(2):thinking phase(2):writing end(2):ok" {
		t.Fatalf("events %s", got)
	}
	_, recs := readCalls(t, e.set.Config().DataDir)
	if events(recs) != "start end start end" || recs[1].Result != "idle" || recs[1].Status != http200 || recs[1].LastEventMS == nil ||
		recs[2].Reason != "idle timeout, waited 2s" {
		t.Errorf("calls %+v", recs)
	}
}

const http200 = 200

// The idle timeout also covers the wait for the response headers; when it
// keeps running out, the error is llm.idle_timeout (SEC4 M5: its own code,
// with the hint for buffering proxies) with the idle timeout as its seconds,
// not permanent.
func TestStreamIdleBeforeHeaders(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.StreamNext(llmtest.RoleSummary, 5, llmtest.StreamHangHeaders)
	e.s.SetStreamHook(func(role, event string) {
		if event == "hang" {
			e.vt.advance(time.Duration(model.DefaultLLMIdleTimeoutS) * time.Second)
		}
	})
	rec := &sched.StatsRecorder{}
	_, err := e.c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if err == nil || sched.IsPermanent(err) || !isCode(err, CodeIdleTimeout, "seconds", float64(model.DefaultLLMIdleTimeoutS), "retries", 4) {
		t.Fatalf("err %v %+v", err, infoOf(err))
	}
	if !strings.Contains(infoOf(err).Detail, "没有任何数据") {
		t.Errorf("detail %q", infoOf(err).Detail)
	}
	if n := strings.Count(streamEventLines(rec.Events), "end"); n != 5 || !strings.Contains(streamEventLines(rec.Events), "end(5):idle") {
		t.Errorf("events %s", streamEventLines(rec.Events))
	}
	_, recs := readCalls(t, e.set.Config().DataDir)
	if recs[1].Result != "idle" || recs[1].HeadersMS != nil {
		t.Errorf("end %+v", recs[1])
	}
}

// Pings keep a stream alive: each restarts the idle timeout, so a stream
// whose pings come 0.6 idle timeouts apart outlasts several idle timeouts.
func TestStreamPingsKeepItAlive(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamPings)
	idle := time.Duration(model.DefaultLLMIdleTimeoutS) * time.Second
	sent, pings := 0, 0
	e.s.SetStreamHook(func(role, event string) {
		sent++
		if event != "ping" {
			return
		}
		pings++
		// the client has handled every event so far (and the headers)
		waitUntil(t, "the client to read the ping", func() bool { _, r, _ := e.vt.counts(); return r >= sent+1 })
		e.vt.advance(idle * 6 / 10)
	})
	res, err := e.c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if err != nil || res.Text != summaryFixture(t) {
		t.Fatalf("res %+v err %v", res, err)
	}
	if _, _, fired := e.vt.counts(); fired != 0 || pings != llmtest.StreamPingCount+1 {
		t.Errorf("fired %d pings %d", fired, pings)
	}
	if e.s.Count(llmtest.RoleSummary) != 1 {
		t.Errorf("%d requests", e.s.Count(llmtest.RoleSummary))
	}
}

// llm_idle_timeout_s = 0: no idle timeout at all; the call log says 0.
func TestStreamIdleTimeoutOff(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	zero := 0
	g := e.set.General()
	g.LLMIdleTimeoutS = &zero
	if err := e.set.SetGeneral(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamPings)
	if _, err := e.c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	if created, _, _ := e.vt.counts(); created != 0 {
		t.Errorf("idle timers %d", created)
	}
	_, recs := readCalls(t, e.set.Config().DataDir)
	if s := recs[0]; !s.Stream || s.IdleTimeoutS == nil || *s.IdleTimeoutS != 0 {
		t.Errorf("start %+v", s)
	}
}

// The real timer: a stream that hangs is given up after the idle timeout
// (1 s here, below MinIdleTimeout and the settings floor, which the test
// lowers) and retried.
func TestStreamIdleTimeoutRealTimer(t *testing.T) {
	defer func(v time.Duration) { MinIdleTimeout = v }(MinIdleTimeout)
	MinIdleTimeout = 0
	defer func(v int) { settings.MinLLMIdleTimeoutS = v }(settings.MinLLMIdleTimeoutS)
	settings.MinLLMIdleTimeoutS = 0
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.c.newIdleTimer = afterFunc
	one := 1
	g := e.set.General()
	g.LLMIdleTimeoutS = &one
	if err := e.set.SetGeneral(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHang)
	rec := &sched.StatsRecorder{}
	begin := time.Now()
	if _, err := e.c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(streamEventLines(rec.Events), "start(1) end(1):idle backoff(1):2s start(2)") || time.Since(begin) > 20*time.Second {
		t.Errorf("events %s after %v", streamEventLines(rec.Events), time.Since(begin))
	}
}

// A cancel while the stream is being read returns at once; the attempt
// ends canceled (start → end canceled), nothing is retried.
func TestStreamCanceled(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{Thinking: sp(model.ThinkingMax)})
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHang)
	rec := &sched.StatsRecorder{}
	ctx, cancel := context.WithCancel(sched.WithStatsSink(context.Background(), rec))
	defer cancel()
	e.s.SetStreamHook(func(role, event string) {
		if event == "hang" {
			cancel()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := e.c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Complete did not return after the cancel")
	}
	if got := streamEventLines(rec.Events); got != "start(1) end(1):canceled" {
		t.Fatalf("events %s", got)
	}
	if len(e.rec.waits) != 0 {
		t.Errorf("waits %v", e.rec.waits)
	}
}

// An error event in the stream (HTTP 200): overloaded_error is a 529 —
// retried with the backoff, the status in the request event, the error type
// in the call log; exhausted, it is llm.overloaded, not permanent.
func TestStreamErrorEventRetried(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamOverloaded)
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamRateLimited)
	rec := &sched.StatsRecorder{}
	if _, err := e.c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	if got := streamEventLines(rec.Events); got != "start(1) end(1):http_error/529 backoff(1):2s start(2) end(2):http_error/429 backoff(2):4s start(3) phase(3):writing end(3):ok" {
		t.Fatalf("events %s", got)
	}
	_, recs := readCalls(t, e.set.Config().DataDir)
	if r := recs[1]; r.Result != "http_error" || r.Status != http200 || r.StreamError != "overloaded_error" || !strings.Contains(r.Error, "Overloaded") {
		t.Errorf("end %+v", r)
	}
	e.s.StreamNext(llmtest.RoleSummary, 5, llmtest.StreamOverloaded)
	_, err := e.c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if err == nil || sched.IsPermanent(err) || !isCode(err, CodeOverloaded, "retries", 4) {
		t.Fatalf("err %v", err)
	}
}

// A connection dropped mid-stream is a network failure, a stream that ends
// without message_stop a bad response; both are retried.
func TestStreamCutOffRetried(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamDisconnect)
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamNoStop)
	rec := &sched.StatsRecorder{}
	res, err := e.c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if err != nil || res.Text != summaryFixture(t) {
		t.Fatalf("res %+v err %v", res, err)
	}
	if got := streamEventLines(rec.Events); got != "start(1) phase(1):writing end(1):network backoff(1):2s start(2) phase(2):writing end(2):bad_response backoff(2):4s start(3) phase(3):writing end(3):ok" {
		t.Fatalf("events %s", got)
	}
}

// The truncation ladder on a stream: max_tokens without text goes one level
// down with the same limit; the billed usage of the cut answer counts.
func TestStreamTruncationLadder(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{Thinking: sp(model.ThinkingMax)})
	e.s.StreamNext(llmtest.RoleRepair, 1, llmtest.StreamMaxTokensNoText)
	rec := &sched.StatsRecorder{}
	if _, err := e.c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleRepair, ctxbuild.RepairSystemTemplate, "t", true); err != nil {
		t.Fatal(err)
	}
	reqs := e.s.Requests(llmtest.RoleRepair)
	if len(reqs) != 2 || reqs[0].Effort != model.ThinkingMax || reqs[1].Effort != model.ThinkingHigh ||
		reqs[0].MaxTokens != BaseMaxTokensRepair+ThinkingHeadroomMax || reqs[1].MaxTokens != reqs[0].MaxTokens || !reqs[1].Stream {
		t.Fatalf("requests %+v", reqs)
	}
	if got := streamEventLines(rec.Events); got != "start(1) phase(1):thinking end(1):truncated start(2) phase(2):thinking phase(2):writing end(2):ok" {
		t.Fatalf("events %s", got)
	}
	if e := rec.Events[2]; !e.EmptyText || e.CompletionTokens != int64(reqs[0].MaxTokens) {
		t.Errorf("end 1 %+v", e)
	}
	if st := rec.Snapshot(); st.Requests != 2 {
		t.Errorf("stats %+v", st)
	}
}

// A refusal in a stream: permanent, coded with the category, billed.
func TestStreamRefusal(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.FailNext(llmtest.RoleSummary, 1, llmtest.FailRefusal)
	rec := &sched.StatsRecorder{}
	_, err := e.c.Complete(sched.WithStatsSink(context.Background(), rec), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if !errors.Is(err, ErrRefusal) || !sched.IsPermanent(err) || !isCode(err, CodeRefusal, "category", llmtest.RefusalCategory) {
		t.Fatalf("err %v", err)
	}
	if st := rec.Snapshot(); st.Requests != 1 || e.s.Count(llmtest.RoleSummary) != 1 {
		t.Errorf("stats %+v", st)
	}
}

// Structured output on a stream: a 400 for output_config.format sends the
// request again without the schema, streamed too.
func TestStreamStructuredOutputFallback(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.RejectFormat(true)
	schema := `{"type":"object","additionalProperties":false,"properties":{"patches":{"type":"array","items":{"type":"string"}}},"required":["patches"]}`
	res, err := e.c.Complete(WithJSONSchema(context.Background(), schema), model.RoleRepair, ctxbuild.RepairSystemTemplate, "t", true)
	if err != nil || res.Text == "" {
		t.Fatalf("res %+v err %v", res, err)
	}
	hits := e.s.Hits()
	if len(hits) != 2 || !hits[0].Format || hits[0].Status != 400 || hits[1].Format || hits[1].Status != 200 {
		t.Fatalf("hits %+v", hits)
	}
	if r := e.s.Requests(llmtest.RoleRepair); len(r) != 1 || !r[0].Stream {
		t.Fatalf("requests %+v", r)
	}
}

// log_level = debug: the raw events of a stream are in the debug log, the
// key is not.
func TestStreamDebugLog(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.set.Config().LogLevel = "debug"
	ctx := sched.WithJob(context.Background(), model.Job{ID: "01STREAMJOB"})
	if _, err := e.c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(e.set.Config().DataDir, "logs", "llm", "01STREAMJOB.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), testKey) {
		t.Fatal("the debug log holds the key")
	}
	var de debugEntry
	if err := json.Unmarshal(b, &de); err != nil {
		t.Fatal(err)
	}
	var stream string
	if err := json.Unmarshal(de.Response, &stream); err != nil || !strings.Contains(stream, "event: message_start") ||
		!strings.Contains(stream, "event: message_stop") || de.Headers["Accept"] != "text/event-stream" {
		t.Fatalf("entry %+v (%v)", de, err)
	}
}
