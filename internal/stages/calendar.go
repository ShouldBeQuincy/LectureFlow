// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	lfmedia "lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// Calendar service of SPEC-v2 P13.3/P13.4 (lead ruling 3 of phase 2). The
// API (upload, PATCH, filename-rule endpoints), the prep stage and the
// filename_rule task call these; the computations themselves are the pure
// package internal/calendar.
//
// Times: lecture.recorded_at, course.term_start and skip_weeks are local
// wall-clock values; lecture.created_at (the upload time) is UTC and is
// converted to the server's local zone (time.Local) before it is compared.

// KVCreationTimePrefix: media_creation_time:{lecture_id} → the media's
// creation_time (RFC 3339) as passed to ResolveRecordedAt by prep. Kept so
// that re-resolving later (a rule applied or undone) still knows it, and so
// that the filename_rule request can show it (P13.3.2 input).
const KVCreationTimePrefix = "media_creation_time:"

// ErrNoFilenameUndo: UndoFilenameRule found nothing to undo. It wraps
// db.ErrNotFound (the API answers 404).
var ErrNoFilenameUndo = fmt.Errorf("no automatic filename rule to undo: %w", db.ErrNotFound)

// FilenameUndo is kv filename_rule_undo:{course_id}: the state before the
// filename_rule task applied a rule automatically (P13.3.3 首次例外).
// UndoFilenameRule restores it; "" = nothing to undo.
type FilenameUndo struct {
	Rule      *string               `json:"rule"` // course.filename_rule before; null = none
	Lectures  []FilenameUndoLecture `json:"lectures"`
	CreatedAt string                `json:"created_at"`
}

// FilenameUndoLecture is one lecture's recorded_at, titles and kind before.
type FilenameUndoLecture struct {
	ID               string  `json:"id"`
	RecordedAt       *string `json:"recorded_at"`
	RecordedAtSource string  `json:"recorded_at_source"`
	Title            string  `json:"title"`
	TitleZh          *string `json:"title_zh"`
	TitleSource      string  `json:"title_source"`
	// Kind and KindSource are lecture.kind ("" = NULL) and kind_source
	// (lecture kinds, K2). KindSource "" = a record written before lecture
	// kinds: undo leaves the kind alone.
	Kind       string `json:"kind,omitempty"`
	KindSource string `json:"kind_source,omitempty"`
}

// ResolveRecordedAt is P13.3.4 for one lecture: user > filename (the
// course's rule applied to original_filename) > creationTime (the media's
// creation_time; not used when it is on the upload day or later) > none.
// creationTime nil means "not known now": the value stored by an earlier
// call is used. A lecture whose source is user keeps its recorded_at. When
// the rule gives a title and title_source is filename, the display title
// becomes that title (P13.3.3); when it gives a kind (lecture kinds: the
// kind group through kind_map), the lecture's kind becomes it unless the
// user set one (resolveOne). The upload time is lecture.created_at. When
// anything changes, the course's seq is refreshed (RefreshCourseSeq) in the
// same transaction; when recorded_at changes, the material sets are
// checked afterwards (state.StaleMaterials: the lecture's teaching week,
// and so its week-bound slides, may have changed).
func ResolveRecordedAt(ctx context.Context, d Deps, lectureID string, creationTime *time.Time) error {
	var (
		ch       resolveChange
		courseID string
	)
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		ch = resolveChange{}
		l, err := tx.GetLecture(ctx, lectureID)
		if err != nil {
			return err
		}
		courseID = l.CourseID
		c, err := tx.GetCourse(ctx, l.CourseID)
		if err != nil {
			return err
		}
		kinds, err := listKinds(ctx, tx)
		if err != nil {
			return err
		}
		if creationTime != nil && !creationTime.IsZero() {
			if err := tx.SetKV(ctx, KVCreationTimePrefix+lectureID, creationTime.Format(time.RFC3339)); err != nil {
				return err
			}
		}
		ct, err := storedCreationTime(ctx, tx, lectureID)
		if err != nil {
			return err
		}
		if ch, err = resolveOne(ctx, tx, courseRule(c), l, ct, kinds); err != nil {
			return err
		}
		if ch.any() {
			_, err = refreshSeq(ctx, tx, l.CourseID)
		}
		return err
	})
	if err == nil && ch.any() {
		d.events().Changed("lecture", lectureID)
		d.events().Changed("course", courseID)
	}
	if err == nil && ch.date {
		d.invalidate(ctx, courseID, state.StaleCause{Kind: state.StaleMaterials})
	}
	return err
}

// ApplyFilenameRule saves rule as the course's filename_rule (P13.3.3 应用 /
// 保存) and re-resolves every lecture: recorded_at of those whose source is
// not user, the display title of those whose title_source is filename
// when the rule captures a title, and the kind of those whose kind was not
// set by the user (lecture kinds). seq is refreshed; a pending proposal and
// the undo record of an earlier automatic apply are cleared (the user's
// decision supersedes both). A rule that does not compile is rejected.
// Empty CreatedAt is filled with now, empty Source with user; SamplesTotal 0
// is filled with the course's file names. When a recorded_at changed, the
// material sets are checked afterwards (state.StaleMaterials).
func ApplyFilenameRule(ctx context.Context, d Deps, courseID string, rule calendar.FilenameRule) error {
	var (
		changedIDs []string
		dated      bool
	)
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		var err error
		changedIDs, dated, err = applyRule(ctx, tx, d, courseID, rule)
		if err != nil {
			return err
		}
		if err := tx.SetKV(ctx, model.KVFilenameProposal+courseID, ""); err != nil {
			return err
		}
		return tx.SetKV(ctx, model.KVFilenameUndo+courseID, "")
	})
	if err != nil {
		return err
	}
	for _, id := range changedIDs {
		d.events().Changed("lecture", id)
	}
	d.events().Changed("course", courseID)
	if dated {
		d.invalidate(ctx, courseID, state.StaleCause{Kind: state.StaleMaterials})
	}
	return nil
}

// UndoFilenameRule restores the state saved by an automatic apply
// (kv filename_rule_undo, P13.3.3 “撤销”): the previous rule, and recorded_at,
// titles and kinds of lectures that were not changed by the user since
// (recorded_at source not user, title_source still filename, kind_source
// still filename). ErrNoFilenameUndo when there is nothing to undo. When a
// recorded_at changed, the material sets are checked afterwards
// (state.StaleMaterials).
func UndoFilenameRule(ctx context.Context, d Deps, courseID string) error {
	var (
		changedIDs []string
		dated      bool
	)
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		changedIDs, dated = nil, false
		raw, ok, err := tx.GetKV(ctx, model.KVFilenameUndo+courseID)
		if err != nil {
			return err
		}
		if !ok || strings.TrimSpace(raw) == "" {
			return ErrNoFilenameUndo
		}
		var u FilenameUndo
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			return fmt.Errorf("filename rule undo record: %w", err)
		}
		if err := tx.SetCourseFilenameRule(ctx, courseID, u.Rule); err != nil {
			return err
		}
		var cat *kindCatalog // read when a kind falls back
		for _, p := range u.Lectures {
			l, err := tx.GetLecture(ctx, p.ID)
			if errors.Is(err, db.ErrNotFound) || (err == nil && l.CourseID != courseID) {
				continue // deleted since
			}
			if err != nil {
				return err
			}
			changed := false
			if l.RecordedAtSource != model.RecordedAtUser &&
				(!sameStrPtr(l.RecordedAt, p.RecordedAt) || recordedSource(l.RecordedAtSource) != recordedSource(p.RecordedAtSource)) {
				if err := tx.SetLectureRecordedAt(ctx, l.ID, p.RecordedAt, recordedSource(p.RecordedAtSource)); err != nil {
					return err
				}
				changed, dated = true, true
			}
			// cleaned like every stored title (lfmedia.CleanTitle: one line,
			// at most 200 characters), so it compares with what was stored
			if t := lfmedia.CleanTitle(p.Title); titleSource(l.TitleSource) == model.TitleSourceFilename && l.Title != t {
				if err := tx.SetLectureTitles(ctx, l.ID, t, l.TitleZh, model.TitleSourceFilename); err != nil {
					return err
				}
				changed = true
			}
			// the kind the applied rule gave (kind_source still filename)
			// goes back to what it was; a record from before lecture kinds
			// (no kind_source) leaves it alone. What was below the file
			// name (an llm kind or none) is the model's latest judgement
			// when one is remembered (state.FallbackKind: the title task
			// may have judged again while the rule held the lecture).
			if p.KindSource != "" && kindSource(l.KindSource) == model.KindSourceFilename {
				kind, src := p.Kind, kindSource(p.KindSource)
				if src != model.KindSourceFilename && src != model.KindSourceUser {
					if cat == nil {
						c, err := listKinds(ctx, tx)
						if err != nil {
							return err
						}
						cat = &c
					}
					k, s, err := state.FallbackKind(ctx, tx, l, cat.kinds)
					if err != nil {
						return err
					}
					if s == model.KindSourceLLM {
						kind, src = k, s
					}
				}
				if l.Kind != kind || src != model.KindSourceFilename {
					if err := state.WriteLectureKind(ctx, tx, l, kind, src); err != nil {
						return err
					}
					changed = true
				}
			}
			if changed {
				changedIDs = append(changedIDs, l.ID)
			}
		}
		if _, err := refreshSeq(ctx, tx, courseID); err != nil {
			return err
		}
		return tx.SetKV(ctx, model.KVFilenameUndo+courseID, "")
	})
	if err != nil {
		return err
	}
	for _, id := range changedIDs {
		d.events().Changed("lecture", id)
	}
	d.events().Changed("course", courseID)
	if dated {
		d.invalidate(ctx, courseID, state.StaleCause{Kind: state.StaleMaterials})
	}
	return nil
}

// RefreshCourseSeq rewrites lecture.seq of the course in calendar.Order
// (position + 1, P13.2): call it after any change of recorded_at.
func RefreshCourseSeq(ctx context.Context, d Deps, courseID string) error {
	var changed bool
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		var err error
		changed, err = refreshSeq(ctx, tx, courseID)
		return err
	})
	if err == nil && changed {
		d.events().Changed("course", courseID)
	}
	return err
}

// CourseLabels returns the calendar number of every lecture of the course,
// drafts included (state.NumberCourse: numbered by teaching week and
// effective kind with the kind catalog and the course's numbering mode,
// lecture kinds; Numbered.ID filled), keyed by lecture id, for the API
// views, the citation JSON (calendar.CiteLabels over all of them) and the
// renumbering check of P13.6.
func CourseLabels(ctx context.Context, d Deps, courseID string) (map[string]calendar.Numbered, error) {
	var out map[string]calendar.Numbered
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		var err error
		out, err = courseLabelsTx(ctx, tx, courseID)
		return err
	})
	return out, err
}

// ---------- helpers ----------

// courseLabelsTx is CourseLabels inside tx.
func courseLabelsTx(ctx context.Context, tx db.Store, courseID string) (map[string]calendar.Numbered, error) {
	c, err := tx.GetCourse(ctx, courseID)
	if err != nil {
		return nil, err
	}
	ls, err := tx.ListLectures(ctx, courseID)
	if err != nil {
		return nil, err
	}
	kinds, err := tx.ListLectureKinds(ctx)
	if err != nil {
		return nil, err
	}
	return courseLabels(c, ls, kinds), nil
}

func courseLabels(c model.Course, ls []model.Lecture, kinds []model.LectureKind) map[string]calendar.Numbered {
	nums := state.NumberCourse(c, ls, kinds)
	out := make(map[string]calendar.Numbered, len(nums))
	for _, n := range nums {
		out[n.ID] = n
	}
	return out
}

// kindCatalog is the lecture kind catalog (ListLectureKinds) as the
// pipeline checks kinds against it: the kind_map of a file-name rule, the
// kind of a title answer.
type kindCatalog struct {
	kinds []model.LectureKind
	ids   map[string]bool
}

func listKinds(ctx context.Context, tx db.Store) (kindCatalog, error) {
	ks, err := tx.ListLectureKinds(ctx)
	if err != nil {
		return kindCatalog{}, err
	}
	return newKindCatalog(ks), nil
}

func newKindCatalog(ks []model.LectureKind) kindCatalog {
	c := kindCatalog{kinds: ks, ids: make(map[string]bool, len(ks))}
	for _, k := range ks {
		c.ids[k.ID] = true
	}
	return c
}

// resolve returns the catalog id equal to id (trimmed), else the one equal
// to it ignoring case ("Tutorial"); "" when there is none.
func (c kindCatalog) resolve(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if c.ids[id] {
		return id
	}
	for _, k := range c.kinds {
		if strings.EqualFold(k.ID, id) {
			return k.ID
		}
	}
	return ""
}

// kindSource normalises lecture.kind_source ("" = none).
func kindSource(s string) string {
	if s == "" {
		return model.KindSourceNone
	}
	return s
}

// courseCalendar parses term_start and skip_weeks (local dates); unparsable
// values are ignored.
func courseCalendar(c model.Course) (*time.Time, []time.Time) {
	var termStart *time.Time
	if c.TermStart != nil && strings.TrimSpace(*c.TermStart) != "" {
		if t, err := calendar.ParseDate(*c.TermStart, time.Local); err == nil {
			termStart = &t
		}
	}
	var skips []time.Time
	for _, s := range c.SkipWeeks {
		if t, err := calendar.ParseDate(s, time.Local); err == nil {
			skips = append(skips, t)
		}
	}
	return termStart, skips
}

// lectureTimes converts lectures (in ListLectures order: created_at) for
// calendar.Order (which does not depend on kinds; numbering uses
// state.NumberCourse).
func lectureTimes(ls []model.Lecture) []calendar.LectureTime {
	out := make([]calendar.LectureTime, len(ls))
	for i, l := range ls {
		out[i] = calendar.LectureTime{ID: l.ID, RecordedAt: recordedAt(l), OriginalFilename: l.OriginalFilename}
	}
	return out
}

// recordedAt parses lecture.recorded_at (local); nil when unset or unparsable.
func recordedAt(l model.Lecture) *time.Time {
	if l.RecordedAt == nil || strings.TrimSpace(*l.RecordedAt) == "" {
		return nil
	}
	t, err := calendar.ParseRecordedAt(*l.RecordedAt, time.Local)
	if err != nil {
		return nil
	}
	return &t
}

// courseRule decodes course.filename_rule; nil when none or invalid JSON.
func courseRule(c model.Course) *calendar.FilenameRule {
	if c.FilenameRule == nil || strings.TrimSpace(*c.FilenameRule) == "" {
		return nil
	}
	var r calendar.FilenameRule
	if err := json.Unmarshal([]byte(*c.FilenameRule), &r); err != nil || r.Regex == "" {
		slog.Warn("course filename_rule is not valid", "course", c.ID, "err", err)
		return nil
	}
	return &r
}

// storedCreationTime reads kv media_creation_time:{lecture}; nil when absent.
func storedCreationTime(ctx context.Context, st db.Store, lectureID string) (*time.Time, error) {
	v, ok, err := st.GetKV(ctx, KVCreationTimePrefix+lectureID)
	if err != nil || !ok || strings.TrimSpace(v) == "" {
		return nil, err
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return nil, nil
	}
	return &t, nil
}

// resolveChange is what resolveOne changed of a lecture.
type resolveChange struct {
	date  bool // recorded_at or its source
	title bool
	kind  bool // kind or kind_source
}

func (c resolveChange) any() bool { return c.date || c.title || c.kind }

// resolveOne applies P13.3.4 (and the P13.3.3 title rule) to one lecture
// inside tx; ct is the media creation_time (nil = unknown). It reports what
// changed. Without a stored creation_time, a lecture already dated from
// metadata keeps that date as its metadata candidate (lectures resolved
// before the value was stored).
//
// Lecture kinds (lead ruling 1.2: user > filename > llm > none): the kind
// the rule gives (Parsed.Kind: its kind group through kind_map; ignored
// when not in the catalog kinds) becomes the lecture's kind with source
// filename unless calendar.KindOverrides refuses (a user kind). A kind of
// source filename that the course's rule no longer gives (another rule, no
// rule, an unmapped code) falls back to the model's judgement (source llm:
// the title task's latest kind, remembered while the file name held the
// lecture; state.FallbackKind), else to none.
func resolveOne(ctx context.Context, tx db.Store, rule *calendar.FilenameRule, l model.Lecture, ct *time.Time, kinds kindCatalog) (resolveChange, error) {
	var ch resolveChange
	up := uploadedAtLocal(l)
	var (
		fromName *time.Time
		title    string
		fileKind string
	)
	if rule != nil && l.OriginalFilename != "" {
		p := calendar.Parse(*rule, calendar.Sample{Filename: l.OriginalFilename, UploadedAt: up, CreationTime: ct})
		if p.Matched {
			// the title the rule captures is stored like every title
			// (lfmedia.CleanTitle: one line, at most 200 characters)
			fromName, title, fileKind = p.RecordedAt, lfmedia.CleanTitle(p.Title), kinds.resolve(p.Kind)
		}
	}
	if ct == nil && recordedSource(l.RecordedAtSource) == model.RecordedAtMetadata {
		ct = recordedAt(l)
	}
	if l.RecordedAtSource != model.RecordedAtUser {
		at, src := calendar.ResolveRecordedAt(nil, fromName, ct, up)
		var s *string
		if at != nil {
			v := calendar.FormatRecordedAt(*at)
			s = &v
		}
		if !sameStrPtr(s, l.RecordedAt) || src != recordedSource(l.RecordedAtSource) {
			if err := tx.SetLectureRecordedAt(ctx, l.ID, s, src); err != nil {
				return ch, err
			}
			ch.date = true
		}
	}
	if title != "" && titleSource(l.TitleSource) == model.TitleSourceFilename && title != l.Title {
		if err := tx.SetLectureTitles(ctx, l.ID, title, l.TitleZh, model.TitleSourceFilename); err != nil {
			return ch, err
		}
		ch.title = true
	}
	src := kindSource(l.KindSource)
	switch {
	case fileKind != "":
		if calendar.KindOverrides(model.KindSourceFilename, src) && (l.Kind != fileKind || src != model.KindSourceFilename) {
			// an llm kind replaced here is remembered (state.WriteLectureKind)
			if err := state.WriteLectureKind(ctx, tx, l, fileKind, model.KindSourceFilename); err != nil {
				return ch, err
			}
			ch.kind = true
		}
	case src == model.KindSourceFilename:
		kind, ksrc, err := state.FallbackKind(ctx, tx, l, kinds.kinds)
		if err != nil {
			return ch, err
		}
		if err := state.WriteLectureKind(ctx, tx, l, kind, ksrc); err != nil {
			return ch, err
		}
		ch.kind = true
	}
	return ch, nil
}

// uploadedAtLocal is lecture.created_at (the upload time) in local time (a
// lecture row always has one; the zero time only for a corrupt row).
func uploadedAtLocal(l model.Lecture) time.Time {
	if t, err := time.Parse(time.RFC3339, l.CreatedAt); err == nil {
		return t.In(time.Local)
	}
	return time.Time{}.In(time.Local)
}

// applyRule saves rule and re-resolves the course's lectures inside tx; it
// returns the ids of the lectures that changed and whether a recorded_at
// changed (the material sets must then be checked: state.StaleMaterials).
func applyRule(ctx context.Context, tx db.Store, d Deps, courseID string, rule calendar.FilenameRule) (changed []string, dated bool, err error) {
	if _, err := calendar.Compile(rule); err != nil {
		return nil, false, model.Errorf("filename_rule.invalid", nil, err)
	}
	c, err := tx.GetCourse(ctx, courseID)
	if err != nil {
		return nil, false, err
	}
	ls, err := tx.ListLectures(ctx, courseID)
	if err != nil {
		return nil, false, err
	}
	kinds, err := listKinds(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if rule.CreatedAt == "" {
		rule.CreatedAt = d.now().UTC().Format(time.RFC3339)
	}
	if rule.Source == "" {
		rule.Source = calendar.RuleSourceUser
	}
	if rule.SamplesTotal == 0 {
		var samples []calendar.Sample
		for _, l := range ls {
			if l.OriginalFilename != "" {
				samples = append(samples, calendar.Sample{Filename: l.OriginalFilename, UploadedAt: uploadedAtLocal(l)})
			}
		}
		_, matched, _, _ := calendar.ValidateRule(rule, samples, nil)
		rule.SamplesMatched, rule.SamplesTotal = matched, len(samples)
	}
	b, err := json.Marshal(rule)
	if err != nil {
		return nil, false, err
	}
	raw := string(b)
	if err := tx.SetCourseFilenameRule(ctx, c.ID, &raw); err != nil {
		return nil, false, err
	}
	for _, l := range ls {
		ct, err := storedCreationTime(ctx, tx, l.ID)
		if err != nil {
			return nil, false, err
		}
		ch, err := resolveOne(ctx, tx, &rule, l, ct, kinds)
		if err != nil {
			return nil, false, err
		}
		if ch.any() {
			changed = append(changed, l.ID)
		}
		dated = dated || ch.date
	}
	if _, err := refreshSeq(ctx, tx, courseID); err != nil {
		return nil, false, err
	}
	return changed, dated, nil
}

// refreshSeq sets seq = position in calendar.Order + 1 for every lecture of
// the course whose seq differs; it reports whether any changed.
func refreshSeq(ctx context.Context, tx db.Store, courseID string) (bool, error) {
	ls, err := tx.ListLectures(ctx, courseID)
	if err != nil {
		return false, err
	}
	changed := false
	for pos, i := range calendar.Order(lectureTimes(ls)) {
		if ls[i].Seq == pos+1 {
			continue
		}
		if err := tx.SetLectureSeq(ctx, ls[i].ID, pos+1); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// recordedSource normalises recorded_at_source ("" = none).
func recordedSource(s string) string {
	if s == "" {
		return model.RecordedAtNone
	}
	return s
}

// titleSource normalises title_source ("" = filename).
func titleSource(s string) string {
	if s == "" {
		return model.TitleSourceFilename
	}
	return s
}

func sameStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
