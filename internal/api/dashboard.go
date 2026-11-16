// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"log/slog"
	"net/http"
	"sort"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// GET /api/dashboard (SPEC-v1.1 A9) and GET /api/resources (A10).

const (
	recentDoneMax  = 10
	failureHorizon = 24 * time.Hour
)

// lecEntry is one lecture with what the dashboard needs about it: its
// pipeline jobs, auxiliary tasks and calendar number.
type lecEntry struct {
	l         model.Lecture
	c         model.Course
	jobs, aux []model.Job
	num       lectureNumber
	pending   int
	nums      numbering // the course's numbering (Lecture.materials)
}

// label is the lecture's calendar number (P13.6: "W02L1", "W??").
func (e lecEntry) label() string {
	if e.num.Label == "" {
		return calendar.Unknown
	}
	return e.num.Label
}

func allDone(jobs []model.Job) bool {
	if len(jobs) == 0 {
		return false
	}
	for _, j := range jobs {
		if j.Status != model.StatusDone {
			return false
		}
	}
	return true
}

func (s *Server) getDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.now()
	d := Dashboard{
		InProgress: []LectureRef{}, Drafts: []CourseCount{}, Reviews: []ReviewGroup{}, Stale: []CourseCount{},
		Failures: []FailureItem{}, RecentDone: []LectureRef{}, UsageToday: []ProviderUsage{}, Courses: []CourseCard{},
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}
	courses, err := s.st.ListCourses(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// One query for every current job; per lecture only the pending patch
	// count, plus full lecture views for the few lectures that are listed.
	ix, err := s.loadJobs(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	courseIDs := make([]string, len(courses))
	for i, c := range courses {
		courseIDs[i] = c.ID
	}
	// every course's lectures, term and patch counts at once, and the kind
	// catalog once (course_reads.go; courseLectures per course before)
	rd, err := s.readCourses(ctx, courseIDs, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	kinds, err := s.st.ListLectureKinds(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var all []lecEntry
	for _, c := range courses {
		nums := numberLectures(c, rd.lectures[c.ID], kinds)
		lecs := orderLectures(rd.lectures[c.ID], nums)
		pending := map[string]int{}
		a := aggregate(c, lecs, ix, rd.pending, pending)
		code := strOrNil(c.Code)
		d.Courses = append(d.Courses, CourseCard{
			ID: c.ID, Name: c.Name, Code: code, LectureCount: a.lectures, TermCount: rd.terms[c.ID],
			DraftCount: a.drafts, StaleCount: a.stale, RunningCount: a.running, PatchesPending: a.patchesPending, FailedCount: a.failed,
			LastActivityAt: a.lastActivity, CourseActions: a.actions(),
		})
		if a.drafts > 0 {
			d.Drafts = append(d.Drafts, CourseCount{CourseID: c.ID, CourseName: c.Name, CourseCode: code, Count: a.drafts})
			d.DraftTotal += a.drafts
		}
		if a.stale > 0 {
			d.Stale = append(d.Stale, CourseCount{CourseID: c.ID, CourseName: c.Name, CourseCode: code, Count: a.stale})
			d.StaleTotal += a.stale
		}
		for _, l := range lecs {
			e := lecEntry{l: l, c: c, jobs: ix.of(model.ScopeLecture, l.ID), aux: ix.aux(model.ScopeLecture, l.ID),
				num: nums[l.ID], pending: pending[l.ID], nums: nums}
			all = append(all, e)
			if e.pending > 0 {
				d.Reviews = append(d.Reviews, ReviewGroup{LectureID: l.ID, LectureTitle: l.Title, Label: e.label(),
					CourseID: c.ID, CourseName: c.Name, CourseCode: code, Count: e.pending, KindTag: kindTag(e.num)})
				d.ReviewTotal += e.pending
			}
		}
	}

	busyByCourse := map[string]map[string]bool{}
	// Lecture.materials of the listed lectures only: the course's materials
	// (with their pages) are read once per course, when a lecture of it is listed.
	matsByCourse := map[string]map[string][]MaterialUse{}
	var counts map[string]db.LectureCounts // the listed lectures', read at once (lecture_counts.go)
	ref := func(e lecEntry) (LectureRef, error) {
		mats, ok := matsByCourse[e.c.ID]
		if !ok {
			var err error
			if mats, err = s.courseMaterialUses(ctx, e.c.ID, e.nums); err != nil {
				return LectureRef{}, err
			}
			matsByCourse[e.c.ID] = mats
		}
		busy, ok := busyByCourse[e.c.ID]
		if !ok {
			var err error
			if busy, err = s.boundBusy(ctx, e.c.ID, ix); err != nil {
				return LectureRef{}, err
			}
			busyByCourse[e.c.ID] = busy
		}
		v, err := s.lectureViewJobs(ctx, e.l, lectureInfo{jobs: e.jobs, aux: e.aux, boundBusy: busy[e.l.ID], num: e.num, materials: mats[e.l.ID],
			counts: countsFor(counts, e.l.ID)})
		return LectureRef{Lecture: v, CourseName: e.c.Name, CourseCode: strOrNil(e.c.Code)}, err
	}
	// 进行中: launched and not finished, oldest launch first. A lecture whose
	// only unfinished stages are stale is listed under 需要更新 instead: nothing
	// runs or waits there (lead ruling, NOTES.md "v1.1 lead 复核").
	var inprog, done []lecEntry
	cutoff := now.Add(-failureHorizon).UTC().Format(time.RFC3339)
	for _, e := range all {
		if state.IsDraft(e.l) {
			continue
		}
		if unfinished(e.jobs) {
			inprog = append(inprog, e)
		}
		if x, ok := findJob(e.jobs, model.StageExport); ok && x.Status == model.StatusDone {
			done = append(done, e)
		}
		actions := state.StageActions(e.jobs, s.mediaDeleted(e.l))
		listed := false
		for _, j := range orderJobs(model.ScopeLecture, e.jobs) {
			// failed is final: automatic retries go failed → queued in one transaction.
			if j.Status != model.StatusFailed || j.FinishedAt == nil || *j.FinishedAt < cutoff {
				continue
			}
			listed = true
			acts := actions[j.Stage]
			if acts == nil {
				acts = []string{}
			}
			// The error as in StageView (v2 P14.3): a coded job.error gives
			// error_info, and error is its detail (legacy text: as stored).
			errText, errInfo := jobErrorView(j)
			d.Failures = append(d.Failures, FailureItem{LectureID: e.l.ID, LectureTitle: e.l.Title, Label: e.label(),
				CourseID: e.c.ID, CourseName: e.c.Name, CourseCode: strOrNil(e.c.Code), Stage: j.Stage, Error: errText, ErrorInfo: errInfo,
				FinishedAt: strOrNil(j.FinishedAt), AllowedActions: acts, KindTag: kindTag(e.num)})
		}
		// v3.2.x r7 (T24 S2): a lecture still failed whose failures all
		// predate the window is counted, not listed ("还有 N 节更早失败").
		if !listed && failedLecture(e.l, e.jobs) {
			d.EarlierFailedTotal++
		}
	}
	launched := func(e lecEntry) string {
		if e.l.LaunchedAt != nil {
			return *e.l.LaunchedAt
		}
		return e.l.CreatedAt
	}
	sort.SliceStable(inprog, func(i, j int) bool { return launched(inprog[i]) < launched(inprog[j]) })
	exportAt := func(e lecEntry) string {
		if x, ok := findJob(e.jobs, model.StageExport); ok && x.FinishedAt != nil {
			return *x.FinishedAt
		}
		return ""
	}
	sort.SliceStable(done, func(i, j int) bool { return exportAt(done[i]) > exportAt(done[j]) })
	if len(done) > recentDoneMax {
		done = done[:recentDoneMax]
	}
	listed := make([]string, 0, len(inprog)+len(done))
	for _, e := range append(append([]lecEntry(nil), inprog...), done...) {
		listed = append(listed, e.l.ID)
	}
	if counts, err = s.lectureCounts(ctx, listed); err != nil {
		s.fail(w, r, err)
		return
	}
	for _, e := range inprog {
		v, err := ref(e)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		d.InProgress = append(d.InProgress, v)
	}
	for _, e := range done {
		v, err := ref(e)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		d.RecentDone = append(d.RecentDone, v)
	}
	sort.SliceStable(d.Failures, func(i, j int) bool {
		return *d.Failures[i].FinishedAt > *d.Failures[j].FinishedAt
	})
	usage, err := s.usageToday(r, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d.UsageToday = usage
	d.DevicePrompts = s.devicePrompts() // SPEC-v2 I9: "检测到新的转写设备，测试一下它的速度？"
	writeJSON(w, http.StatusOK, d)
}

// usageToday is "今日用量" (A9): usage_daily of the local day per provider.
// v2 P12.5: the cost is the accumulated estimated_cost (each request priced
// at its start time, peak or off-peak), shown when the provider has any
// price (provider-level or per model, D3) and not every request of the day
// was unpriced (settings.DisplayCost); cost_unpriced_requests counts the
// unpriced ones. Every configured provider is listed (zero when unused);
// rows of a deleted provider are kept — that money was spent — with its id
// as the name and no cost.
func (s *Server) usageToday(r *http.Request, now time.Time) ([]ProviderUsage, error) {
	out := []ProviderUsage{}
	if s.set == nil {
		return out, nil
	}
	day := now.In(time.Local).Format("2006-01-02")
	rows, err := s.st.ListUsageDaily(r.Context(), day)
	if err != nil {
		return nil, err
	}
	byProv := map[string]model.UsageDaily{}
	for _, u := range rows {
		if u.Day != day {
			continue
		}
		t := byProv[u.ProviderID]
		t.Requests += u.Requests
		t.PromptTokens += u.PromptTokens
		t.CacheHitTokens += u.CacheHitTokens
		t.CompletionTokens += u.CompletionTokens
		t.EstimatedCost += u.EstimatedCost
		byProv[u.ProviderID] = t
	}
	row := func(id, name string, u model.UsageDaily, p *model.Provider) ProviderUsage {
		pu := ProviderUsage{ProviderID: id, Name: name, Requests: u.Requests, PromptTokens: u.PromptTokens,
			CacheHit: u.CacheHitTokens, Completion: u.CompletionTokens}
		if u.PromptTokens > 0 {
			rate := float64(u.CacheHitTokens) / float64(u.PromptTokens)
			pu.CacheHitRate = &rate
		}
		unpriced := int64(0)
		if u.Requests > 0 {
			n, err := settings.UnpricedRequests(r.Context(), s.st, id, day)
			if err != nil {
				slog.Warn("usage today: unpriced requests", "provider", id, "err", err)
			}
			unpriced = n
		}
		pu.Cost, pu.CostUnpricedRequests = settings.DisplayCost(p, u.Requests, unpriced, u.EstimatedCost)
		if pu.Cost != nil {
			pu.Currency = settings.ProviderCurrency(*p)
		}
		return pu
	}
	seen := map[string]bool{}
	for _, p := range s.set.Providers() {
		p := p
		seen[p.ID] = true
		out = append(out, row(p.ID, p.Name, byProv[p.ID], &p))
	}
	var gone []string
	for id := range byProv {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		out = append(out, row(id, id+"（已删除）", byProv[id], nil))
	}
	return out, nil
}

// GET /api/resources (A10.3): the resource watcher's snapshot.
func (s *Server) getResources(w http.ResponseWriter, r *http.Request) {
	if s.res == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "resources"})
		return
	}
	writeJSON(w, http.StatusOK, s.res.Snapshot(r.Context()))
}

// unfinished reports whether a launched lecture's pipeline has not run to the
// end: some stage is pending, queued, running, failed or cancelled. done and
// stale stages count as finished (stale ones wait for "更新", A2.3).
func unfinished(jobs []model.Job) bool {
	for _, j := range jobs {
		switch j.Status {
		case model.StatusPending, model.StatusQueued, model.StatusRunning, model.StatusFailed, model.StatusCancelled:
			return true
		}
	}
	return false
}
