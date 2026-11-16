// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/fetch"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
)

// test-media I7: .m2ts (camcorders, Blu-ray), .mka and .ogv are lecture
// media — their contents are MPEG-TS, Matroska and Ogg, whose demuxers
// the whitelist has — by HTTP upload and by local path.
func TestUploadM2TSMKAOGV(t *testing.T) {
	probe := &fakeProbe{}
	e := newEnv(t, func(s *Server) { s.runCmd = probe.run })
	withFakeFFprobe(t, e)
	c := e.course("C")
	for _, name := range []string{"Sony 0001.M2TS", "talk.mka", "theora.ogv"} {
		res, b := e.upload("/api/courses/"+c.ID+"/lectures", nil, name, []byte("fake media"))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, res.StatusCode, b)
		}
		var l Lecture
		decodeInto(t, b, &l)
		if l.OriginalFilename != name || l.Title != strings.TrimSuffix(name, filepath.Ext(name)) {
			t.Errorf("%s: title %q original %q", name, l.Title, l.OriginalFilename)
		}
	}
	e.desktopOn()
	src := t.TempDir()
	for _, name := range []string{"b.m2ts", "b.mka", "b.ogv"} {
		res, b := e.pathUpload("/api/courses/"+c.ID+"/lectures", map[string]any{"path": writeSrc(t, src, name, []byte("fake media"))})
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("path %s: %d %s", name, res.StatusCode, b)
		}
	}
}

func noBidi(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) })
}

// test-media I8: a title is never empty, has no surrounding white space
// and no bidirectional control characters (a right-to-left override would
// make an export's file name show another extension); the original file
// name is kept as it was.
func TestLectureTitleEdges(t *testing.T) {
	probe := &fakeProbe{}
	e := newEnv(t, func(s *Server) { s.runCmd = probe.run })
	withFakeFFprobe(t, e)
	c := e.course("C")
	for _, tc := range []struct {
		name, field  string
		title, sourc string
	}{
		{".mp4", "", defaultLectureTitle, model.TitleSourceFilename},
		{"\u202e.mp4", "", defaultLectureTitle, model.TitleSourceFilename},
		{"  leading spaces.mp4", "", "leading spaces", model.TitleSourceFilename},
		{"  leading spaces.mp4", "  leading spaces", "leading spaces", model.TitleSourceFilename}, // the form sends the name without its extension
		{"Week 3 \u202ekcod.mp4", "", "Week 3 kcod", model.TitleSourceFilename},
		{"Week 3 \u202ekcod.mp4", "Week 3 \u202ekcod", "Week 3 kcod", model.TitleSourceFilename},
		{"x.mp4", " \u2067My title\u2069 ", "My title", model.TitleSourceUser},
		{".mp4", " \u202e ", defaultLectureTitle, model.TitleSourceFilename},
	} {
		var fields [][2]string
		if tc.field != "" {
			fields = [][2]string{{"title", tc.field}}
		}
		res, b := e.upload("/api/courses/"+c.ID+"/lectures", fields, tc.name, []byte("fake media"))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("%q: %d %s", tc.name, res.StatusCode, b)
		}
		var l Lecture
		decodeInto(t, b, &l)
		if l.Title != tc.title || l.TitleSource != tc.sourc || l.OriginalFilename != media.StripBidi(tc.name) || !noBidi(l.OriginalFilename) {
			t.Errorf("%q (title field %q): title %q source %s original %q, want %q %s", tc.name, tc.field, l.Title, l.TitleSource, l.OriginalFilename, tc.title, tc.sourc)
		}
	}

	// PATCH: cleaned the same way; nothing left is an empty title
	l := e.draft(c.ID, "L")
	if code := e.callErr("PATCH", "/api/lectures/"+l.ID, map[string]any{"title": "\u202e \u200f"}, http.StatusBadRequest); !strings.HasPrefix(code, "lecture.title_empty") {
		t.Errorf("bidi-only title: %s", code)
	}
	var got Lecture
	e.call("PATCH", "/api/lectures/"+l.ID, map[string]any{"title": "  Stream \u202eCiphers ", "title_zh": "\u2067流密码\u2069 "}, http.StatusOK, &got)
	if got.Title != "Stream Ciphers" || got.TitleZh == nil || *got.TitleZh != "流密码" || got.TitleSource != model.TitleSourceUser {
		t.Errorf("patched title %q / %v (%s)", got.Title, got.TitleZh, got.TitleSource)
	}
	e.call("PATCH", "/api/lectures/"+l.ID, map[string]any{"title_zh": "\u202e"}, http.StatusOK, &got)
	if got.TitleZh != nil {
		t.Errorf("bidi-only title_zh kept: %q", *got.TitleZh)
	}

	// by local path (desktop; no fixture call after this)
	e.desktopOn()
	res, b := e.pathUpload("/api/courses/"+c.ID+"/lectures", map[string]any{"path": writeSrc(t, t.TempDir(), ".mp4", []byte("fake media"))})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("path .mp4: %d %s", res.StatusCode, b)
	}
	var byPath Lecture
	decodeInto(t, b, &byPath)
	if byPath.Title != defaultLectureTitle {
		t.Errorf("path .mp4: title %q", byPath.Title)
	}
}

// A download's title (the extension's) is cleaned the same way; without
// one the file name gives it.
func TestImportDownloadTitleEdges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	for _, tc := range []struct{ title, filename, want string }{
		{" Graphs \u202etxt.exe ", "Graphs.mp4", "Graphs txt.exe"},
		{"\u202e", "Graphs \u2066x.mp4", "Graphs x"},
		{"", ".mp4", defaultLectureTitle},
	} {
		res, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{CourseID: c.ID, Path: writeDownload(t, e, "media bytes"),
			Filename: tc.filename, Title: tc.title})
		if err != nil || res.LectureID == "" {
			t.Fatalf("%q: %+v %v", tc.title, res, err)
		}
		l, err := e.st.GetLecture(ctx, res.LectureID)
		if err != nil {
			t.Fatal(err)
		}
		if l.Title != tc.want || !noBidi(l.Title) {
			t.Errorf("%q / %q: title %q, want %q", tc.title, tc.filename, l.Title, tc.want)
		}
		if !noBidi(l.OriginalFilename) {
			t.Errorf("%q: original_filename %q keeps a bidi control", tc.filename, l.OriginalFilename)
		}
	}
}

// test-media I8: a stored title is cut to maxLectureTitleRunes characters
// on a character boundary, kept on one line, and keeps no white space or
// dangling joiner from the cut; titles within the limit are unchanged.
func TestCleanLectureTitleLimits(t *testing.T) {
	long := strings.Repeat("密码学", 100) // 300 characters, 900 bytes
	if got := cleanLectureTitle(long); utf8.RuneCountInString(got) != maxLectureTitleRunes || got != string([]rune(long)[:maxLectureTitleRunes]) {
		t.Errorf("long title: %d characters %q", utf8.RuneCountInString(got), got)
	}
	for _, tc := range []struct{ in, want string }{
		{"Week 1", "Week 1"},
		{"\tWeek 1\n", "Week 1"},
		{"Week 1\nStream Ciphers", "Week 1 Stream Ciphers"},
		{"Week 1\u2028Hashing\r\n", "Week 1 Hashing"},
		{"a\u202e\u2066b\u2069\u200f", "ab"},
		{"مرحبا Week 2", "مرحبا Week 2"}, // right-to-left text itself stays
		{strings.Repeat("x", maxLectureTitleRunes-1) + " y", strings.Repeat("x", maxLectureTitleRunes-1)},
		{strings.Repeat("x", maxLectureTitleRunes-1) + "\u200d\U0001F469", strings.Repeat("x", maxLectureTitleRunes-1)},
		{strings.Repeat("é", maxLectureTitleRunes), strings.Repeat("é", maxLectureTitleRunes)},
	} {
		if got := cleanLectureTitle(tc.in); got != tc.want {
			t.Errorf("cleanLectureTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, s := range []string{long, "x\u202e" + long} {
		if got := cleanLectureTitle(s); !utf8.ValidString(got) || !noBidi(got) {
			t.Errorf("invalid result %q", got)
		}
	}

	// through the API: an upload's long file name and a PATCH
	e := newEnv(t)
	c := e.course("C")
	res, b := e.upload("/api/courses/"+c.ID+"/lectures", nil, strings.Repeat("Lecture ", 40)+".mp4", []byte("fake media"))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", res.StatusCode, b)
	}
	var l Lecture
	decodeInto(t, b, &l)
	if n := utf8.RuneCountInString(l.Title); n > maxLectureTitleRunes || strings.HasSuffix(l.Title, " ") {
		t.Errorf("uploaded title %d characters %q", n, l.Title)
	}
	var got Lecture
	e.call("PATCH", "/api/lectures/"+l.ID, map[string]any{"title": " Stream\nCiphers " + long}, http.StatusOK, &got)
	if n := utf8.RuneCountInString(got.Title); n != maxLectureTitleRunes || !strings.HasPrefix(got.Title, "Stream Ciphers 密码学") {
		t.Errorf("patched title %d characters %q", n, got.Title)
	}
}
