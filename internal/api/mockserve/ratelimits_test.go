// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/api"
)

// T-PERF-03: the default data's Anthropic provider has observed account
// limits with a suggestion; ?mock=nolimits has none. Saving settings
// recomputes the suggestion (nothing is applied by itself); a new key or
// another type drops the limits, as the real server does.
func TestMockRateLimits(t *testing.T) {
	noLatency(t)
	root := filepath.Join("..", "..", "..")
	srv := httptest.NewServer(newHandler(filepath.Join(root, "web"), filepath.Join(root, "testdata", "api"), 10*time.Millisecond))
	t.Cleanup(srv.Close)
	get := func(query string) []api.ProviderView {
		t.Helper()
		var s api.Settings
		doJSON(t, "GET", srv.URL+"/api/settings"+query, nil, http.StatusOK, &s)
		return s.Providers
	}

	ps := get("")
	a := findProvider(ps, pidAnthropic)
	if a == nil || a.RateLimits == nil || a.RateLimits.RequestsPerMinute != 4000 || a.RateLimits.Model != "claude-opus-5-5" ||
		a.RateLimits.Advice != (api.ProviderLimitAdvice{MaxConcurrency: 23, RPM: 600, Below: true, ConcurrencyBelow: true, RPMBelow: true, Measured: true}) {
		t.Fatalf("default Anthropic: %+v", a)
	}
	for _, p := range ps {
		if p.ID != pidAnthropic && p.RateLimits != nil {
			t.Errorf("%s has limits: %+v", p.Name, p.RateLimits)
		}
	}
	for _, p := range get("?mock=nolimits") {
		if p.RateLimits != nil {
			t.Errorf("nolimits: %s has limits: %+v", p.Name, p.RateLimits)
		}
	}

	// R38 M1: Anthropic's default, 4 at a time and no per-minute limit:
	// only the concurrency is below, the per-minute limit stays none
	var pv api.ProviderView
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+pidAnthropic, api.ProviderInput{MaxConcurrency: ip(4), RPM: ip(0)}, http.StatusOK, &pv)
	if pv.RateLimits == nil || pv.RateLimits.Advice != (api.ProviderLimitAdvice{MaxConcurrency: 23, RPM: 0, Below: true, ConcurrencyBelow: true, Measured: true}) {
		t.Fatalf("4 / no limit: %+v", pv.RateLimits)
	}
	// the user saves the suggestion: no longer below; the settings are
	// exactly what was saved
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+pidAnthropic, api.ProviderInput{MaxConcurrency: ip(23), RPM: ip(600)}, http.StatusOK, &pv)
	if pv.MaxConcurrency != 23 || pv.RPM != 600 || pv.RateLimits == nil || pv.RateLimits.Advice != (api.ProviderLimitAdvice{MaxConcurrency: 23, RPM: 600, Measured: true}) {
		t.Fatalf("after saving the suggestion: %+v", pv)
	}
	if a := findProvider(get(""), pidAnthropic); a == nil || a.RateLimits == nil || a.RateLimits.Advice.Below {
		t.Fatalf("listed after saving: %+v", a)
	}
	// a new key: the old key's limits are gone
	key := "sk-mock-another-0000"
	var rekeyed api.ProviderView
	doJSON(t, "PATCH", srv.URL+"/api/providers/"+pidAnthropic, api.ProviderInput{APIKey: &key}, http.StatusOK, &rekeyed)
	if rekeyed.RateLimits != nil {
		t.Fatalf("after a new key: %+v", rekeyed.RateLimits)
	}
	// R38 S4: none at a third-party host (the mock keeps the data, the
	// view hides it, as the real server does)
	if v := mockRateLimits(api.ProviderView{Type: "anthropic", BaseURL: "https://gateway.example.com", RateLimits: a.RateLimits}); v != nil {
		t.Fatalf("third-party host: %+v", v)
	}
	if v := mockRateLimits(api.ProviderView{Type: "anthropic", BaseURL: "http://api.anthropic.com", RateLimits: a.RateLimits}); v != nil {
		t.Fatalf("plain http (R38b N2): %+v", v)
	}
	if v := mockRateLimits(api.ProviderView{Type: "anthropic", BaseURL: "https://api.anthropic.com", MaxConcurrency: 4, RateLimits: a.RateLimits}); v == nil {
		t.Fatal("official host: none")
	}
}
