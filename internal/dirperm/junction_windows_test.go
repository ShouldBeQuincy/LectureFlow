// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// junctionBase is a private folder for the junctions of a test (so that a
// link made in it is private too, whatever the temporary directory lets
// others do).
func junctionBase(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "base")
	if _, err := MkdirPrivate(base); err != nil {
		t.Fatal(err)
	}
	return base
}

// mkJunction makes link a junction to target (mklink /J needs no
// privilege); the link itself, never what it leads to, goes again at the
// end.
func mkJunction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v %s", link, target, err, out)
	}
	t.Cleanup(func() { os.Remove(link) })
}

// finalOf is FinalPath of p (the long names of a temporary directory that
// has short ones, as on CI runners).
func finalOf(t *testing.T, p string) string {
	t.Helper()
	fp, err := FinalPath(p)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// setLinkEntries gives the entry at p (a link: not followed) exactly the
// DACL of sddl, without passing anything on to other entries.
func setLinkEntries(t *testing.T, p, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	h, err := openEntry(p, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// What security review SEC17 (低-2) found with its probes, kept: a data
// directory that is a junction is a link to the walk and to Check — Check
// reads the link's own permissions, not those of the folder it leads to,
// and Restrict sets the link's. Junction tells where it leads.
func TestJunctionRootIsTheLinkSEC17(t *testing.T) {
	base := junctionBase(t)
	target := looseDir(t, base) // Authenticated Users may modify it
	link := filepath.Join(base, "data")
	mkJunction(t, link, target)

	if fi, err := os.Lstat(link); err != nil || fi.IsDir() || fi.Mode().IsRegular() {
		t.Fatalf("Lstat of a junction: %v %v", fi.Mode(), err)
	}
	if real, err := filepath.EvalSymlinks(link); err == nil {
		t.Logf("EvalSymlinks of a junction: %s (Go 1.23 and later keep the junction)", real)
	}
	rep, err := CheckTree(link, TreeOptions{})
	if err != nil || rep.OK() || !strings.Contains(rep.Unconfirmed, "is a link") {
		t.Fatalf("a walk from a junction: %+v %v", rep, err)
	}
	el, err := Check(link)
	if err != nil {
		t.Fatal(err)
	}
	et, err := Check(target)
	if err != nil {
		t.Fatal(err)
	}
	if el.Write || !et.Write {
		t.Fatalf("Check of the link %+v, of the folder %+v: the link's own entries are read", el, et)
	}
	before := sddlOf(t, target)
	if err := Restrict(link); err != nil {
		t.Fatal(err)
	}
	if got := sddlOf(t, target); got != before {
		t.Fatalf("Restrict of the link changed the folder: %s → %s", before, got)
	}

	info, ok, err := Junction(link)
	if !ok || err != nil || info.Target != finalOf(t, target) || info.Link.Write {
		t.Fatalf("Junction: %+v %v %v", info, ok, err)
	}
}

// Junction says nothing about what is not a junction.
func TestJunctionNotAJunction(t *testing.T) {
	base := junctionBase(t)
	f := filepath.Join(base, "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{base, f} {
		if info, ok, err := Junction(p); ok || err != nil || info.Target != "" {
			t.Errorf("%s: %+v %v %v", p, info, ok, err)
		}
	}
	if _, ok, err := Junction(filepath.Join(base, "missing")); ok || err == nil {
		t.Errorf("a missing folder: %v %v", ok, err)
	}
}

// A junction to a private folder; the link's own permissions decide
// Link.Write (whoever may change the link may point it elsewhere).
func TestJunctionLinkPermissions(t *testing.T) {
	base := junctionBase(t)
	target := filepath.Join(base, "real")
	if _, err := MkdirPrivate(target); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "data")
	mkJunction(t, link, target)
	info, ok, err := Junction(link)
	if !ok || err != nil || info.Target != finalOf(t, target) || info.Link.Write ||
		info.LinkPath != filepath.Join(finalOf(t, base), "data") {
		t.Fatalf("a private junction: %+v %v %v", info, ok, err)
	}
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	before := sddlOf(t, target)
	setLinkEntries(t, link, "D:P(A;;FA;;;"+me.String()+")(A;;0x1301bf;;;BU)")
	if got := sddlOf(t, target); got != before {
		t.Fatalf("setting the link's entries changed the folder: %s", got)
	}
	info, ok, err = Junction(link)
	if !ok || err != nil || !info.Link.Write || len(info.Link.Writers) == 0 {
		t.Fatalf("a junction Users may change: %+v %v %v", info, ok, err)
	}
	// write attributes alone are enough to rewrite a reparse point
	setLinkEntries(t, link, "D:P(A;;FA;;;"+me.String()+")(A;;0x100;;;BU)")
	if info, _, _ := Junction(link); !info.Link.Write {
		t.Fatalf("FILE_WRITE_ATTRIBUTES on the link: %+v", info)
	}
}

// Junctions that cannot be vouched for: what they lead to is missing, they
// form a loop, they lead to another junction or through one, they lead to
// a file. Each is a junction with an error, and no Target.
func TestJunctionUnresolved(t *testing.T) {
	base := junctionBase(t)
	real := filepath.Join(base, "real")
	if _, err := MkdirPrivate(real); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(base, "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	mkJunction(t, filepath.Join(base, "toGone"), gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	mkJunction(t, filepath.Join(base, "self"), filepath.Join(base, "self"))
	mkJunction(t, filepath.Join(base, "loopA"), filepath.Join(base, "loopB"))
	mkJunction(t, filepath.Join(base, "loopB"), filepath.Join(base, "loopA"))
	mkJunction(t, filepath.Join(base, "first"), real)
	mkJunction(t, filepath.Join(base, "second"), filepath.Join(base, "first"))
	mkJunction(t, filepath.Join(base, "through"), filepath.Join(base, "first", "sub"))
	f := filepath.Join(base, "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mkJunction(t, filepath.Join(base, "toFile"), f)

	for _, name := range []string{"toGone", "self", "loopA", "second", "through", "toFile"} {
		info, ok, err := Junction(filepath.Join(base, name))
		if !ok || err == nil || info.Target != "" {
			t.Errorf("%s: %+v %v %v", name, info, ok, err)
			continue
		}
		t.Logf("%s: %v", name, err)
	}
	// the one in the middle of the chain is fine on its own
	if info, ok, err := Junction(filepath.Join(base, "first")); !ok || err != nil || info.Target != finalOf(t, real) {
		t.Errorf("first: %+v %v %v", info, ok, err)
	}
}

// mountPointTarget reads the path a junction holds, as the system keeps it.
func TestMountPointTarget(t *testing.T) {
	base := junctionBase(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "data")
	mkJunction(t, link, real)
	h, err := openEntry(link, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	got, err := mountPointTarget(h)
	if err != nil || got != `\??\`+real {
		t.Fatalf("%q %v", got, err)
	}
}

// Security review SEC30, 低-1: the junction names a link whose name differs
// from the folder it leads to only by a Unicode case variant (KELVIN SIGN
// for k). strings.EqualFold took the two paths for one, so the link on the
// way went unseen; NTFS keeps the two names apart.
func TestJunctionThroughCaseVariantLink(t *testing.T) {
	base := finalOf(t, junctionBase(t))
	real := filepath.Join(base, "kreal")
	if _, err := MkdirPrivate(real); err != nil {
		t.Fatal(err)
	}
	middle := filepath.Join(base, "\u212Areal")
	mkJunction(t, middle, real)
	if fi, err := os.Lstat(real); err != nil || !fi.IsDir() {
		t.Fatalf("NTFS keeps the two names apart: %v %v", fi, err)
	}
	link := filepath.Join(base, "data")
	mkJunction(t, link, middle)
	info, ok, err := Junction(link)
	if !ok || err == nil || info.Target != "" {
		t.Fatalf("a junction through a case-variant link: %+v %v %v", info, ok, err)
	}
	t.Log(err)
}

func TestSameNamedPath(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{`C:\Data\LF`, `C:\Data\LF`, true},
		{`c:\data\lf`, `C:\DATA\LF`, true},
		{`C:\Données`, `C:\Données`, true},
		{`C:\kreal`, "C:\\\u212Areal", false}, // KELVIN SIGN
		{`C:\sx`, "C:\\\u017Fx", false},       // LONG S
		{"C:\\\u03C9", "C:\\\u2126", false},   // OHM SIGN
		{`C:\DONNÉES`, `C:\données`, false},   // non-ASCII case: not confirmed
		{`C:\a`, `C:\a\`, false},
		{`C:\a[`, `C:\a{`, false}, // '[' + 32 is '{': not letters
		{`C:\a@`, "C:\\a`", false},
	} {
		if got := sameNamedPath(c.a, c.b); got != c.same {
			t.Errorf("sameNamedPath(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// The path a junction holds (security review SEC30, 信息-4): a folder on a
// drive letter, clean; never a drive's root ("收紧权限" would make the whole
// drive private), a volume, a share or a device. No junction is made for
// these: the parsing is checked on its own.
func TestSubstitutePath(t *testing.T) {
	for _, c := range []struct {
		raw, want string // want "": refused
	}{
		{`\??\C:\Data\LF`, `C:\Data\LF`},
		{`\??\c:\data`, `c:\data`},
		{`\??\D:\LF\`, `D:\LF`},
		{`\??\D:\a\.\b`, `D:\a\b`},
		{`\??\C:\`, ""},
		{`\??\C:\\`, ""},
		{`\??\C:\..`, ""},
		{`\??\C:\LF\..`, ""},
		{`\??\C:\LF\..\..`, ""},
		{`\??\C:`, ""},
		{`\??\C:LF`, ""},
		{`\??\Volume{0b3f1c2a-0000-0000-0000-100000000000}\`, ""},
		{`\??\Volume{0b3f1c2a-0000-0000-0000-100000000000}\LF`, ""},
		{`\??\UNC\server\share\LF`, ""},
		{`\??\GLOBALROOT\Device\HarddiskVolume3\LF`, ""},
		{`\\?\C:\LF`, ""},
		{`\\.\C:\LF`, ""},
		{`C:\LF`, ""},
		{`\??\1:\LF`, ""},
		{`\??\`, ""},
		{``, ""},
	} {
		got, err := substitutePath(c.raw)
		if got != c.want || (err == nil) != (c.want != "") {
			t.Errorf("substitutePath(%q) = %q, %v; want %q", c.raw, got, err, c.want)
		}
	}
}

// What the link leads to must be the folder at the path, the same file:
// any difference in volume or file index is refused, and so is a link or a
// file at the path.
func TestSameDirInfo(t *testing.T) {
	dir := windows.ByHandleFileInformation{FileAttributes: windows.FILE_ATTRIBUTE_DIRECTORY,
		VolumeSerialNumber: 7, FileIndexHigh: 1, FileIndexLow: 2}
	if err := sameDirInfo(`C:\LF`, dir, dir); err != nil {
		t.Fatalf("the same folder: %v", err)
	}
	for name, change := range map[string]func(*windows.ByHandleFileInformation){
		"another volume":     func(i *windows.ByHandleFileInformation) { i.VolumeSerialNumber++ },
		"another index high": func(i *windows.ByHandleFileInformation) { i.FileIndexHigh++ },
		"another index low":  func(i *windows.ByHandleFileInformation) { i.FileIndexLow++ },
		"a link":             func(i *windows.ByHandleFileInformation) { i.FileAttributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT },
		"a file":             func(i *windows.ByHandleFileInformation) { i.FileAttributes = windows.FILE_ATTRIBUTE_ARCHIVE },
	} {
		named := dir
		change(&named)
		if err := sameDirInfo(`C:\LF`, dir, named); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// linkOnTheWay finds a junction anywhere on a path, the last folder
// included, and nothing on a path of plain folders.
func TestLinkOnTheWay(t *testing.T) {
	base := finalOf(t, junctionBase(t))
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(base, "j")
	mkJunction(t, j, real)
	if err := linkOnTheWay(filepath.Join(real, "sub")); err != nil {
		t.Fatalf("plain folders: %v", err)
	}
	for _, p := range []string{j, filepath.Join(j, "sub")} {
		if err := linkOnTheWay(p); err == nil || !strings.Contains(err.Error(), j) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := linkOnTheWay(filepath.Join(base, "missing", "x")); err == nil {
		t.Error("a missing folder is not vouched for")
	}
}
