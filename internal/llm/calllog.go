// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// The call log (v3.2.x P0): what every LLM request did, always written
// (unlike the debug log of debuglog.go, which holds whole requests and
// answers and needs log_level = debug). JSON lines in {data_dir}/logs/llm/,
// one file per local day:
//
//	calls-2026-11-10.jsonl     one line per event, never a prompt or an answer
//	rejected-2026-11-10.jsonl  excerpts of the answers a stage could not use
//	                           (and, of those used although text follows the
//	                           JSON, that text only)
//
// Events of calls-*.jsonl (callRecord):
//
//	start     an HTTP attempt is about to be sent (the API pool granted it):
//	          role, provider, model, effort, max_tokens, attempt, why it is
//	          a retry. A start without an end is a request still running.
//	end       the attempt is over: result, HTTP status, request id, rate
//	          limit headers, time to the response headers and to the end of
//	          the body, stop reason, usage as the provider sent it, the
//	          answer's content blocks by type
//	failed    Complete gave up: the error the job gets
//	rejected  a stage could not use an answer (not JSON, or its check
//	          refused it): the shape of the answer, never its text
//	trailing  a stage used an answer although text follows its JSON value
//	          (ParseAnswer): the shape of the answer, never its text
//
// headers_ms is when the provider started to answer, duration_ms when the
// body was read. Streamed requests (stream.go: current Claude models) say so
// ("stream": true, start: idle_timeout_s) and their end records add
// first_token_ms, first_text_ms, last_event_ms, max_gap_ms,
// max_content_gap_ms, first_event_ms and first_content_ms (numbers only);
// the other requests are not streamed and have no such times.
//
// The folder logs/llm is left out of the diagnostics zip (api, diagLogs).
// Files older than CallLogKeepDays are removed, the oldest first while the
// rest is larger than CallLogMaxBytes; the debug log's files under the same
// rules, counted apart (pruneLocked). Failed attempts, failed calls and
// rejected answers also go to the service log (slog, level WARN).
const (
	CallLogKeepDays = 14
	CallLogMaxBytes = 128 << 20
	// RejectedExcerptBytes is how much of a rejected answer is kept: its
	// first and last RejectedExcerptBytes/2 bytes when it is longer.
	RejectedExcerptBytes = 16 << 10
)

const (
	callsPrefix    = "calls-"
	rejectedPrefix = "rejected-"
	callLogExt     = ".jsonl"
	callLogDay     = "2006-01-02"
	callLogTime    = "2006-01-02T15:04:05.000Z07:00"
)

// callRecord is one line of calls-*.jsonl.
type callRecord struct {
	Time  string `json:"time"`
	Event string `json:"event"` // start | end | failed | rejected | trailing
	Call  string `json:"call"`  // one Complete call (all its attempts)

	JobID      string `json:"job,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Scope      string `json:"scope,omitempty"`
	ScopeID    string `json:"scope_id,omitempty"`
	JobAttempt int    `json:"job_attempt,omitempty"`

	Role       string `json:"role"`
	Provider   string `json:"provider,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	API        string `json:"api,omitempty"` // openai | anthropic
	Model      string `json:"model,omitempty"`
	// Try counts the stage's requests for the same answer (WithCallTrace):
	// 1 = the first, 2 = the first business retry. 0 = the stage keeps no trace.
	Try int `json:"try,omitempty"`
	// Attempt counts the HTTP attempts of the call, 0 = the first.
	Attempt int `json:"attempt"`

	// start
	Reason    string `json:"reason,omitempty"`     // attempt > 0: why it is sent again
	Effort    string `json:"effort,omitempty"`     // thinking level sent (model.Thinking*)
	MaxTokens int    `json:"max_tokens,omitempty"` // 0 = not sent
	// ThinkingBudget: the tokens max_tokens holds for thinking (the budget;
	// on a current Claude model the headroom of maxtokens.go, v3.2.x D1).
	ThinkingBudget int  `json:"thinking_budget,omitempty"`
	JSONMode       bool `json:"json_mode,omitempty"`
	// Structured: the request carries a JSON Schema (output_config.format).
	Structured bool `json:"structured,omitempty"`
	// ThinkingType and EffortSent: the thinking.type and output_config.effort
	// of an Anthropic request as sent ("" = the field is absent). They differ
	// from Effort where a model cannot do the level asked for (thinking.go).
	ThinkingType string `json:"thinking_type,omitempty"`
	EffortSent   string `json:"effort_sent,omitempty"`
	TimeoutS     int    `json:"timeout_s,omitempty"`
	RequestBytes int    `json:"request_bytes,omitempty"`
	QueuedMS     int64  `json:"queued_ms,omitempty"` // wait for the API pool
	// Stream (start, end): the request is streamed (stream.go).
	Stream bool `json:"stream,omitempty"`
	// IdleTimeoutS (start, streamed requests): the idle timeout, 0 = none.
	IdleTimeoutS *int `json:"idle_timeout_s,omitempty"`

	// end
	Result          string            `json:"result,omitempty"` // ok | truncated | refused | http_error | network | timeout | idle | time_limit | bad_response | canceled
	Status          int               `json:"status,omitempty"`
	RequestID       string            `json:"request_id,omitempty"`  // response header
	ResponseID      string            `json:"response_id,omitempty"` // "id" of the answer
	RateLimit       map[string]string `json:"rate_limit,omitempty"`  // rate limit headers as sent
	HeadersMS       *int64            `json:"headers_ms,omitempty"`
	DurationMS      *int64            `json:"duration_ms,omitempty"`
	ResponseBytes   int               `json:"response_bytes,omitempty"`
	StopReason      string            `json:"stop_reason,omitempty"`   // stop_reason | finish_reason
	StopCategory    string            `json:"stop_category,omitempty"` // refusal category
	Usage           json.RawMessage   `json:"usage,omitempty"`         // as the provider sent it
	ReasoningTokens *int64            `json:"reasoning_tokens,omitempty"`
	Blocks          map[string]int    `json:"blocks,omitempty"` // content blocks of the answer by type
	TextChars       *int              `json:"text_chars,omitempty"`
	ThinkingChars   int               `json:"thinking_chars,omitempty"`
	// EmptyText: the provider answered and the answer has no text (with
	// result truncated: the output limit went to thinking).
	EmptyText bool `json:"empty_text,omitempty"`
	// Streamed answers (stream.go), times since the request was sent: the
	// first thinking or text delta, the first text delta, the last event of
	// any kind (pings included). Absent when it did not happen.
	FirstTokenMS *int64 `json:"first_token_ms,omitempty"`
	FirstTextMS  *int64 `json:"first_text_ms,omitempty"`
	LastEventMS  *int64 `json:"last_event_ms,omitempty"`
	// MaxGapMS / MaxContentGapMS (streamed answers, SEC6 O1): the longest
	// time between two consecutive events of the attempt, pings and comment
	// lines included / between two consecutive events other than ping.
	// Absent with fewer than two such events.
	MaxGapMS        *int64 `json:"max_gap_ms,omitempty"`
	MaxContentGapMS *int64 `json:"max_content_gap_ms,omitempty"`
	// FirstEventMS / FirstContentMS (streamed answers, R17 S7): the time
	// from when reading the stream began (about headers_ms) to the first
	// event, pings and comment lines included / to the first event other
	// than ping. Absent when no such event arrived.
	FirstEventMS   *int64 `json:"first_event_ms,omitempty"`
	FirstContentMS *int64 `json:"first_content_ms,omitempty"`
	// StreamError: error.type of an error event in the stream (HTTP 200).
	StreamError string `json:"stream_error,omitempty"`

	// end (failures), failed, rejected
	Error string `json:"error,omitempty"`

	// rejected
	Kind string `json:"kind,omitempty"` // not_json | check | trailing
	*OutputShape
}

// rejectedRecord is one line of rejected-*.jsonl.
type rejectedRecord struct {
	Time  string `json:"time"`
	Call  string `json:"call,omitempty"`
	JobID string `json:"job,omitempty"`
	Role  string `json:"role"`
	Model string `json:"model,omitempty"`
	Kind  string `json:"kind"`
	Error string `json:"error"`
	// Output: the answer (kind "trailing": the text after its JSON value),
	// cut to RejectedExcerptBytes.
	Output string `json:"output"`
	// OmittedBytes: bytes left out of the middle of Output.
	OmittedBytes int `json:"omitted_bytes,omitempty"`
}

// callLog appends to the day's files (T-PERF-08). The files stay open while
// an HTTP attempt is in flight (between its start and end records; a busy
// run with parallel requests keeps them open the whole time) and are closed
// when the last attempt in flight has written its end record, when the day
// changes, by Close, and after a failed write; the next write opens them
// again. A line written with no attempt in flight (failed, rejected,
// trailing) opens and closes its file as before. No timer: nothing runs
// outside the writers (synctest bubbles in tests).
//
// Every line is one Write of the whole line to the file itself, without a
// buffer in the process, so a line is visible to readers as soon as write
// returns and a crash loses at most the part of the line being written.
// Writers are serialized by mu.
//
// The handles are opened with Go's default sharing (read and write):
// readers such as the copy of a data directory move see the files while
// they are open, but on Windows an open handle keeps the folder from being
// renamed or removed, hence Close (HTTPClient.CloseLogs: App shutdown, the
// pause before a data directory move).
type callLog struct {
	dir string
	now func() time.Time

	mu       sync.Mutex
	day      string              // the day pruned for, the day of files
	files    map[string]*os.File // prefix → open file of day
	inFlight int                 // attempts started and not ended
	warned   bool

	// debugMu serializes the debug log's writes (debuglog.go) with the
	// pruning of its files.
	debugMu sync.Mutex
}

func newCallLog(dir string) *callLog {
	return &callLog{dir: dir, now: time.Now}
}

func (l *callLog) stamp() string {
	if l == nil || l.now == nil {
		return time.Now().Format(callLogTime)
	}
	return l.now().Format(callLogTime)
}

// write appends v as one line to {dir}/{prefix}{day}.jsonl.
func (l *callLog) write(prefix string, v any) {
	l.writeFlight(prefix, v, 0)
}

// writeFlight is write for the records of an attempt: flight +1 for its
// start, -1 for its end. The files stay open while attempts are in flight.
func (l *callLog) writeFlight(prefix string, v any, flight int) {
	if l == nil || l.dir == "" {
		return
	}
	var line bytes.Buffer
	enc := json.NewEncoder(&line) // one line, "\n" at its end
	enc.SetEscapeHTML(false)      // "->", "<html>" of an error page: as they are
	encErr := enc.Encode(v)
	day := l.now().Format(callLogDay)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight = max(l.inFlight+flight, 0)
	defer func() {
		if l.inFlight == 0 {
			l.closeFilesLocked()
		}
	}()
	if encErr != nil {
		l.warnLocked(encErr)
		return
	}
	if day != l.day {
		l.closeFilesLocked()
		if err := os.MkdirAll(l.dir, 0o755); err != nil {
			l.warnLocked(err)
			return
		}
		l.pruneLocked(day)
		l.day = day
	}
	f := l.files[prefix]
	if f == nil {
		var err error
		f, err = os.OpenFile(filepath.Join(l.dir, prefix+day+callLogExt), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			l.warnLocked(err)
			return
		}
		if l.files == nil {
			l.files = map[string]*os.File{}
		}
		l.files[prefix] = f
	}
	if _, err := f.Write(line.Bytes()); err != nil {
		l.warnLocked(err)
		f.Close()
		delete(l.files, prefix)
	}
}

// Close closes the open files of the call log; a later write opens them
// again.
func (l *callLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeFilesLocked()
}

// abandon ends an attempt in flight whose end record is not written: the
// request panicked between start and end (review D16). Only the count of
// attempts in flight goes down; the files are closed when it reaches 0, as
// after an end record.
func (l *callLog) abandon() {
	if l == nil || l.dir == "" {
		return // start counted nothing either (writeFlight)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight = max(l.inFlight-1, 0)
	if l.inFlight == 0 {
		l.closeFilesLocked()
	}
}

func (l *callLog) closeFilesLocked() error {
	var first error
	for p, f := range l.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
		delete(l.files, p)
	}
	return first
}

// CloseLogs closes the files the client holds open in its log folder (the
// call log; the debug log opens and closes its file on every write). Call
// it before the data folder is moved and when the application stops; a
// later request opens them again.
func (c *HTTPClient) CloseLogs() error {
	if c == nil {
		return nil
	}
	return c.calls.Close()
}

// warnLocked reports a write failure once: a full disk must not fill the
// service log too.
func (l *callLog) warnLocked(err error) {
	if l.warned {
		return
	}
	l.warned = true
	slog.Warn("llm call log", "err", err)
}

// pruneLocked removes old files of the folder: the call log's (by the day
// in their names) and, under limits of their own, the debug log's (by the
// day they were last written; debuglog.go, T-PERF-08). For each kind: the
// files of days before today that are older than CallLogKeepDays, then the
// oldest ones while the files of the kind together are larger than
// CallLogMaxBytes. Files of other names are not touched.
func (l *callLog) pruneLocked(today string) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	now := l.now()
	oldest := now.AddDate(0, 0, -CallLogKeepDays).Format(callLogDay)
	pruneLogFiles(l.dir, entries, today, oldest, func(name string, _ os.FileInfo) (string, bool) {
		return callLogFileDay(name)
	})
	l.debugMu.Lock()
	defer l.debugMu.Unlock()
	pruneLogFiles(l.dir, entries, today, oldest, func(name string, fi os.FileInfo) (string, bool) {
		if !isDebugLogName(name) {
			return "", false
		}
		return fi.ModTime().In(now.Location()).Format(callLogDay), true
	})
}

// pruneLogFiles applies the retention rule of pruneLocked to the files of
// dir (entries, as os.ReadDir listed them) that dayOf says are of the kind,
// and of which day. Only regular files are considered: symbolic links,
// junctions and folders are never followed or removed.
func pruneLogFiles(dir string, entries []os.DirEntry, today, oldest string, dayOf func(name string, fi os.FileInfo) (string, bool)) {
	type logFile struct {
		name, day string
		size      int64
	}
	var files []logFile
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info() // of the entry itself, not of a link's target
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		day, ok := dayOf(e.Name(), fi)
		if !ok {
			continue
		}
		files = append(files, logFile{e.Name(), day, fi.Size()})
		total += fi.Size()
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].day < files[j].day })
	for _, f := range files {
		if f.day >= today || (f.day >= oldest && total <= CallLogMaxBytes) {
			continue
		}
		if os.Remove(filepath.Join(dir, f.name)) == nil {
			total -= f.size
		}
	}
}

// callLogFileDay returns the day of a call log file name.
func callLogFileDay(name string) (string, bool) {
	for _, p := range []string{callsPrefix, rejectedPrefix} {
		if rest, ok := strings.CutPrefix(name, p); ok {
			day, ok := strings.CutSuffix(rest, callLogExt)
			if !ok {
				return "", false
			}
			if _, err := time.Parse(callLogDay, day); err != nil {
				return "", false
			}
			return day, true
		}
	}
	return "", false
}

func newCallID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().Format("150405.000000")
	}
	return hex.EncodeToString(b[:])
}

// base returns the fields every record of a call has.
func (l *callLog) base(ctx context.Context, event string, call callInfo, attempt int) callRecord {
	r := callRecord{
		Time: l.stamp(), Event: event, Call: call.id,
		Role: call.role, Provider: call.providerName, ProviderID: call.provider, API: call.api, Model: call.model,
		Try: call.try, Attempt: attempt,
	}
	if j, ok := sched.JobFrom(ctx); ok {
		r.JobID, r.Stage, r.Scope, r.ScopeID, r.JobAttempt = j.ID, j.Stage, j.Scope, j.ScopeID, j.Attempt
	}
	return r
}

// start records an attempt that is about to be sent.
func (l *callLog) start(ctx context.Context, call callInfo, attempt int, reason string, reqBytes int, queued time.Duration) {
	if l == nil {
		return
	}
	r := l.base(ctx, "start", call, attempt)
	r.Reason = reason
	r.Effort = call.effort
	r.MaxTokens, r.ThinkingBudget, r.JSONMode = call.maxTokens, call.budget, call.jsonMode
	r.Structured, r.ThinkingType, r.EffortSent = call.structured, call.thinkingType, call.effortSent
	r.TimeoutS = int(call.timeout / time.Second)
	r.RequestBytes = reqBytes
	r.QueuedMS = queued.Milliseconds()
	if call.stream {
		r.Stream = true
		idle := int(call.idle / time.Second)
		r.IdleTimeoutS = &idle
	}
	l.writeFlight(callsPrefix, r, +1)
}

// attemptOutcome is what an attempt's HTTP exchange gave (attempt, http.go).
type attemptOutcome struct {
	status    int
	header    http.Header   // nil: no response
	headersAt time.Duration // when the response headers arrived (0: they did not)
	duration  time.Duration
	// respBody: the body; of a streamed answer the raw events, kept only for
	// the debug log (nil otherwise)
	respBody []byte
	// stream: what a streamed answer (text/event-stream) gave; nil = the
	// body was read whole
	stream *streamOutcome
	// idle: the idle timeout aborted the attempt
	idle bool
}

// end records a finished attempt; failures also go to the service log. It
// returns the record, which the request event of the attempt is made from
// (events.go); without a log (nil) nothing is written but the record is.
func (l *callLog) end(ctx context.Context, call callInfo, attempt int, o attemptOutcome, aerr *attemptErr) callRecord {
	r := l.base(ctx, "end", call, attempt)
	r.Result = attemptResult(ctx, aerr)
	r.Status = o.status
	r.Stream = call.stream
	d := o.duration.Milliseconds()
	r.DurationMS = &d
	if o.header != nil {
		h := o.headersAt.Milliseconds()
		r.HeadersMS = &h
		r.RequestID = requestID(o.header)
		r.RateLimit = rateLimitHeaders(o.header)
		r.ResponseBytes = len(o.respBody)
	}
	if s := o.stream; s != nil {
		r.ResponseBytes = s.bytes
		r.FirstTokenMS, r.FirstTextMS, r.LastEventMS = msOrNil(s.firstToken), msOrNil(s.firstText), msOrNil(s.lastEvent)
		r.MaxGapMS, r.MaxContentGapMS = msOrNil(s.maxGap), msOrNil(s.maxContentGap)
		r.FirstEventMS, r.FirstContentMS = msOrNil(s.firstEvent), msOrNil(s.firstContent)
		r.StreamError = s.errorType
	}
	if o.status == http.StatusOK {
		var m respMeta
		if o.stream != nil {
			m = o.stream.meta
		} else {
			m = answerMeta(call.api, o.respBody)
		}
		r.ResponseID, r.StopReason, r.StopCategory = m.id, m.stopReason, m.stopCategory
		r.Usage, r.ReasoningTokens = m.usage, m.reasoningTokens
		r.Blocks, r.ThinkingChars = m.blocks, m.thinkingChars
		if m.parsed {
			r.TextChars = &m.textChars
			r.EmptyText = m.textChars == 0
		}
	}
	if aerr != nil {
		switch {
		case aerr.body != "":
			r.Error = aerr.body // a redacted excerpt of the server's body
		case aerr.err != nil:
			r.Error = redactSnippet([]byte(aerr.err.Error()), call.key)
		}
	}
	l.writeFlight(callsPrefix, r, -1)
	if aerr != nil && r.Result != "canceled" {
		slog.Warn("llm attempt failed", "call", call.id, "job", r.JobID, "role", call.role, "provider", call.providerName,
			"model", call.model, "attempt", attempt, "result", r.Result, "status", o.status, "request_id", r.RequestID,
			"stop_reason", r.StopReason, "max_tokens", call.maxTokens, "effort", call.effort, "empty_text", r.EmptyText,
			"duration_ms", d, "error", r.Error)
	}
	return r
}

// failed records the error Complete returns after attempts attempts.
func (l *callLog) failed(ctx context.Context, call callInfo, attempt int, err error) {
	if l == nil || err == nil {
		return
	}
	r := l.base(ctx, "failed", call, attempt)
	r.Error = redactSnippet([]byte(err.Error()), call.key)
	l.write(callsPrefix, r)
	slog.Warn("llm call failed", "call", call.id, "job", r.JobID, "role", call.role, "provider", call.providerName,
		"model", call.model, "attempts", attempt+1, "error", r.Error)
}

// msOrNil returns d in milliseconds, nil when d < 0 (it did not happen).
func msOrNil(d time.Duration) *int64 {
	if d < 0 {
		return nil
	}
	v := d.Milliseconds()
	return &v
}

// attemptResult names the outcome of an attempt.
func attemptResult(ctx context.Context, e *attemptErr) string {
	if e == nil {
		return "ok"
	}
	switch e.kind {
	case failTruncated:
		return "truncated"
	case failRefusal:
		return "refused"
	case failStatus:
		return "http_error"
	case failTimeout:
		return "timeout"
	case failEnvelope:
		return "bad_response"
	case failIdle:
		return "idle"
	case failTimeLimit:
		return "time_limit"
	case failResource:
		return "insufficient_resource"
	}
	if ctx.Err() != nil {
		return "canceled"
	}
	return "network"
}

// retryReason is the reason of the next attempt's start record.
func retryReason(e *attemptErr, wait time.Duration) string {
	what := "network error"
	switch e.kind {
	case failStatus:
		what = "http " + strconv.Itoa(e.status)
	case failTimeout:
		what = "timeout"
	case failEnvelope:
		what = "unreadable response"
	case failIdle:
		what = "idle timeout"
	case failResource:
		what = "insufficient system resource"
	}
	s := what + ", waited " + wait.String()
	if e.hasRA {
		s += " (retry-after)"
	}
	return s
}

// requestID returns the request id of a response: request-id (Anthropic),
// x-request-id (OpenAI-compatible), else any header named like one.
func requestID(h http.Header) string {
	for _, k := range []string{"Request-Id", "X-Request-Id"} {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	for k, v := range h {
		if n := strings.ToLower(k); len(v) > 0 && (strings.HasSuffix(n, "request-id") || strings.HasSuffix(n, "trace-id")) {
			return v[0]
		}
	}
	return ""
}

// rateLimitHeaders returns the rate limit headers of a response
// (anthropic-ratelimit-*, x-ratelimit-*, retry-after, x-should-retry) under
// their lower-case names, or nil.
func rateLimitHeaders(h http.Header) map[string]string {
	var out map[string]string
	for k, v := range h {
		n := strings.ToLower(k)
		if len(v) == 0 || !(strings.HasPrefix(n, "anthropic-ratelimit-") || strings.HasPrefix(n, "x-ratelimit-") ||
			n == "retry-after" || n == "x-should-retry") {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[n] = strings.Join(v, ", ")
	}
	return out
}

// respMeta is what the call log reads from a 200 answer, next to the
// Result the parsers of http.go and anthropic.go return.
type respMeta struct {
	parsed          bool // the body is a JSON object
	id              string
	stopReason      string
	stopCategory    string
	usage           json.RawMessage
	reasoningTokens *int64
	blocks          map[string]int
	textChars       int
	thinkingChars   int
}

// maxUsageBytes: a usage object larger than this is not copied to the log.
const maxUsageBytes = 4 << 10

// The content block types of an answer as the call log counts them (SEC4
// L6): the type comes from the provider, any Anthropic-compatible endpoint
// the user entered, so it is untrusted. A type is kept when it is an
// identifier of lower-case letters, digits and underscores of at most
// maxBlockTypeLen bytes (Anthropic's are such: "text", "thinking",
// "redacted_thinking", "server_tool_use", ...), else counted as
// BlockTypeOther; after maxBlockTypes different types, every new one is
// counted as BlockTypeOther too.
const (
	maxBlockTypeLen = 64
	maxBlockTypes   = 16
	BlockTypeOther  = "other"
)

// countBlock counts one content block of type typ in m (allocated when nil).
func countBlock(m map[string]int, typ string) map[string]int {
	if m == nil {
		m = map[string]int{}
	}
	k := cleanBlockType(typ)
	if _, known := m[k]; !known && len(m) >= maxBlockTypes {
		k = BlockTypeOther
	}
	m[k]++
	return m
}

// cleanBlockType returns typ when it is a short identifier, else
// BlockTypeOther.
func cleanBlockType(typ string) string {
	if typ == "" || len(typ) > maxBlockTypeLen {
		return BlockTypeOther
	}
	for i := 0; i < len(typ); i++ {
		c := typ[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return BlockTypeOther
		}
	}
	return typ
}

// answerMeta reads the meta data of a 200 answer of the provider type.
func answerMeta(api string, body []byte) respMeta {
	var m respMeta
	if api == model.ProviderAnthropic {
		var v struct {
			ID          string `json:"id"`
			StopReason  string `json:"stop_reason"`
			StopDetails *struct {
				Category string `json:"category"`
			} `json:"stop_details"`
			Usage   json.RawMessage `json:"usage"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"content"`
		}
		if json.Unmarshal(body, &v) != nil {
			return m
		}
		m.parsed, m.id, m.stopReason = true, v.ID, v.StopReason
		if v.StopDetails != nil {
			m.stopCategory = CleanRefusalCategory(v.StopDetails.Category) // untrusted (SEC1 L1)
		}
		m.usage = usageOf(v.Usage)
		m.reasoningTokens = anthropicThinkingTokens(v.Usage)
		for _, b := range v.Content {
			m.blocks = countBlock(m.blocks, b.Type) // untrusted (SEC4 L6)
			switch b.Type {
			case "text":
				m.textChars += utf8.RuneCountInString(b.Text)
			case "thinking":
				m.thinkingChars += utf8.RuneCountInString(b.Thinking)
			}
		}
		return m
	}
	var v struct {
		ID      string `json:"id"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &v) != nil {
		return m
	}
	m.parsed, m.id = true, v.ID
	m.usage = usageOf(v.Usage)
	if len(v.Choices) > 0 {
		c := v.Choices[0]
		m.stopReason = c.FinishReason
		m.textChars = utf8.RuneCountInString(c.Message.Content)
		m.thinkingChars = utf8.RuneCountInString(c.Message.ReasoningContent)
	}
	// OpenAI-compatible providers report the reasoning part of the
	// completion tokens here (DeepSeek in thinking mode); Anthropic's in
	// output_tokens_details (anthropicThinkingTokens).
	var u struct {
		Details *struct {
			ReasoningTokens *int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(v.Usage, &u) == nil && u.Details != nil {
		m.reasoningTokens = u.Details.ReasoningTokens
	}
	return m
}

// anthropicThinkingTokens reads usage.output_tokens_details.thinking_tokens
// of an Anthropic answer (nil when absent): the part of output_tokens the
// model spent thinking, also when the thinking text is omitted (C91: an
// answer cut off at max_tokens by thinking alone shows it here).
func anthropicThinkingTokens(usage json.RawMessage) *int64 {
	var u struct {
		Details *struct {
			ThinkingTokens *int64 `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	}
	if len(usage) == 0 || json.Unmarshal(usage, &u) != nil || u.Details == nil {
		return nil
	}
	return u.Details.ThinkingTokens
}

func usageOf(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || len(raw) > maxUsageBytes || string(raw) == "null" {
		return nil
	}
	return raw
}

// ---------- answers a stage could not use ----------

// OutputShape describes an answer without quoting it.
type OutputShape struct {
	OutputBytes int `json:"output_bytes"`
	// Fenced: the answer has a ``` line.
	Fenced bool `json:"fenced,omitempty"`
	// JSONEnd: the answer (without fences and outer space) starts with a
	// complete JSON value of this many bytes; 0 = it does not.
	JSONEnd int `json:"json_end,omitempty"`
	// TrailingBytes: bytes after that value (0 when there is no value).
	TrailingBytes int `json:"trailing_bytes,omitempty"`
	// TrailingJSON: the text after the value holds another JSON object or
	// array, most likely the model's corrected answer.
	TrailingJSON bool `json:"trailing_json,omitempty"`
}

// ShapeOf describes text, an answer that was expected to be JSON.
func ShapeOf(text string) OutputShape {
	sh := OutputShape{OutputBytes: len(text)}
	s := stripFences(text)
	sh.Fenced = len(s) != len(text)
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return sh
	}
	end, ok := firstJSONValue(s)
	if !ok {
		return sh
	}
	sh.JSONEnd = end
	rest := strings.TrimSpace(s[end:])
	sh.TrailingBytes = len(rest)
	sh.TrailingJSON = trailingJSONScan(rest, newCandidateScan(len(rest)))
	return sh
}

// trailingJSONScan reports whether rest holds a JSON object or array other
// than "{}" / "[]": every "{" or "[" of it may start the corrected answer,
// at most maxCandidates of them, all within the scan budget of sc (SEC1 L2).
func trailingJSONScan(rest string, sc *candidateScan) bool {
	n := 0
	for i := 0; i < len(rest) && n < maxCandidates; i++ {
		if rest[i] != '{' && rest[i] != '[' {
			continue
		}
		n++
		end, ok, spent := sc.value(rest[i:])
		if spent {
			return false
		}
		if ok && end > 2 {
			return true
		}
	}
	return false
}

// firstJSONValue returns the length of the JSON value s starts with.
func firstJSONValue(s string) (int, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return 0, false
	}
	return int(dec.InputOffset()), true
}

// excerpt cuts s to RejectedExcerptBytes at rune boundaries: its start and
// its end, where a model's second thoughts are.
func excerpt(s string) (out string, omitted int) {
	if len(s) <= RejectedExcerptBytes {
		return s, 0
	}
	half := RejectedExcerptBytes / 2
	a, b := half, len(s)-half
	for a > 0 && !utf8.RuneStart(s[a]) {
		a--
	}
	for b < len(s) && !utf8.RuneStart(s[b]) {
		b++
	}
	return s[:a] + "\n…\n" + s[b:], b - a
}

type callTraceKey struct{}

// callTrace follows the Complete calls made under one context: the stage's
// requests for the same answer, one after the other.
type callTrace struct {
	mu   sync.Mutex
	n    int
	log  *callLog
	last callInfo
}

// WithCallTrace returns a context for the requests of one answer (the first
// and its business retries): the call log numbers them (try), and
// ReportRejected under the same context names the call whose answer was
// refused.
func WithCallTrace(ctx context.Context) context.Context {
	return context.WithValue(ctx, callTraceKey{}, &callTrace{})
}

func traceFrom(ctx context.Context) *callTrace {
	t, _ := ctx.Value(callTraceKey{}).(*callTrace)
	return t
}

// begin counts a call and remembers it.
func (t *callTrace) begin(l *callLog, call callInfo) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n++
	call.try = t.n
	call.key = ""
	t.log, t.last = l, call
	return t.n
}

// ReportRejected records that a stage could not use the answer text of its
// last Complete call under ctx (WithCallTrace): err is ErrBadJSON from
// ParseJSON, or what the stage's check said. The call log gets the shape of
// the answer, rejected-*.jsonl an excerpt, the service log a warning.
// Without a trace or with a client other than HTTPClient only the service
// log is written.
func ReportRejected(ctx context.Context, role, text string, err error) {
	if err == nil {
		return
	}
	kind := "check"
	if errors.Is(err, ErrBadJSON) {
		kind = "not_json"
	}
	reportAnswer(ctx, "rejected", kind, role, text, text, snippet([]byte(err.Error())), rejectedLogReason(err))
}

// rejectedLogReason is the reason of a rejected answer as the service log
// and the calls file hold it: never a quote of the answer (lecture content;
// the rejected file keeps the whole error). An answer without any JSON:
// its error quotes the answer's start, which is left out (review fix of
// v3.2.x P1). An answer a stage's check refused: the check's text quotes
// what the model wrote (a title, a regex, the file names it failed on), so
// only a fixed label, the error's type and the number of problems are kept
// (SEC1 L4).
func rejectedLogReason(err error) string {
	switch {
	case errors.Is(err, ErrSecondJSON):
		return ErrSecondJSON.Error()
	case errors.Is(err, errNoJSONObject):
		return ErrBadJSON.Error() + ": " + errNoJSONObject.Error()
	case errors.Is(err, ErrBadJSON):
		// the decoder's message: a syntax or type error, at most a character
		return snippet([]byte(err.Error()))
	}
	s := fmt.Sprintf("check failed (%T)", err)
	var rp interface{ Problems() []string }
	if errors.As(err, &rp) {
		s += fmt.Sprintf(": %d problem(s)", len(rp.Problems()))
	}
	return s
}

// reportTrailing records that a stage used the answer text of its last
// Complete call under ctx although text follows its JSON value (ParseAnswer;
// v3.2.x P1): the same records as ReportRejected, as event and kind
// "trailing". The rejected file gets only the text after the value (SEC1
// T1): the value itself was used and is stored where the stage keeps its
// results, so a model that comments on every answer does not turn the file
// into a copy of the lecture's translation.
func reportTrailing(ctx context.Context, role, text string) {
	const msg = "text after the JSON value (the value was used; only that text is kept)"
	reportAnswer(ctx, "trailing", "trailing", role, text, trailingText(text), msg, msg)
}

// trailingText returns what follows the first JSON value of text (without
// fences and outer space), or text itself when it does not start with one.
func trailingText(text string) string {
	s := strings.TrimSpace(stripFences(text))
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return text
	}
	end, ok := firstJSONValue(s)
	if !ok {
		return text
	}
	return strings.TrimSpace(s[end:])
}

// reportAnswer writes the records of ReportRejected and reportTrailing.
// text is the answer (its shape is logged), stored what the rejected file
// keeps of it; msg is the reason for the rejected file, logMsg the one
// without lecture content for the service log and the calls file.
func reportAnswer(ctx context.Context, event, kind, role, text, stored, msg, logMsg string) {
	sh := ShapeOf(text)

	var (
		l    *callLog
		call = callInfo{role: role}
	)
	if t := traceFrom(ctx); t != nil {
		t.mu.Lock()
		if t.last.id != "" {
			l, call = t.log, t.last
		}
		t.mu.Unlock()
	}
	jobID := ""
	if j, ok := sched.JobFrom(ctx); ok {
		jobID = j.ID
	}
	what := "llm answer rejected"
	if event == "trailing" {
		what = "llm answer has text after its JSON"
	}
	slog.Warn(what, "call", call.id, "job", jobID, "role", role, "model", call.model, "try", call.try,
		"kind", kind, "output_bytes", sh.OutputBytes, "json_end", sh.JSONEnd, "trailing_bytes", sh.TrailingBytes,
		"trailing_json", sh.TrailingJSON, "error", logMsg)
	if l == nil {
		return
	}
	r := l.base(ctx, event, call, 0)
	r.Kind, r.Error, r.OutputShape = kind, logMsg, &sh
	l.write(callsPrefix, r)
	out, omitted := excerpt(stored)
	l.write(rejectedPrefix, rejectedRecord{
		Time: r.Time, Call: call.id, JobID: jobID, Role: role, Model: call.model,
		Kind: kind, Error: msg, Output: out, OmittedBytes: omitted,
	})
}
