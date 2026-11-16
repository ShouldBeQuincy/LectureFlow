// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// SPEC-v2 P13: calendar numbering of lectures (computed on every read, never
// stored; phase-2 lead ruling 4) and the renumbering warning of P13.6.
//
// The inputs match stages.CourseLabels and stages.RefreshCourseSeq: the
// lectures in ListLectures order (created_at), recorded_at, term_start and
// skip_weeks read as local wall-clock times.

// lectureNumber is the calendar number of one lecture (lecture kinds:
// calendar.LabelKinds through state.NumberCourse — the effective kind,
// Label/CiteLabel per kind, KindN/KindCount/ShowN).
type lectureNumber struct {
	calendar.Numbered
	Seq  int    // position in calendar.Order + 1 (P13.2: the global order)
	Hint string // Lecture.CalendarHint when not numbered
	// KindSource is Lecture.kind_source: the stored source ("" → none), or
	// KindSourceDate when the past-lecture rule made the kind past_lecture.
	KindSource string
}

// numbering maps lecture id → its number within its course.
type numbering map[string]lectureNumber

// Calendar hints (Lecture.CalendarHint): why a lecture shows "W??".
const (
	hintTermStart   = "term_start"
	hintRecordedAt  = "recorded_at"
	hintSkippedWeek = "skipped_week"
	hintBeforeTerm  = "before_term"
)

// recordedTime parses lecture.recorded_at (nil when unset or unreadable).
func recordedTime(at *string) *time.Time { return calendar.RecordedTime(at, time.Local) }

// courseTerm parses course.term_start and skip_weeks (unreadable entries
// are ignored, like missing ones).
func courseTerm(termStart *string, skipWeeks []string) (*time.Time, []time.Time) {
	return calendar.CourseTerm(termStart, skipWeeks, time.Local)
}

// numberLectures numbers the lectures of course c (lecs in ListLectures
// order, every lecture of the course) with the kind catalog kinds: the
// course's calendar, numbering mode and each lecture's stored kind.
func numberLectures(c model.Course, lecs []model.Lecture, kinds []model.LectureKind) numbering {
	ts, _ := courseTerm(c.TermStart, c.SkipWeeks)
	nums := state.NumberCourse(c, lecs, kinds)
	out := make(numbering, len(lecs))
	for pos, i := range calendar.Order(state.LectureTimes(lecs)) {
		l := lecs[i]
		n := lectureNumber{Numbered: nums[i], Seq: pos + 1, KindSource: viewKindSource(l, nums[i].Kind)}
		if !n.OK {
			rec := recordedTime(l.RecordedAt)
			switch {
			case ts == nil:
				n.Hint = hintTermStart
			case rec == nil:
				n.Hint = hintRecordedAt
			case calendar.AlignMonday(*rec).Before(calendar.AlignMonday(*ts)):
				n.Hint = hintBeforeTerm
			default:
				n.Hint = hintSkippedWeek
			}
		}
		out[l.ID] = n
	}
	return out
}

// viewKindSource is Lecture.kind_source of lecture l whose effective kind
// is kind (ViewKindSource).
func viewKindSource(l model.Lecture, kind string) string {
	return ViewKindSource(l.Kind, l.KindSource, kind)
}

// ViewKindSource is Lecture.kind_source (shared with mockserve): the stored
// source of the stored kind ("" or unknown → none), or KindSourceDate when
// the past-lecture rule, not a stored kind, made the effective kind a past
// lecture.
func ViewKindSource(stored, source, effective string) string {
	if !model.ValidKindSource(source) {
		source = model.KindSourceNone
	}
	if source != model.KindSourceUser && effective == model.KindPastLecture && stored != model.KindPastLecture {
		return KindSourceDate
	}
	return source
}

// courseNumbering numbers the lectures of course c (read from the store).
func (s *Server) courseNumbering(ctx context.Context, c model.Course) (numbering, []model.Lecture, error) {
	lecs, err := s.st.ListLectures(ctx, c.ID)
	if err != nil {
		return nil, nil, err
	}
	kinds, err := s.st.ListLectureKinds(ctx)
	if err != nil {
		return nil, nil, err
	}
	return numberLectures(c, lecs, kinds), lecs, nil
}

// orderLectures sorts lecs into the list order of P13.4 (by recorded_at,
// lectures without one last) using their numbering.
func orderLectures(lecs []model.Lecture, nums numbering) []model.Lecture {
	out := make([]model.Lecture, len(lecs))
	placed := make([]bool, len(lecs))
	for _, l := range lecs {
		n, ok := nums[l.ID]
		if !ok || n.Seq < 1 || n.Seq > len(lecs) || placed[n.Seq-1] {
			return append([]model.Lecture(nil), lecs...) // inconsistent numbering: keep the given order
		}
		out[n.Seq-1], placed[n.Seq-1] = l, true
	}
	return out
}

// cite converts a numbering to what cite.Build takes.
func (n numbering) cite() map[string]calendar.Numbered {
	out := make(map[string]calendar.Numbered, len(n))
	for id, x := range n {
		out[id] = x.Numbered
	}
	return out
}

// ---- P13.6: renumbering after a citation export ----

// citeExported reports whether a citation JSON of the course was exported
// (kv cite_exported:{course}; "" = cleared).
func (s *Server) citeExported(ctx context.Context, courseID string) (bool, error) {
	return citeExportedIn(ctx, s.st, courseID)
}

// citeExportedIn is citeExported reading from st (course_reads.go).
func citeExportedIn(ctx context.Context, st db.Store, courseID string) (bool, error) {
	v, ok, err := st.GetKV(ctx, model.KVCiteExportPrefix+courseID)
	return ok && strings.TrimSpace(v) != "", err
}

// renumberChanges lists the lectures whose citation label (CiteLabel, as
// written into the citation JSON) differs between before and after, in the
// after list order. It is the measure of the automatic renumbering notice
// (stages.renumberedOthers): an unnumbered lecture keeps Label "W??" while
// its "W??-{id prefix}" changes when the prefix shrinks or grows, and its
// exported references break all the same (review item 5).
func renumberChanges(lecs []model.Lecture, before, after numbering) []RenumberChange {
	out := []RenumberChange{}
	for _, l := range orderLectures(lecs, after) {
		b, a := citeLabelOf(before[l.ID]), citeLabelOf(after[l.ID])
		if a != b {
			out = append(out, RenumberChange{LectureID: l.ID, Title: l.Title, From: b, To: a})
		}
	}
	return out
}

// citeLabelOf is n's citation label: CiteLabel, else Label, else "W??"
// (a lecture missing from its numbering).
func citeLabelOf(n lectureNumber) string {
	switch {
	case n.CiteLabel != "":
		return n.CiteLabel
	case n.Label != "":
		return n.Label
	}
	return calendar.Unknown
}

// renumberConflict is returned by checkRenumber: the 409 body of P13.6.
type renumberConflict struct{ body RenumberConflict }

func (e *renumberConflict) Error() string { return "calendar.renumber" }

// checkRenumber compares the numbers before and after a change of the
// course calendar, of recorded_at values or of lecture kinds (P13.6; the
// numbering mode is display only and never asks). When citations were
// exported (and the request did not confirm) and any number changes, it
// returns a *renumberConflict. lecs are the course's lectures before the
// change; after is the course and afterLecs the same lectures as they will
// be (new recorded_at values or kinds).
func (s *Server) checkRenumber(ctx context.Context, c model.Course, lecs []model.Lecture, after model.Course,
	afterLecs []model.Lecture, confirm bool) error {
	if confirm {
		return nil
	}
	exported, err := s.citeExported(ctx, c.ID)
	if err != nil || !exported {
		return err
	}
	kinds, err := s.st.ListLectureKinds(ctx)
	if err != nil {
		return err
	}
	if ch := renumberChanges(afterLecs, numberLectures(c, lecs, kinds), numberLectures(after, afterLecs, kinds)); len(ch) > 0 {
		return &renumberConflict{body: RenumberConflict{
			Error:   model.ErrorInfo{Code: "calendar.renumber", Params: map[string]any{"count": len(ch)}},
			Changes: ch,
		}}
	}
	return nil
}

// failRenumber writes a *renumberConflict as its 409 body; other errors as
// usual. It reports whether err was handled as a conflict.
func (s *Server) failRenumber(w http.ResponseWriter, r *http.Request, err error) {
	if rc, ok := err.(*renumberConflict); ok {
		writeJSON(w, http.StatusConflict, rc.body)
		return
	}
	s.fail(w, r, err)
}

// ---- stages helpers (organizer-p2's calendar service) ----

// stageDeps are the stages.Deps the calendar service of P13.3 needs (the
// same collaborators the scheduler's stages get in cmd/lectureflow).
func (s *Server) stageDeps() stages.Deps {
	d := stages.Deps{Store: s.st, Cfg: s.cfg, LLM: s.llmc, DataDir: s.dataDir, Events: s.hub, Settings: s.set}
	if s.sc != nil {
		d.Invalidate = func(ctx context.Context, courseID string, cause state.StaleCause) error {
			return s.sc.InvalidateCourseWith(ctx, courseID, cause)
		}
	}
	return d
}

// refreshSeq rewrites lecture.seq of a course after a calendar or
// recorded_at change (P13.2). The views compute the order themselves, so a
// failure is only logged.
func (s *Server) refreshSeq(ctx context.Context, courseID string) {
	if err := stages.RefreshCourseSeq(ctx, s.stageDeps(), courseID); err != nil && ctx.Err() == nil {
		s.logWarn("refresh course seq", courseID, err)
	}
}

// parseRule decodes course.filename_rule (nil when unset or unreadable).
func parseRule(raw *string) *calendar.FilenameRule {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	var r calendar.FilenameRule
	if json.Unmarshal([]byte(*raw), &r) != nil || r.Regex == "" {
		return nil
	}
	return &r
}

// kvValue reads a kv entry; "" (cleared) counts as absent.
func (s *Server) kvValue(ctx context.Context, key string) (string, bool, error) {
	return kvValueIn(ctx, s.st, key)
}

// kvValueIn is kvValue reading from st (course_reads.go).
func kvValueIn(ctx context.Context, st db.Store, key string) (string, bool, error) {
	v, ok, err := st.GetKV(ctx, key)
	if err != nil || !ok || strings.TrimSpace(v) == "" {
		return "", false, err
	}
	return v, true, nil
}

// courseOr404 reads a course (404 course.not_found).
func (s *Server) courseOr404(ctx context.Context, id string) (model.Course, error) {
	c, err := s.st.GetCourse(ctx, id)
	return c, notFoundAs(err, "course.not_found")
}

var _ = db.ErrNotFound

// ---- recording times (P13.3.4) ----

// uploadedAt is the upload time of a lecture (created_at) as local time.
func uploadedAt(l model.Lecture) time.Time {
	t, err := time.Parse(time.RFC3339, l.CreatedAt)
	if err != nil {
		return time.Now().In(time.Local)
	}
	return t.In(time.Local)
}

// creationTime is the media creation_time prep recorded for a lecture
// (kv stages.KVCreationTimePrefix), nil when unknown.
func (s *Server) creationTime(ctx context.Context, lectureID string) *time.Time {
	raw, ok, err := s.kvValue(ctx, stages.KVCreationTimePrefix+lectureID)
	if err != nil || !ok {
		return nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return nil
	}
	t = t.In(time.Local)
	return &t
}

// sample is one lecture as a file-name sample (P13.3.2).
func (s *Server) sample(ctx context.Context, l model.Lecture) calendar.Sample {
	return calendar.Sample{Filename: l.OriginalFilename, UploadedAt: uploadedAt(l), CreationTime: s.creationTime(ctx, l.ID)}
}

// predictRecordedAt is the automatic recording time of l (P13.3.4 without
// a user value): the course rule over original_filename, else the media's
// creation time recorded by prep. It mirrors stages.ResolveRecordedAt for
// the renumbering check made before anything is written.
func (s *Server) predictRecordedAt(ctx context.Context, c model.Course, l model.Lecture) (*string, string) {
	up := uploadedAt(l)
	var fromName *time.Time
	if rule := parseRule(c.FilenameRule); rule != nil && l.OriginalFilename != "" {
		fromName = calendar.Parse(*rule, calendar.Sample{Filename: l.OriginalFilename, UploadedAt: up}).RecordedAt
	}
	t, src := calendar.ResolveRecordedAt(nil, fromName, s.creationTime(ctx, l.ID), up)
	if t == nil {
		return nil, model.RecordedAtNone
	}
	v := calendar.FormatRecordedAt(*t)
	return &v, src
}
