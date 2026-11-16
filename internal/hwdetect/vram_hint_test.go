// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package hwdetect

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// NVIDIAModel is the model Recommend picks for one NVIDIA card of that
// size (the hint suggests what the setup guide would install).
func TestNVIDIAModelMatchesRecommend(t *testing.T) {
	for _, vram := range []int{1024, 3999, 4000, 4096, 5999, 6000, 6144, 8192, 12227, 24576} {
		r := Recommend(win(nv(vram)))
		want := NVIDIAModel(vram)
		found := false
		for _, c := range r.Components {
			if strings.HasPrefix(c, "model:faster-whisper:"+want+":") {
				found = true
			}
		}
		if !found {
			t.Errorf("%d MB: NVIDIAModel %q, Recommend %v", vram, want, r.Components)
		}
	}
}

func TestNVIDIAModelNeedMB(t *testing.T) {
	cases := []struct {
		model string
		need  int
		ok    bool
	}{
		{"large-v3", vramLargeMB, true}, {"large-v2", vramLargeMB, true}, {"Large-V3", vramLargeMB, true},
		{"large-v3-turbo", vramTurboMB, true}, {"turbo", vramTurboMB, true},
		{"medium", 0, true}, {"small.en", 0, true}, {"tiny", 0, true},
		{"distil-large-v3", 0, false}, {`C:\models\my-whisper`, 0, false}, {"", 0, false},
	}
	for _, c := range cases {
		need, ok := NVIDIAModelNeedMB(c.model)
		if need != c.need || ok != c.ok {
			t.Errorf("%q: %d %v, want %d %v", c.model, need, ok, c.need, c.ok)
		}
	}
}

func TestNVIDIAModelHint(t *testing.T) {
	type params = map[string]any
	cases := []struct {
		name      string
		vram      int
		model     string
		missing   bool
		installed []string // models whose files are there
		code      string   // "" = no hint
		params    params
	}{
		// too little VRAM for the model: hint with the card's recommendation
		{"4 GB large-v3", 4096, "large-v3", false, nil, HintVRAMLow, params{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}},
		{"3 GB large-v3", 3072, "large-v3", false, nil, HintVRAMLow, params{"vram_gb": 3, "model": "large-v3", "need_gb": 6, "suggest": "medium"}},
		{"2 GB turbo", 2048, "large-v3-turbo", false, nil, HintVRAMLow, params{"vram_gb": 2, "model": "large-v3-turbo", "need_gb": 4, "suggest": "medium"}},
		{"4 GB large-v3 missing", 4096, "large-v3", true, nil, HintVRAMLow, params{"vram_gb": 4, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}},
		// rounding: a card that would round up to the needed size is rounded down
		{"5800 MB large-v2", 5800, "large-v2", false, nil, HintVRAMLow, params{"vram_gb": 5, "model": "large-v2", "need_gb": 6, "suggest": "large-v3-turbo"}},
		{"5631 MB large-v3", 5631, "large-v3", false, nil, HintVRAMLow, params{"vram_gb": 5, "model": "large-v3", "need_gb": 6, "suggest": "large-v3-turbo"}},
		{"3700 MB turbo", 3700, "large-v3-turbo", false, nil, HintVRAMLow, params{"vram_gb": 3, "model": "large-v3-turbo", "need_gb": 4, "suggest": "medium"}},
		// enough VRAM: none
		{"12 GB large-v3", 12227, "large-v3", false, nil, "", nil},
		{"6 GB boundary", 6000, "large-v3", false, nil, "", nil},
		{"4 GB turbo", 4000, "large-v3-turbo", false, nil, "", nil},
		{"2 GB medium", 2048, "medium", false, nil, "", nil},
		// model missing, the card's recommendation installed: hint
		{"8 GB medium missing, large-v3 installed", 8192, "medium", true, []string{"large-v3"}, HintVRAMModelMissing, params{"vram_gb": 8, "model": "medium", "suggest": "large-v3"}},
		{"5 GB distil missing, turbo installed", 5120, "distil-large-v3", true, []string{"large-v3-turbo"}, HintVRAMModelMissing, params{"vram_gb": 5, "model": "distil-large-v3", "suggest": "large-v3-turbo"}},
		// model missing, the recommendation not installed either: none (the
		// user may have chosen the model on purpose; install it back)
		{"12 GB large-v2 missing, nothing installed", 12227, "large-v2", true, nil, "", nil},
		{"8 GB medium missing, only small installed", 8192, "medium", true, []string{"small"}, "", nil},
		// model missing but it is the recommendation: nothing to suggest
		{"12 GB large-v3 missing", 12227, "large-v3", true, []string{"large-v3"}, "", nil},
		// installed recommendation, but the model is not missing: none
		{"12 GB large-v2 present", 12227, "large-v2", false, []string{"large-v3"}, "", nil},
		// unknown model that may fit: no claim
		{"4 GB distil", 4096, "distil-large-v3", false, nil, "", nil},
		// VRAM unknown: never
		{"unknown VRAM", 0, "large-v3", false, nil, "", nil},
		{"unknown VRAM missing", 0, "large-v3", true, []string{"large-v3-turbo", "medium"}, "", nil},
		{"negative VRAM", -1, "large-v3", true, nil, "", nil},
		{"no model", 4096, "", true, []string{"large-v3-turbo"}, "", nil},
	}
	for _, c := range cases {
		installed := func(name string) bool { return slices.Contains(c.installed, name) }
		got := NVIDIAModelHint(c.vram, c.model, c.missing, installed)
		if c.code == "" {
			if got != nil {
				t.Errorf("%s: hint %+v, want none", c.name, got)
			}
			continue
		}
		if got == nil || got.Code != c.code || !reflect.DeepEqual(got.Params, map[string]any(c.params)) {
			t.Errorf("%s: hint %+v, want %s %v", c.name, got, c.code, c.params)
		}
	}
	// nil installed: nothing is installed
	if h := NVIDIAModelHint(8192, "medium", true, nil); h != nil {
		t.Errorf("nil installed: %+v", h)
	}
}
