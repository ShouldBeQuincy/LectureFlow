// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// chatBody is an OpenAI-compatible answer with finish_reason finish and
// usage 100 / 20 / 64.
func chatBody(content, finish string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-r","choices":[{"message":{"content":%q},"finish_reason":%q}],`+
		`"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":64}}`, content, finish)
}

// DeepSeek's finish_reason "insufficient_system_resource" (the answer was
// cut short because the inference service ran out of resources) is a
// temporary server failure (N50b): retried with the backoff, the cut-off
// content never returned, the billed usage of every attempt counted.
func TestInsufficientResourceRetried(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			io.WriteString(w, chatBody(`{"idx": 0, "zh": "半`, "insufficient_system_resource"))
			return
		}
		io.WriteString(w, chatBody(`{"idx": 0, "zh": "好"}`, "stop"))
	})
	c, _, rec := newClient(t, srv.URL)
	u := &Usage{}
	stats := &sched.StatsRecorder{}
	ctx := sched.WithStatsSink(WithUsage(context.Background(), u), stats)
	res, err := c.Complete(ctx, config.RoleTranslate, "SYS", "USR", true)
	if err != nil || res.Text != `{"idx": 0, "zh": "好"}` {
		t.Fatalf("res %q err %v", res.Text, err)
	}
	if n := len(got()); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
	if len(rec.waits) != 1 {
		t.Errorf("backoff waits %v, want one", rec.waits)
	}
	if tot := u.Totals(); tot != (Totals{PromptTokens: 200, CompletionTokens: 40, CacheHitTokens: 128, Requests: 2}) {
		t.Errorf("usage %+v", tot)
	}
	if got := eventLines(stats.Events); got != "start(1) end(1):"+model.LLMOutcomeBadResponse+" backoff(1):2s start(2) end(2):"+model.LLMOutcomeOK {
		t.Errorf("events %s", got)
	}
	raw, recs := readCalls(t, c.cfg.DataDir)
	if events(recs) != "start end start end" {
		t.Fatalf("call log events %q:\n%s", events(recs), raw)
	}
	if e := recs[1]; e.Result != "insufficient_resource" || e.StopReason != "insufficient_system_resource" || e.Status != 200 {
		t.Errorf("end %+v", e)
	}
	if e := recs[3]; e.Result != "ok" || e.StopReason != "stop" {
		t.Errorf("second end %+v", e)
	}
}

// Every attempt cut short: after the retries a temporary error (not
// permanent, not a refusal), coded llm.overloaded.
func TestInsufficientResourceExhausted(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
		io.WriteString(w, chatBody("半", "insufficient_system_resource"))
	})
	c, _, rec := newClient(t, srv.URL)
	u := &Usage{}
	_, err := c.Complete(WithUsage(context.Background(), u), config.RoleSummary, "SYS", "USR", false)
	var ce *model.CodedError
	if err == nil || sched.IsPermanent(err) || errors.Is(err, ErrRefusal) || errors.Is(err, ErrTruncated) ||
		!errors.As(err, &ce) || ce.Info.Code != CodeOverloaded {
		t.Fatalf("err %v", err)
	}
	want := len(c.Backoff) + 1
	if n := len(got()); n != want {
		t.Errorf("%d requests, want %d", n, want)
	}
	if len(rec.waits) != len(c.Backoff) {
		t.Errorf("waits %v", rec.waits)
	}
	if tot := u.Totals(); tot.Requests != int64(want) || tot.PromptTokens != 100*int64(want) {
		t.Errorf("usage %+v", tot)
	}
}

// The other finish_reason values keep their handling.
func TestFinishReasonsUnchanged(t *testing.T) {
	for finish, check := range map[string]func(error) bool{
		"stop":           func(err error) bool { return err == nil },
		"length":         func(err error) bool { return errors.Is(err, ErrTruncated) && sched.IsPermanent(err) },
		"content_filter": func(err error) bool { return errors.Is(err, ErrRefusal) && sched.IsPermanent(err) },
	} {
		srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, chatBody("x", finish)) })
		c, _, _ := newClient(t, srv.URL)
		_, err := c.Complete(context.Background(), config.RoleSummary, "SYS", "USR", false)
		if !check(err) {
			t.Errorf("%s: err %v", finish, err)
		}
		if n := len(got()); n != 1 {
			t.Errorf("%s: %d requests", finish, n)
		}
	}
}
