// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"lectureflow/internal/blocklist"
	"lectureflow/internal/model"
)

// downloader fetches content-addressed files into dir: dir/{sha256}.part
// while downloading (resumed with an HTTP Range request), dir/{sha256} once
// the SHA-256 matched.
type downloader struct {
	client  *http.Client
	dir     string
	backoff time.Duration // first delay before retrying a broken transfer
	stall   time.Duration // no bytes for this long = the transfer broke
	log     *slog.Logger
}

// fileSpec is a file to download.
type fileSpec struct {
	Name   string
	SHA256 string
	Size   int64
	URLs   []sourceURL // tried in order (chosen mirror, then default)
}

// maxNoProgress: consecutive broken transfers without any new byte before a
// download fails (network) and the next source is tried.
const maxNoProgress = 3

// errRestart: the server's answer does not continue the partial file; it
// was truncated and the transfer starts again.
var errRestart = errors.New("the server did not resume the partial download")

type httpStatusError struct{ status int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP status %d", e.status) }

func (d *downloader) finalPath(sha string) string { return filepath.Join(d.dir, sha) }
func (d *downloader) partPath(sha string) string  { return filepath.Join(d.dir, sha+".part") }

// have reports whether the verified file exists.
func (d *downloader) have(f fileSpec) bool {
	fi, err := os.Stat(d.finalPath(f.SHA256))
	return err == nil && fi.Mode().IsRegular() && fi.Size() == f.Size
}

// haveVerified reports whether the downloaded file exists with the right
// size and, recomputed now, the right SHA-256 (security audit L-13: the
// name and the size alone do not show that nobody changed it). A file
// that does not match is deleted. Called only when a file is about to be
// used (install, update, resume), never for plans or at start-up: hashing
// reads the whole file (a few seconds per GB). Cancelling ctx stops it.
func (d *downloader) haveVerified(ctx context.Context, f fileSpec) (bool, error) {
	if !d.have(f) {
		return false, nil
	}
	ok, err := fileMatches(ctx, d.finalPath(f.SHA256), f.Size, f.SHA256)
	if err != nil {
		return false, err
	}
	if !ok {
		d.log.Warn("components: a downloaded file no longer matches its SHA-256; downloading it again", "file", f.Name)
		os.Remove(d.finalPath(f.SHA256))
	}
	return ok, nil
}

// fileMatches hashes the file at p (streaming, stopping when ctx ends) and
// compares its size and SHA-256. A file that cannot be read does not match;
// only the context's end is an error.
func fileMatches(ctx context.Context, p string, size int64, sha string) (bool, error) {
	fh, err := os.Open(p)
	if err != nil {
		return false, nil
	}
	defer fh.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, &ctxReader{ctx: ctx, r: fh}, make([]byte, 1<<20))
	if err != nil {
		if c := context.Cause(ctx); c != nil {
			return false, c
		}
		return false, nil
	}
	return n == size && strings.EqualFold(hex.EncodeToString(h.Sum(nil)), sha), nil
}

// partial returns the bytes of a partial download.
func (d *downloader) partial(f fileSpec) int64 {
	if fi, err := os.Stat(d.partPath(f.SHA256)); err == nil && fi.Size() <= f.Size {
		return fi.Size()
	}
	return 0
}

// remove deletes the file and its partial download.
func (d *downloader) remove(sha string) {
	os.Remove(d.finalPath(sha))
	os.Remove(d.partPath(sha))
}

// fetch makes dir/{sha256} exist with the right content and returns its
// path. onBytes receives the bytes of this file present so far. A failing
// mirror source falls back to the next URL (fallback is called with the
// reason); disk-full and cancellation stop at once.
func (d *downloader) fetch(ctx context.Context, f fileSpec, onBytes func(int64),
	fallback func(u sourceURL, reason model.ErrorInfo)) (string, error) {
	final := d.finalPath(f.SHA256)
	ok, err := d.haveVerified(ctx, f)
	if err != nil {
		return "", err
	}
	if ok {
		onBytes(f.Size)
		return final, nil
	}
	os.Remove(final)
	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		return "", asCoded(err, nil)
	}
	var lastErr error
	for i, u := range f.URLs {
		err := d.fetchFrom(ctx, u.URL, f, onBytes)
		if err == nil {
			return final, nil
		}
		if c := context.Cause(ctx); c != nil {
			return "", c
		}
		lastErr = asCoded(err, map[string]any{"file": f.Name})
		if CodeOf(lastErr) == CodeDiskFull || i == len(f.URLs)-1 {
			break
		}
		d.log.Warn("components: download source failed; trying the next one",
			"file", f.Name, "url", redactURL(u.URL), "error", redactText(err.Error()))
		if fallback != nil {
			fallback(u, ErrorInfoOf(lastErr))
		}
	}
	if lastErr == nil {
		lastErr = cerr(CodeNetwork, map[string]any{"file": f.Name}, errors.New("no download URL"))
	}
	return "", lastErr
}

// fetchFrom downloads from one URL and verifies the SHA-256; on a mismatch
// the file is deleted and downloaded once more (V3.3).
func (d *downloader) fetchFrom(ctx context.Context, url string, f fileSpec, onBytes func(int64)) error {
	part := d.partPath(f.SHA256)
	var got string
	for attempt := 0; attempt < 2; attempt++ {
		h, err := d.download(ctx, url, part, f.Size, onBytes)
		if err != nil {
			return err
		}
		got = hex.EncodeToString(h.Sum(nil))
		if got == f.SHA256 {
			if err := renameRetry(part, d.finalPath(f.SHA256)); err != nil {
				return cerr(CodeIO, nil, err)
			}
			return nil
		}
		d.log.Warn("components: SHA-256 mismatch; deleting the file", "file", f.Name, "url", redactURL(url),
			"got", got, "want", f.SHA256, "attempt", attempt+1)
		os.Remove(part)
		onBytes(0)
	}
	return cerr(CodeChecksum, map[string]any{"file": f.Name},
		fmt.Errorf("%s: SHA-256 %s, expected %s", f.Name, got, f.SHA256))
}

// download completes part to size bytes (resuming what is there) and returns
// the SHA-256 state of its content.
func (d *downloader) download(ctx context.Context, url, part string, size int64, onBytes func(int64)) (hash.Hash, error) {
	h := sha256.New()
	n, err := hashExisting(part, size, h)
	if err != nil {
		return nil, err
	}
	onBytes(n)
	noProgress := 0
	for {
		if n == size {
			return h, nil
		}
		before := n
		n, err = d.once(ctx, url, part, n, size, h, onBytes)
		if err == nil {
			continue
		}
		if c := context.Cause(ctx); c != nil {
			return nil, c
		}
		if !transient(err) {
			return nil, err
		}
		if n > before {
			noProgress = 0
		} else {
			noProgress++
		}
		if noProgress >= maxNoProgress {
			return nil, err
		}
		d.log.Info("components: download interrupted; resuming", "url", redactURL(url), "at", n, "error", redactText(err.Error()))
		delay := d.backoff << uint(noProgress)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// hashExisting hashes a partial file (truncating one longer than size) and
// returns its length.
func hashExisting(part string, size int64, h hash.Hash) (int64, error) {
	f, err := os.Open(part)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, asCoded(err, nil)
	}
	n, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		return 0, asCoded(err, nil)
	}
	if n > size {
		h.Reset()
		if err := os.Truncate(part, 0); err != nil {
			return 0, asCoded(err, nil)
		}
		return 0, nil
	}
	return n, nil
}

func transient(err error) bool {
	var rr *redirectRefusedError
	if errors.As(err, &rr) {
		return false // the same answer again: the next source is tried at once
	}
	var hs *httpStatusError
	if errors.As(err, &hs) {
		return hs.status >= 500 || hs.status == http.StatusTooManyRequests || hs.status == http.StatusRequestTimeout
	}
	if errors.Is(err, errRestart) {
		return true
	}
	return classify(err) == CodeNetwork
}

// once performs one request from offset n and returns the new length.
func (d *downloader) once(ctx context.Context, url, part string, n, size int64, h hash.Hash, onBytes func(int64)) (int64, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(withRedirectChain(rctx), http.MethodGet, url, nil)
	if err != nil {
		return n, cerr(CodeNetwork, nil, err)
	}
	req.Header.Set("User-Agent", "LectureFlow-components")
	req.Header.Set("Accept-Encoding", "identity")
	if n > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", n))
	}
	stall := time.AfterFunc(d.stall, func() { cancel(errStalled) })
	defer stall.Stop()
	resp, err := d.client.Do(req)
	if err != nil {
		if context.Cause(rctx) == errStalled && ctx.Err() == nil {
			return n, errStalled
		}
		return n, err
	}
	defer resp.Body.Close()

	restart := func() (int64, error) {
		h.Reset()
		if err := os.Truncate(part, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return n, asCoded(err, nil)
		}
		onBytes(0)
		return 0, nil
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		if n > 0 { // the server ignored Range: start over
			if n, err = restart(); err != nil {
				return n, err
			}
		}
	case resp.StatusCode == http.StatusPartialContent:
		start, ok := contentRangeStart(resp.Header.Get("Content-Range"))
		if !ok || start != n {
			if _, err := restart(); err != nil {
				return n, err
			}
			return 0, errRestart
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		if _, err := restart(); err != nil {
			return n, err
		}
		return 0, errRestart
	default:
		return n, cerr(CodeHTTPStatus, map[string]any{"status": resp.StatusCode}, &httpStatusError{resp.StatusCode})
	}

	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return n, asCoded(err, nil)
	}
	buf := make([]byte, 256<<10)
	var werr error
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			stall.Reset(d.stall)
			if n+int64(k) > size {
				werr = cerr(CodeChecksum, nil, fmt.Errorf("the server sent more than the expected %d bytes", size))
				out.Close()
				h.Reset()
				os.Truncate(part, 0)
				onBytes(0)
				return 0, werr
			}
			if _, err := out.Write(buf[:k]); err != nil {
				out.Close()
				os.Truncate(part, n) // keep the file equal to what was hashed
				return n, asCoded(err, nil)
			}
			h.Write(buf[:k])
			n += int64(k)
			onBytes(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			werr = rerr
			if context.Cause(rctx) == errStalled && ctx.Err() == nil {
				werr = errStalled
			}
			break
		}
	}
	if err := out.Sync(); err != nil && werr == nil {
		werr = asCoded(err, nil)
	}
	if err := out.Close(); err != nil && werr == nil {
		werr = asCoded(err, nil)
	}
	if werr == nil && n < size {
		werr = io.ErrUnexpectedEOF // the body ended early: resume
	}
	return n, werr
}

// errStalled: no bytes arrived for the stall timeout.
var errStalled = &net.OpError{Op: "read", Err: errors.New("download stalled")}

func contentRangeStart(v string) (int64, bool) {
	// "bytes 100-199/200"
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, false
	}
	v = strings.TrimPrefix(v, "bytes ")
	dash := strings.IndexByte(v, '-')
	if dash <= 0 {
		return 0, false
	}
	s, err := strconv.ParseInt(v[:dash], 10, 64)
	return s, err == nil
}

// sha256File hashes a file.
func sha256File(path string, onBytes func(int64)) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		k, err := f.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			n += int64(k)
			if onBytes != nil {
				onBytes(n)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", n, err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// defaultHTTPClient is the client of the manifest check, the lock files and
// the downloads (Options.HTTPClient nil): connection and header timeouts,
// and the redirect rule below.
func defaultHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	tr.DisableCompression = true
	g := newRedirectGuard(tr, nil, nil)
	return &http.Client{Transport: g, CheckRedirect: g.checkRedirect}
}

// Redirects (security audit AUD4 L-3). A download source does not have to
// be trusted for what it serves (every file is checked against the
// manifest's SHA-256), but a mirror that someone controls could answer
// with a redirect to this computer or the local network and so make
// LectureFlow send GET requests there (blind SSRF). net/http follows any
// redirect by default: ten hops, any scheme, any host. The rule here:
//
//   - the first request of a chain goes where the manifest or the user's
//     custom source says, as before: a mirror on this computer (a local
//     package cache, http allowed: InsecureCustomSource) or on the local
//     network (a school's mirror, by address or by a name that resolves
//     there) keeps working;
//   - a redirect is followed at most maxRedirects times, only to https,
//     except within the first request's own origin (scheme, host and
//     port: a local http mirror that redirects to itself), so never from
//     https down to http;
//   - a redirect may not lead closer to this computer than the first
//     request: from a public source only to public addresses, from one on
//     the local network to the local network or public ones, from one on
//     this computer anywhere. Judged by the host as written and by every
//     address it resolves to, and again by the address each connection of
//     a redirect actually dials (a name that answers differently the
//     second time is refused all the same). Unspecified, multicast,
//     broadcast and reserved (240.0.0.0/4) addresses, the numbers
//     resolvers read as an address ("2130706433", "0x7f.1") and host names
//     with unusual characters are never a redirect's target;
//   - how close the first request is: its host as written (an address,
//     localhost, *.local), else the address its connection actually went
//     to (SEC47 low-1). Its name is not looked up again when a redirect
//     comes: a mirror whose DNS someone controls could answer [public,
//     127.0.0.1], or 127.0.0.1 the second time, and the chain would start
//     "on this computer". A school's mirror whose name resolves to its
//     network is connected to there, so its chain starts on the local
//     network as before. The request carries the chain's state in its
//     context (withRedirectChain: the downloads, the lock files, the
//     manifest check; a redirect inherits its first request's context);
//     a request without it, or whose connection's address is unknown,
//     starts from a public source;
//   - with a proxy of the user's (HTTPS_PROXY and the like) the proxy
//     resolves the names, so only the host as written is judged, and the
//     connection to the proxy itself (often on this computer) is not.
//
// The connections of redirects use transports of their own, one per rule,
// so a connection made under one rule is never reused under another.
// internal/fetch has the same rule for downloads of recordings; its
// classification is not exported, so the address classes are written out
// again here (fetch.classifyIP is the reference). uv's own requests (the
// package index) are not covered: uv follows redirects itself.

// maxRedirects bounds a redirect chain: Hugging Face and its mirror send
// one or two (to their CDN), GitHub's release downloads and npmmirror one.
const maxRedirects = 5

// reach is how close to this computer an address is.
type reach int

const (
	reachPublic   reach = iota
	reachLocalNet       // private, link-local, site-local, carrier-grade NAT, *.local
	reachLoopback       // this computer: loopback addresses, localhost
	reachNever          // never a redirect's target (see the rule above)
)

// ipReach tells where ip is: loopback (127.0.0.0/8, ::1); never (0.0.0.0/8,
// ::, multicast, 240.0.0.0/4 with the limited broadcast, an IPv4-compatible
// ::a.b.c.d); the local network (RFC 1918, fc00::/7, link-local
// 169.254.0.0/16 and fe80::/10, site-local fec0::/10, carrier-grade NAT
// 100.64.0.0/10); an IPv6 address carrying an IPv4 one (NAT64, IPv4-
// translated, 6to4, Teredo) is where that IPv4 address is; everything else
// is public. 198.18.0.0/15 stays public: proxy clients answer every name
// with such an address in their "fake IP" mode.
func ipReach(ip netip.Addr) reach {
	ip = ip.Unmap().WithZone("")
	switch {
	case !ip.IsValid():
		return reachNever
	case ip.IsLoopback():
		return reachLoopback
	case ip.IsUnspecified(), ip.IsMulticast():
		return reachNever
	case ip.Is4() && (ip.As4()[0] == 0 || ip.As4()[0] >= 240):
		return reachNever
	case ip.IsPrivate(), ip.IsLinkLocalUnicast():
		return reachLocalNet
	case ip.Is6() && ip.As16()[0] == 0xfe && ip.As16()[1]&0xc0 == 0xc0:
		return reachLocalNet
	case ip.Is4() && ip.As4()[0] == 100 && ip.As4()[1]&0xc0 == 64:
		return reachLocalNet
	}
	inside, compatible := embeddedIPv4(ip)
	if compatible {
		return reachNever
	}
	r := reachPublic
	for _, v4 := range inside {
		r = max(r, ipReach(v4))
	}
	return r
}

// embeddedIPv4 are the IPv4 addresses an IPv6 address carries by a
// transition mechanism, where a translator or relay connects (the same
// ranges as internal/fetch ipv4Inside); compatible: ip is an
// IPv4-compatible address (::a.b.c.d, deprecated, routed by nothing).
func embeddedIPv4(ip netip.Addr) (inside []netip.Addr, compatible bool) {
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
	nat64 := b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b
	switch {
	case nat64 && zero(4, 12): // 64:ff9b::/96
		return []netip.Addr{v4(12)}, false
	case nat64 && b[4] == 0x00 && b[5] == 0x01: // 64:ff9b:1::/48, local use
		return []netip.Addr{v4(12)}, false
	case zero(0, 8) && b[8] == 0xff && b[9] == 0xff && b[10] == 0 && b[11] == 0: // ::ffff:0:0/96, IPv4-translated
		return []netip.Addr{v4(12)}, false
	case zero(0, 12): // ::/96, IPv4-compatible (:: and ::1 are judged before)
		return []netip.Addr{v4(12)}, true
	case b[0] == 0x20 && b[1] == 0x02: // 2002::/16, 6to4
		return []netip.Addr{v4(2)}, false
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0 && b[3] == 0: // 2001::/32, Teredo: the server, the client (inverted)
		return []netip.Addr{v4(4), netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]})}, false
	}
	return nil, false
}

// hostReach tells where a URL's Hostname() is as written; final is false
// for a name whose addresses decide (reachPublic until they are known).
func hostReach(host string) (r reach, final bool) {
	if host == "" || blocklist.UnusualHost(host) {
		return reachNever, true
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(h); err == nil {
		return ipReach(ip), true
	}
	switch {
	case h == "localhost" || strings.HasSuffix(h, ".localhost"):
		return reachLoopback, true
	case lastLabelIsNumber(h):
		// "2130706433", "0x7f.1", "127.1": an address to most resolvers
		// (Windows' among them), not a name
		return reachNever, true
	case h == "local" || strings.HasSuffix(h, ".local"):
		return reachLocalNet, true
	}
	return reachPublic, false
}

// lastLabelIsNumber is the URL standard's test for a host that is an IPv4
// address: its last label is a decimal or 0x hexadecimal number.
func lastLabelIsNumber(h string) bool {
	last := h[strings.LastIndexByte(h, '.')+1:]
	if x, ok := strings.CutPrefix(last, "0x"); ok {
		return strings.Trim(x, "0123456789abcdef") == ""
	}
	return last != "" && strings.Trim(last, "0123456789") == ""
}

// redirectRefusedError is a redirect the downloads do not follow (or a
// connection of one to an address it may not reach). Not transient: the
// next source is tried at once (components.redirect_refused {host}).
type redirectRefusedError struct {
	host, reason string
}

func (e *redirectRefusedError) Error() string {
	return "refused to follow a redirect to " + printableHost(e.host) + ": " + e.reason
}

// printableHost is host for a message: printable ASCII as it is, every
// other byte as "?", at most 255 bytes.
func printableHost(h string) string {
	if len(h) > 255 {
		h = h[:255]
	}
	b := []byte(h)
	for i, c := range b {
		if c < 0x21 || c >= 0x7f {
			b[i] = '?'
		}
	}
	return string(b)
}

// redirectGuard is the RoundTripper (and CheckRedirect) of the downloads'
// client: the redirect rule above.
type redirectGuard struct {
	base   *http.Transport // the template: timeouts, TLS, the proxy
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// dial connects to addr ("host:port"); check, when not nil, judges the
	// address actually dialled before connecting (tests replace it: their
	// servers are on 127.0.0.1 whatever their names resolve to).
	dial func(ctx context.Context, network, addr string, check func(netip.Addr) error) (net.Conn, error)

	start *http.Transport           // the first request of a chain
	hops  map[reach]*http.Transport // redirects, by the reach of the chain's first request
}

// newRedirectGuard builds the guard on base (cloned, not changed); lookup
// and dial nil = the system's resolver and a dialer like net/http's.
func newRedirectGuard(base *http.Transport,
	lookup func(ctx context.Context, host string) ([]netip.Addr, error),
	dial func(ctx context.Context, network, addr string, check func(netip.Addr) error) (net.Conn, error)) *redirectGuard {
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if dial == nil {
		dial = dialChecked
	}
	g := &redirectGuard{base: base, lookup: lookup, dial: dial, hops: map[reach]*http.Transport{}}
	g.start = base.Clone()
	g.start.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return g.dial(ctx, network, addr, nil)
	}
	for _, r := range []reach{reachPublic, reachLocalNet, reachLoopback} {
		g.hops[r] = g.hopTransport(r)
	}
	return g
}

// dialChecked is the production dial: net/http's dialer settings; check
// judges the address each connection attempt is about to use
// (net.Dialer.Control, after the name was resolved).
func dialChecked(ctx context.Context, network, addr string, check func(netip.Addr) error) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if check != nil {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return &redirectRefusedError{host: host, reason: "not an IP address"}
			}
			return check(ip)
		}
	}
	return d.DialContext(ctx, network, addr)
}

// hopTransport is the transport of redirects whose chain started at
// allowed: every connection's address must be at most that close, but for
// those to the user's proxy.
func (g *redirectGuard) hopTransport(allowed reach) *http.Transport {
	tr := g.base.Clone()
	var proxies sync.Map // the "host:port" net/http dials for the proxies it was given
	if proxy := tr.Proxy; proxy != nil {
		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			u, err := proxy(req)
			if u != nil {
				proxies.Store(proxyDialAddr(u), true)
			}
			return u, err
		}
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, ok := proxies.Load(addr); ok {
			return g.dial(ctx, network, addr, nil)
		}
		name, _, _ := net.SplitHostPort(addr)
		return g.dial(ctx, network, addr, func(ip netip.Addr) error {
			if ipReach(ip) > allowed {
				return &redirectRefusedError{host: name, reason: "it resolves to " + ip.String() + ", closer to this computer than the download source"}
			}
			return nil
		})
	}
	return tr
}

// proxyDialAddr is the address net/http dials for a proxy URL (its
// canonicalAddr for an ASCII host).
func proxyDialAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[strings.ToLower(u.Scheme)]
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// chainKey is the context key of a redirect chain's state.
type chainKey struct{}

// redirectChain is what the guard learns of a chain's first request: how
// close to this computer the addresses its connections went to are. The
// start transport writes it (httptrace GotConn, also for a connection
// reused from the pool), checkRedirect and the redirects' RoundTrip read
// it.
type redirectChain struct {
	mu    sync.Mutex
	seen  bool
	reach reach // the farthest from this computer of the connections seen
}

// withRedirectChain is ctx with a new chain state: the context of every
// request the downloads send (downloader.once, httpGetLimited,
// CheckManifest). Each request gets its own; a redirect inherits its
// first request's context, so the state comes with it.
func withRedirectChain(ctx context.Context) context.Context {
	return context.WithValue(ctx, chainKey{}, &redirectChain{})
}

func chainOf(ctx context.Context) *redirectChain {
	c, _ := ctx.Value(chainKey{}).(*redirectChain)
	return c
}

// connected notes a connection of the first request to peer: its reach,
// public when the address cannot be read or is no single server's. Of
// several connections (net/http retries a request whose reused connection
// was closed) the farthest from this computer counts.
func (c *redirectChain) connected(peer net.Addr) {
	r := reachPublic
	if peer != nil {
		if ap, err := netip.ParseAddrPort(peer.String()); err == nil {
			if r = ipReach(ap.Addr()); r == reachNever {
				r = reachPublic
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.seen || r < c.reach {
		c.reach = r
	}
	c.seen = true
}

// get is the reach of the first request's connections; false before any.
func (c *redirectChain) get() (reach, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reach, c.seen
}

// RoundTrip sends the first request of a chain as before (noting where its
// connection went when the request carries a chain's state and goes
// directly, not through the user's proxy, whose address would be noted)
// and a redirect (net/http sets its Response) through the transport of its
// chain's rule.
func (g *redirectGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Response == nil {
		if c := chainOf(req.Context()); c != nil && !g.proxied(req.URL) {
			trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
				if info.Conn != nil {
					c.connected(info.Conn.RemoteAddr())
				}
			}}
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
		}
		return g.start.RoundTrip(req)
	}
	first := req
	for i := 0; i <= maxRedirects+1 && first.Response != nil && first.Response.Request != nil; i++ {
		first = first.Response.Request
	}
	return g.hops[g.startReach(req.Context(), first.URL)].RoundTrip(req)
}

// CloseIdleConnections closes the idle connections of every transport.
func (g *redirectGuard) CloseIdleConnections() {
	g.start.CloseIdleConnections()
	for _, tr := range g.hops {
		tr.CloseIdleConnections()
	}
}

// proxied: a request for u goes through a proxy of the user's.
func (g *redirectGuard) proxied(u *url.URL) bool {
	if g.base.Proxy == nil {
		return false
	}
	p, _ := g.base.Proxy(&http.Request{URL: u})
	return p != nil
}

// startReach is how close the first request u of a chain is: its host as
// written, else where its connection went (the chain's state in ctx: a
// school's mirror whose name resolves to its network is connected to
// there); public when it went through a proxy, when ctx has no chain's
// state or no connection was noted. The name is never looked up again
// here (SEC47 low-1).
func (g *redirectGuard) startReach(ctx context.Context, u *url.URL) reach {
	r, final := hostReach(u.Hostname())
	if !final {
		r = reachPublic
		if c := chainOf(ctx); c != nil && !g.proxied(u) {
			if x, ok := c.get(); ok {
				r = x
			}
		}
	}
	if r == reachNever {
		return reachPublic
	}
	return r
}

// checkRedirect is the client's CheckRedirect: the rule above for the host
// as written and its addresses (the connection's address is judged again
// when it is dialled: hopTransport).
func (g *redirectGuard) checkRedirect(req *http.Request, via []*http.Request) error {
	hop := req.URL
	host := hop.Hostname()
	if len(via) > maxRedirects {
		return &redirectRefusedError{host: host, reason: fmt.Sprintf("more than %d redirects", maxRedirects)}
	}
	first := via[0].URL
	if !strings.EqualFold(hop.Scheme, "https") && !sameOrigin(first, hop) {
		return &redirectRefusedError{host: host, reason: "not an https address"}
	}
	allowed := g.startReach(req.Context(), first)
	r, final := hostReach(host)
	if r > allowed {
		return &redirectRefusedError{host: host, reason: "an address closer to this computer than the download source, or not one LectureFlow follows"}
	}
	if final || g.proxied(hop) {
		return nil
	}
	ips, err := g.lookup(req.Context(), host)
	if err != nil {
		return nil // left to the connection, whose address is judged (hopTransport)
	}
	for _, ip := range ips {
		if ipReach(ip) > allowed {
			return &redirectRefusedError{host: host, reason: "it resolves to " + ip.String() + ", closer to this computer than the download source"}
		}
	}
	return nil
}

// sameOrigin: a and b have the same scheme, host and port (the scheme's
// default port when none is written).
func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		return map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
