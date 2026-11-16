// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ssh-1b: the SSH servers in the UI — Settings → 远程服务器 (the wizard,
// the server cards), the device list's SSH rows, the lecture menu, the
// resource panel's cards, the diagnostics' groups, the waiting texts. The
// logic runs in Node (testdata/ssh_1b_test.mjs); these tests check the
// texts and how the pieces are wired, and that nothing shows while the
// back end has the servers off.

var ssh1bKeys = []string{
	"settings.remote.title", "settings.remote.privacy_title", "settings.remote.privacy", "settings.remote.add", "settings.remote.empty",
	"settings.remote.step.address", "settings.remote.step.authorize", "settings.remote.step.hostkey", "settings.remote.step.detect",
	"settings.remote.step.install", "settings.remote.step.test", "settings.remote.step.use",
	"settings.remote.host", "settings.remote.host_hint", "settings.remote.port", "settings.remote.username",
	"settings.remote.auth_intro", "settings.remote.os_hint.linux", "settings.remote.os_hint.mac", "settings.remote.os_hint.windows",
	"settings.remote.copy", "settings.remote.copied", "settings.remote.auth_done", "settings.remote.hostkey_intro",
	"settings.remote.hostkey.match", "settings.remote.hostkey.mismatch", "settings.remote.hostkey_trust", "settings.remote.detect_intro",
	"settings.remote.fact.gpu", "settings.remote.fact.disk", "settings.remote.install_size", "settings.remote.install_start",
	"settings.remote.install_background", "settings.remote.install_env", "settings.remote.install_model", "settings.remote.install_done",
	"settings.remote.test_ok", "settings.remote.group.prefer", "settings.remote.group.prefer_hint", "settings.remote.group.supplement",
	"settings.remote.group.supplement_hint", "settings.remote.continue", "settings.remote.update_agent", "settings.remote.agent",
	"settings.remote.remove_title", "settings.remote.remove_message", "settings.remote.remove_uninstall", "settings.remote.edit_title",
	"settings.remote.edit_hint",
	"remote.state.offline", "remote.state.idle", "remote.state.connecting", "remote.state.untrusted", "remote.state.host_key_changed",
	"remote.state.auth_failed", "remote.state.unsupported", "remote.state.setup", "remote.state.installing", "remote.state.ready",
	"remote.state.busy", "remote.state.paused", "remote.state.error",
	"devices.kind.ssh", "devices.ssh_note", "menu.lecture.use_remote", "lecture.use_remote.message", "lecture.use_remote.done",
	"resources.ssh.latency", "system.remote.connection", "system.remote.agent", "system.remote.python", "system.remote.gpu", "system.remote.agent_old",
	// ssh-mvp's texts the UI now uses
	"waiting.remote_offline", "waiting.remote_setup", "waiting.remote_fallback_ssh",
}

// Every text exists in zh-CN and en with a context entry; the states the
// back end sends (sshremote.State*) each have a name; the privacy note
// says what is uploaded and when it is deleted.
func TestSSH1bTexts(t *testing.T) {
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	for _, k := range ssh1bKeys {
		if text(t, zh, k) == "" || text(t, en, k) == "" {
			t.Errorf("%s: missing text", k)
		}
		c, ok := ctx[k].(map[string]any)
		if !ok || c["where"] == nil || c["notes"] == nil {
			t.Errorf("%s: no context entry with where and notes: %v", k, ctx[k])
		}
	}
	p := text(t, zh, "settings.remote.privacy")
	for _, w := range []string{"SSH", "立即删除", "2 小时"} {
		if !strings.Contains(p, w) {
			t.Errorf("privacy note lacks %q: %s", w, p)
		}
	}
	if s := text(t, zh, "menu.lecture.use_remote"); !strings.Contains(s, "{name}") {
		t.Errorf("menu.lecture.use_remote: %s", s)
	}
}

// The settings page: the section only while the back end has the servers
// (404 = off: no section, no TOC entry); the wizard's seven steps, the
// endpoints it calls, the server cards' actions.
func TestSSH1bSettingsWiring(t *testing.T) {
	src := readWeb(t, "settings_remote.js")
	for _, want := range []string{
		"e.status === 404", "setServers(false, [])", "export function sshEnabled()",
		"const STEPS = ['address', 'authorize', 'hostkey', 'detect', 'install', 'test', 'use']",
		"'/api/remote/servers'", "serverPath(w.id, '/pubkey')", "serverPath(w.id, '/connect')", "serverPath(w.id, '/trust')",
		"serverPath(w.id, '/install')", "serverPath(w.id, '/test')", "{ group: w.group || 'supplement' }",
		"api.patchDevice(v.device_id, patch)", "p.command_windows_admin", "k.known_hosts === 'match'", "v.kit_size",
		"uninstall.checked ? '?uninstall=1' : ''", "fkey: `set:remote:${v.id}:continue`", "fkey: `set:remote:${v.id}:update`",
		"fkey: `set:remote:${v.id}:pause`", "`/api/lectures/${enc(lec.id)}/use-remote`", "{ device: server.device_id }",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("settings_remote.js lacks %q", want)
		}
	}
	vs := readWeb(t, "view_settings.js")
	for _, want := range []string{
		"['remote', 'plug', 'settings.remote.title']", "const shownBlock = (b) => b[0] !== 'remote' || sshEnabled();",
		"BLOCKS.filter(shownBlock)", "createRemoteSection(sectionUI('remote'))", "remoteUI.destroy()", "remoteUnsub()",
	} {
		if !strings.Contains(vs, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}
	if !strings.Contains(src, "shell.hidden = !sshEnabled();") {
		t.Error("the section does not hide itself while the SSH servers are off")
	}
	app := readWeb(t, "app.js")
	for _, want := range []string{"import { loadServers } from './settings_remote.js'", "debounce('servers', loadServers, 300)"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// The device list, the lecture menu, the resource panel, the diagnostics
// and the waiting texts.
func TestSSH1bOtherWiring(t *testing.T) {
	for file, wants := range map[string][]string{
		"asr_devices.js": {"isSSHDevice(d.id)", "sshFields(d, isBusy)", "fields(d, isBusy, false)", "href: '#/settings/remote'", "t('devices.ssh_note')",
			"stateChip(server.state)", "!isSSHDevice(d.id)"},
		"actions.js":     {"import { useRemoteMenuItem } from './settings_remote.js'", "useRemoteMenuItem(lec, done)"},
		"resources.js":   {"import * as R from './settings_remote.js'", "secs.push(sshArea());", "updateSSH();", "R.sshCards(res)", "R.cardConnected(c)", "r.ram.update(lay, c)", "x.latency_ms", "x.agent_version", "x.storage === 'memory'"},
		"view_system.js": {"remoteCard()", "diagRows(v)", "`/api/remote/servers/${encodeURIComponent(v.id)}/test`", "serversUnsub()"},
		"util.js":        {"w === 'remote_offline'", "w === 'remote_setup'", "w === 'remote_fallback_ssh'"},
	} {
		src := readWeb(t, file)
		for _, want := range wants {
			if !strings.Contains(src, want) {
				t.Errorf("%s lacks %q", file, want)
			}
		}
	}
	css := readWeb(t, "settings_v3.css")
	for _, sel := range []string{".rs-card {", ".rs-wiz {", ".rs-steps {", ".rs-step.is-current {", ".rs-fields-row {", ".rs-choice.is-sel {", ".insp-ssh .ssh-line {", ".dev-ssh {", ".sys-remote-test {"} {
		if !strings.Contains(css, sel) {
			t.Errorf("settings_v3.css lacks %s", sel)
		}
	}
	// narrow windows: the address and port stack, the wizard's buttons fill the row
	if !strings.Contains(css, "@media (max-width: 560px) {\n  .rs-fields-row { grid-template-columns: minmax(0, 1fr); }") {
		t.Error("settings_v3.css: the wizard's fields do not stack in a narrow window")
	}
}

// The logic (testdata/ssh_1b_test.mjs). Needs Node.js; skipped with a note
// where it is not installed (as TestColabObsLogic).
func TestSSH1bLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/ssh_1b_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "ssh_1b_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "ssh-1b checks: ok") {
		t.Fatalf("ssh_1b_test.mjs: %v\n%s", err, out)
	}
}
