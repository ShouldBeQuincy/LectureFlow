// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/fetch"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
)

// Camera angles of a lecture (the popup's "双机位", lead 2026-10-27; the
// player's angle switch is a later task). A lecture's second angle is the
// file angle2.mp4 in its directory (fetch.AngleFile): a file convention,
// no table and no kv, so it goes with the directory (a deleted lecture, a
// moved data directory) and needs no migration. It is video only (the
// lecture's media keeps the audio). The browser extension's download
// brings it (ImportDownload); the desktop page can add one to an existing
// lecture.
//
//	GET    /api/lectures/{id}/angles/{name}  the file like GET .../media: Range → 206,
//	                                         video/mp4, behind the desktop token
//	POST   /api/lectures/{id}/angles         {"path"} (desktop mode, the checks of a
//	                                         path upload) or a multipart "file": its
//	                                         first video stream remuxed (-c copy, no
//	                                         audio) into angle2.mp4, replacing one
//	                                         that is there → 200 Lecture
//	DELETE /api/lectures/{id}/angles/{name}  → 200 Lecture
//
// Lecture.angles lists the angles there are ([{"name": "angle2"}]; left out
// when none).

// angleNames are the angles a lecture can have, in display order.
var angleNames = []string{fetch.AngleName}

// angleNameRe is what an angle's name may look like in a URL.
var angleNameRe = regexp.MustCompile(`^angle[2-9]$`)

func validAngleName(name string) bool { return angleNameRe.MatchString(name) }

func angleFile(dir, name string) string { return filepath.Join(dir, name+".mp4") }

// Codes of the angle endpoints (part of the API's catalog; init adds them).
const (
	codeAngleNotFound     = "angle.not_found"
	codeAngleNoVideo      = "angle.no_video"
	codeAngleRemuxFailed  = "angle.remux_failed"
	codeAngleWriteFailed  = "angle.write_failed"
	angleUploadBase       = ".angle-upload"
	angleReplaceRetryWait = 2 * time.Second
)

var angleCodes = map[string]errDef{
	codeAngleNotFound:    {"这节课没有第二视角（{name}）", []string{"name"}},
	codeAngleNoVideo:     {"这个文件里没有视频画面，不能作为第二视角", nil},
	codeAngleRemuxFailed: {"无法把这个文件保存为第二视角：只支持不重新编码就能放进 MP4 的视频（例如 H.264、HEVC 编码的 MP4、MOV、MKV）", nil},
	codeAngleWriteFailed: {"无法保存第二视角的文件。如果正在播放第二视角，请先停止播放再试", nil},
}

func init() {
	for code, def := range angleCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("angle code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/lectures/{id}/angles/{name}", s.getAngle)
		h("POST /api/lectures/{id}/angles", s.postAngle)
		h("DELETE /api/lectures/{id}/angles/{name}", s.deleteAngle)
	})
}

// lectureAngles is Lecture.angles: the angles whose file is there (nil =
// none, left out of the JSON).
func (s *Server) lectureAngles(l model.Lecture) []Angle {
	var out []Angle
	dir := s.lectureDir(l)
	for _, n := range angleNames {
		if fileExists(angleFile(dir, n)) {
			out = append(out, Angle{Name: n})
		}
	}
	return out
}

// angleLocks serialises the writes of one lecture's angles.
var angleLocks sync.Map // lecture id → *sync.Mutex

func lockAngles(lectureID string) func() {
	v, _ := angleLocks.LoadOrStore(lectureID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Server) getAngle(w http.ResponseWriter, r *http.Request) {
	l, err := s.getLectureRow(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := r.PathValue("name")
	if !validAngleName(name) || !serveMediaFile(w, r, angleFile(s.lectureDir(l), name), "video/mp4") {
		writeError(w, http.StatusNotFound, codeAngleNotFound, map[string]any{"name": name})
	}
}

func (s *Server) postAngle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err == nil {
		err = checkRecordIDs(l.CourseID, l.ID) // record_dir.go
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.checkSpace(r); err != nil {
		s.fail(w, r, err)
		return
	}
	dir := s.lectureDir(l)
	unlock := lockAngles(l.ID)
	defer unlock()
	src, name, cleanup, err := s.angleSource(w, r, dir)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer cleanup()
	// security review 2 M3: no playlist (upload_media.go)
	if err := s.checkMediaContent(ctx, src, name); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.remuxAngle(ctx, src, dir, fetch.AngleName); err != nil {
		s.fail(w, r, err)
		return
	}
	// the lecture may have been deleted meanwhile: leave nothing behind
	if _, err := s.st.GetLecture(ctx, l.ID); err != nil {
		os.Remove(angleFile(dir, fetch.AngleName))
		_ = os.Remove(dir) // only when empty
		s.fail(w, r, notFoundAs(err, "lecture.not_found"))
		return
	}
	s.hub.Changed("lecture", l.ID)
	s.hub.Changed("course", l.CourseID)
	s.respondLecture(w, r, l.ID, http.StatusOK)
}

// angleSource is the file a POST brings and the user's name of it: the
// user's file itself for a desktop path request (after the checks of a
// path upload: absolute, outside the data directory, a regular file, a
// media extension, the size limit, free space for a copy of it), else the
// multipart upload stored in the lecture's directory (removed by cleanup).
func (s *Server) angleSource(w http.ResponseWriter, r *http.Request, dir string) (string, string, func(), error) {
	none := func() {}
	if !s.nativeUpload(r) {
		up, err := s.receiveUpload(w, r, dir, angleUploadBase, acceptMediaName)
		if err != nil {
			return "", "", none, err
		}
		return up.path, up.filename, func() { os.Remove(up.path) }, nil
	}
	pr, err := readPathRequest(w, r)
	if err != nil {
		return "", "", none, err
	}
	resolved, info, err := s.checkSourcePath(r.Context(), pr.path)
	if err != nil {
		return "", "", none, err
	}
	name := filepath.Base(pr.path)
	if err := acceptMediaName(name); err != nil {
		return "", "", none, err
	}
	if info.Size() > s.maxUpload {
		return "", "", none, apiErr(http.StatusRequestEntityTooLarge, "upload.too_large", map[string]any{"max_bytes": s.maxUpload})
	}
	if free, err := s.freeSpace(s.dataDir); err == nil && free < s.minFree+info.Size() {
		return "", "", none, apiErr(http.StatusInsufficientStorage, "upload.no_space", map[string]any{"free_bytes": free, "keep_bytes": s.minFree})
	}
	return resolved, name, none, nil
}

// remuxAngle writes name.mp4 in dir from src with the bundled ffmpeg: the
// first video stream, copied (no re-encoding), no audio; through a .tmp
// file that replaces the angle's file only when complete. The input is
// read with the lecture media whitelists (media.InputArgs: no playlist
// demuxer, local files only).
func (s *Server) remuxAngle(ctx context.Context, src, dir, name string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	out := angleFile(dir, name)
	tmp := out + ".tmp"
	args := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, media.InputArgs()...)
	args = append(args, "-i", src, "-map", "0:v:0", "-c", "copy", "-an", "-f", "mp4", tmp)
	_, stderr, err := s.runCmd(ctx, s.cfg.FFmpeg, args...)
	if err != nil {
		os.Remove(tmp)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		detail := tailText(stderr, 2000)
		if detail == "" {
			detail = err.Error()
		}
		if strings.Contains(stderr, "matches no streams") {
			return apiErrDetail(http.StatusBadRequest, codeAngleNoVideo, nil, detail)
		}
		return apiErrDetail(http.StatusBadRequest, codeAngleRemuxFailed, nil, detail)
	}
	if err := replaceRetry(tmp, out); err != nil {
		os.Remove(tmp)
		return apiErrDetail(http.StatusConflict, codeAngleWriteFailed, nil, err.Error())
	}
	return nil
}

// replaceRetry renames from over to, retrying for a while: on Windows the
// file being replaced may be open for a moment (a player reading it).
func replaceRetry(from, to string) error {
	deadline := time.Now().Add(angleReplaceRetryWait)
	for {
		err := os.Rename(from, to)
		if err == nil || errors.Is(err, fs.ErrNotExist) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// removeRetry is replaceRetry for a removal.
func removeRetry(p string) error {
	deadline := time.Now().Add(angleReplaceRetryWait)
	for {
		err := os.Remove(p)
		if err == nil || errors.Is(err, fs.ErrNotExist) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// tailText is the end of s, at most n bytes, trimmed.
func tailText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = strings.ToValidUTF8(s[len(s)-n:], "")
	}
	return s
}

func (s *Server) deleteAngle(w http.ResponseWriter, r *http.Request) {
	l, err := s.getLectureRow(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := r.PathValue("name")
	p := angleFile(s.lectureDir(l), name)
	// record_dir.go: the file of this lecture's own directory only (not a
	// sibling's that differs only in case)
	if !validAngleName(name) || !s.removableRecordDir(s.lectureDir(l), l.CourseID, l.ID) || !fileExists(p) {
		writeError(w, http.StatusNotFound, codeAngleNotFound, map[string]any{"name": name})
		return
	}
	unlock := lockAngles(l.ID)
	err = removeRetry(p)
	unlock()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		writeErrorInfo(w, http.StatusConflict, model.ErrorInfo{Code: codeAngleWriteFailed, Detail: err.Error()})
		return
	}
	s.hub.Changed("lecture", l.ID)
	s.hub.Changed("course", l.CourseID)
	s.respondLecture(w, r, l.ID, http.StatusOK)
}
