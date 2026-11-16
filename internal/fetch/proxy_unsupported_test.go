// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/bridge"
	"lectureflow/internal/selfupdate"
)

// proxyAt makes every URL of the tests go through raw ("" = none). The
// proxies of these tests are on 127.0.0.1:1, which refuses connections:
// whatever the code under test does with them, nothing leaves the computer.
func proxyAt(raw string) func(*url.URL) (*url.URL, error) {
	return func(*url.URL) (*url.URL, error) {
		if raw == "" {
			return nil, nil
		}
		return url.Parse(raw)
	}
}

// SEC16 low-1 and low-2 (user 2026-11-11): a task whose proxy aria2c
// cannot use (SOCKS: it would connect directly; https://: the proxy's
// password in the clear) fails with downloads.proxy_unsupported {scheme}
// before anything is requested or handed to aria2c: a file track, with
// cookies and without, and an HLS track, whose playlist is not read either.
func TestQueueRefusesUnsupportedProxy(t *testing.T) {
	for _, c := range []struct {
		proxy, scheme string
	}{
		{"socks5://127.0.0.1:1", "socks5"},
		{"socks5h://alice:s3cret@127.0.0.1:1", "socks5h"},
		{"SOCKS4://127.0.0.1:1", "socks4"},
		{"https://alice:s3cret@127.0.0.1:1", "https"},
	} {
		t.Run(c.scheme, func(t *testing.T) {
			e := newQEnv(t, nil)
			e.q.local.proxy = proxyAt(c.proxy)
			e.media.put("/files/a.m4a", pattern(1, 1000))
			e.media.put("/seg/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\nseg0.ts\n#EXT-X-ENDLIST\n"))
			e.media.put("/seg/seg0.ts", pattern(2, 1000))
			plain := e.request("nocookies", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))
			plain.Cookies = nil
			for i, req := range []*bridge.EnqueueRequest{
				e.request("file", e.fileTrack(bridge.KindAudio, "/files/a.m4a")),
				plain,
				e.request("hls", e.hlsTrack(bridge.KindVideo, "/seg/index.m3u8")),
			} {
				info := e.wait(e.enqueue(req), bridge.StateFailed)
				if info.Error == nil || info.Error.Code != CodeProxyUnsupported || info.Error.Params["scheme"] != c.scheme {
					t.Fatalf("%d: %+v", i, info.Error)
				}
				if s := fmt.Sprintf("%+v", info); strings.Contains(s, "s3cret") || strings.Contains(s, "alice") || strings.Contains(s, "127.0.0.1:1") {
					t.Errorf("%d: the task names the proxy: %s", i, s)
				}
			}
			if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 || e.aria.running() {
				t.Errorf("aria2c was started or given %+v", adds)
			}
			if seen := e.media.requests(""); len(seen) != 0 {
				t.Errorf("the media server was asked: %+v", seen)
			}
			if logs := e.logs.String(); strings.Contains(logs, "s3cret") || strings.Contains(logs, "alice") {
				t.Errorf("the log names the proxy's user or password:\n%s", logs)
			}
		})
	}
	// an http:// proxy and no proxy are as before (the media server plays
	// the proxy: it serves a request by its path)
	e := newQEnv(t, nil)
	e.media.put("/files/a.m4a", pattern(1, 1000))
	e.q.local.proxy = proxyAt(e.media.srv.URL)
	e.wait(e.enqueue(e.request("http", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))), bridge.StateDone)
	e.q.local.proxy = noProxy
	e.wait(e.enqueue(e.request("none", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))), bridge.StateDone)
}

// The rule itself: aria2c's proxy is Go's proxy for the URL when it is an
// http:// one; any other scheme is refused, named by its scheme alone.
func TestUpstreamProxy(t *testing.T) {
	media, _ := url.Parse("https://media.example.com/v.mp4")
	for _, c := range []struct {
		proxy  string
		want   string // the proxy aria2c's request goes through ("" = none)
		scheme string // refused: the failure's scheme
	}{
		{"", "", ""},
		{"http://proxy.example:3128", "http://proxy.example:3128", ""},
		{"HTTP://Proxy.Example", "http://Proxy.Example:80", ""},
		{"http://alice:s3cret@proxy.example:3128", "http://alice:s3cret@proxy.example:3128", ""},
		{"http://[fd00::1]:3128", "http://[fd00::1]:3128", ""},
		{"https://proxy.example", "", "https"},
		{"HTTPS://alice:s3cret@proxy.example:8443", "", "https"},
		{"socks5://proxy.example:1080", "", "socks5"},
		{"socks5h://proxy.example", "", "socks5h"},
		{"socks4a://proxy.example", "", "socks4a"},
		{"socks://proxy.example", "", "socks"},
		{"ftp://proxy.example", "", "ftp"},
		{"http://", "", ""}, // no host: no proxy (a direct download, checked as one)
	} {
		p := &addrPolicy{proxy: proxyAt(c.proxy)}
		pu, err := p.upstream(media)
		got := ""
		if pu != nil {
			got = pu.String()
		}
		if got != c.want {
			t.Errorf("%q: upstream %q, want %q", c.proxy, got, c.want)
		}
		f := proxyFailure(err)
		if (f != nil) != (c.scheme != "") {
			t.Errorf("%q: %v", c.proxy, err)
			continue
		}
		if f == nil {
			continue
		}
		if f.code != CodeProxyUnsupported || f.params["scheme"] != c.scheme || f.refresh {
			t.Errorf("%q: %+v", c.proxy, f)
		}
		for _, s := range []string{f.detail, err.Error()} {
			if strings.Contains(s, "proxy.example") || strings.Contains(s, "s3cret") || strings.Contains(s, "alice") {
				t.Errorf("%q: %q names the proxy", c.proxy, s)
			}
		}
		if asFailure(err).code != CodeProxyUnsupported {
			t.Errorf("%q: asFailure %+v", c.proxy, asFailure(err))
		}
	}
	// a scheme is letters, digits, "+", "-" and "." (url.Parse), lower-cased
	// and bounded for the message
	long := &proxyUnsupportedError{scheme: safeScheme(strings.Repeat("Socks5+", 20))}
	if s := proxyFailure(long).params["scheme"].(string); len(s) > 16 || s != strings.ToLower(s) {
		t.Errorf("scheme %q", s)
	}
	// per URL: a SOCKS proxy for https only leaves plain http alone
	p := &addrPolicy{proxy: func(u *url.URL) (*url.URL, error) {
		if u.Scheme == "https" {
			return url.Parse("socks5://127.0.0.1:1")
		}
		return nil, nil
	}}
	plain, _ := url.Parse("http://media.example.com/v.mp4")
	if _, err := p.upstream(media); proxyFailure(err) == nil {
		t.Error("https: not refused")
	}
	if pu, err := p.upstream(plain); pu != nil || err != nil {
		t.Errorf("http: %v %v", pu, err)
	}
}

// envOf is a stand-in for the environment (os.Getenv).
func envOf(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// ALL_PROXY (SEC19 info-8; the user's rule for SOCKS and https:// proxies,
// extended): LectureFlow does not read ALL_PROXY, so a SOCKS or https://
// one alone meant a silent direct connection. When the URL's own variable
// (HTTP_PROXY for http, HTTPS_PROXY for https, either case) is not set
// and ALL_PROXY (or all_proxy) is socks*:// or https://, the URL is
// refused as proxy_unsupported, named by the scheme alone. ALL_PROXY of
// any other kind (http://, no scheme) is left as it was: not read.
func TestAllProxyUnsupported(t *testing.T) {
	plain, _ := url.Parse("http://media.example.com/v.mp4")
	secure, _ := url.Parse("https://media.example.com/v.mp4")
	for _, c := range []struct {
		name          string
		env           map[string]string
		http, https   string // the refusal's scheme for each URL ("" = none)
		upHTTP, upTLS string // proxyFor's answer (the variables as net/http reads them)
	}{
		{"none", nil, "", "", "", ""},
		{"socks5", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1"}, "socks5", "socks5", "", ""},
		{"lower case", map[string]string{"all_proxy": "socks5h://alice:s3cret@127.0.0.1:1"}, "socks5h", "socks5h", "", ""},
		{"socks4, upper-case scheme", map[string]string{"ALL_PROXY": "SOCKS4://127.0.0.1:1"}, "socks4", "socks4", "", ""},
		{"socks", map[string]string{"ALL_PROXY": "socks://127.0.0.1:1"}, "socks", "socks", "", ""},
		{"https", map[string]string{"ALL_PROXY": "https://alice:s3cret@127.0.0.1:1"}, "https", "https", "", ""},
		{"a scheme with odd characters", map[string]string{"ALL_PROXY": "socks5 <b>x</b>://127.0.0.1:1"}, "socks", "socks", "", ""},
		{"http: not handled", map[string]string{"ALL_PROXY": "http://127.0.0.1:1"}, "", "", "", ""},
		{"no scheme: not handled", map[string]string{"ALL_PROXY": "127.0.0.1:1080"}, "", "", "", ""},
		{"HTTP_PROXY set: http is its", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "HTTP_PROXY": "http://127.0.0.1:1"},
			"", "socks5", "http://127.0.0.1:1", ""},
		{"https_proxy set: https is its", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "https_proxy": "http://127.0.0.1:1"},
			"socks5", "", "", "http://127.0.0.1:1"},
		{"both set", map[string]string{"all_proxy": "socks5://127.0.0.1:1", "http_proxy": "http://127.0.0.1:1",
			"HTTPS_PROXY": "http://127.0.0.1:1"}, "", "", "http://127.0.0.1:1", "http://127.0.0.1:1"},
		// NO_PROXY (SEC19 low-6): a host the user excluded is not refused,
		// by net/http's rules (a name and its subdomains, ".", "*.", a
		// port, an IP, a CIDR, "*")
		{"NO_PROXY: the host", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "NO_PROXY": "media.example.com"}, "", "", "", ""},
		{"no_proxy: the domain", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "no_proxy": "campus.edu, Example.COM"}, "", "", "", ""},
		{"NO_PROXY: .domain", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "NO_PROXY": ".example.com"}, "", "", "", ""},
		{"NO_PROXY: *.domain", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "NO_PROXY": "*.example.com"}, "", "", "", ""},
		{"NO_PROXY: *", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "NO_PROXY": "*"}, "", "", "", ""},
		{"NO_PROXY: the host on port 80", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "NO_PROXY": "media.example.com:80"}, "", "socks5", "", ""},
		{"NO_PROXY: another host", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "NO_PROXY": "other.example.com,example.org"}, "socks5", "socks5", "", ""},
		{"NO_PROXY: not a suffix of a label", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1", "NO_PROXY": "ample.com"}, "socks5", "socks5", "", ""},
	} {
		p := &addrPolicy{env: envOf(c.env), proxy: func(u *url.URL) (*url.URL, error) {
			raw := c.upHTTP
			if u.Scheme == "https" {
				raw = c.upTLS
			}
			if raw == "" {
				return nil, nil
			}
			return url.Parse(raw)
		}}
		for _, u := range []struct {
			u    *url.URL
			want string
		}{{plain, c.http}, {secure, c.https}} {
			pu, err := p.upstream(u.u)
			f := proxyFailure(err)
			got := ""
			if f != nil {
				got, _ = f.params["scheme"].(string)
				if strings.Contains(f.detail+err.Error(), "s3cret") || strings.Contains(f.detail+err.Error(), "127.0.0.1") {
					t.Errorf("%s, %s: the failure names the proxy: %v", c.name, u.u.Scheme, err)
				}
			} else if err != nil {
				t.Errorf("%s, %s: %v", c.name, u.u.Scheme, err)
			}
			if got != u.want {
				t.Errorf("%s, %s: refused %q, want %q", c.name, u.u.Scheme, got, u.want)
			}
			if f != nil && pu != nil {
				t.Errorf("%s, %s: refused and a proxy %v", c.name, u.u.Scheme, pu)
			}
		}
	}
	// addresses by IP: NO_PROXY's IP and CIDR entries; this computer is
	// never proxied (net/http's rule), so not refused either
	all := map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1"}
	for raw, noProxyList := range map[string]string{
		"http://203.0.113.5/v.mp4":   "203.0.113.0/24",
		"https://203.0.113.5/v.mp4":  "203.0.113.5",
		"http://[2001:db8::5]/v.mp4": "[2001:db8::5]:80",
		"http://127.0.0.1/v.mp4":     "",
		"http://[::1]/v.mp4":         "",
		"http://localhost/v.mp4":     "",
	} {
		env := map[string]string{"ALL_PROXY": all["ALL_PROXY"], "NO_PROXY": noProxyList}
		u, _ := url.Parse(raw)
		if _, err := (&addrPolicy{env: envOf(env), proxy: noProxy}).upstream(u); err != nil {
			t.Errorf("%s, NO_PROXY %q: %v", raw, noProxyList, err)
		}
	}
	other, _ := url.Parse("http://203.0.113.5:8080/v.mp4")
	if _, err := (&addrPolicy{env: envOf(map[string]string{"ALL_PROXY": all["ALL_PROXY"], "NO_PROXY": "203.0.113.5:80"}),
		proxy: noProxy}).upstream(other); proxyFailure(err) == nil {
		t.Errorf("NO_PROXY of another port: %v", err)
	}
	// the in-app update's policy (ignoreAllProxy): no ALL_PROXY refusal
	// (net/http does not read it either: the update goes through aria2c
	// and the guard, directly, SEC19 low-7); a SOCKS HTTP_PROXY still
	// refuses
	if _, err := (&addrPolicy{env: envOf(all), proxy: noProxy, ignoreAllProxy: true}).upstream(plain); err != nil {
		t.Errorf("the update's policy: %v", err)
	}
	if _, err := (&addrPolicy{proxy: proxyAt("socks5://127.0.0.1:1"), ignoreAllProxy: true}).upstream(plain); proxyFailure(err) == nil {
		t.Errorf("the update's policy, a SOCKS HTTP_PROXY: %v", err)
	}
	// the product's policy reads the environment itself (os.Getenv); a
	// test's stand-in proxy without an env reads none of it
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	if getenv := (&addrPolicy{}).getenv(); getenv == nil || getenv("ALL_PROXY") != "socks5://127.0.0.1:1" {
		t.Error("the product's policy does not read the environment")
	}
	if (&addrPolicy{proxy: noProxy}).getenv() != nil {
		t.Error("a stand-in proxy reads the environment")
	}
}

// The whole queue with only ALL_PROXY=socks5://: the task fails with
// downloads.proxy_unsupported before anything is requested or aria2c is
// started. The in-app update downloads with aria2c through the guard,
// directly (SEC19 low-7): net/http, the other way, does not read
// ALL_PROXY either, and would go without the guard's checks.
//
// (Public names: this computer is never proxied, so not refused either.)
func TestQueueRefusesAllProxySOCKS(t *testing.T) {
	e := newGuardQ(t, nil)
	e.q.local.proxy = noProxy
	e.q.local.env = envOf(map[string]string{"ALL_PROXY": "socks5://alice:s3cret@127.0.0.1:1", "NO_PROXY": "cdn2.example.com"})
	e.media.put("/files/a.m4a", pattern(1, 1000))
	info := e.wait(e.enqueue(e.track("all", e.mediaURL("cdn.example.com", "/files/a.m4a"))), bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeProxyUnsupported || info.Error.Params["scheme"] != "socks5" {
		t.Fatalf("%+v", info.Error)
	}
	if s := fmt.Sprintf("%+v", info) + e.logs.String(); strings.Contains(s, "s3cret") || strings.Contains(s, "alice") {
		t.Errorf("the proxy's user or password got out: %s", s)
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 || e.aria.running() {
		t.Errorf("aria2c was started or given %+v", adds)
	}
	if seen := e.media.requests(""); len(seen) != 0 {
		t.Errorf("the media server was asked: %+v", seen)
	}
	// a host NO_PROXY names: directly, through the guard (SEC19 low-6)
	e.wait(e.enqueue(e.track("noproxy", e.mediaURL("cdn2.example.com", "/files/a.m4a"))), bridge.StateDone)
	// the update: aria2c and the guard, directly (SEC19 low-7)
	srv := newUpdateServer(t)
	srv.files["/pkg.exe"] = pattern(5, 1000)
	d := e.q.UpdateDownloader().(*updateDownloader)
	rec := &recordingDownloader{}
	d.fallback = rec
	pkg := "http://cdn.example.com:" + portOfServer(srv.srv) + "/pkg.exe" // (the guard reaches it at 127.0.0.1)
	if err := d.Download(context.Background(), selfupdate.DownloadRequest{URLs: []string{pkg}, Path: updatePath(t), Size: 1000}); err != nil {
		t.Fatal(err)
	}
	if len(rec.reqs) != 0 || len(srv.headers()) == 0 {
		t.Errorf("the update: net/http %+v, the server asked %d times", rec.reqs, len(srv.headers()))
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 2 || adds[1].URI != pkg {
		t.Errorf("the update: aria2c was given %+v", adds)
	}
}

// recordingDownloader stands in for selfupdate's net/http downloader.
type recordingDownloader struct {
	mu   sync.Mutex
	reqs []selfupdate.DownloadRequest
}

func (d *recordingDownloader) Download(_ context.Context, req selfupdate.DownloadRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reqs = append(d.reqs, req)
	return nil
}

// The in-app update with a proxy aria2c cannot use: downloaded over
// net/http, which speaks SOCKS and TLS to a proxy, instead of by aria2c
// connecting directly; aria2c is not started for it.
func TestUpdateDownloaderUnsupportedProxy(t *testing.T) {
	for _, proxy := range []string{"socks5://127.0.0.1:1", "https://127.0.0.1:1"} {
		e := newQEnv(t, nil)
		e.q.local.proxy = proxyAt(proxy)
		srv := newUpdateServer(t)
		srv.files["/pkg.exe"] = pattern(5, 1000)
		d := e.q.UpdateDownloader().(*updateDownloader)
		rec := &recordingDownloader{}
		d.fallback = rec
		req := selfupdate.DownloadRequest{URLs: []string{srv.srv.URL + "/pkg.exe"}, Path: updatePath(t), Size: 1000}
		if err := d.Download(context.Background(), req); err != nil {
			t.Fatalf("%s: %v", proxy, err)
		}
		if len(rec.reqs) != 1 || rec.reqs[0].Path != req.Path {
			t.Fatalf("%s: the net/http downloader got %+v", proxy, rec.reqs)
		}
		if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 || e.aria.running() {
			t.Errorf("%s: aria2c was started or given %+v", proxy, adds)
		}
		if len(srv.headers()) != 0 {
			t.Errorf("%s: the server was asked by aria2c", proxy)
		}
	}
	// an http:// proxy: aria2c as before
	e := newQEnv(t, nil)
	srv := newUpdateServer(t)
	pkg := pattern(5, 1000)
	srv.files["/pkg.exe"] = pkg
	e.q.local.proxy = proxyAt(srv.srv.URL) // (the server plays the proxy: it serves a request by its path)
	d := e.q.UpdateDownloader().(*updateDownloader)
	rec := &recordingDownloader{}
	d.fallback = rec
	path := updatePath(t)
	if err := d.Download(context.Background(), selfupdate.DownloadRequest{URLs: []string{srv.srv.URL + "/pkg.exe"}, Path: path, Size: 1000}); err != nil {
		t.Fatal(err)
	}
	if len(rec.reqs) != 0 || !bytes.Equal(readAll(t, path), pkg) {
		t.Fatalf("an http proxy: fallback %+v", rec.reqs)
	}
}
