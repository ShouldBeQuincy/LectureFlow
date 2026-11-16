// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/fetch"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/relocate"
)

// Security review 2, M3 (upload_media.go, import_paths.go): lecture media
// and second angles need a media extension and must not be a playlist; an
// imported data directory must not point at other computers.

// fakeProbe stands in for ffprobe (a call with -show_entries): it answers
// with the format of the input's first bytes ("CONCAT…" → refused by the
// demuxer whitelist, "JUNK…" → unreadable, anything else → mp4) and
// records the arguments. Other programs go to fakeCmd.
type fakeProbe struct {
	mu    sync.Mutex
	calls [][]string
}

func (f *fakeProbe) run(ctx context.Context, name string, args ...string) (string, string, error) {
	if !slices.Contains(args, "-show_entries") {
		return fakeCmd(ctx, name, args...)
	}
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	in := args[len(args)-1]
	b, err := os.ReadFile(in)
	if err != nil {
		return "", err.Error(), err
	}
	switch {
	case strings.HasPrefix(string(b), "CONCAT"):
		return "", "[concat @ 000001d2c3e48100] Format not on whitelist '" + media.FormatWhitelist + "'\n" + in + ": Invalid argument\n", errors.New("exit status 1")
	case strings.HasPrefix(string(b), "JUNK"):
		return "", in + ": Invalid data found when processing input\n", errors.New("exit status 1")
	}
	return "mov,mp4,m4a,3gp,3g2,mj2\n", "", nil
}

// withFakeFFprobe points the configured ffmpeg at a directory holding an
// (empty) ffmpeg and ffprobe, so that media.FFprobeFor finds ffprobe on
// any machine; it is never started (runCmd is the fake). It returns the
// ffprobe path.
func withFakeFFprobe(t *testing.T, e *env) string {
	t.Helper()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	dir := t.TempDir()
	for _, n := range []string{"ffmpeg", "ffprobe"} {
		if err := os.WriteFile(filepath.Join(dir, n+exe), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg.FFmpeg = filepath.Join(dir, "ffmpeg"+exe)
	return filepath.Join(dir, "ffprobe"+exe)
}

func errParamsOf(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var eb struct {
		Error model.ErrorInfo `json:"error"`
	}
	if err := json.Unmarshal(b, &eb); err != nil {
		t.Fatalf("error body %s: %v", b, err)
	}
	return eb.Error.Params
}

func TestUploadLectureMediaChecks(t *testing.T) {
	probe := &fakeProbe{}
	e := newEnv(t, func(s *Server) { s.runCmd = probe.run })
	ffprobe := withFakeFFprobe(t, e)
	c := e.course("C")
	courseDir := filepath.Join(e.cfg.DataDir, "courses", c.ID)
	refused := []struct {
		name, body, code string
		params           map[string]any
	}{
		{"list.m3u8", "#EXTM3U\n", codeUploadNotMedia, map[string]any{"name": "list.m3u8", "exts": media.ExtList()}},
		{"notes.txt", "hello", codeUploadNotMedia, nil},
		{"rec.mp4.m3u", "#EXTM3U\n", codeUploadNotMedia, nil},
		{"rec.MP4", "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\n\\\\server\\share\\x.ts\n", codeUploadPlaylist, map[string]any{"name": "rec.MP4", "kind": "m3u8"}},
		{"rec.mp3", "ffconcat version 1.0\nfile 'a.wav'\n", codeUploadPlaylist, map[string]any{"name": "rec.mp3", "kind": "ffconcat"}},
		{"rec.wav", "CONCAT script that only ffprobe recognises", codeUploadMediaFormat, map[string]any{"name": "rec.wav", "format": "concat"}},
		// test-media I6: an empty file and a web page are refused on import
		{"Week 5.mp4", "", codeUploadEmpty, map[string]any{"name": "Week 5.mp4"}},
		{"Week 6.mp4", "<!DOCTYPE html>\n<html><head><title>Sign in</title></head></html>\n", codeUploadHTML, map[string]any{"name": "Week 6.mp4"}},
		{"Week 7.m4a", "\xef\xbb\xbf\r\n<HTML>\n<body>403 Forbidden</body></HTML>", codeUploadHTML, map[string]any{"name": "Week 7.m4a"}},
	}
	for _, tc := range refused {
		res, b := e.upload("/api/courses/"+c.ID+"/lectures", nil, tc.name, []byte(tc.body))
		if res.StatusCode != http.StatusBadRequest || errCode(t, b) != tc.code {
			t.Errorf("%s: %d %s", tc.name, res.StatusCode, b)
			continue
		}
		if tc.params != nil {
			got := errParamsOf(t, b)
			for k, v := range tc.params {
				if got[k] != v {
					t.Errorf("%s: param %s = %v, want %v", tc.name, k, got[k], v)
				}
			}
		}
	}
	if ls, _ := e.st.ListLectures(context.Background(), c.ID); len(ls) != 0 {
		t.Fatalf("refused uploads created lectures: %+v", ls)
	}
	if entries, _ := os.ReadDir(filepath.Join(courseDir, "lectures")); len(entries) != 0 {
		t.Fatalf("refused uploads left directories: %v", entries)
	}
	// an extension check happens before anything is read: ffprobe ran only
	// for the files with a media extension that are no text playlist
	if len(probe.calls) != 1 {
		t.Fatalf("ffprobe calls %v", probe.calls)
	}
	// accepted: a recording, and a file ffprobe cannot read (prep reports it)
	for _, name := range []string{"Week 1.mp4", "Week 2.MKV"} {
		body := "fake media"
		if name == "Week 2.MKV" {
			body = "JUNK"
		}
		res, b := e.upload("/api/courses/"+c.ID+"/lectures", nil, name, []byte(body))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, res.StatusCode, b)
		}
	}
	// ffprobe reads the stored file with the whitelists, the file last
	call := probe.calls[1]
	if !strings.Contains(strings.Join(call, " "), strings.Join(media.InputArgs(), " ")) ||
		filepath.Base(call[len(call)-1]) != "media.mp4" || call[0] != ffprobe {
		t.Errorf("ffprobe call %v", call)
	}
	noTmpFiles(t, e.cfg.DataDir)
}

// A refused replacement leaves the draft's media as it was (same
// extension too); an accepted one replaces it and leaves no upload copy.
func TestReplaceMediaChecks(t *testing.T) {
	probe := &fakeProbe{}
	e := newEnv(t, func(s *Server) { s.runCmd = probe.run })
	withFakeFFprobe(t, e)
	c := e.course("C")
	l := e.draft(c.ID, "L")
	ctx := context.Background()
	before, _ := e.st.GetLecture(ctx, l.ID)
	put := func(name, body string) (*http.Response, []byte) {
		return e.tokenUpload("PUT", "/api/lectures/"+l.ID+"/media", nil, name, []byte(body))
	}
	for _, tc := range []struct{ name, body, code string }{
		{"rec.mp4", "#EXTM3U\n", codeUploadPlaylist},
		{"rec.mp4", "CONCAT", codeUploadMediaFormat},
		{"rec.m3u8", "#EXTM3U\n", codeUploadNotMedia},
		{"rec.mp4", "", codeUploadEmpty},
		{"rec.mp4", "<!doctype html><html>", codeUploadHTML},
	} {
		res, b := put(tc.name, tc.body)
		if res.StatusCode != http.StatusBadRequest || errCode(t, b) != tc.code {
			t.Errorf("%s %q: %d %s", tc.name, tc.body, res.StatusCode, b)
		}
		after, _ := e.st.GetLecture(ctx, l.ID)
		if after.MediaPath != before.MediaPath || after.OriginalFilename != before.OriginalFilename {
			t.Fatalf("a refused replacement changed the lecture: %+v", after)
		}
		if b, err := os.ReadFile(before.MediaPath); err != nil || string(b) != "fake media" {
			t.Fatalf("a refused replacement touched the media: %q %v", b, err)
		}
	}
	res, b := put("better.mp4", "better media")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("replace: %d %s", res.StatusCode, b)
	}
	after, _ := e.st.GetLecture(ctx, l.ID)
	if filepath.Base(after.MediaPath) != "media.mp4" || after.OriginalFilename != "better.mp4" {
		t.Fatalf("replaced lecture %+v", after)
	}
	if b, err := os.ReadFile(after.MediaPath); err != nil || string(b) != "better media" {
		t.Fatalf("replaced media %q %v", b, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(after.MediaPath))
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), mediaUploadBase) {
			t.Fatalf("upload copy left: %s", en.Name())
		}
	}
	noTmpFiles(t, e.cfg.DataDir)
}

// Desktop path uploads get the same checks (the user's file name).
func TestPathUploadMediaChecks(t *testing.T) {
	probe := &fakeProbe{}
	e := newEnv(t, func(s *Server) { s.runCmd = probe.run })
	withFakeFFprobe(t, e)
	c := e.course("C")
	e.desktopOn()
	src := t.TempDir()
	for _, tc := range []struct{ name, body, code string }{
		{"list.m3u8", "#EXTM3U\n", codeUploadNotMedia},
		{"rec.mp4", "#EXTM3U\n", codeUploadPlaylist},
	} {
		res, b := e.pathUpload("/api/courses/"+c.ID+"/lectures", map[string]any{"path": writeSrc(t, src, tc.name, []byte(tc.body))})
		if res.StatusCode != http.StatusBadRequest || errCode(t, b) != tc.code {
			t.Errorf("%s: %d %s", tc.name, res.StatusCode, b)
		}
	}
	if ls, _ := e.st.ListLectures(context.Background(), c.ID); len(ls) != 0 {
		t.Fatalf("lectures created: %+v", ls)
	}
	res, b := e.pathUpload("/api/courses/"+c.ID+"/lectures", map[string]any{"path": writeSrc(t, src, "ok.m4a", []byte("audio"))})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("accepted file: %d %s", res.StatusCode, b)
	}
}

// A finished download that is a playlist is not imported; the files go
// back to the task's directory.
func TestImportDownloadRefusesPlaylist(t *testing.T) {
	probe := &fakeProbe{}
	e := newEnv(t, func(s *Server) { s.runCmd = probe.run })
	withFakeFFprobe(t, e)
	ctx := context.Background()
	c := e.course("C")
	for _, body := range []string{"#EXTM3U\n#EXTINF:1,\n\\\\server\\share\\x.ts\n", "CONCAT"} {
		src := writeDownload(t, e, body)
		_, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{CourseID: c.ID, Path: src, Filename: "Lecture 1.mp4", Title: "Lecture 1"})
		// the download task shows this code (review R10 M1)
		var rf *fetch.ImportRefused
		if !errors.As(err, &rf) || (rf.Code != codeUploadPlaylist && rf.Code != codeUploadMediaFormat) {
			t.Fatalf("%q: err %v", body, err)
		}
		if b, err := os.ReadFile(src); err != nil || string(b) != body {
			t.Fatalf("not given back: %q %v", b, err)
		}
	}
	// a playlist as the second angle
	src := writeDownload(t, e, "media bytes")
	angleSrc := filepath.Join(filepath.Dir(src), fetch.AngleFile)
	os.WriteFile(angleSrc, []byte("#EXTM3U\n"), 0o644)
	if _, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{CourseID: c.ID, Path: src, Filename: "x.mp4", Title: "x",
		Angles: []fetch.ImportAngle{{Name: fetch.AngleName, Path: angleSrc}}}); err == nil {
		t.Fatal("a playlist angle was imported")
	} else if rf := (*fetch.ImportRefused)(nil); !errors.As(err, &rf) || rf.Code != codeUploadPlaylist {
		t.Fatalf("a playlist angle: err %v", err)
	}
	for _, p := range []string{src, angleSrc} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s not given back: %v", p, err)
		}
	}
	if ls, _ := e.st.ListLectures(ctx, c.ID); len(ls) != 0 {
		t.Fatalf("lectures created: %+v", ls)
	}
}

// test-media I6: a finished download that is empty or a sign-in page is
// refused with its own code (not media.corrupt later in prep), and the file
// goes back to the task's directory.
func TestImportDownloadRefusesEmptyAndHTML(t *testing.T) {
	probe := &fakeProbe{}
	e := newEnv(t, func(s *Server) { s.runCmd = probe.run })
	withFakeFFprobe(t, e)
	ctx := context.Background()
	c := e.course("C")
	for _, tc := range []struct{ body, code string }{
		{"", codeUploadEmpty},
		{"<!DOCTYPE html>\n<html lang=\"en\"><head><title>Log in</title>", codeUploadHTML},
	} {
		src := writeDownload(t, e, tc.body)
		_, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{CourseID: c.ID, Path: src, Filename: "Lecture 1.mp4", Title: "Lecture 1"})
		// the download task shows this code and the file name (review R10 M1)
		var rf *fetch.ImportRefused
		if !errors.As(err, &rf) || rf.Code != tc.code || rf.Params["name"] != "Lecture 1.mp4" || len(rf.Params) != 1 {
			t.Fatalf("%q: err %v", tc.body, err)
		}
		if b, err := os.ReadFile(src); err != nil || string(b) != tc.body {
			t.Fatalf("not given back: %q %v", b, err)
		}
	}
	if ls, _ := e.st.ListLectures(ctx, c.ID); len(ls) != 0 {
		t.Fatalf("lectures created: %+v", ls)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("ffprobe ran: %v", probe.calls)
	}
}

// addStoredPaths adds lectures (and a material) whose stored paths point
// at other computers to the data directory dir made by devDataDir.
func addStoredPaths(t *testing.T, dir string, lecturePaths []string, materialPath string) {
	t.Helper()
	d, err := db.OpenDB(filepath.Join(dir, relocate.DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i, p := range lecturePaths {
		if _, err := d.Exec("INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES (?, 'C1', 'W', ?, '2026-01-03T00:00:00Z')", "LX"+string(rune('A'+i)), p); err != nil {
			t.Fatal(err)
		}
	}
	if materialPath != "" {
		if _, err := d.Exec("INSERT INTO material (id, course_id, filename, path, created_at) VALUES ('M1', 'C1', 'slides.pdf', ?, '2026-01-03T00:00:00Z')", materialPath); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDataImportRefusesNetworkPaths(t *testing.T) {
	e := newDDEnv(t)
	src := devDataDir(t)
	addStoredPaths(t, src, []string{`\\attacker\share\a.mp4`, `//?/UNC/attacker/share/b.mp4`, filepath.Join(src, "courses", "C1", "lectures", "L2", "media.mp4")}, `\\attacker\share\slides.pdf`)
	e.pick(src)
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != codeImportNetworkPath {
		t.Fatalf("import: %d %s", res.StatusCode, b)
	}
	if p := errParamsOf(t, b); p["count"] != float64(3) || !strings.Contains(p["path"].(string), "attacker") {
		t.Fatalf("params %v", p)
	}
	if pathExists(filepath.Join(e.cfg.DataDir, relocate.StagingDir)) || e.restarts.Load() != 0 {
		t.Fatal("a refused import was staged")
	}
	// the run is free again
	if st := e.status(); st.State != ddIdle {
		t.Fatalf("status %+v", st)
	}
}

// The staged copy is checked again: paths that appeared after the first
// check (the source changed during the copy) fail the run and remove the
// staging area; nothing is applied on the next start.
func TestDataImportStagedNetworkPaths(t *testing.T) {
	e := newDDEnv(t)
	src := devDataDir(t)
	e.srv.dataDirRun().stage = func(ctx context.Context, o relocate.ImportOptions) error {
		if err := relocate.StageImport(ctx, o); err != nil {
			return err
		}
		d, err := db.OpenDB(filepath.Join(o.DataDir, relocate.StagingDir, relocate.DBName))
		if err != nil {
			return err
		}
		defer d.Close()
		_, err = d.Exec(`UPDATE lecture SET media_path = '\\attacker\share\x.mp4'`)
		return err
	}
	e.pick(src)
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	e.waitRun()
	st := e.status()
	if st.State != ddFailed || st.Error == nil || st.Error.Code != codeImportNetworkPath || e.restarts.Load() != 0 {
		t.Fatalf("status %+v restarts %d", st, e.restarts.Load())
	}
	if pathExists(filepath.Join(e.cfg.DataDir, relocate.StagingDir)) {
		t.Fatal("the staging area is still there")
	}
}

// With the real ffprobe (the bundled build when the desktop build staged
// it, else PATH; skipped without one, SPEC §14): a real recording passes
// the checks, an ffconcat script named .wav does not.
func TestUploadMediaRealFFprobe(t *testing.T) {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	ffmpeg, _ := filepath.Abs(filepath.Join("..", "..", "desktop", "build", "resources", "bin", "ffmpeg"+exe))
	if _, err := os.Stat(ffmpeg); err != nil {
		if ffmpeg, err = exec.LookPath("ffmpeg"); err != nil {
			t.Skip("ffmpeg not found; the real ffprobe upload check is skipped (SPEC §14)")
		}
	}
	if _, err := media.FFprobeFor(ffmpeg); err != nil {
		t.Skipf("ffprobe not found (%v); skipped (SPEC §14)", err)
	}
	e := newEnv(t, func(s *Server) { s.runCmd = runCommand })
	e.cfg.FFmpeg = ffmpeg
	c := e.course("C")
	wav := filepath.Join(t.TempDir(), "rec.wav")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "sine=duration=1", wav).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	b, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	if res, body := e.upload("/api/courses/"+c.ID+"/lectures", nil, "rec.wav", b); res.StatusCode != http.StatusCreated {
		t.Fatalf("real wav: %d %s", res.StatusCode, body)
	}
	res, body := e.upload("/api/courses/"+c.ID+"/lectures", nil, "rec2.wav", []byte("ffconcat version 1.0\nfile 'media.wav'\n"))
	if res.StatusCode != http.StatusBadRequest || errCode(t, body) != codeUploadPlaylist {
		t.Fatalf("ffconcat as .wav: %d %s", res.StatusCode, body)
	}
}
