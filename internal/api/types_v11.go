// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// v1.1 response and request structs (SPEC-v1.1). FROZEN by the lead at the
// start of v1.1, like types.go; testdata/api/v11_*.json holds examples for
// the front end. Fields added to the v1 structs are listed here as
// comments and live in types.go:
//
//   StageView      + display_status, stale_reason, model
//   Lecture        + seq, label, launched_at, draft, stale_text, lecture_actions
//   Course         + code, draft_count, stale_lecture_count,
//                    patches_pending, running_count, last_activity_at
//   Syllabus       + formatted
//
// Lecture actions (model.LectureAction*) are rendered like stage actions:
// only what the back end lists is shown.

import (
	"encoding/json"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
)

// StageModel is the snapshot summary of a stage (hover card "模型").
// Label is preformatted: "DeepSeek / deepseek-chat · 温度 0.3 · 窗口 3" for
// LLM stages, "large-v2 · auto · cuda · en" for asr. null for prep/export
// and for drafts.
type StageModel struct {
	Label        string `json:"label"`
	ProviderID   string `json:"provider_id,omitempty"`
	ProviderName string `json:"provider_name,omitempty"`
	Model        string `json:"model"`
	// Params of an LLM snapshot (temperature, window, thinking), so the
	// front end can write the label in the interface language; omitted for
	// asr. Label stays the preformatted fallback.
	Params *model.RoleParams `json:"params,omitempty"`
	// Source (v3.2.x P1-b): "manual" when the snapshot was named by hand for
	// one retry (model.SnapshotSourceManual, "临时指定"); omitted otherwise.
	Source string `json:"source,omitempty"`
	// ASR is the structured asr snapshot (as SnapshotView.ASR; 3b Gate
	// round 2): {auto: true, language} without a device while no device is
	// chosen yet, so the front end writes that label in the interface
	// language. Omitted for the other stages.
	ASR *model.ASRSnapshot `json:"asr,omitempty"`
}

// ---------- A4.3 model choice ----------

// ModelChoiceRequest is the optional body of rerun, retry, update and the
// batch operations that may need a model choice.
type ModelChoiceRequest struct {
	ModelChoice string `json:"model_choice,omitempty"` // "" | snapshot | current | manual
	// Manual (v3.2.x P1-b, lead freeze): the target of this one retry, with
	// model_choice manual only (400 otherwise, and 400 when manual has none).
	// Retry of summary, repair and translate; written to the retried job's
	// snapshot, never to the role's setting.
	Manual *ManualModel `json:"manual,omitempty"`
	// TextOnly (textrepair-int, an optional field the lead approved): the asr
	// rerun "修复断句" — the stored transcript is repaired, not transcribed
	// again (offered as model.ActionRerunTextOnly). Only with stage asr and
	// action rerun; model_choice then concerns the stages after asr.
	TextOnly bool `json:"text_only,omitempty"`
}

// ManualModel is ModelChoiceRequest.manual. Thinking, MaxTokens and
// Temperature are optional: off, automatic and the role's default.
type ManualModel struct {
	ProviderID  string   `json:"provider_id"`
	Model       string   `json:"model"`
	Thinking    *string  `json:"thinking,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// SnapshotView describes one side of a model diff.
type SnapshotView struct {
	Label        string             `json:"label"` // "DeepSeek / deepseek-chat" or "large-v2 · float16 · cuda · en"; "（无）" when empty
	ProviderID   string             `json:"provider_id,omitempty"`
	ProviderName string             `json:"provider_name,omitempty"`
	Model        string             `json:"model,omitempty"`
	Params       *model.RoleParams  `json:"params,omitempty"`
	ASR          *model.ASRSnapshot `json:"asr,omitempty"`
	// PromptVersion of an LLM snapshot (v2 G5.5; 1 when the snapshot has none).
	// The dialog shows "提示词 v1 → v2" when snapshot and current differ.
	PromptVersion int `json:"prompt_version,omitempty"`
}

// ModelDiffView is one entry of NeedsModelChoice.Diff.
type ModelDiffView struct {
	LectureID      string       `json:"lecture_id,omitempty"`
	LectureTitle   string       `json:"lecture_title,omitempty"`
	Stage          string       `json:"stage"`
	Snapshot       SnapshotView `json:"snapshot"`
	Current        SnapshotView `json:"current"`
	SnapshotUsable bool         `json:"snapshot_usable"`
	Reason         string       `json:"reason,omitempty"` // why 沿用原模型 is not available
	// ReasonInfo is the coded Reason (v2 P14.3).
	ReasonInfo *model.ErrorInfo `json:"reason_info,omitempty"`
}

// NeedsModelChoice is the 409 body of A4.3: {"error":"needs_model_choice","diff":[...]}.
type NeedsModelChoice struct {
	// v2 P14.3: {"code": "needs_model_choice"} (was the bare string in v1.1).
	Error model.ErrorInfo `json:"error"`
	Diff  []ModelDiffView `json:"diff"`
}

// ---------- A3 / A7 batch operations ----------

// BatchRequest is the body of POST /api/lectures/launch, /delete, /cancel, /update.
type BatchRequest struct {
	IDs         []string `json:"ids"`
	Force       bool     `json:"force,omitempty"`        // delete only
	ModelChoice string   `json:"model_choice,omitempty"` // update only
}

// BatchResult is one id's outcome.
type BatchResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"` // transition: Chinese text until the i18n sweep
	// ErrorInfo is the coded error of this item (v2 P14.3).
	ErrorInfo *model.ErrorInfo `json:"error_info,omitempty"`
	// Lecture is the updated lecture (nil after delete or on error).
	Lecture *Lecture `json:"lecture,omitempty"`
}

type BatchResponse struct {
	Results []BatchResult `json:"results"`
}

// LecturePatch and CoursePatch moved to types_v2.go (v2 P13).

// CourseCreate is the body of POST /api/courses (A8.1 step 1).
type CourseCreate struct {
	Name string  `json:"name"`
	Code *string `json:"code,omitempty"`
}

// CourseOpResult is the response of POST /api/courses/{id}/launch-drafts
// and /update-stale: per-lecture results plus the refreshed course.
type CourseOpResult struct {
	Results []BatchResult `json:"results"`
	Course  Course        `json:"course"`
}

// ---------- A5 settings ----------

// ProviderView is a provider without its key (A5.2: only key_hint).
type ProviderView struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	BaseURL        string   `json:"base_url"`
	KeyHint        string   `json:"key_hint"` // "sk-…abcd", "" when no key
	MaxConcurrency int      `json:"max_concurrency"`
	RPM            int      `json:"rpm"`
	BalanceAPI     string   `json:"balance_api"`
	PriceInput     *float64 `json:"price_input"`
	PriceCacheHit  *float64 `json:"price_cache_hit"`
	PriceOutput    *float64 `json:"price_output"`
	Currency       *string  `json:"currency"`
	CreatedAt      string   `json:"created_at"`
	UsedByRoles    []string `json:"used_by_roles"` // roles using it (delete blocked when non-empty)
	SnapshotRefs   int      `json:"snapshot_refs"` // current lecture jobs whose snapshot names it
	Models         []string `json:"models"`        // last probe result (may be empty)
	ModelsProbedAt *string  `json:"models_probed_at"`
	ModelsError    string   `json:"models_error,omitempty"`
	// ModelsErrorInfo is the coded ModelsError (v2 P14.3).
	ModelsErrorInfo *model.ErrorInfo `json:"models_error_info,omitempty"`
	// v2 P12: tariff (raw tariff.Tariff JSON object, null = none), the
	// off-peak-only switch and the current peak state.
	Tariff      json.RawMessage `json:"tariff"`
	OffpeakOnly bool            `json:"offpeak_only"`
	TariffState *TariffState    `json:"tariff_state,omitempty"`
	// SupportsThinking: the roles on this provider may turn on thinking
	// (settings.SupportsThinking: DeepSeek and Anthropic; other
	// OpenAI-compatible providers only "off").
	SupportsThinking bool `json:"supports_thinking"`
	// DefaultBatchClaude: the translate role's sentences per request on
	// this provider for a Claude model when the role leaves the batch empty
	// (settings.ClaudeBatch: 10 on Anthropic, not DeepSeek; 1 otherwise;
	// user 2026-11-11). Any other model is 1: the settings page applies the
	// model rule (provider_rules.js defaultBatch, settings.DefaultBatch) to
	// the role's current model and shows the result next to the field.
	DefaultBatchClaude int `json:"default_batch_claude"`
	// cred-enc: the stored key cannot be decrypted with this installation's
	// key (the data directory came from another computer or account); it
	// has to be entered again. key_hint is then "", key_error_info is
	// settings.provider_key_unreadable {provider}.
	KeyUnreadable bool             `json:"key_unreadable,omitempty"`
	KeyErrorInfo  *model.ErrorInfo `json:"key_error_info,omitempty"`
	// sec3 L2: the key is stored in the clear (saved while there was no data
	// key); key_error_info is then settings.provider_key_unprotected.
	KeyUnprotected bool `json:"key_unprotected,omitempty"`
	// RateLimits: the account limits the provider's answers reported
	// since the application started (Anthropic only; absent before the
	// first answer with them), with the suggested settings (T-PERF-03).
	RateLimits *ProviderRateLimits `json:"rate_limits,omitempty"`
}

// ProviderRateLimits is what the provider's most recent answers said about
// the account's limits, per minute (llm.RateLimits: of the model with the
// smallest request limit), and the settings suggested from it
// (llm.AdviseLimits). Observed, not guaranteed; never a key or a raw
// header.
type ProviderRateLimits struct {
	Model             string `json:"model"`
	RequestsPerMinute int64  `json:"requests_per_minute"`
	// The token limits, absent when not reported.
	InputTokensPerMinute  *int64 `json:"input_tokens_per_minute,omitempty"`
	OutputTokensPerMinute *int64 `json:"output_tokens_per_minute,omitempty"`
	TokensPerMinute       *int64 `json:"tokens_per_minute,omitempty"`
	ObservedAt            string `json:"observed_at"` // RFC 3339, UTC
	// Advice: the values the settings page fills in (never below the
	// current ones; rpm 0 = no per-minute limit stays so) and whether the
	// current settings are clearly below the suggestion.
	Advice ProviderLimitAdvice `json:"advice"`
}

// ProviderLimitAdvice is llm.LimitAdvice on the wire: below = either of
// concurrency_below / rpm_below (rpm_below never without a per-minute
// limit); measured = the suggestion rests on measured answers (time and
// tokens), not only on the request limit.
type ProviderLimitAdvice struct {
	MaxConcurrency   int  `json:"max_concurrency"`
	RPM              int  `json:"rpm"`
	Below            bool `json:"below"`
	ConcurrencyBelow bool `json:"concurrency_below"`
	RPMBelow         bool `json:"rpm_below"`
	Measured         bool `json:"measured"`
}

// ProviderInput is the body of POST /api/providers and PATCH /api/providers/{id}.
// On PATCH absent fields are unchanged; api_key "" or absent = unchanged (A5.2).
type ProviderInput struct {
	Name           *string  `json:"name,omitempty"`
	Type           *string  `json:"type,omitempty"`
	BaseURL        *string  `json:"base_url,omitempty"`
	APIKey         *string  `json:"api_key,omitempty"`
	MaxConcurrency *int     `json:"max_concurrency,omitempty"`
	RPM            *int     `json:"rpm,omitempty"`
	BalanceAPI     *string  `json:"balance_api,omitempty"`
	PriceInput     *float64 `json:"price_input,omitempty"`
	PriceCacheHit  *float64 `json:"price_cache_hit,omitempty"`
	PriceOutput    *float64 `json:"price_output,omitempty"`
	Currency       *string  `json:"currency,omitempty"`
	ClearPrices    bool     `json:"clear_prices,omitempty"`
	// v2 P12: Tariff replaces the tariff (JSON object; null clears it);
	// absent = unchanged. New DeepSeek providers get tariff.DeepSeekPreset().
	Tariff      json.RawMessage `json:"tariff,omitempty"`
	OffpeakOnly *bool           `json:"offpeak_only,omitempty"`
}

// ProviderDeleteResult is the 200 body of DELETE /api/providers/{id}.
type ProviderDeleteResult struct {
	SnapshotRefs int    `json:"snapshot_refs"`
	Warning      string `json:"warning,omitempty"` // "使用该提供商的旧讲座将无法沿用原模型重跑" when refs > 0
}

// RoleView is one role of the settings page (A5.3 角色分配).
type RoleView struct {
	Role        string  `json:"role"`
	Label       string  `json:"label"` // 大纲整理、术语提取、摘要、修复、翻译
	ProviderID  *string `json:"provider_id"`
	Model       *string `json:"model"`
	Temperature float64 `json:"temperature"`      // resolved (default per SPEC §12 when unset)
	Window      *int    `json:"window,omitempty"` // translate only, resolved
	Configured  bool    `json:"configured"`
	Error       string  `json:"error,omitempty"` // e.g. provider deleted
	// ErrorInfo is Error as a code (v2 P14.3: settings.role_not_configured,
	// settings.role_provider_deleted; params.role is the role id).
	ErrorInfo *model.ErrorInfo `json:"error_info,omitempty"`
	// Default: the value was filled in automatically (SPEC-v2 appendix D1)
	// and the user has not changed it since; the page shows a "默认" tag.
	Default bool `json:"default,omitempty"`
	// Hint is the role's explanation line (G3.1 for glossary_janitor).
	Hint string `json:"hint,omitempty"`
	// Thinking is the role's thinking level: off (also when unset), low,
	// high or max (model.RoleParams.Thinking). Saved with
	// RoleInput.params.thinking.
	Thinking string `json:"thinking"`
	// MaxTokens and Batch (v3.2.x D1, P2): the values the user set
	// (RoleInput.params.max_tokens, params.batch), absent while unset —
	// automatic output limit, one sentence per request. Batch is the
	// translate role's only.
	MaxTokens *int `json:"max_tokens,omitempty"`
	Batch     *int `json:"batch,omitempty"`
}

// RoleInput is the body of PUT /api/settings/roles/{role}.
type RoleInput struct {
	ProviderID string           `json:"provider_id"`
	Model      string           `json:"model"`
	Params     model.RoleParams `json:"params"`
}

// Settings is GET /api/settings.
type Settings struct {
	Providers []ProviderView        `json:"providers"`
	Roles     []RoleView            `json:"roles"` // in model.Roles order
	ASR       model.ASRSettings     `json:"asr"`
	General   model.GeneralSettings `json:"general"`
	// DefaultBaseURLs prefill base_url when a type is chosen: {"openai": "...", "anthropic": "..."}.
	DefaultBaseURLs map[string]string `json:"default_base_urls"`
	// GPUTotalMB helps the 转写 section explain compute_type auto (0 = unknown).
	GPUTotalMB int `json:"gpu_total_mb"`
	// Devices are the transcription devices of the 转写 section (SPEC-v2
	// I6, phase 3b freeze; = GET /api/asr/devices). Omitted when there are
	// none (and by builds before 3b).
	Devices []ASRDeviceView `json:"devices,omitempty"`
	// ASRRedecode is the switch "修复退化的转写窗口" of the 转写 section
	// (redecode, 2026-10-27, optional field added with the lead's leave):
	// kv model.KVASRRedecode, on unless the user switched it off. The
	// server always sends it; a client reads a missing value as on. PUT
	// /api/settings/asr takes it as "redecode" (true | false) next to the
	// ASRSettings fields; absent leaves it unchanged.
	ASRRedecode *bool `json:"asr_redecode,omitempty"`
	// CredentialsErrorInfo (sec3 L2): settings.credentials_unavailable
	// {error} while there is no data key: a key entered now is stored in
	// the clear. Omitted normally.
	CredentialsErrorInfo *model.ErrorInfo `json:"credentials_error_info,omitempty"`
}

// ProviderTest is the response of POST /api/providers/{id}/test.
type ProviderTest struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"` // server error text, verbatim
	Model     string `json:"model,omitempty"` // model used for the request
	// ErrorInfo is the coded error (v2 P14.3); Error stays as the detail.
	ErrorInfo *model.ErrorInfo `json:"error_info,omitempty"`
}

// ProviderModels is the response of POST /api/providers/{id}/models.
type ProviderModels struct {
	Models   []string `json:"models"`
	ProbedAt string   `json:"probed_at"`
	Error    string   `json:"error,omitempty"` // probe failed: enter the model name by hand
	// ErrorInfo is the coded Error (v2 P14.3).
	ErrorInfo *model.ErrorInfo `json:"error_info,omitempty"`
}

// ---------- A8 syllabus ----------

// SyllabusDoc is GET /api/courses/{id}/syllabus and the response of the
// raw upload / save endpoints.
type SyllabusDoc struct {
	Filename string `json:"filename"`
	Raw      string `json:"raw"`      // syllabus.raw.txt (PDF already extracted)
	Markdown string `json:"markdown"` // SYLLABUS.md ("" until formatted/saved)
}

// SyllabusRawInput is the JSON body of POST /api/courses/{id}/syllabus/raw
// for pasted text (files use multipart field "file").
type SyllabusRawInput struct {
	Text string `json:"text"`
}

// SyllabusFormatResult is the response of POST /api/courses/{id}/syllabus/format.
type SyllabusFormatResult struct {
	Markdown string     `json:"markdown"`
	Usage    UsageStats `json:"usage"`
}

// SyllabusSaveInput is the body of PUT /api/courses/{id}/syllabus.
type SyllabusSaveInput struct {
	Markdown string `json:"markdown"`
}

// ---------- A9 dashboard ----------

// LectureRef is a lecture with its course for cross-course lists.
type LectureRef struct {
	Lecture
	CourseName string  `json:"course_name"`
	CourseCode *string `json:"course_code"`
}

// ReviewGroup is "待审修改" grouped by lecture.
type ReviewGroup struct {
	LectureID    string  `json:"lecture_id"`
	LectureTitle string  `json:"lecture_title"`
	Label        string  `json:"label"` // L03
	CourseID     string  `json:"course_id"`
	CourseName   string  `json:"course_name"`
	CourseCode   *string `json:"course_code"`
	Count        int     `json:"count"`
	KindTag              // lecture kinds (types_kinds.go)
}

// FailureItem is a stage that failed for good in the last 24 h.
type FailureItem struct {
	LectureID      string           `json:"lecture_id"`
	LectureTitle   string           `json:"lecture_title"`
	Label          string           `json:"label"`
	CourseID       string           `json:"course_id"`
	CourseName     string           `json:"course_name"`
	CourseCode     *string          `json:"course_code,omitempty"` // lecture kinds: the dashboard's code column
	Stage          string           `json:"stage"`
	Error          *string          `json:"error"`
	ErrorInfo      *model.ErrorInfo `json:"error_info,omitempty"` // coded Error (v2 P14.3)
	FinishedAt     *string          `json:"finished_at"`
	AllowedActions []string         `json:"allowed_actions"` // ["retry"]
	KindTag                         // lecture kinds (types_kinds.go)
}

// CourseCount counts something per course (drafts, stale lectures).
type CourseCount struct {
	CourseID   string  `json:"course_id"`
	CourseName string  `json:"course_name"`
	CourseCode *string `json:"course_code"`
	Count      int     `json:"count"`
}

// ProviderUsage is one provider's usage for "今日用量".
type ProviderUsage struct {
	ProviderID   string   `json:"provider_id"`
	Name         string   `json:"name"`
	Requests     int64    `json:"requests"`
	PromptTokens int64    `json:"prompt_tokens"`
	CacheHit     int64    `json:"cache_hit_tokens"`
	Completion   int64    `json:"completion_tokens"`
	CacheHitRate *float64 `json:"cache_hit_rate"`
	Cost         *float64 `json:"cost"`
	Currency     *string  `json:"currency"`
	// CostUnpricedRequests is the number of today's requests that had no
	// price (SPEC-v2 D3), not included in Cost; Cost is null when all of
	// them were unpriced. With Cost set: "部分请求未计价". Omitted at 0
	// (added at the phase-2 gate, lead authorised).
	CostUnpricedRequests int64 `json:"cost_unpriced_requests,omitempty"`
}

// CourseCard is one card of "课程概览" (also used by the course list).
type CourseCard struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Code           *string `json:"code"`
	LectureCount   int     `json:"lecture_count"`
	TermCount      int     `json:"term_count"`
	DraftCount     int     `json:"draft_count"`
	StaleCount     int     `json:"stale_count"`
	RunningCount   int     `json:"running_count"`
	PatchesPending int     `json:"patches_pending"`
	FailedCount    int     `json:"failed_count"`     // as Course.failed_count (v3.2.x r7, T24 S2)
	LastActivityAt string  `json:"last_activity_at"` // latest job finish / lecture creation (UTC RFC3339)
	// CourseActions for the course-card context menu (A12.2): launch_drafts,
	// update_stale, export_cite (rendered only when listed).
	CourseActions []string `json:"course_actions"`
}

// Course-level actions in CourseCard.CourseActions / Course.CourseActions.
const (
	CourseActionLaunchDrafts = "launch_drafts"
	CourseActionUpdateStale  = "update_stale"
	CourseActionExportCite   = "export_cite"
)

// Dashboard is GET /api/dashboard (A9).
type Dashboard struct {
	InProgress  []LectureRef    `json:"in_progress"` // launched, not all done (oldest first)
	Drafts      []CourseCount   `json:"drafts"`
	DraftTotal  int             `json:"draft_total"`
	Reviews     []ReviewGroup   `json:"reviews"`
	ReviewTotal int             `json:"review_total"`
	Stale       []CourseCount   `json:"stale"`
	StaleTotal  int             `json:"stale_total"`
	Failures    []FailureItem   `json:"failures"`    // last 24 h, newest first
	RecentDone  []LectureRef    `json:"recent_done"` // newest first, at most 10, with exports
	UsageToday  []ProviderUsage `json:"usage_today"`
	Courses     []CourseCard    `json:"courses"`
	GeneratedAt string          `json:"generated_at"`
	// DevicePrompts are the open "检测到新的转写设备，测试一下它的速度？"
	// prompts (SPEC-v2 I9, phase 3b freeze); omitted when there is none.
	DevicePrompts []DevicePrompt `json:"device_prompts,omitempty"`
	// EarlierFailedTotal (v3.2.x r7, T24 S2): launched lectures with a
	// failed stage none of which is in Failures (they failed more than 24 h
	// ago and are still failed): "还有 N 节更早失败". Always present.
	EarlierFailedTotal int `json:"earlier_failed_total"`
}

// ---------- A10 resources ----------

// Resources is GET /api/resources and SSE "event: resources".
type Resources = resources.Snapshot
