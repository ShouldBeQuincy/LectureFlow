// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"strings"
	"testing"
)

// Round 2 (W3): small fixes of the settings page.

// T6 #9: an invalid sentences-per-request value gets no "Set to N" chip, and
// the error of the last save goes as soon as the value is valid, in place.
func TestBatchChipInvalidValue(t *testing.T) {
	src := readWeb(t, "view_settings.js")
	for _, want := range []string{
		"const bad = setBatch !== '' && (!isInt(num(setBatch)) || num(setBatch) < 1 || num(setBatch) > MAX_BATCH);",
		"showValue: !bad && setBatch !== '' && (setBatch !== '1' || defBatch !== 1),",
		"if (inp && inp.getAttribute('aria-invalid') === 'true') {",
		"inp.removeAttribute('aria-invalid');",
		"const msg = row.querySelector('.rl-msg.is-err[role=\"alert\"]');",
		"row.classList.remove('is-error');",
		"rowErrors[r.role] = t('errors.settings.batch_invalid', { min: 1, max: MAX_BATCH });",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}
	// syncBatch runs on every edit of the translate row
	if !strings.Contains(src, "if (row && r.role === 'translate') syncBatch(row, r);") {
		t.Error("view_settings.js: syncBatch is not called on edits")
	}
}

// T6 #7: the provider's "Test connection" button keeps the keyboard focus
// while the test runs and after it: aria-disabled and aria-busy, not
// disabled (a disabled button cannot take the focus back after the
// re-render).
func TestProviderTestKeepsFocus(t *testing.T) {
	src := readWeb(t, "view_settings.js")
	for _, want := range []string{
		"const testBtn = btn(testing ? t('settings.provider.testing') : t('settings.provider.test'), { size: 'sm', icon: 'plug', onclick: () => testProvider(p), fkey: `set:prov:${p.id}:test` });",
		"testBtn.setAttribute('aria-disabled', 'true');",
		"testBtn.setAttribute('aria-busy', 'true');",
		"if (tests.get(p.id)?.state === 'running') return;",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}
	// "probe models" the same way (R28 A3)
	for _, want := range []string{
		"const probeBtn = btn(probingNow ? t('settings.provider.probing') : t('settings.provider.probe'), { size: 'sm', icon: 'scan-search', onclick: () => probe(p), fkey: `set:prov:${p.id}:probe` });",
		"probeBtn.setAttribute('aria-disabled', 'true');",
		"probeBtn.setAttribute('aria-busy', 'true');",
		"if (probing.has(p.id)) return;",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}
	if strings.Contains(src, "disabled: probing.has(p.id)") {
		t.Error("view_settings.js: the probe button is still disabled while it runs")
	}
	if strings.Contains(src, "disabled: test && test.state === 'running'") {
		t.Error("view_settings.js: the test button is still disabled while it runs (the focus falls to the body)")
	}
}
