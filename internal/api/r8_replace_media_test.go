// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// R77b "not covered": replacing a draft's media removes the old file only
// when it lies strictly inside this lecture's own directory — not merely
// inside the data directory (another lecture's media, the course folder,
// the database). Nothing here touches the disk.
func TestReplacedMediaToRemoveOnlyInItsOwnDirectory(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	l := model.Lecture{ID: db.NewID(), CourseID: db.NewID()}
	lecDir := model.LectureDir(s.dataDir, l.CourseID, l.ID)
	newPath := filepath.Join(lecDir, "media.mkv")
	for _, tc := range []struct{ media, want string }{
		{filepath.Join(lecDir, "media.mp4"), filepath.Join(lecDir, "media.mp4")},
		{"media.mp4", filepath.Join(lecDir, "media.mp4")},
		{filepath.Join(lecDir, "sub", "media.mp4"), filepath.Join(lecDir, "sub", "media.mp4")},
		{filepath.Join(model.LectureDir(s.dataDir, l.CourseID, db.NewID()), "media.mp4"), ""}, // another lecture's
		{filepath.Join(model.CourseDir(s.dataDir, l.CourseID), "syllabus.pdf"), ""},           // the course folder
		{filepath.Join(s.dataDir, "lectureflow.db"), ""},                                      // the database
		{filepath.Join(lecDir+"2", "media.mp4"), ""},                                          // a sibling sharing its name
		{lecDir, ""}, // the directory itself
		{filepath.Join(lecDir, "..", "x.mp4"), ""},
	} {
		l.MediaPath = tc.media
		if got := s.replacedMediaToRemove(l, newPath); got != tc.want {
			t.Errorf("media_path %q: remove %q, want %q", tc.media, got, tc.want)
		}
	}
}

// R77b (d), SEC44 low-2: a draft whose id ends in a dot ("x.": Win32 opens
// the directory of lecture "x") gets no new media: the request fails with
// the internal error before anything is written, and lecture x's media is
// as it was.
func TestReplaceMediaOfAPathLikeIDWritesNothing(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	ctx := context.Background()
	sibling := model.LectureDir(e.cfg.DataDir, c.ID, "x")
	marker := filepath.Join(sibling, "media.mkv")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("r8 marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.st.CreateLecture(ctx, model.Lecture{ID: "x.", CourseID: c.ID, Title: "L", MediaPath: marker, CreatedAt: db.Now()}); err != nil {
		t.Fatal(err)
	}
	res, b := e.tokenUpload("PUT", "/api/lectures/x./media", nil, "better.mkv", []byte("better media"))
	if res.StatusCode != http.StatusInternalServerError || errCode(t, b) != "server.internal" {
		t.Fatalf("replace media: %d %s", res.StatusCode, b)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "r8 marker" {
		t.Fatalf("lecture x's media: %q %v", got, err)
	}
	if entries, _ := os.ReadDir(sibling); len(entries) != 1 {
		t.Errorf("something was written into lecture x's directory (%d entries)", len(entries))
	}
}
