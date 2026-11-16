// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package api

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// The working directory limit, measured: a process starts in a directory
// of workDirMax characters and not in one of workDirMax+1 (test-upgrade F10).
// Both limits come from MAX_PATH: a file path less its NUL (R6-E), a
// working directory less the NUL and the backslash Windows appends.
func TestWorkDirMaxOnWindows(t *testing.T) {
	if filePathMax != syscall.MAX_PATH-1 || workDirMax != syscall.MAX_PATH-2 {
		t.Fatalf("filePathMax %d, workDirMax %d; MAX_PATH %d", filePathMax, workDirMax, syscall.MAX_PATH)
	}
	exe, err := os.Executable() // this test binary: "-test.run=^$" exits at once
	if err != nil {
		t.Fatal(err)
	}
	try := func(n int) error {
		dir := longDir(t, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Dir = dir
		return cmd.Run()
	}
	if err := try(workDirMax); err != nil {
		t.Fatalf("%d characters: %v", workDirMax, err)
	}
	if err := try(workDirMax + 1); err == nil {
		t.Fatalf("%d characters started", workDirMax+1)
	}
}
