// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// fakeKey is a made-up key; tests assert it never appears in errors or logs.
const fakeKey = "sk-settings-test-SECRET-9f3a"

func sp(s string) *string   { return &s }
func ip(n int) *int         { return &n }
func fp(f float64) *float64 { return &f }
func bg() context.Context   { return context.Background() }
func pv(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func openStore(t *testing.T) db.Store {
	t.Helper()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// legacyConfig is a config.toml with two providers, prices, a budget and
// non-default ASR/storage values. The deepseek key comes from the environment.
func legacyConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("LF_SETTINGS_TEST_KEY", fakeKey)
	cfg := config.Default()
	cfg.LLM.Providers = map[string]config.Provider{
		"deepseek": {BaseURL: "https://api.deepseek.com/", APIKeyEnv: "LF_SETTINGS_TEST_KEY", MaxConcurrency: 8, RequestsPerMinute: 300},
		"ollama":   {BaseURL: "http://127.0.0.1:11434/v1", Local: true, MaxConcurrency: 1, RequestsPerMinute: 120},
	}
	cfg.LLM.Roles = map[string]config.Role{
		model.RoleGlossary:  {Provider: "deepseek", Model: "deepseek-chat"},
		model.RoleSummary:   {Provider: "deepseek", Model: "deepseek-chat"},
		model.RoleRepair:    {Provider: "deepseek", Model: "deepseek-reasoner"},
		model.RoleTranslate: {Provider: "deepseek", Model: "deepseek-chat", Window: 2},
	}
	cfg.LLM.Prices = map[string]map[string]config.Price{
		"deepseek": {"deepseek-chat": {Input: 2, CacheHit: 0.5, Output: 8}, "deepseek-reasoner": {Input: 4, CacheHit: 1, Output: 16}},
	}
	cfg.LLM.DailyBudget = 5
	keep := false
	cfg.Storage.KeepOriginalMedia = &keep
	cfg.ASR = config.ASR{Model: "large-v2", ComputeType: "int8_float16", Device: "cuda", Language: "en",
		VRAMPerJobMB: 4500, MaxParallel: 2, VRAMReserveMB: 800}
	return cfg
}

func TestImportLegacyOnce(t *testing.T) {
	st := openStore(t)
	cfg := legacyConfig(t)
	s := New(st, cfg)
	imported, err := s.ImportLegacy(bg())
	if err != nil || !imported {
		t.Fatalf("first import: %v %v", imported, err)
	}
	if err := s.Load(bg()); err != nil {
		t.Fatal(err)
	}

	provs := s.Providers()
	if len(provs) != 2 || provs[0].ID != "deepseek" || provs[1].ID != "ollama" {
		t.Fatalf("providers = %+v", provs)
	}
	d := provs[0]
	if d.Name != "deepseek" || d.Type != model.ProviderOpenAI || d.BaseURL != "https://api.deepseek.com" || d.APIKey != fakeKey ||
		d.MaxConcurrency != 8 || d.RPM != 300 || d.BalanceAPI != model.BalanceDeepSeek {
		t.Fatalf("deepseek = %+v", d)
	}
	// prices: the model translate uses (highest volume) wins
	if pv(d.PriceInput) != 2.0 || pv(d.PriceCacheHit) != 0.5 || pv(d.PriceOutput) != 8.0 || d.Currency != nil {
		t.Fatalf("deepseek prices = %v %v %v", pv(d.PriceInput), pv(d.PriceCacheHit), pv(d.PriceOutput))
	}
	o := provs[1]
	if o.APIKey != "" || o.BalanceAPI != model.BalanceNone || o.MaxConcurrency != 1 || o.RPM != 120 || o.HasPrices() {
		t.Fatalf("ollama = %+v", o)
	}

	roles := s.Roles()
	if len(roles) != 5 {
		t.Fatalf("roles = %+v", roles)
	}
	for role, want := range map[string]string{
		model.RoleSyllabusFormat: "deepseek-chat", // the glossary role's provider/model
		model.RoleGlossary:       "deepseek-chat",
		model.RoleSummary:        "deepseek-chat",
		model.RoleRepair:         "deepseek-reasoner",
		model.RoleTranslate:      "deepseek-chat",
	} {
		r := roles[role]
		if r.ProviderID == nil || *r.ProviderID != "deepseek" || r.Model == nil || *r.Model != want {
			t.Errorf("role %s = %+v", role, r)
		}
	}
	if roles[model.RoleTranslate].Params != `{"window":2}` || roles[model.RoleRepair].Params != `{}` {
		t.Errorf("params: translate %s repair %s", roles[model.RoleTranslate].Params, roles[model.RoleRepair].Params)
	}
	// Phase 3b: the four new fields have no config.toml key and keep their
	// defaults (device_mode auto, multi_device and battery_aware true,
	// mem_reserve_mb 2048).
	if a := s.ASR(); !model.EqualASRSettings(a, model.ASRSettings{Model: "large-v2", ComputeType: "int8_float16", Device: "cuda", Language: "en",
		MaxParallel: 2, VRAMReserveMB: 800, VRAMPerJobMB: 4500,
		DeviceMode: model.ASRDeviceModeAuto, MultiDevice: true, BatteryAware: true, MemReserveMB: 2048}) {
		t.Errorf("asr = %+v", a)
	}
	if g := s.General(); g != (model.GeneralSettings{KeepOriginalMedia: false, Notifications: true, DailyBudget: 5}) {
		t.Errorf("general = %+v", g)
	}
	if v, ok, _ := st.GetKV(bg(), model.KVSettingsImported); !ok || v != "1" {
		t.Errorf("flag = %q %v", v, ok)
	}

	// Second start, different config.toml, different environment: nothing changes.
	t.Setenv("LF_SETTINGS_TEST_KEY", "sk-other-value")
	cfg2 := config.Default()
	cfg2.LLM.Providers["another"] = config.Provider{BaseURL: "https://example.com"}
	s2, err := Open(bg(), st, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s2.ImportLegacy(bg()); again {
		t.Fatal("imported twice")
	}
	if p := s2.Providers(); len(p) != 2 || p[0].APIKey != fakeKey {
		t.Fatalf("second start providers = %+v", p)
	}
}

func TestImportSkippedWhenSettingsExist(t *testing.T) {
	// A database that already has a provider (but no flag): no import, flag set.
	st := openStore(t)
	if err := st.InsertProvider(bg(), model.Provider{ID: db.NewID(), Name: "mine", Type: model.ProviderAnthropic,
		BaseURL: "https://api.anthropic.com", MaxConcurrency: 2, RPM: 50, BalanceAPI: model.BalanceNone}); err != nil {
		t.Fatal(err)
	}
	s, err := Open(bg(), st, legacyConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if p := s.Providers(); len(p) != 1 || p[0].Name != "mine" {
		t.Fatalf("providers = %+v", p)
	}
	if _, ok, _ := st.GetKV(bg(), model.KVSettingsImported); !ok {
		t.Fatal("flag not set")
	}

	// Only kv settings.asr present: no import either.
	st2 := openStore(t)
	st2.SetKV(bg(), model.KVSettingsASR, `{"model":"small"}`)
	s2, err := Open(bg(), st2, legacyConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Providers()) != 0 || s2.ASR().Model != "small" || s2.ASR().MaxParallel != 1 {
		t.Fatalf("providers %+v asr %+v", s2.Providers(), s2.ASR())
	}

	// Default config (v1 users without config.toml): the default deepseek
	// provider with the environment's key.
	t.Setenv("DEEPSEEK_API_KEY", fakeKey)
	s3, err := Open(bg(), openStore(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := s3.Provider("deepseek"); !ok || p.APIKey != fakeKey || p.MaxConcurrency != 64 || p.RPM != 0 {
		t.Fatalf("default import: %+v", p)
	}
	// SPEC-v2 P12.1: the default model is deepseek-flash (was deepseek-chat).
	if tgt, err := s3.RoleTarget(model.RoleTranslate); err != nil || tgt.Model != "deepseek-flash" || *tgt.Params.Window != 3 {
		t.Fatalf("default translate: %+v %v", tgt, err)
	}
}

func TestLoadDefaultsAndRoleNotConfigured(t *testing.T) {
	st := openStore(t)
	st.SetKV(bg(), model.KVSettingsImported, "1")
	s, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := s.ASR(); !model.EqualASRSettings(a, DefaultASR()) || a.Model != "large-v3" || a.VRAMPerJobMB != 5000 {
		t.Fatalf("asr defaults = %+v", a)
	}
	if g := s.General(); !g.KeepOriginalMedia || !g.Notifications || g.DailyBudget != 0 {
		t.Fatalf("general defaults = %+v", g)
	}
	_, err = s.RoleTarget(model.RoleTranslate)
	if !errors.Is(err, ErrRoleNotConfigured) || !hasCode(err, CodeRoleNotConfigured, map[string]any{"role": model.RoleTranslate}) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.CurrentSnapshots(); !errors.Is(err, ErrRoleNotConfigured) {
		t.Fatalf("CurrentSnapshots: %v", err)
	}
	// Partial kv JSON keeps the defaults of the missing fields.
	st.SetKV(bg(), model.KVSettingsGeneral, `{"daily_budget": 2.5}`)
	st.SetKV(bg(), model.KVSettingsASR, `{"device":"cpu","max_parallel":0}`)
	s.Load(bg())
	if g := s.General(); !g.KeepOriginalMedia || !g.Notifications || g.DailyBudget != 2.5 {
		t.Fatalf("general = %+v", g)
	}
	if a := s.ASR(); a.Device != "cpu" || a.MaxParallel != 1 || a.Model != "large-v3" {
		t.Fatalf("asr = %+v", a)
	}
}

// newService opens an empty (already imported) settings service.
func newService(t *testing.T) (*Service, db.Store) {
	t.Helper()
	st := openStore(t)
	st.SetKV(bg(), model.KVSettingsImported, "1")
	s, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func create(t *testing.T, s *Service, name, typ, base, key string) model.Provider {
	t.Helper()
	p, err := s.CreateProvider(bg(), ProviderInput{Name: sp(name), Type: sp(typ), BaseURL: sp(base), APIKey: sp(key)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRoleTargetResolution(t *testing.T) {
	s, _ := newService(t)
	p := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	for _, role := range model.Roles {
		if err := s.SetRole(bg(), role, p.ID, " deepseek-chat ", model.RoleParams{}); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := s.RoleTarget(model.RoleTranslate)
	if err != nil || tr.ProviderID != p.ID || tr.ProviderName != "DeepSeek" || tr.Model != "deepseek-chat" ||
		*tr.Params.Temperature != 0.3 || *tr.Params.Window != 3 {
		t.Fatalf("translate default = %+v %v", tr, err)
	}
	rp, _ := s.RoleTarget(model.RoleRepair)
	if *rp.Params.Temperature != 0.1 || rp.Params.Window != nil {
		t.Fatalf("repair = %+v", rp.Params)
	}
	sf, _ := s.RoleTarget(model.RoleSyllabusFormat)
	if *sf.Params.Temperature != 0.2 {
		t.Fatalf("syllabus_format temp = %v", *sf.Params.Temperature)
	}
	// explicit values, including window 0 and temperature 0
	if err := s.SetRole(bg(), model.RoleTranslate, p.ID, "m2", model.RoleParams{Temperature: fp(0), Window: ip(0)}); err != nil {
		t.Fatal(err)
	}
	tr, _ = s.RoleTarget(model.RoleTranslate)
	if tr.Model != "m2" || *tr.Params.Temperature != 0 || *tr.Params.Window != 0 {
		t.Fatalf("explicit = %+v", tr.Params)
	}
	// a window on a non-translate role is dropped
	if err := s.SetRole(bg(), model.RoleSummary, p.ID, "m", model.RoleParams{Window: ip(5)}); err != nil {
		t.Fatal(err)
	}
	if r := s.Roles()[model.RoleSummary]; r.Params != `{}` {
		t.Fatalf("summary params = %s", r.Params)
	}
	// validation
	for _, tc := range []struct {
		role, pid, model string
		params           model.RoleParams
		code             string
	}{
		{"nosuch", p.ID, "m", model.RoleParams{}, CodeRoleUnknown},
		{model.RoleSummary, "missing", "m", model.RoleParams{}, CodeRoleProviderMissing},
		{model.RoleSummary, "", "m", model.RoleParams{}, CodeRoleProviderMissing},
		{model.RoleSummary, p.ID, "  ", model.RoleParams{}, CodeRoleModelMissing},
		{model.RoleSummary, p.ID, "m", model.RoleParams{Temperature: fp(2.1)}, CodeTemperatureInvalid},
		{model.RoleSummary, p.ID, "m", model.RoleParams{Temperature: fp(-0.1)}, CodeTemperatureInvalid},
		{model.RoleTranslate, p.ID, "m", model.RoleParams{Window: ip(11)}, CodeWindowInvalid},
		{model.RoleTranslate, p.ID, "m", model.RoleParams{Window: ip(-1)}, CodeWindowInvalid},
	} {
		if err := s.SetRole(bg(), tc.role, tc.pid, tc.model, tc.params); !errors.Is(err, ErrInvalid) || codeOf(err) != tc.code {
			t.Errorf("SetRole(%s, %q, %q, %+v) = %v, want ErrInvalid %s", tc.role, tc.pid, tc.model, tc.params, err, tc.code)
		}
	}

	// A role whose provider vanished from the cache (defensive path).
	s.mu.Lock()
	s.providers = nil
	s.mu.Unlock()
	if _, err := s.RoleTarget(model.RoleSummary); !errors.Is(err, ErrRoleNotConfigured) || !hasCode(err, CodeRoleProviderDeleted, map[string]any{"role": model.RoleSummary}) {
		t.Fatalf("vanished provider: %v", err)
	}
}

func TestCurrentSnapshots(t *testing.T) {
	s, _ := newService(t)
	p := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	s.SetRole(bg(), model.RoleSummary, p.ID, "deepseek-chat", model.RoleParams{})
	s.SetRole(bg(), model.RoleRepair, p.ID, "deepseek-chat", model.RoleParams{Temperature: fp(0.05)})
	if _, err := s.CurrentSnapshots(); !errors.Is(err, ErrRoleNotConfigured) || !hasCode(err, CodeRoleNotConfigured, map[string]any{"role": model.RoleTranslate}) {
		t.Fatalf("missing translate: %v", err)
	}
	s.SetRole(bg(), model.RoleTranslate, p.ID, "deepseek-chat", model.RoleParams{Window: ip(2)})
	s.SetASR(bg(), model.ASRSettings{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "en", MaxParallel: 1, VRAMReserveMB: 1000})
	snaps, err := s.CurrentSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	// v2 G5.5: LLM snapshots carry the stage's prompt version.
	want := map[string]string{
		model.StagePrep:      "{}",
		model.StageASR:       `{"model":"large-v2","compute_type":"auto","device":"cuda","language":"en"}`,
		model.StageSummary:   `{"provider_id":"` + p.ID + `","provider_name":"DeepSeek","model":"deepseek-chat","params":{"temperature":0.2},"prompt_version":2}`,
		model.StageRepair:    `{"provider_id":"` + p.ID + `","provider_name":"DeepSeek","model":"deepseek-chat","params":{"temperature":0.05},"prompt_version":2}`,
		model.StageTranslate: `{"provider_id":"` + p.ID + `","provider_name":"DeepSeek","model":"deepseek-chat","params":{"temperature":0.3,"window":2},"prompt_version":2}`,
		model.StageExport:    "{}",
	}
	if len(snaps) != len(want) {
		t.Fatalf("snapshots = %v", snaps)
	}
	for stage, w := range want {
		if snaps[stage] != w {
			t.Errorf("%s:\n got %s\nwant %s", stage, snaps[stage], w)
		}
		if !model.SameSnapshot(stage, snaps[stage], w) {
			t.Errorf("%s: SameSnapshot false", stage)
		}
	}
	// Renaming the provider changes the display name only (A4.3 ruling 5).
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{Name: sp("DS")}); err != nil {
		t.Fatal(err)
	}
	snaps2, _ := s.CurrentSnapshots()
	if !strings.Contains(snaps2[model.StageSummary], `"provider_name":"DS"`) || !model.SameSnapshot(model.StageSummary, snaps[model.StageSummary], snaps2[model.StageSummary]) {
		t.Fatalf("after rename: %s", snaps2[model.StageSummary])
	}
}

func TestSnapshotUsable(t *testing.T) {
	s, _ := newService(t)
	withKey := create(t, s, "WithKey", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	noKey := create(t, s, "NoKey", model.ProviderAnthropic, "https://api.anthropic.com", "")
	local := create(t, s, "Ollama", model.ProviderOpenAI, "http://localhost:11434/v1", "")
	local6 := create(t, s, "Local6", model.ProviderOpenAI, "http://[::1]:8080", "")
	snap := func(p model.Provider) string {
		return model.MarshalSnapshot(model.LLMSnapshot{ProviderID: p.ID, ProviderName: p.Name, Model: "m", Params: model.RoleParams{Temperature: fp(0.2)}})
	}
	cases := []struct {
		stage, raw string
		ok         bool
		code       string // of the reason (v2 P14.3)
		provider   string // its provider param
	}{
		{model.StageTranslate, snap(withKey), true, "", ""},
		{model.StageSummary, snap(local), true, "", ""},
		{model.StageRepair, snap(local6), true, "", ""},
		{model.StageRepair, snap(noKey), false, CodeSnapshotNoAPIKey, "NoKey"},
		{model.StageTranslate, `{"provider_id":"gone","provider_name":"Old DeepSeek","model":"m"}`, false, CodeSnapshotProviderDeleted, "Old DeepSeek"},
		{model.StageTranslate, `{"provider_id":"gone2","model":"m"}`, false, CodeSnapshotProviderDeleted, "gone2"},
		{model.StageSummary, `{"provider_id": 5}`, false, CodeSnapshotInvalid, ""},
		{model.StageSummary, "{}", false, CodeSnapshotNotRecorded, ""},
		{model.StageSummary, "", false, CodeSnapshotNotRecorded, ""},
		{model.StageASR, `{"model":"large-v3"}`, true, "", ""},
		{model.StagePrep, "{}", true, "", ""},
	}
	for _, tc := range cases {
		ok, info := s.SnapshotUsableInfo(tc.stage, tc.raw)
		code, provider := "", ""
		if info != nil {
			code = info.Code
			provider, _ = info.Params["provider"].(string)
		}
		if ok != tc.ok || code != tc.code || provider != tc.provider || (info != nil && info.Detail == "") {
			t.Errorf("SnapshotUsableInfo(%s, %s) = %v %+v, want %v %s %q", tc.stage, tc.raw, ok, info, tc.ok, tc.code, tc.provider)
		}
		// SnapshotUsable gives the same answer with the detail as the reason.
		ok2, reason := s.SnapshotUsable(tc.stage, tc.raw)
		if ok2 != ok || (info == nil && reason != "") || (info != nil && reason != info.Detail) {
			t.Errorf("SnapshotUsable(%s, %s) = %v %q, info %+v", tc.stage, tc.raw, ok2, reason, info)
		}
	}
	// Adding the key makes the snapshot usable again.
	if _, err := s.UpdateProvider(bg(), noKey.ID, ProviderInput{APIKey: sp("sk-new-key-0000")}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.SnapshotUsable(model.StageRepair, snap(noKey)); !ok {
		t.Fatal("usable after the key was added")
	}
}

func TestProviderCreateAndUpdate(t *testing.T) {
	s, st := newService(t)
	var hooks atomic.Int32
	s.OnChange(func() {
		hooks.Add(1)
		_ = s.Providers() // a hook may read settings (no lock held)
	})

	p, err := s.CreateProvider(bg(), ProviderInput{Name: sp(" DeepSeek "), Type: sp(model.ProviderOpenAI),
		BaseURL: sp("https://api.deepseek.com/ "), APIKey: sp(" " + fakeKey + "\n"),
		PriceInput: fp(2), PriceCacheHit: fp(0.5), PriceOutput: fp(8), Currency: sp("CNY")})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ID) != 26 || p.Name != "DeepSeek" || p.BaseURL != "https://api.deepseek.com" || p.APIKey != fakeKey ||
		p.MaxConcurrency != DefaultMaxConcurrency || p.RPM != DefaultRPM || p.BalanceAPI != model.BalanceDeepSeek ||
		!p.HasPrices() || *p.Currency != "CNY" || p.CreatedAt == "" {
		t.Fatalf("created = %+v", p)
	}
	if got, err := st.GetProvider(bg(), p.ID); err != nil || got.APIKey != fakeKey {
		t.Fatalf("stored = %+v %v", got, err)
	}
	if c, ok := s.Provider(p.ID); !ok || c.Name != "DeepSeek" {
		t.Fatal("cache not refreshed")
	}
	if hooks.Load() != 1 {
		t.Fatalf("hooks = %d", hooks.Load())
	}
	a := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", "")
	if a.BalanceAPI != model.BalanceNone || a.APIKey != "" {
		t.Fatalf("anthropic = %+v", a)
	}

	bad := []ProviderInput{
		{Name: sp("x"), Type: sp("openai")}, // base_url missing
		{Name: sp(" "), Type: sp("openai"), BaseURL: sp("https://a.b")},
		{Name: sp("x"), Type: sp("gemini"), BaseURL: sp("https://a.b")},
		{Name: sp("x"), Type: sp("openai"), BaseURL: sp("ftp://a.b")},
		{Name: sp("x"), Type: sp("openai"), BaseURL: sp("api.deepseek.com")},
		{Name: sp("x"), Type: sp("openai"), BaseURL: sp("https://a.b"), MaxConcurrency: ip(0)},
		{Name: sp("x"), Type: sp("openai"), BaseURL: sp("https://a.b"), MaxConcurrency: ip(MaxConcurrencyLimit + 1)},
		{Name: sp("x"), Type: sp("openai"), BaseURL: sp("https://a.b"), RPM: ip(-1)}, // 0 = no limit (perf-llm)
		{Name: sp("x"), Type: sp("openai"), BaseURL: sp("https://a.b"), RPM: ip(10001)},
		{Name: sp("x"), Type: sp("openai"), BaseURL: sp("https://a.b"), BalanceAPI: sp("openai")},
		{Name: sp("x"), Type: sp("openai"), BaseURL: sp("https://a.b"), PriceOutput: fp(-1)},
		{Name: sp(strings.Repeat("名", 65)), Type: sp("openai"), BaseURL: sp("https://a.b")},
	}
	badCodes := []string{CodeProviderFieldsMissing, CodeProviderNameEmpty, CodeProviderTypeInvalid, CodeBaseURLInvalid,
		CodeBaseURLInvalid, CodeConcurrencyInvalid, CodeConcurrencyInvalid, CodeRPMInvalid, CodeRPMInvalid,
		CodeBalanceAPIInvalid, CodePriceInvalid, CodeProviderNameTooLong}
	for i, in := range bad {
		in.APIKey = sp(fakeKey)
		if _, err := s.CreateProvider(bg(), in); !errors.Is(err, ErrInvalid) || codeOf(err) != badCodes[i] {
			t.Errorf("bad input %d: %v, want %s", i, err, badCodes[i])
		}
	}
	_, err = s.CreateProvider(bg(), ProviderInput{Name: sp("DeepSeek"), Type: sp("openai"), BaseURL: sp("https://x.y"), APIKey: sp(fakeKey)})
	if !errors.Is(err, db.ErrDuplicate) || !hasCode(err, CodeProviderNameTaken, map[string]any{"name": "DeepSeek"}) {
		t.Fatalf("duplicate: %v", err)
	}
	if hooks.Load() != 2 {
		t.Fatalf("failed writes must not call hooks: %d", hooks.Load())
	}

	// patch: empty key keeps the key; other fields change
	u, err := s.UpdateProvider(bg(), p.ID, ProviderInput{APIKey: sp(""), MaxConcurrency: ip(16), RPM: ip(600), Currency: sp("")})
	if err != nil || u.APIKey != fakeKey || u.MaxConcurrency != 16 || u.RPM != 600 || u.Currency != nil || u.Name != "DeepSeek" || !u.HasPrices() {
		t.Fatalf("patch = %+v %v", u, err)
	}
	u, err = s.UpdateProvider(bg(), p.ID, ProviderInput{APIKey: sp("sk-replaced-1234"), ClearPrices: true})
	if err != nil || u.APIKey != "sk-replaced-1234" || u.PriceInput != nil || u.PriceCacheHit != nil || u.PriceOutput != nil {
		t.Fatalf("key/prices = %+v %v", u, err)
	}
	if c, _ := s.Provider(p.ID); c.APIKey != "sk-replaced-1234" || c.MaxConcurrency != 16 {
		t.Fatalf("cache = %+v", c)
	}
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{Name: sp("Claude")}); !errors.Is(err, db.ErrDuplicate) {
		t.Fatalf("rename to existing: %v", err)
	}
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{RPM: ip(-1)}); !errors.Is(err, ErrInvalid) { // 0 = no limit (perf-llm)
		t.Fatalf("invalid patch: %v", err)
	}
	if _, err := s.UpdateProvider(bg(), "missing", ProviderInput{Name: sp("z")}); !errors.Is(err, db.ErrNotFound) || codeOf(err) != CodeProviderNotFound {
		t.Fatalf("missing: %v", err)
	}
	if c, _ := s.Provider(p.ID); c.Name != "DeepSeek" || c.RPM != 600 {
		t.Fatalf("failed patches changed the cache: %+v", c)
	}
	// Returned values are copies.
	c, _ := s.Provider(a.ID)
	c.Name = "mutated"
	if c2, _ := s.Provider(a.ID); c2.Name != "Claude" {
		t.Fatal("Provider returned a shared value")
	}
	if hooks.Load() != 4 {
		t.Fatalf("hooks = %d, want 4", hooks.Load())
	}
}

func TestDeleteProvider(t *testing.T) {
	s, st := newService(t)
	p := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	other := create(t, s, "Other", model.ProviderOpenAI, "https://api.example.com", fakeKey)
	s.SetRole(bg(), model.RoleSummary, p.ID, "deepseek-chat", model.RoleParams{})
	s.SetRole(bg(), model.RoleTranslate, p.ID, "deepseek-chat", model.RoleParams{})
	if got := s.UsedByRoles(p.ID); len(got) != 2 || got[0] != model.RoleSummary || got[1] != model.RoleTranslate {
		t.Fatalf("UsedByRoles = %v", got)
	}
	_, err := s.DeleteProvider(bg(), p.ID)
	if !errors.Is(err, ErrProviderInUse) || !hasCode(err, CodeProviderInUse, map[string]any{"roles": []string{model.RoleSummary, model.RoleTranslate}}) {
		t.Fatalf("in use: %v", err)
	}
	if _, ok := s.Provider(p.ID); !ok {
		t.Fatal("deleted although in use")
	}

	// Two lectures whose summary/translate snapshots name the provider.
	course := model.Course{ID: db.NewID(), Name: "C"}
	st.CreateCourse(bg(), course)
	snap := model.MarshalSnapshot(model.LLMSnapshot{ProviderID: p.ID, ProviderName: "DeepSeek", Model: "deepseek-chat"})
	for i := 0; i < 2; i++ {
		l := model.Lecture{ID: db.NewID(), CourseID: course.ID, Title: "L", MediaPath: "m.mp4"}
		if err := st.CreateLecture(bg(), l); err != nil {
			t.Fatal(err)
		}
		for _, stage := range []string{model.StageSummary, model.StageTranslate} {
			j, _ := st.GetCurrentJob(bg(), model.ScopeLecture, l.ID, stage)
			st.SetJobSnapshot(bg(), j.ID, snap)
		}
		j, _ := st.GetCurrentJob(bg(), model.ScopeLecture, l.ID, model.StageRepair)
		st.SetJobSnapshot(bg(), j.ID, model.MarshalSnapshot(model.LLMSnapshot{ProviderID: other.ID, Model: "x"}))
	}
	if n, err := s.SnapshotRefs(bg(), p.ID); err != nil || n != 4 {
		t.Fatalf("SnapshotRefs = %d %v", n, err)
	}
	s.SetProbedModels(bg(), p.ID, model.ProbedModels{Models: []string{"deepseek-chat"}})

	s.SetRole(bg(), model.RoleSummary, other.ID, "x", model.RoleParams{})
	s.SetRole(bg(), model.RoleTranslate, other.ID, "x", model.RoleParams{})
	var hooks atomic.Int32
	s.OnChange(func() { hooks.Add(1) })
	info, err := s.DeleteProvider(bg(), p.ID)
	if err != nil || info.SnapshotRefs != 4 {
		t.Fatalf("delete = %+v %v", info, err)
	}
	if _, ok := s.Provider(p.ID); ok || hooks.Load() != 1 {
		t.Fatalf("cache still has it / hooks %d", hooks.Load())
	}
	if _, err := st.GetProvider(bg(), p.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("row: %v", err)
	}
	if _, ok, err := s.ProbedModels(bg(), p.ID); ok || err != nil {
		t.Fatalf("probe cache kept: %v %v", ok, err)
	}
	// Old snapshots are now unusable (A5.3 / A4.3).
	if ok, info := s.SnapshotUsableInfo(model.StageSummary, snap); ok || info == nil || info.Code != CodeSnapshotProviderDeleted || info.Params["provider"] != "DeepSeek" {
		t.Fatalf("usable after delete: %v %+v", ok, info)
	}
	if _, err := s.DeleteProvider(bg(), p.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestSetASRAndGeneral(t *testing.T) {
	s, st := newService(t)
	var hooks atomic.Int32
	s.OnChange(func() { hooks.Add(1) })
	good := model.ASRSettings{Model: " large-v2 ", ComputeType: "float16", Device: "cuda", Language: "en", MaxParallel: 2, VRAMReserveMB: 0}
	if err := s.SetASR(bg(), good); err != nil {
		t.Fatal(err)
	}
	if a := s.ASR(); a.Model != "large-v2" || a.VRAMPerJobMB != 5000 || a.VRAMReserveMB != 0 || a.MaxParallel != 2 {
		t.Fatalf("asr = %+v (VRAMPerJobMB 0 keeps the old value)", a)
	}
	good.VRAMPerJobMB = 6000
	s.SetASR(bg(), good)
	good.VRAMPerJobMB = 0
	s.SetASR(bg(), good)
	if a := s.ASR(); a.VRAMPerJobMB != 6000 {
		t.Fatalf("vram_per_job = %d", a.VRAMPerJobMB)
	}
	asrCodes := []string{CodeASRModelMissing, CodeComputeTypeInvalid, CodeDeviceInvalid, CodeLanguageMissing,
		CodeMaxParallelInvalid, CodeMaxParallelInvalid, CodeVRAMReserveInvalid, CodeVRAMReserveInvalid, CodeVRAMPerJobInvalid}
	for i, a := range []model.ASRSettings{
		{Model: "", ComputeType: "auto", Device: "cuda", Language: "en", MaxParallel: 1},
		{Model: "m", ComputeType: "int8", Device: "cuda", Language: "en", MaxParallel: 1},
		{Model: "m", ComputeType: "auto", Device: "rocm", Language: "en", MaxParallel: 1},
		{Model: "m", ComputeType: "auto", Device: "cpu", Language: " ", MaxParallel: 1},
		{Model: "m", ComputeType: "auto", Device: "cpu", Language: "en", MaxParallel: 0},
		{Model: "m", ComputeType: "auto", Device: "cpu", Language: "en", MaxParallel: 9},
		{Model: "m", ComputeType: "auto", Device: "cpu", Language: "en", MaxParallel: 1, VRAMReserveMB: -1},
		{Model: "m", ComputeType: "auto", Device: "cpu", Language: "en", MaxParallel: 1, VRAMReserveMB: 70000},
		{Model: "m", ComputeType: "auto", Device: "cpu", Language: "en", MaxParallel: 1, VRAMPerJobMB: -1},
	} {
		if err := s.SetASR(bg(), a); !errors.Is(err, ErrInvalid) || codeOf(err) != asrCodes[i] {
			t.Errorf("asr %d: %v, want %s", i, err, asrCodes[i])
		}
	}
	if err := s.SetGeneral(bg(), model.GeneralSettings{KeepOriginalMedia: false, Notifications: false, DailyBudget: 3.5}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGeneral(bg(), model.GeneralSettings{DailyBudget: -1}); !errors.Is(err, ErrInvalid) || codeOf(err) != CodeBudgetInvalid {
		t.Fatalf("negative budget: %v", err)
	}
	if hooks.Load() != 4 {
		t.Fatalf("hooks = %d", hooks.Load())
	}
	// persisted
	s2, _ := Open(bg(), st, nil)
	if g := s2.General(); g != (model.GeneralSettings{DailyBudget: 3.5}) || s2.ASR().VRAMPerJobMB != 6000 || s2.ASR().ComputeType != "float16" {
		t.Fatalf("reloaded: %+v %+v", g, s2.ASR())
	}
}

func TestProbedModelsAndTestModel(t *testing.T) {
	s, _ := newService(t)
	p := create(t, s, "P", model.ProviderOpenAI, "https://api.example.com", fakeKey)
	if _, ok, err := s.ProbedModels(bg(), p.ID); ok || err != nil {
		t.Fatalf("empty cache: %v %v", ok, err)
	}
	if m := s.TestModel(bg(), p.ID); m != "" {
		t.Fatalf("TestModel without anything = %q", m)
	}
	if err := s.SetProbedModels(bg(), p.ID, model.ProbedModels{Models: []string{"a-model", "b-model"}}); err != nil {
		t.Fatal(err)
	}
	pm, ok, err := s.ProbedModels(bg(), p.ID)
	if err != nil || !ok || len(pm.Models) != 2 || pm.ProbedAt == "" {
		t.Fatalf("probed = %+v %v %v", pm, ok, err)
	}
	if m := s.TestModel(bg(), p.ID); m != "a-model" {
		t.Fatalf("TestModel from probe = %q", m)
	}
	s.SetRole(bg(), model.RoleRepair, p.ID, "role-model", model.RoleParams{})
	if m := s.TestModel(bg(), p.ID); m != "role-model" {
		t.Fatalf("TestModel from role = %q", m)
	}
	// a failed probe is cached with its error and an empty list
	s.SetProbedModels(bg(), p.ID, model.ProbedModels{Error: "HTTP 401"})
	pm, _, _ = s.ProbedModels(bg(), p.ID)
	if pm.Error != "HTTP 401" || pm.Models == nil || len(pm.Models) != 0 {
		t.Fatalf("failed probe = %+v", pm)
	}
}

// T041: moving a provider to another base_url (or API type) drops the probe
// cache of the old address; a rename, key or limit change keeps it. The
// OnChange hooks (the "changed" event) run either way.
func TestUpdateProviderProbeCache(t *testing.T) {
	s, _ := newService(t)
	p := create(t, s, "P", model.ProviderOpenAI, "https://api.example.com/v1", fakeKey)
	var hooks atomic.Int32
	s.OnChange(func() { hooks.Add(1) })
	probe := func() {
		t.Helper()
		if err := s.SetProbedModels(bg(), p.ID, model.ProbedModels{Error: "HTTP 404"}); err != nil {
			t.Fatal(err)
		}
	}
	cached := func() bool {
		t.Helper()
		_, ok, err := s.ProbedModels(bg(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	update := func(in ProviderInput) {
		t.Helper()
		if _, err := s.UpdateProvider(bg(), p.ID, in); err != nil {
			t.Fatal(err)
		}
	}

	probe()
	conc, rpm := 3, 30
	update(ProviderInput{Name: sp("Renamed"), APIKey: sp("sk-test-other"), MaxConcurrency: &conc, RPM: &rpm})
	if !cached() || hooks.Load() != 1 {
		t.Fatalf("rename / key / limits: cache kept = %v, hooks %d", cached(), hooks.Load())
	}
	update(ProviderInput{BaseURL: sp("https://api.example.com/v1")}) // same address
	if !cached() {
		t.Fatal("an unchanged base_url dropped the cache")
	}
	update(ProviderInput{BaseURL: sp("https://api.example.com/v2")}) // same origin, other path
	if cached() || hooks.Load() != 3 {
		t.Fatalf("new base_url: cache kept = %v, hooks %d", cached(), hooks.Load())
	}
	probe()
	update(ProviderInput{BaseURL: sp("https://other.example.org/v1")}) // another host
	if cached() {
		t.Fatal("another host kept the cache")
	}
	probe()
	update(ProviderInput{Type: sp(model.ProviderAnthropic)})
	if cached() {
		t.Fatal("a new type kept the cache")
	}
	if got, _ := s.Provider(p.ID); got.Name != "Renamed" {
		t.Fatalf("name = %q", got.Name)
	}
}

func TestReadsNeverTouchTheDatabase(t *testing.T) {
	st := openStore(t)
	s, err := Open(bg(), st, legacyConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	// Hold the only database connection: cached reads must not wait for it.
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- st.Tx(bg(), func(tx db.Store) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	// A write queued behind the transaction must not block readers either.
	writeDone := make(chan error, 1)
	go func() { writeDone <- s.SetGeneral(bg(), model.GeneralSettings{DailyBudget: 1}) }()
	time.Sleep(20 * time.Millisecond)
	readsDone := make(chan struct{})
	go func() {
		defer close(readsDone)
		_ = s.Providers()
		_, _ = s.Provider("deepseek")
		_ = s.Roles()
		_ = s.ASR()
		_ = s.General()
		_ = s.UsedByRoles("deepseek")
		_ = s.RoleDefault(model.RoleGlossaryJanitor)
		if _, err := s.RoleTarget(model.RoleTranslate); err != nil {
			t.Error(err)
		}
		if _, err := s.CurrentSnapshots(); err != nil {
			t.Error(err)
		}
		_, _ = s.SnapshotUsable(model.StageSummary, `{"provider_id":"deepseek","model":"m"}`)
	}()
	select {
	case <-readsDone:
	case <-time.After(3 * time.Second):
		t.Fatal("cached reads blocked on the database")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if s.General().DailyBudget != 1 {
		t.Fatal("queued write lost")
	}
}

func TestConcurrentWritesKeepCacheFresh(t *testing.T) {
	s, st := newService(t)
	p := create(t, s, "P", model.ProviderOpenAI, "https://api.example.com", fakeKey)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{RPM: ip(n)}); err != nil {
				t.Error(err)
			}
			_ = s.Providers()
		}(i)
	}
	wg.Wait()
	stored, _ := st.GetProvider(bg(), p.ID)
	if c, _ := s.Provider(p.ID); c.RPM != stored.RPM {
		t.Fatalf("cache rpm %d, database %d", c.RPM, stored.RPM)
	}
}

// TestKeyNeverInErrorsOrLogs runs every path that produces an error or a
// log line with a key configured and checks that the key never shows up.
func TestKeyNeverInErrorsOrLogs(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	st := openStore(t)
	cfg := legacyConfig(t)
	cfg.LLM.Providers["nokey"] = config.Provider{BaseURL: "https://api.example.com", APIKeyEnv: "LF_SETTINGS_UNSET_VAR"}
	s, err := Open(bg(), st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var errs []error
	collect := func(_ any, err error) { errs = append(errs, err) }
	collect(s.CreateProvider(bg(), ProviderInput{Name: sp("deepseek"), Type: sp("openai"), BaseURL: sp("https://a.b"), APIKey: sp(fakeKey)}))
	collect(s.CreateProvider(bg(), ProviderInput{Name: sp("x"), Type: sp("openai"), BaseURL: sp("bad"), APIKey: sp(fakeKey)}))
	collect(s.CreateProvider(bg(), ProviderInput{Name: sp("x"), Type: sp("bad"), BaseURL: sp("https://a.b"), APIKey: sp(fakeKey)}))
	collect(s.UpdateProvider(bg(), "deepseek", ProviderInput{RPM: ip(-1), APIKey: sp(fakeKey)}))
	collect(s.UpdateProvider(bg(), "deepseek", ProviderInput{Name: sp("ollama")}))
	collect(s.DeleteProvider(bg(), "deepseek"))
	errs = append(errs, s.SetRole(bg(), model.RoleSummary, "nope", fakeKey, model.RoleParams{}))
	_, reason := s.SnapshotUsable(model.StageSummary, `{"provider_id":"nokey","model":"m"}`)
	errs = append(errs, errors.New(reason))
	for i, err := range errs {
		if err == nil {
			t.Errorf("case %d: expected an error", i)
			continue
		}
		if strings.Contains(err.Error(), fakeKey) || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("case %d: key in error %q", i, err)
		}
	}
	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	if !strings.Contains(logs, "provider=nokey") {
		t.Errorf("expected a warning about the provider without key, logs:\n%s", logs)
	}
	if strings.Contains(logs, fakeKey) || strings.Contains(logs, "SECRET") {
		t.Errorf("key in logs:\n%s", logs)
	}
}

type lockedWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestIsLocalAndHelpers(t *testing.T) {
	for url, want := range map[string]bool{
		"http://localhost:11434/v1": true,
		"http://127.0.0.1:8080":     true,
		"http://127.0.0.2":          true,
		"http://[::1]:1234":         true,
		"http://ollama.localhost":   true,
		"https://api.deepseek.com":  false,
		"http://192.168.1.5:11434":  false,
		"not a url":                 false,
	} {
		if IsLocal(url) != want {
			t.Errorf("IsLocal(%q) != %v", url, want)
		}
	}
	if RoleLabel(model.RoleTranslate) != "翻译" || RoleLabel("x") != "x" {
		t.Error("RoleLabel")
	}
	if DefaultBaseURL(model.ProviderAnthropic) != "https://api.anthropic.com" || DefaultBaseURL(model.ProviderOpenAI) != "https://api.deepseek.com" {
		t.Error("DefaultBaseURL")
	}
	if s := New(openStore(t), nil); s.Config() == nil || s.Config().Listen == "" {
		t.Error("Config() must default")
	}
}
