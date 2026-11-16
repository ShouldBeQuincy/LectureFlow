// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
)

// The per-course reads of the course views (v3.2.x r7, P1 H3). The course
// list read about eleven statements per course (lectures, materials,
// syllabus, six kv entries, term and patch counts), the dashboard three
// (lectures, the kind catalog, patch counts); while the pipeline keeps the
// single connection busy every statement waits its turn. readCourses reads
// each kind of data once for all the courses of a request through
// db.CourseReader, and course by course — exactly as before — from a store
// without it (tests' wrappers). The views are the same either way
// (TestCourseBatchEqualsPerCourse).

// courseReads is what the course views read per course, for many courses.
type courseReads struct {
	lectures  map[string][]model.Lecture  // ListLectures by course id
	terms     map[string]int              // term counts by course id
	pending   map[string]int              // pending patches by lecture id (absent = 0)
	materials map[string][]model.Material // ListMaterials by course id (views only)
	syllabi   map[string]model.Syllabus   // GetSyllabus by course id, present ones (views only)
	// kv serves GetKV of the courses' kv entries (courseKVKeys and the
	// janitor reports they point at) from one read; any other call goes to
	// the store. It is the store itself without db.CourseReader.
	kv db.Store
}

// readCourses reads the courses ids' lectures, term counts and pending patch
// counts; views adds what a Course view needs besides (materials, syllabus,
// kv entries).
func (s *Server) readCourses(ctx context.Context, ids []string, views bool) (*courseReads, error) {
	rd := &courseReads{kv: s.st}
	br, batch := s.st.(db.CourseReader)
	var err error
	if batch {
		if rd.lectures, err = br.BatchLectures(ctx, ids); err != nil {
			return nil, err
		}
	} else {
		rd.lectures = make(map[string][]model.Lecture, len(ids))
		for _, id := range ids {
			if rd.lectures[id], err = s.st.ListLectures(ctx, id); err != nil {
				return nil, err
			}
		}
	}
	var all []model.Lecture
	for _, id := range ids {
		all = append(all, rd.lectures[id]...)
	}
	if rd.pending, err = s.pendingPatchCounts(ctx, all); err != nil {
		return nil, err
	}
	if rd.terms, err = s.termCounts(ctx, ids); err != nil {
		return nil, err
	}
	if !views {
		return rd, nil
	}
	if !batch {
		rd.materials = make(map[string][]model.Material, len(ids))
		rd.syllabi = map[string]model.Syllabus{}
		for _, id := range ids {
			sy, err := s.st.GetSyllabus(ctx, id)
			switch {
			case err == nil:
				rd.syllabi[id] = sy
			case !errors.Is(err, db.ErrNotFound):
				return nil, err
			}
			if rd.materials[id], err = s.st.ListMaterials(ctx, id); err != nil {
				return nil, err
			}
		}
		return rd, nil
	}
	if rd.syllabi, err = br.BatchSyllabi(ctx, ids); err != nil {
		return nil, err
	}
	if rd.materials, err = br.BatchMaterials(ctx, ids); err != nil {
		return nil, err
	}
	if rd.kv, err = prefetchKV(ctx, s.st, br, ids); err != nil {
		return nil, err
	}
	return rd, nil
}

// courseKVKeys are the kv entries a Course view reads (courseCalendarFields,
// janitorStatus).
func courseKVKeys(id string) []string {
	return []string{
		model.KVFilenameProposal + id, model.KVFilenameUndo + id, model.KVCiteExportPrefix + id,
		stages.KVRenumberedAutoPrefix + id, model.KVJanitorLastRunPrefix + id, model.KVJanitorLatestPrefix + id,
	}
}

// prefetchKV reads the courses' kv entries, then the janitor reports their
// janitor_report_latest entries point at (glossary.Report's key), in two
// statements; the returned store answers GetKV of those keys from them.
func prefetchKV(ctx context.Context, st db.Store, br db.CourseReader, ids []string) (db.Store, error) {
	keys := make([]string, 0, 6*len(ids))
	for _, id := range ids {
		keys = append(keys, courseKVKeys(id)...)
	}
	vals, err := br.BatchKV(ctx, keys)
	if err != nil {
		return nil, err
	}
	var reports []string
	for _, id := range ids {
		if v, ok := vals[model.KVJanitorLatestPrefix+id]; ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				reports = append(reports, model.KVJanitorReportPrefix+id+":"+strconv.Itoa(n))
			}
		}
	}
	if len(reports) > 0 {
		more, err := br.BatchKV(ctx, reports)
		if err != nil {
			return nil, err
		}
		for k, v := range more {
			vals[k] = v
		}
		keys = append(keys, reports...)
	}
	known := make(map[string]bool, len(keys))
	for _, k := range keys {
		known[k] = true
	}
	return kvPrefetch{Store: st, known: known, vals: vals}, nil
}

// kvPrefetch answers GetKV of the prefetched keys (present: vals; absent:
// not found) and passes every other call to the store.
type kvPrefetch struct {
	db.Store
	known map[string]bool
	vals  map[string]string
}

func (k kvPrefetch) GetKV(ctx context.Context, key string) (string, bool, error) {
	if k.known[key] {
		v, ok := k.vals[key]
		return v, ok, nil
	}
	return k.Store.GetKV(ctx, key)
}

// courseJobs is the jobIndex of one course: the current jobs of the course,
// of its materials and of its lectures (rd: the course's reads), through
// job_current — not every current job of the library (loadJobs), which a
// store without db.CourseReader still reads.
func (s *Server) courseJobs(ctx context.Context, c model.Course, rd *courseReads) (jobIndex, error) {
	matIDs := make([]string, 0, len(rd.materials[c.ID]))
	for _, m := range rd.materials[c.ID] {
		matIDs = append(matIDs, m.ID)
	}
	return s.scopeJobsOf(ctx, []string{c.ID}, matIDs, rd.lectures[c.ID])
}

// scopeJobsOf is the jobIndex of the given courses, materials and
// lectures only (BatchCurrentJobs through job_current); every current job
// (loadJobs) from a store without db.CourseReader.
func (s *Server) scopeJobsOf(ctx context.Context, courseIDs, matIDs []string, lecs []model.Lecture) (jobIndex, error) {
	br, ok := s.st.(db.CourseReader)
	if !ok {
		return s.loadJobs(ctx)
	}
	lecIDs := make([]string, 0, len(lecs))
	for _, l := range lecs {
		lecIDs = append(lecIDs, l.ID)
	}
	ix := jobIndex{}
	for _, q := range []struct {
		scope string
		ids   []string
	}{{model.ScopeCourse, courseIDs}, {model.ScopeMaterial, matIDs}, {model.ScopeLecture, lecIDs}} {
		if len(q.ids) == 0 {
			continue
		}
		jobs, err := br.BatchCurrentJobs(ctx, q.scope, q.ids)
		if err != nil {
			return nil, err
		}
		ix.add(jobs)
	}
	return ix, nil
}
