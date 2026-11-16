// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

import (
	"encoding/json"
	"strings"
	"testing"
)

// SEC49 I5: U+061C ARABIC LETTER MARK is a bidi control (Bidi_Control, like
// U+200E and U+200F); the text of a remote result's sentences and words drops
// it with the other bidi controls, while ZWNJ and ZWJ stay (BD-11). The
// identifier-like strings (CleanText: the timing block's strings, probe
// fields) drop it as well, as they drop every other bidi control.
func TestValidateResultDropsArabicLetterMark(t *testing.T) {
	r := func(cs ...rune) string { return string(cs) }
	const alm = 0x061c
	var (
		arabic  = r(0x0645, 0x0631, 0x062d, 0x0628, 0x0627)         // "hello", no marks
		persian = r(0x0645, 0x06cc, 0x200c, 0x062e, 0x0648, 0x0627) // with ZWNJ
		hindi   = r(0x0915, 0x094d, 0x200d, 0x0937)                 // a half form with ZWJ
	)
	text := r(alm) + arabic + r(alm, ' ', '4', '2', alm) + " " + persian + " " + hindi
	raw, err := json.Marshal(map[string]any{
		"sentences": []any{map[string]any{
			"start_ms": 0, "end_ms": 1000, "text": text,
			"low_conf_words": []any{map[string]any{"i": 0, "w": r(alm) + arabic}},
			"words": []any{
				map[string]any{"w": arabic + r(alm), "s": 0, "e": 400},
				map[string]any{"w": persian, "s": 400, "e": 700},
				map[string]any{"w": hindi, "s": 700, "e": 1000},
			},
		}},
		"timing": map[string]any{"note": r('x', alm, 'y', 0x200d, 'z')},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, res, err := ValidateResult(raw, 3000)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Sentences[0]
	if want := arabic + " 42 " + persian + " " + hindi; s.Text != want {
		t.Errorf("text %+q, want %+q", s.Text, want)
	}
	if strings.ContainsRune(s.Text, alm) {
		t.Errorf("text keeps U+061C")
	}
	if s.LowConfWords[0].W != arabic {
		t.Errorf("low-confidence word %+q, want %+q", s.LowConfWords[0].W, arabic)
	}
	if s.Words[0].W != arabic || s.Words[1].W != persian || s.Words[2].W != hindi {
		t.Errorf("words %+q %+q %+q", s.Words[0].W, s.Words[1].W, s.Words[2].W)
	}
	if got := res.Timing["note"]; got != "xyz" {
		t.Errorf("timing string %+q, want U+061C and the joiner removed there", got)
	}
}

func TestCleanTextDropsArabicLetterMark(t *testing.T) {
	in := "Ubuntu" + string(rune(0x061c)) + " 24.04"
	if got := CleanText(in, 64); got != "Ubuntu 24.04" {
		t.Errorf("CleanText(%+q) = %+q", in, got)
	}
	// The joiners stay out of the identifier-like strings, as before.
	if got := CleanText("a‌b‍c", 64); got != "abc" {
		t.Errorf("CleanText kept a joiner: %+q", got)
	}
}
