// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	lfmedia "lectureflow/internal/media"
	"lectureflow/internal/model"
)

// ---------- organizer-p2 test helpers (calendar service, title, chapters, filename_rule) ----------

// organizerRoles configures the chapters and organizer roles of the legacy
// config like summary (Appendix D1's default).
func (e *env) organizerRoles() {
	for _, r := range []string{model.RoleOrganizer, model.RoleChapters} {
		e.cfg.LLM.Roles[r] = e.cfg.LLM.Roles[model.RoleSummary]
	}
}

// lt is a local wall-clock time (recorded_at, upload times).
func lt(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.Local)
}

// upload creates a launched lecture as the v2 upload does: the original
// file name, the title derived from it (title_source filename) and
// created_at = uploaded.
func (e *env) upload(filename string, uploaded time.Time) model.Lecture {
	e.t.Helper()
	l := model.Lecture{ID: db.NewID(), CourseID: e.course.ID, Title: filename, MediaPath: filename,
		CreatedAt: db.FormatTime(uploaded), OriginalFilename: filename}
	e.must(e.st.CreateLecture(e.ctx, l))
	_, err := e.st.SetLectureLaunched(e.ctx, l.ID, l.CreatedAt)
	e.must(err)
	return e.lec(l.ID)
}

func (e *env) lec(id string) model.Lecture {
	e.t.Helper()
	l, err := e.st.GetLecture(e.ctx, id)
	e.must(err)
	return l
}

func (e *env) courseRow() model.Course {
	e.t.Helper()
	c, err := e.st.GetCourse(e.ctx, e.course.ID)
	e.must(err)
	return c
}

// auxJob makes a runnable current job of an auxiliary task (what the
// scheduler's enqueue does): an existing pending/failed one is kept, a done
// one is retired and replaced by a new pending job.
func (e *env) auxJob(stage, scopeID string) {
	e.t.Helper()
	if j, err := e.st.GetCurrentJob(e.ctx, model.AuxScope(stage), scopeID, stage); err == nil {
		if j.Status != model.StatusDone {
			return
		}
		e.must(e.st.RetireJob(e.ctx, j.ID))
	}
	e.must(e.st.InsertJob(e.ctx, model.Job{ID: db.NewID(), Scope: model.AuxScope(stage), ScopeID: scopeID, Stage: stage,
		Status: model.StatusPending, IsCurrent: true, CreatedAt: db.Now()}))
}

// setRule stores a course filename rule directly.
func (e *env) setRule(regex, source string) {
	e.t.Helper()
	b, err := json.Marshal(calendar.FilenameRule{Regex: regex, Source: source, CreatedAt: db.Now()})
	e.must(err)
	raw := string(b)
	e.must(e.st.SetCourseFilenameRule(e.ctx, e.course.ID, &raw))
}

func (e *env) kv(key string) string {
	e.t.Helper()
	v, _, err := e.st.GetKV(e.ctx, key)
	e.must(err)
	return v
}

// errCode is the ErrorInfo code of a stage error ("" when not coded).
func errCode(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return ce.Info.Code
	}
	return ""
}

func strp(s string) *string { return &s }

func ptrStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// ruleCS101 is the testdata/llm/filename_rule.json regex.
const ruleCS101 = `^CS101_(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})_(?P<hour>\d{2})(?P<minute>\d{2})(?:_(?P<title>[^.]+))?\.\w+$`

// ---------- tests ----------

// TestResolveRecordedAtOrder is P13.3.4 / P13.3.5: user > filename >
// creation_time (not on the upload day) > none; the rule's title replaces a
// filename title; seq follows the recording order.
func TestResolveRecordedAtOrder(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	e.setRule(ruleCS101, calendar.RuleSourceLLM)
	up := lt(2026, 10, 20, 15, 0)

	byName := e.upload("CS101_2026-10-06_1000_Stream_Ciphers.mp4", up)
	meta := e.upload("lecture recording.mp4", up.Add(time.Minute))
	sameDay := e.upload("download.mp4", up.Add(2*time.Minute))
	user := e.upload("CS101_2026-10-13_1000.mp4", up.Add(3*time.Minute))
	e.must(e.st.SetLectureRecordedAt(e.ctx, user.ID, strp("2026-09-30T09:00"), model.RecordedAtUser))
	e.must(e.st.SetLectureTitles(e.ctx, user.ID, "My own title", nil, model.TitleSourceUser))

	ctMeta := time.Date(2026, 9, 29, 9, 5, 0, 0, time.UTC) // well before the upload day
	ctSame := up.Add(-time.Hour).UTC()                     // the upload day: a download time
	e.must(ResolveRecordedAt(e.ctx, d, byName.ID, nil))
	e.must(ResolveRecordedAt(e.ctx, d, meta.ID, &ctMeta))
	e.must(ResolveRecordedAt(e.ctx, d, sameDay.ID, &ctSame))
	ctUser := ctMeta
	e.must(ResolveRecordedAt(e.ctx, d, user.ID, &ctUser))

	check := func(id, wantAt, wantSrc string) {
		t.Helper()
		l := e.lec(id)
		if ptrStr(l.RecordedAt) != wantAt || l.RecordedAtSource != wantSrc {
			t.Errorf("%s: recorded_at %s (%s), want %s (%s)", l.Title, ptrStr(l.RecordedAt), l.RecordedAtSource, wantAt, wantSrc)
		}
	}
	check(byName.ID, "2026-10-06T10:00", model.RecordedAtFilename)
	check(meta.ID, calendar.FormatRecordedAt(ctMeta.In(time.Local)), model.RecordedAtMetadata)
	check(sameDay.ID, "<nil>", model.RecordedAtNone)
	check(user.ID, "2026-09-30T09:00", model.RecordedAtUser)

	if l := e.lec(byName.ID); l.Title != "Stream Ciphers" || l.TitleSource != model.TitleSourceFilename {
		t.Errorf("rule title: %q (%s), want the cleaned title group with source filename", l.Title, l.TitleSource)
	}
	if l := e.lec(user.ID); l.Title != "My own title" || l.TitleSource != model.TitleSourceUser {
		t.Errorf("user title changed: %q (%s)", l.Title, l.TitleSource)
	}
	// seq = recording order; undated last in upload order
	for id, want := range map[string]int{meta.ID: 1, user.ID: 2, byName.ID: 3, sameDay.ID: 4} {
		if got := e.lec(id).Seq; got != want {
			t.Errorf("seq of %s = %d, want %d", e.lec(id).Title, got, want)
		}
	}
	if !e.ev.has("lecture", byName.ID) || !e.ev.has("course", e.course.ID) {
		t.Error("no SSE change events")
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Errorf("%d LLM requests while resolving recording times", n)
	}

	// the creation_time is remembered: a later rule that matches nothing
	// keeps the metadata date instead of dropping it
	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: `^XYZ(?P<month>\d\d)(?P<day>\d\d)$`, Source: calendar.RuleSourceUser}))
	check(meta.ID, calendar.FormatRecordedAt(ctMeta.In(time.Local)), model.RecordedAtMetadata)
	check(byName.ID, "<nil>", model.RecordedAtNone)
	check(user.ID, "2026-09-30T09:00", model.RecordedAtUser)
}

// TestNewUploadUsesRuleWithoutLLM is P13.3.5: a new upload in a course with
// a rule is resolved by the program; the fake LLM receives no request.
func TestNewUploadUsesRuleWithoutLLM(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	e.setRule(ruleCS101, calendar.RuleSourceLLM)
	l := e.upload("CS101_2026-10-08_1400.mp4", lt(2026, 10, 9, 8, 0))
	e.must(ResolveRecordedAt(e.ctx, d, l.ID, nil))
	if got := e.lec(l.ID); ptrStr(got.RecordedAt) != "2026-10-08T14:00" || got.RecordedAtSource != model.RecordedAtFilename {
		t.Errorf("recorded_at %s (%s)", ptrStr(got.RecordedAt), got.RecordedAtSource)
	}
	if n := e.fake.Count("filename_rule"); n != 0 {
		t.Errorf("filename_rule requests = %d, want 0", n)
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Errorf("LLM requests = %d, want 0", n)
	}
}

// TestResolveRecordedAtFutureDate: a file name date after the upload day is
// not used (P13.3.2 rule 3; calendar-builder decision 8).
func TestResolveRecordedAtFutureDate(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	e.setRule(ruleCS101, calendar.RuleSourceLLM)
	l := e.upload("CS101_2026-12-01_1000.mp4", lt(2026, 10, 9, 8, 0))
	ct := time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)
	e.must(ResolveRecordedAt(e.ctx, d, l.ID, &ct))
	if got := e.lec(l.ID); ptrStr(got.RecordedAt) != "2026-10-01T09:00" || got.RecordedAtSource != model.RecordedAtMetadata {
		t.Errorf("recorded_at %s (%s), want the metadata date", ptrStr(got.RecordedAt), got.RecordedAtSource)
	}
}

// TestApplyFilenameRule: saving a rule re-resolves every lecture whose
// source is not user, clears the proposal and the undo record, and rejects
// a rule that does not compile without changing anything.
func TestApplyFilenameRule(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	a := e.upload("CS101_2026-10-06_1000_Intro.mp4", lt(2026, 10, 20, 9, 0))
	b := e.upload("CS101_2026-10-01_0900.mp4", lt(2026, 10, 20, 9, 1))
	u := e.upload("CS101_2026-10-02_0900.mp4", lt(2026, 10, 20, 9, 2))
	e.must(e.st.SetLectureRecordedAt(e.ctx, u.ID, strp("2026-09-01T08:00"), model.RecordedAtUser))
	e.must(e.st.SetKV(e.ctx, model.KVFilenameProposal+e.course.ID, `{"rule":{}}`))
	e.must(e.st.SetKV(e.ctx, model.KVFilenameUndo+e.course.ID, `{"rule":null,"lectures":[]}`))

	if err := ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: `(?P<month>\d+`, Source: calendar.RuleSourceUser}); err == nil || errCode(err) != "filename_rule.invalid" {
		t.Fatalf("invalid regex: err %v (code %q)", err, errCode(err))
	}
	if e.courseRow().FilenameRule != nil || e.kv(model.KVFilenameProposal+e.course.ID) == "" {
		t.Fatal("a rejected rule changed the course")
	}

	e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: ruleCS101, Source: calendar.RuleSourceUser}))
	c := e.courseRow()
	var saved calendar.FilenameRule
	if c.FilenameRule == nil || json.Unmarshal([]byte(*c.FilenameRule), &saved) != nil {
		t.Fatalf("rule not saved: %v", c.FilenameRule)
	}
	if saved.Regex != ruleCS101 || saved.Source != calendar.RuleSourceUser || saved.CreatedAt == "" || saved.SamplesTotal != 3 || saved.SamplesMatched != 3 {
		t.Errorf("saved rule %+v", saved)
	}
	if got := e.lec(a.ID); ptrStr(got.RecordedAt) != "2026-10-06T10:00" || got.Title != "Intro" {
		t.Errorf("a: %s %q", ptrStr(got.RecordedAt), got.Title)
	}
	if got := e.lec(b.ID); ptrStr(got.RecordedAt) != "2026-10-01T09:00" || got.RecordedAtSource != model.RecordedAtFilename {
		t.Errorf("b: %s (%s)", ptrStr(got.RecordedAt), got.RecordedAtSource)
	}
	if got := e.lec(u.ID); ptrStr(got.RecordedAt) != "2026-09-01T08:00" || got.RecordedAtSource != model.RecordedAtUser {
		t.Errorf("user lecture re-resolved: %s (%s)", ptrStr(got.RecordedAt), got.RecordedAtSource)
	}
	if e.kv(model.KVFilenameProposal+e.course.ID) != "" || e.kv(model.KVFilenameUndo+e.course.ID) != "" {
		t.Error("proposal / undo record not cleared")
	}
	for id, want := range map[string]int{u.ID: 1, b.ID: 2, a.ID: 3} {
		if got := e.lec(id).Seq; got != want {
			t.Errorf("seq %d, want %d", got, want)
		}
	}
}

// TestCourseLabels: every lecture (drafts too) with its P13.4 number,
// keyed by id; no term start → all W??.
func TestCourseLabels(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	l1 := e.upload("a.mp4", lt(2026, 10, 20, 9, 0))
	l2 := e.upload("b.mp4", lt(2026, 10, 20, 9, 1))
	l3 := e.upload("c.mp4", lt(2026, 10, 20, 9, 2))
	dr := e.draft("draft", "d.mp4")
	e.must(e.st.SetLectureRecordedAt(e.ctx, l1.ID, strp("2026-10-07T10:00"), model.RecordedAtUser))
	e.must(e.st.SetLectureRecordedAt(e.ctx, l2.ID, strp("2026-09-29T10:00"), model.RecordedAtUser))
	e.must(e.st.SetLectureRecordedAt(e.ctx, l3.ID, strp("2026-09-30T10:00"), model.RecordedAtFilename))

	got, err := CourseLabels(e.ctx, d, e.course.ID)
	e.must(err)
	if len(got) != 4 {
		t.Fatalf("%d labels, want 4 (drafts included)", len(got))
	}
	for id, n := range got {
		if n.ID != id || n.Label != calendar.Unknown {
			t.Errorf("without term start: %+v", n)
		}
	}

	ts := "2026-09-30" // any day of week 1 (Monday 28 Sep)
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &ts, nil))
	got, err = CourseLabels(e.ctx, d, e.course.ID)
	e.must(err)
	for id, want := range map[string]string{l2.ID: "W01L1", l3.ID: "W01L2", l1.ID: "W02L1", dr.ID: calendar.Unknown} {
		if got[id].Label != want {
			t.Errorf("label %s, want %s", got[id].Label, want)
		}
	}
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &ts, []string{"2026-10-05"}))
	got, err = CourseLabels(e.ctx, d, e.course.ID)
	e.must(err)
	if got[l1.ID].Label != calendar.Unknown {
		t.Errorf("lecture in a skipped week: %s", got[l1.ID].Label)
	}

	e.must(RefreshCourseSeq(e.ctx, d, e.course.ID))
	for id, want := range map[string]int{l2.ID: 1, l3.ID: 2, l1.ID: 3, dr.ID: 4} {
		if s := e.lec(id).Seq; s != want {
			t.Errorf("seq %d, want %d", s, want)
		}
	}
}

// TestUndoFilenameRule: nothing to undo → ErrNoFilenameUndo (db.ErrNotFound);
// otherwise the rule and the lectures the user did not touch since are
// restored.
func TestUndoFilenameRule(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	err := UndoFilenameRule(e.ctx, d, e.course.ID)
	if !errors.Is(err, ErrNoFilenameUndo) || !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("nothing to undo: %v", err)
	}

	a := e.upload("CS101_2026-10-06_1000_Intro.mp4", lt(2026, 10, 20, 9, 0))
	b := e.upload("CS101_2026-10-01_0900_Caches.mp4", lt(2026, 10, 20, 9, 1))
	e.auxJob(model.StageFilenameRule, e.course.ID)
	if r := e.run(e.stage(d, model.StageFilenameRule), e.course.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if e.courseRow().FilenameRule == nil || e.kv(model.KVFilenameUndo+e.course.ID) == "" {
		t.Fatal("first exception: rule not applied automatically")
	}
	// the user sets b's time afterwards: undo keeps it
	e.must(e.st.SetLectureRecordedAt(e.ctx, b.ID, strp("2026-10-02T11:00"), model.RecordedAtUser))

	e.must(UndoFilenameRule(e.ctx, d, e.course.ID))
	if e.courseRow().FilenameRule != nil {
		t.Error("rule not removed")
	}
	if got := e.lec(a.ID); got.RecordedAt != nil || got.RecordedAtSource != model.RecordedAtNone || got.Title != a.Title {
		t.Errorf("a not restored: %s (%s) %q", ptrStr(got.RecordedAt), got.RecordedAtSource, got.Title)
	}
	if got := e.lec(b.ID); ptrStr(got.RecordedAt) != "2026-10-02T11:00" || got.RecordedAtSource != model.RecordedAtUser {
		t.Errorf("user time undone: %s (%s)", ptrStr(got.RecordedAt), got.RecordedAtSource)
	}
	if !errors.Is(UndoFilenameRule(e.ctx, d, e.course.ID), ErrNoFilenameUndo) {
		t.Error("second undo")
	}
}

// TestFilenameRuleTitleOnlyForFilenameSource (P13.3.3, P13.3.5, P13.5):
// file names that match the course rule's title group give their title
// only to lectures whose title_source is filename — on upload
// (ResolveRecordedAt) and when a rule is applied (ApplyFilenameRule). A
// title the user set and one the title task wrote (llm) are never
// rewritten; their recording time is still resolved from the file name.
func TestFilenameRuleTitleOnlyForFilenameSource(t *testing.T) {
	for _, via := range []string{"resolve", "apply"} {
		t.Run(via, func(t *testing.T) {
			e := newEnv(t)
			d := e.deps(nil)
			up := lt(2026, 10, 20, 15, 0)
			byName := e.upload("CS101_2026-10-06_1000_Stream_Ciphers.mp4", up)
			user := e.upload("CS101_2026-10-08_1000_Hash_Functions.mp4", up.Add(time.Minute))
			byLLM := e.upload("CS101_2026-10-09_1000_Public_Key.mp4", up.Add(2*time.Minute))
			e.must(e.st.SetLectureTitles(e.ctx, user.ID, "My own title", nil, model.TitleSourceUser))
			e.must(e.st.SetLectureTitles(e.ctx, byLLM.ID, "Public-Key Cryptography", strp("公钥密码学"), model.TitleSourceLLM))

			switch via {
			case "resolve":
				e.setRule(ruleCS101, calendar.RuleSourceLLM)
				for _, l := range []model.Lecture{byName, user, byLLM} {
					e.must(ResolveRecordedAt(e.ctx, d, l.ID, nil))
				}
			case "apply":
				e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: ruleCS101, Source: calendar.RuleSourceUser}))
			}

			for _, c := range []struct {
				id, title, titleZh, source, at string
			}{
				{byName.ID, "Stream Ciphers", "<nil>", model.TitleSourceFilename, "2026-10-06T10:00"},
				{user.ID, "My own title", "<nil>", model.TitleSourceUser, "2026-10-08T10:00"},
				{byLLM.ID, "Public-Key Cryptography", "公钥密码学", model.TitleSourceLLM, "2026-10-09T10:00"},
			} {
				l := e.lec(c.id)
				if l.Title != c.title || ptrStr(l.TitleZh) != c.titleZh || l.TitleSource != c.source {
					t.Errorf("title %q / %s (%s), want %q / %s (%s)", l.Title, ptrStr(l.TitleZh), l.TitleSource, c.title, c.titleZh, c.source)
				}
				if ptrStr(l.RecordedAt) != c.at || l.RecordedAtSource != model.RecordedAtFilename {
					t.Errorf("%s: recorded_at %s (%s), want %s from the file name", c.title, ptrStr(l.RecordedAt), l.RecordedAtSource, c.at)
				}
			}
		})
	}
}

// A title the file-name rule captures is stored like every title
// (lfmedia.CleanTitle, test-media I8): at most 200 characters, also when
// the file name gives a longer one, on both the resolve and the apply path.
func TestFilenameRuleTitleCapped(t *testing.T) {
	long := strings.Repeat("x", 230)
	want := strings.Repeat("x", lfmedia.MaxTitleRunes)
	for _, via := range []string{"resolve", "apply"} {
		t.Run(via, func(t *testing.T) {
			e := newEnv(t)
			d := e.deps(nil)
			l := e.upload("CS101_2026-10-06_1000_"+long+".mp4", lt(2026, 10, 20, 15, 0))
			switch via {
			case "resolve":
				e.setRule(ruleCS101, calendar.RuleSourceLLM)
				e.must(ResolveRecordedAt(e.ctx, d, l.ID, nil))
			case "apply":
				e.must(ApplyFilenameRule(e.ctx, d, e.course.ID, calendar.FilenameRule{Regex: ruleCS101, Source: calendar.RuleSourceUser}))
			}
			if got := e.lec(l.ID); got.Title != want || got.TitleSource != model.TitleSourceFilename {
				t.Errorf("title %q (%d characters, %s), want %d x (filename)", got.Title, len([]rune(got.Title)), got.TitleSource, lfmedia.MaxTitleRunes)
			}
		})
	}
}

// TestUndoFilenameRuleKeepsLaterTitles (P13.3.3 撤销; organizer-p2 decision
// 10): undoing the automatic apply restores the title of a lecture whose
// title_source is still filename, and keeps a title set afterwards by the
// user or by the title task (llm); the recording times, which none of them
// set by hand, are all restored.
func TestUndoFilenameRuleKeepsLaterTitles(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	a := e.upload("CS101_2026-10-06_1000_Intro.mp4", lt(2026, 10, 20, 9, 0))
	b := e.upload("CS101_2026-10-01_0900_Caches.mp4", lt(2026, 10, 20, 9, 1))
	c := e.upload("CS101_2026-10-02_0900_Memory.mp4", lt(2026, 10, 20, 9, 2))
	e.auxJob(model.StageFilenameRule, e.course.ID)
	if r := e.run(e.stage(d, model.StageFilenameRule), e.course.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if e.kv(model.KVFilenameUndo+e.course.ID) == "" {
		t.Fatal("first exception: rule not applied automatically")
	}
	for id, want := range map[string]string{a.ID: "Intro", b.ID: "Caches", c.ID: "Memory"} {
		if got := e.lec(id); got.Title != want || got.TitleSource != model.TitleSourceFilename {
			t.Fatalf("after the apply: %q (%s), want %q (filename)", got.Title, got.TitleSource, want)
		}
	}
	// afterwards the user renames b and the title task titles c
	e.must(e.st.SetLectureTitles(e.ctx, b.ID, "My caches", nil, model.TitleSourceUser))
	e.must(e.st.SetLectureTitles(e.ctx, c.ID, "Virtual Memory", strp("虚拟内存"), model.TitleSourceLLM))

	e.must(UndoFilenameRule(e.ctx, d, e.course.ID))
	if got := e.lec(a.ID); got.Title != a.Title || got.TitleSource != model.TitleSourceFilename {
		t.Errorf("a: %q (%s), want the original %q", got.Title, got.TitleSource, a.Title)
	}
	if got := e.lec(b.ID); got.Title != "My caches" || got.TitleSource != model.TitleSourceUser {
		t.Errorf("b: user title undone: %q (%s)", got.Title, got.TitleSource)
	}
	if got := e.lec(c.ID); got.Title != "Virtual Memory" || ptrStr(got.TitleZh) != "虚拟内存" || got.TitleSource != model.TitleSourceLLM {
		t.Errorf("c: llm title undone: %q / %s (%s)", got.Title, ptrStr(got.TitleZh), got.TitleSource)
	}
	for _, id := range []string{a.ID, b.ID, c.ID} {
		if got := e.lec(id); got.RecordedAt != nil || got.RecordedAtSource != model.RecordedAtNone {
			t.Errorf("%q: recorded_at %s (%s), want none again", got.Title, ptrStr(got.RecordedAt), got.RecordedAtSource)
		}
	}
}
