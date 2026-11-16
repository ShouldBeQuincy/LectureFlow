// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
	"lectureflow/internal/wav"
)

// Codes for state.ErrBusy of the review and delete operations.
const (
	reviewBusy = "review.busy"
	deleteBusy = "lecture.running"
)

func (s *Server) getLectureRow(ctx context.Context, id string) (model.Lecture, error) {
	l, err := s.st.GetLecture(ctx, id)
	return l, notFoundAs(err, "lecture.not_found")
}

func (s *Server) respondLecture(w http.ResponseWriter, r *http.Request, id string, status int) {
	l, err := s.getLectureRow(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.lectureView(r.Context(), l)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if wantUsageTotal(r) {
		v.UsageTotal = s.usageTotal(r.Context(), l.ID) // GET ?usage_total=1 only (lecture_usage.go)
	}
	writeJSON(w, status, v)
}

func (s *Server) listLectures(w http.ResponseWriter, r *http.Request) {
	c, err := s.courseOr404(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.lectureViews(r.Context(), c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getLecture(w http.ResponseWriter, r *http.Request) {
	s.respondLecture(w, r, r.PathValue("id"), http.StatusOK)
}

// POST /api/courses/{id}/lectures, multipart: title, file (SPEC §9.4,
// §17.4). v1.1 A3: the new lecture is a draft (launched_at NULL); nothing
// runs until it is launched. v2 P13.3: the uploaded file's name is kept as
// original_filename; with a course file-name rule the recording time (and a
// title group) is resolved at once (ruling 3), and the scheduler is told so
// it can generate a rule for names no rule matches yet (ruling 2, debounced
// over a batch of uploads). A title typed in the form (different from the
// file name) is the user's (title_source user: never replaced by P13.5).
func (s *Server) uploadLecture(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	courseID := r.PathValue("id")
	c, err := s.courseOr404(ctx, courseID)
	if err == nil {
		err = checkRecordIDs(c.ID) // record_dir.go
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.checkSpace(r); err != nil {
		s.fail(w, r, err)
		return
	}
	l := model.Lecture{ID: db.NewID(), CourseID: courseID, CreatedAt: db.Now()}
	dir := s.lectureDir(l)
	// security review 2 M3: a media extension, and no playlist (upload_media.go)
	up, err := s.receiveUpload(w, r, dir, "media", acceptMediaName)
	if err == nil {
		err = s.checkMediaContent(ctx, up.path, up.filename)
	}
	if err != nil {
		os.RemoveAll(dir)
		s.fail(w, r, err)
		return
	}
	stem := titleFromFilename(up.filename)
	l.Title, l.TitleSource = cleanLectureTitle(up.fields["title"]), model.TitleSourceFilename
	switch {
	case l.Title == "":
		l.Title = stem
	case l.Title != stem:
		l.TitleSource = model.TitleSourceUser
	}
	// shown as the file's name: without bidi controls (test-media I8, R10 S7)
	l.OriginalFilename = media.StripBidi(up.filename)
	// Absolute, so the path works however a stage resolves it (state.MediaExists).
	l.MediaPath, _ = filepath.Abs(up.path)
	if err := s.st.CreateLecture(ctx, l); err != nil {
		os.RemoveAll(dir)
		s.fail(w, r, err)
		return
	}
	if parseRule(c.FilenameRule) != nil {
		// A failure leaves recorded_at unset ("设置录制时间"); the upload stands.
		if err := stages.ResolveRecordedAt(ctx, s.stageDeps(), l.ID, nil); err != nil && ctx.Err() == nil {
			s.logWarn("resolve recorded_at on upload", l.ID, err)
		}
	}
	if s.sc != nil {
		s.sc.RequestFilenameRule(courseID)
	}
	s.hub.Changed("course", courseID)
	s.respondLecture(w, r, l.ID, http.StatusCreated)
}

// PATCH /api/lectures/{id} LecturePatch (v2 P13): title / title_zh (the
// title becomes the user's: title_source user) and recorded_at (the user's;
// "" goes back to the automatic value, P13.3.4). seq is computed from the
// recording times since v2 and cannot be set (400). A recorded_at change
// that renumbers lectures of a course whose citations were exported answers
// 409 RenumberConflict unless confirmed (P13.6).
func (s *Server) patchLecture(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req LecturePatch
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Seq != nil {
		writeError(w, http.StatusBadRequest, "lecture.seq_readonly", nil)
		return
	}
	if req.Title == nil && req.TitleZh == nil && req.RecordedAt == nil {
		writeError(w, http.StatusBadRequest, "request.nothing_to_change", map[string]any{"fields": "title, title_zh, recorded_at"})
		return
	}
	if req.Title != nil && cleanLectureTitle(*req.Title) == "" {
		writeError(w, http.StatusBadRequest, "lecture.title_empty", nil)
		return
	}
	var rec *string // the user's recording time; nil with RecordedAt "" = clear
	if req.RecordedAt != nil && strings.TrimSpace(*req.RecordedAt) != "" {
		t, err := calendar.ParseRecordedAt(*req.RecordedAt, time.Local)
		if err != nil {
			writeError(w, http.StatusBadRequest, "lecture.recorded_at_invalid", map[string]any{"value": *req.RecordedAt})
			return
		}
		v := calendar.FormatRecordedAt(t)
		rec = &v
	}
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.RecordedAt != nil {
		c, err := s.st.GetCourse(ctx, l.CourseID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		lecs, err := s.st.ListLectures(ctx, l.CourseID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		after := make([]model.Lecture, len(lecs))
		for i, o := range lecs {
			if o.ID == l.ID {
				if rec != nil {
					o.RecordedAt, o.RecordedAtSource = rec, model.RecordedAtUser
				} else {
					o.RecordedAt, o.RecordedAtSource = s.predictRecordedAt(ctx, c, o)
				}
			}
			after[i] = o
		}
		if err := s.checkRenumber(ctx, c, lecs, c, after, req.Confirm); err != nil {
			s.failRenumber(w, r, err)
			return
		}
	}
	err = s.st.Tx(ctx, func(tx db.Store) error {
		if req.Title != nil || req.TitleZh != nil {
			title, zh := l.Title, l.TitleZh
			if req.Title != nil {
				title = cleanLectureTitle(*req.Title)
			}
			if req.TitleZh != nil {
				z := cleanLectureTitle(*req.TitleZh)
				zh = strOrNil(&z)
			}
			if err := tx.SetLectureTitles(ctx, l.ID, title, zh, model.TitleSourceUser); err != nil {
				return err
			}
		}
		if req.RecordedAt != nil {
			src := model.RecordedAtUser
			if rec == nil {
				src = model.RecordedAtNone
			}
			return tx.SetLectureRecordedAt(ctx, l.ID, rec, src)
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, notFoundAs(err, "lecture.not_found"))
		return
	}
	if req.RecordedAt != nil {
		if rec == nil {
			// Back to automatic: the file-name rule, else the media's creation
			// time (resolve refreshes seq when the value changes).
			if err := stages.ResolveRecordedAt(ctx, s.stageDeps(), l.ID, nil); err != nil && ctx.Err() == nil {
				s.logWarn("resolve recorded_at", l.ID, err)
			}
		}
		s.refreshSeq(ctx, l.CourseID)
		// the teaching week may have changed, and with it the slides bound
		// to a week that the lecture uses (lead freeze 1.5)
		s.materialSetsChanged(l.CourseID)
	}
	s.hub.Changed("lecture", l.ID)
	s.hub.Changed("course", l.CourseID)
	s.respondLecture(w, r, l.ID, http.StatusOK)
}

// PUT /api/lectures/{id}/media, multipart file: replace the media of a
// draft (v1.1 A3.1). Streamed like an upload (SPEC §17.4); the old file is
// removed afterwards when it lies inside data_dir. v2 P13.3: the new file's
// name becomes original_filename and the recording time is resolved again
// (a user-set one is kept).
func (s *Server) replaceMedia(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !state.IsDraft(l) {
		writeError(w, http.StatusConflict, "lecture.not_draft", nil)
		return
	}
	if err := checkRecordIDs(l.CourseID, l.ID); err != nil { // record_dir.go
		s.fail(w, r, err)
		return
	}
	if err := s.checkRecordPath(s.lectureDir(l)); err != nil { // record_dir.go: no link on the way
		s.fail(w, r, err)
		return
	}
	if err := s.checkSpace(r); err != nil {
		s.fail(w, r, err)
		return
	}
	// security review 2 M3 (upload_media.go): the file arrives under a
	// separate name and replaces media{ext} only once checked, so a refused
	// file never overwrites the current media of the same extension.
	dir := s.lectureDir(l)
	up, err := s.receiveUpload(w, r, dir, mediaUploadBase, acceptMediaName)
	if err == nil {
		if err = s.checkMediaContent(ctx, up.path, up.filename); err != nil {
			os.Remove(up.path)
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	final := filepath.Join(dir, "media"+filepath.Ext(up.path))
	if err := replaceRetry(up.path, final); err != nil {
		os.Remove(up.path)
		s.fail(w, r, err)
		return
	}
	newPath, _ := filepath.Abs(final)
	err = s.st.Tx(ctx, func(tx db.Store) error {
		cur, err := tx.GetLecture(ctx, l.ID)
		if err != nil {
			return err
		}
		if !state.IsDraft(cur) {
			return apiErr(http.StatusConflict, "lecture.not_draft", nil)
		}
		if err := tx.SetLectureOriginalFilename(ctx, l.ID, media.StripBidi(up.filename)); err != nil {
			return err
		}
		return tx.SetLectureMedia(ctx, l.ID, newPath)
	})
	if err != nil {
		if !samePath(newPath, l.MediaPath) {
			os.Remove(newPath)
		}
		s.fail(w, r, notFoundAs(err, "lecture.not_found"))
		return
	}
	if err := stages.ResolveRecordedAt(ctx, s.stageDeps(), l.ID, nil); err != nil && ctx.Err() == nil {
		s.logWarn("resolve recorded_at after a media replacement", l.ID, err)
	}
	if s.sc != nil {
		s.sc.RequestFilenameRule(l.CourseID)
	}
	if old := s.replacedMediaToRemove(l, newPath); old != "" {
		// the directory is the lecture's own (media.OwnDirIn: an ordinary
		// directory of exactly its name, no link at it or on its way)
		if own, _ := media.OwnDirIn(s.dataDir, s.lectureDir(l)); !own {
			slog.Warn("replaced media not removed: the lecture's directory is not an ordinary directory of its own", "lecture", l.ID)
		} else if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logWarn("remove replaced media", l.ID, err)
		}
	}
	s.hub.Changed("lecture", l.ID)
	s.hub.Changed("course", l.CourseID)
	s.respondLecture(w, r, l.ID, http.StatusOK)
}

// replacedMediaToRemove is the old media file a replacement deletes, or ""
// when there is none to delete: no old media, the new file itself, a file
// outside data_dir (the user's original, A7), or a stored path naming
// another computer (state.StoredPathUnsafe; security review 2, M3): that
// one is never stat'ed or removed, StoredPathUnsafe logs it once per process
// (Warn) and the replacement goes on. A file inside data_dir but not
// strictly inside this lecture's own directory (another lecture's media,
// the course folder, the database: a media_path the database holds, which
// an imported library supplies; R77b) is not removed either, with a Warn
// that names the lecture only. Nothing here touches the disk; the caller
// checks the directory itself (media.OwnDir) before removing.
func (s *Server) replacedMediaToRemove(l model.Lecture, newPath string) string {
	if l.MediaPath == "" || samePath(newPath, l.MediaPath) {
		return ""
	}
	if state.StoredPathUnsafe(state.PathLectureMedia, l.ID, l.MediaPath) {
		return ""
	}
	old := l.MediaPath
	if !filepath.IsAbs(old) {
		old = filepath.Join(s.lectureDir(l), old)
	}
	if !s.inDataDir(old) {
		return ""
	}
	if !strictlyInside(old, s.lectureDir(l)) {
		slog.Warn("replaced media not removed: it is not inside the lecture's own directory", "lecture", l.ID)
		return ""
	}
	return old
}

// strictlyInside reports whether p lies inside dir, not dir itself
// (lexical, after filepath.Abs; nothing is touched on disk).
func strictlyInside(p, dir string) bool {
	root, err1 := filepath.Abs(dir)
	abs, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

// inDataDir reports whether p lies strictly inside data_dir (files outside
// it, e.g. the user's original recordings, are never deleted; A7).
func (s *Server) inDataDir(p string) bool {
	if s.dataDir == "" {
		return false
	}
	root, err1 := filepath.Abs(s.dataDir)
	abs, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// DELETE /api/lectures/{id}[?force=1] (SPEC §4.6, v1.1 A7): without force
// 409 while a stage runs; with force running stages are cancelled first
// (up to 15 s). Scheduler.DeleteLecture removes the rows and the
// directories inside data_dir.
func (s *Server) deleteLecture(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	force := false
	switch strings.ToLower(r.URL.Query().Get("force")) {
	case "", "0", "false":
	case "1", "true":
		force = true
	default:
		writeError(w, http.StatusBadRequest, "request.bad_param", map[string]any{"name": "force"})
		return
	}
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.sc.DeleteLecture(ctx, l.ID, force); err != nil {
		s.failBusy(w, r, notFoundAs(err, "lecture.not_found"), deleteBusy, map[string]any{"title": l.Title})
		return
	}
	// "lecture" too: a page showing this lecture (another tab) reloads it
	// and shows that it is gone.
	s.hub.Changed("lecture", l.ID)
	s.hub.Changed("course", l.CourseID)
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/lectures/{id}/launch (v1.1 A3).
func (s *Server) launchLecture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.getLectureRow(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.sc.Launch(r.Context(), id); err != nil {
		s.fail(w, r, notFoundAs(err, "lecture.not_found"))
		return
	}
	s.respondLecture(w, r, id, http.StatusOK)
}

// POST /api/lectures/{id}/update {model_choice?} (v1.1 A2.3): rerun the
// earliest stale stage; 409 needs_model_choice when the models differ (A4.3).
func (s *Server) updateLecture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req ModelChoiceRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.getLectureRow(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.sc.UpdateLecture(r.Context(), id, req.ModelChoice); err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondLecture(w, r, id, http.StatusOK)
}

// POST /api/lectures/{id}/stages/{stage}/{action} (SPEC §8) with the
// optional body {model_choice} of v1.1 A4.3 for rerun and retry. Always
// through the scheduler (rerun must hold the scan lock).
func (s *Server) lectureStageAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, stage, action := r.PathValue("id"), r.PathValue("stage"), r.PathValue("action")
	if !validStage(model.ScopeLecture, stage) || !validAction(stage, action) {
		writeError(w, http.StatusBadRequest, "stage.invalid", map[string]any{"stage": stage, "action": action})
		return
	}
	var req ModelChoiceRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	// v3.2.x P1-b: model_choice manual (the retry of summary, repair or
	// translate with a model named for this one retry).
	manual, err := manualTarget(stage, action, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.TextOnly && (stage != model.StageASR || action != model.ActionRerun) {
		// "修复断句" is the asr rerun only (textrepair.go)
		writeError(w, http.StatusBadRequest, "stage.invalid", map[string]any{"stage": stage, "action": action})
		return
	}
	if _, err := s.getLectureRow(ctx, id); err != nil {
		s.fail(w, r, err)
		return
	}
	switch {
	case manual != nil:
		err = s.sc.RetryManual(ctx, model.ScopeLecture, id, stage, *manual)
	case req.TextOnly:
		err = s.rerunTextRepair(ctx, id, req.ModelChoice)
	case stage == model.StageStream && action == model.ActionRetry && s.streamRetryIsNewRun(ctx, id):
		// The playback file: "retry" also after a run that ended without
		// one (audio_only, no_space) or a failure recorded by prep (no
		// stream task): a new run of the task.
		_, err = s.sc.EnqueueAux(ctx, model.ScopeLecture, id, model.StageStream)
	case model.IsAuxStage(stage):
		// Auxiliary tasks (ruling 1): retry or cancel, no model choice.
		err = s.sc.DoAction(ctx, model.ScopeLecture, id, stage, action)
	case action == model.ActionRerun:
		err = s.sc.Rerun(ctx, model.ScopeLecture, id, stage, req.ModelChoice)
	case action == model.ActionRetry:
		err = s.sc.Retry(ctx, model.ScopeLecture, id, stage, req.ModelChoice)
	default:
		err = s.sc.DoAction(ctx, model.ScopeLecture, id, stage, action)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondLecture(w, r, id, http.StatusOK)
}

// streamRetryIsNewRun: retrying the stream task means queueing a new run
// when it has no current job, or one that is done (or stale) although
// stream.mp4 is missing (the result was audio_only or no_space).
func (s *Server) streamRetryIsNewRun(ctx context.Context, lectureID string) bool {
	j, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageStream)
	if errors.Is(err, db.ErrNotFound) {
		return true
	}
	return err == nil && (j.Status == model.StatusDone || j.Status == model.StatusStale)
}

// ---- batch operations (v1.1 A3.2, A7) ----

const maxBatch = 1000

func (s *Server) readBatch(w http.ResponseWriter, r *http.Request) (BatchRequest, bool) {
	var req BatchRequest
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return req, false
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "lecture.batch_empty", nil)
		return req, false
	}
	if len(req.IDs) > maxBatch {
		writeError(w, http.StatusBadRequest, "lecture.batch_too_many", map[string]any{"max": maxBatch})
		return req, false
	}
	return req, true
}

// batchResult builds one id's result: the refreshed lecture on success
// (withLecture), the coded error (and its transitional Chinese text)
// otherwise.
func (s *Server) batchResult(ctx context.Context, id string, err error, withLecture bool, busyCode string) BatchResult {
	if err != nil {
		text, info := s.batchError(ctx, notFoundAs(err, "lecture.not_found"), busyCode)
		return BatchResult{ID: id, Error: text, ErrorInfo: info}
	}
	res := BatchResult{ID: id, OK: true}
	if withLecture {
		if l, err := s.st.GetLecture(ctx, id); err == nil {
			if v, err := s.lectureView(ctx, l); err == nil {
				res.Lecture = &v
			}
		}
	}
	return res
}

// settingsReady checks once, before a batch launch, that the current
// settings can produce the launch snapshots (otherwise every lecture would
// fail with the same message). With no lecture role configured that is
// "只转写" (SPEC-v3 V4.2, sched.LaunchSnapshots): the launch goes ahead.
func (s *Server) settingsReady() error {
	if s.set == nil {
		return nil
	}
	if s.sc != nil {
		_, err := s.sc.LaunchSnapshots()
		return err
	}
	_, err := s.set.CurrentSnapshots()
	return err
}

// launchAll launches lectures one by one (A3.2 启动所选 / 启动全部草稿).
func (s *Server) launchAll(ctx context.Context, ids []string) []BatchResult {
	out := make([]BatchResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.batchResult(ctx, id, s.sc.Launch(ctx, id), true, ""))
	}
	return out
}

// POST /api/lectures/launch {ids}.
func (s *Server) batchLaunch(w http.ResponseWriter, r *http.Request) {
	req, ok := s.readBatch(w, r)
	if !ok {
		return
	}
	if err := s.settingsReady(); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, BatchResponse{Results: s.launchAll(r.Context(), req.IDs)})
}

// POST /api/lectures/update {ids, model_choice?}: one model decision for
// all lectures (A2.3); 409 needs_model_choice lists every lecture's diffs.
func (s *Server) batchUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := s.readBatch(w, r)
	if !ok {
		return
	}
	res, err := s.sc.UpdateLectures(ctx, req.IDs, req.ModelChoice)
	if err != nil {
		s.failChoice(w, r, err)
		return
	}
	out := BatchResponse{Results: make([]BatchResult, 0, len(res))}
	for _, x := range res {
		out.Results = append(out.Results, s.batchResult(ctx, x.LectureID, x.Err, true, ""))
	}
	writeJSON(w, http.StatusOK, out)
}

// failChoice is fail with lecture ids and titles in a needs_model_choice body.
func (s *Server) failChoice(w http.ResponseWriter, r *http.Request, err error) {
	var nm *state.NeedsModelChoiceError
	if errors.As(err, &nm) {
		writeJSON(w, http.StatusConflict, s.needsChoiceView(r.Context(), nm, true))
		return
	}
	s.fail(w, r, err)
}

// POST /api/lectures/cancel {ids} (A7 取消所选).
func (s *Server) batchCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := s.readBatch(w, r)
	if !ok {
		return
	}
	out := BatchResponse{Results: make([]BatchResult, 0, len(req.IDs))}
	for _, id := range req.IDs {
		_, err := s.sc.CancelLecture(ctx, id)
		out.Results = append(out.Results, s.batchResult(ctx, id, err, true, ""))
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/lectures/delete {ids, force} (A7 删除所选).
func (s *Server) batchDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := s.readBatch(w, r)
	if !ok {
		return
	}
	out := BatchResponse{Results: make([]BatchResult, 0, len(req.IDs))}
	courses := map[string]bool{}
	for _, id := range req.IDs {
		l, err := s.st.GetLecture(ctx, id)
		if err == nil {
			err = s.sc.DeleteLecture(ctx, id, req.Force)
			if err == nil {
				courses[l.CourseID] = true
				s.hub.Changed("lecture", id)
			}
		}
		out.Results = append(out.Results, s.batchResult(ctx, id, err, false, deleteBusy))
	}
	for c := range courses {
		s.hub.Changed("course", c)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- sentences and patches ----

const (
	defaultPage = 200
	maxPage     = 1000
)

func queryInt(r *http.Request, name string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, apiErr(http.StatusBadRequest, "request.bad_param", map[string]any{"name": name})
	}
	return n, nil
}

func (s *Server) listSentences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	offset, err := queryInt(r, "offset", 0, 0, 1<<30)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit", defaultPage, 1, maxPage)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	total, err := s.st.CountSentences(ctx, l.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ss, err := s.st.ListSentences(ctx, l.ID, offset, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ps, err := s.st.ListPatches(ctx, l.ID, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byIdx := latestPatches(ps)
	page := SentencePage{Total: total, Offset: offset, Sentences: make([]Sentence, 0, len(ss))}
	for _, x := range ss {
		var p *model.Patch
		if pp, ok := byIdx[x.Idx]; ok {
			p = &pp
		}
		page.Sentences = append(page.Sentences, sentenceView(x, p))
	}
	writeJSON(w, http.StatusOK, page)
}

var reviewStates = map[string]bool{
	"": true, model.ReviewNone: true, model.ReviewPending: true, model.ReviewAccepted: true,
	model.ReviewRejected: true, model.ReviewEdited: true,
}

func (s *Server) listPatches(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	review := r.URL.Query().Get("review")
	if !reviewStates[review] {
		writeError(w, http.StatusBadRequest, "request.bad_param", map[string]any{"name": "review"})
		return
	}
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ps, err := s.st.ListPatches(ctx, l.ID, review)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]Patch, 0, len(ps))
	if len(ps) > 0 {
		all, err := s.st.ListSentences(ctx, l.ID, 0, 0)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		byIdx := make(map[int]model.Sentence, len(all))
		for _, x := range all {
			byIdx[x.Idx] = x
		}
		get := func(i int) (model.Sentence, bool) { x, ok := byIdx[i]; return x, ok }
		for _, p := range ps {
			out = append(out, patchView(p, get))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// snapshotWindow tells the review operations to read the context window x
// from the lecture's translate snapshot inside their own transaction
// (v1.1 lead ruling 8, NOTES.md "v1.1 lead 复核" 1).
const snapshotWindow = -1

// afterSentenceChange: notify clients and request the debounced
// incremental translate (NOTES.md, SPEC §7.3).
func (s *Server) afterSentenceChange(lectureID string, retranslate bool) {
	if retranslate && s.sc != nil {
		s.sc.RequestIncrementalTranslate(lectureID)
	}
	s.hub.Changed("sentences", lectureID)
	s.hub.Changed("patches", lectureID)
	s.hub.Changed("lecture", lectureID)
}

// POST /api/patches/{id}/review {action: accept|reject|edit|reset, text?} (SPEC §6.4).
func (s *Server) reviewPatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Action string  `json:"action"`
		Text   *string `json:"text"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	switch req.Action {
	case model.ReviewActionAccept, model.ReviewActionReject, model.ReviewActionReset:
	case model.ReviewActionEdit:
		if req.Text == nil || strings.TrimSpace(*req.Text) == "" {
			writeError(w, http.StatusBadRequest, "patch.edit_empty", nil)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "patch.unknown_action", nil)
		return
	}
	text := ""
	if req.Text != nil {
		text = strings.TrimSpace(*req.Text)
	}
	p, n, err := state.ReviewPatch(ctx, s.st, snapshotWindow, r.PathValue("id"), req.Action, text)
	if err != nil {
		if errors.Is(err, state.ErrConflict) && !errors.Is(err, state.ErrBusy) {
			err = apiErrDetail(http.StatusConflict, "patch.changed", nil, err.Error())
		}
		s.failBusy(w, r, notFoundAs(err, "patch.not_found"), reviewBusy, nil)
		return
	}
	s.afterSentenceChange(p.LectureID, n > 0)
	v, err := s.singlePatchView(ctx, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v.Retranslated = &n
	writeJSON(w, http.StatusOK, v)
}

// POST /api/lectures/{id}/sentences/{idx}/{edit|reset|retranslate} (SPEC §17.1).
func (s *Server) sentenceOp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op := r.PathValue("op")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 {
		writeError(w, http.StatusBadRequest, "sentence.bad_idx", nil)
		return
	}
	var text string
	var rt RetranslateRequest
	switch op {
	case "retranslate":
		// v3.2.x P1-b: the optional {"use_current": true}
		if err := decodeOptionalJSON(r, &rt); err != nil {
			s.fail(w, r, err)
			return
		}
	case "edit":
		var req struct {
			Text *string `json:"text"`
		}
		if err := decodeJSON(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
		if req.Text == nil || strings.TrimSpace(*req.Text) == "" {
			writeError(w, http.StatusBadRequest, "sentence.text_empty", nil)
			return
		}
		text = strings.TrimSpace(*req.Text)
	case "reset":
	default:
		writeError(w, http.StatusBadRequest, "sentence.unknown_op", nil)
		return
	}
	l, err := s.getLectureRow(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var x model.Sentence
	var mixed *bool
	n := 0
	switch op {
	case "edit":
		x, n, err = state.EditSentence(ctx, s.st, snapshotWindow, l.ID, idx, text)
	case "reset":
		x, n, err = state.ResetSentence(ctx, s.st, snapshotWindow, l.ID, idx)
	case "retranslate":
		if rt.UseCurrent && s.sc != nil {
			var m bool
			x, m, err = s.sc.RetranslateSentence(ctx, l.ID, idx, true)
			mixed = &m
			if errors.Is(err, state.ErrConflict) && !errors.Is(err, state.ErrBusy) {
				// the sentence has a usable translation (state.RetranslateSentenceWith)
				err = apiErrDetail(http.StatusConflict, codeSentenceNotPending, nil, err.Error())
			}
		} else {
			x, err = state.RetranslateSentence(ctx, s.st, l.ID, idx)
		}
		n = 1
	}
	if err != nil {
		s.failBusy(w, r, notFoundAs(err, "sentence.not_found"), reviewBusy, nil)
		return
	}
	s.afterSentenceChange(l.ID, n > 0)
	var p *model.Patch
	if pp, err := s.st.PatchForSentence(ctx, l.ID, idx); err == nil {
		p = &pp
	} else if !errors.Is(err, db.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if mixed != nil {
		writeJSON(w, http.StatusOK, RetranslateResult{Sentence: sentenceView(x, p), Mixed: mixed})
		return
	}
	writeJSON(w, http.StatusOK, sentenceView(x, p))
}

// ---- audio clips (SPEC §8: [start-1s, end+1s] cut by ffmpeg on the fly) ----

const (
	maxClip     = 10 * time.Minute
	clipPadding = 1000 // ms before and after the sentence (SPEC §8)
)

// wavDurationMS is wav.DurationMS (the clip bounds below, the lecture's
// media length in media_duration.go).
func wavDurationMS(path string) (int64, bool) { return wav.DurationMS(path) }

// GET /api/lectures/{id}/audio?start_ms&end_ms: the sentence with 1 s
// before and after, cut by ffmpeg and streamed (SPEC §8, §17.4). v1.1
// A13.3: the response headers give the clip and sentence bounds in ms from
// the start of the lecture, so the front end never hard-codes the padding.
func (s *Server) audio(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, err1 := strconv.ParseInt(q.Get("start_ms"), 10, 64)
	end, err2 := strconv.ParseInt(q.Get("end_ms"), 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		writeError(w, http.StatusBadRequest, "audio.bad_range", nil)
		return
	}
	l, err := s.getLectureRow(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	wav := filepath.Join(s.lectureDir(l), model.AudioFile)
	if _, err := os.Stat(wav); err != nil {
		writeError(w, http.StatusConflict, "audio.not_ready", nil)
		return
	}
	clipStart := max(start-clipPadding, 0)
	clipEnd := end + clipPadding
	if total, ok := wavDurationMS(wav); ok && clipEnd > total {
		clipEnd = total
	}
	if clipEnd-clipStart > maxClip.Milliseconds() {
		clipEnd = clipStart + maxClip.Milliseconds()
	}
	if clipEnd <= clipStart {
		writeError(w, http.StatusBadRequest, "audio.past_end", nil)
		return
	}
	ms := func(v int64) string { return strconv.FormatFloat(float64(v)/1000, 'f', 3, 64) }
	// The request context kills ffmpeg when the browser stops the clip.
	cmd := exec.CommandContext(r.Context(), s.cfg.FFmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-ss", ms(clipStart), "-t", ms(clipEnd-clipStart), "-i", wav, "-vn", "-acodec", "pcm_s16le", "-f", "wav", "pipe:1")
	cmd.WaitDelay = 2 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Tied: ffmpeg ends with LectureFlow, also on a crash (test-chaos 1).
	proc, err := procutil.Start(cmd, procutil.Tied)
	if err != nil {
		writeErrorInfo(w, http.StatusInternalServerError, model.ErrorInfo{Code: "audio.ffmpeg_failed", Detail: err.Error()})
		return
	}
	defer proc.Close()
	// Read the first bytes before committing to 200, so an ffmpeg failure
	// still produces a JSON error.
	head := make([]byte, 4096)
	n, rerr := io.ReadFull(out, head)
	if n == 0 {
		_ = cmd.Wait()
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && rerr != nil {
			msg = rerr.Error()
		}
		writeErrorInfo(w, http.StatusInternalServerError, model.ErrorInfo{Code: "audio.ffmpeg_failed", Detail: msg})
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "audio/wav")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Clip-Start-Ms", strconv.FormatInt(clipStart, 10))
	hd.Set("X-Clip-Sentence-Start-Ms", strconv.FormatInt(start, 10))
	hd.Set("X-Clip-Sentence-End-Ms", strconv.FormatInt(end, 10))
	hd.Set("X-Clip-End-Ms", strconv.FormatInt(clipEnd, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(head[:n]); err == nil && rerr == nil {
		_, _ = io.Copy(w, out) // streamed; never buffered whole (SPEC §17.4)
	}
	_ = cmd.Wait()
}

// ---- export download ----

func (s *Server) exportFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	// A trailing dot or space is refused too (AUD1 I-5): Windows drops them,
	// so "x.tmp." would open the half-written "x.tmp".
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\:`) || name == "." || name == ".." ||
		strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp") || strings.TrimRight(name, ". ") != name {
		writeError(w, http.StatusBadRequest, "export.bad_name", nil)
		return
	}
	l, err := s.getLectureRow(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	dir := filepath.Join(s.lectureDir(l), model.ExportDirName)
	full := filepath.Join(dir, name)
	if filepath.Dir(full) != filepath.Clean(dir) {
		writeError(w, http.StatusBadRequest, "export.bad_name", nil)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeError(w, http.StatusNotFound, "export.not_found", nil)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "export.not_found", nil)
		return
	}
	// P13.6 (lead ruling): downloading a citation JSON is an export the
	// renumbering warning must know about; generating it is not.
	if r.Method == http.MethodGet && strings.HasSuffix(strings.ToLower(name), ".cite.json") {
		if err := stages.MarkCiteExported(r.Context(), s.st, l.CourseID, s.now()); err != nil {
			s.fail(w, r, err)
			return
		}
		s.hub.Changed("course", l.CourseID)
	}
	ct := mime.TypeByExtension(filepath.Ext(name))
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md":
		ct = "text/markdown; charset=utf-8"
	case ".vtt":
		ct = "text/vtt; charset=utf-8"
	case ".txt":
		ct = "text/plain; charset=utf-8"
	case ".json":
		ct = "application/json; charset=utf-8"
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	dl := s.exportDownloadName(r.Context(), l, name)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, asciiFileName(dl), url.PathEscape(dl)))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// exportDownloadName is the download name of export file name of lecture l
// (SPEC-v2 P13.6, stages.ExportDownloadName): a transcript or subtitle
// file is named after the lecture's current number and English title
// ("W02L1 Stream Ciphers.bilingual.md") even when it was written before a
// new title or a renumbering; a citation JSON keeps its name. The number
// is the citation label (unique "W??-{id prefix}" for an unnumbered
// lecture), as the export stage uses. Without it the stored name is used.
func (s *Server) exportDownloadName(ctx context.Context, l model.Lecture, name string) string {
	c, err := s.st.GetCourse(ctx, l.CourseID)
	if err != nil {
		slog.Warn("export download name", "lecture", l.ID, "err", err)
		return name
	}
	nums, _, err := s.courseNumbering(ctx, c)
	if err != nil {
		slog.Warn("export download name", "lecture", l.ID, "err", err)
		return name
	}
	n, ok := nums[l.ID]
	if !ok {
		return name
	}
	label := n.CiteLabel
	if label == "" {
		label = n.Label
	}
	return stages.ExportDownloadName(label, l.Title, name)
}
