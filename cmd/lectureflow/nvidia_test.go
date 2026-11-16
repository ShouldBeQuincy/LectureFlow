// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/resources"
)

// T046: the resource service's NVIDIA answer follows the diagnostics'
// rule, except that nothing known yet still samples.
func TestHasNVIDIAFunc(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		kv   string // kv hardware.last; "" = not set
		det  devices.Detection
		want bool
	}{
		{"nothing known yet", "", devices.Detection{}, true},
		{"detection named a GPU", "", devices.Detection{At: at, CUDAName: "NVIDIA RTX"}, true},
		{"detection found none", "", devices.Detection{At: at, CUDAErr: "not found"}, false},
		{"remembered none, not detected yet", `{"nvidia":[],"detected_at":"2026-11-01T00:00:00Z"}`, devices.Detection{}, false},
		{"remembered none, detection found none", `{"nvidia":[]}`, devices.Detection{At: at}, false},
		{"remembered a GPU, detection failed", `{"nvidia":["NVIDIA RTX"]}`, devices.Detection{At: at, CUDAErr: "driver"}, true},
		{"unreadable memory counts as nothing", `{`, devices.Detection{}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if c.kv != "" {
				if err := st.SetKV(ctx, api.KVHardwareLast, c.kv); err != nil {
					t.Fatal(err)
				}
			}
			want := c.want && runtime.GOOS != "darwin" // never on macOS
			f := hasNVIDIAFunc(st, func() devices.Detection { return c.det })
			if got, err := f(ctx); got != want || err != nil {
				t.Fatalf("got %v (err %v), want %v", got, err, want)
			}
		})
	}
}

// r4 D10: the resource bar asks with the request's context. A request that
// is gone must not make a computer whose remembered detection lists an
// NVIDIA GPU count as having none for resources.NVIDIARecheck; and a read
// that fails is an error, not "nothing remembered".
func TestNVIDIAAnswerSurvivesCancelledRequest(t *testing.T) {
	ctx := context.Background()
	if runtime.GOOS == "darwin" { // never an NVIDIA GPU, nothing read
		if got, err := hasNVIDIAFunc(nil, func() devices.Detection { return devices.Detection{} })(ctx); got || err != nil {
			t.Fatalf("macOS: got %v, err %v", got, err)
		}
		return
	}
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetKV(ctx, api.KVHardwareLast, `{"nvidia":["NVIDIA RTX"]}`); err != nil {
		t.Fatal(err)
	}
	// The detection ran but its nvidia-smi named nothing (a driver hiccup):
	// only the remembered detection says there is a GPU.
	det := devices.Detection{At: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), CUDAErr: "driver"}
	has := hasNVIDIAFunc(st, func() devices.Detection { return det })

	gone, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := has(gone); got || err == nil {
		t.Fatalf("a failed read: got %v, err %v (want an error)", got, err)
	}

	var mu sync.Mutex
	smiCalls := 0
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	res := resources.New(resources.Deps{
		HasNVIDIA: has,
		Detection: func() devices.Detection { return det },
		RunCmd: func(_ context.Context, name string, _ ...string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(name, "nvidia-smi") {
				smiCalls++
				return "NVIDIA RTX, 10, 1000, 16376, 50, 30.5, 100.00\n", nil
			}
			return "", errors.New("not in this test")
		},
		// every reading is one sampling interval later (bounded: a few per snapshot)
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(resources.DefaultInterval + time.Millisecond)
			return now
		},
	})
	res.Snapshot(gone) // the page went away before the sample
	snap := res.Snapshot(ctx)
	mu.Lock()
	calls := smiCalls
	mu.Unlock()
	if calls == 0 || !snap.GPU.Available {
		t.Fatalf("the cancelled request was kept as no NVIDIA: nvidia-smi calls %d, gpu %+v", calls, snap.GPU)
	}
}
