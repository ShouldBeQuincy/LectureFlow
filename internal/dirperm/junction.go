// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"errors"
	"path/filepath"
	"strings"
)

// A directory that is itself a junction (Windows: mklink /J, a reparse
// point of the "mount point" kind that names a folder). Check, Restrict and
// the walks look at the link's own entry, never at the folder it leads to
// (security review SEC17, 低-2): a data directory moved to another drive
// with a junction could never be vouched for, and the folder that really
// holds the data was never looked at. Junction tells such a directory
// apart and says where it leads, so that a caller can look at that folder
// instead — and at the link itself, which decides where that is.

// JunctionInfo is what Junction says about a directory that is a junction.
type JunctionInfo struct {
	// Target is the folder the junction leads to, in the usual form
	// (D:\…): where the system really opens it (FinalPath). It is only set
	// when Junction returns no error.
	Target string
	// LinkPath is where the link itself really lies (the final path of the
	// link, not of what it leads to: a subst or mapped drive letter in dir
	// resolved). Set with Target.
	LinkPath string
	// Link is what other accounts may do with the link itself (Write only:
	// another account owns it, or an entry of its DACL gives another account
	// a write right, as Check judges a directory). Whoever may change the
	// link may point it elsewhere. Reading it only shows where it leads.
	Link Exposure
}

// Junction reports whether dir itself is a junction (not a symbolic link,
// not any other kind of reparse point) and, when it is, where it leads and
// what other accounts may do with the link. It only reads.
//
// isJunction is false, with a nil error, for anything else (macOS and Linux
// have no junctions); an error with isJunction false means dir could not
// be looked at (the caller treats dir as before). With isJunction true, an
// error says why the junction cannot be vouched for — nothing may then be
// checked or changed through it:
//
//   - where it leads cannot be read: the folder is missing, the links form
//     a loop;
//   - it does not name a folder on a drive letter (a volume, a device, a
//     share), or it names the root of a drive;
//   - the folder it names is reached through another link (a junction to a
//     junction, a folder below one): the path it holds is not the path the
//     system really opens;
//   - what it leads to is not a plain folder.
//
// Link is filled in whenever the link's own permissions could be read, also
// next to such an error.
func Junction(dir string) (info JunctionInfo, isJunction bool, err error) {
	return junction(dir)
}

// ResolveJunction says where the permissions of the directory dir are to
// be looked at and set (security review SEC17 低-2, SEC30 低-3: the
// diagnostics and the in-app update ask it alike).
//
// A directory that is not a junction (or cannot be looked at as one) is
// itself: root is dir, unchanged. A junction is the folder it leads to —
// root is that folder, isJunction true — but only when nobody else can have
// chosen that folder: Junction vouches for where it leads; no other
// account owns the link or may change it (whoever may, may point it
// elsewhere); and none may remove or rename the link or a folder above it
// (where it is named and where it really lies), nor a folder above the
// folder it leads to (CheckAbove, below top: "" = up to the root of the
// drive). Reading the link only shows where it leads.
//
// Otherwise doubt says why (short English naming dir: "<dir> is a
// junction: …"), root is dir, and nothing is to be checked or changed
// through it.
func ResolveJunction(dir, top string) (root string, isJunction bool, doubt string) {
	info, isJunction, err := Junction(dir)
	if !isJunction {
		return dir, false, ""
	}
	abs, aerr := filepath.Abs(dir)
	if aerr != nil {
		abs = dir
	}
	doubtOf := func(why string) (string, bool, string) {
		return dir, true, abs + " is a junction: " + why
	}
	if info.Link.Write {
		return doubtOf("other accounts can change the link (" + strings.Join(info.Link.Writers, ", ") + "), so where it leads is not checked")
	}
	if err != nil {
		return doubtOf(err.Error())
	}
	// the link where it is named and where it really lies, and the folder
	// it leads to
	above := []string{abs}
	if !strings.EqualFold(info.LinkPath, abs) {
		above = append(above, info.LinkPath)
	}
	for _, p := range append(above, info.Target) {
		at, e, err := CheckAbove(p, top)
		switch {
		case err != nil && errors.Is(err, ErrLink):
			return doubtOf(at + " " + ErrLink.Error())
		case err != nil:
			return doubtOf("the folders above " + p + " cannot be examined: " + err.Error())
		case e.Write:
			return doubtOf("other accounts can move " + at + " (" + strings.Join(e.Writers, ", ") + "), and with it " + p)
		}
	}
	return info.Target, true, ""
}
