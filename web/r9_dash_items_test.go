// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round 9, the workbench line (r9-web-dash): the workbench and the course
// card, checked on the DOM stand-in of testdata/r7_dom.mjs by
// testdata/r9_dash_items_test.mjs; each section names its item (WD-nn).
func TestR9DashItems(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r9_dash_items_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r9_dash_items_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r9 dash items checks: ok") {
		t.Fatalf("r9_dash_items_test.mjs: %v\n%s", err, out)
	}
}

// T28 suggestion 2 (r9 WD-10, round 9 convention C2): the workbench's
// failure rows say their error as the lecture page's stage card does,
// through U.stageErrorText (which knows the stage; the shared line's WH-07
// gives the stages that can retry with another model their own sentence
// there). Until that lands the texts are the same as errorInfoText's, so
// the node checks pass either way; this pins the call.
func TestR9DashFailureText(t *testing.T) {
	src := readWeb(t, "view_dashboard.js")
	if !strings.Contains(src, "U.stageErrorText({ stage: f.stage, status: 'failed', error_info: f.error_info, error: f.error || '' })") {
		t.Error("view_dashboard.js: a failure's text no longer comes from U.stageErrorText with its stage")
	}
	if strings.Contains(src, "U.errorInfoText(f.error_info") {
		t.Error("view_dashboard.js: a failure's text is still U.errorInfoText's, which does not know the stage")
	}
}

// The page shell (app.js, booted whole) of the same line:
// testdata/r9_dash_app_test.mjs.
func TestR9DashApp(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r9_dash_app_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r9_dash_app_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r9 dash app checks: ok") {
		t.Fatalf("r9_dash_app_test.mjs: %v\n%s", err, out)
	}
}
