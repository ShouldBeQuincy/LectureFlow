// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// The disk while a task downloads (AUD3 low-2): a server must not be able
// to fill the data directory's disk, nor leave what it sent there until
// the user removes the task.

// filesIn lists the regular files under dir (none when it is gone).
func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

// bytesIn is the bytes of the regular files under dir.
func bytesIn(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	for _, p := range filesIn(t, dir) {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		n += fi.Size()
	}
	return n
}

// wantNoSpaceKept: the task failed with downloads.no_space_kept, the free
// space and the reserve as for no_space, and task_bytes the bytes its
// directory keeps (SEC43b low-5: the text tells how much removing the
// task frees).
func wantNoSpaceKept(t *testing.T, e *qenv, id int64, info TaskInfo, free, keep int64) {
	t.Helper()
	kept := bytesIn(t, e.q.taskDir(id))
	if info.Error == nil || info.Error.Code != CodeNoSpaceKept || info.Error.Params["free_bytes"] != float64(free) ||
		info.Error.Params["keep_bytes"] != float64(keep) || kept <= 0 || info.Error.Params["task_bytes"] != float64(kept) {
		t.Fatalf("%+v, want %s with task_bytes %d", info.Error, CodeNoSpaceKept, kept)
	}
	if !Retryable(CodeNoSpaceKept) || !slices.Equal(info.AllowedActions, []string{ActionRetry, ActionRemove}) {
		t.Fatalf("actions %v", info.AllowedActions)
	}
}

// The free space falls below the reserve while aria2c writes: the task
// fails at once (not when the disk is full), with downloads.no_space_kept
// as the partial download stays. What the failure made useless goes (a
// temporary file); the partial download and its progress stay, and a
// retry once there is room resumes it (SEC43 medium-1: it used to be
// deleted and downloaded anew).
func TestNoSpaceWhileDownloading(t *testing.T) {
	var low atomic.Bool
	e := newQEnv(t, func(o *Options) {
		o.MinFree = 1 << 30
		o.SpaceInterval = 10 * time.Millisecond
		o.FreeSpace = func(string) (int64, error) {
			if low.Load() {
				return 512 << 20, nil
			}
			return 100 << 30, nil
		}
	})
	ctx := context.Background()
	video := pattern(3, 400_000)
	e.media.put("/files/v.mp4", video)
	e.media.stall("/files/v.mp4")
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	e.waitFor("the first bytes", func() bool { return e.aria.completed("/files/v.mp4") > 0 })
	// a temporary file a write the full disk refused would leave
	stray := filepath.Join(e.q.taskDir(id), "plan.id.tmp")
	if err := os.WriteFile(stray, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	low.Store(true)
	info := e.wait(id, bridge.StateFailed)
	wantNoSpaceKept(t, e, id, info, 512<<20, 1<<30)
	if fileExists(stray) {
		t.Fatal("the temporary file stayed")
	}
	if !fileExists(filepath.Join(e.q.taskDir(id), "t0.mp4.part")) {
		t.Fatalf("the partial download is gone: %v", filesIn(t, e.q.taskDir(id)))
	}
	if rec := e.row(id); rec.BytesDone <= 0 {
		t.Fatalf("stored progress %d/%d", rec.BytesDone, rec.BytesTotal)
	}
	if !slices.Equal(info.AllowedActions, []string{ActionRetry, ActionRemove}) {
		t.Fatalf("actions %v", info.AllowedActions)
	}
	// room again: the retry resumes where the download stopped
	low.Store(false)
	e.media.release()
	if _, err := e.q.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	info = e.wait(id, bridge.StateDone)
	if !bytes.Equal(readAll(t, e.lecture(info.LectureID).MediaPath), video) {
		t.Fatal("media differs")
	}
	assertResumed(t, e, "/files/v.mp4")
}

// assertResumed: the last request for path asked for the bytes after
// those on disk, with the session.
func assertResumed(t *testing.T, e *qenv, path string) {
	t.Helper()
	reqs := e.media.requests(path)
	if len(reqs) == 0 {
		t.Fatal("no request")
	}
	if last := reqs[len(reqs)-1]; last.Method != http.MethodGet || last.Range == "" || last.Range == "bytes=0-" || last.Cookie != testCookie {
		t.Fatalf("not resumed: %+v", last)
	}
}

// SEC43 medium-1: the check before a download that resumes counts what is
// still missing, not the whole file again. 1,250,000 bytes free hold the
// reserve (1,000,000) and the 200,000 bytes missing, not the reserve and
// the whole file (1,400,000): the download resumes and finishes.
func TestResumeWhenTheRestFits(t *testing.T) {
	var free atomic.Int64
	free.Store(100 << 30)
	e := newQEnv(t, func(o *Options) {
		o.MinFree = 1_000_000
		o.SpaceInterval = 10 * time.Millisecond
		o.FreeSpace = func(string) (int64, error) { return free.Load(), nil }
	})
	video := pattern(4, 400_000)
	e.media.put("/files/v.mp4", video)
	e.media.hold("/files/v.mp4") // half, then nothing
	tr := e.fileTrack(bridge.KindVideo, "/files/v.mp4")
	tr.SizeBytes = int64(len(video))
	id := e.enqueue(e.request("s1", tr))
	e.waitFor("half of the file", func() bool { return e.aria.completed("/files/v.mp4") >= int64(len(video)/2) })
	e.restart()
	e.wait(id, bridge.StateNeedsRefresh)
	e.media.release()
	free.Store(1_250_000)
	if _, err := e.q.Refresh(context.Background(), &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1",
		Tracks: []bridge.Track{tr}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
		t.Fatal(err)
	}
	info := e.wait(id, bridge.StateDone)
	if !bytes.Equal(readAll(t, e.lecture(info.LectureID).MediaPath), video) {
		t.Fatal("media differs")
	}
	assertResumed(t, e, "/files/v.mp4")
}

// A failure for want of space before the download deletes nothing: the
// files there are the progress of the earlier runs, which the checks while
// downloading kept within the reserve (SEC43 medium-1); the reserve and
// the free space are told as before, and what the task keeps
// (downloads.no_space_kept).
func TestNoSpaceBeforeDownloadKeepsPartialFiles(t *testing.T) {
	var low atomic.Bool
	e := newQEnv(t, func(o *Options) {
		o.MinFree = 1 << 30
		o.SpaceInterval = time.Hour // only the check before the download
		o.FreeSpace = func(string) (int64, error) {
			if low.Load() {
				return 512 << 20, nil
			}
			return 100 << 30, nil
		}
	})
	video := pattern(4, 200_000)
	e.media.put("/files/v.mp4", video)
	e.media.hold("/files/v.mp4")
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	e.waitFor("half of the file", func() bool { return e.aria.completed("/files/v.mp4") >= int64(len(video)/4) })
	e.restart()
	e.wait(id, bridge.StateNeedsRefresh)
	before := filesIn(t, e.q.taskDir(id))
	if len(before) == 0 {
		t.Fatal("no partial file to resume from")
	}
	stored := e.row(id).BytesDone
	e.media.release()
	low.Store(true)
	refresh := func() {
		t.Helper()
		if _, err := e.q.Refresh(context.Background(), &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1",
			Tracks: []bridge.Track{e.fileTrack(bridge.KindVideo, "/files/v.mp4")}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	info := e.wait(id, bridge.StateFailed)
	wantNoSpaceKept(t, e, id, info, 512<<20, 1<<30)
	if left := filesIn(t, e.q.taskDir(id)); !slices.Equal(left, before) {
		t.Fatalf("files %v, before %v", left, before)
	}
	if rec := e.row(id); rec.BytesDone != stored {
		t.Fatalf("stored progress %d, before %d", rec.BytesDone, stored)
	}
	// room again: the retry resumes
	low.Store(false)
	if _, err := e.q.Retry(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	info = e.wait(id, bridge.StateDone)
	if !bytes.Equal(readAll(t, e.lecture(info.LectureID).MediaPath), video) {
		t.Fatal("media differs")
	}
	assertResumed(t, e, "/files/v.mp4")
}

// diskFullError is the system's error for a full disk (isDiskFull).
func diskFullError() error {
	if runtime.GOOS == "windows" {
		return syscall.Errno(112) // ERROR_DISK_FULL
	}
	return syscall.ENOSPC
}

// SEC43 medium-2: the disk fills up while a DASH track's segments are
// joined. The downloads are complete: they stay, ready.json with them (it
// went, and the next run downloaded every segment again), and the retry
// after freeing space joins them without the network.
func TestDASHJoinOnAFullDiskKeepsTheSegments(t *testing.T) {
	e := newQEnv(t, nil)
	dashFixture(e, "d", "")
	fr := append(append(pattern(40, 600), pattern(41, 2000)...), pattern(42, 2000)...)
	var full atomic.Bool
	full.Store(true)
	joinCopy = func(dst io.Writer, src io.Reader) (int64, error) {
		if full.Load() {
			return 0, &os.PathError{Op: "write", Path: "joined.tmp", Err: diskFullError()}
		}
		return io.Copy(dst, src)
	}
	t.Cleanup(func() { joinCopy = io.Copy })
	id := e.enqueue(e.request("s1", e.dashTrack(bridge.KindAudio, "/d/manifest.mpd", "audio:fr:0")))
	info := e.wait(id, bridge.StateFailed)
	// (the merge's disk-full error carries no free space: the queue reads
	// the disk's, and this test keeps no reserve)
	free, _ := info.Error.Params["free_bytes"].(float64)
	wantNoSpaceKept(t, e, id, info, int64(free), max(e.opts.MinFree, 0))
	dir := e.q.taskDir(id)
	if !fileExists(filepath.Join(dir, "t0", "ready.json")) {
		t.Fatalf("ready.json is gone: %v", filesIn(t, dir))
	}
	if segs, _ := filepath.Glob(filepath.Join(dir, "t0", "s*.seg")); len(segs) != 2 {
		t.Fatalf("segments %v", segs)
	}
	adds, _, _, _, _ := e.aria.snapshot()
	mpd := len(e.media.requests("/d/manifest.mpd"))
	full.Store(false)
	if _, err := e.q.Retry(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	info = e.wait(id, bridge.StateDone)
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, fr) {
		t.Fatalf("media %d bytes, want %d", len(got), len(fr))
	}
	if after, _, _, _, _ := e.aria.snapshot(); len(after) != len(adds) || len(e.media.requests("/d/manifest.mpd")) != mpd {
		t.Fatalf("the retry downloaded again: %d downloads after %d, MPD read %d times after %d",
			len(after), len(adds), len(e.media.requests("/d/manifest.mpd")), mpd)
	}
}

// A file larger than a task may hold (the size the server declares) fails
// at once, before it is written out: a failure no retry fixes, whose files
// go with it.
func TestTaskTooLargeDeclared(t *testing.T) {
	e := newQEnv(t, func(o *Options) { o.MaxTaskBytes = 100_000 })
	e.media.put("/files/v.mp4", pattern(5, 300_000))
	e.media.stall("/files/v.mp4") // 64 KB, then nothing: the declared length decides
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	info := e.wait(id, bridge.StateFailed)
	assertTooLarge(t, e, id, info)
}

// A response without an end (no length declared: a live stream, or a
// server that never stops) stops at the size limit, measured on disk.
func TestTaskTooLargeEndless(t *testing.T) {
	var sent atomic.Int64
	endless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		if r.Method == http.MethodHead {
			return // (Go's redirect probe: not counted)
		}
		chunk := pattern(6, 16<<10)
		// bounded (32 MiB) so that a test of a queue without the limit ends
		for i := 0; i < 2048; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			sent.Add(int64(len(chunk)))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}))
	t.Cleanup(endless.Close)
	e := newQEnv(t, func(o *Options) {
		o.MaxTaskBytes = 256 << 10
		o.SpaceInterval = 10 * time.Millisecond
	})
	tr := bridge.Track{URL: endless.URL + "/live", Kind: bridge.KindVideo, Format: bridge.FormatFile, Role: "video", SizeBytes: -1}
	id := e.enqueue(e.request("s1", tr))
	info := e.wait(id, bridge.StateFailed)
	assertTooLarge(t, e, id, info)
	if n := sent.Load(); n >= 2048*16<<10 {
		t.Fatalf("the server sent all of its %d bytes", n)
	}
}

// Two tracks downloaded at once, each within the limit (70,000 bytes each,
// no length declared; the limit 120,000), together over it on disk: only
// the size of the task's directory sees that (SEC43 low-2: no test
// reached that check). The sizes are each file's own: on Windows the
// directory listing's lag behind for a file aria2c still writes.
func TestTaskTooLargeAcrossTracks(t *testing.T) {
	// each file: no length declared, 70,000 bytes sent, then nothing until
	// the test ends
	stop := make(chan struct{})
	part := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		if r.Method == http.MethodHead {
			return
		}
		for k := 0; k < 7; k++ {
			w.Write(pattern(byte(k), 10_000))
			w.(http.Flusher).Flush()
		}
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(stop); part.Close() })
	e := newQEnv(t, func(o *Options) {
		o.MaxTaskBytes = 120_000
		o.SpaceInterval = 10 * time.Millisecond
	})
	track := func(kind, p string) bridge.Track {
		return bridge.Track{URL: part.URL + p, Kind: kind, Format: bridge.FormatFile, Role: kind, SizeBytes: -1}
	}
	id := e.enqueue(e.request("s1", track(bridge.KindVideo, "/v.mp4"), track(bridge.KindAudio, "/a.m4a")))
	info := e.wait(id, bridge.StateFailed)
	assertTooLarge(t, e, id, info)
}

// assertTooLarge: the task failed with downloads.too_large {max_bytes}
// (its own text, SEC43 info-4: not a "live stream or format" one), no
// retry offered, no file left, nothing imported.
func assertTooLarge(t *testing.T, e *qenv, id int64, info TaskInfo) {
	t.Helper()
	if info.Error == nil || info.Error.Code != CodeTooLarge || info.Error.Params["max_bytes"] != float64(e.opts.MaxTaskBytes) {
		t.Fatalf("%+v", info.Error)
	}
	if Retryable(CodeTooLarge) {
		t.Fatal("too_large is retryable")
	}
	if !slices.Equal(info.AllowedActions, []string{ActionRemove}) {
		t.Fatalf("actions %v", info.AllowedActions)
	}
	if left := filesIn(t, e.q.taskDir(id)); len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
	if len(e.importsSnapshot()) != 0 {
		t.Fatal("imported")
	}
}

// The sizes the extension reports are estimates: absurd ones are bounded,
// so that the check before the download cannot overflow and pass.
func TestReportedSizeBounded(t *testing.T) {
	huge := int64(1) << 62
	tracks, err := checkTracks([]bridge.Track{
		{URL: "https://media.example.edu/a.m4a", Kind: bridge.KindAudio, Format: bridge.FormatFile, SizeBytes: huge},
		{URL: "https://media.example.edu/v.mp4", Kind: bridge.KindVideo, Format: bridge.FormatFile, SizeBytes: huge},
		{URL: "https://media.example.edu/w.mp4", Kind: bridge.KindVideo, Format: bridge.FormatFile, SizeBytes: -7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tracks[0].rec.SizeBytes != maxReportedSize || tracks[1].rec.SizeBytes != maxReportedSize || tracks[2].rec.SizeBytes != -1 {
		t.Fatalf("%d %d %d", tracks[0].rec.SizeBytes, tracks[1].rec.SizeBytes, tracks[2].rec.SizeBytes)
	}
	// 2 TiB + the reserve do not fit in 10 GiB (unbounded, the two sizes
	// added up to a negative number and the check passed)
	q := New(Options{MinFree: 2 << 30, FreeSpace: func(string) (int64, error) { return 10 << 30, nil }, DataDir: t.TempDir()})
	recs := []trackRecord{tracks[0].rec, tracks[1].rec, tracks[2].rec}
	var f *failure
	if err := q.checkSpace(runSnapshot{records: recs}, t.TempDir()); !errors.As(err, &f) || f.code != CodeNoSpace {
		t.Fatalf("check before the download: %v", err)
	}
}
