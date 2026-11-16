// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeSMIDirEnv set to a file: the test binary plays nvidia-smi's sample
// query and writes its working directory to that file.
const fakeSMIDirEnv = "LF_RESOURCES_FAKE_SMI_DIR"

func init() {
	out := os.Getenv(fakeSMIDirEnv)
	if out == "" {
		return
	}
	wd, err := os.Getwd()
	if err != nil || os.WriteFile(out, []byte(wd), 0o644) != nil {
		os.Exit(8)
	}
	fmt.Println("NVIDIA Fake GPU, 7, 3000, 12227, 45, 5.88, 90.00")
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

// SEC52 info-7: the resource sampler's nvidia-smi (execRun, the default
// RunCmd) runs in nvidia-smi's own directory, not in the service's current
// directory (in web mode the folder LectureFlow was started from, which
// Windows searches for a DLL loaded by name). Its output is returned as
// before.
func TestExecRunRunsInItsOwnDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "cwd.txt")
	t.Setenv(fakeSMIDirEnv, out)
	t.Chdir(t.TempDir()) // the service's current directory: elsewhere
	got, err := execRun(context.Background(), exe, QueryArgs...)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := ParseQuery(got); err != nil || s.Name != "NVIDIA Fake GPU" || intOrNil(s.MemTotalMB) != 12227 {
		t.Fatalf("%q: %+v, %v", got, s, err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !sameDir(string(b), filepath.Dir(exe)) {
		wd, _ := os.Getwd()
		t.Fatalf("nvidia-smi ran in %s, want its own directory %s (the service's is %s)", b, filepath.Dir(exe), wd)
	}
}
