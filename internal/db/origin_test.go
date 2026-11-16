// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
)

// SEC8 low-1: equal origins dial the same host. Unicode lower-casing made
// U+0130 an ASCII "i" (the same binding as the ASCII host), while net/http
// dials its IDNA form, another (punycode) domain. A host with a byte outside
// ASCII now only equals the same bytes.
func TestBaseURLOriginNonASCIIHost(t *testing.T) {
	const ascii = "https://api.chatbot-ai.example/v1"
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{ascii, "https://api.chatbot-a\u0130.example/v1", false},                                       // U+0130 written as is
		{ascii, "https://api.chatbot-a%C4%B0.example/v1", false},                                       // and percent-encoded
		{ascii, "https://api.chatbot-a%c4%b0.example/v1", false},                                       // lower-case escapes
		{ascii, "HTTPS://API.CHATBOT-A\u0130.EXAMPLE/v1", false},                                       // with ASCII capitals
		{"https://\u212Aey.example", "https://key.example", false},                                     // U+212A KELVIN SIGN
		{"https://api.chatbot-a\u0130.example/v1", "https://api.chatbot-a%C4%B0.example:443/v2", true}, // the same bytes
		{"https://b\u00fccher.example/v1", "https://b\u00fccher.example:443/", true},
		{"https://B\u00dcCHER.example", "https://b\u00fccher.example", false}, // no Unicode case mapping
		{"https://b\u00fccher.example", "http://b\u00fccher.example", false},
		{"https://b\u00fccher.example", "https://b\u00fccher.example:8443", false},
		{"https://b\u00fccher.example", "https://xn--bcher-kva.example", false}, // not mapped to punycode either
	} {
		if got := BaseURLOrigin(tc.a) == BaseURLOrigin(tc.b); got != tc.want {
			t.Errorf("%q vs %q: same origin %v (%q, %q)", tc.a, tc.b, got, BaseURLOrigin(tc.a), BaseURLOrigin(tc.b))
		}
	}
	// ASCII hosts keep the bindings existing keys were sealed with
	for in, want := range map[string]string{
		"https://LLM.Example.ORG:443/v1/": "https://llm.example.org:443",
		"HTTP://Localhost:11434":          "http://localhost:11434",
		"https://api.deepseek.com.":       "https://api.deepseek.com.:443",
	} {
		if got := BaseURLOrigin(in); got != want {
			t.Errorf("BaseURLOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

// HostKey is the host part of BaseURLOrigin's rule, for internal/fetch.
func TestHostKey(t *testing.T) {
	for _, tc := range []struct {
		in, key string
		ascii   bool
	}{
		{"CDN.Video.EXAMPLE", "cdn.video.example", true},
		{"xn--bcher-kva.example", "xn--bcher-kva.example", true},
		{"cdn.vİdeo.example", "cdn.vİdeo.example", false}, // not "cdn.video.example"
		{"Key.EXAMPLE", "Key.EXAMPLE", false},             // no case mapping at all
		{"BÜcher.example", "BÜcher.example", false},
	} {
		if k, a := HostKey(tc.in); k != tc.key || a != tc.ascii {
			t.Errorf("HostKey(%q) = %q, %v; want %q, %v", tc.in, k, a, tc.key, tc.ascii)
		}
	}
}

// The look-alike base_url written into the database: the bound key does not
// open there (it did before: the same binding as the ASCII host), and opens
// again when the base_url is set back.
func TestBoundKeyDoesNotOpenOnALookAlikeHost(t *testing.T) {
	ctx := context.Background()
	withKey(t, dataKey(36))
	st := openAt(t, filepath.Join(t.TempDir(), "lectureflow.db"))
	p := model.Provider{ID: "p1", Name: "P1", Type: model.ProviderOpenAI, BaseURL: "https://api.chatbot-ai.example/v1",
		APIKey: fakeKeyA, MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceNone}
	if err := st.InsertProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	captureLogs(t)
	for _, tc := range []struct{ base, want string }{
		{"https://api.chatbot-a\u0130.example/v1", ""},
		{"https://api.chatbot-a%C4%B0.example/v1", ""},
		{"https://API.chatbot-ai.example:443/v2", fakeKeyA},
	} {
		if _, err := st.db.Exec(`UPDATE provider SET base_url = ? WHERE id = 'p1'`, tc.base); err != nil {
			t.Fatal(err)
		}
		if got, err := st.GetProvider(ctx, "p1"); err != nil || got.APIKey != tc.want {
			t.Errorf("base_url %q: key %q (%v), want %q", tc.base, got.APIKey, err, tc.want)
		}
	}
}
