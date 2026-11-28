// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

// v3.3.1 (openai_compat.go): how the resends of a refused limit name or
// temperature go along with the rest of a call (concurrent calls, §12
// retries, the truncation ladder, the API pool, usage, the call log, key
// redaction), and DeepSeek's request fields on OpenAI's own API.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

const errRateLimit = `{"error":{"message":"Rate limit reached for requests","type":"requests","param":null,"code":"rate_limit_exceeded"}}`

// logBuf is a buffer safe for concurrent writes.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *logBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// captureSlog sends the default slog logger to a buffer for the test. Not
// for parallel tests: the logger is the process's.
func captureSlog(t *testing.T) *logBuf {
	t.Helper()
	buf := &logBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// poolEnv is a compatEnv whose client has an API pool and a usage recorder.
type poolEnv struct {
	*compatEnv
	pool  *fakePool
	usage *fakeRecorder
}

func newPoolEnv(t *testing.T, respond func(req jsonReq) (int, string)) *poolEnv {
	t.Helper()
	url, sent := jsonServer(t, respond)
	set := newSettings(t)
	pool, usage, rec := &fakePool{}, &fakeRecorder{}, &recorder{}
	c := NewWithSettings(set, pool, usage, "", redirectClient(t, url))
	c.Sleep = rec.sleep
	return &poolEnv{compatEnv: &compatEnv{t: t, set: set, c: c, url: url, rec: rec, sent: sent}, pool: pool, usage: usage}
}

func (e *poolEnv) waits() []time.Duration {
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	return append([]time.Duration(nil), e.rec.waits...)
}

// limitField is the name the output limit of body went by ("-" = none).
func limitField(body string) string {
	switch {
	case strings.Contains(body, `"max_completion_tokens"`) && strings.Contains(body, `"max_tokens"`):
		return "BOTH"
	case strings.Contains(body, `"max_completion_tokens"`):
		return fieldMaxCompletionTokens
	case strings.Contains(body, `"max_tokens"`):
		return fieldMaxTokens
	}
	return "-"
}

// 32 calls whose first requests are all in flight before any refusal is
// learned (the server holds them until the 32nd arrives): each call answers
// its own refusal once, the pool and the usage stay balanced, the warning is
// logged once, and the next call sends the learned name at once.
func TestConcurrentFirstRefusalsOfTheLimitName(t *testing.T) {
	const n = 32
	logs := captureSlog(t)
	var arrived atomic.Int32
	gate := make(chan struct{})
	var closeOnce sync.Once
	e := newPoolEnv(t, func(req jsonReq) (int, string) {
		if reqHas(req, fieldMaxTokens) {
			if arrived.Add(1) == n {
				closeOnce.Do(func() { close(gate) })
			}
			select {
			case <-gate:
			case <-time.After(20 * time.Second):
			}
			return http.StatusBadRequest, errMaxTokens
		}
		return http.StatusOK, okBody(`{"ok": true}`)
	})
	relay := e.provider("Relay", model.ProviderOpenAI, "")
	params := model.RoleParams{MaxTokens: ip(3000)}
	errs := make([]error, n)
	texts := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := e.complete(relay, "gpt-5-mini", model.RoleTranslate, params, true)
			errs[i], texts[i] = err, res.Text
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || texts[i] != `{"ok": true}` {
			t.Errorf("call %d: text %q err %v", i, texts[i], errs[i])
		}
	}
	count := map[string]int{}
	bodies := e.sent()
	for _, b := range bodies {
		count[limitField(b)]++
	}
	if len(bodies) != 2*n || count[fieldMaxTokens] != n || count[fieldMaxCompletionTokens] != n {
		t.Errorf("requests %v, want %d max_tokens + %d max_completion_tokens", count, n, n)
	}
	if arrived.Load() != n {
		t.Errorf("only %d first requests were in flight together", arrived.Load())
	}
	if acq, rel := e.pool.counts(); acq != 2*n || rel != 2*n {
		t.Errorf("pool acquired %d released %d, want %d each", acq, rel, 2*n)
	}
	if u := e.usage.all(); len(u) != n {
		t.Errorf("usage records %d, want %d (a 400 is not billed)", len(u), n)
	}
	if warned := strings.Count(logs.String(), "refused the name of the output limit"); warned != 1 {
		t.Errorf("warning logged %d times, want 1:\n%s", warned, logs.String())
	}
	if _, err := e.complete(relay, "gpt-5-mini", model.RoleTranslate, params, true); err != nil {
		t.Fatal(err)
	}
	if got := e.sent(); len(got) != 1 || limitField(got[0]) != fieldMaxCompletionTokens {
		t.Errorf("follow-up sent %q, want one request with max_completion_tokens", got)
	}
	e.noWaits()
}

// The rename resend uses none of the §12 backoff retries: after it, 429s
// still get the whole ladder of waits.
func TestRenameResendKeepsTheBackoffRetries(t *testing.T) {
	for _, throttles := range []int{1, 4} {
		t.Run(fmt.Sprintf("rename then %dx429 then ok", throttles), func(t *testing.T) {
			var n429 atomic.Int32
			e := newPoolEnv(t, func(req jsonReq) (int, string) {
				if reqHas(req, fieldMaxTokens) {
					return http.StatusBadRequest, errMaxTokens
				}
				if int(n429.Add(1)) <= throttles {
					return http.StatusTooManyRequests, errRateLimit
				}
				return http.StatusOK, okBody("fine")
			})
			relay := e.provider("Relay", model.ProviderOpenAI, "")
			res, err := e.complete(relay, "gpt-5-mini", model.RoleSummary, model.RoleParams{MaxTokens: ip(1000)}, false)
			if err != nil || res.Text != "fine" {
				t.Fatalf("text %q err %v", res.Text, err)
			}
			bodies := e.sent()
			fields := make([]string, len(bodies))
			for i, b := range bodies {
				fields[i] = limitField(b)
			}
			want := []string{fieldMaxTokens}
			for i := 0; i <= throttles; i++ {
				want = append(want, fieldMaxCompletionTokens)
			}
			if !slices.Equal(fields, want) {
				t.Errorf("fields %v, want %v", fields, want)
			}
			if w := e.waits(); !slices.Equal(w, DefaultBackoff[:throttles]) {
				t.Errorf("waits %v, want %v", w, DefaultBackoff[:throttles])
			}
			if acq, rel := e.pool.counts(); acq != len(bodies) || rel != len(bodies) {
				t.Errorf("pool %d/%d, want %d", acq, rel, len(bodies))
			}
			if len(e.usage.all()) != 1 {
				t.Errorf("usage %d, want 1", len(e.usage.all()))
			}
		})
	}
}

// The resends use none of the truncation retries either: every rung of the
// ladder keeps the new name, and a dropped temperature stays dropped.
func TestResendsAlongTheTruncationLadder(t *testing.T) {
	t.Run("rename, cut, cut, ok", func(t *testing.T) {
		var cuts atomic.Int32
		e := newPoolEnv(t, func(req jsonReq) (int, string) {
			if reqHas(req, fieldMaxTokens) {
				return http.StatusBadRequest, errMaxTokens
			}
			if cuts.Add(1) <= 2 {
				return http.StatusOK, lengthBody(`{"a":`)
			}
			return http.StatusOK, okBody(`{"ok": true}`)
		})
		relay := e.provider("Relay", model.ProviderOpenAI, "")
		_, err := e.complete(relay, "gpt-5-mini", model.RoleRepair, model.RoleParams{MaxTokens: ip(3000)}, true)
		var limits []string
		for _, b := range e.sent() {
			var m map[string]any
			_ = json.Unmarshal([]byte(b), &m)
			limits = append(limits, fmt.Sprintf("%s=%v", limitField(b), m[limitField(b)]))
		}
		want := []string{"max_tokens=3000", "max_completion_tokens=3000", "max_completion_tokens=6000", "max_completion_tokens=12000"}
		if err != nil || !slices.Equal(limits, want) {
			t.Errorf("err %v limits %v, want %v", err, limits, want)
		}
		// The two cut answers are billed, as before; the 400 is not.
		if u := len(e.usage.all()); u != 3 {
			t.Errorf("usage records %d, want 3 (2 truncated + 1 ok)", u)
		}
		e.noWaits()
	})
	t.Run("temperature dropped, cut, ok", func(t *testing.T) {
		var cuts atomic.Int32
		e := newPoolEnv(t, func(req jsonReq) (int, string) {
			if reqHas(req, fieldTemperature) {
				return http.StatusBadRequest, errTemperatureValue
			}
			if cuts.Add(1) == 1 {
				return http.StatusOK, lengthBody(`{"a":`)
			}
			return http.StatusOK, okBody(`{"ok": true}`)
		})
		oa := e.provider("OpenAI", model.ProviderOpenAI, officialBase)
		_, err := e.complete(oa, "o4-mini", model.RoleRepair, model.RoleParams{MaxTokens: ip(3000)}, true)
		bodies := e.sent()
		if err != nil || len(bodies) != 3 || !strings.Contains(bodies[0], `"temperature"`) ||
			strings.Contains(bodies[1], `"temperature"`) || strings.Contains(bodies[2], `"temperature"`) ||
			!strings.Contains(bodies[2], `"max_completion_tokens":6000`) {
			t.Errorf("err %v bodies %q: want temperature, then none, then none with max_completion_tokens 6000", err, bodies)
		}
		e.noWaits()
	})
}

// What the call log and the job's request events say about a rename: the
// resend is the call's second attempt, with its reason; the job's usage
// counts one request. The call log's max_tokens is the output limit,
// whatever name it went by.
func TestRenameInTheCallLogAndRequestEvents(t *testing.T) {
	e := newPoolEnv(t, func(req jsonReq) (int, string) {
		if reqHas(req, fieldMaxTokens) {
			return http.StatusBadRequest, errMaxTokens
		}
		return http.StatusOK, okBody(`{"ok": true}`)
	})
	relay := e.provider("Relay", model.ProviderOpenAI, "")
	stats := &sched.StatsRecorder{}
	snap := model.LLMSnapshot{ProviderID: relay.ID, ProviderName: relay.Name, Model: "gpt-5-mini", Params: model.RoleParams{MaxTokens: ip(3000)}}
	ctx := sched.WithStatsSink(sched.WithJob(context.Background(), model.Job{ID: "01COMPAT", Stage: "translate"}), stats)
	if _, err := e.c.Complete(WithTarget(ctx, snap), model.RoleTranslate, "SYS", "USR", true); err != nil {
		t.Fatal(err)
	}
	raw, recs := readCalls(t, e.set.Config().DataDir)
	var log []string
	for _, r := range recs {
		log = append(log, fmt.Sprintf("%s/attempt=%d/reason=%q/max_tokens=%d/result=%s/status=%d", r.Event, r.Attempt, r.Reason, r.MaxTokens, r.Result, r.Status))
	}
	wantLog := []string{
		`start/attempt=0/reason=""/max_tokens=3000/result=/status=0`,
		`end/attempt=0/reason=""/max_tokens=0/result=http_error/status=400`,
		`start/attempt=1/reason="http 400: max_tokens not supported, sent again with max_completion_tokens"/max_tokens=3000/result=/status=0`,
		`end/attempt=1/reason=""/max_tokens=0/result=ok/status=200`,
	}
	if !slices.Equal(log, wantLog) {
		t.Errorf("call log\n %q\nwant\n %q", log, wantLog)
	}
	var evs []string
	for _, ev := range stats.Events {
		evs = append(evs, fmt.Sprintf("%s/N=%d/outcome=%s/status=%d", ev.Phase, ev.N, ev.Outcome, ev.Status))
	}
	if len(evs) != 4 || stats.Stats.Requests != 1 {
		t.Errorf("events %v requests %d, want 4 events (two attempts) and 1 request", evs, stats.Stats.Requests)
	}
	noSecrets(t, raw)
}

// A server that echoes the key in its 400 answers: neither the call's error,
// the probe's error, the call log, the debug log nor slog holds the key.
func TestCompatRefusalsKeepTheKeyOut(t *testing.T) {
	logs := captureSlog(t)
	errMT := `{"error":{"message":"Unsupported parameter: 'max_tokens' (request from ` + testKey + `)","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`
	errMCT := `{"error":{"message":"Unsupported parameter: 'max_completion_tokens' (request from ` + testKey + `)","type":"invalid_request_error","param":"max_completion_tokens","code":"unsupported_parameter"}}`
	errTemp := `{"error":{"message":"Unsupported value: 'temperature' (request from ` + testKey + `)","type":"invalid_request_error","param":"temperature","code":"unsupported_value"}}`
	e := newPoolEnv(t, func(req jsonReq) (int, string) {
		switch {
		case reqHas(req, fieldMaxTokens):
			return http.StatusBadRequest, errMT
		case reqModel(req) == "both":
			return http.StatusBadRequest, errMCT
		case reqHas(req, fieldTemperature):
			return http.StatusBadRequest, errTemp
		}
		return http.StatusOK, okBody("fine")
	})
	e.set.Config().LogLevel = "debug" // the client's config: the debug log too
	relay := e.provider("Relay", model.ProviderOpenAI, "")
	ctx := sched.WithJob(context.Background(), model.Job{ID: "01KEYJOB", Stage: "summary"})
	call := func(m string) error {
		snap := model.LLMSnapshot{ProviderID: relay.ID, ProviderName: relay.Name, Model: m, Params: model.RoleParams{MaxTokens: ip(1000)}}
		_, err := e.c.Complete(WithTarget(ctx, snap), model.RoleSummary, "SYS", "USR", false)
		return err
	}
	if err := call("fine"); err != nil { // rename, drop the temperature, ok
		t.Errorf("rename and temperature: %v", err)
	}
	if err := call("both"); err == nil || !sched.IsPermanent(err) || strings.Contains(err.Error(), testKey) { // rename, refused again
		t.Errorf("both names refused: %v; want a permanent error without the key", err)
	}
	useProbeClient(t, redirectClient(t, e.url))
	pr := TestConnection(context.Background(), model.Provider{ID: "oa", Name: "OpenAI", Type: model.ProviderOpenAI, BaseURL: officialBase, APIKey: testKey}, "both")
	info, _ := json.Marshal(pr.ErrorInfo)
	if pr.OK || strings.Contains(pr.Error, testKey) || strings.Contains(string(info), testKey) {
		t.Errorf("probe %+v info %s: want a failure without the key", pr, info)
	}
	var files []string
	_ = filepath.WalkDir(e.set.Config().DataDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			files = append(files, filepath.Base(p))
			if strings.Contains(string(b), testKey) {
				t.Errorf("%s holds the key", p)
			}
		}
		return nil
	})
	if len(files) < 2 {
		t.Errorf("expected a call log and a debug log, got %v", files)
	}
	if strings.Contains(logs.String(), testKey) {
		t.Errorf("slog holds the key:\n%s", logs.String())
	}
}

// A refused temperature is learned per provider and model: every role of
// that model goes without one afterwards; another model keeps its own.
func TestRefusedTemperatureHoldsForEveryRoleOfTheModel(t *testing.T) {
	e := newPoolEnv(t, func(req jsonReq) (int, string) {
		if reqModel(req) == "o4-mini" && reqHas(req, fieldTemperature) {
			return http.StatusBadRequest, errTemperatureValue
		}
		return http.StatusOK, okBody("fine")
	})
	oa := e.provider("OpenAI", model.ProviderOpenAI, officialBase)
	var got []string
	for _, s := range []struct {
		role, model string
		params      model.RoleParams
	}{
		{model.RoleSummary, "o4-mini", model.RoleParams{}},
		{model.RoleTranslate, "o4-mini", model.RoleParams{}},
		{model.RoleRepair, "o4-mini", model.RoleParams{Temperature: f64(1)}},
		{model.RoleTranslate, "gpt-4.1", model.RoleParams{}},
	} {
		_, err := e.complete(oa, s.model, s.role, s.params, false)
		var temps []string
		for _, b := range e.sent() {
			var m map[string]any
			_ = json.Unmarshal([]byte(b), &m)
			temps = append(temps, fmt.Sprint(m["temperature"]))
		}
		got = append(got, fmt.Sprintf("%s/%s:%v err=%v", s.role, s.model, temps, err))
	}
	want := []string{
		fmt.Sprintf("summary/o4-mini:[%v <nil>] err=<nil>", model.DefaultTemperature(model.RoleSummary)),
		"translate/o4-mini:[<nil>] err=<nil>",
		"repair/o4-mini:[<nil>] err=<nil>",
		fmt.Sprintf("translate/gpt-4.1:[%v] err=<nil>", model.DefaultTemperature(model.RoleTranslate)),
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// DeepSeek with thinking on sends no temperature (as before): a 400 naming
// the temperature is the call's error after one request. A relay's model
// refusing the temperature gets the resend without it.
func TestTemperatureRefusalWithoutATemperatureSent(t *testing.T) {
	e := newPoolEnv(t, func(req jsonReq) (int, string) {
		if reqHas(req, fieldTemperature) || reqModel(req) == "deepseek-chat" {
			return http.StatusBadRequest, errTemperatureParam
		}
		return http.StatusOK, okBody("fine")
	})
	name, typ, base, key, bal := "DS", model.ProviderOpenAI, "https://api.deepseek.com", testKey, model.BalanceNone
	ds, err := e.set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key, BalanceAPI: &bal})
	if err != nil {
		t.Fatal(err)
	}
	high := model.ThinkingHigh
	_, err = e.complete(ds, "deepseek-chat", model.RoleSummary, model.RoleParams{Thinking: &high}, false)
	if got := e.sent(); len(got) != 1 || strings.Contains(got[0], `"temperature"`) || !sched.IsPermanent(err) {
		t.Errorf("deepseek thinking: sent %q err %v; want one request without a temperature and a permanent error", got, err)
	}
	relay := e.provider("Relay", model.ProviderOpenAI, "")
	_, err = e.complete(relay, "o4-mini", model.RoleSummary, model.RoleParams{}, false)
	if got := e.sent(); err != nil || len(got) != 2 || strings.Contains(got[1], `"temperature"`) {
		t.Errorf("relay: sent %q err %v; want the resend without a temperature", got, err)
	}
	e.noWaits()
}

// An Anthropic provider (always sent max_tokens) answering with an
// OpenAI-format refusal of a limit name or of the temperature is not sent
// the request again: one request, a permanent error, no wait.
func TestAnthropicIgnoresOpenAIFormatRefusals(t *testing.T) {
	for _, body := range []string{errMaxTokens, errMaxCompletionTokens, errTemperatureValue} {
		e := newCompatEnv(t, func(jsonReq) (int, string) { return http.StatusBadRequest, body })
		p := e.provider("A", model.ProviderAnthropic, "")
		for _, role := range []string{model.RoleSummary, model.RoleTranslate} {
			_, err := e.complete(p, "claude-haiku-4-5", role, model.RoleParams{MaxTokens: ip(3000), Temperature: f64(0.2)}, false)
			if got := e.sent(); len(got) != 1 || err == nil || !sched.IsPermanent(err) {
				t.Errorf("%s %s: sent %d, err %v; want 1 request and a permanent error", role, body, len(got), err)
			}
		}
		e.noWaits()
	}
}

// A DeepSeek provider edited to OpenAI's address keeps balance_api deepseek
// (the settings dialog fills it in for a new provider only). Its requests
// and its "测试连接" carry none of DeepSeek's fields: no thinking switch, no
// reasoning_effort, no janitor limit. A relay with that balance query keeps
// them, byte for byte as before.
func TestOpenAIOfficialGetsNoDeepSeekFields(t *testing.T) {
	e := newCompatEnv(t, func(jsonReq) (int, string) { return http.StatusOK, okBody(`{"ok": true}`) })
	create := func(name, base string) model.Provider {
		t.Helper()
		typ, key, bal := model.ProviderOpenAI, testKey, model.BalanceDeepSeek
		p, err := e.set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key, BalanceAPI: &bal})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	official := create("Edited DeepSeek", officialBase)
	relay := create("DeepSeek relay", "https://relay.example.net/v1")
	high := model.ThinkingHigh
	for _, tc := range []struct {
		role      string
		params    model.RoleParams
		jsonMode  bool
		relayHas  []string // in the relay's request, as before
		officialN []string // never in the request to OpenAI's own API
	}{
		{model.RoleGlossaryJanitor, model.RoleParams{}, true,
			[]string{`"max_tokens":16000`, `"thinking":{"type":"disabled"}`},
			[]string{`"thinking"`, `"reasoning_effort"`, `"max_tokens"`, `"max_completion_tokens"`}},
		{model.RoleSummary, model.RoleParams{}, false,
			[]string{`"thinking":{"type":"disabled"}`},
			[]string{`"thinking"`, `"reasoning_effort"`}},
		{model.RoleRepair, model.RoleParams{Thinking: &high}, true,
			[]string{`"thinking":{"type":"enabled"}`, `"reasoning_effort":"high"`},
			[]string{`"thinking"`, `"reasoning_effort"`}},
	} {
		if _, err := e.complete(official, "gpt-5-mini", tc.role, tc.params, tc.jsonMode); err != nil {
			t.Fatalf("%s official: %v", tc.role, err)
		}
		got := e.sent()
		if len(got) != 1 {
			t.Fatalf("%s official: sent %q, want one request", tc.role, got)
		}
		for _, f := range tc.officialN {
			if strings.Contains(got[0], f) {
				t.Errorf("%s: OpenAI's own API was sent %s: %s", tc.role, f, got[0])
			}
		}
		if _, err := e.complete(relay, "deepseek-chat", tc.role, tc.params, tc.jsonMode); err != nil {
			t.Fatalf("%s relay: %v", tc.role, err)
		}
		got = e.sent()
		if len(got) != 1 {
			t.Fatalf("%s relay: sent %q, want one request", tc.role, got)
		}
		for _, f := range tc.relayHas {
			if !strings.Contains(got[0], f) {
				t.Errorf("%s: the relay lost %s: %s", tc.role, f, got[0])
			}
		}
	}
	useProbeClient(t, redirectClient(t, e.url))
	for _, tc := range []struct {
		p    model.Provider
		want string
	}{
		{official, `{"max_completion_tokens":16,"messages":[{"role":"user","content":"ping"}],"model":"m","stream":false}`},
		{relay, `{"max_tokens":1,"messages":[{"role":"user","content":"ping"}],"model":"m","stream":false,"thinking":{"type":"disabled"}}`},
	} {
		if r := TestConnection(context.Background(), tc.p, "m"); !r.OK {
			t.Fatalf("%s: %+v", tc.p.Name, r)
		}
		if got := e.sent(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s probe: sent %q, want one %s", tc.p.Name, got, tc.want)
		}
	}
}
