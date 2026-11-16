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

// Round 8, the shared web modules (dom.js, util.js, actions.js,
// update_confirm.js, menu.js): the menus' "update" and "start all drafts"
// ask first (T25 X1), and the other items of the r8-web-shared line. The
// behaviour is checked by testdata/r8_shared_test.mjs on the DOM stand-in
// of r7_dom.mjs.
func TestR8Shared(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r8_shared_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r8_shared_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r8 shared checks: ok") {
		t.Fatalf("r8_shared_test.mjs: %v\n%s", err, out)
	}
}

// The selection's "start" (lecture_list.js, r8-web-lecture) asks through
// launchLectures' opts.ask, after the glossary check, as launchCourseDrafts
// does (F2): its signature, which that line calls.
func TestR8LaunchLecturesAsk(t *testing.T) {
	if !strings.Contains(readWeb(t, "actions.js"), "export function launchLectures(lectures, opts = {}) {") {
		t.Error("actions.js lacks launchLectures(lectures, opts = {})")
	}
}

// WH-03 (T22 S4): a menu item's description (menu.js desc) is a second
// line under the label and the hint that wraps within the menu (at most
// 340px wide, so whole at 360: measured 340px from 8 to 348 in the built-in
// browser, no item wider than its menu); the item grows with it, 40px at
// least on a touch screen; an item without one keeps its 30px (40 touch).
func TestR8MenuDescriptionStyle(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".ctx-menu", cssRule(t, css, ".ctx-menu"), "min-width: 210px; max-width: 340px;")
	hasDecls(t, ".ctx-item", cssRule(t, css, ".ctx-item"), "height: 30px;")
	hasDecls(t, ".ctx-item.has-desc", cssRule(t, css, ".ctx-item.has-desc"), "height: auto;", "min-height: 30px;")
	hasDecls(t, ".ctx-desc", cssRule(t, css, ".ctx-desc"), "grid-row: 2; grid-column: 2 / 4;", "white-space: normal;", "overflow-wrap: anywhere;")
	hasDecls(t, ".ctx-desc on a highlighted item", cssRule(t, css, `.ctx-item:focus .ctx-desc, .ctx-item[aria-expanded="true"] .ctx-desc`), "color: light-dark(var(--text-3), #a3aab5);")
	touch := strings.Join(mediaBody(t, css, "(pointer: coarse)"), "\n")
	hasDecls(t, "touch .ctx-item", cssRule(t, touch, ".ctx-item"), "height: 40px;")
	hasDecls(t, "touch .ctx-item.has-desc", cssRule(t, touch, ".ctx-item.has-desc"), "min-height: 40px;")
}

// WH-08 (R70 low 3): in the dashboard's "recently finished" at 360 (its
// card's content 182px wide) the date, the length and the cost did not fit
// the 106px beside the actions and took two lines on 9 rows of 10 (en,
// zh-CN; ru 10 of 10), each row 12px higher. In a card up to 200px wide
// (414 gives 236: unchanged there and wider) the cost is left out. The
// wide layouts and the kind's 116px (R70) are as they were.
func TestR8RecentNarrowMeta(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, "narrowest .rd-m2", atRule(t, css, "@container (max-width: 200px)", ".rd-m2"), "display: none;")
	hasDecls(t, "narrow .rd-meta", atRule(t, css, "@container (max-width: 720px)", ".rd-meta"), "display: flex; grid-row: 2; grid-column: 3 / -1; gap: 4px 12px; flex-wrap: wrap;")
	hasDecls(t, "narrower .rd-meta", atRule(t, css, "@container (max-width: 460px)", ".rd-meta"), "grid-column: 1 / 5; grid-row: 3;")
	hasDecls(t, "narrower kind", atRule(t, css, "@container (max-width: 460px)", ".rd-row > .lref-tag"), "grid-column: 2 / -1; grid-row: 1;")
	if !strings.Contains(readWeb(t, "view_dashboard.js"), "meta.map((m, i) => h('span', { class: `rd-m rd-m${i}` }, m)),") {
		t.Error("view_dashboard.js: the recent rows' figures are no longer rd-m0 (date), rd-m1 (length), rd-m2 (cost)")
	}
	if !strings.Contains(readWeb(t, "view_dashboard.js"), "costText(lec) || ''];") {
		t.Error("view_dashboard.js: the cost is no longer the third figure (rd-m2) of a recent row")
	}
}

// Handed over by r8-web-lecture, WL-12 (R70 low 2): a click on the check
// box's cell is one on the box (lecture_list.js on that line), so the cell
// fills its row (into the row's 8px padding) and the header's height.
func TestR8LectureCheckCell(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".lrow > .lt-check", cssRule(t, css, ".lrow > .lt-check"), "align-self: stretch; margin: -8px 0; cursor: pointer;")
	hasDecls(t, ".lt-head > .lt-check", cssRule(t, css, ".lt-head > .lt-check"), "align-self: stretch; cursor: pointer;")
	hasDecls(t, ".lrow padding", cssRule(t, css, ".lrow"), "padding-top: 8px; padding-bottom: 8px;")
}

// Handed over by r8-web-lecture, WL-08: a lecture statistic with an
// explanation is a focusable group on that line, not a link; its focus ring
// is drawn inside the cell as the links' is (.lec-stats clips what
// overflows it).
func TestR8LectureStatFocus(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".lec-stat:focus-visible", cssRule(t, css, ".lec-stat:focus-visible"), "outline-offset: -2px; border-radius: var(--r-sm);")
	if strings.Contains(css, "\na.lec-stat:focus-visible") {
		t.Error("style.css: the stats' focus ring is still for links only")
	}
	hasDecls(t, ".lec-stats", cssRule(t, css, ".lec-stats"), "overflow: hidden;")
}

// Handed over by r8-web-settings, WS-11: a provider's row reached from
// another page (#/settings/providers/{id}, convention F4) flashes as a
// slide file's row does; not with reduced motion.
func TestR8ProviderRowFlash(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".pv-row.flash", cssRule(t, css, ".pv-row.flash"), "animation: lf-flash 1.8s var(--ease); border-radius: var(--r-md);")
	if !strings.Contains(css, "@media (prefers-reduced-motion: reduce) { .pv-row.flash { animation: none; } }") {
		t.Error("style.css: .pv-row.flash still moves with reduced motion")
	}
	if !strings.Contains(css, "@keyframes lf-flash {") {
		t.Error("style.css: no lf-flash keyframes")
	}
}

// Handed over by r8-web-dash, WD-03 (T22 G15): a course card's visible "⋯"
// beside its link in div.ccard-wrap (course_card.js on that line), at the
// card's top right, the card hovered while the pointer is on it, and a 40px
// touch area inside the card.
func TestR8CourseCardMore(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".ccard-wrap", cssRule(t, css, ".ccard-wrap"), "position: relative; display: grid; min-width: 0;")
	hasDecls(t, ".ccard-more", cssRule(t, css, ".ccard-wrap > .ccard-more"), "position: absolute; top: 8px; right: 8px;")
	hasDecls(t, ".ccard-top room", cssRule(t, css, ".ccard-wrap > .ccard .ccard-top"), "padding-right: 26px;")
	hasDecls(t, "hovered card", cssRule(t, css, ".ccard-wrap:hover > .ccard"), "border-color: var(--border-2); box-shadow: var(--shadow-md);")
	hasDecls(t, "the card's own hover", cssRule(t, css, ".ccard:hover"), "border-color: var(--border-2); box-shadow: var(--shadow-md);")
	touch := strings.Join(mediaBody(t, css, "(pointer: coarse)"), "\n")
	hasDecls(t, "touch ccard-more", cssRule(t, touch, ".ccard-wrap > .ccard-more::after"), `content: "";`, "position: absolute;", "inset: min(0px, calc(50% - 20px));")
	hasDecls(t, ".icon-btn", cssRule(t, css, ".icon-btn"), "width: 28px; height: 28px;")
	hasDecls(t, "focus ring", cssRule(t, css, ":focus-visible"), "outline: 2px solid var(--focus-ring);")
}

// Handed over by r8-web-lecture, WL-03 (T22 G15): every sentence of the
// player's transcript has a visible "⋯" (.s-more-btn.pl-row-more,
// player_transcript.js on that line) in the last column with "change to
// review": at the top, at the bottom under the mark; a row with the mark
// is at least 40px on its line (a one-line sentence's "⋯" covered the mark,
// both 7-25px); fully seen on hover, the current sentence and keyboard
// focus. On a touch screen the mark's area stops at its bottom edge and the
// "⋯"'s at the mark's and at its row's top.
func TestR8PlayerRowMore(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".pl-row", cssRule(t, css, ".pl-row"), "grid-template-columns: 46px minmax(0, 1fr) 26px;")
	hasDecls(t, "mark", cssRule(t, css, ".pl-row > .pl-row-pend"), "grid-column: 3; grid-row: 1; justify-self: center;")
	hasDecls(t, "more", cssRule(t, css, ".pl-row > .pl-row-more"), "grid-column: 3; grid-row: 1; align-self: start; margin: -3px 0 0;")
	hasDecls(t, "more under the mark", cssRule(t, css, ".pl-row.is-pending > .pl-row-more"), "align-self: end; margin: 0 0 -4px;")
	hasDecls(t, "a marked row's line", cssRule(t, css, ".pl-row.is-pending"), "grid-template-rows: minmax(40px, auto);")
	hasDecls(t, "more seen", cssRule(t, css, ".pl-row:hover .pl-row-more, .pl-row.is-cur .pl-row-more, .pl-row:focus-visible .pl-row-more"), "opacity: 1;")
	hasDecls(t, "more focused", cssRule(t, css, `.srow:hover .s-more-btn, .s-more-btn:focus-visible, .s-more-btn[aria-expanded="true"], .srow.is-current .s-more-btn`), "opacity: 1;")
	touch := strings.Join(mediaBody(t, css, "(pointer: coarse)"), "\n")
	hasDecls(t, "touch mark", cssRule(t, touch, ".pl-row-pend::after"), "inset: -6px -10px 0 -7px;")
	hasDecls(t, "touch more", cssRule(t, touch, ".pl-row > .pl-row-more::after"), "top: -4px;")
	hasDecls(t, "touch more under the mark", cssRule(t, touch, ".pl-row.is-pending > .pl-row-more::after"), "top: 0;")
}

// WH-05: util.js settingsFix tells the stages without "retry with another
// model…" by its own copy of pipeline.js MODEL_RETRY_STAGES (util.js cannot
// import pipeline.js, which imports it). The two must say the same.
func TestR8ModelRetryStagesMatch(t *testing.T) {
	p := regexp.MustCompile(`export const MODEL_RETRY_STAGES = new Set\((\[[^\]]*\])\);`).FindStringSubmatch(readWeb(t, "pipeline.js"))
	u := regexp.MustCompile(`const MODEL_RETRY_STAGE_SET = new Set\((\[[^\]]*\])\);`).FindStringSubmatch(readWeb(t, "util.js"))
	if p == nil || u == nil {
		t.Fatalf("pipeline.js MODEL_RETRY_STAGES found: %v; util.js MODEL_RETRY_STAGE_SET found: %v", p != nil, u != nil)
	}
	if p[1] != u[1] {
		t.Errorf("pipeline.js MODEL_RETRY_STAGES is %s, util.js MODEL_RETRY_STAGE_SET %s", p[1], u[1])
	}
}
