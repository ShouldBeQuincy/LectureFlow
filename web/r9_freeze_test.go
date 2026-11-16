// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round 9, the interfaces frozen before the lines start: openDialog's
// opts.confirmCancel (F1, dom.js) and confirmLaunchDrafts's opts.courses
// (F2, update_confirm.js). The behaviour is checked by
// testdata/r9_freeze_test.mjs on the DOM stand-in of r7_dom.mjs.
func TestR9Freeze(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r9_freeze_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r9_freeze_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r9 freeze checks: ok") {
		t.Fatalf("r9_freeze_test.mjs: %v\n%s", err, out)
	}
	for file, sig := range map[string]string{
		"dom.js":            "export function openDialog(title, build, opts = {}) {",
		"update_confirm.js": "export function confirmLaunchDrafts(drafts, count, opts = {}) {",
	} {
		if !strings.Contains(readWeb(t, file), sig) {
			t.Errorf("%s lacks %s", file, sig)
		}
	}
}
