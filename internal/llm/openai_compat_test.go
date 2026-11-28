// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// v3.3.1: OpenAI's own API (api.openai.com) refuses max_tokens for its
// current models ("Use 'max_completion_tokens' instead"), and some of them
// take no temperature but the default. A request to it names the output
// limit max_completion_tokens; a 400 that names the limit's field or the
// temperature as unsupported is answered once by the same request without it
// (remembered per provider and model). Every other request is byte for byte
// what it was.

const (
	officialBase = "https://api.openai.com/v1"
	// errMaxTokens is what api.openai.com answered a v3.3.0 user.
	errMaxTokens           = `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`
	errMaxCompletionTokens = `{"error":{"message":"Unsupported parameter: 'max_completion_tokens' is not supported with this model. Use 'max_tokens' instead.","type":"invalid_request_error","param":"max_completion_tokens","code":"unsupported_parameter"}}`
	errTemperatureValue    = `{"error":{"message":"Unsupported value: 'temperature' does not support 0.2 with this model. Only the default (1) value is supported.","type":"invalid_request_error","param":"temperature","code":"unsupported_value"}}`
	errTemperatureParam    = `{"error":{"message":"Unsupported parameter: 'temperature' is not supported with this model.","type":"invalid_request_error","param":"temperature","code":"unsupported_parameter"}}`
)

type jsonReq = map[string]json.RawMessage

func reqHas(req jsonReq, field string) bool {
	_, ok := req[field]
	return ok
}

func reqModel(req jsonReq) string {
	var s string
	_ = json.Unmarshal(req["model"], &s)
	return s
}

// jsonServer records every request body and answers with respond(the
// request's JSON fields); sent returns the bodies since its last call.
func jsonServer(t *testing.T, respond func(req jsonReq) (int, string)) (url string, sent func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		var req jsonReq
		_ = json.Unmarshal(b, &req)
		status, body := respond(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := bodies
		bodies = nil
		return out
	}
}

// useProbeClient sends the test's probes (TestConnection) through hc.
// Not for parallel tests: probeHTTP is the package's.
func useProbeClient(t *testing.T, hc *http.Client) {
	t.Helper()
	old := probeHTTP
	probeHTTP = hc
	t.Cleanup(func() { probeHTTP = old })
}

// compatEnv is a settings-mode client whose requests, whatever host the
// provider names, go to one jsonServer.
type compatEnv struct {
	t    *testing.T
	set  *settings.Service
	c    *HTTPClient
	url  string
	rec  *recorder
	sent func() []string
}

func newCompatEnv(t *testing.T, respond func(req jsonReq) (int, string)) *compatEnv {
	t.Helper()
	url, sent := jsonServer(t, respond)
	set := newSettings(t)
	rec := &recorder{}
	c := NewWithSettings(set, nil, &fakeRecorder{}, "", redirectClient(t, url))
	c.Sleep = rec.sleep
	return &compatEnv{t: t, set: set, c: c, url: url, rec: rec, sent: sent}
}

// provider creates a provider of type typ; baseURL "" = the test server.
func (e *compatEnv) provider(name, typ, baseURL string) model.Provider {
	e.t.Helper()
	if baseURL == "" {
		baseURL = e.url
	}
	key, balance := testKey, model.BalanceNone
	p, err := e.set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &baseURL, APIKey: &key, BalanceAPI: &balance})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *compatEnv) complete(p model.Provider, modelName, role string, params model.RoleParams, jsonMode bool) (Result, error) {
	snap := model.LLMSnapshot{ProviderID: p.ID, ProviderName: p.Name, Model: modelName, Params: params}
	return e.c.Complete(WithTarget(context.Background(), snap), role, "SYS", "USR", jsonMode)
}

func (e *compatEnv) noWaits() {
	e.t.Helper()
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	if len(e.rec.waits) != 0 {
		e.t.Errorf("backoff waits %v, want none (the server is fine)", e.rec.waits)
	}
}

func TestIsOpenAIOfficial(t *testing.T) {
	for _, tc := range []struct {
		base string
		want bool
	}{
		{"https://api.openai.com/v1", true},
		{"https://api.openai.com", true},
		{"https://API.OpenAI.com/v1/", true},
		{"https://api.openai.com:443/v1", true},
		{"https://api.openai.com./v1", true},
		{"https://eu.api.openai.com/v1", true},
		{"https://us.api.openai.com/v1", true},
		{"https://openai.com/v1", false},
		{"https://api.openai.com.example.net/v1", false},
		{"https://myapi.openai.com.example.net/v1", false},
		{"https://notapi.openai.com/v1", false},
		{"https://example.net/api.openai.com/v1", false},
		{"https://api.deepseek.com", false},
		{"http://127.0.0.1:8080/v1", false},
		{"http://localhost:11434/v1", false},
		{"api.openai.com/v1", false}, // no scheme: no URL a request could go to
		{"", false},
	} {
		if got := isOpenAIOfficial(tc.base); got != tc.want {
			t.Errorf("isOpenAIOfficial(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
}

// "测试连接" against OpenAI's own API: max_completion_tokens 16, no
// max_tokens, one request.
func TestOpenAIOfficialProbeSendsMaxCompletionTokens(t *testing.T) {
	url, sent := jsonServer(t, func(req jsonReq) (int, string) {
		if reqHas(req, fieldMaxTokens) {
			return http.StatusBadRequest, errMaxTokens
		}
		return http.StatusOK, okBody("pong")
	})
	useProbeClient(t, redirectClient(t, url))
	p := model.Provider{ID: "oa", Name: "OpenAI", Type: model.ProviderOpenAI, BaseURL: officialBase, APIKey: testKey}
	if r := TestConnection(context.Background(), p, "gpt-5-mini"); !r.OK {
		t.Fatalf("test connection: %+v", r)
	}
	want := `{"max_completion_tokens":16,"messages":[{"role":"user","content":"ping"}],"model":"gpt-5-mini","stream":false}`
	if got := sent(); len(got) != 1 || got[0] != want {
		t.Fatalf("sent %q, want one %s", got, want)
	}
}

// A 400 refusing the field the probe sent: the probe once more with the
// other name, either way round (a relay in front of OpenAI keeps
// max_tokens first).
func TestProbeAnswersARefusedTokenFieldOnce(t *testing.T) {
	const ping = `"messages":[{"role":"user","content":"ping"}],"model":"gpt-5-mini","stream":false}`
	for _, tc := range []struct {
		name, base, refused string
		first, second       string
	}{
		{"official, max_completion_tokens refused", officialBase, fieldMaxCompletionTokens, `{"max_completion_tokens":16,` + ping, `{"max_tokens":1,` + ping},
		{"relay, max_tokens refused", "", fieldMaxTokens, `{"max_tokens":1,` + ping, `{"max_completion_tokens":16,` + ping},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, sent := jsonServer(t, func(req jsonReq) (int, string) {
				switch {
				case tc.refused == fieldMaxTokens && reqHas(req, fieldMaxTokens):
					return http.StatusBadRequest, errMaxTokens
				case tc.refused == fieldMaxCompletionTokens && reqHas(req, fieldMaxCompletionTokens):
					return http.StatusBadRequest, errMaxCompletionTokens
				}
				return http.StatusOK, okBody("pong")
			})
			useProbeClient(t, redirectClient(t, url))
			base := tc.base
			if base == "" {
				base = url
			}
			p := model.Provider{ID: "p", Name: "P", Type: model.ProviderOpenAI, BaseURL: base, APIKey: testKey}
			if r := TestConnection(context.Background(), p, "gpt-5-mini"); !r.OK || r.Latency <= 0 {
				t.Fatalf("test connection: %+v", r)
			}
			if got := sent(); len(got) != 2 || got[0] != tc.first || got[1] != tc.second {
				t.Fatalf("sent %q, want %s then %s", got, tc.first, tc.second)
			}
		})
	}
}

// Any other 400 of the probe is its result, as before: one request.
func TestProbeOtherBadRequestsAreNotSentAgain(t *testing.T) {
	notFound := `{"error":{"message":"The model 'gpt-5-mini' does not exist or you do not have access to it.","type":"invalid_request_error","param":null,"code":"model_not_found"}}`
	for _, tc := range []struct {
		name    string
		respond func(req jsonReq) (int, string)
		posts   int
	}{
		{"unknown model", func(jsonReq) (int, string) { return http.StatusBadRequest, notFound }, 1},
		// OpenAI's own API is sent max_completion_tokens.
		{"a field that was not sent", func(jsonReq) (int, string) { return http.StatusBadRequest, errMaxTokens }, 1},
		{"both names refused", func(req jsonReq) (int, string) {
			if reqHas(req, fieldMaxTokens) {
				return http.StatusBadRequest, errMaxTokens
			}
			return http.StatusBadRequest, errMaxCompletionTokens
		}, 2},
		{"not OpenAI's error format", func(jsonReq) (int, string) { return http.StatusBadRequest, `{"error":"bad request"}` }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, sent := jsonServer(t, tc.respond)
			useProbeClient(t, redirectClient(t, url))
			p := model.Provider{ID: "oa", Name: "OpenAI", Type: model.ProviderOpenAI, BaseURL: officialBase, APIKey: testKey}
			r := TestConnection(context.Background(), p, "gpt-5-mini")
			if r.OK || testCode(r) != CodeProbeHTTP {
				t.Fatalf("test connection: %+v, want %s", r, CodeProbeHTTP)
			}
			if got := sent(); len(got) != tc.posts {
				t.Fatalf("sent %d requests %q, want %d", len(got), got, tc.posts)
			}
		})
	}
}

// Every other provider's probe is byte for byte what v3.3.0 sent.
func TestProbeBodiesOfOtherProvidersUnchanged(t *testing.T) {
	url, sent := jsonServer(t, func(jsonReq) (int, string) { return http.StatusOK, okBody("pong") })
	useProbeClient(t, redirectClient(t, url))
	const plain = `{"max_tokens":1,"messages":[{"role":"user","content":"ping"}],"model":"m","stream":false}`
	for _, tc := range []struct {
		p    model.Provider
		want string
	}{
		{model.Provider{ID: "ds", Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com", APIKey: testKey},
			`{"max_tokens":1,"messages":[{"role":"user","content":"ping"}],"model":"m","stream":false,"thinking":{"type":"disabled"}}`},
		{model.Provider{ID: "local", Name: "Local", Type: model.ProviderOpenAI, BaseURL: url, APIKey: testKey}, plain},
		{model.Provider{ID: "proxy", Name: "Proxy", Type: model.ProviderOpenAI, BaseURL: "https://openai.example.net/v1", APIKey: testKey}, plain},
		{model.Provider{ID: "claude", Name: "Claude", Type: model.ProviderAnthropic, BaseURL: "https://api.anthropic.com", APIKey: testKey},
			`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`},
	} {
		if r := TestConnection(context.Background(), tc.p, "m"); !r.OK {
			t.Fatalf("%s: %+v", tc.p.Name, r)
		}
		if got := sent(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: sent %q, want one %s", tc.p.Name, got, tc.want)
		}
	}
}

// A role's request to OpenAI's own API names a user's output limit
// max_completion_tokens, and so does each rung of the truncation ladder.
func TestOpenAIOfficialRoleRequestUsesMaxCompletionTokens(t *testing.T) {
	var cutNext atomic.Bool
	e := newCompatEnv(t, func(req jsonReq) (int, string) {
		if reqHas(req, fieldMaxTokens) {
			return http.StatusBadRequest, errMaxTokens
		}
		if cutNext.CompareAndSwap(true, false) {
			return http.StatusOK, lengthBody(`{"a":`)
		}
		return http.StatusOK, okBody(`{"ok": true}`)
	})
	oa := e.provider("OpenAI", model.ProviderOpenAI, officialBase)
	params := model.RoleParams{MaxTokens: ip(3000), Temperature: f64(0.1)}
	head := `{"model":"gpt-5-mini","messages":[{"role":"system","content":"SYS"},{"role":"user","content":"USR"}],"temperature":0.1,"response_format":{"type":"json_object"},"stream":false,`

	if _, err := e.complete(oa, "gpt-5-mini", model.RoleRepair, params, true); err != nil {
		t.Fatal(err)
	}
	if got, want := e.sent(), []string{head + `"max_completion_tokens":3000}`}; !slices.Equal(got, want) {
		t.Fatalf("sent\n %q\nwant\n %q", got, want)
	}

	cutNext.Store(true)
	if _, err := e.complete(oa, "gpt-5-mini", model.RoleRepair, params, true); err != nil {
		t.Fatal(err)
	}
	if got, want := e.sent(), []string{head + `"max_completion_tokens":3000}`, head + `"max_completion_tokens":6000}`}; !slices.Equal(got, want) {
		t.Fatalf("truncation ladder: sent\n %q\nwant\n %q", got, want)
	}
	e.noWaits()
}

// Legacy mode (config.toml providers) with OpenAI's own base_url: the same.
func TestOpenAIOfficialLegacyModeUsesMaxCompletionTokens(t *testing.T) {
	url, sent := jsonServer(t, func(req jsonReq) (int, string) {
		if reqHas(req, fieldMaxTokens) {
			return http.StatusBadRequest, errMaxTokens
		}
		return http.StatusOK, okBody(`{"ok": true}`)
	})
	c := New(testConfig(t, officialBase), nil, "", redirectClient(t, url))
	rec := &recorder{}
	c.Sleep = rec.sleep
	snap := model.LLMSnapshot{ProviderID: "deepseek", Model: "gpt-5-mini", Params: model.RoleParams{MaxTokens: ip(2000)}}
	if _, err := c.Complete(WithTarget(context.Background(), snap), model.RoleSummary, "S", "U", false); err != nil {
		t.Fatal(err)
	}
	want := `{"model":"gpt-5-mini","messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}],"temperature":0.2,"stream":false,"max_completion_tokens":2000}`
	if got := sent(); len(got) != 1 || got[0] != want {
		t.Fatalf("sent %q, want one %s", got, want)
	}
}

// A provider other than OpenAI's own API (here a relay in front of it)
// keeps max_tokens. A 400 refusing it is answered once with
// max_completion_tokens, with no backoff wait, and remembered for that
// provider and model; another model, or another provider, still starts with
// max_tokens.
func TestRefusedMaxTokensSwitchesOncePerProviderAndModel(t *testing.T) {
	e := newCompatEnv(t, func(req jsonReq) (int, string) {
		if reqModel(req) == "gpt-5-mini" && reqHas(req, fieldMaxTokens) {
			return http.StatusBadRequest, errMaxTokens
		}
		return http.StatusOK, okBody(`{"ok": true}`)
	})
	relay := e.provider("Relay", model.ProviderOpenAI, "")
	relay2 := e.provider("Relay 2", model.ProviderOpenAI, "")
	params := model.RoleParams{MaxTokens: ip(3000), Temperature: f64(0.3)}
	head := `{"model":"%s","messages":[{"role":"system","content":"SYS"},{"role":"user","content":"USR"}],"temperature":0.3,"response_format":{"type":"json_object"},"stream":false,`
	mt := func(m string) string { return fmt.Sprintf(head, m) + `"max_tokens":3000}` }
	mct := func(m string) string { return fmt.Sprintf(head, m) + `"max_completion_tokens":3000}` }
	for i, s := range []struct {
		p     model.Provider
		model string
		want  []string
	}{
		{relay, "gpt-5-mini", []string{mt("gpt-5-mini"), mct("gpt-5-mini")}},
		{relay, "gpt-5-mini", []string{mct("gpt-5-mini")}},
		{relay, "qwen-max", []string{mt("qwen-max")}},
		{relay2, "gpt-5-mini", []string{mt("gpt-5-mini"), mct("gpt-5-mini")}},
	} {
		res, err := e.complete(s.p, s.model, model.RoleTranslate, params, true)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if res.Text != `{"ok": true}` {
			t.Fatalf("step %d: text %q", i, res.Text)
		}
		if got := e.sent(); !slices.Equal(got, s.want) {
			t.Errorf("step %d: sent\n %q\nwant\n %q", i, got, s.want)
		}
	}
	e.noWaits()
}

// A model that takes no temperature but the default (OpenAI's reasoning
// models answer unsupported_value or unsupported_parameter): the 400 is
// answered once by the request without it, with no backoff wait; later
// requests to that provider and model go without it; other models keep the
// role's temperature.
func TestRefusedTemperatureIsDroppedOncePerProviderAndModel(t *testing.T) {
	e := newCompatEnv(t, func(req jsonReq) (int, string) {
		if reqHas(req, "temperature") {
			switch reqModel(req) {
			case "o4-mini":
				return http.StatusBadRequest, errTemperatureValue
			case "o3":
				return http.StatusBadRequest, errTemperatureParam
			}
		}
		return http.StatusOK, okBody("fine")
	})
	oa := e.provider("OpenAI", model.ProviderOpenAI, officialBase)
	msgs := `"messages":[{"role":"system","content":"SYS"},{"role":"user","content":"USR"}]`
	with := func(m string) string { return `{"model":"` + m + `",` + msgs + `,"temperature":0.2,"stream":false}` }
	without := func(m string) string { return `{"model":"` + m + `",` + msgs + `,"stream":false}` }
	for i, s := range []struct {
		model string
		want  []string
	}{
		{"o4-mini", []string{with("o4-mini"), without("o4-mini")}},
		{"o4-mini", []string{without("o4-mini")}},
		{"gpt-4.1", []string{with("gpt-4.1")}},
		{"o3", []string{with("o3"), without("o3")}},
	} {
		res, err := e.complete(oa, s.model, model.RoleSummary, model.RoleParams{}, false)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if res.Text != "fine" {
			t.Fatalf("step %d: text %q", i, res.Text)
		}
		if got := e.sent(); !slices.Equal(got, s.want) {
			t.Errorf("step %d: sent\n %q\nwant\n %q", i, got, s.want)
		}
	}
	e.noWaits()
}

// Both in one call (a relay that checks max_tokens first): each refusal is
// answered once, in turn.
func TestBothRefusalsInOneCall(t *testing.T) {
	e := newCompatEnv(t, func(req jsonReq) (int, string) {
		switch {
		case reqHas(req, fieldMaxTokens):
			return http.StatusBadRequest, errMaxTokens
		case reqHas(req, "temperature"):
			return http.StatusBadRequest, errTemperatureValue
		}
		return http.StatusOK, okBody(`{"ok": true}`)
	})
	relay := e.provider("Relay", model.ProviderOpenAI, "")
	head := `{"model":"o4-mini","messages":[{"role":"system","content":"SYS"},{"role":"user","content":"USR"}],`
	want := []string{
		head + `"temperature":0.1,"response_format":{"type":"json_object"},"stream":false,"max_tokens":3000}`,
		head + `"temperature":0.1,"response_format":{"type":"json_object"},"stream":false,"max_completion_tokens":3000}`,
		head + `"response_format":{"type":"json_object"},"stream":false,"max_completion_tokens":3000}`,
	}
	if _, err := e.complete(relay, "o4-mini", model.RoleRepair, model.RoleParams{MaxTokens: ip(3000)}, true); err != nil {
		t.Fatal(err)
	}
	if got := e.sent(); !slices.Equal(got, want) {
		t.Fatalf("sent\n %q\nwant\n %q", got, want)
	}
	e.noWaits()
}

// Every other 400 is the call's permanent error, as before (SPEC §12): one
// request, no backoff wait. So is a second refusal of the output limit after
// the switch, and any 400 to an Anthropic provider.
func TestOtherBadRequestsAreNotSentAgain(t *testing.T) {
	always := func(body string) func(jsonReq) (int, string) {
		return func(jsonReq) (int, string) { return http.StatusBadRequest, body }
	}
	limit := model.RoleParams{MaxTokens: ip(3000)}
	for _, tc := range []struct {
		name, typ, base string // base "" = the test server (not OpenAI's own API)
		model           string
		params          model.RoleParams
		respond         func(jsonReq) (int, string)
		posts           int
	}{
		{"another parameter", model.ProviderOpenAI, officialBase, "gpt-5-mini", limit,
			always(`{"error":{"message":"Invalid value for 'messages'.","type":"invalid_request_error","param":"messages","code":"invalid_value"}}`), 1},
		{"no parameter named", model.ProviderOpenAI, officialBase, "gpt-5-mini", limit,
			always(`{"error":{"message":"Bad request.","type":"invalid_request_error","param":null,"code":null}}`), 1},
		{"not OpenAI's error format", model.ProviderOpenAI, officialBase, "gpt-5-mini", limit, always(`{"error":"bad request"}`), 1},
		{"temperature out of range", model.ProviderOpenAI, officialBase, "gpt-5-mini", model.RoleParams{},
			always(`{"error":{"message":"Invalid 'temperature': decimal above maximum value. Expected a value <= 2, but got 3 instead.","type":"invalid_request_error","param":"temperature","code":"decimal_above_max_value"}}`), 1},
		// OpenAI's own API is sent max_completion_tokens; a relay with no
		// output limit set sends neither name.
		{"max_tokens named, not sent (official)", model.ProviderOpenAI, officialBase, "gpt-5-mini", limit, always(errMaxTokens), 1},
		{"max_tokens named, not sent (no limit)", model.ProviderOpenAI, "", "gpt-5-mini", model.RoleParams{}, always(errMaxTokens), 1},
		{"both names refused", model.ProviderOpenAI, officialBase, "gpt-5-mini", limit, func(req jsonReq) (int, string) {
			if reqHas(req, fieldMaxTokens) {
				return http.StatusBadRequest, errMaxTokens
			}
			return http.StatusBadRequest, errMaxCompletionTokens
		}, 2},
		{"anthropic", model.ProviderAnthropic, "", "claude-haiku-4-5", model.RoleParams{}, always(errTemperatureValue), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCompatEnv(t, tc.respond)
			p := e.provider("P", tc.typ, tc.base)
			_, err := e.complete(p, tc.model, model.RoleRepair, tc.params, true)
			if err == nil || !sched.IsPermanent(err) {
				t.Fatalf("err %v, want a permanent error", err)
			}
			if got := e.sent(); len(got) != tc.posts {
				t.Fatalf("sent %d requests %q, want %d", len(got), got, tc.posts)
			}
			e.noWaits()
		})
	}
}
