// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

// Review operations (SPEC §6.4) and sentence-level edits (SPEC §17.1) with the
// sentence-level invalidation of SPEC §7.3.
//
// Signatures FROZEN by the lead for Phase 2 so that web-builder (internal/api)
// and core-builder (bodies) can work in parallel. Bodies: core-builder.
//
// Common contract for every function below:
//   - Everything happens in ONE transaction: update patch, update sentence
//     (human_text), RecomputeText, recompute the translate hash of every
//     sentence in [idx-x, idx+x] with ctxbuild.TranslateHash and mark the ones
//     whose hash differs from zh_hash as zh_status=stale.
//   - x is the context window of the invalidation. x < 0 (what the API
//     passes) means the window of the lecture's current translate job
//     snapshot (model.SnapshotWindow, read in the same transaction; v1.1
//     lead decision 8), so the hashes are computed the way the lecture was
//     translated, not with the current setting. x >= 0 is used as given.
//   - Returns ErrBusy (409) when the lecture's repair or translate job is
//     running (SPEC §4.5), ErrConflict (409) when the patch is not in the
//     required review state, ErrBadInput (400) for empty edit text,
//     db.ErrNotFound (404) for unknown ids.
//   - retranslated = number of sentences newly marked stale. The caller (API)
//     calls sched.RequestIncrementalTranslate(lectureID) after commit when > 0.
//   - Review never modifies the glossary (SPEC §6.4, v1).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// ErrBadInput marks a request that is invalid as such (HTTP 400).
var ErrBadInput = errors.New("bad input")

// ReviewPatch applies accept|reject|edit|reset to a patch (SPEC §6.4).
// accept/reject/edit require review=pending; reset requires an already
// reviewed patch (accepted|rejected|edited) and sets it back to pending.
func ReviewPatch(ctx context.Context, st db.Store, x int, patchID, action, text string) (p model.Patch, retranslated int, err error) {
	edit := strings.TrimSpace(text)
	switch action {
	case model.ReviewActionAccept, model.ReviewActionReject, model.ReviewActionReset:
	case model.ReviewActionEdit:
		if edit == "" {
			return model.Patch{}, 0, fmt.Errorf("%w: edit text is empty", ErrBadInput)
		}
	default:
		return model.Patch{}, 0, fmt.Errorf("%w: unknown review action %q", ErrBadInput, action)
	}
	err = st.Tx(ctx, func(tx db.Store) error {
		pt, err := tx.GetPatch(ctx, patchID)
		if err != nil {
			return err
		}
		if err := ensureReviewable(ctx, tx, pt.LectureID); err != nil {
			return err
		}
		var review string
		var human, edited *string
		switch action {
		case model.ReviewActionReset:
			if !reviewed(pt.Review) {
				return fmt.Errorf("%w: patch is %s, only reviewed patches can be reset", ErrConflict, pt.Review)
			}
			review = model.ReviewPending // human_text = NULL, edited_text = NULL
		default:
			if pt.Review != model.ReviewPending {
				return fmt.Errorf("%w: patch is %s, only pending patches can be reviewed", ErrConflict, pt.Review)
			}
			switch action {
			case model.ReviewActionAccept:
				review, human = model.ReviewAccepted, &pt.After
			case model.ReviewActionReject:
				review, human = model.ReviewRejected, &pt.Before // before is always asr_text
			default:
				review, human, edited = model.ReviewEdited, &edit, &edit
			}
		}
		if err := tx.SetPatchReview(ctx, pt.ID, review, edited); err != nil {
			return err
		}
		n, err := setHumanText(ctx, tx, x, pt.LectureID, pt.SentenceIdx, human)
		if err != nil {
			return err
		}
		retranslated = n
		p, err = tx.GetPatch(ctx, pt.ID)
		return err
	})
	if err != nil {
		return model.Patch{}, 0, err
	}
	return p, retranslated, nil
}

// EditSentence writes human_text = text (trimmed, non-empty) for any sentence;
// a pending patch of that sentence becomes review=edited with edited_text=text (SPEC §17.1).
func EditSentence(ctx context.Context, st db.Store, x int, lectureID string, idx int, text string) (s model.Sentence, retranslated int, err error) {
	edit := strings.TrimSpace(text)
	if edit == "" {
		return model.Sentence{}, 0, fmt.Errorf("%w: text is empty", ErrBadInput)
	}
	err = st.Tx(ctx, func(tx db.Store) error {
		if err := ensureReviewable(ctx, tx, lectureID); err != nil {
			return err
		}
		if _, err := tx.GetSentence(ctx, lectureID, idx); err != nil {
			return err
		}
		pt, err := tx.PatchForSentence(ctx, lectureID, idx)
		switch {
		case err == nil:
			if pt.Review == model.ReviewPending {
				if err := tx.SetPatchReview(ctx, pt.ID, model.ReviewEdited, &edit); err != nil {
					return err
				}
			}
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		if retranslated, err = setHumanText(ctx, tx, x, lectureID, idx, &edit); err != nil {
			return err
		}
		s, err = tx.GetSentence(ctx, lectureID, idx)
		return err
	})
	if err != nil {
		return model.Sentence{}, 0, err
	}
	return s, retranslated, nil
}

// ResetSentence clears human_text (SPEC §17.1). If the sentence's patch was
// reviewed it goes back to review=pending (same semantics as review reset).
func ResetSentence(ctx context.Context, st db.Store, x int, lectureID string, idx int) (s model.Sentence, retranslated int, err error) {
	err = st.Tx(ctx, func(tx db.Store) error {
		if err := ensureReviewable(ctx, tx, lectureID); err != nil {
			return err
		}
		if _, err := tx.GetSentence(ctx, lectureID, idx); err != nil {
			return err
		}
		pt, err := tx.PatchForSentence(ctx, lectureID, idx)
		switch {
		case err == nil:
			if reviewed(pt.Review) {
				if err := tx.SetPatchReview(ctx, pt.ID, model.ReviewPending, nil); err != nil {
					return err
				}
			}
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		if retranslated, err = setHumanText(ctx, tx, x, lectureID, idx, nil); err != nil {
			return err
		}
		s, err = tx.GetSentence(ctx, lectureID, idx)
		return err
	})
	if err != nil {
		return model.Sentence{}, 0, err
	}
	return s, retranslated, nil
}

// RetranslateSentence marks this one sentence zh_status=stale (SPEC §9.6, §17.1).
// A sentence that was never translated (zh NULL, zh_status none) keeps
// zh_status=none, which the incremental run processes as well. A sentence
// left "待处理" by the translate stage (v3.2.x P1: no usable answer after the
// business retries, zh_status qa_failed with an empty or NULL zh) becomes
// stale too: the stage does not send those again by itself.
func RetranslateSentence(ctx context.Context, st db.Store, lectureID string, idx int) (s model.Sentence, err error) {
	s, _, err = RetranslateSentenceWith(ctx, st, lectureID, idx, RetranslateOpts{})
	return s, err
}

// RetranslateOpts are the options of RetranslateSentenceWith.
type RetranslateOpts struct {
	// Snapshot (v3.2.x P1-b, Q10a), when not empty, is the translate
	// snapshot of the CURRENT settings: the sentence is translated again by
	// that model, and so is every later incremental run of the lecture. The
	// lecture's other translations are kept (unlike a retry or rerun with
	// another model, which clears them), so the lecture then mixes the
	// translations of two models. Only for a sentence the model could not
	// translate (zh_status qa_failed, or stale after an earlier call);
	// anything else → ErrConflict. The context window stays the one the
	// lecture was translated with (every zh_hash is built with it).
	Snapshot string
}

// RetranslateSentenceWith is RetranslateSentence with options. switched
// reports whether the lecture's translate job got o.Snapshot: only a done
// translate job is changed (the incremental run that follows copies its
// snapshot, StartIncrementalTranslate) and only when the model, params or
// prompt version differ; a job that has not finished keeps its own snapshot
// and the choice it will be given when it is retried or rerun.
func RetranslateSentenceWith(ctx context.Context, st db.Store, lectureID string, idx int, o RetranslateOpts) (s model.Sentence, switched bool, err error) {
	err = st.Tx(ctx, func(tx db.Store) error {
		switched = false
		if err := ensureReviewable(ctx, tx, lectureID); err != nil {
			return err
		}
		cur, err := tx.GetSentence(ctx, lectureID, idx)
		if err != nil {
			return err
		}
		if !model.IsEmptySnapshot(o.Snapshot) {
			if cur.ZhStatus != model.ZhQAFailed && cur.ZhStatus != model.ZhStale {
				return fmt.Errorf("%w: sentence %d is %s; only a sentence without a usable translation can be sent to another model", ErrConflict, idx, cur.ZhStatus)
			}
			if switched, err = mixTranslateSnapshot(ctx, tx, lectureID, o.Snapshot); err != nil {
				return err
			}
		}
		if cur.ZhStatus != model.ZhStale && (cur.Zh != nil || cur.ZhStatus == model.ZhQAFailed) {
			if err := tx.SetZhStatus(ctx, lectureID, []int{idx}, model.ZhStale); err != nil {
				return err
			}
		}
		s, err = tx.GetSentence(ctx, lectureID, idx)
		return err
	})
	if err != nil {
		return model.Sentence{}, false, err
	}
	return s, switched, nil
}

// mixTranslateSnapshot writes snapshot (an LLM snapshot of the current
// settings) on the lecture's done translate job, with the context window of
// the snapshot it replaces. No translation is cleared. When the provider or
// the model changes, the job's stats.mixed_models gets the old and the new
// model (MixedModelsWith).
func mixTranslateSnapshot(ctx context.Context, tx db.Store, lectureID, snapshot string) (bool, error) {
	next, ok := model.ParseLLMSnapshot(snapshot)
	if !ok {
		return false, fmt.Errorf("%w: not a translate snapshot", ErrBadInput)
	}
	tr, err := tx.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageTranslate)
	if err != nil {
		return false, err
	}
	if tr.Status != model.StatusDone {
		return false, nil
	}
	w := model.SnapshotWindow(tr.ConfigSnapshot)
	next.Params.Window = &w
	raw := model.MarshalSnapshot(next)
	if model.SameSnapshot(model.StageTranslate, tr.ConfigSnapshot, raw) {
		return false, nil
	}
	if err := tx.SetJobSnapshot(ctx, tr.ID, raw); err != nil {
		return false, err
	}
	if old, ok := model.ParseLLMSnapshot(tr.ConfigSnapshot); ok && (old.ProviderID != next.ProviderID || old.Model != next.Model) {
		stats := tr.Stats
		stats.MixedModels = MixedModelsWith(tr.Stats.MixedModels, ModelLabel(old), ModelLabel(next))
		if err := tx.SetJobStats(ctx, tr.ID, stats); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ModelLabel is how stats.mixed_models names the model of an LLM snapshot:
// "provider name / model" (the provider id when the snapshot has no name).
func ModelLabel(s model.LLMSnapshot) string {
	name := strings.TrimSpace(s.ProviderName)
	if name == "" {
		name = s.ProviderID
	}
	return name + " / " + s.Model
}

// MixedModelsWith returns list with each of labels appended that it does
// not hold yet (stable order, no duplicates). A result with fewer than two
// labels means one model: nil.
func MixedModelsWith(list []string, labels ...string) []string {
	out := slices.Clone(list)
	for _, l := range labels {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

func reviewed(review string) bool {
	return review == model.ReviewAccepted || review == model.ReviewRejected || review == model.ReviewEdited
}

// ensureReviewable refuses review operations while repair or translate runs
// (SPEC §4.5): ErrBusy (409).
func ensureReviewable(ctx context.Context, tx db.Store, lectureID string) error {
	jobs, err := tx.ListCurrentJobs(ctx, model.ScopeLecture, lectureID)
	if err != nil {
		return err
	}
	if !Reviewable(jobs) {
		return fmt.Errorf("%w (repair or translate)", ErrBusy)
	}
	return nil
}

// setHumanText writes human_text of one sentence, recomputes its text and
// runs the sentence-level invalidation of SPEC §7.3. Returns the number of
// sentences newly marked stale.
func setHumanText(ctx context.Context, tx db.Store, x int, lectureID string, idx int, human *string) (int, error) {
	if err := tx.SetHumanText(ctx, lectureID, idx, human); err != nil {
		return 0, err
	}
	if err := tx.RecomputeText(ctx, lectureID, []int{idx}); err != nil {
		return 0, err
	}
	return invalidateWindow(ctx, tx, x, lectureID, idx)
}

// invalidateWindow recomputes the translate hash (ctxbuild.TranslateHash) of
// every sentence in [idx-x, idx+x] and marks those whose hash differs from
// zh_hash as stale (SPEC §7.3 first rule). Sentences never translated (zh
// NULL) keep zh_status=none; sentences already stale are not counted.
// x < 0: the window of the translate snapshot (see the contract above).
func invalidateWindow(ctx context.Context, tx db.Store, x int, lectureID string, idx int) (int, error) {
	snap, rows, err := loadSnapshot(ctx, tx, lectureID, false)
	if err != nil {
		return 0, err
	}
	if x < 0 {
		switch x, err = TranslateWindow(ctx, tx, lectureID); {
		case errors.Is(err, db.ErrNotFound):
			x = model.DefaultTranslateWindow
		case err != nil:
			return 0, err
		}
	}
	lo, hi := max(0, idx-x), min(len(rows)-1, idx+x)
	var stale []int
	for i := lo; i <= hi; i++ {
		r := rows[i]
		if r.Zh == nil || r.ZhStatus == model.ZhStale {
			continue
		}
		if h := ctxbuild.TranslateHash(snap, i, x); r.ZhHash == nil || *r.ZhHash != h {
			stale = append(stale, i)
		}
	}
	if err := tx.SetZhStatus(ctx, lectureID, stale, model.ZhStale); err != nil {
		return 0, err
	}
	return len(stale), nil
}

// LoadSnapshot loads the ctxbuild.LectureSnapshot of a lecture: all
// sentences (by idx), every term of the course (ctxbuild ignores disabled
// ones), the summary ("" before the summary stage) and the lecture's
// material set (lecture kinds, lead freeze 1.5: bound to it ∪ bound to its
// teaching week ∪ course-wide; upload order, with pages) — every one of
// them passes ctxbuild.SelectMaterials, so the slides text and the input
// hashes follow the set. Stages that write zh_hash should build their
// snapshot the same way so their hashes match the ones the review
// operations compute.
func LoadSnapshot(ctx context.Context, st db.Store, lectureID string) (ctxbuild.LectureSnapshot, error) {
	snap, _, err := loadSnapshot(ctx, st, lectureID, true)
	return snap, err
}

func loadSnapshot(ctx context.Context, st db.Store, lectureID string, withMaterials bool) (ctxbuild.LectureSnapshot, []model.Sentence, error) {
	lec, err := st.GetLecture(ctx, lectureID)
	if err != nil {
		return ctxbuild.LectureSnapshot{}, nil, err
	}
	rows, err := st.ListSentences(ctx, lectureID, 0, 0)
	if err != nil {
		return ctxbuild.LectureSnapshot{}, nil, err
	}
	for i, r := range rows {
		if r.Idx != i {
			return ctxbuild.LectureSnapshot{}, nil, fmt.Errorf("lecture %s: sentence idx not contiguous at %d (found %d)", lectureID, i, r.Idx)
		}
	}
	terms, err := st.ListTerms(ctx, lec.CourseID)
	if err != nil {
		return ctxbuild.LectureSnapshot{}, nil, err
	}
	summary := ""
	switch sm, err := st.GetSummary(ctx, lectureID); {
	case err == nil:
		summary = sm.Text
	case !errors.Is(err, db.ErrNotFound):
		return ctxbuild.LectureSnapshot{}, nil, err
	}
	snap := ctxbuild.LectureSnapshot{LectureID: lectureID, Sentences: ctxbuild.FromModel(rows), Terms: terms, Summary: summary}
	if withMaterials {
		if snap.Materials, err = lectureMaterials(ctx, st, lec); err != nil {
			return ctxbuild.LectureSnapshot{}, nil, err
		}
	}
	return snap, rows, nil
}
