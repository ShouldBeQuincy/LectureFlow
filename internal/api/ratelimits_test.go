// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
)

// lateRedirect sends every request to the URL stored in to (set once the
// fake server's address is known).
type lateRedirect struct{ to *atomic.Pointer[url.URL] }

func (l lateRedirect) RoundTrip(r *http.Request) (*http.Response, error) {
	to := l.to.Load()
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = to.Scheme, to.Host, to.Host
	return http.DefaultTransport.RoundTrip(r)
}

// T-PERF-03: the provider view carries the limits the provider's answers
// reported (the real llm client against the fake server, which sends the
// headers; the provider is at api.anthropic.com, the only host read, and
// its requests are redirected to the fake), with the suggestion for the
// current settings; never a key or a raw header.
func TestProviderViewRateLimits(t *testing.T) {
	target := &atomic.Pointer[url.URL]{}
	e := newEnvWith(t, envConfig{server: func(s *Server) {
		s.llmc = llm.NewWithSettings(s.set, s.sc.Pool(), s.sc.Pool(), t.TempDir(), &http.Client{Transport: lateRedirect{target}})
	}})
	u, err := url.Parse(e.llm.URL)
	if err != nil {
		t.Fatal(err)
	}
	target.Store(u)
	const key = "sk-test-limits-0123456789abcd"
	call := func(method, path string, body any, out any) string {
		t.Helper()
		res, b := e.do(method, path, body)
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
		}
		if out != nil {
			decodeStrict(t, b, out)
		}
		return string(b)
	}
	var pv ProviderView
	call("POST", "/api/providers", ProviderInput{Name: ptr("Claude"), Type: ptr(model.ProviderAnthropic), BaseURL: ptr("https://" + llm.AnthropicAPIHost),
		APIKey: ptr(key), MaxConcurrency: ptr(6), RPM: ptr(100)}, &pv)
	if pv.RateLimits != nil {
		t.Fatalf("limits before any answer: %+v", pv.RateLimits)
	}
	call("PUT", "/api/settings/roles/"+model.RoleSyllabusFormat, RoleInput{ProviderID: pv.ID, Model: "claude-test-model"}, nil)

	// An answer without the headers: still nothing to show.
	ask := func() {
		t.Helper()
		if _, err := e.srv.llmc.Complete(context.Background(), model.RoleSyllabusFormat, ctxbuild.SyllabusFormatSystem, "outline", false); err != nil {
			t.Fatal(err)
		}
	}
	ask()
	provider := func() (ProviderView, string) {
		t.Helper()
		var ps []ProviderView
		body := call("GET", "/api/providers", nil, &ps)
		for _, p := range ps {
			if p.ID == pv.ID {
				return p, body
			}
		}
		t.Fatalf("provider %s not listed", pv.ID)
		return ProviderView{}, ""
	}
	if p, _ := provider(); p.RateLimits != nil {
		t.Fatalf("limits from an answer without headers: %+v", p.RateLimits)
	}

	e.llm.SetResponseHeaders(http.Header{
		"Anthropic-Ratelimit-Requests-Limit":      {"4000"},
		"Anthropic-Ratelimit-Requests-Remaining":  {"3999"},
		"Anthropic-Ratelimit-Input-Tokens-Limit":  {"2000000"},
		"Anthropic-Ratelimit-Output-Tokens-Limit": {"400000"},
	})
	before := time.Now().Add(-time.Second)
	ask()
	p, body := provider()
	rl := p.RateLimits
	if rl == nil || rl.Model != "claude-test-model" || rl.RequestsPerMinute != 4000 || rl.InputTokensPerMinute == nil || *rl.InputTokensPerMinute != 2000000 ||
		rl.OutputTokensPerMinute == nil || *rl.OutputTokensPerMinute != 400000 || rl.TokensPerMinute != nil {
		t.Fatalf("rate limits %+v", rl)
	}
	if at, err := time.Parse(time.RFC3339, rl.ObservedAt); err != nil || at.Before(before.Truncate(time.Second)) || !strings.HasSuffix(rl.ObservedAt, "Z") {
		t.Errorf("observed_at %q: %v", rl.ObservedAt, err)
	}
	// the suggestion is llm.AdviseLimits for the current settings
	l, _ := e.srv.llmc.(rateLimitSource).RateLimits(pv.ID)
	adv, _ := llm.AdviseLimits(l, 6, 100)
	if rl.Advice != (ProviderLimitAdvice{MaxConcurrency: adv.MaxConcurrency, RPM: adv.RPM, Below: adv.Below,
		ConcurrencyBelow: adv.ConcurrencyBelow, RPMBelow: adv.RPMBelow, Measured: adv.Measured}) || !rl.Advice.Below ||
		!rl.Advice.ConcurrencyBelow || !rl.Advice.RPMBelow || !rl.Advice.Measured || rl.Advice.MaxConcurrency <= 6 || rl.Advice.RPM <= 100 {
		t.Errorf("advice %+v, AdviseLimits %+v", rl.Advice, adv)
	}
	for _, bad := range []string{key, "anthropic-ratelimit", "Anthropic-Ratelimit", "remaining"} {
		if strings.Contains(body, bad) {
			t.Errorf("the provider list contains %q", bad)
		}
	}

	// Saving settings at or above the suggestion: not below any more, and
	// nothing is suggested below them.
	var saved ProviderView
	call("PATCH", "/api/providers/"+pv.ID, ProviderInput{MaxConcurrency: ptr(32), RPM: ptr(0)}, &saved)
	if saved.RateLimits == nil || saved.RateLimits.Advice.Below || saved.RateLimits.Advice.ConcurrencyBelow || saved.RateLimits.Advice.RPMBelow ||
		saved.RateLimits.Advice.MaxConcurrency != 32 || saved.RateLimits.Advice.RPM != 0 {
		t.Errorf("after saving 32 / no limit: %+v", saved.RateLimits)
	}
	// The settings themselves are what the user saved: nothing applied by itself.
	if saved.MaxConcurrency != 32 || saved.RPM != 0 {
		t.Errorf("saved %d / %d", saved.MaxConcurrency, saved.RPM)
	}

	// Another key may be another account: its limits are not known yet.
	var rekeyed ProviderView // a new value: decoding leaves absent fields as they were
	call("PATCH", "/api/providers/"+pv.ID, ProviderInput{APIKey: ptr("sk-test-other-account-987654")}, &rekeyed)
	if rekeyed.RateLimits != nil || rekeyed.KeyHint != "sk-…7654" {
		t.Errorf("limits of the old key after a key change: %+v", rekeyed.RateLimits)
	}

	// R38 S4: an Anthropic-type provider at another host (here the fake
	// server itself) is never read, whatever headers it sends.
	var other ProviderView
	call("POST", "/api/providers", ProviderInput{Name: ptr("Gateway"), Type: ptr(model.ProviderAnthropic), BaseURL: ptr(e.llm.URL),
		APIKey: ptr(key), MaxConcurrency: ptr(4)}, &other)
	call("PUT", "/api/settings/roles/"+model.RoleSyllabusFormat, RoleInput{ProviderID: other.ID, Model: "claude-test-model"}, nil)
	ask()
	var ps []ProviderView
	call("GET", "/api/providers", nil, &ps)
	for _, p := range ps {
		if p.ID == other.ID && p.RateLimits != nil {
			t.Errorf("limits read from a third-party host: %+v", p.RateLimits)
		}
	}
}

// RateLimitsView: nothing without a request limit.
func TestRateLimitsViewNone(t *testing.T) {
	if v := RateLimitsView(llm.RateLimits{InputTokens: 100}, 4, 0); v != nil {
		t.Errorf("%+v", v)
	}
}
