// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package api is the HTTP JSON API and SSE (SPEC §8).
//
// The response structs in this file are FROZEN in Phase 0 (SPEC §16.2);
// testdata/api/*.json holds one example per struct for front-end development.
package api

import "lectureflow/internal/model"

// ErrorResponse is the body of every 4xx/5xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// StageView is one stage of a lecture, course (glossary_seed) or material.
type StageView struct {
	Stage    string  `json:"stage"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Attempt  int     `json:"attempt"`
	Error    *string `json:"error"`
	// ErrorInfo is the coded form of Error (v2 P14.3) when the stage wrote one;
	// the front end prefers t("errors."+code) and shows Error as the detail.
	ErrorInfo *model.ErrorInfo `json:"error_info,omitempty"`
	// StaleInfo is the decoded stale_reason of a stale job (v2 P14.3).
	StaleInfo *model.StaleReasonCode `json:"stale_info,omitempty"`
	// NoteInfo / WarningInfo: coded forms of Note and of a done-with-warning
	// Error (job.stats.note_info / warning_info, v2 P14.3).
	NoteInfo       *model.ErrorInfo `json:"note_info,omitempty"`
	WarningInfo    *model.ErrorInfo `json:"warning_info,omitempty"`
	AllowedActions []string         `json:"allowed_actions"`
	// WaitingFor is set only for pending/queued stages (SPEC §9.5).
	WaitingFor *string `json:"waiting_for,omitempty"`
	// Note: human-readable reason shown in the hover card, e.g.
	// "首次运行，正在下载模型" (SPEC §17.2) or why rerun is unavailable (SPEC §17.5).
	Note       *string    `json:"note,omitempty"`
	StartedAt  *string    `json:"started_at"`
	FinishedAt *string    `json:"finished_at"`
	DurationMS *int64     `json:"duration_ms"`
	Stats      UsageStats `json:"stats"`
	// v1.1
	DisplayStatus string      `json:"display_status"` // status, or "outdated" (A2.1)
	StaleReason   *string     `json:"stale_reason"`   // A2.2, stale jobs only
	Model         *StageModel `json:"model"`          // A4 snapshot summary
	// LLM and LastActivityAt (v3.2.x P1-c, lead freeze): job.stats.llm and
	// job.stats.last_activity_at — the attempts of the run's LLM requests
	// and when the run last showed life; absent for jobs without them.
	LLM            *model.LLMStats `json:"llm,omitempty"`
	LastActivityAt *string         `json:"last_activity_at,omitempty"`
	// ASRTiming is the asr worker's timing of the last successful run
	// (job.stats.asr_timing, SPEC-v2 I7 / I12: the hover card's transcription
	// time, audio length and RTF); asr stages only, absent without one.
	ASRTiming *model.ASRTiming `json:"asr_timing,omitempty"`
	// Checkpoint (power management, docs/benchmarks/power-checkpoint.md
	// §6.8 item 2): a queued asr stage that a power interrupt put back in
	// line (job.stats.asr_resume) and whose worker checkpoint belongs to
	// this job — how much of the lecture is already transcribed, where the
	// next attempt continues. Progress stays the job's own (0 while
	// queued); absent in every other case.
	Checkpoint *ASRCheckpointView `json:"checkpoint,omitempty"`
}

// ASRCheckpointView is StageView.checkpoint.
type ASRCheckpointView struct {
	ResumeMS   int64   `json:"resume_ms"`   // the checkpoint's resume_ms: transcribed up to here
	DurationMS int64   `json:"duration_ms"` // Lecture.duration_ms, the length the share is taken of
	Progress   float64 `json:"progress"`    // ResumeMS / DurationMS, in (0, 1]
}

type UsageStats struct {
	PromptTokens     int64    `json:"prompt_tokens"`
	CacheHitTokens   int64    `json:"cache_hit_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	EstimatedCost    *float64 `json:"estimated_cost"` // null when no price configured
	// Remote (stage views only): the sub-step of an asr job running on
	// colab:remote (SPEC-v3 V14.5 hover card); omitted otherwise.
	Remote *model.RemoteStats `json:"remote,omitempty"`
	// PendingSentences (stage views and job events only; v3.2.x P1, lead
	// freeze 2026-11-11): job.stats.pending_sentences of a translate job —
	// sentences the run left without a usable translation; omitted when 0.
	PendingSentences int `json:"pending_sentences,omitempty"`
	// MixedModels (stage views and job events only; v3.2.x P1-b):
	// job.stats.mixed_models of a translate job — the models the lecture's
	// current translations come from ("provider name / model", first use
	// first) when they are more than one; omitted otherwise.
	MixedModels []string `json:"mixed_models,omitempty"`
}

type LectureCounts struct {
	Sentences      int `json:"sentences"`
	PatchesPending int `json:"patches_pending"`
	ZhOK           int `json:"zh_ok"`
	ZhQAFailed     int `json:"zh_qa_failed"`
	ZhStale        int `json:"zh_stale"`
}

type ExportFile struct {
	Name string `json:"name"` // file name under export/
	URL  string `json:"url"`  // /api/lectures/{id}/export/{name}
	Size int64  `json:"size"`
}

// Lecture is GET /api/lectures/{id} and each item of GET /api/courses/{id}/lectures.
type Lecture struct {
	ID        string        `json:"id"`
	CourseID  string        `json:"course_id"`
	Title     string        `json:"title"`
	CreatedAt string        `json:"created_at"`
	Stages    []StageView   `json:"stages"`
	Counts    LectureCounts `json:"counts"`
	CanDelete bool          `json:"can_delete"`
	// Reviewable is false while repair or translate is running (SPEC §4.5, §9.7).
	Reviewable bool         `json:"reviewable"`
	Exports    []ExportFile `json:"exports"`
	Usage      UsageStats   `json:"usage"` // sum over the lecture's current jobs (SPEC §17.7)
	// UsageTotal (v3.2.x r7, T25 S1): every job the lecture ever had —
	// retired ones (reruns, updates, retranslations), auxiliary tasks, every
	// attempt — priced like Usage. Only in GET /api/lectures/{id}?usage_total=1
	// (lecture_usage.go); left out of every other response and when the
	// store cannot read it.
	UsageTotal *UsageStats `json:"usage_total,omitempty"`
	// v1.1
	Seq            int      `json:"seq"`
	Label          string   `json:"label"`       // "L03" (A11.3); v2 P13 and lecture kinds: see CiteLabel
	LaunchedAt     *string  `json:"launched_at"` // null = draft (A3)
	Draft          bool     `json:"draft"`
	StaleText      *string  `json:"stale_text"`      // A2.2 "{原因}，需要更新：{阶段}"; null when nothing is stale
	LectureActions []string `json:"lecture_actions"` // model.LectureAction*
	DurationMS     *int64   `json:"duration_ms"`     // media length (audio.wav once prep is done; else the last sentence's end), null when neither exists
	// v2 P1, P8.1: Playable is true once asr is done (the player shows English
	// only where translate has not finished); Stream describes stream.mp4.
	Playable bool        `json:"playable"`
	Stream   *StreamInfo `json:"stream,omitempty"`
	// v2 P4.2: HasWords is true when the transcript has word timestamps
	// (some sentence has words). Without them the subtitles are timed by
	// character position, and lecture_actions offers fill_words.
	HasWords bool `json:"has_words"`
	// v2 P13: calendar numbering, titles and blurbs. Label is now the P13.4
	// number ("W02L1", "W??" when it cannot be computed); Seq the global
	// order by recorded_at.
	// CiteLabel is the label used in citations (P13.6): Label, or for an
	// unnumbered lecture "W??-{id prefix}" unique in its course (calendar.CiteLabels).
	CiteLabel        string  `json:"cite_label"`
	Week             int     `json:"week"`                       // teaching week of any kind (lecture kinds, K1); 0 when it cannot be computed
	N                int     `json:"n"`                          // the number in Label: session kinds within the week and kind, material kinds within the course and kind; 0 without a number
	Numbered         bool    `json:"numbered"`                   // has a number (material kinds also without a week); false → "W??"
	CalendarHint     string  `json:"calendar_hint,omitempty"`    // "term_start" | "recorded_at" | "skipped_week" | "before_term" when not numbered
	SameDayByName    bool    `json:"same_day_by_name,omitempty"` // "同日顺序按文件名推测"
	RecordedAt       *string `json:"recorded_at"`                // local "YYYY-MM-DDTHH:MM"
	RecordedHasTime  bool    `json:"recorded_has_time"`          // false: date only (00:00 placeholder)
	RecordedAtSource string  `json:"recorded_at_source"`         // none | metadata | filename | user
	TitleZh          *string `json:"title_zh"`
	TitleSource      string  `json:"title_source"` // filename | llm | user
	BlurbEn          *string `json:"blurb_en"`
	BlurbZh          *string `json:"blurb_zh"`
	OriginalFilename string  `json:"original_filename"`
	// Stale is the structured form of StaleText (v2 P14.3); null = nothing stale.
	Stale *StaleSummary `json:"stale,omitempty"`
	// Aux lists the lecture's auxiliary tasks that exist (title, chapters,
	// stream; P13.5): status and allowed_actions (retry) for the hint shown
	// on the lecture page when one failed. Never part of the pipeline bar.
	Aux []StageView `json:"aux,omitempty"`
	// Lecture kinds (types_kinds.go, .claude/kinds-brief.md): the effective
	// kind (calendar.EffectiveKind, computed on every read) and where it came
	// from. Label and CiteLabel are numbered per kind (W02T1, P1); Week is
	// the teaching week of any kind (0 = none) and N the number in Label.
	// KindN is the position among the lectures of the same kind in the same
	// teaching week (material kinds without a week: in the course),
	// KindCount their number; ShowN is the course's numbering mode applied
	// ("第 n 节").
	Kind       string `json:"kind"`
	KindSource string `json:"kind_source"` // KindSource*
	KindN      int    `json:"kind_n,omitempty"`
	KindCount  int    `json:"kind_count,omitempty"`
	ShowN      bool   `json:"show_n,omitempty"`
	// Materials are the slide files this lecture uses (bound to it, to its
	// teaching week, course-wide; state.MaterialSetOf), never null.
	Materials []MaterialUse `json:"materials"`
	// Angles are the lecture's extra camera angles (the extension's "双机位",
	// handlers_angles.go): [{"name": "angle2"}], served by GET
	// /api/lectures/{id}/angles/{name}; left out when there is none.
	Angles []Angle `json:"angles,omitempty"`
	// WeekOverride: the user placed the lecture in a teaching week
	// (course-ux, types_course_ux.go); absent when it has none.
	WeekOverride *WeekOverride `json:"week_override,omitempty"`
}

// Angle is one extra camera angle of a lecture: a video-only MP4 (the
// lecture's media keeps the audio).
type Angle struct {
	Name string `json:"name"` // "angle2"
}

type Course struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	CreatedAt       string      `json:"created_at"`
	GlossaryVersion int         `json:"glossary_version"`
	Syllabus        *Syllabus   `json:"syllabus"`
	Stages          []StageView `json:"stages"` // glossary_seed (empty when no syllabus)
	Materials       []Material  `json:"materials"`
	TermCount       int         `json:"term_count"`
	LectureCount    int         `json:"lecture_count"`
	// v1.1
	Code              *string  `json:"code"`
	DraftCount        int      `json:"draft_count"`
	StaleLectureCount int      `json:"stale_lecture_count"`
	PatchesPending    int      `json:"patches_pending"`
	RunningCount      int      `json:"running_count"`
	FailedCount       int      `json:"failed_count"` // v3.2.x r7 (T24 S2): launched lectures with a failed stage, no time window
	LastActivityAt    string   `json:"last_activity_at"`
	CourseActions     []string `json:"course_actions"` // CourseAction* (A12.2)
	// GlossaryPending is true while glossary_seed or a material_terms is not
	// done: launching now makes asr wait (A3.2 warning).
	GlossaryPending bool `json:"glossary_pending"`
	// v2 G3.5: the glossary janitor. Stages also lists glossary_review when
	// the course has that job.
	Janitor JanitorStatus `json:"janitor"`
	// v2 P13.2–P13.4: course calendar and the filename rule.
	TermStart    *string       `json:"term_start"` // Monday of week 1, "YYYY-MM-DD"
	SkipWeeks    []string      `json:"skip_weeks"` // Mondays, never null
	FilenameRule *FilenameRule `json:"filename_rule"`
	// FilenameProposal: a generated rule waits for 应用 / 编辑 / 忽略 (P13.3.3:
	// "已识别文件名中的录制时间格式，查看结果"). FilenameUndo: a rule was
	// applied automatically (首次例外) and can be undone.
	FilenameProposal bool       `json:"filename_proposal"`
	FilenameUndo     bool       `json:"filename_undo"`
	FilenameTask     *StageView `json:"filename_task,omitempty"` // the filename_rule aux job, when one exists
	// CiteExported: a citation JSON was exported before, so changing numbers
	// asks for confirmation (P13.6).
	CiteExported bool `json:"cite_exported"`
	// Numbering is the course's numbering mode (types_kinds.go Numbering*):
	// whether lecture rows show "第 n 节". Display only.
	Numbering string `json:"numbering"`
	// RenumberNotices: kinds the title task set by itself changed citation
	// numbers of other lectures after a citation export (code
	// calendar.renumbered_auto, kind note; oldest first). Shown on the
	// course page until dismissed (DELETE /api/courses/{id}/renumber-notices).
	RenumberNotices []RenumberNotice `json:"renumber_notices,omitempty"`
}

type Syllabus struct {
	Filename string `json:"filename"`
	Chars    int    `json:"chars"`
	// Formatted is true when SYLLABUS.md exists (A8.2).
	Formatted bool `json:"formatted"`
}

type Material struct {
	ID        string      `json:"id"`
	CourseID  string      `json:"course_id"`
	LectureID *string     `json:"lecture_id"`
	Filename  string      `json:"filename"`
	Pages     int         `json:"pages"`
	CreatedAt string      `json:"created_at"`
	Stages    []StageView `json:"stages"`
	// CanDelete is a lead addition (NOTES.md): false while a material stage runs.
	CanDelete bool `json:"can_delete"`
	// Bind is what the slides apply to (types_kinds.go; LectureID stays set
	// for scope lecture). WeekSuggested is the teaching week guessed from the
	// file name ("Week3-…", "W03"; calendar.WeekFromFilename), null when none;
	// an upload without lecture_id is bound to it.
	Bind          *MaterialBind `json:"bind"`
	WeekSuggested *int          `json:"week_suggested"`
	// TextWarning is TextWarningIncomplete ("text_incomplete") when many of
	// the extracted pages are empty (SPEC-v3 V6, material_text.go), so the
	// card says the text may not have been extracted in full; omitted
	// otherwise.
	TextWarning string `json:"text_warning,omitempty"`
}

type Term struct {
	ID      string `json:"id"`
	En      string `json:"en"`
	Zh      string `json:"zh"`
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	Freq    int    `json:"freq"`
	// v2 (SPEC-v2 G1, G3.5, G6)
	Tier        string `json:"tier"`         // enforce | reference
	Locked      bool   `json:"locked"`       // edited by the user; the janitor leaves it alone
	CommonWord  bool   `json:"common_word"`  // G2 pre-screen
	JanitorNote string `json:"janitor_note"` // "" = never changed by the janitor
	ZhSource    string `json:"zh_source"`    // extractor | janitor | user
}

type LowConfWord struct {
	I int    `json:"i"`
	W string `json:"w"`
}

type PatchRef struct {
	ID     string `json:"id"`
	Class  string `json:"class"`
	Review string `json:"review"`
	Reason string `json:"reason"`
}

// Sentence is one item of GET /api/lectures/{id}/sentences.
type Sentence struct {
	Idx          int           `json:"idx"`
	StartMS      int64         `json:"start_ms"`
	EndMS        int64         `json:"end_ms"`
	ASRText      string        `json:"asr_text"`
	RepairedText string        `json:"repaired_text"`
	HumanText    *string       `json:"human_text"`
	Text         string        `json:"text"`
	LowConfWords []LowConfWord `json:"low_conf_words"`
	Zh           *string       `json:"zh"`
	ZhStatus     string        `json:"zh_status"`
	QAError      *string       `json:"qa_error"`
	// QAErrorInfo is QAError coded (v2 P14.3, qa.ParseReason): the first
	// failure, the others in params.extra; null for a reason qa did not write.
	QAErrorInfo *model.ErrorInfo `json:"qa_error_info,omitempty"`
	Patch       *PatchRef        `json:"patch"`
}

type SentencePage struct {
	Total     int        `json:"total"`
	Offset    int        `json:"offset"`
	Sentences []Sentence `json:"sentences"`
}

// Patch is each item of GET /api/lectures/{id}/patches and the response of
// POST /api/patches/{id}/review.
type Patch struct {
	ID           string  `json:"id"`
	LectureID    string  `json:"lecture_id"`
	SentenceIdx  int     `json:"sentence_idx"`
	Before       string  `json:"before"`
	After        string  `json:"after"`
	Class        string  `json:"class"`
	Reason       string  `json:"reason"`
	Review       string  `json:"review"`
	EditedText   *string `json:"edited_text"`
	LowConfCount int     `json:"low_conf_count"` // for "最可疑优先" ordering (SPEC §9.7)
	StartMS      int64   `json:"start_ms"`
	EndMS        int64   `json:"end_ms"`
	PrevText     *string `json:"prev_text"` // previous sentence text (grey context)
	NextText     *string `json:"next_text"`
	// Retranslated is the number of sentences marked stale by this review
	// action (only set in the review response).
	Retranslated *int `json:"retranslated,omitempty"`
}

// SystemCheck is one row of the diagnostics page (SPEC §17.1).
type SystemCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | error
	Detail string `json:"detail"`
	Hint   string `json:"hint"`
	// v2 P14.3: Detail and Hint as codes (catalog area "check"; the front
	// end shows t("errors."+code, params)). DetailInfo is nil when Detail is
	// a program's own output (ffmpeg -version, python --version, the
	// self-test line), which is shown as is; check.checking marks a check
	// that is still running.
	DetailInfo *model.ErrorInfo `json:"detail_info,omitempty"`
	HintInfo   *model.ErrorInfo `json:"hint_info,omitempty"`
}

type GPUInfo struct {
	TotalMB        int  `json:"total_mb"`
	FreeMB         int  `json:"free_mb"`
	Paused         bool `json:"paused"`
	SlotsUsed      int  `json:"slots_used"`
	SlotsMax       int  `json:"slots_max"`
	EstimatedJobMB int  `json:"estimated_job_mb"`
	// EstimateRaised: estimated_job_mb was raised in memory after a
	// transcription ran out of GPU memory (sched.GPUStatus); a restart or
	// another model or precision on cuda:0 drops it. Omitted when false.
	EstimateRaised bool `json:"estimate_raised,omitempty"`
}

type APIInfo struct {
	Inflight int `json:"inflight"`
	Max      int `json:"max"`
	RPMLimit int `json:"rpm_limit"`
}

type DiskInfo struct {
	FreeMB  int64            `json:"free_mb"`
	DataMB  int64            `json:"data_mb"`
	Courses map[string]int64 `json:"courses_mb"` // per-course usage (SPEC §17.5)
	// StreamMB is the part of the usage taken by the playback files (every
	// lecture's stream.mp4), shown on its own (SPEC-v2 P8.1); included in
	// DataMB and CoursesMB.
	StreamMB int64 `json:"stream_mb"`
}

// System is GET /api/system.
type System struct {
	Checks     []SystemCheck      `json:"checks"`
	GPU        GPUInfo            `json:"gpu"`
	API        map[string]APIInfo `json:"api"`
	UsageToday UsageStats         `json:"usage_today"`
	BudgetHit  bool               `json:"budget_hit"`
	Disk       DiskInfo           `json:"disk"`
	Config     any                `json:"config"` // read-only, API keys masked

	// SPEC-v2 I10 (phase 3b freeze, types_intel.go): the devices table, the
	// power state and the OpenVINO environment. Omitted by builds before
	// 3b (the example files have none).
	Devices  []ASRDeviceView `json:"devices,omitempty"`
	Power    *PowerView      `json:"power,omitempty"`
	OpenVINO *OpenVINOInfo   `json:"openvino,omitempty"`
}

// SSE payloads (SPEC §8.2).
type JobEvent struct {
	Scope    string  `json:"scope"`
	ScopeID  string  `json:"scope_id"`
	Stage    string  `json:"stage"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	// v3.2.x P1-c (lead freeze; SPEC §8.2 as the user revised it on
	// 2026-11-10): optional, present for a running job whose statistics the
	// scheduler sent with the event — the same values as StageView.stats,
	// llm and last_activity_at, so the card follows them without a refetch.
	Stats          *UsageStats     `json:"stats,omitempty"`
	LLM            *model.LLMStats `json:"llm,omitempty"`
	LastActivityAt *string         `json:"last_activity_at,omitempty"`
}

type ChangedEvent struct {
	Kind string `json:"kind"` // lecture|course|sentences|patches|terms|system
	ID   string `json:"id"`
}

// StaleSummary is Lecture.Stale (v2 P14.3): the distinct stale reasons in
// pipeline order (a stale job without a recorded reason counts as
// input_changed) and the stale stages in pipeline order.
type StaleSummary struct {
	Reasons []model.StaleReasonCode `json:"reasons"`
	Stages  []string                `json:"stages"`
}
