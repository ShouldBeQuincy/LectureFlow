// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
)

// SEC49 L2: before any run (and so before checkScopeIDs) the scheduler
// looks at a waiting asr job's audio.wav for Colab: its length (audioSec)
// and the path the backend uploads ahead (planEntry → audioPath). For a
// lecture whose directory is not one of its own — an id like ".." (the
// course folder) or "x.", or a link on the way — there is no audio path:
// nothing is read or uploaded. An ordinary lecture's is as before.
func TestAudioPathOnlyInALecturesOwnDirectory(t *testing.T) {
	e := newEnv(t, nil)
	wav := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, model.AudioFile), []byte("RIFF r8 marker"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// what the bad ids name: the course folder, lecture x's folder
	wav(model.CourseDir(e.dataDir, e.course.ID))
	wav(model.LectureDir(e.dataDir, e.course.ID, "x"))
	for _, id := range []string{"..", "x.", "CON"} {
		e.launched(model.Lecture{ID: id, CourseID: e.course.ID, Title: "L", MediaPath: "m.mp4"})
		if p := e.s.audioPath(e.ctx, e.lj(id, model.StageASR)); p != "" {
			t.Errorf("lecture %q: audio path %q, want none", id, p)
		}
	}

	good := e.lecture()
	want := filepath.Join(model.LectureDir(e.dataDir, good.CourseID, good.ID), model.AudioFile)
	if p := e.s.audioPath(e.ctx, e.lj(good.ID, model.StageASR)); p != want {
		t.Errorf("an ordinary lecture: audio path %q, want %q", p, want)
	}

	// another course whose lectures folder is a link leading elsewhere
	other := model.Course{ID: "C2" + good.ID[2:], Name: "Linked"}
	if err := e.st.CreateCourse(e.ctx, other); err != nil {
		t.Fatal(err)
	}
	linked := e.launched(model.Lecture{ID: "L2" + good.ID[2:], CourseID: other.ID, Title: "L", MediaPath: "m.mp4"})
	outside := t.TempDir()
	wav(filepath.Join(outside, linked.ID))
	mkDirLink(t, filepath.Dir(model.LectureDir(e.dataDir, other.ID, linked.ID)), outside)
	if p := e.s.audioPath(e.ctx, e.lj(linked.ID, model.StageASR)); p != "" {
		t.Errorf("a lecture behind a link: audio path %q, want none", p)
	}
	if sec := e.s.audioSec(e.ctx, e.lj(linked.ID, model.StageASR)); sec != 0 {
		t.Errorf("a lecture behind a link: audio length %v read", sec)
	}
}
