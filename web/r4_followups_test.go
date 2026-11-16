// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"regexp"
	"strings"
	"testing"
)

// mustContain fails for each snippet missing from the file.
func mustContain(t *testing.T, file string, snippets ...string) {
	t.Helper()
	src := readWeb(t, file)
	for _, s := range snippets {
		if !strings.Contains(src, s) {
			t.Errorf("%s: missing %q", file, s)
		}
	}
}

// C1 (T6 #14): the transcript's review mark button, which expands a
// sentence's details, names the details it controls while they are shown.
func TestTranscriptMarkControlsDetails(t *testing.T) {
	mustContain(t, "view_transcript.js",
		"'aria-expanded': open ? 'true' : 'false',\n                // C1: the details the button shows (only there while open)\n                'aria-controls': open ? detailId(s.idx) : null,",
		"{ class: 's-detail', id: detailId(s.idx) },",
		"return `s-detail-${idx}`;",
	)
}

// C4 (T6 #10, R6 A3): the retry dialog's output-limit error asks for a
// positive whole number in English and Hindi too (0 is a whole number and
// is refused); the English manual_conflict error quotes the choice as the
// dialog labels it.
func TestRetryDialogTexts(t *testing.T) {
	en := loadCatalog(t, "i18n/en.json")
	hi := loadCatalog(t, "i18n/hi.json")
	str := func(c catalog, k string) string {
		s, _ := c[k].(string)
		if s == "" {
			t.Fatalf("%s: missing", k)
		}
		return s
	}
	if s := str(en, "common.model_choice.max_tokens_bad"); !strings.Contains(s, "positive whole number") {
		t.Errorf("en max_tokens_bad does not ask for a positive whole number: %q", s)
	}
	if s := str(hi, "common.model_choice.max_tokens_bad"); !hiAsksPositiveWhole(s) {
		t.Errorf("hi max_tokens_bad does not ask for a positive whole number: %q", s)
	}
	manual := str(en, "common.model_choice.manual")
	if s := str(en, "errors.model_choice.manual_conflict"); !strings.Contains(s, "“"+manual+"”") {
		t.Errorf("en manual_conflict does not quote the choice %q: %q", manual, s)
	}
	mustContain(t, "retry_model.js", "return n >= 1 && n <= MAX_TOKENS_LIMIT ? n : NaN;", "p.type === 'anthropic' && !!p.supports_thinking && thinking === 'off' && thinkingFloorsAtLow(model)")
	mustContain(t, "actions.js", "floorNote.textContent = on ? t('settings.roles.thinking_floor_note', { model: model.value.trim() }) : '';")
}

// R6-E (replacing C5's steps): "data folder path too long" and "close to
// the limit" ({max} = 259 − 94, the deepest file a Python worker opens in
// the lecture and slide folders; internal/api TestDataDirPathTexts pins
// the rule) say that transcription, fixing sentence breaks (R66 F5) and
// slide text extraction may be unable to write their result files on a
// computer without Windows long path support, the files about 90
// characters deeper. Past {hard} (258 − 72) "too long to start" says both
// (R66 F1): no process starts in the folders about 70 characters deeper,
// long path support or not, and within {hard} but past {max} the files
// may still fail. The limit is derived from the code, not measured: no
// "measured", and the old "from 2 characters more" steps are gone. The
// source texts, zh-CN and en.
func TestDataDirPathTexts(t *testing.T) {
	const (
		toStart = "errors.check.data_dir_path_too_long_to_start"
		tooLong = "errors.check.data_dir_path_too_long"
		near    = "errors.check.data_dir_path_near_limit"
	)
	type texts struct{ want, bad []string }
	for loc, keys := range map[string]map[string]texts{
		"zh-CN": {
			toStart: {
				want: []string{"超过了 {hard}", "约 70 个字符深的文件夹", "不论是否开启 Windows 长路径支持", "转写、修复断句与课件文本抽取都无法启动", "超过 {max} 时", "这几步也可能写不了结果文件"},
				bad:  []string{"实测", "多出 2 个字符", "这两步"},
			},
			tooLong: {
				want: []string{"超过了 {max}", "转写、修复断句与课件文本抽取", "约 90 个字符", "没有开启 Windows 长路径支持", "这几步可能写不了结果文件"},
				bad:  []string{"实测", "多出 2 个字符", "约 70 个字符", "这两步", "无法启动", "{hard}"},
			},
			near: {
				want: []string{"接近上限 {max}", "转写、修复断句与课件文本抽取", "约 90 个字符", "没有开启 Windows 长路径支持", "这几步就可能写不了结果文件"},
				bad:  []string{"实测", "多出 2 个字符", "约 70 个字符", "这两步", "无法启动", "{hard}"},
			},
		},
		"en": {
			toStart: {
				want: []string{"more than {hard}", "folders about 70 characters deeper", "cannot start, whether or not Windows long path support is turned on", "transcription, fixing sentence breaks and slide text extraction", "beyond {max}", "may be unable to write their result files"},
				bad:  []string{"measured", "2 characters more", "these two steps"},
			},
			tooLong: {
				want: []string{"more than {max}", "transcription, fixing sentence breaks and slide text extraction", "about 90 characters deeper", "Windows long path support is not turned on", "these steps may be unable to write their result files"},
				bad:  []string{"measured", "2 characters more", "about 70 characters", "these two steps", "cannot start", "{hard}"},
			},
			near: {
				want: []string{"close to the limit of {max}", "transcription, fixing sentence breaks and slide text extraction", "about 90 characters deeper", "Windows long path support is not turned on", "these steps may be unable to write their result files"},
				bad:  []string{"measured", "2 characters more", "about 70 characters", "these two steps", "cannot start", "{hard}"},
			},
		},
	} {
		cat := loadCatalog(t, "i18n/"+loc+".json")
		for key, x := range keys {
			s, _ := cat[key].(string)
			for _, w := range x.want {
				if !strings.Contains(s, w) {
					t.Errorf("%s %s: %q lacks %q", loc, key, s, w)
				}
			}
			for _, b := range x.bad {
				if strings.Contains(s, b) {
					t.Errorf("%s %s: %q still says %q", loc, key, s, b)
				}
			}
		}
	}
}

// hiAsksPositiveWhole: the Hindi text asks for a whole number (पूर्ण संख्या,
// पूर्णांक or पूरी संख्या) that is positive, said either way: a standalone
// 1 (Latin or Devanagari digit, "1 या उससे बड़ी") or धनात्मक. The hi
// reviewer's point: the wording is the translator's, not this test's.
var hiStandaloneOne = regexp.MustCompile(`(^|[^0-9०-९])[1१]([^0-9०-९]|$)`)

func hiAsksPositiveWhole(s string) bool {
	whole := strings.Contains(s, "पूर्ण संख्या") || strings.Contains(s, "पूर्णांक") || strings.Contains(s, "पूरी संख्या")
	return whole && (hiStandaloneOne.MatchString(s) || strings.Contains(s, "धनात्मक"))
}

func TestHiAsksPositiveWhole(t *testing.T) {
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"आउटपुट सीमा के लिए धनात्मक पूर्ण संख्या डालें, या ख़ाली छोड़ें", true},                  // the present hi.json
		{"आउटपुट सीमा के लिए धनात्मक पूर्ण संख्या (1 या उससे बड़ी) डालें, या ख़ाली छोड़ें", true}, // the compromise
		{"आउटपुट सीमा में 1 या उससे बड़ी पूर्ण संख्या डालें, या इसे ख़ाली छोड़ें", true},          // the reviewer's
		{"आउटपुट सीमा में १ या उससे बड़ा पूर्णांक डालें", true},                                   // Devanagari digit
		{"आउटपुट सीमा के लिए पूर्ण संख्या डालें, या ख़ाली छोड़ें", false},                         // whole, not positive
		{"आउटपुट सीमा में 1 या उससे बड़ी संख्या डालें", false},                                    // a number, not whole
		{"आउटपुट सीमा में 10 या उससे बड़ी पूर्ण संख्या डालें", false},                             // 10 is not a standalone 1
	} {
		if got := hiAsksPositiveWhole(c.s); got != c.want {
			t.Errorf("%q: %v, want %v", c.s, got, c.want)
		}
	}
}

// C2 (R19 §5): the glossary markers' cards hold a button, so they are no
// tooltips; testdata/a11y_test.mjs checks the behaviour (Esc, the focus,
// aria-expanded), this pins the declarations and the CSS that hides a card
// Esc closed.
func TestGlossaryMarkCardIsNoTooltip(t *testing.T) {
	src := readWeb(t, "glossary.js")
	if strings.Contains(src, "role: 'tooltip'") || strings.Contains(src, "'aria-describedby': id") {
		t.Error("glossary.js: a marker card (with a button) is still a tooltip or the marker's description")
	}
	mustContain(t, "glossary.js",
		"if (head) head.setAttribute('id', `${id}-t`);",
		"const card = h('div', { class: 'mk-card', id, role: 'group', 'aria-labelledby': head ? `${id}-t` : null, 'aria-label': head ? null : title }, inner);",
		"'aria-controls': id, 'aria-expanded': 'false',",
		"title: t('glossary.term.locked'),", "title: t('glossary.term.janitor_title'),")
	mustContain(t, "style.css", ".mk-wrap.is-dismissed .mk-card, .mk-wrap.is-dismissed:hover .mk-card, .mk-wrap.is-dismissed:focus-within .mk-card { display: none; }")
}

// T16 D3 (R49, R58): at phone width the lecture row's menu (⋯) went alone
// onto a second line. The review chip, the downloads and the menu are one
// element that does not wrap inside, after the primary action in the DOM;
// when the row is too narrow for all of them the unit goes to the second
// line as a whole (plain wrap), so the order seen is the DOM (Tab and
// reading) order of R34b. Wide layouts are unchanged (measured in the
// browser: the same boxes at 1280).
func TestLectureRowTailKeepsMenuWithChip(t *testing.T) {
	src := readWeb(t, "lecture_list.js")
	i := strings.Index(src, "{ class: 'lrow-end', role: 'cell' },")
	if i < 0 {
		t.Fatal("lecture_list.js: no .lrow-end cell")
	}
	end := src[i:]
	order := []string{"primaryAction(lec),", "{ class: 'lrow-tail' },", "{ class: 'chip chip-review',", "h('span', { class: 'lrow-dlmenu' }, dl, more),"}
	at := 0
	for _, s := range order {
		j := strings.Index(end[at:], s)
		if j < 0 {
			t.Fatalf("lecture_list.js: the row's actions lack %q in the order %q", s, order)
		}
		at += j + len(s)
	}
	css := readWeb(t, "style.css")
	// T18c: wide too, the actions wrap in their fixed-width column rather
	// than run out to the left (ru), the tail shrinking to the cell
	hasDecls(t, ".lrow-tail", cssRule(t, css, ".lrow-tail"), "display: inline-flex;", "flex: 0 1 auto;", "flex-wrap: wrap;", "justify-content: flex-end;", "gap: 4px;", "max-width: 100%;", "min-width: 0;")
	hasDecls(t, ".lrow-end", cssRule(t, css, ".lrow-end"), "display: flex;", "justify-content: flex-end;", "flex-wrap: wrap;", "row-gap: 4px;")
	lt := "@container (max-width: 480px)"
	hasDecls(t, "narrow .lrow-end", atRule(t, css, lt, ".lrow-end"), "flex-wrap: wrap;")
	// T18 N1: the downloads and the menu never split, wide or narrow; at
	// phone width the tail shrinks to the cell and wraps inside it
	hasDecls(t, ".lrow-dlmenu", cssRule(t, css, ".lrow-dlmenu"), "display: inline-flex;", "flex: none;", "gap: 4px;")
	hasDecls(t, "narrow .lrow-tail", atRule(t, css, lt, ".lrow-tail"), "flex: 0 1 auto;", "flex-wrap: wrap;", "justify-content: flex-end;", "max-width: 100%;", "min-width: 0;")
	for _, b := range atBodies(t, css, lt) {
		if strings.Contains(b, ".lrow-dlmenu") && strings.Contains(b, "wrap") {
			t.Error("the narrow rules let the downloads and the menu split")
		}
	}
	for _, bad := range lrowReorders(css) {
		t.Errorf("style.css: %s shows the row's parts out of their DOM (Tab) order", bad)
	}
}

// reorderDecl: a declaration that shows boxes in another order than the
// DOM's (order, reversed wrap or direction, grid-auto-flow dense). "order"
// as a property name only (not "border:").
var reorderDecl = regexp.MustCompile(`(?:^|[\s;{])(?:order\s*:|flex-wrap\s*:\s*wrap-reverse|flex-direction\s*:\s*(?:row|column)-reverse|flex-flow\s*:[^;]*-reverse|grid-auto-flow\s*:[^;]*dense)`)

// lrowActionSel: the selector names the row itself, its actions cell, the
// actions' tail unit, its downloads + menu unit or the review chip in any
// of its compounds (R58b: a
// rule on their children, ".lrow-end > .btn { order: 2 }", reorders the
// actions as well). Whole class names only: .lrow-subtitle (moved under
// the title below 1100px on purpose) is not .lrow.
var lrowClassRe = regexp.MustCompile(`\.([A-Za-z0-9_-]+)`)

func lrowActionSel(sel string) bool {
	for _, m := range lrowClassRe.FindAllStringSubmatch(sel, -1) {
		switch m[1] {
		case "lrow", "lrow-end", "lrow-tail", "lrow-dlmenu", "chip-review":
			return true
		}
	}
	return false
}

// lrowReorders lists the rules of a lecture row and its actions (see
// lrowActionSel; wide or in an at-rule) that reorder their boxes:
// "selector { declaration }". (The title line's subtitle, moved under the
// title below 1100px on purpose, is not one of them.)
func lrowReorders(css string) []string {
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	var out []string
	for _, m := range regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1) {
		sel := strings.TrimSpace(m[1])
		if !lrowActionSel(sel) {
			continue
		}
		body := " " + m[2]
		if loc := reorderDecl.FindStringIndex(body); loc != nil {
			d := body[loc[0]:]
			if j := strings.IndexByte(d, ';'); j >= 0 {
				d = d[:j]
			}
			out = append(out, sel+" { "+strings.TrimSpace(strings.TrimLeft(d, " \t\r\n;{"))+" }")
		}
	}
	return out
}

// The guard itself: it finds each way of reordering, and not "border:".
func TestLrowReorderGuard(t *testing.T) {
	for _, c := range []struct {
		css  string
		want bool
	}{
		{"@container (max-width: 480px) {\n  .lrow-end { flex-wrap: wrap-reverse; }\n}", true},
		{".lrow-tail { order: 2; }", true},
		{".lrow .chip-review{order:-1}", true},
		{".lrow-end { flex-direction: row-reverse; }", true},
		{".lrow-end { flex-flow: row wrap-reverse; }", true},
		{".lrow { grid-auto-flow: row dense; }", true},
		{".lrow-end { flex-wrap: wrap; border: 1px solid red; }", false},
		{".other { order: 2; }", false},
		{".lrow-subtitle { order: 1; }", false},
		{".lrow.is-child { flex-direction: row-reverse; }", true},
		{".lt .lrow > .lrow-end { order: 1; }", true},
		{"@container (max-width: 480px) {\n  .lrow-end > .btn { order: 2; }\n}", true},
		{".lrow-tail .icon-btn { order: 1; }", true},
		{".lrow-dlmenu { flex-direction: row-reverse; }", true},
	} {
		if got := len(lrowReorders(c.css)) > 0; got != c.want {
			t.Errorf("%q: found %v, want %v", c.css, got, c.want)
		}
	}
}
