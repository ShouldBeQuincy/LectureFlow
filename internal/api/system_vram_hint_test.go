// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	"lectureflow/internal/devices"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// r2 N15: GET /api/system tells about cuda:0 whose model does not suit the
// card's VRAM (or whose model files are missing while the card's
// recommendation is installed), adding that transcription will keep
// waiting when the scheduler's per-task estimate plus the reserve exceed
// the card. The VRAM is the scheduler's nvidia-smi reading (faked here);
// nothing changes the device.

// fwLocator has the faster-whisper environment and the faster-whisper
// models named in models (any precision).
type fwLocator struct {
	mu     sync.Mutex
	models []string
	// R67 (M6), devices.ComponentTasks: the environment is not installed
	// (noPython), an unfinished task installs it (pyInstalling) or the
	// faster-whisper models named in installing.
	noPython, pyInstalling bool
	installing             []string
}

func (l *fwLocator) Python(backend string) (string, bool) {
	if backend == model.BackendFasterWhisper && !l.noPython {
		return compFWPython, true
	}
	return "", false
}

func (l *fwLocator) PythonInstalling(backend string) bool {
	return backend == model.BackendFasterWhisper && l.pyInstalling
}

func (l *fwLocator) ModelInstalling(backend, name, precision string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return backend == model.BackendFasterWhisper && slices.Contains(l.installing, name)
}

func (l *fwLocator) ModelDir(backend, name, precision string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if backend != model.BackendFasterWhisper || !slices.Contains(l.models, name) {
		return "", false
	}
	return "C:/data/components/model-" + name, true
}

// vramOtherMB is what other programs take on the card in these tests.
const vramOtherMB = 700

// vramEnv is the reference laptop's device table with nvidia-smi reporting
// totalMB (0: nvidia-smi failed) and cuda:0 running mdl.
func vramEnv(t *testing.T, totalMB int, mdl string, server func(*Server)) *devEnv {
	return vramEnvAt(t, totalMB, mdl, model.PrecisionAuto, server)
}

// vramEnvAt is vramEnv with cuda:0 at precision. The scheduler of this env
// has no device source (sched.GPUStatus prices the legacy ASR settings):
// they are set to the same model and precision, as the device source
// gives the scheduler in the application. Other programs take
// vramOtherMB of the card (T10's desktop baseline), nothing of the
// application runs there.
func vramEnvAt(t *testing.T, totalMB int, mdl, precision string, server func(*Server)) *devEnv {
	t.Helper()
	gpu := func(o *sched.Options) {
		o.GPUQuery = func(context.Context) (sched.GPUMem, error) {
			if totalMB <= 0 {
				return sched.GPUMem{}, errors.New("nvidia-smi: no answer")
			}
			return sched.GPUMem{FreeMB: totalMB - vramOtherMB, TotalMB: totalMB, UsedMB: vramOtherMB}, nil
		}
	}
	de := newDevEnv(t, envConfig{sched: gpu, server: server})
	de.reg.mu.Lock()
	for i := range de.reg.rows {
		if de.reg.rows[i].ID == model.DeviceCUDA0 {
			de.reg.rows[i].Model, de.reg.rows[i].Precision = mdl, precision
		}
	}
	de.reg.mu.Unlock()
	a := de.srv.set.ASR()
	a.Model, a.ComputeType = mdl, precision
	if err := de.srv.set.SetASR(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return de
}

// vramHintsOf is GET /api/system's hint of each device (no checks run:
// the device table needs none) and its GPU card.
func vramHintsOf(t *testing.T, de *devEnv) (map[string]*model.ErrorInfo, GPUInfo) {
	t.Helper()
	sys := de.srv.systemView(context.Background())
	if len(sys.Devices) == 0 {
		t.Fatal("no devices")
	}
	out := map[string]*model.ErrorInfo{}
	for _, d := range sys.Devices {
		out[d.ID] = d.VRAMHint
	}
	return out, sys.GPU
}

func TestSystemVRAMHint(t *testing.T) {
	cases := []struct {
		name      string
		totalMB   int
		model     string
		precision string   // cuda:0's ("": the row's auto)
		locator   bool     // a component manager with the faster-whisper environment
		installed []string // its faster-whisper models
		code      string
		params    map[string]any // without extra
		extra     string         // the code of the extra sentence ("" = none)
		perJob    int            // the scheduler's estimate (GPU card)
		switchTo  map[string]any // the extra's suggestion params (vram_queue_switch)
	}{
		// default settings: vram_per_job_mb 5000, reserve 1000 MB; since
		// T-PERF-02 the estimate follows the model and precision (auto:
		// int8_float16 below 10 GB)
		{"4 GB card, large-v3", 4096, "large-v3", "", false, nil, hwdetect.HintVRAMLow,
			map[string]any{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}, hwdetect.HintVRAMQueue, 3100, nil},
		// float16 alone is more than the card: switching to the card's
		// recommendation (large-v3-turbo int8_float16, 1600 MB) fits
		{"4 GB card, large-v3 float16", 4096, "large-v3", model.PrecisionFloat16, false, nil, hwdetect.HintVRAMLow,
			map[string]any{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}, hwdetect.HintVRAMQueueSwitch, 5000,
			map[string]any{"suggest": "large-v3-turbo", "suggest_precision": "int8_float16", "suggest_mb": 1600}},
		// R27 A-6 no longer applies: after switching to the suggestion the
		// job fits (1600 + 1000 <= 3396 available)
		{"4 GB card, large-v3-turbo", 4096, "large-v3-turbo", "", false, nil, "", nil, "", 1600, nil},
		// R37 M1: 2600 + 1000 is within the card but not within the 3396 MB
		// other programs leave (the scheduler does not start it)
		{"4 GB card, medium float16", 4096, "medium", model.PrecisionFloat16, false, nil, hwdetect.HintVRAMQueueOnly,
			map[string]any{"vram_gb": 4}, hwdetect.HintVRAMQueue, 2600, nil},
		{"4 GB card, large-v3-turbo float16", 4096, "large-v3-turbo", model.PrecisionFloat16, false, nil, hwdetect.HintVRAMQueueOnly,
			map[string]any{"vram_gb": 4}, hwdetect.HintVRAMQueue, 2700, nil},
		// R37 M1: the recommendation (medium int8_float16, 1800 MB) fits
		// the 3 GB card with the reserve (2800 <= 3072) but not the 2372 MB
		// available: no switch suggested
		{"3 GB card, large-v3 float16", 3072, "large-v3", model.PrecisionFloat16, false, nil, hwdetect.HintVRAMLow,
			map[string]any{"vram_gb": 3, "model": "large-v3", "need_gb": 6, "suggest": "medium"}, hwdetect.HintVRAMQueueEstimate, 5000, nil},
		{"5800 MB card, large-v2", 5800, "large-v2", "", false, nil, hwdetect.HintVRAMLow,
			map[string]any{"vram_gb": 5, "model": "large-v2", "need_gb": 6, "suggest": "large-v3-turbo"}, "", 3100, nil},
		{"5800 MB card, large-v3-turbo", 5800, "large-v3-turbo", "", false, nil, "", nil, "", 1600, nil},
		// a model the table does not know keeps vram_per_job_mb (5000)
		{"5800 MB card, unknown model", 5800, "whisper-custom", "", false, nil, hwdetect.HintVRAMQueueOnly,
			map[string]any{"vram_gb": 6}, hwdetect.HintVRAMQueue, 5000, nil},
		// nothing fits beside the reserve, not even the recommendation
		// (medium int8_float16 1800 + 1000 > 2048): another device
		{"2 GB card, medium float16", 2048, "medium", model.PrecisionFloat16, false, nil, hwdetect.HintVRAMQueueOnly,
			map[string]any{"vram_gb": 2}, hwdetect.HintVRAMQueueEstimate, 2600, nil},
		{"6 GB card, large-v3", 6144, "large-v3", "", false, nil, "", nil, "", 3100, nil},
		{"12 GB card, large-v3", 12000, "large-v3", "", false, nil, "", nil, "", 5000, nil},
		{"12 GB card, medium missing, large-v3 installed", 12000, "medium", "", true, []string{"large-v3"}, hwdetect.HintVRAMModelMissing,
			map[string]any{"vram_gb": 12, "model": "medium", "suggest": "large-v3"}, "", 2600, nil},
		{"12 GB card, large-v2 missing, large-v3 not installed", 12000, "large-v2", "", true, nil, "", nil, "", 4900, nil},
		{"12 GB card, large-v3 missing", 12000, "large-v3", "", true, nil, "", nil, "", 5000, nil},
		// r6 (T043): the files of a model the card is too small for are
		// missing (the scheduler's model_missing wait): the hint says so and
		// names the card's recommendation, installed or still to install.
		// R67: without the sentence about lowering the reserve (3100 + 1000 >
		// 3396 as in the first case): the wait is for the model's files
		{"4 GB card, large-v3 missing", 4096, "large-v3", "", true, nil, HintVRAMLowMissingInstall,
			map[string]any{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}, "", 3100, nil},
		{"4 GB card, large-v3 missing, medium installed", 4096, "large-v3", "", true, []string{"medium"}, HintVRAMLowMissingInstall,
			map[string]any{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}, "", 3100, nil},
		{"4 GB card, large-v3 missing, large-v3-turbo installed", 4096, "large-v3", "", true, []string{"large-v3-turbo"}, HintVRAMLowMissing,
			map[string]any{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}, "", 3100, nil},
		{"5800 MB card, large-v2 missing, large-v3-turbo installed", 5800, "large-v2", "", true, []string{"large-v3-turbo"}, HintVRAMLowMissing,
			map[string]any{"vram_gb": 5, "model": "large-v2", "need_gb": 6, "suggest": "large-v3-turbo"}, "", 3100, nil},
		// the other two sentences stay: the recommendation fits (float16
		// alone is more than the card) / does not fit either (3 GB card)
		{"4 GB card, large-v3 float16 missing, large-v3-turbo installed", 4096, "large-v3", model.PrecisionFloat16, true, []string{"large-v3-turbo"}, HintVRAMLowMissing,
			map[string]any{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}, hwdetect.HintVRAMQueueSwitch, 5000,
			map[string]any{"suggest": "large-v3-turbo", "suggest_precision": "int8_float16", "suggest_mb": 1600}},
		{"3 GB card, large-v3 float16 missing", 3072, "large-v3", model.PrecisionFloat16, true, nil, HintVRAMLowMissingInstall,
			map[string]any{"vram_gb": 3, "model": "large-v3", "need_gb": 6, "suggest": "medium"}, hwdetect.HintVRAMQueueEstimate, 5000, nil},
		// installed: the hint about the VRAM alone, as before
		{"4 GB card, large-v3 installed", 4096, "large-v3", "", true, []string{"large-v3"}, hwdetect.HintVRAMLow,
			map[string]any{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}, hwdetect.HintVRAMQueue, 3100, nil},
		// a missing model that suits the card: the waiting text and the
		// components say to install it, no hint here
		{"4 GB card, large-v3-turbo missing", 4096, "large-v3-turbo", "", true, nil, "", nil, "", 1600, nil},
		{"nvidia-smi failed", 0, "large-v3", "", true, []string{"large-v3-turbo"}, "", nil, "", -1, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var server func(*Server)
			if c.locator {
				loc := &fwLocator{models: c.installed}
				server = func(s *Server) { s.cfg.Components = loc }
			}
			prec := c.precision
			if prec == "" {
				prec = model.PrecisionAuto
			}
			hints, gpu := vramHintsOf(t, vramEnvAt(t, c.totalMB, c.model, prec, server))
			if c.perJob >= 0 && gpu.EstimatedJobMB != c.perJob {
				t.Errorf("GPU card estimate %d, want %d", gpu.EstimatedJobMB, c.perJob)
			}
			for id, h := range hints {
				if id != model.DeviceCUDA0 && h != nil { // Intel GPU / NPU / CPU: never
					t.Errorf("%s has a VRAM hint %+v", id, h)
				}
			}
			got := hints[model.DeviceCUDA0]
			if c.code == "" {
				if got != nil {
					t.Errorf("cuda:0 hint %+v, want none", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("cuda:0 no hint, want %s", c.code)
			}
			params := map[string]any{}
			for k, v := range got.Params {
				if k != "extra" {
					params[k] = v
				}
			}
			if got.Code != c.code || !reflect.DeepEqual(params, c.params) {
				t.Errorf("cuda:0 hint %+v, want %s %v", got, c.code, c.params)
			}
			extra, _ := got.Params["extra"].([]model.ErrorInfo)
			if c.extra == "" {
				if extra != nil {
					t.Errorf("cuda:0 extra %+v, want none", extra)
				}
				return
			}
			// the scheduler's estimate in effect, the default
			// vram_reserve_mb 1000 and the free memory (R37 M1)
			wantParams := map[string]any{"per_job_mb": c.perJob, "reserve_mb": 1000, "avail_mb": c.totalMB - vramOtherMB}
			for k, v := range c.switchTo {
				wantParams[k] = v
			}
			want := []model.ErrorInfo{{Code: c.extra, Params: wantParams}}
			if !reflect.DeepEqual(extra, want) {
				t.Errorf("cuda:0 extra %+v, want %+v", extra, want)
			}
		})
	}
}

// The sentences only when the per-task estimate plus the reserve is more
// than the card (the values in effect, never a constant): the estimate
// alone too large, or only with the reserve; alone (vram_queue_only) when
// there is no hint about the model. T-PERF-02: with the estimate alone too
// large, the card's recommendation is suggested when it fits with the
// reserve (vram_queue_switch), else another device (vram_queue_estimate).
// R37 M1: judged by the VRAM available now (avail; -1, unknown: the
// total), as the scheduler admits a first job.
func TestWithVRAMQueue(t *testing.T) {
	hint := func() *model.ErrorInfo { return &model.ErrorInfo{Code: hwdetect.HintVRAMLow, Params: map[string]any{}} }
	turbo := VRAMFit{Model: "large-v3-turbo", Precision: "int8_float16", MB: 1600}
	medium := VRAMFit{Model: "medium", Precision: "int8_float16", MB: 1800}
	for _, c := range []struct {
		total, avail, perJob, reserve int
		fit                           VRAMFit
		extra                         string
	}{
		{4096, -1, 5000, 1000, VRAMFit{}, hwdetect.HintVRAMQueueEstimate},
		{4096, -1, 5000, 0, VRAMFit{}, hwdetect.HintVRAMQueueEstimate},
		{4096, -1, 5000, 1000, turbo, hwdetect.HintVRAMQueueSwitch},
		{4096, -1, 5000, 2496, turbo, hwdetect.HintVRAMQueueSwitch}, // 1600 + 2496 = 4096
		{4096, -1, 5000, 2497, turbo, hwdetect.HintVRAMQueueEstimate},
		// a suggestion without its estimate is not made
		{4096, -1, 5000, 1000, VRAMFit{Model: "large-v3-turbo", Precision: "int8_float16"}, hwdetect.HintVRAMQueueEstimate},
		{4096, -1, 3000, 1000, turbo, ""}, // 4000 <= 4096
		{4096, -1, 3000, 1200, turbo, hwdetect.HintVRAMQueue},
		{4096, -1, 4096, 0, turbo, ""},
		{4096, -1, 4096, 1, turbo, hwdetect.HintVRAMQueue},
		{5800, -1, 5000, 0, turbo, ""},
		{5800, -1, 5000, 1000, turbo, hwdetect.HintVRAMQueue},
		{4096, -1, 0, 1000, turbo, ""}, // no estimate: no claim
		{0, -1, 5000, 1000, turbo, ""}, // no VRAM: no claim
		// by the available VRAM
		{4096, 3396, 2600, 1000, turbo, hwdetect.HintVRAMQueue}, // 3600 > 3396
		{4096, 3396, 2396, 1000, turbo, ""},                     // 3396 <= 3396
		{4096, 3396, 3397, 1000, turbo, hwdetect.HintVRAMQueueSwitch},
		{3072, 2372, 5000, 1000, medium, hwdetect.HintVRAMQueueEstimate}, // 2800 > 2372
		{3072, 2800, 5000, 1000, medium, hwdetect.HintVRAMQueueSwitch},
		{4096, 5000, 4000, 1000, turbo, hwdetect.HintVRAMQueue}, // above the total: the total
		// R37b B2: nothing free is nothing free, not "unknown"
		{4096, 0, 1600, 1000, turbo, hwdetect.HintVRAMQueueEstimate},
		{4096, 0, 1600, 0, turbo, hwdetect.HintVRAMQueueEstimate},
	} {
		for _, withHint := range []bool{true, false} {
			var h *model.ErrorInfo
			if withHint {
				h = hint()
			}
			got := WithVRAMQueue(h, c.total, c.avail, c.perJob, c.reserve, c.fit)
			if c.extra == "" {
				if withHint && (got == nil || got.Params["extra"] != nil) || !withHint && got != nil {
					t.Errorf("%+v hint %v: %+v, want no sentence", c, withHint, got)
				}
				continue
			}
			wantCode := hwdetect.HintVRAMLow
			if !withHint {
				wantCode = hwdetect.HintVRAMQueueOnly
			}
			if got == nil {
				t.Errorf("%+v hint %v: none, want %s + %s", c, withHint, wantCode, c.extra)
				continue
			}
			extra, _ := got.Params["extra"].([]model.ErrorInfo)
			if got.Code != wantCode || len(extra) != 1 || extra[0].Code != c.extra {
				t.Errorf("%+v hint %v: %+v, want %s + %s", c, withHint, got, wantCode, c.extra)
				continue
			}
			avail := c.avail
			if avail < 0 || avail > c.total {
				avail = c.total
			}
			wantParams := map[string]any{"per_job_mb": c.perJob, "reserve_mb": c.reserve, "avail_mb": avail}
			if c.extra == hwdetect.HintVRAMQueueSwitch {
				wantParams["suggest"], wantParams["suggest_precision"], wantParams["suggest_mb"] = c.fit.Model, c.fit.Precision, c.fit.MB
			}
			if !reflect.DeepEqual(extra[0].Params, wantParams) {
				t.Errorf("%+v hint %v: extra params %v, want %v", c, withHint, extra[0].Params, wantParams)
			}
			if !withHint && got.Params["vram_gb"] != hwdetect.VRAMGB(c.total) {
				t.Errorf("%+v: vram_gb %v", c, got.Params["vram_gb"])
			}
		}
	}
}

// R67: the hint of a model that is not installed on a card too small for it
// (HintVRAMLowMissing, HintVRAMLowMissingInstall) does not get the sentence
// about lowering the reserve (HintVRAMQueue): the estimate is the missing
// model's, and the wait is for its files. The two sentences about the
// card's recommendation stay; every other hint keeps all three.
func TestWithVRAMQueueMissingModel(t *testing.T) {
	turbo := VRAMFit{Model: "large-v3-turbo", Precision: "int8_float16", MB: 1600}
	medium := VRAMFit{Model: "medium", Precision: "int8_float16", MB: 1800}
	for _, c := range []struct {
		name                          string
		code                          string
		total, avail, perJob, reserve int
		fit                           VRAMFit
		extra                         string // "" = no sentence
	}{
		{"missing, reserve too large", HintVRAMLowMissing, 4096, 3396, 3100, 1000, turbo, ""},
		{"missing (to install), reserve too large", HintVRAMLowMissingInstall, 4096, 3396, 3100, 1000, turbo, ""},
		{"missing, the total as the available", HintVRAMLowMissing, 4096, -1, 3100, 1200, turbo, ""},
		{"missing, the recommendation fits", HintVRAMLowMissing, 4096, 3396, 5000, 1000, turbo, hwdetect.HintVRAMQueueSwitch},
		{"missing (to install), the recommendation fits", HintVRAMLowMissingInstall, 4096, 3396, 5000, 1000, turbo, hwdetect.HintVRAMQueueSwitch},
		{"missing, nothing fits", HintVRAMLowMissing, 3072, 2372, 5000, 1000, medium, hwdetect.HintVRAMQueueEstimate},
		{"missing (to install), nothing fits", HintVRAMLowMissingInstall, 3072, 2372, 5000, 1000, medium, hwdetect.HintVRAMQueueEstimate},
		{"missing, it all fits", HintVRAMLowMissing, 5800, 5100, 3100, 1000, turbo, ""},
		// the other hints as before
		{"installed, reserve too large", hwdetect.HintVRAMLow, 4096, 3396, 3100, 1000, turbo, hwdetect.HintVRAMQueue},
		{"a suiting model missing, reserve too large", hwdetect.HintVRAMModelMissing, 4096, 3396, 2600, 1000, turbo, hwdetect.HintVRAMQueue},
	} {
		params := map[string]any{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": c.fit.Model}
		got := WithVRAMQueue(&model.ErrorInfo{Code: c.code, Params: maps.Clone(params)}, c.total, c.avail, c.perJob, c.reserve, c.fit)
		if got == nil || got.Code != c.code {
			t.Errorf("%s: %+v, want the hint %s", c.name, got, c.code)
			continue
		}
		extra, _ := got.Params["extra"].([]model.ErrorInfo)
		if c.extra == "" {
			if !reflect.DeepEqual(got.Params, params) {
				t.Errorf("%s: params %v, want %v unchanged (no sentence)", c.name, got.Params, params)
			}
			continue
		}
		if len(extra) != 1 || extra[0].Code != c.extra {
			t.Errorf("%s: extra %+v, want %s", c.name, extra, c.extra)
		}
	}
	// no hint about the model: the sentence alone, as before
	if got := WithVRAMQueue(nil, 4096, 3396, 3100, 1000, turbo); got == nil || got.Code != hwdetect.HintVRAMQueueOnly {
		t.Errorf("no hint: %+v, want %s", got, hwdetect.HintVRAMQueueOnly)
	}
}

// r6 (T043): NVIDIAVRAMHint is hwdetect.NVIDIAModelHint, except that a
// model too large for the card whose files are missing gets its own code
// (the same parameters): by whether the card's recommendation is installed.
func TestNVIDIAVRAMHint(t *testing.T) {
	only := func(names ...string) func(string) bool {
		return func(name string) bool { return slices.Contains(names, name) }
	}
	for _, c := range []struct {
		name      string
		vramMB    int
		model     string
		missing   bool
		installed func(string) bool
		code      string // "" = none
		suggest   string
	}{
		{"too small, installed", 4096, "large-v3", false, only("large-v3"), hwdetect.HintVRAMLow, "large-v3-turbo"},
		{"too small, missing, no locator", 4096, "large-v3", true, nil, HintVRAMLowMissingInstall, "large-v3-turbo"},
		{"too small, missing, nothing installed", 4096, "large-v3", true, only(), HintVRAMLowMissingInstall, "large-v3-turbo"},
		{"too small, missing, another model installed", 4096, "large-v3", true, only("medium", "small"), HintVRAMLowMissingInstall, "large-v3-turbo"},
		{"too small, missing, recommendation installed", 4096, "large-v3", true, only("large-v3-turbo"), HintVRAMLowMissing, "large-v3-turbo"},
		{"5999 MB, large-v2 missing", 5999, "large-v2", true, only("large-v3-turbo"), HintVRAMLowMissing, "large-v3-turbo"},
		{"under 4000 MB, turbo missing", 3072, "large-v3-turbo", true, only("medium"), HintVRAMLowMissing, "medium"},
		{"under 4000 MB, turbo missing, medium not installed", 3072, "large-v3-turbo", true, only("large-v3-turbo"), HintVRAMLowMissingInstall, "medium"},
		// unchanged: a model that suits the card
		{"suits, missing, recommendation installed", 12000, "medium", true, only("large-v3"), hwdetect.HintVRAMModelMissing, "large-v3"},
		{"suits, missing, recommendation not installed", 12000, "large-v2", true, only(), "", ""},
		{"suits, missing, is the recommendation", 4096, "large-v3-turbo", true, only(), "", ""},
		{"6000 MB, large-v3 missing", 6000, "large-v3", true, only(), "", ""},
		{"unknown model, missing", 4096, "whisper-custom", true, only(), "", ""},
		// no claim without the card's size or a model
		{"VRAM unknown", 0, "large-v3", true, only("large-v3-turbo"), "", ""},
		{"no model", 4096, " ", true, only("large-v3-turbo"), "", ""},
	} {
		got := NVIDIAVRAMHint(c.vramMB, c.model, c.missing, c.installed)
		if c.code == "" {
			if got != nil {
				t.Errorf("%s: %+v, want none", c.name, got)
			}
			continue
		}
		if got == nil || got.Code != c.code || got.Params["suggest"] != c.suggest || got.Params["model"] != c.model {
			t.Errorf("%s: %+v, want %s suggesting %s", c.name, got, c.code, c.suggest)
			continue
		}
		// the parameters of hwdetect's hint for the same card and model
		base := hwdetect.NVIDIAModelHint(c.vramMB, c.model, c.missing, c.installed)
		if base == nil || !reflect.DeepEqual(got.Params, base.Params) {
			t.Errorf("%s: params %v, hwdetect's hint %+v", c.name, got.Params, base)
			continue
		}
		if c.code == HintVRAMLowMissing || c.code == HintVRAMLowMissingInstall {
			if base.Code != hwdetect.HintVRAMLow {
				t.Errorf("%s: hwdetect's hint is %s", c.name, base.Code)
			}
			for _, k := range []string{"vram_gb", "model", "need_gb", "suggest"} {
				if _, ok := got.Params[k]; !ok {
					t.Errorf("%s: no %s in %v", c.name, k, got.Params)
				}
			}
		}
	}
}

// R67 (M6): only a model whose files are missing and that no task installs
// (devices.WorkerNoModel, the scheduler's model_missing wait) gets the two
// hints. While a task installs the model, or when the transcription
// environment itself is missing or being installed (the lectures wait with
// component_installing / component_missing), the hint stays the one about
// the VRAM alone, as for an installed model.
func TestSystemVRAMHintOnlyForMissingModel(t *testing.T) {
	turbo := []string{"large-v3-turbo"}
	for _, c := range []struct {
		name  string
		loc   *fwLocator
		state string // devices.WorkerState of cuda:0's large-v3
		code  string
	}{
		{"model missing", &fwLocator{models: turbo}, devices.WorkerNoModel, HintVRAMLowMissing},
		{"model missing, another one being installed", &fwLocator{installing: turbo}, devices.WorkerNoModel, HintVRAMLowMissingInstall},
		{"model being installed", &fwLocator{models: turbo, installing: []string{"large-v3"}}, devices.WorkerInstalling, hwdetect.HintVRAMLow},
		{"model being installed, nothing installed", &fwLocator{installing: []string{"large-v3"}}, devices.WorkerInstalling, hwdetect.HintVRAMLow},
		{"environment missing", &fwLocator{noPython: true, models: turbo}, devices.WorkerNoComponent, hwdetect.HintVRAMLow},
		{"environment being installed", &fwLocator{noPython: true, pyInstalling: true, models: turbo}, devices.WorkerInstalling, hwdetect.HintVRAMLow},
		{"model installed", &fwLocator{models: []string{"large-v3", "large-v3-turbo"}}, devices.WorkerReady, hwdetect.HintVRAMLow},
	} {
		t.Run(c.name, func(t *testing.T) {
			noPython := filepath.Join(t.TempDir(), "no-python.exe") // the configured interpreter, not there
			de := vramEnv(t, 4096, "large-v3", func(s *Server) { s.cfg.Components, s.cfg.Python = c.loc, noPython })
			if got := devices.WorkerState(de.srv.cfg, de.srv.absDataDir(), model.BackendFasterWhisper, "large-v3", model.PrecisionAuto); got != c.state {
				t.Fatalf("worker state %q, want %q", got, c.state)
			}
			h, _ := vramHintsOf(t, de)
			got := h[model.DeviceCUDA0]
			if got == nil || got.Code != c.code {
				t.Fatalf("hint %+v, want %s", got, c.code)
			}
			want := map[string]any{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}
			for k, v := range want {
				if got.Params[k] != v {
					t.Errorf("%s = %v, want %v (%+v)", k, got.Params[k], v, got)
				}
			}
		})
	}
}

// r6 (T043): the hint of a missing model only advises: cuda:0 keeps its
// model and stays turned on, nothing is installed.
func TestSystemVRAMHintMissingChangesNothing(t *testing.T) {
	loc := &fwLocator{models: []string{"large-v3-turbo"}}
	de := vramEnv(t, 4096, "large-v3", func(s *Server) { s.cfg.Components = loc })
	for range 2 {
		h, _ := vramHintsOf(t, de)
		if got := h[model.DeviceCUDA0]; got == nil || got.Code != HintVRAMLowMissing {
			t.Fatalf("hint %+v, want %s", got, HintVRAMLowMissing)
		}
	}
	d, ok := de.reg.Get(model.DeviceCUDA0)
	if !ok || d.Model != "large-v3" || !d.Enabled || d.Precision != model.PrecisionAuto {
		t.Errorf("cuda:0 changed: %+v", d)
	}
	if a := de.srv.set.ASR(); a.Model != "large-v3" {
		t.Errorf("the ASR settings changed: %+v", a)
	}
	loc.mu.Lock()
	defer loc.mu.Unlock()
	if !reflect.DeepEqual(loc.models, []string{"large-v3-turbo"}) {
		t.Errorf("installed models changed: %v", loc.models)
	}
}

// A cuda:0 the user turned off gets no hint.
func TestSystemVRAMHintDisabled(t *testing.T) {
	de := vramEnv(t, 4096, "large-v3", nil)
	de.reg.mu.Lock()
	for i := range de.reg.rows {
		if de.reg.rows[i].ID == model.DeviceCUDA0 {
			de.reg.rows[i].Enabled = false
		}
	}
	de.reg.mu.Unlock()
	if h, _ := vramHintsOf(t, de); h[model.DeviceCUDA0] != nil {
		t.Errorf("turned-off cuda:0 has a hint %+v", h[model.DeviceCUDA0])
	}
}

// The hint only advises: cuda:0 keeps its model.
func TestSystemVRAMHintChangesNothing(t *testing.T) {
	de := vramEnv(t, 4096, "large-v3", nil)
	if h, _ := vramHintsOf(t, de); h[model.DeviceCUDA0] == nil {
		t.Fatal("no hint on a 4 GB card")
	}
	d, ok := de.reg.Get(model.DeviceCUDA0)
	if !ok || d.Model != "large-v3" {
		t.Errorf("cuda:0 changed: %+v", d)
	}
}

// Without an NVIDIA GPU there is no cuda:0 row, hence no hint.
func TestSystemVRAMHintNoNVIDIA(t *testing.T) {
	de := vramEnv(t, 4096, "large-v3", nil)
	de.reg.mu.Lock()
	de.reg.det.CUDAName = ""
	de.reg.det.CUDAErr = "nvidia-smi: not found"
	de.reg.mu.Unlock()
	sys := de.srv.systemView(context.Background())
	if len(sys.Devices) == 0 {
		t.Fatal("no devices")
	}
	for _, d := range sys.Devices {
		if d.ID == model.DeviceCUDA0 {
			t.Errorf("cuda:0 listed without an NVIDIA GPU")
		}
		if d.VRAMHint != nil {
			t.Errorf("%s has a VRAM hint without an NVIDIA GPU: %+v", d.ID, d.VRAMHint)
		}
	}
}
