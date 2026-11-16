// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package pyenv

import (
	"slices"
	"strings"
	"testing"
)

// isolationDropped are the variables of the service's environment that no
// Python child inherits (security audit AUD4 L-4), written out again here;
// __PYVENV_LAUNCHER__ since SEC47 low-2 (even -I does not ignore it).
var isolationDropped = []string{
	"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONINSPECT", "PYTHONBREAKPOINT", "PYTHONWARNINGS",
	"PYTHONPYCACHEPREFIX", "PYTHONPLATLIBDIR", "PYTHONUSERBASE", "PYTHONEXECUTABLE", "PYTHONCASEOK",
	"PYTHON_PRESITE", "PYTHONSAFEPATH", "PYTHONVERBOSE", "PYTHONDEBUG", "PYTHONLEGACYWINDOWSFSENCODING",
	"VIRTUAL_ENV", "CONDA_PREFIX", "__PYVENV_LAUNCHER__",
}

// The list of Dropped is the one above, in full (a variable taken out of
// Dropped fails here, not only in the packages that start Python).
func TestDroppedList(t *testing.T) {
	for _, k := range isolationDropped {
		if !slices.Contains(Dropped, k) {
			t.Errorf("Dropped lacks %s", k)
		}
	}
	if got := Clean([]string{"__pyvenv_launcher__=C:\\elsewhere\\Scripts\\python.exe", "PATH=/bin"}); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Errorf("Clean kept the venv launcher variable: %q", got)
	}
}

// Env and Clean drop the variables whatever the case of their names
// (Windows reads them case-insensitively) and keep the rest, the drive
// entries of Windows ("=C:=C:\...") included; extra still sets what the
// caller wants.
func TestEnvDropsPythonVariables(t *testing.T) {
	base := []string{"=C:=C:\\work", "PATH=/bin", "PythonPath=/x", "pythonhome=/y", "PYTHONPATHX=kept",
		"MYPYTHONPATH=kept", "PYTHONUTF8=1", "HF_ENDPOINT=https://hf.example"}
	for _, k := range isolationDropped {
		base = append(base, k+"=/marker")
	}
	kept := []string{"=C:=C:\\work", "PATH=/bin", "PYTHONPATHX=kept", "MYPYTHONPATH=kept", "PYTHONUTF8=1", "HF_ENDPOINT=https://hf.example"}
	if got := Clean(base); !slices.Equal(got, kept) {
		t.Errorf("Clean:\n got %q\nwant %q", got, kept)
	}
	env := Env(base, "PYTHONIOENCODING=utf-8")
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if slices.ContainsFunc(isolationDropped, func(d string) bool { return strings.EqualFold(d, k) }) {
			t.Errorf("Env kept %q", kv)
		}
	}
	for _, kv := range kept {
		if !slices.Contains(env, kv) {
			t.Errorf("Env lost %q: %q", kv, env)
		}
	}
	if !Has(env) || env[len(env)-1] != "PYTHONIOENCODING=utf-8" {
		t.Errorf("Env: %q", env)
	}
	if got := Env(nil, "PYTHONNOUSERSITE=1", "VIRTUAL_ENV=/caller"); !slices.Contains(got, "VIRTUAL_ENV=/caller") {
		t.Errorf("extra is the caller's own: %q", got)
	}
}

func TestEnvironDropsPythonVariables(t *testing.T) {
	t.Setenv("PYTHONPATH", t.TempDir())
	t.Setenv("PYTHONSTARTUP", "start.py")
	t.Setenv("LF_PYENV_TEST", "kept")
	env := Environ()
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "PYTHONPATH=") || strings.HasPrefix(strings.ToUpper(kv), "PYTHONSTARTUP=") {
			t.Errorf("Environ kept %q", kv)
		}
	}
	if !slices.Contains(env, "LF_PYENV_TEST=kept") {
		t.Errorf("Environ: %q", env)
	}
}

// A script runs with -s (no user site-packages), never -I (which drops the
// script's directory from sys.path).
func TestScriptArgs(t *testing.T) {
	got := ScriptArgs("/w/asr.py", "--in", "a", "--out", "b")
	if want := []string{"-s", "/w/asr.py", "--in", "a", "--out", "b"}; !slices.Equal(got, want) {
		t.Fatalf("ScriptArgs = %q, want %q", got, want)
	}
	if got := ScriptArgs("x.py"); !slices.Equal(got, []string{"-s", "x.py"}) {
		t.Fatalf("ScriptArgs = %q", got)
	}
}
