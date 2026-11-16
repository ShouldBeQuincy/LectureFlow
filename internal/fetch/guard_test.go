// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// guardRig is a guard proxy with the tests' network around it. Nothing
// leaves the computer: names are answered by the rig's table alone (a name
// it does not know cannot be looked up), and every connection the guard
// makes goes to 127.0.0.1 (loopbackWire), whatever address the table gave.
//
//   - origin plays a public media server: origin.example and
//     cdn.example.com are 203.0.113.x, its port;
//   - inner plays a server of this computer or the user's network, which
//     no download may reach: every request to it is counted;
//   - the policy is the product's (loopback not allowed).
type guardRig struct {
	t      *testing.T
	g      *guard
	p      *addrPolicy
	logs   *syncBuffer
	origin *httptest.Server
	inner  *httptest.Server
	// innerHits counts the requests inner got; innerConns the connections
	// its listener accepted.
	innerHits, innerConns atomic.Int64
	originHits            atomic.Int64

	mu      sync.Mutex
	names   map[string]func(nth int) []string // the answers of a name's nth look-up
	lookups map[string]int
	dialed  []string // the addresses the guard connected to
	release chan struct{}
}

type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

func always(addrs ...string) func(int) []string { return func(int) []string { return addrs } }

func newGuardRig(t *testing.T) *guardRig {
	t.Helper()
	r := &guardRig{t: t, logs: &syncBuffer{}, lookups: map[string]int{}, release: make(chan struct{})}
	r.names = map[string]func(int) []string{
		"origin.example":          always("203.0.113.10"),
		"cdn.example.com":         always("203.0.113.11"),
		"example.com":             always("203.0.113.12"), // (the name of httptest's certificate)
		"intranet.example":        always("10.0.0.5"),
		"loop.example":            always("127.0.0.1"),
		"mapped.example":          always("::ffff:127.0.0.1"),
		"mixed.example":           always("203.0.113.13", "192.168.1.9"),
		"linklocal.example":       always("169.254.169.254"),
		"lms.publicuni.example":   always("203.0.113.14"),
		"opencast.campus.example": always("10.0.0.2"),
		// NAT64 (an IPv6-only network): the local network, a public server
		"nat64.example":    always("64:ff9b::a00:5"),
		"nat64pub.example": always("64:ff9b::cb00:710a"),
		// DNS rebinding: a public address for the first look-up, this
		// computer for every later one
		"rebind.example": func(nth int) []string {
			if nth == 1 {
				return []string{"203.0.113.20"}
			}
			return []string{"127.0.0.1"}
		},
	}
	r.p = &addrPolicy{proxy: noProxy,
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.lookups[host]++
			answer, ok := r.names[strings.ToLower(strings.TrimSuffix(host, "."))]
			if !ok {
				return nil, errors.New("no such host (test)")
			}
			var out []netip.Addr
			for _, a := range answer(r.lookups[host]) {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			r.mu.Lock()
			r.dialed = append(r.dialed, address)
			r.mu.Unlock()
			return loopbackWire(ctx, network, address)
		}}
	r.origin = httptest.NewServer(http.HandlerFunc(r.serveOrigin))
	r.inner = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.innerHits.Add(1)
		io.WriteString(w, "INNER SECRET")
	}))
	r.inner.Listener = countingListener{r.inner.Listener, &r.innerConns}
	r.inner.Start()
	g, err := newGuard(slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	r.g = g
	t.Cleanup(func() {
		close(r.release)
		g.close()
		r.origin.Close()
		r.inner.Close()
		// the guard no longer listens
		if c, err := net.DialTimeout("tcp", g.addr, time.Second); err == nil {
			c.Close()
			t.Errorf("the guard still listens on %s after close", g.addr)
		}
	})
	return r
}

// serveOrigin: /ok, /redir?to=URL (302), /stall (some bytes, then nothing
// until the rig is released or the request ends), /big?n=BYTES.
func (r *guardRig) serveOrigin(w http.ResponseWriter, req *http.Request) {
	r.originHits.Add(1)
	if strings.HasPrefix(req.URL.Path, "/signed/") { // a path that is a credential: echoed as it arrived
		w.Header().Set("X-Seen-URI", req.RequestURI)
		io.WriteString(w, "SIGNED")
		return
	}
	switch req.URL.Path {
	case "/ok":
		w.Header().Set("X-Seen-Proxy-Authorization", req.Header.Get("Proxy-Authorization"))
		w.Header().Set("X-Seen-Cookie", req.Header.Get("Cookie"))
		w.Header().Set("X-Seen-Query", req.URL.RawQuery)
		w.Header().Set("X-Seen-Headers", strings.Join(headerNames(req.Header), ","))
		io.WriteString(w, "ORIGIN")
	case "/redir":
		http.Redirect(w, req, req.URL.Query().Get("to"), http.StatusFound)
	case "/stall":
		w.Header().Set("Content-Length", "1000000")
		w.Write(bytes.Repeat([]byte("a"), 1000))
		w.(http.Flusher).Flush()
		select {
		case <-r.release:
		case <-req.Context().Done():
		}
	case "/big":
		n, _ := strconv.Atoi(req.URL.Query().Get("n"))
		http.ServeContent(w, req, "big.bin", time.Time{}, bytes.NewReader(pattern(9, n)))
	default:
		http.NotFound(w, req)
	}
}

// headerNames are a request's header names, sorted.
func headerNames(h http.Header) []string {
	var names []string
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func portOfServer(s *httptest.Server) string {
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	return port
}

// originURL is a URL of the origin by its public name.
func (r *guardRig) originURL(path string) string {
	return "http://origin.example:" + portOfServer(r.origin) + path
}

// session opens a session for a recording whose page is on pageHost.
func (r *guardRig) session(pageHost string, trusted ...string) *guardSession {
	r.t.Helper()
	s, err := r.g.open(r.p.forTask(pageHost), trusted...)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(s.close)
	return s
}

// client plays aria2c: every request, each redirect included, goes to the
// guard with the session's credentials, to the port of its protocol (an
// http URL by its absolute URI, an https URL through CONNECT).
func (r *guardRig) client(s *guardSession) *http.Client {
	opts := s.options()
	proxy := func(req *http.Request) (*url.URL, error) { return url.Parse(opts[req.URL.Scheme+"-proxy"]) }
	tr := &http.Transport{Proxy: proxy, DisableKeepAlives: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	r.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

func (r *guardRig) lookupsOf(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookups[host]
}

func (r *guardRig) dialedAddrs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dialed...)
}

// refusalOf is the code and host of the session's refusal ("" = none).
func refusalOf(s *guardSession) (code, host string) {
	err := s.refused()
	if err == nil {
		return "", ""
	}
	f := asFailure(err)
	host, _ = f.params["host"].(string)
	return f.code, host
}

// get makes a request as aria2c would and returns the final status and
// body (status 0: the request failed, e.g. a refused CONNECT).
func get(hc *http.Client, rawURL string) (int, string) {
	res, err := hc.Get(rawURL)
	if err != nil {
		return 0, err.Error()
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, string(b)
}

// A download through the guard: the bytes, the query and the headers
// arrive as sent; the guard's credentials do not reach the server; the
// name was looked up once, by the guard, and the connection went to that
// address.
func TestGuardForwardsADownload(t *testing.T) {
	r := newGuardRig(t)
	s := r.session("lms.publicuni.example")
	hc := r.client(s)
	req, _ := http.NewRequest(http.MethodGet, r.originURL("/ok?Signature=a%2Fb+c&x=%E4%B8%AD"), nil)
	req.Header.Set("Cookie", "sid=abc")
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "ORIGIN" {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
	if res.Header.Get("X-Seen-Proxy-Authorization") != "" {
		t.Errorf("the guard's credentials reached the server: %q", res.Header.Get("X-Seen-Proxy-Authorization"))
	}
	if res.Header.Get("X-Seen-Cookie") != "sid=abc" || res.Header.Get("X-Seen-Query") != "Signature=a%2Fb+c&x=%E4%B8%AD" {
		t.Errorf("the request changed on the way: %v", res.Header)
	}
	// no header of the guard's making (no Via, X-Forwarded-For or the like)
	if got := res.Header.Get("X-Seen-Headers"); got != "Accept-Encoding,Cookie,User-Agent" {
		t.Errorf("the server saw the headers %s", got)
	}
	// a path and a query as they were written, escapes and all
	const signed = "/signed/a%20b%2Fc/~x;v=1/%E4%B8%AD.m4a?Policy=eyJ%3D&Key-Pair-Id=K%2B1&sig=a/b+c=="
	if res, err := hc.Get(r.originURL(signed)); err != nil || res.Header.Get("X-Seen-URI") != signed {
		t.Errorf("a signed URL arrived as %q (%v)", res.Header.Get("X-Seen-URI"), err)
	} else {
		res.Body.Close()
	}
	if n := r.lookupsOf("origin.example"); n != 1 {
		t.Errorf("origin.example looked up %d times", n)
	}
	if d := r.dialedAddrs(); !slices.Equal(d, []string{"203.0.113.10:" + portOfServer(r.origin)}) {
		t.Errorf("connected to %v", d)
	}
	if code, _ := refusalOf(s); code != "" {
		t.Errorf("refusal %s", code)
	}
	// a large body, ranges included, byte for byte
	const size = 3 << 20
	status, got := 0, []byte(nil)
	if res, err := hc.Get(r.originURL("/big?n=" + strconv.Itoa(size))); err == nil {
		status = res.StatusCode
		got, _ = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if status != 200 || !bytes.Equal(got, pattern(9, size)) {
		t.Fatalf("big: %d, %d bytes", status, len(got))
	}
	req, _ = http.NewRequest(http.MethodGet, r.originURL("/big?n=1000"), nil)
	req.Header.Set("Range", "bytes=10-19")
	if res, err := hc.Do(req); err != nil || res.StatusCode != http.StatusPartialContent || res.Header.Get("Content-Range") != "bytes 10-19/1000" {
		t.Fatalf("range: %v %v", res, err)
	} else {
		res.Body.Close()
	}
	// nothing of a request in the log
	if logs := r.logs.String(); strings.Contains(logs, "Signature") || strings.Contains(logs, "sid=abc") || strings.Contains(logs, s.pass) {
		t.Errorf("the log holds a request:\n%s", logs)
	}
}

// SEC16 R1: every hop is judged. A public server's redirect to this
// computer, the local network, a listed site or an address in another
// notation is refused by the guard with the rule's code and host, and the
// server it points at is never connected to; the same targets asked for
// directly are refused too.
func TestGuardJudgesEveryHop(t *testing.T) {
	r := newGuardRig(t)
	in := portOfServer(r.inner)
	cases := []struct {
		name, target string
		code, host   string
	}{
		{"loopback", "http://127.0.0.1:" + in + "/secret", CodeLoopbackAddress, "127.0.0.1"},
		{"loopback, another address of 127/8", "http://127.9.8.7:" + in + "/secret", CodeLoopbackAddress, "127.9.8.7"},
		{"localhost", "http://localhost:" + in + "/secret", CodeLoopbackAddress, "localhost"},
		{"a name under localhost", "http://app.localhost:" + in + "/secret", CodeLoopbackAddress, "app.localhost"},
		{"localhost with the root dot", "http://LOCALHOST.:" + in + "/secret", CodeLoopbackAddress, "LOCALHOST."},
		{"IPv6 loopback", "http://[::1]:" + in + "/secret", CodeLoopbackAddress, "::1"},
		{"IPv4-mapped loopback", "http://[::ffff:127.0.0.1]:" + in + "/secret", CodeLoopbackAddress, "::ffff:127.0.0.1"},
		{"IPv4-mapped loopback in hex", "http://[::ffff:7f00:1]:" + in + "/secret", CodeLoopbackAddress, "::ffff:7f00:1"},
		{"decimal address", "http://2130706433:" + in + "/secret", CodeLoopbackAddress, "2130706433"},
		{"hexadecimal address", "http://0x7f000001:" + in + "/secret", CodeLoopbackAddress, "0x7f000001"},
		{"hexadecimal parts", "http://0x7f.0x0.0x0.0x1:" + in + "/secret", CodeLoopbackAddress, "0x7f.0x0.0x0.0x1"},
		{"octal address", "http://0177.0.0.1:" + in + "/secret", CodeLoopbackAddress, "0177.0.0.1"},
		{"short address", "http://127.1:" + in + "/secret", CodeLoopbackAddress, "127.1"},
		{"unspecified address", "http://0.0.0.0:" + in + "/secret", CodeLoopbackAddress, "0.0.0.0"},
		{"IPv6 unspecified", "http://[::]:" + in + "/secret", CodeLoopbackAddress, "::"},
		{"private address", "http://10.0.0.5:" + in + "/secret", CodeLocalAddress, "10.0.0.5"},
		{"private address 192.168", "http://192.168.1.1:" + in + "/secret", CodeLocalAddress, "192.168.1.1"},
		{"link-local (cloud metadata)", "http://169.254.169.254:" + in + "/latest/meta-data", CodeLocalAddress, "169.254.169.254"},
		{"unique local IPv6", "http://[fd00::7]:" + in + "/secret", CodeLocalAddress, "fd00::7"},
		{"link-local IPv6", "http://[fe80::1]:" + in + "/secret", CodeLocalAddress, "fe80::1"},
		{"mDNS name", "http://printer.local:" + in + "/secret", CodeLocalAddress, "printer.local"},
		{"a name of a private address", "http://intranet.example:" + in + "/secret", CodeLocalAddress, "intranet.example"},
		{"a name of this computer", "http://loop.example:" + in + "/secret", CodeLoopbackAddress, "loop.example"},
		{"a name of an IPv4-mapped loopback address", "http://mapped.example:" + in + "/secret", CodeLoopbackAddress, "mapped.example"},
		{"a name with a public and a private address", "http://mixed.example:" + in + "/secret", CodeLocalAddress, "mixed.example"},
		{"a name of a link-local address", "http://linklocal.example:" + in + "/secret", CodeLocalAddress, "linklocal.example"},
		{"a listed site", "http://www.youtube.com:" + in + "/watch", CodeSiteBlocked, "www.youtube.com"},
		{"a listed site by its path", "http://www.linkedin.com:" + in + "/learning/x", CodeSiteBlocked, "www.linkedin.com"},
		{"a listed site, user info before it", "http://cdn.example.com@m.youtube.com:" + in + "/x", CodeSiteBlocked, "m.youtube.com"},
		{"an unusual host name", "http://a;b.example:" + in + "/secret", CodeUnusualHost, "a;b.example"},
		{"an empty label", "http://localhost..:" + in + "/secret", CodeUnusualHost, "localhost.."},
		{"https: this computer", "https://127.0.0.1:" + in + "/secret", CodeLoopbackAddress, "127.0.0.1"},
		{"https: IPv6 loopback", "https://[::1]:" + in + "/secret", CodeLoopbackAddress, "::1"},
		{"https: a private address", "https://10.0.0.5:" + in + "/secret", CodeLocalAddress, "10.0.0.5"},
		{"https: a name of this computer", "https://loop.example:" + in + "/secret", CodeLoopbackAddress, "loop.example"},
		{"https: decimal address", "https://2130706433:" + in + "/secret", CodeLoopbackAddress, "2130706433"},
		{"https: a listed site", "https://www.youtube.com:" + in + "/watch", CodeSiteBlocked, "www.youtube.com"},
		{"https: a listed site's media host", "https://rr1---sn-abc.googlevideo.com:" + in + "/videoplayback", CodeSiteBlocked, "rr1---sn-abc.googlevideo.com"},
		// SEC16 R4 / SEC19 low-5: an IPv4 address inside an IPv6 one, and
		// the ranges that were public
		{"NAT64 of this computer", "http://[64:ff9b::7f00:1]:" + in + "/secret", CodeLoopbackAddress, "64:ff9b::7f00:1"},
		{"https: NAT64 of the local network", "https://[64:ff9b::a00:5]:" + in + "/secret", CodeLocalAddress, "64:ff9b::a00:5"},
		{"a name of a NAT64 address of the local network", "http://nat64.example:" + in + "/secret", CodeLocalAddress, "nat64.example"},
		{"IPv4-compatible loopback", "http://[::127.0.0.1]:" + in + "/secret", CodeLoopbackAddress, "::127.0.0.1"},
		{"6to4 of this computer", "http://[2002:7f00:1::1]:" + in + "/secret", CodeLoopbackAddress, "2002:7f00:1::1"},
		{"carrier-grade NAT", "http://100.64.0.1:" + in + "/secret", CodeLocalAddress, "100.64.0.1"},
		{"https: carrier-grade NAT", "https://100.100.100.100:" + in + "/secret", CodeLocalAddress, "100.100.100.100"},
		// (no single server's address: unusual_host, not "this computer")
		{"multicast", "http://239.1.1.1:" + in + "/secret", CodeUnusualHost, "239.1.1.1"},
		{"IPv6 multicast", "https://[ff05::1]:" + in + "/secret", CodeUnusualHost, "ff05::1"},
		{"the limited broadcast", "http://255.255.255.255:" + in + "/secret", CodeUnusualHost, "255.255.255.255"},
		{"NAT64 of multicast", "http://[64:ff9b::e000:1]:" + in + "/secret", CodeUnusualHost, "64:ff9b::e000:1"},
		{"IPv4-compatible public", "http://[::8.8.8.8]:" + in + "/secret", CodeUnusualHost, "::8.8.8.8"},
		// (0.0.0.0/8 inside an IPv6 address: no server, not this computer)
		{"NAT64 of 0.0.0.0/8", "http://[64:ff9b::1]:" + in + "/secret", CodeUnusualHost, "64:ff9b::1"},
		{"https: 6to4 of 0.0.0.0/8", "https://[2002:0:1::1]:" + in + "/secret", CodeUnusualHost, "2002:0:1::1"},
		{"Teredo with the server 0.0.0.0", "http://[2001::8000:63bf:3fff:fdd2]:" + in + "/secret", CodeUnusualHost, "2001::8000:63bf:3fff:fdd2"},
		{"IPv4-mapped 0.0.0.0/8 (itself)", "http://[::ffff:0.1.2.3]:" + in + "/secret", CodeLoopbackAddress, "::ffff:0.1.2.3"},
	}
	for _, c := range cases {
		for _, via := range []string{"redirect", "directly"} {
			t.Run(c.name+", "+via, func(t *testing.T) {
				s := r.session("lms.publicuni.example")
				hc := r.client(s)
				first := c.target
				if via == "redirect" {
					first = r.originURL("/redir?to=" + url.QueryEscape(c.target))
				}
				status, body := get(hc, first)
				// 403 for an http hop; a refused CONNECT is an error of the request
				if status != http.StatusForbidden && status != 0 {
					t.Errorf("status %d %q", status, body)
				}
				if strings.Contains(body, "INNER") {
					t.Errorf("the inner server's answer came back: %q", body)
				}
				code, host := refusalOf(s)
				if code != c.code || host != c.host {
					t.Errorf("refusal %s {host: %q}, want %s {host: %q}", code, host, c.code, c.host)
				}
			})
		}
	}
	if r.innerHits.Load() != 0 || r.innerConns.Load() != 0 {
		t.Fatalf("the inner server got %d requests on %d connections", r.innerHits.Load(), r.innerConns.Load())
	}
	// the port of the inner server was connected to by no address at all
	for _, d := range r.dialedAddrs() {
		if strings.HasSuffix(d, ":"+in) {
			t.Errorf("the guard connected to %s", d)
		}
	}
	// (the rig works: the same server is reached where the rule allows it)
	near := r.session("opencast.campus.example")
	if status, body := get(r.client(near), "http://intranet.example:"+in+"/secret"); status != 200 || body != "INNER SECRET" {
		t.Fatalf("a recording of the local network: %d %q", status, body)
	}
	// NAT64 of a public address: reached, at that address
	pub := r.session("lms.publicuni.example")
	if status, body := get(r.client(pub), "http://nat64pub.example:"+portOfServer(r.origin)+"/ok"); status != 200 || body != "ORIGIN" {
		t.Fatalf("NAT64 of a public address: %d %q", status, body)
	}
	if d := r.dialedAddrs(); !slices.Contains(d, "[64:ff9b::cb00:710a]:"+portOfServer(r.origin)) {
		t.Errorf("NAT64 of a public address: connected to %v", d)
	}
	if r.innerHits.Load() != 1 {
		t.Fatalf("inner hits %d", r.innerHits.Load())
	}
	// ... but this computer never, also for a page on the local network
	if status, _ := get(r.client(near), "http://127.0.0.1:"+in+"/secret"); status != http.StatusForbidden {
		t.Errorf("loopback for a page on the local network: %d", status)
	}
	if status, _ := get(r.client(near), "http://loop.example:"+in+"/secret"); status != http.StatusForbidden {
		t.Errorf("a name of this computer for a page on the local network: %d", status)
	}
	if r.innerHits.Load() != 1 {
		t.Fatalf("inner hits %d", r.innerHits.Load())
	}
}

// An https download that a redirect sends down to plain http on this
// computer: the http hop is judged like any other.
func TestGuardHTTPSRedirectDownToLocalHTTP(t *testing.T) {
	r := newGuardRig(t)
	in := portOfServer(r.inner)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/ok" {
			io.WriteString(w, "SECURE")
			return
		}
		http.Redirect(w, req, "http://127.0.0.1:"+in+"/secret", http.StatusFound)
	}))
	defer secure.Close()
	s := r.session("lms.publicuni.example")
	hc := r.client(s)
	base := "https://example.com:" + portOfServer(secure)
	// the tunnel works (TLS end to end, the guard sees the host and port)
	if status, body := get(hc, base+"/ok"); status != 200 || body != "SECURE" {
		t.Fatalf("https through the guard: %d %q", status, body)
	}
	if status, body := get(hc, base+"/down"); status != http.StatusForbidden {
		t.Fatalf("https to local http: %d %q", status, body)
	}
	if code, host := refusalOf(s); code != CodeLoopbackAddress || host != "127.0.0.1" {
		t.Errorf("refusal %s %s", code, host)
	}
	if r.innerHits.Load() != 0 || r.innerConns.Load() != 0 {
		t.Fatalf("the inner server was reached")
	}
}

// DNS rebinding: the name answers with a public address once and with
// this computer afterwards. The guard connects to the address of its own
// look-up and to no other, so the second connection is refused; aria2c
// resolves nothing itself.
func TestGuardPinsTheAddressItChecked(t *testing.T) {
	r := newGuardRig(t)
	port := portOfServer(r.origin)
	s := r.session("lms.publicuni.example")
	hc := r.client(s)
	if status, body := get(hc, "http://rebind.example:"+port+"/ok"); status != 200 || body != "ORIGIN" {
		t.Fatalf("the first answer (public): %d %q", status, body)
	}
	hits := r.originHits.Load()
	s.direct.CloseIdleConnections() // a new connection: a new look-up
	if status, body := get(hc, "http://rebind.example:"+port+"/ok"); status != http.StatusForbidden {
		t.Fatalf("the second answer (this computer): %d %q", status, body)
	}
	if code, host := refusalOf(s); code != CodeLoopbackAddress || host != "rebind.example" {
		t.Errorf("refusal %s %s", code, host)
	}
	if r.originHits.Load() != hits {
		t.Errorf("the server was asked again")
	}
	if n := r.lookupsOf("rebind.example"); n != 2 {
		t.Errorf("%d look-ups for two connections", n)
	}
	if d := r.dialedAddrs(); !slices.Equal(d, []string{"203.0.113.20:" + port}) {
		t.Errorf("connected to %v, want the one address that was checked", d)
	}
	// the same through CONNECT
	r2 := newGuardRig(t)
	s2 := r2.session("lms.publicuni.example")
	for i, want := range []int{http.StatusOK, http.StatusForbidden} {
		status, c := rawConnect(t, s2.listenHTTPS, "rebind.example:"+portOfServer(r2.origin), s2.user, s2.pass)
		if c != nil {
			c.Close()
		}
		if status != want {
			t.Errorf("CONNECT %d: %d, want %d", i, status, want)
		}
	}
	if d := r2.dialedAddrs(); !slices.Equal(d, []string{"203.0.113.20:" + portOfServer(r2.origin)}) {
		t.Errorf("CONNECT: connected to %v", d)
	}
	// a name that cannot be looked up is not connected to: a failure of
	// the network, not a refusal (aria2c tries again)
	if status, _ := get(hc, "http://nowhere.example:"+port+"/ok"); status != http.StatusBadGateway {
		t.Errorf("an unknown name: %d", status)
	}
	var f *failure
	if err := s2.failureOf(ariaStatus{ErrorCode: "1", ErrorMessage: "Proxy connection failed."}); !errors.As(err, &f) || f.code != CodeLoopbackAddress {
		t.Errorf("the refusal comes before aria2c's error: %v", err)
	}
}

// rawConnect sends CONNECT authority to the guard (user "" = without
// credentials) and returns the status; the connection when it is 200.
func rawConnect(t *testing.T, guardAddr, authority, user, pass string) (int, net.Conn) {
	t.Helper()
	c, err := net.DialTimeout("tcp", guardAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	req := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n"
	if user != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)) + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		t.Fatalf("CONNECT %s: %v", authority, err)
	}
	if res.StatusCode != http.StatusOK {
		c.Close()
		return res.StatusCode, nil
	}
	c.SetDeadline(time.Time{})
	return res.StatusCode, c
}

// Another program of this computer cannot use the guard: a session's port
// takes that session's user and password and no other, the guard's own
// port (the one on aria2c's command line) takes none at all; everything
// else is answered 407 and nothing is connected to. A closed session's
// port is closed.
func TestGuardNeedsASessionsCredentials(t *testing.T) {
	r := newGuardRig(t)
	s := r.session("lms.publicuni.example")
	port := portOfServer(r.origin)
	plain := func(listen, user, pass string) int {
		req, _ := http.NewRequest(http.MethodGet, r.originURL("/ok"), nil) // (sent to the guard by its absolute URI)
		tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
			pu := &url.URL{Scheme: "http", Host: listen}
			if user != "" {
				pu.User = url.UserPassword(user, pass)
			}
			return pu, nil
		}, DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusProxyAuthRequired && !strings.HasPrefix(res.Header.Get("Proxy-Authenticate"), "Basic") {
			t.Errorf("407 without Proxy-Authenticate")
		}
		return res.StatusCode
	}
	other := r.session("lms.publicuni.example")
	ports := map[string]bool{r.g.addr: true}
	for _, ss := range []*guardSession{s, other} {
		for _, listen := range []string{ss.listenHTTP, ss.listenHTTPS, ss.listenOther} {
			if ports[listen] {
				t.Fatalf("a port shared: %s", listen)
			}
			ports[listen] = true
		}
	}
	// (GET to the first port, CONNECT to the second)
	for _, c := range []struct {
		name                     string
		get, connect, user, pass string
		want                     int
	}{
		{"none", s.listenHTTP, s.listenHTTPS, "", "", 407},
		{"a wrong password", s.listenHTTP, s.listenHTTPS, s.user, strings.Repeat("0", 48), 407},
		{"an empty password", s.listenHTTP, s.listenHTTPS, s.user, "", 407},
		{"a wrong user", s.listenHTTP, s.listenHTTPS, "0123456789abcdef", s.pass, 407},
		{"another session's password", s.listenHTTP, s.listenHTTPS, s.user, other.pass, 407},
		{"another session's credentials", s.listenHTTP, s.listenHTTPS, other.user, other.pass, 407},
		{"the session's on another session's port", other.listenHTTP, other.listenHTTPS, s.user, s.pass, 407},
		{"the session's on the guard's own port", r.g.addr, r.g.addr, s.user, s.pass, 407},
		{"none on the guard's own port", r.g.addr, r.g.addr, "", "", 407},
		{"none on the port that serves nothing", s.listenOther, s.listenOther, "", "", 407},
		{"the session's", s.listenHTTP, s.listenHTTPS, s.user, s.pass, 200},
	} {
		if got := plain(c.get, c.user, c.pass); got != c.want {
			t.Errorf("GET, %s: %d, want %d", c.name, got, c.want)
		}
		status, conn := rawConnect(t, c.connect, "origin.example:"+port, c.user, c.pass)
		if conn != nil {
			conn.Close()
		}
		if status != c.want {
			t.Errorf("CONNECT, %s: %d, want %d", c.name, status, c.want)
		}
	}
	if d := r.dialedAddrs(); len(d) != 2 {
		t.Errorf("connections made: %v", d)
	}
	// (a request without the credentials is no refusal of the session)
	if code, _ := refusalOf(s); code != "" {
		t.Errorf("refusal %s", code)
	}
	// it is a proxy only: a request for the guard itself is not served
	for _, listen := range []string{r.g.addr, s.listenHTTP, s.listenHTTPS, s.listenOther} {
		res, err := http.Get("http://" + listen + "/ok")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 407 {
			t.Errorf("a request for the guard itself (%s): %d", listen, res.StatusCode)
		}
		// "OPTIONS *" too (SEC19 info-11: net/http answered it 200 itself)
		c, err := net.DialTimeout("tcp", listen, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(10 * time.Second))
		io.WriteString(c, "OPTIONS * HTTP/1.1\r\nHost: "+listen+"\r\nConnection: close\r\n\r\n")
		res, err = http.ReadResponse(bufio.NewReader(c), nil)
		c.Close()
		if err != nil || res.StatusCode != 407 {
			t.Errorf("OPTIONS * (%s): %v %v", listen, res, err)
		}
	}
	// it listens on 127.0.0.1 alone
	for _, ln := range append(append([]net.Listener{r.g.ln}, s.lns...), other.lns...) {
		if host, _, _ := net.SplitHostPort(ln.Addr().String()); host != "127.0.0.1" {
			t.Errorf("the guard listens on %s", ln.Addr())
		}
	}
	// a closed session's ports no longer listen
	other.close()
	for _, listen := range []string{other.listenHTTP, other.listenHTTPS, other.listenOther} {
		if c, err := net.DialTimeout("tcp", listen, time.Second); err == nil {
			c.Close()
			t.Errorf("a closed session's port still listens")
		}
	}
}

// What is not an http or https address is refused: ftp:// (aria2c follows
// a redirect there by asking its proxy for the URL), a request that is not
// a proxy request, a CONNECT without a port or with more than a host.
func TestGuardRefusesOtherRequests(t *testing.T) {
	r := newGuardRig(t)
	s := r.session("lms.publicuni.example")
	raw := func(requestLine string) int {
		listen := s.listenHTTP // (GET; CONNECT to the https port)
		if strings.HasPrefix(requestLine, "CONNECT ") {
			listen = s.listenHTTPS
		}
		c, err := net.DialTimeout("tcp", listen, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		io.WriteString(c, requestLine+" HTTP/1.1\r\nHost: origin.example\r\nProxy-Authorization: Basic "+
			base64.StdEncoding.EncodeToString([]byte(s.user+":"+s.pass))+"\r\nConnection: close\r\n\r\n")
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			return 0
		}
		res.Body.Close()
		return res.StatusCode
	}
	if got := raw("GET ftp://anonymous@origin.example:" + portOfServer(r.inner) + "/x.bin"); got != http.StatusForbidden {
		t.Errorf("ftp: %d", got)
	}
	if code, host := refusalOf(s); code != CodeUnusualHost || host != "ftp://origin.example" {
		t.Errorf("ftp: refusal %s %q", code, host)
	}
	for _, line := range []string{
		"GET /ok",                                                    // not a proxy request
		"GET https://origin.example/ok",                              // https is tunnelled, never asked for by its URL
		"CONNECT origin.example",                                     // no port
		"CONNECT origin.example:0",                                   // no such port
		"CONNECT origin.example:99999",                               //
		"CONNECT user@origin.example:443",                            // more than a host
		"CONNECT origin.example:443/path",                            //
		"CONNECT http://origin.example:80",                           //
		"POST http://127.0.0.1:" + portOfServer(r.inner) + "/secret", // (any method: this computer)
	} {
		if got := raw(line); got != http.StatusBadRequest && got != http.StatusForbidden && got != 0 {
			t.Errorf("%q: %d", line, got)
		}
	}
	// a host name outside ASCII, as aria2c would send it (net/http's own
	// client maps it by IDNA first): unusual, whatever it would map to
	s2 := r.session("lms.publicuni.example")
	s = s2
	if got := raw("GET http://ｌｏｃａｌｈｏｓｔ:" + portOfServer(r.inner) + "/secret"); got != http.StatusForbidden && got != http.StatusBadRequest && got != 0 {
		t.Errorf("full-width localhost: %d", got)
	}
	if code, _ := refusalOf(s2); code != "" && code != CodeUnusualHost {
		t.Errorf("full-width localhost: refusal %s", code)
	}
	if r.innerHits.Load() != 0 || r.innerConns.Load() != 0 || len(r.dialedAddrs()) != 0 {
		t.Fatalf("something was connected to: %v", r.dialedAddrs())
	}
}

// SEC19 low-1 to low-3: a port per protocol. The http port serves plain
// requests only, the https port CONNECT only, and only for TLS; the port
// of ftp-proxy and all-proxy serves nothing: a request there (a redirect
// to ftp:// or sftp://) is the session's refusal. Nothing is connected to
// for any of them.
func TestGuardPortsPerProtocol(t *testing.T) {
	r := newGuardRig(t)
	port := portOfServer(r.origin)
	authority := "origin.example:" + port
	// send writes a request with the session's credentials to listen and
	// returns the status (0: no answer)
	send := func(s *guardSession, listen, requestLine string) int {
		c, err := net.DialTimeout("tcp", listen, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		io.WriteString(c, requestLine+" HTTP/1.1\r\nHost: "+authority+"\r\nProxy-Authorization: Basic "+
			base64.StdEncoding.EncodeToString([]byte(s.user+":"+s.pass))+"\r\nConnection: close\r\n\r\n")
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			return 0
		}
		res.Body.Close()
		return res.StatusCode
	}
	// the wrong port for a protocol: not served, and no refusal either
	// (aria2c sends neither)
	s := r.session("lms.publicuni.example")
	if got := send(s, s.listenHTTP, "CONNECT "+authority); got != http.StatusBadRequest {
		t.Errorf("CONNECT on the http port: %d", got)
	}
	if got := send(s, s.listenHTTPS, "GET http://"+authority+"/ok"); got != http.StatusBadRequest {
		t.Errorf("GET on the https port: %d", got)
	}
	if code, _ := refusalOf(s); code != "" {
		t.Errorf("refusal %s", code)
	}
	// the port that serves nothing
	for _, c := range []struct{ name, line, host string }{
		{"ftp", "GET ftp://origin.example:" + port + "/x.bin", "ftp://origin.example"},
		{"sftp", "CONNECT " + authority, "sftp://origin.example"},
		{"sftp, a name of this computer", "CONNECT loop.example:" + port, "sftp://loop.example"},
	} {
		s := r.session("lms.publicuni.example")
		if got := send(s, s.listenOther, c.line); got != http.StatusForbidden {
			t.Errorf("%s: %d", c.name, got)
		}
		if code, host := refusalOf(s); code != CodeUnusualHost || host != c.host {
			t.Errorf("%s: refusal %s %q", c.name, code, host)
		}
		var f *failure
		if err := s.failureOf(ariaStatus{ErrorCode: "1", ErrorMessage: "Proxy connection failed."}); !errors.As(err, &f) || f.code != CodeUnusualHost {
			t.Errorf("%s: the task's failure %v", c.name, err)
		}
	}
	// a tunnel that does not start with a TLS handshake (a plain request,
	// an ssh banner): closed before a byte reaches the server, refused
	for _, first := range []string{"GET /ok HTTP/1.1\r\nHost: " + authority + "\r\n\r\n", "SSH-2.0-libssh2_1.11.0\r\n"} {
		s := r.session("lms.publicuni.example")
		status, c := rawConnect(t, s.listenHTTPS, authority, s.user, s.pass)
		if status != 200 {
			t.Fatalf("CONNECT %d", status)
		}
		io.WriteString(c, first)
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		n, err := c.Read(make([]byte, 64))
		c.Close()
		if n != 0 || err == nil || isTimeout(err) {
			t.Errorf("%q: the tunnel answered %d bytes (%v)", first[:3], n, err)
		}
		if code, host := refusalOf(s); code != CodeUnusualHost || host != "tcp://origin.example" {
			t.Errorf("%q: refusal %s %q", first[:3], code, host)
		}
	}
	if r.originHits.Load() != 0 {
		t.Errorf("the server got %d requests", r.originHits.Load())
	}
	// (only the two tunnels were connected; none of the others)
	if d := r.dialedAddrs(); len(d) != 2 {
		t.Errorf("connections made: %v", d)
	}
}

// userProxy is a stand-in for a proxy of the user's (HTTP_PROXY): it
// records what it is asked, serves an http request itself and answers
// CONNECT with a tunnel to tunnelTo.
type userProxy struct {
	srv      *httptest.Server
	tunnelTo string
	status   int // answer everything with this status (0: serve)

	mu   sync.Mutex
	seen []string // "METHOD target auth"
}

func newUserProxy(t *testing.T) *userProxy {
	p := &userProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.RequestURI
		p.mu.Lock()
		p.seen = append(p.seen, r.Method+" "+target+" "+r.Header.Get("Proxy-Authorization"))
		status, tunnelTo := p.status, p.tunnelTo
		p.mu.Unlock()
		if status != 0 {
			http.Error(w, "proxy says no", status)
			return
		}
		if r.Method != http.MethodConnect {
			io.WriteString(w, "VIA THE USER'S PROXY")
			return
		}
		back, err := net.DialTimeout("tcp", tunnelTo, 5*time.Second)
		if err != nil {
			http.Error(w, "no tunnel", http.StatusBadGateway)
			return
		}
		defer back.Close()
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
		go func() { io.Copy(back, rw); back.(*net.TCPConn).CloseWrite() }()
		io.Copy(c, back)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *userProxy) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// With an http:// proxy of the user's the guard goes on through it, with
// the proxy's own credentials and without the guard's; the name is the
// proxy's to resolve (no look-up here), and every hop is still judged by
// the list and by the host as written, as before the guard.
func TestGuardGoesThroughTheUsersProxy(t *testing.T) {
	r := newGuardRig(t)
	up := newUserProxy(t)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "SECURE") }))
	defer secure.Close()
	up.tunnelTo = secure.Listener.Addr().String()
	pu, _ := url.Parse(up.srv.URL)
	pu.User = url.UserPassword("alice", "s3cret")
	r.p.proxy = func(*url.URL) (*url.URL, error) { return pu, nil }
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	s := r.session("lms.publicuni.example")
	hc := r.client(s)
	// names the rig's look-up does not know: the proxy resolves them
	if status, body := get(hc, "http://media.elsewhere.example/v.mp4?sig=1"); status != 200 || body != "VIA THE USER'S PROXY" {
		t.Fatalf("http through the user's proxy: %d %q", status, body)
	}
	if status, body := get(hc, "https://secure.elsewhere.example/v.mp4"); status != 200 || body != "SECURE" {
		t.Fatalf("https through the user's proxy: %d %q", status, body)
	}
	want := []string{"GET http://media.elsewhere.example/v.mp4?sig=1 " + auth, "CONNECT secure.elsewhere.example:443 " + auth}
	if got := up.asked(); !slices.Equal(got, want) {
		t.Fatalf("the user's proxy was asked %q, want %q", got, want)
	}
	if len(r.dialedAddrs()) != 2 || r.lookupsOf("media.elsewhere.example") != 0 || r.lookupsOf("secure.elsewhere.example") != 0 {
		t.Errorf("connections %v, look-ups %v", r.dialedAddrs(), r.lookups)
	}
	for _, d := range r.dialedAddrs() {
		if d != up.srv.Listener.Addr().String() {
			t.Errorf("connected to %s, not the proxy", d)
		}
	}
	// the list and the host as written, hop by hop
	in := portOfServer(r.inner)
	for target, code := range map[string]string{
		"http://127.0.0.1:" + in + "/secret":     CodeLoopbackAddress,
		"https://127.0.0.1:" + in + "/secret":    CodeLoopbackAddress,
		"http://localhost:" + in + "/secret":     CodeLoopbackAddress,
		"http://2130706433:" + in + "/secret":    CodeLoopbackAddress,
		"http://10.0.0.5:" + in + "/secret":      CodeLocalAddress,
		"https://[fd00::7]:" + in + "/secret":    CodeLocalAddress,
		"http://printer.local:" + in + "/secret": CodeLocalAddress,
		"https://www.youtube.com/watch":          CodeSiteBlocked,
		"http://a;b.example/x":                   CodeUnusualHost,
	} {
		s := r.session("lms.publicuni.example")
		if status, body := get(r.client(s), target); status != http.StatusForbidden && status != 0 {
			t.Errorf("%s: %d %q", target, status, body)
		}
		if got, _ := refusalOf(s); got != code {
			t.Errorf("%s: refusal %q, want %s", target, got, code)
		}
	}
	if got := up.asked(); len(got) != 2 {
		t.Errorf("the user's proxy was asked for a refused address: %q", got)
	}
	if r.innerHits.Load() != 0 || r.innerConns.Load() != 0 {
		t.Fatal("the inner server was reached")
	}
	// the proxy refuses or fails: a failure of the network, with nothing
	// of the proxy in the detail
	up.mu.Lock()
	up.status = http.StatusForbidden
	up.mu.Unlock()
	s3 := r.session("lms.publicuni.example")
	if status, _ := rawConnect(t, s3.listenHTTPS, "secure.elsewhere.example:443", s3.user, s3.pass); status != http.StatusBadGateway {
		t.Errorf("CONNECT the proxy refuses: %d", status)
	}
	var f *failure
	if err := s3.failureOf(ariaStatus{ErrorCode: "1", ErrorMessage: "Proxy connection failed."}); !errors.As(err, &f) || f.code != CodeNetwork ||
		!strings.Contains(f.detail, "HTTP 403") || strings.Contains(f.detail, "s3cret") || strings.Contains(f.detail, "alice") ||
		strings.Contains(f.detail, up.srv.Listener.Addr().String()) {
		t.Errorf("failure %+v", f)
	}
	if logs := r.logs.String(); strings.Contains(logs, "s3cret") || strings.Contains(logs, "alice") {
		t.Errorf("the log names the proxy's user or password:\n%s", logs)
	}
	// a proxy that cannot be used refuses every request
	for _, raw := range []string{"socks5://127.0.0.1:1", "https://127.0.0.1:1"} {
		r.p.proxy = proxyAt(raw)
		s := r.session("lms.publicuni.example")
		if status, _ := get(r.client(s), r.originURL("/ok")); status != http.StatusForbidden {
			t.Errorf("%s: %d", raw, status)
		}
		if status, _ := rawConnect(t, s.listenHTTPS, "origin.example:443", s.user, s.pass); status != http.StatusForbidden {
			t.Errorf("%s: CONNECT %d", raw, status)
		}
		if code, _ := refusalOf(s); code != CodeProxyUnsupported {
			t.Errorf("%s: refusal %q", raw, code)
		}
	}
}

// SEC19 info-1: a proxy of the user's that cannot be reached is a failure
// of the network whose detail (the task's, and the log's line of it)
// does not name the proxy's address, for a plain request as for CONNECT;
// the log may name its host, never its user or password.
func TestGuardProxyFailureNamesNoProxy(t *testing.T) {
	r := newGuardRig(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := ln.Addr().String()
	ln.Close() // (nothing listens there: the proxy cannot be reached)
	_, gonePort, _ := net.SplitHostPort(gone)
	r.p.proxy = proxyAt("http://alice:s3cret@" + gone)
	for _, c := range []struct {
		name string
		st   ariaStatus
		do   func(s *guardSession) int
	}{
		{"http", ariaStatus{ErrorCode: "29", ErrorMessage: "The response status is not successful. status=502"},
			func(s *guardSession) int {
				status, _ := get(r.client(s), "http://media.elsewhere.example/v.mp4")
				return status
			}},
		{"CONNECT", ariaStatus{ErrorCode: "1", ErrorMessage: "Proxy connection failed."},
			func(s *guardSession) int {
				status, c := rawConnect(t, s.listenHTTPS, "secure.elsewhere.example:443", s.user, s.pass)
				if c != nil {
					c.Close()
				}
				return status
			}},
	} {
		s := r.session("lms.publicuni.example")
		if status := c.do(s); status != http.StatusBadGateway {
			t.Errorf("%s: %d", c.name, status)
		}
		var f *failure
		if err := s.failureOf(c.st); !errors.As(err, &f) || f.code != CodeNetwork {
			t.Fatalf("%s: %v", c.name, err)
		}
		if strings.Contains(f.detail, gonePort) || strings.Contains(f.detail, "s3cret") || strings.Contains(f.detail, "alice") ||
			!strings.Contains(f.detail, "proxy") {
			t.Errorf("%s: the detail %q", c.name, f.detail)
		}
	}
	if logs := r.logs.String(); strings.Contains(logs, "s3cret") || strings.Contains(logs, "alice") ||
		strings.Count(logs, "could not go through the proxy set") != 1 || !strings.Contains(logs, "proxy_host=127.0.0.1") {
		t.Errorf("the log of the proxy's failure:\n%s", logs)
	}
}

// The bytes are passed on as they arrive, and a session's connections end
// with it (a cancelled task), for a plain request and for a tunnel.
func TestGuardStreamsAndEndsWithTheSession(t *testing.T) {
	r := newGuardRig(t)
	s := r.session("lms.publicuni.example")
	hc := r.client(s)
	hc.Timeout = 0
	res, err := hc.Get(r.originURL("/stall"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// the first bytes arrive while the server still holds the rest
	buf := make([]byte, 1000)
	got := make(chan error, 1)
	go func() { _, err := io.ReadFull(res.Body, buf); got <- err }()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the first bytes: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first bytes were held back")
	}
	// a tunnel, idle
	status, tun := rawConnect(t, s.listenHTTPS, "origin.example:"+portOfServer(r.origin), s.user, s.pass)
	if status != 200 {
		t.Fatalf("CONNECT %d", status)
	}
	defer tun.Close()
	start := time.Now()
	s.close()
	rest := make(chan error, 2)
	go func() { _, err := io.Copy(io.Discard, res.Body); rest <- err }()
	go func() {
		tun.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err := tun.Read(make([]byte, 1))
		rest <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-rest:
			if err == nil {
				t.Errorf("a connection ended without an error: the body would look complete")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a connection of the closed session stayed open")
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the connections took %v to end", d)
	}
	// the session is gone: its port is closed, and its credentials open
	// no other
	for _, listen := range []string{s.listenHTTP, s.listenHTTPS, s.listenOther} {
		if c, err := net.DialTimeout("tcp", listen, time.Second); err == nil {
			c.Close()
			t.Errorf("the closed session's port %s still listens", listen)
		}
	}
	if status, _ := rawConnect(t, r.g.addr, "origin.example:"+portOfServer(r.origin), s.user, s.pass); status != 407 {
		t.Errorf("a closed session's credentials: %d", status)
	}
}

// A tunnel without a byte either way is closed after the idle time; one
// in use is not. No more than the limit of connections at once.
func TestGuardIdleTunnelsAndLimit(t *testing.T) {
	r := newGuardRig(t)
	// (another guard, with a short idle time and a low limit; the rig's
	// servers and network stay)
	g, err := newGuardWith(slog.New(slog.NewTextHandler(io.Discard, nil)), 2, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	s, err := g.open(r.p.forTask("lms.publicuni.example"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	// (the busy tunnel speaks TLS: the guard lets nothing else through a
	// tunnel)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "SECURE") }))
	defer secure.Close()
	authority := "origin.example:" + portOfServer(r.origin)
	busyAuthority := "example.com:" + portOfServer(secure)
	_, idle := rawConnect(t, s.listenHTTPS, authority, s.user, s.pass)
	_, busy := rawConnect(t, s.listenHTTPS, busyAuthority, s.user, s.pass)
	if idle == nil || busy == nil {
		t.Fatal("no tunnel")
	}
	defer idle.Close()
	defer busy.Close()
	// a third connection is over the limit
	if status, c := rawConnect(t, s.listenHTTPS, authority, s.user, s.pass); status != http.StatusServiceUnavailable {
		if c != nil {
			c.Close()
		}
		t.Errorf("over the limit: %d", status)
	}
	// the busy tunnel carries a request every 100 ms for a second
	busyTLS := tls.Client(busy, &tls.Config{InsecureSkipVerify: true})
	br := bufio.NewReader(busyTLS)
	for i := 0; i < 10; i++ {
		fmt.Fprintf(busyTLS, "GET /ok HTTP/1.1\r\nHost: %s\r\n\r\n", busyAuthority)
		busy.SetReadDeadline(time.Now().Add(5 * time.Second))
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("the tunnel in use was closed after %d requests: %v", i, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
	idle.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil || errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		t.Errorf("the idle tunnel was not closed: %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// The dialler itself (the product's, no test wire): it connects to the
// address it was given, asks the check about the address it connects to,
// and connects to nothing the check refuses.
func TestDialPinned(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p := &addrPolicy{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var checked []string
	c, err := p.dialPinned(ctx, netip.MustParseAddr("127.0.0.1"), port, func(address string) error {
		checked = append(checked, address)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !slices.Equal(checked, []string{"127.0.0.1:" + port}) {
		t.Errorf("checked %v", checked)
	}
	// the IPv4-mapped form of the same address: the check sees what is
	// connected to
	checked = nil
	made := int64(1)
	if c, err := p.dialPinned(ctx, netip.MustParseAddr("::ffff:127.0.0.1"), port, func(address string) error {
		checked = append(checked, address)
		return nil
	}); err == nil {
		c.Close()
		made++
	}
	for _, a := range checked {
		if ap, err := netip.ParseAddrPort(a); err != nil || !ap.Addr().Unmap().IsLoopback() {
			t.Errorf("checked %q", a)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for accepted.Load() < made && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	before := accepted.Load()
	if before != made {
		t.Fatalf("%d connections accepted for %d made", before, made)
	}
	// refused by the check: no connection
	refusal := &localAddrError{host: "loop.example", loopback: true}
	if c, err := p.dialPinned(ctx, netip.MustParseAddr("127.0.0.1"), port, func(string) error { return refusal }); err == nil {
		c.Close()
		t.Fatal("connected to a refused address")
	} else if f := localFailure(err); f == nil || f.code != CodeLoopbackAddress {
		t.Errorf("the refusal is lost in the dialler's error: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if accepted.Load() != before {
		t.Errorf("the listener accepted a refused connection")
	}
	// the session's own check, without the wire: the product's policy
	// refuses this computer when the dialler reaches it
	g, err := newGuard(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	strict := &addrPolicy{proxy: noProxy, lookup: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}}
	s, _ := g.open(strict.forTask(""))
	defer s.close()
	if c, err := s.dialTarget(ctx, "loop.example", port); err == nil {
		c.Close()
		t.Fatal("the session connected to this computer")
	} else if f := localFailure(err); f == nil || f.code != CodeLoopbackAddress {
		t.Errorf("%v", err)
	}
	if accepted.Load() != before {
		t.Errorf("the listener accepted a refused connection")
	}
	// with loopback allowed (the development switch, the tests) it connects
	loose := &addrPolicy{allowLoopback: true, proxy: noProxy}
	s2, _ := g.open(loose.forTask(""))
	defer s2.close()
	for _, host := range []string{"127.0.0.1", "localhost"} {
		c, err := s2.dialTarget(ctx, host, port)
		if err != nil {
			t.Fatalf("%s with loopback allowed: %v", host, err)
		}
		c.Close()
	}
}

// aria2c is started without the proxy variables, in any case.
func TestLaunchEnv(t *testing.T) {
	got := launchEnv([]string{"PATH=/bin", "http_proxy=http://a", "HTTP_PROXY=http://b", "Https_Proxy=http://c", "ftp_proxy=x",
		"ALL_PROXY=socks5://d", "all_proxy=e", "NO_PROXY=*", "no_proxy=localhost", "HTTP_PROXY_EXTRA=kept", "MY_no_proxy=kept", "LF_FAKE_ARIA2C=1", "=C:=C:\\"})
	want := []string{"PATH=/bin", "HTTP_PROXY_EXTRA=kept", "MY_no_proxy=kept", "LF_FAKE_ARIA2C=1", "=C:=C:\\"}
	if !slices.Equal(got, want) {
		t.Errorf("launchEnv: %q, want %q", got, want)
	}
	args := guardArgs("127.0.0.1:4711")
	for _, want := range []string{"--all-proxy=http://127.0.0.1:4711", "--http-proxy=http://127.0.0.1:4711", "--https-proxy=http://127.0.0.1:4711",
		"--ftp-proxy=http://127.0.0.1:4711", "--no-proxy=", "--proxy-method=get"} {
		if !slices.Contains(args, want) {
			t.Errorf("guardArgs without %s: %v", want, args)
		}
	}
}

// The update's own URLs may be a mirror on this computer or the local
// network; a redirect from them is judged like any other.
func TestGuardTrustsTheUpdatesOwnHosts(t *testing.T) {
	r := newGuardRig(t)
	r.p.dial = nil // (this computer for real: the product's dialler)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if to := req.URL.Query().Get("to"); to != "" {
			http.Redirect(w, req, to, http.StatusFound)
			return
		}
		io.WriteString(w, "MIRROR")
	}))
	defer mirror.Close()
	local := "http://localhost:" + portOfServer(mirror) + "/pkg.exe"
	s := r.session("", mirror.URL+"/pkg.exe", local)
	hc := r.client(s)
	for _, u := range []string{mirror.URL + "/pkg.exe", local} {
		if status, body := get(hc, u); status != 200 || body != "MIRROR" {
			t.Fatalf("%s: %d %q", u, status, body)
		}
	}
	// another port of this computer, by a redirect or directly: refused
	in := "http://127.0.0.1:" + portOfServer(r.inner) + "/secret"
	if status, _ := get(hc, mirror.URL+"/pkg.exe?to="+url.QueryEscape(in)); status != http.StatusForbidden {
		t.Errorf("a redirect from the mirror: %d", status)
	}
	if status, _ := get(hc, in); status != http.StatusForbidden {
		t.Errorf("another port: %d", status)
	}
	if r.innerHits.Load() != 0 || r.innerConns.Load() != 0 {
		t.Fatal("the inner server was reached")
	}
	// without the trust (a recording): the mirror's address is refused
	if status, _ := get(r.client(r.session("")), mirror.URL+"/pkg.exe"); status != http.StatusForbidden {
		t.Errorf("a recording's session: %d", status)
	}
}
