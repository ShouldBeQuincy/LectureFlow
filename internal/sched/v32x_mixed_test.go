// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// v3.2.x P1-b: stats.mixed_models of a lecture's translate job.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// A stage cannot change stats.mixed_models through UpdateStats (like the
// token fields), not even through the slice it is handed.
func TestStageCannotChangeMixedModels(t *testing.T) {
	r := bareRun(newTestClock())
	r.stats.MixedModels = []string{"a / x", "b / y"}
	r.Update(func(st *model.JobStats) {
		st.MixedModels[0] = "changed"
		st.MixedModels = append(st.MixedModels, "c / z")
		st.PendingSentences = 2
	})
	st, _ := r.statsSnapshot()
	if !reflect.DeepEqual(st.MixedModels, []string{"a / x", "b / y"}) || st.PendingSentences != 2 {
		t.Fatalf("stats = %+v", st)
	}
	r.Update(func(st *model.JobStats) { st.MixedModels = nil })
	if st, _ := r.statsSnapshot(); len(st.MixedModels) != 2 {
		t.Fatalf("cleared by the stage: %v", st.MixedModels)
	}
}

// "重新翻译这句" with the current model records both models on the translate
// job; the incremental run that follows (a new job, whose stage tries to
// clear the field) keeps them; the plain operation records nothing; a rerun
// of translate with the current model translates everything again and
// drops the record.
func TestRetranslateSentenceRecordsMixedModels(t *testing.T) {
	translate := &fakeStage{name: model.StageTranslate, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		UpdateStats(ctx, func(st *model.JobStats) { st.MixedModels = nil })
		return nil
	}}
	e := newEnvOpts(t, nil, func(o *Options) { o.IncrementalDebounce = 20 * time.Millisecond }, allInstant(translate)...)
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
	zh, empty, h, reason := "译文", "", "hash", "no usable answer"
	for i := 0; i < 2; i++ {
		if err := e.st.SetTranslation(e.ctx, l.ID, i, db.SentenceTr{Zh: &zh, ZhStatus: model.ZhOK, ZhHash: &h}); err != nil {
			t.Fatal(err)
		}
	}
	setPending := func() {
		t.Helper()
		if err := e.st.SetTranslation(e.ctx, l.ID, 2, db.SentenceTr{Zh: &empty, ZhStatus: model.ZhQAFailed, QAError: &reason}); err != nil {
			t.Fatal(err)
		}
	}
	setPending()
	e.assign(p2.ID, "model-b", model.StageTranslate)

	// the plain operation: no record
	if _, mixed, err := e.s.RetranslateSentence(e.ctx, l.ID, 2, false); err != nil || mixed {
		t.Fatalf("plain: mixed=%v err=%v", mixed, err)
	}
	first := e.lj(l.ID, model.StageTranslate)
	if first.Stats.MixedModels != nil {
		t.Fatalf("plain retranslate recorded %v", first.Stats.MixedModels)
	}
	e.waitFor("incremental translate", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageTranslate)
		return j.ID != first.ID && j.Status == model.StatusDone
	})
	if j := e.lj(l.ID, model.StageTranslate); j.Stats.MixedModels != nil || providerOfSnap(j.ConfigSnapshot) != p1.ID {
		t.Fatalf("after the plain operation: %v %s", j.Stats.MixedModels, j.ConfigSnapshot)
	}

	// with the current model
	setPending()
	before := e.lj(l.ID, model.StageTranslate)
	if _, mixed, err := e.s.RetranslateSentence(e.ctx, l.ID, 2, true); err != nil || !mixed {
		t.Fatalf("current: mixed=%v err=%v", mixed, err)
	}
	want := []string{"p1 / model-a", "p2 / model-b"}
	e.waitFor("incremental translate", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageTranslate)
		return j.ID != before.ID && j.Status == model.StatusDone
	})
	if j := e.lj(l.ID, model.StageTranslate); !reflect.DeepEqual(j.Stats.MixedModels, want) {
		t.Fatalf("incremental job: mixed %v, want %v", j.Stats.MixedModels, want)
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)

	// the current setting cannot be used (a provider without a key that is
	// not a local server): refused, nothing changes
	name, typ, url := "nokey", model.ProviderOpenAI, "https://nokey.example.test/v1"
	noKey, err := e.s.set.CreateProvider(e.ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url})
	if err != nil {
		t.Fatal(err)
	}
	e.assign(noKey.ID, "model-c", model.StageTranslate)
	setPending()
	held := e.lj(l.ID, model.StageTranslate)
	_, mixed, err := e.s.RetranslateSentence(e.ctx, l.ID, 2, true)
	var ce *model.CodedError
	if !errors.Is(err, settings.ErrRoleNotConfigured) || !errors.As(err, &ce) || ce.Info.Code != settings.CodeSnapshotNoAPIKey || mixed {
		t.Fatalf("unusable current setting: mixed=%v err=%v", mixed, err)
	}
	if j := e.lj(l.ID, model.StageTranslate); j.ID != held.ID || j.ConfigSnapshot != held.ConfigSnapshot || !reflect.DeepEqual(j.Stats.MixedModels, want) {
		t.Fatalf("a refused call changed the job: %+v", j)
	}
	if s, _ := e.st.GetSentence(e.ctx, l.ID, 2); s.ZhStatus != model.ZhQAFailed {
		t.Fatalf("a refused call marked the sentence: %s", s.ZhStatus)
	}
	// a local server needs no key
	local := "http://127.0.0.1:11434/v1"
	name = "local"
	lp, err := e.s.set.CreateProvider(e.ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &local})
	if err != nil {
		t.Fatal(err)
	}
	e.assign(lp.ID, "model-l", model.StageTranslate)
	if _, mixed, err := e.s.RetranslateSentence(e.ctx, l.ID, 2, true); err != nil || !mixed {
		t.Fatalf("local server: mixed=%v err=%v", mixed, err)
	}
	e.waitFor("incremental translate", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StageTranslate)
		return j.ID != held.ID && j.Status == model.StatusDone
	})
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if got := e.lj(l.ID, model.StageTranslate).Stats.MixedModels; !reflect.DeepEqual(got, append(want, "local / model-l")) {
		t.Fatalf("mixed = %v", got)
	}

	// a rerun of translate: the whole lecture again, one model
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageTranslate, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	if j := e.lj(l.ID, model.StageTranslate); j.Stats.MixedModels != nil {
		t.Fatalf("rerun kept %v", j.Stats.MixedModels)
	}
}
