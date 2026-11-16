// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/settings"
)

// SEC9 new-low-1: the provider dialog's "the key will be cleared" hint
// (provider_rules.js sameOrigin) and the back end's settings.SameOrigin
// (db.BaseURLOrigin, the origin a sealed key is bound to) judge base_urls
// alike. new URL() maps a non-ASCII host by IDNA, so changing only its case,
// its full-width spelling or between punycode and Unicode looked like the
// same origin to the dialog while the back end cleared the key. Each pair is
// held against the back end: answered alike (agree), or at least never "the
// same" where the back end clears the key (warnOnly: the dialog warns once
// too often where its copy of Go's url.Parse leaves a rare form "raw:").
func TestProviderSameOriginMirrorsBackend(t *testing.T) {
	const idn = "https://b\u00FCcher.example/v1"
	agree := [][2]string{
		// ASCII hosts: unchanged (case, default port, path, scheme, port)
		{"https://api.deepseek.com", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com", "HTTPS://API.DEEPSEEK.COM:443"},
		{"http://localhost:11434", "http://localhost:11434/v1"},
		{"http://localhost", "http://localhost:80/x"},
		{"https://api.deepseek.com", "http://api.deepseek.com"},
		{"https://api.deepseek.com", "https://api.deepseek.com.evil.example"},
		{"http://localhost:11434", "http://localhost:11435"},
		{"  https://llm.example.org/v1/  ", "https://LLM.Example.ORG"},
		{"not a url", "not a url"},
		{"not a url", "https://x.example"},
		{"https://api.example.com/v1/\u00E9", "https://api.example.com/v2"}, // non-ASCII path only
		// a non-ASCII host equals only its own bytes
		{idn, idn},
		{idn, "https://b\u00FCcher.example:443/v2/"},
		{idn, "https://b%C3%BCcher.example/v1"}, // the same bytes, percent-encoded
		{idn, "https://b%c3%bccher.example"},
		{idn, "https://user@b\u00FCcher.example/v1"},
		{"https://B\u00FCcher.example/v1", idn}, // ASCII case of a non-ASCII host
		{"https://B\u00DCCHER.example/v1", idn},
		{"https://xn--bcher-kva.example/v1", idn}, // punycode and Unicode spellings
		{"https://XN--BCHER-KVA.example/v1", "https://xn--bcher-kva.example"},
		{"https://\uFF41pi.example.com/v1", "https://api.example.com/v1"}, // full-width a
		{"https://api\uFF0Eexample.com/v1", "https://api.example.com/v1"}, // full-width dot
		{"https://ap\u0130.example.com/v1", "https://api.example.com/v1"}, // U+0130
		{"https://ap%C4%B0.example.com/v1", "https://ap\u0130.example.com/v1"},
		{"https://\u212Aey.example", "https://key.example"}, // U+212A KELVIN SIGN
		{idn, "http://b\u00FCcher.example/v1"},
		{idn, "https://b\u00FCcher.example:8443/v1"},
		{"https://b\u00FCcher.example:/v1", idn}, // an empty port is the default
		{"HTTPS://b\u00FCcher.example", idn},
		{"https://b\u00FCcher.example%41/v1", idn},   // Go refuses an escaped ASCII byte in a host: raw
		{"https://b\u00FCcher.example/%zz", idn},     // and a broken escape in the path
		{"https://b\u00FCcher.example/#%zz", idn},    // or the fragment
		{"https://b\u00FCcher.example:8x/v1", idn},   // or a port that is not a number
		{"https://b\u00FCcher example/v1", idn},      // or a space in the host
		{"https://u\u00E9@b\u00FCcher.example", idn}, // or non-ASCII user info
		{"https://a:b:1\u00E9/v1", "https://a:b:1\u00E9/v1"},
		{"https:/b\u00FCcher.example/v1", idn}, // no authority: raw
		{"https:///b\u00FCcher.example/v1", idn},
		{"\uFEFFhttps://b\u00FCcher.example/v1", idn}, // Go does not trim U+FEFF
		{"\u0085https://b\u00FCcher.example/v1\u3000", idn},
		{"https://b\u00FCcher.example?q=1", "https://b\u00FCcher.example/?q=2"},
		{"https://a\u00E9b%25.example", "https://a\u00E9b%25.example/x"},
		{"https://a\u00E9b%25C3%A9.example", "https://a\u00E9b%C3%A9.example"}, // "%" itself is not an escape
	}
	warnOnly := [][2]string{
		{"https://[fe80::1%25\u00E9]/v1", "https://[fe80::1%25\u00E9]/v2"},
		{"foo://\u00E9:b:1/x", "foo://\u00E9:b:1/y"},
		{"//b\u00FCcher.example/v1", "//b\u00FCcher.example/v2"},
	}
	all := append(append([][2]string{}, agree...), warnOnly...)
	got := runSameOrigin(t, all)
	for i, p := range all {
		want := settings.SameOrigin(p[0], p[1])
		if i < len(agree) && got[i] != want {
			t.Errorf("sameOrigin(%q, %q) = %v, settings.SameOrigin %v", p[0], p[1], got[i], want)
		}
		if got[i] && !want {
			t.Errorf("sameOrigin(%q, %q): no hint, but the back end clears the key", p[0], p[1])
		}
	}
	// the cases the review found: the back end clears the key, the dialog warns
	for _, p := range [][2]string{
		{"https://B\u00FCcher.example/v1", idn},
		{"https://xn--bcher-kva.example/v1", idn},
		{"https://\uFF41pi.example.com/v1", "https://api.example.com/v1"},
	} {
		if settings.SameOrigin(p[0], p[1]) {
			t.Errorf("settings.SameOrigin(%q, %q): the back end keeps the key", p[0], p[1])
		}
	}
}

// runSameOrigin answers each pair with provider_rules.js's sameOrigin
// (testdata/provider_origin_test.mjs; the pairs go on stdin, so no argument
// carries non-ASCII text).
func runSameOrigin(t *testing.T, pairs [][2]string) []bool {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/provider_origin_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	in, err := json.Marshal(pairs)
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, stderr, err := runNodeIO(t, node, "provider_origin_test.mjs", in, url)
	if err != nil {
		t.Fatalf("provider_origin_test.mjs: %v\n%s", err, stderr)
	}
	var got []bool
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(pairs) {
		t.Fatalf("provider_origin_test.mjs answered %q (%v)", out, err)
	}
	return got
}
