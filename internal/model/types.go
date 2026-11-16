// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import (
	"path/filepath"
	"time"
)

// Rows of the tables in migrations/001_init.sql. Times are UTC RFC3339 strings.

type Course struct {
	ID              string
	Name            string
	GlossaryVersion int
	CreatedAt       string
	Code            *string // v1.1 A5.1 course.code, e.g. "CSC1003"; nil = none

	// v2 P13.2, P13.3.1 (migrations/006_v2.sql).
	TermStart    *string  // Monday of teaching week 1, "YYYY-MM-DD"; nil = not set
	SkipWeeks    []string // Mondays ("YYYY-MM-DD") of calendar weeks not counted; never nil when read
	FilenameRule *string  // JSON calendar.FilenameRule; nil = none

	// Lecture kinds (migrations/007_lecture_kinds.sql).
	Numbering string // Numbering* ("" is read and written as auto)
}

type Syllabus struct {
	CourseID string
	Filename string
	Path     string
	Text     *string // nil until extracted
}

type Lecture struct {
	ID        string
	CourseID  string
	Title     string
	MediaPath string
	CreatedAt string
	// LaunchedAt is nil while the lecture is a draft (v1.1 A3.1).
	LaunchedAt *string
	// Seq is the number of the lecture within its course (v1.1 A11.3).
	// CreateLecture fills it with max+1 when it is 0. v2 P13.2: it is the
	// global order by recorded_at, computed and refreshed, never user-edited.
	Seq int

	// v2 P13.2, P13.3.1 (migrations/006_v2.sql).
	RecordedAt       *string // local wall-clock time "YYYY-MM-DDTHH:MM" (calendar.FormatRecordedAt); nil = unknown
	RecordedAtSource string  // RecordedAt* ("" is read and written as none)
	TitleZh          *string
	TitleSource      string // TitleSource* ("" is read and written as filename)
	BlurbEn          *string
	BlurbZh          *string
	OriginalFilename string // the uploaded file's name ("" for lectures created before v2)

	// Lecture kinds (migrations/007_lecture_kinds.sql). The stored kind is
	// what was decided (user > filename > llm); the effective kind used for
	// numbering is computed on read (calendar.EffectiveKind: unknown or
	// empty → lecture; the past-lecture rule).
	Kind       string // lecture_kind.id ("" = NULL: not decided)
	KindSource string // KindSource* ("" is read and written as none)

	// WeekOverride is the lecture's row of lecture_week_override (course-ux,
	// migration 011), read with the lecture by GetLecture and ListLectures;
	// nil = none. Written only through Store.SetWeekOverride /
	// DeleteWeekOverride (CreateLecture ignores it).
	WeekOverride *WeekOverride
}

type Material struct {
	ID        string
	CourseID  string
	LectureID *string // bound to that lecture; nil = not bound to a lecture
	Filename  string
	Path      string
	PagesMD   []string // nil until extracted; JSON array in the DB
	CreatedAt string

	// BindWeek is material.bind_week (migration 007): bound to that teaching
	// week (1–MaxBindWeek). Never set together with LectureID (the Store
	// refuses it). Both nil = course-wide. See BindScope.
	BindWeek *int
}

type Term struct {
	ID       string
	CourseID string
	En       string
	EnKey    string
	Zh       string
	Source   string
	Enabled  bool
	Freq     int

	// v2 (SPEC-v2 G1, G6; migrations/005_glossary_tiers.sql).
	Tier        string // TierEnforce | TierReference ("" is read and written as enforce)
	Locked      bool   // edited by the user: the janitor never acts on it
	CommonWord  bool   // G2 pre-screen result
	JanitorNote string // reason of the janitor's latest change ("" = NULL)
	ZhSource    string // ZhSourceExtractor | ZhSourceJanitor | ZhSourceUser ("" = extractor)
}

type LowConfWord struct {
	I int    `json:"i"`
	W string `json:"w"`
}

type Sentence struct {
	LectureID    string
	Idx          int
	StartMS      int64
	EndMS        int64
	ASRText      string
	LowConfWords []LowConfWord
	RepairedText string
	HumanText    *string
	Text         string // written only by RecomputeText
	Zh           *string
	ZhStatus     string
	ZhHash       *string
	QAError      *string
	// Words are the word timestamps of v2 P4.2 (sentence.words); nil = none.
	Words []Word
}

type Patch struct {
	ID          string
	LectureID   string
	SentenceIdx int
	Before      string
	After       string
	Class       string
	Reason      string
	Review      string
	EditedText  *string
	CreatedAt   string
}

type Summary struct {
	LectureID string
	Text      string
	InputHash string
}

type Job struct {
	ID         string
	Scope      string
	ScopeID    string
	Stage      string
	Status     string // written only by state.Transition
	InputHash  *string
	Attempt    int
	Error      *string
	Progress   float64
	IsCurrent  bool
	Stats      JobStats
	CreatedAt  string
	StartedAt  *string
	FinishedAt *string
	// ConfigSnapshot is job.config_snapshot (v1.1 A4.1), raw JSON: an
	// LLMSnapshot for summary/repair/translate, an ASRSnapshot for asr, "{}"
	// for prep/export, drafts and course/material jobs. Use the helpers in
	// snapshot.go to read it. "" is treated as "{}".
	ConfigSnapshot string
	// StaleReason is job.stale_reason (v1.1 A2.2), set together with done →
	// stale; a glossary reason is rewritten by later glossary edits
	// (Store.SetStaleReason, NOTES "v1.1 冻结与裁决" 9).
	StaleReason *string
}

// JobStats is the JSON stored in job.stats (SPEC §12, §17.7).
type JobStats struct {
	PromptTokens     int64  `json:"prompt_tokens,omitempty"`
	CompletionTokens int64  `json:"completion_tokens,omitempty"`
	CacheHitTokens   int64  `json:"cache_hit_tokens,omitempty"`
	Requests         int64  `json:"requests,omitempty"`
	DurationMS       int64  `json:"duration_ms,omitempty"`
	VRAMPeakMB       int64  `json:"vram_peak_mb,omitempty"`
	PromptTermsUsed  int    `json:"prompt_terms_used,omitempty"`
	Note             string `json:"note,omitempty"`   // e.g. failed repair chunks (SPEC §6.4)
	Status           string `json:"status,omitempty"` // transient worker status, e.g. "downloading_model" (SPEC §17.2)
	// Warning is a lead addition (Gate B, NOTES.md): a stage that finishes with
	// a partial problem (SPEC §6.4: repair chunks without patches) sets it; the
	// scheduler writes it into job.error when the job becomes done.
	Warning       string  `json:"warning,omitempty"`
	EstimatedCost float64 `json:"estimated_cost,omitempty"`

	// v3.2.x additions (lead freeze, .claude/v32x-brief.md). Written by the
	// scheduler only: a stage's UpdateStats cannot change LLM and
	// LastActivityAt (like the token fields).
	// LLM: the requests of the run, attempt by attempt (P1-c).
	LLM *LLMStats `json:"llm,omitempty"`
	// LastActivityAt: when the run last showed life (a request event, a
	// usage report, a progress call, a stats update), RFC 3339 UTC.
	LastActivityAt string `json:"last_activity_at,omitempty"`
	// PendingSentences (translate, written by the stage, P1): sentences left
	// without a translation after the business retries (zh "", qa_failed);
	// the stage is done with a warning.
	PendingSentences int `json:"pending_sentences,omitempty"`
	// MixedModels (translate, v3.2.x P1-b, Q10a): the models the lecture's
	// current translations come from, as "provider name / model", in the
	// order they were first used, without duplicates. Empty = one model.
	// Written by state (RetranslateSentenceWith, when a pending sentence is
	// sent to the current setting's model), carried to the next translate
	// job by an incremental run, and dropped with the translations (a retry
	// with another model, a rerun). Stages cannot change it (like the token
	// fields).
	MixedModels []string `json:"mixed_models,omitempty"`

	// v1.1 additions.
	// ASRPrompt is the initial_prompt the asr run actually sent (A1).
	ASRPrompt string `json:"asr_prompt,omitempty"`
	// GlossaryVersion is course.glossary_version when the run started
	// (asr, repair, translate); used for "术语表已更新（v3 → v4）" (A2.2).
	// nil = not recorded (jobs of v1); 0 is a real version.
	GlossaryVersion *int `json:"glossary_version,omitempty"`
	// HashVersion is the input_hash formula of the run. asr runs of v1.1
	// write 2 (A1: wav + ASR config, no prompt); an asr job recorded with an
	// older formula is never compared (its InputHash reports ""). Phase 3b
	// (SPEC-v2 I6 revising A1) writes 3: wav + language only
	// (ctxbuild.ASRInputHashV3); version 2 jobs keep their formula.
	HashVersion int `json:"hash_version,omitempty"`

	// v2 P14.3: the coded forms of Note and Warning (the front end prefers
	// them and translates "notes.{code}" / "warnings.{code}"); Note and
	// Warning keep the Chinese text as the detail until the i18n sweep ends.
	NoteInfo    *ErrorInfo `json:"note_info,omitempty"`
	WarningInfo *ErrorInfo `json:"warning_info,omitempty"`

	// ASRTiming is the asr worker's timing (SPEC-v2 I7, phase 3b freeze; nil
	// before 3b and for other stages). After a resume from a checkpoint the
	// transcribe time adds up every attempt and the audio is the whole
	// lecture; load, first compile and peak memory are the last attempt's
	// (power management, NOTES "实机验证后的修复：pipeline-builder").
	ASRTiming *ASRTiming `json:"asr_timing,omitempty"`

	// Power management (v2-step20 freeze, power-brief §3.4), asr jobs only,
	// written by the scheduler:
	// PowerInterrupts counts the requeues caused by the power policy
	// (migrate or suspend); CountedAttempts subtracts it from job.attempt.
	PowerInterrupts int `json:"power_interrupts,omitempty"`
	// ASRResume is the device the last power interrupt happened on (nil
	// when none, cleared when the job is done).
	ASRResume *ASRResume `json:"asr_resume,omitempty"`
	// PowerEvent is the last power-policy event (stage card note).
	PowerEvent *PowerEvent `json:"power_event,omitempty"`
	// ASRPriorTranscribeMS (asr jobs, written by the asr stage; power
	// management real-machine verification fix): the transcription time in
	// ms of this job's earlier attempts whose checkpoint the next attempt
	// continues (power interrupts, cancels, failures, shutdown), each
	// measured from the worker's first PROGRESS line to its exit, kept up
	// to date while an attempt runs. A run that resumed from the checkpoint
	// adds it to ASRTiming.TranscribeMS (and reports the whole lecture as
	// ASRTiming.AudioMS); the successful run clears it (0 = absent: an
	// uninterrupted run's stats are unchanged).
	ASRPriorTranscribeMS int64 `json:"asr_prior_transcribe_ms,omitempty"`

	// SPEC-v3 V14 (Colab remote transcription; v14-colab-integrate), asr
	// jobs only:
	// Remote is the remote run's sub-step while the job runs on
	// colab:remote (the stage writes it, the scheduler clears it when the
	// run ends); the hover card shows it (stats.remote of the stage view).
	Remote *RemoteStats `json:"remote,omitempty"`
	// RemotePin: the user chose "改用 Colab 处理" (V14.4): the job runs on
	// colab:remote only, from the start. Cleared when Colab hands the
	// lecture back (RemoteFallback).
	RemotePin bool `json:"remote_pin,omitempty"`
	// RemoteRequeues counts the requeues of a lecture Colab handed back
	// (IsFallback / IsRequeue); CountedAttempts subtracts it like
	// PowerInterrupts (a Colab problem never uses up the retries).
	RemoteRequeues int `json:"remote_requeues,omitempty"`
	// RemoteFallback is why Colab handed the lecture back the last time
	// (the colab.* error); the scheduler never sends that job to Colab
	// automatically again (only "改用 Colab 处理" does).
	RemoteFallback *ErrorInfo `json:"remote_fallback,omitempty"`

	// ASRMode (textrepair-int, asr jobs only): ASRModeTextRepair for a
	// "修复断句" run (the asr rerun with text_only: the stored transcript is
	// repaired, nothing is transcribed); "" = a transcription. Written when
	// the rerun creates the job (state.RerunOpts.TextRepair), kept by its
	// retries.
	ASRMode string `json:"asr_mode,omitempty"`
}

// Data directory layout (SPEC §1).

func CourseDir(dataDir, courseID string) string {
	return filepath.Join(dataDir, "courses", courseID)
}

func LectureDir(dataDir, courseID, lectureID string) string {
	return filepath.Join(CourseDir(dataDir, courseID), "lectures", lectureID)
}

func MaterialDir(dataDir, courseID, materialID string) string {
	return filepath.Join(CourseDir(dataDir, courseID), "materials", materialID)
}

func SyllabusDir(dataDir, courseID string) string {
	return filepath.Join(CourseDir(dataDir, courseID), "syllabus")
}

// Fixed file names inside a lecture directory.
const (
	AudioFile     = "audio.wav"
	ExportDirName = "export"
	// ASRCheckpointFile is the asr worker's checkpoint (power management,
	// v2-step20; format: devices.Checkpoint). The worker writes it through
	// ASRCheckpointFile+".tmp" and os.replace, so §4.7 recovery's *.tmp
	// cleanup never removes a complete checkpoint.
	ASRCheckpointFile = "asr.ckpt.json"
)

// ---------------------------------------------------------------------------
// v1.1 settings rows (SPEC-v1.1 A5). FROZEN by the lead.
// ---------------------------------------------------------------------------

// Provider is one row of the provider table (A5.1). APIKey is the plain key;
// it never leaves the back end (A5.2): API responses carry KeyHint instead.
type Provider struct {
	ID             string
	Name           string
	Type           string // ProviderOpenAI | ProviderAnthropic
	BaseURL        string
	APIKey         string
	MaxConcurrency int
	RPM            int
	BalanceAPI     string   // BalanceNone | BalanceDeepSeek
	PriceInput     *float64 // per million tokens, nil = not set
	PriceCacheHit  *float64
	PriceOutput    *float64
	Currency       *string
	CreatedAt      string
	// v2 P12.2 / D3: time-of-day pricing, raw JSON tariff.Tariff; nil = none.
	Tariff *string
	// v2 P12.6: dispatch new requests only off-peak (only meaningful with a Tariff).
	OffpeakOnly bool
}

// HasPrices reports whether all three prices are configured.
func (p Provider) HasPrices() bool {
	return p.PriceInput != nil && p.PriceCacheHit != nil && p.PriceOutput != nil
}

// EstimateCost applies the provider's prices to a usage (SPEC §17.7: input to
// non-cached prompt tokens, cache_hit to cached ones, output to completion).
// ok is false when the provider has no prices.
func (p Provider) EstimateCost(promptTokens, cacheHitTokens, completionTokens int64) (cost float64, ok bool) {
	if !p.HasPrices() {
		return 0, false
	}
	miss := promptTokens - cacheHitTokens
	if miss < 0 {
		miss = 0
	}
	in, hit, out := *p.PriceInput, *p.PriceCacheHit, *p.PriceOutput
	return (float64(miss)*in + float64(cacheHitTokens)*hit + float64(completionTokens)*out) / 1e6, true
}

// KeyHint formats a key as "sk-…abcd" (A5.2); "" for an empty key.
func KeyHint(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "sk-…"
	}
	return "sk-…" + key[len(key)-4:]
}

// RoleSetting is one row of role_setting (A5.1). Params is raw JSON
// (RoleParams).
type RoleSetting struct {
	Role       string
	ProviderID *string
	Model      *string
	Params     string
}

// RoleParams is role_setting.params and the params of an LLMSnapshot.
// In a snapshot both are resolved (Temperature always set; Window set for
// translate only; Thinking set only when it is not off).
type RoleParams struct {
	Temperature *float64 `json:"temperature,omitempty"`
	Window      *int     `json:"window,omitempty"` // translate only: context window x
	// Thinking is the role's "深度思考" level (thinking.go): ThinkingOff
	// (the default; nil, "" and "off" all mean off), ThinkingLow,
	// ThinkingHigh or ThinkingMax. Stored and resolved params leave it out
	// when off, so a role or snapshot without thinking is byte for byte
	// what it was before the option existed.
	Thinking *string `json:"thinking,omitempty"`
	// MaxTokens (v3.2.x D1, lead freeze): the output limit the user set for
	// the role, or for one retry (LLMSnapshot.Source manual). nil = automatic:
	// the client's default for the role and thinking level. A snapshot holds
	// it only when the user set one, so snapshots without it are byte for
	// byte what they were. Compared by sameParams; never part of a hash.
	MaxTokens *int `json:"max_tokens,omitempty"`
	// Batch (v3.2.x P2, lead freeze; translate only): target sentences per
	// request, 1–20. nil = 1 (one request per sentence, the behaviour before
	// the option existed; a snapshot written before it has none). Not
	// compared by sameParams and not part of a hash: like the prompt
	// version it changes how the stage asks, not what it reads. A snapshot
	// of the current settings for a Claude model on an Anthropic provider
	// (settings.DefaultBatch) whose role leaves it
	// empty holds settings.DefaultAnthropicBatch (user 2026-11-11), so the
	// default is fixed when the snapshot is written; since Batch is not
	// compared, an older snapshot without it still equals the current
	// setting.
	Batch *int `json:"batch,omitempty"`
}

// UsageDaily is one row of usage_daily (A5.1, A10.3).
type UsageDaily struct {
	ProviderID       string
	Day              string // local date YYYY-MM-DD
	Requests         int64
	PromptTokens     int64 // includes cache-hit tokens (DeepSeek semantics)
	CacheHitTokens   int64
	CompletionTokens int64
	// EstimatedCost is the sum of per-request estimates priced at each
	// request's start time (v2 P12.5), in the provider's currency.
	EstimatedCost float64
}

// ASRSettings is kv settings.asr (A5.1, A5.3 转写).
//
// SPEC-v2 I6/I8 (phase 3b freeze): the per-device configuration moved to
// the asr_device rows. Model, ComputeType, Device and MaxParallel are
// legacy fields from then on: devices.Registry.MigrateLegacy reads them
// once to create cuda:0 (or fw:cpu), and nothing else may read them for a
// run (they stay in the JSON and in the settings validation so an older
// client's PUT keeps working). Language, VRAMReserveMB and VRAMPerJobMB
// stay global. The four fields after VRAMPerJobMB are new: settings.Load
// starts from settings.DefaultASR() and decodes the stored JSON over it, so
// a missing key keeps its default (MultiDevice and BatteryAware default to
// true, DeviceMode to "auto", MemReserveMB to 2048), and PUT
// /api/settings/asr merges the request into the current settings first
// (lead ruling 15).
type ASRSettings struct {
	Model         string `json:"model"`
	ComputeType   string `json:"compute_type"` // auto | float16 | int8_float16
	Device        string `json:"device"`       // cuda | cpu
	Language      string `json:"language"`
	MaxParallel   int    `json:"max_parallel"`
	VRAMReserveMB int    `json:"vram_reserve_mb"`
	// VRAMPerJobMB is the initial per-job estimate of SPEC §17.2 (not on the
	// settings page; imported from config.toml, default 5000). Since r2
	// T-PERF-02 it caps the estimate of the models devices.VRAMTableMB
	// knows (the smaller one is used) and is the estimate of the others.
	VRAMPerJobMB int `json:"vram_per_job_mb"`

	// DeviceMode is "转写设备" (I8): ASRDeviceModeAuto (default) or the id
	// of the one device to use (an existing asr_device row).
	//
	// Power management (v2-step20 freeze, power-brief §3.2): the values are
	// ASRDeviceModeAuto ("自动（电源感知）") and ASRDeviceModeManual ("手动指
	// 定", the devices of ManualDevices, on AC power only); a 3b value naming
	// one device is read as manual [that id] (NormalizeASRDeviceMode, wired
	// into settings by task P4). Until P1/P4 deliver, the 3b meaning above
	// stays in effect.
	DeviceMode string `json:"device_mode"`
	// MultiDevice is "允许多个设备同时转写" (I8, default true): off, at
	// most one pipeline asr job runs at a time over all devices.
	MultiDevice bool `json:"multi_device"`
	// BatteryAware is "电源感知" (I8, default true): on battery, CUDA
	// starts no new job and auto mode uses only the NPU (else the Intel GPU).
	//
	// RETIRED by power management (power-plan §0): on a machine with a
	// battery the power-aware policy always applies; the field is still
	// read and written (old clients, old data) but the policy ignores it
	// and the settings page hides the switch (tasks P1, P5).
	BatteryAware bool `json:"battery_aware"`
	// MemReserveMB is the system memory kept free when admitting a job on
	// an Intel device (lead ruling 8; default 2048, 0..65536).
	MemReserveMB int `json:"mem_reserve_mb"`
	// ManualDevices are the device ids of DeviceMode manual (power
	// management; never a CPU device; empty in auto mode). Omitted when
	// empty, so settings JSON from before stays byte for byte the same; the
	// front end reads a missing list as []. Order carries no meaning (the
	// device priority decides).
	ManualDevices []string `json:"manual_devices,omitempty"`
}

// GeneralSettings is kv settings.general (A5.3 存储与其他).
type GeneralSettings struct {
	KeepOriginalMedia bool `json:"keep_original_media"` // SPEC §17.5, default true
	Notifications     bool `json:"notifications"`       // SPEC §9.5 browser notifications, default true
	// DailyBudget is SPEC §17.7 llm.daily_budget, in the providers' price
	// currency; 0 = no budget.
	DailyBudget float64 `json:"daily_budget"`
	// AutoChapters (v3.2.x P3, lead freeze): queue the chapters task when a
	// lecture's export completes and the lecture has no chapters yet. nil
	// (settings saved before the option) = on; use AutoChaptersOn.
	AutoChapters *bool `json:"auto_chapters,omitempty"`
	// LLMIdleTimeoutS (v3.2.x P1-c, lead freeze): seconds a streamed LLM
	// request may stay without any event before it is cut and retried.
	// nil = DefaultLLMIdleTimeoutS, 0 = no idle timeout; use LLMIdleTimeout.
	LLMIdleTimeoutS *int `json:"llm_idle_timeout_s,omitempty"`
}

// DefaultLLMIdleTimeoutS is the idle timeout of a streamed LLM request when
// the user set none: 90 (180 until the keep-alive during hidden thinking was
// verified against the real API; real acceptance T9: a ping every 30 s, the
// longest gap 30.0 s; approved by the user 2026-11-11, NOTES.md). Only
// Anthropic requests to current Claude models are streamed (llm.streamed),
// so it applies to nothing else. A value the user saved is kept.
const DefaultLLMIdleTimeoutS = 90

// AutoChaptersOn reports whether chapters are generated after an export.
func (g GeneralSettings) AutoChaptersOn() bool { return g.AutoChapters == nil || *g.AutoChapters }

// LLMIdleTimeout returns the idle timeout of a streamed LLM request (0 = none).
func (g GeneralSettings) LLMIdleTimeout() time.Duration {
	s := DefaultLLMIdleTimeoutS
	if g.LLMIdleTimeoutS != nil {
		s = max(*g.LLMIdleTimeoutS, 0)
	}
	return time.Duration(s) * time.Second
}

// ProbedModels is the cached result of "探测模型" (A5.3), kv provider_models:{id}.
type ProbedModels struct {
	Models   []string `json:"models"`
	ProbedAt string   `json:"probed_at"`
	Error    string   `json:"error,omitempty"`
	// ErrorInfo is the coded Error (v2 P14.3, llm.probe.* codes; lead
	// authorized codes-p2 to add it). Probes cached before it have none.
	ErrorInfo *ErrorInfo `json:"error_info,omitempty"`
}
