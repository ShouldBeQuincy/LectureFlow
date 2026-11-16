// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/dirperm"
)

// SEC50 info-1: the server built the way the application builds it
// (newServer without a tweak, as New does) runs its diagnostic commands
// through runDiagnostic, so a Python call runs in the worker directory.
// newEnvWith replaces runCmd with a fake, and TestDiagnosticPythonCallsRun-
// InWorkerDir calls runDiagnostic directly; neither notices when newServer
// wires runCmd to runCommand instead.
func TestNewServerRunsDiagnosticPythonInWorkerDir(t *testing.T) {
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if _, err := dirperm.MkdirPrivate(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	// The startup checks run with the real runner too: no program of the
	// configuration exists, there is no model service, and PATH holds
	// nothing, so they start no program and reach no network.
	missing := filepath.Join(dir, "missing")
	cfg.FFmpeg = filepath.Join(missing, "ffmpeg")
	cfg.Aria2c = filepath.Join(missing, "aria2c")
	cfg.Python = filepath.Join(missing, "python")
	cfg.PythonOpenVINO = filepath.Join(missing, "python-ov")
	cfg.PythonMLX = filepath.Join(missing, "python-mlx")
	cfg.LLM.Providers = nil
	t.Setenv("PATH", missing)

	worker := t.TempDir()
	cfg.WorkerDir = worker
	// The fake worker of TestMain (fakeExtractEnv) creates started in the
	// directory it is given, relative here, so in its working directory,
	// and needs release there.
	probe := filepath.Join(worker, "cwd-probe")
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

	srv := newServer(Deps{Store: st, Cfg: cfg, DataDir: cfg.DataDir}, nil)
	t.Cleanup(func() {
		if !srv.StopStartupChecks(30 * time.Second) {
			t.Error("the startup checks did not end")
		}
		srv.Hub().Close()
	})

	t.Setenv(fakeExtractEnv, "cwd-probe")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out := filepath.Join(t.TempDir(), "out.json")
	_, stderr, err := srv.runCmd(ctx, exe, "-s", filepath.Join(worker, "extract_pdf.py"), "--out", out)
	if err != nil {
		t.Fatalf("the Python call failed (not run in the worker directory?): %v %s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(probe, "started")); err != nil {
		t.Fatalf("the Python call did not run in the worker directory: %v", err)
	}
}
