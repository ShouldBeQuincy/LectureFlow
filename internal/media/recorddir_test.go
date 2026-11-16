// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// IDElement: a record id read from the database names one directory below
// its parent, or nothing at all (AUD1 M-1, R77 mid-1).
func TestIDElement(t *testing.T) {
	for _, id := range []string{"01JD000000000000000000AB", "own", "a-b_c", "x.y", ".x", "COM10", "CONX", "a b"} {
		if !IDElement(id) {
			t.Errorf("IDElement(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", ".", "..", "../x", `..\x`, "a/b", `a\b`, "C:", `C:\x`, "x:stream", "/abs", `\\host\share`,
		// R77b (d): a trailing dot or space (Win32 drops it: "x." opens
		// "x"; "..." is ".." and more dots) and the device names
		"...", "x.", "x ", "x. ", "CON", "con", "nul", "Aux", "PRN", "COM1", "lpt9", "COM¹", "NUL.txt", "con .pdf", "CONIN$"} {
		if IDElement(id) {
			t.Errorf("IDElement(%q) = true, want false", id)
		}
	}
}

// OwnDir: only an entry named exactly like the record is its directory. On a
// file system that ignores case (Windows, macOS) "OWN" opens "own" — that
// is another record's directory (R77 low-2); on one that does not, "OWN"
// does not exist.
func TestOwnDir(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "own"), 0o700); err != nil {
		t.Fatal(err)
	}
	if own, exists := OwnDir(filepath.Join(parent, "own")); !own || !exists {
		t.Errorf("own: own %v exists %v, want true true", own, exists)
	}
	if own, _ := OwnDir(filepath.Join(parent, "OWN")); own {
		t.Errorf("OWN is not the directory of record OWN (only \"own\" is there)")
	}
	if own, exists := OwnDir(filepath.Join(parent, "missing")); own || exists {
		t.Errorf("missing: own %v exists %v, want false false", own, exists)
	}
	// Win32 drops trailing dots and spaces: "own." would open "own".
	if own, _ := OwnDir(filepath.Join(parent, "own.")); own {
		t.Errorf(`"own." is not the directory of record "own."`)
	}
}

// mkLink makes link a junction (Windows) or symbolic link to target; the
// link itself is removed first at the end.
func mkLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v %s", err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
}

// SEC49b: an ordinary directory is a directory that is no link. Go reports
// a junction as ModeIrregular without ModeDir (no IsDir), a symbolic link
// as ModeSymlink, and a directory behind another kind of reparse point —
// a OneDrive or Dropbox placeholder of a synced folder — as
// ModeDir|ModeIrregular: that one is an ordinary directory, the links are
// not. A real junction on this machine is checked too.
func TestPlainDirMode(t *testing.T) {
	for m, want := range map[fs.FileMode]bool{
		fs.ModeDir:                        true,
		fs.ModeDir | fs.ModeIrregular:     true, // a cloud placeholder directory
		fs.ModeDir | 0o755:                true,
		fs.ModeIrregular:                  false, // a junction, as Go reports it
		fs.ModeSymlink:                    false,
		fs.ModeDir | fs.ModeSymlink:       false,
		0:                                 false, // a file
		fs.ModeDevice | fs.ModeCharDevice: false,
	} {
		if got := PlainDir(m); got != want {
			t.Errorf("PlainDir(%v) = %v, want %v", m, got, want)
		}
	}
	parent, target := t.TempDir(), t.TempDir()
	link := filepath.Join(parent, "j")
	mkLink(t, link, target)
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if PlainDir(fi.Mode()) {
		t.Errorf("a junction / symbolic link (Lstat mode %v) counts as an ordinary directory", fi.Mode())
	}
	if fi, err := os.Lstat(target); err != nil || !PlainDir(fi.Mode()) {
		t.Errorf("an ordinary directory: %v %v", fi, err)
	}
}

// SEC49 L1: PlainPath looks at every folder from below the data directory
// down to the record's directory (courses, the course, lectures, the
// record): a link at any of them is refused, found through Lstat (never
// followed); folders that do not exist yet are fine. The data directory
// itself may be a link. OwnDirIn is OwnDir plus that.
func TestPlainPathEveryLevel(t *testing.T) {
	rel := []string{"courses", "C1", "lectures", "L1"}
	for i := range rel {
		t.Run(rel[i], func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dir := filepath.Join(append([]string{root}, rel...)...)
			link := filepath.Join(append([]string{root}, rel[:i+1]...)...)
			// behind the link, the same folders down to the record's
			target := filepath.Join(append([]string{outside}, rel[i+1:]...)...)
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			mkLink(t, link, outside)
			if PlainPath(root, dir) {
				t.Errorf("a link at %s: PlainPath true", rel[i])
			}
			if own, exists := OwnDirIn(root, dir); own || !exists {
				t.Errorf("a link at %s: OwnDirIn own %v exists %v, want false true", rel[i], own, exists)
			}
		})
	}

	root := t.TempDir()
	dir := filepath.Join(root, "courses", "C1", "lectures", "L1")
	if !PlainPath(root, dir) {
		t.Error("nothing there yet: PlainPath false")
	}
	if own, exists := OwnDirIn(root, dir); own || exists {
		t.Errorf("nothing there yet: OwnDirIn own %v exists %v", own, exists)
	}
	if err := os.MkdirAll(filepath.Join(root, "courses", "C1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !PlainPath(root, dir) {
		t.Error("the course only: PlainPath false")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !PlainPath(root, dir) {
		t.Error("ordinary folders: PlainPath false")
	}
	if own, exists := OwnDirIn(root, dir); !own || !exists {
		t.Errorf("ordinary folders: OwnDirIn own %v exists %v", own, exists)
	}
	// the data directory itself a link
	linkedRoot := filepath.Join(t.TempDir(), "data")
	mkLink(t, linkedRoot, root)
	if own, _ := OwnDirIn(linkedRoot, filepath.Join(linkedRoot, "courses", "C1", "lectures", "L1")); !own {
		t.Error("under a linked data directory: not own")
	}
	// not below the data directory, or the data directory itself
	for _, p := range []string{root, filepath.Dir(root), filepath.Join(root, "..", "x"), filepath.Join(t.TempDir(), "courses")} {
		if PlainPath(root, p) {
			t.Errorf("%s: PlainPath true outside the data directory", p)
		}
	}
	if PlainPath("", dir) {
		t.Error("no data directory: PlainPath true")
	}
}

// R77b low-3: an entry named exactly like the record that is not an
// ordinary directory — a link (a junction on Windows, a symbolic link
// elsewhere) to a folder somewhere else, or a file — is not the record's
// directory: removing it would follow the link (a tombstone written behind
// it, the files there removed). Found, but not own.
func TestOwnDirRefusesLinksAndFiles(t *testing.T) {
	parent := t.TempDir()
	target := t.TempDir() // another place
	link := filepath.Join(parent, "linked")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v %s", err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) }) // the link itself, never what it leads to
	if own, exists := OwnDir(link); own || !exists {
		t.Errorf("a link: own %v exists %v, want false true", own, exists)
	}
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if own, exists := OwnDir(file); own || !exists {
		t.Errorf("a file: own %v exists %v, want false true", own, exists)
	}
}
