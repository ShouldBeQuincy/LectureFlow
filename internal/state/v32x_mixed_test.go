// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

// v3.2.x P1-b: stats.mixed_models of the lecture's translate job records the
// models its translations come from after a pending sentence was sent to the
// current setting's model.

func (e *reviewEnv) mixed() []string {
	e.t.Helper()
	return e.lj(e.l.ID, model.StageTranslate).Stats.MixedModels
}

func (e *reviewEnv) retranslateWith(idx int, snap string) bool {
	e.t.Helper()
	_, switched, err := RetranslateSentenceWith(e.ctx, e.st, e.l.ID, idx, RetranslateOpts{Snapshot: snap})
	if err != nil {
		e.t.Fatalf("retranslate %d: %v", idx, err)
	}
	return switched
}

func TestMixedModelsRecordedAndCarried(t *testing.T) {
	e := newReviewEnv(t)
	e.translateSnapshot(trSnap("p1", "model-a", 5))
	e.pending(3, sp(""))

	// without a snapshot (the plain operation): nothing recorded
	if e.retranslateWith(3, "") || e.mixed() != nil {
		t.Fatalf("plain retranslate recorded %v", e.mixed())
	}
	// same provider and model, other params: the snapshot changes, no mix
	e.pending(3, sp(""))
	hot := 0.5
	w := 5
	sameModel := model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "p1", ProviderName: "p1", Model: "model-a",
		Params: model.RoleParams{Temperature: &hot, Window: &w}, PromptVersion: model.PromptVersionOf(model.StageTranslate)})
	if !e.retranslateWith(3, sameModel) || e.mixed() != nil {
		t.Fatalf("params only: mixed %v", e.mixed())
	}
	// another model: both are recorded, the old one first
	if !e.retranslateWith(3, trSnap("p2", "model-b", 5)) {
		t.Fatal("not switched")
	}
	want := []string{"p1 / model-a", "p2 / model-b"}
	if got := e.mixed(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed = %v, want %v", got, want)
	}
	// the same model again: nothing to switch, nothing added
	if e.retranslateWith(3, trSnap("p2", "model-b", 5)) || !reflect.DeepEqual(e.mixed(), want) {
		t.Fatalf("repeat: %v", e.mixed())
	}

	// the incremental run keeps the translations, so the record too
	old := e.lj(e.l.ID, model.StageTranslate)
	if started, err := StartIncrementalTranslate(e.ctx, e.st, e.l.ID); err != nil || !started {
		t.Fatalf("incremental: %v %v", started, err)
	}
	if j := e.lj(e.l.ID, model.StageTranslate); j.ID == old.ID || !IsIncremental(j.Stats.Note) || !reflect.DeepEqual(j.Stats.MixedModels, want) {
		t.Fatalf("incremental job stats = %+v", j.Stats)
	}
	// a restart while it runs (§4.7): requeued, the record stays
	e.drive(model.ScopeLecture, e.l.ID, model.StageTranslate, model.StatusQueued, model.StatusRunning)
	if err := Recover(e.ctx, e.st, e.dataDir); err != nil {
		t.Fatal(err)
	}
	if j := e.lj(e.l.ID, model.StageTranslate); j.Status != model.StatusQueued || !reflect.DeepEqual(j.Stats.MixedModels, want) {
		t.Fatalf("after Recover: %s %v", j.Status, j.Stats.MixedModels)
	}
	e.drive(model.ScopeLecture, e.l.ID, model.StageTranslate, model.StatusRunning, model.StatusDone)

	// a third model, then back to the first: stable order, no duplicates
	e.pending(8, nil)
	e.retranslateWith(8, trSnap("p3", "model-c", 5))
	e.pending(8, nil)
	e.retranslateWith(8, trSnap("p1", "model-a", 5))
	want = []string{"p1 / model-a", "p2 / model-b", "p3 / model-c"}
	if got := e.mixed(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed = %v, want %v", got, want)
	}

	// a retry with the same model keeps the translations and the record
	if started, err := StartIncrementalTranslate(e.ctx, e.st, e.l.ID); err != nil || !started {
		t.Fatalf("incremental: %v %v", started, err)
	}
	e.drive(model.ScopeLecture, e.l.ID, model.StageTranslate, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	if err := RetryWith(e.ctx, e.st, e.dataDir, model.ScopeLecture, e.l.ID, model.StageTranslate, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.mixed(); !reflect.DeepEqual(got, want) {
		t.Fatalf("retry without a model change: %v", got)
	}
	// a retry with another model clears the translations: the record goes
	e.drive(model.ScopeLecture, e.l.ID, model.StageTranslate, model.StatusRunning, model.StatusFailed)
	if err := RetryWith(e.ctx, e.st, e.dataDir, model.ScopeLecture, e.l.ID, model.StageTranslate,
		map[string]string{model.StageTranslate: trSnap("p4", "model-d", 5)}); err != nil {
		t.Fatal(err)
	}
	if j := e.lj(e.l.ID, model.StageTranslate); j.Stats.MixedModels != nil || j.Stats.Note != "" {
		t.Fatalf("after a retry with another model: %+v", j.Stats)
	}
	if s := e.sentence(0); s.Zh != nil {
		t.Fatal("translations not cleared")
	}
}

// A rerun of translate (or of a stage before it) translates the whole
// lecture again: the new job has no record.
func TestMixedModelsDroppedByRerun(t *testing.T) {
	for _, stage := range []string{model.StageTranslate, model.StageRepair} {
		e := newReviewEnv(t)
		e.translateSnapshot(trSnap("p1", "model-a", 5))
		e.pending(3, sp(""))
		e.retranslateWith(3, trSnap("p2", "model-b", 5))
		if len(e.mixed()) != 2 {
			t.Fatalf("mixed = %v", e.mixed())
		}
		if err := Rerun(e.ctx, e.st, e.dataDir, model.ScopeLecture, e.l.ID, stage); err != nil {
			t.Fatalf("rerun %s: %v", stage, err)
		}
		if got := e.mixed(); got != nil {
			t.Fatalf("rerun %s kept %v", stage, got)
		}
	}
	// a rerun of export alone keeps the translate job and its record
	e := newReviewEnv(t)
	e.translateSnapshot(trSnap("p1", "model-a", 5))
	e.pending(3, sp(""))
	e.retranslateWith(3, trSnap("p2", "model-b", 5))
	if err := Rerun(e.ctx, e.st, e.dataDir, model.ScopeLecture, e.l.ID, model.StageExport); err != nil {
		t.Fatal(err)
	}
	if len(e.mixed()) != 2 {
		t.Fatalf("export rerun: %v", e.mixed())
	}
}

func TestMixedModelsWith(t *testing.T) {
	if got := MixedModelsWith(nil, "a / x", "a / x"); got != nil {
		t.Fatalf("one model: %v", got)
	}
	if got := MixedModelsWith([]string{"a / x", "b / y"}, "b / y", "a / x"); !reflect.DeepEqual(got, []string{"a / x", "b / y"}) {
		t.Fatalf("known models: %v", got)
	}
	if got := ModelLabel(model.LLMSnapshot{ProviderID: "id1", Model: "m"}); got != "id1 / m" {
		t.Fatalf("label without a provider name: %q", got)
	}
}
