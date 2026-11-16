// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package model holds the shared constants and plain structs of LectureFlow.
// FROZEN in Phase 0 (SPEC §16.2): changes only by the lead, recorded in NOTES.md.
package model

// Scope of a job (SPEC §3.2 job.scope).
const (
	ScopeCourse   = "course"
	ScopeMaterial = "material"
	ScopeLecture  = "lecture"
)

// Lecture stages, in strict dependency order (SPEC §4.1).
const (
	StagePrep      = "prep"
	StageASR       = "asr"
	StageSummary   = "summary"
	StageRepair    = "repair"
	StageTranslate = "translate"
	StageExport    = "export"
)

// Course-level stages (SPEC §3.2, §4.3).
const (
	StageGlossarySeed    = "glossary_seed"    // scope=course
	StageMaterialExtract = "material_extract" // scope=material
	StageMaterialTerms   = "material_terms"   // scope=material
)

// LectureStages lists the six lecture stages in dependency order.
var LectureStages = []string{StagePrep, StageASR, StageSummary, StageRepair, StageTranslate, StageExport}

// MaterialStages lists the material stages in dependency order.
var MaterialStages = []string{StageMaterialExtract, StageMaterialTerms}

// ScopeOfStage returns the job scope a stage belongs to, or "" if unknown.
func ScopeOfStage(stage string) string {
	switch stage {
	case StagePrep, StageASR, StageSummary, StageRepair, StageTranslate, StageExport:
		return ScopeLecture
	case StageGlossarySeed, StageGlossaryReview, StageFilenameRule:
		return ScopeCourse
	case StageTitle, StageChapters, StageStream:
		return ScopeLecture
	case StageMaterialExtract, StageMaterialTerms:
		return ScopeMaterial
	}
	return ""
}

// Job statuses (SPEC §4.2; DDL CHECK constraint).
const (
	StatusPending   = "pending"
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusStale     = "stale"
)

// User actions on a stage (SPEC §4.5).
const (
	ActionCancel = "cancel"
	ActionRetry  = "retry"
	ActionRerun  = "rerun"
)

// Patch classes (SPEC §6.4).
const (
	ClassAuto   = "auto"
	ClassAbsorb = "absorb"
	ClassDecide = "decide"
)

// Patch review states (SPEC §3.2, §6.4).
const (
	ReviewNone     = "none"
	ReviewPending  = "pending"
	ReviewAccepted = "accepted"
	ReviewRejected = "rejected"
	ReviewEdited   = "edited"
)

// Review actions on a patch (SPEC §6.4, §8).
const (
	ReviewActionAccept = "accept"
	ReviewActionReject = "reject"
	ReviewActionEdit   = "edit"
	ReviewActionReset  = "reset"
)

// Sentence translation status (SPEC §3.2).
const (
	ZhNone     = "none"
	ZhOK       = "ok"
	ZhQAFailed = "qa_failed"
	ZhStale    = "stale"
)

// Term sources (SPEC §3.1).
const (
	SourceSyllabus = "syllabus"
	SourceMaterial = "material"
	SourceReview   = "review"
	SourceManual   = "manual"
)

// waiting_for values (SPEC §9.5). Parameterised values are built with the
// helper functions below; the prefixes are exported for parsing.
const (
	WaitUpstreamPrefix      = "upstream:" // upstream:{stage}
	WaitGlossarySeed        = "glossary_seed"
	WaitMaterialTermsPrefix = "material_terms:" // material_terms:{filename}
	WaitGPUPaused           = "gpu_paused"
	WaitGPUSlot             = "gpu_slot"
	WaitVRAM                = "vram"
	WaitAPISlot             = "api_slot"
	WaitBudget              = "budget"
	WaitRetryBackoffPrefix  = "retry_backoff:" // retry_backoff:{seconds}
	// WaitCPUSlot is a lead addition (NOTES.md): CPU pool full. UI: "等待 CPU 空位".
	WaitCPUSlot = "cpu_slot"
)

func WaitUpstream(stage string) string         { return WaitUpstreamPrefix + stage }
func WaitMaterialTerms(filename string) string { return WaitMaterialTermsPrefix + filename }

// KV keys (SPEC §17.2).
const (
	KVGPUPaused          = "gpu_paused"
	KVVRAMObservedPrefix = "vram_observed:" // vram_observed:{model}:{compute_type}
)

// Error text for jobs interrupted by a restart (SPEC §4.7).
const ErrInterruptedByRestart = "interrupted by restart"

// MaxAutoRetries is the number of automatic retries after a failure (SPEC §4.2).
const MaxAutoRetries = 2

// RetryBackoffSeconds are the delays before automatic retry 1 and 2 (SPEC §4.2).
var RetryBackoffSeconds = []int{10, 60}

// ---------------------------------------------------------------------------
// v1.1 (SPEC-v1.1.md). FROZEN by the lead at the start of v1.1; changes only
// by the lead, recorded in NOTES.md.
// ---------------------------------------------------------------------------

// LLM roles (SPEC §2 [llm.roles]; v1.1 A5.1 role_setting.role).
// material_terms uses the glossary role.
const (
	RoleSyllabusFormat = "syllabus_format" // v1.1 A8.3
	RoleGlossary       = "glossary"
	RoleSummary        = "summary"
	RoleRepair         = "repair"
	RoleTranslate      = "translate"
)

// Roles lists the roles in settings-page order (v1.1 A5.3; v2 G3.1 adds
// glossary_janitor after glossary; phase 2 adds chapters and organizer).
var Roles = []string{RoleSyllabusFormat, RoleGlossary, RoleGlossaryJanitor, RoleSummary, RoleRepair, RoleTranslate, RoleChapters, RoleOrganizer}

// RoleOfStage maps an API stage to its role; non-API stages return "".
func RoleOfStage(stage string) string {
	switch stage {
	case StageGlossarySeed, StageMaterialTerms:
		return RoleGlossary
	case StageGlossaryReview:
		return RoleGlossaryJanitor
	case StageTitle, StageFilenameRule:
		return RoleOrganizer
	case StageChapters:
		return RoleChapters
	case StageSummary:
		return RoleSummary
	case StageRepair:
		return RoleRepair
	case StageTranslate:
		return RoleTranslate
	}
	return ""
}

// DefaultTemperature is the SPEC §12 temperature of a role when the role
// setting leaves it unset: repair 0.1, translate 0.3, others 0.2.
func DefaultTemperature(role string) float64 {
	switch role {
	case RoleRepair:
		return 0.1
	case RoleTranslate:
		return 0.3
	}
	return 0.2
}

// DefaultTranslateWindow is the translate context window x when unset (SPEC §2).
const DefaultTranslateWindow = 3

// WaitNotStarted: the lecture is a draft (launched_at IS NULL, v1.1 A3.1).
// UI: "未启动". Every pending job of a draft reports it.
const WaitNotStarted = "not_started"

// DisplayOutdated is the display-only status of v1.1 A2.1: the job is done
// but an upstream stage of the same scope is stale, pending, queued, running,
// failed or cancelled. Never written to the database.
const DisplayOutdated = "outdated"

// Provider types (v1.1 A5.1 provider.type).
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

// Balance APIs (v1.1 A5.1 provider.balance_api, A10.2).
const (
	BalanceNone     = "none"
	BalanceDeepSeek = "deepseek"
)

// model_choice values of the rerun decision (v1.1 A4.3).
const (
	ModelChoiceSnapshot = "snapshot" // 沿用原模型 (default)
	ModelChoiceCurrent  = "current"  // 使用当前设置
	// ModelChoiceManual (v3.2.x P1-b, retry only): provider, model and
	// params named for this one retry; written to the retried job's
	// snapshot only, never to the role's setting.
	ModelChoiceManual = "manual" // 临时指定
)

// ErrNeedsModelChoice is the "error" of the 409 body of v1.1 A4.3.
const ErrNeedsModelChoice = "needs_model_choice"

// Lecture-level actions returned in api.Lecture.LectureActions (v1.1 A3, A2.3,
// A7). Like stage actions, the front end renders only what is listed.
const (
	LectureActionLaunch = "launch" // draft only
	LectureActionUpdate = "update" // some stage is stale
	LectureActionCancel = "cancel" // some stage is queued or running
	LectureActionDelete = "delete" // always offered (A7); while a stage runs (can_delete=false) the UI deletes with force: cancel first
	// SPEC-v2 P13.5 "regenerate title and blurb": the summary is done or
	// stale and the organizer role is configured (POST /api/lectures/{id}/title).
	LectureActionRegenerateTitle = "regenerate_title"
	// SPEC-v2 P4.2 "补全词级时间戳": the lecture has a transcript without
	// word timestamps (api.Lecture.HasWords false), asr is done or stale and
	// can be rerun now; the front end reruns asr (POST .../stages/asr/rerun,
	// with the needs_model_choice dialog as usual).
	LectureActionFillWords = "fill_words"
)

// KV keys added in v1.1.
const (
	KVSettingsASR        = "settings.asr"         // JSON ASRSettings (A5.1)
	KVSettingsGeneral    = "settings.general"     // JSON GeneralSettings (A5.3 存储与其他)
	KVSettingsImported   = "settings.imported_v1" // "1" once config.toml legacy fields were imported (A5.1)
	KVSnapshotsBackfill  = "v1_1.snapshots_backfilled"
	KVProviderModelsPref = "provider_models:" // provider_models:{provider_id} → JSON ProbedModels (A5.3)
)

// StaleReason* format the human-readable job.stale_reason of v1.1 A2.2.
// from is the job's recorded stats.glossary_version (nil = not recorded).
func StaleReasonGlossary(from *int, to int) string {
	if from == nil {
		return staleGlossaryPrefix + "（现为 v" + itoa(to) + "）"
	}
	return staleGlossaryPrefix + "（v" + itoa(*from) + " → v" + itoa(to) + "）"
}

const staleGlossaryPrefix = "术语表已更新"

// IsGlossaryReason reports whether a stale_reason was made by
// StaleReasonGlossary or StaleReasonJanitor (v2 G3.1); such a reason follows
// later glossary edits.
func IsGlossaryReason(reason string) bool {
	for _, p := range []string{staleGlossaryPrefix, staleJanitorPrefix} {
		if len(reason) >= len(p) && reason[:len(p)] == p {
			return true
		}
	}
	return false
}
func StaleReasonSummary() string                 { return "摘要已更新" }
func StaleReasonMaterial(filename string) string { return "课件已变化：" + filename }

// StageLabel is the Chinese name of a stage used in UI texts built by the
// back end (e.g. "需要更新：修复、翻译", v1.1 A2.2).
func StageLabel(stage string) string {
	switch stage {
	case StagePrep:
		return "预处理"
	case StageASR:
		return "转写"
	case StageSummary:
		return "摘要"
	case StageRepair:
		return "修复"
	case StageTranslate:
		return "翻译"
	case StageExport:
		return "导出"
	case StageGlossarySeed:
		return "大纲术语"
	case StageMaterialExtract:
		return "课件抽取"
	case StageMaterialTerms:
		return "课件术语"
	case StageGlossaryReview:
		return "整理术语表"
	}
	return stage
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
