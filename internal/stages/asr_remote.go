// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

// An asr run on an SSH inference server (ssh-mvp; plan §5): the job.json is
// the one a local run of the same engine would get — built by the same
// constructor (asrCallAt), for the engine's local device (mlx:gpu,
// cuda:0) — without its paths (remote.StripPaths: the agent adds its own);
// the checkpoint object names the server's device, so its parts say where
// they ran. The server returns result.json checked and re-encoded
// (remote.ValidateResult); the stage stores it like a local result.
// Errors are the server's (*remote.Error: the scheduler hands the lecture
// back or requeues it) or sched.ErrRemoteFallback without a server; a
// cancelled ctx returns within remoteCancelGrace.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/sched"
	"lectureflow/internal/wav"
)

func (s *asrStage) sshRun(ctx context.Context, job model.Job, run model.ASRSnapshot, dir, audio, prompt string,
	progress func(float64)) ([]byte, error) {
	fleet := sched.SSHFrom(ctx)
	if fleet == nil {
		return nil, fmt.Errorf("%w (no SSH servers in this run)", sched.ErrRemoteFallback)
	}
	dev, ok := fleet.Device(run.DeviceID)
	if !ok {
		return nil, fmt.Errorf("%w (the server %s is no longer configured)", sched.ErrRemoteFallback, run.DeviceID)
	}
	ms, ok := wav.DurationMS(audio)
	if !ok || ms <= 0 {
		return nil, sched.Permanent(model.Errorf(CodeASRAudioMissing, map[string]any{"file": model.AudioFile},
			fmt.Errorf("%s 无法读取（预处理的产物），请重新运行预处理阶段", audio)))
	}
	sum, err := s.wavSHA256(audio)
	if err != nil {
		return nil, err
	}
	params, err := s.remoteParams(ctx, job, run, dir, audio, prompt, sum)
	if err != nil {
		return nil, err
	}
	rjob := remote.Job{ID: job.ID, LectureID: job.ScopeID, AudioPath: audio, AudioSHA256: sum, DurationSec: float64(ms) / 1000,
		Engine: run.Backend, Model: run.Model, Precision: run.Precision, Params: params, OutDir: dir}
	rep := &remoteReporter{ctx: ctx, acc: s.d.deviceLabel(ctx, run.DeviceID), progress: progress}
	rep.report(colab.Event{Step: colab.StepWaitAccelerator, ETASec: -1}, true)
	type outcome struct {
		path string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		p, err := dev.Transcribe(ctx, rjob, func(ev remote.Event) {
			rep.report(colab.Event{Step: ev.Step, Stage: ev.Stage, Fraction: ev.Fraction, Overall: ev.Overall,
				WaitingFor: ev.WaitingFor, ETASec: ev.ETASec}, false)
		})
		done <- outcome{p, err}
	}()
	var o outcome
	select {
	case o = <-done:
	case <-ctx.Done():
		select {
		case o = <-done:
		case <-time.After(remoteCancelGrace):
			return nil, fmt.Errorf("%w: the run on the SSH server was stopped; its files are removed in the background", ctx.Err())
		}
	}
	if o.err != nil {
		if ctx.Err() != nil && !errors.Is(o.err, context.Canceled) {
			return nil, fmt.Errorf("%w: %w", ctx.Err(), o.err)
		}
		return nil, o.err
	}
	raw, err := os.ReadFile(o.path)
	if err != nil {
		return nil, model.Errorf(CodeASRResultUnparsable, nil, fmt.Errorf("远程服务器的转写结果无法读取：%w", err))
	}
	return raw, nil
}

// remoteEngineDevice is the local device whose job.json an engine's remote
// run gets.
func remoteEngineDevice(engine string) string {
	if engine == model.BackendMLX {
		return model.DeviceMLXGPU
	}
	return model.DeviceCUDA0
}

// remoteParams is the run's job.json without paths (remote.Job.Params).
func (s *asrStage) remoteParams(ctx context.Context, job model.Job, run model.ASRSnapshot, dir, audio, prompt, wavSum string) (map[string]any, error) {
	engine := run.Backend
	if engine != model.BackendMLX && engine != model.BackendFasterWhisper {
		return nil, sched.Permanent(model.Errorf(CodeASRDeviceMissing, map[string]any{"device": s.d.deviceLabel(ctx, run.DeviceID)},
			fmt.Errorf("远程服务器的后端 %q 无法运行", engine)))
	}
	local := run
	local.DeviceID = remoteEngineDevice(engine)
	chunk := 0
	if engine == model.BackendMLX {
		chunk = ovCheckpointChunkS
	}
	ckpt := &devices.CheckpointJob{JobID: job.ID, InputHash: ctxbuild.ASRInputHashV3(wavSum, run.Language),
		DeviceID: run.DeviceID, Model: run.Model, Precision: run.Precision, ChunkS: chunk}
	// The remote's own interpreter and model: placeholders, removed below.
	w, err := s.d.asrCallAt(ctx, local, dir, audio, prompt, ckpt, &workerLoc{python: "remote", modelDir: "remote"})
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(w.job)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	remote.StripPaths(engine, m)
	if _, err := remote.CheckParams(engine, paramsJSON(m)); err != nil {
		return nil, fmt.Errorf("the remote job.json of %s: %w", engine, err)
	}
	return m, nil
}

func paramsJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
