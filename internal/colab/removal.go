// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Removing a finished lecture's files from the runtime (3.2.0 fix after the
// accept-320 run: the first lecture's cleanup took about 42 s — 13 slow
// DELETE api/contents, most of them hitting the 8 s timeout — a third of
// the lecture, while the runtime stayed occupied; the same proxy stalls
// colab-batch recorded).
//
//   - In the background: once result.json is downloaded, checked and
//     written, the lecture is done (Transcribe returns); its directory is
//     removed meanwhile. A release waits for the removals under way (at most
//     removalReleaseWait) and stops their retries, so the runtime is held no
//     longer than before and every lecture's files go before the runtime is
//     given back — or with it, when a removal cannot finish.
//   - In one step: a cell in the lecture's own kernel (idle once its
//     attaching cell has ended) removes the whole directory with
//     shutil.rmtree — one kernel channel round trip instead of nine DELETEs
//     through the runtime proxy (0.3 s on a T4, 2026-10-27). When the cell
//     fails or says the directory is still there, the files are deleted one
//     by one through the Contents API with the retries as before
//     (cleanupRemote).
//   - A file that still cannot be deleted releases the runtime once no
//     lecture runs on it (cleanup_failed, Gate round 1 item 8: the rule of
//     cleanupKept, unchanged).
//
// Checked on a real runtime (2026-10-27): Colab's Jupyter server deletes
// through the Contents API for good — no trash directory appears on the
// persistent disk or on a tmpfs, and a non-empty directory is refused
// (Jupyter's class default delete_to_trash = True is overridden on Colab) —
// so the old path did not leave the memory mode's files on the disk; the
// cell is about speed and fewer proxy requests.
//
// Failed and cancelled lectures keep their rules (lectureFailed,
// cleanupKept): their files go through the Contents API at once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// removeCellTimeout bounds the removal cell (it normally takes well
	// under a second; kernelExec reconnects a broken channel within it).
	removeCellTimeout = 20 * time.Second
	removedMark       = "LECTUREFLOW_REMOVED"
	leftMark          = "LECTUREFLOW_LEFT"
)

// removalReleaseWait: a release waits this long for the removals under
// way, then goes on (the runtime's storage goes with it). A variable for
// the tests.
var removalReleaseWait = cleanupStoppedTimeout

// removal is a finished lecture's directory being removed in the
// background.
type removal struct {
	stop context.CancelFunc // no more retries (a release is under way)
	done chan struct{}      // closed when it has ended (before any release it starts)
}

// removeCellCode removes jobDir (a Contents API path, BASE/jobs/ID) with
// shutil.rmtree when it is a job directory of base, and prints one mark:
// removedMark when it is gone, leftMark otherwise. Nothing else reaches
// stdout.
func removeCellCode(base, jobDir string) string {
	q := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	return fmt.Sprintf(`import os, shutil
_lf_base = %s
_lf_dir = %s
if os.path.dirname(_lf_dir) == os.path.join(_lf_base, 'jobs') and os.path.basename(_lf_dir):
    shutil.rmtree(_lf_dir, ignore_errors=True)
print(%q if not os.path.lexists(_lf_dir) else %q)
`, q("/"+base), q("/"+jobDir), removedMark, leftMark)
}

// startRemoval removes the finished lecture's job directory in the
// background with its kernel kid (given back afterwards).
func (b *Backend) startRemoval(s *liveSession, kid, base, jobDir string) {
	stopCtx, stop := context.WithCancel(context.Background())
	r := &removal{stop: stop, done: make(chan struct{})}
	b.mu.Lock()
	s.removals = append(s.removals, r)
	b.mu.Unlock()
	b.markBusyDir(s, jobDir)
	b.removalWG.Add(1)
	go func() {
		defer b.removalWG.Done()
		defer stop()
		start := b.now()
		how, left, stopped, kernelOK := b.removeJob(stopCtx, s, kid, base, jobDir)
		// left is "" also when a release stopped the retries: gone only
		// when the cell confirmed it or the deletions ran to their end.
		b.settleBusyDir(s, jobDir, how == "cell" || (left == "" && stopCtx.Err() == nil))
		b.giveKernel(s, kid, kernelOK)
		b.mu.Lock()
		for i, x := range s.removals {
			if x == r {
				s.removals = append(s.removals[:i:i], s.removals[i+1:]...)
				break
			}
		}
		live := b.sess == s
		if live && left != "" {
			s.leftFile = left
		}
		if live && stopped != "" && s.rec.State == StateRunning {
			// The release that stopped the retries waits for this removal
			// and may still be skipped because a lecture was dispatched
			// meanwhile (ifIdle): the file would stay on a runtime that goes
			// on, with nothing recording it (review R52 M1). Recorded, the
			// left-file rule takes over (the end of the last lecture, or
			// Tick once none is on the runtime); when
			// that release does happen, the record goes with the
			// session. No release of its own here: one is under way.
			s.leftFile = stopped
		}
		idle := len(b.jobs) == 0
		b.mu.Unlock()
		close(r.done)
		b.log.Info("colab: a finished lecture's files on the runtime were removed", "how", how,
			"ms", b.now().Sub(start).Milliseconds(), "left", left != "" || stopped != "")
		if live && left != "" && idle {
			// Gate round 1 item 8: release now, its storage goes with it
			// (with lectures running, the last of them releases it).
			_ = b.releaseWith(context.Background(), ReleaseCleanupFailed, releaseOpts{file: left, expect: s, ifIdle: true})
		}
	}()
}

// removeJob removes jobDir: the cell first, the Contents API when it does
// not confirm. how is "cell" or "contents"; left names a file that could
// not be deleted ("" = all gone, or the removal was stopped); stopped names
// a file whose single deletion failed after a release stopped the retries;
// kernelOK is false when the kernel may still run the cell (not used
// again).
func (b *Backend) removeJob(stopCtx context.Context, s *liveSession, kid, base, jobDir string) (how, left, stopped string, kernelOK bool) {
	removed, kernelOK, err := b.removeByCell(stopCtx, s, kid, base, jobDir)
	if removed {
		return "cell", "", "", kernelOK
	}
	b.log.Info("colab: removing a lecture's files with a cell did not work; deleting them one by one",
		"code", Code(codedHTTP("api/kernels", err)))
	left, stopped = b.cleanupRemoteStopped(stopCtx, s, jobDir, true)
	return "contents", left, stopped, kernelOK
}

// removeByCell runs removeCellCode in kernel kid. It is not stopped by
// stopCtx (one attempt always runs; a release waits for it), only bounded
// by removeCellTimeout.
func (b *Backend) removeByCell(stopCtx context.Context, s *liveSession, kid, base, jobDir string) (removed, kernelOK bool, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(stopCtx), removeCellTimeout)
	defer cancel()
	rt, err := b.runtime(ctx, s)
	if err != nil {
		return false, true, err
	}
	var out strings.Builder
	res, err := rt.kernelExec(ctx, kid, removeCellCode(base, jobDir), func(text string) error {
		if out.Len()+len(text) > 256 {
			return errors.New("the removal cell printed more than its mark")
		}
		out.WriteString(text)
		return nil
	}, b.newReconnector(nil), nil)
	if err != nil {
		return false, false, err
	}
	if res.Status != "ok" {
		return false, true, fmt.Errorf("the removal cell ended with status %s", safeToken(res.Status))
	}
	if strings.TrimSpace(out.String()) != removedMark {
		return false, true, errors.New("the removal cell did not confirm")
	}
	return true, true, nil
}

// awaitRemovals stops the retries of the removals under way on s and
// waits for them, at most removalReleaseWait (a release, before it shuts
// the kernels down).
func (b *Backend) awaitRemovals(s *liveSession) {
	b.mu.Lock()
	rs := append([]*removal(nil), s.removals...)
	b.mu.Unlock()
	if len(rs) == 0 {
		return
	}
	deadline := time.NewTimer(removalReleaseWait)
	defer deadline.Stop()
	for _, r := range rs {
		r.stop()
	}
	for _, r := range rs {
		select {
		case <-r.done:
		case <-deadline.C:
			b.log.Info("colab: releasing the runtime before a lecture's files were removed (its storage goes with it)")
			return
		}
	}
}

// removalsSettled waits until every background removal (and the release it
// may start) has ended (tests).
func (b *Backend) removalsSettled() { b.removalWG.Wait() }

// ---- one directory per attempt while an earlier one is being removed ----
//
// T051 (test review R3): a lecture's job directory is named after the
// lecture (remoteJobID), and its files are deleted in the background after
// a success (startRemoval) or a failure that keeps the runtime
// (cleanupKept), with retries that may take a minute. The same lecture can
// be dispatched to the same runtime again meanwhile (a retry, "改用 Colab
// 处理"): in the same directory, the deletion under way would delete its
// new upload or job.json, and its new job.json would meet the previous
// attempt's (the resident worker queues a directory name once while its
// job.json stays). So a directory being deleted, or left with a file, is
// marked busy until it is known gone, and an attempt whose directory is
// busy takes the lecture's id with a random suffix — the naming of the
// uploads ahead (aheadID). Nothing waits; a deletion only ever deletes its
// own directory. The worker finds jobs by BASE/jobs/*/job.json and the
// memory mode by BASE/jobs/ID's grandparent, whatever the name.

// markBusyDir marks jobDir busy on s; call it before the lecture's
// Transcribe returns (before a goroutine that deletes it starts).
func (b *Backend) markBusyDir(s *liveSession, jobDir string) {
	if jobDir == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.busyDirs == nil {
		s.busyDirs = map[string]bool{}
	}
	s.busyDirs[jobDir] = true
}

// settleBusyDir ends the mark of jobDir when its deletion has ended with
// every file gone; a directory that keeps a file stays busy.
func (b *Backend) settleBusyDir(s *liveSession, jobDir string, gone bool) {
	if !gone || jobDir == "" {
		return
	}
	b.mu.Lock()
	delete(s.busyDirs, jobDir)
	b.mu.Unlock()
}

// jobDirFor names lecture j's job directory under base on s: BASE/jobs/ID,
// or BASE/jobs/ID-xxxxxx when that one is busy (j.id follows).
func (b *Backend) jobDirFor(s *liveSession, base string, j *jobState) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	dir := base + "/jobs/" + j.id
	for s.busyDirs[dir] {
		j.id = aheadID(j.key)
		dir = base + "/jobs/" + j.id
	}
	return dir
}
