// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/devices"
)

// fakeNVIDIA is a switchable Deps.HasNVIDIA that counts its calls. Like the
// real one (a kv read), it fails on a cancelled context; err makes it fail
// anyway.
type fakeNVIDIA struct {
	mu    sync.Mutex
	has   bool
	err   error
	asked int
}

func (f *fakeNVIDIA) Has(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if f.err != nil {
		return false, f.err
	}
	return f.has, nil
}

func (f *fakeNVIDIA) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeNVIDIA) set(has bool) {
	f.mu.Lock()
	f.has = has
	f.mu.Unlock()
}

func (f *fakeNVIDIA) Asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

// snapshots takes n snapshots, each one interval apart (each one samples).
func snapshots(s *Service, clk *fakeClock, n int) Snapshot {
	var snap Snapshot
	for i := 0; i < n; i++ {
		clk.Advance(DefaultInterval + time.Millisecond)
		snap = s.Snapshot(context.Background())
	}
	return snap
}

// T046: without an NVIDIA GPU sampling never starts nvidia-smi, and the
// GPU card is unavailable without an error.
func TestNoNVIDIANeverRunsNvidiaSMI(t *testing.T) {
	clk := newClock()
	smi := &fakeSMI{}
	nv := &fakeNVIDIA{has: false}
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now, HasNVIDIA: nv.Has})
	snap := snapshots(s, clk, 20)
	if smi.Calls() != 0 {
		t.Fatalf("nvidia-smi ran %d times without an NVIDIA GPU", smi.Calls())
	}
	if g := snap.GPU; g.Available || g.Error != "" || g.UtilPct != nil || len(g.UtilHistory) != 0 {
		t.Fatalf("gpu %+v", g)
	}
	if snap.SampledAt == "" {
		t.Fatal("a skipped GPU sample still counts as a sample")
	}
	// 20 samples within 60 s: asked once, not per sample.
	if nv.Asked() != 1 {
		t.Fatalf("HasNVIDIA asked %d times", nv.Asked())
	}
}

// With an NVIDIA GPU every sample runs nvidia-smi as before.
func TestNVIDIARunsNvidiaSMIEverySample(t *testing.T) {
	clk := newClock()
	smi := &fakeSMI{}
	nv := &fakeNVIDIA{has: true}
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now, HasNVIDIA: nv.Has})
	snap := snapshots(s, clk, 10)
	if smi.Calls() != 10 {
		t.Fatalf("nvidia-smi calls %d, want 10", smi.Calls())
	}
	if !snap.GPU.Available || snap.GPU.Name != "NVIDIA Test GPU" {
		t.Fatalf("gpu %+v", snap.GPU)
	}
	if nv.Asked() != 1 {
		t.Fatalf("HasNVIDIA asked %d times", nv.Asked())
	}
}

// The answer is asked again after NVIDIARecheck: a driver that becomes
// ready later is picked up, and a GPU that disappears stops the runs.
func TestNVIDIARecheckAfterInterval(t *testing.T) {
	clk := newClock()
	smi := &fakeSMI{}
	nv := &fakeNVIDIA{has: false}
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now, HasNVIDIA: nv.Has})
	snapshots(s, clk, 3)
	nv.set(true)
	snapshots(s, clk, 3) // still the cached "no"
	if smi.Calls() != 0 {
		t.Fatalf("calls %d before the recheck", smi.Calls())
	}
	clk.Advance(NVIDIARecheck)
	snapshots(s, clk, 2)
	if smi.Calls() != 2 || nv.Asked() != 2 {
		t.Fatalf("after the recheck: calls %d, asked %d", smi.Calls(), nv.Asked())
	}
	nv.set(false)
	clk.Advance(NVIDIARecheck)
	snap := snapshots(s, clk, 3)
	if smi.Calls() != 2 {
		t.Fatalf("calls %d after the GPU went away", smi.Calls())
	}
	if snap.GPU.Available || snap.GPU.Error != "" || len(snap.GPU.UtilHistory) != 0 {
		t.Fatalf("gpu %+v", snap.GPU)
	}
}

// A new device detection asks again at once, without waiting for
// NVIDIARecheck.
func TestNVIDIARecheckOnNewDetection(t *testing.T) {
	clk := newClock()
	smi := &fakeSMI{}
	nv := &fakeNVIDIA{has: false}
	var mu sync.Mutex
	det := devices.Detection{} // before the first detection
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now, HasNVIDIA: nv.Has,
		Detection: func() devices.Detection { mu.Lock(); defer mu.Unlock(); return det }})
	snapshots(s, clk, 3)
	if smi.Calls() != 0 || nv.Asked() != 1 {
		t.Fatalf("calls %d, asked %d", smi.Calls(), nv.Asked())
	}
	nv.set(true)
	mu.Lock()
	det = devices.Detection{At: clk.Now(), CUDAName: "NVIDIA Test GPU"}
	mu.Unlock()
	snapshots(s, clk, 2)
	if smi.Calls() != 2 || nv.Asked() != 2 {
		t.Fatalf("after the detection: calls %d, asked %d", smi.Calls(), nv.Asked())
	}
}

// r4 D10: GET /api/resources samples with the request's context. A request
// cancelled before the sample (the page went away) must not make the
// NVIDIA check fail and the failure be kept as "no NVIDIA" for
// NVIDIARecheck: the check gets a context of its own.
func TestNVIDIACheckNotCancelledWithTheRequest(t *testing.T) {
	clk := newClock()
	smi := &fakeSMI{}
	nv := &fakeNVIDIA{has: true}
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now, HasNVIDIA: nv.Has})
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	clk.Advance(DefaultInterval + time.Millisecond)
	s.Snapshot(gone)
	if nv.Asked() != 1 {
		t.Fatalf("HasNVIDIA asked %d times", nv.Asked())
	}
	snap := snapshots(s, clk, 3)
	if smi.Calls() == 0 || !snap.GPU.Available {
		t.Fatalf("a cancelled request was kept as no NVIDIA: calls %d, gpu %+v", smi.Calls(), snap.GPU)
	}
	if nv.Asked() != 1 {
		t.Fatalf("the answer was not kept: asked %d times", nv.Asked())
	}
}

// r4 D10: a failed check is not an answer. Nothing known yet: the sample
// skips nvidia-smi (no error shown on a computer without a GPU, review R57
// D-1) and keeps nothing; a known answer stands; either way the next sample
// asks again.
func TestNVIDIACheckFailureNotCached(t *testing.T) {
	clk := newClock()
	smi := &fakeSMI{}
	nv := &fakeNVIDIA{has: true}
	nv.fail(errors.New("database is locked"))
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now, HasNVIDIA: nv.Has})
	snap := snapshots(s, clk, 2)
	if nv.Asked() != 2 || smi.Calls() != 0 {
		t.Fatalf("failing with nothing known: asked %d, nvidia-smi calls %d (want 2, 0)", nv.Asked(), smi.Calls())
	}
	if g := snap.GPU; g.Available || g.Error != "" {
		t.Fatalf("failing with nothing known: gpu %+v (want empty, no error)", g)
	}
	nv.fail(nil) // the read works again: the computer has a GPU
	snapshots(s, clk, 2)
	if nv.Asked() != 3 || smi.Calls() != 2 {
		t.Fatalf("after recovery: asked %d, calls %d (want 3, 2)", nv.Asked(), smi.Calls())
	}
	// A recheck that fails keeps the known answer and asks again at the next sample.
	nv.set(false)
	nv.fail(errors.New("database is locked"))
	clk.Advance(NVIDIARecheck)
	snapshots(s, clk, 2)
	if nv.Asked() != 5 || smi.Calls() != 4 {
		t.Fatalf("failed recheck: asked %d, calls %d (want 5, 4)", nv.Asked(), smi.Calls())
	}
	nv.fail(nil)
	snapshots(s, clk, 2)
	if nv.Asked() != 6 || smi.Calls() != 4 {
		t.Fatalf("recheck after the failure: asked %d, calls %d (want 6, 4)", nv.Asked(), smi.Calls())
	}
}
