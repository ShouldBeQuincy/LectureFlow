// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Fetching a new runtime proxy token in the middle of a lecture (task
// R6-H, review R52 T2): a transient failure of the refresh is retried in
// place with the step's waits, attempts and window; 401 / 403 and the other
// decisions still hand the lecture back at once. All on the stateful fake
// with the fake clock: nothing waits for real.

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// refreshMidLecture runs lecture l1 and holds it in its transcription, moves
// the clock to where the fake's proxy token (3600 s) is within
// proxyRefreshAhead of its end and its access token (3599 s) within
// accessSkew of its own, adds faults, and lets the lecture go on: the
// result's download fetches a new proxy token first, with a new access token
// for that request. probes is the count of GET api/kernels before the faults
// (the boot's).
func refreshMidLecture(t *testing.T, e *fakeEnv, faults ...fakeFault) (fts []*fakeFault, probes int, err error) {
	t.Helper()
	return refreshMidLectureWith(t, e, midLecture{faults: faults})
}

// midLecture is what refreshMidLectureWith does besides: advance is how far
// the clock moves (0: 59 minutes, which leaves the proxy token less than
// proxyStaleUse, so it cannot stand in for a new one); progress gets the
// lecture's events (nil: none); setup runs once the clock moved, before the
// faults are added.
type midLecture struct {
	advance  time.Duration
	progress func(Event)
	setup    func()
	faults   []fakeFault
}

func refreshMidLectureWith(t *testing.T, e *fakeEnv, m midLecture) (fts []*fakeFault, probes int, err error) {
	t.Helper()
	e.f.hold("l1")
	done := make(chan error, 1)
	j := e.lecture(t, "l1")
	go func() {
		_, err := e.b.Transcribe(context.Background(), j, m.progress)
		done <- err
	}()
	e.transcribingNow(t)
	if m.advance == 0 {
		m.advance = 59 * time.Minute
	}
	e.clk.Advance(m.advance)
	if m.setup != nil {
		m.setup()
	}
	probes = e.f.count(func() int { return e.f.probes })
	for _, ft := range m.faults {
		fts = append(fts, e.f.fault(ft))
	}
	e.f.release("l1")
	return fts, probes, waitErr(t, done)
}

const assignmentsPath = "/tun/m/assignments"

// The refresh fails once (or hangs until the probe's time limit), then
// works: the lecture ends on Colab, the runtime stays. (Red before R6-H: the
// coded failure of the refresh handed the lecture back at once.)
func TestTokenRefreshTransientFailureIsRetriedInPlace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		faults []fakeFault
		mod    func(*Options)
		waits  []time.Duration
	}{
		{name: "tunnel 503", faults: []fakeFault{{method: "GET", suffix: assignmentsPath, times: 1, status: 503}},
			waits: []time.Duration{5 * time.Second}},
		{name: "tunnel 429", faults: []fakeFault{{method: "GET", suffix: assignmentsPath, times: 1, status: 429}},
			waits: []time.Duration{5 * time.Second}},
		// (A lost connection of the tunnel's GET is not here: net/http sends
		// a GET again by itself when a reused connection breaks. The
		// timeout below and the token endpoint's lost POST are the
		// transport failures.)
		{name: "token endpoint 500", faults: []fakeFault{{method: "POST", suffix: "/oauth/token", times: 1, status: 500}},
			waits: []time.Duration{5 * time.Second}},
		{name: "token endpoint connection lost", faults: []fakeFault{{method: "POST", suffix: "/oauth/token", times: 1, drop: true}},
			waits: []time.Duration{5 * time.Second}},
		// The step's refresh answers 503; the first probe's refresh hangs
		// until the probe's time limit (a timeout); the second probe works.
		{name: "probe's refresh times out", faults: []fakeFault{
			{method: "GET", suffix: assignmentsPath, times: 1, status: 503},
			{method: "GET", suffix: assignmentsPath, times: 1, hang: true},
		}, mod: func(o *Options) { o.Retry.ProbeTimeout = 300 * time.Millisecond },
			waits: []time.Duration{5 * time.Second, 15 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mods []func(*Options)
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			e := newFakeEnv(t, t4(), mods...)
			ctx := context.Background()
			fts, probes, err := refreshMidLecture(t, e, tc.faults...)
			if err != nil {
				t.Fatalf("Transcribe after a transient failure of the token's refresh: %v (code %s)", err, Code(err))
			}
			for i, ft := range fts {
				if n := e.f.faultHits(ft); n != 1 {
					t.Errorf("fault %d struck %d times, want 1", i, n)
				}
			}
			if got := e.sleeps(); !slices.Equal(got, tc.waits) {
				t.Errorf("waits %v, want %v", got, tc.waits)
			}
			if n := e.f.count(func() int { return e.f.probes }); n != probes+1 {
				t.Errorf("%d probes reached the runtime, want 1", n-probes)
			}
			e.b.removalsSettled()
			if e.f.wasReleased() {
				t.Error("the runtime was released after a transient failure of the token's refresh")
			}
			if st := e.b.Status(ctx); st.State != StateRunning || st.LastError != nil {
				t.Errorf("state %s, last error %+v: want running and none", st.State, st.LastError)
			}
			if !strings.Contains(e.log.String(), "token_refresh=true") {
				t.Error("the log does not tell that the token's refresh failed")
			}
		})
	}
}

// The refresh keeps failing: the step's refresh and one per probe (5 s,
// 15 s, 30 s), four in all, then the lecture is handed back with the
// refresh's error and the runtime released — no more attempts than a step
// gets.
func TestTokenRefreshRetriesEndWithTheProbes(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	// times 4: a fifth attempt would get a token and the lecture would end
	// well; the release afterwards gets one too.
	fts, probes, err := refreshMidLecture(t, e, fakeFault{method: "GET", suffix: assignmentsPath, times: 4, status: 503})
	if !IsFallback(err) || Code(err) != CodeUnavailable {
		t.Fatalf("err %v (code %s), want a hand-back with %s", err, Code(err), CodeUnavailable)
	}
	if n := e.f.faultHits(fts[0]); n != 4 {
		t.Errorf("the token was asked for %d times, want 4 (the step's and three probes')", n)
	}
	if got, want := e.sleeps(), []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	if n := e.f.count(func() int { return e.f.probes }); n != probes {
		t.Errorf("%d probes reached the runtime without a token", n-probes)
	}
	if !strings.Contains(e.log.String(), "did not come back within the probe window") {
		t.Error("the log does not tell the hand-back")
	}
	e.b.handbacksSettled()
	st := e.b.Status(ctx)
	if !e.f.wasReleased() || st.State != StateReleased {
		t.Errorf("after the hand-back: released %v, state %s", e.f.wasReleased(), st.State)
	}
	if st.LastRelease == nil || st.LastRelease.Reason != ReleaseFailed {
		t.Errorf("last release %+v, want %s", st.LastRelease, ReleaseFailed)
	}
}

// 401 / 403 (the tunnel already tried a fresh access token once), an answer
// that is not the API's, a runtime no longer assigned, and a credential that
// is gone or whose lifecycle ended: no wait, no probe, handed back at once,
// as before.
func TestTokenRefreshDecisionsAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fault    *fakeFault
		setup    func(t *testing.T, e *fakeEnv) // once the clock moved
		hits     int
		code     string
		waitsFor string
	}{
		{name: "401", fault: &fakeFault{method: "GET", suffix: assignmentsPath, times: 2, status: 401}, hits: 2,
			code: CodeAuthRejected, waitsFor: model.WaitRemoteAuth},
		{name: "403", fault: &fakeFault{method: "GET", suffix: assignmentsPath, times: 2, status: 403}, hits: 2,
			code: CodeAuthRejected, waitsFor: model.WaitRemoteAuth},
		{name: "404", fault: &fakeFault{method: "GET", suffix: assignmentsPath, times: 1, status: 404}, hits: 1,
			code: CodeAPIChanged},
		{name: "reclaimed", setup: func(t *testing.T, e *fakeEnv) {
			// the account's listing has another runtime only
			e.f.mu.Lock()
			e.f.multi, e.f.live = true, []string{"gpu-fake-other"}
			e.f.mu.Unlock()
		}, code: CodeSessionReclaimed},
		// review R65 L1: the credential's own decisions (authManager.load)
		{name: "credential gone", setup: func(t *testing.T, e *fakeEnv) {
			if err := e.b.auth.deleteLocal(context.Background()); err != nil {
				t.Fatal(err)
			}
		}, code: CodeAuthMissing, waitsFor: model.WaitRemoteAuth},
		{name: "credential lifecycle ended", setup: func(t *testing.T, e *fakeEnv) {
			// a session credential of another run: no longer usable
			storeCredential(t, e.kv, Lifecycle{Kind: LifecycleSession}, e.clk.Now(), "run-0")
		}, code: CodeAuthExpired, waitsFor: model.WaitRemoteAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFakeEnv(t, t4())
			m := midLecture{}
			if tc.fault != nil {
				m.faults = append(m.faults, *tc.fault)
			}
			if tc.setup != nil {
				m.setup = func() { tc.setup(t, e) }
			}
			fts, _, err := refreshMidLectureWith(t, e, m)
			if !IsFallback(err) || Code(err) != tc.code || WaitingFor(err) != tc.waitsFor {
				t.Fatalf("err %v (code %s, waiting for %q), want a hand-back with %s (%q)", err, Code(err), WaitingFor(err), tc.code, tc.waitsFor)
			}
			if tc.fault != nil {
				if n := e.f.faultHits(fts[0]); n != tc.hits {
					t.Errorf("the token was asked for %d times, want %d", n, tc.hits)
				}
			}
			if got := e.sleeps(); len(got) != 0 {
				t.Errorf("waits %v, want none", got)
			}
			e.b.handbacksSettled()
		})
	}
}

// While the probes' refreshes fail, Status.Runtime says the runtime cannot
// be reached: each probe that fails so records it, not only the step's
// first failure (review R65 L2). With waits of 5 s, 40 s and 5 s the step's
// own record is 45 s old at the third wait, beyond reachStale; only the
// second probe's keeps it "unreachable".
func TestTokenRefreshFailuresOfTheProbesShowTheRuntimeUnreachable(t *testing.T) {
	e := newFakeEnv(t, t4(), func(o *Options) {
		o.Retry = RetryOptions{Jitter: -1, Waits: []time.Duration{5 * time.Second, 40 * time.Second, 5 * time.Second}}
	})
	ctx := context.Background()
	var mu sync.Mutex
	reach := map[int]string{} // attempt -> Status.Runtime.Reachable as it began
	progress := func(ev Event) {
		if ev.Reconnect == nil {
			return
		}
		st := e.b.Status(ctx)
		mu.Lock()
		defer mu.Unlock()
		if st.Runtime == nil {
			reach[ev.Reconnect.Attempt] = "none"
		} else {
			reach[ev.Reconnect.Attempt] = st.Runtime.Reachable
		}
	}
	// Google's token endpoint fails for the step's refresh and the first two
	// probes' (CodeAuthEndpoint: not transient() by itself); the third
	// probe gets a token.
	fts, _, err := refreshMidLectureWith(t, e, midLecture{progress: progress,
		faults: []fakeFault{{method: "POST", suffix: "/oauth/token", times: 3, status: 500}}})
	if err != nil {
		t.Fatalf("Transcribe: %v (code %s)", err, Code(err))
	}
	if n := e.f.faultHits(fts[0]); n != 3 {
		t.Errorf("the token endpoint failed %d times, want 3", n)
	}
	if got, want := e.sleeps(), []time.Duration{5 * time.Second, 40 * time.Second, 5 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	for a := 1; a <= 3; a++ {
		if reach[a] != ReachUnreachable {
			t.Errorf("Status.Runtime.Reachable %q as attempt %d began, want %q", reach[a], a, ReachUnreachable)
		}
	}
	e.b.removalsSettled()
}

// The limits stay a step's: a failed refresh and the step's own failures
// share one sequence of attempts, and the window ends the waits whatever
// failed.
func TestTokenRefreshRetriesStayWithinTheStepsLimits(t *testing.T) {
	t.Run("one sequence with the step's failures", func(t *testing.T) {
		e := newFakeEnv(t, t4())
		// the refresh fails once, then the download itself every time
		fts, _, err := refreshMidLecture(t, e,
			fakeFault{method: "GET", suffix: assignmentsPath, times: 1, status: 503},
			fakeFault{method: "GET", suffix: "/l1/result.json", times: -1, status: 503})
		if !IsFallback(err) {
			t.Fatalf("err %v (code %s), want a hand-back", err, Code(err))
		}
		if got, want := e.sleeps(), []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}; !slices.Equal(got, want) {
			t.Errorf("waits %v, want %v (three in all, not three more after the refresh)", got, want)
		}
		if n := e.f.faultHits(fts[1]); n != 3 {
			t.Errorf("the result was asked for %d times, want 3 (after each probe)", n)
		}
		e.b.handbacksSettled()
	})
	t.Run("window", func(t *testing.T) {
		e := newFakeEnv(t, t4(), func(o *Options) { o.Retry = RetryOptions{Jitter: -1, Window: 12 * time.Second} })
		// times 3: the step's refresh and two probes'; a later attempt would
		// get a token
		fts, _, err := refreshMidLecture(t, e, fakeFault{method: "GET", suffix: assignmentsPath, times: 3, status: 503})
		if !IsFallback(err) || Code(err) != CodeUnavailable {
			t.Fatalf("err %v (code %s), want a hand-back with %s", err, Code(err), CodeUnavailable)
		}
		got := e.sleeps()
		if want := []time.Duration{5 * time.Second, 7 * time.Second}; !slices.Equal(got, want) {
			t.Errorf("waits %v, want %v (the second one cut at the window's end)", got, want)
		}
		var sum time.Duration
		for _, d := range got {
			sum += d
		}
		if sum > 12*time.Second {
			t.Errorf("waited %s in all, beyond the 12 s window", sum)
		}
		if n := e.f.faultHits(fts[0]); n != 3 {
			t.Errorf("the token was asked for %d times, want 3", n)
		}
		e.b.handbacksSettled()
	})
}

// The refresh begins proxyRefreshAhead before the token ends; when it fails
// transiently and the token still has proxyStaleUse or more, the step goes
// on with it: no wait at all (review R65 S1; red before: the 503 started the
// waits). Decisions still hand the lecture back at once.
func TestTokenRefreshFailureKeepsTheCurrentTokenWhileItLasts(t *testing.T) {
	const fourMinutesLeft = 56 * time.Minute // of the fake's 3600 s token
	t.Run("503", func(t *testing.T) {
		e := newFakeEnv(t, t4())
		ctx := context.Background()
		fts, _, err := refreshMidLectureWith(t, e, midLecture{advance: fourMinutesLeft,
			faults: []fakeFault{{method: "GET", suffix: assignmentsPath, times: 1, status: 503}}})
		if err != nil {
			t.Fatalf("Transcribe: %v (code %s)", err, Code(err))
		}
		if n := e.f.faultHits(fts[0]); n != 1 {
			t.Errorf("the fault struck %d times, want 1", n)
		}
		if got := e.sleeps(); len(got) != 0 {
			t.Errorf("waits %v, want none: the current token still serves", got)
		}
		e.b.removalsSettled()
		if st := e.b.Status(ctx); e.f.wasReleased() || st.State != StateRunning || st.LastError != nil {
			t.Errorf("released %v, state %s, last error %+v", e.f.wasReleased(), st.State, st.LastError)
		}
		if n := strings.Count(e.log.String(), "the current one is used"); n != 1 {
			t.Errorf("the log tells %d times that the current token was kept, want once", n)
		}
	})
	for _, tc := range []struct {
		name  string
		fault *fakeFault
		setup func(e *fakeEnv)
		code  string
	}{
		{name: "401", fault: &fakeFault{method: "GET", suffix: assignmentsPath, times: 2, status: 401}, code: CodeAuthRejected},
		{name: "reclaimed", setup: func(e *fakeEnv) {
			e.f.mu.Lock()
			e.f.multi, e.f.live = true, []string{"gpu-fake-other"}
			e.f.mu.Unlock()
		}, code: CodeSessionReclaimed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFakeEnv(t, t4())
			m := midLecture{advance: fourMinutesLeft}
			if tc.fault != nil {
				m.faults = append(m.faults, *tc.fault)
			}
			if tc.setup != nil {
				m.setup = func() { tc.setup(e) }
			}
			_, _, err := refreshMidLectureWith(t, e, m)
			if !IsFallback(err) || Code(err) != tc.code {
				t.Fatalf("err %v (code %s), want a hand-back with %s", err, Code(err), tc.code)
			}
			if got := e.sleeps(); len(got) != 0 {
				t.Errorf("waits %v, want none", got)
			}
			e.b.handbacksSettled()
		})
	}
}
