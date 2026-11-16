// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// v3.2.x P1-b: the retry of an LLM stage asks when the settings changed, and
// a retry with a model named for that one retry (model choice manual).

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// failing is a stage that fails permanently while on is set.
func failing(name string, res ResourceKind, on *atomic.Bool) *fakeStage {
	return &fakeStage{name: name, res: res, run: func(ctx context.Context, id string, p func(float64)) error {
		if on.Load() {
			return Permanent(errors.New("the model's answer was cut off"))
		}
		return nil
	}}
}

// waitAttempt waits until the stage's job is in status with that attempt.
func (e *env) waitAttempt(lectureID, stage, status string, attempt int) model.Job {
	e.t.Helper()
	e.waitFor(stage+" "+status, 5*time.Second, func() bool {
		j := e.lj(lectureID, stage)
		return j.Status == status && j.Attempt == attempt
	})
	return e.lj(lectureID, stage)
}

func diffStages(ds []state.ModelDiff) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Stage)
	}
	return out
}

// retryChoices runs the checks shared by a failed and a cancelled summary:
// the question, and what each of the three answers writes.
func retryChoices(t *testing.T, e *env, l model.Lecture, p2 model.Provider, status string, stop func()) {
	t.Helper()
	first := e.lj(l.ID, model.StageSummary)
	old := e.snaps(l.ID)
	again := func(n int) { // the retry ran and ended the same way again
		t.Helper()
		e.waitFor("retry started", 5*time.Second, func() bool { return e.lj(l.ID, model.StageSummary).Attempt == first.Attempt+n })
		stop()
		e.waitAttempt(l.ID, model.StageSummary, status, first.Attempt+n)
	}

	// Nothing changed in the settings: the retry asks nothing (A4.2).
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""); err != nil {
		t.Fatalf("retry without a difference: %v", err)
	}
	again(1)

	// A pending downstream stage changed in the settings: the retry asks.
	e.assign(p2.ID, "model-b", model.StageRepair)
	ds := needsChoice(t, e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""))
	if !reflect.DeepEqual(diffStages(ds), []string{model.StageRepair}) || !ds[0].SnapshotUsable || ds[0].LectureID != l.ID ||
		ds[0].Snapshot != old[model.StageRepair] || ds[0].Current != e.current()[model.StageRepair] {
		t.Fatalf("diffs = %+v", ds)
	}
	// The retried stage too (an output limit set on the role).
	limit := 32000
	if err := e.s.set.SetRole(e.ctx, model.RoleSummary, providerOfSnap(old[model.StageSummary]), "model-a", model.RoleParams{MaxTokens: &limit}); err != nil {
		t.Fatal(err)
	}
	ds = needsChoice(t, e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ActionRetry))
	if !reflect.DeepEqual(diffStages(ds), []string{model.StageSummary, model.StageRepair}) {
		t.Fatalf("diffs = %+v", ds)
	}
	if j := e.lj(l.ID, model.StageSummary); j.Status != status || j.Attempt != first.Attempt+1 {
		t.Fatalf("the question changed the job: %s attempt %d", j.Status, j.Attempt)
	}

	// 沿用原模型: every snapshot stays byte for byte.
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	again(2)
	if got := e.snaps(l.ID); !reflect.DeepEqual(got, old) {
		t.Fatalf("snapshot choice changed snapshots:\n%v\n%v", got, old)
	}

	// 临时指定: only the retried job; the role's setting is not written.
	rolesBefore := e.s.set.Roles()
	mt := 64000
	target := settings.ManualTarget{ProviderID: p2.ID, Model: " model-m ", MaxTokens: &mt}
	if err := e.s.RetryManual(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, target); err != nil {
		t.Fatal(err)
	}
	again(3)
	snap, ok := model.ParseLLMSnapshot(e.lj(l.ID, model.StageSummary).ConfigSnapshot)
	if !ok || snap.ProviderID != p2.ID || snap.ProviderName != p2.Name || snap.Model != "model-m" || snap.Source != model.SnapshotSourceManual ||
		snap.Params.MaxTokens == nil || *snap.Params.MaxTokens != mt || snap.Params.Temperature == nil ||
		*snap.Params.Temperature != model.DefaultTemperature(model.RoleSummary) || snap.Params.Thinking != nil ||
		snap.PromptVersion != model.PromptVersionOf(model.StageSummary) {
		t.Fatalf("manual snapshot = %+v", snap)
	}
	for _, s := range []string{model.StageRepair, model.StageTranslate, model.StageExport, model.StageASR, model.StagePrep} {
		if e.lj(l.ID, s).ConfigSnapshot != old[s] {
			t.Errorf("manual choice changed the %s snapshot", s)
		}
	}
	if !reflect.DeepEqual(e.s.set.Roles(), rolesBefore) {
		t.Fatal("manual choice wrote the role settings")
	}

	// 使用当前设置: the retried stage and its pending downstream stages.
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	again(4)
	cur := e.current()
	for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		if got := e.lj(l.ID, s).ConfigSnapshot; got != cur[s] {
			t.Errorf("current choice: %s snapshot %q, want %q", s, got, cur[s])
		}
	}
	if s, _ := model.ParseLLMSnapshot(e.lj(l.ID, model.StageSummary).ConfigSnapshot); s.Source != "" || s.Params.MaxTokens == nil || *s.Params.MaxTokens != limit {
		t.Fatalf("snapshot of the current settings = %+v", s)
	}
	// Everything matches the settings again: no question.
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""); err != nil {
		t.Fatalf("retry with matching snapshots: %v", err)
	}
	again(5)
}

func TestRetryOfFailedLLMStageAsksAndTakesThreeChoices(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newEnv(t, nil, allInstant(failing(model.StageSummary, ResourceAPI, &fail))...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageSummary, model.StatusFailed)
	retryChoices(t, e, l, p2, model.StatusFailed, func() {})

	fail.Store(false)
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
}

func TestRetryOfCancelledLLMStageAsksAndTakesThreeChoices(t *testing.T) {
	g := newGate()
	e := newEnv(t, nil, allInstant(&fakeStage{name: model.StageSummary, res: ResourceAPI, run: g.run})...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	l := e.lecture()
	e.start()
	cancel := func() {
		t.Helper()
		j := e.waitStatus(l.ID, model.StageSummary, model.StatusRunning)
		if err := e.s.Cancel(j.ID); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	e.waitStatus(l.ID, model.StageSummary, model.StatusCancelled)
	retryChoices(t, e, l, p2, model.StatusCancelled, cancel)
}

// The prompt version alone is no difference for a retry: it always runs the
// current prompts, whatever is chosen.
func TestRetryIgnoresPromptVersionDifference(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newEnv(t, nil, allInstant(failing(model.StageSummary, ResourceAPI, &fail))...)
	e.assign(e.provider("p1", 4).ID, "model-a", llmStages...)
	l := e.lecture()
	e.start()
	j := e.waitStatus(l.ID, model.StageSummary, model.StatusFailed)
	snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot)
	if !ok {
		t.Fatalf("summary snapshot %q", j.ConfigSnapshot)
	}
	snap.PromptVersion = 0 // as written before v2
	if err := e.st.SetJobSnapshot(e.ctx, j.ID, model.MarshalSnapshot(snap)); err != nil {
		t.Fatal(err)
	}
	if model.SameSnapshot(model.StageSummary, e.lj(l.ID, model.StageSummary).ConfigSnapshot, e.current()[model.StageSummary]) &&
		model.PromptVersionOf(model.StageSummary) > 1 {
		t.Fatal("the test needs snapshots that differ by their prompt version")
	}
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, ""); err != nil {
		t.Fatalf("retry asked because of the prompt version: %v", err)
	}
}

func TestRetryManualValidation(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newEnv(t, nil, allInstant(failing(model.StageSummary, ResourceAPI, &fail))...)
	p1 := e.provider("p1", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	// a provider without a key that is not a local server
	name, typ, url := "nokey", model.ProviderOpenAI, "https://nokey.example.test/v1"
	noKey, err := e.s.set.CreateProvider(e.ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url})
	if err != nil {
		t.Fatal(err)
	}
	l := e.lecture()
	e.start()
	failed := e.waitStatus(l.ID, model.StageSummary, model.StatusFailed)

	zero, big, hot, high := 0, settings.MaxTokensLimit+1, 2.5, model.ThinkingHigh
	bogus := "extreme"
	cases := []struct {
		name string
		in   settings.ManualTarget
		code string
	}{
		{"unknown provider", settings.ManualTarget{ProviderID: "gone", Model: "m"}, settings.CodeRoleProviderMissing},
		{"no provider", settings.ManualTarget{Model: "m"}, settings.CodeRoleProviderMissing},
		{"empty model", settings.ManualTarget{ProviderID: p1.ID, Model: "  "}, settings.CodeRoleModelMissing},
		{"max_tokens 0", settings.ManualTarget{ProviderID: p1.ID, Model: "m", MaxTokens: &zero}, settings.CodeMaxTokensInvalid},
		{"max_tokens too large", settings.ManualTarget{ProviderID: p1.ID, Model: "m", MaxTokens: &big}, settings.CodeMaxTokensInvalid},
		{"temperature", settings.ManualTarget{ProviderID: p1.ID, Model: "m", Temperature: &hot}, settings.CodeTemperatureInvalid},
		{"thinking level", settings.ManualTarget{ProviderID: p1.ID, Model: "m", Thinking: &bogus}, settings.CodeThinkingInvalid},
		{"thinking unsupported", settings.ManualTarget{ProviderID: p1.ID, Model: "m", Thinking: &high}, settings.CodeThinkingUnsupported},
		{"provider without a key", settings.ManualTarget{ProviderID: noKey.ID, Model: "m"}, settings.CodeSnapshotNoAPIKey},
	}
	for _, c := range cases {
		err := e.s.RetryManual(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, c.in)
		var ce *model.CodedError
		if !errors.Is(err, settings.ErrInvalid) || !errors.As(err, &ce) || ce.Info.Code != c.code {
			t.Errorf("%s: err = %v, want code %s", c.name, err, c.code)
		}
	}
	if j := e.lj(l.ID, model.StageSummary); j.Status != model.StatusFailed || j.Attempt != failed.Attempt || j.ConfigSnapshot != failed.ConfigSnapshot {
		t.Fatalf("a refused manual retry changed the job: %+v", j)
	}

	// manual needs its target, and only LLM stages of a lecture take one
	ok := settings.ManualTarget{ProviderID: p1.ID, Model: "m"}
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceManual); !errors.Is(err, state.ErrBadInput) {
		t.Errorf("Retry with manual: %v", err)
	}
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StagePrep, model.ModelChoiceManual); !errors.Is(err, state.ErrBadInput) {
		t.Errorf("Rerun with manual: %v", err)
	}
	for _, stage := range []string{model.StageASR, model.StagePrep, model.StageExport, model.StageTitle} {
		if err := e.s.RetryManual(e.ctx, model.ScopeLecture, l.ID, stage, ok); !errors.Is(err, state.ErrBadInput) {
			t.Errorf("RetryManual of %s: %v", stage, err)
		}
	}
	if err := e.s.RetryManual(e.ctx, model.ScopeCourse, e.course.ID, model.StageGlossarySeed, ok); !errors.Is(err, state.ErrBadInput) {
		t.Errorf("RetryManual of a course stage: %v", err)
	}
	// a stage that is not failed or cancelled; an unknown lecture
	if err := e.s.RetryManual(e.ctx, model.ScopeLecture, l.ID, model.StageRepair, ok); !errors.Is(err, state.ErrConflict) {
		t.Errorf("RetryManual of a pending stage: %v", err)
	}
	if err := e.s.RetryManual(e.ctx, model.ScopeLecture, "nope", model.StageSummary, ok); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("RetryManual of an unknown lecture: %v", err)
	}
}

// A manual retry of translate keeps the context window (and batch size) of
// the snapshot it replaces and clears the lecture's translations, like the
// current-settings choice does.
func TestRetryManualTranslateKeepsWindowAndClears(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newEnv(t, nil, allInstant(failing(model.StageTranslate, ResourceAPI, &fail))...)
	p1 := e.provider("p1", 4)
	e.assign(p1.ID, "model-a", model.StageSummary, model.StageRepair)
	w, b := 5, 10
	if err := e.s.set.SetRole(e.ctx, model.RoleTranslate, p1.ID, "model-a", model.RoleParams{Window: &w, Batch: &b}); err != nil {
		t.Fatal(err)
	}
	l := e.lecture()
	if err := e.st.InsertSentences(e.ctx, l.ID, []model.Sentence{{Idx: 0, EndMS: 1000, ASRText: "one", RepairedText: "one"}}); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitStatus(l.ID, model.StageTranslate, model.StatusFailed)
	zh, h := "一", "hash"
	if err := e.st.SetTranslation(e.ctx, l.ID, 0, db.SentenceTr{Zh: &zh, ZhStatus: model.ZhOK, ZhHash: &h}); err != nil {
		t.Fatal(err)
	}
	// the settings' window changes meanwhile: the manual retry does not take it
	w2 := 2
	if err := e.s.set.SetRole(e.ctx, model.RoleTranslate, p1.ID, "model-a", model.RoleParams{Window: &w2}); err != nil {
		t.Fatal(err)
	}
	// the same provider, another model: window and batch are kept (review
	// R24: on another provider the batch is the target's default instead,
	// settings/batch_default_test.go)
	if err := e.s.RetryManual(e.ctx, model.ScopeLecture, l.ID, model.StageTranslate, settings.ManualTarget{ProviderID: p1.ID, Model: "model-m"}); err != nil {
		t.Fatal(err)
	}
	snap, _ := model.ParseLLMSnapshot(e.lj(l.ID, model.StageTranslate).ConfigSnapshot)
	if snap.Model != "model-m" || snap.Source != model.SnapshotSourceManual || snap.Params.Window == nil || *snap.Params.Window != w ||
		snap.Params.Batch == nil || *snap.Params.Batch != b {
		t.Fatalf("manual translate snapshot = %+v", snap)
	}
	s, err := e.st.GetSentence(e.ctx, l.ID, 0)
	if err != nil || s.Zh != nil || s.ZhStatus != model.ZhNone {
		t.Fatalf("translations not cleared: %+v err=%v", s, err)
	}
	if !e.hasChanged("sentences", l.ID) {
		t.Fatal("no sentences event after the translations were cleared")
	}
}

// A manual translate retry on another provider keeps the window of the
// snapshot it replaces, but not its batch size: that is the target's
// default (settings.DefaultBatch; none for this OpenAI-compatible provider),
// so a batch chosen for one provider is not carried to another (review R24).
func TestRetryManualTranslateOtherProviderBatch(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newEnv(t, nil, allInstant(failing(model.StageTranslate, ResourceAPI, &fail))...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", model.StageSummary, model.StageRepair)
	w, b := 5, 10
	if err := e.s.set.SetRole(e.ctx, model.RoleTranslate, p1.ID, "model-a", model.RoleParams{Window: &w, Batch: &b}); err != nil {
		t.Fatal(err)
	}
	l := e.lecture()
	if err := e.st.InsertSentences(e.ctx, l.ID, []model.Sentence{{Idx: 0, EndMS: 1000, ASRText: "one", RepairedText: "one"}}); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitStatus(l.ID, model.StageTranslate, model.StatusFailed)
	if before, _ := model.ParseLLMSnapshot(e.lj(l.ID, model.StageTranslate).ConfigSnapshot); before.Params.Batch == nil || *before.Params.Batch != b {
		t.Fatalf("snapshot before the retry = %+v", before)
	}
	if err := e.s.RetryManual(e.ctx, model.ScopeLecture, l.ID, model.StageTranslate, settings.ManualTarget{ProviderID: p2.ID, Model: "model-m"}); err != nil {
		t.Fatal(err)
	}
	snap, _ := model.ParseLLMSnapshot(e.lj(l.ID, model.StageTranslate).ConfigSnapshot)
	if snap.ProviderID != p2.ID || snap.Model != "model-m" || snap.Source != model.SnapshotSourceManual ||
		snap.Params.Window == nil || *snap.Params.Window != w || snap.Params.Batch != nil {
		t.Fatalf("manual translate snapshot on another provider = %+v", snap)
	}
}

// The retry of asr is what it was: it never asks because the settings of a
// downstream LLM stage (or of asr itself) changed.
func TestRetryOfASRStillAsksNothing(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newEnv(t, nil, allInstant(failing(model.StageASR, ResourceGPU, &fail))...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	l := e.lecture()
	e.start()
	failed := e.waitStatus(l.ID, model.StageASR, model.StatusFailed)
	old := e.snaps(l.ID)

	e.assign(p2.ID, "model-b", llmStages...)
	a := e.s.set.ASR()
	a.Language = "de"
	if err := e.s.set.SetASR(e.ctx, a); err != nil {
		t.Fatal(err)
	}
	if model.SameSnapshot(model.StageASR, old[model.StageASR], e.current()[model.StageASR]) {
		t.Fatal("the test needs an asr snapshot that differs from the settings")
	}
	for _, retry := range []func() error{
		func() error { return e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageASR, "") },
		func() error { return e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageASR, model.ActionRetry) },
	} {
		before := e.lj(l.ID, model.StageASR).Attempt
		if err := retry(); err != nil {
			t.Fatalf("asr retry: %v", err)
		}
		e.waitAttempt(l.ID, model.StageASR, model.StatusFailed, before+1)
	}
	if got := e.snaps(l.ID); !reflect.DeepEqual(got, old) {
		t.Fatalf("asr retry changed snapshots:\n%v\n%v", got, old)
	}
	if e.lj(l.ID, model.StageASR).ID != failed.ID {
		t.Fatal("asr retry replaced the job")
	}
	// prep and export are no LLM stages either: a failed export retries silently
}

// "重新翻译这句" with the current settings' model: the done translate job
// takes that model, the other translations stay, the incremental run uses it.
func TestRetranslateSentenceWithCurrentModel(t *testing.T) {
	e := newEnvOpts(t, nil, func(o *Options) { o.IncrementalDebounce = 20 * time.Millisecond }, allInstant()...)
	p1, p2 := e.provider("p1", 4), e.provider("p2", 4)
	e.assign(p1.ID, "model-a", llmStages...)
	l := e.lecture()
	var ss []model.Sentence
	for i := 0; i < 3; i++ {
		ss = append(ss, model.Sentence{Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i+1) * 1000, ASRText: "s", RepairedText: "s"})
	}
	if err := e.st.InsertSentences(e.ctx, l.ID, ss); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	zh, empty, h := "译文", "", "hash"
	for i := 0; i < 2; i++ {
		if err := e.st.SetTranslation(e.ctx, l.ID, i, db.SentenceTr{Zh: &zh, ZhStatus: model.ZhOK, ZhHash: &h}); err != nil {
			t.Fatal(err)
		}
	}
	reason := "no usable answer"
	if err := e.st.SetTranslation(e.ctx, l.ID, 2, db.SentenceTr{Zh: &empty, ZhStatus: model.ZhQAFailed, QAError: &reason}); err != nil {
		t.Fatal(err)
	}
	first := e.lj(l.ID, model.StageTranslate)

	// a translated sentence cannot be sent to another model on its own
	e.assign(p2.ID, "model-b", model.StageTranslate)
	if _, _, err := e.s.RetranslateSentence(e.ctx, l.ID, 0, true); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("translated sentence with the current model: %v", err)
	}
	s, mixed, err := e.s.RetranslateSentence(e.ctx, l.ID, 2, true)
	if err != nil || !mixed || s.ZhStatus != model.ZhStale {
		t.Fatalf("retranslate with the current model: %+v mixed=%v err=%v", s, mixed, err)
	}
	// the incremental run (a new translate job) carries the new model
	e.waitFor("incremental translate", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageTranslate)
		return j.ID != first.ID && j.Status == model.StatusDone
	})
	j := e.lj(l.ID, model.StageTranslate)
	if providerOfSnap(j.ConfigSnapshot) != p2.ID || !state.IsIncremental(j.Stats.Note) {
		t.Fatalf("incremental job: snapshot %q note %q", j.ConfigSnapshot, j.Stats.Note)
	}
	if model.SnapshotWindow(j.ConfigSnapshot) != model.SnapshotWindow(first.ConfigSnapshot) {
		t.Fatal("the context window changed")
	}
	for i := 0; i < 2; i++ {
		if s, _ := e.st.GetSentence(e.ctx, l.ID, i); s.Zh == nil || *s.Zh != zh || s.ZhStatus != model.ZhOK {
			t.Fatalf("sentence %d lost its translation: %+v", i, s)
		}
	}

	// without the option: the plain sentence operation, the model stays
	if _, mixed, err := e.s.RetranslateSentence(e.ctx, l.ID, 0, false); err != nil || mixed {
		t.Fatalf("plain retranslate: mixed=%v err=%v", mixed, err)
	}
	// the translate role not configured: nothing to switch to
	e2 := newEnvOpts(t, nil, func(o *Options) {
		set := settings.New(o.Store, o.Config)
		if err := set.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		o.Settings = set
	}, allInstant()...)
	if _, _, err := e2.s.RetranslateSentence(e2.ctx, "any", 0, true); !errors.Is(err, settings.ErrRoleNotConfigured) {
		t.Fatalf("no translate role: %v", err)
	}
}
