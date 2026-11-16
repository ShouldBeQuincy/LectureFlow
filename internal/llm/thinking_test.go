// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Per-role "深度思考" (.claude/thinking-brief.md, T1).

type rawCapture struct {
	path string
	body []byte
}

// rawCaptureServer records every request body byte for byte and answers
// with a normal answer of the API the path names.
func rawCaptureServer(t *testing.T) (*httptest.Server, func() []rawCapture) {
	t.Helper()
	var mu sync.Mutex
	var got []rawCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, rawCapture{r.URL.Path, b})
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/v1/messages") {
			io.WriteString(w, anthropicOK(`{"ok": true}`))
			return
		}
		io.WriteString(w, okBody(`{"ok": true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []rawCapture {
		mu.Lock()
		defer mu.Unlock()
		return append([]rawCapture(nil), got...)
	}
}

// offProvider is one provider of the thinking-off golden test.
type offProvider struct {
	name, typ, baseURL, balance, model string
}

func offProviders(srvURL string) []offProvider {
	return []offProvider{
		{"deepseek-host", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceNone, "deepseek-flash"},
		{"deepseek-balance", model.ProviderOpenAI, srvURL, model.BalanceDeepSeek, "deepseek-v4-pro"},
		{"openai-other", model.ProviderOpenAI, srvURL, model.BalanceNone, "gpt-test"},
		{"anthropic-current", model.ProviderAnthropic, srvURL, model.BalanceNone, "claude-opus-5-5"},
		{"anthropic-legacy", model.ProviderAnthropic, srvURL, model.BalanceNone, "claude-haiku-4-5"},
		{"anthropic-deepseek", model.ProviderAnthropic, "https://api.deepseek.com/anthropic", model.BalanceNone, "deepseek-flash"},
	}
}

// offParams are the params variants of the golden test: base is what the
// role stores; thinking is applied on top (nil = the field is absent).
func offParams(thinking *string) []model.RoleParams {
	return []model.RoleParams{
		{Thinking: thinking},
		{Temperature: f64(0.7), Window: ip(2), Thinking: thinking},
	}
}

func ip(n int) *int { return &n }

// offBodies sends every request kind of the app (each provider kind × each
// role × params × JSON mode, snapshot targets, legacy mode) with thinking
// set to thinking (nil or "off") and returns one golden line per request.
func offBodies(t *testing.T, thinking *string) []string {
	t.Helper()
	srv, got := rawCaptureServer(t)
	set := newSettings(t)
	ctx := context.Background()
	c := NewWithSettings(set, nil, nil, "", redirectClient(t, srv.URL))
	c.Sleep = (&recorder{}).sleep
	var names []string
	for _, op := range offProviders(srv.URL) {
		name, typ, u, key, bal := op.name, op.typ, op.baseURL, testKey, op.balance
		p, err := set.CreateProvider(ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &u, APIKey: &key, BalanceAPI: &bal})
		if err != nil {
			t.Fatal(err)
		}
		for pi, params := range offParams(thinking) {
			for _, role := range model.Roles {
				setRole(t, set, role, p.ID, op.model, params)
				for _, jsonMode := range []bool{true, false} {
					if _, err := c.Complete(ctx, role, "SYS PREFIX", "USER PART", jsonMode); err != nil {
						t.Fatalf("%s %s: %v", op.name, role, err)
					}
					names = append(names, fmt.Sprintf("settings/%s/params%d/%s/json=%v", op.name, pi, role, jsonMode))
				}
			}
			// A lecture stage: the job's snapshot attached with WithTarget.
			snap := model.LLMSnapshot{ProviderID: p.ID, ProviderName: op.name, Model: op.model,
				Params: settings.ResolveParams(model.RoleTranslate, params)}
			if _, err := c.Complete(WithTarget(ctx, snap), model.RoleTranslate, "SYS PREFIX", "USER PART", true); err != nil {
				t.Fatalf("%s target: %v", op.name, err)
			}
			names = append(names, fmt.Sprintf("target/%s/params%d/translate", op.name, pi))
			// A hand-written snapshot with the raw (unresolved) params.
			snap.Params = params
			if _, err := c.Complete(WithTarget(ctx, snap), model.RoleRepair, "SYS PREFIX", "USER PART", true); err != nil {
				t.Fatalf("%s raw target: %v", op.name, err)
			}
			names = append(names, fmt.Sprintf("target-raw/%s/params%d/repair", op.name, pi))
		}
	}
	// Legacy mode (config.toml providers): DeepSeek by host, and a local server.
	for _, lc := range []struct{ name, url string }{{"legacy-deepseek", "https://api.deepseek.com"}, {"legacy-local", srv.URL}} {
		lcl := New(testConfig(t, lc.url), nil, "", redirectClient(t, srv.URL))
		lcl.Sleep = (&recorder{}).sleep
		for _, role := range model.Roles {
			if _, err := lcl.Complete(ctx, role, "SYS PREFIX", "USER PART", true); err != nil {
				continue // roles the legacy config does not know: no request
			}
			names = append(names, fmt.Sprintf("%s/%s", lc.name, role))
		}
		for pi, params := range offParams(thinking) {
			snap := model.LLMSnapshot{ProviderID: "deepseek", Model: "deepseek-chat", Params: params}
			if _, err := lcl.Complete(WithTarget(ctx, snap), model.RoleSummary, "SYS PREFIX", "USER PART", false); err != nil {
				t.Fatalf("%s target: %v", lc.name, err)
			}
			names = append(names, fmt.Sprintf("%s/target/params%d/summary", lc.name, pi))
		}
	}
	caps := got()
	if len(caps) != len(names) {
		t.Fatalf("%d requests for %d cases", len(caps), len(names))
	}
	lines := make([]string, len(caps))
	for i, cp := range caps {
		if bytes.ContainsAny(cp.body, "\n\r") {
			t.Fatalf("%s: body has a line break", names[i])
		}
		lines[i] = names[i] + "\t" + cp.path + "\t" + string(cp.body)
	}
	return lines
}

// With thinking off (absent, "off" or "") every request body is byte for
// byte what the client sent before the thinking option existed. The golden
// file was written from offBodies on the unchanged client of commit 064f675
// (the commit before the option; 230 requests, sha256 d9f9a8c9…19f0) and is
// not regenerated: a difference is a regression, never a golden update.
//
// Amendments the user approved (NOTES.md, 用户决定 v3.2.x), each made by
// editing only the lines it concerns, the others staying byte for byte:
//   - v3.2.x P1 (Q4a): the 36 requests of anthropic-current (claude-opus-5-5,
//     whose thinking runs without a thinking field) end with
//     "output_config":{"effort":"low"} (sha256 f6a6aeb7…98ad).
//   - v3.2.x D1 (Q6a): the same 36 requests have the max_tokens of a
//     current Claude model: the role's base + 4000 of thinking headroom
//     for claude-opus-5-5's level off (maxtokens.go; sha256 6b9cea08…3ea6).
//     DeepSeek and older Claude models are unchanged.
//   - v3.2.x P1-c (Q7a): the same 36 requests are streamed and end with
//     "stream":true (stream.go: current Claude models only). Their thinking
//     fields are unchanged (claude-opus-5-5's off sends no thinking field,
//     so no display either; sha256 f1f1c1e7…462e).
//   - C91 (round 2, real acceptance T9; approved by the user 2026-11-11):
//     the same 36 requests hold 12000 instead of 4000 tokens of thinking
//     headroom (max_tokens + 8000): claude-opus-5-5's thinking cannot be
//     switched off and at effort low thought 9,965 tokens on one repair
//     chunk (maxtokens.go, ThinkingHeadroomOffAlwaysThinking). Their
//     thinking fields are unchanged. The same headroom for the level low of
//     these models (also approved by the user 2026-11-11) does not reach
//     this file: it holds the level off only.
//
// claude-sonnet-5-5 is not among the golden providers; its level off
// ({"type":"between_tools"}) is tested in llmtest (p1_test.go), as is
// {"type":"disabled"} of claude-opus-5, claude-sonnet-5 and claude-haiku-5-5
// (C91; also TestAnthropicRequestThinkingByModelAndLevel).
func TestThinkingOffBodiesUnchanged(t *testing.T) {
	golden := filepath.Join("testdata", "thinking_off_bodies.golden")
	want := offBodies(t, nil)
	b, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	gl := strings.Split(strings.TrimRight(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n"), "\n")
	off, empty := model.ThinkingOff, ""
	for _, variant := range []struct {
		name  string
		lines []string
	}{{"absent", want}, {"off", offBodies(t, &off)}, {"empty", offBodies(t, &empty)}} {
		if len(variant.lines) != len(gl) {
			t.Fatalf("%s: %d requests, golden has %d", variant.name, len(variant.lines), len(gl))
		}
		for i := range gl {
			if variant.lines[i] != gl[i] {
				t.Errorf("%s: request %d differs:\n got  %s\n want %s", variant.name, i, variant.lines[i], gl[i])
			}
		}
	}
}
