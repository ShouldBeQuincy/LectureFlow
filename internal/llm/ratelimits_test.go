// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// T-PERF-03: the account limits of Anthropic answers and the suggestion
// made from them. The fake servers below answer with the headers; no
// real request is sent.

func limitHeaders(w http.ResponseWriter, kv ...string) {
	for i := 0; i+1 < len(kv); i += 2 {
		w.Header().Set(kv[i], kv[i+1])
	}
}

// limitsClient is a settings-mode client whose Anthropic provider is at
// api.anthropic.com (the only host whose limits are read); its requests
// go to the test server srvURL. Every role on claude-test-model.
func limitsClient(t *testing.T, srvURL string) (*HTTPClient, *settings.Service, model.Provider) {
	t.Helper()
	set := newSettings(t)
	p := addProvider(t, set, "Claude", model.ProviderAnthropic, "https://"+AnthropicAPIHost, testKey)
	for _, role := range model.Roles {
		setRole(t, set, role, p.ID, "claude-test-model", model.RoleParams{})
	}
	c := NewWithSettings(set, &fakePool{}, &fakeRecorder{}, "", redirectClient(t, srvURL))
	c.Sleep = (&recorder{}).sleep
	return c, set, p
}

var fullLimits = []string{
	"anthropic-ratelimit-requests-limit", "4000",
	"anthropic-ratelimit-requests-remaining", "3999",
	"anthropic-ratelimit-requests-reset", "2026-11-11T08:00:01Z",
	"anthropic-ratelimit-input-tokens-limit", "2000000",
	"anthropic-ratelimit-output-tokens-limit", "400000",
	"anthropic-ratelimit-tokens-limit", "2400000",
}

func TestParseRateLimits(t *testing.T) {
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	const req = "anthropic-ratelimit-requests-limit"
	for _, tc := range []struct {
		name string
		h    http.Header
		want RateLimits
		ok   bool
	}{
		{"all", h(fullLimits...), RateLimits{Requests: 4000, InputTokens: 2000000, OutputTokens: 400000, Tokens: 2400000}, true},
		{"requests only, spaces", h(req, " 50 "), RateLimits{Requests: 50}, true},
		{"nil", nil, RateLimits{}, false},
		{"none", h("x-ratelimit-limit-requests", "4000", "retry-after", "3"), RateLimits{}, false},
		{"not a number", h(req, "many"), RateLimits{}, false},
		{"fraction", h(req, "4000.5"), RateLimits{}, false},
		{"negative", h(req, "-4000"), RateLimits{}, false},
		{"zero", h(req, "0"), RateLimits{}, false},
		{"too large", h(req, "100000001"), RateLimits{}, false},
		{"overflow", h(req, "99999999999999999999999"), RateLimits{}, false},
		{"hex", h(req, "0x10"), RateLimits{}, false},
		// a bad token limit is dropped, the request limit kept
		{"bad tokens", h(req, "1000", "anthropic-ratelimit-input-tokens-limit", "-1",
			"anthropic-ratelimit-output-tokens-limit", "1e6", "anthropic-ratelimit-tokens-limit", "1000000000001"), RateLimits{Requests: 1000}, true},
	} {
		got, ok := parseRateLimits(tc.h)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: %+v %v, want %+v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAdviseLimits(t *testing.T) {
	measured := func(l RateLimits, secs, in, out float64) RateLimits {
		l.Samples, l.MeanSeconds, l.MeanUncachedInput, l.MeanOutput = 10, secs, in, out
		return l
	}
	opus := RateLimits{Requests: 4000, InputTokens: 2000000, OutputTokens: 400000}
	for _, tc := range []struct {
		name      string
		l         RateLimits
		conc, rpm int
		want      LimitAdvice
		ok        bool
	}{
		{"no data", RateLimits{}, 6, 100, LimitAdvice{}, false},
		{"token limits only", RateLimits{InputTokens: 1000}, 6, 100, LimitAdvice{}, false},
		// the audit's case: 6 at a time, 100 a minute, 4000 a minute
		// observed, nothing measured yet: 2400/min, 2400*3/60=120 → 32
		{"user's settings, unmeasured", opus, 6, 100, LimitAdvice{MaxConcurrency: 32, RPM: 2400, Below: true, ConcurrencyBelow: true, RPMBelow: true}, true},
		// measured 2.3 s, 2000 uncached input and 300 output tokens a
		// request: input allows 600/min, output 800/min → 600/min,
		// 600*2.3/60 = 23
		{"user's settings, measured", measured(opus, 2.3, 2000, 300), 6, 100, LimitAdvice{MaxConcurrency: 23, RPM: 600, Below: true, ConcurrencyBelow: true, RPMBelow: true, Measured: true}, true},
		// R38 M1: Anthropic's default, 4 at a time and no per-minute limit:
		// only the concurrency is below
		{"default 4 / unlimited", measured(opus, 2.3, 2000, 300), 4, 0, LimitAdvice{MaxConcurrency: 23, RPM: 0, Below: true, ConcurrencyBelow: true, Measured: true}, true},
		// no per-minute limit stays none
		{"unlimited rpm", opus, 6, 0, LimitAdvice{MaxConcurrency: 32, RPM: 0, Below: true, ConcurrencyBelow: true}, true},
		// already enough: not below, nothing lowered
		{"enough", opus, 32, 0, LimitAdvice{MaxConcurrency: 32, RPM: 0}, true},
		{"enough, above", opus, 40, 3000, LimitAdvice{MaxConcurrency: 40, RPM: 3000}, true},
		{"slightly below is not clearly below", opus, 20, 1500, LimitAdvice{MaxConcurrency: 32, RPM: 2400}, true},
		// the concurrency is fine, the per-minute limit is not
		{"rpm below", opus, 32, 100, LimitAdvice{MaxConcurrency: 32, RPM: 2400, Below: true, RPMBelow: true}, true},
		// a small account: 10/min → 6/min, one at a time; the user's
		// settings are above it and stay
		{"small limit", RateLimits{Requests: 10}, 6, 100, LimitAdvice{MaxConcurrency: 6, RPM: 100}, true},
		{"tiny limit", RateLimits{Requests: 1}, 1, 0, LimitAdvice{MaxConcurrency: 1, RPM: 0}, true},
		{"tiny limit, rpm set", RateLimits{Requests: 1}, 1, 1, LimitAdvice{MaxConcurrency: 1, RPM: 1}, true},
		// a large account: capped at settings.MaxRPM and 32 at a time
		{"large limit", RateLimits{Requests: 100000}, 4, 60, LimitAdvice{MaxConcurrency: MaxSuggestedConcurrency, RPM: settings.MaxRPM, Below: true, ConcurrencyBelow: true, RPMBelow: true}, true},
		// long requests (60 s) need more at a time for the same rate,
		// still capped
		{"long requests", measured(RateLimits{Requests: 50}, 60, 0, 0), 4, 0, LimitAdvice{MaxConcurrency: 30, RPM: 0, Below: true, ConcurrencyBelow: true, Measured: true}, true},
		// very short requests are taken as 0.5 s: 2400*0.5/60 = 20
		{"short requests", measured(RateLimits{Requests: 4000}, 0.01, 0, 0), 4, 0, LimitAdvice{MaxConcurrency: 20, RPM: 0, Below: true, ConcurrencyBelow: true, Measured: true}, true},
		// the combined token limit when the split ones are absent:
		// 0.6*100000/(900+100) = 60/min
		{"combined tokens", measured(RateLimits{Requests: 4000, Tokens: 100000}, 3, 900, 100), 1, 10, LimitAdvice{MaxConcurrency: 3, RPM: 60, Below: true, ConcurrencyBelow: true, RPMBelow: true, Measured: true}, true},
	} {
		got, ok := AdviseLimits(tc.l, tc.conc, tc.rpm)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: %+v %v, want %+v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// The whole-message path (the answer is JSON, not an event stream: the
// branch of http.go that reads the body at once): the limits and the
// measured answer (uncached input = 450 prompt - 300 cache reads).
func TestRateLimitsObservedWholeMessage(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
		limitHeaders(w, fullLimits...)
		io.WriteString(w, anthropicOK(`{"ok": true}`))
	})
	c, _, p := limitsClient(t, srv.URL)
	if _, ok := c.RateLimits(p.ID); ok {
		t.Fatal("limits before any answer")
	}
	before := time.Now()
	if _, err := c.Complete(context.Background(), model.RoleTranslate, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	if n := len(got()); n != 1 {
		t.Fatalf("%d requests", n)
	}
	l, ok := c.RateLimits(p.ID)
	if !ok || l.Model != "claude-test-model" || l.Requests != 4000 || l.InputTokens != 2000000 || l.OutputTokens != 400000 || l.Tokens != 2400000 ||
		l.ObservedAt.Before(before) || l.Samples != 1 || l.MeanUncachedInput != 150 || l.MeanOutput != 20 || l.MeanSeconds <= 0 {
		t.Fatalf("limits %+v %v", l, ok)
	}
	if _, ok := c.RateLimits("no-such-provider"); ok {
		t.Error("limits of an unknown provider")
	}
}

// The streamed path (a Claude model with thinking): the headers come with
// the event stream.
func TestRateLimitsObservedStream(t *testing.T) {
	s := newSEC4Server(t, func(n int, w http.ResponseWriter, r *http.Request) {
		limitHeaders(w, "anthropic-ratelimit-requests-limit", "1000", "anthropic-ratelimit-output-tokens-limit", "80000")
		writeSSE(w, sec4OK(t, "streamed answer"))
	})
	c, set, p := limitsClient(t, s.URL)
	setRole(t, set, model.RoleTranslate, p.ID, "claude-opus-5-5", model.RoleParams{Thinking: sp("low")})
	if _, err := c.Complete(context.Background(), model.RoleTranslate, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	streamed := len(s.bodies) == 1 && s.bodies[0]["stream"] == true
	s.mu.Unlock()
	if !streamed {
		t.Fatal("the request was not streamed")
	}
	l, ok := c.RateLimits(p.ID)
	if !ok || l.Model != "claude-opus-5-5" || l.Requests != 1000 || l.OutputTokens != 80000 || l.InputTokens != 0 || l.Samples != 1 || l.MeanOutput != 20 {
		t.Fatalf("limits %+v %v", l, ok)
	}
}

// A 429 reports the limits too (without a measured answer); a success
// without the headers changes nothing.
func TestRateLimitsFrom429(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			limitHeaders(w, "anthropic-ratelimit-requests-limit", "50", "retry-after", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`)
			return
		}
		io.WriteString(w, anthropicOK(`{"ok": true}`))
	})
	c, _, p := limitsClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), model.RoleTranslate, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	if n := len(got()); n != 2 {
		t.Fatalf("%d requests, want the 429 and its retry", n)
	}
	l, ok := c.RateLimits(p.ID)
	if !ok || l.Requests != 50 || l.Samples != 0 {
		t.Fatalf("limits %+v %v", l, ok)
	}
}

// Per provider and model: the latest answer of a model replaces its
// limits; the provider shows its most restrictive model; a new key hides
// what the old one's answers said.
func TestRateLimitsPerModelAndKey(t *testing.T) {
	var limit string
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		limitHeaders(w, "anthropic-ratelimit-requests-limit", limit)
		io.WriteString(w, anthropicOK(`{"ok": true}`))
	})
	c, set, p := limitsClient(t, srv.URL)
	setRole(t, set, model.RoleSummary, p.ID, "claude-small", model.RoleParams{})
	call := func(role, lim string) {
		t.Helper()
		limit = lim
		if _, err := c.Complete(context.Background(), role, "S", "U", false); err != nil {
			t.Fatal(err)
		}
	}
	want := func(mdl string, req int64, samples int) {
		t.Helper()
		l, ok := c.RateLimits(p.ID)
		if !ok || l.Model != mdl || l.Requests != req || l.Samples != samples {
			t.Fatalf("limits %+v %v, want %s %d (%d samples)", l, ok, mdl, req, samples)
		}
	}
	call(model.RoleTranslate, "4000")
	want("claude-test-model", 4000, 1)
	call(model.RoleSummary, "1000")
	want("claude-small", 1000, 1)
	call(model.RoleTranslate, "3000") // still above claude-small's
	want("claude-small", 1000, 1)
	call(model.RoleSummary, "5000") // claude-small's replaced: now claude-test-model is the smaller
	want("claude-test-model", 3000, 2)
	call(model.RoleSummary, "garbage") // ignored: nothing changes, not even the samples
	want("claude-test-model", 3000, 2)
	// equal limits: the first model by name, whichever answered last
	call(model.RoleSummary, "3000")
	want("claude-small", 3000, 3)
	call(model.RoleTranslate, "3000")
	want("claude-small", 3000, 3)

	k := "sk-another-account-0000000000"
	if _, err := set.UpdateProvider(context.Background(), p.ID, settings.ProviderInput{APIKey: &k}); err != nil {
		t.Fatal(err)
	}
	if l, ok := c.RateLimits(p.ID); ok {
		t.Fatalf("limits of the old key after a key change: %+v", l)
	}
	call(model.RoleTranslate, "200")
	want("claude-test-model", 200, 1)

	// A provider switched to the OpenAI-compatible type shows none.
	typ := model.ProviderOpenAI
	if _, err := set.UpdateProvider(context.Background(), p.ID, settings.ProviderInput{Type: &typ}); err != nil {
		t.Fatal(err)
	}
	if l, ok := c.RateLimits(p.ID); ok {
		t.Fatalf("limits of a provider that is no longer Anthropic: %+v", l)
	}
}

// Only Anthropic providers: an OpenAI-compatible one's x-ratelimit-*
// headers (or anthropic-ratelimit-* ones) are not read.
func TestRateLimitsNotReadForOpenAI(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		limitHeaders(w, "x-ratelimit-limit-requests", "4000", "anthropic-ratelimit-requests-limit", "4000")
		io.WriteString(w, okBody(`{"ok": true}`))
	})
	set := newSettings(t)
	p := addProvider(t, set, "Compat", model.ProviderOpenAI, srv.URL, testKey)
	setRole(t, set, model.RoleTranslate, p.ID, "some-model", model.RoleParams{})
	c := NewWithSettings(set, nil, &fakeRecorder{}, "", nil)
	if _, err := c.Complete(context.Background(), model.RoleTranslate, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	if l, ok := c.RateLimits(p.ID); ok {
		t.Fatalf("limits read from an OpenAI-compatible provider: %+v", l)
	}
}

// The store stays bounded.
func TestRateLimitStoreBounded(t *testing.T) {
	var st rateLimitStore
	at := time.Unix(1_800_000_000, 0)
	for i := range maxLimitEntries + 10 {
		st.observe(limitKey{provider: "p", model: strings.Repeat("m", i+1)}, RateLimits{Requests: int64(1000 + i)}, at.Add(time.Duration(i)*time.Second), nil)
	}
	if n := len(st.m); n != maxLimitEntries {
		t.Fatalf("%d entries", n)
	}
	// the oldest (smallest limits) went first
	if l, ok := st.get("p", "", "", false, at.Add(time.Hour)); !ok || l.Requests != 1010 {
		t.Fatalf("%+v %v", l, ok)
	}
}

// R38 S2: a model not seen for 24 hours no longer holds its provider's
// limits down.
func TestRateLimitsStale(t *testing.T) {
	var st rateLimitStore
	at := time.Unix(1_800_000_000, 0)
	k := func(m string) limitKey { return limitKey{provider: "p", host: AnthropicAPIHost, model: m} }
	st.observe(k("old-model"), RateLimits{Requests: 1000}, at, nil)
	st.observe(k("new-model"), RateLimits{Requests: 4000}, at.Add(20*time.Hour), nil)
	if l, ok := st.get("p", AnthropicAPIHost, "", false, at.Add(23*time.Hour)); !ok || l.Model != "old-model" {
		t.Fatalf("within 24 h: %+v %v", l, ok)
	}
	if l, ok := st.get("p", AnthropicAPIHost, "", false, at.Add(25*time.Hour)); !ok || l.Model != "new-model" {
		t.Fatalf("after 24 h: %+v %v", l, ok)
	}
	if l, ok := st.get("p", AnthropicAPIHost, "", false, at.Add(45*time.Hour)); ok {
		t.Fatalf("all stale: %+v", l)
	}
	if _, ok := st.get("p", "gateway.example.com", "", false, at.Add(time.Hour)); ok {
		t.Fatal("another host's group")
	}
}

// R38 S4: an Anthropic-type provider at another host (a gateway, a
// compatible service) is not read, even with headers of the same name;
// nor is one moved there afterwards.
func TestRateLimitsOnlyOfficialHost(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		limitHeaders(w, fullLimits...)
		io.WriteString(w, anthropicOK(`{"ok": true}`))
	})
	c, set, p, _, _, _ := anthropicClient(t, srv.URL) // base_url = the test server itself
	if _, err := c.Complete(context.Background(), model.RoleTranslate, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	if l, ok := c.RateLimits(p.ID); ok {
		t.Fatalf("limits read from %s: %+v", srv.URL, l)
	}
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://api.anthropic.com", true}, {"https://API.Anthropic.com:443/", true}, {"https://api.anthropic.com/v1/messages", true},
		{"https://api.anthropic.com.example.com", false}, {"http://api.anthropic.com", false}, {"HTTP://api.anthropic.com", false}, {"HTTPS://api.anthropic.com", true}, {"https://gateway.example.com/anthropic", false}, {"http://127.0.0.1:8080", false}, {"", false}, {"::bad", false},
	} {
		if got := OfficialAnthropicHost(tc.url); got != tc.want {
			t.Errorf("OfficialAnthropicHost(%q) = %v", tc.url, got)
		}
	}

	// observed at the official host, then the provider moves away
	c2, set2, p2 := limitsClient(t, srv.URL)
	if _, err := c2.Complete(context.Background(), model.RoleTranslate, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.RateLimits(p2.ID); !ok {
		t.Fatal("no limits at the official host")
	}
	moved, key := "https://gateway.example.com", testKey
	if _, err := set2.UpdateProvider(context.Background(), p2.ID, settings.ProviderInput{BaseURL: &moved, APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	if l, ok := c2.RateLimits(p2.ID); ok {
		t.Fatalf("limits after moving to another host: %+v", l)
	}
	_ = set
}
