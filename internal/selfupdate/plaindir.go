// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Removing things in the data directory without following links (security
// review SEC17, 高-A).
//
// A path such as <data>/updates/backup is resolved anew, component by
// component, each time it is used: when an account that could once write
// to the data directory left a junction or a symbolic link in place of
// updates/ (or electron/), everything below it lies wherever the link
// leads, and removing "the backups" removes a folder of the user's that
// happens to be called backup. So what is removed is reached through
// handles: the directory is looked at (Lstat: the entry itself), opened as
// an os.Root and checked to be the very directory that was looked at; what
// lies below is then named relative to that handle (os.Root never leaves
// it, and its RemoveAll opens every folder it descends into without
// following a link: a link is removed, never entered).

// errNotPlainDir: the entry is not a plain directory (see isPlainDir).
var errNotPlainDir = errors.New("not a plain directory (a link, a junction or another reparse point)")

// isPlainDir: a directory itself. On Windows (Go 1.23 and later) a
// junction is ModeIrregular without ModeDir, a symbolic link ModeSymlink,
// and a directory carrying another kind of reparse point (cloud files, for
// one) ModeDir|ModeIrregular; only a directory without any of them is
// plain, as for dirperm's walk, which takes every reparse point for a link.
func isPlainDir(fi os.FileInfo) bool { return fi.Mode().Type() == os.ModeDir }

// isLinkLike: a symbolic link, a junction or another reparse point.
func isLinkLike(fi os.FileInfo) bool { return fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 }

// openPlainDirIn opens the entry name of r as a root of its own when it is
// a plain directory. fi is what Lstat found (nil: nothing by that name, err
// is nil then). An entry that is something else gives errNotPlainDir with
// fi set; one replaced between the look and the opening gives an error.
func openPlainDirIn(r *os.Root, name string) (*os.Root, os.FileInfo, error) {
	fi, err := r.Lstat(name)
	if err != nil {
		if isNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if !isPlainDir(fi) {
		return nil, fi, errNotPlainDir
	}
	sub, err := r.OpenRoot(name) // never outside r: an absolute link (any junction) is refused
	if err != nil {
		return nil, fi, err
	}
	if err := samePlainDir(sub, fi); err != nil {
		sub.Close()
		return nil, fi, fmt.Errorf("%s: %w", name, err)
	}
	return sub, fi, nil
}

// openPlainDir opens the directory at path as an os.Root when it is a
// plain directory. Its parent (the data directory: the user's own path,
// which no other account can replace) is opened first, and the last
// component is looked at and opened relative to that handle
// (openPlainDirIn), so that the look and the opening describe the same
// directory: one replaced in between — by a link or by another plain
// directory — is refused. (Looking with os.Lstat by path, as before, could
// not tell another plain directory: on Windows a FileInfo from a path
// carries no file ID, and os.SameFile fetched it later by the path again,
// from whatever was there by then — security review SEC17, 低-E.) Nothing
// there: the error is fs.ErrNotExist; something else there: errNotPlainDir.
func openPlainDir(path string) (*os.Root, error) {
	clean := filepath.Clean(path)
	parent, err := os.OpenRoot(filepath.Dir(clean))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	r, fi, err := openPlainDirIn(parent, filepath.Base(clean))
	switch {
	case err != nil:
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	case fi == nil:
		return nil, &os.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return r, nil
}

// samePlainDir: the directory r holds is the one fi described (not a link
// or another folder put in its place in between). fi must come from a
// handle (os.Root's Lstat), whose FileInfo carries the file's ID.
func samePlainDir(r *os.Root, fi os.FileInfo) error {
	got, err := r.Stat(".")
	if err != nil {
		return err
	}
	if !isPlainDir(got) || !os.SameFile(fi, got) {
		return errors.New("replaced while it was being opened")
	}
	return nil
}

// readFileIn reads a small file of r (at most 1 MiB).
func readFileIn(r *os.Root, name string) ([]byte, error) {
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// writeFileAtomicIn writes data to a new temporary file of r, syncs it and
// renames it over name (the entry name is replaced, whatever it was; a
// link is never written through).
func writeFileAtomicIn(r *os.Root, name string, data []byte) error {
	var (
		tmp string
		f   *os.File
		err error
	)
	for i := 0; i < 100; i++ {
		tmp = fmt.Sprintf(".%s.%d.tmp", name, rand.Uint32())
		f, err = r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = renameRetryIn(r, tmp, name)
	}
	if werr != nil {
		r.Remove(tmp)
	}
	return werr
}

// renameRetryIn is renameRetry within r.
func renameRetryIn(r *os.Root, from, to string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = r.Rename(from, to); err == nil || isNotExist(err) || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}
