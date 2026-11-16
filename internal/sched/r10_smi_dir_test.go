// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeSMIDirEnv set to a file: the test binary plays nvidia-smi's memory
// and compute capability queries and writes its working directory to that
// file.
const fakeSMIDirEnv = "LF_SCHED_FAKE_SMI_DIR"

func init() {
	out := os.Getenv(fakeSMIDirEnv)
	if out == "" {
		return
	}
	wd, err := os.Getwd()
	if err != nil || os.WriteFile(out, []byte(wd), 0o644) != nil {
		os.Exit(8)
	}
	if slices.Contains(os.Args[1:], "--query-gpu=compute_cap") {
		fmt.Println("8.9")
	} else {
		fmt.Println("9000, 12000, 3000")
	}
	os.Exit(0)
}

// sameDir compares two directories as the file system does.
func sameDir(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// SEC52 info-7: the scheduler's nvidia-smi queries (free memory, compute
// capability) run in nvidia-smi's own directory, not in the service's
// current directory (in web mode the folder LectureFlow was started from,
// which Windows searches for a DLL loaded by name). Their answers are read
// as before.
func TestNvidiaSMIQueriesRunInTheirOwnDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ran := func(t *testing.T, out string) {
		t.Helper()
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if !sameDir(string(b), filepath.Dir(exe)) {
			wd, _ := os.Getwd()
			t.Fatalf("nvidia-smi ran in %s, want its own directory %s (the service's is %s)", b, filepath.Dir(exe), wd)
		}
	}
	t.Run("memory", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "cwd.txt")
		t.Setenv(fakeSMIDirEnv, out)
		t.Chdir(t.TempDir()) // the service's current directory: elsewhere
		m, err := NvidiaSMIQuery(exe)(context.Background())
		if err != nil || m != (GPUMem{FreeMB: 9000, TotalMB: 12000, UsedMB: 3000}) {
			t.Fatalf("%+v, %v", m, err)
		}
		ran(t, out)
	})
	t.Run("compute capability", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "cwd.txt")
		t.Setenv(fakeSMIDirEnv, out)
		t.Chdir(t.TempDir())
		v, err := NvidiaSMIComputeCap(exe)(context.Background())
		if err != nil || v != "8.9" {
			t.Fatalf("%q, %v", v, err)
		}
		ran(t, out)
	})
}
