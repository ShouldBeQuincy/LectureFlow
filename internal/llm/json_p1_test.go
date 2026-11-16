// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// v3.2.x P1 (Q1b): only the first JSON value of an answer is used. Text
// after it is accepted; a second JSON value of the same kind that differs
// from the first is the model correcting itself and makes the answer
// unusable (ErrSecondJSON); an equal one does not.
func TestParseJSONFirstValue(t *testing.T) {
	type out struct {
		Idx int    `json:"idx"`
		Zh  string `json:"zh"`
	}
	first := `{"idx": 3, "zh": "好"}`
	for _, c := range []struct {
		name, text string
		trailing   bool
		want       error // nil = the first value is used
	}{
		{"no tail", first, false, nil},
		{"text only", first + "\n\nNote: I kept the term in English.", true, nil},
		{"text with a brace", first + "\nWait... } never mind", true, nil},
		{"text with braces that are not JSON", first + "\nThe format {idx, zh} was used.", true, nil},
		{"second value equal", first + "\n\nWait, let me check. Final answer:\n" + first, true, nil},
		{"second value equal, other spacing and key order", first + "\nAgain: {\"zh\":\"好\",\"idx\":3}", true, nil},
		{"second value differs", first + "\n\nWait, that is wrong. Corrected:\n" + `{"idx": 3, "zh": "很好"}`, false, ErrSecondJSON},
		{"second value differs, no text between", first + `{"idx": 3, "zh": "很好"}`, false, ErrSecondJSON},
		{"equal then different", first + "\n" + first + "\nActually:\n" + `{"idx": 4, "zh": "好"}`, false, ErrSecondJSON},
		{"fenced, text after the fence", "```json\n" + first + "\n```\nHope this helps.", true, nil},
		{"two fences, second differs", "```json\n" + first + "\n```\nWait.\n```json\n{\"idx\": 3, \"zh\": \"坏\"}\n```", false, ErrSecondJSON},
		{"two fences, second equal", "```json\n" + first + "\n```\nSame again:\n```json\n" + first + "\n```", true, nil},
		{"prose before, text after", "Here you go: " + first + " Done.", true, nil},
		{"an array in the tail of an object answer", first + "\nSee note [1] and [2, 3].", true, nil},
		{"an empty object in the tail", first + "\n{} is what I would send otherwise.", true, nil},
		{"second value broken", first + "\nWait: {\"idx\": 3, \"zh\": ", true, nil},
	} {
		var v out
		trailing, err := parseJSON(c.text, &v)
		switch {
		case c.want == nil:
			if err != nil || v != (out{3, "好"}) || trailing != c.trailing {
				t.Errorf("%s: %+v trailing %v, err %v", c.name, v, trailing, err)
			}
		default:
			if !errors.Is(err, c.want) || !errors.Is(err, ErrBadJSON) {
				t.Errorf("%s: err %v, want %v", c.name, err, c.want)
			}
		}
		// ParseJSON is the same without the flag.
		var v2 out
		if err2 := ParseJSON(c.text, &v2); (err2 == nil) != (err == nil) || v2 != v {
			t.Errorf("%s: ParseJSON %+v %v", c.name, v2, err2)
		}
	}
	// The message of ErrSecondJSON says what to do instead.
	if !strings.Contains(ErrSecondJSON.Error(), "exactly one JSON value") {
		t.Errorf("ErrSecondJSON: %q", ErrSecondJSON)
	}

	// Arrays: the value starts at a leading "[".
	for _, c := range []struct {
		name, text string
		ok         bool
	}{
		{"array", "[1, 2, 3]", true},
		{"array then text", "[1, 2, 3]\nThese are the numbers.", true},
		{"array then an object", "[1, 2, 3]\n{\"note\": \"x\"}", true},
		{"array then the same array", "[1, 2, 3] and again [1,2,3]", true},
		{"array then another array", "[1, 2, 3]\nCorrection: [1, 2, 4]", false},
		{"fenced array then text", "```json\n[1, 2, 3]\n```\nDone.", true},
	} {
		var arr []int
		err := ParseJSON(c.text, &arr)
		if c.ok && (err != nil || len(arr) != 3 || arr[2] != 3) || !c.ok && !errors.Is(err, ErrSecondJSON) {
			t.Errorf("%s: %v %v", c.name, arr, err)
		}
	}
	// Text that starts with "[" but is not an array: the first "{" as before.
	var v out
	if err := ParseJSON("[draft] "+first, &v); err != nil || v != (out{3, "好"}) {
		t.Errorf("bracket prose: %+v %v", v, err)
	}
	// A truncated answer is not a second-thoughts answer.
	if err := ParseJSON(`{"idx": 3, "zh": "好`, &v); !errors.Is(err, ErrBadJSON) || errors.Is(err, ErrSecondJSON) {
		t.Errorf("truncated: %v", err)
	}
}

// ParseAnswer records an answer that is used although text follows its JSON
// value: event "trailing" in the call log (the shape, not the text), an
// excerpt in the rejected file, a warning in the service log. An answer
// without a tail leaves no record; a failure is left to ReportRejected.
func TestParseAnswerRecordsTrailingText(t *testing.T) {
	log := captureLog(t)
	answers := []string{
		`{"zh":"第一版"}` + "\n\nNote: SECOND-THOUGHTS about the tone.",
		`{"zh":"第一版"}`,
		`{"zh":"第一版"}` + "\nWait:\n" + `{"zh":"第二版"}`,
	}
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody(answers[n-1])) })
	c, _, _ := newClient(t, srv.URL)
	ctx := WithCallTrace(sched.WithJob(context.Background(), model.Job{ID: "01JOB"}))
	var errs []error
	for range answers {
		res, err := c.Complete(ctx, config.RoleTranslate, secretSystem, secretUser, true)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]string
		perr := ParseAnswer(ctx, config.RoleTranslate, res.Text, &v)
		if perr == nil && v["zh"] != "第一版" {
			t.Fatalf("value %v", v)
		}
		errs = append(errs, perr)
	}
	if errs[0] != nil || errs[1] != nil || !errors.Is(errs[2], ErrSecondJSON) {
		t.Fatalf("errors %v", errs)
	}
	raw, recs := readCalls(t, c.cfg.DataDir)
	noSecrets(t, raw, "SECOND-THOUGHTS", "第一版", "第二版")
	if events(recs) != "start end trailing start end start end" {
		t.Fatalf("events %q:\n%s", events(recs), raw)
	}
	if r := recs[2]; r.Kind != "trailing" || r.Try != 1 || r.Call != recs[0].Call || r.JobID != "01JOB" || r.OutputShape == nil ||
		r.JSONEnd != len(`{"zh":"第一版"}`) || r.TrailingBytes == 0 || r.TrailingJSON {
		t.Errorf("trailing record %+v %+v", r, r.OutputShape)
	}
	rawRej, rej := readLog[rejectedRecord](t, c.cfg.DataDir, rejectedPrefix)
	// SEC1 T1: the rejected file keeps the text after the used value only.
	if len(rej) != 1 || rej[0].Kind != "trailing" || rej[0].Output != "Note: SECOND-THOUGHTS about the tone." || rej[0].Call != recs[0].Call ||
		strings.Contains(rawRej, "第一版") {
		t.Errorf("rejected file:\n%s", rawRej)
	}
	if s := log.String(); strings.Count(s, `msg="llm answer has text after its JSON"`) != 1 || strings.Contains(s, "SECOND-THOUGHTS") ||
		strings.Contains(s, `msg="llm answer rejected"`) {
		t.Errorf("service log:\n%s", s)
	}
}
