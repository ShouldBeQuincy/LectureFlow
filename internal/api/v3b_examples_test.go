// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// The example files of the device endpoints (SPEC-v2 I6-I10, phase 3b task
// E) for the front end: testdata/api/asr_devices.json (GET
// /api/asr/devices), openvino.json (System.openvino) and
// system_devices_lowvram*.json (System.devices with cuda:0's vram_hint, r2
// N15 and T-PERF-02). They are written
// by the real view code from fixed data; regenerate them with
// LECTUREFLOW_UPDATE_EXAMPLES=1 go test ./internal/api -run TestDeviceExampleFiles.
// Like the other examples they must decode into the frozen types without
// unknown fields.

func exampleDeviceServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	st, err := db.Open(filepath.Join(t.TempDir(), "ex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateCourse(ctx, model.Course{ID: "01JC0URSE0000000000000000A", Name: "CSC1003 Cryptography"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateLecture(ctx, model.Lecture{ID: "01JLECTURE000000000000000B", CourseID: "01JC0URSE0000000000000000A",
		Title: "Week 3 · Vigenère and one-time pads", MediaPath: "media.mp4"}); err != nil {
		t.Fatal(err)
	}
	at := "2026-10-24T08:00:00Z"
	wer1, wer2 := 0.057, 0.058
	bench := model.MarshalSnapshot(model.DeviceBenchmark{Version: model.BenchmarkVersion, Status: model.BenchmarkDone,
		StartedAt: "2026-10-24T08:10:00Z", FinishedAt: "2026-10-24T08:16:40Z",
		Audio: model.BenchmarkAudio{Source: model.BenchmarkSourceLec, LectureID: "01JLECTURE000000000000000B", StartMS: 1510000, EndMS: 1630500, ReferenceWords: 312},
		Results: []model.BenchmarkResult{
			{Model: "large-v3-turbo", Precision: "fp16", OK: true, LoadS: 21.4, TranscribeS: 14.9, RTF: 0.124, WER: &wer1, FirstCompile: true, PeakMemMB: 2710, Words: true},
			{Model: "large-v3-turbo", Precision: "int8", OK: true, LoadS: 9.8, TranscribeS: 11.2, RTF: 0.093, WER: &wer2, Prepared: true, PeakMemMB: 1980, Words: true},
		},
		Pick: &model.BenchmarkPick{Model: "large-v3-turbo", Precision: "fp16", Rule: model.PickRuleBest}})
	reg := &fakeDevReg{
		rows: []model.ASRDevice{
			{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: nvidiaName, Enabled: true, Model: "large-v3", Precision: "auto", MaxParallel: 1, Priority: 300, DetectedAt: &at},
			{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Label: igpuName, Enabled: true, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 200, DetectedAt: &at, LastBenchmark: &bench},
			{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: npuName, Enabled: true, Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 100, DetectedAt: &at},
			{ID: model.DeviceOVCPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Core(TM) Ultra 9 386H", Enabled: false, Model: "large-v3-turbo", Precision: "int8", MaxParallel: 1, Priority: 0},
		},
		present:   map[string]bool{model.DeviceCUDA0: true, model.DeviceOVGPU: true, model.DeviceOVNPU: true},
		detected:  true,
		dismissed: map[string]bool{model.DeviceCUDA0: true},
		det: devices.Detection{At: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC), CUDAName: nvidiaName, Python: `.venv-ov\Scripts\python.exe`,
			OV: &devices.OVProbe{OpenVINO: "2026.4.1-22982-e213a147257-releases/2026/4", GenAI: "2026.4.1.0-3408-ab01d1771e4", CanConvert: true,
				Devices: []devices.OVDevice{
					{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: igpuName, Vendor: "0x8086", LUID: "e220010000000000", DriverVersion: "32.0.101.8724"},
					{Name: "NPU", Kind: model.DeviceKindNPU, FullName: npuName, Vendor: "0x8086", LUID: "d026010000000000", DriverVersion: "32.0.100.4841"},
					{Name: "CPU", Kind: model.DeviceKindCPU, FullName: "Intel(R) Core(TM) Ultra 9 386H", Vendor: "0x8086"},
				},
				Ignored: []devices.OVDevice{{Name: "GPU.1", FullName: nvidiaName + " (dGPU)", Vendor: "0x10de", Reason: devices.ProbeReasonNotIntel}},
			}},
	}
	ds := &fakeDevSched{reg: reg, paused: map[string]bool{model.DeviceOVCPU: true},
		statuses: map[string]sched.DeviceStatus{
			model.DeviceCUDA0: {SlotsUsed: 1, SlotsMax: 1, Running: []sched.RunningASR{{JobID: "01JJOB0000000000000000000C", LectureID: "01JLECTURE000000000000000B", Progress: 0.42}}},
			model.DeviceOVNPU: {SlotsUsed: 1, SlotsMax: 1},
		},
		tests: map[string]sched.DeviceTestState{
			model.DeviceOVNPU: {DeviceID: model.DeviceOVNPU, State: sched.DeviceTestRunning, Phase: sched.DeviceTestPhaseCompiling, Step: 1, Steps: 3,
				Candidate: &devices.Candidate{Model: "large-v3-turbo", Precision: "fp16"}, Progress: 0, StartedAt: time.Date(2026, 10, 24, 8, 30, 0, 0, time.UTC)},
			model.DeviceOVGPU: {DeviceID: model.DeviceOVGPU, State: sched.DeviceTestDone, Step: 2, Steps: 3, Progress: 1,
				StartedAt: time.Date(2026, 10, 24, 8, 10, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 24, 8, 16, 40, 0, time.UTC)},
		},
	}
	var b model.DeviceBenchmark
	if err := json.Unmarshal([]byte(bench), &b); err != nil {
		t.Fatal(err)
	}
	done := ds.tests[model.DeviceOVGPU]
	done.Result = &b
	ds.tests[model.DeviceOVGPU] = done
	return &Server{st: st, dataDir: t.TempDir(), dev: reg, dsc: ds,
		findClip: func(context.Context) (devices.ClipSource, bool, error) { return devices.ClipSource{}, true, nil }}
}

func TestDeviceExampleFiles(t *testing.T) {
	s := exampleDeviceServer(t)
	ctx := context.Background()
	// r2 N15: System.devices on a 4 GB NVIDIA card (cuda:0 runs large-v3,
	// auto: its vram_hint; the per-task estimate of large-v3 int8_float16
	// is 3100 MB since T-PERF-02, which fits the card: this server has no
	// settings, so no reserve and no extra sentence)
	lowVRAM := s.deviceViews(ctx)
	s.vramHints(ctx, lowVRAM, GPUInfo{TotalMB: 4096, EstimatedJobMB: 3100})
	// T-PERF-02: the same card with cuda:0 at float16 (5000 MB, more than
	// the card): the sentence suggesting large-v3-turbo int8_float16
	lowVRAMFP16 := s.deviceViews(ctx)
	for i := range lowVRAMFP16 {
		if lowVRAMFP16[i].ID == model.DeviceCUDA0 {
			lowVRAMFP16[i].Precision = model.PrecisionFloat16
		}
	}
	s.vramHints(ctx, lowVRAMFP16, GPUInfo{TotalMB: 4096, EstimatedJobMB: 5000})
	// the same card after switching cuda:0 to large-v3-turbo (1600 MB): no
	// hint since T-PERF-02
	lowVRAMTurbo := s.deviceViews(ctx)
	for i := range lowVRAMTurbo {
		if lowVRAMTurbo[i].ID == model.DeviceCUDA0 {
			lowVRAMTurbo[i].Model = "large-v3-turbo"
		}
	}
	s.vramHints(ctx, lowVRAMTurbo, GPUInfo{TotalMB: 4096, EstimatedJobMB: 1600})
	files := map[string]struct {
		v   any
		dst func() any
	}{
		"asr_devices.json":                  {s.deviceViews(ctx), func() any { return &[]ASRDeviceView{} }},
		"openvino.json":                     {s.openVINOInfo(), func() any { return &OpenVINOInfo{} }},
		"system_devices_lowvram.json":       {lowVRAM, func() any { return &[]ASRDeviceView{} }},
		"system_devices_lowvram_fp16.json":  {lowVRAMFP16, func() any { return &[]ASRDeviceView{} }},
		"system_devices_lowvram_turbo.json": {lowVRAMTurbo, func() any { return &[]ASRDeviceView{} }},
	}
	dir := filepath.Join("..", "..", "testdata", "api")
	for name, f := range files {
		path := filepath.Join(dir, name)
		want, err := json.MarshalIndent(f.v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, '\n')
		if os.Getenv("LECTUREFLOW_UPDATE_EXAMPLES") == "1" {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Errorf("%s is out of date: regenerate it (LECTUREFLOW_UPDATE_EXAMPLES=1)", path)
		}
		dec := json.NewDecoder(bytes.NewReader(got))
		dec.DisallowUnknownFields()
		if err := dec.Decode(f.dst()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
