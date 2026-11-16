// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
)

// SEC10 info-1 and info-2: a host name that is not plain ASCII (full-width
// or circled letters, U+00AD SOFT HYPHEN, zero-width characters, a percent
// sign) is mapped by net/http's IDNA before dialling ("ｌｏｃａｌｈｏｓｔ" is
// localhost, "you­tube.com" is youtube.com), and aria2c gets it with no
// dial-time check at all. Such a host is refused outright, for the
// do-not-handle list and the local-address rules alike; ASCII (punycode)
// hosts are judged as before.

const fullwidthLocalhost = "ｌｏｃａｌｈｏｓｔ" // ｌｏｃａｌｈｏｓｔ

var unusualHosts = []string{
	fullwidthLocalhost,
	"１２７．０．０．１",      // １２７．０．０．１
	"local­host",     // soft hyphen
	"ⓛⓞⓒⓐⓛⓗⓞⓢⓣ",      // ⓛⓞⓒⓐⓛⓗⓞⓢⓣ
	"local​host",     // zero-width space
	"printer‍.local", // zero-width joiner
	"10.0.0.1​",
	"cdn.bücher.example", // a Unicode name (the extension sends xn--bcher-kva)
	"local%host",         // "%25" in a URL
}

// inURL is h written in a URL as is ("%" as "%25", which url.Parse
// decodes back).
func inURL(h string) string { return strings.ReplaceAll(h, "%", "%25") }

func TestClassifyUnusualHosts(t *testing.T) {
	for _, h := range unusualHosts {
		if class, final := classifyHost(h); class != classUnusual || !final {
			t.Errorf("classifyHost(%q) = %d %v, want unusual, final", h, class, final)
		}
	}
	// ASCII as before, punycode included; an IPv6 zone keeps its "%"
	for h, want := range map[string]addrClass{"xn--bcher-kva.example": classPublic, "LOCALHOST": classLoopback,
		"fe80::1%en0": classLocalNet, "Printer.Local.": classLocalNet} {
		if class, _ := classifyHost(h); class != want {
			t.Errorf("classifyHost(%q) = %d, want %d", h, class, want)
		}
	}
	// an unusual page host is never "on the local network"
	if testPolicy(false, map[string]int{}).forTask("wiki​.local").pageOnLocalNet(context.Background()) {
		t.Error("an unusual page host counted as the local network")
	}
}

// checkURL (Go's requests, every redirect hop, every URL for aria2c):
// refused with downloads.unusual_host, the host percent-encoded in the
// failure; a punycode URL passes; a blocked site written with these
// characters is refused too.
func TestCheckURLUnusualHosts(t *testing.T) {
	ctx := context.Background()
	lookups := map[string]int{}
	ta := testPolicy(true, lookups).forTask("lms.publicuni.example") // even with loopback allowed
	var raws []string
	for _, h := range unusualHosts {
		raws = append(raws, "http://"+inURL(h)+":8080/v.mp4", "http://"+url.PathEscape(h)+"/v.mp4")
	}
	raws = append(raws,
		"https://you­tube.com/watch?v=x", "https://www.you%C2%ADtube.com/v.mp4",
		"https://ⓨⓞⓤⓣⓤⓑⓔ.com/v.mp4", "https://www.ud​emy.com/course/x",
		"https://cdn.example.com%E2%80%8B/v.mp4")
	for _, raw := range raws {
		if _, err := url.Parse(raw); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		for _, resolve := range []bool{false, true} {
			f := localFailure(ta.checkURL(ctx, raw, resolve))
			if f == nil || f.code != CodeUnusualHost {
				t.Errorf("%q (resolve %v): %+v", raw, resolve, f)
				continue
			}
			host, _ := f.params["host"].(string)
			for i := 0; i < len(host); i++ {
				if host[i] < 0x21 || host[i] >= 0x7f {
					t.Errorf("%q: the failure's host %q is not printable ASCII", raw, host)
					break
				}
			}
			if strings.Contains(f.detail, "?") || strings.Contains(f.detail, "watch") {
				t.Errorf("%q: detail %q carries more than the host", raw, f.detail)
			}
		}
	}
	if len(lookups) != 0 {
		t.Errorf("unusual hosts were looked up: %v", lookups)
	}
	if f := localFailure(ta.checkURL(ctx, "https://you­tube.com/", false)); f == nil || f.params["host"] != "you%C2%ADtube.com" {
		t.Errorf("host param: %+v", f)
	}
	// punycode IDN hosts as before
	for _, raw := range []string{"https://xn--bcher-kva.example/v.mp4", "https://CDN.XN--BCHER-KVA.example:443/v.mp4"} {
		if err := ta.checkURL(ctx, raw, false); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// Enqueue and refresh: a page or track URL with such a host is
// request.invalid {field, reason: unusual_host} (the protocol's own code;
// the extension sends ASCII hosts only).
func TestEnqueueRefusesUnusualHosts(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	refused := func(err error, field string) bool {
		var be *bridge.Error
		return errors.As(err, &be) && be.Code == bridge.CodeBadRequest && be.Params["field"] == field &&
			be.Params["reason"] == "unusual_host"
	}
	for i, h := range unusualHosts {
		tr := bridge.Track{URL: "http://" + inURL(h) + "/a.mp4", Kind: bridge.KindVideo, Format: bridge.FormatFile}
		if _, err := e.q.Enqueue(ctx, e.request(fmt.Sprint("t", i), tr)); !refused(err, "tracks.url") {
			t.Errorf("track %q: %v", h, err)
		}
	}
	for i, raw := range []string{"https://www.you­tube.com/a.mp4", "https://www.%E2%93%A8outube.com/a.mp4"} {
		tr := bridge.Track{URL: raw, Kind: bridge.KindVideo, Format: bridge.FormatFile}
		if _, err := e.q.Enqueue(ctx, e.request(fmt.Sprint("y", i), tr)); !refused(err, "tracks.url") {
			t.Errorf("track %q: %v", raw, err)
		}
	}
	req := e.request("page", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))
	req.Referer = "https://lms.ex­ample.edu/Panopto/Pages/Viewer.aspx?id=fake"
	if _, err := e.q.Enqueue(ctx, req); !refused(err, "referer") {
		t.Errorf("page: %v", err)
	}
	ref := &bridge.RefreshRequest{Envelope: bridge.Envelope{Type: bridge.TypeRefresh, ID: "r"}, Platform: "panopto",
		SessionID: "page", Referer: testReferer, Cookies: e.cookies(),
		Tracks: []bridge.Track{{URL: "http://" + fullwidthLocalhost + "/a.mp4", Kind: bridge.KindAudio, Format: bridge.FormatFile}}}
	if _, err := e.q.Refresh(ctx, ref); !refused(err, "tracks.url") {
		t.Errorf("refresh: %v", err)
	}
	// the direct check of track hosts, should a caller skip the list
	if err := e.q.local.checkTrackHosts([]liveTrack{{url: "http://" + fullwidthLocalhost + "/a.mp4"}}); !refused(err, "tracks.url") {
		t.Errorf("checkTrackHosts: %v", err)
	}
}

// Enqueue: a track whose host is a multicast or broadcast address (no
// single server's address, classNotUnicast) is request.invalid {field,
// reason: unusual_host}, not local_address (SEC19 low-8; review R39
// suggestion 3: the branch in checkTrackHosts had no test).
func TestEnqueueRefusesMulticastAndBroadcastHosts(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	for i, raw := range []string{"http://239.1.1.1/x", "https://[ff05::1]/x", "http://255.255.255.255/x"} {
		tr := bridge.Track{URL: raw, Kind: bridge.KindVideo, Format: bridge.FormatFile}
		_, err := e.q.Enqueue(ctx, e.request(fmt.Sprint("m", i), tr))
		var be *bridge.Error
		if !errors.As(err, &be) || be.Code != bridge.CodeBadRequest || be.Params["field"] != "tracks.url" || be.Params["reason"] != "unusual_host" {
			t.Errorf("track %q: %v", raw, err)
		}
		if err := e.q.local.checkTrackHosts([]liveTrack{{url: raw}}); !errors.As(err, &be) || be.Params["reason"] != "unusual_host" {
			t.Errorf("checkTrackHosts %q: %v", raw, err)
		}
	}
}

// Playlists and redirects: an entry or a Location with such a host fails
// the task with downloads.unusual_host before anything is requested there
// (the hosts map to this computer, so nothing leaves it even if the check
// were missing).
func TestPlaylistAndRedirectToUnusualHost(t *testing.T) {
	e := newQEnv(t, nil)
	port := e.media.srv.URL[strings.LastIndexByte(e.media.srv.URL, ':')+1:]
	e.media.put("/hls/master.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nhttp://"+fullwidthLocalhost+":"+port+"/var/index.m3u8\n"))
	e.media.put("/var/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	e.media.put("/seg/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\n"+
		"http://local­host:"+port+"/files/seg0.ts\n#EXT-X-ENDLIST\n"))
	e.media.put("/landing.m4a", []byte("fake media"))
	e.media.mu.Lock()
	e.media.redir["/files/r.m4a"] = "http://" + url.PathEscape(fullwidthLocalhost) + ":" + port + "/landing.m4a"
	e.media.mu.Unlock()
	for i, tr := range []bridge.Track{
		e.hlsTrack(bridge.KindVideo, "/hls/master.m3u8"), // Go reads the variant playlist
		e.hlsTrack(bridge.KindVideo, "/seg/index.m3u8"),  // a segment for aria2c
		e.fileTrack(bridge.KindAudio, "/files/r.m4a"),    // a redirect (followed by Go: the task has cookies)
	} {
		id := e.enqueue(e.request(fmt.Sprint("u", i), tr))
		info := e.wait(id, bridge.StateFailed)
		if info.Error == nil || info.Error.Code != CodeUnusualHost {
			t.Errorf("%d: %+v", i, info.Error)
		}
	}
	for _, p := range []string{"/var/index.m3u8", "/files/seg0.ts", "/landing.m4a"} {
		if rs := e.media.requests(p); len(rs) != 0 {
			t.Errorf("%s was requested: %+v", p, rs)
		}
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 {
		t.Fatalf("aria2c was given %+v", adds)
	}
}

// SEC10 info-3: the resolver's per-host cache holds only what depends on
// the host (its addresses); whether a URL is on the do-not-handle list
// (path entries) and whether it goes through a proxy (by scheme and port)
// are judged for every URL. Before, the first URL's answer stood for every
// later URL on the same host: a path on the list, or a direct connection
// after a proxied one (never looked up), went to aria2c unchecked.
func TestResolverCacheIsPerHostOnly(t *testing.T) {
	ctx := context.Background()
	lookups := map[string]int{}
	p := &addrPolicy{
		// a proxy for https only (HTTPS_PROXY set, HTTP_PROXY not)
		proxy: func(u *url.URL) (*url.URL, error) {
			if u.Scheme == "https" {
				return url.Parse("http://proxy.school.example:3128")
			}
			return nil, nil
		},
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			lookups[host]++
			switch host {
			case "media.campus-cdn.example":
				return []netip.Addr{netip.MustParseAddr("10.0.0.7")}, nil
			case "www.linkedin.com", "cdn.example.com":
				return []netip.Addr{netip.MustParseAddr("93.184.216.36")}, nil
			}
			return nil, errors.New("no such host")
		}}
	r := (&Queue{}).newResolver(runSnapshot{net: &taskNet{addr: p.forTask("lms.publicuni.example")}})
	code := func(err error) string {
		var f *failure
		if errors.As(err, &f) {
			return f.code
		}
		if err != nil {
			return err.Error()
		}
		return ""
	}
	// a path entry of the list after another path on its host
	if err := r.checkHost(ctx, "http://www.linkedin.com/feed/a.ts"); err != nil {
		t.Fatalf("an ordinary path: %v", err)
	}
	if got := code(r.checkHost(ctx, "http://www.linkedin.com/learning/b.ts")); got != CodeSiteBlocked {
		t.Errorf("the listed path after the cached host: %q", got)
	}
	// a direct connection after a proxied one: looked up and refused
	if err := r.checkHost(ctx, "https://media.campus-cdn.example/a.ts"); err != nil || lookups["media.campus-cdn.example"] != 0 {
		t.Fatalf("proxied: %v, lookups %v", err, lookups)
	}
	if got := code(r.checkHost(ctx, "http://media.campus-cdn.example/b.ts")); got != CodeLocalAddress {
		t.Errorf("direct after proxied: %q", got)
	}
	// the addresses themselves are still looked up once per host
	for _, raw := range []string{"http://cdn.example.com/1.ts", "http://CDN.Example.com/2.ts", "http://cdn.example.com:8080/3.ts"} {
		if err := r.checkHost(ctx, raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	if lookups["cdn.example.com"] != 1 || lookups["media.campus-cdn.example"] != 1 || lookups["www.linkedin.com"] != 1 {
		t.Errorf("lookups %v", lookups)
	}
}
