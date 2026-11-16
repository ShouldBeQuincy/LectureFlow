// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// mkDirLink makes link a link to the directory target: a junction on
// Windows (mklink /J needs no privilege), a symbolic link elsewhere. The
// link itself is removed first at the end, never what it leads to.
func mkDirLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J %s %s: %v %s", link, target, err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
}

// R77b low-3, R77 info-3: a lecture directory, or a bound material's,
// that is a link (a junction) named exactly like the record and leading
// outside the data directory is not the record's own directory: deleting
// the lecture deletes its rows and leaves the link and everything it
// leads to — no tombstone written there, no file removed (before, the
// tombstone went to the link's target and the files there were removed).
// An ordinary lecture of the same course is deleted as before.
func TestDeleteLectureLeavesLinkedDirectories(t *testing.T) {
	e := newEnv(t, nil)
	outside := t.TempDir() // another place than e.dataDir
	write := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("r8 marker"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	lecTarget, matTarget := filepath.Join(outside, "lecture"), filepath.Join(outside, "material")
	files := []string{
		write(filepath.Join(lecTarget, "keep.txt")),
		write(filepath.Join(lecTarget, "media.mp4")),
		write(filepath.Join(lecTarget, "export", "keep.md")),
		write(filepath.Join(matTarget, "source.pdf")),
	}

	l := e.draft()
	ldir := model.LectureDir(e.dataDir, l.CourseID, l.ID)
	mkDirLink(t, ldir, lecTarget)
	mid := db.NewID()
	lid := l.ID
	if err := e.st.CreateMaterial(e.ctx, model.Material{ID: mid, CourseID: l.CourseID, LectureID: &lid,
		Filename: "s.pdf", Path: "s.pdf", CreatedAt: db.Now()}); err != nil {
		t.Fatal(err)
	}
	mdir := model.MaterialDir(e.dataDir, l.CourseID, mid)
	mkDirLink(t, mdir, matTarget)

	if err := e.s.DeleteLecture(e.ctx, l.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetLecture(e.ctx, l.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the lecture's rows: %v, want them deleted", err)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s (behind the link) was removed: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(lecTarget, state.TombstoneFile)); err == nil {
		t.Error("a tombstone was written behind the lecture's link")
	}
	for _, link := range []string{ldir, mdir} {
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("the link %s was removed: %v", link, err)
		}
	}

	// an ordinary lecture: tombstone → rows → directory, as before
	o := e.draft()
	odir := model.LectureDir(e.dataDir, o.CourseID, o.ID)
	write(filepath.Join(odir, "media.mp4"))
	if err := e.s.DeleteLecture(e.ctx, o.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(odir); !os.IsNotExist(err) {
		t.Errorf("an ordinary lecture's directory was not removed: %v", err)
	}
}
