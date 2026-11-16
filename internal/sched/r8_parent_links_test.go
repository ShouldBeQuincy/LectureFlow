// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// SEC49 L1: not only the lecture's or material's directory itself — a link
// (a junction) at any folder between the data directory and it (courses,
// the course, its lectures or materials folder) leads a removal elsewhere.
// Deleting the lecture then deletes its rows and touches nothing behind the
// link: the same-named directories there keep their files, no tombstone is
// written there. The data directory itself may be a link (a data directory
// on another drive): an ordinary lecture under it is deleted as before.
func TestDeleteLectureLeavesLinksAboveItsDirectory(t *testing.T) {
	for _, level := range []string{"courses", "course", "lectures", "materials"} {
		t.Run(level, func(t *testing.T) {
			e := newEnv(t, nil)
			outside := t.TempDir()
			l := e.draft()
			mid := db.NewID()
			lid := l.ID
			if err := e.st.CreateMaterial(e.ctx, model.Material{ID: mid, CourseID: l.CourseID, LectureID: &lid,
				Filename: "s.pdf", Path: "s.pdf", CreatedAt: db.Now()}); err != nil {
				t.Fatal(err)
			}
			ldir := model.LectureDir(e.dataDir, l.CourseID, l.ID)
			mdir := model.MaterialDir(e.dataDir, l.CourseID, mid)
			// the folder that becomes a link, and where its target holds the
			// lecture's and the material's directories
			var link string
			switch level {
			case "courses":
				link = filepath.Dir(model.CourseDir(e.dataDir, l.CourseID))
			case "course":
				link = model.CourseDir(e.dataDir, l.CourseID)
			case "lectures":
				link = filepath.Dir(ldir)
			case "materials":
				link = filepath.Dir(mdir)
			}
			target := filepath.Join(outside, "target")
			var files []string
			for _, dir := range []string{ldir, mdir} {
				rel, err := filepath.Rel(link, dir)
				if err != nil || rel == ".." || filepath.IsAbs(rel) || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator) {
					continue // not below this link
				}
				f := filepath.Join(target, rel, "keep.txt")
				if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, []byte("r8 marker"), 0o600); err != nil {
					t.Fatal(err)
				}
				files = append(files, f)
			}
			if len(files) == 0 {
				t.Fatal("setup: no directory below the link")
			}
			mkDirLink(t, link, target)

			if err := e.s.DeleteLecture(e.ctx, l.ID, true); err != nil {
				t.Fatal(err)
			}
			if _, err := e.st.GetLecture(e.ctx, l.ID); err == nil {
				t.Error("the lecture's rows were not deleted")
			}
			for _, f := range files {
				if _, err := os.Stat(f); err != nil {
					t.Errorf("%s (behind the link) was removed: %v", f, err)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(f), state.TombstoneFile)); err == nil {
					t.Errorf("a tombstone was written behind the link next to %s", f)
				}
			}
		})
	}
}

// SEC49 L1: a job whose record directory has a link on its way (here the
// course's lectures folder, leading outside the data directory) fails
// before its stage runs, like one whose id names no directory of its own:
// no stage reads or writes behind the link.
func TestJobBehindALinkFailsBeforeItsStage(t *testing.T) {
	ran := make(chan string, 4)
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU,
		run: func(_ context.Context, id string, _ func(float64)) error { ran <- id; return nil }}
	e := newEnv(t, nil, prep)
	l := e.lecture()
	mkDirLink(t, filepath.Dir(model.LectureDir(e.dataDir, l.CourseID, l.ID)), t.TempDir())
	e.start()
	j := e.waitStatus(l.ID, model.StagePrep, model.StatusFailed)
	if j.Error == nil || !strings.Contains(*j.Error, `"server.internal"`) {
		t.Errorf("error %v, want the internal error", j.Error)
	}
	select {
	case id := <-ran:
		t.Errorf("the stage ran for %s", id)
	default:
	}
}

// The data directory itself may be a link: the folders below it are
// ordinary, so a lecture under it is deleted as before.
func TestDeleteLectureUnderALinkedDataDirectory(t *testing.T) {
	e := newEnv(t, nil)
	real := t.TempDir()
	if err := os.RemoveAll(e.dataDir); err != nil {
		t.Fatal(err)
	}
	mkDirLink(t, e.dataDir, real)
	l := e.draft()
	ldir := model.LectureDir(e.dataDir, l.CourseID, l.ID)
	if err := os.MkdirAll(ldir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ldir, "media.mp4"), []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteLecture(e.ctx, l.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ldir); !os.IsNotExist(err) {
		t.Errorf("the lecture's directory under a linked data directory was not removed: %v", err)
	}
}
