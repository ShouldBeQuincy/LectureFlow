// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"strings"
	"testing"
)

// R4 (T16 recheck, round four): layout fixes at phone width (375 / 414,
// where the nav and the inspector rails leave the content some 230px). No
// browser runs here: the declarations are pinned, the layout is looked at
// in the browser.

// S1: the course page's download queue kept its four columns (subgrid) at
// any width; at 375 the bar's 56px and the buttons took the whole row, the
// title and the state were 0 wide, a row some 900px high. A narrow list has
// one column, as the upload rows below 960px; the wide grid is as it was.
// The limit is a 560px list (R49 B4): narrower, the four columns leave the
// title 130px or less (en; ru 118); at 768 (a 593px card) they stay.
func TestR4NarrowDownloadRows(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".dl-list", cssRule(t, css, ".dl-list:not([hidden])"), "grid-template-columns: minmax(0, 1.8fr) minmax(56px, 0.8fr) minmax(0, 2.4fr) auto;")
	hasDecls(t, ".dl-row", cssRule(t, css, ".dl-list > .dl-row"), "grid-column: 1 / -1; grid-template-columns: subgrid;")
	const narrow = "@container (max-width: 560px)"
	hasDecls(t, "narrow .dl-list", atRule(t, css, narrow, ".dl-list:not([hidden])"), "grid-template-columns: minmax(0, 1fr);")
	row := atRule(t, css, narrow, ".dl-list > .dl-row")
	hasDecls(t, "narrow .dl-row", row, "grid-template-columns: minmax(0, 1fr);")
	if strings.Contains(row, "subgrid") {
		t.Error("a narrow download row still takes the list's four columns (subgrid)")
	}
	// the state of a row without a bar spans columns 2–3 when wide; narrow
	// it takes its own line, not implicit columns beside the title
	hasDecls(t, "narrow .dl-nobar .up-status", atRule(t, css, narrow, ".dl-row.dl-nobar .up-status"), "grid-column: auto;")
}

// S2: the course page's glossary card at 375: the title was 0 wide under
// the 210px search box, and the add form (a 240px input) and its note ran
// out of the card under the inspector rail.
func TestR4NarrowGlossaryCard(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".card-terms header", cssRule(t, css, ".card-terms > .card-hd"), "flex-wrap: wrap;")
	hasDecls(t, ".card-terms title", cssRule(t, css, ".card-terms > .card-hd > .card-title"), "flex: none;", "max-width: 100%;")
	// max-width, not width: min(210px, 100%): a percentage width counts as
	// auto in the filters' max-content width, and the search and the source
	// select wrapped into two lines even at 1280
	hasDecls(t, ".term-search", cssRule(t, css, ".term-search"), "width: 210px; max-width: 100%;")
	hasDecls(t, ".term-filters", cssRule(t, css, ".term-filters"), "min-width: 0;", "max-width: 100%;")
	tools := atRule(t, css, "@container (max-width: 900px)", ".term-tools")
	hasDecls(t, "narrow .term-tools", tools, "grid-template-columns: minmax(0, 1fr);")
	if strings.Contains(strings.Join(strings.Fields(tools), " "), "grid-template-columns: 1fr;") {
		t.Error("the narrow glossary tools column is a plain 1fr again (as wide as the 240px input)")
	}
	if !strings.Contains(css, "\n.term-add .input { max-width: 100%; }") {
		t.Error("style.css: the glossary add form's inputs are no longer at most as wide as the form")
	}
}

// S3: two buttons with a long label (ru) ran out of their row unwrapped:
// the provider limits banner's (settings, 375) and the dashboard's "start
// all drafts" in the to-do card (414). Their rows wrap and the buttons wrap
// their label; the texts are as they were.
func TestR4WrappingButtons(t *testing.T) {
	css := readWeb(t, "style.css")
	btn := "max-width: 100%; height: auto; min-height: 26px; padding-top: 4px; padding-bottom: 4px; line-height: 1.25; white-space: normal; text-align: center;"
	hasDecls(t, ".pv-limits > .btn", cssRule(t, css, ".pv-limits > .btn"), btn)
	hasDecls(t, ".todo-head", cssRule(t, css, ".todo-head"), "display: flex; flex-wrap: wrap;")
	hasDecls(t, ".todo-action", cssRule(t, css, ".todo-action"), "margin-left: auto; max-width: 100%;")
	hasDecls(t, ".todo-action > .btn", cssRule(t, css, ".todo-action > .btn"), btn)
	// the banner wraps by its inline style (the button goes under the text)
	for _, f := range []string{"provider_limits.js", "view_settings.js"} {
		if !strings.Contains(readWeb(t, f), "{ class: 'banner banner-info set-banner pv-limits") {
			t.Errorf("%s: the limits banner is no longer a .pv-limits banner (the wrapping rule selects on it)", f)
		}
	}
	if !strings.Contains(readWeb(t, "view_dashboard.js"), "action ? h('span', { class: 'todo-action' }, action) : null") {
		t.Error("view_dashboard.js: the to-do group's action is no longer a .todo-action span (the wrapping rule selects on it)")
	}
}

// R4 (C126b): a card header's title and what stands right of it (buttons,
// a hint, a count) did not wrap: at 375 "Providers 3" ran under "Add
// provider" (settings) and "Materials" under its hint (course page). In a
// narrow container the header wraps and the title keeps its content's
// width; the wide header is as it was (one line, title free to shrink).
func TestR4NarrowCardHeads(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".card-hd", cssRule(t, css, ".card-hd"), "display: flex; align-items: center; justify-content: space-between; gap: 12px; min-height: 48px;")
	if strings.Contains(cssRule(t, css, ".card-hd"), "wrap") {
		t.Error("every card header wraps again at any width")
	}
	hasDecls(t, ".card-title", cssRule(t, css, ".card-title"), "min-width: 0;")
	const narrow = "@container (max-width: 480px)"
	hasDecls(t, "narrow .card-hd", atRule(t, css, narrow, ".card-hd"), "flex-wrap: wrap;")
	hasDecls(t, "narrow .card-title", atRule(t, css, narrow, ".card-hd > .card-title"), "min-width: auto;")
	hasDecls(t, "narrow header tools", atRule(t, css, narrow, ".card-hd > .card-title + *"), "margin-left: auto;")
	// the headers it is for: a title followed by its tools or hint
	if !strings.Contains(readWeb(t, "view_course.js"), "h('header', { class: 'card-hd' }, h('h2', { class: 'card-title' }, icon('book-open', { size: 16 }), h('span', null, t('course.resources.title'))), h('span', { class: 'card-hint' }") {
		t.Error("view_course.js: the materials card header is no longer a .card-title followed by its hint")
	}
}

// R4 (C126b): below 960px the upload rows have one column, but a failed
// row's error kept grid-column 2 / 4 and made two implicit columns beside
// the name (375: name 118px, error 69px wide). Narrow, it takes its own
// line; the wide row keeps the error across columns 2–3.
func TestR4NarrowFailedUpload(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".upload-failed .up-status", cssRule(t, css, ".upload-failed .up-status"), "grid-column: 2 / 4;")
	hasDecls(t, "narrow .upload-row", atRule(t, css, "@media (max-width: 960px)", ".upload-row"), "grid-template-columns: 1fr;")
	hasDecls(t, "narrow failed .up-status", atRule(t, css, "@media (max-width: 960px)", ".upload-failed .up-status"), "grid-column: auto;")
	if !strings.Contains(readWeb(t, "widgets.js"), "{ class: `upload-row upload-${item.status}`, dataset: { up: item.key } }") {
		t.Error("widgets.js: a failed upload row is no longer .upload-row.upload-failed (the narrow rule selects on it)")
	}
}

// R49 M1: kept whole as a flex item, a week group's count ran 11–18px under
// the slides button (ru, 375): the title box shrank below its content. The
// dates and the count flow as text: the count, kept whole, goes on the
// dates' last line when it fits, else under them, flush left (the gap
// before it is a space a line break drops). Wide, one line as before.
func TestR4WeekGroupCount(t *testing.T) {
	css := readWeb(t, "style.css")
	title := cssRule(t, css, ".lt-group-t")
	hasDecls(t, ".lt-group-t", title, "display: block;")
	if strings.Contains(title, "flex") {
		t.Error(".lt-group-t is a flex box again (the count either breaks into letters or runs under the slides button)")
	}
	n := cssRule(t, css, ".lt-group-n")
	hasDecls(t, ".lt-group-n", n, "white-space: nowrap;")
	if strings.Contains(n, "flex") || strings.Contains(n, "inline-block") || strings.Contains(n, "margin-left") {
		t.Error(".lt-group-n is no longer an inline span after a breakable space (a wrapped count would be indented or overflow)")
	}
	hasDecls(t, ".lt-group-n::before", cssRule(t, css, ".lt-group-n::before"), `content: " ";`, "white-space: normal;", "word-spacing: 5px;")
	if !strings.Contains(readWeb(t, "lecture_list.js"), "h('span', { class: 'lt-group-t', role: 'cell', title: g.hint || undefined }, g.title, h('span', { class: 'lt-group-n' }") {
		t.Error("lecture_list.js: a week group's title is no longer its dates followed by the .lt-group-n count")
	}
}

// R49 B2 / B3: two buttons with a long label ran out of their box at phone
// width: the course head's "update stale lectures" (ru, 7 / 22px out of
// main at 375 / 360) and the glossary janitor's "view cleanup report" (en,
// 9px out of the card at 375). They wrap their label; wide, one line and
// the same height as before.
func TestR4NarrowHeadAndJanitorButtons(t *testing.T) {
	css := readWeb(t, "style.css")
	const page = "@container (max-width: 480px)"
	hasDecls(t, "narrow .page-actions", atRule(t, css, page, ".page-actions"), "max-width: 100%;", "min-width: 0;")
	pb := atRule(t, css, page, ".page-actions > .btn")
	hasDecls(t, "narrow .page-actions > .btn", pb, "max-width: 100%;", "height: auto;", "min-height: 32px;", "padding-top: 6px;", "padding-bottom: 6px;", "white-space: normal;")
	hasDecls(t, ".page-actions", cssRule(t, css, ".page-actions"), "display: flex; align-items: center; gap: 8px; flex-wrap: wrap;")
	const jan = "@container (max-width: 760px)"
	hasDecls(t, "narrow .jan-actions", atRule(t, css, jan, ".jan-actions"), "width: 100%;", "padding-left: 42px;")
	jb := atRule(t, css, jan, ".jan-actions > .btn")
	hasDecls(t, "narrow .jan-actions > .btn", jb, "max-width: 100%;", "height: auto;", "min-height: 26px;", "padding-top: 3px;", "padding-bottom: 3px;", "white-space: normal;")
}

// General and detail items of R4 that are only style.
func TestR4NarrowDetails(t *testing.T) {
	css := readWeb(t, "style.css")
	// G1: the holiday date chips under the year at narrow width
	const narrow = "@container (max-width: 480px)"
	hasDecls(t, ".hol-year", cssRule(t, css, ".hol-year"), "grid-template-columns: 72px 52px minmax(0, 1fr);")
	hasDecls(t, "narrow .hol-year", atRule(t, css, narrow, ".hol-year"), "grid-template-columns: auto minmax(0, 1fr);")
	hasDecls(t, "narrow .hol-ranges", atRule(t, css, narrow, ".hol-ranges"), "grid-column: 1 / -1;")
	// G2: a week group's "N lectures" stays whole (TestR4WeekGroupCount)
	hasDecls(t, ".lt-group-n", cssRule(t, css, ".lt-group-n"), "white-space: nowrap;")
	// D1: "Temperature" fits the 51px inspector rail; a longer label ends in "…"
	hasDecls(t, ".rail-k", cssRule(t, css, ".rail-k"), "max-width: 100%;", "overflow: hidden;", "text-overflow: ellipsis;", "white-space: nowrap;", "font-size: 9px;", "letter-spacing: 0;")
	// D4: the glossary janitor's meta: the "·" lives in the column gap, so a
	// part that starts a line has its "·" outside the (clipping) box
	hasDecls(t, ".jan-meta", cssRule(t, css, ".jan-meta"), "flex-wrap: wrap;", "gap: 2px 15px;", "overflow: hidden;")
	hasDecls(t, ".jan-meta > span", cssRule(t, css, ".jan-meta > span"), "position: relative;")
	hasDecls(t, ".jan-meta separator", cssRule(t, css, ".jan-meta > span + span::before"), `content: "·";`, "position: absolute; left: -15px; width: 15px;")
}
