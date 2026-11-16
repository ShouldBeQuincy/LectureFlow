// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Probing and retrying in place (v3.2.x P2 Colab; design note 3.4, the
// user's Q11a; SPEC-v3 V14.3 step 5 as narrowed with the user's approval:
// a transient failure is not yet a failed lecture while the probe window
// lasts, and the runtime is released at once when it is used up).
//
// Before, one failed request to the runtime handed the lecture back for
// good and released the runtime. Now a transient failure (transient: a
// transport error, a timeout, 429, 5xx) of an idempotent request of a
// running lecture — the directories, the worker scripts, the audio,
// job.json, the result, listing and starting kernels — is followed by up to
// len(Waits) rounds of: wait (5 s, 15 s, 30 s, each up to 20 % longer),
// probe the runtime (GET api/kernels, ProbeTimeout), and when it answers do
// the failed step again. The waits end Window after the first failure.
// 401 / 403 / 404 / 410, an answer that does not look like the API
// (CodeAPIChanged) and output outside the contract are never retried: they
// hand the lecture back at once, as before (V14.6).
//
// Fetching a new runtime proxy token (Backend.runtime, before a step or a
// probe once the token is close to expiry: a long lecture comes to it on
// the way) asks Colab's tunnel (GET tun/m/assignments), and Google's token
// endpoint first when the access token is close to its own expiry. When
// that fails transiently (refreshTransient) while the current token still
// has proxyStaleUse left, the step simply goes on with the current token
// (Backend.runtime, review R65 S1). Otherwise it is retried in the same way,
// in the same sequence as the step's own failures: the same waits, at most
// len(Waits) probes for both together, the same window — never more than a
// step got before. Before, one such failure (a single 503) handed the
// lecture back and the compute units it had used were lost (review R52 T2,
// task R6-H). Its other failures still hand the lecture back at once: 401 /
// 403 (after the tunnel's own retry with a fresh access token), a missing
// or ended credential, an answer of an unexpected shape, the runtime no
// longer assigned.
//
// The audio upload is retried from its first chunk: in the chunked protocol
// chunk 1 creates the file and the later ones append, so a chunk whose
// answer was lost would be appended twice if only it were sent again.
//
// The kernel channels' reconnection (kernelExec) uses the same waits and
// window. While a lecture reconnects, its events and Status.Jobs carry
// Reconnect, and Status.Runtime says the runtime is unreachable.

import (
	"context"
	"math/rand/v2"
	"time"

	"lectureflow/internal/remote"
)

// RetryOptions are the parameters of the reconnection (Options.Retry). They
// are not settings: the defaults are the user's decision (Q11a).
type RetryOptions struct {
	// Waits before each probe (nil = 5 s, 15 s, 30 s); their number is the
	// number of probes.
	Waits []time.Duration
	// Jitter lengthens each wait by up to this fraction (0 = 0.2; negative
	// = none).
	Jitter float64
	// ProbeTimeout of one probe (0 = 10 s).
	ProbeTimeout time.Duration
	// Window: no wait goes on beyond this long after the first failure
	// (0 = 60 s); the last probe may end up to ProbeTimeout later.
	Window time.Duration
	// Rand returns a number in [0, 1) for the jitter (nil = math/rand).
	Rand func() float64
}

func (o RetryOptions) normalized() RetryOptions {
	if o.Waits == nil {
		o.Waits = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}
	}
	switch {
	case o.Jitter == 0:
		o.Jitter = 0.2
	case o.Jitter < 0:
		o.Jitter = 0
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 10 * time.Second
	}
	if o.Window <= 0 {
		o.Window = 60 * time.Second
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	return o
}

// wait is the i-th wait (from 0) with its jitter.
func (o RetryOptions) wait(i int) time.Duration {
	d := o.Waits[i]
	if o.Jitter > 0 {
		d += time.Duration(float64(d) * o.Jitter * o.Rand())
	}
	return d
}

// reconnector is one sequence of reconnection attempts: from the first
// failure until the step works again (reset) or the attempts or the window
// are used up. It is shared by the requests (retryOp) and the kernel
// channels (kernelExec).
type reconnector struct {
	o     RetryOptions
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	// notify (may be nil) is told the attempt that begins (r) and, once the
	// sequence has ended either way, nil.
	notify func(r *remote.Reconnect)

	first   time.Time // the first failure of this sequence (zero: none)
	attempt int
	shown   bool // notify was told an attempt and not yet nil
}

// newReconnector is a sequence with the backend's parameters; notify may be
// nil (the memory probe, the removal cell: nothing to show).
func (b *Backend) newReconnector(notify func(*remote.Reconnect)) *reconnector {
	return &reconnector{o: b.o.Retry, now: b.o.Now, sleep: b.o.Sleep, notify: notify}
}

// active: a failure was seen and not yet overcome.
func (rc *reconnector) active() bool { return rc != nil && rc.attempt > 0 }

// next waits before the next probe. false: the attempts or the window are
// used up, or ctx ended (the caller checks ctx): the sequence is over.
func (rc *reconnector) next(ctx context.Context) bool {
	if rc == nil || rc.attempt >= len(rc.o.Waits) {
		rc.reset()
		return false
	}
	now := rc.now()
	if rc.first.IsZero() {
		rc.first = now
	}
	w := min(rc.o.wait(rc.attempt), rc.first.Add(rc.o.Window).Sub(now))
	if w <= 0 {
		rc.reset()
		return false
	}
	rc.attempt++
	if rc.notify != nil {
		rc.shown = true
		rc.notify(&remote.Reconnect{Attempt: rc.attempt, Of: len(rc.o.Waits), NextAt: now.Add(w).UTC()})
	}
	if rc.sleep(ctx, w) != nil {
		rc.reset()
		return false
	}
	return true
}

// hide tells notify that nothing is waited for any more (the runtime
// answered the probe and the step runs again); the sequence goes on: a
// step that fails again continues with the next attempt.
func (rc *reconnector) hide() {
	if rc != nil && rc.shown {
		rc.shown = false
		rc.notify(nil)
	}
}

// reset ends the sequence: the step worked again, or it is given up.
func (rc *reconnector) reset() {
	if rc == nil {
		return
	}
	rc.hide()
	rc.first, rc.attempt = time.Time{}, 0
}

// retryOp runs one idempotent step of a running lecture on session s's
// runtime (op gets a client with a valid proxy token), reconnecting after a
// transient failure of the step or of fetching that token, as the file
// comment says. what names the step in the log (a fixed word). jp (may be
// nil) shows the reconnection. The error is the step's (or its token's)
// last one, or the probe's when that is not worth another attempt (the
// runtime is gone or refuses us: no further attempt).
func (b *Backend) retryOp(ctx context.Context, s *liveSession, jp *jobProgress, what string, op func(rt *runtimeClient) error) error {
	var notify func(*remote.Reconnect)
	if jp != nil {
		notify = jp.reconnectNotify()
	}
	rc := b.newReconnector(notify)
	for {
		rt, err := b.runtime(ctx, s)
		refresh := err != nil // fetching the token failed, not the step
		if err == nil {
			err = op(rt)
		}
		if err == nil {
			if rc.active() {
				b.log.Info("colab: the runtime answers again; the lecture goes on", "step", what, "attempts", rc.attempt)
				rc.reset()
			}
			b.noteReach(s, true)
			return nil
		}
		if ctx.Err() != nil || !retryable(err, refresh) {
			rc.reset()
			return err
		}
		b.noteReach(s, false)
		for {
			tried := rc.attempt
			if !rc.next(ctx) {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				b.log.Warn("colab: the runtime did not come back within the probe window; the lecture is handed back",
					"step", what, "token_refresh", refresh, "attempts", tried, "code", Code(codedHTTP("runtime", err)))
				return err
			}
			b.log.Warn("colab: a request to the runtime failed; it was probed after a wait", "step", what,
				"token_refresh", refresh, "attempt", tried+1, "of", len(rc.o.Waits), "code", Code(codedHTTP("runtime", err)))
			prefresh, perr := b.probe(ctx, s)
			if perr == nil {
				rc.hide()
				break // it answers: the step again
			}
			if ctx.Err() != nil {
				rc.reset()
				return ctx.Err()
			}
			if !retryable(perr, prefresh) {
				rc.reset()
				return perr
			}
		}
	}
}

// retryable: a failure worth a wait, a probe and another attempt. A
// request to the runtime: transient, without a code (a coded failure is a
// decision: the runtime is gone, the answer is not the API's). Fetching a
// new proxy token (refresh: Backend.runtime failed, not the request): also
// refreshTransient.
func retryable(err error, refresh bool) bool {
	return Code(err) == "" && transient(err) || refresh && refreshTransient(err)
}

// refreshTransient: fetching a new runtime proxy token (Backend.runtime)
// failed in a way worth another attempt: Colab's tunnel (GET
// tun/m/assignments) did not answer, timed out, or answered 429 / 5xx
// (codedHTTP: CodeUnavailable), or Google's token endpoint did so while the
// access token for that request was renewed (authManager.refresh:
// CodeAuthEndpoint). Its other failures are decisions: 401 / 403
// (CodeAuthRejected, after the tunnel's own retry with a fresh access
// token), a missing or ended credential, an answer of an unexpected shape
// (CodeAPIChanged), the runtime no longer assigned (CodeSessionReclaimed).
func refreshTransient(err error) bool {
	switch Code(err) {
	case CodeUnavailable, CodeAuthEndpoint:
		return true
	}
	return false
}

// probe asks the runtime whether it answers (GET api/kernels, the boot's
// readiness probe) within ProbeTimeout. refresh: err is fetching a new proxy
// token's (Backend.runtime), not the runtime's.
func (b *Backend) probe(ctx context.Context, s *liveSession) (refresh bool, err error) {
	pctx, cancel := context.WithTimeout(ctx, b.o.Retry.ProbeTimeout)
	defer cancel()
	rt, err := b.runtime(pctx, s)
	refresh = err != nil
	if err == nil {
		_, err = rt.listKernels(pctx)
	}
	if err == nil {
		b.noteReach(s, true)
	} else if ctx.Err() == nil && (transient(err) || refresh && refreshTransient(err)) {
		b.noteReach(s, false)
	}
	return refresh, err
}

// ---- is the runtime reachable (Status.Runtime) ----

// Reachability of the held runtime (RuntimeInfo.Reachable).
const (
	ReachOK          = "ok"
	ReachUnreachable = "unreachable"
	ReachUnknown     = "unknown"
)

// reachStale: an observation older than this says nothing about now (no
// lecture runs and no front end watches: nothing asks the runtime).
const reachStale = 30 * time.Second

// reachState is the last observation of session's runtime.
type reachState struct {
	ok    bool
	at    time.Time // of the last observation (zero: none)
	since time.Time // unreachable since
}

// noteReach records that s's runtime answered (ok) or failed transiently.
func (b *Backend) noteReach(s *liveSession, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sess != s || s == nil {
		return
	}
	now := b.now()
	if ok {
		b.reach = reachState{ok: true, at: now}
		return
	}
	if b.reach.at.IsZero() || b.reach.ok {
		b.reach.since = now
	}
	b.reach.ok, b.reach.at = false, now
}

// RuntimeInfo is Status.Runtime: present while a session exists. The card
// warns when Held and Reachable is unreachable (still billed, or still
// using the session's fair-use window); nothing is released because of it.
type RuntimeInfo struct {
	// Held: a runtime is assigned to us (from the assignment until the
	// release succeeded).
	Held bool `json:"held"`
	// Reachable: ReachOK, ReachUnreachable, or ReachUnknown (nothing asked
	// the runtime lately).
	Reachable        string     `json:"reachable"`
	UnreachableSince *time.Time `json:"unreachable_since,omitempty"`
	// Billing: Held on a paid account (compute units are being spent; the
	// free level only uses up its window).
	Billing bool `json:"billing"`
}

// runtimeInfoLocked is Status.Runtime of session s.
func (b *Backend) runtimeInfoLocked(s *liveSession, paid bool) *RuntimeInfo {
	ri := &RuntimeInfo{Held: !s.rec.AssignedAt.IsZero() && s.rec.Endpoint != "", Reachable: ReachUnknown}
	ri.Billing = ri.Held && paid
	// While stopping (a release that keeps failing), a "cannot reach"
	// observation does not expire: the runtime is still held and billed,
	// and nothing samples it any more (review R3 suggestion 3).
	keep := s.rec.State == StateStopping && !b.reach.ok
	if !ri.Held || b.reach.at.IsZero() || !keep && b.now().Sub(b.reach.at) > reachStale {
		return ri
	}
	if b.reach.ok {
		ri.Reachable = ReachOK
		return ri
	}
	ri.Reachable = ReachUnreachable
	t := b.reach.since.UTC()
	ri.UnreachableSince = &t
	return ri
}

// ---- hand-backs in the background ----

// handBack releases session s's runtime after a lecture failed on it, in
// the background: the lecture's Transcribe returns at once (it leaves
// Status.Jobs and goes back to the local queue) instead of waiting for the
// cleanup and the release, which take a minute or two when the network is
// the reason. The session is stopping from now on: nothing is dispatched to
// it (CanDispatch, acquire) and the card shows the release. jobDir ("" =
// none) has its files deleted first, one attempt each, as before.
//
// Close and every other release wait for this one (relMu) or do it
// themselves; a release that fails stays "stopping" for Tick, and the
// session record lets the next start release what a quit left behind.
// The returned channel is closed when the background part has ended (at
// once when s is not the backend's session).
func (b *Backend) handBack(s *liveSession, jobDir, reason string) <-chan struct{} {
	done := make(chan struct{})
	b.mu.Lock()
	if b.sess != s || s == nil {
		b.mu.Unlock()
		close(done)
		return done
	}
	s.rec.State = StateStopping
	s.handBackReason = reason
	b.mu.Unlock()
	b.persist(context.Background(), s)
	// T051: nothing is dispatched to s any more; marked all the same
	b.markBusyDir(s, jobDir)
	b.changed() // the card shows the lecture gone and the release under way now
	b.handbackWG.Add(1)
	go func() {
		defer b.handbackWG.Done()
		defer close(done)
		if jobDir != "" {
			// ends with the app: Close releases the runtime itself
			left := b.cleanupRemote(b.bg, s, jobDir, false)
			b.settleBusyDir(s, jobDir, left == "" && b.bg.Err() == nil)
		}
		if b.handBackHook != nil {
			b.handBackHook()
		}
		// s only: when another release came first and a new session was
		// borrowed meanwhile, the new one is not this hand-back's.
		_ = b.releaseWith(context.Background(), reason, releaseOpts{expect: s})
		b.changed()
	}()
	return done
}

// cancelReleaseWait: how long a cancelled lecture's Transcribe waits for
// its runtime's release before leaving it to the background.
const cancelReleaseWait = 5 * time.Second

// releaseCancelled releases session s after the caller cancelled its
// lecture (V14.3 step 5: "用户取消时立即释放"). The release is a
// hand-back's: s is stopping and recorded so at once (nothing is
// dispatched to it), Close waits for the release or does it, a failed one
// stays for Tick, and the record lets the next start release what a quit
// left behind. Transcribe waits for it at most cancelWait: usually it has
// ended by then, as before; when Colab hangs, the cancel still takes effect
// within seconds and the release goes on with its own time limit (review
// R65 O1: it used to take up to a minute).
func (b *Backend) releaseCancelled(s *liveSession) {
	done := b.handBack(s, "", ReleaseCancelled)
	t := time.NewTimer(b.cancelWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		b.log.Info("colab: the cancelled lecture's runtime is still being released; the release goes on in the background")
	}
}

// handbacksSettled waits until every background hand-back has ended (tests).
func (b *Backend) handbacksSettled() { b.handbackWG.Wait() }
