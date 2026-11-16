// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"log/slog"
	"net/http"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Lecture.usage_total (v3.2.x r7, T25 S1): what a lecture has used in all.
// Lecture.usage adds up the current stages' latest jobs, so it shrinks when a
// rerun, an update or a retranslated sentence replaces a job; usage_total
// adds up every job the lecture ever had — retired ones and auxiliary tasks
// included — each priced as usage prices it (usageOf: the cost the run
// recorded with each response, the same responses usage_daily, "今日用量",
// counts). A retry keeps its job and that job's accumulated stats, so every
// attempt is in it.
//
// The history query scans the job table (db.JobHistory), so it runs only
// when asked for: GET /api/lectures/{id}?usage_total=1, the lecture page's
// own read. The course page's refreshes of one lecture on SSE events, the
// responses of single-lecture operations, the lists and the dashboard all
// leave the field out.

// wantUsageTotal reports whether r asks for Lecture.usage_total.
func wantUsageTotal(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Query().Get("usage_total") == "1"
}

// usageTotal is Lecture.usage_total of lecture id; nil when the store
// cannot list a job history or the read fails (logged: the field is extra,
// the lecture is still shown).
func (s *Server) usageTotal(ctx context.Context, id string) *UsageStats {
	h, ok := s.st.(db.JobHistory)
	if !ok {
		return nil
	}
	jobs, err := h.ListJobHistory(ctx, model.ScopeLecture, id)
	if err != nil {
		slog.Warn("lecture usage_total", "lecture", id, "err", err)
		return nil
	}
	var sum UsageStats
	for _, j := range jobs {
		addUsage(&sum, s.usageOf(j))
	}
	return &sum
}
