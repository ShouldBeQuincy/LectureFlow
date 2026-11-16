// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// SEC49 L1: with a link (a junction) at a folder above the lecture's
// directory — the course's lectures folder, leading outside the data
// directory — the export fails before it creates, writes or removes
// anything: the export/ behind the link keeps its files.
func TestExportTouchesNothingBehindALink(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	l := e.lecture("Week 1", "a.mp4")
	e.insertSentences(l.ID, []model.Sentence{{LectureID: l.ID, Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "Hi.", RepairedText: "Hi.", ZhStatus: model.ZhNone}})
	outside := t.TempDir()
	keep := filepath.Join(outside, l.ID, model.ExportDirName, "keep.bilingual.md")
	e.must(os.MkdirAll(filepath.Dir(keep), 0o755))
	e.must(os.WriteFile(keep, []byte("r8 marker"), 0o644))
	link := filepath.Dir(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID))
	e.must(os.RemoveAll(link))
	e.must(os.MkdirAll(filepath.Dir(link), 0o755))
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v %s", err, out)
		}
	} else {
		e.must(os.Symlink(outside, link))
	}
	t.Cleanup(func() { os.Remove(link) })
	err := (&exportStage{d: d}).Run(e.ctx, l.ID, func(float64) {})
	var ce *model.CodedError
	if err == nil || !sched.IsPermanent(err) || !errors.As(err, &ce) || ce.Info.Code != "server.internal" {
		t.Fatalf("export: %v, want a permanent internal error", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(keep)); len(entries) != 1 {
		t.Errorf("the export folder behind the link changed (%d entries)", len(entries))
	}
}

// R77b "not covered", SEC44 low-2: the export removes the files of export/
// that are not this run's. Its directory is built from ids the database
// holds; for a lecture whose id names no directory of its own ("..": the
// course directory; "x.": lecture x's on Windows; a device name) the stage
// fails before it creates, writes or removes anything — permanently, with
// the existing internal error — and the other directories keep their
// files (the scheduler's start check stops such a job first; this is the
// stage's own guard for the one that removes files).
func TestExportTouchesNoOtherDirectory(t *testing.T) {
	for _, id := range []string{"..", "x.", "CON"} {
		t.Run(id, func(t *testing.T) {
			e := newEnv(t)
			d := e.deps(nil)
			var keep []string
			for _, dir := range []string{model.CourseDir(e.cfg.DataDir, e.course.ID), model.LectureDir(e.cfg.DataDir, e.course.ID, "x")} {
				p := filepath.Join(dir, model.ExportDirName, "keep.bilingual.md")
				e.must(os.MkdirAll(filepath.Dir(p), 0o755))
				e.must(os.WriteFile(p, []byte("r8 marker"), 0o644))
				keep = append(keep, p)
			}
			e.must(e.st.CreateLecture(e.ctx, model.Lecture{ID: id, CourseID: e.course.ID, Title: "Week 1", MediaPath: "m.mp4", CreatedAt: db.Now()}))
			e.insertSentences(id, []model.Sentence{{LectureID: id, Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "Hi.", RepairedText: "Hi.", ZhStatus: model.ZhNone}})
			err := (&exportStage{d: d}).Run(e.ctx, id, func(float64) {})
			var ce *model.CodedError
			if err == nil || !sched.IsPermanent(err) || !errors.As(err, &ce) || ce.Info.Code != "server.internal" {
				t.Fatalf("export: %v, want a permanent internal error", err)
			}
			for _, p := range keep {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("%s was removed: %v", p, err)
				}
				if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
					t.Errorf("%s: something was written beside it (%d entries)", filepath.Dir(p), len(entries))
				}
			}
		})
	}
}
