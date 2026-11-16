// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// queueChaptersAfterExport queues the lecture's chapters task after its
// export job exportJobID completed (v3.2.x P3, Q16a), like queueTitle after
// a summary, when
//   - the general setting auto_chapters is on (GeneralSettings.AutoChaptersOn),
//   - the chapters role is configured,
//   - the lecture has no chapters yet: no kv chapters:{lecture_id} (chapters
//     that are out of date still count here; see below) and no chapters task of
//     any status (a task already queued, running, failed or cancelled was
//     the automatic or the user's request), and
//   - this is the lecture's first export (exportedBefore): a later export,
//     after an incremental retranslation or a rerun, queues none, so
//     lectures exported before the upgrade are never backfilled. An export
//     that completed while the role was not configured or the option was
//     off was the first one too: configuring the role later does not queue
//     chapters on the next export.
//
// Chapters that went out of date because the summary was regenerated are
// refreshed (round 2 N39): when the lecture's current chapters task is
// done, its kv chapters:{lecture_id} no longer matches the current summary
// and sentences (the "章节可能已过时" the player shows; an auxiliary task
// never becomes stale itself, P13.5) and the current summary task finished
// no earlier than the chapters task started (a rerun of asr, repair or summary regenerated
// the summary; see chaptersOutdated), the export queues the chapters task
// again, whether or not it is the first export. Sentences edited by hand
// trigger an incremental retranslation and an export without a new
// summary: that queues none (the player's hint and the manual rerun
// remain), so proofreading does not cost a full chapters request after
// every pause (review R26 A-1). A chapters task that is
// pending, queued, running, failed or cancelled still blocks (the user's
// cancellation or a failure is not retried for them), and a lecture without
// a chapters task is still not backfilled. The new run goes through
// state.EnqueueAux like the first one (snapshot "{}": the chapters role's
// current setting at run time) and touches no other stage. A second export
// completing while that run is pending, queued or running queues none.
//
// The chapters task stays an auxiliary task: its failure does not change
// the lecture (SPEC-v2 P7). Errors here are only logged: the export that
// completed stays done.
func (s *Scheduler) queueChaptersAfterExport(lectureID, exportJobID string) {
	if !s.set.General().AutoChaptersOn() {
		return
	}
	ctx := context.Background()
	var queued, refresh bool
	err := s.st.Tx(ctx, func(tx db.Store) error {
		ok, err := state.RoleConfigured(ctx, tx, model.AuxRole(model.StageChapters))
		if err != nil || !ok {
			return err
		}
		kv, hasKV, err := tx.GetKV(ctx, model.KVChaptersPrefix+lectureID)
		if err != nil {
			return err
		}
		switch cur, err := tx.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageChapters); {
		case err == nil:
			outdated, err := chaptersOutdated(ctx, tx, lectureID, cur, kv, hasKV)
			if err != nil || !outdated {
				return err
			}
			refresh = true
		case !errors.Is(err, db.ErrNotFound):
			return err
		case hasKV:
			return nil
		default:
			if before, err := exportedBefore(ctx, tx, lectureID, exportJobID); err != nil || before {
				return err
			}
		}
		queued, err = state.EnqueueAux(ctx, tx, model.ScopeLecture, lectureID, model.StageChapters)
		return err
	})
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) { // the lecture was deleted meanwhile
			slog.Error("queue the chapters after the export", "lecture", lectureID, "err", err)
		}
		return
	}
	if queued {
		slog.Info("chapters queued after the export", "lecture", lectureID, "outdated", refresh)
		s.emitCurrent(ctx, model.ScopeLecture, lectureID, model.StageChapters)
	}
}

// chaptersOutdated reports whether an export should refresh the lecture's
// chapters. All of these must hold, anything else (or anything missing or
// unreadable) is "no":
//   - the current chapters job cur is done;
//   - the current summary job is done or stale and finished no earlier than
//     cur started (summary finished_at >= chapters started_at; both parse
//     as RFC3339, whole seconds). The chapters read their input when they
//     start, so this also covers a summary rerun that finished while the
//     chapters were running, and one in the same second (review R26 A-6);
//     the hash check below keeps a summary the chapters did see from
//     queueing them again;
//   - the stored chapters (kv, found = hasKV) parse and carry an
//     input_hash, and it differs from the current one (state.ChaptersInput,
//     the read the chapters stage hashes too).
//
// The cheap checks come first; the sentences are read last.
func chaptersOutdated(ctx context.Context, tx db.Store, lectureID string, cur model.Job, kv string, hasKV bool) (bool, error) {
	if cur.Status != model.StatusDone || !hasKV {
		return false, nil
	}
	chStart, ok := parseJobTime(cur.StartedAt)
	if !ok {
		return false, nil
	}
	sum, err := tx.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageSummary)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	if sum.Status != model.StatusDone && sum.Status != model.StatusStale {
		return false, nil
	}
	if sumDone, ok := parseJobTime(sum.FinishedAt); !ok || sumDone.Before(chStart) {
		return false, nil
	}
	var ch model.Chapters
	if err := json.Unmarshal([]byte(kv), &ch); err != nil || ch.InputHash == "" {
		return false, nil
	}
	h, err := chaptersInputHash(ctx, tx, lectureID)
	if err != nil {
		return false, err
	}
	return h != ch.InputHash, nil
}

// parseJobTime parses a job's started_at or finished_at, ok=false when
// absent, empty or unparsable.
func parseJobTime(s *string) (time.Time, bool) {
	if s == nil || *s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, *s)
	return t, err == nil
}

// chaptersInputHash is the current input hash of the lecture's chapters,
// as the chapters stage stores it (state.ChaptersInput, shared with it).
func chaptersInputHash(ctx context.Context, tx db.Store, lectureID string) (string, error) {
	summary, ss, err := state.ChaptersInput(ctx, tx, lectureID)
	if err != nil {
		return "", err
	}
	return ctxbuild.ChaptersInputHash(summary, ss), nil
}

// exportedBefore reports whether the lecture has an export job other than
// exportJobID that completed: status done, or stale (only a done job becomes
// stale). A job reaches done at most once (done → stale is its only way
// on), and a rerun or an incremental retranslation retires it and inserts a
// new row (state.newPending) that keeps its status and finished_at, so such
// a row is an earlier export. Exports before the upgrade count as well.
//
// It uses the frozen Store's ListJobsFinishedSince (no new query, no
// migration), from a day before the lecture was created so the scan stays
// small; the margin covers a clock set back a little. An unparsable
// created_at scans all finished jobs.
func exportedBefore(ctx context.Context, tx db.Store, lectureID, exportJobID string) (bool, error) {
	lec, err := tx.GetLecture(ctx, lectureID)
	if err != nil {
		return false, err
	}
	since := ""
	if t, err := time.Parse(time.RFC3339, lec.CreatedAt); err == nil {
		since = db.FormatTime(t.Add(-24 * time.Hour))
	}
	jobs, err := tx.ListJobsFinishedSince(ctx, since)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.Scope == model.ScopeLecture && j.ScopeID == lectureID && j.Stage == model.StageExport && j.ID != exportJobID &&
			(j.Status == model.StatusDone || j.Status == model.StatusStale) {
			return true, nil
		}
	}
	return false, nil
}
