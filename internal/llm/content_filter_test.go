// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// finish_reason "content_filter" of an OpenAI-compatible provider is a
// refusal (N50): ErrRefusal, permanent, coded CodeRefusal without a
// category, after one request; the cut-off content is not an answer; the
// billed usage counts; call log and request events say "refused". (OpenAI-
// compatible requests are never streamed, see streamed: this is their only
// path.)
func TestContentFilterIsRefusal(t *testing.T) {
	for _, jsonMode := range []bool{true, false} {
		srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
			io.WriteString(w, `{"id":"chatcmpl-cf","choices":[{"message":{"content":"{\"idx\": 0, \"zh\": \"半"},"finish_reason":"content_filter"}],`+
				`"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":64}}`)
		})
		c, _, rec := newClient(t, srv.URL)
		u := &Usage{}
		stats := &sched.StatsRecorder{}
		ctx := sched.WithStatsSink(WithUsage(context.Background(), u), stats)
		res, err := c.Complete(ctx, config.RoleTranslate, "SYS", "USR", jsonMode)
		var ce *model.CodedError
		if !errors.Is(err, ErrRefusal) || !sched.IsPermanent(err) || !errors.As(err, &ce) || ce.Info.Code != CodeRefusal ||
			ce.Info.Params["category"] != "" || errors.Is(err, ErrTruncated) || errors.Is(err, ErrBadJSON) {
			t.Fatalf("json %v: err %v", jsonMode, err)
		}
		if res.Text != "" {
			t.Errorf("json %v: the filtered content came back: %q", jsonMode, res.Text)
		}
		if n := len(got()); n != 1 {
			t.Errorf("json %v: %d requests for a filtered answer", jsonMode, n)
		}
		if len(rec.waits) != 0 {
			t.Errorf("json %v: backoff waits %v", jsonMode, rec.waits)
		}
		if tot := u.Totals(); tot != (Totals{PromptTokens: 100, CompletionTokens: 20, CacheHitTokens: 64, Requests: 1}) {
			t.Errorf("json %v: usage %+v", jsonMode, tot)
		}
		if got := eventLines(stats.Events); got != "start(1) end(1):"+model.LLMOutcomeRefused {
			t.Errorf("json %v: events %s", jsonMode, got)
		}
		raw, recs := readCalls(t, c.cfg.DataDir)
		if events(recs) != "start end failed" {
			t.Fatalf("json %v: call log events %q:\n%s", jsonMode, events(recs), raw)
		}
		if e := recs[1]; e.Result != "refused" || e.StopReason != "content_filter" || e.Status != 200 {
			t.Errorf("json %v: end %+v", jsonMode, e)
		}
	}
}
