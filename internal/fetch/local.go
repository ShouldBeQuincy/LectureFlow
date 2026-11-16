// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"lectureflow/internal/blocklist"
	"lectureflow/internal/db"
)

// Local addresses (security review L1, blind SSRF). The URLs a download
// requests come from a web page (through the extension) and from the
// playlists and MPDs that page's servers return; a public page must not
// make LectureFlow request something on this computer or the user's
// network. The classes (classify*):
//
//   - this computer: loopback (127.0.0.0/8, ::1, localhost, *.localhost),
//     always refused (DevLoopbackEnv and this package's tests aside);
//     unspecified addresses (0.0.0.0/8, ::), multicast, the limited
//     broadcast and hosts that resolvers read as a number ("2130706433",
//     "0x7f.1", "127.1") are refused always;
//   - the local network: private (RFC 1918, fc00::/7, the old fec0::/10),
//     link-local (169.254.0.0/16, fe80::/10), carrier-grade NAT
//     (100.64.0.0/10) and *.local (mDNS) are reached
//     only when the recording's page is on the local network itself (its
//     host is such an address or name, or resolves to such addresses only;
//     lead 2026-10-27: a self-hosted Opencast behind the school's VPN has
//     its media there; a public page sending LectureFlow there is the
//     attack, also when the media host is on the page's own site, whose
//     owner can point any of its names at a private address);
//   - everything else is public.
//
// Where (the same rule everywhere, per task: taskAddr):
//
//   - enqueue and refresh: the track URLs by their host as written, this
//     computer only (request.invalid {field: "tracks.url", reason:
//     "local_address"}); the local network needs the page's addresses,
//     looked up when the task runs;
//   - Go's own requests (playlists, MPDs, the redirect probes): the URL and
//     every redirect hop as written, and every connection by the address it
//     actually dials (net.Dialer.Control: a name resolving to this computer,
//     or answering differently the second time, is refused all the same);
//     each task has its own transport, so a connection one task may use is
//     never lent to another; a connection to the user's proxy is not
//     checked, the proxy resolves the names it is given;
//   - what aria2c gets (segments, keys, files after Go's redirects): the
//     host as written, and its addresses when no proxy of the user's
//     applies (upstream: one answer for this check and for the guard
//     proxy, whatever proxy variables aria2c would read itself);
//   - every connection aria2c then makes, a redirect's included: aria2c
//     connects to nothing itself, the guard proxy (guard.go) judges each
//     request by the same rules, looks the name up and connects to an
//     address it allowed. (aria2c follows redirects by itself and has no
//     option to stop that, and it would look every name up again: SEC16
//     R1 and R2.)
//
// What stays open, and why (NOTES "sec-fetch"): with a proxy of the
// user's, the proxy resolves the names, so only the host as written is
// judged, hop by hop; and of an https hop the guard sees the host alone,
// not the path (the do-not-handle list's path entries).

// DevLoopbackEnv is the development switch of the native end-to-end test
// (extension/test/native, whose fake platform serves its media on
// 127.0.0.1): "1" lets downloads reach loopback addresses (127.0.0.0/8,
// ::1, localhost). The product never sets it; the other classes keep their
// rules. Like LECTUREFLOW_DEV_POWER, the variable itself is the switch.
const DevLoopbackEnv = "LECTUREFLOW_DEV_DOWNLOAD_LOOPBACK"

// addrClass is where an address is.
type addrClass int

const (
	classPublic   addrClass = iota
	classLocalNet           // private, link-local, *.local
	classLoopback           // this computer: loopback, localhost
	classUnsafe             // unspecified (0.0.0.0/8, ::), or a number resolvers read as an address: this computer
	// no single server's address: multicast, the limited broadcast, an
	// IPv4-compatible address of a public one (nothing routes it), an IPv6
	// address with 0.0.0.0/8 inside (NAT64, 6to4, Teredo...). Refused
	// always, as an address LectureFlow cannot handle safely
	// (downloads.unusual_host), not as this computer (SEC19 low-8)
	classNotUnicast
	// a name with characters that cannot be judged as written
	// (blocklist.UnusualHost: outside ASCII, control characters, a percent
	// sign): net/http and the resolvers map it by IDNA ("ｌｏｃａｌｈｏｓｔ" is
	// localhost), so it is refused whatever it would map to (SEC10 info-1);
	// so is a name with any other character than letters, digits, "-", "_"
	// and dots, or an empty label ("localhost..", "a;b.example"): Go's
	// resolver refuses it, aria2c's may look it up (SEC11 low-1)
	classUnusual
)

// classifyIP tells where ip is (SEC16 R4, SEC19 low-5 for the ranges
// after the first ones):
//
//   - this computer: 127.0.0.0/8, ::1;
//   - unsafe, never connected to: 0.0.0.0/8 and :: (this computer to
//     most systems: downloads.loopback_address); multicast (224.0.0.0/4,
//     ff00::/8) and the limited broadcast 255.255.255.255, no single
//     server's address (classNotUnicast: downloads.unusual_host);
//   - the local network: RFC 1918, link-local (169.254.0.0/16,
//     fe80::/10), fc00::/7, the old site-local fec0::/10, and the shared
//     address space of carrier-grade NAT 100.64.0.0/10 (Tailscale's
//     addresses too);
//   - an IPv6 address with an IPv4 address inside is where that IPv4
//     address is, since that is where a translator or a relay connects
//     (ipv4Inside): NAT64 64:ff9b::/96 and the local-use 64:ff9b:1::/48
//     (the IPv4 address in the last 32 bits), IPv4-translated
//     ::ffff:0:0/96, 6to4 2002::/16, Teredo 2001::/32 (the server's and the
//     client's address, the stricter of the two), and the deprecated
//     IPv4-compatible ::/96, which nothing routes: classNotUnicast unless
//     what is inside is refused anyway. So an IPv6-only network still
//     reaches public servers through NAT64, and nothing on this computer
//     or the local network that way. An address of 0.0.0.0/8 inside is
//     classNotUnicast, not classUnsafe: a translator or relay connects to
//     no server for it, and it is not this computer either (the IPv6
//     address is not 0.0.0.0 to any system; SEC19 low-8, the rest of it).
//     An IPv4-mapped ::ffff:0.0.0.0/104 is 0.0.0.0/8 itself (Unmap);
//   - everything else is public.
//
// Known limits (SEC19 info-12; rare, left so): a NAT64 prefix of the
// network's own, not 64:ff9b::, looks like any public IPv6 address and is
// not told apart; and 64:ff9b:1::/48 is read in the /96 layout only (RFC
// 6052): deployed with a /48, /56 or /64 prefix the IPv4 address is
// elsewhere (64:ff9b:1:a00:5:0:808:808 is judged by 8.8.8.8, public), so
// such a translator could reach what that layout hides, should it let
// private destinations through.
func classifyIP(ip netip.Addr) addrClass {
	ip = ip.Unmap().WithZone("")
	switch {
	case ip.IsLoopback():
		return classLoopback
	case ip.IsUnspecified(), ip.Is4() && ip.As4()[0] == 0: // 0.0.0.0/8, "this network"
		return classUnsafe
	case ip.IsMulticast(), ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return classNotUnicast
	case ip.IsPrivate(), ip.IsLinkLocalUnicast():
		return classLocalNet
	case ip.Is6() && ip.As16()[0] == 0xfe && ip.As16()[1]&0xc0 == 0xc0: // fec0::/10, site-local
		return classLocalNet
	case ip.Is4() && ip.As4()[0] == 100 && ip.As4()[1]&0xc0 == 64: // 100.64.0.0/10, carrier-grade NAT
		return classLocalNet
	}
	if inside, compatible := ipv4Inside(ip); len(inside) > 0 {
		class := classPublic
		for _, v4 := range inside {
			c := classifyIP(v4)
			if c == classUnsafe { // 0.0.0.0/8 inside: no server's address, not this computer
				c = classNotUnicast
			}
			class = stricter(class, c)
		}
		if compatible && class == classPublic {
			return classNotUnicast
		}
		return class
	}
	return classPublic
}

// stricter is the class of the two that is refused more.
func stricter(a, b addrClass) addrClass {
	rank := map[addrClass]int{classPublic: 0, classLocalNet: 1, classLoopback: 2, classNotUnicast: 3, classUnsafe: 4, classUnusual: 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// ipv4Inside are the IPv4 addresses an IPv6 address ip carries by a
// transition mechanism (classifyIP); compatible: ip is an IPv4-compatible
// address (::a.b.c.d, deprecated).
func ipv4Inside(ip netip.Addr) (inside []netip.Addr, compatible bool) {
	if !ip.Is6() {
		return nil, false
	}
	b := ip.As16()
	v4 := func(i int) netip.Addr { return netip.AddrFrom4([4]byte{b[i], b[i+1], b[i+2], b[i+3]}) }
	zero := func(from, to int) bool {
		for _, x := range b[from:to] {
			if x != 0 {
				return false
			}
		}
		return true
	}
	switch {
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && zero(4, 12): // 64:ff9b::/96, NAT64
		return []netip.Addr{v4(12)}, false
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && b[4] == 0x00 && b[5] == 0x01: // 64:ff9b:1::/48, NAT64 (local use)
		return []netip.Addr{v4(12)}, false
	case zero(0, 8) && b[8] == 0xff && b[9] == 0xff && b[10] == 0 && b[11] == 0: // ::ffff:0:0/96, IPv4-translated
		return []netip.Addr{v4(12)}, false
	case zero(0, 12): // ::/96, IPv4-compatible (:: and ::1 are judged before)
		return []netip.Addr{v4(12)}, true
	case b[0] == 0x20 && b[1] == 0x02: // 2002::/16, 6to4
		return []netip.Addr{v4(2)}, false
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0 && b[3] == 0: // 2001::/32, Teredo: the server, the client (inverted)
		client := netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]})
		return []netip.Addr{v4(4), client}, false
	}
	return nil, false
}

// classifyHost tells where host is as written; final is false when its
// addresses decide (a name). A name that is not plain ASCII, or not only
// letters, digits, "-", "_" and single dots (one at the end at most), is
// classUnusual, before any case mapping (blocklist.UnusualHost); the one
// trailing dot left is the root's ("localhost." is localhost).
//
// host is a URL's Hostname(): with a port, user info or brackets (a Host,
// "127.0.0.1:80", "[::1]") it would read as a public name, which
// UnusualHost sets aside, so such a form is classUnusual (refused) rather
// than misread (SEC16 info-3; no caller passes one).
func classifyHost(host string) (class addrClass, final bool) {
	if strings.ContainsAny(host, "@[]") || strings.Contains(host, ":") && !isIPLiteral(host) {
		return classUnusual, true
	}
	if blocklist.UnusualHost(host) {
		return classUnusual, true
	}
	h, _ := db.HostKey(host) // (ASCII here: strings.ToLower's result)
	h = strings.TrimSuffix(h, ".")
	if ip, err := netip.ParseAddr(h); err == nil {
		return classifyIP(ip), true
	}
	switch {
	case h == "localhost" || strings.HasSuffix(h, ".localhost"):
		return classLoopback, true
	case endsInNumber(h):
		return classUnsafe, true
	case h == "local" || strings.HasSuffix(h, ".local"):
		return classLocalNet, true
	}
	return classPublic, false
}

// isIPLiteral: host is an IP address as Hostname() gives it (an IPv6 one
// with its zone, unbracketed).
func isIPLiteral(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// endsInNumber is the URL standard's test for a host that is an IPv4
// address: its last label is a decimal or 0x hexadecimal number. Those
// that are not plain dotted quads ("2130706433", "0x7f.1", "127.1",
// "0177.0.0.1") mean 127.0.0.1 and the like to most resolvers, aria2c's
// included.
func endsInNumber(h string) bool {
	last := h[strings.LastIndexByte(h, '.')+1:]
	if last == "" {
		return false
	}
	if x, ok := strings.CutPrefix(last, "0x"); ok {
		if x == "" {
			return true
		}
		for _, c := range x {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return false
			}
		}
		return true
	}
	for _, c := range last {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// addrPolicy is what is the same for every task.
type addrPolicy struct {
	allowLoopback bool
	// lookup resolves a name (default net.DefaultResolver; tests).
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// proxy tells the proxy a URL goes through (default
	// http.ProxyFromEnvironment; tests).
	proxy func(*url.URL) (*url.URL, error)
	// env reads an environment variable for ALL_PROXY's rule (upstream).
	// Default: os.Getenv when proxy is the environment's too (nil); a test
	// that stands in for the proxy reads no environment unless it sets env.
	env func(string) string
	// ignoreAllProxy: ALL_PROXY's rule does not apply (the in-app update,
	// updateDownloader.policy: SEC19 low-7)
	ignoreAllProxy bool
	// dial makes the guard proxy's connections (guard.go; default: a
	// net.Dialer that checks the address it connects to). Tests set it:
	// their servers listen on 127.0.0.1 whatever address the tests'
	// look-up gave a name.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

func (p *addrPolicy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if p.lookup != nil {
		return p.lookup(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// proxyFor is the proxy of a request for u (nil: none), as Go's own
// requests take it.
func (p *addrPolicy) proxyFor(u *url.URL) *url.URL {
	proxy := p.proxy
	if proxy == nil {
		proxy = func(u *url.URL) (*url.URL, error) { return http.ProxyFromEnvironment(&http.Request{URL: u}) }
	}
	pu, _ := proxy(u)
	return pu
}

func (p *addrPolicy) proxied(u *url.URL) bool {
	return p.proxyFor(u) != nil
}

// proxyUnsupportedError: the proxy set for a URL is one aria2c cannot use
// (upstream; CodeProxyUnsupported). It carries the proxy's scheme alone:
// never its host, user or password.
type proxyUnsupportedError struct {
	scheme string
}

func (e *proxyUnsupportedError) Error() string {
	return "refused: the proxy set for this address is a " + e.scheme + ":// one, which aria2c cannot use"
}

// safeScheme is a proxy URL's scheme for a message: lower case, at most 16
// bytes (url.Parse leaves it letters, digits, "+", "-" and "." only).
func safeScheme(scheme string) string {
	scheme = strings.ToLower(scheme)
	if len(scheme) > 16 {
		scheme = scheme[:16]
	}
	return scheme
}

// proxyFailure is the task failure of a proxy aria2c cannot use (nil when
// err is not one): downloads.proxy_unsupported {scheme}.
func proxyFailure(err error) *failure {
	var pe *proxyUnsupportedError
	if !errors.As(err, &pe) {
		return nil
	}
	return &failure{code: CodeProxyUnsupported, params: map[string]any{"scheme": pe.scheme},
		detail: "the proxy set for this address is a " + pe.scheme + ":// one, which aria2c cannot use; nothing was handed to aria2c"}
}

// upstream is the user's proxy that aria2c's request for u goes through
// (nil = none, a direct connection), and so the one answer to "does
// aria2c's request for u go through a proxy" (SEC11 low-3): Go's proxy for
// u (proxyFor: HTTP_PROXY, HTTPS_PROXY and NO_PROXY as net/http reads
// them), so the proxy variables aria2c would read itself (lower case only:
// they differ on macOS) and its no_proxy decide nothing. aria2c itself
// only ever talks to the guard proxy (guard.go), which goes on through
// this one.
//
// Only an http:// proxy is used. For any other the answer is a
// proxyUnsupportedError and the task fails before anything is handed to
// aria2c (SEC16 low-1 and low-2; user 2026-11-11): aria2c has no SOCKS
// (it connected directly, past the proxy the user set, from the user's
// own address while Go's requests for the same recording went through the
// proxy), and to an https:// proxy it spoke plain HTTP, sending the
// proxy's password in the clear. The guard speaks plain HTTP and CONNECT
// to the proxy and refuses the others the same way. A proxy URL without a
// host is no proxy (a direct download, checked as one). Without a proxy
// for u, a SOCKS or https:// ALL_PROXY is refused the same way
// (allProxyUnsupported).
//
// The URL returned may carry the proxy's user and password: the guard
// alone uses it; it never reaches aria2c, a log or an error.
func (p *addrPolicy) upstream(u *url.URL) (*url.URL, error) {
	pu := p.proxyFor(u)
	if pu == nil || pu.Hostname() == "" {
		if scheme := p.allProxyUnsupported(u); scheme != "" {
			return nil, &proxyUnsupportedError{scheme: scheme}
		}
		return nil, nil
	}
	if scheme := strings.ToLower(pu.Scheme); scheme != "http" {
		return nil, &proxyUnsupportedError{scheme: safeScheme(scheme)}
	}
	port := pu.Port()
	if port == "" {
		port = "80" // net/http's default
	}
	return &url.URL{Scheme: "http", User: pu.User, Host: net.JoinHostPort(pu.Hostname(), port)}, nil
}

// allProxyUnsupported is the scheme of ALL_PROXY (or all_proxy) when it
// would be the proxy of u for a program that reads it and is one that
// LectureFlow refuses (SOCKS, https://), "" otherwise. Neither net/http
// nor the guard reads ALL_PROXY, so a download with it alone went
// directly, past the proxy the user set, without a word (SEC19 info-8);
// the same rule as for HTTP_PROXY / HTTPS_PROXY now refuses it
// (downloads.proxy_unsupported). It applies when u's own variable
// (HTTP_PROXY for http, HTTPS_PROXY for https, either case) is not set;
// set, that one decides, as for net/http. ALL_PROXY of another kind
// (http://, no scheme) is still not read: a direct download, judged as
// one. A host NO_PROXY (or no_proxy) excludes, and this computer, are not
// refused: they go directly for a program that reads ALL_PROXY too
// (noProxyFor; SEC19 low-6). The in-app update does not apply the rule
// (ignoreAllProxy: its other way, net/http, reads no ALL_PROXY either).
func (p *addrPolicy) allProxyUnsupported(u *url.URL) string {
	getenv := p.getenv()
	if getenv == nil || p.ignoreAllProxy {
		return ""
	}
	envAny := func(names ...string) string {
		for _, name := range names {
			if v := getenv(name); v != "" {
				return v
			}
		}
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		if envAny("HTTP_PROXY", "http_proxy") != "" {
			return ""
		}
	case "https":
		if envAny("HTTPS_PROXY", "https_proxy") != "" {
			return ""
		}
	default:
		return ""
	}
	if noProxyFor(envAny("NO_PROXY", "no_proxy"), u) {
		return "" // (the user said this host goes directly; SEC19 low-6)
	}
	scheme, _, ok := strings.Cut(strings.TrimSpace(envAny("ALL_PROXY", "all_proxy")), "://")
	if !ok {
		return ""
	}
	scheme = strings.ToLower(scheme)
	switch {
	case scheme == "https":
		return scheme
	case strings.HasPrefix(scheme, "socks"):
		if strings.Trim(scheme, "abcdefghijklmnopqrstuvwxyz0123456789+-.") != "" {
			return "socks" // (not a scheme url.Parse would take: not quoted)
		}
		return safeScheme(scheme)
	}
	return ""
}

// noProxyFor: a request for u goes to no proxy by noProxy (NO_PROXY's
// value) or by being for this computer. These are net/http's rules
// (golang.org/x/net/http/httpproxy, config.init and useProxy, which
// ProxyFromEnvironment applies to HTTP_PROXY and HTTPS_PROXY), written
// out again for ALL_PROXY: net/http offers no way to ask them for another
// variable, and its copy of the package cannot be imported.
//
//   - "localhost" and a loopback address: never proxied;
//   - each comma-separated entry, trimmed and lower-cased: "*" matches
//     everything; a CIDR ("10.0.0.0/8") an address in it; an IP, with a
//     port or not ("10.1.2.3", "[::1]:80"), that address (and port); else
//     a name, with a port or not: "example.com" matches it and its
//     subdomains, ".example.com" and "*.example.com" the subdomains alone;
//     names never match an address.
//
// The port is u's, or its scheme's (80, 443). (net/http maps a name by
// IDNA first; the names LectureFlow fetches are ASCII.)
//
// Neither the NO_PROXY entries nor u's host are converted to punycode
// here, which differs from net/http in two cases: a Unicode entry
// ("例え.jp") does not match the punycode host ("xn--r8jz45g.jp"), and a
// punycode entry does not match a Unicode host. Both err on the strict
// side: a request net/http would send directly is refused, never the
// other way round (and a non-ASCII host is refused as unusual anyway).
func noProxyFor(noProxy string, u *url.URL) bool {
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
	}
	if host == "localhost" {
		return true
	}
	var ip net.IP
	if a, err := netip.ParseAddr(host); err == nil {
		ip = net.IP(a.AsSlice())
		if ip.IsLoopback() {
			return true
		}
	}
	host = strings.ToLower(strings.TrimSpace(host))
	for _, p := range strings.Split(noProxy, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == "*" {
			return true
		}
		if _, cidr, err := net.ParseCIDR(p); err == nil {
			if ip != nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		phost, pport, err := net.SplitHostPort(p)
		if err == nil {
			if phost == "" {
				continue
			}
			if phost[0] == '[' && phost[len(phost)-1] == ']' {
				phost = phost[1 : len(phost)-1]
			}
		} else {
			phost = p
		}
		if pip := net.ParseIP(phost); pip != nil {
			if ip != nil && pip.Equal(ip) && (pport == "" || pport == port) {
				return true
			}
			continue
		}
		if phost == "" {
			continue
		}
		if strings.HasPrefix(phost, "*.") {
			phost = phost[1:]
		}
		matchHost := false
		if phost[0] != '.' {
			matchHost = true
			phost = "." + phost
		}
		if ip == nil && (strings.HasSuffix(host, phost) || matchHost && host == phost[1:]) && (pport == "" || pport == port) {
			return true
		}
	}
	return false
}

// getenv is how ALL_PROXY's rule reads the environment: env, else
// os.Getenv when proxy is the environment's too; nil (nothing read) for a
// test that stands in for the proxy alone.
func (p *addrPolicy) getenv() func(string) string {
	switch {
	case p.env != nil:
		return p.env
	case p.proxy != nil:
		return nil
	}
	return os.Getenv
}

// viaUpstream: aria2c's request for u goes on through a proxy of the
// user's (upstream), which resolves the name: the address check of a URL
// before it is handed to aria2c (resolver.checkHost) and the guard proxy's
// own look-up (guard.go) are skipped exactly then. (A proxy aria2c cannot
// use has refused the URL before either.)
func (p *addrPolicy) viaUpstream(u *url.URL) bool {
	pu, _ := p.upstream(u)
	return pu != nil
}

// forTask is the policy of a task whose recording page is on pageHost (""
// = unknown: the local network stays out of reach).
func (p *addrPolicy) forTask(pageHost string) *taskAddr {
	h, _ := db.HostKey(pageHost) // (no Unicode mapping: an unusual page host is never on the local network)
	return &taskAddr{p: p, pageHost: strings.TrimSuffix(h, ".")}
}

// taskAddr is the policy of one task: its page decides whether the local
// network may be reached.
type taskAddr struct {
	p        *addrPolicy
	pageHost string

	once     sync.Once
	pageNear bool // the page is on the local network
}

// localAddrError: host is, or resolves to, an address the task must not
// reach; loopback when it is this computer (CodeLoopbackAddress), else the
// local network from a public page (CodeLocalAddress).
type localAddrError struct {
	host     string
	loopback bool
}

func (e *localAddrError) Error() string {
	if e.loopback {
		return "refused: " + e.host + " is this computer"
	}
	return "refused: " + e.host + " is on the local network and the recording's page is not"
}

// unusualHostError: a URL's host name has characters that cannot be
// judged as written (blocklist.UnusualHost), refused for the do-not-handle
// list and the local-address rules alike (CodeUnusualHost).
type unusualHostError struct {
	host string
}

func (e *unusualHostError) Error() string {
	return "refused: the host name " + safeHost(e.host) + " has unusual characters"
}

// notUnicastError: host is, or resolves to, an address of no single
// server (classNotUnicast): refused as CodeUnusualHost.
type notUnicastError struct {
	host string
}

func (e *notUnicastError) Error() string {
	return "refused: " + safeHost(e.host) + " is no single server's address (multicast, broadcast or the like)"
}

// userInfoError: a URL with user info before its host ("user@",
// "user:pass@", a bare "@"): aria2c would send it as Basic authentication
// to the server, and the guard proxy would pass that on unchanged (SEC11
// info-6, SEC16 info-9). The extension never sends one (WHATWG URLs from
// the page); a hand-made playlist or a redirect may. Refused as
// CodeUnusualHost {host}; the user info itself is in no text.
type userInfoError struct {
	host string
}

func (e *userInfoError) Error() string {
	return "refused: the address of " + safeHost(e.host) + " carries a user name or password"
}

// otherSchemeError: aria2c asked the guard proxy for an address that is
// neither http nor https (guard.go: a redirect to ftp://, which aria2c
// follows). LectureFlow fetches http and https only; the task fails with
// CodeUnusualHost, whose host is the scheme and the host ("an address
// LectureFlow cannot handle safely").
type otherSchemeError struct {
	scheme, host string
}

func (e *otherSchemeError) Error() string {
	return "refused: " + safeHost(e.scheme+"://"+e.host) + " is neither an http nor an https address"
}

// safeHost is host for a message: printable ASCII as is, every other byte
// (and "%") percent-encoded, at most 256 bytes; no Unicode, bidi or
// invisible character reaches a text the user reads.
func safeHost(host string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(host) && b.Len() < 256; i++ {
		c := host[i]
		if c < 0x21 || c >= 0x7f || c == '%' {
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// pageOnLocalNet: the page's host is on the local network, by address or
// name, or every address it resolves to is (looked up once per task).
func (t *taskAddr) pageOnLocalNet(ctx context.Context) bool {
	t.once.Do(func() {
		if t.pageHost == "" {
			return
		}
		class, final := classifyHost(t.pageHost)
		if final {
			t.pageNear = class == classLocalNet
			return
		}
		ips, err := t.p.resolve(ctx, t.pageHost)
		if err != nil || len(ips) == 0 {
			return
		}
		for _, ip := range ips {
			if classifyIP(ip) != classLocalNet {
				return
			}
		}
		t.pageNear = true
	})
	return t.pageNear
}

// allow judges host (the URL's host) found to be of class.
func (t *taskAddr) allow(ctx context.Context, host string, class addrClass) error {
	switch class {
	case classLoopback:
		if t.p.allowLoopback {
			return nil
		}
		return &localAddrError{host: host, loopback: true}
	case classUnsafe: // 0.0.0.0/8, :: and numbers resolvers read as an address: this computer
		return &localAddrError{host: host, loopback: true}
	case classNotUnicast:
		return &notUnicastError{host: host}
	case classUnusual:
		return &unusualHostError{host: host}
	case classLocalNet:
		if t.pageOnLocalNet(ctx) {
			return nil
		}
		return &localAddrError{host: host}
	}
	return nil
}

// blockedSiteError: a URL of the task is on a site LectureFlow does not
// handle (internal/blocklist), wherever it came from (a playlist's entry,
// an MPD, a redirect).
type blockedSiteError struct {
	host, category string
}

func (e *blockedSiteError) Error() string {
	return "refused: " + safeHost(e.host) + " is a site LectureFlow does not handle (" + e.category + ")"
}

// checkURL judges raw's host as written and, with resolve and no proxy
// for raw, every address it resolves to (a lookup error is left to the
// request itself). A URL of a site on the do-not-handle list is refused
// first, whatever its address; so is a host with unusual characters
// (blocklist.UnusualHost: a playlist's entry, a redirect's Location). A
// URL with user info is refused after the host's rules, before any
// look-up (userInfoError).
func (t *taskAddr) checkURL(ctx context.Context, raw string, resolve bool) error {
	return t.checkURLWith(ctx, raw, resolve, nil, t.p.proxied)
}

// checkURLWith is checkURL whose look-up of a name's addresses is addrs
// (nil: checkAddrs) and whose proxy is told by proxied (a proxied request
// is not looked up: the proxy resolves its name; for aria2c, ariaProxy).
// The list, the host as written and the proxy are judged for raw itself,
// every time: they depend on its path, scheme and port; only the addresses
// are the host's alone (resolver.checkHost keeps them per host, SEC10
// info-3).
func (t *taskAddr) checkURLWith(ctx context.Context, raw string, resolve bool, addrs func(host string) error, proxied func(*url.URL) bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		// Allowed here only because every caller has already refused such a
		// URL: enqueue / refresh by validHTTPURL, playlist and manifest URLs
		// by resolve(), which requires a host (fuzz decision 4). A new caller
		// must check the URL itself first.
		return nil
	}
	switch c := blocklist.URLCategory(raw); c {
	case "":
	case blocklist.CategoryUnusualHost:
		return &unusualHostError{host: u.Hostname()}
	default:
		return &blockedSiteError{host: u.Hostname(), category: c}
	}
	host := u.Hostname()
	class, final := classifyHost(host)
	if final {
		if err := t.allow(ctx, host, class); err != nil {
			return err
		}
	}
	// user info: never handed over (aria2c sends it as Basic
	// authentication), whatever the host; the rules above name the more
	// telling reason first
	if u.User != nil {
		return &userInfoError{host: host}
	}
	if final || !resolve || proxied(u) {
		return nil // (a proxy resolves it)
	}
	if addrs != nil {
		return addrs(host)
	}
	return t.checkAddrs(ctx, host)
}

// checkAddrs judges every address the name host resolves to (a lookup
// error is left to the request itself).
func (t *taskAddr) checkAddrs(ctx context.Context, host string) error {
	return t.checkAddrsWith(ctx, host, false)
}

// checkAddrsWith is checkAddrs; with failClosed a name that cannot be
// looked up (an error, or no address) is refused as a network failure
// (downloads.network: retryable, the name may resolve later) instead of
// being left to the request. That is right where the request resolves
// the name with the same resolver as this look-up: aria2c on the system's
// resolver (engine.systemResolver; SEC16 R2). Otherwise a name Go's
// look-up failed on (an answer withheld once, by an attacker's DNS with
// TTL 0) went to aria2c unchecked, to be answered there with any address.
func (t *taskAddr) checkAddrsWith(ctx context.Context, host string, failClosed bool) error {
	ips, err := t.p.resolve(ctx, host)
	if err != nil || len(ips) == 0 {
		if !failClosed || ctx.Err() != nil {
			return nil
		}
		return &failure{code: CodeNetwork, detail: "could not look up the address of " + safeHost(host) + "; not handed to aria2c"}
	}
	for _, ip := range ips {
		if err := t.allow(ctx, host, classifyIP(ip)); err != nil {
			return err
		}
	}
	return nil
}

// checkDialed is the dialer's check of the address it connects to for the
// URL host name.
func (t *taskAddr) checkDialed(ctx context.Context, name, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return &localAddrError{host: name, loopback: true}
	}
	return t.allow(ctx, name, classifyIP(ip))
}

// transport is the HTTP transport of Go's own requests for the task: the
// default one whose connections pass checkDialed, but for those to the
// user's proxy (HTTP_PROXY and the like: a school's proxy may well have a
// private address).
func (t *taskAddr) transport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	var proxies sync.Map // "host:port" of the proxies the transport was given
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		var u *url.URL
		var err error
		if t.p.proxy != nil {
			u, err = t.p.proxy(req.URL)
		} else {
			u, err = http.ProxyFromEnvironment(req)
		}
		if u != nil {
			proxies.Store(proxyAddr(u), true)
		}
		return u, err
	}
	direct := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, ok := proxies.Load(proxyKey(addr)); ok {
			return direct.DialContext(ctx, network, addr)
		}
		name, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error { return t.checkDialed(ctx, name, address) }}
		return guarded.DialContext(ctx, network, addr)
	}
	return tr
}

// proxyAddr is the key of the address net/http dials for a proxy URL
// (proxyKey's form; net/http keeps an ASCII host's case, SEC10 low-1).
func proxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[u.Scheme]
	}
	host, _ := db.HostKey(u.Hostname()) // (sameHost's rule: never another host's address)
	return net.JoinHostPort(host, port)
}

// proxyKey is the "host:port" net/http dials, compared as proxyAddr
// stores it: the host by db.HostKey (a non-ASCII proxy host, which
// net/http dials in its IDNA form, never matches: its connections are
// checked like any other).
func proxyKey(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	key, _ := db.HostKey(host)
	return net.JoinHostPort(key, port)
}

// localFailure is the task failure of a refused address (nil when err is
// not one): the host of the URL being requested when the error tells it,
// else the host refused. A site on the do-not-handle list is
// downloads.site_blocked {category, host}; a host name with unusual
// characters downloads.unusual_host {host}; the local and loopback codes
// {host}: every host in safeHost's form (a listed site written with
// full-width letters or a bidi control still matches the list: SEC11
// low-2).
func localFailure(err error) *failure {
	var be *blockedSiteError
	if errors.As(err, &be) {
		host := safeHost(be.host)
		return &failure{code: CodeSiteBlocked, params: map[string]any{"category": be.category, "host": host},
			detail: "refused a site LectureFlow does not handle: " + host}
	}
	var he *unusualHostError
	if errors.As(err, &he) {
		host := safeHost(he.host)
		return &failure{code: CodeUnusualHost, params: map[string]any{"host": host},
			detail: "refused a host name with unusual characters: " + host}
	}
	var ne *notUnicastError
	if errors.As(err, &ne) {
		host := safeHost(ne.host)
		return &failure{code: CodeUnusualHost, params: map[string]any{"host": host},
			detail: "refused an address of no single server (multicast, broadcast or the like): " + host}
	}
	var ie *userInfoError
	if errors.As(err, &ie) {
		host := safeHost(ie.host)
		return &failure{code: CodeUnusualHost, params: map[string]any{"host": host},
			detail: "refused an address with a user name or password before its host: " + host}
	}
	var se *otherSchemeError
	if errors.As(err, &se) {
		host := safeHost(se.scheme + "://" + se.host)
		return &failure{code: CodeUnusualHost, params: map[string]any{"host": host},
			detail: "refused an address that is neither http nor https (a redirect led there): " + host}
	}
	var le *localAddrError
	if !errors.As(err, &le) {
		return nil
	}
	host := le.host
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
	}
	host = safeHost(host) // (printable ASCII here already: bounded, and alike for the four codes; SEC16 info-7)
	if le.loopback {
		return &failure{code: CodeLoopbackAddress, params: map[string]any{"host": host},
			detail: "refused an address of this computer: " + host}
	}
	return &failure{code: CodeLocalAddress, params: map[string]any{"host": host},
		detail: "refused an address of the local network for a recording whose page is not on it: " + host}
}

// checkTrackHosts refuses track URLs whose host as written is this
// computer, and those with user info (the enqueue and refresh check; the
// local network is judged when the task runs, with the page's addresses).
func (p *addrPolicy) checkTrackHosts(tracks []liveTrack) error {
	t := p.forTask("")
	for _, tr := range tracks {
		u, err := url.Parse(tr.url)
		if err != nil {
			continue
		}
		class, final := classifyHost(u.Hostname())
		if final && class == classUnusual {
			return unusualRequestError("tracks.url")
		}
		if final && class == classNotUnicast {
			return unusualRequestError("tracks.url")
		}
		if final && (class == classLoopback || class == classUnsafe) {
			if t.allow(context.Background(), u.Hostname(), class) != nil {
				return localRequestError()
			}
		}
		if u.User != nil { // (userInfoError: also checked when the task runs)
			return unusualRequestError("tracks.url")
		}
	}
	return nil
}

// taskNet is the network side of a task run: its address policy and the
// HTTP client of Go's own requests, whose connections are the task's
// alone (so a connection to the local network one task may make is never
// lent to another).
type taskNet struct {
	addr *taskAddr
	hc   *http.Client
	tr   *http.Transport // nil when the client came from Options.HTTPClient
}

// newTaskNet is the network side of a run whose page is on pageHost.
func (q *Queue) newTaskNet(pageHost string) *taskNet {
	n := &taskNet{addr: q.local.forTask(pageHost)}
	if q.o.HTTPClient != nil {
		n.hc = q.o.HTTPClient
		return n
	}
	n.tr = n.addr.transport()
	n.hc = &http.Client{Timeout: q.hc.Timeout, Transport: n.tr}
	return n
}

func (n *taskNet) close() {
	if n != nil && n.tr != nil {
		n.tr.CloseIdleConnections()
	}
}

// netOf is s's network side (a snapshot made outside a run: an unknown
// page).
func (q *Queue) netOf(s runSnapshot) *taskNet {
	if s.net != nil {
		return s.net
	}
	return q.newTaskNet("")
}

// pageHostOf is the host of the recording's page: from the referer, else
// the stored page_host.
func pageHostOf(referer, stored string) string {
	if u, err := url.Parse(referer); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if h, _, err := net.SplitHostPort(stored); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(stored, "[]")
}
