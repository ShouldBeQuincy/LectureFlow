// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// v3.2.x (.claude/v32x-design.md 3.4): the resource panel's Colab card shows
// a lecture retrying in place, a runtime held but not answering (paid:
// still billed; free: still uses the window — a hint only), the release
// under way without the lectures handed back, no error of an earlier
// session, and a runtime not released that is retried. The logic runs in
// Node (testdata/colab_card_test.mjs); these tests check the texts and the
// wiring.

var colabCardKeys = []string{
	"resources.remote.reconnecting", "resources.remote.reconnect_next", "resources.remote.still_billing", "resources.remote.still_billing_plain",
	"resources.remote.still_holding", "resources.remote.still_billing_releasing", "resources.remote.releasing_note",
	"resources.remote.leftover_retrying", "resources.remote.leftover_retrying_any", "resources.remote.leftover_session_any",
}

func TestColabCardTexts(t *testing.T) {
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	for _, k := range colabCardKeys {
		if text(t, zh, k) == "" || text(t, en, k) == "" {
			t.Errorf("%s: missing text", k)
		}
		c, ok := ctx[k].(map[string]any)
		if !ok || c["where"] == nil || c["notes"] == nil {
			t.Errorf("%s: no context entry with where and notes: %v", k, ctx[k])
		}
		if s := text(t, zh, k); strings.ContainsAny(s, "!！") {
			t.Errorf("%s: no exclamation marks: %s", k, s)
		}
	}
	// the user's wording (design 3.4 point 5)
	for k, want := range map[string][]string{
		"resources.remote.reconnecting":      {"连接不稳定，正在重试（{attempt}/{of}）"},
		"resources.remote.still_billing":     {"运行时连接不上，但仍在计费", "本次已用 {cu} CU", "约 {rate} CU/小时", "“立即关机”"},
		"resources.remote.still_holding":     {"运行时连接不上，但仍在占用本次使用窗口"},
		"resources.remote.releasing_note":    {"正在释放运行时"},
		"resources.remote.leftover_retrying": {"{accelerator}", "继续"},
	} {
		s := text(t, zh, k)
		for _, w := range want {
			if !strings.Contains(s, w) {
				t.Errorf("%s: %q lacks %q", k, s, w)
			}
		}
	}
	if s, now := text(t, zh, "resources.remote.still_billing"), text(t, zh, "resources.remote.now"); !strings.Contains(s, "“"+now+"”") {
		t.Errorf("still_billing quotes the button %q: %s", now, s)
	}
	if s, now := text(t, en, "resources.remote.still_billing"), text(t, en, "resources.remote.now"); !strings.Contains(s, "“"+now+"”") {
		t.Errorf("en still_billing quotes the button %q: %s", now, s)
	}
}

func TestColabCardWiring(t *testing.T) {
	res := readWeb(t, "resources.js")
	for _, want := range []string{
		"C.reconnectText(j.reconnect)", "rc-reconnect", "C.runtimeWarning(r, jobs)", "C.leftoverText(r.leftover)", "C.visibleError(r)",
		"t('resources.remote.releasing_note')", "remoteNotes(r, c.jobs)",
	} {
		if !strings.Contains(res, want) {
			t.Errorf("resources.js lacks %q", want)
		}
	}
	// the last error goes through visibleError only (no stale error of an earlier session)
	if strings.Contains(res, "C.infoText(r.last_error)") {
		t.Error("resources.js shows r.last_error without visibleError")
	}
	// "立即关机" stays enabled while the runtime does not answer: it is disabled only while busy or stopping
	if !strings.Contains(res, "disabled: !!remoteBusy || stopping, dataset: { fkey: 'insp:remote:now' }") {
		t.Error("the shutdown-now button's enabled state changed")
	}
	css := readWeb(t, "settings_v3.css")
	if !strings.Contains(css, ".rc-sub.rc-reconnect {") {
		t.Error("settings_v3.css has no .rc-sub.rc-reconnect rule")
	}
	src := readWeb(t, "settings_colab.js")
	for _, want := range []string{"export function reconnectText(", "export function runtimeWarning(", "export function visibleError(", "export function leftoverText(", "reconnect: reconnectOf(j)", "if (s.state === 'stopping') jobs = [];"} {
		if !strings.Contains(src, want) {
			t.Errorf("settings_colab.js lacks %q", want)
		}
	}
}

func TestColabCardLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/colab_card_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "colab_card_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "colab-card checks: ok") {
		t.Fatalf("colab_card_test.mjs: %v\n%s", err, out)
	}
}
