// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"encoding/json"
	"runtime"

	"lectureflow/internal/api"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
)

// hasNVIDIAFunc is resources.Deps.HasNVIDIA (T046). It follows the
// diagnostics' rule (internal/api/system_env.go hasNVIDIA): never on
// macOS; yes when the device detection's nvidia-smi named a GPU or the
// remembered hardware detection (kv hardware.last) lists an NVIDIA GPU.
// Unlike the diagnostics, a computer about which nothing is known yet (no
// device detection finished, nothing remembered: the first start) counts
// as having one, so the resource bar keeps its old behaviour until the
// startup detection answers; the resource service asks again as soon as
// that detection finishes. r4 D10: a failed read of kv hardware.last is
// an error, not "nothing remembered" (which could mean "no NVIDIA"): the
// resource service does not keep an answer it could not get.
func hasNVIDIAFunc(st db.Store, detection func() devices.Detection) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		if runtime.GOOS == "darwin" {
			return false, nil
		}
		d := detection()
		if d.CUDAName != "" {
			return true, nil
		}
		h, remembered, err := hardwareLast(ctx, st)
		if err != nil {
			return false, err
		}
		if remembered && len(h.NVIDIA) > 0 {
			return true, nil
		}
		return d.At.IsZero() && !remembered, nil
	}
}

// hardwareLast reads kv hardware.last (api.KVHardwareLast); false when
// nothing (readable) was remembered, an error when the read failed.
func hardwareLast(ctx context.Context, st db.Store) (api.HardwareLast, bool, error) {
	if st == nil {
		return api.HardwareLast{}, false, nil
	}
	v, ok, err := st.GetKV(ctx, api.KVHardwareLast)
	if err != nil || !ok {
		return api.HardwareLast{}, false, err
	}
	var h api.HardwareLast
	if json.Unmarshal([]byte(v), &h) != nil {
		return api.HardwareLast{}, false, nil
	}
	return h, true, nil
}
