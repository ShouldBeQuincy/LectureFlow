// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// v2 scheduler rules for the course task glossary_review (SPEC-v2 G3.1,
// appendix D2; lead rulings 2–4 in .claude/v2-brief.md).
//
// The stage itself is registered like any other (stages.All): an API stage
// whose provider is the glossary_janitor role's current setting
// (providerOf: course jobs have no snapshot). Its dependencies and the asr
// wait are state.Deps. What is left here:
//   - the G2 fallback after a review that ended failed (for good) or
//     cancelled: asr stops waiting for such a review, so the glossary must
//     not stay unsorted (lead, Gate 1);
//   - the "dirty" flag: new terms arriving while a review runs set kv
//     glossary_review_dirty:{course} (state.EnqueueGlossaryReview), and the
//     scheduler queues one more review when that run has ended.

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/glossary"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// kvReviewFallbackPrefix: glossary_review_fallback:{course_id} → the stamp
// (reviewStamp) of the last review whose end got its fallback, so the
// sweep at Start runs it once per end, not on every start.
const kvReviewFallbackPrefix = "glossary_review_fallback:"

// reviewStamp identifies one end of a review job (a retry reuses the job).
func reviewStamp(j model.Job) string {
	fin := ""
	if j.FinishedAt != nil {
		fin = *j.FinishedAt
	}
	return j.ID + "/" + strconv.Itoa(j.Attempt) + "/" + fin
}

// reviewFallbackNote is the report note of the fallback after a review that
// did not complete.
func reviewFallbackNote(status string) string {
	what := "失败"
	if status == model.StatusCancelled {
		what = "取消"
	}
	return "术语表清道夫未完成（" + what + "），已按常用词规则整理"
}

// reviewFallbackInfo is the coded form of reviewFallbackNote (v2 P14.3):
// status is failed or cancelled.
func reviewFallbackInfo(status string) *model.ErrorInfo {
	return &model.ErrorInfo{Code: CodeNoteReviewIncomplete, Params: map[string]any{"status": status}}
}

// finishReview is finish for a glossary_review run. A review that ended
// failed (no automatic retry left) or cancelled no longer holds asr, so the
// G2 fallback runs at once (lead, Gate 1). The final status and the
// fallback are applied under the scan lock: no asr can start in between
// with the unsorted glossary. The invalidation and the events follow after
// the lock is released.
func (s *Scheduler) finishReview(r *run, runErr error, took time.Duration) string {
	ctx := context.Background()
	s.scanMu.Lock()
	final := s.finish(r, runErr, took)
	var ran, changed bool
	if final == model.StatusFailed || final == model.StatusCancelled {
		if j, err := s.st.GetJob(ctx, r.job.ID); err == nil {
			ran, changed = s.fallbackAfterReview(ctx, j)
		}
	}
	s.scanMu.Unlock()
	if ran {
		s.announceFallback(ctx, r.job.ScopeID, changed)
	}
	return final
}

// fallbackAfterReview runs glossary.RunFallbackNote for j when it is the
// course's current review and ended failed or cancelled, once per end
// (kvReviewFallbackPrefix). It is skipped when a fallback already ran
// after the job started (fellBackSinceStart): the stage itself does that for
// permanent janitor errors (appendix D2), and its report and note (with the
// reason) must stay the latest; a second fallback would change nothing
// anyway (idempotent, no version bump). The caller
// holds the scan lock, or calls it at Start before any scan. ran reports
// whether a fallback ran, changed whether it changed terms.
func (s *Scheduler) fallbackAfterReview(ctx context.Context, j model.Job) (ran, changed bool) {
	due, err := s.reviewFallbackDue(ctx, j)
	if err != nil {
		slog.Error("glossary fallback after the review", "course", j.ScopeID, "err", err)
		return false, false
	}
	if !due {
		return false, false
	}
	key, stamp := kvReviewFallbackPrefix+j.ScopeID, reviewStamp(j)
	if !s.fellBackSinceStart(ctx, j) {
		rep, ch, err := glossary.RunFallbackNote(ctx, s.st, j.ScopeID, s.now(), reviewFallbackNote(j.Status), reviewFallbackInfo(j.Status))
		if err != nil {
			slog.Error("glossary fallback after the review", "course", j.ScopeID, "err", err)
			return false, false
		}
		ran, changed = true, ch
		slog.Info("glossary review did not complete; fallback rule applied", "course", j.ScopeID,
			"status", j.Status, "changed_terms", len(rep.Items), "version", rep.Version)
	}
	if err := s.st.SetKV(ctx, key, stamp); err != nil {
		slog.Warn("glossary fallback marker", "course", j.ScopeID, "err", err)
	}
	return ran, changed
}

// reviewFallbackDue reports whether j is a course's current glossary_review
// that ended failed or cancelled and has no fallback for that end yet (its
// marker under kvReviewFallbackPrefix is not reviewStamp(j)): the end
// fallbackAfterReview handles (it then still skips the fallback itself
// when fellBackSinceStart). fallbackMayRun asks the same at Start, so the
// two cannot drift apart.
func (s *Scheduler) reviewFallbackDue(ctx context.Context, j model.Job) (bool, error) {
	if j.Stage != model.StageGlossaryReview || !j.IsCurrent ||
		(j.Status != model.StatusFailed && j.Status != model.StatusCancelled) {
		return false, nil
	}
	v, ok, err := s.st.GetKV(ctx, kvReviewFallbackPrefix+j.ScopeID)
	if err != nil {
		return false, err
	}
	return !ok || v != reviewStamp(j), nil
}

// fellBackSinceStart: the course's last janitor run (glossary.LastRun, also
// a run that changed nothing) is a fallback created after j (last) started:
// by the stage in this run, or by an earlier pass of fallbackAfterReview.
// A job cancelled while queued has no started_at: the kv marker alone keeps
// its fallback to one run.
func (s *Scheduler) fellBackSinceStart(ctx context.Context, j model.Job) bool {
	if j.StartedAt == nil || *j.StartedAt == "" {
		return false
	}
	rep, err := glossary.LastRun(ctx, s.st, j.ScopeID)
	if err != nil || !rep.Fallback {
		return false
	}
	started, err1 := time.Parse(time.RFC3339, *j.StartedAt)
	created, err2 := time.Parse(time.RFC3339, rep.CreatedAt)
	return err1 == nil && err2 == nil && !created.Before(started)
}

// announceFallback: the course invalidation of a fallback that changed
// terms (stale_reason "术语表已整理（vN → vM）") and the SSE events.
func (s *Scheduler) announceFallback(ctx context.Context, courseID string, changed bool) {
	if changed {
		if err := s.InvalidateCourseWith(ctx, courseID, state.StaleCause{Kind: state.StaleJanitor}); err != nil {
			slog.Error("invalidate course after the glossary fallback", "course", courseID, "err", err)
		}
	}
	s.ev.Changed("terms", courseID)
	s.ev.Changed("course", courseID)
}

// reviewEnded consumes the course's dirty flag after its glossary_review
// ended (lead ruling 4): called when a run finished done, failed (final) or
// cancelled, when a queued review was cancelled, and once at Start for
// flags left over by a run that ended unseen (startup recovery failing it
// for good, a crash). In one transaction:
//   - no flag → nothing;
//   - the current review is pending, queued or running → the flag is kept
//     for the end of that run (an automatic retry's backoff is not an end;
//     a newer run may own the flag);
//   - the current review is cancelled → the flag is dropped and nothing is
//     queued: a user cancel must stick, and retrying the job reviews the
//     whole glossary anyway (lead answer to core-builder, NOTES.md);
//   - otherwise (done, failed, stale, no job) → the flag is dropped and
//     state.EnqueueGlossaryReview queues one more run (nothing when the
//     janitor is no longer configured).
func (s *Scheduler) reviewEnded(ctx context.Context, courseID string) {
	key := model.KVGlossaryReviewDirtyPrefix + courseID
	queued := false
	err := s.st.Tx(ctx, func(tx db.Store) error {
		v, ok, err := tx.GetKV(ctx, key)
		if err != nil || !ok || v == "" {
			return err
		}
		cur, err := tx.GetCurrentJob(ctx, model.ScopeCourse, courseID, model.StageGlossaryReview)
		found := err == nil
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return err
		}
		if found {
			switch cur.Status {
			case model.StatusPending, model.StatusQueued, model.StatusRunning:
				return nil
			}
		}
		// The kv table has no delete: "" means "not set".
		if err := tx.SetKV(ctx, key, ""); err != nil {
			return err
		}
		if found && cur.Status == model.StatusCancelled {
			slog.Info("glossary review cancelled; new terms wait for the next review", "course", courseID)
			return nil
		}
		queued, err = state.EnqueueGlossaryReview(ctx, tx, courseID)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("glossary review: queue the run for new terms", "course", courseID, "err", err)
		}
		return
	}
	if queued {
		slog.Info("glossary review queued again: terms changed while it ran", "course", courseID)
		s.emitScope(ctx, model.ScopeCourse, courseID)
		s.Kick()
	}
}

// reviewEndedAfterRun is reviewEnded for a run that left its job in final.
// A run requeued for an automatic retry (queued) or left running by a
// shutdown has not ended.
func (s *Scheduler) reviewEndedAfterRun(r *run, final string) {
	if r.job.Stage != model.StageGlossaryReview {
		return
	}
	switch final {
	case model.StatusDone, model.StatusFailed, model.StatusCancelled:
		s.reviewEnded(context.Background(), r.job.ScopeID)
	}
}

// resumeReviewFlags runs at Start for reviews that ended while no scheduler
// watched them (startup recovery failing one for good, a crash): the
// fallback a failed or cancelled review needs (once per end), then the
// dirty flag.
func (s *Scheduler) resumeReviewFlags(ctx context.Context) error {
	courses, err := s.st.ListCourses(ctx)
	if err != nil {
		return err
	}
	for _, c := range courses {
		j, err := s.st.GetCurrentJob(ctx, model.ScopeCourse, c.ID, model.StageGlossaryReview)
		switch {
		case err == nil:
			if ran, changed := s.fallbackAfterReview(ctx, j); ran {
				s.announceFallback(ctx, c.ID, changed)
			}
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		s.reviewEnded(ctx, c.ID)
	}
	return nil
}
