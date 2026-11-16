// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The diagnostics' Python calls (the import checks' `-I -c`, the
// self-tests' and the cuDNN check's `-s script`) run in the worker
// directory, not in the service's current directory (the folder the user
// started LectureFlow from in web mode): -I already keeps that off
// sys.path, but Windows still looks there for a DLL loaded by name (SEC47
// info-5). Other commands (ffprobe, ffmpeg, nvidia-smi, --version) keep
// the current directory.
func TestDiagnosticPythonCallsRunInWorkerDir(t *testing.T) {
	wd := t.TempDir()
	ctx := context.Background()
	for _, tc := range []struct {
		dir  string
		args []string
		want string
	}{
		{wd, []string{"-I", "-c", "import pypdf"}, wd},
		{wd, []string{"-s", filepath.Join(wd, "asr.py"), "--check-cudnn"}, wd},
		{wd, []string{"--version"}, ""},
		{wd, []string{"-hide_banner", "-loglevel", "error", "-i", "in.mp4"}, ""},
		{wd, []string{"-v", "error", "-show_format", "in.mp4"}, ""},
		{wd, nil, ""},
		{"worker", []string{"-I", "-c", "import pypdf"}, ""}, // a relative directory: not used
		{"", []string{"-s", "asr.py"}, ""},
	} {
		if got := diagCommand(ctx, tc.dir, "python", tc.args...).Dir; got != tc.want {
			t.Errorf("dir %q, args %q: Dir %q, want %q", tc.dir, tc.args, got, tc.want)
		}
	}

	// The server's own runner (Server.runCmd unless a test replaces it):
	// the fake worker of TestMain (fakeExtractEnv) creates started in the
	// directory it is given, relative here, so in its current directory,
	// and needs release there.
	probe := filepath.Join(wd, "cwd-probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probe, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeExtractEnv, "cwd-probe")
	e := newEnvWith(t, envConfig{server: func(s *Server) { s.cfg.WorkerDir = wd }})
	out := filepath.Join(t.TempDir(), "out.json")
	_, stderr, err := e.srv.runDiagnostic(ctx, exe, "-s", filepath.Join(wd, "extract_pdf.py"), "--out", out)
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(probe, "started")); err != nil {
		t.Fatalf("the Python call did not run in the worker directory: %v", err)
	}
}
