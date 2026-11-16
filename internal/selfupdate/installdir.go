// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lectureflow/internal/dirperm"
)

// The installation directory (security audit M-4). The Windows installer
// installs per user, without administrator rights, wherever the user
// chooses. Its default, %LOCALAPPDATA%\Programs\lectureflow, admits only
// the user (with SYSTEM and the administrators), and so does any folder
// the installer protects (desktop/packaging/installer.nsh gives every
// installation outside %LOCALAPPDATA% the private permissions of
// dirperm); but a folder made in D:\ by an older version, or one whose
// permissions were changed since, may let every signed-in account replace
// the programs there: the next start, and every update and rollback the
// app installs, would run theirs. The app cannot repair that without the
// user (the folder may need an administrator, and changing a program
// folder behind the user's back is worse than leaving it), so it says so
// on the diagnostics page (internal/api, the install_dir row) and installs
// no update in it: the user installs the new version by hand.
//
// The desktop shell names the installation directory (-install-dir for
// the service, -selfupdate-install-dir for the check before an update):
// only the Windows installation set up by the installer has one
// (desktop/src/main/lib/updatefiles.js installDirOf). Development runs,
// the portable build and macOS (no Mac to test on: a known limitation)
// name none, and nothing is checked for them.

// installTree bounds the walk of the installation directory: an
// installation holds about 150 entries (Electron's files, resources/ with
// the service program, the worker scripts and the tools); the bounds leave
// room for Python's bytecode caches and a few versions more. A variable
// for the tests.
var installTree = dirperm.TreeOptions{MaxEntries: 20000, MaxDepth: 32}

// installAboveTop is where CheckInstallDir stops looking at the folders
// above the installation ("" = the root of the drive; tests stop at a
// folder of their own).
var installAboveTop = ""

// CheckInstallDir reports selfupdate.untrusted_install_dir {path, who}
// when another account of this computer may change the programs in the
// installation directory dir:
//
//   - dir is not an absolute path, or is a link (a junction, a symbolic
//     link or another reparse point: where it leads is not checked);
//   - another account owns dir or may change it (dirperm.Check: a write
//     right, the right to change its permissions, no DACL at all);
//   - another account may remove or rename dir or a folder above it and
//     put its own folder there (dirperm.CheckAbove);
//   - another account owns or may change an entry below dir, or the walk
//     cannot vouch for every entry (dirperm.CheckTree with installTree: a
//     link, more entries or levels than the bounds, an entry that cannot
//     be examined; who then starts with dirperm.NotConfirmed).
//
// Owners and entries of SYSTEM, the administrators and TrustedInstaller
// count as the user's (a per-machine installation in Program Files passes),
// and so does reading: everyone may read and run a program folder.
// path is the entry found (dir, a folder above it or an entry below it).
// It only reads. Other errors (dir missing, its permissions unreadable):
// CodeIO; callers that must vouch for the installation treat any error as
// "not vouched for".
func CheckInstallDir(dir string) error {
	untrusted := func(p, who string) error {
		return cerr(CodeUntrustedInstallDir, map[string]any{"path": p, "who": who},
			fmt.Errorf("other accounts of this computer can change LectureFlow's installation folder %s (%s)", p, who))
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return untrusted(dir, dirperm.NotConfirmed+"not an absolute path")
	}
	dir = filepath.Clean(dir)
	fi, err := os.Lstat(dir)
	if err != nil {
		return ioErr(err)
	}
	if !isPlainDir(fi) {
		return untrusted(dir, dirperm.NotConfirmed+"it is a link (a junction, a symbolic link or another reparse point)")
	}
	e, err := dirperm.Check(dir)
	if err != nil {
		return ioErr(err)
	}
	if e.Write {
		return untrusted(dir, writers(e))
	}
	if err := checkAboveInstall(dir, installAboveTop, untrusted); err != nil {
		return err
	}
	// the folders the path really goes through, when a subst or mapped
	// drive letter (or a link the walk above did not meet) hides them
	real, err := dirperm.FinalPath(dir)
	if err != nil {
		return untrusted(dir, dirperm.NotConfirmed+fmt.Sprintf("where %s really lies cannot be read: %v", dir, err))
	}
	if real = filepath.Clean(real); !strings.EqualFold(real, dir) {
		top := installAboveTop // (tests) in the same form
		if top != "" {
			if t, err := dirperm.FinalPath(top); err == nil {
				top = filepath.Clean(t)
			}
		}
		if err := checkAboveInstall(real, top, untrusted); err != nil {
			return err
		}
	}
	rep, err := dirperm.CheckTree(dir, installTree)
	if err != nil {
		return ioErr(err)
	}
	if !rep.OK() {
		p, who := rep.Problem()
		if p == "" {
			p = dir
		}
		return untrusted(p, who)
	}
	return nil
}

// checkAboveInstall: dirperm.CheckAbove of dir as CheckInstallDir's
// verdict (nil: nobody else can move it away).
func checkAboveInstall(dir, top string, untrusted func(p, who string) error) error {
	p, e, err := dirperm.CheckAbove(dir, top)
	switch {
	case err != nil && errors.Is(err, dirperm.ErrLink):
		return untrusted(p, dirperm.NotConfirmed+p+" "+dirperm.ErrLink.Error())
	case err != nil && p == "":
		return ioErr(err)
	case err != nil:
		return untrusted(p, dirperm.NotConfirmed+fmt.Sprintf("the permissions of %s cannot be read: %v", p, err))
	case e.Write:
		return untrusted(p, writers(e))
	}
	return nil
}

// writers names the accounts that may change it, not those that may only
// read it (security review SEC20).
func writers(e dirperm.Exposure) string {
	if len(e.Writers) > 0 {
		return strings.Join(e.Writers, ", ")
	}
	return strings.Join(e.Who, ", ")
}
