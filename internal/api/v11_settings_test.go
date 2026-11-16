// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/settings"
)

// A5: the settings and provider endpoints; no response may contain a key.
func TestSettingsAndProvidersNeverReturnKeys(t *testing.T) {
	e := newEnv(t)
	const secret = "sk-test-secret-0123456789abcd"
	var bodies []string
	call := func(method, path string, body any, want int, out any) {
		t.Helper()
		res, b := e.do(method, path, body)
		bodies = append(bodies, string(b))
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d, want %d; %s", method, path, res.StatusCode, want, b)
		}
		if out != nil {
			decodeStrict(t, b, out)
		}
	}
	r := openSSE(t, e.ts.URL+"/api/events")
	waitClients(t, e.hub, 1)

	// v2: glossary_janitor (G3.1) is not in the legacy config and, without an
	// Anthropic provider, stays unconfigured (appendix D1); every other role
	// is imported onto the legacy provider.
	imported := make([]string, 0, len(model.Roles))
	// v2 roles are not in the legacy config (the test server does not run D1).
	v2Roles := map[string]bool{model.RoleGlossaryJanitor: true, model.RoleChapters: true, model.RoleOrganizer: true}
	for _, r := range model.Roles {
		if !v2Roles[r] {
			imported = append(imported, r)
		}
	}
	var sv Settings
	call("GET", "/api/settings", nil, http.StatusOK, &sv)
	if len(sv.Providers) != 1 || sv.Providers[0].ID != "deepseek" || sv.Providers[0].KeyHint != "sk-…-llm" ||
		!eqStrings(sv.Providers[0].UsedByRoles, imported) || sv.GPUTotalMB != 12000 ||
		sv.DefaultBaseURLs[model.ProviderAnthropic] == "" || sv.ASR.Model != "large-v3" || !sv.General.KeepOriginalMedia {
		t.Fatalf("settings %+v", sv)
	}
	var roles []string
	byRole := map[string]RoleView{}
	for _, rv := range sv.Roles {
		roles = append(roles, rv.Role)
		byRole[rv.Role] = rv
		if v2Roles[rv.Role] {
			if rv.Configured || rv.ProviderID != nil || rv.Error == "" || rv.Label == "" {
				t.Errorf("v2 role %+v", rv)
			}
			continue
		}
		if !rv.Configured || rv.ProviderID == nil || *rv.ProviderID != "deepseek" || rv.Label == "" {
			t.Errorf("role %+v", rv)
		}
	}
	if !eqStrings(roles, model.Roles) {
		t.Fatalf("roles %v", roles)
	}
	if tr, rp := byRole[model.RoleTranslate], byRole[model.RoleRepair]; tr.Temperature != 0.3 || tr.Window == nil || *tr.Window != 3 || rp.Temperature != 0.1 || rp.Window != nil {
		t.Errorf("resolved params %+v / %+v", tr, rp)
	}

	// Create, then patch with an empty key: the key is kept (A5.2).
	in := ProviderInput{Name: ptr("Claude"), Type: ptr(model.ProviderAnthropic), BaseURL: ptr(e.llm.URL), APIKey: ptr(secret),
		MaxConcurrency: ptr(2), PriceInput: ptr(3.0), PriceCacheHit: ptr(0.3), PriceOutput: ptr(15.0), Currency: ptr("USD")}
	var pv ProviderView
	call("POST", "/api/providers", in, http.StatusCreated, &pv)
	if pv.KeyHint != "sk-…abcd" || pv.Type != model.ProviderAnthropic || pv.MaxConcurrency != 2 || pv.UsedByRoles == nil || pv.Models == nil {
		t.Fatalf("created %+v", pv)
	}
	// the translate batch an empty role setting means on it (user 2026-11-11)
	if pv.DefaultBatchClaude != settings.DefaultAnthropicBatch {
		t.Errorf("anthropic default_batch_claude %d, want %d", pv.DefaultBatchClaude, settings.DefaultAnthropicBatch)
	}
	r.next(3*time.Second, func(f sseFrame) bool { return f.event == "changed" && strings.Contains(f.data, `"settings"`) })
	call("PATCH", "/api/providers/"+pv.ID, ProviderInput{Name: ptr("Claude 2"), APIKey: ptr("")}, http.StatusOK, &pv)
	if p, _ := e.set.Provider(pv.ID); p.APIKey != secret || p.Name != "Claude 2" {
		t.Fatalf("patch changed the key or ignored the name: %q", p.Name)
	}
	e.callErr("POST", "/api/providers", ProviderInput{Name: ptr("Claude 2"), Type: ptr(model.ProviderOpenAI), BaseURL: ptr(e.llm.URL)}, http.StatusConflict)
	e.callErr("POST", "/api/providers", ProviderInput{Name: ptr("X"), Type: ptr("gemini"), BaseURL: ptr(e.llm.URL)}, http.StatusBadRequest)
	e.callErr("PATCH", "/api/providers/nope", ProviderInput{Name: ptr("Y")}, http.StatusNotFound)

	// Roles, ASR, general.
	e.callErr("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: "nope", Model: "m"}, http.StatusBadRequest)
	e.callErr("PUT", "/api/settings/roles/bogus", RoleInput{ProviderID: pv.ID, Model: "m"}, http.StatusBadRequest)
	temp, win := 0.5, 2
	call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: pv.ID, Model: "claude-x", Params: model.RoleParams{Temperature: &temp, Window: &win}}, http.StatusOK, &sv)
	var tr RoleView // by name: v2 inserted glossary_janitor into model.Roles
	for _, rv := range sv.Roles {
		if rv.Role == model.RoleTranslate {
			tr = rv
		}
	}
	if tr.ProviderID == nil || *tr.ProviderID != pv.ID || *tr.Model != "claude-x" || tr.Temperature != 0.5 || *tr.Window != 2 {
		t.Fatalf("translate role %+v", tr)
	}
	asr := sv.ASR
	asr.Device = "tpu"
	e.callErr("PUT", "/api/settings/asr", asr, http.StatusBadRequest)
	asr.Device, asr.Model = "cpu", "large-v2"
	call("PUT", "/api/settings/asr", asr, http.StatusOK, &sv)
	if sv.ASR.Device != "cpu" || sv.ASR.Model != "large-v2" {
		t.Fatalf("asr %+v", sv.ASR)
	}
	e.callErr("PUT", "/api/settings/general", model.GeneralSettings{DailyBudget: -1}, http.StatusBadRequest)
	call("PUT", "/api/settings/general", model.GeneralSettings{KeepOriginalMedia: false, Notifications: true, DailyBudget: 5}, http.StatusOK, &sv)
	if sv.General.KeepOriginalMedia || sv.General.DailyBudget != 5 {
		t.Fatalf("general %+v", sv.General)
	}

	// Delete: refused while a role uses it.
	msg := e.callErr("DELETE", "/api/providers/"+pv.ID, nil, http.StatusConflict)
	if !strings.Contains(msg, "翻译") {
		t.Fatalf("delete in use: %q", msg)
	}

	// Test connection and model probe against the fake server.
	var pt ProviderTest
	call("POST", "/api/providers/deepseek/test", nil, http.StatusOK, &pt)
	if !pt.OK || pt.Model == "" || pt.Error != "" {
		t.Fatalf("test %+v", pt)
	}
	e.llm.RequireKey("sk-some-other-key-zzzz") // 401 that echoes the key it got
	call("POST", "/api/providers/deepseek/test", nil, http.StatusOK, &pt)
	if pt.OK || pt.Error == "" || strings.Contains(pt.Error, llmtest.TestKey) || pt.ErrorInfo == nil ||
		pt.ErrorInfo.Code != llm.CodeProbeHTTP || strings.Contains(pt.ErrorInfo.Detail, llmtest.TestKey) {
		t.Fatalf("failed test %+v", pt)
	}
	e.llm.RequireKey("")
	var pm ProviderModels
	call("POST", "/api/providers/deepseek/models", nil, http.StatusOK, &pm)
	if !eqStrings(pm.Models, llmtest.DefaultModels) || pm.Error != "" || pm.ProbedAt == "" {
		t.Fatalf("models %+v", pm)
	}
	e.llm.SetError(http.StatusInternalServerError, "upstream down")
	call("POST", "/api/providers/deepseek/models", nil, http.StatusOK, &pm)
	if pm.Error == "" || !eqStrings(pm.Models, llmtest.DefaultModels) || pm.ErrorInfo == nil ||
		pm.ErrorInfo.Code != llm.CodeProbeHTTP || pm.ErrorInfo.Params["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("failed probe %+v (the last good list is kept)", pm)
	}
	e.llm.SetError(0, "")
	var list []ProviderView
	call("GET", "/api/providers", nil, http.StatusOK, &list)
	var ds ProviderView
	for _, x := range list {
		if x.ID == "deepseek" {
			ds = x
		}
	}
	if len(list) != 2 || ds.ModelsError == "" || ds.ModelsProbedAt == nil || len(ds.Models) != 2 {
		t.Fatalf("providers %+v", list)
	}
	e.callErr("POST", "/api/providers/nope/test", nil, http.StatusNotFound)

	// Back to deepseek; then the unused provider can be deleted.
	call("PUT", "/api/settings/roles/translate", RoleInput{ProviderID: "deepseek", Model: "deepseek-chat"}, http.StatusOK, &sv)
	var del ProviderDeleteResult
	call("DELETE", "/api/providers/"+pv.ID, nil, http.StatusOK, &del)
	if del.SnapshotRefs != 0 || del.Warning != "" {
		t.Fatalf("delete %+v", del)
	}
	e.callErr("DELETE", "/api/providers/"+pv.ID, nil, http.StatusNotFound)

	for _, b := range bodies {
		if strings.Contains(b, secret) || strings.Contains(b, llmtest.TestKey) {
			t.Fatalf("a response contains an API key: %s", b)
		}
	}
	var sys System
	res, b := e.do("GET", "/api/system", nil)
	if res.StatusCode != http.StatusOK || strings.Contains(string(b), llmtest.TestKey) {
		t.Fatalf("system view leaks the key: %s", b)
	}
	decodeStrict(t, b, &sys)
	if cfg, _ := sys.Config.(map[string]any); cfg["llm"] != nil || cfg["asr"] == nil {
		t.Errorf("config view %v", sys.Config)
	}
}

func ptr[T any](v T) *T { return &v }

// Without the settings service the settings endpoints answer 503.
func TestSettingsEndpointsWithoutSettings(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.getSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

// A10: GET /api/resources and SSE "event: resources"; 503 without a watcher.
func TestResourcesEndpointAndEvents(t *testing.T) {
	e := newEnv(t)
	if info := e.callErrInfo("GET", "/api/resources", nil, http.StatusServiceUnavailable); info.Code != "server.unavailable" ||
		info.Params["service"] != "resources" {
		t.Fatalf("503 body %+v", info)
	}

	e = newEnvWith(t, envConfig{resources: true})
	var snap resources.Snapshot
	e.call("GET", "/api/resources", nil, http.StatusOK, &snap)
	if !snap.GPU.Available || snap.GPU.MemTotalMB == nil || *snap.GPU.MemTotalMB != 12227 || len(snap.Providers) != 1 ||
		snap.Providers[0].ID != "deepseek" || snap.GPU.ASRSlotsMax != 1 {
		t.Fatalf("snapshot %+v", snap)
	}
	r := openSSE(t, e.ts.URL+"/api/events")
	f := r.next(3*time.Second, func(f sseFrame) bool { return f.event == "resources" })
	var ev resources.Snapshot
	decodeStrict(t, []byte(f.data), &ev)
	if !ev.GPU.Available || ev.GPU.Name == "" || len(ev.Providers) != 1 {
		t.Fatalf("SSE resources %+v", ev)
	}
}

// A10.2: the balance of a provider is fetched again when one of its API
// jobs ends (once per end, not for unrelated events).
func TestBalanceRefreshAfterAPIJob(t *testing.T) {
	var hits atomic.Int32
	bal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"}]}`))
	}))
	defer bal.Close()
	e := newEnvWith(t, envConfig{resources: true})
	p, err := e.set.CreateProvider(context.Background(), providerIn("Balance", bal.URL, "sk-test-balance-0000", model.BalanceDeepSeek))
	if err != nil {
		t.Fatal(err)
	}
	openSSE(t, e.ts.URL+"/api/events") // a client: the watcher samples
	waitFor(t, func() bool { return hits.Load() == 1 })
	time.Sleep(200 * time.Millisecond)
	if hits.Load() != 1 {
		t.Fatalf("balance fetched %d times without a job end", hits.Load())
	}
	fin := "2026-10-23T10:00:00Z"
	job := model.Job{ID: "J1", Scope: model.ScopeLecture, ScopeID: "L1", Stage: model.StageTranslate, Status: model.StatusRunning,
		ConfigSnapshot: model.MarshalSnapshot(model.LLMSnapshot{ProviderID: p.ID, Model: "m"})}
	e.hub.Job(job) // still running: no refresh
	time.Sleep(200 * time.Millisecond)
	if hits.Load() != 1 {
		t.Fatalf("a running job refreshed the balance")
	}
	job.Status, job.FinishedAt = model.StatusDone, &fin
	e.hub.Job(job)
	waitFor(t, func() bool { return hits.Load() == 2 })
	e.hub.Job(job) // the same end re-emitted (e.g. emitScope) is not a new end
	time.Sleep(300 * time.Millisecond)
	if hits.Load() != 2 {
		t.Fatalf("balance fetched %d times, want 2", hits.Load())
	}
}

func providerIn(name, base, key, balance string) (in settings.ProviderInput) {
	typ := model.ProviderOpenAI
	in.Name, in.Type, in.BaseURL, in.APIKey, in.BalanceAPI = &name, &typ, &base, &key, &balance
	return in
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
