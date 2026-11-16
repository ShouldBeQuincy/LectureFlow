// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package qa

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"lectureflow/internal/model"
)

// Coded failures (SPEC-v2 P14.3). The stored qa_error and the retry
// feedback of SPEC §7 are the English sentences below, unchanged; the API
// also sends the same failures as codes and params (Sentence.qa_error_info)
// so the interface can show them in its own language. Both directions live
// here — Failure.Text writes a sentence, ParseReason reads it back — so the
// two cannot drift apart (TestFailureRoundTrip).

// Failure codes and their params.
const (
	CodeEmpty          = "qa.empty"           //
	CodeTermMissing    = "qa.term_missing"    // term, zh
	CodeMostlyEnglish  = "qa.mostly_english"  // percent (int)
	CodeLengthRatio    = "qa.length_ratio"    // ratio (2 decimals), min, max
	CodeNumberMissing  = "qa.number_missing"  // number
	CodeNumbersMissing = "qa.numbers_missing" // numbers ("3, 64": a language-neutral list)
)

// A sentence left without a translation (v3.2.x P1, Q2a: "待处理"): the
// translate stage stores zh = "", zh_status = qa_failed and one of the
// reasons below as qa_error. They are not failures of Check (they are not
// in ErrorCodes and are never retry feedback); ErrorInfo codes them like
// the others, with codes of the translate stage's catalog
// (stages.ErrorCodes, error_codes_backend.json).
const (
	CodeSentenceUnusable = "translate.sentence_unusable" // attempts
	CodeSentenceRefused  = "translate.sentence_refused"  // category ("" = none given)
)

// UnusableReason is the qa_error of a sentence whose answers could not be
// used attempts times in a row; detail is what was wrong with the last one
// (kept as the text after the colon, on one line).
func UnusableReason(attempts int, detail string) string {
	return fmt.Sprintf("no usable translation after %d attempts: %s", attempts, strings.Join(strings.Fields(detail), " "))
}

// RefusedReason is the qa_error of a sentence the provider declined to
// translate; category is the provider's refusal category ("" = none).
func RefusedReason(category string) string {
	if category == "" {
		return "the provider declined to translate this sentence"
	}
	return fmt.Sprintf("the provider declined to translate this sentence (category %s)", category)
}

var (
	unusableRe = regexp.MustCompile(`^no usable translation after (\d+) attempts: `)
	refusedRe  = regexp.MustCompile(`^the provider declined to translate this sentence(?: \(category ([^)]*)\))?$`)
)

// PendingInfo reads UnusableReason and RefusedReason back; nil for any
// other reason.
func PendingInfo(reason string) *model.ErrorInfo {
	if m := unusableRe.FindStringSubmatch(reason); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil
		}
		return &model.ErrorInfo{Code: CodeSentenceUnusable, Params: map[string]any{"attempts": n}, Detail: reason}
	}
	if m := refusedRe.FindStringSubmatch(reason); m != nil {
		return &model.ErrorInfo{Code: CodeSentenceRefused, Params: map[string]any{"category": m[1]}, Detail: reason}
	}
	return nil
}

// ErrorCodes lists every failure code (the API's catalog has their texts).
var ErrorCodes = []string{CodeEmpty, CodeTermMissing, CodeMostlyEnglish, CodeLengthRatio, CodeNumberMissing, CodeNumbersMissing}

// Failure is one failed check.
type Failure struct {
	Code   string
	Params map[string]any
}

// reasonSep joins the sentences of several failures (Check's reason).
const reasonSep = "; "

// Text is the English sentence of the failure (SPEC §7.4 reason).
func (f Failure) Text() string {
	p := f.Params
	switch f.Code {
	case CodeEmpty:
		return "the translation is empty"
	case CodeTermMissing:
		return fmt.Sprintf("glossary term %q must be translated as %q", p["term"], p["zh"])
	case CodeMostlyEnglish:
		return fmt.Sprintf("the translation is mostly English (%d%% Latin letters)", p["percent"])
	case CodeLengthRatio:
		return fmt.Sprintf("the length ratio is %.2f characters per English word, outside [%g, %g]", p["ratio"], p["min"], p["max"])
	case CodeNumberMissing:
		return fmt.Sprintf("the number %s from the source sentence is missing", p["number"])
	case CodeNumbersMissing:
		return fmt.Sprintf("the numbers %s from the source sentence are missing", p["numbers"])
	}
	return f.Code
}

// Reason joins the failures' sentences: the qa_error and retry feedback.
func Reason(fs []Failure) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = f.Text()
	}
	return strings.Join(parts, reasonSep)
}

// quoted matches a Go %q string.
const quoted = `("(?:[^"\\]|\\.)*")`

// failurePatterns read the sentences of Text back, anchored at the start of
// the rest of a reason.
var failurePatterns = []struct {
	code string
	re   *regexp.Regexp
	read func(m []string) (map[string]any, bool)
}{
	{CodeEmpty, regexp.MustCompile(`^the translation is empty`), func([]string) (map[string]any, bool) { return nil, true }},
	{CodeTermMissing, regexp.MustCompile(`^glossary term ` + quoted + ` must be translated as ` + quoted), func(m []string) (map[string]any, bool) {
		term, err1 := strconv.Unquote(m[1])
		zh, err2 := strconv.Unquote(m[2])
		return map[string]any{"term": term, "zh": zh}, err1 == nil && err2 == nil
	}},
	{CodeMostlyEnglish, regexp.MustCompile(`^the translation is mostly English \((\d+)% Latin letters\)`), func(m []string) (map[string]any, bool) {
		n, err := strconv.Atoi(m[1])
		return map[string]any{"percent": n}, err == nil
	}},
	{CodeLengthRatio, regexp.MustCompile(`^the length ratio is (\d+\.\d\d) characters per English word, outside \[([0-9.e+-]+), ([0-9.e+-]+)\]`), func(m []string) (map[string]any, bool) {
		r, err1 := strconv.ParseFloat(m[1], 64)
		lo, err2 := strconv.ParseFloat(m[2], 64)
		hi, err3 := strconv.ParseFloat(m[3], 64)
		return map[string]any{"ratio": r, "min": lo, "max": hi}, err1 == nil && err2 == nil && err3 == nil
	}},
	{CodeNumberMissing, regexp.MustCompile(`^the number (\S+) from the source sentence is missing`), func(m []string) (map[string]any, bool) {
		return map[string]any{"number": m[1]}, true
	}},
	{CodeNumbersMissing, regexp.MustCompile(`^the numbers (.+?) from the source sentence are missing`), func(m []string) (map[string]any, bool) {
		return map[string]any{"numbers": m[1]}, true
	}},
}

// ParseReason reads a stored reason (Check's, possibly several failures)
// back into failures. ok is false when some part is not a sentence of this
// package (a reason written by another version): the caller shows the text.
func ParseReason(reason string) (fs []Failure, ok bool) {
	rest := reason
	for rest != "" {
		matched := false
		for _, p := range failurePatterns {
			m := p.re.FindStringSubmatch(rest)
			if m == nil {
				continue
			}
			params, good := p.read(m)
			after := rest[len(m[0]):]
			if !good || (after != "" && !strings.HasPrefix(after, reasonSep)) {
				continue
			}
			fs = append(fs, Failure{Code: p.code, Params: params})
			rest = strings.TrimPrefix(after, reasonSep)
			matched = true
			break
		}
		if !matched {
			return nil, false
		}
	}
	return fs, len(fs) > 0
}

// ErrorInfo is a stored reason as one coded text (P14.3): the first
// failure, the others in params.extra (the front end joins them as
// clauses); the reason itself is the detail. nil when it cannot be read.
// The reason of a sentence left without a translation: PendingInfo.
func ErrorInfo(reason string) *model.ErrorInfo {
	if info := PendingInfo(reason); info != nil {
		return info
	}
	fs, ok := ParseReason(reason)
	if !ok {
		return nil
	}
	info := &model.ErrorInfo{Code: fs[0].Code, Detail: reason}
	if len(fs[0].Params) > 0 || len(fs) > 1 {
		info.Params = map[string]any{}
	}
	for k, v := range fs[0].Params {
		info.Params[k] = v
	}
	if len(fs) > 1 {
		extra := make([]model.ErrorInfo, 0, len(fs)-1)
		for _, f := range fs[1:] {
			extra = append(extra, model.ErrorInfo{Code: f.Code, Params: f.Params})
		}
		info.Params["extra"] = extra
	}
	return info
}
