// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// v3.2.x P1-b: "使用当前设置" for a retry or a rerun needs a usable current
// setting (the check a model named by hand gets): otherwise it is refused
// with the reason's code and nothing changes.

import (
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// keyless creates a provider on a remote address without an API key.
func (e *env) keyless(name string) model.Provider {
	e.t.Helper()
	typ, url, key, conc := model.ProviderOpenAI, "https://"+name+".example.test/v1", "", 4
	p, err := e.s.set.CreateProvider(e.ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key, MaxConcurrency: &conc})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// wantUnusableCurrent checks the refusal of "使用当前设置": a conflict
// (state.ErrSnapshotUnusable) coded with the reason of the stage's setting.
func wantUnusableCurrent(t *testing.T, err error, provider string) {
	t.Helper()
	var ce *model.CodedError
	if !errors.Is(err, state.ErrSnapshotUnusable) || !errors.Is(err, state.ErrConflict) || !errors.As(err, &ce) ||
		ce.Info.Code != settings.CodeSnapshotNoAPIKey || ce.Info.Params["provider"] != provider || ce.Info.Detail == "" {
		t.Fatalf("current choice with an unusable setting: %v (%+v)", err, ce)
	}
}

func TestCurrentChoiceNeedsAUsableSetting(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newEnv(t, nil, allInstant(failing(model.StageSummary, ResourceAPI, &fail))...)
	p1, p3 := e.provider("p1", 4), e.keyless("p3")
	e.assign(p1.ID, "model-a", llmStages...)
	l := e.lecture()
	e.start()
	failed := e.waitStatus(l.ID, model.StageSummary, model.StatusFailed)
	old := e.snaps(l.ID)

	unchanged := func(what string) {
		t.Helper()
		if got := e.snaps(l.ID); !reflect.DeepEqual(got, old) {
			t.Fatalf("%s changed snapshots:\n%v\n%v", what, got, old)
		}
		if j := e.lj(l.ID, model.StageSummary); j.Status != failed.Status || j.Attempt != failed.Attempt {
			t.Fatalf("%s changed the job: %s attempt %d", what, j.Status, j.Attempt)
		}
	}

	// The retried stage's current setting has no key.
	e.assign(p3.ID, "model-c", model.StageSummary)
	wantUnusableCurrent(t, e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceCurrent), p3.Name)
	unchanged("retry of summary")
	// A model named by hand on that provider is refused for the same reason.
	err := e.s.RetryManual(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, settings.ManualTarget{ProviderID: p3.ID, Model: "model-c"})
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Code != settings.CodeSnapshotNoAPIKey {
		t.Fatalf("manual choice on a provider without a key: %v", err)
	}
	unchanged("manual retry")

	// A pending downstream stage's current setting has no key: refused too,
	// the summary's own snapshot is not replaced either.
	e.assign(p1.ID, "model-a", model.StageSummary)
	e.assign(p3.ID, "model-c", model.StageTranslate)
	wantUnusableCurrent(t, e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceCurrent), p3.Name)
	unchanged("retry with an unusable downstream setting")

	// Usable again: the retry takes the current settings and the lecture
	// finishes.
	e.assign(p1.ID, "model-b", model.StageTranslate)
	fail.Store(false)
	if err := e.s.Retry(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceCurrent); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	cur := e.current()
	for _, s := range llmStages {
		if got := e.lj(l.ID, s).ConfigSnapshot; got != cur[s] {
			t.Errorf("%s snapshot %q, want %q", s, got, cur[s])
		}
	}

	// A rerun with the current settings is checked the same way (here a
	// downstream stage of the rerun).
	done := e.snaps(l.ID)
	e.assign(p3.ID, "model-c", model.StageRepair)
	wantUnusableCurrent(t, e.s.Rerun(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ModelChoiceCurrent), p3.Name)
	if got := e.snaps(l.ID); !reflect.DeepEqual(got, done) {
		t.Fatalf("refused rerun changed snapshots:\n%v\n%v", got, done)
	}
	for _, s := range []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
		if j := e.lj(l.ID, s); j.Status != model.StatusDone {
			t.Fatalf("refused rerun changed %s: %s", s, j.Status)
		}
	}
}
