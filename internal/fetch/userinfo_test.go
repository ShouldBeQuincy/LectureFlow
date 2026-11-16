// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// SEC11 info-6 / SEC16 info-9: a URL with user info ("user@",
// "user:pass@", a bare "@") is never handed over: aria2c would send it as
// Basic authentication and the guard would pass that on. Every check
// before a request refuses it as downloads.unusual_host {host}, the host
// only (the user info is in no text); an ordinary URL is not affected.

const userInfoSecret = "s3cr3t-pw"

// checkURL (Go's requests, every redirect hop of Go's, every URL for
// aria2c): with and without the look-up.
func TestUserInfoCheckURL(t *testing.T) {
	ctx := context.Background()
	ta := testPolicy(false, map[string]int{}).forTask("lms.publicuni.example")
	refusedRows := map[string]string{
		"https://user@cdn.example.com/v.mp4":                                 "cdn.example.com",
		"https://user:" + userInfoSecret + "@cdn.example.com/v.mp4":          "cdn.example.com",
		"https://:" + userInfoSecret + "@cdn.example.com/v.mp4":              "cdn.example.com",
		"https://user:@cdn.example.com:8443/v.mp4?Policy=x":                  "cdn.example.com",
		"https://@cdn.example.com/v.mp4":                                     "cdn.example.com",
		"http://user:" + userInfoSecret + "@203.0.113.10:8080/v.mp4":         "203.0.113.10",
		"http://user:" + userInfoSecret + "@[2606:4700::6810:84e5]:8080/v.m": "2606:4700::6810:84e5",
		"https://a%40b:" + userInfoSecret + "@media.example.net/seg1.ts":     "media.example.net",
		"HTTPS://USER:" + userInfoSecret + "@CDN.EXAMPLE.COM/v.mp4":          "CDN.EXAMPLE.COM",
	}
	for raw, host := range refusedRows {
		for _, resolve := range []bool{false, true} {
			err := ta.checkURL(ctx, raw, resolve)
			f := localFailure(err)
			if f == nil || f.code != CodeUnusualHost || f.params["host"] != host {
				t.Errorf("%q (resolve %v): %v, want unusual_host {host: %q}", raw, resolve, err, host)
				continue
			}
			for _, s := range []string{err.Error(), f.detail, fmt.Sprint(f.params)} {
				if strings.Contains(s, userInfoSecret) {
					t.Errorf("%q: the password is in %q", raw, s)
				}
			}
		}
	}
	// the host's own rules name the more telling reason first
	for raw, code := range map[string]string{
		"http://a:b@youtube.com/v.mp4":         CodeSiteBlocked,
		"http://evil.example@youtube.com/v":    CodeSiteBlocked,
		"http://user@127.0.0.1/v.mp4":          CodeLoopbackAddress,
		"http://user:pw@10.0.0.5/v.mp4":        CodeLocalAddress,
		"http://user:pw@239.1.1.1/v.mp4":       CodeUnusualHost,
		"http://user:pw@a;b.example.com/v.mp4": CodeUnusualHost,
	} {
		if f := localFailure(ta.checkURL(ctx, raw, true)); f == nil || f.code != code {
			t.Errorf("%q: %+v, want %s", raw, f, code)
		}
	}
	// ordinary URLs: a port, a query, an "@" in the path or the query, an
	// IPv6 literal, a public address
	for _, raw := range []string{
		"https://cdn.example.com/v.mp4",
		"https://cdn.example.com:8443/v.mp4?Policy=abc&Key-Pair-Id=K1",
		"https://cdn.example.com/p@th/v.mp4",
		"https://cdn.example.com/v.mp4?owner=a@b.example",
		"http://[2606:4700::6810:84e5]:8080/v.mp4",
		"https://203.0.113.10/v.mp4",
	} {
		for _, resolve := range []bool{false, true} {
			if err := ta.checkURL(ctx, raw, resolve); err != nil {
				t.Errorf("%q (resolve %v): %v", raw, resolve, err)
			}
		}
	}
}

// Enqueue and refresh: a track (a file, an HLS or DASH manifest) with
// user info is request.invalid {field: tracks.url, reason: unusual_host}.
func TestUserInfoTrackRefusedAtEnqueue(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	withUser := func(tr bridge.Track, ui string) bridge.Track {
		tr.URL = strings.Replace(tr.URL, "http://", "http://"+ui+"@", 1)
		return tr
	}
	isUnusual := func(err error) bool {
		var be *bridge.Error
		return errors.As(err, &be) && be.Code == bridge.CodeBadRequest && be.Params["field"] == "tracks.url" && be.Params["reason"] == "unusual_host"
	}
	for i, tr := range []bridge.Track{
		withUser(e.fileTrack(bridge.KindAudio, "/files/a.m4a"), "user:"+userInfoSecret),
		withUser(e.fileTrack(bridge.KindAudio, "/files/a.m4a"), "user"),
		withUser(e.hlsTrack(bridge.KindVideo, "/hls/master.m3u8"), "user:"+userInfoSecret),
		withUser(e.dashTrack(bridge.KindVideo, "/d/manifest.mpd", ""), ":"+userInfoSecret),
	} {
		if _, err := e.q.Enqueue(ctx, e.request(fmt.Sprint("u", i), tr)); !isUnusual(err) {
			t.Errorf("%d enqueue %q: %v", i, tr.URL, err)
		}
	}
	// refresh of a task that exists: the new tracks are refused the same
	e.media.put("/files/a.m4a", pattern(1, 1000))
	id := e.enqueue(e.request("ok", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e.wait(id, bridge.StateDone)
	ref := &bridge.RefreshRequest{Envelope: bridge.Envelope{Type: bridge.TypeRefresh, ID: "r"}, Platform: "panopto",
		SessionID: "ok", Referer: testReferer, Cookies: e.cookies(),
		Tracks: []bridge.Track{withUser(e.fileTrack(bridge.KindAudio, "/files/a.m4a"), "user:"+userInfoSecret)}}
	if _, err := e.q.Refresh(ctx, ref); !isUnusual(err) {
		t.Errorf("refresh: %v", err)
	}
	if err := e.q.local.checkTrackHosts([]liveTrack{{url: "https://user@cdn.example.com/a.mp4"}}); !isUnusual(err) {
		t.Errorf("checkTrackHosts: %v", err)
	}
	// ordinary track URLs pass
	if err := e.q.local.checkTrackHosts([]liveTrack{{url: "https://cdn.example.com:8443/a.mp4?x=a@b"}, {url: "https://[2606:4700::1]/a.mp4"}}); err != nil {
		t.Errorf("ordinary tracks: %v", err)
	}
}

// When the task runs: an HLS variant playlist (read by Go), an HLS
// segment and key, a DASH BaseURL (for aria2c) and a redirect hop
// followed by Go, each with user info, fail the task with
// downloads.unusual_host {host} before anything is requested there.
func TestUserInfoInPlaylistsAndRedirects(t *testing.T) {
	e := newQEnv(t, nil)
	hostport := strings.TrimPrefix(e.media.srv.URL, "http://")
	ui := "user:" + userInfoSecret + "@"
	e.media.put("/hls/master.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nhttp://"+ui+hostport+"/var/index.m3u8\n"))
	e.media.put("/var/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	e.media.put("/seg/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\nhttp://"+ui+hostport+"/files/seg0.ts\n#EXT-X-ENDLIST\n"))
	e.media.put("/key/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-KEY:METHOD=AES-128,URI=\"http://"+ui+hostport+
		"/files/k.bin\"\n#EXTINF:6,\nhttp://"+ui+hostport+"/files/seg1.ts\n#EXT-X-ENDLIST\n"))
	e.media.put("/d/manifest.mpd", []byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT6S"><Period><BaseURL>http://`+
		ui+hostport+`/dash/</BaseURL><AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="1"><BaseURL>v.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`))
	e.media.put("/landing.m4a", []byte("fake media"))
	e.media.put("/files/seg0.ts", []byte("fake ts"))
	e.media.put("/files/seg1.ts", []byte("fake ts"))
	e.media.put("/files/k.bin", []byte("0123456789abcdef"))
	e.media.put("/dash/v.mp4", []byte("fake mp4"))
	e.media.mu.Lock()
	e.media.redir["/files/r.m4a"] = "http://" + ui + hostport + "/landing.m4a"
	e.media.mu.Unlock()
	host := hostport[:strings.LastIndexByte(hostport, ':')]
	for i, tr := range []bridge.Track{
		e.hlsTrack(bridge.KindVideo, "/hls/master.m3u8"),     // a variant playlist, read by Go
		e.hlsTrack(bridge.KindVideo, "/seg/index.m3u8"),      // a segment for aria2c
		e.hlsTrack(bridge.KindVideo, "/key/index.m3u8"),      // a key (and its segment) for aria2c
		e.dashTrack(bridge.KindVideo, "/d/manifest.mpd", ""), // a DASH BaseURL
		e.fileTrack(bridge.KindAudio, "/files/r.m4a"),        // a redirect hop (followed by Go: the task has cookies)
	} {
		id := e.enqueue(e.request(fmt.Sprint("p", i), tr))
		info := e.wait(id, bridge.StateFailed)
		if info.Error == nil || info.Error.Code != CodeUnusualHost || info.Error.Params["host"] != host {
			t.Errorf("%d: %+v", i, info.Error)
		}
		if info.Error != nil && (strings.Contains(info.Error.Detail, userInfoSecret) || strings.Contains(fmt.Sprint(info.Error.Params), userInfoSecret)) {
			t.Errorf("%d: the password is in the failure %+v", i, info.Error)
		}
	}
	for _, p := range []string{"/var/index.m3u8", "/files/seg0.ts", "/files/seg1.ts", "/files/k.bin", "/dash/v.mp4", "/landing.m4a"} {
		if rs := e.media.requests(p); len(rs) != 0 {
			t.Errorf("%s was requested: %+v", p, rs)
		}
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 {
		t.Errorf("aria2c was given %+v", adds)
	}
	if strings.Contains(e.logs.String(), userInfoSecret) {
		t.Errorf("the password is in the log")
	}
}

// The URL handed to aria2c (resolver.final, the last check before addUri)
// and Go's redirect policy, directly: user info refused, an ordinary URL
// on the same server passes.
func TestUserInfoResolverAndRedirectHop(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	hostport := strings.TrimPrefix(e.media.srv.URL, "http://")
	s := runSnapshot{net: e.q.newTaskNet("lms.publicuni.example"), referer: testReferer}
	defer s.net.close()
	r := e.q.newResolver(s)
	for _, raw := range []string{"http://user:" + userInfoSecret + "@" + hostport + "/files/a.m4a", "http://@" + hostport + "/files/a.m4a"} {
		// (checkHost hands back the task's failure itself)
		if _, err := r.final(ctx, raw); err == nil || asFailure(err).code != CodeUnusualHost {
			t.Errorf("resolver %q: %v", raw, err)
		}
	}
	plain := "http://" + hostport + "/files/a.m4a?Signature=x&owner=a@b"
	if f, err := r.final(ctx, plain); err != nil || f != plain {
		t.Errorf("resolver, an ordinary URL: %q %v", f, err)
	}
	c := e.q.client(s)
	prev, err := http.NewRequest(http.MethodGet, e.media.url("/files/a.m4a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []string{"http://user:" + userInfoSecret + "@" + hostport + "/x", "http://user@media-01.cdn.example/x"} {
		req, err := http.NewRequest(http.MethodGet, loc, nil)
		if err != nil {
			t.Fatal(err)
		}
		if f := localFailure(c.CheckRedirect(req, []*http.Request{prev})); f == nil || f.code != CodeUnusualHost {
			t.Errorf("redirect hop %q: %+v", loc, f)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+hostport+"/x?a=b", nil)
	if err := c.CheckRedirect(req, []*http.Request{prev}); err != nil {
		t.Errorf("an ordinary hop: %v", err)
	}
}

// The guard proxy (every hop aria2c makes, redirects it follows
// included): an http request with user info, which a client sends as an
// Authorization header, is refused 403 as unusual_host {host}, and the
// server is not asked; the same URL without user info is served.
func TestGuardRefusesUserInfo(t *testing.T) {
	r := newGuardRig(t)
	port := portOfServer(r.origin)
	for _, target := range []string{
		"http://user:" + userInfoSecret + "@origin.example:" + port + "/ok",
		"http://user@origin.example:" + port + "/ok",
	} {
		for _, via := range []string{"redirect", "directly"} {
			t.Run(via, func(t *testing.T) {
				s := r.session("lms.publicuni.example")
				first := target
				if via == "redirect" {
					first = r.originURL("/redir?to=" + url.QueryEscape(target))
				}
				status, body := get(r.client(s), first)
				if status != http.StatusForbidden || strings.Contains(body, "ORIGIN") {
					t.Errorf("%s: status %d %q", target, status, body)
				}
				if code, host := refusalOf(s); code != CodeUnusualHost || host != "origin.example" {
					t.Errorf("%s: refusal %s {host: %q}", target, code, host)
				}
			})
		}
	}
	// user info in the request line itself (a client that writes it there)
	s := r.session("lms.publicuni.example")
	c, err := net.DialTimeout("tcp", s.listenHTTP, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte(s.user + ":" + s.pass))
	io.WriteString(c, "GET http://user:"+userInfoSecret+"@origin.example:"+port+"/ok HTTP/1.1\r\nHost: origin.example:"+port+
		"\r\nProxy-Authorization: Basic "+auth+"\r\nConnection: close\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	c.Close()
	if err != nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("user info in the request line: %v %v", res, err)
	}
	if code, host := refusalOf(s); code != CodeUnusualHost || host != "origin.example" {
		t.Errorf("user info in the request line: refusal %s {host: %q}", code, host)
	}
	if strings.Contains(r.logs.String(), userInfoSecret) {
		t.Errorf("the password is in the log")
	}
	// an ordinary URL (a port and a query) on the same server
	s = r.session("lms.publicuni.example")
	if status, body := get(r.client(s), "http://origin.example:"+port+"/ok?owner=a@b"); status != 200 || body != "ORIGIN" {
		t.Fatalf("an ordinary URL: %d %q", status, body)
	}
	if code, _ := refusalOf(s); code != "" {
		t.Fatalf("an ordinary URL refused: %s", code)
	}
}
