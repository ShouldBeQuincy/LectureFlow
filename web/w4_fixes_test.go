// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// atBodies returns the bodies of the at-rules whose head is exactly head
// (for example "@container (max-width: 480px)").
func atBodies(t *testing.T, css, head string) []string {
	t.Helper()
	var out []string
	open := head + " {"
	for rest := css; ; {
		i := strings.Index(rest, open)
		if i < 0 {
			break
		}
		rest = rest[i+len(open):]
		depth, end := 1, -1
		for k, c := range rest {
			if c == '{' {
				depth++
			} else if c == '}' {
				depth--
				if depth == 0 {
					end = k
					break
				}
			}
		}
		if end < 0 {
			t.Fatalf("unclosed %s", open)
		}
		out = append(out, rest[:end])
		rest = rest[end:]
	}
	return out
}

// atRule returns the declarations of sel in the first body of head that
// has a rule for it.
func atRule(t *testing.T, css, head, sel string) string {
	t.Helper()
	for _, b := range atBodies(t, css, head) {
		if strings.Contains(b, sel+" {") {
			return cssRule(t, b, sel)
		}
	}
	t.Fatalf("style.css has no %q rule in %s", sel, head)
	return ""
}

// W4 (T8 general 1): at phone width (a browser; the desktop app is at
// least 880 wide) the title column of the lecture rows and of the
// dashboard's lists was squeezed to 0. Narrow containers put the title on
// a line of its own; the wide layouts are unchanged. No browser runs here:
// the declarations are pinned, the layout is looked at in the browser.
func TestW4NarrowLists(t *testing.T) {
	css := readWeb(t, "style.css")
	// wide layouts as they were
	hasDecls(t, ".lt", cssRule(t, css, ".lt"), "--lt-cols: 22px var(--lt-kind-w) minmax(240px, 1fr) minmax(140px, 200px) 196px;")
	hasDecls(t, ".ip-list", cssRule(t, css, ".ip-list"), "grid-template-columns: max-content minmax(0, max-content) minmax(0, 1fr) minmax(190px, 42%);")
	hasDecls(t, "main", cssRule(t, css, "main"), "padding: 24px 32px 64px;")

	// course page lecture rows
	const lt = "@container (max-width: 480px)"
	hasDecls(t, "narrow .lt", atRule(t, css, lt, ".lt"), "--lt-cols: 22px minmax(4em, 1fr) auto;")
	hasDecls(t, "narrow .lrow-kind", atRule(t, css, lt, ".lrow-kind"), "grid-column: 2; grid-row: 1;")
	hasDecls(t, "narrow .lrow-end", atRule(t, css, lt, ".lrow-end"), "grid-column: 3; grid-row: 1;", "flex-wrap: wrap;")
	hasDecls(t, "narrow .lrow-main", atRule(t, css, lt, ".lrow-main"), "grid-column: 2 / -1; grid-row: 2;")
	hasDecls(t, "narrow .lrow-pipe", atRule(t, css, lt, ".lrow-pipe"), "grid-column: 2 / -1; grid-row: 3;")
	hasDecls(t, "narrow .lrow-sub", atRule(t, css, lt, ".lrow-sub"), "flex-wrap: wrap;")
	// W4c (R34b A): a long warning chip wraps and shows all of its text
	// (W4b cut it with an ellipsis: a mouse user could not read the rest)
	chip := atRule(t, css, lt, ".lrow-sub > .chip")
	hasDecls(t, "narrow .lrow-sub > .chip", chip, "max-width: 100%;", "overflow: hidden;", "height: auto;", "white-space: normal;", "overflow-wrap: anywhere;")
	if strings.Contains(chip, "text-overflow") {
		t.Error("the narrow warning chip is cut with an ellipsis again")
	}
	// W4b (R34): the moved columns' heads are hidden from view only (a
	// screen reader's table keeps them), out of the grid's flow
	head := atRule(t, css, lt, ".lt-head > :nth-child(n+3)")
	hasDecls(t, "narrow .lt-head cells", head, "display: block;", "position: absolute;", "width: 1px; height: 1px;", "overflow: hidden;", "clip: rect(0 0 0 0);")
	if strings.Contains(head, "display: none") {
		t.Error("the narrow column heads are taken from screen readers too (display: none)")
	}
	// W4c (R34b): the progress head of the 760px layout is hidden from view
	// only as well (the desktop app at 1280 with both side panels open)
	head4 := atRule(t, css, "@container (max-width: 760px)", ".lt-head > :nth-child(4)")
	hasDecls(t, "760 .lt-head progress", head4, "position: absolute;", "width: 1px; height: 1px;", "overflow: hidden;", "clip: rect(0 0 0 0);")
	if strings.Contains(head4, "display: none") {
		t.Error("the 760px progress head is taken from screen readers too (display: none)")
	}
	// W4c (R34b): the actions keep their DOM order (the Tab order), so no
	// order on the review chip (W4b put it after the menu)
	for _, b := range atBodies(t, css, lt) {
		if strings.Contains(b, ".chip-review") && strings.Contains(b, "order:") {
			t.Error("the narrow review chip is moved out of its DOM (Tab) order again")
		}
	}
	// the narrow rules come after the 760px ones (same specificity: the later wins)
	if strings.Index(css, lt+" {\n  .lt {") < strings.Index(css, "@container (max-width: 760px) {\n  .lt {") {
		t.Error("the 480px lecture-row rules come before the 760px ones")
	}

	// dashboard: in progress and recently finished
	const dash = "@container (max-width: 460px)"
	hasDecls(t, "narrow .ip-main", atRule(t, css, dash, ".ip-main"), "grid-column: 1 / -1; grid-row: 2;")
	hasDecls(t, "narrow .ip-pipe", atRule(t, css, dash, ".ip-pipe"), "grid-row: 3;")
	hasDecls(t, "narrow .rd-title", atRule(t, css, dash, ".rd-title"), "grid-column: 1 / -1; grid-row: 2;")
	// R7 (T23 general 2): the figures leave the last column to the actions
	hasDecls(t, "narrow .rd-meta", atRule(t, css, dash, ".rd-meta"), "grid-column: 1 / 5; grid-row: 3;")

	// lecture page: the figures two per line, the transcript text on full lines
	hasDecls(t, "narrow .lec-stats", atRule(t, css, "@container (max-width: 480px)", ".lec-stats"), "grid-template-columns: repeat(2, minmax(0, 1fr));")
	if strings.Index(css, "@container (max-width: 480px) {\n  .lec-stats") < strings.Index(css, ".lec-stat {") {
		t.Error("the narrow .lec-stat padding comes before the base rule and loses to it")
	}
	// W4c (user 2026-11-11): the names under the lecture page's bar are
	// hidden at phone width; they are aria-hidden (each segment is named by
	// its aria-label, its card headed by the stage), so only their look goes
	hasDecls(t, "narrow .seg-name", atRule(t, css, "@container (max-width: 480px)", ".pipe-lg .seg-name"), "display: none;")
	pipe := readWeb(t, "pipeline.js")
	for _, want := range []string{
		"h('span', { class: 'seg-name', 'aria-hidden': 'true' }, U.stageName(st.stage))",
		"'aria-label': segLabel(st, opts),",
		"h('strong', { class: 'card-stage' }, U.stageName(st.stage))",
	} {
		if !strings.Contains(pipe, want) {
			t.Errorf("pipeline.js lacks %q: hiding .seg-name may now hide a stage's name", want)
		}
	}
	// W4b: a long figure breaks at its spaces ("551K /" "42.7K"), no "…"
	hasDecls(t, "narrow .lec-stat-v", atRule(t, css, "@container (max-width: 480px)", ".lec-stat-v"), "white-space: normal;")
	// W4b: a material's state and menu wrap under its file name
	hasDecls(t, "narrow .mat-top", atRule(t, css, "@container (max-width: 480px)", ".mat-top"), "flex-wrap: wrap;")
	hasDecls(t, "narrow .mat-main", atRule(t, css, "@container (max-width: 480px)", ".mat-main"), "flex-basis: 10em;")
	if strings.Index(css, "@container (max-width: 480px) {\n  .mat-top") < strings.Index(css, ".mat-main {") {
		t.Error("the narrow .mat-main rule comes before the base rule and loses to it")
	}
	const tx = "@container (max-width: 440px)"
	hasDecls(t, "narrow .tx-card", atRule(t, css, tx, ".tx-card, .tx-card.is-en-only"), "--tx-cols: auto 22px minmax(0, 1fr) 30px;")
	hasDecls(t, "narrow .s-en", atRule(t, css, tx, ".srow > .s-en"), "grid-column: 1 / -1; grid-row: 2;")
	hasDecls(t, "narrow .s-zh", atRule(t, css, tx, ".srow > .s-zh"), "grid-column: 1 / -1; grid-row: 3;")

	// the page's gutter at phone width
	hasDecls(t, "narrow main", atRule(t, css, "@media (max-width: 640px)", "main"), "padding: 16px 16px 48px;")
}

// W4 (T8 general 2, T6 #15): the settings page's section list no longer
// covers half a phone-width window; the diagnostics hints wrap their link
// under the text; wide blocks no longer widen the page.
func TestW4NarrowSettingsAndDiagnostics(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, "narrow .set-toc-list", atRule(t, css, "@container (max-width: 560px)", ".set-toc-list"), "flex-wrap: nowrap;", "overflow-x: auto;")
	hasDecls(t, "narrow .set-toc-list > li", atRule(t, css, "@container (max-width: 560px)", ".set-toc-list > li"), "flex: none;")
	hasDecls(t, ".sys-hint", cssRule(t, css, ".sys-hint"), "flex-wrap: wrap;")
	hasDecls(t, ".sys-hint > span", cssRule(t, css, ".sys-hint > span"), "flex: 1 1 12em;", "min-width: 0;")
	hasDecls(t, ".sys-cfg-grid", cssRule(t, css, ".sys-cfg-grid"), "minmax(min(300px, 100%), 1fr)")
	hasDecls(t, ".set-title-preview", cssRule(t, css, ".set-title-preview"), "max-width: min(520px, 100%);")
	// W4b: lecture kinds (the name over code and group) and a provider's
	// tariff tag (under its label) in a phone-width settings column
	const sm = "@container set-main (max-width: 480px)"
	hasDecls(t, "narrow .kind-row", atRule(t, css, sm, ".kind-row"), "grid-template-columns: auto minmax(0, 1fr) auto;")
	hasDecls(t, "narrow .kind-name", atRule(t, css, sm, ".kind-name"), "grid-column: 1 / 3; grid-row: 1;")
	hasDecls(t, "narrow .kind-code", atRule(t, css, sm, ".kind-code"), "grid-column: 1; grid-row: 2;")
	hasDecls(t, "narrow .kind-group", atRule(t, css, sm, ".kind-group"), "grid-column: 2; grid-row: 2;")
	hasDecls(t, "narrow .pv-tariff", atRule(t, css, sm, ".pv-fact.pv-tariff"), "flex-wrap: wrap;")
	if strings.Index(css, sm+" {") < strings.Index(css, ".kind-row {") {
		t.Error("the narrow .kind-row rule comes before the base rule and loses to it")
	}
}

// W4 (T6 #9, R28): the long explanations of the output limit and the batch
// size describe their fields; the provider dialog's key hints sit outside
// the field's label (heard once, as the description, not in the name too).
func TestW4SettingsDescriptions(t *testing.T) {
	src := readWeb(t, "view_settings.js")
	for _, want := range []string{
		"const hintId = (what) => `rl-${r.role}-${what}-hint`;",
		// T18: the row's error first while it is about the field (errFirst)
		"'aria-describedby': errFirst('max_tokens', hintId('max_tokens')),",
		"h('span', { class: 'rl-sub', id: hintId('max_tokens') }, t('settings.roles.max_tokens_hint')),",
		"'aria-describedby': errFirst('batch', `${hintId('batch_default')} ${hintId('batch')}`),",
		"h('span', { class: 'dim', id: hintId('batch_default'), dataset: fk('batch_default') }",
		"h('span', { class: 'rl-sub', id: hintId('batch') }, t('settings.roles.batch_hint')),",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}
	if strings.Contains(src, "h('label', { class: 'dlg-field span-2' }, h('span', { class: 'dlg-label' }, icon('key-round'") {
		t.Error("the key hints are inside the key field's label again")
	}
}

// W4 (T8 detail 3, detail 7): compound warnings and the tariff summary,
// with the real catalogs (testdata/w4_text_test.mjs).
func TestW4TextJoin(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/w4_text_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "w4_text_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "w4 text checks: ok") {
		t.Fatalf("w4_text_test.mjs: %v\n%s", err, out)
	}
}
