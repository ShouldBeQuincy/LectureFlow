// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// cpuOnlyProbe is the OpenVINO probe of a computer without an Intel GPU or
// NPU (an AMD desktop): the CPU only.
func cpuOnlyProbe() *OVProbe {
	return &OVProbe{OpenVINO: "2026.4.1", Devices: []OVDevice{{Name: "CPU", Kind: model.DeviceKindCPU, FullName: "AMD Ryzen 7 7700"}}}
}

const (
	ovPy = "C:/c/asr-openvino/venv/python.exe"
	fwPy = "C:/c/asr-cuda/venv/python.exe"
)

// fallbackReg is a registry with a component manager (loc, changed by the
// test as components are installed) on a computer whose outside world is w.
func fallbackReg(t *testing.T, st db.Store, w *world, loc config.ComponentLocator) (*Registry, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Python, cfg.PythonOpenVINO, cfg.PythonMLX = "", "", ""
	cfg.Components = loc
	dataDir := t.TempDir()
	r := NewRegistry(st, cfg, dataDir, w.opts())
	if err := r.Load(tctx()); err != nil {
		t.Fatal(err)
	}
	return r, dataDir
}

func fallbackMark(t *testing.T, st db.Store) (cpuFallbackMark, bool) {
	t.Helper()
	v, ok, err := st.GetKV(tctx(), kvCPUFallback)
	if err != nil {
		t.Fatal(err)
	}
	var m cpuFallbackMark
	if ok && v != "" {
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			t.Fatalf("mark %q: %v", v, err)
		}
	}
	return m, ok && v != ""
}

func detect(t *testing.T, r *Registry) {
	t.Helper()
	if _, _, err := r.Detect(tctx()); err != nil {
		t.Fatal(err)
	}
}

// next-33: a computer without an accelerator that installed the
// recommendation (SPEC-v3 V3.4: asr-openvino + small int8, environment
// first, model second). Before: fw:cpu on (no asr-cuda), ov:CPU off, nothing
// transcribes. Now ov:CPU is turned on when its environment is installed,
// its model follows the installed one once that is there, and a user who
// turns it off keeps it off.
func TestCPUFallbackTurnsOnOpenVINOCPU(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	w.cuda = ""
	w.probe, w.probeErr = nil, model.Errorf(CodeOVPythonMissing, map[string]any{"path": ""}, errors.New("missing"))
	loc := taskLocator{instLocator: instLocator{py: map[string]string{}, models: map[string]string{}}, busy: map[string]bool{}}
	r, dataDir := fallbackReg(t, st, w, loc)

	// fresh install, nothing installed: fw:cpu on as before, nothing else
	detect(t, r)
	if d, ok := r.Get(model.DeviceFWCPU); !ok || !d.Enabled {
		t.Fatalf("fw:cpu %+v", d)
	}
	if _, ok := r.Get(model.DeviceOVCPU); ok {
		t.Fatal("ov:CPU without OpenVINO")
	}
	if _, ok := fallbackMark(t, st); ok {
		t.Fatal("the rule ran with nothing installed")
	}

	// asr-openvino installed, the model still downloading
	loc.py[model.BackendOpenVINO] = ovPy
	loc.busy["model/"+model.BackendOpenVINO+"/small/"+model.PrecisionINT8] = true
	w.set(func(w *world) { w.probe, w.probeErr = cpuOnlyProbe(), nil })
	detect(t, r)
	ov, ok := r.Get(model.DeviceOVCPU)
	if !ok || !ov.Enabled || ov.Model != "large-v3-turbo" || ov.Precision != model.PrecisionFP16 {
		t.Fatalf("ov:CPU after asr-openvino: %+v", ov)
	}
	if m, _ := fallbackMark(t, st); m.Device != model.DeviceOVCPU || !m.ModelPending {
		t.Fatalf("mark %+v", m)
	}
	if fw, _ := r.Get(model.DeviceFWCPU); !fw.Enabled {
		t.Fatal("fw:cpu was changed")
	}

	// the model is installed: ov:CPU uses it
	dir := filepath.Join(t.TempDir(), "ov-small-int8")
	writeOVModel(t, dir)
	loc.models[model.BackendOpenVINO+"/small/"+model.PrecisionINT8] = dir
	delete(loc.busy, "model/"+model.BackendOpenVINO+"/small/"+model.PrecisionINT8)
	detect(t, r)
	ov, _ = r.Get(model.DeviceOVCPU)
	if !ov.Enabled || ov.Model != "small" || ov.Precision != model.PrecisionINT8 {
		t.Fatalf("ov:CPU after the model: %+v", ov)
	}
	if st := WorkerState(r.cfg, dataDir, ov.Backend, ov.Model, ov.Precision); st != WorkerReady {
		t.Fatalf("ov:CPU cannot run: %q", st)
	}
	if m, _ := fallbackMark(t, st); m.Device != model.DeviceOVCPU || m.ModelPending {
		t.Fatalf("mark %+v", m)
	}
	if s, ok := r.CurrentASRSnapshot(model.ASRSettings{DeviceMode: model.ASRDeviceModeAuto, Language: "en"}); !ok || !s.Auto {
		t.Fatalf("snapshot %+v %v", s, ok)
	}

	// later detections change nothing
	before, _ := r.Get(model.DeviceOVCPU)
	detect(t, r)
	if after, _ := r.Get(model.DeviceOVCPU); after.Enabled != before.Enabled || after.Model != before.Model || after.Precision != before.Precision {
		t.Fatalf("changed again: %+v", after)
	}

	// the user picks another model (not installed) and later turns ov:CPU
	// off: neither is undone
	turbo, int8 := "large-v3-turbo", model.PrecisionINT8
	if _, err := r.Update(tctx(), model.DeviceOVCPU, model.ASRDevicePatch{Model: &turbo, Precision: &int8}); err != nil {
		t.Fatal(err)
	}
	detect(t, r)
	if d, _ := r.Get(model.DeviceOVCPU); d.Model != turbo || d.Precision != int8 {
		t.Fatalf("the user's model was replaced: %+v", d)
	}
	off := false
	if _, err := r.Update(tctx(), model.DeviceOVCPU, model.ASRDevicePatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	detect(t, r)
	if d, _ := r.Get(model.DeviceOVCPU); d.Enabled {
		t.Fatal("the user's off was undone")
	}
}

// A model the user chose while the rule waited for one: once it is
// installed nothing is left to match, and a later choice of a model that
// is not installed is not undone either.
func TestCPUFallbackPendingModelUserChoice(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	w.cuda, w.probe = "", cpuOnlyProbe()
	loc := taskLocator{instLocator: instLocator{py: map[string]string{model.BackendOpenVINO: ovPy}, models: map[string]string{}}, busy: map[string]bool{}}
	r, _ := fallbackReg(t, st, w, loc)
	detect(t, r)
	if m, _ := fallbackMark(t, st); m.Device != model.DeviceOVCPU || !m.ModelPending {
		t.Fatalf("mark %+v", m)
	}
	// the user picks medium int8 and installs it
	medium, int8 := "medium", model.PrecisionINT8
	if _, err := r.Update(tctx(), model.DeviceOVCPU, model.ASRDevicePatch{Model: &medium, Precision: &int8}); err != nil {
		t.Fatal(err)
	}
	loc.busy["model/"+model.BackendOpenVINO+"/medium/"+int8] = true
	detect(t, r)
	if m, _ := fallbackMark(t, st); !m.ModelPending {
		t.Fatal("the pending mark went while the model is still being installed")
	}
	dir := filepath.Join(t.TempDir(), "medium")
	writeOVModel(t, dir)
	loc.models[model.BackendOpenVINO+"/medium/"+int8] = dir
	delete(loc.busy, "model/"+model.BackendOpenVINO+"/medium/"+int8)
	detect(t, r)
	if m, _ := fallbackMark(t, st); m.Device != model.DeviceOVCPU || m.ModelPending {
		t.Fatalf("mark %+v", m)
	}
	if d, _ := r.Get(model.DeviceOVCPU); d.Model != medium || d.Precision != int8 {
		t.Fatalf("the user's model was replaced: %+v", d)
	}
	// later a model that is not installed: the user's, kept
	turbo := "large-v3-turbo"
	if _, err := r.Update(tctx(), model.DeviceOVCPU, model.ASRDevicePatch{Model: &turbo}); err != nil {
		t.Fatal(err)
	}
	detect(t, r)
	if d, _ := r.Get(model.DeviceOVCPU); d.Model != turbo {
		t.Fatalf("the user's later model was replaced: %+v", d)
	}
}

// The model installed before the device is turned on: it gets it at once;
// a row whose own model is installed keeps it.
func TestCPUFallbackModelAlreadyInstalled(t *testing.T) {
	for _, tc := range []struct {
		name, installed, precision string
		wantModel, wantPrecision   string
	}{
		{"recommended small int8", "small", model.PrecisionINT8, "small", model.PrecisionINT8},
		{"the row's own model", "large-v3-turbo", model.PrecisionFP16, "large-v3-turbo", model.PrecisionFP16},
		{"a model of no candidate", "medium", model.PrecisionINT4, "medium", model.PrecisionINT4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := regStore(t)
			w := newWorld()
			w.cuda, w.probe = "", cpuOnlyProbe()
			dir := filepath.Join(t.TempDir(), "model")
			writeOVModel(t, dir)
			loc := instLocator{py: map[string]string{model.BackendOpenVINO: ovPy},
				models: map[string]string{model.BackendOpenVINO + "/" + tc.installed + "/" + tc.precision: dir}}
			r, _ := fallbackReg(t, st, w, loc)
			detect(t, r)
			ov, _ := r.Get(model.DeviceOVCPU)
			if !ov.Enabled || ov.Model != tc.wantModel || ov.Precision != tc.wantPrecision {
				t.Fatalf("ov:CPU %+v", ov)
			}
			if m, _ := fallbackMark(t, st); m.Device != model.DeviceOVCPU || m.ModelPending {
				t.Fatalf("mark %+v", m)
			}
		})
	}
}

// fw:cpu turned on by default without its model (asr-cuda installed with
// another model) is not the rule's to change: ov:CPU, whose environment is
// installed too, is turned on instead.
func TestCPUFallbackLeavesAnEnabledDeviceAlone(t *testing.T) {
	st := regStore(t)
	w := newWorld()
	w.cuda, w.probe = "", cpuOnlyProbe()
	ovDir := filepath.Join(t.TempDir(), "ov")
	writeOVModel(t, ovDir)
	loc := instLocator{py: map[string]string{model.BackendFasterWhisper: fwPy, model.BackendOpenVINO: ovPy},
		models: map[string]string{
			model.BackendFasterWhisper + "/large-v3/" + model.PrecisionFloat16: "C:/c/fw-large",
			model.BackendOpenVINO + "/small/" + model.PrecisionINT8:            ovDir,
		}}
	r, _ := fallbackReg(t, st, w, loc)
	detect(t, r)
	if fw, _ := r.Get(model.DeviceFWCPU); !fw.Enabled || fw.Model != "small" || fw.Precision != model.PrecisionAuto {
		t.Fatalf("fw:cpu was changed: %+v", fw)
	}
	if ov, _ := r.Get(model.DeviceOVCPU); !ov.Enabled || ov.Model != "small" {
		t.Fatalf("ov:CPU %+v", ov)
	}
}

// Where a device works, or will: nothing changes. Nor on a computer with
// an accelerator row, without a component manager, or in test mode.
func TestCPUFallbackNotNeeded(t *testing.T) {
	ovOnly := func() instLocator {
		return instLocator{py: map[string]string{model.BackendOpenVINO: ovPy}, models: map[string]string{}}
	}
	for _, tc := range []struct {
		name  string
		cuda  string
		probe *OVProbe
		loc   config.ComponentLocator
		fake  bool
	}{
		// an NVIDIA card (cuda:0 waits for asr-cuda: its own wait, fresh-cuda)
		{"NVIDIA", rtx, cpuOnlyProbe(), ovOnly(), false},
		// an Intel GPU: ov:GPU is the device
		{"Intel GPU", "", intelProbe(), ovOnly(), false},
		// fw:cpu works (asr-cuda and its model installed)
		{"fw:cpu works", "", cpuOnlyProbe(), instLocator{py: map[string]string{model.BackendFasterWhisper: fwPy, model.BackendOpenVINO: ovPy},
			models: map[string]string{model.BackendFasterWhisper + "/small/" + model.PrecisionInt8Float16: "C:/c/fw-small"}}, false},
		// fw:cpu's environment is being installed
		{"fw:cpu installing", "", cpuOnlyProbe(), taskLocator{instLocator: ovOnly(),
			busy: map[string]bool{"python/" + model.BackendFasterWhisper: true}}, false},
		{"no component manager", "", cpuOnlyProbe(), nil, false},
		{"test mode", "", cpuOnlyProbe(), ovOnly(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := regStore(t)
			w := newWorld()
			w.cuda, w.probe = tc.cuda, tc.probe
			cfg := config.Default()
			cfg.Python, cfg.PythonOpenVINO, cfg.PythonMLX = "", "", ""
			cfg.Components = tc.loc
			if tc.fake {
				cfg.ASR.FakeResult = "testdata/fake_asr.json"
			}
			r := NewRegistry(st, cfg, t.TempDir(), w.opts())
			if err := r.Load(tctx()); err != nil {
				t.Fatal(err)
			}
			detect(t, r)
			if ov, ok := r.Get(model.DeviceOVCPU); ok && ov.Enabled {
				t.Fatalf("ov:CPU turned on: %+v", ov)
			}
			if _, ok := fallbackMark(t, st); ok {
				t.Fatal("the rule ran")
			}
		})
	}
}

// A mark the rule cannot read counts as "ran before": nothing is turned on.
func TestCPUFallbackUnreadableMark(t *testing.T) {
	st := regStore(t)
	if err := st.SetKV(tctx(), kvCPUFallback, "{broken"); err != nil {
		t.Fatal(err)
	}
	w := newWorld()
	w.cuda, w.probe = "", cpuOnlyProbe()
	r, _ := fallbackReg(t, st, w, instLocator{py: map[string]string{model.BackendOpenVINO: ovPy}, models: map[string]string{}})
	detect(t, r)
	if ov, _ := r.Get(model.DeviceOVCPU); ov.Enabled {
		t.Fatalf("ov:CPU turned on: %+v", ov)
	}
}

// jobCountingStore counts the asr history reads (asrRan) of the rule.
type jobCountingStore struct {
	db.Store
	reads atomic.Int32
}

func (s *jobCountingStore) ListCurrentJobsByStatus(ctx context.Context, statuses ...string) ([]model.Job, error) {
	s.reads.Add(1)
	return s.Store.ListCurrentJobsByStatus(ctx, statuses...)
}

// ovReady is a component manager with asr-openvino and its recommended
// model installed: everything the rule needs to turn ov:CPU on.
func ovReady(t *testing.T) instLocator {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ov-small-int8")
	writeOVModel(t, dir)
	return instLocator{py: map[string]string{model.BackendOpenVINO: ovPy},
		models: map[string]string{model.BackendOpenVINO + "/small/" + model.PrecisionINT8: dir}}
}

// R15 item 1: a fresh install (no asr job ever started; a lecture waiting
// in the queue does not count) still gets ov:CPU turned on.
func TestCPUFallbackFreshInstallWithQueuedLecture(t *testing.T) {
	st := regStore(t)
	insertASRJob(t, st, false) // queued, never started
	w := newWorld()
	w.cuda, w.probe = "", cpuOnlyProbe()
	r, _ := fallbackReg(t, st, w, ovReady(t))
	detect(t, r)
	if ov, _ := r.Get(model.DeviceOVCPU); !ov.Enabled || ov.Model != "small" || ov.Precision != model.PrecisionINT8 {
		t.Fatalf("ov:CPU on a fresh install: %+v", ov)
	}
	if m, _ := fallbackMark(t, st); m.Device != model.DeviceOVCPU || m.ModelPending {
		t.Fatalf("mark %+v", m)
	}
}

// R15 item 1: an upgrade where transcription ran before changes no device:
// the user's ov:CPU (a model chosen, then turned off) keeps its enabled
// switch, model and precision, fw:cpu is untouched, and the mark settles
// the rule, at this detection and every later one.
func TestCPUFallbackUpgradeKeepsTheUsersDevices(t *testing.T) {
	st := regStore(t)
	insertASRJob(t, st, true)
	w := newWorld()
	w.cuda, w.probe = "", cpuOnlyProbe()
	// 3.2.x (no rule): ov:CPU detected; the user picks a model and turns it off
	old, _ := fallbackReg(t, st, w, nil)
	detect(t, old)
	medium, int8, on, off := "medium", model.PrecisionINT8, true, false
	if _, err := old.Update(tctx(), model.DeviceOVCPU, model.ASRDevicePatch{Model: &medium, Precision: &int8, Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Update(tctx(), model.DeviceOVCPU, model.ASRDevicePatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	before, _ := old.Get(model.DeviceOVCPU)
	fwBefore, _ := old.Get(model.DeviceFWCPU)
	if _, ok := fallbackMark(t, st); ok {
		t.Fatal("a mark before the upgrade")
	}
	// the upgrade: asr-openvino and small int8 installed, no device can run
	r, _ := fallbackReg(t, st, w, ovReady(t))
	for i := 0; i < 2; i++ {
		detect(t, r)
		if ov, _ := r.Get(model.DeviceOVCPU); ov.Enabled || ov.Model != before.Model || ov.Precision != before.Precision {
			t.Fatalf("detection %d changed ov:CPU: %+v (was %+v)", i, ov, before)
		}
		if fw, _ := r.Get(model.DeviceFWCPU); fw.Enabled != fwBefore.Enabled || fw.Model != fwBefore.Model || fw.Precision != fwBefore.Precision {
			t.Fatalf("detection %d changed fw:cpu: %+v (was %+v)", i, fw, fwBefore)
		}
		if m, ok := fallbackMark(t, st); !ok || m.Device != markSettled || m.ModelPending {
			t.Fatalf("detection %d: mark %+v %v", i, m, ok)
		}
	}
}

// R15 item 1: a user who transcribes on Colab or an SSH server only (an
// enabled remote row, listed or not) does not get ov:CPU turned on, and
// the rule leaves no mark; once no remote device is enabled, a computer
// that never transcribed is a fresh install again.
func TestCPUFallbackRemoteOnlyUser(t *testing.T) {
	sshRow := model.ASRDevice{ID: model.SSHDeviceID("gpu-box"), Backend: model.BackendFasterWhisper, Label: "gpu-box",
		Model: "large-v3", Precision: model.PrecisionFloat16, MaxParallel: 1}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, r *Registry) string
	}{
		{"Colab", func(t *testing.T, r *Registry) string {
			if err := r.SetRemote(tctx(), true, colabRow("someone")); err != nil {
				t.Fatal(err)
			}
			return model.DeviceColab
		}},
		{"Colab not listed yet", func(t *testing.T, r *Registry) string {
			if err := r.SetRemote(tctx(), true, colabRow("someone")); err != nil {
				t.Fatal(err)
			}
			if err := r.SetRemote(tctx(), false, colabRow("someone")); err != nil {
				t.Fatal(err)
			}
			return model.DeviceColab
		}},
		{"SSH", func(t *testing.T, r *Registry) string {
			if err := r.UpsertSSH(tctx(), sshRow); err != nil {
				t.Fatal(err)
			}
			return sshRow.ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := regStore(t)
			w := newWorld()
			w.cuda, w.probe = "", cpuOnlyProbe()
			r, _ := fallbackReg(t, st, w, ovReady(t))
			id := tc.setup(t, r)
			for i := 0; i < 2; i++ {
				detect(t, r)
				if ov, _ := r.Get(model.DeviceOVCPU); ov.Enabled {
					t.Fatalf("detection %d: ov:CPU turned on beside %s: %+v", i, id, ov)
				}
				if _, ok := fallbackMark(t, st); ok {
					t.Fatalf("detection %d: the rule left a mark", i)
				}
			}
			off := false
			if _, err := r.Update(tctx(), id, model.ASRDevicePatch{Enabled: &off}); err != nil {
				t.Fatal(err)
			}
			detect(t, r)
			if ov, _ := r.Get(model.DeviceOVCPU); !ov.Enabled {
				t.Fatalf("ov:CPU with %s off on a fresh install: %+v", id, ov)
			}
		})
	}
}

// R15 item 1: once the mark is written the asr history is not read again
// (neither after the settled mark nor after the mark of a device the rule
// enabled), and turning everything off turns nothing back on.
func TestCPUFallbackMarkStopsTheCheck(t *testing.T) {
	for _, ran := range []bool{true, false} {
		t.Run(fmt.Sprintf("asr ran %v", ran), func(t *testing.T) {
			base := regStore(t)
			if ran {
				insertASRJob(t, base, true)
			}
			st := &jobCountingStore{Store: base}
			w := newWorld()
			w.cuda, w.probe = "", cpuOnlyProbe()
			r, _ := fallbackReg(t, st, w, ovReady(t))
			detect(t, r)
			m, ok := fallbackMark(t, st)
			if !ok {
				t.Fatal("no mark after the first detection")
			}
			if (ran && m.Device != markSettled) || (!ran && m.Device != model.DeviceOVCPU) {
				t.Fatalf("mark %+v", m)
			}
			// the user turns every CPU device off: nothing works, the rule
			// would act again if it read the history again
			off := false
			for _, id := range []string{model.DeviceOVCPU, model.DeviceFWCPU} {
				if _, err := r.Update(tctx(), id, model.ASRDevicePatch{Enabled: &off}); err != nil {
					t.Fatal(err)
				}
			}
			reads := st.reads.Load()
			for i := 0; i < 3; i++ {
				detect(t, r)
			}
			if got := st.reads.Load(); got != reads {
				t.Fatalf("the asr history was read %d more times after the mark", got-reads)
			}
			for _, id := range []string{model.DeviceOVCPU, model.DeviceFWCPU} {
				if d, _ := r.Get(id); d.Enabled {
					t.Fatalf("%s turned on again: %+v", id, d)
				}
			}
			if m2, _ := fallbackMark(t, st); m2 != m {
				t.Fatalf("mark changed: %+v -> %+v", m, m2)
			}
		})
	}
}
