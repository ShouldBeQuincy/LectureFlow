// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"strings"
	"testing"
)

// W5 (T11, the browser recheck): layout fixes at phone width. No browser
// runs here: the declarations are pinned, the layout is looked at in the
// browser. (The behaviour fixes of W5 are checked by testdata/a11y_test.mjs.)
func TestW5NarrowFixes(t *testing.T) {
	css := readWeb(t, "style.css")

	// general 1: the dashboard's "N to review" has its own place (left to
	// the grid it covered the code and kind): round 9 WH-09 (T28 suggestion
	// 7) gives it a line of its own under the title, which keeps its whole
	// line (at the end of the title's line it left the title 94px of 182 at
	// 360). The wide layout is as it was.
	const dash = "@container (max-width: 460px)"
	hasDecls(t, "narrow .rd-badge", atRule(t, css, dash, ".rd-badge"), "grid-column: 4 / -1; grid-row: 2;")
	hasDecls(t, "narrow badge on its own line", atRule(t, css, dash, ".rd-row.has-review .rd-badge"), "grid-column: 1 / -1; grid-row: 3; justify-self: start;")
	hasDecls(t, "narrow .rd-title", atRule(t, css, dash, ".rd-title"), "grid-column: 1 / -1; grid-row: 2;")
	hasDecls(t, ".rd-list", cssRule(t, css, ".rd-list"), "grid-template-columns: max-content minmax(0, max-content) minmax(0, 1fr) max-content max-content max-content max-content max-content;")
	hasDecls(t, ".rd-badge", cssRule(t, css, ".rd-badge"), "justify-self: end;")
	// the narrow rules select on the badge's span and on the row's
	// has-review class, which the row has exactly when the badge holds a chip
	// (no :has(), which Firefox < 121 lacks; testdata/a11y_test.mjs checks
	// the rows)
	if strings.Contains(css, ":has(.chip-review)") {
		t.Error("style.css: the dashboard's narrow layout selects with :has(.chip-review) again")
	}
	dsrc := readWeb(t, "view_dashboard.js")
	if !strings.Contains(dsrc, "h('span', { class: 'rd-badge' }, review ? h('a', { class: 'chip chip-review',") {
		t.Error("view_dashboard.js: the recently finished badge is no longer a .rd-badge holding .chip-review (the narrow rules select on it)")
	}
	if !strings.Contains(dsrc, "{ class: review ? 'rd-row lgrid-row has-review' : 'rd-row lgrid-row' }") {
		t.Error("view_dashboard.js: a recently finished row with a badge no longer has the has-review class")
	}

	// general 2: a long path in a diagnostics hint (install and data
	// directories) breaks instead of running out of the hint
	hasDecls(t, ".sys-hint > span", cssRule(t, css, ".sys-hint > span"), "overflow-wrap: anywhere;", "flex: 1 1 12em;", "min-width: 0;")

	// detail 1: the narrow settings section list leaves room for the focus
	// ring inside its scroll box
	hasDecls(t, "narrow .set-toc-list", atRule(t, css, "@container (max-width: 560px)", ".set-toc-list"), "padding: 4px;", "overflow-x: auto;")
}
