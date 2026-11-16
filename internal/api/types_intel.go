// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
)

// SPEC-v2 part 3, I6-I10: transcription devices (phase 3b). FROZEN by the
// lead (.claude/v3b-brief.md, section E/F); handlers: task E, front end and
// mock: task F. Endpoints:
//
//	GET    /api/asr/devices                          → []ASRDeviceView
//	POST   /api/asr/devices/detect                   → []ASRDeviceView (also run by the diagnostics "重新检查")
//	PATCH  /api/asr/devices/{id}        ASRDevicePatchInput → ASRDeviceView | 400/404 {error}
//	PUT    /api/asr/devices/order       DeviceOrderInput    → []ASRDeviceView (drag order, rewrites priority)
//	POST   /api/asr/devices/{id}/pause  DevicePauseInput    → ASRDeviceView
//	POST   /api/asr/devices/{id}/selftest                → 202 SelfTestView | 409 selftest.running, selftest.no_audio
//	GET    /api/asr/devices/{id}/selftest                → SelfTestView
//	DELETE /api/asr/devices/{id}/selftest                → SelfTestView (cancel; idempotent)
//	POST   /api/asr/devices/{id}/selftest/apply SelfTestApplyInput → ASRDeviceView | 400 devices.apply_not_tested
//	POST   /api/asr/devices/{id}/prompt/dismiss          → 204
//
// Changes of existing endpoints: GET /api/settings carries the new ASR
// fields (model.ASRSettings) and Settings.Devices; PUT /api/settings/asr
// merges the body into the current settings before validating (lead
// ruling 15); GET /api/system adds System.Devices, Power, OpenVINO and the
// checks openvino_venv, openvino, openvino_devices, npu_driver (at most
// warn; gpu/cuda stay error only while cuda:0 is enabled, lead ruling
// 13); POST /api/system/gpu pauses every device (I8); GET /api/dashboard
// adds Dashboard.DevicePrompts; GET /api/resources adds the device cards;
// SSE sends "changed" {kind: "devices", id: ""} when a device, its pause,
// a self-test or the power state changes.

// ModelPrecision is a model + precision pair (self-test candidates and
// "应用").
type ModelPrecision struct {
	Model     string `json:"model"`
	Precision string `json:"precision"`
}

// DeviceOptions are the choices of a device's settings row.
type DeviceOptions struct {
	Models     []string `json:"models"`     // devices.ModelChoices(backend); faster-whisper also accepts other names
	Precisions []string `json:"precisions"` // devices.PrecisionChoices(backend)
	// Recommended is devices.Recommended(id) (the "推荐" tag); null when
	// there is none.
	Recommended *ModelPrecision `json:"recommended"`
}

// DeviceTaskView is one pipeline job running on a device.
type DeviceTaskView struct {
	JobID        string  `json:"job_id"`
	LectureID    string  `json:"lecture_id"`
	LectureTitle string  `json:"lecture_title"`
	CourseID     string  `json:"course_id"`
	Progress     float64 `json:"progress"`
}

// SelfTestView is a device's self-test (I9).
type SelfTestView struct {
	DeviceID string `json:"device_id"`
	// State: idle (none since the program started) | waiting | running |
	// done | failed | cancelled (sched.DeviceTest*).
	State string `json:"state"`
	// Phase while running: clip | preparing_model | compiling_model |
	// transcribing.
	Phase     string          `json:"phase,omitempty"`
	Step      int             `json:"step"`  // 1-based candidate index
	Steps     int             `json:"steps"` // number of candidates
	Candidate *ModelPrecision `json:"candidate,omitempty"`
	Progress  float64         `json:"progress"` // of the current candidate
	StartedAt *string         `json:"started_at"`
	// FinishedAt is set when the test ended.
	FinishedAt *string          `json:"finished_at,omitempty"`
	ErrorInfo  *model.ErrorInfo `json:"error_info,omitempty"`
	// Result is the benchmark of this test once it ended (the device's
	// Benchmark after it was stored).
	Result *model.DeviceBenchmark `json:"result,omitempty"`
	// WaitingFor (power management, backend Gate round 1, fix 5): why a
	// waiting test waits when the power-aware policy is the reason — on a
	// machine with a battery the waiting_for code power_queue:{device} of
	// the device that is active now (front end waiting.power_queue);
	// absent otherwise (always on a desktop). sched.DeviceTestState.
	WaitingFor string `json:"waiting_for,omitempty"`
}

// ASRDeviceView is one transcription device (GET /api/asr/devices, the
// devices of GET /api/settings and GET /api/system).
type ASRDeviceView struct {
	ID          string  `json:"id"`      // model.Device*: cuda:0 | ov:GPU | ov:NPU | ov:CPU | fw:cpu
	Backend     string  `json:"backend"` // faster-whisper | openvino
	Kind        string  `json:"kind"`    // cuda | igpu | npu | cpu (model.DeviceKind)
	Label       string  `json:"label"`   // the device's own name (never translated)
	Enabled     bool    `json:"enabled"`
	Model       string  `json:"model"`
	Precision   string  `json:"precision"`
	MaxParallel int     `json:"max_parallel"`
	Priority    int     `json:"priority"`
	DetectedAt  *string `json:"detected_at"`
	// Present: seen by the last detection (before the first one: seen
	// earlier). A device not present is shown greyed ("未检测到").
	Present bool `json:"present"`
	// Paused: the device's own pause switch (the global one is GPUInfo.Paused).
	Paused bool `json:"paused"`
	// New: the workbench prompt of this device is open (devices.Prompts).
	New bool `json:"new"`
	// ModelReady / CompileCached are for OpenVINO devices only (null for
	// faster-whisper): the configured model is converted
	// (devices.ModelReady) and compiled for this device
	// (devices.CompileCached). false shows "首次使用需要准备模型" / "首次使用
	// 需要编译（约 4 分钟）".
	ModelReady    *bool `json:"model_ready"`
	CompileCached *bool `json:"compile_cached"`
	// Benchmark is last_benchmark (null = never tested).
	Benchmark *model.DeviceBenchmark `json:"benchmark"`
	SelfTest  SelfTestView           `json:"selftest"`
	// SelfTestBlocked is why "测试这台设备" is not available now
	// (selftest.no_audio, ...; lead ruling 7); null = available.
	SelfTestBlocked *model.ErrorInfo `json:"selftest_blocked"`
	SlotsUsed       int              `json:"slots_used"`
	Running         []DeviceTaskView `json:"running"` // never null
	BatteryHold     bool             `json:"battery_hold"`
	Options         DeviceOptions    `json:"options"`
	// VRAMHint (GET /api/system only, cuda:0 only): the card's VRAM is
	// too small for the device's model, or the model's files are missing
	// and the card's recommendation is another model
	// (NVIDIAVRAMHint: code system.devices.vram_low |
	// system.devices.vram_model_missing; r6: system.devices.vram_low_missing |
	// system.devices.vram_low_missing_install when the model the card is
	// too small for is not installed either), or the scheduler's per-task
	// estimate does not fit (WithVRAMQueue: system.devices.vram_queue_only,
	// or params.extra of the others). Advice only; absent otherwise.
	VRAMHint *model.ErrorInfo `json:"vram_hint,omitempty"`
}

// ASRDevicePatchInput is the body of PATCH /api/asr/devices/{id}: absent
// fields are unchanged (model.ASRDevicePatch).
type ASRDevicePatchInput = model.ASRDevicePatch

// DeviceOrderInput is the body of PUT /api/asr/devices/order: every device
// id once, highest priority first.
type DeviceOrderInput struct {
	IDs []string `json:"ids"`
}

// DevicePauseInput is the body of POST /api/asr/devices/{id}/pause.
type DevicePauseInput struct {
	Paused        bool `json:"paused"`
	CancelRunning bool `json:"cancel_running,omitempty"`
}

// SelfTestApplyInput is the body of POST /api/asr/devices/{id}/selftest/apply.
type SelfTestApplyInput = ModelPrecision

// DevicePrompt is one workbench prompt "检测到新的转写设备，测试一下它的
// 速度？" (I9).
type DevicePrompt struct {
	DeviceID string `json:"device_id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
}

// PowerView is the power state (GET /api/system; the resource bar has
// resources.Power).
type PowerView = resources.Power

// OVDeviceInfo is one OpenVINO device of the diagnostics table.
type OVDeviceInfo struct {
	Name          string `json:"name"` // OpenVINO name: GPU.0, NPU, CPU
	Kind          string `json:"kind"` // igpu | npu | cpu; "" when ignored
	FullName      string `json:"full_name"`
	DriverVersion string `json:"driver_version"`
	DeviceID      string `json:"device_id,omitempty"` // the asr_device id it maps to; "" when ignored
	Reason        string `json:"reason,omitempty"`    // ignored: not_intel | second_intel_gpu | error
}

// OpenVINOInfo is the OpenVINO environment of the diagnostics page (I10).
type OpenVINOInfo struct {
	Python     string         `json:"python"`    // config python_openvino
	Available  bool           `json:"available"` // the probe ran
	Version    string         `json:"version,omitempty"`
	GenAI      string         `json:"genai_version,omitempty"`
	CanConvert bool           `json:"can_convert"`
	Devices    []OVDeviceInfo `json:"devices"` // never null
	Ignored    []OVDeviceInfo `json:"ignored"` // never null
	DetectedAt *string        `json:"detected_at"`
	// ErrorInfo is why it is not available (devices.ov_python_missing,
	// devices.ov_import_failed, devices.probe_failed).
	ErrorInfo *model.ErrorInfo `json:"error_info,omitempty"`
}
