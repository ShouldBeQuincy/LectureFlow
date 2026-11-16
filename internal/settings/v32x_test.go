// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"errors"
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

// v3.2.x: role params max_tokens (D1) and batch (P2 translate), and the
// snapshot of a manual retry (P1-b).

func TestSetRoleMaxTokensAndBatch(t *testing.T) {
	s, _ := newService(t)
	p := create(t, s, "P", model.ProviderOpenAI, "https://p.example.test/v1", fakeKey)

	for _, tc := range []struct {
		role   string
		in     model.RoleParams
		stored string
	}{
		{model.RoleSummary, model.RoleParams{MaxTokens: ip(32000)}, `{"max_tokens":32000}`},
		{model.RoleSummary, model.RoleParams{}, `{}`},                                              // leaving it out clears it
		{model.RoleSummary, model.RoleParams{MaxTokens: ip(1), Batch: ip(10)}, `{"max_tokens":1}`}, // batch: translate only
		{model.RoleSummary, model.RoleParams{MaxTokens: ip(MaxTokensLimit)}, `{"max_tokens":1000000}`},
		{model.RoleTranslate, model.RoleParams{Batch: ip(1)}, `{"batch":1}`},
		{model.RoleTranslate, model.RoleParams{Window: ip(3), MaxTokens: ip(8000), Batch: ip(MaxBatch)}, `{"window":3,"max_tokens":8000,"batch":20}`},
		{model.RoleTranslate, model.RoleParams{}, `{}`},
	} {
		if err := s.SetRole(bg(), tc.role, p.ID, "m", tc.in); err != nil {
			t.Fatalf("%s %+v: %v", tc.role, tc.in, err)
		}
		if got := s.Roles()[tc.role].Params; got != tc.stored {
			t.Errorf("%s: stored %s, want %s", tc.role, got, tc.stored)
		}
	}
	// what was stored before stays when a value is refused
	before := s.Roles()[model.RoleTranslate].Params
	for _, tc := range []struct {
		role string
		in   model.RoleParams
		code string
	}{
		{model.RoleSummary, model.RoleParams{MaxTokens: ip(0)}, CodeMaxTokensInvalid},
		{model.RoleSummary, model.RoleParams{MaxTokens: ip(-5)}, CodeMaxTokensInvalid},
		{model.RoleTranslate, model.RoleParams{MaxTokens: ip(MaxTokensLimit + 1)}, CodeMaxTokensInvalid},
		{model.RoleTranslate, model.RoleParams{Batch: ip(0)}, CodeBatchInvalid},
		{model.RoleTranslate, model.RoleParams{Batch: ip(MaxBatch + 1)}, CodeBatchInvalid},
	} {
		err := s.SetRole(bg(), tc.role, p.ID, "m", tc.in)
		if !errors.Is(err, ErrInvalid) || codeOf(err) != tc.code {
			t.Errorf("%s %+v: err = %v, want %s", tc.role, tc.in, err, tc.code)
		}
	}
	if got := s.Roles()[model.RoleTranslate].Params; got != before {
		t.Fatalf("a refused SetRole changed the row: %s", got)
	}
}

func TestResolveParamsMaxTokensAndBatch(t *testing.T) {
	// absent: exactly what the params resolved to before the options existed
	plain := model.RoleParams{Temperature: fp(model.DefaultTemperature(model.RoleTranslate)), Window: ip(model.DefaultTranslateWindow)}
	if got := ResolveParams(model.RoleTranslate, model.RoleParams{}); !reflect.DeepEqual(got, plain) {
		t.Errorf("translate params without the options: %+v", got)
	}
	for _, tc := range []struct {
		role      string
		in        model.RoleParams
		maxTokens *int
		batch     *int
	}{
		{model.RoleSummary, model.RoleParams{}, nil, nil},
		{model.RoleSummary, model.RoleParams{MaxTokens: ip(16000), Batch: ip(5)}, ip(16000), nil},
		{model.RoleTranslate, model.RoleParams{MaxTokens: ip(16000), Batch: ip(5)}, ip(16000), ip(5)},
		{model.RoleTranslate, model.RoleParams{Batch: ip(1)}, nil, ip(1)},
		// out of range (a hand-edited row): dropped, not passed on
		{model.RoleTranslate, model.RoleParams{MaxTokens: ip(0), Batch: ip(99)}, nil, nil},
		{model.RoleRepair, model.RoleParams{MaxTokens: ip(-1)}, nil, nil},
	} {
		got := ResolveParams(tc.role, tc.in)
		if !reflect.DeepEqual(got.MaxTokens, tc.maxTokens) || !reflect.DeepEqual(got.Batch, tc.batch) {
			t.Errorf("%s %+v: max_tokens %v batch %v", tc.role, tc.in, got.MaxTokens, got.Batch)
		}
	}
	// the resolved value is a copy
	in := model.RoleParams{MaxTokens: ip(5)}
	out := ResolveParams(model.RoleSummary, in)
	*out.MaxTokens = 6
	if *in.MaxTokens != 5 {
		t.Fatal("ResolveParams shares the pointer")
	}
}

// The current settings' snapshot carries max_tokens (compared by the rerun
// and retry decision); batch is carried and not compared.
func TestCurrentSnapshotsMaxTokensAndBatch(t *testing.T) {
	s, _ := newService(t)
	p := create(t, s, "P", model.ProviderOpenAI, "https://p.example.test/v1", fakeKey)
	for _, role := range []string{model.RoleSummary, model.RoleRepair, model.RoleTranslate} {
		if err := s.SetRole(bg(), role, p.ID, "m", model.RoleParams{}); err != nil {
			t.Fatal(err)
		}
	}
	base, err := s.CurrentSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(bg(), model.RoleTranslate, p.ID, "m", model.RoleParams{Batch: ip(10)}); err != nil {
		t.Fatal(err)
	}
	withBatch, _ := s.CurrentSnapshots()
	if !model.SameSnapshot(model.StageTranslate, base[model.StageTranslate], withBatch[model.StageTranslate]) {
		t.Error("batch alone makes the snapshots differ")
	}
	if snap, _ := model.ParseLLMSnapshot(withBatch[model.StageTranslate]); snap.Params.Batch == nil || *snap.Params.Batch != 10 {
		t.Errorf("snapshot without the batch: %s", withBatch[model.StageTranslate])
	}
	if err := s.SetRole(bg(), model.RoleSummary, p.ID, "m", model.RoleParams{MaxTokens: ip(9000)}); err != nil {
		t.Fatal(err)
	}
	withLimit, _ := s.CurrentSnapshots()
	if model.SameSnapshot(model.StageSummary, base[model.StageSummary], withLimit[model.StageSummary]) {
		t.Error("max_tokens does not make the snapshots differ")
	}
}

func TestManualSnapshot(t *testing.T) {
	s, _ := newService(t)
	claude := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	local := create(t, s, "Local", model.ProviderOpenAI, "http://127.0.0.1:11434/v1", "")
	if err := s.SetRole(bg(), model.RoleSummary, local.ID, "llama", model.RoleParams{}); err != nil {
		t.Fatal(err)
	}
	roles := s.Roles()

	got, err := s.ManualSnapshot(model.StageSummary, ManualTarget{ProviderID: claude.ID, Model: "claude-x", Thinking: sp("high"),
		MaxTokens: ip(64000), Temperature: fp(0.7)}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := model.LLMSnapshot{ProviderID: claude.ID, ProviderName: "Claude", Model: "claude-x",
		Params:        model.RoleParams{Temperature: fp(0.7), Thinking: sp("high"), MaxTokens: ip(64000)},
		PromptVersion: model.PromptVersionOf(model.StageSummary), Source: model.SnapshotSourceManual}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manual snapshot\n got %+v\nwant %+v", got, want)
	}
	// a local server needs no key; optional fields absent = off, automatic, the role's default
	got, err = s.ManualSnapshot(model.StageRepair, ManualTarget{ProviderID: local.ID, Model: "llama", Thinking: sp("off")}, "")
	if err != nil || got.Params.Thinking != nil || got.Params.MaxTokens != nil || got.Params.Window != nil ||
		got.Params.Temperature == nil || *got.Params.Temperature != model.DefaultTemperature(model.RoleRepair) {
		t.Fatalf("local manual snapshot: %+v err=%v", got, err)
	}
	// translate keeps the window of the snapshot it replaces, and its batch
	// on the same provider (review R24: on another provider the target's
	// default, DefaultBatch; batch_default_test.go has the other cases)
	old := model.MarshalSnapshot(model.LLMSnapshot{ProviderID: claude.ID, Model: "claude-y", Params: model.RoleParams{Temperature: fp(1), Window: ip(6), Batch: ip(8)}})
	got, err = s.ManualSnapshot(model.StageTranslate, ManualTarget{ProviderID: claude.ID, Model: "claude-x"}, old)
	if err != nil || got.Params.Window == nil || *got.Params.Window != 6 || got.Params.Batch == nil || *got.Params.Batch != 8 {
		t.Fatalf("translate manual snapshot: %+v err=%v", got, err)
	}
	old = model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "x", Model: "y", Params: model.RoleParams{Temperature: fp(1), Window: ip(6), Batch: ip(8)}})
	got, err = s.ManualSnapshot(model.StageTranslate, ManualTarget{ProviderID: claude.ID, Model: "claude-x"}, old)
	if err != nil || got.Params.Window == nil || *got.Params.Window != 6 || got.Params.Batch == nil || *got.Params.Batch != DefaultAnthropicBatch {
		t.Fatalf("translate manual snapshot, another provider: %+v err=%v", got, err)
	}
	// without an old snapshot: the default window and the target's default
	// batch (none for a local model, 10 for a Claude model on Anthropic)
	got, err = s.ManualSnapshot(model.StageTranslate, ManualTarget{ProviderID: local.ID, Model: "llama"}, model.EmptySnapshot)
	if err != nil || got.Params.Window == nil || *got.Params.Window != model.DefaultTranslateWindow || got.Params.Batch != nil {
		t.Fatalf("translate manual snapshot without an old one: %+v err=%v", got, err)
	}
	got, err = s.ManualSnapshot(model.StageTranslate, ManualTarget{ProviderID: claude.ID, Model: "claude-x"}, model.EmptySnapshot)
	if err != nil || got.Params.Window == nil || *got.Params.Window != model.DefaultTranslateWindow || got.Params.Batch == nil || *got.Params.Batch != DefaultAnthropicBatch {
		t.Fatalf("translate manual snapshot without an old one, claude: %+v err=%v", got, err)
	}
	if _, err := s.ManualSnapshot(model.StageASR, ManualTarget{ProviderID: claude.ID, Model: "m"}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("asr: %v", err)
	}
	if !reflect.DeepEqual(s.Roles(), roles) {
		t.Fatal("ManualSnapshot wrote the role settings")
	}
}
