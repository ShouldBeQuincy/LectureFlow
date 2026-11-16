// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// mkdirSDDL creates dir with the security descriptor sddl.
func mkdirSDDL(t *testing.T, dir, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(dir)
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, sa); err != nil {
		t.Fatal(err)
	}
}

// A data directory like one created under D:\ (Authenticated Users may
// modify it), or an updates directory another account may change, is not
// trusted.
func TestCheckTrustLooseWindows(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := u.User.Sid.String()
	data := filepath.Join(privateDataDir(t), "D")
	mkdirSDDL(t, data, "D:P(A;OICI;FA;;;"+me+")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;AU)")
	err = CheckTrust(filepath.Join(data, "updates"))
	if CodeOf(err) != CodeUntrustedDir || !strings.Contains(InfoOf(err).Params["who"].(string), "Authenticated Users") && !strings.Contains(InfoOf(err).Params["who"].(string), "S-1-5-11") {
		t.Fatalf("loose data directory: %v %+v", err, InfoOf(err).Params)
	}
	// private data directory, but its updates directory lets others in
	data2 := privateDataDir(t)
	mkdirSDDL(t, filepath.Join(data2, "updates"), "D:P(A;OICI;FA;;;"+me+")(A;OICI;FA;;;WD)")
	if err := CheckTrust(filepath.Join(data2, "updates")); CodeOf(err) != CodeUntrustedDir {
		t.Fatalf("loose updates directory: %v", err)
	}
	os.RemoveAll(data)
}

// What another account left below the updates directory while it could
// write there (security audit M-3): the data directory and the updates
// directory themselves are private again, but a backup folder keeps its
// own entries for other accounts, so nothing in the updates directory is
// trusted. The same for the shell's record of the backups' hashes when the
// shell's profile lies in the data directory.
func TestCheckTrustSubItemsWindows(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := u.User.Sid.String()
	data := privateDataDir(t)
	updates := filepath.Join(data, "updates")
	if err := os.MkdirAll(filepath.Join(updates, "backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrust(updates); err != nil {
		t.Fatalf("private: %v", err)
	}
	backup := filepath.Join(updates, "backup", "3.1.0")
	mkdirSDDL(t, backup, "D:P(A;OICI;FA;;;"+me+")(A;OICI;0x1301bf;;;AU)")
	if err := os.WriteFile(filepath.Join(backup, "LectureFlow.exe"), []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = CheckTrust(updates)
	if CodeOf(err) != CodeUntrustedDir {
		t.Fatalf("a backup folder other accounts may change: %v", err)
	}
	who, _ := InfoOf(err).Params["who"].(string)
	if InfoOf(err).Params["path"] != backup ||
		!strings.Contains(who, "Authenticated Users") && !strings.Contains(who, "S-1-5-11") {
		t.Fatalf("a backup folder other accounts may change: %v %+v", err, InfoOf(err).Params)
	}
	if err := os.RemoveAll(backup); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrust(updates); err != nil {
		t.Fatalf("after removing it: %v", err)
	}

	// the shell's profile in the data directory: its folder and the record
	// count, the browser's own files in it do not
	profile := filepath.Join(data, ShellProfileDir)
	if err := os.Mkdir(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(profile, TrustRecordFile)
	other := filepath.Join(profile, "Preferences")
	for _, f := range []string{record, other} {
		if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loosen := func(p string) {
		t.Helper()
		sd, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;BU)")
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, _ := sd.DACL()
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
			t.Fatal(err)
		}
	}
	loosen(other)
	if err := CheckTrust(updates); err != nil {
		t.Fatalf("another file of the profile: %v", err)
	}
	loosen(record)
	if err := CheckTrust(updates); CodeOf(err) != CodeUntrustedDir || InfoOf(err).Params["path"] != record {
		t.Fatalf("the record other accounts may change: %v", err)
	}
}

// updates/backup as a junction (what an account that could write there can
// leave): VoidRollback removes the link, not what it leads to.
func TestVoidRollbackJunctionWindows(t *testing.T) {
	data := privateDataDir(t)
	outside := filepath.Join(privateDataDir(t), "elsewhere")
	for _, d := range []string{filepath.Join(data, ShellProfileDir), filepath.Join(data, "updates"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(data, "updates", "backup")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("no junction on this system: %v %s", err, out)
	}
	t.Cleanup(func() { os.Remove(link) })
	v, err := VoidRollback(data)
	if err != nil || !v.Backups {
		t.Fatalf("VoidRollback: %+v %v", v, err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the junction is still there: %v", err)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "x" {
		t.Fatalf("what the junction led to was touched: %q %v", b, err)
	}
}
