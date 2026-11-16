// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// v3.2.x P1: answers with second thoughts, retries with feedback, pending
// sentences, refusals and structured output, through the translate stage.

// The production path — the HTTP client and the translate stage — against a
// server that appends "Wait, …" and a corrected JSON object to an answer:
// the answer is not used, the sentence is asked again with the reason, and
// the lecture is translated completely.
func TestTranslateSecondJSONAnswerIsRetriedWithFeedback(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	d := e.deps(nil)
	d.Concurrency = 1
	e.fake.FailNext(llmtest.RoleTranslate, 2, llmtest.FailSecondJSON)
	e.fake.FailNext(llmtest.RoleTranslate, 1, llmtest.FailTrailingText)
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.stats.PendingSentences != 0 || r.stats.Warning != "" {
		t.Fatalf("err %v stats %+v", r.err, r.stats)
	}
	for _, s := range e.sentences(l.ID) {
		if s.Zh == nil || *s.Zh == "" || s.ZhStatus == model.ZhNone || s.ZhStatus == model.ZhStale {
			t.Errorf("sentence %d: %s %v", s.Idx, s.ZhStatus, s.Zh)
		}
	}
	reqs := e.fake.Requests(llmtest.RoleTranslate)
	n := len(e.sentences(l.ID))
	if len(reqs) < n+2 {
		t.Fatalf("%d requests for %d sentences and 2 rejected answers", len(reqs), n)
	}
	// The first sentence: the request, then two with the feedback (the
	// third answer has trailing text only and is used).
	first := reqs[0]
	want := ctxbuild.WithRetryFeedback(first.User, []string{ctxbuild.SecondJSONProblem})
	if reqs[1].TargetIdx != first.TargetIdx || reqs[2].TargetIdx != first.TargetIdx || reqs[1].User != want || reqs[2].User != want ||
		reqs[1].System != first.System || strings.Contains(first.User, "REJECTED") {
		t.Fatalf("retries of sentence %d:\n%q\n%q", first.TargetIdx, reqs[1].User, reqs[2].User)
	}
	if !strings.Contains(want, "exactly one JSON object") {
		t.Errorf("feedback %q", want)
	}
	if reqs[3].TargetIdx == first.TargetIdx {
		t.Errorf("the answer with trailing text was not used: sentence %d asked a fourth time", first.TargetIdx)
	}
}

// Three answers with second thoughts in a row: the sentence is pending,
// with the reason; the others are translated and the stage is done.
func TestTranslateSecondJSONThreeTimesIsPending(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	d := e.deps(nil)
	d.Concurrency = 1
	e.fake.FailNext(llmtest.RoleTranslate, 3, llmtest.FailSecondJSON)
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 1 {
		t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
	}
	idx := e.fake.Requests(llmtest.RoleTranslate)[0].TargetIdx
	s := e.sentences(l.ID)[idx]
	if !isPending(s) || s.QAError == nil || !strings.HasPrefix(*s.QAError, "no usable translation after 3 attempts: ") ||
		!strings.Contains(*s.QAError, "two different JSON values") {
		t.Fatalf("sentence %d: %+v", idx, s)
	}
	if !paramsMatch(r.stats.WarningInfo, "n", 1, "ids", fmt.Sprintf("#%d", idx)) {
		t.Errorf("warning %+v", r.stats.WarningInfo)
	}

	// The export shows the sentence as untranslated.
	e.markDone(model.ScopeLecture, l.ID, model.StageTranslate)
	if r := e.run(e.stage(d, model.StageExport), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	lec, err := e.st.GetLecture(e.ctx, l.ID)
	e.must(err)
	md := exportedMarkdown(t, e, lec)
	if strings.Count(md, MissingZh) != 1 {
		t.Errorf("%d %q in the export, want 1", strings.Count(md, MissingZh), MissingZh)
	}
}

// exportedMarkdown reads the bilingual Markdown file of the lecture's export.
func exportedMarkdown(t *testing.T, e *env, l model.Lecture) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ExportDirName, "*.bilingual.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no exported Markdown file (%v)", err)
	}
	var sb strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
	}
	return sb.String()
}

// Not one sentence of a run got a usable answer: the stage fails as before
// v3.2.x and the sentences go back to stale, so that a retry sends them.
func TestTranslateEverySentenceUnusableFailsStage(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	ss := e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "I cannot do that.", nil }}
	r := e.run(e.stage(e.deps(fake), model.StageTranslate), l.ID)
	var ce *model.CodedError
	if r.err == nil || sched.IsPermanent(r.err) || !errors.As(r.err, &ce) || ce.Info.Code != CodeTranslateFailed ||
		!paramsMatch(&ce.Info, "n", len(ss), "attempts", 3) || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	if n := len(fake.Calls()); n != 3*len(ss) {
		t.Errorf("%d requests, want %d", n, 3*len(ss))
	}
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhStale {
			t.Errorf("sentence %d status %s, want stale", s.Idx, s.ZhStatus)
		}
	}
	fake2 := answerFn(func(idx, _ int, _ string) string { return zhJSON(idx, plainZh[idx]) })
	if r := e.run(e.stage(e.deps(fake2), model.StageTranslate), l.ID); r.err != nil || r.stats.PendingSentences != 0 {
		t.Fatalf("retry: %v %+v", r.err, r.stats)
	}
	if got := targetIdxs(fake2); len(got) != len(ss) {
		t.Errorf("retry translated %d sentences, want %d", len(got), len(ss))
	}
}

// claudeDeps returns Deps on the settings path whose translate job runs on
// an Anthropic provider (the fake server) with modelName.
func (e *env) claudeDeps(lectureID, modelName string) Deps {
	e.t.Helper()
	set, err := settings.Open(e.ctx, e.st, e.cfg)
	e.must(err)
	p, err := e.fake.ConfigureSettings(e.ctx, set, "Claude", model.ProviderAnthropic, modelName)
	e.must(err)
	c := llm.NewWithSettings(set, nil, nil, "", nil)
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	d := e.deps(c)
	d.Settings = set
	d.Concurrency = 1
	for _, stage := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		role := model.RoleOfStage(stage)
		snap := model.LLMSnapshot{ProviderID: p.ID, ProviderName: p.Name, Model: modelName, Params: settings.ResolveParams(role, model.RoleParams{})}
		e.setSnapshot(lectureID, stage, model.MarshalSnapshot(snap))
	}
	return d
}

// On a Claude model the stages send the JSON Schema of their answer
// (structured output); a refused sentence is pending with the category,
// asked once, and does not stop the lecture.
func TestTranslateOnClaudeSchemaAndRefusal(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	ss := e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	d := e.claudeDeps(l.ID, "claude-sonnet-5-5")
	e.fake.FailNext(llmtest.RoleTranslate, 1, llmtest.FailRefusal)
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 1 {
		t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
	}
	reqs := e.fake.Requests(llmtest.RoleTranslate)
	if len(reqs) < len(ss) {
		t.Fatalf("%d requests", len(reqs))
	}
	var schema string
	for i, q := range reqs {
		if q.API != model.ProviderAnthropic || q.Schema == "" || q.Thinking != "between_tools" || q.Effort != "" || !q.JSONMode {
			t.Fatalf("request %d: schema %q thinking %q effort %q", i, q.Schema, q.Thinking, q.Effort)
		}
		if schema == "" {
			schema = q.Schema
		}
		if q.Schema != schema {
			t.Fatalf("request %d has another schema", i)
		}
	}
	if llmtest.CheckSchema([]byte(schema)) != "" || !strings.Contains(schema, `"zh"`) {
		t.Errorf("schema %s", schema)
	}
	refused := reqs[0].TargetIdx
	asked := 0
	for _, q := range reqs {
		if q.TargetIdx == refused {
			asked++
		}
	}
	s := e.sentences(l.ID)[refused]
	if asked != 1 || !isPending(s) || s.QAError == nil || *s.QAError != qa.RefusedReason(llmtest.RefusalCategory) {
		t.Fatalf("refused sentence %d: asked %d times, %+v", refused, asked, s)
	}
	if info := qa.ErrorInfo(*s.QAError); info == nil || info.Code != qa.CodeSentenceRefused || info.Params["category"] != llmtest.RefusalCategory {
		t.Errorf("qa_error_info %+v", info)
	}
	// Usage of the refused request is in the job's stats like any other.
	if r.stats.PromptTokens == 0 {
		t.Errorf("stats %+v", r.stats)
	}

	// The other JSON stages attach their schemas too.
	e.fake.Reset()
	if r := e.run(e.stage(d, model.StageRepair), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	for _, q := range e.fake.Requests(llmtest.RoleRepair) {
		if q.Schema == "" || !strings.Contains(q.Schema, `"patches"`) {
			t.Fatalf("repair schema %q", q.Schema)
		}
	}
	if e.fake.Count(llmtest.RoleRepair) == 0 {
		t.Fatal("no repair request")
	}
}

// A model without structured output (HTTP 400 for the field): one request
// is sent again without the schema, every later one goes without it, and
// the lecture is translated.
func TestTranslateOnClaudeWithoutStructuredOutput(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	ss := e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	d := e.claudeDeps(l.ID, "claude-opus-4-7")
	e.fake.RejectFormat(true)
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.stats.PendingSentences != 0 {
		t.Fatalf("err %v stats %+v", r.err, r.stats)
	}
	// Refused requests never reach the role handler: every recorded request
	// is one without a schema, one per sentence.
	reqs := e.fake.Requests(llmtest.RoleTranslate)
	if len(reqs) != len(ss) {
		t.Fatalf("%d requests for %d sentences", len(reqs), len(ss))
	}
	for i, q := range reqs {
		if q.Schema != "" {
			t.Fatalf("request %d has a schema", i)
		}
	}
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhOK {
			t.Errorf("sentence %d: %s", s.Idx, s.ZhStatus)
		}
	}
}

// askJSON tells the model what was wrong (askJSONTries): an unparsable
// answer, two different JSON values, a check's text; the final error keeps
// the last reason. A refusal is returned at once.
func TestAskJSONRetriesWithFeedback(t *testing.T) {
	type answer struct {
		V int `json:"v"`
	}
	check := func(a *answer) error {
		if a.V == 0 {
			return errors.New("v must not be 0")
		}
		return nil
	}
	answers := []string{"no JSON here", `{"v":1} Wait: {"v":2}`, `{"v":0}`}
	script := &scriptLLM{fn: func(_ context.Context, n int, _ llmCall) (string, error) { return answers[n], nil }}
	_, err := askJSON(context.Background(), script, config.RoleRepair, "sys", "user", check)
	var u *errUnusable
	if !errors.As(err, &u) || u.last == nil || u.last.Error() != "v must not be 0" || codeOf(err) != CodeOutputUnusable {
		t.Fatalf("err %v", err)
	}
	calls := script.Calls()
	if len(calls) != 3 || calls[0].User != "user" {
		t.Fatalf("calls %+v", calls)
	}
	if !strings.HasPrefix(calls[1].User, "user\n\n") || !strings.Contains(calls[1].User, "not a valid JSON object") || strings.Contains(calls[1].User, "more than one JSON value") {
		t.Errorf("after an unparsable answer: %q", calls[1].User)
	}
	if calls[2].User != ctxbuild.WithRetryFeedback("user", []string{ctxbuild.SecondJSONProblem}) {
		t.Errorf("after two JSON values: %q", calls[2].User)
	}
	for _, c := range calls {
		if c.System != "sys" || !c.JSONMode {
			t.Errorf("call %+v", c)
		}
	}

	refusal := sched.Permanent(fmt.Errorf("declined: %w", llm.ErrRefusal))
	script = &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", refusal }}
	if _, err := askJSON(context.Background(), script, config.RoleRepair, "sys", "user", check); !errors.Is(err, llm.ErrRefusal) || isUnusable(err) || len(script.Calls()) != 1 {
		t.Fatalf("refusal: %v after %d calls", err, len(script.Calls()))
	}
}
