// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/calendar"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/glossary"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// ---- courses (SPEC §8) ----

func (s *Server) listCourses(w http.ResponseWriter, r *http.Request) {
	cs, err := s.st.ListCourses(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ix, err := s.loadJobs(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// the per-course data of every course at once (course_reads.go)
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	rd, err := s.readCourses(r.Context(), ids, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	janitor := s.janitorConfigured(r.Context())
	out := make([]Course, 0, len(cs))
	for _, c := range cs {
		v, err := s.courseViewJobs(r.Context(), c, ix, janitor, rd)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getCourse(w http.ResponseWriter, r *http.Request) {
	s.respondCourse(w, r, r.PathValue("id"), http.StatusOK)
}

func (s *Server) respondCourse(w http.ResponseWriter, r *http.Request, id string, status int) {
	c, err := s.st.GetCourse(r.Context(), id)
	if err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	v, err := s.courseView(r.Context(), c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, v)
}

// POST /api/courses {name, code?} (SPEC §8, v1.1 A8.1 step 1).
func (s *Server) createCourse(w http.ResponseWriter, r *http.Request) {
	var req CourseCreate
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	name, err := courseName(req.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c := model.Course{ID: db.NewID(), Name: name, CreatedAt: db.Now()}
	if req.Code != nil {
		if c.Code, err = courseCode(*req.Code); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.st.CreateCourse(r.Context(), c); err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("course", c.ID)
	s.respondCourse(w, r, c.ID, http.StatusCreated)
}

func courseName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	switch {
	case name == "":
		return "", apiErr(http.StatusBadRequest, "course.name_empty", nil)
	case utf8.RuneCountInString(name) > 200:
		return "", apiErr(http.StatusBadRequest, "course.name_too_long", map[string]any{"max": 200})
	}
	return name, nil
}

// courseCode validates a course code (A5.1 course.code, e.g. CSC1003): ""
// clears it. It is the course part of every citation ref and id ("CSC1003
// L03@12:34", "CSC1003.L03.s0001", A11.3), so it may not contain spaces.
func courseCode(raw string) (*string, error) {
	code := strings.TrimSpace(raw)
	switch {
	case code == "":
		return nil, nil
	case utf8.RuneCountInString(code) > 32:
		return nil, apiErr(http.StatusBadRequest, "course.code_too_long", map[string]any{"max": 32})
	case strings.IndexFunc(code, unicode.IsSpace) >= 0:
		return nil, apiErr(http.StatusBadRequest, "course.code_has_space", nil)
	}
	return &code, nil
}

// PATCH /api/courses/{id} CoursePatch (v1.1 name, code; v2 P13.4 term
// start and skipped weeks; lecture kinds: numbering, display only, 400
// course.numbering_invalid). code "" and term_start "" clear. term_start may
// be any day of teaching week 1 and is aligned to its Monday; skip_weeks are
// aligned, deduplicated and sorted. A calendar change that renumbers lectures
// of a course whose citations were exported answers 409 RenumberConflict
// unless confirmed (P13.6).
func (s *Server) patchCourse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req CoursePatch
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Name == nil && req.Code == nil && req.TermStart == nil && req.SkipWeeks == nil && req.Numbering == nil {
		writeError(w, http.StatusBadRequest, "request.nothing_to_change", map[string]any{"fields": "name, code, term_start, skip_weeks, numbering"})
		return
	}
	if req.Numbering != nil && !model.ValidNumbering(*req.Numbering) {
		writeError(w, http.StatusBadRequest, "course.numbering_invalid", map[string]any{"value": *req.Numbering})
		return
	}
	var name string
	var err error
	if req.Name != nil {
		if name, err = courseName(*req.Name); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	var code *string
	if req.Code != nil {
		if code, err = courseCode(*req.Code); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	var term *string
	if req.TermStart != nil {
		if term, err = termStartValue(*req.TermStart); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	var skip []string
	if req.SkipWeeks != nil {
		if skip, err = skipWeeksValue(*req.SkipWeeks); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	c, err := s.courseOr404(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	newTerm, newSkip := c.TermStart, c.SkipWeeks
	if req.TermStart != nil {
		newTerm = term
	}
	if req.SkipWeeks != nil {
		newSkip = skip
	}
	calendarChanged := !sameStr(newTerm, c.TermStart) || strings.Join(newSkip, ",") != strings.Join(c.SkipWeeks, ",")
	if calendarChanged {
		lecs, err := s.st.ListLectures(ctx, c.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		after := c
		after.TermStart, after.SkipWeeks = newTerm, newSkip
		if err := s.checkRenumber(ctx, c, lecs, after, lecs, req.Confirm); err != nil {
			s.failRenumber(w, r, err)
			return
		}
	}
	err = s.st.Tx(ctx, func(tx db.Store) error {
		if req.Name != nil {
			if err := tx.SetCourseName(ctx, c.ID, name); err != nil {
				return err
			}
		}
		if req.Code != nil {
			if err := tx.SetCourseCode(ctx, c.ID, code); err != nil {
				return err
			}
		}
		if req.Numbering != nil {
			// display only (lecture kinds): no renumbering check
			if err := tx.SetCourseNumbering(ctx, c.ID, *req.Numbering); err != nil {
				return err
			}
		}
		if calendarChanged {
			return tx.SetCourseCalendar(ctx, c.ID, newTerm, newSkip)
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	if calendarChanged {
		s.refreshSeq(ctx, c.ID)
		// teaching weeks moved: the slides bound to a week may now apply to
		// other lectures (lead freeze 1.5)
		s.materialSetsChanged(c.ID)
	}
	s.hub.Changed("course", c.ID)
	s.respondCourse(w, r, c.ID, http.StatusOK)
}

// termStartValue validates a term start (P13.4): "" clears; any day of
// teaching week 1, stored as its Monday.
func termStartValue(raw string) (*string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	t, err := calendar.ParseDate(raw, time.Local)
	if err != nil {
		return nil, apiErr(http.StatusBadRequest, "course.term_start_invalid", map[string]any{"value": raw})
	}
	v := calendar.FormatDate(calendar.AlignMonday(t))
	return &v, nil
}

// skipWeeksValue validates the weeks not counted (P13.4): any day of each
// week, stored as Mondays, deduplicated and sorted; never nil.
func skipWeeksValue(raw []string) ([]string, error) {
	ts := make([]time.Time, 0, len(raw))
	for _, x := range raw {
		t, err := calendar.ParseDate(x, time.Local)
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "course.skip_week_invalid", map[string]any{"value": x})
		}
		ts = append(ts, t)
	}
	out := []string{}
	for _, t := range calendar.NormalizeSkipWeeks(ts) {
		out = append(out, calendar.FormatDate(t))
	}
	return out, nil
}

func sameStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ---- syllabus (SPEC §9.3: one file, .txt/.md/.pdf, re-upload replaces) ----

// acceptExt checks the extension of an uploaded file; code gets {filename}.
func acceptExt(exts []string, code string) func(string) error {
	return func(name string) error {
		ext := strings.ToLower(filepath.Ext(name))
		for _, e := range exts {
			if ext == e {
				return nil
			}
		}
		return apiErr(http.StatusBadRequest, code, map[string]any{"filename": name})
	}
}

// POST /api/courses/{id}/syllabus, multipart file (SPEC §8, §9.3; kept in
// v1.1): the file is stored as the original, its text (PDF extracted) is
// saved as syllabus.raw.txt AND used unchanged as SYLLABUS.md (not
// formatted, A8 §5 of the v1.1 brief), then glossary_seed runs again.
func (s *Server) uploadSyllabus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	courseID := r.PathValue("id")
	if _, err := s.st.GetCourse(ctx, courseID); err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	if err := checkRecordIDs(courseID); err != nil { // record_dir.go
		s.fail(w, r, err)
		return
	}
	if s.glossarySeedRunning(ctx, courseID) {
		writeError(w, http.StatusConflict, syllabusBusy, nil)
		return
	}
	if err := s.checkSpace(r); err != nil {
		s.fail(w, r, err)
		return
	}
	dir := model.SyllabusDir(s.dataDir, courseID)
	up, raw, err := s.receiveSyllabusFile(w, r, dir)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	final := filepath.Join(dir, "syllabus"+strings.ToLower(filepath.Ext(up.filename)))
	// Replace older syllabus files (possibly with another extension).
	if old, _ := filepath.Glob(filepath.Join(dir, "syllabus.*")); len(old) > 0 {
		for _, f := range old {
			if !strings.HasSuffix(f, ".tmp") {
				os.Remove(f)
			}
		}
	}
	if err := os.Rename(up.path, final); err != nil {
		os.Remove(up.path)
		s.fail(w, r, err)
		return
	}
	abs, _ := filepath.Abs(final)
	if err := s.writeRaw(courseID, up.filename, raw); err != nil {
		s.fail(w, r, err)
		return
	}
	// A file without any text (a scanned PDF) gets no SYLLABUS.md;
	// glossary_seed then reports that no terms were found (NOTES.md).
	mdTmp := ""
	if strings.TrimSpace(raw) != "" {
		if mdTmp, err = writeTemp(s.syllabusMDPath(courseID), raw); err != nil {
			s.fail(w, r, err)
			return
		}
	} else {
		os.Remove(s.syllabusMDPath(courseID))
	}
	err = s.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.UpsertSyllabus(ctx, model.Syllabus{CourseID: courseID, Filename: up.filename, Path: abs, Text: &raw}); err != nil {
			return err
		}
		return state.ResetGlossarySeed(ctx, tx, courseID)
	})
	if err == nil && mdTmp != "" {
		err = os.Rename(mdTmp, s.syllabusMDPath(courseID))
	}
	if err != nil {
		if mdTmp != "" {
			os.Remove(mdTmp)
		}
		s.failBusy(w, r, err, syllabusBusy, nil)
		return
	}
	s.kick()
	s.hub.Changed("course", courseID)
	s.respondCourse(w, r, courseID, http.StatusOK)
}

// readText reads a UTF-8 text file (BOM removed, invalid bytes replaced).
func readText(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		return strings.ToValidUTF8(string(b), "�"), nil
	}
	return string(b), nil
}

// ---- materials (SPEC §9.3; NOTES.md B, D, E) ----

func (s *Server) uploadMaterial(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	courseID := r.PathValue("id")
	c, err := s.st.GetCourse(ctx, courseID)
	if err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	if err := checkRecordIDs(courseID); err != nil { // record_dir.go
		s.fail(w, r, err)
		return
	}
	if err := s.checkSpace(r); err != nil {
		s.fail(w, r, err)
		return
	}
	m := model.Material{ID: db.NewID(), CourseID: courseID, CreatedAt: db.Now()}
	dir := model.MaterialDir(s.dataDir, courseID, m.ID)
	up, err := s.receiveUpload(w, r, dir, "source", acceptExt([]string{".pdf"}, "material.pdf_only"))
	if err != nil {
		os.RemoveAll(dir)
		s.fail(w, r, err)
		return
	}
	if lid := strings.TrimSpace(up.fields["lecture_id"]); lid != "" {
		if err := s.checkLectureInCourse(ctx, lid, courseID); err != nil {
			os.RemoveAll(dir)
			s.fail(w, r, err)
			return
		}
		m.LectureID = &lid
	}
	m.Filename = up.filename
	m.Path, _ = filepath.Abs(up.path)
	if m.LectureID == nil {
		// lecture kinds: slides named after a teaching week ("Week3-...",
		// "W03") apply to that week (Material.week_suggested) once the course
		// has a term start; others, and every one of a course without a term
		// start (no lecture has a teaching week, so week-bound slides would
		// be used by none), to the whole course. The course page offers the
		// suggested week as a one-click binding (review item 2).
		if w, ok := uploadWeek(c, m.Filename); ok {
			m.BindWeek = &w
		}
	}
	if err := s.st.CreateMaterial(ctx, m); err != nil {
		os.RemoveAll(dir)
		s.fail(w, r, err)
		return
	}
	s.materialSetsChanged(courseID)
	s.kick()
	s.hub.Changed("course", courseID)
	s.respondCourse(w, r, courseID, http.StatusCreated)
}

// uploadWeek is the teaching week a slide file uploaded to course c (not
// for one lecture) is bound to: the week its name gives
// (calendar.WeekFromFilename, = Material.week_suggested), but only when the
// course has a readable term start; ok false → the whole course.
func uploadWeek(c model.Course, filename string) (int, bool) {
	if ts, _ := courseTerm(c.TermStart, c.SkipWeeks); ts == nil {
		return 0, false
	}
	return calendar.WeekFromFilename(filename)
}

func (s *Server) checkLectureInCourse(ctx context.Context, lectureID, courseID string) error {
	l, err := s.st.GetLecture(ctx, lectureID)
	if errors.Is(err, db.ErrNotFound) || (err == nil && l.CourseID != courseID) {
		return apiErr(http.StatusBadRequest, "material.lecture_not_in_course", nil)
	}
	return err
}

func (s *Server) respondMaterial(w http.ResponseWriter, r *http.Request, id string) {
	m, err := s.st.GetMaterial(r.Context(), id)
	if err != nil {
		s.fail(w, r, notFoundAs(err, "material.not_found"))
		return
	}
	v, err := s.materialView(r.Context(), m)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// PATCH /api/materials/{id} {bind: MaterialBind} (lecture kinds: scope
// lecture, week or course) or the v1 body {lecture_id: string|null}
// (NOTES.md B; null = course-wide). Errors: material.bind_missing (neither
// key), material.bind_invalid (unknown scope, scope lecture without
// lecture_id), material.bind_week_invalid (week not in 1-60),
// material.bind_lecture_other_course (a lecture of another course or none;
// v1 body: material.lecture_not_in_course).
// Bindings never change lecture numbers, so there is no renumbering check.
func (s *Server) patchMaterial(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req map[string]json.RawMessage
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	var lid *string
	var week *int
	raw, isBind := req["bind"]
	if !isBind {
		raw, ok := req["lecture_id"]
		if !ok {
			// neither the binding nor the v1 lecture_id (review item 11)
			writeError(w, http.StatusBadRequest, "material.bind_missing", nil)
			return
		}
		if err := json.Unmarshal(raw, &lid); err != nil {
			writeError(w, http.StatusBadRequest, "material.lecture_id_type", nil)
			return
		}
		if lid != nil && strings.TrimSpace(*lid) == "" {
			lid = nil
		}
	}
	m, err := s.st.GetMaterial(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, notFoundAs(err, "material.not_found"))
		return
	}
	if isBind {
		if lid, week, err = s.parseBind(ctx, raw, m.CourseID); err != nil {
			s.fail(w, r, err)
			return
		}
	} else if lid != nil {
		if err := s.checkLectureInCourse(ctx, *lid, m.CourseID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.st.SetMaterialBinding(ctx, m.ID, lid, week); err != nil {
		if errors.Is(err, db.ErrBindingInvalid) {
			err = apiErr(http.StatusBadRequest, "material.bind_invalid", nil)
		}
		s.fail(w, r, notFoundAs(err, "material.not_found"))
		return
	}
	// The binding changes which slides a lecture's summary sees and which
	// material_terms its asr waits for (SPEC §4.3, lead freeze 1.5).
	s.materialSetsChanged(m.CourseID)
	s.kick()
	s.hub.Changed("course", m.CourseID)
	s.respondMaterial(w, r, m.ID)
}

// parseBind validates the MaterialBind of PATCH /api/materials/{id} for a
// material of course courseID: the lecture id (scope lecture) or the week
// (scope week); neither for scope course.
func (s *Server) parseBind(ctx context.Context, raw json.RawMessage, courseID string) (*string, *int, error) {
	var b MaterialBind
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, nil, apiErr(http.StatusBadRequest, "material.bind_invalid", nil)
	}
	switch b.Scope {
	case BindCourse:
		return nil, nil, nil
	case BindWeek:
		if b.Week == nil || *b.Week < 1 || *b.Week > model.MaxBindWeek {
			return nil, nil, apiErr(http.StatusBadRequest, "material.bind_week_invalid", nil)
		}
		w := *b.Week
		return nil, &w, nil
	case BindLecture:
		if b.LectureID == nil || strings.TrimSpace(*b.LectureID) == "" {
			return nil, nil, apiErr(http.StatusBadRequest, "material.bind_invalid", nil)
		}
		lid := strings.TrimSpace(*b.LectureID)
		l, err := s.st.GetLecture(ctx, lid)
		if errors.Is(err, db.ErrNotFound) || (err == nil && l.CourseID != courseID) {
			return nil, nil, apiErr(http.StatusBadRequest, "material.bind_lecture_other_course", nil)
		}
		if err != nil {
			return nil, nil, err
		}
		return &lid, nil, nil
	}
	return nil, nil, apiErr(http.StatusBadRequest, "material.bind_invalid", nil)
}

// materialSetsChanged asks the background invalidation to compare every
// lecture's material set with the one last seen (state.StaleMaterials):
// after anything that may change which slides a lecture uses - the term
// start or skipped weeks, a recording time, a binding, slides added or
// deleted (lead freeze 1.5). Lectures whose set did not change are left
// alone; the others are checked with stale_reason materials_changed.
func (s *Server) materialSetsChanged(courseID string) {
	s.inval.request(courseID, state.StaleCause{Kind: state.StaleMaterials})
}

// DELETE /api/materials/{id} (NOTES.md E; SPEC §4.6).
func (s *Server) deleteMaterial(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := s.st.GetMaterial(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, notFoundAs(err, "material.not_found"))
		return
	}
	jobs, err := s.st.ListCurrentJobs(ctx, model.ScopeMaterial, m.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !state.CanDelete(jobs) {
		writeError(w, http.StatusConflict, "material.busy", nil)
		return
	}
	if err := s.st.DeleteMaterial(ctx, m.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.removeMaterialDir(m)
	s.materialSetsChanged(m.CourseID)
	s.kick()
	s.hub.Changed("course", m.CourseID)
	w.WriteHeader(http.StatusNoContent)
}

// removeMaterialDir removes the directory of material m (the row is gone
// already, §4.6: database first). The ids come from the database, which an
// imported library supplies (AUD1 M-1), so the directory is removed only
// when it is the material's own (removableRecordDir: the course and
// material ids single path elements — no separator, not "." or ".." —, the
// directory strictly inside data_dir, and named exactly like the material,
// not a sibling "own" for the material "OWN"; the same rule the lecture
// deletion applies). Anything else is logged and left alone.
func (s *Server) removeMaterialDir(m model.Material) {
	p := model.MaterialDir(s.dataDir, m.CourseID, m.ID)
	if !s.removableRecordDir(p, m.CourseID, m.ID) {
		if _, err := os.Lstat(p); err == nil || checkRecordIDs(m.CourseID, m.ID) != nil {
			slog.Warn("refusing to remove a material directory outside its place", "material", m.ID, "path", p)
		}
		return
	}
	if err := os.RemoveAll(p); err != nil {
		s.logWarn("remove material dir", m.ID, err)
	}
}

// POST /api/materials/{id}/stages/{stage}/{action} (NOTES.md D).
func (s *Server) materialStageAction(w http.ResponseWriter, r *http.Request) {
	id, stage, action := r.PathValue("id"), r.PathValue("stage"), r.PathValue("action")
	if !validStage(model.ScopeMaterial, stage) || !validAction(stage, action) {
		writeError(w, http.StatusBadRequest, "stage.invalid", map[string]any{"stage": stage, "action": action})
		return
	}
	m, err := s.st.GetMaterial(r.Context(), id)
	if err != nil {
		s.fail(w, r, notFoundAs(err, "material.not_found"))
		return
	}
	if err := s.sc.DoAction(r.Context(), model.ScopeMaterial, id, stage, action); err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("course", m.CourseID)
	s.respondMaterial(w, r, id)
}

// POST /api/courses/{id}/stages/{glossary_seed|glossary_review|filename_rule}/{action}
// (NOTES.md C; v2 G3.1 glossary_review; the filename_rule auxiliary task
// only takes retry and cancel).
func (s *Server) courseStageAction(w http.ResponseWriter, r *http.Request) {
	id, stage, action := r.PathValue("id"), r.PathValue("stage"), r.PathValue("action")
	if !validStage(model.ScopeCourse, stage) || !validAction(stage, action) {
		writeError(w, http.StatusBadRequest, "stage.invalid", map[string]any{"stage": stage, "action": action})
		return
	}
	if _, err := s.st.GetCourse(r.Context(), id); err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	if err := s.sc.DoAction(r.Context(), model.ScopeCourse, id, stage, action); err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("course", id)
	s.respondCourse(w, r, id, http.StatusOK)
}

// ---- terms (SPEC §8, §4.6: every change bumps glossary_version and
// re-runs the invalidation check) ----

func (s *Server) listTerms(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.st.GetCourse(ctx, id); err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	ts, err := s.st.ListTerms(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]Term, 0, len(ts))
	for _, t := range ts {
		out = append(out, termView(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) termsChanged(courseID string) {
	s.inval.request(courseID, state.StaleCause{Kind: state.StaleGlossary})
	s.hub.Changed("terms", courseID)
	s.hub.Changed("course", courseID)
}

// POST /api/courses/{id}/terms {en, zh}: a manual term, locked from the
// start with the user's zh (v2 G1, G6; lead ruling 7).
func (s *Server) createTerm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	courseID := r.PathValue("id")
	var req struct {
		En string `json:"en"`
		Zh string `json:"zh"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	en := strings.Join(strings.Fields(req.En), " ")
	if en == "" {
		writeError(w, http.StatusBadRequest, "term.en_empty", nil)
		return
	}
	// the length limits of an extracted term (AUD5 low-1)
	switch {
	case ctxbuild.TermTooLong(en, ""):
		writeError(w, http.StatusBadRequest, "upload.field_too_long", map[string]any{"field": "en"})
		return
	case ctxbuild.TermTooLong("", req.Zh):
		writeError(w, http.StatusBadRequest, "upload.field_too_long", map[string]any{"field": "zh"})
		return
	}
	if _, err := s.st.GetCourse(ctx, courseID); err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return
	}
	t := model.Term{ID: db.NewID(), CourseID: courseID, En: en, EnKey: db.EnKey(en), Zh: strings.TrimSpace(req.Zh),
		Source: model.SourceManual, Enabled: true,
		Tier: model.TierEnforce, Locked: true, ZhSource: model.ZhSourceUser, CommonWord: glossary.IsCommonWord(en)}
	err := s.st.Tx(ctx, func(tx db.Store) error {
		existing, err := tx.ListTerms(ctx, courseID)
		if err != nil {
			return err
		}
		for _, e := range existing {
			if e.EnKey == t.EnKey {
				return apiErr(http.StatusConflict, "term.duplicate", map[string]any{"en": e.En})
			}
		}
		if err := tx.InsertTerm(ctx, t); err != nil {
			return err
		}
		_, err = tx.BumpGlossaryVersion(ctx, courseID)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.termsChanged(courseID)
	writeJSON(w, http.StatusCreated, termView(t))
}

// PATCH /api/terms/{id} TermPatch (v2 G1, lead ruling 8): a change of zh,
// enabled or tier locks the term (the janitor leaves it alone), a zh change
// makes zh_source user, and the version is bumped. {"locked": false} only
// unlocks: nothing else changes and the version stays (the hashes of later
// stages do not depend on the lock). Setting a value it already has is no
// change.
func (s *Server) patchTerm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req TermPatch
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	edit := req.Zh != nil || req.Enabled != nil || req.Tier != nil
	switch {
	case !edit && req.Locked == nil:
		writeError(w, http.StatusBadRequest, "request.nothing_to_change", map[string]any{"fields": "zh, enabled, tier, locked"})
		return
	case req.Locked != nil && *req.Locked:
		writeError(w, http.StatusBadRequest, "term.lock_only_unlock", nil)
		return
	case req.Locked != nil && edit:
		writeError(w, http.StatusBadRequest, "term.unlock_with_edit", nil)
		return
	case req.Tier != nil && *req.Tier != model.TierEnforce && *req.Tier != model.TierReference:
		writeError(w, http.StatusBadRequest, "term.bad_tier", nil)
		return
	case req.Zh != nil && ctxbuild.TermTooLong("", *req.Zh): // as createTerm (AUD5 low-1)
		writeError(w, http.StatusBadRequest, "upload.field_too_long", map[string]any{"field": "zh"})
		return
	}
	var t model.Term
	changed, unlocked := false, false
	err := s.st.Tx(ctx, func(tx db.Store) error {
		var err error
		if t, err = tx.GetTerm(ctx, r.PathValue("id")); err != nil {
			return notFoundAs(err, "term.not_found")
		}
		if req.Locked != nil {
			if !t.Locked {
				return nil
			}
			t.Locked, unlocked = false, true
			return tx.UpdateTerm(ctx, t)
		}
		if req.Zh != nil {
			if zh := strings.TrimSpace(*req.Zh); zh != t.Zh {
				t.Zh, t.ZhSource, changed = zh, model.ZhSourceUser, true
			}
		}
		if req.Enabled != nil && *req.Enabled != t.Enabled {
			t.Enabled, changed = *req.Enabled, true
		}
		if req.Tier != nil && *req.Tier != termView(t).Tier {
			t.Tier, changed = *req.Tier, true
		}
		if !changed {
			return nil
		}
		t.Locked = true
		if err := tx.UpdateTerm(ctx, t); err != nil {
			return err
		}
		_, err = tx.BumpGlossaryVersion(ctx, t.CourseID)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	switch {
	case changed:
		s.termsChanged(t.CourseID)
	case unlocked:
		s.hub.Changed("terms", t.CourseID)
	}
	writeJSON(w, http.StatusOK, termView(t))
}

func (s *Server) deleteTerm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var t model.Term
	err := s.st.Tx(ctx, func(tx db.Store) error {
		var err error
		if t, err = tx.GetTerm(ctx, r.PathValue("id")); err != nil {
			return notFoundAs(err, "term.not_found")
		}
		if err := tx.DeleteTerm(ctx, t.ID); err != nil {
			return err
		}
		_, err = tx.BumpGlossaryVersion(ctx, t.CourseID)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.termsChanged(t.CourseID)
	w.WriteHeader(http.StatusNoContent)
}
