// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// echoKey is a key long enough to straddle snippet's 300-byte cut.
const echoKey = "sk-ECHOECHO-0123456789abcdefghijklmnopqrstuvwxyz-WXYZ"

// keyBodies are server bodies that echo the key: in full in a short body,
// and straddling the 300-byte excerpt (sec2-app L1).
func keyBodies() map[string]string {
	return map[string]string{
		"full":      `{"error": {"message": "Incorrect API key provided: ` + echoKey + `"}}`,
		"straddles": strings.Repeat("x", 300-10) + echoKey + " trailing text",
	}
}

// leaksKey reports whether s contains any 8-byte piece of echoKey (its
// redacted hint keeps only "sk-" and the last 4 characters).
func leaksKey(s string) bool {
	for i := 0; i+8 <= len(echoKey); i++ {
		if strings.Contains(s, echoKey[i:i+8]) {
			return true
		}
	}
	return false
}

func echoServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestKeyRedactedBeforeTruncation (sec2-app L1): an echoed key never reaches
// a job error, a probe error or the debug log, in full or in part.
func TestKeyRedactedBeforeTruncation(t *testing.T) {
	if len(model.KeyHint(echoKey)) >= 12 || leaksKey(model.KeyHint(echoKey)) {
		t.Fatalf("the hint %q would defeat the check", model.KeyHint(echoKey))
	}
	for name, body := range keyBodies() {
		t.Run(name, func(t *testing.T) {
			check := func(what, s string) {
				t.Helper()
				if leaksKey(s) {
					t.Errorf("%s leaks the key: %q", what, s)
				}
			}
			// HTTP status error (401: permanent)
			c, _, _ := newClient(t, echoServer(t, http.StatusUnauthorized, body).URL)
			setKey(c, echoKey)
			c.cfg.LogLevel = "debug"
			ctx := sched.WithJob(context.Background(), model.Job{ID: "01JOBREDACT"})
			_, err := c.Complete(ctx, config.RoleTranslate, "S", "U", true)
			if err == nil {
				t.Fatal("no error")
			}
			check("status error", err.Error())
			log, rerr := os.ReadFile(filepath.Join(c.cfg.DataDir, "logs", "llm", "01JOBREDACT.jsonl"))
			if rerr != nil {
				t.Fatal(rerr)
			}
			check("debug log", string(log))

			// 200 with an unreadable envelope (retried, then CodeBadResponse)
			c, _, _ = newClient(t, echoServer(t, http.StatusOK, body).URL)
			setKey(c, echoKey)
			if _, err := c.Complete(context.Background(), config.RoleTranslate, "S", "U", true); err == nil {
				t.Fatal("no envelope error")
			} else {
				check("OpenAI envelope error", err.Error())
			}

			// Anthropic envelope
			srv := echoServer(t, http.StatusOK, body)
			ac, set, p, _, _, _ := anthropicClient(t, srv.URL)
			k := echoKey
			if _, err := set.UpdateProvider(context.Background(), p.ID, settings.ProviderInput{APIKey: &k}); err != nil {
				t.Fatal(err)
			}
			if _, err := ac.Complete(context.Background(), config.RoleTranslate, "S", "U", true); err == nil {
				t.Fatal("no Anthropic envelope error")
			} else {
				check("Anthropic envelope error", err.Error())
			}

			// probes: HTTP error, not JSON, unreadable model list
			for _, tc := range []struct {
				status int
				probe  bool
			}{{http.StatusUnauthorized, false}, {http.StatusOK, false}, {http.StatusOK, true}} {
				b := body
				if tc.status == http.StatusOK {
					b = "<html> " + body // not JSON
				}
				srv := echoServer(t, tc.status, b)
				pv := model.Provider{ID: "p1", Name: "Echo", Type: model.ProviderOpenAI, BaseURL: srv.URL, APIKey: echoKey}
				if tc.probe {
					_, err := ProbeModels(context.Background(), pv)
					if err == nil {
						t.Fatal("model list parsed")
					}
					check("probe models", err.Error())
					continue
				}
				r := TestConnection(context.Background(), pv, "m")
				if r.OK {
					t.Fatal("test connection passed")
				}
				check("test connection", r.Error)
				if r.ErrorInfo != nil {
					check("test connection detail", r.ErrorInfo.Detail)
				}
			}
		})
	}
}

// setKey sets the key of the legacy client's deepseek provider.
func setKey(c *HTTPClient, key string) {
	p := c.cfg.LLM.Providers["deepseek"]
	p.APIKey = key
	c.cfg.LLM.Providers["deepseek"] = p
}

// TestRedirectsKeepTheKeyOnItsHost (sec2-app): a redirect to another host
// is not followed (the key would travel with it: net/http keeps x-api-key on
// every redirect); a redirect on the same host is.
func TestRedirectsKeepTheKeyOnItsHost(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		io.WriteString(w, okBody(`{"idx": 0, "zh": "好"}`))
	}))
	defer other.Close()
	var sameHits atomic.Int32
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/moved/"):
			sameHits.Add(1)
			gotAuth.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("x-api-key"))
			io.WriteString(w, okBody(`{"idx": 0, "zh": "好"}`))
		case strings.HasPrefix(r.URL.Path, "/same/"):
			http.Redirect(w, r, "/moved"+strings.TrimPrefix(r.URL.Path, "/same"), http.StatusTemporaryRedirect)
		default:
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
		}
	}))
	defer srv.Close()

	// another host: not followed, the request fails with the 307
	c, _, _ := newClient(t, srv.URL+"/away")
	_, err := c.Complete(context.Background(), config.RoleTranslate, "S", "U", true)
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("cross-host redirect: %v", err)
	}
	ac, _, _, _, _, _ := anthropicClient(t, srv.URL+"/away")
	if _, err := ac.Complete(context.Background(), config.RoleTranslate, "S", "U", true); err == nil {
		t.Fatal("Anthropic cross-host redirect followed")
	}
	pv := model.Provider{ID: "p1", Name: "R", Type: model.ProviderAnthropic, BaseURL: srv.URL + "/away", APIKey: testKey}
	if r := TestConnection(context.Background(), pv, "m"); r.OK {
		t.Fatal("probe followed a cross-host redirect")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the other host got %d requests", n)
	}

	// the same host: followed, with the key
	c, _, _ = newClient(t, srv.URL+"/same")
	if _, err := c.Complete(context.Background(), config.RoleTranslate, "S", "U", true); err != nil {
		t.Fatalf("same-host redirect: %v", err)
	}
	if sameHits.Load() != 1 || gotAuth.Load() != "Bearer "+testKey+"|" {
		t.Fatalf("same-host redirect: hits %d, auth %v", sameHits.Load(), gotAuth.Load())
	}
	// a caller's own client gets the policy too, without being changed
	own := &http.Client{}
	c2 := New(testConfig(t, srv.URL+"/away"), nil, "", own)
	c2.Sleep = (&recorder{}).sleep
	if _, err := c2.Complete(context.Background(), config.RoleTranslate, "S", "U", true); err == nil || own.CheckRedirect != nil {
		t.Fatalf("caller's client: err %v, CheckRedirect changed %v", err, own.CheckRedirect != nil)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the other host got a request")
	}

	u := func(s string) *url.URL { v, _ := url.Parse(s); return v }
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{"https://api.deepseek.com/x", "https://API.deepseek.com/y", true},
		{"http://api.deepseek.com/x", "https://api.deepseek.com/x", true},
		{"https://api.deepseek.com/x", "http://api.deepseek.com/x", false},
		{"https://api.deepseek.com/x", "https://evil.example/x", false},
		{"https://api.deepseek.com/x", "https://api.deepseek.com:8443/x", false},
		{"https://api.deepseek.com:443/x", "https://api.deepseek.com/x", true},
		{"https://api.deepseek.com/x", "https://deepseek.com/x", false},
		// SEC10: hosts by db.HostKey's rule, as the key's bound origin
		{"https://api.lmσ.example/x", "https://api.lmς.example/x", false}, // sigma / final sigma: EqualFold's pair, two hosts
		{"https://api.lmς.example/x", "https://api.lm%CF%83.example/x", false},
		{"https://api.lmσ.example/x", "https://api.lm%CF%83.example:443/y", true}, // the same bytes
		{"https://api.vİdeo.example/x", "https://api.video.example/x", false},     // U+0130
		{"https://api.Key.example/x", "https://api.key.example/x", false},         // U+212A: the same host after IDNA; hardening
		{"https://API.Key.EXAMPLE/x", "https://api.key.example/y", true},
	} {
		if got := sameOrigin(u(tc.from), u(tc.to)); got != tc.want {
			t.Errorf("sameOrigin(%s, %s) = %v", tc.from, tc.to, got)
		}
	}
}

// feedbackPool is a fakePool that records RateFeedback.
type feedbackPool struct {
	fakePool
	mu        sync.Mutex
	throttled []time.Time
	succeeded int
}

func (p *feedbackPool) Throttled(provider string, sent time.Time) {
	p.mu.Lock()
	p.throttled = append(p.throttled, sent)
	p.mu.Unlock()
}

func (p *feedbackPool) Succeeded(provider string) {
	p.mu.Lock()
	p.succeeded++
	p.mu.Unlock()
}

// TestRateFeedbackAndSentHook (perf-llm): 429 / 503 / 529 are reported as
// throttles with the attempt's send time, answers as successes; the sent
// hook runs before every attempt.
func TestRateFeedbackAndSentHook(t *testing.T) {
	statuses := []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, StatusOverloaded, http.StatusBadGateway}
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i := int(n.Add(1)) - 1; i < len(statuses) {
			w.WriteHeader(statuses[i])
			return
		}
		io.WriteString(w, okBody(`{"idx": 0, "zh": "好"}`))
	}))
	defer srv.Close()
	pool := &feedbackPool{}
	c := New(testConfig(t, srv.URL), pool, "", nil)
	c.Sleep = (&recorder{}).sleep
	var sent atomic.Int32
	ctx := WithSentHook(context.Background(), func() { sent.Add(1) })
	before := time.Now()
	if _, err := c.Complete(ctx, config.RoleTranslate, "S", "U", true); err != nil {
		t.Fatal(err)
	}
	if sent.Load() != 5 {
		t.Errorf("sent hook ran %d times, want 5 (every attempt)", sent.Load())
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.throttled) != 3 || pool.succeeded != 1 {
		t.Fatalf("throttled %d (want 3: 429, 503, 529; not 502), succeeded %d", len(pool.throttled), pool.succeeded)
	}
	for _, at := range pool.throttled {
		if at.Before(before) || at.After(time.Now()) {
			t.Errorf("throttle sent at %v", at)
		}
	}
}

// TestThrottledBurstAdaptsPool (perf-llm): 64 requests at once against a
// provider that refuses more than 10 in flight (HTTP 429, like DeepSeek's
// account concurrency limit) all succeed: the pool halves its limit on the
// first throttles and the retries queue in it.
func TestThrottledBurstAdaptsPool(t *testing.T) {
	fake := llmtest.New(t)
	fake.SetConcurrencyLimit(10)
	fake.SetDelay(20 * time.Millisecond)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	fake.Configure(cfg)
	p := cfg.LLM.Providers["deepseek"]
	p.MaxConcurrency, p.RequestsPerMinute = 64, 0
	cfg.LLM.Providers["deepseek"] = p
	pool := sched.NewPool(cfg, nil, sched.PoolOptions{})
	c := New(cfg, pool, "", nil)
	c.Backoff = []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	system := ctxbuild.SummarySystemTemplate // the fake answers it as the summary role
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Complete(context.Background(), config.RoleSummary, system, "u", false); err != nil {
				failed.Add(1)
				t.Log(err)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.Fatalf("%d of 64 requests failed", failed.Load())
	}
	limit, max, _ := pool.Limit("deepseek")
	if fake.Throttled() == 0 || limit >= max {
		t.Fatalf("throttled %d, limit %d / %d: the burst should have lowered the limit", fake.Throttled(), limit, max)
	}
	if fake.MaxInflight() > 10 {
		t.Fatalf("max in flight %d", fake.MaxInflight())
	}
	t.Logf("429 answers %d, adaptive limit %d / %d", fake.Throttled(), limit, max)
}
