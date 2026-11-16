// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// ---------- helpers ----------

// newSettings opens a settings service on a fresh database WITHOUT the
// legacy import (which would read DEEPSEEK_API_KEY from the environment).
func newSettings(t *testing.T) *settings.Service {
	t.Helper()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetKV(context.Background(), model.KVSettingsImported, "1")
	set, err := settings.Open(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	set.Config().DataDir = t.TempDir()
	return set
}

func addProvider(t *testing.T, set *settings.Service, name, typ, url, key string) model.Provider {
	t.Helper()
	p, err := set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func setRole(t *testing.T, set *settings.Service, role, pid, m string, params model.RoleParams) {
	t.Helper()
	if err := set.SetRole(context.Background(), role, pid, m, params); err != nil {
		t.Fatal(err)
	}
}

func f64(v float64) *float64 { return &v }

type usageCall struct {
	provider                string
	prompt, hit, completion int64
	ctxErr                  error
}

type fakeRecorder struct {
	mu    sync.Mutex
	calls []usageCall
}

func (r *fakeRecorder) RecordUsage(ctx context.Context, providerID string, prompt, hit, completion int64) {
	r.mu.Lock()
	r.calls = append(r.calls, usageCall{providerID, prompt, hit, completion, ctx.Err()})
	r.mu.Unlock()
}

func (r *fakeRecorder) all() []usageCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]usageCall(nil), r.calls...)
}

// recordingPool is a fakePool that is also a UsageRecorder (like sched.Pool).
type recordingPool struct {
	fakePool
	fakeRecorder
}

type capture struct {
	path   string
	header http.Header
	body   map[string]any
}

// captureServer records every request and answers with respond(n) (n from 1).
func captureServer(t *testing.T, respond func(n int, w http.ResponseWriter)) (*httptest.Server, func() []capture) {
	t.Helper()
	var mu sync.Mutex
	var got []capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, capture{r.URL.Path, r.Header.Clone(), body})
		n := len(got)
		mu.Unlock()
		respond(n, w)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capture {
		mu.Lock()
		defer mu.Unlock()
		return append([]capture(nil), got...)
	}
}

func anthropicOK(text string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "stop_reason": "end_turn",
		"content": []any{
			map[string]any{"type": "thinking", "thinking": ""},
			map[string]any{"type": "text", "text": text[:len(text)/2]},
			map[string]any{"type": "text", "text": text[len(text)/2:]},
		},
		"usage": map[string]any{"input_tokens": 100, "cache_read_input_tokens": 300, "cache_creation_input_tokens": 50, "output_tokens": 20},
	})
	return string(b)
}

// anthropicFixtureModel is the model of every role of anthropicClient: a
// current Claude model (adaptive thinking, streamed) that does not think
// when the request has no thinking field (anthropicOffWithoutFieldModels),
// so its level off holds no thinking headroom. Until R6-C it was the
// made-up "claude-test-model", which now counts as a Claude model the lists
// do not know and holds ThinkingHeadroomOffUnlisted at off
// (thinking_off_unlisted_test.go); the tests on this fixture pin the bases
// and limits of a model that stays silent, as they always did.
const anthropicFixtureModel = "claude-opus-4-8"

func anthropicClient(t *testing.T, url string) (*HTTPClient, *settings.Service, model.Provider, *fakePool, *recorder, *fakeRecorder) {
	t.Helper()
	set := newSettings(t)
	p := addProvider(t, set, "Claude", model.ProviderAnthropic, url, testKey)
	for _, role := range model.Roles {
		setRole(t, set, role, p.ID, anthropicFixtureModel, model.RoleParams{})
	}
	pool, rec, usage := &fakePool{}, &recorder{}, &fakeRecorder{}
	c := NewWithSettings(set, pool, usage, "", nil)
	c.Sleep = rec.sleep
	return c, set, p, pool, rec, usage
}

// ---------- Anthropic adapter (A6) ----------

func TestAnthropicRequestFormat(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, anthropicOK(`{"ok": true}`)) })
	c, _, p, pool, _, usage := anthropicClient(t, srv.URL+"/")

	res, err := c.Complete(context.Background(), model.RoleTranslate, "FIXED PREFIX", "VARIABLE PART", true)
	if err != nil {
		t.Fatal(err)
	}
	// text = all text blocks (thinking skipped); prompt = input + cache read + cache creation
	if res != (Result{Text: `{"ok": true}`, PromptTokens: 450, CacheHitTokens: 300, CompletionTokens: 20}) {
		t.Fatalf("result %+v", res)
	}
	if _, err := c.Complete(context.Background(), model.RoleSummary, "FIXED PREFIX", "VARIABLE PART", false); err != nil {
		t.Fatal(err)
	}
	caps := got()
	if len(caps) != 2 {
		t.Fatalf("%d requests", len(caps))
	}
	for i, cp := range caps {
		h := cp.header
		if cp.path != "/v1/messages" || h.Get("x-api-key") != testKey || h.Get("anthropic-version") != "2023-06-01" ||
			h.Get("Authorization") != "" || h.Get("Content-Type") != "application/json" {
			t.Errorf("request %d: path %q headers %v", i, cp.path, h)
		}
		b := cp.body
		// v3.2.x D1: the fixture's model is a current Claude model, so each
		// role has its own base (translate 2000, summary 4000).
		if b["model"] != anthropicFixtureModel || b["max_tokens"] != float64(BaseMaxTokens([]string{model.RoleTranslate, model.RoleSummary}[i], 0)) {
			t.Errorf("request %d: body %v", i, b)
		}
		for _, k := range []string{"temperature", "top_p", "top_k", "response_format"} {
			if _, has := b[k]; has {
				t.Errorf("request %d: unexpected field %q", i, k)
			}
		}
		// The fixture's model is a current Claude model: streamed (v3.2.x
		// P1-c); this server answers with a whole message, which is read
		// as before.
		if b["stream"] != true || h.Get("Accept") != "text/event-stream" {
			t.Errorf("request %d: stream %v, accept %q", i, b["stream"], h.Get("Accept"))
		}
		sys, _ := json.Marshal(b["system"])
		if string(sys) != `[{"cache_control":{"type":"ephemeral"},"text":"FIXED PREFIX","type":"text"}]` {
			t.Errorf("request %d: system %s", i, sys)
		}
	}
	msgs, _ := json.Marshal(caps[0].body["messages"])
	if string(msgs) != `[{"content":"VARIABLE PART\n\nRespond with JSON only.","role":"user"}]` {
		t.Errorf("json-mode messages %s", msgs)
	}
	msgs, _ = json.Marshal(caps[1].body["messages"])
	if string(msgs) != `[{"content":"VARIABLE PART","role":"user"}]` {
		t.Errorf("text-mode messages %s", msgs)
	}
	if pool.providers[0] != p.ID {
		t.Errorf("pool acquired %q, want provider id %q", pool.providers[0], p.ID)
	}
	if u := usage.all(); len(u) != 2 || u[0] != (usageCall{provider: p.ID, prompt: 450, hit: 300, completion: 20}) {
		t.Errorf("usage %+v", u)
	}
}

func TestAnthropicUsageWithoutCacheFields(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) {
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":7,"output_tokens":3}}`)
	})
	c, _, _, _, _, _ := anthropicClient(t, srv.URL)
	res, err := c.Complete(context.Background(), model.RoleSummary, "", "u", false)
	if err != nil || res != (Result{Text: "hi", PromptTokens: 7, CompletionTokens: 3}) {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestAnthropicEmptySystemOmitted(t *testing.T) {
	srv, got := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, anthropicOK("xy")) })
	c, _, _, _, _, _ := anthropicClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), model.RoleSummary, "", "u", false); err != nil {
		t.Fatal(err)
	}
	if _, has := got()[0].body["system"]; has {
		t.Fatal("empty system must be omitted (the API rejects empty text blocks)")
	}
}

func TestAnthropic529Retried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(529)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		io.WriteString(w, anthropicOK("done"))
	}))
	defer srv.Close()
	c, _, _, pool, rec, _ := anthropicClient(t, srv.URL)
	res, err := c.Complete(context.Background(), model.RoleRepair, "s", "u", true)
	if err != nil || res.Text != "done" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if n.Load() != 3 || fmt.Sprint(rec.waits) != fmt.Sprint([]time.Duration{2 * time.Second, 4 * time.Second}) {
		t.Errorf("attempts %d waits %v", n.Load(), rec.waits)
	}
	if a, r := pool.counts(); a != 3 || r != 3 {
		t.Errorf("pool %d/%d", a, r)
	}

	// exhausted: retryable (not permanent), readable message
	n.Store(-100)
	_, err = c.Complete(context.Background(), model.RoleRepair, "s", "u", true)
	if err == nil || sched.IsPermanent(err) || !isCode(err, CodeOverloaded, "role", model.RoleRepair, "retries", 4) {
		t.Fatalf("exhausted: %v", err)
	}
}

func TestAnthropicPermanentStatuses(t *testing.T) {
	for _, code := range []int{400, 401, 403} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(code)
			// Some servers quote the key they rejected.
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key `+r.Header.Get("x-api-key")+`"}}`)
		}))
		c, _, _, _, rec, usage := anthropicClient(t, srv.URL)
		_, err := c.Complete(context.Background(), model.RoleSummary, "s", "u", false)
		srv.Close()
		if err == nil || !sched.IsPermanent(err) || n.Load() != 1 || len(rec.waits) != 0 {
			t.Errorf("HTTP %d: err %v attempts %d", code, err, n.Load())
			continue
		}
		if !strings.Contains(err.Error(), "请检查 API key 或模型名") || !isCode(err, CodeRejected, "status", code, "provider", "Claude") {
			t.Errorf("HTTP %d: %v", code, err)
		}
		if strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), model.KeyHint(testKey)) {
			t.Errorf("HTTP %d: key not redacted: %v", code, err)
		}
		if len(usage.all()) != 0 {
			t.Errorf("HTTP %d: usage recorded for a failed request", code)
		}
	}
}

func TestAnthropicBadEnvelopeRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			io.WriteString(w, `<html>proxy</html>`)
		case 2:
			io.WriteString(w, `{"type":"message"}`) // no content
		default:
			io.WriteString(w, anthropicOK("ok"))
		}
	}))
	defer srv.Close()
	c, _, _, _, rec, _ := anthropicClient(t, srv.URL)
	res, err := c.Complete(context.Background(), model.RoleSummary, "s", "u", false)
	if err != nil || res.Text != "ok" || n.Load() != 3 || len(rec.waits) != 2 {
		t.Errorf("res %+v err %v attempts %d", res, err, n.Load())
	}
}

// ---------- target selection (A4.1) ----------

func TestTargetPriorityInSettingsMode(t *testing.T) {
	type seen struct {
		srv, model, auth string
		temp             any
	}
	var mu sync.Mutex
	var log []seen
	mk := func(name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			log = append(log, seen{name, fmt.Sprint(body["model"]), r.Header.Get("Authorization"), body["temperature"]})
			mu.Unlock()
			io.WriteString(w, okBody("x"))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	srvA, srvB, srvL := mk("A"), mk("B"), mk("L")
	set := newSettings(t)
	a := addProvider(t, set, "ProvA", model.ProviderOpenAI, srvA.URL, "sk-key-a-1111")
	b := addProvider(t, set, "ProvB", model.ProviderOpenAI, srvB.URL, "sk-key-b-2222")
	setRole(t, set, model.RoleSummary, a.ID, "model-a", model.RoleParams{Temperature: f64(0.5)})
	setRole(t, set, model.RoleRepair, a.ID, "model-a", model.RoleParams{})
	pool := &fakePool{}
	c := NewWithSettings(set, pool, nil, "", nil) // nil usage recorder: skipped
	c.Sleep = (&recorder{}).sleep
	last := func() seen { mu.Lock(); defer mu.Unlock(); return log[len(log)-1] }

	// 1) no target: the role's current setting
	if _, err := c.Complete(context.Background(), model.RoleSummary, "s", "u", false); err != nil {
		t.Fatal(err)
	}
	if s := last(); s != (seen{"A", "model-a", "Bearer sk-key-a-1111", 0.5}) {
		t.Fatalf("current setting: %+v", s)
	}
	if _, err := c.Complete(context.Background(), model.RoleRepair, "s", "u", true); err != nil {
		t.Fatal(err)
	}
	if s := last(); s.temp != 0.1 {
		t.Fatalf("default repair temperature: %+v", s)
	}

	// 2) a snapshot target wins over the current setting
	snap := model.LLMSnapshot{ProviderID: b.ID, ProviderName: "ProvB", Model: "model-b", Params: model.RoleParams{Temperature: f64(0.9)}}
	ctx := WithTarget(context.Background(), snap)
	if got, ok := TargetFrom(ctx); !ok || got.Model != "model-b" {
		t.Fatal("TargetFrom")
	}
	if _, ok := TargetFrom(context.Background()); ok {
		t.Fatal("TargetFrom without target")
	}
	if _, err := c.Complete(ctx, model.RoleSummary, "s", "u", false); err != nil {
		t.Fatal(err)
	}
	if s := last(); s != (seen{"B", "model-b", "Bearer sk-key-b-2222", 0.9}) {
		t.Fatalf("target: %+v", s)
	}
	if pool.providers[len(pool.providers)-1] != b.ID {
		t.Fatalf("pool provider %v", pool.providers)
	}
	// a target without temperature uses the role default
	snap.Params = model.RoleParams{}
	c.Complete(WithTarget(context.Background(), snap), model.RoleTranslate, "s", "u", true)
	if s := last(); s.temp != 0.3 {
		t.Fatalf("target default temperature: %+v", s)
	}

	// 3) settings edits apply to the next request (no restart)
	newKey := "sk-key-b-3333"
	if _, err := set.UpdateProvider(context.Background(), b.ID, settings.ProviderInput{APIKey: &newKey}); err != nil {
		t.Fatal(err)
	}
	c.Complete(ctx, model.RoleSummary, "s", "u", false)
	if s := last(); s.auth != "Bearer "+newKey {
		t.Fatalf("after key edit: %+v", s)
	}

	// 4) deleted provider: permanent, no request
	n := len(log)
	gone := model.LLMSnapshot{ProviderID: "01DELETED", ProviderName: "Old DeepSeek", Model: "m"}
	_, err := c.Complete(WithTarget(context.Background(), gone), model.RoleTranslate, "s", "u", true)
	if err == nil || !sched.IsPermanent(err) || !isCode(err, CodeSnapshotProviderDeleted, "provider", "Old DeepSeek") {
		t.Fatalf("deleted provider: %v", err)
	}

	// 5) role not configured: permanent with the settings message
	_, err = c.Complete(context.Background(), model.RoleTranslate, "s", "u", true)
	if err == nil || !sched.IsPermanent(err) || !errors.Is(err, settings.ErrRoleNotConfigured) || !isCode(err, settings.CodeRoleNotConfigured, "role", model.RoleTranslate) {
		t.Fatalf("not configured: %v", err)
	}

	// 6) provider without key: permanent; a local one without key works and sends no Authorization
	nokey := addProvider(t, set, "NoKey", model.ProviderOpenAI, "https://api.example.invalid", "")
	_, err = c.Complete(WithTarget(context.Background(), model.LLMSnapshot{ProviderID: nokey.ID, Model: "m"}), model.RoleSummary, "s", "u", false)
	if err == nil || !sched.IsPermanent(err) || !isCode(err, CodeNoAPIKey, "provider", "NoKey") {
		t.Fatalf("no key: %v", err)
	}
	if len(log) != n {
		t.Fatal("requests were sent for permanent configuration errors")
	}
	local := addProvider(t, set, "Local", model.ProviderOpenAI, srvL.URL, "") // 127.0.0.1: local
	if _, err := c.Complete(WithTarget(context.Background(), model.LLMSnapshot{ProviderID: local.ID, Model: "qwen"}), model.RoleSummary, "s", "u", false); err != nil {
		t.Fatal(err)
	}
	if s := last(); s.srv != "L" || s.auth != "" {
		t.Fatalf("local: %+v", s)
	}
}

func TestTargetInLegacyMode(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, body)
		mu.Unlock()
		io.WriteString(w, okBody("x"))
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL+"/deepseek")
	cfg.LLM.Providers["other"] = config.Provider{BaseURL: srv.URL + "/other", APIKey: testKey}
	pool := &recordingPool{}
	c := New(cfg, pool, "", nil)

	ctx := WithTarget(context.Background(), model.LLMSnapshot{ProviderID: "other", Model: "m2", Params: model.RoleParams{Temperature: f64(0.7)}})
	if _, err := c.Complete(ctx, model.RoleTranslate, "s", "u", true); err != nil {
		t.Fatal(err)
	}
	if paths[0] != "/other/chat/completions" || bodies[0]["model"] != "m2" || bodies[0]["temperature"] != 0.7 {
		t.Fatalf("legacy target: %s %v", paths[0], bodies[0])
	}
	if pool.fakePool.providers[0] != "other" {
		t.Fatalf("pool provider %v", pool.fakePool.providers)
	}
	// the legacy pool is also the usage recorder (sched.Pool implements both)
	if u := pool.fakeRecorder.all(); len(u) != 1 || u[0].provider != "other" || u[0].prompt != 100 || u[0].hit != 64 || u[0].completion != 20 {
		t.Fatalf("legacy usage %+v", u)
	}
	// syllabus_format falls back to the glossary role
	if _, err := c.Complete(context.Background(), model.RoleSyllabusFormat, "s", "u", false); err != nil {
		t.Fatal(err)
	}
	if paths[1] != "/deepseek/chat/completions" || bodies[1]["model"] != "deepseek-flash" || bodies[1]["temperature"] != 0.2 {
		t.Fatalf("syllabus_format: %s %v", paths[1], bodies[1])
	}
	_, err := c.Complete(WithTarget(context.Background(), model.LLMSnapshot{ProviderID: "gone", Model: "m"}), model.RoleRepair, "s", "u", true)
	if err == nil || !sched.IsPermanent(err) || !isCode(err, CodeSnapshotProviderDeleted, "provider", "gone") {
		t.Fatalf("missing legacy provider: %v", err)
	}
}

func TestUsageRecordedAndReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okBody("x"))
	}))
	defer srv.Close()
	set := newSettings(t)
	p := addProvider(t, set, "P", model.ProviderOpenAI, srv.URL, testKey)
	setRole(t, set, model.RoleRepair, p.ID, "m", model.RoleParams{})
	usage := &fakeRecorder{}
	c := NewWithSettings(set, nil, usage, "", nil)
	stats := &sched.StatsRecorder{}
	ctx := sched.WithStatsSink(context.Background(), stats)
	for range 3 {
		if _, err := c.Complete(ctx, model.RoleRepair, "s", "u", true); err != nil {
			t.Fatal(err)
		}
	}
	calls := usage.all()
	if len(calls) != 3 {
		t.Fatalf("calls %+v", calls)
	}
	for _, u := range calls {
		if u != (usageCall{provider: p.ID, prompt: 100, hit: 64, completion: 20}) {
			t.Errorf("usage call %+v", u)
		}
	}
	if s := stats.Snapshot(); s.PromptTokens != 300 || s.CacheHitTokens != 192 || s.CompletionTokens != 60 || s.Requests != 3 {
		t.Errorf("stats sink %+v", s)
	}
}

func TestUsageRecordedWithLiveContextAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okBody("x"))
	}))
	defer srv.Close()
	set := newSettings(t)
	p := addProvider(t, set, "P", model.ProviderOpenAI, srv.URL, testKey)
	setRole(t, set, model.RoleSummary, p.ID, "m", model.RoleParams{})
	usage := &cancelOnRecord{cancel: cancel}
	c := NewWithSettings(set, nil, usage, "", nil)
	if _, err := c.Complete(ctx, model.RoleSummary, "s", "u", false); err != nil {
		t.Fatal(err)
	}
	if usage.err != nil {
		t.Fatalf("RecordUsage got a context that dies with the job: %v", usage.err)
	}
}

// cancelOnRecord cancels the job's context first, then checks the
// context it was given is still usable for the database write.
type cancelOnRecord struct {
	cancel context.CancelFunc
	err    error
}

func (r *cancelOnRecord) RecordUsage(ctx context.Context, _ string, _, _, _ int64) {
	r.cancel()
	r.err = ctx.Err()
}

func TestDebugLogStripsAnthropicKey(t *testing.T) {
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, anthropicOK("ok")) })
	c, set, _, _, _, _ := anthropicClient(t, srv.URL)
	set.Config().LogLevel = "debug"
	ctx := sched.WithJob(context.Background(), model.Job{ID: "01ANTHROPICJOB"})
	if _, err := c.Complete(ctx, model.RoleSummary, "SYS", "USR", false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(set.Config().DataDir, "logs", "llm", "01ANTHROPICJOB.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, testKey) || strings.Contains(strings.ToLower(s), "x-api-key") {
		t.Fatalf("debug log leaks the key:\n%s", s)
	}
	var e debugEntry
	json.Unmarshal(b, &e)
	if e.Provider != "Claude" || e.API != model.ProviderAnthropic || e.Headers["Anthropic-Version"] != AnthropicVersion || !strings.HasSuffix(e.URL, "/v1/messages") {
		t.Fatalf("entry %+v", e)
	}
}
