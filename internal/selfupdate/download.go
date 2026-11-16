// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Downloader fetches one package file. The application's implementation
// uses the bundled aria2c (internal/fetch); HTTPDownloader is the fallback
// for tests and for when aria2c is not available.
//
// Contract:
//   - Write the file to req.Path, resuming what is already there when the
//     server allows it. Helper files next to it must start with req.Path
//     (aria2c's "<path>.aria2") and are removed once the download is done.
//   - req.URLs are mirrors of the same file, best first; any may be used.
//   - Requests carry only "User-Agent: LectureFlow": no cookies, no
//     referrer, no other header that identifies the user.
//   - Return nil when req.Path holds req.Size bytes. The caller checks the
//     size and SHA-256 itself and deletes the file when they differ.
//   - Return context.Cause(ctx) promptly once ctx is done.
//   - req.Progress (may be nil) gets the bytes present so far.
type Downloader interface {
	Download(ctx context.Context, req DownloadRequest) error
}

// DownloadRequest is one file to download.
type DownloadRequest struct {
	URLs []string
	// UserURLs are those of URLs that come from the user's own update
	// sources (the settings' extra sources: a {url} template or a
	// directory), which may be a mirror on this computer or the local
	// network. The others (GitHub, update.json's URL and its mirror rules)
	// are public addresses: a downloader that judges where it connects
	// (internal/fetch's guard proxy) lets the hosts of UserURLs alone be on
	// this computer or the local network.
	UserURLs []string
	Path     string
	Size     int64
	SHA256   string // hex; aria2c may check it too (--checksum)
	Progress func(done int64)
}

// NewHTTPClient returns the client of the update checks and of
// HTTPDownloader: no cookie jar, redirects followed without a Referer
// header, the environment's proxy settings. timeout bounds a whole request
// (0 = none: downloads).
//
// A redirect must lead to an address a source may have (secureURL: https,
// or plain http to this computer) and never from https to plain http, also
// on the same host: the reason for https (nobody on the way sees which
// release is fetched or holds back a newer one) holds for the address
// redirected to as well. (The bundled aria2c cannot be restricted like
// this; the signature and the SHA-256 still decide what is installed.)
func NewHTTPClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.DisableCompression = true // byte ranges must address the file itself
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if !secureURL(req.URL.String()) || (len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https") {
				return fmt.Errorf("%w to %s", errInsecureRedirect, redactURL(req.URL.String()))
			}
			// net/http adds the previous URL as Referer; keep requests bare.
			req.Header.Del("Referer")
			req.Header.Del("Cookie")
			return nil
		},
	}
}

// newRequest builds a GET with only the User-Agent header.
func newRequest(ctx context.Context, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	return req, nil
}

// HTTPDownloader is a plain net/http Downloader: URLs in order, resumed
// with Range requests, a URL given up after three attempts in a row that
// brought no new byte.
type HTTPDownloader struct {
	Client  *http.Client  // nil = NewHTTPClient(0)
	Backoff time.Duration // first delay before a retry (0 = 1 s)
	Stall   time.Duration // no byte for this long = the transfer broke (0 = 60 s)
}

type httpStatusError struct{ status int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP status %d", e.status) }

// localError marks a failure of the local file (disk full, permissions):
// another URL would not help.
type localError struct{ err error }

func (e *localError) Error() string { return e.err.Error() }
func (e *localError) Unwrap() error { return e.err }

var errRestart = errors.New("the server did not resume the partial download")
var errTooLong = errors.New("the server sent more bytes than expected")
var errStalled = errors.New("the download stalled")
var errOtherSize = errors.New("the server's file has another size than update.json says")
var errInsecureRedirect = errors.New("refused a redirect to plain http")

// maxAttempts bounds the requests to one URL (a transfer that keeps
// breaking off after some progress).
const maxAttempts = 12

// Download implements Downloader.
func (d *HTTPDownloader) Download(ctx context.Context, req DownloadRequest) error {
	if len(req.URLs) == 0 {
		return errors.New("no download URL")
	}
	var last error
	for _, u := range req.URLs {
		err := d.from(ctx, u, req)
		if err == nil {
			return nil
		}
		if c := context.Cause(ctx); c != nil {
			return c
		}
		var le *localError
		if errors.As(err, &le) {
			return le.err
		}
		last = fmt.Errorf("%s: %w", redactURL(u), err)
	}
	return last
}

func (d *HTTPDownloader) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return NewHTTPClient(0)
}

func (d *HTTPDownloader) from(ctx context.Context, u string, req DownloadRequest) error {
	backoff := d.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	noProgress := 0
	for attempt := 0; ; attempt++ {
		n, err := partSize(req.Path, req.Size)
		if err != nil {
			return &localError{err}
		}
		if req.Progress != nil {
			req.Progress(n)
		}
		if n == req.Size {
			return nil
		}
		got, err := d.once(ctx, u, req, n)
		if err == nil {
			continue
		}
		if c := context.Cause(ctx); c != nil {
			return c
		}
		var le *localError
		if errors.As(err, &le) || !transient(err) {
			return err
		}
		if got > n {
			noProgress = 0
		} else {
			noProgress++
		}
		if noProgress >= 3 || attempt >= maxAttempts {
			return err
		}
		select {
		case <-time.After(backoff << uint(noProgress)):
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// partSize returns the bytes of path, emptying a file longer than size.
func partSize(path string, size int64) (int64, error) {
	fi, err := os.Stat(path)
	if isNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if fi.Size() > size {
		return 0, os.Truncate(path, 0)
	}
	return fi.Size(), nil
}

func transient(err error) bool {
	if errors.Is(err, errInsecureRedirect) {
		return false // the same answer next time: try the next URL
	}
	var hs *httpStatusError
	if errors.As(err, &hs) {
		return hs.status >= 500 || hs.status == http.StatusTooManyRequests || hs.status == http.StatusRequestTimeout
	}
	return errors.Is(err, errRestart) || errors.Is(err, errStalled) || errors.Is(err, io.ErrUnexpectedEOF) || isNetErr(err)
}

// once makes one request from offset n and returns the new length.
func (d *HTTPDownloader) once(ctx context.Context, u string, req DownloadRequest, n int64) (int64, error) {
	stall := d.Stall
	if stall <= 0 {
		stall = 60 * time.Second
	}
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	hreq, err := newRequest(rctx, u)
	if err != nil {
		return n, err
	}
	hreq.Header.Set("Accept-Encoding", "identity")
	if n > 0 {
		hreq.Header.Set("Range", "bytes="+strconv.FormatInt(n, 10)+"-")
	}
	timer := time.AfterFunc(stall, func() { cancel(errStalled) })
	defer timer.Stop()
	resp, err := d.client().Do(hreq)
	if err != nil {
		if context.Cause(rctx) == errStalled && ctx.Err() == nil {
			return n, errStalled
		}
		return n, err
	}
	defer resp.Body.Close()
	truncate := func() error {
		if err := os.Truncate(req.Path, 0); err != nil && !isNotExist(err) {
			return &localError{err}
		}
		return nil
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		if resp.ContentLength >= 0 && resp.ContentLength != req.Size {
			return n, errOtherSize
		}
		if n > 0 { // Range ignored: start over
			if err := truncate(); err != nil {
				return n, err
			}
			n = 0
		}
	case resp.StatusCode == http.StatusPartialContent:
		if total, ok := contentRangeTotal(resp.Header.Get("Content-Range")); ok && total != req.Size {
			return n, errOtherSize
		}
		if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); !ok || start != n {
			if err := truncate(); err != nil {
				return n, err
			}
			return 0, errRestart
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		if err := truncate(); err != nil {
			return n, err
		}
		return 0, errRestart
	default:
		return n, &httpStatusError{resp.StatusCode}
	}
	out, err := os.OpenFile(req.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return n, &localError{err}
	}
	buf := make([]byte, 256<<10)
	var rerr error
	for {
		k, err := resp.Body.Read(buf)
		if k > 0 {
			timer.Reset(stall)
			if n+int64(k) > req.Size {
				out.Close()
				if terr := truncate(); terr != nil {
					return 0, terr
				}
				return 0, errTooLong
			}
			if _, werr := out.Write(buf[:k]); werr != nil {
				out.Close()
				return n, &localError{werr}
			}
			n += int64(k)
			if req.Progress != nil {
				req.Progress(n)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			rerr = err
			if context.Cause(rctx) == errStalled && ctx.Err() == nil {
				rerr = errStalled
			}
			break
		}
	}
	if err := out.Close(); err != nil && rerr == nil {
		return n, &localError{err}
	}
	if rerr == nil && n < req.Size {
		rerr = io.ErrUnexpectedEOF
	}
	return n, rerr
}

func contentRangeStart(v string) (int64, bool) {
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

// contentRangeTotal reads the complete length of "bytes 100-199/200"
// (false for "*" or a malformed value).
func contentRangeTotal(v string) (int64, bool) {
	i := strings.LastIndexByte(v, '/')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v[i+1:]), 10, 64)
	return n, err == nil
}

// redactURL drops the query and fragment of a URL for logs and error texts.
func redactURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}
