// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round 8, the lecture and course pages, the player's transcript (the
// r8-web-lecture line): every "update" of the lecture page and the course
// page's "start all drafts" ask first, once; the course list builds its
// stage cards when they open; the player's sentences have a visible "⋯";
// the notices and toasts say what holds. The behaviour is checked by
// testdata/r8_lecture_test.mjs on the DOM stand-in of r7_dom.mjs.
func TestR8LecturePage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r8_lecture_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r8_lecture_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r8 lecture checks: ok") {
		t.Fatalf("r8_lecture_test.mjs: %v\n%s", err, out)
	}
}

// WL-06 (R78c): the player's "用当前设置重翻此句" passes the lecture, so the
// toast says when the translation is held (the player is not mounted on
// the stand-in; the transcript tab's call is checked in the .mjs).
func TestR8RetranslateCurrentHeldInPlayer(t *testing.T) {
	if src := readWeb(t, "view_player.js"); !strings.Contains(src, "retranslateCurrent(id, s.idx, S.lec)") {
		t.Error("view_player.js does not pass the lecture to retranslateCurrent")
	}
}

// WL-04 (R82 C-I2): the inline styles that said what style.css says were
// dropped; the rules that give the look stay (TestR7WaitNoticeAndErrDetail,
// TestR7LecturePageInlineRules check them).
func TestR8NoDuplicateInlineStyles(t *testing.T) {
	for _, c := range []struct{ file, gone, rule string }{
		{"pipeline.js", "minWidth: '2.9em'", ".pipe-lg .pipe-pct"},
		{"pipeline.js", "margin: '4px 0 6px'", ".card-errdetail"},
		{"view_lecture.js", "flexWrap: 'wrap', rowGap: '6px'", ".lec-notice.is-wait"},
		{"view_lecture.js", "flex: '1 1 14em'", ".lec-notice.is-wait > .lec-notice-text"},
		{"view_lecture.js", "marginLeft: 'auto'", ".lec-notice.is-wait > .btn"},
	} {
		if strings.Contains(readWeb(t, c.file), c.gone) {
			t.Errorf("%s still has the inline style %q (style.css %s gives it)", c.file, c.gone, c.rule)
		}
		cssRule(t, readWeb(t, "style.css"), c.rule)
	}
}
