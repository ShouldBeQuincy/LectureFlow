// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// r4 T5: PUT /api/settings/general refuses an llm_idle_timeout_s other than
// 0 or MinLLMIdleTimeoutS..MaxLLMIdleTimeoutS, and a negative daily_budget,
// with 400 and the settings' codes; nothing is stored (the mock follows
// these same rules).
func TestPutGeneralRefusesOutOfRange(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.call("PUT", "/api/settings/general", map[string]any{"daily_budget": 5, "llm_idle_timeout_s": 120}, http.StatusOK, nil)
	stored := func() string {
		t.Helper()
		v, _, err := e.st.GetKV(ctx, model.KVSettingsGeneral)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := stored()
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
		info := e.callErrInfo("PUT", "/api/settings/general", tc.body, http.StatusBadRequest)
		if info.Code != tc.code {
			t.Errorf("%v: %+v, want %s", tc.body, info, tc.code)
		}
		if tc.code == settings.CodeIdleTimeoutInvalid &&
			(info.Params["min"] != float64(settings.MinLLMIdleTimeoutS) || info.Params["max"] != float64(settings.MaxLLMIdleTimeoutS)) {
			t.Errorf("%v: params %v", tc.body, info.Params)
		}
	}
	if after := stored(); after != before {
		t.Fatalf("a refused PUT changed kv settings.general: %s, was %s", after, before)
	}
	if g := e.set.General(); g.DailyBudget != 5 || g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != 120 || !g.AutoChaptersOn() {
		t.Fatalf("a refused PUT changed the settings: %+v", g)
	}
	// The edges are accepted: 0 (off), the minimum and the maximum.
	for _, v := range []int{0, settings.MinLLMIdleTimeoutS, settings.MaxLLMIdleTimeoutS} {
		var sv Settings
		e.call("PUT", "/api/settings/general", map[string]any{"llm_idle_timeout_s": v}, http.StatusOK, &sv)
		if g := sv.General; g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != v {
			t.Fatalf("llm_idle_timeout_s %d: %+v", v, g)
		}
	}
}

// r4 T5: a retry with a model named by hand on a provider without a key is
// refused with 400 snapshot.no_api_key and leaves the job alone, except
// for a local server, which needs no key (settings.ManualSnapshot; the
// mock follows the same rule).
func TestRetryManualKeylessProvider(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "asr", "queued", "running", "done")
	failed := e.moveJob(model.ScopeLecture, l.ID, "summary", "queued", "running", "failed")
	create := func(name, base string) model.Provider {
		t.Helper()
		typ, key := model.ProviderOpenAI, ""
		p, err := e.set.CreateProvider(ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	remote := create("Keyless", "https://keyless.example.test/v1")
	local := create("Local", "http://127.0.0.1:11434/v1")
	path := "/api/lectures/" + l.ID + "/stages/summary/retry"

	info := e.callErrInfo("POST", path, ModelChoiceRequest{ModelChoice: model.ModelChoiceManual,
		Manual: &ManualModel{ProviderID: remote.ID, Model: "m"}}, http.StatusBadRequest)
	if info.Code != settings.CodeSnapshotNoAPIKey || info.Params["provider"] != "Keyless" {
		t.Fatalf("keyless remote provider: %+v", info)
	}
	if j := e.moveJob(model.ScopeLecture, l.ID, "summary"); j.Status != model.StatusFailed || j.ConfigSnapshot != failed.ConfigSnapshot {
		t.Fatalf("a refused retry changed the job: %s %q", j.Status, j.ConfigSnapshot)
	}

	var lv Lecture
	e.call("POST", path, ModelChoiceRequest{ModelChoice: model.ModelChoiceManual,
		Manual: &ManualModel{ProviderID: local.ID, Model: "qwen3:8b"}}, http.StatusOK, &lv)
	if st := stageOf(lv.Stages, "summary"); st.Status != model.StatusQueued || st.Model == nil ||
		st.Model.ProviderID != local.ID || st.Model.Source != model.SnapshotSourceManual {
		t.Fatalf("summary after a manual retry on the local provider: %+v %+v", st, st.Model)
	}
}
