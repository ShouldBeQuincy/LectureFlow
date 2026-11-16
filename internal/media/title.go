// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTitleRunes caps a stored lecture title (test-media I8): a file name or
// a platform's page title can be very long, and the title is shown in one
// line of the lists and is the stem of every export's file name (which
// stages.SafeFileName cuts further, to 100 characters).
const MaxTitleRunes = 200

// CleanTitle is a lecture title as it is stored (test-media I8): without
// bidirectional control characters (StripBidi; they would make an export's
// file name show another extension), on one line (control characters such
// as a newline or tab, and the line and paragraph separators, become
// spaces), at most MaxTitleRunes characters (cut on a character boundary,
// never inside one; by character, not by grapheme, so a flag can lose its
// second regional indicator), and without surrounding white space (also
// none left by the cut, nor a dangling zero-width joiner). The API (upload,
// download import, PATCH) and the file-name rule (stages) both store titles
// through it.
func CleanTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, StripBidi(s))
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= MaxTitleRunes {
		return s
	}
	s = string([]rune(s)[:MaxTitleRunes])
	return strings.TrimRightFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '\u200d' })
}
