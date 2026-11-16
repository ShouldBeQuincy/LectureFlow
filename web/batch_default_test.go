// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// User 2026-11-11 (Claude models only after review R24): an empty
// sentences-per-request field means the default of the role's provider and
// model (10 for a Claude model on an Anthropic provider, 1 otherwise). The
// settings page keeps the number alone in the 72px field and says
// "Default N" next to it and, above 1, as a chip on the folded summary; the
// hint no longer says "empty means 1" and says how to go back to 1.
func TestSettingsBatchDefaultShown(t *testing.T) {
	if settings.DefaultAnthropicBatch != 10 {
		t.Fatalf("DefaultAnthropicBatch %d: update the hint texts", settings.DefaultAnthropicBatch)
	}
	src := readWeb(t, "view_settings.js")
	for _, want := range []string{
		"  defaultBatch,\n} from './provider_rules.js';",
		"const defBatch = defaultBatch(providerOf(v.provider_id), v.model);",
		// the field: the number only
		"placeholder: String(defBatch),",
		// "Default N" next to the field, and the chips (kept current by syncBatch)
		"h('span', { class: 'dim', id: hintId('batch_default'), dataset: fk('batch_default') }, t('settings.roles.batch_default', { n: defBatch })),", // W4: the field's description
		"h('span', { class: 'chip chip-quiet', hidden: !bv.showDefault, dataset: fk('batch_default_chip') }, t('settings.roles.batch_default', { n: defBatch })),",
		"h('span', { class: 'chip chip-quiet', hidden: !bv.showValue, dataset: fk('batch_value_chip') }, t('settings.roles.batch_value_chip', { value: bv.setBatch })),",
		"showDefault: setBatch === '' && defBatch > 1,",
		// round 2 (T6 #9): an invalid value gets no chip
		"showValue: !bad && setBatch !== '' && (setBatch !== '1' || defBatch !== 1),",
		// typing a model or a batch updates them without a re-render
		"if (row && r.role === 'translate') syncBatch(row, r);",
		"if (inp) inp.placeholder = String(b.defBatch);",
		"if (def) def.textContent = t('settings.roles.batch_default', { n: b.defBatch });",
		"dchip.hidden = !b.showDefault;",
		"vchip.hidden = !b.showValue;",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}
	for _, gone := range []string{
		"placeholder: '1',",
		"placeholder: t('settings.roles.batch_default'",
		"prov.default_batch)",
		"the default 1 (batch, translate only)",
	} {
		if strings.Contains(src, gone) {
			t.Errorf("view_settings.js still has %q", gone)
		}
	}

	// CSS: the field stays 72px (a number only), its line wraps instead of
	// pushing the row wider at 375px, and the hidden chips are hidden
	css := readWeb(t, "style.css")
	for _, want := range []string{
		".rl-batch-ctl { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 6px; min-width: 0; }",
		".rl-batch-ctl .rl-batch-in { width: 72px; flex: none; }",
		".rl-batch [hidden] { display: none !important; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
	if n := len(regexp.MustCompile(`\.rl-batch-in\s*\{`).FindAllString(css, -1)); n != 1 {
		t.Errorf("style.css styles .rl-batch-in %d times, want 1", n)
	}

	// the new short text has the wording of settings.general.idle_timeout_default
	for _, loc := range []string{"zh-CN", "en", "zh-TW", "yue-Hant-HK", "ru", "hi"} {
		c := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		got, _ := c["settings.roles.batch_default"].(string)
		if want, _ := c["settings.general.idle_timeout_default"].(string); got == "" || got != want || !strings.Contains(got, "{n}") {
			t.Errorf("%s: settings.roles.batch_default = %q, want %q", loc, got, want)
		}
	}
	if _, ok := loadCatalog(t, filepath.Join(catalogDir, "context.json"))["settings.roles.batch_default"]; !ok {
		t.Error("context.json: no settings.roles.batch_default")
	}

	// the hint names the default per provider and model, and how to get 1
	for loc, c := range map[string]struct {
		bad  string
		must []string
	}{
		"zh-CN":       {"留空为 1", []string{"留空为默认值：Anthropic 服务商上的 Claude 模型为 10，其余为 1", "要逐句翻译请填 1"}},
		"en":          {"Empty means 1", []string{"Empty means the default: 10 for Claude models on Anthropic providers, 1 for everything else", "to translate one sentence at a time, enter 1"}},
		"zh-TW":       {"留空為 1", []string{"Anthropic 供應商上的 Claude 模型為 10", "要逐句翻譯請填 1"}},
		"yue-Hant-HK": {"留空即係 1", []string{"Anthropic 供應商上面嘅 Claude 模型係 10", "想逐句翻譯就填 1"}},
		"ru":          {"Пусто — 1", []string{"10 для моделей Claude у провайдеров Anthropic", "укажите 1"}},
		"hi":          {"ख़ाली का मतलब 1", []string{"Anthropic प्रोवाइडर पर Claude मॉडल के लिए 10", "1 भरें"}},
	} {
		hint, _ := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))["settings.roles.batch_hint"].(string)
		if strings.Contains(hint, c.bad) {
			t.Errorf("%s: settings.roles.batch_hint still says %q", loc, c.bad)
		}
		for _, m := range c.must {
			if !strings.Contains(hint, m) {
				t.Errorf("%s: settings.roles.batch_hint lacks %q: %q", loc, m, hint)
			}
		}
	}
}

// The settings page applies the same rule as the snapshot: provider_rules.js
// defaultBatch(provider view, model) equals settings.DefaultBatch(provider,
// model) on every provider × model pair below.
func TestSettingsBatchDefaultMatchesGo(t *testing.T) {
	provs := []model.Provider{
		{Type: model.ProviderAnthropic, BaseURL: "https://api.anthropic.com"},
		{Type: model.ProviderAnthropic, BaseURL: "https://relay.example.test"},
		{Type: model.ProviderAnthropic, BaseURL: "https://api.deepseek.com/anthropic"},
		{Type: model.ProviderAnthropic, BaseURL: "https://relay.example.test", BalanceAPI: model.BalanceDeepSeek},
		{Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com", BalanceAPI: model.BalanceDeepSeek},
		{Type: model.ProviderOpenAI, BaseURL: "https://openrouter.ai/api/v1"},
	}
	models := []string{"claude-sonnet-5-5", "Claude-Opus-5-5", " claude-opus-5-5 ", "anthropic/claude-sonnet-5-5",
		"claude-sonnet-5-5@20261101", "CLAUDE", "kimi-k2", "glm-4.6", "MiniMax-M2", "deepseek-flash", "", "cla ude"}
	type tc struct {
		Claude int    `json:"claude"`
		Model  string `json:"model"`
		Want   int    `json:"want"`
	}
	var cases []tc
	saw10, saw1 := false, false
	for _, p := range provs {
		for _, m := range models {
			w := settings.DefaultBatch(p, m)
			saw10, saw1 = saw10 || w == settings.DefaultAnthropicBatch, saw1 || w == 1
			cases = append(cases, tc{settings.ClaudeBatch(p), m, w})
		}
	}
	if !saw10 || !saw1 {
		t.Fatalf("the cases do not cover both defaults")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/batch_default_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "batch_default_test.mjs", url, string(b))
	if err != nil || !strings.Contains(string(out), "batch default checks: ok") {
		t.Fatalf("batch_default_test.mjs: %v\n%s", err, out)
	}
}
