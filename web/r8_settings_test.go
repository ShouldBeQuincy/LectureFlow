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

// Round 8 (the settings line): the settings page uses the shared rules of
// util.js instead of copies of its own — the guide left unfinished (F3,
// WS-12) and whether a model is in a provider's list (R79 L12, WS-09).
func TestR8SettingsSharedRules(t *testing.T) {
	src := readWeb(t, "view_settings.js")
	if regexp.MustCompile(`const guideLeft\s*=`).MatchString(src) {
		t.Error("view_settings.js keeps its own guideLeft (util.js has the shared one)")
	}
	if !strings.Contains(src, "U.guideLeft(guide)") {
		t.Error("view_settings.js does not ask U.guideLeft(guide) whether the guide was left unfinished")
	}
	if regexp.MustCompile(`function modelListed\s*\(`).MatchString(src) {
		t.Error("view_settings.js keeps its own modelListed (util.js has the shared one)")
	}
	if !strings.Contains(src, "U.modelListed(") {
		t.Error("view_settings.js does not use U.modelListed")
	}
}

// Round 8 (the settings line): the settings page, the setup guide and the
// wizard on a DOM stand-in (testdata/r8_settings_test.mjs says what each
// part checks).
func TestR8SettingsLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r8_settings_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r8_settings_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r8 settings checks: ok") {
		t.Fatalf("r8_settings_test.mjs: %v\n%s", err, out)
	}
}
