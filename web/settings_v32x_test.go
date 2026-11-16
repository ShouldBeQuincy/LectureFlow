// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// v3.2.x settings page (web-builder): a role's output limit (max_tokens,
// empty = automatic) and the translate role's folded, experimental
// sentences-per-request field; the note that claude-opus-5-5 runs "off" at
// effort low; the general switches auto_chapters and llm_idle_timeout_s sent
// as a merge (only the changed fields, null = default); a new Anthropic
// provider starts at 4 concurrent requests and no per-minute limit, other
// types at 4 / 60, following the type until the user edits a field.

func TestSettingsV32xRanges(t *testing.T) {
	src := readWeb(t, "provider_rules.js")
	if got := jsConst(t, src, `export const MAX_TOKENS_LIMIT = (\d+);`); got != settings.MaxTokensLimit {
		t.Errorf("MAX_TOKENS_LIMIT %d, settings %d", got, settings.MaxTokensLimit)
	}
	if got := jsConst(t, src, `export const MAX_BATCH = (\d+);`); got != settings.MaxBatch {
		t.Errorf("MAX_BATCH %d, settings %d", got, settings.MaxBatch)
	}
	if got := jsConst(t, src, `export const DEFAULT_IDLE_TIMEOUT_S = (\d+);`); got != model.DefaultLLMIdleTimeoutS {
		t.Errorf("DEFAULT_IDLE_TIMEOUT_S %d, model %d", got, model.DefaultLLMIdleTimeoutS)
	}
	// the idle timeout's bounds are the settings check's (0 = off, else
	// MIN..MAX; SEC5 N4)
	if got := jsConst(t, src, `export const MAX_IDLE_TIMEOUT_S = (\d+);`); got != 3600 || settings.MaxLLMIdleTimeoutS != 3600 {
		t.Errorf("MAX_IDLE_TIMEOUT_S %d, want 3600 = settings %d", got, settings.MaxLLMIdleTimeoutS)
	}
	if got := jsConst(t, src, `export const MIN_IDLE_TIMEOUT_S = (\d+);`); got != settings.MinLLMIdleTimeoutS {
		t.Errorf("MIN_IDLE_TIMEOUT_S %d, settings %d", got, settings.MinLLMIdleTimeoutS)
	}
	for _, want := range []string{
		"ANTHROPIC_LIMITS = Object.freeze({ max_concurrency: 4, rpm: 0 })",
		"DEFAULT_LIMITS = Object.freeze({ max_concurrency: 4, rpm: 60 })",
		"return type === 'anthropic' ? ANTHROPIC_LIMITS : DEFAULT_LIMITS;",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("provider_rules.js lacks %q", want)
		}
	}
}

func TestSettingsV32xWiring(t *testing.T) {
	src := readWeb(t, "view_settings.js")
	for _, want := range []string{
		// provider dialog: prefill by type, follow the type until edited
		"const pre = limitsPrefill(type);",
		"value: String(p ? p.max_concurrency : pre.max_concurrency)",
		"value: String(p ? p.rpm : pre.rpm)",
		"const limitsTouched = { conc: !isNew, rpm: !isNew };",
		"if (!limitsTouched.conc) conc.value = String(lim.max_concurrency);",
		"if (!limitsTouched.rpm) rpm.value = String(lim.rpm);",
		"limitsNote.hidden = type !== 'anthropic';",
		"openExternal(ANTHROPIC_RATE_LIMITS_URL)",
		// roles: max_tokens and batch, left out when empty (= cleared)
		"max_tokens: r.max_tokens != null ? String(r.max_tokens) : ''",
		"batch: r.batch != null ? String(r.batch) : ''",
		"sameOpt(a.max_tokens, b.max_tokens) &&",
		"if (String(d.max_tokens || '').trim() !== '') params.max_tokens = num(d.max_tokens);",
		"if (r.role === 'translate' && String(d.batch || '').trim() !== '') params.batch = num(d.batch);",
		"if (e.code === 'settings.max_tokens_invalid') rowErrorFields[r.role] = 'max_tokens';",
		// batch size is live: the "experimental" chip and the hint stay
		"h('span', { class: 'chip chip-quiet' }, t('settings.roles.batch_experimental'))",
		"h('span', { class: 'rl-sub', id: hintId('batch') }, t('settings.roles.batch_hint'))", // W4: the field's description
		"thinkVal === 'off' && thinkingFloorsAtLow(v.model)",
		// general: merge semantics
		"auto_chapters: g.auto_chapters !== false",
		"if (g.auto_chapters !== was.auto_chapters) body.auto_chapters = g.auto_chapters;",
		"body.llm_idle_timeout_s = idle === '' ? null : num(idle);",
		"const s = await api.setGeneral(body);",
		"if (e.code === 'settings.idle_timeout_invalid') {",
		// idle timeout: 0 (off) or MIN..MAX, the hint and error name the bounds
		"(num(idle) !== 0 && (num(idle) < MIN_IDLE_TIMEOUT_S || num(idle) > MAX_IDLE_TIMEOUT_S))",
		"t('errors.settings.idle_timeout_invalid', { min: MIN_IDLE_TIMEOUT_S, max: MAX_IDLE_TIMEOUT_S })",
		"t('settings.general.idle_timeout_hint', { n: DEFAULT_IDLE_TIMEOUT_S, min: MIN_IDLE_TIMEOUT_S, max: MAX_IDLE_TIMEOUT_S })",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}
	for _, gone := range []string{
		"value: String(p ? p.rpm : 60)",
		"api.setGeneral({ keep_original_media: g.keep_original_media, notifications: g.notifications, daily_budget: b })",
		// the batch size now works: no "not in effect yet" warning
		"settings.roles.batch_pending",
	} {
		if strings.Contains(src, gone) {
			t.Errorf("view_settings.js still has %q", gone)
		}
	}

	keys := []string{
		"settings.provider.anthropic_limits_note", "settings.provider.anthropic_limits_link",
		"settings.roles.max_tokens", "settings.roles.max_tokens_label", "settings.roles.max_tokens_auto", "settings.roles.max_tokens_hint",
		"settings.roles.batch_summary", "settings.roles.batch_experimental", "settings.roles.batch_value_chip", "settings.roles.batch_label",
		"settings.roles.batch_hint", "settings.roles.thinking_floor_short", "settings.roles.thinking_floor_note",
		"settings.general.auto_chapters", "settings.general.auto_chapters_hint", "settings.general.auto_chapters_no_role",
		"settings.general.idle_timeout", "settings.general.idle_timeout_unit", "settings.general.idle_timeout_default",
		"settings.general.idle_timeout_off", "settings.general.idle_timeout_hint", "errors.settings.idle_timeout_invalid",
	}
	for _, loc := range []string{"zh-CN", "en", "zh-TW", "yue-Hant-HK", "ru", "hi", "context"} {
		c := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		for _, k := range keys {
			if _, ok := c[k]; !ok {
				t.Errorf("%s: no %s", loc, k)
			}
		}
	}
	// the batch warning is gone from every catalog, and the chip only says
	// "experimental" (no "not in effect yet")
	for _, loc := range []string{"zh-CN", "en", "zh-TW", "yue-Hant-HK", "ru", "hi", "context"} {
		c := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		if _, ok := c["settings.roles.batch_pending"]; ok {
			t.Errorf("%s: still has settings.roles.batch_pending", loc)
		}
	}
	for loc, want := range map[string]string{"zh-CN": "实验性", "en": "Experimental"} {
		c := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		if got, _ := c["settings.roles.batch_experimental"].(string); got != want {
			t.Errorf("%s: settings.roles.batch_experimental = %q, want %q", loc, got, want)
		}
		hint, _ := c["settings.roles.batch_hint"].(string)
		for _, bad := range []string{"尚未生效", "not in effect", "still being built", "开发中",
			// review R11: only some failures split a batch, and 150 tokens
			// per sentence is too little for long sentences
			"某一批失败时会自动拆小重试", "If a batch fails", "约 150 token”", "about 150 tokens per sentence"} {
			if strings.Contains(hint, bad) {
				t.Errorf("%s: settings.roles.batch_hint still says %q: %q", loc, bad, hint)
			}
		}
		for _, must := range []string{"150–300", map[string]string{"zh-CN": "超时", "en": "times out"}[loc],
			map[string]string{"zh-CN": "网络或服务商出错", "en": "network or provider errors"}[loc],
			// review R18: an idle stream timeout does not split a batch
			map[string]string{"zh-CN": "空闲超时", "en": "idle timeout"}[loc]} {
			if !strings.Contains(hint, must) {
				t.Errorf("%s: settings.roles.batch_hint does not say %q: %q", loc, must, hint)
			}
		}
	}

	// the Anthropic note names no tier numbers (the account decides)
	for _, loc := range []string{"zh-CN", "en"} {
		c := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		note, _ := c["settings.provider.anthropic_limits_note"].(string)
		if strings.ContainsAny(strings.ReplaceAll(note, "429", ""), "0123456789") {
			t.Errorf("%s: settings.provider.anthropic_limits_note has a number: %q", loc, note)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/settings_v32x_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "settings_v32x_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "settings v3.2.x checks: ok") {
		t.Fatalf("settings_v32x_test.mjs: %v\n%s", err, out)
	}
}
