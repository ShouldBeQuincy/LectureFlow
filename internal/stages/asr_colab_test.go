// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/sched"
)

// SPEC-v3 V14.3 (v14-colab-integrate): an asr job bound to colab:remote
// runs through the scheduler's remote backend (fake): prep's audio.wav, the
// snapshot's language, the same initial_prompt as a local run, progress
// from the events, stats.remote, result.json parsed like the local worker's.

type fakeColabASR struct {
	mu     sync.Mutex
	jobs   []colab.Job
	err    error
	result string        // file copied to OutDir/result.json
	block  bool          // wait for ctx
	linger time.Duration // after ctx ends, before returning (a slow release)
}

func (f *fakeColabASR) CanDispatch(context.Context, float64) (bool, string) { return true, "" }
func (f *fakeColabASR) SetQueue([]float64)                                  {}
func (f *fakeColabASR) QueueDrained(context.Context) error                  { return nil }
func (f *fakeColabASR) ShutdownAfterCurrent(context.Context) error          { return nil }
func (f *fakeColabASR) ShutdownNow(context.Context) error                   { return nil }
func (f *fakeColabASR) Settings() colab.Settings                            { return colab.DefaultSettings() }
func (f *fakeColabASR) Credential(context.Context) colab.CredentialInfo {
	return colab.CredentialInfo{Present: true, RemainingSec: -1}
}
func (f *fakeColabASR) Status(context.Context) colab.Status {
	return colab.Status{Service: "colab", Accelerator: "L4", State: colab.StateRunning}
}

func (f *fakeColabASR) Transcribe(ctx context.Context, job colab.Job, progress func(colab.Event)) (string, error) {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	err, block := f.err, f.block
	f.mu.Unlock()
	progress(colab.Event{Step: colab.StepWaitAccelerator, Fraction: -1, Overall: 0, WaitingFor: model.WaitRemoteQueue, ETASec: -1})
	progress(colab.Event{Step: colab.StepUploadAudio, Fraction: 0.5, Overall: 0.1, ETASec: 300})
	progress(colab.Event{Step: colab.StepRemoteTranscribe, Stage: "transcribe", Fraction: 0.5, Overall: 0.5, ETASec: 120.4})
	if block {
		<-ctx.Done()
		time.Sleep(f.linger)
		return "", colab.NewFallbackError(colab.CodeCancelled, nil) // as the backend reports a cancel
	}
	if err != nil {
		return "", err
	}
	progress(colab.Event{Step: colab.StepDownloadResult, Fraction: 1, Overall: 0.95, ETASec: 5})
	progress(colab.Event{Step: colab.StepVerify, Fraction: 1, Overall: 1, ETASec: 0})
	b, rerr := os.ReadFile(f.result)
	if rerr != nil {
		return "", rerr
	}
	out := filepath.Join(job.OutDir, "result.json")
	if werr := writeFileAtomic(out, b); werr != nil {
		return "", werr
	}
	return out, nil
}

func (e *env) colabLecture(lang string) model.Lecture {
	e.t.Helper()
	l := e.lecture("L colab", "m.mp4")
	e.must(os.WriteFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.AudioFile), silenceWAV(2000), 0o644))
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceColab, "large-v3", "float16", lang))
	return l
}

func TestASRColabRun(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR // a colab snapshot never uses the local fake (nor a local worker)
	e.addTerm("cache", "缓存", model.SourceSyllabus, 9)
	l := e.colabLecture("de")
	dir, _ := filepath.Abs(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID))
	ckpt := filepath.Join(dir, model.ASRCheckpointFile)
	e.must(os.WriteFile(ckpt, []byte(`{"version":1}`), 0o644)) // a local checkpoint of an earlier attempt
	fr := &fakeColabASR{result: fakeASR}
	r := e.runCtx(sched.WithRemote(e.ctx, fr), e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(fr.jobs) != 1 {
		t.Fatalf("%d remote jobs", len(fr.jobs))
	}
	got := fr.jobs[0]
	if got.ID != r.job.ID || got.AudioPath != filepath.Join(dir, model.AudioFile) || got.OutDir != dir ||
		got.Language != "de" || got.InitialPrompt != "cache" || math.Abs(got.DurationSec-2) > 0.01 {
		t.Fatalf("remote job = %+v", got)
	}
	if n := len(e.sentences(l.ID)); n != 14 {
		t.Fatalf("%d sentences", n)
	}
	if r.job.Status != model.StatusDone || r.stats.ASRPrompt != "cache" || r.stats.HashVersion != ASRHashVersion {
		t.Fatalf("job %s stats %+v", r.job.Status, r.stats)
	}
	rs := r.stats.Remote
	if rs == nil || rs.Step != model.RemoteStepVerify || rs.Accelerator != "L4" || !slices.Equal(rs.Steps, model.RemoteSteps) || rs.EtaSec != 0 {
		t.Fatalf("stats.remote = %+v", rs)
	}
	if !slices.Contains(r.progress.vals, 0.5) || r.progress.last() != 1 {
		t.Fatalf("progress = %v", r.progress.vals)
	}
	if _, err := os.Stat(ckpt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the local checkpoint is still there: %v", err)
	}
	// No observed memory for a remote device.
	if _, ok, _ := e.st.GetKV(e.ctx, model.MemObservedKey(model.DeviceColab, "large-v3", "float16")); ok {
		t.Fatal("mem_observed recorded for colab:remote")
	}
}

func TestASRColabFallbackAndCancel(t *testing.T) {
	e := newEnv(t)
	l := e.colabLecture("en")
	fr := &fakeColabASR{err: colab.NewFallbackError(colab.CodeSessionReclaimed, nil)}
	r := e.runCtx(sched.WithRemote(e.ctx, fr), e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err == nil || !colab.IsFallback(r.err) || colab.Code(r.err) != colab.CodeSessionReclaimed {
		t.Fatalf("err = %v", r.err)
	}
	if n := len(e.sentences(l.ID)); n != 0 {
		t.Fatalf("%d sentences after a hand-back", n)
	}
	if rs := r.stats.Remote; rs == nil || rs.Step != model.RemoteStepRemoteTranscribe || rs.Stage != "transcribe" || rs.EtaSec != 120 {
		t.Fatalf("stats.remote = %+v", rs)
	}

	// Without a remote backend in the run: back to the local devices.
	r = e.runCtx(e.ctx, e.stage(e.deps(nil), model.StageASR), l.ID)
	if !errors.Is(r.err, sched.ErrRemoteFallback) {
		t.Fatalf("err without a backend = %v", r.err)
	}

	// Cancel: the run returns promptly with the context's error.
	ctx, cancel := context.WithCancel(e.ctx)
	fr = &fakeColabASR{block: true}
	done := make(chan runResult, 1)
	go func() { done <- e.runCtx(sched.WithRemote(ctx, fr), e.stage(e.deps(nil), model.StageASR), l.ID) }()
	for {
		fr.mu.Lock()
		n := len(fr.jobs)
		fr.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	r = <-done
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err after cancel = %v", r.err)
	}
}

func TestASRColabBadAudio(t *testing.T) {
	e := newEnv(t)
	l := e.colabLecture("en")
	e.writeWAV(l, "not a wav")
	fr := &fakeColabASR{result: fakeASR}
	r := e.runCtx(sched.WithRemote(e.ctx, fr), e.stage(e.deps(nil), model.StageASR), l.ID)
	if !sched.IsPermanent(r.err) || len(fr.jobs) != 0 {
		t.Fatalf("err = %v, remote jobs %d", r.err, len(fr.jobs))
	}
}

// A cancel takes effect within seconds even while the backend still
// releases the runtime (up to a minute when Colab is slow).
func TestASRColabCancelDoesNotWaitForTheRelease(t *testing.T) {
	old := remoteCancelGrace
	remoteCancelGrace = 50 * time.Millisecond
	t.Cleanup(func() { remoteCancelGrace = old })
	e := newEnv(t)
	l := e.colabLecture("en")
	fr := &fakeColabASR{block: true, linger: 5 * time.Second}
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan runResult, 1)
	go func() { done <- e.runCtx(sched.WithRemote(ctx, fr), e.stage(e.deps(nil), model.StageASR), l.ID) }()
	for {
		fr.mu.Lock()
		n := len(fr.jobs)
		fr.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	cancel()
	r := <-done
	if took := time.Since(start); took > 2*time.Second || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancel took %v, err %v", took, r.err)
	}
}

// v3.2.x: the backend's reconnect state reaches stats.remote at once (a
// change, not throttled), each attempt too, and the next event without one
// clears it; the stats hold a copy of the event's value.
func TestASRColabReconnectInStats(t *testing.T) {
	rec := &sched.StatsRecorder{}
	rep := &remoteReporter{ctx: sched.WithStatsSink(context.Background(), rec), acc: "T4", progress: func(float64) {}}
	remoteOf := func() *model.RemoteStats { return rec.Snapshot().Remote }
	tr := colab.Event{Step: colab.StepRemoteTranscribe, Stage: "transcribe", Fraction: 0.4, Overall: 0.4, ETASec: 300}
	rep.report(tr, true)
	if rs := remoteOf(); rs == nil || rs.Reconnect != nil {
		t.Fatalf("before: %+v", rs)
	}
	next := time.Date(2026, 11, 11, 9, 0, 5, 0, time.UTC)
	rc := &remote.Reconnect{Attempt: 1, Of: 4, NextAt: next}
	ev := tr
	ev.Reconnect = rc
	rep.report(ev, false) // within remoteStatsEvery of the last write: still written
	rs := remoteOf()
	if rs == nil || rs.Reconnect == nil || *rs.Reconnect != *rc || rs.Step != model.RemoteStepRemoteTranscribe {
		t.Fatalf("reconnecting: %+v", rs)
	}
	rc.Attempt = 9 // the backend's value changes later: the stats keep theirs
	if remoteOf().Reconnect.Attempt != 1 {
		t.Fatal("stats.remote.reconnect shares the event's value")
	}
	ev.Reconnect = &remote.Reconnect{Attempt: 2, Of: 4, NextAt: next.Add(15 * time.Second)}
	rep.report(ev, false)
	if rs := remoteOf(); rs.Reconnect == nil || rs.Reconnect.Attempt != 2 || !rs.Reconnect.NextAt.Equal(next.Add(15*time.Second)) {
		t.Fatalf("second attempt: %+v", rs.Reconnect)
	}
	// the same state again within the interval: not written again
	rec.Update(func(st *model.JobStats) { st.Remote.EtaSec = 7 })
	rep.report(ev, false)
	if remoteOf().EtaSec != 7 {
		t.Fatal("an unchanged reconnect state was written again within remoteStatsEvery")
	}
	rep.report(tr, false) // the runtime answers again
	if rs := remoteOf(); rs == nil || rs.Reconnect != nil || rs.EtaSec != 300 {
		t.Fatalf("after the reconnect: %+v", rs)
	}
	b, _ := json.Marshal(remoteOf())
	if bytes.Contains(b, []byte("reconnect")) {
		t.Fatalf("stats.remote after the reconnect: %s", b)
	}
}
