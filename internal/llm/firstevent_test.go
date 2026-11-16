// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// WithFirstEventHook (v3.2.x r2 N46): the hook of the prompt-cache warm-up
// of repair on Anthropic.

// countHook returns a context whose first-event hook counts its calls.
func countHook(ctx context.Context) (context.Context, *atomic.Int32) {
	var n atomic.Int32
	return WithFirstEventHook(ctx, func() { n.Add(1) }), &n
}

// The hook fires on message_start, while the rest of the answer is still to
// come (the server holds the stream until it did), once per attempt: the
// pings after message_start do not call it again.
func TestFirstEventHookStreamed(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamPings)
	ctx, n := countHook(context.Background())
	var during atomic.Int32
	during.Store(-1)
	e.s.SetStreamHook(func(role, event string) {
		if event == "message_start" {
			// Nothing after message_start has been sent yet.
			waitUntil(t, "the hook", func() bool { return n.Load() >= 1 })
			during.Store(n.Load())
		}
	})
	res, err := e.c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
	if err != nil || res.Text != summaryFixture(t) {
		t.Fatalf("res %+v err %v", res, err)
	}
	if during.Load() != 1 || n.Load() != 1 {
		t.Fatalf("hook calls: %d while the stream was held after message_start, %d in all; want 1 and 1", during.Load(), n.Load())
	}
	// No hook in ctx: the stream is read as before.
	if _, err := e.c.Complete(context.Background(), model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
}

// Comment lines and pings ahead of message_start do not call the hook; a
// second message_start in the same stream does not call it again.
func TestFirstEventHookOnMessageStartOnly(t *testing.T) {
	body := ": keep-alive\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var seq strings.Builder
	h := streamHooks{since: func() time.Duration { return 0 },
		touch: func() { seq.WriteString("e") }, start: func() { seq.WriteString("S") }}
	out, err := readAnthropicStream(strings.NewReader(body), "", h)
	if err != nil || out.aerr != nil || out.res.Text != "hi" {
		t.Fatalf("err %v aerr %v res %+v", err, out.aerr, out.res)
	}
	// touch: comment, ping, message_start, then the hook, then the six
	// events after it (the second message_start included)
	if got := seq.String(); got != "eeeSeeeeee" {
		t.Fatalf("events (e) and hook calls (S): %s", got)
	}
	// A stream without message_start never calls it.
	calls := 0
	h.start = func() { calls++ }
	if _, err := readAnthropicStream(strings.NewReader(": x\n\nevent: ping\ndata: {\"type\":\"ping\"}\n\n"), "", h); err != nil || calls != 0 {
		t.Fatalf("no message_start: err %v, %d calls", err, calls)
	}
}

// Requests that are not streamed never call the hook: an older Claude model
// on an Anthropic provider, an OpenAI-compatible provider.
func TestFirstEventHookNotStreamed(t *testing.T) {
	e := newStreamEnv(t, "claude-haiku-4-5", model.RoleParams{})
	ctx, n := countHook(context.Background())
	if _, err := e.c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	if r := e.s.Requests(llmtest.RoleSummary); len(r) != 1 || r[0].Stream || n.Load() != 0 {
		t.Fatalf("older Claude model: %d requests, %d hook calls", len(r), n.Load())
	}

	fake := llmtest.New(t)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	fake.Configure(cfg)
	c := New(cfg, nil, "", nil)
	ctx, n = countHook(context.Background())
	if _, err := c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 0 {
		t.Fatalf("OpenAI-compatible: %d hook calls", n.Load())
	}
}

// Failed attempts: an error status or a stream whose headers never come does
// not call the hook; the retry that streams calls it. An error event after
// message_start does not undo the call, and its retry calls it again.
func TestFirstEventHookRetries(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.FailNext(llmtest.RoleSummary, 1, llmtest.FailHTTP529)
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHangHeaders)
	e.s.SetStreamHook(func(role, event string) {
		if event == "hang" {
			e.vt.advance(time.Duration(model.DefaultLLMIdleTimeoutS) * time.Second)
		}
	})
	rec := &sched.StatsRecorder{}
	ctx, n := countHook(sched.WithStatsSink(context.Background(), rec))
	if _, err := e.c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	if got := streamEventLines(rec.Events); !strings.HasPrefix(got, "start(1) end(1):http_error/529 backoff(1):2s start(2) end(2):idle backoff(2):4s start(3)") || n.Load() != 1 {
		t.Fatalf("events %s; %d hook calls, want 1 (the third attempt)", got, n.Load())
	}

	e.s.SetStreamHook(nil)
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamOverloaded)
	ctx, n = countHook(context.Background())
	if _, err := e.c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("error event after message_start, then a retry: %d hook calls, want 2", n.Load())
	}
}

// Cancelled before the answer began: the hook is not called and the request
// returns at once.
func TestFirstEventHookCanceled(t *testing.T) {
	e := newStreamEnv(t, "claude-opus-5-5", model.RoleParams{})
	e.s.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHangHeaders)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.s.SetStreamHook(func(role, event string) {
		if event == "hang" {
			cancel()
		}
	})
	ctx, n := countHook(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := e.c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "t", false)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || n.Load() != 0 {
			t.Fatalf("err %v, %d hook calls", err, n.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Complete did not return after the cancel")
	}
}
