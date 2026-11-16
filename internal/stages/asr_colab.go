// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/sched"
	"lectureflow/internal/wav"
)

// remoteStatsEvery is how often a remote run's ETA alone refreshes
// stats.remote (a step, stage or wait change always does).
const remoteStatsEvery = 2 * time.Second

// remoteCancelGrace is how long a cancelled remote run waits for the
// backend's own return before the stage returns (tests shorten it).
var remoteCancelGrace = 3 * time.Second

// remoteRun transcribes the lecture on Colab (SPEC-v3 V14.3) through the
// scheduler's remote backend (sched.RemoteFrom): audio.wav of prep, the
// snapshot's language and the same initial_prompt as a local run (G4).
// Progress is the lecture's overall fraction (colab.Event.Overall);
// stats.remote follows the hover card's sub-step, the remote stage, the
// ETA and the accelerator. It returns result.json's bytes (the local
// faster-whisper worker's structure). Errors are Colab's own
// (*model.CodedError; colab.IsFallback / IsRequeue tell the scheduler to
// hand the lecture back) or sched.ErrRemoteFallback without a backend; a
// cancelled ctx stops the run within seconds (the runtime is released).
func (s *asrStage) remoteRun(ctx context.Context, job model.Job, run model.ASRSnapshot, dir, audio, prompt string,
	progress func(float64)) ([]byte, error) {
	rb := sched.RemoteFrom(ctx)
	if rb == nil {
		return nil, fmt.Errorf("%w (no Colab backend in this run)", sched.ErrRemoteFallback)
	}
	ms, ok := wav.DurationMS(audio)
	if !ok || ms <= 0 {
		return nil, sched.Permanent(model.Errorf(CodeASRAudioMissing, map[string]any{"file": model.AudioFile},
			fmt.Errorf("%s 无法读取（预处理的产物），请重新运行预处理阶段", audio)))
	}
	acc := rb.Status(ctx).Accelerator
	if acc == "" {
		acc = rb.Settings().Accelerator
	}
	rep := &remoteReporter{ctx: ctx, acc: acc, progress: progress}
	rep.report(colab.Event{Step: colab.StepWaitAccelerator, ETASec: -1}, true)
	type outcome struct {
		path string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		p, err := rb.Transcribe(ctx, colab.Job{
			ID: job.ID, AudioPath: audio, DurationSec: float64(ms) / 1000,
			Language: run.Language, InitialPrompt: prompt, OutDir: dir,
			NoRedecode: !s.d.redecodeOn(ctx),
		}, func(ev colab.Event) { rep.report(ev, false) })
		done <- outcome{p, err}
	}()
	var o outcome
	select {
	case o = <-done:
	case <-ctx.Done():
		// A cancel takes effect within seconds (SPEC §5): the backend
		// releases the runtime synchronously (up to a minute when Colab is
		// slow); the job does not wait for that. The backend takes no other
		// lecture before its release has ended.
		select {
		case o = <-done:
		case <-time.After(remoteCancelGrace):
			return nil, fmt.Errorf("%w: the Colab run was stopped; its runtime is released in the background", ctx.Err())
		}
	}
	path, err := o.path, o.err
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("%w: %w", ctx.Err(), err)
		}
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, model.Errorf(CodeASRResultUnparsable, nil, fmt.Errorf("Colab 的转写结果无法读取：%w", err))
	}
	return raw, nil
}

// remoteReporter turns colab.Events into the run's progress and
// stats.remote (throttled: the stats sink is in memory, the scheduler
// flushes it).
type remoteReporter struct {
	ctx      context.Context
	acc      string
	progress func(float64)

	mu   sync.Mutex
	last model.RemoteStats
	at   time.Time
}

func (r *remoteReporter) report(ev colab.Event, force bool) {
	if ev.Overall > 0 && !math.IsNaN(ev.Overall) {
		r.progress(min(ev.Overall, 1))
	}
	eta := int64(-1)
	if ev.ETASec >= 0 && !math.IsNaN(ev.ETASec) && !math.IsInf(ev.ETASec, 0) {
		eta = int64(math.Round(ev.ETASec))
	}
	step := ev.Step
	if step == "" {
		step = model.RemoteStepWaitAccelerator
	}
	st := model.RemoteStats{Step: step, Steps: model.RemoteSteps, EtaSec: eta, Accelerator: r.acc,
		Stage: ev.Stage, WaitingFor: ev.WaitingFor}
	if ev.Reconnect != nil { // a copy: the backend keeps its own
		rc := *ev.Reconnect
		st.Reconnect = &rc
	}
	now := time.Now()
	r.mu.Lock()
	// a reconnect that starts, moves on or ends is a change too (an event
	// without one clears it from stats.remote)
	changed := force || r.last.Step != st.Step || r.last.Stage != st.Stage || r.last.WaitingFor != st.WaitingFor ||
		!sameReconnect(r.last.Reconnect, st.Reconnect)
	if !changed && now.Sub(r.at) < remoteStatsEvery {
		r.mu.Unlock()
		return
	}
	r.last, r.at = st, now
	r.mu.Unlock()
	sched.UpdateStats(r.ctx, func(js *model.JobStats) {
		v := st
		v.Steps = append([]string(nil), st.Steps...)
		if st.Reconnect != nil {
			rc := *st.Reconnect
			v.Reconnect = &rc
		}
		js.Remote = &v
	})
}

// sameReconnect reports whether a and b tell the same reconnect state.
func sameReconnect(a, b *remote.Reconnect) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Attempt == b.Attempt && a.Of == b.Of && a.NextAt.Equal(b.NextAt)
}
