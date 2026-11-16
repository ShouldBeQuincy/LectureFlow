// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// SEC1 L2: the candidates of an answer's tail (a second JSON value?) are
// decoded within a byte budget. The answer of the SEC1 probe — a valid
// first value, then 64 unclosed `{"a":` and an unclosed long string — made
// every candidate decode to the end of the text: about 64 times its length.
// Now all candidates together read at most a small multiple of it, and the
// answer ends as it did (the first value is used, the tail is text).
func TestCandidateScanBudget(t *testing.T) {
	const tail = 1 << 20
	rest := strings.Repeat(`{"a":`, maxCandidates) + `"` + strings.Repeat("x", tail)
	text := `{"a":1}` + rest
	limit := 5 * len(rest) // the budget is 4× the tail plus 64 KB

	var want any
	if err := json.Unmarshal([]byte(`{"a":1}`), &want); err != nil {
		t.Fatal(err)
	}
	sc := newCandidateScan(len(rest))
	if secondValueDiffersScan(want, '{', rest, sc) {
		t.Error("secondValueDiffers: a second value found in an unclosed tail")
	}
	if sc.read > limit {
		t.Errorf("secondValueDiffers decoded %d bytes of a %d-byte tail (at most %d)", sc.read, len(rest), limit)
	}
	sc = newCandidateScan(len(rest))
	if trailingJSONScan(rest, sc) {
		t.Error("ShapeOf: trailing JSON found in an unclosed tail")
	}
	if sc.read > limit {
		t.Errorf("ShapeOf decoded %d bytes of a %d-byte tail (at most %d)", sc.read, len(rest), limit)
	}

	var v struct{ A int }
	if trailing, err := parseJSON(text, &v); err != nil || !trailing || v.A != 1 {
		t.Errorf("parseJSON: trailing %v, err %v, value %+v; want the first value with its tail", trailing, err, v)
	}
	if sh := ShapeOf(text); sh.JSONEnd != len(`{"a":1}`) || sh.TrailingBytes != len(rest) || sh.TrailingJSON {
		t.Errorf("ShapeOf %+v", sh)
	}
}

// Within the budget nothing changes: a second value after many candidates
// that are not values (the most the scan looks at) is still found, by
// ParseJSON and by ShapeOf.
func TestCandidateScanWithinBudget(t *testing.T) {
	first := `{"idx": 3, "zh": "好"}`
	noise := strings.Repeat("{x ", maxCandidates-1)
	text := first + "\nNotes " + noise + "\nCorrected: " + `{"idx": 3, "zh": "很好"}`
	var v struct {
		Idx int
		Zh  string
	}
	if err := ParseJSON(text, &v); !errors.Is(err, ErrSecondJSON) {
		t.Errorf("ParseJSON: %v, want ErrSecondJSON", err)
	}
	if sh := ShapeOf(text); !sh.TrailingJSON {
		t.Errorf("ShapeOf %+v: want trailing_json", sh)
	}
	// one candidate more than the scan looks at: not found, as before
	text = first + "\nNotes " + noise + "{x " + `{"idx": 3, "zh": "很好"}`
	if err := ParseJSON(text, &v); err != nil {
		t.Errorf("ParseJSON beyond %d candidates: %v", maxCandidates, err)
	}
}
