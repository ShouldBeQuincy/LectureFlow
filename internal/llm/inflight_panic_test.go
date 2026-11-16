// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"io"
	"net/http"
	"testing"

	"lectureflow/internal/config"
)

// Review D16: a request that panics between the call log's start and end
// records is not left counted in flight: the panic goes on to the caller,
// the count goes back to 0 and the files are closed, and the next request
// is counted and closed as usual.
func TestCallLogInFlightAfterPanic(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody("ok")) })
	c := New(testConfig(t, srv.URL), nil, "", nil)
	next := c.http.Transport
	boom := true
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if boom {
			panic("transport panic")
		}
		return next.RoundTrip(r)
	})
	func() {
		defer func() {
			if r := recover(); r != "transport panic" {
				t.Errorf("recovered %v, want the transport's panic", r)
			}
		}()
		_, _ = c.Complete(context.Background(), config.RoleSummary, "s", "u", false)
	}()
	inFlight := func() int {
		c.calls.mu.Lock()
		defer c.calls.mu.Unlock()
		return c.calls.inFlight
	}
	if n := inFlight(); n != 0 {
		t.Errorf("%d attempts in flight after the panic, want 0", n)
	}
	if open := openFiles(c.calls); len(open) != 0 {
		t.Errorf("open after the panic: %v", open)
	}
	boom = false
	if _, err := c.Complete(context.Background(), config.RoleSummary, "s", "u", false); err != nil {
		t.Fatal(err)
	}
	if n := inFlight(); n != 0 {
		t.Errorf("%d attempts in flight after a request, want 0", n)
	}
	if open := openFiles(c.calls); len(open) != 0 {
		t.Errorf("open after a request: %v", open)
	}
}
