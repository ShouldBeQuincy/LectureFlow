// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/fetch"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
)

// Importing a local file into the data directory: the desktop page's path
// upload (SPEC-v3 V1.4, copyFromPath) and a finished download of the
// browser extension (.claude/ext-brief.md §6.2, ImportDownload) share the
// checks and the copy.

// importSource is a local file to bring into the data directory.
type importSource struct {
	// path is the file: a path the desktop page sent, or the app's own file.
	path string
	// own marks the app's own file inside the data directory (a finished
	// download): it is moved — renamed on the same volume, else copied and
	// removed — and the checks of a user's path (outside the data
	// directory, the upload size limit of SPEC §17.4, which is about
	// streaming uploads) do not apply.
	own bool
	// name is the file name for accept and original_filename ("" = the
	// path's base name).
	name string
}

// importLocal brings src to dir/base+ext (ext from the file name, lower
// case) after the checks of an HTTP upload: file name (accept), size limit
// (§17.4), free space (§17.5), a regular file. A copy goes through a .tmp
// file renamed on success; a cancelled ctx stops it within one buffer; no
// file is left behind on any error.
func (s *Server) importLocal(ctx context.Context, src importSource, dir, base string, accept func(name string) error) (uploadResult, error) {
	var res uploadResult
	var resolved string
	var info os.FileInfo
	var err error
	if src.own {
		resolved = src.path
		if info, err = os.Lstat(resolved); err != nil {
			return res, sourceOpenErr(src.path, err)
		}
		if !info.Mode().IsRegular() {
			return res, apiErr(http.StatusBadRequest, "upload.path_not_file", map[string]any{"path": src.path})
		}
	} else if resolved, info, err = s.checkSourcePath(ctx, src.path); err != nil {
		return res, err
	}
	name := src.name
	if name == "" {
		name = filepath.Base(src.path)
	}
	if accept != nil {
		if err := accept(name); err != nil {
			return res, err
		}
	}
	if !src.own && info.Size() > s.maxUpload {
		return res, apiErr(http.StatusRequestEntityTooLarge, "upload.too_large", map[string]any{"max_bytes": s.maxUpload})
	}
	res.filename = name
	res.path = filepath.Join(dir, base+strings.ToLower(filepath.Ext(name)))
	if src.own {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return res, err
		}
		// the same volume (the downloads live in the data directory): a rename
		if err := os.Rename(resolved, res.path); err == nil {
			res.size = info.Size()
			return res, nil
		}
	}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return res, err
	}
	if free, err := s.freeSpace(s.dataDir); err == nil && free < s.minFree+info.Size() {
		return res, apiErr(http.StatusInsufficientStorage, "upload.no_space", map[string]any{"free_bytes": free, "keep_bytes": s.minFree})
	}
	f, err := os.Open(resolved)
	if err != nil {
		return res, sourceOpenErr(src.path, err)
	}
	defer f.Close()
	// The file may have been swapped since the checks: check what was opened.
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return res, apiErr(http.StatusBadRequest, "upload.path_not_file", map[string]any{"path": src.path})
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	tmp := res.path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return res, err
	}
	limit := s.maxUpload
	if src.own {
		limit = info.Size()
	}
	// writerOnly hides os.File.ReadFrom so that the copy goes through the
	// buffer and the context check below.
	n, err := io.CopyBuffer(writerOnly{out}, ctxReader{ctx, io.LimitReader(f, limit+1)}, make([]byte, copyBufSize))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && !src.own && n > s.maxUpload {
		err = apiErr(http.StatusRequestEntityTooLarge, "upload.too_large", map[string]any{"max_bytes": s.maxUpload})
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = os.Rename(tmp, res.path)
	}
	if err != nil {
		os.Remove(tmp)
		var he *httpError
		if errors.As(err, &he) || ctx.Err() != nil {
			return res, err
		}
		return res, apiErrDetail(http.StatusInternalServerError, "upload.copy_failed", map[string]any{"path": src.path}, err.Error())
	}
	if src.own {
		f.Close()
		os.Remove(resolved)
	}
	res.size = n
	return res, nil
}

// ImportDownload creates the lecture of a finished download (fetch.ImportFunc,
// wired by cmd): the file (and the second camera angle's, req.Angles) is
// moved into a new lecture directory and the
// lecture is a draft like an upload's (title_source filename,
// original_filename the platform's title made safe plus the extension),
// with the extension's recording time as the user's (recorded_at_source
// user: the user saw it in the popup and confirmed; the file-name-rule
// task is not requested, a synthetic file name would mislead it). Then it
// is launched; a refused launch (no transcription set up, say) leaves the
// draft and is reported in ImportResult.LaunchError, not as an error.
func (s *Server) ImportDownload(ctx context.Context, req fetch.ImportRequest) (fetch.ImportResult, error) {
	var out fetch.ImportResult
	c, err := s.courseOr404(ctx, req.CourseID)
	if err != nil {
		return out, err
	}
	if err := checkRecordIDs(c.ID); err != nil { // record_dir.go: the file is not moved
		return out, err
	}
	l := model.Lecture{ID: db.NewID(), CourseID: c.ID, CreatedAt: db.Now(), TitleSource: model.TitleSourceFilename}
	dir := s.lectureDir(l)
	up, err := s.importLocal(ctx, importSource{path: req.Path, own: true, name: downloadMediaName(req.Filename)}, dir, "media", nil)
	if err != nil {
		os.RemoveAll(dir)
		return out, err
	}
	// giveBack returns the files to the task's directory, so that a retry
	// need not download them again
	moved := [][2]string{{up.path, req.Path}}
	giveBack := func() {
		for _, m := range moved {
			_ = os.Rename(m[0], m[1])
		}
		os.RemoveAll(dir)
	}
	// security review 2 M3 (upload_media.go): a downloaded "recording" that
	// is a playlist would make prep's ffmpeg read other files. The file
	// name is the platform's title plus a media extension
	// (downloadMediaName), so what decides is the content.
	if err := s.checkMediaContent(ctx, up.path, up.filename); err != nil {
		giveBack()
		return out, importRefused(err)
	}
	// the second camera angle (handlers_angles.go): next to the media
	for _, a := range req.Angles {
		if !validAngleName(a.Name) {
			giveBack()
			return out, fmt.Errorf("download import: invalid angle name %q", a.Name)
		}
		au, err := s.importLocal(ctx, importSource{path: a.Path, own: true, name: a.Name + ".mp4"}, dir, a.Name, nil)
		if err != nil {
			giveBack()
			return out, err
		}
		moved = append(moved, [2]string{au.path, a.Path})
		if err := s.checkMediaContent(ctx, au.path, au.filename); err != nil {
			giveBack()
			return out, importRefused(err)
		}
	}
	l.Title = cleanLectureTitle(req.Title)
	if l.Title == "" {
		l.Title = titleFromFilename(up.filename)
	}
	l.OriginalFilename = media.StripBidi(up.filename) // shown: no bidi controls (test-media I8)
	l.MediaPath, _ = filepath.Abs(up.path)
	if req.RecordedAt != nil {
		if t, err := calendar.ParseRecordedAt(*req.RecordedAt, time.Local); err == nil {
			v := calendar.FormatRecordedAt(t)
			l.RecordedAt, l.RecordedAtSource = &v, model.RecordedAtUser
		}
	}
	if err := s.st.CreateLecture(ctx, l); err != nil {
		giveBack()
		return out, err
	}
	if parseRule(c.FilenameRule) != nil {
		// as for an upload: the rule may give a kind; a user's recording
		// time is kept
		if err := stages.ResolveRecordedAt(ctx, s.stageDeps(), l.ID, nil); err != nil && ctx.Err() == nil {
			s.logWarn("resolve recorded_at of a download", l.ID, err)
		}
	}
	if l.RecordedAt != nil {
		s.refreshSeq(ctx, c.ID)
		s.materialSetsChanged(c.ID)
	}
	s.hub.Changed("course", c.ID)
	out.LectureID = l.ID
	if s.sc == nil {
		out.LaunchError = &model.ErrorInfo{Code: "server.unavailable", Params: map[string]any{"service": "scheduler"}}
		return out, nil
	}
	if err := s.sc.Launch(ctx, l.ID); err != nil {
		_, info := classify(ctx, err, "", nil)
		if info.Code == "" {
			info = model.ErrorInfo{Code: "server.internal", Detail: err.Error()}
		}
		out.LaunchError = &info
	}
	return out, nil
}

// downloadMediaName is the file name of a finished download as the lecture
// keeps it (original_filename, and the extension of media{ext} in the
// lecture's directory): an upload's name must have a media extension
// (acceptMediaName), and so does this one (AUD3 low-3). The queue names a
// file track by its URL's extension only when it is one (internal/fetch
// fileTrackExt), so this is the second line, for a file of an older
// version's naming say: another extension (.exe, .html, .part) is replaced
// by the neutral .mp4, a name without one gets it. The content was probed
// (checkMediaContent) and ffmpeg reads the file by what it is, not by its
// name.
func downloadMediaName(name string) string {
	if media.AllowedName(name) {
		return name
	}
	if ext := filepath.Ext(name); downloadExt.MatchString(ext) {
		name = strings.TrimSuffix(name, ext)
	}
	return name + ".mp4"
}

// downloadExt is what downloadMediaName takes for an extension to replace
// (the queue's extOf: a dot and one to five letters or digits); "Lecture
// 3.5 notes" has none.
var downloadExt = regexp.MustCompile(`^\.[A-Za-z0-9]{1,5}$`)

// importRefused turns checkMediaContent's refusal of a downloaded file (an
// upload.media_* httpError: empty, a web page, a playlist, another format)
// into fetch.ImportRefused, so that the download task shows that reason
// instead of downloads.import_failed (review R10 M1). Other errors (ffprobe
// could not run, say) are returned as they are.
func importRefused(err error) error {
	var he *httpError
	if errors.As(err, &he) && fetch.RefusedFile(he.info.Code) {
		return &fetch.ImportRefused{Code: he.info.Code, Params: he.info.Params}
	}
	return err
}
