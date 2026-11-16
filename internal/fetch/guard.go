// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"lectureflow/internal/blocklist"
	"lectureflow/internal/db"
)

// The guard proxy (SEC16 R1; user 2026-11-11). aria2c follows redirects by
// itself and has no option to restrict where they lead: a media URL that
// passed every check could answer 302 to http://127.0.0.1/... or an
// address of the user's network, and aria2c went there, saved the answer
// as the download, and looked every name up again on its own. So aria2c
// never connects to a server itself: every connection it makes goes
// through this proxy, a part of LectureFlow, which judges each one by the
// same rules as the checks before a URL is handed over (local.go) and
// makes the connection in aria2c's place.
//
// Where it listens. 127.0.0.1 only, on ports the system picks: never on
// another interface. Each aria2c process has a guard (engine.start,
// engine.stop), and in it each task run (or update download) a session
// with ports of its own, open for as long as the run downloads. A request
// to a session's port must carry the session's user and password
// (Proxy-Authorization), 32 random bytes made for the run and forgotten
// with it; anything else is answered 407, so another program of this
// computer cannot use the guard as a proxy. The guard's own port, the one
// aria2c's command line names, serves no session: every request there is
// answered 407.
//
// Ports per session, not only a password: aria2c keeps the connections to
// a proxy for later requests, by the server's and the proxy's host and
// port, whatever download made them (and whatever protocol: the key has
// no scheme). With one port for all, a tunnel one task was allowed (a
// recording of the local network) could carry another task's request to
// the same server. Sessions share no proxy address, so aria2c lends no
// connection of one to another.
//
// Three ports per session, one per protocol (SEC19 low-1 to low-3):
//
//   - http-proxy: plain http requests by their absolute URI, nothing else
//     (CONNECT is answered 400);
//   - https-proxy: CONNECT, nothing else; the first bytes through the
//     tunnel must be a TLS handshake, else the tunnel is closed and the
//     session refuses;
//   - ftp-proxy (and all-proxy): serves nothing. aria2c asks it for
//     ftp:// ("GET ftp://...", --proxy-method=get) and sftp:// (CONNECT,
//     always: measured on aria2c 1.37.0), which LectureFlow does not
//     fetch; a request there is the session's refusal.
//
// With one port for every protocol aria2c reused a CONNECT tunnel for a
// redirect from https to plain http on the same host and port: the plain
// request, the session's credentials in its Proxy-Authorization, went
// down the tunnel to the server (measured), unjudged; the other way round
// it started TLS on its plain connection to the guard and failed; and an
// sftp:// redirect went through as a CONNECT. With a port per protocol the
// pool keys differ and each request comes to the port of its own
// protocol.
//
// How aria2c is made to use it (guardArgs, guardSession.options):
//
//   - its command line names the guard's own port, without credentials,
//     as the proxy of every protocol, with an empty --no-proxy: a
//     download added without its session's options is refused (407)
//     rather than made directly. (--all-proxy alone is not enough: the
//     environment's no_proxy would send its hosts directly; measured on
//     aria2c 1.37.0.)
//   - each download's addUri options name its session's ports with the
//     session's credentials, which so reach aria2c over its RPC only: not
//     its command line, its environment or a file;
//   - the proxy variables are taken out of aria2c's environment
//     (launchEnv): it reads http_proxy, https_proxy, ftp_proxy, all_proxy
//     and no_proxy itself.
//
// aria2c asks for an https URL with CONNECT host:port and for an http URL
// with the absolute URI (--proxy-method=get); a redirect is a new request
// to the guard, so every hop is judged, whatever its scheme. Anything that
// is not http or https is refused.
//
// What is judged, for every request (guardSession.judge, dialTarget):
//
//  1. the do-not-handle list (internal/blocklist) and a host name with
//     unusual characters. Of an https hop the guard sees the host only,
//     so the list's path entries (linkedin.com/learning) apply to http
//     hops and to the URLs checked before they are handed over;
//  2. the host as written: this computer, numbers resolvers read as an
//     address, the local network unless the recording's page is on it;
//     then, of an http hop, user info (userInfoError): in the request's
//     URL, or as the Authorization header aria2c makes of a URL's user
//     info (LectureFlow gives aria2c no Authorization of its own, and
//     --no-netrc). Of an https hop the header is inside TLS: user info
//     there can only come from a redirect's Location, the redirecting
//     server's own words, since no URL handed over has any (checkURL);
//  3. without a proxy of the user's: the name is looked up here, every
//     address it has must pass, and the guard connects to one of exactly
//     those addresses (dialPinned: the dialler checks the address it
//     connects to once more, and the peer's address afterwards). aria2c
//     resolves nothing, so an answer that changes between the check and
//     the connection (DNS rebinding) has nothing to change. A name that
//     cannot be looked up is not connected to;
//  4. with an http:// proxy of the user's (upstream): the connection goes
//     on through that proxy, which resolves the name; 1 and 2 hold for
//     every hop, the addresses are the proxy's business as before (it
//     may be the only way out, and may well resolve names this computer
//     cannot). A proxy aria2c could not use (SOCKS, https://) refuses the
//     download before (downloads.proxy_unsupported) and here again.
//
// A refusal is answered 403 and kept in the session: the task fails with
// the code of the rule (downloads.loopback_address, local_address,
// site_blocked, unusual_host: guardSession.refused), not with whatever
// aria2c makes of the 403. A connection that could not be made is
// answered 502; aria2c tries again as it does for a server.
//
// The bytes are copied as they come (a tunnel for CONNECT, the response
// body flushed as it arrives otherwise): nothing is kept, nothing is
// logged but a refusal's code and host. A session's connections end with
// it (a cancelled task: at once); a tunnel without a byte either way for
// guardIdle is closed.

const (
	// guardMaxConns bounds the connections of aria2c at once (every
	// session; the queue's own limit is 6 tasks x 2 tracks x 16
	// connections, plus an update).
	guardMaxConns = 512
	// guardHeaderWait bounds the wait for a request's header.
	guardHeaderWait = 15 * time.Second
	// guardDialTimeout bounds making a connection, guardDialEach one
	// address when there are more.
	guardDialTimeout = 30 * time.Second
	guardDialEach    = 10 * time.Second
	// guardIdle: a tunnel without a byte either way for this long is
	// closed (aria2c gives up on a silent server after 60 s itself).
	guardIdle = 5 * time.Minute
	// guardMaxAddrs: the addresses of a name that are tried.
	guardMaxAddrs = 8
)

// ariaProxyFailed is aria2c's error when the proxy answers CONNECT with
// anything but 200 (errorCode 1, after its retries).
const ariaProxyFailed = "Proxy connection failed"

// guard is the proxy between one aria2c process and every server: the
// port aria2c's command line names, where nothing is served, and the
// sessions, each on its own port.
type guard struct {
	ln    net.Listener
	addr  string // "127.0.0.1:port"
	srv   *http.Server
	log   *slog.Logger
	slots chan struct{}
	idle  time.Duration
	stop  chan struct{}
	// done is closed once close has finished: the listener, every session
	// and its connections are closed (a close that meets another one waits
	// for it).
	done chan struct{}

	mu       sync.Mutex
	sessions map[string]*guardSession // by user
	closed   bool
}

// guardServer is the HTTP server of a guard's port (its own, a session's).
func guardServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: guardHeaderWait,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// "OPTIONS *" goes to the handler like any request (407 without
		// the credentials), not answered 200 by net/http (SEC19 info-11)
		DisableGeneralOptionsHandler: true,
		// (the server's own messages may quote a request: dropped)
		ErrorLog: log.New(io.Discard, "", 0),
	}
}

// needAuth answers a request without a session's credentials.
func needAuth(w http.ResponseWriter) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="LectureFlow"`)
	http.Error(w, "LectureFlow: proxy authentication required", http.StatusProxyAuthRequired)
}

// newGuard starts a guard on a free port of 127.0.0.1.
func newGuard(logger *slog.Logger) (*guard, error) {
	return newGuardWith(logger, guardMaxConns, guardIdle)
}

// newGuardWith is newGuard with its limits (tests).
func newGuardWith(logger *slog.Logger, maxConns int, idle time.Duration) (*guard, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("guard proxy: %w", err)
	}
	g := &guard{ln: ln, addr: ln.Addr().String(), log: logger, slots: make(chan struct{}, maxConns), idle: idle,
		stop: make(chan struct{}), done: make(chan struct{}), sessions: map[string]*guardSession{}}
	// the guard's own port: no session's, whatever credentials are sent
	g.srv = guardServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { needAuth(w) }))
	go func() { _ = g.srv.Serve(ln) }()
	go g.sweep()
	return g, nil
}

// guardCloseWait bounds how long a close waits for another close under
// way (SEC43b info-9). Closing takes milliseconds (listeners and
// connections only, nothing that blocks), but should a later change make
// it block, Queue.Close (which waits for the engine's stop) must not hang
// with it: the second close gives up after this, with a warning.
var guardCloseWait = 5 * time.Second

// close ends the guard: the listener, every session and its connections.
// It returns once all of them are closed, also when another close is
// under way (the engine's stop and the goroutine waiting for aria2c's exit
// both close it: the second one returned at once, the listener still
// open; SEC43 info-7), but waits for that one guardCloseWait at most.
func (g *guard) close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		wait := time.NewTimer(guardCloseWait)
		defer wait.Stop()
		select {
		case <-g.done:
		case <-wait.C:
			if g.log != nil {
				g.log.Warn("downloads: the guard proxy's other close has not finished; not waiting for it any longer",
					"waited", guardCloseWait.String())
			}
		}
		return
	}
	defer close(g.done)
	g.closed = true
	sessions := make([]*guardSession, 0, len(g.sessions))
	for _, s := range g.sessions {
		sessions = append(sessions, s)
	}
	g.mu.Unlock()
	close(g.stop)
	for _, s := range sessions {
		s.close()
	}
	_ = g.srv.Close()
}

// sweep closes the tunnels that have been silent for g.idle.
func (g *guard) sweep() {
	every := g.idle / 4
	if every > 30*time.Second {
		every = 30 * time.Second
	}
	if every <= 0 {
		every = time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-tick.C:
		}
		limit := time.Now().Add(-g.idle).UnixNano()
		g.mu.Lock()
		sessions := make([]*guardSession, 0, len(g.sessions))
		for _, s := range g.sessions {
			sessions = append(sessions, s)
		}
		g.mu.Unlock()
		for _, s := range sessions {
			s.closeIdle(limit)
		}
	}
}

// guardArgs make the guard at addr aria2c's proxy for every protocol
// unless a download says otherwise (it names its session's ports, with the
// session's credentials): --all-proxy and each protocol's own, which
// aria2c takes over --all-proxy and reads from its environment; an empty
// --no-proxy, or the environment's no_proxy would send its hosts directly;
// --proxy-method=get, so that an ftp:// URL a redirect leads to is asked of
// the guard by its URL (and refused) instead of tunnelled.
func guardArgs(addr string) []string {
	p := "http://" + addr
	return []string{"--all-proxy=" + p, "--http-proxy=" + p, "--https-proxy=" + p, "--ftp-proxy=" + p, "--no-proxy=", "--proxy-method=get"}
}

// proxyEnvNames are the variables aria2c reads its proxies from.
var proxyEnvNames = []string{"http_proxy", "https_proxy", "ftp_proxy", "all_proxy", "no_proxy"}

// launchEnv is env without the proxy variables, in any case (aria2c reads
// the lower-case names; Windows does not tell the cases apart): the
// environment aria2c is started with. Its proxy is the guard, by its
// options; the user's proxy is the guard's to use.
func launchEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, p := range proxyEnvNames {
			if strings.EqualFold(name, p) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// guardSession is one task run's (or one update download's) use of the
// guard: its ports and credentials, its address policy, its connections,
// and the first refusal.
type guardSession struct {
	g    *guard
	addr *taskAddr
	// the session's own ports ("127.0.0.1:port"): plain http requests,
	// CONNECT (https), and the one that serves nothing (ftp, sftp and any
	// other protocol aria2c would use a proxy for)
	listenHTTP, listenHTTPS, listenOther string
	lns                                  []net.Listener
	srvs                                 []*http.Server
	user, pass                           string
	// trusted are the "host:port" the session may reach whatever their
	// address: an update URL of the user's own sources, which may be a
	// mirror on this computer or the local network (selfupdate's
	// DownloadRequest.UserURLs); never GitHub or another URL of
	// update.json, never a URL of a recording.
	trusted map[string]bool
	rp      *httputil.ReverseProxy
	direct  *http.Transport

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	refusal     error
	netNote     string
	proxyLogged bool                       // proxyFailed has logged
	via         map[string]*http.Transport // by the user's proxy
	tunnels     map[*tunnel]struct{}
	closed      bool
}

// open starts a session judged by addr. trusted are URLs whose hosts (with
// their ports) the session may reach whatever their address.
func (g *guard) open(addr *taskAddr, trusted ...string) (*guardSession, error) {
	user, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	pass, err := randomHex(24)
	if err != nil {
		return nil, err
	}
	s := &guardSession{g: g, addr: addr, user: user, pass: pass, trusted: map[string]bool{},
		via: map[string]*http.Transport{}, tunnels: map[*tunnel]struct{}{}}
	for _, kind := range []portKind{portHTTP, portHTTPS, portOther} {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			for _, ln := range s.lns {
				ln.Close()
			}
			return nil, fmt.Errorf("guard proxy: %w", err)
		}
		s.lns = append(s.lns, ln)
		s.srvs = append(s.srvs, guardServer(sessionPort{s: s, kind: kind}))
		switch kind {
		case portHTTP:
			s.listenHTTP = ln.Addr().String()
		case portHTTPS:
			s.listenHTTPS = ln.Addr().String()
		default:
			s.listenOther = ln.Addr().String()
		}
	}
	for _, raw := range trusted {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			s.trusted[hostPortKey(u.Hostname(), portOf(u))] = true
		}
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.direct = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return s.dialTarget(ctx, host, port)
		},
		DisableCompression:    true, // the bytes as the server sends them
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	s.rp = &httputil.ReverseProxy{
		Rewrite:       func(*httputil.ProxyRequest) {}, // the request as aria2c made it, without the hop-by-hop headers
		Transport:     roundTripFunc(s.roundTrip),
		FlushInterval: -1, // every chunk as it arrives
		ErrorHandler:  s.forwardFailed,
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		s.cancel()
		for _, ln := range s.lns {
			ln.Close()
		}
		return nil, errors.New("the guard proxy has stopped")
	}
	g.sessions[user] = s
	for i, srv := range s.srvs {
		go func() { _ = srv.Serve(s.lns[i]) }()
	}
	return s, nil
}

// portKind is what a session's port serves.
type portKind int

const (
	portHTTP  portKind = iota // plain http requests, by their absolute URI
	portHTTPS                 // CONNECT
	portOther                 // nothing: ftp-proxy and all-proxy
)

// sessionPort is the handler of one of a session's ports.
type sessionPort struct {
	s    *guardSession
	kind portKind
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// options are the proxy options of aria2c's addUri for a download of the
// session: each protocol's port of the session with its credentials (ftp
// and every other protocol: the port that serves nothing), and no-proxy
// empty (guardArgs). The values hold the credentials: they go to aria2c's
// RPC only, never into a log or an error.
func (s *guardSession) options() map[string]string {
	cred := "http://" + s.user + ":" + s.pass + "@"
	return map[string]string{"http-proxy": cred + s.listenHTTP, "https-proxy": cred + s.listenHTTPS,
		"ftp-proxy": cred + s.listenOther, "all-proxy": cred + s.listenOther, "no-proxy": ""}
}

// close ends the session: its ports are closed, and so are its connections.
func (s *guardSession) close() {
	if s == nil {
		return
	}
	s.g.mu.Lock()
	delete(s.g.sessions, s.user)
	s.g.mu.Unlock()
	s.cancel()
	for _, srv := range s.srvs {
		_ = srv.Close() // the listener, and the connections that are no tunnel
	}
	s.mu.Lock()
	s.closed = true
	tunnels := make([]*tunnel, 0, len(s.tunnels))
	for t := range s.tunnels {
		tunnels = append(tunnels, t)
	}
	via := make([]*http.Transport, 0, len(s.via))
	for _, tr := range s.via {
		via = append(via, tr)
	}
	s.mu.Unlock()
	for _, t := range tunnels {
		t.close()
	}
	s.direct.CloseIdleConnections()
	for _, tr := range via {
		tr.CloseIdleConnections()
	}
}

// refused is the first refusal of the session (nil: none): a rule's error
// (localAddrError, blockedSiteError, unusualHostError, a proxy aria2c
// cannot use), which asFailure turns into the task's failure.
func (s *guardSession) refused() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refusal
}

// failureOf is the failure of a download of the session that aria2c
// reports failed: the session's refusal when there is one (aria2c saw a
// 403 from its proxy, which it reads as the server's); a network failure
// when the guard could not make a connection for a CONNECT (aria2c:
// "Proxy connection failed", which without the guard meant a broken
// proxy of the user's); else ariaFailure's. A network failure carries what
// the guard last could not reach.
func (s *guardSession) failureOf(st ariaStatus) error {
	if err := s.refused(); err != nil {
		return asFailure(err)
	}
	err := ariaFailure(st)
	var f *failure
	if !errors.As(err, &f) {
		return err
	}
	if strings.Contains(st.ErrorMessage, ariaProxyFailed) && f.code == CodeEngineFailed {
		f = &failure{code: CodeNetwork, detail: f.detail}
	}
	if f.code == CodeNetwork && s != nil {
		s.mu.Lock()
		note := s.netNote
		s.mu.Unlock()
		if note != "" {
			f = &failure{code: CodeNetwork, params: f.params, detail: f.detail + " (" + note + ")"}
		}
	}
	return f
}

// refuse keeps err as the session's refusal (the first one stays) and
// logs its code and host.
func (s *guardSession) refuse(err error) {
	s.mu.Lock()
	first := s.refusal == nil
	if first {
		s.refusal = err
	}
	s.mu.Unlock()
	if first {
		f := asFailure(err)
		s.g.log.Warn("downloads: the guard proxy refused a connection of aria2c", "code", f.code, "detail", f.detail)
	}
}

// noteNet keeps what could not be reached, for the failure's detail.
func (s *guardSession) noteNet(host string, err error) {
	note := "the guard proxy could not reach " + safeHost(host)
	var f *failure
	if errors.As(err, &f) {
		note = f.detail
	} else if err != nil {
		note += ": " + redactText(err.Error())
	}
	if len(note) > 400 {
		note = note[:400]
	}
	s.mu.Lock()
	s.netNote = note
	s.mu.Unlock()
}

// isRefusal: err is a rule's refusal (not a network failure).
func isRefusal(err error) bool {
	return localFailure(err) != nil || proxyFailure(err) != nil
}

// hostPortKey is a host and port as the trusted set compares them.
func hostPortKey(host, port string) string {
	h, _ := db.HostKey(host)
	return net.JoinHostPort(strings.TrimSuffix(h, "."), port)
}

// portOf is u's port, or its scheme's.
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return "443"
}

// authorized: r carries the session's user and password.
func (s *guardSession) authorized(r *http.Request) bool {
	kind, enc, ok := strings.Cut(r.Header.Get("Proxy-Authorization"), " ")
	if !ok || !strings.EqualFold(kind, "Basic") {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(b), ":")
	if !ok {
		return false
	}
	// (both compared, neither telling the other's result by its time)
	u := subtle.ConstantTimeCompare([]byte(user), []byte(s.user))
	p := subtle.ConstantTimeCompare([]byte(pass), []byte(s.pass))
	return u&p == 1
}

// ServeHTTP is the proxy of one of the session's ports, for requests with
// the session's credentials: a request for an absolute http URL on the
// http port, CONNECT host:port on the https port, a refusal on the other.
func (p sessionPort) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := p.s
	if !s.authorized(r) {
		needAuth(w)
		return
	}
	if p.kind == portOther {
		s.refuseOther(w, r)
		return
	}
	g := s.g
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		http.Error(w, "LectureFlow: too many connections", http.StatusServiceUnavailable)
		return
	}
	// the request ends with its session
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(s.ctx, cancel)()
	r = r.WithContext(ctx)
	switch connect := r.Method == http.MethodConnect; {
	case connect && p.kind == portHTTPS:
		s.connect(w, r)
	case !connect && p.kind == portHTTP:
		s.forward(w, r)
	default: // (aria2c sends neither: a request on the wrong port is not served)
		s.answer(ctx, w, "", errBadTarget)
	}
}

// refuseOther answers a request on the port of ftp-proxy and all-proxy:
// "GET ftp://host/..." (a redirect to ftp://), CONNECT host:port (sftp://,
// which aria2c always tunnels) or anything else. Nothing is served there:
// the request is the session's refusal (otherSchemeError), answered 403.
func (s *guardSession) refuseOther(w http.ResponseWriter, r *http.Request) {
	scheme, host := "", ""
	if r.Method == http.MethodConnect {
		scheme = "sftp"
		if u, err := targetOf(r.RequestURI); err == nil {
			host = u.Hostname()
		}
	} else if r.URL != nil {
		scheme, host = r.URL.Scheme, r.URL.Hostname()
	}
	if scheme == "" {
		scheme = "ftp"
	}
	s.answer(r.Context(), w, host, &otherSchemeError{scheme: safeScheme(scheme), host: host})
}

// errBadTarget: a request the guard cannot read a target from.
var errBadTarget = errors.New("not a proxy request for an http or https address")

// targetOf is the URL a CONNECT authority stands for: https://host:port,
// when the authority is a host and a port and nothing else.
func targetOf(authority string) (*url.URL, error) {
	u, err := url.Parse("https://" + authority)
	if err != nil || u.Host != authority || u.User != nil || u.Hostname() == "" || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errBadTarget
	}
	if n, err := strconv.Atoi(u.Port()); err != nil || n < 1 || n > 65535 {
		return nil, errBadTarget
	}
	return u, nil
}

// answer answers a request that is not served: a refusal with 403, kept
// in the session; a failure to connect with 502, noted for the failure's
// detail unless the request itself was given up (ctx).
func (s *guardSession) answer(ctx context.Context, w http.ResponseWriter, host string, err error) {
	switch {
	case errors.Is(err, errBadTarget):
		http.Error(w, "LectureFlow: "+err.Error(), http.StatusBadRequest)
	case isRefusal(err):
		s.refuse(err)
		http.Error(w, "LectureFlow: refused ("+asFailure(err).code+")", http.StatusForbidden)
	default:
		if ctx.Err() == nil {
			s.noteNet(host, err)
		}
		http.Error(w, "LectureFlow: could not connect", http.StatusBadGateway)
	}
}

// judge is rules 1 and 2 for u, a URL with a host: the do-not-handle list,
// a host name with unusual characters, and the host as written. It tells
// the user's proxy the request goes on through (nil: none).
func (s *guardSession) judge(ctx context.Context, u *url.URL) (*url.URL, error) {
	host := u.Hostname()
	if host == "" {
		return nil, errBadTarget
	}
	category := blocklist.URLCategory(u.String())
	if category == "" {
		category = blocklist.HostCategory(host) // (whatever the URL reads as once written out again)
	}
	switch category {
	case "":
	case blocklist.CategoryUnusualHost:
		return nil, &unusualHostError{host: host}
	default:
		return nil, &blockedSiteError{host: host, category: category}
	}
	if class, final := classifyHost(host); final {
		if err := s.allow(ctx, host, portOf(u), class); err != nil {
			return nil, err
		}
	}
	if u.User != nil { // (an absolute URI's; a CONNECT authority has none: targetOf)
		return nil, &userInfoError{host: host}
	}
	return s.addr.p.upstream(u)
}

// allow is taskAddr.allow, but for the session's trusted hosts, which may
// be on this computer or the local network (never a name with unusual
// characters, never an address that is no address).
func (s *guardSession) allow(ctx context.Context, host, port string, class addrClass) error {
	if (class == classLoopback || class == classLocalNet) && s.trusted[hostPortKey(host, port)] {
		return nil
	}
	return s.addr.allow(ctx, host, class)
}

// addresses is rule 3 for a host that passed judge: the addresses the
// guard may connect to for it. An address as written is itself; localhost
// and *.localhost are this computer whatever a resolver says (RFC 6761);
// any other name is looked up, and every address it has must be allowed.
// A name that cannot be looked up is a network failure.
func (s *guardSession) addresses(ctx context.Context, host, port string) ([]netip.Addr, error) {
	h, _ := db.HostKey(host)
	h = strings.TrimSuffix(h, ".")
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(h); err == nil {
		ips = []netip.Addr{ip}
	} else if class, final := classifyHost(host); final && class == classLoopback {
		ips = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}
	} else {
		ips, err = s.addr.p.resolve(ctx, host)
		if err != nil || len(ips) == 0 {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &failure{code: CodeNetwork, detail: "the guard proxy could not look up the address of " + safeHost(host)}
		}
	}
	for _, ip := range ips {
		if err := s.allow(ctx, host, port, classifyIP(ip)); err != nil {
			return nil, err
		}
	}
	if len(ips) > guardMaxAddrs {
		ips = ips[:guardMaxAddrs]
	}
	return ips, nil
}

// dialTarget connects to host:port, a host that passed judge, without a
// proxy: to one of the addresses that were looked up and allowed here,
// and to no other (addresses, dialPinned).
func (s *guardSession) dialTarget(ctx context.Context, host, port string) (net.Conn, error) {
	ips, err := s.addresses(ctx, host, port)
	if err != nil {
		return nil, err
	}
	trusted := s.trusted[hostPortKey(host, port)]
	check := func(address string) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return &localAddrError{host: host, loopback: true}
		}
		class := classifyIP(ap.Addr())
		if trusted && (class == classLoopback || class == classLocalNet) {
			return nil
		}
		return s.addr.allow(ctx, host, class)
	}
	ctx, cancel := context.WithTimeout(ctx, guardDialTimeout)
	defer cancel()
	var last error
	for i, ip := range ips {
		actx, acancel := ctx, context.CancelFunc(func() {})
		if len(ips)-i > 1 {
			actx, acancel = context.WithTimeout(ctx, guardDialEach)
		}
		c, err := s.addr.p.dialPinned(actx, ip, port, check)
		acancel()
		if err == nil {
			return c, nil
		}
		last = err
		if ctx.Err() != nil || isRefusal(err) {
			break
		}
	}
	return nil, last
}

// dialPinned connects to ip:port and to nothing else: the dialler is given
// the address, not a name, check judges the address it actually connects
// to (net.Dialer.Control), and the peer of the connection must be ip.
func (p *addrPolicy) dialPinned(ctx context.Context, ip netip.Addr, port string, check func(address string) error) (net.Conn, error) {
	address := net.JoinHostPort(ip.String(), port)
	if p.dial != nil {
		if err := check(address); err != nil {
			return nil, err
		}
		return p.dial(ctx, "tcp", address) // (tests: the wire)
	}
	d := &net.Dialer{Timeout: guardDialTimeout, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error { return check(address) }}
	c, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	peer, perr := netip.ParseAddrPort(c.RemoteAddr().String())
	if perr != nil || peer.Addr().Unmap().WithZone("") != ip.Unmap().WithZone("") {
		c.Close()
		return nil, fmt.Errorf("connected to another address than the one checked")
	}
	return c, nil
}

// dialProxy connects to the user's proxy (its address is not judged: a
// school's proxy may well have a private one, local.go transport).
func (p *addrPolicy) dialProxy(ctx context.Context, address string) (net.Conn, error) {
	if p.dial != nil {
		return p.dial(ctx, "tcp", address)
	}
	return (&net.Dialer{Timeout: guardDialTimeout, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", address)
}

// ---------- CONNECT ----------

// connect serves CONNECT host:port: a tunnel to the host, made here.
func (s *guardSession) connect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, err := targetOf(r.RequestURI) // (as sent: net/http has set user info and the like aside in r.Host)
	if err != nil {
		s.answer(ctx, w, "", err)
		return
	}
	host, port := u.Hostname(), u.Port()
	up, err := s.judge(ctx, u)
	if err != nil {
		s.answer(ctx, w, host, err)
		return
	}
	var target net.Conn
	var fromTarget io.Reader
	if up == nil {
		target, err = s.dialTarget(ctx, host, port)
		fromTarget = target
	} else {
		target, fromTarget, err = s.connectVia(ctx, up, u.Host)
	}
	if err != nil {
		s.answer(ctx, w, host, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		target.Close()
		http.Error(w, "LectureFlow: no tunnel", http.StatusInternalServerError)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		target.Close()
		return
	}
	_ = client.SetDeadline(time.Time{})
	t := &tunnel{client: client, target: target}
	t.touch()
	if !s.track(t) {
		t.close()
		return
	}
	defer s.untrack(t)
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		t.close()
		return
	}
	// aria2c tunnels https alone through this port: what it sends first is
	// a TLS handshake record. Anything else (a plain request, an ssh
	// banner) is not let through to the server.
	_ = client.SetReadDeadline(time.Now().Add(guardHeaderWait))
	first, err := rw.Reader.Peek(1)
	_ = client.SetReadDeadline(time.Time{})
	if err != nil {
		t.close()
		return
	}
	if first[0] != tlsHandshake {
		t.close()
		s.refuse(&otherSchemeError{scheme: "tcp", host: host})
		return
	}
	t.run(rw.Reader, fromTarget)
}

// tlsHandshake is the content type of a TLS handshake record, the first
// byte a TLS client sends.
const tlsHandshake = 0x16

// connectVia asks the user's proxy up for a tunnel to authority
// (host:port). The reader holds what the proxy sent after its answer.
func (s *guardSession) connectVia(ctx context.Context, up *url.URL, authority string) (net.Conn, io.Reader, error) {
	ctx, cancel := context.WithTimeout(ctx, guardDialTimeout)
	defer cancel()
	c, err := s.addr.p.dialProxy(ctx, up.Host)
	if err != nil {
		return nil, nil, errors.New("the proxy could not be reached") // (its address stays out of the detail)
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	req := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n"
	if up.User != nil {
		pass, _ := up.User.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(up.User.Username()+":"+pass)) + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		c.Close()
		return nil, nil, errors.New("the proxy closed the connection")
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errors.New("the proxy gave no answer to CONNECT")
	}
	if res.StatusCode != http.StatusOK {
		c.Close()
		return nil, nil, fmt.Errorf("the proxy answered CONNECT with HTTP %d", res.StatusCode)
	}
	if !stop() {
		c.Close()
		return nil, nil, ctx.Err()
	}
	return c, br, nil
}

// tunnel is one CONNECT: the two connections and when a byte last passed.
type tunnel struct {
	client, target net.Conn
	last           atomic.Int64
	once           sync.Once
}

func (t *tunnel) touch() { t.last.Store(time.Now().UnixNano()) }

func (t *tunnel) close() {
	t.once.Do(func() {
		t.client.Close()
		t.target.Close()
	})
}

// run copies both ways until both ends are done, or one failed.
func (t *tunnel) run(fromClient, fromTarget io.Reader) {
	done := make(chan error, 2)
	go func() { done <- t.copy(t.target, fromClient) }()
	go func() { done <- t.copy(t.client, fromTarget) }()
	if err := <-done; err != nil {
		t.close()
	}
	<-done
	t.close()
}

// copy moves the bytes of src to dst as they come; at the end of src the
// writing side of dst is closed, the other direction goes on.
func (t *tunnel) copy(dst net.Conn, src io.Reader) error {
	buf := make([]byte, 64<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			t.touch()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			if cw, ok := dst.(interface{ CloseWrite() error }); ok {
				return cw.CloseWrite()
			}
			return io.ErrClosedPipe // (no half-close: the tunnel ends)
		}
		if err != nil {
			return err
		}
	}
}

func (s *guardSession) track(t *tunnel) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.tunnels[t] = struct{}{}
	return true
}

func (s *guardSession) untrack(t *tunnel) {
	s.mu.Lock()
	delete(s.tunnels, t)
	s.mu.Unlock()
}

// closeIdle closes the session's tunnels silent since limit.
func (s *guardSession) closeIdle(limit int64) {
	s.mu.Lock()
	var idle []*tunnel
	for t := range s.tunnels {
		if t.last.Load() < limit {
			idle = append(idle, t)
		}
	}
	s.mu.Unlock()
	for _, t := range idle {
		t.close()
	}
}

// ---------- plain http ----------

// forward serves a request for an absolute http URL: it is made here and
// the answer copied as it comes. A redirect is the answer like any other;
// aria2c asks the guard for where it leads.
func (s *guardSession) forward(w http.ResponseWriter, r *http.Request) {
	ctx, u := r.Context(), r.URL
	if !u.IsAbs() || u.Host == "" {
		s.answer(ctx, w, "", errBadTarget)
		return
	}
	if !strings.EqualFold(u.Scheme, "http") {
		// ftp:// after a redirect (aria2c asks its proxy for the URL), or
		// anything else: LectureFlow fetches http and https only
		s.answer(ctx, w, u.Hostname(), &otherSchemeError{scheme: safeScheme(u.Scheme), host: u.Hostname()})
		return
	}
	if _, err := s.judge(ctx, u); err != nil {
		s.answer(ctx, w, u.Hostname(), err)
		return
	}
	// aria2c writes a URL's user info into the request line (judge refuses
	// it there) and sends it as Basic authentication too (measured on
	// aria2c 1.37.0, review R53); both are checked. It has no Authorization
	// of LectureFlow's to send (requestHeaders, --no-netrc)
	if len(r.Header.Values("Authorization")) > 0 {
		s.answer(ctx, w, u.Hostname(), &userInfoError{host: u.Hostname()})
		return
	}
	s.rp.ServeHTTP(w, r)
}

// roundTrip makes a request that passed judge: directly, to an address
// the guard looked up and allowed (s.direct), or through the user's proxy.
func (s *guardSession) roundTrip(r *http.Request) (*http.Response, error) {
	up, err := s.addr.p.upstream(r.URL)
	if err != nil {
		return nil, err
	}
	if up == nil {
		return s.direct.RoundTrip(r)
	}
	res, err := s.viaProxy(up).RoundTrip(r)
	if err != nil {
		// (net/http's error names the proxy's address: it stays out of the
		// task's detail, SEC19 info-1; the log gets its host once)
		if r.Context().Err() == nil {
			s.proxyFailed(up, err)
		}
		return nil, errProxyFailed
	}
	return res, nil
}

// errProxyFailed: a request through the user's proxy failed (the proxy
// could not be reached, or broke the connection). Its address is not in
// the text, which goes into the task's detail.
var errProxyFailed = errors.New("the connection through the proxy failed")

// proxyFailed logs, once per session, that the user's proxy up failed:
// its host, never its port, user or password.
func (s *guardSession) proxyFailed(up *url.URL, err error) {
	s.mu.Lock()
	first := !s.proxyLogged
	s.proxyLogged = true
	s.mu.Unlock()
	if first {
		s.g.log.Warn("downloads: the guard proxy could not go through the proxy set", "proxy_host", safeHost(up.Hostname()),
			"timeout", isNetTimeout(err))
	}
}

// isNetTimeout: err is a network timeout.
func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// viaProxy is the session's transport through the user's proxy up.
func (s *guardSession) viaProxy(up *url.URL) *http.Transport {
	key := up.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	if tr, ok := s.via[key]; ok {
		return tr
	}
	tr := &http.Transport{
		Proxy: http.ProxyURL(up),
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return s.addr.p.dialProxy(ctx, address)
		},
		DisableCompression:    true,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	s.via[key] = tr
	return tr
}

// forwardFailed answers a request that could not be made.
func (s *guardSession) forwardFailed(w http.ResponseWriter, r *http.Request, err error) {
	s.answer(r.Context(), w, r.URL.Hostname(), err)
}
