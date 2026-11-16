// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package qa is the per-sentence translation check of SPEC §7.4.
//
// It is pure: no database, files, network, clock or randomness (SPEC §1.1).
// The exact definitions of every rule are recorded in NOTES.md.
package qa

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/model"
)

// Thresholds of SPEC §7.4.
const (
	MaxLatinRatio  = 0.5 // rule 3: English residue
	CodeSymbolMax  = 0.3 // rule 3: target counts as code/formula at >= 30% code symbols
	MinLengthRatio = 0.5 // rule 4
	MaxLengthRatio = 4.0 // rule 4
	// ShortTargetWords: for a target of at most this many English words
	// (EnglishWords) rule 4 skips the lower bound and computes the upper
	// bound for ShortTargetWords+1 words (MaxLengthRatio*(ShortTargetWords+1)
	// characters). Amendment of SPEC §7.4 item 4.
	ShortTargetWords = 3
)

// codeSymbols is the symbol set of rule 3 ("= ( ) { } ; 等符号").
const codeSymbols = "=(){}[];<>+-*/%^&|~!\\_#$@"

// numberRE matches a number: digits with optional thousands separators
// ("1,000") and an optional decimal part ("3.14").
var numberRE = regexp.MustCompile(`\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?`)

// Check runs the five checks of SPEC §7.4 (rule 2 as amended by SPEC-v2
// G5.4) on the translation zh of the target sentence (sentence.text). terms
// are the course terms; disabled and reference-tier ones are ignored. On
// failure reason is a short English sentence (several failures joined by
// "; "), suitable for "Your previous translation was rejected: {reason}.
// Translate again."
func Check(target, zh string, terms []model.Term) (ok bool, reason string) {
	if fails := Failures(target, zh, terms); len(fails) > 0 {
		return false, Reason(fails)
	}
	return true, ""
}

// Failures are Check's failed checks, coded (codes.go); none = ok.
func Failures(target, zh string, terms []model.Term) []Failure {
	// 1. non-empty
	if strings.TrimSpace(zh) == "" {
		return []Failure{{Code: CodeEmpty}}
	}
	var fails []Failure

	// 2. enforce terms with a Chinese translation must be used (whole word,
	//    plural endings s/es accepted on the last word: Gate B ruling). A
	//    term found only inside a longer enforce term of the target ("key"
	//    in "secret key") is not checked on its own: only the longer one is
	//    (G5.4). Enforce terms without zh still count as the longer term —
	//    the prompt keeps them in English as a whole.
	var enforce []model.Term
	var phrases []string
	for _, t := range terms {
		if !t.Enabled || strings.TrimSpace(t.En) == "" || !ctxbuild.IsEnforce(t) {
			continue
		}
		enforce = append(enforce, t)
		phrases = append(phrases, t.En)
	}
	outer := ctxbuild.OuterTerms(target, phrases)
	zhNorm := compact(zh)
	var englishOnly []string
	for i, t := range enforce {
		tz := strings.TrimSpace(t.Zh)
		if tz == "" {
			englishOnly = append(englishOnly, t.En)
			continue
		}
		if outer[i] && !strings.Contains(zhNorm, compact(tz)) {
			fails = append(fails, Failure{Code: CodeTermMissing, Params: map[string]any{"term": strings.TrimSpace(t.En), "zh": tz}})
		}
	}

	// 3. English residue
	if lr := LatinRatio(zh, englishOnly); lr > MaxLatinRatio && CodeSymbolRatio(target) < CodeSymbolMax {
		// the percent as the sentence has always written it (%.0f)
		pct, _ := strconv.Atoi(fmt.Sprintf("%.0f", lr*100))
		fails = append(fails, Failure{Code: CodeMostlyEnglish, Params: map[string]any{"percent": pct}})
	}

	// 4. length ratio. For very short targets (at most ShortTargetWords
	//    words: "Okay?" → "好的，明白了吗？" is a fine translation) the lower
	//    bound is not checked (an empty zh is caught by rule 1) and the upper
	//    bound is computed as if the target had ShortTargetWords+1 words, so
	//    a one-word target expanded into a whole sentence still fails and the
	//    limit is continuous at 3/4 words. The reported ratio uses the same
	//    word count, so it is outside [min, max] exactly when the check fails.
	//    User-approved amendment of SPEC §7.4 item 4 (NOTES.md).
	if words := EnglishWords(target); words > 0 {
		short := words <= ShortTargetWords
		if short {
			words = ShortTargetWords + 1
		}
		r := float64(ZhChars(zh)) / float64(words)
		if (!short && r < MinLengthRatio) || r > MaxLengthRatio {
			// the ratio as the sentence has always written it (%.2f)
			r2, _ := strconv.ParseFloat(fmt.Sprintf("%.2f", r), 64)
			fails = append(fails, Failure{Code: CodeLengthRatio, Params: map[string]any{"ratio": r2, "min": MinLengthRatio, "max": MaxLengthRatio}})
		}
	}

	// 5. every number of the target appears in zh
	if missing := MissingNumbers(target, zh); len(missing) == 1 {
		fails = append(fails, Failure{Code: CodeNumberMissing, Params: map[string]any{"number": missing[0]}})
	} else if len(missing) > 1 {
		fails = append(fails, Failure{Code: CodeNumbersMissing, Params: map[string]any{"numbers": strings.Join(missing, ", ")}})
	}
	return fails
}

// LatinRatio is the share of Latin-script letters among all letters of zh
// (digits, punctuation, symbols and spaces are not letters). Whole-word
// occurrences of englishOnly (enforce terms whose zh is empty, which the
// prompt requires to stay in English; SPEC-v2 G5.4: reference terms do not
// count) are removed first. 0 when zh has no
// letter.
func LatinRatio(zh string, englishOnly []string) float64 {
	s := zh
	if len(englishOnly) > 0 {
		s = ctxbuild.StripTerms(zh, englishOnly)
	}
	latin, letters := 0, 0
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if unicode.Is(unicode.Latin, r) {
			latin++
		}
	}
	if letters == 0 {
		return 0
	}
	return float64(latin) / float64(letters)
}

// CodeSymbolRatio is the share of code symbols (codeSymbols) among the
// non-whitespace characters of s; 0 for an empty s.
func CodeSymbolRatio(s string) float64 {
	sym, all := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		all++
		if strings.ContainsRune(codeSymbols, r) {
			sym++
		}
	}
	if all == 0 {
		return 0
	}
	return float64(sym) / float64(all)
}

// ZhChars counts the characters of zh excluding whitespace (Han characters,
// punctuation, digits and Latin letters each count 1).
func ZhChars(zh string) int {
	n := 0
	for _, r := range zh {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// EnglishWords counts the whitespace-separated tokens of s that contain at
// least one letter or digit: pure-punctuation tokens ("—", "?") are not
// words; contractions ("don't"), hyphenated words ("well-known") and numbers
// ("42", "3.14") count as one word each.
func EnglishWords(s string) int {
	n := 0
	for _, f := range strings.Fields(s) {
		if strings.IndexFunc(f, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			n++
		}
	}
	return n
}

// Numbers returns the numbers of s in order of appearance, without
// duplicates. Full-width digits are read as ASCII digits; thousands
// separators are removed ("1,000" → "1000").
func Numbers(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range numberRE.FindAllString(asciiDigits(s), -1) {
		m = strings.ReplaceAll(m, ",", "")
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// MissingNumbers returns the numbers of target that do not appear as a
// number in zh (so "5" is not found inside "15").
func MissingNumbers(target, zh string) []string {
	have := map[string]bool{}
	for _, n := range Numbers(zh) {
		have[n] = true
	}
	var missing []string
	for _, n := range Numbers(target) {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

// asciiDigits maps full-width digits (U+FF10..U+FF19) to ASCII. The Chinese
// comma "，" is deliberately not mapped: it separates list items, not
// thousands.
func asciiDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '０' && r <= '９' {
			return '0' + (r - '０')
		}
		return r
	}, s)
}

// compact lowercases s and removes all whitespace (for term containment).
func compact(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
}
