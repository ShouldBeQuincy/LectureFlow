// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// SEC49b: the startup sweep looks at every folder from below the data
// directory down to a tombstoned lecture's directory (media.PlainPath), so
// a link at courses itself — not only at a course or its lectures folder,
// which it skipped before — keeps it from removing what the link leads to.
// An ordinary tombstoned directory of a deleted lecture is removed as
// before.
func TestSweepLectureDirsSkipsALinkedCoursesFolder(t *testing.T) {
	f := newFixture(t)
	target := t.TempDir()
	cid, id := db.NewID(), db.NewID()
	behind := filepath.Join(target, cid, "lectures", id)
	mkfile(t, filepath.Join(behind, "precious.txt"))
	tomb(t, behind, id)
	if err := os.MkdirAll(f.dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	courses := filepath.Dir(model.CourseDir(f.dataDir, cid))
	if err := makeDirLink(target, courses); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(courses) })
	if n := SweepLectureDirs(f.ctx, f.st, f.dataDir); n != 0 {
		t.Errorf("removed %d through a linked courses folder", n)
	}
	if !exists(filepath.Join(behind, "precious.txt")) {
		t.Fatal("the sweep removed what the courses link leads to")
	}

	// an ordinary data directory: the deleted lecture's directory goes
	g := newFixture(t)
	dir := model.LectureDir(g.dataDir, g.course.ID, db.NewID())
	mkfile(t, filepath.Join(dir, "media.mp4"))
	tomb(t, dir, filepath.Base(dir))
	if n := SweepLectureDirs(g.ctx, g.st, g.dataDir); n != 1 || exists(dir) {
		t.Errorf("an ordinary tombstoned directory: removed %d, still there %v", n, exists(dir))
	}
}

// SEC49 L1: a link (a junction) at a folder above the lecture's directory —
// here the course's lectures folder, leading outside the data directory —
// makes the rerun's and the retry's cleanup touch nothing: the files behind
// the link stay (the rows are rerun and retried as for any lecture).
func TestRerunAndRetryTouchNothingBehindALink(t *testing.T) {
	f := newFixture(t)
	id := db.NewID()
	outside := t.TempDir()
	behind := filepath.Join(outside, id)
	var keep []string
	for _, name := range []string{model.AudioFile, filepath.Join(model.ExportDirName, "keep.md"), model.ASRCheckpointFile} {
		p := filepath.Join(behind, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("r8 marker"), 0o644); err != nil {
			t.Fatal(err)
		}
		keep = append(keep, p)
	}
	link := filepath.Dir(model.LectureDir(f.dataDir, f.course.ID, id))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v %s", err, out)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
	kept := func(after string) {
		t.Helper()
		for _, p := range keep {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s: %s (behind the link) was removed (%v)", after, p, err)
			}
		}
	}
	media := filepath.Join(t.TempDir(), "media.mp4")
	if err := os.WriteFile(media, []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.st.CreateLecture(f.ctx, model.Lecture{ID: id, CourseID: f.course.ID, Title: "L", MediaPath: media}); err != nil {
		t.Fatal(err)
	}
	snaps := launchSnaps()
	snaps[model.StageASR] = autoSnap(bindNPU, "en")
	if err := Launch(f.ctx, f.st, id, snaps); err != nil {
		t.Fatal(err)
	}
	for _, s := range model.LectureStages {
		f.done(model.ScopeLecture, id, s)
	}
	if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeLecture, id, model.StagePrep); err != nil {
		t.Fatal(err)
	}
	kept("rerun")
	f.done(model.ScopeLecture, id, model.StagePrep)
	f.drive(model.ScopeLecture, id, model.StageASR, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	small := bindNPU
	small.Model, small.Precision = "small", model.PrecisionINT8
	if err := RetryWith(f.ctx, f.st, f.dataDir, model.ScopeLecture, id, model.StageASR,
		map[string]string{model.StageASR: autoSnap(small, "en")}); err != nil {
		t.Fatal(err)
	}
	kept("retry with another model")
}

// R77b "not covered", SEC44 low-2: the rerun's file cleanup (audio.wav,
// export/) and the retry's removal of the asr checkpoint build the lecture
// directory from ids the database holds (an imported library supplies
// them). A lecture id that is not one path element of a directory of its
// own — "..", which names the course directory, "x." and "x " (Win32
// drops the trailing dot or space: the directory of lecture "x"), a device
// name — touches no file: neither the course directory nor a sibling
// lecture's. The database part of the rerun and the retry is the same as
// for any lecture (§4 unchanged). Everything happens under t.TempDir().
func TestRerunAndRetryTouchNoOtherDirectory(t *testing.T) {
	for _, id := range []string{"..", "x.", "x ", "CON"} {
		t.Run(id, func(t *testing.T) {
			f := newFixture(t)
			courseDir := model.CourseDir(f.dataDir, f.course.ID)
			sibling := model.LectureDir(f.dataDir, f.course.ID, "x")
			var keep []string
			for _, dir := range []string{courseDir, sibling} {
				for _, name := range []string{model.AudioFile, filepath.Join(model.ExportDirName, "keep.md"),
					model.ASRCheckpointFile, model.ASRCheckpointFile + ".tmp"} {
					p := filepath.Join(dir, name)
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, []byte("r8 marker"), 0o644); err != nil {
						t.Fatal(err)
					}
					keep = append(keep, p)
				}
			}
			kept := func(after string) {
				t.Helper()
				for _, p := range keep {
					if _, err := os.Stat(p); err != nil {
						t.Errorf("%s: %s was removed (%v)", after, p, err)
					}
				}
			}
			media := filepath.Join(t.TempDir(), "media.mp4") // the prep rerun needs the media
			if err := os.WriteFile(media, []byte("m"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := f.st.CreateLecture(f.ctx, model.Lecture{ID: id, CourseID: f.course.ID, Title: "L", MediaPath: media}); err != nil {
				t.Fatal(err)
			}
			snaps := launchSnaps()
			snaps[model.StageASR] = autoSnap(bindNPU, "en")
			if err := Launch(f.ctx, f.st, id, snaps); err != nil {
				t.Fatal(err)
			}
			for _, s := range model.LectureStages {
				f.done(model.ScopeLecture, id, s)
			}

			if err := Rerun(f.ctx, f.st, f.dataDir, model.ScopeLecture, id, model.StagePrep); err != nil {
				t.Fatal(err)
			}
			if got := f.lj(id, model.StagePrep); got.Status != model.StatusPending {
				t.Errorf("rerun: prep is %s, want pending", got.Status)
			}
			kept("rerun")

			f.done(model.ScopeLecture, id, model.StagePrep)
			f.drive(model.ScopeLecture, id, model.StageASR, model.StatusQueued, model.StatusRunning, model.StatusFailed)
			small := bindNPU
			small.Model, small.Precision = "small", model.PrecisionINT8
			if err := RetryWith(f.ctx, f.st, f.dataDir, model.ScopeLecture, id, model.StageASR,
				map[string]string{model.StageASR: autoSnap(small, "en")}); err != nil {
				t.Fatal(err)
			}
			if got := f.lj(id, model.StageASR); got.Status != model.StatusQueued {
				t.Errorf("retry: asr is %s, want queued", got.Status)
			}
			kept("retry with another model")
		})
	}
}
