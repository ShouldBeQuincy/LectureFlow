// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/glossary"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// The glossary janitor (SPEC-v2 G3; lead rulings 4, 5, 11): "整理术语表",
// the latest report and undoing one of its changes.

// lastModelFallback is JanitorStatus.LastModel of a fallback report (G2 rule).
const lastModelFallback = "常用词规则"

// janitorConfigured is state.JanitorConfigured for a view: a read error is
// logged and shown as "not configured" (the course page must still load;
// "整理术语表" then reports the error itself).
func (s *Server) janitorConfigured(ctx context.Context) bool {
	ok, err := state.JanitorConfigured(ctx, s.st)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("janitor configured", "err", err)
		}
		return false
	}
	return ok
}

// janitorStatus is Course.Janitor (G3.5). review is the course's current
// glossary_review job (has: it exists). Last* describe the latest run
// (glossary.LastRun, also a run that changed nothing); LastChanges counts
// the changes not undone in the latest report that changed something
// (glossary.LatestReport), the one "查看整理报告" lists for undo. Unreadable
// runs or reports are logged and left out, like missing ones. kv is the
// store the runs and reports are read from (courseReads.kv).
func (s *Server) janitorStatus(ctx context.Context, kv db.Store, courseID string, review model.Job, has, configured bool) JanitorStatus {
	js := JanitorStatus{Configured: configured}
	if has && (review.Status == model.StatusQueued || review.Status == model.StatusRunning) {
		js.Running = true
	}
	soft := func(what string, err error) bool {
		switch {
		case err == nil:
			return true
		case !errors.Is(err, db.ErrNotFound) && ctx.Err() == nil:
			slog.Warn(what, "course", courseID, "err", err)
		}
		return false
	}
	run, err := glossary.LastRun(ctx, kv, courseID)
	if !soft("janitor last run", err) {
		return js
	}
	js.LastAt, js.LastVersion, js.LastFallback, js.LastModel = run.CreatedAt, run.Version, run.Fallback, runModel(run)
	rep, err := glossary.LatestReport(ctx, kv, courseID)
	if !soft("janitor report", err) {
		return js
	}
	for _, it := range rep.Items {
		if !it.Undone {
			js.LastChanges++
		}
	}
	return js
}

// runModel is JanitorStatus.LastModel of a run: "Anthropic / claude-opus-…",
// or 常用词规则 for the G2 rule.
func runModel(rep model.JanitorReport) string {
	switch {
	case rep.Fallback:
		return lastModelFallback
	case rep.ProviderName != "" && rep.Model != "":
		return rep.ProviderName + " / " + rep.Model
	}
	return rep.ProviderName + rep.Model
}

// janitorChanged runs after the janitor's rule or an undo changed the
// glossary: the course invalidation with the janitor reason ("术语表已整理
// （vN → vM）", G3.1) and the SSE notices (terms, course).
func (s *Server) janitorChanged(courseID string, changed bool) {
	if changed {
		s.inval.request(courseID, state.StaleCause{Kind: state.StaleJanitor})
	}
	s.hub.Changed("terms", courseID)
	s.hub.Changed("course", courseID)
}

// POST /api/courses/{id}/glossary/review ("整理术语表", G3.1):
//   - janitor configured: one glossary_review run is queued (several
//     requests merge; a request during a run makes it run once more) → 202
//     {queued: true};
//   - not configured (ruling 5): the G2 rule runs at once, without a model
//     → 200 {queued: false, report}.
func (s *Server) reviewGlossary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.st.GetCourse(ctx, id); err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	configured, err := state.JanitorConfigured(ctx, s.st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if configured {
		queued, err := state.EnqueueGlossaryReview(ctx, s.st, id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if queued {
			s.kick()
			s.hub.Changed("course", id)
			writeJSON(w, http.StatusAccepted, GlossaryReviewResult{Queued: true})
			return
		}
		// The role stopped being usable in between: the rule instead.
	}
	rep, changed, err := glossary.RunFallback(ctx, s.st, id, s.now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.janitorChanged(id, changed)
	writeJSON(w, http.StatusOK, GlossaryReviewResult{Report: &rep})
}

// GET /api/courses/{id}/glossary/report[?version=N | ?run=last] (G3.3,
// G3.5): the latest report that changed something (its items can be
// undone), the one stored for glossary version N, or with run=last the
// latest run — also one that changed nothing (time, model, rejected).
func (s *Server) glossaryReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.st.GetCourse(ctx, id); err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	q := r.URL.Query()
	raw, run := strings.TrimSpace(q.Get("version")), strings.TrimSpace(q.Get("run"))
	var rep model.JanitorReport
	var err error
	switch {
	case run != "" && (run != "last" || raw != ""):
		writeError(w, http.StatusBadRequest, "glossary.bad_run", nil)
		return
	case run == "last":
		rep, err = glossary.LastRun(ctx, s.st, id)
		err = notFoundAs(err, "glossary.no_runs")
	case raw != "":
		v, perr := strconv.Atoi(raw)
		if perr != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "glossary.bad_version", nil)
			return
		}
		rep, err = glossary.Report(ctx, s.st, id, v)
		if errors.Is(err, db.ErrNotFound) {
			err = apiErr(http.StatusNotFound, "glossary.report_not_found", map[string]any{"version": v})
		}
	default:
		rep, err = glossary.LatestReport(ctx, s.st, id)
		err = notFoundAs(err, "glossary.no_reports")
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// POST /api/courses/{id}/glossary/report/undo JanitorUndoInput (G3.5): the
// term goes back to its state before that change and is locked; the
// glossary version + 1 and the course is invalidated with the janitor
// reason. 409 when the item was undone already, 404 when the report, the
// item or the term is gone.
func (s *Server) undoGlossaryReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	var in JanitorUndoInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	in.TermID = strings.TrimSpace(in.TermID)
	if in.Version <= 0 || in.TermID == "" {
		writeError(w, http.StatusBadRequest, "glossary.undo_missing", nil)
		return
	}
	if _, err := s.st.GetCourse(ctx, id); err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	rep, err := glossary.Undo(ctx, s.st, id, in.Version, in.TermID, s.now())
	switch {
	case errors.Is(err, glossary.ErrAlreadyUndone):
		writeError(w, http.StatusConflict, "glossary.already_undone", nil)
		return
	case err != nil:
		s.fail(w, r, notFoundAs(err, "glossary.undo_not_found"))
		return
	}
	s.janitorChanged(id, true)
	writeJSON(w, http.StatusOK, rep)
}
