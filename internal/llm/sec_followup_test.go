// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// Follow-ups of the security review SEC1 of v3.2.x line A.

// SEC1 L1: stop_details.category is untrusted input; only a short
// identifier is passed on, anything else becomes "other".
func TestCleanRefusalCategory(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"cyber":                  "cyber",
		"bio_weapons2":           "bio_weapons2",
		"Cyber":                  RefusalCategoryOther,
		"cyber-security":         RefusalCategoryOther,
		"a b":                    RefusalCategoryOther,
		"类别":                     RefusalCategoryOther,
		"sk-test-0123456789abcd": RefusalCategoryOther,
		strings.Repeat("a", 32):  strings.Repeat("a", 32),
		strings.Repeat("a", 33):  RefusalCategoryOther,
		"x\nWARN fake log line":  RefusalCategoryOther,
	} {
		if got := CleanRefusalCategory(in); got != want {
			t.Errorf("CleanRefusalCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

// SEC1 L1: the parser of the answer and the call log both clean it; the
// job error never holds what the server sent.
func TestRefusalCategoryCleanedAtParse(t *testing.T) {
	const key = "sk-ant-test-SECRETKEY"
	evil := strings.Repeat("x", 5000) + " " + key
	body := fmt.Sprintf(`{"type":"message","content":[],"stop_reason":"refusal","stop_details":{"category":%q},"usage":{"input_tokens":5,"output_tokens":1}}`, evil)
	_, ae := parseAnthropic([]byte(body), key)
	if ae == nil || ae.kind != failRefusal || ae.category != RefusalCategoryOther {
		t.Fatalf("attempt error %+v", ae)
	}
	if m := answerMeta(model.ProviderAnthropic, []byte(body)); m.stopCategory != RefusalCategoryOther {
		t.Errorf("call log category %q", m.stopCategory)
	}
	err := refusalError(callInfo{role: "translate", providerName: "p", model: "claude-x"}, ae.category, 0)
	if s := err.Error(); strings.Contains(s, key) || strings.Contains(s, "xxxx") || !strings.Contains(s, RefusalCategoryOther) {
		t.Errorf("job error %q", s)
	}
	ok := `{"type":"message","content":[],"stop_reason":"refusal","stop_details":{"category":"cyber"}}`
	if _, ae := parseAnthropic([]byte(ok), key); ae == nil || ae.category != "cyber" {
		t.Errorf("a short identifier must be kept: %+v", ae)
	}
}

// titleCheckErr is a stage's check error that quotes the model's answer and
// lists its problems (as stages.titleRejected does).
type titleCheckErr struct{ title string }

func (e *titleCheckErr) Error() string {
	return fmt.Sprintf("title_en has 40 words; at most 12 are allowed: %q", e.title)
}
func (e *titleCheckErr) Problems() []string { return []string{e.Error(), "blurb_zh is empty"} }

// SEC1 L4: an answer a stage's check refused. Its text (a title, a regex,
// file names) goes to the rejected file only: the service log and the calls
// file name the kind, the error's type and the number of problems.
func TestReportRejectedCheckKeepsAnswerOutOfLogs(t *testing.T) {
	log := captureLog(t)
	const title = "SECRET-TITLE-WORDS of the lecture"
	answer := fmt.Sprintf(`{"title_en": %q}`, title)
	srv, _ := captureServer(t, func(n int, w http.ResponseWriter) { io.WriteString(w, okBody(answer)) })
	c, _, _ := newClient(t, srv.URL)
	ctx := WithCallTrace(sched.WithJob(context.Background(), model.Job{ID: "01JOB"}))
	res, err := c.Complete(ctx, config.RoleTranslate, secretSystem, secretUser, true)
	if err != nil {
		t.Fatal(err)
	}
	ReportRejected(ctx, config.RoleTranslate, res.Text, &titleCheckErr{title: title})
	ReportRejected(ctx, config.RoleTranslate, res.Text, fmt.Errorf("answer idx does not match: %q", title))

	raw, recs := readCalls(t, c.cfg.DataDir)
	noSecrets(t, raw, "SECRET-TITLE-WORDS")
	if events(recs) != "start end rejected rejected" {
		t.Fatalf("events %q:\n%s", events(recs), raw)
	}
	if r := recs[2]; r.Kind != "check" || !strings.Contains(r.Error, "check failed") || !strings.Contains(r.Error, "titleCheckErr") ||
		!strings.Contains(r.Error, "2 problem(s)") {
		t.Errorf("rejected record %+v", r)
	}
	if r := recs[3]; r.Kind != "check" || !strings.HasPrefix(r.Error, "check failed") {
		t.Errorf("rejected record %+v", r)
	}
	rawRej, rej := readLog[rejectedRecord](t, c.cfg.DataDir, rejectedPrefix)
	if len(rej) != 2 || rej[0].Output != answer || !strings.Contains(rej[0].Error, title) {
		t.Errorf("rejected file (the answer and the full reason belong here):\n%s", rawRej)
	}
	if s := log.String(); strings.Count(s, `msg="llm answer rejected"`) != 2 || strings.Contains(s, "SECRET-TITLE-WORDS") ||
		!strings.Contains(s, "kind=check") {
		t.Errorf("service log:\n%s", s)
	}
}

// A JSON decoder error is kept (it quotes at most a character); the wrapped
// ErrBadJSON stays recognisable.
func TestRejectedLogReasonDecoderError(t *testing.T) {
	var v struct{ Idx int }
	err := ParseJSON(`{"idx": "seven"}`, &v)
	if !errors.Is(err, ErrBadJSON) {
		t.Fatalf("parse error %v", err)
	}
	if got := rejectedLogReason(err); !strings.Contains(got, "cannot unmarshal") || strings.Contains(got, "seven") {
		t.Errorf("reason %q", got)
	}
}
