// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// r2 T-PERF-02 after review R37.

// stillWaits checks over several scans that the asr job of l stays queued
// for reason.
func (e *env) stillWaits(l model.Lecture, reason string) {
	e.t.Helper()
	e.waitFor("asr waits for "+reason, 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageASR)
		return j.Status == model.StatusQueued && e.s.WaitingFor(j) == reason
	})
	time.Sleep(60 * time.Millisecond)
	if j := e.lj(l.ID, model.StageASR); j.Status != model.StatusQueued || e.s.WaitingFor(j) != reason {
		e.t.Fatalf("asr %s / %q, want queued / %q", j.Status, e.s.WaitingFor(j), reason)
	}
}

// S1: a second job only the formula before T-PERF-02 keeps off the card
// waits for a slot, not for VRAM ("6092 MB free, 2600 MB needed" read as
// a bug). 8 GB card, large-v3-turbo int8_float16, max_parallel 2.
func TestVRAMSecondJobWaitsForSlot(t *testing.T) {
	g := newGate()
	e := newEnv(t, func(c *config.Config) {
		c.ASR.Model, c.ASR.ComputeType, c.ASR.MaxParallel = "large-v3-turbo", "int8_float16", 2
	}, gpuStages(g)...)
	e.gpu.set(8192-otherMB, 8192, otherMB)
	l1, l2 := e.lecture(), e.lecture()
	e.prepDone(l1, l2)
	e.start()
	e.waitStatus(l1.ID, model.StageASR, model.StatusRunning)
	// 7492 - 1000 - 1600 >= 1600, but 7492 - 1000 - 5000 < 5000
	e.stillWaits(l2, model.WaitGPUSlot)
	close(g.release)
	e.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

// S6: the running jobs' unreached estimates count with their sampled
// peaks: 12 GB card, large-v3 float16 (5000 MB), max_parallel 3.
func TestVRAMPeakAccounting(t *testing.T) {
	newPeakEnv := func(t *testing.T) (*env, *gate) {
		g := newGate()
		e := newEnv(t, func(c *config.Config) { c.ASR.MaxParallel = 3 }, gpuStages(g)...)
		e.gpu.set(12227-otherMB, 12227, otherMB)
		return e, g
	}
	// The first job reached 4400 MB (600 of its estimate still reserved):
	// 7127 - 1000 - 600 >= 5000 starts a second one; a third would need
	// 7127 - 1000 - 600 - 5000 = 527 >= 5000.
	t.Run("below the estimate", func(t *testing.T) {
		e, g := newPeakEnv(t)
		l1 := e.lecture()
		e.start()
		e.waitStatus(l1.ID, model.StageASR, model.StatusRunning)
		e.gpu.set(12227-otherMB-4400, 12227, otherMB+4400)
		if err := e.waitSampledPeak(l1.ID, 4400); err != nil {
			t.Fatal(err)
		}
		l2 := e.lecture()
		e.waitStatus(l2.ID, model.StageASR, model.StatusRunning)
		l3 := e.lecture()
		e.stillWaits(l3, model.WaitVRAM)
		close(g.release) // the released gate ends l3's run at once: wait for done
		e.waitStatus(l3.ID, model.StageASR, model.StatusDone)
	})
	// The first job took 6000 MB, more than its estimate: nothing of it is
	// reserved any more (not a negative reservation), 5527 - 1000 < 5000.
	t.Run("above the estimate", func(t *testing.T) {
		e, g := newPeakEnv(t)
		l1 := e.lecture()
		e.start()
		e.waitStatus(l1.ID, model.StageASR, model.StatusRunning)
		e.gpu.set(12227-otherMB-6000, 12227, otherMB+6000)
		if err := e.waitSampledPeak(l1.ID, 6000); err != nil {
			t.Fatal(err)
		}
		l2 := e.lecture()
		e.stillWaits(l2, model.WaitVRAM)
		close(g.release)
	})
	// A calibration of 6000 MB (estimate 6600): one job on the card.
	t.Run("calibrated", func(t *testing.T) {
		e, g := newPeakEnv(t)
		if err := e.st.SetKV(e.ctx, vramKey("large-v3", "float16"), "6000"); err != nil {
			t.Fatal(err)
		}
		l1, l2 := e.lecture(), e.lecture()
		e.prepDone(l1, l2)
		e.start()
		e.waitStatus(l1.ID, model.StageASR, model.StatusRunning)
		e.stillWaits(l2, model.WaitVRAM) // 11527 - 1000 - 6600 < 6600
		close(g.release)
	})
}

// oomEnv is a 4 GB card (otherMB in use) transcribing with
// large-v3-turbo auto (int8_float16, 1600 MB) whose asr runs return
// errs[n] for the n-th run (0-based) and, past the list, run until
// release is closed; estimates[n] is the GPU card's estimate when the n-th
// run started.
//
// runs counts the runs whose estimate is recorded: it moves only after the
// store (R8 BC-03), so a test that waits for runs >= n+1 reads the n-th
// estimate, never the zero before it (the runs of the one asr job follow
// each other). next hands out the run numbers.
type oomEnv struct {
	*env
	next      atomic.Int32
	runs      atomic.Int32
	estimates [8]atomic.Int32
	release   chan struct{}
}

func newOOMEnv(t *testing.T, errs ...error) *oomEnv {
	o := &oomEnv{release: make(chan struct{})}
	asr := &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(ctx context.Context, id string, p func(float64)) error {
		n := int(o.next.Add(1)) - 1
		if n < len(o.estimates) {
			o.estimates[n].Store(int32(o.s.GPUStatus(ctx).EstimatedJobMB))
		}
		o.runs.Add(1)
		if n < len(errs) {
			return errs[n]
		}
		select {
		case <-o.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	o.env = newEnv(t, func(cf *config.Config) { cf.ASR.Model = "large-v3-turbo" }, instant(model.StagePrep, ResourceCPU), asr)
	o.gpu.set(4096-otherMB, 4096, otherMB)
	return o
}

var (
	errCT2OOM   = errors.New("转写失败（退出码 1）：RuntimeError: CUDA failed with error out of memory")
	errCuDNNOOM = errors.New("转写失败（退出码 1）：RuntimeError: cuDNN failed with status CUDNN_STATUS_ALLOC_FAILED")
)

// R37b R1: a run that ran out of VRAM raises its estimate × 1.25 (to 100
// MB), bounded by the free VRAM it was admitted with less the reserve, in
// memory only: the retry still starts on the card it ran on. Two failures
// in a row raise twice (1600 → 2000 → 2396 = 3396 - 1000); vram_observed
// keeps the measured peaks; another scheduler (a restart) has the table
// value again.
func TestVRAMOutOfMemoryRaisesEstimate(t *testing.T) {
	o := newOOMEnv(t, errCT2OOM, errCuDNNOOM)
	l := o.lecture()
	o.start()
	o.waitFor("the third run", 5*time.Second, func() bool { return o.runs.Load() >= 3 })
	o.waitStatus(l.ID, model.StageASR, model.StatusRunning)
	for i, want := range []int32{1600, 2000, 2396} {
		if got := o.estimates[i].Load(); got != want {
			t.Errorf("run %d started with the estimate %d, want %d", i+1, got, want)
		}
	}
	if est := o.s.GPUStatus(o.ctx).EstimatedJobMB; est != 2396 {
		t.Errorf("estimate %d after two out-of-memory failures, want 2396", est)
	}
	// C108 (R37 recheck E1): the diagnostics say the estimate is raised
	if !o.s.GPUStatus(o.ctx).EstimateRaised {
		t.Error("GPUStatus.EstimateRaised is false after out-of-memory failures")
	}
	key := vramKey("large-v3-turbo", "int8_float16")
	if v, ok, _ := o.st.GetKV(o.ctx, key); ok {
		t.Errorf("%s = %s: the raise must not be stored", key, v)
	}
	if est := o.newScheduler().GPUStatus(o.ctx).EstimatedJobMB; est != 1600 {
		t.Errorf("after a restart the estimate is %d, want 1600", est)
	}
	if o.newScheduler().GPUStatus(o.ctx).EstimateRaised {
		t.Error("after a restart GPUStatus.EstimateRaised is still true")
	}
	// a calibration above the raise (2396) wins: ceil(2400 × 1.1) = 2640,
	// not raised
	o.s.mu.Lock()
	o.s.vramObs[key] = 2400
	o.s.mu.Unlock()
	if g := o.s.GPUStatus(o.ctx); g.EstimatedJobMB != 2640 || g.EstimateRaised {
		t.Errorf("calibrated above the raise: estimate %d, raised %v; want 2640, false", g.EstimatedJobMB, g.EstimateRaised)
	}
	close(o.release)
	o.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

// R37b R1: another model or precision on cuda:0 drops the raise (switching
// back has the table value); a failure that is not out of memory raises
// nothing.
func TestVRAMOutOfMemoryRaiseDropped(t *testing.T) {
	t.Run("model change", func(t *testing.T) {
		o := newOOMEnv(t, errCT2OOM)
		l := o.lecture()
		o.start()
		o.waitFor("the second run", 5*time.Second, func() bool { return o.runs.Load() >= 2 })
		if est := o.s.VRAMEstimate(o.ctx, "large-v3-turbo", "int8_float16"); est != 2000 {
			t.Fatalf("estimate %d after out of memory, want 2000", est)
		}
		setModel := func(mdl string) {
			a := o.s.set.ASR()
			a.Model = mdl
			if err := o.s.set.SetASR(o.ctx, a); err != nil {
				t.Fatal(err)
			}
		}
		setModel("medium")
		time.Sleep(60 * time.Millisecond) // several scans
		setModel("large-v3-turbo")
		if est := o.s.VRAMEstimate(o.ctx, "large-v3-turbo", "int8_float16"); est != 1600 {
			t.Errorf("estimate %d after switching away and back, want 1600", est)
		}
		if o.s.GPUStatus(o.ctx).EstimateRaised {
			t.Error("after switching away and back GPUStatus.EstimateRaised is still true")
		}
		close(o.release)
		o.waitStatus(l.ID, model.StageASR, model.StatusDone)
	})
	t.Run("not out of memory", func(t *testing.T) {
		o := newOOMEnv(t, errors.New("转写失败（退出码 1）：RuntimeError: unsupported audio"))
		l := o.lecture()
		o.start()
		o.waitFor("the second run", 5*time.Second, func() bool { return o.runs.Load() >= 2 })
		if got := o.estimates[1].Load(); got != 1600 {
			t.Errorf("the retry started with the estimate %d, want 1600", got)
		}
		if o.s.GPUStatus(o.ctx).EstimateRaised {
			t.Error("GPUStatus.EstimateRaised after a failure that is not out of memory")
		}
		close(o.release)
		o.waitStatus(l.ID, model.StageASR, model.StatusDone)
	})
}

// R37b B1: which worker errors count as the GPU running out of memory.
func TestCUDAOOMText(t *testing.T) {
	for _, c := range []struct {
		msg string
		oom bool
	}{
		{"RuntimeError: CUDA failed with error out of memory", true},
		{"RuntimeError: cuBLAS failed with status CUBLAS_STATUS_ALLOC_FAILED", true},
		{"RuntimeError: cuDNN failed with status CUDNN_STATUS_ALLOC_FAILED", true},
		{"CUDA error: out of memory", true},
		{"torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 20.00 MiB", true},
		{"cudaErrorMemoryAllocation", true},
		{"RuntimeError: cuBLAS failed with status CUBLAS_STATUS_NOT_INITIALIZED", false},
		{"RuntimeError: CUDA failed with error unspecified launch failure", false},
		{"MemoryError", false},
		{"numpy.core._exceptions._ArrayMemoryError: Unable to allocate 3.2 GiB", false},
		{"std::bad_alloc", false},
		{"out of memory", false}, // no GPU context
		{"", false},
	} {
		if got := cudaOOMText(c.msg); got != c.oom {
			t.Errorf("cudaOOMText(%q) = %v, want %v", c.msg, got, c.oom)
		}
	}
	if isCUDAOOM(nil) {
		t.Error("nil error is out of memory")
	}
}
