// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// SEC9 info-4 (SEC8 low-1's root cause in fetch): hosts are compared by
// db.HostKey. Two foldings made different hosts look like one while net/http
// dials each by its IDNA form, another (punycode) domain: Unicode
// lower-casing turned U+0130 into an ASCII "i" ("vİdeo.example" dials
// xn--video-8fd.example, not video.example), and strings.EqualFold folds
// final sigma with sigma ("lmς.example" and "lmσ.example" dial two
// different punycode names). The task's cookies, the page's whole URL as Referer and a
// playlist's signature went there. U+017F LONG S and U+212A KELVIN SIGN,
// which EqualFold folds with "s" and "k", are not such a case: IDNA maps
// them to that very ASCII letter, the same host (SEC10 low-4); that they no
// longer match is hardening (a host that is not plain ASCII matches only
// its own bytes).

func TestCookieHeaderLookAlikeHosts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cs := []bridge.Cookie{
		{Name: "dom", Value: "fake-1", Domain: ".video.example", Path: "/"},
		{Name: "host", Value: "fake-2", Domain: "cdn.video.example", Path: "/", HostOnly: true},
		{Name: "idn", Value: "fake-3", Domain: "vİdeo.example", Path: "/"},
		{Name: "puny", Value: "fake-4", Domain: ".xn--bcher-kva.example", Path: "/"},
	}
	for u, want := range map[string]string{
		// the review's probe: no cookie for the look-alike host
		"https://cdn.vİdeo.example/a.m3u8":        "",
		"https://cdn.v%C4%B0deo.example/a.m3u8":   "",
		"https://cdn.v%c4%b0deo.example/a.m3u8":   "",
		"https://CDN.VİDEO.EXAMPLE/a.m3u8":        "",
		"https://cdn.video．example/a.m3u8":        "", // full-width dot
		"https://Key.video.example/a.m3u8":        "", // U+212A KELVIN SIGN (IDNA: key.video.example itself): hardening, a non-ASCII host matches no domain cookie
		"https://cdn.video.example.evil.example/": "",
		// ASCII hosts as before: the domain, its subdomains, any ASCII case
		"https://cdn.video.example/a.m3u8":   "dom=fake-1; host=fake-2",
		"https://CDN.Video.EXAMPLE/a.m3u8":   "dom=fake-1; host=fake-2",
		"https://video.example/a.m3u8":       "dom=fake-1",
		"https://x.cdn.video.example/a.m3u8": "dom=fake-1",
		// a cookie of a non-ASCII domain goes to exactly its own host only
		"https://vİdeo.example/a":     "idn=fake-3",
		"https://cdn.vİdeo.example/b": "",
		// punycode URLs are ASCII and match as usual; the Unicode spelling
		// of the same name is not mapped (no IDNA here) and gets none
		"https://xn--bcher-kva.example/a":     "puny=fake-4",
		"https://CDN.XN--BCHER-KVA.example/a": "puny=fake-4",
		"https://cdn.bücher.example/a":        "",
	} {
		if got := cookieHeader(u, cs, now); got != want {
			t.Errorf("%s: %q, want %q", u, got, want)
		}
	}
	for _, c := range []struct {
		host, domain string
		hostOnly     bool
		want         bool
	}{
		{"cdn.video.example", "video.example", false, true},
		{"cdn.vİdeo.example", "video.example", false, false},
		{"cdn.video.example", "vİdeo.example", false, false},
		{"cdn.vİdeo.example", "vİdeo.example", false, false}, // no suffix rule for non-ASCII
		{"vİdeo.example", "vİdeo.example", true, true},
		{"Vİdeo.example", "vİdeo.example", true, false},     // no case mapping either
		{"lmſ.example.edu", "lms.example.edu", true, false}, // U+017F LONG S (IDNA: lms.example.edu itself): hardening
		// true mismatches: different hosts once net/http maps them
		{"lmς.example", "lmσ.example", true, false},      // final sigma / sigma: equal to strings.EqualFold
		{"cdn.lmς.example", "lmσ.example", false, false}, // (and no suffix rule for either)
		{"vİdeo.example", "video.example", true, false},  // U+0130: "video" to strings.ToLower
		{"lmσ.example", "lmσ.example", true, true},       // the same bytes
	} {
		if got := domainMatch(c.host, bridge.Cookie{Domain: c.domain, HostOnly: c.hostOnly}); got != c.want {
			t.Errorf("domainMatch(%q, %q) = %v", c.host, c.domain, got)
		}
	}
}

// The Referer: the page's whole URL only for its own origin; a look-alike
// host (which strings.EqualFold took for the page's) gets the origin alone.
func TestRefererLookAlikeHosts(t *testing.T) {
	const page = "https://lms.example.edu/v?id=fake-session-1"
	for target, want := range map[string]string{
		"https://lms.example.edu/x.mp4":          page,
		"https://LMS.Example.EDU:443/x.mp4":      page,
		"https://lmſ.example.edu/x.mp4":          "https://lms.example.edu/", // U+017F LONG S (IDNA: lms.example.edu itself): hardening
		"https://lm%C5%BF.example.edu/x.mp4":     "https://lms.example.edu/",
		"https://cdn.example.edu/x.mp4":          "https://lms.example.edu/",
		"https://lms.example.edu.evil.example/x": "https://lms.example.edu/",
	} {
		if got := refererFor(page, target); got != want {
			t.Errorf("%s: Referer %q, want %q", target, got, want)
		}
	}
	// the page's origin is written by the same rule: an ASCII host
	// lower-cased, a non-ASCII host as it is (not "i" for U+0130)
	if got := refererFor("https://LMS.Example.EDU/v?id=1", "https://cdn.example.com/x"); got != "https://lms.example.edu/" {
		t.Errorf("origin of an ASCII page: %q", got)
	}
	if got := refererFor("https://LMS.VİDEO.example/v?id=1", "https://cdn.example.com/x"); got != "https://LMS.VİDEO.example/" {
		t.Errorf("origin of a non-ASCII page: %q", got)
	}
	if got := refererFor("https://lms.vİdeo.example/v?id=1", "https://lms.video.example/x"); got != "https://lms.vİdeo.example/" {
		t.Errorf("look-alike of the page: %q", got)
	}
	// true mismatches with both hosts outside ASCII: final sigma and sigma
	// (strings.EqualFold's pair) dial two punycode names
	const greek = "https://lmσ.example/v?id=fake-session-2"
	whole := cleanReferer(greek) // the page's whole URL, as written for its own origin
	if !strings.Contains(whole, "fake-session-2") {
		t.Fatalf("cleanReferer: %q", whole)
	}
	for target, want := range map[string]string{
		"https://lmσ.example/x.mp4":       whole, // the same bytes: the same origin
		"https://lm%CF%83.example/x.mp4":  whole,
		"https://lmς.example/x.mp4":       "https://lmσ.example/",
		"https://lm%CF%82.example/x.mp4":  "https://lmσ.example/",
		"https://lmσ.example:8443/x.mp4":  "https://lmσ.example/",
		"https://lms.vİdeo.example/x.mp4": "https://lmσ.example/",
	} {
		if got := refererFor(greek, target); got != want {
			t.Errorf("%s: Referer %q, want %q", target, got, want)
		}
	}
	if got := refererFor("https://lms.vİdeo.example/v?id=1", "https://lms.vİdeo.example/x"); got != cleanReferer("https://lms.vİdeo.example/v?id=1") {
		t.Errorf("a U+0130 page's own origin: %q", got)
	}
}

// A playlist's query (its CDN signature) goes only to URIs on its own
// host; a look-alike host is another host.
func TestInheritQueryLookAlikeHost(t *testing.T) {
	base, _ := url.Parse("https://cdn.key.example/v/index.m3u8?Signature=fake-sig")
	for ref, wantQuery := range map[string]bool{
		"seg1.ts":                            true,
		"https://CDN.KEY.example/v/seg2.ts":  true,
		"https://cdn.Key.example/v/seg.ts":   false, // U+212A KELVIN SIGN, folded with "k" by EqualFold (IDNA: cdn.key.example itself): hardening
		"https://cdn.keу.example/v/seg.ts":   false, // Cyrillic u
		"https://cdn.other.example/v/seg.ts": false,
	} {
		got, err := resolve(base, ref)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if has := strings.Contains(got, "fake-sig"); has != wantQuery {
			t.Errorf("%s -> %s: signature %v, want %v", ref, got, has, wantQuery)
		}
	}
	// true mismatches: a playlist on a host outside ASCII keeps its
	// signature to the same bytes; final sigma / sigma (EqualFold's pair)
	// and U+0130 / "i" are other hosts once net/http maps them
	for _, c := range []struct {
		base, ref string
		want      bool
	}{
		{"https://cdn.lmσ.example/v/index.m3u8?Signature=fake-sig", "seg1.ts", true},
		{"https://cdn.lmσ.example/v/index.m3u8?Signature=fake-sig", "https://cdn.lmσ.example/v/seg2.ts", true},
		{"https://cdn.lmσ.example/v/index.m3u8?Signature=fake-sig", "https://cdn.lmς.example/v/seg.ts", false},
		{"https://cdn.lmς.example/v/index.m3u8?Signature=fake-sig", "https://cdn.lmσ.example/v/seg.ts", false},
		{"https://cdn.vİdeo.example/v/index.m3u8?Signature=fake-sig", "https://cdn.video.example/v/seg.ts", false},
		{"https://cdn.video.example/v/index.m3u8?Signature=fake-sig", "https://cdn.vİdeo.example/v/seg.ts", false},
	} {
		b, _ := url.Parse(c.base)
		got, err := resolve(b, c.ref)
		if err != nil {
			t.Fatalf("%s: %v", c.ref, err)
		}
		if has := strings.Contains(got, "fake-sig"); has != c.want {
			t.Errorf("%s from %s -> %s: signature %v, want %v", c.ref, c.base, got, has, c.want)
		}
	}
}

// The resolver checks each host once; a look-alike host is not the host
// already checked (it shared its cache entry: the lower-cased name), so a
// look-alike that resolves to the local network is refused.
func TestResolverHostCacheLookAlike(t *testing.T) {
	lookups := map[string]int{}
	p := &addrPolicy{proxy: noProxy, lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
		lookups[host]++
		switch host {
		case "cdn.video.example":
			return []netip.Addr{netip.MustParseAddr("93.184.216.40")}, nil
		case "cdn.vİdeo.example":
			return []netip.Addr{netip.MustParseAddr("10.0.0.9")}, nil
		}
		return nil, errors.New("no such host")
	}}
	r := (&Queue{}).newResolver(runSnapshot{net: &taskNet{addr: p.forTask("")}})
	ctx := context.Background()
	if err := r.checkHost(ctx, "https://cdn.video.example/a.ts"); err != nil {
		t.Fatalf("public host: %v", err)
	}
	if err := r.checkHost(ctx, "https://CDN.Video.example/b.ts"); err != nil || lookups["cdn.video.example"] != 1 {
		t.Fatalf("ASCII case shares the entry: %v, %v", err, lookups)
	}
	if err := r.checkHost(ctx, "https://cdn.vİdeo.example/a.ts"); err == nil {
		t.Fatal("the look-alike host used the real host's cached check")
	} else if f := (*failure)(nil); !errors.As(err, &f) || f.code != CodeUnusualHost {
		t.Fatalf("the look-alike host: %v", err)
	}
	// (SEC10: a non-ASCII host is refused as written, before any lookup;
	// it was looked up and refused by its address before)
	if lookups["cdn.vİdeo.example"] != 0 {
		t.Fatalf("lookups %v", lookups)
	}
}

// proxyAddr's key is compared with the address net/http dials, by the
// same rule on both sides (proxyKey): net/http keeps an ASCII host's case
// ("Proxy.Example:3128" is dialled as written, SEC10 low-1); a non-ASCII
// proxy host is kept as written, never an ASCII host's address.
func TestProxyAddrLookAlike(t *testing.T) {
	for in, c := range map[string]struct {
		key, dialled string
		match        bool
	}{
		"http://Proxy.Example:3128": {"proxy.example:3128", "Proxy.Example:3128", true},
		"socks5://proxy.example":    {"proxy.example:1080", "proxy.example:1080", true},
		"http://prİxy.example:3128": {"prİxy.example:3128", "PRIXY.example:3128", false},
		"http://proxy.example:3128": {"proxy.example:3128", "proxy.example:8080", false},
	} {
		u, _ := url.Parse(in)
		if got := proxyAddr(u); got != c.key {
			t.Errorf("proxyAddr(%q) = %q, want %q", in, got, c.key)
		}
		if got := proxyKey(c.dialled) == proxyAddr(u); got != c.match {
			t.Errorf("%q dialled as %q: proxy %v, want %v", in, c.dialled, got, c.match)
		}
	}
}

// The transport: a proxy written with capitals is still recognised when
// net/http dials it as written, so its connection is not refused as a
// local address (it was: every request of Go's own failed when the
// user's proxy was on the local network and named in capitals).
func TestProxyCapitalsReachTheProxy(t *testing.T) {
	var got []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.String()) // a proxy gets the absolute URL
	}))
	defer proxy.Close()
	pu, _ := url.Parse(strings.Replace(proxy.URL, "127.0.0.1", "LocalHost", 1))
	p := &addrPolicy{proxy: func(*url.URL) (*url.URL, error) { return pu, nil },
		lookup: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no lookups here") }}
	tr := p.forTask("lms.publicuni.example").transport()
	defer tr.CloseIdleConnections()
	res, err := (&http.Client{Transport: tr}).Get("http://media.example.com/v.mp4")
	if err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	res.Body.Close()
	if len(got) != 1 || got[0] != "http://media.example.com/v.mp4" {
		t.Fatalf("the proxy got %v", got)
	}
}
