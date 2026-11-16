// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// SPEC-v3 V1.4: desktop-mode uploads by local path (upload_path.go).

// grant registers paths as the desktop main process does when the system
// dialog or a drop hands them to the page (pathgrant.go, T-SEC-08).
func (e *env) grant(paths ...string) { e.srv.grants.add(paths) }

// pathUpload posts a JSON path upload with the desktop token. Like the
// page, it sends a path the user picked: the main process's grant for it is
// registered first (ungrantedUpload sends one without).
func (e *env) pathUpload(path string, body map[string]any) (*http.Response, []byte) {
	e.t.Helper()
	if p, ok := body["path"].(string); ok && p != "" {
		e.grant(p)
	}
	return e.ungrantedUpload(path, body)
}

// ungrantedUpload posts a JSON path upload with the desktop token and no grant.
func (e *env) ungrantedUpload(path string, body map[string]any) (*http.Response, []byte) {
	e.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.doWith("POST", path, b, testToken, "")
}

// tokenUpload is a multipart upload carrying the desktop token.
func (e *env) tokenUpload(method, path string, fields [][2]string, filename string, content []byte) (*http.Response, []byte) {
	e.t.Helper()
	body, ctype := multipartBody(e.t, fields, filename, content)
	req, err := http.NewRequest(method, e.ts.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set(TokenHeader, testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// desktopOn switches an env to desktop mode once its fixtures exist (the
// fixture helpers send no token); the server reads s.desktop per request.
func (e *env) desktopOn() {
	e.t.Helper()
	u, err := url.Parse(e.ts.URL)
	if err != nil {
		e.t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv.desktop = &DesktopOptions{Token: testToken, Port: port}
}

func writeSrc(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func decodeInto(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %T: %v; body %s", v, err, b)
	}
}

// noTmpFiles fails when a .tmp file is left anywhere under dir.
func noTmpFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".tmp") {
			t.Errorf("temporary file left behind: %s", p)
		}
		return nil
	})
}

func TestPathUploadLectureMatchesHTTP(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	e.desktopOn()
	content := bytes.Repeat([]byte("0123456789abcdef"), 200000) // 3.2 MB: several copy buffers
	src := writeSrc(t, t.TempDir(), "Week 1 Intro.MP4", content)

	res, b := e.pathUpload("/api/courses/"+c.ID+"/lectures", map[string]any{"path": src, "title": ""})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("path upload: %d %s", res.StatusCode, b)
	}
	var byPath Lecture
	decodeInto(t, b, &byPath)

	res, b = e.tokenUpload("POST", "/api/courses/"+c.ID+"/lectures", [][2]string{{"title", ""}}, "Week 1 Intro.MP4", content)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("http upload: %d %s", res.StatusCode, b)
	}
	var byHTTP Lecture
	decodeInto(t, b, &byHTTP)

	if byPath.Title != "Week 1 Intro" || byPath.OriginalFilename != "Week 1 Intro.MP4" {
		t.Fatalf("path upload: title %q original %q", byPath.Title, byPath.OriginalFilename)
	}
	if byPath.Title != byHTTP.Title || byPath.OriginalFilename != byHTTP.OriginalFilename || byPath.Label != byHTTP.Label {
		t.Fatalf("path and http uploads differ: %+v / %+v", byPath, byHTTP)
	}
	lp, err := e.st.GetLecture(ctx, byPath.ID)
	if err != nil {
		t.Fatal(err)
	}
	lh, err := e.st.GetLecture(ctx, byHTTP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(lp.MediaPath) != "media.mp4" || filepath.Base(lh.MediaPath) != "media.mp4" || !filepath.IsAbs(lp.MediaPath) {
		t.Fatalf("media paths %q / %q", lp.MediaPath, lh.MediaPath)
	}
	if !e.srv.inDataDir(lp.MediaPath) {
		t.Fatalf("copy is not in the data dir: %s", lp.MediaPath)
	}
	got, err := os.ReadFile(lp.MediaPath)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("copied content differs (err %v, %d bytes)", err, len(got))
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("the source file must stay: %v", err)
	}
	noTmpFiles(t, e.cfg.DataDir)

	// A typed title is the user's, as with a form field.
	res, b = e.pathUpload("/api/courses/"+c.ID+"/lectures", map[string]any{"path": src, "title": "Intro to things"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("path upload with title: %d %s", res.StatusCode, b)
	}
	var titled Lecture
	decodeInto(t, b, &titled)
	if titled.Title != "Intro to things" {
		t.Fatalf("title %q", titled.Title)
	}

	// PUT /media of a draft by path.
	src2 := writeSrc(t, t.TempDir(), "Week 1 Intro (fixed).m4a", []byte("audio"))
	b, _ = json.Marshal(map[string]any{"path": src2})
	e.grant(src2)
	res, b = e.doWith("PUT", "/api/lectures/"+byPath.ID+"/media", b, testToken, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("replace media by path: %d %s", res.StatusCode, b)
	}
	lp, _ = e.st.GetLecture(ctx, byPath.ID)
	if filepath.Base(lp.MediaPath) != "media.m4a" || lp.OriginalFilename != "Week 1 Intro (fixed).m4a" {
		t.Fatalf("replaced media %q original %q", lp.MediaPath, lp.OriginalFilename)
	}
	if got, _ := os.ReadFile(lp.MediaPath); string(got) != "audio" {
		t.Fatalf("replaced content %q", got)
	}
}

func TestPathUploadMaterialAndSyllabus(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.draft(c.ID, "Week 1")
	e.desktopOn()
	srcDir := t.TempDir()
	base := "/api/courses/" + c.ID

	pdf := writeSrc(t, srcDir, "Slides 1.pdf", []byte("%PDF-1.4 not really"))
	res, b := e.pathUpload(base+"/materials", map[string]any{"path": pdf, "lecture_id": l.ID})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("material by path: %d %s", res.StatusCode, b)
	}
	var course Course
	decodeInto(t, b, &course)
	if len(course.Materials) != 1 || course.Materials[0].Filename != "Slides 1.pdf" || course.Materials[0].LectureID == nil || *course.Materials[0].LectureID != l.ID {
		t.Fatalf("materials %+v", course.Materials)
	}
	// The extension check of an HTTP upload applies.
	exe := writeSrc(t, srcDir, "tool.exe", []byte("MZ"))
	res, b = e.pathUpload(base+"/materials", map[string]any{"path": exe})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != "material.pdf_only" {
		t.Fatalf("exe as material: %d %s", res.StatusCode, b)
	}

	// syllabus/raw: a path is a file, {text} stays pasted text.
	txt := writeSrc(t, srcDir, "Syllabus.TXT", []byte("\xef\xbb\xbfWeek 1: graphs\nWeek 2: trees\n"))
	res, b = e.pathUpload(base+"/syllabus/raw", map[string]any{"path": txt})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("syllabus raw by path: %d %s", res.StatusCode, b)
	}
	var doc SyllabusDoc
	decodeInto(t, b, &doc)
	if doc.Filename != "Syllabus.TXT" || doc.Raw != "Week 1: graphs\nWeek 2: trees\n" {
		t.Fatalf("syllabus doc %+v", doc)
	}
	res, b = e.pathUpload(base+"/syllabus/raw", map[string]any{"text": "pasted words"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("pasted syllabus in desktop mode: %d %s", res.StatusCode, b)
	}
	decodeInto(t, b, &doc)
	if doc.Filename != "pasted.txt" || doc.Raw != "pasted words" {
		t.Fatalf("pasted doc %+v", doc)
	}
	// Legacy POST /syllabus by path.
	res, b = e.pathUpload(base+"/syllabus", map[string]any{"path": txt})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("syllabus by path: %d %s", res.StatusCode, b)
	}
	decodeInto(t, b, &course)
	if course.Syllabus == nil || course.Syllabus.Filename != "Syllabus.TXT" {
		t.Fatalf("syllabus %+v", course.Syllabus)
	}
	md := writeSrc(t, srcDir, "notes.docx", []byte("x"))
	res, b = e.pathUpload(base+"/syllabus/raw", map[string]any{"path": md})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != "syllabus.bad_type" {
		t.Fatalf("docx syllabus: %d %s", res.StatusCode, b)
	}
	noTmpFiles(t, e.cfg.DataDir)
}

func TestPathUploadRejectsBadPaths(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	e.desktopOn()
	ep := "/api/courses/" + c.ID + "/lectures"
	srcDir := t.TempDir()
	good := writeSrc(t, srcDir, "a.mp4", []byte("x"))
	if err := os.MkdirAll(e.cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inData := writeSrc(t, e.cfg.DataDir, "inside.mp4", []byte("x"))
	if err := os.Mkdir(filepath.Join(srcDir, "folder.mp4"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{"relative", map[string]any{"path": "a.mp4"}, 400, "upload.path_invalid"},
		{"dot relative", map[string]any{"path": "." + string(filepath.Separator) + "a.mp4"}, 400, "upload.path_invalid"},
		{"directory", map[string]any{"path": filepath.Join(srcDir, "folder.mp4")}, 400, "upload.path_not_file"},
		{"missing", map[string]any{"path": filepath.Join(srcDir, "gone.mp4")}, 404, "upload.path_not_found"},
		{"inside data dir", map[string]any{"path": inData}, 400, "upload.path_in_data_dir"},
		{"data dir itself", map[string]any{"path": e.cfg.DataDir}, 400, "upload.path_in_data_dir"},
		{"inside data dir, not clean", map[string]any{"path": filepath.Join(e.cfg.DataDir, "x", "..") + string(filepath.Separator) + "inside.mp4"}, 400, "upload.path_in_data_dir"},
		{"no path", map[string]any{"title": "x"}, 400, "upload.no_file"},
		{"empty path", map[string]any{"path": ""}, 400, "upload.no_file"},
		{"path not a string", map[string]any{"path": 3}, 400, "request.bad_param"},
		{"several paths", map[string]any{"paths": []string{good, good}}, 400, "upload.multiple_files"},
		{"long field", map[string]any{"path": good, "title": strings.Repeat("x", maxFieldBytes+1)}, 400, "upload.field_too_long"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			struct {
				name   string
				body   map[string]any
				status int
				code   string
			}{"device", map[string]any{"path": `\\.\NUL`}, 400, "upload.path_invalid"},
			struct {
				name   string
				body   map[string]any
				status int
				code   string
			}{"extended", map[string]any{"path": `\\?\` + good}, 400, "upload.path_invalid"},
			struct {
				name   string
				body   map[string]any
				status int
				code   string
			}{"reserved name", map[string]any{"path": filepath.Join(srcDir, "NUL")}, 400, "upload.path_not_file"},
		)
	} else {
		cases = append(cases, struct {
			name   string
			body   map[string]any
			status int
			code   string
		}{"device", map[string]any{"path": "/dev/null"}, 400, "upload.path_not_file"})
	}
	for _, tc := range cases {
		res, b := e.pathUpload(ep, tc.body)
		if res.StatusCode != tc.status || errCode(t, b) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, res.StatusCode, b, tc.status, tc.code)
		}
	}
	// Bad JSON.
	res, b := e.doWith("POST", ep, []byte("{not json"), testToken, "")
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != "request.bad_json" {
		t.Errorf("bad json: %d %s", res.StatusCode, b)
	}
	// A link from outside that points into the data directory is refused too.
	link := filepath.Join(srcDir, "link.mp4")
	if err := os.Symlink(inData, link); err == nil {
		res, b := e.pathUpload(ep, map[string]any{"path": link})
		if res.StatusCode != http.StatusBadRequest || errCode(t, b) != "upload.path_in_data_dir" {
			t.Errorf("link into data dir: %d %s", res.StatusCode, b)
		}
	} else {
		t.Logf("symlinks unavailable here (%v); link case not exercised", err)
	}
	var list []Lecture
	res, b = e.doWith("GET", "/api/courses/"+c.ID+"/lectures", nil, testToken, "")
	decodeInto(t, b, &list)
	if res.StatusCode != http.StatusOK || len(list) != 0 {
		t.Fatalf("rejected uploads created lectures: %d %s", res.StatusCode, b)
	}
	entries, _ := os.ReadDir(filepath.Join(e.cfg.DataDir, "courses", c.ID, "lectures"))
	if len(entries) != 0 {
		t.Fatalf("rejected uploads left directories: %v", entries)
	}
}

func TestPathUploadLimits(t *testing.T) {
	free := int64(100 << 30)
	e := newEnv(t, func(s *Server) {
		s.maxUpload = 1000
		s.freeSpace = func(string) (int64, error) { return free, nil }
	})
	c := e.course("C")
	e.desktopOn()
	ep := "/api/courses/" + c.ID + "/lectures"
	srcDir := t.TempDir()
	big := writeSrc(t, srcDir, "big.mp4", make([]byte, 5000))
	res, b := e.pathUpload(ep, map[string]any{"path": big})
	if res.StatusCode != http.StatusRequestEntityTooLarge || errCode(t, b) != "upload.too_large" {
		t.Fatalf("too large: %d %s", res.StatusCode, b)
	}
	// The check counts the file's size: 2 GB + 900 bytes free is not
	// enough for a 1000-byte file (SPEC §17.5).
	free = e.srv.minFree + 900
	small := writeSrc(t, srcDir, "small.mp4", make([]byte, 1000))
	res, b = e.pathUpload(ep, map[string]any{"path": small})
	if res.StatusCode != http.StatusInsufficientStorage || errCode(t, b) != "upload.no_space" {
		t.Fatalf("disk full: %d %s", res.StatusCode, b)
	}
	free = e.srv.minFree + 1000
	res, b = e.pathUpload(ep, map[string]any{"path": small})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("just enough space: %d %s", res.StatusCode, b)
	}
	noTmpFiles(t, e.cfg.DataDir)
}

func TestPathUploadWebModeRefusesJSON(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	src := writeSrc(t, t.TempDir(), "a.mp4", []byte("x"))
	if code := e.callErr("POST", "/api/courses/"+c.ID+"/lectures", map[string]any{"path": src}, http.StatusBadRequest); !strings.HasPrefix(code, "upload.not_multipart ") {
		t.Fatalf("web mode lecture by path: %s", code)
	}
	if code := e.callErr("POST", "/api/courses/"+c.ID+"/materials", map[string]any{"path": src}, http.StatusBadRequest); !strings.HasPrefix(code, "upload.not_multipart ") {
		t.Fatalf("web mode material by path: %s", code)
	}
	// syllabus/raw keeps reading JSON as {text}: a path alone is empty text.
	if code := e.callErr("POST", "/api/courses/"+c.ID+"/syllabus/raw", map[string]any{"path": src}, http.StatusBadRequest); !strings.HasPrefix(code, "syllabus.empty ") {
		t.Fatalf("web mode syllabus by path: %s", code)
	}
}

func TestPathUploadCancelRemovesTemp(t *testing.T) {
	e := newEnv(t)
	src := writeSrc(t, t.TempDir(), "long.mp4", make([]byte, 3*copyBufSize))
	dir := filepath.Join(e.cfg.DataDir, "cancel-test")

	// ctxReader stops at the first read after the cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	r := ctxReader{ctx, readerFunc(func(p []byte) (int, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return len(p), nil
	})}
	buf := make([]byte, 10)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(buf); err != nil { // cancels during this read
		t.Fatal(err)
	}
	if _, err := r.Read(buf); err == nil {
		t.Fatal("read after cancel succeeded")
	}

	// A copy whose request went away stops and leaves nothing behind.
	e.grant(src)
	_, err := e.srv.copyFromPath(ctx, pathRequest{path: src, fields: map[string]string{}}, dir, "media", nil)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("copy with a cancelled context: %v", err)
	}
	// the cancellation itself, not a refusal before the copy (SEC24 L4)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy with a cancelled context: %v, want context.Canceled", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("cancelled copy left files: %v", entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source removed: %v", err)
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestWeirdPathPrefix(t *testing.T) {
	if runtime.GOOS != "windows" {
		if weirdPathPrefix(`\\.\NUL`) {
			t.Fatal("only Windows has device prefixes")
		}
		return
	}
	for p, want := range map[string]bool{
		`\\server\share\lecture.mp4`: false, // UNC: allowed
		`C:\Users\x\a.mp4`:           false,
		`\\.\NUL`:                    true,
		`\\.\PhysicalDrive0`:         true,
		`//./COM1`:                   true,
		`\\?\C:\a.mp4`:               true,
		`\\?\UNC\server\share\a.mp4`: true,
		`\??\C:\a.mp4`:               true,
	} {
		if got := weirdPathPrefix(p); got != want {
			t.Errorf("weirdPathPrefix(%q) = %v, want %v", p, got, want)
		}
	}
}
