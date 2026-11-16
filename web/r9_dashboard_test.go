// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round 9, the workbench line (r9-web-dash), from the UI recheck of round 8
// (T28): where the page and the focus are after the workbench is drawn
// again. Checked on the DOM stand-in of testdata/r7_dom.mjs with the simple
// layout testdata/r9_dashboard_scroll_test.mjs adds to it; the file names
// its items (WD9-nn).
func TestR9DashboardScroll(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r9_dashboard_scroll_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r9_dashboard_scroll_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r9 dashboard scroll checks: ok") {
		t.Fatalf("r9_dashboard_scroll_test.mjs: %v\n%s", err, out)
	}
}
