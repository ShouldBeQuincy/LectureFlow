// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
)

// Cookies (SPEC-ext 凭据处理): the extension sends the cookies of the track
// URLs' domains; each request carries only those that a browser would send
// to that URL (domain, path, secure, not expired). They exist only in the
// task's memory.

// validCookie rejects what could not come from a browser and could break
// out of the Cookie header (control characters, separators in the name).
func validCookie(c bridge.Cookie) bool {
	if c.Name == "" || strings.ContainsAny(c.Name, "=; \t") || hasCTL(c.Name) || hasCTL(c.Value) ||
		strings.ContainsRune(c.Value, ';') || hasCTL(c.Domain) || hasCTL(c.Path) {
		return false
	}
	return true
}

func hasCTL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func cookieExpired(c bridge.Cookie, now time.Time) bool {
	return c.Expires > 0 && c.Expires <= now.Unix()
}

// allExpired reports whether cookies were sent and every one has expired
// (session cookies never do).
func allExpired(cs []bridge.Cookie, now time.Time) bool {
	if len(cs) == 0 {
		return false
	}
	for _, c := range cs {
		if !cookieExpired(c, now) {
			return false
		}
	}
	return true
}

// Host names are compared by db.HostKey, the rule of the origins API keys
// are bound to (SEC8 low-1, SEC9 info-4): ASCII letters lower-cased only. A
// host or cookie domain with a byte outside ASCII (written as is or
// percent-encoded) equals only the same bytes: Unicode lower-casing made
// "cdn.v\u0130deo.example" (U+0130) a subdomain of "video.example", while
// net/http dials its IDNA form, another (punycode) domain. A URL written in
// punycode ("xn--...") is ASCII and matches as before.

// sameHost reports whether two hosts as written are one host (db.HostKey).
func sameHost(a, b string) bool {
	ka, _ := db.HostKey(a)
	kb, _ := db.HostKey(b)
	return ka == kb
}

// domainMatch is RFC 6265 5.1.3: a host-only cookie goes to its host
// only, a domain cookie also to its subdomains; an IP address is never a
// "subdomain" (a cookie of "0.0.1" must not go to 127.0.0.1; fuzz), nor is
// a host or domain with a byte outside ASCII (only the same bytes match).
func domainMatch(host string, c bridge.Cookie) bool {
	d, dASCII := db.HostKey(strings.TrimPrefix(c.Domain, "."))
	host, hASCII := db.HostKey(host)
	if d == "" {
		return false
	}
	if c.HostOnly || !dASCII || !hASCII || net.ParseIP(host) != nil {
		return host == d
	}
	return host == d || strings.HasSuffix(host, "."+d)
}

func pathMatch(reqPath, cookiePath string) bool {
	if cookiePath == "" {
		cookiePath = "/"
	}
	if reqPath == "" {
		reqPath = "/"
	}
	if reqPath == cookiePath {
		return true
	}
	if strings.HasPrefix(reqPath, cookiePath) {
		return strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/'
	}
	return false
}

// cookieHeader is the Cookie header value for rawURL ("" = none).
func cookieHeader(rawURL string, cs []bridge.Cookie, now time.Time) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	var pick []bridge.Cookie
	for _, c := range cs {
		if cookieExpired(c, now) || !domainMatch(u.Hostname(), c) || !pathMatch(u.EscapedPath(), c.Path) {
			continue
		}
		if c.Secure && u.Scheme != "https" {
			continue
		}
		pick = append(pick, c)
	}
	// RFC 6265 5.4: longer paths first (stable otherwise)
	sort.SliceStable(pick, func(i, j int) bool { return len(pick[i].Path) > len(pick[j].Path) })
	parts := make([]string, len(pick))
	for i, c := range pick {
		parts[i] = c.Name + "=" + c.Value
	}
	return strings.Join(parts, "; ")
}

// requestHeaders are the extra headers of a request for rawURL: the
// matching cookies and the page as Referer, as much of it as rawURL may
// see (refererFor). Never logged.
//
// Only Cookie and Referer. A request handed to aria2c must not carry an
// Authorization header: the guard proxy takes one for the Basic
// authentication aria2c makes of a URL's user info and refuses the
// request (guard.go forward: downloads.unusual_host). A platform that
// needs Bearer authentication, or a mirror with credentials, needs that
// rule of the guard changed first.
func requestHeaders(rawURL string, cs []bridge.Cookie, referer string, now time.Time) []string {
	var h []string
	if v := cookieHeader(rawURL, cs, now); v != "" {
		h = append(h, "Cookie: "+v)
	}
	if r := refererFor(referer, rawURL); r != "" {
		h = append(h, "Referer: "+r)
	}
	return h
}

// refererFor is the Referer of a request for target from the page (the
// browser's default policy, strict-origin-when-cross-origin; security
// review L2): the page's URL for its own origin, the page's origin alone
// ("https://lms.example.edu/") for another one, nothing from an https page
// to a URL that is not potentially trustworthy (plain http but to a
// loopback host, as the Secure Contexts standard has it). Never the page's
// fragment or user info. Each redirect hop is judged by its own URL
// (client, resolver).
func refererFor(page, target string) string {
	if page == "" {
		return ""
	}
	p, err := url.Parse(page)
	if err != nil || p.Host == "" {
		return ""
	}
	t, err := url.Parse(target)
	if err != nil {
		return ""
	}
	if strings.EqualFold(p.Scheme, "https") && !trustworthy(t) {
		return ""
	}
	if sameOrigin(p, t) {
		return cleanReferer(page)
	}
	scheme, host := strings.ToLower(p.Scheme), p.Hostname()
	host, _ = db.HostKey(host)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := p.Port(); port != "" && port != effectivePort(&url.URL{Scheme: scheme}) {
		host += ":" + port
	}
	return scheme + "://" + host + "/"
}

// trustworthy is the Secure Contexts standard's "potentially trustworthy
// URL" for http(s): https, or a loopback host (127.0.0.0/8, ::1, localhost
// and *.localhost).
func trustworthy(u *url.URL) bool {
	if strings.EqualFold(u.Scheme, "https") {
		return true
	}
	h, _ := db.HostKey(u.Hostname())
	h = strings.TrimSuffix(h, ".")
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.Unmap().IsLoopback()
	}
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// cleanReferer is the page URL as a Referer: without fragment and user
// info.
func cleanReferer(page string) string {
	p, err := url.Parse(page)
	if err != nil {
		return ""
	}
	p.User, p.Fragment, p.RawFragment = nil, "", ""
	return p.String()
}

// sameOrigin compares scheme, host (sameHost) and port (the scheme's
// default port when none is written).
func sameOrigin(a, b *url.URL) bool {
	if !strings.EqualFold(a.Scheme, b.Scheme) || !sameHost(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// validHTTPURL accepts absolute http(s) URLs without control characters.
func validHTTPURL(raw string) bool {
	if raw == "" || hasCTL(raw) || strings.ContainsAny(raw, " ") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}
