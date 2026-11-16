// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"lectureflow/internal/model"
)

// NewID returns a new ULID string (SPEC §3: all ids are ULIDs). IDs created
// by one process are monotonic, so ORDER BY id follows creation order.
func NewID() string { return ulid.Make().String() }

// Now returns the current time as UTC RFC3339 (SPEC §3).
func Now() string { return time.Now().UTC().Format(time.RFC3339) }

// FormatTime formats t as UTC RFC3339.
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// EnKey normalises a term: lower(trim(en)) with internal whitespace runs
// collapsed to one space (DDL comment on term.en_key).
func EnKey(en string) string {
	return strings.ToLower(strings.Join(strings.Fields(en), " "))
}

// querier is the subset shared by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sqlStore implements Store on top of database/sql. When tx is non-nil the
// store is bound to that transaction and q == tx.
type sqlStore struct {
	db *sql.DB
	tx *sql.Tx
	q  querier
}

// New wraps an opened (and migrated) database.
func New(sqlDB *sql.DB) Store {
	return &sqlStore{db: sqlDB, q: sqlDB}
}

// Open opens the database at path (OpenDB: SPEC §3.3 pragmas + migrations)
// and returns a Store. Close the Store to close the database.
func Open(path string) (Store, error) {
	d, err := OpenDB(path)
	if err != nil {
		return nil, err
	}
	return New(d), nil
}

func (s *sqlStore) Tx(ctx context.Context, fn func(tx Store) error) (err error) {
	if s.tx != nil {
		return fn(s) // nested: reuse the outer transaction
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	inner := &sqlStore{db: s.db, tx: tx, q: tx}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(inner); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *sqlStore) Close() error {
	if s.tx != nil {
		return errors.New("db: Close on a transaction-bound store")
	}
	return s.db.Close()
}

// ---------- helpers ----------

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func strPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func affectedOrNotFound(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// chunks splits idxs so IN (...) lists stay well below SQLite's variable limit.
func chunks(idxs []int, size int) [][]int {
	var out [][]int
	for len(idxs) > size {
		out = append(out, idxs[:size])
		idxs = idxs[size:]
	}
	if len(idxs) > 0 {
		out = append(out, idxs)
	}
	return out
}

func intsAny(prefix []any, idxs []int) []any {
	args := append([]any{}, prefix...)
	for _, i := range idxs {
		args = append(args, i)
	}
	return args
}

// ---------- course ----------

func (s *sqlStore) CreateCourse(ctx context.Context, c model.Course) error {
	if c.ID == "" {
		return errors.New("db: course id required")
	}
	if c.CreatedAt == "" {
		c.CreatedAt = Now()
	}
	skip, err := skipWeeksJSON(c.SkipWeeks)
	if err != nil {
		return err
	}
	_, err = s.q.ExecContext(ctx, `INSERT INTO course (id, name, glossary_version, created_at, code, term_start, skip_weeks, filename_rule, numbering)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Name, c.GlossaryVersion, c.CreatedAt, nullStr(c.Code), nullStr(c.TermStart), skip, nullStr(c.FilenameRule),
		numberingOrAuto(c.Numbering))
	return err
}

const courseCols = `id, name, glossary_version, created_at, code, term_start, skip_weeks, filename_rule, numbering`

func scanCourse(sc interface{ Scan(...any) error }) (model.Course, error) {
	var c model.Course
	var code, termStart, rule sql.NullString
	var skip string
	err := sc.Scan(&c.ID, &c.Name, &c.GlossaryVersion, &c.CreatedAt, &code, &termStart, &skip, &rule, &c.Numbering)
	c.Code, c.TermStart, c.FilenameRule = strPtr(code), strPtr(termStart), strPtr(rule)
	c.SkipWeeks = []string{}
	if err == nil && skip != "" {
		if jerr := json.Unmarshal([]byte(skip), &c.SkipWeeks); jerr != nil {
			return c, fmt.Errorf("course %s: bad skip_weeks: %w", c.ID, jerr)
		}
	}
	return c, err
}

// skipWeeksJSON encodes course.skip_weeks (nil means []).
func skipWeeksJSON(w []string) (string, error) {
	if w == nil {
		w = []string{}
	}
	b, err := json.Marshal(w)
	return string(b), err
}

func (s *sqlStore) ListCourses(ctx context.Context) ([]model.Course, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+courseCols+` FROM course ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Course
	for rows.Next() {
		c, err := scanCourse(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetCourse(ctx context.Context, id string) (model.Course, error) {
	c, err := scanCourse(s.q.QueryRowContext(ctx, `SELECT `+courseCols+` FROM course WHERE id = ?`, id))
	return c, notFound(err)
}

func (s *sqlStore) BumpGlossaryVersion(ctx context.Context, courseID string) (int, error) {
	var v int
	err := s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		if err := affectedOrNotFound(t.q.ExecContext(ctx,
			`UPDATE course SET glossary_version = glossary_version + 1 WHERE id = ?`, courseID)); err != nil {
			return err
		}
		return t.q.QueryRowContext(ctx, `SELECT glossary_version FROM course WHERE id = ?`, courseID).Scan(&v)
	})
	return v, err
}

// ---------- syllabus ----------

func (s *sqlStore) UpsertSyllabus(ctx context.Context, sy model.Syllabus) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO syllabus (course_id, filename, path, text) VALUES (?,?,?,?)
		ON CONFLICT(course_id) DO UPDATE SET filename = excluded.filename, path = excluded.path, text = excluded.text`,
		sy.CourseID, sy.Filename, sy.Path, nullStr(sy.Text))
	return err
}

func (s *sqlStore) GetSyllabus(ctx context.Context, courseID string) (model.Syllabus, error) {
	var sy model.Syllabus
	var text sql.NullString
	err := s.q.QueryRowContext(ctx, `SELECT course_id, filename, path, text FROM syllabus WHERE course_id = ?`, courseID).
		Scan(&sy.CourseID, &sy.Filename, &sy.Path, &text)
	if err != nil {
		return sy, notFound(err)
	}
	sy.Text = strPtr(text)
	return sy, nil
}

func (s *sqlStore) SetSyllabusText(ctx context.Context, courseID, text string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE syllabus SET text = ? WHERE course_id = ?`, text, courseID))
}

// ---------- lecture ----------

func (s *sqlStore) CreateLecture(ctx context.Context, l model.Lecture) error {
	if l.ID == "" {
		return errors.New("db: lecture id required")
	}
	if l.CreatedAt == "" {
		l.CreatedAt = Now()
	}
	return s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		if l.Seq == 0 {
			n, err := t.NextLectureSeq(ctx, l.CourseID)
			if err != nil {
				return err
			}
			l.Seq = n
		}
		lectureV2(&l)
		if _, err := t.q.ExecContext(ctx, `INSERT INTO lecture (id, course_id, title, media_path, created_at, launched_at, seq,
			recorded_at, recorded_at_source, title_zh, title_source, blurb_en, blurb_zh, original_filename, kind, kind_source)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			l.ID, l.CourseID, l.Title, l.MediaPath, l.CreatedAt, nullStr(l.LaunchedAt), l.Seq,
			nullStr(l.RecordedAt), l.RecordedAtSource, nullStr(l.TitleZh), l.TitleSource, nullStr(l.BlurbEn), nullStr(l.BlurbZh),
			l.OriginalFilename, nullIfEmpty(l.Kind), l.KindSource); err != nil {
			return err
		}
		for _, stage := range model.LectureStages {
			if err := t.InsertJob(ctx, model.Job{ID: NewID(), Scope: model.ScopeLecture, ScopeID: l.ID,
				Stage: stage, Status: model.StatusPending, IsCurrent: true, CreatedAt: l.CreatedAt}); err != nil {
				return err
			}
		}
		return nil
	})
}

const lectureCols = `l.id, l.course_id, l.title, l.media_path, l.created_at, l.launched_at, l.seq,
	l.recorded_at, l.recorded_at_source, l.title_zh, l.title_source, l.blurb_en, l.blurb_zh, l.original_filename, l.kind, l.kind_source`

// lectureSelect reads lecture rows (alias l) with their week override
// (lecture_week_override, migration 011; alias o, NULL when none).
const lectureSelect = `SELECT ` + lectureCols + `, o.week, o.kind, o.created_at
	FROM lecture l LEFT JOIN lecture_week_override o ON o.lecture_id = l.id`

// lectureSelectPre011 is lectureSelect on a database below migration 011,
// which has no lecture_week_override: the upgrade tests read lectures with
// the store before migrating further (a store opened by Open is always
// migrated). queryLectures falls back to it.
const lectureSelectPre011 = `SELECT ` + lectureCols + `, NULL, NULL, NULL FROM lecture l`

// noOverrideTable reports the error of lectureSelect on a database below
// migration 011.
func noOverrideTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: lecture_week_override")
}

func scanLecture(sc interface{ Scan(...any) error }) (model.Lecture, error) {
	var l model.Lecture
	var launched, recorded, titleZh, blurbEn, blurbZh, kind sql.NullString
	var seq sql.NullInt64
	var ovWeek sql.NullInt64
	var ovKind, ovAt sql.NullString
	err := sc.Scan(&l.ID, &l.CourseID, &l.Title, &l.MediaPath, &l.CreatedAt, &launched, &seq,
		&recorded, &l.RecordedAtSource, &titleZh, &l.TitleSource, &blurbEn, &blurbZh, &l.OriginalFilename, &kind, &l.KindSource,
		&ovWeek, &ovKind, &ovAt)
	l.Kind = kind.String
	l.LaunchedAt = strPtr(launched)
	l.Seq = int(seq.Int64)
	l.RecordedAt, l.TitleZh, l.BlurbEn, l.BlurbZh = strPtr(recorded), strPtr(titleZh), strPtr(blurbEn), strPtr(blurbZh)
	if ovWeek.Valid && ovKind.Valid {
		l.WeekOverride = &model.WeekOverride{Week: int(ovWeek.Int64), Kind: ovKind.String, CreatedAt: ovAt.String}
	}
	return l, err
}

// lectureV2 fills the v2 defaults of an unset lecture (P13.2; kind_source
// of migration 007).
func lectureV2(l *model.Lecture) {
	if l.RecordedAtSource == "" {
		l.RecordedAtSource = model.RecordedAtNone
	}
	if l.TitleSource == "" {
		l.TitleSource = model.TitleSourceFilename
	}
	if l.KindSource == "" {
		l.KindSource = model.KindSourceNone
	}
}

func (s *sqlStore) ListLectures(ctx context.Context, courseID string) ([]model.Lecture, error) {
	const where = ` WHERE l.course_id = ? ORDER BY l.created_at, l.id`
	rows, err := s.q.QueryContext(ctx, lectureSelect+where, courseID)
	if noOverrideTable(err) {
		rows, err = s.q.QueryContext(ctx, lectureSelectPre011+where, courseID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Lecture
	for rows.Next() {
		l, err := scanLecture(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetLecture(ctx context.Context, id string) (model.Lecture, error) {
	l, err := scanLecture(s.q.QueryRowContext(ctx, lectureSelect+` WHERE l.id = ?`, id))
	if noOverrideTable(err) {
		l, err = scanLecture(s.q.QueryRowContext(ctx, lectureSelectPre011+` WHERE l.id = ?`, id))
	}
	return l, notFound(err)
}

func (s *sqlStore) SetLectureTitle(ctx context.Context, id, title string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE lecture SET title = ? WHERE id = ?`, title, id))
}

func (s *sqlStore) SetLectureMedia(ctx context.Context, id, mediaPath string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE lecture SET media_path = ? WHERE id = ?`, mediaPath, id))
}

// DeleteLecture deletes the lecture row (sentences, patches, summary and bound
// materials cascade) together with the jobs of the lecture and of the
// materials bound to it.
func (s *sqlStore) DeleteLecture(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		if _, err := t.q.ExecContext(ctx, `DELETE FROM job WHERE scope = 'material' AND scope_id IN
			(SELECT id FROM material WHERE lecture_id = ?)`, id); err != nil {
			return err
		}
		if _, err := t.q.ExecContext(ctx, `DELETE FROM job WHERE scope = 'lecture' AND scope_id = ?`, id); err != nil {
			return err
		}
		return affectedOrNotFound(t.q.ExecContext(ctx, `DELETE FROM lecture WHERE id = ?`, id))
	})
}

// ---------- material ----------

func encodePages(p []string) (any, error) {
	if p == nil {
		return nil, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (s *sqlStore) CreateMaterial(ctx context.Context, m model.Material) error {
	if m.ID == "" {
		return errors.New("db: material id required")
	}
	if m.CreatedAt == "" {
		m.CreatedAt = Now()
	}
	pages, err := encodePages(m.PagesMD)
	if err != nil {
		return err
	}
	if err := checkBinding(m.LectureID, m.BindWeek); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		if _, err := t.q.ExecContext(ctx, `INSERT INTO material (id, course_id, lecture_id, filename, path, pages_md, created_at, bind_week)
			VALUES (?,?,?,?,?,?,?,?)`, m.ID, m.CourseID, nullStr(m.LectureID), m.Filename, m.Path, pages, m.CreatedAt,
			nullInt(m.BindWeek)); err != nil {
			return err
		}
		for _, stage := range model.MaterialStages {
			if err := t.InsertJob(ctx, model.Job{ID: NewID(), Scope: model.ScopeMaterial, ScopeID: m.ID,
				Stage: stage, Status: model.StatusPending, IsCurrent: true, CreatedAt: m.CreatedAt}); err != nil {
				return err
			}
		}
		return nil
	})
}

const materialCols = `id, course_id, lecture_id, filename, path, pages_md, created_at, bind_week`

func scanMaterial(sc interface{ Scan(...any) error }) (model.Material, error) {
	var m model.Material
	var lec, pages sql.NullString
	var week sql.NullInt64
	if err := sc.Scan(&m.ID, &m.CourseID, &lec, &m.Filename, &m.Path, &pages, &m.CreatedAt, &week); err != nil {
		return m, err
	}
	m.LectureID = strPtr(lec)
	m.BindWeek = intPtr(week)
	if pages.Valid {
		if err := json.Unmarshal([]byte(pages.String), &m.PagesMD); err != nil {
			return m, fmt.Errorf("material %s: bad pages_md: %w", m.ID, err)
		}
		if m.PagesMD == nil {
			m.PagesMD = []string{}
		}
	}
	return m, nil
}

func (s *sqlStore) ListMaterialBindings(ctx context.Context, courseID string) (map[string]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, lecture_id FROM material WHERE course_id = ? AND lecture_id IS NOT NULL`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, lid string
		if err := rows.Scan(&id, &lid); err != nil {
			return nil, err
		}
		out[id] = lid
	}
	return out, rows.Err()
}

func (s *sqlStore) ListMaterials(ctx context.Context, courseID string) ([]model.Material, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+materialCols+` FROM material WHERE course_id = ? ORDER BY created_at, id`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Material
	for rows.Next() {
		m, err := scanMaterial(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetMaterial(ctx context.Context, id string) (model.Material, error) {
	m, err := scanMaterial(s.q.QueryRowContext(ctx, `SELECT `+materialCols+` FROM material WHERE id = ?`, id))
	return m, notFound(err)
}

func (s *sqlStore) SetMaterialPages(ctx context.Context, id string, pages []string) error {
	p, err := encodePages(pages)
	if err != nil {
		return err
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE material SET pages_md = ? WHERE id = ?`, p, id))
}

// SetMaterialLecture is the v1 binding: to a lecture, or (nil) course-wide.
// Either way the week binding of migration 007 is cleared (a material is
// bound to at most one of a lecture and a week): it is
// SetMaterialBinding(id, lectureID, nil).
func (s *sqlStore) SetMaterialLecture(ctx context.Context, id string, lectureID *string) error {
	return s.SetMaterialBinding(ctx, id, lectureID, nil)
}

func (s *sqlStore) DeleteMaterial(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		if _, err := t.q.ExecContext(ctx, `DELETE FROM job WHERE scope = 'material' AND scope_id = ?`, id); err != nil {
			return err
		}
		return affectedOrNotFound(t.q.ExecContext(ctx, `DELETE FROM material WHERE id = ?`, id))
	})
}

// ---------- term ----------

const termCols = `id, course_id, en, en_key, zh, source, enabled, freq, tier, locked, common_word, janitor_note, zh_source`

func scanTerm(sc interface{ Scan(...any) error }) (model.Term, error) {
	var t model.Term
	var enabled, locked, common int
	var note sql.NullString
	err := sc.Scan(&t.ID, &t.CourseID, &t.En, &t.EnKey, &t.Zh, &t.Source, &enabled, &t.Freq,
		&t.Tier, &locked, &common, &note, &t.ZhSource)
	t.Enabled = enabled != 0
	t.Locked = locked != 0
	t.CommonWord = common != 0
	t.JanitorNote = note.String
	return t, err
}

// termV2 fills the v2 defaults of an unset term (v2 G1, G6).
func termV2(t *model.Term) {
	if t.Tier == "" {
		t.Tier = model.TierEnforce
	}
	if t.ZhSource == "" {
		t.ZhSource = model.ZhSourceExtractor
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *sqlStore) ListTerms(ctx context.Context, courseID string) ([]model.Term, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+termCols+` FROM term WHERE course_id = ? ORDER BY en_key`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Term
	for rows.Next() {
		t, err := scanTerm(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetTerm(ctx context.Context, id string) (model.Term, error) {
	t, err := scanTerm(s.q.QueryRowContext(ctx, `SELECT `+termCols+` FROM term WHERE id = ?`, id))
	return t, notFound(err)
}

// InsertTerm inserts t. An empty ID is an error; an empty EnKey is derived from En.
func (s *sqlStore) InsertTerm(ctx context.Context, t model.Term) error {
	if t.ID == "" {
		return errors.New("db: term id required")
	}
	if t.EnKey == "" {
		t.EnKey = EnKey(t.En)
	}
	termV2(&t)
	_, err := s.q.ExecContext(ctx, `INSERT INTO term (id, course_id, en, en_key, zh, source, enabled, freq,
		tier, locked, common_word, janitor_note, zh_source) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.CourseID, t.En, t.EnKey, t.Zh, t.Source, boolInt(t.Enabled), t.Freq,
		t.Tier, boolInt(t.Locked), boolInt(t.CommonWord), nullIfEmpty(t.JanitorNote), t.ZhSource)
	return err
}

// UpdateTerm writes every column except id and course_id (v2: including
// tier, locked, common_word, janitor_note and zh_source).
func (s *sqlStore) UpdateTerm(ctx context.Context, t model.Term) error {
	if t.EnKey == "" {
		t.EnKey = EnKey(t.En)
	}
	termV2(&t)
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE term SET en = ?, en_key = ?, zh = ?, source = ?, enabled = ?, freq = ?,
		tier = ?, locked = ?, common_word = ?, janitor_note = ?, zh_source = ? WHERE id = ?`,
		t.En, t.EnKey, t.Zh, t.Source, boolInt(t.Enabled), t.Freq,
		t.Tier, boolInt(t.Locked), boolInt(t.CommonWord), nullIfEmpty(t.JanitorNote), t.ZhSource, t.ID))
}

func (s *sqlStore) DeleteTerm(ctx context.Context, id string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `DELETE FROM term WHERE id = ?`, id))
}

func (s *sqlStore) SetTermFreq(ctx context.Context, id string, freq int) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE term SET freq = ? WHERE id = ?`, freq, id))
}

// ---------- sentence ----------

const sentenceCols = `lecture_id, idx, start_ms, end_ms, asr_text, low_conf_words, repaired_text, human_text, text, zh, zh_status, zh_hash, qa_error, words`

func scanSentence(sc interface{ Scan(...any) error }) (model.Sentence, error) {
	var x model.Sentence
	var lc string
	var human, zh, zhHash, qaErr, words sql.NullString
	if err := sc.Scan(&x.LectureID, &x.Idx, &x.StartMS, &x.EndMS, &x.ASRText, &lc, &x.RepairedText,
		&human, &x.Text, &zh, &x.ZhStatus, &zhHash, &qaErr, &words); err != nil {
		return x, err
	}
	if words.Valid && words.String != "" {
		if err := json.Unmarshal([]byte(words.String), &x.Words); err != nil {
			return x, fmt.Errorf("sentence %s/%d: bad words: %w", x.LectureID, x.Idx, err)
		}
	}
	if lc != "" {
		if err := json.Unmarshal([]byte(lc), &x.LowConfWords); err != nil {
			return x, fmt.Errorf("sentence %s/%d: bad low_conf_words: %w", x.LectureID, x.Idx, err)
		}
	}
	if x.LowConfWords == nil {
		x.LowConfWords = []model.LowConfWord{}
	}
	x.HumanText, x.Zh, x.ZhHash, x.QAError = strPtr(human), strPtr(zh), strPtr(zhHash), strPtr(qaErr)
	return x, nil
}

// InsertSentences writes all rows and then fills text through RecomputeText
// in the same transaction (text is never written directly).
func (s *sqlStore) InsertSentences(ctx context.Context, lectureID string, ss []model.Sentence) error {
	return s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		idxs := make([]int, 0, len(ss))
		for _, x := range ss {
			lc := x.LowConfWords
			if lc == nil {
				lc = []model.LowConfWord{}
			}
			b, err := json.Marshal(lc)
			if err != nil {
				return err
			}
			zs := x.ZhStatus
			if zs == "" {
				zs = model.ZhNone
			}
			var words any // nil Words: NULL, no word timestamps (v2 P4.2)
			if x.Words != nil {
				wb, err := json.Marshal(x.Words)
				if err != nil {
					return err
				}
				words = string(wb)
			}
			if _, err := t.q.ExecContext(ctx, `INSERT INTO sentence (lecture_id, idx, start_ms, end_ms, asr_text, low_conf_words,
				repaired_text, human_text, text, zh, zh_status, zh_hash, qa_error, words) VALUES (?,?,?,?,?,?,?,?,'',?,?,?,?,?)`,
				lectureID, x.Idx, x.StartMS, x.EndMS, x.ASRText, string(b), x.RepairedText, nullStr(x.HumanText),
				nullStr(x.Zh), zs, nullStr(x.ZhHash), nullStr(x.QAError), words); err != nil {
				return err
			}
			idxs = append(idxs, x.Idx)
		}
		return t.RecomputeText(ctx, lectureID, idxs)
	})
}

func (s *sqlStore) ListSentences(ctx context.Context, lectureID string, offset, limit int) ([]model.Sentence, error) {
	if limit <= 0 {
		limit = -1
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.q.QueryContext(ctx, `SELECT `+sentenceCols+` FROM sentence WHERE lecture_id = ? ORDER BY idx LIMIT ? OFFSET ?`,
		lectureID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Sentence
	for rows.Next() {
		x, err := scanSentence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetSentence(ctx context.Context, lectureID string, idx int) (model.Sentence, error) {
	x, err := scanSentence(s.q.QueryRowContext(ctx, `SELECT `+sentenceCols+` FROM sentence WHERE lecture_id = ? AND idx = ?`, lectureID, idx))
	return x, notFound(err)
}

func (s *sqlStore) CountSentences(ctx context.Context, lectureID string) (int, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT count(*) FROM sentence WHERE lecture_id = ?`, lectureID).Scan(&n)
	return n, err
}

// CountZh returns the number of sentences per zh_status; all four statuses
// are present in the map (0 when absent).
func (s *sqlStore) CountZh(ctx context.Context, lectureID string) (map[string]int, error) {
	out := map[string]int{model.ZhNone: 0, model.ZhOK: 0, model.ZhQAFailed: 0, model.ZhStale: 0}
	rows, err := s.q.QueryContext(ctx, `SELECT zh_status, count(*) FROM sentence WHERE lecture_id = ? GROUP BY zh_status`, lectureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

func (s *sqlStore) DeleteSentences(ctx context.Context, lectureID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM sentence WHERE lecture_id = ?`, lectureID)
	return err
}

// SetRepairedText writes repaired_text only; the caller runs RecomputeText.
func (s *sqlStore) SetRepairedText(ctx context.Context, lectureID string, idx int, repaired string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE sentence SET repaired_text = ? WHERE lecture_id = ? AND idx = ?`,
		repaired, lectureID, idx))
}

func (s *sqlStore) ResetRepairedText(ctx context.Context, lectureID string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE sentence SET repaired_text = asr_text WHERE lecture_id = ?`, lectureID)
	return err
}

// SetHumanText writes human_text only; the caller runs RecomputeText.
func (s *sqlStore) SetHumanText(ctx context.Context, lectureID string, idx int, human *string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE sentence SET human_text = ? WHERE lecture_id = ? AND idx = ?`,
		nullStr(human), lectureID, idx))
}

func (s *sqlStore) SetTranslation(ctx context.Context, lectureID string, idx int, tr SentenceTr) error {
	zs := tr.ZhStatus
	if zs == "" {
		zs = model.ZhNone
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE sentence SET zh = ?, zh_status = ?, zh_hash = ?, qa_error = ?
		WHERE lecture_id = ? AND idx = ?`, nullStr(tr.Zh), zs, nullStr(tr.ZhHash), nullStr(tr.QAError), lectureID, idx))
}

// SetZhStatus sets zh_status for the listed sentences. An empty idxs list
// changes nothing (there is no "all" shortcut here).
func (s *sqlStore) SetZhStatus(ctx context.Context, lectureID string, idxs []int, status string) error {
	for _, part := range chunks(idxs, 500) {
		if _, err := s.q.ExecContext(ctx, `UPDATE sentence SET zh_status = ? WHERE lecture_id = ? AND idx IN (`+placeholders(len(part))+`)`,
			intsAny([]any{status, lectureID}, part)...); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqlStore) ClearTranslations(ctx context.Context, lectureID string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE sentence SET zh = NULL, zh_status = 'none', zh_hash = NULL, qa_error = NULL WHERE lecture_id = ?`, lectureID)
	return err
}

// RecomputeText is the only writer of sentence.text (SPEC §3.2):
// text = human_text when non-null, else repaired_text. idxs == nil means all
// sentences of the lecture; an empty non-nil slice changes nothing.
func (s *sqlStore) RecomputeText(ctx context.Context, lectureID string, idxs []int) error {
	const upd = `UPDATE sentence SET text = COALESCE(human_text, repaired_text) WHERE lecture_id = ?`
	if idxs == nil {
		_, err := s.q.ExecContext(ctx, upd, lectureID)
		return err
	}
	for _, part := range chunks(idxs, 500) {
		if _, err := s.q.ExecContext(ctx, upd+` AND idx IN (`+placeholders(len(part))+`)`, intsAny([]any{lectureID}, part)...); err != nil {
			return err
		}
	}
	return nil
}

// ---------- patch ----------

const patchCols = `id, lecture_id, sentence_idx, before, after, class, reason, review, edited_text, created_at`

func scanPatch(sc interface{ Scan(...any) error }) (model.Patch, error) {
	var p model.Patch
	var edited sql.NullString
	err := sc.Scan(&p.ID, &p.LectureID, &p.SentenceIdx, &p.Before, &p.After, &p.Class, &p.Reason, &p.Review, &edited, &p.CreatedAt)
	p.EditedText = strPtr(edited)
	return p, err
}

// InsertPatches inserts all patches in one transaction. Empty IDs and
// CreatedAt are filled in.
func (s *sqlStore) InsertPatches(ctx context.Context, ps []model.Patch) error {
	return s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		now := Now()
		for _, p := range ps {
			if p.ID == "" {
				p.ID = NewID()
			}
			if p.CreatedAt == "" {
				p.CreatedAt = now
			}
			if _, err := t.q.ExecContext(ctx, `INSERT INTO patch (`+patchCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
				p.ID, p.LectureID, p.SentenceIdx, p.Before, p.After, p.Class, p.Reason, p.Review, nullStr(p.EditedText), p.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *sqlStore) ListPatches(ctx context.Context, lectureID string, review string) ([]model.Patch, error) {
	q := `SELECT ` + patchCols + ` FROM patch WHERE lecture_id = ?`
	args := []any{lectureID}
	if review != "" {
		q += ` AND review = ?`
		args = append(args, review)
	}
	q += ` ORDER BY sentence_idx, created_at, id`
	rows, err := s.q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Patch
	for rows.Next() {
		p, err := scanPatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetPatch(ctx context.Context, id string) (model.Patch, error) {
	p, err := scanPatch(s.q.QueryRowContext(ctx, `SELECT `+patchCols+` FROM patch WHERE id = ?`, id))
	return p, notFound(err)
}

// PatchForSentence returns the (latest) patch of the sentence.
func (s *sqlStore) PatchForSentence(ctx context.Context, lectureID string, idx int) (model.Patch, error) {
	p, err := scanPatch(s.q.QueryRowContext(ctx, `SELECT `+patchCols+` FROM patch WHERE lecture_id = ? AND sentence_idx = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, lectureID, idx))
	return p, notFound(err)
}

func (s *sqlStore) SetPatchReview(ctx context.Context, id, review string, editedText *string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE patch SET review = ?, edited_text = ? WHERE id = ?`,
		review, nullStr(editedText), id))
}

func (s *sqlStore) DeletePatches(ctx context.Context, lectureID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM patch WHERE lecture_id = ?`, lectureID)
	return err
}

func (s *sqlStore) CountPatches(ctx context.Context, lectureID, review string) (int, error) {
	q := `SELECT count(*) FROM patch WHERE lecture_id = ?`
	args := []any{lectureID}
	if review != "" {
		q += ` AND review = ?`
		args = append(args, review)
	}
	var n int
	err := s.q.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// ---------- summary ----------

func (s *sqlStore) UpsertSummary(ctx context.Context, sm model.Summary) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO summary (lecture_id, text, input_hash) VALUES (?,?,?)
		ON CONFLICT(lecture_id) DO UPDATE SET text = excluded.text, input_hash = excluded.input_hash`,
		sm.LectureID, sm.Text, sm.InputHash)
	return err
}

func (s *sqlStore) GetSummary(ctx context.Context, lectureID string) (model.Summary, error) {
	var sm model.Summary
	err := s.q.QueryRowContext(ctx, `SELECT lecture_id, text, input_hash FROM summary WHERE lecture_id = ?`, lectureID).
		Scan(&sm.LectureID, &sm.Text, &sm.InputHash)
	return sm, notFound(err)
}

func (s *sqlStore) DeleteSummary(ctx context.Context, lectureID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM summary WHERE lecture_id = ?`, lectureID)
	return err
}

// ---------- job ----------

const jobCols = `id, scope, scope_id, stage, status, input_hash, attempt, error, progress, is_current, stats, created_at, started_at, finished_at, config_snapshot, stale_reason`

func scanJob(sc interface{ Scan(...any) error }) (model.Job, error) {
	var j model.Job
	var hash, errText, started, finished, reason sql.NullString
	var cur int
	var stats string
	if err := sc.Scan(&j.ID, &j.Scope, &j.ScopeID, &j.Stage, &j.Status, &hash, &j.Attempt, &errText, &j.Progress,
		&cur, &stats, &j.CreatedAt, &started, &finished, &j.ConfigSnapshot, &reason); err != nil {
		return j, err
	}
	j.InputHash, j.Error, j.StartedAt, j.FinishedAt = strPtr(hash), strPtr(errText), strPtr(started), strPtr(finished)
	j.StaleReason = strPtr(reason)
	j.IsCurrent = cur != 0
	if stats != "" {
		if err := json.Unmarshal([]byte(stats), &j.Stats); err != nil {
			return j, fmt.Errorf("job %s: bad stats: %w", j.ID, err)
		}
	}
	return j, nil
}

func scanJobs(rows *sql.Rows) ([]model.Job, error) {
	defer rows.Close()
	var out []model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// InsertJob inserts a new job row with its initial status. ID and Status are
// required; CreatedAt defaults to now.
func (s *sqlStore) InsertJob(ctx context.Context, j model.Job) error {
	if j.ID == "" || j.Status == "" {
		return errors.New("db: job id and status required")
	}
	if j.CreatedAt == "" {
		j.CreatedAt = Now()
	}
	stats, err := json.Marshal(j.Stats)
	if err != nil {
		return err
	}
	_, err = s.q.ExecContext(ctx, `INSERT INTO job (`+jobCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Scope, j.ScopeID, j.Stage, j.Status, nullStr(j.InputHash), j.Attempt, nullStr(j.Error), j.Progress,
		boolInt(j.IsCurrent), string(stats), j.CreatedAt, nullStr(j.StartedAt), nullStr(j.FinishedAt),
		snapshotOrEmpty(j.ConfigSnapshot), nullStr(j.StaleReason))
	return err
}

func (s *sqlStore) GetJob(ctx context.Context, id string) (model.Job, error) {
	j, err := scanJob(s.q.QueryRowContext(ctx, `SELECT `+jobCols+` FROM job WHERE id = ?`, id))
	return j, notFound(err)
}

func (s *sqlStore) GetCurrentJob(ctx context.Context, scope, scopeID, stage string) (model.Job, error) {
	j, err := scanJob(s.q.QueryRowContext(ctx, `SELECT `+jobCols+` FROM job WHERE scope = ? AND scope_id = ? AND stage = ? AND is_current = 1`,
		scope, scopeID, stage))
	return j, notFound(err)
}

func (s *sqlStore) ListCurrentJobs(ctx context.Context, scope, scopeID string) ([]model.Job, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+jobCols+` FROM job WHERE scope = ? AND scope_id = ? AND is_current = 1
		ORDER BY created_at, id`, scope, scopeID)
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

func (s *sqlStore) ListCurrentJobsByStatus(ctx context.Context, statuses ...string) ([]model.Job, error) {
	return s.listCurrentJobsByStatus(ctx, nil, statuses...)
}

func (s *sqlStore) ListScanJobs(ctx context.Context, draftHeld []string) ([]model.Job, error) {
	return s.listCurrentJobsByStatus(ctx, draftHeld, model.StatusPending, model.StatusQueued)
}

// listCurrentJobsByStatus is ListCurrentJobsByStatus leaving out the
// pending jobs of draft lectures whose stage is in draftHeld (ListScanJobs).
func (s *sqlStore) listCurrentJobsByStatus(ctx context.Context, draftHeld []string, statuses ...string) ([]model.Job, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	cols := strings.ReplaceAll("j."+jobCols, ", ", ", j.")
	args := make([]any, 0, len(statuses)+len(draftHeld)+2)
	for _, st := range statuses {
		args = append(args, st)
	}
	drafts := ""
	if len(draftHeld) > 0 {
		// l.id IS NOT NULL: a job whose lecture row is missing stays listed
		// (the scan reports its lookup error as before).
		drafts = ` AND NOT (j.status = ? AND j.scope = ? AND l.id IS NOT NULL AND l.launched_at IS NULL
			AND j.stage IN (` + placeholders(len(draftHeld)) + `))`
		args = append(args, model.StatusPending, model.ScopeLecture)
		for _, st := range draftHeld {
			args = append(args, st)
		}
	}
	q := `SELECT ` + cols + ` FROM job j
		LEFT JOIN lecture l ON j.scope = 'lecture' AND l.id = j.scope_id
		LEFT JOIN material m ON j.scope = 'material' AND m.id = j.scope_id
		LEFT JOIN course c ON j.scope = 'course' AND c.id = j.scope_id
		WHERE j.is_current = 1 AND j.status IN (` + placeholders(len(statuses)) + `)` + drafts + `
		ORDER BY COALESCE(l.created_at, m.created_at, c.created_at, j.created_at), j.scope_id, j.created_at, j.id`
	rows, err := s.q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

func (s *sqlStore) ListJobsFinishedSince(ctx context.Context, since string) ([]model.Job, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+jobCols+` FROM job WHERE finished_at IS NOT NULL AND finished_at >= ?
		ORDER BY finished_at, id`, since)
	if err != nil {
		return nil, err
	}
	return scanJobs(rows)
}

// CASJobStatus is the compare-and-set primitive behind state.Transition.
// In f, Error == "" clears the column; StartedAt/FinishedAt == "" clear them too.
func (s *sqlStore) CASJobStatus(ctx context.Context, jobID, from, to string, f JobFinish) (bool, error) {
	set := []string{"status = ?"}
	args := []any{to}
	optStr := func(col string, p *string) {
		if p == nil {
			return
		}
		set = append(set, col+" = ?")
		if *p == "" {
			args = append(args, nil)
		} else {
			args = append(args, *p)
		}
	}
	optStr("error", f.Error)
	optStr("started_at", f.StartedAt)
	optStr("finished_at", f.FinishedAt)
	optStr("stale_reason", f.StaleReason)
	if f.Attempt != nil {
		set = append(set, "attempt = ?")
		args = append(args, *f.Attempt)
	}
	if f.Progress != nil {
		set = append(set, "progress = ?")
		args = append(args, *f.Progress)
	}
	args = append(args, jobID, from)
	res, err := s.q.ExecContext(ctx, `UPDATE job SET `+strings.Join(set, ", ")+` WHERE id = ? AND status = ? AND is_current = 1`, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *sqlStore) RetireJob(ctx context.Context, jobID string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE job SET is_current = 0 WHERE id = ?`, jobID))
}

func (s *sqlStore) SetJobProgress(ctx context.Context, jobID string, p float64) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE job SET progress = ? WHERE id = ?`, p, jobID))
}

func (s *sqlStore) SetJobInputHash(ctx context.Context, jobID, hash string) error {
	var h any = hash
	if hash == "" {
		h = nil
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE job SET input_hash = ? WHERE id = ?`, h, jobID))
}

func (s *sqlStore) SetJobError(ctx context.Context, jobID string, errText *string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE job SET error = ? WHERE id = ?`, nullStr(errText), jobID))
}

func (s *sqlStore) SetJobStats(ctx context.Context, jobID string, st model.JobStats) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE job SET stats = ? WHERE id = ?`, string(b), jobID))
}

func (s *sqlStore) DeleteJobs(ctx context.Context, scope, scopeID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM job WHERE scope = ? AND scope_id = ?`, scope, scopeID)
	return err
}

// ---------- kv ----------

func (s *sqlStore) GetKV(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if isSealedKV(key) { // cred-enc: sealed at rest (sealed.go)
		v, ok := openKV(key, v)
		return v, ok, nil
	}
	return v, true, nil
}

func (s *sqlStore) SetKV(ctx context.Context, key, value string) error {
	if isSealedKV(key) { // cred-enc: sealed at rest (sealed.go)
		var err error
		if value, err = sealKV(key, value); err != nil {
			return err
		}
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

var _ Store = (*sqlStore)(nil)
