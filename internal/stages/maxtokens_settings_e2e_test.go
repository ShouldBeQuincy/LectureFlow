// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// maxTokensServer stands in for a provider: it records the body of every
// request and refuses it (HTTP 401), so a stage run makes its first request
// and fails. Only that first request matters here (backlog T3: the doubled
// limit after a truncation is another test's business).
type maxTokensServer struct {
	mu     sync.Mutex
	bodies [][]byte
	srv    *httptest.Server
}

func newMaxTokensServer(t *testing.T) *maxTokensServer {
	t.Helper()
	m := &maxTokensServer{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		m.mu.Lock()
		if len(m.bodies) < 100 {
			m.bodies = append(m.bodies, b)
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// first returns max_tokens of the first request (absent = 0) and whether
// the field was sent.
func (m *maxTokensServer) first(t *testing.T) (int, bool) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		t.Fatal("no request reached the provider")
	}
	var b struct {
		MaxTokens *int `json:"max_tokens"`
	}
	if err := json.Unmarshal(m.bodies[0], &b); err != nil {
		t.Fatalf("body %s: %v", m.bodies[0], err)
	}
	if b.MaxTokens == nil {
		return 0, false
	}
	return *b.MaxTokens, true
}

// TestMaxTokensFromSetRoleToRequest is the end-to-end path of a role's
// max_tokens (review R1 suggestion 5 / F1, backlog T3): the settings entry
// (settings.Service.SetRole, what PUT /api/settings/roles calls) → the
// snapshot launch writes (CurrentSnapshots → the job's config_snapshot) →
// the stage's run (llmTarget → llm.WithTarget) → llm.HTTPClient in settings
// mode → max_tokens in the body of the first request to the provider.
//
// The expected values follow the rules of internal/llm/maxtokens.go
// (outputLimit; v3.2.x D1, NOTES.md "max_tokens" rulings) and are written
// out as numbers on purpose:
//
//   - the user's value is the whole limit, thinking included, on every
//     provider type;
//   - except on an older Claude model (Anthropic, not adaptive) whose
//     thinking budget is not below it: the budget is added (the API refuses
//     budget_tokens >= max_tokens);
//   - a current Claude model sends at most its output limit (128000);
//   - without a value: current Claude = role base + thinking headroom
//     (summary 4000; high +24000), OpenAI-compatible = no field, DeepSeek
//     with thinking on = its default 8192 + the level's budget (high 8192).
func TestMaxTokensFromSetRoleToRequest(t *testing.T) {
	const (
		claude   = "claude"   // Anthropic type
		openai   = "openai"   // OpenAI-compatible, local, not DeepSeek
		deepseek = "deepseek" // OpenAI-compatible with balance_api deepseek
	)
	for _, tc := range []struct {
		name     string
		provider string
		model    string
		stage    string
		thinking string
		mt       *int
		want     int // 0 = no max_tokens field
	}{
		// Automatic, for contrast: summary base 4000 + high headroom 24000.
		{"claude current, high, automatic", claude, "claude-sonnet-5-5", model.StageSummary, model.ThinkingHigh, nil, 28000},
		// The user's value replaces base + headroom.
		{"claude current, high, 1234", claude, "claude-sonnet-5-5", model.StageSummary, model.ThinkingHigh, maxTokensPtr(1234), 1234},
		// Above the output limit of current Claude models: sent as 128000.
		{"claude current, off, 200000", claude, "claude-sonnet-5-5", model.StageSummary, "", maxTokensPtr(200000), 128000},
		// A model that always thinks (effort low, headroom 12000 when
		// automatic): the user's value still is the whole limit.
		{"claude opus 5.5, off, 1234", claude, "claude-opus-5-5", model.StageSummary, "", maxTokensPtr(1234), 1234},
		// Older Claude model, thinking low (budget 2048): 1500 is not above
		// the budget, so the budget is added: 1500 + 2048.
		{"claude legacy, low, 1500", claude, "claude-sonnet-4-5", model.StageSummary, model.ThinkingLow, maxTokensPtr(1500), 3548},
		// Above the budget: sent as it is.
		{"claude legacy, low, 5000", claude, "claude-sonnet-4-5", model.StageSummary, model.ThinkingLow, maxTokensPtr(5000), 5000},
		// OpenAI-compatible: no field unless the user sets one.
		{"openai, automatic", openai, "local-model", model.StageSummary, "", nil, 0},
		{"openai, 1234", openai, "local-model", model.StageSummary, "", maxTokensPtr(1234), 1234},
		// DeepSeek with thinking high: 8192 + 8192 when automatic; the
		// user's value is the whole limit (no budget is added).
		{"deepseek, high, automatic", deepseek, "deepseek-chat", model.StageSummary, model.ThinkingHigh, nil, 16384},
		{"deepseek, high, 1234", deepseek, "deepseek-chat", model.StageSummary, model.ThinkingHigh, maxTokensPtr(1234), 1234},
		// Another role reads its own value (the summary role is set to
		// 1111 below for every case whose stage is not summary).
		{"claude current, repair, 4321", claude, "claude-sonnet-5-5", model.StageRepair, "", maxTokensPtr(4321), 4321},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			srv := newMaxTokensServer(t)
			e.must(e.st.SetKV(e.ctx, model.KVSettingsImported, "1")) // no legacy import from config.toml
			set, err := settings.Open(e.ctx, e.st, e.cfg)
			e.must(err)

			name, typ, balance := "Claude", model.ProviderAnthropic, model.BalanceNone
			switch tc.provider {
			case openai:
				name, typ = "Local", model.ProviderOpenAI
			case deepseek:
				name, typ, balance = "DeepSeek", model.ProviderOpenAI, model.BalanceDeepSeek
			}
			url, key := srv.srv.URL, "sk-test-not-a-real-key"
			p, err := set.CreateProvider(e.ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key, BalanceAPI: &balance})
			e.must(err)

			params := model.RoleParams{MaxTokens: tc.mt}
			if tc.thinking != "" {
				th := tc.thinking
				params.Thinking = &th
			}
			role := model.RoleOfStage(tc.stage)
			// Every lecture role must be set for CurrentSnapshots; the
			// others get a value of their own that must not leak.
			for _, r := range []string{model.RoleSummary, model.RoleRepair, model.RoleTranslate} {
				if r != role {
					e.must(set.SetRole(e.ctx, r, p.ID, tc.model, model.RoleParams{MaxTokens: maxTokensPtr(1111)}))
				}
			}
			e.must(set.SetRole(e.ctx, role, p.ID, tc.model, params))

			l := e.lecture("L1", "m.mp4")
			e.insertSample(l.ID)
			e.setSummary(l.ID, "A lecture about caches.")
			snaps, err := set.CurrentSnapshots()
			e.must(err)
			e.setSnapshot(l.ID, tc.stage, snaps[tc.stage])

			c := llm.NewWithSettings(set, nil, nil, "", nil)
			c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
			r := e.run(e.stage(e.deps(c), tc.stage), l.ID)
			if r.err == nil {
				t.Fatal("the stage succeeded against a provider that refuses every request")
			}
			got, sent := srv.first(t)
			if tc.want == 0 {
				if sent {
					t.Errorf("max_tokens %d sent, want no field", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("max_tokens %d (sent %v), want %d", got, sent, tc.want)
			}
		})
	}
}

func maxTokensPtr(v int) *int { return &v }
