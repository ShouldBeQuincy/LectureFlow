// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// SEC2 T3 (r4): the idle-type releases (Tick's queue drained, after
// current, idle limit, window used up; the end of a lecture's after
// current, queue drained, "0 = right after each lecture") decide on a
// snapshot, and the release waits for the finished lectures' removals
// before it sets the session stopping. A lecture dispatched to the runtime
// in between keeps it (ifIdle) instead of being withdrawn with it. These
// tests hold the release in that gap with the removal gate of the stateful
// fake, on the fake clock, and prove: (a) the lecture dispatched meanwhile
// goes on and the runtime stays; (b) with nothing new the release happens
// exactly when it did before; (c) when the lecture that kept the runtime
// leaves without using it, the next Tick (30 s, Run) releases it — that is
// the most the runtime is held longer than before.

import (
	"context"
	"testing"
	"time"
)

// tickEvery is Run's Tick interval.
const tickEvery = 30 * time.Second

// holdRemovals makes the next removal cell wait until the returned gate is
// closed; arrived is closed when it got there.
func (e *fakeEnv) holdRemovals() (gate, arrived chan struct{}) {
	gate, arrived = make(chan struct{}), make(chan struct{})
	e.f.mu.Lock()
	e.f.removeGate, e.f.removing = gate, arrived
	e.f.mu.Unlock()
	return gate, arrived
}

// removalArrived waits for the held removal cell; the later ones do not
// report (arrived is closed once).
func (e *fakeEnv) removalArrived(t *testing.T, arrived chan struct{}) {
	t.Helper()
	waitClosed(t, arrived, "the removal cell")
	e.f.mu.Lock()
	e.f.removing = nil
	e.f.mu.Unlock()
}

// waitReleasing waits until a release holds relMu (it then waits for the
// held removal: past its snapshot, before it sets the session stopping).
func (e *fakeEnv) waitReleasing(t *testing.T) {
	t.Helper()
	e.waitFor(t, "a release under way", func() bool {
		if e.b.relMu.TryLock() {
			e.b.relMu.Unlock()
			return false
		}
		return true
	})
}

func (e *fakeEnv) session() *liveSession {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	return e.b.sess
}

func (e *fakeEnv) jobsOn() int {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	return len(e.b.jobs)
}

// phantom puts a lecture on the backend that never gets to run (its
// Transcribe leaves before the runtime is used: a dispatch decision, a
// cancel); the returned func takes it off as that Transcribe's end does.
func (e *fakeEnv) phantom() func() {
	j := &jobState{id: "phantom", key: "phantom", cancel: func(error) {}, done: make(chan struct{})}
	e.b.mu.Lock()
	e.b.jobs = append(e.b.jobs, j)
	e.b.mu.Unlock()
	return func() {
		e.b.mu.Lock()
		for i, x := range e.b.jobs {
			if x == j {
				e.b.jobs = append(e.b.jobs[:i:i], e.b.jobs[i+1:]...)
				break
			}
		}
		e.b.mu.Unlock()
		close(j.done)
	}
}

func (e *fakeEnv) wantRunning(t *testing.T, what string) {
	t.Helper()
	if st := e.b.Status(context.Background()); st.State != StateRunning || e.f.wasReleased() {
		t.Fatalf("%s: state %s, last release %+v, released %v; want the runtime kept", what, st.State, st.LastRelease, e.f.wasReleased())
	}
}

func (e *fakeEnv) wantReleased(t *testing.T, what, reason string) {
	t.Helper()
	st := e.b.Status(context.Background())
	if st.State != StateReleased || st.LastRelease == nil || st.LastRelease.Reason != reason || !e.f.wasReleased() {
		t.Fatalf("%s: state %s, last release %+v, released %v; want released (%s)", what, st.State, st.LastRelease, e.f.wasReleased(), reason)
	}
}

// (a) and (b) for Tick: the idle limit is reached and Tick's release waits
// for the last lecture's removal; lecture b is dispatched meanwhile. b runs
// on the runtime, which stays; once b is over the idle limit counts from
// b's end and Tick releases it then. (Red before r4: Tick released the
// runtime b had just been given.)
func TestTickIdleReleaseKeepsALectureDispatchedMeanwhile(t *testing.T) {
	ctx := context.Background()
	e := newFakeEnv(t, t4()) // idle limit 5 min
	gate, arrived := e.holdRemovals()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "a"), nil); err != nil {
		t.Fatalf("a: %v", err)
	}
	e.removalArrived(t, arrived)
	s0 := e.session()
	e.clk.Advance(6 * time.Minute)
	ticked := make(chan struct{})
	go func() { e.b.Tick(ctx); close(ticked) }()
	e.waitReleasing(t)
	db := startLecture(e, ctx, e.lecture(t, "b"))
	e.waitFor(t, "b on the backend", func() bool { return e.jobsOn() == 1 })
	close(gate)
	waitClosed(t, ticked, "Tick")
	e.wantRunning(t, "Tick with b dispatched")
	if err := waitErr(t, db); err != nil {
		t.Fatalf("b: %v", err)
	}
	if e.session() != s0 {
		t.Error("b ran on another session, want a's")
	}
	e.b.removalsSettled()
	e.clk.Advance(4 * time.Minute)
	e.b.Tick(ctx)
	e.wantRunning(t, "4 min after b")
	e.clk.Advance(time.Minute)
	e.b.Tick(ctx)
	e.wantReleased(t, "5 min after b", ReleaseIdle)
}

// R52 L1 (billing): a finished lecture's file is recorded on a running,
// otherwise idle runtime (idle limit far off). Tick releases it for the
// file — not while a lecture is on the runtime (one dispatched while that
// release waited keeps it; a Tick with it there does nothing), and at the
// very next Tick (30 s) once that lecture left without using it. (Red
// before: Tick had no left-file reason and kept the runtime until the idle
// limit, up to 60 min.)
func TestTickReleasesForALeftFile(t *testing.T) {
	ctx := context.Background()
	set := t4()
	set.IdleReleaseMin = 60
	e := newFakeEnv(t, set)
	gate, arrived := e.holdRemovals()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "a"), nil); err != nil {
		t.Fatalf("a: %v", err)
	}
	e.removalArrived(t, arrived)
	e.b.mu.Lock()
	e.b.sess.leftFile = "result.json" // as cleanupKept or a stopped removal records it
	e.b.mu.Unlock()
	ticked := make(chan struct{})
	go func() { e.b.Tick(ctx); close(ticked) }()
	e.waitReleasing(t)
	leave := e.phantom()
	close(gate)
	waitClosed(t, ticked, "Tick")
	e.wantRunning(t, "Tick with a lecture dispatched")
	e.clk.Advance(tickEvery)
	e.b.Tick(ctx)
	e.wantRunning(t, "a Tick while the lecture is on the runtime")
	leave()
	e.clk.Advance(tickEvery)
	e.b.Tick(ctx)
	e.wantReleased(t, "the next Tick after the lecture left", ReleaseCleanupFailed)
	if st := e.b.Status(ctx); st.LastRelease.File != "result.json" {
		t.Errorf("last release %+v, want the file named", st.LastRelease)
	}
}

// The window's release keeps its cooldown when a file was left too.
func TestTickWindowReleaseWinsOverALeftFile(t *testing.T) {
	ctx := context.Background()
	set := t4()
	set.IdleReleaseMin = 60
	e := newFakeEnv(t, set)
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "a"), nil); err != nil {
		t.Fatalf("a: %v", err)
	}
	e.b.removalsSettled()
	e.b.mu.Lock()
	e.b.sess.leftFile = "result.json"
	e.b.mu.Unlock()
	e.clk.Advance(FairWindow)
	e.b.Tick(ctx)
	e.wantReleased(t, "the window used up", ReleaseWindow)
	if st := e.b.Status(ctx); st.CooldownUntil == nil {
		t.Error("no cooldown after the window's release")
	}
}

// R52 L3: the credential's end releases the runtime even when a lecture
// was dispatched while that release waited — kept, the runtime could not be
// released once the credential has ended (no ifIdle for it).
func TestTickAuthEndingReleasesRegardless(t *testing.T) {
	ctx := context.Background()
	set := t4()
	set.IdleReleaseMin = 60
	e := newFakeEnv(t, set)
	// A 1-day credential imported 23 hours ago: one hour left.
	storeCredential(t, e.kv, Lifecycle{Kind: LifecycleDays, Days: 1}, e.clk.Now().Add(-23*time.Hour), "run-1")
	gate, arrived := e.holdRemovals()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "a"), nil); err != nil {
		t.Fatalf("a: %v", err)
	}
	e.removalArrived(t, arrived)
	e.clk.Advance(51 * time.Minute) // 9 minutes left: less than the 10-minute release margin
	ticked := make(chan struct{})
	go func() { e.b.Tick(ctx); close(ticked) }()
	e.waitReleasing(t)
	leave := e.phantom()
	defer leave()
	close(gate)
	waitClosed(t, ticked, "Tick")
	e.wantReleased(t, "the credential's end with a lecture dispatched", ReleaseAuthEnding)
}

func (e *fakeEnv) leftFile() string {
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	if e.b.sess == nil {
		return ""
	}
	return e.b.sess.leftFile
}

// transcribingUntil waits until lecture name starts transcribing (the
// names of earlier lectures still in the channel are skipped).
func (e *fakeEnv) transcribingUntil(t *testing.T, name string) {
	t.Helper()
	for i := 0; i < 16; i++ {
		if e.transcribingNow(t) == name {
			return
		}
	}
	t.Fatalf("%s never started transcribing", name)
}

// R52 M1: the release Tick skips for lecture b had already stopped the
// retries of a's removal; the cell did not confirm and the one deletion of
// a's result.json failed. The file is recorded (leftFile) instead of
// staying unnoticed on the runtime b goes on with, and b's end releases the
// runtime for it. (Red before: leftFile empty, the runtime kept after b
// with a's result.json on it.)
func TestSkippedReleaseRecordsTheFileItsStopLeft(t *testing.T) {
	ctx := context.Background()
	e := newFakeEnv(t, t4()) // idle limit 5 min
	e.f.mu.Lock()
	e.f.removeLeft, e.f.failDelete = true, "jobs/a/result.json"
	e.f.mu.Unlock()
	gate, arrived := e.holdRemovals()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "a"), nil); err != nil {
		t.Fatalf("a: %v", err)
	}
	e.removalArrived(t, arrived)
	e.clk.Advance(6 * time.Minute)
	ticked := make(chan struct{})
	go func() { e.b.Tick(ctx); close(ticked) }()
	e.waitReleasing(t)
	e.f.hold("b")
	db := startLecture(e, ctx, e.lecture(t, "b"))
	e.waitFor(t, "b on the backend", func() bool { return e.jobsOn() == 1 })
	close(gate)
	waitClosed(t, ticked, "Tick")
	e.transcribingUntil(t, "b")
	e.wantRunning(t, "Tick with b dispatched")
	const aResult = base + "/jobs/a/result.json"
	if !e.f.has(aResult) {
		t.Fatal("the probe's premise failed: a's result.json is gone")
	}
	if got := e.leftFile(); got != "result.json" {
		t.Errorf("leftFile %q while a's result.json stays on the runtime, want it recorded", got)
	}
	e.f.release("b")
	if err := waitErr(t, db); err != nil {
		t.Fatalf("b: %v", err)
	}
	e.wantReleased(t, "b's end", ReleaseCleanupFailed)
	if st := e.b.Status(ctx); st.LastRelease.File != "result.json" {
		t.Errorf("last release %+v, want the file named", st.LastRelease)
	}
}

// (c) for every Tick reason that waits: the lecture that kept the runtime
// leaves without using it. The next Tick releases it for the same reason —
// at most one Tick interval later than before. (Red before r4 at the first
// check: the release went on regardless of the lecture.)
func TestTickIdleReleaseAfterTheLectureLeftComesWithTheNextTick(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		arm          func(e *fakeEnv)
	}{
		{"idle", ReleaseIdle, func(e *fakeEnv) { e.clk.Advance(6 * time.Minute) }},
		{"drained", ReleaseQueueEmpty, func(e *fakeEnv) {
			e.b.mu.Lock()
			e.b.drained = true
			e.b.mu.Unlock()
		}},
		{"after current", ReleaseAfterCurrent, func(e *fakeEnv) {
			e.b.mu.Lock()
			e.b.afterCurrent = true
			e.b.mu.Unlock()
		}},
		{"window", ReleaseWindow, func(e *fakeEnv) {
			set := e.b.Settings()
			set.IdleReleaseMin = 60
			if err := e.b.SetSettings(context.Background(), set); err != nil {
				t.Fatal(err)
			}
			e.clk.Advance(FairWindow)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newFakeEnv(t, t4())
			gate, arrived := e.holdRemovals()
			if _, err := e.b.Transcribe(ctx, e.lecture(t, "a"), nil); err != nil {
				t.Fatalf("a: %v", err)
			}
			e.removalArrived(t, arrived)
			tc.arm(e)
			ticked := make(chan struct{})
			go func() { e.b.Tick(ctx); close(ticked) }()
			e.waitReleasing(t)
			leave := e.phantom()
			close(gate)
			waitClosed(t, ticked, "Tick")
			e.wantRunning(t, "Tick with a lecture dispatched")
			leave()
			e.clk.Advance(tickEvery)
			e.b.Tick(ctx)
			e.wantReleased(t, "the next Tick", tc.reason)
		})
	}
}

// (a), (b) and (c) at the end of a lecture ("0 = right after each
// lecture", two lectures at once on an L4): a's release waits for a's
// removal; b is dispatched meanwhile and runs on the runtime, which stays;
// b's own end releases it. (Red before r4: a's end released the runtime b
// had just been given.)
func TestIdleReleaseAtALecturesEndKeepsALectureDispatchedMeanwhile(t *testing.T) {
	ctx := context.Background()
	set := DefaultSettings() // paid: the L4, two lectures at once
	set.IdleReleaseMin = 0
	e := newFakeEnv(t, set)
	e.b.RefreshAccount(ctx)
	if e.b.Slots() != 2 {
		t.Fatalf("slots %d", e.b.Slots())
	}
	gate, arrived := e.holdRemovals()
	da := startLecture(e, ctx, e.lecture(t, "a"))
	e.removalArrived(t, arrived)
	e.waitReleasing(t) // a's end: its release waits for its removal
	s0 := e.session()
	e.f.hold("b")
	db := startLecture(e, ctx, e.lecture(t, "b"))
	e.waitFor(t, "b on the backend", func() bool { return e.jobsOn() == 2 })
	close(gate)
	if err := waitErr(t, da); err != nil {
		t.Fatalf("a: %v", err)
	}
	e.wantRunning(t, "a's end with b dispatched")
	got := e.transcribingNow(t)
	if got == "a" {
		got = e.transcribingNow(t)
	}
	if got != "b" {
		t.Fatalf("transcribing %q", got)
	}
	if e.session() != s0 {
		t.Error("b runs on another session, want a's")
	}
	e.f.release("b")
	if err := waitErr(t, db); err != nil {
		t.Fatalf("b: %v", err)
	}
	e.wantReleased(t, "b's end", ReleaseIdle)
}

// (b) and (c) at the end of a lecture, for each idle-type reason: with
// nothing else on the runtime it is released before Transcribe returns, as
// before (the lecture itself, still among the jobs, does not count); when a
// lecture was dispatched meanwhile and leaves without using the runtime,
// the next Tick releases it for the same reason.
func TestIdleReleaseAtALecturesEnd(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		idle0        bool
		arm          func(e *fakeEnv) // while the lecture runs
	}{
		{"idle 0", ReleaseIdle, true, func(*fakeEnv) {}},
		{"drained", ReleaseQueueEmpty, false, func(e *fakeEnv) {
			if err := e.b.QueueDrained(context.Background()); err != nil {
				t.Fatal(err)
			}
		}},
		{"after current", ReleaseAfterCurrent, false, func(e *fakeEnv) {
			if err := e.b.ShutdownAfterCurrent(context.Background()); err != nil {
				t.Fatal(err)
			}
		}},
		// R52 L1: a file an earlier lecture left (cleanupKept, a removal)
		{"left file", ReleaseCleanupFailed, false, func(e *fakeEnv) {
			e.b.mu.Lock()
			e.b.sess.leftFile = "result.json"
			e.b.mu.Unlock()
		}},
	} {
		for _, meanwhile := range []bool{false, true} {
			name := tc.name
			if meanwhile {
				name += ", a lecture dispatched meanwhile"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				set := t4()
				if tc.idle0 {
					set.IdleReleaseMin = 0
				}
				e := newFakeEnv(t, set)
				gate, arrived := e.holdRemovals()
				if !meanwhile {
					close(gate)
				}
				e.f.hold("a")
				da := startLecture(e, ctx, e.lecture(t, "a"))
				e.transcribingNow(t)
				tc.arm(e)
				e.f.release("a")
				if !meanwhile {
					if err := waitErr(t, da); err != nil {
						t.Fatalf("a: %v", err)
					}
					e.wantReleased(t, "a's end", tc.reason)
					return
				}
				e.removalArrived(t, arrived)
				e.waitReleasing(t)
				leave := e.phantom()
				close(gate)
				if err := waitErr(t, da); err != nil {
					t.Fatalf("a: %v", err)
				}
				e.wantRunning(t, "a's end with a lecture dispatched")
				leave()
				e.clk.Advance(tickEvery)
				e.b.Tick(ctx)
				e.wantReleased(t, "the next Tick", tc.reason)
			})
		}
	}
}
