// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"fmt"
	"strings"

	"lectureflow/internal/model"
)

// Limits of the role params added by v3.2.x (D1, P2).
const (
	// MaxTokensLimit is the upper bound of a user-set max_tokens. It only
	// keeps nonsense out: the llm client caps the value at the model's own
	// output limit.
	MaxTokensLimit = 1_000_000
	// MaxBatch is the largest number of target sentences per translate request.
	MaxBatch = 20
	// DefaultAnthropicBatch is the translate role's sentences per request
	// for a Claude model on an Anthropic provider whose role setting leaves
	// the batch empty (DefaultBatch; user 2026-11-11 after the T5
	// acceptance: 10 per request took a 220-sentence lecture on
	// claude-sonnet-5-5 from 148 s to 49 s and from 0.68 to 0.30 USD).
	// Every other provider and model stays at 1.
	DefaultAnthropicBatch = 10
)

// ClaudeBatch is the translate batch an empty role setting means for a
// Claude model (IsClaudeModel) on provider p: DefaultAnthropicBatch on an
// Anthropic-type provider, 1 on every other one. DeepSeek's
// Anthropic-compatible endpoint (an Anthropic-type provider that IsDeepSeek)
// is DeepSeek, not Claude, and stays at 1 like DeepSeek's own API. The
// settings page gets it per provider (default_batch_claude) and applies the
// model rule itself (web/provider_rules.js defaultBatch; a web test checks
// both sides on the same cases).
func ClaudeBatch(p model.Provider) int {
	if p.Type == model.ProviderAnthropic && !IsDeepSeek(p.BaseURL, p.BalanceAPI) {
		return DefaultAnthropicBatch
	}
	return 1
}

// IsClaudeModel reports whether a model name names a Claude model: it
// contains "claude", ignoring case (claude-sonnet-5-5, a gateway's
// anthropic/claude-… or claude-…@…). Other vendors' models on an
// Anthropic-compatible endpoint (Kimi, GLM, MiniMax, …) are not.
func IsClaudeModel(name string) bool {
	return strings.Contains(strings.ToLower(name), "claude")
}

// DefaultBatch is the translate role's sentences per request when the
// role setting leaves the batch empty: ClaudeBatch(p) for a Claude model,
// 1 for any other model (user 2026-11-11; narrowed to Claude models by the
// lead after review R24).
func DefaultBatch(p model.Provider, modelName string) int {
	if IsClaudeModel(modelName) {
		return ClaudeBatch(p)
	}
	return 1
}

// resolveRoleParams is ResolveParams for a role's current setting (model
// modelName on provider p), the params a snapshot of the current settings
// holds: a translate role without a batch size gets DefaultBatch written in
// when it is above 1. The default is fixed here, when the snapshot is
// written, and the stage reads only the snapshot: a snapshot written before
// the default existed has no batch and keeps running one sentence per
// request (model.RoleParams.Batch nil = 1), while a new snapshot of a
// Claude model on an Anthropic provider always names its size (10 by
// default, 1 when the user set 1). Other providers and models resolve to
// exactly what they did before.
func resolveRoleParams(role string, p model.Provider, modelName string, params model.RoleParams) model.RoleParams {
	out := ResolveParams(role, params)
	if role == model.RoleTranslate && out.Batch == nil {
		if n := DefaultBatch(p, modelName); n > 1 {
			out.Batch = &n
		}
	}
	return out
}

// checkParams validates the params of role for a model of provider prov and
// normalises them in place (the rules of SetRole, shared with a manual
// retry): temperature in [0, 2]; window translate only, in [0, 10];
// thinking one of model.ThinkingLevels, other than off only when prov
// SupportsThinking, off stored as absent; max_tokens absent or in
// [1, MaxTokensLimit]; batch translate only, absent or in [1, MaxBatch].
func checkParams(role string, prov model.Provider, params *model.RoleParams) error {
	if t := params.Temperature; t != nil && !(*t >= 0 && *t <= 2) {
		return userErr(ErrInvalid, CodeTemperatureInvalid, map[string]any{"min": 0, "max": 2}, "温度必须在 0 到 2 之间")
	}
	if role != model.RoleTranslate {
		params.Window = nil // only translate has a context window
		params.Batch = nil  // and a batch size
	} else {
		if w := params.Window; w != nil && (*w < 0 || *w > 10) {
			return userErr(ErrInvalid, CodeWindowInvalid, map[string]any{"min": 0, "max": 10}, "翻译的上下文窗口必须在 0 到 10 之间")
		}
		if b := params.Batch; b != nil && (*b < 1 || *b > MaxBatch) {
			return userErr(ErrInvalid, CodeBatchInvalid, map[string]any{"min": 1, "max": MaxBatch},
				"每批翻译的句数必须在 1 到 %d 之间", MaxBatch)
		}
	}
	if m := params.MaxTokens; m != nil && (*m < 1 || *m > MaxTokensLimit) {
		return userErr(ErrInvalid, CodeMaxTokensInvalid, map[string]any{"min": 1, "max": MaxTokensLimit},
			"输出上限（max_tokens）必须是 1 到 %d 之间的整数", MaxTokensLimit)
	}
	switch lvl := model.ThinkingOf(*params); {
	case !model.ValidThinking(lvl):
		return userErr(ErrInvalid, CodeThinkingInvalid, map[string]any{"value": lvl},
			"深度思考的强度「%s」无效，只能是 off、low、high 或 max", lvl)
	case lvl == model.ThinkingOff:
		params.Thinking = nil
	case !SupportsThinking(prov):
		return userErr(ErrInvalid, CodeThinkingUnsupported, map[string]any{"provider": prov.Name},
			"提供商「%s」不支持深度思考，请选择“关闭”", prov.Name)
	}
	return nil
}

// ManualTarget is the model the user names for ONE retry of an LLM stage
// (v3.2.x P1-b, model choice manual). Thinking, MaxTokens and Temperature
// are optional: off, automatic and the role's default.
type ManualTarget struct {
	ProviderID  string
	Model       string
	Thinking    *string
	MaxTokens   *int
	Temperature *float64
}

// ManualSnapshot validates a manual retry target for stage (summary, repair
// or translate) with the rules of the settings page and returns the snapshot
// to write on the retried job: Source manual, the stage's current prompt
// version, resolved params. Nothing is stored: the role's setting stays as
// it is. old is the job's snapshot so far: a translate job keeps its context
// window (it is part of every zh_hash), and its batch size when the
// provider stays the same and the old and new models are both Claude or
// both not; otherwise the batch is the target's default (DefaultBatch: 10
// for a Claude model on Anthropic, else none).
//
// Errors (ErrInvalid, coded): the provider does not exist
// (CodeRoleProviderMissing), cannot be used (no API key, or a key that
// cannot be read here: CodeSnapshotNoAPIKey, CodeSnapshotKeyUnreadable),
// the model is empty (CodeRoleModelMissing), or a param is out of range
// (checkParams).
func (s *Service) ManualSnapshot(stage string, in ManualTarget, old string) (model.LLMSnapshot, error) {
	role := ""
	switch stage {
	case model.StageSummary, model.StageRepair, model.StageTranslate:
		role = model.RoleOfStage(stage)
	default:
		return model.LLMSnapshot{}, fmt.Errorf("%w: stage %q has no model to choose", ErrInvalid, stage)
	}
	prov, ok := s.Provider(in.ProviderID)
	if !ok || in.ProviderID == "" {
		return model.LLMSnapshot{}, userErr(ErrInvalid, CodeRoleProviderMissing, map[string]any{"role": role},
			"请为「%s」选择一个已存在的提供商", RoleLabel(role))
	}
	if prov.APIKey == "" && s.KeyUnreadable(prov.ID) {
		return model.LLMSnapshot{}, userErr(ErrInvalid, CodeSnapshotKeyUnreadable, map[string]any{"provider": prov.Name},
			"提供商「%s」保存的 API key 无法在这台电脑或这个账户上解密，需要重新输入。输入之前，请先确认这个提供商的地址是你设置的；如果不是，先把地址改回正确的再输入", prov.Name)
	}
	if prov.APIKey == "" && !IsLocal(prov.BaseURL) {
		return model.LLMSnapshot{}, userErr(ErrInvalid, CodeSnapshotNoAPIKey, map[string]any{"provider": prov.Name},
			"提供商「%s」没有设置 API key", prov.Name)
	}
	name := strings.TrimSpace(in.Model)
	if name == "" {
		return model.LLMSnapshot{}, userErr(ErrInvalid, CodeRoleModelMissing, map[string]any{"role": role},
			"请为「%s」选择或填写模型名", RoleLabel(role))
	}
	params := model.RoleParams{Temperature: in.Temperature, Thinking: in.Thinking, MaxTokens: in.MaxTokens}
	sameProvider := false
	if stage == model.StageTranslate {
		w := model.SnapshotWindow(old)
		params.Window = &w
		// The batch size goes with the provider and the kind of model (lead
		// after review R24 and its recheck): on the same provider, with the
		// old and the new model both Claude or both not (IsClaudeModel, the
		// rule of DefaultBatch), the old snapshot's is kept (none = 1).
		// Otherwise (another provider, the other kind of model, or no valid
		// old snapshot, e.g. one without a model name)
		// the target's default is taken as a snapshot of the current
		// settings would (resolveRoleParams): a default 10 written for
		// Claude is not carried to DeepSeek or to a gateway's non-Claude
		// model, and a job moved to a Claude model gets 10.
		if o, ok := model.ParseLLMSnapshot(old); ok && o.ProviderID == prov.ID && IsClaudeModel(o.Model) == IsClaudeModel(name) {
			params.Batch = o.Params.Batch
			sameProvider = true
		}
	}
	if err := checkParams(role, prov, &params); err != nil {
		return model.LLMSnapshot{}, err
	}
	resolved := ResolveParams(role, params)
	if !sameProvider {
		resolved = resolveRoleParams(role, prov, name, params)
	}
	return model.LLMSnapshot{
		ProviderID:    prov.ID,
		ProviderName:  prov.Name,
		Model:         name,
		Params:        resolved,
		PromptVersion: model.PromptVersionOf(stage),
		Source:        model.SnapshotSourceManual,
	}, nil
}
