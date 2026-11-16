// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/selfupdate"
)

// updateServer serves update packages and records every request's headers.
type updateServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	seen  []http.Header
	paths []string
	gate  chan struct{} // non-nil: a GET without Range stops after half
}

func newUpdateServer(t *testing.T) *updateServer {
	u := &updateServer{files: map[string][]byte{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.seen = append(u.seen, r.Header.Clone())
		u.paths = append(u.paths, r.URL.Path)
		b, ok := u.files[r.URL.Path]
		gate := u.gate
		u.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if gate != nil && r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", itoa(int64(len(b))))
			w.WriteHeader(http.StatusOK)
			w.Write(b[:len(b)/2])
			w.(http.Flusher).Flush()
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			w.Write(b[len(b)/2:])
			return
		}
		http.ServeContent(w, r, "pkg", time.Time{}, bytes.NewReader(b))
	}))
	t.Cleanup(func() {
		u.open()
		u.srv.Close()
	})
	return u
}

func (u *updateServer) open() {
	u.mu.Lock()
	if u.gate != nil {
		close(u.gate)
		u.gate = nil
	}
	u.mu.Unlock()
}

func (u *updateServer) headers() []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]http.Header(nil), u.seen...)
}

func updatePath(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "updates", "3.2.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "LectureFlow-Setup-3.2.0.exe.part")
}

// The selfupdate.Downloader contract on the queue's aria2c: mirrors in
// order, only "User-Agent: LectureFlow", helper files beside the path and
// gone afterwards, the size checked.
func TestUpdateDownloader(t *testing.T) {
	e := newQEnv(t, nil)
	srv := newUpdateServer(t)
	pkg := pattern(5, 700_000)
	srv.files["/good/pkg.exe"] = pkg
	srv.files["/other/pkg.exe"] = pattern(6, 1000) // another file than the signed one
	path := updatePath(t)
	var progress int64
	var dl selfupdate.Downloader = e.q.UpdateDownloader()
	err := dl.Download(context.Background(), selfupdate.DownloadRequest{
		URLs:     []string{srv.srv.URL + "/missing/pkg.exe", srv.srv.URL + "/other/pkg.exe", srv.srv.URL + "/good/pkg.exe"},
		Path:     path,
		Size:     int64(len(pkg)),
		Progress: func(n int64) { progress = n },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, path), pkg) || progress != int64(len(pkg)) {
		t.Fatalf("file %d bytes, progress %d", len(readAll(t, path)), progress)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("left in the directory: %v", entries)
	}
	for _, h := range srv.headers() {
		if h.Get("User-Agent") != selfupdate.UserAgent || h.Get("Cookie") != "" || h.Get("Referer") != "" {
			t.Fatalf("request headers %v", h)
		}
	}
	adds, _, _, _, _ := e.aria.snapshot()
	if len(adds) != 3 {
		t.Fatalf("adds %+v", adds)
	}
	for _, a := range adds {
		if a.UA != selfupdate.UserAgent || len(a.Headers) != 0 || a.Out != filepath.Base(path) || a.Dir != filepath.Dir(path) {
			t.Fatalf("addUri %+v", a)
		}
	}
	// already there: nothing to do
	if err := dl.Download(context.Background(), selfupdate.DownloadRequest{URLs: []string{srv.srv.URL + "/good/pkg.exe"},
		Path: path, Size: int64(len(pkg))}); err != nil {
		t.Fatal(err)
	}
	if after, _, _, _, _ := e.aria.snapshot(); len(after) != 3 {
		t.Fatal("a complete file was downloaded again")
	}
	// every URL fails: the error says which, without the query
	err = dl.Download(context.Background(), selfupdate.DownloadRequest{URLs: []string{srv.srv.URL + "/missing/x.exe?token=SECRETQ"},
		Path: path + "2", Size: 10})
	if err == nil || strings.Contains(err.Error(), "SECRETQ") || !strings.Contains(err.Error(), "/missing/x.exe") {
		t.Fatalf("all failed: %v", err)
	}
}

// Cancelled: back at once with the cause, the partial file kept; the next
// call resumes it with a Range request.
func TestUpdateDownloaderCancelAndResume(t *testing.T) {
	e := newQEnv(t, nil)
	srv := newUpdateServer(t)
	pkg := pattern(7, 400_000)
	srv.files["/pkg.exe"] = pkg
	srv.gate = make(chan struct{})
	path := updatePath(t)
	dl := e.q.UpdateDownloader()
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("cancelled by the user")
	done := make(chan error, 1)
	go func() {
		done <- dl.Download(ctx, selfupdate.DownloadRequest{URLs: []string{srv.srv.URL + "/pkg.exe"}, Path: path, Size: int64(len(pkg))})
	}()
	e.waitFor("half of the package", func() bool {
		fi, err := os.Stat(path)
		return err == nil && fi.Size() >= int64(len(pkg)/2)
	})
	cancel(cause)
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("cancelled: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the download did not stop")
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("the partial file is gone: %v", err)
	}
	srv.open()
	if err := dl.Download(context.Background(), selfupdate.DownloadRequest{URLs: []string{srv.srv.URL + "/pkg.exe"},
		Path: path, Size: int64(len(pkg))}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, path), pkg) {
		t.Fatal("resumed file differs")
	}
	hs := srv.headers()
	if r := hs[len(hs)-1].Get("Range"); r == "" || r == "bytes=0-" {
		t.Fatalf("not resumed: Range %q", r)
	}
}

// SEC19 info-5: the update's session lets a host be on this computer or
// the local network only when its URL is one of the user's own sources
// (DownloadRequest.UserURLs); any other URL of the update (GitHub,
// update.json's) is judged like a recording's, so a public name that
// resolves here, or an official URL pointed at this computer, is refused.
func TestUpdateTrustsOnlyTheUsersSources(t *testing.T) {
	e := newQEnv(t, func(o *Options) { o.allowLoopback = false }) // (the product's policy)
	srv := newUpdateServer(t)
	pkg := pattern(9, 20_000)
	srv.files["/pkg.exe"] = pkg
	mirror := srv.srv.URL + "/pkg.exe" // (on 127.0.0.1)
	dl := e.q.UpdateDownloader()
	path := updatePath(t)
	if err := dl.Download(context.Background(), selfupdate.DownloadRequest{URLs: []string{mirror}, UserURLs: []string{mirror},
		Path: path, Size: int64(len(pkg))}); err != nil {
		t.Fatalf("the user's mirror on this computer: %v", err)
	}
	if !bytes.Equal(readAll(t, path), pkg) {
		t.Fatal("the file differs")
	}
	before := len(srv.headers())
	other := filepath.Join(filepath.Dir(updatePath(t)), "other.exe.part")
	err := dl.Download(context.Background(), selfupdate.DownloadRequest{URLs: []string{mirror}, Path: other, Size: int64(len(pkg))})
	if err == nil || !strings.Contains(err.Error(), "this computer") {
		t.Fatalf("a URL that is not the user's, on this computer: %v", err)
	}
	if n := len(srv.headers()); n != before {
		t.Errorf("the server was asked %d times", n-before)
	}
}

// aria2c missing: the update still downloads, over net/http; a file that
// cannot be written is a local error (selfupdate.io_failed).
func TestUpdateDownloaderFallbackAndLocalError(t *testing.T) {
	e := newQEnv(t, func(o *Options) {
		o.Aria2c = filepath.Join(t.TempDir(), "no-aria2c.exe")
		o.Launcher = nil
	})
	srv := newUpdateServer(t)
	pkg := pattern(8, 50_000)
	srv.files["/pkg.exe"] = pkg
	path := updatePath(t)
	if err := e.q.UpdateDownloader().Download(context.Background(), selfupdate.DownloadRequest{URLs: []string{srv.srv.URL + "/pkg.exe"},
		Path: path, Size: int64(len(pkg))}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, path), pkg) || srv.headers()[0].Get("User-Agent") != selfupdate.UserAgent {
		t.Fatal("fallback download")
	}

	e2 := newQEnv(t, nil)
	missingDir := filepath.Join(t.TempDir(), "gone", "pkg.exe.part")
	err := e2.q.UpdateDownloader().Download(context.Background(), selfupdate.DownloadRequest{URLs: []string{srv.srv.URL + "/pkg.exe"},
		Path: missingDir, Size: int64(len(pkg))})
	if selfupdate.CodeOf(err) != selfupdate.CodeIO {
		t.Fatalf("local error: %v (%s)", err, selfupdate.CodeOf(err))
	}
}
