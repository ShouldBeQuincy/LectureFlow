// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/dirperm"
)

// pendingDir makes a real pending update (an offline import of 3.2.0 into
// a 3.1.0 windows-x64 installation) and returns its updates directory.
// withPrevious: 3.1.0 came through the updater too (its verified package
// is in updates/3.1.0/), so previous.json names it.
func pendingDir(t *testing.T, k testKey, withPrevious ...bool) (string, *release) {
	t.Helper()
	rel := makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	src := t.TempDir()
	u := offlineUpdater(t, k, nil)
	if len(withPrevious) > 0 && withPrevious[0] {
		old := makeRelease(t, nil, k, "3.1.0", ChannelStable, nil)
		dir := filepath.Join(u.Dir(), "3.1.0")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, manifestFile), old.raw, 0o644)
		os.WriteFile(filepath.Join(dir, signatureFile), old.sig, 0o644)
		oldName := "LectureFlow-Setup-3.1.0-x64.exe"
		os.WriteFile(filepath.Join(dir, oldName), old.pkgs[oldName], 0o644)
	}
	if _, err := u.ImportPaths(bg, []string{userFile(t, src, name, rel.pkgs[name]),
		userFile(t, src, "update.json.minisig", rel.sig), userFile(t, src, "update.json", rel.raw)}); err != nil {
		t.Fatal(err)
	}
	return u.Dir(), rel
}

func verifyOpts(k testKey) VerifyOptions {
	return VerifyOptions{PublicKey: k.pubFile(), Current: "3.1.0", Platform: PlatformWindowsX64}
}

func flipByte(t *testing.T, dir, rel string, at int) {
	t.Helper()
	f := filepath.Join(dir, filepath.FromSlash(rel))
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	b[at] ^= 0x01
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b[at] ^= 0x01
		os.WriteFile(f, b, 0o644)
	})
}

func editJSON[T any](t *testing.T, f string, mod func(*T)) {
	t.Helper()
	orig, _ := os.ReadFile(f)
	var v T
	json.Unmarshal(orig, &v)
	mod(&v)
	b, _ := json.Marshal(v)
	os.WriteFile(f, b, 0o644)
	t.Cleanup(func() { os.WriteFile(f, orig, 0o644) })
}

// The pending update the updater accepted verifies again; every change
// made since (a byte of the package, of update.json, of its signature, of
// pending.json's claims) is refused.
func TestVerifyPending(t *testing.T) {
	k := newTestKey(t)
	dir, rel := pendingDir(t, k)
	p, err := VerifyPending(dir, verifyOpts(k))
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "3.2.0" || p.Kind != KindInstaller || p.File != "3.2.0/LectureFlow-Setup-3.2.0-x64.exe" || p.Copy != "" {
		t.Fatalf("%+v", p)
	}
	// the public line alone works as well as the .pub file
	o := verifyOpts(k)
	o.PublicKey = k.PublicKey()
	if _, err := VerifyPending(dir, o); err != nil {
		t.Fatal(err)
	}

	editPending := func(t *testing.T, mod func(*Pending)) { editJSON(t, filepath.Join(dir, pendingFile), mod) }
	other := newTestKey(t)
	for _, c := range []struct {
		name string
		do   func(t *testing.T) VerifyOptions
		code string
	}{
		{"package byte", func(t *testing.T) VerifyOptions { flipByte(t, dir, p.File, 1000); return verifyOpts(k) }, CodeChecksum},
		{"package last byte", func(t *testing.T) VerifyOptions { flipByte(t, dir, p.File, int(p.Size-1)); return verifyOpts(k) }, CodeChecksum},
		{"update.json byte", func(t *testing.T) VerifyOptions { flipByte(t, dir, p.Manifest, 10); return verifyOpts(k) }, CodeSignatureInvalid},
		{"signature byte", func(t *testing.T) VerifyOptions { flipByte(t, dir, p.Signature, len(rel.sig)-5); return verifyOpts(k) }, CodeSignatureInvalid},
		{"another key", func(t *testing.T) VerifyOptions {
			o := verifyOpts(k)
			o.PublicKey = other.pubFile()
			return o
		}, CodeSignatureInvalid},
		{"pending claims another hash", func(t *testing.T) VerifyOptions {
			editPending(t, func(p *Pending) { p.SHA256 = sha([]byte("x")) })
			return verifyOpts(k)
		}, CodeChecksum},
		{"pending names another version", func(t *testing.T) VerifyOptions {
			editPending(t, func(p *Pending) { p.Version = "9.0.0" })
			return verifyOpts(k)
		}, CodeVersionMismatch},
		{"pending file outside", func(t *testing.T) VerifyOptions {
			editPending(t, func(p *Pending) { p.File = "../outside.exe" })
			return verifyOpts(k)
		}, CodeManifestInvalid},
		{"pending file under another name", func(t *testing.T) VerifyOptions {
			// the genuine bytes, but not where the updater keeps them
			b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p.File)))
			os.WriteFile(filepath.Join(dir, "3.2.0", "renamed.exe"), b, 0o644)
			editPending(t, func(p *Pending) { p.File = "3.2.0/renamed.exe" })
			return verifyOpts(k)
		}, CodeManifestInvalid},
		{"pending kind not signed", func(t *testing.T) VerifyOptions {
			editPending(t, func(p *Pending) { p.Kind = "component:x" })
			return verifyOpts(k)
		}, CodeNoPackage},
		{"not newer", func(t *testing.T) VerifyOptions {
			o := verifyOpts(k)
			o.Current = "3.2.0"
			return o
		}, CodeNotNewer},
		{"other platform", func(t *testing.T) VerifyOptions {
			o := verifyOpts(k)
			o.Platform = PlatformDarwinArm64
			return o
		}, CodeNoPackage},
		{"package gone", func(t *testing.T) VerifyOptions {
			f := filepath.Join(dir, filepath.FromSlash(p.File))
			b, _ := os.ReadFile(f)
			os.Remove(f)
			t.Cleanup(func() { os.WriteFile(f, b, 0o644) })
			return verifyOpts(k)
		}, CodeIO},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := VerifyPending(dir, c.do(t))
			if CodeOf(err) != c.code {
				t.Fatalf("error %v (code %q), want %s", err, CodeOf(err), c.code)
			}
		})
	}
	// a development build does not compare versions
	o = verifyOpts(k)
	o.Current = "dev"
	if _, err := VerifyPending(dir, o); err != nil {
		t.Fatal(err)
	}
}

// With CopyDir the package is copied, while it is hashed, into a new
// private directory; a package that does not match leaves no copy.
func TestVerifyPendingCopy(t *testing.T) {
	k := newTestKey(t)
	dir, rel := pendingDir(t, k)
	tmp := t.TempDir()
	o := verifyOpts(k)
	o.CopyDir = tmp
	p, err := VerifyPending(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	if filepath.Base(p.Copy) != name || filepath.Dir(filepath.Dir(p.Copy)) != tmp ||
		!strings.HasPrefix(filepath.Base(filepath.Dir(p.Copy)), CopyPrefix) {
		t.Fatalf("copy at %q", p.Copy)
	}
	got, err := os.ReadFile(p.Copy)
	if err != nil || !bytes.Equal(got, rel.pkgs[name]) {
		t.Fatalf("copy content: %v", err)
	}
	if e, err := dirperm.Check(filepath.Dir(p.Copy)); err != nil || e.Exposed() {
		t.Fatalf("the copy's directory is not private: %+v %v", e, err)
	}
	// a second check makes a second, separate copy
	p2, err := VerifyPending(dir, o)
	if err != nil || p2.Copy == p.Copy {
		t.Fatalf("second copy %q (%v)", p2.Copy, err)
	}

	// a changed package: refused, and nothing is left in CopyDir
	empty := t.TempDir()
	o.CopyDir = empty
	flipByte(t, dir, p.File, int(p.Size/2))
	if _, err := VerifyPending(dir, o); CodeOf(err) != CodeChecksum {
		t.Fatalf("changed package: %v", err)
	}
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Fatalf("a refused package left %v", ents)
	}
}

func TestVerifyPendingNotConfiguredOrNone(t *testing.T) {
	k := newTestKey(t)
	dir, _ := pendingDir(t, k)
	o := verifyOpts(k)
	o.PublicKey = ""
	_, err := VerifyPending(dir, o)
	if !errors.Is(err, ErrNotConfigured) || CodeOf(err) != CodeNotConfigured {
		t.Fatalf("no key: %v", err)
	}
	if _, err := VerifyPending(t.TempDir(), verifyOpts(k)); CodeOf(err) != CodeNotAvailable {
		t.Fatalf("no pending update: %v", err)
	}
	if BuiltinPublicKey() != PublicKey {
		t.Fatal("BuiltinPublicKey is not the built-in key")
	}
}

// The package a rollback runs (previous.json "file") is held to the same
// standard as the update (sec-update M2): signature of its update.json,
// version binding, older than the running version, size and SHA-256.
func TestVerifyPrevious(t *testing.T) {
	k := newTestKey(t)
	dir, _ := pendingDir(t, k, true)
	var prev Previous
	if ok, err := readJSON(filepath.Join(dir, previousFile), &prev); !ok || err != nil || prev.File == "" {
		t.Fatalf("previous.json %+v %v", prev, err)
	}
	o := verifyOpts(k)
	o.Current = "3.2.0" // the new version, which failed
	o.CopyDir = t.TempDir()
	v, err := VerifyPrevious(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "3.1.0" || v.Kind != KindInstaller || v.File != "3.1.0/LectureFlow-Setup-3.1.0-x64.exe" || v.SHA256 != prev.SHA256 ||
		filepath.Base(v.Copy) != "LectureFlow-Setup-3.1.0-x64.exe" {
		t.Fatalf("%+v", v)
	}
	o.CopyDir = ""

	editPrev := func(t *testing.T, mod func(*Previous)) { editJSON(t, filepath.Join(dir, previousFile), mod) }
	other := newTestKey(t)
	for _, c := range []struct {
		name string
		do   func(t *testing.T) VerifyOptions
		code string
	}{
		{"package byte", func(t *testing.T) VerifyOptions { flipByte(t, dir, prev.File, 77); return o }, CodeChecksum},
		{"update.json byte", func(t *testing.T) VerifyOptions { flipByte(t, dir, "3.1.0/update.json", 10); return o }, CodeSignatureInvalid},
		{"another key", func(t *testing.T) VerifyOptions {
			x := o
			x.PublicKey = other.pubFile()
			return x
		}, CodeSignatureInvalid},
		{"claims another hash", func(t *testing.T) VerifyOptions {
			editPrev(t, func(p *Previous) { p.SHA256 = sha([]byte("x")) })
			return o
		}, CodeChecksum},
		{"names a file elsewhere", func(t *testing.T) VerifyOptions {
			editPrev(t, func(p *Previous) { p.File = "../../evil.exe" })
			return o
		}, CodeManifestInvalid},
		{"names the new version's package", func(t *testing.T) VerifyOptions {
			// 3.2.0's genuine package, signed: but not older than 3.2.0
			var p Pending
			readJSON(filepath.Join(dir, pendingFile), &p)
			editPrev(t, func(x *Previous) { x.Version, x.File, x.Size, x.SHA256 = p.Version, p.File, p.Size, p.SHA256 })
			return o
		}, CodeManifestInvalid},
		{"another kind", func(t *testing.T) VerifyOptions {
			editPrev(t, func(p *Previous) { p.Kind = KindPortable })
			return o
		}, CodeChecksum},
		{"no package (backup only)", func(t *testing.T) VerifyOptions {
			editPrev(t, func(p *Previous) { p.File, p.Size, p.SHA256, p.Backup = "", 0, "", "backup/3.1.0" })
			return o
		}, CodeNotAvailable},
		{"other platform", func(t *testing.T) VerifyOptions {
			x := o
			x.Platform = PlatformDarwinArm64
			return x
		}, CodeNoPackage},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := VerifyPrevious(dir, c.do(t))
			if CodeOf(err) != c.code {
				t.Fatalf("error %v (code %q), want %s", err, CodeOf(err), c.code)
			}
		})
	}
	if _, err := VerifyPrevious(t.TempDir(), o); CodeOf(err) != CodeNotAvailable {
		t.Fatalf("no previous.json: %v", err)
	}
	x := o
	x.PublicKey = ""
	if _, err := VerifyPrevious(dir, x); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no key: %v", err)
	}
}

// A private data directory is trusted; a missing updates directory counts
// as its data directory.
func TestCheckTrustPrivate(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	if _, err := dirperm.MkdirPrivate(data); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrust(filepath.Join(data, "updates")); err != nil {
		t.Fatalf("missing updates directory: %v", err)
	}
	os.Mkdir(filepath.Join(data, "updates"), 0o755)
	if err := CheckTrust(filepath.Join(data, "updates")); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrust(filepath.Join(t.TempDir(), "gone", "updates")); CodeOf(err) != CodeIO {
		t.Fatalf("missing data directory: %v", err)
	}
}

// Everything below the updates directory is looked at, within bounds: a
// tree larger than the bounds cannot be vouched for, so it is not trusted
// (security audit M-3).
func TestCheckTrustWalksTheUpdatesDirectory(t *testing.T) {
	data := privateDataDir(t)
	updates := filepath.Join(data, "updates")
	for _, f := range []string{"3.2.0/update.json", "3.2.0/update.json.minisig", "backup/3.1.0/app/LectureFlow.exe", "backup/3.1.0/backup.json", "previous.json"} {
		p := filepath.Join(updates, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// the shell's profile with its record, as in an unmoved data directory
	if err := os.Mkdir(filepath.Join(data, ShellProfileDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, ShellProfileDir, TrustRecordFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrust(updates); err != nil {
		t.Fatalf("a private tree: %v", err)
	}
	orig := updatesTree
	t.Cleanup(func() { updatesTree = orig })
	updatesTree = dirperm.TreeOptions{MaxEntries: 5}
	err := CheckTrust(updates)
	if CodeOf(err) != CodeUntrustedDir || InfoOf(err).Params["path"] != updates ||
		!strings.Contains(InfoOf(err).Params["who"].(string), "not confirmed: more than 5 entries") {
		t.Fatalf("beyond the bounds: %v", err)
	}
	updatesTree = dirperm.TreeOptions{MaxDepth: 2}
	if err := CheckTrust(updates); CodeOf(err) != CodeUntrustedDir || !strings.Contains(InfoOf(err).Params["who"].(string), "deeper than 2 levels") {
		t.Fatalf("too deep: %v", err)
	}
}

// Download and import refuse an untrusted data directory.
func TestUntrustedDirRefusesDownloadAndImport(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.publishLatest(rel)
	orig := checkTrust
	t.Cleanup(func() { checkTrust = orig })
	checkTrust = func(dir string) error {
		return cerr(CodeUntrustedDir, map[string]any{"path": filepath.Dir(dir), "who": "Authenticated Users"}, errors.New("test"))
	}
	u := newTestUpdater(t, f, k, nil)
	if st, _ := u.Check(bg); st.State != StateAvailable {
		t.Fatalf("checking still works: %+v", st)
	}
	_, err := u.Download(bg)
	wantCode(t, err, CodeUntrustedDir)
	if st := u.Status(bg); st.State != StateAvailable || st.Pending != nil {
		t.Fatalf("after a refused download: %+v", st)
	}
	src := t.TempDir()
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	_, err = u.ImportPaths(bg, []string{userFile(t, src, name, rel.pkgs[name]),
		userFile(t, src, "update.json.minisig", rel.sig), userFile(t, src, "update.json", rel.raw)})
	wantCode(t, err, CodeUntrustedDir)
	if _, err := os.Stat(filepath.Join(u.Dir(), pendingFile)); !os.IsNotExist(err) {
		t.Fatal("pending.json written into an untrusted data directory")
	}
	// another failure of the check does not stop anything
	checkTrust = func(string) error { return ioErr(errors.New("cannot read")) }
	waitDownloaded(t, u)
}
