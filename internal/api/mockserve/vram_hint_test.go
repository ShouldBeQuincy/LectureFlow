// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
)

// r2 N15: the lowvram scenarios show cuda:0's VRAM hint; the default data
// (12 GB card) and the cards without NVIDIA show none. T-PERF-02: the
// per-task estimate follows cuda:0's model and precision, so the 4 GB card
// with large-v3-turbo shows none either, and a model too large for the
// card gets the sentence suggesting one that fits.
func TestMockVRAMHint(t *testing.T) {
	srv := testServer(t)
	for sc, c := range map[string]struct {
		code, extra string
		perJob      float64 // decoded JSON
	}{
		"lowvram":       {hwdetect.HintVRAMLow, hwdetect.HintVRAMQueue, 3100},
		"lowvram_fp16":  {hwdetect.HintVRAMLow, hwdetect.HintVRAMQueueSwitch, 4900},
		"lowvram_turbo": {"", "", 1600},
		// r6 (T043): cuda:0's large-v2 is not installed on the 4 GB card;
		// large-v3-turbo is installed / is not installed either. R67: no
		// sentence about lowering the reserve (the wait is for the model)
		"lowvram_missing":         {api.HintVRAMLowMissing, "", 3100},
		"lowvram_missing_install": {api.HintVRAMLowMissingInstall, "", 3100},
	} {
		var sys api.System
		doJSON(t, "GET", srv.URL+"/api/system?mock="+sc, nil, http.StatusOK, &sys)
		if sys.GPU.TotalMB != lowVRAMTotalMB || float64(sys.GPU.EstimatedJobMB) != c.perJob {
			t.Errorf("%s gpu %+v", sc, sys.GPU)
		}
		for _, d := range sys.Devices {
			switch {
			case d.ID == model.DeviceCUDA0 && c.code == "":
				if d.VRAMHint != nil {
					t.Errorf("%s cuda:0 hint %+v, want none", sc, d.VRAMHint)
				}
			case d.ID == model.DeviceCUDA0:
				if h := d.VRAMHint; h == nil || h.Code != c.code {
					t.Errorf("%s cuda:0 hint %+v, want %s", sc, h, c.code)
				} else if h.Params["suggest"] != "large-v3-turbo" || h.Params["model"] != d.Model {
					t.Errorf("%s cuda:0 hint %+v", sc, h)
				} else if x, has := h.Params["extra"]; c.extra == "" {
					if has {
						t.Errorf("%s cuda:0 has a queue sentence: %+v", sc, x)
					}
				} else if x, _ := h.Params["extra"].([]any); len(x) != 1 { // decoded JSON
					t.Errorf("%s cuda:0 without the queue sentence: %+v", sc, h)
				} else if e, _ := x[0].(map[string]any); e["code"] != c.extra {
					t.Errorf("%s cuda:0 extra %+v", sc, x)
				} else if p, _ := e["params"].(map[string]any); p["per_job_mb"] != c.perJob || p["reserve_mb"] != float64(mockVRAMReserveMB) ||
					p["avail_mb"] != float64(lowVRAMTotalMB-512) { // R37 M1: the free memory
					t.Errorf("%s cuda:0 extra %+v", sc, x)
				} else if c.extra == hwdetect.HintVRAMQueueSwitch && (p["suggest"] != "large-v3-turbo" || p["suggest_precision"] != "int8_float16" || p["suggest_mb"] != float64(1600)) {
					t.Errorf("%s cuda:0 extra %+v", sc, x)
				}
			case d.VRAMHint != nil:
				t.Errorf("%s %s hint %+v", sc, d.ID, d.VRAMHint)
			}
		}
	}
	for _, sc := range []string{"1", "cpuonly", "diag_intel"} {
		var sys api.System
		doJSON(t, "GET", srv.URL+"/api/system?mock="+sc, nil, http.StatusOK, &sys)
		for _, d := range sys.Devices {
			if d.VRAMHint != nil {
				t.Errorf("%s: %s hint %+v", sc, d.ID, d.VRAMHint)
			}
		}
		// the default 12 GB card: cuda:0's large-v2 at float16
		if sc == "1" && sys.GPU.EstimatedJobMB != 4900 {
			t.Errorf("default card estimate %d, want 4900", sys.GPU.EstimatedJobMB)
		}
	}
}
