// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// CheckAbove (security audit M-4): who can take a folder out of its place
// by removing or renaming a folder above it. Real ACLs in a private
// temporary folder, with well-known groups (Authenticated Users, Users,
// Everyone) standing in for another account; the walk stops at that
// temporary folder (top), so what lies above it on this computer does not
// count.

// aboveTree makes top/<name>/inst with the given DACL for <name> (inst
// itself private) and returns top, <name> and inst.
func aboveTree(t *testing.T, sddl string) (top, mid, inst string) {
	t.Helper()
	top = filepath.Join(t.TempDir(), "top")
	if _, err := MkdirPrivate(top); err != nil {
		t.Fatal(err)
	}
	mid = filepath.Join(top, "Apps")
	mkdirSDDL(t, mid, sddl)
	inst = filepath.Join(mid, "LectureFlow")
	if err := os.Mkdir(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Restrict(inst); err != nil {
		t.Fatal(err)
	}
	return top, mid, inst
}

func TestCheckAboveWindows(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	mine := "(A;OICI;FA;;;" + me.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	for _, c := range []struct {
		name, sddl string
		open       bool
	}{
		// a folder made in D:\ (inheriting "Authenticated Users: modify"):
		// they may rename it, and with it everything in it
		{"modify inherited from D:", "D:P" + mine + "(A;OICI;0x1301bf;;;AU)", true},
		// FILE_DELETE_CHILD for this folder only: they may remove or rename
		// the installation in it
		{"delete child", "D:P" + mine + "(A;;0x40;;;BU)", true},
		{"write DAC", "D:P" + mine + "(A;;WD;;;WD)", true},
		{"generic all", "D:P" + mine + "(A;;GA;;;BU)", true},
		// as D:\ itself: "create folders" for this folder, "modify" only for
		// what lies below (inherit-only) — the folder stays in its place
		{"like the root of D:", "D:P" + mine + "(A;;0x4;;;AU)(A;OICIIO;0x1301bf;;;AU)(A;OICI;0x1200a9;;;BU)", false},
		// reading and running, and deny entries, change nothing
		{"read and execute", "D:P" + mine + "(A;OICI;0x1200a9;;;BU)(D;;FA;;;BG)", false},
		{"private", "D:P" + mine, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			top, mid, inst := aboveTree(t, c.sddl)
			// the installation itself is private either way
			if e, err := Check(inst); err != nil || e.Write {
				t.Fatalf("Check(inst): %+v %v", e, err)
			}
			p, e, err := CheckAbove(inst, top)
			if err != nil {
				t.Fatal(err)
			}
			if !c.open {
				if p != "" || e.Write || len(e.Who) != 0 {
					t.Fatalf("reported: %q %+v", p, e)
				}
				return
			}
			if p != mid || !e.Write || len(e.Who) == 0 || e.Read {
				t.Fatalf("not reported: %q %+v", p, e)
			}
			// from a folder deeper down too (the first folder up counts)
			deep := filepath.Join(inst, "resources", "bin")
			if err := os.MkdirAll(deep, 0o755); err != nil {
				t.Fatal(err)
			}
			if p2, e2, err := CheckAbove(deep, top); err != nil || p2 != mid || !e2.Write {
				t.Fatalf("from below: %q %+v %v", p2, e2, err)
			}
		})
	}
}

// The walk starts at the folder that holds dir and stops below top; a
// missing dir is an error; without top it goes up to the root of the drive
// and reads every folder on the way (what it finds there depends on this
// computer and is not asserted).
func TestCheckAboveBoundsWindows(t *testing.T) {
	top, mid, inst := aboveTree(t, "D:P(A;OICI;FA;;;WD)")
	if p, e, err := CheckAbove(mid, top); err != nil || p != "" || e.Write {
		t.Fatalf("top's own permissions are not looked at: %q %+v %v", p, e, err)
	}
	if p, e, err := CheckAbove(inst, top); err != nil || p != mid || !e.Write {
		t.Fatalf("Everyone: %q %+v %v", p, e, err)
	}
	if _, _, err := CheckAbove(filepath.Join(inst, "missing"), top); !os.IsNotExist(err) {
		t.Fatalf("a missing folder: %v", err)
	}
	if _, _, err := CheckAbove(inst, ""); err != nil {
		t.Fatalf("up to the root: %v", err)
	}
}

// A junction on the way (security review SEC20): the walk does not read
// through it (its own entry is what the system shows there), so the folder
// is not vouched for; FinalPath names where the path really lies, for the
// caller to look at that way too.
func TestCheckAboveLinkWindows(t *testing.T) {
	top := filepath.Join(t.TempDir(), "top")
	if _, err := MkdirPrivate(top); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(top, "weak", "realparent")
	if err := os.MkdirAll(filepath.Join(real, "LectureFlow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(top, "strong"), 0o755); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(top, "strong", "J")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, real).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	t.Cleanup(func() { os.Remove(j) }) // the junction itself, never what it leads to
	inst := filepath.Join(j, "LectureFlow")
	p, e, err := CheckAbove(inst, top)
	if !errors.Is(err, ErrLink) || p != j || e.Write {
		t.Fatalf("through a junction: %q %+v %v", p, e, err)
	}
	// the real path has no link on the way
	fp, err := FinalPath(inst)
	if err != nil || !strings.EqualFold(fp, filepath.Join(real, "LectureFlow")) {
		t.Fatalf("FinalPath: %q %v", fp, err)
	}
	if p, e, err := CheckAbove(fp, top); err != nil || p != "" || e.Write {
		t.Fatalf("the real path: %q %+v %v", p, e, err)
	}
	if fp, err := FinalPath(real); err != nil || !strings.EqualFold(fp, real) {
		t.Fatalf("FinalPath of a plain folder: %q %v", fp, err)
	}
}

// The final path's forms: a drive letter and a share in the usual form, a
// volume without a drive letter kept absolute (SEC20 低-B).
func TestTidyFinalPathWindows(t *testing.T) {
	for in, want := range map[string]string{
		`\\?\D:\Apps\lectureflow`:                    `D:\Apps\lectureflow`,
		`\\?\UNC\nas.example.com\share\lf`:           `\\nas.example.com\share\lf`,
		`\\?\Volume{0e1f2a3b-0000-0000-0000-1}\Apps`: `\\?\Volume{0e1f2a3b-0000-0000-0000-1}\Apps`,
		`\\?\GLOBALROOT\Device\HarddiskVolume9\Apps`: `\\?\GLOBALROOT\Device\HarddiskVolume9\Apps`,
	} {
		got := tidyFinalPath(in)
		if got != want || !filepath.IsAbs(got) {
			t.Errorf("%s: %s (absolute: %v)", in, got, filepath.IsAbs(got))
		}
	}
}

// Exposure.Writers names only the accounts that may change the folder.
func TestWritersWindows(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + me.String() + ")(A;OICI;0x1301bf;;;AU)(A;OICI;0x1200a9;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	j, err := judge(sd, me, func(s *windows.SID) string { return s.String() })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(j.exp.Writers, ",") != "S-1-5-11" || strings.Join(j.exp.Who, ",") != "S-1-5-11,S-1-5-32-545" || !j.exp.Write || !j.exp.Read {
		t.Fatalf("%+v", j.exp)
	}
}

// trustedSIDs names TrustedInstaller by the SID Windows gives it (the
// owner of C:\, Program Files and the system's files).
func TestTrustedInstallerSIDWindows(t *testing.T) {
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\TrustedInstaller`)
	if err != nil {
		t.Fatal(err)
	}
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if !trustedSID(sid, me) {
		t.Fatalf("TrustedInstaller (%s) is not among the trusted accounts", sid)
	}
	const ti = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	if sid.String() != ti {
		t.Fatalf("TrustedInstaller is %s here", sid)
	}
}

// judgeAbove on descriptors that cannot be made here (owners other than
// this account): the owners of system folders count as trusted, another
// account as owner does not; a folder without a DACL lets everyone.
func TestJudgeAboveWindows(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	const ti = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	// C:\Program Files as Windows sets it up
	programFiles := "O:" + ti + "G:" + ti + "D:P(A;;FA;;;" + ti + ")(A;CIIO;GA;;;" + ti + ")(A;;0x1301bf;;;SY)(A;OICIIO;GA;;;SY)" +
		"(A;;0x1301bf;;;BA)(A;OICIIO;GA;;;BA)(A;;0x1200a9;;;BU)(A;OICIIO;GXGR;;;BU)(A;OICIIO;GA;;;CO)(A;;0x1200a9;;;AC)(A;OICIIO;GXGR;;;AC)"
	name := func(s *windows.SID) string { return s.String() }
	for _, c := range []struct {
		sddl   string
		mask   uint32
		open   bool
		naming string
	}{
		{programFiles, rightsRemoveSelf | rightsRemoveChild, false, ""},
		{"O:BAD:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)", rightsRemoveSelf | rightsRemoveChild, false, ""},
		{"O:SYD:P(A;OICI;FA;;;SY)", rightsRemoveSelf | rightsRemoveChild, false, ""},
		{"O:" + me.String() + "D:P(A;OICI;FA;;;" + me.String() + ")", rightsRemoveSelf | rightsRemoveChild, false, ""},
		{"O:BUD:P(A;OICI;FA;;;SY)", rightsRemoveSelf | rightsRemoveChild, true, "S-1-5-32-545"},
		{"O:SYD:", rightsRemoveSelf | rightsRemoveChild, false, ""}, // an empty DACL: nobody
		{"O:SYD:NO_ACCESS_CONTROL", rightsRemoveChild, true, "Everyone"},
		// DELETE on the folder: counts for a folder, not for the root of a drive
		{"O:SYD:(A;;SD;;;AU)", rightsRemoveSelf | rightsRemoveChild, true, "S-1-5-11"},
		{"O:SYD:(A;;SD;;;AU)", rightsRemoveChild, false, ""},
	} {
		sd, err := windows.SecurityDescriptorFromString(c.sddl)
		if err != nil {
			t.Fatalf("%s: %v", c.sddl, err)
		}
		e, err := judgeAbove(sd, me, name, c.mask)
		if err != nil {
			t.Fatalf("%s: %v", c.sddl, err)
		}
		if e.Write != c.open || (c.open && !strings.Contains(strings.Join(e.Who, ","), c.naming)) {
			t.Errorf("%s (mask %#x): %+v", c.sddl, c.mask, e)
		}
	}
}
