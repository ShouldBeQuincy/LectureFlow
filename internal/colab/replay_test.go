// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Replay tests (V14.7): every exchange with Colab comes from a sample in
// testdata/replay (constructed from upstream's source; see each sample's
// _source), played by replayServer in order.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
)

type testEnv struct {
	b   *Backend
	kv  *MemKV
	clk *fakeClock
	log *logBuf
	rs  *replayServer
	dir string
}

func newEnv(t *testing.T, sample string, mods ...func(*Options)) *testEnv {
	t.Helper()
	rs := newReplay(t, sample)
	kv := newDiskKV()
	clk := newFakeClock()
	lg, lb := newTestLogger()
	o := Options{KV: kv, ColabURL: rs.colabURL(), TokenURL: rs.tokenURL(), RevokeURL: rs.revokeURL(),
		WorkerDir: workerDir(t), FFmpeg: "ffmpeg", RunFFmpeg: fakeFFmpeg(300), Logger: lg, Now: clk.Now, Sleep: clk.Sleep,
		RunID: "run-1", AllowHTTP: true, ChunkSize: 200, ADCPath: filepath.Join(t.TempDir(), "absent.json"),
		Retry: RetryOptions{Jitter: -1}} // exact waits
	for _, m := range mods {
		m(&o)
	}
	b, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{b: b, kv: kv, clk: clk, log: lb, rs: rs, dir: t.TempDir()}
}

func (e *testEnv) withCredential(t *testing.T) *testEnv {
	storeCredential(t, e.kv, Lifecycle{Kind: LifecycleManual}, e.clk.Now(), "run-1")
	return e
}

func (e *testEnv) startup(t *testing.T) *testEnv {
	t.Helper()
	if err := e.b.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	return e
}

func (e *testEnv) job(t *testing.T) Job {
	t.Helper()
	wav := filepath.Join(e.dir, "audio.wav")
	if err := os.WriteFile(wav, []byte("RIFF....WAVEfmt fake wav"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Job{ID: "lecture-1", AudioPath: wav, DurationSec: 600, Language: "en", InitialPrompt: "Vigenère, cipher",
		OutDir: e.dir}
}

// checkClean: no secret and no transcript text in the logs, kv (except
// the credential itself) or the given errors.
func (e *testEnv) checkClean(t *testing.T, errs ...error) {
	t.Helper()
	logs := e.log.String()
	assertNoSecrets(t, "log", logs)
	for _, w := range []string{"Welcome", "Zebrafish", "Vigenère"} {
		if strings.Contains(logs, w) {
			t.Errorf("log contains audio-derived or prompt text %q", w)
		}
	}
	for k, v := range e.kv.Snapshot() {
		if k != KVCredential {
			assertNoSecrets(t, "kv "+k, v)
		}
	}
	for _, err := range errs {
		assertNoSecrets(t, "error", errTexts(err)...)
	}
}

type eventLog struct {
	mu sync.Mutex
	ev []Event
}

func (l *eventLog) add(e Event) {
	l.mu.Lock()
	l.ev = append(l.ev, e)
	l.mu.Unlock()
}

func (l *eventLog) all() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ev)
}

func TestReplayBorrowTranscribeRelease(t *testing.T) {
	e := newEnv(t, "borrow_run_release.json").withCredential(t).startup(t)
	ctx := context.Background()
	var evs eventLog
	path, err := e.b.Transcribe(ctx, e.job(t), evs.add)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if path != filepath.Join(e.dir, "result.json") {
		t.Fatalf("path %s", path)
	}
	got, _ := os.ReadFile(path)
	if !sameResult(got, e.rs.vars["result_json"]) {
		t.Fatalf("result.json differs from the download")
	}
	// No temporary FLAC is left.
	if m, _ := filepath.Glob(filepath.Join(e.dir, "colab-upload-*")); len(m) > 0 {
		t.Errorf("temporary upload left: %v", m)
	}
	// Steps in the order of the hover card, overall progress never goes back.
	var steps []string
	last := -1.0
	sawStage, sawQuarter, sawThreeQ := false, false, false
	for _, ev := range evs.all() {
		if len(steps) == 0 || steps[len(steps)-1] != ev.Step {
			steps = append(steps, ev.Step)
		}
		if ev.Overall+1e-9 < last {
			t.Errorf("overall progress went back: %v after %v", ev.Overall, last)
		}
		last = max(last, ev.Overall)
		if ev.Stage == "transcribe" {
			sawStage = true
		}
		if ev.Step == StepRemoteTranscribe && ev.Fraction == 0.25 {
			sawQuarter = true
		}
		if ev.Step == StepRemoteTranscribe && ev.Fraction == 0.75 {
			sawThreeQ = true // the line split across two stream messages
		}
	}
	want := []string{StepWaitAccelerator, StepUploadAudio, StepRemoteTranscribe, StepDownloadResult, StepVerify}
	if !slices.Equal(steps, want) {
		t.Errorf("steps %v, want %v", steps, want)
	}
	if !sawStage || !sawQuarter || !sawThreeQ || last != 1 {
		t.Errorf("progress: stage %v, 0.25 %v, 0.75 %v, last %v", sawStage, sawQuarter, sawThreeQ, last)
	}
	st := e.b.Status(ctx)
	if st.State != StateRunning || st.Current != nil || st.BalanceCU == nil || *st.BalanceCU != 95.5 {
		t.Errorf("status after the lecture: %+v", st)
	}
	// The measured rate is stored: 96.25 s for 10 audio minutes.
	var rates map[string]Rate
	v, _, _ := e.kv.GetKV(ctx, KVRates)
	_ = json.Unmarshal([]byte(v), &rates)
	if r := rates["T4|large-v3|float16"]; r.Samples != 1 || r.SecPerAudioMin < 9.6 || r.SecPerAudioMin > 9.7 {
		t.Errorf("rate %+v", rates)
	}
	// The queue is empty: release now.
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatalf("QueueDrained: %v", err)
	}
	st = e.b.Status(ctx)
	if st.State != StateReleased || st.LastRelease == nil || st.LastRelease.Reason != ReleaseQueueEmpty {
		t.Errorf("status after release: %+v", st)
	}
	if v, _, _ := e.kv.GetKV(ctx, KVSession); v != "" {
		t.Errorf("session record left: %s", v)
	}
	e.rs.finish()
	e.checkClean(t)
}

func TestReplayShutdownAfterCurrent(t *testing.T) {
	e := newEnv(t, "borrow_run_release.json").withCredential(t).startup(t)
	ctx := context.Background()
	asked := false
	_, err := e.b.Transcribe(ctx, e.job(t), func(ev Event) {
		if ev.Step == StepRemoteTranscribe && !asked {
			asked = true
			if err := e.b.ShutdownAfterCurrent(ctx); err != nil {
				t.Errorf("ShutdownAfterCurrent: %v", err)
			}
			if !e.b.Status(ctx).ShutdownAfter {
				t.Error("status does not show 处理完当前这节后关机")
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	st := e.b.Status(ctx)
	if st.State != StateReleased || st.LastRelease.Reason != ReleaseAfterCurrent || st.ShutdownAfter {
		t.Errorf("status %+v", st)
	}
	e.rs.finish()
	e.checkClean(t)
}

func TestReplayIdleRelease(t *testing.T) {
	e := newEnv(t, "borrow_run_release.json").withCredential(t).startup(t)
	ctx := context.Background()
	if _, err := e.b.Transcribe(ctx, e.job(t), nil); err != nil {
		t.Fatal(err)
	}
	e.b.removalsSettled() // the job directory goes in the background, before the clock moves
	e.clk.Advance(4 * time.Minute)
	e.b.Tick(ctx)
	if e.b.Status(ctx).State != StateRunning {
		t.Fatal("released before the idle limit")
	}
	e.clk.Advance(time.Minute)
	e.b.Tick(ctx)
	if st := e.b.Status(ctx); st.State != StateReleased || st.LastRelease.Reason != ReleaseIdle {
		t.Errorf("status %+v", st)
	}
	e.rs.finish()
	e.checkClean(t)
}

func TestReplayWindowRelease(t *testing.T) {
	e := newEnv(t, "window_release.json").withCredential(t).startup(t)
	ctx := context.Background()
	set := e.b.Settings()
	set.IdleReleaseMin = 60
	if err := e.b.SetSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.Transcribe(ctx, e.job(t), nil); err != nil {
		t.Fatal(err)
	}
	// The job directory goes in the background (3.2.0), before the clock
	// moves: 50 minutes later the access token would have to be refreshed
	// first, which the recording does not expect (flake-hunt #1).
	e.b.removalsSettled()
	// Lectures keep coming (the idle limit never triggers); only the window
	// can release the runtime: at 4 h minus the overhead of one lecture.
	for i := 0; i < 5; i++ {
		e.clk.Advance(50 * time.Minute)
		e.b.mu.Lock()
		live := e.b.sess != nil
		if live {
			e.b.sess.lastJobEnd = e.clk.Now().Add(-time.Minute)
		}
		e.b.mu.Unlock()
		if !live {
			t.Fatalf("released after %d min", (i+1)*50)
		}
		e.b.Tick(ctx)
		if i < 4 && e.b.Status(ctx).State != StateRunning {
			t.Fatalf("released early, after %d min", (i+1)*50)
		}
	}
	st := e.b.Status(ctx)
	if st.State != StateReleased || st.LastRelease.Reason != ReleaseWindow || st.CooldownUntil == nil {
		t.Fatalf("status %+v", st)
	}
	// Cooling down: no new session for an hour (remote_window).
	if ok, w := e.b.CanDispatch(ctx, 600); ok || w != model.WaitRemoteWindow {
		t.Errorf("CanDispatch during the cooldown = %v %q", ok, w)
	}
	e.rs.finish()
	e.checkClean(t)
}

func TestReplayAuthEndingRelease(t *testing.T) {
	e := newEnv(t, "borrow_run_release.json").startup(t)
	ctx := context.Background()
	// A 1-day credential imported 23 hours ago: one hour left.
	storeCredential(t, e.kv, Lifecycle{Kind: LifecycleDays, Days: 1}, e.clk.Now().Add(-23*time.Hour), "run-1")
	if _, err := e.b.Transcribe(ctx, e.job(t), nil); err != nil {
		t.Fatal(err)
	}
	e.b.removalsSettled() // the job directory goes in the background, before the clock moves
	set := e.b.Settings()
	set.IdleReleaseMin = 60
	_ = e.b.SetSettings(ctx, set)
	e.clk.Advance(51 * time.Minute) // 9 minutes left: less than the 10-minute release margin
	e.b.Tick(ctx)
	if st := e.b.Status(ctx); st.State != StateReleased || st.LastRelease.Reason != ReleaseAuthEnding {
		t.Errorf("status %+v", st)
	}
	e.rs.finish()
	e.checkClean(t)
}

func TestReplayHashMismatch(t *testing.T) {
	e := newEnv(t, "hash_mismatch.json").withCredential(t).startup(t)
	ctx := context.Background()
	_, err := e.b.Transcribe(ctx, e.job(t), nil)
	if Code(err) != CodeHashMismatch || !IsFallback(err) {
		t.Fatalf("err %v", err)
	}
	if _, serr := os.Stat(filepath.Join(e.dir, "result.json")); serr == nil {
		t.Error("a result that failed verification was written")
	}
	e.b.handbacksSettled() // v3.2.x: the files and the runtime go in the background
	if st := e.b.Status(ctx); st.State != StateReleased || st.LastRelease.Reason != ReleaseFailed || st.LastError.Code != CodeHashMismatch {
		t.Errorf("status %+v", st)
	}
	e.rs.finish()
	e.checkClean(t, err)
}

func TestReplayContractViolation(t *testing.T) {
	e := newEnv(t, "contract_violation.json").withCredential(t).startup(t)
	ctx := context.Background()
	_, err := e.b.Transcribe(ctx, e.job(t), nil)
	if Code(err) != CodeContractViolation || !IsFallback(err) {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "Welcome") {
		t.Error("the error quotes the offending line")
	}
	e.b.handbacksSettled() // v3.2.x: the files and the runtime go in the background
	if st := e.b.Status(ctx); st.State != StateReleased {
		t.Errorf("status %+v", st)
	}
	e.rs.finish()
	e.checkClean(t, err)
}

func TestReplayRemoteError(t *testing.T) {
	e := newEnv(t, "remote_error.json").withCredential(t).startup(t)
	_, err := e.b.Transcribe(context.Background(), e.job(t), nil)
	if Code(err) != CodeRemoteFailed || !IsFallback(err) {
		t.Fatalf("err %v", err)
	}
	if r, _ := codedParam(err, "reason"); r != "no_gpu" {
		t.Errorf("reason %q", r)
	}
	e.b.handbacksSettled() // v3.2.x: the files and the runtime go in the background
	e.rs.finish()
	e.checkClean(t, err)
}

func TestReplaySessionReclaimed(t *testing.T) {
	e := newEnv(t, "session_reclaimed.json").withCredential(t).startup(t)
	ctx := context.Background()
	_, err := e.b.Transcribe(ctx, e.job(t), nil)
	if Code(err) != CodeSessionReclaimed || !IsFallback(err) {
		t.Fatalf("err %v", err)
	}
	e.b.handbacksSettled() // v3.2.x: the runtime goes in the background
	if st := e.b.Status(ctx); st.LastRelease == nil || st.LastRelease.Reason != ReleaseReclaimed {
		t.Errorf("status %+v", st)
	}
	e.rs.finish()
	e.checkClean(t, err)
}

func TestReplayQueueThenAssigned(t *testing.T) {
	e := newEnv(t, "queue_then_assigned.json").withCredential(t).startup(t)
	ctx := context.Background()
	var evs eventLog
	jp := &jobProgress{b: e.b, out: evs.add, est: -1, j: &jobState{started: e.clk.Now()}}
	s, err := e.b.acquire(ctx, e.b.Settings(), jp)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if s.rec.State != StateRunning || s.rec.Endpoint != "gpu-t4-s-replay1" {
		t.Errorf("session %+v", s.rec)
	}
	// Exponential back-off: 5 s, then 10 s (colab-prof R3: the first retry
	// soon, still doubling); boot answered at once.
	if !slices.Equal(e.clk.sleeps, []time.Duration{5 * time.Second, 10 * time.Second}) {
		t.Errorf("sleeps %v", e.clk.sleeps)
	}
	waits := 0
	for _, ev := range evs.all() {
		if ev.WaitingFor == model.WaitRemoteQueue {
			waits++
			if ev.Step != StepWaitAccelerator || ev.SessionState != StateQueued {
				t.Errorf("waiting event %+v", ev)
			}
		}
	}
	if waits != 2 {
		t.Errorf("%d waiting events", waits)
	}
	// The window counts the queue wait (V14.3 step 4).
	if st := e.b.Status(ctx); st.WindowUsedSec != 15 || st.State != StateRunning {
		t.Errorf("status %+v", st)
	}
	var rec sessionRecord
	v, _, _ := e.kv.GetKV(ctx, KVSession)
	if json.Unmarshal([]byte(v), &rec) != nil || rec.Endpoint != "gpu-t4-s-replay1" || rec.State != StateRunning {
		t.Errorf("persisted record %s", v)
	}
	if err := e.b.release(ctx, ReleaseQueueEmpty); err != nil {
		t.Fatal(err)
	}
	e.rs.finish()
	e.checkClean(t)
}

func TestReplayQueueTimeout(t *testing.T) {
	e := newEnv(t, "queue_timeout.json").withCredential(t).startup(t)
	ctx := context.Background()
	set := e.b.Settings()
	set.MaxQueueWaitMin = 1
	if err := e.b.SetSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	var evs eventLog
	_, err := e.b.Transcribe(ctx, e.job(t), evs.add)
	if Code(err) != CodeQueueTimeout || !IsFallback(err) {
		t.Fatalf("err %v", err)
	}
	queued := false
	for _, ev := range evs.all() {
		queued = queued || ev.WaitingFor == model.WaitRemoteQueue
	}
	if !queued {
		t.Error("no remote_queue event while waiting")
	}
	if st := e.b.Status(ctx); st.State != StateNone {
		t.Errorf("status %+v", st)
	}
	if v, _, _ := e.kv.GetKV(ctx, KVSession); v != "" {
		t.Errorf("record left: %s", v)
	}
	e.rs.finish()
	e.checkClean(t, err)
}

func TestReplayAPIChanged(t *testing.T) {
	for _, tc := range []struct{ sample, endpoint string }{
		{"api_changed_missing_field.json", "tun/m/assign"},
		{"api_changed_unknown_accelerator.json", "tun/m/assign"},
		{"api_changed_unknown_variant.json", "tun/m/assign"},
		{"api_changed_http_404.json", "tun/m/ccu-info"},
	} {
		t.Run(tc.sample, func(t *testing.T) {
			e := newEnv(t, tc.sample).withCredential(t).startup(t)
			ctx := context.Background()
			_, err := e.b.Transcribe(ctx, e.job(t), nil)
			if Code(err) != CodeAPIChanged || !IsFallback(err) {
				t.Fatalf("err %v", err)
			}
			if ep, _ := codedParam(err, "endpoint"); ep != tc.endpoint {
				t.Errorf("endpoint %q", ep)
			}
			if !strings.Contains(err.Error(), "Colab 接口可能已变更") {
				t.Errorf("message %q", err.Error())
			}
			if st := e.b.Status(ctx); st.LastError == nil || st.LastError.Code != CodeAPIChanged || st.State != StateNone {
				t.Errorf("status %+v", st)
			}
			e.rs.finish()
			e.checkClean(t, err)
		})
	}
}

func leftoverRecord(t *testing.T, kv KV, rec sessionRecord) {
	t.Helper()
	rec.Version = recordVersion
	b, _ := json.Marshal(rec)
	if err := kv.SetKV(context.Background(), KVSession, string(b)); err != nil {
		t.Fatal(err)
	}
}

func TestReplayLeftoverReleasedAtStartup(t *testing.T) {
	for _, tc := range []struct {
		sample string
		rec    sessionRecord
	}{
		{"leftover_listed.json", sessionRecord{State: StateRunning, Endpoint: "gpu-t4-s-replay1", Accelerator: "T4", RunID: "run-0"}},
		{"leftover_gone.json", sessionRecord{State: StateRunning, Endpoint: "gpu-t4-s-replay1", Accelerator: "T4", RunID: "run-0"}},
		{"leftover_queued.json", sessionRecord{State: StateQueued, NotebookHash: "11111111_2222_4333_8444_555555555555........",
			Accelerator: "T4", RunID: "run-0"}},
	} {
		t.Run(tc.sample, func(t *testing.T) {
			e := newEnv(t, tc.sample).withCredential(t)
			leftoverRecord(t, e.kv, tc.rec)
			e.startup(t)
			ctx := context.Background()
			if v, _, _ := e.kv.GetKV(ctx, KVSession); v != "" {
				t.Errorf("record left: %s", v)
			}
			if st := e.b.Status(ctx); st.Leftover != nil || st.LastRelease == nil || st.LastRelease.Reason != ReleaseLeftover {
				t.Errorf("status %+v", st)
			}
			e.rs.finish()
			e.checkClean(t)
		})
	}
}

func TestReplayLeftoverWaitsForCredential(t *testing.T) {
	e := newEnv(t, "leftover_listed.json")
	e.b.o.ADCPath = writeADC(t, t.TempDir(), "authorized_user")
	leftoverRecord(t, e.kv, sessionRecord{State: StateRunning, Endpoint: "gpu-t4-s-replay1", Accelerator: "T4", RunID: "run-0"})
	e.startup(t) // no credential: nothing can be released, nothing is sent
	ctx := context.Background()
	st := e.b.Status(ctx)
	if st.Leftover == nil || st.Leftover.Code != CodeLeftoverSession || st.Leftover.Accelerator != "T4" {
		t.Fatalf("status %+v", st)
	}
	// Importing a credential releases it (the import's refresh is the
	// sample's first step).
	if _, err := e.b.ImportADC(ctx, Lifecycle{Kind: LifecycleManual}); err != nil {
		t.Fatalf("ImportADC: %v", err)
	}
	if st := e.b.Status(ctx); st.Leftover != nil || st.LastRelease == nil || st.LastRelease.Reason != ReleaseLeftover {
		t.Errorf("status %+v", st)
	}
	e.rs.finish()
	e.checkClean(t)
}

func TestReplayDisconnect(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "disconnect.json").withCredential(t).startup(t)
	res, err := e.b.Disconnect(ctx)
	if err != nil || !res.Revoked || res.Released || len(res.Warnings) != 0 {
		t.Fatalf("Disconnect = %+v, %v", res, err)
	}
	if v, _, _ := e.kv.GetKV(ctx, KVCredential); v != "" {
		t.Error("credential not deleted")
	}
	if ok, w := e.b.CanDispatch(ctx, 600); ok || w != model.WaitRemoteAuth {
		t.Errorf("CanDispatch after disconnect = %v %q", ok, w)
	}
	e.rs.finish()
	e.checkClean(t)

	// Revocation fails: the local copy is deleted anyway and the failure
	// reported.
	e = newEnv(t, "disconnect_revoke_fails.json").withCredential(t).startup(t)
	res, err = e.b.Disconnect(ctx)
	if err != nil || res.Revoked || len(res.Warnings) != 1 || res.Warnings[0].Code != CodeRevokeFailed {
		t.Fatalf("Disconnect = %+v, %v", res, err)
	}
	if v, _, _ := e.kv.GetKV(ctx, KVCredential); v != "" {
		t.Error("credential not deleted after a failed revocation")
	}
	e.rs.finish()
	b, _ := json.Marshal(res)
	e.checkClean(t)
	assertNoSecrets(t, "disconnect result", string(b))
}

// blockingConn is kernel channels that never answer: the lecture "runs"
// until it is interrupted.
type blockingConn struct{ closed chan struct{} }

func (c *blockingConn) ReadMessage(ctx context.Context) (bool, []byte, error) {
	select {
	case <-ctx.Done():
		return false, nil, ctx.Err()
	case <-c.closed:
		return false, nil, errWSClosed
	}
}
func (c *blockingConn) WriteText(context.Context, []byte) error { return nil }
func (c *blockingConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func blockingDial(o *Options) {
	o.Dial = func(context.Context, string, http.Header) (MessageConn, error) {
		return &blockingConn{closed: make(chan struct{})}, nil
	}
}

func runUntilRemote(t *testing.T, e *testEnv, ctx context.Context) (chan error, *eventLog) {
	t.Helper()
	var evs eventLog
	reached := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := e.b.Transcribe(ctx, e.job(t), func(ev Event) {
			evs.add(ev)
			if ev.Step == StepRemoteTranscribe {
				once.Do(func() { close(reached) })
			}
		})
		done <- err
	}()
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("Transcribe ended early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the lecture never reached the remote transcription")
	}
	return done, &evs
}

func TestReplayShutdownNowRequeues(t *testing.T) {
	e := newEnv(t, "interrupted_during_run.json", blockingDial).withCredential(t).startup(t)
	ctx := context.Background()
	done, _ := runUntilRemote(t, e, ctx)
	if st := e.b.Status(ctx); st.Current == nil || st.Current.Step != StepRemoteTranscribe {
		t.Errorf("status during the run %+v", st)
	}
	if err := e.b.ShutdownNow(ctx); err != nil {
		t.Fatalf("ShutdownNow: %v", err)
	}
	err := <-done
	if !IsRequeue(err) || IsFallback(err) || Code(err) != CodeShutdownRequeue {
		t.Fatalf("err %v", err)
	}
	if st := e.b.Status(ctx); st.State != StateReleased || st.LastRelease.Reason != ReleaseNow {
		t.Errorf("status %+v", st)
	}
	e.rs.finish()
	e.checkClean(t, err)
}

func TestReplayCancelReleasesWithinSeconds(t *testing.T) {
	e := newEnv(t, "interrupted_during_run.json", blockingDial).withCredential(t).startup(t)
	ctx, cancel := context.WithCancel(context.Background())
	done, _ := runUntilRemote(t, e, ctx)
	start := time.Now()
	cancel()
	err := <-done
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("cancel took %s", took)
	}
	if !errors.Is(err, context.Canceled) || Code(err) != CodeCancelled {
		t.Fatalf("err %v", err)
	}
	if st := e.b.Status(context.Background()); st.State != StateReleased || st.LastRelease.Reason != ReleaseCancelled {
		t.Errorf("status %+v", st)
	}
	e.rs.finish()
	e.checkClean(t, err)
}

func TestReplayLowBalance(t *testing.T) {
	e := newEnv(t, "ccu_low_balance.json").withCredential(t).startup(t)
	ctx := context.Background()
	if ok, w := e.b.CanDispatch(ctx, 3000); ok || w != model.WaitRemoteBalance {
		t.Fatalf("CanDispatch = %v %q", ok, w)
	}
	// Within a minute the balance is not read again (no further step).
	e.clk.Advance(30 * time.Second)
	if ok, w := e.b.CanDispatch(ctx, 3000); ok || w != model.WaitRemoteBalance {
		t.Fatalf("CanDispatch = %v %q", ok, w)
	}
	// colab-obs-core: the tier follows the account (0.1 CU paid: paid);
	// a tier in the settings is ignored.
	set := e.b.UserSettings()
	set.Tier = TierFree
	_ = e.b.SetSettings(ctx, set)
	if ok, w := e.b.CanDispatch(ctx, 3000); ok || w != model.WaitRemoteBalance || e.b.Settings().Tier != TierPaid {
		t.Errorf("tier setting not ignored: CanDispatch = %v %q", ok, w)
	}
	e.rs.finish()
	e.checkClean(t)
}

// TestReplaySamplesAnnotated: every sample names its upstream source and
// says whether it is a real recording.
func TestReplaySamplesAnnotated(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "replay", "*.json"))
	if len(files) < 15 {
		t.Fatalf("%d samples", len(files))
	}
	for _, f := range files {
		ff, steps, _ := loadReplay(t, filepath.Base(f))
		if ff.Upstream.Package != "google-colab-cli" || ff.Upstream.Version != "0.7.5" {
			t.Errorf("%s: upstream %+v", f, ff.Upstream)
		}
		if !ff.Recorded && !strings.Contains(ff.Source, "非真实录制") {
			t.Errorf("%s: a constructed sample must say 非真实录制", f)
		}
		if len(steps) == 0 {
			t.Errorf("%s: no steps", f)
		}
	}
}

// TestReplayCredentialEndsWhileHoldingRuntime: the lifecycle ends while a
// runtime is held (e.g. the computer slept past it). The credential is
// deleted and never used again, so the runtime cannot be released: the
// record stays for the next import, Status reports it, Tick stops trying.
func TestReplayCredentialEndsWhileHoldingRuntime(t *testing.T) {
	e := newEnv(t, "acquire_only.json").startup(t)
	ctx := context.Background()
	storeCredential(t, e.kv, Lifecycle{Kind: LifecycleDays, Days: 1}, e.clk.Now().Add(-22*time.Hour), "run-1")
	jp := &jobProgress{b: e.b, out: func(Event) {}, est: -1, j: &jobState{started: e.clk.Now()}}
	if _, err := e.b.acquire(ctx, e.b.Settings(), jp); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(3 * time.Hour)
	e.b.Tick(ctx)
	st := e.b.Status(ctx)
	if st.Leftover == nil || st.Leftover.Code != CodeLeftoverSession || st.State != StateNone || st.Configured {
		t.Fatalf("status %+v", st)
	}
	if v, _, _ := e.kv.GetKV(ctx, KVSession); v == "" {
		t.Error("the record of the unreleased runtime was dropped")
	}
	if v, _, _ := e.kv.GetKV(ctx, KVCredential); v != "" {
		t.Error("expired credential kept")
	}
	e.b.Tick(ctx) // nothing more to try
	e.rs.finish()
	e.checkClean(t)
}

// TestReplayCleanupFailureReleases (Gate round 1 item 8): after a
// successful lecture result.json cannot be deleted on the runtime. The
// deletion is retried three times with growing waits, then the runtime is
// released at once (its disk goes with it); the release names the file,
// never any content. The lecture itself succeeded.
func TestReplayCleanupFailureReleases(t *testing.T) {
	e := newEnv(t, "cleanup_fails.json").withCredential(t).startup(t)
	ctx := context.Background()
	path, err := e.b.Transcribe(ctx, e.job(t), nil)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got, _ := os.ReadFile(path); !sameResult(got, e.rs.vars["result_json"]) {
		t.Fatal("result.json differs from the download")
	}
	// 3.2.0: the lecture is done; its files go in the background (the
	// removal cell cannot confirm, the deletions one by one are retried).
	e.b.removalsSettled()
	st := e.b.Status(ctx)
	if st.State != StateReleased || st.LastRelease == nil || st.LastRelease.Reason != ReleaseCleanupFailed ||
		st.LastRelease.File != "result.json" {
		t.Fatalf("status %+v (last release %+v)", st, st.LastRelease)
	}
	if v, _, _ := e.kv.GetKV(ctx, KVSession); v != "" {
		t.Errorf("session record left: %s", v)
	}
	// The waits between the attempts grow: 1 s, 2 s, 4 s.
	e.clk.mu.Lock()
	sleeps := slices.Clone(e.clk.sleeps)
	e.clk.mu.Unlock()
	var waits []time.Duration
	for _, d := range sleeps {
		if d == time.Second || d == 2*time.Second || d == 4*time.Second {
			waits = append(waits, d)
		}
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !slices.Equal(waits[max(0, len(waits)-3):], want) {
		t.Errorf("waits %v, want %v at the end", sleeps, want)
	}
	logs := e.log.String()
	if !strings.Contains(logs, "result.json") || !strings.Contains(logs, "cleanup_failed") {
		t.Errorf("the log does not tell why the runtime was released:\n%s", logs)
	}
	e.rs.finish()
	e.checkClean(t)
}
