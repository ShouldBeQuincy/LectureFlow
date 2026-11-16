// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// colab-batch on the stateful fake (fakecolab_test.go): uploads ahead of a
// lecture's turn, the resident worker reused, two lectures at once on an
// L4, the limits (free tier, memory, window), a lecture that leaves the
// plan or is cancelled has its remote files deleted at once while the
// runtime stays for the others, R1's upload failure and early setup error.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func t4() Settings {
	s := DefaultSettings()
	s.Accelerator, s.AcceleratorChosen = "T4", true
	return s
}

func plan(running []Job, ahead ...Job) []PlanEntry {
	var p []PlanEntry
	for _, j := range running {
		p = append(p, PlanEntry{Key: j.ID, AudioPath: j.AudioPath, AudioSec: j.DurationSec, Running: true})
	}
	for _, j := range ahead {
		p = append(p, PlanEntry{Key: j.ID, AudioPath: j.AudioPath, AudioSec: j.DurationSec, Upload: true})
	}
	return p
}

// aheadPath is the remote audio of a lecture uploaded ahead ("" when none).
func (f *fakeColab) aheadPath(base, key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aheadPathLocked(base, key)
}

func (f *fakeColab) aheadPathLocked(base, key string) string {
	for p := range f.files {
		if strings.HasPrefix(p, base+"/jobs/"+key+"-") && strings.HasSuffix(p, "/audio.flac") {
			return p
		}
	}
	return ""
}

func (f *fakeColab) count(fn func() int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn()
}

func startLecture(e *fakeEnv, ctx context.Context, j Job) chan error {
	done := make(chan error, 1)
	go func() {
		_, err := e.b.Transcribe(ctx, j, nil)
		done <- err
	}()
	return done
}

func waitErr(t *testing.T, ch chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the lecture did not end")
		return nil
	}
}

const base = "content/lectureflow"

func TestBatchUploadsAheadAndReusesTheWorker(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.f.hold("a")
	da := startLecture(e, ctx, a)
	if n := e.transcribingNow(t); n != "a" {
		t.Fatalf("transcribing %q", n)
	}
	e.b.SetPlan(plan([]Job{a}, b))
	e.waitFor(t, "b uploaded ahead", func() bool {
		return e.b.Held()["b"] && e.f.aheadPath(base, "b") != "" && e.b.Status(ctx).UploadedAhead == 1
	})
	ahead := e.f.aheadPath(base, "b")
	dir := ahead[:strings.LastIndex(ahead, "/")]
	if e.f.has(dir + "/job.json") {
		t.Fatal("job.json of a lecture uploaded ahead (the worker would start it)")
	}
	// One upload at a time: b's audio went up after a's.
	e.f.mu.Lock()
	sa, sb := e.f.uploadSpans[base+"/jobs/a/audio.flac"], e.f.uploadSpans[ahead]
	e.f.mu.Unlock()
	if sb[0].Before(sa[1]) {
		t.Errorf("uploads overlap: a %v, b %v", sa, sb)
	}
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatalf("a: %v", err)
	}
	if st := e.b.Status(ctx); st.State != StateRunning {
		t.Fatalf("released with b uploaded ahead: %+v", st)
	}
	// b's turn: only job.json goes up; the worker is ready (no second setup).
	path, err := e.b.Transcribe(ctx, b, nil)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "sentences") {
		t.Fatalf("result %s", got)
	}
	if n := e.f.count(func() int { return e.f.uploadCount[ahead] }); n != 1 {
		t.Errorf("b's audio uploaded %d times", n)
	}
	if n := e.f.count(func() int { return e.f.setups }); n != 1 {
		t.Errorf("%d worker setups", n)
	}
	e.b.removalsSettled() // 3.2.0: the directory goes in the background, after the lecture
	if e.f.hasDir(dir) || e.f.has(dir+"/result.json") {
		t.Error("b's directory left after its lecture")
	}
	if !strings.Contains(e.log.String(), "uploaded_ahead=true") {
		t.Errorf("log: %s", e.log.String())
	}
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatal(err)
	}
	if !e.f.released || e.f.count(func() int { return len(e.f.kernels) }) != 0 {
		t.Errorf("released %v, kernels %v", e.f.released, e.f.kernels)
	}
	e.checkSecrets(t)
}

func TestBatchTwoLecturesAtOnceOnAnL4(t *testing.T) {
	e := newFakeEnv(t, DefaultSettings()) // paid: the L4 (user decision)
	ctx := context.Background()
	e.b.RefreshAccount(ctx) // colab-obs-core: the level follows the account
	if e.b.Settings().Accelerator != "L4" || e.b.Slots() != 2 {
		t.Fatalf("default %s, slots %d", e.b.Settings().Accelerator, e.b.Slots())
	}
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.f.hold("a")
	e.f.hold("b")
	da := startLecture(e, ctx, a)
	e.transcribingNow(t)
	if ok, w := e.b.CanDispatch(ctx, 600); !ok {
		t.Fatalf("CanDispatch with one lecture on an L4: %q", w)
	}
	db := startLecture(e, ctx, b)
	e.transcribingNow(t)
	if n := e.f.count(func() int { return e.f.maxRunning }); n != 2 {
		t.Errorf("at most %d at once", n)
	}
	if st := e.b.Status(ctx); len(st.Jobs) != 2 || st.Current == nil {
		t.Errorf("status %+v", st)
	}
	if ok, _ := e.b.CanDispatch(ctx, 600); ok {
		t.Error("a third lecture on an L4")
	}
	e.f.mu.Lock()
	cells := append([]string(nil), e.f.cells...)
	kernels := e.f.nKernels
	e.f.mu.Unlock()
	if kernels != 2 {
		t.Errorf("%d kernels", kernels)
	}
	for _, c := range cells {
		if strings.Contains(c, "--attach") && !strings.Contains(c, `"--workers","2"`) {
			t.Errorf("cell without two workers: %s", c)
		}
	}
	e.f.release("a")
	e.f.release("b")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, db); err != nil {
		t.Fatal(err)
	}
	if n := e.f.count(func() int { return e.f.setups }); n != 1 {
		t.Errorf("%d worker setups", n)
	}
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestBatchLectureLeavingThePlanIsDeletedAtOnce(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.f.hold("a")
	da := startLecture(e, ctx, a)
	e.transcribingNow(t)
	e.b.SetPlan(plan([]Job{a}, b))
	e.waitFor(t, "b uploaded ahead", func() bool { return e.b.Status(ctx).UploadedAhead == 1 })
	ahead := e.f.aheadPath(base, "b")
	dir := ahead[:strings.LastIndex(ahead, "/")]
	// b was cancelled (or a local device took it): its files go now.
	e.b.SetPlan(plan([]Job{a}))
	e.waitFor(t, "b's files deleted", func() bool { return !e.f.has(ahead) && !e.f.hasDir(dir) && len(e.b.Held()) == 0 })
	if st := e.b.Status(ctx); st.State != StateRunning {
		t.Errorf("status %+v", st)
	}
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	_ = e.b.QueueDrained(ctx)
}

func TestBatchCancelledLectureGoesTheRuntimeStays(t *testing.T) {
	e := newFakeEnv(t, DefaultSettings())
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.f.hold("a")
	e.f.hold("b")
	actx, cancel := context.WithCancel(context.Background())
	da := startLecture(e, actx, a)
	e.transcribingNow(t)
	db := startLecture(e, context.Background(), b)
	e.transcribingNow(t)
	start := time.Now()
	cancel()
	err := waitErr(t, da)
	if !errors.Is(err, context.Canceled) || Code(err) != CodeCancelled {
		t.Fatalf("a: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("cancel took %s", took)
	}
	e.waitFor(t, "a's files deleted", func() bool {
		return !e.f.has(base+"/jobs/a/job.json") && !e.f.hasDir(base+"/jobs/a")
	})
	if st := e.b.Status(context.Background()); st.State != StateRunning || e.f.released {
		t.Fatalf("the runtime went with the cancel: %+v", st)
	}
	e.f.release("b")
	if err := waitErr(t, db); err != nil {
		t.Fatalf("b: %v", err)
	}
	if err := e.b.QueueDrained(context.Background()); err != nil || !e.f.released {
		t.Fatalf("drain: %v released %v", err, e.f.released)
	}
}

func TestBatchFreeTierHoldsThree(t *testing.T) {
	set := t4()
	set.Tier = TierPaid // colab-obs-core: ignored, the account says free
	e := newFakeEnv(t, set)
	e.f.balance, e.f.freeTokens = 0, "1500"
	ctx := context.Background()
	var js []Job
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		js = append(js, e.lecture(t, n))
	}
	e.f.hold("a")
	da := startLecture(e, ctx, js[0])
	e.transcribingNow(t)
	e.b.SetPlan(plan(js[:1], js[1:]...))
	e.waitFor(t, "two uploaded ahead", func() bool { return e.b.Status(ctx).UploadedAhead == 2 })
	time.Sleep(50 * time.Millisecond)
	e.b.planSettled()
	if h := e.b.Held(); len(h) != 2 || !h["b"] || !h["c"] {
		t.Errorf("held %v (the free tier holds three lectures)", h)
	}
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	_ = e.b.QueueDrained(ctx)
}

func TestBatchMemoryLimitsUploadsAhead(t *testing.T) {
	e := newFakeEnv(t, t4())
	e.kv.mu.Lock()
	delete(e.kv.m, KVMemoryMode) // the default: memory mode
	e.kv.mu.Unlock()
	// 4 MiB beyond the first lecture's estimate: with its own 2 MiB share,
	// 6 MiB for recordings; each tiny lecture costs 3 MiB (rounded up).
	e.f.memory = map[string]any{"mode": "memory", "base": "/dev/shm/lectureflow", "reason": "ok", "mem_total_mb": 12985,
		"mem_available_mb": 11000, "need_mb": 10996, "free_mb": 9000}
	ctx := context.Background()
	a, b, c := e.lecture(t, "a"), e.lecture(t, "b"), e.lecture(t, "c")
	e.f.hold("a")
	da := startLecture(e, ctx, a)
	e.transcribingNow(t)
	e.b.SetPlan(plan([]Job{a}, b, c))
	e.waitFor(t, "b uploaded ahead", func() bool { return e.b.Status(ctx).UploadedAhead == 1 })
	time.Sleep(50 * time.Millisecond)
	e.b.planSettled()
	if h := e.b.Held(); len(h) != 1 || !h["b"] {
		t.Errorf("held %v", h)
	}
	if p := e.f.aheadPath("dev/shm/lectureflow", "b"); p == "" {
		t.Error("b's audio is not on the RAM disk")
	}
	if !strings.Contains(e.log.String(), "the runtime's memory holds no more") {
		t.Errorf("log: %s", e.log.String())
	}
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	_ = e.b.QueueDrained(ctx)
}

func TestBatchWindowLimitsUploadsAhead(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.f.hold("a")
	da := startLecture(e, ctx, a)
	e.transcribingNow(t)
	// 200 s of the window left: a (about 132 s on a T4) and b do not both fit.
	e.clk.Advance(FairWindow - 200*time.Second)
	e.b.SetPlan(plan([]Job{a}, b))
	time.Sleep(50 * time.Millisecond)
	e.b.planSettled()
	if h := e.b.Held(); len(h) != 0 {
		t.Errorf("held %v past the window", h)
	}
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	_ = e.b.release(ctx, ReleaseQueueEmpty)
}

func TestBatchRuntimeStaysAfterALectureWhileOthersAreAhead(t *testing.T) {
	set := t4()
	set.IdleReleaseMin = 0 // "right after each lecture"
	e := newFakeEnv(t, set)
	ctx := context.Background()
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.f.hold("a")
	da := startLecture(e, ctx, a)
	e.transcribingNow(t)
	e.b.SetPlan(plan([]Job{a}, b))
	e.waitFor(t, "b uploaded ahead", func() bool { return e.b.Status(ctx).UploadedAhead == 1 })
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	if st := e.b.Status(ctx); st.State != StateRunning {
		t.Fatalf("released with b ahead: %+v", st)
	}
	e.b.SetPlan(plan([]Job{b}))
	if _, err := e.b.Transcribe(ctx, b, nil); err != nil {
		t.Fatal(err)
	}
	if st := e.b.Status(ctx); st.State != StateReleased || st.LastRelease.Reason != ReleaseIdle {
		t.Errorf("after the last lecture: %+v", st)
	}
}

func TestBatchStaleUploadAheadIsReplaced(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.f.hold("a")
	da := startLecture(e, ctx, a)
	e.transcribingNow(t)
	e.b.SetPlan(plan([]Job{a}, b))
	e.waitFor(t, "b uploaded ahead", func() bool { return e.b.Status(ctx).UploadedAhead == 1 })
	ahead := e.f.aheadPath(base, "b")
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	// prep ran again: another audio.wav
	if err := os.WriteFile(b.AudioPath, []byte("RIFF....WAVEfmt b, again and longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(b.AudioPath, later, later)
	if _, err := e.b.Transcribe(ctx, b, nil); err != nil {
		t.Fatal(err)
	}
	if e.f.has(ahead) || e.f.count(func() int { return e.f.uploadCount[base+"/jobs/b/audio.flac"] }) != 1 {
		t.Errorf("stale upload kept or the audio not uploaded again (%v)", e.f.uploadCount)
	}
	_ = e.b.QueueDrained(ctx)
}

// R1: the cell runs while the audio uploads; a failed upload deletes the
// job directory, which ends the attaching cell, and the runtime goes.
func TestUploadFailureEndsTheCell(t *testing.T) {
	e := newFakeEnv(t, t4())
	e.f.failUpload = "audio.flac"
	_, err := e.b.Transcribe(context.Background(), e.lecture(t, "a"), nil)
	if Code(err) != CodeUploadFailed || !IsFallback(err) {
		t.Fatalf("err %v", err)
	}
	e.b.handbacksSettled() // v3.2.x: the files and the runtime go in the background
	if e.f.hasDir(base+"/jobs/a") || !e.f.wasReleased() {
		t.Errorf("job directory left (%v) or runtime kept (released %v)", e.f.hasDir(base+"/jobs/a"), e.f.wasReleased())
	}
	if n := e.f.count(func() int { return e.f.running }); n != 0 {
		t.Errorf("%d cells still transcribing", n)
	}
}

// The worker's setup failed before the audio was ready: nothing uploads.
func TestSetupErrorBeforeTheUploadSkipsIt(t *testing.T) {
	slow := func(ctx context.Context, ffmpeg string, args []string) error {
		time.Sleep(300 * time.Millisecond)
		return fakeFFmpeg(300)(ctx, ffmpeg, args)
	}
	e := newFakeEnv(t, t4(), func(o *Options) { o.RunFFmpeg = slow })
	e.f.setupErr = "no_gpu"
	_, err := e.b.Transcribe(context.Background(), e.lecture(t, "a"), nil)
	if r, _ := codedParam(err, "reason"); Code(err) != CodeRemoteFailed || r != "no_gpu" {
		t.Fatalf("err %v", err)
	}
	if n := e.f.count(func() int { return e.f.uploadCount[base+"/jobs/a/audio.flac"] }); n != 0 {
		t.Errorf("audio uploaded %d times after the setup failed", n)
	}
	e.b.handbacksSettled() // v3.2.x: the runtime goes in the background
	if !e.f.wasReleased() {
		t.Error("runtime kept after the worker failed")
	}
}

func (e *fakeEnv) checkSecrets(t *testing.T) {
	t.Helper()
	logs := e.log.String()
	for _, w := range []string{"ya29.fake", "fake-proxy-token", "xsrf-fake"} {
		if strings.Contains(logs, w) {
			t.Errorf("log contains %q", w)
		}
	}
}

// Two lectures dispatched at once to an L4 that is still being borrowed:
// the second waits for the borrowing instead of being sent back (busy).
func TestBatchSecondLectureWaitsForTheBorrowing(t *testing.T) {
	e := newFakeEnv(t, DefaultSettings())
	gate := make(chan struct{})
	e.f.bootGate, e.f.booting = gate, make(chan struct{})
	ctx := context.Background()
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	da := startLecture(e, ctx, a)
	select {
	case <-e.f.booting:
	case <-time.After(10 * time.Second):
		t.Fatal("never booted")
	}
	db := startLecture(e, ctx, b)
	time.Sleep(50 * time.Millisecond)
	close(gate)
	if err := waitErr(t, da); err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := waitErr(t, db); err != nil {
		t.Fatalf("b: %v (busy: %v)", err, IsBusy(err))
	}
	if n := e.f.count(func() int { return e.f.setups }); n != 1 {
		t.Errorf("%d worker setups", n)
	}
	_ = e.b.QueueDrained(ctx)
}

// A lecture that runs sends its audio before any lecture goes up ahead
// (they share the uplink; seen on the real run: an upload ahead had taken
// the lane before the running lectures' own uploads).
func TestBatchRunningLectureUploadsFirst(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.b.SetPlan(plan([]Job{a}, b)) // planned before a even starts
	e.f.hold("a")
	da := startLecture(e, ctx, a)
	e.transcribingNow(t)
	e.waitFor(t, "b uploaded ahead", func() bool { return e.b.Status(ctx).UploadedAhead == 1 })
	e.f.mu.Lock()
	sa, sb := e.f.uploadSpans[base+"/jobs/a/audio.flac"], e.f.uploadSpans[e.f.aheadPathLocked(base, "b")]
	e.f.mu.Unlock()
	if sa[1].IsZero() || sb[0].Before(sa[1]) {
		t.Errorf("b went up before a was sent: a %v, b %v", sa, sb)
	}
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	_ = e.b.QueueDrained(ctx)
}

// A cancelled lecture's file that cannot be deleted (Gate round 1 item 8):
// the runtime stays for the lecture still running, then goes with the file.
func TestBatchLeftoverFileReleasesAfterTheOthers(t *testing.T) {
	e := newFakeEnv(t, DefaultSettings())
	a, b := e.lecture(t, "a"), e.lecture(t, "b")
	e.f.hold("a")
	e.f.hold("b")
	actx, cancel := context.WithCancel(context.Background())
	da := startLecture(e, actx, a)
	e.transcribingNow(t)
	db := startLecture(e, context.Background(), b)
	e.transcribingNow(t)
	e.f.mu.Lock()
	e.f.failDelete = "jobs/a/job.json"
	e.f.mu.Unlock()
	cancel()
	if err := waitErr(t, da); Code(err) != CodeCancelled {
		t.Fatalf("a: %v", err)
	}
	if st := e.b.Status(context.Background()); st.State != StateRunning {
		t.Fatalf("released while b runs: %+v", st)
	}
	e.f.release("b")
	if err := waitErr(t, db); err != nil {
		t.Fatalf("b: %v", err)
	}
	st := e.b.Status(context.Background())
	if st.State != StateReleased || st.LastRelease.Reason != ReleaseCleanupFailed || st.LastRelease.File != "job.json" {
		t.Fatalf("status %+v (last release %+v)", st, st.LastRelease)
	}
}
