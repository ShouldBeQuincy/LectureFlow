// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// r2 N15 (the real API: internal/api/system.go vramHints): the diagnostics
// hint of cuda:0 whose model does not suit the card's VRAM, or whose card
// is too small for the scheduler's per-task estimate plus the reserve.
// Since r2 T-PERF-02 the estimate follows cuda:0's model and precision
// (devices.VRAMBaseMB of the default 5000 MB setting; the GPU card's
// estimated_job_mb shows it in every scenario with an NVIDIA card).
//
//	lowvram        the default data on a 4 GB NVIDIA card: cuda:0 still
//	               runs the migrated large-v2 (auto: int8_float16, 3100
//	               MB), so the device table shows system.devices.vram_low
//	               (suggesting large-v3-turbo) with the sentence
//	               system.devices.vram_queue (3100 + the 1000 MB reserve
//	               is more than the card: lowering the reserve helps)
//	lowvram_fp16   the same card with cuda:0's precision float16 (4900 MB,
//	               more than the card): system.devices.vram_low with the
//	               sentence system.devices.vram_queue_switch (large-v3-turbo
//	               int8_float16, 1600 MB, fits)
//	lowvram_turbo  the same card after switching to large-v3-turbo (in the
//	               diagnostics' device table only): 1600 MB fits, no hint
//	lowvram_missing  r6 (T043): the same card where cuda:0's large-v2 is
//	               not installed (the setup guide installed large-v3-turbo
//	               for the card; lectures would wait with
//	               model_missing:cuda:0): system.devices.vram_low_missing
//	               (switch to the installed large-v3-turbo) instead of
//	               vram_low, without lowvram's queue sentence (R67: the
//	               wait is for the model, lowering the reserve would not
//	               help)
//	lowvram_missing_install  the same, large-v3-turbo not installed either:
//	               system.devices.vram_low_missing_install (install it
//	               first; the row also links to the Components page)
//	vram_raised    the default data after a transcription ran out of GPU
//	               memory: the per-task estimate raised in memory (× 1.25,
//	               to 100 MB, as sched.raiseVRAMAfterOOM) and the GPU card's
//	               system.gpu.per_job_raised (estimate_raised)
//
// Every other scenario gets the hint the real API would give for its GPU
// card (none on the default 12 GB card). Only the two lowvram_missing
// scenarios have a missing model (in the diagnostics' hint only: the
// components page and the lectures' waiting reasons are the default
// data's), so system.devices.vram_model_missing is not shown here.

import (
	"fmt"
	"net/http"

	"lectureflow/internal/api"
	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
)

// lowVRAMTotalMB is what nvidia-smi reports for a 4 GB card.
const lowVRAMTotalMB = 4096

// vramSystem sets the lowvram scenarios' GPU card and gpu check, then the
// per-task estimate and VRAM hint of cuda:0 (after sys.Devices and
// sys.Checks are set).
func vramSystem(r *http.Request, sys *api.System) {
	sc := scenario(r)
	// r6 (T043): cuda:0's model is not installed; the card's recommendation
	// is (lowvram_missing) or is not either (lowvram_missing_install)
	missing := sc == "lowvram_missing" || sc == "lowvram_missing_install"
	installed := func(string) bool { return sc == "lowvram_missing" }
	low := sc == "lowvram" || sc == "lowvram_fp16" || sc == "lowvram_turbo" || missing
	if low {
		// nothing runs on the card: the hint judges by the free memory
		// (api.VRAMAvail, R37 M1), 512 MB taken by other programs
		sys.GPU.TotalMB, sys.GPU.FreeMB, sys.GPU.SlotsUsed = lowVRAMTotalMB, lowVRAMTotalMB-512, 0
		for i := range sys.Checks {
			if c := &sys.Checks[i]; c.Name == "gpu" && c.DetailInfo != nil {
				c.DetailInfo = &model.ErrorInfo{Code: c.DetailInfo.Code, Params: map[string]any{"total": sys.GPU.TotalMB, "free": sys.GPU.FreeMB}}
				c.Detail = fmt.Sprintf("总显存 %d MB，剩余 %d MB", sys.GPU.TotalMB, sys.GPU.FreeMB)
			}
		}
	}
	if sys.GPU.TotalMB <= 0 {
		return
	}
	estimate := func(mdl, precision string) int {
		return devices.VRAMBaseMB(mockVRAMPerJobMB, mdl, config.ResolveComputeType(precision, "cuda", sys.GPU.TotalMB))
	}
	for i := range sys.Devices {
		if d := &sys.Devices[i]; d.ID == model.DeviceCUDA0 {
			if low {
				d.SlotsUsed, d.Running = 0, []api.DeviceTaskView{}
			}
			switch sc {
			case "lowvram_turbo":
				d.Model = "large-v3-turbo"
			case "lowvram_fp16":
				d.Precision = model.PrecisionFloat16
			}
			sys.GPU.EstimatedJobMB = estimate(d.Model, d.Precision) // sched.GPUStatus: cuda:0's
			if sc == "vram_raised" {
				sys.GPU.EstimatedJobMB = ((sys.GPU.EstimatedJobMB*5+3)/4 + 99) / 100 * 100
				sys.GPU.EstimateRaised = true
			}
			if !d.Enabled {
				continue
			}
			fit := api.VRAMFit{Model: hwdetect.NVIDIAModel(sys.GPU.TotalMB), Precision: config.ResolveComputeType(model.PrecisionAuto, "cuda", sys.GPU.TotalMB)}
			fit.MB = estimate(fit.Model, fit.Precision)
			h := api.NVIDIAVRAMHint(sys.GPU.TotalMB, d.Model, missing, installed)
			d.VRAMHint = api.WithVRAMQueue(h, sys.GPU.TotalMB, api.VRAMAvail(sys.GPU), sys.GPU.EstimatedJobMB, mockVRAMReserveMB, fit)
		}
	}
}

// mockVRAMReserveMB and mockVRAMPerJobMB are the defaults of vram_reserve_mb
// and vram_per_job_mb (settings.helpers).
const (
	mockVRAMReserveMB = 1000
	mockVRAMPerJobMB  = 5000
)
