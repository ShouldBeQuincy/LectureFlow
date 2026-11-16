// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// SEC11 low-1: a name with punctuation ("a;b.evil.example", "localhost,")
// or an empty label ("localhost..") was a public name; Go's resolver
// refuses it (the look-up fails, which the check lets through) while
// aria2c's own resolver may send it to DNS and connect wherever the answer
// says. Such a name is now unusual (downloads.unusual_host), refused
// before any look-up. The rows are SEC11's probe table (its last table),
// every written form in a URL as a playlist or a Location would carry it.

// probeRow: raw's host classified (classifyHost of u.Hostname()) and the
// failure code of checkURL (resolve, a public page, loopback not allowed,
// a fake resolver that knows none of these names); parseErr: url.Parse
// refuses raw (it never gets in). code "-": not asserted (SEC11 info-5,
// another task). stricter marks a row the new rule changed (it was let
// through, or refused under another code).
type probeRow struct {
	raw      string
	parseErr bool
	class    addrClass
	final    bool
	code     string
	stricter bool
}

func probeRows() []probeRow {
	var rows []probeRow
	add := func(class addrClass, final bool, code string, stricter bool, raws ...string) {
		for _, r := range raws {
			rows = append(rows, probeRow{raw: r, class: class, final: final, code: code, stricter: stricter})
		}
	}
	refused := func(raws ...string) {
		for _, r := range raws {
			rows = append(rows, probeRow{raw: r, parseErr: true})
		}
	}
	// a trailing dot (the root): the same host
	add(classPublic, false, "", false, "http://example.com./v.mp4")
	// the list strips every trailing dot and lower-cases: still the site's
	// category, whatever the host's class
	add(classPublic, false, CodeSiteBlocked, false, "http://YOUTUBE.COM./v.mp4", "http://WWW.YouTube.com/v.mp4")
	add(classUnusual, true, CodeSiteBlocked, false, "http://youtube.com../v.mp4", "http://.youtube.com/v.mp4", "http://a..youtube.com/v.mp4")
	// names of this computer
	add(classLoopback, true, CodeLoopbackAddress, false, "http://localhost./v.mp4", "http://LOCALHOST/v.mp4", "http://foo.localhost/v.mp4",
		"http://foo.localhost./v.mp4", "http://Foo.LocalHost/v.mp4", "http://localhost:0/v.mp4")
	add(classUnusual, true, CodeUnusualHost, true, "http://*.localhost/v.mp4") // was loopback (refused): now unusual
	add(classLocalNet, true, CodeLocalAddress, false, "http://printer.local./v.mp4")
	// empty labels: were public names whose look-up failed, let through
	add(classUnusual, true, CodeUnusualHost, true, "http://localhost../v.mp4", "http://foo.localhost../v.mp4",
		"http://printer.local../v.mp4", "http://127.0.0.1../v.mp4", "http://0x7f.1../v.mp4", "http://foo..bar/v.mp4",
		"http://.example.com/v.mp4", "http://./v.mp4", "http://a.b.../v.mp4")
	// numbers resolvers read as an address
	add(classUnsafe, true, CodeLoopbackAddress, false, "http://2130706433/v.mp4", "http://0x7f.1/v.mp4", "http://0X7F.1/v.mp4",
		"http://017700000001/v.mp4", "http://0177.0.0.1/v.mp4", "http://127.1/v.mp4", "http://0x7f000001/v.mp4",
		"http://127.000.000.001/v.mp4", "http://10.1/v.mp4", "http://0xa.1/v.mp4", "http://1.0x7f/v.mp4", "http://0x/v.mp4",
		"http://0x7f.1./v.mp4")
	add(classLoopback, true, CodeLoopbackAddress, false, "http://127.0.0.1./v.mp4", "http://127.0.0.1.:80/v.mp4", "http://127.0.0.1:/v.mp4")
	// IPv6 literals: their own rules, zones included
	add(classLoopback, true, CodeLoopbackAddress, false, "http://[::1]/v.mp4", "http://[::ffff:127.0.0.1]/v.mp4",
		"http://[::ffff:7f00:1]/v.mp4", "http://[0:0:0:0:0:ffff:7f00:1]/v.mp4", "http://[::1%25x]/v.mp4",
		"http://[::ffff:127.0.0.1%25x]/v.mp4", "http://[::1]:/v.mp4")
	add(classLocalNet, true, CodeLocalAddress, false, "http://[fe80::1%25eth0]/v.mp4", "http://[fe80::1%25Eth0]/v.mp4", "http://[FE80::1]/v.mp4")
	add(classUnusual, true, CodeUnusualHost, false, "http://[fe80::1%25eth%200]/v.mp4") // (SEC11 info-3, already so)
	// an IPv4 address inside (IPv4-compatible, NAT64): judged by it (SEC19
	// low-5; were public, SEC11 info-5)
	add(classLoopback, true, CodeLoopbackAddress, true, "http://[::127.0.0.1]/v.mp4", "http://[64:ff9b::7f00:1]/v.mp4")
	add(classLocalNet, true, CodeLocalAddress, true, "http://[64:ff9b::a00:1]/v.mp4")
	refused("http://[127.0.0.1]/v.mp4", "http://[youtube.com]/v.mp4", "http://[v1.x]/v.mp4")
	// user info: the host is after the last "@". A public name is refused
	// for the user info itself (SEC11 info-6 / SEC16 info-9: aria2c would
	// send it as Basic authentication; were let through); a listed site or
	// this computer keeps its own reason
	add(classPublic, false, CodeUnusualHost, false, "http://127.0.0.1@evil.example/v.mp4", "http://a@127.0.0.1@evil.example/v.mp4",
		"http://127.0.0.1:80@evil.example/v.mp4", "http://youtube.com@evil.example/v.mp4")
	add(classPublic, false, CodeSiteBlocked, false, "http://evil.example@youtube.com/v.mp4", "http://a:b@youtube.com/v.mp4")
	add(classLoopback, true, CodeLoopbackAddress, false, "http://evil.example:@127.0.0.1/v.mp4", "http://user@127.0.0.1/v.mp4",
		"http://evil.example%5C@127.0.0.1/v.mp4")
	// a backslash, control characters, escaped ASCII: never get in
	refused("http://evil.example\\@127.0.0.1/v.mp4", "http://a\\b.example/v.mp4", "http://lms%09.example/v.mp4",
		"http://lms%0A.example/v.mp4", "http://lms\x01.example/v.mp4", "http://%6c%6focalhost/v.mp4")
	// ports: the host is judged alone
	add(classPublic, false, "", false, "http://evil.example:0080/v.mp4", "http://evil.example:65536/v.mp4")
	add(classPublic, false, CodeSiteBlocked, false, "http://youtube.com:00443/v.mp4", "http://youtube.com:/v.mp4")
	// punctuation: were public names whose look-up failed, let through
	add(classUnusual, true, CodeUnusualHost, true, "http://a;b.evil.example/v.mp4", "http://127.0.0.1;.evil.example/v.mp4",
		"http://localhost;/v.mp4", "http://127.0.0.1,/v.mp4")
	for _, c := range []string{"!", "$", "&", "'", "(", ")", "*", "+", ",", "=", "~", "<", ">", `"`, ";"} {
		add(classUnusual, true, CodeUnusualHost, true, "http://media"+c+"cdn.evil.example/v.mp4", "http://localhost"+c+"/v.mp4")
	}
	// letters, digits, "-", "_": names as before
	add(classPublic, false, "", false, "http://a_b.example/v.mp4", "http://media-01.cdn_x.example/v.mp4")
	// a percent sign, Unicode: unusual as before
	add(classUnusual, true, CodeUnusualHost, false, "http://local%25host/v.mp4", "http://you%C2%ADtube.com/v.mp4",
		"http://b%C3%BCcher.example/v.mp4", "http://bücher.example/v.mp4")
	// punycode: names as before (a look-alike punycode name is another
	// domain, SEC11 info-7)
	add(classPublic, false, "", false, "http://xn--bcher-kva.example/v.mp4", "http://XN--BCHER-KVA.example/v.mp4",
		"http://xn--yutube-wqf.com/v.mp4", "http://www.xn--ytube-0cb.com/v.mp4")
	return rows
}

func TestSEC11ProbeTable(t *testing.T) {
	ctx := context.Background()
	lookups := map[string]int{}
	ta := testPolicy(false, lookups).forTask("lms.publicuni.example")
	stricter := 0
	for _, r := range probeRows() {
		u, err := url.Parse(r.raw)
		if r.parseErr {
			if err == nil {
				t.Errorf("%q: url.Parse took it (host %q)", r.raw, u.Hostname())
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", r.raw, err)
			continue
		}
		if class, final := classifyHost(u.Hostname()); class != r.class || final != r.final {
			t.Errorf("%q: classifyHost(%q) = %d %v, want %d %v", r.raw, u.Hostname(), class, final, r.class, r.final)
		}
		if r.stricter {
			stricter++
		}
		if r.code == "-" {
			continue
		}
		for _, resolve := range []bool{false, true} {
			err := ta.checkURL(ctx, r.raw, resolve)
			got := ""
			if f := localFailure(err); f != nil {
				got = f.code
			} else if err != nil {
				got = err.Error()
			}
			if got != r.code {
				t.Errorf("%q (resolve %v): %q, want %q", r.raw, resolve, got, r.code)
			}
		}
		if r.class == classUnusual && lookups[u.Hostname()] != 0 {
			t.Errorf("%q: an unusual host was looked up", r.raw)
		}
	}
	if stricter < 40 {
		t.Errorf("only %d stricter rows", stricter)
	}
}

// Only stricter: for every row, the old rule's refusal (any class but
// public, and any code) is still a refusal. The old classification is
// rebuilt here as it was (non-ASCII, a control character or space, a
// percent sign outside an IPv6 zone; one trailing dot stripped).
func TestSEC11NoRowLoosened(t *testing.T) {
	oldUnusual := func(h string) bool {
		for i := 0; i < len(h); i++ {
			if h[i] < 0x21 || h[i] >= 0x7f {
				return true
			}
		}
		if strings.IndexByte(h, '%') >= 0 {
			_, err := netip.ParseAddr(h)
			return err != nil
		}
		return false
	}
	for _, r := range probeRows() {
		u, err := url.Parse(r.raw)
		if err != nil {
			continue
		}
		h := u.Hostname()
		newClass, newFinal := classifyHost(h)
		if oldUnusual(h) && newClass != classUnusual {
			t.Errorf("%q: was unusual, now %d", r.raw, newClass)
		}
		if newClass == classPublic && !newFinal && r.stricter {
			t.Errorf("%q: marked stricter but still a public name", r.raw)
		}
	}
}

// The same names in a playlist's entry, a segment for aria2c and a
// redirect's Location: the task fails with downloads.unusual_host and
// nothing is requested there (the names would reach this computer if
// they were looked up: the media server listens on 127.0.0.1).
func TestPlaylistAndRedirectToPunctuatedHost(t *testing.T) {
	e := newQEnv(t, nil)
	port := e.media.srv.URL[strings.LastIndexByte(e.media.srv.URL, ':')+1:]
	e.media.put("/hls/master.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nhttp://localhost..:"+port+"/var/index.m3u8\n"))
	e.media.put("/var/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	e.media.put("/seg/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\n"+
		"http://127.0.0.1;.evil.example:"+port+"/files/seg0.ts\n#EXT-X-ENDLIST\n"))
	e.media.put("/landing.m4a", []byte("fake media"))
	e.media.put("/landing2.m4a", []byte("fake media"))
	e.media.mu.Lock()
	e.media.redir["/files/r.m4a"] = "http://localhost,:" + port + "/landing.m4a"
	e.media.redir["/files/r2.m4a"] = "http://.localhost:" + port + "/landing2.m4a"
	e.media.mu.Unlock()
	for i, tr := range []bridge.Track{
		e.hlsTrack(bridge.KindVideo, "/hls/master.m3u8"), // Go reads the variant playlist
		e.hlsTrack(bridge.KindVideo, "/seg/index.m3u8"),  // a segment for aria2c
		e.fileTrack(bridge.KindAudio, "/files/r.m4a"),    // a redirect hop (followed by Go: the task has cookies)
		e.fileTrack(bridge.KindAudio, "/files/r2.m4a"),   // an empty label in a hop
	} {
		id := e.enqueue(e.request(fmt.Sprint("p", i), tr))
		info := e.wait(id, bridge.StateFailed)
		if info.Error == nil || info.Error.Code != CodeUnusualHost {
			t.Errorf("%d: %+v", i, info.Error)
		}
	}
	for _, p := range []string{"/var/index.m3u8", "/files/seg0.ts", "/landing.m4a", "/landing2.m4a"} {
		if rs := e.media.requests(p); len(rs) != 0 {
			t.Errorf("%s was requested: %+v", p, rs)
		}
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 {
		t.Fatalf("aria2c was given %+v", adds)
	}
}

// The redirect policy of Go's client (every hop): a punctuated or
// empty-labelled Location is refused before it is dialled.
func TestRedirectHopPunctuatedHost(t *testing.T) {
	e := newQEnv(t, nil)
	port := e.media.srv.URL[strings.LastIndexByte(e.media.srv.URL, ':')+1:]
	s := runSnapshot{net: e.q.newTaskNet("lms.publicuni.example"), referer: testReferer}
	defer s.net.close()
	c := e.q.client(s)
	prev, err := http.NewRequest(http.MethodGet, e.media.url("/files/a.m4a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []string{"http://localhost..:" + port + "/x", "http://a;b.localhost:" + port + "/x",
		"http://*.localhost:" + port + "/x", "http://.localhost:" + port + "/x", "http://media~cdn.example/x"} {
		req, err := http.NewRequest(http.MethodGet, loc, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = c.CheckRedirect(req, []*http.Request{prev})
		if f := localFailure(err); f == nil || f.code != CodeUnusualHost {
			t.Errorf("%s: %v", loc, err)
		}
	}
	// a plain name still passes the hop's check (its address is the dialler's)
	req, _ := http.NewRequest(http.MethodGet, "http://media-01.cdn_x.example/x", nil)
	if err := c.CheckRedirect(req, []*http.Request{prev}); err != nil {
		t.Errorf("a plain name: %v", err)
	}
}

// SEC11 low-2: a host on the do-not-handle list written with characters a
// text should not carry (a bidi control, full-width letters, an
// ideographic full stop) reaches the failure's params and detail, and the
// error's text, percent-encoded; an ASCII host as is.
func TestSiteBlockedHostIsSafe(t *testing.T) {
	ctx := context.Background()
	ta := testPolicy(false, map[string]int{}).forTask("lms.publicuni.example")
	for raw, want := range map[string]string{
		"https://x\u202e.youtube.com/v":         "x%E2%80%AE.youtube.com",
		"https://www.coursera\u3002org/learn/x": "www.coursera%E3%80%82org",
		"https://ｗｗｗ.ｙｏｕｔｕｂｅ.com/v":             "%EF%BD%97%EF%BD%97%EF%BD%97.%EF%BD%99%EF%BD%8F%EF%BD%95%EF%BD%94%EF%BD%95%EF%BD%82%EF%BD%85.com",
		"https://www.youtube.com/watch?v=x":     "www.youtube.com",
		"https://evil.example@udemy.com/c":      "udemy.com",
	} {
		err := ta.checkURL(ctx, raw, false)
		f := localFailure(err)
		if f == nil || f.code != CodeSiteBlocked {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if f.params["host"] != want {
			t.Errorf("%q: host %q, want %q", raw, f.params["host"], want)
		}
		for _, s := range []string{f.detail, err.Error()} {
			if !strings.Contains(s, want) {
				t.Errorf("%q: %q does not name %q", raw, s, want)
			}
			for i := 0; i < len(s); i++ {
				if s[i] < 0x20 || s[i] >= 0x7f {
					t.Errorf("%q: %q is not printable ASCII", raw, s)
					break
				}
			}
		}
	}
}

// aria2c resolves with the system's resolver, as Go does (SEC11 low-1):
// --async-dns=false, for a build that has the option (Async DNS) only.
func TestResolverArgs(t *testing.T) {
	if got := resolverArgs(true); !slices.Equal(got, []string{"--async-dns=false"}) {
		t.Errorf("with Async DNS: %v", got)
	}
	if got := resolverArgs(false); len(got) != 0 {
		t.Errorf("without Async DNS: %v", got)
	}
	for v, want := range map[string]bool{
		fakeVersion(featuresWindows):           true,
		fakeVersion(featuresMac):               false,
		"Enabled Features: Async DNS\r\n":      true,
		"Enabled Features: Async DNSSEC, GZip": false,
		"Async DNS":                            false, // not on the features line
		"":                                     false,
	} {
		if got := hasFeature(v, "Async DNS"); got != want {
			t.Errorf("hasFeature(%q) = %v", v, got)
		}
	}
}

// The engine gives --async-dns=false to the Windows build (every download
// of the process: a command-line option) and not to the macOS build,
// which would refuse it; both start.
func TestEngineAsyncDNSFollowsBuild(t *testing.T) {
	for _, c := range []struct {
		name, features string
		want           bool
	}{{"windows", featuresWindows, true}, {"macos", featuresMac, false}} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LF_FAKE_ARIA2C_FEATURES", c.features)
			eng := childEngine(t, io.Discard, nil)
			eng.versionOf = runVersion
			in, err := eng.acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer eng.release()
			if args := in.proc.(*execProcess).cmd.Args[1:]; slices.Contains(args, "--async-dns=false") != c.want {
				t.Fatalf("args %v", args)
			}
		})
	}
	t.Run("macos given the option", func(t *testing.T) {
		t.Setenv("LF_FAKE_ARIA2C_FEATURES", featuresMac)
		eng := childEngine(t, io.Discard, nil)
		eng.versionOf = func(string) (string, error) { return fakeVersion(featuresMac + ", Async DNS"), nil }
		if _, err := eng.acquire(context.Background()); !errors.Is(err, errExitedEarly) {
			t.Fatalf("a build without Async DNS started with --async-dns: %v", err)
		}
	})
}

// The bundled aria2c takes --async-dns=false (its build has Async DNS) and
// the proxy options with their values (an empty one, and a proxy with a
// user and password) on its command line, where it checks them as it
// checks addUri's: exit 28 would be a refusal. As TestRealAria2cAcceptsOurOptions:
// a dry run to a port of this computer that refuses connections; skipped
// where no bundled aria2c is staged.
func TestRealAria2cAcceptsResolverAndProxyOptions(t *testing.T) {
	exe := realAria2c()
	if exe == "" {
		t.Skip("no bundled aria2c (stage the desktop resources or set LF_ARIA2C)")
	}
	eng := &engine{exe: exe, log: slog.New(slog.NewTextHandler(io.Discard, nil)), hc: &http.Client{}}
	eng.features(exe)
	if runtime.GOOS == "windows" && !eng.asyncDNS {
		t.Errorf("the Windows build reads as having no Async DNS")
	}
	for _, proxy := range []string{"", "http://alice:s3cret@127.0.0.1:1"} { // (a proxy that refuses: nothing leaves the computer)
		args := append(resolverArgs(eng.asyncDNS), "--no-conf=true", "--dir="+t.TempDir(), "--dry-run=true",
			"--max-tries=1", "--connect-timeout=1", "--timeout=1")
		for name, v := range map[string]string{"all-proxy": proxy, "http-proxy": proxy, "https-proxy": proxy, "ftp-proxy": proxy, "no-proxy": ""} {
			args = append(args, "--"+name+"="+v)
		}
		args = append(args, "http://127.0.0.1:1/smoke.mp4")
		cmd := exec.Command(exe, args...)
		cmd.Dir = t.TempDir()
		hideWindow(cmd)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 28 {
			t.Errorf("aria2c refused an option (exit 28):\n%s", out)
		}
	}
}

// SEC11 low-3: the proxy of aria2c's request for a URL is Go's answer for
// that URL, so aria2c's own reading of the environment (lower-case names
// only, its no_proxy) decides nothing. Since the guard proxy (SEC16 R1)
// that answer is the guard's to use (upstream); aria2c's own options name
// the guard, given explicitly with every download (every protocol's
// proxy, no-proxy empty), whatever the user's proxy.
func TestAriaProxyOptions(t *testing.T) {
	g, err := newGuard(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	media, _ := url.Parse("https://media.example.com/v.mp4")
	for _, c := range []struct {
		proxy string // "" = none
		err   bool
		want  string
	}{
		{"", false, ""},
		{"http://proxy.example:3128", false, "http://proxy.example:3128"},
		{"http://alice:s3cret@proxy.example:3128", false, "http://alice:s3cret@proxy.example:3128"},
		{"HTTP://Proxy.Example", false, "http://Proxy.Example:80"},
		{"https://proxy.example", false, ""}, // aria2c speaks plain HTTP to it: never given (the URL is refused, TestUpstreamProxy)
		{"http://[fd00::1]:3128", false, "http://[fd00::1]:3128"},
		{"http://192.0.2.7:8080", false, "http://192.0.2.7:8080"},
		{"socks5://proxy.example:1080", false, ""}, // aria2c has no SOCKS: never given (refused too)
		{"socks5h://proxy.example", false, ""},
		{"http://proxy.example:3128", true, "http://proxy.example:3128"}, // (proxied() takes the URL whatever the error)
		{"", true, ""},
	} {
		p := &addrPolicy{proxy: func(*url.URL) (*url.URL, error) {
			var pu *url.URL
			if c.proxy != "" {
				pu, _ = url.Parse(c.proxy)
			}
			if c.err {
				return pu, errors.New("bad proxy variable")
			}
			return pu, nil
		}}
		got := ""
		if pu, _ := p.upstream(media); pu != nil {
			got = pu.String()
		}
		if got != c.want {
			t.Errorf("%q (err %v): the guard's proxy %q, want %q", c.proxy, c.err, got, c.want)
		}
		if p.viaUpstream(media) != (c.want != "") {
			t.Errorf("%q: viaUpstream disagrees with upstream", c.proxy)
		}
		// aria2c's options: the guard with the session's credentials
		s, err := g.open(p.forTask(""))
		if err != nil {
			t.Fatal(err)
		}
		// (a port per protocol; ftp and all-proxy: the port that serves nothing)
		v := "http://" + s.user + ":" + s.pass + "@"
		ports := []string{s.listenHTTP, s.listenHTTPS, s.listenOther}
		if opts := s.options(); !mapsEqual(opts, map[string]string{"all-proxy": v + s.listenOther, "http-proxy": v + s.listenHTTP,
			"https-proxy": v + s.listenHTTPS, "ftp-proxy": v + s.listenOther, "no-proxy": ""}) ||
			len(s.user) != 16 || len(s.pass) != 48 {
			t.Errorf("%q: aria2c's options %v", c.proxy, opts)
		}
		for i, listen := range ports {
			if !strings.HasPrefix(listen, "127.0.0.1:") || listen == g.addr || slices.Contains(ports[:i], listen) {
				t.Errorf("%q: the session's ports %v (the guard's %s)", c.proxy, ports, g.addr)
			}
		}
		s.close()
	}
	// per URL: a proxy for https only (HTTPS_PROXY set, HTTP_PROXY not)
	p := &addrPolicy{proxy: func(u *url.URL) (*url.URL, error) {
		if u.Scheme == "https" {
			return url.Parse("http://proxy.school.example:3128")
		}
		return nil, nil
	}}
	plain, _ := url.Parse("http://media.example.com/v.mp4")
	if pu, err := p.upstream(media); err != nil || pu == nil || pu.String() != "http://proxy.school.example:3128" {
		t.Errorf("https: %v %v", pu, err)
	}
	if pu, err := p.upstream(plain); pu != nil || err != nil {
		t.Errorf("http: %v %v", pu, err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// The address check of a URL for aria2c is skipped exactly when aria2c is
// given a proxy for it. A proxy aria2c cannot use (SOCKS, https://) is not
// a direct download any more (SEC16 low-1 and low-2): the URL is refused
// before any look-up, and aria2c is given nothing.
func TestAriaCheckFollowsAriaProxy(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		proxy   string
		code    string
		lookups int
	}{
		{"http://proxy.school.example:3128", "", 0},
		{"https://proxy.school.example:3128", CodeProxyUnsupported, 0},
		{"socks5://proxy.school.example:1080", CodeProxyUnsupported, 0},
		{"", CodeLocalAddress, 1},
	} {
		lookups := map[string]int{}
		p := testPolicy(false, lookups)
		p.proxy = func(*url.URL) (*url.URL, error) {
			if c.proxy == "" {
				return nil, nil
			}
			return url.Parse(c.proxy)
		}
		r := (&Queue{}).newResolver(runSnapshot{net: &taskNet{addr: p.forTask("lms.publicuni.example")}})
		err := r.checkHost(ctx, "https://media.campus-cdn.example/a.ts")
		got := ""
		var f *failure
		if errors.As(err, &f) {
			got = f.code
		} else if err != nil {
			got = err.Error()
		}
		if got != c.code || lookups["media.campus-cdn.example"] != c.lookups {
			t.Errorf("%q: %q, lookups %v", c.proxy, got, lookups)
		}
		if u, _ := url.Parse("https://media.campus-cdn.example/a.ts"); p.viaUpstream(u) != (c.code == "") {
			t.Errorf("%q: the guard's proxy disagrees with the check", c.proxy)
		}
	}
}

// The whole queue: every addUri carries the proxy options, which name the
// guard proxy with the run's own credentials (SEC16 R1: before, the
// user's proxy or none), with a proxy of the user's and without; the
// user's proxy, credentials included, reaches no option of aria2c any
// more (the guard uses it: guard_test.go), nor the log or the task, and
// neither do the guard's credentials.
func TestQueueGivesAriaTheProxy(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/files/a.m4a", pattern(1, 1000))
	e.wait(e.enqueue(e.request("np", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))), bridge.StateDone)
	adds, args, _, _, _ := e.aria.snapshot()
	guardURL := regexp.MustCompile(`^http://[0-9a-f]{16}:[0-9a-f]{48}@127\.0\.0\.1:[0-9]+$`)
	// (the session's credentials on a port per protocol; ftp and all-proxy
	// on the one that serves nothing)
	isGuard := func(p map[string]string) bool {
		cred, _, _ := strings.Cut(p["http-proxy"], "@")
		for _, name := range []string{"http-proxy", "https-proxy", "ftp-proxy"} {
			if !guardURL.MatchString(p[name]) || !strings.HasPrefix(p[name], cred+"@") {
				return false
			}
		}
		return p["http-proxy"] != p["https-proxy"] && p["http-proxy"] != p["ftp-proxy"] && p["https-proxy"] != p["ftp-proxy"] &&
			mapsEqual(p, map[string]string{"all-proxy": p["ftp-proxy"], "http-proxy": p["http-proxy"], "https-proxy": p["https-proxy"],
				"ftp-proxy": p["ftp-proxy"], "no-proxy": ""})
	}
	if len(adds) != 1 || !isGuard(adds[0].Proxy) {
		t.Fatalf("no proxy: %+v", adds)
	}
	if !slices.Contains(args, "--async-dns=false") {
		t.Errorf("args %v", args)
	}
	// the command line names the guard too: its own port, where nothing is
	// served, without credentials; and it empties no-proxy
	base := argValue(args, "all-proxy")
	if !regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+$`).MatchString(base) {
		t.Errorf("the command line's proxy %q", base)
	}
	for _, name := range []string{"http-proxy", "https-proxy", "ftp-proxy"} {
		if _, sessionPort, _ := strings.Cut(adds[0].Proxy[name], "@"); base == "http://"+sessionPort {
			t.Errorf("the command line's proxy %q is the session's %s", base, name)
		}
	}
	for _, want := range []string{"--all-proxy=" + base, "--http-proxy=" + base, "--https-proxy=" + base,
		"--ftp-proxy=" + base, "--no-proxy=", "--proxy-method=get"} {
		if !slices.Contains(args, want) {
			t.Errorf("aria2c started without %s: %v", want, args)
		}
	}

	// the media server plays the proxy too (it serves a request by its path)
	pu, _ := url.Parse(e.media.srv.URL)
	pu.User = url.UserPassword("alice", "s3cret")
	e.q.local.proxy = func(*url.URL) (*url.URL, error) { return pu, nil }
	e.media.put("/files/b.m4a", pattern(2, 1000))
	id := e.enqueue(e.request("px", e.fileTrack(bridge.KindAudio, "/files/b.m4a")))
	info := e.wait(id, bridge.StateDone)
	adds, args, _, _, _ = e.aria.snapshot()
	if len(adds) != 2 || !isGuard(adds[1].Proxy) || adds[1].Proxy["all-proxy"] == adds[0].Proxy["all-proxy"] {
		t.Fatalf("proxied: %+v", adds)
	}
	if given := fmt.Sprintf("%+v %v", adds, args); strings.Contains(given, "s3cret") || strings.Contains(given, "alice") {
		t.Errorf("the user's proxy reached aria2c: %s", given)
	}
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(e.logs.String(), "s3cret") || strings.Contains(fmt.Sprintf("%+v", info), "s3cret") {
		t.Errorf("the proxy's password got out: %s", e.logs.String())
	}
	for _, a := range adds {
		user, rest, _ := strings.Cut(strings.TrimPrefix(a.Proxy["all-proxy"], "http://"), ":")
		pass, _, _ := strings.Cut(rest, "@")
		if logs := e.logs.String() + fmt.Sprintf("%+v", info) + strings.Join(args, " "); strings.Contains(logs, pass) || strings.Contains(logs, user) {
			t.Errorf("the guard's credentials got out")
		}
	}
}
