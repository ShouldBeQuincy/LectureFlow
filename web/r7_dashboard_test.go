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

// Round 7 (the user walk-throughs T22-T26): the workbench, the course cards,
// the navigation and the resource panel say what failed and where the way
// out is. The behaviour is checked by testdata/r7_dashboard_test.mjs on a
// DOM stand-in (the dashboard, the course card, the resource panel).
func TestR7Dashboard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r7_dashboard_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r7_dashboard_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r7 dashboard checks: ok") {
		t.Fatalf("r7_dashboard_test.mjs: %v\n%s", err, out)
	}
}

// T22 S5, R76 L-11: the workbench's "a role is not set up" notice judges the
// roles by the settings page's rule (view_settings.js roleBroken), of which
// it keeps its own copy (util.js was frozen this round). The two copies
// must say the same: the launch roles, the optional roles, "transcription
// only" and the broken role.
func TestR7RolesRuleMatchesSettings(t *testing.T) {
	settings, dash := readWeb(t, "view_settings.js"), readWeb(t, "view_dashboard.js")
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`const LAUNCH_ROLES = (\[[^\]]*\]);`),
		regexp.MustCompile(`const OPTIONAL_ROLES = new Set\((\[[^\]]*\])\);`),
	} {
		s, d := re.FindStringSubmatch(settings), re.FindStringSubmatch(dash)
		if s == nil || d == nil {
			t.Fatalf("%s: settings page %v, workbench %v", re, s != nil, d != nil)
		}
		if s[1] != d[1] {
			t.Errorf("%s: the settings page has %s, the workbench %s", re, s[1], d[1])
		}
	}
	for _, w := range []string{
		"LAUNCH_ROLES.every((id) => roles.some((r) => r.role === id && !r.configured))", // transcription only
		"!r.configured && !OPTIONAL_ROLES.has(r.role)",                                    // a broken role
	} {
		if !strings.Contains(settings, w) || !strings.Contains(dash, w) {
			t.Errorf("the settings page (%v) and the workbench (%v) must both judge by %q", strings.Contains(settings, w), strings.Contains(dash, w), w)
		}
	}
}

// The page shell (app.js: the navigation, the connection banner, the
// notifications, the page title), booted whole on the same stand-in by
// testdata/r7_app_test.mjs.
func TestR7App(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r7_app_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r7_app_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r7 app checks: ok") {
		t.Fatalf("r7_app_test.mjs: %v\n%s", err, out)
	}
}
