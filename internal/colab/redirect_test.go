// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The redirect policy (redirect.go, v3.2.x r2 N20). Every server listens on
// the loopback; host names are mapped onto the servers by the test
// transport's dialer, so "oauth2.googleapis.com" and "evil.example" are two
// local servers and no request leaves the machine.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// sink records what reached a redirect target.
type sink struct {
	mu   sync.Mutex
	hits []sinkHit
}

type sinkHit struct {
	method, host string
	header       http.Header
	body         int
}

func (s *sink) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.hits = append(s.hits, sinkHit{method: r.Method, host: r.Host, header: r.Header.Clone(), body: len(b)})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `[]`)
}

func (s *sink) all() []sinkHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkHit(nil), s.hits...)
}

// redirectRig: origin answers every request with a redirect to the URL
// next returns (with the status st); everything else lands on target.
type redirectRig struct {
	origin, target         *httptest.Server
	plainOrigin, plainSink *httptest.Server
	sink, plain            *sink
	originHits             []sinkHit
	mu                     sync.Mutex
	status                 int
	location               string
}

func newRedirectRig(t *testing.T) *redirectRig {
	t.Helper()
	g := &redirectRig{sink: &sink{}, plain: &sink{}, status: http.StatusTemporaryRedirect}
	redir := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.originHits = append(g.originHits, sinkHit{method: r.Method, host: r.Host, header: r.Header.Clone(), body: len(b)})
		loc, st := g.location, g.status
		g.mu.Unlock()
		if loc == "" || strings.HasPrefix(r.URL.Path, "/final") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
			return
		}
		http.Redirect(w, r, loc, st)
	}
	g.origin = httptest.NewTLSServer(http.HandlerFunc(redir))
	g.plainOrigin = httptest.NewServer(http.HandlerFunc(redir))
	g.target = httptest.NewTLSServer(http.HandlerFunc(g.sink.handler))
	g.plainSink = httptest.NewServer(http.HandlerFunc(g.plain.handler))
	t.Cleanup(func() { g.origin.Close(); g.plainOrigin.Close(); g.target.Close(); g.plainSink.Close() })
	return g
}

func (g *redirectRig) redirectTo(status int, loc string) {
	g.mu.Lock()
	g.status, g.location = status, loc
	g.mu.Unlock()
}

// client: "runtime.example" and "colab.research.google.com" are the
// origin, every other name or address the sink (TLS on 443, plain
// otherwise); only 127.0.0.1 is dialled as it is.
func (g *redirectRig) client() *http.Client {
	addr := func(s *httptest.Server) string { return s.Listener.Addr().String() }
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	tr.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
		h, port, _ := net.SplitHostPort(a)
		dst := a
		if h != "127.0.0.1" { // any other name or address is one of the rig's servers
			origin := strings.EqualFold(h, "runtime.example") || strings.EqualFold(h, "colab.research.google.com")
			switch {
			case origin && port == "443":
				dst = addr(g.origin)
			case origin:
				dst = addr(g.plainOrigin)
			case port == "443":
				dst = addr(g.target)
			default:
				dst = addr(g.plainSink)
			}
		}
		return (&net.Dialer{}).DialContext(ctx, network, dst)
	}
	return withRedirectPolicy(&http.Client{Transport: tr}, nil)
}

func (g *redirectRig) runtime(t *testing.T, base string) *runtimeClient {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	hc := g.client()
	return &runtimeClient{hc: hc, up: hc, base: u, token: testProxyToken, agent: "lectureflow"}
}

func (g *redirectRig) hitsOfOrigin() []sinkHit {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]sinkHit(nil), g.originHits...)
}

func assertRefused(t *testing.T, err error, host string) {
	t.Helper()
	if err == nil {
		t.Fatal("redirect followed, want it refused")
	}
	if Code(err) != CodeAPIChanged {
		t.Fatalf("code %q (%v), want %s", Code(err), err, CodeAPIChanged)
	}
	if transient(err) {
		t.Fatalf("a refused redirect counts as transient: %v", err)
	}
	if !strings.Contains(err.Error(), "redirect to "+host+" not followed") {
		t.Fatalf("error %q does not name the target host %s", err, host)
	}
	assertNoSecrets(t, "error", errTexts(err)...)
	if strings.Contains(err.Error(), "/api/") || strings.Contains(err.Error(), "?") {
		t.Fatalf("error %q carries a path or query", err)
	}
}

func TestRedirectSameHostFollowed(t *testing.T) {
	g := newRedirectRig(t)
	r := g.runtime(t, g.plainOrigin.URL)
	g.redirectTo(http.StatusTemporaryRedirect, "/final/kernels")
	if _, err := r.listKernels(context.Background()); err != nil {
		t.Fatal(err)
	}
	// PUT 307 on the same origin: the body goes again, as before
	if err := r.upload(context.Background(), "content/a.flac", make([]byte, 3000), 0, nil); err != nil {
		t.Fatal(err)
	}
	hits := g.hitsOfOrigin()
	if len(hits) != 4 {
		t.Fatalf("origin hits %d, want 4", len(hits))
	}
	if hits[1].header.Get("X-Colab-Runtime-Proxy-Token") != testProxyToken || hits[3].body == 0 || hits[3].method != http.MethodPut {
		t.Fatalf("same-host redirect lost the token or the body: %+v", hits)
	}
	if len(g.sink.all())+len(g.plain.all()) != 0 {
		t.Fatal("a same-host redirect reached another server")
	}
}

func TestRedirectToOtherHostRefused(t *testing.T) {
	g := newRedirectRig(t)
	r := g.runtime(t, g.plainOrigin.URL)
	other := g.plainSink.Listener.Addr().String() // 127.0.0.1, another port
	for _, st := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		g.redirectTo(st, "http://"+other+"/api/kernels?colab-runtime-proxy-token=x")
		_, err := r.listKernels(context.Background())
		assertRefused(t, err, other)
		err = r.upload(context.Background(), "content/a.flac", make([]byte, 3000), 0, nil)
		assertRefused(t, err, other)
	}
	if n := len(g.plain.all()); n != 0 {
		t.Fatalf("the other host got %d requests (token and audio leaked)", n)
	}
}

func TestRedirectToAllowedGoogleHostDropsCredentials(t *testing.T) {
	g := newRedirectRig(t)
	hc := g.client() // not a runtime request: those never leave their host
	for _, loc := range []string{"https://oauth2.googleapis.com/x", "https://OAuth2.GoogleAPIs.com/x", "https://accounts.google.com/ServiceLogin",
		"https://colab.research.google.com/final"} {
		g.redirectTo(http.StatusFound, loc)
		_, _ = plainRequest(hc, http.MethodGet, "https://runtime.example/api/kernels") // whatever the answer, it went there
	}
	hits := append(g.sink.all(), g.hitsOfOrigin()...)
	n := 0
	for _, h := range hits {
		if strings.EqualFold(h.host, "runtime.example") {
			continue
		}
		n++
		if h.header.Get("X-Colab-Runtime-Proxy-Token") != "" || h.header.Get("Authorization") != "" || h.method != http.MethodGet {
			t.Fatalf("redirect to %s kept credentials or the method: %+v", h.host, h)
		}
	}
	if n != 4 {
		t.Fatalf("allowed hosts reached %d times, want 4: %+v", n, hits)
	}
}

func TestRedirectBodyNotResentToAllowedHost(t *testing.T) {
	g := newRedirectRig(t)
	r := g.runtime(t, "https://runtime.example")
	for _, st := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		g.redirectTo(st, "https://oauth2.googleapis.com/upload")
		err := r.upload(context.Background(), "content/a.flac", make([]byte, 3000), 0, nil)
		assertRefused(t, err, "oauth2.googleapis.com")
		_, err = r.startKernel(context.Background()) // POST
		assertRefused(t, err, "oauth2.googleapis.com")
	}
	if n := len(g.sink.all()); n != 0 {
		t.Fatalf("the body was resent to another host %d times", n)
	}
}

func TestRedirectHTTPSDowngradeRefused(t *testing.T) {
	g := newRedirectRig(t)
	r := g.runtime(t, "https://runtime.example")
	g.redirectTo(http.StatusFound, "http://runtime.example/final")
	_, err := r.listKernels(context.Background())
	assertRefused(t, err, "runtime.example")
	g.redirectTo(http.StatusFound, "http://oauth2.googleapis.com/x")
	_, err = r.listKernels(context.Background())
	assertRefused(t, err, "oauth2.googleapis.com")
	if n := len(g.plain.all()); n != 0 {
		t.Fatalf("downgraded request reached the plain server %d times", n)
	}
	for _, h := range g.hitsOfOrigin() {
		if h.host != "runtime.example" {
			t.Fatalf("downgrade reached the plain origin: %+v", h)
		}
	}
}

func TestRedirectLookalikeHostsRefused(t *testing.T) {
	g := newRedirectRig(t)
	r := g.runtime(t, "https://runtime.example")
	for _, host := range []string{"googleusercontent.com.evil.example", "evilgoogleapis.com", "oauth2.googleapis.com.",
		"googleapis.com.evil.example", "accounts.google.com.example.com", "google.com", "evil.example", "192.0.2.1"} {
		g.redirectTo(http.StatusFound, "https://"+host+"/x")
		_, err := r.listKernels(context.Background())
		assertRefused(t, err, host)
	}
	if n := len(g.sink.all()); n != 0 {
		t.Fatalf("lookalike hosts reached %d times", n)
	}
}

func TestRedirectAllowedHostRules(t *testing.T) {
	for _, c := range []struct {
		u  string
		ok bool
	}{
		{"https://oauth2.googleapis.com/", true},
		{"https://OAUTH2.GOOGLEAPIS.COM/", true},
		{"https://oauth2.googleapis.com:443/", true},
		{"https://WWW.GOOGLEAPIS.COM/", false},
		{"https://googleapis.com:443/", false},
		{"https://colab.research.google.com/", true},
		{"https://accounts.google.com/", true},
		{"https://oauth2.googleapis.com:8443/", false},
		{"http://oauth2.googleapis.com/", true}, // the scheme is checkColabRedirect's
		{"https://oauth2.googleapis.com./", false},
		{"https://evilgoogleapis.com/", false},
		{"https://googleapis.com.evil.example/", false},
		{"https://googleusercontent.com/", false},
		{"https://research.google.com/", false},
		{"https://192.0.2.1/", false},
		{"https://[2001:db8::1]/", false},
		{"https://googleapis.comı/", false},
	} {
		u, err := url.Parse(c.u)
		if err != nil {
			continue // net/url refuses it: never a redirect target
		}
		if got := allowedRedirectHost(u); got != c.ok {
			t.Errorf("allowedRedirectHost(%s) = %v, want %v", c.u, got, c.ok)
		}
	}
}

func TestRedirectLimit(t *testing.T) {
	g := newRedirectRig(t)
	r := g.runtime(t, g.plainOrigin.URL)
	g.redirectTo(http.StatusFound, "/loop")
	_, err := r.listKernels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("err %v, want too many redirects", err)
	}
	if n := len(g.hitsOfOrigin()); n != maxRedirects {
		t.Fatalf("%d requests, want %d", n, maxRedirects)
	}
}

func TestRedirectTunnelAndToken(t *testing.T) {
	g := newRedirectRig(t)
	kv := NewMemKV()
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, time.Now(), "")
	log, lb := newTestLogger()
	hc := g.client()
	hc.CheckRedirect = redirectPolicy(log)
	auth := &authManager{kv: kv, hc: hc, tokenURL: "https://oauth2.googleapis.com/token", now: time.Now, secrets: &secretSet{}, log: log,
		access: testAccessToken, accessExp: time.Now().Add(time.Hour)}
	tun := &tunnel{base: "https://colab.research.google.com", hc: hc, auth: auth, agent: "lectureflow", secrets: auth.secrets, log: log}
	g.redirectTo(http.StatusFound, "https://evil.example/x?token=1")
	_, _, err := tun.once(context.Background(), http.MethodGet, "/tun/m/ccu-info", "tun/m/ccu-info", nil, nil, 10*time.Second)
	assertRefused(t, err, "evil.example")

	// The token endpoint answering 307 to another host: the refresh token
	// in the form is not resent.
	auth.tokenURL = "https://runtime.example/token"
	g.redirectTo(http.StatusTemporaryRedirect, "https://oauth2.googleapis.com/token")
	c, err := auth.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = auth.refresh(context.Background(), c, false)
	if err == nil || !strings.Contains(err.Error(), "redirect to oauth2.googleapis.com not followed") {
		t.Fatalf("refresh err %v", err)
	}
	assertNoSecrets(t, "refresh error", errTexts(err)...)
	if n := len(g.sink.all()); n != 0 {
		t.Fatalf("another host got %d requests", n)
	}
	logs := lb.String()
	assertNoSecrets(t, "log", logs)
	if !strings.Contains(logs, "host=evil.example") || strings.Contains(logs, "/x") || strings.Contains(logs, "token=1") {
		t.Fatalf("log should name the host only: %s", logs)
	}
}

func TestNewInstallsRedirectPolicy(t *testing.T) {
	own := &http.Client{}
	b, err := New(Options{KV: NewMemKV(), HTTPClient: own})
	if err != nil {
		t.Fatal(err)
	}
	if own.CheckRedirect != nil {
		t.Fatal("the caller's client was changed")
	}
	if b.o.HTTPClient.CheckRedirect == nil || b.o.UploadClient != b.o.HTTPClient {
		t.Fatal("HTTPClient has no redirect policy")
	}
	b, err = New(Options{KV: NewMemKV()})
	if err != nil {
		t.Fatal(err)
	}
	if b.o.HTTPClient.CheckRedirect == nil || b.o.UploadClient.CheckRedirect == nil || b.o.UploadClient == b.o.HTTPClient {
		t.Fatal("default clients have no redirect policy")
	}
	custom := func(*http.Request, []*http.Request) error { return errors.New("mine") }
	b, err = New(Options{KV: NewMemKV(), HTTPClient: &http.Client{CheckRedirect: custom}, UploadClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(b.o.HTTPClient.CheckRedirect(nil, nil)) != "mine" || b.o.UploadClient.CheckRedirect == nil {
		t.Fatal("a caller's own policy must be kept, a client without one gets the package's")
	}
}

// plainRequest sends a request that is not a runtime request, with both
// credentials and the proxy token in the query (as the Contents API has it).
func plainRequest(hc *http.Client, method, u string) (int, error) {
	req, err := http.NewRequest(method, u+"?colab-runtime-proxy-token="+testProxyToken, strings.NewReader("body"))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+testAccessToken)
	req.Header.Set("X-Colab-Runtime-Proxy-Token", testProxyToken)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// SEC14 N20-L1: Referer carried the previous URL, proxy token included.
func TestRedirectDropsReferer(t *testing.T) {
	g := newRedirectRig(t)
	hc := g.client()
	g.redirectTo(http.StatusFound, "https://accounts.google.com/ServiceLogin")
	if _, err := plainRequest(hc, http.MethodGet, "https://runtime.example/api/contents/a"); err != nil {
		t.Fatal(err)
	}
	hits := g.sink.all()
	if len(hits) != 1 {
		t.Fatalf("sink hits %d, want 1", len(hits))
	}
	if ref := hits[0].header.Get("Referer"); ref != "" {
		t.Fatalf("Referer sent to another host: %q", ref)
	}
	for k, vs := range hits[0].header {
		assertNoSecrets(t, "header "+k, vs...)
	}
}

// SEC14 N20-L1: googleapis.com subdomains include storage buckets.
func TestRedirectStorageHostsRefused(t *testing.T) {
	g := newRedirectRig(t)
	hc := g.client()
	for _, host := range []string{"attacker-bucket.storage.googleapis.com", "storage.googleapis.com", "www.googleapis.com"} {
		g.redirectTo(http.StatusFound, "https://"+host+"/log")
		_, err := plainRequest(hc, http.MethodGet, "https://runtime.example/api/contents/a")
		var rr *redirectRefused
		if !errors.As(err, &rr) || rr.host != host {
			t.Fatalf("%s: err %v, want the redirect refused", host, err)
		}
	}
	if n := len(g.sink.all()); n != 0 {
		t.Fatalf("storage hosts reached %d times", n)
	}
}

// SEC14 N20-L2: 301/302/303 turn PUT and DELETE into GET; a sign-in page's
// 200 must not count as an upload or a deletion.
func TestRedirectRewrittenMethodNotFollowed(t *testing.T) {
	g := newRedirectRig(t)
	r := g.runtime(t, "https://runtime.example")
	for _, st := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		g.redirectTo(st, "https://accounts.google.com/ServiceLogin")
		err := r.upload(context.Background(), "content/a.flac", make([]byte, 3000), 0, nil)
		assertRefused(t, err, "accounts.google.com")
		err = r.remove(context.Background(), "content/a.flac")
		assertRefused(t, err, "accounts.google.com")
		// not a runtime request either: the first request's method decides
		for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPost} {
			_, err = plainRequest(g.client(), m, "https://runtime.example/x")
			var rr *redirectRefused
			if !errors.As(err, &rr) {
				t.Fatalf("%s %d: err %v, want the redirect refused", m, st, err)
			}
		}
	}
	if n := len(g.sink.all()); n != 0 {
		t.Fatalf("rewritten requests reached the sign-in host %d times", n)
	}
}

// SEC14 N20 residual: on the same host too, a 301/302/303 that turns PUT,
// DELETE or POST into GET is not followed; 307 keeps the method and is.
func TestRedirectSameHostMethodRewriteRefused(t *testing.T) {
	g := newRedirectRig(t)
	r := g.runtime(t, g.plainOrigin.URL)
	host := g.plainOrigin.Listener.Addr().String()
	for _, st := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		g.redirectTo(st, "/final/x")
		err := r.upload(context.Background(), "content/a.flac", make([]byte, 3000), 0, nil)
		assertRefused(t, err, host)
		if !strings.Contains(err.Error(), "turned into GET") {
			t.Fatalf("err %v", err)
		}
		assertRefused(t, r.remove(context.Background(), "content/a.flac"), host)
		_, err = r.startKernel(context.Background())
		assertRefused(t, err, host)
		// the token refresh (POST) is not a runtime request: refused as well
		_, err = plainRequest(g.client(), http.MethodPost, g.plainOrigin.URL+"/token")
		var rr *redirectRefused
		if !errors.As(err, &rr) {
			t.Fatalf("POST %d: err %v, want the redirect refused", st, err)
		}
	}
	for _, h := range g.hitsOfOrigin() {
		if strings.HasPrefix(h.method, "GET") {
			t.Fatalf("a rewritten request reached the server: %+v", h)
		}
	}
	// 307 / 308 on the same origin: followed with method and body
	before := len(g.hitsOfOrigin())
	for _, st := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		g.redirectTo(st, "/final/x")
		if err := r.upload(context.Background(), "content/a.flac", make([]byte, 3000), 0, nil); err != nil {
			t.Fatalf("%d: %v", st, err)
		}
		if err := r.remove(context.Background(), "content/a.flac"); err != nil {
			t.Fatalf("%d: %v", st, err)
		}
	}
	hits := g.hitsOfOrigin()[before:]
	if len(hits) != 8 || hits[1].method != http.MethodPut || hits[1].body == 0 || hits[3].method != http.MethodDelete {
		t.Fatalf("307/308 hits: %+v", hits)
	}
}

// SEC14 N20-L2: a runtime request never leaves its host, allowed or not.
func TestRedirectRuntimeRequestNeverCrossHost(t *testing.T) {
	g := newRedirectRig(t)
	r := g.runtime(t, "https://runtime.example")
	for _, host := range []string{"colab.research.google.com", "accounts.google.com", "oauth2.googleapis.com"} {
		g.redirectTo(http.StatusFound, "https://"+host+"/final")
		_, err := r.listKernels(context.Background())
		assertRefused(t, err, host)
		_, err = r.download(context.Background(), "content/r.json")
		assertRefused(t, err, host)
	}
	if n := len(g.sink.all()); n != 0 {
		t.Fatalf("runtime requests reached another host %d times", n)
	}
	for _, h := range g.hitsOfOrigin() {
		if h.host != "runtime.example" {
			t.Fatalf("a runtime request reached %s", h.host)
		}
	}
}
