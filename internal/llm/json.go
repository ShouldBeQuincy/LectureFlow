// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// ErrBadJSON wraps every ParseJSON failure. A stage counts it as one failed
// attempt of its business retry (repair: 2 per chunk, translate: 2 per
// sentence; SPEC §12).
var ErrBadJSON = errors.New("LLM output is not valid JSON")

// ErrSecondJSON: the answer holds a JSON value and, after it, another one
// that differs — the model corrected itself ("Wait, …"). Neither is used
// (v3.2.x P1, Q1b): the stage asks again and says so. It is an ErrBadJSON;
// its text is written to be quoted in the retry feedback.
var ErrSecondJSON = errors.New("the answer holds two different JSON values; output exactly one JSON value and nothing after it")

// errNoJSONObject: the answer holds no "{" (the error quotes its start,
// which the service log leaves out: rejectedLogReason).
var errNoJSONObject = errors.New("no JSON object in")

// ParseJSON decodes model output into v (SPEC §12 as amended by v3.2.x P1):
// the text as is when it is one JSON value; otherwise, with ``` code fences
// removed, the FIRST JSON value of the text (from its first "{", or from a
// leading "[" when v takes an array). What follows that value:
//
//   - only text: the first value is used;
//   - another JSON object or array equal to the first: the first is used;
//   - another JSON value of the same kind that differs: ErrSecondJSON.
//
// ParseAnswer is ParseJSON that also records an accepted answer's tail.
func ParseJSON(text string, v any) error {
	_, err := parseJSON(text, v)
	return err
}

// ParseAnswer is ParseJSON for the answer of the last Complete call under
// ctx (WithCallTrace): an answer that is used although text follows its JSON
// value is recorded in the call log (event "trailing") and the service log.
// A failure is returned as is; the stage reports it with ReportRejected.
func ParseAnswer(ctx context.Context, role, text string, v any) error {
	trailing, err := parseJSON(text, v)
	if err == nil && trailing {
		reportTrailing(ctx, role, text)
	}
	return err
}

// parseJSON is ParseJSON; trailing reports a used value that text follows.
func parseJSON(text string, v any) (trailing bool, err error) {
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), v); err == nil {
		return false, nil
	}
	s := strings.TrimSpace(stripFences(text))
	// The value starts at a leading "[" (an array answer) when v takes an
	// array, else at the first "{" (text before an object is skipped, as it
	// always was). A leading "[" before an object answer ("[{...}]") is such
	// text: the object inside is the value, and the "]" after it is text
	// after the value (accepted). An array of two different objects is two
	// different answers (ErrSecondJSON), as the same text without the "["
	// would be.
	start, end, ok := 0, 0, false
	if strings.HasPrefix(s, "[") && takesArray(v) {
		end, ok = firstJSONValue(s)
	}
	if !ok {
		if start = strings.Index(s, "{"); start < 0 {
			return false, fmt.Errorf("%w: %w %q", ErrBadJSON, errNoJSONObject, short(text))
		}
		end, ok = firstJSONValue(s[start:])
	}
	if !ok {
		// Not a complete value: the decoder's error says where it breaks.
		var raw json.RawMessage
		derr := json.NewDecoder(strings.NewReader(s[start:])).Decode(&raw)
		return false, fmt.Errorf("%w: %v", ErrBadJSON, derr)
	}
	first := s[start : start+end]
	rest := strings.TrimSpace(s[start+end:])
	if rest != "" && secondValueDiffers(first, rest) {
		return false, fmt.Errorf("%w: %w", ErrBadJSON, ErrSecondJSON)
	}
	if err := json.Unmarshal([]byte(first), v); err != nil {
		return false, fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	return rest != "", nil
}

// secondValueDiffers reports whether rest, the text after the JSON value
// first, holds another JSON value of the same kind (object or array) that
// is not equal to first: the model's corrected answer. Values of the other
// kind ("[1]" in a note after an object) and "{}" / "[]" are not answers.
func secondValueDiffers(first, rest string) bool {
	var want any
	if json.Unmarshal([]byte(first), &want) != nil {
		return false
	}
	return secondValueDiffersScan(want, first[0], rest, newCandidateScan(len(rest)))
}

// secondValueDiffersScan is secondValueDiffers with its scan budget.
func secondValueDiffersScan(want any, open byte, rest string, sc *candidateScan) bool {
	n := 0
	for i := 0; i < len(rest) && n < maxCandidates; i++ {
		if rest[i] != open {
			continue
		}
		n++
		end, ok, spent := sc.value(rest[i:])
		if spent {
			return false // the budget is used up: no further candidate is looked at
		}
		if !ok {
			continue
		}
		var got any
		if json.Unmarshal([]byte(rest[i:i+end]), &got) == nil && !isEmptyJSON(got) && !reflect.DeepEqual(want, got) {
			return true
		}
		i += end - 1 // the values inside it are not answers of their own
	}
	return false
}

// maxCandidates is the most positions of an answer's tail that are tried as
// the start of a second JSON value (secondValueDiffers, ShapeOf).
const maxCandidates = 64

// Candidate scan budget (SEC1 L2): every candidate may decode up to the end
// of the tail, so 64 of them could cost 64 times the tail. All candidates of
// one tail together read at most candidateBudgetFactor × its length plus
// candidateBudgetSlack bytes (the slack covers the decoder's first read of
// 512 bytes for each of the 64 candidates on a short tail). A real answer
// stays far inside it: a value that decodes is skipped as a whole, a broken
// one fails where it breaks. A tail that uses the budget up is scanned no
// further: what was found until then stands, a second value beyond is not
// seen — the rule is a heuristic anyway (NOTES.md, “第二个 JSON”, SEC1 T2).
const (
	candidateBudgetFactor = 4
	candidateBudgetSlack  = 64 << 10
)

// candidateScan decodes candidate values within a byte budget.
type candidateScan struct {
	left int // bytes the decoder may still read
	read int // bytes it read (tests)
}

func newCandidateScan(tail int) *candidateScan {
	return &candidateScan{left: candidateBudgetFactor*tail + candidateBudgetSlack}
}

// errScanBudget: the candidate scan's budget is used up.
var errScanBudget = errors.New("candidate scan budget used up")

// value is firstJSONValue within the budget: the length of the JSON value s
// starts with; spent reports that the budget ran out before the decoder was
// done (ok is false then).
func (sc *candidateScan) value(s string) (end int, ok, spent bool) {
	dec := json.NewDecoder(&budgetReader{r: strings.NewReader(s), sc: sc})
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return 0, false, errors.Is(err, errScanBudget)
	}
	return int(dec.InputOffset()), true, false
}

// budgetReader reads from r what the scan's budget still allows.
type budgetReader struct {
	r  *strings.Reader
	sc *candidateScan
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.r.Len() == 0 {
		return 0, io.EOF
	}
	if b.sc.left <= 0 {
		return 0, errScanBudget
	}
	if len(p) > b.sc.left {
		p = p[:b.sc.left]
	}
	n, err := b.r.Read(p)
	b.sc.left -= n
	b.sc.read += n
	return n, err
}

// isEmptyJSON reports an empty object or array ("{}", "{ }", "[]"): not an
// answer of its own.
func isEmptyJSON(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// takesArray reports whether v (a pointer, as json.Unmarshal takes) can
// hold a JSON array: a slice, an array, an interface or a json.Unmarshaler.
func takesArray(v any) bool {
	t := reflect.TypeOf(v)
	if t == nil || t.Kind() != reflect.Pointer {
		return true // json.Unmarshal reports what is wrong
	}
	if t.Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return true
	}
	switch t.Elem().Kind() {
	case reflect.Slice, reflect.Array, reflect.Interface:
		return true
	}
	return false
}

// stripFences drops every line that starts with ``` (e.g. "```json", "```").
func stripFences(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func short(s string) string {
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}
