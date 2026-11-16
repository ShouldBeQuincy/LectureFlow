// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// v3.2.x P2 Colab (design note 3.4): a transient failure on the runtime is
// probed and retried in place instead of handing the lecture back at once;
// a hand-back leaves the job list at once and releases in the background;
// a new session forgets the last error; Status tells whether the runtime is
// still held, reachable and billed. All on the stateful fake
// (fakecolab_test.go) with the fake clock: nothing waits for real.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
)

func (e *fakeEnv) sleeps() []time.Duration {
	e.clk.mu.Lock()
	defer e.clk.mu.Unlock()
	return slices.Clone(e.clk.sleeps)
}

// The audio upload answers 503 once, then works: the lecture stays on
// Colab, the runtime is not released. (Red before v3.2.x: one failure handed
// the lecture back and released the runtime.)
func TestTransientUploadFailureIsRetriedInPlace(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	l1 := e.lecture(t, "l1")
	ft := e.f.fault(fakeFault{method: "PUT", suffix: "/audio.flac", times: 1, status: 503})
	if _, err := e.b.Transcribe(ctx, l1, nil); err != nil {
		t.Fatalf("Transcribe after one 503 of the audio upload: %v (code %s)", err, Code(err))
	}
	if n := e.f.faultHits(ft); n != 1 {
		t.Fatalf("the fault struck %d times, want 1", n)
	}
	e.b.removalsSettled()
	if e.f.wasReleased() {
		t.Error("the runtime was released after a single transient failure")
	}
	if st := e.b.Status(ctx); st.State != StateRunning || st.LastError != nil {
		t.Errorf("state %s, last error %+v: want running and none", st.State, st.LastError)
	}
	if got := e.sleeps(); !slices.Equal(got, []time.Duration{5 * time.Second}) {
		t.Errorf("waits %v, want one of 5 s before the probe", got)
	}
}

// A lecture fails on the runtime while the release cannot finish (the
// unassign request hangs): Transcribe has returned and the lecture has left
// Status.Jobs all the same; the release goes on in the background. (Red
// before v3.2.x: Transcribe returned only after the release, and the card
// kept the lecture with its last step and progress meanwhile.)
func TestHandBackLeavesTheJobListBeforeTheRelease(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	l1 := e.lecture(t, "l1")
	e.f.setupErr = "no_gpu" // the worker's setup fails: the runtime is no use
	gate, arrived := make(chan struct{}), make(chan struct{})
	e.f.mu.Lock()
	e.f.unassignGate, e.f.unassigning = gate, arrived
	e.f.mu.Unlock()
	open := false
	letGo := func() {
		if !open {
			open = true
			close(gate)
		}
	}
	t.Cleanup(letGo)

	done := startLecture(e, ctx, l1)
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the release never started")
	}
	// The release hangs. The lecture must be back already.
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		st := e.b.Status(ctx)
		t.Fatalf("Transcribe has not returned while the release hangs; Status.Jobs = %+v, state %s", st.Jobs, st.State)
	}
	if !IsFallback(err) || Code(err) != CodeRemoteFailed {
		t.Fatalf("err %v (code %s), want a hand-back with %s", err, Code(err), CodeRemoteFailed)
	}
	st := e.b.Status(ctx)
	if len(st.Jobs) != 0 || st.Current != nil {
		t.Errorf("Status.Jobs = %+v, want none after the hand-back", st.Jobs)
	}
	if st.State != StateStopping {
		t.Errorf("state %s, want %s while the release runs", st.State, StateStopping)
	}
	if ok, _ := e.b.CanDispatch(ctx, 600); ok {
		t.Error("a lecture may be dispatched while the runtime is being released")
	}
	if e.f.wasReleased() {
		t.Fatal("the fake released the runtime although the request is held")
	}
	letGo()
	e.waitFor(t, "the background release", func() bool { return e.b.Status(ctx).State == StateReleased })
	if st := e.b.Status(ctx); st.LastRelease == nil || st.LastRelease.Reason != ReleaseFailed {
		t.Errorf("last release %+v, want %s", st.LastRelease, ReleaseFailed)
	}
}

// holdHandBack makes the next background hand-back wait right before its
// release: arrived is closed when it got there, closing proceed lets it go
// on (also at the end of the test).
func holdHandBack(t *testing.T, e *fakeEnv) (arrived chan struct{}, letGo func()) {
	t.Helper()
	arrived, proceed := make(chan struct{}), make(chan struct{})
	var once, onceP sync.Once
	e.b.handBackHook = func() {
		once.Do(func() { close(arrived) })
		<-proceed
	}
	letGo = func() { onceP.Do(func() { close(proceed) }) }
	t.Cleanup(letGo)
	return arrived, letGo
}

func waitClosed(t *testing.T, c chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatal(what + " never happened")
	}
}

// A lecture fails and its hand-back is slow to reach the release (its file
// cleanup takes a while); meanwhile the scheduler drains the queue and
// releases the runtime first. The card's release reason is still the
// hand-back's ("failed"), not "queue_empty". (Red before the review fix:
// the drain's reason won.)
func TestHandBackKeepsItsReasonWhenTheDrainReleasesFirst(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	arrived, letGo := holdHandBack(t, e)
	e.f.mu.Lock()
	e.f.setupErr = "no_gpu"
	e.f.mu.Unlock()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); Code(err) != CodeRemoteFailed {
		t.Fatalf("err %v (code %s), want %s", err, Code(err), CodeRemoteFailed)
	}
	waitClosed(t, arrived, "the hand-back's release")
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatalf("QueueDrained: %v", err)
	}
	if !e.f.wasReleased() {
		t.Fatal("the drain did not release the runtime")
	}
	if st := e.b.Status(ctx); st.LastRelease == nil || st.LastRelease.Reason != ReleaseFailed {
		t.Errorf("last release %+v after the drain, want %s", st.LastRelease, ReleaseFailed)
	}
	letGo()
	e.b.handbacksSettled()
	if st := e.b.Status(ctx); st.LastRelease == nil || st.LastRelease.Reason != ReleaseFailed {
		t.Errorf("last release %+v after the hand-back ended, want %s", st.LastRelease, ReleaseFailed)
	}
}

// A hand-back's release is overtaken: another release frees the runtime and
// a new lecture borrows a new one before the hand-back gets the release
// lock. The hand-back must leave the new session alone. (Red before the
// review fix: it released the new session, whose lecture then went on on a
// runtime nobody would release.)
func TestStaleHandBackLeavesTheNextSessionAlone(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	arrived, letGo := holdHandBack(t, e)
	e.f.mu.Lock()
	e.f.setupErr = "no_gpu"
	e.f.mu.Unlock()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); Code(err) != CodeRemoteFailed {
		t.Fatalf("err %v (code %s), want %s", err, Code(err), CodeRemoteFailed)
	}
	waitClosed(t, arrived, "the hand-back's release")
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatalf("QueueDrained: %v", err)
	}
	if st := e.b.Status(ctx); st.State != StateReleased {
		t.Fatalf("state %s after the drain, want %s", st.State, StateReleased)
	}
	// A new session for the next lecture, held in its transcription.
	e.f.mu.Lock()
	e.f.setupErr = ""
	e.f.mu.Unlock()
	e.b.SetQueue([]float64{300})
	e.f.hold("l2")
	done := startLecture(e, ctx, e.lecture(t, "l2"))
	if n := e.transcribingNow(t); n != "l2" {
		t.Fatalf("transcribing %q, want l2", n)
	}
	letGo()
	e.b.handbacksSettled()
	if e.f.wasReleased() {
		t.Error("the stale hand-back released the new session's runtime")
	}
	if st := e.b.Status(ctx); st.State != StateRunning || len(st.Jobs) != 1 {
		t.Errorf("state %s with %d lectures, want running with l2", st.State, len(st.Jobs))
	}
	e.f.release("l2")
	if err := <-done; err != nil {
		t.Fatalf("l2: %v (code %s)", err, Code(err))
	}
	// The new session is still the backend's: the app's exit releases it.
	if err := e.b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !e.f.wasReleased() {
		t.Error("the new session's runtime was left assigned after Close")
	}
}

// A release ends the session while its runtime is still being assigned
// (the release found no runtime yet and forgot the session). When the
// assignment then arrives, the runtime goes back at once and the lecture
// gets IsBusy: no runtime is left assigned with nobody to release it.
// (Red before the review fix: the lecture went on on a runtime the backend
// no longer knew.)
func TestReleaseWhileAssigningLeavesNoRuntime(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	gate, arrived := make(chan struct{}), make(chan struct{})
	e.f.mu.Lock()
	e.f.assignGate, e.f.assigning = gate, arrived
	e.f.mu.Unlock()
	var once sync.Once
	letGo := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(letGo)
	done := startLecture(e, ctx, e.lecture(t, "l1"))
	waitClosed(t, arrived, "the assignment request")
	if err := e.b.release(ctx, ReleaseFailed); err != nil {
		t.Fatalf("release: %v", err)
	}
	letGo()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Transcribe did not return")
	}
	if !IsBusy(err) {
		t.Errorf("err %v (code %s), want IsBusy", err, Code(err))
	}
	e.f.mu.Lock()
	assigned := e.f.assigned
	e.f.mu.Unlock()
	if assigned {
		t.Error("the runtime assigned after the release is still assigned")
	}
	if st := e.b.Status(ctx); st.State != StateReleased {
		t.Errorf("state %s, want %s", st.State, StateReleased)
	}
}

// A release keeps failing (the network is down): the session stays
// stopping, still held and billed, and nothing samples the runtime any
// more. Its last "cannot reach" observation stays on the card instead of
// turning unknown after 30 s (review R3 suggestion 3).
func TestStoppingKeepsTheUnreachableObservation(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	e.b.removalsSettled()
	e.b.mu.Lock()
	s := e.b.sess
	e.b.mu.Unlock()
	if s == nil {
		t.Fatal("no session after the lecture")
	}
	e.b.noteReach(s, false)
	e.b.mu.Lock()
	s.rec.State = StateStopping
	e.b.mu.Unlock()
	e.clk.Advance(5 * time.Minute)
	st := e.b.Status(ctx)
	if st.Runtime == nil || st.Runtime.Reachable != ReachUnreachable || !st.Runtime.Held {
		t.Errorf("runtime %+v, want held and unreachable while stopping", st.Runtime)
	}
	// running, the same observation expires as before
	e.b.mu.Lock()
	s.rec.State = StateRunning
	e.b.mu.Unlock()
	if st := e.b.Status(ctx); st.Runtime == nil || st.Runtime.Reachable != ReachUnknown {
		t.Errorf("runtime %+v, want unknown once the observation is old while running", st.Runtime)
	}
}

// leakyKV fails reading the credential with err once it is set: a
// storage error quoting the row, or one that says the credential cannot be
// used (the WaitRemoteAuth branch of a release), carrying the refresh
// token.
type leakyKV struct {
	*MemKV
	mu  sync.Mutex
	err error
}

func (k *leakyKV) fail(err error) {
	k.mu.Lock()
	k.err = err
	k.mu.Unlock()
}

func (k *leakyKV) GetKV(ctx context.Context, key string) (string, bool, error) {
	k.mu.Lock()
	err := k.err
	k.mu.Unlock()
	if err != nil && key == KVCredential {
		return "", false, err
	}
	return k.MemKV.GetKV(ctx, key)
}

// A release fails with an error that carries a known secret: the error kept
// for the card, the API and the diagnostics zip (Status.LastError) is
// scrubbed like every other one, in both branches of releaseWith — a
// failure Tick retries (its text in Detail) and a credential that cannot
// release the runtime (kept for later; its Params). (SEC2 T1; red before
// r4: releaseWith recorded infoOf(err) unscrubbed. The second case, review
// R52 L2, is red when only that branch records it unscrubbed.)
func TestReleaseFailureErrorIsScrubbed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		leftover bool
		shown    func(info *model.ErrorInfo) string
	}{
		{"storage error", errors.New("reading row " + testRefreshToken + " failed"), false,
			func(info *model.ErrorInfo) string { return info.Detail }},
		{"credential refused", authErr(CodeAuthRejected, map[string]any{"reason": "refused " + testRefreshToken}, "refused"), true,
			func(info *model.ErrorInfo) string { s, _ := info.Params["reason"].(string); return s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var kv *leakyKV
			e := newFakeEnv(t, t4(), func(o *Options) {
				kv = &leakyKV{MemKV: o.KV.(*MemKV)}
				o.KV = kv
			})
			ctx := context.Background()
			if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			e.b.removalsSettled()
			kv.fail(tc.err)
			err := e.b.release(ctx, ReleaseNow)
			kv.fail(nil)
			if err == nil {
				t.Fatal("the release did not fail")
			}
			st := e.b.Status(ctx)
			if st.LastError == nil {
				t.Fatal("the failed release left no last error")
			}
			if (st.Leftover != nil) != tc.leftover {
				t.Fatalf("leftover %+v, want %v (the branch under test)", st.Leftover, tc.leftover)
			}
			raw, _ := json.Marshal(st)
			assertNoSecrets(t, "Status after a failed release", string(raw))
			if !strings.Contains(tc.shown(st.LastError), "[redacted]") {
				t.Errorf("last error %+v, want the failure's text with the secret redacted", st.LastError)
			}
		})
	}
}
