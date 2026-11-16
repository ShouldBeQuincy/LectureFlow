// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// pidDeepSeek, pidAnthropic and pidOpenRouter are the fixture providers (fixtures_test.go).
const (
	// fakeKey is a made-up key: it must never appear in a response (A5.2).
	fakeKey = "sk-mock-not-a-real-key-9f8e7d6c"
)

func noLatency(t *testing.T) {
	t.Helper()
	old := settingsLatency
	settingsLatency = 0
	t.Cleanup(func() { settingsLatency = old })
}

// rawCall sends a JSON request and returns the status and raw body.
func rawCall(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// wantError checks a 4xx/5xx body {"error": msg}.
func wantError(t *testing.T, method, url string, body any, status int, msg string) {
	t.Helper()
	code, b := rawCall(t, method, url, body)
	var e api.ErrorBody
	if code != status || json.Unmarshal(b, &e) != nil || !strings.Contains(e.Error.Detail, msg) {
		t.Fatalf("%s %s: status %d %s, want %d containing %q", method, url, code, b, status, msg)
	}
}

func fp(f float64) *float64 { return &f }

func findProvider(ps []api.ProviderView, id string) *api.ProviderView {
	for i := range ps {
		if ps[i].ID == id {
			return &ps[i]
		}
	}
	return nil
}

func findRole(rs []api.RoleView, role string) *api.RoleView {
	for i := range rs {
		if rs[i].Role == role {
			return &rs[i]
		}
	}
	return nil
}

func TestSettingsProvidersNeverReturnKey(t *testing.T) {
	srv := testServer(t)
	in := api.ProviderInput{Name: sp(" Local DeepSeek "), Type: sp("openai"), BaseURL: sp("https://api.deepseek.com/"), APIKey: sp(fakeKey)}
	code, b := rawCall(t, "POST", srv.URL+"/api/providers", in)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	if bytes.Contains(b, []byte(fakeKey)) {
		t.Fatal("create response contains the key")
	}
	var pv api.ProviderView
	if err := json.Unmarshal(b, &pv); err != nil {
		t.Fatal(err)
	}
	if pv.Name != "Local DeepSeek" || pv.BaseURL != "https://api.deepseek.com" || pv.KeyHint != "sk-…7d6c" {
		t.Errorf("trimmed fields / key_hint: %+v", pv)
	}
	if pv.BalanceAPI != model.BalanceDeepSeek || pv.MaxConcurrency != 4 || pv.RPM != 60 {
		t.Errorf("defaults: balance %q, conc %d, rpm %d", pv.BalanceAPI, pv.MaxConcurrency, pv.RPM)
	}
	if pv.UsedByRoles == nil || pv.Models == nil {
		t.Errorf("used_by_roles and models must be [] not null")
	}

	// Every read path keeps the key out.
	for _, p := range []string{"/api/settings", "/api/providers"} {
		code, b := rawCall(t, "GET", srv.URL+p, nil)
		if code != http.StatusOK || bytes.Contains(b, []byte(fakeKey)) || !bytes.Contains(b, []byte("sk-…7d6c")) {
			t.Errorf("GET %s: %d, key leaked or hint missing", p, code)
		}
	}

	// PATCH: an empty key keeps the key; a new one changes the hint.
	var got api.ProviderView
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+pv.ID, api.ProviderInput{APIKey: sp(""), RPM: ip(90)}, http.StatusOK, &got)
	if got.KeyHint != "sk-…7d6c" || got.RPM != 90 {
		t.Errorf("patch with empty key: %+v", got)
	}
	code, b = rawCall(t, "PATCH", srv.URL+"/api/providers/"+pv.ID, api.ProviderInput{APIKey: sp("sk-other-made-up-0000aaaa")})
	if code != http.StatusOK || bytes.Contains(b, []byte("sk-other-made-up-0000aaaa")) || !bytes.Contains(b, []byte("sk-…aaaa")) {
		t.Errorf("patch with new key: %d %s", code, b)
	}

	// Prices: set, then clear.
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+pv.ID, api.ProviderInput{PriceInput: fp(2), PriceCacheHit: fp(0.5), PriceOutput: fp(8), Currency: sp(" CNY ")}, http.StatusOK, &got)
	if got.PriceInput == nil || *got.PriceInput != 2 || got.Currency == nil || *got.Currency != "CNY" {
		t.Errorf("prices: %+v", got)
	}
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+pv.ID, api.ProviderInput{ClearPrices: true, Currency: sp("")}, http.StatusOK, &got)
	if got.PriceInput != nil || got.PriceCacheHit != nil || got.PriceOutput != nil || got.Currency != nil {
		t.Errorf("clear prices: %+v", got)
	}

	// Validation (internal/settings messages).
	u := srv.URL + "/api/providers"
	wantError(t, "POST", u, api.ProviderInput{Name: sp("X")}, 400, "请填写提供商的名称、类型和 base_url")
	wantError(t, "POST", u, api.ProviderInput{Name: sp("DeepSeek"), Type: sp("openai"), BaseURL: sp("https://x.example")}, 409, "已有名为「DeepSeek」的提供商")
	wantError(t, "POST", u, api.ProviderInput{Name: sp("X"), Type: sp("gemini"), BaseURL: sp("https://x.example")}, 400, "提供商类型只能是")
	wantError(t, "POST", u, api.ProviderInput{Name: sp("X"), Type: sp("openai"), BaseURL: sp("ftp://x.example")}, 400, "base_url 必须是 http:// 或 https://")
	wantError(t, "POST", u, api.ProviderInput{Name: sp("X"), Type: sp("openai"), BaseURL: sp("https://x.example"), MaxConcurrency: ip(0)}, 400, "并发上限必须在 1 到 512 之间")
	wantError(t, "POST", u, api.ProviderInput{Name: sp("X"), Type: sp("openai"), BaseURL: sp("https://x.example"), RPM: ip(10001)}, 400, "每分钟请求数必须在 0 到 10000 之间（0 表示不限）")
	wantError(t, "POST", u, api.ProviderInput{Name: sp("X"), Type: sp("openai"), BaseURL: sp("https://x.example"), BalanceAPI: sp("stripe")}, 400, "余额接口只能是")
	wantError(t, "POST", u, api.ProviderInput{Name: sp("X"), Type: sp("openai"), BaseURL: sp("https://x.example"), PriceOutput: fp(-1)}, 400, "价格必须是不小于 0 的数")
	wantError(t, "POST", u, api.ProviderInput{Name: sp(strings.Repeat("长", 65)), Type: sp("openai"), BaseURL: sp("https://x.example")}, 400, "不能超过 64 个字符")
	wantError(t, "PATCH", u+"/"+pv.ID, api.ProviderInput{Name: sp("Anthropic")}, 409, "已有名为「Anthropic」的提供商")
	wantError(t, "PATCH", u+"/nope", api.ProviderInput{RPM: ip(5)}, 404, "找不到这个提供商")

	// A non-DeepSeek host gets balance_api none.
	doJSON(t, "POST", u, api.ProviderInput{Name: sp("Claude"), Type: sp("anthropic"), BaseURL: sp("https://api.anthropic.com")}, http.StatusCreated, &got)
	if got.BalanceAPI != model.BalanceNone || got.KeyHint != "" {
		t.Errorf("anthropic defaults: %+v", got)
	}
}

func TestSettingsRolesAndDerivedFields(t *testing.T) {
	srv := testServer(t)
	var s api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings", nil, http.StatusOK, &s)
	if len(s.Roles) != len(model.Roles) {
		t.Fatalf("%d roles", len(s.Roles))
	}
	for i, r := range s.Roles {
		if r.Role != model.Roles[i] || !r.Configured || r.Error != "" || r.Label == "" {
			t.Errorf("role %d: %+v", i, r)
		}
	}
	// snapshot_refs is derived from the lecture snapshots, like the real API.
	refs := 0
	for _, p := range s.Providers {
		refs += p.SnapshotRefs
	}
	if refs == 0 {
		t.Error("no snapshot references derived from the lectures")
	}

	// translate: Anthropic → DeepSeek; params are resolved in the view.
	u := srv.URL + "/api/settings/roles/"
	doJSON(t, "PUT", u+"translate", api.RoleInput{ProviderID: pidDeepSeek, Model: " deepseek-chat ", Params: model.RoleParams{Window: ip(5)}}, http.StatusOK, &s)
	tr := findRole(s.Roles, model.RoleTranslate)
	if tr == nil || *tr.ProviderID != pidDeepSeek || *tr.Model != "deepseek-chat" || tr.Temperature != 0.3 || tr.Window == nil || *tr.Window != 5 {
		t.Fatalf("translate after PUT: %+v", tr)
	}
	ds := findProvider(s.Providers, pidDeepSeek)
	an := findProvider(s.Providers, pidAnthropic)
	// v2 appendix D1: the fixture's glossary_janitor uses Anthropic (Opus);
	// chapters and organizer default to the summary role's DeepSeek.
	if strings.Join(ds.UsedByRoles, ",") != "glossary,summary,repair,translate,chapters,organizer" || strings.Join(an.UsedByRoles, ",") != "syllabus_format,glossary_janitor" {
		t.Errorf("used_by_roles: deepseek %v, anthropic %v", ds.UsedByRoles, an.UsedByRoles)
	}
	// A non-translate role drops the window silently.
	doJSON(t, "PUT", u+"summary", api.RoleInput{ProviderID: pidAnthropic, Model: "claude-haiku-4-5", Params: model.RoleParams{Temperature: fp(0), Window: ip(4)}}, http.StatusOK, &s)
	sm := findRole(s.Roles, model.RoleSummary)
	if sm.Window != nil || sm.Temperature != 0 || *sm.ProviderID != pidAnthropic {
		t.Errorf("summary after PUT: %+v", sm)
	}

	wantError(t, "PUT", u+"outline", api.RoleInput{ProviderID: pidDeepSeek, Model: "m"}, 400, `未知的角色 "outline"`)
	wantError(t, "PUT", u+"repair", api.RoleInput{ProviderID: "gone", Model: "m"}, 400, "请为「修复」选择一个已存在的提供商")
	wantError(t, "PUT", u+"repair", api.RoleInput{ProviderID: pidDeepSeek, Model: "  "}, 400, "请为「修复」选择或填写模型名")
	wantError(t, "PUT", u+"repair", api.RoleInput{ProviderID: pidDeepSeek, Model: "m", Params: model.RoleParams{Temperature: fp(2.5)}}, 400, "温度必须在 0 到 2 之间")
	wantError(t, "PUT", u+"translate", api.RoleInput{ProviderID: pidDeepSeek, Model: "m", Params: model.RoleParams{Window: ip(11)}}, 400, "翻译的上下文窗口必须在 0 到 10 之间")

	// The lecture endpoints see the new settings (A4.3 diffs use them).
	var nm api.NeedsModelChoice
	code, b := rawCall(t, "POST", srv.URL+"/api/lectures/"+lecDone+"/stages/translate/rerun", nil)
	if code != http.StatusConflict || json.Unmarshal(b, &nm) != nil || len(nm.Diff) == 0 || !strings.Contains(nm.Diff[0].Current.Label, "deepseek-chat") {
		t.Errorf("rerun after changing translate: %d %s", code, b)
	}
}

func TestSettingsDeleteProvider(t *testing.T) {
	srv := testServer(t)
	u := srv.URL + "/api/providers/"
	wantError(t, "DELETE", u+pidAnthropic, nil, 409, "提供商正被「大纲整理」、「术语表清道夫」、「翻译」使用，请先在角色分配中改用其他提供商")
	wantError(t, "DELETE", u+"nope", nil, 404, "找不到这个提供商")

	var before []api.ProviderView
	doJSON(t, "GET", srv.URL+"/api/providers", nil, http.StatusOK, &before)
	or := findProvider(before, pidOpenRouter)
	if or == nil || len(or.UsedByRoles) != 0 || or.SnapshotRefs == 0 {
		t.Fatalf("fixture OpenRouter: %+v", or)
	}
	var res api.ProviderDeleteResult
	doJSON(t, "DELETE", u+pidOpenRouter, nil, http.StatusOK, &res)
	if res.SnapshotRefs != or.SnapshotRefs || res.Warning != "使用该提供商的旧讲座将无法沿用原模型重跑" {
		t.Errorf("delete result: %+v", res)
	}
	var after []api.ProviderView
	doJSON(t, "GET", srv.URL+"/api/providers", nil, http.StatusOK, &after)
	if len(after) != len(before)-1 || findProvider(after, pidOpenRouter) != nil {
		t.Errorf("provider still listed")
	}

	// A provider without snapshot references: no warning.
	var pv api.ProviderView
	doJSON(t, "POST", srv.URL+"/api/providers", api.ProviderInput{Name: sp("Tmp"), Type: sp("openai"), BaseURL: sp("http://127.0.0.1:11434/v1")}, http.StatusCreated, &pv)
	res = api.ProviderDeleteResult{}
	doJSON(t, "DELETE", u+pv.ID, nil, http.StatusOK, &res)
	if res.SnapshotRefs != 0 || res.Warning != "" {
		t.Errorf("delete unreferenced: %+v", res)
	}
}

func TestSettingsASRAndGeneral(t *testing.T) {
	srv := testServer(t)
	u := srv.URL + "/api/settings/"
	a := model.ASRSettings{Model: " large-v3 ", ComputeType: "int8_float16", Device: "cpu", Language: " zh ", MaxParallel: 2, VRAMReserveMB: 2048}
	var s api.Settings
	doJSON(t, "PUT", u+"asr", a, http.StatusOK, &s)
	// phase 3b: the body names every field (a Go struct), so the merge of
	// lead ruling 15 keeps none of the current values; device_mode "" is
	// auto (settings.SetASR)
	want := model.ASRSettings{Model: "large-v3", ComputeType: "int8_float16", Device: "cpu", Language: "zh", MaxParallel: 2, VRAMReserveMB: 2048, VRAMPerJobMB: 5000,
		DeviceMode: model.ASRDeviceModeAuto}
	if !model.EqualASRSettings(s.ASR, want) {
		t.Errorf("asr = %+v, want %+v (vram_per_job_mb 0 keeps the old value)", s.ASR, want)
	}
	bad := func(mut func(*model.ASRSettings), msg string) {
		t.Helper()
		b := want
		mut(&b)
		wantError(t, "PUT", u+"asr", b, 400, msg)
	}
	bad(func(a *model.ASRSettings) { a.Model = "" }, "请填写转写模型")
	bad(func(a *model.ASRSettings) { a.ComputeType = "int8" }, "compute_type 只能是 auto、float16 或 int8_float16")
	bad(func(a *model.ASRSettings) { a.Device = "rocm" }, "设备只能是 cuda 或 cpu")
	bad(func(a *model.ASRSettings) { a.Language = " " }, "请填写语言")
	bad(func(a *model.ASRSettings) { a.MaxParallel = 9 }, "最大并行数必须在 1 到 8 之间")
	bad(func(a *model.ASRSettings) { a.VRAMReserveMB = -1 }, "保留显存必须在 0 到 65536 MB 之间")

	doJSON(t, "PUT", u+"general", model.GeneralSettings{KeepOriginalMedia: false, Notifications: false, DailyBudget: 12.5}, http.StatusOK, &s)
	// v3.2.x: the body (without the new optional fields) is merged into the
	// current settings, so the example's llm_idle_timeout_s stays.
	if g := s.General; g.KeepOriginalMedia || g.Notifications || g.DailyBudget != 12.5 || g.AutoChapters != nil ||
		g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != 300 {
		t.Errorf("general = %+v", s.General)
	}
	wantError(t, "PUT", u+"general", model.GeneralSettings{DailyBudget: -1}, 400, "每日预算必须是不小于 0 的数（0 表示不限制）")
}

func TestProviderTestAndProbe(t *testing.T) {
	noLatency(t)
	srv := testServer(t)
	u := srv.URL + "/api/providers/"

	var pt api.ProviderTest
	doJSON(t, "POST", u+pidDeepSeek+"/test", nil, http.StatusOK, &pt)
	if !pt.OK || pt.LatencyMS <= 0 || pt.Model != "deepseek-chat" || pt.Error != "" {
		t.Errorf("test deepseek: %+v", pt)
	}
	pt = api.ProviderTest{}
	doJSON(t, "POST", u+pidOpenRouter+"/test", nil, http.StatusOK, &pt)
	if pt.OK || pt.Error != "提供商「OpenRouter」没有设置 API key" {
		t.Errorf("test without key: %+v", pt)
	}
	// A provider whose base_url contains "fail" gets the server's error text.
	var pv api.ProviderView
	doJSON(t, "POST", srv.URL+"/api/providers", api.ProviderInput{Name: sp("Broken"), Type: sp("openai"), BaseURL: sp("https://fail.example/v1"), APIKey: sp(fakeKey)}, http.StatusCreated, &pv)
	code, b := rawCall(t, "POST", u+pv.ID+"/test", nil)
	if code != http.StatusOK || bytes.Contains(b, []byte(fakeKey)) || !bytes.Contains(b, []byte("HTTP 401")) {
		t.Errorf("failing test: %d %s", code, b)
	}
	wantError(t, "POST", u+"nope/test", nil, 404, "找不到这个提供商")

	// Probe: success replaces the list; failure keeps the last models.
	var pm api.ProviderModels
	doJSON(t, "POST", u+pidAnthropic+"/models", nil, http.StatusOK, &pm)
	if len(pm.Models) == 0 || pm.Error != "" || pm.ProbedAt == "" {
		t.Errorf("probe anthropic: %+v", pm)
	}
	if _, err := time.Parse(time.RFC3339, pm.ProbedAt); err != nil {
		t.Errorf("probed_at %q: %v", pm.ProbedAt, err)
	}
	var s api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings", nil, http.StatusOK, &s)
	if an := findProvider(s.Providers, pidAnthropic); strings.Join(an.Models, ",") != strings.Join(pm.Models, ",") || an.ModelsProbedAt == nil || an.ModelsError != "" {
		t.Errorf("probe result not cached: %+v", an)
	}
	pm = api.ProviderModels{}
	doJSON(t, "POST", u+pidOpenRouter+"/models", nil, http.StatusOK, &pm)
	if !strings.HasPrefix(pm.Error, "探测模型失败：") || pm.Models == nil || len(pm.Models) != 0 {
		t.Errorf("probe without key: %+v", pm)
	}
	doJSON(t, "GET", srv.URL+"/api/settings", nil, http.StatusOK, &s)
	if or := findProvider(s.Providers, pidOpenRouter); or.ModelsError != pm.Error {
		t.Errorf("probe error not cached: %+v", or)
	}
	wantError(t, "POST", u+"nope/models", nil, 404, "找不到这个提供商")
}

func TestSettingsWritesNotifyAndScenarios(t *testing.T) {
	noLatency(t)
	root := filepath.Join("..", "..", "..")
	h := newHandler(filepath.Join(root, "web"), filepath.Join(root, "testdata", "api"), 10*time.Millisecond)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ch, unsubscribe := h.(*handler).m.main.subscribe()
	defer unsubscribe()

	var s api.Settings
	doJSON(t, "PUT", srv.URL+"/api/settings/general", model.GeneralSettings{KeepOriginalMedia: true, Notifications: true, DailyBudget: 3}, http.StatusOK, &s)
	select {
	case ev := <-ch:
		if ev.Kind != "settings" {
			t.Errorf("event %+v, want kind settings", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no changed settings event")
	}

	// ?mock=unset: an unconfigured role and a deleted provider, separate from the default world.
	doJSON(t, "GET", srv.URL+"/api/settings?mock=unset", nil, http.StatusOK, &s)
	sf, tr := findRole(s.Roles, model.RoleSyllabusFormat), findRole(s.Roles, model.RoleTranslate)
	if sf.Configured || sf.Error != "请先在设置中为「大纲整理」选择提供商和模型" {
		t.Errorf("unset syllabus_format: %+v", sf)
	}
	if tr.Configured || tr.Error != "「翻译」使用的提供商已删除，请先在设置中为它重新选择提供商和模型" {
		t.Errorf("unset translate: %+v", tr)
	}
	doJSON(t, "PUT", srv.URL+"/api/settings/roles/translate?mock=unset", api.RoleInput{ProviderID: pidDeepSeek, Model: "deepseek-chat"}, http.StatusOK, &s)
	if tr := findRole(s.Roles, model.RoleTranslate); !tr.Configured {
		t.Errorf("translate not fixed: %+v", tr)
	}
	doJSON(t, "GET", srv.URL+"/api/settings", nil, http.StatusOK, &s)
	if tr := findRole(s.Roles, model.RoleTranslate); *tr.ProviderID != pidAnthropic {
		t.Errorf("the unset scenario changed the default world: %+v", tr)
	}

	// ?mock=error: the settings card's error state.
	wantError(t, "GET", srv.URL+"/api/settings?mock=error", nil, 500, "mock 模拟的失败")
}

// ?mock=oldmodel: DeepSeek lists only its current names while roles still
// use deepseek-chat; the diagnostics show the real API's
// check.role_model_unlisted row (api.RoleModelChecks), the default world none
// until DeepSeek is probed.
func TestRoleModelCheckScenario(t *testing.T) {
	noLatency(t)
	root := filepath.Join("..", "..", "..")
	srv := httptest.NewServer(newHandler(filepath.Join(root, "web"), filepath.Join(root, "testdata", "api"), 10*time.Millisecond))
	t.Cleanup(srv.Close)
	unlisted := func(query string) []api.SystemCheck {
		t.Helper()
		var sys api.System
		doJSON(t, "GET", srv.URL+"/api/system"+query, nil, http.StatusOK, &sys)
		var out []api.SystemCheck
		for _, c := range sys.Checks {
			if strings.HasPrefix(c.Name, "role_model:") {
				out = append(out, c)
			}
		}
		if n := len(sys.Checks); n == 0 || sys.Checks[n-1].Name != "disk" {
			t.Errorf("%s: the disk check stays last: %+v", query, sys.Checks)
		}
		return out
	}
	if got := unlisted(""); len(got) != 0 {
		t.Fatalf("default world: %+v", got)
	}
	got := unlisted("?mock=oldmodel")
	if len(got) != 1 || got[0].Name != "role_model:DeepSeek/deepseek-chat" || got[0].Status != "warn" ||
		got[0].DetailInfo == nil || got[0].DetailInfo.Code != "check.role_model_unlisted" || got[0].HintInfo == nil {
		t.Fatalf("oldmodel: %+v", got)
	}
	var s api.Settings
	doJSON(t, "GET", srv.URL+"/api/settings?mock=oldmodel", nil, http.StatusOK, &s)
	if p := findProvider(s.Providers, pidDeepSeek); p == nil || strings.Join(p.Models, ",") != "deepseek-flash,deepseek-v4-pro" {
		t.Fatalf("oldmodel DeepSeek: %+v", p)
	}
	// moving the roles to a listed model clears the row
	for _, r := range s.Roles {
		if r.ProviderID != nil && *r.ProviderID == pidDeepSeek {
			doJSON(t, "PUT", srv.URL+"/api/settings/roles/"+r.Role+"?mock=oldmodel", api.RoleInput{ProviderID: pidDeepSeek, Model: "deepseek-flash"}, http.StatusOK, &s)
		}
	}
	if got := unlisted("?mock=oldmodel"); len(got) != 0 {
		t.Fatalf("after the change: %+v", got)
	}
	// the default world after a probe of DeepSeek (the mock answers the current names)
	doJSON(t, "POST", srv.URL+"/api/providers/"+pidDeepSeek+"/models", nil, http.StatusOK, nil)
	if got := unlisted(""); len(got) != 1 {
		t.Fatalf("default world after the probe: %+v", got)
	}
}
