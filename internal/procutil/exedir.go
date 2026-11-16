// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package procutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

// CommandInExeDir is exec.CommandContext for a helper program whose
// arguments hold no relative path (nvidia-smi, the hardware detection's
// PowerShell): it runs in the directory of its own executable, not in this
// process's current directory, which in web mode is whatever folder
// LectureFlow was started from and which Windows searches when a program
// loads a DLL by name (SEC52 info-7; the aria2c of internal/fetch runs the
// same way). The executable is resolved first, so the new working directory
// does not change which file runs: a bare name is looked up in PATH
// (exec.Command), a relative path with a directory part is made absolute
// against the current directory, as it was run before. On Windows the
// look-up does find a file of that name in the current directory (unless
// NoDefaultCurrentDirectoryInExePath is set), and a relative PATH entry
// finds one on every system, but exec.LookPath answers exec.ErrDot for
// such a file (on Windows also when PATH names a different one): that
// error is kept, no directory is set, and Start refuses to run it (SEC53).
// A program that cannot be resolved, or whose directory does not exist, is
// left as exec.CommandContext makes it, so it fails with the same error as
// before.
func CommandInExeDir(ctx context.Context, name string, args ...string) *exec.Cmd {
	if filepath.Base(name) != name && !filepath.IsAbs(name) {
		if abs, err := filepath.Abs(name); err == nil {
			name = abs
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if cmd.Err != nil || !filepath.IsAbs(cmd.Path) {
		return cmd
	}
	if dir := filepath.Dir(cmd.Path); isDir(dir) {
		cmd.Dir = dir
	}
	return cmd
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
