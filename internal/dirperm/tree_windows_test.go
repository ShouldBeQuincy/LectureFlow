// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These tests use real ACLs with well-known SIDs (Everyone, Users) standing
// in for another account; a file another account owns cannot be made with
// one account, so the owner tests replace who the walk takes for the user.

// mkdirSDDL creates dir with the DACL of sddl.
func mkdirSDDL(t *testing.T, dir, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	if err := createPrivate(dir, sd); err != nil {
		t.Fatal(err)
	}
}

// setEntries gives p the entries of sddl as its own (it keeps inheriting);
// protected replaces the whole DACL instead.
func setEntries(t *testing.T, p, sddl string, protected bool) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if protected {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func findingPaths(r TreeReport) string {
	var ps []string
	for _, f := range r.Findings {
		ps = append(ps, f.Path)
	}
	return strings.Join(ps, "\n")
}

// What another account left below a data directory while it could write
// there (audit M-3): a folder with its own entries for other accounts, and
// a file that names another account outright. The directory itself is
// private (Check passes); CheckTree reports the entries, RestrictTree
// resets them, and then the tree is OK.
func TestCheckAndRestrictTreeForeignEntries(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	root := privateTree(t)
	// a folder made while "Authenticated Users: modify" was inherited, kept
	// as its own entries (as a protected folder keeps them)
	backup := filepath.Join(root, "updates", "backup", "3.0.0")
	mkdirSDDL(t, backup, "D:P(A;OICI;FA;;;"+me.String()+")(A;OICI;0x1301bf;;;AU)")
	exe := filepath.Join(backup, "LectureFlow.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a file of the Python environment that names Users itself
	cfg := filepath.Join(root, "components", "whisper", "1.0", "venv", "pyvenv.cfg")
	setEntries(t, cfg, "D:(A;;FW;;;BU)", false)

	for _, d := range []string{root, filepath.Join(root, "updates")} {
		if e, err := Check(d); err != nil || e.Write {
			t.Fatalf("%s itself: %+v %v", d, e, err)
		}
	}
	rep, err := CheckTree(root, TreeOptions{})
	if err != nil || rep.OK() || rep.Unconfirmed != "" || rep.Entries != privateTreeEntries+2 {
		t.Fatalf("CheckTree: %+v %v", rep, err)
	}
	got := findingPaths(rep)
	for _, p := range []string{backup, exe, cfg} {
		if !strings.Contains(got, p) {
			t.Errorf("%s is not reported:\n%s", p, got)
		}
	}
	if len(rep.Findings) != 3 {
		t.Fatalf("findings:\n%s", got)
	}
	if p, who := rep.Problem(); p == "" || who == "" || strings.HasPrefix(who, "not confirmed") {
		t.Fatalf("Problem: %q %q", p, who)
	}
	// only the tree asked about
	if rep, err := CheckTree(filepath.Join(root, "updates"), TreeOptions{}); err != nil || len(rep.Findings) != 2 {
		t.Fatalf("updates: %+v %v", rep, err)
	}

	// (the file inside the folder had the folder's entries: resetting the
	// folder was enough for it)
	fixed, err := RestrictTree(root, TreeOptions{})
	if err != nil || len(fixed) != 2 || fixed[0] != cfg || fixed[1] != backup {
		t.Fatalf("RestrictTree: %v %v", fixed, err)
	}
	if rep, err := CheckTree(root, TreeOptions{}); err != nil || !rep.OK() {
		t.Fatalf("after RestrictTree: %+v %v", rep, err)
	}
	for _, p := range []string{backup, exe, cfg} {
		s := sddlOf(t, p)
		if strings.Contains(s, ";AU)") || strings.Contains(s, ";BU)") || strings.HasPrefix(s, "D:P") ||
			!strings.Contains(s, "ID;FA;;;"+sddlToken(t, me.String())+")") || strings.Count(s, "(") != 3 {
			t.Errorf("%s after the reset: %s", p, s)
		}
	}
	// the root's own DACL is not RestrictTree's to change
	if got, want := sddlOf(t, root), "D:P(A;OICI;FA;;;"+sddlToken(t, me.String())+")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"; got != want {
		t.Fatalf("root: %s, want %s", got, want)
	}
	if fi, err := os.ReadFile(exe); err != nil || string(fi) != "MZ" {
		t.Fatalf("the file is still readable and unchanged: %q %v", fi, err)
	}
}

// An entry whose permissions cannot be reset is an error, and the tree
// stays reported: nothing is called private that is not.
func TestRestrictTreeCannotReset(t *testing.T) {
	root := privateTree(t)
	// the owner may only read it, everyone may modify it: nobody here may
	// change its permissions
	stuck := filepath.Join(root, "updates", "3.2.0", "update.json")
	setEntries(t, stuck, "D:P(A;;0x1200a9;;;OW)(A;;0x1301bf;;;WD)", true)
	fixed, err := RestrictTree(root, TreeOptions{})
	if err == nil || !strings.Contains(err.Error(), stuck) || len(fixed) != 0 {
		t.Fatalf("RestrictTree: %v %v", fixed, err)
	}
	rep, cerr := CheckTree(root, TreeOptions{})
	if cerr != nil || rep.OK() || len(rep.Findings) != 1 || rep.Findings[0].Path != stuck {
		t.Fatalf("after a failed reset: %+v %v", rep, cerr)
	}
	// the root of the walk has entries for others: RestrictTree leaves them
	// to Restrict and does not pretend
	loose := looseDir(t, t.TempDir())
	if _, err := RestrictTree(loose, TreeOptions{}); err == nil {
		t.Fatal("a loose root passed RestrictTree")
	}
	if e, _ := Check(loose); !e.Write {
		t.Fatalf("RestrictTree changed the root's permissions: %+v", e)
	}
	if err := Restrict(loose); err != nil {
		t.Fatal(err)
	}
	if _, err := RestrictTree(loose, TreeOptions{}); err != nil {
		t.Fatalf("after Restrict: %v", err)
	}
}

// Entries another account owns (injected: the walk takes another SID for
// the user, so the files this account made count as someone else's): they
// are reported with their owner, and an owner that cannot be replaced is an
// error.
func TestTreeForeignOwnerInjected(t *testing.T) {
	root := privateTree(t)
	orig := treeSelf
	t.Cleanup(func() { treeSelf = orig })
	treeSelf = func() (*windows.SID, error) {
		return windows.StringToSid("S-1-5-21-1111111111-2222222222-3333333333-1001")
	}

	rep, err := CheckTree(filepath.Join(root, "updates", "3.2.0"), TreeOptions{})
	if err != nil || rep.OK() || len(rep.Findings) != 2 || len(rep.Findings[0].Who) == 0 {
		t.Fatalf("CheckTree: %+v %v", rep, err)
	}
	// (the account running the tests may be an administrator, whose files
	// belong to the Administrators group: trusted for everyone)
	if _, err := RestrictTree(filepath.Join(root, "updates", "3.2.0"), TreeOptions{}); err == nil {
		t.Fatal("an owner that cannot be replaced passed RestrictTree")
	}
	treeSelf = orig
	if rep, err := CheckTree(root, TreeOptions{}); err != nil || !rep.OK() {
		t.Fatalf("for the real user: %+v %v", rep, err)
	}
}

// A junction is never entered and makes the tree unconfirmed, as the root
// of the walk too; RestrictTree does not take it over.
func TestCheckTreeJunction(t *testing.T) {
	root := privateTree(t)
	outside := looseDir(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(outside, "evil.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "components", "whisper", "1.0", "site")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("no junction on this system: %v %s", err, out)
	}
	t.Cleanup(func() { os.Remove(link) }) // the link, not what it leads to
	rep, err := CheckTree(root, TreeOptions{})
	if err != nil || rep.OK() || rep.Entries != privateTreeEntries+1 || len(rep.Findings) != 0 ||
		!strings.Contains(rep.Unconfirmed, link) || !strings.Contains(rep.Unconfirmed, "link") {
		t.Fatalf("junction: %+v %v", rep, err)
	}
	if _, err := RestrictTree(root, TreeOptions{}); err == nil || !strings.Contains(err.Error(), link) {
		t.Fatalf("RestrictTree with a junction: %v", err)
	}
	if e, _ := Check(outside); !e.Write {
		t.Fatal("RestrictTree changed what the junction leads to")
	}
	rep, err = CheckTree(link, TreeOptions{})
	if err != nil || rep.OK() || rep.Entries != 1 || !strings.Contains(rep.Unconfirmed, "is a link") {
		t.Fatalf("a junction as the root: %+v %v", rep, err)
	}
}

// The empty ACL RestrictTree writes is a DACL without entries, not a
// missing one.
func TestEmptyACL(t *testing.T) {
	a := emptyACL()
	if a == nil || a.AceCount != 0 {
		t.Fatalf("%+v", a)
	}
	root := privateTree(t)
	f := filepath.Join(root, "lectureflow.db")
	setEntries(t, f, "D:(A;;FA;;;WD)", false)
	if _, err := RestrictTree(root, TreeOptions{}); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if dacl, _, err := sd.DACL(); err != nil || dacl == nil || dacl.AceCount != 3 {
		t.Fatalf("DACL after the reset: %v %v (%s)", dacl, err, sd.String())
	}
}

// aceTypes are the kinds (ACE_HEADER.AceType) of the entries p has itself,
// inherited ones left out.
func aceTypes(t *testing.T, p string) []byte {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("DACL of %s: %v", p, err)
	}
	var kinds []byte
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			kinds = append(kinds, ace.Header.AceType)
		}
	}
	return kinds
}

// rawEntryACL is a DACL with one allow entry of the given kind for sid,
// written byte by byte: an object entry (kinds 5 and 11) without any GUID
// cannot be written as SDDL, which turns it into a plain one.
func rawEntryACL(kind byte, mask uint32, sid *windows.SID) *windows.ACL {
	sidLen := int(windows.GetLengthSid(sid))
	object := kind == 5 || kind == 11
	aceLen := 8 + sidLen
	if object {
		aceLen += 4 // the flags: no GUID present
	}
	b := make([]byte, 8+aceLen)
	b[0] = 4 // ACL_REVISION_DS: object entries need it
	binary.LittleEndian.PutUint16(b[2:], uint16(len(b)))
	binary.LittleEndian.PutUint16(b[4:], 1)
	a := b[8:]
	a[0] = kind
	binary.LittleEndian.PutUint16(a[2:], uint16(aceLen))
	binary.LittleEndian.PutUint32(a[4:], mask)
	off := 8
	if object {
		off = 12
	}
	copy(a[off:], unsafe.Slice((*byte)(unsafe.Pointer(sid)), sidLen))
	return (*windows.ACL)(unsafe.Pointer(&b[0]))
}

// Allow entries of every kind count (security review SEC17, 高-1): an
// "object" allow entry (kind 5, which no usual tool writes) gives a file's
// rights like a plain one, with its account at another place in the entry
// (after the flags and the GUIDs that are there); callback entries (9, 11)
// may give them. Each is reported with the account it names, by Check and
// by CheckTree, and RestrictTree removes it.
func TestTreeAllowEntriesOfEveryKind(t *testing.T) {
	const guid1, guid2 = "bf967aba-0de6-11d0-a285-00aa003049e2", "bf967a86-0de6-11d0-a285-00aa003049e2"
	users, err := windows.StringToSid("S-1-5-32-545")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, sddl string // sddl "": written byte by byte
		kind       byte
	}{
		{"object allow, no GUID", "", 5},
		{"object allow, object GUID", "D:(OA;;FA;" + guid1 + ";;BU)", 5},
		{"object allow, inherited object GUID", "D:(OA;;FA;;" + guid2 + ";BU)", 5},
		{"object allow, both GUIDs", "D:(OA;;FA;" + guid1 + ";" + guid2 + ";BU)", 5},
		{"callback allow", `D:(XA;;FA;;;BU;(Member_of {SID(BU)}))`, 9},
		{"callback object allow, no GUID", "", 11},
		{"callback object allow, object GUID", `D:(ZA;;FA;` + guid1 + `;;BU;(Member_of {SID(BU)}))`, 11},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := privateTree(t)
			f := filepath.Join(root, "updates", "backup", "3.1.0", "app", "LectureFlow.exe")
			if c.sddl != "" {
				setEntries(t, f, c.sddl, false)
			} else if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT,
				windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, rawEntryACL(c.kind, 0x1f01ff, users), nil); err != nil {
				t.Fatal(err)
			}
			if got := aceTypes(t, f); len(got) != 1 || got[0] != c.kind {
				t.Fatalf("the file has entries of kinds %v, want one of kind %d (%s)", got, c.kind, sddlOf(t, f))
			}
			e, err := Check(f)
			if err != nil || !e.Write || len(e.Who) != 1 || !strings.Contains(e.Who[0], "Users") && e.Who[0] != "S-1-5-32-545" {
				t.Fatalf("Check: %+v %v", e, err)
			}
			rep, err := CheckTree(root, TreeOptions{})
			if err != nil || rep.Unconfirmed != "" || len(rep.Findings) != 1 || rep.Findings[0].Path != f ||
				len(rep.Findings[0].Who) != 1 || rep.Findings[0].Who[0] != e.Who[0] {
				t.Fatalf("CheckTree: %+v %v", rep, err)
			}
			fixed, err := RestrictTree(root, TreeOptions{})
			if err != nil || len(fixed) != 1 || fixed[0] != f {
				t.Fatalf("RestrictTree: %v %v", fixed, err)
			}
			if got := aceTypes(t, f); len(got) != 0 {
				t.Fatalf("entries of kinds %v are left (%s)", got, sddlOf(t, f))
			}
			if rep, err := CheckTree(root, TreeOptions{}); err != nil || !rep.OK() {
				t.Fatalf("after RestrictTree: %+v %v", rep, err)
			}
		})
	}
	// deny entries of these kinds take away only
	root := privateTree(t)
	f := filepath.Join(root, "lectureflow.db")
	setEntries(t, f, "D:(OD;;0x2;"+guid1+";;BU)(D;;0x2;;;WD)", false)
	if got := aceTypes(t, f); len(got) != 2 || got[0] != 6 || got[1] != 1 {
		t.Fatalf("deny entries of kinds %v (%s)", got, sddlOf(t, f))
	}
	if rep, err := CheckTree(root, TreeOptions{}); err != nil || !rep.OK() {
		t.Fatalf("deny entries: %+v %v", rep, err)
	}
}

// An entry of a kind this program does not know, or one too short to hold
// an account, is taken to allow: reported, never passed over.
func TestJudgeUnknownEntries(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	name := func(sid *windows.SID) string { return sid.String() }
	for _, c := range []struct {
		name   string
		change func(*windows.ACCESS_ALLOWED_ACE)
		who    string
	}{
		{"plain allow", func(*windows.ACCESS_ALLOWED_ACE) {}, "S-1-5-32-545"},
		{"compound allow", func(a *windows.ACCESS_ALLOWED_ACE) { a.Header.AceType = 4 }, "an entry of kind 4"},
		{"a kind of the future", func(a *windows.ACCESS_ALLOWED_ACE) { a.Header.AceType = 77 }, "an entry of kind 77"},
		{"an audit entry in the DACL", func(a *windows.ACCESS_ALLOWED_ACE) { a.Header.AceType = 2 }, "an entry of kind 2"},
		{"too short for an account", func(a *windows.ACCESS_ALLOWED_ACE) { a.Header.AceSize = 12 }, "an entry of kind 0"},
		{"an object entry whose GUIDs leave no room", func(a *windows.ACCESS_ALLOWED_ACE) { a.Header.AceType = 5; a.SidStart = 3 }, "an entry of kind 5"},
	} {
		// only its own account allowed to write, then Users: read, as an
		// entry whose kind (or size) is changed in place
		sd, err := windows.SecurityDescriptorFromString("D:(A;;0x1200a9;;;BU)")
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, _ := sd.DACL()
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		c.change(ace)
		j, err := judge(sd, me, name)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		plain := c.name == "plain allow"
		if j.exp.Write == plain || j.badACL == plain || len(j.exp.Who) != 1 || j.exp.Who[0] != c.who {
			t.Errorf("%s: %+v", c.name, j)
		}
	}
	// the deny kinds are never taken to allow
	for _, kind := range []byte{1, 6, 10, 12} {
		sd, _ := windows.SecurityDescriptorFromString("D:(A;;FA;;;BU)")
		dacl, _, _ := sd.DACL()
		var ace *windows.ACCESS_ALLOWED_ACE
		windows.GetAce(dacl, 0, &ace)
		ace.Header.AceType = kind
		if j, err := judge(sd, me, name); err != nil || j.exp.Exposed() || j.badACL {
			t.Errorf("deny kind %d: %+v %v", kind, j, err)
		}
	}
}

// The rights that count as "may change" (SEC17, 中-1): writing attributes
// alone is enough to turn an empty folder into a junction, and writing
// extended attributes belongs to "write" as well. The read and execute
// entries that folders usually pass down to other accounts hold neither.
func TestWriteAttributesCount(t *testing.T) {
	for _, c := range []struct {
		sddl  string
		write bool
	}{
		{"D:(A;;0x100;;;BU)", true},           // FILE_WRITE_ATTRIBUTES
		{"D:(A;;0x10;;;BU)", true},            // FILE_WRITE_EA
		{"D:(A;;FW;;;BU)", true},              // FILE_GENERIC_WRITE
		{"D:(A;OICIIO;0x100;;;AU)", true},     // only for what lies below
		{"D:(A;OICI;0x1200a9;;;BU)", false},   // read & execute, as C:\ gives Users
		{"D:(A;OICI;FR;;;AU)", false},         // FILE_GENERIC_READ
		{"D:(A;;FX;;;WD)", false},             // FILE_GENERIC_EXECUTE
		{"D:(A;OICIIO;GRGX;;;BU)", false},     // generic read & execute for what lies below
		{"D:(A;OICI;0x1200a9;;;IU)", false},   // INTERACTIVE
		{"D:(A;;0x120089;;;S-1-5-11)", false}, // Authenticated Users: read
	} {
		root := privateTree(t)
		dir := filepath.Join(root, "components", "whisper")
		setEntries(t, dir, c.sddl, false)
		e, err := Check(dir)
		if err != nil || e.Write != c.write {
			t.Errorf("%s: Check %+v %v (%s)", c.sddl, e, err, sddlOf(t, dir))
		}
		rep, err := CheckTree(root, TreeOptions{})
		if err != nil || rep.OK() == c.write || rep.Unconfirmed != "" {
			t.Errorf("%s: CheckTree %+v %v", c.sddl, rep, err)
		}
		if _, err := RestrictTree(root, TreeOptions{}); err != nil {
			t.Errorf("%s: RestrictTree: %v", c.sddl, err)
		}
		if rep, err := CheckTree(root, TreeOptions{}); err != nil || !rep.OK() {
			t.Errorf("%s: after RestrictTree: %+v %v", c.sddl, rep, err)
		}
	}
}

// Names that end in a dot or a space (SEC17, 低-1): the usual path rules
// would strip the ending and open another entry or none, so such entries
// used to be passed over. They are opened by their exact names: reported,
// and reset, like any other.
func TestTreeNamesEndingInDotOrSpace(t *testing.T) {
	root := privateTree(t)
	raw := func(p string) string { return `\\?\` + p }
	dir := filepath.Join(root, "components", "whisper")
	evil := dir + `\evil.`
	lib := dir + `\lib `
	inside := lib + `\x.py`
	// (removed by their exact names before the temporary directory goes)
	t.Cleanup(func() {
		os.Remove(raw(inside))
		os.Remove(raw(lib))
		os.Remove(raw(evil))
	})
	if err := os.WriteFile(raw(evil), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(raw(lib), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(raw(inside), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(evil); err == nil {
		t.Skip("this system opens names ending in a dot by their usual paths")
	}
	rep, err := CheckTree(root, TreeOptions{})
	if err != nil || !rep.OK() || rep.Entries != privateTreeEntries+3 {
		t.Fatalf("private entries with such names: %+v %v", rep, err)
	}
	setEntries(t, raw(evil), "D:(A;;FA;;;WD)", false)
	setEntries(t, raw(lib), "D:(A;OICI;FA;;;WD)", false)
	rep, err = CheckTree(root, TreeOptions{})
	if err != nil || rep.Unconfirmed != "" || rep.Entries != privateTreeEntries+3 {
		t.Fatalf("CheckTree: %+v %v", rep, err)
	}
	if got := findingPaths(rep); got != evil+"\n"+lib+"\n"+inside {
		t.Fatalf("findings:\n%s", got)
	}
	fixed, err := RestrictTree(root, TreeOptions{})
	if err != nil || len(fixed) != 2 || fixed[0] != evil || fixed[1] != lib {
		t.Fatalf("RestrictTree: %v %v", fixed, err)
	}
	if rep, err := CheckTree(root, TreeOptions{}); err != nil || !rep.OK() || rep.Entries != privateTreeEntries+3 {
		t.Fatalf("after RestrictTree: %+v %v", rep, err)
	}
	for _, p := range []string{evil, lib, inside} {
		if s := sddlOf(t, raw(p)); strings.Contains(s, ";WD)") {
			t.Errorf("%s after the reset: %s", p, s)
		}
	}
}
