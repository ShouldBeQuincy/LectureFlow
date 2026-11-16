// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"strings"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Store is what the queue needs from the database. It is not part of
// db.Store: the SQLite store implements it (internal/db/downloads.go) and
// cmd passes it in.
type Store interface {
	InsertDownloadTask(ctx context.Context, t db.DownloadTask) (int64, error)
	UpdateDownloadTask(ctx context.Context, t db.DownloadTask) error
	SetDownloadProgress(ctx context.Context, id, done, total int64, updatedAt string) error
	GetDownloadTask(ctx context.Context, id int64) (db.DownloadTask, error)
	FindDownloadTask(ctx context.Context, platform, sessionID string) (db.DownloadTask, error)
	ListDownloadTasks(ctx context.Context) ([]db.DownloadTask, error)
	DeleteDownloadTask(ctx context.Context, id int64) error
	ResetUnfinishedDownloads(ctx context.Context, code, updatedAt string) (int64, error)
	// ListLectureNames: the titles and original file names of a course's
	// lectures ("" = every course), for check's dedup by title.
	ListLectureNames(ctx context.Context, courseID string) ([]db.LectureName, error)

	GetCourse(ctx context.Context, id string) (model.Course, error)
	GetKV(ctx context.Context, key string) (string, bool, error)
}

// ImportRequest hands a finished download to LectureFlow's lecture import
// (internal/api import_internal.go, injected by cmd so that this package
// does not depend on the HTTP layer).
type ImportRequest struct {
	TaskID   int64
	CourseID string
	// Path is the finished media file in the task's directory (inside the
	// data directory); the import moves it.
	Path string
	// Filename is the lecture's original_filename: the platform's title
	// made safe for a file name, plus the file's extension.
	Filename string
	// Title is the platform's title (whitespace normalised).
	Title string
	// RecordedAt is the recording time the extension sent, as
	// lecture.recorded_at ("YYYY-MM-DDTHH:MM", the wall time the page
	// stated, not converted to this computer's zone; see recordedAt in
	// bridge.go); nil = unknown.
	RecordedAt *string
	// Angles are the extra camera angles to keep with the lecture (angle.go:
	// at most AngleName): the import moves each file into the lecture's
	// directory as Name + ".mp4".
	Angles []ImportAngle
}

// ImportAngle is a camera angle of ImportRequest.
type ImportAngle struct {
	Name string // AngleName
	Path string // the remuxed file in the task's directory (moved by the import)
}

// ImportResult is the lecture created from a download.
type ImportResult struct {
	LectureID string
	// LaunchError is why the new draft was not launched (nil = it was).
	LaunchError *model.ErrorInfo
}

// ImportFunc creates the lecture of a finished download and launches it.
type ImportFunc func(ctx context.Context, req ImportRequest) (ImportResult, error)

// ImportRefused is an ImportFunc's error when the lecture import's media
// checks refuse the downloaded file itself (internal/api upload_media.go:
// an empty file, a web page such as a sign-in or error page, a playlist, a
// format outside the whitelist). Code is one of the API's "upload.media_*"
// codes and Params its params (the file name only, never a path or URL).
// The task fails with this code instead of CodeImportFailed, so that the
// download list shows the reason (review R10 M1). The same file would be
// refused again: the extension sending the recording again downloads it
// anew (Enqueue) instead of reusing the file.
type ImportRefused struct {
	Code   string
	Params map[string]any
}

func (e *ImportRefused) Error() string {
	return "the lecture import refused the downloaded file: " + e.Code
}

// RefusedFile reports a task error code of ImportRefused: the downloaded
// file is not a recording, and a retry with the same file fails again.
func RefusedFile(code string) bool { return strings.HasPrefix(code, "upload.media_") }

// Settings keys (kv) of the download queue, read and written by
// /api/app/settings (both modes).
const (
	KVConcurrency = "app.download_concurrency" // tasks downloading at once
	KVConnections = "app.download_connections" // connections per file
)

// Limits of the settings.
const (
	MinConcurrency, MaxConcurrency, DefaultConcurrency = 1, 6, 3
	MinConnections, MaxConnections, DefaultConnections = 1, 16, 4
)

// DefaultFailedCredentialTTL is how long a failed task keeps its cookies
// and signed URLs in memory so that "重试" works without the recording page
// (lead's ruling 4).
const DefaultFailedCredentialTTL = 30 * time.Minute

// trackRecord is one entry of download_task.tracks_json: no URL query, no
// path, no credential (privacy.go).
type trackRecord struct {
	Kind      string `json:"kind"`
	Format    string `json:"format"`
	Role      string `json:"role"`
	Label     string `json:"label"`
	Quality   string `json:"quality,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Host      string `json:"host"`
	// PathHash is pathHash of the URL's path (lead's ruling 3: the path of
	// a recording may itself be the credential).
	PathHash string `json:"path_hash"`
	// Selector picks the AdaptationSet of a DASH track (bridge.Track).
	Selector string `json:"selector,omitempty"`
	// Path is how rows written before ruling 3 kept the path: read only,
	// turned into PathHash when read and gone when the row is written.
	Path string `json:"path,omitempty"`
}

// TrackInfo is a track of a task as the HTTP layer shows it.
type TrackInfo struct {
	Kind, Format, Role, Label string
}

// TaskInfo is a task as the HTTP layer shows it (ext-brief §6.2 "接口"):
// the stored row plus what only the running queue knows.
type TaskInfo struct {
	ID               int64
	Platform         string
	SessionID        string
	CourseID         string
	Title            string
	RecordedAt       string // "" = unknown
	State            string
	Position         int // 1-based among waiting tasks; 0 otherwise
	BytesDone        int64
	BytesTotal       int64 // -1 = unknown
	LectureID        string
	Error            *model.ErrorInfo // why it failed or needs a refresh; nil otherwise
	OverrideDisabled bool
	Tracks           []TrackInfo
	// AllowedActions are what the user may do now: cancel, retry, remove.
	AllowedActions []string
	CreatedAt      string
	UpdatedAt      string
}

// Actions of TaskInfo.AllowedActions.
const (
	ActionCancel = "cancel"
	ActionRetry  = "retry"
	ActionRemove = "remove"
)

// Activity is what /api/app/activity counts (close prompt, sleep blocker).
type Activity struct {
	Active int // downloading, merging or importing
	Queued int
}
