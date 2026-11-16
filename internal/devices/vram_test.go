// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"testing"

	"lectureflow/internal/config"
)

// r2 T-PERF-02: the per-task VRAM table (T10 peaks × 1.1, rounded up to
// 100 MB) and the setting's role.
func TestVRAMTable(t *testing.T) {
	for _, c := range []struct {
		mdl, ct string
		mb      int
		ok      bool
	}{
		// measured
		{"large-v3", "float16", 5000, true},
		{"large-v3", "int8_float16", 3100, true},
		{"large-v2", "float16", 4900, true},
		{"large-v3-turbo", "float16", 2700, true},
		{"large-v3-turbo", "int8_float16", 1600, true},
		{"medium", "float16", 2600, true},
		{"medium", "int8_float16", 1800, true},
		// inferred
		{"large-v2", "int8_float16", 3100, true},
		{"large", "float16", 5000, true},
		{"large-v1", "float16", 4900, true},
		{" Turbo ", "int8_float16", 1600, true},
		{"distil-large-v3", "float16", 5000, true},
		{"distil-large-v3.5", "int8_float16", 3100, true},
		{"medium.en", "float16", 2600, true},
		{"small", "float16", 2600, true},
		{"base.en", "int8_float16", 1800, true},
		{"tiny", "float16", 2600, true},
		{"distil-small.en", "int8_float16", 1800, true},
		// unknown: the setting stays
		{"large-v3", "float32", 0, false},
		{"large-v3", "int8", 0, false},
		{"large-v3", "auto", 0, false}, // not resolved
		{"Systran/faster-whisper-large-v3", "float16", 0, false},
		{`D:\models\medium`, "float16", 0, false},
		{"whisper-custom", "float16", 0, false},
		{"", "float16", 0, false},
	} {
		if mb, ok := VRAMTableMB(c.mdl, c.ct); mb != c.mb || ok != c.ok {
			t.Errorf("VRAMTableMB(%q, %q) = %d, %v; want %d, %v", c.mdl, c.ct, mb, ok, c.mb, c.ok)
		}
	}
	// An inferred value is never below the measured value of a model at
	// least as large (small/base/tiny take medium's, large-v2 int8 large-v3's).
	for _, ct := range []string{"float16", "int8_float16"} {
		turbo, _ := VRAMTableMB("large-v3-turbo", ct)
		v2, _ := VRAMTableMB("large-v2", ct)
		v3, _ := VRAMTableMB("large-v3", ct)
		if v2 > v3 || turbo > v2 {
			t.Errorf("%s: large-v3 %d, large-v2 %d, turbo %d out of order", ct, v3, v2, turbo)
		}
	}
}

func TestVRAMBaseMB(t *testing.T) {
	for _, c := range []struct {
		setting int
		mdl, ct string
		want    int
	}{
		{5000, "large-v3", "float16", 5000},
		{5000, "large-v3-turbo", "int8_float16", 1600},
		{5000, "medium", "float16", 2600},
		{1500, "large-v3-turbo", "int8_float16", 1500}, // a lower setting is kept
		{3000, "large-v3-turbo", "int8_float16", 3000}, // R37 S2: any value but the default is kept
		{8000, "large-v3", "float16", 8000},            // a higher one too
		{config.DefaultVRAMPerJobMB, "large-v3-turbo", "float16", 2700},
		{5000, "whisper-custom", "float16", 5000}, // unknown model: the setting
		{6000, "large-v3", "float32", 6000},       // unknown compute type: the setting
		{0, "medium", "int8_float16", 1800},
		{0, "whisper-custom", "float16", 0},
	} {
		if got := VRAMBaseMB(c.setting, c.mdl, c.ct); got != c.want {
			t.Errorf("VRAMBaseMB(%d, %q, %q) = %d, want %d", c.setting, c.mdl, c.ct, got, c.want)
		}
	}
}
