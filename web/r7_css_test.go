// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"regexp"
	"strings"
	"testing"
)

// R7 (T22 S1): the lecture page's transcript search hid the rows that do
// not match (row.hidden), but `.srow { display: grid }` beat the hidden
// attribute and every row stayed. The same held for a hidden `.btn` (the
// player's chapters button while the chapters load).
func TestR7HiddenBeatsDisplay(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".srow", cssRule(t, css, ".srow"), "display: grid;")
	hasDecls(t, ".srow[hidden]", cssRule(t, css, ".srow[hidden]"), "display: none;")
	hasDecls(t, ".btn[hidden]", cssRule(t, css, ".btn[hidden]"), "display: none;")
}

// R7 (T23 severe 1): at 414 and below the player's control bar did not
// fit (394px of buttons and clock in 340 at 360): "Transcript" and "Full
// screen" ran out of the window, the speed button covered the clock. The
// bar wraps, the right group (speed … full screen) under the left one,
// flush right; wide, one line of 42px as before. The resume notice was
// placed from the stage's middle (left: 50%, so it had half the stage to
// grow in) and moved back by a transform that lf-pop replaced while it ran:
// its × was out of the window. It is centred by its margins.
func TestR7NarrowPlayerBar(t *testing.T) {
	css := readWeb(t, "style.css")
	bar := cssRule(t, css, ".pl-bar")
	hasDecls(t, ".pl-bar", bar, "display: flex;", "flex-wrap: wrap;", "min-height: 42px;")
	if strings.Contains(" "+strings.Join(strings.Fields(bar), " "), " height: 42px;") {
		t.Error(".pl-bar has a fixed height again (a wrapped second line would overflow it)")
	}
	// a group wider than its line (the left one has the 5 s back button
	// too) wraps inside rather than run the clock under the next group
	hasDecls(t, ".pl-bar-group", cssRule(t, css, ".pl-bar-group"), "display: flex; flex-wrap: wrap;", "min-width: 0;")
	hasDecls(t, "right .pl-bar-group", cssRule(t, css, ".pl-bar-group:last-child"), "margin-left: auto;")
	if !strings.Contains(readWeb(t, "view_player.js"), "h('div', { class: 'pl-bar-spacer' }),\n    h('div', { class: 'pl-bar-group' }, rateBtn,") {
		t.Error("view_player.js: the bar's last child is no longer the right group of controls (the wrapping rule selects on it)")
	}

	n := cssRule(t, css, ".pl-notice")
	hasDecls(t, ".pl-notice", n, "position: absolute; left: 0; right: 0;", "width: fit-content; max-width: calc(100% - 16px); margin: 0 auto;")
	if nn := strings.Join(strings.Fields(n), " "); strings.Contains(nn, "left: 50%") || strings.Contains(nn, "transform:") {
		t.Error(".pl-notice is placed by left: 50% and a transform again (half the stage to grow in; lf-pop replaces the transform)")
	}
	hasDecls(t, ".pl-notice-btn", cssRule(t, css, ".pl-notice-btn"), "flex: none; white-space: nowrap;", "height: 26px;")
	hasDecls(t, ".pl-notice-x", cssRule(t, css, ".pl-notice-x"), "flex: none;")
}

// R7 (T23 general 2–6): five places that did not fit 360.
func TestR7Narrow360(t *testing.T) {
	css := readWeb(t, "style.css")
	// 2: the dashboard's recently finished: beside the actions the kind was
	// 5–16px wide; it takes the rest of the first line, the actions go to
	// the end of the figures' line
	const dash = "@container (max-width: 460px)"
	hasDecls(t, "narrow recent kind", atRule(t, css, dash, ".rd-row > .lref-tag"), "grid-column: 2 / -1; grid-row: 1;")
	hasDecls(t, "narrow .rd-meta", atRule(t, css, dash, ".rd-meta"), "grid-column: 1 / 5; grid-row: 3;")
	hasDecls(t, "narrow .rd-end", atRule(t, css, dash, ".rd-end"), "grid-row: 3;")
	hasDecls(t, ".rd-end (720)", atRule(t, css, "@container (max-width: 720px)", ".rd-end"), "grid-row: 1; grid-column: 5;")

	// 3: the lecture page's two tabs: narrow, no icons, less padding, and
	// the second under the first when still too wide (ru); wide as before
	const page = "@container (max-width: 480px)"
	hasDecls(t, ".lec-tab", cssRule(t, css, ".lec-tab"), "gap: 7px;", "padding: 0 12px;")
	hasDecls(t, "narrow .lec-tabs", atRule(t, css, page, ".lec-tabs"), "flex-wrap: wrap;")
	hasDecls(t, "narrow .lec-tab", atRule(t, css, page, ".lec-tab"), "gap: 5px; padding: 0 8px;")
	hasDecls(t, "narrow .lec-tab icon", atRule(t, css, page, ".lec-tab > .icon"), "display: none;")
	if !strings.Contains(readWeb(t, "view_lecture.js"), "icon(ic, { size: 15 }),\n        label,") {
		t.Error("view_lecture.js: a lecture tab is no longer its icon followed by its label (the narrow rule hides the icon)")
	}

	// 4: the transcript search box: 46cqw (95px at 360) wide; narrow, a
	// line of its own as wide as the card
	const tx = "@container (max-width: 440px)"
	hasDecls(t, ".tx-search-input", cssRule(t, css, ".tx-search-input"), "width: 300px; max-width: 46cqw;")
	hasDecls(t, "narrow .tx-search", atRule(t, css, tx, ".tx-search"), "flex: 1 1 100%; min-width: 0;")
	hasDecls(t, "narrow .tx-search-input", atRule(t, css, tx, ".tx-search-input"), "width: 100%; max-width: none;")

	// 5: the sentence audio bar: the time went 55px out of the row; narrow,
	// under the track, as an error's message
	hasDecls(t, ".abar", cssRule(t, css, ".abar"), "grid-template-columns: auto auto minmax(80px, 1fr) auto auto auto;")
	hasDecls(t, "narrow .abar", atRule(t, css, tx, ".abar"), "grid-template-columns: auto auto minmax(0, 1fr) auto;")
	hasDecls(t, "narrow .abar-time", atRule(t, css, tx, ".abar-time"), "grid-column: 3 / -1; grid-row: 2; min-width: 0;")
	hasDecls(t, "narrow .abar-err", atRule(t, css, tx, ".abar.is-error .abar-err"), "grid-column: 1 / -1; grid-row: 2;")
	if strings.Index(css, "@container (max-width: 440px) {\n  .abar {") < strings.Index(css, ".abar-time {") {
		t.Error("the narrow audio bar rules come before the base ones and lose to them")
	}

	// 6: course cards: a 240px (208px) minimum ran out of a narrower list
	hasDecls(t, ".ccards", cssRule(t, css, ".ccards"), "grid-template-columns: repeat(auto-fill, minmax(min(208px, 100%), 1fr));")
	hasDecls(t, ".ccards-lg", cssRule(t, css, ".ccards-lg"), "grid-template-columns: repeat(auto-fill, minmax(min(240px, 100%), 1fr));")
}

// R70 (f), T23 suggestion 7: the transcript panel's "back to current
// position" was centred by left: 50% and translateX(-50%), which lf-pop's
// transform replaced while it ran (for 150ms half a button to the right,
// 1132–1305 in a panel ending at 1280). It is centred by its margins.
func TestR7BackCurrentCentred(t *testing.T) {
	css := readWeb(t, "style.css")
	b := cssRule(t, css, ".pl-back-current")
	hasDecls(t, ".pl-back-current", b, "position: absolute; left: 0; right: 0; bottom: 12px;", "width: fit-content; max-width: calc(100% - 16px); margin: 0 auto;", "animation: lf-pop")
	norm := strings.Join(strings.Fields(b), " ")
	if strings.Contains(norm, "left: 50%") || strings.Contains(norm, "transform:") {
		t.Error(".pl-back-current is placed by left: 50% and a transform again (lf-pop replaces the transform while it runs)")
	}
}

// R7 (for the lecture page's line): five inline styles move into the
// style sheet. A waiting stage's notice wraps as a failed one (with the
// button beside it the text was 0px wide at 360), and a stage card's folded
// error detail has its margin and a quiet, clickable summary.
func TestR7WaitNoticeAndErrDetail(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".lec-notice.is-wait", cssRule(t, css, ".lec-notice.is-wait"), "flex-wrap: wrap; row-gap: 6px;")
	hasDecls(t, "waiting notice text", cssRule(t, css, ".lec-notice.is-wait > .lec-notice-text"), "flex: 1 1 14em;")
	hasDecls(t, "waiting notice button", cssRule(t, css, ".lec-notice.is-wait > .btn"), "margin-left: auto;")
	if strings.Index(css, "\n.lec-notice.is-wait {") < strings.Index(css, "\n.lec-notice-text {") {
		t.Error("the waiting notice's rules come before .lec-notice-text and lose to it")
	}
	hasDecls(t, ".card-errdetail", cssRule(t, css, ".card-errdetail"), "margin: 4px 0 6px;")
	hasDecls(t, ".card-errdetail > summary", cssRule(t, css, ".card-errdetail > summary"), "cursor: pointer; color: var(--text-3);")
}

// R7 (for the lecture page's line): its last inline styles. A failed
// stage's second and third buttons sit beside the first, at the right, and
// stay at the right on a line of their own (a phone), where a label wider
// than the notice wraps in its button (360: 169px in 160); a figure's line
// under it wraps at phone width; the large bar's percentage takes the room
// a longer figure needs, on one line.
func TestR7LecturePageInlineRules(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, "second button", cssRule(t, css, ".lec-notice.is-failed > .btn + .btn"), "margin-left: 0;")
	hasDecls(t, "failed notice end", atRuleless(t, css, ".lec-notice.is-failed", "justify-content"), "justify-content: flex-end;")
	hasDecls(t, "failed notice button", cssRule(t, css, ".lec-notice.is-failed > .btn"), "max-width: 100%; height: auto; min-height: 26px;", "white-space: normal;")
	if strings.Index(css, "\n.lec-notice.is-failed > .btn + .btn {") < strings.Index(css, "\n.lec-notice.is-failed > .btn, .lec-notice.is-cancelled > .btn {") {
		t.Error("the second button's margin comes before the first's margin-left: auto and loses to it")
	}
	hasDecls(t, ".lec-stat-sub", cssRule(t, css, ".lec-stat-sub"), "white-space: nowrap;")
	hasDecls(t, "narrow .lec-stat-sub", atRule(t, css, "@container (max-width: 480px)", ".lec-stat-sub"), "white-space: normal; overflow-wrap: anywhere;")
	hasDecls(t, ".pipe-pct", cssRule(t, css, ".pipe-pct"), "width: 2.9em;")
	hasDecls(t, "large .pipe-pct", cssRule(t, css, ".pipe-lg .pipe-pct"), "width: auto; min-width: 2.9em; white-space: nowrap;")
}

// atRuleless returns the declarations of the first top-level rule for sel
// that sets prop (a selector can have several rules).
func atRuleless(t *testing.T, css, sel, prop string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(sel) + `\s*\{([^}]*)\}`)
	for _, m := range re.FindAllStringSubmatch(css, -1) {
		if strings.Contains(m[1], prop+":") {
			return m[1]
		}
	}
	t.Fatalf("style.css has no %q rule setting %s", sel, prop)
	return ""
}

// R70 (f): the dashboard's failure list had the fault of T23 general 2:
// beside Retry the kind was 5px wide at 360. It takes the rest of the first
// line and Retry goes to the title's line, whose fourth column it kept free
// anyway (the title keeps its 75px, the list is no higher).
func TestR7NarrowFailKind(t *testing.T) {
	css := readWeb(t, "style.css")
	const dash = "@container (max-width: 460px)"
	hasDecls(t, "narrow failure kind", atRule(t, css, dash, ".lgrid-fail .lref-tag"), "grid-column: 2 / -1; grid-row: 1;")
	hasDecls(t, "narrow .fail-end", atRule(t, css, dash, ".fail-end"), "grid-row: 2;")
	hasDecls(t, "narrow .fail-title", atRule(t, css, dash, ".lgrid-fail .fail-title"), "grid-column: 1 / 4; grid-row: 2;")
	hasDecls(t, ".fail-end", cssRule(t, css, ".fail-end"), "grid-column: 4; grid-row: 1 / span 2;")
	if !strings.Contains(readWeb(t, "view_dashboard.js"), "{ class: 'fail-row lgrid-row' },\n      lref(f.course_code, f),") {
		t.Error("view_dashboard.js: a failure row no longer starts with the code and kind of lref (the narrow rule places its .lref-tag)")
	}
}

// R7 (for the dashboard's line): its new classes. A reason several
// lectures failed for heads its rows with the dashed line above it, none
// between it and its first row; the way out is quiet text; the Colab
// warning sits where the calm Colab line does; roles not set up are the
// error variant of .asr-notice; an empty power-wait box takes no room; the
// way out's link is like the panel's .rc-link.
func TestR7DashboardLineClasses(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".fail-reason", cssRule(t, css, ".lgrid-fail .fail-reason"), "border-top: 1px dashed var(--border-1);")
	hasDecls(t, "first .fail-reason", cssRule(t, css, ".lgrid-fail .fail-reason:first-child"), "border-top: 0;")
	hasDecls(t, "row under its reason", cssRule(t, css, ".lgrid-fail .fail-reason + .fail-row"), "border-top: 0;")
	hasDecls(t, ".fail-row", cssRule(t, css, ".fail-row"), "border-top: 1px dashed var(--border-1);")
	hasDecls(t, ".fail-hint", cssRule(t, css, ".fail-hint"), "margin-top: 3px; color: var(--text-2);")
	if strings.Index(css, "\n.fail-hint {") < strings.Index(css, "\n.fail-meta {") {
		t.Error(".fail-hint comes before .fail-meta and loses its colour to it (the row's hint is .fail-meta.fail-hint)")
	}
	colab := regexp.MustCompile(`\.dash-colab \{[^}]*margin: (-8px 0 16px);`).FindStringSubmatch(readWeb(t, "settings_v3.css"))
	if colab == nil {
		t.Fatal("settings_v3.css: .dash-colab no longer has margin -8px 0 16px (the warning state copies it)")
	}
	hasDecls(t, ".dash-colab-warn", cssRule(t, css, ".asr-notice.dash-colab-warn"), "margin: "+colab[1]+";")
	hasDecls(t, ".dash-roles", cssRule(t, css, ".asr-notice.dash-roles"), "border-left-color: var(--err); background: var(--err-soft);")
	hasDecls(t, ".dash-roles icon", cssRule(t, css, ".asr-notice.dash-roles .asr-notice-icon"), "color: var(--err);")
	hasDecls(t, ".pw-waits:empty", cssRule(t, css, ".pw-waits:empty"), "display: none;")
	if !regexp.MustCompile(`\n\.rc-link \{([^}]*)\}`).MatchString(readWeb(t, "settings_v3.css")) {
		t.Fatal("settings_v3.css has no .rc-link rule")
	}
	// r8 WH-10 (R82 C-I): the way out's link has the class .rc-link, which
	// styles it; style.css keeps no copy of those rules for .pw-way a
	if !strings.Contains(readWeb(t, "resources.js"), "h('a', { class: 'rc-link', href: '#/settings/asr', dataset: { fkey: 'insp:power:settings' } }") {
		t.Error("resources.js: the power wait's way out is no longer an a.rc-link")
	}
	if regexp.MustCompile(`(?m)^\.pw-way a\b`).MatchString(css) {
		t.Error("style.css: .pw-way a copies .rc-link again (the link has the class)")
	}
}

// R7 (T23 general 11): at phone width the status lines ("Transcription
// queued: Waiting for a free transcription device", a failure's way out,
// a download's progress) were one line cut with "…", the rest only in a
// title a touch screen cannot show. Narrow, they wrap: the statuses up to
// three lines, the meta and the download detail two. Wide, one line as
// before. The dashboard's failure (.fail-why, which replaced .fail-err in
// the dashboard's script) shows its whole sentence at any width.
func TestR7NarrowStatusLinesWrap(t *testing.T) {
	css := readWeb(t, "style.css")
	const clamp = "display: -webkit-box; -webkit-box-orient: vertical;"
	wraps := "white-space: normal; overflow-wrap: anywhere;"
	for sel, base := range map[string]string{".lrow-status": "white-space: nowrap;", ".lrow-meta": "white-space: nowrap;", ".ip-status": "white-space: nowrap;", ".dl-detail": "white-space: nowrap;"} {
		hasDecls(t, sel, cssRule(t, css, sel), base, "text-overflow: ellipsis;")
	}
	why := cssRule(t, css, ".fail-why")
	hasDecls(t, ".fail-why", why, "background: var(--err-soft); color: var(--err);", wraps)
	if n := strings.Join(strings.Fields(why), " "); strings.Contains(n, "nowrap") || strings.Contains(n, "line-clamp") || strings.Contains(n, "text-overflow: ellipsis") {
		t.Error(".fail-why is cut again (its way out ends the sentence)")
	}
	if regexp.MustCompile(`(?m)^[^/*\n]*\.fail-err\b[^\n{]*\{`).MatchString(css) {
		t.Error("style.css styles .fail-err again (no script uses it since the dashboard shows .fail-why)")
	}

	const rows = "@container (max-width: 480px)"
	hasDecls(t, "narrow .lrow-status, .lrow-meta", atRule(t, css, rows, ".lrow-status, .lrow-meta"), clamp, "-webkit-line-clamp: 2; line-clamp: 2;", wraps)
	hasDecls(t, "narrow .lrow-status", atRule(t, css, rows, ".lrow-status"), "-webkit-line-clamp: 3; line-clamp: 3;")
	hasDecls(t, "narrow .ip-status", atRule(t, css, "@container (max-width: 560px)", ".ip-status"), clamp, "-webkit-line-clamp: 3; line-clamp: 3;", wraps)
	hasDecls(t, "narrow .dl-status", atRule(t, css, "@container (max-width: 560px)", ".dl-status"), "flex-wrap: wrap;")
	hasDecls(t, "narrow .dl-detail", atRule(t, css, "@container (max-width: 560px)", ".dl-detail"), clamp, "-webkit-line-clamp: 2; line-clamp: 2;", wraps)

	// the narrow rules come after their base rules (same specificity)
	for _, p := range [][2]string{
		{".lrow-status {", "  .lrow-status, .lrow-meta {"},
		{".ip-status {", "@container (max-width: 560px) {\n  .ip-list {"},
		{".dl-detail {", "  .dl-detail { display: -webkit-box;"},
	} {
		base, narrow := strings.Index(css, "\n"+p[0]), strings.Index(css, p[1])
		if base < 0 || narrow < 0 || narrow < base {
			t.Errorf("the narrow rule %q is missing or comes before its base rule %q", p[1], p[0])
		}
	}
}

// R7 (T23 general 12): on a touch screen the small controls were small
// targets close together (stage segments 18 × 20, icon buttons 28, a
// sentence's "⋯" 26, change marks 22, check boxes 15, the player's "change
// to review" 14 × 18, menu items 30 high). With a coarse pointer they get a
// transparent ::after reaching about 40px; what is seen keeps its size.
// The neighbours they would cover keep their own boxes; a fine pointer
// (the desktop app, a mouse) is as before.
func TestR7TouchTargets(t *testing.T) {
	css := readWeb(t, "style.css")
	const touch = "@media (pointer: coarse)"
	const small = ".lrow-end .icon-btn, .rd-end > .icon-btn, .s-more-btn, .mk, button.seg, .pl-row-pend, .lt-check > input.check"
	hasDecls(t, "touch targets", atRule(t, css, touch, small), "position: relative;")
	area := atRule(t, css, touch, ".lrow-end .icon-btn::after, .rd-end > .icon-btn::after, .s-more-btn::after, .mk::after, button.seg::after, .pl-row-pend::after, .lt-check > input.check::after")
	hasDecls(t, "touch hit area", area, `content: "";`, "position: absolute;", "inset: min(0px, calc(50% - 20px));")
	for _, drawn := range []string{"background:", "background-color:", "border:", "border-color:", "outline:", "box-shadow:"} {
		if strings.Contains(strings.Join(strings.Fields(area), " "), drawn) {
			t.Errorf("the touch hit area is drawn (%s; it is to be transparent: the controls keep their look)", drawn)
		}
	}
	// the segments sit 2px apart: theirs grows up and down only
	hasDecls(t, "touch segment", atRule(t, css, touch, "button.seg::after"), "left: -1px; right: -1px;", "top: min(0px, calc(50% - 19px));")
	hasDecls(t, "touch second icon button", atRule(t, css, touch, ".lrow-end .icon-btn + .icon-btn::after"), "left: -2px;")
	// R70 low 1: "change to review" stays in its column, the gap before it
	// and the row's padding, short of the sentence and the row's top; r8
	// WL-03: not below the mark either, where the sentence's "⋯" is now (the
	// last column 26px wide for it)
	hasDecls(t, "touch change to review", atRule(t, css, touch, ".pl-row-pend::after"), "inset: -6px -10px 0 -7px;")
	hasDecls(t, ".pl-row", cssRule(t, css, ".pl-row"), "grid-template-columns: 46px minmax(0, 1fr) 26px; gap: 2px 8px;", "padding: 7px 10px 7px 14px;")
	hasDecls(t, ".pl-row-pend", cssRule(t, css, ".pl-row-pend"), "align-self: start;", "width: 14px; height: 18px;")
	hasDecls(t, "touch neighbours", atRule(t, css, touch, ".lrow-title, .lrow-end .btn, .chip-review"), "position: relative; z-index: 1;")
	hasDecls(t, "touch grip", atRule(t, css, touch, ".lrow-grip"), "z-index: 1;")
	hasDecls(t, "touch menu item", atRule(t, css, touch, ".ctx-item"), "height: 40px;")
	// wide and with a mouse as before
	hasDecls(t, ".ctx-item", cssRule(t, css, ".ctx-item"), "height: 30px;")
	hasDecls(t, ".check", cssRule(t, css, ".check"), "width: 15px; height: 15px;")
	if !strings.Contains(css, "\n.mk { display: grid; place-items: center; width: 22px; height: 22px;") {
		t.Error("style.css: the change marks are no longer 22px (the touch hit area is measured from it)")
	}
	hasDecls(t, ".s-more-btn", cssRule(t, css, ".s-more-btn"), "width: 26px; height: 26px;")
	// none of them draws an ::after of its own the touch rule would replace
	for _, sel := range []string{".icon-btn::after", ".mk::after", ".seg::after", ".s-more-btn::after", ".pl-row-pend::after", ".check::after"} {
		if strings.Contains(css, "\n"+sel) {
			t.Errorf("style.css draws %s elsewhere; the touch hit area would replace it", sel)
		}
	}
	// the drag handle is absolutely placed (course_ux.css): z-index needs no position here
	if !strings.Contains(readWeb(t, "course_ux.css"), ".lrow-grip {\n  position: absolute;") {
		t.Error("course_ux.css: the drag handle is no longer absolutely placed (its touch z-index needs a position)")
	}
}
