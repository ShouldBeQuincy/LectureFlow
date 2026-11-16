// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// SPEC-v2 I6-I10 (phase 3b task E): the device endpoints and the device
// parts of the existing views, against a fake registry and a fake device
// scheduler (the real ones are tasks B and C; cmd's e2e covers the wiring).

// ---- fakes ----

type fakeDevReg struct {
	mu        sync.Mutex
	rows      []model.ASRDevice
	present   map[string]bool
	detected  bool
	det       devices.Detection
	dismissed map[string]bool
	detects   int
	detectDet *devices.Detection // what the next Detect records (nil: det with At = now)
	onChange  func()             // cmd's wiring: SSE changed devices + kick
}

func (f *fakeDevReg) changed() {
	if f.onChange != nil {
		f.onChange()
	}
}

func (f *fakeDevReg) sortLocked() {
	sort.SliceStable(f.rows, func(i, j int) bool {
		if f.rows[i].Priority != f.rows[j].Priority {
			return f.rows[i].Priority > f.rows[j].Priority
		}
		return f.rows[i].ID < f.rows[j].ID
	})
}

func (f *fakeDevReg) List() []model.ASRDevice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.ASRDevice(nil), f.rows...)
}

func (f *fakeDevReg) Get(id string) (model.ASRDevice, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.rows {
		if d.ID == id {
			return d, true
		}
	}
	return model.ASRDevice{}, false
}

func (f *fakeDevReg) Present(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.present[id]
}

func (f *fakeDevReg) Detected() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.detected }

func (f *fakeDevReg) LastDetection() devices.Detection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.det
}

func (f *fakeDevReg) Detect(ctx context.Context) (devices.Detection, []string, error) {
	f.mu.Lock()
	f.detects++
	if f.detectDet != nil {
		f.det = *f.detectDet
	} else {
		f.det.At = time.Now()
	}
	f.detected = true
	d := f.det
	f.mu.Unlock()
	f.changed()
	return d, nil, nil
}

func (f *fakeDevReg) Update(ctx context.Context, id string, p model.ASRDevicePatch) (model.ASRDevice, error) {
	f.mu.Lock()
	var out model.ASRDevice
	err := func() error {
		for i := range f.rows {
			d := &f.rows[i]
			if d.ID != id {
				continue
			}
			n := *d
			if p.Enabled != nil {
				n.Enabled = *p.Enabled
			}
			if p.Model != nil {
				if *p.Model == "" {
					return model.Errorf(devices.CodeModelMissing, nil, devices.ErrInvalid)
				}
				n.Model = *p.Model
			}
			if p.Precision != nil {
				if !model.ValidPrecision(n.Backend, *p.Precision) {
					return model.Errorf(devices.CodePrecisionInvalid, map[string]any{"value": *p.Precision}, devices.ErrInvalid)
				}
				n.Precision = *p.Precision
			}
			if p.MaxParallel != nil {
				if *p.MaxParallel < 1 || *p.MaxParallel > 8 {
					return model.Errorf(devices.CodeMaxParallelInvalid, map[string]any{"min": 1, "max": 8}, devices.ErrInvalid)
				}
				n.MaxParallel = *p.MaxParallel
			}
			*d, out = n, n
			return nil
		}
		return model.Errorf(devices.CodeNotFound, map[string]any{"device": id}, db.ErrNotFound)
	}()
	f.mu.Unlock()
	if err == nil {
		f.changed()
	}
	return out, err
}

func (f *fakeDevReg) Reorder(ctx context.Context, ids []string) ([]model.ASRDevice, error) {
	f.mu.Lock()
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if len(ids) != len(f.rows) || len(seen) != len(ids) {
		f.mu.Unlock()
		return nil, model.Errorf(devices.CodeOrderInvalid, nil, devices.ErrInvalid)
	}
	pos := map[string]int{}
	for i, id := range ids {
		pos[id] = i
	}
	for i := range f.rows {
		p, ok := pos[f.rows[i].ID]
		if !ok {
			f.mu.Unlock()
			return nil, model.Errorf(devices.CodeOrderInvalid, nil, devices.ErrInvalid)
		}
		f.rows[i].Priority = (len(ids) - p) * 100
	}
	f.sortLocked()
	out := append([]model.ASRDevice(nil), f.rows...)
	f.mu.Unlock()
	f.changed()
	return out, nil
}

func (f *fakeDevReg) ApplyBenchmark(ctx context.Context, id, mdl, precision string) (model.ASRDevice, error) {
	d, ok := f.Get(id)
	if !ok {
		return model.ASRDevice{}, model.Errorf(devices.CodeNotFound, map[string]any{"device": id}, db.ErrNotFound)
	}
	notTested := model.Errorf(devices.CodeApplyNotTested, map[string]any{"model": mdl, "precision": precision}, devices.ErrInvalid)
	if d.LastBenchmark == nil {
		return model.ASRDevice{}, notTested
	}
	var b model.DeviceBenchmark
	if err := json.Unmarshal([]byte(*d.LastBenchmark), &b); err != nil {
		return model.ASRDevice{}, err
	}
	for _, r := range b.Results {
		if r.OK && r.Model == mdl && r.Precision == precision {
			at := "2026-10-24T09:00:00Z"
			b.AppliedAt = &at
			raw := model.MarshalSnapshot(b)
			f.mu.Lock()
			for i := range f.rows {
				if f.rows[i].ID == id {
					f.rows[i].Model, f.rows[i].Precision, f.rows[i].LastBenchmark = mdl, precision, &raw
					d = f.rows[i]
				}
			}
			f.mu.Unlock()
			f.changed()
			return d, nil
		}
	}
	return model.ASRDevice{}, notTested
}

func (f *fakeDevReg) Prompts() []model.ASRDevice {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.ASRDevice
	for _, d := range f.rows {
		if f.present[d.ID] && d.Enabled && d.LastBenchmark == nil && !f.dismissed[d.ID] {
			out = append(out, d)
		}
	}
	return out
}

func (f *fakeDevReg) DismissPrompt(ctx context.Context, id string) error {
	f.mu.Lock()
	f.dismissed[id] = true
	f.mu.Unlock()
	f.changed()
	return nil
}

type fakeDevSched struct {
	mu         sync.Mutex
	reg        *fakeDevReg
	statuses   map[string]sched.DeviceStatus
	paused     map[string]bool
	pauseCalls []string
	power      devices.Power
	tests      map[string]sched.DeviceTestState
	noTests    bool  // no DeviceTest function configured
	startErr   error // StartDeviceTest's error (power management: selftest.power_battery)
	starts     int
	startCtx   context.Context
}

func (f *fakeDevSched) DeviceStatuses() []sched.DeviceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sched.DeviceStatus
	for _, d := range f.reg.List() {
		st := f.statuses[d.ID]
		st.ID, st.Paused = d.ID, f.paused[d.ID]
		if st.SlotsMax == 0 {
			st.SlotsMax = d.MaxParallel
		}
		if t, ok := f.tests[d.ID]; ok {
			t := t
			st.SelfTest = &t
		}
		out = append(out, st)
	}
	return out
}

func (f *fakeDevSched) SetDevicePaused(id string, paused, cancelRunning bool) error {
	if _, ok := f.reg.Get(id); !ok {
		return sched.ErrUnknownDevice
	}
	f.mu.Lock()
	f.paused[id] = paused
	f.pauseCalls = append(f.pauseCalls, id+" "+boolStr(paused)+" "+boolStr(cancelRunning))
	f.mu.Unlock()
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (f *fakeDevSched) DevicePaused(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused[id]
}
func (f *fakeDevSched) Power() devices.Power { f.mu.Lock(); defer f.mu.Unlock(); return f.power }

func (f *fakeDevSched) StartDeviceTest(ctx context.Context, id string) (sched.DeviceTestState, error) {
	if _, ok := f.reg.Get(id); !ok {
		return sched.DeviceTestState{}, sched.ErrUnknownDevice
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noTests {
		return sched.DeviceTestState{}, sched.ErrDeviceTestUnavailable
	}
	if f.startErr != nil {
		return sched.DeviceTestState{}, f.startErr
	}
	if t, ok := f.tests[id]; ok && (t.State == sched.DeviceTestWaiting || t.State == sched.DeviceTestRunning) {
		return sched.DeviceTestState{}, sched.ErrSelfTestRunning
	}
	f.starts++
	f.startCtx = ctx
	st := sched.DeviceTestState{DeviceID: id, State: sched.DeviceTestWaiting, Steps: len(devices.Candidates(id))}
	f.tests[id] = st
	return st, nil
}

func (f *fakeDevSched) CancelDeviceTest(id string) (sched.DeviceTestState, error) {
	if _, ok := f.reg.Get(id); !ok {
		return sched.DeviceTestState{}, sched.ErrUnknownDevice
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tests[id]
	if !ok {
		return sched.DeviceTestState{}, nil
	}
	if t.State == sched.DeviceTestWaiting || t.State == sched.DeviceTestRunning {
		t.State, t.FinishedAt = sched.DeviceTestCancelled, time.Date(2026, 10, 24, 9, 30, 0, 0, time.UTC)
		f.tests[id] = t
	}
	return t, nil
}

func (f *fakeDevSched) DeviceTest(id string) (sched.DeviceTestState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tests[id]
	return t, ok
}

// ---- fixture ----

const (
	nvidiaName = "NVIDIA GeForce RTX 5070 Ti Laptop GPU"
	igpuName   = "Intel(R) Graphics (iGPU)"
	npuName    = "Intel(R) AI Boost"
)

type devEnv struct {
	*env
	reg   *fakeDevReg
	ds    *fakeDevSched
	clip  bool // a repaired lecture exists for the self-test
	clipM sync.Mutex
}

func (d *devEnv) setClip(ok bool) { d.clipM.Lock(); d.clip = ok; d.clipM.Unlock() }

// newDevEnv is a laptop with cuda:0, ov:GPU, ov:NPU (present) and ov:CPU
// (disabled), the last detection having seen all of them.
func newDevEnv(t *testing.T, ec envConfig) *devEnv {
	t.Helper()
	at := "2026-10-24T08:00:00Z"
	det := devices.Detection{At: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC), CUDAName: nvidiaName,
		Python: filepath.Join(t.TempDir(), "missing", "python.exe"),
		OV: &devices.OVProbe{OpenVINO: "2026.4.1", GenAI: "2026.4.1.0", CanConvert: true,
			Devices: []devices.OVDevice{
				{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: igpuName, Vendor: "0x8086", LUID: "e220010000000000", DriverVersion: "32.0.101.8724"},
				{Name: "NPU", Kind: model.DeviceKindNPU, FullName: npuName, Vendor: "0x8086", LUID: "d026010000000000", DriverVersion: "32.0.100.4841"},
				{Name: "CPU", Kind: model.DeviceKindCPU, FullName: "Intel(R) Core(TM) Ultra 9 386H"},
			},
			Ignored: []devices.OVDevice{{Name: "GPU.1", FullName: nvidiaName + " (dGPU)", Vendor: "0x10de", Reason: devices.ProbeReasonNotIntel}},
		}}
	reg := &fakeDevReg{
		rows: []model.ASRDevice{
			{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: nvidiaName, Enabled: true, Model: "large-v3", Precision: "auto", MaxParallel: 1, Priority: 300, DetectedAt: &at},
			{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Label: igpuName, Enabled: true, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 200, DetectedAt: &at},
			{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: npuName, Enabled: true, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 100, DetectedAt: &at},
			{ID: model.DeviceOVCPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Core(TM) Ultra 9 386H", Enabled: false, Model: "large-v3-turbo", Precision: "int8", MaxParallel: 1, Priority: 0, DetectedAt: &at},
		},
		present:   map[string]bool{model.DeviceCUDA0: true, model.DeviceOVGPU: true, model.DeviceOVNPU: true, model.DeviceOVCPU: true},
		detected:  true,
		det:       det,
		dismissed: map[string]bool{model.DeviceCUDA0: true}, // migrated: not a new device
	}
	ds := &fakeDevSched{reg: reg, statuses: map[string]sched.DeviceStatus{}, paused: map[string]bool{}, tests: map[string]sched.DeviceTestState{}}
	de := &devEnv{reg: reg, ds: ds}
	inner := ec.server
	ec.server = func(s *Server) {
		s.dev, s.dsc = reg, ds
		s.findClip = func(context.Context) (devices.ClipSource, bool, error) {
			de.clipM.Lock()
			defer de.clipM.Unlock()
			return devices.ClipSource{LectureID: "l1"}, de.clip, nil
		}
		if inner != nil {
			inner(s)
		}
	}
	de.env = newEnvWith(t, ec)
	reg.onChange = func() { de.hub.Changed("devices", "") }
	return de
}

func (d *devEnv) devicePath(id string, rest ...string) string {
	p := "/api/asr/devices/" + url.PathEscape(id)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}

func viewOf(t *testing.T, vs []ASRDeviceView, id string) ASRDeviceView {
	t.Helper()
	for _, v := range vs {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no device %s in %+v", id, vs)
	return ASRDeviceView{}
}

// ---- tests ----

// Without the registry (builds and tests before 3b) the device endpoints
// answer 503 and the views carry no device fields.
func TestDevicesWithoutRegistry(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/asr/devices"}, {"POST", "/api/asr/devices/detect"}, {"PUT", "/api/asr/devices/order"},
		{"PATCH", "/api/asr/devices/ov:GPU"}, {"POST", "/api/asr/devices/ov:GPU/pause"},
		{"POST", "/api/asr/devices/ov:GPU/selftest"}, {"GET", "/api/asr/devices/ov:GPU/selftest"},
		{"DELETE", "/api/asr/devices/ov:GPU/selftest"}, {"POST", "/api/asr/devices/ov:GPU/selftest/apply"},
		{"POST", "/api/asr/devices/ov:GPU/prompt/dismiss"},
	} {
		info := e.callErrInfo(c.method, c.path, map[string]any{}, http.StatusServiceUnavailable)
		if info.Code != "server.unavailable" || info.Params["service"] != "devices" {
			t.Errorf("%s %s: %+v", c.method, c.path, info)
		}
	}
	for _, p := range []string{"/api/settings", "/api/system", "/api/dashboard"} {
		_, b := e.do("GET", p, nil)
		for _, k := range []string{`"devices"`, `"power"`, `"openvino"`, `"device_prompts"`} {
			if strings.Contains(string(b), k) {
				t.Errorf("%s carries %s without the registry", p, k)
			}
		}
	}
	e.srv.diag.waitStartup()
	var sys System
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	for _, c := range sys.Checks {
		if strings.HasPrefix(c.Name, "openvino") || c.Name == "npu_driver" {
			t.Errorf("OpenVINO check without the registry: %+v", c)
		}
	}
}

func TestDeviceList(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	c := de.course("C")
	l := de.lecture(c.ID, "Week 3 Ciphers")
	// large-v3-turbo fp16 is converted (one model directory for every
	// OpenVINO device); the NPU has its compile cache, the Intel GPU not.
	gpuDir := devices.ModelDir(de.cfg.DataDir, "large-v3-turbo", "fp16")
	if err := os.MkdirAll(gpuDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range devices.ModelFiles {
		if err := os.WriteFile(filepath.Join(gpuDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	npuCache := devices.CacheDir(de.cfg.DataDir, "NPU", "large-v3-turbo", "fp16")
	if err := os.MkdirAll(npuCache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(npuCache, "model.blob"), []byte("blob"), 0o644); err != nil {
		t.Fatal(err)
	}
	wer := 0.057
	bench := model.MarshalSnapshot(model.DeviceBenchmark{Version: 1, Status: model.BenchmarkDone, StartedAt: "2026-10-24T08:10:00Z",
		FinishedAt: "2026-10-24T08:20:00Z", Audio: model.BenchmarkAudio{Source: model.BenchmarkSourceLec, LectureID: l.ID, StartMS: 1000, EndMS: 121000, ReferenceWords: 300},
		Results: []model.BenchmarkResult{{Model: "large-v3-turbo", Precision: "fp16", OK: true, LoadS: 4.2, TranscribeS: 10, RTF: 0.083, WER: &wer, Words: true}},
		Pick:    &model.BenchmarkPick{Model: "large-v3-turbo", Precision: "fp16", Rule: model.PickRuleBest}})
	de.reg.mu.Lock()
	de.reg.rows[1].LastBenchmark = &bench
	de.reg.mu.Unlock()
	de.ds.mu.Lock()
	de.ds.statuses[model.DeviceOVGPU] = sched.DeviceStatus{SlotsUsed: 1, SlotsMax: 1,
		Running: []sched.RunningASR{{JobID: "job-1", LectureID: l.ID, Progress: 0.4}}}
	de.ds.statuses[model.DeviceCUDA0] = sched.DeviceStatus{BatteryHold: true}
	de.ds.paused[model.DeviceOVCPU] = true
	de.ds.mu.Unlock()
	de.setClip(true)

	var vs []ASRDeviceView
	de.call("GET", "/api/asr/devices", nil, http.StatusOK, &vs)
	if len(vs) != 4 || vs[0].ID != model.DeviceCUDA0 || vs[3].ID != model.DeviceOVCPU {
		t.Fatalf("devices %+v", vs)
	}
	cuda := viewOf(t, vs, model.DeviceCUDA0)
	if cuda.Kind != model.DeviceKindCUDA || cuda.Backend != model.BackendFasterWhisper || cuda.ModelReady != nil || cuda.CompileCached != nil ||
		!cuda.BatteryHold || cuda.New || cuda.Benchmark != nil || cuda.SelfTest.State != sched.DeviceTestIdle || cuda.SelfTest.StartedAt != nil ||
		cuda.SelfTestBlocked != nil || cuda.Running == nil || len(cuda.Running) != 0 {
		t.Fatalf("cuda %+v", cuda)
	}
	if o := cuda.Options; o.Recommended == nil || o.Recommended.Model != "large-v3" || o.Recommended.Precision != "auto" ||
		strings.Join(o.Precisions, ",") != "auto,float16,int8_float16" || len(o.Models) == 0 {
		t.Fatalf("cuda options %+v", o)
	}
	gpu := viewOf(t, vs, model.DeviceOVGPU)
	if gpu.Kind != model.DeviceKindIGPU || gpu.Label != igpuName || gpu.ModelReady == nil || !*gpu.ModelReady || gpu.CompileCached == nil ||
		*gpu.CompileCached || gpu.New || gpu.SlotsUsed != 1 || len(gpu.Running) != 1 {
		t.Fatalf("igpu %+v", gpu)
	}
	if r := gpu.Running[0]; r != (DeviceTaskView{JobID: "job-1", LectureID: l.ID, LectureTitle: "Week 3 Ciphers", CourseID: c.ID, Progress: 0.4}) {
		t.Fatalf("igpu running %+v", r)
	}
	if b := gpu.Benchmark; b == nil || b.Pick == nil || b.Pick.Rule != model.PickRuleBest || len(b.Results) != 1 || *b.Results[0].WER != wer {
		t.Fatalf("igpu benchmark %+v", gpu.Benchmark)
	}
	npu := viewOf(t, vs, model.DeviceOVNPU)
	if !npu.New || npu.ModelReady == nil || !*npu.ModelReady || npu.CompileCached == nil || !*npu.CompileCached ||
		npu.Options.Recommended == nil || npu.Options.Recommended.Model != "large-v3-turbo" || strings.Join(npu.Options.Precisions, ",") != "fp16,int8,int4" ||
		strings.Join(npu.Options.Models, ",") != "large-v3-turbo,large-v3,medium,small,base" {
		t.Fatalf("npu %+v", npu)
	}
	cpu := viewOf(t, vs, model.DeviceOVCPU) // int8: not converted; the CPU compiles nothing
	if cpu.Enabled || !cpu.Paused || cpu.New || cpu.ModelReady == nil || *cpu.ModelReady || cpu.CompileCached == nil || !*cpu.CompileCached {
		t.Fatalf("ov:CPU %+v", cpu)
	}

	// No repaired lecture: "测试这台设备" is not available (lead ruling 7);
	// an OpenVINO device the detection did not see cannot be tested either.
	de.setClip(false)
	de.reg.mu.Lock()
	de.reg.present[model.DeviceOVNPU] = false
	de.reg.mu.Unlock()
	de.call("GET", "/api/asr/devices", nil, http.StatusOK, &vs)
	for _, v := range vs {
		if v.SelfTestBlocked == nil || v.SelfTestBlocked.Code != stages.CodeSelfTestNoAudio {
			t.Errorf("%s without audio: blocked %+v", v.ID, v.SelfTestBlocked)
		}
	}
	de.setClip(true)
	de.call("GET", "/api/asr/devices", nil, http.StatusOK, &vs)
	if b := viewOf(t, vs, model.DeviceOVNPU).SelfTestBlocked; b == nil || b.Code != stages.CodeASROVDeviceUnavailable || b.Params["device"] != npuName {
		t.Fatalf("npu not present: blocked %+v", b)
	}
	if v := viewOf(t, vs, model.DeviceOVNPU); v.Present || v.New {
		t.Fatalf("npu not present: %+v", v)
	}
	if b := viewOf(t, vs, model.DeviceOVGPU).SelfTestBlocked; b != nil {
		t.Fatalf("igpu blocked %+v", b)
	}

	// The settings page carries the same list (I6 转写 section).
	var sv Settings
	de.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	if len(sv.Devices) != 4 || sv.Devices[1].ID != model.DeviceOVGPU || len(sv.Devices[1].Running) != 1 {
		t.Fatalf("settings devices %+v", sv.Devices)
	}
}

func TestDevicePatchAndOrder(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	var v ASRDeviceView
	de.call("PATCH", de.devicePath(model.DeviceOVGPU), map[string]any{"precision": "int8", "max_parallel": 2}, http.StatusOK, &v)
	if v.ID != model.DeviceOVGPU || v.Precision != "int8" || v.MaxParallel != 2 || v.Model != "large-v3-turbo" {
		t.Fatalf("patched %+v", v)
	}
	de.call("PATCH", de.devicePath(model.DeviceOVCPU), map[string]any{"enabled": true}, http.StatusOK, &v)
	if !v.Enabled {
		t.Fatalf("enable %+v", v)
	}
	cases := []struct {
		body   map[string]any
		status int
		code   string
		params map[string]any
	}{
		{map[string]any{"model": ""}, 400, devices.CodeModelMissing, nil},
		{map[string]any{"precision": "float16"}, 400, devices.CodePrecisionInvalid, map[string]any{"value": "float16"}},
		{map[string]any{"max_parallel": 9}, 400, devices.CodeMaxParallelInvalid, map[string]any{"min": float64(1), "max": float64(8)}},
		{map[string]any{}, 400, "request.nothing_to_change", nil},
	}
	for _, c := range cases {
		info := de.callErrInfo("PATCH", de.devicePath(model.DeviceOVGPU), c.body, c.status)
		if info.Code != c.code {
			t.Errorf("%v: %+v, want %s", c.body, info, c.code)
		}
		for k, want := range c.params {
			if info.Params[k] != want {
				t.Errorf("%v: param %s = %v, want %v", c.body, k, info.Params[k], want)
			}
		}
	}
	if info := de.callErrInfo("PATCH", de.devicePath("ov:TPU"), map[string]any{"enabled": false}, http.StatusNotFound); info.Code != devices.CodeNotFound || info.Params["device"] != "ov:TPU" {
		t.Fatalf("unknown device %+v", info)
	}
	if res, b := de.do("PATCH", de.devicePath(model.DeviceOVGPU), nil); res.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "request.bad_json") {
		t.Fatalf("no body: %d %s", res.StatusCode, b)
	}

	// Drag order (I8): the new priorities, every device in the new order.
	var vs []ASRDeviceView
	order := []string{model.DeviceOVNPU, model.DeviceOVGPU, model.DeviceCUDA0, model.DeviceOVCPU}
	de.call("PUT", "/api/asr/devices/order", DeviceOrderInput{IDs: order}, http.StatusOK, &vs)
	for i, v := range vs {
		if v.ID != order[i] || (i > 0 && v.Priority >= vs[i-1].Priority) {
			t.Fatalf("order %d: %+v", i, vs)
		}
	}
	if info := de.callErrInfo("PUT", "/api/asr/devices/order", DeviceOrderInput{IDs: order[:2]}, http.StatusBadRequest); info.Code != devices.CodeOrderInvalid {
		t.Fatalf("partial order %+v", info)
	}
}

func TestDevicePause(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	if info := de.callErrInfo("POST", de.devicePath(model.DeviceOVNPU, "pause"), map[string]any{}, http.StatusBadRequest); info.Code != "system.paused_missing" {
		t.Fatalf("missing paused %+v", info)
	}
	var v ASRDeviceView
	de.call("POST", de.devicePath(model.DeviceOVNPU, "pause"), DevicePauseInput{Paused: true, CancelRunning: true}, http.StatusOK, &v)
	if !v.Paused || v.ID != model.DeviceOVNPU {
		t.Fatalf("paused %+v", v)
	}
	// cancel_running only goes with paused (resuming cancels nothing).
	de.call("POST", de.devicePath(model.DeviceOVNPU, "pause"), DevicePauseInput{Paused: false, CancelRunning: true}, http.StatusOK, &v)
	if v.Paused {
		t.Fatalf("resumed %+v", v)
	}
	de.ds.mu.Lock()
	got := strings.Join(de.ds.pauseCalls, "; ")
	de.ds.mu.Unlock()
	if got != "ov:NPU true true; ov:NPU false false" {
		t.Fatalf("scheduler calls %q", got)
	}
	if info := de.callErrInfo("POST", de.devicePath("ov:TPU", "pause"), DevicePauseInput{Paused: true}, http.StatusNotFound); info.Code != devices.CodeNotFound || info.Params["device"] != "ov:TPU" {
		t.Fatalf("unknown device %+v", info)
	}
}

func TestSelfTestEndpoints(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	path := de.devicePath(model.DeviceOVGPU, "selftest")
	// No repaired lecture yet (lead ruling 7).
	if info := de.callErrInfo("POST", path, nil, http.StatusConflict); info.Code != stages.CodeSelfTestNoAudio {
		t.Fatalf("no audio %+v", info)
	}
	de.setClip(true)
	var st SelfTestView
	de.call("GET", path, nil, http.StatusOK, &st)
	if st.State != sched.DeviceTestIdle || st.DeviceID != model.DeviceOVGPU || st.StartedAt != nil {
		t.Fatalf("idle %+v", st)
	}
	de.call("POST", path, nil, http.StatusAccepted, &st)
	if st.State != sched.DeviceTestWaiting || st.Steps != 3 || st.DeviceID != model.DeviceOVGPU {
		t.Fatalf("started %+v", st)
	}
	// The run outlives the request: the scheduler gets a context the
	// response does not cancel.
	de.ds.mu.Lock()
	startCtx := de.ds.startCtx
	de.ds.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if startCtx == nil || startCtx.Err() != nil {
		t.Fatalf("self-test context %v", startCtx)
	}
	if info := de.callErrInfo("POST", path, nil, http.StatusConflict); info.Code != sched.CodeSelfTestRunning {
		t.Fatalf("second start %+v", info)
	}
	// Running with a candidate: the view carries the phase and candidate.
	de.ds.mu.Lock()
	de.ds.tests[model.DeviceOVGPU] = sched.DeviceTestState{DeviceID: model.DeviceOVGPU, State: sched.DeviceTestRunning,
		Phase: sched.DeviceTestPhaseCompiling, Step: 2, Steps: 3, Progress: 0.3,
		Candidate: &devices.Candidate{Model: "large-v3-turbo", Precision: "int8"}, StartedAt: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
	de.ds.mu.Unlock()
	st = SelfTestView{}
	de.call("GET", path, nil, http.StatusOK, &st)
	if st.State != sched.DeviceTestRunning || st.Phase != model.ASRStatusCompilingModel || st.Step != 2 || st.Candidate == nil ||
		*st.Candidate != (ModelPrecision{"large-v3-turbo", "int8"}) || st.StartedAt == nil || *st.StartedAt != "2026-10-24T09:00:00Z" || st.FinishedAt != nil {
		t.Fatalf("running %+v", st)
	}
	var vs []ASRDeviceView
	de.call("GET", "/api/asr/devices", nil, http.StatusOK, &vs)
	if v := viewOf(t, vs, model.DeviceOVGPU); v.SelfTest.State != sched.DeviceTestRunning || v.SelfTest.Phase != sched.DeviceTestPhaseCompiling {
		t.Fatalf("device view self-test %+v", v.SelfTest)
	}
	st = SelfTestView{}
	de.call("DELETE", path, nil, http.StatusOK, &st)
	if st.State != sched.DeviceTestCancelled || st.FinishedAt == nil || st.Phase != "" {
		t.Fatalf("cancelled %+v", st)
	}
	st = SelfTestView{}
	de.call("DELETE", path, nil, http.StatusOK, &st) // idempotent
	if st.State != sched.DeviceTestCancelled {
		t.Fatalf("second cancel %+v", st)
	}
	st = SelfTestView{}
	de.call("DELETE", de.devicePath(model.DeviceOVNPU, "selftest"), nil, http.StatusOK, &st) // none to cancel
	if st.State != sched.DeviceTestIdle || st.DeviceID != model.DeviceOVNPU {
		t.Fatalf("cancel without a test %+v", st)
	}
	for _, m := range []string{"POST", "GET", "DELETE"} {
		if info := de.callErrInfo(m, de.devicePath("ov:TPU", "selftest"), nil, http.StatusNotFound); info.Code != devices.CodeNotFound {
			t.Errorf("%s unknown device %+v", m, info)
		}
	}
	// An OpenVINO device the detection did not see.
	de.reg.mu.Lock()
	de.reg.present[model.DeviceOVNPU] = false
	de.reg.mu.Unlock()
	if info := de.callErrInfo("POST", de.devicePath(model.DeviceOVNPU, "selftest"), nil, http.StatusConflict); info.Code != stages.CodeASROVDeviceUnavailable || info.Params["device"] != npuName {
		t.Fatalf("not present %+v", info)
	}
	// No self-test function configured.
	de.ds.mu.Lock()
	de.ds.noTests = true
	de.ds.mu.Unlock()
	if info := de.callErrInfo("POST", de.devicePath(model.DeviceCUDA0, "selftest"), nil, http.StatusServiceUnavailable); info.Code != "server.unavailable" || info.Params["service"] != "selftest" {
		t.Fatalf("unavailable %+v", info)
	}
}

func TestSelfTestApply(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	path := de.devicePath(model.DeviceOVNPU, "selftest", "apply")
	if info := de.callErrInfo("POST", path, SelfTestApplyInput{Model: "large-v3-turbo", Precision: "int8"}, http.StatusBadRequest); info.Code != devices.CodeApplyNotTested ||
		info.Params["model"] != "large-v3-turbo" || info.Params["precision"] != "int8" {
		t.Fatalf("not tested %+v", info)
	}
	b := model.MarshalSnapshot(model.DeviceBenchmark{Version: 1, Status: model.BenchmarkDone, Results: []model.BenchmarkResult{
		{Model: "large-v3-turbo", Precision: "fp16", OK: true, RTF: 0.2}, {Model: "large-v3-turbo", Precision: "int8", OK: true, RTF: 0.15},
	}})
	de.reg.mu.Lock()
	de.reg.rows[2].LastBenchmark = &b
	de.reg.mu.Unlock()
	var v ASRDeviceView
	de.call("POST", path, SelfTestApplyInput{Model: "large-v3-turbo", Precision: "int8"}, http.StatusOK, &v)
	if v.Precision != "int8" || v.Benchmark == nil || v.Benchmark.AppliedAt == nil {
		t.Fatalf("applied %+v", v)
	}
	if info := de.callErrInfo("POST", de.devicePath("ov:TPU", "selftest", "apply"), SelfTestApplyInput{Model: "x", Precision: "y"}, http.StatusNotFound); info.Code != devices.CodeNotFound {
		t.Fatalf("unknown %+v", info)
	}
}

// The workbench prompt (I9) for a new device that was never tested.
func TestDevicePromptsOnDashboard(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	var d Dashboard
	de.call("GET", "/api/dashboard", nil, http.StatusOK, &d)
	// cuda:0 was migrated (dismissed), ov:CPU is disabled.
	if len(d.DevicePrompts) != 2 || d.DevicePrompts[0] != (DevicePrompt{DeviceID: model.DeviceOVGPU, Label: igpuName, Kind: model.DeviceKindIGPU}) ||
		d.DevicePrompts[1].DeviceID != model.DeviceOVNPU || d.DevicePrompts[1].Kind != model.DeviceKindNPU {
		t.Fatalf("prompts %+v", d.DevicePrompts)
	}
	res, body := de.do("POST", de.devicePath(model.DeviceOVGPU, "prompt", "dismiss"), nil)
	if res.StatusCode != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("dismiss: %d %s", res.StatusCode, body)
	}
	var d2 Dashboard
	de.call("GET", "/api/dashboard", nil, http.StatusOK, &d2)
	if len(d2.DevicePrompts) != 1 || d2.DevicePrompts[0].DeviceID != model.DeviceOVNPU {
		t.Fatalf("prompts after dismiss %+v", d2.DevicePrompts)
	}
	if info := de.callErrInfo("POST", de.devicePath("ov:TPU", "prompt", "dismiss"), nil, http.StatusNotFound); info.Code != devices.CodeNotFound {
		t.Fatalf("unknown %+v", info)
	}
	// Every device dismissed or tested: no prompts, the field is omitted.
	de.do("POST", de.devicePath(model.DeviceOVNPU, "prompt", "dismiss"), nil)
	_, raw := de.do("GET", "/api/dashboard", nil)
	if strings.Contains(string(raw), "device_prompts") {
		t.Fatalf("empty prompts not omitted: %s", raw)
	}
}

// SSE: every device change reaches the front end as changed {kind:
// devices} (here the registry's OnChange hook wired as cmd does).
func TestDeviceChangesReachSSE(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	r := openSSE(t, de.ts.URL+"/api/events")
	waitClients(t, de.hub, 1)
	de.call("PATCH", de.devicePath(model.DeviceOVGPU), map[string]any{"enabled": false}, http.StatusOK, nil)
	f := r.next(3*time.Second, func(f sseFrame) bool { return f.event == "changed" && strings.Contains(f.data, `"devices"`) })
	var ev ChangedEvent
	if err := json.Unmarshal([]byte(f.data), &ev); err != nil || ev.Kind != "devices" || ev.ID != "" {
		t.Fatalf("event %s", f.data)
	}
	// A detection also refreshes the diagnostics page.
	de.call("POST", "/api/asr/devices/detect", nil, http.StatusOK, nil)
	r.next(3*time.Second, func(f sseFrame) bool { return f.event == "changed" && strings.Contains(f.data, `"system"`) })
}

// PUT /api/settings/asr merges the body into the current settings (lead
// ruling 15): an old client's body without the 3b fields keeps them.
func TestPutASRSettingsMerges(t *testing.T) {
	e := newEnv(t)
	var sv Settings
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	a := sv.ASR
	a.DeviceMode, a.MultiDevice, a.BatteryAware, a.MemReserveMB = model.ASRDeviceModeAuto, true, true, 3072
	e.call("PUT", "/api/settings/asr", a, http.StatusOK, &sv)
	if !sv.ASR.MultiDevice || !sv.ASR.BatteryAware || sv.ASR.MemReserveMB != 3072 || sv.ASR.DeviceMode != model.ASRDeviceModeAuto {
		t.Fatalf("stored %+v", sv.ASR)
	}
	// A v1.1 client sends only the fields it knows.
	old := map[string]any{"model": "large-v2", "compute_type": "auto", "device": "cuda", "language": "zh",
		"max_parallel": 1, "vram_reserve_mb": 1500, "vram_per_job_mb": 5000}
	e.call("PUT", "/api/settings/asr", old, http.StatusOK, &sv)
	if sv.ASR.Model != "large-v2" || sv.ASR.Language != "zh" || sv.ASR.VRAMReserveMB != 1500 ||
		!sv.ASR.MultiDevice || !sv.ASR.BatteryAware || sv.ASR.MemReserveMB != 3072 || sv.ASR.DeviceMode != model.ASRDeviceModeAuto {
		t.Fatalf("after an old client's PUT %+v", sv.ASR)
	}
	// One field: the rest is kept; a false that is sent is written.
	e.call("PUT", "/api/settings/asr", map[string]any{"battery_aware": false}, http.StatusOK, &sv)
	if sv.ASR.BatteryAware || !sv.ASR.MultiDevice || sv.ASR.Model != "large-v2" || sv.ASR.Language != "zh" {
		t.Fatalf("after a one-field PUT %+v", sv.ASR)
	}
	// Validation still applies to the merged settings.
	e.callErr("PUT", "/api/settings/asr", map[string]any{"device": "tpu"}, http.StatusBadRequest)
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	if sv.ASR.Device != "cuda" {
		t.Fatalf("invalid PUT changed the settings %+v", sv.ASR)
	}
}

func TestSystemDevicesAndOpenVINO(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	de.srv.diag.waitStartup()
	de.ds.mu.Lock()
	de.ds.power = devices.Power{Known: true, OnBattery: true, BatteryPct: ptr(64)}
	de.ds.mu.Unlock()
	var sys System
	de.call("GET", "/api/system", nil, http.StatusOK, &sys)
	if len(sys.Devices) != 4 || sys.Devices[0].ID != model.DeviceCUDA0 {
		t.Fatalf("system devices %+v", sys.Devices)
	}
	if p := sys.Power; p == nil || !p.Known || !p.OnBattery || p.BatteryPct == nil || *p.BatteryPct != 64 || p.BatteryAware != de.set.ASR().BatteryAware {
		t.Fatalf("power %+v", sys.Power)
	}
	ov := sys.OpenVINO
	if ov == nil || !ov.Available || ov.Version != "2026.4.1" || ov.GenAI != "2026.4.1.0" || !ov.CanConvert || ov.DetectedAt == nil ||
		len(ov.Devices) != 3 || ov.Devices[0] != (OVDeviceInfo{Name: "GPU.0", Kind: "igpu", FullName: igpuName, DriverVersion: "32.0.101.8724", DeviceID: model.DeviceOVGPU}) ||
		ov.Devices[2].DeviceID != model.DeviceOVCPU || len(ov.Ignored) != 1 || ov.Ignored[0].Reason != devices.ProbeReasonNotIntel || ov.Ignored[0].Kind != "" ||
		ov.Ignored[0].DeviceID != "" || ov.ErrorInfo != nil || ov.Python == "" {
		t.Fatalf("openvino %+v", ov)
	}
	// The OpenVINO checks follow cuda, each at most warn (I10, lead ruling 13).
	var names []string
	for _, c := range sys.Checks {
		names = append(names, c.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "gpu,cudnn,cuda,openvino_venv,openvino,openvino_devices,npu_driver,") || !strings.HasSuffix(joined, ",disk") { // ext-step1: cudnn
		t.Fatalf("check order %s", joined)
	}
	by := checksByName(sys)
	// .venv-ov is missing in this test: warn with the hint; the probe's
	// results are still reported (they come from the last detection).
	wantCheck(t, by["openvino_venv"], "check.openvino_venv_missing", "check.hint_openvino_venv")
	wantCheck(t, by["openvino"], "check.import_ok", "")
	wantCheck(t, by["openvino_devices"], "check.openvino_devices", "")
	if d, _ := by["openvino_devices"].DetailInfo.Params["devices"].(string); d != igpuName+" (GPU.0), "+npuName+" (NPU), Intel(R) Core(TM) Ultra 9 386H (CPU)" {
		t.Errorf("devices param %q", d)
	}
	wantCheck(t, by["npu_driver"], "check.npu_driver_ok", "")
	if v := by["npu_driver"].DetailInfo.Params["version"]; v != "32.0.100.4841" {
		t.Errorf("npu version %v", v)
	}
	for _, n := range []string{"openvino", "openvino_devices", "npu_driver"} {
		if by[n].Status != statusOK {
			t.Errorf("%s: %+v", n, by[n])
		}
	}
	if by["openvino_venv"].Status != statusWarn {
		t.Errorf("venv %+v", by["openvino_venv"])
	}
}

// The OpenVINO checks for the other detection outcomes: never error.
func TestOpenVINOCheckOutcomes(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	de.srv.diag.waitStartup()
	python := filepath.Join(t.TempDir(), "python.exe")
	if err := os.WriteFile(python, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	set := func(det devices.Detection) map[string]SystemCheck {
		t.Helper()
		// ext-step1: the reference laptop's NVIDIA GPU still answers (a
		// cuda:0 row alone no longer means an NVIDIA GPU)
		det.CUDAName = nvidiaName
		de.reg.mu.Lock()
		de.reg.det = det
		de.reg.mu.Unlock()
		var sys System
		de.call("GET", "/api/system", nil, http.StatusOK, &sys)
		by := checksByName(sys)
		for _, n := range []string{"openvino_venv", "openvino", "openvino_devices", "npu_driver"} {
			if by[n].Status == statusError || by[n].Status == "" {
				t.Errorf("%s: status %q", n, by[n].Status)
			}
		}
		return by
	}
	at := time.Now()
	// The import failed (worker --probe exit 2).
	by := set(devices.Detection{At: at, Python: python, OVErr: &model.ErrorInfo{Code: devices.CodeOVImportFailed,
		Params: map[string]any{"error": "No module named 'openvino'"}, Detail: "Traceback"}})
	wantCheck(t, by["openvino_venv"], "check.openvino_venv_ok", "")
	wantCheck(t, by["openvino"], "check.import_failed", "check.hint_openvino_venv")
	if p := by["openvino"].DetailInfo.Params; p["module"] != "openvino" || p["error"] != "No module named 'openvino'" || by["openvino"].Status != statusWarn {
		t.Errorf("import failed %+v", by["openvino"])
	}
	wantCheck(t, by["openvino_devices"], "check.api_skipped", "")
	wantCheck(t, by["npu_driver"], "check.api_skipped", "")
	// The probe failed otherwise (timeout).
	by = set(devices.Detection{At: at, Python: python, OVErr: &model.ErrorInfo{Code: devices.CodeProbeFailed, Params: map[string]any{"error": "timeout"}}})
	wantCheck(t, by["openvino"], "check.run_failed", "")
	// No Intel device, no NPU.
	by = set(devices.Detection{At: at, Python: python, OV: &devices.OVProbe{}})
	wantCheck(t, by["openvino"], "check.import_ok", "")
	wantCheck(t, by["openvino_devices"], "check.openvino_no_devices", "")
	wantCheck(t, by["npu_driver"], "check.npu_missing", "check.hint_npu_driver")
	if by["npu_driver"].Status != statusWarn || by["openvino_devices"].Status != statusWarn {
		t.Errorf("no devices: %+v %+v", by["openvino_devices"], by["npu_driver"])
	}
	// The NPU is listed but its properties could not be read: the driver.
	by = set(devices.Detection{At: at, Python: python, OV: &devices.OVProbe{
		Devices: []devices.OVDevice{{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: igpuName}},
		Ignored: []devices.OVDevice{{Name: "NPU", Reason: devices.ProbeReasonError, Error: "ZE_RESULT_ERROR_UNINITIALIZED"}}}})
	wantCheck(t, by["npu_driver"], "check.npu_driver_failed", "check.hint_npu_driver")
	if e := by["npu_driver"].DetailInfo.Params["error"]; e != "ZE_RESULT_ERROR_UNINITIALIZED" {
		t.Errorf("npu error %v", e)
	}
	// The first detection is still running.
	by = set(devices.Detection{Python: python})
	for _, n := range []string{"openvino", "openvino_devices", "npu_driver"} {
		wantCheck(t, by[n], checkPending, "")
	}
	// No .venv-ov at all: the other three are not checked.
	by = set(devices.Detection{At: at, Python: filepath.Join(t.TempDir(), "none.exe"),
		OVErr: &model.ErrorInfo{Code: devices.CodeOVPythonMissing, Params: map[string]any{"path": "none.exe"}}})
	wantCheck(t, by["openvino_venv"], "check.openvino_venv_missing", "check.hint_openvino_venv")
	for _, n := range []string{"openvino", "openvino_devices", "npu_driver"} {
		wantCheck(t, by[n], "check.api_skipped", "")
	}
}

// Lead ruling 13: a GPU / CUDA failure is an error while cuda:0 is enabled,
// a warning otherwise (the Intel devices still transcribe).
func TestGPUChecksFollowCUDADevice(t *testing.T) {
	failGPU := func(o *sched.Options) {
		o.GPUQuery = func(context.Context) (sched.GPUMem, error) { return sched.GPUMem{}, errors.New("nvidia-smi not found") }
	}
	failCmd := func(s *Server) {
		s.runCmd = func(ctx context.Context, name string, args ...string) (string, string, error) {
			if isScriptRun(args, "--selftest") {
				return "", "RuntimeError: CUDA failed", errUnknownCmd
			}
			return fakeCmd(ctx, name, args...)
		}
	}
	de := newDevEnv(t, envConfig{sched: failGPU, server: failCmd})
	de.srv.diag.waitStartup()
	var sys System
	de.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	by := checksByName(sys)
	if by["gpu"].Status != statusError || by["cuda"].Status != statusError {
		t.Fatalf("cuda:0 enabled: gpu %+v cuda %+v", by["gpu"], by["cuda"])
	}
	de.reg.mu.Lock()
	detects := de.reg.detects
	de.reg.mu.Unlock()
	if detects != 1 {
		t.Fatalf("recheck ran %d detections, want 1", detects)
	}
	de.call("PATCH", de.devicePath(model.DeviceCUDA0), map[string]any{"enabled": false}, http.StatusOK, nil)
	de.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	by = checksByName(sys)
	if by["gpu"].Status != statusWarn || by["cuda"].Status != statusWarn {
		t.Fatalf("cuda:0 disabled: gpu %+v cuda %+v", by["gpu"], by["cuda"])
	}
	wantCheck(t, by["gpu"], "check.gpu_unreadable", "check.hint_nvidia") // not the CPU hint: the Intel devices transcribe
	// Only CPU devices left: the CPU hint.
	de.call("PATCH", de.devicePath(model.DeviceOVGPU), map[string]any{"enabled": false}, http.StatusOK, nil)
	de.call("PATCH", de.devicePath(model.DeviceOVNPU), map[string]any{"enabled": false}, http.StatusOK, nil)
	de.call("PATCH", de.devicePath(model.DeviceOVCPU), map[string]any{"enabled": true}, http.StatusOK, nil)
	de.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	if c := checksByName(sys)["gpu"]; c.Status != statusWarn || c.HintInfo == nil || c.HintInfo.Code != "check.hint_cpu" {
		t.Fatalf("cpu only: %+v", c)
	}
}

// The asr label (brief section 5): "device · model · precision · language",
// the device's own name from the registry; the hover-card notes of the
// OpenVINO model preparation and first compile.
func TestASRLabelAndNotes(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	c := de.course("C")
	l := de.lecture(c.ID, "L")
	var lv Lecture
	de.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	// The v1.1-style snapshot of this env reads as the cuda:0 it maps to.
	if m := stageOf(lv.Stages, "asr").Model; m == nil || m.Label != nvidiaName+" · large-v3 · auto · en" || m.Model != "large-v3" {
		t.Fatalf("asr model %+v", m)
	}
	ctx := context.Background()
	j := de.moveJob(model.ScopeLecture, l.ID, "asr")
	snap := model.MarshalSnapshot(model.ASRSnapshot{DeviceID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo",
		Precision: "fp16", Language: "en", Auto: true})
	if err := de.st.SetJobSnapshot(ctx, j.ID, snap); err != nil {
		t.Fatal(err)
	}
	de.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	j = de.moveJob(model.ScopeLecture, l.ID, "asr", "queued", "running")
	for _, c := range []struct{ status, code, zh string }{
		{model.ASRStatusPreparingModel, state.CodeNotePreparingModel, "正在准备模型"},
		{model.ASRStatusCompilingModel, state.CodeNoteCompilingModel, "正在编译模型"},
		{model.ASRStatusDownloadingModel, state.CodeNoteDownloadingModel, "正在下载模型"},
	} {
		if err := de.st.SetJobStats(ctx, j.ID, model.JobStats{Status: c.status}); err != nil {
			t.Fatal(err)
		}
		var lv Lecture
		de.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
		st := stageOf(lv.Stages, "asr")
		if st.NoteInfo == nil || st.NoteInfo.Code != c.code || st.Note == nil || !strings.Contains(*st.Note, c.zh) {
			t.Errorf("%s: note %v %+v", c.status, st.Note, st.NoteInfo)
		}
		if st.Model == nil || st.Model.Label != npuName+" · large-v3-turbo · fp16 · en" {
			t.Errorf("asr model %+v", st.Model)
		}
	}
	// A device without a row: its id.
	gone := model.MarshalSnapshot(model.ASRSnapshot{DeviceID: "ov:XPU", Backend: model.BackendOpenVINO, Model: "small", Precision: "int8", Language: "en"})
	if err := de.st.SetJobSnapshot(ctx, j.ID, gone); err != nil {
		t.Fatal(err)
	}
	var lv2 Lecture
	de.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv2)
	if m := stageOf(lv2.Stages, "asr").Model; m == nil || m.Label != "ov:XPU · small · int8 · en" {
		t.Fatalf("unknown device label %+v", m)
	}
}

// The rerun dialog (A4.3) shows both asr snapshots with their devices.
func TestRerunDialogASRLabels(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	c := de.course("C")
	l := de.lecture(c.ID, "L")
	de.allDone(l.ID)
	var sv Settings
	de.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	a := sv.ASR
	a.Model = "large-v2"
	de.call("PUT", "/api/settings/asr", a, http.StatusOK, &sv)
	var nm NeedsModelChoice
	de.call("POST", "/api/lectures/"+l.ID+"/stages/asr/rerun", nil, http.StatusConflict, &nm)
	if len(nm.Diff) != 1 || nm.Diff[0].Stage != "asr" {
		t.Fatalf("diff %+v", nm.Diff)
	}
	d := nm.Diff[0]
	if d.Snapshot.Label != nvidiaName+" · large-v3 · auto · en" || d.Current.Label != nvidiaName+" · large-v2 · auto · en" {
		t.Fatalf("labels %q → %q", d.Snapshot.Label, d.Current.Label)
	}
	if d.Snapshot.ASR == nil || d.Snapshot.ASR.DeviceID != model.DeviceCUDA0 || d.Snapshot.ASR.Backend != model.BackendFasterWhisper ||
		d.Snapshot.ASR.Precision != "auto" || d.Snapshot.ASR.Model != "large-v3" {
		t.Fatalf("snapshot asr %+v", d.Snapshot.ASR)
	}
}
