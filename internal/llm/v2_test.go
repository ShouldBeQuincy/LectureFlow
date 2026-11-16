// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// v2 (SPEC-v2 appendix D2): DeepSeek thinking off, Anthropic max_tokens per
// role and the retry of a truncated answer.

// redirectTransport sends every request to the test server, whatever host
// the URL names (lets a provider keep its real DeepSeek base_url offline).
type redirectTransport struct{ to *url.URL }

func (t redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.to.Scheme, t.to.Host
	r.Host = t.to.Host
	return http.DefaultTransport.RoundTrip(r)
}

func redirectClient(t *testing.T, srvURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: redirectTransport{to: u}}
}

// Settings mode: DeepSeek (by host or by balance API) gets
// "thinking": {"type": "disabled"} on every role; other providers get no
// thinking field at all.
func TestDeepSeekThinkingDisabledSettings(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody(`{"ok": true}`)) })
	set := newSettings(t)
	ctx := context.Background()

	create := func(name, baseURL, balance string) model.Provider {
		t.Helper()
		typ, key := model.ProviderOpenAI, testKey
		p, err := set.CreateProvider(ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &baseURL, APIKey: &key, BalanceAPI: &balance})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Real DeepSeek base_url (redirected to the test server), recognised by
	// its host alone.
	byHost := create("DeepSeek", "https://api.deepseek.com", model.BalanceNone)
	// A proxy in front of DeepSeek, recognised by its balance API.
	byBalance := create("Proxy", srv.URL, model.BalanceDeepSeek)
	other := addProvider(t, set, "Other", model.ProviderOpenAI, srv.URL, testKey)
	if other.BalanceAPI != model.BalanceNone {
		t.Fatalf("other provider balance api %q", other.BalanceAPI)
	}
	c := NewWithSettings(set, nil, nil, "", redirectClient(t, srv.URL))
	c.Sleep = (&recorder{}).sleep

	cases := []struct {
		p        model.Provider
		role     string
		json     bool
		thinking bool
	}{
		{byHost, model.RoleRepair, true, true},
		{byHost, model.RoleSummary, false, true},
		{byHost, model.RoleGlossaryJanitor, true, true},
		{byBalance, model.RoleTranslate, true, true},
		{other, model.RoleTranslate, true, false},
		{other, model.RoleSummary, false, false},
	}
	for i, tc := range cases {
		setRole(t, set, tc.role, tc.p.ID, "deepseek-flash", model.RoleParams{})
		if _, err := c.Complete(ctx, tc.role, "SYS", "USR", tc.json); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		b := got()[i].body
		th, has := b["thinking"]
		if has != tc.thinking {
			t.Errorf("case %d (%s, %s): thinking present = %v, want %v (body %v)", i, tc.p.Name, tc.role, has, tc.thinking, b)
		}
		if has {
			if raw, _ := json.Marshal(th); string(raw) != `{"type":"disabled"}` {
				t.Errorf("case %d: thinking = %s", i, raw)
			}
		}
		// The switch changes nothing else: JSON mode and temperature stay.
		if _, rf := b["response_format"]; rf != tc.json || b["model"] != "deepseek-flash" || b["temperature"] == nil {
			t.Errorf("case %d: body %v", i, b)
		}
	}
}

// Legacy mode (config.toml providers): the base_url host decides.
func TestDeepSeekThinkingDisabledLegacy(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody(`{"ok": true}`)) })
	cfg := testConfig(t, "https://api.deepseek.com")
	c := New(cfg, nil, "", redirectClient(t, srv.URL))
	if _, err := c.Complete(context.Background(), model.RoleTranslate, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(got()[0].body["thinking"]); string(raw) != `{"type":"disabled"}` {
		t.Errorf("deepseek host: thinking = %s", raw)
	}

	local, _, _ := newClient(t, srv.URL) // 127.0.0.1: not DeepSeek
	if _, err := local.Complete(context.Background(), model.RoleTranslate, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	if _, has := got()[1].body["thinking"]; has {
		t.Errorf("non-DeepSeek provider got a thinking field: %v", got()[1].body)
	}
}

// The Anthropic adapter never sends a thinking switch, not even to
// DeepSeek's Anthropic-compatible endpoint (no reliable documentation).
func TestAnthropicNoThinkingSwitch(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, anthropicOK(`{"ok": true}`)) })
	set := newSettings(t)
	p := addProvider(t, set, "DeepSeek (Anthropic)", model.ProviderAnthropic, "https://api.deepseek.com/anthropic", testKey)
	setRole(t, set, model.RoleRepair, p.ID, "deepseek-flash", model.RoleParams{})
	c := NewWithSettings(set, nil, nil, "", redirectClient(t, srv.URL))
	if _, err := c.Complete(context.Background(), model.RoleRepair, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	b := got()[0].body
	for _, k := range []string{"thinking", "reasoning", "output_config", "reasoning_effort"} {
		if _, has := b[k]; has {
			t.Errorf("unexpected field %q in %v", k, b)
		}
	}
	if got()[0].path != "/anthropic/v1/messages" {
		t.Errorf("path %q", got()[0].path)
	}
}

// "测试连接" sends the same switch as the roles.
func TestConnectionThinkingSwitch(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody("p")) })
	ds := model.Provider{Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: srv.URL, APIKey: testKey, BalanceAPI: model.BalanceDeepSeek}
	if r := TestConnection(context.Background(), ds, "deepseek-flash"); !r.OK {
		t.Fatalf("test connection: %+v", r)
	}
	other := ds
	other.BalanceAPI = model.BalanceNone
	if r := TestConnection(context.Background(), other, "some-model"); !r.OK {
		t.Fatalf("test connection: %+v", r)
	}
	caps := got()
	if raw, _ := json.Marshal(caps[0].body["thinking"]); string(raw) != `{"type":"disabled"}` || caps[0].body["max_tokens"] != float64(1) {
		t.Errorf("deepseek ping: %v", caps[0].body)
	}
	if _, has := caps[1].body["thinking"]; has {
		t.Errorf("other ping has thinking: %v", caps[1].body)
	}
}

func TestAnthropicMaxTokensPerRole(t *testing.T) {
	want := map[string]int{
		model.RoleSyllabusFormat:  4000,
		model.RoleGlossary:        4000,
		model.RoleGlossaryJanitor: 16000,
		model.RoleSummary:         4000,
		model.RoleRepair:          4000,
		model.RoleTranslate:       4000,
		model.RoleChapters:        4000,
		model.RoleOrganizer:       4000,
		"unknown":                 4000,
	}
	for role, n := range want {
		if got := AnthropicMaxTokensFor(role); got != n {
			t.Errorf("AnthropicMaxTokensFor(%s) = %d, want %d", role, got, n)
		}
	}

	// The table above is what an older Claude model gets; a current one
	// (anthropicFixtureModel: adaptive thinking) gets the bases of v3.2.x D1
	// (maxtokens.go, BaseMaxTokens).
	current := map[string]int{
		model.RoleSyllabusFormat:  16000,
		model.RoleGlossary:        8000,
		model.RoleGlossaryJanitor: 16000,
		model.RoleSummary:         4000,
		model.RoleRepair:          6000,
		model.RoleTranslate:       2000,
		model.RoleChapters:        4000,
		model.RoleOrganizer:       4000,
	}
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, anthropicOK(`{"ok": true}`)) })
	c, set, p, _, _, _ := anthropicClient(t, srv.URL)
	for _, role := range model.Roles {
		if _, err := c.Complete(context.Background(), role, "SYS", "USR", true); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
	}
	for i, role := range model.Roles {
		if mt := got()[i].body["max_tokens"]; mt != float64(current[role]) || current[role] != BaseMaxTokens(role, 0) {
			t.Errorf("%s: max_tokens %v, want %d", role, mt, current[role])
		}
	}
	for _, role := range model.Roles {
		setRole(t, set, role, p.ID, "claude-haiku-4-5", model.RoleParams{})
		if _, err := c.Complete(context.Background(), role, "SYS", "USR", true); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
	}
	for i, role := range model.Roles {
		if mt := got()[len(model.Roles)+i].body["max_tokens"]; mt != float64(want[role]) {
			t.Errorf("older model, %s: max_tokens %v, want %d", role, mt, want[role])
		}
	}

	// A positive HTTPClient.AnthropicMaxTokens overrides every role.
	c.AnthropicMaxTokens = 123
	if _, err := c.Complete(context.Background(), model.RoleGlossaryJanitor, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	if mt := got()[2*len(model.Roles)].body["max_tokens"]; mt != float64(123) {
		t.Errorf("override: max_tokens %v", mt)
	}
}

// anthropicTruncated is a 200 answer cut off at max_tokens.
func anthropicTruncated(text string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_t", "type": "message", "role": "assistant", "stop_reason": "max_tokens",
		"content": []any{map[string]any{"type": "thinking", "thinking": ""}, map[string]any{"type": "text", "text": text}},
		"usage":   map[string]any{"input_tokens": 100, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 4000},
	})
	return string(b)
}

func TestAnthropicTruncationRetriedWithDoubledLimit(t *testing.T) {
	for _, tc := range []struct {
		role        string
		first, then float64
	}{
		{model.RoleRepair, 6000, 12000}, // v3.2.x D1: anthropicFixtureModel is a current Claude model
		{model.RoleGlossaryJanitor, 16000, 32000},
	} {
		srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
			if n == 1 {
				io.WriteString(w, anthropicTruncated(`{"patches": [{"idx": 1, "af`))
				return
			}
			io.WriteString(w, anthropicOK(`{"ok": true}`))
		})
		c, _, p, pool, rec, usage := anthropicClient(t, srv.URL)
		tot := &Usage{}
		ctx := WithUsage(context.Background(), tot)
		res, err := c.Complete(ctx, tc.role, "SYS", "USR", true)
		if err != nil {
			t.Fatalf("%s: %v", tc.role, err)
		}
		if res.Text != `{"ok": true}` {
			t.Errorf("%s: text %q", tc.role, res.Text)
		}
		caps := got()
		if len(caps) != 2 || caps[0].body["max_tokens"] != tc.first || caps[1].body["max_tokens"] != tc.then {
			t.Fatalf("%s: %d requests, max_tokens %v then %v", tc.role, len(caps), caps[0].body["max_tokens"], caps[len(caps)-1].body["max_tokens"])
		}
		// Only max_tokens changes between the two requests.
		a, b := caps[0].body, caps[1].body
		delete(a, "max_tokens")
		delete(b, "max_tokens")
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Errorf("%s: retried body differs:\n%s\n%s", tc.role, ja, jb)
		}
		if len(rec.waits) != 0 {
			t.Errorf("%s: truncation retry waited %v", tc.role, rec.waits)
		}
		if acq, rel := pool.counts(); acq != 2 || rel != 2 {
			t.Errorf("%s: pool acquired %d released %d", tc.role, acq, rel)
		}
		// The truncated answer was billed: both responses are counted.
		u := usage.all()
		if len(u) != 2 || u[0] != (usageCall{provider: p.ID, prompt: 100, completion: 4000}) ||
			u[1] != (usageCall{provider: p.ID, prompt: 450, hit: 300, completion: 20}) {
			t.Errorf("%s: usage %+v", tc.role, u)
		}
		if tt := tot.Totals(); tt.Requests != 2 || tt.CompletionTokens != 4020 {
			t.Errorf("%s: totals %+v", tc.role, tt)
		}
	}
}

func TestAnthropicTruncatedTwiceFails(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, anthropicTruncated(`{"zh": "缓存`)) })
	c, _, _, _, rec, usage := anthropicClient(t, srv.URL)
	_, err := c.Complete(context.Background(), model.RoleTranslate, "SYS", "USR", true)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) {
		t.Errorf("error %v: want ErrTruncated wrapped permanent", err)
	}
	// v3.2.x D1: translate on a current Claude model starts at 2000; P1-c:
	// two truncation retries, each doubling (the answer has text).
	if !isCode(err, CodeTruncated, "role", model.RoleTranslate, "max_tokens", 8000, "model", anthropicFixtureModel, "provider", "Claude",
		"tried", "off/2000, off/4000, off/8000") ||
		!strings.Contains(infoOf(err).Detail, "从 2000 翻倍到 8000") {
		t.Errorf("error %v: %+v", err, infoOf(err))
	}
	caps := got()
	if len(caps) != 3 || caps[0].body["max_tokens"] != float64(2000) || caps[1].body["max_tokens"] != float64(4000) ||
		caps[2].body["max_tokens"] != float64(8000) {
		t.Fatalf("%d requests: %v", len(caps), caps)
	}
	if len(rec.waits) != 0 {
		t.Errorf("waited %v", rec.waits)
	}
	if len(usage.all()) != 3 {
		t.Errorf("usage %+v", usage.all())
	}
}

// A transport failure after a truncation keeps the doubled limit and uses
// the §12 backoff (the truncation retry does not consume it).
func TestAnthropicTruncationThenServerError(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) {
		switch n {
		case 1:
			io.WriteString(w, anthropicTruncated("{"))
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
		default:
			io.WriteString(w, anthropicOK(`{"ok": true}`))
		}
	})
	c, _, _, _, rec, _ := anthropicClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), model.RoleSummary, "SYS", "USR", false); err != nil {
		t.Fatal(err)
	}
	caps := got()
	if len(caps) != 3 || caps[1].body["max_tokens"] != float64(8000) || caps[2].body["max_tokens"] != float64(8000) {
		t.Fatalf("%d requests: %v", len(caps), caps)
	}
	if len(rec.waits) != 1 || rec.waits[0] != DefaultBackoff[0] {
		t.Errorf("waits %v, want [%v]", rec.waits, DefaultBackoff[0])
	}
}

// ---------- OpenAI-compatible finish_reason "length" (lead, after the real acceptance) ----------

// lengthBody is an OpenAI-compatible answer cut off at the output limit.
func lengthBody(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "length"}},
		"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 8192, "prompt_cache_hit_tokens": 0},
	})
	return string(b)
}

// Without max_tokens in the request the limit is the server's default,
// which the client cannot raise: permanent at once, no retry of any kind.
func TestOpenAILengthWithoutMaxTokensFails(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, lengthBody(`{"actions": [{"id": "t1", "act`)) })
	set := newSettings(t)
	// DeepSeek recognised by its balance API: thinking off, but only the
	// janitor role sends max_tokens.
	name, typ, u, key, bal := "DeepSeek", model.ProviderOpenAI, srv.URL, testKey, model.BalanceDeepSeek
	ds, err := set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &u, APIKey: &key, BalanceAPI: &bal})
	if err != nil {
		t.Fatal(err)
	}
	other := addProvider(t, set, "Other", model.ProviderOpenAI, srv.URL, testKey)
	setRole(t, set, model.RoleRepair, ds.ID, "deepseek-flash", model.RoleParams{})
	setRole(t, set, model.RoleGlossaryJanitor, other.ID, "some-model", model.RoleParams{})
	rec, usage := &recorder{}, &fakeRecorder{}
	c := NewWithSettings(set, nil, usage, "", nil)
	c.Sleep = rec.sleep

	for i, role := range []string{model.RoleRepair, model.RoleGlossaryJanitor} {
		tot := &Usage{}
		_, err := c.Complete(WithUsage(context.Background(), tot), role, "SYS", "USR", true)
		if err == nil {
			t.Fatalf("%s: expected an error", role)
		}
		if !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) {
			t.Errorf("%s: error %v: want ErrTruncated wrapped permanent", role, err)
		}
		// max_tokens 0: the server's default output limit; no retries.
		if !isCode(err, CodeTruncated, "role", role, "max_tokens", 0, "retries", 0) {
			t.Errorf("%s: error %v: %+v", role, err, infoOf(err))
		}
		if n := len(got()); n != i+1 {
			t.Fatalf("%s: %d requests in total, want %d (no retry)", role, n, i+1)
		}
		if _, has := got()[i].body["max_tokens"]; has {
			t.Errorf("%s: request has max_tokens: %v", role, got()[i].body)
		}
		// The truncated answer was billed.
		if tt := tot.Totals(); tt.Requests != 1 || tt.CompletionTokens != 8192 || tt.PromptTokens != 1000 {
			t.Errorf("%s: totals %+v", role, tt)
		}
	}
	if len(rec.waits) != 0 {
		t.Errorf("waited %v", rec.waits)
	}
	if u := usage.all(); len(u) != 2 || u[0].completion != 8192 {
		t.Errorf("usage %+v", u)
	}
}

// The janitor role on DeepSeek sends max_tokens 16000 (DeepSeek's default
// is 8K): a truncated answer is retried with 32000, then 64000 (v3.2.x
// P1-c), like Anthropic.
func TestDeepSeekJanitorMaxTokens(t *testing.T) {
	if openAIMaxTokens(model.RoleGlossaryJanitor, true) != 16000 || openAIMaxTokens(model.RoleGlossaryJanitor, false) != 0 ||
		openAIMaxTokens(model.RoleRepair, true) != 0 || openAIMaxTokens(model.RoleTranslate, true) != 0 {
		t.Fatal("openAIMaxTokens")
	}
	for _, tc := range []struct {
		name    string
		answers []string // per request
		ok      bool
	}{
		{"retry succeeds", []string{lengthBody(`{"act`), okBody(`{"actions": []}`)}, true},
		// v3.2.x P1-c: two truncation retries (16000, 32000, 64000)
		{"truncated three times", []string{lengthBody(`{"act`), lengthBody(`{"actions": [`), lengthBody(`{"actions": [{`)}, false},
	} {
		srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, tc.answers[n-1]) })
		set := newSettings(t)
		ds := addProvider(t, set, "DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", testKey)
		setRole(t, set, model.RoleGlossaryJanitor, ds.ID, "deepseek-v4-pro", model.RoleParams{})
		rec, usage := &recorder{}, &fakeRecorder{}
		c := NewWithSettings(set, nil, usage, "", redirectClient(t, srv.URL))
		c.Sleep = rec.sleep
		res, err := c.Complete(context.Background(), model.RoleGlossaryJanitor, "SYS", "USR", true)
		caps := got()
		if len(caps) != len(tc.answers) || caps[0].body["max_tokens"] != float64(16000) || caps[1].body["max_tokens"] != float64(32000) ||
			!tc.ok && caps[2].body["max_tokens"] != float64(64000) {
			t.Fatalf("%s: %d requests: %v", tc.name, len(caps), caps)
		}
		if raw, _ := json.Marshal(caps[1].body["thinking"]); string(raw) != `{"type":"disabled"}` {
			t.Errorf("%s: retried request lost the thinking switch: %v", tc.name, caps[1].body)
		}
		if tc.ok {
			if err != nil || res.Text != `{"actions": []}` {
				t.Errorf("%s: %q %v", tc.name, res.Text, err)
			}
		} else {
			if !errors.Is(err, ErrTruncated) || !sched.IsPermanent(err) || !isCode(err, CodeTruncated, "max_tokens", 64000) ||
				!strings.Contains(infoOf(err).Detail, "16000") {
				t.Errorf("%s: error %v", tc.name, err)
			}
		}
		if len(rec.waits) != 0 || len(usage.all()) != len(tc.answers) {
			t.Errorf("%s: waits %v usage %+v", tc.name, rec.waits, usage.all())
		}
	}
}

// finish_reason "stop" (and any other value) is a normal answer.
func TestOpenAIStopIsNotTruncated(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody(`{"ok": true}`)) })
	c, _, _ := newClient(t, srv.URL)
	if res, err := c.Complete(context.Background(), model.RoleTranslate, "SYS", "USR", true); err != nil || res.Text != `{"ok": true}` {
		t.Fatalf("%q %v", res.Text, err)
	}
	if _, has := got()[0].body["max_tokens"]; has {
		t.Errorf("max_tokens sent: %v", got()[0].body)
	}
}
