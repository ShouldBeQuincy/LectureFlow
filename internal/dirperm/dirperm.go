// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package dirperm keeps directories private to the user who runs
// LectureFlow (security review sec-update, M2): the data directory holds the
// database with the providers' API keys, the Python environments the
// service runs and the in-app update's files, so other accounts of the
// computer must neither read nor change it.
//
// A private directory has, on Windows, a protected DACL (nothing inherited
// from the parent) that allows full control to the current user, SYSTEM and
// the Administrators group, inherited by everything below it:
//
//	D:P(A;OICI;FA;;;<user SID>)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)
//
// The same accounts as a user profile (%APPDATA%) by default: SYSTEM and
// the administrators can take any file anyway, while system services that
// open files as SYSTEM without backup semantics (some backup programs) keep
// working. The named pipe of the browser extension admits the user alone
// (internal/nmhost): no service has to open it. On macOS and Linux a
// private directory has mode 0700.
//
// Check tells whether other accounts can change (or read) a directory; it
// never changes anything. Restrict makes an existing directory private (the
// directory itself; what lies below inherits it). Nothing here changes a
// directory the user already had unless they ask for it: the data directory
// is made private when LectureFlow creates it or moves the data into it.
//
// What already lies below a directory keeps its own owner and its own
// entries: CheckTree looks at every entry of a tree, RestrictTree resets
// the ones it would report (tree.go).
package dirperm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Exposure is what other accounts may do with a directory (Check).
type Exposure struct {
	// Write: another account can create, change or delete entries (or
	// change the permissions: a foreign owner, WRITE_DAC).
	Write bool
	// Read: another account can list the directory or read its files.
	Read bool
	// Who names those accounts (Windows: DOMAIN\name or a SID; elsewhere
	// "group <name>", "others", "owner <uid>", or the ancestor that lets
	// them rename the directory). At most a few names.
	Who []string
	// Writers are the names of Who that may change the directory (the
	// reason for Write), without those that may only read it.
	Writers []string
}

// Exposed reports whether another account can read or change the directory.
func (e Exposure) Exposed() bool { return e.Write || e.Read }

// maxWho bounds Exposure.Who and Exposure.Writers.
const maxWho = 4

func addName(list []string, who string) []string {
	for _, w := range list {
		if w == who {
			return list
		}
	}
	if len(list) < maxWho {
		list = append(list, who)
	}
	return list
}

func (e *Exposure) add(who string) { e.Who = addName(e.Who, who) }

// addWriter names an account that may change the directory.
func (e *Exposure) addWriter(who string) {
	e.Who = addName(e.Who, who)
	e.Writers = addName(e.Writers, who)
}

// MkdirPrivate creates dir (and its missing parents, as os.MkdirAll does)
// and makes dir itself private from the start. It reports whether dir was
// created; an existing directory is left exactly as it is.
func MkdirPrivate(dir string) (bool, error) {
	if dir == "" {
		return false, errors.New("dirperm: empty path")
	}
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return false, &os.PathError{Op: "mkdir", Path: dir, Err: errors.New("not a directory")}
		}
		return false, nil
	}
	parent := filepath.Dir(filepath.Clean(dir))
	if parent != dir && parent != "." && !isRoot(parent) {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return false, err
		}
	}
	if err := mkdirPrivate(dir); err != nil {
		if errors.Is(err, os.ErrExist) {
			if fi, serr := os.Stat(dir); serr == nil && fi.IsDir() {
				return false, nil // created at the same moment by someone else: not ours
			}
		}
		return false, err
	}
	return true, nil
}

// MkdirTempPrivate creates a new private directory in parent (os.MkdirTemp's
// pattern rules) and returns its path.
func MkdirTempPrivate(parent, pattern string) (string, error) {
	if parent == "" {
		parent = os.TempDir()
	}
	return mkdirTempPrivate(parent, pattern)
}

// Restrict makes the existing directory dir private. Only dir's own
// permissions are set; what lies below inherits them (Windows propagates
// the inheritable entries; on Unix the mode of dir alone keeps others out).
// The owner is not changed.
func Restrict(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return &os.PathError{Op: "restrict", Path: dir, Err: errors.New("not a directory")}
	}
	return restrict(dir)
}

// Check reports what other accounts can do with the directory dir. It
// only reads.
func Check(dir string) (Exposure, error) {
	if _, err := os.Stat(dir); err != nil {
		return Exposure{}, err
	}
	return check(dir)
}

// CheckAbove reports whether another account can take dir out of its
// place: remove or rename a folder above it, or remove or rename dir in its
// folder, and put a folder of its own there (security audit M-4: an
// installation in D:\Apps\LectureFlow is only as safe as D:\Apps). path is
// the first folder, walking up from dir's own, whose owner or permissions
// allow it ("" when none does); e names the accounts. Only the folders
// below top are looked at ("" = every folder up to the root of the drive or
// share; tests stop at a folder of their own). It only reads; a folder whose
// permissions cannot be read is an error naming it.
//
// Windows: a folder above dir counts when another account owns it, or an
// entry that applies to the folder itself (not one that is only passed
// down) gives another account DELETE, FILE_DELETE_CHILD, WRITE_DAC,
// WRITE_OWNER or GENERIC_ALL; the root of the drive only for what lets it
// remove its entries (FILE_DELETE_CHILD and the rights to give itself
// that), since nobody renames a drive's root. A folder without any DACL
// (FAT, exFAT) lets everyone. Deny entries are not counted (the safe side).
// A folder on the way that is a link (a junction, a symbolic link or
// another reparse point: what the system reads is the link's own entry,
// not the folders it leads into) is an error wrapping ErrLink, with path
// naming it: not vouched for (security review SEC20). Callers that also
// want the folders the path really goes through (a subst or mapped drive
// letter hides them) check FinalPath(dir) as well.
// macOS, Linux: Check already looks at every folder above dir (links
// resolved); CheckAbove reports nothing there.
func CheckAbove(dir, top string) (path string, e Exposure, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", Exposure{}, err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", Exposure{}, err
	}
	if top != "" {
		if top, err = filepath.Abs(top); err != nil {
			return "", Exposure{}, err
		}
	}
	return checkAbove(filepath.Clean(abs), top)
}

// ErrLink: a folder CheckAbove looked at is a link.
var ErrLink = errors.New("it is a link (a junction, a symbolic link or another reparse point): where it leads is not checked")

// FinalPath is the path the system really opens for dir (Windows:
// GetFinalPathNameByHandle, which resolves junctions, symbolic links, subst
// and mapped drive letters; elsewhere filepath.EvalSymlinks).
func FinalPath(dir string) (string, error) { return finalPath(dir) }

// Save records dir's current permissions; the returned function puts them
// back (a move into a directory the user chose undoes its Restrict when it
// fails).
func Save(dir string) (func() error, error) {
	return save(dir)
}

func isRoot(p string) bool {
	return filepath.Dir(p) == p || strings.HasSuffix(p, `:\`) || strings.HasSuffix(p, ":/")
}
