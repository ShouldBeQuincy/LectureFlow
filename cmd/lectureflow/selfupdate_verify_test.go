// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/selfupdate"
	"lectureflow/internal/selfupdate/selfupdatetest"
)

// updatesDir is {a new private data directory}/updates, as LectureFlow
// makes them (the system's temporary directory may let other accounts in,
// which -selfupdate-verify refuses).
func updatesDir(t *testing.T) string {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	if _, err := dirperm.MkdirPrivate(data); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, "updates")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeRelease lays out version's signed update.json and its installer
// under dir/<version>/ for this system's platform and returns the
// installer's name, content and SHA-256.
func writeRelease(t *testing.T, k *selfupdatetest.Key, dir, version string) (string, []byte, string) {
	t.Helper()
	platform := selfupdate.CurrentPlatform()
	if platform == "" {
		t.Skip("no update packages for this platform")
	}
	pkg := bytes.Repeat([]byte("installer "+version+" "), 4096)
	sum := sha256.Sum256(pkg)
	name := "LectureFlow-Setup-" + version + "-x64.exe"
	m := selfupdate.Manifest{Schema: 1, App: selfupdate.AppName, Version: version, Channel: selfupdate.ChannelStable,
		ReleasedAt: "2026-11-02T10:00:00Z", Platforms: map[string][]selfupdate.Asset{platform: {{Kind: selfupdate.KindInstaller,
			Name: name, URL: "https://example.invalid/" + name, Size: int64(len(pkg)), SHA256: hex.EncodeToString(sum[:])}}}}
	raw, _ := json.Marshal(m)
	vdir := filepath.Join(dir, version)
	os.MkdirAll(vdir, 0o755)
	os.WriteFile(filepath.Join(vdir, "update.json"), raw, 0o644)
	os.WriteFile(filepath.Join(vdir, "update.json.minisig"), k.Sign(raw, selfupdate.TrustedComment(version), true), 0o644)
	os.WriteFile(filepath.Join(vdir, name), pkg, 0o644)
	return name, pkg, hex.EncodeToString(sum[:])
}

// writePending lays out a verified pending update of 3.2.0 for this
// system's platform under dir (signed with a key made for the test) and
// returns the key and the package's path.
func writePending(t *testing.T, dir string) (*selfupdatetest.Key, string) {
	t.Helper()
	k, err := selfupdatetest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	name, pkg, sum := writeRelease(t, k, dir, "3.2.0")
	p := selfupdate.Pending{Schema: 1, Version: "3.2.0", FromVersion: "3.1.0", Channel: selfupdate.ChannelStable, Platform: selfupdate.CurrentPlatform(),
		Kind: selfupdate.KindInstaller, File: "3.2.0/" + name, Size: int64(len(pkg)), SHA256: sum,
		Manifest: "3.2.0/update.json", Signature: "3.2.0/update.json.minisig", Origin: selfupdate.OriginFile}
	b, _ := json.Marshal(p)
	os.WriteFile(filepath.Join(dir, "pending.json"), b, 0o644)
	return k, filepath.Join(dir, "3.2.0", name)
}

func runVerify(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runMain(runEnv{args: args, stdin: strings.NewReader(""), stdout: &out, stderr: &errb, exit: func(int) {}})
	return code, out.String()
}

func useKey(t *testing.T, k *selfupdatetest.Key) {
	orig := selfupdateVerifyKey
	t.Cleanup(func() { selfupdateVerifyKey = orig })
	selfupdateVerifyKey = func() string { return k.PublicKey() }
}

func TestSelfUpdateVerifyCommand(t *testing.T) {
	dir := updatesDir(t)
	k, pkg := writePending(t, dir)
	useKey(t, k)

	code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.1.0")
	if code != 0 || !strings.HasPrefix(out, "ok version=3.2.0 kind=installer file=3.2.0/") || strings.Contains(out, "copy=") || strings.Count(out, "\n") != 1 {
		t.Fatalf("valid: %d %q", code, out)
	}
	// not newer than the running version
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.2.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.not_newer: ") {
		t.Fatalf("same version: %d %q", code, out)
	}
	// a changed package
	b, _ := os.ReadFile(pkg)
	b[len(b)/2] ^= 1
	os.WriteFile(pkg, b, 0o644)
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.1.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.checksum: ") {
		t.Fatalf("changed package: %d %q", code, out)
	}
	b[len(b)/2] ^= 1
	os.WriteFile(pkg, b, 0o644)
	// no pending update
	if code, out := runVerify(t, "-selfupdate-verify", updatesDir(t)); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.not_available: ") {
		t.Fatalf("nothing pending: %d %q", code, out)
	}
	// only checks: nothing was written next to the files
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("the updates directory changed: %v", entries)
	}
	// a build without a key
	selfupdateVerifyKey = func() string { return "" }
	if code, out := runVerify(t, "-selfupdate-verify", dir); code != 2 || out != "not_configured: this build has no update public key\n" {
		t.Fatalf("no key: %d %q", code, out)
	}
	// a data directory that does not exist cannot be checked: untrusted
	if code, out := runVerify(t, "-selfupdate-verify", filepath.Join(t.TempDir(), "gone", "updates")); code != 3 || !strings.HasPrefix(out, "untrusted_dir: ") {
		t.Fatalf("missing data directory: %d %q", code, out)
	}
}

// -selfupdate-copy: the package is copied into a new private directory and
// named on the ok line; the shell runs that copy (sec-update M3).
func TestSelfUpdateVerifyCopy(t *testing.T) {
	dir := updatesDir(t)
	k, pkg := writePending(t, dir)
	useKey(t, k)
	tmp := t.TempDir()
	code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.1.0", "-selfupdate-copy", tmp)
	line := strings.TrimSuffix(out, "\n")
	i := strings.Index(line, " copy=")
	if code != 0 || i < 0 || strings.Count(out, "\n") != 1 {
		t.Fatalf("copy: %d %q", code, out)
	}
	cp := line[i+len(" copy="):]
	want, _ := os.ReadFile(pkg)
	got, err := os.ReadFile(cp)
	if err != nil || !bytes.Equal(got, want) || filepath.Dir(filepath.Dir(cp)) != tmp || !strings.HasPrefix(filepath.Base(filepath.Dir(cp)), selfupdate.CopyPrefix) {
		t.Fatalf("the copy %q: %v", cp, err)
	}
	if e, err := dirperm.Check(filepath.Dir(cp)); err != nil || e.Exposed() {
		t.Fatalf("the copy's directory: %+v %v", e, err)
	}
}

// -selfupdate-previous: the package a rollback runs is verified like the
// update (signature, size and SHA-256, older than this version).
func TestSelfUpdateVerifyPrevious(t *testing.T) {
	dir := updatesDir(t)
	k, _ := writePending(t, dir)
	useKey(t, k)
	name, pkg, sum := writeRelease(t, k, dir, "3.1.0")
	prev := selfupdate.Previous{Schema: 1, Version: "3.1.0", Platform: selfupdate.CurrentPlatform(), Kind: selfupdate.KindInstaller,
		File: "3.1.0/" + name, Size: int64(len(pkg)), SHA256: sum, CreatedAt: "2026-11-02T12:00:00Z"}
	b, _ := json.Marshal(prev)
	os.WriteFile(filepath.Join(dir, "previous.json"), b, 0o644)
	tmp := t.TempDir()
	code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.2.0", "-selfupdate-copy", tmp)
	if code != 0 || !strings.HasPrefix(out, "ok version=3.1.0 kind=installer file=3.1.0/"+name+" sha256="+sum+" copy="+tmp) {
		t.Fatalf("previous: %d %q", code, out)
	}
	// not older than the running version
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.1.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.manifest_invalid: ") {
		t.Fatalf("same version: %d %q", code, out)
	}
	// a changed package
	f := filepath.Join(dir, "3.1.0", name)
	pkg[10] ^= 1
	os.WriteFile(f, pkg, 0o644)
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.2.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.checksum: ") {
		t.Fatalf("changed package: %d %q", code, out)
	}
	// previous.json without a package (only a backup)
	prev.File, prev.Size, prev.SHA256, prev.Backup = "", 0, "", "backup/3.1.0"
	b, _ = json.Marshal(prev)
	os.WriteFile(filepath.Join(dir, "previous.json"), b, 0o644)
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.2.0"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.not_available: ") {
		t.Fatalf("backup only: %d %q", code, out)
	}
}

// -selfupdate-private: no package is verified; after the check of the
// data directory's permissions an empty private directory is made for the
// shell's copy of a backup (security audit M-3). It needs no public key.
func TestSelfUpdateVerifyPrivate(t *testing.T) {
	dir := updatesDir(t)
	orig := selfupdateVerifyKey
	t.Cleanup(func() { selfupdateVerifyKey = orig })
	selfupdateVerifyKey = func() string { return "" }
	tmp := t.TempDir()
	code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.2.0", "-selfupdate-private", "-selfupdate-copy", tmp)
	if code != 0 || !strings.HasPrefix(out, "ok private=") || strings.Count(out, "\n") != 1 {
		t.Fatalf("private: %d %q", code, out)
	}
	priv := strings.TrimSuffix(strings.TrimPrefix(out, "ok private="), "\n")
	if filepath.Dir(priv) != tmp || !strings.HasPrefix(filepath.Base(priv), selfupdate.CopyPrefix) {
		t.Fatalf("the private directory %q is not in %q", priv, tmp)
	}
	if entries, err := os.ReadDir(priv); err != nil || len(entries) != 0 {
		t.Fatalf("not an empty directory: %v %v", entries, err)
	}
	if e, err := dirperm.Check(priv); err != nil || e.Exposed() {
		t.Fatalf("the private directory: %+v %v", e, err)
	}
	if rep, err := dirperm.CheckTree(priv, dirperm.TreeOptions{}); err != nil || !rep.OK() {
		t.Fatalf("the private directory: %+v %v", rep, err)
	}
	// every call makes another one
	_, out2 := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-private", "-selfupdate-copy", tmp)
	if out2 == out || !strings.HasPrefix(out2, "ok private=") {
		t.Fatalf("second: %q", out2)
	}
	// nowhere to make it
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-private"); code != 1 || !strings.HasPrefix(out, "invalid selfupdate.io_failed: ") {
		t.Fatalf("no -selfupdate-copy: %d %q", code, out)
	}
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-private", "-selfupdate-copy", filepath.Join(tmp, "missing")); code != 1 || !strings.HasPrefix(out, "invalid ") {
		t.Fatalf("a missing parent: %d %q", code, out)
	}
	// the permissions of the data directory come first
	if code, out := runVerify(t, "-selfupdate-verify", filepath.Join(t.TempDir(), "gone", "updates"), "-selfupdate-private", "-selfupdate-copy", tmp); code != 3 || !strings.HasPrefix(out, "untrusted_dir: ") {
		t.Fatalf("missing data directory: %d %q", code, out)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 2 {
		t.Fatalf("directories left in %s: %v", tmp, entries)
	}
}

// -selfupdate-trust-dir (SEC17 中-B): the shell's trust directory is
// checked in every mode after the data directory; -selfupdate-trust-check
// checks nothing else. Never the real %LOCALAPPDATA%: a folder in
// t.TempDir() stands for it.
func TestSelfUpdateVerifyTrustDir(t *testing.T) {
	dir := updatesDir(t)
	k, _ := writePending(t, dir)
	useKey(t, k)
	trust := filepath.Join(t.TempDir(), "LectureFlow", "selfupdate")

	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-trust-dir", trust, "-selfupdate-trust-check"); code != 0 || out != "ok trusted\n" {
		t.Fatalf("trust check: %d %q", code, out)
	}
	if e, err := dirperm.Check(trust); err != nil || e.Exposed() {
		t.Fatalf("the trust directory was not made privately: %+v %v", e, err)
	}
	code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.1.0", "-selfupdate-trust-dir", trust)
	if code != 0 || !strings.HasPrefix(out, "ok version=3.2.0 ") {
		t.Fatalf("verify with the trust directory: %d %q", code, out)
	}
	// in the data directory: refused in every mode, and not made there.
	// The reason ends with the words the desktop shell recognises
	// (desktop/src/main/lib/verifyupdate.js OVERLAP_RE, OVERLAP_DIR_RE: it
	// then says to move the data folder, not to tighten its permissions;
	// R47 S2): change both together.
	overlap := regexp.MustCompile(`^untrusted_dir: selfupdate\.untrusted_dir: the update's trust directory .+ cannot vouch for the record \(it lies in the data directory, or the data directory lies in it\)\n$`)
	in := filepath.Join(filepath.Dir(dir), "electron", "selfupdate")
	for _, extra := range [][]string{{"-selfupdate-trust-check"}, {"-app-version", "3.1.0"}, {"-selfupdate-previous"}, {"-selfupdate-private", "-selfupdate-copy", t.TempDir()}} {
		args := append([]string{"-selfupdate-verify", dir, "-selfupdate-trust-dir", in}, extra...)
		if code, out := runVerify(t, args...); code != 3 || !strings.HasPrefix(out, "untrusted_dir: ") || !overlap.MatchString(out) {
			t.Fatalf("%v: %d %q", extra, code, out)
		}
	}
	if _, err := os.Lstat(filepath.Dir(in)); !os.IsNotExist(err) {
		t.Fatalf("something was made in the data directory: %v", err)
	}
	// the data directory comes first
	if code, out := runVerify(t, "-selfupdate-verify", filepath.Join(t.TempDir(), "gone", "updates"), "-selfupdate-trust-dir", trust, "-selfupdate-trust-check"); code != 3 || !strings.HasPrefix(out, "untrusted_dir: ") {
		t.Fatalf("missing data directory: %d %q", code, out)
	}
	// without a trust directory the check covers the data directory only
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-trust-check"); code != 0 || out != "ok trusted\n" {
		t.Fatalf("trust check without a trust directory: %d %q", code, out)
	}
}

// -selfupdate-install-dir (security audit M-4): the installation directory
// is checked last, in every mode; refused with its own line and exit 3.
func TestSelfUpdateVerifyInstallDir(t *testing.T) {
	dir := updatesDir(t)
	k, _ := writePending(t, dir)
	useKey(t, k)
	install := filepath.Join(filepath.Dir(updatesDir(t)), "LectureFlow") // in a private folder
	if err := os.MkdirAll(filepath.Join(install, "resources", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// the folders above the temporary folder are this computer's: the
	// check of the installation itself, without them
	orig := checkInstallDir
	t.Cleanup(func() { checkInstallDir = orig })
	var asked []string
	checkInstallDir = func(dir string) error {
		asked = append(asked, dir)
		if dir == install {
			if e, err := dirperm.Check(dir); err != nil || e.Write {
				t.Fatalf("not private: %+v %v", e, err)
			}
			return nil
		}
		return orig(dir)
	}
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-install-dir", install, "-selfupdate-trust-check"); code != 0 || out != "ok trusted\n" {
		t.Fatalf("a private installation: %d %q", code, out)
	}
	code, out := runVerify(t, "-selfupdate-verify", dir, "-app-version", "3.1.0", "-selfupdate-install-dir", install)
	if code != 0 || !strings.HasPrefix(out, "ok version=3.2.0 ") {
		t.Fatalf("verify with a private installation: %d %q", code, out)
	}
	tmp := t.TempDir()
	for _, bad := range []string{"LectureFlow", filepath.Join(install, "missing")} {
		for _, extra := range [][]string{{"-selfupdate-trust-check"}, {"-app-version", "3.1.0", "-selfupdate-copy", tmp}, {"-selfupdate-previous"}, {"-selfupdate-private", "-selfupdate-copy", tmp}} {
			args := append([]string{"-selfupdate-verify", dir, "-selfupdate-install-dir", bad}, extra...)
			if code, out := runVerify(t, args...); code != 3 || !strings.HasPrefix(out, "untrusted_install_dir: ") || strings.Count(out, "\n") != 1 {
				t.Fatalf("%s %v: %d %q", bad, extra, code, out)
			}
		}
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Fatalf("something was made: %v", ents)
	}
	// the data directory comes first, with its own line
	n := len(asked)
	if code, out := runVerify(t, "-selfupdate-verify", filepath.Join(t.TempDir(), "gone", "updates"), "-selfupdate-install-dir", "LectureFlow", "-selfupdate-trust-check"); code != 3 || !strings.HasPrefix(out, "untrusted_dir: ") {
		t.Fatalf("missing data directory: %d %q", code, out)
	}
	// without the flag nothing is asked
	if code, out := runVerify(t, "-selfupdate-verify", dir, "-selfupdate-trust-check"); code != 0 || out != "ok trusted\n" || len(asked) != n {
		t.Fatalf("without an installation directory: %d %q %v", code, out, asked)
	}
}
