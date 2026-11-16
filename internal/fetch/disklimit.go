// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The disk while a task downloads (AUD3 low-2). The sizes the extension
// reports are estimates, and the server decides how much it sends: a
// response that never ends, or a playlist of tens of thousands of large
// segments, would otherwise fill the data directory's volume (aria2c
// writes until the system refuses, --file-allocation=none), and the files
// of the failed task would stay until the user removed it. So:
//
//   - before the download: the reserve (Options.MinFree) plus what is
//     still missing, the reported sizes (each bounded by maxReportedSize so
//     that their sum cannot overflow) less what the task's directory holds
//     already (SEC43 medium-1: a resumed download counted the whole file
//     again);
//   - while aria2c writes: at most every Options.SpaceInterval the free
//     space (below the reserve: downloads.no_space) and the size of the
//     task's directory (over Options.MaxTaskBytes: too large), and at every
//     poll the sizes the servers declare for the files in flight.
//
// What a failure deletes (discardAfter) is what it made useless, nothing
// a retry can go on with:
//
//   - no space, before the download, while it runs or in the merge: only
//     the temporary files (*.tmp) of writes the full disk cut short; the
//     partial and complete downloads and their progress stay, so that the
//     retry after freeing space resumes (the checks while downloading keep
//     them within the reserve; SEC43 medium-1 and medium-2). When some
//     stay, the failure says how much (downloads.no_space_kept);
//   - too large: everything, with the stored progress; no retry is offered
//     and the same download would fail again.

const (
	// DefaultMaxTaskBytes is the most a task's files may hold (64 GiB): a
	// lecture recording is a few hundred MB to a few GB (a 3-hour 1080p
	// screen recording at 10 Mbit/s is about 13 GB, at 4K 25 Mbit/s about
	// 34 GB), so only a live stream or a server that never stops gets
	// near it.
	DefaultMaxTaskBytes int64 = 64 << 30
	// DefaultSpaceInterval: how often a running download looks at the free
	// space and the task directory's size. At 1 Gbit/s that is 250 MB
	// past the reserve at most.
	DefaultSpaceInterval = 2 * time.Second
	// maxReportedSize bounds the size the extension reports for a track
	// (bridge.Track.SizeBytes; 1 TiB): three of them and the reserve fit in
	// an int64.
	maxReportedSize int64 = 1 << 40
)

// boundSize is a reported size within [-1, maxReportedSize] (-1: unknown).
func boundSize(n int64) int64 {
	switch {
	case n < 0:
		return -1
	case n > maxReportedSize:
		return maxReportedSize
	}
	return n
}

// tooLarge is the failure of a task over the size limit
// (downloads.too_large {max_bytes}, not retryable); its files go.
func tooLarge(n, limit int64) *failure {
	return &failure{code: CodeTooLarge, params: map[string]any{"max_bytes": limit}, discard: true,
		detail: fmt.Sprintf("the recording would hold %d bytes or more, over the %d bytes one task may fetch", n, limit)}
}

// noSpace is the failure of a run that found the free space below the
// reserve.
func (q *Queue) noSpace(free int64) *failure {
	return &failure{code: CodeNoSpace, params: map[string]any{"free_bytes": free, "keep_bytes": q.o.MinFree},
		detail: fmt.Sprintf("%d bytes free on the data directory's volume, %d to keep", free, q.o.MinFree)}
}

// overDeclared checks the bytes one fetchItems call's files will hold:
// those complete and, for the files in flight, the larger of what came
// and what the servers declared (an endless response declares nothing:
// diskWatch measures it on disk).
func (q *Queue) overDeclared(done, activeDone, activeTotal int64) error {
	limit := q.o.MaxTaskBytes
	if limit <= 0 {
		return nil
	}
	if n := done + max(activeDone, activeTotal); n > limit {
		return tooLarge(n, limit)
	}
	return nil
}

// diskWatch is a run's look at the disk while aria2c writes: the run's
// downloads (one per track, in parallel) share it, so one look serves them
// all. Once it found a problem it answers it to every later check.
type diskWatch struct {
	q    *Queue
	dir  string // the task's directory
	mu   sync.Mutex
	next time.Time
	err  error
}

func (q *Queue) newDiskWatch(dir string) *diskWatch { return &diskWatch{q: q, dir: dir} }

// check looks at most every Options.SpaceInterval; nil without a watch (a
// snapshot made outside a run).
func (w *diskWatch) check() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	now := time.Now()
	if now.Before(w.next) {
		return nil
	}
	w.next = now.Add(w.q.o.SpaceInterval)
	if w.q.o.MinFree >= 0 {
		// unknown free space does not stop the download (checkSpace)
		if free, err := w.q.o.FreeSpace(w.q.o.DataDir); err == nil && free < w.q.o.MinFree {
			w.err = w.q.noSpace(free)
			return w.err
		}
	}
	if limit := w.q.o.MaxTaskBytes; limit > 0 {
		if n := dirSize(w.dir); n > limit {
			w.err = tooLarge(n, limit)
		}
	}
	return w.err
}

// discardAfter removes what a failure made useless (see above): every file
// of the task after one marked discard (the runner then forgets the
// progress too), the temporary files after one for want of space. Not
// after a stop (ctx ended: Cancel removes the files itself, Close keeps
// them for the resume).
//
// A failure for want of space that keeps files of the task (its partial
// and complete downloads, for the resume) becomes downloads.no_space_kept
// with task_bytes, what they hold: the user learns that the task itself
// takes that much and that removing it frees it (SEC43b low-5). Measured
// here, after the temporary files went and outside the queue's lock.
func (q *Queue) discardAfter(ctx context.Context, err error, id int64, dir string) {
	var f *failure
	if err == nil || ctx.Err() != nil || !errors.As(err, &f) || f.refresh {
		return
	}
	switch {
	case f.discard:
		removeAllRetry(dir)
		q.log.Info("downloads: the task's files removed with its failure", "task", id, "code", f.code)
	case f.code == CodeNoSpace:
		if n := removeTempFiles(dir); n > 0 {
			q.log.Info("downloads: temporary files removed after the disk was full", "task", id, "files", n)
		}
		if kept := dirSize(dir); kept > 0 {
			params := map[string]any{"task_bytes": kept}
			for k, v := range f.params {
				params[k] = v
			}
			f.code, f.params = CodeNoSpaceKept, params
		}
	}
}

// fillNoSpace completes the params of a failure for want of space whose
// cause did not know the free space (a write the full disk refused,
// aria2c's error 9): free_bytes and keep_bytes, as the check's own.
func (q *Queue) fillNoSpace(f *failure) {
	if f.code != CodeNoSpace && f.code != CodeNoSpaceKept {
		return
	}
	if _, ok := f.params["free_bytes"]; ok {
		return
	}
	free, _ := q.o.FreeSpace(q.o.DataDir)
	params := map[string]any{"free_bytes": free, "keep_bytes": max(q.o.MinFree, 0)}
	for k, v := range f.params {
		params[k] = v
	}
	f.params = params
}

// removeTempFiles removes the *.tmp files under dir (writeFileAtomic's,
// the joins' and ffmpeg's outputs before their rename: what a write cut
// short leaves; nothing reads them) and returns how many.
func removeTempFiles(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".tmp") && os.Remove(p) == nil {
			n++
		}
		return nil
	})
	return n
}

// missingBytes is what the reported sizes still lack beyond the bytes the
// task's directory holds (a partial download, complete tracks), at least 0.
func missingBytes(records []trackRecord, dir string) int64 {
	var reported int64
	for _, r := range records {
		if n := boundSize(r.SizeBytes); n > 0 { // (a row from before the bound too)
			reported += n
		}
	}
	if reported == 0 {
		return 0
	}
	return max(reported-dirSize(dir), 0)
}
