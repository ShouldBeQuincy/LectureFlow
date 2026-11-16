// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Streamed requests (v3.2.x P1-c; design note 3.2, the user's Q7a).
//
// Requests of an Anthropic provider to a current Claude model
// (anthropicAdaptive) carry "stream": true and read the answer as
// server-sent events: message_start (id, input-side usage), per content
// block content_block_start / content_block_delta (thinking_delta,
// text_delta, signature_delta, …) / content_block_stop, message_delta
// (stop_reason, stop_details, output_tokens and any updated input-side
// usage), message_stop; ping events anywhere; an error event (HTTP status
// still 200) when the server fails mid-stream. Only the "type" of an event
// is read first; the known types are then decoded each into its own shape,
// so unknown event types, unknown fields and new fields of another type are
// ignored (SEC4 M2); a field the answer is read from that has another type
// than the API's fails the stream instead of losing content (SEC5 N2). The
// Result is what parseAnthropic gives for the same message: the text blocks
// concatenated (thinking skipped), usage mapped by the same rule,
// stop_reason max_tokens / refusal as failTruncated / failRefusal — also
// when the stream breaks after the stop reason came (SEC4 L9).
//
// Nothing of an event's data reaches an error, the call log or the service
// log: an unreadable event is described by its (cleaned) name, its size and
// the kind of decoding error; of an error event only error.type and its
// (cut, redacted) message are kept (SEC4 M3). A failed attempt after
// message_start carries the usage the stream reported so far (SEC4 M1); the
// output side is what message_start / message_delta said, never estimated.
//
// Older Claude models and non-Claude names on an Anthropic endpoint (whose
// streaming support is not known) and every OpenAI-compatible provider stay
// unstreamed: their requests are byte for byte what they were. A server that
// answers a streamed request with a plain JSON body (a proxy that ignores
// "stream") is read the old way.
//
// Idle timeout (GeneralSettings.LLMIdleTimeout, 0 = none): from the moment
// the request is sent, every SSE event — ping and comment lines included —
// and the arrival of the response headers restart it; when it runs out the
// attempt is aborted and counts as a transport failure (failIdle, call log
// result and request outcome "idle"), retried with the usual backoff. A set
// idle timeout is at least MinIdleTimeout (SEC4 L10). The attempt timeout
// (attemptTimeout, at most MaxAttemptTimeout) stays as the cap of the whole
// attempt; when it runs out while the stream is still delivering events (the
// last one within stillStreamingWindow of the end: one idle timeout, at most
// a quarter of the attempt timeout; SEC5 N1), the answer is longer than one
// attempt may take: failTimeLimit, permanent, not sent again (SEC4 M4). A
// stream that stalled before that window is an ordinary timeout, retried.
// Cancellation ends the read at once.
//
// Events of the call log end record: first_token_ms (the first thinking or
// text delta), first_text_ms, last_event_ms; the scheduler gets a
// RequestPhase event (thinking / writing) when the first delta of each kind
// arrives.

// maxResponseBytes caps the bytes read from one response, streamed or not.
// A stream that goes past it fails for good (SEC4 L3).
const maxResponseBytes = 32 << 20

// maxStreamLine caps one line of a stream and the data of one event (SEC4
// L2); a longer one fails the attempt for good.
const maxStreamLine = 1 << 20

// maxEventName: event names longer than this are kept cut (they match no
// known event); cleanEventName cuts names for messages further.
const maxEventName = 64

// MinIdleTimeout is the shortest idle timeout a streamed request gets: a
// setting between 0 (off) and this is raised to it (SEC4 L10), since a few
// seconds would cut most answers before their first event. The settings
// check uses the same floor (settings.MinLLMIdleTimeoutS; SEC5 N4), so this
// is the backstop.
var MinIdleTimeout = time.Duration(settings.MinLLMIdleTimeoutS) * time.Second

// streamActivityEvery: how often at most a streamed attempt reports that
// events keep arriving (sched.RequestActivity, the job's last activity;
// SEC4 L7).
var streamActivityEvery = 15 * time.Second

// streamed reports whether requests to tg are streamed.
func streamed(tg target) bool {
	return tg.ptype == model.ProviderAnthropic && anthropicAdaptive(tg.model)
}

// Streams reports whether requests of a provider of type providerType
// (model.Provider*) to modelName are streamed (streamed), so that their
// attempts can call the hook of WithFirstEventHook.
func Streams(providerType, modelName string) bool {
	return streamed(target{ptype: providerType, model: modelName})
}

// errIdle is the cancel cause of an attempt aborted by the idle timeout.
var errIdle = errors.New("no event within the idle timeout")

// idleTimer is the timer of the idle timeout (*time.Timer of
// time.AfterFunc; tests inject their own through HTTPClient.newIdleTimer).
type idleTimer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

func afterFunc(d time.Duration, f func()) idleTimer { return time.AfterFunc(d, f) }

// idleTimeout returns the idle timeout of streamed requests: the general
// setting, its default in legacy mode (whose providers are never streamed);
// 0 = none, else at least MinIdleTimeout.
func (c *HTTPClient) idleTimeout() time.Duration {
	d := model.GeneralSettings{}.LLMIdleTimeout()
	if c.set != nil {
		d = c.set.General().LLMIdleTimeout()
	}
	if d > 0 && d < MinIdleTimeout {
		d = MinIdleTimeout
	}
	return d
}

// isEventStream reports a text/event-stream response.
func isEventStream(h http.Header) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Get("Content-Type"))), "text/event-stream")
}

// touchReader calls touch whenever a read returns data (the idle timeout of
// a streamed request answered with a plain body).
type touchReader struct {
	r     io.Reader
	touch func()
}

func (t *touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.touch()
	}
	return n, err
}

// streamOutcome is what a streamed answer gave.
type streamOutcome struct {
	res  Result
	aerr *attemptErr // nil = ok; else the answer's own failure (error event, truncation, refusal, malformed or incomplete stream)
	meta respMeta    // for the call log, as answerMeta reads a whole message
	// bytes read from the stream
	bytes int
	// errorType: error.type of an error event
	errorType string
	// since the request was sent; -1 = did not happen
	firstToken, firstText, lastEvent time.Duration
	// lastContent: the last event other than ping (or a comment line);
	// maxGap / maxContentGap: the longest time between two consecutive
	// events of any kind / two consecutive non-ping events; -1 = fewer than
	// two such events (SEC6 O1: observation for the real acceptance run).
	lastContent, maxGap, maxContentGap time.Duration
	// readStart: when reading the stream began (since the request was sent,
	// about headers_ms). firstEvent / firstContent (R17 S7): the time from
	// readStart to the first event, pings and comment lines included / to
	// the first event other than ping; -1 = none arrived. The wait a
	// buffering gateway or proxy causes before message_start shows here and
	// in neither max gap.
	readStart, firstEvent, firstContent time.Duration
	// started: message_start arrived. usage: the usage the stream reported
	// so far (tokens only), valid when hasUsage (SEC4 M1).
	started  bool
	usage    Result
	hasUsage bool
}

// failWith returns e carrying the usage the stream reported (SEC4 M1): a
// failed attempt after message_start was billed for its input at least.
func (o *streamOutcome) failWith(e *attemptErr) *attemptErr {
	if o != nil && o.hasUsage && e.kind != failTruncated && e.kind != failRefusal {
		e.res, e.usage = o.usage, true
	}
	return e
}

// streamHooks are what readAnthropicStream reports while it reads.
type streamHooks struct {
	since func() time.Duration // time since the request was sent
	touch func()               // an event arrived (idle timeout); nil = none
	alive func()               // an event arrived (the job's last activity; throttled by the caller); nil = none
	phase func(string)         // the first delta of model.LLMPhase* arrived; nil = none
	start func()               // the first message_start arrived (WithFirstEventHook); nil = none
	keep  *bytes.Buffer        // the raw stream, for the debug log; nil = not kept
}

// countingReader counts the bytes read.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// errLineTooLong: a line of the stream is longer than maxStreamLine.
var errLineTooLong = errors.New("line too long")

// readLine reads one line ("\n" included) of at most limit bytes; a longer
// one is errLineTooLong. At the end of the input it returns what is left
// with io.EOF, like ReadBytes.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(line)+len(frag) > limit {
			return nil, errLineTooLong
		}
		line = append(line, frag...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

// readAnthropicStream reads a text/event-stream answer of the Messages API.
// err is a read error (the transport failed: network, timeout, idle,
// cancel); out holds what arrived until then. A stream that ends cleanly but
// incomplete is out.aerr (failEnvelope).
func readAnthropicStream(body io.Reader, key string, h streamHooks) (out streamOutcome, err error) {
	// One byte more than the cap tells a stream that is too long from one
	// that ends right at it (SEC4 L3).
	cr := &countingReader{r: io.LimitReader(body, maxResponseBytes+1)}
	var r io.Reader = cr
	if h.keep != nil {
		r = io.TeeReader(cr, h.keep)
	}
	st := &anthropicStream{key: key, out: &out, h: h}
	out.firstToken, out.firstText, out.lastEvent = -1, -1, -1
	out.lastContent, out.maxGap, out.maxContentGap = -1, -1, -1
	out.readStart, out.firstEvent, out.firstContent = h.since(), -1, -1
	defer func() { out.bytes = cr.n }()

	tooLong := func(what string, n int) (streamOutcome, error) {
		st.fail = &attemptErr{kind: failEnvelope, permanent: true,
			err: fmt.Errorf("%s is longer than %d bytes", what, n)}
		st.finish()
		return out, nil
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var name string
	var data []byte
	hasData := false
	for {
		line, rerr := readLine(br, maxStreamLine)
		if rerr == errLineTooLong {
			return tooLong("a line of the stream", maxStreamLine)
		}
		if rerr == nil {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if hasData || name != "" {
					if st.event(name, data) {
						st.finish()
						return out, nil
					}
				}
				name, data, hasData = "", data[:0], false
			case line[0] == ':': // a comment: the server is alive
				st.activity()
			default:
				field, value, _ := bytes.Cut(line, []byte(":"))
				value = bytes.TrimPrefix(value, []byte(" "))
				switch string(field) {
				case "event":
					name = string(value[:min(len(value), maxEventName+1)])
				case "data":
					if len(data)+len(value)+1 > maxStreamLine {
						return tooLong("the data of a stream event", maxStreamLine)
					}
					if hasData {
						data = append(data, '\n')
					}
					data = append(data, value...)
					hasData = true
				}
			}
			continue
		}
		if rerr == io.EOF { // an unterminated last event is dropped (SSE)
			if cr.n > maxResponseBytes {
				return tooLong("the stream", maxResponseBytes)
			}
			st.finish()
			return out, nil
		}
		st.finish()
		return out, rerr
	}
}

// anthropicStream accumulates the events of one streamed message.
type anthropicStream struct {
	key string
	out *streamOutcome
	h   streamHooks

	started, stopped bool
	id               string
	usage            map[string]json.RawMessage
	stopReason       string
	category         string
	text             strings.Builder
	blocks           map[string]int
	textChars        int
	thinkingChars    int
	fail             *attemptErr
}

// The shapes of the known events: only the fields this client reads, each a
// json.RawMessage. The fields the answer is read from — a content block and
// its type, a delta and its type, the text, the stop reason — fail the
// stream when they have another type than the API's (SEC5 N2: content would
// be lost silently); absent or null they are skipped as before. The other
// fields (id, usage, stop_details, …) are read leniently (rawString), so a
// value of an unexpected type there is ignored, not fatal.
type (
	evMessageStart struct {
		Message json.RawMessage `json:"message"`
	}
	evMessage struct {
		ID    json.RawMessage `json:"id"`
		Usage json.RawMessage `json:"usage"`
	}
	evBlockStart struct {
		ContentBlock json.RawMessage `json:"content_block"`
	}
	evDelta struct {
		Delta json.RawMessage `json:"delta"`
		Usage json.RawMessage `json:"usage"`
	}
	// evContent is a content block (content_block_start) or the delta of
	// one (content_block_delta): text / thinking by type.
	evContent struct {
		Type     json.RawMessage `json:"type"`
		Text     json.RawMessage `json:"text"`
		Thinking json.RawMessage `json:"thinking"`
	}
	evMessageDelta struct {
		StopReason  json.RawMessage `json:"stop_reason"`
		StopDetails json.RawMessage `json:"stop_details"`
	}
	evStopDetails struct {
		Category json.RawMessage `json:"category"`
	}
	evError struct {
		Error json.RawMessage `json:"error"`
	}
	evErrorBody struct {
		Type    json.RawMessage `json:"type"`
		Message json.RawMessage `json:"message"`
	}
)

// rawString returns raw as a string ("" when it is absent, null or not a
// string).
func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// stringField returns a string field the answer is read from (its text, a
// block or delta type, the stop reason): "" when absent or null; ok false
// when it is there but not a string (the answer would be wrong).
func stringField(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// object decodes raw into v when raw is a JSON object; false otherwise
// (absent, null, another type).
func object(raw json.RawMessage, v any) bool {
	return len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, v) == nil
}

// objectField decodes an object field the answer is read from (a content
// block, a delta) into v: decoded when it is a JSON object; absent or null
// decodes nothing and is ok; ok false when it is there but not an object.
func objectField(raw json.RawMessage, v any) (decoded, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, true
	}
	if !object(raw, v) {
		return false, false
	}
	return true, true
}

// knownEvents are the event types this client acts on.
var knownEvents = map[string]bool{
	"message_start": true, "content_block_start": true, "content_block_delta": true, "content_block_stop": true,
	"message_delta": true, "message_stop": true, "error": true, "ping": true,
}

// cleanEventName is an event name for a message or log: letters, digits,
// "_", ".", "-" kept, anything else "?", at most 40 characters (SEC4 L2).
func cleanEventName(name string) string {
	if name == "" {
		return "(unnamed)"
	}
	const n = 40
	var b strings.Builder
	for i := 0; i < len(name) && i < n; i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('?')
		}
	}
	if len(name) > n {
		b.WriteString("…")
	}
	return b.String()
}

// decodeErrorKind names the kind of a JSON decoding error, without any of
// the data it quotes.
func decodeErrorKind(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return "syntax error"
	case errors.As(err, &te):
		return "unexpected type"
	}
	return "unreadable"
}

// unreadable fails the stream for an event that cannot be read: its name,
// the size of its data and the kind of error, nothing of the data (SEC4 M3).
func (s *anthropicStream) unreadable(name string, size int, kind string) bool {
	ev := cleanEventName(name)
	s.fail = &attemptErr{kind: failEnvelope, body: fmt.Sprintf("event %s, %d bytes of data, %s", ev, size, kind),
		err: fmt.Errorf("unreadable stream event %s (%s)", ev, kind)}
	return true
}

// wrongType fails the stream for a known event whose field the answer is
// read from has another type than the API's (SEC5 N2): a gateway rewrote or
// damaged it, and skipping it would lose part of the answer or its stop
// reason silently. field is this client's name of it, never the data. New
// events and new fields stay ignored (SEC4 M2).
func (s *anthropicStream) wrongType(name string, data []byte, field string) bool {
	return s.unreadable(name, len(data), "unexpected type of "+field)
}

// streamErrorStatus maps error.type of an error event to the HTTP status
// the same error has without streaming (Anthropic's errors), so it is
// retried, or not, and reported as it would be (SEC4 L1). Unknown types
// count as a server error.
func streamErrorStatus(typ string) int {
	switch typ {
	case "invalid_request_error":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "billing_error":
		return http.StatusPaymentRequired
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "request_too_large":
		return http.StatusRequestEntityTooLarge
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "timeout_error":
		return http.StatusGatewayTimeout
	case "overloaded_error":
		return StatusOverloaded
	}
	return http.StatusInternalServerError // api_error and anything else
}

func (s *anthropicStream) activity() {
	now := s.h.since()
	if s.out.lastEvent >= 0 {
		s.out.maxGap = max(s.out.maxGap, now-s.out.lastEvent)
	} else {
		s.out.firstEvent = now - s.out.readStart
	}
	s.out.lastEvent = now
	if s.h.touch != nil {
		s.h.touch()
	}
	if s.h.alive != nil {
		s.h.alive()
	}
}

// content notes that the event activity() just saw is not a ping: it
// counts for maxContentGap.
func (s *anthropicStream) content() {
	now := s.out.lastEvent
	if s.out.lastContent >= 0 {
		s.out.maxContentGap = max(s.out.maxContentGap, now-s.out.lastContent)
	} else {
		s.out.firstContent = now - s.out.readStart
	}
	s.out.lastContent = now
}

// event handles one SSE event; true ends the stream (message_stop, an
// error event, an unreadable event).
func (s *anthropicStream) event(name string, data []byte) bool {
	s.activity()
	if name != "" && name != "ping" {
		s.content() // (a nameless event counts once its type is read)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return false // an event without data (none is defined): ignored
	}
	// The type first; the data of an unknown event is not looked at further.
	var head struct {
		Type json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		if name != "" && !knownEvents[name] {
			return false // an unknown event whose data is not a JSON object
		}
		return s.unreadable(name, len(data), decodeErrorKind(err))
	}
	typ := rawString(head.Type)
	if typ == "" {
		typ = name
	}
	if name == "" && typ != "ping" {
		s.content()
	}
	switch typ {
	case "message_start":
		if !s.started && s.h.start != nil {
			s.h.start() // the answer began: the prompt was read (WithFirstEventHook)
		}
		s.started = true
		var ev evMessageStart
		var m evMessage
		if json.Unmarshal(data, &ev) == nil && object(ev.Message, &m) {
			s.id = rawString(m.ID)
			s.mergeUsage(m.Usage)
		}
	case "content_block_start":
		var ev evBlockStart
		var b evContent
		if json.Unmarshal(data, &ev) != nil {
			return false
		}
		decoded, ok := objectField(ev.ContentBlock, &b)
		if !ok {
			return s.wrongType(typ, data, "content_block")
		}
		if !decoded {
			return false // no block: nothing to read
		}
		bt, ok := stringField(b.Type)
		if !ok {
			return s.wrongType(typ, data, "content_block.type")
		}
		s.blocks = countBlock(s.blocks, bt) // untrusted (SEC4 L6)
		switch bt {
		case "text":
			text, ok := stringField(b.Text)
			if !ok {
				return s.wrongType(typ, data, "content_block.text")
			}
			s.text.WriteString(text)
			s.textChars += utf8.RuneCountInString(text)
		case "thinking":
			s.thinkingChars += utf8.RuneCountInString(rawString(b.Thinking))
		}
	case "content_block_delta":
		var ev evDelta
		var d evContent
		if json.Unmarshal(data, &ev) != nil {
			return false
		}
		decoded, ok := objectField(ev.Delta, &d)
		if !ok {
			return s.wrongType(typ, data, "delta")
		}
		if !decoded {
			return false // no delta: nothing to read
		}
		dt, ok := stringField(d.Type)
		if !ok {
			return s.wrongType(typ, data, "delta.type")
		}
		switch dt { // other (new) kinds of delta are ignored (SEC4 M2)
		case "text_delta":
			text, ok := stringField(d.Text)
			if !ok {
				return s.wrongType(typ, data, "delta.text")
			}
			s.text.WriteString(text)
			s.textChars += utf8.RuneCountInString(text)
			if s.out.firstText < 0 {
				s.out.firstText = s.h.since()
				s.firstToken()
				if s.h.phase != nil {
					s.h.phase(model.LLMPhaseWriting)
				}
			}
		case "thinking_delta":
			s.thinkingChars += utf8.RuneCountInString(rawString(d.Thinking))
			if s.firstToken() && s.h.phase != nil {
				s.h.phase(model.LLMPhaseThinking)
			}
		}
	case "message_delta":
		var ev evDelta
		if json.Unmarshal(data, &ev) != nil {
			return false
		}
		// The usage first: a failure below still counts it (SEC4 M1).
		s.mergeUsage(ev.Usage)
		var d evMessageDelta
		decoded, ok := objectField(ev.Delta, &d)
		if !ok {
			return s.wrongType(typ, data, "delta")
		}
		if decoded {
			sr, ok := stringField(d.StopReason)
			if !ok {
				return s.wrongType(typ, data, "delta.stop_reason")
			}
			if sr != "" {
				s.stopReason = sr
			}
			var sd evStopDetails
			if object(d.StopDetails, &sd) {
				s.category = CleanRefusalCategory(rawString(sd.Category)) // untrusted (SEC1 L1)
			}
		}
	case "message_stop":
		s.stopped = true
		return true
	case "error":
		// error is an object {type, message}; a gateway may send a string.
		var ev evError
		_ = json.Unmarshal(data, &ev)
		var eb evErrorBody
		et, msg := "", ""
		if object(ev.Error, &eb) {
			et, msg = rawString(eb.Type), rawString(eb.Message)
		} else {
			msg = rawString(ev.Error)
		}
		s.out.errorType = CleanRefusalCategory(et) // an identifier like a category: kept only when short and plain
		what := s.out.errorType
		if what == "" {
			what = "error"
		}
		if msg != "" {
			what += ": " + msg // the server's error text, not the answer
		}
		s.fail = &attemptErr{kind: failStatus, status: streamErrorStatus(et), inStream: true, body: redactSnippet([]byte(what), s.key),
			err: fmt.Errorf("error event in the stream: %s", s.out.errorType)}
		return true
	}
	// ping, content_block_stop and unknown types: activity only
	return false
}

// firstToken records the first thinking or text delta; true when this was it
// (thinking: the phase is reported once, however the deltas interleave).
func (s *anthropicStream) firstToken() bool {
	if s.out.firstToken >= 0 {
		return false
	}
	s.out.firstToken = s.h.since()
	return true
}

// mergeUsage lays the fields of raw over the usage so far: message_start
// brings the input side, message_delta the (cumulative) output tokens and
// possibly updated input-side counts.
func (s *anthropicStream) mergeUsage(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if s.usage == nil {
		s.usage = map[string]json.RawMessage{}
	}
	for k, v := range m {
		if string(v) != "null" {
			s.usage[k] = v
		}
	}
}

// usageCount reads a token count of the usage (0 when absent or not a number).
func usageCount(raw json.RawMessage) int64 {
	var n int64
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil {
		return 0
	}
	return n
}

// finish sets the outcome from what arrived.
func (s *anthropicStream) finish() {
	o := s.out
	o.started = s.started
	o.meta = respMeta{id: s.id, stopReason: s.stopReason, stopCategory: s.category,
		blocks: s.blocks, textChars: s.textChars, thinkingChars: s.thinkingChars}
	if s.usage != nil {
		usage, _ := json.Marshal(s.usage) // keys sorted
		o.meta.usage = usageOf(usage)
		o.meta.reasoningTokens = anthropicThinkingTokens(usage)
		read := s.usage["cache_read_input_tokens"]
		o.usage = Result{
			PromptTokens:     usageCount(s.usage["input_tokens"]) + usageCount(read) + usageCount(s.usage["cache_creation_input_tokens"]),
			CacheHitTokens:   usageCount(read),
			CompletionTokens: usageCount(s.usage["output_tokens"]),
		}
		o.hasUsage = s.started
	}
	res := o.usage
	res.Text = s.text.String()
	switch {
	case s.fail != nil:
		o.aerr = s.fail
	case !s.started:
		o.aerr = &attemptErr{kind: failEnvelope, err: errors.New("the stream has no message_start")}
	case s.stopReason == stopMaxTokens: // with or without message_stop (SEC4 L9)
		o.aerr = &attemptErr{kind: failTruncated, res: res, err: errors.New("stop_reason max_tokens")}
	case s.stopReason == stopRefusal:
		o.aerr = &attemptErr{kind: failRefusal, res: res, err: errors.New("stop_reason refusal"), category: s.category}
	case !s.stopped:
		o.aerr = &attemptErr{kind: failEnvelope, err: errors.New("the stream ended before message_stop")}
	default:
		o.res = res
	}
	if o.aerr != nil {
		o.aerr = o.failWith(o.aerr)
	}
	// The answer is known (whole, truncated or declined) only then; a failed
	// stream is not an empty answer (SEC4 L4).
	o.meta.parsed = o.aerr == nil || o.aerr.kind == failTruncated || o.aerr.kind == failRefusal
}

// stillStreaming reports whether a streamed attempt that ran out of its
// attempt timeout was still receiving events when it ended (SEC4 M4): only
// a stream whose events kept arriving until shortly before the cap counts —
// message_start came and the last event is less than stillStreamingWindow
// before the end. A stream that stalled earlier is an ordinary timeout,
// retried with the usual backoff.
func stillStreaming(call callInfo, out attemptOutcome) bool {
	so := out.stream
	if so == nil || !so.started || so.lastEvent < 0 {
		return false
	}
	return out.duration-so.lastEvent < stillStreamingWindow(call)
}

// stillStreamingWindow is how close to the end of a timed-out attempt its
// last event must be for the stream to count as still delivering: one idle
// timeout (its default when it is off), and at most a quarter of the attempt
// timeout (SEC5 N1). Without the second bound a short request (an attempt
// timeout of 120 s against the default idle timeout of 90 s) whose stream
// stalls right after message_start would always look still delivering when
// its timeout runs out, and fail for good instead of being retried. A zero
// attempt timeout gives no window: never still delivering.
func stillStreamingWindow(call callInfo) time.Duration {
	window := call.idle
	if window <= 0 {
		window = time.Duration(model.DefaultLLMIdleTimeoutS) * time.Second
	}
	return min(window, call.timeout/4)
}
