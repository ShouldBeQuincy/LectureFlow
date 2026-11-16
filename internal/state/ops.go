// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Retry is the user "retry" action (SPEC §4.6): failed|cancelled → queued on
// the same job, attempt+1, error cleared, config_snapshot kept (v1.1 A4.2).
// Any other status → ErrConflict. A missing job → db.ErrNotFound.
// Retry(...) is RetryWith(..., nil) (no snapshot is replaced, so no file
// is touched and no data directory is needed).
func Retry(ctx context.Context, st db.Store, scope, scopeID, stage string) error {
	return RetryWith(ctx, st, "", scope, scopeID, stage, nil)
}

// AutoRetry is the scheduler's automatic retry (SPEC §4.2, §4.7):
// failed → queued on the same job, attempt+1. The last error text is kept
// so the UI can show why it is being retried. Refused (ErrConflict) unless
// j is failed with model.CountedAttempts(j) < model.MaxAutoRetries (power
// management: requeues by the power policy do not count).
func AutoRetry(ctx context.Context, st db.Store, j model.Job) error {
	if j.Status != model.StatusFailed || model.CountedAttempts(j) >= model.MaxAutoRetries {
		return fmt.Errorf("%w: job %s (%s, attempt %d) is not auto-retryable", ErrConflict, j.ID, j.Status, j.Attempt)
	}
	return requeue(ctx, st, j, false)
}

// FailJob records the failure of a running job: running → failed with
// errText and, when retry is true and model.CountedAttempts (attempt minus
// the power policy's requeues, job.stats.power_interrupts) is below
// model.MaxAutoRetries, on to queued (attempt+1, error text kept) in the
// SAME transaction. "failed" is
// therefore only ever committed as a final state (SPEC §4.2 + NOTES.md).
// Returns whether the job was requeued. ErrConflict when the job is not the
// current running job.
func FailJob(ctx context.Context, st db.Store, jobID, errText string, retry bool) (bool, error) {
	requeued := false
	err := st.Tx(ctx, func(tx db.Store) error {
		if err := TransitionJob(ctx, tx, jobID, model.StatusRunning, model.StatusFailed, db.JobFinish{Error: &errText}); err != nil {
			return err
		}
		if !retry {
			return nil
		}
		j, err := tx.GetJob(ctx, jobID)
		if err != nil {
			return err
		}
		if model.CountedAttempts(j) >= model.MaxAutoRetries {
			return nil
		}
		if err := requeue(ctx, tx, j, false); err != nil {
			return err
		}
		requeued = true
		return nil
	})
	return requeued && err == nil, err
}

func requeue(ctx context.Context, st db.Store, j model.Job, clearError bool) error {
	attempt := j.Attempt + 1
	zero := 0.0
	f := db.JobFinish{Attempt: &attempt, Progress: &zero}
	if clearError {
		empty := ""
		f.Error = &empty
	}
	return TransitionJob(ctx, st, j.ID, j.Status, model.StatusQueued, f)
}

// RecordPromptVersion writes the current prompt version of the job's stage
// into its config_snapshot (v2 G5.5, lead Gate 2): the scheduler calls it in
// the transaction that moves the job to running, so the snapshot always
// records the prompt the job ran with (stages always use the current
// prompts). Only prompt_version changes; non-LLM stages and empty or invalid
// snapshots are left alone. Jobs that never ran keep their version, so the
// rerun decision still shows "提示词 v1 → v2" for them.
func RecordPromptVersion(ctx context.Context, st db.Store, jobID string) error {
	j, err := st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if snap := withPromptVersion(j.Stage, j.ConfigSnapshot, model.PromptVersionOf(j.Stage)); snap != j.ConfigSnapshot {
		return st.SetJobSnapshot(ctx, jobID, snap)
	}
	return nil
}

// CancelQueued cancels a queued job directly (queued → cancelled). Running
// jobs are cancelled by the scheduler (context cancel, then running →
// cancelled after Run returns). Anything else → ErrConflict.
func CancelQueued(ctx context.Context, st db.Store, jobID string) error {
	return TransitionJob(ctx, st, jobID, model.StatusQueued, model.StatusCancelled, db.JobFinish{})
}

// NoteIncremental is the prefix of stats.note of a translate job created by
// StartIncrementalTranslate. The full note is "incremental:" + the previous
// translate job's input_hash (the stage-level prefix hash of the round that
// completed), or just "incremental" when that job had no input_hash
// (Gate B round 2). See IncrementalBaseHash.
const NoteIncremental = "incremental"

// IsIncremental reports whether a translate job's stats.note marks an
// incremental run (either form of the note).
func IsIncremental(note string) bool {
	return note == NoteIncremental || strings.HasPrefix(note, NoteIncremental+":")
}

// IncrementalBaseHash extracts the previous round's input_hash from an
// incremental translate note. ok is false when note is not of the form
// "incremental:<hash>" with a non-empty hash (including the plain
// "incremental" note): then there is nothing to compare against.
func IncrementalBaseHash(note string) (hash string, ok bool) {
	h, found := strings.CutPrefix(note, NoteIncremental+":")
	if !found || h == "" {
		return "", false
	}
	return h, true
}

// newPending retires the current job of (scope, scopeID, stage), if any, and
// inserts a fresh pending one with the given config_snapshot (v1.1 A4; ""
// is stored as "{}") and initial stats. The new job has no stale_reason.
func newPending(ctx context.Context, tx db.Store, scope, scopeID, stage string, cur *model.Job, snapshot string, stats model.JobStats) error {
	if cur != nil {
		if err := tx.RetireJob(ctx, cur.ID); err != nil {
			return err
		}
	}
	return tx.InsertJob(ctx, model.Job{ID: db.NewID(), Scope: scope, ScopeID: scopeID, Stage: stage,
		Status: model.StatusPending, IsCurrent: true, CreatedAt: db.Now(), ConfigSnapshot: snapshot, Stats: stats})
}

// snapshotOf is the config_snapshot of a job, "{}" when there is none.
func snapshotOf(j *model.Job) string {
	if j == nil || model.IsEmptySnapshot(j.ConfigSnapshot) {
		return model.EmptySnapshot
	}
	return j.ConfigSnapshot
}

// Rerun is the user "rerun" action (SPEC §4.6) for a done or stale stage.
// In one transaction: the stage's job and the current job of every
// downstream stage are retired and replaced with new pending jobs, and the
// cleanup of the §4.6 table is applied for the stage and all its downstream
// stages, followed by RecomputeText. File cleanup (audio.wav, export/) runs
// after the commit. Every new job keeps the config_snapshot of the job it
// replaces (v1.1 A4.2). Rerun(...) is RerunWith(..., RerunOpts{}).
//
// Errors: ErrDraft for a draft lecture; ErrConflict when the stage is not
// done/stale or any stage that would be replaced is running (ErrBusy);
// ErrMediaMissing for prep when the original media is gone.
func Rerun(ctx context.Context, st db.Store, dataDir, scope, scopeID, stage string) error {
	return RerunWith(ctx, st, dataDir, scope, scopeID, stage, RerunOpts{})
}

// rerun is the body of RerunWith (see Rerun for the SPEC §4.6 semantics).
func rerun(ctx context.Context, st db.Store, dataDir, scope, scopeID, stage string, o RerunOpts) error {
	if model.ScopeOfStage(stage) != scope {
		return fmt.Errorf("stage %q does not belong to scope %q", stage, scope)
	}
	if model.IsAuxStage(stage) {
		// v2 phase 2 ruling 1: an auxiliary task is retried or run again
		// (EnqueueAux), never rerun with downstream cleanup.
		return fmt.Errorf("%w: %s is an auxiliary task and has no rerun", ErrConflict, stage)
	}
	if o.TextRepair {
		if scope != model.ScopeLecture || stage != model.StageASR {
			return fmt.Errorf("%w: text_only is a rerun of a lecture's asr stage, not of %s", ErrBadInput, stage)
		}
		if _, ok := o.NewSnapshots[model.StageASR]; ok {
			return fmt.Errorf("%w: text_only keeps the asr snapshot", ErrBadInput)
		}
	}
	// The stage and its downstream stages. glossary_review is outside the
	// course chain (v2 lead ruling 2): it has none, and a glossary_seed
	// rerun does not replace it.
	affected := append([]string{stage}, Downstream(stage)...)
	var lec model.Lecture
	err := st.Tx(ctx, func(tx db.Store) error {
		if scope == model.ScopeLecture {
			var err error
			if lec, err = tx.GetLecture(ctx, scopeID); err != nil {
				return err
			}
			if IsDraft(lec) {
				return ErrDraft
			}
		}
		cur := map[string]*model.Job{}
		for _, s := range affected {
			j, err := tx.GetCurrentJob(ctx, scope, scopeID, s)
			if errors.Is(err, db.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			cur[s] = &j
		}
		self := cur[stage]
		if self == nil {
			return fmt.Errorf("%w: no current %s job", ErrConflict, stage)
		}
		if self.Status != model.StatusDone && self.Status != model.StatusStale {
			return fmt.Errorf("%w: %s is %s, rerun needs done or stale", ErrConflict, stage, self.Status)
		}
		for _, s := range affected {
			if j := cur[s]; j != nil && j.Status == model.StatusRunning {
				return fmt.Errorf("%w (%s)", ErrBusy, s)
			}
		}
		if scope == model.ScopeLecture {
			if stage == model.StagePrep && !MediaExists(dataDir, lec) {
				return ErrMediaMissing
			}
			clean := stage
			if o.TextRepair {
				// "修复断句": the sentences stay for the asr job to repair;
				// everything after asr is cleaned up as by a rerun of the
				// stage after it (textrepair-int).
				clean = Downstream(stage)[0]
			}
			if err := cleanupLecture(ctx, tx, scopeID, clean); err != nil {
				return err
			}
		}
		for _, s := range affected {
			// A4.2/A4.3: "使用当前设置" for the listed stages, otherwise the
			// snapshot of the job being replaced.
			snap, ok := o.NewSnapshots[s]
			if !ok {
				// The stage runs the current prompt whatever model it
				// keeps (v2 G5.5, lead Gate 1): record that version.
				snap = withPromptVersion(s, snapshotOf(cur[s]), model.PromptVersionOf(s))
			}
			var stats model.JobStats
			if o.TextRepair && s == model.StageASR {
				stats = textRepairStats(self.Stats)
			}
			if err := newPending(ctx, tx, scope, scopeID, s, cur[s], snap, stats); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if scope == model.ScopeLecture {
		// The ids come from the database (an imported library supplies
		// them): only a lecture directory of its own, with no link on its
		// way, is cleaned (LectureFilesOK; R77b, SEC44 low-2, SEC49 L1). The
		// rows are rerun anyway.
		if !LectureFilesOK(dataDir, lec) {
			slog.Warn("rerun: the lecture's directory is not one of its own (its ids, or a link on the way); no file removed")
			return nil
		}
		dir := model.LectureDir(dataDir, lec.CourseID, lec.ID)
		if stage == model.StagePrep {
			if err := os.Remove(filepath.Join(dir, model.AudioFile)); err != nil && !os.IsNotExist(err) {
				slog.Warn("rerun: remove audio.wav", "lecture", lec.ID, "err", err)
			}
		}
		// export is downstream of (or equal to) every lecture stage.
		if err := os.RemoveAll(filepath.Join(dir, model.ExportDirName)); err != nil {
			slog.Warn("rerun: remove export dir", "lecture", lec.ID, "err", err)
		}
	}
	return nil
}

// cleanupLecture applies the database part of the SPEC §4.6 cleanup table
// for rerunning stage (which already includes every downstream row), then
// recomputes text. human_text survives everything except prep/asr reruns,
// where the sentences themselves are deleted.
func cleanupLecture(ctx context.Context, tx db.Store, lectureID, stage string) error {
	switch stage {
	case model.StagePrep, model.StageASR:
		if err := tx.DeletePatches(ctx, lectureID); err != nil {
			return err
		}
		if err := tx.DeleteSummary(ctx, lectureID); err != nil {
			return err
		}
		return tx.DeleteSentences(ctx, lectureID)
	case model.StageSummary, model.StageRepair:
		if stage == model.StageSummary {
			if err := tx.DeleteSummary(ctx, lectureID); err != nil {
				return err
			}
		}
		if err := tx.DeletePatches(ctx, lectureID); err != nil {
			return err
		}
		if err := tx.ResetRepairedText(ctx, lectureID); err != nil {
			return err
		}
		if err := tx.ClearTranslations(ctx, lectureID); err != nil {
			return err
		}
	case model.StageTranslate:
		if err := tx.ClearTranslations(ctx, lectureID); err != nil {
			return err
		}
	case model.StageExport:
		// files only
	}
	return tx.RecomputeText(ctx, lectureID, nil)
}

// StartIncrementalTranslate implements SPEC §7.3 (second rule): when the
// translate stage is done, its job is replaced by a new pending one (no
// sentence translation is cleared, so the run only processes
// stale/none/qa_failed sentences) and export is replaced by a new pending
// job too. Returns false, nil when translate is not done (nothing to do: a
// later or retried run will pick the stale sentences up), and ErrBusy when
// export is running at that moment (the caller should try again shortly).
// The new translate job carries stats.note = "incremental:<old input_hash>"
// (or "incremental" when the old job had none). Both new jobs copy the
// config_snapshot of the jobs they replace: the incremental run uses the
// model of the lecture's latest translate job (v1.1 A4.2).
func StartIncrementalTranslate(ctx context.Context, st db.Store, lectureID string) (bool, error) {
	started := false
	err := st.Tx(ctx, func(tx db.Store) error {
		tr, err := tx.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageTranslate)
		if err != nil {
			return err
		}
		if tr.Status != model.StatusDone {
			return nil
		}
		var exp *model.Job
		ex, err := tx.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageExport)
		switch {
		case err == nil:
			if ex.Status == model.StatusRunning {
				return fmt.Errorf("%w (export)", ErrBusy)
			}
			exp = &ex
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		note := NoteIncremental
		if tr.InputHash != nil && *tr.InputHash != "" {
			note += ":" + *tr.InputHash
		}
		// The translations are kept, so are the models they come from
		// (stats.mixed_models, v3.2.x P1-b).
		stats := model.JobStats{Note: note, MixedModels: slices.Clone(tr.Stats.MixedModels)}
		if err := newPending(ctx, tx, model.ScopeLecture, lectureID, model.StageTranslate, &tr, snapshotOf(&tr), stats); err != nil {
			return err
		}
		if err := newPending(ctx, tx, model.ScopeLecture, lectureID, model.StageExport, exp, snapshotOf(exp), model.JobStats{}); err != nil {
			return err
		}
		started = true
		return nil
	})
	return started && err == nil, err
}

// ResetGlossarySeed inserts (or resets) the course's glossary_seed job as a
// new pending job, for syllabus upload (SPEC §3.3). ErrBusy while the
// current glossary_seed is running.
func ResetGlossarySeed(ctx context.Context, st db.Store, courseID string) error {
	return st.Tx(ctx, func(tx db.Store) error {
		var cur *model.Job
		j, err := tx.GetCurrentJob(ctx, model.ScopeCourse, courseID, model.StageGlossarySeed)
		switch {
		case err == nil:
			if j.Status == model.StatusRunning {
				return fmt.Errorf("%w (glossary_seed)", ErrBusy)
			}
			cur = &j
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		// Course-level jobs have no snapshot (v1.1 A4.1 covers lecture stages only).
		return newPending(ctx, tx, model.ScopeCourse, courseID, model.StageGlossarySeed, cur, model.EmptySnapshot, model.JobStats{})
	})
}
