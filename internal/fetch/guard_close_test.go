// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"io"
	"log/slog"
	"net"
	"runtime"
	"testing"
	"time"
)

// The guard's end (SEC43 info-7, the occasional "the guard still listens"
// of TestRealAria2cThroughGuard). Two closes of a guard can meet: the
// engine's stop and the goroutine that waits for aria2c to exit both close
// it. The one that came second returned at once, while the first was still
// closing the sessions, before the listener; and that goroutine marked the
// instance exited before it closed the guard. Whoever saw "exited", or
// returned from a close, could still connect.

// openSessions opens n sessions of g (each with its own three ports), so
// that closing g takes a while.
func openSessions(t *testing.T, g *guard, n int) {
	t.Helper()
	for k := 0; k < n; k++ {
		if _, err := g.open((&addrPolicy{}).forTask("")); err != nil {
			t.Fatal(err)
		}
	}
}

func listens(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// A close that meets another one returns once the guard has stopped
// listening, not before.
func TestGuardCloseWaitsForTheOtherClose(t *testing.T) {
	for round := 0; round < 20; round++ {
		g, err := newGuard(slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		openSessions(t, g, 30)
		go g.close()
		// the first close has begun (bounded wait)
		for spin := 0; ; spin++ {
			g.mu.Lock()
			closing := g.closed
			g.mu.Unlock()
			if closing {
				break
			}
			if spin > 1_000_000 {
				t.Fatal("the first close did not begin")
			}
			runtime.Gosched()
		}
		g.close()
		if listens(g.addr) {
			t.Fatalf("round %d: a close returned while the guard still listened", round)
		}
	}
}

// A close that meets another one waits for it at most guardCloseWait
// (SEC43b info-9): a first close that got stuck (here on a session's lock,
// held by the test) does not hold the second one, and with it
// Queue.Close, for ever. Once unstuck, the first close ends the guard.
func TestGuardSecondCloseWaitIsBounded(t *testing.T) {
	old := guardCloseWait
	guardCloseWait = 300 * time.Millisecond
	t.Cleanup(func() { guardCloseWait = old })
	g, err := newGuard(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := g.open((&addrPolicy{}).forTask(""))
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	held := true
	release := func() {
		if held {
			held = false
			s.mu.Unlock()
		}
	}
	defer release()
	first := make(chan struct{})
	go func() { g.close(); close(first) }()
	for spin := 0; ; spin++ { // the first close has begun (bounded wait)
		g.mu.Lock()
		closing := g.closed
		g.mu.Unlock()
		if closing {
			break
		}
		if spin > 1_000_000 {
			t.Fatal("the first close did not begin")
		}
		runtime.Gosched()
	}
	second := make(chan struct{})
	start := time.Now()
	go func() { g.close(); close(second) }()
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("the second close still waits for the stuck first one after 5 s")
	}
	if took := time.Since(start); took < 250*time.Millisecond {
		t.Errorf("the second close returned after %v, before waiting for the first", took)
	}
	select {
	case <-first:
		t.Fatal("the first close was not stuck")
	default:
	}
	release()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("the first close did not end")
	}
	if listens(g.addr) {
		t.Fatal("the guard still listens")
	}
}

// An aria2c that exits by itself (a crash): once its instance is marked
// exited, its guard no longer listens.
func TestExitedInstanceGuardClosed(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	for round := 0; round < 10; round++ {
		inst, err := e.q.eng.acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		openSessions(t, inst.guard, 30)
		e.aria.stop() // aria2c exits on its own
		select {
		case <-inst.exited:
		case <-time.After(10 * time.Second):
			t.Fatal("the instance was not marked exited")
		}
		if listens(inst.guard.addr) {
			t.Fatalf("round %d: the exited aria2c's guard still listens", round)
		}
		e.q.eng.release()
	}
}
