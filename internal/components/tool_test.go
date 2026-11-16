// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Tool components (3.1.0, v31-brief 2.1): the gcloud archive is faked by a
// zip whose bin/gcloud.cmd (Windows) and bin/gcloud (sh) behave like
// "gcloud --version". Switches (inherited environment variables):
//
//	LF_FAKE_GCLOUD_LOG=file    append the arguments and the CLOUDSDK_* / PYTHONPATH values, the working directory (cwd) and whether it is empty (cwdempty)
//	LF_FAKE_GCLOUD_FAIL=1      exit 1 with "ERROR: gcloud crashed"
//	LF_FAKE_GCLOUD_GATE=file   wait until the file exists (pause / cancel / close during the check)
//
// The script writes CLOUDSDK_CONFIG/created.txt like gcloud creating its
// configuration directory, so the tests see whether the check cleans up.

const fakeGcloudCmd = "@echo off\r\n" +
	"if not defined LF_FAKE_GCLOUD_LOG goto nolog\r\n" +
	">>\"%LF_FAKE_GCLOUD_LOG%\" echo args=%*\r\n" +
	">>\"%LF_FAKE_GCLOUD_LOG%\" echo python=%CLOUDSDK_PYTHON%\r\n" +
	">>\"%LF_FAKE_GCLOUD_LOG%\" echo config=%CLOUDSDK_CONFIG%\r\n" +
	">>\"%LF_FAKE_GCLOUD_LOG%\" echo update=%CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK%\r\n" +
	">>\"%LF_FAKE_GCLOUD_LOG%\" echo pythonpath=%PYTHONPATH%\r\n" +
	">>\"%LF_FAKE_GCLOUD_LOG%\" echo cwd=%CD%\r\n" +
	"set LFCWDEMPTY=yes\r\n" +
	"for /f \"delims=\" %%f in ('dir /b /a 2^>nul') do set LFCWDEMPTY=no\r\n" +
	">>\"%LF_FAKE_GCLOUD_LOG%\" echo cwdempty=%LFCWDEMPTY%\r\n" +
	":nolog\r\n" +
	"if not defined CLOUDSDK_CONFIG goto noconfig\r\n" +
	">\"%CLOUDSDK_CONFIG%\\created.txt\" echo created\r\n" +
	":noconfig\r\n" +
	":wait\r\n" +
	"if not defined LF_FAKE_GCLOUD_GATE goto run\r\n" +
	"if exist \"%LF_FAKE_GCLOUD_GATE%\" goto run\r\n" +
	"ping -n 2 127.0.0.1 >nul\r\n" +
	"goto wait\r\n" +
	":run\r\n" +
	"if \"%LF_FAKE_GCLOUD_FAIL%\"==\"1\" goto fail\r\n" +
	"echo Google Cloud SDK 1.0.0\r\n" +
	"exit /b 0\r\n" +
	":fail\r\n" +
	"echo ERROR: gcloud crashed 1>&2\r\n" +
	"exit /b 1\r\n"

const fakeGcloudSh = `#!/bin/sh
if [ -n "$LF_FAKE_GCLOUD_LOG" ]; then
  {
    echo "args=$*"
    echo "python=$CLOUDSDK_PYTHON"
    echo "config=$CLOUDSDK_CONFIG"
    echo "update=$CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK"
    echo "pythonpath=$PYTHONPATH"
    echo "cwd=$(pwd -P)"
    if [ -z "$(ls -A)" ]; then echo "cwdempty=yes"; else echo "cwdempty=no"; fi
  } >> "$LF_FAKE_GCLOUD_LOG"
fi
if [ -n "$CLOUDSDK_CONFIG" ]; then echo created > "$CLOUDSDK_CONFIG/created.txt"; fi
if [ -n "$LF_FAKE_GCLOUD_GATE" ]; then
  while [ ! -e "$LF_FAKE_GCLOUD_GATE" ]; do sleep 0.1; done
fi
if [ "$LF_FAKE_GCLOUD_FAIL" = 1 ]; then echo "ERROR: gcloud crashed" >&2; exit 1; fi
echo "Google Cloud SDK 1.0.0"
`

// hostGcloudExe is the fake's executable for the machine running the
// tests (the manager's platform is windows-x64 in every test).
func hostGcloudExe() string {
	if runtime.GOOS == "windows" {
		return "bin/gcloud.cmd"
	}
	return "bin/gcloud"
}

// zipEntry is one entry of a zip built by makeModeZip.
type zipEntry struct {
	name string
	body string
	mode os.FileMode // 0: 0644; os.ModeSymlink|...: body is the target
}

func makeModeZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		if strings.HasSuffix(e.name, "/") {
			mode = os.ModeDir | 0o755
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e.body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeGcloudZip is a google-cloud-sdk/ archive like Google's.
func fakeGcloudZip(t *testing.T, readme string) []byte {
	return makeModeZip(t, []zipEntry{
		{name: "google-cloud-sdk/"},
		{name: "google-cloud-sdk/README", body: readme},
		{name: "google-cloud-sdk/bin/gcloud.cmd", body: fakeGcloudCmd, mode: 0o755},
		{name: "google-cloud-sdk/bin/gcloud", body: fakeGcloudSh, mode: 0o755},
		{name: "google-cloud-sdk/lib/gcloud.py", body: "# gcloud"},
		{name: "google-cloud-sdk/docs/index.rst", body: "../README", mode: os.ModeSymlink | 0o777},
		{name: "google-cloud-sdk/certs/expired/ca/ca.crt", body: "cert"},
		{name: "google-cloud-sdk/certs/valid/ca", body: "../expired/ca", mode: os.ModeSymlink | 0o777},
	})
}

type toolSpec struct {
	version  string
	archive  []byte
	depends  []string          // the build's own dependencies
	env      map[string]string // Build.Env
	offline  *Offline
	maxPath  int
	serial   int64
	checkEnv map[string]string
}

func defaultToolSpec(t *testing.T) toolSpec {
	return toolSpec{version: "1.0.0", archive: fakeGcloudZip(t, "gcloud 1.0.0"), serial: 1,
		env: map[string]string{
			"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK": "true",
			"CLOUDSDK_PYTHON": "{dir}/platform/bundledpython/python.exe",
		},
		checkEnv: map[string]string{"CLOUDSDK_CONFIG": "{tmp}"}}
}

// toolManifest is the test manifest plus a gcloud component served from
// an absolute URL (no placeholder, like dl.google.com in the real one).
func (e *testEnv) toolManifest(ts toolSpec) *Manifest {
	spec := defaultSpec()
	if ts.serial > 0 {
		spec.serial = ts.serial
	}
	m := e.manifest(spec)
	name := "google-cloud-sdk-" + ts.version + "-windows-x86_64-bundled-python.zip"
	e.srv.putExact("/dl/"+name, ts.archive)
	m.Components = append(m.Components, Component{
		ID: IDGcloud, Version: ts.version, Kind: KindTool, Upstream: "Google Cloud CLI (fake)",
		License: "Apache-2.0", Source: "https://example.invalid/gcloud",
		CheckArgs: []string{"--version"}, CheckEnv: ts.checkEnv,
		Platforms: map[string]*Build{PlatformWindowsX64: {
			DownloadSize: int64(len(ts.archive)), InstalledSize: 1 << 20, InstalledEstimated: true,
			Files: []File{{Name: name, URL: e.url("/dl/" + name), SHA256: shaHex(ts.archive), Size: int64(len(ts.archive)),
				Unpack: "zip", Strip: "google-cloud-sdk"}},
			Exe: hostGcloudExe(), Depends: ts.depends, Env: ts.env, Offline: ts.offline, MaxPath: ts.maxPath,
		}},
	})
	return m
}

func (e *testEnv) writeToolBuiltin(ts toolSpec) *Manifest {
	m := e.toolManifest(ts)
	if err := os.WriteFile(filepath.Join(e.manDir, "components.json"), marshalManifest(e.t, m), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return m
}

// gcloudLog reads the fake's log ("key=value" lines, the last value wins).
func gcloudLog(t *testing.T, p string) (map[string]string, int) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		return map[string]string{}, 0
	}
	out := map[string]string{}
	runs := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k == "args" {
			runs++
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, runs
}

func logEnv(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "gcloud.log")
	t.Setenv("LF_FAKE_GCLOUD_LOG", p)
	return p
}

func envOf(vars []string, key string) (string, bool) {
	for _, kv := range vars {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func noFile(t *testing.T, root, name string) {
	t.Helper()
	for _, f := range listFiles(t, root) {
		if filepath.Base(f) == name {
			t.Errorf("%s left behind: %s", name, f)
		}
	}
}

func TestToolInstallCheckUninstall(t *testing.T) {
	e := newTestEnv(t)
	e.writeToolBuiltin(defaultToolSpec(t))
	log := logEnv(t)
	t.Setenv("PYTHONPATH", filepath.Join(t.TempDir(), "elsewhere"))
	m := e.open()

	if p, ok := m.ToolPath(IDGcloud); ok || p != "" {
		t.Fatalf("ToolPath before installing = %q %v", p, ok)
	}
	if st, _ := statusOf(m, IDGcloud); st.Kind != KindTool || st.Installed || !st.Available {
		t.Fatalf("status before installing: %+v", st)
	}
	mustInstall(t, m, Sources{}, IDGcloud)

	verDir := filepath.Join(e.data, "components", "gcloud", "1.0.0")
	want := filepath.Join(verDir, "tool", filepath.FromSlash(hostGcloudExe()))
	p, ok := m.ToolPath(IDGcloud)
	if !ok || p != want {
		t.Fatalf("ToolPath = %q %v, want %q", p, ok, want)
	}
	// the top-level directory is stripped; the links work
	if _, err := os.Stat(filepath.Join(verDir, "tool", "google-cloud-sdk")); err == nil {
		t.Error("google-cloud-sdk/ was not stripped")
	}
	if b, err := os.ReadFile(filepath.Join(verDir, "tool", "docs", "index.rst")); err != nil || string(b) != "gcloud 1.0.0" {
		t.Errorf("docs/index.rst (a link to ../README) = %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(verDir, "tool", "certs", "expired", "ca", "ca.crt")); err != nil || string(b) != "cert" {
		t.Errorf("ca.crt = %q %v", b, err)
	}
	// the check ran once, with the build's variables, its own configuration
	// directory (removed again) and without the user's PYTHONPATH
	got, runs := gcloudLog(t, log)
	if runs != 1 || got["args"] != "--version" {
		t.Fatalf("check runs %d, log %v", runs, got)
	}
	if got["update"] != "true" || got["pythonpath"] != "" {
		t.Errorf("check environment: %v", got)
	}
	if wantPy := filepath.Join(verDir, "tool", "platform", "bundledpython", "python.exe"); !strings.EqualFold(
		filepath.Clean(got["python"]), filepath.Clean(strings.Replace(wantPy, verDir, filepath.Join(filepath.Dir(verDir), ".s1"), 1))) {
		// the check runs in the staging directory, before the rename
		t.Errorf("CLOUDSDK_PYTHON during the check = %q", got["python"])
	}
	if got["config"] == "" {
		t.Error("the check had no CLOUDSDK_CONFIG")
	} else if _, err := os.Stat(got["config"]); err == nil {
		t.Errorf("the check's configuration directory %s is still there", got["config"])
	}
	noFile(t, e.data, "created.txt")
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		if strings.Contains(f, "/.u/") || strings.Contains(f, "/.chk/") || strings.HasPrefix(f, "downloads/") || strings.Contains(f, "/.s") {
			t.Errorf("left behind: %s", f)
		}
	}
	st, _ := statusOf(m, IDGcloud)
	if !st.Installed || st.Broken || st.InstalledVersion != "1.0.0" || st.DiskUsage <= 0 {
		t.Errorf("status after installing: %+v", st)
	}
	env := m.ToolEnv(IDGcloud)
	if v, _ := envOf(env, "CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK"); v != "true" {
		t.Errorf("ToolEnv = %v", env)
	}
	if v, _ := envOf(env, "CLOUDSDK_PYTHON"); v != filepath.Join(verDir, "tool", "platform", "bundledpython", "python.exe") {
		t.Errorf("ToolEnv CLOUDSDK_PYTHON = %q", v)
	}
	r, err := readReceipt(verDir)
	if err != nil || r.Kind != KindTool || r.Exe != "tool/"+hostGcloudExe() || r.Env["CLOUDSDK_PYTHON"] == "" {
		t.Errorf("receipt %+v %v", r, err)
	}

	// survives a restart (state.json)
	m.Close()
	m = e.open()
	if p2, ok := m.ToolPath(IDGcloud); !ok || p2 != want {
		t.Fatalf("after a restart ToolPath = %q %v", p2, ok)
	}
	// a missing executable: broken, no path
	os.Rename(want, want+".gone")
	if _, ok := m.ToolPath(IDGcloud); ok {
		t.Error("ToolPath with the executable missing")
	}
	if st, _ := statusOf(m, IDGcloud); !st.Broken {
		t.Error("a tool without its executable is not reported broken")
	}
	os.Rename(want+".gone", want)

	if err := m.Uninstall(IDGcloud); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.ToolPath(IDGcloud); ok {
		t.Error("ToolPath after uninstalling")
	}
	if m.ToolEnv(IDGcloud) != nil {
		t.Error("ToolEnv after uninstalling")
	}
	if _, err := os.Stat(filepath.Join(e.data, "components", "gcloud")); err == nil {
		t.Error("components/gcloud still exists")
	}
}

// A failing check installs nothing; the archive stays for the retry, which
// does not download it again.
func TestToolCheckFailureAndRetry(t *testing.T) {
	e := newTestEnv(t)
	ts := defaultToolSpec(t)
	m0 := e.writeToolBuiltin(ts)
	archivePath := strings.TrimPrefix(m0.Component(IDGcloud).Platforms[PlatformWindowsX64].Files[0].URL, e.srv.URL)
	t.Setenv("LF_FAKE_GCLOUD_FAIL", "1")
	m := e.open()
	id, err := m.Install([]string{IDGcloud}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled)
	if st.State != TaskFailed || st.Error == nil || st.Error.Code != CodeVerifyFailed || !strings.Contains(st.Error.Detail, "gcloud crashed") {
		t.Fatalf("task %s %+v", st.State, st.Error)
	}
	if st.Items[0].Phase != PhaseFailed || st.Items[0].Error == nil || st.Items[0].Error.Params["id"] != IDGcloud {
		t.Errorf("item %+v", st.Items[0])
	}
	if _, ok := m.ToolPath(IDGcloud); ok {
		t.Fatal("a tool whose check failed looks installed")
	}
	if s, _ := statusOf(m, IDGcloud); s.Installed {
		t.Fatal("a tool whose check failed is listed as installed")
	}
	if es, _ := os.ReadDir(filepath.Join(e.data, "components", "gcloud")); len(es) != 0 {
		t.Errorf("components/gcloud: %v", es)
	}
	if !fileExists(filepath.Join(e.data, "components", "downloads", shaHex(ts.archive))) {
		t.Error("the verified archive was removed after the failed check")
	}
	t.Setenv("LF_FAKE_GCLOUD_FAIL", "")
	if err := m.Retry(id, nil); err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled); st.State != TaskDone {
		t.Fatalf("retry: %s %+v", st.State, st.Error)
	}
	if n := e.srv.count(archivePath); n != 1 {
		t.Errorf("archive requested %d times, want 1", n)
	}
	if _, ok := m.ToolPath(IDGcloud); !ok {
		t.Error("not installed after the retry")
	}
}

// A damaged download is fetched once more (V3.3), an archive without the
// executable or with an entry outside google-cloud-sdk/ fails the unpack.
func TestToolBadArchives(t *testing.T) {
	t.Run("checksum retried once", func(t *testing.T) {
		e := newTestEnv(t)
		m0 := e.writeToolBuiltin(defaultToolSpec(t))
		u := strings.TrimPrefix(m0.Component(IDGcloud).Platforms[PlatformWindowsX64].Files[0].URL, e.srv.URL)
		e.srv.mu.Lock()
		e.srv.corrupt[u] = 1
		e.srv.mu.Unlock()
		m := e.open()
		mustInstall(t, m, Sources{}, IDGcloud)
		if n := e.srv.count(u); n != 2 {
			t.Errorf("requests %d, want 2", n)
		}
	})
	for name, arc := range map[string][]zipEntry{
		"no executable": {{name: "google-cloud-sdk/README", body: "x"}},
		"outside strip": {{name: "google-cloud-sdk/" + hostGcloudExe(), body: fakeGcloudSh, mode: 0o755}, {name: "other/x", body: "x"}},
		"unsafe link":   {{name: "google-cloud-sdk/" + hostGcloudExe(), body: fakeGcloudSh, mode: 0o755}, {name: "google-cloud-sdk/x", body: "../../etc/passwd", mode: os.ModeSymlink | 0o777}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			ts := defaultToolSpec(t)
			ts.archive = makeModeZip(t, arc)
			e.writeToolBuiltin(ts)
			m := e.open()
			id, err := m.Install([]string{IDGcloud}, Sources{})
			if err != nil {
				t.Fatal(err)
			}
			st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled)
			if st.State != TaskFailed || st.Error.Code != CodeUnpackFailed {
				t.Fatalf("%s %+v", st.State, st.Error)
			}
			if _, ok := m.ToolPath(IDGcloud); ok {
				t.Fatal("installed")
			}
			if es, _ := os.ReadDir(filepath.Join(e.data, "components", "gcloud")); len(es) != 0 {
				t.Errorf("components/gcloud: %v", es)
			}
		})
	}
}

// macOS: gcloud needs the python component (Build.Depends) and runs with
// CLOUDSDK_PYTHON = its interpreter ({exe:python}).
func TestToolPlatformDependsAndEnv(t *testing.T) {
	e := newTestEnv(t)
	ts := defaultToolSpec(t)
	ts.depends = []string{IDPython}
	ts.env = map[string]string{"CLOUDSDK_PYTHON": "{exe:python}", "CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK": "true"}
	e.writeToolBuiltin(ts)
	log := logEnv(t)
	m := e.open()
	p, err := m.Plan([]string{IDGcloud})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 2 || p.Items[0].ID != IDPython || !p.Items[0].Dependency || p.Items[1].ID != IDGcloud {
		t.Fatalf("plan %+v", p.Items)
	}
	if st, _ := statusOf(m, IDGcloud); len(st.Depends) != 1 || st.Depends[0] != IDPython {
		t.Errorf("Status.Depends = %v", st.Depends)
	}
	mustInstall(t, m, Sources{}, IDGcloud)
	py := filepath.Join(e.data, "components", "python", "1.0.0", "runtime", "python", "python.exe")
	got, _ := gcloudLog(t, log)
	if got["python"] != py {
		t.Errorf("CLOUDSDK_PYTHON during the check = %q, want %q", got["python"], py)
	}
	if v, _ := envOf(m.ToolEnv(IDGcloud), "CLOUDSDK_PYTHON"); v != py {
		t.Errorf("ToolEnv CLOUDSDK_PYTHON = %q, want %q", v, py)
	}
	err = m.Uninstall(IDPython)
	if CodeOf(err) != CodeInUse || !strings.Contains(err.Error(), IDGcloud) {
		t.Fatalf("uninstalling python: %v", err)
	}
	if err := m.Uninstall(IDGcloud); err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall(IDPython); err != nil {
		t.Fatalf("python after gcloud: %v", err)
	}
}

// Offline import of a tool archive (files/{name}), like the other kinds.
func TestToolOfflineImport(t *testing.T) {
	e := newTestEnv(t)
	ts := defaultToolSpec(t)
	name := "google-cloud-sdk-1.0.0-windows-x86_64-bundled-python.zip"
	info, _ := json.Marshal(OfflineInfo{Schema: 1, ID: IDGcloud, Version: "1.0.0", Platform: PlatformWindowsX64})
	arc := makeZip(t, map[string][]byte{"component.json": info, "files/" + name: ts.archive})
	ts.offline = &Offline{Name: "gcloud-1.0.0-windows-x64.zip", SHA256: shaHex(arc), Size: int64(len(arc))}
	e.writeToolBuiltin(ts)
	src := filepath.Join(t.TempDir(), "gcloud-1.0.0-windows-x64.zip")
	os.WriteFile(src, arc, 0o644)
	log := logEnv(t)
	e.srv.mu.Lock()
	e.srv.status["/dl/"] = 503 // never downloaded
	e.srv.mu.Unlock()
	m := e.open()
	id, err := m.ImportFile(src)
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled)
	if st.State != TaskDone || st.Items[0].ID != IDGcloud || st.Items[0].Source != SourceOffline {
		t.Fatalf("import %s %+v %+v", st.State, st.Error, st.Items)
	}
	if _, ok := m.ToolPath(IDGcloud); !ok {
		t.Fatal("not installed")
	}
	if _, runs := gcloudLog(t, log); runs != 1 {
		t.Errorf("check runs %d", runs)
	}
	if e.srv.count("/dl/") != 0 {
		t.Error("the import downloaded the archive")
	}
	// a damaged archive inside a matching offline zip is refused
	e2 := newTestEnv(t)
	bad := append([]byte(nil), ts.archive...)
	bad[len(bad)/2] ^= 0xff
	arc2 := makeZip(t, map[string][]byte{"component.json": info, "files/" + name: bad})
	ts2 := defaultToolSpec(t)
	ts2.offline = &Offline{SHA256: shaHex(arc2), Size: int64(len(arc2))}
	e2.writeToolBuiltin(ts2)
	src2 := filepath.Join(t.TempDir(), "bad.zip")
	os.WriteFile(src2, arc2, 0o644)
	m2 := e2.open()
	id2, err := m2.ImportFile(src2)
	if err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m2, id2, TaskDone, TaskFailed, TaskCancelled); st.State != TaskFailed || st.Error.Code != CodeImportChecksum {
		t.Fatalf("damaged import: %s %+v", st.State, st.Error)
	}
	if _, ok := m2.ToolPath(IDGcloud); ok {
		t.Fatal("a damaged import is installed")
	}
}

// An update keeps the installed version until the new one passed its
// check; the worker self-test (Options.SelfTest) is not used for a tool.
func TestToolUpdate(t *testing.T) {
	e := newTestEnv(t)
	e.writeToolBuiltin(defaultToolSpec(t))
	e.setSelfTest(func(ctx context.Context, req SelfTestRequest) error {
		if req.ID == IDGcloud {
			t.Errorf("worker self-test called for the tool: %+v", req)
		}
		return nil
	})
	m := e.open()
	mustInstall(t, m, Sources{}, IDGcloud)
	old, _ := m.ToolPath(IDGcloud)

	ts := defaultToolSpec(t)
	ts.version, ts.serial, ts.archive = "1.1.0", 2, fakeGcloudZip(t, "gcloud 1.1.0")
	e.srv.putExact("/manifest.json", marshalManifest(t, e.toolManifest(ts)))
	if ok, err := m.CheckManifest(context.Background()); !ok || err != nil {
		t.Fatalf("CheckManifest %v %v", ok, err)
	}
	if st, _ := statusOf(m, IDGcloud); !st.UpdateAvailable {
		t.Fatalf("no update: %+v", st)
	}
	t.Setenv("LF_FAKE_GCLOUD_FAIL", "1")
	id, err := m.Update(IDGcloud)
	if err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled); st.State != TaskFailed || st.Error.Code != CodeVerifyFailed {
		t.Fatalf("failing update: %s %+v", st.State, st.Error)
	}
	if p, ok := m.ToolPath(IDGcloud); !ok || p != old {
		t.Fatalf("after a failed update ToolPath = %q %v, want the old %q", p, ok, old)
	}
	t.Setenv("LF_FAKE_GCLOUD_FAIL", "")
	id, err = m.Update(IDGcloud)
	if err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled); st.State != TaskDone {
		t.Fatalf("update: %s %+v", st.State, st.Error)
	}
	p, ok := m.ToolPath(IDGcloud)
	if !ok || !strings.Contains(p, filepath.Join("gcloud", "1.1.0", "tool")) {
		t.Fatalf("after the update ToolPath = %q %v", p, ok)
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(p)), "README")); string(b) != "gcloud 1.1.0" {
		t.Errorf("README = %q", b)
	}
	if _, err := os.Stat(filepath.Join(e.data, "components", "gcloud", "1.0.0")); err == nil {
		t.Error("the old version was not removed")
	}
	if st, _ := m.Task(id); len(st.Notices) != 0 {
		t.Errorf("notices %v (no selftest_skipped for a tool)", st.Notices)
	}
}

// Pause and cancel while the check runs; closing the manager there leaves
// nothing that looks installed after the next start.
func TestToolPauseCancelCloseDuringCheck(t *testing.T) {
	gate := func(t *testing.T) string {
		p := filepath.Join(t.TempDir(), "gate")
		t.Setenv("LF_FAKE_GCLOUD_GATE", p)
		return p
	}
	// running waits until the fake gcloud runs (it logs before the gate)
	running := func(t *testing.T, m *Manager, id TaskID, log string) {
		t.Helper()
		waitPhase(t, m, id, IDGcloud, PhaseChecking)
		waitFor(t, "the check to run", func() bool { _, runs := gcloudLog(t, log); return runs >= 1 })
	}
	t.Run("pause and resume", func(t *testing.T) {
		e := newTestEnv(t)
		m0 := e.writeToolBuiltin(defaultToolSpec(t))
		u := strings.TrimPrefix(m0.Component(IDGcloud).Platforms[PlatformWindowsX64].Files[0].URL, e.srv.URL)
		g := gate(t)
		log := logEnv(t)
		m := e.open()
		id, err := m.Install([]string{IDGcloud}, Sources{})
		if err != nil {
			t.Fatal(err)
		}
		running(t, m, id, log)
		if err := m.Pause(id); err != nil {
			t.Fatal(err)
		}
		if st := wait(t, m, id, TaskPaused, TaskFailed, TaskDone); st.State != TaskPaused {
			t.Fatalf("%s %+v", st.State, st.Error)
		}
		if _, ok := m.ToolPath(IDGcloud); ok {
			t.Fatal("installed while paused")
		}
		os.WriteFile(g, nil, 0o644)
		if err := m.Resume(id); err != nil {
			t.Fatal(err)
		}
		if st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled); st.State != TaskDone {
			t.Fatalf("%s %+v", st.State, st.Error)
		}
		if n := e.srv.count(u); n != 1 {
			t.Errorf("archive requested %d times", n)
		}
		if _, runs := gcloudLog(t, log); runs != 2 {
			t.Errorf("check runs %d, want 2 (interrupted, then again)", runs)
		}
		if _, ok := m.ToolPath(IDGcloud); !ok {
			t.Fatal("not installed")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		e := newTestEnv(t)
		e.writeToolBuiltin(defaultToolSpec(t))
		gate(t)
		log := logEnv(t)
		m := e.open()
		id, err := m.Install([]string{IDGcloud}, Sources{})
		if err != nil {
			t.Fatal(err)
		}
		running(t, m, id, log)
		start := time.Now()
		if err := m.Cancel(id); err != nil {
			t.Fatal(err)
		}
		if st := wait(t, m, id, TaskCancelled, TaskFailed, TaskDone); st.State != TaskCancelled {
			t.Fatalf("%s %+v", st.State, st.Error)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("cancel took %s", d)
		}
		if _, ok := m.ToolPath(IDGcloud); ok {
			t.Fatal("installed after cancel")
		}
		waitFor(t, "cleanup", func() bool {
			fs := listFiles(t, filepath.Join(e.data, "components"))
			for _, f := range fs {
				if f != stateFileName {
					return false
				}
			}
			return true
		})
	})
	t.Run("close and restart", func(t *testing.T) {
		e := newTestEnv(t)
		e.writeToolBuiltin(defaultToolSpec(t))
		gate(t)
		log := logEnv(t)
		m := e.open()
		id, err := m.Install([]string{IDGcloud}, Sources{})
		if err != nil {
			t.Fatal(err)
		}
		running(t, m, id, log)
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		m2 := e.open()
		if _, ok := m2.ToolPath(IDGcloud); ok {
			t.Fatal("an interrupted install looks installed")
		}
		if st, _ := statusOf(m2, IDGcloud); st.Installed {
			t.Fatal("listed as installed")
		}
		if es, _ := os.ReadDir(filepath.Join(e.data, "components", "gcloud")); len(es) != 0 {
			t.Errorf("components/gcloud after the restart: %v", es)
		}
	})
}

// An unpack interrupted half way (a leftover ".u" next to no "tool") is
// unpacked again from the start.
func TestToolHalfUnpackedStagingIsRedone(t *testing.T) {
	e := newTestEnv(t)
	man := e.writeToolBuiltin(defaultToolSpec(t))
	m := e.open()
	t1 := &task{id: "t1", kind: TaskInstall, man: man, items: []*item{{id: IDGcloud, version: "1.0.0", phase: PhasePending}}}
	it := t1.items[0]
	m.mu.Lock()
	m.tasks[t1.id] = t1
	m.order = append(m.order, t1.id)
	m.mu.Unlock()
	staging, err := m.stagingDir(t1, it)
	if err != nil {
		t.Fatal(err)
	}
	half := filepath.Join(staging, unpackTmp, filepath.FromSlash(hostGcloudExe()))
	os.MkdirAll(filepath.Dir(half), 0o755)
	os.WriteFile(half, []byte("half"), 0o755)
	if err := m.runItem(context.Background(), t1, it); err != nil {
		t.Fatal(err)
	}
	p, ok := m.ToolPath(IDGcloud)
	if !ok {
		t.Fatal("not installed")
	}
	if b, _ := os.ReadFile(p); string(b) == "half" {
		t.Fatal("the half unpacked executable was kept")
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(p)), "lib", "gcloud.py")); err != nil || string(b) != "# gcloud" {
		t.Errorf("lib/gcloud.py = %q %v", b, err)
	}
}

// Windows path length: a tool's measured max_path counts like a venv's.
func TestToolPathTooLong(t *testing.T) {
	e := newTestEnv(t)
	ts := defaultToolSpec(t)
	ts.maxPath = 240
	e.writeToolBuiltin(ts)
	m := e.open()
	c := m.manifest().Component(IDGcloud)
	if n := RelPathNeed(c, c.Build(PlatformWindowsX64)); n != PathLen("components/gcloud/1.0.0-r99/")+240 {
		t.Errorf("RelPathNeed = %d", n)
	}
	_, err := m.Plan([]string{IDGcloud})
	if CodeOf(err) != CodePathTooLong {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := m.Install([]string{IDGcloud}, Sources{}); CodeOf(err) != CodePathTooLong {
		t.Fatalf("Install: %v", err)
	}
}

func TestToolManifestValidation(t *testing.T) {
	e := newTestEnv(t)
	base := func() *Manifest { return e.toolManifest(defaultToolSpec(t)) }
	gb := func(m *Manifest) *Build { return m.Component(IDGcloud).Platforms[PlatformWindowsX64] }
	parse := func(m *Manifest) error {
		_, err := ParseManifest(marshalManifest(t, m), e.manDir)
		return err
	}
	if err := parse(base()); err != nil {
		t.Fatalf("valid tool manifest: %v", err)
	}
	cases := map[string]func(m *Manifest){
		"two files":      func(m *Manifest) { b := gb(m); b.Files = append(b.Files, b.Files[0]); b.Files[1].Name = "x.zip" },
		"not an archive": func(m *Manifest) { gb(m).Files[0].Unpack = ""; gb(m).Files[0].Strip = "" },
		"no exe":         func(m *Manifest) { gb(m).Exe = "" },
		"unsafe exe":     func(m *Manifest) { gb(m).Exe = "../gcloud" },
		"lock":           func(m *Manifest) { gb(m).Lock = &Lock{Path: "locks/x.txt", SHA256: strings.Repeat("a", 64)} },
		"size":           func(m *Manifest) { gb(m).DownloadSize++ },
		"strip path":     func(m *Manifest) { gb(m).Files[0].Strip = "a/b" },
		"strip no archive": func(m *Manifest) {
			c := m.Component("tool")
			c.Platforms[PlatformWindowsX64].Files[0].Unpack = ""
			c.Platforms[PlatformWindowsX64].Files[0].Strip = "x"
		},
		"env not a tool":   func(m *Manifest) { m.Component("tool").Platforms[PlatformWindowsX64].Env = map[string]string{"A": "b"} },
		"check not a tool": func(m *Manifest) { m.Component("tool").CheckArgs = []string{"--version"} },
		"env bad name":     func(m *Manifest) { gb(m).Env = map[string]string{"A-B": "x"} },
		"env unknown":      func(m *Manifest) { gb(m).Env = map[string]string{"A": "{home}/x"} },
		"env dir inside":   func(m *Manifest) { gb(m).Env = map[string]string{"A": "x{dir}"} },
		"env tmp in build": func(m *Manifest) { gb(m).Env = map[string]string{"A": "{tmp}"} },
		"env exe not dep":  func(m *Manifest) { gb(m).Env = map[string]string{"A": "{exe:python}"} },
		"env exe of venv": func(m *Manifest) {
			gb(m).Depends = []string{IDASRCUDA}
			gb(m).Env = map[string]string{"A": "{exe:asr-cuda}"}
		},
		"check env unknown": func(m *Manifest) { m.Component(IDGcloud).CheckEnv = map[string]string{"A": "{dir}{x}"} },
		"exclude unsafe":    func(m *Manifest) { gb(m).Files[0].Exclude = []string{"../x/"} },
		"exclude absolute":  func(m *Manifest) { gb(m).Files[0].Exclude = []string{"/lib/"} },
		"exclude no archive": func(m *Manifest) {
			f := &m.Component("model:openvino:tiny:int8").Platforms[PlatformWindowsX64].Files[0]
			f.Exclude = []string{"x"}
		},
		"depends unknown": func(m *Manifest) { gb(m).Depends = []string{"nope"} },
		"depends self":    func(m *Manifest) { gb(m).Depends = []string{IDGcloud} },
		"depends twice": func(m *Manifest) {
			m.Component(IDGcloud).Depends = []string{IDPython}
			gb(m).Depends = []string{IDPython}
		},
		"depends model": func(m *Manifest) { gb(m).Depends = []string{"model:openvino:tiny:int8"} },
		"cycle": func(m *Manifest) {
			gb(m).Depends = []string{IDPython}
			m.Component(IDPython).Depends = []string{IDGcloud}
		},
	}
	for name, mutate := range cases {
		m := base()
		mutate(m)
		if err := parse(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// accepted: {exe:python} of a platform dependency, {tmp} in check_env
	m := base()
	gb(m).Depends = []string{IDPython}
	gb(m).Env = map[string]string{"CLOUDSDK_PYTHON": "{exe:python}"}
	m.Component(IDGcloud).CheckEnv = map[string]string{"CLOUDSDK_CONFIG": "{tmp}", "X": "plain"}
	if err := parse(m); err != nil {
		t.Errorf("platform dependency and templates: %v", err)
	}
	if d := m.Component(IDGcloud).DependsOn(PlatformWindowsX64); len(d) != 1 || d[0] != IDPython {
		t.Errorf("DependsOn = %v", d)
	}
	if d := m.Component(IDGcloud).DependsOn(PlatformDarwinARM64); len(d) != 0 {
		t.Errorf("DependsOn(darwin) = %v", d)
	}
}

// Unpacking with strip: tar.gz and zip, links checked after the strip.
func TestUnpackStripAndLinks(t *testing.T) {
	tarGz := func(t *testing.T, hs []*tar.Header, bodies map[string]string) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, h := range hs {
			b := bodies[h.Name]
			h.Size = int64(len(b))
			if h.Typeflag != tar.TypeReg {
				h.Size = 0
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			tw.Write([]byte(b))
		}
		tw.Close()
		gz.Close()
		p := filepath.Join(t.TempDir(), "a.tar.gz")
		os.WriteFile(p, buf.Bytes(), 0o644)
		return p
	}
	ctx := context.Background()
	t.Run("tar strip", func(t *testing.T) {
		p := tarGz(t, []*tar.Header{
			{Name: "sdk/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "sdk/bin/gcloud", Typeflag: tar.TypeReg, Mode: 0o755},
			{Name: "sdk/README", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "sdk/docs/index.rst", Typeflag: tar.TypeSymlink, Linkname: "../README"},
			{Name: "sdk/copy", Typeflag: tar.TypeLink, Linkname: "sdk/README"},
		}, map[string]string{"sdk/bin/gcloud": "#!/bin/sh\n", "sdk/README": "readme"})
		dst := filepath.Join(t.TempDir(), "out")
		if err := unpack(ctx, p, "tar.gz", layout{Strip: "sdk"}, dst); err != nil {
			t.Fatal(err)
		}
		for rel, want := range map[string]string{"bin/gcloud": "#!/bin/sh\n", "docs/index.rst": "readme", "copy": "readme"} {
			if b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel))); err != nil || string(b) != want {
				t.Errorf("%s = %q %v", rel, b, err)
			}
		}
		if runtime.GOOS != "windows" {
			if fi, err := os.Stat(filepath.Join(dst, "bin", "gcloud")); err != nil || fi.Mode()&0o111 == 0 {
				t.Errorf("bin/gcloud is not executable: %v", fi)
			}
		}
	})
	t.Run("tar link escaping after the strip", func(t *testing.T) {
		// "sdk/x -> ../y" stays inside the archive but leaves dst once sdk/ is dropped
		p := tarGz(t, []*tar.Header{{Name: "sdk/x", Typeflag: tar.TypeSymlink, Linkname: "../y"}}, nil)
		if err := unpack(ctx, p, "tar.gz", layout{Strip: "sdk"}, filepath.Join(t.TempDir(), "out")); CodeOf(err) != CodeUnpackFailed {
			t.Fatalf("err %v", err)
		}
		if err := unpack(ctx, p, "tar.gz", layout{}, filepath.Join(t.TempDir(), "out")); err != nil {
			t.Fatalf("without strip: %v", err)
		}
	})
	t.Run("tar outside strip", func(t *testing.T) {
		p := tarGz(t, []*tar.Header{{Name: "other/x", Typeflag: tar.TypeReg, Mode: 0o644}}, map[string]string{"other/x": "x"})
		if err := unpack(ctx, p, "tar.gz", layout{Strip: "sdk"}, filepath.Join(t.TempDir(), "out")); CodeOf(err) != CodeUnpackFailed {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("zip links", func(t *testing.T) {
		for name, target := range map[string]string{"absolute": "/etc/passwd", "drive": "C:/Windows/x", "up": "../../x", "empty": ""} {
			zp := filepath.Join(t.TempDir(), "a.zip")
			os.WriteFile(zp, makeModeZip(t, []zipEntry{{name: "sdk/a/l", body: target, mode: os.ModeSymlink | 0o777}}), 0o644)
			if err := unpack(ctx, zp, "zip", layout{Strip: "sdk"}, filepath.Join(t.TempDir(), "out")); CodeOf(err) != CodeUnpackFailed {
				t.Errorf("%s link: %v", name, err)
			}
		}
		zp := filepath.Join(t.TempDir(), "b.zip")
		os.WriteFile(zp, makeModeZip(t, []zipEntry{
			{name: "sdk/f", body: "file"},
			{name: "sdk/d/inner", body: "inner"},
			{name: "sdk/lf", body: "f", mode: os.ModeSymlink | 0o777},
			{name: "sdk/ld", body: "d", mode: os.ModeSymlink | 0o777},
			{name: "sdk/dangling", body: "missing", mode: os.ModeSymlink | 0o777},
		}), 0o644)
		dst := filepath.Join(t.TempDir(), "out")
		if err := unpack(ctx, zp, "zip", layout{Strip: "sdk"}, dst); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(dst, "lf")); err != nil || string(b) != "file" {
			t.Errorf("file link = %q %v", b, err)
		}
		if runtime.GOOS != "windows" {
			if b, err := os.ReadFile(filepath.Join(dst, "ld", "inner")); err != nil || string(b) != "inner" {
				t.Errorf("directory link: %q %v", b, err)
			}
			if fi, err := os.Lstat(filepath.Join(dst, "dangling")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("dangling link: %v %v", fi, err)
			}
		}
	})
	t.Run("exclude", func(t *testing.T) {
		lay := layout{Strip: "sdk", Exclude: []string{"lib/deep/", "lib/one.py"}}
		zp := filepath.Join(t.TempDir(), "x.zip")
		os.WriteFile(zp, makeModeZip(t, []zipEntry{
			{name: "sdk/lib/deep/"},
			{name: "sdk/lib/deep/a/b.py", body: "deep"},
			{name: "sdk/lib/deeper.py", body: "kept: only lib/deep/ is a directory prefix"},
			{name: "sdk/lib/one.py", body: "one"},
			{name: "sdk/lib/one.pyc", body: "kept"},
			{name: "sdk/lib/link", body: "deep/a/b.py", mode: os.ModeSymlink | 0o777},
		}), 0o644)
		dst := filepath.Join(t.TempDir(), "out")
		if err := unpack(ctx, zp, "zip", lay, dst); err != nil {
			t.Fatal(err)
		}
		got := listFiles(t, dst)
		if strings.Join(got, ",") != "lib/deeper.py,lib/one.pyc" && strings.Join(got, ",") != "lib/deeper.py,lib/link,lib/one.pyc" {
			t.Errorf("files %v", got)
		}
		if _, err := os.Stat(filepath.Join(dst, "lib", "deep")); err == nil {
			t.Error("the excluded directory was created")
		}
		// tar: a hard link to an excluded file is left out too
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: "sdk/lib/one.py", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
		tw.Write([]byte("one"))
		tw.WriteHeader(&tar.Header{Name: "sdk/lib/copy.py", Typeflag: tar.TypeLink, Linkname: "sdk/lib/one.py"})
		tw.WriteHeader(&tar.Header{Name: "sdk/lib/keep.py", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
		tw.Write([]byte("keep"))
		tw.Close()
		gz.Close()
		tp := filepath.Join(t.TempDir(), "x.tar.gz")
		os.WriteFile(tp, buf.Bytes(), 0o644)
		dst2 := filepath.Join(t.TempDir(), "out")
		if err := unpack(ctx, tp, "tar.gz", lay, dst2); err != nil {
			t.Fatal(err)
		}
		if got := listFiles(t, dst2); strings.Join(got, ",") != "lib/keep.py" {
			t.Errorf("tar files %v", got)
		}
	})
	t.Run("offline wheels still refuse links", func(t *testing.T) {
		zp := filepath.Join(t.TempDir(), "w.zip")
		os.WriteFile(zp, makeModeZip(t, []zipEntry{{name: "wheels/a.whl", body: "x", mode: os.ModeSymlink | 0o777}}), 0o644)
		o := &offlineSource{path: zp}
		if _, err := o.extractWheels(ctx, filepath.Join(t.TempDir(), "w")); err == nil {
			t.Fatal("a link among the wheels was accepted")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		zp := filepath.Join(t.TempDir(), "c.zip")
		os.WriteFile(zp, makeModeZip(t, []zipEntry{{name: "sdk/f", body: "file"}}), 0o644)
		cctx, cancel := context.WithCancelCause(ctx)
		cancel(errCancelled)
		if err := unpack(cctx, zp, "zip", layout{Strip: "sdk"}, filepath.Join(t.TempDir(), "out")); !errors.Is(err, errCancelled) {
			t.Fatalf("err %v", err)
		}
	})
}

// The shipped manifest's gcloud (v31-brief 2.1): Google's archives with
// their own Python on Windows, the python component's on macOS.
func TestBuiltinGcloud(t *testing.T) {
	m, err := LoadManifest(builtinManifest)
	if err != nil {
		t.Fatal(err)
	}
	c := m.Component(IDGcloud)
	if c == nil {
		t.Fatal("no gcloud component")
	}
	if c.Kind != KindTool || len(c.Depends) != 0 || c.NameKey != "compui.name.gcloud" || c.DescKey != "compui.hint.gcloud" ||
		c.License != "Apache-2.0" || len(c.CheckArgs) == 0 || c.CheckArgs[0] != "--version" || c.CheckEnv["CLOUDSDK_CONFIG"] != "{tmp}" {
		t.Errorf("gcloud: %+v", c)
	}
	for plat, exe := range map[string]string{PlatformWindowsX64: "bin/gcloud.cmd", PlatformDarwinARM64: "bin/gcloud"} {
		b := c.Build(plat)
		if b == nil {
			t.Errorf("no %s build", plat)
			continue
		}
		f := b.Files[0]
		if b.Exe != exe || f.Strip != "google-cloud-sdk" || !strings.HasPrefix(f.URL, "https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/") ||
			b.Env["CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK"] != "true" || b.Offline != nil {
			t.Errorf("%s: %+v %+v", plat, b, f)
		}
		if !strings.Contains(f.URL, "-"+strings.Fields(c.Revision)[0]+"-") {
			t.Errorf("%s: %s is not version %s", plat, f.URL, c.Revision)
		}
	}
	win, mac := c.Build(PlatformWindowsX64), c.Build(PlatformDarwinARM64)
	if win.Files[0].Unpack != "zip" || !strings.HasSuffix(win.Files[0].Name, "-bundled-python.zip") || len(win.Depends) != 0 ||
		win.Env["CLOUDSDK_PYTHON"] != "{dir}/platform/bundledpython/python.exe" || win.MaxPath == 0 {
		t.Errorf("windows build %+v", win)
	}
	// Windows: the few API clients whose paths are too long are left out,
	// and max_path is measured without them
	if len(win.Files[0].Exclude) == 0 || RelPathNeed(c, win) > MaxPathLen-1-80 {
		t.Errorf("windows build: exclude %v, path need %d", win.Files[0].Exclude, RelPathNeed(c, win))
	}
	for _, x := range win.Files[0].Exclude {
		for _, needed := range []string{"bin/", "lib/gcloud.py", "lib/googlecloudsdk/core/", "lib/googlecloudsdk/calliope/",
			"lib/googlecloudsdk/command_lib/auth/", "lib/googlecloudsdk/api_lib/auth/", "lib/surface/auth/", "lib/third_party/",
			"platform/bundledpython/", ".install/"} {
			if strings.HasPrefix(x, needed) || strings.HasPrefix(needed, x) {
				t.Errorf("windows build excludes %s, which signing in needs (%s)", x, needed)
			}
		}
	}
	if mac.Files[0].Unpack != "tar.gz" || len(mac.Files[0].Exclude) != 0 || len(mac.Depends) != 1 || mac.Depends[0] != IDPython || mac.Env["CLOUDSDK_PYTHON"] != "{exe:python}" {
		t.Errorf("macOS build %+v", mac)
	}
	if d := c.DependsOn(PlatformDarwinARM64); len(d) != 1 || d[0] != IDPython {
		t.Errorf("DependsOn(darwin) = %v", d)
	}
}

// A tool the current manifest no longer lists still keeps the component its
// variables name ({exe:python}) from being uninstalled.
func TestToolOrphanKeepsItsPython(t *testing.T) {
	e := newTestEnv(t)
	ts := defaultToolSpec(t)
	ts.depends = []string{IDPython}
	ts.env = map[string]string{"CLOUDSDK_PYTHON": "{exe:python}"}
	e.writeToolBuiltin(ts)
	m := e.open()
	mustInstall(t, m, Sources{}, IDGcloud)
	spec := defaultSpec()
	spec.serial = 2
	e.srv.putExact("/manifest.json", marshalManifest(t, e.manifest(spec))) // without gcloud
	if ok, err := m.CheckManifest(context.Background()); !ok || err != nil {
		t.Fatalf("CheckManifest %v %v", ok, err)
	}
	if st, _ := statusOf(m, IDGcloud); !st.Orphaned || !st.Installed {
		t.Fatalf("gcloud %+v", st)
	}
	if err := m.Uninstall(IDPython); CodeOf(err) != CodeInUse || !strings.Contains(err.Error(), IDGcloud) {
		t.Fatalf("uninstalling python under an orphaned gcloud: %v", err)
	}
	if _, ok := m.ToolPath(IDGcloud); !ok {
		t.Fatal("the orphaned tool lost its path")
	}
	if v, _ := envOf(m.ToolEnv(IDGcloud), "CLOUDSDK_PYTHON"); !strings.HasSuffix(v, filepath.Join("runtime", "python", "python.exe")) {
		t.Errorf("orphan ToolEnv CLOUDSDK_PYTHON = %q (from the receipt)", v)
	}
}
