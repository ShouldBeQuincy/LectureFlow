// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/hwdetect"
)

// r2 N15: the diagnostics page shows a device's vram_hint (GET
// /api/system) under the device table, in the checks' warning style, for
// the two codes the back end sends; every catalog has both keys with the
// back end's parameters.
func TestVRAMHintTexts(t *testing.T) {
	keys := map[string][]string{
		hwdetect.HintVRAMLow:           {"{vram_gb}", "{model}", "{need_gb}", "{suggest}"},
		hwdetect.HintVRAMModelMissing:  {"{vram_gb}", "{model}", "{suggest}"},
		hwdetect.HintVRAMQueueOnly:     {"{vram_gb}"},
		hwdetect.HintVRAMQueue:         {"{per_job_mb}", "{reserve_mb}"},
		hwdetect.HintVRAMQueueEstimate: {"{per_job_mb}"},
		hwdetect.HintVRAMQueueSwitch:   {"{per_job_mb}", "{reserve_mb}", "{suggest}", "{suggest_precision}", "{suggest_mb}"},
	}
	for _, loc := range []string{"zh-CN", "en", "hi", "ru", "zh-TW", "yue-Hant-HK"} {
		c := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		for key, params := range keys {
			s := text(t, c, key)
			for _, p := range params {
				if !strings.Contains(s, p) {
					t.Errorf("%s %s lacks %s: %q", loc, key, p, s)
				}
			}
		}
	}
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	// R37 M1: the sentences judge by what the card has available now, after
	// other programs (the earlier keys without the number: the other four
	// catalogs keep their parameters until re-translated)
	for _, x := range []struct {
		c    catalog
		name string
		w    string
	}{{zh, "zh-CN", "现在可用的显存"}, {en, "en", "available now"}} {
		for _, key := range []string{hwdetect.HintVRAMQueue, hwdetect.HintVRAMQueueEstimate, hwdetect.HintVRAMQueueSwitch} {
			if s := text(t, x.c, key); !strings.Contains(s, x.w) {
				t.Errorf("%s %s does not say %q: %q", x.name, key, x.w, s)
			}
		}
		if s := text(t, x.c, hwdetect.HintVRAMQueueSwitch); !strings.Contains(s, "{avail_mb}") {
			t.Errorf("%s vram_queue_switch lacks {avail_mb}: %q", x.name, s)
		}
	}
	for key := range keys {
		if key == hwdetect.HintVRAMQueueOnly { // always followed by a queue sentence
			continue
		}
		if s := text(t, zh, key); !strings.Contains(s, "显存") || !strings.Contains(s, "设置") {
			t.Errorf("zh-CN %s does not name 显存 and 设置: %q", key, s)
		}
	}
	// R27 A-2/A-4: no promise that switching makes it run; the setup guide's
	// suggestion, not a hard limit
	if s := text(t, zh, hwdetect.HintVRAMLow); !strings.Contains(s, "可能跑不起来或很慢") || !strings.Contains(s, "引导建议") {
		t.Errorf("zh-CN vram_low: %q", s)
	}
	// R27 A-1: only an installed recommendation is suggested
	if s := text(t, zh, hwdetect.HintVRAMModelMissing); !strings.Contains(s, "已安装的 {suggest}") {
		t.Errorf("zh-CN vram_model_missing: %q", s)
	}
	// R27 A-7/A-8: "likely" waiting; the reserve can be lowered in
	// Settings. T-PERF-02: the estimate follows the model, so it is no
	// longer called unchangeable: the recommended model is suggested when
	// it fits, another device when nothing fits.
	for _, x := range []struct {
		c                    catalog
		name, key, likely, w string
	}{
		// only this card's transcription waits (R27 re-review)
		{zh, "zh-CN", hwdetect.HintVRAMQueue, "这块显卡上的转写很可能一直等待显存", "调低“保留显存”"},
		{zh, "zh-CN", hwdetect.HintVRAMQueueEstimate, "这块显卡上的转写很可能一直等待显存", "即使换成为这块显卡推荐的模型"},
		{zh, "zh-CN", hwdetect.HintVRAMQueueSwitch, "这块显卡上的转写很可能一直等待显存", "把这块显卡换成 {suggest}"},
		{en, "en", hwdetect.HintVRAMQueue, "transcription on this card will likely keep waiting", "lower “Reserved GPU memory”"},
		{en, "en", hwdetect.HintVRAMQueueEstimate, "transcription on this card will likely keep waiting", "Even the model recommended for this card does not fit"},
		{en, "en", hwdetect.HintVRAMQueueSwitch, "transcription on this card will likely keep waiting", "switch this card to {suggest}"},
	} {
		if s := text(t, x.c, x.key); !strings.Contains(s, x.likely) || !strings.Contains(s, x.w) {
			t.Errorf("%s %s: %q", x.name, x.key, s)
		}
	}
	// vram_queue_estimate names the settings section and the devices as the
	// page does; vram_queue_switch the section and the precision label
	for _, c := range []catalog{zh, en} {
		sw := text(t, c, hwdetect.HintVRAMQueueSwitch)
		for _, k := range []string{"settings.block.asr", "devices.precision"} {
			if l := text(t, c, k); !strings.Contains(strings.ToLower(sw), strings.ToLower(l)) {
				t.Errorf("vram_queue_switch lacks %s (%q): %q", k, l, sw)
			}
		}
		if l := text(t, c, "settings.block.asr"); !strings.Contains(sw, "“"+l+"”") {
			t.Errorf("vram_queue_switch does not quote the section %q: %q", l, sw)
		}
		s := text(t, c, hwdetect.HintVRAMQueueEstimate)
		for _, k := range []string{"settings.block.asr", "devices.kind.cpu", "devices.kind.remote"} {
			if l := text(t, c, k); !strings.Contains(s, l) {
				t.Errorf("vram_queue_estimate lacks %s (%q): %q", k, l, s)
			}
		}
		if l := text(t, c, "settings.block.asr"); !strings.Contains(s, "“"+l+"”") {
			t.Errorf("vram_queue_estimate does not quote the section %q: %q", l, s)
		}
	}
	// the queue sentences name the page's own labels
	for _, x := range []struct {
		c          catalog
		name, uiKy string
	}{{zh, "zh-CN", "system.gpu.per_job"}, {zh, "zh-CN", "settings.asr.vram_reserve"}, {en, "en", "system.gpu.per_job"}, {en, "en", "settings.asr.vram_reserve"}} {
		label := text(t, x.c, x.uiKy)
		for _, key := range []string{hwdetect.HintVRAMQueue, hwdetect.HintVRAMQueueEstimate, hwdetect.HintVRAMQueueSwitch} {
			if s := text(t, x.c, key); !strings.Contains(s, "“"+label+"”") {
				t.Errorf("%s %s does not quote %s (%q): %q", x.name, key, x.uiKy, label, s)
			}
		}
	}

	b, err := os.ReadFile("view_system.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, w := range []string{
		// the known codes only
		"'" + hwdetect.HintVRAMLow + "'", "'" + hwdetect.HintVRAMModelMissing + "'", "'" + hwdetect.HintVRAMQueueOnly + "'",
		"'" + hwdetect.HintVRAMQueue + "'", "'" + hwdetect.HintVRAMQueueEstimate + "'", "'" + hwdetect.HintVRAMQueueSwitch + "'",
		"info.params.extra",
		"d.vram_hint", "class: 'sys-hint is-warn'", "'#/settings/asr'", "...vramHints",
	} {
		if !strings.Contains(src, w) {
			t.Errorf("view_system.js lacks %s", w)
		}
	}
}

// R67: the NVIDIA card hints name the setup guide and the GPU memory as the
// interface and web/i18n/glossary.md do: zh-CN 引导 (settings.app
// .onboarding_label; 向导 is the add-course wizard), en "GPU memory"
// (system.gpu.vram on the same page), not "video memory". zh-CN and en
// only; the same word in other places (hw.reason.*) is not this family.
func TestVRAMHintGlossaryTerms(t *testing.T) {
	const lowMissing, lowMissingInstall = "system.devices.vram_low_missing", "system.devices.vram_low_missing_install"
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	guideZh, guideEn := text(t, zh, "settings.app.onboarding_label"), strings.ToLower(text(t, en, "settings.app.onboarding_label"))
	if guideZh != "引导" || guideEn != "setup guide" {
		t.Fatalf("the setup guide's name: %q, %q", guideZh, guideEn)
	}
	// the keys that name the setup guide
	for _, key := range []string{hwdetect.HintVRAMLow, lowMissing, lowMissingInstall} {
		if s := text(t, zh, key); strings.Contains(s, "向导") || !strings.Contains(s, guideZh+"建议 {need_gb} GB 以上") {
			t.Errorf("zh-CN %s does not call the setup guide %q: %q", key, guideZh, s)
		}
		if s := text(t, en, key); !strings.Contains(s, "the "+guideEn+" suggests {need_gb} GB or more") {
			t.Errorf("en %s does not call the setup guide %q: %q", key, guideEn, s)
		}
	}
	for _, key := range []string{lowMissing, lowMissingInstall} {
		if s := text(t, zh, key); !strings.Contains(s, "所以"+guideZh+"为这样的显卡推荐的是 {suggest}") {
			t.Errorf("zh-CN %s: %q", key, s)
		}
	}
	// the keys that name the card's memory
	memZh, memEn := text(t, zh, "system.gpu.vram"), text(t, en, "system.gpu.vram")
	if memZh != "显存" || memEn != "GPU memory" {
		t.Fatalf("the GPU memory's name: %q, %q", memZh, memEn)
	}
	for _, key := range []string{hwdetect.HintVRAMLow, hwdetect.HintVRAMModelMissing, hwdetect.HintVRAMQueueOnly, lowMissing, lowMissingInstall} {
		if s := text(t, en, key); strings.Contains(strings.ToLower(s), "video memory") || !strings.Contains(s, "{vram_gb} GB of "+memEn) {
			t.Errorf("en %s does not call the card's memory %q: %q", key, memEn, s)
		}
		if s := text(t, zh, key); !strings.Contains(s, memZh+"约 {vram_gb} GB") {
			t.Errorf("zh-CN %s does not call the card's memory %q: %q", key, memZh, s)
		}
	}
}

// r6 (T043, USER-18 no. 5): on a card with less VRAM than the setup guide
// wants for cuda:0's model, that model is usually not installed either
// (the scheduler waits with model_missing). The diagnostics page then says
// so in one hint: the model is not installed, the card's VRAM is short for
// it, and what to do — switch to the card's recommendation when that is
// installed (vram_low_missing), else install it first
// (vram_low_missing_install). zh-CN and en only until the other four
// catalogs are translated (the page falls back meanwhile).
func TestVRAMLowMissingTexts(t *testing.T) {
	const installedKey, installKey = "system.devices.vram_low_missing", "system.devices.vram_low_missing_install"
	for _, x := range []struct {
		loc                                  string
		start, cannot, needsMore, slow       string
		guide, isInstalled, installIt, comps string
	}{
		{"zh-CN", "当前模型 {model} 还没有安装", "现在没法用它转写", "通常需要更多显存", "可能跑不起来或很慢",
			"引导建议 {need_gb} GB 以上", "已安装的 {suggest}", "安装 {suggest}", "设置的“%s”"},
		{"en", "The current model {model} is not installed", "cannot transcribe with it", "usually needs more", "may not run or may be slow",
			"the setup guide suggests {need_gb} GB or more", "{suggest}, which is installed", "install {suggest}", "Settings → %s"},
	} {
		c := loadCatalog(t, filepath.Join(catalogDir, x.loc+".json"))
		asr, comp := text(t, c, "settings.block.asr"), text(t, c, "settings.comp.title")
		for _, key := range []string{installedKey, installKey} {
			s := text(t, c, key)
			for _, p := range []string{"{vram_gb}", "{model}", "{need_gb}", "{suggest}"} {
				if !strings.Contains(s, p) {
					t.Errorf("%s %s lacks %s: %q", x.loc, key, p, s)
				}
			}
			// the cause first, then the VRAM in the earlier hint's cautious
			// words (R27 A-2/A-4: the setup guide's suggestion, no promise
			// that the model runs or that switching starts transcription)
			if !strings.HasPrefix(s, x.start) {
				t.Errorf("%s %s does not start with %q: %q", x.loc, key, x.start, s)
			}
			for _, w := range []string{x.cannot, x.needsMore, x.slow, x.guide} {
				if !strings.Contains(s, w) {
					t.Errorf("%s %s does not say %q: %q", x.loc, key, w, s)
				}
			}
			// where to go: the settings sections by the page's own names
			if !strings.Contains(s, "“"+asr+"”") {
				t.Errorf("%s %s does not quote the section %q: %q", x.loc, key, asr, s)
			}
			if w := strings.ReplaceAll(x.comps, "%s", comp); !strings.Contains(s, w) {
				t.Errorf("%s %s does not name %q: %q", x.loc, key, w, s)
			}
		}
		// only an installed recommendation is called installed (R27 A-1)
		if s := text(t, c, installedKey); !strings.Contains(s, x.isInstalled) {
			t.Errorf("%s %s does not say %q: %q", x.loc, installedKey, x.isInstalled, s)
		}
		if s := text(t, c, installKey); strings.Contains(s, x.isInstalled) || !strings.Contains(s, x.installIt) {
			t.Errorf("%s %s must say %q and not %q: %q", x.loc, installKey, x.installIt, x.isInstalled, s)
		}
	}
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	for _, key := range []string{installedKey, installKey} {
		if s := text(t, zh, key); !strings.Contains(s, "显存") || !strings.Contains(s, "设置") {
			t.Errorf("zh-CN %s does not name 显存 and 设置: %q", key, s)
		}
	}
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	for _, key := range []string{installedKey, installKey} {
		if _, ok := ctx[key]; !ok {
			t.Errorf("context.json has no entry for %s", key)
		}
	}

	b, err := os.ReadFile("view_system.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, w := range []string{
		"'" + installedKey + "'", "'" + installKey + "'",
		// the hint that says to install a model links to Components too
		"'#/settings/components'", "`sys:vramcomp:${d.id}`", "export function vramHintRow(d)",
	} {
		if !strings.Contains(src, w) {
			t.Errorf("view_system.js lacks %s", w)
		}
	}
}
