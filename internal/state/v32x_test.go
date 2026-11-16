// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"errors"
	"reflect"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// v3.2.x P1 / P1-b: "重新翻译这句" for a sentence the translate stage left
// without a translation, optionally with the current settings' model.

func (e *reviewEnv) translateSnapshot(raw string) {
	e.t.Helper()
	if err := e.st.SetJobSnapshot(e.ctx, e.job(model.ScopeLecture, e.l.ID, model.StageTranslate).ID, raw); err != nil {
		e.t.Fatal(err)
	}
}

// pending marks a sentence the way the translate stage leaves one it could
// not translate: no text (NULL or ""), qa_failed, the reason.
func (e *reviewEnv) pending(idx int, zh *string) {
	e.t.Helper()
	if err := e.st.SetTranslation(e.ctx, e.l.ID, idx, db.SentenceTr{Zh: zh, ZhStatus: model.ZhQAFailed, QAError: sp("no usable answer")}); err != nil {
		e.t.Fatal(err)
	}
}

func trSnap(provider, name string, window int) string {
	t := 1.3
	return model.MarshalSnapshot(model.LLMSnapshot{ProviderID: provider, ProviderName: provider, Model: name,
		Params: model.RoleParams{Temperature: &t, Window: &window}, PromptVersion: model.PromptVersionOf(model.StageTranslate)})
}

func TestRetranslatePendingSentence(t *testing.T) {
	e := newReviewEnv(t)
	e.pending(3, sp(""))
	e.pending(4, nil)
	for _, idx := range []int{3, 4} {
		s, err := RetranslateSentence(e.ctx, e.st, e.l.ID, idx)
		if err != nil || s.ZhStatus != model.ZhStale {
			t.Fatalf("sentence %d: %+v err=%v", idx, s, err)
		}
	}
	if got := e.stale(); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Fatalf("stale = %v", got)
	}
	// a sentence never translated keeps none (the incremental run takes it anyway)
	if err := e.st.SetTranslation(e.ctx, e.l.ID, 7, db.SentenceTr{ZhStatus: model.ZhNone}); err != nil {
		t.Fatal(err)
	}
	if s, err := RetranslateSentence(e.ctx, e.st, e.l.ID, 7); err != nil || s.ZhStatus != model.ZhNone {
		t.Fatalf("untranslated sentence: %+v err=%v", s, err)
	}
}

func TestRetranslateSentenceWithCurrentSnapshot(t *testing.T) {
	e := newReviewEnv(t)
	old := trSnap("p1", "model-a", 5)
	e.translateSnapshot(old)
	e.pending(3, sp(""))
	next := trSnap("p2", "model-b", 2) // the settings' window differs: not taken

	// only a sentence without a usable translation
	if _, _, err := RetranslateSentenceWith(e.ctx, e.st, e.l.ID, 6, RetranslateOpts{Snapshot: next}); !errors.Is(err, ErrConflict) {
		t.Fatalf("translated sentence: %v", err)
	}
	if got := e.job(model.ScopeLecture, e.l.ID, model.StageTranslate).ConfigSnapshot; got != old {
		t.Fatalf("a refused call changed the snapshot: %s", got)
	}

	s, switched, err := RetranslateSentenceWith(e.ctx, e.st, e.l.ID, 3, RetranslateOpts{Snapshot: next})
	if err != nil || !switched || s.ZhStatus != model.ZhStale {
		t.Fatalf("pending sentence: %+v switched=%v err=%v", s, switched, err)
	}
	got, ok := model.ParseLLMSnapshot(e.job(model.ScopeLecture, e.l.ID, model.StageTranslate).ConfigSnapshot)
	if !ok || got.ProviderID != "p2" || got.Model != "model-b" || got.Params.Window == nil || *got.Params.Window != 5 {
		t.Fatalf("translate snapshot = %+v", got)
	}
	// no other translation was touched
	if st := e.stale(); !reflect.DeepEqual(st, []int{3}) {
		t.Fatalf("stale = %v", st)
	}
	for _, idx := range []int{0, 2, 9} {
		if x := e.sentence(idx); x.Zh == nil || *x.Zh == "" || x.ZhStatus != model.ZhOK || x.ZhHash == nil {
			t.Fatalf("sentence %d: %+v", idx, x)
		}
	}
	// again (now stale; the job already has that model): nothing to switch
	if _, switched, err := RetranslateSentenceWith(e.ctx, e.st, e.l.ID, 3, RetranslateOpts{Snapshot: next}); err != nil || switched {
		t.Fatalf("second call: switched=%v err=%v", switched, err)
	}
	// the incremental run copies the snapshot
	if started, err := StartIncrementalTranslate(e.ctx, e.st, e.l.ID); err != nil || !started {
		t.Fatalf("incremental: %v %v", started, err)
	}
	j := e.job(model.ScopeLecture, e.l.ID, model.StageTranslate)
	if s, _ := model.ParseLLMSnapshot(j.ConfigSnapshot); s.ProviderID != "p2" || j.Status != model.StatusPending {
		t.Fatalf("incremental job: %s %s", j.Status, j.ConfigSnapshot)
	}

	// a translate job that is not done keeps its snapshot (pending here)
	e.pending(8, nil)
	if _, switched, err := RetranslateSentenceWith(e.ctx, e.st, e.l.ID, 8, RetranslateOpts{Snapshot: trSnap("p3", "model-c", 5)}); err != nil || switched {
		t.Fatalf("pending translate job: switched=%v err=%v", switched, err)
	}
	if s, _ := model.ParseLLMSnapshot(e.job(model.ScopeLecture, e.l.ID, model.StageTranslate).ConfigSnapshot); s.ProviderID != "p2" {
		t.Fatalf("snapshot of a job that has not run changed: %+v", s)
	}
	// not a snapshot
	e.pending(9, nil)
	if _, _, err := RetranslateSentenceWith(e.ctx, e.st, e.l.ID, 9, RetranslateOpts{Snapshot: `{"model":""}`}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("invalid snapshot: %v", err)
	}
	if e.sentence(9).ZhStatus != model.ZhQAFailed {
		t.Fatal("a refused call marked the sentence")
	}
}
