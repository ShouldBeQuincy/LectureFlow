// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// r2 T-PERF-02: the per-task VRAM estimate follows the model and resolved
// compute type (devices.VRAMBaseMB, the T10 measurements) instead of the
// fixed vram_per_job_mb, so a small card can start a model that fits; the
// reserve rule and the parallel jobs are what they were.

// otherMB is what other programs take on the card (T10's desktop baseline).
const otherMB = 700

// A 4 GB card with large-v3-turbo (auto: int8_float16, 1600 MB) starts
// the job; before T-PERF-02 it waited for VRAM for ever (5000 MB).
func TestVRAM4GBTurboStarts(t *testing.T) {
	g := newGate()
	e := newEnv(t, func(c *config.Config) { c.ASR.Model = "large-v3-turbo" }, gpuStages(g)...)
	e.gpu.set(4096-otherMB, 4096, otherMB)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageASR, model.StatusRunning)
	if est := e.s.GPUStatus(e.ctx).EstimatedJobMB; est != 1600 {
		t.Errorf("estimate %d, want 1600 (large-v3-turbo int8_float16)", est)
	}
	close(g.release)
	e.waitStatus(l.ID, model.StageASR, model.StatusDone)
}

// The same card with large-v3: float16 (5000 MB) or auto (int8_float16,
// 3100 MB) still waits, with the model's estimate on the GPU card.
func TestVRAM4GBLargeWaits(t *testing.T) {
	for _, c := range []struct {
		ct  string
		est int
	}{{"float16", 5000}, {"auto", 3100}, {"int8_float16", 3100}} {
		t.Run(c.ct, func(t *testing.T) {
			g := newGate()
			e := newEnv(t, func(cf *config.Config) { cf.ASR.Model, cf.ASR.ComputeType = "large-v3", c.ct }, gpuStages(g)...)
			e.gpu.set(4096-otherMB, 4096, otherMB)
			l := e.lecture()
			e.start()
			e.waitFor("vram", 5*time.Second, func() bool { return e.s.WaitingFor(e.lj(l.ID, model.StageASR)) == model.WaitVRAM })
			time.Sleep(60 * time.Millisecond)
			if j := e.lj(l.ID, model.StageASR); j.Status != model.StatusQueued || e.s.WaitingFor(j) != model.WaitVRAM {
				t.Fatalf("asr %s / %q", j.Status, e.s.WaitingFor(j))
			}
			if est := e.s.GPUStatus(e.ctx).EstimatedJobMB; est != c.est {
				t.Errorf("estimate %d, want %d", est, c.est)
			}
			if _, _, runs := g.stats(); runs != 0 {
				t.Fatal("asr started")
			}
		})
	}
}

// vram_per_job_mb at its default (5000 MB, every installation) gives way
// to the table; any other value is the user's and is kept, below or above
// the table (R37 S2). A model or compute type the table does not know
// keeps the setting. A calibrated peak only raises the estimate.
func TestVRAMEstimateSettingAndCalibration(t *testing.T) {
	e := newEnv(t, nil)
	set := func(perJob int) {
		a := e.s.set.ASR()
		a.VRAMPerJobMB = perJob
		if err := e.s.set.SetASR(e.ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		perJob      int
		mdl, ct     string
		obs         int // vram_observed (0: none)
		est, legacy int
	}{
		{5000, "large-v3", "float16", 0, 5000, 5000},
		{5000, "large-v3", "int8_float16", 0, 3100, 5000},
		{5000, "large-v2", "float16", 0, 4900, 5000},
		{5000, "large-v3-turbo", "int8_float16", 0, 1600, 5000},
		{5000, "medium", "float16", 0, 2600, 5000},
		{1500, "large-v3-turbo", "int8_float16", 0, 1500, 1500}, // the lower setting
		{2000, "large-v3", "float16", 0, 2000, 2000},
		{7000, "large-v3", "float16", 0, 7000, 7000}, // R37 S2: a value other than the default is kept
		{3000, "large-v3-turbo", "int8_float16", 0, 3000, 3000},
		{5000, "distil-whisper-custom", "float16", 0, 5000, 5000},
		{5000, "large-v3", "float32", 0, 5000, 5000},
		{4000, `D:\models\whisper`, "float16", 0, 4000, 4000},
		{5000, "large-v3-turbo", "float16", 2000, 2700, 5000}, // 2200 < 2700: no lower
		{5000, "large-v3-turbo", "float16", 3000, 3300, 5000}, // raised
		{5000, "large-v3", "float16", 6000, 6600, 6600},
	} {
		set(c.perJob)
		key := vramKey(c.mdl, c.ct)
		e.s.mu.Lock()
		delete(e.s.vramObs, key)
		e.s.mu.Unlock()
		if c.obs > 0 {
			if err := e.st.SetKV(e.ctx, key, strconv.Itoa(c.obs)); err != nil {
				t.Fatal(err)
			}
		}
		est, legacy := e.s.vramEstimates(e.ctx, c.mdl, c.ct)
		if est != c.est || legacy != c.legacy {
			t.Errorf("%+v: estimate %d, previous formula %d", c, est, legacy)
		}
	}
	// The GPU card shows the setting below the table too.
	set(1500)
	a := e.s.set.ASR()
	a.Model = "large-v3-turbo"
	if err := e.s.set.SetASR(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	e.gpu.set(4096-otherMB, 4096, otherMB)
	if est := e.s.GPUStatus(e.ctx).EstimatedJobMB; est != 1500 {
		t.Errorf("GPU card estimate %d, want the setting 1500", est)
	}
}

// vramParallel queues jobs lectures on cuda:0 of a totalMB card (otherMB
// in use by other programs, the default 1000 MB reserve) running mdl at
// precision with maxParallel slots, and returns how many start; the others
// must wait for wait.
func vramParallel(t *testing.T, totalMB int, mdl, precision string, maxParallel, jobs int, wait string) int {
	t.Helper()
	cuda := devRow(model.DeviceCUDA0, 300, maxParallel)
	cuda.Model, cuda.Precision = mdl, precision
	de := newDevEnv(t, []model.ASRDevice{cuda}, nil)
	de.gpu.set(totalMB-otherMB, totalMB, otherMB)
	ls := make([]model.Lecture, jobs)
	for i := range ls {
		ls[i] = de.lectureOn(autoOn(cuda))
	}
	de.prepDone(ls...)
	de.start()
	count := func() (running int, settled bool) {
		settled = true
		for _, l := range ls {
			j := de.lj(l.ID, model.StageASR)
			switch {
			case j.Status == model.StatusRunning:
				running++
			case j.Status != model.StatusQueued || de.s.WaitingFor(j) == "":
				settled = false
			}
		}
		return running, settled
	}
	var n int
	de.waitFor("settled", 5*time.Second, func() bool { var ok bool; n, ok = count(); return ok })
	time.Sleep(60 * time.Millisecond) // several scans: nothing more starts
	if n2, ok := count(); !ok || n2 != n {
		t.Fatalf("not settled: %d then %d running", n, n2)
	}
	for _, l := range ls {
		if j := de.lj(l.ID, model.StageASR); j.Status == model.StatusQueued && de.s.WaitingFor(j) != wait {
			t.Errorf("queued asr waits for %q, want %q", de.s.WaitingFor(j), wait)
		}
	}
	for _, l := range ls {
		de.asr.release(l.ID)
	}
	return n
}

// The jobs a card starts by model and precision (T-PERF-02 report table):
// with max_parallel 1 (the default) one job wherever the model's estimate
// fits beside the reserve; with more slots never more than before
// T-PERF-02 (a second job needs what the 5000 MB estimate needed), so the
// smaller estimates only let a first job start. A job only the formula
// before T-PERF-02 keeps out waits for a slot (gpu_slot, R37 S1), one the
// new estimate keeps out for VRAM.
func TestVRAMParallelByCard(t *testing.T) {
	const vram, slot = model.WaitVRAM, model.WaitGPUSlot
	type c struct {
		total     int
		mdl, prec string
		one, four int    // jobs started with max_parallel 1 and 4
		fourWait  string // what the others wait for with max_parallel 4
	}
	cases := []c{
		// 4 GB: 4096 - 700 - 1000 = 2396 MB for tasks
		{4096, "large-v3", "float16", 0, 0, vram},
		{4096, "large-v3", "auto", 0, 0, vram}, // int8_float16 3100
		{4096, "large-v2", "auto", 0, 0, vram},
		{4096, "large-v3-turbo", "float16", 0, 0, vram}, // 2700
		{4096, "large-v3-turbo", "auto", 1, 1, vram},    // 1600: 796 left
		{4096, "medium", "float16", 0, 0, vram},         // 2600
		{4096, "medium", "auto", 1, 1, vram},            // 1800
		// 6 GB: 4444 MB
		{6144, "large-v3", "float16", 0, 0, vram},
		{6144, "large-v3", "auto", 1, 1, vram},
		{6144, "large-v3-turbo", "float16", 1, 1, vram},
		{6144, "large-v3-turbo", "auto", 1, 1, slot}, // 2 by the estimate
		{6144, "medium", "auto", 1, 1, slot},         // 2 by the estimate
		// 8 GB: 6492 MB (before: one job of 5000 MB)
		{8192, "large-v3", "float16", 1, 1, vram},
		{8192, "large-v3", "auto", 1, 1, slot},       // 2 by the estimate
		{8192, "large-v3-turbo", "auto", 1, 1, slot}, // 4 by the estimate
		{8192, "medium", "auto", 1, 1, slot},
		// 12 GB: 10527 MB (before: two jobs of 5000 MB)
		{12227, "large-v3", "auto", 1, 2, vram}, // float16 5000
		{12227, "large-v2", "auto", 1, 2, vram},
		{12227, "large-v3", "int8_float16", 1, 2, slot},       // 3 by the estimate
		{12227, "large-v3-turbo", "auto", 1, 2, slot},         // float16 2700: 3 by the estimate
		{12227, "large-v3-turbo", "int8_float16", 1, 2, slot}, // 6 by the estimate
		{12227, "medium", "auto", 1, 2, slot},
	}
	for _, x := range cases {
		t.Run(fmt.Sprintf("%d_%s_%s", x.total, x.mdl, x.prec), func(t *testing.T) {
			oneWait := vram
			if x.one == 1 {
				oneWait = slot // max_parallel 1
			}
			if n := vramParallel(t, x.total, x.mdl, x.prec, 1, 2, oneWait); n != x.one {
				t.Errorf("max_parallel 1: %d jobs started, want %d", n, x.one)
			}
			if n := vramParallel(t, x.total, x.mdl, x.prec, 4, 4, x.fourWait); n != x.four {
				t.Errorf("max_parallel 4: %d jobs started, want %d", n, x.four)
			}
		})
	}
}

// The legacy single-pool path keeps "no more parallel jobs than before"
// too: a 12 GB card with large-v3-turbo int8_float16 (1600 MB) and
// max_parallel 4 runs two jobs, as with the 5000 MB estimate; the third
// waits for a slot (R37 S1: 1600 MB would fit).
func TestVRAMLegacyPoolParallel(t *testing.T) {
	g := newGate()
	e := newEnv(t, func(c *config.Config) {
		c.ASR.Model, c.ASR.ComputeType, c.ASR.MaxParallel = "large-v3-turbo", "int8_float16", 4
	}, gpuStages(g)...)
	e.gpu.set(12227-otherMB, 12227, otherMB)
	ls := []model.Lecture{e.lecture(), e.lecture(), e.lecture()}
	e.prepDone(ls...)
	e.start()
	e.waitFor("two running, one waiting for a slot", 5*time.Second, func() bool {
		c, _, _ := g.stats()
		return c == 2 && e.s.WaitingFor(e.lj(ls[2].ID, model.StageASR)) == model.WaitGPUSlot
	})
	time.Sleep(60 * time.Millisecond)
	if c, m, _ := g.stats(); c != 2 || m != 2 {
		t.Fatalf("running %d (max %d), want 2", c, m)
	}
	close(g.release)
	for _, l := range ls {
		e.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
}
