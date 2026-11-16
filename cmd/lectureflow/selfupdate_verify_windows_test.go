// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A data directory other accounts may change (like one made under D:\,
// where Authenticated Users may modify): exit 3, whatever the files say;
// no copy is made.
func TestSelfUpdateVerifyUntrustedWindows(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;0x1301bf;;;AU)")
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(filepath.Dir(updatesDir(t)), "loose")
	p, _ := windows.UTF16PtrFromString(data)
	if err := windows.CreateDirectory(p, &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, "updates")
	os.Mkdir(dir, 0o755)
	k, _ := writePending(t, dir)
	useKey(t, k)
	tmp := t.TempDir()
	for _, args := range [][]string{
		{"-selfupdate-verify", dir, "-app-version", "3.1.0", "-selfupdate-copy", tmp},
		{"-selfupdate-verify", dir, "-selfupdate-previous", "-app-version", "3.2.0"},
	} {
		code, out := runVerify(t, args...)
		if code != 3 || !strings.HasPrefix(out, "untrusted_dir: ") || strings.Count(out, "\n") != 1 {
			t.Fatalf("%v: %d %q", args, code, out)
		}
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Fatalf("a copy was made: %v", ents)
	}
}

// An installation directory other accounts may change (made in a folder
// like D:\, where Authenticated Users may modify new folders): exit 3 with
// untrusted_install_dir in every mode; no copy, no private folder.
func TestSelfUpdateVerifyInstallDirLooseWindows(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;0x1301bf;;;AU)")
	if err != nil {
		t.Fatal(err)
	}
	dir := updatesDir(t)
	k, _ := writePending(t, dir)
	useKey(t, k)
	install := filepath.Join(filepath.Dir(updatesDir(t)), "LectureFlow")
	p, _ := windows.UTF16PtrFromString(install)
	if err := windows.CreateDirectory(p, &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	for _, extra := range [][]string{{"-selfupdate-trust-check"}, {"-app-version", "3.1.0", "-selfupdate-copy", tmp}, {"-selfupdate-previous", "-app-version", "3.2.0"}, {"-selfupdate-private", "-selfupdate-copy", tmp}} {
		args := append([]string{"-selfupdate-verify", dir, "-selfupdate-install-dir", install}, extra...)
		code, out := runVerify(t, args...)
		if code != 3 || !strings.HasPrefix(out, "untrusted_install_dir: ") || !strings.Contains(out, install) || strings.Count(out, "\n") != 1 {
			t.Fatalf("%v: %d %q", extra, code, out)
		}
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Fatalf("something was made: %v", ents)
	}
}
