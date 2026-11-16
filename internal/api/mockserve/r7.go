// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// Lecture.usage_total (v3.2.x r7, T25 S1): the real back end adds up every
// job the lecture ever had. The mock keeps, per lecture, the usage of the
// runs that are no longer current (store.history): seeded for "Joins and
// Subqueries" (an earlier full translation, then one sentence retranslated,
// as in T25) and grown by every rerun (restartFrom). Like the real API it
// is only in GET /api/lectures/{id}?usage_total=1 (lectureJSON).

// historyLectureFile is the lecture seeded with an earlier run.
const historyLectureFile = "db-lecture-3.mp4"

// seedHistory: the earlier full translation of historyLectureFile.
func seedHistory(s *store) {
	s.history = map[string]api.UsageStats{}
	for _, l := range s.lectures {
		if l.OriginalFilename == historyLectureFile {
			cost := 0.1842
			s.history[l.ID] = api.UsageStats{PromptTokens: 1_118_400, CacheHitTokens: 806_200, CompletionTokens: 23_650, EstimatedCost: &cost}
		}
	}
}

// retireUsage moves a stage's usage into the lecture's history before the
// stage is reset for a new job.
func (s *store) retireUsage(lectureID string, u api.UsageStats) {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.CacheHitTokens == 0 && u.EstimatedCost == nil {
		return
	}
	if s.history == nil {
		s.history = map[string]api.UsageStats{}
	}
	s.history[lectureID] = addUsageStats(s.history[lectureID], u)
}

func addUsageStats(a, b api.UsageStats) api.UsageStats {
	out := api.UsageStats{PromptTokens: a.PromptTokens + b.PromptTokens, CacheHitTokens: a.CacheHitTokens + b.CacheHitTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens}
	if a.EstimatedCost != nil || b.EstimatedCost != nil {
		c := 0.0
		for _, p := range []*float64{a.EstimatedCost, b.EstimatedCost} {
			if p != nil {
				c += *p
			}
		}
		out.EstimatedCost = &c
	}
	return out
}

// withUsageTotal is a copy of l with usage_total: its usage plus its history.
func (s *store) withUsageTotal(l *api.Lecture) *api.Lecture {
	out := *l
	t := addUsageStats(l.Usage, s.history[l.ID])
	out.UsageTotal = &t
	return &out
}

// Scenario "old_failures" (v3.2.x r7, T24 S2): the default data with the
// translate failure of COMP2120 "Deadlocks" three days old. The dashboard's
// failure group (24 h) keeps only the RISC-V guest talk; Dashboard
// .earlier_failed_total is 1 ("还有 1 节更早失败") and COMP2120 still counts
// it in failed_count.

const oldFailureAge = 72 * time.Hour

func setupOldFailures(s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	age := func(p *string) *string {
		if p == nil {
			return nil
		}
		t, err := time.Parse(time.RFC3339, *p)
		if err != nil {
			return p
		}
		v := t.Add(-oldFailureAge).UTC().Format(time.RFC3339)
		return &v
	}
	for i := range s.lectures {
		l := &s.lectures[i]
		for j := range l.Stages {
			st := &l.Stages[j]
			if st.Stage != model.StageTranslate || !finalFailure(*st) {
				continue
			}
			st.StartedAt, st.FinishedAt = age(st.StartedAt), age(st.FinishedAt)
			for k := range s.courses {
				if s.courses[k].ID == l.CourseID {
					finalizeCourse(&s.courses[k], s.lectures, s.kctx())
				}
			}
		}
	}
}
