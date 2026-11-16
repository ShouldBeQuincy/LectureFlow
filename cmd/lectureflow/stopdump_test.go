// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuf records each Write separately.
type lockedBuf struct {
	mu     sync.Mutex
	writes [][]byte
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes = append(b.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (b *lockedBuf) all() (int, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.writes), string(bytes.Join(b.writes, nil))
}

// stuckInGracefulStop is a recognisable frame for the dump.
func stuckInGracefulStop(release <-chan struct{}) { <-release }

func TestWithLimitDumpsStacksOnTimeout(t *testing.T) {
	dump := &lockedBuf{}
	var mu sync.Mutex
	var exits []int
	var dumpedBeforeExit bool
	exit := func(c int) {
		n, _ := dump.all()
		mu.Lock()
		exits = append(exits, c)
		dumpedBeforeExit = n == 1
		mu.Unlock()
	}
	release := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(release)
	}()
	withLimit(80*time.Millisecond, exitRestart, exit, dump, func(time.Time) { stuckInGracefulStop(release) })

	mu.Lock()
	defer mu.Unlock()
	if len(exits) != 1 || exits[0] != exitRestart || !dumpedBeforeExit {
		t.Fatalf("exits %v, dumped before exit %v", exits, dumpedBeforeExit)
	}
	n, out := dump.all()
	if n != 1 {
		t.Fatalf("dump written %d times, want once", n)
	}
	for _, want := range []string{
		"=== goroutine dump: graceful stop exceeded its time limit (80ms) ===\n",
		"goroutine ",
		"stuckInGracefulStop",
		"=== end of goroutine dump ===\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump lacks %q:\n%.2000s", want, out)
		}
	}
	if strings.Contains(out, "truncated") {
		t.Errorf("a small dump was marked truncated")
	}
}

func TestWithLimitNoDumpWithinLimit(t *testing.T) {
	dump := &lockedBuf{}
	exited := false
	withLimit(200*time.Millisecond, exitOK, func(int) { exited = true }, dump, func(time.Time) {
		time.Sleep(10 * time.Millisecond)
	})
	time.Sleep(300 * time.Millisecond) // past the limit: the watchdog was stopped
	if n, out := dump.all(); n != 0 || exited {
		t.Fatalf("stop within the limit dumped %d writes (exit %v):\n%s", n, exited, out)
	}
}

func TestStackDumpTruncated(t *testing.T) {
	const max = 300
	dump := &lockedBuf{}
	writeStackDump(dump, time.Second, max)
	n, out := dump.all()
	if n != 1 {
		t.Fatalf("%d writes", n)
	}
	head := "=== goroutine dump: graceful stop exceeded its time limit (1s) ===\n"
	tail := "\n=== truncated at 300 bytes ===\n=== end of goroutine dump ===\n"
	if !strings.HasPrefix(out, head) || !strings.HasSuffix(out, tail) {
		t.Fatalf("dump:\n%s", out)
	}
	if body := strings.TrimSuffix(strings.TrimPrefix(out, head), tail); len(body) != max {
		t.Fatalf("stacks %d bytes, want %d", len(body), max)
	}
	writeStackDump(nil, time.Second, max) // no writer: nothing, no panic
}
