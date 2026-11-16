// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// v3.2.x P2 Colab, continued (handback_test.go has the two cases that were
// red before the change): the probe window, permanent failures, the chunked
// upload, the last error, Status.Runtime, quitting, the jitter and the
// kernel channels.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/remote"
)

// The runtime answers nothing any more: exactly three probes after 5 s,
// 15 s and 30 s, then the lecture is handed back and the runtime released.
// The lecture's events and Status.Jobs tell the attempts meanwhile.
func TestProbeWindowUsedUpHandsBack(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	l1 := e.lecture(t, "l1")
	e.f.fault(fakeFault{method: "PUT", suffix: "/audio.flac", times: -1, status: 503})
	probe := e.f.fault(fakeFault{method: "GET", suffix: "/rt/api/kernels", skip: 1, times: -1, status: 502}) // the boot's probe goes through
	var mu sync.Mutex
	var seen []remote.Reconnect
	var during []JobInfo
	_, err := e.b.Transcribe(ctx, l1, func(ev Event) {
		if ev.Reconnect == nil {
			return
		}
		st := e.b.Status(ctx)
		mu.Lock()
		seen = append(seen, *ev.Reconnect)
		during = append(during, st.Jobs...)
		mu.Unlock()
		if ev.Step != StepUploadAudio {
			t.Errorf("a reconnect event of step %q, want the lecture's last step %q", ev.Step, StepUploadAudio)
		}
	})
	if !IsFallback(err) || Code(err) != CodeUploadFailed {
		t.Fatalf("err %v (code %s), want a hand-back with %s", err, Code(err), CodeUploadFailed)
	}
	if got, want := e.sleeps(), []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	if n := e.f.faultHits(probe); n != 3 {
		t.Errorf("%d probes, want exactly 3", n)
	}
	if n := e.f.count(func() int { return e.f.uploadCount[base+"/jobs/l1/audio.flac"] }); n != 0 {
		t.Errorf("the audio was written %d times although every attempt failed", n)
	}
	mu.Lock()
	if len(seen) != 3 {
		t.Fatalf("reconnect events %+v, want three", seen)
	}
	start := newFakeClock().Now()
	for i, r := range seen {
		wantAt := start.Add([]time.Duration{5, 20, 50}[i] * time.Second)
		if r.Attempt != i+1 || r.Of != 3 || !r.NextAt.Equal(wantAt) {
			t.Errorf("reconnect %d = %+v, want attempt %d of 3 at %s", i, r, i+1, wantAt)
		}
	}
	if len(during) != 3 || during[2].Reconnect == nil || during[2].Reconnect.Attempt != 3 {
		t.Errorf("Status.Jobs during the waits: %+v, want the lecture with its reconnect", during)
	}
	mu.Unlock()
	e.b.handbacksSettled()
	st := e.b.Status(ctx)
	if !e.f.wasReleased() || st.State != StateReleased || st.Runtime != nil || st.SessionStartedAt != nil {
		t.Errorf("after the hand-back: released %v, state %s, runtime %+v", e.f.wasReleased(), st.State, st.Runtime)
	}
	if st.LastError == nil || st.LastError.Code != CodeUploadFailed {
		t.Errorf("last error %+v", st.LastError)
	}
	for _, want := range []string{"probed after a wait", "did not come back within the probe window"} {
		if !strings.Contains(e.log.String(), want) {
			t.Errorf("the log does not say %q", want)
		}
	}
}

// The reconnection ends: an event without Reconnect follows, and
// Status.Jobs no longer carries it.
func TestReconnectIsClearedWhenTheRuntimeAnswers(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	e.f.fault(fakeFault{method: "PUT", suffix: "/audio.flac", times: 1, status: 503})
	e.f.hold("l1")
	var mu sync.Mutex
	var marks []bool // per event of the upload step: does it carry Reconnect
	done := make(chan error, 1)
	go func() {
		_, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), func(ev Event) {
			if ev.Step == StepUploadAudio {
				mu.Lock()
				marks = append(marks, ev.Reconnect != nil)
				mu.Unlock()
			}
		})
		done <- err
	}()
	e.transcribingNow(t)
	st := e.b.Status(ctx)
	if len(st.Jobs) != 1 || st.Jobs[0].Reconnect != nil {
		t.Errorf("Status.Jobs %+v while it transcribes, want the lecture without a reconnect", st.Jobs)
	}
	if r := st.Runtime; r == nil || r.Reachable != ReachOK {
		t.Errorf("runtime %+v after the step worked again, want reachable", r)
	}
	e.f.release("l1")
	if err := waitErr(t, done); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	first, last := slices.Index(marks, true), -1
	for i, m := range marks {
		if m {
			last = i
		}
	}
	if first < 0 || last == len(marks)-1 {
		t.Errorf("upload events carrying a reconnect: %v, want some and then none", marks)
	}
	e.b.removalsSettled()
}

// The probe answers but the step keeps failing: it is tried once at first
// and once after each of the three probes, never more.
func TestStepThatKeepsFailingIsTriedFourTimes(t *testing.T) {
	e := newFakeEnv(t, t4())
	ft := e.f.fault(fakeFault{method: "PUT", suffix: "/job.json", times: -1, status: 500})
	_, err := e.b.Transcribe(context.Background(), e.lecture(t, "l1"), nil)
	if Code(err) != CodeUploadFailed {
		t.Fatalf("err %v", err)
	}
	if n := e.f.faultHits(ft); n != 4 {
		t.Errorf("job.json was sent %d times, want 4", n)
	}
	if got, want := e.sleeps(), []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	e.b.handbacksSettled()
}

// 401, 403, 404, 410 and an answer that is not the API's: no wait, no
// probe, handed back at once (V14.6 unchanged).
func TestPermanentFailuresAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		reason string
	}{
		{401, CodeSessionReclaimed, ReleaseReclaimed}, {403, CodeSessionReclaimed, ReleaseReclaimed},
		{404, CodeSessionReclaimed, ReleaseReclaimed}, {410, CodeSessionReclaimed, ReleaseReclaimed},
		{418, CodeAPIChanged, ReleaseFailed},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			e := newFakeEnv(t, t4())
			ctx := context.Background()
			ft := e.f.fault(fakeFault{method: "PUT", suffix: "/audio.flac", times: -1, status: tc.status})
			_, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil)
			if !IsFallback(err) || Code(err) != tc.code {
				t.Fatalf("err %v (code %s), want %s", err, Code(err), tc.code)
			}
			if got := e.sleeps(); len(got) != 0 {
				t.Errorf("waits %v, want none", got)
			}
			if n := e.f.faultHits(ft); n != 1 {
				t.Errorf("the upload was sent %d times, want once", n)
			}
			if n := e.f.count(func() int { return e.f.probes }); n != 1 {
				t.Errorf("%d GET api/kernels, want only the boot's", n)
			}
			e.b.handbacksSettled()
			if st := e.b.Status(ctx); st.LastRelease == nil || st.LastRelease.Reason != tc.reason {
				t.Errorf("last release %+v, want %s", st.LastRelease, tc.reason)
			}
		})
	}
}

// The probe says the runtime is gone (404): no further attempt.
func TestProbeThatIsRefusedEndsTheRetries(t *testing.T) {
	e := newFakeEnv(t, t4())
	e.f.fault(fakeFault{method: "PUT", suffix: "/audio.flac", times: -1, status: 503})
	e.f.fault(fakeFault{method: "GET", suffix: "/rt/api/kernels", skip: 1, times: -1, status: 404})
	_, err := e.b.Transcribe(context.Background(), e.lecture(t, "l1"), nil)
	if Code(err) != CodeSessionReclaimed {
		t.Fatalf("err %v (code %s), want %s", err, Code(err), CodeSessionReclaimed)
	}
	if got, want := e.sleeps(), []time.Duration{5 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	e.b.handbacksSettled()
}

// Chunk 2 of the audio is written on the runtime but its answer never
// comes (the request hangs until its time limit), and the last chunk's
// connection breaks after it was written: the upload starts again from
// chunk 1 each time, so the file on the runtime is the audio, not the audio
// with a chunk twice — the fake's worker checks its sha256 as the real one
// does.
func TestUploadRetryStartsFromTheFirstChunk(t *testing.T) {
	e := newFakeEnv(t, t4(), func(o *Options) { o.RuntimeTimeout = 250 * time.Millisecond }) // a chunk: 750 ms
	ctx := context.Background()
	l1 := e.lecture(t, "l1")
	hang := e.f.fault(fakeFault{method: "PUT", suffix: "/audio.flac", chunk: 2, times: 1, hang: true, apply: true})
	drop := e.f.fault(fakeFault{method: "PUT", suffix: "/audio.flac", chunk: -1, times: 1, drop: true, apply: true})
	if _, err := e.b.Transcribe(ctx, l1, nil); err != nil {
		t.Fatalf("Transcribe: %v (code %s)", err, Code(err))
	}
	if e.f.faultHits(hang) != 1 || e.f.faultHits(drop) != 1 {
		t.Fatalf("faults struck %d and %d times, want 1 and 1", e.f.faultHits(hang), e.f.faultHits(drop))
	}
	// chunk 1 three times: the first upload, after the hang, after the drop
	if n := e.f.count(func() int { return e.f.uploadCount[base+"/jobs/l1/audio.flac"] }); n != 3 {
		t.Errorf("the upload began %d times, want 3", n)
	}
	if got, want := e.sleeps(), []time.Duration{5 * time.Second, 15 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v (one sequence for the step)", got, want)
	}
	e.b.removalsSettled()
	if e.f.wasReleased() {
		t.Error("the runtime was released")
	}
}

// The fake does append a chunk that is sent twice (as Jupyter's chunked
// protocol does): the test above would fail if only the failed chunk were
// sent again.
func TestFakeAppendsARepeatedChunk(t *testing.T) {
	e := newFakeEnv(t, t4())
	rt := &runtimeClient{hc: e.f.srv.Client(), token: "x", agent: "t"}
	rt.base, _ = url.Parse(e.f.srv.URL + "/rt")
	put := func(chunk int, data string) {
		body, _ := json.Marshal(map[string]any{"type": "file", "format": "base64", "chunk": chunk,
			"content": base64.StdEncoding.EncodeToString([]byte(data))})
		if st, _, err := rt.request(context.Background(), "PUT", contentsPath("d/f"), rt.contentsQuery(nil), jsonHeader, body, time.Second); err != nil || st != 201 {
			t.Fatalf("PUT: %d %v", st, err)
		}
	}
	file := func() string {
		e.f.mu.Lock()
		defer e.f.mu.Unlock()
		return string(e.f.files["d/f"])
	}
	put(1, "ab")
	put(2, "cd")
	put(2, "cd")
	put(-1, "ef")
	if got := file(); got != "abcdcdef" {
		t.Fatalf("file %q", got)
	}
	put(1, "ab")
	if got := file(); got != "ab" {
		t.Fatalf("chunk 1 does not start the file again: %q", got)
	}
}

// A new session forgets the last one's error; Status tells when the
// session began.
func TestNewSessionClearsTheLastError(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	e.f.setupErr = "no_gpu"
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); Code(err) != CodeRemoteFailed {
		t.Fatalf("err %v", err)
	}
	e.b.handbacksSettled()
	st := e.b.Status(ctx)
	if st.LastError == nil || st.LastError.Code != CodeRemoteFailed || st.LastErrorAt == nil || st.SessionStartedAt != nil {
		t.Fatalf("after the failure: last error %+v at %v, session started %v", st.LastError, st.LastErrorAt, st.SessionStartedAt)
	}
	e.clk.Advance(10 * time.Minute)
	e.f.mu.Lock()
	e.f.setupErr = ""
	gate, booting := make(chan struct{}), make(chan struct{})
	e.f.bootGate, e.f.booting = gate, booting
	e.f.mu.Unlock()
	done := startLecture(e, ctx, e.lecture(t, "l2"))
	select {
	case <-booting:
	case <-time.After(10 * time.Second):
		t.Fatal("the second session never booted")
	}
	st = e.b.Status(ctx)
	if st.LastError != nil || st.LastErrorAt != nil {
		t.Errorf("the new session still shows the last one's error: %+v", st.LastError)
	}
	if st.SessionStartedAt == nil || !st.SessionStartedAt.Equal(e.clk.Now()) {
		t.Errorf("session_started_at %v, want %v", st.SessionStartedAt, e.clk.Now())
	}
	if st.Runtime == nil || !st.Runtime.Held || st.Runtime.Reachable != ReachUnknown {
		t.Errorf("runtime %+v while booting, want held and unknown", st.Runtime)
	}
	close(gate)
	if err := waitErr(t, done); err != nil {
		t.Fatalf("the second lecture: %v", err)
	}
	e.b.removalsSettled()
}

// Without a session: an error recorded because the account could not be
// read goes once it can be read again.
func TestAccountErrorClearsWhenTheAccountIsReadAgain(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	e.f.mu.Lock()
	e.f.ccuRaw = `[1,2]` // not ccu-info: CodeAPIChanged stops the dispatch
	e.f.mu.Unlock()
	if ok, _ := e.b.CanDispatch(ctx, 600); ok {
		t.Fatal("dispatched although the account answer is not ccu-info")
	}
	if st := e.b.Status(ctx); st.LastError == nil || st.LastError.Code != CodeAPIChanged {
		t.Fatalf("last error %+v, want %s", st.LastError, CodeAPIChanged)
	}
	e.f.mu.Lock()
	e.f.ccuRaw = ""
	e.f.mu.Unlock()
	e.clk.Advance(2 * time.Minute) // past the cached reading
	if ok, _ := e.b.CanDispatch(ctx, 600); !ok {
		t.Fatal("not dispatched after the account answered again")
	}
	if st := e.b.Status(ctx); st.LastError != nil {
		t.Errorf("the account's error stays after a good reading: %+v", st.LastError)
	}
}

// A lecture's own failure is not an account error: a good reading of the
// account leaves it.
func TestAccountReadingKeepsALectureError(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	e.f.setupErr = "no_gpu"
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); Code(err) != CodeRemoteFailed {
		t.Fatalf("err %v", err)
	}
	e.b.handbacksSettled()
	e.clk.Advance(2 * time.Minute)
	if ok, _ := e.b.CanDispatch(ctx, 600); !ok {
		t.Fatal("not dispatched")
	}
	if st := e.b.Status(ctx); st.LastError == nil || st.LastError.Code != CodeRemoteFailed {
		t.Errorf("last error %+v, want the lecture's until a session starts", st.LastError)
	}
}

// Status.Runtime: the runtime is held and billed; its resources endpoint
// answering 502 makes it unreachable (with the time it began), an answer
// makes it reachable again, an old observation is no observation, and the
// release takes the section away. Nothing is released because of it.
func TestStatusRuntimeHeldUnreachableBilling(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); err != nil {
		t.Fatal(err)
	}
	e.b.removalsSettled()
	e.b.SetViewers(func() int { return 1 })
	st := e.b.Status(ctx)
	if st.Tier != TierPaid {
		t.Fatalf("the fake account is %s, the test wants a paid one", st.Tier)
	}
	if st.Runtime == nil || !st.Runtime.Held || !st.Runtime.Billing || st.Runtime.Reachable != ReachOK {
		t.Fatalf("runtime %+v after a lecture, want held, billed and reachable", st.Runtime)
	}
	e.clk.Advance(10 * time.Second)
	e.f.fault(fakeFault{method: "GET", suffix: "/api/colab/resources", times: -1, status: 502})
	e.b.pollResources(ctx)
	since := e.clk.Now()
	e.clk.Advance(5 * time.Second)
	e.b.pollResources(ctx)
	st = e.b.Status(ctx)
	if r := st.Runtime; r == nil || !r.Held || !r.Billing || r.Reachable != ReachUnreachable ||
		r.UnreachableSince == nil || !r.UnreachableSince.Equal(since) {
		t.Fatalf("runtime %+v, want held, billed, unreachable since %s", st.Runtime, since)
	}
	if st.State != StateRunning || e.f.wasReleased() {
		t.Errorf("state %s, released %v: an unreachable runtime is only reported", st.State, e.f.wasReleased())
	}
	// it answers again
	e.f.clearFaults()
	e.f.mu.Lock()
	e.f.resources = map[string]any{"memory": map[string]any{"totalBytes": 8 << 30, "freeBytes": 4 << 30}, "gpus": []any{}}
	e.f.mu.Unlock()
	e.clk.Advance(5 * time.Second)
	e.b.pollResources(ctx)
	if r := e.b.Status(ctx).Runtime; r == nil || r.Reachable != ReachOK || r.UnreachableSince != nil {
		t.Errorf("runtime %+v after it answered, want reachable", r)
	}
	// nobody asked for a while
	e.clk.Advance(reachStale + time.Second)
	if r := e.b.Status(ctx).Runtime; r == nil || r.Reachable != ReachUnknown || !r.Held {
		t.Errorf("runtime %+v after %s without a reading, want unknown", r, reachStale)
	}
	if err := e.b.ShutdownNow(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.b.Status(ctx); st.Runtime != nil || st.SessionStartedAt != nil {
		t.Errorf("after the release: runtime %+v, session started %v", st.Runtime, st.SessionStartedAt)
	}
}

// The free level is not billed: it only uses up its window.
func TestRuntimeInfoFreeLevelIsNotBilled(t *testing.T) {
	e := newFakeEnv(t, t4())
	s := &liveSession{rec: sessionRecord{Endpoint: "gpu-1", AssignedAt: e.clk.Now()}}
	e.b.mu.Lock()
	free, paid := e.b.runtimeInfoLocked(s, false), e.b.runtimeInfoLocked(s, true)
	queued := e.b.runtimeInfoLocked(&liveSession{}, true)
	e.b.mu.Unlock()
	if !free.Held || free.Billing || !paid.Billing {
		t.Errorf("free %+v, paid %+v", free, paid)
	}
	if queued.Held || queued.Billing || queued.Reachable != ReachUnknown {
		t.Errorf("a session still waiting for its runtime: %+v", queued)
	}
}

// heldRelease makes the fake hold the release; the returned function lets
// it go (also at the end of the test).
func heldRelease(t *testing.T, f *fakeColab) (arrived chan struct{}, letGo func()) {
	gate := make(chan struct{})
	arrived = make(chan struct{})
	f.mu.Lock()
	f.unassignGate, f.unassigning = gate, arrived
	f.mu.Unlock()
	var once sync.Once
	letGo = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(letGo)
	return arrived, letGo
}

// Quitting while a hand-back is being released: Close waits for that
// release instead of leaving the runtime behind.
func TestCloseWaitsForTheBackgroundRelease(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	e.f.setupErr = "no_gpu"
	arrived, letGo := heldRelease(t, e.f)
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); Code(err) != CodeRemoteFailed {
		t.Fatalf("err %v", err)
	}
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the background release never started")
	}
	closed := make(chan error, 1)
	go func() { closed <- e.b.Close(ctx) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while the runtime was still being released", err)
	case <-time.After(300 * time.Millisecond):
	}
	letGo()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	if !e.f.wasReleased() {
		t.Error("the runtime was not released when Close returned")
	}
	if v, _, _ := e.kv.GetKV(ctx, KVSession); v != "" {
		t.Errorf("session record left: %s", v)
	}
}

// A quit that cannot wait leaves the session record in state stopping: the
// next start releases the runtime (cleanupLeftover).
func TestHandBackRecordLetsTheNextStartRelease(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	e.f.setupErr = "no_gpu"
	arrived, letGo := heldRelease(t, e.f)
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "l1"), nil); Code(err) != CodeRemoteFailed {
		t.Fatalf("err %v", err)
	}
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the background release never started")
	}
	v, _, _ := e.kv.GetKV(ctx, KVSession)
	var rec sessionRecord
	if json.Unmarshal([]byte(v), &rec) != nil || rec.State != StateStopping || rec.Endpoint == "" {
		t.Fatalf("session record %q, want one in state stopping with its endpoint", v)
	}
	// the next run of the app, the first one's release still hanging
	lg, _ := newTestLogger()
	o := e.b.o
	o.Logger, o.RunID = lg, "run-2"
	b2, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.bgCancel)
	e.f.mu.Lock()
	e.f.unassignGate = nil
	e.f.mu.Unlock()
	if err := b2.Startup(ctx); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if !e.f.wasReleased() {
		t.Error("the next start did not release the runtime")
	}
	letGo()
	e.b.handbacksSettled()
}

// The waits get up to 20 % longer (Options.Retry.Jitter, Rand).
func TestRetryJitter(t *testing.T) {
	e := newFakeEnv(t, t4(), func(o *Options) { o.Retry = RetryOptions{Rand: func() float64 { return 0.5 }} })
	e.f.fault(fakeFault{method: "PUT", suffix: "/audio.flac", times: 2, status: 503})
	if _, err := e.b.Transcribe(context.Background(), e.lecture(t, "l1"), nil); err != nil {
		t.Fatal(err)
	}
	if got, want := e.sleeps(), []time.Duration{5500 * time.Millisecond, 16500 * time.Millisecond}; !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	e.b.removalsSettled()
	o := RetryOptions{}.normalized()
	if !slices.Equal(o.Waits, []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}) || o.Jitter != 0.2 ||
		o.ProbeTimeout != 10*time.Second || o.Window != 60*time.Second {
		t.Errorf("defaults %+v", o)
	}
	for i := 0; i < 200; i++ {
		if d := o.wait(0); d < 5*time.Second || d > 6*time.Second {
			t.Fatalf("a first wait of %s", d)
		}
	}
}

// No wait goes on beyond the window after the first failure.
func TestReconnectorWindow(t *testing.T) {
	clk := newFakeClock()
	var seen []*remote.Reconnect
	rc := &reconnector{o: RetryOptions{Jitter: -1, Window: 12 * time.Second}.normalized(), now: clk.Now, sleep: clk.Sleep,
		notify: func(r *remote.Reconnect) { seen = append(seen, r) }}
	ctx := context.Background()
	if !rc.next(ctx) || !rc.next(ctx) {
		t.Fatal("the first two waits fit the window")
	}
	if rc.next(ctx) {
		t.Error("a third wait after the window was used up")
	}
	if want := []time.Duration{5 * time.Second, 7 * time.Second}; !slices.Equal(clk.sleeps, want) {
		t.Errorf("waits %v, want %v", clk.sleeps, want)
	}
	if len(seen) != 3 || seen[0].Attempt != 1 || seen[1].Attempt != 2 || seen[2] != nil {
		t.Errorf("notifications %+v, want two attempts and the end", seen)
	}
	if rc.active() {
		t.Error("still active after giving up")
	}
	// a new sequence starts from the first wait
	if !rc.next(ctx) || clk.sleeps[2] != 5*time.Second {
		t.Errorf("waits %v, want a new sequence to begin with 5 s", clk.sleeps)
	}
	// a cancelled context ends it
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if rc.next(cctx) || rc.active() {
		t.Error("a wait under a cancelled context")
	}
}

// scriptConn is a kernel channel of the test below: its messages are read
// in order, then the connection breaks.
type scriptConn struct {
	msgs  [][]byte
	wrote chan []byte // gets the request (may be nil)
}

func (c *scriptConn) ReadMessage(context.Context) (bool, []byte, error) {
	if len(c.msgs) == 0 {
		return false, nil, errWSClosed
	}
	m := c.msgs[0]
	c.msgs = c.msgs[1:]
	return false, m, nil
}

func (c *scriptConn) WriteText(_ context.Context, b []byte) error {
	if c.wrote != nil {
		c.wrote <- b
	}
	return nil
}

func (c *scriptConn) Close() error { return nil }

func kernelReply(parent, channel, typ string, content map[string]any) []byte {
	cb, _ := json.Marshal(content)
	b, _ := json.Marshal(kernelMsg{Header: map[string]any{"msg_id": newUUID(), "msg_type": typ},
		ParentHeader: map[string]any{"msg_id": parent}, Metadata: map[string]any{}, Content: cb, Channel: channel, Buffers: []any{}})
	return b
}

// The kernel channels reconnect with the same waits as a failed request:
// 5 s, 15 s, 30 s; a failed connection uses up an attempt; the request's
// messages arriving again end the sequence, so a later break starts with
// 5 s again; three attempts without them lose the connection.
func TestKernelChannelsReconnectWithTheRetryWaits(t *testing.T) {
	clk := newFakeClock()
	var notes []int
	newRC := func() *reconnector {
		return &reconnector{o: RetryOptions{Jitter: -1}.normalized(), now: clk.Now, sleep: clk.Sleep,
			notify: func(r *remote.Reconnect) {
				if r == nil {
					notes = append(notes, 0)
				} else {
					notes = append(notes, r.Attempt)
				}
			}}
	}
	refused := func() (MessageConn, error) { return nil, &netError{text: "websocket handshake: refused"} }
	base, _ := url.Parse("http://runtime.invalid/rt")
	rt := &runtimeClient{base: base, token: "x", agent: "t"}
	ctx := context.Background()
	noOut := func(string) error { return nil }

	wrote := make(chan []byte, 1)
	var parent string
	dials := 0
	rt.dial = func(context.Context, string, http.Header) (MessageConn, error) {
		dials++
		switch dials {
		case 1: // the request goes out, then the connection breaks
			return &scriptConn{wrote: wrote}, nil
		case 2: // the first reconnection fails
			var m kernelMsg
			_ = json.Unmarshal(<-wrote, &m)
			parent, _ = m.Header["msg_id"].(string)
			return refused()
		case 3: // the second works: one line of output, then it breaks again
			return &scriptConn{msgs: [][]byte{kernelReply(parent, "iopub", "stream", map[string]any{"name": "stdout", "text": "a\n"})}}, nil
		}
		return &scriptConn{msgs: [][]byte{ // the cell ends
			kernelReply(parent, "iopub", "status", map[string]any{"execution_state": "idle"}),
			kernelReply(parent, "shell", "execute_reply", map[string]any{"status": "ok"})}}, nil
	}
	var out strings.Builder
	res, err := rt.kernelExec(ctx, "k1", "pass", func(s string) error { out.WriteString(s); return nil }, newRC(), nil)
	if err != nil || res.Status != "ok" || out.String() != "a\n" {
		t.Fatalf("kernelExec: %+v, %v, output %q", res, err, out.String())
	}
	if want := []time.Duration{5 * time.Second, 15 * time.Second, 5 * time.Second}; !slices.Equal(clk.sleeps, want) {
		t.Errorf("waits %v, want %v", clk.sleeps, want)
	}
	if want := []int{1, 2, 0, 1, 0}; !slices.Equal(notes, want) {
		t.Errorf("notifications %v, want %v", notes, want)
	}

	// never again: three waits, then the connection is lost
	clk.sleeps, dials = nil, 0
	rt.dial = func(context.Context, string, http.Header) (MessageConn, error) {
		dials++
		if dials == 1 {
			return &scriptConn{}, nil
		}
		return refused()
	}
	_, err = rt.kernelExec(ctx, "k1", "pass", noOut, newRC(), nil)
	if Code(err) != CodeConnectionLost || !IsFallback(err) {
		t.Fatalf("err %v, want %s", err, CodeConnectionLost)
	}
	if want := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}; !slices.Equal(clk.sleeps, want) || dials != 4 {
		t.Errorf("waits %v with %d connections, want %v with 4", clk.sleeps, dials, want)
	}

	// the first connection fails once: nothing was sent yet, it is tried again
	clk.sleeps, dials = nil, 0
	rt.dial = func(context.Context, string, http.Header) (MessageConn, error) {
		dials++
		if dials == 1 {
			return refused()
		}
		return &scriptConn{}, nil
	}
	_, _ = rt.kernelExec(ctx, "k1", "pass", noOut, newRC(), nil)
	if len(clk.sleeps) == 0 || clk.sleeps[0] != 5*time.Second || dials < 2 {
		t.Errorf("waits %v with %d connections, want a first wait of 5 s and a second connection", clk.sleeps, dials)
	}

	// a refusal that is not transient is not retried
	clk.sleeps = nil
	rt.dial = func(context.Context, string, http.Header) (MessageConn, error) {
		return nil, &httpError{endpoint: "api/kernels/channels", status: 404}
	}
	if _, err := rt.kernelExec(ctx, "k1", "pass", noOut, newRC(), nil); statusOf(err) != 404 || len(clk.sleeps) != 0 {
		t.Errorf("err %v after waits %v, want the 404 at once", err, clk.sleeps)
	}
}

// Two sources of one lecture reconnect at once (a request's retryOp, the
// kernel channel): the end of one does not hide the other's wait still
// going, on the card (jobState.reconnect, which Status.Jobs carries) nor in
// the events; the lecture shows nothing once both ended, whichever ends
// first. (Red before r4: both shared one field and the first end cleared
// it; review R3 suggestion 2, D13.)
func TestTwoReconnectingSourcesOfALecture(t *testing.T) {
	for _, firstEnds := range []string{"upload", "kernel"} {
		t.Run(firstEnds+" ends first", func(t *testing.T) {
			b, err := New(Options{KV: NewMemKV()})
			if err != nil {
				t.Fatal(err)
			}
			j := &jobState{id: "l1", key: "l1", cancel: func(error) {}, done: make(chan struct{}),
				ev: Event{Step: StepRemoteTranscribe, Fraction: 0.5}}
			b.mu.Lock()
			b.jobs = append(b.jobs, j)
			b.mu.Unlock()
			var last *Event
			jp := &jobProgress{b: b, j: j, est: -1, out: func(ev Event) { last = &ev }}
			shown := func(what string, want *remote.Reconnect) {
				t.Helper()
				b.mu.Lock()
				got := j.reconnect
				b.mu.Unlock()
				if got != want {
					t.Errorf("%s: the card shows %+v, want %+v", what, got, want)
				}
				if last == nil || last.Reconnect != want {
					t.Errorf("%s: the last event shows %+v, want %+v", what, last, want)
				}
			}
			upload, kernel := jp.reconnectNotify(), jp.reconnectNotify()
			ru := &remote.Reconnect{Attempt: 1, Of: 3}
			rk := &remote.Reconnect{Attempt: 1, Of: 3}
			upload(ru)
			shown("the upload reconnects", ru)
			kernel(rk)
			shown("both reconnect", rk)
			ru2 := &remote.Reconnect{Attempt: 2, Of: 3}
			upload(ru2) // the upload's next attempt: the kernel channel began later
			shown("the upload's second attempt", rk)
			if firstEnds == "upload" {
				upload(nil)
				shown("the upload answers again", rk)
				kernel(nil)
			} else {
				kernel(nil)
				shown("the kernel channel is back", ru2)
				upload(nil)
			}
			shown("both ended", nil)
			upload(nil) // a second end changes nothing
			shown("ended twice", nil)
		})
	}
}
