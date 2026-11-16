// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// TestUseEnvKeyMatchesByBaseURLOnly (sec2-app L2): a provider that only has
// the preset's name (here the fixture's "deepseek", pointing at the fake
// LLM server) does not receive the environment's key; a new provider with
// the preset's base_url and a free name is created instead.
func TestUseEnvKeyMatchesByBaseURLOnly(t *testing.T) {
	const secret = "sk-envkey-l2-test-0042"
	e := newEnv(t, func(s *Server) {
		s.compState().getenv = func(k string) string {
			if k == "DEEPSEEK_API_KEY" {
				return secret
			}
			return ""
		}
	})
	var fixture, fixtureKey string
	for _, p := range e.set.Providers() {
		if strings.EqualFold(p.Name, "DeepSeek") && !strings.Contains(strings.ToLower(p.BaseURL), "api.deepseek.com") {
			fixture, fixtureKey = p.ID, p.APIKey
		}
	}
	if fixture == "" {
		t.Fatalf("no fixture provider named deepseek on another host: %+v", e.set.Providers())
	}
	var pv ProviderView
	e.call("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusCreated, &pv)
	if pv.ID == fixture || pv.Name != "DeepSeek (deepseek)" || pv.BaseURL != "https://api.deepseek.com" || pv.KeyHint != "sk-…0042" {
		t.Fatalf("created %+v", pv)
	}
	if p, _ := e.set.Provider(fixture); p.APIKey != fixtureKey {
		t.Fatal("the provider with the preset's name got the environment's key")
	}
	// Once more: the provider with the base_url is found (200), no third one.
	n := len(e.set.Providers())
	var pv2 ProviderView
	e.call("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusOK, &pv2)
	if pv2.ID != pv.ID || len(e.set.Providers()) != n {
		t.Fatalf("second call: %+v (%d providers, was %d)", pv2, len(e.set.Providers()), n)
	}
}

// TestSamePresetBaseURL (SEC9 new-low-2): a provider matches the preset by
// the origin the backend binds keys to (settings.SameOrigin) and the path:
// ASCII case, a default port and trailing slashes still match, a look-alike
// host (U+0130, written as is or percent-encoded, which Unicode lower-casing
// turned into an ASCII "i") does not.
func TestSamePresetBaseURL(t *testing.T) {
	const preset = "https://api.example.com/v1beta/openai"
	for _, c := range []struct {
		base string
		want bool
	}{
		{"https://api.example.com/v1beta/openai", true},
		{"HTTPS://API.Example.COM/v1beta/openai", true},
		{"https://api.example.com:443/v1beta/openai", true},
		{"https://api.example.com/v1beta/openai/", true},
		{"  https://api.example.com/V1BETA/OpenAI//  ", true},
		{"https://apİ.example.com/v1beta/openai", false},
		{"https://ap%C4%B0.example.com/v1beta/openai", false},
		{"https://xn--ap-x9a.example.com/v1beta/openai", false},
		{"https://api.examplİ.com/v1beta/openai", false},
		{"https://ａpi.example.com/v1beta/openai", false},
		{"http://api.example.com/v1beta/openai", false},
		{"https://api.example.com:8443/v1beta/openai", false},
		{"https://api.example.com/v1/openai", false},
		{"https://api.example.com", false},
		{"https://api.example.com.evil.example/v1beta/openai", false},
	} {
		if got := samePresetBaseURL(c.base, preset); got != c.want {
			t.Errorf("samePresetBaseURL(%q) = %v, want %v", c.base, got, c.want)
		}
	}
	// a non-ASCII host still equals exactly itself
	if idn := "https://bücher.example/v1"; !samePresetBaseURL(idn, idn+"/") || samePresetBaseURL(idn, "https://Bücher.example/v1") {
		t.Error("a non-ASCII host must equal only its own bytes")
	}
}

// TestUseEnvKeySkipsLookAlikeProvider (SEC9 new-low-2): an existing provider
// whose base_url only looks like the preset's (U+0130 in the host) does not
// receive the environment's key; one that differs from the preset only in
// ASCII case, the default port or a trailing slash does.
func TestUseEnvKeySkipsLookAlikeProvider(t *testing.T) {
	const secret = "sk-test-envkey-lookalike-0077"
	const other = "sk-test-placeholder-not-env-1111"
	e := newEnv(t, func(s *Server) {
		s.compState().getenv = func(k string) string {
			if k == "DEEPSEEK_API_KEY" {
				return secret
			}
			return ""
		}
	})
	renameDeepSeekFixture(t, e.set)
	create := func(set *settings.Service, name, base string) string {
		t.Helper()
		typ, key := model.ProviderOpenAI, other
		p, err := set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key})
		if err != nil {
			t.Fatalf("create %q: %v", base, err)
		}
		return p.ID
	}
	look := create(e.set, "look-alike", "https://apİ.deepseek.com")
	var pv ProviderView
	e.call("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusCreated, &pv)
	if pv.ID == look || pv.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("created %+v", pv)
	}
	if p, _ := e.set.Provider(look); p.APIKey != other {
		t.Fatal("the look-alike provider got the environment's key")
	}
	if p, _ := e.set.Provider(pv.ID); p.APIKey != secret {
		t.Fatal("the new provider does not hold the environment's key")
	}

	// the same origin written differently is still the preset's provider
	e2 := newEnv(t, func(s *Server) {
		s.compState().getenv = func(k string) string {
			if k == "DEEPSEEK_API_KEY" {
				return secret
			}
			return ""
		}
	})
	renameDeepSeekFixture(t, e2.set)
	e = e2
	same := create(e.set, "upper-case", "HTTPS://API.DeepSeek.COM:443/")
	n := len(e.set.Providers())
	var pv2 ProviderView
	e.call("POST", "/api/onboarding/use-env-key", map[string]string{"provider_type": "deepseek"}, http.StatusOK, &pv2)
	if pv2.ID != same || len(e.set.Providers()) != n {
		t.Fatalf("matched %+v (%d providers, was %d)", pv2, len(e.set.Providers()), n)
	}
	if p, _ := e.set.Provider(same); p.APIKey != secret {
		t.Fatal("the provider with the preset's origin did not get the key")
	}
}

func TestFreeProviderName(t *testing.T) {
	pp := &components.PresetProvider{ID: "deepseek", Name: "DeepSeek"}
	ps := providersNamed("deepseek", "DeepSeek (deepseek)", "DeepSeek (DEEPSEEK) 2")
	if got := freeProviderName(ps, pp); got != "DeepSeek (deepseek) 3" {
		t.Errorf("name %q", got)
	}
	if got := freeProviderName(providersNamed("Other"), pp); got != "DeepSeek" {
		t.Errorf("name %q", got)
	}
}

func providersNamed(names ...string) []model.Provider {
	out := make([]model.Provider, len(names))
	for i, n := range names {
		out[i] = model.Provider{ID: n, Name: n}
	}
	return out
}
