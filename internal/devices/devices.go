// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package devices owns the transcription devices of SPEC-v2 I6-I10: the
// asr_device rows, their detection (nvidia-smi for CUDA, the OpenVINO
// worker's --probe for the Intel GPU / NPU / CPU), the one-time migration of
// the legacy ASR settings, the current asr snapshot of the settings, power
// and memory readings, the OpenVINO model locations, the recommendation
// data and the self-test helpers.
//
// FROZEN by the lead for phase 3b (.claude/v3b-brief.md): the types and
// signatures below. Bodies: task B (Registry, power.go, mem.go) and task D
// (selftest.go, wer.go); recommend.go and paths.go are complete.
//
// The package never imports sched, settings, stages or api: the Registry
// implements sched.DeviceSource and settings.DeviceSource by method set.
package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// errNotImplemented is returned by the freeze stubs until task B / D
// delivers.
var errNotImplemented = errors.New("devices: not implemented yet (phase 3b)")

// ErrInvalid: invalid input of Update / Reorder / ApplyBenchmark (HTTP
// 400); always wrapped with a coded error (codes.go).
var ErrInvalid = errors.New("invalid device settings")

// OVProbe is the JSON of the OpenVINO worker's probe line
// (`{python_openvino} worker/asr_openvino.py --probe` prints one line
// "PROBE {json}" and exits 0; exit 2 when openvino cannot be imported).
type OVProbe struct {
	OpenVINO string `json:"openvino"` // openvino.__version__
	GenAI    string `json:"genai"`    // openvino_genai.__version__
	// Devices are the usable Intel devices, at most one per kind (igpu,
	// npu, cpu); the Go side maps kind → id (model.OVDeviceID).
	Devices []OVDevice `json:"devices"`
	// Ignored are devices OpenVINO lists but LectureFlow does not use
	// (Reason set): an NVIDIA card listed as GPU.1, a second Intel GPU.
	Ignored []OVDevice `json:"ignored"`
	// CanConvert: optimum-intel (with torch) imports, so a missing model
	// can be prepared; false = only already converted models run.
	CanConvert bool `json:"can_convert"`
}

// OVDevice is one OpenVINO device of a probe.
type OVDevice struct {
	Name          string `json:"name"`             // OpenVINO device name: GPU.0, NPU, CPU
	Kind          string `json:"kind"`             // model.DeviceKindIGPU | NPU | CPU ("" for ignored devices)
	FullName      string `json:"full_name"`        // FULL_DEVICE_NAME, becomes asr_device.label
	Vendor        string `json:"vendor"`           // "0x8086" for Intel; "" when unknown
	LUID          string `json:"luid"`             // DEVICE_LUID as hex (GPU, NPU): matches the PDH counters (I10); "" when unknown
	DriverVersion string `json:"driver_version"`   // GPU_DRIVER_VERSION / NPU_DRIVER_VERSION; "" when unknown
	Reason        string `json:"reason,omitempty"` // ignored only: ProbeReason*
	Error         string `json:"error,omitempty"`  // ignored with ProbeReasonError: the exception text
}

// Why a probe ignores a device (OVDevice.Reason).
const (
	ProbeReasonNotIntel       = "not_intel"        // e.g. the NVIDIA card OpenVINO lists as GPU.1
	ProbeReasonSecondIntelGPU = "second_intel_gpu" // lead ruling 10: one Intel GPU only
	ProbeReasonError          = "error"            // reading its properties failed
)

// Detection is the result of the last Detect.
type Detection struct {
	At       time.Time // when it finished; zero before the first detection
	CUDAName string    // nvidia-smi's name of GPU 0; "" = no NVIDIA GPU answered
	CUDAErr  string    // why nvidia-smi gave no name ("" when it did)
	Python   string    // the OpenVINO interpreter of the probe: the asr-openvino component's, else python_openvino
	OV       *OVProbe  // nil when the probe did not run or failed
	// OVErr is why there are no OpenVINO devices: CodeOVNotInstalled,
	// CodeOVPythonMissing, CodeOVImportFailed or CodeProbeFailed (Detail =
	// the raw text); nil when the probe ran.
	OVErr *model.ErrorInfo
	// MLX is the label of the Apple GPU when mlx:gpu was found (SPEC-v3 V5:
	// Apple Silicon and the python_mlx interpreter exists); "" otherwise.
	MLX string
}

// RegistryOptions injects the outside world (tests; nil = the real one).
type RegistryOptions struct {
	// Probe runs the OpenVINO probe. nil = the worker
	// (cfg.WorkerPython(openvino): the asr-openvino component, else
	// cfg.PythonOpenVINO; cfg.WorkerDir/asr_openvino.py --probe, 60 s
	// timeout). In test mode (cfg.ASR.FakeResult set) the default probe
	// never starts a process: it reports ov:GPU and ov:NPU only when
	// cfg.ASR.FakeResultOpenVINO is set, otherwise no OpenVINO device.
	Probe func(ctx context.Context) (*OVProbe, error)
	// CUDAName reads the NVIDIA GPU's name. nil = `{cfg.NvidiaSMI}
	// --query-gpu=name --format=csv,noheader` (first line); in test mode a
	// fixed fake name without a process.
	CUDAName func(ctx context.Context) (string, error)
	// MLX reports the Apple GPU (SPEC-v3 V5) and its label. nil = the
	// platform's default (mlx_darwin.go: Apple Silicon and the asr-mlx
	// component's interpreter, else cfg.PythonMLX,
	// exists; never on other systems, never in test mode).
	MLX func(ctx context.Context) (label string, ok bool)
	Now func() time.Time // nil = time.Now
}

// Registry is the in-memory view of asr_device plus the detection state.
// Reads (List, Get, Present, Detected, LastDetection, Prompts,
// CurrentASRSnapshot, Usable) never touch the database; writes go to the
// database first, refresh the cache, then call the OnChange hooks outside
// any lock (like settings.Service).
type Registry struct {
	st      db.Store
	cfg     *config.Config
	dataDir string
	o       RegistryOptions
	// goos is GOOS when the registry was made: the system whose rules
	// apply (fw:cpu, platform.go).
	goos string

	mu    sync.RWMutex
	hooks []func()
	// rows is the cache of asr_device, priority DESC then id.
	rows []model.ASRDevice
	// dismissed are the devices whose workbench prompt was closed (kv
	// asr_device_prompt_dismissed:{id}).
	dismissed map[string]bool
	// detected: a Detect finished since Load; present is then the set of
	// ids it saw (fw:cpu always counts as present).
	detected bool
	present  map[string]bool
	last     Detection
	// remote: colab:remote is in the device table (SPEC-v3 V14; SetRemote,
	// remote.go). Its row stays in the database but List hides it while
	// this is false.
	remote bool
	// ssh: the SSH inference servers (ssh:*) that are usable now
	// (SetSSHPresent, remote.go; ssh-mvp). Never part of a detection.
	ssh map[string]bool

	// wmu serialises the writes (database write + cache reload) so a slower
	// reload never stores an older state over a newer one; mu is never held
	// across database I/O. detectMu lets one detection run at a time; it is
	// taken before wmu, never after.
	wmu      sync.Mutex
	detectMu sync.Mutex
}

// NewRegistry returns an empty registry; call Load before use.
func NewRegistry(st db.Store, cfg *config.Config, dataDir string, o RegistryOptions) *Registry {
	if cfg == nil {
		cfg = config.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Registry{st: st, cfg: cfg, dataDir: dataDir, o: o, goos: GOOS, dismissed: map[string]bool{}, present: map[string]bool{}}
}

// Load (re)reads the rows and the dismissed-prompt marks into the cache.
// Until the first Detect finishes, a row counts as present when its
// detected_at is set (a device seen before is assumed to be there).
func (r *Registry) Load(ctx context.Context) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if err := r.reload(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	r.detected, r.present, r.last = false, map[string]bool{}, Detection{}
	r.mu.Unlock()
	return nil
}

// reload reads the rows and the dismissed marks into the cache (caller
// holds wmu).
func (r *Registry) reload(ctx context.Context) error {
	rows, err := r.st.ListASRDevices(ctx)
	if err != nil {
		return fmt.Errorf("读取转写设备失败: %w", err)
	}
	dismissed := make(map[string]bool, len(rows))
	for _, d := range rows {
		v, ok, err := r.st.GetKV(ctx, model.KVASRDevicePromptDismissedPrefix+d.ID)
		if err != nil {
			return err
		}
		dismissed[d.ID] = ok && strings.TrimSpace(v) != "" && v != "0"
	}
	sortRows(rows)
	r.mu.Lock()
	r.rows, r.dismissed = rows, dismissed
	r.mu.Unlock()
	return nil
}

// sortRows orders rows priority DESC, then id (the Store's order; kept
// here so the cache never depends on it).
func sortRows(rows []model.ASRDevice) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Priority != rows[j].Priority {
			return rows[i].Priority > rows[j].Priority
		}
		return rows[i].ID < rows[j].ID
	})
}

// changed calls the OnChange hooks (outside every lock).
func (r *Registry) changed() {
	r.mu.RLock()
	hooks := append([]func(){}, r.hooks...)
	r.mu.RUnlock()
	for _, fn := range hooks {
		fn()
	}
}

func (r *Registry) nowRFC3339() string { return r.o.Now().UTC().Format(time.RFC3339) }

// MigrateLegacy turns the legacy ASR settings into rows, once (kv
// model.KVASRDevicesMigrated): device cuda → cuda:0 with model, precision
// = compute_type, max_parallel of a, priority DefaultDevicePriority, label
// the NVIDIA name (o.CUDAName), created when an NVIDIA GPU answers or the
// database has an asr job that ran (an existing CUDA user whose nvidia-smi
// failed this time; label "NVIDIA GPU", detected_at NULL); device cpu →
// fw:cpu the same way (always created, except where faster-whisper cannot
// run: macOS, mac-opti M5). The migrated row's workbench
// prompt is marked dismissed (it is not a new device). A fresh install
// without NVIDIA gets no cuda:0. migrated reports whether rows were
// written in this call.
//
// Task B addition (NOTES "决定（3b B）"): with device cpu and an NVIDIA GPU
// that answers, cuda:0 is created as well but disabled (its prompt
// dismissed), so the user's choice of the CPU stays in effect after the
// first detection instead of auto mode moving every job to the GPU.
func (r *Registry) MigrateLegacy(ctx context.Context, a model.ASRSettings) (migrated bool, err error) {
	if v, ok, err := r.st.GetKV(ctx, model.KVASRDevicesMigrated); err != nil {
		return false, err
	} else if ok && strings.TrimSpace(v) != "" {
		return false, nil
	}
	// nvidia-smi runs before the transaction (never hold the database while
	// a process runs). A Mac has no NVIDIA GPU (mac-fix): nvidia-smi is not
	// run there, so the migration logs nothing about it (test mode keeps
	// the other systems' rules, platform.go).
	cudaName := ""
	if r.goos != "darwin" || r.testMode() {
		if name, err := r.cudaName(ctx); err == nil {
			cudaName = strings.TrimSpace(name)
		} else {
			slog.Info("asr devices: nvidia-smi did not answer during the migration", "err", err)
		}
	}
	mdl := strings.TrimSpace(a.Model)
	if mdl == "" {
		mdl = "large-v3"
	}
	precision := a.ComputeType
	if !model.ValidPrecision(model.BackendFasterWhisper, precision) {
		precision = model.PrecisionAuto
	}
	maxPar := min(max(a.MaxParallel, 1), MaxParallelLimit)
	now := r.nowRFC3339()
	legacyRow := func(id, label string, detected bool, enabled bool) model.ASRDevice {
		d := model.ASRDevice{ID: id, Backend: model.BackendFasterWhisper, Label: label, Enabled: enabled,
			Model: mdl, Precision: precision, MaxParallel: maxPar, Priority: model.DefaultDevicePriority(id)}
		if detected {
			d.DetectedAt = &now
		}
		return d
	}

	var created []string
	r.wmu.Lock()
	err = r.st.Tx(ctx, func(tx db.Store) error {
		if v, ok, err := tx.GetKV(ctx, model.KVASRDevicesMigrated); err != nil {
			return err
		} else if ok && strings.TrimSpace(v) != "" {
			return nil
		}
		var rows []model.ASRDevice
		if a.Device == "cpu" {
			if r.fwCPU() { // mac-opti M5: no fw:cpu where faster-whisper cannot run
				rows = append(rows, legacyRow(model.DeviceFWCPU, LabelFWCPU, true, true))
			}
			if cudaName != "" {
				rows = append(rows, legacyRow(model.DeviceCUDA0, cudaName, true, false))
			}
		} else {
			switch {
			case cudaName != "":
				rows = append(rows, legacyRow(model.DeviceCUDA0, cudaName, true, true))
			default:
				ran, err := asrRan(ctx, tx)
				if err != nil {
					return err
				}
				if ran {
					rows = append(rows, legacyRow(model.DeviceCUDA0, LabelCUDAUnknown, false, true))
				}
			}
		}
		for _, d := range rows {
			if _, err := tx.GetASRDevice(ctx, d.ID); err == nil {
				continue // already there (an earlier detection): never overwritten
			} else if !errors.Is(err, db.ErrNotFound) {
				return err
			}
			if err := tx.InsertASRDevice(ctx, d); err != nil {
				return err
			}
			if err := tx.SetKV(ctx, model.KVASRDevicePromptDismissedPrefix+d.ID, "1"); err != nil {
				return err
			}
			created = append(created, d.ID)
		}
		return tx.SetKV(ctx, model.KVASRDevicesMigrated, "1")
	})
	if err == nil {
		err = r.reload(ctx)
	}
	r.wmu.Unlock()
	if err != nil {
		return false, fmt.Errorf("迁移旧的转写设置失败: %w", err)
	}
	if len(created) == 0 {
		return false, nil
	}
	slog.Info("asr devices: migrated the legacy ASR settings (one time)", "devices", created)
	r.changed()
	return true, nil
}

// asrRan reports whether the database has a lecture asr job that started
// once (an existing user of the CUDA transcription).
func asrRan(ctx context.Context, st db.Store) (bool, error) {
	jobs, err := st.ListCurrentJobsByStatus(ctx, model.StatusPending, model.StatusQueued, model.StatusRunning,
		model.StatusDone, model.StatusFailed, model.StatusCancelled, model.StatusStale)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.Stage == model.StageASR && j.StartedAt != nil {
			return true, nil
		}
	}
	return false, nil
}

// Detect reads the NVIDIA name and runs the OpenVINO probe, then syncs the
// rows: a device seen for the first time is inserted (model and precision
// Recommended(id), else Fallback; ov:CPU disabled, every other enabled;
// max_parallel 1; priority DefaultDevicePriority), a known one gets its
// label and detected_at refreshed (the user's enabled, model, precision,
// max_parallel and priority are never overwritten), a row not seen is
// kept and marked not present. newIDs are the ids of detected devices
// inserted by this call. Calls the OnChange hooks when anything changed.
// Safe to call concurrently (one detection at a time; a second caller
// waits for it).
//
// fw:cpu (faster-whisper on the CPU, never probed) always has a row (3b
// Gate round 1, item 1): when it has none, Detect inserts it — enabled
// only on a fresh install without any accelerator (no CUDA, Intel GPU or
// NPU row in the database, present or not, and none found by this
// detection; Gate round 2), disabled otherwise (the user can enable it),
// its workbench prompt dismissed (it is built in, not a new device), and
// not listed in newIDs. Where faster-whisper cannot run (macOS, mac-opti
// M5, platform.go) there is no fw:cpu: Detect neither inserts nor touches
// it. Then, with a component manager, applyCPUFallback (cpu_fallback.go)
// may turn on the CPU device of an installed backend, once.
func (r *Registry) Detect(ctx context.Context) (d Detection, newIDs []string, err error) {
	r.detectMu.Lock()
	defer r.detectMu.Unlock()

	d = Detection{}
	d.Python, _, _ = r.cfg.WorkerPython(model.BackendOpenVINO)
	if name, err := r.cudaName(ctx); err != nil {
		d.CUDAErr = err.Error()
	} else if name = strings.TrimSpace(name); name == "" {
		d.CUDAErr = "nvidia-smi: empty output"
	} else {
		d.CUDAName = name
	}
	if p, err := r.probe(ctx); err != nil {
		d.OVErr = probeErrorInfo(err)
	} else {
		d.OV = normalizeProbe(p)
	}
	if label, ok := r.mlx(ctx); ok {
		d.MLX = label
	}
	if err := ctx.Err(); err != nil {
		// Cut short (shutdown): keep the previous state rather than marking
		// every device absent.
		return Detection{}, nil, err
	}
	d.At = r.o.Now()

	// seen: id → label of every device this detection found.
	seen := map[string]string{}
	if d.CUDAName != "" {
		seen[model.DeviceCUDA0] = d.CUDAName
	}
	if d.MLX != "" {
		seen[model.DeviceMLXGPU] = d.MLX
	}
	if d.OV != nil {
		for _, ov := range d.OV.Devices {
			id := model.OVDeviceID(ov.Kind)
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue // the worker reports one per kind; keep the first
			}
			seen[id] = ovLabel(ov)
		}
	}
	ids := make([]string, 0, len(seen))
	accelerator := false // a CUDA, Intel GPU or NPU device was found
	for id := range seen {
		ids = append(ids, id)
		accelerator = accelerator || isAccelerator(id)
	}
	sort.Slice(ids, func(i, j int) bool {
		pi, pj := model.DefaultDevicePriority(ids[i]), model.DefaultDevicePriority(ids[j])
		if pi != pj {
			return pi > pj
		}
		return ids[i] < ids[j]
	})

	now := d.At.UTC().Format(time.RFC3339)
	r.wmu.Lock()
	err = r.st.Tx(ctx, func(tx db.Store) error {
		newIDs = nil
		for _, id := range ids {
			row, err := tx.GetASRDevice(ctx, id)
			switch {
			case errors.Is(err, db.ErrNotFound):
				nd := NewDeviceRow(id, seen[id])
				nd.DetectedAt = &now
				if err := tx.InsertASRDevice(ctx, nd); err != nil {
					return err
				}
				newIDs = append(newIDs, id)
			case err != nil:
				return err
			default:
				row.Label = seen[id]
				row.DetectedAt = &now
				if err := tx.UpdateASRDevice(ctx, row); err != nil {
					return err
				}
			}
		}
		// fw:cpu is never probed: it is always there, and always has a row
		// (except where faster-whisper cannot run, mac-opti M5).
		if !r.fwCPU() {
			return nil
		}
		row, err := tx.GetASRDevice(ctx, model.DeviceFWCPU)
		switch {
		case errors.Is(err, db.ErrNotFound):
			// The fallback is on only when nothing faster exists at all:
			// any accelerator row counts, also one not present now (e.g.
			// the cuda:0 MigrateLegacy made while nvidia-smi was silent).
			enabled := !accelerator
			if enabled {
				rows, err := tx.ListASRDevices(ctx)
				if err != nil {
					return err
				}
				for _, x := range rows {
					if isAccelerator(x.ID) {
						enabled = false
						break
					}
				}
			}
			nd := NewDeviceRow(model.DeviceFWCPU, LabelFWCPU)
			nd.Enabled = enabled
			nd.DetectedAt = &now
			if err := tx.InsertASRDevice(ctx, nd); err != nil {
				return err
			}
			return tx.SetKV(ctx, model.KVASRDevicePromptDismissedPrefix+model.DeviceFWCPU, "1")
		case err != nil:
			return err
		}
		row.DetectedAt = &now
		return tx.UpdateASRDevice(ctx, row)
	})
	if err == nil {
		err = r.reload(ctx)
	}
	if err == nil {
		present := make(map[string]bool, len(seen)+1)
		for id := range seen {
			present[id] = true
		}
		if r.fwCPU() {
			present[model.DeviceFWCPU] = true
		}
		r.mu.Lock()
		r.detected, r.present, r.last = true, present, d
		r.mu.Unlock()
	}
	r.wmu.Unlock()
	if err != nil {
		return d, nil, fmt.Errorf("同步转写设备失败: %w", err)
	}
	if len(newIDs) > 0 {
		slog.Info("asr devices: new devices detected", "devices", newIDs)
	}
	// next-33: a computer without an accelerator whose devices cannot
	// transcribe gets its CPU device of an installed backend (cpu_fallback.go)
	if _, err := r.applyCPUFallback(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("asr devices: turning on the CPU device failed; tried again at the next detection", "err", err)
	}
	r.changed()
	return d, newIDs, nil
}

// isAccelerator: a CUDA, Intel GPU, NPU or Apple GPU device (not a CPU,
// not the remote Colab runtime).
func isAccelerator(id string) bool { return isAcceleratorKind(model.DeviceKind(id)) }

// NewDeviceRow is the row a device seen for the first time gets (I6): model
// and precision Recommended(id), else Fallback(backend); ov:CPU disabled
// (the last resort), every other device enabled; max_parallel 1; priority
// DefaultDevicePriority(id). detected_at is left to the caller.
func NewDeviceRow(id, label string) model.ASRDevice {
	backend := model.DeviceBackend(id)
	c, ok := Recommended(id)
	if !ok {
		c = Fallback(backend)
	}
	return model.ASRDevice{ID: id, Backend: backend, Label: label, Enabled: id != model.DeviceOVCPU,
		Model: c.Model, Precision: c.Precision, MaxParallel: 1, Priority: model.DefaultDevicePriority(id)}
}

// Labels of rows whose device did not name itself.
const (
	// LabelCUDAUnknown is cuda:0 migrated while nvidia-smi did not answer;
	// the next detection replaces it with the card's name.
	LabelCUDAUnknown = "NVIDIA GPU"
	// LabelFWCPU is fw:cpu (faster-whisper on the CPU; never probed).
	LabelFWCPU = "CPU (faster-whisper)"
)

// ovLabel is the label of an OpenVINO device: its FULL_DEVICE_NAME, else
// a generic name of its kind.
func ovLabel(ov OVDevice) string {
	if s := strings.TrimSpace(ov.FullName); s != "" {
		return s
	}
	switch ov.Kind {
	case model.DeviceKindIGPU:
		return "Intel GPU"
	case model.DeviceKindNPU:
		return "Intel NPU"
	}
	if s := strings.TrimSpace(ov.Name); s != "" {
		return s
	}
	return "CPU"
}

// Detected reports whether a Detect has finished since Load.
func (r *Registry) Detected() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.detected
}

// LastDetection returns the result of the last Detect (zero before one).
func (r *Registry) LastDetection() Detection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d := r.last
	if d.OV != nil {
		cp := *d.OV
		cp.Devices = append([]OVDevice{}, d.OV.Devices...)
		cp.Ignored = append([]OVDevice{}, d.OV.Ignored...)
		d.OV = &cp
	}
	if d.OVErr != nil {
		e := *d.OVErr
		d.OVErr = &e
	}
	return d
}

// List returns every row, priority DESC then id (copies).
func (r *Registry) List() []model.ASRDevice {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.ASRDevice, 0, len(r.rows))
	for _, d := range r.rows {
		if r.remoteHiddenLocked(d.ID) {
			continue // SPEC-v3 V14: in the table only while Colab is enabled and authorised; ssh-mvp: SSH rows only with the SSH servers on
		}
		if r.hiddenLocked(d.ID) {
			continue // mac-opti M5: fw:cpu where faster-whisper cannot run
		}
		out = append(out, cloneDevice(d))
	}
	return out
}

// Get returns one row (none for fw:cpu where faster-whisper cannot run,
// mac-opti M5).
func (r *Registry) Get(id string) (model.ASRDevice, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.hiddenLocked(id) {
		return model.ASRDevice{}, false
	}
	if d, ok := r.rowLocked(id); ok {
		return cloneDevice(d), true
	}
	return model.ASRDevice{}, false
}

func (r *Registry) rowLocked(id string) (model.ASRDevice, bool) {
	for _, d := range r.rows {
		if d.ID == id {
			return d, true
		}
	}
	return model.ASRDevice{}, false
}

func cloneDevice(d model.ASRDevice) model.ASRDevice {
	if d.DetectedAt != nil {
		v := *d.DetectedAt
		d.DetectedAt = &v
	}
	if d.LastBenchmark != nil {
		v := *d.LastBenchmark
		d.LastBenchmark = &v
	}
	return d
}

// Present reports whether the last detection saw the device (before the
// first detection: whether its detected_at is set). fw:cpu is always
// present, except where faster-whisper cannot run (never; mac-opti M5).
func (r *Registry) Present(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.presentLocked(id)
}

func (r *Registry) presentLocked(id string) bool {
	if model.IsSSHDevice(id) {
		return r.ssh[id] // ssh-mvp: connected and set up (SetSSHPresent), never detected
	}
	if id == model.DeviceFWCPU {
		return r.fwCPU()
	}
	if isRemoteID(id) {
		return r.remote // never detected: Colab is enabled and authorised (SetRemote)
	}
	if r.detected {
		return r.present[id]
	}
	d, ok := r.rowLocked(id)
	return ok && d.DetectedAt != nil
}

// labelLocked is the device param of the coded errors: the row's label, or
// the id when there is no row.
func (r *Registry) labelLocked(id string) string {
	if d, ok := r.rowLocked(id); ok && d.Label != "" {
		return d.Label
	}
	return id
}

// MaxParallelLimit is the upper bound of a device's max_parallel (the same
// 1..8 as the legacy setting).
const MaxParallelLimit = 8

// Update applies a patch (validation: CodeModelMissing, CodeModelInvalid
// for OpenVINO, CodePrecisionInvalid, CodeMaxParallelInvalid 1..8;
// CodeNotFound) and returns the row as stored. Errors are
// *model.CodedError (errors.As) wrapping ErrInvalid or db.ErrNotFound.
func (r *Registry) Update(ctx context.Context, id string, p model.ASRDevicePatch) (model.ASRDevice, error) {
	var out model.ASRDevice
	r.wmu.Lock()
	err := r.st.Tx(ctx, func(tx db.Store) error {
		if r.hiddenLocked(id) {
			return notFound(id) // mac-opti M5: not a device of this system
		}
		d, err := tx.GetASRDevice(ctx, id)
		if errors.Is(err, db.ErrNotFound) {
			return notFound(id)
		} else if err != nil {
			return err
		}
		if err := applyPatch(&d, p); err != nil {
			return err
		}
		if err := tx.UpdateASRDevice(ctx, d); err != nil {
			return err
		}
		out = d
		return nil
	})
	if err == nil {
		err = r.reload(ctx)
	}
	r.wmu.Unlock()
	if err != nil {
		return model.ASRDevice{}, err
	}
	r.changed()
	if d, ok := r.Get(id); ok {
		return d, nil
	}
	return out, nil
}

// applyPatch validates the patched fields and applies them to d.
func applyPatch(d *model.ASRDevice, p model.ASRDevicePatch) error {
	if isRemoteID(d.ID) && (p.Model != nil || p.Precision != nil) {
		return invalid(CodeRemoteManaged, nil, "Colab 的模型与精度在 Colab 设置中修改")
	}
	if p.Model != nil {
		m := strings.TrimSpace(*p.Model)
		switch {
		case m == "":
			return invalid(CodeModelMissing, nil, "请为这台转写设备选择模型")
		case d.Backend == model.BackendOpenVINO && !ValidOVModel(m):
			return invalid(CodeModelInvalid, map[string]any{"value": m}, "Intel 设备不支持模型「%s」，请从列表中选择", m)
		case d.Backend == model.BackendMLX && !slices.Contains(ModelChoices(model.BackendMLX), m):
			return invalid(CodeModelInvalid, map[string]any{"value": m}, "Apple 设备不支持模型「%s」，请从列表中选择", m)
		}
		d.Model = m
	}
	if p.Precision != nil {
		v := strings.TrimSpace(*p.Precision)
		if !model.ValidPrecision(d.Backend, v) {
			return invalid(CodePrecisionInvalid, map[string]any{"value": v}, "精度「%s」不适用于这台设备", v)
		}
		d.Precision = v
	}
	if p.MaxParallel != nil {
		n := *p.MaxParallel
		if n < 1 || n > MaxParallelLimit {
			return invalid(CodeMaxParallelInvalid, map[string]any{"min": 1, "max": MaxParallelLimit},
				"并发数必须在 1 到 %d 之间", MaxParallelLimit)
		}
		d.MaxParallel = n
	}
	if p.Enabled != nil {
		d.Enabled = *p.Enabled
	}
	return nil
}

// Reorder rewrites the priorities from the drag order (ids = every row
// exactly once, highest priority first; CodeOrderInvalid otherwise) in one
// transaction and returns the rows in the new order.
//
// The new priorities reuse the current values (sorted, made strictly
// decreasing), so a device detected later still gets a priority on the
// same scale (model.DefaultDevicePriority) and lands next to its kind.
func (r *Registry) Reorder(ctx context.Context, ids []string) ([]model.ASRDevice, error) {
	r.wmu.Lock()
	err := r.st.Tx(ctx, func(tx db.Store) error {
		rows, err := tx.ListASRDevices(ctx)
		if err != nil {
			return err
		}
		byID := make(map[string]model.ASRDevice, len(rows))
		for _, d := range rows {
			byID[d.ID] = d
		}
		// Rows List hides (a colab:remote not present, fw:cpu where it
		// cannot run) keep the last places when the order leaves them out.
		if hidden := r.hiddenIDs(rows); len(hidden) > 0 && len(ids) == len(rows)-len(hidden) &&
			!slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(hidden, id) }) {
			ids = append(slices.Clone(ids), hidden...)
		}
		if len(ids) != len(rows) {
			return orderInvalid()
		}
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			if _, ok := byID[id]; !ok || seen[id] {
				return orderInvalid()
			}
			seen[id] = true
		}
		prios := make([]int, len(rows))
		for i, d := range rows {
			prios[i] = d.Priority
		}
		sort.Sort(sort.Reverse(sort.IntSlice(prios)))
		for i := 1; i < len(prios); i++ {
			if prios[i] >= prios[i-1] {
				prios[i] = prios[i-1] - 1
			}
		}
		for i, id := range ids {
			d := byID[id]
			if d.Priority == prios[i] {
				continue
			}
			d.Priority = prios[i]
			if err := tx.UpdateASRDevice(ctx, d); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = r.reload(ctx)
	}
	r.wmu.Unlock()
	if err != nil {
		return nil, err
	}
	r.changed()
	return r.List(), nil
}

// SetBenchmark stores a finished self-test (last_benchmark) and refreshes
// the cache; the device's workbench prompt disappears with it.
func (r *Registry) SetBenchmark(ctx context.Context, id string, b model.DeviceBenchmark) error {
	if b.Version == 0 {
		b.Version = model.BenchmarkVersion
	}
	if b.Results == nil {
		b.Results = []model.BenchmarkResult{}
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	s := string(raw)
	r.wmu.Lock()
	err = r.st.SetASRDeviceBenchmark(ctx, id, &s)
	if errors.Is(err, db.ErrNotFound) {
		err = notFound(id)
	}
	if err == nil {
		err = r.reload(ctx)
	}
	r.wmu.Unlock()
	if err != nil {
		return err
	}
	r.changed()
	return nil
}

// ApplyBenchmark is "应用" (I9): mdl / precision must be an OK result of
// the device's last_benchmark (CodeApplyNotTested otherwise); it updates
// the row's model and precision and sets the benchmark's applied_at.
func (r *Registry) ApplyBenchmark(ctx context.Context, id, mdl, precision string) (model.ASRDevice, error) {
	mdl, precision = strings.TrimSpace(mdl), strings.TrimSpace(precision)
	r.wmu.Lock()
	err := r.st.Tx(ctx, func(tx db.Store) error {
		if r.hiddenLocked(id) {
			return notFound(id) // mac-opti M5: not a device of this system
		}
		d, err := tx.GetASRDevice(ctx, id)
		if errors.Is(err, db.ErrNotFound) {
			return notFound(id)
		} else if err != nil {
			return err
		}
		notTested := invalid(CodeApplyNotTested, map[string]any{"model": mdl, "precision": precision},
			"%s · %s 没有在最近一次测试中成功运行，不能应用", mdl, precision)
		b, ok := ParseBenchmark(d.LastBenchmark)
		if !ok {
			return notTested
		}
		found := false
		for _, res := range b.Results {
			if res.OK && res.Model == mdl && res.Precision == precision {
				found = true
				break
			}
		}
		if !found {
			return notTested
		}
		if err := applyPatch(&d, model.ASRDevicePatch{Model: &mdl, Precision: &precision}); err != nil {
			return err
		}
		if err := tx.UpdateASRDevice(ctx, d); err != nil {
			return err
		}
		at := r.nowRFC3339()
		b.AppliedAt = &at
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		s := string(raw)
		return tx.SetASRDeviceBenchmark(ctx, id, &s)
	})
	if err == nil {
		err = r.reload(ctx)
	}
	r.wmu.Unlock()
	if err != nil {
		return model.ASRDevice{}, err
	}
	r.changed()
	d, _ := r.Get(id)
	return d, nil
}

// ParseBenchmark decodes asr_device.last_benchmark; ok is false when there
// is none or it is not valid JSON.
func ParseBenchmark(raw *string) (model.DeviceBenchmark, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return model.DeviceBenchmark{}, false
	}
	var b model.DeviceBenchmark
	if err := json.Unmarshal([]byte(*raw), &b); err != nil {
		return model.DeviceBenchmark{}, false
	}
	return b, true
}

// Prompts are the devices of the workbench prompt "检测到新的转写设备，测试
// 一下它的速度？" (I9): present, enabled, never self-tested, prompt not
// dismissed; priority order.
func (r *Registry) Prompts() []model.ASRDevice {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []model.ASRDevice{}
	for _, d := range r.rows {
		if d.Enabled && d.LastBenchmark == nil && !r.dismissed[d.ID] && r.presentLocked(d.ID) {
			out = append(out, cloneDevice(d))
		}
	}
	return out
}

// DismissPrompt closes a device's workbench prompt for good (kv
// model.KVASRDevicePromptDismissedPrefix+id = "1").
func (r *Registry) DismissPrompt(ctx context.Context, id string) error {
	r.wmu.Lock()
	err := r.st.Tx(ctx, func(tx db.Store) error {
		if r.hiddenLocked(id) {
			return notFound(id)
		}
		if _, err := tx.GetASRDevice(ctx, id); errors.Is(err, db.ErrNotFound) {
			return notFound(id)
		} else if err != nil {
			return err
		}
		return tx.SetKV(ctx, model.KVASRDevicePromptDismissedPrefix+id, "1")
	})
	if err == nil {
		err = r.reload(ctx)
	}
	r.wmu.Unlock()
	if err != nil {
		return err
	}
	r.changed()
	return nil
}

// OnChange registers fn, called after every change of a row, of presence or
// of the detection (cmd wires it to SSE "changed devices" and a scheduler
// kick).
func (r *Registry) OnChange(fn func()) {
	r.mu.Lock()
	r.hooks = append(r.hooks, fn)
	r.mu.Unlock()
}

// CurrentASRSnapshot implements settings.DeviceSource: the asr snapshot a
// launch writes now (A4.1, I6), always an auto snapshot since power
// management (power-brief §3.2: the scheduler binds the device the policy
// chooses when the job starts). a is read through
// model.NormalizeASRDeviceMode. Device mode auto: model.ASRDeviceSnapshot
// of the highest-priority enabled device that is present (none present:
// the highest-priority enabled row), auto = true. Device mode manual: the
// same over the rows of the manual list only (the list's effective device
// on AC power, lead ruling 9.2: A4.3 compares it), auto = true; a phase-3b
// single device id is manual [id]. ok is false when there is no such row
// (no enabled row in auto mode, no enabled row of the list); settings then
// writes the fallback auto snapshot (settings.FallbackASRSnapshot, bound
// when the job starts; 3b Gate round 1, item 2). language is a.Language.
func (r *Registry) CurrentASRSnapshot(a model.ASRSettings) (model.ASRSnapshot, bool) {
	a = model.NormalizeASRDeviceMode(a)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var first *model.ASRDevice
	for i := range r.rows {
		d := &r.rows[i]
		if !d.Enabled || (a.DeviceMode == model.ASRDeviceModeManual && !slices.Contains(a.ManualDevices, d.ID)) {
			continue
		}
		if r.remoteHiddenLocked(d.ID) || r.hiddenLocked(d.ID) {
			continue // not in the table (SPEC-v3 V14; ssh-mvp; mac-opti M5)
		}
		if r.presentLocked(d.ID) {
			return model.ASRDeviceSnapshot(*d, a.Language, true), true
		}
		if first == nil {
			first = d
		}
	}
	if first != nil {
		return model.ASRDeviceSnapshot(*first, a.Language, true), true
	}
	return model.ASRSnapshot{}, false
}

// Usable implements the asr part of the A4.3 usability check (lead ruling
// 2, v1.1 ruling 7): an auto snapshot is usable while some enabled device
// is present; a fixed one (and a v1.1 one, normalized) while its row
// exists, is enabled and is present. The reason is
// CodeSnapshotDeviceUnavailable {device}.
func (r *Registry) Usable(s model.ASRSnapshot) (bool, *model.ErrorInfo) {
	s = model.NormalizeASRSnapshot(s)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s.Auto {
		for _, d := range r.rows {
			if d.Enabled && r.presentLocked(d.ID) {
				return true, nil
			}
		}
	} else if d, ok := r.rowLocked(s.DeviceID); ok && d.Enabled && r.presentLocked(d.ID) {
		return true, nil
	}
	name := r.labelLocked(s.DeviceID)
	return false, &model.ErrorInfo{Code: CodeSnapshotDeviceUnavailable, Params: map[string]any{"device": name},
		Detail: fmt.Sprintf("原转写设备「%s」现在不可用（未检测到或已停用）", name)}
}

// ---------- coded errors ----------

// msgError is the wrapped error of a coded error: its text is the Chinese
// message (the API's detail), it matches its sentinel with errors.Is.
type msgError struct {
	kind error
	msg  string
}

func (e *msgError) Error() string { return e.msg }
func (e *msgError) Unwrap() error { return e.kind }

func coded(kind error, code string, params map[string]any, format string, a ...any) error {
	return &model.CodedError{Info: model.ErrorInfo{Code: code, Params: params},
		Err: &msgError{kind: kind, msg: fmt.Sprintf(format, a...)}}
}

func invalid(code string, params map[string]any, format string, a ...any) error {
	return coded(ErrInvalid, code, params, format, a...)
}

// notFound: device is the id (there is no row to take a label from).
func notFound(id string) error {
	return coded(db.ErrNotFound, CodeNotFound, map[string]any{"device": id}, "找不到转写设备「%s」", id)
}

func orderInvalid() error {
	return invalid(CodeOrderInvalid, nil, "设备顺序无效：需要列出每台设备各一次")
}
