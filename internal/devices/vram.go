// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"strings"

	"lectureflow/internal/config"
)

// Per-task VRAM estimates of a faster-whisper run on an NVIDIA card, by
// model and resolved compute type (r2 T-PERF-02: the revision of the SPEC
// §17.2 estimate the user approved on 2026-11-11). Until then every task
// was estimated at vram_per_job_mb (5000 MB by default) whatever the
// model, so a card with less than about 6 GB never started a GPU task.
//
// The measured values come from the T10 measurement (2026-11-11, RTX 5070
// Ti Laptop GPU 12 GB, faster-whisper 1.2.1 / CTranslate2 4.8.3, one
// 55-minute lecture with the application's own job.json): the highest
// peak of a single run and of each process of two parallel runs, × 1.1,
// rounded up to 100 MB. "inferred" values were not measured; each comment
// says what it rests on. An inferred value is never below the measured
// value of a model at least as large.
//
//	model           float16                    int8_float16
//	large-v3        5000 measured (4520)       3100 measured (2800)
//	large-v2        4900 measured (4424)       3100 inferred: large-v3's (same architecture)
//	large-v3-turbo  2700 measured (2432)       1600 measured (1384)
//	medium          2600 measured (2312)       1800 measured (1581)
//
// Names mapped onto those rows (all inferred):
//   - large, large-v1: large-v3 / large-v2 (faster-whisper's "large" is
//     large-v3; large-v1 has large-v2's architecture);
//   - turbo: large-v3-turbo (faster-whisper's alias);
//   - distil-large-v2, distil-large-v3, distil-large-v3.5: large-v3 (the
//     same encoder with a smaller decoder: large-v3 is an upper bound);
//   - small, base, tiny, distil-medium.en, distil-small.en: medium (smaller
//     models: medium is an upper bound; small and base were not measured,
//     T10 §6);
//   - a ".en" suffix is ignored (the same size as the multilingual model).
//
// Any other name (a local path, a Hugging Face repository id) and any
// other compute type (float32, int8, int8_float32, bfloat16: not offered
// for CUDA devices, only reachable through a hand-written legacy config)
// are not in the table: the setting vram_per_job_mb stays the estimate
// (VRAMBaseMB), as before T-PERF-02. Neither is a card of compute
// capability below 7.0 (sched.vramEstimates): CTranslate2 runs float16 as
// float32 and int8_float16 as int8_float32 there, which takes more than
// these values (not measured).
type vramRow struct{ float16, int8Float16 int }

var vramTable = map[string]vramRow{
	"large-v3":       {float16: 5000, int8Float16: 3100},
	"large-v2":       {float16: 4900, int8Float16: 3100},
	"large-v3-turbo": {float16: 2700, int8Float16: 1600},
	"medium":         {float16: 2600, int8Float16: 1800},
}

// vramFamily is the vramTable row of a model name ("" when it has none).
func vramFamily(mdl string) string {
	s := strings.ToLower(strings.TrimSpace(mdl))
	if strings.ContainsAny(s, `/\`) {
		return "" // a path or repository id: its size is unknown
	}
	switch strings.TrimSuffix(s, ".en") {
	case "large-v3", "large", "distil-large-v2", "distil-large-v3", "distil-large-v3.5":
		return "large-v3"
	case "large-v2", "large-v1":
		return "large-v2"
	case "large-v3-turbo", "turbo":
		return "large-v3-turbo"
	case "medium", "small", "base", "tiny", "distil-medium", "distil-small":
		return "medium"
	}
	return ""
}

// VRAMTableMB is the table's per-task VRAM estimate of mdl at computeType,
// the compute type resolved for the card (config.ResolveComputeType:
// float16 or int8_float16); ok is false for a model or compute type the
// table does not know.
func VRAMTableMB(mdl, computeType string) (mb int, ok bool) {
	row, found := vramTable[vramFamily(mdl)]
	if !found {
		return 0, false
	}
	switch computeType {
	case "float16":
		return row.float16, true
	case "int8_float16":
		return row.int8Float16, true
	}
	return 0, false
}

// VRAMBaseMB is the per-task VRAM estimate before calibration of the
// setting vram_per_job_mb (settingMB) for mdl at computeType:
//   - the table does not know the model or compute type: the setting (as
//     before T-PERF-02; 0 for a setting <= 0);
//   - the setting is the default config.DefaultVRAMPerJobMB (5000, what
//     every installation imported once; it is not on the settings page)
//     or unset (<= 0): the table's value (never above 5000);
//   - any other setting is the user's own choice (config.toml before the
//     first run, or the API) and is kept as it is, below or above the
//     table: whoever raised it keeps the margin (R37 S2).
func VRAMBaseMB(settingMB int, mdl, computeType string) int {
	mb, ok := VRAMTableMB(mdl, computeType)
	switch {
	case !ok:
		return max(settingMB, 0)
	case settingMB > 0 && settingMB != config.DefaultVRAMPerJobMB:
		return settingMB
	}
	return mb
}
