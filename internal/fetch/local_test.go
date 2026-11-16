// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/bridge"
)

// noProxy: the tests' URLs go direct, whatever the environment says.
func noProxy(*url.URL) (*url.URL, error) { return nil, nil }

// Security review L1: where a host is, as written.
func TestClassifyHost(t *testing.T) {
	cases := []struct {
		host  string
		class addrClass
		final bool
	}{
		{"127.0.0.1", classLoopback, true},
		{"127.5.6.7", classLoopback, true},
		{"::1", classLoopback, true},
		{"::ffff:127.0.0.1", classLoopback, true},
		{"localhost", classLoopback, true},
		{"LocalHost.", classLoopback, true},
		{"api.localhost", classLoopback, true},
		{"0.0.0.0", classUnsafe, true},
		{"0.1.2.3", classUnsafe, true},
		{"::", classUnsafe, true},
		{"2130706433", classUnsafe, true}, // 127.0.0.1 to a resolver
		{"0x7f000001", classUnsafe, true},
		{"127.1", classUnsafe, true},
		{"0177.0.0.1", classUnsafe, true},
		{"cdn.0x1f", classUnsafe, true},
		{"10.1.2.3", classLocalNet, true},
		{"172.16.0.1", classLocalNet, true},
		{"172.31.255.255", classLocalNet, true},
		{"192.168.1.1", classLocalNet, true},
		{"169.254.169.254", classLocalNet, true},
		{"fe80::1", classLocalNet, true},
		{"fe80::1%en0", classLocalNet, true},
		{"fc00::1", classLocalNet, true},
		{"fd12:3456::1", classLocalNet, true},
		{"fec0::1", classLocalNet, true},
		{"::ffff:10.0.0.1", classLocalNet, true},
		{"printer.local", classLocalNet, true},
		// SEC16 R4 / SEC19 low-5: carrier-grade NAT (Tailscale too), the
		// local network
		{"100.64.0.1", classLocalNet, true},
		{"100.127.255.254", classLocalNet, true},
		{"100.63.255.255", classPublic, true},
		{"100.128.0.1", classPublic, true},
		// multicast and the limited broadcast: never connected to, no single
		// server's address (unusual_host, SEC19 low-8)
		{"224.0.0.1", classNotUnicast, true},
		{"239.1.1.1", classNotUnicast, true},
		{"233.252.0.1", classNotUnicast, true},
		{"255.255.255.255", classNotUnicast, true},
		{"ff02::1", classNotUnicast, true},
		{"ff05::1", classNotUnicast, true},
		{"ff0e::1", classNotUnicast, true},
		// IPv6 with an IPv4 address inside: judged by that address. NAT64
		// (64:ff9b::/96 and the local-use 64:ff9b:1::/48, the address in the
		// last 32 bits): an IPv6-only network reaches public IPv4 servers
		// so, and its own network or this computer so too
		{"64:ff9b::7f00:1", classLoopback, true},
		{"64:ff9b::127.0.0.1", classLoopback, true},
		{"64:ff9b::a00:5", classLocalNet, true},
		{"64:ff9b::a9fe:a9fe", classLocalNet, true},
		{"64:ff9b::6440:1", classLocalNet, true},
		// 0.0.0.0/8 inside an IPv6 address: no server's address, not this
		// computer (unusual_host, SEC19 low-8); 0.0.0.0/8 IPv4-mapped is
		// itself (this computer to most systems)
		{"64:ff9b::0:1", classNotUnicast, true},
		{"64:ff9b::1", classNotUnicast, true},
		{"64:ff9b:1::1:203", classNotUnicast, true},               // 0.1.2.3
		{"::ffff:0:0:1", classNotUnicast, true},                   // IPv4-translated 0.0.0.1
		{"::0.1.2.3", classNotUnicast, true},                      // IPv4-compatible
		{"2002:0:1::1", classNotUnicast, true},                    // 6to4 of 0.0.1.x
		{"2002:a:b::1", classNotUnicast, true},                    // 6to4 of 0.10.0.11
		{"2001:0:0:0:8000:63bf:3fff:fdd2", classNotUnicast, true}, // Teredo: server 0.0.0.0, client 192.0.2.45
		{"2001:0:0:0:8000:63bf:80ff:fffe", classNotUnicast, true}, // Teredo: server 0.0.0.0, client 127.0.0.1
		{"::ffff:0.0.0.0", classUnsafe, true},
		{"::ffff:0.1.2.3", classUnsafe, true},
		{"64:ff9b::efff:1", classNotUnicast, true},
		{"64:ff9b::cb00:710a", classPublic, true}, // 203.0.113.10
		{"64:ff9b::808:808", classPublic, true},
		{"64:ff9b:1::7f00:1", classLoopback, true},
		{"64:ff9b:1:ab::c0a8:101", classLocalNet, true},
		{"64:ff9b:1::808:808", classPublic, true},
		// IPv4-translated (SIIT)
		{"::ffff:0:7f00:1", classLoopback, true},
		{"::ffff:0:a00:1", classLocalNet, true},
		{"::ffff:0:808:808", classPublic, true},
		// IPv4-compatible (deprecated, never routed): refused whatever is inside
		{"::127.0.0.1", classLoopback, true},
		{"::7f00:1", classLoopback, true},
		{"::10.0.0.1", classLocalNet, true},
		{"::8.8.8.8", classNotUnicast, true},
		// 6to4: the IPv4 address after 2002:
		{"2002:7f00:1::", classLoopback, true},
		{"2002:c0a8:101::1", classLocalNet, true},
		{"2002:808:808::1", classPublic, true},
		// Teredo: the server's address, and the client's (inverted)
		{"2001:0:4136:e378:8000:63bf:80ff:fffe", classLoopback, true}, // client 127.0.0.1
		{"2001:0:7f00:1:8000:63bf:3fff:fdd2", classLoopback, true},    // server 127.0.0.1
		{"2001:0:4136:e378:8000:63bf:f5ff:fffa", classLocalNet, true}, // client 10.0.0.5
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", classPublic, true},   // client 192.0.2.45
		{"172.32.0.1", classPublic, true},
		{"8.8.8.8", classPublic, true},
		{"2606:4700::6810:84e5", classPublic, true},
		{"cdn.example.edu", classPublic, false},
		{"localhost.example.edu", classPublic, false},
		{"1password.com", classPublic, false},
	}
	for _, c := range cases {
		if class, final := classifyHost(c.host); class != c.class || final != c.final {
			t.Errorf("%s: class %d final %v, want %d %v", c.host, class, final, c.class, c.final)
		}
	}
}

// testPolicy resolves the tests' names: a public page, a page inside a
// campus network, a page with both kinds of addresses, media in the campus
// network, media on the public page's own site pointed at a private
// address, and a name of this computer. Every lookup is counted.
func testPolicy(allowLoopback bool, lookups map[string]int) *addrPolicy {
	addrs := map[string][]string{
		"lms.publicuni.example":       {"93.184.216.34"},
		"opencast.campus.example.net": {"10.0.0.2"},
		"mixed.example.org":           {"93.184.216.35", "10.0.0.3"},
		"media.campus-cdn.example":    {"10.0.0.7"},
		"media.publicuni.example":     {"10.0.0.8"},
		"loop.campus-cdn.example":     {"127.0.0.1"},
		"cdn.example.com":             {"93.184.216.36"},
	}
	var mu sync.Mutex
	return &addrPolicy{allowLoopback: allowLoopback, proxy: noProxy,
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			mu.Lock()
			lookups[host]++
			mu.Unlock()
			list, ok := addrs[host]
			if !ok {
				return nil, errors.New("no such host")
			}
			var out []netip.Addr
			for _, a := range list {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, nil
		}}
}

// The rule (lead 2026-10-27): this computer never; the local network only
// for a page on the local network itself (not for a public page's own
// site either: its owner can point any of its names at a private address).
func TestTaskAddrRules(t *testing.T) {
	ctx := context.Background()
	const (
		ok       = ""
		local    = CodeLocalAddress
		loopback = CodeLoopbackAddress
	)
	cases := []struct {
		page, track string
		want        string
	}{
		{"lms.publicuni.example", "https://media.campus-cdn.example/v.mp4", local},    // public page, private media
		{"lms.publicuni.example", "http://10.1.2.3/v.mp4", local},                     // the same, by address
		{"lms.publicuni.example", "http://[fe80::1]/v.mp4", local},                    // link-local
		{"lms.publicuni.example", "http://printer.local/v.mp4", local},                // mDNS
		{"lms.publicuni.example", "https://media.publicuni.example/v.mp4", local},     // the page's own site too
		{"lms.publicuni.example", "https://cdn.example.com/v.mp4", ok},                // public media
		{"opencast.campus.example.net", "https://media.campus-cdn.example/v.mp4", ok}, // a page inside the network
		{"opencast.campus.example.net", "http://10.1.2.3/v.mp4", ok},
		{"opencast.campus.example.net", "http://169.254.169.254/latest", ok},
		{"opencast.campus.example.net", "https://loop.campus-cdn.example/v.mp4", loopback}, // never this computer
		{"opencast.campus.example.net", "http://127.0.0.1:8000/v.mp4", loopback},
		{"opencast.campus.example.net", "http://localhost/v.mp4", loopback},
		{"opencast.campus.example.net", "http://2130706433/v.mp4", loopback},
		{"opencast.campus.example.net", "http://0.0.0.0/v.mp4", loopback},
		{"10.20.30.40", "http://10.1.2.3/v.mp4", ok}, // a page on a private address
		{"wiki.local", "http://10.1.2.3/v.mp4", ok},
		{"mixed.example.org", "http://10.1.2.3/v.mp4", local}, // one public address: a public page
		{"unknown.example", "http://10.1.2.3/v.mp4", local},   // unresolvable: not on the network
		{"", "http://10.1.2.3/v.mp4", local},                  // no page
		{"127.0.0.1", "http://10.1.2.3/v.mp4", local},         // a page on this computer is not the network
	}
	for _, c := range cases {
		lookups := map[string]int{}
		ta := testPolicy(false, lookups).forTask(c.page)
		for i := 0; i < 2; i++ {
			got := ""
			if f := localFailure(ta.checkURL(ctx, c.track, true)); f != nil {
				got = f.code
			}
			if got != c.want {
				t.Errorf("page %q, %s: %q, want %q", c.page, c.track, got, c.want)
			}
		}
		if lookups[c.page] > 1 {
			t.Errorf("page %q looked up %d times", c.page, lookups[c.page])
		}
	}
	// the development switch opens loopback only
	ta := testPolicy(true, map[string]int{}).forTask("lms.publicuni.example")
	if ta.checkURL(ctx, "http://127.0.0.1:1/a", true) != nil || ta.checkURL(ctx, "https://loop.campus-cdn.example/a", true) != nil ||
		ta.checkURL(ctx, "http://10.1.2.3/a", true) == nil || ta.checkURL(ctx, "http://0.0.0.0/a", true) == nil {
		t.Fatal("loopback allowed: the other rules changed")
	}
	// by name alone (Go's requests, whose connections are checked when dialled)
	if ta := testPolicy(false, map[string]int{}).forTask("lms.publicuni.example"); ta.checkURL(ctx, "https://media.campus-cdn.example/v.mp4", false) != nil {
		t.Fatal("a name was refused before its addresses were known")
	}
	if f := localFailure(&url.Error{Op: "Get", URL: "https://intranet.example.edu/x", Err: &localAddrError{host: "10.0.0.5"}}); f == nil ||
		f.code != CodeLocalAddress || f.params["host"] != "intranet.example.edu" {
		t.Fatalf("failure %+v", f)
	}
}

// Go's own requests: the address actually dialled is checked, against the
// task's page (a name that resolves to this computer is refused like the
// address itself).
func TestGuardedTransport(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	near := testPolicy(false, map[string]int{}).forTask("opencast.campus.example.net") // even a page inside the network
	hc := &http.Client{Transport: near.transport()}
	for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
		_, err := hc.Get(u + "/x")
		if f := localFailure(err); f == nil || f.code != CodeLoopbackAddress {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if hits != 0 {
		t.Fatalf("the local server got %d requests", hits)
	}
	ok := &http.Client{Transport: testPolicy(true, map[string]int{}).forTask("").transport()}
	res, err := ok.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if hits != 1 {
		t.Fatalf("with loopback allowed: %d requests", hits)
	}
	// the dialler's check of a private address follows the page
	ctx := context.Background()
	far := testPolicy(false, map[string]int{}).forTask("lms.publicuni.example")
	if near.checkDialed(ctx, "media.campus-cdn.example", "10.0.0.7:443") != nil {
		t.Fatal("refused for a page inside the network")
	}
	if f := localFailure(far.checkDialed(ctx, "media.campus-cdn.example", "10.0.0.7:443")); f == nil || f.code != CodeLocalAddress ||
		f.params["host"] != "media.campus-cdn.example" {
		t.Fatalf("a public page: %+v", f)
	}
	for raw, want := range map[string]string{"http://proxy.school.edu:8080": "proxy.school.edu:8080",
		"http://Proxy.School.edu": "proxy.school.edu:80", "https://[fd00::1]": "[fd00::1]:443", "socks5://10.0.0.1": "10.0.0.1:1080"} {
		u, _ := url.Parse(raw)
		if got := proxyAddr(u); got != want {
			t.Errorf("proxyAddr(%s) = %s, want %s", raw, got, want)
		}
	}
}

// Enqueue and refresh refuse track URLs of this computer (request.invalid
// with the reason); the local network is judged when the task runs.
func TestEnqueueRefusesLocalTracks(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	refused := func(err error) bool {
		var be *bridge.Error
		return errors.As(err, &be) && be.Code == bridge.CodeBadRequest && be.Params["field"] == "tracks.url" &&
			be.Params["reason"] == "local_address"
	}
	for i, raw := range []string{"http://2130706433/a.mp4", "http://0.0.0.0/a.mp4", "http://[::]/a.mp4", "http://127.1/a.mp4"} {
		tr := bridge.Track{URL: raw, Kind: bridge.KindVideo, Format: bridge.FormatFile}
		if _, err := e.q.Enqueue(ctx, e.request(fmt.Sprint("s", i), tr)); !refused(err) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	// a private address waits for the page's addresses: enqueued
	priv := e.enqueue(e.request("priv", bridge.Track{URL: "http://10.1.2.3/a.mp4", Kind: bridge.KindVideo, Format: bridge.FormatFile}))
	if info := e.wait(priv, bridge.StateFailed); info.Error == nil || info.Error.Code != CodeLocalAddress {
		t.Errorf("private, public page: %+v", info.Error)
	}
	e.media.put("/files/a.m4a", pattern(1, 100))
	id := e.enqueue(e.request("ok", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e.wait(id, bridge.StateDone)
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Envelope: bridge.Envelope{Type: bridge.TypeRefresh, ID: "r"},
		Platform: "panopto", SessionID: "ok", Tracks: []bridge.Track{{URL: "http://0x7f.1/a.m4a", Kind: bridge.KindAudio,
			Format: bridge.FormatFile}}}); !refused(err) {
		t.Errorf("refresh: %v", err)
	}
	// the product: loopback too
	p := newQEnv(t, func(o *Options) { o.allowLoopback = false })
	for i, tr := range []bridge.Track{p.fileTrack(bridge.KindAudio, "/files/a.m4a"),
		{URL: "http://localhost:1/a.mp4", Kind: bridge.KindVideo, Format: bridge.FormatFile}} {
		if _, err := p.q.Enqueue(ctx, p.request(fmt.Sprint("s", i), tr)); !refused(err) {
			t.Errorf("loopback %s: %v", tr.URL, err)
		}
	}
	if rows, _ := p.opts.Store.ListDownloadTasks(ctx); len(rows) != 0 {
		t.Fatalf("refused requests left rows: %+v", rows)
	}
}

// From a public page, the URLs a playlist, an MPD or a redirect leads to
// in the local network are refused before any request: the task fails
// with downloads.local_address {host}.
func TestPlaylistAndRedirectToLocal(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/hls/master.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nhttp://10.255.255.1/v/index.m3u8\n"))
	e.media.put("/seg/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\nhttp://169.254.169.254/latest/meta-data\n#EXT-X-ENDLIST\n"))
	e.media.put("/d/manifest.mpd", []byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT6S"><Period><BaseURL>http://192.168.1.10/</BaseURL><AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="1"><BaseURL>v.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`))
	e.media.mu.Lock()
	e.media.redir["/files/r/v.mp4"] = "http://[fd00::7]/v.mp4"
	e.media.mu.Unlock()
	dash := bridge.Track{URL: e.media.url("/d/manifest.mpd") + "?Policy=" + testSig, Kind: bridge.KindVideo,
		Format: bridge.FormatDASH, Role: "dash", SizeBytes: -1}
	for i, c := range []struct {
		tr   bridge.Track
		host string
	}{
		{e.hlsTrack(bridge.KindVideo, "/hls/master.m3u8"), "10.255.255.1"},
		{e.hlsTrack(bridge.KindVideo, "/seg/index.m3u8"), "169.254.169.254"},
		{dash, "192.168.1.10"},
		{e.fileTrack(bridge.KindVideo, "/files/r/v.mp4"), "fd00::7"},
	} {
		id := e.enqueue(e.request(fmt.Sprint("s", i), c.tr))
		info := e.wait(id, bridge.StateFailed)
		if info.Error == nil || info.Error.Code != CodeLocalAddress || info.Error.Params["host"] != c.host {
			t.Errorf("%d: %+v", i, info.Error)
		}
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 {
		t.Fatalf("aria2c was given %+v", adds)
	}
}

// The lead's three cases end to end, for what aria2c gets (a task without
// cookies, whose URLs Go does not request itself): a public page with
// media in the local network is refused; a page inside the network with
// the same media downloads; a page inside the network with media on this
// computer is refused.
func TestIntranetRecordings(t *testing.T) {
	cases := []struct {
		name, page, mediaHost string
		want                  string // "" = done
	}{
		{"public page, private media", "https://lms.publicuni.example/Panopto/Pages/Viewer.aspx?id=1", "media.campus-cdn.example", CodeLocalAddress},
		{"intranet page, private media", "https://opencast.campus.example.net/engage/ui/watch.html?id=1", "media.campus-cdn.example", ""},
		{"intranet page, loopback media", "https://opencast.campus.example.net/engage/ui/watch.html?id=1", "loop.campus-cdn.example", CodeLoopbackAddress},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newQEnv(t, func(o *Options) { o.allowLoopback = false })
			lookups := map[string]int{}
			e.q.local.lookup = testPolicy(false, lookups).lookup
			e.q.local.dial = loopbackWire // where the guard proxy finds 10.0.0.7 (aria2c connects through it)
			e.media.put("/files/a.m4a", pattern(1, 5000))
			e.media.mu.Lock()
			// no cookie; no Referer either (an https page, plain http media)
			e.media.cookie, e.media.refPrefix = "", ""
			e.media.mu.Unlock()
			e.aria.mu.Lock()
			e.aria.hostsAt[c.mediaHost] = "127.0.0.1" // where the fake aria2c finds it
			e.aria.mu.Unlock()
			_, port, _ := strings.Cut(strings.TrimPrefix(e.media.srv.URL, "http://"), ":")
			req := e.request("s1", bridge.Track{URL: "http://" + c.mediaHost + ":" + port + "/files/a.m4a?Signature=" + testSig,
				Kind: bridge.KindAudio, Format: bridge.FormatFile, Role: "audio"})
			req.Referer, req.Cookies = c.page, nil
			id := e.enqueue(req)
			adds := func() []fakeAdd { a, _, _, _, _ := e.aria.snapshot(); return a }
			if c.want == "" {
				e.wait(id, bridge.StateDone)
				if len(adds()) != 1 {
					t.Fatalf("aria2c got %+v", adds())
				}
				return
			}
			info := e.wait(id, bridge.StateFailed)
			if info.Error == nil || info.Error.Code != c.want || info.Error.Params["host"] != c.mediaHost {
				t.Fatalf("%+v", info.Error)
			}
			if len(adds()) != 0 {
				t.Fatalf("aria2c was given %+v", adds())
			}
		})
	}
}

func TestDevLoopbackSwitch(t *testing.T) {
	logs := &syncBuffer{}
	o := Options{Logger: slog.New(slog.NewTextHandler(logs, nil))}
	if q := New(o); q.local.allowLoopback {
		t.Fatal("loopback allowed by default")
	}
	t.Setenv(DevLoopbackEnv, "1")
	if q := New(o); !q.local.allowLoopback || !strings.Contains(logs.String(), DevLoopbackEnv) {
		t.Fatalf("the development switch: %v\n%s", q.local.allowLoopback, logs)
	}
}

// Security review L2: the page's whole URL for its own origin only.
func TestRefererFor(t *testing.T) {
	page := "https://u:p@lms.example.edu/Panopto/Pages/Viewer.aspx?id=abc&token=T#t=10"
	full := "https://lms.example.edu/Panopto/Pages/Viewer.aspx?id=abc&token=T"
	cases := map[string]string{
		"https://lms.example.edu/media/a.mp4":      full,
		"https://LMS.example.edu:443/media/a.mp4":  full,
		"https://cdn.example.net/a.mp4":            "https://lms.example.edu/",
		"https://lms.example.edu:8443/a.mp4":       "https://lms.example.edu/",
		"http://lms.example.edu/a.mp4":             "", // to plain http from https: nothing
		"http://cdn.example.net/a.mp4":             "",
		"http://127.0.0.1:8000/a.mp4":              "https://lms.example.edu/", // loopback is trustworthy
		"http://localhost:8000/a.mp4":              "https://lms.example.edu/",
		"https://lms.example.edu.evil.test/a.mp4":  "https://lms.example.edu/",
		"https://xn--lms-example-edu.test/a.mp4":   "https://lms.example.edu/",
		"https://[2001:db8::1]:8443/a.mp4":         "https://lms.example.edu/",
		"https://lms.example.edu/Panopto/x?y=1#zz": full,
	}
	for target, want := range cases {
		if got := refererFor(page, target); got != want {
			t.Errorf("%s: %q, want %q", target, got, want)
		}
	}
	for p, want := range map[string]string{
		"http://lms.example.edu:8080/p?q": "http://lms.example.edu:8080/",
		"https://LMS.Example.edu:443/p":   "https://lms.example.edu/",
		"https://[2001:db8::2]:8443/p":    "https://[2001:db8::2]:8443/",
		"":                                "",
		"not a url":                       "",
	} {
		if got := refererFor(p, "https://cdn.example.net/a.mp4"); got != want {
			t.Errorf("page %q: %q, want %q", p, got, want)
		}
	}
}

// Each hop gets its own Referer: the media on the page's origin the whole
// page URL, the host a redirect leads to the origin only; aria2c, which
// follows redirects with the same headers, gets the URL after them. With
// cookies and without (then the page's whole URL alone makes Go resolve
// the redirect first).
func TestRefererPerHop(t *testing.T) {
	const pageTok = "PAGE_SECRET_9d1e"
	for _, withCookies := range []bool{true, false} {
		t.Run(fmt.Sprint("cookies=", withCookies), func(t *testing.T) {
			e := newQEnv(t, nil)
			other, otherURL := otherHost(t)
			page := e.media.url("/course/view?id=1&token=" + pageTok)
			origin := e.media.url("/")
			video := pattern(3, 40_000)
			other.put("/cdn/v.mp4", video)
			e.media.mu.Lock()
			e.media.refPrefix = origin
			e.media.redir["/files/r/v.mp4"] = otherURL + "/cdn/v.mp4?Sig=cdn"
			if !withCookies {
				e.media.cookie = ""
			}
			e.media.mu.Unlock()
			other.mu.Lock()
			other.refPrefix = origin
			other.mu.Unlock()
			req := e.request("s1", e.fileTrack(bridge.KindVideo, "/files/r/v.mp4"))
			req.Referer = page
			if !withCookies {
				req.Cookies = nil
			}
			e.wait(e.enqueue(req), bridge.StateDone)
			first := e.media.requests("/files/r/v.mp4")
			if len(first) == 0 {
				t.Fatal("the media URL was not resolved by Go first")
			}
			for _, r := range first {
				if r.Referer != page {
					t.Errorf("same origin: %+v", r)
				}
			}
			seen := other.requests("")
			if len(seen) < 2 {
				t.Fatalf("the other host saw %+v", seen)
			}
			for _, r := range seen {
				if r.Referer != origin || strings.Contains(r.Referer, pageTok) {
					t.Errorf("another origin: %+v", r)
				}
			}
			adds, _, _, _, _ := e.aria.snapshot()
			if len(adds) != 1 || !strings.Contains(adds[0].URI, "/cdn/v.mp4") || !strings.Contains(strings.Join(adds[0].Headers, "\n"), "Referer: "+origin) {
				t.Fatalf("aria2c got %+v", adds)
			}
		})
	}
}

// Security review L7: ffmpeg's output is the failure's detail with any URL
// in it reduced to its host and path hash.
func TestFFmpegDetailRedacted(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/files/v.mp4", pattern(2, 100))
	e.media.put("/files/a.m4a", pattern(1, 100))
	t.Setenv("LF_FAKE_FFMPEG", "failurl")
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"), e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeMergeFailed || !strings.Contains(info.Error.Detail, "cdn.example.edu") ||
		strings.Contains(info.Error.Detail, testSig) || strings.Contains(info.Error.Detail, "/s/abc") {
		t.Fatalf("%+v", info.Error)
	}
	if strings.Contains(e.logs.String(), testSig) {
		t.Fatalf("the log holds the signature:\n%s", e.logs)
	}
}
