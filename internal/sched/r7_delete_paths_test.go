// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// R77 mid-1 (AUD1 M-1): deleting a lecture removes its own directory and
// those of the materials bound to it, nothing else, whatever ids the
// database holds (an imported library supplies them): a material id
// "../../{another course}" or "..", a lecture id like that, or an id that
// differs from a sibling's directory only in case must not remove another
// course, the whole course or a sibling lecture's files. Everything
// happens under t.TempDir().
func TestDeleteLectureRemovesOnlyItsOwnDirectories(t *testing.T) {
	e := newEnv(t, nil)
	other := model.Course{ID: db.NewID(), Name: "Other"}
	if err := e.st.CreateCourse(e.ctx, other); err != nil {
		t.Fatal(err)
	}
	keep := func(dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, "keep.txt")
		if err := os.WriteFile(f, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	otherFile := keep(model.CourseDir(e.dataDir, other.ID))
	sibling := e.draft()
	siblingFile := keep(model.LectureDir(e.dataDir, e.course.ID, sibling.ID))
	lecture := func(id string) model.Lecture {
		t.Helper()
		l := model.Lecture{ID: id, CourseID: e.course.ID, Title: "L", MediaPath: "m.mp4"}
		if err := e.st.CreateLecture(e.ctx, l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	bind := func(l model.Lecture, materialID string) {
		t.Helper()
		lid := l.ID
		if err := e.st.CreateMaterial(e.ctx, model.Material{ID: materialID, CourseID: e.course.ID, LectureID: &lid,
			Filename: "s.pdf", Path: "s.pdf", CreatedAt: db.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	survives := func(what, f string) {
		t.Helper()
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was removed: %v", what, err)
		}
	}

	// bound materials whose ids climb out of the materials directory
	a := e.draft()
	keep(model.LectureDir(e.dataDir, a.CourseID, a.ID))
	bind(a, "../../"+other.ID)
	bind(a, "..")
	if err := e.s.DeleteLecture(e.ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	survives("another course's directory", otherFile)
	survives("a sibling lecture's directory (the course directory)", siblingFile)

	// lecture ids like that
	for _, id := range []string{"../../" + other.ID, ".."} {
		lecture(id)
		if err := e.s.DeleteLecture(e.ctx, id, true); err != nil {
			t.Fatal(err)
		}
		survives("another course's directory (lecture "+id+")", otherFile)
		survives("a sibling lecture's directory (lecture "+id+")", siblingFile)
		if _, err := os.Stat(filepath.Join(model.CourseDir(e.dataDir, other.ID), state.TombstoneFile)); err == nil {
			t.Errorf("a tombstone was written into another course's directory")
		}
	}

	// an id that differs from the sibling's directory only in case
	lowerID := "own0000000000000000000000"
	low := lecture(lowerID)
	lowFile := keep(model.LectureDir(e.dataDir, low.CourseID, low.ID))
	upper := lecture("OWN0000000000000000000000")
	if err := e.s.DeleteLecture(e.ctx, upper.ID, true); err != nil {
		t.Fatal(err)
	}
	survives("the directory of the lecture that differs only in case", lowFile)

	// an ordinary lecture with a bound material: both directories go
	b := e.draft()
	own := keep(model.LectureDir(e.dataDir, b.CourseID, b.ID))
	mid := db.NewID()
	bind(b, mid)
	mat := keep(model.MaterialDir(e.dataDir, b.CourseID, mid))
	if err := e.s.DeleteLecture(e.ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{own, mat} {
		if _, err := os.Stat(filepath.Dir(f)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed: %v", filepath.Dir(f), err)
		}
	}
}
