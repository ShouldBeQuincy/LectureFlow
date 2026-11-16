// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// T-PERF-03 (user 2026-11-11: show, never apply): the account limits an
// Anthropic provider reports in its response headers, kept per provider
// and model so the settings page can show them with a suggested
// concurrency and requests per minute.
//
// Anthropic's documentation (platform.claude.com/docs/en/api/rate-limits,
// checked 2026-11-11): every Messages response carries
// anthropic-ratelimit-requests-limit ("the maximum number of requests
// allowed within any rate limit period"; the limits are per minute and per
// model class) and the -input-tokens-limit / -output-tokens-limit /
// -tokens-limit counterparts; a 429 carries them too. Only the uncached
// input counts towards the input token limit. There is no header for, and
// no documented limit on, concurrent requests: the concurrency is
// suggested from the requests per minute and the measured time of a
// request. Other providers are not read: DeepSeek documents a concurrency
// limit per account and no rate limit headers. Neither are Anthropic-type
// providers at another host (a gateway or a compatible service may send
// headers of the same name that mean something else): only
// https://api.anthropic.com. The documentation does not say whether a 429 carries
// the headers; when an answer has none, nothing is recorded.
//
// Nothing is persisted and no request is ever sent to learn the limits: a
// new process knows them after its first Anthropic answer.

const (
	// limitHeadroom is the share of an observed limit a suggestion uses.
	limitHeadroom = 0.6
	// MaxSuggestedConcurrency caps the suggested concurrency. The
	// performance audit (FB2 F03) estimated that 24-32 at a time bring a
	// lecture's translation (about 240 one-sentence requests of ~2.3 s)
	// from 148 s to about 20 s; beyond 32 the warm-up and the slowest
	// request dominate, while every extra request in flight raises the
	// risk of a burst of 429s (Anthropic also warns of "acceleration
	// limits" on a sharp increase in usage).
	MaxSuggestedConcurrency = 32
	// defaultRequestSeconds is the time of a request assumed before one
	// was measured (the audit's median translation request took 2.3 s).
	defaultRequestSeconds = 3.0
	// limitSamples is how many recent answers the typical request time
	// and token counts are measured over.
	limitSamples = 64
	// maxLimitEntries bounds the (provider, host, model, key) entries kept.
	maxLimitEntries = 64
	// limitsFresh: a model not seen for this long no longer counts for its
	// provider (a model used once, e.g. a retry with an old snapshot, would
	// otherwise hold the provider's limits down until a restart).
	limitsFresh = 24 * time.Hour
	// AnthropicAPIHost is the only host whose limit headers are read.
	AnthropicAPIHost = "api.anthropic.com"
	// Header values above these are not believed.
	maxRequestsLimit = 100_000_000
	maxTokensLimit   = 1_000_000_000_000
)

// RateLimits is what one provider's answers for one model said about the
// account's limits (per minute; 0 = not reported), with the typical
// request measured over its recent successful answers.
type RateLimits struct {
	Model                                       string
	Requests, InputTokens, OutputTokens, Tokens int64
	ObservedAt                                  time.Time
	// Samples answers measured: their mean duration, uncached input
	// tokens (input + cache writes; what the input limit counts) and
	// output tokens.
	Samples                                    int
	MeanSeconds, MeanUncachedInput, MeanOutput float64
}

// LimitAdvice is the suggestion for a provider's settings. MaxConcurrency
// and RPM are the values to fill in: never below the current ones (an rpm
// of 0, no per-minute limit, stays 0). ConcurrencyBelow / RPMBelow: that
// current setting is clearly below the suggestion (less than half of it;
// never RPMBelow without a per-minute limit); Below: either. Measured: the
// suggestion rests on measured answers (time and tokens); without them it
// only follows the request limit (and 3 s a request).
type LimitAdvice struct {
	MaxConcurrency, RPM               int
	Below, ConcurrencyBelow, RPMBelow bool
	Measured                          bool
}

// AdviseLimits suggests a concurrency and a per-minute limit from the
// observed limits l for the current settings (curRPM 0 = no per-minute
// limit). ok is false when no request limit was observed. The rules:
//
//   - per minute: 60 % of the observed request limit, and of what the
//     token limits allow at the measured tokens per request; at least 1,
//     at most settings.MaxRPM;
//   - concurrency: what reaches that rate at the measured mean request
//     time (3 s before one was measured, clamped to 0.5-120 s), rounded
//     up; 1 to MaxSuggestedConcurrency;
//   - below: the current concurrency is less than half the suggested one,
//     or the current per-minute limit (when set) less than half of it.
func AdviseLimits(l RateLimits, curConc, curRPM int) (LimitAdvice, bool) {
	if l.Requests <= 0 {
		return LimitAdvice{}, false
	}
	budget := float64(l.Requests) * limitHeadroom
	if l.Samples > 0 {
		in, out := l.MeanUncachedInput, l.MeanOutput
		if l.InputTokens > 0 && in >= 1 {
			budget = math.Min(budget, float64(l.InputTokens)*limitHeadroom/in)
		}
		if l.OutputTokens > 0 && out >= 1 {
			budget = math.Min(budget, float64(l.OutputTokens)*limitHeadroom/out)
		}
		if l.InputTokens == 0 && l.OutputTokens == 0 && l.Tokens > 0 && in+out >= 1 {
			budget = math.Min(budget, float64(l.Tokens)*limitHeadroom/(in+out))
		}
	}
	rpm := clampInt(int(math.Floor(budget)), 1, settings.MaxRPM)
	secs := defaultRequestSeconds
	if l.Samples > 0 && l.MeanSeconds > 0 {
		secs = math.Min(math.Max(l.MeanSeconds, 0.5), 120)
	}
	conc := clampInt(int(math.Ceil(float64(rpm)*secs/60)), 1, MaxSuggestedConcurrency)
	adv := LimitAdvice{
		MaxConcurrency:   max(conc, curConc),
		RPM:              rpm,
		ConcurrencyBelow: 2*curConc < conc,
		RPMBelow:         curRPM > 0 && 2*curRPM < rpm,
		Measured:         l.Samples > 0,
	}
	adv.Below = adv.ConcurrencyBelow || adv.RPMBelow
	switch {
	case curRPM <= 0:
		adv.RPM = 0 // no per-minute limit stays none
	case curRPM > rpm:
		adv.RPM = curRPM
	}
	return adv, true
}

func clampInt(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

// parseLimit reads one limit header: a whole number from 1 to most;
// anything else (absent, not a number, negative, zero, too large) is 0.
func parseLimit(h http.Header, name string, most int64) int64 {
	v := strings.TrimSpace(h.Get(name))
	if v == "" || len(v) > 20 {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 || n > most {
		return 0
	}
	return n
}

// parseRateLimits reads the limit headers of an Anthropic response; ok is
// false without a valid request limit.
func parseRateLimits(h http.Header) (l RateLimits, ok bool) {
	if h == nil {
		return l, false
	}
	l.Requests = parseLimit(h, "anthropic-ratelimit-requests-limit", maxRequestsLimit)
	if l.Requests == 0 {
		return RateLimits{}, false
	}
	l.InputTokens = parseLimit(h, "anthropic-ratelimit-input-tokens-limit", maxTokensLimit)
	l.OutputTokens = parseLimit(h, "anthropic-ratelimit-output-tokens-limit", maxTokensLimit)
	l.Tokens = parseLimit(h, "anthropic-ratelimit-tokens-limit", maxTokensLimit)
	return l, true
}

// keyFingerprint tells answers of one key from another's (a provider whose
// key changed may be another account) without keeping the key.
func keyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// OfficialAnthropicHost: rawURL is https and its host is api.anthropic.com
// (any case, any port). Over plain http the limit headers could be forged
// on the way, so they are neither read nor shown (review R38b N2). Keep in
// step with officialAnthropicHost in web/provider_limits.js: a change here
// is a change there.
func OfficialAnthropicHost(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	return err == nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), AnthropicAPIHost)
}

type limitKey struct{ provider, host, model, key string }

type limitSample struct{ secs, in, out float64 }

type limitEntry struct {
	limits  RateLimits
	samples [limitSamples]limitSample
	n, next int
}

// rateLimitStore keeps the latest limits per (provider, model, key); the
// zero value is ready to use.
type rateLimitStore struct {
	mu sync.Mutex
	m  map[limitKey]*limitEntry
}

// observe records the limits of one answer, and its time and tokens when
// it is a success (s != nil).
func (st *rateLimitStore) observe(k limitKey, l RateLimits, at time.Time, s *limitSample) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.m == nil {
		st.m = map[limitKey]*limitEntry{}
	}
	e := st.m[k]
	if e == nil {
		if len(st.m) >= maxLimitEntries {
			st.evictOldestLocked()
		}
		e = &limitEntry{}
		st.m[k] = e
	}
	l.Model, l.ObservedAt = k.model, at
	e.limits = l
	if s != nil {
		e.samples[e.next] = *s
		e.next = (e.next + 1) % limitSamples
		e.n = min(e.n+1, limitSamples)
	}
}

func (st *rateLimitStore) evictOldestLocked() {
	var oldest limitKey
	var at time.Time
	first := true
	for k, e := range st.m {
		if first || e.limits.ObservedAt.Before(at) {
			oldest, at, first = k, e.limits.ObservedAt, false
		}
	}
	delete(st.m, oldest)
}

// get returns the provider's most restrictive limits at host (the smallest
// request limit; of equal ones the first model by name, so the choice does
// not flip with the latest request) among the answers to the key with
// fingerprint key (any key when !byKey) observed within limitsFresh of now.
func (st *rateLimitStore) get(provider, host, key string, byKey bool, now time.Time) (RateLimits, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var best RateLimits
	found := false
	for k, e := range st.m {
		if k.provider != provider || k.host != host || (byKey && k.key != key) || now.Sub(e.limits.ObservedAt) > limitsFresh {
			continue
		}
		l := e.limits
		if e.n > 0 {
			var secs, in, out float64
			for _, s := range e.samples[:e.n] {
				secs, in, out = secs+s.secs, in+s.in, out+s.out
			}
			n := float64(e.n)
			l.Samples, l.MeanSeconds, l.MeanUncachedInput, l.MeanOutput = e.n, secs/n, in/n, out/n
		}
		if !found || l.Requests < best.Requests || (l.Requests == best.Requests && l.Model < best.Model) {
			best, found = l, true
		}
	}
	return best, found
}

// observeLimits records what an attempt's response said about the
// account's limits: Anthropic providers at api.anthropic.com only, any
// status; nothing when the headers are absent or invalid. A successful
// answer also adds its time and tokens.
func (c *HTTPClient) observeLimits(call callInfo, out attemptOutcome, res Result, aerr *attemptErr) {
	if call.api != model.ProviderAnthropic || call.provider == "" || out.header == nil || !OfficialAnthropicHost(call.url) {
		return
	}
	l, ok := parseRateLimits(out.header)
	if !ok {
		return
	}
	var s *limitSample
	if out.status == http.StatusOK && aerr == nil && out.duration > 0 {
		in := res.PromptTokens - res.CacheHitTokens
		s = &limitSample{secs: out.duration.Seconds(), in: float64(max(in, 0)), out: float64(max(res.CompletionTokens, 0))}
	}
	c.limits.observe(limitKey{provider: call.provider, host: AnthropicAPIHost, model: call.model, key: keyFingerprint(call.key)}, l, time.Now(), s)
}

// RateLimits returns the most restrictive limits the provider's answers
// reported in the last limitsFresh, for its current key and host
// (settings mode: an Anthropic provider at api.anthropic.com); ok is false
// when none did.
func (c *HTTPClient) RateLimits(providerID string) (RateLimits, bool) {
	now := time.Now()
	if c.set == nil {
		return c.limits.get(providerID, AnthropicAPIHost, "", false, now)
	}
	p, ok := c.set.Provider(providerID)
	if !ok || p.Type != model.ProviderAnthropic || !OfficialAnthropicHost(p.BaseURL) { // switched to another type or host
		return RateLimits{}, false
	}
	return c.limits.get(providerID, AnthropicAPIHost, keyFingerprint(p.APIKey), true, now)
}
