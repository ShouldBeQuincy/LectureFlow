// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"

	"lectureflow/internal/model"
)

// Lean reads of current jobs (r7 perf, P1 report H1). Two callers read
// every current job (ListCurrentJobsByStatus: three joins, a sort and every
// row's stats JSON; 15 ms on a 400-lecture library, about 57 ms on 1500)
// to use a few columns of a few rows: the self-test's clip source
// (devices.FindClipSource, behind GET /api/asr/devices, /api/settings and
// /api/system) and the providers' snapshot counts (the API's snapshotRefs,
// behind /api/settings and /api/providers). These read only what those
// callers use, through the job_current index. They are not part of the
// frozen Store interface: callers use them when the store has them
// (JobReader) and otherwise derive the same results from
// ListCurrentJobsByStatus.

// FinishedJob is a current job's scope id and finished_at ("" for NULL).
type FinishedJob struct {
	ScopeID    string
	FinishedAt string
}

// JobReader is implemented by the SQLite store.
type JobReader interface {
	// DoneLectureJobsByFinish returns the current lecture jobs of stage
	// whose status is done, the most recently finished first (a NULL
	// finished_at counts as ""), equal finish times by scope id ascending:
	// the order devices.FindClipSource tries lectures in.
	DoneLectureJobsByFinish(ctx context.Context, stage string) ([]FinishedJob, error)
	// LectureSnapshotCounts counts the current lecture jobs of stages, in
	// any status, by their raw config_snapshot (the caller parses each
	// distinct snapshot once).
	LectureSnapshotCounts(ctx context.Context, stages ...string) (map[string]int, error)
}

var _ JobReader = (*sqlStore)(nil)

// doneByFinishSQL (args: scope, stage, status): the job_current index
// (scope, scope_id, stage) WHERE is_current = 1 gives the current jobs of
// the scope. COALESCE sorts NULL as "", like the Go comparison it replaces
// (SQLite alone would sort NULL below ""); TEXT compares bytewise, like Go
// strings.
const doneByFinishSQL = `SELECT scope_id, COALESCE(finished_at, '') FROM job
	WHERE is_current = 1 AND scope = ? AND stage = ? AND status = ?
	ORDER BY COALESCE(finished_at, '') DESC, scope_id`

func (s *sqlStore) DoneLectureJobsByFinish(ctx context.Context, stage string) ([]FinishedJob, error) {
	rows, err := s.q.QueryContext(ctx, doneByFinishSQL, model.ScopeLecture, stage, model.StatusDone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FinishedJob
	for rows.Next() {
		var j FinishedJob
		if err := rows.Scan(&j.ScopeID, &j.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// snapshotCountsSQL (args: scope, then the stages) groups by the raw
// snapshot: BINARY collation puts only byte-equal snapshots in one group,
// so a group parses exactly like each of its rows. No status condition:
// the job table's CHECK admits only the seven statuses.
func snapshotCountsSQL(stages int) string {
	return `SELECT config_snapshot, count(*) FROM job
	WHERE is_current = 1 AND scope = ? AND stage IN (` + placeholders(stages) + `)
	GROUP BY config_snapshot`
}

func (s *sqlStore) LectureSnapshotCounts(ctx context.Context, stages ...string) (map[string]int, error) {
	out := map[string]int{}
	if len(stages) == 0 {
		return out, nil
	}
	args := append([]any{model.ScopeLecture}, anyArgs(stages)...)
	rows, err := s.q.QueryContext(ctx, snapshotCountsSQL(len(stages)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var snap string
		var n int
		if err := rows.Scan(&snap, &n); err != nil {
			return nil, err
		}
		out[snap] += n
	}
	return out, rows.Err()
}
