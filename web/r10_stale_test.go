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

// Round 10, the stale reasons (the r10-web-stale line, C190). The behaviour
// is checked by testdata/r10_stale_test.mjs on the DOM stand-in of
// r7_dom.mjs with the real catalogs; the catalogs' rules below.
func TestR10Stale(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r10_stale_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r10_stale_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r10 stale checks: ok") {
		t.Fatalf("r10_stale_test.mjs: %v\n%s", err, out)
	}
}

// staleNoLc are the placeholders of the stale texts that take no {x:lc}
// (C190 (2)): a reason, or the reasons joined, carries file names and texts
// recorded before v2, which a :lc of the whole would change. A reason in
// mid-sentence is lowered by the code, one at a time, through
// stale.reason_inline ({reason:lc} in Russian), with those texts kept out.
// The keys under stale.reason. ("stale.reason.") hold their params the same
// way: a file name, a list, a recorded text.
var staleNoLc = map[string][]string{
	"stale.need_update":         {"reasons"},
	"course.confirm.update_why": {"reasons"},
	"stale.card":                {"reason"},
	"stale.reason.":             {"filename", "added", "removed", "text"},
}

// staleLcProblems lists the :lc placeholders that staleNoLc forbids in the
// value v of key.
func staleLcProblems(loc, key string, v any) []string {
	var params []string
	for k, ps := range staleNoLc {
		if key == k || (strings.HasSuffix(k, ".") && strings.HasPrefix(key, k)) {
			params = append(params, ps...)
		}
	}
	if len(params) == 0 {
		return nil
	}
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
			for _, p := range params {
				if m[1] == p && m[2] != "" {
					out = append(out, fmt.Sprintf("%s.json %s: %s (the code lowers a reason in mid-sentence itself, stale.reason_inline; a modifier here would reach the file names and recorded texts)", loc, key, m[0]))
				}
			}
		}
	}
	return out
}

// C190 (2): no :lc where a stale text would reach file names.
func TestR10StaleNoLcOnNames(t *testing.T) {
	// the guard itself
	for _, c := range []struct {
		key  string
		v    any
		want int
	}{
		{"course.confirm.update_why", "Почему нужно обновить: {reasons:lc}.", 1},
		{"course.confirm.update_why", "Почему нужно обновить: {reasons}.", 0},
		// {stages} holds stage names only (stageName: a catalog word, else
		// the stage's id), never a file name: its :lc is the translator's (R95)
		{"stale.need_update", "{reasons}. Нужно обновить: {stages:lc}", 0},
		{"stale.need_update", "{reasons:lc}. Нужно обновить: {stages:lc}", 1},
		{"stale.card", "{reason:lc}. Результат…", 1},
		{"stale.reason.material_changed", "Слайды изменились: «{filename:lc}»", 1},
		{"stale.reason.materials_changed", map[string]any{"other": "добавлены: {added:lc}; убраны: {removed}"}, 1},
		{"stale.reason.legacy", "{text:lc}", 1},
		{"stale.reason_inline", "{reason:lc}", 0},
		{"waiting.upstream", "Ждёт предыдущего этапа: {stage:lc}", 0},
	} {
		if got := staleLcProblems("ru", c.key, c.v); len(got) != c.want {
			t.Errorf("staleLcProblems(%s, %v) = %v, want %d problems", c.key, c.v, got, c.want)
		}
	}
	for _, loc := range append([]string{sourceLocale}, otherLocales(t)...) {
		cat := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		for k := range cat.textKeys() {
			for _, p := range staleLcProblems(loc, k, cat[k]) {
				t.Error(p)
			}
		}
	}
}

// quoteNextToName is a quotation mark right before or after the file name
// of material_changed or the file-name lists of materials_changed: util.js
// quotes each name itself (settings.quote), so a template's own marks would
// show twice.
var quoteNextToName = regexp.MustCompile(`[«»„“”"‘’'「」『』]\{(filename|added|removed)(?::\w+)?\}|\{(filename|added|removed)(?::\w+)?\}[«»„“”"‘’'「」『』]`)

// C190 (1), R94/R95: no catalog quotes a slide file name again, alone
// (material_changed) or in a list (materials_changed).
func TestR10StaleNamesQuotedOnce(t *testing.T) {
	// the guard itself
	for s, want := range map[string]bool{
		"Изменился набор слайдов (добавлены: {added}; убраны: {removed})": false,
		"適用嘅簡報有變（新增 {added}；{removed} 唔再適用）":                              false,
		"Изменились слайды {filename}":                                    false,
		"课件已变化：{filename}":                                                false,
		"Slides changed (added “{added}”)":                                true,
		"添加了「{added}」":                                                    true,
		"убраны: «{removed:lc}»":                                          true,
		"Слайды изменились: «{filename}»":                                 true,
		`Slides changed: "{filename}"`:                                    true,
	} {
		if got := quoteNextToName.MatchString(s); got != want {
			t.Errorf("quoteNextToName(%q) = %v, want %v", s, got, want)
		}
	}
	locs := append([]string{sourceLocale}, otherLocales(t)...)
	seen := 0
	for _, loc := range locs {
		cat := loadCatalog(t, filepath.Join(catalogDir, loc+".json"))
		for k := range cat.textKeys() {
			if k != "stale.reason.material_changed" && !strings.HasPrefix(k, "stale.reason.materials_changed") {
				continue
			}
			seen++
			if s, _ := cat[k].(string); quoteNextToName.MatchString(s) {
				t.Errorf("%s.json %s: %q quotes a slide file name that the code quotes already (util.js staleReasonText, settings.quote)", loc, k, s)
			}
		}
	}
	// material_changed and materials_changed{,_added,_removed,_plain} each
	if seen < 5*len(locs) {
		t.Errorf("only %d material keys checked in %d catalogs", seen, len(locs))
	}
}
