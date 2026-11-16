// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// Review R51: both title answers hold no JSON. llm.ParseJSON's error quotes
// the start of the answer; the retry feedback still carries that text word
// for word, but the job detail, the error (the scheduler's "job failed"
// line) and the service log say only that the answer was not JSON and its
// length. Chapters, the same.
func TestNotJSONAnswerKeptOutOfJobErrorAndLog(t *testing.T) {
	for _, c := range []struct {
		role, stage, code string
	}{
		{llmtest.RoleTitle, model.StageTitle, "title.invalid_output"},
		{llmtest.RoleChapters, model.StageChapters, "chapters.invalid_output"},
	} {
		t.Run(c.stage, func(t *testing.T) {
			log := stagesLog(t)
			e := newEnv(t)
			e.organizerRoles()
			answer := "Sure. Secretword caches keep recent blocks close to the processor."
			e.fake.Handle(c.role, func(string, string) string { return answer })
			var r runResult
			if c.stage == model.StageTitle {
				r = e.runTitle(e.deps(nil), e.titleLecture("x.mp4").ID)
			} else {
				r = e.runChapters(e.deps(nil), e.chaptersLecture().ID)
			}
			if errCode(r.err) != c.code || r.job.Status != model.StatusFailed {
				t.Fatalf("err %v, job %s", r.err, r.job.Status)
			}
			detail := jobDetail(t, r.job)
			for _, s := range []string{r.err.Error(), detail, log.String()} {
				if strings.Contains(s, "Secretword") {
					t.Errorf("answer text leaked:\n%s", s)
				}
			}
			want := fmt.Sprintf("not valid JSON (an answer of %d characters, not quoted here)", len([]rune(answer)))
			if !strings.Contains(detail, want) || !errors.Is(r.err, llm.ErrBadJSON) {
				t.Errorf("detail %q, want %q", detail, want)
			}
			// the feedback, word for word as before: the parser's own text
			var v map[string]any
			perr := llm.ParseJSON(answer, &v)
			if perr == nil || !strings.Contains(perr.Error(), "Secretword") {
				t.Fatalf("parse error %v", perr)
			}
			reqs := e.fake.Requests(c.role)
			if len(reqs) != 2 {
				t.Fatalf("%d requests", len(reqs))
			}
			if got, want := reqs[1].User, ctxbuild.WithRetryFeedback(reqs[0].User, []string{ctxbuild.NotJSONProblem(perr.Error())}); got != want {
				t.Errorf("retry request:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// Review R51: an askJSON stage that gives up (errUnusable) keeps the
// answer's text out of its error and coded detail; the reason a pending
// sentence stores (qa_error, via the last error) is unchanged.
func TestUnusableErrorQuotesNoAnswer(t *testing.T) {
	answer := "Secretword lecture text without any JSON"
	var v map[string]any
	perr := llm.ParseJSON(answer, &v)
	u := &errUnusable{last: &unparsedAnswer{err: perr, chars: len([]rune(answer))}}
	var ce *model.CodedError
	if !errors.As(u, &ce) || ce.Info.Code != CodeOutputUnusable {
		t.Fatalf("coded %+v", ce)
	}
	for _, s := range []string{u.Error(), ce.Err.Error(), state.EncodeJobError(u)} {
		if strings.Contains(s, "Secretword") || !strings.Contains(s, "an answer of 40 characters") {
			t.Errorf("text %q", s)
		}
	}
	if fmt.Sprint(u.last) != perr.Error() || !errors.Is(u, llm.ErrBadJSON) {
		t.Errorf("last error changed: %v", u.last)
	}
	// two different JSON values quote nothing: kept as they are
	second := &unparsedAnswer{err: fmt.Errorf("%w: %w", llm.ErrBadJSON, llm.ErrSecondJSON), chars: 9}
	if withoutAnswerText(second) != error(second) {
		t.Error("second-JSON error replaced")
	}
}

// jobDetail is the detail of a failed job's coded error (shown under 查看详情).
func jobDetail(t *testing.T, j model.Job) string {
	t.Helper()
	if j.Error == nil {
		t.Fatal("job has no error")
	}
	info, _ := state.ParseJobError(*j.Error)
	if info == nil {
		t.Fatalf("job error is not coded: %q", *j.Error)
	}
	return info.Detail
}

// R7 A-S4 (r4 D1): a title answer over the limits fails the task twice.
// The generated title and description reach the model's retry feedback,
// never the error text (the scheduler's "job failed" log line and the job
// detail) nor the service log; the error still names the field, the count
// and the limit.
func TestTitleRejectedErrorHoldsNoGeneratedText(t *testing.T) {
	log := stagesLog(t)
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("x.mp4")
	title := strings.TrimSpace(strings.Repeat("Secretword ", 9))
	blurb := strings.Repeat("秘", 51)
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
		return fmt.Sprintf(`{"title_en": %q, "title_zh": "标题", "blurb_en": "A blurb.", "blurb_zh": %q}`, title, blurb)
	})
	r := e.runTitle(e.deps(nil), l.ID)
	if errCode(r.err) != "title.invalid_output" || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v, job %s", r.err, r.job.Status)
	}
	detail := jobDetail(t, r.job)
	for _, s := range []string{r.err.Error(), detail, log.String()} {
		if strings.Contains(s, "Secretword") || strings.Contains(s, "秘") {
			t.Errorf("generated text leaked:\n%s", s)
		}
	}
	for _, want := range []string{"title_en has 9 words; at most 8 are allowed", "blurb_zh has 51 characters; at most 50 are allowed"} {
		if !strings.Contains(r.err.Error(), want) || !strings.Contains(detail, want) {
			t.Errorf("error lacks %q: %v / %s", want, r.err, detail)
		}
	}
	reqs := e.fake.Requests(llmtest.RoleTitle)
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	fb := strings.TrimPrefix(reqs[1].User, reqs[0].User)
	for _, want := range []string{fmt.Sprintf("title_en has 9 words; at most 8 are allowed: %q", title),
		"blurb_zh has 51 characters; at most 50 are allowed (each Chinese character or punctuation mark counts as one, an English term as one): \"" + blurb + "\""} {
		if !strings.Contains(fb, want) {
			t.Errorf("feedback lacks %q:\n%s", want, fb)
		}
	}
}

// R7 A-S4 (r4 D1): a kind that is not in the catalog is dropped; the log
// line keeps the length and, for free text, a placeholder, not the answer.
func TestTitleUnknownKindLogIsCleaned(t *testing.T) {
	log := stagesLog(t)
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("x.mp4")
	kind := "seminar about " + strings.Repeat("秘密", 30) + " and Secretword " + strings.Repeat("x", 80)
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
		return fmt.Sprintf(`{"title_en": "Caches", "title_zh": "缓存", "blurb_en": "A blurb.", "blurb_zh": "简介。", "kind": %q}`, kind)
	})
	if r := e.runTitle(e.deps(nil), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	s := log.String()
	if !strings.Contains(s, "kind not in the catalog") || !strings.Contains(s, fmt.Sprintf("kind_len=%d", len([]rune(kind)))) {
		t.Fatalf("no log line for the dropped kind:\n%s", s)
	}
	if strings.Contains(s, "秘") || strings.Contains(s, "Secretword") || strings.Contains(s, strings.Repeat("x", 41)) {
		t.Errorf("kind logged as written:\n%s", s)
	}
}

func TestLogKindValue(t *testing.T) {
	for in, want := range map[string]string{
		"":                            "",
		"tutorial":                    "tutorial",
		"Lab_2-b.x":                   "Lab_2-b.x",
		"a b":                         logKindFreeText,
		"lab\n":                       logKindFreeText,
		"讲座":                          logKindFreeText,
		strings.Repeat("a", 40):       strings.Repeat("a", 40),
		strings.Repeat("a", 41):       logKindFreeText,
		"lab\x1b[31m":                 logKindFreeText,
		string(rune(0x202e)) + "evil": logKindFreeText,
	} {
		if got := logKindValue(in); got != want {
			t.Errorf("logKindValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// R7 A-S4 (r4 D1): a chapter title over its limit is quoted in the retry
// feedback only; the error (job detail, scheduler log line) names the
// chapter, the field, the count and the limit.
func TestChaptersRejectedErrorHoldsNoGeneratedText(t *testing.T) {
	for _, c := range []struct {
		name, en, zh, feedback, summary string
	}{
		{"English", "Intro to Secretword caches and their many secret uses", "引言",
			`title_en "Intro to Secretword caches and their many secret uses" has more than 8 words`,
			"chapter 2: title_en has 9 words; at most 8 are allowed"},
		{"Chinese", "Intro", strings.Repeat("秘", 17),
			`title_zh "` + strings.Repeat("秘", 17) + `" has more than 16 characters`,
			"chapter 2: title_zh has 17 characters; at most 16 are allowed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := stagesLog(t)
			e := newEnv(t)
			e.organizerRoles()
			l := e.chaptersLecture()
			e.fake.Handle(llmtest.RoleChapters, func(string, string) string {
				return fmt.Sprintf(`{"chapters": [{"start_idx": 0, "title_en": "Start", "title_zh": "开始"}, {"start_idx": 3, "title_en": %q, "title_zh": %q}, {"start_idx": 9, "title_en": "End", "title_zh": "结束"}]}`, c.en, c.zh)
			})
			r := e.runChapters(e.deps(nil), l.ID)
			if errCode(r.err) != "chapters.invalid_output" {
				t.Fatalf("err %v", r.err)
			}
			detail := jobDetail(t, r.job)
			for _, s := range []string{r.err.Error(), detail, log.String()} {
				if strings.Contains(s, "Secretword") || strings.Contains(s, "秘") {
					t.Errorf("generated text leaked:\n%s", s)
				}
			}
			if !strings.Contains(r.err.Error(), c.summary) || !strings.Contains(detail, c.summary) {
				t.Errorf("error lacks %q: %v / %s", c.summary, r.err, detail)
			}
			reqs := e.fake.Requests(llmtest.RoleChapters)
			if len(reqs) != 2 {
				t.Fatalf("%d requests", len(reqs))
			}
			if fb := strings.TrimPrefix(reqs[1].User, reqs[0].User); !strings.Contains(fb, "- "+c.feedback) {
				t.Errorf("feedback lacks %q:\n%s", c.feedback, fb)
			}
		})
	}
}
