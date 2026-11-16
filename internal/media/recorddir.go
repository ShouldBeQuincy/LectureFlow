// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Record directories: {data_dir}/courses/{course id}/lectures/{lecture id}
// and …/materials/{material id} are built from ids read from the database
// (model.CourseDir and friends). The application's own ids are ULIDs, but
// an imported library supplies its own rows (AUD1 M-1): an id such as
// "../../{another course}" or ".." would make "remove this lecture's
// directory" remove another course, the whole course or a folder outside
// the data directory, and "OWN" removes the directory "own" of another
// record on a file system that ignores case (R77 low-2). Whoever removes or
// writes a record directory checks its ids with IDElement, the folders on
// its way with PlainPath and, before removing, the directory with OwnDirIn.

// PlainDir reports whether m, the mode os.Lstat (or a DirEntry) gives, is
// an ordinary directory: a directory that is no link. Go reports a
// symbolic link as ModeSymlink and, on Windows, a junction (a mount point)
// as ModeIrregular without ModeDir: neither is one. A directory behind
// another kind of reparse point — the placeholder of a OneDrive or Dropbox
// folder kept in the cloud — is ModeDir|ModeIrregular: it is one (a data
// directory in a synced folder must keep working; SEC49b). One rule for
// OwnDir, PlainPath and the startup sweep (state.SweepLectureDirs).
func PlainDir(m fs.FileMode) bool { return m.IsDir() && m&fs.ModeSymlink == 0 }

// PlainPath reports whether dir, a record directory under the data
// directory root, leads only where its path says: it lies strictly inside
// root (lexically, after filepath.Abs) and every folder from below root
// down to dir itself — courses, the course, its lectures or materials
// folder, the record's — that exists is an ordinary directory (os.Lstat,
// never followed; PlainDir: not a junction, a symbolic link or a file). A
// link at any of them would lead a removal or a write
// somewhere else (SEC49 L1). Folders that do not exist yet are fine
// (whoever makes them makes ordinary ones), and nothing below a missing
// one is there to look at. root itself may be a link: a data directory on
// another drive is the user's choice. No root: false.
func PlainPath(root, dir string) bool {
	if root == "" {
		return false
	}
	r, err1 := filepath.Abs(root)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(r, d)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	p := r
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return true
		}
		if err != nil || !PlainDir(fi.Mode()) {
			return false
		}
	}
	return true
}

// OwnDirIn is OwnDir for dir, a record directory under the data directory
// root, with the folders on its way checked too (PlainPath): a link at
// courses, the course or its lectures or materials folder leads a removal
// elsewhere as surely as a link at dir itself (SEC49 L1). exists is
// OwnDir's (something is found at dir, through whatever links).
func OwnDirIn(root, dir string) (own, exists bool) {
	own, exists = OwnDir(dir)
	return own && PlainPath(root, dir), exists
}

// IDElement reports whether id, a course, lecture or material id read from
// the database, can name one directory below its parent, its own: not
// empty, not "." or "..", without a path separator or a drive colon,
// without a trailing dot or space (Win32 drops them: "x." opens the
// directory of record "x") and not a device name (DeviceName). The rule
// of the data import (internal/relocate) is stricter (letters, digits,
// '_' and '-' only); this one also covers what reached the database
// before that rule or by another way (R77b (d)).
func IDElement(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\:`) &&
		strings.TrimRight(id, ". ") == id && !DeviceName(id)
}

// DeviceName reports a name Windows takes for a device whatever its
// directory: CON, PRN, AUX, NUL, COM0–9, LPT0–9 (also with ¹ ² ³),
// CONIN$, CONOUT$, in any case, also before an extension or trailing
// spaces ("NUL.txt", "con .pdf"). The same rule as the update manifest's
// file names (internal/selfupdate) and the data import's.
func DeviceName(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if r := []rune(stem); len(r) == 4 && (string(r[:3]) == "COM" || string(r[:3]) == "LPT") {
		return r[3] >= '0' && r[3] <= '9' || r[3] == '¹' || r[3] == '²' || r[3] == '³'
	}
	return false
}

// OwnDir reports whether dir is an ordinary directory, the entry of its
// parent named exactly filepath.Base(dir), byte for byte (own), and
// whether anything is found at dir (exists). On a file system that ignores
// case "…/OWN" opens a sibling "own", and Win32 drops trailing dots and
// spaces ("own." opens "own"): exists is then true and own false — that
// directory belongs to another record. An entry that is not an ordinary
// directory (PlainDir) — a link (a junction or a symbolic link: removing
// "its" files would follow it to wherever it leads; R77b low-3) or a file
// — is not own either: the same rule as the startup sweep
// (state.SweepLectureDirs, which follows no link). A directory whose
// parent cannot be listed is not own.
func OwnDir(dir string) (own, exists bool) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return false, false
	}
	if !PlainDir(fi.Mode()) {
		return false, true
	}
	f, err := os.Open(filepath.Dir(dir))
	if err != nil {
		return false, true
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return false, true
	}
	name := filepath.Base(dir)
	for _, n := range names {
		if n == name {
			return true, true
		}
	}
	return false, true
}
