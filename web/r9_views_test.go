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

// runR9Views runs one of the r9-web-views line's JavaScript checks with the
// web directory as a file: URL and wants its "ok" line.
func runR9Views(t *testing.T, script, okLine string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/" + script + " need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, script, url)
	if err != nil || !strings.Contains(string(out), okLine) {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
}

// Round 9, the r9-web-views line, its lecture side: the held sentence
// actions' toasts stay (WV-01), and the later items of that side; checked
// by testdata/r9_views_lecture_test.mjs on the DOM stand-in of r7_dom.mjs.
func TestR9ViewsLecture(t *testing.T) {
	runR9Views(t, "r9_views_lecture_test.mjs", "r9 views lecture checks: ok")
}

// Round 9, the r9-web-views line, its settings side: the provider dialog
// asks before a change is dropped (WV-06), and the later items of that
// side; checked by testdata/r9_views_settings_test.mjs on the DOM
// stand-in of r8_settings_test.mjs.
func TestR9ViewsSettings(t *testing.T) {
	runR9Views(t, "r9_views_settings_test.mjs", "r9 views settings checks: ok")
}

// WV-11 (R86 L3; convention C3): no disk space through the binary fmtMB in
// the remote server's card and wizard (their facts are not drawn on the
// stand-in; installSizeText and diagRows are checked in the .mjs) or the
// guide's "computer" step; memory and video memory keep fmtMB.
func TestR9ViewsDiskDecimal(t *testing.T) {
	disk := regexp.MustCompile(`fmtMB\([^)]*disk_free_mb`)
	for _, f := range []string{"settings_remote.js", "onboarding_computer.js"} {
		if m := disk.FindString(readWeb(t, f)); m != "" {
			t.Errorf("%s shows disk space with fmtMB: %s", f, m)
		}
	}
	src := readWeb(t, "settings_remote.js")
	for _, want := range []string{
		"fact(t('settings.remote.fact.disk'), p.disk_free_mb >= 0 && v.probe ? diskText(p.disk_free_mb) : ''),",
		"p && p.disk_free_mb >= 0 ? [t('settings.remote.fact.disk'), diskText(p.disk_free_mb)] : null,",
		"const size = installSizeText(v);",
		"U.fmtMB(p.mem_total_mb)",
		"U.fmtMB(p.vram_total_mb)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("settings_remote.js lacks %q", want)
		}
	}
}
