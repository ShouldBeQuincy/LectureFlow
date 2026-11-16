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
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

const testKey = "sk-unit-test-key-1234"

type fakePool struct {
	mu                 sync.Mutex
	acquired, released int
	inUse, maxInUse    int
	providers          []string
	err                error
}

func (p *fakePool) Acquire(ctx context.Context, provider string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	p.acquired++
	p.inUse++
	p.maxInUse = max(p.maxInUse, p.inUse)
	p.providers = append(p.providers, provider)
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.released++
			p.inUse--
			p.mu.Unlock()
		})
	}, nil
}

func (p *fakePool) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acquired, p.released
}

func testConfig(t *testing.T, baseURL string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	p := cfg.LLM.Providers["deepseek"]
	p.BaseURL = baseURL
	p.APIKey = testKey
	cfg.LLM.Providers["deepseek"] = p
	return cfg
}

// recorder sleeps instantly and records the requested waits.
type recorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (r *recorder) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.waits = append(r.waits, d)
	r.mu.Unlock()
	return ctx.Err()
}

func newClient(t *testing.T, url string) (*HTTPClient, *fakePool, *recorder) {
	t.Helper()
	pool := &fakePool{}
	rec := &recorder{}
	c := New(testConfig(t, url), pool, "", nil)
	c.Sleep = rec.sleep
	return c, pool, rec
}

func okBody(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "prompt_cache_hit_tokens": 64},
	})
	return string(b)
}

func TestCompleteSuccess(t *testing.T) {
	type seen struct {
		path, auth, ctype string
		body              map[string]any
	}
	var mu sync.Mutex
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, seen{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body})
		mu.Unlock()
		io.WriteString(w, okBody(`{"ok": true}`))
	}))
	defer srv.Close()
	c, pool, _ := newClient(t, srv.URL+"/") // trailing slash is tolerated

	cases := []struct {
		role string
		json bool
		temp float64
	}{
		{config.RoleRepair, true, 0.1},
		{config.RoleTranslate, true, 0.3},
		{config.RoleSummary, false, 0.2},
		{config.RoleGlossary, true, 0.2},
	}
	for i, tc := range cases {
		res, err := c.Complete(context.Background(), tc.role, "SYS", "USR", tc.json)
		if err != nil {
			t.Fatalf("%s: %v", tc.role, err)
		}
		if res != (Result{Text: `{"ok": true}`, PromptTokens: 100, CompletionTokens: 20, CacheHitTokens: 64}) {
			t.Errorf("%s: result %+v", tc.role, res)
		}
		s := got[i]
		if s.path != "/chat/completions" || s.auth != "Bearer "+testKey || s.ctype != "application/json" {
			t.Errorf("%s: path %q auth %q ctype %q", tc.role, s.path, s.auth, s.ctype)
		}
		if s.body["model"] != "deepseek-flash" || s.body["temperature"] != tc.temp || s.body["stream"] != false {
			t.Errorf("%s: body %v", tc.role, s.body)
		}
		msgs, _ := s.body["messages"].([]any)
		if len(msgs) != 2 || fmt.Sprint(msgs[0]) != "map[content:SYS role:system]" || fmt.Sprint(msgs[1]) != "map[content:USR role:user]" {
			t.Errorf("%s: messages %v", tc.role, msgs)
		}
		rf, has := s.body["response_format"]
		if tc.json != has || (has && fmt.Sprint(rf) != "map[type:json_object]") {
			t.Errorf("%s: response_format %v (json=%v)", tc.role, rf, tc.json)
		}
	}
	if a, r := pool.counts(); a != len(cases) || r != len(cases) {
		t.Errorf("pool acquired %d released %d", a, r)
	}
	if pool.providers[0] != "deepseek" {
		t.Errorf("pool provider %q", pool.providers[0])
	}
}

func TestRetry429HonoursRetryAfter(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, okBody("fine"))
	}))
	defer srv.Close()
	c, pool, rec := newClient(t, srv.URL)
	res, err := c.Complete(context.Background(), config.RoleTranslate, "s", "u", true)
	if err != nil || res.Text != "fine" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if n.Load() != 2 || len(rec.waits) != 1 || rec.waits[0] != 3*time.Second {
		t.Errorf("attempts %d waits %v", n.Load(), rec.waits)
	}
	if a, r := pool.counts(); a != 2 || r != 2 {
		t.Errorf("pool acquired %d released %d (one slot per attempt)", a, r)
	}
}

func TestRetry5xxExhausted(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Error(w, `{"error":"overloaded"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, _, rec := newClient(t, srv.URL)
	_, err := c.Complete(context.Background(), config.RoleRepair, "s", "u", true)
	if err == nil {
		t.Fatal("expected error")
	}
	if n.Load() != 5 {
		t.Errorf("attempts %d, want 5 (1 + 4 retries)", n.Load())
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	if fmt.Sprint(rec.waits) != fmt.Sprint(want) {
		t.Errorf("waits %v, want %v", rec.waits, want)
	}
	if sched.IsPermanent(err) {
		t.Error("5xx must not be permanent")
	}
	if !isCode(err, CodeServerError, "status", 503, "retries", 4) {
		t.Errorf("error %q", err)
	}
}

func TestPermanentStatusesNotRetried(t *testing.T) {
	for _, tc := range []struct {
		code    int
		errCode string
		msg     string // the frozen Client contract for 400/401/403
	}{
		{400, CodeRejected, "请检查 API key 或模型名"},
		{401, CodeRejected, "请检查 API key 或模型名"},
		{403, CodeRejected, "请检查 API key 或模型名"},
		{402, CodeNoBalance, ""},
		{404, CodeRequestFailed, ""},
	} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			http.Error(w, `{"error":{"message":"Authentication Fails"}}`, tc.code)
		}))
		c, _, rec := newClient(t, srv.URL)
		_, err := c.Complete(context.Background(), config.RoleSummary, "s", "u", false)
		srv.Close()
		if err == nil || !sched.IsPermanent(err) {
			t.Errorf("HTTP %d: err %v, want permanent", tc.code, err)
			continue
		}
		if n.Load() != 1 || len(rec.waits) != 0 {
			t.Errorf("HTTP %d: %d attempts, waits %v", tc.code, n.Load(), rec.waits)
		}
		if !strings.Contains(err.Error(), tc.msg) || !isCode(err, tc.errCode, "role", config.RoleSummary) ||
			(tc.errCode != CodeNoBalance && !isCode(err, tc.errCode, "status", tc.code)) {
			t.Errorf("HTTP %d: error %q %+v", tc.code, err, infoOf(err))
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("HTTP %d: key leaked into error", tc.code)
		}
	}
}

// blockingServer never answers; its handlers return when the client hangs up
// or the test ends.
func blockingServer(t *testing.T, n *atomic.Int32) *httptest.Server {
	return blockingServerEntered(t, n, nil)
}

// blockingServerEntered is blockingServer that also signals entered (when
// not nil; never blocking) once a request reached the handler.
func blockingServerEntered(t *testing.T, n *atomic.Int32, entered chan<- struct{}) *httptest.Server {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	return srv
}

func TestTimeoutRetried(t *testing.T) {
	var n atomic.Int32
	srv := blockingServer(t, &n)
	c, _, _ := newClient(t, srv.URL)
	c.Timeout = 50 * time.Millisecond
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	start := time.Now()
	_, err := c.Complete(context.Background(), config.RoleTranslate, "s", "u", true)
	if err == nil || sched.IsPermanent(err) {
		t.Fatalf("err %v", err)
	}
	if !isCode(err, CodeTimeout, "role", config.RoleTranslate, "retries", 2, "seconds", 0.05) {
		t.Errorf("error %q", err)
	}
	if n.Load() != 3 {
		t.Errorf("attempts %d, want 3", n.Load())
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestNetworkUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	c, pool, rec := newClient(t, url)
	_, err := c.Complete(context.Background(), config.RoleGlossary, "s", "u", true)
	if err == nil || sched.IsPermanent(err) {
		t.Fatalf("err %v", err)
	}
	if !isCode(err, CodeNetwork, "role", config.RoleGlossary) {
		t.Errorf("error %q", err)
	}
	if len(rec.waits) != 4 {
		t.Errorf("waits %v, want 4 retries", rec.waits)
	}
	if a, r := pool.counts(); a != 5 || r != 5 {
		t.Errorf("pool acquired %d released %d", a, r)
	}
}

func TestCancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(testConfig(t, srv.URL), &fakePool{}, "", nil)
	c.Backoff = []time.Duration{time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.Complete(ctx, config.RoleRepair, "s", "u", true)
	if !errors.Is(err, context.Canceled) || err != context.Canceled {
		t.Errorf("err %v, want ctx.Err()", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("cancel took %v", d)
	}
}

func TestCancelDuringRequest(t *testing.T) {
	var n atomic.Int32
	entered := make(chan struct{}, 1)
	srv := blockingServerEntered(t, &n, entered)
	c, pool, rec := newClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Cancelled once the request is at the server (a fixed delay could fire
	// before it got there on a loaded machine). Without a request, cancelled
	// after 10 s and the attempt count below fails. The goroutine ends when
	// Complete returns (cancel below), also when the test fails (review T6).
	go func() {
		select {
		case <-entered:
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
		cancel()
	}()
	start := time.Now()
	_, err := c.Complete(ctx, config.RoleTranslate, "s", "u", true)
	cancel()
	if err != context.Canceled {
		t.Errorf("err %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("cancel took %v", d)
	}
	if n.Load() != 1 {
		t.Errorf("cancelled request retried: %d attempts", n.Load())
	}
	if len(rec.waits) != 0 {
		t.Errorf("backoff waits after the cancel: %v", rec.waits)
	}
	if a, r := pool.counts(); a != r {
		t.Errorf("pool slot leaked: acquired %d released %d", a, r)
	}
	// already cancelled: no request at all
	if _, err := c.Complete(ctx, config.RoleTranslate, "s", "u", true); err != context.Canceled {
		t.Errorf("pre-cancelled err %v", err)
	}
}

func TestBadEnvelopeRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			io.WriteString(w, "<html>gateway</html>")
		case 2:
			io.WriteString(w, `{"choices": []}`)
		default:
			io.WriteString(w, okBody("ok"))
		}
	}))
	defer srv.Close()
	c, _, rec := newClient(t, srv.URL)
	res, err := c.Complete(context.Background(), config.RoleSummary, "s", "u", false)
	if err != nil || res.Text != "ok" || n.Load() != 3 || len(rec.waits) != 2 {
		t.Errorf("res %+v err %v attempts %d waits %v", res, err, n.Load(), rec.waits)
	}
}

func TestCachedTokensFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":50,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":32}}}`)
	}))
	defer srv.Close()
	c, _, _ := newClient(t, srv.URL)
	res, err := c.Complete(context.Background(), config.RoleSummary, "s", "u", false)
	if err != nil || res.CacheHitTokens != 32 || res.PromptTokens != 50 || res.CompletionTokens != 5 {
		t.Errorf("res %+v err %v", res, err)
	}
}

func TestConfigErrorsArePermanent(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("local provider without key sent Authorization")
		}
		io.WriteString(w, okBody("local"))
	}))
	defer srv.Close()

	cfg := testConfig(t, srv.URL)
	p := cfg.LLM.Providers["deepseek"]
	p.APIKey = ""
	p.APIKeyEnv = "LECTUREFLOW_TEST_UNSET_KEY_VAR"
	cfg.LLM.Providers["deepseek"] = p
	c := New(cfg, nil, "", nil)
	_, err := c.Complete(context.Background(), config.RoleSummary, "s", "u", false)
	if err == nil || !sched.IsPermanent(err) || !strings.Contains(err.Error(), "LECTUREFLOW_TEST_UNSET_KEY_VAR") ||
		!strings.Contains(err.Error(), "请检查 API key 或模型名") || !isCode(err, CodeNoAPIKey, "provider", "deepseek") {
		t.Errorf("missing key: %v", err)
	}
	if n.Load() != 0 {
		t.Error("request sent without a key")
	}

	if _, err := c.Complete(context.Background(), "nosuchrole", "s", "u", false); err == nil || !sched.IsPermanent(err) {
		t.Errorf("unknown role: %v", err)
	}

	p.Local = true // e.g. Ollama: no key needed
	cfg.LLM.Providers["deepseek"] = p
	if res, err := c.Complete(context.Background(), config.RoleSummary, "s", "u", false); err != nil || res.Text != "local" {
		t.Errorf("local provider: %+v %v", res, err)
	}
}

func TestPoolErrorReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okBody("x"))
	}))
	defer srv.Close()
	c, pool, rec := newClient(t, srv.URL)
	pool.err = errors.New("pool closed")
	_, err := c.Complete(context.Background(), config.RoleSummary, "s", "u", false)
	if err == nil || !strings.Contains(err.Error(), "pool closed") || len(rec.waits) != 0 {
		t.Errorf("err %v waits %v", err, rec.waits)
	}
}

func TestUsageAccumulation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okBody("x"))
	}))
	defer srv.Close()
	c, pool, _ := newClient(t, srv.URL)
	u := &Usage{}
	ctx := WithUsage(context.Background(), u)
	if UsageFrom(ctx) != u || UsageFrom(context.Background()) != nil {
		t.Fatal("UsageFrom")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Complete(ctx, config.RoleTranslate, "s", "u", true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// a call without the accumulator is not counted
	if _, err := c.Complete(context.Background(), config.RoleTranslate, "s", "u", true); err != nil {
		t.Fatal(err)
	}
	tot := u.Totals()
	if tot != (Totals{PromptTokens: 2000, CompletionTokens: 400, CacheHitTokens: 1280, Requests: 20}) {
		t.Errorf("totals %+v", tot)
	}

	// the scheduler's stats sink receives every successful response
	rec := &sched.StatsRecorder{}
	sctx := sched.WithStatsSink(context.Background(), rec)
	for range 3 {
		if _, err := c.Complete(sctx, config.RoleRepair, "s", "u", true); err != nil {
			t.Fatal(err)
		}
	}
	if s := rec.Snapshot(); s.PromptTokens != 300 || s.CompletionTokens != 60 || s.CacheHitTokens != 192 || s.Requests != 3 {
		t.Errorf("stats sink %+v", s)
	}
	if a, r := pool.counts(); a != 24 || r != 24 {
		t.Errorf("pool %d/%d", a, r)
	}

	price := &config.Price{Input: 2, CacheHit: 0.5, Output: 8}
	// miss 720*2 + hit 1280*0.5 + out 400*8 = 1440 + 640 + 3200 = 5280 per 1e6
	if got := tot.Cost(*price); fmt.Sprintf("%.6f", got) != "0.005280" {
		t.Errorf("cost %v", got)
	}
	base := model.JobStats{PromptTokens: 1, CompletionTokens: 2, CacheHitTokens: 3, Requests: 4, EstimatedCost: 1, DurationMS: 9}
	st := tot.AddTo(base, price)
	if st.PromptTokens != 2001 || st.CompletionTokens != 402 || st.CacheHitTokens != 1283 || st.Requests != 24 ||
		fmt.Sprintf("%.6f", st.EstimatedCost) != "1.005280" || st.DurationMS != 9 {
		t.Errorf("stats %+v", st)
	}
	if st := tot.AddTo(model.JobStats{}, nil); st.EstimatedCost != 0 || st.PromptTokens != 2000 {
		t.Errorf("no price: %+v", st)
	}

	cfg := testConfig(t, srv.URL)
	if PriceForRole(cfg, config.RoleRepair) != nil {
		t.Error("price without config")
	}
	cfg.LLM.Prices = map[string]map[string]config.Price{"deepseek": {"deepseek-flash": *price}}
	if p := PriceForRole(cfg, config.RoleRepair); p == nil || *p != *price {
		t.Errorf("price %v", p)
	}
}

func TestDebugLog(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			http.Error(w, "busy", http.StatusBadGateway)
			return
		}
		io.WriteString(w, okBody(`{"idx": 1, "zh": "好"}`))
	}))
	defer srv.Close()

	for _, level := range []string{"info", "debug"} {
		n.Store(0)
		c, _, _ := newClient(t, srv.URL)
		c.cfg.LogLevel = level
		dir := filepath.Join(c.cfg.DataDir, "logs", "llm")
		ctx := sched.WithJob(context.Background(), model.Job{ID: "01JOBTEST"})
		if _, err := c.Complete(ctx, config.RoleTranslate, "SYSTEM PROMPT", "USER PART", true); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "01JOBTEST.jsonl"))
		if level == "info" {
			if err == nil {
				t.Errorf("info level wrote a debug log")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		lines := strings.Split(strings.TrimSpace(s), "\n")
		if len(lines) != 2 {
			t.Fatalf("debug log has %d lines:\n%s", len(lines), s)
		}
		if strings.Contains(s, testKey) || strings.Contains(strings.ToLower(s), "authorization") {
			t.Errorf("debug log contains the Authorization header:\n%s", s)
		}
		var first, second debugEntry
		_ = json.Unmarshal([]byte(lines[0]), &first)
		_ = json.Unmarshal([]byte(lines[1]), &second)
		if first.Status != 502 || second.Status != 200 || first.Attempt != 0 || second.Attempt != 1 || second.JobID != "01JOBTEST" ||
			!strings.Contains(string(second.Request), "SYSTEM PROMPT") || !strings.Contains(string(second.Response), `\"zh\"`) {
			t.Errorf("entries:\n%s", s)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("7"); !ok || d != 7*time.Second {
		t.Errorf("seconds: %v %v", d, ok)
	}
	if d, ok := parseRetryAfter("0"); !ok || d != 0 {
		t.Errorf("zero: %v %v", d, ok)
	}
	if d, ok := parseRetryAfter("100000"); !ok || d != MaxRetryAfter {
		t.Errorf("cap: %v %v", d, ok)
	}
	date := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(date); !ok || d < 25*time.Second || d > 31*time.Second {
		t.Errorf("date: %v %v", d, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Error("empty")
	}
	if _, ok := parseRetryAfter("soon"); ok {
		t.Error("garbage")
	}
}
