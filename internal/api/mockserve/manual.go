// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// v3.2.x P1-b in the mock: the retry of summary, repair or translate with a
// model named by hand (model_choice "manual" + manual{provider_id, model,
// thinking?, max_tokens?, temperature?}), with the real handler's codes
// (internal/api/v32x_retry.go) and the settings page's rules
// (settings.ManualSnapshot); and "重新翻译这句" with {"use_current": true}.

import (
	"fmt"
	"net/http"
	"strings"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

func manualStage(stage string) bool {
	switch stage {
	case model.StageSummary, model.StageRepair, model.StageTranslate:
		return true
	}
	return false
}

// useCurrentModel is "重新翻译这句" with {"use_current": true} (sched.
// RetranslateSentence): only a sentence without a usable translation (409
// sentence.not_pending otherwise); the lecture's translate stage takes the
// current translate model, and mixed reports whether that changed it.
// Called with s.mu held.
func (s *store) useCurrentModel(lectureID string, sn *api.Sentence) (bool, error) {
	if sn.ZhStatus != model.ZhQAFailed && sn.ZhStatus != model.ZhStale {
		return false, codedErr(http.StatusConflict, "sentence.not_pending", nil, "")
	}
	l, err := s.findLecture(lectureID)
	if err != nil {
		return false, nil // the mock's sentences are not tied to one lecture
	}
	i := stageIndex(l, model.StageTranslate)
	cur := s.currentModel(model.StageTranslate)
	if i < 0 || l.Stages[i].Status != model.StatusDone || cur == nil {
		return false, nil
	}
	st := &l.Stages[i]
	if st.Model != nil && st.Model.Label == cur.Label {
		return false, nil
	}
	if st.Model != nil && st.Model.Params != nil && st.Model.Params.Window != nil {
		w := *st.Model.Params.Window
		cur.Params.Window = &w
	}
	st.Model = cur
	s.changed(l)
	return true, nil
}

// settleLLM drops the request in flight of a stage that stopped running
// (the real view shows in_flight and current for running jobs only).
func settleLLM(st *api.StageView) {
	if st.LLM == nil {
		return
	}
	v := *st.LLM
	v.InFlight, v.Current = 0, nil
	st.LLM = &v
	if len(v.Attempts) == 0 && v.Retries == 0 {
		st.LLM = nil
	}
}

// checkManual is the real handler's manualTarget without the target.
func checkManual(stage, action string, req api.ModelChoiceRequest) error {
	if req.Manual == nil && req.ModelChoice != model.ModelChoiceManual {
		return nil
	}
	if req.ModelChoice != model.ModelChoiceManual || req.TextOnly {
		return codedErr(http.StatusBadRequest, "model_choice.manual_conflict", nil, "")
	}
	if action != model.ActionRetry || !manualStage(stage) {
		return codedErr(http.StatusBadRequest, "model_choice.manual_unsupported", map[string]any{"stage": stage, "action": action}, "")
	}
	m := req.Manual
	field := ""
	switch {
	case m == nil:
		field = "manual"
	case strings.TrimSpace(m.ProviderID) == "":
		field = "provider_id"
	case strings.TrimSpace(m.Model) == "":
		field = "model"
	}
	if field != "" {
		return codedErr(http.StatusBadRequest, "model_choice.manual_incomplete", map[string]any{"field": field}, "")
	}
	return nil
}

// manualModel validates m like settings.ManualSnapshot and returns the
// stage's new model (source manual; a translate stage keeps the window and
// batch size of old, its model so far). Called with s.mu held.
func (s *store) manualModel(stage string, m api.ManualModel, old *api.StageModel) (*api.StageModel, error) {
	role := model.RoleOfStage(stage)
	p, ok := s.providerByID(strings.TrimSpace(m.ProviderID))
	if !ok {
		return nil, codedErr(http.StatusBadRequest, settings.CodeRoleProviderMissing, map[string]any{"role": role},
			fmt.Sprintf("请为「%s」选择一个已存在的提供商", settings.RoleLabel(role)))
	}
	if p.KeyHint == "" && !settings.IsLocal(p.BaseURL) { // a local server needs no key (as settings.ManualSnapshot, r4 T5)
		return nil, codedErr(http.StatusBadRequest, settings.CodeSnapshotNoAPIKey, map[string]any{"provider": p.Name},
			fmt.Sprintf("提供商「%s」没有设置 API key", p.Name))
	}
	params := model.RoleParams{Temperature: m.Temperature, Thinking: m.Thinking, MaxTokens: m.MaxTokens}
	if t := params.Temperature; t != nil && !(*t >= 0 && *t <= 2) {
		return nil, codedErr(http.StatusBadRequest, settings.CodeTemperatureInvalid, map[string]any{"min": 0, "max": 2}, "温度必须在 0 到 2 之间")
	}
	if mt := params.MaxTokens; mt != nil && (*mt < 1 || *mt > settings.MaxTokensLimit) {
		return nil, codedErr(http.StatusBadRequest, settings.CodeMaxTokensInvalid, map[string]any{"min": 1, "max": settings.MaxTokensLimit},
			fmt.Sprintf("输出上限（max_tokens）必须是 1 到 %d 之间的整数", settings.MaxTokensLimit))
	}
	switch lvl := model.ThinkingOf(params); {
	case !model.ValidThinking(lvl):
		return nil, codedErr(http.StatusBadRequest, settings.CodeThinkingInvalid, map[string]any{"value": lvl},
			fmt.Sprintf("深度思考的强度「%s」无效，只能是 off、low、high 或 max", lvl))
	case lvl == model.ThinkingOff:
		params.Thinking = nil
	case !s.providerView(p).SupportsThinking:
		return nil, codedErr(http.StatusBadRequest, settings.CodeThinkingUnsupported, map[string]any{"provider": p.Name},
			fmt.Sprintf("提供商「%s」不支持深度思考，请选择“关闭”", p.Name))
	}
	if stage == model.StageTranslate {
		w := model.DefaultTranslateWindow
		if old != nil && old.Params != nil && old.Params.Window != nil {
			w = *old.Params.Window
		}
		params.Window = &w
		if old != nil && old.Params != nil {
			params.Batch = old.Params.Batch
		}
	}
	res := settings.ResolveParams(role, params)
	name := strings.TrimSpace(m.Model)
	return &api.StageModel{Label: mockLLMLabel(p.Name, name, res), ProviderID: p.ID, ProviderName: p.Name, Model: name,
		Params: &res, Source: model.SnapshotSourceManual}, nil
}
