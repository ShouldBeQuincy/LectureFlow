// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// dirSize's cost (SEC43b low-6): it runs every Options.SpaceInterval in
// diskWatch's lock while aria2c writes, and a task of many segments has
// tens of thousands of files. Only the files aria2c is writing (*.part)
// are measured one by one; the others' sizes come with the listing.

// manySegments makes n small complete segments and one partial download
// under dir and returns the bytes they hold.
func manySegments(t *testing.T, dir string, n int) int64 {
	t.Helper()
	sub := filepath.Join(dir, "t0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	seg := make([]byte, 64)
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("s%06d.seg", i+1)), seg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "t1.m4a.part"), make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	return int64(n*len(seg) + 5000)
}

// Twenty thousand segments: one look takes well under the interval (it
// took 0.38-0.50 s with a system call per file on Windows), and only the
// partial download is measured on its own.
func TestDirSizeManySegments(t *testing.T) {
	const n = 20_000
	dir := t.TempDir()
	start := time.Now()
	want := manySegments(t, dir, n)
	made := time.Since(start)
	var lstats []string
	lstatFile = func(p string) (os.FileInfo, error) {
		lstats = append(lstats, filepath.Base(p))
		return os.Lstat(p)
	}
	t.Cleanup(func() { lstatFile = os.Lstat })
	best := time.Hour
	for k := 0; k < 3; k++ {
		lstats = nil
		begin := time.Now()
		got := dirSize(dir)
		best = min(best, time.Since(begin))
		if got != want {
			t.Fatalf("dirSize = %d, want %d", got, want)
		}
		if len(lstats) != 1 || lstats[0] != "t1.m4a.part" {
			t.Fatalf("measured one by one: %d files (%.5q), want the partial download alone", len(lstats), lstats)
		}
	}
	t.Logf("dirSize of %d files: best of 3 %v (making them: %v)", n+1, best, made)
	// On Windows the listing's sizes come without a system call per file
	// (elsewhere DirEntry.Info is an lstat, as before: the count above is
	// the check there)
	if runtime.GOOS == "windows" && best > 50*time.Millisecond {
		t.Fatalf("dirSize of %d files took %v, want under 50 ms", n+1, best)
	}
}

// A partial download still being written (the handle open, as aria2c
// keeps it) is counted at the size written so far (SEC43 low-2). Written
// by the same handle the listing may well be right too; the test that
// sees the listing lag behind the fake aria2c's files is
// TestTaskTooLargeAcrossTracks (it fails when *.part is not measured on
// its own).
func TestDirSizeCountsPartialBeingWritten(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plan.id"), []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "t0.mp4.part"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	chunk := make([]byte, 256<<10)
	var written int64
	for k := 0; k < 8; k++ {
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
		written += int64(len(chunk))
		if got := dirSize(dir); got != written+16 {
			t.Fatalf("after %d bytes written: dirSize = %d, want %d", written, got, written+16)
		}
	}
}
