// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round 2 (W3): small fixes of the settings page and its Colab card.

// T6 #11 and #12 (texts; the logic is in testdata/r2_settings_polish_test.mjs).
func TestColabNeedAuthLockText(t *testing.T) {
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	const k = "resources.remote.lock.need_auth"
	if s := text(t, zh, k); !strings.Contains(s, "授权") || strings.Contains(s, "没有读到账户") {
		t.Errorf("zh-CN %s: %s", k, s)
	}
	if s := text(t, en, k); !strings.Contains(s, "authorized") {
		t.Errorf("en %s: %s", k, s)
	}
	if c, ok := ctx[k].(map[string]any); !ok || c["where"] == nil || c["notes"] == nil {
		t.Errorf("context %s: %v", k, ctx[k])
	}
	src := readWeb(t, "settings_colab.js")
	for _, want := range []string{
		"if (!accountKnown(a)) return st.auth && !st.auth.present ? 'need_auth' : 'account_unknown';",
		"h('span', null, lockLine(lock))) : null,",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("settings_colab.js lacks %q", want)
		}
	}
}

func TestR2SettingsPolishLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r2_settings_polish_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r2_settings_polish_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r2 settings polish checks: ok") {
		t.Fatalf("r2_settings_polish_test.mjs: %v\n%s", err, out)
	}
}
