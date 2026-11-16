// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// R7-PF1 (P2 hotspot 1): the course page's glossary table is kept up to
// date instead of being built anew — one term edited draws that term's row
// only, a reload or a course that changes nothing draws nothing, the filter
// moves the same rows out and back — with the focus, keys, groups, filter
// results and refusals as before (the logic is in
// testdata/glossary_table_test.mjs).
func TestGlossaryTableDrawsOnlyWhatChanged(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/glossary_table_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "glossary_table_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "glossary table checks: ok") {
		t.Fatalf("glossary_table_test.mjs: %v\n%s", err, out)
	}
}
