// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"path/filepath"
	"testing"

	"lectureflow/internal/fetch"
)

// AUD3 low-3: a finished download takes the media extension check of an
// upload. Its file name keeps a lecture media extension (media.Exts, any
// case); any other extension is replaced by the neutral .mp4 (the content
// was probed: ffmpeg reads it by what it is), so that neither the file in
// the lecture's directory nor its original_filename ends in .exe, .html or
// .part. A name without an extension gets .mp4.
func TestImportDownloadMediaExtension(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	for _, tc := range []struct{ filename, wantName, wantFile string }{
		{"Lecture 3.exe", "Lecture 3.mp4", "media.mp4"},
		{"Lecture 3.html", "Lecture 3.mp4", "media.mp4"},
		{"Lecture 3.part", "Lecture 3.mp4", "media.mp4"},
		{"Lecture 3.aria2", "Lecture 3.mp4", "media.mp4"},
		{"Lecture 3.M4A", "Lecture 3.M4A", "media.m4a"},
		{"Lecture 3.webm", "Lecture 3.webm", "media.webm"},
		{"Lecture 3.5 notes", "Lecture 3.5 notes.mp4", "media.mp4"},
		{"Lecture", "Lecture.mp4", "media.mp4"},
	} {
		res, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{CourseID: c.ID, Path: writeDownload(t, e, "media bytes"),
			Filename: tc.filename, Title: "Lecture 3"})
		if err != nil || res.LectureID == "" {
			t.Fatalf("%q: %+v %v", tc.filename, res, err)
		}
		l, err := e.st.GetLecture(ctx, res.LectureID)
		if err != nil {
			t.Fatal(err)
		}
		if l.OriginalFilename != tc.wantName || filepath.Base(l.MediaPath) != tc.wantFile {
			t.Errorf("%q: original_filename %q, file %s; want %q, %s", tc.filename, l.OriginalFilename,
				filepath.Base(l.MediaPath), tc.wantName, tc.wantFile)
		}
	}
}
