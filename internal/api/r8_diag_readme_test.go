// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// The diagnostics zip's README says what the zip leaves out the way the
// settings page does (settings.app.diag_privacy): recordings, transcripts,
// translations, slides and syllabi — and that holds: every file of the zip
// is one the README lists (the logs under logs/), and none holds the text
// of a transcript, a translation, a slide or a syllabus of the library.
func TestDiagnosticsReadmeSaysWhatIsLeftOut(t *testing.T) {
	stubAppInfo(t)
	e := newEnv(t)
	ctx := context.Background()
	const (
		sentence    = "r8-marker transcript sentence"
		translation = "r8-marker 译文"
		slides      = "r8-marker slide page"
		syllabus    = "r8-marker syllabus text"
	)
	c := e.course("Operating Systems")
	l := e.lecture(c.ID, "Week 1")
	if err := e.st.InsertSentences(ctx, l.ID, []model.Sentence{{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: sentence, RepairedText: sentence}}); err != nil {
		t.Fatal(err)
	}
	zh := translation
	if err := e.st.SetTranslation(ctx, l.ID, 0, db.SentenceTr{Zh: &zh, ZhStatus: model.ZhOK}); err != nil {
		t.Fatal(err)
	}
	m := model.Material{ID: db.NewID(), CourseID: c.ID, Filename: "slides.pdf", Path: "slides.pdf", CreatedAt: db.Now()}
	if err := e.st.CreateMaterial(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetMaterialPages(ctx, m.ID, []string{slides}); err != nil {
		t.Fatal(err)
	}
	sy := syllabus
	if err := e.st.UpsertSyllabus(ctx, model.Syllabus{CourseID: c.ID, Text: &sy}); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(e.cfg.DataDir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "lectureflow.log"), []byte("level=INFO msg=\"job done\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, body := e.doWith("POST", "/api/app/diagnostics", nil, "", "")
	if res.StatusCode != 200 {
		t.Fatalf("diagnostics: %d %s", res.StatusCode, body)
	}
	files := unzipAll(t, body)
	readme := files["README.txt"]
	if !strings.Contains(strings.Join(strings.Fields(readme), " "), "recordings, transcripts, translations, slides and syllabi") {
		t.Errorf("README does not name what is left out as the settings page does:\n%s", readme)
	}
	if strings.Contains(readme, "course materials") {
		t.Errorf("README still says \"course materials\":\n%s", readme)
	}
	for name, content := range files {
		listed := strings.HasPrefix(name, "logs/") || name == "README.txt" ||
			strings.Contains(readme, "\n  "+name+" ")
		if !listed {
			t.Errorf("%s is in the zip but not in its README", name)
		}
		for _, marker := range []string{sentence, translation, slides, syllabus} {
			if strings.Contains(content, marker) {
				t.Errorf("%s holds %q", name, marker)
			}
		}
	}
}
