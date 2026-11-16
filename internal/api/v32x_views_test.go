// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

func iptr(v int) *int { return &v }

// setStats writes stats on the current job of stage (as the scheduler does).
func (e *env) setStats(lectureID, stage string, st model.JobStats) {
	e.t.Helper()
	j := e.moveJob(model.ScopeLecture, lectureID, stage)
	if err := e.st.SetJobStats(context.Background(), j.ID, st); err != nil {
		e.t.Fatal(err)
	}
}

func llmStatsFixture() model.JobStats {
	rt := int64(120)
	return model.JobStats{
		PromptTokens: 1000, CompletionTokens: 200,
		LastActivityAt: "2026-11-10T10:00:42Z",
		LLM: &model.LLMStats{
			InFlight: 1,
			Current:  &model.LLMAttempt{CallID: "c1", N: 2, Model: "deepseek-chat", MaxTokens: 8000, StartedAt: "2026-11-10T10:00:30Z", TimeoutMS: 120000},
			Attempts: []model.LLMAttempt{{CallID: "c1", N: 1, Model: "deepseek-chat", MaxTokens: 4000, Outcome: model.LLMOutcomeTruncated,
				CompletionTokens: 4000, ReasoningTokens: &rt}},
			Retries: 1,
		},
	}
}

// v3.2.x P1-c: StageView.llm and last_activity_at; the request in flight
// only for a running job; nothing new for a job without the data.
func TestStageViewRequestStats(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "asr", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "summary", "queued", "running")
	e.setStats(l.ID, "summary", llmStatsFixture())

	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	st := stageOf(lv.Stages, "summary")
	if st.LLM == nil || st.LLM.InFlight != 1 || st.LLM.Current == nil || st.LLM.Current.N != 2 || len(st.LLM.Attempts) != 1 ||
		st.LLM.Retries != 1 || st.LastActivityAt == nil || *st.LastActivityAt != "2026-11-10T10:00:42Z" {
		t.Fatalf("running summary: %+v %+v", st.LLM, st.LastActivityAt)
	}
	// Every other stage: no llm, no last_activity_at keys at all.
	_, b := e.do("GET", "/api/lectures/"+l.ID, nil)
	var raw struct {
		Stages []map[string]json.RawMessage `json:"stages"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, s := range raw.Stages {
		if string(s["stage"]) == `"summary"` {
			continue
		}
		for _, k := range []string{"llm", "last_activity_at"} {
			if _, ok := s[k]; ok {
				t.Errorf("stage %s has %q: %s", s["stage"], k, s[k])
			}
		}
	}

	// Not running (a crash left in_flight and current; recovery queued it):
	// attempts and retries stay, the request in flight does not.
	e.moveJob(model.ScopeLecture, l.ID, "summary", "failed")
	e.setStats(l.ID, "summary", llmStatsFixture())
	var lv2 Lecture // a fresh value: decoding into lv would keep fields of its old pointers
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv2)
	st = stageOf(lv2.Stages, "summary")
	if st.LLM == nil || st.LLM.InFlight != 0 || st.LLM.Current != nil || len(st.LLM.Attempts) != 1 || st.LLM.Retries != 1 {
		t.Fatalf("failed summary: %s %+v", st.Status, st.LLM)
	}
	// Only a request in flight, not running: nothing to show.
	only := model.JobStats{LLM: &model.LLMStats{InFlight: 2, Current: &model.LLMAttempt{N: 1, Model: "m"}}}
	e.setStats(l.ID, "summary", only)
	_, b = e.do("GET", "/api/lectures/"+l.ID, nil)
	if strings.Contains(string(b), `"llm"`) {
		t.Fatalf("a stale request in flight is shown: %s", b)
	}
}

// v3.2.x P1-c: the SSE job event of a running job carries its stats (the
// stage view's, with the estimated cost), llm and last_activity_at; other
// events stay as they were.
func TestJobEventStats(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "asr", "queued", "running", "done")
	j := e.moveJob(model.ScopeLecture, l.ID, "summary", "queued", "running")
	j.Stats = llmStatsFixture()
	var ev JobEvent
	e.srv.jobEventStats(&ev, j)
	want := e.srv.usageOf(j)
	if ev.Stats == nil || ev.Stats.PromptTokens != 1000 || ev.LLM == nil || ev.LLM.Current == nil || ev.LastActivityAt == nil ||
		(want.EstimatedCost == nil) != (ev.Stats.EstimatedCost == nil) {
		t.Fatalf("event of a running job: %+v", ev)
	}
	// Not running, or no stats: the event as before.
	for _, jj := range []model.Job{{Status: model.StatusDone, Stats: llmStatsFixture()}, {Status: model.StatusRunning, Stage: "prep", Progress: 0.5}} {
		var ev JobEvent
		e.srv.jobEventStats(&ev, jj)
		b, _ := json.Marshal(ev)
		if bytes.Contains(b, []byte("stats")) || bytes.Contains(b, []byte("llm")) || bytes.Contains(b, []byte("last_activity")) {
			t.Fatalf("event %s", b)
		}
	}
}

// The hub keeps the latest event per job: an event that changes only the
// stats (progress and status unchanged) replaces the pending one and goes
// out after MinInterval, not sooner and not never.
func TestHubStatsOnlyEventThrottled(t *testing.T) {
	h := NewHub()
	h.MinInterval = time.Second
	h.setJobStats(func(ev *JobEvent, j model.Job) {
		u := UsageStats{PromptTokens: j.Stats.PromptTokens}
		ev.Stats = &u
	})
	c := h.add(clientVisibility{visible: true})
	defer h.remove(c)
	job := model.Job{ID: "j1", Scope: model.ScopeLecture, ScopeID: "l", Stage: "summary", Status: model.StatusRunning, Progress: 0.2}
	t0 := time.Now()
	job.Stats.PromptTokens = 1
	h.Job(job)
	items, _ := c.take(t0, h.MinInterval)
	if len(items) != 1 || items[0].data.(JobEvent).Stats.PromptTokens != 1 {
		t.Fatalf("first event: %+v", items)
	}
	job.Stats.PromptTokens = 2
	h.Job(job)
	job.Stats.PromptTokens = 3
	h.Job(job)
	items, next := c.take(t0.Add(200*time.Millisecond), h.MinInterval)
	if len(items) != 0 || !next.Equal(t0.Add(time.Second)) {
		t.Fatalf("stats-only event within MinInterval: %+v next %v", items, next)
	}
	items, _ = c.take(t0.Add(time.Second), h.MinInterval)
	if len(items) != 1 || items[0].data.(JobEvent).Stats.PromptTokens != 3 || items[0].data.(JobEvent).Progress != 0.2 {
		t.Fatalf("stats-only event after MinInterval: %+v", items)
	}
}

// v3.2.x: the role's max_tokens and batch (as set; absent while unset), the
// general settings' auto_chapters and llm_idle_timeout_s (absent = default;
// a body without them keeps them; null resets one).
func TestSettingsV32x(t *testing.T) {
	e := newEnv(t)
	var sv Settings
	_, b := e.do("GET", "/api/settings", nil)
	for _, k := range []string{`"max_tokens"`, `"batch"`, `"auto_chapters"`, `"llm_idle_timeout_s"`} {
		if strings.Contains(string(b), k) {
			t.Fatalf("settings without the new values show %s: %s", k, b)
		}
	}
	e.call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: "deepseek", Model: "deepseek-chat",
		Params: model.RoleParams{MaxTokens: iptr(6000), Batch: iptr(8)}}, http.StatusOK, &sv)
	if r := roleOf(sv, model.RoleTranslate); r.MaxTokens == nil || *r.MaxTokens != 6000 || r.Batch == nil || *r.Batch != 8 {
		t.Fatalf("translate role %+v", r)
	}
	// batch is the translate role's only
	e.call("PUT", "/api/settings/roles/summary", RoleInput{ProviderID: "deepseek", Model: "deepseek-chat",
		Params: model.RoleParams{MaxTokens: iptr(9000), Batch: iptr(8)}}, http.StatusOK, &sv)
	if r := roleOf(sv, model.RoleSummary); r.MaxTokens == nil || *r.MaxTokens != 9000 || r.Batch != nil {
		t.Fatalf("summary role %+v", r)
	}
	// cleared (decoded into a fresh value)
	var sv2 Settings
	e.call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: "deepseek", Model: "deepseek-chat"}, http.StatusOK, &sv2)
	if r := roleOf(sv2, model.RoleTranslate); r.MaxTokens != nil || r.Batch != nil {
		t.Fatalf("translate role after clearing %+v", r)
	}
	for _, tc := range []struct {
		p    model.RoleParams
		code string
	}{
		{model.RoleParams{MaxTokens: iptr(0)}, settings.CodeMaxTokensInvalid},
		{model.RoleParams{Batch: iptr(21)}, settings.CodeBatchInvalid},
		{model.RoleParams{Batch: iptr(0)}, settings.CodeBatchInvalid},
	} {
		info := e.callErrInfo("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: "deepseek", Model: "deepseek-chat", Params: tc.p}, http.StatusBadRequest)
		if info.Code != tc.code || info.Params["min"] == nil || info.Params["max"] == nil {
			t.Errorf("%+v: %+v", tc.p, info)
		}
	}

	// general (each response decoded into a fresh value)
	put := func(body map[string]any) Settings {
		var out Settings
		e.call("PUT", "/api/settings/general", body, http.StatusOK, &out)
		return out
	}
	if g := put(map[string]any{"daily_budget": 5, "auto_chapters": false, "llm_idle_timeout_s": 0}).General; g.DailyBudget != 5 ||
		g.AutoChapters == nil || *g.AutoChapters || g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != 0 {
		t.Fatalf("general %+v", g)
	}
	// an older page sends daily_budget only: the new values stay
	if g := put(map[string]any{"daily_budget": 7}).General; g.DailyBudget != 7 || g.AutoChapters == nil || *g.AutoChapters || g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != 0 {
		t.Fatalf("general after an older page saved %+v", g)
	}
	if g := e.set.General(); g.AutoChaptersOn() || g.LLMIdleTimeout() != 0 {
		t.Fatalf("service %+v", g)
	}
	// null: back to the default (absent)
	if g := put(map[string]any{"auto_chapters": nil, "llm_idle_timeout_s": nil}).General; g.DailyBudget != 7 || g.AutoChapters != nil || g.LLMIdleTimeoutS != nil {
		t.Fatalf("general after null %+v", g)
	}
	if g := e.set.General(); !g.AutoChaptersOn() || g.LLMIdleTimeout() != model.DefaultLLMIdleTimeoutS*time.Second {
		t.Fatalf("service after null %+v", g)
	}
}

func roleOf(sv Settings, role string) RoleView {
	for _, r := range sv.Roles {
		if r.Role == role {
			return r
		}
	}
	return RoleView{}
}
