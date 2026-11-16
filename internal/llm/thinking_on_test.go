// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// Per-role "深度思考" turned on (.claude/thinking-brief.md section 2; the
// Anthropic form per model: NOTES.md 决定（深度思考 T1）).

func sp(s string) *string { return &s }

// thinkingEnv is a settings-mode client whose requests (whatever host the
// provider names) go to one test server answering with respond(n, path).
type thinkingEnv struct {
	t    *testing.T
	set  *settings.Service
	c    *HTTPClient
	got  func() []rawCapture
	url  string
	rec  *recorder
	used *fakeRecorder
}

func newThinkingEnv(t *testing.T, respond func(n int, path string, w http.ResponseWriter)) *thinkingEnv {
	t.Helper()
	var mu sync.Mutex
	var got []rawCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, rawCapture{r.URL.Path, b})
		n := len(got)
		mu.Unlock()
		respond(n, r.URL.Path, w)
	}))
	t.Cleanup(srv.Close)
	set := newSettings(t)
	rec, used := &recorder{}, &fakeRecorder{}
	c := NewWithSettings(set, nil, used, "", redirectClient(t, srv.URL))
	c.Sleep = rec.sleep
	return &thinkingEnv{t: t, set: set, c: c, url: srv.URL, rec: rec, used: used, got: func() []rawCapture {
		mu.Lock()
		defer mu.Unlock()
		return append([]rawCapture(nil), got...)
	}}
}

// okAnswers answers every request with a normal answer of its API.
func okAnswers(n int, path string, w http.ResponseWriter) {
	if strings.HasSuffix(path, "/v1/messages") {
		io.WriteString(w, anthropicOK(`{"ok": true}`))
		return
	}
	io.WriteString(w, okBody(`{"ok": true}`))
}

// provider creates a provider; baseURL "" = the test server.
func (e *thinkingEnv) provider(name, typ, baseURL, balance string) model.Provider {
	e.t.Helper()
	if baseURL == "" {
		baseURL = e.url
	}
	key := testKey
	p, err := e.set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &baseURL, APIKey: &key, BalanceAPI: &balance})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *thinkingEnv) last() rawCapture {
	e.t.Helper()
	caps := e.got()
	if len(caps) == 0 {
		e.t.Fatal("no request")
	}
	return caps[len(caps)-1]
}

// Every provider kind × level: the request bodies, byte for byte. The
// role's temperature is set and never sent with thinking on.
func TestThinkingRequestBodies(t *testing.T) {
	e := newThinkingEnv(t, okAnswers)
	dsHost := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone)
	dsBal := e.provider("DeepSeek proxy", model.ProviderOpenAI, "", model.BalanceDeepSeek)
	claude := e.provider("Claude", model.ProviderAnthropic, "", model.BalanceNone)
	dsAnth := e.provider("DeepSeek (Anthropic)", model.ProviderAnthropic, "https://api.deepseek.com/anthropic", model.BalanceNone)
	const (
		oaiMsgs  = `"messages":[{"role":"system","content":"SYS"},{"role":"user","content":"USR"}]`
		jsonFmt  = `"response_format":{"type":"json_object"},`
		anthSys  = `"system":[{"type":"text","text":"SYS","cache_control":{"type":"ephemeral"}}]`
		anthJSON = `"messages":[{"role":"user","content":"USR\n\nRespond with JSON only."}]`
		anthText = `"messages":[{"role":"user","content":"USR"}]`
	)
	for _, tc := range []struct {
		p           model.Provider
		model, role string
		level       string
		json        bool
		path, want  string
	}{
		// DeepSeek: the role's limit (8192 when it sends none) + the budget.
		{dsHost, "deepseek-flash", model.RoleRepair, "low", true, "/chat/completions",
			`{"model":"deepseek-flash",` + oaiMsgs + `,` + jsonFmt + `"stream":false,"thinking":{"type":"enabled"},"max_tokens":10240,"reasoning_effort":"low"}`},
		{dsHost, "deepseek-flash", model.RoleSummary, "high", false, "/chat/completions",
			`{"model":"deepseek-flash",` + oaiMsgs + `,"stream":false,"thinking":{"type":"enabled"},"max_tokens":16384,"reasoning_effort":"high"}`},
		{dsBal, "deepseek-v4-pro", model.RoleTranslate, "max", true, "/chat/completions",
			`{"model":"deepseek-v4-pro",` + oaiMsgs + `,` + jsonFmt + `"stream":false,"thinking":{"type":"enabled"},"max_tokens":32768,"reasoning_effort":"max"}`},
		{dsHost, "deepseek-v4-pro", model.RoleGlossaryJanitor, "high", true, "/chat/completions",
			`{"model":"deepseek-v4-pro",` + oaiMsgs + `,` + jsonFmt + `"stream":false,"thinking":{"type":"enabled"},"max_tokens":24192,"reasoning_effort":"high"}`},
		// Anthropic, current Claude model: adaptive thinking with the effort;
		// max_tokens = the role's base + the level's headroom (v3.2.x D1,
		// maxtokens.go). Streamed with a thinking summary (v3.2.x P1-c,
		// stream.go), so max holds its full 60000.
		{claude, "claude-opus-5-5", model.RoleTranslate, "low", true, "/v1/messages",
			`{"model":"claude-opus-5-5","max_tokens":14000,` + anthSys + `,` + anthJSON + `,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"low"},"stream":true}`},
		{claude, "claude-sonnet-5-5", model.RoleSummary, "high", false, "/v1/messages",
			`{"model":"claude-sonnet-5-5","max_tokens":28000,` + anthSys + `,` + anthText + `,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"},"stream":true}`},
		{claude, "claude-opus-5-5", model.RoleGlossaryJanitor, "max", true, "/v1/messages",
			`{"model":"claude-opus-5-5","max_tokens":76000,` + anthSys + `,` + anthJSON + `,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"max"},"stream":true}`},
		// Anthropic, older Claude model: budget_tokens.
		{claude, "claude-haiku-4-5", model.RoleRepair, "high", true, "/v1/messages",
			`{"model":"claude-haiku-4-5","max_tokens":12192,` + anthSys + `,` + anthJSON + `,"thinking":{"type":"enabled","budget_tokens":8192}}`},
		{claude, "claude-sonnet-4-5-20250929", model.RoleChapters, "low", true, "/v1/messages",
			`{"model":"claude-sonnet-4-5-20250929","max_tokens":6048,` + anthSys + `,` + anthJSON + `,"thinking":{"type":"enabled","budget_tokens":2048}}`},
		// Anthropic-compatible endpoint with a non-Claude model: budget_tokens.
		{dsAnth, "deepseek-flash", model.RoleRepair, "max", true, "/anthropic/v1/messages",
			`{"model":"deepseek-flash","max_tokens":28576,` + anthSys + `,` + anthJSON + `,"thinking":{"type":"enabled","budget_tokens":24576}}`},
	} {
		name := tc.p.Name + "/" + tc.model + "/" + tc.role + "/" + tc.level
		setRole(t, e.set, tc.role, tc.p.ID, tc.model, model.RoleParams{Temperature: f64(0.7), Thinking: sp(tc.level)})
		if _, err := e.c.Complete(context.Background(), tc.role, "SYS", "USR", tc.json); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := e.last(); got.path != tc.path || string(got.body) != tc.want {
			t.Errorf("%s:\n got  %s %s\n want %s %s", name, got.path, got.body, tc.path, tc.want)
		}
		// The same request from a lecture stage (its snapshot attached).
		snap, err := e.set.RoleTarget(tc.role)
		if err != nil || snap.Params.Thinking == nil || *snap.Params.Thinking != tc.level {
			t.Fatalf("%s: RoleTarget %+v %v", name, snap, err)
		}
		if _, err := e.c.Complete(WithTarget(context.Background(), snap), tc.role, "SYS", "USR", tc.json); err != nil {
			t.Fatal(err)
		}
		if got := e.last(); string(got.body) != tc.want {
			t.Errorf("%s (snapshot):\n got  %s\n want %s", name, got.body, tc.want)
		}
	}
}

// Legacy mode (config.toml providers): a snapshot's level applies to a
// DeepSeek host only.
func TestThinkingLegacyMode(t *testing.T) {
	srv, got := rawCaptureServer(t)
	snap := model.LLMSnapshot{ProviderID: "deepseek", Model: "deepseek-chat", Params: model.RoleParams{Temperature: f64(0.2), Thinking: sp("high")}}
	ds := New(testConfig(t, "https://api.deepseek.com"), nil, "", redirectClient(t, srv.URL))
	if _, err := ds.Complete(WithTarget(context.Background(), snap), model.RoleSummary, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	local := New(testConfig(t, srv.URL), nil, "", nil)
	if _, err := local.Complete(WithTarget(context.Background(), snap), model.RoleSummary, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	caps := got()
	msgs := `"messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}]`
	if want := `{"model":"deepseek-chat",` + msgs + `,"stream":false,"thinking":{"type":"enabled"},"max_tokens":16384,"reasoning_effort":"high"}`; string(caps[0].body) != want {
		t.Errorf("deepseek:\n got  %s\n want %s", caps[0].body, want)
	}
	if want := `{"model":"deepseek-chat",` + msgs + `,"temperature":0.2,"stream":false}`; string(caps[1].body) != want {
		t.Errorf("local (no thinking support):\n got  %s\n want %s", caps[1].body, want)
	}
}

// A truncated thinking answer is retried once with max_tokens doubled
// (appendix D2); nothing else changes (budget_tokens stays). Still
// truncated: permanent llm.truncated with the doubled limit.
func TestThinkingTruncationRetry(t *testing.T) {
	for _, tc := range []struct {
		name, typ, base, model string
		first, then            int
	}{
		{"DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", "deepseek-flash", 16384, 32768},
		{"Claude adaptive", model.ProviderAnthropic, "", "claude-opus-5-5", 30000, 60000}, // v3.2.x D1: repair 6000 + high 24000
		{"Claude budget", model.ProviderAnthropic, "", "claude-haiku-4-5", 12192, 24384},
	} {
		for _, twice := range []bool{false, true} {
			e := newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) {
				anth := strings.HasSuffix(path, "/v1/messages")
				switch {
				case (n == 1 || twice) && anth:
					io.WriteString(w, anthropicTruncated(`{"patches": [`))
				case n == 1 || twice:
					io.WriteString(w, lengthBody(`{"patches": [`))
				default:
					okAnswers(n, path, w)
				}
			})
			p := e.provider(tc.name, tc.typ, tc.base, model.BalanceNone)
			setRole(t, e.set, model.RoleRepair, p.ID, tc.model, model.RoleParams{Thinking: sp(model.ThinkingHigh)})
			_, err := e.c.Complete(context.Background(), model.RoleRepair, "SYS", "USR", true)
			caps := e.got()
			want := 2
			if twice {
				want = 3 // v3.2.x P1-c: two truncation retries; text was written, so each doubles
			}
			if len(caps) != want {
				t.Fatalf("%s twice=%v: %d requests", tc.name, twice, len(caps))
			}
			if twice {
				mt3 := fmt.Sprintf(`"max_tokens":%d`, 2*tc.then)
				if third := string(caps[2].body); strings.Replace(string(caps[0].body), fmt.Sprintf(`"max_tokens":%d`, tc.first), mt3, 1) != third {
					t.Errorf("%s: the third request is not the first with max_tokens %d:\n%s", tc.name, 2*tc.then, third)
				}
			}
			first, then := string(caps[0].body), string(caps[1].body)
			mtFirst, mtThen := fmt.Sprintf(`"max_tokens":%d`, tc.first), fmt.Sprintf(`"max_tokens":%d`, tc.then)
			if !strings.Contains(first, mtFirst) || !strings.Contains(then, mtThen) {
				t.Errorf("%s: requests\n%s\n%s\nwant %s then %s", tc.name, first, then, mtFirst, mtThen)
			}
			if strings.Replace(first, mtFirst, mtThen, 1) != then {
				t.Errorf("%s: the retry changes more than max_tokens:\n%s\n%s", tc.name, first, then)
			}
			if strings.Contains(then, "temperature") || !strings.Contains(then, `"thinking":`) {
				t.Errorf("%s: retry %s", tc.name, then)
			}
			if twice {
				if !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) || !isCode(err, CodeTruncated, "max_tokens", 2*tc.then, "role", model.RoleRepair) {
					t.Errorf("%s: truncated every time: %v %+v", tc.name, err, infoOf(err))
				}
			} else if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			// No backoff wait; every answer billed.
			if len(e.rec.waits) != 0 || len(e.used.all()) != want {
				t.Errorf("%s: waits %v usage %+v", tc.name, e.rec.waits, e.used.all())
			}
		}
	}
}

// DeepSeek's thinking answer carries reasoning_content next to content:
// only content is the result (reasoning_content is neither kept nor
// parsed); the reasoning tokens are part of completion_tokens and billed.
func TestDeepSeekReasoningContentIgnored(t *testing.T) {
	answer := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant",` +
		`"reasoning_content":"Let me think. The answer could be {\"patches\": [{\"idx\": 9}]}.",` +
		`"content":"{\"patches\": []}"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":900,"prompt_cache_hit_tokens":640,` +
		`"completion_tokens_details":{"reasoning_tokens":850}}}`
	e := newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) { io.WriteString(w, answer) })
	ds := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone)
	setRole(t, e.set, model.RoleRepair, ds.ID, "deepseek-flash", model.RoleParams{Thinking: sp(model.ThinkingLow)})
	res, err := e.c.Complete(context.Background(), model.RoleRepair, "SYS", "USR", true)
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Text: `{"patches": []}`, PromptTokens: 1000, CompletionTokens: 900, CacheHitTokens: 640}) {
		t.Errorf("result %+v", res)
	}
	if u := e.used.all(); len(u) != 1 || u[0].completion != 900 {
		t.Errorf("usage %+v", u)
	}
}

// captureLog sends slog's default logger to a buffer for the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A level the provider cannot take (a snapshot naming a provider without
// support, or a DeepSeek provider whose base_url was edited away) and a
// value that is not a level are sent as off — the request of a role
// without thinking — and logged once per provider, role and value.
func TestThinkingUnsupportedSentAsOff(t *testing.T) {
	logs := captureLog(t)
	e := newThinkingEnv(t, okAnswers)
	ctx := context.Background()
	other := e.provider("Other", model.ProviderOpenAI, "", model.BalanceNone)
	ds := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone)

	// The reference: the same role without thinking.
	setRole(t, e.set, model.RoleSummary, other.ID, "m", model.RoleParams{Temperature: f64(0.5)})
	if _, err := e.c.Complete(ctx, model.RoleSummary, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	offBody := string(e.last().body)
	if offBody != `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}],"temperature":0.5,"stream":false}` {
		t.Fatalf("reference body %s", offBody)
	}
	// 1) A snapshot target with a level, for a provider without support.
	snap := model.LLMSnapshot{ProviderID: other.ID, ProviderName: "Other", Model: "m", Params: model.RoleParams{Temperature: f64(0.5), Thinking: sp("high")}}
	for range 3 {
		if _, err := e.c.Complete(WithTarget(ctx, snap), model.RoleSummary, "S", "U", false); err != nil {
			t.Fatal(err)
		}
		if got := string(e.last().body); got != offBody {
			t.Errorf("unsupported target: %s", got)
		}
	}
	if n := strings.Count(logs.String(), "thinking sent as off"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, logs.String())
	}
	// 2) The provider stops being DeepSeek after the role turned thinking on.
	setRole(t, e.set, model.RoleSummary, ds.ID, "m", model.RoleParams{Temperature: f64(0.5), Thinking: sp("max")})
	if _, err := e.c.Complete(ctx, model.RoleSummary, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	if got := string(e.last().body); !strings.Contains(got, `"reasoning_effort":"max"`) {
		t.Fatalf("DeepSeek with thinking: %s", got)
	}
	u := e.url
	if _, err := e.set.UpdateProvider(ctx, ds.ID, settings.ProviderInput{BaseURL: &u}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Complete(ctx, model.RoleSummary, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	if got := string(e.last().body); got != offBody {
		t.Errorf("after the base_url edit: %s", got)
	}
	// 3) Not a level (a hand-edited row or snapshot): off, also on DeepSeek.
	ds2 := e.provider("DeepSeek 2", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone)
	bad := model.LLMSnapshot{ProviderID: ds2.ID, Model: "m", Params: model.RoleParams{Temperature: f64(0.5), Thinking: sp("medium")}}
	if _, err := e.c.Complete(WithTarget(ctx, bad), model.RoleSummary, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}],"temperature":0.5,"stream":false,"thinking":{"type":"disabled"}}`
	if got := string(e.last().body); got != want {
		t.Errorf("invalid level:\n got  %s\n want %s", got, want)
	}
	for _, s := range []string{"provider=Other", `provider="DeepSeek 2"`, "thinking=medium", "thinking=high", "thinking=max", "role=summary"} {
		if !strings.Contains(logs.String(), s) {
			t.Errorf("log lacks %q:\n%s", s, logs.String())
		}
	}
}

// With thinking on, one attempt may take longer than the plain timeout
// (ThinkingTimeoutPerToken per budget token): a slow answer that times out
// without thinking succeeds with it.
func TestThinkingTimeout(t *testing.T) {
	if got := thinkingTimeout(DefaultTimeout, ThinkingBudgetLow); got != DefaultTimeout+81920*time.Millisecond {
		t.Errorf("low: %v", got)
	}
	if got := thinkingTimeout(DefaultTimeout, ThinkingBudgetMax); got != DefaultTimeout+983040*time.Millisecond {
		t.Errorf("max: %v", got)
	}
	e := newThinkingEnv(t, func(n int, path string, w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond)
		okAnswers(n, path, w)
	})
	e.c.Timeout = 100 * time.Millisecond
	e.c.Backoff = nil // no transport retry
	ds := e.provider("DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone)
	setRole(t, e.set, model.RoleRepair, ds.ID, "deepseek-flash", model.RoleParams{})
	if _, err := e.c.Complete(context.Background(), model.RoleRepair, "S", "U", true); !isCode(err, CodeTimeout) {
		t.Fatalf("thinking off: %v, want a timeout", err)
	}
	setRole(t, e.set, model.RoleRepair, ds.ID, "deepseek-flash", model.RoleParams{Thinking: sp(model.ThinkingLow)})
	if _, err := e.c.Complete(context.Background(), model.RoleRepair, "S", "U", true); err != nil {
		t.Fatalf("thinking low: %v", err)
	}
}

func TestThinkingBudgets(t *testing.T) {
	for level, want := range map[string]int{"off": 0, "": 0, "low": 2048, "high": 8192, "max": 24576, "medium": 0, "HIGH": 0} {
		if got := ThinkingBudget(level); got != want {
			t.Errorf("ThinkingBudget(%q) = %d, want %d", level, got, want)
		}
	}
	if DeepSeekDefaultMaxTokens != 8192 {
		t.Errorf("DeepSeekDefaultMaxTokens = %d", DeepSeekDefaultMaxTokens)
	}
	// Every level of the model package has a budget, except off.
	for _, l := range model.ThinkingLevels {
		if (ThinkingBudget(l) > 0) != (l != model.ThinkingOff) {
			t.Errorf("level %q: budget %d", l, ThinkingBudget(l))
		}
	}
}

// The Anthropic thinking form per model (anthropicLegacyModels): every
// class of model id.
func TestAnthropicAdaptiveModels(t *testing.T) {
	for _, tc := range []struct {
		class    string
		adaptive bool
		ids      []string
	}{
		{"current Claude: adaptive + effort", true, []string{
			"claude-opus-5-5", "claude-opus-5", "claude-sonnet-5-5", "claude-sonnet-5", "claude-haiku-5-5",
			"claude-fable-5-1", "claude-fable-5", "claude-mythos-5-1",
			"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-4-6",
			"claude-opus-4-6@20260101", " Claude-Opus-5-5 ", "claude-future-9",
		}},
		{"Claude 4.5 family and Opus 4.1: budget_tokens", false, []string{
			"claude-haiku-4-5", "claude-haiku-4-5-20251001", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929",
			"claude-opus-4-5", "claude-opus-4-5-20251101", "claude-opus-4-5@20251101", "claude-opus-4-1", "claude-opus-4-1-20250805",
		}},
		{"Claude 4 (Opus 4, Sonnet 4): budget_tokens", false, []string{
			"claude-opus-4-0", "claude-opus-4-20250514", "claude-sonnet-4-0", "claude-sonnet-4-20250514",
		}},
		{"Claude 3 and older: budget_tokens", false, []string{
			"claude-3-7-sonnet-20250219", "claude-3-7-sonnet-latest", "claude-3-5-haiku-latest", "claude-3-opus-20240229",
			"claude-3-haiku-20240307", "claude-2", "claude-2.1", "claude-instant-1",
		}},
		{"not a Claude id (Anthropic-compatible endpoints): budget_tokens", false, []string{
			"deepseek-flash", "deepseek-v4-pro", "kimi-k2", "glm-4.6", "claude", "", "opus-5-5",
		}},
	} {
		for _, id := range tc.ids {
			if got := anthropicAdaptive(id); got != tc.adaptive {
				t.Errorf("%s: anthropicAdaptive(%q) = %v", tc.class, id, got)
			}
			th, oc := anthropicThinkingFields(id, model.ThinkingHigh)
			if tc.adaptive && (th.Type != "adaptive" || th.BudgetTokens != 0 || oc == nil || oc.Effort != "high") ||
				!tc.adaptive && (th.Type != "enabled" || th.BudgetTokens != ThinkingBudgetHigh || oc != nil) {
				t.Errorf("%s: fields of %q: %+v %+v", tc.class, id, th, oc)
			}
			// Off: no field, except the models that think without one
			// (v3.2.x P1, Q4a; C91; TestAnthropicRequestThinkingByModelAndLevel).
			th, oc = anthropicThinkingFields(id, model.ThinkingOff)
			switch norm := strings.ToLower(strings.TrimSpace(id)); {
			case strings.HasPrefix(norm, "claude-sonnet-5-5"):
				if th == nil || *th != (anthropicThinking{Type: "between_tools"}) || oc != nil {
					t.Errorf("%q off: %+v %+v", id, th, oc)
				}
			case strings.HasPrefix(norm, "claude-opus-5-5"), strings.HasPrefix(norm, "claude-fable-"), strings.HasPrefix(norm, "claude-mythos-"):
				if th != nil || oc == nil || oc.Effort != "low" || oc.Format != nil {
					t.Errorf("%q off: %+v %+v", id, th, oc)
				}
			case norm == "claude-opus-5", norm == "claude-sonnet-5", norm == "claude-haiku-5-5":
				if th == nil || *th != (anthropicThinking{Type: "disabled"}) || oc != nil {
					t.Errorf("%q off: %+v %+v", id, th, oc)
				}
			default:
				if th != nil || oc != nil {
					t.Errorf("%q off: %+v %+v", id, th, oc)
				}
			}
		}
	}
}
