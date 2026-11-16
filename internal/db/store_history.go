// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"

	"lectureflow/internal/model"
)

// JobHistory is implemented by the SQLite store (v3.2.x r7, T25 S1): every
// job of one scope, the retired ones included, so a lecture's cost can be
// added up over all its runs (reruns, updates and retranslations retire the
// job they replace). Not part of the frozen Store interface: the API uses it
// when the store has it, like Counter.
type JobHistory interface {
	// ListJobHistory returns every job of (scope, scopeID), current or
	// retired, oldest first (created_at, id).
	//
	// The job table has no index for retired jobs (job_current covers
	// is_current = 1 only), so the query scans the table (EXPLAIN QUERY
	// PLAN: SCAN job): about 2 ms on a library of 400 lectures, 5 ms with a
	// heavy rerun history (measured 2026-11-15, C155). Read it for one scope
	// per request, never once per item of a list.
	ListJobHistory(ctx context.Context, scope, scopeID string) ([]model.Job, error)
}

var _ JobHistory = (*sqlStore)(nil)

func (s *sqlStore) ListJobHistory(ctx context.Context, scope, scopeID string) ([]model.Job, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+jobCols+` FROM job WHERE scope = ? AND scope_id = ?
		ORDER BY created_at, id`, scope, scopeID)
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

// CourseReader is implemented by the SQLite store (v3.2.x r7, P1 H3): the
// per-course reads of the course list, a course and the dashboard for many
// courses in one statement each (in chunks of countChunk ids), so a request
// does not grow by a dozen statements per course. Each method gives exactly
// what the per-course Store method gives, course by course. Not part of the
// frozen Store interface: the API uses it when the store has it, like
// Counter.
type CourseReader interface {
	// BatchLectures is ListLectures of every course in ids, by course id
	// (a course without lectures is absent), each in ListLectures' order.
	BatchLectures(ctx context.Context, courseIDs []string) (map[string][]model.Lecture, error)
	// BatchMaterials is ListMaterials of every course in ids, by course id.
	BatchMaterials(ctx context.Context, courseIDs []string) (map[string][]model.Material, error)
	// BatchSyllabi is GetSyllabus of every course in ids that has one.
	BatchSyllabi(ctx context.Context, courseIDs []string) (map[string]model.Syllabus, error)
	// BatchKV is GetKV of every key in keys that GetKV finds (sealed values
	// opened as GetKV opens them; absent keys are left out).
	BatchKV(ctx context.Context, keys []string) (map[string]string, error)
	// BatchCurrentJobs is the current jobs of the scopes (scope, id) for
	// every id in ids, ordered by scope id and then, as
	// ListCurrentJobsByStatus orders the jobs of one scope, by created_at
	// and id. It reads through job_current, not the whole job table.
	BatchCurrentJobs(ctx context.Context, scope string, ids []string) ([]model.Job, error)
}

var _ CourseReader = (*sqlStore)(nil)

func (s *sqlStore) BatchLectures(ctx context.Context, courseIDs []string) (map[string][]model.Lecture, error) {
	out := map[string][]model.Lecture{}
	for _, part := range idChunks(courseIDs) {
		where := ` WHERE l.course_id IN (` + placeholders(len(part)) + `) ORDER BY l.created_at, l.id`
		rows, err := s.q.QueryContext(ctx, lectureSelect+where, anyArgs(part)...)
		if noOverrideTable(err) {
			rows, err = s.q.QueryContext(ctx, lectureSelectPre011+where, anyArgs(part)...)
		}
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			l, err := scanLecture(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[l.CourseID] = append(out[l.CourseID], l)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *sqlStore) BatchMaterials(ctx context.Context, courseIDs []string) (map[string][]model.Material, error) {
	out := map[string][]model.Material{}
	for _, part := range idChunks(courseIDs) {
		rows, err := s.q.QueryContext(ctx, `SELECT `+materialCols+` FROM material WHERE course_id IN (`+placeholders(len(part))+`)
			ORDER BY created_at, id`, anyArgs(part)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, err := scanMaterial(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[m.CourseID] = append(out[m.CourseID], m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *sqlStore) BatchSyllabi(ctx context.Context, courseIDs []string) (map[string]model.Syllabus, error) {
	out := map[string]model.Syllabus{}
	for _, part := range idChunks(courseIDs) {
		rows, err := s.q.QueryContext(ctx, `SELECT course_id, filename, path, text FROM syllabus WHERE course_id IN (`+placeholders(len(part))+`)`,
			anyArgs(part)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sy model.Syllabus
			var text sql.NullString
			if err := rows.Scan(&sy.CourseID, &sy.Filename, &sy.Path, &text); err != nil {
				rows.Close()
				return nil, err
			}
			sy.Text = strPtr(text)
			out[sy.CourseID] = sy
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *sqlStore) BatchKV(ctx context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range idChunks(keys) {
		rows, err := s.q.QueryContext(ctx, `SELECT key, value FROM kv WHERE key IN (`+placeholders(len(part))+`)`, anyArgs(part)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return nil, err
			}
			if isSealedKV(k) { // as GetKV: sealed at rest (sealed.go)
				var ok bool
				if v, ok = openKV(k, v); !ok {
					continue
				}
			}
			out[k] = v
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *sqlStore) BatchCurrentJobs(ctx context.Context, scope string, ids []string) ([]model.Job, error) {
	var out []model.Job
	for _, part := range idChunks(ids) {
		args := append([]any{scope}, anyArgs(part)...)
		rows, err := s.q.QueryContext(ctx, `SELECT `+jobCols+` FROM job WHERE is_current = 1 AND scope = ? AND scope_id IN (`+
			placeholders(len(part))+`) ORDER BY scope_id, created_at, id`, args...)
		if err != nil {
			return nil, err
		}
		jobs, err := scanJobs(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, jobs...)
	}
	return out, nil
}
