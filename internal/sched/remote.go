// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// SPEC-v3 V14.3–V14.5: the Colab remote transcription backend inside the
// scheduler (v14-colab-integrate; .claude/v3-brief.md §8; NOTES "决定（v3
// v14-colab-integrate）").
//
//   - colab:remote is a row of the device table while Colab is enabled
//     (kv model.KVColabEnabled) and a credential is stored (Options.OnRemote
//     keeps the registry's presence in step); it is never part of the
//     power-aware policy's input (devices.Place, View, SelfTest,
//     OnPowerChange): it uses neither the machine's power nor its cooling,
//     so the single-active-device rule and multi_device do not apply to it
//     (V14.4) and a run on it is never migrated or suspended.
//   - Automatic dispatch ("overflow"): a queued asr job the local devices
//     cannot take now goes to colab:remote when the job's snapshot is auto,
//     it was not suspended by the power-aware policy (stats.asr_resume: a
//     suspended local CUDA job is never handed to Colab, V14.4), Colab never
//     handed it back before (stats.remote_fallback), the manual device list
//     (if any) names colab:remote, the global pause is off, and — on battery
//     power — battery_prefer is on. Colab.CanDispatch decides last; its
//     waiting_for (model.WaitRemote*) becomes the job's when it says no.
//   - A job pinned with "改用 Colab 处理" (stats.remote_pin, UseRemote) or
//     with a fixed colab:remote snapshot runs on Colab only.
//   - Colab hands a lecture back (colab.IsFallback, colab.IsRequeue): the
//     job is requeued without using up a retry (stats.remote_requeues), its
//     pin is dropped and it goes to the local devices (an auto snapshot is
//     bound to a local device at its next start). Without a local device
//     it waits with remote_fallback and the stage offers "改用 Colab 处理"
//     (colabfix). Colab only busy (colab.IsBusy: its runtime was being
//     released or started) is no hand-back: requeued as it was.
//   - The lectures that wait and could go to Colab are its planned queue
//     (SetQueue); when none is left and no run on Colab exists,
//     QueueDrained releases the runtime. A job that waits for a busy Colab
//     and could not go to a local device either says remote_busy.
//   - "处理完当前这节后关机" / "立即关机" (ShutdownRemote) hold the automatic
//     dispatch until no such lecture is left, so the next scan does not
//     borrow a new runtime at once. A held lecture no local device can take
//     says remote_held and the stage offers "改用 Colab 处理" (UseRemote
//     ends the hold: the user asked for Colab explicitly; colabfix2).
//   - Batches (colab-batch; a backend that is a RemoteBatcher): the
//     scheduler tells Colab its plan (SetPlan: the lectures running there
//     and those waiting, which of them may go up ahead of their turn). With
//     local devices that Colab only relieves, Colab holds at most
//     remoteOffloadHold lectures ("1 + 1"): the first lecture that cannot
//     go to a local device is dispatched, the next one is kept for Colab
//     (reserved: no local device takes it any more) and uploaded ahead;
//     once Colab is free again a local device gets the first try ("续杯时本
//     机优先"). Without a local device every waiting lecture may go ahead
//     (the backend limits that by tier, window, balance and memory). A
//     lecture whose audio is on Colab already (Held) or that is reserved
//     runs on Colab only and goes before the others; Slots of them at once
//     (two on the larger GPUs). The runtime is not released while lectures
//     are being prepared (prep) or downloaded (Options.RemoteSoon) that
//     would go to Colab, for at most RemoteImminentMax after its queue
//     emptied.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/state"
	"lectureflow/internal/wav"
)

// RemoteBackend is the Colab backend as the scheduler and the asr stage see
// it (*colab.Backend implements it; tests fake it).
type RemoteBackend interface {
	// CanDispatch: may a lecture of lectureSec go to Colab now; if not, the
	// waiting_for (model.WaitRemote*, "" = busy or cannot be asked).
	CanDispatch(ctx context.Context, lectureSec float64) (bool, string)
	SetQueue(audioSecs []float64)
	QueueDrained(ctx context.Context) error
	ShutdownAfterCurrent(ctx context.Context) error
	ShutdownNow(ctx context.Context) error
	Settings() colab.Settings
	Credential(ctx context.Context) colab.CredentialInfo
	Status(ctx context.Context) colab.Status
	Transcribe(ctx context.Context, job colab.Job, progress func(colab.Event)) (string, error)
}

// RemoteBatcher is a RemoteBackend that runs batches (colab-batch;
// *colab.Backend): several lectures at once, the next ones uploaded ahead
// of their turn. Without it the scheduler sends one lecture at a time and
// nothing ahead, as before.
type RemoteBatcher interface {
	// SetPlan replaces the plan: the lectures running on Colab
	// (Running) and those waiting that could go there, in dispatch order;
	// Upload marks those whose audio may go up ahead of their turn. A
	// lecture that leaves the plan has its uploaded files deleted.
	SetPlan(plan []colab.PlanEntry)
	// Held returns the job ids whose audio is on Colab (or on its way)
	// ahead of their turn.
	Held() map[string]bool
	// Slots is how many lectures may run on Colab at once.
	Slots() int
}

// remoteOffloadHold: with local devices, Colab holds at most this many
// lectures — one transcribing, one uploaded ahead (user decision
// 2026-10-27 04:30).
const remoteOffloadHold = 2

// RemotePrefs are the scheduler's Colab switches (kv model.KVColabEnabled,
// model.KVColabBatteryPrefer).
type RemotePrefs struct {
	Enabled       bool `json:"enabled"`
	BatteryPrefer bool `json:"battery_prefer"`
}

// ReadRemotePrefs reads the switches (missing = off).
func ReadRemotePrefs(ctx context.Context, st db.Store) (RemotePrefs, error) {
	var p RemotePrefs
	v, ok, err := st.GetKV(ctx, model.KVColabEnabled)
	if err != nil {
		return p, err
	}
	p.Enabled = model.ParseKVFlag(v, ok)
	v, ok, err = st.GetKV(ctx, model.KVColabBatteryPrefer)
	if err != nil {
		return p, err
	}
	p.BatteryPrefer = model.ParseKVFlag(v, ok)
	return p, nil
}

// WriteRemotePrefs stores the switches.
func WriteRemotePrefs(ctx context.Context, st db.Store, p RemotePrefs) error {
	return st.Tx(ctx, func(tx db.Store) error {
		if err := tx.SetKV(ctx, model.KVColabEnabled, model.KVFlag(p.Enabled)); err != nil {
			return err
		}
		return tx.SetKV(ctx, model.KVColabBatteryPrefer, model.KVFlag(p.BatteryPrefer))
	})
}

// ErrRemoteFallback: the lecture cannot run on Colab after all and goes
// back to the local devices (the asr stage wraps it when it has no backend
// to call; Colab's own errors say so through colab.IsFallback).
var ErrRemoteFallback = errors.New("remote transcription is not available: back to the local devices")

// remoteHandBack reports whether a run's error hands the lecture back to
// the local devices (requeue, not a failure), or — remoteBusy — just puts
// it back in the queue.
func remoteHandBack(err error) bool {
	return err != nil && (errors.Is(err, ErrRemoteFallback) || colab.IsFallback(err) || colab.IsRequeue(err) || colab.IsBusy(err) ||
		remote.IsFallback(err) || remote.IsBusy(err)) // ssh-mvp: the SSH servers' classes
}

// remoteBusy reports whether Colab could not take the lecture yet (the
// runtime was being released or started; colabfix): requeued without a
// hand-back (no stats.remote_fallback, the pin and a fixed colab snapshot
// stay), and no lecture goes to Colab for Options.RemoteBusyPause.
func remoteBusy(err error) bool { return err != nil && (colab.IsBusy(err) || remote.IsBusy(err)) }

// localUnavailable reports whether the local devices' reason says that no
// local device can take the job at all (none exists, is enabled or allowed
// by the power policy, or every one is paused; fresh-cuda: the only ones
// lack a component that no task installs), as opposed to being busy (a
// component being installed counts as busy: the device takes jobs soon).
func localUnavailable(reason string) bool {
	switch reason {
	case model.WaitNoASRDevice, model.WaitPowerNoDevice, model.WaitPowerNoSaverDevice:
		return true
	}
	return strings.HasPrefix(reason, model.WaitDevicePausedPrefix) ||
		strings.HasPrefix(reason, devices.WaitComponentMissingPrefix) || strings.HasPrefix(reason, devices.WaitModelMissingPrefix)
}

// remoteBusyReason is the waiting_for of a job that could go to Colab
// while Colab is busy: WaitRemoteBusy when no local device can take it
// (colabfix: only Colab transcribes, the local reason would be misleading),
// else the local reason (whichever frees first takes it).
func remoteBusyReason(local string) string {
	if localUnavailable(local) {
		return model.WaitRemoteBusy
	}
	return local
}

// remoteHeldReason is the waiting_for of a job that could go to Colab
// while a shutdown holds the automatic dispatch (colabfix2):
// WaitRemoteHeld when no local device can take it — it would wait until
// the user acts; the stage offers "改用 Colab 处理", which ends the hold —
// else the local reason.
func remoteHeldReason(local string) string {
	if localUnavailable(local) {
		return model.WaitRemoteHeld
	}
	return local
}

type remoteKey struct{}

// WithRemote attaches the remote backend to a run's context (the scheduler
// does this for every run; tests of the asr stage do it themselves).
func WithRemote(ctx context.Context, b RemoteBackend) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, remoteKey{}, b)
}

// RemoteFrom returns the remote backend of a run's context (nil = none).
func RemoteFrom(ctx context.Context) RemoteBackend {
	b, _ := ctx.Value(remoteKey{}).(RemoteBackend)
	return b
}

// RemoteLabel is the label of the colab:remote row.
func RemoteLabel(accelerator string) string {
	if accelerator == "" {
		return "Google Colab"
	}
	return "Google Colab · " + accelerator
}

// RemoteRow is the colab:remote row the scheduler binds and reports: model
// and precision are Colab's settings (the remote worker runs them).
func RemoteRow(set colab.Settings) model.ASRDevice {
	return model.ASRDevice{ID: model.DeviceColab, Backend: model.BackendColab, Label: RemoteLabel(set.Accelerator),
		Enabled: true, Model: set.Model, Precision: set.ComputeType, MaxParallel: colab.SlotsFor(set),
		Priority: model.DefaultDevicePriority(model.DeviceColab)}
}

// remoteScan is the Colab state one scan decides on.
type remoteScan struct {
	configured bool // Options.Remote and Options.Devices are set
	prefs      RemotePrefs
	authOK     bool // a credential is stored and its lifecycle allows it
	rowEnabled bool // the colab:remote row's own switch (no row yet: on)
	set        colab.Settings
}

// present: colab:remote is in the device table.
func (r remoteScan) present() bool { return r.configured && r.prefs.Enabled && r.authOK }

// usable: lectures may be sent to it.
func (r remoteScan) usable() bool { return r.present() && r.rowEnabled }

// remoteState is the scheduler's own Colab bookkeeping (guarded by
// Scheduler.mu).
type remoteState struct {
	hold     bool      // ShutdownRemote: no automatic dispatch until the planned queue is empty
	queue    []float64 // the last SetQueue
	planned  bool      // the last scan had eligible lectures
	runEnded bool      // a colab run ended since the last scan
	reported bool      // OnRemote was called once
	lastPres bool
	lastRow  model.ASRDevice
	draining bool // a QueueDrained call runs
	// busyUntil: Colab answered "busy" (remoteBusy); no lecture is sent to
	// it before this time.
	busyUntil time.Time
	durations map[string]audioLen

	// colab-batch: reserved are the queued lectures kept for Colab (the
	// "+1" with local devices); colabOnly: the last lecture planned for
	// Colab had no local device to go to (or was pinned); emptySince: when
	// the planned queue last became empty (the imminent-work hold);
	// lastPlan: the last SetPlan.
	reserved   map[string]bool
	colabOnly  bool
	emptySince time.Time
	lastPlan   []colab.PlanEntry
	// drainHeld: a drain was put off for work about to arrive; it comes
	// once that ends (or the limit passes).
	drainHeld bool

	// ssh-mvp (remote_devices.go): the SSH servers (SetSSH; nil = none).
	ssh SSHFleet
}

type audioLen struct {
	size int64
	mod  time.Time
	sec  float64
}

// readRemote reads the Colab state of a scan and keeps the registry's
// presence of colab:remote in step (Options.OnRemote). Never with s.mu held.
func (s *Scheduler) readRemote(ctx context.Context) remoteScan {
	var r remoteScan
	if s.remote == nil || s.devs == nil {
		return r
	}
	r.configured = true
	p, err := ReadRemotePrefs(ctx, s.st)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("colab: reading the switches", "err", err)
		}
		return remoteScan{}
	}
	r.prefs = p
	r.authOK = s.remote.Credential(ctx).Present
	r.set = s.remote.Settings()
	r.rowEnabled = true
	if d, ok := s.devs.Get(model.DeviceColab); ok {
		r.rowEnabled = d.Enabled
	}
	s.reportRemote(ctx, r)
	return r
}

// reportRemote calls Options.OnRemote when the presence or the row's
// label, model or precision changed.
func (s *Scheduler) reportRemote(ctx context.Context, r remoteScan) {
	if s.onRemote == nil {
		return
	}
	row := RemoteRow(r.set)
	s.mu.Lock()
	same := s.rem.reported && s.rem.lastPres == r.present() && s.rem.lastRow.Label == row.Label &&
		s.rem.lastRow.Model == row.Model && s.rem.lastRow.Precision == row.Precision
	s.rem.reported, s.rem.lastPres, s.rem.lastRow = true, r.present(), row
	s.mu.Unlock()
	if !same {
		s.onRemote(ctx, r.present(), row)
	}
}

// RefreshRemote re-reads the Colab state now (after the switches or the
// credential changed), updates the device table and kicks a scan.
func (s *Scheduler) RefreshRemote(ctx context.Context) {
	s.readRemote(ctx)
	s.ev.Changed("devices", "")
	s.Kick()
}

// Remote returns Options.Remote (nil = none).
func (s *Scheduler) Remote() RemoteBackend { return s.remote }

// RemotePrefs reads the switches.
func (s *Scheduler) RemotePrefs(ctx context.Context) (RemotePrefs, error) {
	return ReadRemotePrefs(ctx, s.st)
}

// SetRemotePrefs stores the switches; turning Colab on again ends a
// shutdown's hold. The device table follows and a scan runs.
func (s *Scheduler) SetRemotePrefs(ctx context.Context, p RemotePrefs) error {
	old, err := ReadRemotePrefs(ctx, s.st)
	if err != nil {
		return err
	}
	if err := WriteRemotePrefs(ctx, s.st, p); err != nil {
		return err
	}
	if p.Enabled && !old.Enabled {
		s.mu.Lock()
		s.rem.hold = false
		s.mu.Unlock()
	}
	s.RefreshRemote(ctx)
	return nil
}

// RemoteHeld reports whether a shutdown holds the automatic dispatch.
func (s *Scheduler) RemoteHeld() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rem.hold
}

// remoteRunningLocked reports whether a run uses colab:remote (caller holds s.mu).
func (s *Scheduler) remoteRunningLocked() bool {
	return s.pipelineOnLocked(model.DeviceColab) > 0
}

// isRemote: a remote device id (Colab or an SSH server; ssh-mvp): outside
// the power-aware policy, never a local worker. Colab's own bookkeeping
// uses isColab.
func isRemote(id string) bool { return model.IsRemoteDevice(id) }

// remotePinned: the job may run on colab:remote only (a "改用 Colab 处理"
// pin, or a fixed colab:remote snapshot).
func remotePinned(j model.Job) bool {
	if j.Stats.RemotePin {
		return true
	}
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	return ok && !snap.Auto && isColab(snap.DeviceID)
}

// remoteOnlyAdmit decides a pinned job: Colab or wait.
func (s *Scheduler) remoteOnlyAdmit(ctx context.Context, sc *scanState, r *run) string {
	s.scanSettings(sc)
	rs := sc.remote
	switch {
	case sc.pausedAll:
		return model.WaitGPUPaused
	case !rs.present() && rs.configured && rs.prefs.Enabled && !rs.authOK:
		return model.WaitRemoteAuth
	case !rs.usable():
		return model.WaitNoASRDevice
	case sc.devPaused[model.DeviceColab]:
		return model.WaitDevicePaused(model.DeviceColab)
	}
	sc.remoteQueue = append(sc.remoteQueue, s.audioSec(ctx, r.job))
	sc.remoteEligible++
	if sc.remoteHold {
		// Only a fixed colab:remote snapshot gets here (a shutdown drops
		// the pins, UseRemote ends the hold): it runs on Colab only, so it
		// says remote_held like a job without a local device (colabfix2).
		return model.WaitRemoteHeld
	}
	// colab-batch: on Colab only (pinned, or its audio is there already):
	// it may go up ahead of its turn.
	s.planEntry(ctx, sc, r.job, true)
	s.mu.Lock()
	s.rem.colabOnly = true
	s.mu.Unlock()
	return s.remoteAdmit(ctx, sc, r, model.WaitRemoteBusy)
}

// remoteScanStart notes, at the start of a scan, which queued asr jobs
// are kept for Colab (colab-batch): their audio is on Colab already
// (Held) or they are reserved, and Colab can take lectures now. They run
// on Colab only, before the others.
func (s *Scheduler) remoteScanStart(sc *scanState, jobs []model.Job) {
	if s.batch == nil || !sc.remote.usable() || sc.remoteHold {
		return
	}
	sc.remoteHeld = s.batch.Held()
	s.mu.Lock()
	reserved := maps.Clone(s.rem.reserved)
	s.mu.Unlock()
	sc.remoteKeptJobs = map[string]bool{}
	for _, j := range jobs {
		// pending ones too: they may become queued in this scan (a kept
		// job skipped by the scan gives its place back, scan())
		if j.Stage != model.StageASR {
			continue
		}
		if sc.remoteHeld[j.ID] || reserved[j.ID] {
			sc.remoteKeptJobs[j.ID] = true
			sc.remoteKeptLeft++
		}
	}
}

// planEntry adds a waiting job to this scan's plan for Colab.
func (s *Scheduler) planEntry(ctx context.Context, sc *scanState, j model.Job, upload bool) {
	if s.batch == nil {
		return
	}
	sc.remotePlan = append(sc.remotePlan, colab.PlanEntry{Key: j.ID, AudioPath: s.audioPath(ctx, j),
		AudioSec: s.audioSec(ctx, j), Upload: upload})
}

// remoteOverflow is the automatic dispatch of a job the local devices did
// not take (local = their reason): Colab when every condition of the
// package comment holds and Colab can take it, else local (or Colab's
// waiting_for).
func (s *Scheduler) remoteOverflow(ctx context.Context, sc *scanState, r *run, local string) string {
	rs := sc.remote
	if !rs.usable() || local == model.WaitGPUPaused {
		return local
	}
	j := r.job
	if j.Stats.RemoteFallback != nil && j.Stats.ASRResume == nil && localUnavailable(local) {
		// colabfix: Colab handed it back and no local device can take it:
		// it would wait for good; the stage offers "改用 Colab 处理".
		return model.WaitRemoteFallback
	}
	if j.Stats.ASRResume != nil || j.Stats.RemoteFallback != nil {
		return local // suspended by the power policy (V14.4), or Colab handed it back before
	}
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	if !ok || !snap.Auto {
		return local // a fixed snapshot runs on its own device only
	}
	s.scanSettings(sc)
	if sc.devPaused[model.DeviceColab] {
		return local // its own pause switch
	}
	if sc.asr.DeviceMode == model.ASRDeviceModeManual && !slices.Contains(sc.asr.ManualDevices, model.DeviceColab) {
		return local // "只用某台设备": Colab only when the list names it
	}
	if sc.power.OnBatteryPower() && !rs.prefs.BatteryPrefer {
		return local
	}
	sc.remoteQueue = append(sc.remoteQueue, s.audioSec(ctx, j))
	sc.remoteEligible++
	if sc.remoteHold {
		return remoteHeldReason(local)
	}
	only := localUnavailable(local)
	s.mu.Lock()
	s.rem.colabOnly = only
	s.mu.Unlock()
	reason := s.remoteAdmit(ctx, sc, r, remoteBusyReason(local))
	if reason == "" || s.batch == nil {
		return reason
	}
	// colab-batch: not dispatched now. Without a local device that could
	// take it, it may go up ahead of its turn; with local devices Colab
	// keeps it only as the "+1" (reserved), and only once its runtime is
	// asked for.
	upload := only
	if !only && reason != model.WaitRemoteWindow && reason != model.WaitRemoteBalance && reason != model.WaitRemoteAuth {
		s.mu.Lock()
		// "1 + 1": one transcribing on Colab, one waiting there.
		onColab := s.pipelineOnLocked(model.DeviceColab)
		inflight := onColab + len(s.rem.reserved) + len(sc.remoteHeld)
		if onColab > 0 && inflight < remoteOffloadHold {
			if s.rem.reserved == nil {
				s.rem.reserved = map[string]bool{}
			}
			s.rem.reserved[j.ID] = true
			upload = true
			slog.Info("asr job kept for Colab (uploaded ahead while the current lecture runs there)", "job", j.ID)
		}
		s.mu.Unlock()
	}
	s.planEntry(ctx, sc, j, upload)
	return reason
}

// remoteDispatchTimeout bounds CanDispatch inside a scan (the paid tier
// reads the balance from Colab at most once a minute).
const remoteDispatchTimeout = 10 * time.Second

// remoteAdmit asks Colab and, when it can take the job, binds the run to
// colab:remote (busy: the reason the job keeps).
func (s *Scheduler) remoteAdmit(ctx context.Context, sc *scanState, r *run, busy string) string {
	slots := 1
	if s.batch != nil {
		slots = max(1, s.batch.Slots())
	}
	kept := sc.remoteKeptJobs[r.job.ID]
	s.mu.Lock()
	// Slots lectures at a time (one, or two on the larger GPUs;
	// colab-batch); none while QueueDrained runs (the runtime is being
	// released: a lecture sent now would find it stopping) or shortly after
	// Colab answered busy (colabfix). The lectures kept for Colab (their
	// audio is there already) go first.
	running := max(s.pipelineOnLocked(model.DeviceColab), sc.remoteDispatched)
	free := slots - running
	if !kept {
		free -= sc.remoteKeptLeft
	}
	taken := free <= 0 || s.rem.draining || s.now().Before(s.rem.busyUntil)
	s.mu.Unlock()
	if taken {
		return busy
	}
	sec := sc.remoteQueue[len(sc.remoteQueue)-1]
	cctx, cancel := context.WithTimeout(ctx, remoteDispatchTimeout)
	ok, wf := s.remote.CanDispatch(cctx, sec)
	cancel()
	if !ok {
		if wf != "" {
			return wf
		}
		return busy
	}
	// Dispatched: not part of the planned queue any more.
	sc.remoteQueue = sc.remoteQueue[:len(sc.remoteQueue)-1]
	sc.remoteEligible--
	sc.remoteDispatched++
	if kept {
		sc.remoteKeptLeft--
	}
	s.mu.Lock()
	delete(s.rem.reserved, r.job.ID)
	s.mu.Unlock()
	snap, snapOK := model.ParseASRSnapshot(r.job.ConfigSnapshot)
	r.device, r.devRow = model.DeviceColab, RemoteRow(sc.remote.set)
	r.bind = snapOK && snap.Auto
	r.fixed = !r.bind
	slog.Info("asr job goes to Colab", "job", r.job.ID, "lecture", r.job.ScopeID, "pinned", r.job.Stats.RemotePin)
	return ""
}

// audioPath is the job's lecture audio.wav ("" when the lecture is gone).
// It is read (audioSec) and uploaded ahead to Colab (planEntry) before any
// run, so before the start check (checkScopeIDs): a lecture whose directory
// is not one of its own — an id that names another folder, a link on the
// way (state.LectureFilesOK) — has none either (SEC49 L2).
func (s *Scheduler) audioPath(ctx context.Context, j model.Job) string {
	l, err := s.st.GetLecture(ctx, j.ScopeID)
	if err != nil || !state.LectureFilesOK(s.dataDir, l) {
		return ""
	}
	return model.LectureDir(s.dataDir, l.CourseID, l.ID) + string(os.PathSeparator) + model.AudioFile
}

// audioSec is the length of the job's lecture audio.wav in seconds (0 when
// unknown), cached by size and modification time.
func (s *Scheduler) audioSec(ctx context.Context, j model.Job) float64 {
	path := s.audioPath(ctx, j)
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	s.mu.Lock()
	c, ok := s.rem.durations[path]
	s.mu.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c.sec
	}
	ms, ok := wav.DurationMS(path)
	if !ok {
		return 0
	}
	sec := float64(ms) / 1000
	s.mu.Lock()
	if s.rem.durations == nil {
		s.rem.durations = map[string]audioLen{}
	}
	s.rem.durations[path] = audioLen{size: fi.Size(), mod: fi.ModTime(), sec: sec}
	s.mu.Unlock()
	return sec
}

// endRemoteScan tells Colab its planned queue (SetQueue) and, when none is
// left, that the queue is drained (once; the runtime is released). A
// shutdown's hold ends with the queue.
//
// colabfix (accept2): never while a run on colab:remote exists — also one
// this scan has just started, whose asr stage has not reached Transcribe
// yet: the backend would release the runtime under it and the lecture
// would find it stopping. The drain waits for that run's end (runEnded).
func (s *Scheduler) endRemoteScan(ctx context.Context, sc *scanState) {
	if s.remote == nil || !sc.remote.configured {
		return
	}
	q := sc.remoteQueue
	if sc.remoteHold || !sc.remote.usable() {
		q = nil // nothing is planned for Colab while held or unusable
	}
	plan := s.remotePlanOf(sc, len(q) > 0)
	s.mu.Lock()
	prevPlanned, ended := s.rem.planned, s.rem.runEnded
	s.rem.runEnded = false
	s.rem.planned = len(q) > 0
	changed := !slices.Equal(q, s.rem.queue)
	s.rem.queue = slices.Clone(q)
	planChanged := s.batch != nil && !slices.Equal(plan, s.rem.lastPlan)
	s.rem.lastPlan = plan
	// A run this scan started is in s.running already (start registers it
	// before its goroutine); one whose start failed is not.
	running := s.remoteRunningLocked()
	if sc.remoteEligible == 0 && s.rem.hold && !running {
		s.rem.hold = false
		slog.Info("colab: no lecture is waiting any more; automatic dispatch to Colab allowed again")
	}
	if len(q) > 0 || running {
		s.rem.emptySince = time.Time{}
	} else if s.rem.emptySince.IsZero() {
		s.rem.emptySince = s.now()
	}
	if len(q) > 0 || running {
		s.rem.drainHeld = false
	}
	drain := len(q) == 0 && (prevPlanned || ended || s.rem.drainHeld) && !s.rem.draining && !running
	s.mu.Unlock()
	if drain && s.remoteImminent(ctx, sc) {
		drain = false // work is about to arrive: keep the runtime (colab-batch)
		s.mu.Lock()
		s.rem.drainHeld = true
		s.mu.Unlock()
	}
	if drain {
		s.mu.Lock()
		drain = !s.rem.draining
		s.rem.draining, s.rem.drainHeld = true, false
		s.mu.Unlock()
	}
	if changed {
		s.remote.SetQueue(q)
	}
	if planChanged {
		s.batch.SetPlan(plan)
	}
	if drain {
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), 2*time.Minute)
			defer cancel()
			if err := s.remote.QueueDrained(ctx); err != nil {
				slog.Info("colab: releasing the runtime after the queue drained failed (retried by the backend)", "code", colab.Code(err))
			}
			s.mu.Lock()
			s.rem.draining = false
			s.mu.Unlock()
		}()
	}
}

// ShutdownRemote is "处理完当前这节后关机" (now false) or "立即关机" (now
// true; the running lecture goes back to the queue): queued lectures are
// no longer pinned to Colab, and the automatic dispatch is held until no
// lecture that could go to Colab is left.
func (s *Scheduler) ShutdownRemote(ctx context.Context, now bool) error {
	if s.remote == nil {
		return ErrRemoteNotConfigured
	}
	s.mu.Lock()
	s.rem.hold = true
	s.rem.reserved = nil
	s.mu.Unlock()
	if err := s.unpinQueued(ctx); err != nil {
		slog.Warn("colab: unpinning the queued lectures", "err", err)
	}
	var err error
	if now {
		err = s.remote.ShutdownNow(ctx)
	} else {
		err = s.remote.ShutdownAfterCurrent(ctx)
	}
	s.ev.Changed("devices", "")
	s.Kick()
	return err
}

// unpinQueued drops the "改用 Colab 处理" pin of the queued and pending asr
// jobs (under the scan lock: none starts meanwhile).
func (s *Scheduler) unpinQueued(ctx context.Context) error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	jobs, err := s.st.ListCurrentJobsByStatus(ctx, model.StatusPending, model.StatusQueued)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Stage != model.StageASR || !j.Stats.RemotePin {
			continue
		}
		st := j.Stats
		st.RemotePin = false
		if err := s.st.SetJobStats(ctx, j.ID, st); err != nil {
			return err
		}
		s.emitJob(ctx, j)
	}
	return nil
}

// ErrRemoteNotConfigured: no Colab backend (HTTP 503).
var ErrRemoteNotConfigured = errors.New("the Colab backend is not configured")

// remoteRefusal is UseRemote's refusal (HTTP 409): CodeRemoteDisabled, or
// CodeRemoteUnavailable {waiting_for}.
func remoteRefusal(code string, waitingFor string) error {
	var params map[string]any
	if waitingFor != "" {
		params = map[string]any{"waiting_for": waitingFor}
	}
	return &model.CodedError{Info: model.ErrorInfo{Code: code, Params: params},
		Err: fmt.Errorf("%w: Colab cannot take the lecture (%s)", state.ErrConflict, code+" "+waitingFor)}
}

// UseRemote is V14.4 "改用 Colab 处理" for one lecture: the asr job starts
// over and runs on colab:remote. A queued job (also one the power policy
// suspended) or one running on a local device is cancelled first; a failed
// or cancelled one is retried. Refusals (HTTP 409): Colab is off or its row
// disabled (CodeRemoteDisabled), no usable credential or Colab cannot take
// a lecture now (CodeRemoteUnavailable {waiting_for}: remote_auth,
// remote_window, remote_balance), the job is pending, done or stale
// (state.ErrConflict). A job already pinned or running on Colab: nothing
// to do.
func (s *Scheduler) UseRemote(ctx context.Context, lectureID string) error {
	if s.remote == nil || s.devs == nil {
		return ErrRemoteNotConfigured
	}
	l, err := s.st.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	if state.IsDraft(l) {
		return state.ErrDraft
	}
	rs := s.readRemote(ctx)
	switch {
	case !rs.prefs.Enabled || !rs.rowEnabled:
		return remoteRefusal(CodeRemoteDisabled, "")
	case !rs.authOK:
		return remoteRefusal(CodeRemoteUnavailable, model.WaitRemoteAuth)
	}
	j, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageASR)
	if err != nil {
		return err
	}
	s.mu.Lock()
	cur := s.running[j.ID]
	onRemote := cur != nil && isColab(cur.device)
	s.mu.Unlock()
	if onRemote || (j.Status == model.StatusQueued && j.Stats.RemotePin) {
		return nil
	}
	switch j.Status {
	case model.StatusQueued, model.StatusRunning, model.StatusFailed, model.StatusCancelled:
	default:
		return fmt.Errorf("%w: asr is %s; 改用 Colab needs a queued, running, failed or cancelled transcription", state.ErrConflict, j.Status)
	}
	if !s.remoteRunning() {
		cctx, cancel := context.WithTimeout(ctx, remoteDispatchTimeout)
		ok, wf := s.remote.CanDispatch(cctx, s.audioSec(ctx, j))
		cancel()
		if !ok && wf != "" {
			return remoteRefusal(CodeRemoteUnavailable, wf)
		}
	}
	if j.Status == model.StatusQueued || j.Status == model.StatusRunning {
		if err := s.Cancel(j.ID); err != nil {
			return err
		}
	}
	lang := s.set.ASR().Language
	if snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot); ok {
		lang = snap.Language
	}
	raw := model.MarshalSnapshot(model.ASRDeviceSnapshot(RemoteRow(rs.set), lang, true))
	s.scanMu.Lock()
	err = state.RetryWith(ctx, s.st, s.dataDir, model.ScopeLecture, lectureID, model.StageASR,
		map[string]string{model.StageASR: raw})
	if err == nil {
		var nj model.Job
		if nj, err = s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageASR); err == nil {
			st := nj.Stats
			// From the start (V14.4): no checkpoint, no power event to show;
			// a transcription on Colab, not a "修复断句" run (textrepair-int:
			// the mode would only be ignored, lead ruling 05:50 item 3).
			st.RemotePin, st.RemoteFallback, st.ASRResume, st.PowerEvent = true, nil, nil, nil
			st.ASRMode = ""
			err = s.st.SetJobStats(ctx, nj.ID, st)
		}
	}
	if err == nil {
		s.mu.Lock()
		s.rem.hold = false // the user asked for Colab explicitly
		s.mu.Unlock()
	}
	s.scanMu.Unlock()
	if err != nil {
		return err
	}
	slog.Info("asr job pinned to Colab by the user", "lecture", lectureID)
	s.emitScope(ctx, model.ScopeLecture, lectureID)
	s.Kick()
	return nil
}

// remoteRunning reports whether a run uses colab:remote.
func (s *Scheduler) remoteRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remoteRunningLocked()
}

// remoteRequeue is finish's hand-back of a lecture Colab gave back
// (remoteHandBack): requeued like a power interrupt (no retry used up,
// job.error cleared), the pin dropped, and a fixed colab:remote snapshot
// replaced by the current auto snapshot so a local device can take it.
func (s *Scheduler) remoteRequeue(ctx context.Context, r *run, runErr error) {
	id := r.job.ID
	if model.IsSSHDevice(r.device) {
		s.sshRequeue(ctx, r, runErr)
		return
	}
	if remoteBusy(runErr) {
		// colabfix: Colab was not ready (its runtime was being released or
		// started): back to the queue as it was — pinned or fixed to Colab
		// it stays so, an auto job may go to Colab again — after a short
		// pause (CanDispatch says busy until the release has ended anyway).
		s.mu.Lock()
		s.rem.busyUntil = s.now().Add(s.remoteBusyPause)
		s.mu.Unlock()
		if err := state.PowerRequeue(ctx, s.st, id); err != nil {
			slog.Error("job requeue after Colab was busy", "job", id, "err", err)
			return
		}
		slog.Info("Colab was not ready for the lecture (its runtime was being released or started); back to the queue",
			"job", id, "lecture", r.scopeID)
		return
	}
	// colab-batch: a hand-back ends the lectures kept for Colab too; the
	// local devices may take them (their uploads go with the plan).
	s.mu.Lock()
	s.rem.reserved = nil
	s.mu.Unlock()
	if err := state.PowerRequeue(ctx, s.st, id); err != nil {
		slog.Error("job requeue after Colab handed it back", "job", id, "err", err)
		return
	}
	if snap, ok := model.ParseASRSnapshot(r.job.ConfigSnapshot); ok && !snap.Auto && isRemote(snap.DeviceID) {
		if cur, err := s.currentSnapshots([]string{model.StageASR}, false); err == nil && cur[model.StageASR] != "" {
			if err := s.st.SetJobSnapshot(ctx, id, cur[model.StageASR]); err != nil {
				slog.Warn("replacing the colab snapshot of a lecture Colab handed back", "job", id, "err", err)
			}
		}
	}
	slog.Info("Colab handed the lecture back; it goes to the local devices", "job", id, "lecture", r.scopeID,
		"code", colab.Code(runErr), "requeue", colab.IsRequeue(runErr))
}

// remoteErrorInfo is the stats.remote_fallback of a hand-back.
func remoteErrorInfo(err error) *model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) && ce.Info.Code != "" {
		info := ce.Info
		info.Detail = ""
		return &info
	}
	return &model.ErrorInfo{Code: colab.CodeUnavailable, Params: map[string]any{"endpoint": "local"}}
}

// remotePlanOf is the plan for Colab (colab-batch): the lectures running
// there, then the waiting ones of this scan in dispatch order (kept ones
// first). Reserved lectures that are no longer waiting are forgotten.
// Without lectures planned and nothing running it is empty.
func (s *Scheduler) remotePlanOf(sc *scanState, planned bool) []colab.PlanEntry {
	if s.batch == nil {
		return nil
	}
	s.mu.Lock()
	var plan []colab.PlanEntry
	for _, r := range s.running {
		if r.res == ResourceGPU && isColab(r.device) {
			plan = append(plan, colab.PlanEntry{Key: r.job.ID, Running: true})
		}
	}
	waiting := map[string]bool{}
	for _, e := range sc.remotePlan {
		waiting[e.Key] = true
	}
	for id := range s.rem.reserved {
		if !waiting[id] || !sc.remote.usable() || sc.remoteHold {
			delete(s.rem.reserved, id)
		}
	}
	s.mu.Unlock()
	slices.SortStableFunc(plan, func(a, b colab.PlanEntry) int { return strings.Compare(a.Key, b.Key) })
	if !planned && len(plan) == 0 {
		return nil
	}
	if sc.remoteHold || !sc.remote.usable() {
		return plan
	}
	kept := func(e colab.PlanEntry) bool { return sc.remoteKeptJobs[e.Key] }
	for _, e := range sc.remotePlan {
		if kept(e) {
			plan = append(plan, e)
		}
	}
	for _, e := range sc.remotePlan {
		if !kept(e) {
			plan = append(plan, e)
		}
	}
	return plan
}

// RemoteImminentMax: the longest the runtime is kept, after its queue
// emptied, for lectures still being prepared or downloaded (colab-batch:
// "设上限，例如这些任务的预计到达时间不超过几分钟").
const remoteImminentMax = 5 * time.Minute

// remoteImminent reports whether work that would go to Colab is about to
// arrive (colab-batch, user's principle of 2026-10-27 04:40): a lecture
// whose asr job waits for its prep, or a download that ends (and becomes
// a lecture) within the limit (Options.RemoteSoon), while no local device
// would take it (none is usable for the lectures planned so far, or the
// local devices are busy). Never longer than remoteImminentMax after the
// queue emptied.
func (s *Scheduler) remoteImminent(ctx context.Context, sc *scanState) bool {
	if !sc.remote.usable() || sc.remoteHold {
		return false
	}
	s.mu.Lock()
	since, only := s.rem.emptySince, s.rem.colabOnly
	localBusy := false
	for _, r := range s.running {
		if r.res == ResourceGPU && r.job.Stage == model.StageASR && r.device != "" && !isRemote(r.device) {
			localBusy = true
		}
	}
	s.mu.Unlock()
	if !since.IsZero() && s.now().Sub(since) >= s.remoteImminentMax {
		return false
	}
	if !only && !localBusy {
		return false
	}
	for _, j := range sc.remotePending {
		if remotePinned(j) || remoteEligibleAuto(j) {
			if p, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, j.ScopeID, model.StagePrep); err == nil &&
				(p.Status == model.StatusQueued || p.Status == model.StatusRunning) {
				slog.Info("colab: keeping the runtime for a lecture being prepared", "lecture", j.ScopeID)
				return true
			}
		}
	}
	if s.remoteSoon != nil {
		if eta := s.remoteSoon(ctx); eta >= 0 && eta <= s.remoteImminentMax {
			slog.Info("colab: keeping the runtime for a download about to become a lecture", "eta_s", int(eta.Seconds()))
			return true
		}
	}
	return false
}

// remoteEligibleAuto: an asr job Colab could take automatically (auto
// snapshot, never handed back, not suspended by the power policy).
func remoteEligibleAuto(j model.Job) bool {
	if j.Stats.RemoteFallback != nil || j.Stats.ASRResume != nil {
		return false
	}
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	return !ok || snap.Auto
}
