// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"testing"

	"lectureflow/internal/model"
)

// TestProviderKeyStaysWithItsHost (sec2-app L2): moving base_url to another
// origin clears the key unless the same patch brings a new one; a change
// within the origin (path, case, default port) keeps it. Also with a data
// key (sec3 L3): the key is sealed bound to the origin, and every key the
// settings keep still opens (SameOrigin and the binding agree).
func TestProviderKeyStaysWithItsHost(t *testing.T) {
	t.Run("plain", testProviderKeyStaysWithItsHost)
	t.Run("sealed", func(t *testing.T) {
		useDataKey(t, 7)
		testProviderKeyStaysWithItsHost(t)
	})
}

func testProviderKeyStaysWithItsHost(t *testing.T) {
	s, st := newService(t)
	p := create(t, s, "DS", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	steps := []struct {
		base, key string
		wantKey   string
	}{
		{"https://api.deepseek.com/v1", "", fakeKey},     // same origin: kept
		{"https://API.deepseek.com:443/v1", "", fakeKey}, // default port, case
		{"https://evil.example/v1", "", ""},              // another host: cleared
		{"https://evil.example/v2", "sk-new-key-1234", "sk-new-key-1234"},
		{"http://evil.example/v2", "", ""}, // another scheme
		{"http://evil.example:8080", "sk-other-5678", "sk-other-5678"},
		{"http://evil.example:8081", "", ""}, // another port
	}
	store := st
	for i, st := range steps {
		in := ProviderInput{BaseURL: sp(st.base)}
		if st.key != "" {
			in.APIKey = sp(st.key)
		}
		u, err := s.UpdateProvider(bg(), p.ID, in)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if u.APIKey != st.wantKey {
			t.Fatalf("step %d (%s): key %q, want %q", i, st.base, u.APIKey, st.wantKey)
		}
		if got, _ := s.Provider(p.ID); got.APIKey != st.wantKey {
			t.Fatalf("step %d: stored key %q", i, got.APIKey)
		}
		if got, _ := store.GetProvider(bg(), p.ID); got.APIKey != st.wantKey {
			t.Fatalf("step %d: the database reads key %q", i, got.APIKey)
		}
	}
	// A patch without base_url never touches the key.
	if u, err := s.UpdateProvider(bg(), p.ID, ProviderInput{APIKey: sp("sk-again-0000")}); err != nil || u.APIKey != "sk-again-0000" {
		t.Fatalf("key only: %+v %v", u, err)
	}
	if u, err := s.UpdateProvider(bg(), p.ID, ProviderInput{MaxConcurrency: ip(16)}); err != nil || u.APIKey != "sk-again-0000" {
		t.Fatalf("limits only: %+v %v", u, err)
	}
}

// TestProviderLimitRanges (perf-llm): max_concurrency up to 512, rpm 0 (no
// per-minute limit) to 10000.
func TestProviderLimitRanges(t *testing.T) {
	s, _ := newService(t)
	p := create(t, s, "DS", model.ProviderOpenAI, "https://api.deepseek.com", fakeKey)
	u, err := s.UpdateProvider(bg(), p.ID, ProviderInput{MaxConcurrency: ip(MaxConcurrencyLimit), RPM: ip(0)})
	if err != nil || u.MaxConcurrency != 512 || u.RPM != 0 {
		t.Fatalf("512 / 0: %+v %v", u, err)
	}
	for _, in := range []ProviderInput{{MaxConcurrency: ip(513)}, {MaxConcurrency: ip(0)}, {RPM: ip(-1)}, {RPM: ip(10001)}} {
		if _, err := s.UpdateProvider(bg(), p.ID, in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{RPM: ip(-1)}); !hasCode(err, CodeRPMInvalid, map[string]any{"min": 0, "max": 10000}) {
		t.Errorf("rpm error: %v", err)
	}
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{MaxConcurrency: ip(513)}); !hasCode(err, CodeConcurrencyInvalid, map[string]any{"min": 1, "max": 512}) {
		t.Errorf("concurrency error: %v", err)
	}
	for v, want := range map[int]int{0: 0, -5: DefaultRPM, 300: 300, 20000: MaxRPM} {
		if got := legacyRPM(v); got != want {
			t.Errorf("legacyRPM(%d) = %d, want %d", v, got, want)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://api.deepseek.com", "https://api.deepseek.com/v1", true},
		{"https://api.deepseek.com", "HTTPS://API.DEEPSEEK.COM:443", true},
		{"http://localhost:11434", "http://localhost:11434/v1", true},
		{"http://localhost", "http://localhost:80/x", true},
		{"https://api.deepseek.com", "http://api.deepseek.com", false},
		{"https://api.deepseek.com", "https://api.deepseek.com.evil.example", false},
		{"http://localhost:11434", "http://localhost:11435", false},
		{"not a url", "not a url", true},
		{"not a url", "https://x.example", false},
	} {
		if got := SameOrigin(c.a, c.b); got != c.want {
			t.Errorf("SameOrigin(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
