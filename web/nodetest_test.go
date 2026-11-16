// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/procutil"
)

// Every node run of the JavaScript checks in testdata goes through runNode
// or runNodeIO (INC-4, 2026-11-11: one failed assert.equal of two stand-in
// DOM nodes made a node process take some 236 GB):
//   - testdata/safe_assert.mjs is preloaded: a failed comparison of large or
//     non-plain operands throws a short AssertionError instead of node's
//     whole-object diff (the pass or fail is node's own answer);
//   - the JavaScript heap is capped (--max-old-space-size), which does not
//     hold node's diff: its arrays are typed arrays, outside the heap;
//   - so the whole process is capped too, at nodeMemoryBytes of committed
//     memory (procutil: a Job Object on Windows, where an allocation over
//     the cap fails in node and the run ends with an error that says so).
//     Other systems have no such cap in procutil; there the timeout is the
//     only bound (the CI runners are thrown away after each job);
//   - the run is killed after nodeTimeout;
//   - the output is cut to its head and tail beyond nodeOutputCap bytes.
//
// TestNoBareNodeRuns holds every web test to these helpers. testdata/README.md
// has the same command for a run by hand.
const (
	nodeTimeout = 2 * time.Minute
	nodeHeapMB  = 1024
	// The checks peak at some 60 MB; a diff that slipped past the preload
	// grows with the square of its lines and is stopped here, well before
	// it can take the machine (64 GB, other programs and test runs beside).
	nodeMemoryBytes = 2 << 30
	nodeOutputCap   = 16 << 10
)

// nodeArgs are the node options every run starts with.
func nodeArgs(t *testing.T) []string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "safe_assert.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	pre := (&url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(filepath.ToSlash(p), "/")}).String()
	return []string{"--import", pre, fmt.Sprintf("--max-old-space-size=%d", nodeHeapMB)}
}

// nodeCommand is node (the path exec.LookPath gave) with the guards and
// args, killed after timeout. Run it with runCapped only.
func nodeCommand(t *testing.T, node string, timeout time.Duration, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, node, append(nodeArgs(t), args...)...)
	cmd.Cancel = func() error {
		if ctx.Err() == context.DeadlineExceeded {
			t.Logf("node %s: killed after %v", strings.Join(args, " "), timeout)
		}
		return cmd.Process.Kill()
	}
	// The pipes are closed this long after the kill even if something still
	// holds them.
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// runCapped runs cmd under a cap of memBytes of committed memory (Windows;
// see the comment on the constants) and says so in the error when the run
// ran out of it.
func runCapped(cmd *exec.Cmd, memBytes uint64) error {
	l := procutil.Limits{MemoryBytes: memBytes}
	p, err := procutil.Start(cmd, l)
	if err != nil {
		return err
	}
	defer p.Close()
	err = cmd.Wait()
	if u := p.Usage(); u.MemoryExceeded(l, err != nil) {
		return fmt.Errorf("node ran out of its memory cap of %d MB (peak %d MB): %w", memBytes>>20, u.PeakMemory>>20, err)
	}
	return err
}

// runNode runs testdata/<script> guarded and answers its output (stdout and
// stderr together, cut to its head and tail beyond nodeOutputCap bytes).
func runNode(t *testing.T, node, script string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := nodeCommand(t, node, nodeTimeout, append([]string{filepath.Join("testdata", script)}, args...)...)
	return combinedCapped(cmd, nodeMemoryBytes)
}

// runNodeIO runs testdata/<script> guarded with stdin and answers its
// stdout (up to 1 MB) and stderr (cut as runNode's output) apart.
func runNodeIO(t *testing.T, node, script string, stdin []byte, args ...string) (stdout, stderr []byte, err error) {
	t.Helper()
	cmd := nodeCommand(t, node, nodeTimeout, append([]string{filepath.Join("testdata", script)}, args...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	out := cappedBuffer{limit: 1 << 20}
	var errOut cappedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = runCapped(cmd, nodeMemoryBytes)
	return out.Bytes(), errOut.Bytes(), err
}

func combinedCapped(cmd *exec.Cmd, memBytes uint64) ([]byte, error) {
	var out cappedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := runCapped(cmd, memBytes)
	return out.Bytes(), err
}

// cappedBuffer keeps the first and the last limit/2 bytes written to it
// (limit: nodeOutputCap when zero) and counts the rest.
type cappedBuffer struct {
	limit      int
	head, tail []byte
	cut        int64
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	half := b.limit / 2
	if b.limit == 0 {
		half = nodeOutputCap / 2
	}
	if room := half - len(b.head); room > 0 {
		k := min(room, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	b.tail = append(b.tail, p...)
	if over := len(b.tail) - half; over > 0 {
		b.cut += int64(over)
		b.tail = append(b.tail[:0], b.tail[over:]...)
	}
	return n, nil
}

func (b *cappedBuffer) Bytes() []byte {
	if b.cut == 0 {
		return append(append([]byte{}, b.head...), b.tail...)
	}
	var s bytes.Buffer
	s.Write(b.head)
	fmt.Fprintf(&s, "\n… %d bytes of output cut …\n", b.cut)
	s.Write(b.tail)
	return s.Bytes()
}

// The preload: every patched function of node:assert answers as node's own,
// node's message is kept for small plain data, and a failed comparison of
// two nodes of a stand-in page is short and at once.
func TestNodeSafeAssert(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/safe_assert_test.mjs need it")
	}
	out, err := runNode(t, node, "safe_assert_test.mjs")
	if err != nil || !strings.Contains(string(out), "safe_assert checks: ok") {
		t.Fatalf("safe_assert_test.mjs: %v\n%s", err, out)
	}
}

// A node run that does not end is killed at its timeout, and the test goes
// on.
func TestNodeTimeoutKills(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the guarded node runs need it")
	}
	start := time.Now()
	cmd := nodeCommand(t, node, 5*time.Second, "-e", "console.log('pid', process.pid); setInterval(() => {}, 1000);")
	out, err := combinedCapped(cmd, nodeMemoryBytes)
	took := time.Since(start)
	if err == nil {
		t.Fatalf("a node run that does not end ended on its own: %s", out)
	}
	if took > 30*time.Second {
		t.Fatalf("the run took %v, not about its 5s timeout", took)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.Exited() && cmd.ProcessState.ExitCode() == 0 {
		t.Fatalf("node was not killed: %v", cmd.ProcessState)
	}
	t.Logf("output: %s", out)
	if !strings.Contains(string(out), "pid ") {
		t.Fatalf("node did not start: %v\n%s", err, out)
	}
}

// Every node run of web's tests goes through runNode / runNodeIO: a test
// file that starts a process itself (exec.Command / exec.CommandContext)
// fails here.
func TestNoBareNodeRuns(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	bare := regexp.MustCompile(`exec\.Command(Context)?\(`)
	for _, f := range files {
		if f == "nodetest_test.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if bare.MatchString(line) {
				t.Errorf("%s:%d starts a process itself; run node with runNode (or runNodeIO) of nodetest_test.go: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestCappedBuffer(t *testing.T) {
	var b cappedBuffer
	b.Write([]byte("start\n"))
	for range 10000 {
		b.Write([]byte("0123456789"))
	}
	b.Write([]byte("\nchecks: ok"))
	got := string(b.Bytes())
	if len(got) > nodeOutputCap+100 || !strings.HasPrefix(got, "start\n") || !strings.HasSuffix(got, "checks: ok") || !strings.Contains(got, "bytes of output cut") {
		t.Fatalf("capped output of %d bytes: %.200q … %.200q", len(got), got, got[max(0, len(got)-200):])
	}
	var small cappedBuffer
	small.Write([]byte("a"))
	small.Write([]byte("b"))
	if string(small.Bytes()) != "ab" {
		t.Fatalf("small output %q", small.Bytes())
	}
}
