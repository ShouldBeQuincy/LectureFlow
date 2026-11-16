// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"
)

// Server-sent events of the Anthropic endpoint (v3.2.x P1-c).

// StreamMode is a scripted stream (StreamNext).
type StreamMode int

const (
	// StreamPings: the normal answer with StreamPingCount ping events after
	// message_start.
	StreamPings StreamMode = iota
	// StreamHang: message_start, then nothing until the client hangs up.
	StreamHang
	// StreamHangHeaders: no response headers until the client hangs up.
	StreamHangHeaders
	// StreamOverloaded: message_start, then an error event overloaded_error
	// (HTTP 200).
	StreamOverloaded
	// StreamRateLimited: message_start, then an error event rate_limit_error.
	StreamRateLimited
	// StreamDisconnect: message_start, part of a text block, then the
	// connection is closed.
	StreamDisconnect
	// StreamMaxTokensNoText: a thinking block (ThinkingSummary), no text,
	// stop_reason max_tokens with output_tokens = the request's max_tokens.
	StreamMaxTokensNoText
	// StreamNoStop: the normal answer without message_stop at its end.
	StreamNoStop
)

// StreamPingCount is how many pings StreamPings sends.
const StreamPingCount = 5

// ThinkingSummary is the thinking a streamed answer carries when the request
// asks for display "summarized" (and of StreamMaxTokensNoText).
const ThinkingSummary = "llmtest: weighing the terms. Checking the context. Writing the answer."

// StreamNext makes the next n streamed requests of role use mode (queued
// after modes queued earlier). Requests that are not streamed are not
// affected and do not use up the queue.
func (s *Server) StreamNext(role string, n int, mode StreamMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.streams[role] = append(s.streams[role], mode)
	}
}

// SetStreamHook calls fn(role, event) after every event of a streamed
// answer is sent and flushed (event: the SSE event name), and with event
// "hang" before a scripted hang. fn may block to pause the stream; nil
// removes it.
func (s *Server) SetStreamHook(fn func(role, event string)) {
	s.mu.Lock()
	s.streamHook = fn
	s.mu.Unlock()
}

// SSEEvent is one server-sent event.
type SSEEvent struct {
	Name string
	Data string // one line of JSON
}

// MessageSSE returns the events of the Messages API stream that delivers
// message (a whole /v1/messages answer), encoded as the API sends them.
func MessageSSE(message []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(message, &m); err != nil {
		return nil, err
	}
	return EncodeSSE(messageEvents(m, "")), nil
}

// EncodeSSE encodes events as text/event-stream.
func EncodeSSE(evs []SSEEvent) []byte {
	var b bytes.Buffer
	for _, ev := range evs {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", ev.Name, ev.Data)
	}
	return b.Bytes()
}

func sseEvent(name string, data map[string]any) SSEEvent {
	data["type"] = name
	b, _ := json.Marshal(data)
	return SSEEvent{Name: name, Data: string(b)}
}

// chunks splits s into up to n pieces at rune boundaries ("" = none).
func chunks(s string, n int) []string {
	if s == "" {
		return nil
	}
	size := max((utf8.RuneCountInString(s)+n-1)/n, 1)
	var out []string
	r := []rune(s)
	for i := 0; i < len(r); i += size {
		out = append(out, string(r[i:min(i+size, len(r))]))
	}
	return out
}

// messageEvents turns a whole message into its stream: message_start with
// the input side of the usage, a ping, every content block (start, deltas,
// stop; a thinking block gets summary as its thinking text when it has
// none, and a signature), message_delta with stop_reason, stop_details and
// output_tokens, message_stop.
func messageEvents(m map[string]any, summary string) []SSEEvent {
	usage, _ := m["usage"].(map[string]any)
	startUsage := map[string]any{"output_tokens": 1}
	for _, k := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		if v, ok := usage[k]; ok {
			startUsage[k] = v
		}
	}
	msg := map[string]any{"id": m["id"], "type": "message", "role": "assistant", "model": m["model"],
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": startUsage}
	evs := []SSEEvent{sseEvent("message_start", map[string]any{"message": msg}), sseEvent("ping", map[string]any{})}
	blocks, _ := m["content"].([]any)
	for i, raw := range blocks {
		b, _ := raw.(map[string]any)
		typ, _ := b["type"].(string)
		switch typ {
		case "thinking":
			text, _ := b["thinking"].(string)
			if text == "" {
				text = summary
			}
			evs = append(evs, sseEvent("content_block_start", map[string]any{"index": i,
				"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}}))
			for _, c := range chunks(text, 3) {
				evs = append(evs, sseEvent("content_block_delta", map[string]any{"index": i,
					"delta": map[string]any{"type": "thinking_delta", "thinking": c}}))
			}
			evs = append(evs, sseEvent("content_block_delta", map[string]any{"index": i,
				"delta": map[string]any{"type": "signature_delta", "signature": "llmtest-signature"}}))
		case "text":
			text, _ := b["text"].(string)
			evs = append(evs, sseEvent("content_block_start", map[string]any{"index": i,
				"content_block": map[string]any{"type": "text", "text": ""}}))
			for _, c := range chunks(text, 3) {
				evs = append(evs, sseEvent("content_block_delta", map[string]any{"index": i,
					"delta": map[string]any{"type": "text_delta", "text": c}}))
			}
		default:
			evs = append(evs, sseEvent("content_block_start", map[string]any{"index": i, "content_block": b}))
		}
		evs = append(evs, sseEvent("content_block_stop", map[string]any{"index": i}))
	}
	delta := map[string]any{"stop_reason": m["stop_reason"], "stop_sequence": nil}
	if sd, ok := m["stop_details"]; ok {
		delta["stop_details"] = sd
	}
	evs = append(evs, sseEvent("message_delta", map[string]any{"delta": delta,
		"usage": map[string]any{"output_tokens": usage["output_tokens"]}}))
	return append(evs, sseEvent("message_stop", map[string]any{}))
}

// writeStream answers a streamed request with the events of resp (the
// message the server would send whole), changed by a scripted mode.
func (s *Server) writeStream(w http.ResponseWriter, r *http.Request, req Request, resp map[string]any) {
	s.mu.Lock()
	var mode *StreamMode
	if q := s.streams[req.Role]; len(q) > 0 {
		m := q[0]
		mode = &m
		s.streams[req.Role] = q[1:]
	}
	hook := s.streamHook
	s.mu.Unlock()
	emit := func(event string) {
		if hook != nil {
			hook(req.Role, event)
		}
	}
	hang := func() {
		emit("hang")
		select {
		case <-r.Context().Done():
		case <-time.After(time.Minute):
		}
	}
	if mode != nil && *mode == StreamHangHeaders {
		hang()
		return
	}
	summary := ""
	if req.Display == "summarized" {
		summary = ThinkingSummary
	}
	if mode != nil && *mode == StreamMaxTokensNoText {
		resp["content"] = []any{map[string]any{"type": "thinking", "thinking": ThinkingSummary}}
		resp["stop_reason"] = "max_tokens"
		if u, ok := resp["usage"].(map[string]any); ok {
			u["output_tokens"] = int64(req.MaxTokens)
		}
	}
	evs := messageEvents(resp, summary)
	if mode != nil {
		switch *mode {
		case StreamPings:
			pings := make([]SSEEvent, StreamPingCount)
			for i := range pings {
				pings[i] = sseEvent("ping", map[string]any{})
			}
			evs = append(evs[:1], append(pings, evs[1:]...)...)
		case StreamHang:
			evs = evs[:1]
		case StreamOverloaded, StreamRateLimited:
			typ, msg := "overloaded_error", "Overloaded"
			if *mode == StreamRateLimited {
				typ, msg = "rate_limit_error", "Rate limited"
			}
			evs = append(evs[:1], sseEvent("error", map[string]any{"error": map[string]any{"type": typ, "message": msg}}))
		case StreamDisconnect:
			cut := len(evs)
			for i, ev := range evs {
				if ev.Name == "content_block_delta" && bytes.Contains([]byte(ev.Data), []byte(`"text_delta"`)) {
					cut = i + 1
					break
				}
			}
			evs = evs[:cut]
		case StreamNoStop:
			evs = evs[:len(evs)-1]
		}
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Request-Id", "req_llmtest_stream")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	for _, ev := range evs {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		emit(ev.Name)
	}
	if mode == nil {
		return
	}
	switch *mode {
	case StreamHang:
		hang()
	case StreamDisconnect:
		if conn, _, err := rc.Hijack(); err == nil {
			conn.Close()
		}
	}
}
