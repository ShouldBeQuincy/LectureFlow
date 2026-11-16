// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"strings"
	"testing"
)

// AUD5 info-1: the links of a provider preset (signup_url, keys_url,
// pricing_url, terms_url, refs) go into the onboarding page's href; a
// manifest is only valid when each of them is an https:// address with a
// host (the empty optional ones excepted).
func TestPresetLinksMustBeHTTPS(t *testing.T) {
	if errs := testPresets().validate(); len(errs) != 0 {
		t.Fatalf("test presets: %v", errs)
	}
	for _, bad := range []string{
		"http://example.invalid/s",
		"javascript:void(0)//AUD5",
		"data:text/html,x",
		"HTTPS://example.invalid/s",
		"https://",
		"https:///path",
		"https://example.invalid/a b",
		"https://example.invalid/\n",
		"//example.invalid/s",
		"example.invalid/s",
	} {
		for field, set := range map[string]func(*PresetProvider){
			"signup_url":  func(p *PresetProvider) { p.SignupURL = bad },
			"keys_url":    func(p *PresetProvider) { p.KeysURL = bad },
			"pricing_url": func(p *PresetProvider) { p.PricingURL = bad },
			"terms_url":   func(p *PresetProvider) { p.TermsURL = bad },
			"refs":        func(p *PresetProvider) { p.Refs = []string{"https://example.invalid/ok", bad} },
		} {
			p := testPresets()
			set(&p.Providers[1])
			errs := p.validate()
			if !strings.Contains(strings.Join(errs, "; "), field) {
				t.Errorf("%s = %q accepted (errors %v)", field, bad, errs)
			}
		}
	}
	// optional links may be left out
	p := testPresets()
	p.Providers[0].PricingURL, p.Providers[0].TermsURL, p.Providers[0].Refs = "", "", nil
	if errs := p.validate(); len(errs) != 0 {
		t.Errorf("without the optional links: %v", errs)
	}
}

// Every link of the shipped presets is https (the built-in manifest loads,
// and each link passes the check on its own).
func TestShippedPresetLinksAreHTTPS(t *testing.T) {
	m, err := LoadManifest(builtinManifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Presets.Providers) == 0 {
		t.Fatal("no shipped providers")
	}
	for _, p := range m.Presets.Providers {
		links := append([]string{p.SignupURL, p.KeysURL, p.PricingURL, p.TermsURL}, p.Refs...)
		for _, l := range links {
			if l != "" && !isHTTPSLink(l) {
				t.Errorf("%s: link %q", p.ID, l)
			}
		}
	}
}
