// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/fetch"
	"lectureflow/internal/model"
)

// R77 mid-1 / SEC44 table B: the handlers that write into a record
// directory built from database ids (uploads of a lecture, a material, a
// syllabus; an imported download) refuse ids that are not single path
// elements instead of writing outside their place. The handlers are called
// with the id set directly (an imported database supplies such ids, the
// router never passes them). Everything happens under t.TempDir().
func TestUploadsRefuseUnsafeRecordIDs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bad := "../../victim"
	if err := e.st.CreateCourse(ctx, model.Course{ID: bad, Name: "Imported"}); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(e.dir, "victim")
	if got := filepath.Clean(model.CourseDir(e.cfg.DataDir, bad)); got != victim {
		t.Fatalf("test setup: %s is not %s", got, victim)
	}
	untouched := func(what string) {
		t.Helper()
		if _, err := os.Stat(victim); !os.IsNotExist(err) {
			t.Errorf("%s wrote outside the data directory (%s): %v", what, victim, err)
			os.RemoveAll(victim)
		}
	}
	multipart := func(h func(http.ResponseWriter, *http.Request), fields [][2]string, filename string, content []byte) int {
		t.Helper()
		body, ctype := multipartBody(t, fields, filename, content)
		req := httptest.NewRequest(http.MethodPost, "/api/courses/x/y", body)
		req.Header.Set("Content-Type", ctype)
		req.SetPathValue("id", bad)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	pdf := []byte("%PDF-1.4\n%fake\n")
	if code := multipart(e.srv.uploadMaterial, nil, "slides.pdf", pdf); code < 400 {
		t.Errorf("uploadMaterial: %d, want an error", code)
	}
	untouched("uploadMaterial")
	if code := multipart(e.srv.uploadLecture, [][2]string{{"title", "L"}}, "rec.mp4", []byte("fake media")); code < 400 {
		t.Errorf("uploadLecture: %d, want an error", code)
	}
	untouched("uploadLecture")
	if code := multipart(e.srv.uploadSyllabus, nil, "syllabus.txt", []byte("week 1")); code < 400 {
		t.Errorf("uploadSyllabus: %d, want an error", code)
	}
	untouched("uploadSyllabus")
	if code := multipart(e.srv.syllabusRaw, nil, "syllabus.txt", []byte("week 1")); code < 400 {
		t.Errorf("syllabusRaw: %d, want an error", code)
	}
	untouched("syllabusRaw")
	b, _ := json.Marshal(SyllabusSaveInput{Markdown: "# Week 1"})
	req := httptest.NewRequest(http.MethodPut, "/api/courses/x/syllabus", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", bad)
	rec := httptest.NewRecorder()
	e.srv.saveSyllabus(rec, req)
	if rec.Code < 400 {
		t.Errorf("saveSyllabus: %d, want an error", rec.Code)
	}
	untouched("saveSyllabus")

	// an extension download handed over for that course
	src := filepath.Join(e.dir, "download.mp4")
	if err := os.WriteFile(src, []byte("fake media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{CourseID: bad, Path: src, Filename: "download.mp4"}); err == nil {
		t.Errorf("ImportDownload: no error")
	}
	untouched("ImportDownload")
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the refused download's file is gone: %v", err)
	}
}
