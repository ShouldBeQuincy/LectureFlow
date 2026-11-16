// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// SPEC-v2 I8 / I9 / I11 (phase 3b task C): multi-device scheduling, device
// binding, power awareness, per-device pause, self-tests. Written before the
// implementation (TEST FIRST, .claude/v3b-brief.md section 7 C).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// ---------- fakes ----------

// fakeDevices is a DeviceSource (and a settings.DeviceSource) over rows in
// memory, following the documented semantics of devices.Registry.
type fakeDevices struct {
	mu      sync.Mutex
	rows    map[string]model.ASRDevice
	absent  map[string]bool
	benches map[string][]model.DeviceBenchmark
}

func newFakeDevices(rows ...model.ASRDevice) *fakeDevices {
	f := &fakeDevices{rows: map[string]model.ASRDevice{}, absent: map[string]bool{}, benches: map[string][]model.DeviceBenchmark{}}
	for _, r := range rows {
		f.rows[r.ID] = r
	}
	return f
}

func (f *fakeDevices) List() []model.ASRDevice {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.ASRDevice, 0, len(f.rows))
	for _, d := range f.rows {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (f *fakeDevices) Get(id string) (model.ASRDevice, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.rows[id]
	return d, ok
}

func (f *fakeDevices) Present(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[id]
	return ok && !f.absent[id]
}

func (f *fakeDevices) Detected() bool { return true }

func (f *fakeDevices) Usable(s model.ASRSnapshot) (bool, *model.ErrorInfo) {
	s = model.NormalizeASRSnapshot(s)
	bad := &model.ErrorInfo{Code: devices.CodeSnapshotDeviceUnavailable, Params: map[string]any{"device": s.DeviceID}}
	if s.Auto {
		for _, d := range f.List() {
			if d.Enabled && f.Present(d.ID) {
				return true, nil
			}
		}
		return false, bad
	}
	if d, ok := f.Get(s.DeviceID); ok && d.Enabled && f.Present(d.ID) {
		return true, nil
	}
	return false, bad
}

func (f *fakeDevices) SetBenchmark(ctx context.Context, id string, b model.DeviceBenchmark) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.benches[id] = append(f.benches[id], b)
	return nil
}

// CurrentASRSnapshot follows devices.Registry since power management: an
// auto snapshot of the highest-priority enabled device that is present
// (else the first enabled row), over the manual list's rows in manual mode
// (a 3b device id is manual [id]).
func (f *fakeDevices) CurrentASRSnapshot(a model.ASRSettings) (model.ASRSnapshot, bool) {
	a = model.NormalizeASRDeviceMode(a)
	var first *model.ASRDevice
	for _, d := range f.List() {
		if !d.Enabled || (a.DeviceMode == model.ASRDeviceModeManual && !slices.Contains(a.ManualDevices, d.ID)) {
			continue
		}
		if f.Present(d.ID) {
			return model.ASRDeviceSnapshot(d, a.Language, true), true
		}
		if first == nil {
			d := d
			first = &d
		}
	}
	if first != nil {
		return model.ASRDeviceSnapshot(*first, a.Language, true), true
	}
	return model.ASRSnapshot{}, false
}

func (f *fakeDevices) set(id string, mut func(*model.ASRDevice)) {
	f.mu.Lock()
	d := f.rows[id]
	mut(&d)
	f.rows[id] = d
	f.mu.Unlock()
}

func (f *fakeDevices) add(d model.ASRDevice) {
	f.mu.Lock()
	f.rows[d.ID] = d
	f.mu.Unlock()
}

func (f *fakeDevices) setPresent(id string, present bool) {
	f.mu.Lock()
	f.absent[id] = !present
	f.mu.Unlock()
}

func (f *fakeDevices) benchmarks(id string) []model.DeviceBenchmark {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.DeviceBenchmark(nil), f.benches[id]...)
}

func devRow(id string, priority, maxParallel int) model.ASRDevice {
	d := model.ASRDevice{ID: id, Backend: model.DeviceBackend(id), Label: "label " + id, Enabled: true,
		MaxParallel: maxParallel, Priority: priority}
	if d.Backend == model.BackendOpenVINO {
		d.Model, d.Precision = "large-v3-turbo", model.PrecisionFP16
	} else {
		d.Model, d.Precision = "large-v3", model.PrecisionAuto
	}
	now := db.Now()
	d.DetectedAt = &now
	return d
}

func autoOn(d model.ASRDevice) model.ASRSnapshot  { return model.ASRDeviceSnapshot(d, "en", true) }
func fixedOn(d model.ASRDevice) model.ASRSnapshot { return model.ASRDeviceSnapshot(d, "en", false) }

type fakePower struct {
	mu sync.Mutex
	p  devices.Power
}

func (f *fakePower) read() devices.Power {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.p
}

func (f *fakePower) set(p devices.Power) {
	f.mu.Lock()
	f.p = p
	f.mu.Unlock()
}

var (
	// A laptop on battery (power management: a reading on battery always
	// has a battery, devices.PowerFromStatus; the 3b fixture lacked the
	// HasBattery field the policy reads) and on AC.
	onBattery  = devices.Power{Known: true, OnBattery: true, HasBattery: true}
	onACLaptop = devices.Power{Known: true, HasBattery: true}
	// A desktop on AC (no battery: the 3b rules).
	onAC = devices.Power{Known: true}
)

type fakeMem struct {
	mu  sync.Mutex
	mb  int
	err error
}

func (m *fakeMem) read() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mb, m.err
}

func (m *fakeMem) set(mb int, err error) {
	m.mu.Lock()
	m.mb, m.err = mb, err
	m.mu.Unlock()
}

// devStage is an asr stage that records the device of every run (the
// device fields of its job's snapshot, as the real stage reads them) and
// blocks until released or cancelled.
type devStage struct {
	mu     sync.Mutex
	cur    map[string]string // lecture → device of the running attempt
	starts []string          // "lecture@device" in start order
	pass   map[string]bool
	ch     map[string]chan struct{}
	fail   map[string]error // the next run of the lecture returns it
	hook   func(ctx context.Context, lectureID, device string) error
}

func newDevStage() *devStage {
	return &devStage{cur: map[string]string{}, pass: map[string]bool{}, ch: map[string]chan struct{}{}, fail: map[string]error{}}
}

func (d *devStage) stage() Stage {
	return &fakeStage{name: model.StageASR, res: ResourceGPU, run: d.run}
}

func (d *devStage) run(ctx context.Context, id string, progress func(float64)) error {
	dev := ""
	if j, ok := JobFrom(ctx); ok {
		if snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot); ok {
			dev = model.NormalizeASRSnapshot(snap).DeviceID
		}
	}
	d.mu.Lock()
	d.cur[id] = dev
	d.starts = append(d.starts, id+"@"+dev)
	err := d.fail[id]
	delete(d.fail, id)
	ch, ok := d.ch[id]
	if !ok {
		ch = make(chan struct{})
		d.ch[id] = ch
	}
	pass, hook := d.pass[id], d.hook
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.cur, id)
		d.mu.Unlock()
	}()
	progress(0.3)
	if err != nil {
		return err
	}
	if hook != nil {
		return hook(ctx, id, dev)
	}
	if pass {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release lets the lecture's current and later runs finish.
func (d *devStage) release(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.pass[id] {
		d.pass[id] = true
		if ch, ok := d.ch[id]; ok {
			close(ch)
		} else {
			d.ch[id] = closedChan()
		}
	}
}

// block makes the lecture's next run wait again.
func (d *devStage) block(id string) {
	d.mu.Lock()
	d.pass[id] = false
	d.ch[id] = make(chan struct{})
	d.mu.Unlock()
}

func (d *devStage) failNext(id string, err error) {
	d.mu.Lock()
	d.fail[id] = err
	d.mu.Unlock()
}

func (d *devStage) runningOn(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur[id]
}

// startsOf lists the devices of the lecture's runs, in start order.
func (d *devStage) startsOf(id string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, s := range d.starts {
		if l, dev, ok := strings.Cut(s, "@"); ok && l == id {
			out = append(out, dev)
		}
	}
	return out
}

// ran reports whether every run of the lecture used dev (and there was one).
func (d *devStage) ran(id, dev string) bool {
	devs := d.startsOf(id)
	for _, x := range devs {
		if x != dev {
			return false
		}
	}
	return len(devs) > 0
}

func (d *devStage) startCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.starts)
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// fakeTest is a DeviceTestFunc with `steps` candidates; each candidate
// finishes when the test sends on step.
type fakeTest struct {
	steps   int
	step    chan struct{}
	started chan model.ASRDevice
}

func newFakeTest(steps int) *fakeTest {
	return &fakeTest{steps: steps, step: make(chan struct{}), started: make(chan model.ASRDevice, 8)}
}

func (ft *fakeTest) run(ctx context.Context, dev model.ASRDevice, env DeviceTestEnv) (model.DeviceBenchmark, error) {
	ft.started <- dev
	b := model.DeviceBenchmark{Version: model.BenchmarkVersion, Status: model.BenchmarkDone, Results: []model.BenchmarkResult{}}
	for i := 0; i < ft.steps; i++ {
		if err := env.BeforeCandidate(ctx); err != nil {
			b.Status = model.BenchmarkCancelled
			return b, nil
		}
		c := devices.Candidate{Model: "large-v3-turbo", Precision: model.PrecisionFP16}
		env.Report(DeviceTestProgress{Phase: DeviceTestPhaseTranscribing, Step: i + 1, Steps: ft.steps, Candidate: c, Progress: 0.5})
		select {
		case <-ft.step:
			b.Results = append(b.Results, model.BenchmarkResult{Model: c.Model, Precision: c.Precision, OK: true, RTF: 0.1})
		case <-ctx.Done():
			b.Status = model.BenchmarkCancelled
			return b, nil
		}
	}
	return b, nil
}

// bindFailStore makes SetJobSnapshot fail (inside transactions too) while
// fail is set: the bind of the start transaction fails.
type bindFailStore struct {
	db.Store
	fail *atomic.Bool
}

func (s *bindFailStore) Tx(ctx context.Context, fn func(tx db.Store) error) error {
	return s.Store.Tx(ctx, func(tx db.Store) error { return fn(&bindFailStore{Store: tx, fail: s.fail}) })
}

func (s *bindFailStore) SetJobSnapshot(ctx context.Context, jobID, snapshot string) error {
	if s.fail.Load() {
		return errors.New("simulated snapshot write failure")
	}
	return s.Store.SetJobSnapshot(ctx, jobID, snapshot)
}

// ---------- environment ----------

type devEnv struct {
	*env
	devs   *fakeDevices
	power  *fakePower
	mem    *fakeMem
	asr    *devStage
	memEst atomic.Int64
}

func newDevEnv(t *testing.T, rows []model.ASRDevice, opt func(*Options), stages ...Stage) *devEnv {
	t.Helper()
	de := &devEnv{devs: newFakeDevices(rows...), power: &fakePower{}, mem: &fakeMem{mb: 64000}, asr: newDevStage()}
	de.memEst.Store(1000)
	if len(stages) == 0 {
		stages = []Stage{instant(model.StagePrep, ResourceCPU), de.asr.stage()}
	}
	de.env = newEnvOpts(t, nil, func(o *Options) {
		o.Devices = de.devs
		o.Power = de.power.read
		o.MemAvailMB = de.mem.read
		if opt != nil {
			opt(o)
		}
	}, stages...)
	de.wire(de.s)
	de.setASR(func(a *model.ASRSettings) {
		a.DeviceMode = model.ASRDeviceModeAuto
		a.MultiDevice = true
		a.BatteryAware = true
		a.MemReserveMB = 2048
	})
	return de
}

// wire installs the fakes a scheduler of this env needs beyond Options.
func (de *devEnv) wire(s *Scheduler) {
	s.set.SetDeviceSource(de.devs)
	s.memEstimate = func(ctx context.Context, st db.Store, dataDir string, d model.ASRDevice) int {
		return int(de.memEst.Load())
	}
}

func (de *devEnv) setASR(mut func(*model.ASRSettings)) {
	de.t.Helper()
	a := de.s.set.ASR()
	mut(&a)
	if err := de.s.set.SetASR(de.ctx, a); err != nil {
		de.t.Fatal(err)
	}
}

// lectureOn creates a launched lecture whose asr job has snap (written by
// the launch itself, so a running scheduler never sees another snapshot).
func (de *devEnv) lectureOn(snap model.ASRSnapshot) model.Lecture {
	de.t.Helper()
	l := model.Lecture{ID: db.NewID(), CourseID: de.course.ID, Title: "L", MediaPath: "m.mp4"}
	if err := de.st.CreateLecture(de.ctx, l); err != nil {
		de.t.Fatal(err)
	}
	snaps, err := de.s.set.CurrentSnapshots()
	if err != nil {
		de.t.Fatal(err)
	}
	snaps[model.StageASR] = model.MarshalSnapshot(snap)
	if err := state.Launch(de.ctx, de.st, l.ID, snaps); err != nil {
		de.t.Fatal(err)
	}
	got, err := de.st.GetLecture(de.ctx, l.ID)
	if err != nil {
		de.t.Fatal(err)
	}
	return got
}

func (de *devEnv) asrSnap(l model.Lecture) model.ASRSnapshot {
	de.t.Helper()
	s, ok := model.ParseASRSnapshot(de.lj(l.ID, model.StageASR).ConfigSnapshot)
	if !ok {
		de.t.Fatalf("asr snapshot of %s unparsable", l.ID)
	}
	return s
}

func (de *devEnv) waitRunningOn(l model.Lecture, dev string) {
	de.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if de.lj(l.ID, model.StageASR).Status == model.StatusRunning && de.asr.runningOn(l.ID) == dev {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	j := de.lj(l.ID, model.StageASR)
	de.t.Fatalf("timed out waiting for asr of %s running on %s (status %s, on %q, waiting_for %q, snapshot %s, starts %v)", l.ID, dev, j.Status, de.asr.runningOn(l.ID), de.s.WaitingFor(j), j.ConfigSnapshot, de.asr.startsOf(l.ID))
}

func (de *devEnv) waitReason(l model.Lecture, reason string) {
	de.t.Helper()
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j := de.lj(l.ID, model.StageASR)
		got = de.s.WaitingFor(j)
		if j.Status == model.StatusQueued && got == reason {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	de.t.Fatalf("asr of %s: waiting_for %q, want %q (status %s)", l.ID, got, reason, de.lj(l.ID, model.StageASR).Status)
}

// stillQueued checks over several scans that the job does not start.
func (de *devEnv) stillQueued(l model.Lecture, reason string) {
	de.t.Helper()
	de.waitReason(l, reason)
	time.Sleep(60 * time.Millisecond)
	j := de.lj(l.ID, model.StageASR)
	if j.Status != model.StatusQueued || de.s.WaitingFor(j) != reason {
		de.t.Fatalf("asr of %s: %s / %q, want queued / %q", l.ID, j.Status, de.s.WaitingFor(j), reason)
	}
}

func (de *devEnv) status(id string) DeviceStatus {
	de.t.Helper()
	for _, s := range de.s.DeviceStatuses() {
		if s.ID == id {
			return s
		}
	}
	de.t.Fatalf("no device status for %s", id)
	return DeviceStatus{}
}

func (de *devEnv) waitTest(id, state string) DeviceTestState {
	de.t.Helper()
	var st DeviceTestState
	de.waitFor("self-test of "+id+" "+state, 5*time.Second, func() bool {
		var ok bool
		st, ok = de.s.DeviceTest(id)
		return ok && st.State == state
	})
	return st
}

// ---------- I8: devices, slots, priority ----------

func TestDevicesTwoLecturesRunOnTwoDevices(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil)
	l1, l2, l3 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	de.prepDone(l1, l2, l3)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	de.waitRunningOn(l2, model.DeviceOVGPU)
	de.stillQueued(l3, model.WaitGPUSlot)

	// The device the job runs on is in its snapshot, still auto (lead ruling 1).
	want := model.ASRSnapshot{DeviceID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo",
		Precision: model.PrecisionFP16, Language: "en", Auto: true}
	if got := de.asrSnap(l2); !model.EqualASRSnapshot(got, want) {
		t.Fatalf("snapshot of the job on ov:GPU = %+v", got)
	}
	if got := de.asrSnap(l1); got.DeviceID != model.DeviceCUDA0 || !got.Auto {
		t.Fatalf("snapshot of the job on cuda:0 = %+v", got)
	}

	sts := de.s.DeviceStatuses()
	if len(sts) != 2 || sts[0].ID != model.DeviceCUDA0 || sts[1].ID != model.DeviceOVGPU {
		t.Fatalf("statuses = %+v", sts)
	}
	for _, s := range sts {
		if s.SlotsUsed != 1 || s.SlotsMax != 1 || len(s.Running) != 1 || s.Paused || s.PausedAll || s.BatteryHold || s.SelfTest != nil {
			t.Fatalf("status %s = %+v", s.ID, s)
		}
	}
	if r := de.status(model.DeviceOVGPU).Running[0]; r.JobID != de.lj(l2.ID, model.StageASR).ID || r.LectureID != l2.ID || r.StartedAt.IsZero() {
		t.Fatalf("running on ov:GPU = %+v", r)
	}

	// The first free device takes the waiting job.
	de.asr.release(l1.ID)
	de.waitRunningOn(l3, model.DeviceCUDA0)
	de.asr.release(l2.ID)
	de.asr.release(l3.ID)
	for _, l := range []model.Lecture{l1, l2, l3} {
		de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
	if s := de.status(model.DeviceCUDA0); s.SlotsUsed != 0 || len(s.Running) != 0 {
		t.Fatalf("cuda:0 after the runs = %+v", s)
	}
}

func TestDevicesMaxParallelPerDevice(t *testing.T) {
	igpu := devRow(model.DeviceOVGPU, 200, 2)
	de := newDevEnv(t, []model.ASRDevice{igpu}, nil)
	l1, l2, l3 := de.lectureOn(autoOn(igpu)), de.lectureOn(autoOn(igpu)), de.lectureOn(autoOn(igpu))
	de.prepDone(l1, l2, l3)
	de.start()
	de.waitRunningOn(l1, model.DeviceOVGPU)
	de.waitRunningOn(l2, model.DeviceOVGPU)
	de.stillQueued(l3, model.WaitGPUSlot)
	if s := de.status(model.DeviceOVGPU); s.SlotsUsed != 2 || s.SlotsMax != 2 {
		t.Fatalf("status = %+v", s)
	}
	for _, l := range []model.Lecture{l1, l2, l3} {
		de.asr.release(l.ID)
	}
	de.waitStatus(l3.ID, model.StageASR, model.StatusDone)
}

func TestDevicesMultiDeviceOff(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil)
	de.setASR(func(a *model.ASRSettings) { a.MultiDevice = false })
	l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	// ov:GPU is free, but only one asr job may run over all devices.
	de.stillQueued(l2, model.WaitGPUSlot)
	if s := de.status(model.DeviceOVGPU); s.SlotsUsed != 0 {
		t.Fatalf("ov:GPU = %+v", s)
	}
	de.asr.release(l1.ID)
	de.waitRunningOn(l2, model.DeviceCUDA0)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

func TestDevicesFixedSnapshotUsesOnlyItsDevice(t *testing.T) {
	cuda, igpu, npu, cpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1),
		devRow(model.DeviceOVNPU, 100, 1), devRow(model.DeviceOVCPU, 0, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu, cpu}, nil)
	de.devs.setPresent(model.DeviceOVCPU, false)
	l1, l2 := de.lectureOn(fixedOn(igpu)), de.lectureOn(fixedOn(igpu))
	raw := de.lj(l1.ID, model.StageASR).ConfigSnapshot
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceOVGPU)
	// cuda:0 is free: a fixed job never moves.
	de.stillQueued(l2, model.WaitGPUSlot)
	if got := de.lj(l1.ID, model.StageASR).ConfigSnapshot; got != raw {
		t.Fatalf("a fixed snapshot was rewritten: %s → %s", raw, got)
	}

	missing := de.lectureOn(fixedOn(npu)) // no ov:NPU row
	absent := de.lectureOn(fixedOn(cpu))  // row, not present
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = false })
	disabled := de.lectureOn(fixedOn(cuda))
	for _, l := range []model.Lecture{missing, absent, disabled} {
		de.stillQueued(l, model.WaitNoASRDevice)
	}
	// A v1.1 snapshot (no device_id) is cuda:0, fixed.
	de.asr.release(disabled.ID)
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = true })
	legacy := de.lectureOn(model.ASRSnapshot{Model: "large-v2", ComputeType: "auto", Device: "cuda", Language: "en"})
	legacyRaw := de.lj(legacy.ID, model.StageASR).ConfigSnapshot
	de.s.Kick()
	de.waitStatus(disabled.ID, model.StageASR, model.StatusDone)
	de.waitRunningOn(legacy, model.DeviceCUDA0)
	if got := de.lj(legacy.ID, model.StageASR).ConfigSnapshot; got != legacyRaw {
		t.Fatalf("a v1.1 snapshot was rewritten: %s", got)
	}
	for _, l := range []model.Lecture{l1, l2, legacy} {
		de.asr.release(l.ID)
	}
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	if !de.asr.ran(l2.ID, model.DeviceOVGPU) {
		t.Fatalf("l2 ran on %v", de.asr.startsOf(l2.ID))
	}
}

// device_mode = a device id (3b) is manual [id] since power management
// (power-brief §3.2): the launch writes an AUTO snapshot of that row
// (settings.CurrentSnapshots through the DeviceSource; before: a fixed
// snapshot), and the job runs only there: the manual list is the pool, a
// free cuda:0 first in priority takes nothing.
func TestDeviceModeFixedThroughSettings(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil)
	de.setASR(func(a *model.ASRSettings) { a.DeviceMode = model.DeviceOVGPU })
	if a := de.s.set.ASR(); a.DeviceMode != model.ASRDeviceModeManual || !slices.Equal(a.ManualDevices, []string{model.DeviceOVGPU}) {
		t.Fatalf("stored device mode %q %v", a.DeviceMode, a.ManualDevices)
	}
	l := de.lecture()
	if s := de.asrSnap(l); s.DeviceID != model.DeviceOVGPU || s.Backend != model.BackendOpenVINO || s.Model != igpu.Model || !s.Auto {
		t.Fatalf("launch snapshot in device mode manual [ov:GPU] = %+v", s)
	}
	l1b := de.lecture()
	de.prepDone(l, l1b)
	de.start()
	de.waitRunningOn(l, model.DeviceOVGPU)
	de.stillQueued(l1b, model.WaitGPUSlot) // cuda:0 is free, but not in the list
	de.asr.release(l.ID)
	de.asr.release(l1b.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	de.waitStatus(l1b.ID, model.StageASR, model.StatusDone)
	if !de.asr.ran(l1b.ID, model.DeviceOVGPU) {
		t.Fatalf("second lecture ran on %v", de.asr.startsOf(l1b.ID))
	}

	// auto: the launch writes an auto snapshot of the first device.
	de.setASR(func(a *model.ASRSettings) { a.DeviceMode = model.ASRDeviceModeAuto })
	l2 := de.lecture()
	if s := de.asrSnap(l2); s.DeviceID != model.DeviceCUDA0 || !s.Auto {
		t.Fatalf("launch snapshot in auto mode = %+v", s)
	}
	de.waitRunningOn(l2, model.DeviceCUDA0)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

func TestDevicesPriorityAndNewOrder(t *testing.T) {
	cuda, igpu, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{npu, igpu, cuda}, nil)
	de.start()
	l1 := de.lectureOn(autoOn(npu)) // the snapshot's device does not matter in auto mode
	de.waitRunningOn(l1, model.DeviceCUDA0)
	de.asr.release(l1.ID)
	de.waitStatus(l1.ID, model.StageASR, model.StatusDone)

	// Dragged: NPU first.
	de.devs.set(model.DeviceOVNPU, func(d *model.ASRDevice) { d.Priority = 400 })
	de.s.Kick()
	l2 := de.lectureOn(autoOn(cuda))
	de.waitRunningOn(l2, model.DeviceOVNPU)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	if sts := de.s.DeviceStatuses(); sts[0].ID != model.DeviceOVNPU {
		t.Fatalf("statuses not in the new order: %+v", sts)
	}

	// A disabled device is skipped: the next one in order.
	de.devs.set(model.DeviceOVNPU, func(d *model.ASRDevice) { d.Enabled = false })
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = false })
	l3 := de.lectureOn(autoOn(cuda))
	de.waitRunningOn(l3, model.DeviceOVGPU)
	de.asr.release(l3.ID)
	de.waitStatus(l3.ID, model.StageASR, model.StatusDone)
}

func TestNoASRDevice(t *testing.T) {
	de := newDevEnv(t, nil, nil)
	l := de.lectureOn(autoOn(devRow(model.DeviceCUDA0, 300, 1)))
	de.prepDone(l)
	de.start()
	de.stillQueued(l, model.WaitNoASRDevice)

	igpu := devRow(model.DeviceOVGPU, 200, 1)
	igpu.Enabled = false
	de.devs.add(igpu)
	de.s.Kick()
	de.stillQueued(l, model.WaitNoASRDevice) // disabled
	de.devs.set(model.DeviceOVGPU, func(d *model.ASRDevice) { d.Enabled = true })
	de.devs.setPresent(model.DeviceOVGPU, false)
	de.s.Kick()
	de.stillQueued(l, model.WaitNoASRDevice) // not present (e.g. first detection pending)
	de.devs.setPresent(model.DeviceOVGPU, true)
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceOVGPU)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

// ---------- I8: power ----------

// Power management (power-req.md part 1; replaces the 3b battery rules,
// NOTES "决定（电源管理 P1）"): a laptop on battery uses the NPU only — one
// active device, a busy NPU queues the next job (power_queue, never another
// device), jobs fixed to CUDA or the Intel GPU wait for the adapter, the CPU
// never runs; without an NPU the Intel GPU is the battery device; plugging
// in interrupts nothing and the next lecture follows the new state.
func TestBatteryModeUsesNPUOnly(t *testing.T) {
	cuda, igpu, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	// An enabled CPU device too: never used on a machine with a battery.
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu, npu, devRow(model.DeviceFWCPU, 10, 1)}, nil)
	de.power.set(onBattery)
	l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	fixedCUDA := de.lectureOn(fixedOn(cuda))
	fixedIGPU := de.lectureOn(fixedOn(igpu))
	de.prepDone(l1, l2, fixedCUDA, fixedIGPU)
	de.start()
	de.waitRunningOn(l1, model.DeviceOVNPU)
	// CUDA and the Intel GPU are free, but on battery the NPU is the one
	// active device: the next job queues for it.
	de.stillQueued(l2, model.WaitPowerQueue(model.DeviceOVNPU))
	// Jobs fixed to CUDA or to the Intel GPU (not the battery device) wait
	// for the adapter (3b: the Intel GPU one ran, the CUDA one waited
	// on_battery).
	de.stillQueued(fixedCUDA, model.WaitPowerWaitAC)
	de.stillQueued(fixedIGPU, model.WaitPowerWaitAC)
	if p := de.s.Power(); p != onBattery {
		t.Fatalf("Power() = %+v", p)
	}
	// BatteryHold: every enabled, present device but the battery device.
	for id, want := range map[string]bool{model.DeviceCUDA0: true, model.DeviceOVGPU: true, model.DeviceFWCPU: true, model.DeviceOVNPU: false} {
		if got := de.status(id).BatteryHold; got != want {
			t.Fatalf("battery hold of %s = %v", id, got)
		}
	}
	if !de.hasChanged("devices", "") {
		t.Fatal("no changed devices event for the power state")
	}
	de.asr.release(l1.ID)
	de.waitRunningOn(l2, model.DeviceOVNPU)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)

	// No NPU: the Intel GPU is the battery device; the job fixed to it is
	// first in line, the auto job queues behind it.
	de.devs.set(model.DeviceOVNPU, func(d *model.ASRDevice) { d.Enabled = false })
	l3 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(fixedIGPU, model.DeviceOVGPU)
	de.stillQueued(l3, model.WaitPowerQueue(model.DeviceOVGPU))
	if de.status(model.DeviceOVGPU).BatteryHold || !de.status(model.DeviceCUDA0).BatteryHold || de.status(model.DeviceOVNPU).BatteryHold {
		t.Fatal("battery hold without an NPU")
	}
	de.stillQueued(fixedCUDA, model.WaitPowerWaitAC)

	// Back on the mains: the Intel GPU finishes its lecture (plugging in
	// never interrupts); CUDA, the AC device, waits for the switch.
	de.power.set(onACLaptop)
	de.stillQueued(fixedCUDA, model.WaitPowerSwitchPending(model.DeviceOVGPU, model.DeviceCUDA0))
	de.stillQueued(l3, model.WaitPowerSwitchPending(model.DeviceOVGPU, model.DeviceCUDA0))
	if j := de.lj(fixedIGPU.ID, model.StageASR); j.Status != model.StatusRunning || de.asr.runningOn(fixedIGPU.ID) != model.DeviceOVGPU {
		t.Fatalf("plugging in interrupted the Intel GPU: %s", j.Status)
	}
	de.asr.release(fixedIGPU.ID)
	de.waitRunningOn(fixedCUDA, model.DeviceCUDA0)
	// CUDA is busy: the auto job queues for it, the free Intel GPU takes nothing.
	de.stillQueued(l3, model.WaitPowerQueue(model.DeviceCUDA0))
	de.asr.release(fixedCUDA.ID)
	de.waitRunningOn(l3, model.DeviceCUDA0)
	de.asr.release(l3.ID)
	de.waitStatus(l3.ID, model.StageASR, model.StatusDone)
	for _, id := range []string{l1.ID, l2.ID, fixedCUDA.ID, fixedIGPU.ID, l3.ID} {
		for _, dev := range de.asr.startsOf(id) {
			if model.DeviceKind(dev) == model.DeviceKindCPU {
				t.Fatalf("%s ran on %s", id, dev)
			}
		}
	}
}

// No Intel GPU / NPU on a laptop: on battery nothing runs (3b: the CPU
// device ran) — CUDA is never a battery device and the CPU is never used;
// battery_aware is retired (3b: off let CUDA run on battery); on AC CUDA,
// never the CPU (CUDA disabled: power_no_device). An unknown power state
// is AC power (lead ruling 5, unchanged) and a desktop keeps the 3b rules,
// CPU device included.
func TestBatteryModeWithoutIntelDevices(t *testing.T) {
	cuda, cpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVCPU, 0, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, cpu}, nil)
	de.power.set(onBattery)
	l0 := de.lectureOn(autoOn(cuda))
	de.prepDone(l0)
	de.start()
	de.stillQueued(l0, model.WaitPowerNoSaverDevice)
	if n := de.asr.startCount(); n != 0 {
		t.Fatalf("%d runs on battery without a battery device: %v", n, de.asr.startsOf(l0.ID))
	}
	// battery_aware no longer changes anything.
	de.setASR(func(a *model.ASRSettings) { a.BatteryAware = false })
	de.s.Kick()
	de.stillQueued(l0, model.WaitPowerNoSaverDevice)
	if !de.status(model.DeviceCUDA0).BatteryHold || !de.status(model.DeviceOVCPU).BatteryHold {
		t.Fatal("battery hold of CUDA / the CPU device")
	}
	de.setASR(func(a *model.ASRSettings) { a.BatteryAware = true })

	// On the mains: CUDA.
	de.power.set(onACLaptop)
	de.waitRunningOn(l0, model.DeviceCUDA0)
	de.asr.release(l0.ID)
	de.waitStatus(l0.ID, model.StageASR, model.StatusDone)
	// CUDA disabled on AC: only the CPU device is left, and it is never used.
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = false })
	l := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.stillQueued(l, model.WaitPowerNoDevice)
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = true })
	de.waitRunningOn(l, model.DeviceCUDA0)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)

	// Unknown power state = on AC power (lead ruling 5): a desktop, the 3b
	// rules — CUDA, also with a stale "on battery" flag.
	de.power.set(devices.Power{Known: false, OnBattery: true})
	l2 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceCUDA0)
	if de.status(model.DeviceCUDA0).BatteryHold {
		t.Fatal("battery hold with an unknown power state")
	}
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	// A desktop (no battery) keeps the CPU device of 3b: CUDA disabled, the
	// user's ov:CPU runs (Q1).
	de.power.set(onAC)
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = false })
	l3 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l3, model.DeviceOVCPU)
	de.asr.release(l3.ID)
	de.waitStatus(l3.ID, model.StageASR, model.StatusDone)
}

// Unplugging a laptop while CUDA transcribes (requirement 4b; 3b: the job
// finished on battery): the run is stopped at once and keeps its
// checkpoint, the job waits for the adapter (not handed to another
// device) and continues on CUDA once plugged in. A desktop (no battery)
// never interrupts a run.
func TestBatteryModeLetsRunningCUDAJobFinish(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda}, nil)
	de.power.set(onACLaptop)
	l := de.lectureOn(autoOn(cuda))
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, model.DeviceCUDA0)
	de.power.set(onBattery)
	de.waitReason(l, model.WaitPowerWaitAC)
	j := de.lj(l.ID, model.StageASR)
	if j.Stats.PowerInterrupts != 1 || j.Stats.ASRResume == nil || j.Stats.ASRResume.DeviceID != model.DeviceCUDA0 || (j.Error != nil && *j.Error != "") {
		t.Fatalf("suspended job: %+v / error %v", j.Stats, j.Error)
	}
	l2 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.stillQueued(l2, model.WaitPowerNoSaverDevice)
	de.stillQueued(l, model.WaitPowerWaitAC)
	de.power.set(onACLaptop)
	de.waitRunningOn(l, model.DeviceCUDA0)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	de.waitRunningOn(l2, model.DeviceCUDA0)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)

	// A desktop: a power reading that changes (here: it became unknown)
	// interrupts nothing.
	de.power.set(onAC)
	l3 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l3, model.DeviceCUDA0)
	de.power.set(devices.Power{})
	de.waitFor("unknown state seen", 5*time.Second, func() bool { return de.s.Power() == devices.Power{} })
	time.Sleep(40 * time.Millisecond)
	if j := de.lj(l3.ID, model.StageASR); j.Status != model.StatusRunning || j.Stats.PowerInterrupts != 0 {
		t.Fatalf("desktop run after a power change: %s %+v", j.Status, j.Stats)
	}
	de.asr.release(l3.ID)
	de.waitStatus(l3.ID, model.StageASR, model.StatusDone)
}

// ---------- I8: pause ----------

func TestDevicePause(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil)
	if err := de.s.SetDevicePaused(model.DeviceCUDA0, true, false); err != nil {
		t.Fatal(err)
	}
	if !de.s.DevicePaused(model.DeviceCUDA0) || de.s.DevicePaused(model.DeviceOVGPU) || de.s.GPUPaused() {
		t.Fatal("pause flags")
	}
	if v, _, _ := de.st.GetKV(de.ctx, model.KVASRDevicePausedPrefix+model.DeviceCUDA0); v != "1" {
		t.Fatalf("kv = %q", v)
	}
	if !de.hasChanged("devices", "") {
		t.Fatal("no changed devices event")
	}
	l1 := de.lectureOn(autoOn(cuda))
	fixed := de.lectureOn(fixedOn(cuda))
	de.prepDone(l1, fixed)
	de.start()
	de.waitRunningOn(l1, model.DeviceOVGPU) // cuda:0 is paused: the next device
	de.stillQueued(fixed, model.WaitDevicePaused(model.DeviceCUDA0))
	if s := de.status(model.DeviceCUDA0); !s.Paused || s.PausedAll {
		t.Fatalf("cuda:0 status = %+v", s)
	}

	// Every device the job may use is paused: the highest-priority one is named.
	if err := de.s.SetDevicePaused(model.DeviceOVGPU, true, false); err != nil {
		t.Fatal(err)
	}
	l2 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.stillQueued(l2, model.WaitDevicePaused(model.DeviceCUDA0))
	if j := de.lj(l1.ID, model.StageASR); j.Status != model.StatusRunning {
		t.Fatalf("pausing without cancel stopped the running job: %s", j.Status)
	}

	// Persisted: a new scheduler starts with the switch on.
	s2 := de.newScheduler()
	if !s2.DevicePaused(model.DeviceCUDA0) || !s2.DevicePaused(model.DeviceOVGPU) {
		t.Fatal("device pause not loaded from kv")
	}

	// cancel_running cancels the jobs of that device only.
	if err := de.s.SetDevicePaused(model.DeviceOVGPU, true, true); err != nil {
		t.Fatal(err)
	}
	if j := de.lj(l1.ID, model.StageASR); j.Status != model.StatusCancelled {
		t.Fatalf("job on the paused device: %s", j.Status)
	}
	if err := de.s.SetDevicePaused(model.DeviceCUDA0, false, false); err != nil {
		t.Fatal(err)
	}
	de.waitRunningOn(fixed, model.DeviceCUDA0)
	if v, _, _ := de.st.GetKV(de.ctx, model.KVASRDevicePausedPrefix+model.DeviceCUDA0); v == "1" {
		t.Fatalf("kv still %q after unpause", v)
	}
	if err := de.s.SetDevicePaused(model.DeviceOVGPU, false, false); err != nil {
		t.Fatal(err)
	}
	de.waitRunningOn(l2, model.DeviceOVGPU)

	if err := de.s.SetDevicePaused("ov:XPU", true, false); !errors.Is(err, ErrUnknownDevice) || !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	de.asr.release(fixed.ID)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

func TestGlobalPauseStopsAllDevices(t *testing.T) {
	cuda, igpu, npu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	ft := newFakeTest(1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu, npu}, func(o *Options) { o.DeviceTest = ft.run })
	l1, l2 := de.lectureOn(fixedOn(cuda)), de.lectureOn(fixedOn(igpu))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	de.waitRunningOn(l2, model.DeviceOVGPU)
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)

	// The top-bar switch with cancel_running: every device's jobs and the self-test.
	if err := de.s.SetGPUPaused(true, true); err != nil {
		t.Fatal(err)
	}
	for _, l := range []model.Lecture{l1, l2} {
		if j := de.lj(l.ID, model.StageASR); j.Status != model.StatusCancelled {
			t.Fatalf("%s after global pause+cancel: %s", l.ID, j.Status)
		}
	}
	de.waitTest(model.DeviceOVNPU, DeviceTestCancelled)
	l3 := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.stillQueued(l3, model.WaitGPUPaused)
	for _, s := range de.s.DeviceStatuses() {
		if !s.PausedAll || s.Paused {
			t.Fatalf("status %s = %+v", s.ID, s)
		}
	}
	// No self-test starts while everything is paused.
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVNPU); st.State != DeviceTestWaiting {
		t.Fatalf("self-test while paused: %s", st.State)
	}
	if err := de.s.SetGPUPaused(false, false); err != nil {
		t.Fatal(err)
	}
	de.waitRunningOn(l3, model.DeviceCUDA0)
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVNPU, DeviceTestDone)
	de.asr.release(l3.ID)
	de.waitStatus(l3.ID, model.StageASR, model.StatusDone)
}

// ---------- I8: admission ----------

func TestDevicesVRAMAdmission(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 2)
	de := newDevEnv(t, []model.ASRDevice{cuda}, nil)
	de.setASR(func(a *model.ASRSettings) { a.VRAMPerJobMB = 5000; a.VRAMReserveMB = 1000 })
	de.gpu.set(5500, 24000, 18500) // 5500 - 1000 < 5000
	l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	de.prepDone(l1, l2)
	de.start()
	de.stillQueued(l1, model.WaitVRAM)
	de.stillQueued(l2, model.WaitVRAM)

	// Room for one: the second keeps waiting, the first's estimate is reserved.
	de.gpu.set(10000, 24000, 14000)
	de.waitRunningOn(l1, model.DeviceCUDA0)
	de.stillQueued(l2, model.WaitVRAM)

	// An Intel device does not need VRAM: the waiting job goes there.
	de.devs.add(devRow(model.DeviceOVGPU, 200, 1))
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceOVGPU)
	de.asr.release(l1.ID)
	de.asr.release(l2.ID)
	de.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

// The VRAM calibration key is the model and resolved compute type of the
// device the job ran on; only runs on the same card count as overlapping.
func TestDevicesVRAMCalibrationPerCard(t *testing.T) {
	var de *devEnv
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	cuda.Model, cuda.Precision = "large-v2", model.PrecisionFloat16
	de = newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil)
	// Not fixed sleeps (under load the CUDA run could see no VRAM sample, or
	// the two runs not overlap): the CUDA run lasts until it was sampled and
	// the other run has started; the other run lasts until the CUDA run ended.
	otherStarted, cudaDone := make(chan struct{}), make(chan struct{})
	de.asr.hook = func(ctx context.Context, id, dev string) error {
		if dev == model.DeviceCUDA0 {
			defer close(cudaDone)
			de.gpu.set(14000, 24000, 7000) // +6000 MB over the 1000 MB baseline
			err := de.waitSampledPeak(id, 6000)
			select {
			case <-otherStarted:
			case <-ctx.Done():
				err = ctx.Err()
			}
			de.gpu.set(20000, 24000, 1000)
			return err
		}
		close(otherStarted)
		select { // overlaps the CUDA run on another device
		case <-cudaDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	l1, l2 := de.lectureOn(autoOn(cuda)), de.lectureOn(autoOn(cuda))
	de.prepDone(l1, l2)
	de.start()
	de.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	if dev := de.asrSnap(l2).DeviceID; dev != model.DeviceOVGPU {
		t.Fatalf("second job ran on %s", dev)
	}
	key := model.KVVRAMObservedPrefix + "large-v2:float16"
	if v, ok, _ := de.st.GetKV(de.ctx, key); !ok || v != "6000" {
		t.Fatalf("%s = %q (ok=%v): a run on another device must not count as overlapping", key, v, ok)
	}
}

func TestDevicesMemoryAdmission(t *testing.T) {
	igpu, npu := devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{igpu, npu}, nil)
	de.memEst.Store(3000)
	de.mem.set(6000, nil)
	// l2 is fixed to the NPU: an auto job would report the reason of its
	// highest-priority device (the Intel GPU, full: gpu_slot).
	l1, l2 := de.lectureOn(autoOn(igpu)), de.lectureOn(fixedOn(npu))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceOVGPU)
	// 6000 - 2048 (mem_reserve_mb) - 3000 (l1 has not used its estimate yet) < 3000
	de.stillQueued(l2, model.WaitMemory)

	// l1's worker took 2500 MB: 500 MB of its estimate stay reserved.
	de.mem.set(3500, nil)
	de.waitFor("memory sampled", 5*time.Second, func() bool {
		de.s.mu.Lock()
		defer de.s.mu.Unlock()
		for _, r := range de.s.running {
			if r.device == model.DeviceOVGPU && r.memPeakMB >= 2500 {
				return true
			}
		}
		return false
	})
	de.stillQueued(l2, model.WaitMemory)
	// 5600 - 2048 - 500 >= 3000 (without the sampled use it would be 552)
	de.mem.set(5600, nil)
	de.waitRunningOn(l2, model.DeviceOVNPU)

	// mem_reserve_mb comes from the settings.
	de.asr.release(l1.ID)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	de.setASR(func(a *model.ASRSettings) { a.MemReserveMB = 4000 })
	de.mem.set(6000, nil)
	l3 := de.lectureOn(autoOn(igpu))
	de.s.Kick()
	de.stillQueued(l3, model.WaitMemory) // 6000 - 4000 < 3000 on both devices
	de.setASR(func(a *model.ASRSettings) { a.MemReserveMB = 1000 })
	de.s.Kick()
	de.waitRunningOn(l3, model.DeviceOVGPU)
	de.asr.release(l3.ID)
	de.waitStatus(l3.ID, model.StageASR, model.StatusDone)
}

func TestDevicesMemoryReadFailureAllowsOne(t *testing.T) {
	igpu, npu := devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{igpu, npu}, nil)
	de.mem.set(0, errors.New("GlobalMemoryStatusEx failed"))
	l1, l2 := de.lectureOn(autoOn(igpu)), de.lectureOn(fixedOn(npu))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceOVGPU)
	de.stillQueued(l2, model.WaitMemory)
	de.asr.release(l1.ID)
	de.waitRunningOn(l2, model.DeviceOVNPU)
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

// ---------- binding (lead ruling 1) ----------

func TestBindFailureCommitsNothing(t *testing.T) {
	var fail atomic.Bool
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda}, func(o *Options) { o.Store = &bindFailStore{Store: o.Store, fail: &fail} })
	igpu := devRow(model.DeviceOVGPU, 200, 1)
	l := de.lectureOn(autoOn(igpu)) // launched while ov:GPU was the first device
	raw := de.lj(l.ID, model.StageASR).ConfigSnapshot
	de.prepDone(l)
	fail.Store(true)
	de.start()
	time.Sleep(80 * time.Millisecond) // many scans, every start transaction fails
	j := de.lj(l.ID, model.StageASR)
	if j.Status != model.StatusQueued || j.ConfigSnapshot != raw || j.StartedAt != nil && *j.StartedAt != "" {
		t.Fatalf("failed bind left %s %s started_at=%v", j.Status, j.ConfigSnapshot, j.StartedAt)
	}
	if n := de.asr.startCount(); n != 0 {
		t.Fatalf("stage ran %d times without its device in the snapshot", n)
	}
	if s := de.status(model.DeviceCUDA0); s.SlotsUsed != 0 {
		t.Fatalf("slot kept after the failed start: %+v", s)
	}
	fail.Store(false)
	de.waitRunningOn(l, model.DeviceCUDA0)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || !s.Auto {
		t.Fatalf("snapshot = %+v", s)
	}
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

func TestRebindOnRetryAndRecovery(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil)
	l := de.lectureOn(autoOn(cuda))
	de.asr.failNext(l.ID, Permanent(errors.New("worker exit code 2")))
	de.prepDone(l)
	de.start()
	de.waitStatus(l.ID, model.StageASR, model.StatusFailed)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 {
		t.Fatalf("bound to %s", s.DeviceID)
	}
	// The user retries after cuda:0 is disabled: the retry binds again.
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = false })
	if err := de.s.Retry(de.ctx, model.ScopeLecture, l.ID, model.StageASR, ""); err != nil {
		t.Fatal(err)
	}
	de.waitRunningOn(l, model.DeviceOVGPU)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceOVGPU || !s.Auto || s.Model != igpu.Model || s.Precision != igpu.Precision {
		t.Fatalf("after retry: %+v", s)
	}

	// Restart while it runs (§4.7): recovery requeues it, the next start binds again.
	de.s.Stop()
	if err := state.Recover(de.ctx, de.st, de.dataDir); err != nil {
		t.Fatal(err)
	}
	if j := de.lj(l.ID, model.StageASR); j.Status != model.StatusQueued {
		t.Fatalf("after recover: %s", j.Status)
	}
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = true })
	de.s = de.newScheduler(instant(model.StagePrep, ResourceCPU), de.asr.stage())
	de.wire(de.s)
	de.start()
	de.waitRunningOn(l, model.DeviceCUDA0)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || !s.Auto {
		t.Fatalf("after recovery: %+v", s)
	}
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

// ---------- A4.3 with devices (lead ruling 2, refined 2026-10-24) ----------

// a43Env runs whole lectures: prep instant, asr through a devStage whose
// input hash (like hash_version 3) reads only the wav and the language.
func a43Env(t *testing.T) (*devEnv, *devStage) {
	t.Helper()
	asr := newDevStage()
	hashed := &fakeStage{name: model.StageASR, res: ResourceGPU, run: asr.run, hash: func(ctx context.Context, id string) (string, error) {
		return "wav+en", nil
	}}
	de := newDevEnv(t, []model.ASRDevice{devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)}, nil, allInstant(hashed)...)
	de.asr = asr
	de.setASR(func(a *model.ASRSettings) {
		a.Model, a.ComputeType, a.Device, a.Language = "large-v3", "auto", "cuda", "en"
	})
	return de, asr
}

func (de *devEnv) rerunASR(l model.Lecture, choice string) error {
	return de.s.Rerun(de.ctx, model.ScopeLecture, l.ID, model.StageASR, choice)
}

// The device the scheduler picked in auto mode is not the user's change: a
// rerun asks nothing, and nothing becomes stale.
func TestAutoSnapshotSchedulerChoiceNeverAsks(t *testing.T) {
	de, asr := a43Env(t)
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = false })
	l := de.lectureOn(autoOn(devRow(model.DeviceCUDA0, 300, 1)))
	asr.release(l.ID)
	de.start()
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceOVGPU || !s.Auto {
		t.Fatalf("ran with %+v", s)
	}
	// cuda:0, the first device, is back: the run's device is not the first one.
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = true })
	if _, err := de.s.CheckStale(de.ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if j := de.lj(l.ID, model.StageASR); j.Status != model.StatusDone {
		t.Fatalf("asr = %s after the device change", j.Status)
	}
	ran := de.lj(l.ID, model.StageASR)
	if err := de.rerunASR(l, ""); err != nil {
		t.Fatalf("rerun after the scheduler's own device choice: %v", err)
	}
	if j := de.lj(l.ID, model.StageASR); j.ID == ran.ID || j.ConfigSnapshot != ran.ConfigSnapshot {
		t.Fatalf("rerun: id %s→%s snapshot %s", ran.ID, j.ID, j.ConfigSnapshot)
	}
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || !s.Auto {
		t.Fatalf("the rerun (still auto) ran with %+v", s)
	}
	// UpdateLectures asks nothing either.
	if _, err := de.s.UpdateLectures(de.ctx, []string{l.ID}, ""); err != nil {
		t.Fatal(err)
	}

	// The language is the user's change: it asks.
	de.setASR(func(a *model.ASRSettings) { a.Language = "de" })
	diffs := needsChoice(t, de.rerunASR(l, ""))
	if len(diffs) != 1 || diffs[0].Stage != model.StageASR || !diffs[0].SnapshotUsable {
		t.Fatalf("diffs = %+v", diffs)
	}
	if cur, _ := model.ParseASRSnapshot(diffs[0].Current); cur.Language != "de" || cur.DeviceID != model.DeviceCUDA0 {
		t.Fatalf("current = %s", diffs[0].Current)
	}
}

// The bound device's own configuration changed, or the device is gone: the
// rerun asks.
func TestAutoSnapshotBoundDeviceChangeAsks(t *testing.T) {
	de, asr := a43Env(t)
	l := de.lectureOn(autoOn(devRow(model.DeviceOVGPU, 200, 1)))
	asr.release(l.ID)
	de.start()
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	ran := de.lj(l.ID, model.StageASR)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || s.Precision != model.PrecisionAuto || !s.Auto {
		t.Fatalf("ran with %+v", s)
	}

	// Its precision is changed in the settings.
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Precision = model.PrecisionFloat16 })
	diffs := needsChoice(t, de.rerunASR(l, ""))
	if len(diffs) != 1 || diffs[0].Stage != model.StageASR || diffs[0].Snapshot != ran.ConfigSnapshot || !diffs[0].SnapshotUsable {
		t.Fatalf("diffs = %+v", diffs)
	}
	if cur, _ := model.ParseASRSnapshot(diffs[0].Current); cur.DeviceID != model.DeviceCUDA0 || cur.Precision != model.PrecisionFloat16 {
		t.Fatalf("current = %s, want cuda:0 with the new precision", diffs[0].Current)
	}
	// 沿用原模型: fixed to the device and precision it ran with.
	if err := de.rerunASR(l, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || s.Auto || s.Precision != model.PrecisionAuto {
		t.Fatalf("pinned snapshot = %+v", s)
	}
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)

	// Back to auto with the current settings, then the bound device is disabled.
	if err := de.rerunASR(l, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || !s.Auto || s.Precision != model.PrecisionFloat16 {
		t.Fatalf("current choice ran with %+v", s)
	}
	if err := de.rerunASR(l, ""); err != nil {
		t.Fatalf("nothing changed since: %v", err)
	}
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	de.devs.set(model.DeviceCUDA0, func(d *model.ASRDevice) { d.Enabled = false })
	diffs = needsChoice(t, de.rerunASR(l, ""))
	if len(diffs) != 1 || diffs[0].SnapshotUsable || diffs[0].ReasonInfo == nil || diffs[0].ReasonInfo.Code != devices.CodeSnapshotDeviceUnavailable {
		t.Fatalf("diffs with the bound device disabled = %+v", diffs)
	}
	if err := de.rerunASR(l, model.ModelChoiceSnapshot); !errors.Is(err, state.ErrSnapshotUnusable) {
		t.Fatalf("沿用原模型 with the bound device disabled: %v", err)
	}
	if err := de.rerunASR(l, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceOVGPU || !s.Auto {
		t.Fatalf("current choice ran with %+v", s)
	}
}

// SPEC-v2 I11 with power management (lead decision A, 2026-10-24; ruling
// 9.2): the user sets the device mode to one device (manual [id]); a rerun
// compares effective configurations only, so it asks when the list's
// effective device differs from the device the job ran on, and not when
// the run already had that configuration (before power management a device
// id always asked: auto became fixed).
func TestDeviceModeChangeRerunAsks(t *testing.T) {
	de, asr := a43Env(t)
	cudaRow, igpuRow := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	l := de.lectureOn(autoOn(cudaRow))
	asr.release(l.ID)
	de.start()
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	ran := de.lj(l.ID, model.StageASR)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || !s.Auto {
		t.Fatalf("ran with %+v", s)
	}
	// manual [ov:GPU], in both forms (the list, and a 3b device id): the
	// list's effective device is the Intel GPU → the rerun asks; current
	// is its auto snapshot (manual writes auto snapshots, power-brief §3.2).
	for _, set := range []func(a *model.ASRSettings){
		func(a *model.ASRSettings) {
			a.DeviceMode, a.ManualDevices = model.ASRDeviceModeManual, []string{model.DeviceOVGPU}
		},
		func(a *model.ASRSettings) { a.DeviceMode, a.ManualDevices = model.DeviceOVGPU, nil },
	} {
		de.setASR(set)
		diffs := needsChoice(t, de.rerunASR(l, ""))
		if len(diffs) != 1 || diffs[0].Stage != model.StageASR || diffs[0].Snapshot != ran.ConfigSnapshot || !diffs[0].SnapshotUsable {
			t.Fatalf("manual [ov:GPU]: diffs = %+v", diffs)
		}
		if cur, _ := model.ParseASRSnapshot(diffs[0].Current); cur.DeviceID != model.DeviceOVGPU || !cur.Auto ||
			cur.Model != igpuRow.Model || cur.Precision != igpuRow.Precision || cur.Language != "en" {
			t.Fatalf("manual [ov:GPU]: current = %s", diffs[0].Current)
		}
		if j := de.lj(l.ID, model.StageASR); j.ID != ran.ID {
			t.Fatal("a needs_model_choice must not change anything")
		}
	}

	// manual [cuda:0]: the run already had the list's effective
	// configuration → no question; the rerun runs, bound to cuda:0 again
	// with the same effective configuration.
	de.setASR(func(a *model.ASRSettings) { a.DeviceMode, a.ManualDevices = model.DeviceCUDA0, nil })
	if err := de.rerunASR(l, ""); err != nil {
		t.Fatalf("manual [cuda:0], same effective configuration: %v", err)
	}
	if j := de.lj(l.ID, model.StageASR); j.ID == ran.ID || j.ConfigSnapshot != ran.ConfigSnapshot {
		t.Fatalf("rerun: id %s→%s snapshot %s", ran.ID, j.ID, j.ConfigSnapshot)
	}
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || !s.Auto || !model.SameSnapshot(model.StageASR, model.MarshalSnapshot(s), ran.ConfigSnapshot) {
		t.Fatalf("the rerun ran with %+v (before %s)", s, ran.ConfigSnapshot)
	}
	if !asr.ran(l.ID, model.DeviceCUDA0) {
		t.Fatalf("runs: %v", asr.startsOf(l.ID))
	}
	de.setASR(func(a *model.ASRSettings) { a.DeviceMode, a.ManualDevices = model.DeviceOVGPU, nil })

	// 沿用原模型: fixed to the device it ran on (cuda:0), not the mode's.
	if err := de.rerunASR(l, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	if s := de.asrSnap(l); s.DeviceID != model.DeviceCUDA0 || s.Auto {
		t.Fatalf("pinned snapshot = %+v", s)
	}
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if !asr.ran(l.ID, model.DeviceCUDA0) {
		t.Fatalf("runs: %v", asr.startsOf(l.ID))
	}

	// The pinned device fails and then disappears: retry asks (409).
	asr.failNext(l.ID, Permanent(errors.New("device lost")))
	if err := de.rerunASR(l, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	de.waitStatus(l.ID, model.StageASR, model.StatusFailed)
	de.devs.setPresent(model.DeviceCUDA0, false)
	diffs := needsChoice(t, de.s.Retry(de.ctx, model.ScopeLecture, l.ID, model.StageASR, ""))
	if len(diffs) != 1 || diffs[0].Stage != model.StageASR || diffs[0].SnapshotUsable || diffs[0].ReasonInfo == nil ||
		diffs[0].ReasonInfo.Code != devices.CodeSnapshotDeviceUnavailable {
		t.Fatalf("diffs = %+v", diffs)
	}
	if err := de.s.Retry(de.ctx, model.ScopeLecture, l.ID, model.StageASR, model.ModelChoiceSnapshot); !errors.Is(err, state.ErrSnapshotUnusable) {
		t.Fatalf("snapshot choice with the device gone: %v", err)
	}
	if err := de.s.Retry(de.ctx, model.ScopeLecture, l.ID, model.StageASR, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	de.waitStatus(l.ID, model.StageExport, model.StatusDone)
	// 使用当前设置: the manual list's device, an auto snapshot (power-brief §3.2).
	if s := de.asrSnap(l); s.DeviceID != model.DeviceOVGPU || !s.Auto {
		t.Fatalf("current choice (device mode manual [ov:GPU]) ran with %+v", s)
	}
}

// An auto snapshot is usable while some enabled device is present.
func TestAutoSnapshotRetryNeedsAnyDevice(t *testing.T) {
	igpu := devRow(model.DeviceOVGPU, 200, 1)
	de := newDevEnv(t, []model.ASRDevice{igpu}, nil)
	l := de.lectureOn(autoOn(igpu))
	de.asr.failNext(l.ID, Permanent(errors.New("boom")))
	de.prepDone(l)
	de.start()
	de.waitStatus(l.ID, model.StageASR, model.StatusFailed)
	de.devs.set(model.DeviceOVGPU, func(d *model.ASRDevice) { d.Enabled = false })
	diffs := needsChoice(t, de.s.Retry(de.ctx, model.ScopeLecture, l.ID, model.StageASR, ""))
	if len(diffs) != 1 || diffs[0].SnapshotUsable {
		t.Fatalf("diffs = %+v", diffs)
	}
	de.devs.add(devRow(model.DeviceOVNPU, 100, 1))
	if err := de.s.Retry(de.ctx, model.ScopeLecture, l.ID, model.StageASR, ""); err != nil {
		t.Fatal(err)
	}
	de.waitRunningOn(l, model.DeviceOVNPU)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

// ---------- ruling 9: cancel mid-transcription ----------

func TestCancelMidTranscriptionFreesDevice(t *testing.T) {
	npu := devRow(model.DeviceOVNPU, 100, 1)
	de := newDevEnv(t, []model.ASRDevice{npu}, nil)
	out := func(id string) string { return filepath.Join(de.dataDir, "worker-"+id) }
	// A worker that writes result.json.tmp and is killed on cancel.
	de.asr.hook = func(ctx context.Context, id, dev string) error {
		os.MkdirAll(out(id), 0o755)
		tmp := filepath.Join(out(id), "result.json.tmp")
		if err := os.WriteFile(tmp, []byte("{}"), 0o644); err != nil {
			return err
		}
		de.asr.mu.Lock()
		ch := de.asr.ch[id]
		de.asr.mu.Unlock()
		select {
		case <-ctx.Done():
			os.Remove(tmp) // the killed process leaves nothing; the stage removes the .tmp
			return ctx.Err()
		case <-ch:
			return os.Rename(tmp, filepath.Join(out(id), "result.json"))
		}
	}
	l1, l2 := de.lectureOn(fixedOn(npu)), de.lectureOn(fixedOn(npu))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceOVNPU)
	de.waitFor("worker output started", 5*time.Second, func() bool { return exists(filepath.Join(out(l1.ID), "result.json.tmp")) })
	de.stillQueued(l2, model.WaitGPUSlot)

	t0 := time.Now()
	if err := de.s.Cancel(de.lj(l1.ID, model.StageASR).ID); err != nil {
		t.Fatal(err)
	}
	if j := de.lj(l1.ID, model.StageASR); j.Status != model.StatusCancelled || time.Since(t0) > 3*time.Second {
		t.Fatalf("cancel: %s after %v", j.Status, time.Since(t0))
	}
	if exists(filepath.Join(out(l1.ID), "result.json")) || exists(filepath.Join(out(l1.ID), "result.json.tmp")) {
		t.Fatal("a cancelled run left output")
	}
	// The slot is free and the next job uses the same device.
	de.waitRunningOn(l2, model.DeviceOVNPU)
	if s := de.status(model.DeviceOVNPU); s.SlotsUsed != 1 || len(s.Running) != 1 || s.Running[0].LectureID != l2.ID {
		t.Fatalf("status = %+v", s)
	}
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	if !exists(filepath.Join(out(l2.ID), "result.json")) {
		t.Fatal("the next job on the device produced no output")
	}
}

// ---------- I9: self-tests ----------

func TestSelfTestOccupiesSlotYieldsAndStoresBenchmark(t *testing.T) {
	// ov:GPU first, so a job's highest-priority candidate is the tested device.
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 400, 1)
	ft := newFakeTest(2)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, func(o *Options) { o.DeviceTest = ft.run })
	de.start()
	st, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU)
	if err != nil {
		t.Fatal(err)
	}
	if st.DeviceID != model.DeviceOVGPU || (st.State != DeviceTestWaiting && st.State != DeviceTestRunning) {
		t.Fatalf("start = %+v", st)
	}
	run := de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	if run.StartedAt.IsZero() {
		t.Fatal("running test without started_at")
	}
	if dev := <-ft.started; dev.ID != model.DeviceOVGPU {
		t.Fatalf("test got device %+v", dev)
	}
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); !errors.Is(err, ErrSelfTestRunning) || !errors.Is(err, state.ErrConflict) {
		t.Fatalf("second start: %v", err)
	}
	s := de.status(model.DeviceOVGPU)
	if s.SlotsUsed != 1 || s.SelfTest == nil || s.SelfTest.State != DeviceTestRunning || len(s.Running) != 0 {
		t.Fatalf("status = %+v", s)
	}
	de.waitFor("progress reported", 5*time.Second, func() bool {
		st, _ := de.s.DeviceTest(model.DeviceOVGPU)
		return st.Step == 1 && st.Steps == 2 && st.Phase == DeviceTestPhaseTranscribing && st.Candidate != nil && st.Progress == 0.5
	})

	// An auto job goes to the other device. A second one has two candidates
	// (ov:GPU testing, cuda:0 busy): it waits for the test, which is not
	// preempted for it (it could also take cuda:0; Gate round 1, item 8)
	// but yields between two candidates.
	auto := de.lectureOn(autoOn(igpu))
	de.s.Kick()
	de.waitRunningOn(auto, model.DeviceCUDA0)
	fixed := de.lectureOn(autoOn(igpu)) // (named for its role before item 8: the job that waits for the test)
	de.s.Kick()
	de.waitReason(fixed, model.WaitDeviceSelfTest(model.DeviceOVGPU))
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVGPU); st.State != DeviceTestRunning || st.Step != 1 {
		t.Fatalf("a job with another candidate preempted the test: %+v", st)
	}

	// Between two candidates the test gives the slot to the waiting job.
	ft.step <- struct{}{}
	de.waitRunningOn(fixed, model.DeviceOVGPU)
	if st, _ := de.s.DeviceTest(model.DeviceOVGPU); st.State != DeviceTestWaiting {
		t.Fatalf("yielded test = %s", st.State)
	}
	if s := de.status(model.DeviceOVGPU); s.SlotsUsed != 1 || len(s.Running) != 1 {
		t.Fatalf("status while yielded = %+v", s)
	}
	de.asr.release(fixed.ID)
	de.waitStatus(fixed.ID, model.StageASR, model.StatusDone)
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	ft.step <- struct{}{}
	done := de.waitTest(model.DeviceOVGPU, DeviceTestDone)
	if done.Result == nil || done.Result.Status != model.BenchmarkDone || len(done.Result.Results) != 2 || done.FinishedAt.IsZero() {
		t.Fatalf("done = %+v", done)
	}
	bs := de.devs.benchmarks(model.DeviceOVGPU)
	if len(bs) != 1 || bs[0].Status != model.BenchmarkDone || len(bs[0].Results) != 2 {
		t.Fatalf("stored benchmarks = %+v", bs)
	}
	if s := de.status(model.DeviceOVGPU); s.SlotsUsed != 0 || s.SelfTest == nil || s.SelfTest.State != DeviceTestDone {
		t.Fatalf("status after the test = %+v", s)
	}
	if !de.hasChanged("devices", "") {
		t.Fatal("no changed devices event")
	}
	de.asr.release(auto.ID)
	de.waitStatus(auto.ID, model.StageASR, model.StatusDone)

	// A finished test can be started again.
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	ft.step <- struct{}{}
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)
}

func TestSelfTestWaitsForPipelineWork(t *testing.T) {
	igpu := devRow(model.DeviceOVGPU, 200, 2)
	ft := newFakeTest(1)
	de := newDevEnv(t, []model.ASRDevice{igpu}, func(o *Options) { o.DeviceTest = ft.run })
	l := de.lectureOn(fixedOn(igpu))
	de.prepDone(l)
	de.start()
	de.waitRunningOn(l, model.DeviceOVGPU)
	// A pipeline job runs on the device (one slot is still free): the test waits.
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVGPU); st.State != DeviceTestWaiting || !st.StartedAt.IsZero() {
		t.Fatalf("test with a pipeline job on the device: %+v", st)
	}
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)

	// A paused device starts no test.
	if err := de.s.SetDevicePaused(model.DeviceOVGPU, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if st, _ := de.s.DeviceTest(model.DeviceOVGPU); st.State != DeviceTestWaiting {
		t.Fatalf("test on a paused device: %s", st.State)
	}
	if err := de.s.SetDevicePaused(model.DeviceOVGPU, false, false); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)
}

func TestSelfTestCancel(t *testing.T) {
	igpu, npu := devRow(model.DeviceOVGPU, 200, 1), devRow(model.DeviceOVNPU, 100, 1)
	ft := newFakeTest(2)
	de := newDevEnv(t, []model.ASRDevice{igpu, npu}, func(o *Options) { o.DeviceTest = ft.run })
	de.start()
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	ft.step <- struct{}{} // one candidate done
	de.waitFor("second candidate", 5*time.Second, func() bool { st, _ := de.s.DeviceTest(model.DeviceOVGPU); return st.Step == 2 })
	t0 := time.Now()
	st, err := de.s.CancelDeviceTest(model.DeviceOVGPU)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != DeviceTestCancelled || time.Since(t0) > 3*time.Second || st.Result == nil ||
		st.Result.Status != model.BenchmarkCancelled || len(st.Result.Results) != 1 || st.FinishedAt.IsZero() {
		t.Fatalf("cancel = %+v after %v", st, time.Since(t0))
	}
	// The partial result is stored.
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 1 || bs[0].Status != model.BenchmarkCancelled || len(bs[0].Results) != 1 {
		t.Fatalf("stored = %+v", bs)
	}
	if s := de.status(model.DeviceOVGPU); s.SlotsUsed != 0 {
		t.Fatalf("slot after cancel: %+v", s)
	}
	// Idempotent.
	if again, err := de.s.CancelDeviceTest(model.DeviceOVGPU); err != nil || again.State != DeviceTestCancelled {
		t.Fatalf("second cancel: %+v %v", again, err)
	}
	if len(de.devs.benchmarks(model.DeviceOVGPU)) != 1 {
		t.Fatal("second cancel stored again")
	}
	// The device is free for pipeline work.
	l := de.lectureOn(fixedOn(igpu))
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceOVGPU)

	// A waiting test (its device busy) is cancelled without a stored result.
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	if st, _ := de.s.DeviceTest(model.DeviceOVGPU); st.State != DeviceTestWaiting {
		t.Fatalf("test = %s", st.State)
	}
	if st, err := de.s.CancelDeviceTest(model.DeviceOVGPU); err != nil || st.State != DeviceTestCancelled || st.Result != nil {
		t.Fatalf("cancel waiting: %+v %v", st, err)
	}
	if len(de.devs.benchmarks(model.DeviceOVGPU)) != 1 {
		t.Fatal("a test that never ran stored a result")
	}

	// Never tested: idle; unknown device: not found.
	if st, ok := de.s.DeviceTest(model.DeviceOVNPU); ok {
		t.Fatalf("never tested: %+v", st)
	}
	if st, err := de.s.CancelDeviceTest(model.DeviceOVNPU); err != nil || st.State != DeviceTestIdle || st.DeviceID != model.DeviceOVNPU {
		t.Fatalf("cancel never tested: %+v %v", st, err)
	}
	if _, err := de.s.CancelDeviceTest("ov:XPU"); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("cancel unknown: %v", err)
	}
	if _, err := de.s.StartDeviceTest(de.ctx, "ov:XPU"); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("start unknown: %v", err)
	}

	// Pausing the device with cancel_running cancels its test.
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVNPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVNPU, DeviceTestRunning)
	if err := de.s.SetDevicePaused(model.DeviceOVNPU, true, true); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVNPU, DeviceTestCancelled)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

func TestSelfTestUnavailableWithoutFunc(t *testing.T) {
	de := newDevEnv(t, []model.ASRDevice{devRow(model.DeviceOVGPU, 200, 1)}, nil)
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); !errors.Is(err, ErrDeviceTestUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

// multi_device off counts pipeline jobs only: a self-test on one device and
// a job on another run at the same time.
func TestSelfTestDoesNotCountForMultiDevice(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 1), devRow(model.DeviceOVGPU, 200, 1)
	ft := newFakeTest(1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, func(o *Options) { o.DeviceTest = ft.run })
	de.setASR(func(a *model.ASRSettings) { a.MultiDevice = false })
	de.start()
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	l := de.lectureOn(autoOn(cuda))
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceCUDA0)
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

// ---------- without devices ----------

func TestNoDeviceSourceKeepsLegacyViews(t *testing.T) {
	e := newEnv(t, nil)
	if sts := e.s.DeviceStatuses(); sts != nil {
		t.Fatalf("statuses = %+v", sts)
	}
	if err := e.s.SetDevicePaused(model.DeviceCUDA0, true, false); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("pause: %v", err)
	}
	if _, err := e.s.StartDeviceTest(e.ctx, model.DeviceCUDA0); !errors.Is(err, ErrDeviceTestUnavailable) {
		t.Fatalf("self-test: %v", err)
	}
	if p := e.s.Power(); p.Known {
		t.Fatalf("power = %+v", p)
	}
}

// GPUStatus and GPUSlots with devices: GPUStatus is the cuda:0 view,
// GPUSlots totals every device (the resource bar's summary).
func TestGPUStatusWithDevices(t *testing.T) {
	cuda, igpu := devRow(model.DeviceCUDA0, 300, 2), devRow(model.DeviceOVGPU, 200, 1)
	de := newDevEnv(t, []model.ASRDevice{cuda, igpu}, nil)
	l1, l2 := de.lectureOn(fixedOn(cuda)), de.lectureOn(fixedOn(igpu))
	de.prepDone(l1, l2)
	de.start()
	de.waitRunningOn(l1, model.DeviceCUDA0)
	de.waitRunningOn(l2, model.DeviceOVGPU)
	if g := de.s.GPUStatus(de.ctx); g.SlotsUsed != 1 || g.SlotsMax != 2 || !g.OK {
		t.Fatalf("gpu status = %+v", g)
	}
	if used, max, paused := de.s.GPUSlots(); used != 2 || max != 3 || paused {
		t.Fatalf("gpu slots = %d/%d %v", used, max, paused)
	}
	de.asr.release(l1.ID)
	de.asr.release(l2.ID)
	de.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

// ---------- 3b Gate round 1 ----------

// Item 8: a pipeline job that can use no other device (here: the only
// device of the machine) preempts the running candidate at once; the
// interrupted attempt is discarded and the test starts over afterwards.
func TestSelfTestPreemptedByJobWithoutOtherDevice(t *testing.T) {
	igpu := devRow(model.DeviceOVGPU, 200, 1)
	ft := newFakeTest(2)
	de := newDevEnv(t, []model.ASRDevice{igpu}, func(o *Options) { o.DeviceTest = ft.run })
	de.start()
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	<-ft.started
	ft.step <- struct{}{} // the first candidate finishes; the second one runs (and never finishes by itself)
	de.waitFor("second candidate", 5*time.Second, func() bool { st, _ := de.s.DeviceTest(model.DeviceOVGPU); return st.Step == 2 })

	t0 := time.Now()
	l := de.lectureOn(autoOn(igpu))
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceOVGPU)
	// At once: the candidate never finishes by itself, so without the
	// preemption the job would not start at all. The bound includes the
	// lecture's launch, its prep and a few scans: 1 s was too tight on a
	// loaded machine (a full go test round next to the load tests,
	// 2026-10-28: 1.1 s).
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("the job waited %v for the self-test", d)
	}
	if st, _ := de.s.DeviceTest(model.DeviceOVGPU); st.State != DeviceTestWaiting {
		t.Fatalf("preempted test = %+v", st)
	}
	if s := de.status(model.DeviceOVGPU); s.SlotsUsed != 1 || len(s.Running) != 1 {
		t.Fatalf("status while preempted = %+v", s)
	}
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 0 {
		t.Fatalf("the preempted attempt was stored: %+v", bs)
	}

	// Pipeline work done: the test starts over from the first candidate.
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	select {
	case <-ft.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the test did not start over")
	}
	ft.step <- struct{}{}
	ft.step <- struct{}{}
	done := de.waitTest(model.DeviceOVGPU, DeviceTestDone)
	if done.Result == nil || len(done.Result.Results) != 2 {
		t.Fatalf("done = %+v", done)
	}
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 1 || len(bs[0].Results) != 2 || bs[0].Status != model.BenchmarkDone {
		t.Fatalf("stored = %+v", bs)
	}

	// Cancelled while waiting to start over: nothing is stored.
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	<-ft.started
	de.asr.block(l.ID)
	l2 := de.lectureOn(fixedOn(igpu))
	de.s.Kick()
	de.waitRunningOn(l2, model.DeviceOVGPU)
	if st, err := de.s.CancelDeviceTest(model.DeviceOVGPU); err != nil || st.State != DeviceTestCancelled || st.Result != nil {
		t.Fatalf("cancel while preempted: %+v %v (no result expected)", st, err)
	}
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 1 {
		t.Fatalf("a cancelled test without results stored: %+v", bs)
	}
	de.asr.release(l2.ID)
	de.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

// Item 6: cancelled, or cut short by a shutdown, before any candidate
// finished: last_benchmark keeps the previous complete result.
func TestSelfTestWithoutFinishedCandidateStoresNothing(t *testing.T) {
	igpu := devRow(model.DeviceOVGPU, 200, 1)
	ft := newFakeTest(1)
	de := newDevEnv(t, []model.ASRDevice{igpu}, func(o *Options) { o.DeviceTest = ft.run })
	de.start()
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	ft.step <- struct{}{}
	de.waitTest(model.DeviceOVGPU, DeviceTestDone)
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 1 {
		t.Fatalf("stored = %+v", bs)
	}

	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	st, err := de.s.CancelDeviceTest(model.DeviceOVGPU)
	if err != nil || st.State != DeviceTestCancelled || st.Result != nil {
		t.Fatalf("cancel: %+v %v (no result expected)", st, err)
	}
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 1 {
		t.Fatalf("a cancel before any finished candidate stored: %+v", bs)
	}

	// Shutdown while the first candidate runs.
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	de.s.Stop()
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 1 {
		t.Fatalf("a test cut short by the shutdown stored: %+v", bs)
	}
}

// Item 7: a failure without a code of its own is selftest.failed, the
// original text the detail.
func TestSelfTestUncodedFailureIsSelfTestFailed(t *testing.T) {
	igpu := devRow(model.DeviceOVGPU, 200, 1)
	var mode atomic.Int32
	run := func(ctx context.Context, dev model.ASRDevice, env DeviceTestEnv) (model.DeviceBenchmark, error) {
		if mode.Load() == 0 {
			return model.DeviceBenchmark{}, errors.New("disk read error")
		}
		return model.DeviceBenchmark{Status: model.BenchmarkFailed, Results: []model.BenchmarkResult{{Model: "small", Precision: "int8"}},
			Error: &model.ErrorInfo{Detail: "worker said no"}}, nil
	}
	de := newDevEnv(t, []model.ASRDevice{igpu}, func(o *Options) { o.DeviceTest = run })
	de.start()
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	st := de.waitTest(model.DeviceOVGPU, DeviceTestFailed)
	if st.Error == nil || st.Error.Code != CodeSelfTestFailed || st.Error.Detail != "disk read error" || st.Result != nil {
		t.Fatalf("error = %+v", st)
	}
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 0 {
		t.Fatalf("an error with nothing worth storing stored: %+v", bs)
	}
	mode.Store(1)
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	st = de.waitTest(model.DeviceOVGPU, DeviceTestFailed)
	if st.Error == nil || st.Error.Code != CodeSelfTestFailed || st.Error.Detail != "worker said no" {
		t.Fatalf("failed benchmark without a code: %+v", st.Error)
	}
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 1 {
		t.Fatalf("a failed benchmark with results must be stored: %+v", bs)
	}
}

// Gate round 2, suggestion 3: a test that completes although it was
// preempted (the preemption came too late to stop it) keeps its result.
func TestSelfTestCompleteResultSurvivesLatePreemption(t *testing.T) {
	igpu := devRow(model.DeviceOVGPU, 200, 1)
	finish := make(chan struct{})
	var runs atomic.Int32
	var attempt atomic.Value // context.Context of the running attempt
	run := func(ctx context.Context, dev model.ASRDevice, env DeviceTestEnv) (model.DeviceBenchmark, error) {
		runs.Add(1)
		attempt.Store(ctx)
		<-finish // ignores the cancel, like a worker that just finished
		return model.DeviceBenchmark{Version: model.BenchmarkVersion, Status: model.BenchmarkDone,
			Results: []model.BenchmarkResult{{Model: "large-v3-turbo", Precision: "fp16", OK: true, RTF: 0.1}}}, nil
	}
	de := newDevEnv(t, []model.ASRDevice{igpu}, func(o *Options) { o.DeviceTest = run })
	de.start()
	if _, err := de.s.StartDeviceTest(de.ctx, model.DeviceOVGPU); err != nil {
		t.Fatal(err)
	}
	de.waitTest(model.DeviceOVGPU, DeviceTestRunning)
	l := de.lectureOn(autoOn(igpu))
	de.s.Kick()
	de.waitFor("preemption", 5*time.Second, func() bool {
		c, _ := attempt.Load().(context.Context)
		return c != nil && c.Err() != nil
	})
	close(finish)
	done := de.waitTest(model.DeviceOVGPU, DeviceTestDone)
	if done.Result == nil || len(done.Result.Results) != 1 || runs.Load() != 1 {
		t.Fatalf("done = %+v, runs %d", done, runs.Load())
	}
	if bs := de.devs.benchmarks(model.DeviceOVGPU); len(bs) != 1 || bs[0].Status != model.BenchmarkDone {
		t.Fatalf("stored = %+v", bs)
	}
	de.waitRunningOn(l, model.DeviceOVGPU)
	de.asr.release(l.ID)
	de.waitStatus(l.ID, model.StageASR, model.StatusDone)
	time.Sleep(60 * time.Millisecond)
	if runs.Load() != 1 {
		t.Fatalf("a complete test started over (%d runs)", runs.Load())
	}
}
