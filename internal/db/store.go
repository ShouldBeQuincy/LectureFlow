// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package db owns the SQLite connection, migrations and all queries.
//
// The Store interface below is FROZEN in Phase 0 (SPEC §16.2). Adding or
// changing a method is a lead decision recorded in NOTES.md.
//
// Invariants enforced here:
//   - sentence.text is written only by RecomputeText (SPEC §3.2).
//   - job.status is written only by CASJobStatus, which only state.Transition
//     may call (SPEC §4.5). InsertJob may set the initial status of a new row.
package db

import (
	"context"
	"errors"

	"lectureflow/internal/model"
)

// ErrNotFound is returned by Get* methods when the row does not exist.
var ErrNotFound = errors.New("not found")

// SentenceTr is one translation result written by translate (SPEC §7).
type SentenceTr struct {
	Zh       *string
	ZhStatus string
	ZhHash   *string
	QAError  *string
}

// JobFinish carries the columns written together with a status change.
// Nil fields are left unchanged.
type JobFinish struct {
	Error      *string // set to "" to clear
	StartedAt  *string
	FinishedAt *string
	Attempt    *int
	Progress   *float64
	// StaleReason is written with done → stale (v1.1 A2.2); "" clears it.
	// (A stale job's glossary reason is later rewritten by SetStaleReason.)
	StaleReason *string
}

// ErrDuplicate is returned when a UNIQUE constraint rejects a write
// (e.g. a provider name that already exists, v1.1 A5.1).
var ErrDuplicate = errors.New("duplicate")

type Store interface {
	// Tx runs fn inside one transaction with a Store bound to it.
	// Nested calls on a tx-bound Store reuse the outer transaction.
	Tx(ctx context.Context, fn func(tx Store) error) error
	Close() error

	// --- course ---
	CreateCourse(ctx context.Context, c model.Course) error
	ListCourses(ctx context.Context) ([]model.Course, error)
	GetCourse(ctx context.Context, id string) (model.Course, error)
	// BumpGlossaryVersion increments course.glossary_version and returns the new value.
	BumpGlossaryVersion(ctx context.Context, courseID string) (int, error)

	// --- syllabus ---
	// UpsertSyllabus replaces the syllabus row (text reset to s.Text).
	UpsertSyllabus(ctx context.Context, s model.Syllabus) error
	GetSyllabus(ctx context.Context, courseID string) (model.Syllabus, error)
	SetSyllabusText(ctx context.Context, courseID, text string) error

	// --- lecture ---
	// CreateLecture inserts the lecture and, in the same transaction, one
	// pending job for each of the six lecture stages (SPEC §3.3).
	CreateLecture(ctx context.Context, l model.Lecture) error
	ListLectures(ctx context.Context, courseID string) ([]model.Lecture, error) // ordered by created_at
	GetLecture(ctx context.Context, id string) (model.Lecture, error)
	SetLectureTitle(ctx context.Context, id, title string) error
	SetLectureMedia(ctx context.Context, id, mediaPath string) error
	DeleteLecture(ctx context.Context, id string) error // cascades; also deletes its jobs

	// --- material ---
	// CreateMaterial inserts the material and its material_extract and
	// material_terms pending jobs in the same transaction (SPEC §3.3).
	CreateMaterial(ctx context.Context, m model.Material) error
	ListMaterials(ctx context.Context, courseID string) ([]model.Material, error) // ordered by created_at
	// ListMaterialBindings returns material id → lecture id of the course's
	// materials bound to a lecture (no pages_md read).
	ListMaterialBindings(ctx context.Context, courseID string) (map[string]string, error)
	GetMaterial(ctx context.Context, id string) (model.Material, error)
	SetMaterialPages(ctx context.Context, id string, pages []string) error
	SetMaterialLecture(ctx context.Context, id string, lectureID *string) error
	DeleteMaterial(ctx context.Context, id string) error // also deletes its jobs

	// --- term ---
	ListTerms(ctx context.Context, courseID string) ([]model.Term, error) // ordered by en_key
	GetTerm(ctx context.Context, id string) (model.Term, error)
	InsertTerm(ctx context.Context, t model.Term) error
	UpdateTerm(ctx context.Context, t model.Term) error // writes every column but id and course_id (v2: tier, locked, common_word, janitor_note, zh_source too)
	DeleteTerm(ctx context.Context, id string) error
	SetTermFreq(ctx context.Context, id string, freq int) error

	// --- sentence ---
	// InsertSentences writes all rows (text is filled by RecomputeText afterwards
	// by the caller in the same transaction).
	InsertSentences(ctx context.Context, lectureID string, ss []model.Sentence) error
	ListSentences(ctx context.Context, lectureID string, offset, limit int) ([]model.Sentence, error) // limit<=0 = all
	GetSentence(ctx context.Context, lectureID string, idx int) (model.Sentence, error)
	CountSentences(ctx context.Context, lectureID string) (int, error)
	// CountZh returns counts per zh_status.
	CountZh(ctx context.Context, lectureID string) (map[string]int, error)
	DeleteSentences(ctx context.Context, lectureID string) error
	SetRepairedText(ctx context.Context, lectureID string, idx int, repaired string) error
	// ResetRepairedText sets repaired_text = asr_text for every sentence.
	ResetRepairedText(ctx context.Context, lectureID string) error
	SetHumanText(ctx context.Context, lectureID string, idx int, human *string) error
	SetTranslation(ctx context.Context, lectureID string, idx int, tr SentenceTr) error
	SetZhStatus(ctx context.Context, lectureID string, idxs []int, status string) error
	// ClearTranslations sets zh=NULL, zh_status='none', zh_hash=NULL, qa_error=NULL for all sentences.
	ClearTranslations(ctx context.Context, lectureID string) error
	// RecomputeText sets text = COALESCE(human_text, repaired_text) for idxs
	// (nil = all sentences). The ONLY writer of sentence.text.
	RecomputeText(ctx context.Context, lectureID string, idxs []int) error

	// --- patch ---
	InsertPatches(ctx context.Context, ps []model.Patch) error
	ListPatches(ctx context.Context, lectureID string, review string) ([]model.Patch, error) // review "" = all; ordered by sentence_idx
	GetPatch(ctx context.Context, id string) (model.Patch, error)
	// PatchForSentence returns the patch of that sentence, ErrNotFound if none.
	PatchForSentence(ctx context.Context, lectureID string, idx int) (model.Patch, error)
	SetPatchReview(ctx context.Context, id, review string, editedText *string) error
	DeletePatches(ctx context.Context, lectureID string) error
	CountPatches(ctx context.Context, lectureID, review string) (int, error)

	// --- summary ---
	UpsertSummary(ctx context.Context, s model.Summary) error
	GetSummary(ctx context.Context, lectureID string) (model.Summary, error)
	DeleteSummary(ctx context.Context, lectureID string) error

	// --- job ---
	InsertJob(ctx context.Context, j model.Job) error
	GetJob(ctx context.Context, id string) (model.Job, error)
	GetCurrentJob(ctx context.Context, scope, scopeID, stage string) (model.Job, error)
	ListCurrentJobs(ctx context.Context, scope, scopeID string) ([]model.Job, error)
	// ListCurrentJobsByStatus returns current jobs in any of the statuses,
	// oldest scope first (lecture/material created_at, then job created_at).
	ListCurrentJobsByStatus(ctx context.Context, statuses ...string) ([]model.Job, error)
	// ListScanJobs is ListCurrentJobsByStatus(pending, queued), in the same
	// order, without the pending jobs of draft lectures (launched_at IS
	// NULL) whose stage is in draftHeld: the stages state.Deps holds with
	// "not_started" on a draft before checking anything else, so the
	// scheduler's scan has nothing to do with them (performance audit F08).
	// A job whose lecture row is missing is still listed. Queued jobs are
	// always listed. draftHeld empty: exactly ListCurrentJobsByStatus.
	ListScanJobs(ctx context.Context, draftHeld []string) ([]model.Job, error)
	// ListJobsFinishedSince returns all jobs (current or not) with finished_at >= since (for usage_today).
	ListJobsFinishedSince(ctx context.Context, since string) ([]model.Job, error)
	// CASJobStatus: UPDATE job SET status=to (+ fields) WHERE id=? AND status=from AND is_current=1.
	// Returns false when no row matched. Only state.Transition may call it.
	CASJobStatus(ctx context.Context, jobID, from, to string, f JobFinish) (bool, error)
	RetireJob(ctx context.Context, jobID string) error // is_current = 0
	SetJobProgress(ctx context.Context, jobID string, p float64) error
	SetJobInputHash(ctx context.Context, jobID, hash string) error
	SetJobError(ctx context.Context, jobID string, errText *string) error
	SetJobStats(ctx context.Context, jobID string, st model.JobStats) error
	DeleteJobs(ctx context.Context, scope, scopeID string) error

	// --- kv (migrations/002_kv.sql) ---
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, value string) error

	// ---------------------------------------------------------------------
	// v1.1 (migrations/003_v1_1.sql). FROZEN by the lead.
	// CreateCourse also writes course.code; CreateLecture writes seq (0 →
	// max(seq)+1 within the course) and launched_at, and inserts the six
	// pending jobs with config_snapshot "{}". InsertJob writes
	// config_snapshot ("" → "{}") and stale_reason. All job reads fill
	// ConfigSnapshot and StaleReason.
	// ---------------------------------------------------------------------

	// SetCourseCode sets course.code (nil or "" = none).
	SetCourseCode(ctx context.Context, id string, code *string) error
	// SetCourseName renames a course.
	SetCourseName(ctx context.Context, id, name string) error

	// SetLectureLaunched sets launched_at = at only while it is NULL.
	// Returns false (no error) when the lecture was already launched.
	SetLectureLaunched(ctx context.Context, id, at string) (bool, error)
	// SetLectureSeq sets lecture.seq.
	SetLectureSeq(ctx context.Context, id string, seq int) error
	// NextLectureSeq returns max(seq)+1 of the course's lectures (1 when none).
	NextLectureSeq(ctx context.Context, courseID string) (int, error)

	// SetJobSnapshot writes job.config_snapshot ("" → "{}").
	SetJobSnapshot(ctx context.Context, jobID, snapshot string) error
	// SetStaleReason rewrites stale_reason of a current job that is still
	// stale; it never touches status. False when the job is no longer stale
	// (or no longer current) or already has that reason.
	SetStaleReason(ctx context.Context, jobID, reason string) (bool, error)

	// --- provider (A5.1) ---
	ListProviders(ctx context.Context) ([]model.Provider, error) // ordered by created_at, id
	GetProvider(ctx context.Context, id string) (model.Provider, error)
	// InsertProvider: ErrDuplicate when the name exists. CreatedAt defaults to now.
	InsertProvider(ctx context.Context, p model.Provider) error
	// UpdateProvider writes every column except id and created_at
	// (including api_key: callers keep the old key when it is unchanged).
	// ErrDuplicate when the new name belongs to another provider.
	UpdateProvider(ctx context.Context, p model.Provider) error
	// DeleteProvider deletes the row. It fails (FK) while a role_setting
	// references it; callers check first.
	DeleteProvider(ctx context.Context, id string) error

	// --- role_setting (A5.1) ---
	ListRoleSettings(ctx context.Context) ([]model.RoleSetting, error) // ordered by role
	// UpsertRoleSetting inserts or replaces the row of r.Role (Params "" → "{}").
	UpsertRoleSetting(ctx context.Context, r model.RoleSetting) error

	// --- usage_daily (A5.1, A10.3) ---
	// AddUsageDaily adds the deltas to (u.ProviderID, u.Day), creating the row.
	AddUsageDaily(ctx context.Context, u model.UsageDaily) error
	// ListUsageDaily returns rows with day >= sinceDay ("" = all), ordered by day, provider_id.
	ListUsageDaily(ctx context.Context, sinceDay string) ([]model.UsageDaily, error)

	// ---------------------------------------------------------------------
	// v2 phase 2 (migrations/006_v2.sql). FROZEN by the lead. Course,
	// lecture, sentence, provider and usage reads/writes include the new
	// columns (CreateCourse/CreateLecture/InsertSentences/InsertProvider/
	// UpdateProvider/AddUsageDaily write them; Lecture.RecordedAtSource ""
	// → none, TitleSource "" → filename; Sentence.Words nil → NULL).
	// ---------------------------------------------------------------------

	// SetCourseCalendar sets term_start and skip_weeks (P13.4; nil skip → []).
	SetCourseCalendar(ctx context.Context, id string, termStart *string, skipWeeks []string) error
	// SetCourseFilenameRule sets filename_rule (raw JSON calendar.FilenameRule; nil = none).
	SetCourseFilenameRule(ctx context.Context, id string, rule *string) error
	// SetLectureRecordedAt sets recorded_at and its source (P13.3.4).
	SetLectureRecordedAt(ctx context.Context, id string, at *string, source string) error
	// SetLectureTitles sets title, title_zh and title_source (P13.5).
	SetLectureTitles(ctx context.Context, id, title string, titleZh *string, source string) error
	// SetLectureBlurb sets blurb_en and blurb_zh (P13.5).
	SetLectureBlurb(ctx context.Context, id string, en, zh *string) error
	// SetLectureOriginalFilename sets original_filename (P13.3.1).
	SetLectureOriginalFilename(ctx context.Context, id, name string) error
	// ListStaleReasons returns job id → stale_reason of every job that has one
	// (for the one-time P14.3 conversion).
	ListStaleReasons(ctx context.Context) (map[string]string, error)
	// ReplaceStaleReason overwrites stale_reason of one job (P14.3 conversion only).
	ReplaceStaleReason(ctx context.Context, jobID, reason string) error

	// ---------------------------------------------------------------------
	// Lecture kinds, numbering mode and week bindings
	// (migrations/007_lecture_kinds.sql; lead freeze .claude/kinds-brief.md,
	// added by K1). Course reads/writes include numbering ("" → auto),
	// lecture reads/writes kind ("" ↔ NULL) and kind_source ("" → none),
	// material reads/writes bind_week; CreateMaterial refuses LectureID and
	// BindWeek together (ErrBindingInvalid). SetMaterialLecture clears
	// bind_week (it is SetMaterialBinding(id, lectureID, nil)).
	// ---------------------------------------------------------------------

	// ListLectureKinds returns the kind catalog ordered by ord, id.
	ListLectureKinds(ctx context.Context) ([]model.LectureKind, error)
	GetLectureKind(ctx context.Context, id string) (model.LectureKind, error)
	// InsertLectureKind inserts a custom kind and returns it as stored: ID
	// "" → NewID(), Order 0 → max(ord)+10, Builtin always false.
	// ErrKindCodeTaken / ErrKindNameTaken (both errors.Is ErrDuplicate).
	InsertLectureKind(ctx context.Context, k model.LectureKind) (model.LectureKind, error)
	// UpdateLectureKind writes code, name, grp and ord (Order 0 keeps the
	// old one) of a custom kind. ErrBuiltinKind, ErrKindCodeTaken,
	// ErrKindNameTaken, ErrNotFound. It does not check use: callers refuse
	// a code or group change of a kind in use (CountLecturesByKind).
	UpdateLectureKind(ctx context.Context, k model.LectureKind) error
	// DeleteLectureKind deletes a custom kind. ErrBuiltinKind; ErrKindInUse
	// while a lecture's stored kind is it; ErrNotFound. kind_map entries of
	// course rules that point at it are the caller's to drop.
	DeleteLectureKind(ctx context.Context, id string) error
	// CountLecturesByKind returns stored kind → number of lectures (lectures
	// without a stored kind are not counted); courseID "" = every course.
	CountLecturesByKind(ctx context.Context, courseID string) (map[string]int, error)
	// SetLectureKind sets kind ("" = NULL, not decided) and kind_source ("" = none).
	SetLectureKind(ctx context.Context, id, kind, source string) error
	// SetCourseNumbering sets course.numbering ("" = auto).
	SetCourseNumbering(ctx context.Context, id, numbering string) error
	// SetMaterialBinding binds a material to a lecture (lectureID), to a
	// teaching week (week, 1–model.MaxBindWeek) or to neither (course-wide).
	// ErrBindingInvalid when both are set or the week is out of range.
	SetMaterialBinding(ctx context.Context, id string, lectureID *string, week *int) error
	// ListMaterialRefs is ListMaterials without reading pages_md (PagesMD is
	// always nil): bindings and material sets, without the page texts.
	ListMaterialRefs(ctx context.Context, courseID string) ([]model.Material, error)
	// MaterialPageCounts returns material id → number of pages of the
	// course's materials whose pages_md is set (len(PagesMD), counted by
	// SQLite without reading the page texts); materials not yet extracted
	// are missing (0 pages). Read only (lead, lecture-kinds review item 9).
	MaterialPageCounts(ctx context.Context, courseID string) (map[string]int, error)

	// ---------------------------------------------------------------------
	// asr_device (SPEC-v2 I6, migrations/008_asr_devices.sql). FROZEN by the
	// lead for phase 3b (.claude/v3b-brief.md). No delete: a device that a
	// detection no longer sees keeps its row (devices.Registry marks it as
	// not present in memory).
	// ---------------------------------------------------------------------

	// ListASRDevices returns every row ordered by priority DESC, then id.
	ListASRDevices(ctx context.Context) ([]model.ASRDevice, error)
	// GetASRDevice returns one row; ErrNotFound when there is none.
	GetASRDevice(ctx context.Context, id string) (model.ASRDevice, error)
	// InsertASRDevice inserts a row (all columns, last_benchmark included);
	// ErrDuplicate when the id exists.
	InsertASRDevice(ctx context.Context, d model.ASRDevice) error
	// UpdateASRDevice writes every column but id and last_benchmark;
	// ErrNotFound when there is no such row.
	UpdateASRDevice(ctx context.Context, d model.ASRDevice) error
	// SetASRDeviceBenchmark writes last_benchmark (raw JSON of
	// model.DeviceBenchmark; nil = NULL); ErrNotFound when there is no such row.
	SetASRDeviceBenchmark(ctx context.Context, id string, raw *string) error

	// ---------------------------------------------------------------------
	// Week overrides (migrations/011_week_override.sql; course-ux, user
	// request 2026-10-27). GetLecture and ListLectures read a lecture's
	// override into Lecture.WeekOverride.
	// ---------------------------------------------------------------------

	// SetWeekOverride writes the lecture's override (replacing one it has).
	// CreatedAt "" → now. ErrWeekOverrideInvalid for a week outside
	// 1–model.MaxBindWeek or an unknown kind; ErrNotFound when there is no
	// such lecture.
	SetWeekOverride(ctx context.Context, lectureID string, o model.WeekOverride) error
	// DeleteWeekOverride removes the lecture's override; ErrNotFound when it
	// has none.
	DeleteWeekOverride(ctx context.Context, lectureID string) error
}
