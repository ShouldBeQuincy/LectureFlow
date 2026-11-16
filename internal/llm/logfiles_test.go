// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// T-PERF-08: the debug log's files follow the call log's retention rules,
// the call log keeps its files open between writes.

// Debug log names used below: job ids as db.NewID makes them (ULIDs).
const (
	ulidOld1 = "01JC0000000000000000000001"
	ulidOld2 = "01JC0000000000000000000002"
	ulidNew1 = "01JC0000000000000000000003"
	ulidNew2 = "01JC0000000000000000000004"
	ulidNew3 = "01JC0000000000000000000005"
	ulidDir  = "01JC0000000000000000000006"
)

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// sizedFile makes a file of size bytes (sparse where the file system can)
// last written at mtime.
func sizedFile(t *testing.T, path string, size int64, mtime time.Time) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestIsDebugLogName(t *testing.T) {
	for name, want := range map[string]bool{
		ulidOld1 + ".jsonl":                  true,
		"nojob.jsonl":                        true,
		strings.ToLower(ulidOld1) + ".jsonl": false,
		ulidOld1 + ".json":                   false,
		ulidOld1:                             false,
		"01JOB.jsonl":                        false,
		"notes.jsonl":                        false,
		"calls-2026-11-10.jsonl":             false,
		"rejected-2026-11-10.jsonl":          false,
		"ZZZZZZZZZZZZZZZZZZZZZZZZZZ.jsonl":   false, // not a valid ULID
		"../" + ulidOld1 + ".jsonl":          false,
	} {
		if got := isDebugLogName(name); got != want {
			t.Errorf("isDebugLogName(%q) = %v, want %v", name, got, want)
		}
	}
}

// Old debug log files go by age and by total size, when the call log
// prunes (its first write of a day); today's files, the call log's files
// and files of other names stay.
func TestDebugLogPrune(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	l := newCallLog(dir)
	l.now = func() time.Time { return now }
	daysAgo := func(d int) time.Time { return now.AddDate(0, 0, -d) }
	old := daysAgo(CallLogKeepDays + 1)

	sizedFile(t, filepath.Join(dir, ulidOld1+".jsonl"), 10, old)
	sizedFile(t, filepath.Join(dir, "nojob.jsonl"), 10, old)
	sizedFile(t, filepath.Join(dir, ulidNew1+".jsonl"), 10, daysAgo(CallLogKeepDays))
	// not ours, however old
	sizedFile(t, filepath.Join(dir, "notes.jsonl"), 10, old)
	sizedFile(t, filepath.Join(dir, "01JOB.jsonl"), 10, old)
	sizedFile(t, filepath.Join(dir, strings.ToLower(ulidOld2)+".jsonl"), 10, old)
	sizedFile(t, filepath.Join(dir, "readme.txt"), 10, old)
	if err := os.Mkdir(filepath.Join(dir, ulidDir+".jsonl"), 0o755); err != nil { // a folder of a debug log's name
		t.Fatal(err)
	}
	// A symbolic link of a debug log's name to an old file elsewhere: neither
	// the link nor its target goes. Creating symbolic links needs a privilege
	// on Windows; without it a directory junction of the same name (no
	// privilege needed) to a folder holding an old file is checked instead:
	// neither the junction nor what is behind it goes (review T6).
	outside := filepath.Join(t.TempDir(), "target.jsonl")
	sizedFile(t, outside, 10, old)
	link := filepath.Join(dir, ulidNew3+".jsonl")
	linked := os.Symlink(outside, link) == nil
	if !linked && runtime.GOOS == "windows" {
		behind := t.TempDir()
		outside = filepath.Join(behind, ulidOld1+".jsonl")
		sizedFile(t, outside, 10, old)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := exec.CommandContext(ctx, "cmd", "/c", "mklink", "/J", link, behind).CombinedOutput()
		cancel()
		if linked = err == nil; !linked {
			t.Logf("no directory junction either: %v %s", err, out)
		}
	}
	if !linked {
		t.Log("no symbolic link (no privilege): the link case is not checked")
	}

	l.write(callsPrefix, map[string]string{"event": "start"})

	want := []string{"01JOB.jsonl", strings.ToLower(ulidOld2) + ".jsonl", ulidDir + ".jsonl", ulidNew1 + ".jsonl",
		callsPrefix + now.Format(callLogDay) + callLogExt, "notes.jsonl", "readme.txt"}
	if linked {
		want = append(want, ulidNew3+".jsonl")
	}
	sort.Strings(want)
	if got := dirNames(t, dir); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("after pruning by age:\n got %v\nwant %v", got, want)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the link's target: %v", err)
	}

	// The next day: the debug files together are over the limit, the
	// oldest go first; yesterday's file is the newest of them and stays. The
	// call log's files count separately and stay.
	big := int64(CallLogMaxBytes/2) + 1
	sizedFile(t, filepath.Join(dir, ulidNew2+".jsonl"), big, daysAgo(2))
	sizedFile(t, filepath.Join(dir, "nojob.jsonl"), big, daysAgo(0))
	sizedFile(t, filepath.Join(dir, callsPrefix+daysAgo(3).Format(callLogDay)+callLogExt), big, daysAgo(3))
	now = now.AddDate(0, 0, 1)
	l.write(callsPrefix, map[string]string{"event": "end"})
	got := strings.Join(dirNames(t, dir), " ")
	for _, gone := range []string{ulidNew1 + ".jsonl", ulidNew2 + ".jsonl"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s is still there: %s", gone, got)
		}
	}
	for _, kept := range []string{"nojob.jsonl", callsPrefix + "2026-11-07", callsPrefix + "2026-11-10", callsPrefix + "2026-11-11",
		"notes.jsonl", "01JOB.jsonl", "readme.txt", ulidDir + ".jsonl"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%s is gone: %s", kept, got)
		}
	}
	l.Close()
}

// The debug log of a client in debug mode is pruned with the call log, and
// a file written today stays.
func TestDebugLogPrunedThroughClient(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody("ok")) })
	cfg := testConfig(t, srv.URL)
	cfg.LogLevel = "debug"
	dir := filepath.Join(t.TempDir(), "llm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ulidOld1+".jsonl")
	sizedFile(t, stale, 10, time.Now().AddDate(0, 0, -CallLogKeepDays-2))
	c := New(cfg, nil, dir, nil)
	t.Cleanup(func() { c.CloseLogs() })
	ctx := sched.WithJob(context.Background(), model.Job{ID: ulidNew1})
	if _, err := c.Complete(ctx, config.RoleSummary, "s", "u", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old debug log is still there: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ulidNew1+".jsonl"))
	if err != nil || !bytes.Contains(b, []byte(`"job_id":"`+ulidNew1+`"`)) {
		t.Errorf("today's debug log: %q, %v", b, err)
	}
}

// openFiles returns the files the call log holds open, by prefix.
func openFiles(l *callLog) map[string]*os.File {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]*os.File{}
	for p, f := range l.files {
		out[p] = f
	}
	return out
}

// While an attempt is in flight the call log keeps its files open between
// writes, and each line is readable as soon as write returns; the end of
// the last attempt in flight closes them; a new day closes the old day's
// file and opens the new day's.
func TestCallLogReusesHandle(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	l := newCallLog(dir)
	l.now = func() time.Time { return now }
	defer l.Close()
	path1 := filepath.Join(dir, callsPrefix+"2026-11-10"+callLogExt)
	content := func(path string) string {
		b, _ := os.ReadFile(path)
		return string(b)
	}

	l.writeFlight(callsPrefix, map[string]string{"event": "start", "n": "1"}, +1)
	f1 := openFiles(l)[callsPrefix]
	if f1 == nil {
		t.Fatal("no open file while an attempt is in flight")
	}
	if got := content(path1); got != "{\"event\":\"start\",\"n\":\"1\"}\n" {
		t.Errorf("after one write the file holds %q", got)
	}
	l.writeFlight(callsPrefix, map[string]string{"event": "start", "n": "2"}, +1)
	l.write(rejectedPrefix, map[string]string{"kind": "check"})
	l.writeFlight(callsPrefix, map[string]string{"event": "end", "n": "1"}, -1)
	if open := openFiles(l); open[callsPrefix] != f1 || open[rejectedPrefix] == nil {
		t.Errorf("with one attempt still in flight: %v (first handle %p)", open, f1)
	}
	if got := content(path1); strings.Count(got, "\n") != 3 {
		t.Errorf("after three writes the file holds %q", got)
	}
	l.writeFlight(callsPrefix, map[string]string{"event": "end", "n": "2"}, -1)
	if open := openFiles(l); len(open) != 0 {
		t.Errorf("open after the last end: %v", open)
	}
	if _, err := f1.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("the file is still open: %v", err)
	}
	if got := content(path1); strings.Count(got, "\n") != 4 || !strings.HasSuffix(got, "{\"event\":\"end\",\"n\":\"2\"}\n") {
		t.Errorf("after the last end the file holds %q", got)
	}

	// A record with nothing in flight opens and closes its file.
	l.write(callsPrefix, map[string]string{"event": "failed"})
	if open := openFiles(l); len(open) != 0 {
		t.Errorf("open after a record outside an attempt: %v", open)
	}

	// The day changes during an attempt.
	l.writeFlight(callsPrefix, map[string]string{"event": "start", "n": "3"}, +1)
	f3 := openFiles(l)[callsPrefix]
	now = now.AddDate(0, 0, 1)
	l.writeFlight(callsPrefix, map[string]string{"event": "end", "n": "3"}, -1)
	if _, err := f3.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("the old day's file is still open: %v", err)
	}
	if got := content(filepath.Join(dir, callsPrefix+"2026-11-11"+callLogExt)); got != "{\"event\":\"end\",\"n\":\"3\"}\n" {
		t.Errorf("the new day's file holds %q", got)
	}
	if got := content(path1); strings.Contains(got, `"end","n":"3"`) {
		t.Errorf("the old day's file got the new day's line: %q", got)
	}
	// An end without a start (cannot happen) does not keep files open.
	l.writeFlight(callsPrefix, map[string]string{"event": "end"}, -1)
	l.write(callsPrefix, map[string]string{"event": "failed"})
	if open := openFiles(l); len(open) != 0 {
		t.Errorf("open after an unmatched end: %v", open)
	}
}

// Requests through the client: no file stays open once they returned.
func TestCallLogClosedAfterRequests(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody("ok")) })
	c := New(testConfig(t, srv.URL), nil, "", nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Complete(context.Background(), config.RoleSummary, "s", "u", false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if open := openFiles(c.calls); len(open) != 0 {
		t.Errorf("open after the requests: %v", open)
	}
	_, recs := readCalls(t, c.cfg.DataDir)
	if got := events(recs); strings.Count(got, "start") != 8 || strings.Count(got, "end") != 8 {
		t.Errorf("events: %s", got)
	}
}

// Writers at the same time: every line arrives whole, none is lost.
func TestCallLogConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	l := newCallLog(dir)
	const writers, each = 16, 100
	pad := strings.Repeat("x", 3000) // longer lines show interleaving
	var wg sync.WaitGroup
	for g := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				l.writeFlight(callsPrefix, map[string]any{"g": g, "i": i, "e": "start", "pad": pad}, +1)
				l.write(rejectedPrefix, map[string]any{"g": g, "i": i, "e": "rejected", "pad": pad})
				l.writeFlight(callsPrefix, map[string]any{"g": g, "i": i, "e": "end", "pad": pad}, -1)
			}
		}()
	}
	wg.Wait()
	if open := openFiles(l); len(open) != 0 {
		t.Errorf("open after all ends: %v", open)
	}
	for prefix, kinds := range map[string][]string{callsPrefix: {"start", "end"}, rejectedPrefix: {"rejected"}} {
		names, _ := filepath.Glob(filepath.Join(dir, prefix+"*"+callLogExt))
		if len(names) != 1 {
			t.Fatalf("files: %v", names)
		}
		b, err := os.ReadFile(names[0])
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var v struct {
				G, I   int
				E, Pad string
			}
			if err := json.Unmarshal(sc.Bytes(), &v); err != nil || v.Pad != pad {
				t.Fatalf("a broken line (%v): %.80q", err, sc.Text())
			}
			k := fmt.Sprint(v.G, "/", v.I, "/", v.E)
			if seen[k] {
				t.Errorf("line %s twice", k)
			}
			seen[k] = true
		}
		if want := writers * each * len(kinds); len(seen) != want {
			t.Errorf("%s: %d lines, want %d", prefix, len(seen), want)
		}
	}
}

// CloseLogs releases the open file during an attempt: the folder can be
// renamed (on Windows an open file inside keeps it from being renamed;
// checked there), and the attempt's end opens the file again.
func TestCloseLogsLetsFolderMove(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	dir := filepath.Join(data, "logs", "llm")
	c := &HTTPClient{calls: newCallLog(dir)}
	today := callsPrefix + time.Now().Format(callLogDay) + callLogExt
	c.calls.writeFlight(callsPrefix, map[string]string{"event": "start"}, +1)
	moved := filepath.Join(root, "moved")
	if runtime.GOOS == "windows" {
		if err := os.Rename(data, moved); err == nil {
			t.Fatal("the folder was renamed with the call log's file open: the test does not show what it should")
		}
	}
	if err := c.CloseLogs(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(data, moved); err != nil {
		t.Fatalf("rename after CloseLogs: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(moved, "logs", "llm", today))
	if err != nil || string(b) != "{\"event\":\"start\"}\n" {
		t.Errorf("the moved file: %q, %v", b, err)
	}
	// Back in place, the end of the attempt opens the file again.
	if err := os.Rename(moved, data); err != nil {
		t.Fatal(err)
	}
	c.calls.writeFlight(callsPrefix, map[string]string{"event": "end"}, -1)
	if open := openFiles(c.calls); len(open) != 0 {
		t.Errorf("open after the end: %v", open)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, today)); string(b) != "{\"event\":\"start\"}\n{\"event\":\"end\"}\n" {
		t.Errorf("after the end: %q", b)
	}
	var nilClient *HTTPClient
	if err := nilClient.CloseLogs(); err != nil {
		t.Error(err)
	}
}
