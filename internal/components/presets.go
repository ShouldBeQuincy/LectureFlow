// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"
)

// Presets are the cost tiers of onboarding step 4 (SPEC-v3 V4.2): they come
// from the manifest so they can change with a newer manifest. Texts are
// i18n keys; the onboarding UI (step 6) turns a tier into provider rows and
// role settings.
type Presets struct {
	// Default is the tier preselected by onboarding ("standard").
	Default string       `json:"default"`
	Tiers   []PresetTier `json:"tiers"`
	// Providers are the services the tiers refer to.
	Providers []PresetProvider `json:"providers"`
	// Free picks the provider that "$free" stands for in Roles.
	Free FreeChoice `json:"free"`
}

// FreeProvider is the role provider that stands for the free channel chosen
// by Presets.Free.
const FreeProvider = "$free"

// PresetTier is one cost tier.
type PresetTier struct {
	ID      string `json:"id"` // standard | saver | free | transcribe_only
	NameKey string `json:"name_key"`
	DescKey string `json:"desc_key"`
	CostKey string `json:"cost_key"`
	// NightProcessing turns on the slow overnight processing (zero-cost tier).
	NightProcessing bool `json:"night_processing"`
	// TranscribeOnly: no provider at all; transcription, playback and subtitles only.
	TranscribeOnly bool `json:"transcribe_only"`
	// Roles maps every LLM role (model.Roles) to a provider id or "$free";
	// Model "" = the provider's DefaultModel.
	Roles map[string]PresetRole `json:"roles,omitempty"`
}

// PresetRole is the provider and model of one role.
type PresetRole struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

// PresetProvider is a provider the tiers can configure.
type PresetProvider struct {
	ID           string `json:"id"`
	Name         string `json:"name"` // brand name, not translated
	Type         string `json:"type"` // "openai" (OpenAI-compatible) | "anthropic"
	BaseURL      string `json:"base_url"`
	DefaultModel string `json:"default_model"`
	// Free: the provider is used through its free quota.
	Free bool `json:"free"`
	// TermsNotice: show "some free services' terms allow the provider to use
	// what you submit to improve its models" next to this provider (V4.2).
	TermsNotice bool   `json:"terms_notice"`
	TermsURL    string `json:"terms_url,omitempty"`
	// Regions where onboarding suggests the provider ("cn" = mainland
	// China); empty = everywhere.
	Regions    []string `json:"regions,omitempty"`
	SignupURL  string   `json:"signup_url"`
	KeysURL    string   `json:"keys_url"`
	PricingURL string   `json:"pricing_url,omitempty"`
	// StepsKeys are the i18n keys of the numbered steps "where to sign up,
	// where to create the key, roughly what it costs".
	StepsKeys []string `json:"steps_keys"`
	CostKey   string   `json:"cost_key"`
	// Rate limits of the provider created from the preset: MaxConcurrency 0
	// = the application's default; RPM 0 (or absent) = no per-minute limit
	// (perf-llm: DeepSeek limits concurrency per account, not requests per
	// minute).
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	RPM            int `json:"rpm,omitempty"`
	// BalanceAPI as in the provider settings ("deepseek" | "").
	BalanceAPI string `json:"balance_api,omitempty"`
	// Refs are the pages the free quota, the API compatibility and the
	// terms were checked against (NOTES.md, v3-components).
	Refs []string `json:"refs,omitempty"`
}

// FreeChoice selects the free provider by region.
type FreeChoice struct {
	Default  string            `json:"default"`
	ByRegion map[string]string `json:"by_region,omitempty"`
}

func (p Presets) validate() []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf("presets: "+f, a...)) }
	provs := map[string]PresetProvider{}
	for _, pr := range p.Providers {
		if pr.ID == "" || provs[pr.ID].ID != "" {
			add("provider id %q missing or duplicate", pr.ID)
		}
		provs[pr.ID] = pr
		if pr.Type != "openai" && pr.Type != "anthropic" {
			add("provider %s: type %q", pr.ID, pr.Type)
		}
		if !isHTTPURL(pr.BaseURL) || pr.DefaultModel == "" || pr.SignupURL == "" || pr.KeysURL == "" ||
			len(pr.StepsKeys) == 0 || pr.CostKey == "" || pr.Name == "" {
			add("provider %s: base_url, default_model, name, signup_url, keys_url, steps_keys and cost_key are required", pr.ID)
		}
		if pr.Free && pr.TermsURL == "" {
			add("provider %s: a free provider needs terms_url", pr.ID)
		}
		// AUD5 info-1: these links become the href of the onboarding page's
		// external links; only https addresses (the optional ones may be
		// empty)
		for _, l := range []struct {
			field, url string
			optional   bool
		}{{"signup_url", pr.SignupURL, false}, {"keys_url", pr.KeysURL, false},
			{"pricing_url", pr.PricingURL, true}, {"terms_url", pr.TermsURL, true}} {
			if !(l.optional && l.url == "") && !isHTTPSLink(l.url) {
				add("provider %s: %s %q is not an https:// address", pr.ID, l.field, l.url)
			}
		}
		for _, r := range pr.Refs {
			if !isHTTPSLink(r) {
				add("provider %s: refs entry %q is not an https:// address", pr.ID, r)
			}
		}
	}
	free := func(id string) bool { pr, ok := provs[id]; return ok && pr.Free }
	if p.Free.Default == "" || !free(p.Free.Default) {
		add("free.default %q is not a free provider", p.Free.Default)
	}
	regions := make([]string, 0, len(p.Free.ByRegion))
	for r := range p.Free.ByRegion {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	for _, r := range regions {
		if !free(p.Free.ByRegion[r]) {
			add("free.by_region[%s] %q is not a free provider", r, p.Free.ByRegion[r])
		}
	}
	tiers := map[string]bool{}
	for _, t := range p.Tiers {
		if t.ID == "" || tiers[t.ID] {
			add("tier id %q missing or duplicate", t.ID)
		}
		tiers[t.ID] = true
		if t.NameKey == "" || t.DescKey == "" || t.CostKey == "" {
			add("tier %s: name_key, desc_key and cost_key are required", t.ID)
		}
		if t.TranscribeOnly != (len(t.Roles) == 0) {
			add("tier %s: transcribe_only exactly when there are no roles", t.ID)
		}
		for role, r := range t.Roles {
			if r.Provider == FreeProvider {
				continue
			}
			if _, ok := provs[r.Provider]; !ok {
				add("tier %s role %s: unknown provider %q", t.ID, role, r.Provider)
			}
		}
	}
	if p.Default == "" || !tiers[p.Default] {
		add("default tier %q", p.Default)
	}
	return errs
}

// isHTTPSLink: an absolute "https://" address (lower-case scheme, as the
// page's own check ^https://\S+$ expects) with a host and without spaces or
// control characters.
func isHTTPSLink(s string) bool {
	if !strings.HasPrefix(s, "https://") ||
		strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Host != ""
}
