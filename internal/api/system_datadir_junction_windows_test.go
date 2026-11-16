// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"lectureflow/internal/dirperm"
)

// A data directory that is a junction (security review SEC17, 低-2): the
// folder it leads to is what is checked and made private, when nobody else
// can have chosen that folder; otherwise the row cannot be confirmed and
// "收紧权限" changes nothing — not the link, not the folder.

// junctionLayout is a data directory top\links\data that is a junction to
// top\parent\real; every folder is private, and the folders above the link
// and above real are looked at up to top only (what lies above top on this
// computer is not the test's business).
type junctionLayout struct {
	top, links, link, parent, real string
}

func newJunctionLayout(t *testing.T, e *env) junctionLayout {
	t.Helper()
	top := filepath.Join(t.TempDir(), "top")
	if _, err := dirperm.MkdirPrivate(top); err != nil {
		t.Fatal(err)
	}
	// the long names of a temporary directory that has short ones (CI): the
	// form the final path of the folder has
	top, err := dirperm.FinalPath(top)
	if err != nil {
		t.Fatal(err)
	}
	l := junctionLayout{top: top, links: filepath.Join(top, "links"), parent: filepath.Join(top, "parent")}
	l.link, l.real = filepath.Join(l.links, "data"), filepath.Join(l.parent, "real")
	for _, d := range []string{l.links, l.parent, l.real} {
		if _, err := dirperm.MkdirPrivate(d); err != nil {
			t.Fatal(err)
		}
	}
	mklinkJ(t, l.link, l.real)
	orig := dataDirAboveTop
	dataDirAboveTop = top
	t.Cleanup(func() { dataDirAboveTop = orig })
	e.srv.dataDir = l.link
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	return l
}

// mklinkJ makes link a junction to target; the link itself (never what it
// leads to) goes again at the end.
func mklinkJ(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v %s", link, target, err, out)
	}
	t.Cleanup(func() { os.Remove(link) })
}

// ownSDDL is the owner and DACL of the entry at p itself (a link is not
// followed).
func ownSDDL(t *testing.T, p string) string {
	t.Helper()
	h := openOwn(t, p, windows.READ_CONTROL)
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// setOwnDACL gives the entry at p itself (a link is not followed) the DACL
// of sddl, protected, passing nothing on to what lies below or behind it.
func setOwnDACL(t *testing.T, p, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	h := openOwn(t, p, windows.READ_CONTROL|windows.WRITE_DAC)
	defer windows.CloseHandle(h)
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func openOwn(t *testing.T, p string, access uint32) windows.Handle {
	t.Helper()
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(u, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	return h
}

func meSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// loose: like a folder made under D:\ (Authenticated Users may modify it
// and what it holds).
func looseSDDL(t *testing.T) string {
	return "D:P(A;OICI;FA;;;" + meSID(t) + ")(A;OICI;0x1301bf;;;AU)"
}

// A junction to a private folder, the link and the folders above both
// private: the row is fine (it was "cannot be confirmed" before).
func TestDataDirJunctionPrivateWindows(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	l := newJunctionLayout(t, e)
	e.srv.checkDataDirEntries()
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("a private junction to a private folder: %+v %+v", c, c.DetailInfo)
	}
	// "收紧权限" has nothing to object to either, and changes nothing
	keep := ownSDDL(t, l.link)
	var out struct {
		Check SystemCheck `json:"check"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.Check.Status != statusOK {
		t.Fatalf("restrict: %+v", out.Check)
	}
	if got := ownSDDL(t, l.link); got != keep {
		t.Fatalf("the link was changed: %s → %s", keep, got)
	}
	if p := resolveDataDir(l.link); p.doubt != "" || !p.junction || p.root != l.real {
		t.Fatalf("resolveDataDir: %+v", p)
	}
}

// A private junction to a folder other accounts may change: the row says
// so, with the hint to restrict; "收紧权限" makes the folder and what it
// holds private — not the link, not the folders above either — and the
// row is fine afterwards.
func TestDataDirJunctionLooseFolderWindows(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	l := newJunctionLayout(t, e)
	inner := filepath.Join(l.real, "components", "whisper")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString(looseSDDL(t))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(l.real, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if s := ownSDDL(t, inner); !strings.Contains(s, ";AU)") {
		t.Fatalf("the folder's entries were not passed down: %s", s)
	}
	keep := map[string]string{}
	for _, p := range []string{l.link, l.links, l.parent, l.top} {
		keep[p] = ownSDDL(t, p)
	}
	e.srv.checkDataDirEntries()
	c := dataDirRow(t, e)
	abs, _ := filepath.Abs(l.link)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || c.HintInfo == nil || c.HintInfo.Code != codeHintDataDirRestrict ||
		c.DetailInfo.Params["path"] != abs ||
		!strings.Contains(c.DetailInfo.Params["who"].(string), "Authenticated Users") && !strings.Contains(c.DetailInfo.Params["who"].(string), "S-1-5-11") {
		t.Fatalf("a folder other accounts may change behind a junction: %+v %+v", c, c.DetailInfo)
	}
	var out struct {
		Check SystemCheck `json:"check"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.Check.Status != statusOK {
		t.Fatalf("restrict: %+v", out.Check)
	}
	for _, p := range []string{l.real, inner} {
		if s := ownSDDL(t, p); strings.Contains(s, ";AU)") {
			t.Fatalf("%s still lets others in: %s", p, s)
		}
	}
	for p, s := range keep {
		if got := ownSDDL(t, p); got != s {
			t.Errorf("%s was changed: %s → %s", p, s, got)
		}
	}
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("after restricting: %+v %+v", c, c.DetailInfo)
	}
}

// Junctions nobody can vouch for: the row cannot be confirmed (the reason
// names the junction, the hint says what to do), and "收紧权限" is a 409
// that changes nothing — neither the folder (here one other accounts may
// change, so that a change would show) nor the link.
func TestDataDirJunctionUnvouchedWindows(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, l junctionLayout)
		why   string // in the reason
	}{
		{"link others may change", func(t *testing.T, l junctionLayout) {
			setOwnDACL(t, l.link, "D:P(A;;FA;;;"+meSID(t)+")(A;;0x1301bf;;;BU)")
		}, "change the link"},
		{"link only others' write attributes", func(t *testing.T, l junctionLayout) {
			setOwnDACL(t, l.link, "D:P(A;;FA;;;"+meSID(t)+")(A;;0x100;;;BU)")
		}, "change the link"},
		{"folder above the link others may change", func(t *testing.T, l junctionLayout) {
			setOwnDACL(t, l.links, "D:P(A;OICI;FA;;;"+meSID(t)+")(A;;0x1301bf;;;AU)")
		}, filepath.Join("top", "links") + " ("},
		{"folder above the target others may change", func(t *testing.T, l junctionLayout) {
			setOwnDACL(t, l.parent, "D:P(A;OICI;FA;;;"+meSID(t)+")(A;;0x1301bf;;;AU)")
		}, filepath.Join("top", "parent") + " ("},
		{"target missing", func(t *testing.T, l junctionLayout) {
			if err := os.Remove(l.real); err != nil {
				t.Fatal(err)
			}
		}, "cannot be opened"},
		{"loop", func(t *testing.T, l junctionLayout) {
			if err := os.Remove(l.link); err != nil {
				t.Fatal(err)
			}
			mklinkJ(t, l.link, l.link)
		}, "cannot be opened"},
		{"two junctions", func(t *testing.T, l junctionLayout) {
			if err := os.Remove(l.link); err != nil {
				t.Fatal(err)
			}
			j2 := filepath.Join(l.top, "second")
			mklinkJ(t, j2, l.real)
			mklinkJ(t, l.link, j2)
		}, "another link"},
		// security review SEC30, 低-1: through a link whose name differs
		// from its folder's only by KELVIN SIGN for k, others may rewrite it
		{"case-variant link on the way", func(t *testing.T, l junctionLayout) {
			if err := os.Remove(l.link); err != nil {
				t.Fatal(err)
			}
			kreal := filepath.Join(l.top, "kreal")
			if _, err := dirperm.MkdirPrivate(kreal); err != nil {
				t.Fatal(err)
			}
			middle := filepath.Join(l.top, "Kreal")
			mklinkJ(t, middle, kreal)
			setOwnDACL(t, middle, "D:P(A;;FA;;;"+meSID(t)+")(A;;0x1301bf;;;BU)")
			mklinkJ(t, l.link, middle)
		}, "another link"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.srv.diag.waitStartup()
			l := newJunctionLayout(t, e)
			setOwnDACL(t, l.real, looseSDDL(t))
			c.setup(t, l)
			keep := map[string]string{}
			for _, p := range []string{l.link, l.links, l.parent, l.real, l.top} {
				if _, err := os.Lstat(p); err == nil {
					keep[p] = ownSDDL(t, p)
				}
			}
			e.srv.checkDataDirEntries()
			row := dataDirRow(t, e)
			abs, _ := filepath.Abs(l.link)
			reason, _ := row.DetailInfo.Params["reason"].(string)
			if row.Status != statusWarn || row.DetailInfo.Code != codeDataDirUnconfirmed || row.DetailInfo.Params["path"] != abs ||
				!strings.Contains(reason, "junction") || !strings.Contains(reason, c.why) ||
				row.HintInfo == nil || row.HintInfo.Code != codeHintDataDirUnconfirm {
				t.Fatalf("row: %+v %+v", row, row.DetailInfo)
			}
			rep, ok := e.srv.dataDirTreeReport()
			if !ok || rep.OK() || !strings.Contains(rep.Unconfirmed, "junction") {
				t.Fatalf("walk: %+v %v", rep, ok)
			}
			info := e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusConflict)
			if info.Code != codeDataDirRestrictFailed || !strings.Contains(info.Params["error"].(string), dirperm.NotConfirmed) {
				t.Fatalf("restrict: %+v", info)
			}
			for p, s := range keep {
				if got := ownSDDL(t, p); got != s {
					t.Errorf("%s was changed: %s → %s", p, s, got)
				}
			}
		})
	}
}

// Without a junction nothing changes: a data directory whose folder above
// other accounts may change is judged by its own permissions, as before
// (the folders above a data directory are only looked at for a junction).
func TestDataDirNotJunctionUnchangedWindows(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	if p := resolveDataDir(e.cfg.DataDir); p.junction || p.doubt != "" || p.root != e.cfg.DataDir {
		t.Fatalf("resolveDataDir of a folder: %+v", p)
	}
	setOwnDACL(t, e.cfg.DataDir, looseSDDL(t))
	e.srv.checkDataDirEntries()
	if c := dataDirRow(t, e); c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared {
		t.Fatalf("a loose folder: %+v %+v", c, c.DetailInfo)
	}
}
