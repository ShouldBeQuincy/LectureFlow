// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// SEC52 info-1: python_openvino given as a relative path with a directory
// part (config.Default; Load makes it absolute) names a file below the
// service's current directory. The probe runs in the worker directory, so
// it starts the interpreter by its absolute path: the system would
// otherwise look for it below the worker directory, and the check before
// (executableExists, from the current directory) would disagree with the
// start.
func TestProbeRelativeInterpreter(t *testing.T) {
	cfg := probeConfig(t)
	exe := cfg.PythonOpenVINO
	t.Chdir(filepath.Dir(filepath.Dir(exe)))
	cfg.PythonOpenVINO = filepath.Join(filepath.Base(filepath.Dir(exe)), filepath.Base(exe))
	r := NewRegistry(regStore(t), cfg, t.TempDir(), RegistryOptions{CUDAName: func(context.Context) (string, error) { return "", errors.New("none") }})
	r.Load(tctx())
	out := filepath.Join(t.TempDir(), "cwd.txt")
	t.Setenv(fakeProbeEnv, "cwd")
	t.Setenv(fakeProbeEnvOut, out)
	p, err := r.workerProbe(tctx())
	if err != nil || p.OpenVINO != "2026.4.1" {
		t.Fatalf("probe %+v, %v", p, err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(string(b), cfg.WorkerDir) {
		t.Fatalf("the probe ran in %s, want the worker directory %s", b, cfg.WorkerDir)
	}
}
