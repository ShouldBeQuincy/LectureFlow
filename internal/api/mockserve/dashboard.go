// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"sort"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// Course aggregates and the dashboard (v1.1 A9) are computed from the mock
// lectures, so launching, updating or deleting a lecture shows up everywhere
// the way it would with the real back end.

// inProgress: launched and still moving — something is queued or running,
// or waiting for an upstream stage that is not stuck (failed, cancelled and
// stale stages never advance by themselves; they are listed under 待处理).
func inProgress(l *api.Lecture) bool {
	if l.Draft {
		return false
	}
	pending, stuck := false, false
	for _, st := range l.Stages {
		switch st.Status {
		case model.StatusQueued, model.StatusRunning:
			return true
		case model.StatusPending:
			pending = true
		case model.StatusFailed, model.StatusCancelled, model.StatusStale:
			stuck = true
		}
	}
	return pending && !stuck
}

func busy(l *api.Lecture) bool {
	for _, st := range l.Stages {
		if st.Status == model.StatusQueued || st.Status == model.StatusRunning {
			return true
		}
	}
	return false
}

func exportFinished(l *api.Lecture) string {
	for _, st := range l.Stages {
		if st.Stage == model.StageExport && st.FinishedAt != nil {
			return *st.FinishedAt
		}
	}
	return ""
}

// lastActivity is the latest stage finish, launch or creation time.
func lastActivity(l *api.Lecture) string {
	t := l.CreatedAt
	if l.LaunchedAt != nil && *l.LaunchedAt > t {
		t = *l.LaunchedAt
	}
	for _, st := range l.Stages {
		if st.FinishedAt != nil && *st.FinishedAt > t {
			t = *st.FinishedAt
		}
		if st.StartedAt != nil && *st.StartedAt > t {
			t = *st.StartedAt
		}
	}
	return t
}

// finalizeCourse recomputes the lecture-derived fields of a course.
// kc: the kind catalog and kind maps (lecture kinds, kinds.go).
func finalizeCourse(c *api.Course, lectures []api.Lecture, kc *kindCtx) {
	numberCourse(c, lectures, kc) // v2 P13.4 (course_p2.go) and the kinds pass
	c.LectureCount, c.DraftCount, c.StaleLectureCount, c.PatchesPending, c.RunningCount = 0, 0, 0, 0, 0
	c.FailedCount = 0
	last := c.CreatedAt
	cite := false
	for i := range lectures {
		l := &lectures[i]
		if l.CourseID != c.ID {
			continue
		}
		c.LectureCount++
		if l.Draft {
			c.DraftCount++
		}
		if l.StaleText != nil {
			c.StaleLectureCount++
		}
		if busy(l) {
			c.RunningCount++
		}
		if failedLecture(l) {
			c.FailedCount++
		}
		c.PatchesPending += l.Counts.PatchesPending
		if t := lastActivity(l); t > last {
			last = t
		}
		if stageStatus(l, model.StageExport) == model.StatusDone {
			cite = true
		}
	}
	for _, st := range c.Stages {
		if st.FinishedAt != nil && *st.FinishedAt > last {
			last = *st.FinishedAt
		}
	}
	c.LastActivityAt = last
	acts := []string{}
	if c.DraftCount > 0 {
		acts = append(acts, api.CourseActionLaunchDrafts)
	}
	if c.StaleLectureCount > 0 {
		acts = append(acts, api.CourseActionUpdateStale)
	}
	if cite {
		acts = append(acts, api.CourseActionExportCite)
	}
	c.CourseActions = acts
	if c.Materials == nil {
		c.Materials = []api.Material{}
	}
	if c.Stages == nil {
		c.Stages = []api.StageView{}
	}
	pending := false
	for _, st := range c.Stages {
		switch {
		case st.Stage == model.StageGlossaryReview: // v2: only while it waits or runs (lead ruling 3)
			if st.Status == model.StatusPending || st.Status == model.StatusQueued || st.Status == model.StatusRunning {
				pending = true
			}
		case st.Status != model.StatusDone:
			pending = true
		}
	}
	for _, m := range c.Materials {
		for _, st := range m.Stages {
			if st.Stage == model.StageMaterialTerms && st.Status != model.StatusDone {
				pending = true
			}
		}
	}
	c.GlossaryPending = pending
}

func courseCard(c api.Course) api.CourseCard {
	return api.CourseCard{
		ID: c.ID, Name: c.Name, Code: c.Code,
		LectureCount: c.LectureCount, TermCount: c.TermCount,
		DraftCount: c.DraftCount, StaleCount: c.StaleLectureCount,
		RunningCount: c.RunningCount, PatchesPending: c.PatchesPending,
		FailedCount:    c.FailedCount,
		LastActivityAt: c.LastActivityAt,
		CourseActions:  append([]string{}, c.CourseActions...),
	}
}

// buildDashboard computes GET /api/dashboard from the mock state.
func buildDashboard(courses []api.Course, lectures []api.Lecture, usage []api.ProviderUsage, now time.Time) api.Dashboard {
	byID := map[string]api.Course{}
	for _, c := range courses {
		byID[c.ID] = c
	}
	ref := func(l api.Lecture) api.LectureRef {
		c := byID[l.CourseID]
		return api.LectureRef{Lecture: l, CourseName: c.Name, CourseCode: c.Code}
	}
	d := api.Dashboard{
		InProgress: []api.LectureRef{}, Drafts: []api.CourseCount{}, Reviews: []api.ReviewGroup{},
		Stale: []api.CourseCount{}, Failures: []api.FailureItem{}, RecentDone: []api.LectureRef{},
		UsageToday: usage, Courses: []api.CourseCard{},
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}
	if d.UsageToday == nil {
		d.UsageToday = []api.ProviderUsage{}
	}
	var inprog, done []api.Lecture
	cutoff := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	for _, l := range lectures {
		c, ok := byID[l.CourseID]
		if !ok {
			continue
		}
		if inProgress(&l) {
			inprog = append(inprog, l)
		}
		if allDone(&l) {
			done = append(done, l)
		}
		if l.Counts.PatchesPending > 0 {
			d.Reviews = append(d.Reviews, api.ReviewGroup{
				LectureID: l.ID, LectureTitle: l.Title, Label: l.Label,
				CourseID: c.ID, CourseName: c.Name, CourseCode: c.Code, Count: l.Counts.PatchesPending,
				KindTag: tagOf(l), // lecture kinds (kinds.go)
			})
			d.ReviewTotal += l.Counts.PatchesPending
		}
		listed := false
		for _, st := range l.Stages {
			if finalFailure(st) && st.FinishedAt != nil && *st.FinishedAt >= cutoff {
				listed = true
				d.Failures = append(d.Failures, api.FailureItem{
					LectureID: l.ID, LectureTitle: l.Title, Label: l.Label,
					CourseID: c.ID, CourseName: c.Name, CourseCode: c.Code, Stage: st.Stage,
					Error: st.Error, ErrorInfo: st.ErrorInfo, FinishedAt: st.FinishedAt,
					AllowedActions: append([]string{}, st.AllowedActions...),
					KindTag:        tagOf(l), // lecture kinds (kinds.go)
				})
			}
		}
		// v3.2.x r7 (T24 S2): still failed, but every failure predates the window
		if !listed && failedLecture(&l) {
			d.EarlierFailedTotal++
		}
	}
	sort.SliceStable(inprog, func(i, j int) bool { return launched(inprog[i]) < launched(inprog[j]) })
	for _, l := range inprog {
		d.InProgress = append(d.InProgress, ref(l))
	}
	sort.SliceStable(done, func(i, j int) bool { return exportFinished(&done[i]) > exportFinished(&done[j]) })
	for i, l := range done {
		if i == 10 {
			break
		}
		d.RecentDone = append(d.RecentDone, ref(l))
	}
	sort.SliceStable(d.Failures, func(i, j int) bool { return deref(d.Failures[i].FinishedAt) > deref(d.Failures[j].FinishedAt) })
	for _, c := range courses {
		if c.DraftCount > 0 {
			d.Drafts = append(d.Drafts, api.CourseCount{CourseID: c.ID, CourseName: c.Name, CourseCode: c.Code, Count: c.DraftCount})
			d.DraftTotal += c.DraftCount
		}
		if c.StaleLectureCount > 0 {
			d.Stale = append(d.Stale, api.CourseCount{CourseID: c.ID, CourseName: c.Name, CourseCode: c.Code, Count: c.StaleLectureCount})
			d.StaleTotal += c.StaleLectureCount
		}
		d.Courses = append(d.Courses, courseCard(c))
	}
	return d
}

func launched(l api.Lecture) string {
	if l.LaunchedAt != nil {
		return *l.LaunchedAt
	}
	return l.CreatedAt
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
