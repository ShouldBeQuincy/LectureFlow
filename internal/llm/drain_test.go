// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// drainServer answers every request with the stream of an answer, flushed
// up to message_stop; then the handler waits for hold before it returns
// (the end of the response reaches the client after message_stop). It
// counts the connections the client opened.
func drainServer(t *testing.T, sse string, hold func(r *http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
		w.(http.Flusher).Flush()
		hold(r)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

// drainClient is a client of url whose drainBody reports to the channel.
func drainClient(t *testing.T, url string) (*HTTPClient, chan bool) {
	t.Helper()
	c, _, _, _ := sec4Client(t, url)
	drained := make(chan bool, 4)
	c.drained = func(eof bool) { drained <- eof }
	return c, drained
}

func waitDrained(t *testing.T, drained chan bool) bool {
	t.Helper()
	select {
	case eof := <-drained:
		return eof
	case <-time.After(10 * time.Second):
		t.Fatal("the response was not closed")
	}
	return false
}

// SEC4 L5: a streamed answer read up to message_stop is answered at once;
// the rest of the response (the end of the chunked body) is read before the
// body is closed, so the next request uses the same connection.
func TestStreamBodyDrainedForReuse(t *testing.T) {
	sse := sec4OK(t, `{"ok": true}`)
	srv, conns := drainServer(t, sse, func(*http.Request) { time.Sleep(50 * time.Millisecond) })
	c, drained := drainClient(t, srv.URL)
	for i := range 3 {
		res, err := c.Complete(context.Background(), model.RoleSummary, "s", "u", false)
		if err != nil || res.Text != `{"ok": true}` {
			t.Fatalf("request %d: %+v %v", i, res, err)
		}
		if !waitDrained(t, drained) {
			t.Errorf("request %d: the response was not read to its end", i)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("%d connections for 3 requests one after the other, want 1", n)
	}
}

// A server that does not end the response after message_stop: the answer
// is not held up, and the rest of the response is given up on after
// drainWait (the request is cancelled: the server sees it).
func TestStreamBodyDrainGivesUp(t *testing.T) {
	sse := sec4OK(t, `{"ok": true}`)
	release := make(chan struct{})
	gone := make(chan struct{}, 1)
	srv, _ := drainServer(t, sse, func(r *http.Request) {
		select {
		case <-r.Context().Done():
			gone <- struct{}{}
		case <-release:
		case <-time.After(20 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) }) // before srv.Close (cleanups run last first)
	c, drained := drainClient(t, srv.URL)
	start := time.Now()
	res, err := c.Complete(context.Background(), model.RoleSummary, "s", "u", false)
	if err != nil || res.Text != `{"ok": true}` {
		t.Fatalf("%+v %v", res, err)
	}
	// The answer came before the rest of the response was given up on (an
	// order, not a time: no margin for a loaded machine needed).
	select {
	case <-drained:
		t.Errorf("the answer came after %v, when the rest of the response had been given up on: it waited for it", time.Since(start))
		return
	default:
	}
	if waitDrained(t, drained) {
		t.Error("a response that never ended was read to its end")
	}
	if d := time.Since(start); d < drainWait/2 {
		t.Errorf("given up after %v, before drainWait (%v)", d, drainWait)
	}
	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Error("the server's request was not cancelled")
	}
}
