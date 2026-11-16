// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"testing"

	"lectureflow/internal/model"
)

// v3.2.x P2 (Q13a): a new Anthropic-type provider has no per-minute limit
// and 4 concurrent requests; an explicit value wins; other types and
// providers that exist already keep theirs.
func TestAnthropicProviderRateDefaults(t *testing.T) {
	s, st := newService(t)

	claude := create(t, s, "Claude", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	if claude.RPM != 0 || claude.MaxConcurrency != 4 {
		t.Fatalf("new anthropic provider: rpm %d, max_concurrency %d", claude.RPM, claude.MaxConcurrency)
	}
	if row, err := st.GetProvider(bg(), claude.ID); err != nil || row.RPM != 0 || row.MaxConcurrency != 4 {
		t.Fatalf("stored row: %+v err=%v", row, err)
	}
	other := create(t, s, "Other", model.ProviderOpenAI, "https://other.example.test/v1", fakeKey)
	if other.RPM != DefaultRPM || other.MaxConcurrency != DefaultMaxConcurrency {
		t.Fatalf("new openai provider: rpm %d, max_concurrency %d", other.RPM, other.MaxConcurrency)
	}
	// the type is trimmed like everywhere else
	spaced, err := s.CreateProvider(bg(), ProviderInput{Name: sp("Spaced"), Type: sp(" anthropic "), BaseURL: sp("https://api.anthropic.com")})
	if err != nil || spaced.RPM != 0 {
		t.Fatalf("type with spaces: %+v err=%v", spaced, err)
	}
	// explicit values win
	explicit, err := s.CreateProvider(bg(), ProviderInput{Name: sp("Explicit"), Type: sp(model.ProviderAnthropic),
		BaseURL: sp("https://api.anthropic.com"), APIKey: sp(fakeKey), RPM: ip(50), MaxConcurrency: ip(2)})
	if err != nil || explicit.RPM != 50 || explicit.MaxConcurrency != 2 {
		t.Fatalf("explicit limits: %+v err=%v", explicit, err)
	}

	// An existing provider is never touched: not by a patch of another
	// field, not by a change of its type, not by a reload.
	old := create(t, s, "Old", model.ProviderAnthropic, "https://api.anthropic.com", fakeKey)
	if _, err := s.UpdateProvider(bg(), old.ID, ProviderInput{RPM: ip(60)}); err != nil { // as created before v3.2.x
		t.Fatal(err)
	}
	if got, err := s.UpdateProvider(bg(), old.ID, ProviderInput{Name: sp("Old renamed")}); err != nil || got.RPM != 60 {
		t.Fatalf("patch of the name: %+v err=%v", got, err)
	}
	if got, err := s.UpdateProvider(bg(), other.ID, ProviderInput{Type: sp(model.ProviderAnthropic)}); err != nil || got.RPM != DefaultRPM {
		t.Fatalf("patch of the type: %+v err=%v", got, err)
	}
	if err := s.Load(bg()); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Provider(old.ID); p.RPM != 60 {
		t.Fatalf("after a reload: rpm %d", p.RPM)
	}
}
