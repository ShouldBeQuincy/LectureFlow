// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/procutil"
)

// publicIP is the tests' public media server as an address written in a
// URL: of the documentation range, reached on 127.0.0.1 by loopbackWire.
// A URL with it needs no look-up by anyone.
const publicIP = "203.0.113.11"

// guardQ is a queue as the product has it (this computer not allowed)
// whose media server plays a public one: cdn.example.com, cdn2.example.com
// and publicIP, on the media server's port. inner plays a server of this
// computer that no download may reach. The task of a request made by
// track has no cookies and a page on another origin, so Go does not
// follow its redirects first: aria2c gets the URL as it is and follows
// them itself (SEC16 R1).
type guardQ struct {
	*qenv
	inner                 *httptest.Server
	innerHits, innerConns atomic.Int64

	mu      sync.Mutex
	lookups map[string]int
}

func newGuardQ(t *testing.T, tweak func(*Options)) *guardQ {
	t.Helper()
	g := &guardQ{lookups: map[string]int{}}
	g.qenv = newQEnv(t, func(o *Options) {
		o.allowLoopback = false
		if tweak != nil {
			tweak(o)
		}
	})
	names := map[string]func(nth int) []string{
		"cdn.example.com":       always(publicIP),
		"cdn2.example.com":      always("203.0.113.12"),
		"lms.publicuni.example": always("203.0.113.14"),
		"loop.example":          always("127.0.0.1"),
		"intranet.example":      always("10.0.0.5"),
		// DNS rebinding: public for the check before the URL is handed
		// over, this computer for every look-up after it
		"rebind.example": func(nth int) []string {
			if nth == 1 {
				return []string{"203.0.113.20"}
			}
			return []string{"127.0.0.1"}
		},
	}
	g.q.local.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.lookups[host]++
		answer, ok := names[strings.ToLower(host)]
		if !ok {
			return nil, errors.New("no such host (test)")
		}
		var out []netip.Addr
		for _, a := range answer(g.lookups[host]) {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
	g.q.local.dial = loopbackWire
	// should a download get past the guard, the fake aria2c dials this
	// computer: not a name looked up for real, not an address of the
	// tests that is no server of theirs
	g.aria.mu.Lock()
	for name := range names {
		g.aria.hostsAt[name] = "127.0.0.1"
	}
	for _, host := range []string{"www.youtube.com", publicIP, "203.0.113.12", "203.0.113.20", "10.0.0.5", "169.254.169.254",
		"2130706433", "0x7f.1", "localhost", "::1", "::ffff:127.0.0.1"} {
		g.aria.hostsAt[host] = "127.0.0.1"
	}
	g.aria.mu.Unlock()
	g.media.mu.Lock()
	g.media.cookie, g.media.refPrefix = "", "" // no session cookie; an https page, plain http media: no Referer
	g.media.mu.Unlock()
	g.inner = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.innerHits.Add(1)
		io.WriteString(w, "INNER SECRET")
	}))
	g.inner.Listener = countingListener{g.inner.Listener, &g.innerConns}
	g.inner.Start()
	t.Cleanup(g.inner.Close)
	return g
}

// mediaURL is a URL of the media server by host (a name or publicIP).
func (g *guardQ) mediaURL(host, path string) string {
	return "http://" + host + ":" + portOfServer(g.media.srv) + path + "?Signature=" + testSig
}

// track is a request for one file track at rawURL.
func (g *guardQ) track(session, rawURL string) *bridge.EnqueueRequest {
	req := g.request(session, bridge.Track{URL: rawURL, Kind: bridge.KindAudio, Format: bridge.FormatFile, Role: "audio", SizeBytes: -1})
	req.Referer, req.Cookies = "https://lms.publicuni.example/Panopto/Pages/Viewer.aspx?id=1", nil
	return req
}

// redirect makes the media server answer path with a 302 to target.
func (g *guardQ) redirect(path, target string) {
	g.media.mu.Lock()
	g.media.redir[path] = target
	g.media.mu.Unlock()
}

// SEC16 R1, the whole queue: the media URL passes every check and is
// handed to aria2c, the server answers with a redirect, and aria2c follows
// it by itself. Through the guard the task fails with the rule's code and
// host, and the server the redirect points at gets nothing.
func TestQueueGuardRefusesWhereARedirectLeads(t *testing.T) {
	g := newGuardQ(t, nil)
	in := portOfServer(g.inner)
	g.media.put("/files/a.m4a", pattern(1, 5000))
	// two hops: a public server first, then this computer
	g.redirect("/files/hop2.m4a", "http://127.0.0.1:"+in+"/secret")
	for i, c := range []struct {
		name, target string
		code, host   string
	}{
		{"this computer", "http://127.0.0.1:" + in + "/secret", CodeLoopbackAddress, "127.0.0.1"},
		{"localhost", "http://localhost:" + in + "/secret", CodeLoopbackAddress, "localhost"},
		{"IPv6 loopback", "http://[::1]:" + in + "/secret", CodeLoopbackAddress, "::1"},
		{"IPv4-mapped loopback", "http://[::ffff:127.0.0.1]:" + in + "/secret", CodeLoopbackAddress, "::ffff:127.0.0.1"},
		{"a decimal address", "http://2130706433:" + in + "/secret", CodeLoopbackAddress, "2130706433"},
		{"a hexadecimal address", "http://0x7f.1:" + in + "/secret", CodeLoopbackAddress, "0x7f.1"},
		{"the local network", "http://10.0.0.5:" + in + "/secret", CodeLocalAddress, "10.0.0.5"},
		{"the cloud metadata address", "http://169.254.169.254:" + in + "/latest/meta-data", CodeLocalAddress, "169.254.169.254"},
		{"a name of the local network", "http://intranet.example:" + in + "/secret", CodeLocalAddress, "intranet.example"},
		{"a name of this computer", "http://loop.example:" + in + "/secret", CodeLoopbackAddress, "loop.example"},
		{"a listed site", "http://www.youtube.com:" + in + "/watch", CodeSiteBlocked, "www.youtube.com"},
		{"a second hop", g.mediaURL("cdn2.example.com", "/files/hop2.m4a"), CodeLoopbackAddress, "127.0.0.1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := fmt.Sprintf("/files/r%d.m4a", i)
			g.redirect(path, c.target)
			info := g.wait(g.enqueue(g.track(fmt.Sprint("s", i), g.mediaURL("cdn.example.com", path))), bridge.StateFailed)
			if info.Error == nil || info.Error.Code != c.code || info.Error.Params["host"] != c.host {
				t.Fatalf("%+v, want %s {host: %s}", info.Error, c.code, c.host)
			}
			if len(g.media.requests(path)) == 0 {
				t.Errorf("the media URL was not asked for: aria2c was not given it")
			}
			if adds, _, _, _, _ := g.aria.snapshot(); len(adds) != i+1 {
				t.Errorf("aria2c was given %d downloads, want %d (the URL passes the checks before)", len(adds), i+1)
			}
		})
	}
	if g.innerHits.Load() != 0 || g.innerConns.Load() != 0 {
		t.Fatalf("the inner server got %d requests on %d connections", g.innerHits.Load(), g.innerConns.Load())
	}
	// a redirect to another public server is followed
	g.redirect("/files/ok.m4a", g.mediaURL("cdn2.example.com", "/files/a.m4a"))
	info := g.wait(g.enqueue(g.track("ok", g.mediaURL("cdn.example.com", "/files/ok.m4a"))), bridge.StateDone)
	if !bytes.Equal(readAll(t, g.lecture(info.LectureID).MediaPath), pattern(1, 5000)) {
		t.Fatal("the file differs")
	}
	if g.innerHits.Load() != 0 || g.innerConns.Load() != 0 {
		t.Fatal("the inner server was reached")
	}
}

// DNS rebinding, the whole queue: the name is public when the URL is
// checked before it is handed over and this computer when the connection
// is made. aria2c resolves nothing; the guard looks the name up for the
// connection and refuses what it finds.
func TestQueueGuardRefusesARebindingName(t *testing.T) {
	g := newGuardQ(t, nil)
	g.media.put("/files/a.m4a", pattern(1, 5000))
	info := g.wait(g.enqueue(g.track("rb", g.mediaURL("rebind.example", "/files/a.m4a"))), bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeLoopbackAddress || info.Error.Params["host"] != "rebind.example" {
		t.Fatalf("%+v", info.Error)
	}
	if seen := g.media.requests("/files/a.m4a"); len(seen) != 0 {
		t.Fatalf("the server was asked: %+v", seen)
	}
	if adds, _, _, _, _ := g.aria.snapshot(); len(adds) != 1 {
		t.Fatalf("aria2c was given %+v (the first answer is public)", adds)
	}
	g.mu.Lock()
	n := g.lookups["rebind.example"]
	g.mu.Unlock()
	if n < 2 {
		t.Errorf("%d look-ups: the guard did not look the name up itself", n)
	}
}

// The guard's sessions follow the tasks: one per run, gone when the run
// ends however it ends (done, failed, cancelled), its connections with it;
// the guard itself goes with aria2c.
func TestQueueGuardSessionsEndWithTheirRuns(t *testing.T) {
	g := newGuardQ(t, func(o *Options) { o.IdleTimeout = 200 * time.Millisecond })
	sessions := func() int {
		g.q.eng.mu.Lock()
		in := g.q.eng.inst
		g.q.eng.mu.Unlock()
		if in == nil {
			return 0
		}
		in.guard.mu.Lock()
		defer in.guard.mu.Unlock()
		return len(in.guard.sessions)
	}
	g.media.put("/files/a.m4a", pattern(1, 5000))
	g.media.put("/files/v.mp4", pattern(2, 300_000))
	g.wait(g.enqueue(g.track("done", g.mediaURL(publicIP, "/files/a.m4a"))), bridge.StateDone)
	g.redirect("/files/r.m4a", "http://127.0.0.1:"+portOfServer(g.inner)+"/secret")
	g.wait(g.enqueue(g.track("failed", g.mediaURL(publicIP, "/files/r.m4a"))), bridge.StateFailed)
	// a download that the server holds after its first bytes, cancelled
	g.media.stall("/files/v.mp4")
	id := g.enqueue(g.track("cancelled", g.mediaURL(publicIP, "/files/v.mp4")))
	g.waitFor("a partial file", func() bool {
		fi, err := os.Stat(filepath.Join(g.q.taskDir(id), "t0.mp4.part"))
		return err == nil && fi.Size() > 0
	})
	if sessions() != 1 {
		t.Fatalf("%d sessions while one task runs", sessions())
	}
	g.q.eng.mu.Lock()
	guard := g.q.eng.inst.guard
	g.q.eng.mu.Unlock()
	start := time.Now()
	if info, err := g.q.Cancel(context.Background(), id); err != nil || info.State != bridge.StateCancelled {
		t.Fatalf("cancel %+v %v", info, err)
	}
	g.waitFor("the cancelled run's session gone", func() bool { return sessions() == 0 })
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the cancellation took %v", d)
	}
	if g.innerHits.Load() != 0 {
		t.Fatal("the inner server was reached")
	}
	// aria2c stops when idle: its guard stops listening
	g.waitFor("aria2c stopped", func() bool { return !g.q.eng.running() })
	g.waitFor("the guard closed", func() bool {
		c, err := net.DialTimeout("tcp", guard.addr, time.Second)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
}

// keepProxyEnv starts the real aria2c like execLauncher but with the
// proxy variables in its environment, set to a proxy that does not exist
// and a no_proxy that names the tests' servers: were aria2c to read them,
// its downloads would fail or go directly.
func keepProxyEnv(exe string, args []string, out io.Writer) (Process, error) {
	if out == nil {
		out = io.Discard
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(launchEnv(os.Environ()), "http_proxy=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1",
		"ftp_proxy=http://127.0.0.1:1", "all_proxy=http://127.0.0.1:1", "no_proxy="+publicIP+",127.0.0.1,localhost")
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdout, cmd.Stderr = out, out
	hideWindow(cmd)
	proc, err := procutil.Start(cmd, procutil.Tied)
	if err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, proc: proc}, nil
}

// The bundled aria2c through the guard, end to end: a download from a
// public server arrives whole (several connections, byte ranges); where a
// redirect leads to this computer, the local network, a listed site or an
// ftp:// address the task fails with the rule's code and the server
// pointed at is never connected to (SEC16 R1, measured there with this
// aria2c: it followed them all); a download added without a session's
// options is refused instead of made directly; a cancelled task's
// connections end; the proxy variables decide nothing, taken out of
// aria2c's environment or not. Everything is on this computer: the
// "public" server is an address of the documentation range, reached on
// 127.0.0.1 through the guard's test wire, and no name is looked up.
// Skipped where no bundled aria2c is staged (like
// TestRealAria2cAcceptsOurOptions).
func TestRealAria2cThroughGuard(t *testing.T) {
	exe := realAria2c()
	if exe == "" {
		t.Skip("no bundled aria2c (stage the desktop resources or set LF_ARIA2C)")
	}
	for _, mode := range []string{"the product's environment", "proxy variables kept"} {
		t.Run(mode, func(t *testing.T) {
			g := newGuardQ(t, func(o *Options) {
				o.Launcher, o.ariaVersion = nil, nil // the real aria2c and its --version
				if mode == "proxy variables kept" {
					o.Launcher = keepProxyEnv
				}
				o.Aria2c = exe
				o.PollInterval = 50 * time.Millisecond
			})
			in := portOfServer(g.inner)
			big := pattern(3, 3<<20)
			g.media.put("/files/big.m4a", big)
			g.media.put("/files/a.m4a", pattern(1, 5000))

			// a download, split into connections
			info := g.wait(g.enqueue(g.track("ok", g.mediaURL(publicIP, "/files/big.m4a"))), bridge.StateDone)
			if !bytes.Equal(readAll(t, g.lecture(info.LectureID).MediaPath), big) {
				t.Fatal("the file differs")
			}
			ranged := 0
			for _, r := range g.media.requests("/files/big.m4a") {
				if r.Range != "" {
					ranged++
				}
			}
			if ranged == 0 {
				t.Errorf("no byte ranges: %+v", g.media.requests("/files/big.m4a"))
			}
			// a redirect to another path of the public server is followed
			g.redirect("/files/ok.m4a", g.mediaURL(publicIP, "/files/a.m4a"))
			info = g.wait(g.enqueue(g.track("redirected", g.mediaURL(publicIP, "/files/ok.m4a"))), bridge.StateDone)
			if !bytes.Equal(readAll(t, g.lecture(info.LectureID).MediaPath), pattern(1, 5000)) {
				t.Fatal("the redirected file differs")
			}

			for i, c := range []struct {
				name, target string
				code, host   string
			}{
				{"this computer", "http://127.0.0.1:" + in + "/secret", CodeLoopbackAddress, "127.0.0.1"},
				{"localhost", "http://localhost:" + in + "/secret", CodeLoopbackAddress, "localhost"},
				{"IPv6 loopback", "http://[::1]:" + in + "/secret", CodeLoopbackAddress, "::1"},
				{"a decimal address", "http://2130706433:" + in + "/secret", CodeLoopbackAddress, "2130706433"},
				{"https on this computer", "https://127.0.0.1:" + in + "/secret", CodeLoopbackAddress, "127.0.0.1"},
				{"the local network", "http://10.0.0.5:" + in + "/secret", CodeLocalAddress, "10.0.0.5"},
				{"a listed site", "http://www.youtube.com:" + in + "/watch", CodeSiteBlocked, "www.youtube.com"},
				{"ftp", "ftp://127.0.0.1:" + in + "/x.bin", CodeUnusualHost, "ftp://127.0.0.1"},
			} {
				path := fmt.Sprintf("/files/r%d.m4a", i)
				g.redirect(path, c.target)
				info := g.wait(g.enqueue(g.track(fmt.Sprint("s", i), g.mediaURL(publicIP, path))), bridge.StateFailed)
				if info.Error == nil || info.Error.Code != c.code || info.Error.Params["host"] != c.host {
					t.Errorf("%s: %+v, want %s {host: %s}", c.name, info.Error, c.code, c.host)
				}
				if len(g.media.requests(path)) == 0 {
					t.Errorf("%s: the media URL was not asked for", c.name)
				}
			}
			if g.innerHits.Load() != 0 || g.innerConns.Load() != 0 {
				t.Fatalf("the inner server got %d requests on %d connections", g.innerHits.Load(), g.innerConns.Load())
			}

			// a download without a session's options: refused (407), not made
			ctx := context.Background()
			inst, err := g.q.eng.acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			g.media.put("/files/bare.m4a", pattern(4, 100))
			gid, err := inst.rpc.addURI(ctx, g.mediaURL(publicIP, "/files/bare.m4a"), map[string]any{"dir": t.TempDir(), "out": "bare.part"})
			if err != nil {
				t.Fatal(err)
			}
			status := ""
			for k := 0; k < 300 && status != "error" && status != "complete"; k++ {
				time.Sleep(50 * time.Millisecond)
				if sts, err := inst.rpc.tellStatuses(ctx, []string{gid}); err == nil {
					status = sts[0].Status
				}
			}
			g.q.stopGIDs(inst, []string{gid})
			if status != "error" || len(g.media.requests("/files/bare.m4a")) != 0 {
				t.Errorf("a download without the session's options: %q, server asked %d times", status, len(g.media.requests("/files/bare.m4a")))
			}
			g.q.eng.release()

			// a cancelled task: its connections end at once
			g.media.put("/files/v.mp4", pattern(2, 2<<20))
			g.media.stall("/files/v.mp4")
			id := g.enqueue(g.track("cancelled", g.mediaURL(publicIP, "/files/v.mp4")))
			g.waitFor("the first bytes", func() bool { // (aria2c keeps them in memory for a while: not the file's size)
				info, err := g.q.Get(ctx, id)
				return err == nil && info.BytesDone > 0
			})
			start := time.Now()
			if info, err := g.q.Cancel(ctx, id); err != nil || info.State != bridge.StateCancelled {
				t.Fatalf("cancel %+v %v", info, err)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("the cancellation took %v", d)
			}

			// the end: aria2c has exited and its guard no longer listens
			guard := inst.guard
			g.q.Close(5 * time.Second)
			select {
			case <-inst.exited:
			case <-time.After(10 * time.Second):
				t.Fatal("aria2c did not exit")
			}
			if c, err := net.DialTimeout("tcp", guard.addr, time.Second); err == nil {
				c.Close()
				t.Errorf("the guard still listens")
			}
			if logs := g.logs.String(); strings.Contains(logs, testSig) {
				t.Errorf("the log holds a signed URL:\n%s", logs)
			}
		})
	}
	t.Run("redirects across schemes", func(t *testing.T) { realAriaAcrossSchemes(t, exe) })
}

// dualListener serves plain http and TLS on one port, as a server may:
// a connection whose first byte is a TLS handshake record is TLS.
type dualListener struct {
	net.Listener
	tls   *tls.Config
	conns chan net.Conn
	done  chan struct{}
}

func newDualListener(t *testing.T, cfg *tls.Config) *dualListener {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &dualListener{Listener: ln, tls: cfg, conns: make(chan net.Conn), done: make(chan struct{})}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(d.done)
				return
			}
			go func() {
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				br := bufio.NewReader(c)
				first, err := br.Peek(1)
				c.SetReadDeadline(time.Time{})
				if err != nil {
					c.Close()
					return
				}
				var out net.Conn = &peekedConn{Conn: c, r: br}
				if first[0] == 0x16 {
					out = tls.Server(out, d.tls)
				}
				select {
				case d.conns <- out:
				case <-d.done:
					c.Close()
				}
			}()
		}
	}()
	return d
}

func (d *dualListener) Accept() (net.Conn, error) {
	select {
	case c := <-d.conns:
		return c, nil
	case <-d.done:
		return nil, net.ErrClosed
	}
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// SEC19 low-1, low-2 and low-3 with the bundled aria2c: a redirect that
// changes the protocol on the same host and port, or leads to sftp://.
// One server serves plain http and https on the same port; every name is
// answered by the tests' table, every connection the guard makes goes to
// 127.0.0.1, and aria2c's own DNS (were it to resolve a name) asks
// 127.0.0.1:1. The certificate is httptest's (--check-certificate=false
// for this aria2c: the option is not one of a download).
//
//   - https, then http on the same host and port: the http request comes
//     to the guard (its own port), plain, not down the https tunnel with
//     the session's credentials (low-1: aria2c reused the tunnel);
//   - http, then https on the same host and port: aria2c tunnels it and
//     the download completes (low-3: TLS on the plain connection to the
//     guard, which failed);
//   - a redirect to sftp:// of a public name: refused, the sftp server
//     gets no connection (low-2: it went through as a CONNECT).
func realAriaAcrossSchemes(t *testing.T, exe string) {
	logs := &syncBuffer{}
	pinDNS := func(exe string, args []string, out io.Writer) (Process, error) {
		return execLauncher(exe, append(append([]string(nil), args...), "--async-dns=true", "--async-dns-server=127.0.0.1:1",
			"--check-certificate=false"), out)
	}
	e := &engine{exe: exe, launch: pinDNS, hc: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}},
		log: slog.New(slog.NewTextHandler(logs, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	in, err := e.start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer e.stop(in, true)
	var global map[string]string
	if err := in.rpc.call(ctx, "aria2.getGlobalOption", nil, &global); err != nil || global["async-dns-server"] != "127.0.0.1:1" {
		t.Fatalf("aria2c's DNS is not pinned to 127.0.0.1: %v %q", err, global["async-dns-server"])
	}

	// the sftp server: a port that records the connections it gets
	sftp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sftp.Close()
	var sftpConns atomic.Int64
	go func() {
		for {
			c, err := sftp.Accept()
			if err != nil {
				return
			}
			sftpConns.Add(1)
			c.Close()
		}
	}()
	sftpPort := portOf(&url.URL{Host: sftp.Addr().String()})

	type seen struct {
		path, uri string
		tls, auth bool
	}
	var mu sync.Mutex
	var requests []seen
	cert := httptest.NewTLSServer(http.NotFoundHandler()) // (for its certificate)
	cfg := cert.TLS.Clone()
	cert.Close()
	dual := httptest.NewUnstartedServer(nil)
	dual.Listener.Close()
	ln := newDualListener(t, cfg)
	dual.Listener = ln
	port := portOf(&url.URL{Host: ln.Addr().String()})
	base := "dual.example:" + port
	dual.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, seen{path: r.URL.Path, uri: r.RequestURI, tls: r.TLS != nil, auth: r.Header.Get("Proxy-Authorization") != ""})
		mu.Unlock()
		switch r.URL.Path {
		case "/s2h":
			http.Redirect(w, r, "http://"+base+"/b", http.StatusFound)
		case "/h2s":
			http.Redirect(w, r, "https://"+base+"/b", http.StatusFound)
		case "/sftp":
			http.Redirect(w, r, "sftp://sftp.example:"+sftpPort+"/file.bin", http.StatusFound)
		case "/b":
			io.WriteString(w, "DUAL B")
		default:
			http.NotFound(w, r)
		}
	})
	dual.Start()
	defer dual.Close()
	take := func() []seen {
		mu.Lock()
		defer mu.Unlock()
		out := requests
		requests = nil
		return out
	}

	names := map[string]string{"dual.example": "203.0.113.50", "sftp.example": "203.0.113.51"}
	p := &addrPolicy{proxy: noProxy,
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			a, ok := names[strings.ToLower(strings.TrimSuffix(host, "."))]
			if !ok {
				return nil, errors.New("no such host (test)")
			}
			return []netip.Addr{netip.MustParseAddr(a)}, nil
		},
		dial: loopbackWire}
	run := func(rawURL string) (ariaStatus, *guardSession, string) {
		t.Helper()
		s, err := in.guard.open(p.forTask("lms.publicuni.example"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.close)
		dir := t.TempDir()
		opts := map[string]any{"dir": dir, "out": "o.bin", "max-tries": "1"}
		for k, v := range s.options() {
			opts[k] = v
		}
		gid, err := in.rpc.addURI(ctx, rawURL, opts)
		if err != nil {
			t.Fatal(err)
		}
		var st ariaStatus
		for k := 0; k < 300; k++ { // (at most 15 s)
			time.Sleep(50 * time.Millisecond)
			if sts, err := in.rpc.tellStatuses(ctx, []string{gid}); err == nil && len(sts) == 1 {
				st = sts[0]
				if st.Status == "error" || st.Status == "complete" {
					break
				}
			}
		}
		in.rpc.forceRemove(ctx, gid)
		in.rpc.removeResult(ctx, gid)
		body, _ := os.ReadFile(filepath.Join(dir, "o.bin"))
		return st, s, string(body)
	}

	// low-1: https, then plain http on the same host and port
	st, s, body := run("https://" + base + "/s2h")
	got := take()
	t.Logf("https -> http: %s %s %q, the server saw %+v", st.Status, st.ErrorCode, st.ErrorMessage, got)
	for _, r := range got {
		if r.auth {
			t.Errorf("https -> http: the session's credentials reached the server: %+v", r)
		}
		if r.path == "/b" && (r.tls || r.uri != "/b") {
			t.Errorf("https -> http: the plain request went down the https tunnel: %+v", r)
		}
	}
	if st.Status != "complete" || body != "DUAL B" {
		t.Errorf("https -> http: %s %s %q, file %q", st.Status, st.ErrorCode, st.ErrorMessage, body)
	}
	if code, _ := refusalOf(s); code != "" {
		t.Errorf("https -> http: refusal %s", code)
	}

	// low-3: plain http, then https on the same host and port
	st, s, body = run("http://" + base + "/h2s")
	got = take()
	t.Logf("http -> https: %s %s %q, the server saw %+v", st.Status, st.ErrorCode, st.ErrorMessage, got)
	if st.Status != "complete" || body != "DUAL B" {
		t.Errorf("http -> https: %s %s %q, file %q", st.Status, st.ErrorCode, st.ErrorMessage, body)
	}
	if !slices.ContainsFunc(got, func(r seen) bool { return r.path == "/b" && r.tls }) {
		t.Errorf("http -> https: /b was not asked for over TLS: %+v", got)
	}
	if code, _ := refusalOf(s); code != "" {
		t.Errorf("http -> https: refusal %s", code)
	}

	// low-2: a redirect to sftp:// of a public name
	st, s, _ = run("http://" + base + "/sftp")
	take()
	time.Sleep(300 * time.Millisecond) // (a connection would be accepted by now)
	code, host := refusalOf(s)
	t.Logf("sftp: %s %s %q, refusal %s %s, sftp connections %d", st.Status, st.ErrorCode, st.ErrorMessage, code, host, sftpConns.Load())
	if sftpConns.Load() != 0 {
		t.Errorf("sftp: the sftp server got %d connections", sftpConns.Load())
	}
	if st.Status != "error" || code != CodeUnusualHost || host != "sftp://sftp.example" {
		t.Errorf("sftp: %s, refusal %s {host: %q}", st.Status, code, host)
	}
	if f := s.failureOf(st); asFailure(f).code != CodeUnusualHost {
		t.Errorf("sftp: the task's failure %+v", f)
	}
}
