// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Redirects of the component downloads (security audit AUD4 L-3): no
// network; the host names below are answered by fakeNet and every
// connection goes to a local test server.

// hitLog records the requests a test server received ("host/path").
type hitLog struct {
	mu   sync.Mutex
	hits []string
}

func (h *hitLog) add(s string) {
	h.mu.Lock()
	h.hits = append(h.hits, s)
	h.mu.Unlock()
}

func (h *hitLog) list() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.hits...)
}

func (h *hitLog) has(s string) bool { return slices.Contains(h.list(), s) }

// redirectHandler serves /file (content), /to?u=URL (302 to URL) and
// /chain/N (N relative redirects, then the content).
func redirectHandler(hits *hitLog, content []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.add(r.Host + r.URL.Path)
		switch {
		case r.URL.Path == "/file":
			w.Write(content)
		case r.URL.Path == "/to":
			http.Redirect(w, r, r.URL.Query().Get("u"), http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/chain/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/chain/"))
			if n <= 0 {
				w.Write(content)
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/chain/%d", n-1), http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
}

// fakeNet answers the tests' host names: lookup is what the resolver says
// (the redirect check); a connection to a name is judged by dialIP (default:
// lookup's first address; dialSeq, when set, gives the address of the
// name's first, second, ... connection, its last one after that) and goes
// to route's real server. The connection's RemoteAddr is the address it
// was judged by (what a real connection to that address would report),
// not the test server's. A literal loopback address without a route is
// connected to as it is; anything else without a route is never connected
// to.
type fakeNet struct {
	mu      sync.Mutex
	lookup  map[string][]netip.Addr
	dialIP  map[string]netip.Addr
	dialSeq map[string][]netip.Addr
	route   map[string]string // "host:port" → the real address
	dialed  []string          // "host:port" of every connection made
	dials   map[string]int    // connections made to a name (dialSeq)
}

// fakePeerConn is a connection to a test server that reports the address
// fakeNet judged it by as its peer.
type fakePeerConn struct {
	net.Conn
	peer net.Addr
}

func (c *fakePeerConn) RemoteAddr() net.Addr { return c.peer }

// fakeHostKey is a host name as fakeNet's maps key it.
func fakeHostKey(host string) string { return strings.TrimSuffix(strings.ToLower(host), ".") }

func (n *fakeNet) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if a, ok := n.lookup[fakeHostKey(host)]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (n *fakeNet) dial(ctx context.Context, network, addr string, check func(netip.Addr) error) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(portText)
	n.mu.Lock()
	ip, perr := netip.ParseAddr(host)
	if perr != nil {
		key := fakeHostKey(host)
		ip = n.dialIP[key]
		if seq := n.dialSeq[key]; len(seq) > 0 {
			if n.dials == nil {
				n.dials = map[string]int{}
			}
			ip = seq[min(n.dials[key], len(seq)-1)]
			n.dials[key]++
		}
		if a := n.lookup[key]; !ip.IsValid() && len(a) > 0 {
			ip = a[0]
		}
	}
	target, routed := n.route[addr]
	n.mu.Unlock()
	if !ip.IsValid() {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	if check != nil {
		if err := check(ip); err != nil {
			return nil, &net.OpError{Op: "dial", Net: network, Err: err}
		}
	}
	if !routed {
		if perr != nil || !ip.Unmap().IsLoopback() {
			return nil, errors.New("test: no route to " + addr)
		}
		target = addr
	}
	n.mu.Lock()
	n.dialed = append(n.dialed, addr)
	n.mu.Unlock()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, target)
	if err != nil {
		return nil, err
	}
	return &fakePeerConn{Conn: conn, peer: &net.TCPAddr{IP: ip.AsSlice(), Port: port, Zone: ip.Zone()}}, nil
}

func (n *fakeNet) wasDialed(addr string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Contains(n.dialed, addr)
}

var redirectContent = []byte("component file content")

// redirectEnv: one TLS server (its certificate covers example.com and
// *.example.com, 127.0.0.1 and ::1) and one plain HTTP server behind the
// names of fakeNet.
type redirectEnv struct {
	tls, plain *httptest.Server
	tlsHits    *hitLog
	plainHits  *hitLog
	net        *fakeNet
}

func addrs(s ...string) []netip.Addr {
	var out []netip.Addr
	for _, a := range s {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

func newRedirectEnv(t *testing.T) *redirectEnv {
	t.Helper()
	e := &redirectEnv{tlsHits: &hitLog{}, plainHits: &hitLog{}}
	e.tls = httptest.NewTLSServer(redirectHandler(e.tlsHits, redirectContent))
	t.Cleanup(e.tls.Close)
	e.plain = httptest.NewServer(redirectHandler(e.plainHits, redirectContent))
	t.Cleanup(e.plain.Close)
	tlsAddr, plainAddr := e.tls.Listener.Addr().String(), e.plain.Listener.Addr().String()
	e.net = &fakeNet{
		lookup: map[string][]netip.Addr{
			"mirror.example.com":  addrs("203.0.113.10"), // a public mirror
			"cdn.example.com":     addrs("203.0.113.20", "2001:db8::20"),
			"lan.example.com":     addrs("192.168.5.5"),
			"rebind.example.com":  addrs("203.0.113.30"), // public to the check, this computer to the dial
			"school.example.com":  addrs("10.0.0.5"),     // a school's mirror on its network
			"storage.example.com": addrs("10.0.0.6"),
			"mixed.example.com":   addrs("2001:db8::7", "10.0.0.7"),
			"fakeip.example.com":  addrs("198.18.0.42"), // a proxy client's "fake IP" answer
		},
		dialIP: map[string]netip.Addr{"rebind.example.com": netip.MustParseAddr("127.0.0.1")},
		route:  map[string]string{"cdn.example.com:80": plainAddr, "10.0.0.5:80": plainAddr},
	}
	for name := range e.net.lookup {
		e.net.route[name+":443"] = tlsAddr
	}
	return e
}

// client is a guarded client trusting the TLS server, without a proxy.
func (e *redirectEnv) client(t *testing.T, proxy func(*http.Request) (*url.URL, error)) *http.Client {
	t.Helper()
	base := e.tls.Client().Transport.(*http.Transport).Clone()
	base.Proxy = proxy
	g := newRedirectGuard(base, e.net.resolve, e.net.dial)
	t.Cleanup(g.CloseIdleConnections)
	return &http.Client{Transport: g, CheckRedirect: g.checkRedirect, Timeout: 20 * time.Second}
}

// getBody GETs u as the downloads do (the redirect chain's state in the
// request's context: withRedirectChain).
func getBody(c *http.Client, u string) ([]byte, error) {
	return getBodyCtx(withRedirectChain(context.Background()), c, u)
}

func getBodyCtx(ctx context.Context, c *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func to(start, target string) string { return start + "/to?u=" + url.QueryEscape(target) }

func wantFollowed(t *testing.T, c *http.Client, u string) {
	t.Helper()
	b, err := getBody(c, u)
	if err != nil || string(b) != string(redirectContent) {
		t.Fatalf("%s: %q %v, want the file", u, b, err)
	}
}

func wantRefused(t *testing.T, c *http.Client, u string) {
	t.Helper()
	b, err := getBody(c, u)
	var rr *redirectRefusedError
	if !errors.As(err, &rr) {
		t.Fatalf("%s: %q %v, want a refused redirect", u, b, err)
	}
	if transient(err) || classify(err) != CodeRedirectRefused {
		t.Errorf("%s: transient %v, code %s: want a non-transient %s", u, transient(err), classify(err), CodeRedirectRefused)
	}
	if info := ErrorInfoOf(asCoded(err, nil)); info.Code != CodeRedirectRefused || info.Params["host"] != printableHost(rr.host) {
		t.Errorf("%s: %+v, want %s {host: %q}", u, info, CodeRedirectRefused, rr.host)
	}
}

// From a public source a redirect is followed to public https addresses:
// a CDN (Hugging Face, GitHub, the mirrors), the source's own paths, a
// proxy client's fake IP; five hops at most.
func TestRedirectFromPublicSourceFollowed(t *testing.T) {
	e := newRedirectEnv(t)
	c := e.client(t, nil)
	wantFollowed(t, c, to("https://mirror.example.com", "https://cdn.example.com/file"))
	wantFollowed(t, c, to("https://mirror.example.com", "/file"))
	wantFollowed(t, c, to("https://mirror.example.com", "https://fakeip.example.com/file"))
	wantFollowed(t, c, "https://mirror.example.com/chain/5")
	if !e.tlsHits.has("cdn.example.com/file") || !e.tlsHits.has("fakeip.example.com/file") {
		t.Errorf("hits %q", e.tlsHits.list())
	}
	before := len(e.tlsHits.list())
	wantRefused(t, c, "https://mirror.example.com/chain/6")
	if got := e.tlsHits.list()[before:]; len(got) != 6 || slices.Contains(got, "mirror.example.com/chain/0") {
		t.Errorf("requests of a chain of six redirects: %q, want the first request and five redirects", got)
	}
}

// From a public source no redirect leads to plain http, this computer, the
// local network or an address that is no single server's, as written or
// by what the name resolves to; nothing is requested there.
func TestRedirectFromPublicSourceRefused(t *testing.T) {
	e := newRedirectEnv(t)
	c := e.client(t, nil)
	tlsPort := e.tls.Listener.Addr().(*net.TCPAddr).Port
	targets := []string{
		"http://cdn.example.com/file", // https → http
		fmt.Sprintf("https://127.0.0.1:%d/file", tlsPort),
		fmt.Sprintf("https://[::1]:%d/file", tlsPort),
		fmt.Sprintf("https://localhost:%d/file", tlsPort),
		"https://app.localhost/file",
		"https://10.1.2.3/file",
		"https://172.16.0.1/file",
		"https://192.168.1.1/file",
		"https://169.254.169.254/latest",
		"https://100.64.0.1/file",
		"https://[fc00::1]/file",
		"https://[fe80::1]/file",
		"https://[fec0::1]/file",
		"https://[::ffff:127.0.0.1]/file",
		"https://[::ffff:10.0.0.1]/file",
		"https://[64:ff9b::a01:203]/file",   // NAT64 of 10.1.2.3
		"https://[2002:c0a8:101::1]/file",   // 6to4 of 192.168.1.1
		"https://[::7f00:1]/file",           // IPv4-compatible
		"https://2130706433/file",           // 127.0.0.1 to most resolvers
		"https://0x7f.1/file",               //
		"https://127.1/file",                //
		"https://0.0.0.0/file",              //
		"https://[::]/file",                 //
		"https://240.0.0.1/file",            // reserved
		"https://255.255.255.255/file",      // broadcast
		"https://224.0.0.1/file",            // multicast
		"https://printer.local/file",        // mDNS
		"https://lan.example.com/file",      // resolves to 192.168.5.5
		"https://a..example.com/file",       // an empty label
		"https://storage.example.com/file",  // resolves to 10.0.0.6
		"https://school.example.com/file",   //
		"https://mixed.example.com/file",    // one of its addresses is local
		"https://lan.example.com:8443/file", //
		"https://LAN.Example.COM./file",     //
	}
	for _, target := range targets {
		wantRefused(t, c, to("https://mirror.example.com", target))
	}
	for _, h := range e.tlsHits.list() {
		if !strings.HasPrefix(h, "mirror.example.com/") {
			t.Errorf("requested %s", h)
		}
	}
	if len(e.plainHits.list()) != 0 {
		t.Errorf("plain HTTP requests: %q", e.plainHits.list())
	}
}

// A name that resolves to a public address for the check and to this
// computer when the connection is made is refused at the connection.
func TestRedirectRebindingRefusedAtDial(t *testing.T) {
	e := newRedirectEnv(t)
	c := e.client(t, nil)
	wantRefused(t, c, to("https://mirror.example.com", "https://rebind.example.com/file"))
	if e.net.wasDialed("rebind.example.com:443") || e.tlsHits.has("rebind.example.com/file") {
		t.Errorf("connected: dialled %q, hits %q", e.net.dialed, e.tlsHits.list())
	}
}

// A school's mirror on its own network (by its addresses, a plain http
// address, or a name with a local address among public ones whose
// connection went to the local one) keeps working, and so do its
// redirects to the local network; not to this computer.
func TestRedirectFromLocalNetworkSource(t *testing.T) {
	e := newRedirectEnv(t)
	e.net.mu.Lock()
	e.net.dialIP["mixed.example.com"] = netip.MustParseAddr("10.0.0.7") // (lookup's first is public)
	e.net.mu.Unlock()
	c := e.client(t, nil)
	tlsPort := e.tls.Listener.Addr().(*net.TCPAddr).Port
	wantFollowed(t, c, "https://school.example.com/file")
	wantFollowed(t, c, to("https://school.example.com", "https://storage.example.com/file"))
	wantFollowed(t, c, to("https://school.example.com", "https://cdn.example.com/file"))
	wantFollowed(t, c, to("https://mixed.example.com", "https://storage.example.com/file"))
	wantFollowed(t, c, to("http://10.0.0.5", "https://storage.example.com/file"))
	wantFollowed(t, c, to("http://10.0.0.5", "/file"))
	wantRefused(t, c, to("https://school.example.com", fmt.Sprintf("https://127.0.0.1:%d/file", tlsPort)))
	wantRefused(t, c, to("https://school.example.com", "https://app.localhost/file"))
	wantRefused(t, c, to("https://school.example.com", "http://storage.example.com/file"))
	wantRefused(t, c, to("http://10.0.0.5", "http://cdn.example.com/file"))
}

// The user's own mirror on this computer (a custom source on plain http:
// InsecureCustomSource allows it) works with the production client:
// redirects within its origin are followed (five at most); to another
// origin over plain http they are not.
func TestRedirectFromThisComputerSource(t *testing.T) {
	hits, other := &hitLog{}, &hitLog{}
	local := httptest.NewServer(redirectHandler(hits, redirectContent))
	defer local.Close()
	elsewhere := httptest.NewServer(redirectHandler(other, redirectContent))
	defer elsewhere.Close()
	c := defaultHTTPClient()
	defer c.CloseIdleConnections()
	wantFollowed(t, c, local.URL+"/file")
	wantFollowed(t, c, to(local.URL, "/file"))
	wantFollowed(t, c, to(local.URL, local.URL+"/file"))
	wantFollowed(t, c, local.URL+"/chain/5")
	wantRefused(t, c, local.URL+"/chain/6")
	wantRefused(t, c, to(local.URL, elsewhere.URL+"/file"))
	wantRefused(t, c, to(local.URL, strings.Replace(local.URL, "127.0.0.1", "localhost", 1)+"/file"))
	if len(other.list()) != 0 {
		t.Errorf("requested %q", other.list())
	}
}

// connectProxy is an HTTP CONNECT proxy on 127.0.0.1 (as a proxy client on
// the user's computer) that takes every tunnel to target.
func connectProxy(t *testing.T, target string, seen *hitLog) *httptest.Server {
	t.Helper()
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		seen.add(r.Host)
		up, err := net.DialTimeout("tcp", target, 5*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			up.Close()
			return
		}
		conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		done := make(chan struct{})
		go func() {
			io.Copy(up, conn)
			up.Close()
			close(done)
		}()
		io.Copy(conn, up)
		conn.Close()
		<-done
	}))
	t.Cleanup(p.Close)
	return p
}

// With the user's proxy (on this computer) the proxy resolves the names:
// the connection to the proxy is not refused, redirects to public names go
// through it, and a host that is local as written is still refused.
func TestRedirectThroughUserProxy(t *testing.T) {
	e := newRedirectEnv(t)
	seen := &hitLog{}
	proxy := connectProxy(t, e.tls.Listener.Addr().String(), seen)
	pu, _ := url.Parse(proxy.URL)
	c := e.client(t, func(r *http.Request) (*url.URL, error) {
		if r.URL.Scheme == "https" && strings.HasSuffix(r.URL.Hostname(), "example.com") {
			return pu, nil
		}
		return nil, nil
	})
	wantFollowed(t, c, to("https://mirror.example.com", "https://cdn.example.com/file"))
	if got := seen.list(); !slices.Contains(got, "mirror.example.com:443") || !slices.Contains(got, "cdn.example.com:443") {
		t.Errorf("tunnels %q", got)
	}
	wantRefused(t, c, to("https://mirror.example.com", "https://10.1.2.3/file"))
	wantRefused(t, c, to("https://mirror.example.com", "https://printer.local/file"))
}

// The production dial judges the address before connecting: a refused one
// never reaches the listener.
func TestDialCheckedJudgesBeforeConnecting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var judged []netip.Addr
	_, err = dialChecked(ctx, "tcp", ln.Addr().String(), func(ip netip.Addr) error {
		judged = append(judged, ip)
		return &redirectRefusedError{host: "x", reason: "test"}
	})
	var rr *redirectRefusedError
	if !errors.As(err, &rr) || len(judged) != 1 || judged[0] != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("refused dial: %v, judged %v", err, judged)
	}
	select {
	case <-accepted:
		t.Fatal("a refused address was connected to")
	case <-time.After(200 * time.Millisecond):
	}
	for _, check := range []func(netip.Addr) error{nil, func(netip.Addr) error { return nil }} {
		conn, err := dialChecked(ctx, "tcp", ln.Addr().String(), check)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		select {
		case <-accepted:
		case <-time.After(5 * time.Second):
			t.Fatal("not connected")
		}
	}
}

func TestReachOfAddressesAndHosts(t *testing.T) {
	ips := map[string]reach{
		"8.8.8.8": reachPublic, "203.0.113.5": reachPublic, "198.18.0.1": reachPublic, "2606:4700::1": reachPublic,
		"2001:db8::1": reachPublic, "64:ff9b::808:808": reachPublic, "2002:808:808::1": reachPublic,
		"127.0.0.1": reachLoopback, "127.255.0.9": reachLoopback, "::1": reachLoopback, "::ffff:127.0.0.1": reachLoopback,
		"10.0.0.1": reachLocalNet, "172.31.255.1": reachLocalNet, "192.168.0.1": reachLocalNet, "169.254.1.1": reachLocalNet,
		"100.64.0.1": reachLocalNet, "100.127.255.255": reachLocalNet, "fc00::1": reachLocalNet, "fd12::1": reachLocalNet,
		"fe80::1": reachLocalNet, "fe80::1%eth0": reachLocalNet, "fec0::1": reachLocalNet, "64:ff9b::a00:1": reachLocalNet,
		"64:ff9b:1::7f00:1": reachLoopback, "::ffff:0:a00:1": reachLocalNet, "2002:7f00:1::": reachLoopback,
		"100.128.0.1": reachPublic, "2001:0:4136:e378:8000:63bf:f5ff:fffe": reachLocalNet, // (Teredo, client 10.0.0.1)
		"0.0.0.0": reachNever, "0.1.2.3": reachNever, "::": reachNever, "224.0.0.1": reachNever, "ff02::1": reachNever,
		"240.0.0.1": reachNever, "255.255.255.255": reachNever, "::808:808": reachNever, "64:ff9b::1.2.3.4": reachPublic,
	}
	for s, want := range ips {
		if got := ipReach(netip.MustParseAddr(s)); got != want {
			t.Errorf("ipReach(%s) = %d, want %d", s, got, want)
		}
	}
	hosts := map[string]struct {
		r     reach
		final bool
	}{
		"huggingface.co": {reachPublic, false}, "HF-Mirror.com.": {reachPublic, false},
		"localhost": {reachLoopback, true}, "LOCALHOST.": {reachLoopback, true}, "a.localhost": {reachLoopback, true},
		"printer.local": {reachLocalNet, true}, "10.0.0.1": {reachLocalNet, true}, "::1": {reachLoopback, true},
		"2130706433": {reachNever, true}, "0x7f.1": {reachNever, true}, "127.1": {reachNever, true}, "a.0x": {reachNever, true},
		"": {reachNever, true}, "a..b": {reachNever, true}, "a b": {reachNever, true}, "ｌｏｃａｌｈｏｓｔ": {reachNever, true},
		"a%2e.b": {reachNever, true}, "x.example.1a": {reachPublic, false},
	}
	for h, want := range hosts {
		if r, final := hostReach(h); r != want.r || final != want.final {
			t.Errorf("hostReach(%q) = %d %v, want %d %v", h, r, final, want.r, want.final)
		}
	}
}

// A mirror that redirects somewhere it may not lead fails at once (no
// retries of the same answer) and the download falls back to the default
// source, saying so.
func TestMirrorRedirectRefusedFallsBack(t *testing.T) {
	e := newTestEnv(t)
	hits := &hitLog{}
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.add(r.URL.Path)
		http.Redirect(w, r, e.url(r.URL.Path), http.StatusFound) // another origin, plain http
	}))
	defer redir.Close()
	man := e.manifest(defaultSpec())
	g := man.Sources[GroupModels]
	g.Mirrors = []SourceDef{{ID: "hf-mirror", Region: "cn", Values: map[string]string{"models": redir.URL + "/mirror/hf"}}}
	man.Sources[GroupModels] = g
	if err := os.WriteFile(filepath.Join(e.manDir, "components.json"), marshalManifest(t, man), 0o644); err != nil {
		t.Fatal(err)
	}
	m := e.open()
	st := mustInstall(t, m, Sources{Models: "hf-mirror"}, ovModel)
	checkModelFile(t, m, ovContent(e))
	if got := hits.list(); len(got) != 1 || got[0] != "/mirror/hf/ov-tiny/model.xml" {
		t.Errorf("mirror requests %q, want one", got)
	}
	if e.srv.count("/mirror/") != 0 || e.srv.count(ovFile) != 1 {
		t.Errorf("requests %+v: the redirect was followed", e.srv.log())
	}
	if len(st.Notices) != 1 || st.Notices[0].Code != CodeMirrorFallback || st.Notices[0].Params["reason"] != CodeRedirectRefused {
		t.Errorf("notices %+v", st.Notices)
	}
}

// The last source (here the only one, the default) redirects somewhere
// the downloads do not follow: the task fails at once with
// components.redirect_refused {host} (SEC47 low-3), not with
// components.network, whose text promises that a retry continues; the
// redirect is asked once, not retried.
func TestDefaultSourceRedirectRefused(t *testing.T) {
	e := newTestEnv(t)
	hits := &hitLog{}
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.add(r.URL.Path)
		http.Redirect(w, r, e.url(r.URL.Path), http.StatusFound) // another origin, plain http
	}))
	defer redir.Close()
	man := e.manifest(defaultSpec())
	g := man.Sources[GroupModels]
	g.Default.Values = map[string]string{"models": redir.URL + "/default/hf"}
	g.Mirrors = nil
	man.Sources[GroupModels] = g
	if err := os.WriteFile(filepath.Join(e.manDir, "components.json"), marshalManifest(t, man), 0o644); err != nil {
		t.Fatal(err)
	}
	m := e.open()
	id, err := m.Install([]string{ovModel}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled)
	if st.State != TaskFailed || st.Error == nil || st.Error.Code != CodeRedirectRefused || st.Error.Params["host"] != "127.0.0.1" {
		t.Fatalf("%s %+v, want failed with %s {host: 127.0.0.1}", st.State, st.Error, CodeRedirectRefused)
	}
	if got := hits.list(); len(got) != 1 {
		t.Errorf("requests %q, want one", got)
	}
	if e.srv.count("/default/") != 0 || len(st.Notices) != 0 {
		t.Errorf("requests %+v, notices %+v", e.srv.log(), st.Notices)
	}
}
