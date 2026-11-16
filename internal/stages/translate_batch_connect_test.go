// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
)

// connectTimeout is a dial timeout (net.Error with Timeout() true).
type connectTimeout struct{}

func (connectTimeout) Error() string   { return "i/o timeout" }
func (connectTimeout) Timeout() bool   { return true }
func (connectTimeout) Temporary() bool { return true }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Review D8: a batch request that timed out connecting to the server (the
// client's retries used up) is not split: the request never got there, so
// smaller halves would only take five more attempts each. The stage fails
// as on the single path, with llm.CodeTimeout; nothing is written.
func TestTranslateBatchConnectTimeoutNotSplit(t *testing.T) {
	t.Run("the client's error", func(t *testing.T) {
		e := newEnv(t)
		l := e.sampleLecture()
		e.setTranslateBatch(l.ID, 4)
		var attempts atomic.Int32
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts.Add(1)
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: connectTimeout{}}
		})}
		c := llm.New(e.cfg, nil, "", hc)
		c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
		d := e.deps(c)
		d.Concurrency = 1
		log := captureLog(t)
		r := e.run(e.stage(d, model.StageTranslate), l.ID)
		if jobErrorCode(r.job) != llm.CodeTimeout || r.job.Status != model.StatusFailed {
			t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
		}
		if !errors.Is(r.err, llm.ErrConnectTimeout) {
			t.Errorf("err %v: want llm.ErrConnectTimeout in its chain", r.err)
		}
		if n := attempts.Load(); n != 5 {
			t.Errorf("%d attempts, want the five of the first chunk only", n)
		}
		if out := log.String(); strings.Contains(out, "batch timed out, split") {
			t.Errorf("split:\n%s", out)
		}
		for _, s := range e.sentences(l.ID) {
			if s.ZhStatus != model.ZhNone {
				t.Errorf("sentence %d is %s", s.Idx, s.ZhStatus)
			}
		}
	})
	t.Run("scripted", func(t *testing.T) {
		connect := model.Errorf(llm.CodeTimeout, map[string]any{"role": config.RoleTranslate, "retries": 4, "seconds": 240.0},
			errors.Join(errors.New("LLM 请求超时（translate，已重试 4 次）"), llm.ErrConnectTimeout))
		e := newEnv(t)
		l := e.sampleLecture()
		e.setTranslateBatch(l.ID, 4)
		fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", connect }}
		d := e.deps(fake)
		d.Concurrency = 1
		r := e.run(e.stage(d, model.StageTranslate), l.ID)
		if jobErrorCode(r.job) != llm.CodeTimeout || r.job.Status != model.StatusFailed {
			t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
		}
		if batch, single := splitCalls(fake); len(batch) != 1 || len(single) != 0 {
			t.Errorf("%d batch and %d single requests, want the one batch request", len(batch), len(single))
		}
	})
}
