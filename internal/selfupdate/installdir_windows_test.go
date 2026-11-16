// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"lectureflow/internal/dirperm"
)

// Installations on a drive like D:\ (security audit M-4), with real ACLs in
// a private temporary folder: "top" stands for D:\ (Authenticated Users may
// create folders in it, and modify what lies below it), the walk above the
// installation stops there. Never a real installation or Program Files.

func mySID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// driveLike makes a folder with the permissions of the root of a freshly
// formatted NTFS drive (and stops CheckInstallDir there).
func driveLike(t *testing.T) string {
	t.Helper()
	me := mySID(t)
	top := filepath.Join(privateDataDir(t), "D")
	mkdirSDDL(t, top, "D:P(A;OICI;FA;;;"+me+")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;;0x4;;;AU)(A;OICIIO;0x1301bf;;;AU)(A;OICI;0x1200a9;;;BU)")
	stopAbove(t, filepath.Dir(top))
	return top
}

// installInto makes an installation in parent the way the installer's
// files come to be: plain folders and files that inherit what parent
// passes down.
func installInto(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "LectureFlow")
	for _, f := range []string{"LectureFlow.exe", "Uninstall LectureFlow.exe", `resources\app.asar`, `resources\bin\lectureflow.exe`} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("MZ"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// installerIcacls is the icacls command of desktop/packaging/installer.nsh
// for dir and the user sid: the installer's own line, with its NSIS
// variables filled in.
func installerIcacls(t *testing.T, dir, sid string) (string, []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "desktop", "packaging", "installer.nsh"))
	if err != nil {
		t.Fatal(err)
	}
	lines := regexp.MustCompile(`(?m)^\s*nsExec::ExecToLog '(.*icacls\.exe.*)'\s*$`).FindAllStringSubmatch(string(b), -1)
	if len(lines) != 1 {
		t.Fatalf("installer.nsh runs icacls %d times", len(lines))
	}
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.NewReplacer("$SYSDIR", sys, "$R0", dir, "$R1", sid).Replace(lines[0][1])
	if strings.Contains(cmd, "$") {
		t.Fatalf("an NSIS variable left: %s", cmd)
	}
	// NSIS passes the line as it is; split it like a command line without
	// escapes (no argument has a quote or ends in a backslash)
	var args []string
	for _, m := range regexp.MustCompile(`"([^"]*)"|(\S+)`).FindAllStringSubmatch(cmd, -1) {
		args = append(args, m[1]+m[2])
	}
	return args[0], args[1:]
}

func runIcacls(t *testing.T, exe string, args []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, exe, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %q: %v %s", exe, args, err, out)
	}
}

// Installed in a new folder of D:\ the installation lets every signed-in
// account change it; the installer's icacls makes it private, and then it
// is vouched for — also once its files were replaced by an update.
func TestInstallDirOnDriveWindows(t *testing.T) {
	top := driveLike(t)
	dir := installInto(t, top)
	err := CheckInstallDir(dir)
	wantUntrustedInstall(t, "fresh on D:", err, dir, "")
	if who := InfoOf(err).Params["who"].(string); !strings.Contains(who, "Authenticated Users") && !strings.Contains(who, "S-1-5-11") {
		t.Fatalf("who: %q", who)
	}
	// only those that may change it: Users may only read and run (SEC20)
	for _, n := range strings.Split(InfoOf(err).Params["who"].(string), ", ") {
		if n == "Users" || n == "S-1-5-32-545" {
			t.Fatalf("a reader among who: %q", InfoOf(err).Params["who"])
		}
	}
	exe, args := installerIcacls(t, dir, mySID(t))
	if !strings.EqualFold(filepath.Base(exe), "icacls.exe") || !filepath.IsAbs(exe) {
		t.Fatalf("the installer runs %s", exe)
	}
	runIcacls(t, exe, args)
	if err := CheckInstallDir(dir); err != nil {
		t.Fatalf("after the installer's icacls: %v", err)
	}
	// what lies below inherited the new permissions
	if err := os.WriteFile(filepath.Join(dir, "resources", "bin", "new.exe"), []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckInstallDir(dir); err != nil {
		t.Fatalf("a new file: %v", err)
	}
	// running it again (an update) keeps it so
	runIcacls(t, exe, args)
	if err := CheckInstallDir(dir); err != nil {
		t.Fatalf("again: %v", err)
	}
}

// What other accounts left: an entry below the installation that names
// them, or a folder above it they may rename.
func TestInstallDirForeignWindows(t *testing.T) {
	me := mySID(t)
	top := driveLike(t)
	dir := installInto(t, top)
	exe, args := installerIcacls(t, dir, me)
	runIcacls(t, exe, args)
	// a file of the installation that Users may change
	p := filepath.Join(dir, "resources", "app.asar")
	sd, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	wantUntrustedInstall(t, "an entry below", CheckInstallDir(dir), p, "")
	runIcacls(t, "icacls", []string{p, "/reset", "/Q"})
	if err := CheckInstallDir(dir); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// D:\Apps\LectureFlow: the installation is private, but D:\Apps is a
	// folder of D:\ that every signed-in account may rename
	apps := filepath.Join(top, "Apps")
	if err := os.Mkdir(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := installInto(t, apps)
	exe, args = installerIcacls(t, inner, me)
	runIcacls(t, exe, args)
	wantUntrustedInstall(t, "a folder above", CheckInstallDir(inner), apps, "")

	// a folder above that lets them remove what it holds
	loose := filepath.Join(privateDataDir(t), "share")
	mkdirSDDL(t, loose, "D:P(A;OICI;FA;;;"+me+")(A;;0x40;;;BU)")
	stopAbove(t, filepath.Dir(loose))
	inLoose := installInto(t, loose)
	if err := CheckInstallDir(inLoose); CodeOf(err) != CodeUntrustedInstallDir || InfoOf(err).Params["path"] != loose {
		t.Fatalf("delete child: %v", err)
	}
}

// A junction on the path (security review SEC20): reached through it the
// installation is not vouched for, whatever lies behind it.
func TestInstallDirLinkAboveWindows(t *testing.T) {
	base := privateDataDir(t)
	stopAbove(t, filepath.Dir(base))
	real := filepath.Join(base, "weak")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := installInto(t, real)
	if err := CheckInstallDir(dir); err != nil {
		t.Fatalf("the real path: %v", err)
	}
	if err := os.Mkdir(filepath.Join(base, "strong"), 0o755); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(base, "strong", "J")
	dirLink(t, j, real)
	t.Cleanup(func() { os.Remove(j) })
	wantUntrustedInstall(t, "through a junction", CheckInstallDir(filepath.Join(j, "LectureFlow")), j, dirperm.NotConfirmed)
}

// Without any stand-in: a folder of Windows itself (owned by
// TrustedInstaller, readable by Users, the folders above it up to C:\
// Windows' own) is vouched for, the whole way up to the root of the drive.
// Fails with a wrong SID for TrustedInstaller.
func TestInstallDirSystemFolderWindows(t *testing.T) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckInstallDir(filepath.Join(sys, "drivers", "etc")); err != nil {
		t.Fatalf("a folder of Windows: %v", err)
	}
}

// Installed in a folder that admits the user alone (the default, under
// %LOCALAPPDATA%): vouched for without the installer's icacls; a folder
// like Program Files (owned by TrustedInstaller, readable by Users) would
// pass too (see dirperm's TestJudgeAboveWindows: an owner of the system
// cannot be set here).
func TestInstallDirInProfileWindows(t *testing.T) {
	base := privateDataDir(t)
	stopAbove(t, filepath.Dir(base))
	programs := filepath.Join(base, "Programs")
	if err := os.Mkdir(programs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckInstallDir(installInto(t, programs)); err != nil {
		t.Fatalf("in the profile: %v", err)
	}
}
