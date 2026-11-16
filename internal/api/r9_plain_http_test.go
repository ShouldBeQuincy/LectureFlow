// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
)

// plainHTTPCases are the settings page's cases of the same rule (WS-10:
// web/testdata/r8_settings_test.mjs, plainHTTPFar in web/view_settings.js),
// in their order; TestPlainHTTPFarSameTableAsWeb keeps the two lists equal.
var plainHTTPCases = []struct {
	url string
	far bool
}{
	{"http://127.0.0.1:8000/v1", false},
	{"http://192.168.1.5:8000/v1", false},
	{"http://example.com/v1", true},
	{"https://example.com/v1", false},
	{"http://localhost:11434/v1", false},
	{"http://10.0.0.2/v1", false},
	{"http://172.20.1.1/v1", false},
	{"http://172.32.0.1/v1", true},
	{"http://100.64.0.9/v1", false},
	{"http://169.254.1.1/v1", false},
	{"http://8.8.8.8/v1", true},
	{"http://[fd00::1]:8080/v1", false},
	{"http://[fe80::1]/v1", false},
	{"http://[::ffff:192.168.1.5]/v1", false},
	{"http://[2001:db8::1]/v1", true},
	{"http://gpu-box:8000/v1", false},
	{"http://gpu-box.local:8000/v1", false},
	{"http://api.example.org.:8000/v1", true},
	{"HTTP://EXAMPLE.COM/v1", true},
}

// AUD1 I-3: the diagnostics' provider row warns about plain http to a
// computer off the local network by the settings page's rule.
func TestPlainHTTPFar(t *testing.T) {
	if len(plainHTTPCases) != 19 {
		t.Fatalf("%d cases, want the settings page's 19", len(plainHTTPCases))
	}
	for _, c := range plainHTTPCases {
		if got := plainHTTPFar(c.url); got != c.far {
			t.Errorf("plainHTTPFar(%q) = %v, want %v", c.url, got, c.far)
		}
	}
	// more of the same rule, beyond the page's table
	for url, far := range map[string]bool{
		"":                              false,
		"not a url":                     false,
		"ftp://example.com/":            false,
		"http://":                       false,
		"  http://example.com/v1  ":     true,
		"http://[::1]:8080/v1":          false, // loopback (settings.IsLocal)
		"http://[::]/v1":                false,
		"http://[fec0::1]/v1":           false, // the old site-local block
		"http://[::ffff:8.8.8.8]/v1":    true,
		"http://[::ffff:100.127.0.1]/":  false,
		"http://0.0.0.0/v1":             false,
		"http://100.128.0.1/v1":         true,
		"http://172.15.0.1/v1":          true,
		"http://169.253.1.1/v1":         true,
		"http://192.169.0.1/v1":         true,
		"http://nas.home.arpa/v1":       false,
		"http://gpu.corp.internal/v1":   false,
		"http://api.localhost/v1":       false, // settings.IsLocal
		"http://user:pw@example.com/v1": true,
		"http://GPU-BOX.LOCAL./v1":      false,
	} {
		if got := plainHTTPFar(url); got != far {
			t.Errorf("plainHTTPFar(%q) = %v, want %v", url, got, far)
		}
	}
}

// SEC52 info-3 (mutation M8): the legacy configuration's providers (no
// settings service: config.toml [llm.providers]) get the same note on their
// api_key row; a row with a hint of its own keeps it.
func TestAPIKeyRowPlainHTTPHintLegacyConfig(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.Providers = map[string]config.Provider{
		"far":   {BaseURL: "http://example.com/v1", APIKey: "sk-test-r9-legacy-0000"},
		"tls":   {BaseURL: "https://example.com/v1", APIKey: "sk-test-r9-legacy-0001"},
		"lan":   {BaseURL: "http://192.168.1.5:8000/v1", APIKey: "sk-test-r9-legacy-0002"},
		"nokey": {BaseURL: "http://example.org/v1"},
		"local": {BaseURL: "http://127.0.0.1:11434/v1", Local: true},
	}
	s := &Server{cfg: cfg} // no settings service: checkAPIKey's config branch
	for id, tc := range map[string]struct{ status, detail, hint string }{
		"far":   {statusOK, "check.key_from_config", codeHintKeyPlainHTTP},
		"tls":   {statusOK, "check.key_from_config", ""},
		"lan":   {statusOK, "check.key_from_config", ""},
		"nokey": {statusError, "check.key_not_configured", "check.hint_key_config"},
		"local": {statusOK, "check.key_local_model", ""},
	} {
		c := s.checkAPIKey(diagProvider{id: id, name: id})
		hint := ""
		if c.HintInfo != nil {
			hint = c.HintInfo.Code
		}
		if c.Status != tc.status || c.DetailInfo == nil || c.DetailInfo.Code != tc.detail || hint != tc.hint {
			t.Errorf("%s: %+v, want %+v", id, c, tc)
		}
	}
}

// The settings page's test and this one read the same table: the cases of
// web/testdata/r8_settings_test.mjs's WS-10 block are plainHTTPCases, in
// the same order (a case added on either side fails here until both have
// it).
func TestPlainHTTPFarSameTableAsWeb(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "testdata", "r8_settings_test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(b), "\r\n", "\n")
	start := strings.Index(src, "const note = t('settings.provider.plain_http_note');")
	if start < 0 {
		t.Fatal("r8_settings_test.mjs: no WS-10 block")
	}
	block := src[start:]
	open := strings.Index(block, "const cases = [")
	end := strings.Index(block, "\n  ];")
	if open < 0 || end < open {
		t.Fatal("r8_settings_test.mjs: no cases table in the WS-10 block")
	}
	block = block[open:end]
	re := regexp.MustCompile(`\[\s*'([^']*)'\s*,\s*(true|false)\s*\]`)
	var web []string
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		web = append(web, m[1]+" "+m[2])
	}
	var ours []string
	for _, c := range plainHTTPCases {
		far := "false"
		if c.far {
			far = "true"
		}
		ours = append(ours, c.url+" "+far)
	}
	if strings.Join(web, "\n") != strings.Join(ours, "\n") {
		t.Fatalf("the settings page's cases differ:\nweb:\n%s\nhere:\n%s", strings.Join(web, "\n"), strings.Join(ours, "\n"))
	}
}

// The api_key: row (no network; present for every provider) of a provider
// on plain http to a computer off the local network has the hint
// check.hint_key_plain_http; its status does not change. https and an
// address on the local network have none; a row that already has a hint
// (no key) keeps that one.
func TestAPIKeyRowPlainHTTPHint(t *testing.T) {
	// the api: rows' connection test never leaves this computer here
	var tested atomic.Int32
	e := newEnv(t, func(s *Server) {
		s.testConn = func(context.Context, model.Provider, string) llm.TestResult {
			tested.Add(1)
			return llm.TestResult{OK: true, Latency: time.Millisecond}
		}
	})
	e.srv.diag.waitStartup()
	add := func(name, url, key string) {
		t.Helper()
		body := map[string]any{"name": name, "type": model.ProviderOpenAI, "base_url": url}
		if key != "" {
			body["api_key"] = key
		}
		e.call("POST", "/api/providers", body, http.StatusCreated, nil)
	}
	add("far-http", "http://example.com/v1", "sk-test-r9-far-0000")
	add("far-https", "https://example.com/v1", "sk-test-r9-tls-0000")
	add("lan-http", "http://192.168.1.5:8000/v1", "sk-test-r9-lan-0000")
	add("far-http-nokey", "http://example.org/v1", "")
	var sys System
	e.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	if tested.Load() == 0 {
		t.Fatal("the connection tests did not run")
	}
	rows := sys.Checks
	for _, tc := range []struct {
		name, status, detail, hint string
	}{
		{"far-http", statusOK, "check.key_set", codeHintKeyPlainHTTP},
		{"far-https", statusOK, "check.key_set", ""},
		{"lan-http", statusOK, "check.key_set", ""},
		{"far-http-nokey", statusError, "check.key_missing", "check.hint_key_settings"},
	} {
		c, i := rowNamed(rows, "api_key:"+tc.name)
		if i < 0 || c.Status != tc.status || c.DetailInfo == nil || c.DetailInfo.Code != tc.detail {
			t.Fatalf("%s: %+v", tc.name, c)
		}
		got := ""
		if c.HintInfo != nil {
			got = c.HintInfo.Code
		}
		if got != tc.hint {
			t.Fatalf("%s: hint %q, want %q (%+v)", tc.name, got, tc.hint, c)
		}
		if tc.hint == codeHintKeyPlainHTTP && (c.Hint == "" || !strings.Contains(c.Hint, "https")) {
			t.Fatalf("%s: hint text %q", tc.name, c.Hint)
		}
	}
	// the JSON of GET /api/system carries it in hint_info
	res, body := e.do("GET", "/api/system", nil)
	if res.StatusCode != 200 || !strings.Contains(string(body), `"hint_info":{"code":"`+codeHintKeyPlainHTTP+`"}`) {
		t.Fatalf("GET /api/system: %d %s", res.StatusCode, body)
	}
	if strings.Contains(string(body), "sk-test-r9-far-0000") {
		t.Fatal("the key is in the response")
	}
}
