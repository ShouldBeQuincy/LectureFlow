// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round 7 (the user walk-throughs T22, T24, T25), the lecture page and the
// stage cards: a failure's way out can be reached from where it is shown
// (another model first where a plain retry cannot get past it, Settings
// where the fix is a key or a model name), the card shows the translated
// sentence with the server's text folded away, an automatic retry is not
// told to be clicked, a wait says how to go on, and the lecture's usage
// over every run is shown beside its latest. The behaviour is checked by
// testdata/r7_lecture_test.mjs on a DOM stand-in (copied from
// a11y_test.mjs).
func TestR7LecturePage(t *testing.T) {
	// the lecture page's background note is chosen by the app's mode
	// (DESKTOP, a constant when api.js is loaded, which the DOM stand-in
	// cannot switch): the call is pinned here, the choice in the .mjs
	// (R79b m5d3)
	if src := readWeb(t, "view_lecture.js"); !strings.Contains(src, "t(backgroundNoteKey(DESKTOP))") {
		t.Error("view_lecture.js does not choose the background note by DESKTOP")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r7_lecture_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r7_lecture_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r7 lecture checks: ok") {
		t.Fatalf("r7_lecture_test.mjs: %v\n%s", err, out)
	}
}
