// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
)

// GPUMem is one nvidia-smi reading in MB (first GPU).
type GPUMem struct {
	FreeMB  int
	TotalMB int
	UsedMB  int
}

// GPUQueryFunc reads the GPU memory. Injected in tests.
type GPUQueryFunc func(ctx context.Context) (GPUMem, error)

// NvidiaSMIQuery returns a GPUQueryFunc running
// `nvidia-smi --query-gpu=memory.free,memory.total,memory.used --format=csv,noheader,nounits`
// in nvidia-smi's own directory, not the service's current one
// (procutil.CommandInExeDir; SEC52 info-7).
func NvidiaSMIQuery(exe string) GPUQueryFunc {
	return func(ctx context.Context) (GPUMem, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := procutil.CommandInExeDir(ctx, exe, "--query-gpu=memory.free,memory.total,memory.used",
			"--format=csv,noheader,nounits").Output()
		if err != nil {
			return GPUMem{}, fmt.Errorf("nvidia-smi: %w", err)
		}
		return ParseNvidiaSMI(string(out))
	}
}

// ParseNvidiaSMI parses "free, total, used" from the first non-empty line.
func ParseNvidiaSMI(out string) (GPUMem, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			return GPUMem{}, fmt.Errorf("nvidia-smi: unexpected output %q", line)
		}
		var v [3]int
		for i := 0; i < 3; i++ {
			n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
			if err != nil {
				return GPUMem{}, fmt.Errorf("nvidia-smi: unexpected output %q", line)
			}
			v[i] = n
		}
		return GPUMem{FreeMB: v[0], TotalMB: v[1], UsedMB: v[2]}, nil
	}
	return GPUMem{}, fmt.Errorf("nvidia-smi: empty output")
}

func (s *Scheduler) fakeASR() bool { return s.cfg.ASR.FakeResult != "" }

// maxGPU is max_parallel of the current ASR settings (v1.1 A5.3, read on
// every call so a settings change applies to the next scan).
func (s *Scheduler) maxGPU() int {
	if s.fakeASR() {
		return 1 // SPEC §14: one free slot in fake mode
	}
	if n := s.set.ASR().MaxParallel; n >= 1 {
		return n
	}
	return 1
}

// gpuAdmit applies the GPU pool rules of SPEC §5 / §17.2 and fills r's GPU
// accounting when the job may start.
func (s *Scheduler) gpuAdmit(ctx context.Context, sc *scanState, r *run) string {
	s.mu.Lock()
	paused := s.paused
	n, reserved, legacyReserved := 0, 0, 0
	for _, x := range s.running {
		if x.res == ResourceGPU {
			n++
			reserved += max(x.estimateMB-x.peakMB, 0)
			legacyReserved += max(x.legacyEstMB-x.peakMB, 0)
		}
	}
	s.mu.Unlock()
	if paused {
		return model.WaitGPUPaused
	}
	if sc.gpuFull || n >= s.maxGPU() {
		sc.gpuFull = true // a slot freed later in this scan goes to the next one (scanState)
		return model.WaitGPUSlot
	}
	if s.fakeASR() {
		return ""
	}
	if !sc.gpuRead {
		sc.gpu, sc.gpuErr = s.gpuQ(ctx)
		sc.gpuRead = true
		s.rememberGPU(sc.gpu, sc.gpuErr)
	}
	if sc.gpuErr != nil {
		// Reading failed: allow a single GPU task (SPEC §5).
		if n > 0 {
			sc.gpuFull = true
			return model.WaitGPUSlot
		}
		return ""
	}
	est, legacy, mdl, ct := s.estimate(ctx, sc.gpu.TotalMB, &r.job)
	if why := vramFits(sc.gpu.FreeMB, s.set.ASR().VRAMReserveMB, n, est, reserved, legacy, legacyReserved); why != "" {
		return why
	}
	r.estimateMB, r.legacyEstMB, r.baseUsedMB, r.admitFreeMB, r.sample, r.computeType, r.asrModel = est, legacy, sc.gpu.UsedMB, sc.gpu.FreeMB, true, ct, mdl
	return ""
}

// vramFits is the VRAM admission of SPEC §17.2 as revised by T-PERF-02
// (user, 2026-11-11): free VRAM - vram_reserve_mb - the unreached
// estimates of the jobs running on the card (reserved: jobs started
// recently have not reached their peak yet, so two jobs cannot claim the
// same memory) >= the job's estimate, else model.WaitVRAM; the reserve is
// taken off for the first job too, as before. When n > 0 jobs already run
// on the card, the job must also fit by the formula before T-PERF-02
// (legacy estimates and legacyReserved): the smaller estimates by model
// let a first job start on a card that had none, never more parallel jobs
// than before (T10: two processes are only 1.18-1.36 times as fast, each
// lecture finishes later, two workers take about 6.7 GB of host memory;
// more parallel jobs were not approved). A job only that keeps out waits
// with model.WaitGPUSlot (R37 S1: "this card takes no further job";
// "not enough VRAM, X free, Y needed" with X > Y would read as a bug).
// "" when the job fits.
func vramFits(freeMB, reserveMB, n, est, reserved, legacy, legacyReserved int) string {
	switch {
	case freeMB-reserveMB-reserved < est:
		return model.WaitVRAM
	case n > 0 && freeMB-reserveMB-legacyReserved < legacy:
		return model.WaitGPUSlot
	}
	return ""
}

func (s *Scheduler) rememberGPU(m GPUMem, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastGPU.OK = err == nil
	s.lastGPU.Error = ""
	if err != nil {
		s.lastGPU.Error = err.Error()
		return
	}
	s.lastGPU.TotalMB, s.lastGPU.FreeMB, s.lastGPU.UsedMB = m.TotalMB, m.FreeMB, m.UsedMB
}

func vramKey(asrModel, computeType string) string {
	return model.KVVRAMObservedPrefix + asrModel + ":" + computeType
}

// asrTarget is the model, configured compute type and device an asr job
// runs with: its snapshot (decision 4), or the current ASR settings when the
// job has no valid snapshot (j nil: the current settings; with
// Options.Devices the cuda:0 row's model and precision).
func (s *Scheduler) asrTarget(j *model.Job) (asrModel, computeType, device string) {
	a := s.set.ASR()
	asrModel, computeType, device = a.Model, a.ComputeType, a.Device
	if j == nil {
		if s.devs != nil {
			if d, ok := s.devs.Get(model.DeviceCUDA0); ok {
				return d.Model, d.Precision, model.FWDevice(d.ID)
			}
		}
		return
	}
	if snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot); ok {
		asrModel = snap.Model
		if snap.ComputeType != "" {
			computeType = snap.ComputeType
		}
		if snap.Device != "" {
			device = snap.Device
		}
	}
	return
}

// observedVRAM returns the calibrated peak for the model and compute type (kv, cached).
func (s *Scheduler) observedVRAM(ctx context.Context, asrModel, computeType string) int {
	key := vramKey(asrModel, computeType)
	s.mu.Lock()
	v, ok := s.vramObs[key]
	s.mu.Unlock()
	if ok {
		return v
	}
	str, found, err := s.st.GetKV(ctx, key)
	if err != nil {
		return 0
	}
	if found {
		v, _ = strconv.Atoi(str)
	}
	s.mu.Lock()
	s.vramObs[key] = v
	s.mu.Unlock()
	return v
}

// estimate is vramEstimates for the model and resolved compute type job
// runs with (nil: the current ASR settings; with Options.Devices cuda:0's
// model and precision).
func (s *Scheduler) estimate(ctx context.Context, totalMB int, j *model.Job) (est, legacy int, asrModel, computeType string) {
	est, legacy, _, asrModel, computeType = s.estimateBump(ctx, totalMB, j)
	return est, legacy, asrModel, computeType
}

// estimateBump is estimate with the raise after running out of VRAM it
// used (vramBumpMB, read once; 0: none).
func (s *Scheduler) estimateBump(ctx context.Context, totalMB int, j *model.Job) (est, legacy, bump int, asrModel, computeType string) {
	asrModel, ct, device := s.asrTarget(j)
	computeType = config.ResolveComputeType(ct, device, totalMB)
	est, legacy, bump = s.vramEstimatesBump(ctx, asrModel, computeType)
	return est, legacy, bump, asrModel, computeType
}

// vramEstimates is the per-task VRAM estimate of a model at a resolved
// compute type (SPEC §17.2 as revised by T-PERF-02, user 2026-11-11):
//
//	est    = max(base, ceil(observed peak × 1.1)), where base is
//	         devices.VRAMBaseMB(vram_per_job_mb, model, compute type): the
//	         smaller of the setting and the T10 table, or the setting for
//	         a model or compute type the table does not know;
//	legacy = max(vram_per_job_mb, ceil(observed peak × 1.1)), the formula
//	         before T-PERF-02, which a second parallel job must fit too
//	         (vramFits).
//
// The calibration (vram_observed, from runs alone on the card) still only
// raises the estimate: nvidia-smi is sampled every 2 s and may miss the
// peak, and the table values already are measured peaks × 1.1. A run that
// ran out of VRAM raises it for a while, in memory only (vramBumpMB,
// raiseVRAMAfterOOM). On a card of compute capability below 7.0
// (preVolta) base is the setting, as before T-PERF-02: CTranslate2 runs
// float16 as float32 and int8_float16 as int8_float32 there, more than the
// table's values, and nothing was measured (R37 S5). vram_per_job_mb is
// read from the current settings.
func (s *Scheduler) vramEstimates(ctx context.Context, mdl, computeType string) (est, legacy int) {
	est, legacy, _ = s.vramEstimatesBump(ctx, mdl, computeType)
	return est, legacy
}

// vramEstimatesBump is vramEstimates with the raise it used (0: none).
func (s *Scheduler) vramEstimatesBump(ctx context.Context, mdl, computeType string) (est, legacy, bump int) {
	perJob := s.set.ASR().VRAMPerJobMB
	est, legacy = devices.VRAMBaseMB(perJob, mdl, computeType), perJob
	if s.preVolta(ctx) {
		est = perJob
	}
	if obs := s.observedVRAM(ctx, mdl, computeType); obs > 0 {
		v := (obs*11 + 9) / 10 // ceil(obs × 1.1) in integers
		est, legacy = max(est, v), max(legacy, v)
	}
	if bump = s.vramBumpMB(mdl, computeType); bump > 0 {
		est, legacy = max(est, bump), max(legacy, bump) // est <= legacy stays true
	}
	return est, legacy, bump
}

// VRAMEstimate is the per-task VRAM estimate the scheduler admits a job of
// mdl at the resolved computeType with (vramEstimates): the diagnostics
// price the model they suggest for a small card with it.
func (s *Scheduler) VRAMEstimate(ctx context.Context, mdl, computeType string) int {
	est, _ := s.vramEstimates(ctx, mdl, computeType)
	return est
}

// recordVRAM stores a successful run's observed peak in kv (only the
// maximum is kept). A run that overlapped with another GPU run is skipped:
// its increase over the pre-start baseline includes the other job's memory.
func (s *Scheduler) recordVRAM(r *run) {
	s.mu.Lock()
	sample, peak, ct, mdl, overlapped := r.sample, r.peakMB, r.computeType, r.asrModel, r.overlapped
	s.mu.Unlock()
	if mdl == "" {
		mdl = s.set.ASR().Model
	}
	if !sample || peak <= 0 {
		return
	}
	if overlapped {
		slog.Info("vram calibration skipped: another GPU job ran at the same time", "job", r.job.ID, "peak_increase_mb", peak)
		return
	}
	ctx := context.Background()
	key := vramKey(mdl, ct)
	if peak <= s.observedVRAM(ctx, mdl, ct) {
		return
	}
	if err := s.st.SetKV(ctx, key, strconv.Itoa(peak)); err != nil {
		slog.Error("record vram", "key", key, "err", err)
		return
	}
	s.mu.Lock()
	s.vramObs[key] = peak
	s.mu.Unlock()
}

// GPUComputeCapFunc reads the first GPU's compute capability as nvidia-smi
// prints it ("8.9"; "" or "[N/A]" when it is not reported). Injected in
// tests.
type GPUComputeCapFunc func(ctx context.Context) (string, error)

// NvidiaSMIComputeCap returns a GPUComputeCapFunc running
// `nvidia-smi --query-gpu=compute_cap --format=csv,noheader,nounits`
// (drivers from 510; CTranslate2 4 needs a newer one anyway), in
// nvidia-smi's own directory like NvidiaSMIQuery.
func NvidiaSMIComputeCap(exe string) GPUComputeCapFunc {
	return func(ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := procutil.CommandInExeDir(ctx, exe, "--query-gpu=compute_cap", "--format=csv,noheader,nounits").Output()
		if err != nil {
			return "", fmt.Errorf("nvidia-smi: %w", err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line, nil
			}
		}
		return "", nil
	}
}

// computeCapMajor is the major version of a compute capability ("6.1" →
// 6); ok false for anything else ("", "[N/A]").
func computeCapMajor(v string) (major int, ok bool) {
	head, _, _ := strings.Cut(strings.TrimSpace(v), ".")
	n, err := strconv.Atoi(head)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// capRetry is how long an unread compute capability is not asked again.
const capRetry = 10 * time.Minute

// preVolta reports whether the first GPU's compute capability is below 7.0
// (Pascal and older: GTX 10xx, Quadro P, Tesla P; R37 S5). It is read once
// (again after capRetry while it could not be read) and only once
// nvidia-smi answered the memory query (no NVIDIA card: never). Unknown is
// false: the table applies, as on the cards it was measured on.
func (s *Scheduler) preVolta(ctx context.Context) bool {
	s.mu.Lock()
	if s.capQ == nil || !s.lastGPU.OK || s.capKnown || (!s.capTried.IsZero() && time.Since(s.capTried) < capRetry) {
		old := s.capOld
		s.mu.Unlock()
		return old
	}
	s.capTried = time.Now()
	s.mu.Unlock()
	v, err := s.capQ(ctx)
	major, ok := computeCapMajor(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && ok {
		s.capKnown, s.capOld = true, major < 7
	} else if err != nil {
		slog.Warn("nvidia-smi compute capability", "err", err)
	}
	return s.capOld
}

// VRAMAvailMB is the VRAM the transcription tasks on the first GPU could
// have now if none ran, after the last nvidia-smi reading (the
// diagnostics' VRAM hint, R37 M1: it must judge as the scheduler admits a
// first job, free - reserve >= estimate): its free memory when none of the
// application's GPU tasks runs on it; while some run, free + used - what
// was in use when the earliest of them started (other programs: the tasks'
// own memory is not counted as theirs). ok is false without a successful
// reading or while a task without a baseline (started when nvidia-smi
// failed, or in fake ASR mode) runs there: the caller falls back to the
// total.
func (s *Scheduler) VRAMAvailMB() (mb int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.lastGPU
	if !g.OK || g.TotalMB <= 0 {
		return 0, false
	}
	base, running := 0, false
	for _, r := range s.running {
		if r.res != ResourceGPU || (s.devs != nil && r.device != model.DeviceCUDA0) {
			continue
		}
		if !r.sample {
			return 0, false
		}
		if !running || r.baseUsedMB < base {
			base = r.baseUsedMB
		}
		running = true
	}
	if !running {
		return g.FreeMB, true
	}
	return min(g.FreeMB+g.UsedMB-base, g.TotalMB), true
}

// cudaOOMTexts are the texts (lower case) of a GPU memory allocation
// failure in a faster-whisper worker's error (its stderr tail), from the
// CTranslate2 4 sources (src/cuda/utils.h: CUDA_CHECK throws "CUDA failed
// with error " + cudaGetErrorString, which is "out of memory" for
// cudaErrorMemoryAllocation; CUBLAS_CHECK "cuBLAS failed with status " +
// the status name; CUDNN_CHECK "cuDNN failed with status " +
// cudnnGetErrorString) and the CUDA, cuBLAS and cuDNN references
// (cudaErrorMemoryAllocation, CUBLAS_STATUS_ALLOC_FAILED,
// CUDNN_STATUS_ALLOC_FAILED). Each entry: every part must be in the text.
// Left out on purpose: CUBLAS_STATUS_NOT_INITIALIZED (often, not only, a
// lack of memory) and host memory errors (MemoryError, "Unable to
// allocate", std::bad_alloc).
//
// Checked against the CTranslate2 4.8.3 Windows library files: of these
// texts only "CUDA failed with error out of memory" and "cuBLAS failed with
// status CUBLAS_STATUS_ALLOC_FAILED" can actually occur there (no cuDNN
// error text is in that build; the generic {"cuda", "out of memory"} entry
// matches the first of those texts too). The cudaErrorMemoryAllocation and
// cuDNN entries never match with that version and stay as harmless. What
// this table can miss: a failed thrust allocation, which reaches Python as
// MemoryError("bad allocation…"), and a worker that crashes outright. A
// miss only means the estimate is not raised after that failure.
var cudaOOMTexts = [][]string{
	{"cuda failed with error out of memory"},
	{"cuda", "out of memory"}, // CUDA_ERROR_OUT_OF_MEMORY wordings: "CUDA error: out of memory", "CUDA out of memory"
	{"cudaerrormemoryallocation"},
	{"cublas_status_alloc_failed"},
	{"cudnn_status_alloc_failed"},
}

// cudaOOMText reports whether msg tells that the GPU ran out of memory.
func cudaOOMText(msg string) bool {
	m := strings.ToLower(msg)
	for _, parts := range cudaOOMTexts {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(m, p)
		}
		if all {
			return true
		}
	}
	return false
}

// isCUDAOOM is cudaOOMText of a run's error.
func isCUDAOOM(err error) bool { return err != nil && cudaOOMText(err.Error()) }

// vramBumpTarget is what the in-memory raises after running out of VRAM
// belong to: cuda:0's configured model and precision (asrTarget(nil)).
func (s *Scheduler) vramBumpTarget() string {
	mdl, precision, _ := s.asrTarget(nil)
	return mdl + "|" + precision
}

// syncVRAMBumps drops the raises after running out of VRAM when cuda:0's
// model or precision changed since they were made (checked on every scan
// and every estimate; R37b R1).
func (s *Scheduler) syncVRAMBumps() {
	tgt := s.vramBumpTarget()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.vramBump) > 0 && s.vramBumpTgt != tgt {
		clear(s.vramBump)
	}
	s.vramBumpTgt = tgt
}

// vramBumpMB is the raise of mdl at computeType after running out of VRAM
// (0: none).
func (s *Scheduler) vramBumpMB(mdl, computeType string) int {
	s.syncVRAMBumps()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vramBump[vramKey(mdl, computeType)]
}

// raiseVRAMAfterOOM raises the estimate of a CUDA run's model and compute
// type after the run ran out of VRAM (R37 S5, R37b R1), so the retry is
// not admitted with the estimate that just failed:
//
//	new = min(ceil(estimate × 1.25) rounded up to 100 MB, free VRAM when
//	      the run was admitted - vram_reserve_mb)
//
// and only when that is more than the estimate. The bound is what the run
// was admitted with: under the same conditions the retry starts again (no
// waiting for ever on a model that ran before); repeated failures end with
// the job's retry limit, as a failure the user sees. The raise lives in
// memory only (vram_observed keeps the measured peaks): a restart, or
// another model or precision on cuda:0 (syncVRAMBumps), drops it.
func (s *Scheduler) raiseVRAMAfterOOM(r *run) {
	s.syncVRAMBumps()
	reserve := s.set.ASR().VRAMReserveMB
	s.mu.Lock()
	defer s.mu.Unlock()
	if !r.sample || r.asrModel == "" || r.computeType == "" || r.estimateMB <= 0 {
		return
	}
	up := ((r.estimateMB*5+3)/4 + 99) / 100 * 100 // ceil(estimate × 1.25), to 100 MB
	bump := min(up, r.admitFreeMB-reserve)
	key := vramKey(r.asrModel, r.computeType)
	if bump <= r.estimateMB || bump <= s.vramBump[key] {
		return
	}
	if s.vramBump == nil {
		s.vramBump = map[string]int{}
	}
	s.vramBump[key] = bump
	slog.Warn("transcription ran out of GPU memory: VRAM estimate raised until restart or a model change", "job", r.job.ID,
		"model", r.asrModel, "compute_type", r.computeType, "estimate_mb", r.estimateMB, "new_estimate_mb", bump)
}
