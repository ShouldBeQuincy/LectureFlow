// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"lectureflow/internal/selfupdate"
)

// -selfupdate-verify DIR (SPEC-ext 应用内更新, hardening): the desktop shell
// runs it right before it installs an update. It checks the pending update
// of the updates directory DIR again with the public key built into this
// program (selfupdate.VerifyPending: the signature of update.json, the
// package's size and SHA-256, pending.json's claims, a newer version than
// this one), prints one line on stdout and exits:
//
//	0  ok version=X.Y.Z kind=K file=<path relative to DIR> sha256=<hex>[ copy=<absolute path>]
//	1  invalid <code>: <reason>
//	2  not_configured: this build has no update public key
//	3  untrusted_dir: <reason>   another account of this computer can
//	   change the data directory (or DIR): nothing in it is installed or
//	   rolled back (sec-update M2)
//
// -selfupdate-previous checks the package of previous.json instead (the
// version a rollback returns to; selfupdate.VerifyPrevious): older than
// this version. -selfupdate-copy PARENT copies the package, while it is
// hashed, into a new private directory in PARENT and names the copy on the
// ok line; the shell runs that copy (M3). Nothing else is written, no log
// file. The data directory's permissions are checked first, with
// everything below DIR (selfupdate.CheckTrust).
//
// -selfupdate-private (with -selfupdate-copy PARENT; security audit M-3)
// verifies no package: after the same check of the permissions it makes an
// empty directory in PARENT that only this account can use and prints
//
//	0  ok private=<absolute path>
//	1  invalid <code>: <reason>   the directory could not be made
//	3  untrusted_dir: <reason>
//
// The shell copies a backup of the installation into it and compares the
// copy's hash with the one it recorded before a rollback runs or restores
// anything from it: nothing can change the backup between the check and
// its use. No public key is involved.
//
// -selfupdate-trust-dir T (any mode; security review SEC17 中-B): the
// directory the shell keeps its record of the installation and of the
// backups' hashes in (Windows: %LOCALAPPDATA%\LectureFlow\selfupdate,
// outside the data directory). After the data directory it is checked too
// (selfupdate.CheckTrustDir: made privately when missing; refused when it
// lies in the data directory or the data directory in it, is a link, or
// another account can change it): exit 3 then, like an untrusted data
// directory. -selfupdate-trust-check verifies no package: after these
// checks it prints
//
//	0  ok trusted
//	3  untrusted_dir: <reason>
//
// (the shell asks it before it takes over a record kept by an older
// version in its profile).
//
// -selfupdate-install-dir I (any mode; security audit M-4): the
// installation directory of the Windows installation (the shell names it
// only for an installation set up by the installer). Checked last
// (selfupdate.CheckInstallDir: another account owns it, may change it or
// an entry in it, or may remove or rename it or a folder above it; a link;
// an installation the walk cannot vouch for): then nothing is installed or
// rolled back in the app either, whatever the mode, and it prints
//
//	3  untrusted_install_dir: <reason>
//
// (the same exit code as an untrusted data directory; the line says which:
// the user installs the new version by hand instead of tightening the data
// directory). Any error of the check counts as untrusted.

// Exit codes of -selfupdate-verify besides exitOK.
const (
	exitVerifyInvalid       = 1
	exitVerifyNotConfigured = 2
	exitVerifyUntrusted     = 3
)

// selfupdateVerifyKey is the public key -selfupdate-verify checks with
// (tests replace it).
var selfupdateVerifyKey = selfupdate.BuiltinPublicKey

// checkInstallDir is selfupdate.CheckInstallDir (tests replace it for a
// private installation: what lies above a temporary folder is this
// computer's, not the test's).
var checkInstallDir = selfupdate.CheckInstallDir

// selfUpdateVerifyArgs are the flags of -selfupdate-verify.
type selfUpdateVerifyArgs struct {
	Dir      string // the updates directory
	Current  string // this program's version
	Previous bool   // previous.json instead of pending.json
	CopyDir  string // where the private copy goes ("" = none)
	Private  bool   // make an empty private directory in CopyDir instead of verifying a package
	// TrustDir: where the shell keeps its record (checked too; "" = in its
	// profile, which CheckTrust looks at when it lies in the data directory)
	TrustDir   string
	TrustCheck bool // verify no package: the checks of the permissions only
	// InstallDir: the installation directory (checked too; "" = none)
	InstallDir string
}

// runSelfUpdateVerify checks a's package.
func runSelfUpdateVerify(a selfUpdateVerifyArgs, stdout io.Writer) int {
	if err := selfupdate.CheckTrust(a.Dir); err != nil {
		fmt.Fprintf(stdout, "untrusted_dir: %s\n", oneLine(err.Error()))
		return exitVerifyUntrusted
	}
	if a.TrustDir != "" {
		if err := selfupdate.CheckTrustDir(a.TrustDir, filepath.Dir(filepath.Clean(a.Dir))); err != nil {
			fmt.Fprintf(stdout, "untrusted_dir: %s\n", oneLine(err.Error()))
			return exitVerifyUntrusted
		}
	}
	if a.InstallDir != "" {
		if err := checkInstallDir(a.InstallDir); err != nil {
			fmt.Fprintf(stdout, "untrusted_install_dir: %s\n", oneLine(err.Error()))
			return exitVerifyUntrusted
		}
	}
	if a.TrustCheck {
		fmt.Fprintln(stdout, "ok trusted")
		return exitOK
	}
	if a.Private {
		dir, err := selfupdate.PrivateDir(a.CopyDir)
		if err != nil {
			fmt.Fprintf(stdout, "invalid %s: %s\n", selfupdate.CodeOf(err), oneLine(err.Error()))
			return exitVerifyInvalid
		}
		fmt.Fprintf(stdout, "ok private=%s\n", dir)
		return exitOK
	}
	o := selfupdate.VerifyOptions{PublicKey: selfupdateVerifyKey(), Current: a.Current, CopyDir: a.CopyDir}
	verify := selfupdate.VerifyPending
	if a.Previous {
		verify = selfupdate.VerifyPrevious
	}
	v, err := verify(a.Dir, o)
	switch {
	case errors.Is(err, selfupdate.ErrNotConfigured):
		fmt.Fprintln(stdout, "not_configured: this build has no update public key")
		return exitVerifyNotConfigured
	case err != nil:
		fmt.Fprintf(stdout, "invalid %s: %s\n", selfupdate.CodeOf(err), oneLine(err.Error()))
		return exitVerifyInvalid
	}
	line := fmt.Sprintf("ok version=%s kind=%s file=%s sha256=%s", v.Version, v.Kind, v.File, v.SHA256)
	if v.Copy != "" {
		line += " copy=" + v.Copy
	}
	fmt.Fprintln(stdout, line)
	return exitOK
}
