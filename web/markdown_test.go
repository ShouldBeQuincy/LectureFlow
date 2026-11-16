// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Markdown preview (markdown.js, AUD5 low-3): the same output as before
// for typical and hostile documents (testdata/markdown_golden.json), only
// allowed elements, attributes and links; a long source rendered in slices;
// the inputs that were quadratic or overflowed the stack each done within
// a few seconds; nesting past the bounds shown as text
// (testdata/markdown_test.mjs).
func TestMarkdownRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/markdown_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "markdown_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "markdown checks: ok") {
		t.Fatalf("markdown_test.mjs: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
