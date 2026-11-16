// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// N35: a saved key that cannot be decrypted is bound to its provider's
// address; one changed by someone else also makes it unreadable, and
// re-entering the key would hand it to that address. Each text that asks
// to re-enter it first asks to make sure the address is the one the user
// set up and to correct it if not (wording of review R28). The back end's
// own zh-CN texts say the same, and the provider dialog shows the text
// under the key field (not only as a tooltip, R28 S3).
func TestKeyUnreadableAsksToCheckAddress(t *testing.T) {
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	for _, k := range []string{
		"errors.settings.provider_key_unreadable",
		"errors.check.credentials_unreadable",
		"errors.snapshot.unusable.api_key_unreadable",
	} {
		if s := text(t, zh, k); !strings.Contains(s, "请先确认这个提供商的地址是你设置的；如果不是，先把地址改回正确的再输入") {
			t.Errorf("zh-CN %s: %s", k, s)
		}
		if s := text(t, en, k); !strings.Contains(s, "make sure the provider’s address is the one you set up; if it isn’t, correct the address first") {
			t.Errorf("en %s: %s", k, s)
		}
		c, _ := ctx[k].(map[string]any)
		if n, _ := c["notes"].(string); !strings.Contains(n, "N35") {
			t.Errorf("context %s: notes do not explain the added sentence: %v", k, c)
		}
	}
	// plural forms: each one asks
	if m, ok := en["errors.check.credentials_unreadable"].(map[string]any); ok {
		for f, v := range m {
			if s, _ := v.(string); !strings.Contains(s, "correct the address first") {
				t.Errorf("en errors.check.credentials_unreadable[%s]: %s", f, s)
			}
		}
	}
	// the provider row's API key fact uses the text as its explanation, and
	// the provider dialog shows it under the key field, described by it
	// (W4, R28: with the other hints, all outside the field's label, so
	// each is heard once, as the description, and not in the name too)
	src := readWeb(t, "view_settings.js")
	for _, want := range []string{
		"fact(t('settings.provider.key'), h('span', { class: 'tone-warn' }, t('settings.provider.key_unreadable')), t('errors.settings.provider_key_unreadable', { provider: p.name }))",
		"const unreadHint = p && p.key_unreadable ? h('span', { class: 'dlg-hint tone-warn', id: 'pv-key-unreadable' }, icon('key-round', { size: 12 }), ' ', t('errors.settings.provider_key_unreadable', { provider: p.name })) : null;",
		"key.setAttribute('aria-describedby', [unreadHint, keyHint, movedHint, storeHint].filter(Boolean).map((x) => x.getAttribute('id')).join(' '));",
		"h('div', { class: 'dlg-field span-2' }, h('label', { class: 'dlg-label', for: 'pv-key' }, icon('key-round', { size: 13 }), ' ', t('settings.provider.key')), key, unreadHint, keyHint, movedHint, storeHint),",
		"key.setAttribute('id', 'pv-key');",
		"const keyHint = h('span', { class: 'dlg-hint', id: 'pv-key-hint' });",
		"const movedHint = h('span', { class: 'dlg-hint tone-warn', role: 'status', id: 'pv-key-moved' });",
		"h('span', { class: 'dlg-hint tone-warn', role: 'status', id: 'pv-key-store' }",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}

	// the back end's own texts (details, logs, fallbacks) match zh-CN
	var reg map[string]struct {
		Zh string `json:"zh"`
	}
	b, err := os.ReadFile(filepath.Join("..", "testdata", "api", "error_codes_backend.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &reg); err != nil {
		t.Fatal(err)
	}
	for code, key := range map[string]string{
		"settings.provider_key_unreadable":     "errors.settings.provider_key_unreadable",
		"snapshot.unusable.api_key_unreadable": "errors.snapshot.unusable.api_key_unreadable",
	} {
		if got, want := reg[code].Zh, text(t, zh, key); got != want {
			t.Errorf("error_codes_backend.json %s:\n got %s\nwant %s", code, got, want)
		}
	}
	prov := strings.ReplaceAll(text(t, zh, "errors.settings.provider_key_unreadable"), "{provider}", "%s")
	snap := strings.ReplaceAll(text(t, zh, "errors.snapshot.unusable.api_key_unreadable"), "{provider}", "%s")
	for name, want := range map[string]string{
		"internal/settings/keys.go":          prov,
		"internal/settings/settings.go":      snap,
		"internal/settings/manual.go":        snap,
		"internal/api/system_credentials.go": text(t, zh, "errors.check.credentials_unreadable"),
	} {
		b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"`+want+`"`) {
			t.Errorf("%s lacks the zh-CN text %q", name, want)
		}
	}
}
