// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

// Review D8: the final CodeTimeout error of a request whose last attempt
// timed out connecting carries ErrConnectTimeout (the translate stage does
// not split a batch for it); one whose attempt ran out of its own timeout
// does not. Text and params are those of any CodeTimeout.
func TestConnectTimeoutMarked(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: dialTimeout{}}
	})}
	c := New(testConfig(t, "http://127.0.0.1:9"), &fakePool{}, "", hc)
	c.Sleep = (&recorder{}).sleep
	_, err := c.Complete(WithNoDeadlineRetry(context.Background()), model.RoleTranslate, "s", "u", true)
	if !isCode(err, CodeTimeout, "retries", 4) || !errors.Is(err, ErrConnectTimeout) {
		t.Fatalf("connect: err %v %+v", err, infoOf(err))
	}
	if len(infoOf(err).Params) != 3 {
		t.Errorf("params %v", infoOf(err).Params)
	}

	shortAttempts(t, 200*time.Millisecond)
	for _, mark := range []bool{true, false} {
		s := llmtest.New(t)
		c, _, _ := newClient(t, s.URL)
		s.FailNext(llmtest.RoleSummary, 5, llmtest.FailHang)
		ctx := context.Background()
		if mark {
			ctx = WithNoDeadlineRetry(ctx)
		}
		_, err := c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "u", false)
		if !isCode(err, CodeTimeout) || errors.Is(err, ErrConnectTimeout) {
			t.Errorf("attempt timeout (marked %v): err %v", mark, err)
		}
	}
}
