// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

// Downloads of the kit's files: resumable (HTTP Range on the .part file),
// size-limited, SHA-256 checked; https only (http to a loopback host in the
// tests, remote.KitSpec.Validate). The finished file stays in downloads/
// until the caller moves it.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lectureflow/internal/remote"
)

// httpClient is the downloads' client (tests replace it).
var httpClient = &http.Client{
	Timeout: 0, // large files: the per-read idle timeout below bounds a stall
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	},
}

const downloadTries = 4

// download fetches f into downloads/<sha256>.part and returns the path of
// the checked file (downloads/<sha256>).
func (e *env) download(ctx context.Context, f remote.KitDownload, progress func(float64)) (string, error) {
	dir := filepath.Join(e.dirs.Root, dirDownloads)
	final, err := inside(dir, f.SHA256)
	if err != nil {
		return "", err
	}
	if n, ok := regularFile(final); ok && n == f.Size {
		if sum, err := fileSHA256(final); err == nil && sum == f.SHA256 {
			return final, nil
		}
		_ = os.Remove(final)
	}
	part := final + ".part"
	var last error
	for try := 0; try < downloadTries; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(try*try) * 2 * time.Second):
			}
		}
		last = e.fetch(ctx, f, part, progress)
		if last == nil {
			break
		}
		if errors.Is(last, errHashMismatch) {
			_ = os.Remove(part)
		}
	}
	if last != nil {
		if errors.Is(last, errHashMismatch) {
			return "", coded(remote.AgentHashMismatch, last)
		}
		return "", coded(remote.AgentDownloadFailed, last)
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}

var errHashMismatch = errors.New("SHA-256 mismatch")

func (e *env) fetch(ctx context.Context, f remote.KitDownload, part string, progress func(float64)) error {
	have, _ := regularFile(part)
	if have > f.Size {
		_ = os.Remove(part)
		have = 0
	}
	if have < f.Size {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "lectureflow-agent/"+version())
		if have > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		switch {
		case have > 0 && resp.StatusCode == http.StatusPartialContent:
		case resp.StatusCode == http.StatusOK:
			have = 0 // the server ignored the range: start over
		default:
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		flags := os.O_WRONLY | os.O_CREATE
		if have == 0 {
			flags |= os.O_TRUNC
		} else {
			flags |= os.O_APPEND
		}
		fh, err := os.OpenFile(part, flags, 0o600)
		if err != nil {
			return err
		}
		w := &progressWriter{w: fh, n: have, total: f.Size, report: progress}
		_, err = io.Copy(w, &idleReader{r: io.LimitReader(resp.Body, f.Size-have+1), idle: 2 * time.Minute})
		if cerr := fh.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	n, _ := regularFile(part)
	if n != f.Size {
		if n > f.Size {
			_ = os.Remove(part)
		}
		return fmt.Errorf("size %d, want %d", n, f.Size)
	}
	sum, err := fileSHA256(part)
	if err != nil {
		return err
	}
	if sum != f.SHA256 {
		return errHashMismatch
	}
	return nil
}

type progressWriter struct {
	w        io.Writer
	n, total int64
	report   func(float64)
	last     time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	if p.report != nil && p.total > 0 && time.Since(p.last) > time.Second {
		p.last = time.Now()
		p.report(min(float64(p.n)/float64(p.total), 1))
	}
	return n, err
}

// idleReader fails a read that takes longer than idle (a stalled download).
type idleReader struct {
	r    io.Reader
	idle time.Duration
}

func (i *idleReader) Read(b []byte) (int, error) {
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		n, err := i.r.Read(b)
		ch <- res{n, err}
	}()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-time.After(i.idle):
		return 0, errors.New("download stalled")
	}
}

// untarGz unpacks a .tar.gz into dst: regular files, directories, hard
// links and symbolic links whose target stays inside dst; no absolute
// paths, no "..", no devices, no link chains (untar_links.go). Modes keep
// their execute bits, never set-id bits. The archive is read twice: the
// first pass checks every entry, so a refused archive leaves dst as it was;
// the second writes, with the hard links and then the symbolic links last.
func untarGz(archive, dst string) error {
	g := newLinkGuard()
	if err := eachTarEntry(archive, func(h *tar.Header, _ io.Reader) error {
		p, err := tarEntryPath(dst, h.Name)
		if err != nil || p == "" {
			return err
		}
		rel := relSlash(dst, p)
		switch h.Typeflag {
		case tar.TypeDir:
			return g.entry(rel, true)
		case tar.TypeReg:
			return g.entry(rel, false)
		case tar.TypeSymlink:
			if !linkTargetOK(h.Linkname) || !within(dst, filepath.Join(filepath.Dir(p), filepath.FromSlash(h.Linkname))) {
				return errUnsafeLink
			}
			return g.symlink(rel)
		case tar.TypeLink:
			src := filepath.Join(dst, filepath.FromSlash(h.Linkname))
			if !within(dst, src) || src == dst {
				return errUnsafeLink
			}
			return g.hardLink(rel, relSlash(dst, src))
		}
		return nil
	}); err != nil {
		return err
	}

	type link struct{ path, target string }
	var hard, soft []link
	plain := map[string]bool{}
	if err := eachTarEntry(archive, func(h *tar.Header, r io.Reader) error {
		p, err := tarEntryPath(dst, h.Name)
		if err != nil || p == "" {
			return err
		}
		mode := os.FileMode(h.Mode).Perm() & 0o755
		switch h.Typeflag {
		case tar.TypeDir:
			if err := plainDirs(dst, p, plain); err != nil {
				return err
			}
			return os.MkdirAll(p, 0o700)
		case tar.TypeReg:
			if err := plainDirs(dst, p, plain); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(r, h.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			return err
		case tar.TypeSymlink:
			soft = append(soft, link{p, h.Linkname})
		case tar.TypeLink:
			hard = append(hard, link{p, filepath.Join(dst, filepath.FromSlash(h.Linkname))})
		default:
			// devices, fifos and the like: skipped
		}
		return nil
	}); err != nil {
		return err
	}
	for _, l := range hard {
		if err := plainDirs(dst, l.target, plain); err != nil {
			return err
		}
		if err := plainDirs(dst, l.path, plain); err != nil {
			return err
		}
		b, err := os.ReadFile(l.target)
		if err != nil {
			return err
		}
		fi, _ := os.Lstat(l.target)
		mode := os.FileMode(0o600)
		if fi != nil {
			mode = fi.Mode().Perm()
		}
		if err := os.WriteFile(l.path, b, mode); err != nil {
			return err
		}
	}
	for _, l := range soft {
		if err := plainDirs(dst, l.path, plain); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
			return err
		}
		if err := os.Symlink(l.target, l.path); err != nil {
			return err
		}
	}
	return nil
}

// eachTarEntry calls fn for each entry of a .tar.gz, with its contents.
func eachTarEntry(archive string, fn func(h *tar.Header, r io.Reader) error) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

// tarEntryPath maps an entry name to a path below dst ("" = nothing to
// write: the archive's root).
func tarEntryPath(dst, entry string) (string, error) {
	name := filepath.FromSlash(strings.TrimPrefix(entry, "./"))
	if name == "" || name == "." {
		return "", nil
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, `\`) || hasDotDot(name) {
		return "", errUnsafePath
	}
	p := filepath.Join(dst, name)
	if !within(dst, p) {
		return "", errUnsafePath
	}
	if p == filepath.Clean(dst) {
		return "", nil
	}
	return p, nil
}

// relSlash is p relative to dst, "/"-separated.
func relSlash(dst, p string) string {
	rel, _ := filepath.Rel(dst, p)
	return filepath.ToSlash(rel)
}

func hasDotDot(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

// within reports whether p is dst or below it (lexically).
func within(dst, p string) bool {
	rel, err := filepath.Rel(dst, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
