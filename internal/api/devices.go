// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/stages"
)

// Transcription devices (SPEC-v2 I6-I10, phase 3b task E; the endpoints and
// types are frozen in types_intel.go). The registry owns the rows and the
// detection, the scheduler the pause switches, slots and self-tests; the
// handlers only translate. Every change reaches the front end as SSE
// "changed" {kind: "devices"} from the registry's OnChange hook and the
// scheduler's events (cmd wires both); the views never change state
// optimistically.

// deviceRegistry is the part of *devices.Registry the API uses (tests
// inject a fake).
type deviceRegistry interface {
	List() []model.ASRDevice
	Get(id string) (model.ASRDevice, bool)
	Present(id string) bool
	Detected() bool
	LastDetection() devices.Detection
	Detect(ctx context.Context) (devices.Detection, []string, error)
	Update(ctx context.Context, id string, p model.ASRDevicePatch) (model.ASRDevice, error)
	Reorder(ctx context.Context, ids []string) ([]model.ASRDevice, error)
	ApplyBenchmark(ctx context.Context, id, mdl, precision string) (model.ASRDevice, error)
	Prompts() []model.ASRDevice
	DismissPrompt(ctx context.Context, id string) error
}

// deviceScheduler is the device part of *sched.Scheduler (tests inject a
// fake).
type deviceScheduler interface {
	DeviceStatuses() []sched.DeviceStatus
	SetDevicePaused(id string, paused, cancelRunning bool) error
	DevicePaused(id string) bool
	Power() devices.Power
	StartDeviceTest(ctx context.Context, id string) (sched.DeviceTestState, error)
	CancelDeviceTest(id string) (sched.DeviceTestState, error)
	DeviceTest(id string) (sched.DeviceTestState, bool)
}

var (
	_ deviceRegistry  = (*devices.Registry)(nil)
	_ deviceScheduler = (*sched.Scheduler)(nil)
)

// requireDevices answers 503 when the server runs without the device
// registry (tests and builds before 3b); true when the handler may continue.
func (s *Server) requireDevices(w http.ResponseWriter) bool {
	if s.dev == nil || s.dsc == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "devices"})
		return false
	}
	return true
}

// deviceName is the device param of the devices.* codes: the row's label
// (the device's own name), or the id when there is no row.
func (s *Server) deviceName(id string) string {
	if s.dev != nil {
		if d, ok := s.dev.Get(id); ok && d.Label != "" {
			return d.Label
		}
	}
	return id
}

// deviceError maps the registry's and scheduler's errors (brief section 5):
// a coded error keeps its code (classify: 400, 404 with db.ErrNotFound, 409
// with state.ErrConflict); sched.ErrSelfTestRunning → 409 selftest.running;
// sched.ErrSelfTestPowerBattery → 409 selftest.power_battery {device}
// (power management, on battery only the battery device is tested); an unknown device
// (sched.ErrUnknownDevice, a bare db.ErrNotFound) → 404 devices.not_found
// {device}; sched.ErrDeviceTestUnavailable → 503 server.unavailable.
func (s *Server) deviceError(err error, id string) error {
	var ce *model.CodedError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sched.ErrSelfTestRunning):
		return apiErr(http.StatusConflict, sched.CodeSelfTestRunning, nil)
	case errors.Is(err, sched.ErrDeviceTestUnavailable):
		return apiErrDetail(http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "selftest"}, err.Error())
	case errors.As(err, &ce):
		// e.g. selftest.power_battery {device} (power management), which
		// wraps state.ErrConflict: 409 (codedStatus)
		return err
	case errors.Is(err, sched.ErrSelfTestPowerBattery):
		// the bare sentinel without its coded form: the same body
		return apiErrDetail(http.StatusConflict, sched.CodeSelfTestPowerBattery, map[string]any{"device": s.deviceName(id)}, err.Error())
	case errors.Is(err, db.ErrNotFound):
		return apiErrDetail(http.StatusNotFound, devices.CodeNotFound, map[string]any{"device": id}, err.Error())
	case errors.Is(err, devices.ErrInvalid):
		return apiErrDetail(http.StatusBadRequest, "request.invalid", nil, err.Error())
	}
	return err
}

// deviceOr404 returns the row of the path's device.
func (s *Server) deviceOr404(w http.ResponseWriter, r *http.Request) (model.ASRDevice, bool) {
	id := r.PathValue("id")
	d, ok := s.dev.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, devices.CodeNotFound, map[string]any{"device": id})
	}
	return d, ok
}

// ---- views ----

func rfc3339OrNil(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	v := t.UTC().Format(time.RFC3339)
	return &v
}

// selfTestView converts the scheduler's state; ok false (no test since the
// program started) is state idle.
func selfTestView(id string, st sched.DeviceTestState, ok bool) SelfTestView {
	if !ok || st.State == "" {
		return SelfTestView{DeviceID: id, State: sched.DeviceTestIdle}
	}
	v := SelfTestView{
		DeviceID: id, State: st.State, Step: st.Step, Steps: st.Steps, Progress: st.Progress,
		StartedAt: rfc3339OrNil(st.StartedAt), FinishedAt: rfc3339OrNil(st.FinishedAt),
		WaitingFor: st.WaitingFor,
	}
	if v.DeviceID == "" {
		v.DeviceID = id
	}
	if st.State == sched.DeviceTestRunning {
		v.Phase = st.Phase
	}
	if st.Candidate != nil {
		v.Candidate = &ModelPrecision{Model: st.Candidate.Model, Precision: st.Candidate.Precision}
	}
	if st.Error != nil {
		e := *st.Error
		v.ErrorInfo = &e
	}
	if st.Result != nil {
		b := *st.Result
		if b.Results == nil {
			b.Results = []model.BenchmarkResult{}
		}
		v.Result = &b
	}
	return v
}

// parseBenchmark decodes asr_device.last_benchmark (nil when never tested
// or unreadable).
func parseBenchmark(d model.ASRDevice) *model.DeviceBenchmark {
	if d.LastBenchmark == nil || *d.LastBenchmark == "" {
		return nil
	}
	var b model.DeviceBenchmark
	if err := json.Unmarshal([]byte(*d.LastBenchmark), &b); err != nil {
		slog.Warn("asr device: unreadable last_benchmark", "device", d.ID, "err", err)
		return nil
	}
	if b.Results == nil {
		b.Results = []model.BenchmarkResult{}
	}
	return &b
}

// deviceOptions are the choices of a device's settings row.
func deviceOptions(d model.ASRDevice) DeviceOptions {
	backend := d.Backend
	if backend == "" {
		backend = model.DeviceBackend(d.ID)
	}
	o := DeviceOptions{Models: devices.ModelChoices(backend), Precisions: devices.PrecisionChoices(backend)}
	if o.Models == nil {
		o.Models = []string{}
	}
	if o.Precisions == nil {
		o.Precisions = []string{}
	}
	if c, ok := devices.Recommended(d.ID); ok {
		o.Recommended = &ModelPrecision{Model: c.Model, Precision: c.Precision}
	}
	return o
}

// deviceCtx is what every view of one request shares: the scheduler's
// states, the open prompts, the self-test audio, the power policy's input
// and the lecture titles.
type deviceCtx struct {
	statuses map[string]sched.DeviceStatus
	prompts  map[string]bool
	noAudio  *model.ErrorInfo // selftest.no_audio, or nil
	// power is the self-test part of the power policy's input (machine with
	// a battery only; nil on a desktop, where the policy allows every test).
	power  *devices.PolicyInput
	titles map[string]model.Lecture
}

func (s *Server) newDeviceCtx(ctx context.Context) *deviceCtx {
	dc := &deviceCtx{statuses: map[string]sched.DeviceStatus{}, prompts: map[string]bool{}, titles: map[string]model.Lecture{}}
	for _, st := range s.dsc.DeviceStatuses() {
		dc.statuses[st.ID] = st
	}
	for _, p := range s.dev.Prompts() {
		dc.prompts[p.ID] = true
	}
	if ok, known := s.clipAvailable(ctx); known && !ok {
		dc.noAudio = &model.ErrorInfo{Code: stages.CodeSelfTestNoAudio}
	}
	dc.power = s.selfTestPolicyInput()
	return dc
}

// selfTestPolicyInput is the devices.PolicyInput that decides whether the
// power policy allows a self-test (devices.SelfTest Allowed), from the
// sources sched.StartDeviceTest reads (readPolicyBase): the scheduler's
// power reading and the registry's rows with their presence. Allowed
// depends on nothing else (runs, slots and pauses only decide Start, which
// the scheduler waits for). nil on a desktop, unknown power state included:
// the policy allows every test there.
func (s *Server) selfTestPolicyInput() *devices.PolicyInput {
	p := s.dsc.Power()
	if !p.BatteryMachine() {
		return nil
	}
	rows := s.dev.List()
	in := &devices.PolicyInput{Power: p, Devices: make([]devices.PolicyDevice, 0, len(rows))}
	for _, d := range rows {
		backend := d.Backend
		if backend == "" {
			backend = model.DeviceBackend(d.ID)
		}
		in.Devices = append(in.Devices, devices.PolicyDevice{ID: d.ID, Kind: model.DeviceKind(d.ID), Backend: backend,
			Enabled: d.Enabled, Present: s.dev.Present(d.ID)})
	}
	return in
}

// powerTestBlocked is the power policy's refusal of a self-test of d, with
// the code and params sched.StartDeviceTest refuses it with (sched
// testRefusal): selftest.power_cpu {device} for a CPU device on a machine
// with a battery (any power state), selftest.power_battery {device} on
// battery for a device that is not the battery device; device = the row's
// label (the id when blank). nil when the policy allows the test.
func powerTestBlocked(in *devices.PolicyInput, d model.ASRDevice) *model.ErrorInfo {
	if in == nil || devices.SelfTest(*in, d.ID).Allowed {
		return nil
	}
	code := sched.CodeSelfTestPowerBattery
	if model.DeviceKind(d.ID) == model.DeviceKindCPU {
		code = sched.CodeSelfTestPowerCPU
	}
	label := d.Label
	if strings.TrimSpace(label) == "" {
		label = d.ID
	}
	return &model.ErrorInfo{Code: code, Params: map[string]any{"device": label}}
}

// clipAvailable reports whether a self-test can find its audio (lead
// ruling 7: a repaired lecture). known is false when the lookup failed (the
// test is then allowed; it reports its own error).
func (s *Server) clipAvailable(ctx context.Context) (ok, known bool) {
	_, ok, err := s.findClip(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("self-test audio lookup failed", "err", err)
		}
		return false, false
	}
	return ok, true
}

func (dc *deviceCtx) lecture(ctx context.Context, st db.Store, id string) model.Lecture {
	if l, ok := dc.titles[id]; ok {
		return l
	}
	l, err := st.GetLecture(ctx, id)
	if err != nil {
		l = model.Lecture{ID: id}
	}
	dc.titles[id] = l
	return l
}

// selfTestBlocked is why "测试这台设备" is not available now, first reason
// first: the power policy refuses it (power management: selftest.power_cpu /
// selftest.power_battery {device}, the scheduler's refusal; first because
// the CPU of a machine with a battery is never testable, and fixing the
// other reasons does not lift a block on battery); no
// repaired lecture to take the clip from (selftest.no_audio, lead ruling 7);
// an OpenVINO device the last detection did not see
// (asr.ov_device_unavailable). POST .../selftest refuses with the same
// answer.
func (s *Server) selfTestBlocked(dc *deviceCtx, d model.ASRDevice) *model.ErrorInfo {
	if b := powerTestBlocked(dc.power, d); b != nil {
		return b
	}
	if dc.noAudio != nil {
		e := *dc.noAudio
		return &e
	}
	if model.DeviceBackend(d.ID) == model.BackendOpenVINO && !s.dev.Present(d.ID) {
		return &model.ErrorInfo{Code: stages.CodeASROVDeviceUnavailable, Params: map[string]any{"device": s.deviceName(d.ID)}}
	}
	return nil
}

func (s *Server) deviceView(ctx context.Context, dc *deviceCtx, d model.ASRDevice) ASRDeviceView {
	v := ASRDeviceView{
		ID: d.ID, Backend: d.Backend, Kind: model.DeviceKind(d.ID), Label: d.Label, Enabled: d.Enabled,
		Model: d.Model, Precision: d.Precision, MaxParallel: d.MaxParallel, Priority: d.Priority,
		DetectedAt: strOrNil(d.DetectedAt), Present: s.dev.Present(d.ID), New: dc.prompts[d.ID],
		Benchmark: parseBenchmark(d), Running: []DeviceTaskView{}, Options: deviceOptions(d),
	}
	if v.Backend == "" {
		v.Backend = model.DeviceBackend(d.ID)
	}
	if v.Label == "" {
		v.Label = d.ID
	}
	if model.IsSSHDevice(d.ID) {
		s.sshDeviceOptions(&v) // ssh-1b: the server's own recommendation (handlers_remote.go)
	}
	if v.Backend == model.BackendOpenVINO {
		ready := devices.OVModelReady(s.cfg, s.dataDir, d.Model, d.Precision) // component dir first (v3)
		cached := devices.CompileCached(s.dataDir, model.OVDeviceName(d.ID), d.Model, d.Precision)
		v.ModelReady, v.CompileCached = &ready, &cached
	}
	if st, ok := dc.statuses[d.ID]; ok {
		v.Paused, v.SlotsUsed, v.BatteryHold = st.Paused, st.SlotsUsed, st.BatteryHold
		for _, r := range st.Running {
			l := dc.lecture(ctx, s.st, r.LectureID)
			v.Running = append(v.Running, DeviceTaskView{JobID: r.JobID, LectureID: r.LectureID, LectureTitle: l.Title,
				CourseID: l.CourseID, Progress: r.Progress})
		}
	} else {
		v.Paused = s.dsc.DevicePaused(d.ID)
	}
	st, ok := s.dsc.DeviceTest(d.ID)
	v.SelfTest = selfTestView(d.ID, st, ok)
	v.SelfTestBlocked = s.selfTestBlocked(dc, d)
	return v
}

// deviceViews lists every device, priority order (nil without the registry).
func (s *Server) deviceViews(ctx context.Context) []ASRDeviceView {
	if s.dev == nil || s.dsc == nil {
		return nil
	}
	dc := s.newDeviceCtx(ctx)
	rows := s.dev.List()
	out := make([]ASRDeviceView, 0, len(rows))
	for _, d := range rows {
		out = append(out, s.deviceView(ctx, dc, d))
	}
	return out
}

// oneDeviceView is the view of one device as it is now.
func (s *Server) oneDeviceView(ctx context.Context, id string) (ASRDeviceView, error) {
	d, ok := s.dev.Get(id)
	if !ok {
		return ASRDeviceView{}, apiErr(http.StatusNotFound, devices.CodeNotFound, map[string]any{"device": id})
	}
	return s.deviceView(ctx, s.newDeviceCtx(ctx), d), nil
}

func (s *Server) respondDevice(w http.ResponseWriter, r *http.Request, id string) {
	v, err := s.oneDeviceView(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// devicePrompts are the workbench prompts (I9) for GET /api/dashboard.
func (s *Server) devicePrompts() []DevicePrompt {
	if s.dev == nil {
		return nil
	}
	var out []DevicePrompt
	for _, d := range s.dev.Prompts() {
		label := d.Label
		if label == "" {
			label = d.ID
		}
		out = append(out, DevicePrompt{DeviceID: d.ID, Label: label, Kind: model.DeviceKind(d.ID)})
	}
	return out
}

// powerView is the power state with the battery-awareness setting (power
// management: with has_battery, charging and the source, as the resource
// bar's, resources.PowerOf).
func (s *Server) powerView() *PowerView {
	if s.dsc == nil {
		return nil
	}
	aware := false
	if s.set != nil {
		aware = s.set.ASR().BatteryAware
	}
	v := resources.PowerOf(s.dsc.Power(), aware)
	return &v
}

// openVINOInfo is the OpenVINO environment of the last detection (I10).
func (s *Server) openVINOInfo() *OpenVINOInfo {
	if s.dev == nil {
		return nil
	}
	det := s.dev.LastDetection()
	v := &OpenVINOInfo{Python: det.Python, Devices: []OVDeviceInfo{}, Ignored: []OVDeviceInfo{}, DetectedAt: rfc3339OrNil(det.At)}
	if v.Python == "" && s.cfg != nil {
		v.Python = s.cfg.PythonOpenVINO
	}
	if det.OVErr != nil {
		e := *det.OVErr
		v.ErrorInfo = &e
	}
	if det.OV != nil {
		v.Available, v.Version, v.GenAI, v.CanConvert = true, det.OV.OpenVINO, det.OV.GenAI, det.OV.CanConvert
		for _, d := range det.OV.Devices {
			v.Devices = append(v.Devices, OVDeviceInfo{Name: d.Name, Kind: d.Kind, FullName: d.FullName,
				DriverVersion: d.DriverVersion, DeviceID: model.OVDeviceID(d.Kind)})
		}
		for _, d := range det.OV.Ignored {
			v.Ignored = append(v.Ignored, OVDeviceInfo{Name: d.Name, FullName: d.FullName, DriverVersion: d.DriverVersion, Reason: d.Reason})
		}
	}
	return v
}

// ---- handlers ----

// GET /api/asr/devices → []ASRDeviceView.
func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.deviceViews(r.Context()))
}

// detectDevices runs a detection, not bound to the request: a probe cut
// short would record a misleading failure.
func (s *Server) detectDevices(ctx context.Context) error {
	_, _, err := s.dev.Detect(context.WithoutCancel(ctx))
	s.hub.Changed("system", "") // the OpenVINO table and checks of the diagnostics page
	return err
}

// POST /api/asr/devices/detect → []ASRDeviceView (also run by the
// diagnostics "重新检查").
func (s *Server) postDetectDevices(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	if err := s.detectDevices(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.deviceViews(r.Context()))
}

// PATCH /api/asr/devices/{id} ASRDevicePatchInput → ASRDeviceView.
func (s *Server) patchDevice(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	id := r.PathValue("id")
	var in ASRDevicePatchInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Enabled == nil && in.Model == nil && in.Precision == nil && in.MaxParallel == nil {
		writeError(w, http.StatusBadRequest, "request.nothing_to_change", map[string]any{"fields": "enabled, model, precision, max_parallel"})
		return
	}
	if _, err := s.dev.Update(r.Context(), id, in); err != nil {
		s.fail(w, r, s.deviceError(err, id))
		return
	}
	s.respondDevice(w, r, id)
}

// PUT /api/asr/devices/order DeviceOrderInput → []ASRDeviceView.
func (s *Server) putDeviceOrder(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	var in DeviceOrderInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.dev.Reorder(r.Context(), in.IDs); err != nil {
		s.fail(w, r, s.deviceError(err, ""))
		return
	}
	writeJSON(w, http.StatusOK, s.deviceViews(r.Context()))
}

// POST /api/asr/devices/{id}/pause DevicePauseInput → ASRDeviceView (I8:
// one device; the top bar's switch pauses all, POST /api/system/gpu).
func (s *Server) pauseDevice(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	id := r.PathValue("id")
	var in struct {
		Paused        *bool `json:"paused"`
		CancelRunning bool  `json:"cancel_running"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Paused == nil {
		writeError(w, http.StatusBadRequest, "system.paused_missing", nil)
		return
	}
	if _, ok := s.deviceOr404(w, r); !ok {
		return
	}
	if err := s.dsc.SetDevicePaused(id, *in.Paused, *in.Paused && in.CancelRunning); err != nil {
		s.fail(w, r, s.deviceError(err, id))
		return
	}
	s.respondDevice(w, r, id)
}

// POST /api/asr/devices/{id}/selftest → 202 SelfTestView.
func (s *Server) startSelfTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	ctx := r.Context()
	d, ok := s.deviceOr404(w, r)
	if !ok {
		return
	}
	// The view's selftest_blocked, in the same order (the scheduler checks
	// the power policy again under its lock).
	dc := &deviceCtx{power: s.selfTestPolicyInput()}
	if ok, known := s.clipAvailable(ctx); known && !ok {
		dc.noAudio = &model.ErrorInfo{Code: stages.CodeSelfTestNoAudio}
	}
	if b := s.selfTestBlocked(dc, d); b != nil {
		writeErrorInfo(w, http.StatusConflict, *b)
		return
	}
	// The test runs in the background after the response: never bound to
	// the request's cancellation (DELETE cancels it).
	st, err := s.dsc.StartDeviceTest(context.WithoutCancel(ctx), d.ID)
	if err != nil {
		s.fail(w, r, s.deviceError(err, d.ID))
		return
	}
	writeJSON(w, http.StatusAccepted, selfTestView(d.ID, st, true))
}

// GET /api/asr/devices/{id}/selftest → SelfTestView.
func (s *Server) getSelfTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	d, ok := s.deviceOr404(w, r)
	if !ok {
		return
	}
	st, has := s.dsc.DeviceTest(d.ID)
	writeJSON(w, http.StatusOK, selfTestView(d.ID, st, has))
}

// DELETE /api/asr/devices/{id}/selftest → SelfTestView (cancel; idempotent:
// a test that is not waiting or running is returned as it is).
func (s *Server) cancelSelfTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	d, ok := s.deviceOr404(w, r)
	if !ok {
		return
	}
	st, err := s.dsc.CancelDeviceTest(d.ID)
	if err != nil {
		s.fail(w, r, s.deviceError(err, d.ID))
		return
	}
	writeJSON(w, http.StatusOK, selfTestView(d.ID, st, true))
}

// POST /api/asr/devices/{id}/selftest/apply SelfTestApplyInput →
// ASRDeviceView ("应用", I9).
func (s *Server) applySelfTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	id := r.PathValue("id")
	var in SelfTestApplyInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.dev.ApplyBenchmark(r.Context(), id, in.Model, in.Precision); err != nil {
		s.fail(w, r, s.deviceError(err, id))
		return
	}
	s.respondDevice(w, r, id)
}

// POST /api/asr/devices/{id}/prompt/dismiss → 204 (the workbench prompt of
// a new device, I9).
func (s *Server) dismissDevicePrompt(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevices(w) {
		return
	}
	d, ok := s.deviceOr404(w, r)
	if !ok {
		return
	}
	if err := s.dev.DismissPrompt(r.Context(), d.ID); err != nil {
		s.fail(w, r, s.deviceError(err, d.ID))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
