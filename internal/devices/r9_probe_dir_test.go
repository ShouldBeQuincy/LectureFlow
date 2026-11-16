// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// samePath compares two directories as the file system does.
func samePath(a, b string) bool {
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

// SEC50 info-2 / SEC47 info-5: the OpenVINO probe runs in the directory of
// its script (the worker directory, the application's own), not in the
// service's current directory, which the Windows DLL search would consult
// when openvino loads its native libraries. The arguments (-s) and the
// filtered environment are unchanged (TestProbeRunsIsolated).
func TestProbeRunsInWorkerDir(t *testing.T) {
	cfg := probeConfig(t)
	r := NewRegistry(regStore(t), cfg, t.TempDir(), RegistryOptions{CUDAName: func(context.Context) (string, error) { return "", errors.New("none") }})
	r.Load(tctx())
	out := filepath.Join(t.TempDir(), "cwd.txt")
	t.Setenv(fakeProbeEnv, "cwd")
	t.Setenv(fakeProbeEnvOut, out)
	if _, err := r.workerProbe(tctx()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !samePath(got, cfg.WorkerDir) {
		wd, _ := os.Getwd()
		t.Fatalf("the probe ran in %s, want the worker directory %s (the test's own directory is %s)", got, cfg.WorkerDir, wd)
	}
}

// A relative worker_dir (config.Default before Load makes it absolute) still
// finds the script: the probe passes it as an absolute path, since the
// child's working directory is no longer the service's.
func TestProbeRelativeWorkerDir(t *testing.T) {
	cfg := probeConfig(t)
	t.Chdir(filepath.Dir(cfg.WorkerDir))
	rel := filepath.Base(cfg.WorkerDir)
	cfg.WorkerDir = rel
	r := NewRegistry(regStore(t), cfg, t.TempDir(), RegistryOptions{CUDAName: func(context.Context) (string, error) { return "", errors.New("none") }})
	r.Load(tctx())
	out := filepath.Join(t.TempDir(), "cwd.txt")
	t.Setenv(fakeProbeEnv, "cwd")
	t.Setenv(fakeProbeEnvOut, out)
	if _, err := r.workerProbe(tctx()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if abs, _ := filepath.Abs(rel); !samePath(string(b), abs) {
		t.Fatalf("the probe ran in %s, want %s", b, abs)
	}
}
