// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The installation folder's row of the diagnostics (security audit M-4;
// the real row is internal/api/system_installdir.go, whose codes the mock
// uses). The real server shows it only in the Windows desktop app set up
// by the installer; the mock only in these scenarios:
//
//	installdir_ok          %LOCALAPPDATA%\Programs\lectureflow: only the
//	                       user can change it (check.install_dir_private)
//	installdir_shared      D:\LectureFlow, other accounts can change it
//	                       (check.install_dir_shared + check.hint_install_dir;
//	                       no button)
//	installdir_unconfirmed it cannot be confirmed (a link inside it:
//	                       check.install_dir_unconfirmed +
//	                       check.hint_install_dir_unconfirmed)
//	installdir_above       D:\Apps\lectureflow, D:\Apps can be renamed by
//	                       others (check.hint_install_dir_above)
//	installdir_root        D:\lectureflow, others can replace entries in
//	                       D:\ (check.hint_install_dir_root)
//	installdir_profile     the default place, but a group was added to it
//	                       (check.hint_install_dir_profile)
//
// The row follows the data folder's rows (data_dir, credentials,
// data_dir_location), like the real server's.

import (
	"net/http"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// mockDefaultInstall is where the installer proposes to install (the hints'
// default).
const mockDefaultInstall = `C:\Users\alice\AppData\Local\Programs\lectureflow`

type mockInstallDir struct {
	status string
	code   string
	params map[string]any
	dir    string
	hint   string // "" = check.hint_install_dir {dir, default}; else that code {path(, default)}
}

var installDirScenarios = map[string]mockInstallDir{
	"installdir_ok": {status: "ok", code: "check.install_dir_private", dir: `C:\Users\alice\AppData\Local\Programs\lectureflow`,
		params: map[string]any{"path": `C:\Users\alice\AppData\Local\Programs\lectureflow`}},
	"installdir_shared": {status: "warn", code: "check.install_dir_shared", dir: `D:\LectureFlow`,
		params: map[string]any{"path": `D:\LectureFlow`, "who": `NT AUTHORITY\Authenticated Users`}},
	"installdir_unconfirmed": {status: "warn", code: "check.install_dir_unconfirmed", dir: `D:\Apps\LectureFlow`, hint: "check.hint_install_dir_unconfirmed",
		params: map[string]any{"path": `D:\Apps\LectureFlow`, "reason": `D:\Apps\LectureFlow\resources\worker is a link (a reparse point): where it leads is not checked`}},
	"installdir_above": {status: "warn", code: "check.install_dir_shared", dir: `D:\Apps\lectureflow`, hint: "check.hint_install_dir_above",
		params: map[string]any{"path": `D:\Apps`, "who": `NT AUTHORITY\Authenticated Users`}},
	"installdir_root": {status: "warn", code: "check.install_dir_shared", dir: `D:\lectureflow`, hint: "check.hint_install_dir_root",
		params: map[string]any{"path": `D:\`, "who": `NT AUTHORITY\Authenticated Users`}},
	"installdir_profile": {status: "warn", code: "check.install_dir_shared", dir: `C:\Users\alice\AppData\Local\Programs\lectureflow`, hint: "check.hint_install_dir_profile",
		params: map[string]any{"path": `C:\Users\alice\AppData\Local\Programs\lectureflow`, "who": `EXAMPLE\Helpdesk`}},
}

// installDirSystem adds the scenario's install_dir row after the data
// folder's rows. Called with s.mu held.
func (m *mock) installDirSystem(r *http.Request, sys *api.System) {
	d, ok := installDirScenarios[scenario(r)]
	if !ok {
		return
	}
	c := codedCheck("install_dir", d.status, d.code, d.params)
	switch {
	case d.status == "ok":
	case d.hint != "":
		ps := map[string]any{"path": d.params["path"]}
		if d.hint != "check.hint_install_dir_profile" {
			ps["default"] = mockDefaultInstall
		}
		c.HintInfo = &model.ErrorInfo{Code: d.hint, Params: ps}
	default:
		c.HintInfo = &model.ErrorInfo{Code: "check.hint_install_dir", Params: map[string]any{"dir": d.dir, "default": mockDefaultInstall}}
	}
	at := len(sys.Checks)
	for i, x := range sys.Checks {
		if x.Name == "data_dir" || x.Name == "credentials" || x.Name == "data_dir_location" {
			at = i + 1
		}
	}
	rows := make([]api.SystemCheck, 0, len(sys.Checks)+1)
	rows = append(rows, sys.Checks[:at]...)
	rows = append(rows, c)
	sys.Checks = append(rows, sys.Checks[at:]...)
}
