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

// r4: a link the desktop shell refuses for its limit says so in a notice
// (the logic is in testdata/link_throttled_test.mjs).
func TestLinkThrottledLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/link_throttled_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "link_throttled_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "link throttled checks: ok") {
		t.Fatalf("link_throttled_test.mjs: %v\n%s", err, out)
	}
}

// Every call of the shell's openExternal goes through the shared catch:
// none swallows the limit's error.
func TestLinkThrottledWired(t *testing.T) {
	for file, want := range map[string]string{
		"onboarding_api.js":  "Promise.resolve(d.openExternal(url)).catch(externalLinkFailed);",
		"settings_update.js": "Promise.resolve(lf.openExternal(url)).catch(externalLinkFailed);",
		"update_hint.js":     "Promise.resolve(lf.openExternal(url)).catch(externalLinkFailed);",
	} {
		if !strings.Contains(readWeb(t, file), want) {
			t.Errorf("%s lacks %q", file, want)
		}
	}
	// the main process's words, which the page looks for
	if !strings.Contains(readWeb(t, "onboarding_api.js"), "const LINK_THROTTLED = 'too many links in a short time';") {
		t.Error("onboarding_api.js: the limit's error text changed")
	}
	swallowed := regexp.MustCompile(`openExternal\([^)]*\)\)?\s*\.catch\(\(\)\s*=>\s*\{\s*\}\)`)
	files, err := filepath.Glob("*.js")
	if err != nil || len(files) == 0 {
		t.Fatalf("no web/*.js: %v", err)
	}
	for _, f := range files {
		if m := swallowed.FindString(readWeb(t, f)); m != "" {
			t.Errorf("%s swallows openExternal's error: %s", f, m)
		}
	}
}
