// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The redirect policy of every HTTP client of this package (v3.2.x r2 N20,
// SEC2 L1, SEC14 N20-L1/L2). net/http keeps custom headers such as
// X-Colab-Runtime-Proxy-Token on every redirect, puts the previous hop's
// whole URL (with the runtime proxy token in its query) into Referer and
// resends a request body on 307 / 308, so by default a runtime proxy that
// redirected elsewhere would hand that host the proxy token and the
// lecture's audio. The policy:
//   - Referer is removed on every hop (none of these APIs uses it);
//   - a redirect within the origin of the first request (scheme, host, port;
//     http → https on the same host name included) is followed as before;
//   - https → http is never followed;
//   - a redirect that changed the method (301 / 302 / 303 turn PUT, DELETE
//     and POST into GET) is never followed, not even on the same host;
//   - a runtime request (runtime.go: the Jupyter server behind the proxy URL
//     the tunnel returned; marked by runtimeRequestContext) never follows a
//     redirect to another host;
//   - another request follows a redirect to another host only when the
//     first request was GET / HEAD (a 301 / 302 / 303 turns PUT or DELETE
//     into GET; a sign-in page's 200 must not count as an upload) and only to
//     https on the default port of one of redirectAllowedHosts, and then
//     without Authorization, X-Colab-Runtime-Proxy-Token and Cookie;
//   - anything else, and more than maxRedirects redirects, is not followed:
//     the request fails with *redirectRefused, which names the target host
//     (never its path or query, which may carry the runtime proxy token).
//
// The WebSocket handshake (ws.go) follows no redirect at all.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// maxRedirects: redirects followed for one request (net/http's default).
const maxRedirects = 10

// redirectAllowedHosts are the exact Google host names a redirect to another
// host may go to: the hosts the tunnel and the credential calls use
// (DefaultColabURL; DefaultTokenURL / DefaultRevokeURL on
// oauth2.googleapis.com) and Google's sign-in host (login.go), where an
// expired session may plausibly be sent. Exact names, not suffixes: the
// subdomains of googleapis.com include storage buckets anyone can create
// (SEC14 N20-L1). Compared ASCII case-insensitively; IP literals, a trailing
// dot and non-ASCII names never match.
var redirectAllowedHosts = []string{
	"colab.research.google.com",
	"accounts.google.com",
	"oauth2.googleapis.com",
}

// redirectStripHeaders are removed from a request redirected to another
// host (net/http itself drops Authorization only outside the original
// domain and its subdomains, and never drops custom headers).
var redirectStripHeaders = []string{"Authorization", "X-Colab-Runtime-Proxy-Token", "Cookie"}

// redirectRefused is the error of a redirect the policy does not follow.
type redirectRefused struct {
	host   string // the target's host[:port] (sanitised), never the path or query
	reason string
}

func (e *redirectRefused) Error() string {
	return fmt.Sprintf("redirect to %s not followed (%s)", e.host, e.reason)
}

type runtimeRequestKey struct{}

// runtimeRequestContext marks ctx's requests as runtime requests: they
// follow no redirect to another host (a redirected request keeps the first
// request's context).
func runtimeRequestContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, runtimeRequestKey{}, true)
}

func isRuntimeRequest(req *http.Request) bool {
	v, _ := req.Context().Value(runtimeRequestKey{}).(bool)
	return v
}

// withRedirectPolicy returns hc with the package's redirect policy: a copy
// when hc has no CheckRedirect of its own (the caller's client is left as
// it is), hc itself otherwise (a caller that set a policy keeps it).
func withRedirectPolicy(hc *http.Client, log *slog.Logger) *http.Client {
	if hc == nil {
		hc = &http.Client{}
	} else if hc.CheckRedirect != nil {
		return hc
	} else {
		cp := *hc
		hc = &cp
	}
	hc.CheckRedirect = redirectPolicy(log)
	return hc
}

// redirectPolicy is the CheckRedirect of withRedirectPolicy; log (may be
// nil) gets the target host of a redirect to another host, followed or not.
func redirectPolicy(log *slog.Logger) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		err := checkColabRedirect(req, via)
		if log != nil {
			var rr *redirectRefused
			switch {
			case errors.As(err, &rr):
				log.Warn("colab: redirect not followed", "host", rr.host, "reason", rr.reason)
			case err == nil && !sameOrigin(via[0].URL, req.URL):
				log.Info("colab: redirect to another host followed; Authorization, the proxy token, Cookie and Referer removed",
					"host", safeHost(req.URL))
			}
		}
		return err
	}
}

// checkColabRedirect decides one redirect; req is the next request (net/http
// has already copied the first request's headers into it and set Referer to
// the previous URL), via the requests so far (via[0] the first).
func checkColabRedirect(req *http.Request, via []*http.Request) error {
	req.Header.Del("Referer")
	host := safeHost(req.URL)
	if len(via) >= maxRedirects {
		return &redirectRefused{host: host, reason: "too many redirects"}
	}
	prev := via[len(via)-1].URL
	if strings.EqualFold(prev.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return &redirectRefused{host: host, reason: "https to http"}
	}
	// 301 / 302 / 303 turn PUT, DELETE and POST into GET (without the body):
	// followed, a 200 would count as an upload, a deletion or a token
	// refresh that never happened (SEC14 N20 residual). 307 / 308 keep the
	// method and are judged below.
	if req.Method != via[0].Method {
		return &redirectRefused{host: host, reason: "the request was redirected and turned into " + req.Method}
	}
	// Against the first request: net/http copies its headers into every
	// later hop, so a hop back from an allowed host is judged like the first.
	if sameOrigin(via[0].URL, req.URL) {
		return nil
	}
	if isRuntimeRequest(via[0]) {
		return &redirectRefused{host: host, reason: "runtime request to another host"}
	}
	if !strings.EqualFold(req.URL.Scheme, "https") || !allowedRedirectHost(req.URL) {
		return &redirectRefused{host: host, reason: "another host"}
	}
	// The first request's method: 301 / 302 / 303 have already turned a PUT
	// or DELETE into GET.
	if m := via[0].Method; m != "" && m != http.MethodGet && m != http.MethodHead {
		return &redirectRefused{host: host, reason: m + " to another host"}
	}
	for _, h := range redirectStripHeaders {
		req.Header.Del(h)
	}
	return nil
}

// allowedRedirectHost: u is https on the default port and its host is one
// of redirectAllowedHosts.
func allowedRedirectHost(u *url.URL) bool {
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	h := u.Hostname()
	if h == "" || strings.HasSuffix(h, ".") || net.ParseIP(h) != nil || !isASCIIHost(h) {
		return false
	}
	h = asciiLowerHost(h)
	for _, a := range redirectAllowedHosts {
		if h == a {
			return true
		}
	}
	return false
}

// sameOrigin: next is on prev's origin, or an upgrade from http to https on
// the same host name. Host names compare ASCII case-insensitively; a name
// with a byte outside ASCII equals only the same bytes (SEC10).
func sameOrigin(prev, next *url.URL) bool {
	if hostKey(prev.Hostname()) != hostKey(next.Hostname()) {
		return false
	}
	ps, ns := strings.ToLower(prev.Scheme), strings.ToLower(next.Scheme)
	switch {
	case ps == ns:
		return portOf(prev) == portOf(next)
	case ps == "http" && ns == "https":
		return true
	}
	return false
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func hostKey(h string) string {
	if !isASCIIHost(h) {
		return h
	}
	return asciiLowerHost(h)
}

func isASCIIHost(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func asciiLowerHost(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// safeHost is u's host[:port] for a message: letters, digits and . - : [ ]
// only, at most 255 bytes; anything else is "?".
func safeHost(u *url.URL) string {
	h := u.Host
	if h == "" || len(h) > 255 {
		return "?"
	}
	for _, r := range h {
		if !(r == '.' || r == '-' || r == ':' || r == '[' || r == ']' || r == '_' ||
			r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return "?"
		}
	}
	return h
}
