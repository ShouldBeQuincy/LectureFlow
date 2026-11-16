// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// r7 (T24 G4, T23 general 9, 10): an unreadable model answer says whose
// problem it usually is and what to do; the stage state "stale" has one
// name (the course list's legend and the sentences that quote it say what
// the stage segments say, glossary: Input changed / 输入已变化); en spells
// canceled the American way (glossary: lead ruling).
func TestR7StateNamesAndModelErrors(t *testing.T) {
	cat := func(loc string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, filepath.Join("i18n", loc+".json"))), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	zh, en := cat("zh-CN"), cat("en")
	for loc, c := range map[string]map[string]any{"zh-CN": zh, "en": en} {
		stale, _ := c["common.status.stale"].(string)
		if legend, _ := c["course.lectures.legend.stale"].(string); legend != stale {
			t.Errorf("%s: the legend calls the stale state %q, the stage segments %q", loc, legend, stale)
		}
		for _, k := range []string{"glossary.note", "glossary.delete.message", "course.materials.delete_message"} {
			if s, _ := c[k].(string); !strings.Contains(s, "“"+stale+"”") {
				t.Errorf("%s %s: quotes the stages' state as they show it (%q): %q", loc, k, stale, s)
			}
		}
		retry, _ := c["common.action.retry_model"].(string)
		roles, _ := c["settings.block.roles"].(string)
		for _, k := range []string{"errors.llm.bad_response", "errors.stage.output_unusable"} {
			if s, _ := c[k].(string); !strings.Contains(s, "“"+retry+"”") {
				t.Errorf("%s %s: names %q as the way out: %q", loc, k, retry, s)
			}
			// R75 L4: that button is only on a lecture's three stages; the
			// other places change the model under Settings → Roles
			if s, _ := c[k].(string); !strings.Contains(s, "“"+roles+"”") {
				t.Errorf("%s %s: names %q for where there is no such button: %q", loc, k, roles, s)
			}
		}
	}
	for k, v := range en {
		if s, ok := v.(string); ok && strings.Contains(strings.ToLower(s), "cancelled") {
			t.Errorf("en %s: “cancelled” (glossary: canceled): %q", k, s)
		}
		// a quoted button in body text is selected, not clicked (glossary;
		// “Click to …” stays in tooltips, and pointer gestures such as
		// right-click or clicking the browser's extension icon are no button)
		if s, ok := v.(string); ok && strings.Contains(strings.ToLower(s), "click “") {
			t.Errorf("en %s: click “…” (glossary: select “…”): %q", k, s)
		}
	}
}

// r7 (review R69 L-3): "when closing the window" applies while tasks run or
// wait (fetches of recordings too; desktop/src/main/lib/policy.js
// hasTasks), and the tray's "Quit" asks first then as well (decideQuit).
func TestR7CloseHintFollowsTheShell(t *testing.T) {
	var zh, en map[string]any
	for loc, m := range map[string]*map[string]any{"zh-CN": &zh, "en": &en} {
		if err := json.Unmarshal([]byte(readWeb(t, filepath.Join("i18n", loc+".json"))), m); err != nil {
			t.Fatal(err)
		}
	}
	z, _ := zh["settings.app.close_hint"].(string)
	e, _ := en["settings.app.close_hint"].(string)
	for _, w := range []string{"排队", "刮削", "托盘", "“退出”"} {
		if !strings.Contains(z, w) {
			t.Errorf("zh-CN settings.app.close_hint lacks %q: %q", w, z)
		}
	}
	for _, w := range []string{"queued", "fetch", "tray", "“Quit”"} {
		if !strings.Contains(e, w) {
			t.Errorf("en settings.app.close_hint lacks %q: %q", w, e)
		}
	}
}

// R75 L5: the outcome of a failed installation is shown where no link to a
// release page can be offered (the version is not offered again): it does
// not send the user there, only to what the page has.
func TestR7UpdateOutcomesNoDeadEnd(t *testing.T) {
	for loc, page := range map[string]string{"zh-CN": "发布页", "en": "Releases page"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, filepath.Join("i18n", loc+".json"))), &m); err != nil {
			t.Fatal(err)
		}
		label, _ := m["settings.update.label"].(string)
		for _, k := range []string{"settings.update.result.rolled_back", "settings.update.result.failed"} {
			s, _ := m[k].(string)
			if strings.Contains(s, page) {
				t.Errorf("%s %s: sends to the %s without a link: %q", loc, k, page, s)
			}
			if !strings.Contains(s, "“"+label+"”") {
				t.Errorf("%s %s: says where to try again (%q): %q", loc, k, label, s)
			}
		}
	}
}

// R81 L7: LectureFlow sends the requests, not the services named; and the
// sentence about kept pricing reads right with one name or several.
func TestR7NightWindowSubject(t *testing.T) {
	want := map[string][2]string{
		"zh-CN": {"发往 {names} 的请求", "{names} 已经有"},
		"en":    {"Requests to {names} are only sent", "{names} already has"},
	}
	for loc, w := range want {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, filepath.Join("i18n", loc+".json"))), &m); err != nil {
			t.Fatal(err)
		}
		if s, _ := m["settings.provider.preset_roles_night"].(string); !strings.HasPrefix(s, w[0]) {
			t.Errorf("%s preset_roles_night: the requests are the subject (%q): %q", loc, w[0], s)
		}
		if s, _ := m["settings.provider.preset_roles_night_kept"].(string); loc == "en" && strings.Contains(s, w[1]) {
			t.Errorf("en preset_roles_night_kept: “has” after a list of names: %q", s)
		}
	}
}

// R75 L1: the settings page's banner button that opens the setup guide and
// a remote server card's "continue setup" are on the same page: two names.
func TestR7GuideButtonOwnName(t *testing.T) {
	for _, loc := range []string{"zh-CN", "en"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, filepath.Join("i18n", loc+".json"))), &m); err != nil {
			t.Fatal(err)
		}
		g, _ := m["settings.alert.guide_continue"].(string)
		r, _ := m["settings.remote.continue"].(string)
		if g == "" || g == r {
			t.Errorf("%s: the guide's banner button %q has the name of the server card's %q", loc, g, r)
		}
		if toast, _ := m["onboarding.leave_toast"].(string); !strings.Contains(toast, "“"+g+"”") {
			t.Errorf("%s: onboarding.leave_toast quotes the banner button %q: %q", loc, g, toast)
		}
	}
}

// r7 (T24 S5): a failed transcription worker is told in plain words first,
// then what to do; the process name (and exit code) only at the end, in
// parentheses.
func TestR7WorkerErrorsPlainFirst(t *testing.T) {
	cat := func(loc string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, filepath.Join("i18n", loc+".json"))), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	zh, en := cat("zh-CN"), cat("en")
	for ns, proc := range map[string]string{"asr": "asr.py", "asr_ov": "asr_openvino.py", "asr_mlx": "asr_mlx.py"} {
		for _, k := range []string{"worker_start_failed", "worker_input_error", "worker_failed", "worker_error", "worker_no_output"} {
			key := "errors." + ns + "." + k
			z, _ := zh[key].(string)
			e, _ := en[key].(string)
			if !strings.HasPrefix(z, "转写") || !strings.HasSuffix(z, "）") {
				t.Errorf("zh-CN %s: plain words first, the details last in parentheses: %q", key, z)
			}
			if i := strings.LastIndex(z, "（"); i < 0 || !strings.Contains(z[i:], proc) || strings.Contains(z[:i], proc) {
				t.Errorf("zh-CN %s: the process name only in the closing parentheses: %q", key, z)
			}
			if !strings.HasPrefix(e, "Transcription") || !strings.HasSuffix(e, ").") {
				t.Errorf("en %s: plain words first, the details last in parentheses: %q", key, e)
			}
			if i := strings.LastIndex(e, "("); i < 0 || !strings.Contains(e[i:], proc) || strings.Contains(e[:i], proc) {
				t.Errorf("en %s: the process name only in the closing parentheses: %q", key, e)
			}
			if k != "worker_start_failed" && k != "worker_input_error" && (!strings.Contains(z, "重试") || !strings.Contains(e, "Retry")) {
				t.Errorf("%s: says it can be retried: %q / %q", key, z, e)
			}
			// R75 M3: exit code 2 is the audio or the model / device
			// (worker/asr.py InputError): the text blames neither alone and
			// gives the way out of each
			if k == "worker_input_error" {
				for _, w := range []string{"预处理", "模型", "设备"} {
					if !strings.Contains(z, w) {
						t.Errorf("zh-CN %s: names %q as a cause and way out: %q", key, w, z)
					}
				}
				for _, w := range []string{"preprocessing", "model", "device"} {
					if !strings.Contains(e, w) {
						t.Errorf("en %s: names %q as a cause and way out: %q", key, w, e)
					}
				}
				if strings.Contains(z, "这段录音的输入有问题") || strings.Contains(e, "something is wrong with the input of this recording") {
					t.Errorf("%s: blames the recording alone: %q / %q", key, z, e)
				}
			}
		}
	}
}

// r7 (user simulations T22-T26): the setup guide, the settings page and the
// diagnostics page on a DOM stand-in whose focus behaves as a browser's
// (testdata/r7_settings_test.mjs says what each part checks).
func TestR7SettingsLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r7_settings_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r7_settings_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r7 settings checks: ok") {
		t.Fatalf("r7_settings_test.mjs: %v\n%s", err, out)
	}
}
