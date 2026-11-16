// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Keys already in the environment (.claude/v3-brief.md §7.3): onboarding
// lists, for the services of the manifest's presets, whether the variable
// the service's own tools use is set (masked only), and "使用环境变量中的
// key" creates or updates that provider on the server, so the key never
// passes through the browser. No response and no log line carries more of
// the key than model.KeyHint's last four characters.

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/onboarding/env-keys", s.getEnvKeys)
		h("POST /api/onboarding/use-env-key", s.useEnvKey)
	})
}

// providerEnvVars are the environment variables the preset services
// document for their API keys, by preset provider id.
var providerEnvVars = map[string]string{
	"deepseek": "DEEPSEEK_API_KEY",
	"gemini":   "GEMINI_API_KEY",
	"zhipu":    "ZHIPUAI_API_KEY",
}

// fallbackPresetProviders stand in for the presets without a component
// manager: DeepSeek only, with the limits of config.example.toml and of the
// shipped manifest's preset (64 concurrent requests, no per-minute limit;
// perf-llm, NOTES.md).
var fallbackPresetProviders = []components.PresetProvider{{
	ID: "deepseek", Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com",
	DefaultModel: "deepseek-flash", BalanceAPI: model.BalanceDeepSeek, MaxConcurrency: config.DeepSeekMaxConcurrency, RPM: 0,
}}

// EnvKeyView is one row of GET /api/onboarding/env-keys.
type EnvKeyView struct {
	ProviderType string `json:"provider_type"`
	Env          string `json:"env"`
	Present      bool   `json:"present"`
	Masked       string `json:"masked,omitempty"` // model.KeyHint ("sk-…abcd")
}

// UseEnvKeyBody is the body of POST /api/onboarding/use-env-key.
type UseEnvKeyBody struct {
	ProviderType string `json:"provider_type"`
}

// presetProviders returns the preset services that have a known variable.
func (s *Server) presetProviders() []components.PresetProvider {
	list := fallbackPresetProviders
	if s.comps != nil {
		list = s.comps.Presets().Providers
	}
	var out []components.PresetProvider
	for _, p := range list {
		if providerEnvVars[p.ID] != "" {
			out = append(out, p)
		}
	}
	return out
}

// envKey reads the key of a variable (surrounding spaces removed).
func (s *Server) envKey(name string) string {
	return strings.TrimSpace(s.compState().getenv(name))
}

// GET /api/onboarding/env-keys.
func (s *Server) getEnvKeys(w http.ResponseWriter, r *http.Request) {
	out := []EnvKeyView{}
	for _, p := range s.presetProviders() {
		env := providerEnvVars[p.ID]
		v := EnvKeyView{ProviderType: p.ID, Env: env}
		if k := s.envKey(env); k != "" {
			v.Present, v.Masked = true, model.KeyHint(k)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/onboarding/use-env-key {"provider_type"}: updates the key of
// the provider with the preset's base_url, or creates the provider from the
// preset (201) with the preset's max_concurrency (the settings default 4
// where the preset gives none) and rpm (0 = no per-minute limit, perf-llm),
// through the settings service like the v1.1 provider endpoints. An
// existing provider keeps its limits: they may be the user's own. A
// provider is matched by its base_url only, never by its name: the key goes
// to the host it was issued for (sec2-app L2); a provider that merely has
// the preset's name leaves the new one a free name ("DeepSeek (deepseek)").
// 404 onboarding.env_key_missing when the variable is not set.
func (s *Server) useEnvKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	var in UseEnvKeyBody
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	var pp *components.PresetProvider
	for _, p := range s.presetProviders() {
		if p.ID == in.ProviderType {
			p := p
			pp = &p
			break
		}
	}
	if pp == nil {
		s.fail(w, r, apiErr(http.StatusBadRequest, "request.bad_param", map[string]any{"name": "provider_type"}))
		return
	}
	env := providerEnvVars[pp.ID]
	key := s.envKey(env)
	if key == "" {
		s.fail(w, r, apiErr(http.StatusNotFound, "onboarding.env_key_missing", map[string]any{"provider_type": pp.ID, "env": env}))
		return
	}
	var match *model.Provider
	ps := s.set.Providers()
	for i := range ps {
		if samePresetBaseURL(ps[i].BaseURL, pp.BaseURL) {
			match = &ps[i]
			break
		}
	}
	if match != nil {
		p, err := s.set.UpdateProvider(r.Context(), match.ID, settings.ProviderInput{APIKey: &key})
		if err != nil {
			s.fail(w, r, notFoundAs(err, "provider.not_found"))
			return
		}
		s.respondProvider(w, r, p, http.StatusOK)
		return
	}
	name, typ, base := freeProviderName(ps, pp), pp.Type, pp.BaseURL
	pin := settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key}
	if pp.BalanceAPI != "" {
		b := pp.BalanceAPI
		pin.BalanceAPI = &b
	}
	if pp.MaxConcurrency > 0 {
		c := pp.MaxConcurrency
		pin.MaxConcurrency = &c
	}
	rpm := max(pp.RPM, 0) // 0 = no per-minute limit
	pin.RPM = &rpm
	p, err := s.set.CreateProvider(r.Context(), pin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondProvider(w, r, p, http.StatusCreated)
}

// samePresetBaseURL reports whether a provider's base_url is the preset's:
// the same origin as the backend compares them (settings.SameOrigin, the
// origin a sealed key is bound to: ASCII case and default ports only, a
// host with a byte outside ASCII equals only itself) and the same path and
// query (ASCII case, trailing slashes ignored). Unicode lower-casing would
// let a look-alike host ("apİ…", U+0130) match the preset's while net/http
// dials another (punycode) domain with the key (SEC9 new-low-2).
func samePresetBaseURL(a, b string) bool {
	if !settings.SameOrigin(a, b) {
		return false
	}
	return baseURLRest(a) == baseURLRest(b)
}

// baseURLRest is the path and query of a base_url, trailing slashes
// removed and ASCII letters lower-cased ("" when it does not parse: then
// SameOrigin has compared the whole text already).
func baseURLRest(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return ""
	}
	rest := strings.TrimRight(u.EscapedPath(), "/")
	if u.RawQuery != "" || u.ForceQuery {
		rest += "?" + u.RawQuery
	}
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, rest)
}

// freeProviderName is the name of a provider created from preset pp: the
// preset's name, else "{name} ({id})", else that with a number, whichever
// no provider of ps has (names are unique, case-insensitive).
func freeProviderName(ps []model.Provider, pp *components.PresetProvider) string {
	taken := func(n string) bool {
		for _, p := range ps {
			if strings.EqualFold(strings.TrimSpace(p.Name), n) {
				return true
			}
		}
		return false
	}
	name := pp.Name
	if !taken(name) {
		return name
	}
	base := fmt.Sprintf("%s (%s)", pp.Name, pp.ID)
	name = base
	for i := 2; taken(name); i++ {
		name = fmt.Sprintf("%s %d", base, i)
	}
	return name
}
