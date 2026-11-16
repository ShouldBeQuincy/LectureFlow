// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// R37 S5: on a card of compute capability below 7.0 CTranslate2 runs
// float16 as float32 and int8_float16 as int8_float32, so the T10 table
// (measured on a newer card) is not used: the estimate is the setting, as
// before T-PERF-02. Unknown (not reported, not read) keeps the table.
func TestVRAMEstimatePreVolta(t *testing.T) {
	for _, c := range []struct {
		cap  string
		err  error
		want int // 4 GB card, large-v3-turbo auto (int8_float16)
	}{
		{"6.1", nil, 5000}, // GTX 1050 Ti / 1080
		{"5.2", nil, 5000},
		{"7.5", nil, 1600}, // GTX 16xx / RTX 20xx
		{"12.0", nil, 1600},
		{"[N/A]", nil, 1600},
		{"", errors.New("nvidia-smi: exit status 2"), 1600},
	} {
		t.Run(c.cap, func(t *testing.T) {
			var calls atomic.Int32
			e := newEnvOpts(t, func(cf *config.Config) { cf.ASR.Model = "large-v3-turbo" }, func(o *Options) {
				o.GPUComputeCap = func(context.Context) (string, error) {
					calls.Add(1)
					return c.cap, c.err
				}
			})
			e.gpu.set(4096-otherMB, 4096, otherMB)
			for i := 0; i < 3; i++ {
				if est := e.s.GPUStatus(e.ctx).EstimatedJobMB; est != c.want {
					t.Fatalf("estimate %d, want %d", est, c.want)
				}
			}
			if n := calls.Load(); n != 1 { // read once (an unread one again only after capRetry)
				t.Errorf("compute capability read %d times", n)
			}
			if est := e.s.VRAMEstimate(e.ctx, "medium", "float16"); c.want == 5000 && est != 5000 || c.want != 5000 && est != 2600 {
				t.Errorf("medium float16 estimate %d", est)
			}
		})
	}
	// A pre-Volta card waits on 4 GB as before; a newer one starts.
	for _, c := range []struct {
		cap    string
		starts bool
	}{{"6.1", false}, {"8.6", true}} {
		t.Run("admission "+c.cap, func(t *testing.T) {
			g := newGate()
			e := newEnvOpts(t, func(cf *config.Config) { cf.ASR.Model = "large-v3-turbo" }, func(o *Options) {
				o.GPUComputeCap = func(context.Context) (string, error) { return c.cap, nil }
			}, gpuStages(g)...)
			e.gpu.set(4096-otherMB, 4096, otherMB)
			l := e.lecture()
			e.start()
			if c.starts {
				e.waitStatus(l.ID, model.StageASR, model.StatusRunning)
			} else {
				e.stillWaits(l, model.WaitVRAM)
			}
			close(g.release)
		})
	}
	if _, ok := computeCapMajor("[N/A]"); ok {
		t.Error("[N/A] parsed")
	}
	if m, ok := computeCapMajor(" 6.1 "); !ok || m != 6 {
		t.Errorf("6.1: %d %v", m, ok)
	}
}

// R37 M1: the VRAM the diagnostics judge by is what the scheduler admits a
// first job by: the free memory with nothing running; with the
// application's tasks running, free + used - what was in use when the
// earliest started (their own memory is not "other programs").
func TestVRAMAvailMB(t *testing.T) {
	g := newGate()
	e := newEnv(t, func(c *config.Config) { c.ASR.MaxParallel = 2 }, gpuStages(g)...)
	e.gpu.set(12227-otherMB, 12227, otherMB)
	if _, ok := e.s.VRAMAvailMB(); ok {
		t.Error("available VRAM before any reading")
	}
	e.s.GPUStatus(e.ctx)
	if mb, ok := e.s.VRAMAvailMB(); !ok || mb != 12227-otherMB {
		t.Errorf("idle: %d %v", mb, ok)
	}
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageASR, model.StatusRunning)
	e.gpu.set(12227-otherMB-4400, 12227, otherMB+4400) // the task took 4400 MB
	if err := e.waitSampledPeak(l.ID, 4400); err != nil {
		t.Fatal(err)
	}
	if mb, ok := e.s.VRAMAvailMB(); !ok || mb != 12227-otherMB {
		t.Errorf("running: %d %v, want %d", mb, ok, 12227-otherMB)
	}
	close(g.release)
}
