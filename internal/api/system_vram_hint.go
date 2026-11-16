// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
)

// r6 (T043, the user's ruling USER-18 no. 5: "only a hint on the
// diagnostics page, the model is not changed"). On an NVIDIA card with less
// VRAM than the setup guide wants for cuda:0's model, the model is usually
// not installed either: the setup guide installs the model it recommends
// for the card (large-v3-turbo or medium below 6 GB), while cuda:0 may still
// name the model of the earlier settings (large-v3). Transcription on the
// card then waits with model_missing:cuda:0, and the hint about the VRAM
// alone (hwdetect.HintVRAMLow: "may not run or may be slow") does not say
// why. These two codes replace it in that case, with the same parameters
// {vram_gb, model, need_gb, suggest} (translated by web/i18n).
const (
	// HintVRAMLowMissing: the model needs more VRAM than the setup guide's
	// tier for the card, its files are missing, and the card's
	// recommendation {suggest} is installed: switching to it is the way out.
	HintVRAMLowMissing = "system.devices.vram_low_missing"
	// HintVRAMLowMissingInstall: the same, but the card's recommendation is
	// not installed either: install it (Settings, Components), then switch.
	HintVRAMLowMissingInstall = "system.devices.vram_low_missing_install"
)

// NVIDIAVRAMHint is the diagnostics hint of an NVIDIA device running mdl on
// a card with vramMB of VRAM: hwdetect.NVIDIAModelHint (the arguments are
// its own), except that its HintVRAMLow becomes HintVRAMLowMissing when the
// model's files are missing (modelMissing: devices.WorkerNoModel) and the
// card's recommendation is installed, HintVRAMLowMissingInstall when that
// is not installed either (installed nil: nothing is). Like every hint here
// it only advises: nothing is changed, installed or switched.
func NVIDIAVRAMHint(vramMB int, mdl string, modelMissing bool, installed func(name string) bool) *model.ErrorInfo {
	h := hwdetect.NVIDIAModelHint(vramMB, mdl, modelMissing, installed)
	if h == nil || h.Code != hwdetect.HintVRAMLow || !modelMissing {
		return h
	}
	h.Code = HintVRAMLowMissingInstall
	if suggest, _ := h.Params["suggest"].(string); suggest != "" && installed != nil && installed(suggest) {
		h.Code = HintVRAMLowMissing
	}
	return h
}
