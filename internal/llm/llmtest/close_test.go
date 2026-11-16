// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"context"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
)

// Review R18 suggestion 5 (T4): Close does not wait for a request the server
// hangs on (FailHang) until the client gives up: it closes the clients'
// connections, the handler returns, the client's request fails at once.
func TestCloseEndsHangingRequests(t *testing.T) {
	s, c := setup(t)
	c.Timeout = 20 * time.Second // far longer than the test waits
	s.FailNext(llmtest.RoleSummary, 5, llmtest.FailHang)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // bounds the request if Close does not end it
	defer cancel()
	go func() {
		_, err := c.Complete(ctx, config.RoleSummary, ctxbuild.SummarySystemTemplate, "u", false)
		done <- err
	}()
	for i := 0; s.Count(llmtest.RoleSummary) == 0; i++ {
		if i > 1000 {
			t.Fatal("the request did not arrive")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	s.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Close took %v: it waited for the hanging request", d)
	}
	cancel() // the client's retries are not what this test is about
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("the request did not end")
	}
}
