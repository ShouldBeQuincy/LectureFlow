// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

// SEC8 low-1: a base_url whose host only looks like the old one (U+0130,
// which Unicode lower-cases to "i" but net/http dials as another, punycode
// domain) is another origin, so the key does not stay with the provider.
func TestSameOriginLookAlikeHost(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://api.chatbot-ai.example", "https://api.chatbot-a\u0130.example", false},
		{"https://api.chatbot-ai.example/v1", "https://api.chatbot-a%C4%B0.example/v1", false},
		{"https://api.chatbot-a\u0130.example", "https://api.chatbot-a%C4%B0.example:443/v1", true},
	} {
		if got := SameOrigin(c.a, c.b); got != c.want {
			t.Errorf("SameOrigin(%q, %q) = %v", c.a, c.b, got)
		}
	}
	for _, mode := range []string{"plain", "sealed"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "sealed" {
				useDataKey(t, 11)
			}
			for _, look := range []string{"https://api.chatbot-a\u0130.example/v1", "https://api.chatbot-a%C4%B0.example/v1"} {
				s, st := newService(t)
				p := create(t, s, "AI", model.ProviderOpenAI, "https://api.chatbot-ai.example/v1", fakeKey)
				u, err := s.UpdateProvider(bg(), p.ID, ProviderInput{BaseURL: sp(look)})
				if err != nil {
					t.Fatal(err)
				}
				if u.APIKey != "" {
					t.Fatalf("%s: the key stayed with the look-alike host", look)
				}
				if got, _ := st.GetProvider(bg(), p.ID); got.APIKey != "" {
					t.Fatalf("%s: the database still holds a key", look)
				}
			}
		})
	}
}

// SEC8 low-3: an API key that starts with "enc:" (a sealed value's prefix)
// is refused on create and on update, with its own code; the stored key
// stays. A key that only contains "enc:" further on is a key like any other.
func TestAPIKeyLooksSealedIsRefused(t *testing.T) {
	for _, p := range secrets.Prefixes() {
		if !strings.HasPrefix(p, sealedKeyPrefix) {
			t.Fatalf("the sealed prefix %q does not start with %q", p, sealedKeyPrefix)
		}
	}
	for _, mode := range []string{"plain", "sealed"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "sealed" {
				useDataKey(t, 12)
			}
			s, st := newService(t)
			p := create(t, s, "DS", model.ProviderOpenAI, "https://llm.example.org", fakeKey)
			for _, key := range []string{
				secrets.Prefix + "dGVzdC1ub3QtYS1yZWFsLWNpcGhlcnRleHQ",
				secrets.PrefixV2 + "dGVzdC1ub3QtYS1yZWFsLWNpcGhlcnRleHQ",
				"  enc:v3:test-value  ",
				"enc:",
			} {
				_, err := s.CreateProvider(bg(), ProviderInput{Name: sp("X"), Type: sp(model.ProviderOpenAI),
					BaseURL: sp("https://evil.example"), APIKey: sp(key)})
				if !hasCode(err, CodeAPIKeyLooksSealed, nil) {
					t.Errorf("create with %q: %v", key, err)
				}
				if k := strings.TrimSpace(key); err != nil && len(k) > len(sealedKeyPrefix) && strings.Contains(err.Error(), k) {
					t.Errorf("the error text holds the key: %v", err)
				}
				if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{APIKey: sp(key)}); !hasCode(err, CodeAPIKeyLooksSealed, nil) {
					t.Errorf("update with %q: %v", key, err)
				}
				if got, _ := st.GetProvider(bg(), p.ID); got.APIKey != fakeKey {
					t.Fatalf("after %q the stored key reads %q", key, got.APIKey)
				}
			}
			if ps, _ := st.ListProviders(bg()); len(ps) != 1 {
				t.Fatalf("a refused create stored a provider: %d providers", len(ps))
			}
			for _, key := range []string{"sk-test-enc:0001", "ENC:not-our-prefix-0002"} {
				if u, err := s.UpdateProvider(bg(), p.ID, ProviderInput{APIKey: sp(key)}); err != nil || u.APIKey != key {
					t.Errorf("key %q: %+v %v", key, u.APIKey, err)
				}
			}
		})
	}
}
