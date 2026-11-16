// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// newTestRotatingLog builds a writer with small limits; hook may replace
// the rename / remove functions before the file is opened.
func newTestRotatingLog(t *testing.T, path string, max int64, keep int, hook func(r *rotatingLog)) (*rotatingLog, *[]error) {
	t.Helper()
	var warnings []error
	r := &rotatingLog{
		path: path, maxBytes: max, keep: keep,
		rename: os.Rename, remove: os.Remove, open: openAppend,
		warn: func(err error) { warnings = append(warnings, err) },
	}
	if hook != nil {
		hook(r)
	}
	if err := r.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, &warnings
}

func rlReadFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rlExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestRotatingLogRotatesPastLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	r, warnings := newTestRotatingLog(t, path, 20, 5, nil)
	for _, line := range []string{"aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n"} { // 9 bytes each
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if got := rlReadFile(t, path+".1"); got != "aaaaaaaa\nbbbbbbbb\n" {
		t.Fatalf(".1 = %q", got)
	}
	if got := rlReadFile(t, path); got != "cccccccc\n" {
		t.Fatalf("current = %q", got)
	}
	if rlExists(path + ".2") {
		t.Fatal(".2 should not exist yet")
	}
	if len(*warnings) != 0 {
		t.Fatalf("warnings: %v", *warnings)
	}
}

func TestRotatingLogKeepsAtMostKeepFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	r, _ := newTestRotatingLog(t, path, 10, 5, nil)
	// One 10-byte line per file: line i lands in its own file.
	for i := 0; i < 9; i++ {
		if _, err := fmt.Fprintf(r, "line %03d\n", i); err != nil { // 9 bytes
			t.Fatal(err)
		}
		if _, err := r.Write([]byte("\n")); err != nil { // 10 bytes total
			t.Fatal(err)
		}
	}
	if got := rlReadFile(t, path); got != "line 008\n\n" {
		t.Fatalf("current = %q", got)
	}
	for n := 1; n <= 5; n++ {
		want := fmt.Sprintf("line %03d\n\n", 8-n)
		if got := rlReadFile(t, path+"."+strconv.Itoa(n)); got != want {
			t.Fatalf(".%d = %q, want %q", n, got, want)
		}
	}
	if rlExists(path + ".6") {
		t.Fatal(".6 must not exist: only 5 old files are kept")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 6 {
		t.Fatalf("%d files in logs/, want 6", len(entries))
	}
}

func TestRotatingLogRotatesOversizedFileAtStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lectureflow.log")
	old := strings.Repeat("x", 30) + "\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("older\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := newTestRotatingLog(t, path, 20, 5, nil)
	if got := rlReadFile(t, path+".1"); got != old {
		t.Fatalf(".1 = %q", got)
	}
	if got := rlReadFile(t, path+".2"); got != "older\n" {
		t.Fatalf(".2 = %q", got)
	}
	if _, err := r.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if got := rlReadFile(t, path); got != "new\n" {
		t.Fatalf("current = %q", got)
	}
}

func TestRotatingLogAppendsToSmallExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := newTestRotatingLog(t, path, 20, 5, nil)
	if _, err := r.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if got := rlReadFile(t, path); got != "old\nnew\n" {
		t.Fatalf("current = %q", got)
	}
	if rlExists(path + ".1") {
		t.Fatal("no rotation expected")
	}
}

// Concurrent slog records: none lost, none split across files, and no file
// past the limit (every line is shorter than the limit).
func TestRotatingLogConcurrentWritesKeepEveryLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	const workers, perWorker, keep = 8, 50, 100 // keep everything to count it
	r, _ := newTestRotatingLog(t, path, 4096, keep, nil)
	logger := slog.New(slog.NewTextHandler(r, nil))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				logger.Info("concurrent", "id", fmt.Sprintf("w%d-%d", w, i))
			}
		}(w)
	}
	wg.Wait()
	_ = r.Close()

	seen := map[string]bool{}
	files := []string{path}
	for n := 1; rlExists(path + "." + strconv.Itoa(n)); n++ {
		files = append(files, path+"."+strconv.Itoa(n))
	}
	if len(files) < 3 {
		t.Fatalf("expected several rotations, got %d files", len(files))
	}
	for _, f := range files {
		b := rlReadFile(t, f)
		if int64(len(b)) > 4096 {
			t.Fatalf("%s has %d bytes, past the limit", f, len(b))
		}
		if b != "" && !strings.HasSuffix(b, "\n") {
			t.Fatalf("%s ends mid-line", f)
		}
		sc := bufio.NewScanner(strings.NewReader(b))
		for sc.Scan() {
			line := sc.Text()
			i := strings.Index(line, "id=")
			if !strings.HasPrefix(line, "time=") || i < 0 {
				t.Fatalf("broken line in %s: %q", f, line)
			}
			id := line[i+3:]
			if seen[id] {
				t.Fatalf("duplicate line %s", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("%d lines, want %d", len(seen), workers*perWorker)
	}
}

// A rotation that fails (a file held open by another process on Windows)
// leaves the writer appending to the current file, warns once, and is
// retried later.
func TestRotatingLogKeepsWritingWhenRotationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	locked := true
	var calls int
	r, warnings := newTestRotatingLog(t, path, 20, 5, func(r *rotatingLog) {
		r.rename = func(oldpath, newpath string) error {
			if oldpath == path {
				calls++
				if locked {
					return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errors.New("file in use")}
				}
			}
			return os.Rename(oldpath, newpath)
		}
	})
	var want strings.Builder
	for i := 0; i < 10; i++ {
		line := fmt.Sprintf("line %d\n", i) // 7 bytes
		want.WriteString(line)
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if got := rlReadFile(t, path); got != want.String() {
		t.Fatalf("current = %q, want every line", got)
	}
	if len(*warnings) != 1 {
		t.Fatalf("%d warnings, want exactly 1: %v", len(*warnings), *warnings)
	}
	if calls < 2 {
		t.Fatalf("rotation tried %d times, want retries", calls)
	}
	if calls > 9 {
		t.Fatalf("rotation tried %d times: should back off between tries", calls)
	}

	// The lock goes away: the next try rotates everything written so far.
	locked = false
	for i := 0; i < 10 && !rlExists(path+".1"); i++ {
		if _, err := r.Write([]byte("more\n")); err != nil {
			t.Fatal(err)
		}
	}
	if !rlExists(path + ".1") {
		t.Fatal("rotation never retried after the lock went away")
	}
	if !strings.HasPrefix(rlReadFile(t, path+".1"), want.String()) {
		t.Fatal(".1 lost the lines written while rotation failed")
	}
	if len(*warnings) != 1 {
		t.Fatalf("%d warnings after recovery, want 1", len(*warnings))
	}
}

// The log file stays first in main's MultiWriter: a failing stdout does not
// keep a record out of the file.
func TestRotatingLogFirstInMultiWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	r, _ := newTestRotatingLog(t, path, 1<<20, 5, nil)
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(r, rlFailWriter{}), nil))
	logger.Info("still logged")
	if got := rlReadFile(t, path); !strings.Contains(got, "still logged") {
		t.Fatalf("current = %q", got)
	}
}

type rlFailWriter struct{}

func (rlFailWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestRotatingLogWriteAfterCloseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	r, _ := newTestRotatingLog(t, path, 100, 5, nil)
	_ = r.Close()
	if _, err := r.Write([]byte("late\n")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("err = %v, want os.ErrClosed", err)
	}
}

// rlSeedOld writes path.1 … path.n with the contents "old 1\n" … .
func rlSeedOld(t *testing.T, path string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if err := os.WriteFile(path+"."+strconv.Itoa(i), []byte(fmt.Sprintf("old %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// rlCheckFiles fails unless path.N holds want[N-1] for every N.
func rlCheckFiles(t *testing.T, path string, want []string) {
	t.Helper()
	for i, w := range want {
		p := path + "." + strconv.Itoa(i+1)
		if got := rlReadFile(t, p); got != w {
			t.Fatalf(".%d = %q, want %q", i+1, got, w)
		}
	}
	if rlExists(path + "." + strconv.Itoa(len(want)+1)) {
		t.Fatalf(".%d must not exist", len(want)+1)
	}
}

// Review R5: when the current file cannot be renamed (held open by another
// process on Windows), no numbered file is touched, however often the
// rotation is retried: the five old logs stay as they were.
func TestRotatingLogFailedRenameKeepsOldLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	rlSeedOld(t, path, 5)
	var tries int
	r, warnings := newTestRotatingLog(t, path, 100, 5, func(r *rotatingLog) {
		r.rename = func(oldpath, newpath string) error {
			if oldpath == path {
				tries++
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errors.New("file in use")}
			}
			return os.Rename(oldpath, newpath)
		}
	})
	var want strings.Builder
	for i := 0; i < 120; i++ { // 1200 bytes against a limit of 100
		line := fmt.Sprintf("line %03d\n\n", i) // 10 bytes
		want.WriteString(line)
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if tries < 5 {
		t.Fatalf("rotation tried %d times, want at least 5 retries", tries)
	}
	rlCheckFiles(t, path, []string{"old 1\n", "old 2\n", "old 3\n", "old 4\n", "old 5\n"})
	if rlExists(path + pendingSuffix) {
		t.Fatal("a pending file was left although the current file was never renamed")
	}
	if got := rlReadFile(t, path); got != want.String() {
		t.Fatalf("current file lost lines: %d bytes, want %d", len(got), want.Len())
	}
	if len(*warnings) != 1 {
		t.Fatalf("%d warnings, want 1", len(*warnings))
	}
}

// Review R5: a rotation that fails after the current file was renamed (here
// moving path.3 to path.4) loses nothing: the renamed file waits as
// path.rotating, retries drop no further old log, and once the move works
// the next rotation places it first — the result is that of two ordinary
// rotations.
func TestRotatingLogMidwayFailureLosesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	rlSeedOld(t, path, 5)
	locked := true
	r, warnings := newTestRotatingLog(t, path, 20, 5, func(r *rotatingLog) {
		r.rename = func(oldpath, newpath string) error {
			if locked && oldpath == path+".3" {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errors.New("file in use")}
			}
			return os.Rename(oldpath, newpath)
		}
	})
	write := func(s string) {
		t.Helper()
		if _, err := r.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	write("first 1\nfirst 2\n") // 16 bytes
	write("second\n")           // past 20: rotation, which fails at .3 → .4
	if got := rlReadFile(t, path+pendingSuffix); got != "first 1\nfirst 2\n" {
		t.Fatalf("pending = %q", got)
	}
	for i := 0; i < 20; i++ { // retries while .3 stays locked
		write("again\n")
	}
	if len(*warnings) != 1 {
		t.Fatalf("%d warnings, want 1", len(*warnings))
	}
	// Only the oldest log went, once (the first rotation's intended drop);
	// .4 is the gap the failed move left.
	for n, want := range map[int]string{1: "old 1\n", 2: "old 2\n", 3: "old 3\n", 5: "old 4\n"} {
		if got := rlReadFile(t, path+"."+strconv.Itoa(n)); got != want {
			t.Fatalf("while locked: .%d = %q, want %q", n, got, want)
		}
	}
	if rlExists(path + ".4") {
		t.Fatal(".4 should be the gap left by the failed move")
	}
	if got := rlReadFile(t, path+pendingSuffix); got != "first 1\nfirst 2\n" {
		t.Fatalf("pending after retries = %q", got)
	}
	current := rlReadFile(t, path)

	locked = false
	write("x\n")
	if rlExists(path + pendingSuffix) {
		t.Fatal("pending not placed after the lock went away")
	}
	rlCheckFiles(t, path, []string{current, "first 1\nfirst 2\n", "old 1\n", "old 2\n", "old 3\n"})
	if got := rlReadFile(t, path); got != "x\n" {
		t.Fatalf("current = %q", got)
	}
}
