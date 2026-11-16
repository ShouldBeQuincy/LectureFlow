// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// SEC7 M1 / L1: the SSH server wizard's "主机密钥已变化" step and the box
// to tick when the user's known_hosts disagrees. The behaviour runs in Node
// (testdata/ssh_keychange_test.mjs); these tests check the texts and the
// styles.

var sshKeyChangeKeys = []string{
	"settings.remote.step.hostkey_changed", "settings.remote.hostkey.mismatch_confirm",
	"settings.remote.keychange.title", "settings.remote.keychange.warning", "settings.remote.keychange.previous",
	"settings.remote.keychange.presented", "settings.remote.keychange.check", "settings.remote.keychange.confirm",
	"settings.remote.keychange.replace", "errors.remote_server.host_key_pinned",
}

// Every text exists in zh-CN and en with a context entry; the warning names
// both causes; the mismatch error has the presented key's placeholder.
func TestSSHKeyChangeTexts(t *testing.T) {
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	for _, k := range sshKeyChangeKeys {
		if text(t, zh, k) == "" || text(t, en, k) == "" {
			t.Errorf("%s: missing text", k)
		}
		c, ok := ctx[k].(map[string]any)
		if !ok || c["where"] == nil || c["notes"] == nil {
			t.Errorf("%s: no context entry with where and notes: %v", k, ctx[k])
		}
	}
	w := text(t, zh, "settings.remote.keychange.warning")
	for _, s := range []string{"重装", "冒充"} {
		if !strings.Contains(w, s) {
			t.Errorf("the warning lacks %q: %s", s, w)
		}
	}
	for _, cat := range []map[string]any{zh, en} {
		if s := text(t, cat, "errors.remote.host_key_mismatch"); !strings.Contains(s, "{fingerprint}") || !strings.Contains(s, "{device}") {
			t.Errorf("errors.remote.host_key_mismatch: %s", s)
		}
	}
}

func TestSSHKeyChangeStyles(t *testing.T) {
	css := readWeb(t, "settings_v3.css")
	for _, sel := range []string{".rs-keychange {", ".rs-keychange:focus-visible {", ".rs-fpcmp {", ".rs-fpcmp-row.is-new {", ".rs-keycheck {"} {
		if !strings.Contains(css, sel) {
			t.Errorf("settings_v3.css lacks %s", sel)
		}
	}
	// narrow windows: the two fingerprints stack
	if !strings.Contains(css, "  .rs-fpcmp { grid-template-columns: minmax(0, 1fr); }") {
		t.Error("settings_v3.css: the fingerprints do not stack in a narrow window")
	}
}

// The behaviour (testdata/ssh_keychange_test.mjs). Needs Node.js; skipped
// with a note where it is not installed (as TestSSH1bLogic).
func TestSSHKeyChangeLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/ssh_keychange_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "ssh_keychange_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "ssh keychange checks: ok") {
		t.Fatalf("ssh_keychange_test.mjs: %v\n%s", err, out)
	}
}
