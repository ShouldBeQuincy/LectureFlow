// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"lectureflow/internal/db"
)

// Redirects and cookies (lead's ruling 5). aria2c sends the headers of
// addUri, Cookie included, unchanged to wherever a redirect leads, so a
// media URL that redirects to another host would hand that host the
// session. For a task with cookies Go therefore follows the redirects
// first (HEAD; GET of byte 0 when HEAD is refused) with the cookies a
// browser would send to each hop, and aria2c gets the final URL with the
// cookies of the final host only. A task without cookies (Panopto, M0)
// skips this: aria2c only has the Referer to carry, and that is the page's
// origin alone for another origin than the page's (refererFor); a URL on
// the page's own origin, whose Referer is the whole page URL, is resolved
// by Go first all the same (resolver.needed).

// maxRedirects bounds a redirect chain (net/http's own default is 10).
const maxRedirects = 10

// client returns the run's HTTP client (taskNet) with the redirect policy
// of a task: a hop the task may not reach is refused (local.go; the
// transport also checks the address dialled); on every hop the Cookie
// header is rebuilt
// for the new URL from the task's cookies (none for another host, none
// over plain HTTP for a Secure cookie) and the Referer is the page's again,
// as much of it as the new URL may see (refererFor; net/http would put the
// previous URL there, signed query and all).
func (q *Queue) client(s runSnapshot) *http.Client {
	n := q.netOf(s)
	c := *n.hc
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("stopped after 10 redirects")
		}
		if err := n.addr.checkURL(req.Context(), req.URL.String(), false); err != nil {
			return err
		}
		req.Header.Del("Cookie")
		req.Header.Del("Referer")
		for _, h := range requestHeaders(req.URL.String(), s.cookies, s.referer, q.o.Now()) {
			k, v, _ := strings.Cut(h, ": ")
			req.Header.Set(k, v)
		}
		return nil
	}
	return &c
}

// probe makes one request (HEAD, or GET of byte 0) for rawURL with the
// task's headers and returns the URL it ended at and the final status.
func (q *Queue) probe(ctx context.Context, method, rawURL string, s runSnapshot) (string, int, error) {
	if err := q.netOf(s).addr.checkURL(ctx, rawURL, false); err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	for _, h := range requestHeaders(rawURL, s.cookies, s.referer, q.o.Now()) {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-0")
	}
	res, err := q.client(s).Do(req)
	if err != nil {
		return "", 0, err
	}
	res.Body.Close() // a GET answered with the whole file stops here
	return res.Request.URL.String(), res.StatusCode, nil
}

// resolveFinal returns where rawURL leads after its redirects. HEAD first;
// any failure of HEAD (405, a 403 of a URL signed for GET only, a network
// error) is tried again as GET of byte 0, whose answer decides.
func (q *Queue) resolveFinal(ctx context.Context, rawURL string, s runSnapshot) (string, error) {
	final, status, err := q.probe(ctx, http.MethodHead, rawURL, s)
	if err == nil && status >= 200 && status < 300 {
		return final, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if f := localFailure(err); f != nil {
		return "", f // (GET would be refused the same)
	}
	final, status, err = q.probe(ctx, http.MethodGet, rawURL, s)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", netFailure(err)
	}
	detail := fmt.Sprintf("HTTP %d from %s", status, logURL(rawURL))
	if f := statusFailure(status); f != nil {
		f.detail = detail
		return "", f
	}
	if status < 200 || status >= 300 {
		return "", &failure{code: CodeHTTPStatus, params: map[string]any{"status": status}, detail: detail}
	}
	return final, nil
}

// resolver resolves the URLs of a task's downloads, each once (an HLS key
// shared by many segments, a file resumed after a stop), and checks the
// host of each URL it hands aria2c (local.go), each host once.
type resolver struct {
	q     *Queue
	s     runSnapshot
	mu    sync.Mutex
	done  map[string]string
	hosts map[string]error
}

func (q *Queue) newResolver(s runSnapshot) *resolver {
	return &resolver{q: q, s: s, done: map[string]string{}, hosts: map[string]error{}}
}

// needed: a URL is resolved by Go first when its request carries a
// credential aria2c would take along to wherever a redirect leads: the
// task's cookies, or the page's whole URL as Referer (the media on the
// page's own origin, refererFor).
func (r *resolver) needed(rawURL string) bool {
	return len(r.s.cookies) > 0 || (r.s.referer != "" && refererFor(r.s.referer, rawURL) == cleanReferer(r.s.referer))
}

// checkHost refuses a URL for aria2c whose host is local (by name or by
// address, local.go) or on the do-not-handle list. The URL itself is
// judged every time (the list's path entries, the proxy by scheme and
// port); only the check of a name's addresses is kept, per host (SEC10
// info-3: the first URL's answer stood for every later one on its host).
//
// Whether the URL goes through a proxy of the user's, which skips the
// address check, is upstream's answer, the guard proxy's too (guard.go:
// aria2c's own proxy is always the guard, which judges the URL again,
// and every redirect after it, when aria2c asks for it). A proxy that
// cannot be used (SOCKS, https://) refuses the URL first, before any
// look-up: downloads.proxy_unsupported (local.go upstream).
func (r *resolver) checkHost(ctx context.Context, rawURL string) error {
	addr := r.q.netOf(r.s).addr
	if u, err := url.Parse(rawURL); err == nil {
		if _, err := addr.p.upstream(u); err != nil {
			return asFailure(err)
		}
	}
	ariaProxied := addr.p.viaUpstream
	err := addr.checkURLWith(ctx, rawURL, true, func(host string) error {
		key, _ := db.HostKey(host) // (sameHost's rule: a look-alike host is checked itself)
		r.mu.Lock()
		err, ok := r.hosts[key]
		r.mu.Unlock()
		if ok {
			return err
		}
		// a name Go cannot look up is refused when aria2c would look it
		// up with the same resolver (SEC16 R2); with aria2c's features
		// unknown, left to aria2c as before (engine.features logs it)
		err = addr.checkAddrsWith(ctx, host, r.q.eng.systemResolver())
		if ctx.Err() != nil {
			return ctx.Err() // (not kept)
		}
		r.mu.Lock()
		r.hosts[key] = err
		r.mu.Unlock()
		return err
	}, ariaProxied)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if f := localFailure(err); f != nil {
		return f
	}
	return err
}

// final returns the URL to give aria2c for rawURL.
func (r *resolver) final(ctx context.Context, rawURL string) (string, error) {
	if !r.needed(rawURL) {
		if err := r.checkHost(ctx, rawURL); err != nil {
			return "", err
		}
		return rawURL, nil
	}
	r.mu.Lock()
	f, ok := r.done[rawURL]
	r.mu.Unlock()
	if ok {
		return f, nil
	}
	f, err := r.q.resolveFinal(ctx, rawURL, r.s)
	if err != nil {
		return "", err
	}
	if err := r.checkHost(ctx, f); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.done[rawURL] = f
	r.mu.Unlock()
	return f, nil
}

// finals resolves several URLs at once (one slot of the download window
// each) and returns them in order; the first error wins.
func (r *resolver) finals(ctx context.Context, urls []string) ([]string, error) {
	out := make([]string, len(urls))
	if len(r.s.cookies) == 0 || len(urls) == 1 {
		for i, u := range urls {
			f, err := r.final(ctx, u)
			if err != nil {
				return nil, err
			}
			out[i] = f
		}
		return out, nil
	}
	errs := make([]error, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = r.final(ctx, u)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
