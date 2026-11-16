// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// colab-obs-web (user decisions 2026-10-27 07:00 and 14:50): the resource
// panel's Colab area (the account without a session; the machine, its
// sparklines and the segmented memory bar with one), the rail's Colab
// cell, the settings page's account row and accelerator radio list (no
// paid / free switch, no subscription row), the lecture page's speed-mode
// notice. The logic runs in Node (testdata/colab_obs_test.mjs); these tests
// check the texts and how the pieces are wired.

var colabObsKeys = []string{
	"resources.remote.level_k", "resources.remote.gpu_util", "resources.remote.fit_k", "resources.remote.fit_unit", "resources.remote.fit_n",
	"resources.remote.cards_t4", "resources.remote.free_quota", "resources.remote.free_quota_now", "resources.remote.free_t4",
	"resources.remote.unknown_t4", "resources.remote.checked_at", "resources.remote.check_failed", "resources.remote.refresh", "resources.remote.locked",
	"resources.remote.lock.account_unknown", "resources.remote.lock.no_balance", "resources.remote.lock.ineligible",
	"resources.remote.machine_src", "resources.remote.machine_mismatch", "resources.remote.ram_free", "resources.remote.ram_low",
	"resources.remote.seg.ramdisk", "resources.remote.seg.whisper", "resources.remote.seg.system", "resources.remote.seg.data", "resources.remote.seg.used", "resources.remote.seg.free",
	"resources.remote.seg_tip.ramdisk", "resources.remote.seg_tip.whisper", "resources.remote.seg_tip.system", "resources.remote.seg_tip.data", "resources.remote.seg_tip.used", "resources.remote.seg_tip.free",
	"resources.remote.data_disk", "resources.remote.vram_used", "resources.remote.jobs_k", "resources.remote.job_split",
	"resources.remote.queue_k", "resources.remote.queue_ahead", "resources.remote.rate",
	"lecture.notice.split",
	"settings.colab.account_label", "settings.colab.account_hint", "settings.colab.account_fit", "settings.colab.account_unlocked",
	"settings.colab.accel_measured_hint", "settings.colab.acc_rate", "settings.colab.acc_unmeasured",
	"settings.colab.acc_fit_k", "settings.colab.acc_fit_free", "settings.colab.acc_locked",
	"settings.colab.accel_default_account", "settings.colab.locked_note", "onboarding.remote.account_unknown",
}

// Every text exists in zh-CN and en with a context entry; the user's
// wording of 14:50 ("免费额度约 X CU，下次补充 …"), only paid / free as the
// tier and no subscription texts.
func TestColabObsTexts(t *testing.T) {
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	for _, k := range colabObsKeys {
		if text(t, zh, k) == "" || text(t, en, k) == "" {
			t.Errorf("%s: missing text", k)
		}
		c, ok := ctx[k].(map[string]any)
		if !ok || c["where"] == nil || c["notes"] == nil {
			t.Errorf("%s: no context entry with where and notes: %v", k, ctx[k])
		}
	}
	if s := text(t, zh, "resources.remote.free_quota"); !strings.Contains(s, "免费额度约 {cu} CU") || !strings.Contains(s, "下次补充 {time}") {
		t.Errorf("free quota: %s", s)
	}
	if text(t, zh, "resources.remote.tier.paid") != "付费" || !strings.HasPrefix(text(t, zh, "resources.remote.tier.free"), "免费") {
		t.Errorf("tier texts: %s / %s", text(t, zh, "resources.remote.tier.paid"), text(t, zh, "resources.remote.tier.free"))
	}
	for k, v := range zh {
		if s, ok := v.(string); ok && (strings.HasPrefix(k, "resources.remote.") || strings.HasPrefix(k, "settings.colab.account")) && strings.Contains(s, "订阅") {
			t.Errorf("%s mentions a subscription (not shown, user decision 14:50): %s", k, s)
		}
	}
	// the texts no longer used are marked, not deleted
	for _, k := range []string{"resources.remote.current_k", "resources.remote.plan_k", "settings.colab.accel_hint", "settings.colab.accel_default", "settings.colab.accel_info_title",
		"settings.colab.tier_label", "settings.colab.tier.paid", "settings.colab.tier.free", "settings.colab.tier_paid_hint"} {
		c, _ := ctx[k].(map[string]any)
		if n, _ := c["notes"].(string); !strings.HasPrefix(n, "已废弃") {
			t.Errorf("%s: not marked as no longer used: %v", k, c)
		}
	}
}

// The settings page: the account row with "重新查询" (POST
// /api/colab/account/refresh), the accelerators as radios with the locked
// ones disabled and the reason in their row, no paid / free switch.
func TestColabObsSettingsWiring(t *testing.T) {
	src := readWeb(t, "settings_colab.js")
	for _, want := range []string{
		"function accountRow()", "authed ? accountRow() : null", "'/api/colab/account/refresh'", "fkey: 'set:colab:recheck'",
		"role: 'radiogroup'", "type: 'radio'", "disabled: !!lock || !!saving", "lockLine(lock)", "const s = lockText(reason);", "fkey: `set:colab:accel:${x}`",
		"settings.colab.locked_note", "s.accelerator_chosen && s.accelerator !== eff", "effectiveAccel(view.status)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("settings_colab.js lacks %q", want)
		}
	}
	for _, gone := range []string{"set:colab:tier", "tierRow()", "'settings.colab.tier_label'", "colab-accel-list", ".subscription"} {
		if strings.Contains(src, gone) {
			t.Errorf("settings_colab.js still has %q", gone)
		}
	}
}

// The resource panel: the memory bar built once and updated in place, the
// segments' width from their share (CSS keeps 3 px), the hover card; the
// rail's Colab cell; the lecture page's speed-mode notice.
func TestColabObsPanelWiring(t *testing.T) {
	res := readWeb(t, "resources.js")
	for _, want := range []string{
		"function ramMeter()", "C.ramSegments(c.usage)", "ref.ram.update(lay, c)", "e.style.flexBasis = `${(s.frac * 100).toFixed(3)}%`",
		"'mouseenter'", "'mouseleave'", "class: 'rc-rtip'", "u.cpu_history", "u.gpu_history", "C.railColab(cardOf())", "rail-colab",
		"t('resources.remote.machine_src'", "C.gpuMismatch(m)", "t('resources.remote.queue_ahead'", "fkey: 'insp:remote:recheck'",
	} {
		if !strings.Contains(res, want) {
			t.Errorf("resources.js lacks %q", want)
		}
	}
	if strings.Contains(res, "meter(frac, frac != null && frac > 0.9") {
		t.Error("resources.js: the usage window is still a meter (one line of text now)")
	}
	lec := readWeb(t, "view_lecture.js")
	for _, want := range []string{"import { splitParts } from './settings_colab.js'", "splitParts(lec)", "t('lecture.notice.split', { n: U.count(parts) })", "t('settings.colab.fast_hint')"} {
		if !strings.Contains(lec, want) {
			t.Errorf("view_lecture.js lacks %q", want)
		}
	}
	css := readWeb(t, "settings_v3.css")
	rule := func(sel string) string {
		m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(sel) + ` \{([^}]*)\}`).FindStringSubmatch(css)
		if m == nil {
			t.Errorf("settings_v3.css has no rule %s", sel)
			return ""
		}
		return m[1]
	}
	if r := rule(".rc-seg"); !strings.Contains(r, "min-width: 3px") || !strings.Contains(r, "flex: 0 1 auto") {
		t.Errorf(".rc-seg keeps 3 px and its share: %s", r)
	}
	if r := rule(".rc-seg.seg-free"); !strings.Contains(r, "flex: 1 1 0") || !strings.Contains(r, "min-width: 0") {
		t.Errorf("the free part fills the rest: %s", r)
	}
	if r := rule(".rc-rbar"); !strings.Contains(r, "gap: 2px") {
		t.Errorf("2 px between segments: %s", r)
	}
	// the draft's validated colours, light and dark
	for _, tok := range []string{"--ram-disk: light-dark(#1baf7a, #199e70)", "--ram-asr: light-dark(#1f6feb, #3b8cff)", "--ram-sys: light-dark(#8a919d, #6b7380)", "--ram-data: light-dark(#eb6834, #d95926)"} {
		if !strings.Contains(css, tok) {
			t.Errorf("settings_v3.css lacks %s", tok)
		}
	}
	// .insp-note is a flex box: a hidden one needs its own rule
	if r := rule(".rc-ram > .insp-note[hidden]"); !strings.Contains(r, "display: none") {
		t.Errorf("the low-memory note hides: %s", r)
	}
	for _, sel := range []string{".seg-ramdisk", ".seg-whisper", ".seg-system, .seg-used", ".seg-data", ".rail-mini", ".acc.is-locked .acc-radio", ".lec-notice.is-split"} {
		rule(sel)
	}
}

// The logic of the Colab area (testdata/colab_obs_test.mjs): memory
// segments and their shares, locks and reasons, lectures covered, the rail
// cell, speed mode, the card. Needs Node.js (as the desktop and extension
// builds do); skipped with a note where it is not installed.
func TestColabObsLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/colab_obs_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "colab_obs_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "colab-obs checks: ok") {
		t.Fatalf("colab_obs_test.mjs: %v\n%s", err, out)
	}
}

// The onboarding's remote step (lead's follow-up to colab-obs-web): after
// signing in it shows the account — the tier (paid / free) and the GPUs it
// may borrow, or "only the T4 for now, checked again by itself" with
// "重新查询" — and no longer lets the user pick a tier (PUT tier is ignored
// by the back end); the account read later reloads the view.
func TestColabObsOnboarding(t *testing.T) {
	src := readWeb(t, "onboarding_remote.js")
	for _, want := range []string{
		"import * as C from './settings_colab.js'", "function accountBlock()", "accountBlock()]", "C.accountKnown(a)", "C.levelText(st)",
		"st.selectable_accelerators", "t('onboarding.remote.account_unknown')", "fkey: 'ob:remote:recheck'", "C.refreshAccount()",
		"ev.kind === 'colab'", "t('settings.colab.account_label')",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("onboarding_remote.js lacks %q", want)
		}
	}
	for _, gone := range []string{"setTier", "tierBlock", "ob:remote:tier", "colabSettings({ tier", "'settings.colab.tier_label'", "settings.colab.tier.${", "TIERS"} {
		if strings.Contains(src, gone) {
			t.Errorf("onboarding_remote.js still has %q (the tier follows the account)", gone)
		}
	}
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	if s := text(t, zh, "onboarding.remote.account_unknown"); !strings.Contains(s, "只能使用 T4") || !strings.Contains(s, "稍后会自动再查") {
		t.Errorf("onboarding.remote.account_unknown: %s", s)
	}
}
