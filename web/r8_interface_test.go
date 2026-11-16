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

// The interfaces frozen for the round 8 web lines (the workbench, the
// lecture and course pages, the settings, the shared modules): F1 sticky
// toasts (dom.js toast's opts.sticky), F2 a confirmation after the glossary
// check before the drafts are launched (actions.js launchCourseDrafts's
// opts.ask), F3 the guide-left rule (util.js guideLeft). The behaviour is
// checked by testdata/r8_interface_test.mjs on the DOM stand-in.
func TestR8Interfaces(t *testing.T) {
	for _, f := range []struct{ file, sig string }{
		{"dom.js", "export function toast(message, kind = 'info', opts = {}) {"},
		{"actions.js", "export function launchCourseDrafts(course, opts = {}) {"},
		{"util.js", "export function guideLeft(guide) {"},
	} {
		if !strings.Contains(readWeb(t, f.file), f.sig) {
			t.Errorf("%s lacks the frozen signature %q", f.file, f.sig)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r8_interface_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r8_interface_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r8 interface checks: ok") {
		t.Fatalf("r8_interface_test.mjs: %v\n%s", err, out)
	}
}

// F3: util.js guideLeft is the settings page's rule, word for word. While
// view_settings.js keeps its own copy (until it calls U.guideLeft), the two
// must say the same.
func TestR8GuideLeftMatchesSettings(t *testing.T) {
	util, settings := readWeb(t, "util.js"), readWeb(t, "view_settings.js")
	u := regexp.MustCompile(`export function guideLeft\(guide\) \{\s*return (.+);\s*\}`).FindStringSubmatch(util)
	if u == nil {
		t.Fatal("util.js: no guideLeft(guide) { return …; }")
	}
	s := regexp.MustCompile(`const guideLeft = \(\) => (.+);`).FindStringSubmatch(settings)
	if s == nil {
		if !strings.Contains(settings, "guideLeft(") {
			t.Fatal("view_settings.js: neither its own guideLeft nor a call of U.guideLeft")
		}
		return
	}
	if u[1] != s[1] {
		t.Errorf("util.js guideLeft says\n  %s\nthe settings page\n  %s", u[1], s[1])
	}
}
