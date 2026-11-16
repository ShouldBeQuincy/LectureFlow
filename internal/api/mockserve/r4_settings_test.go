// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// r4 T5: PUT /api/settings/general checks what settings.SetGeneral checks,
// with its codes: llm_idle_timeout_s 0 or MinLLMIdleTimeoutS..MaxLLMIdleTimeoutS,
// daily_budget >= 0. A refused body changes nothing.
func TestMockGeneralChecksLikeTheService(t *testing.T) {
	srv := testServer(t)
	url := srv.URL + "/api/settings/general"
	var before api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings", nil, 200, &before)
	for _, tc := range []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"llm_idle_timeout_s": 5}, settings.CodeIdleTimeoutInvalid},
		{map[string]any{"llm_idle_timeout_s": settings.MinLLMIdleTimeoutS - 1}, settings.CodeIdleTimeoutInvalid},
		{map[string]any{"llm_idle_timeout_s": -1}, settings.CodeIdleTimeoutInvalid},
		{map[string]any{"llm_idle_timeout_s": settings.MaxLLMIdleTimeoutS + 1}, settings.CodeIdleTimeoutInvalid},
		{map[string]any{"daily_budget": -1, "auto_chapters": false}, settings.CodeBudgetInvalid},
	} {
		var eb api.ErrorBody
		doJSON(t, "PUT", url, tc.body, 400, &eb)
		if eb.Error.Code != tc.code {
			t.Errorf("%v: %+v, want %s", tc.body, eb.Error, tc.code)
		}
		if tc.code == settings.CodeIdleTimeoutInvalid &&
			(eb.Error.Params["min"] != float64(settings.MinLLMIdleTimeoutS) || eb.Error.Params["max"] != float64(settings.MaxLLMIdleTimeoutS)) {
			t.Errorf("%v: params %v", tc.body, eb.Error.Params)
		}
	}
	var after api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings", nil, 200, &after)
	if b, a := before.General, after.General; a.DailyBudget != b.DailyBudget || a.AutoChaptersOn() != b.AutoChaptersOn() ||
		a.LLMIdleTimeout() != b.LLMIdleTimeout() {
		t.Fatalf("a refused PUT changed the settings: %+v, was %+v", a, b)
	}
	// The edges are accepted: 0 (off), the minimum and the maximum.
	for _, v := range []int{0, settings.MinLLMIdleTimeoutS, settings.MaxLLMIdleTimeoutS} {
		var sv api.Settings
		doJSON(t, "PUT", url, map[string]any{"llm_idle_timeout_s": v}, 200, &sv)
		if g := sv.General; g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != v {
			t.Fatalf("llm_idle_timeout_s %d: %+v", v, g)
		}
	}
}

// r4 T5: a model named by hand on a provider without a key is refused,
// except for a local server (settings.ManualSnapshot: IsLocal needs no key).
func TestMockManualLocalProviderNeedsNoKey(t *testing.T) {
	srv := testServer(t)
	name, typ, base := "Local", model.ProviderOpenAI, "http://127.0.0.1:11434/v1"
	var p api.ProviderView
	doJSON(t, "POST", srv.URL+"/api/providers", api.ProviderInput{Name: &name, Type: &typ, BaseURL: &base}, 201, &p)
	if p.KeyHint != "" {
		t.Fatalf("provider created with a key: %+v", p)
	}
	url := srv.URL + "/api/lectures/" + lecFailedOR + "/stages/translate/retry"
	var l api.Lecture
	doJSON(t, "POST", url, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceManual,
		Manual: &api.ManualModel{ProviderID: p.ID, Model: "qwen3:8b"}}, 200, &l)
	st := l.Stages[stageIndex(&l, model.StageTranslate)]
	if st.Model == nil || st.Model.ProviderID != p.ID || st.Model.Source != model.SnapshotSourceManual {
		t.Fatalf("translate after a manual retry on the local provider: %+v", st.Model)
	}
}
