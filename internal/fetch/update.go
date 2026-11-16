// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/model"
	"lectureflow/internal/selfupdate"
)

// The in-app update's downloader on the queue's aria2c (SPEC-ext 主程序配套
// 改动 4; the selfupdate.Downloader contract in internal/selfupdate
// download.go and README.md). It shares the queue's aria2c process: the
// same start on demand, idle stop, RPC secret and conf file handling.

// UpdateDownloader returns the queue's aria2c as a selfupdate.Downloader.
// When aria2c cannot be started (missing, refused to start), or the proxy
// set for a URL is one aria2c cannot use, a download falls back to
// selfupdate's own net/http downloader.
func (q *Queue) UpdateDownloader() selfupdate.Downloader {
	return &updateDownloader{q: q, fallback: &selfupdate.HTTPDownloader{}}
}

type updateDownloader struct {
	q        *Queue
	fallback selfupdate.Downloader
}

// policy is the queue's address policy for the update's downloads,
// without ALL_PROXY's rule (addrPolicy.ignoreAllProxy): the update's
// other way, net/http, does not read ALL_PROXY either, so refusing it
// there would only trade aria2c and the guard for a direct download
// without the guard's checks.
func (d *updateDownloader) policy() *addrPolicy {
	p := *d.q.local
	p.ignoreAllProxy = true
	return &p
}

// errLocal marks a failure of the local file (disk full, permissions):
// another mirror would not help, and selfupdate shows it as
// selfupdate.io_failed.
type errLocal struct{ err error }

func (e *errLocal) Error() string { return e.err.Error() }
func (e *errLocal) Unwrap() error { return e.err }

func ioFailure(err error) error { return model.Errorf(selfupdate.CodeIO, nil, err) }

// Download implements selfupdate.Downloader: the URLs in order (mirrors of
// one file), each a single aria2c download into req.Path resuming what is
// there; requests carry "User-Agent: LectureFlow" and no other header of
// ours (no cookie, no referrer). aria2c's helper file is req.Path +
// ".aria2"; it is gone once the download completes.
func (d *updateDownloader) Download(ctx context.Context, req selfupdate.DownloadRequest) error {
	if len(req.URLs) == 0 {
		return errors.New("no download URL")
	}
	for _, u := range req.URLs {
		if !validHTTPURL(u) {
			return fmt.Errorf("not a download URL: %s", bridge.RedactURL(u))
		}
	}
	if done, err := d.prepare(req); err != nil || done {
		return err
	}
	// a proxy aria2c cannot use (SOCKS, https:// HTTP(S)_PROXY: local.go
	// upstream): net/http speaks to it, where aria2c would connect directly
	// or send the proxy's password in the clear. A SOCKS or https://
	// ALL_PROXY alone does not count here (policy): net/http does not read
	// it either, so aria2c and the guard download directly, with the
	// guard's checks (SEC19 low-7).
	pol := d.policy()
	for _, raw := range req.URLs {
		if u, err := url.Parse(raw); err == nil {
			if _, err := pol.upstream(u); err != nil {
				d.q.log.Warn("selfupdate: the proxy set is one aria2c cannot use; the update downloads over net/http", "err", err)
				return d.fallback.Download(ctx, req)
			}
		}
	}
	inst, err := d.q.eng.acquire(ctx)
	if err != nil {
		if c := context.Cause(ctx); c != nil {
			return c
		}
		if errors.Is(err, ErrClosed) {
			return err
		}
		d.q.log.Warn("selfupdate: aria2c unavailable; the update downloads over net/http", "err", err)
		return d.fallback.Download(ctx, req)
	}
	defer d.q.eng.release()
	var last error
	for _, u := range req.URLs {
		err := d.one(ctx, inst, u, req)
		if err == nil {
			return nil
		}
		if c := context.Cause(ctx); c != nil {
			return c
		}
		var le *errLocal
		if errors.As(err, &le) {
			return ioFailure(le.err)
		}
		// (update URLs are public: their paths may be logged)
		d.q.log.Warn("selfupdate: a download URL failed", "url", bridge.RedactURL(u), "err", err)
		last = fmt.Errorf("%s: %w", bridge.RedactURL(u), err)
	}
	return last
}

// prepare looks at what req.Path holds: a file longer than expected is
// removed (it cannot be resumed), a complete one without aria2c's control
// file is done.
func (d *updateDownloader) prepare(req selfupdate.DownloadRequest) (done bool, err error) {
	fi, err := os.Stat(req.Path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, ioFailure(err)
	}
	control := fileExists(req.Path + ".aria2")
	switch {
	case fi.Size() > req.Size:
		if err := os.Remove(req.Path); err != nil {
			return false, ioFailure(err)
		}
		os.Remove(req.Path + ".aria2")
	case fi.Size() == req.Size && !control:
		if req.Progress != nil {
			req.Progress(fi.Size())
		}
		return true, nil
	}
	return false, nil
}

var errUpdateSize = errors.New("the server's file has another size than update.json says")

// one downloads req from u with aria2c, through the guard proxy like every
// download (guard.go). When u is one of the user's own sources
// (req.UserURLs: a mirror the user added) its host may be on this computer
// or the local network; any other URL of the update (GitHub, update.json's
// own: public addresses) is judged like a recording's URL from an unknown
// page, and so is what a redirect leads to: public addresses only (SEC19
// info-5).
func (d *updateDownloader) one(ctx context.Context, inst *instance, u string, req selfupdate.DownloadRequest) error {
	d.q.mu.Lock()
	conns := d.q.conns
	d.q.mu.Unlock()
	var trusted []string
	if slices.Contains(req.UserURLs, u) {
		trusted = []string{u}
	}
	sess, err := inst.guard.open(d.policy().forTask(""), trusted...)
	if err != nil {
		return engineFailure(inst, err)
	}
	defer sess.close()
	opts := map[string]any{
		"dir": filepath.Dir(req.Path), "out": filepath.Base(req.Path),
		"split": strconv.Itoa(conns), "max-connection-per-server": strconv.Itoa(conns), "min-split-size": "1M",
		"user-agent": selfupdate.UserAgent,
	}
	for name, v := range sess.options() {
		opts[name] = v
	}
	gid, err := inst.rpc.addURIDetached(ctx, u, opts)
	if err != nil {
		if c := context.Cause(ctx); c != nil {
			return c
		}
		return engineFailure(inst, err)
	}
	stop := func() { d.q.stopGIDs(inst, []string{gid}) }
	tick := time.NewTicker(d.q.o.PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			stop() // the partial file stays for the next attempt
			return context.Cause(ctx)
		case <-tick.C:
		}
		sts, err := inst.rpc.tellStatuses(ctx, []string{gid})
		if err != nil {
			stop()
			if c := context.Cause(ctx); c != nil {
				return c
			}
			return engineFailure(inst, err)
		}
		if err := sess.refused(); err != nil {
			stop() // a redirect the guard refused: the next mirror
			_ = inst.rpc.removeResult(ctx, gid)
			return errors.New(asFailure(err).detail)
		}
		st := sts[0]
		switch st.Status {
		case "complete":
			_ = inst.rpc.removeResult(ctx, gid)
			fi, err := os.Stat(req.Path)
			if err != nil {
				return &errLocal{err}
			}
			if req.Progress != nil {
				req.Progress(fi.Size())
			}
			if fi.Size() != req.Size {
				os.Remove(req.Path) // another file than the signed one: no resume from it
				return errUpdateSize
			}
			return nil
		case "error":
			_ = inst.rpc.removeResult(ctx, gid)
			var f *failure
			if !errors.As(sess.failureOf(st), &f) {
				return errors.New("aria2 error " + st.ErrorCode)
			}
			if f.code == CodeNoSpace || f.code == CodeWriteFailed {
				return &errLocal{errors.New(f.detail)}
			}
			return errors.New(f.detail)
		case "removed", "":
			return errors.New("aria2c dropped the download")
		default:
			if total := num(st.TotalLength); total > 0 && total != req.Size {
				stop()
				os.Remove(req.Path) // not the signed file: nothing to resume
				os.Remove(req.Path + ".aria2")
				return errUpdateSize
			}
			if req.Progress != nil {
				req.Progress(num(st.CompletedLength))
			}
		}
	}
}
