// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// One lecture on a server (plan §5): open the job, upload the audio (WAV
// for MLX, FLAC for CUDA; resumed at the agent's "have"), run the worker
// with the job's path-free parameters, download result.json, check it
// (size, SHA-256, remote.ValidateResult), write it atomically and delete
// the remote job directory. A cancel returns within remote.CancelWithin
// (the remote cancel and delete continue in the background). A lost
// connection is retried for ReconnectGrace: the job directory is still
// there (the agent keeps it with an expiry) and the worker continues from
// its checkpoint; after that the lecture goes back to the local devices.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/remote"
)

// Overall progress of the steps.
const (
	pConnect = 0.02
	pUpload  = 0.10
	pRun     = 0.97
	pFetch   = 0.99
)

type reporter struct {
	fn    func(remote.Event)
	start time.Time
	runAt time.Time
}

func (r *reporter) emit(step, stage string, fraction, overall float64, waiting string) {
	if r.fn == nil {
		return
	}
	eta := -1.0
	if step == remote.StepTranscribe && fraction > 0.02 && !r.runAt.IsZero() {
		el := time.Since(r.runAt).Seconds()
		eta = el / fraction * (1 - fraction)
	}
	r.fn(remote.Event{Step: step, Stage: stage, Fraction: fraction, Overall: overall, ETASec: eta, WaitingFor: waiting})
}

// Transcribe runs job on the server (remote.Device).
func (s *Server) Transcribe(ctx context.Context, job remote.Job, progress func(remote.Event)) (string, error) {
	label := s.label()
	p := map[string]any{"device": label}
	rep := &reporter{fn: progress, start: time.Now()}
	rep.emit(remote.StepConnect, "", 0, 0, "")
	if !remote.JobIDPattern.MatchString(job.ID) {
		return "", remoteErr(remote.ErrLocal, remote.CodeAudioUnreadable, nil, "", errors.New("bad job id"))
	}
	s.mu.Lock()
	if len(s.runs) >= s.slotsLocked() {
		s.mu.Unlock()
		return "", busy(remote.CodeBusy, p, "", errors.New("every slot is in use"))
	}
	cj := &remote.CardJob{LectureID: job.LectureID, JobID: job.ID, Stage: remote.StepConnect}
	s.runs[job.ID] = cj
	s.mu.Unlock()
	s.m.changed()
	defer func() {
		s.mu.Lock()
		delete(s.runs, job.ID)
		s.lastUse = s.m.now()
		s.mu.Unlock()
		s.m.changed()
	}()

	// The audio to upload.
	up, cleanup, err := s.prepareAudio(ctx, job)
	if err != nil {
		return "", err
	}
	defer cleanup()

	var lostAt time.Time
	grace := s.m.o.ReconnectGrace
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		c, ctl, err := s.session(ctx, false)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if attempt == 0 {
				// Nothing ran: back to the queue as it was (the scheduler's
				// admission sees the server's state).
				_, code, wf := connError(err)
				return "", busy(code, connErrParams(label, err), wf, err)
			}
			if lostAt.IsZero() {
				lostAt = time.Now()
			}
			if time.Since(lostAt) > grace {
				return "", fallback(remote.CodeDisconnected, map[string]any{"device": label, "minutes": int(grace.Minutes())}, err)
			}
			rep.emit(remote.StepTranscribe, remote.StageReconnect, -1, pUpload, model.WaitRemoteOffline)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(min(time.Duration(1<<min(attempt, 5))*time.Second, 30*time.Second)):
			}
			continue
		}
		rep.emit(remote.StepConnect, "", 1, pConnect, "")
		path, err := s.runOnce(ctx, c, ctl, job, up, rep, cj)
		if err == nil {
			return path, nil
		}
		if ctx.Err() != nil {
			s.abandon(job.ID, ctl)
			return "", ctx.Err()
		}
		if !errors.Is(err, errLost) && !errors.Is(err, errControlClosed) {
			return "", err
		}
		if lostAt.IsZero() {
			lostAt = time.Now()
		}
		if time.Since(lostAt) > grace {
			return "", fallback(remote.CodeDisconnected, map[string]any{"device": label, "minutes": int(grace.Minutes())}, err)
		}
		slog.Info("ssh server: connection lost during a lecture; reconnecting", "device", s.devID, "job", job.ID)
		rep.emit(remote.StepTranscribe, remote.StageReconnect, -1, pUpload, model.WaitRemoteOffline)
	}
}

// upload is the prepared audio.
type upload struct {
	path string
	kind string
	size int64
	sum  string
	ms   int64
}

// prepareAudio is the file to send: audio.wav for MLX, a FLAC made with
// ffmpeg for faster-whisper (plan §5).
func (s *Server) prepareAudio(ctx context.Context, job remote.Job) (upload, func(), error) {
	none := func() {}
	ms := int64(math.Round(job.DurationSec * 1000))
	if job.Engine == remote.EngineMLX {
		fi, err := os.Stat(job.AudioPath)
		if err != nil {
			return upload{}, none, remoteErr(remote.ErrLocal, remote.CodeAudioUnreadable, nil, "", err)
		}
		sum := job.AudioSHA256
		if !remote.SHA256Pattern.MatchString(sum) {
			if sum, err = fileSHA256(job.AudioPath); err != nil {
				return upload{}, none, remoteErr(remote.ErrLocal, remote.CodeAudioUnreadable, nil, "", err)
			}
		}
		return upload{path: job.AudioPath, kind: remote.AudioWAV, size: fi.Size(), sum: sum, ms: ms}, none, nil
	}
	dir := job.WorkDir
	if dir == "" {
		dir = job.OutDir
	}
	out := filepath.Join(dir, "ssh-upload-"+job.ID+".flac")
	tmp := out + ".tmp"
	encode := s.m.o.EncodeFLAC
	if encode == nil {
		encode = func(ctx context.Context, in, out string) error {
			ffmpeg := s.m.o.FFmpeg
			if ffmpeg == "" {
				ffmpeg = "ffmpeg"
			}
			cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", in,
				"-map", "0:a:0", "-c:a", "flac", "-compression_level", "8", "-f", "flac", out)
			return procutil.Run(cmd, procutil.Tied) // ends with LectureFlow, also on a crash (test-chaos 1)
		}
	}
	if err := encode(ctx, job.AudioPath, tmp); err != nil {
		_ = os.Remove(tmp)
		if ctx.Err() != nil {
			return upload{}, none, ctx.Err()
		}
		return upload{}, none, remoteErr(remote.ErrLocal, remote.CodeAudioUnreadable, nil, "", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return upload{}, none, err
	}
	clean := func() { _ = os.Remove(out) }
	fi, err := os.Stat(out)
	if err != nil {
		clean()
		return upload{}, none, err
	}
	sum, err := fileSHA256(out)
	if err != nil {
		clean()
		return upload{}, none, err
	}
	return upload{path: out, kind: remote.AudioFLAC, size: fi.Size(), sum: sum, ms: ms}, clean, nil
}

// runOnce is one attempt on a live connection.
func (s *Server) runOnce(ctx context.Context, c *conn, ctl *control, job remote.Job, up upload, rep *reporter,
	cj *remote.CardJob) (string, error) {
	label := s.label()
	p := map[string]any{"device": label}
	s.mu.Lock()
	build := s.m.builds[s.id]
	platform, vram := s.rec.Platform, int(s.probe.VRAMTotalMB)
	s.mu.Unlock()
	// The kit of the job's own model and precision (the snapshot bound at
	// the start; the session's kit follows the row, which may have changed).
	precision := modelPrecision(job.Engine, job.Precision, vram)
	kit, err := s.m.kitFor(platform, job.Engine, job.Model, precision)
	if err != nil {
		return "", fallback(remote.CodeSetupNeeded, p, err)
	}
	kitHash, err := s.kitReady(ctx, c, platform, build, kit)
	if err != nil {
		return "", err
	}
	if job.Engine == remote.EngineFasterWhisper && (job.Precision == "" || job.Precision == model.PrecisionAuto) {
		// "auto" is resolved by the remote's VRAM, not this machine's.
		params := maps.Clone(job.Params)
		params["compute_type"] = precision
		job.Params = params
	}
	ok, err := ctl.call(ctx, remote.Request{Op: remote.OpJob, Job: job.ID, AudioKind: up.kind, AudioSize: up.size, AudioSHA256: up.sum})
	if err != nil {
		return "", s.mapCallErr(err, p)
	}
	// Upload what is missing.
	if ok.Have < up.size {
		s.noteStep(cj, remote.StepUpload)
		rep.emit(remote.StepUpload, "", float64(ok.Have)/float64(up.size), pConnect, "")
		if err := s.put(ctx, c, platform, build, job.ID, up, ok.Have, rep); err != nil {
			return "", err
		}
	}
	rep.emit(remote.StepUpload, "", 1, pUpload, "")
	// Run.
	params, err := json.Marshal(job.Params)
	if err != nil {
		return "", err
	}
	events := ctl.subscribe(job.ID)
	defer ctl.unsubscribe(job.ID)
	if _, err := ctl.call(ctx, remote.Request{Op: remote.OpRun, Job: job.ID, Engine: job.Engine, Kit: kitHash, Params: params}); err != nil {
		return "", s.mapCallErr(err, p)
	}
	s.noteStep(cj, remote.StepTranscribe)
	rep.runAt = time.Now()
	rep.emit(remote.StepTranscribe, remote.StageLoadModel, 0, pUpload, "")
	var done remote.AgentEvent
wait:
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ctl.dead:
			return "", wrapNet(errLost, ctl.Err())
		case ev := <-events:
			switch ev.Type {
			case remote.EvProgress:
				f := ev.Fraction
				cj.Progress = &f
				rep.emit(remote.StepTranscribe, remote.StageTranscribe, f, pUpload+(pRun-pUpload)*f, "")
			case remote.EvStatus:
				rep.emit(remote.StepTranscribe, remote.StageLoadModel, -1, pUpload, "")
			case remote.EvError:
				s.deleteRemote(ctl, job.ID)
				if ev.Code == remote.AgentInterrupted {
					return "", errLost
				}
				return "", agentRunError(label, ev)
			case remote.EvDone:
				done = ev
				break wait
			}
		}
	}
	// Download and check.
	s.noteStep(cj, remote.StepDownload)
	rep.emit(remote.StepDownload, "", 0, pRun, "")
	raw, err := s.get(ctx, c, platform, build, job.ID)
	if err != nil {
		if errors.Is(err, errLost) || ctx.Err() != nil {
			return "", err
		}
		s.deleteRemote(ctl, job.ID)
		return "", fallback(remote.CodeDownloadFailed, p, err)
	}
	rep.emit(remote.StepVerify, "", 0, pFetch, "")
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != done.Bytes || hex.EncodeToString(sum[:]) != done.SHA256 {
		s.deleteRemote(ctl, job.ID)
		return "", fallback(remote.CodeResultInvalid, map[string]any{"device": label, "reason": "hash"}, errors.New("result.json differs from what the agent reported"))
	}
	clean, _, err := remote.ValidateResult(raw, up.ms)
	if err != nil {
		s.deleteRemote(ctl, job.ID)
		return "", fallback(remote.CodeResultInvalid, map[string]any{"device": label, "reason": "content"}, err)
	}
	out := filepath.Join(job.OutDir, "result.json")
	if err := writeAtomic(out, clean); err != nil {
		return "", err
	}
	if !s.deleteRemoteSync(ctx, ctl, job.ID) {
		slog.Warn("ssh server: the job's files could not be deleted now; the agent removes them when they expire",
			"device", s.devID, "job", job.ID, "code", remote.CodeCleanupIncomplete)
	}
	rep.emit(remote.StepVerify, "", 1, 1, "")
	return out, nil
}

func (s *Server) noteStep(cj *remote.CardJob, step string) {
	s.mu.Lock()
	cj.Stage = step
	s.mu.Unlock()
	s.m.changed()
}

// mapCallErr classifies a control call's failure.
func (s *Server) mapCallErr(err error, p map[string]any) error {
	switch failCode(err) {
	case "":
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return wrapNet(errLost, err)
	case remote.AgentNoSpace:
		var f *agentFail
		errors.As(err, &f)
		p["free_mb"], p["need_mb"] = f.freeMB, f.needMB
		return fallback(remote.CodeNoSpace, p, err)
	case remote.AgentKitMissing:
		s.setState(StateSetup, errSetup)
		return busy(remote.CodeSetupNeeded, p, model.WaitRemoteSetup, err)
	case remote.AgentBusy:
		return busy(remote.CodeBusy, p, "", err)
	}
	return fallback(remote.CodeProtocol, map[string]any{"device": p["device"], "reason": failCode(err)}, err)
}

// put uploads up from offset (resumed at the agent's "have" after a failure).
func (s *Server) put(ctx context.Context, c *conn, platform string, b agentBuild, job string, up upload, offset int64, rep *reporter) error {
	p := map[string]any{"device": s.label()}
	for try := 0; try < 3; try++ {
		f, err := os.Open(up.path)
		if err != nil {
			return remoteErr(remote.ErrLocal, remote.CodeAudioUnreadable, nil, "", err)
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		r := &countingReader{r: f, n: offset, total: up.size, report: func(f float64) {
			rep.emit(remote.StepUpload, "", f, pConnect+(pUpload-pConnect)*f, "")
		}}
		_, err = agentEvent(ctx, c, platform, b, r, "put", "--job", job, "--kind", remote.FileAudio, "--offset", fmt.Sprint(offset))
		f.Close()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch failCode(err) {
		case remote.AgentNoSpace:
			var af *agentFail
			errors.As(err, &af)
			p["free_mb"], p["need_mb"] = af.freeMB, af.needMB
			return fallback(remote.CodeNoSpace, p, err)
		case remote.AgentAudioMismatch:
			offset = 0 // the agent removed the file
			continue
		}
		if !c.alive() {
			return errLost
		}
		st, serr := agentEvent(ctx, c, platform, b, nil, "stat", "--job", job, "--kind", remote.FileAudio)
		if serr != nil {
			if !c.alive() {
				return errLost
			}
			return fallback(remote.CodeUploadFailed, p, err)
		}
		if st.Complete {
			return nil
		}
		offset = st.Have
	}
	return fallback(remote.CodeUploadFailed, p, errors.New("the upload failed three times"))
}

// get downloads result.json.
func (s *Server) get(ctx context.Context, c *conn, platform string, b agentBuild, job string) ([]byte, error) {
	cmd, err := agentCommand(platform, b.id(), "get", "--job", job, "--kind", remote.FileResult)
	if err != nil {
		return nil, err
	}
	out, _, err := c.output(ctx, cmd, nil, remote.MaxResultBytes)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// deleteRemote deletes the job directory in the background.
func (s *Server) deleteRemote(ctl *control, job string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.deleteRemoteSync(ctx, ctl, job)
	}()
}

// deleteRemoteSync deletes the job directory (three tries).
func (s *Server) deleteRemoteSync(ctx context.Context, ctl *control, job string) bool {
	for try := 0; try < 3; try++ {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, err := ctl.call(cctx, remote.Request{Op: remote.OpDelete, Job: job})
		cancel()
		if err == nil {
			return true
		}
		if !ctl.alive() {
			return false
		}
	}
	return false
}

// abandon is a cancelled lecture: the remote run is cancelled and its
// directory deleted in the background (bounded), so the caller returns at once.
func (s *Server) abandon(job string, ctl *control) {
	if ctl == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = ctl.call(ctx, remote.Request{Op: remote.OpCancel, Job: job})
		s.deleteRemoteSync(ctx, ctl, job)
	}()
}

type countingReader struct {
	r        io.Reader
	n, total int64
	report   func(float64)
	last     time.Time
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.total > 0 && time.Since(c.last) > 500*time.Millisecond {
		c.last = time.Now()
		c.report(min(float64(c.n)/float64(c.total), 1))
	}
	return n, err
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeAtomic writes path through path.tmp and a rename.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// kitReady makes sure the kit is installed on the server: the session's
// kit, one listed by the last probe, or (once the user installed the
// environment) installed now; else the lecture waits with remote_setup.
func (s *Server) kitReady(ctx context.Context, c *conn, platform string, b agentBuild, kit remote.KitSpec) (string, error) {
	hash := kit.Hash()
	s.mu.Lock()
	have := s.rec.KitHash == hash || slices.Contains(s.probe.Kits, hash)
	auto := s.set.Installed
	s.mu.Unlock()
	if !have {
		if !auto {
			return "", busy(remote.CodeSetupNeeded, map[string]any{"device": s.label()}, model.WaitRemoteSetup, errSetup)
		}
		if err := s.ensure(ctx, c, platform, b, kit); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if !c.alive() {
				return "", errLost
			}
			return "", fallback(remote.CodeInstallFailed, map[string]any{"device": s.label(), "step": "kit"}, err)
		}
		s.setState(StateReady, nil)
		s.mu.Lock()
		s.probe.Kits = append(s.probe.Kits, hash)
		s.mu.Unlock()
	}
	return hash, nil
}
