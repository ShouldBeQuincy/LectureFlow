// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeSMIDirEnv set to a file: the test binary plays nvidia-smi's name
// query and writes its working directory to that file.
const fakeSMIDirEnv = "LF_DEVICES_FAKE_SMI_DIR"

func init() {
	out := os.Getenv(fakeSMIDirEnv)
	if out == "" {
		return
	}
	wd, err := os.Getwd()
	if err != nil || os.WriteFile(out, []byte(wd), 0o644) != nil {
		os.Exit(8)
	}
	fmt.Println("NVIDIA Fake GPU")
	os.Exit(0)
}

// SEC52 info-7: nvidia-smi runs in its own directory, not in the service's
// current directory (in web mode the folder LectureFlow was started from,
// which Windows searches for a DLL loaded by name). A bare name (the
// default, "nvidia-smi.exe") is looked up in PATH as before, a path is
// used as given; the name it reports is read as before.
func TestNvidiaNameRunsInItsOwnDir(t *testing.T) {
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
			t.Setenv(fakeSMIDirEnv, out)
			if c.path != "" {
				t.Setenv("PATH", c.path)
			}
			t.Chdir(t.TempDir()) // the service's current directory: elsewhere
			name, err := nvidiaName(tctx(), c.arg)
			if err != nil || name != "NVIDIA Fake GPU" {
				t.Fatalf("name %q, %v", name, err)
			}
			b, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if !samePath(string(b), filepath.Dir(exe)) {
				wd, _ := os.Getwd()
				t.Fatalf("nvidia-smi ran in %s, want its own directory %s (the service's is %s)", b, filepath.Dir(exe), wd)
			}
		})
	}
}
