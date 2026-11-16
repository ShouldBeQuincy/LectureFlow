// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// One borrowed runtime ("一次借卡", V14.3): waiting for the accelerator
// with back-off, boot, the runtime proxy token, release, and the record
// that lets the next start release a session a crash left behind.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"lectureflow/internal/model"
)

// Session states (V14.5: 排队等卡 / 开机中 / 运行中 / 关机中 / 已释放).
const (
	StateNone     = "none"
	StateQueued   = "queued"
	StateBooting  = "booting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateReleased = "released"
)

// Release reasons (Status.LastRelease.Reason; V14.3 step 5).
const (
	ReleaseQueueEmpty   = "queue_empty"   // the scheduler said the queue is empty
	ReleaseIdle         = "idle"          // nothing to do for IdleReleaseMin
	ReleaseWindow       = "window"        // the fair-use window is used up
	ReleaseFailed       = "failed"        // a lecture failed on the runtime
	ReleaseCancelled    = "cancelled"     // the user cancelled the lecture
	ReleaseAfterCurrent = "after_current" // "处理完当前这节后关机"
	ReleaseNow          = "shutdown_now"  // "立即关机"
	ReleaseAuthEnding   = "auth_ending"   // the credential's lifecycle is about to end
	ReleaseDisconnect   = "disconnect"    // "断开连接"
	ReleaseAppExit      = "app_exit"      // the app is closing
	ReleaseLeftover     = "leftover"      // a session of an earlier run, released at start
	ReleaseReclaimed    = "reclaimed"     // Colab took the runtime back
	// ReleaseCleanupFailed: a file of the finished lecture (the uploaded
	// audio, result.json) could not be deleted on the runtime even after
	// retries; releasing the runtime removes its disk (Gate round 1 item 8).
	// ReleaseInfo.File names the file.
	ReleaseCleanupFailed = "cleanup_failed"
)

const (
	recordVersion     = 1
	proxyRefreshAhead = 5 * time.Minute // upstream common.py TOKEN_REFRESH_MARGIN
	// proxyStaleUse: after a transient failure of the refresh, the current
	// proxy token still serves a step that begins at least this long
	// before the token ends (runtime). Below it a step could outlast the
	// token (an audio upload takes a minute or more) and be refused as if
	// the runtime were gone: the failure goes to the retry instead.
	proxyStaleUse  = 2 * time.Minute
	releaseTimeout = 60 * time.Second
)

// sessionRecord is kv colab.session: no token, only what releasing needs.
type sessionRecord struct {
	Version      int       `json:"v"`
	State        string    `json:"state"`
	NotebookHash string    `json:"nbh"`
	Accelerator  string    `json:"accelerator"`
	Shape        string    `json:"shape,omitempty"`
	Endpoint     string    `json:"endpoint,omitempty"`
	RequestedAt  time.Time `json:"requested_at"`
	AssignedAt   time.Time `json:"assigned_at,omitzero"`
	ReadyAt      time.Time `json:"ready_at,omitzero"`
	RunID        string    `json:"run_id"`
	Jobs         int       `json:"jobs"`
}

// liveSession is the runtime of this run (in memory).
type liveSession struct {
	rec        sessionRecord
	rt         *runtimeClient
	proxyExp   time.Time
	proxyStale bool   // rt's token was kept after a failed refresh (logged once per token)
	kernelID   string // the first kernel (the memory probe's)
	workerHash string
	setupDone  bool
	lastJobEnd time.Time

	// colab-batch (job.go, batch.go)
	// ready is closed when the borrowing that created the session ended
	// (running, or given up): a second lecture of the larger GPUs waits for
	// it instead of being sent back.
	ready    chan struct{}
	kernels  []string // every kernel started on the runtime: all are shut down at the release
	idle     []string // kernels without a running cell
	prep     *sessionPrep
	workers  int        // the resident worker's --workers
	upMu     sync.Mutex // one audio upload at a time: the uplink is the bottleneck (colab-prof §4.5)
	uploads  map[string]*preUpload
	upFailed map[string]bool
	// leftFile: a file of a job that is gone (cancelled, left the plan,
	// finished) could not be deleted: the runtime is released once no
	// lecture runs.
	leftFile string
	// removals: finished lectures' directories being removed in the
	// background (removal.go); a release waits for them.
	removals []*removal
	// busyDirs (T051): job directories whose files are being deleted in
	// the background, or could not all be deleted. A later attempt of the
	// same lecture on this runtime writes to a directory of its own
	// (jobDirFor), so a deletion still under way never reaches its files.
	busyDirs map[string]bool
	// handBackReason: the reason of the hand-back that set the session
	// stopping (handBack): a release that overtakes the hand-back's own
	// (QueueDrained, Tick's retry) records it instead of its own, unless
	// the user asked for the release (userRelease).
	handBackReason string
}

// persist saves s's record (kv colab.session) while s is the backend's
// session: a late write of a session that was released since must neither
// bring its record back nor overwrite the record of a session borrowed
// meanwhile (SEC2 H1). recMu orders it with every other write of the
// record and of the lost runtimes.
func (b *Backend) persist(ctx context.Context, s *liveSession) {
	b.recMu.Lock()
	defer b.recMu.Unlock()
	b.mu.Lock()
	if b.sess != s {
		b.mu.Unlock()
		return
	}
	rec := s.rec
	b.mu.Unlock()
	b.writeRecord(ctx, rec)
}

func (b *Backend) writeRecord(ctx context.Context, rec sessionRecord) {
	raw, _ := json.Marshal(rec)
	if err := b.o.KV.SetKV(context.WithoutCancel(ctx), KVSession, string(raw)); err != nil {
		b.log.Warn("colab: saving the session record", "err", err)
	}
}

// persistNew saves the record of a session just created. A record already
// there is a session nothing released yet (its release failed or waits for
// a credential, or a crash left it behind): it is kept among the lost
// runtimes first, so that the new record does not overwrite the only trace
// of it (SEC2 H1).
func (b *Backend) persistNew(ctx context.Context, s *liveSession) {
	b.recMu.Lock()
	defer b.recMu.Unlock()
	if old, err := b.loadRecord(ctx); err == nil && old != nil && old.NotebookHash != s.rec.NotebookHash {
		b.mu.Lock()
		b.addLostLocked(lostOf(*old), nil)
		b.leftover = nil // the lost runtime stands for it on the card now (leftoverLocked)
		b.mu.Unlock()
		b.saveLostRecLocked(ctx)
		b.log.Warn("colab: a session not released yet is kept among the runtimes to release", "accelerator", old.Accelerator)
	}
	b.mu.Lock()
	if b.sess != s {
		b.mu.Unlock()
		return
	}
	rec := s.rec
	b.mu.Unlock()
	b.writeRecord(ctx, rec)
}

// dropSession forgets s when it is still the backend's session (locked,
// when set, runs under mu right then) and deletes the session record when
// it is the record of notebook hash nbh: a record written since by another
// session, or one kept for a later release, is not this caller's to delete
// (SEC2 H1). s nil: no live session is concerned (cleanupLeftover).
func (b *Backend) dropSession(ctx context.Context, s *liveSession, nbh string, locked func()) {
	b.recMu.Lock()
	defer b.recMu.Unlock()
	b.mu.Lock()
	if s != nil && b.sess == s {
		b.sess = nil
	}
	if locked != nil {
		locked()
	}
	b.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	if rec, err := b.loadRecord(ctx); err != nil || rec == nil || rec.NotebookHash != nbh {
		return
	}
	if err := deleteKV(ctx, b.o.KV, KVSession); err != nil {
		b.log.Warn("colab: deleting the session record", "err", err)
	}
}

func (b *Backend) loadRecord(ctx context.Context) (*sessionRecord, error) {
	v, ok, err := getKV(ctx, b.o.KV, KVSession)
	if err != nil || !ok {
		return nil, err
	}
	var rec sessionRecord
	if json.Unmarshal([]byte(v), &rec) != nil || rec.Version != recordVersion {
		b.log.Warn("colab: unreadable session record dropped")
		return nil, deleteKV(ctx, b.o.KV, KVSession)
	}
	return &rec, nil
}

// newRuntime is the client of an assignment's runtime.
func (b *Backend) newRuntime(p proxyInfo) *runtimeClient {
	return &runtimeClient{hc: b.o.HTTPClient, up: b.o.UploadClient, base: p.URL, token: p.Token, agent: b.o.ClientAgent, dial: b.o.Dial,
		timeout: b.o.RuntimeTimeout,
		slow: func(method, endpoint string, took time.Duration) {
			b.log.Info("colab: slow request to the runtime", "method", method, "endpoint", endpoint, "ms", took.Milliseconds())
		}}
}

// acquire returns the running session, borrowing a runtime first when
// there is none: assign with exponential back-off while Colab refuses for
// capacity (412) or fails transiently (waiting_for remote_queue), up to the
// user's limit; then boot until the Jupyter server answers. A failed boot
// returns the session with the error: the caller releases it (jobErr). A
// session released by someone else meanwhile gives IsBusy.
func (b *Backend) acquire(ctx context.Context, set Settings, jp *jobProgress) (*liveSession, error) {
	b.mu.Lock()
	if s := b.sess; s != nil {
		state, ready := s.rec.State, s.ready
		b.mu.Unlock()
		if state == StateRunning {
			return s, nil
		}
		if ready != nil && (state == StateQueued || state == StateBooting) {
			// colab-batch: another lecture of this run is borrowing the
			// runtime (two at once on the larger GPUs): wait for it.
			jp.waiting("")
			select {
			case <-ready:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			b.mu.Lock()
			ok := b.sess == s && s.rec.State == StateRunning
			b.mu.Unlock()
			if ok {
				return s, nil
			}
		}
		return nil, busyErr(errors.New("the remote session is busy (starting or stopping)"))
	}
	if now := b.now(); now.Before(b.cooldownUntil) {
		left := b.cooldownUntil.Sub(now)
		b.mu.Unlock()
		return nil, newErr(CodeWindowCooldown, map[string]any{"minutes": int(left.Minutes() + 0.5)},
			kindFallback, model.WaitRemoteWindow, errors.New("the fair-use window of the last session is cooling down"))
	}
	s := &liveSession{rec: sessionRecord{
		Version: recordVersion, State: StateQueued, NotebookHash: newNotebookHash(), Accelerator: set.Accelerator,
		Shape: set.shape(), RequestedAt: b.now(), RunID: b.o.RunID,
	}, ready: make(chan struct{})}
	b.sess = s
	b.samples.Reset()
	// A new session: the last one's error is over (v3.2.x: it used to stay
	// on the card until the app quit), and nothing is known about the new
	// runtime yet.
	b.clearErrLocked()
	b.pollFail, b.reach = false, reachState{}
	b.mu.Unlock()
	defer close(s.ready)
	b.persistNew(ctx, s)
	b.log.Info("colab: requesting a runtime", "accelerator", set.Accelerator)

	wait := b.o.Backoff.Initial
	deadline := s.rec.RequestedAt.Add(time.Duration(set.MaxQueueWaitMin) * time.Minute)
	var a assignment
	for {
		var err error
		a, err = b.tun.assign(ctx, s.rec.NotebookHash, s.rec.Accelerator, s.rec.Shape, b.o.AllowHTTP)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			b.dropUnassigned(ctx, s)
			return nil, ctx.Err()
		}
		if !b.stillBorrowing(s) {
			// Released while the request was on its way or while waiting
			// to ask again (SEC2): not this lecture's failure, and no new
			// request for a session nobody keeps. The release may have asked
			// Colab before this answer came: ask once more.
			b.dropUnassigned(ctx, s)
			return nil, busyErr(errors.New("the remote session was released while a runtime was being assigned"))
		}
		st := statusOf(err)
		switch {
		case st == 412 || transient(err):
			if b.now().Add(wait).After(deadline) {
				b.dropUnassigned(ctx, s)
				b.log.Info("colab: no accelerator within the wait limit; back to the local devices",
					"accelerator", set.Accelerator, "minutes", set.MaxQueueWaitMin)
				return nil, fallbackErr(CodeQueueTimeout, map[string]any{"accelerator": set.Accelerator, "minutes": set.MaxQueueWaitMin},
					fmt.Errorf("no %s runtime within %d minutes (last answer: %s)", set.Accelerator, set.MaxQueueWaitMin, err))
			}
			jp.waiting(model.WaitRemoteQueue)
			if err := b.o.Sleep(ctx, wait); err != nil {
				b.dropUnassigned(ctx, s)
				return nil, err
			}
			if !b.stillBorrowing(s) {
				b.dropUnassigned(ctx, s)
				return nil, busyErr(errors.New("the remote session was released while a runtime was being assigned"))
			}
			wait = min(2*wait, b.o.Backoff.Max)
		case st == 400:
			b.dropUnassigned(ctx, s)
			return nil, fallbackErr(CodeAcceleratorUnavailable, map[string]any{"accelerator": set.Accelerator},
				errors.New("Colab refused the accelerator (HTTP 400: no entitlement or quota)"))
		default:
			b.dropUnassigned(ctx, s)
			return nil, codedHTTP("tun/m/assign", err)
		}
	}
	b.mu.Lock()
	if b.sess != s || s.rec.State == StateStopping {
		// Released while the assignment was on its way (a release of the
		// queued session forgot it): nothing would release this runtime
		// any more, so it goes back now (review R3 item 2); when that
		// fails it is kept among the lost runtimes (SEC2 H1).
		l := lostOf(s.rec)
		l.Endpoint = a.Endpoint
		if a.Accelerator != "" {
			l.Accelerator = a.Accelerator
		}
		b.mu.Unlock()
		b.giveBackLost(ctx, l)
		return nil, busyErr(errors.New("the remote session was released while a runtime was being assigned"))
	}
	s.rec.Endpoint, s.rec.AssignedAt, s.rec.State = a.Endpoint, b.now(), StateBooting
	if a.Accelerator != "" && a.Accelerator != s.rec.Accelerator {
		b.log.Warn("colab: assigned another accelerator than requested", "requested", s.rec.Accelerator, "assigned", a.Accelerator)
		s.rec.Accelerator = a.Accelerator
	}
	s.rt = b.newRuntime(a.Proxy)
	s.proxyExp = b.now().Add(a.Proxy.ExpiresIn)
	acc := s.rec.Accelerator
	b.mu.Unlock()
	b.persist(ctx, s)
	jp.waiting("")
	b.log.Info("colab: runtime assigned; booting", "accelerator", acc)

	if err := b.boot(ctx, s); err != nil {
		if !b.stillBorrowing(s) {
			// Released while it booted, and the boot then failed (its
			// runtime is gone): the release gave the runtime back; not this
			// lecture's failure (SEC2 L4).
			if ctx.Err() != nil {
				return nil, ctx.Err() // cancelled all the same: nothing left to release
			}
			return nil, busyErr(errors.New("the remote session was released while it booted"))
		}
		return s, err // the caller releases s (jobErr)
	}
	b.mu.Lock()
	if b.sess != s || s.rec.State == StateStopping {
		// released while it booted: the release gave the runtime back; its
		// record is gone and must not be written again
		b.mu.Unlock()
		return nil, busyErr(errors.New("the remote session was released while it booted"))
	}
	s.rec.State, s.rec.ReadyAt = StateRunning, b.now()
	rec := s.rec
	b.mu.Unlock()
	b.persist(ctx, s)
	b.log.Info("colab: runtime ready", "accelerator", rec.Accelerator, "waited_s", int(rec.ReadyAt.Sub(rec.RequestedAt).Seconds()))
	return s, nil
}

// stillBorrowing: s is still the backend's session and nobody is
// releasing it.
func (b *Backend) stillBorrowing(s *liveSession) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sess == s && s.rec.State != StateStopping
}

// boot waits until the runtime's Jupyter server answers (GET /api/kernels).
// Upstream connects right away (with three retries on timeouts); polling
// covers a proxy that is not ready yet. A session released meanwhile ends
// the wait (SEC2 L4: its runtime is gone; polling it would go on until the
// boot timeout).
func (b *Backend) boot(ctx context.Context, s *liveSession) error {
	started := b.now()
	deadline := started.Add(b.o.BootTimeout)
	delay := time.Second
	refreshed := false
	for {
		if !b.stillBorrowing(s) {
			return busyErr(errors.New("the remote session was released while it booted"))
		}
		rt, err := b.runtime(ctx, s)
		if err == nil {
			_, err = rt.listKernels(ctx)
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st := statusOf(err)
		switch {
		case (st == 401 || st == 403) && !refreshed:
			refreshed = true
			b.mu.Lock()
			s.proxyExp = time.Time{} // fetch a fresh proxy token
			b.mu.Unlock()
			continue
		case st == 401 || st == 403:
			return fallbackErr(CodeSessionReclaimed, nil, fmt.Errorf("the runtime refuses its proxy token (HTTP %d)", st))
		case st == 404 || transient(err):
		default:
			return codedHTTP("api/kernels", err)
		}
		if !b.now().Add(delay).Before(deadline) {
			return fallbackErr(CodeBootTimeout, map[string]any{"minutes": int(b.o.BootTimeout.Minutes())},
				fmt.Errorf("the runtime did not answer within %s (last: %s)", b.o.BootTimeout, err))
		}
		if err := b.o.Sleep(ctx, delay); err != nil {
			return err
		}
		// colab-prof R3: every second for the first minute (a slow boot is
		// noticed at once), then doubling from 5 s to 30 s.
		if b.now().Sub(started) >= time.Minute {
			delay = min(max(2*delay, 5*time.Second), 30*time.Second)
		}
	}
}

// runtime returns the session's runtime client with a proxy token that is
// valid for at least proxyRefreshAhead (upstream State.get_session: refresh
// through list_assignments; an assignment that is gone = reclaimed). When
// the refresh fails transiently (refreshTransient) and the current token
// is still valid for proxyStaleUse, the current client is returned: the
// step goes on, and the next call tries the refresh again (review R65 S1).
// Every other failure of the refresh is returned as before.
func (b *Backend) runtime(ctx context.Context, s *liveSession) (*runtimeClient, error) {
	b.mu.Lock()
	if s.rt != nil && b.now().Add(proxyRefreshAhead).Before(s.proxyExp) {
		rt := s.rt
		b.mu.Unlock()
		return rt, nil
	}
	endpoint := s.rec.Endpoint
	b.mu.Unlock()
	list, err := b.tun.listAssignments(ctx, b.o.AllowHTTP)
	if err != nil {
		if !refreshTransient(err) {
			return nil, err
		}
		b.mu.Lock()
		rt, left, first := s.rt, s.proxyExp.Sub(b.now()), !s.proxyStale
		if rt == nil || left < proxyStaleUse {
			b.mu.Unlock()
			return nil, err
		}
		s.proxyStale = true
		b.mu.Unlock()
		if first {
			b.log.Info("colab: fetching a new runtime token failed; the current one is used while it lasts",
				"code", Code(err), "left_s", int(left/time.Second))
		}
		return rt, nil
	}
	for _, a := range list {
		if a.Endpoint == endpoint {
			b.mu.Lock()
			s.rt = b.newRuntime(a.Proxy)
			s.proxyExp = b.now().Add(a.Proxy.ExpiresIn)
			s.proxyStale = false
			rt := s.rt
			b.mu.Unlock()
			return rt, nil
		}
	}
	return nil, fallbackErr(CodeSessionReclaimed, nil, errors.New("the runtime is no longer among the account's assignments"))
}

// clientOf is runtime for session s as a clientSource (a cell's every
// connection takes a current token: execCell).
func (b *Backend) clientOf(s *liveSession) clientSource {
	return func(ctx context.Context) (*runtimeClient, error) { return b.runtime(ctx, s) }
}

// dropUnassigned forgets a session that never got a runtime. A POST whose
// answer was lost may still have assigned one: one GET with the same
// notebook hash tells (upstream reconciles the same way), and such a
// runtime is released. When that cannot be done now (the GET fails, or the
// release does) the runtime is not forgotten: s stays stopping with the
// endpoint for Tick while it is the backend's session, else it is kept
// among the lost runtimes (SEC2 H1).
func (b *Backend) dropUnassigned(ctx context.Context, s *liveSession) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	b.mu.Lock()
	rec := s.rec
	b.mu.Unlock()
	l := lostOf(rec)
	a, _, ok, err := b.tun.getAssign(ctx, rec.NotebookHash, rec.Accelerator, rec.Shape, b.o.AllowHTTP)
	switch {
	case err != nil:
		// Colab cannot tell now whether a POST whose answer was lost
		// assigned a runtime: asked again later with the notebook hash.
		err = codedHTTP("tun/m/assign", err)
		b.log.Warn("colab: cannot tell whether a runtime was assigned after giving up; asking again later", "code", Code(err))
		b.dropSession(ctx, s, rec.NotebookHash, nil)
		b.addLost(ctx, l, err)
		return
	case ok:
		if err := b.unassignRetry(ctx, a.Endpoint, rec.Accelerator); err != nil {
			b.log.Warn("colab: releasing a runtime assigned after giving up", "code", Code(err))
			b.mu.Lock()
			mine := b.sess == s
			if mine {
				s.rec.Endpoint, s.rec.State = a.Endpoint, StateStopping
			}
			b.mu.Unlock()
			if mine {
				b.persist(ctx, s) // Tick tries again (releaseWith)
				return
			}
			l.Endpoint = a.Endpoint
			b.addLost(ctx, l, err)
			return
		}
	}
	b.dropSession(ctx, s, rec.NotebookHash, nil)
}

// giveBackLost releases the runtime of a session that is already gone
// (acquire: assigned after the session was released). A release that fails
// keeps it among the lost runtimes: Tick tries again, the card shows it
// (Status.Leftover), and the next start releases it (SEC2 H1).
func (b *Backend) giveBackLost(ctx context.Context, l lostRuntime) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	// Straight to the endpoint (just assigned: the account's list may not
	// show it yet); after a failure the list says whether it is still there.
	err := b.unassignRetry(ctx, l.Endpoint, l.Accelerator)
	if err != nil {
		err = b.unassignIfListed(ctx, l.Endpoint)
	}
	if err != nil {
		b.log.Warn("colab: releasing a runtime assigned to a session already released failed; will retry", "code", Code(err))
		b.addLost(ctx, l, err)
		return
	}
	b.log.Info("colab: a runtime assigned to a session already released was given back")
}

// release gives the runtime back (V14.3 step 5): the kernel is shut down
// (best effort, as upstream's stop), then the assignment released. A
// release that fails keeps the record in state stopping; Tick tries again.
func (b *Backend) release(ctx context.Context, reason string) error {
	return b.releaseWith(ctx, reason, releaseOpts{})
}

// releaseOpts qualifies a release.
type releaseOpts struct {
	// file is recorded in ReleaseInfo.File (a remote file name,
	// ReleaseCleanupFailed).
	file string
	// expect, when set, is the session the caller means: the release does
	// nothing when the backend's session is another one by the time it
	// holds relMu (a background release overtaken by another release and a
	// new session must not release the new one: review R3 item 2).
	expect *liveSession
	// ifIdle: only when no lecture is on the backend (checked under mu
	// together with setting the session stopping, so no lecture can be
	// dispatched to it in between).
	ifIdle bool
	// self: the lecture whose end asks for the release (finishJob). It is
	// still among b.jobs until its Transcribe returns and does not count
	// for ifIdle.
	self *jobState
}

// idleLocked: no lecture other than self is on the backend (ifIdle).
func (b *Backend) idleLocked(self *jobState) bool {
	for _, j := range b.jobs {
		if j != self {
			return false
		}
	}
	return true
}

// userRelease tells the reasons the user (or the app's end) asked for:
// they are recorded even over a pending hand-back's reason.
func userRelease(reason string) bool {
	switch reason {
	case ReleaseNow, ReleaseAppExit, ReleaseDisconnect, ReleaseCancelled:
		return true
	}
	return false
}

// releaseWith is release with the options o.
func (b *Backend) releaseWith(ctx context.Context, reason string, o releaseOpts) error {
	b.relMu.Lock()
	defer b.relMu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	// mine tells (under mu) whether this release still applies to s.
	mine := func(s *liveSession) bool {
		return s != nil && (o.expect == nil || s == o.expect) && (!o.ifIdle || b.idleLocked(o.self))
	}
	b.mu.Lock()
	s := b.sess
	ok := mine(s)
	b.mu.Unlock()
	if !ok {
		return nil
	}
	b.awaitRemovals(s) // finished lectures' files go first (removal.go)
	b.mu.Lock()
	s = b.sess
	if !mine(s) {
		b.mu.Unlock()
		return nil
	}
	if s.handBackReason != "" && !userRelease(reason) {
		reason = s.handBackReason // the lecture's failure, not the drain or Tick's retry
	}
	file := o.file
	s.rec.State = StateStopping
	b.samples.Reset() // the runtime's readings go with it (the account stays)
	for _, j := range b.jobs {
		j.reconnect, j.reconnects = nil, nil
	}
	rec, rt := s.rec, s.rt
	kids := append([]string(nil), s.kernels...)
	if len(kids) == 0 && s.kernelID != "" {
		kids = []string{s.kernelID}
	}
	b.dropAllUploadsLocked(s)
	b.mu.Unlock()
	b.persist(ctx, s)
	if rec.Endpoint == "" {
		b.dropUnassigned(ctx, s)
		b.noteRelease(reason, file)
		return nil
	}
	if rt != nil && len(kids) > 0 {
		if rt2, err := b.runtime(ctx, s); err == nil {
			rt = rt2
		}
		for _, kid := range kids {
			if err := rt.deleteKernel(ctx, kid); err != nil {
				b.log.Info("colab: shutting down a kernel before release failed (ignored)", "code", Code(codedHTTP("api/kernels", err)))
			}
		}
	}
	err := b.unassignIfListed(ctx, rec.Endpoint)
	if err != nil && WaitingFor(err) == model.WaitRemoteAuth {
		// No usable credential (its lifecycle ended, or Google rejects it):
		// the runtime cannot be released now. Keep the record for the next
		// import or start, stop retrying, and tell the user.
		b.log.Warn("colab: the runtime cannot be released without a credential; kept for later", "reason", reason, "code", Code(err))
		b.mu.Lock()
		if b.sess == s {
			b.sess = nil
		}
		b.leftover = &LeftoverInfo{Accelerator: rec.Accelerator, Since: rec.RequestedAt, Code: CodeLeftoverSession}
		b.noteErrLocked(infoOf(b.secrets.scrubErr(err)))
		b.mu.Unlock()
		return err
	}
	if err != nil {
		b.log.Warn("colab: releasing the runtime failed; will retry", "reason", reason, "code", Code(err))
		b.mu.Lock()
		b.noteErrLocked(infoOf(b.secrets.scrubErr(err)))
		b.mu.Unlock()
		return err
	}
	b.dropSession(ctx, s, rec.NotebookHash, func() {
		if reason == ReleaseWindow && b.settings.WindowCooldownMin > 0 {
			b.cooldownUntil = b.now().Add(time.Duration(b.settings.WindowCooldownMin) * time.Minute)
			_ = b.o.KV.SetKV(ctx, KVCooldown, b.cooldownUntil.UTC().Format(time.RFC3339))
		}
	})
	b.noteRelease(reason, file)
	b.log.Info("colab: runtime released", "reason", reason, "accelerator", rec.Accelerator, "jobs", rec.Jobs,
		"occupied_min", int(b.now().Sub(rec.RequestedAt).Minutes()))
	return nil
}

func (b *Backend) noteRelease(reason, file string) {
	b.mu.Lock()
	b.lastRelease = &ReleaseInfo{Reason: reason, At: b.now(), File: file}
	b.drained, b.afterCurrent = false, false
	b.mu.Unlock()
}

// unassignIfListed releases endpoint when the account still has it.
func (b *Backend) unassignIfListed(ctx context.Context, endpoint string) error {
	list, err := b.tun.listAssignments(ctx, b.o.AllowHTTP)
	if err != nil {
		return codedHTTP("tun/m/assignments", err)
	}
	for _, a := range list {
		if a.Endpoint == endpoint {
			return b.unassignRetry(ctx, endpoint, a.Accelerator)
		}
	}
	return nil // already gone
}

// unassignRetry releases endpoint, trying a transient failure three times.
func (b *Backend) unassignRetry(ctx context.Context, endpoint, accelerator string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if lastErr = b.tun.unassign(ctx, endpoint); lastErr == nil {
			return nil
		}
		if !transient(lastErr) {
			break
		}
		if err := b.o.Sleep(ctx, time.Duration(2<<attempt)*time.Second); err != nil {
			break
		}
	}
	return fallbackErr(CodeReleaseFailed, map[string]any{"accelerator": accelerator}, codedHTTP("tun/m/unassign", lastErr))
}

// cleanupLeftover releases the session an earlier run left behind (V14.3
// step 5: "应用启动时检查并清理上次残留的会话"). Only the runtime the
// record names is touched, never other Colab sessions of the account.
// Without a usable credential it cannot: the record stays and Status
// reports it (CodeLeftoverSession).
func (b *Backend) cleanupLeftover(ctx context.Context, rec *sessionRecord) error {
	if _, err := b.auth.remaining(ctx); err != nil {
		b.mu.Lock()
		b.leftover = &LeftoverInfo{Accelerator: rec.Accelerator, Since: rec.RequestedAt, Code: CodeLeftoverSession}
		b.mu.Unlock()
		b.log.Warn("colab: a session of an earlier run may still be running, and there is no usable credential to release it",
			"accelerator", rec.Accelerator)
		return nil
	}
	err := b.releaseLost(ctx, lostOf(*rec))
	if err != nil {
		b.mu.Lock()
		b.leftover = &LeftoverInfo{Accelerator: rec.Accelerator, Since: rec.RequestedAt, Code: Code(err)}
		b.mu.Unlock()
		b.log.Warn("colab: releasing the session of an earlier run failed", "code", Code(err))
		return err
	}
	// a session borrowed meanwhile keeps its record (dropSession)
	b.dropSession(ctx, nil, rec.NotebookHash, func() { b.leftover = nil })
	b.noteRelease(ReleaseLeftover, "")
	b.log.Info("colab: the session of an earlier run was released", "accelerator", rec.Accelerator)
	return nil
}

// getAssign is the GET half of assign (upstream _get_assignment): an
// existing assignment of the notebook hash (assigned true), or the XSRF
// token for the POST (the answer was GetAssignmentResponse {acc, nbh,
// token, variant}).
func (t *tunnel) getAssign(ctx context.Context, nbh, accelerator, shape string, allowHTTP bool) (assignment, string, bool, error) {
	const ep = "tun/m/assign"
	o, ok, err := t.do(ctx, "GET", tunPrefix+"/assign", ep, assignQuery(nbh, accelerator, shape), nil, tunnelTimeout)
	if err != nil {
		return assignment{}, "", false, err
	}
	if !ok {
		return assignment{}, "", false, apiChanged(ep, "empty answer")
	}
	if o.has("endpoint") && o.has("runtimeProxyInfo") {
		a := assignment{Variant: -1, Shape: -1}
		if a.Endpoint, err = t.endpointField(o); err != nil {
			return assignment{}, "", false, err
		}
		if a.Proxy, err = t.proxyInfo(o, allowHTTP); err != nil {
			return assignment{}, "", false, err
		}
		return a, "", true, nil
	}
	if _, err := o.str("acc"); err != nil {
		return assignment{}, "", false, err
	}
	if _, err := o.str("nbh"); err != nil {
		return assignment{}, "", false, err
	}
	xsrf, err := o.str("token")
	if err != nil {
		return assignment{}, "", false, err
	}
	if xsrf == "" {
		return assignment{}, "", false, apiChanged(ep, "field token empty")
	}
	// "p": Colab wants a reCAPTCHA before it assigns (colab-vscode's older
	// schema; read leniently, the boolean only). LectureFlow cannot and
	// does not answer one: the borrowing then fails as Colab decides; the
	// log says why (colab-obs-core: false on the free and the paid account,
	// 2026-10-27).
	if raw, ok := o.m["p"]; ok {
		var p bool
		if json.Unmarshal(raw, &p) == nil && p {
			t.log.Warn("colab: Colab asks for a reCAPTCHA before it assigns a runtime; if the borrowing fails, open Colab once in the browser with this Google account")
		}
	}
	if v, err := o.str("variant"); err != nil {
		return assignment{}, "", false, err
	} else if v != "DEFAULT" && v != "GPU" && v != "TPU" {
		return assignment{}, "", false, apiChanged(ep, "unknown variant "+safeToken(v))
	}
	t.secrets.add(xsrf)
	return assignment{}, xsrf, false, nil
}
