// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"regexp"
	"strings"
	"testing"
)

// bareGap: a "gap" shorthand declaration (not row-gap / column-gap).
var bareGap = regexp.MustCompile(`(?:^|[;{\s])gap\s*:`)

// lrowRuleRe: a rule (selector list and declarations) of the style sheet.
var lrowRuleRe = regexp.MustCompile(`(?m)^\s*([^{}@/]+?)\s*\{([^{}]*)\}`)

// T18 (minor): a lecture row's actions on two lines end at the same right
// edge. The primary action had a 4px right margin (".lrow-end .btn"), so
// alone on its line it stopped 4px short of the line under it; the space
// beside it on one line is the actions cell's column gap instead (8px, as
// the margin and the 4px gap were). And the narrow rule's comment no
// longer says "two lines at most" (360 wide can take three: R58c).
func TestLectureRowActionsAlignRight(t *testing.T) {
	css := readWeb(t, "style.css")
	// R62 N7: the two gaps by their own names only; a "gap" shorthand,
	// before or after them, would hide or undo the column gap
	end := cssRule(t, css, ".lrow-end")
	hasDecls(t, ".lrow-end", end, "row-gap: 4px;", "column-gap: 8px;")
	if bareGap.MatchString(end) {
		t.Errorf(".lrow-end has a gap shorthand: %q", strings.TrimSpace(end))
	}
	for _, head := range []string{"@container (max-width: 760px)", "@container (max-width: 480px)"} {
		if r := atRule(t, css, head, ".lrow-end"); bareGap.MatchString(r) || strings.Contains(r, "column-gap") {
			t.Errorf("%s .lrow-end changes the column gap: %q", head, strings.TrimSpace(r))
		}
	}
	for _, m := range lrowRuleRe.FindAllStringSubmatch(css, -1) {
		sel, decls := m[1], m[2]
		if !strings.Contains(sel, ".lrow-end") && !strings.Contains(sel, ".lrow-tail") && !strings.Contains(sel, ".lrow-dlmenu") {
			continue
		}
		if !strings.Contains(sel, ".btn") && !strings.Contains(sel, "button") {
			continue
		}
		if strings.Contains(decls, "margin") {
			t.Errorf("style.css: %s { %s } gives the row's action buttons a margin (two lines no longer end at one edge)", sel, strings.TrimSpace(decls))
		}
	}
	if strings.Contains(css, "Two lines at most") {
		t.Error("style.css: the narrow actions comment still says \"Two lines at most\"")
	}
}

// The guard itself: a gap shorthand anywhere in a rule, not row-gap or
// column-gap (R62 N7: "row-gap: 4px;" passed for "gap: 4px;").
func TestBareGapGuard(t *testing.T) {
	for decls, want := range map[string]bool{
		" display: flex; gap: 4px;": true,
		"gap:4px":                   true,
		" row-gap: 4px; column-gap: 8px; gap: 4px;": true,
		" row-gap: 4px; column-gap: 8px;":           false,
		" grid-gap: 4px;":                           false,
	} {
		if got := bareGap.MatchString(decls); got != want {
			t.Errorf("%q: %v, want %v", decls, got, want)
		}
	}
}

// T21 M1: a context menu that is taller than the room beside its button is
// height-limited and scrolls inside (menu.js anchoredMenuPlace). Scrolling
// it to its end must not move the page under it: the page's scroll closes
// the menu and drops the focus to the body.
func TestContextMenuKeepsItsScroll(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".ctx-menu", cssRule(t, css, ".ctx-menu"), "overscroll-behavior: contain;")
}

// T21 M7: the playback rate menu opens with the focus on the rate in use
// (menu.js openMenu's focus option), as a list of choices with one current
// does; it opened on 0.5x.
func TestRateMenuOpensOnCurrentRate(t *testing.T) {
	js := readWeb(t, "view_player.js")
	if !strings.Contains(js, "focus: Math.max(0, RATES.indexOf(S.rate))") {
		t.Error("view_player.js: the rate menu does not open on the rate in use")
	}
}
