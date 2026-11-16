// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"sync"
)

// Size-based rotation of {data_dir}/logs/lectureflow.log (T067,
// test-chaos #5). Not a setting: the limits are constants.
const (
	logRotateMaxBytes = 10 << 20 // rotate once the current file would pass this
	logRotateKeep     = 5        // lectureflow.log.1 … .5; the oldest is dropped
)

// rotatingLog is an io.Writer over one log file that renames it to
// path.1 (shifting path.1 … path.keep-1 up by one and dropping path.keep;
// see shift) before a write would take it past maxBytes, then starts a new
// file.
//
// Each Write goes whole into one file: slog handlers call Write once per
// record, so a line is never split between two files, and the mutex keeps
// concurrent records from interleaving.
//
// A failed rotation (on Windows another process holding path or path.N open
// is the usual cause) never stops logging: the writer keeps appending to the
// current file, warns once through warn, and tries again after another
// retryStep bytes. After a rotation succeeds, a later failure warns again.
type rotatingLog struct {
	path     string
	maxBytes int64
	keep     int

	// Injected for tests; os.Rename / os.Remove / os.OpenFile otherwise.
	rename func(oldpath, newpath string) error
	remove func(name string) error
	open   func(name string) (*os.File, error)
	// warn is called without the writer's lock held but possibly from
	// inside a slog handler's Write, so it must not log synchronously
	// through the same handler (newRotatingLog's default logs from a new
	// goroutine).
	warn func(err error)

	mu        sync.Mutex
	f         *os.File
	size      int64
	nextTry   int64 // after a failed rotation: size at which to try again
	warned    bool
	closed    bool
	retryStep int64
}

// newRotatingLog opens path for appending, rotating it first when it is
// already at or past maxBytes (a log left by an earlier run).
func newRotatingLog(path string, maxBytes int64, keep int) (*rotatingLog, error) {
	r := &rotatingLog{
		path:     path,
		maxBytes: maxBytes,
		keep:     keep,
		rename:   os.Rename,
		remove:   os.Remove,
		open:     openAppend,
		warn: func(err error) {
			go slog.Warn("log rotation failed; still writing to the current log file", "err", err)
		},
	}
	return r, r.start()
}

// start finishes setting r up once its fields are set (tests set their
// own hooks before calling it).
func (r *rotatingLog) start() error {
	if r.keep < 1 {
		r.keep = 1
	}
	if r.maxBytes < 1 {
		r.maxBytes = 1
	}
	r.retryStep = r.maxBytes / 10
	if r.retryStep < 1 {
		r.retryStep = 1
	}
	if err := r.openCurrent(); err != nil {
		return err
	}
	if r.size >= r.maxBytes {
		r.mu.Lock()
		err := r.rotateLocked()
		r.mu.Unlock()
		r.reportRotation(err)
		if r.f == nil {
			return err
		}
	}
	return nil
}

func openAppend(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

// openCurrent opens r.path for appending and records its size.
func (r *rotatingLog) openCurrent() error {
	f, err := r.open(r.path)
	if err != nil {
		return err
	}
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	r.f, r.size = f, size
	return nil
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, os.ErrClosed
	}
	var rotErr error
	rotated := false
	if r.f == nil { // the reopen after an earlier rotation failed
		if err := r.openCurrent(); err != nil {
			r.mu.Unlock()
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes && r.size >= r.nextTry {
		rotErr, rotated = r.rotateLocked(), true
		if r.f == nil {
			r.mu.Unlock()
			r.reportRotation(rotErr)
			return 0, rotErr
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	r.mu.Unlock()
	if rotated {
		r.reportRotation(rotErr)
	}
	return n, err
}

// rotateLocked shifts the files and reopens r.path. On a failed shift it
// reopens the current file and schedules the next try; r.f is nil only
// when even that reopen failed.
func (r *rotatingLog) rotateLocked() error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	shiftErr := r.shift()
	if err := r.openCurrent(); err != nil {
		return errors.Join(shiftErr, err)
	}
	if shiftErr != nil {
		r.nextTry = r.size + r.retryStep
		return shiftErr
	}
	r.nextTry = 0
	return nil
}

// pendingSuffix names the file the current log is renamed to first when it
// is rotated (path + pendingSuffix), before any numbered file is touched.
const pendingSuffix = ".rotating"

// shift moves path to path.1, shifting path.1 … up by one and dropping
// path.keep only when every slot up to it is taken. Missing files are
// skipped.
//
// The step most likely to fail comes first: path, which another process may
// hold open on Windows, is renamed to path.rotating, and only after that
// worked are the numbered files touched (settlePending). A failure therefore
// never costs a log: when path cannot be renamed nothing else happens, and
// a failure later on leaves path.rotating (and at worst a gap in the
// numbers), which the next rotation places first. Before this order a
// rename of path that kept failing dropped one more old log at every retry
// (review R5).
func (r *rotatingLog) shift() error {
	pending := r.path + pendingSuffix
	if rlFileExists(pending) { // left by an earlier rotation that failed midway
		if err := r.settlePending(pending); err != nil {
			return err
		}
	}
	if err := r.rename(r.path, pending); err != nil {
		return fmt.Errorf("rotate log: %w", err)
	}
	return r.settlePending(pending)
}

// settlePending renames pending to path.1 after freeing that slot: the
// numbered files below the first missing one move up by one; path.keep is
// removed only when path.1 … path.keep all exist. A gap left by a shift
// that failed midway is filled first, so a retry drops nothing more.
func (r *rotatingLog) settlePending(pending string) error {
	numbered := func(i int) string { return r.path + "." + strconv.Itoa(i) }
	gap := 0
	for i := 1; i <= r.keep; i++ {
		if !rlFileExists(numbered(i)) {
			gap = i
			break
		}
	}
	if gap == 0 {
		if err := r.remove(numbered(r.keep)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove oldest log: %w", err)
		}
		gap = r.keep
	}
	for i := gap - 1; i >= 1; i-- {
		if err := r.rename(numbered(i), numbered(i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("shift log %d: %w", i, err)
		}
	}
	if err := r.rename(pending, numbered(1)); err != nil {
		return fmt.Errorf("rotate log: %w", err)
	}
	return nil
}

// rlFileExists reports whether name exists (an error other than "not
// found" counts as existing, so it is never taken for a free slot).
func rlFileExists(name string) bool {
	_, err := os.Lstat(name)
	return !errors.Is(err, fs.ErrNotExist)
}

// reportRotation warns about the first failure of a streak and re-arms
// the warning once a rotation succeeds.
func (r *rotatingLog) reportRotation(err error) {
	r.mu.Lock()
	if err == nil {
		r.warned = false
		r.mu.Unlock()
		return
	}
	first := !r.warned
	r.warned = true
	warn := r.warn
	r.mu.Unlock()
	if first && warn != nil {
		warn(err)
	}
}

func (r *rotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
