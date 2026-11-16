// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// vramGPUStage is a GPU-pool asr stage that runs until it is cancelled.
type vramGPUStage struct{}

func (vramGPUStage) Name() string                 { return model.StageASR }
func (vramGPUStage) Resource() sched.ResourceKind { return sched.ResourceGPU }
func (vramGPUStage) InputHash(context.Context, string) (string, error) {
	return "", nil
}
func (vramGPUStage) Run(ctx context.Context, _ string, progress func(float64)) error {
	progress(0.1)
	<-ctx.Done()
	return ctx.Err()
}

// vramOutcome launches a lecture whose transcription runs mdl at precision
// on a totalMB card where other programs take otherMB, with the real
// scheduler (default reserve 1000 MB, one slot), and returns whether its
// asr job starts, and the code of the diagnostics' extra VRAM sentence for
// cuda:0 with that model (vramHints) at that moment ("" = none) with the
// hint's suggestion.
func vramOutcome(t *testing.T, totalMB, otherMB int, mdl, precision string) (starts bool, extra string, sw map[string]any) {
	t.Helper()
	prep := &fakeStage{name: model.StagePrep}
	e := newEnvWith(t, envConfig{start: true, sched: func(o *sched.Options) {
		fastScan(prep, vramGPUStage{})(o)
		o.GPUQuery = func(context.Context) (sched.GPUMem, error) {
			return sched.GPUMem{FreeMB: totalMB - otherMB, TotalMB: totalMB, UsedMB: otherMB}, nil
		}
	}})
	a := e.set.ASR()
	a.Model, a.ComputeType = mdl, precision
	if err := e.set.SetASR(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	c := e.course("C")
	l := e.draft(c.ID, "Week 1")
	var lv Lecture
	e.call("POST", "/api/lectures/"+l.ID+"/launch", nil, http.StatusOK, &lv)
	deadline := time.Now().Add(5 * time.Second)
	waits := 0 // consecutive readings waiting for VRAM
	for !starts && waits < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("asr neither started nor waited for vram: %+v", stageOf(lv.Stages, model.StageASR))
		}
		time.Sleep(25 * time.Millisecond)
		e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
		st := stageOf(lv.Stages, model.StageASR)
		switch {
		case st.Status == model.StatusRunning:
			starts = true
		case st.Status == model.StatusQueued && st.WaitingFor != nil && *st.WaitingFor == model.WaitVRAM:
			waits++
		default:
			waits = 0
		}
	}
	ctx := context.Background()
	g := e.sc.GPUStatus(ctx)
	devs := []ASRDeviceView{{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Enabled: true, Model: mdl, Precision: precision}}
	e.srv.vramHints(ctx, devs, GPUInfo{TotalMB: g.TotalMB, FreeMB: g.FreeMB, Paused: g.Paused, SlotsUsed: g.SlotsUsed, SlotsMax: g.SlotsMax, EstimatedJobMB: g.EstimatedJobMB})
	if h := devs[0].VRAMHint; h != nil {
		if x, _ := h.Params["extra"].([]model.ErrorInfo); len(x) == 1 {
			extra, sw = x[0].Code, x[0].Params
		}
	}
	return starts, extra, sw
}

// R37 M1: for the same card, other programs' use, model and precision the
// diagnostics' VRAM sentence appears exactly when the scheduler keeps the
// transcription waiting for VRAM, and a model it suggests is started.
func TestVRAMHintMatchesAdmission(t *testing.T) {
	for _, c := range []struct {
		name           string
		total, other   int
		mdl, precision string
		extra          string // "" = starts
	}{
		{"4 GB medium float16", 4096, 700, "medium", "float16", hwdetect.HintVRAMQueue},
		{"4 GB turbo float16", 4096, 700, "large-v3-turbo", "float16", hwdetect.HintVRAMQueue},
		{"4 GB turbo auto", 4096, 700, "large-v3-turbo", "auto", ""},
		{"4 GB large-v3 float16", 4096, 700, "large-v3", "float16", hwdetect.HintVRAMQueueSwitch},
		{"3 GB large-v3 float16", 3072, 700, "large-v3", "float16", hwdetect.HintVRAMQueueEstimate},
		{"3 GB large-v3 float16, nothing else on the card", 3072, 0, "large-v3", "float16", hwdetect.HintVRAMQueueSwitch},
		{"6 GB large-v3 auto, 2000 MB in use", 6144, 2000, "large-v3", "auto", ""},
		{"6 GB large-v3 auto, 2100 MB in use", 6144, 2100, "large-v3", "auto", hwdetect.HintVRAMQueue},
		{"12 GB large-v3 auto", 12227, 700, "large-v3", "auto", ""},
		// R37b B2: read, and nothing free: judged by 0, not by the total
		{"4 GB turbo auto, all taken", 4096, 4096, "large-v3-turbo", "auto", hwdetect.HintVRAMQueueEstimate},
	} {
		t.Run(c.name, func(t *testing.T) {
			starts, extra, sw := vramOutcome(t, c.total, c.other, c.mdl, c.precision)
			if starts != (extra == "") {
				t.Fatalf("scheduler starts=%v but the hint's sentence is %q", starts, extra)
			}
			if extra != c.extra {
				t.Fatalf("sentence %q, want %q", extra, c.extra)
			}
			if extra != hwdetect.HintVRAMQueueSwitch {
				return
			}
			mdl, _ := sw["suggest"].(string)
			prec, _ := sw["suggest_precision"].(string)
			if ok, x, _ := vramOutcome(t, c.total, c.other, mdl, prec); !ok || x != "" {
				t.Fatalf("the suggestion %s %s: starts=%v, sentence %q", mdl, prec, ok, x)
			}
		})
	}
}
