// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Gate round 2: a lecture stopped after its result was downloaded, checked
// and written, but before the remote cleanup. The cleanup still runs (on a
// context of its own, with a time limit) and the runtime is released for
// the stop whatever the cleanup did.

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cancelRunningJob cancels the running lecture with cause, as stopJobs
// does, without waiting for Transcribe (it is called from Transcribe's own
// goroutine).
func cancelRunningJob(t *testing.T, b *Backend, cause error) {
	t.Helper()
	b.mu.Lock()
	var j *jobState
	if len(b.jobs) > 0 {
		j = b.jobs[0]
	}
	b.mu.Unlock()
	if j == nil {
		t.Fatal("no running lecture to stop")
	}
	j.cancel(cause)
}

// resultWritten: the progress event sent right after result.json was
// written locally (the step before the remote cleanup).
func resultWritten(ev Event) bool { return ev.Step == StepVerify && ev.Fraction == 1 }

func checkStoppedAfterResult(t *testing.T, e *testEnv, path string, err error, reason string) {
	t.Helper()
	ctx := context.Background()
	if err != nil {
		t.Fatalf("Transcribe: %v (the result was already written)", err)
	}
	if got, _ := os.ReadFile(path); !sameResult(got, e.rs.vars["result_json"]) {
		t.Fatal("result.json differs from the download")
	}
	st := e.b.Status(ctx)
	if st.State != StateReleased || st.LastRelease == nil || st.LastRelease.Reason != reason {
		t.Fatalf("status %+v (last release %+v), want released for %s", st, st.LastRelease, reason)
	}
	if v, _, _ := e.kv.GetKV(ctx, KVSession); v != "" {
		t.Errorf("session record left: %s", v)
	}
}

// TestReplayShutdownNowAfterResultCleansUpAndReleases: 立即关机 arrives
// in the window. Every remote job file is still deleted (the replay
// requires each DELETE of borrow_run_release.json), then the runtime is
// released with reason "now"; a later ShutdownNow finds nothing to do.
func TestReplayShutdownNowAfterResultCleansUpAndReleases(t *testing.T) {
	e := newEnv(t, "borrow_run_release.json").withCredential(t).startup(t)
	ctx := context.Background()
	path, err := e.b.Transcribe(ctx, e.job(t), func(ev Event) {
		if resultWritten(ev) {
			cancelRunningJob(t, e.b, errShutdownNow)
		}
	})
	checkStoppedAfterResult(t, e, path, err, ReleaseNow)
	if err := e.b.ShutdownNow(ctx); err != nil {
		t.Fatalf("ShutdownNow afterwards: %v", err)
	}
	e.rs.finish()
	e.checkClean(t)
}

// TestReplayCancelAfterResultCleansUpAndReleases: the caller's context is
// cancelled in the window (the user cancelled the lecture).
func TestReplayCancelAfterResultCleansUpAndReleases(t *testing.T) {
	e := newEnv(t, "borrow_run_release.json").withCredential(t).startup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path, err := e.b.Transcribe(ctx, e.job(t), func(ev Event) {
		if resultWritten(ev) {
			cancel()
		}
	})
	checkStoppedAfterResult(t, e, path, err, ReleaseCancelled)
	e.rs.finish()
	e.checkClean(t)
}

// TestReplayAuthEndingAfterResultReleases: Tick stops the lecture because
// the credential is about to end, in the same window.
func TestReplayAuthEndingAfterResultReleases(t *testing.T) {
	e := newEnv(t, "borrow_run_release.json").withCredential(t).startup(t)
	path, err := e.b.Transcribe(context.Background(), e.job(t), func(ev Event) {
		if resultWritten(ev) {
			cancelRunningJob(t, e.b, errAuthEnding)
		}
	})
	checkStoppedAfterResult(t, e, path, err, ReleaseAuthEnding)
	e.rs.finish()
	e.checkClean(t)
}

// TestReplayShutdownNowCallAfterResult: the real call from another
// goroutine in the window. Whichever way the race goes (the stop seen
// before, during or after the cleanup), the files are deleted, the runtime
// is released for "now" once, and ShutdownNow returns.
func TestReplayShutdownNowCallAfterResult(t *testing.T) {
	e := newEnv(t, "borrow_run_release.json").withCredential(t).startup(t)
	ctx := context.Background()
	shut := make(chan error, 1)
	var once sync.Once
	path, err := e.b.Transcribe(ctx, e.job(t), func(ev Event) {
		if resultWritten(ev) {
			once.Do(func() { go func() { shut <- e.b.ShutdownNow(ctx) }() })
		}
	})
	select {
	case serr := <-shut:
		if serr != nil {
			t.Fatalf("ShutdownNow: %v", serr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ShutdownNow did not return")
	}
	checkStoppedAfterResult(t, e, path, err, ReleaseNow)
	e.rs.finish()
	e.checkClean(t)
}

// TestReplayShutdownNowDuringCleanupRetries: result.json cannot be deleted
// and 立即关机 arrives between the retries. 3.2.0: the lecture has already
// returned (its files go in the background); the release stops the retries
// at once (the replay has no third attempt) and the runtime is released for
// "now", not as a cleanup failure.
func TestReplayShutdownNowDuringCleanupRetries(t *testing.T) {
	var written atomic.Bool
	var e *testEnv
	shut := make(chan error, 1)
	e = newEnv(t, "cleanup_stopped.json", func(o *Options) {
		sleep := o.Sleep
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			if written.Load() && d == cleanupWaits[1] {
				go func() { shut <- e.b.ShutdownNow(context.Background()) }()
				select { // the release stops the retries: this wait ends
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Second):
					t.Error("the release did not stop the retries")
				}
			}
			return sleep(ctx, d)
		}
	}).withCredential(t).startup(t)
	path, err := e.b.Transcribe(context.Background(), e.job(t), func(ev Event) {
		if resultWritten(ev) {
			written.Store(true)
		}
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	select {
	case serr := <-shut:
		if serr != nil {
			t.Fatalf("ShutdownNow: %v", serr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ShutdownNow did not return")
	}
	e.b.removalsSettled()
	checkStoppedAfterResult(t, e, path, nil, ReleaseNow)
	if st := e.b.Status(context.Background()); st.LastRelease.File != "" {
		t.Errorf("release names a file: %+v", st.LastRelease)
	}
	if logs := e.log.String(); !strings.Contains(logs, "after the lecture was stopped") || strings.Contains(logs, "cleanup_failed") {
		t.Errorf("log:\n%s", logs)
	}
	e.rs.finish()
	e.checkClean(t)
}
