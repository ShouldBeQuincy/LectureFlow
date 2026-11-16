// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/pyenv"
)

// SEC50 info-2 / SEC47 info-5: the components' import check runs in the
// worker directory (the application's own), not in the service's current
// directory, which the Windows DLL search would consult when the imports
// load native extensions. Without a worker directory it runs in the
// interpreter's own directory, never in the service's. -I and pyenv's
// environment are unchanged.
func TestImportCheckRunsInWorkerDir(t *testing.T) {
	py := filepath.Join(t.TempDir(), "env", "Scripts", "python.exe")
	worker := filepath.Join(t.TempDir(), "worker")
	cmd := importCheckCmd(context.Background(), py, worker, []string{"openvino", "openvino_genai"})
	if cmd.Dir != worker {
		t.Errorf("dir %q, want the worker directory %q", cmd.Dir, worker)
	}
	if len(cmd.Args) != 4 || cmd.Args[0] != py || cmd.Args[1] != "-I" || cmd.Args[2] != "-c" ||
		!strings.HasPrefix(cmd.Args[3], pyenv.BlockTelemetryModules+";") {
		t.Errorf("args %q", cmd.Args)
	}
	if !pyenv.Has(cmd.Env) {
		t.Errorf("env without %q: %q", pyenv.TelemetryOff, cmd.Env)
	}
	if cmd := importCheckCmd(context.Background(), py, "", []string{"os"}); cmd.Dir != filepath.Dir(py) {
		t.Errorf("without a worker directory: dir %q, want the interpreter's %q", cmd.Dir, filepath.Dir(py))
	}
}

// The update self-test passes the configured worker directory to the
// import check.
func TestComponentSelfTestImportCheckGetsWorkerDir(t *testing.T) {
	e := newEnv(t)
	e.cfg.WorkerDir = filepath.Join(e.root, "worker")
	d := e.deps(nil)
	gotDir := "unset"
	prev := componentImportCheck
	t.Cleanup(func() { componentImportCheck = prev })
	componentImportCheck = func(_ context.Context, _, dir string, _ []string) error {
		gotDir = dir
		return nil
	}
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "asr-openvino", Backend: model.BackendOpenVINO, Python: "/c/ov/python"}); err != nil {
		t.Fatal(err)
	}
	if gotDir != e.cfg.WorkerDir {
		t.Fatalf("import check dir %q, want %q", gotDir, e.cfg.WorkerDir)
	}
}
