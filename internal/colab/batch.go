// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Batches (colab-batch; user decisions of 2026-10-27 04:30 and 04:40, lead
// ruling of 05:40; NOTES "决定（colab-batch：core-builder，2026-10-27）").
//
// The scheduler tells the backend which lectures it plans for Colab
// (SetPlan): those running there and those waiting, each saying whether
// its audio may go up ahead of its turn. While a lecture transcribes, the
// backend uploads the next planned ones (one upload at a time: the
// uplink is the bottleneck), so a later lecture only writes its job.json
// and the resident worker, model already loaded, starts at once. How many
// go ahead:
//   - the scheduler decides which may (Upload): for a user with local
//     devices that Colab only relieves, one lecture besides the running one
//     ("1 + 1"); for a user without a local device, every waiting lecture;
//   - the backend limits that: the free tier holds at most freeTierHold
//     lectures; the paid tier as many as fit the fair-use window, the
//     balance and the credential (the estimates of everything ahead of
//     them included); in memory mode as many as fit the memory the probe
//     found (each costs about three times its WAV: the upload until it is
//     decoded, the decoded samples while it transcribes).
// A lecture that leaves the plan (cancelled, taken by a local device,
// handed back) has its uploaded files deleted at once.
//
// On the larger GPUs two lectures transcribe at once (Slots: one process,
// one model, num_workers 2; the words are those of one at a time,
// colab-prof §9.2); a T4 is already busy with one. "极速模式" (FastMode)
// instead cuts each lecture into parts transcribed at once (splitFor), one
// lecture at a time.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// PlanEntry is one lecture the scheduler plans for Colab.
type PlanEntry struct {
	// Key is the scheduler's job id (Job.ID of the lecture's Transcribe).
	Key       string
	AudioPath string
	AudioSec  float64
	// Running: dispatched to Colab (its Transcribe may not have begun).
	Running bool
	// Upload: its audio may go up ahead of its turn.
	Upload bool
}

// freeTierHold is how many lectures the free tier holds at once (running
// and uploaded ahead; user decision: 2–3).
const freeTierHold = 3

// slotsFor is how many lectures run at once on an accelerator: two on the
// larger GPUs, one on a T4 (fully used by one: ×1.05 with two) and in
// "极速模式" (the parts of one lecture use the workers).
func slotsFor(accelerator string, fast bool) int {
	if fast || accelerator == "T4" || accelerator == "" {
		return 1
	}
	return 2
}

// splitFor is the number of parts of a lecture in "极速模式" (1 = whole):
// 4 on G4 and H100 (−62 % on a G4), 2 on L4 and A100 (−16 %, −26 %); never
// on a T4, which gains nothing (colab-prof §9.3).
func splitFor(accelerator string, fast bool) int {
	if !fast {
		return 0
	}
	switch accelerator {
	case "G4", "H100":
		return 4
	case "L4", "A100":
		return 2
	}
	return 0
}

// workersFor is the resident worker's --workers (WhisperModel num_workers).
func workersFor(accelerator string, fast bool) int {
	if n := splitFor(accelerator, fast); n > 1 {
		return n
	}
	return slotsFor(accelerator, fast)
}

// Slots is how many lectures may run on Colab at once now (the
// session's accelerator, else the setting's).
func (b *Backend) Slots() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.slotsLocked()
}

func (b *Backend) slotsLocked() int {
	acc := b.selectionLocked().effective
	if b.sess != nil {
		acc = b.sess.rec.Accelerator
	}
	return slotsFor(acc, b.settings.FastMode)
}

// SetPlan replaces the planned lectures (see the package comment above).
func (b *Backend) SetPlan(plan []PlanEntry) {
	b.mu.Lock()
	b.plan = append([]PlanEntry(nil), plan...)
	b.mu.Unlock()
	b.kickPlan()
}

// Held returns the keys of the lectures whose audio is on the runtime or
// on its way there ahead of their turn: the scheduler keeps them for
// Colab and sends them first.
func (b *Backend) Held() map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]bool{}
	if s := b.sess; s != nil {
		for k := range s.uploads {
			out[k] = true
		}
	}
	return out
}

// preUpload is a lecture's audio uploaded ahead of its turn.
type preUpload struct {
	key, audio, rid string
	size            int64
	mod             time.Time
	sec             float64
	costMB          int64
	cancel          context.CancelFunc
	done            chan struct{} // closed when the upload ended
	ok              bool
	name, sha       string // the uploaded file and its sha256
	claimed         bool   // a Transcribe waits for it
}

// othersLocked counts what keeps the runtime busy besides lecture j: other
// running lectures, lectures uploaded ahead, planned lectures that may
// still go up.
func (b *Backend) othersLocked(s *liveSession, j *jobState) int {
	n := 0
	for _, x := range b.jobs {
		if x != j {
			n++
		}
	}
	if s != nil {
		n += len(s.uploads)
	}
	for _, e := range b.plan {
		if e.Upload && !e.Running && (s == nil || s.uploads[e.Key] == nil) && (j == nil || e.Key != j.key) {
			n++
		}
	}
	return n
}

// runningOthers counts the running lectures other than j.
func (b *Backend) runningOthers(j *jobState) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, x := range b.jobs {
		if x != j {
			n++
		}
	}
	return n
}

// kickPlan runs the planner (planStep) in the background, again when asked
// while it runs.
func (b *Backend) kickPlan() {
	b.mu.Lock()
	if b.planning {
		b.planAgain = true
		b.mu.Unlock()
		return
	}
	b.planning = true
	b.planIdle = make(chan struct{})
	b.mu.Unlock()
	go func() {
		for {
			b.planStep()
			b.mu.Lock()
			if !b.planAgain {
				b.planning = false
				close(b.planIdle)
				b.mu.Unlock()
				return
			}
			b.planAgain = false
			b.mu.Unlock()
		}
	}()
}

// planSettled waits until the planner has nothing left to start (tests).
func (b *Backend) planSettled() {
	for {
		b.mu.Lock()
		ch, busy := b.planIdle, b.planning
		b.mu.Unlock()
		if !busy || ch == nil {
			return
		}
		<-ch
	}
}

// planStep drops the uploads that left the plan and starts the next
// upload ahead when one may go.
func (b *Backend) planStep() {
	ctx := b.bg
	b.mu.Lock()
	s := b.sess
	if s == nil {
		b.mu.Unlock()
		return
	}
	keep := map[string]bool{}
	for _, e := range b.plan {
		keep[e.Key] = true
	}
	stopping := s.rec.State != StateRunning || b.afterCurrent || b.drained
	var drop []*preUpload
	for k, u := range s.uploads {
		if !u.claimed && (stopping || !keep[k]) {
			drop = append(drop, u)
			delete(s.uploads, k)
		}
	}
	// A lecture that runs and has not sent its audio yet goes first: no
	// upload ahead starts before it (they share the uplink).
	uploading := false
	for _, j := range b.jobs {
		uploading = uploading || !j.sent
	}
	for _, u := range s.uploads {
		select {
		case <-u.done:
		default:
			uploading = true
		}
	}
	prep := s.prep
	ready := prep != nil && isClosed(prep.done) && prep.err == nil
	var cands []PlanEntry
	var aheadSecs []float64
	var usedMB int64
	if !stopping && ready && !uploading {
		running := map[string]bool{}
		for _, j := range b.jobs {
			running[j.key] = true
			aheadSecs = append(aheadSecs, j.durSec)
			usedMB += audioCostMB(j.audio)
		}
		for _, u := range s.uploads {
			aheadSecs = append(aheadSecs, u.sec)
			usedMB += u.costMB
		}
		inflight := len(b.jobs) + len(s.uploads)
		if b.liveSettingsLocked().Tier != TierFree || inflight < freeTierHold {
			for _, e := range b.plan {
				if e.Upload && !e.Running && !running[e.Key] && s.uploads[e.Key] == nil && !s.upFailed[e.Key] {
					cands = append(cands, e)
				}
			}
		}
	}
	b.mu.Unlock()
	for _, u := range drop {
		b.dropUpload(ctx, s, prep, u)
	}
	if len(cands) == 0 {
		return
	}
	e := cands[0]
	cost := audioCostMB(e.AudioPath)
	if prep.memBudgetMB >= 0 && usedMB+cost > prep.memBudgetMB {
		b.log.Info("colab: the next lecture is not uploaded ahead: the runtime's memory holds no more",
			"budget_mb", prep.memBudgetMB, "in_use_mb", usedMB, "need_mb", cost)
		return
	}
	if !b.fitsAhead(ctx, e, aheadSecs) {
		return
	}
	fi, err := os.Stat(e.AudioPath)
	if err != nil {
		return
	}
	uctx, cancel := context.WithCancel(ctx)
	u := &preUpload{key: e.Key, audio: e.AudioPath, rid: aheadID(e.Key), size: fi.Size(), mod: fi.ModTime(),
		sec: e.AudioSec, costMB: cost, cancel: cancel, done: make(chan struct{})}
	b.mu.Lock()
	if b.sess != s || s.uploads[e.Key] != nil || s.rec.State != StateRunning {
		b.mu.Unlock()
		cancel()
		return
	}
	if s.uploads == nil {
		s.uploads = map[string]*preUpload{}
	}
	s.uploads[e.Key] = u
	b.mu.Unlock()
	go b.uploadAhead(uctx, s, prep, u)
}

// aheadID names the job directory of an upload ahead: the lecture's id and
// a random suffix, so that a directory still being deleted (the lecture
// left the plan) is never the one a later upload of it writes to.
func aheadID(key string) string {
	id := remoteJobID(key)
	if len(id) > 56 {
		id = id[:56]
	}
	return id + "-" + randomHex(3)
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// audioCostMB is what a lecture costs the RAM disk in memory mode: the
// upload (at most the WAV) until it is decoded, the decoded samples (twice
// the 16-bit WAV) while it transcribes.
func audioCostMB(wav string) int64 {
	fi, err := os.Stat(wav)
	if err != nil {
		return 0
	}
	return 3 * ((fi.Size() + 1<<20 - 1) >> 20)
}

// fitsAhead: the lecture still fits the fair-use window, the balance and
// the credential after everything ahead of it (running and uploaded
// lectures, sharing the slots).
func (b *Backend) fitsAhead(ctx context.Context, e PlanEntry, aheadSecs []float64) bool {
	authLeft, err := b.auth.remaining(ctx)
	if err != nil {
		return false
	}
	in := b.dispatchInput(e.AudioSec, authLeft, true)
	in.SetupDone = true
	slots := float64(max(1, b.Slots()))
	var ahead float64
	for _, a := range aheadSecs {
		ahead += EstimateSec(a, in.Rate, in.Defaults, true)
	}
	in.UsedSec += ahead / slots
	if in.Paid {
		in.BalanceCU -= ahead / slots / 3600 * max(0, in.CUPerHour)
	}
	if in.AuthRemainingSec >= 0 {
		in.AuthRemainingSec = max(0, in.AuthRemainingSec-ahead/slots)
	}
	d := Decide(in)
	if !d.Dispatch {
		b.log.Info("colab: the next lecture is not uploaded ahead", "waiting_for", d.WaitingFor)
	}
	return d.Dispatch
}

// uploadAhead encodes and uploads one planned lecture's audio into its job
// directory (no job.json: the resident worker leaves it alone until the
// lecture's turn).
func (b *Backend) uploadAhead(ctx context.Context, s *liveSession, prep *sessionPrep, u *preUpload) {
	defer b.kickPlan()
	defer close(u.done)
	fail := func(what string, err error) {
		b.log.Info("colab: uploading a lecture ahead of its turn failed; it uploads when it runs", "step", what,
			"code", Code(codedHTTP("api/contents", err)))
		b.mu.Lock()
		if s.uploads[u.key] == u {
			delete(s.uploads, u.key)
		}
		if s.upFailed == nil {
			s.upFailed = map[string]bool{}
		}
		s.upFailed[u.key] = true
		b.mu.Unlock()
		if !errors.Is(err, context.Canceled) {
			// before done is closed: a lecture waiting for this upload
			// uploads its own audio only after these files are gone
			b.cleanupKept(b.bg, s, prep.base+"/jobs/"+u.rid)
		}
	}
	pa, err := prepareAudio(ctx, b.o.RunFFmpeg, b.o.FFmpeg, u.audio, filepath.Dir(u.audio), b.log.Info)
	if err != nil {
		fail("encode", err)
		return
	}
	if pa.Temp {
		defer os.Remove(pa.Path)
	}
	s.upMu.Lock()
	defer s.upMu.Unlock()
	if err := ctx.Err(); err != nil {
		fail("upload", err)
		return
	}
	jobDir := prep.base + "/jobs/" + u.rid
	rt, err := b.runtime(ctx, s)
	if err == nil {
		err = rt.mkdir(ctx, jobDir)
	}
	if err == nil {
		err = rt.upload(ctx, jobDir+"/"+pa.Name, pa.Data, b.o.ChunkSize, nil)
	}
	if err != nil {
		fail("upload", err)
		return
	}
	b.mu.Lock()
	u.ok, u.name, u.sha = true, pa.Name, pa.SHA256
	b.mu.Unlock()
	b.log.Info("colab: a lecture's audio is on the runtime ahead of its turn", "audio_min", int(u.sec/60))
}

// dropUpload stops an upload ahead and deletes its files on the runtime
// at once (the lecture was cancelled, went to a local device or was
// handed back).
func (b *Backend) dropUpload(ctx context.Context, s *liveSession, prep *sessionPrep, u *preUpload) {
	if u.cancel != nil {
		u.cancel()
	}
	go func() {
		<-u.done
		if prep == nil || prep.base == "" {
			return
		}
		b.mu.Lock()
		live := b.sess == s
		b.mu.Unlock()
		if !live {
			return // released: its disk went with it
		}
		if b.cleanupKept(ctx, s, prep.base+"/jobs/"+u.rid) {
			b.log.Info("colab: the audio uploaded ahead of a lecture that left the plan was deleted on the runtime")
		}
	}()
}

// claimUpload hands the lecture its audio uploaded ahead, if any (waiting
// for an upload under way); nil when there is none or it is stale (the
// lecture's audio changed since), and the lecture uploads as usual.
func (b *Backend) claimUpload(ctx context.Context, s *liveSession, job Job) (*preUpload, error) {
	b.mu.Lock()
	u := s.uploads[job.ID]
	if u == nil {
		for _, x := range s.uploads {
			if !x.claimed && x.audio == job.AudioPath {
				u = x
				break
			}
		}
	}
	if u == nil || u.claimed {
		b.mu.Unlock()
		return nil, nil
	}
	u.claimed = true
	b.mu.Unlock()
	select {
	case <-u.done:
	case <-ctx.Done():
		b.mu.Lock()
		u.claimed = false
		b.mu.Unlock()
		b.kickPlan()
		return nil, ctx.Err()
	}
	b.mu.Lock()
	if s.uploads[u.key] == u {
		delete(s.uploads, u.key) // the lecture owns its directory now
	}
	b.mu.Unlock()
	fi, err := os.Stat(job.AudioPath)
	if !u.ok || err != nil || fi.Size() != u.size || !fi.ModTime().Equal(u.mod) {
		if u.ok {
			// stale: the audio changed after it went up
			b.mu.Lock()
			prep := s.prep
			b.mu.Unlock()
			if prep != nil {
				b.cleanupKept(context.WithoutCancel(ctx), s, prep.base+"/jobs/"+u.rid)
			}
			b.log.Info("colab: the lecture's audio changed after it was uploaded ahead; it uploads again")
		}
		return nil, nil
	}
	return u, nil
}

// cleanupKept deletes the files of a job directory while the runtime stays
// for other lectures (a cancelled lecture, an upload ahead that left the
// plan or went stale), with retries. A file that still cannot be deleted
// (Gate round 1 item 8: nothing of a lecture stays on Colab) releases the
// runtime as soon as no lecture runs on it any more (leftFile: now, or at
// the end of the running ones). false when a file was left.
func (b *Backend) cleanupKept(ctx context.Context, s *liveSession, jobDir string) bool {
	b.markBusyDir(s, jobDir) // T051 (removal.go); the caller marks it first when this runs in a goroutine
	f := b.cleanupRemote(ctx, s, jobDir, true)
	b.settleBusyDir(s, jobDir, f == "" && ctx.Err() == nil)
	if f == "" {
		return true
	}
	b.mu.Lock()
	live := b.sess == s
	if live {
		s.leftFile = f
	}
	idle := len(b.jobs) == 0
	b.mu.Unlock()
	if live && idle {
		_ = b.releaseWith(context.WithoutCancel(ctx), ReleaseCleanupFailed, releaseOpts{file: f, expect: s, ifIdle: true})
	}
	return false
}

// dropAllUploads forgets every upload ahead (the runtime is being
// released: they go with it).
func (b *Backend) dropAllUploadsLocked(s *liveSession) {
	for k, u := range s.uploads {
		if u.cancel != nil {
			u.cancel()
		}
		delete(s.uploads, k)
	}
}

// SlotsFor is how many lectures run at once on Colab with these settings
// (the scheduler's colab:remote row; Backend.Settings is the accelerator in
// effect).
func SlotsFor(set Settings) int { return slotsFor(set.Accelerator, set.FastMode) }
