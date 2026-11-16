// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/model"
)

// SEC6 O1: a streamed attempt measures the longest time between two
// consecutive events (pings and comment lines included) and between two
// consecutive events other than ping, a ping without an event name
// included.
func TestStreamMaxGaps(t *testing.T) {
	// Each piece arrives at its time (ms since the request was sent).
	pieces := []timedPiece{
		{0, sec4Start},
		{100, "event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		{400, "event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		{1000, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"},
		{1100, ": keep-alive\n"},
		{1150, "data: {\"type\":\"ping\"}\n\n"}, // no event name
		{1300, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n"},
		{1350, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"},
		{1360, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"},
		{1370, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	}
	r := &timedReader{pieces: pieces}
	out, err := readAnthropicStream(r, testKey, streamHooks{since: func() time.Duration { return r.now }})
	if err != nil || out.aerr != nil || out.res.Text != "Hi" {
		t.Fatalf("err %v %+v res %+v", err, out.aerr, out.res)
	}
	// all: 100 300 600 100 50 150 50 10 10; content: 0 → 1000 → 1300 → …
	if out.maxGap != 600*time.Millisecond || out.maxContentGap != 1000*time.Millisecond || out.lastEvent != 1370*time.Millisecond {
		t.Errorf("max gap %v, max content gap %v, last %v", out.maxGap, out.maxContentGap, out.lastEvent)
	}

	// One event: no gap of either kind.
	r = &timedReader{pieces: []timedPiece{{5, sec4Start}}}
	out, err = readAnthropicStream(r, testKey, streamHooks{since: func() time.Duration { return r.now }})
	if err != nil || out.maxGap != -1 || out.maxContentGap != -1 {
		t.Errorf("one event: err %v, gaps %v %v", err, out.maxGap, out.maxContentGap)
	}
}

// R17 S7: a streamed attempt measures the time from when reading the
// stream began to the first event (a ping or a comment line included) and
// to the first event other than ping; neither when no such event arrived.
func TestStreamFirstEvent(t *testing.T) {
	ping := "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	// The headers arrived at 200 ms: reading the stream begins there.
	r := &timedReader{now: 200 * time.Millisecond, pieces: []timedPiece{
		{900, ping},
		{1000, ": keep-alive\n"},
		{1100, "data: {\"type\":\"ping\"}\n\n"}, // no event name
		{1500, sec4Start},
		{1600, sec4Text("Hi")},
		{1700, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"},
		{1800, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	}}
	out, err := readAnthropicStream(r, testKey, streamHooks{since: func() time.Duration { return r.now }})
	if err != nil || out.aerr != nil || out.res.Text != "Hi" {
		t.Fatalf("err %v %+v res %+v", err, out.aerr, out.res)
	}
	if out.firstEvent != 700*time.Millisecond || out.firstContent != 1300*time.Millisecond {
		t.Errorf("first event %v, first content %v", out.firstEvent, out.firstContent)
	}

	// A comment line is the first sign of life; the first event is
	// message_start itself.
	r = &timedReader{now: 50 * time.Millisecond, pieces: []timedPiece{{80, ": hello\n"}, {300, sec4Start}}}
	out, _ = readAnthropicStream(r, testKey, streamHooks{since: func() time.Duration { return r.now }})
	if out.firstEvent != 30*time.Millisecond || out.firstContent != 250*time.Millisecond {
		t.Errorf("comment first: first event %v, first content %v", out.firstEvent, out.firstContent)
	}

	// Pings only: a first event, no first content.
	r = &timedReader{pieces: []timedPiece{{40, ping}, {90, ping}}}
	out, _ = readAnthropicStream(r, testKey, streamHooks{since: func() time.Duration { return r.now }})
	if out.firstEvent != 40*time.Millisecond || out.firstContent != -1 {
		t.Errorf("pings only: first event %v, first content %v", out.firstEvent, out.firstContent)
	}

	// Nothing arrived: neither.
	r = &timedReader{now: 10 * time.Millisecond}
	out, _ = readAnthropicStream(r, testKey, streamHooks{since: func() time.Duration { return r.now }})
	if out.firstEvent != -1 || out.firstContent != -1 {
		t.Errorf("empty: first event %v, first content %v", out.firstEvent, out.firstContent)
	}
	if rec := msOrNil(out.firstEvent); rec != nil {
		t.Errorf("empty: first_event_ms %v", *rec)
	}
}

// timedPiece is part of a stream and when it arrives, in ms.
type timedPiece struct {
	ms   int
	text string
}

// timedReader hands out one piece per Read and sets its clock (now) to the
// piece's time.
type timedReader struct {
	pieces []timedPiece
	now    time.Duration
}

func (r *timedReader) Read(p []byte) (int, error) {
	if len(r.pieces) == 0 {
		return 0, io.EOF
	}
	pc := r.pieces[0]
	if len(pc.text) > len(p) {
		panic("piece larger than the read buffer")
	}
	r.pieces = r.pieces[1:]
	r.now = time.Duration(pc.ms) * time.Millisecond
	return copy(p, pc.text), nil
}

// The end record of a streamed attempt carries max_gap_ms and
// max_content_gap_ms (numbers only); a request that is not streamed has
// neither.
func TestCallLogMaxGaps(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	if _, err := e.c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "the transcript", false); err != nil {
		t.Fatal(err)
	}
	raw, recs := readCalls(t, e.set.Config().DataDir)
	if events(recs) != "start end" {
		t.Fatalf("calls %s", raw)
	}
	end := recs[1]
	if !end.Stream || end.MaxGapMS == nil || end.MaxContentGapMS == nil || end.LastEventMS == nil ||
		*end.MaxGapMS < 0 || *end.MaxGapMS > *end.LastEventMS || *end.MaxContentGapMS < 0 || *end.MaxContentGapMS > *end.LastEventMS {
		t.Errorf("end %+v", end)
	}
	if !strings.Contains(raw, `"max_gap_ms":`) || !strings.Contains(raw, `"max_content_gap_ms":`) {
		t.Errorf("calls %s", raw)
	}
	// R17 S7: the first event and the first event other than ping.
	if end.FirstEventMS == nil || end.FirstContentMS == nil || *end.FirstEventMS < 0 ||
		*end.FirstContentMS < *end.FirstEventMS || *end.FirstContentMS > *end.LastEventMS {
		t.Errorf("first event: end %+v", end)
	}
	if !strings.Contains(raw, `"first_event_ms":`) || !strings.Contains(raw, `"first_content_ms":`) {
		t.Errorf("calls %s", raw)
	}

	// OpenAI-compatible: not streamed.
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	})
	c, _, _ := newClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), config.RoleTranslate, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	raw, recs = readCalls(t, c.cfg.DataDir)
	if events(recs) != "start end" || recs[1].Stream {
		t.Fatalf("calls %s", raw)
	}
	if strings.Contains(raw, "max_gap_ms") || strings.Contains(raw, "max_content_gap_ms") ||
		strings.Contains(raw, "first_event_ms") || strings.Contains(raw, "first_content_ms") {
		t.Errorf("a request that is not streamed has gap fields: %s", raw)
	}
}
