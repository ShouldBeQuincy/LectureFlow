// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/pyenv"
)

// pyArgsLog records the arguments of every Python check the diagnostics
// run with code (`-c`).
type pyArgsLog struct {
	mu   sync.Mutex
	args [][]string
}

func (l *pyArgsLog) run(ctx context.Context, name string, args ...string) (string, string, error) {
	if slices.Contains(args, "-c") {
		l.mu.Lock()
		l.args = append(l.args, append([]string(nil), args...))
		l.mu.Unlock()
	}
	return fakeCmd(ctx, name, args...)
}

// codes are the codes run; every run must be exactly `-I -c <code>`.
func (l *pyArgsLog) codes(t *testing.T) []string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, a := range l.args {
		if len(a) != 3 || a[0] != "-I" || a[1] != "-c" {
			t.Errorf("python %q, want -I -c <code>", a)
			continue
		}
		out = append(out, a[2])
	}
	return out
}

// The diagnostics' import checks run `python -I -c`: no PYTHONPATH and
// the like, no user site-packages, and not the service's current
// directory on sys.path (a pypdf.py in the folder lectureflow.exe was
// started from ran at start-up; security audit AUD4 L-4).
func TestDiagnosticImportChecksIsolated(t *testing.T) {
	log := &pyArgsLog{}
	e := newEnvWith(t, envConfig{server: func(s *Server) {
		s.cfg.Components = fakeLocator{}
		s.runCmd = log.run
	}})
	e.srv.diag.waitStartup()
	var sys System
	e.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	codes := log.codes(t)
	for _, want := range []string{"import faster_whisper", "import pypdf"} {
		if !slices.Contains(codes, want) {
			t.Errorf("did not run %q: %q", want, codes)
		}
	}
	for _, c := range sys.Checks {
		if (c.Name == "faster-whisper" || c.Name == "pypdf") && c.Status != statusOK {
			t.Errorf("%s: %+v", c.Name, c)
		}
	}
}

// The MLX import check too (macOS).
func TestDiagnosticMLXImportCheckIsolated(t *testing.T) {
	pinGOOS(t, "darwin")
	log := &pyArgsLog{}
	de := newDevEnv(t, envConfig{server: func(s *Server) { s.runCmd = log.run }})
	macDevices(de, true, false)
	var sys System
	de.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	if codes := log.codes(t); !slices.Contains(codes, mlxImport) {
		t.Errorf("did not run the MLX import: %q", codes)
	}
	if c := checksByName(sys)[checkMLX]; c.Status != statusOK {
		t.Errorf("%s: %+v", checkMLX, c)
	}
}

// pyRun is what the fake Python of TestMain (fakePDFLimitsWorker, mode
// "argsenv") received.
type pyRun struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

// pyEnvValue is the value env gives key (the last entry wins, as with
// os/exec; keys compared case-insensitively on Windows).
func pyEnvValue(env []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			val, found = v, true
		}
	}
	return val, found
}

// setPythonMarkers puts every variable of pyenv.Dropped (PYTHONPATH among
// them, to a directory with a json.py that would shadow the standard
// library) and a variable of LectureFlow's own into this process's
// environment.
func setPythonMarkers(t *testing.T) {
	t.Helper()
	marker := t.TempDir()
	if err := os.WriteFile(filepath.Join(marker, "json.py"), []byte("raise SystemExit('LF-MARKER imported')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, k := range pyenv.Dropped {
		t.Setenv(k, marker)
	}
	t.Setenv("LF_ISOLATION_MARKER", "kept")
}

// checkIsolatedEnv: none of pyenv.Dropped, pyenv's switches, the rest kept.
func checkIsolatedEnv(t *testing.T, what string, env []string) {
	t.Helper()
	for _, k := range pyenv.Dropped {
		if v, ok := pyEnvValue(env, k); ok {
			t.Errorf("%s: inherited %s=%s", what, k, v)
		}
	}
	if v, _ := pyEnvValue(env, "LF_ISOLATION_MARKER"); v != "kept" || !pyenv.Has(env) {
		t.Errorf("%s: environment %q", what, env)
	}
}

// The diagnostics' commands (runCommand: the self-tests and the cuDNN
// check of system.go and system_env.go) run without PYTHONPATH and the
// like, whatever arguments they get.
func TestRunCommandDropsPythonVariables(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	setPythonMarkers(t)
	t.Setenv(fakePDFLimitsEnv, "argsenv")
	out, _, err := runCommand(context.Background(), exe, "-s", "asr.py", "--selftest")
	if err != nil {
		t.Fatal(err)
	}
	var got pyRun
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	if !slices.Equal(got.Args, []string{"-s", "asr.py", "--selftest"}) {
		t.Errorf("args %q", got.Args)
	}
	checkIsolatedEnv(t, "runCommand", got.Env)
}

// The syllabus PDF extraction runs `python -s extract_pdf.py --in … --out
// …` without PYTHONPATH and the like (with UTF-8, unbuffered output).
func TestSyllabusExtractionIsolated(t *testing.T) {
	setPythonMarkers(t)
	e := pdfLimitsEnv(t, "argsenv")
	work := t.TempDir()
	pdf := filepath.Join(work, "syllabus.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4 fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	pages, err := e.srv.runExtractPDF(context.Background(), work, pdf)
	if err != nil || len(pages) != 1 {
		t.Fatalf("pages %q, %v", pages, err)
	}
	var got pyRun
	if err := json.Unmarshal([]byte(pages[0]), &got); err != nil {
		t.Fatalf("%q: %v", pages[0], err)
	}
	script, _ := filepath.Abs(filepath.Join(e.srv.cfg.WorkerDir, "extract_pdf.py"))
	if len(got.Args) != 6 || got.Args[0] != "-s" || got.Args[1] != script || got.Args[2] != "--in" || got.Args[4] != "--out" {
		t.Errorf("args %q, want -s %s --in … --out …", got.Args, script)
	}
	checkIsolatedEnv(t, "extract_pdf", got.Env)
	for k, want := range map[string]string{"PYTHONIOENCODING": "utf-8", "PYTHONUNBUFFERED": "1"} {
		if v, _ := pyEnvValue(got.Env, k); v != want {
			t.Errorf("%s=%q, want %q", k, v, want)
		}
	}
}
