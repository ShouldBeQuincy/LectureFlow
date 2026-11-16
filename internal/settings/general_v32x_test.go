// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// v3.2.x: general settings auto_chapters (P3) and llm_idle_timeout_s (P1-c).

func TestGeneralAutoChaptersAndIdleTimeout(t *testing.T) {
	s, st := newService(t)
	// Settings saved before the options (no keys) read as the defaults.
	if err := st.SetKV(bg(), model.KVSettingsGeneral, `{"keep_original_media":true,"notifications":true,"daily_budget":2}`); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(bg()); err != nil {
		t.Fatal(err)
	}
	g := s.General()
	if !g.AutoChaptersOn() || g.LLMIdleTimeout() != model.DefaultLLMIdleTimeoutS*time.Second || g.DailyBudget != 2 {
		t.Fatalf("old settings: %+v", g)
	}

	off, idle := false, 0
	if err := s.SetGeneral(bg(), model.GeneralSettings{KeepOriginalMedia: true, AutoChapters: &off, LLMIdleTimeoutS: &idle}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := st.GetKV(bg(), model.KVSettingsGeneral); v != `{"keep_original_media":true,"notifications":false,"daily_budget":0,"auto_chapters":false,"llm_idle_timeout_s":0}` {
		t.Fatalf("stored %s", v)
	}
	g = s.General()
	if g.AutoChaptersOn() || g.LLMIdleTimeout() != 0 {
		t.Fatalf("off / no idle timeout: %+v", g)
	}
	// General hands out copies.
	*g.AutoChapters, *g.LLMIdleTimeoutS = true, 99
	if g2 := s.General(); *g2.AutoChapters || *g2.LLMIdleTimeoutS != 0 {
		t.Fatalf("General shares its pointers: %+v", g2)
	}
	s2, _ := Open(bg(), st, nil)
	if g := s2.General(); g.AutoChaptersOn() || g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != 0 {
		t.Fatalf("reloaded: %+v", g)
	}

	on, max := true, MaxLLMIdleTimeoutS
	if err := s.SetGeneral(bg(), model.GeneralSettings{AutoChapters: &on, LLMIdleTimeoutS: &max}); err != nil {
		t.Fatal(err)
	}
	if g := s.General(); !g.AutoChaptersOn() || g.LLMIdleTimeout() != time.Hour {
		t.Fatalf("on / 3600: %+v", g)
	}
	// Leaving the keys out goes back to the defaults.
	if err := s.SetGeneral(bg(), model.GeneralSettings{}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := st.GetKV(bg(), model.KVSettingsGeneral); v != `{"keep_original_media":false,"notifications":false,"daily_budget":0}` {
		t.Fatalf("stored %s", v)
	}

	before, _, _ := st.GetKV(bg(), model.KVSettingsGeneral)
	for _, bad := range []int{-1, 1, MinLLMIdleTimeoutS - 1, MaxLLMIdleTimeoutS + 1, 1 << 30} {
		v := bad
		err := s.SetGeneral(bg(), model.GeneralSettings{LLMIdleTimeoutS: &v})
		if !errors.Is(err, ErrInvalid) || codeOf(err) != CodeIdleTimeoutInvalid {
			t.Errorf("idle %d: %v", bad, err)
		}
	}
	if after, _, _ := st.GetKV(bg(), model.KVSettingsGeneral); after != before {
		t.Fatalf("a refused SetGeneral wrote %s", after)
	}
}

// SEC5 N4: llm_idle_timeout_s is 0 (off) or 30..3600, the floor the LLM
// client applies; a value of 1..29 saved before reads as 30, and saving the
// block again (the API merges into General) passes.
func TestGeneralIdleTimeoutFloor(t *testing.T) {
	if MinLLMIdleTimeoutS != 30 {
		t.Fatalf("MinLLMIdleTimeoutS %d, want 30", MinLLMIdleTimeoutS)
	}
	s, st := newService(t)
	for _, ok := range []int{0, MinLLMIdleTimeoutS, 100, MaxLLMIdleTimeoutS} {
		v := ok
		if err := s.SetGeneral(bg(), model.GeneralSettings{LLMIdleTimeoutS: &v}); err != nil {
			t.Errorf("idle %d: %v", ok, err)
		}
	}
	v := 5
	err := s.SetGeneral(bg(), model.GeneralSettings{LLMIdleTimeoutS: &v})
	if !errors.Is(err, ErrInvalid) || !hasCode(err, CodeIdleTimeoutInvalid, map[string]any{"min": MinLLMIdleTimeoutS, "max": MaxLLMIdleTimeoutS}) {
		t.Fatalf("idle 5: %v", err)
	}

	for _, old := range []int{1, 5, 29} {
		raw := fmt.Sprintf(`{"keep_original_media":true,"notifications":true,"daily_budget":2,"llm_idle_timeout_s":%d}`, old)
		if err := st.SetKV(bg(), model.KVSettingsGeneral, raw); err != nil {
			t.Fatal(err)
		}
		if err := s.Load(bg()); err != nil {
			t.Fatal(err)
		}
		g := s.General()
		if g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != MinLLMIdleTimeoutS || g.DailyBudget != 2 {
			t.Fatalf("stored %d reads as %+v", old, g)
		}
		g.DailyBudget = 3
		if err := s.SetGeneral(bg(), g); err != nil {
			t.Fatalf("stored %d: saving again: %v", old, err)
		}
		if v, _, _ := st.GetKV(bg(), model.KVSettingsGeneral); v != `{"keep_original_media":true,"notifications":true,"daily_budget":3,"llm_idle_timeout_s":30}` {
			t.Fatalf("stored %d: saved %s", old, v)
		}
	}
	// 0 stays off.
	if err := st.SetKV(bg(), model.KVSettingsGeneral, `{"llm_idle_timeout_s":0}`); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(bg()); err != nil {
		t.Fatal(err)
	}
	if g := s.General(); g.LLMIdleTimeoutS == nil || *g.LLMIdleTimeoutS != 0 {
		t.Fatalf("stored 0 reads as %+v", g)
	}
}

// The default idle timeout is 90 s (approved by the user 2026-11-11: during
// hidden thinking the real API sent a ping every 30 s, the longest gap of
// the real acceptance T9 was 30.0 s). It is the default only: no value is
// stored for it, so a value the user saved — the old default 180 too — is
// kept as it is, and only settings without one change.
func TestGeneralIdleTimeoutDefault(t *testing.T) {
	if model.DefaultLLMIdleTimeoutS != 90 {
		t.Fatalf("DefaultLLMIdleTimeoutS %d, want 90", model.DefaultLLMIdleTimeoutS)
	}
	s, st := newService(t)
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{`{"keep_original_media":true}`, 90 * time.Second},
		{`{"keep_original_media":true,"llm_idle_timeout_s":180}`, 180 * time.Second},
		{`{"keep_original_media":true,"llm_idle_timeout_s":300}`, 300 * time.Second},
	} {
		if err := st.SetKV(bg(), model.KVSettingsGeneral, tc.raw); err != nil {
			t.Fatal(err)
		}
		if err := s.Load(bg()); err != nil {
			t.Fatal(err)
		}
		if got := s.General().LLMIdleTimeout(); got != tc.want {
			t.Errorf("%s: idle timeout %v, want %v", tc.raw, got, tc.want)
		}
		if v, _, _ := st.GetKV(bg(), model.KVSettingsGeneral); v != tc.raw {
			t.Errorf("loading rewrote %s as %s", tc.raw, v)
		}
	}
}
