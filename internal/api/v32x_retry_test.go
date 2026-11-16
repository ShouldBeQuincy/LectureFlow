// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

func strp(s string) *string { return &s }

// v3.2.x P1-b: the retry of an LLM stage with a model named by hand.
func TestRetryManualModel(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "asr", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "summary", "queued", "running", "failed")
	before := e.set.Roles()[model.RoleSummary]
	path := "/api/lectures/" + l.ID + "/stages/summary/retry"

	// Rejected before anything is read: the field names only, never the key.
	for _, tc := range []struct {
		path string
		body any
		code string
	}{
		{path, ModelChoiceRequest{ModelChoice: model.ModelChoiceCurrent, Manual: &ManualModel{ProviderID: "deepseek", Model: "m"}}, codeManualConflict},
		{path, ModelChoiceRequest{Manual: &ManualModel{ProviderID: "deepseek", Model: "m"}}, codeManualConflict},
		{path, ModelChoiceRequest{ModelChoice: model.ModelChoiceManual}, codeManualIncomplete},
		{path, ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: &ManualModel{Model: "m"}}, codeManualIncomplete},
		{path, ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: &ManualModel{ProviderID: "deepseek", Model: "  "}}, codeManualIncomplete},
		{"/api/lectures/" + l.ID + "/stages/asr/retry", ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: &ManualModel{ProviderID: "deepseek", Model: "m"}}, codeManualUnsupported},
		{"/api/lectures/" + l.ID + "/stages/summary/rerun", ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: &ManualModel{ProviderID: "deepseek", Model: "m"}}, codeManualUnsupported},
		{"/api/lectures/" + l.ID + "/stages/title/retry", ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: &ManualModel{ProviderID: "deepseek", Model: "m"}}, codeManualUnsupported},
		// the settings page's rules (settings.ManualSnapshot)
		{path, ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: &ManualModel{ProviderID: "nope", Model: "m"}}, settings.CodeRoleProviderMissing},
		{path, ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: &ManualModel{ProviderID: "deepseek", Model: "m", MaxTokens: iptr(0)}}, settings.CodeMaxTokensInvalid},
		{path, ModelChoiceRequest{ModelChoice: model.ModelChoiceManual, Manual: &ManualModel{ProviderID: "deepseek", Model: "m", Thinking: strp("extreme")}}, settings.CodeThinkingInvalid},
	} {
		res, b := e.do("POST", tc.path, tc.body)
		if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), `"code":"`+tc.code+`"`) {
			t.Errorf("%s %+v: %d %s, want 400 %s", tc.path, tc.body, res.StatusCode, b, tc.code)
		}
		if strings.Contains(string(b), llmtest.TestKey) {
			t.Errorf("the error response shows the provider's key: %s", b)
		}
	}
	if j := e.moveJob(model.ScopeLecture, l.ID, "summary"); j.Status != model.StatusFailed {
		t.Fatalf("a rejected manual retry changed the job: %s", j.Status)
	}

	repairBefore := e.moveJob(model.ScopeLecture, l.ID, "repair").ConfigSnapshot
	var lv Lecture
	e.call("POST", path, ModelChoiceRequest{ModelChoice: model.ModelChoiceManual,
		Manual: &ManualModel{ProviderID: "deepseek", Model: " deepseek-reasoner ", MaxTokens: iptr(32768)}}, http.StatusOK, &lv)
	st := stageOf(lv.Stages, "summary")
	if st.Status != model.StatusQueued || st.Model == nil || st.Model.Source != model.SnapshotSourceManual ||
		st.Model.Model != "deepseek-reasoner" || st.Model.Params == nil || st.Model.Params.MaxTokens == nil || *st.Model.Params.MaxTokens != 32768 {
		t.Fatalf("summary after a manual retry: %+v %+v", st, st.Model)
	}
	if r := stageOf(lv.Stages, "repair").Model; r == nil || r.Source != "" {
		t.Fatalf("repair view %+v", r)
	}
	if e.moveJob(model.ScopeLecture, l.ID, "repair").ConfigSnapshot != repairBefore {
		t.Fatal("a manual retry changed the downstream snapshot")
	}
	if after := e.set.Roles()[model.RoleSummary]; *after.Model != *before.Model || after.Params != before.Params {
		t.Fatalf("a manual retry wrote the role's setting: %+v", after)
	}
	// Raw JSON: source and max_tokens on the stage's model only.
	_, b := e.do("GET", "/api/lectures/"+l.ID, nil)
	if n := strings.Count(string(b), `"source":"manual"`); n != 1 {
		t.Fatalf("%d manual sources in %s", n, b)
	}
}

// v3.2.x P1-b (Q9a): a retry without a choice asks when the settings of an
// LLM stage changed since the run (409 needs_model_choice with the diff).
func TestRetryAsksWhenSettingsChanged(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "asr", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "summary", "queued", "running", "failed")
	e.call("PUT", "/api/settings/roles/summary", RoleInput{ProviderID: "deepseek", Model: "deepseek-reasoner"}, http.StatusOK, nil)
	path := "/api/lectures/" + l.ID + "/stages/summary/retry"
	var nm NeedsModelChoice
	res, b := e.do("POST", path, nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("retry: %d %s", res.StatusCode, b)
	}
	decodeStrict(t, b, &nm)
	if nm.Error.Code != model.ErrNeedsModelChoice || len(nm.Diff) != 1 || nm.Diff[0].Stage != "summary" ||
		nm.Diff[0].Current.Model != "deepseek-reasoner" || !nm.Diff[0].SnapshotUsable {
		t.Fatalf("409 body %+v", nm)
	}
	var lv Lecture
	e.call("POST", path, ModelChoiceRequest{ModelChoice: model.ModelChoiceSnapshot}, http.StatusOK, &lv)
	if st := stageOf(lv.Stages, "summary"); st.Status != model.StatusQueued || st.Model.Model == "deepseek-reasoner" {
		t.Fatalf("summary after keeping the snapshot: %+v", st)
	}
}

// v3.2.x P1-b: "使用当前设置" when the current setting cannot be used (its
// provider has no key) is refused with 409 and the reason's code, before
// any snapshot is replaced.
func TestRetryCurrentNeedsUsableSetting(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "asr", "queued", "running", "done")
	failed := e.moveJob(model.ScopeLecture, l.ID, "summary", "queued", "running", "failed")
	name, typ, url, key := "Keyless", model.ProviderOpenAI, "https://keyless.example.test/v1", ""
	p, err := e.set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	e.call("PUT", "/api/settings/roles/summary", RoleInput{ProviderID: p.ID, Model: "m"}, http.StatusOK, nil)
	res, b := e.do("POST", "/api/lectures/"+l.ID+"/stages/summary/retry", ModelChoiceRequest{ModelChoice: model.ModelChoiceCurrent})
	if res.StatusCode != http.StatusConflict || !strings.Contains(string(b), `"code":"`+settings.CodeSnapshotNoAPIKey+`"`) ||
		!strings.Contains(string(b), `"provider":"Keyless"`) {
		t.Fatalf("retry with an unusable current setting: %d %s", res.StatusCode, b)
	}
	if j := e.moveJob(model.ScopeLecture, l.ID, "summary"); j.Status != model.StatusFailed || j.ConfigSnapshot != failed.ConfigSnapshot {
		t.Fatalf("a refused retry changed the job: %s %q", j.Status, j.ConfigSnapshot)
	}
}

// v3.2.x P1-b (Q10a): retranslate {"use_current": true} sends a sentence
// the model left without a translation to the current model; the response
// says whether the lecture now mixes two models. Without the body nothing
// changed (no "mixed" in the response).
func TestRetranslateUseCurrent(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	ctx := context.Background()
	var ss []model.Sentence
	for i := 0; i < 3; i++ {
		ss = append(ss, model.Sentence{Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i)*1000 + 900, ASRText: "s", RepairedText: "s", ZhStatus: model.ZhNone})
	}
	if err := e.st.Tx(ctx, func(tx db.Store) error { return tx.InsertSentences(ctx, l.ID, ss) }); err != nil {
		t.Fatal(err)
	}
	for i := range ss {
		zh, hash := "译", "h"
		tr := db.SentenceTr{Zh: &zh, ZhStatus: model.ZhOK, ZhHash: &hash}
		if i == 1 { // "待处理": no usable answer
			empty := ""
			tr = db.SentenceTr{Zh: &empty, ZhStatus: model.ZhQAFailed}
		}
		if err := e.st.SetTranslation(ctx, l.ID, i, tr); err != nil {
			t.Fatal(err)
		}
	}
	e.allDone(l.ID)
	old := snapshotModel(t, e.moveJob(model.ScopeLecture, l.ID, "translate"))
	e.call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: "deepseek", Model: "deepseek-reasoner"}, http.StatusOK, nil)
	base := "/api/lectures/" + l.ID + "/sentences/"

	// a sentence with a usable translation: 409, nothing changed
	info := e.callErrInfo("POST", base+"0/retranslate", RetranslateRequest{UseCurrent: true}, http.StatusConflict)
	if info.Code != codeSentenceNotPending {
		t.Fatalf("ok sentence: %+v", info)
	}
	if m := snapshotModel(t, e.moveJob(model.ScopeLecture, l.ID, "translate")); m != old {
		t.Fatalf("translate model changed to %s", m)
	}

	res, b := e.do("POST", base+"1/retranslate", RetranslateRequest{UseCurrent: true})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("use_current: %d %s", res.StatusCode, b)
	}
	var rr RetranslateResult
	decodeStrict(t, b, &rr)
	if rr.Mixed == nil || !*rr.Mixed || rr.Idx != 1 || rr.ZhStatus != model.ZhStale {
		t.Fatalf("use_current result %s", b)
	}
	if m := snapshotModel(t, e.moveJob(model.ScopeLecture, l.ID, "translate")); m != "deepseek-reasoner" {
		t.Fatalf("translate model %s", m)
	}
	// again: already the current model, not mixed by this call
	e.call("POST", base+"1/retranslate", RetranslateRequest{UseCurrent: true}, http.StatusOK, &rr)
	if rr.Mixed == nil || *rr.Mixed {
		t.Fatalf("second use_current: %+v", rr.Mixed)
	}
	// without a body: the response of SPEC §17.1 (no "mixed")
	res, b = e.do("POST", base+"2/retranslate", nil)
	if res.StatusCode != http.StatusOK || strings.Contains(string(b), "mixed") {
		t.Fatalf("plain retranslate: %d %s", res.StatusCode, b)
	}
	var sv Sentence
	decodeStrict(t, b, &sv)
}
