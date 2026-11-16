// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// How close to this computer a redirect chain starts is the address the
// first request's connection actually went to (SEC47 low-1), not the
// closest of the addresses its name resolves to when a redirect comes: a
// mirror whose DNS someone controls could otherwise answer [public,
// 127.0.0.1], or 127.0.0.1 the second time, and so lead the chain to this
// computer.

// The first request's name has a public and a local address; its
// connection went to the public one: no redirect to this computer or the
// local network is followed (the probe of SEC47 low-1, static records).
func TestRedirectFirstHopMultiRecord(t *testing.T) {
	e := newRedirectEnv(t)
	tlsPort := e.tls.Listener.Addr().(*net.TCPAddr).Port
	tlsAddr := e.tls.Listener.Addr().String()
	e.net.mu.Lock()
	e.net.lookup["evil.example.com"] = addrs("203.0.113.66", "127.0.0.1") // the connection: the first
	e.net.lookup["evil2.example.com"] = addrs("203.0.113.67", "10.9.9.9")
	e.net.lookup["mixed6.example.com"] = addrs("2001:db8::7", "10.0.0.7")
	e.net.route["evil.example.com:443"] = tlsAddr
	e.net.route["evil2.example.com:443"] = tlsAddr
	e.net.route["mixed6.example.com:443"] = tlsAddr
	e.net.route["192.168.1.1:443"] = tlsAddr // stands in for a router's https page
	e.net.mu.Unlock()
	c := e.client(t, nil)
	for _, tc := range []struct{ start, target string }{
		{"https://evil.example.com", fmt.Sprintf("https://127.0.0.1:%d/file", tlsPort)},
		{"https://evil.example.com", fmt.Sprintf("https://localhost:%d/file", tlsPort)},
		{"https://evil.example.com", fmt.Sprintf("https://[::1]:%d/file", tlsPort)},
		{"https://evil.example.com", "https://192.168.1.1/file"},
		{"https://evil.example.com", "https://storage.example.com/file"},
		{"https://evil2.example.com", "https://192.168.1.1/file"},
		{"https://mixed6.example.com", "https://storage.example.com/file"},
	} {
		wantRefused(t, c, to(tc.start, tc.target))
	}
	for _, h := range e.tlsHits.list() {
		if !strings.HasSuffix(h, "/to") {
			t.Errorf("requested %s", h)
		}
	}
	// a redirect to a public address is followed as before
	wantFollowed(t, c, to("https://evil.example.com", "https://cdn.example.com/file"))
}

// The first request's connection went to a public address; every later
// look-up of its name answers 127.0.0.1 (DNS rebinding): neither a
// redirect to this computer nor one within the name's own origin, which
// would be dialled to 127.0.0.1, is followed.
func TestRedirectFirstHopRebinding(t *testing.T) {
	e := newRedirectEnv(t)
	tlsPort := e.tls.Listener.Addr().(*net.TCPAddr).Port
	e.net.mu.Lock()
	e.net.lookup["rb.example.com"] = addrs("127.0.0.1") // the guard's look-ups
	e.net.dialSeq = map[string][]netip.Addr{"rb.example.com": addrs("203.0.113.68", "127.0.0.1")}
	e.net.route["rb.example.com:443"] = e.tls.Listener.Addr().String()
	e.net.mu.Unlock()
	c := e.client(t, nil)
	wantRefused(t, c, to("https://rb.example.com", fmt.Sprintf("https://127.0.0.1:%d/file", tlsPort)))
	// each case: new connections, the first one public again
	reset := func() {
		c.CloseIdleConnections()
		e.net.mu.Lock()
		e.net.dials = nil
		e.net.mu.Unlock()
	}
	reset()
	wantRefused(t, c, to("https://rb.example.com", "/file"))
	if e.tlsHits.has("rb.example.com/file") || e.tlsHits.has(fmt.Sprintf("127.0.0.1:%d/file", tlsPort)) {
		t.Errorf("hits %q", e.tlsHits.list())
	}
	// the look-up answers public, the redirect's own connection 127.0.0.1:
	// refused when it is dialled
	e.net.mu.Lock()
	e.net.lookup["rb.example.com"] = addrs("203.0.113.68")
	e.net.mu.Unlock()
	reset()
	wantRefused(t, c, to("https://rb.example.com", "/file"))
	if e.tlsHits.has("rb.example.com/file") {
		t.Errorf("hits %q", e.tlsHits.list())
	}
}

// A first request on a connection reused from the pool tells where that
// connection goes all the same (GotConn): a school's mirror keeps its
// redirects to the local network.
func TestRedirectFirstHopReusedConnection(t *testing.T) {
	e := newRedirectEnv(t)
	c := e.client(t, nil)
	dials := func() int {
		e.net.mu.Lock()
		defer e.net.mu.Unlock()
		n := 0
		for _, a := range e.net.dialed {
			if a == "school.example.com:443" {
				n++
			}
		}
		return n
	}
	wantFollowed(t, c, "https://school.example.com/file")
	before := dials()
	wantFollowed(t, c, to("https://school.example.com", "https://storage.example.com/file"))
	if before != 1 || dials() != before {
		t.Fatalf("connections to the mirror: %d, then %d (want one, reused)", before, dials())
	}
}

// The chain's state comes with the request's context (the downloads put it
// there: withRedirectChain). A request without it starts from a public
// source whatever its connection: a named mirror on the local network
// redirects only to public addresses then.
func TestRedirectWithoutChainStateIsPublic(t *testing.T) {
	e := newRedirectEnv(t)
	c := e.client(t, nil)
	u := to("https://school.example.com", "https://storage.example.com/file")
	if _, err := getBodyCtx(context.Background(), c, u); err == nil {
		t.Fatalf("%s: followed without the chain's state", u)
	}
	wantFollowed(t, c, u)
	// a host local as written stays so
	if b, err := getBodyCtx(context.Background(), c, to("http://10.0.0.5", "https://storage.example.com/file")); err != nil || string(b) != string(redirectContent) {
		t.Fatalf("local by its address: %q %v", b, err)
	}
}

// The reach of a chain's start is the farthest from this computer of the
// connections its first request used, an address that cannot be read
// being public.
func TestRedirectChainReach(t *testing.T) {
	tcp := func(s string) net.Addr { return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s)) }
	for _, tc := range []struct {
		conns []net.Addr
		want  reach
	}{
		{[]net.Addr{tcp("10.0.0.5:443")}, reachLocalNet},
		{[]net.Addr{tcp("127.0.0.1:8080")}, reachLoopback},
		{[]net.Addr{tcp("[::1]:443")}, reachLoopback},
		{[]net.Addr{tcp("[fe80::1%eth0]:443")}, reachLocalNet},
		{[]net.Addr{tcp("127.0.0.1:443"), tcp("203.0.113.5:443")}, reachPublic},
		{[]net.Addr{tcp("10.0.0.5:443"), tcp("127.0.0.1:443")}, reachLocalNet},
		{[]net.Addr{tcp("0.0.0.0:443")}, reachPublic},
		{[]net.Addr{&net.UnixAddr{Name: "x", Net: "unix"}}, reachPublic},
	} {
		c := &redirectChain{}
		for _, a := range tc.conns {
			c.connected(a)
		}
		if got, ok := c.get(); !ok || got != tc.want {
			t.Errorf("%v: %d %v, want %d", tc.conns, got, ok, tc.want)
		}
	}
	if _, ok := (&redirectChain{}).get(); ok {
		t.Error("a chain without a connection has a reach")
	}
}
