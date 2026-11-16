// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Voiding the rollback state (security review SEC17, 中-3).
//
// What a failed update returns to is decided by files the desktop shell
// keeps: applying.json (an installation is on probation), its record of
// that installation and of the backups' hashes (TrustRecordFile in its
// profile), the backups themselves and previous.json's "backup". The shell
// believes them because its profile is the user's own. When the profile
// lies in the data directory and other accounts could write there, all of
// them can be prepared together: an "installation on probation" of the
// running version, a "backup" of an older one with a matching hash. They
// agree with each other, the checks of a rollback pass, and after two
// failed starts the prepared program is put into the installation's place.
//
// Making the data directory private stops further changes; it does not
// tell what was changed before (the entries are the user's now, whoever
// wrote them). So the moment the permissions are tightened is the moment
// this state stops counting: VoidRollback removes it. Nothing here is
// needed to run LectureFlow or to install a verified update — packages and
// pending.json are checked against the signature when they are used, and
// the next update makes its backup from the installation again. What is
// lost is the way back for an update that is on probation right now: it
// keeps running; should it fail to start later, the shell finds no record
// of it, rolls nothing back and shows the start's error (the previous
// version can be installed by hand).
//
// Since the record moved out of the data directory on Windows (SEC17 中-B,
// trustdir.go) this is defence in depth there, kept on purpose and
// unchanged in when it acts:
//   - the record the Windows shell believes now lies in
//     %LOCALAPPDATA%\LectureFlow\selfupdate and is never touched here
//     (the service does not even know that folder); what VoidRollback
//     removes as "the record" is the profile's old file, which the shell
//     reads once when it takes a record over from an older version, and
//     which an older version reads after a rollback to it;
//   - applying.json, the backups and previous.json's "backup" still lie in
//     the data directory and are still removed when the profile lies in it
//     (a record taken over from that profile carried no backup, and the
//     shell drops a record of an installation without applying.json);
//   - on macOS (no Mac to test a move on) the record stays in the profile:
//     there VoidRollback is what it always was.
// The condition stays "the profile lies in the data directory": a data
// directory whose profile lies elsewhere never held a record another
// account could prepare, then or now.

const (
	applyingFile = "applying.json" // the shell's (README.md "The updates directory"); never read here
	backupsDir   = "backup"        // the shell's copies of replaced installations
)

// Voided says what VoidRollback removed.
type Voided struct {
	Record   bool // the shell's record (installation, backups' hashes)
	Applying bool // applying.json
	Backups  bool // updates/backup
	Previous bool // previous.json's "backup" (or a previous.json that was a link)
	// Links: updates/ or electron/ was a link (a junction, a symbolic link,
	// another reparse point) and was removed; what it led to is untouched.
	Links bool
}

// Any reports whether anything was removed.
func (v Voided) Any() bool { return v.Record || v.Applying || v.Backups || v.Previous || v.Links }

// VoidRollback removes the rollback state kept in the data directory
// dataDir, when the desktop shell's profile lies in it (dataDir/electron
// exists; otherwise the shell's record is under the user's own profile
// folder, was never writable for others and vouches for itself, and
// nothing is touched):
//
//   - electron/selfupdate-trust.json, the profile's record of the
//     installation the shell started and of its backups' hashes (on
//     Windows the record the shell believes lies outside, see above):
//     without it the shell ignores applying.json and uses no backup;
//   - updates/applying.json;
//   - updates/backup/ (copies of replaced installations, database
//     snapshots of an update on probation);
//   - "backup" in updates/previous.json (its other fields are kept).
//
// Links (security review SEC17, 高-A). Nothing is followed out of the data
// directory: dataDir itself must be a plain directory (a link there is an
// error, not followed); updates/ and electron/ are entered only when they
// are plain directories, through handles (plaindir.go). One that is a junction, a
// symbolic link or another reparse point was put there by whoever could
// write to the data directory: the link itself is removed (Links), never
// what it leads to — with it the shell finds no record, applying.json or
// backup by that path any more. The record, applying.json and backup/ are
// removed as entries (a link among them, or below backup/, is removed, not
// entered); a previous.json that is a link is removed, not read or
// written through.
//
// When this runs (POST /api/system/data-dir/restrict) the data directory
// already has the private permissions: restrictDataDir set its protected
// DACL first, and restrictDataDirTree then reset the entries below it, so
// other accounts can no longer create or rename entries in it. Two cases
// remain where someone else might still change things meanwhile: a drive
// without permissions (FAT, exFAT; it keeps no links either, and the
// request ends in 409) and handles opened before the permissions changed
// (they keep the rights they were opened with). Neither can redirect what
// is removed here: the look (Lstat) and the use go through the same
// handle, and a directory replaced between the two is refused (an error).
// Within a handle, os.Root's RemoveAll opens each folder it enters without
// following a link, relative to the folder above it.
//
// Every step is tried; an error names what could not be removed (the
// caller must not report the data directory as taken care of then): the
// rollback state is either gone or the call fails.
func VoidRollback(dataDir string) (Voided, error) {
	var v Voided
	// The data directory itself is not followed either (security review
	// SEC30, 低-2): replaced by a link while "收紧权限" ran, it leads to
	// another folder, and nothing there is removed (an error).
	root, err := openPlainDir(dataDir)
	if err != nil {
		if isNotExist(err) {
			return v, nil
		}
		return v, ioErr(err)
	}
	defer root.Close()
	var errs []error
	// unlink removes the entry name of r itself (it is not a plain
	// directory: nothing below it is looked at).
	unlink := func(r *os.Root, name string, done *bool) {
		if err := r.Remove(name); err != nil && !isNotExist(err) {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return
		}
		*done = true
	}
	// remove removes the entry name of r, whatever it is (a link: the
	// link; a folder: with what it holds, links in it removed, not
	// entered).
	remove := func(r *os.Root, name string, done *bool) {
		if _, err := r.Lstat(name); err != nil {
			if !isNotExist(err) {
				errs = append(errs, err)
			}
			return
		}
		if err := r.RemoveAll(name); err != nil {
			errs = append(errs, err)
			return
		}
		*done = true
	}
	profile, fi, err := openPlainDirIn(root, ShellProfileDir)
	switch {
	case fi == nil && err == nil:
		return v, nil // no shell profile here
	case errors.Is(err, errNotPlainDir) && isLinkLike(fi):
		unlink(root, ShellProfileDir, &v.Links)
	case errors.Is(err, errNotPlainDir):
		// a file: no record can lie below it
	case err != nil:
		errs = append(errs, err)
	default:
		remove(profile, TrustRecordFile, &v.Record)
		profile.Close()
	}
	updates, fi, err := openPlainDirIn(root, "updates")
	switch {
	case fi == nil && err == nil:
	case errors.Is(err, errNotPlainDir) && isLinkLike(fi):
		unlink(root, "updates", &v.Links)
	case errors.Is(err, errNotPlainDir):
	case err != nil:
		errs = append(errs, err)
	default:
		defer updates.Close()
		remove(updates, applyingFile, &v.Applying)
		remove(updates, backupsDir, &v.Backups)
		if err := voidPrevious(updates, &v); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return v, ioErr(fmt.Errorf("the update's rollback state could not be removed: %w", errors.Join(errs...)))
	}
	return v, nil
}

// voidPrevious drops "backup" from previous.json in the updates directory
// r (its other fields are kept). One that is a link is removed instead;
// one that cannot be read as JSON, or names no backup, is left as it is.
func voidPrevious(r *os.Root, v *Voided) error {
	fi, err := r.Lstat(previousFile)
	switch {
	case isNotExist(err):
		return nil
	case err != nil:
		return err
	case isLinkLike(fi):
		if err := r.Remove(previousFile); err != nil && !isNotExist(err) {
			return fmt.Errorf("%s: %w", previousFile, err)
		}
		v.Previous = true
		return nil
	case !fi.Mode().IsRegular():
		return nil // a folder: nothing the shell can read
	}
	b, err := readFileIn(r, previousFile)
	if err != nil {
		return nil // as readJSON's callers: unreadable is no previous version
	}
	var prev Previous
	if json.Unmarshal(b, &prev) != nil || prev.Backup == "" {
		return nil
	}
	prev.Backup = ""
	out, err := json.MarshalIndent(prev, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomicIn(r, previousFile, append(out, '\n')); err != nil {
		return fmt.Errorf("%s: %w", previousFile, err)
	}
	v.Previous = true
	return nil
}
