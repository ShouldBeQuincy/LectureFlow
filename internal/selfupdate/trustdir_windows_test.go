// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A trust directory other accounts can change (Authenticated Users may
// modify it), or with an entry they can, is refused.
func TestCheckTrustDirLooseWindows(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := u.User.Sid.String()
	data := privateDataDir(t)
	loose := filepath.Join(privateDataDir(t), "selfupdate")
	mkdirSDDL(t, loose, "D:P(A;OICI;FA;;;"+me+")(A;OICI;FA;;;SY)(A;OICI;0x1301bf;;;AU)")
	if err := CheckTrustDir(loose, data); CodeOf(err) != CodeUntrustedDir {
		t.Fatalf("a loose trust directory: %v", err)
	}
	// private itself, an entry in it open to others
	trust := filepath.Join(privateDataDir(t), "selfupdate")
	if err := CheckTrustDir(trust, data); err != nil {
		t.Fatal(err)
	}
	mkdirSDDL(t, filepath.Join(trust, "x"), "D:P(A;OICI;FA;;;"+me+")(A;OICI;FA;;;WD)")
	if err := CheckTrustDir(trust, data); CodeOf(err) != CodeUntrustedDir {
		t.Fatalf("an entry others can change: %v", err)
	}
	os.Remove(filepath.Join(trust, "x"))
	if err := CheckTrustDir(trust, data); err != nil {
		t.Fatalf("without it: %v", err)
	}
}
