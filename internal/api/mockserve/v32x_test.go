// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// v3.2.x P1-b: a retry with a model named by hand, as the real handler.
func TestMockRetryManual(t *testing.T) {
	srv := testServer(t)
	url := srv.URL + "/api/lectures/" + lecFailedOR + "/stages/translate/retry"
	mk := func(pid, name string) *api.ManualModel { return &api.ManualModel{ProviderID: pid, Model: name} }
	for _, tc := range []struct {
		url  string
		body api.ModelChoiceRequest
		want string
	}{
		{url, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceCurrent, Manual: mk(pidAnthropic, "claude-opus-5-5")}, "model_choice.manual_conflict"},
		{url, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceManual}, "model_choice.manual_incomplete"},
		{url, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: mk(pidAnthropic, "")}, "model_choice.manual_incomplete"},
		{srv.URL + "/api/lectures/" + lecFailedOR + "/stages/translate/rerun", api.ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: mk(pidAnthropic, "x")}, "model_choice.manual_unsupported"},
		{url, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: mk(pidOpenRouter, "x")}, settings.CodeSnapshotNoAPIKey},
		{url, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: &api.ManualModel{ProviderID: pidAnthropic, Model: "x", MaxTokens: ip(0)}}, settings.CodeMaxTokensInvalid},
	} {
		var eb api.ErrorBody
		doJSON(t, "POST", tc.url, tc.body, 400, &eb)
		if eb.Error.Code != tc.want {
			t.Errorf("%+v: %+v, want %s", tc.body, eb.Error, tc.want)
		}
	}
	var l api.Lecture
	doJSON(t, "POST", url, api.ModelChoiceRequest{ModelChoice: model.ModelChoiceManual,
		Manual: &api.ManualModel{ProviderID: pidAnthropic, Model: "claude-opus-5-5", MaxTokens: ip(32768)}}, 200, &l)
	st := l.Stages[stageIndex(&l, model.StageTranslate)]
	if st.Status != model.StatusQueued || st.Model == nil || st.Model.Source != model.SnapshotSourceManual || st.Model.Model != "claude-opus-5-5" ||
		st.Model.Params.MaxTokens == nil || *st.Model.Params.MaxTokens != 32768 || st.Model.Params.Window == nil || *st.Model.Params.Window != 3 {
		t.Fatalf("translate after a manual retry: %+v %+v", st, st.Model)
	}
}

// v3.2.x P1-b (Q10a): retranslate with use_current.
func TestMockRetranslateUseCurrent(t *testing.T) {
	srv := testServer(t)
	base := srv.URL + "/api/lectures/" + lecDone + "/sentences/"
	var eb api.ErrorBody
	doJSON(t, "POST", base+"0/retranslate", api.RetranslateRequest{UseCurrent: true}, 409, &eb)
	if eb.Error.Code != "sentence.not_pending" {
		t.Fatalf("ok sentence: %+v", eb)
	}
	var rr api.RetranslateResult
	doJSON(t, "POST", base+"3/retranslate", api.RetranslateRequest{UseCurrent: true}, 200, &rr)
	if rr.Mixed == nil || !*rr.Mixed || rr.ZhStatus != model.ZhStale {
		t.Fatalf("use_current: %+v", rr)
	}
	var again api.RetranslateResult
	doJSON(t, "POST", base+"3/retranslate", api.RetranslateRequest{UseCurrent: true}, 200, &again)
	if again.Mixed == nil || *again.Mixed {
		t.Fatalf("second use_current: %+v", again.Mixed)
	}
	var sn api.Sentence // without the body: a plain Sentence
	doJSON(t, "POST", base+"1/retranslate", nil, 200, &sn)
}

// v3.2.x: role max_tokens / batch and the general settings' merge.
func TestMockSettingsV32x(t *testing.T) {
	srv := testServer(t)
	var sv api.Settings
	doJSON(t, "PUT", srv.URL+"/api/settings/roles/repair", api.RoleInput{ProviderID: pidDeepSeek, Model: "deepseek-chat",
		Params: model.RoleParams{MaxTokens: ip(5000), Batch: ip(3)}}, 200, &sv)
	for _, r := range sv.Roles {
		if r.Role == model.RoleRepair && (r.MaxTokens == nil || *r.MaxTokens != 5000 || r.Batch != nil) {
			t.Fatalf("repair %+v", r)
		}
	}
	var eb api.ErrorBody
	doJSON(t, "PUT", srv.URL+"/api/settings/roles/translate", api.RoleInput{ProviderID: pidAnthropic, Model: "claude-sonnet-4-5",
		Params: model.RoleParams{Batch: ip(21)}}, 400, &eb)
	if eb.Error.Code != settings.CodeBatchInvalid {
		t.Fatalf("batch 21: %+v", eb)
	}
	var g1, g2 api.Settings
	doJSON(t, "PUT", srv.URL+"/api/settings/general", map[string]any{"auto_chapters": false}, 200, &g1)
	if g := g1.General; g.AutoChapters == nil || *g.AutoChapters || g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != 300 || g.DailyBudget != 20 {
		t.Fatalf("general %+v", g)
	}
	doJSON(t, "PUT", srv.URL+"/api/settings/general", map[string]any{"llm_idle_timeout_s": nil}, 200, &g2)
	if g := g2.General; g.AutoChapters == nil || *g.AutoChapters || g.LLMIdleTimeoutS != nil {
		t.Fatalf("general after null %+v", g)
	}
}

// v3.2.x P1: CSC1003 L04's translate finished with two pending sentences:
// stats.pending_sentences and the warning, not in the lecture's usage sum.
func TestMockPendingSentences(t *testing.T) {
	srv := testServer(t)
	var l api.Lecture
	doJSON(t, "GET", srv.URL+"/api/lectures/"+lecID(c3631, 4), nil, 200, &l)
	st := l.Stages[stageIndex(&l, model.StageTranslate)]
	if st.Status != model.StatusDone || st.Stats.PendingSentences != 2 || st.WarningInfo == nil || st.WarningInfo.Code != "translate.sentences_pending" {
		t.Fatalf("translate: %+v %+v", st.Stats, st.WarningInfo)
	}
	if l.Usage.PendingSentences != 0 {
		t.Fatalf("lecture usage: %+v", l.Usage)
	}
	for _, s := range l.Stages {
		if s.Stage != model.StageTranslate && s.Stats.PendingSentences != 0 {
			t.Errorf("%s: %+v", s.Stage, s.Stats)
		}
	}
}
