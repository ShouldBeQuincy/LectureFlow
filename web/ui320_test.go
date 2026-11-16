// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 3.2.0 UI fixes from the sweep before the release (P1, P2, L1, L12, L13,
// L18). The behaviour is checked by testdata/ui320_test.mjs on a DOM
// stand-in (double clicks, the countdown, Esc on the hover card, the
// provider hint); the checks here cover what it cannot load (the views,
// app.js, the style sheet) and the native DOM calls of every file.

// P1: the diagnostics page printed "[object HTMLElement],[object
// HTMLElement]" while it loaded: the native append (prepend, before, after,
// replaceWith, replaceChildren) turns an array argument into its string.
// Arrays go to dom.js append(el, …) or are spread. Flagged: an argument
// that is (or whose branch of ?:, ||, &&, ?? is) an array literal or ends in
// a call of an array method (map, filter, …).
var nativeInsert = map[string]bool{"append": true, "prepend": true, "before": true, "after": true, "replaceWith": true, "replaceChildren": true}

var arrayMethods = map[string]bool{"map": true, "filter": true, "flat": true, "flatMap": true, "concat": true, "slice": true, "splice": true, "sort": true, "reverse": true, "toSorted": true, "toReversed": true}

func arrayArgs(src string) []string {
	_, toks := lexJS(src)
	opens := map[string]bool{"(": true, "[": true, "{": true}
	closes := map[string]bool{")": true, "]": true, "}": true}
	isP := func(k int, v string) bool { return k < len(toks) && toks[k].kind == 'p' && toks[k].val == v }
	// the branches of an argument at its top level, split at ?, :, ||, &&, ??
	branches := func(arg []jsToken) [][]jsToken {
		var out [][]jsToken
		depth, start := 0, 0
		for k, tk := range arg {
			if tk.kind != 'p' {
				continue
			}
			switch {
			case opens[tk.val]:
				depth++
			case closes[tk.val]:
				depth--
			case depth == 0 && (tk.val == "?" || tk.val == ":" || tk.val == "||" || tk.val == "&&" || tk.val == "??"):
				out = append(out, arg[start:k])
				start = k + 1
			}
		}
		return append(out, arg[start:])
	}
	risky := func(b []jsToken) bool {
		if len(b) == 0 {
			return false
		}
		if b[0].kind == 'p' && b[0].val == "[" {
			return true
		}
		// the last top-level member access: x.map(…) at the very end
		depth, last := 0, -1
		for k, tk := range b {
			if tk.kind != 'p' {
				continue
			}
			switch {
			case opens[tk.val]:
				depth++
			case closes[tk.val]:
				depth--
			case depth == 0 && (tk.val == "." || tk.val == "?."):
				last = k
			}
		}
		if last < 0 || last+2 >= len(b) || b[last+1].kind != 'i' || !arrayMethods[b[last+1].val] || b[last+2].kind != 'p' || b[last+2].val != "(" {
			return false
		}
		d := 0
		for k := last + 2; k < len(b); k++ {
			if b[k].kind == 'p' && opens[b[k].val] {
				d++
			} else if b[k].kind == 'p' && closes[b[k].val] {
				d--
				if d == 0 {
					return k == len(b)-1
				}
			}
		}
		return false
	}
	var bad []string
	for i := 0; i+2 < len(toks); i++ {
		if !(isP(i, ".") || isP(i, "?.")) || toks[i+1].kind != 'i' || !nativeInsert[toks[i+1].val] || !isP(i+2, "(") {
			continue
		}
		depth := 0
		var arg []jsToken
		check := func() {
			if len(arg) > 0 && !(arg[0].kind == 'p' && arg[0].val == "...") {
				for _, b := range branches(arg) {
					if risky(b) {
						bad = append(bad, fmt.Sprintf("line %d: .%s(…) gets an array", toks[i+1].line, toks[i+1].val))
						break
					}
				}
			}
			arg = nil
		}
		for k := i + 3; k < len(toks); k++ {
			tk := toks[k]
			if tk.kind == 'p' && depth == 0 && (tk.val == ")" || tk.val == ",") {
				check()
				if tk.val == ")" {
					break
				}
				continue
			}
			if tk.kind == 'p' && opens[tk.val] {
				depth++
			} else if tk.kind == 'p' && closes[tk.val] {
				depth--
			}
			arg = append(arg, tk)
		}
	}
	return bad
}

func TestNoArrayToNativeInsert(t *testing.T) {
	// the checker itself
	for src, want := range map[string]int{
		"el.append(error ? h('p') : [h('a'), h('b')]);":           1,
		"el.append([a, b]);":                                      1,
		"el.replaceChildren(items.map((x) => h('li', null, x)));": 1,
		"box.replaceChildren(...(rs ? steps(rs) : []));":          0,
		"el.append(...list.map(row));":                            0,
		"el.append(h('p', null, [a, b]), list.join(', '));":       0,
		"el.append(names.map(f).join(' · '));":                    0,
		"el.append(a, b.filter(Boolean).length);":                 0,
		"fd.append('file', file, file.name);":                     0,
		"append(el, [a, b]);":                                     0,
		"x?.after(cond && [a]);":                                  1,
	} {
		if got := len(arrayArgs(src)); got != want {
			t.Errorf("%q: %d findings, want %d", src, got, want)
		}
	}
	for _, f := range sourceFiles(t) {
		if !strings.HasSuffix(f.path, ".js") {
			continue
		}
		for _, b := range arrayArgs(f.src) {
			t.Errorf("%s %s (spread it, or use dom.js append)", f.path, b)
		}
	}
}

// L1: the inspector is drawn again once the catalog is loaded (it was drawn
// when app.js loaded, before it).
func TestInspectorDrawnAfterCatalog(t *testing.T) {
	src := readWeb(t, "app.js")
	re := regexp.MustCompile(`await setLocale\(locale\);\s*translateStatic\(\);\s*(?://[^\n]*\s*)*inspector\.render\(\);`)
	if !re.MatchString(src) {
		t.Error("boot() does not call inspector.render() right after setLocale and translateStatic")
	}
}

// P2, L12, L13, L18 in the views and the style sheet; the behaviour in
// testdata/ui320_test.mjs.
func TestUI320Fixes(t *testing.T) {
	want := map[string][]string{
		// P2: the row's and the notices' buttons go through actions.js
		// (T25 X1: the update asks first, then goes through actions.js as before)
		"lecture_list.js": {"done(A.launchLectures([lec]))", "done(A.stageAction(lec, failed.stage, 'retry', true))", "done(updateConfirmed([lec]))", "((await confirmUpdate(ls, { named: list.length > 1 })) ? A.updateLectures(ls) : [])",
			// L12: the row's status counts down
			"if (U.backoffCounting(lec.stages))\n      everySecond(statusEl, () => {"},
		"view_lecture.js": {"run(A.taskRetry(lec, 'stream'))", "run(A.taskRetry(lec, 'title'))", "run(A.stageAction(lec, st.stage, 'retry', true))",
			"if (U.backoffCounting(lec.stages))\n        everySecond(sumText, () => {"},
		"view_dashboard.js": {"if (U.backoffCounting(lec.stages))\n      everySecond(statusEl, () => {"},
		"actions.js": {"export function once(key, run) {", "return once(`launch:${idsKey(list)}`", "return once(key, async () => {",
			"if (key && choosing.has(key)) return null;"},
		"pipeline.js": {"if (acting.has(k)) return;", "everySecond(wrap, tick)", "stopEverySecond(wrap)", "wrap.dismiss = () => {",
			"btn.focus({ preventScroll: true });", "hoveredWrap.dismiss();", "U.stageWaitingText(st, sys, res)"},
		"util.js": {"const wait = (st) => stageWaitingText(st, opt.sys, opt.res);"},
		"view_settings.js": {"endpointParts(t(v === 'anthropic' ? 'settings.provider.endpoint_anthropic' : 'settings.provider.endpoint_openai', { base_url: mark }), mark, base.value)",
			"typeNote.replaceChildren(...endpointNote(type));", "typeNote.replaceChildren(...endpointNote(v));"},
		"style.css": {".seg-wrap.is-dismissed .seg-card, .seg-wrap.is-dismissed:hover .seg-card, .seg-wrap.is-dismissed:focus-within .seg-card { display: none; }"},
	}
	for f, list := range want {
		src := readWeb(t, f)
		for _, w := range list {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %q", f, w)
			}
		}
	}
	for f, gone := range map[string]string{
		"view_lecture.js":  "api.lectureStageAction(",
		"pipeline.js":      ".blur()",
		"view_settings.js": "base_url: '{base_url}'",
	} {
		if strings.Contains(readWeb(t, f), gone) {
			t.Errorf("%s still has %q", f, gone)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/ui320_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "ui320_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "ui 3.2.0 checks: ok") {
		t.Fatalf("ui320_test.mjs: %v\n%s", err, out)
	}
}
