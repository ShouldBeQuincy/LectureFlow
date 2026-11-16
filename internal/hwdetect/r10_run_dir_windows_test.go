// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package hwdetect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunDirEnv set to a file: the test binary plays nvidia-smi's query and
// writes its working directory to that file.
const fakeRunDirEnv = "LF_HWDETECT_FAKE_RUN_DIR"

func init() {
	out := os.Getenv(fakeRunDirEnv)
	if out == "" {
		return
	}
	wd, err := os.Getwd()
	if err != nil || os.WriteFile(out, []byte(wd), 0o644) != nil {
		os.Exit(8)
	}
	fmt.Println("NVIDIA Fake GPU, 8188, 566.14")
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
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// SEC52 info-7: the hardware detection's programs (nvidia-smi, PowerShell)
// run in their own directory, not in the service's current directory (in
// web mode the folder LectureFlow was started from, which Windows searches
// for a DLL loaded by name). A bare name is looked up in PATH as before;
// the output is returned as before.
func TestRunHiddenRunsInItsOwnDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		arg  string
		path string // PATH; "" = unchanged
	}{
		{"path", exe, ""},
		{"bare name in PATH", filepath.Base(exe), filepath.Dir(exe)},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "cwd.txt")
			t.Setenv(fakeRunDirEnv, out)
			if c.path != "" {
				t.Setenv("PATH", c.path)
			}
			t.Chdir(t.TempDir()) // the service's current directory: elsewhere
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			got, err := runHidden(ctx, c.arg, nvidiaSMIArgs...)
			if err != nil {
				t.Fatal(err)
			}
			if g := ParseNvidiaSMI(string(got)); len(g) != 1 || g[0].Name != "NVIDIA Fake GPU" || g[0].VRAMMB != 8188 {
				t.Fatalf("%q: %+v", got, g)
			}
			b, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if !sameDir(string(b), filepath.Dir(exe)) {
				wd, _ := os.Getwd()
				t.Fatalf("the program ran in %s, want its own directory %s (the service's is %s)", b, filepath.Dir(exe), wd)
			}
		})
	}
}
