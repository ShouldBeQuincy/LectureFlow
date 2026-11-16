// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"lectureflow/internal/dirperm"
)

// A data directory that is a junction (security review SEC30, 低-3): the
// in-app update looks at the folder it leads to, as the diagnostics do, not
// at the link's own permissions; a junction nobody can vouch for is
// refused.

// junctionData makes top\links\data, a junction to top\parent\real (all
// private), and stops CheckTrust's look above them at top.
func junctionData(t *testing.T) (link, real string) {
	t.Helper()
	top, err := dirperm.FinalPath(privateDataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	links, parent := filepath.Join(top, "links"), filepath.Join(top, "parent")
	real = filepath.Join(parent, "real")
	for _, d := range []string{links, parent, real} {
		if _, err := dirperm.MkdirPrivate(d); err != nil {
			t.Fatal(err)
		}
	}
	link = filepath.Join(links, "data")
	dirLink(t, link, real)
	orig := junctionAboveTop
	junctionAboveTop = top
	t.Cleanup(func() { junctionAboveTop = orig })
	return link, real
}

// setOwnDACLWindows gives the entry at p itself (a link: not followed) the
// protected DACL of sddl, passing nothing on.
func setOwnDACLWindows(t *testing.T, p, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	u, _ := windows.UTF16PtrFromString(p)
	h, err := windows.CreateFile(u, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func looseFor(t *testing.T) string {
	return "D:P(A;OICI;FA;;;" + mySID(t) + ")(A;OICI;0x1301bf;;;AU)"
}

// Everything private: the update goes on, with or without updates/.
func TestCheckTrustJunctionPrivateWindows(t *testing.T) {
	link, real := junctionData(t)
	if err := CheckTrust(filepath.Join(link, "updates")); err != nil {
		t.Fatalf("no updates/: %v", err)
	}
	if _, err := dirperm.MkdirPrivate(filepath.Join(real, "updates")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "updates", "previous.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrust(filepath.Join(link, "updates")); err != nil {
		t.Fatalf("a private updates/: %v", err)
	}
}

// A private junction to a folder other accounts may change: refused, the
// folder named, whether updates/ is missing or private (both passed the
// look at the link before).
func TestCheckTrustJunctionLooseFolderWindows(t *testing.T) {
	link, real := junctionData(t)
	setOwnDACLWindows(t, real, looseFor(t))
	check := func(what string) {
		t.Helper()
		err := CheckTrust(filepath.Join(link, "updates"))
		if err == nil {
			t.Fatalf("%s: the update would go on", what)
		}
		info := InfoOf(err)
		who, _ := info.Params["who"].(string)
		if CodeOf(err) != CodeUntrustedDir || info.Params["path"] != real ||
			!strings.Contains(who, "Authenticated Users") && !strings.Contains(who, "S-1-5-11") ||
			!strings.Contains(err.Error(), "other accounts of this computer can change "+real) {
			t.Fatalf("%s: %v %+v", what, err, info.Params)
		}
	}
	check("no updates/")
	if _, err := dirperm.MkdirPrivate(filepath.Join(real, "updates")); err != nil {
		t.Fatal(err)
	}
	check("a private updates/")
}

// A junction nobody can vouch for (here: other accounts may rewrite the
// link): refused as "cannot be confirmed", whatever the folder is like.
func TestCheckTrustJunctionUnvouchedWindows(t *testing.T) {
	link, _ := junctionData(t)
	setOwnDACLWindows(t, link, "D:P(A;;FA;;;"+mySID(t)+")(A;;0x1301bf;;;BU)")
	err := CheckTrust(filepath.Join(link, "updates"))
	if err == nil {
		t.Fatal("the update would go on")
	}
	who, _ := InfoOf(err).Params["who"].(string)
	if CodeOf(err) != CodeUntrustedDir || InfoOf(err).Params["path"] != link ||
		!strings.HasPrefix(who, dirperm.NotConfirmed) || !strings.Contains(who, "junction") ||
		!strings.Contains(err.Error(), "cannot be confirmed to be this account's alone") {
		t.Fatalf("%v %+v", err, InfoOf(err).Params)
	}
}

// Not a junction: as before — a private data directory passes, one others
// may change is named.
func TestCheckTrustNotJunctionWindows(t *testing.T) {
	data := privateDataDir(t)
	if err := CheckTrust(filepath.Join(data, "updates")); err != nil {
		t.Fatalf("private: %v", err)
	}
	setOwnDACLWindows(t, data, looseFor(t))
	err := CheckTrust(filepath.Join(data, "updates"))
	if CodeOf(err) != CodeUntrustedDir || InfoOf(err).Params["path"] != data {
		t.Fatalf("loose: %v %+v", err, InfoOf(err).Params)
	}
}
