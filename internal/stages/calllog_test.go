// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"lectureflow/internal/config"
)

// Every answer a JSON request cannot use is reported (llm.ReportRejected):
// one that is not JSON and one its check refuses, in askJSON and in
// askJSONRetry. The answers themselves stay out of the service log.
func TestRejectedAnswersAreReported(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	type answer struct {
		V int `json:"v"`
	}
	check := func(a *answer) error {
		if a.V == 0 {
			return errors.New("v is 0")
		}
		return nil
	}
	answers := []string{
		`{"v":1}` + "\nWait, SECOND-THOUGHTS:\n" + `{"v":2}`, // not JSON: text after the value
		`{"v":0}`, // refused by the check
		`{"v":3}`,
	}
	script := &scriptLLM{fn: func(ctx context.Context, n int, c llmCall) (string, error) { return answers[n%len(answers)], nil }}

	v, err := askJSON(context.Background(), script, config.RoleTranslate, "sys", "user", check)
	if err != nil || v.V != 3 {
		t.Fatalf("askJSON: %+v, %v", v, err)
	}
	s := logBuf.String()
	if strings.Count(s, `msg="llm answer rejected"`) != 2 || strings.Count(s, "kind=not_json") != 1 || strings.Count(s, "kind=check") != 1 ||
		!strings.Contains(s, "trailing_json=true") || !strings.Contains(s, "role="+config.RoleTranslate) {
		t.Errorf("after askJSON:\n%s", s)
	}

	logBuf.Reset()
	script.calls = nil
	_, unusable, err := askJSONRetry(context.Background(), script, config.RoleGlossary, "sys", "user", check)
	if !unusable || err == nil {
		t.Fatalf("askJSONRetry: unusable %v, %v", unusable, err)
	}
	s = logBuf.String()
	if strings.Count(s, `msg="llm answer rejected"`) != auxAttempts || strings.Count(s, "kind=not_json") != 1 || strings.Count(s, "kind=check") != 1 {
		t.Errorf("after askJSONRetry:\n%s", s)
	}
	if strings.Contains(s, "SECOND-THOUGHTS") {
		t.Errorf("the answer is in the service log:\n%s", s)
	}
}
