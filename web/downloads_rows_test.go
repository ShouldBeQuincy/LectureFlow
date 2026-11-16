// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Review R61 F2 and F4: the download rows draw the task's allowed_actions
// (remove only for a failure no retry can fix), each a focusable, named
// button, and a retry refused for a refused file says so in the row's words
// (the logic is in testdata/downloads_rows_test.mjs).
func TestDownloadRowsActions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/downloads_rows_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "downloads_rows_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "download rows checks: ok") {
		t.Fatalf("downloads_rows_test.mjs: %v\n%s", err, out)
	}
}
