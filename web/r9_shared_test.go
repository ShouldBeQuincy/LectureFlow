// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Round 9, the shared web modules (dom.js, util.js, actions.js,
// update_confirm.js, menu.js, i18n.js; the r9-web-shared line). The
// behaviour is checked by testdata/r9_shared_test.mjs on the DOM stand-in of
// r7_dom.mjs; the style rules below, as measured in the built-in browser.
func TestR9Shared(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r9_shared_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r9_shared_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r9 shared checks: ok") {
		t.Fatalf("r9_shared_test.mjs: %v\n%s", err, out)
	}
}

// WH-02 (T28 G3): in a settings column under 316px a role's temperature
// slider is left out, so the number box stays in its column, clear of the
// thinking level (measured in the built-in browser: at 360 the number box
// 159-227 under the select 191-275, 36px, became 93-161; at 414 9px, now
// none; rows as high as before; 768 and 1280 unchanged). The wider layouts
// are as they were.
func TestR9SettingsTempNarrow(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, "narrow slider", atRule(t, css, "@container set-main (max-width: 320px)", ".rl-temp-ctl > .set-range"), "display: none;")
	hasDecls(t, ".rl-temp-ctl", cssRule(t, css, ".rl-temp-ctl"), "display: flex; align-items: center; gap: 10px; height: 32px;")
	hasDecls(t, ".rl-tnum", cssRule(t, css, ".rl-cell .rl-tnum"), "width: 68px; flex: none;")
	hasDecls(t, ".set-range", cssRule(t, css, ".set-range"), "flex: 1; min-width: 56px;")
	hasDecls(t, "two columns", atRule(t, css, "@container set-main (max-width: 780px)", ".rl-row"), "grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);")
	if strings.Contains(strings.Join(atBodies(t, css, "@container set-main (max-width: 780px)"), "\n"), "set-range") {
		t.Error("style.css: the slider is changed for every two-column width (768 and 1280 with the inspector open have room for it)")
	}
	if !strings.Contains(readWeb(t, "view_settings.js"), "h('span', { class: 'rl-temp-ctl' }, range, tNum)") {
		t.Error("view_settings.js: the temperature cell is no longer the slider and the number box in .rl-temp-ctl")
	}
}

// WH-03 (T28 G5): every term's "⋯" (td.td-end) stays at the right edge of
// the table's scroll box, opaque over the columns under it in every state
// of its row; its header sticks on both edges, above the cells (measured:
// 1280 with the inspector, box 281-887, table 680: the "⋯" at 928-956 for
// 0 of 48 terms in view, now 854-882 for 48 of 48, elementFromPoint on
// it; the same at 768 and 360, where the table still scrolls in its box).
func TestR9GlossaryActionsSticky(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, "sticky actions", cssRule(t, css, ".terms-grouped thead th:last-child, .terms-grouped td.td-end"), "position: sticky; right: 0;")
	hasDecls(t, "the corner header", cssRule(t, css, ".terms-grouped thead th:last-child"), "z-index: 2;")
	hasDecls(t, "opaque cell", cssRule(t, css, ".terms-grouped td.td-end"), "background-color: var(--bg-card);")
	hasDecls(t, "hovered row", cssRule(t, css, ".terms tbody tr:hover > td.td-end, .terms tbody tr:focus-within > td.td-end"), "background-image: linear-gradient(var(--bg-hover), var(--bg-hover));")
	hasDecls(t, "drop target", cssRule(t, css, ".tg.is-drop td.td-end"), "background: var(--accent-soft);")
	hasDecls(t, "busy row", cssRule(t, css, ".term-row.is-busy td.td-end"), "opacity: 1;")
	hasDecls(t, "busy row content", cssRule(t, css, ".term-row.is-busy td.td-end > *"), "opacity: 0.55;")
	// the header row sticks on top (the group headers do not), the table
	// keeps its width and its own sideways scroll, the drag handle its place
	hasDecls(t, "header", cssRule(t, css, ".terms th, .checks th, .table th"), "position: sticky; top: 0; z-index: 1;")
	hasDecls(t, "group header", cssRule(t, css, ".terms .tg-hd th"), "position: static;")
	hasDecls(t, "table width", cssRule(t, css, ".terms-grouped"), "table-layout: fixed; min-width: 680px;")
	hasDecls(t, "scroll box", cssRule(t, css, ".terms-table-wrap"), "overflow: auto;")
	if !regexp.MustCompile(`\.td-grip \{[^}]*cursor: grab;`).MatchString(css) {
		t.Error("style.css: the drag handle lost its rule")
	}
	src := readWeb(t, "glossary.js")
	if !strings.Contains(src, "h('td', { class: 'td-end' }, more)") || !strings.Contains(src, "h('th', null, h('span', { class: 'visually-hidden' }, t('glossary.col.actions')))") {
		t.Error("glossary.js: the actions column is no longer the last th and td.td-end (the sticky rules select on them)")
	}
}

// WH-04 (T28 G6, R86 L4; R90 L2, R90b L6): the page's toasts show the
// newest three, two in a window under 1250px high or up to 480px wide, one
// under 640px high or narrow and under 970px high: about half the window
// at most with the longest notices in ru (198px each at 440px wide; three
// took 609px of 1024 at 768x1024, two 481 of 896 at 414x896 with the
// earlier 1000 / 820). The older ones are hidden, not removed (measured
// with six notices that stay: at 360x740 one, 237px of 740, was six over
// 1225px; closing it shows the one before; an error toast hidden behind
// newer ones comes back; a wizard still shows four).
func TestR9ToastLimit(t *testing.T) {
	css := readWeb(t, "style.css")
	if !regexp.MustCompile(`(?m)^:not\(\.wiz\) > \.toasts > \.toast:nth-last-child\(n\+4\) \{ display: none; \}`).MatchString(css) {
		t.Error("style.css: the page's toasts are not limited to the newest three")
	}
	// R90 L2: by the window's height too (a low, wide window showed three);
	// R90b L6: three only from 1250px high, as three ru notices take 610px
	if !strings.Contains(strings.Join(mediaBody(t, css, "(max-height: 1249px), (max-width: 480px)"), "\n"), ":not(.wiz) > .toasts > .toast:nth-last-child(n+3) { display: none; }") {
		t.Error("style.css: a window under 1250px high or up to 480px wide does not show the newest two")
	}
	// R90b L6: a narrow window shows two only from 970px high (two ru
	// notices took 481px at 414 wide, narrower than the 440px of 404)
	if !strings.Contains(strings.Join(mediaBody(t, css, "(max-height: 639px), (max-width: 480px) and (max-height: 969px)"), "\n"), ":not(.wiz) > .toasts > .toast:nth-last-child(n+2) { display: none; }") {
		t.Error("style.css: a low window, or a narrow one under 970px high, does not show the newest one")
	}
	// the earlier limits are gone (not kept beside the new ones)
	for _, old := range []string{"(max-height: 999px), (max-width: 480px)", "(max-width: 480px) and (max-height: 819px)"} {
		if strings.Contains(css, old) {
			t.Errorf("style.css still limits the toasts by %s", old)
		}
	}
	// the wizard's own limit, and the toasts' box, as before
	hasDecls(t, "wizard", cssRule(t, css, ".dlg.wiz > .toasts > .toast:nth-last-child(n+5)"), "display: none;")
	hasDecls(t, ".toasts", cssRule(t, css, ".toasts"), "position: fixed; right: 20px; bottom: 20px;", "flex-direction: column;")
	// hidden, not removed: dom.js still removes a toast only by its close
	// button or its timer
	dom := readWeb(t, "dom.js")
	if !strings.Contains(dom, "onclick: () => el.remove() }, icon('x', { size: 14 })") || !strings.Contains(dom, "el._timer = setTimeout(() => el.remove(), 4000);") {
		t.Error("dom.js: toasts are removed some other way than their close button or timer")
	}
}

// WH-06 (T28 S1, convention C1): no scroll anchor is taken in the
// dashboard, the second line behind WD-01 (measured on this line's branch,
// without WD-01: a cancelled "update all" with the focus in the dashboard
// moved the page from scrollY 1000 to 808 and 400 to 208; with the rule it
// stays at 1000, 2264 and 400). No other page's anchoring changes.
func TestR9DashboardNoScrollAnchor(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".page-dashboard", cssRule(t, css, ".page-dashboard"), "overflow-anchor: none;")
	if n := strings.Count(css, "overflow-anchor"); n != 1 {
		t.Errorf("style.css sets overflow-anchor %d times; only the dashboard's page has it", n)
	}
	if !strings.Contains(readWeb(t, "view_dashboard.js"), "h('div', { class: 'page page-dashboard' })") {
		t.Error("view_dashboard.js: the dashboard's page is no longer div.page-dashboard")
	}
}

// WH-08 (T28 suggestion 6): a course page's week title breaks Chinese only
// at its spaces and after the range's "–" (measured at 360, 92px: "第 1 周
// · 9月28 / 日–10月4日" became "第 1 周 · / 9月28日– / 10月4日 2 节", zh-TW
// alike; ru and en line for line as before; one line at 1280).
func TestR9WeekTitleKeepAll(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".lt-group-t", atRuleless(t, css, ".lt-group-t", "word-break"), "min-width: 0; word-break: keep-all; overflow-wrap: break-word;")
	hasDecls(t, "count", cssRule(t, css, ".lt-group-n"), "white-space: nowrap;")
	hasDecls(t, "block title", cssRule(t, css, ".lt-group-t"), "display: block;")
}

// WH-09 (T28 suggestion 7): in a narrow dashboard card the review list's
// kind takes the rest of the first line up to the count and the arrow (the
// title keeps its line: C154c's way took it from 150 to 106px), and a
// recently finished row's "N to review" gets a line of its own under the
// title, before the figures and actions (measured at 360: the kind 36 ->
// 44px, its title 150 as before; the recent title with a badge 94 -> 182px
// zh-CN, 83 -> 182 ru, the row 79 -> 98px; at 414: 90 -> 98, 148 -> 236;
// nothing overlaps, nothing leaves its row).
func TestR9DashboardNarrowReview(t *testing.T) {
	css := readWeb(t, "style.css")
	const dash = "@container (max-width: 460px)"
	hasDecls(t, "review kind", atRule(t, css, dash, ".lgrid-review .lref-tag"), "grid-column: 2 / 4; grid-row: 1;")
	hasDecls(t, "review title", atRule(t, css, dash, ".lgrid-review .todo-link-title"), "grid-column: 1 / -1; grid-row: 2;")
	hasDecls(t, "review count", cssRule(t, css, ".lgrid-review .todo-num"), "grid-column: 4; grid-row: 1;")
	hasDecls(t, "review arrow", cssRule(t, css, ".lgrid-review .todo-go"), "grid-column: 5; grid-row: 1;")
	hasDecls(t, "recent title", atRule(t, css, dash, ".rd-title"), "grid-column: 1 / -1; grid-row: 2;")
	hasDecls(t, "badge line", atRule(t, css, dash, ".rd-row.has-review .rd-badge"), "grid-column: 1 / -1; grid-row: 3; justify-self: start;")
	hasDecls(t, "figures after it", atRule(t, css, dash, ".rd-row.has-review .rd-meta, .rd-row.has-review .rd-end"), "grid-row: 4;")
	if strings.Contains(strings.Join(atBodies(t, css, dash), "\n"), ".rd-row.has-review .rd-title") {
		t.Error("style.css: a recent row with a badge narrows its title again")
	}
	if !strings.Contains(readWeb(t, "view_dashboard.js"), "lref(r.course_code, r),\n                  h('span', { class: 'todo-link-title' }, r.lecture_title),") {
		t.Error("view_dashboard.js: a review row is no longer code and kind (lref), then its title")
	}
}

// WH-12 (convention C4): a glossary row that WV-05 marks .flash is
// highlighted as a slide file's and a provider's rows are, on its cells
// (the sticky "⋯" cell ending on its opaque colour); every .flash rule has
// its reduced-motion branch (looked at in the built-in browser: the six
// cells start at accent-soft, the "⋯" cell ends at bg-card).
func TestR9TermRowFlash(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".term-row.flash", cssRule(t, css, ".term-row.flash > td"), "animation: lf-flash 1.8s var(--ease);")
	hasDecls(t, "the sticky cell", cssRule(t, css, ".term-row.flash > td.td-end"), "animation-name: lf-flash-solid;")
	if !strings.Contains(css, "@keyframes lf-flash-solid { 0%, 45% { background-color: var(--accent-soft); } 100% { background-color: var(--bg-card); } }") {
		t.Error("style.css: no lf-flash-solid keyframes ending on the cell's own colour")
	}
	for sel, want := range map[string]string{
		".mat-row.flash":       "@media (prefers-reduced-motion: reduce) { .mat-row.flash { animation: none;",
		".pv-row.flash":        "@media (prefers-reduced-motion: reduce) { .pv-row.flash { animation: none;",
		".term-row.flash > td": "@media (prefers-reduced-motion: reduce) { .term-row.flash > td { animation: none; background: var(--accent-soft); } }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css: %s has no reduced-motion branch (%s)", sel, want)
		}
	}
	if n := len(regexp.MustCompile(`(?m)^[^@\n]*\.flash[^{\n]*\{[^}]*animation:`).FindAllString(css, -1)); n != 3 {
		t.Errorf("style.css has %d animated .flash rules, want 3 (each with a reduced-motion branch)", n)
	}
	if !strings.Contains(readWeb(t, "glossary.js"), "class: 'term-row' + (term.enabled ? '' : ' term-disabled')") {
		t.Error("glossary.js: a term's row is no longer tr.term-row")
	}
}

// R90 (seen in its scan): in a narrow card the buttons right of the title
// wrap within the card and a long label inside its button (ru settings,
// "Провайдеры" at 360: the two buttons 93-430 in a card 76-292, the page
// 430 wide; now 93-275 on two lines, the page 360). Wider cards are as
// they were.
func TestR9NarrowCardTools(t *testing.T) {
	css := readWeb(t, "style.css")
	const narrow = "@container (max-width: 480px)"
	hasDecls(t, "narrow tools", atRule(t, css, narrow, ".card-hd > .card-tools"), "flex-wrap: wrap; justify-content: flex-end; min-width: 0; max-width: 100%;")
	hasDecls(t, "narrow tool buttons", atRule(t, css, narrow, ".card-hd > .card-tools > .btn"), "max-width: 100%; height: auto; min-height: 26px; white-space: normal; text-align: center;")
	hasDecls(t, "narrow head", atRule(t, css, narrow, ".card-hd"), "flex-wrap: wrap; row-gap: 8px;")
	hasDecls(t, ".card-tools", cssRule(t, css, ".card-tools"), "display: flex; align-items: center; gap: 8px;")
}

// modifierProblems lists what is wrong with the placeholder modifiers
// ({name:lc}) of one catalog value (R90 L3): the source catalogs (zh-CN, en)
// use none; a placeholder that the code finds by its literal text ('{free}',
// '{link}': split or replaced by a node, not by t()) or one of a key whose
// placeholders all become nodes (tNodes) cannot have one, t() would not
// fill it and it would show as written.
func modifierProblems(loc, key string, v any, literal, nodeKeys map[string]bool) []string {
	var texts []string
	switch x := v.(type) {
	case string:
		texts = []string{x}
	case map[string]any:
		for _, s := range x {
			if str, ok := s.(string); ok {
				texts = append(texts, str)
			}
		}
	}
	var out []string
	for _, s := range texts {
		for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
			if m[2] == "" {
				continue
			}
			switch {
			case loc == sourceLocale || loc == "en":
				out = append(out, fmt.Sprintf("%s.json %s: %s (zh-CN and en use no modifiers; :lc is for the translators' templates)", loc, key, m[0]))
			case nodeKeys[key]:
				out = append(out, fmt.Sprintf("%s.json %s: %s (its placeholders become nodes, tNodes)", loc, key, m[0]))
			case literal[m[1]]:
				out = append(out, fmt.Sprintf("%s.json %s: %s (the code finds {%s} by its text)", loc, key, m[0], m[1]))
			}
		}
	}
	return out
}

// R90 L3 (WH-10): {name:lc} only where t() fills it, and not in zh-CN / en.
func TestR9LcModifierGuards(t *testing.T) {
	literal := map[string]bool{}
	nodeKeys := map[string]bool{}
	litRe := regexp.MustCompile("['\"`]\\{(\\w+)\\}['\"`]")
	nodesRe := regexp.MustCompile(`\btNodes\(\s*'([\w.]+)'`)
	for _, f := range sourceFiles(t) {
		if !strings.HasSuffix(f.path, ".js") {
			continue
		}
		src, _ := lexJS(f.src)
		for _, m := range litRe.FindAllStringSubmatch(src, -1) {
			literal[m[1]] = true
		}
		for _, m := range nodesRe.FindAllStringSubmatch(src, -1) {
			nodeKeys[m[1]] = true
		}
	}
	// the ones known today are found (resources.js '{free}', view_system.js
	// '{link}', glossary.js tNodes('glossary.foot.keys', …))
	if !literal["free"] || !literal["link"] || !nodeKeys["glossary.foot.keys"] {
		t.Fatalf("literal placeholders %v, tNodes keys %v: the scan misses the known ones", literal, nodeKeys)
	}
	// the guard itself
	for _, c := range []struct {
		loc, key string
		v        any
		want     int
	}{
		{"ru", "common.summary.pending", "Расшифровка: {wait:lc}", 0},
		{"ru", "resources.remote.ram_free", "свободно {free:lc} из {total}", 1},
		{"ru", "glossary.foot.keys", "{f2:lc} — изменить", 1},
		{"ru", "x.y", map[string]any{"one": "{n} {link:lc}", "other": "{n}"}, 1},
		{"zh-CN", "x.y", "等待：{wait:lc}", 1},
		{"en", "x.y", map[string]any{"one": "{n} {stages:lc}", "other": "{n}"}, 1},
		{"en", "x.y", "Waiting: {wait}", 0},
	} {
		if got := modifierProblems(c.loc, c.key, c.v, literal, nodeKeys); len(got) != c.want {
			t.Errorf("modifierProblems(%s, %s, %v) = %v, want %d problems", c.loc, c.key, c.v, got, c.want)
		}
	}
	// the catalogs
	locs := append([]string{sourceLocale}, otherLocales(t)...)
	seen := map[string]bool{}
	for _, loc := range locs {
		if seen[loc] {
			continue
		}
		seen[loc] = true
		cat := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		for k := range cat.textKeys() {
			for _, p := range modifierProblems(loc, k, cat[k], literal, nodeKeys) {
				t.Error(p)
			}
		}
	}
	if !seen["en"] {
		t.Error("the en catalog was not checked")
	}
}

// WH-10 (convention C5): the catalogs' placeholder check
// (i18n_check_test.go paramSets) counts {name:lc} as {name}, the param set
// of every language being compared by name, and reports a modifier t()
// does not know. t() itself is checked by r9_shared_test.mjs.
func TestR9PlaceholderModifiers(t *testing.T) {
	sets := func(v any) string {
		t.Helper()
		forms, err := paramSets(v)
		if err != nil {
			t.Fatalf("paramSets(%v): %v", v, err)
		}
		var parts []string
		for form, ps := range forms {
			parts = append(parts, form+"="+strings.Join(ps, ","))
		}
		sort.Strings(parts)
		return strings.Join(parts, ";")
	}
	if a, b := sets("Расшифровка: {wait}"), sets("Расшифровка: {wait:lc}"); a != b || a != "=wait" {
		t.Errorf("{wait} gives %q, {wait:lc} %q; want the same, =wait", a, b)
	}
	if got := sets("{a:lc} — {b} ({a})"); got != "=a,b" {
		t.Errorf("{a:lc} and {a} with {b}: %q", got)
	}
	if got := sets(map[string]any{"one": "{n} — {stages:lc}", "other": "{n}: {stages}"}); got != "one=n,stages;other=n,stages" {
		t.Errorf("plural forms: %q", got)
	}
	for _, v := range []any{"{x:foo}", "{x:LC}", map[string]any{"other": "{n:upper}"}} {
		if _, err := paramSets(v); err == nil {
			t.Errorf("paramSets(%v): no error for an unknown modifier", v)
		}
	}
	// t() reads the same syntax (i18n.js interpolate)
	src := readWeb(t, "i18n.js")
	if !strings.Contains(src, `s.replace(/\{(\w+)(?::(\w+))?\}/g,`) || !strings.Contains(src, "if (mod === 'lc')") {
		t.Error("i18n.js: interpolate no longer reads {name} and {name:lc}")
	}
}
