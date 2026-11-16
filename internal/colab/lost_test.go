// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Security review SEC2 of v3.2.x line C: a runtime assigned to a session
// that was released while the assignment was on its way must not be
// forgotten when giving it back fails (H1); the kernel channels' redial
// gives up at once when the runtime is gone (L2); a lecture's cancel
// releases only its own session (L3); a session released while it boots
// sends its lecture back as busy, not failed (L4). All on the stateful fake
// with the fake clock: nothing waits for real.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/remote"
)

// lostAfterAssign borrows a runtime for lecture l1 while a release of the
// still queued session runs, and lets the runtime's release (POST unassign
// of gpu-fake-1) fail with 503 every time: the lecture gets IsBusy and the
// runtime stays assigned. It returns the fault (clear it with clearFaults).
func lostAfterAssign(t *testing.T, e *fakeEnv) *fakeFault {
	t.Helper()
	ctx := context.Background()
	gate, arrived := make(chan struct{}), make(chan struct{})
	e.f.mu.Lock()
	e.f.multi = true
	e.f.assignGate, e.f.assigning = gate, arrived
	e.f.mu.Unlock()
	var once sync.Once
	letGo := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(letGo)
	done := startLecture(e, ctx, e.lecture(t, "l1"))
	waitClosed(t, arrived, "the assignment request")
	// the release finds no runtime yet and forgets the session
	if err := e.b.release(ctx, ReleaseFailed); err != nil {
		t.Fatalf("release: %v", err)
	}
	ft := e.f.fault(fakeFault{method: http.MethodPost, suffix: "/unassign/gpu-fake-1", times: -1, status: 503})
	letGo()
	if err := waitErr(t, done); !IsBusy(err) {
		t.Fatalf("l1: %v (code %s), want IsBusy", err, Code(err))
	}
	if n := e.f.faultHits(ft); n == 0 {
		t.Fatal("the runtime assigned after the release was never given back")
	}
	if live := e.f.liveEndpoints(); !slices.Equal(live, []string{"gpu-fake-1"}) {
		t.Fatalf("assigned %v, want gpu-fake-1 still assigned (its release failed)", live)
	}
	return ft
}

func sessionEndpoint(t *testing.T, kv KV) string {
	t.Helper()
	v, _, _ := kv.GetKV(context.Background(), KVSession)
	if v == "" {
		return ""
	}
	var rec sessionRecord
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		t.Fatalf("session record %q: %v", v, err)
	}
	return rec.Endpoint
}

// H1: the runtime that could not be given back is shown as left over and
// released by a later Tick once Colab answers again; a new session borrowed
// meanwhile keeps its runtime, its lecture and its record. (Red before the
// fix: the runtime was forgotten — no leftover, and no Tick ever released
// it.)
func TestRuntimeLostWhileAssigningIsReleasedByTick(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	lostAfterAssign(t, e)
	if st := e.b.Status(ctx); st.Leftover == nil || st.Leftover.Accelerator != "T4" {
		t.Fatalf("leftover %+v, want the runtime that could not be released (T4)", st.Leftover)
	}

	// The next lecture borrows a runtime of its own.
	e.f.hold("l2")
	done := startLecture(e, ctx, e.lecture(t, "l2"))
	if n := e.transcribingNow(t); n != "l2" {
		t.Fatalf("transcribing %q, want l2", n)
	}
	if live := e.f.liveEndpoints(); !slices.Equal(live, []string{"gpu-fake-1", "gpu-fake-2"}) {
		t.Fatalf("assigned %v, want gpu-fake-1 and gpu-fake-2", live)
	}
	if ep := sessionEndpoint(t, e.kv); ep != "gpu-fake-2" {
		t.Fatalf("session record of %q, want the new session's gpu-fake-2", ep)
	}

	// Colab still refuses: Tick tries, nothing changes.
	e.b.Tick(ctx)
	if live := e.f.liveEndpoints(); !slices.Equal(live, []string{"gpu-fake-1", "gpu-fake-2"}) {
		t.Fatalf("assigned %v after a failing Tick, want both", live)
	}
	if st := e.b.Status(ctx); st.Leftover == nil {
		t.Error("the leftover disappeared while its runtime is still assigned")
	}

	// Colab answers again: the next Tick releases the lost runtime only.
	e.f.clearFaults()
	e.b.Tick(ctx)
	if live := e.f.liveEndpoints(); !slices.Equal(live, []string{"gpu-fake-2"}) {
		t.Fatalf("assigned %v after Tick, want only the new session's gpu-fake-2", live)
	}
	st := e.b.Status(ctx)
	if st.Leftover != nil {
		t.Errorf("leftover %+v after its runtime was released", st.Leftover)
	}
	if st.State != StateRunning || len(st.Jobs) != 1 {
		t.Errorf("state %s with %d lectures, want running with l2", st.State, len(st.Jobs))
	}
	if ep := sessionEndpoint(t, e.kv); ep != "gpu-fake-2" {
		t.Errorf("session record of %q, want gpu-fake-2", ep)
	}
	e.f.release("l2")
	if err := waitErr(t, done); err != nil {
		t.Fatalf("l2: %v (code %s)", err, Code(err))
	}
	if err := e.b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if live := e.f.liveEndpoints(); len(live) != 0 {
		t.Errorf("assigned %v after Close", live)
	}
}

// H1 across a restart: the app ends (no Close) with the lost runtime still
// assigned and a new session's record in the database. The next start
// releases both. (Red before the fix: the lost runtime stayed assigned.)
func TestRuntimeLostWhileAssigningIsReleasedAfterARestart(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	lostAfterAssign(t, e)
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l2"), nil); err != nil {
		t.Fatalf("l2: %v (code %s)", err, Code(err))
	}
	e.b.removalsSettled()
	if ep := sessionEndpoint(t, e.kv); ep != "gpu-fake-2" {
		t.Fatalf("session record of %q, want gpu-fake-2", ep)
	}
	// the next run of the app; Colab answers again
	e.f.clearFaults()
	lg, _ := newTestLogger()
	o := e.b.o
	o.Logger, o.RunID = lg, "run-2"
	b2, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.bgCancel)
	if err := b2.Startup(ctx); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if live := e.f.liveEndpoints(); len(live) != 0 {
		t.Errorf("assigned %v after the next start, want none", live)
	}
	if st := b2.Status(ctx); st.Leftover != nil {
		t.Errorf("leftover %+v after the next start released everything", st.Leftover)
	}
}

// H1 with Colab still refusing at the next start: the runtime stays known
// (leftover shown) and Tick releases it later; the record of the session
// the first run left behind is released too.
func TestRuntimeLostWhileAssigningSurvivesAFailingStart(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	lostAfterAssign(t, e)
	lg, _ := newTestLogger()
	o := e.b.o
	o.Logger, o.RunID = lg, "run-2"
	b2, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.bgCancel)
	if err := b2.Startup(ctx); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if st := b2.Status(ctx); st.Leftover == nil {
		t.Fatal("no leftover after a start that could not release the lost runtime")
	}
	e.f.clearFaults()
	b2.Tick(ctx)
	if live := e.f.liveEndpoints(); len(live) != 0 {
		t.Errorf("assigned %v after Tick, want none", live)
	}
	if st := b2.Status(ctx); st.Leftover != nil {
		t.Errorf("leftover %+v after Tick released the runtime", st.Leftover)
	}
}

// L4: the session is released while it boots, and the boot then fails (the
// runtime is gone: 404 until the boot timeout). The lecture goes back as
// busy (Colab may take it later), not as failed to the local devices.
// (Red before the fix: boot_timeout, a fallback.)
func TestReleaseWhileBootingThenBootFailsIsBusy(t *testing.T) {
	arrived, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e := newFakeEnv(t, t4(), func(o *Options) {
		inner := o.Sleep
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			first := false
			if d == time.Second { // the boot's first wait (no other wait is 1 s)
				once.Do(func() { first = true })
			}
			if first {
				close(arrived)
				<-proceed
			}
			return inner(ctx, d)
		}
	})
	ctx := context.Background()
	e.f.fault(fakeFault{method: http.MethodGet, suffix: "/rt/api/kernels", times: -1, status: 404})
	done := startLecture(e, ctx, e.lecture(t, "l1"))
	waitClosed(t, arrived, "the boot's first wait")
	if err := e.b.release(ctx, ReleaseFailed); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !e.f.wasReleased() {
		t.Fatal("the release did not give the runtime back")
	}
	close(proceed)
	err := waitErr(t, done)
	if !IsBusy(err) || IsFallback(err) {
		t.Fatalf("l1: %v (code %s, fallback %v), want IsBusy", err, Code(err), IsFallback(err))
	}
	if v, _, _ := e.kv.GetKV(ctx, KVSession); v != "" {
		t.Errorf("session record written again: %s", v)
	}
}

// L3: on an L4 a lecture waits for the borrowing another lecture started;
// it is cancelled meanwhile. The cancel must not release the other
// lecture's session: that lecture goes on. (Red before the fix: the cancel
// released the session being borrowed and the other lecture got IsBusy.)
func TestCancelWhileWaitingForTheBorrowingLeavesItAlone(t *testing.T) {
	e := newFakeEnv(t, DefaultSettings())
	ctx := context.Background()
	e.b.RefreshAccount(ctx)
	if e.b.Settings().Accelerator != "L4" || e.b.Slots() != 2 {
		t.Fatalf("default %s, slots %d", e.b.Settings().Accelerator, e.b.Slots())
	}
	gate, arrived := make(chan struct{}), make(chan struct{})
	e.f.mu.Lock()
	e.f.assignGate, e.f.assigning = gate, arrived
	e.f.mu.Unlock()
	var once sync.Once
	letGo := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(letGo)
	da := startLecture(e, ctx, e.lecture(t, "a"))
	waitClosed(t, arrived, "the assignment request")
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	db := startLecture(e, pctx, e.lecture(t, "b"))
	e.waitFor(t, "b on the backend", func() bool { return len(e.b.Status(ctx).Jobs) == 2 })
	cancel()
	if err := waitErr(t, db); Code(err) != CodeCancelled {
		t.Fatalf("b: %v (code %s), want %s", err, Code(err), CodeCancelled)
	}
	letGo()
	if err := waitErr(t, da); err != nil {
		t.Fatalf("a: %v (code %s, busy %v), want it to run on its session", err, Code(err), IsBusy(err))
	}
	_ = e.b.QueueDrained(ctx)
	if !e.f.wasReleased() {
		t.Error("the runtime is still assigned after the drain")
	}
}

// L2: the kernel channels break after the request went out and the
// runtime is gone when they are opened again (404): kernelExec gives up
// after that redial instead of waiting out the window, with the 404 (the
// caller maps it to session_reclaimed). The same for 401, 403 and 410.
// (Red before the fix: 5 s, 15 s and 30 s of waits, then connection_lost.)
func TestKernelRedialToAGoneRuntimeGivesUpAtOnce(t *testing.T) {
	for _, status := range []int{401, 403, 404, 410} {
		clk := newFakeClock()
		rc := &reconnector{o: RetryOptions{Jitter: -1}.normalized(), now: clk.Now, sleep: clk.Sleep, notify: func(*remote.Reconnect) {}}
		base, _ := url.Parse("http://runtime.invalid/rt")
		rt := &runtimeClient{base: base, token: "x", agent: "t"}
		dials := 0
		rt.dial = func(context.Context, string, http.Header) (MessageConn, error) {
			dials++
			if dials == 1 {
				return &scriptConn{}, nil // the request goes out, then the connection breaks
			}
			return nil, &httpError{endpoint: "api/kernels/channels", status: status}
		}
		_, err := rt.kernelExec(context.Background(), "k1", "pass", func(string) error { return nil }, rc, nil)
		if statusOf(err) != status || !isRuntimeErr(err) {
			t.Errorf("%d: err %v, want the runtime's %d", status, err, status)
		}
		if want := []time.Duration{5 * time.Second}; !slices.Equal(clk.sleeps, want) || dials != 2 {
			t.Errorf("%d: waits %v with %d connections, want %v with 2", status, clk.sleeps, dials, want)
		}
	}
}

// H1, the record: a session whose release failed at the start keeps its
// record (cleanupLeftover); a new session borrowed afterwards must not
// overwrite it and leave that runtime to nobody. It is kept among the lost
// runtimes and Tick releases it once Colab answers; the new session keeps
// its runtime and record. (Red before the fix: the new record replaced the
// old one and the old runtime stayed assigned.)
func TestNewSessionDoesNotOverwriteAnUnreleasedRecord(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	e.f.mu.Lock()
	e.f.multi = true
	e.f.mu.Unlock()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); err != nil {
		t.Fatalf("l1: %v (code %s)", err, Code(err))
	}
	e.b.removalsSettled()
	// the app ends without releasing; the next start cannot release it
	e.f.fault(fakeFault{method: http.MethodPost, suffix: "/unassign/gpu-fake-1", times: -1, status: 503})
	lg, _ := newTestLogger()
	o := e.b.o
	o.Logger, o.RunID = lg, "run-2"
	b2, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.bgCancel)
	if err := b2.Startup(ctx); err == nil {
		t.Fatal("Startup released a runtime whose release fails")
	}
	if st := b2.Status(ctx); st.Leftover == nil {
		t.Fatal("no leftover after a start that could not release the earlier session")
	}
	if _, err := b2.Transcribe(ctx, e.lecture(t, "l2"), nil); err != nil {
		t.Fatalf("l2: %v (code %s)", err, Code(err))
	}
	b2.removalsSettled()
	if ep := sessionEndpoint(t, e.kv); ep != "gpu-fake-2" {
		t.Fatalf("session record of %q, want the new session's gpu-fake-2", ep)
	}
	if st := b2.Status(ctx); st.Leftover == nil {
		t.Error("the leftover disappeared while its runtime is still assigned")
	}
	e.f.clearFaults()
	b2.Tick(ctx)
	if live := e.f.liveEndpoints(); !slices.Equal(live, []string{"gpu-fake-2"}) {
		t.Fatalf("assigned %v after Tick, want only the new session's gpu-fake-2", live)
	}
	if st := b2.Status(ctx); st.Leftover != nil || st.State != StateRunning {
		t.Errorf("leftover %+v, state %s: want none and the new session running", st.Leftover, st.State)
	}
	if err := b2.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if live := e.f.liveEndpoints(); len(live) != 0 {
		t.Errorf("assigned %v after Close", live)
	}
}

// A borrowing given up (Colab refuses the accelerator) whose "was one
// assigned after all?" question fails too: the notebook hash is kept and
// asked again by Tick instead of being forgotten; nothing assigned, it is
// dropped then.
func TestUnansweredAssignQuestionIsAskedAgain(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	e.f.fault(fakeFault{method: http.MethodPost, suffix: "/tun/m/assign", times: 1, status: 400})
	// the GET of assign goes through, the GET of dropUnassigned fails
	e.f.fault(fakeFault{method: http.MethodGet, suffix: "/tun/m/assign", skip: 1, times: 1, status: 503})
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); Code(err) != CodeAcceleratorUnavailable {
		t.Fatalf("l1: %v (code %s), want %s", err, Code(err), CodeAcceleratorUnavailable)
	}
	if st := e.b.Status(ctx); st.Leftover == nil {
		t.Fatal("the unanswered question was forgotten")
	}
	e.b.Tick(ctx)
	if st := e.b.Status(ctx); st.Leftover != nil {
		t.Errorf("leftover %+v after Colab said nothing was assigned", st.Leftover)
	}
	if v, _, _ := e.kv.GetKV(ctx, KVLost); v != "" {
		t.Errorf("kv %s = %s, want empty", KVLost, v)
	}
}

// The start cannot release the session an earlier run left behind (Colab
// does not answer): Tick tries again, not only the next import or start.
// (Red before the fix: with no session of its own, Tick did nothing.)
func TestFailedStartCleanupIsRetriedByTick(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); err != nil {
		t.Fatalf("l1: %v (code %s)", err, Code(err))
	}
	e.b.removalsSettled()
	e.f.fault(fakeFault{method: http.MethodPost, suffix: "/unassign/gpu-fake-1", times: -1, status: 503})
	lg, _ := newTestLogger()
	o := e.b.o
	o.Logger, o.RunID = lg, "run-2"
	b2, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.bgCancel)
	if err := b2.Startup(ctx); err == nil {
		t.Fatal("Startup released a runtime whose release fails")
	}
	b2.Tick(ctx) // still failing: kept
	if st := b2.Status(ctx); st.Leftover == nil || e.f.wasReleased() {
		t.Fatalf("leftover %+v, released %v: want it kept and shown", st.Leftover, e.f.wasReleased())
	}
	e.f.clearFaults()
	b2.Tick(ctx)
	if !e.f.wasReleased() {
		t.Error("Tick did not release the earlier run's session")
	}
	if st := b2.Status(ctx); st.Leftover != nil {
		t.Errorf("leftover %+v after the release", st.Leftover)
	}
	if v, _, _ := e.kv.GetKV(ctx, KVSession); v != "" {
		t.Errorf("session record left: %s", v)
	}
}
