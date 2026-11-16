// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"lectureflow/internal/fetch"
	"lectureflow/internal/model"
)

// The browser extension's download tasks (SPEC-ext 下载队列, ext-brief §6.2
// "接口（冻结…）"): GET /api/downloads[?course_id=], POST
// /api/downloads/{id}/cancel, POST /api/downloads/{id}/retry, DELETE
// /api/downloads/{id}. A task changes: SSE changed {kind: "downloads", id:
// "<task id>"} (the queue throttles it per task), then the page GETs again.

// DownloadQueue is the download queue (internal/fetch.Queue) as the HTTP
// layer uses it; an interface so tests can fake it.
type DownloadQueue interface {
	List(ctx context.Context, courseID string) ([]fetch.TaskInfo, error)
	Get(ctx context.Context, id int64) (fetch.TaskInfo, error)
	Cancel(ctx context.Context, id int64) (fetch.TaskInfo, error)
	Retry(ctx context.Context, id int64) (fetch.TaskInfo, error)
	Remove(ctx context.Context, id int64) error
	Activity() fetch.Activity
	// SettingsChanged rereads app.download_concurrency / _connections.
	SettingsChanged()
}

// Download is one download task.
type Download struct {
	ID         int64  `json:"id"`
	Platform   string `json:"platform"`
	SessionID  string `json:"session_id"`
	CourseID   string `json:"course_id"`
	Title      string `json:"title"`
	RecordedAt string `json:"recorded_at"` // local "YYYY-MM-DDTHH:MM"; "" = unknown
	// State: queued | downloading | merging | importing | done | failed |
	// needs_refresh | cancelled.
	State      string `json:"state"`
	Position   int    `json:"position"` // 1-based among waiting tasks; 0 otherwise
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"` // -1 = unknown
	LectureID  string `json:"lecture_id"`  // "" until done (or when the lecture was deleted)
	// Error is why the task failed or needs a refresh (errors.<code>).
	Error            *model.ErrorInfo `json:"error"`
	OverrideDisabled bool             `json:"override_disabled"`
	Tracks           []DownloadTrack  `json:"tracks"`
	// AllowedActions: cancel (not ended, not importing), retry (failed,
	// when a retry can fix it: fetch.Retryable), remove (done, failed,
	// cancelled). A retry not offered answers 409 with the task's own
	// error code and params.
	AllowedActions []string `json:"allowed_actions"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

// DownloadTrack is a track of a task (no URL: they hold credentials).
type DownloadTrack struct {
	Kind   string `json:"kind"`   // audio | video | unknown
	Format string `json:"format"` // file | hls
	Role   string `json:"role"`
	Label  string `json:"label"`
}

// DownloadList is GET /api/downloads.
type DownloadList struct {
	Items []Download `json:"items"` // newest first
}

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(pattern string, fn http.HandlerFunc)) {
		h("GET /api/downloads", s.listDownloads)
		h("POST /api/downloads/{id}/cancel", s.cancelDownload)
		h("POST /api/downloads/{id}/retry", s.retryDownload)
		h("DELETE /api/downloads/{id}", s.deleteDownload)
	})
}

func downloadView(t fetch.TaskInfo) Download {
	d := Download{
		ID: t.ID, Platform: t.Platform, SessionID: t.SessionID, CourseID: t.CourseID, Title: t.Title, RecordedAt: t.RecordedAt,
		State: t.State, Position: t.Position, BytesDone: t.BytesDone, BytesTotal: t.BytesTotal, LectureID: t.LectureID,
		Error: t.Error, OverrideDisabled: t.OverrideDisabled, Tracks: []DownloadTrack{}, AllowedActions: t.AllowedActions,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
	if d.AllowedActions == nil {
		d.AllowedActions = []string{}
	}
	for _, tr := range t.Tracks {
		d.Tracks = append(d.Tracks, DownloadTrack{Kind: tr.Kind, Format: tr.Format, Role: tr.Role, Label: tr.Label})
	}
	return d
}

// requireDownloads answers 503 without a queue.
func (s *Server) requireDownloads(w http.ResponseWriter) bool {
	if s.dl == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "downloads"})
		return false
	}
	return true
}

// failDownload answers the queue's coded errors: 404 downloads.not_found,
// 409 for a state that does not allow the action.
func (s *Server) failDownload(w http.ResponseWriter, r *http.Request, err error) {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, fetch.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, fetch.ErrConflict):
			status = http.StatusConflict
		}
		writeErrorInfo(w, status, model.ErrorInfo{Code: ce.Info.Code, Params: ce.Info.Params})
		return
	}
	s.fail(w, r, err)
}

// downloadID reads {id}; anything but a positive integer is not a task.
func downloadID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, fetch.CodeNotFound, nil)
		return 0, false
	}
	return id, true
}

func (s *Server) listDownloads(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w) {
		return
	}
	tasks, err := s.dl.List(r.Context(), r.URL.Query().Get("course_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := DownloadList{Items: make([]Download, 0, len(tasks))}
	for _, t := range tasks {
		out.Items = append(out.Items, downloadView(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) cancelDownload(w http.ResponseWriter, r *http.Request) {
	if s.requireDownloads(w) {
		s.downloadAction(w, r, s.dl.Cancel)
	}
}

func (s *Server) retryDownload(w http.ResponseWriter, r *http.Request) {
	if s.requireDownloads(w) {
		s.downloadAction(w, r, s.dl.Retry)
	}
}

func (s *Server) downloadAction(w http.ResponseWriter, r *http.Request, act func(context.Context, int64) (fetch.TaskInfo, error)) {
	id, ok := downloadID(w, r)
	if !ok {
		return
	}
	t, err := act(r.Context(), id)
	if err != nil {
		s.failDownload(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, downloadView(t))
}

func (s *Server) deleteDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireDownloads(w) {
		return
	}
	id, ok := downloadID(w, r)
	if !ok {
		return
	}
	if err := s.dl.Remove(r.Context(), id); err != nil {
		s.failDownload(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
