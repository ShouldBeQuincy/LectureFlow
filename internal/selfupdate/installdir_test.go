// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/dirperm"
)

// The installation directory (security audit M-4). Everything lies in
// t.TempDir(); never a real installation, Program Files or the user's
// folders.

// fakeInstall makes a private folder laid out like a Windows installation
// (the shell names such a folder) and returns its path.
func fakeInstall(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(privateDataDir(t), "LectureFlow")
	for _, f := range []string{"LectureFlow.exe", "Uninstall LectureFlow.exe", "resources/app.asar", "resources/bin/lectureflow.exe", "locales/en-US.pak"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("MZ"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// stopAbove makes CheckInstallDir stop looking above at top for the test.
func stopAbove(t *testing.T, top string) {
	t.Helper()
	orig := installAboveTop
	installAboveTop = top
	t.Cleanup(func() { installAboveTop = orig })
}

func wantUntrustedInstall(t *testing.T, what string, err error, path, who string) {
	t.Helper()
	if CodeOf(err) != CodeUntrustedInstallDir {
		t.Fatalf("%s: %v", what, err)
	}
	p := InfoOf(err).Params
	if path != "" && p["path"] != path {
		t.Fatalf("%s: path %v, want %s", what, p["path"], path)
	}
	if !strings.Contains(p["who"].(string), who) {
		t.Fatalf("%s: who %q, want %q", what, p["who"], who)
	}
}

func TestCheckInstallDir(t *testing.T) {
	dir := fakeInstall(t)
	stopAbove(t, filepath.Dir(filepath.Dir(dir)))
	if err := CheckInstallDir(dir); err != nil {
		t.Fatalf("a private installation: %v", err)
	}
	// also named with a trailing separator
	if err := CheckInstallDir(dir + string(filepath.Separator)); err != nil {
		t.Fatalf("with a trailing separator: %v", err)
	}
	for _, rel := range []string{"", "LectureFlow", filepath.Join("Programs", "LectureFlow")} {
		wantUntrustedInstall(t, "relative "+rel, CheckInstallDir(rel), rel, dirperm.NotConfirmed+"not an absolute path")
	}
	if err := CheckInstallDir(filepath.Join(dir, "missing")); CodeOf(err) != CodeIO {
		t.Fatalf("a missing folder: %v", err)
	}
	// more entries or levels than the bounds: not confirmed
	orig := installTree
	t.Cleanup(func() { installTree = orig })
	installTree = dirperm.TreeOptions{MaxEntries: 5}
	wantUntrustedInstall(t, "beyond the bounds", CheckInstallDir(dir), dir, dirperm.NotConfirmed+"more than 5 entries")
	installTree = dirperm.TreeOptions{MaxDepth: 1}
	wantUntrustedInstall(t, "too deep", CheckInstallDir(dir), dir, "deeper than 1 levels")
	installTree = orig
	if err := CheckInstallDir(dir); err != nil {
		t.Fatalf("within the bounds again: %v", err)
	}
}

// An installation directory that is a link (a junction on Windows) is not
// vouched for, whatever it leads to; nor, on Windows, is one with a link
// inside (dirperm.CheckTree: elsewhere a link inside is judged by its own
// owner).
func TestCheckInstallDirLink(t *testing.T) {
	real := fakeInstall(t)
	base := privateDataDir(t)
	stopAbove(t, filepath.Dir(base))
	link := filepath.Join(base, "LectureFlow")
	dirLink(t, link, real)
	wantUntrustedInstall(t, "a link", CheckInstallDir(link), link, dirperm.NotConfirmed)
	if runtime.GOOS != "windows" {
		return
	}
	// a link below it
	inner := filepath.Join(real, "resources", "worker")
	dirLink(t, inner, privateDataDir(t))
	stopAbove(t, filepath.Dir(filepath.Dir(real)))
	err := CheckInstallDir(real)
	wantUntrustedInstall(t, "a link inside", err, real, dirperm.NotConfirmed)
	if !strings.Contains(InfoOf(err).Params["who"].(string), "worker") {
		t.Fatalf("the link is not named: %v", err)
	}
}

// Download and import refuse while the installation directory is not
// vouched for (the data directory is fine); another failure of the check
// does not stop anything (the shell's check refuses then); without an
// installation directory nothing is checked.
func TestUntrustedInstallDirRefusesDownloadAndImport(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.publishLatest(rel)
	orig := checkInstall
	t.Cleanup(func() { checkInstall = orig })
	var asked []string
	checkInstall = func(dir string) error {
		asked = append(asked, dir)
		return cerr(CodeUntrustedInstallDir, map[string]any{"path": dir, "who": "Authenticated Users"}, errors.New("test"))
	}
	install := filepath.Join(t.TempDir(), "LectureFlow")
	u := newTestUpdater(t, f, k, func(o *Options) { o.InstallDir = install })
	if st, _ := u.Check(bg); st.State != StateAvailable {
		t.Fatalf("checking still works: %+v", st)
	}
	_, err := u.Download(bg)
	wantCode(t, err, CodeUntrustedInstallDir)
	if InfoOf(err).Params["path"] != install {
		t.Fatalf("params: %+v", InfoOf(err).Params)
	}
	if st := u.Status(bg); st.State != StateAvailable || st.Pending != nil || st.Progress != nil {
		t.Fatalf("after a refused download: %+v", st)
	}
	src := t.TempDir()
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	_, err = u.ImportPaths(bg, []string{userFile(t, src, name, rel.pkgs[name]),
		userFile(t, src, "update.json.minisig", rel.sig), userFile(t, src, "update.json", rel.raw)})
	wantCode(t, err, CodeUntrustedInstallDir)
	if _, err := os.Stat(filepath.Join(u.Dir(), pendingFile)); !os.IsNotExist(err) {
		t.Fatal("pending.json written for an installation others can change")
	}
	if len(asked) != 2 || asked[0] != install {
		t.Fatalf("checked: %v", asked)
	}
	// the data directory comes first: its own code
	origTrust := checkTrust
	t.Cleanup(func() { checkTrust = origTrust })
	checkTrust = func(dir string) error {
		return cerr(CodeUntrustedDir, map[string]any{"path": dir, "who": "Users"}, errors.New("test"))
	}
	_, err = u.Download(bg)
	wantCode(t, err, CodeUntrustedDir)
	checkTrust = origTrust
	// another failure of the check (the folder cannot be read) refuses too,
	// as "not confirmed" (security review SEC20)
	checkInstall = func(string) error { return ioErr(errors.New("cannot read")) }
	_, err = u.Download(bg)
	wantCode(t, err, CodeUntrustedInstallDir)
	if who := InfoOf(err).Params["who"].(string); who != dirperm.NotConfirmed+"cannot read" || InfoOf(err).Params["path"] != install {
		t.Fatalf("params: %+v", InfoOf(err).Params)
	}
	if st := u.Status(bg); st.State != StateAvailable || st.Pending != nil || st.Progress != nil {
		t.Fatalf("after a refused download: %+v", st)
	}
	// vouched for: downloaded
	checkInstall = func(string) error { return nil }
	waitDownloaded(t, u)
}

func TestNoInstallDirNotChecked(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	orig := checkInstall
	t.Cleanup(func() { checkInstall = orig })
	checkInstall = func(dir string) error {
		t.Errorf("checked %q without an installation directory", dir)
		return cerr(CodeUntrustedInstallDir, nil, errors.New("test"))
	}
	u := newTestUpdater(t, f, k, nil)
	if st, _ := u.Check(bg); st.State != StateAvailable {
		t.Fatalf("check: %+v", st)
	}
	waitDownloaded(t, u)
}
