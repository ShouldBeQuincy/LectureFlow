// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Error codes of the manual model choice (v3.2.x P1-b; errorCatalog).
const (
	codeManualConflict    = "model_choice.manual_conflict"
	codeManualIncomplete  = "model_choice.manual_incomplete"
	codeManualUnsupported = "model_choice.manual_unsupported"
	// codeSentenceNotPending: retranslate with use_current of a sentence
	// that has a usable translation (409).
	codeSentenceNotPending = "sentence.not_pending"
)

// manualStages are the stages whose retry may name a model by hand.
func manualStage(stage string) bool {
	switch stage {
	case model.StageSummary, model.StageRepair, model.StageTranslate:
		return true
	}
	return false
}

// manualTarget checks the manual model choice of a lecture stage action
// (POST /api/lectures/{id}/stages/{stage}/{action}). It returns nil, nil
// when the body names none. The checks run before anything is read or
// changed, and no error carries anything of the body but the field names
// (a provider's key is never in the body, and never in an error):
//   - manual with another model_choice, or model_choice manual with
//     text_only → 400 model_choice.manual_conflict;
//   - manual for another stage than summary, repair or translate, or for
//     another action than retry → 400 model_choice.manual_unsupported
//     {stage, action};
//   - model_choice manual without manual, or without provider_id or model
//     → 400 model_choice.manual_incomplete {field}.
//
// The rest (the provider exists and can be used, thinking, max_tokens and
// temperature in range) is the settings service's (settings.ManualSnapshot,
// through sched.RetryManual), with the settings page's error codes.
func manualTarget(stage, action string, req ModelChoiceRequest) (*settings.ManualTarget, error) {
	if req.Manual == nil && req.ModelChoice != model.ModelChoiceManual {
		return nil, nil
	}
	if req.ModelChoice != model.ModelChoiceManual || req.TextOnly {
		return nil, apiErr(http.StatusBadRequest, codeManualConflict, nil)
	}
	if action != model.ActionRetry || !manualStage(stage) {
		return nil, apiErr(http.StatusBadRequest, codeManualUnsupported, map[string]any{"stage": stage, "action": action})
	}
	m := req.Manual
	switch {
	case m == nil:
		return nil, apiErr(http.StatusBadRequest, codeManualIncomplete, map[string]any{"field": "manual"})
	case strings.TrimSpace(m.ProviderID) == "":
		return nil, apiErr(http.StatusBadRequest, codeManualIncomplete, map[string]any{"field": "provider_id"})
	case strings.TrimSpace(m.Model) == "":
		return nil, apiErr(http.StatusBadRequest, codeManualIncomplete, map[string]any{"field": "model"})
	}
	return &settings.ManualTarget{
		ProviderID:  strings.TrimSpace(m.ProviderID),
		Model:       strings.TrimSpace(m.Model),
		Thinking:    m.Thinking,
		MaxTokens:   m.MaxTokens,
		Temperature: m.Temperature,
	}, nil
}
