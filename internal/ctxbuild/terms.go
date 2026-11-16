// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/model"
)

// EnKey normalises a term for the (course_id, en_key) uniqueness of SPEC §3.3:
// lower(trim(en)) with every internal whitespace run collapsed to one space.
func EnKey(en string) string {
	return strings.ToLower(strings.Join(strings.Fields(en), " "))
}

// DropTerm reports whether an extracted term must be discarded by MergeTerms
// (SPEC §6.0.1): fewer than 2 characters, more than 6 words, or made only of
// digits / punctuation (symbols and spaces count as punctuation here).
func DropTerm(en string) bool {
	en = strings.TrimSpace(en)
	if len([]rune(en)) < 2 {
		return true
	}
	if len(strings.Fields(en)) > 6 {
		return true
	}
	for _, r := range en {
		if !(unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsSpace(r)) {
			return false
		}
	}
	return true
}

// Length limits of one glossary entry, in characters (runes) after trimming
// (AUD5 low-1). Real entries are far shorter (an English term has at most
// six words, §6.0.1; the longest translations, with alternatives as in
// "公开 / 公钥 (public key)", are a few dozen characters); an entry over a
// limit is not a term but text, e.g. instructions injected through a
// syllabus or slides, which would otherwise stand under MANDATORY TERMS of
// every later request of the course. They are rules of the glossary's
// content, checked where an entry is written: MergeTerms drops such
// extracted entries whole (never cut to half a phrase), a manual term or
// edit over a limit is refused, and the janitor's fix_zh over the limit is
// not applied. The prompts are not filtered: every enabled term still goes
// into {glossary_lines}, and QA checks them as before (SPEC §7).
const (
	TermEnMaxChars = 100
	TermZhMaxChars = 100
)

// TermTooLong reports whether a glossary entry is over TermEnMaxChars or
// TermZhMaxChars.
func TermTooLong(en, zh string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(en)) > TermEnMaxChars ||
		utf8.RuneCountInString(strings.TrimSpace(zh)) > TermZhMaxChars
}

// enabledTerms returns the enabled terms sorted by en_key (SPEC §12), with
// duplicates (same en_key) removed. Empty en is skipped.
func enabledTerms(terms []model.Term) []model.Term {
	out := make([]model.Term, 0, len(terms))
	seen := map[string]bool{}
	for _, t := range terms {
		if !t.Enabled || strings.TrimSpace(t.En) == "" {
			continue
		}
		k := termKey(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return termKey(out[i]) < termKey(out[j]) })
	return out
}

func termKey(t model.Term) string {
	if t.EnKey != "" {
		return t.EnKey
	}
	return EnKey(t.En)
}

// IsEnforce reports whether t is in the enforce tier (SPEC-v2 G1). The
// empty tier is read as enforce, like the database does.
func IsEnforce(t model.Term) bool { return t.Tier != model.TierReference }

// termLine is one glossary line: "en => zh", or just "en" when zh is empty
// (SPEC §13).
func termLine(t model.Term) string {
	en := oneLine(strings.TrimSpace(t.En))
	if zh := oneLine(strings.TrimSpace(t.Zh)); zh != "" {
		return en + " => " + zh
	}
	return en
}

// Section headers of {glossary_lines}, SPEC-v2 G5.1 verbatim.
const (
	MandatoryTermsHeader = "MANDATORY TERMS (always use these translations):"
	PreferredTermsHeader = "PREFERRED TERMS (use these translations only when the word is used in its technical sense):"
)

// GlossaryLines formats {glossary_lines} of the summary, repair and
// translate prompts (SPEC §13 as amended by SPEC-v2 G5.1): the enabled
// enforce terms under MandatoryTermsHeader, a blank line, the enabled
// reference terms under PreferredTermsHeader. Each section is sorted by
// en_key, one term per line as "en => zh" or just "en" when zh is empty; an
// empty section is "(none)". Disabled terms never appear.
//
// It is prompt text only: hashes use GlossaryHashText, which does not
// depend on this format (lead ruling 9).
func GlossaryLines(terms []model.Term) string {
	var enforce, reference []string
	for _, t := range enabledTerms(terms) {
		if IsEnforce(t) {
			enforce = append(enforce, termLine(t))
		} else {
			reference = append(reference, termLine(t))
		}
	}
	return MandatoryTermsHeader + "\n" + linesOrNone(enforce) + "\n\n" + PreferredTermsHeader + "\n" + linesOrNone(reference)
}

// GlossaryHashText is the glossary as it enters the repair and translate
// input hashes (lead ruling 9): the enabled terms sorted by en_key, one per
// line in the v1 {glossary_lines} format ("en => zh" or "en"), with
// " [reference]" appended to reference terms; "(none)" without terms. It
// never depends on the prompt format, so an all-enforce glossary gives
// exactly the v1 text (and the v1 hashes), while moving a term between
// tiers changes it.
func GlossaryHashText(terms []model.Term) string {
	ts := enabledTerms(terms)
	lines := make([]string, len(ts))
	for i, t := range ts {
		lines[i] = termLine(t)
		if !IsEnforce(t) {
			lines[i] += " [reference]"
		}
	}
	return linesOrNone(lines)
}

func linesOrNone(lines []string) string {
	if len(lines) == 0 {
		return None
	}
	return strings.Join(lines, "\n")
}

// BuildASRPrompt builds the faster-whisper initial_prompt (SPEC §6.2 as
// amended by SPEC-v2 G4). lectureText is the text of the materials bound to
// the lecture and, since lecture kinds, to its teaching week
// (BoundMaterialTextWeek). Candidates are the enabled enforce terms
// only, ordered by:
//  1. common_word = 0 first (whisper knows common words already);
//  2. source manual or review first;
//  3. occurrences in lectureText (whole word, CountTerm), descending;
//  4. terms found only in this lecture's materials (lecture count > 0 and
//     equal to the course freq) first;
//  5. course freq, descending;
//  6. multi-word terms first;
//  7. en_key.
//
// G4 rule 4 ("in equal conditions") is split (lead ruling, Gate round 1,
// NOTES.md): "only in this lecture" applies at equal lecture count, before
// the course freq (after it, it could never separate two terms); the
// multi-word preference only breaks freq ties, so rare multi-word terms do
// not push single-word proper nouns (Kasiski, Feistel) out of the 200
// tokens.
//
// Joined with ", ". Token truncation to 200 is the worker's job. The prompt
// is not part of the asr input hash (SPEC-v1.1 A1).
func BuildASRPrompt(terms []model.Term, lectureText string) string {
	type cand struct {
		t           model.Term
		lecture     int  // occurrences in this lecture's materials
		lectureOnly bool // found only in this lecture's materials
		multi       bool // multi-word term
	}
	text := PrepareTermText(lectureText)
	var cs []cand
	for _, t := range enabledTerms(terms) {
		if !IsEnforce(t) {
			continue
		}
		n := text.Count(t.En)
		cs = append(cs, cand{t: t, lecture: n, lectureOnly: n > 0 && n == t.Freq, multi: len(strings.Fields(t.En)) > 1})
	}
	manual := func(t model.Term) bool {
		return t.Source == model.SourceManual || t.Source == model.SourceReview
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.t.CommonWord != b.t.CommonWord {
			return !a.t.CommonWord
		}
		if ma, mb := manual(a.t), manual(b.t); ma != mb {
			return ma
		}
		if a.lecture != b.lecture {
			return a.lecture > b.lecture
		}
		if a.lectureOnly != b.lectureOnly {
			return a.lectureOnly
		}
		if a.t.Freq != b.t.Freq {
			return a.t.Freq > b.t.Freq
		}
		if a.multi != b.multi {
			return a.multi
		}
		return termKey(a.t) < termKey(b.t)
	})
	ens := make([]string, len(cs))
	for i, c := range cs {
		ens[i] = oneLine(strings.TrimSpace(c.t.En))
	}
	return strings.Join(ens, ", ")
}

// isWordRune is the word-character class used for whole-word matching.
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// normForMatch lowercases s and collapses whitespace runs to one space.
func normForMatch(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// CountTerm counts case-insensitive whole-word occurrences of phrase in text
// (SPEC §6.0.1 freq, §7.4 rule 2). Multi-word phrases match as a complete
// phrase; whitespace runs in both are treated as one space. A match must not
// be preceded or followed by a letter, digit or underscore. Occurrences do
// not overlap.
func CountTerm(text, phrase string) int {
	return countNorm(normForMatch(text), normForMatch(phrase))
}

// ContainsTerm reports whether text contains phrase as a whole word/phrase.
func ContainsTerm(text, phrase string) bool { return CountTerm(text, phrase) > 0 }

// ContainsTermPlural is ContainsTerm that also accepts the common English
// plural endings "s" and "es" on the phrase's last word ("cache" matches
// "caches", "page fault" matches "page faults", "bus" matches "buses").
// Used by qa rule 2 (SPEC §7.4; lead ruling at Gate B, NOTES.md).
func ContainsTermPlural(text, phrase string) bool {
	return len(pluralSpans(normForMatch(text), normForMatch(phrase))) > 0
}

// OuterTerms reports, for each phrase, whether it occurs in text (whole
// word, case-insensitive, plural endings as in ContainsTermPlural) at least
// once outside an occurrence of another, longer phrase of the list: in
// "the secret key" with phrases "key" and "secret key", "key" only occurs
// inside "secret key" and is false (SPEC-v2 G5.4: only the longer term is
// checked). Equal phrases never hide each other; overlapping occurrences
// that do not contain each other both count.
func OuterTerms(text string, phrases []string) []bool {
	t := normForMatch(text)
	spans := make([][][2]int, len(phrases))
	for i, p := range phrases {
		spans[i] = pluralSpans(t, normForMatch(p))
	}
	out := make([]bool, len(phrases))
	for i := range phrases {
		for _, s := range spans[i] {
			if !insideLonger(s, i, spans) {
				out[i] = true
				break
			}
		}
	}
	return out
}

// insideLonger reports whether span s of phrase i lies inside a strictly
// longer occurrence of another phrase.
func insideLonger(s [2]int, i int, spans [][][2]int) bool {
	for j, ss := range spans {
		if j == i {
			continue
		}
		for _, o := range ss {
			if o[0] <= s[0] && s[1] <= o[1] && o[1]-o[0] > s[1]-s[0] {
				return true
			}
		}
	}
	return false
}

// pluralSpans returns the byte spans of the whole-word occurrences of the
// normalised phrase p in the normalised text t, each optionally followed by
// the plural ending "s" or "es" (included in the span). Occurrences do not
// overlap.
func pluralSpans(t, p string) [][2]int {
	if p == "" {
		return nil
	}
	var out [][2]int
	for off := 0; off <= len(t)-len(p); {
		i := strings.Index(t[off:], p)
		if i < 0 {
			break
		}
		start := off + i
		end := start + len(p)
		next := start + runeLenAt(t, start)
		if boundaryBefore(t, start) {
			for _, suf := range []string{"", "s", "es"} {
				if strings.HasPrefix(t[end:], suf) && boundaryAfter(t, end+len(suf)) {
					out = append(out, [2]int{start, end + len(suf)})
					next = end + len(suf)
					break
				}
			}
		}
		off = next
	}
	return out
}

// TermText is a text prepared once for many CountTerm-style lookups (e.g.
// recomputing freq of every course term over all material text).
type TermText struct{ norm string }

// PrepareTermText normalises text for repeated Count calls.
func PrepareTermText(text string) TermText { return TermText{normForMatch(text)} }

// Count is CountTerm(text, phrase) for the prepared text.
func (t TermText) Count(phrase string) int { return countNorm(t.norm, normForMatch(phrase)) }

// StripTerms returns text lowercased with whitespace runs collapsed, and every
// whole-word occurrence of the phrases replaced by a space. Longer phrases
// are removed first. Used by qa to ignore glossary terms that must stay in
// English when measuring English residue.
func StripTerms(text string, phrases []string) string {
	t := normForMatch(text)
	ps := make([]string, 0, len(phrases))
	for _, p := range phrases {
		if p = normForMatch(p); p != "" {
			ps = append(ps, p)
		}
	}
	sort.SliceStable(ps, func(i, j int) bool { return len(ps[i]) > len(ps[j]) })
	for _, p := range ps {
		var b strings.Builder
		last := 0
		for off := 0; off <= len(t)-len(p); {
			i := strings.Index(t[off:], p)
			if i < 0 {
				break
			}
			start := off + i
			end := start + len(p)
			if boundaryBefore(t, start) && boundaryAfter(t, end) {
				b.WriteString(t[last:start])
				b.WriteByte(' ')
				last, off = end, end
			} else {
				off = start + runeLenAt(t, start)
			}
		}
		b.WriteString(t[last:])
		t = b.String()
	}
	return t
}

func countNorm(t, p string) int {
	if p == "" {
		return 0
	}
	n := 0
	for off := 0; off <= len(t)-len(p); {
		i := strings.Index(t[off:], p)
		if i < 0 {
			break
		}
		start := off + i
		end := start + len(p)
		if boundaryBefore(t, start) && boundaryAfter(t, end) {
			n++
			off = end
		} else {
			off = start + runeLenAt(t, start)
		}
	}
	return n
}

func boundaryBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return !isWordRune(r)
}

func boundaryAfter(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return !isWordRune(r)
}

func runeLenAt(s string, i int) int {
	_, n := utf8.DecodeRuneInString(s[i:])
	if n < 1 {
		return 1
	}
	return n
}
