// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A real Windows sharing violation: another handle on lectureflow.log
// without FILE_SHARE_DELETE (a viewer, an antivirus scan) makes the rename
// fail. The writer keeps appending and rotates once the handle is gone.
func TestRotatingLogWindowsFileHeldOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.log")
	r, warnings := newTestRotatingLog(t, path, 20, 5, nil)
	if _, err := r.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := fmt.Fprintf(r, "held %d\n", i); err != nil {
			syscall.CloseHandle(h)
			t.Fatalf("write while held: %v", err)
		}
	}
	syscall.CloseHandle(h)
	if rlExists(path + ".1") {
		t.Fatal("rename should have failed while the file was held open")
	}
	if len(*warnings) != 1 {
		t.Fatalf("%d warnings, want 1", len(*warnings))
	}
	if got := rlReadFile(t, path); !strings.HasSuffix(got, "held 7\n") {
		t.Fatalf("current = %q", got)
	}
	for i := 0; i < 10 && !rlExists(path+".1"); i++ {
		if _, err := r.Write([]byte("after\n")); err != nil {
			t.Fatal(err)
		}
	}
	if !rlExists(path + ".1") {
		t.Fatal("no rotation after the handle was closed")
	}
}
