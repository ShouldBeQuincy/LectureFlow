// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeAria is a stand-in for aria2c: the JSON-RPC methods the queue uses
// (getVersion, addUri, tellStatus through system.multicall, forceRemove,
// removeDownloadResult, shutdown, forceShutdown), checking the secret of
// every call, and real HTTP downloads with the headers given (resuming a
// partial file with a Range request, like aria2c's --continue). It runs in
// the test process (fakeLauncher) or as a separate process (TestMain with
// LF_FAKE_ARIA2C, for the process manager test).
type fakeAria struct {
	hc *http.Client

	mu        sync.Mutex
	secret    string
	conf      string // what the --conf-path file held
	args      []string
	ln        net.Listener
	srv       *http.Server
	dls       map[string]*fakeDL
	seq       int
	active    int
	maxActive int
	adds      []fakeAdd
	removed   []string
	exited    chan struct{}
	shutdowns int
	// slowExit: answer shutdown but do not exit (aria2c 1.37.0 takes
	// 4.3–4.5 s after its answer); only a kill ends it.
	slowExit bool
	// likeAria2c: a taken 127.0.0.1 port is handled as aria2c does
	// (the separate-process mode).
	likeAria2c bool
	// noBitTorrent, noMetalink: built without them, like the macOS build;
	// their options are then unknown and refused (exit 28).
	noBitTorrent, noMetalink bool
	// noAsyncDNS: built without c-ares, like the macOS build: --async-dns
	// is refused like the options above (SEC11 low-1; aria2c registers it
	// only with Async DNS, assumed: no build without it was at hand).
	noAsyncDNS bool
	// hostsAt resolves test names (host → address; no DNS in the tests).
	hostsAt map[string]string
}

// The "Enabled Features:" of the official Windows build and of the macOS
// build (packaging/aria2c).
const (
	featuresWindows = "Async DNS, BitTorrent, Firefox3 Cookie, GZip, HTTPS, Message Digest, Metalink, XML-RPC, SFTP"
	featuresMac     = "GZip, HTTPS, Message Digest"
)

// fakeVersion is aria2c --version of a build with features.
func fakeVersion(features string) string {
	return "aria2 version 1.37.0\nCopyright (C) 2006, 2019 Tatsuhiro Tsujikawa\n\n** Configuration **\nEnabled Features: " +
		features + "\nHash Algorithms: sha-1, sha-256, md5\n\nReport bugs to https://github.com/aria2/aria2/issues\n"
}

// errUnrecognized is aria2c's refusal of an option it was built without.
type errUnrecognized struct{ opt string }

func (e *errUnrecognized) Error() string {
	return "aria2c: unrecognized option `" + e.opt + "'\nUsage: aria2c [OPTIONS] [URI | MAGNET | TORRENT_FILE | METALINK_FILE]..."
}

// btOptions and metalinkOptions exist only in a build with the feature.
var (
	btOptions       = []string{"follow-torrent", "enable-dht", "enable-dht6", "bt-enable-lpd", "enable-peer-exchange"}
	metalinkOptions = []string{"follow-metalink"}
)

// checkOptions refuses what this build does not know.
func (f *fakeAria) checkOptions(args []string) error {
	for _, a := range args {
		name, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if (f.noBitTorrent && slices.Contains(btOptions, name)) || (f.noMetalink && slices.Contains(metalinkOptions, name)) ||
			(f.noAsyncDNS && name == "async-dns") {
			return &errUnrecognized{opt: a}
		}
	}
	return nil
}

type fakeAdd struct {
	URI     string
	Dir     string
	Out     string
	Split   string
	MaxConn string
	UA      string // the user-agent option ("" = aria2c's own)
	Headers []string
	// Proxy holds the proxy options given (all-proxy, http-proxy,
	// https-proxy, ftp-proxy, no-proxy), when given. Like aria2c the fake
	// makes the download's requests, redirects included, through the proxy
	// of the URL's protocol (else all-proxy) unless no-proxy names the
	// host; without one it connects directly (proxyFor).
	Proxy map[string]string
	// global holds the same options of the command line: a download's own
	// option comes first, like aria2c's.
	global map[string]string
}

// proxyOptionNames are aria2c's proxy options.
var proxyOptionNames = []string{"all-proxy", "http-proxy", "https-proxy", "ftp-proxy", "no-proxy"}

// option is a proxy option of the download: its own, else the command
// line's.
func (a fakeAdd) option(name string) string {
	if v, ok := a.Proxy[name]; ok {
		return v
	}
	return a.global[name]
}

// proxyFor is the proxy of a request of the download, as aria2c picks it:
// the protocol's own option, else all-proxy; none for a host of no-proxy
// (a host or a domain suffix; the tests use no address ranges).
func (a fakeAdd) proxyFor(r *http.Request) (*url.URL, error) {
	v := a.option(r.URL.Scheme + "-proxy")
	if v == "" {
		v = a.option("all-proxy")
	}
	if v == "" {
		return nil, nil
	}
	host := strings.ToLower(r.URL.Hostname())
	for _, n := range strings.Split(a.option("no-proxy"), ",") {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && (host == n || strings.HasSuffix(host, "."+strings.TrimPrefix(n, "."))) {
			return nil, nil
		}
	}
	return url.Parse(v)
}

type fakeDL struct {
	gid       string
	add       fakeAdd
	status    string
	completed int64
	total     int64
	code      string
	msg       string
	followed  []string // followedBy: a Metalink answer (as aria2c would follow it)
	cancel    context.CancelFunc
	done      chan struct{}
}

// newFakeAria follows redirects like aria2c does: every header of addUri,
// Cookie included, goes unchanged to the next location, whatever its host
// (net/http alone would drop the cookie on another host and hide the
// leak that ruling 5 prevents).
func newFakeAria() *fakeAria {
	f := &fakeAria{dls: map[string]*fakeDL{}, hostsAt: map[string]string{}}
	f.hc = f.client(fakeAdd{})
	return f
}

// client is the HTTP client of a download: through its proxy when it was
// given one (fakeAdd.proxyFor), else directly, a test name at the address
// hostsAt gives it. Redirects are followed as aria2c follows them.
func (f *fakeAria) client(a fakeAdd) *http.Client {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil {
			f.mu.Lock()
			to, ok := f.hostsAt[host]
			f.mu.Unlock()
			if ok {
				addr = net.JoinHostPort(to, port)
			}
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	tr := &http.Transport{Proxy: nil, DialContext: dial}
	if len(a.Proxy) > 0 || len(a.global) > 0 {
		// (a transport of its own: its connections end with the download)
		tr = &http.Transport{Proxy: a.proxyFor, DialContext: dial, DisableKeepAlives: true}
	}
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		for k, v := range via[0].Header {
			req.Header[k] = v
		}
		return nil
	}}
}

// argValue returns the value of --name=value.
func argValue(args []string, name string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v
		}
	}
	return ""
}

// start serves RPC on the port of args with the secret of the conf file.
func (f *fakeAria) start(args []string) error {
	if err := f.checkOptions(args); err != nil {
		return err
	}
	conf := argValue(args, "conf-path")
	b, err := os.ReadFile(conf)
	if err != nil {
		return fmt.Errorf("fake aria2c: conf: %w", err)
	}
	secret := ""
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "rpc-secret="); ok {
			secret = v
		}
	}
	port := argValue(args, "rpc-listen-port")
	ln, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil && f.likeAria2c {
		// what aria2c 1.37.0 does when 127.0.0.1:port is taken: say so
		// (unless LF_FAKE_ARIA2C_QUIET) and listen on [::1] alone
		if os.Getenv("LF_FAKE_ARIA2C_QUIET") == "" {
			fmt.Printf("\n10/27 03:57:51 [ERROR] IPv4 RPC: failed to bind TCP port %s\nException: [SocketCore.cc:312] errorCode=1 Failed to bind a socket\n", port)
		}
		ln, err = net.Listen("tcp6", "[::1]:"+port)
	}
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.secret, f.conf, f.args, f.ln = secret, string(b), append([]string(nil), args...), ln
	f.exited = make(chan struct{})
	f.srv = &http.Server{Handler: http.HandlerFunc(f.serveRPC)}
	srv := f.srv
	f.mu.Unlock()
	go srv.Serve(ln)
	return nil
}

func (f *fakeAria) stop() {
	f.mu.Lock()
	srv, exited := f.srv, f.exited
	f.srv = nil
	for _, d := range f.dls {
		if d.cancel != nil {
			d.cancel()
		}
	}
	f.mu.Unlock()
	if srv == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(exited)
	}()
}

// fakeLauncher starts the fake in this process.
func (f *fakeAria) launcher() Launcher {
	return func(exe string, args []string, _ io.Writer) (Process, error) {
		if err := f.start(args); err != nil {
			return nil, err
		}
		f.mu.Lock()
		exited := f.exited
		f.mu.Unlock()
		return &fakeProc{f: f, exited: exited}, nil
	}
}

type fakeProc struct {
	f      *fakeAria
	exited chan struct{}
}

func (p *fakeProc) Wait() error { <-p.exited; return nil }
func (p *fakeProc) Kill() error { p.f.stop(); return nil }
func (p *fakeProc) Pid() int    { return os.Getpid() } // its listener is this process's

type rpcCall struct {
	ID     string            `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (f *fakeAria) serveRPC(w http.ResponseWriter, r *http.Request) {
	var c rpcCall
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	res, rerr := f.dispatch(c.Method, c.Params)
	out := map[string]any{"id": c.ID, "jsonrpc": "2.0"}
	if rerr != nil {
		out["error"] = rerr
	} else {
		out["result"] = res
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (f *fakeAria) dispatch(method string, params []json.RawMessage) (any, *rpcError) {
	if method == "system.multicall" {
		var calls []struct {
			MethodName string            `json:"methodName"`
			Params     []json.RawMessage `json:"params"`
		}
		if len(params) != 1 || json.Unmarshal(params[0], &calls) != nil {
			return nil, &rpcError{Code: 1, Message: "bad multicall"}
		}
		out := make([]any, len(calls))
		for i, c := range calls {
			res, err := f.dispatch(c.MethodName, c.Params)
			if err != nil {
				out[i] = map[string]any{"faultCode": err.Code, "faultString": err.Message}
			} else {
				out[i] = []any{res}
			}
		}
		return out, nil
	}
	var tok string
	if len(params) == 0 || json.Unmarshal(params[0], &tok) != nil {
		return nil, &rpcError{Code: 1, Message: "Unauthorized"}
	}
	f.mu.Lock()
	ok := tok == "token:"+f.secret && f.secret != ""
	f.mu.Unlock()
	if !ok {
		return nil, &rpcError{Code: 1, Message: "Unauthorized"}
	}
	params = params[1:]
	str := func(i int) string {
		var s string
		if i < len(params) {
			json.Unmarshal(params[i], &s)
		}
		return s
	}
	switch method {
	case "aria2.getVersion":
		return map[string]any{"version": "1.37.0-fake", "enabledFeatures": []string{"HTTPS"}}, nil
	case "aria2.addUri":
		var uris []string
		var opts map[string]any
		if len(params) < 2 || json.Unmarshal(params[0], &uris) != nil || json.Unmarshal(params[1], &opts) != nil || len(uris) != 1 {
			return nil, &rpcError{Code: 1, Message: "bad addUri"}
		}
		a := fakeAdd{URI: uris[0]}
		a.Dir, _ = opts["dir"].(string)
		a.Out, _ = opts["out"].(string)
		a.Split, _ = opts["split"].(string)
		a.MaxConn, _ = opts["max-connection-per-server"].(string)
		a.UA, _ = opts["user-agent"].(string)
		if hs, ok := opts["header"].([]any); ok {
			for _, h := range hs {
				s, _ := h.(string)
				a.Headers = append(a.Headers, s)
			}
		}
		f.mu.Lock()
		args := f.args
		f.mu.Unlock()
		for _, name := range proxyOptionNames {
			if v, ok := opts[name].(string); ok {
				if a.Proxy == nil {
					a.Proxy = map[string]string{}
				}
				a.Proxy[name] = v
			}
			if v := argValue(args, name); v != "" {
				if a.global == nil {
					a.global = map[string]string{}
				}
				a.global[name] = v
			}
		}
		if !filepath.IsAbs(a.Dir) || a.Out == "" {
			return nil, &rpcError{Code: 1, Message: "dir must be absolute and out set"}
		}
		return f.add(a), nil
	case "aria2.tellStatus":
		f.mu.Lock()
		defer f.mu.Unlock()
		d := f.dls[str(0)]
		if d == nil {
			return nil, &rpcError{Code: 1, Message: "GID is not found"}
		}
		st := map[string]any{"gid": d.gid, "status": d.status, "totalLength": strconv.FormatInt(d.total, 10),
			"completedLength": strconv.FormatInt(d.completed, 10), "downloadSpeed": "1000",
			"errorCode": d.code, "errorMessage": d.msg}
		if len(d.followed) > 0 {
			st["followedBy"] = d.followed
		}
		return st, nil
	case "aria2.forceRemove", "aria2.remove":
		f.mu.Lock()
		d := f.dls[str(0)]
		f.removed = append(f.removed, str(0))
		f.mu.Unlock()
		if d == nil {
			return nil, &rpcError{Code: 1, Message: "GID is not found"}
		}
		if d.cancel != nil {
			d.cancel()
		}
		<-d.done
		return d.gid, nil
	case "aria2.removeDownloadResult":
		f.mu.Lock()
		defer f.mu.Unlock()
		d := f.dls[str(0)]
		if d == nil || d.status == "active" {
			return nil, &rpcError{Code: 1, Message: "Could not remove"}
		}
		delete(f.dls, d.gid)
		return "OK", nil
	case "aria2.shutdown", "aria2.forceShutdown":
		f.mu.Lock()
		f.shutdowns++
		slow := f.slowExit
		f.mu.Unlock()
		if !slow {
			go f.stop()
		}
		return "OK", nil
	}
	return nil, &rpcError{Code: 1, Message: "No such method: " + method}
}

func (f *fakeAria) add(a fakeAdd) string {
	ctx, cancel := context.WithCancel(context.Background())
	f.mu.Lock()
	f.seq++
	d := &fakeDL{gid: fmt.Sprintf("%016x", f.seq), add: a, status: "active", total: 0, cancel: cancel, done: make(chan struct{})}
	f.dls[d.gid] = d
	f.adds = append(f.adds, a)
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	go f.download(ctx, d)
	return d.gid
}

func (f *fakeAria) finish(d *fakeDL, status, code, msg string) {
	f.mu.Lock()
	d.status, d.code, d.msg = status, code, msg
	f.active--
	f.mu.Unlock()
	close(d.done)
}

func (f *fakeAria) download(ctx context.Context, d *fakeDL) {
	path := filepath.Join(d.add.Dir, d.add.Out)
	control := path + ".aria2"
	os.WriteFile(control, []byte("fake control"), 0o644)
	var have int64
	if fi, err := os.Stat(path); err == nil {
		have = fi.Size()
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.add.URI, nil)
	req.Header.Set("User-Agent", "aria2/1.37.0-fake")
	if d.add.UA != "" {
		req.Header.Set("User-Agent", d.add.UA)
	}
	for _, h := range d.add.Headers {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	hc := f.hc
	if len(d.add.Proxy) > 0 || len(d.add.global) > 0 {
		hc = f.client(d.add)
	}
	res, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			f.finish(d, "removed", "", "")
			return
		}
		f.finish(d, "error", "6", "Network problem has occurred. cause: "+err.Error())
		return
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == 401:
		f.finish(d, "error", "24", "Authorization failed.")
		return
	case res.StatusCode == 404:
		f.finish(d, "error", "3", "Resource not found")
		return
	case res.StatusCode >= 400:
		f.finish(d, "error", "22", fmt.Sprintf("The response status is not successful. status=%d URI=%s", res.StatusCode, d.add.URI))
		return
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if res.StatusCode == http.StatusPartialContent {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	} else {
		have = 0
	}
	out, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		f.finish(d, "error", "16", err.Error())
		return
	}
	f.mu.Lock()
	d.completed = have
	if res.ContentLength >= 0 {
		d.total = have + res.ContentLength
	}
	f.mu.Unlock()
	buf := make([]byte, 16<<10)
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			f.mu.Lock()
			d.completed += int64(n)
			f.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			if ctx.Err() != nil {
				f.finish(d, "removed", "", "")
			} else {
				f.finish(d, "error", "6", "Network problem has occurred.")
			}
			return
		}
	}
	out.Close()
	os.Remove(control)
	if strings.HasSuffix(res.Request.URL.Path, ".meta4") {
		// what aria2c does with --follow-metalink=true: the download is
		// complete and another one fetches what the Metalink lists
		f.mu.Lock()
		d.followed = []string{"00000000000000ff"}
		f.mu.Unlock()
	}
	f.finish(d, "complete", "", "")
}

// snapshot copies what the tests inspect.
func (f *fakeAria) snapshot() (adds []fakeAdd, args []string, conf string, maxActive, shutdowns int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeAdd(nil), f.adds...), append([]string(nil), f.args...), f.conf, f.maxActive, f.shutdowns
}

// completed is the most bytes any download of a URL with that path got.
func (f *fakeAria) completed(path string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, d := range f.dls {
		if strings.Contains(d.add.URI, path+"?") && d.completed > n {
			n = d.completed
		}
	}
	return n
}

func (f *fakeAria) running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.srv != nil
}

// runFakeAriaProcess is the separate-process mode (TestMain): serve until
// shut down, then exit.
// The build it plays: LF_FAKE_ARIA2C_FEATURES ("Enabled Features:"; the
// Windows build's by default).
func runFakeAriaProcess() int {
	features := os.Getenv("LF_FAKE_ARIA2C_FEATURES")
	if features == "" {
		features = featuresWindows
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Print(fakeVersion(features))
		return 0
	}
	f := newFakeAria()
	f.likeAria2c = true
	have := parseFeatures(fakeVersion(features))
	f.noBitTorrent, f.noMetalink = !have.bitTorrent, !have.metalink
	f.noAsyncDNS = !hasFeature(fakeVersion(features), "Async DNS")
	if err := f.start(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var ue *errUnrecognized
		if errors.As(err, &ue) {
			return 28 // aria2c's exit status for a bad option
		}
		return 1
	}
	f.mu.Lock()
	exited := f.exited
	f.mu.Unlock()
	<-exited
	return 0
}

var errFakeLaunch = errors.New("fake launch refused")
