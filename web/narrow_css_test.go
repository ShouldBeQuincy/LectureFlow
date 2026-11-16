// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"regexp"
	"strings"
	"testing"
)

// cssRule returns the declarations of the first rule whose selector list is
// exactly sel, searched in css (a style sheet or the body of an at-rule).
func cssRule(t *testing.T, css, sel string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(sel) + `\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("style.css has no rule %q", sel)
	}
	return m[1]
}

// mediaBody returns the body of the @media block with the given condition.
func mediaBody(t *testing.T, css, cond string) []string {
	t.Helper()
	var out []string
	head := "@media " + cond + " {"
	for rest := css; ; {
		i := strings.Index(rest, head)
		if i < 0 {
			break
		}
		rest = rest[i+len(head):]
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
			t.Fatalf("unclosed %s", head)
		}
		out = append(out, rest[:end])
		rest = rest[end:]
	}
	return out
}

func hasDecls(t *testing.T, where, decls string, want ...string) {
	t.Helper()
	norm := strings.Join(strings.Fields(decls), " ")
	for _, w := range want {
		if !strings.Contains(norm, w) {
			t.Errorf("%s lacks %q (has %q)", where, w, norm)
		}
	}
}

// T6 walkthrough 2 (and T7): at phone width the stage card (330px, hung
// from its segment) ran off either side of the window. Up to 640px the
// lecture page's large bar gives the card its own width (at least 240px),
// and the small bars (lecture rows, glossary, materials, often at the right
// of a row) give it the width of the enclosing content card, right under
// the segment; wider windows keep the card as it was.
func TestNarrowStageCard(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".seg-card", cssRule(t, css, ".seg-card"), "position: absolute;", "left: -4px;", "width: 330px;", "max-width: 80vw;")
	hasDecls(t, ".seg-card-end", cssRule(t, css, ".seg-card-end"), "left: auto; right: -4px;")
	hasDecls(t, ".card", cssRule(t, css, ".card"), "position: relative;")
	found := false
	for _, body := range mediaBody(t, css, "(max-width: 640px)") {
		if !strings.Contains(body, ".seg-card") {
			continue
		}
		found = true
		hasDecls(t, "narrow .pipe-lg", cssRule(t, body, ".pipe-lg"), "position: relative;")
		hasDecls(t, "narrow .pipe-lg .seg-wrap", cssRule(t, body, ".pipe-lg .seg-wrap"), "position: static;")
		hasDecls(t, "narrow .pipe-lg .seg-card", cssRule(t, body, ".pipe-lg .seg-card"), "left: 0;", "width: 100%;", "min-width: min(240px, calc(100vw - 32px));", "max-width: none;")
		hasDecls(t, "narrow .pipe-lg .seg-card-end", cssRule(t, body, ".pipe-lg .seg-card-end"), "left: auto; right: 0;")
		hasDecls(t, "narrow small .seg-wrap", cssRule(t, body, ".pipe:not(.pipe-lg) .seg-wrap"), "position: static;", "display: block;")
		hasDecls(t, "narrow small .seg-card", cssRule(t, body, ".pipe:not(.pipe-lg) .seg-card"), "top: auto;", "bottom: auto;", "left: 0;", "right: 0;", "width: auto;", "max-width: calc(100vw - 32px);")
		hasDecls(t, "narrow small .seg-up card", cssRule(t, body, ".pipe:not(.pipe-lg) .seg-up .seg-card"), "translate: 0 calc(-100% - 20px);")
		if regexp.MustCompile(`(?m)^\s*\.(pipe|seg-card) \{`).MatchString(body) {
			t.Error("the narrow rules position every bar's card from the bar's left edge again")
		}
	}
	if !found {
		t.Error("style.css has no narrow (max-width: 640px) rules for the stage card")
	}
}

// R28 S1: a button kept focusable while busy (aria-disabled="true" instead
// of disabled) looks like a disabled one and takes no pointer events (no
// hover, no click). With the keyboard focus (T7) it is not faded, so the
// ring keeps its strength, but drawn as an inactive grey button.
func TestAriaDisabledButton(t *testing.T) {
	css := readWeb(t, "style.css")
	hasDecls(t, ".btn:disabled", cssRule(t, css, ".btn:disabled"), "opacity: 0.5;")
	hasDecls(t, ".btn[aria-disabled]", cssRule(t, css, `.btn[aria-disabled="true"]`), "opacity: 0.5;", "pointer-events: none;")
	hasDecls(t, ".btn[aria-disabled]:focus-visible", cssRule(t, css, `.btn[aria-disabled="true"]:focus-visible`), "opacity: 1;", "background: var(--bg-sunken);", "border-color: var(--border-1);", "color: var(--text-3);", "box-shadow: none;")
}

// T6 walkthrough 3 and 4: the failure notice's error showed one line at
// any width, and none at phone width, where the heading ran under the
// Retry button. The error wraps under its heading when less than 14em are
// left, shows up to three lines, and the button takes its own line when
// the text would get less than 14em.
func TestNoticeErrorWraps(t *testing.T) {
	css := readWeb(t, "style.css")
	err := cssRule(t, css, ".lec-notice-err")
	hasDecls(t, ".lec-notice-err", err, "flex: 1 1 14em;", "min-width: 0;", "display: -webkit-box;", "-webkit-box-orient: vertical;", "-webkit-line-clamp: 3;", "line-clamp: 3;", "text-overflow: ellipsis;")
	if strings.Contains(err, "nowrap") {
		t.Error(".lec-notice-err is still cut to one line (white-space: nowrap)")
	}
	hasDecls(t, "notice text with an error", cssRule(t, css, ".lec-notice-text:has(> .lec-notice-err)"), "flex-wrap: wrap;")
	// T7: a cancelled notice's error stays one line beside its heading
	// above phone width; a failed one keeps up to three lines
	wide := strings.Join(mediaBody(t, css, "(min-width: 641px)"), "\n")
	hasDecls(t, "wide cancelled notice text", cssRule(t, wide, ".lec-notice.is-cancelled .lec-notice-text"), "flex-wrap: nowrap;")
	hasDecls(t, "wide cancelled notice error", cssRule(t, wide, ".lec-notice.is-cancelled .lec-notice-err"), "-webkit-line-clamp: 1;", "line-clamp: 1;")
	if strings.Contains(wide, "is-failed") {
		t.Error("a failed notice's error is cut to one line above phone width")
	}
	hasDecls(t, "notice heading with an error", cssRule(t, css, ".lec-notice-text:has(> .lec-notice-err) > b"), "flex: 0 1 auto;", "min-width: 0;")
	hasDecls(t, "failed notice", cssRule(t, css, ".lec-notice.is-failed, .lec-notice.is-cancelled"), "flex-wrap: wrap;")
	hasDecls(t, "failed notice text", cssRule(t, css, ".lec-notice.is-failed > .lec-notice-text, .lec-notice.is-cancelled > .lec-notice-text"), "flex: 1 1 14em;")
	hasDecls(t, "failed notice button", cssRule(t, css, ".lec-notice.is-failed > .btn, .lec-notice.is-cancelled > .btn"), "margin-left: auto;")
}
