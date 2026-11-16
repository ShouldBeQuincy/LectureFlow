// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"errors"
	"os"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// test-media I6: a missing media file is named as the user imported it,
// not by the stored name (media.mp4); a lecture without an original name
// (created before v2) keeps the stored one.
func TestMediaDisplayName(t *testing.T) {
	for _, tc := range []struct {
		original, path, want string
	}{
		{"Week 3 Stream Ciphers.mp4", "/data/courses/c/lectures/l/media.mp4", "Week 3 Stream Ciphers.mp4"},
		{"", "/data/courses/c/lectures/l/media.mp4", "media.mp4"},
		{"   ", "media.mkv", "media.mkv"},
	} {
		got := mediaDisplayName(model.Lecture{OriginalFilename: tc.original, MediaPath: tc.path})
		if got != tc.want {
			t.Errorf("%q / %q: %q, want %q", tc.original, tc.path, got, tc.want)
		}
	}
}

func TestPrepMediaMissingOriginalName(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	l := e.lecture("missing", "media.mp4")
	if err := e.st.SetLectureOriginalFilename(e.ctx, l.ID, "Lecture 4 - Hashing.mp4"); err != nil {
		t.Fatal(err)
	}
	r := e.run(e.stage(d, model.StagePrep), l.ID)
	var ce *model.CodedError
	if r.err == nil || !sched.IsPermanent(r.err) || !errors.As(r.err, &ce) || ce.Info.Code != CodePrepMediaMissing {
		t.Fatalf("missing media: %v", r.err)
	}
	if ce.Info.Params["file"] != "Lecture 4 - Hashing.mp4" {
		t.Errorf("file param %v, want the original file name", ce.Info.Params["file"])
	}
	// the job keeps the same params (the UI's t("errors.prep.media_missing", params))
	if r.job.Error == nil {
		t.Fatal("job has no error")
	}
	info, _ := state.ParseJobError(*r.job.Error)
	if info == nil || info.Code != CodePrepMediaMissing || info.Params["file"] != "Lecture 4 - Hashing.mp4" {
		t.Errorf("job error %+v", info)
	}
}

// test-media I6: the texts of the ASR stage name the stage as the user
// sees it (预处理), not by its internal name.
func TestASRTextsNoInternalStageName(t *testing.T) {
	for _, f := range []string{"asr.go", "asr_colab.go", "asr_remote.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"重跑 prep", "prep 的产物", "prep 阶段"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s still says %q", f, bad)
			}
		}
	}
}
