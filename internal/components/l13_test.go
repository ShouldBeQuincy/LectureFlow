// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Security audit L-13: hardening of receipts, reused downloads and the
// tools' working directory.

func TestExeOK(t *testing.T) {
	for _, c := range []struct {
		kind, exe string
		want      bool
	}{
		{KindPythonRuntime, "runtime/python/python.exe", true},
		{KindTool, "tool/bin/gcloud.cmd", true},
		{KindPythonRuntime, "", false},
		{KindTool, "", false},
		{KindPythonRuntime, "../evil/python.exe", false},
		{KindPythonRuntime, "runtime/../../evil/python.exe", false},
		{KindPythonRuntime, "/tmp/evil/python", false},
		{KindPythonRuntime, `C:\evil\python.exe`, false},
		{KindPythonRuntime, "C:/evil/python.exe", false},
		{KindPythonRuntime, "C:evil.exe", false},
		{KindPythonRuntime, `\\server\share\python.exe`, false},
		{KindPythonRuntime, "//server/share/python.exe", false},
		{KindPythonRuntime, `runtime\python.exe`, false},
		{KindPythonRuntime, "runtime/./python.exe", false},
		{KindTool, "tool//gcloud", false},
		{KindVenv, "", true},
		{KindModel, "", true},
		{KindModel, "../evil", false},
	} {
		if got := exeOK(Installed{Kind: c.kind, Exe: c.exe}); got != c.want {
			t.Errorf("exeOK(%s, %q) = %v, want %v", c.kind, c.exe, got, c.want)
		}
	}
}

// A receipt (and state.json) whose Exe leaves its directory is treated as
// damaged: the component counts as not installed, and installing what
// needs it reinstalls it instead of running the named program.
func TestReceiptExeOutsideItsDirectory(t *testing.T) {
	abs := "/tmp/evil/python"
	if runtime.GOOS == "windows" {
		abs = `C:\evil\python.exe`
	}
	for i, bad := range []string{"../../evil/python.exe", "runtime/../../../evil/python.exe", abs, "C:evil/python.exe",
		`\\evil-server\share\python.exe`, "//evil-server/share/python.exe", ""} {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) { // the name stays out of the temporary paths
			t.Logf("exe %q", bad)
			e := newTestEnv(t)
			m := e.open()
			mustInstall(t, m, Sources{}, IDPython)
			m.Close()

			root := filepath.Join(e.data, "components")
			statePath := filepath.Join(root, stateFileName)
			var st map[string]any
			data, _ := os.ReadFile(statePath)
			if err := json.Unmarshal(data, &st); err != nil {
				t.Fatal(err)
			}
			py := st["active"].(map[string]any)[IDPython].(map[string]any)
			py["exe"] = bad
			b, _ := json.Marshal(st)
			os.WriteFile(statePath, b, 0o644)
			receipt := filepath.Join(root, filepath.FromSlash(py["dir"].(string)), receiptFileName)
			var r map[string]any
			data, _ = os.ReadFile(receipt)
			json.Unmarshal(data, &r)
			r["exe"] = bad
			b, _ = json.Marshal(r)
			os.WriteFile(receipt, b, 0o644)

			log := filepath.Join(t.TempDir(), "uv.log")
			t.Setenv("LF_FAKE_UV_LOG", log)
			m2 := e.open()
			if s, _ := statusOf(m2, IDPython); s.Installed {
				t.Fatalf("python with exe %q counts as installed", bad)
			}
			if aside, _ := filepath.Glob(statePath + ".corrupt-*"); len(aside) != 1 {
				t.Errorf("state.json moved aside: %v", aside)
			}
			mustInstall(t, m2, Sources{}, IDASRCUDA)
			calls, _ := os.ReadFile(log)
			if bytes.Contains(calls, []byte("evil")) {
				t.Errorf("uv got the receipt's exe: %s", calls)
			}
			if s, _ := statusOf(m2, IDPython); !s.Installed {
				t.Error("python was not installed again")
			}
		})
	}
}

// A downloaded file with the right name and size but other content is
// not reused: it is deleted and downloaded again.
func TestDownloadedFileRehashedBeforeReuse(t *testing.T) {
	e := newTestEnv(t)
	want := ovContent(e)
	dl := filepath.Join(e.data, "components", "downloads")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := bytes.Repeat([]byte{'x'}, len(want))
	if err := os.WriteFile(filepath.Join(dl, shaHex(want)), fake, 0o644); err != nil {
		t.Fatal(err)
	}
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	checkModelFile(t, m, want)
	if n := e.srv.count(ovFile); n != 1 {
		t.Errorf("%d downloads of the model, want 1", n)
	}
}

// An intact downloaded file is still reused without a download.
func TestDownloadedFileReusedWhenIntact(t *testing.T) {
	e := newTestEnv(t)
	want := ovContent(e)
	dl := filepath.Join(e.data, "components", "downloads")
	os.MkdirAll(dl, 0o755)
	if err := os.WriteFile(filepath.Join(dl, shaHex(want)), want, 0o644); err != nil {
		t.Fatal(err)
	}
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	checkModelFile(t, m, want)
	if n := e.srv.count(ovFile); n != 0 {
		t.Errorf("%d downloads of an intact file", n)
	}
}

func TestFileMatches(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	content := []byte("hello")
	os.WriteFile(p, content, 0o644)
	ctx := context.Background()
	if ok, err := fileMatches(ctx, p, 5, strings.ToUpper(shaHex(content))); !ok || err != nil {
		t.Errorf("match: %v %v", ok, err)
	}
	if ok, _ := fileMatches(ctx, p, 5, shaHex([]byte("world"))); ok {
		t.Error("other hash matched")
	}
	if ok, _ := fileMatches(ctx, p, 6, shaHex(content)); ok {
		t.Error("other size matched")
	}
	if ok, err := fileMatches(ctx, filepath.Join(t.TempDir(), "missing"), 5, shaHex(content)); ok || err != nil {
		t.Errorf("missing: %v %v", ok, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if ok, err := fileMatches(cctx, p, 5, shaHex(content)); ok || err == nil {
		t.Errorf("cancelled: %v %v", ok, err)
	}
}

// The tool's check runs in a new, empty directory below the temporary
// directory, removed afterwards.
func TestToolRunsInFreshEmptyDir(t *testing.T) {
	e := newTestEnv(t)
	e.writeToolBuiltin(defaultToolSpec(t))
	log := logEnv(t)
	tmp := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(k, tmp)
	}
	if !sameRealPath(os.TempDir(), tmp) {
		t.Fatalf("os.TempDir() = %q does not follow TMP", os.TempDir())
	}
	m := e.open()
	mustInstall(t, m, Sources{}, IDGcloud)
	got, runs := gcloudLog(t, log)
	if runs != 1 {
		t.Fatalf("runs: %d", runs)
	}
	cwd := got["cwd"]
	if !sameRealPath(filepath.Dir(cwd), tmp) || !strings.HasPrefix(filepath.Base(cwd), "lectureflow-tool-") {
		t.Errorf("working directory %q, want a new directory in %q", cwd, tmp)
	}
	if got["cwdempty"] != "yes" {
		t.Errorf("the working directory was not empty: %q", got["cwdempty"])
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Errorf("the working directory is left: %v", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left in the temporary directory: %v", left)
	}
}

func sameRealPath(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	if err1 == nil {
		a = ea
	}
	if err2 == nil {
		b = eb
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
