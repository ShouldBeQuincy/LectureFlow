// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/qa"
)

// Follow-ups of the security review SEC1 of v3.2.x line A.

// SEC1 L5: every answer is valid JSON with an empty "zh". The sentences have
// no translation, so they are pending like the unusable ones: not one
// sentence of the run was translated, the stage fails and the sentences go
// back to stale (before the fix: done, no warning, pending_sentences 0).
func TestTranslateEmptyZhEverySentenceFailsStage(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	d := e.deps(nil)
	d.Concurrency = 1
	ss := e.sentences(l.ID)
	for _, s := range ss {
		e.fake.SetTranslation(s.Idx, "")
	}
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	var ce *model.CodedError
	if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != CodeTranslateFailed || r.job.Status != model.StatusFailed ||
		!paramsMatch(&ce.Info, "n", len(ss)) {
		t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
	}
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhStale {
			t.Errorf("sentence %d status %s, want stale", s.Idx, s.ZhStatus)
		}
	}
}

// SEC1 L5: one sentence gets empty translations, the others are fine: the
// stage is done with a warning and the sentence is counted as pending, with
// the QA reason of SPEC §7 as its qa_error.
func TestTranslateEmptyZhOneSentenceIsPending(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	d := e.deps(nil)
	d.Concurrency = 1
	idx := e.sentences(l.ID)[2].Idx
	e.fake.SetTranslation(idx, "")
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 1 || r.stats.Warning == "" ||
		!paramsMatch(r.stats.WarningInfo, "n", 1, "ids", fmt.Sprintf("#%d", idx)) {
		t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
	}
	var s model.Sentence
	for _, x := range e.sentences(l.ID) {
		if x.Idx == idx {
			s = x
		}
	}
	if !isPending(s) || s.QAError == nil || !strings.Contains(*s.QAError, qa.Failure{Code: qa.CodeEmpty}.Text()) {
		t.Fatalf("sentence %d: %+v", idx, s)
	}
}

// stagesLog captures the service log (slog) of a test.
func stagesLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// SEC1 L4: a repair patch without idx is dropped; the service log names its
// length, not the sentence the model wrote.
func TestRepairPatchWithoutIdxLogsNoText(t *testing.T) {
	log := stagesLog(t)
	e := newEnv(t)
	l := synthLecture(e, 10)
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) {
		return `{"patches": [{"after": "SECRET-LECTURE-SENTENCE about the cache.", "class": "auto", "reason": "no idx"}]}`, nil
	}}
	if r := e.run(e.stage(e.deps(fake), model.StageRepair), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	s := log.String()
	if !strings.Contains(s, "patch without idx dropped") || !strings.Contains(s, "after_chars=") || strings.Contains(s, "SECRET-LECTURE-SENTENCE") {
		t.Errorf("service log:\n%s", s)
	}
}

// SEC1 L4: a title answer that breaks the limits is rejected three times;
// the retry feedback quotes the title, the service log does not (nor does
// the error since r4 D1: TestTitleRejectedErrorHoldsNoGeneratedText).
func TestTitleCheckRejectionLogsNoText(t *testing.T) {
	log := stagesLog(t)
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("CS101 lecture 2.mp4")
	long := strings.Repeat("SECRETWORD ", 40)
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) {
		return fmt.Sprintf(`{"title_en": %q, "title_zh": "标题", "blurb_en": "A blurb.", "blurb_zh": "简介。", "kind": ""}`, long), nil
	}}
	r := e.runTitle(e.deps(fake), l.ID)
	if r.err == nil {
		t.Fatal("title answer over the limit was accepted")
	}
	s := log.String()
	if !strings.Contains(s, `msg="llm answer rejected"`) || !strings.Contains(s, "kind=check") || strings.Contains(s, "SECRETWORD") {
		t.Errorf("service log:\n%s", s)
	}
}
