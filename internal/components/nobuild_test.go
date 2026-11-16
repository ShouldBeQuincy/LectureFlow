// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syncCalls returns the "uv pip sync" argument lists of a LF_FAKE_UV_LOG file.
func syncCalls(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if len(args) >= 2 && args[0] == "pip" && args[1] == "sync" {
			out = append(out, args)
		}
	}
	return out
}

// Security audit M-1: uv installs wheels only, from an index (online) and
// from the offline archive's wheels.
func TestSyncNoBuild(t *testing.T) {
	t.Run("online", func(t *testing.T) {
		e := newTestEnv(t)
		log := filepath.Join(t.TempDir(), "uv.log")
		t.Setenv("LF_FAKE_UV_LOG", log)
		m := e.open()
		mustInstall(t, m, Sources{}, IDASRCUDA)
		calls := syncCalls(t, log)
		if len(calls) != 2 { // python's base venv, asr-cuda
			t.Fatalf("sync calls: %q", calls)
		}
		for _, args := range calls {
			if !hasFlag(args, "--no-build") || !hasFlag(args, "--require-hashes") || flagValue(args, "--index-url") == "" {
				t.Errorf("sync args %q", args)
			}
		}
	})
	t.Run("offline", func(t *testing.T) {
		e, zips := offlineEnv(t)
		log := filepath.Join(t.TempDir(), "uv.log")
		t.Setenv("LF_FAKE_UV_LOG", log)
		m := e.open()
		for _, id := range []string{IDPython, IDASRCUDA} {
			if st := importAndWait(t, m, zips[id]); st.State != TaskDone {
				t.Fatalf("%s: %+v", id, st.Error)
			}
		}
		calls := syncCalls(t, log)
		if len(calls) != 2 {
			t.Fatalf("sync calls: %q", calls)
		}
		for _, args := range calls {
			if !hasFlag(args, "--no-build") || !hasFlag(args, "--offline") || !hasFlag(args, "--no-index") {
				t.Errorf("sync args %q", args)
			}
		}
	})
}

// An index that offers only source distributions: the install fails and no
// build backend runs (the fake uv stands for uv; the real uv 0.13.1 was
// checked by hand with a local sdist-only find-links directory).
func TestSdistOnlyIndexIsNotBuilt(t *testing.T) {
	e := newTestEnv(t)
	marker := filepath.Join(t.TempDir(), "built")
	t.Setenv("LF_FAKE_UV_SDIST_ONLY", marker)
	m := e.open()
	id, err := m.Install([]string{IDPython}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskFailed, TaskDone)
	if st.State != TaskFailed || st.Error.Code != CodeUVFailed || !strings.Contains(st.Error.Detail, "no usable wheels") {
		t.Fatalf("got %s %+v", st.State, st.Error)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the build backend ran: %v", err)
	}
	if _, ok := m.Python(BackendBase); ok {
		t.Error("python installed without its packages")
	}
}

func TestInsecureCustomSource(t *testing.T) {
	for _, c := range []struct {
		group, choice string
		want          bool
	}{
		{GroupPyPI, "http://pypi.example.com/simple", true},
		{GroupPyPI, "http://192.0.2.10:8080/simple", true},
		{GroupPyPI, "HTTP://pypi.example.com/simple", true},
		{GroupPyPI, "Http://192.0.2.10/simple", true},
		{GroupPyPI, "http://127.0.0.1.example.com/simple", true},
		{GroupPyPI, "http://localhost.example.com/simple", true},
		{GroupPyPI, "http://127.0.0.1@pypi.example.com/simple", true},
		{GroupPyPI, "http://[::ffff:192.0.2.10]/simple", true},
		{GroupPyPI, "  http://pypi.example.com/simple  ", true},
		{GroupPyPI, "https://pypi.example.com/simple", false},
		{GroupPyPI, "HTTPS://pypi.example.com/simple", false},
		{GroupPyPI, "http://127.0.0.1/simple", false},
		{GroupPyPI, "http://127.0.0.1:3141/simple", false},
		{GroupPyPI, "http://127.8.9.10/simple", false},
		{GroupPyPI, "http://[::1]/simple", false},
		{GroupPyPI, "http://[::1]:3141/simple", false},
		{GroupPyPI, "http://localhost/simple", false},
		{GroupPyPI, "http://LOCALHOST:3141/simple", false},
		{GroupPyPI, "tuna", false},
		{GroupPyPI, "", false},
		{GroupPyPI, SourceDefault, false},
		{GroupModels, "http://hf.example.com", false},
	} {
		if got := InsecureCustomSource(c.group, c.choice); got != c.want {
			t.Errorf("InsecureCustomSource(%q, %q) = %v, want %v", c.group, c.choice, got, c.want)
		}
	}
}

// A custom package index on plain http to another computer is refused when
// a task starts or retries, and again where uv would use it.
func TestInsecureCustomIndexRefused(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	bad := Sources{PyPI: "http://192.0.2.10/simple"}
	check := func(what string, err error) {
		t.Helper()
		info := ErrorInfoOf(err)
		if info.Code != CodeSourcesInvalid || info.Params["group"] != GroupPyPI || info.Params["field"] != "custom_url" {
			t.Errorf("%s: %v %+v", what, err, info)
		}
	}
	_, err := m.Install([]string{IDPython}, bad)
	check("install", err)
	_, err = m.UpdateFrom(IDPython, bad)
	check("update", err)
	man := e.manifest(defaultSpec())
	check("validate", man.ValidateSources(bad))
	_, err = man.indexURLs(bad)
	check("index urls", err)
	if len(m.Tasks()) != 0 {
		t.Errorf("tasks: %+v", m.Tasks())
	}
	// http for the models group and https / loopback http for packages pass
	for _, ok := range []Sources{{Models: "http://hf.example.com"}, {PyPI: "https://pypi.example.com/simple"},
		{PyPI: "http://localhost:3141/simple"}, {PyPI: "http://[::1]/simple"}} {
		if err := man.ValidateSources(ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	// an install from a package index on this computer works
	st := mustInstall(t, m, Sources{PyPI: e.url("/default/simple")}, IDPython)
	if st.State != TaskDone {
		t.Errorf("%+v", st)
	}
	// a retry of a failed task may not switch to an insecure index
	t.Setenv("LF_FAKE_UV_FAIL", "sync")
	id, err := m.Install([]string{IDASRCUDA}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed); st.State != TaskFailed {
		t.Fatalf("asr-cuda: %s", st.State)
	}
	check("retry", m.Retry(id, &bad))
}
