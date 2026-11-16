// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// fakeLocator is a component manager with the asr-cuda environment and the
// python component's base environment installed.
type fakeLocator struct{}

const (
	compFWPython   = "C:/data/components/asr-cuda/1.0.0/venv/Scripts/python.exe"
	compBasePython = "C:/data/components/python/3.12.10/venv/Scripts/python.exe"
)

func (fakeLocator) Python(backend string) (string, bool) {
	switch backend {
	case model.BackendFasterWhisper:
		return compFWPython, true
	case config.ComponentBase:
		return compBasePython, true
	}
	return "", false
}

func (fakeLocator) ModelDir(string, string, string) (string, bool) { return "", false }

// TestDiagnosticsUseComponentPython (v3 Gate round 1): with the components
// installed, the Python version, the faster-whisper import and the CUDA
// self-test run the asr-cuda environment's interpreter, the pypdf import
// the base environment's; while a check runs SelfCheckPythons reports its
// interpreter.
func TestDiagnosticsUseComponentPython(t *testing.T) {
	var mu sync.Mutex
	used := map[string]string{} // check → program
	var e *env
	seen := make(chan []string, 1)
	e = newEnvWith(t, envConfig{server: func(s *Server) {
		s.cfg.Components = fakeLocator{}
		s.runCmd = func(ctx context.Context, name string, args ...string) (string, string, error) {
			key := ""
			switch {
			case len(args) == 1 && args[0] == "--version":
				key = "version"
			case len(args) == 3 && args[0] == "-I" && args[1] == "-c":
				key = args[2]
			case isScriptRun(args, "--selftest"):
				key = "selftest"
				select {
				case seen <- s.SelfCheckPythons():
				default:
				}
			}
			if key != "" {
				mu.Lock()
				used[key] = name
				mu.Unlock()
			}
			return fakeCmd(ctx, name, args...)
		}
	}})
	e.srv.diag.waitStartup()
	var sys System
	e.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	mu.Lock()
	defer mu.Unlock()
	want := map[string]string{
		"version":               compFWPython,
		"import faster_whisper": compFWPython,
		"import pypdf":          compBasePython,
		"selftest":              compFWPython,
	}
	for k, w := range want {
		if used[k] != w {
			t.Errorf("%s ran %q, want %q", k, used[k], w)
		}
	}
	for _, c := range sys.Checks {
		if (c.Name == "python" || c.Name == "faster-whisper" || c.Name == "pypdf" || c.Name == "cuda") && c.Status != statusOK {
			t.Errorf("%s: %+v", c.Name, c)
		}
	}
	select {
	case got := <-seen:
		if !slices.Contains(got, compFWPython) {
			t.Errorf("SelfCheckPythons during the CUDA self-test: %v", got)
		}
	case <-time.After(time.Second):
		t.Error("the CUDA self-test did not run")
	}
	if got := e.srv.SelfCheckPythons(); len(got) != 0 {
		t.Errorf("SelfCheckPythons after the checks: %v", got)
	}
}

// Without components the configured python is used, as before.
func TestDiagnosticsConfiguredPython(t *testing.T) {
	var mu sync.Mutex
	var programs []string
	e := newEnvWith(t, envConfig{server: func(s *Server) {
		s.runCmd = func(ctx context.Context, name string, args ...string) (string, string, error) {
			if len(args) > 0 && args[0] != "-version" {
				mu.Lock()
				programs = append(programs, name)
				mu.Unlock()
			}
			return fakeCmd(ctx, name, args...)
		}
	}})
	e.srv.diag.waitStartup()
	e.call("POST", "/api/system/recheck", nil, http.StatusOK, nil)
	mu.Lock()
	defer mu.Unlock()
	if len(programs) == 0 {
		t.Fatal("no python check ran")
	}
	for _, p := range programs {
		if p != e.cfg.Python {
			t.Errorf("ran %q, want the configured %q", p, e.cfg.Python)
		}
	}
}
