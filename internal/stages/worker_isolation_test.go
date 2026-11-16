// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/pyenv"
)

// isolationDropped are variables of the service's environment that make
// Python load or run code from elsewhere, break the worker's imports from
// its own directory, garble its output or point at another environment
// (security audit AUD4 L-4); none reaches a worker.
var isolationDropped = []string{
	"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONINSPECT", "PYTHONBREAKPOINT", "PYTHONWARNINGS",
	"PYTHONPYCACHEPREFIX", "PYTHONPLATLIBDIR", "PYTHONUSERBASE", "PYTHONEXECUTABLE", "PYTHONCASEOK",
	"PYTHON_PRESITE", "PYTHONSAFEPATH", "PYTHONVERBOSE", "PYTHONDEBUG", "PYTHONLEGACYWINDOWSFSENCODING",
	"VIRTUAL_ENV", "CONDA_PREFIX",
}

// envValue is the value env gives key (the last entry wins, as with
// os/exec; keys compared case-insensitively on Windows).
func envValue(env []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			val, found = v, true
		}
	}
	return val, found
}

// A worker runs as `python -s <script> --in … --out …` (no user
// site-packages; not -I, which would drop the script's directory from
// sys.path and break the worker's imports of its siblings) and without the
// variables of isolationDropped, a PYTHONPATH with a module that shadows
// the standard library included; its own variables, pyenv's and the
// others stay.
func TestWorkerRunsIsolatedFromPythonVariables(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("argsenv", "")
	marker := t.TempDir()
	if err := os.WriteFile(filepath.Join(marker, "json.py"), []byte("raise SystemExit('LF-MARKER imported')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, k := range isolationDropped {
		t.Setenv(k, marker)
	}
	t.Setenv("PYTHONPATH", marker)
	t.Setenv("PYTHONINSPECT", "1")
	t.Setenv("PYTHONSAFEPATH", "1")
	t.Setenv("PYTHONIOENCODING", "latin-1")
	t.Setenv("PYTHONUNBUFFERED", "0")
	t.Setenv("PYTHONUTF8", "1")
	t.Setenv("HF_HOME", marker)
	t.Setenv("LF_ISOLATION_MARKER", "kept")
	for _, name := range []string{"asr", "asr_openvino", "extract_pdf"} {
		dir := filepath.Join(t.TempDir(), name)
		w := workerCall{name: name, label: "x", code: workerASR, dir: dir, job: map[string]any{}, out: "result.json"}
		raw, err := e.deps(nil).runWorker(e.ctx, w)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got struct {
			Args []string `json:"args"`
			Env  []string `json:"env"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		script, _ := filepath.Abs(filepath.Join(e.cfg.WorkerDir, name+".py"))
		if len(got.Args) != 6 || got.Args[0] != "-s" || got.Args[1] != script || got.Args[2] != "--in" ||
			got.Args[3] != filepath.Join(dir, "job.json") || got.Args[4] != "--out" {
			t.Errorf("%s: args %q, want -s %s --in … --out …", name, got.Args, script)
		}
		for _, k := range isolationDropped {
			if v, ok := envValue(got.Env, k); ok {
				t.Errorf("%s: inherited %s=%s", name, k, v)
			}
		}
		for k, want := range map[string]string{"PYTHONIOENCODING": "utf-8", "PYTHONUNBUFFERED": "1", "PYTHONUTF8": "1",
			"HF_HOME": marker, "LF_ISOLATION_MARKER": "kept"} {
			if v, ok := envValue(got.Env, k); !ok || v != want {
				t.Errorf("%s: %s=%q (set %v), want %q", name, k, v, ok, want)
			}
		}
		if !pyenv.Has(got.Env) {
			t.Errorf("%s: without pyenv's switches: %q", name, got.Env)
		}
	}
}
