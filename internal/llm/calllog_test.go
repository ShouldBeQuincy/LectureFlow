// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Texts no call log file may hold: the prompt, the answer, the key.
const (
	secretSystem = "SYSTEM-PROMPT-TEXT"
	secretUser   = "USER-PROMPT-TEXT"
)

func callLogDir(dataDir string) string { return filepath.Join(dataDir, "logs", "llm") }

// readLog returns the raw text and the decoded lines of the day's file
// with prefix ("" and nil when there is none).
func readLog[T any](t *testing.T, dataDir, prefix string) (string, []T) {
	t.Helper()
	names, _ := filepath.Glob(filepath.Join(callLogDir(dataDir), prefix+"*.jsonl"))
	if len(names) == 0 {
		return "", nil
	}
	if len(names) > 1 {
		t.Fatalf("%d %s files: %v", len(names), prefix, names)
	}
	b, err := os.ReadFile(names[0])
	if err != nil {
		t.Fatal(err)
	}
	var out []T
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, v)
	}
	return string(b), out
}

func readCalls(t *testing.T, dataDir string) (string, []callRecord) {
	t.Helper()
	return readLog[callRecord](t, dataDir, callsPrefix)
}

func events(recs []callRecord) string {
	var ev []string
	for _, r := range recs {
		ev = append(ev, r.Event)
	}
	return strings.Join(ev, " ")
}

func noSecrets(t *testing.T, raw string, more ...string) {
	t.Helper()
	for _, s := range append([]string{secretSystem, secretUser, testKey}, more...) {
		if strings.Contains(raw, s) {
			t.Errorf("the call log holds %q:\n%s", s, raw)
		}
	}
}

// One request: a start and an end line with what the request was and what
// the provider said about it, and nothing of the prompt, the answer or the key.
func TestCallLogSuccess(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		w.Header().Set("X-Request-Id", "req-abc")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "59")
		w.Header().Set("X-Other", "not logged")
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"content":"ANSWER-TEXT","reasoning_content":"abc"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":64,"completion_tokens_details":{"reasoning_tokens":7}}}`)
	})
	c, _, _ := newClient(t, srv.URL)
	ctx := sched.WithJob(context.Background(), model.Job{ID: "01JOB", Stage: "translate", Scope: "lecture", ScopeID: "01LEC", Attempt: 2})
	if _, err := c.Complete(ctx, config.RoleTranslate, secretSystem, secretUser, true); err != nil {
		t.Fatal(err)
	}
	raw, recs := readCalls(t, c.cfg.DataDir)
	noSecrets(t, raw, "ANSWER-TEXT", "not logged")
	if events(recs) != "start end" {
		t.Fatalf("events %q:\n%s", events(recs), raw)
	}
	s, e := recs[0], recs[1]
	if s.Call == "" || s.Call != e.Call {
		t.Errorf("call ids %q %q", s.Call, e.Call)
	}
	if s.JobID != "01JOB" || s.Stage != "translate" || s.Scope != "lecture" || s.ScopeID != "01LEC" || s.JobAttempt != 2 {
		t.Errorf("job fields %+v", s)
	}
	if s.Role != config.RoleTranslate || s.Provider != "deepseek" || s.API != model.ProviderOpenAI || s.Model != "deepseek-flash" ||
		s.Effort != model.ThinkingOff || s.Attempt != 0 || s.Try != 0 || !s.JSONMode || s.MaxTokens != 0 || s.TimeoutS != 120 || s.RequestBytes == 0 {
		t.Errorf("start %+v", s)
	}
	if _, err := time.Parse(callLogTime, s.Time); err != nil {
		t.Errorf("time %q: %v", s.Time, err)
	}
	if e.Result != "ok" || e.Status != 200 || e.RequestID != "req-abc" || e.ResponseID != "chatcmpl-1" || e.StopReason != "stop" ||
		e.HeadersMS == nil || e.DurationMS == nil || e.ResponseBytes == 0 || e.EmptyText || e.Error != "" {
		t.Errorf("end %+v", e)
	}
	if len(e.RateLimit) != 1 || e.RateLimit["x-ratelimit-remaining-requests"] != "59" {
		t.Errorf("rate limit headers %v", e.RateLimit)
	}
	var u map[string]any
	if json.Unmarshal(e.Usage, &u) != nil || u["prompt_tokens"] != 100.0 || u["prompt_cache_hit_tokens"] != 64.0 {
		t.Errorf("usage %s", e.Usage)
	}
	if e.ReasoningTokens == nil || *e.ReasoningTokens != 7 || e.TextChars == nil || *e.TextChars != len("ANSWER-TEXT") || e.ThinkingChars != 3 {
		t.Errorf("thinking and text: %+v", e)
	}
}

// The case of the summary role: thinking uses up max_tokens, the answer has
// no text; the truncation ladder (v3.2.x P1-c) lowers the level twice with
// the same limit, and those attempts are cut off too. Every attempt is in
// the log with its level and limit, and the log says the text was empty.
func TestCallLogTruncatedByThinking(t *testing.T) {
	log := captureLog(t)
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		w.Header().Set("Request-Id", "req_0"+string(rune('0'+n)))
		w.Header().Set("Anthropic-Ratelimit-Output-Tokens-Remaining", "1000")
		io.WriteString(w, `{"id":"msg_1","type":"message","stop_reason":"max_tokens","content":[{"type":"thinking","thinking":""}],`+
			`"usage":{"input_tokens":900,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":28576}}`)
	})
	c, set, p, _, _, _ := anthropicClient(t, srv.URL)
	setRole(t, set, model.RoleSummary, p.ID, "claude-opus-5-5", model.RoleParams{Thinking: sp(model.ThinkingMax)})
	_, err := c.Complete(context.Background(), model.RoleSummary, secretSystem, secretUser, false)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err %v", err)
	}
	raw, recs := readCalls(t, set.Config().DataDir)
	noSecrets(t, raw)
	if events(recs) != "start end start end start end failed" {
		t.Fatalf("events %q:\n%s", events(recs), raw)
	}
	// v3.2.x D1: summary's base + the headroom of max (the full 60000: the
	// request is streamed since v3.2.x P1-c)
	first := BaseMaxTokensSummary + ThinkingHeadroomMax
	if s := recs[0]; s.Effort != model.ThinkingMax || s.MaxTokens != first || s.ThinkingBudget != ThinkingHeadroomMax || s.Attempt != 0 || s.Reason != "" ||
		s.Provider != "Claude" || s.API != model.ProviderAnthropic || s.Model != "claude-opus-5-5" {
		t.Errorf("first start %+v", s)
	}
	if s := recs[2]; s.MaxTokens != first || s.Attempt != 1 || s.Effort != model.ThinkingHigh || s.ThinkingBudget != ThinkingHeadroomHigh ||
		s.Reason != "truncated without text: thinking max -> high, max_tokens 64000" {
		t.Errorf("second start %+v", s)
	}
	if s := recs[4]; s.MaxTokens != first || s.Attempt != 2 || s.Effort != model.ThinkingLow || s.EffortSent != model.ThinkingLow ||
		s.Reason != "truncated without text: thinking high -> low, max_tokens 64000" {
		t.Errorf("third start %+v", s)
	}
	if !strings.Contains(raw, "max -> high") { // readable as written: no >
		t.Errorf("the reason is escaped:\n%s", raw)
	}
	for _, e := range []callRecord{recs[1], recs[3], recs[5]} {
		if e.Result != "truncated" || e.StopReason != "max_tokens" || !e.EmptyText || e.TextChars == nil || *e.TextChars != 0 ||
			e.Blocks["thinking"] != 1 || e.Blocks["text"] != 0 || !strings.Contains(string(e.Usage), `"output_tokens":28576`) ||
			e.RateLimit["anthropic-ratelimit-output-tokens-remaining"] != "1000" {
			t.Errorf("end %+v", e)
		}
	}
	if recs[1].RequestID != "req_01" || recs[3].RequestID != "req_02" {
		t.Errorf("request ids %q %q", recs[1].RequestID, recs[3].RequestID)
	}
	if f := recs[6]; f.Attempt != 2 || !strings.Contains(f.Error, "截断") || f.Call != recs[0].Call {
		t.Errorf("failed %+v", f)
	}
	// The service log names the failed attempts and the failed call.
	if s := log.String(); strings.Count(s, `msg="llm attempt failed"`) != 3 || strings.Count(s, `msg="llm call failed"`) != 1 ||
		!strings.Contains(s, "empty_text=true") || !strings.Contains(s, "max_tokens=64000") || strings.Contains(s, testKey) {
		t.Errorf("service log:\n%s", s)
	}
}

// A throttled attempt and its retry: the status, the rate limit headers
// and the wait are in the log; a key echoed by the server is not.
func TestCallLogRetry(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("Retry-After", "3")
			w.Header().Set("Request-Id", "req_429")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down `+testKey+`"}}`)
			return
		}
		io.WriteString(w, anthropicOK("fine"))
	})
	c, set, _, _, rec, _ := anthropicClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), model.RoleRepair, secretSystem, secretUser, true); err != nil {
		t.Fatal(err)
	}
	raw, recs := readCalls(t, set.Config().DataDir)
	noSecrets(t, raw)
	if events(recs) != "start end start end" {
		t.Fatalf("events %q:\n%s", events(recs), raw)
	}
	if e := recs[1]; e.Result != "http_error" || e.Status != 429 || e.RequestID != "req_429" || e.RateLimit["retry-after"] != "3" ||
		!strings.Contains(e.Error, "rate_limit_error") || e.StopReason != "" {
		t.Errorf("first end %+v", e)
	}
	if s := recs[2]; s.Attempt != 1 || s.Reason != "http 429, waited 3s (retry-after)" {
		t.Errorf("second start %+v", s)
	}
	if e := recs[3]; e.Result != "ok" || e.StopReason != "end_turn" || e.ResponseID != "msg_1" || e.Blocks["text"] != 2 || e.Blocks["thinking"] != 1 ||
		e.TextChars == nil || *e.TextChars != 4 {
		t.Errorf("second end %+v", e)
	}
	if len(rec.waits) != 1 || rec.waits[0] != 3*time.Second {
		t.Errorf("waits %v", rec.waits)
	}
}

// A request that gets no response: the end line says so, with no status.
func TestCallLogNetworkError(t *testing.T) {
	c, _, _ := newClient(t, "http://127.0.0.1:1")
	c.Backoff = nil
	if _, err := c.Complete(context.Background(), config.RoleSummary, secretSystem, secretUser, false); err == nil {
		t.Fatal("no error")
	}
	raw, recs := readCalls(t, c.cfg.DataDir)
	if events(recs) != "start end failed" {
		t.Fatalf("events %q:\n%s", events(recs), raw)
	}
	if e := recs[1]; e.Result != "network" || e.Status != 0 || e.HeadersMS != nil || e.DurationMS == nil || e.Error == "" {
		t.Errorf("end %+v", e)
	}
}

// An answer a stage cannot parse: the call log gets its shape (a JSON value
// followed by text that holds another, different JSON value: ErrSecondJSON
// since v3.2.x P1) and the try, the rejected file an excerpt; the excerpt
// is nowhere else.
func TestReportRejected(t *testing.T) {
	log := captureLog(t)
	answer := `{"zh":"第一版"}` + "\n\nWait, sentence 2 is wrong. Corrected:\n" + `{"zh":"第二版"}`
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody(answer)) })
	c, _, _ := newClient(t, srv.URL)
	ctx := WithCallTrace(sched.WithJob(context.Background(), model.Job{ID: "01JOB"}))
	for range 2 {
		res, err := c.Complete(ctx, config.RoleTranslate, secretSystem, secretUser, true)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]string
		perr := ParseJSON(res.Text, &v)
		if !errors.Is(perr, ErrBadJSON) || !errors.Is(perr, ErrSecondJSON) {
			t.Fatalf("parse error %v", perr)
		}
		ReportRejected(ctx, config.RoleTranslate, res.Text, perr)
	}
	raw, recs := readCalls(t, c.cfg.DataDir)
	noSecrets(t, raw, "Wait", "第一版", "第二版")
	if events(recs) != "start end rejected start end rejected" {
		t.Fatalf("events %q:\n%s", events(recs), raw)
	}
	for i, r := range []callRecord{recs[2], recs[5]} {
		if r.Kind != "not_json" || r.Try != i+1 || r.Call != recs[3*i].Call || r.JobID != "01JOB" || r.Model != "deepseek-flash" ||
			r.OutputShape == nil || r.OutputBytes != len(answer) || r.JSONEnd != len(`{"zh":"第一版"}`) || r.TrailingBytes == 0 || !r.TrailingJSON ||
			!strings.Contains(r.Error, "two different JSON values") {
			t.Errorf("rejected %d: %+v %+v", i, r, r.OutputShape)
		}
	}
	if recs[0].Try != 1 || recs[3].Try != 2 || recs[0].Call == recs[3].Call {
		t.Errorf("tries %d %d", recs[0].Try, recs[3].Try)
	}
	rawRej, rej := readLog[rejectedRecord](t, c.cfg.DataDir, rejectedPrefix)
	noSecrets(t, rawRej)
	if len(rej) != 2 || rej[0].Output != answer || rej[0].OmittedBytes != 0 || rej[0].Call != recs[0].Call || rej[0].Kind != "not_json" || rej[0].Role != config.RoleTranslate {
		t.Errorf("rejected file:\n%s", rawRej)
	}
	if s := log.String(); strings.Count(s, `msg="llm answer rejected"`) != 2 || !strings.Contains(s, "trailing_json=true") || strings.Contains(s, "Wait") {
		t.Errorf("service log:\n%s", s)
	}
}

// Without a trace, or with a client that is not the HTTP client, a rejected
// answer is only a warning in the service log.
func TestReportRejectedWithoutTrace(t *testing.T) {
	log := captureLog(t)
	ReportRejected(context.Background(), config.RoleRepair, "not json at all", errors.New("check: 2 patches for one sentence"))
	ReportRejected(WithCallTrace(context.Background()), config.RoleRepair, "x", nil) // no error: nothing
	s := log.String()
	if strings.Count(s, `msg="llm answer rejected"`) != 1 || !strings.Contains(s, "kind=check") || strings.Contains(s, "not json at all") {
		t.Errorf("service log:\n%s", s)
	}
}

func TestShapeOf(t *testing.T) {
	obj := `{"a":[1,2,{"b":"}"}]}`
	for _, tc := range []struct {
		name, text string
		want       OutputShape
	}{
		{"valid", obj, OutputShape{JSONEnd: len(obj)}},
		{"outer space", "  " + obj + "\n", OutputShape{JSONEnd: len(obj)}},
		{"fenced", "```json\n" + obj + "\n```", OutputShape{Fenced: true, JSONEnd: len(obj)}},
		{"trailing prose", obj + "\nWait, that is wrong.", OutputShape{JSONEnd: len(obj), TrailingBytes: len("Wait, that is wrong.")}},
		{"trailing correction", obj + " Correction: " + `{"a":2}`, OutputShape{JSONEnd: len(obj), TrailingBytes: len(`Correction: {"a":2}`), TrailingJSON: true}},
		{"trailing array", obj + "\n" + `[{"a":2}]`, OutputShape{JSONEnd: len(obj), TrailingBytes: len(`[{"a":2}]`), TrailingJSON: true}},
		{"trailing braces that are no value", obj + " see {note} and []", OutputShape{JSONEnd: len(obj), TrailingBytes: len("see {note} and []")}},
		{"prose first", "Here you go: " + obj, OutputShape{}},
		{"cut off", `{"a":[1,2`, OutputShape{}},
		{"empty", "", OutputShape{}},
	} {
		tc.want.OutputBytes = len(tc.text)
		if got := ShapeOf(tc.text); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestExcerpt(t *testing.T) {
	if out, omitted := excerpt("short"); out != "short" || omitted != 0 {
		t.Errorf("short: %q %d", out, omitted)
	}
	long := "HEAD" + strings.Repeat("中", RejectedExcerptBytes) + "TAIL"
	out, omitted := excerpt(long)
	if !strings.HasPrefix(out, "HEAD") || !strings.HasSuffix(out, "TAIL") || !strings.Contains(out, "\n…\n") {
		t.Errorf("excerpt ends: %q … %q", out[:8], out[len(out)-8:])
	}
	if !strings.ContainsRune(out, '中') || strings.ContainsRune(out, '\uFFFD') || len(out)-len("\n…\n")+omitted != len(long) || len(out) > RejectedExcerptBytes+8 {
		t.Errorf("excerpt of %d bytes: %d kept, %d omitted", len(long), len(out), omitted)
	}
}

// Old call log files go, by age and by total size; today's files and the
// debug log's files stay.
func TestCallLogPrune(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	l := newCallLog(dir)
	l.now = func() time.Time { return now }
	day := func(d int) string { return now.AddDate(0, 0, -d).Format(callLogDay) }
	write := func(name string, size int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(callsPrefix+day(CallLogKeepDays+1)+callLogExt, 10)
	write(rejectedPrefix+day(CallLogKeepDays+1)+callLogExt, 10)
	write(callsPrefix+day(CallLogKeepDays)+callLogExt, 10)
	write(callsPrefix+day(1)+callLogExt, 10)
	write("01JOB.jsonl", 10)                    // debug log
	write(callsPrefix+"notaday"+callLogExt, 10) // not ours
	l.write(callsPrefix, map[string]string{"event": "start"})
	l.write(callsPrefix, map[string]string{"event": "end"})

	names := func() string {
		es, _ := os.ReadDir(dir)
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		return strings.Join(out, " ")
	}
	want := strings.Join([]string{"01JOB.jsonl", callsPrefix + day(CallLogKeepDays) + callLogExt, callsPrefix + day(1) + callLogExt,
		callsPrefix + day(0) + callLogExt, callsPrefix + "notaday" + callLogExt}, " ")
	if got := names(); got != want {
		t.Errorf("after pruning by age:\n got %s\nwant %s", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, callsPrefix+day(0)+callLogExt)); string(b) != "{\"event\":\"start\"}\n{\"event\":\"end\"}\n" {
		t.Errorf("today's file: %q", b)
	}

	// The next day: the files together are over the limit, the oldest go
	// first; the day that just ended is the newest of them and stays.
	big := CallLogMaxBytes / 2
	write(callsPrefix+day(3)+callLogExt, big)
	write(callsPrefix+day(2)+callLogExt, big)
	write(rejectedPrefix+day(0)+callLogExt, big)
	now = now.AddDate(0, 0, 1)
	l.write(rejectedPrefix, map[string]string{"kind": "not_json"})
	for _, gone := range []string{callsPrefix + "2026-10-27", callsPrefix + "2026-11-07", callsPrefix + "2026-11-08"} {
		if strings.Contains(names(), gone) {
			t.Errorf("%s is still there: %s", gone, names())
		}
	}
	for _, kept := range []string{callsPrefix + "2026-11-09", callsPrefix + "2026-11-10", rejectedPrefix + "2026-11-10", rejectedPrefix + "2026-11-11", "01JOB.jsonl"} {
		if !strings.Contains(names(), kept) {
			t.Errorf("%s is gone: %s", kept, names())
		}
	}
}

// A folder that cannot be written is reported once and never fails a request.
func TestCallLogUnwritable(t *testing.T) {
	log := captureLog(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody("ok")) })
	c := New(testConfig(t, srv.URL), nil, filepath.Join(blocker, "llm"), nil)
	for range 2 {
		if _, err := c.Complete(context.Background(), config.RoleSummary, "s", "u", false); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(log.String(), `msg="llm call log"`); n != 1 {
		t.Errorf("%d warnings:\n%s", n, log.String())
	}
}
