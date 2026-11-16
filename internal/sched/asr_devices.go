// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// SPEC-v2 I8 (multi-device scheduling) and I9 (device self-test). Types and
// signatures FROZEN by the lead for phase 3b (.claude/v3b-brief.md);
// bodies: task C. With Options.Devices nil the scheduler behaves exactly as
// before 3b (one GPU pool from the legacy ASR settings).
//
// Rules the implementation keeps (brief section "C"):
//   - one pool per device, max_parallel slots each; CUDA admitted by
//     nvidia-smi free VRAM (as before, the calibration counts only runs on
//     the same card as overlapping), Intel devices by available system
//     memory − mem_reserve_mb − the unreached estimates of their running
//     jobs ≥ devices.MemEstimateMB;
//   - auto mode binds the device when the job starts running, in the same
//     transaction as queued → running and RecordPromptVersion
//     (state.BindASRDevice; lead ruling 1), again on every retry and after
//     §4.7 recovery; a fixed snapshot runs only on its device;
//   - waiting_for: WaitDevicePaused(id), WaitMemory, WaitNoASRDevice,
//     WaitDeviceSelfTest(id), and the v1 gpu_paused (global switch),
//     gpu_slot, vram; since power management the power_* codes (power.go)
//     instead of WaitOnBattery;
//   - power management (v2-step20, power.go): the device rules are the
//     pure policy devices.Place (a desktop keeps the 3b rules below; a
//     machine with a battery has one active device, NPU → Intel GPU on
//     battery, CUDA → Intel GPU on AC, never a CPU device); battery_aware
//     is no longer read; the worker memory budget is checked on every
//     backend;
//   - multi_device off: at most one pipeline asr job runs over all devices
//     (self-tests do not count);
//   - a power state change triggers a scan at once and Events.Changed
//     ("devices", "");
//   - self-tests run in memory (no job row; lost on restart), occupy one
//     slot of their device, start only when no pipeline job runs on the
//     device or waits for it, yield between candidates
//     (DeviceTestEnv.BeforeCandidate), can be cancelled; the result is
//     written through DeviceSource.SetBenchmark when the test ends (done,
//     failed or cancelled with partial results);
//   - power management, backend Gate round 1: a self-test holding a slot
//     counts in the worker memory budget and takes its slot only within it
//     (fix 2); it is preempted when a pipeline job waits for its device
//     with no other candidate (3b), when its memory alone keeps a pipeline
//     job from starting (fix 2), and on a machine with a battery when a
//     pipeline job runs on another device (one active device, fix 1); its
//     state's WaitingFor names the active device it waits for (fix 5).
//
// Decisions of task C (NOTES.md "决定（3b C）"):
//   - The device-specific reasons are checked per candidate in this order:
//     self-test holding the device (device_selftest:{id}), multi_device
//     off with another job running (gpu_slot), slots full (gpu_slot), then
//     VRAM (CUDA) or memory (OpenVINO). gpu_paused, no_asr_device,
//     on_battery and device_paused:{id} are decided before (they leave no
//     candidate).
//   - "Waits for the device" (self-test start and yield) = a queued
//     pipeline asr job that did not start and has the device among its
//     candidates — also while in retry backoff — unless only the
//     multi_device rule holds it back.
//   - BatteryHold (power management): on battery, every enabled, present
//     device but the battery device.
//   - In fake ASR mode (SPEC §14) every device has one slot and no VRAM or
//     memory reading is taken.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// DeviceSource is the device registry as the scheduler sees it
// (devices.Registry implements it).
type DeviceSource interface {
	// List returns every row, priority DESC then id.
	List() []model.ASRDevice
	Get(id string) (model.ASRDevice, bool)
	// Present reports whether the last detection saw the device.
	Present(id string) bool
	// Detected reports whether the first detection has finished.
	Detected() bool
	// Usable is the A4.3 usability of an asr snapshot (device exists, is
	// enabled and present; auto: some enabled device is present).
	Usable(s model.ASRSnapshot) (bool, *model.ErrorInfo)
	// SetBenchmark stores a finished self-test.
	SetBenchmark(ctx context.Context, id string, b model.DeviceBenchmark) error
}

// Self-test states (DeviceTestState.State).
const (
	DeviceTestIdle      = "idle" // no test since the program started (never stored)
	DeviceTestWaiting   = "waiting"
	DeviceTestRunning   = "running"
	DeviceTestDone      = "done"
	DeviceTestFailed    = "failed"
	DeviceTestCancelled = "cancelled"
)

// Self-test phases (DeviceTestProgress.Phase, while running).
const (
	DeviceTestPhaseClip         = "clip"                        // cutting the clip from the lecture's audio
	DeviceTestPhasePreparing    = model.ASRStatusPreparingModel // the worker downloads / converts the candidate's model
	DeviceTestPhaseCompiling    = model.ASRStatusCompilingModel // first compile on this device
	DeviceTestPhaseTranscribing = "transcribing"
)

// DeviceTestProgress is one report of a running self-test.
type DeviceTestProgress struct {
	Phase     string
	Step      int // 1-based index of the candidate
	Steps     int // number of candidates
	Candidate devices.Candidate
	Progress  float64 // of the current candidate, 0..1
}

// DeviceTestEnv is what the scheduler hands to a self-test run.
type DeviceTestEnv struct {
	// Report records progress (never blocks, never touches the database).
	Report func(DeviceTestProgress)
	// BeforeCandidate is called before every candidate: it returns at once
	// when no pipeline job waits for the device; otherwise it gives the
	// slot back, waits until the device is free of pipeline work again and
	// takes the slot back (the test is "waiting" meanwhile). It returns
	// ctx.Err() when the test is cancelled.
	BeforeCandidate func(ctx context.Context) error
}

// DeviceTestFunc runs a self-test of dev (I9; implemented by
// stages.Deps.DeviceTest). It returns the benchmark to store — also when
// cancelled or failed part way (Status set accordingly, results so far) —
// and an error only when nothing worth storing exists.
type DeviceTestFunc func(ctx context.Context, dev model.ASRDevice, env DeviceTestEnv) (model.DeviceBenchmark, error)

// DeviceTestState is the in-memory state of a device's self-test.
type DeviceTestState struct {
	DeviceID   string
	State      string // DeviceTest*
	Phase      string // DeviceTestPhase* (running only)
	Step       int
	Steps      int
	Candidate  *devices.Candidate
	Progress   float64
	StartedAt  time.Time // zero while waiting
	FinishedAt time.Time // zero until it ended
	Error      *model.ErrorInfo
	Result     *model.DeviceBenchmark // when ended (also partial)
	// WaitingFor is why a waiting test does not run, when the power-aware
	// policy is the reason (backend Gate round 1, fix 5): on a machine with
	// a battery, model.WaitPowerQueue(id) of the device that is active now
	// (a pipeline asr run on it, else its self-test) — the test waits or
	// yielded for the single active device. "" otherwise (always on a
	// desktop). Computed when the state is read.
	WaitingFor string `json:"waiting_for,omitempty"`
}

// RunningASR is a pipeline asr job running on a device.
type RunningASR struct {
	JobID     string
	LectureID string
	Progress  float64
	StartedAt time.Time
}

// DeviceStatus is one device's scheduling state (resource bar, settings
// page, diagnostics).
type DeviceStatus struct {
	ID          string
	SlotsUsed   int  // pipeline jobs + a running self-test
	SlotsMax    int  // max_parallel
	Paused      bool // the device's own switch
	PausedAll   bool // the global switch (SetGPUPaused)
	BatteryHold bool // power management: on battery, an enabled, present device that is not the battery device
	Running     []RunningASR
	SelfTest    *DeviceTestState // nil = none since the program started
}

var (
	// ErrSelfTestRunning: StartDeviceTest while the device's test is
	// waiting or running (HTTP 409, code CodeSelfTestRunning).
	ErrSelfTestRunning = fmt.Errorf("%w: a self-test of this device is already running", state.ErrConflict)
	// ErrUnknownDevice: no such device row (HTTP 404, devices.not_found).
	ErrUnknownDevice = fmt.Errorf("%w: unknown asr device", db.ErrNotFound)
	// ErrDeviceTestUnavailable: no DeviceTest function was configured.
	ErrDeviceTestUnavailable = errors.New("device self-test is not available")
)

// devTestRun is one self-test (guarded by Scheduler.mu).
type devTestRun struct {
	st       DeviceTestState
	dev      model.ASRDevice
	started  bool // its goroutine runs (or ran)
	holding  bool // it holds a slot of its device
	cancel   context.CancelFunc
	done     chan struct{}
	lastEmit time.Time
	// attemptCancel cancels the running attempt only (a pipeline job that
	// can use no other device preempts the test, 3b Gate round 1 item 8);
	// preempted marks that the attempt ended for that reason.
	attemptCancel context.CancelFunc
	preempted     bool
	// cancelInfo is the state's error when the power-aware policy cancelled
	// the test (CodeSelfTestPowerBattery / CodeSelfTestPowerCPU;
	// enforceTestPolicy).
	cancelInfo *model.ErrorInfo
	// memCand is the candidate the test runs now, for the worker memory
	// budget (backend Gate round 1, fix 2): set by its progress reports
	// while it holds the slot, nil before the first, between two
	// candidates and after it gave the slot back (then its device's
	// largest candidate counts).
	memCand *devices.Candidate
}

func (t *devTestRun) snapshot() DeviceTestState {
	st := t.st
	if t.st.Candidate != nil {
		c := *t.st.Candidate
		st.Candidate = &c
	}
	if t.st.Error != nil {
		e := *t.st.Error
		st.Error = &e
	}
	if t.st.Result != nil {
		r := *t.st.Result
		st.Result = &r
	}
	return st
}

func (t *devTestRun) active() bool {
	return t.st.State == DeviceTestWaiting || t.st.State == DeviceTestRunning
}

// stateIn is t's state with WaitingFor filled from the policy input in
// (caller holds s.mu; in from policyInputLocked).
func (t *devTestRun) stateIn(in devices.PolicyInput) DeviceTestState {
	st := t.snapshot()
	st.WaitingFor = testWaitFor(st.State, in, st.DeviceID)
	return st
}

// testWaitFor is DeviceTestState.WaitingFor (backend Gate round 1, fix 5):
// a waiting test on a machine with a battery that the policy allows but
// does not let start (devices.SelfTest: another device is active) waits
// power_queue:{that device} — the highest-priority device with a pipeline
// asr run, else the device whose self-test holds a slot. "" otherwise.
func testWaitFor(state string, in devices.PolicyInput, id string) string {
	if state != DeviceTestWaiting || !in.Power.BatteryMachine() {
		return ""
	}
	if d := devices.SelfTest(in, id); !d.Allowed || d.Start {
		return ""
	}
	for _, r := range in.Running { // priority order (policyInputLocked)
		if r.DeviceID != id {
			return model.WaitPowerQueue(r.DeviceID)
		}
	}
	for _, d := range in.Devices {
		if d.ID != id && d.SelfTest {
			return model.WaitPowerQueue(d.ID)
		}
	}
	return ""
}

func unknownDevice(id string) error { return fmt.Errorf("%w: %s", ErrUnknownDevice, id) }

// ---------- admission ----------

// devicePlan is the outcome of the device rules that do not depend on a
// device's load: the candidates in priority order, or the reason there
// are none.
type devicePlan struct {
	cands        []model.ASRDevice
	reason       string
	snap         model.ASRSnapshot // normalized
	snapOK       bool
	fixed        bool // a fixed snapshot: only its device, never rebound
	bind         bool // an auto snapshot: bind the chosen device at start
	multiBlocked bool // multi_device off and another pipeline asr job runs
	resume       bool // devices.Placement.Resume (logging)
}

// policyBase is the state of a policy decision that the scheduler does not
// own: the ASR settings (normalized device mode), the device rows and their
// presence, the power reading and the pause switches. A scan reads it once
// (scanSettings), so every job of a scan is decided on the same state (FIFO
// holds even when a switch flips during the scan); outside a scan
// readPolicyBase reads it fresh.
type policyBase struct {
	asr       model.ASRSettings
	devList   []model.ASRDevice
	present   map[string]bool
	power     devices.Power
	pausedAll bool            // the global switch
	devPaused map[string]bool // the device switches
	// workers caches devices.WorkerState per backend|model|precision for a
	// scan (fresh-cuda, component_admit.go); filled by planDevices.
	workers map[string]string
}

// readPolicyBase reads a policyBase now. Never call it with s.mu held.
func (s *Scheduler) readPolicyBase() policyBase {
	b := policyBase{asr: model.NormalizeASRDeviceMode(s.set.ASR()), power: s.Power()}
	if s.devs != nil {
		b.devList = s.devs.List()
	}
	b.present = make(map[string]bool, len(b.devList))
	for _, d := range b.devList {
		b.present[d.ID] = s.devs.Present(d.ID)
	}
	s.mu.Lock()
	b.pausedAll = s.paused
	b.devPaused = make(map[string]bool, len(s.devPaused))
	for id, p := range s.devPaused {
		b.devPaused[id] = p
	}
	s.mu.Unlock()
	return b
}

// policyInputLocked is the devices.PolicyInput of b with the scheduler's
// own state now: slots, pipeline asr runs and self-tests holding a slot
// (caller holds s.mu).
func (s *Scheduler) policyInputLocked(b *policyBase) devices.PolicyInput {
	in := devices.PolicyInput{Power: b.power, Mode: b.asr.DeviceMode, Manual: b.asr.ManualDevices,
		MultiDevice: b.asr.MultiDevice, PausedAll: b.pausedAll, Devices: make([]devices.PolicyDevice, 0, len(b.devList))}
	prio := make(map[string]int, len(b.devList))
	for i, d := range b.devList {
		if isRemote(d.ID) {
			continue // SPEC-v3 V14.4: Colab is outside the power-aware policy (remote.go)
		}
		prio[d.ID] = i
		t := s.tests[d.ID]
		holding := t != nil && t.holding
		used := s.pipelineOnLocked(d.ID)
		if holding {
			used++
		}
		backend := d.Backend
		if backend == "" {
			backend = model.DeviceBackend(d.ID)
		}
		in.Devices = append(in.Devices, devices.PolicyDevice{ID: d.ID, Kind: model.DeviceKind(d.ID), Backend: backend,
			Enabled: d.Enabled, Present: b.present[d.ID], Paused: b.devPaused[d.ID], Slots: s.deviceSlots(d), Used: used, SelfTest: holding})
	}
	for id, r := range s.running {
		if r.res == ResourceGPU && r.device != "" && !isRemote(r.device) {
			in.Running = append(in.Running, devices.PolicyRun{JobID: id, DeviceID: r.device, Backend: model.DeviceBackend(r.device), Fixed: r.fixed})
		}
	}
	sort.Slice(in.Running, func(a, b int) bool {
		pa, oka := prio[in.Running[a].DeviceID]
		pb, okb := prio[in.Running[b].DeviceID]
		switch {
		case oka != okb:
			return oka
		case pa != pb:
			return pa < pb
		}
		return in.Running[a].JobID < in.Running[b].JobID
	})
	return in
}

// policyInput is policyInputLocked of a fresh policyBase.
func (s *Scheduler) policyInput() devices.PolicyInput {
	b := s.readPolicyBase()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policyInputLocked(&b)
}

// scanSettings reads the scan's policyBase once.
func (s *Scheduler) scanSettings(sc *scanState) {
	if sc.asrRead {
		return
	}
	sc.policyBase, sc.asrRead = s.readPolicyBase(), true
}

// row is the scan's row of a device id.
func (sc *scanState) row(id string) (model.ASRDevice, bool) {
	for _, d := range sc.devList {
		if d.ID == id {
			return d, true
		}
	}
	return model.ASRDevice{}, false
}

// planDevices asks the power-aware policy (devices.Place, power-brief §3.3)
// where the job may start, with the barrier of lead ruling 9.3
// (devices.SwitchHold) in the scan's FIFO order: record marks the first
// power_switch_pending of the scan (a job whose turn it is, not one in
// retry backoff).
func (s *Scheduler) planDevices(sc *scanState, j model.Job, record bool) devicePlan {
	s.scanSettings(sc)
	var p devicePlan
	pj := devices.PolicyJob{JobID: j.ID, Resume: j.Stats.ASRResume}
	if snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot); ok {
		p.snap, p.snapOK = model.NormalizeASRSnapshot(snap), true
		pj.Snap, pj.SnapOK = p.snap, true
	}
	// A job without a valid snapshot may run anywhere (the stage fails it,
	// v1.1 decision 4); it is never bound.
	p.fixed = p.snapOK && !p.snap.Auto
	p.bind = p.snapOK && p.snap.Auto
	s.mu.Lock()
	in := s.policyInputLocked(&sc.policyBase)
	s.mu.Unlock()
	// fresh-cuda (component_admit.go): a device whose components are
	// missing is not a place for the job; the wait then names it.
	masked := s.maskUnready(sc, &in, p)
	pl := devices.Place(in, pj)
	if len(pl.Devices) == 0 && len(masked) > 0 && noLocalDevice(pl.Wait) {
		if w := componentWait(in, pj, masked); w != "" {
			pl.Wait = w
		}
	}
	if sc.switchHold != "" {
		pl = devices.SwitchHold(in, sc.switchHold, pl)
	}
	if pl.Wait != "" {
		p.reason = pl.Wait
		if _, _, sw := model.ParseWaitPowerSwitchPending(pl.Wait); sw && record && sc.switchHold == "" {
			sc.switchHold = pl.Wait
		}
		return p
	}
	for _, id := range pl.Devices {
		if d, ok := sc.row(id); ok {
			p.cands = append(p.cands, d)
		}
	}
	if len(p.cands) == 0 {
		p.reason = model.WaitNoASRDevice
		return p
	}
	p.resume = pl.Resume
	// sc.multiFull: an earlier job of this scan waited for the running one
	// (scanState: a run that ended meanwhile frees the turn for the next scan).
	p.multiBlocked = !sc.asr.MultiDevice && (len(in.Running) > 0 || sc.multiFull)
	return p
}

// waitDevice is the device a policy wait names when the job waits for that
// one device to be free (power_queue:{id}, device_selftest:{id}): a
// self-test there yields to it and is preempted (3b Gate round 1, item 8).
func waitDevice(reason string) string {
	for _, prefix := range []string{model.WaitPowerQueuePrefix, model.WaitDeviceSelfTestPrefix} {
		if id, ok := strings.CutPrefix(reason, prefix); ok {
			return id
		}
	}
	return ""
}

// target is the model and precision a job runs with on d: an auto (or
// unparsable) snapshot takes the device row's, a fixed one keeps its own.
func (p devicePlan) target(d model.ASRDevice) (mdl, precision string) {
	if p.fixed {
		return p.snap.Model, p.snap.Precision
	}
	return d.Model, d.Precision
}

// deviceSlots is a device's slot count (one in fake ASR mode, SPEC §14).
// SPEC-v3 V5: on battery power (a machine with a battery running on it,
// devices.Power.OnBatteryPower of the last scan's reading) mlx:gpu runs
// one job at a time whatever its max_parallel; the unified memory and the
// battery are shared with everything else on the Mac. Safe with or
// without s.mu held (onBattery is atomic).
func (s *Scheduler) deviceSlots(d model.ASRDevice) int {
	if s.fakeASR() || d.MaxParallel < 1 {
		return 1
	}
	if d.ID == model.DeviceMLXGPU && s.onBattery.Load() {
		return 1
	}
	return d.MaxParallel
}

// memEstimateWith is devices.MemEstimateMB with the model components of
// cfg (devices.MemEstimateMBFor: an installed model's size on disk).
func memEstimateWith(cfg *config.Config) func(ctx context.Context, st db.Store, dataDir string, d model.ASRDevice) int {
	return func(ctx context.Context, st db.Store, dataDir string, d model.ASRDevice) int {
		return devices.MemEstimateMBFor(ctx, st, cfg, dataDir, d)
	}
}

// deviceAdmit picks the first candidate device that can take the job and
// fills r's device and accounting fields; otherwise it returns the reason
// of the highest-priority candidate (I8).
func (s *Scheduler) deviceAdmit(ctx context.Context, sc *scanState, r *run) string {
	p := s.planDevices(sc, r.job, true)
	if p.reason != "" {
		if id := waitDevice(p.reason); id != "" {
			// It waits for this one device (power_queue, a self-test there):
			// a self-test of it yields and is preempted, as in 3b. The
			// device is not given to a later job of this scan either, and
			// with multi_device off (power_queue: some asr run exists) no
			// device is (scanState).
			sc.noSlot(id, p.reason)
			if !sc.asr.MultiDevice && strings.HasPrefix(p.reason, model.WaitPowerQueuePrefix) {
				sc.multiFull = true
			}
			if d, ok := sc.row(id); ok {
				sc.want([]model.ASRDevice{d})
				sc.wantOnly(id)
			}
		}
		return p.reason
	}
	first, budgetWait := "", false
	var memTests []string // self-tests whose memory alone kept the job off a candidate
	for _, d := range p.cands {
		reason, miss := s.admitOn(ctx, sc, r, d, p)
		if reason == "" {
			r.device, r.devRow, r.bind, r.fixed = d.ID, d, p.bind, p.fixed
			if p.resume && r.job.Stats.ASRResume != nil && model.DeviceBackend(d.ID) == r.job.Stats.ASRResume.Backend {
				slog.Info("asr job continues from its power checkpoint", "job", r.job.ID, "device", d.ID,
					"interrupted_on", r.job.Stats.ASRResume.DeviceID)
			}
			return ""
		}
		budgetWait = budgetWait || miss.waits
		memTests = append(memTests, miss.blockers...)
		if first == "" {
			first = reason
		}
	}
	if p.multiBlocked {
		sc.multiFull = true // multi_device off: no other job takes the turn in this scan
	}
	if budgetWait {
		// No self-test takes a slot while a pipeline job waits for room in
		// the worker memory budget (it would only take more of it;
		// deviceFreeLocked).
		sc.memWait = true
	}
	if len(memTests) > 0 {
		// The worker memory budget counts the self-tests (fix 2), but a
		// self-test never holds up the transcription (3b): those tests are
		// preempted like the one of a device a job waits for.
		if sc.memPreempt == nil {
			sc.memPreempt = map[string]bool{}
		}
		for _, id := range memTests {
			sc.memPreempt[id] = true
		}
	}
	if !p.multiBlocked {
		sc.want(p.cands)
		if len(p.cands) == 1 {
			// It can use no other device (a single-device machine, or a
			// fixed snapshot): a self-test there is preempted at once.
			sc.wantOnly(p.cands[0].ID)
		}
	}
	return first
}

// markWanted records the candidates of a queued job that cannot start for
// a reason outside the device rules (retry backoff). Such a job makes a
// self-test yield between candidates but never preempts one: it could not
// start yet anyway.
func (s *Scheduler) markWanted(sc *scanState, j model.Job) {
	if p := s.planDevices(sc, j, false); p.reason == "" && !p.multiBlocked {
		sc.want(p.cands)
	}
}

// budgetMiss describes why the worker memory budget kept a job off a
// candidate (admitOn; backend Gate round 1, fix 2).
type budgetMiss struct {
	// waits: the job fits the budget by itself, so it waits for running
	// workers or self-tests to end (not set for a job that can never fit:
	// it must not keep every self-test out for good).
	waits bool
	// blockers are the devices of the self-tests whose memory alone breaks
	// the budget (without them the job would fit): the caller preempts them
	// when the job cannot start anywhere.
	blockers []string
}

// admitOn checks whether the job can start on candidate d and fills r's
// accounting fields when it can (reason ""); miss is set when the worker
// memory budget is the reason.
func (s *Scheduler) admitOn(ctx context.Context, sc *scanState, r *run, d model.ASRDevice, p devicePlan) (reason string, miss budgetMiss) {
	s.mu.Lock()
	t := s.tests[d.ID]
	testHolds := t != nil && t.holding
	n, vramReserved, legacyReserved, intelRunning, memReserved, workerMem := 0, 0, 0, 0, 0, 0
	for _, x := range s.running {
		if x.res != ResourceGPU {
			continue
		}
		workerMem += x.workerMemMB
		if x.device == d.ID {
			n++
			vramReserved += max(x.estimateMB-x.peakMB, 0)
			legacyReserved += max(x.legacyEstMB-x.peakMB, 0)
		}
		if systemMemBackend(model.DeviceBackend(x.device)) {
			intelRunning++
			if left := x.memEstimateMB - x.memPeakMB; left > 0 {
				memReserved += left
			}
		}
	}
	s.mu.Unlock()
	if why, full := sc.devFull[d.ID]; full {
		return why, miss // no slot for an earlier job of this scan (scanState.devFull)
	}
	switch {
	case testHolds:
		sc.noSlot(d.ID, model.WaitDeviceSelfTest(d.ID))
		return model.WaitDeviceSelfTest(d.ID), miss
	case p.multiBlocked:
		return model.WaitGPUSlot, miss
	case n >= s.deviceSlots(d):
		sc.noSlot(d.ID, model.WaitGPUSlot)
		return model.WaitGPUSlot, miss
	}
	if s.fakeASR() {
		return "", miss
	}
	mdl, precision := p.target(d)
	// The worker memory budget (power management, lead addition after I12;
	// CLAUDE.md revision 4): the running workers' estimates plus this one
	// stay within asr.worker_mem_budget_mb, on every backend (the CUDA
	// worker's host memory too). A self-test holding a slot counts with
	// the candidate it runs (backend Gate round 1, fix 2).
	workerEst := sc.memEstimate(ctx, s, d, mdl, precision)
	held := s.heldTestMem(ctx, sc.memCache())
	testMem := 0
	for _, mb := range held {
		testMem += mb
	}
	budget := s.workerMemBudget()
	if workerMem+workerEst > budget {
		// The pipeline workers alone leave no room (or the job never fits:
		// then it keeps no self-test out, miss.waits false).
		miss.waits = workerEst <= budget
		return model.WaitMemory, miss
	}
	// With the self-tests it does not fit: the job waits, and when every
	// other check passes, the tests are what keeps it out (miss.blockers:
	// preempted, a self-test never holds up the transcription). Checked
	// last, so a job that waits for VRAM or system memory anyway does not
	// preempt a test again and again.
	testsBlock := workerMem+testMem+workerEst > budget
	admit := func() (string, budgetMiss) {
		if !testsBlock {
			return "", miss
		}
		miss.waits = true
		for id := range held {
			miss.blockers = append(miss.blockers, id)
		}
		sort.Strings(miss.blockers)
		return model.WaitMemory, miss
	}
	r.workerMemMB = workerEst
	switch {
	case model.DeviceKind(d.ID) == model.DeviceKindCUDA:
		if !sc.gpuRead {
			sc.gpu, sc.gpuErr = s.gpuQ(ctx)
			sc.gpuRead = true
			s.rememberGPU(sc.gpu, sc.gpuErr)
		}
		if sc.gpuErr != nil {
			// Reading failed: a single job on the card (SPEC §5).
			if n > 0 {
				sc.noSlot(d.ID, model.WaitGPUSlot)
				return model.WaitGPUSlot, miss
			}
			return admit()
		}
		ct := config.ResolveComputeType(precision, model.FWDevice(d.ID), sc.gpu.TotalMB)
		est, legacy := s.vramEstimates(ctx, mdl, ct)
		if why := vramFits(sc.gpu.FreeMB, sc.asr.VRAMReserveMB, n, est, vramReserved, legacy, legacyReserved); why != "" {
			return why, miss
		}
		r.estimateMB, r.legacyEstMB, r.baseUsedMB, r.admitFreeMB, r.sample, r.computeType, r.asrModel = est, legacy, sc.gpu.UsedMB, sc.gpu.FreeMB, true, ct, mdl
	case systemMemBackend(d.Backend):
		if !sc.memRead {
			sc.memMB, sc.memErr = s.memAvail()
			sc.memRead = true
		}
		if sc.memErr != nil {
			// Reading failed: a single Intel job at a time.
			if intelRunning > 0 {
				return model.WaitMemory, miss
			}
			return admit()
		}
		est := sc.memEstimate(ctx, s, d, mdl, precision)
		if sc.memMB-sc.asr.MemReserveMB-memReserved < est {
			return model.WaitMemory, miss
		}
		r.memEstimateMB, r.memBaseAvailMB, r.memSample = est, sc.memMB, true
	}
	return admit()
}

// systemMemBackend: the backend's model lives in system memory, admitted
// by the available memory (I8): OpenVINO (Intel GPU and NPU share it) and
// MLX (Apple unified memory, SPEC-v3 V5).
func systemMemBackend(backend string) bool {
	return backend == model.BackendOpenVINO || backend == model.BackendMLX
}

// workerMemBudget is asr.worker_mem_budget_mb (≤ 0: the default).
func (s *Scheduler) workerMemBudget() int {
	if b := s.cfg.ASR.WorkerMemBudgetMB; b > 0 {
		return b
	}
	return config.DefaultWorkerMemBudgetMB
}

// cachedMemEstimate is devices.MemEstimateMB (s.memEstimate) of d running
// mdl/precision, memoized in cache (device|model|precision).
func (s *Scheduler) cachedMemEstimate(ctx context.Context, cache map[string]int, d model.ASRDevice, mdl, precision string) int {
	key := d.ID + "|" + mdl + "|" + precision
	if v, ok := cache[key]; ok {
		return v
	}
	d.Model, d.Precision = mdl, precision
	v := s.memEstimate(ctx, s.st, s.dataDir, d)
	cache[key] = v
	return v
}

// testMemMB is a self-test's worker for the worker memory budget (power-
// brief §3.7; backend Gate round 1, fix 2): the estimate of the candidate
// it runs now, else (none reported yet, between two candidates, about to
// take or retake its slot) of its device's largest candidate
// (devices.Candidates; the row's own model when the device has none).
// Never call it with s.mu held: the estimate may read the database.
func (s *Scheduler) testMemMB(ctx context.Context, cache map[string]int, dev model.ASRDevice, cand *devices.Candidate) int {
	if cand != nil {
		return s.cachedMemEstimate(ctx, cache, dev, cand.Model, cand.Precision)
	}
	cands := devices.Candidates(dev.ID)
	if len(cands) == 0 {
		return s.cachedMemEstimate(ctx, cache, dev, dev.Model, dev.Precision)
	}
	largest := 0
	for _, c := range cands {
		largest = max(largest, s.cachedMemEstimate(ctx, cache, dev, c.Model, c.Precision))
	}
	return largest
}

// heldTestMem prices the self-tests holding a slot now: device id → their
// testMemMB. Takes s.mu briefly; never call it with s.mu held.
func (s *Scheduler) heldTestMem(ctx context.Context, cache map[string]int) map[string]int {
	type held struct {
		dev  model.ASRDevice
		cand *devices.Candidate
	}
	var hs []held
	s.mu.Lock()
	for _, t := range s.tests {
		if !t.holding {
			continue
		}
		h := held{dev: t.dev}
		if t.memCand != nil {
			c := *t.memCand
			h.cand = &c
		}
		hs = append(hs, h)
	}
	s.mu.Unlock()
	out := make(map[string]int, len(hs))
	for _, h := range hs {
		out[h.dev.ID] = s.testMemMB(ctx, cache, h.dev, h.cand)
	}
	return out
}

// testBudget is the worker memory budget of a self-test about to take (or
// take back) its device's slot, priced before s.mu is taken
// (testBudgetFor): the running pipeline workers (r.workerMemMB, read under
// s.mu) + the other slot-holding tests + this test's largest candidate
// must stay within asr.worker_mem_budget_mb (backend Gate round 1, fix 2).
type testBudget struct {
	check bool           // false in fake ASR mode (no memory admission, as for pipeline jobs)
	held  map[string]int // heldTestMem
	own   int            // testMemMB of the test (no candidate runs at that point)
}

// testBudgetFor prices dev's test (never with s.mu held).
func (s *Scheduler) testBudgetFor(ctx context.Context, cache map[string]int, dev model.ASRDevice) testBudget {
	if s.fakeASR() {
		return testBudget{}
	}
	return testBudget{check: true, held: s.heldTestMem(ctx, cache), own: s.testMemMB(ctx, cache, dev, nil)}
}

// fitsLocked reports whether the test of device id fits the budget (caller
// holds s.mu).
func (tb testBudget) fitsLocked(s *Scheduler, id string) bool {
	if !tb.check {
		return true
	}
	total := tb.own
	for dev, mb := range tb.held {
		if dev != id {
			total += mb
		}
	}
	for _, x := range s.running {
		if x.res == ResourceGPU {
			total += x.workerMemMB
		}
	}
	return total <= s.workerMemBudget()
}

func (sc *scanState) want(ds []model.ASRDevice) {
	if sc.wanted == nil {
		sc.wanted = map[string]bool{}
	}
	for _, d := range ds {
		sc.wanted[d.ID] = true
	}
}

func (sc *scanState) wantOnly(id string) {
	if sc.wantedOnly == nil {
		sc.wantedOnly = map[string]bool{}
	}
	sc.wantedOnly[id] = true
}

// noSlot records that device id had no free slot for a job of this scan,
// reason being that job's waiting_for there: the device counts as full for
// the rest of the scan (scanState.devFull). The first reason is kept.
func (sc *scanState) noSlot(id, reason string) {
	if sc.devFull == nil {
		sc.devFull = map[string]string{}
	}
	if _, ok := sc.devFull[id]; !ok {
		sc.devFull[id] = reason
	}
}

// preemptDeviceTests cancels the running attempt of every self-test that
// holds up the transcription (the test must not hold it up for a whole
// candidate): the device is waited for by a queued pipeline job that can
// use no other device (3b Gate round 1, item 8); the test's memory alone
// keeps a queued pipeline job from starting (the worker memory budget,
// backend Gate round 1, fix 2; memPreempt); or, on a machine with a
// battery, a pipeline asr job runs on another device (one active device,
// backend Gate round 1, fix 1 — so a job admitted while another device is
// tested makes the test yield in the same scan). The test starts over once
// its device is free again (runDeviceTest; deviceFreeLocked); the
// interrupted attempt is not part of the result. Called at the end of a
// scan, after s.wanted / s.wantedOnly / s.memPreempt are set.
func (s *Scheduler) preemptDeviceTests(memPreempt map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	battery := s.powerRead && s.power.BatteryMachine()
	for id, t := range s.tests {
		if !t.holding || t.st.State != DeviceTestRunning || t.attemptCancel == nil || t.preempted {
			continue
		}
		other := ""
		if battery {
			other = s.pipelineElsewhereLocked(id)
		}
		why := ""
		switch {
		case s.wantedOnly[id]:
			why = "a pipeline job waits for the device"
		case memPreempt[id]:
			why = "its memory keeps a pipeline job from starting (worker memory budget)"
		case other != "":
			why = "single active device: a pipeline job runs on " + other
		default:
			continue
		}
		t.preempted = true
		t.attemptCancel()
		slog.Info("device self-test preempted: "+why, "device", id)
	}
}

// pipelineElsewhereLocked is a device other than id with a pipeline asr
// run ("" when none; caller holds s.mu).
func (s *Scheduler) pipelineElsewhereLocked(id string) string {
	other := ""
	for _, r := range s.running {
		if r.res == ResourceGPU && r.device != "" && r.device != id && !isRemote(r.device) && (other == "" || r.device < other) {
			other = r.device
		}
	}
	return other
}

// memEstimate is devices.MemEstimateMB for d running mdl/precision, once
// per scan and configuration.
func (sc *scanState) memEstimate(ctx context.Context, s *Scheduler, d model.ASRDevice, mdl, precision string) int {
	return s.cachedMemEstimate(ctx, sc.memCache(), d, mdl, precision)
}

// memCache is the scan's memoization of the memory estimates.
func (sc *scanState) memCache() map[string]int {
	if sc.memEst == nil {
		sc.memEst = map[string]int{}
	}
	return sc.memEst
}

// sampleMem follows the available memory while an Intel run runs: its
// peak use is the largest drop below the reading at admission.
func (s *Scheduler) sampleMem(ctx context.Context, r *run) {
	t := time.NewTicker(s.sampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		mb, err := s.memAvail()
		if err != nil {
			continue
		}
		if d := r.memBaseAvailMB - mb; d > 0 {
			s.mu.Lock()
			if d > r.memPeakMB {
				r.memPeakMB = d
			}
			s.mu.Unlock()
		}
	}
}

// ---------- power ----------

// readPower takes the scan's power reading. A change of known / on
// battery / battery present (or of the saver flag, display only) is
// announced and triggers another scan; a change of the first three is a
// power change of the policy (applyPowerChange: unplugging kills the runs
// that migrate or suspend, power-brief §3.4).
func (s *Scheduler) readPower() {
	if s.powerFn == nil {
		return
	}
	p := s.powerFn()
	s.mu.Lock()
	old, had := s.power, s.powerRead
	s.power, s.powerRead = p, true
	s.onBattery.Store(p.OnBatteryPower())
	s.mu.Unlock()
	policy := old.Known != p.Known || old.OnBattery != p.OnBattery || old.HasBattery != p.HasBattery
	if had && !policy && old.Saver == p.Saver {
		return
	}
	if s.devs != nil {
		s.ev.Changed("devices", "")
	}
	if had {
		if policy && s.devs != nil {
			slog.Info("power state changed", "known", p.Known, "on_battery", p.OnBattery, "battery", p.HasBattery)
			s.applyPowerChange(old)
		}
		s.Kick()
	}
}

// applyPowerChange runs devices.OnPowerChange for the running pipeline asr
// jobs: a migrate or suspend action is recorded on the run (finish turns
// it into the power requeue) and the run's context is cancelled with
// ErrPowerMigrate / ErrPowerSuspend as the cause, which kills the worker at
// once (3b lead ruling 9). A run the user cancels meanwhile stays a cancel.
func (s *Scheduler) applyPowerChange(prev devices.Power) {
	b := s.readPolicyBase()
	s.mu.Lock()
	in := s.policyInputLocked(&b)
	type kill struct {
		r     *run
		cause error
		a     devices.RunAction
	}
	var kills []kill
	for _, a := range devices.OnPowerChange(prev, in) {
		r := s.running[a.JobID]
		if r == nil || a.Action == devices.PowerContinue || r.powerAction != "" || r.userCancel || r.cancelCause == nil {
			continue
		}
		r.powerAction, r.powerTo, r.powerOnBattery = a.Action, a.To, in.Power.OnBatteryPower()
		cause := ErrPowerSuspend
		if a.Action == devices.PowerMigrate {
			cause = ErrPowerMigrate
		}
		kills = append(kills, kill{r: r, cause: cause, a: a})
	}
	s.mu.Unlock()
	for _, k := range kills {
		slog.Info("power-aware policy interrupts an asr job", "job", k.r.job.ID, "action", k.a.Action, "from", k.a.From, "to", k.a.To)
		k.r.cancelCause(k.cause)
	}
}

// Power is the power state of the last scan (Options.Power; always on AC
// without it).
func (s *Scheduler) Power() devices.Power {
	s.mu.Lock()
	p, ok := s.power, s.powerRead
	s.mu.Unlock()
	if ok {
		return p
	}
	if s.powerFn == nil {
		return devices.Power{}
	}
	return s.powerFn()
}

// ---------- statuses and pause ----------

// pipelineOnLocked counts the pipeline asr runs on a device (caller holds s.mu).
func (s *Scheduler) pipelineOnLocked(id string) int {
	n := 0
	for _, r := range s.running {
		if r.res == ResourceGPU && r.device == id {
			n++
		}
	}
	return n
}

// DeviceStatuses returns every device's state in priority order (nil
// without Options.Devices).
func (s *Scheduler) DeviceStatuses() []DeviceStatus {
	if s.devs == nil {
		return nil
	}
	b := s.readPolicyBase()
	list := b.devList
	slots := make([]int, len(list))
	for i, d := range list {
		slots[i] = s.deviceSlots(d)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// BatteryHold (power management, power-brief §3.4): a machine with a
	// battery runs on battery, and the device is enabled, present and not
	// the battery device.
	onBattery := b.power.OnBatteryPower()
	in := s.policyInputLocked(&b)
	pbatt := ""
	if onBattery {
		pbatt, _ = devices.BatteryDevice(in)
	}
	out := make([]DeviceStatus, 0, len(list))
	for i, d := range list {
		ds := DeviceStatus{ID: d.ID, SlotsMax: slots[i], Paused: s.devPaused[d.ID], PausedAll: s.paused,
			BatteryHold: onBattery && d.Enabled && b.present[d.ID] && d.ID != pbatt && !isRemote(d.ID), Running: []RunningASR{}}
		for jid, r := range s.running {
			if r.res != ResourceGPU || r.device != d.ID {
				continue
			}
			r.pmu.Lock()
			p := r.latest
			r.pmu.Unlock()
			// r.job is reassigned under r.wmu by start(): only the fields
			// fixed before the run was registered are read here.
			ds.Running = append(ds.Running, RunningASR{JobID: jid, LectureID: r.scopeID, Progress: p, StartedAt: r.startedAt})
			ds.SlotsUsed++
		}
		sort.Slice(ds.Running, func(a, b int) bool { return ds.Running[a].StartedAt.Before(ds.Running[b].StartedAt) })
		if t := s.tests[d.ID]; t != nil {
			st := t.stateIn(in)
			ds.SelfTest = &st
			if t.holding {
				ds.SlotsUsed++
			}
		}
		out = append(out, ds)
	}
	return out
}

// SetDevicePaused persists one device's pause switch (kv
// model.KVASRDevicePausedPrefix+id, loaded by New). Paused: no new job and
// no self-test starts on it; with cancelRunning, its running jobs and
// self-test are cancelled. Emits Changed("devices", "") and kicks.
// ErrUnknownDevice for an unknown id.
func (s *Scheduler) SetDevicePaused(id string, paused, cancelRunning bool) error {
	if s.devs == nil {
		return unknownDevice(id)
	}
	if _, ok := s.devs.Get(id); !ok {
		return unknownDevice(id)
	}
	v := "0"
	if paused {
		v = "1"
	}
	if err := s.st.SetKV(context.Background(), model.KVASRDevicePausedPrefix+id, v); err != nil {
		return err
	}
	s.mu.Lock()
	s.devPaused[id] = paused
	var ids []string
	if paused && cancelRunning {
		for jid, r := range s.running {
			if r.res == ResourceGPU && r.device == id {
				ids = append(ids, jid)
			}
		}
	}
	s.mu.Unlock()
	s.cancelJobs(ids, "cancel asr job on device pause")
	if paused && cancelRunning {
		if _, err := s.CancelDeviceTest(id); err != nil {
			slog.Warn("cancel self-test on device pause", "device", id, "err", err)
		}
	}
	s.ev.Changed("devices", "")
	s.Kick()
	return nil
}

// cancelJobs cancels running jobs concurrently and waits for them.
func (s *Scheduler) cancelJobs(ids []string, what string) {
	done := make(chan struct{}, len(ids))
	for _, id := range ids {
		go func(id string) {
			if err := s.Cancel(id); err != nil {
				slog.Warn(what, "job", id, "err", err)
			}
			done <- struct{}{}
		}(id)
	}
	for range ids {
		<-done
	}
}

// DevicePaused reports one device's own pause switch.
func (s *Scheduler) DevicePaused(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devPaused[id]
}

// loadDevicePauses reads the pause switches of the known rows (New).
func (s *Scheduler) loadDevicePauses(ctx context.Context) error {
	if s.devs == nil {
		return nil
	}
	for _, d := range s.devs.List() {
		v, ok, err := s.st.GetKV(ctx, model.KVASRDevicePausedPrefix+d.ID)
		if err != nil {
			return err
		}
		s.devPaused[d.ID] = ok && (v == "1" || v == "true")
	}
	return nil
}

// ---------- self-tests ----------

// StartDeviceTest queues a self-test of the device (state waiting; it
// starts when the device is free). ErrUnknownDevice, ErrSelfTestRunning,
// ErrDeviceTestUnavailable.
func (s *Scheduler) StartDeviceTest(ctx context.Context, id string) (DeviceTestState, error) {
	if s.devs == nil || s.devTest == nil {
		return DeviceTestState{}, ErrDeviceTestUnavailable
	}
	dev, ok := s.devs.Get(id)
	if !ok {
		return DeviceTestState{}, unknownDevice(id)
	}
	if isRemote(id) {
		// SPEC-v3 V14: Colab measures its speed on every lecture (colab.rates).
		return DeviceTestState{DeviceID: id, State: DeviceTestIdle}, &model.CodedError{
			Info: model.ErrorInfo{Code: selfTestRemoteCode(id), Params: map[string]any{"device": deviceLabel(dev)}},
			Err:  fmt.Errorf("%w: colab:remote has no device self-test", state.ErrConflict)}
	}
	b := s.readPolicyBase()
	s.mu.Lock()
	in := s.policyInputLocked(&b)
	if t := s.tests[id]; t != nil && t.active() {
		st := t.stateIn(in)
		s.mu.Unlock()
		return st, ErrSelfTestRunning
	}
	if !devices.SelfTest(in, id).Allowed {
		// Power management: on battery only the battery device is tested,
		// never a CPU device on a machine with a battery.
		st := DeviceTestState{DeviceID: id, State: DeviceTestIdle}
		if t := s.tests[id]; t != nil {
			st = t.snapshot()
		}
		s.mu.Unlock()
		return st, powerRefusalError(dev, in)
	}
	t := &devTestRun{st: DeviceTestState{DeviceID: id, State: DeviceTestWaiting}}
	s.tests[id] = t
	st := t.stateIn(in)
	s.mu.Unlock()
	slog.Info("device self-test queued", "device", id)
	s.ev.Changed("devices", "")
	s.Kick()
	return st, nil
}

// CancelDeviceTest cancels a waiting or running self-test (a running worker
// is killed; partial results are stored) and returns the state; a test
// that is not waiting or running is returned as it is (state idle when
// the device has had none since the program started).
func (s *Scheduler) CancelDeviceTest(id string) (DeviceTestState, error) {
	if s.devs == nil {
		return DeviceTestState{}, unknownDevice(id)
	}
	if _, ok := s.devs.Get(id); !ok {
		return DeviceTestState{}, unknownDevice(id)
	}
	s.mu.Lock()
	t := s.tests[id]
	switch {
	case t == nil:
		s.mu.Unlock()
		return DeviceTestState{DeviceID: id, State: DeviceTestIdle}, nil
	case !t.active():
		st := t.snapshot()
		s.mu.Unlock()
		return st, nil
	case !t.started:
		t.st.State, t.st.FinishedAt = DeviceTestCancelled, time.Now()
		st := t.snapshot()
		s.mu.Unlock()
		slog.Info("device self-test cancelled before it started", "device", id)
		s.ev.Changed("devices", "")
		return st, nil
	}
	t.cancel()
	done := t.done
	s.mu.Unlock()
	timer := time.NewTimer(s.cancelWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		slog.Warn("self-test did not stop within the cancel wait", "device", id, "wait", s.cancelWait)
	}
	s.mu.Lock()
	st := t.snapshot()
	s.mu.Unlock()
	return st, nil
}

// DeviceTest returns the device's self-test state (false = none since the
// program started), WaitingFor computed now.
func (s *Scheduler) DeviceTest(id string) (DeviceTestState, bool) {
	var b policyBase
	if s.devs != nil {
		b = s.readPolicyBase()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tests[id]
	if t == nil {
		return DeviceTestState{}, false
	}
	return t.stateIn(s.policyInputLocked(&b)), true
}

// cancelAllTests cancels every waiting or running self-test (global pause
// with cancel_running).
func (s *Scheduler) cancelAllTests() {
	s.mu.Lock()
	var ids []string
	for id, t := range s.tests {
		if t.active() {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		if _, err := s.CancelDeviceTest(id); err != nil {
			slog.Warn("cancel self-test on pause", "device", id, "err", err)
		}
	}
}

// deviceFreeLocked reports whether a self-test may hold a slot of the
// device now: nothing paused, no pipeline job running on it or waiting for
// it (3b), the power policy lets it start (devices.SelfTest: on a machine
// with a battery only while no other device is active, on battery only
// the battery device), no pipeline job waited for room in the worker
// memory budget in the last scan (s.memWait) and the budget fits the test
// (tb; backend Gate round 1, fix 2). b is the policy state read before s.mu was taken (caller holds
// s.mu).
func (s *Scheduler) deviceFreeLocked(id string, b *policyBase, tb testBudget) bool {
	if s.paused || s.devPaused[id] || s.wanted[id] || s.pipelineOnLocked(id) > 0 {
		return false
	}
	d := devices.SelfTest(s.policyInputLocked(b), id)
	if !d.Allowed || !d.Start {
		return false
	}
	return !(tb.check && s.memWait) && tb.fitsLocked(s, id)
}

// testRefusal is why the power policy does not allow a self-test of d
// (devices.SelfTest Allowed false): CodeSelfTestPowerCPU for a CPU device
// on a machine with a battery (any power state), else
// CodeSelfTestPowerBattery (on battery, not the battery device); params
// device = the row's label.
func testRefusal(d model.ASRDevice, in devices.PolicyInput) model.ErrorInfo {
	code := CodeSelfTestPowerBattery
	if in.Power.BatteryMachine() && model.DeviceKind(d.ID) == model.DeviceKindCPU {
		code = CodeSelfTestPowerCPU
	}
	return model.ErrorInfo{Code: code, Params: map[string]any{"device": deviceLabel(d)}}
}

// powerRefusalError is StartDeviceTest's refusal by the power policy (HTTP
// 409; testRefusal's code, wrapping ErrSelfTestPowerCPU or
// ErrSelfTestPowerBattery).
func powerRefusalError(d model.ASRDevice, in devices.PolicyInput) error {
	info := testRefusal(d, in)
	sentinel := ErrSelfTestPowerBattery
	if info.Code == CodeSelfTestPowerCPU {
		sentinel = ErrSelfTestPowerCPU
	}
	return &model.CodedError{Info: info, Err: fmt.Errorf("%w (%s)", sentinel, deviceLabel(d))}
}

func deviceLabel(d model.ASRDevice) string {
	if strings.TrimSpace(d.Label) != "" {
		return d.Label
	}
	return d.ID
}

// enforceTestPolicy cancels every waiting or running self-test the power
// policy no longer allows (devices.SelfTest Allowed false: on battery, not
// the battery device, also right after the charger was pulled; a CPU
// device once the reading shows a machine with a battery), with
// testRefusal's code (CodeSelfTestPowerBattery / CodeSelfTestPowerCPU) as
// the test's error. A running test's worker is
// killed (its partial result is stored as for any cancel). Called by every
// scan after the power reading; never waits for a test to end.
func (s *Scheduler) enforceTestPolicy() {
	if s.devs == nil {
		return
	}
	// A fresh read, not the scan's policyBase: that one is read lazily by
	// the first asr job of the scan (as in 3b), as late as possible.
	b := s.readPolicyBase()
	s.mu.Lock()
	in := s.policyInputLocked(&b)
	changed := false
	for id, t := range s.tests {
		if !t.active() || devices.SelfTest(in, id).Allowed {
			continue
		}
		row := model.ASRDevice{ID: id}
		for _, d := range b.devList {
			if d.ID == id {
				row = d
			}
		}
		ri := testRefusal(row, in)
		info := &ri
		if !t.started {
			t.st.State, t.st.FinishedAt, t.st.Error = DeviceTestCancelled, time.Now(), info
			changed = true
			slog.Info("device self-test cancelled by the power-aware policy before it started", "device", id)
			continue
		}
		if t.cancelInfo == nil && t.cancel != nil {
			t.cancelInfo = info
			t.cancel()
			slog.Info("device self-test cancelled by the power-aware policy", "device", id)
		}
	}
	s.mu.Unlock()
	if changed {
		s.ev.Changed("devices", "")
	}
}

// startDeviceTests starts the waiting self-tests whose device is free. It
// runs at the end of a scan (under scanMu), after the pipeline jobs had
// their turn.
func (s *Scheduler) startDeviceTests(ctx context.Context, sc *scanState) {
	if s.devs == nil || s.devTest == nil || ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	var waiting []string
	for id, t := range s.tests {
		if t.st.State == DeviceTestWaiting && !t.started {
			waiting = append(waiting, id)
		}
	}
	s.mu.Unlock()
	if len(waiting) == 0 {
		return
	}
	sort.Strings(waiting)
	s.scanSettings(sc)
	for _, id := range waiting {
		dev, ok := s.devs.Get(id)
		if !ok {
			continue
		}
		tb := s.testBudgetFor(ctx, sc.memCache(), dev)
		s.mu.Lock()
		t := s.tests[id]
		if t == nil || t.started || t.st.State != DeviceTestWaiting || !s.deviceFreeLocked(id, &sc.policyBase, tb) {
			s.mu.Unlock()
			continue
		}
		tctx, cancel := context.WithCancel(s.baseCtx)
		t.started, t.holding, t.cancel, t.done, t.dev = true, true, cancel, make(chan struct{}), dev
		t.st.State, t.st.StartedAt = DeviceTestRunning, time.Now()
		s.wg.Add(1)
		s.mu.Unlock()
		s.awake(1) // released at the end of runDeviceTest
		slog.Info("device self-test started", "device", id)
		s.ev.Changed("devices", "")
		go s.runDeviceTest(tctx, t)
	}
}

func (s *Scheduler) runDeviceTest(ctx context.Context, t *devTestRun) {
	defer s.wg.Done()
	defer close(t.done)
	defer s.awake(-1)
	id := t.dev.ID
	env := DeviceTestEnv{
		Report:          func(p DeviceTestProgress) { s.reportTest(t, p) },
		BeforeCandidate: func(c context.Context) error { return s.beforeCandidate(c, t) },
	}
	var (
		b   model.DeviceBenchmark
		err error
	)
	for {
		// Every attempt has its own context: a preemption ends the attempt,
		// not the test (DeviceTestFunc drops the interrupted candidate).
		actx, acancel := context.WithCancel(ctx)
		s.mu.Lock()
		t.attemptCancel, t.preempted = acancel, false
		s.mu.Unlock()
		b, err = s.callDeviceTest(actx, t.dev, env)
		acancel()
		// A complete result (done or failed) is kept even when the
		// preemption came too late to stop it (Gate round 2, suggestion 3):
		// only an attempt that really was cut short starts over.
		complete := err == nil && b.Status != model.BenchmarkCancelled
		s.mu.Lock()
		t.attemptCancel = nil
		preempted := t.preempted && ctx.Err() == nil && !complete
		if preempted {
			t.holding, t.memCand = false, nil
			t.st.State, t.st.Phase, t.st.Progress = DeviceTestWaiting, "", 0
		}
		s.mu.Unlock()
		if !preempted {
			break
		}
		// The whole attempt is discarded; the test starts over when the
		// device is free of pipeline work.
		s.ev.Changed("devices", "")
		s.Kick()
		if werr := s.waitDeviceFree(ctx, t); werr != nil {
			// Cancelled while waiting to start over: no result at all.
			b, err = model.DeviceBenchmark{Status: model.BenchmarkCancelled}, nil
			break
		}
		slog.Info("device self-test starts over after the pipeline work", "device", id)
		s.ev.Changed("devices", "")
	}
	cancelled := ctx.Err() != nil
	t.cancel()
	// nothing: cancelled (or cut short) before any candidate finished.
	nothing := err == nil && (b.Status == model.BenchmarkCancelled || (b.Status == "" && cancelled)) && len(b.Results) == 0
	if err == nil {
		if b.Status == "" {
			b.Status = model.BenchmarkDone
			if cancelled {
				b.Status = model.BenchmarkCancelled
			}
		}
		if b.Results == nil {
			b.Results = []model.BenchmarkResult{}
		}
		// A test cancelled or cut short by a shutdown before any candidate
		// finished has nothing to keep: the previous complete result stays
		// (3b Gate round 1, item 6, revising brief §3.5).
		if b.Status == model.BenchmarkCancelled && len(b.Results) == 0 {
			slog.Info("self-test ended without a finished candidate: last_benchmark kept", "device", id)
		} else if serr := s.devs.SetBenchmark(context.Background(), id, b); serr != nil {
			slog.Error("store self-test result", "device", id, "err", serr)
		}
	}
	s.mu.Lock()
	t.holding, t.memCand = false, nil
	t.st.Phase = ""
	t.st.FinishedAt = time.Now()
	switch {
	case err != nil && cancelled:
		t.st.State = DeviceTestCancelled
	case err != nil:
		t.st.State, t.st.Error = DeviceTestFailed, testErrorInfo(err)
	case b.Status == model.BenchmarkCancelled:
		t.st.State = DeviceTestCancelled
	case b.Status == model.BenchmarkFailed:
		t.st.State, t.st.Error = DeviceTestFailed, failedInfo(b.Error)
	default:
		t.st.State = DeviceTestDone
	}
	if t.st.State == DeviceTestCancelled && t.cancelInfo != nil {
		e := *t.cancelInfo
		t.st.Error = &e // cancelled by the power-aware policy (enforceTestPolicy)
	}
	if err == nil && !nothing {
		bb := b
		t.st.Result = &bb
	} // else no Result: there is nothing to show (Gate round 2, suggestion 4)
	final := t.st.State
	s.mu.Unlock()
	slog.Info("device self-test ended", "device", id, "state", final, "err", err)
	s.ev.Changed("devices", "")
	s.Kick()
}

// callDeviceTest runs the test function, turning a panic into an error.
func (s *Scheduler) callDeviceTest(ctx context.Context, dev model.ASRDevice, env DeviceTestEnv) (b model.DeviceBenchmark, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic in device self-test: %v", p)
			slog.Error("self-test panic", "device", dev.ID, "panic", p)
		}
	}()
	return s.devTest(ctx, dev, env)
}

// testErrorInfo is the coded form of a self-test error: its ErrorInfo when
// coded, else CodeSelfTestFailed with the text as Detail (3b Gate round 1,
// item 7).
func testErrorInfo(err error) *model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) && ce.Info.Code != "" {
		info := ce.Info
		if ce.Err != nil && info.Detail == "" {
			info.Detail = ce.Err.Error()
		}
		return &info
	}
	return &model.ErrorInfo{Code: CodeSelfTestFailed, Detail: err.Error()}
}

// failedInfo is the error of a failed benchmark, CodeSelfTestFailed when
// it has no code (the original text, if any, kept as Detail).
func failedInfo(info *model.ErrorInfo) *model.ErrorInfo {
	if info != nil && info.Code != "" {
		return info
	}
	out := &model.ErrorInfo{Code: CodeSelfTestFailed}
	if info != nil {
		out.Detail = info.Detail
	}
	return out
}

// waitDeviceFree waits until a preempted test can hold its device again
// (no pipeline work on it or waiting for it, nothing paused) and takes the
// slot; ctx.Err() when the test is cancelled meanwhile.
func (s *Scheduler) waitDeviceFree(ctx context.Context, t *devTestRun) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.retakeSlot(ctx, t) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.scanInterval):
		}
	}
}

func (s *Scheduler) reportTest(t *devTestRun, p DeviceTestProgress) {
	if p.Progress != p.Progress { // NaN
		p.Progress = 0
	}
	p.Progress = max(0, min(1, p.Progress))
	s.mu.Lock()
	changed := t.st.Phase != p.Phase || t.st.Step != p.Step || t.st.Steps != p.Steps
	t.st.Phase, t.st.Step, t.st.Steps, t.st.Progress = p.Phase, p.Step, p.Steps, p.Progress
	if p.Candidate.Model != "" || p.Candidate.Precision != "" {
		c := p.Candidate
		t.st.Candidate = &c
	} else {
		t.st.Candidate = nil
	}
	t.memCand = nil
	if t.holding && t.st.Candidate != nil {
		c := *t.st.Candidate
		t.memCand = &c // the candidate it runs now (worker memory budget)
	}
	emit := changed || time.Since(t.lastEmit) >= time.Second
	if emit {
		t.lastEmit = time.Now()
	}
	s.mu.Unlock()
	if emit {
		s.ev.Changed("devices", "")
	}
}

// beforeCandidate implements DeviceTestEnv.BeforeCandidate.
func (s *Scheduler) beforeCandidate(ctx context.Context, t *devTestRun) error {
	id := t.dev.ID
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := s.readPolicyBase()
		s.mu.Lock()
		// Power management: on a machine with a battery the test also
		// yields while another device runs pipeline work (single active
		// device; devices.SelfTest Start).
		busy := s.wanted[id] || s.pipelineOnLocked(id) > 0 || !devices.SelfTest(s.policyInputLocked(&b), id).Start
		switch {
		case t.holding && !busy:
			t.memCand = nil // between two candidates: the next one is not known yet
			s.mu.Unlock()
			return nil
		case t.holding:
			t.holding, t.memCand = false, nil
			t.st.State, t.st.Phase = DeviceTestWaiting, ""
			s.mu.Unlock()
			slog.Info("device self-test yields to pipeline work", "device", id)
			s.ev.Changed("devices", "")
			s.Kick()
		default:
			s.mu.Unlock()
			if s.retakeSlot(ctx, t) {
				slog.Info("device self-test resumes", "device", id)
				s.ev.Changed("devices", "")
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.scanInterval):
		}
	}
}

// retakeSlot takes the device back for a yielded test when it is free
// (deviceFreeLocked, the worker memory budget included). It holds the scan
// lock meanwhile (TryLock: never waits for a long scan or a rerun), so no
// pipeline job can be admitted between the check and the take.
func (s *Scheduler) retakeSlot(ctx context.Context, t *devTestRun) bool {
	if !s.scanMu.TryLock() {
		return false
	}
	defer s.scanMu.Unlock()
	b := s.readPolicyBase()
	tb := s.testBudgetFor(ctx, map[string]int{}, t.dev)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.deviceFreeLocked(t.dev.ID, &b, tb) {
		return false
	}
	t.holding, t.memCand = true, nil
	t.st.State = DeviceTestRunning
	return true
}
