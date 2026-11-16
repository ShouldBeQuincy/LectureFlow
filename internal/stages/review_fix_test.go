// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/sched"
)

// refusedErr is what the LLM client returns for a request the provider
// declined (llm.refusalError): permanent, coded llm.refusal, ErrRefusal.
func refusedErr() error {
	return sched.Permanent(model.Errorf(llm.CodeRefusal, map[string]any{"category": "cyber"},
		fmt.Errorf("declined: %w", llm.ErrRefusal)))
}

// captureServiceLog sends the service log to a buffer for the test.
func captureServiceLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// Review fix of v3.2.x P1: a repair chunk the provider declines is skipped
// with a warning, like an unusable one, and asked once; the stage finishes
// and the other chunks are applied.
func TestRepairRefusedChunkIsSkipped(t *testing.T) {
	e := newEnv(t)
	l := synthLecture(e, 90) // chunks [0,40) [40,80) [80,90)
	log := captureServiceLog(t)
	fake := &scriptLLM{fn: func(_ context.Context, _ int, c llmCall) (string, error) {
		first, _ := targetRange(c.User)
		switch first {
		case 0:
			return `{"patches": [` + fixPatch(3, "auto") + `]}`, nil
		case 40:
			return "", refusedErr()
		case 80:
			return `{"patches": [` + fixPatch(85, "auto") + `]}`, nil
		}
		return "", fmt.Errorf("unexpected chunk %d", first)
	}}
	r := e.run(e.stage(e.deps(fake), model.StageRepair), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	if n := len(fake.Calls()); n != 3 {
		t.Errorf("%d LLM calls, want 3 (the declined chunk is not asked again)", n)
	}
	if ps := patchesByIdx(t, e, l.ID); len(ps) != 2 || ps[3].After == "" || ps[85].After == "" {
		t.Errorf("patches %v", ps)
	}
	if n := r.stats.NoteInfo; infoCode(n) != NoteRepairChunksRefused || !paramsMatch(n, "total", 3, "chunks", "2", "ranges", "41–80") {
		t.Errorf("note %+v", n)
	}
	if w := r.stats.WarningInfo; infoCode(w) != WarnRepairRefused || !paramsMatch(w, "ranges", "41–80") ||
		!strings.Contains(r.stats.Warning, "拒绝") {
		t.Errorf("warning %+v %q", w, r.stats.Warning)
	}
	if s := log.String(); !strings.Contains(s, "category=cyber") || !strings.Contains(s, "chunk=1") {
		t.Errorf("service log:\n%s", s)
	}
}

// Declined and unusable chunks in one run: both are reported (the
// unusable ones first, the declined ones as an extra part); every chunk
// declined is the refusal error; declined plus unusable is
// repair.all_chunks_failed.
func TestRepairRefusedAndUnusableChunks(t *testing.T) {
	e := newEnv(t)
	l := synthLecture(e, 90)
	log := captureServiceLog(t)
	fake := &scriptLLM{fn: func(_ context.Context, _ int, c llmCall) (string, error) {
		first, _ := targetRange(c.User)
		switch first {
		case 40:
			return "", refusedErr()
		case 80:
			return "SECRET-LECTURE-WORDS, not JSON", nil
		}
		return `{"patches": [` + fixPatch(3, "auto") + `]}`, nil
	}}
	r := e.run(e.stage(e.deps(fake), model.StageRepair), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	if w := r.stats.WarningInfo; infoCode(w) != WarnRepairUnrepaired || !paramsMatch(w, "ranges", "81–90") ||
		!slices.Contains(extraCodes(w), WarnRepairRefused) {
		t.Errorf("warning %+v", w)
	}
	if n := r.stats.NoteInfo; infoCode(n) != NoteRepairChunksFailed || !slices.Contains(extraCodes(n), NoteRepairChunksRefused) {
		t.Errorf("note %+v", n)
	}
	// The unusable answer is not in the service log (lecture content).
	if s := log.String(); strings.Contains(s, "SECRET-LECTURE-WORDS") || !strings.Contains(s, "problem=not_json") {
		t.Errorf("service log:\n%s", s)
	}

	// Every chunk declined: the refusal error, permanent; nothing written.
	all := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", refusedErr() }}
	l2 := synthLecture(e, 90)
	r = e.run(e.stage(e.deps(all), model.StageRepair), l2.ID)
	if !errors.Is(r.err, llm.ErrRefusal) || !sched.IsPermanent(r.err) || r.job.Status != model.StatusFailed || jobErrorCode(r.job) != llm.CodeRefusal {
		t.Fatalf("all declined: err %v status %s code %s", r.err, r.job.Status, jobErrorCode(r.job))
	}
	if n := len(all.Calls()); n != 3 {
		t.Errorf("all declined: %d calls", n)
	}
	if n, _ := e.st.CountPatches(e.ctx, l2.ID, ""); n != 0 {
		t.Errorf("all declined: %d patches written", n)
	}

	// Declined and unusable, nothing usable: repair.all_chunks_failed.
	none := &scriptLLM{fn: func(_ context.Context, _ int, c llmCall) (string, error) {
		if first, _ := targetRange(c.User); first == 40 {
			return "", refusedErr()
		}
		return "not JSON", nil
	}}
	r = e.run(e.stage(e.deps(none), model.StageRepair), synthLecture(e, 90).ID)
	if r.err == nil || r.job.Status != model.StatusFailed || jobErrorCode(r.job) != CodeRepairAllFailed {
		t.Fatalf("none usable: err %v status %s", r.err, r.job.Status)
	}
}

// Review fix of v3.2.x P1: a material_terms chunk the provider declines is
// skipped with a warning; every chunk declined is the refusal error.
func TestMaterialTermsRefusedChunkIsSkipped(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("cache line ", 1500) // one page per chunk
	m := e.material(nil, "big.pdf", []string{big, big})
	fake := &scriptLLM{fn: func(_ context.Context, n int, _ llmCall) (string, error) {
		if n == 0 {
			return `{"terms": [{"en": "cache line", "zh": "缓存行"}]}`, nil
		}
		return "", refusedErr()
	}}
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageMaterialTerms), m.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	if n := len(fake.Calls()); n != 2 {
		t.Errorf("%d calls, want 2", n)
	}
	if w := r.stats.WarningInfo; infoCode(w) != WarnMaterialTermsRefused || !paramsMatch(w, "chunks", "2", "total", 2, "filename", "big.pdf") ||
		infoCode(r.stats.NoteInfo) != NoteMaterialTermsDone || !slices.Contains(extraCodes(r.stats.NoteInfo), NoteMaterialRefused) || r.stats.Warning == "" {
		t.Errorf("warning %+v note %+v", r.stats.WarningInfo, r.stats.NoteInfo)
	}
	if tm := termMap(e.terms()); tm["cache line"].Zh != "缓存行" {
		t.Errorf("terms %v", tm)
	}

	m2 := e.material(nil, "all.pdf", []string{big, big})
	all := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", refusedErr() }}
	d = e.deps(all)
	d.Concurrency = 1
	r = e.run(e.stage(d, model.StageMaterialTerms), m2.ID)
	if !errors.Is(r.err, llm.ErrRefusal) || !sched.IsPermanent(r.err) || r.job.Status != model.StatusFailed {
		t.Fatalf("all declined: err %v status %s", r.err, r.job.Status)
	}
}

// Review fix of v3.2.x P1: the service log line of a pending sentence holds
// its code, not its reason (which quotes the model's answer).
func TestTranslatePendingLogHoldsNoAnswerText(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	log := captureServiceLog(t)
	fake := answerFn(func(idx, _ int, _ string) string {
		if idx == 2 {
			return "Sorry, I will not translate SECRET-LECTURE-WORDS."
		}
		return zhJSON(idx, plainZh[idx])
	})
	r := e.run(e.stage(e.deps(fake), model.StageTranslate), l.ID)
	if r.err != nil || r.stats.PendingSentences != 1 {
		t.Fatalf("err %v stats %+v", r.err, r.stats)
	}
	s := log.String()
	if strings.Contains(s, "SECRET-LECTURE-WORDS") {
		t.Errorf("the service log holds the answer:\n%s", s)
	}
	if !strings.Contains(s, "sentence left pending") || !strings.Contains(s, "code="+qa.CodeSentenceUnusable) || !strings.Contains(s, "idx=2") {
		t.Errorf("service log:\n%s", s)
	}
	// The reason, with the answer, is still the sentence's qa_error.
	if q := e.sentences(l.ID)[2].QAError; q == nil || !strings.Contains(*q, "SECRET-LECTURE-WORDS") {
		t.Errorf("qa_error %v", q)
	}
}
