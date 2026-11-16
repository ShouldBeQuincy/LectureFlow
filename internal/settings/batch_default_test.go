// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// User 2026-11-11 (narrowed to Claude models after review R24): a translate
// role running a Claude model on an Anthropic provider without a batch size
// runs 10 sentences per request; every other provider and model stays at 1.
// The default is written into the snapshot of the current settings, so an
// older snapshot (no batch) keeps running one sentence per request.

const sonnet = "claude-sonnet-5-5"

func TestDefaultAnthropicBatchInRange(t *testing.T) {
	if DefaultAnthropicBatch != 10 || DefaultAnthropicBatch < 1 || DefaultAnthropicBatch > MaxBatch {
		t.Fatalf("DefaultAnthropicBatch %d (MaxBatch %d)", DefaultAnthropicBatch, MaxBatch)
	}
}

func TestDefaultBatchByProviderAndModel(t *testing.T) {
	anthropic := model.Provider{Type: model.ProviderAnthropic, BaseURL: "https://api.anthropic.com"}
	relay := model.Provider{Type: model.ProviderAnthropic, BaseURL: "https://relay.example.test"}
	kimi := model.Provider{Type: model.ProviderAnthropic, BaseURL: "https://api.moonshot.example.test/anthropic"}
	for _, c := range []struct {
		name  string
		p     model.Provider
		model string
		want  int
	}{
		{"anthropic, claude", anthropic, sonnet, DefaultAnthropicBatch},
		{"anthropic, claude upper case", anthropic, " Claude-Opus-5-5 ", DefaultAnthropicBatch},
		{"anthropic type on a relay, gateway name", relay, "anthropic/claude-sonnet-5-5", DefaultAnthropicBatch},
		{"anthropic type on a relay, vertex-style name", relay, "claude-sonnet-5-5@20261101", DefaultAnthropicBatch},
		{"anthropic-compatible endpoint, other vendor's model", kimi, "kimi-k2", 1},
		{"anthropic type, GLM", relay, "glm-4.6", 1},
		{"anthropic type, empty model", anthropic, "", 1},
		{"openai named like claude", model.Provider{Type: model.ProviderOpenAI, Name: "Claude", BaseURL: "https://api.anthropic.com/v1"}, sonnet, 1},
		{"deepseek", model.Provider{Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com", BalanceAPI: model.BalanceDeepSeek}, "deepseek-flash", 1},
		{"deepseek anthropic-compatible, claude name", model.Provider{Type: model.ProviderAnthropic, BaseURL: "https://api.deepseek.com/anthropic"}, "claude-opus-5-5", 1},
		{"other openai-compatible, claude name", model.Provider{Type: model.ProviderOpenAI, BaseURL: "https://openrouter.ai/api/v1"}, "anthropic/claude-sonnet-5-5", 1},
	} {
		if got := DefaultBatch(c.p, c.model); got != c.want {
			t.Errorf("%s: DefaultBatch %d, want %d", c.name, got, c.want)
		}
	}
	if ClaudeBatch(anthropic) != DefaultAnthropicBatch || ClaudeBatch(kimi) != DefaultAnthropicBatch ||
		ClaudeBatch(model.Provider{Type: model.ProviderOpenAI}) != 1 {
		t.Error("ClaudeBatch")
	}
}

// translateSnap is the translate snapshot of the current settings.
func translateSnap(t *testing.T, s *Service) (string, model.LLMSnapshot) {
	t.Helper()
	cur, err := s.CurrentSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	raw := cur[model.StageTranslate]
	snap, ok := model.ParseLLMSnapshot(raw)
	if !ok {
		t.Fatalf("translate snapshot %q", raw)
	}
	return raw, snap
}

func setLectureRoles(t *testing.T, s *Service, providerID, modelName string, translate model.RoleParams) {
	t.Helper()
	for _, role := range []string{model.RoleSummary, model.RoleRepair} {
		if err := s.SetRole(bg(), role, providerID, modelName, model.RoleParams{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetRole(bg(), model.RoleTranslate, providerID, modelName, translate); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentSnapshotAnthropicDefaultBatch(t *testing.T) {
	s, _ := newService(t)
	claude := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)

	// empty: the snapshot names 10; the stored role setting stays empty
	setLectureRoles(t, s, claude.ID, sonnet, model.RoleParams{})
	raw, snap := translateSnap(t, s)
	if snap.Params.Batch == nil || *snap.Params.Batch != DefaultAnthropicBatch || !strings.Contains(raw, `"batch":10`) {
		t.Fatalf("anthropic, batch empty: %s", raw)
	}
	if r := s.Roles()[model.RoleTranslate]; strings.Contains(r.Params, "batch") {
		t.Errorf("the default was stored in the role setting: %s", r.Params)
	}
	// SnapshotTarget is what the snapshot records; RoleTarget (requests made
	// without a job snapshot) keeps only what the role sets, so those
	// requests are sized as before
	if st, err := s.SnapshotTarget(model.RoleTranslate); err != nil || st.Params.Batch == nil || *st.Params.Batch != DefaultAnthropicBatch {
		t.Errorf("SnapshotTarget %+v %v", st.Params, err)
	}
	if rt, err := s.RoleTarget(model.RoleTranslate); err != nil || rt.Params.Batch != nil {
		t.Errorf("RoleTarget carries a default batch: %+v %v", rt.Params, err)
	}
	// only translate has a batch
	cur, _ := s.CurrentSnapshots()
	for _, st := range []string{model.StageSummary, model.StageRepair} {
		if strings.Contains(cur[st], "batch") {
			t.Errorf("%s snapshot has a batch: %s", st, cur[st])
		}
	}

	// explicitly 1: one sentence per request, written as 1
	setLectureRoles(t, s, claude.ID, sonnet, model.RoleParams{Batch: ip(1)})
	if raw, snap := translateSnap(t, s); snap.Params.Batch == nil || *snap.Params.Batch != 1 {
		t.Fatalf("anthropic, batch 1: %s", raw)
	}
	// another value is kept as set
	setLectureRoles(t, s, claude.ID, sonnet, model.RoleParams{Batch: ip(4)})
	if raw, snap := translateSnap(t, s); snap.Params.Batch == nil || *snap.Params.Batch != 4 {
		t.Fatalf("anthropic, batch 4: %s", raw)
	}
}

// Other providers, and models other than Claude on an Anthropic-type
// provider: the snapshot is byte for byte what it was before the default
// (no batch at all when the role leaves it empty).
func TestCurrentSnapshotOtherProvidersNoDefaultBatch(t *testing.T) {
	for _, c := range []struct{ name, typ, url, model string }{
		{"openai", model.ProviderOpenAI, "https://p.example.test/v1", "m"},
		{"openai, claude name", model.ProviderOpenAI, "https://openrouter.ai/api/v1", "anthropic/claude-sonnet-5-5"},
		{"deepseek", model.ProviderOpenAI, "https://api.deepseek.com", "deepseek-flash"},
		{"deepseek anthropic-compatible", model.ProviderAnthropic, "https://api.deepseek.com/anthropic", "claude-opus-5-5"},
		{"anthropic-compatible, other vendor's model", model.ProviderAnthropic, "https://api.moonshot.example.test/anthropic", "kimi-k2"},
	} {
		s, _ := newService(t)
		p := create(t, s, "P", c.typ, c.url, fakeKey)
		setLectureRoles(t, s, p.ID, c.model, model.RoleParams{})
		raw, snap := translateSnap(t, s)
		want := model.MarshalSnapshot(model.LLMSnapshot{ProviderID: p.ID, ProviderName: "P", Model: c.model,
			Params:        ResolveParams(model.RoleTranslate, model.RoleParams{}),
			PromptVersion: model.PromptVersionOf(model.StageTranslate)})
		if snap.Params.Batch != nil || raw != want {
			t.Errorf("%s: snapshot changed\n got %s\nwant %s", c.name, raw, want)
		}
	}
}

// A snapshot written before the default (an Anthropic translate job without
// a batch) equals the current setting that now resolves to 10: the rerun
// and retranslate decisions (model.SameSnapshot, state.SnapshotDiffs) do not
// report a change, and the old snapshot, kept as it is, still means 1.
func TestOldAnthropicSnapshotSameAsCurrent(t *testing.T) {
	s, _ := newService(t)
	claude := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	setLectureRoles(t, s, claude.ID, sonnet, model.RoleParams{})
	cur, snap := translateSnap(t, s)
	old := snap
	old.Params.Batch = nil
	oldRaw := model.MarshalSnapshot(old)
	if strings.Contains(oldRaw, "batch") || !strings.Contains(cur, `"batch":10`) {
		t.Fatalf("old %s current %s", oldRaw, cur)
	}
	if !model.SameSnapshot(model.StageTranslate, oldRaw, cur) || !model.SameSnapshot(model.StageTranslate, cur, oldRaw) {
		t.Error("an old snapshot without a batch differs from the current setting")
	}
	// the user setting 1 explicitly is not a change either (batch is never compared)
	setLectureRoles(t, s, claude.ID, sonnet, model.RoleParams{Batch: ip(1)})
	one, _ := translateSnap(t, s)
	if !model.SameSnapshot(model.StageTranslate, oldRaw, one) || !model.SameSnapshot(model.StageTranslate, cur, one) {
		t.Error("batch 1 differs from the default")
	}
	// a real change still shows
	if err := s.SetRole(bg(), model.RoleTranslate, claude.ID, "claude-opus-5-5", model.RoleParams{}); err != nil {
		t.Fatal(err)
	}
	changed, _ := translateSnap(t, s)
	if model.SameSnapshot(model.StageTranslate, oldRaw, changed) {
		t.Error("a model change is not reported")
	}
}

// A manual retry (ManualSnapshot) takes the batch with the provider: on the
// same provider the old snapshot's is kept (none stays none); on another
// provider the target's default replaces it (lead after review R24).
func TestManualSnapshotBatchFollowsProvider(t *testing.T) {
	s, _ := newService(t)
	claude := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	claude2 := create(t, s, "Claude relay", model.ProviderAnthropic, "https://relay.example.test", fakeKey)
	ds := create(t, s, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	snapOf := func(p model.Provider, mdl string, batch *int) string {
		return model.MarshalSnapshot(model.LLMSnapshot{ProviderID: p.ID, ProviderName: p.Name, Model: mdl,
			Params: model.RoleParams{Temperature: fp(0.3), Window: ip(3), Batch: batch}, PromptVersion: 2})
	}
	batchOf := func(t *testing.T, p model.Provider, mdl, old string) *int {
		t.Helper()
		got, err := s.ManualSnapshot(model.StageTranslate, ManualTarget{ProviderID: p.ID, Model: mdl}, old)
		if err != nil {
			t.Fatal(err)
		}
		if got.Params.Window == nil || *got.Params.Window != 3 {
			t.Errorf("window %v", got.Params.Window)
		}
		return got.Params.Batch
	}
	eq := func(b *int, want int) bool { return b != nil && *b == want }

	// Claude's default 10 -> DeepSeek: one sentence per request (no batch)
	if b := batchOf(t, ds, "deepseek-flash", snapOf(claude, sonnet, ip(10))); b != nil {
		t.Errorf("claude(10) -> deepseek: batch %d, want none", *b)
	}
	// Claude's 10 -> a non-Claude model on another Anthropic-type provider: none
	if b := batchOf(t, claude2, "kimi-k2", snapOf(claude, sonnet, ip(10))); b != nil {
		t.Errorf("claude(10) -> kimi: batch %d, want none", *b)
	}
	// DeepSeek (no batch) -> Claude on Anthropic: the target's default, 10
	if b := batchOf(t, claude, sonnet, snapOf(ds, "deepseek-flash", nil)); !eq(b, DefaultAnthropicBatch) {
		t.Errorf("deepseek(none) -> claude: batch %v, want %d", b, DefaultAnthropicBatch)
	}
	// Claude (4) -> Claude on another Anthropic provider: the target's default
	if b := batchOf(t, claude2, "claude-opus-5-5", snapOf(claude, sonnet, ip(4))); !eq(b, DefaultAnthropicBatch) {
		t.Errorf("claude(4) -> other anthropic provider: batch %v, want %d", b, DefaultAnthropicBatch)
	}
	// the same provider, another model: the old value is kept, also none
	if b := batchOf(t, claude, "claude-opus-5-5", snapOf(claude, sonnet, ip(4))); !eq(b, 4) {
		t.Errorf("same provider, batch 4: %v", b)
	}
	if b := batchOf(t, claude, "claude-opus-5-5", snapOf(claude, sonnet, nil)); b != nil {
		t.Errorf("same provider, old snapshot without batch: %d, want none", *b)
	}
	if b := batchOf(t, ds, "deepseek-pro", snapOf(ds, "deepseek-flash", ip(10))); !eq(b, 10) {
		t.Errorf("same provider (deepseek), batch 10 set by the user: %v", b)
	}

	// the same Anthropic gateway, the other kind of model (review R24
	// recheck): the target's default, not the old value
	if b := batchOf(t, claude2, "kimi-k2", snapOf(claude2, sonnet, ip(10))); b != nil {
		t.Errorf("same gateway, claude(10) -> non-claude: batch %d, want none", *b)
	}
	if b := batchOf(t, claude2, sonnet, snapOf(claude2, "kimi-k2", nil)); !eq(b, DefaultAnthropicBatch) {
		t.Errorf("same gateway, non-claude(none) -> claude: batch %v, want %d", b, DefaultAnthropicBatch)
	}
	// the same gateway, Claude to Claude: the old value
	if b := batchOf(t, claude2, "anthropic/claude-opus-5-5", snapOf(claude2, sonnet, ip(4))); !eq(b, 4) {
		t.Errorf("same gateway, claude(4) -> claude: batch %v, want 4", b)
	}
	// an old snapshot without a model name is not a valid LLM snapshot
	// (model.ParseLLMSnapshot): the target's default, as without one
	if _, ok := model.ParseLLMSnapshot(snapOf(claude2, "", ip(3))); ok {
		t.Fatal("a snapshot without a model parses")
	}
	if b := batchOf(t, claude2, "kimi-k2", snapOf(claude2, "", ip(3))); b != nil {
		t.Errorf("no old model name -> non-claude: batch %d, want none", *b)
	}
	if b := batchOf(t, claude2, sonnet, snapOf(claude2, "", ip(3))); !eq(b, DefaultAnthropicBatch) {
		t.Errorf("no old model name -> claude: batch %v, want %d", b, DefaultAnthropicBatch)
	}
}
