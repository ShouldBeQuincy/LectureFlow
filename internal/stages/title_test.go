// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"fmt"
	"strings"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

// testdata/llm/title.json
const (
	fixtureTitleEn = "Cache Hierarchy, Write-Back and Associativity"
	fixtureTitleZh = "缓存层次结构、写回与组相联"
	fixtureBlurbEn = "How caches locate lines with tags and indexes, and how associativity reduces conflict misses."
)

// titleLecture is a lecture whose summary is done, with a title job.
func (e *env) titleLecture(filename string) model.Lecture {
	e.t.Helper()
	l := e.upload(filename, lt(2026, 10, 20, 9, 0))
	e.insertSample(l.ID)
	e.setSummary(l.ID, "This lecture introduces the cache hierarchy.")
	e.markDone(model.ScopeLecture, l.ID, model.StageSummary)
	e.auxJob(model.StageTitle, l.ID)
	return l
}

func (e *env) runTitle(d Deps, lectureID string) runResult {
	e.t.Helper()
	e.auxJob(model.StageTitle, lectureID)
	return e.run(e.stage(d, model.StageTitle), lectureID)
}

// TestTitleFromFilenameSource is P13.7: a filename title becomes the model's
// title (source llm); the blurbs are written.
func TestTitleFromFilenameSource(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := e.titleLecture("CS101 lecture 2.mp4")
	r := e.runTitle(d, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	got := e.lec(l.ID)
	if got.Title != fixtureTitleEn || ptrStr(got.TitleZh) != fixtureTitleZh || got.TitleSource != model.TitleSourceLLM {
		t.Errorf("title %q / %s (%s)", got.Title, ptrStr(got.TitleZh), got.TitleSource)
	}
	if ptrStr(got.BlurbEn) != fixtureBlurbEn || got.BlurbZh == nil || *got.BlurbZh == "" {
		t.Errorf("blurbs %s / %s", ptrStr(got.BlurbEn), ptrStr(got.BlurbZh))
	}
	if got.OriginalFilename != "CS101 lecture 2.mp4" {
		t.Error("original file name lost")
	}
	reqs := e.fake.Requests(llmtest.RoleTitle)
	if len(reqs) != 1 || !reqs[0].JSONMode {
		t.Fatalf("%d title requests", len(reqs))
	}
	if !strings.Contains(reqs[0].User, "SUMMARY:\nThis lecture introduces the cache hierarchy.") ||
		!strings.Contains(reqs[0].User, "ORIGINAL FILE NAME:\nCS101 lecture 2.mp4") {
		t.Errorf("request user:\n%s", reqs[0].User)
	}
	if strings.Contains(reqs[0].User, "SYLLABUS") {
		t.Error("syllabus entry without a computable week")
	}
	if !e.ev.has("lecture", l.ID) {
		t.Error("no SSE change")
	}
}

// TestTitleUserSourceKept is P13.7: a user title is never rewritten; the
// blurbs are still updated. Regeneration with force (P13.5 重新生成 after the
// confirmation) overwrites it once and makes it llm.
func TestTitleUserSourceKept(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := e.titleLecture("x.mp4")
	zh := "我的标题"
	e.must(e.st.SetLectureTitles(e.ctx, l.ID, "My title", &zh, model.TitleSourceUser))

	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	got := e.lec(l.ID)
	if got.Title != "My title" || ptrStr(got.TitleZh) != zh || got.TitleSource != model.TitleSourceUser {
		t.Errorf("user title rewritten: %q / %s (%s)", got.Title, ptrStr(got.TitleZh), got.TitleSource)
	}
	if ptrStr(got.BlurbEn) != fixtureBlurbEn {
		t.Errorf("blurb not updated: %s", ptrStr(got.BlurbEn))
	}

	e.must(RequestTitleForce(e.ctx, e.st, l.ID))
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	got = e.lec(l.ID)
	if got.Title != fixtureTitleEn || got.TitleSource != model.TitleSourceLLM {
		t.Errorf("forced regeneration: %q (%s)", got.Title, got.TitleSource)
	}
	if e.kv(KVTitleForcePrefix+l.ID) != "" {
		t.Error("force flag not cleared")
	}

	// the flag applies to one run only
	e.must(e.st.SetLectureTitles(e.ctx, l.ID, "Mine again", nil, model.TitleSourceUser))
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := e.lec(l.ID); got.Title != "Mine again" || got.TitleSource != model.TitleSourceUser {
		t.Errorf("user title rewritten after a forced run: %q (%s)", got.Title, got.TitleSource)
	}
}

// TestTitleLLMSourceUpdated: a title generated earlier is not the user's, so
// a new run (summary rerun, 重新生成) updates it (P13.5 “只更新简介和非用户设
// 定的标题”).
func TestTitleLLMSourceUpdated(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := e.titleLecture("x.mp4")
	old := "旧标题"
	e.must(e.st.SetLectureTitles(e.ctx, l.ID, "Old generated title", &old, model.TitleSourceLLM))
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := e.lec(l.ID); got.Title != fixtureTitleEn || ptrStr(got.TitleZh) != fixtureTitleZh || got.TitleSource != model.TitleSourceLLM {
		t.Errorf("llm title: %q / %s (%s)", got.Title, ptrStr(got.TitleZh), got.TitleSource)
	}
}

// TestTitleInvalidOutput is P13.7: unusable output (twice) fails the task;
// the titles and the pipeline stages are unchanged. When only the titles
// break the limits, the valid descriptions are still written (lead ruling
// after the phase 2 acceptance; P13.5 “简介每次都写入”).
func TestTitleInvalidOutput(t *testing.T) {
	cases := map[string]struct {
		setup  func(e *env)
		blurbs bool // the descriptions are written
	}{
		"not JSON": {func(e *env) { e.fake.FailNext(llmtest.RoleTitle, 2, llmtest.FailNotJSON) }, false},
		"title too long": {func(e *env) {
			e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
				return `{"title_en": "One Two Three Four Five Six Seven Eight Nine", "title_zh": "标题", "blurb_en": "x.", "blurb_zh": "简介。"}`
			})
		}, true},
		"Chinese title too long": {func(e *env) {
			e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
				return `{"title_en": "Caches", "title_zh": "一二三四五六七八九十一二三四五六七八九", "blurb_en": "x.", "blurb_zh": "简介。"}`
			})
		}, true},
		"missing blurb": {func(e *env) {
			e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
				return `{"title_en": "Caches", "title_zh": "缓存", "blurb_en": "", "blurb_zh": "简介。"}`
			})
		}, false},
		"blurb too long": {func(e *env) {
			e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
				return `{"title_en": "One Two Three Four Five Six Seven Eight Nine", "title_zh": "缓存", "blurb_en": "x.", "blurb_zh": "` +
					strings.Repeat("长", 51) + `"}`
			})
		}, false},
	}
	for name, c := range cases {
		setup, wantBlurbs := c.setup, c.blurbs
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			d := e.deps(nil)
			l := e.titleLecture("x.mp4")
			before := map[string]string{}
			for _, s := range model.LectureStages {
				before[s] = e.job(model.ScopeLecture, l.ID, s).Status
			}
			setup(e)
			r := e.runTitle(d, l.ID)
			if errCode(r.err) != "title.invalid_output" {
				t.Fatalf("err %v (code %q)", r.err, errCode(r.err))
			}
			if n := e.fake.Count(llmtest.RoleTitle); n != 2 {
				t.Errorf("%d requests, want 2 (one retry)", n)
			}
			got := e.lec(l.ID)
			if got.Title != "x.mp4" || got.TitleSource != model.TitleSourceFilename || got.TitleZh != nil {
				t.Errorf("title changed: %+v", got)
			}
			if wantBlurbs {
				if ptrStr(got.BlurbEn) != "x." || ptrStr(got.BlurbZh) != "简介。" {
					t.Errorf("valid descriptions not written: %s / %s", ptrStr(got.BlurbEn), ptrStr(got.BlurbZh))
				}
			} else if got.BlurbEn != nil || got.BlurbZh != nil {
				t.Errorf("descriptions written: %s / %s", ptrStr(got.BlurbEn), ptrStr(got.BlurbZh))
			}
			for _, s := range model.LectureStages {
				if st := e.job(model.ScopeLecture, l.ID, s).Status; st != before[s] {
					t.Errorf("%s: %s → %s", s, before[s], st)
				}
			}
		})
	}
}

// TestTitleRetryOnce: a first unusable answer is asked again.
func TestTitleRetryOnce(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("x.mp4")
	e.fake.FailNext(llmtest.RoleTitle, 1, llmtest.FailNotJSON)
	if r := e.runTitle(e.deps(nil), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if n := e.fake.Count(llmtest.RoleTitle); n != 2 {
		t.Errorf("%d requests", n)
	}
	if e.lec(l.ID).Title != fixtureTitleEn {
		t.Error("title not written after the retry")
	}
}

// TestTitleRetryFeedback: the retry repeats the request with the reasons of
// the rejection appended (lead ruling after the phase 2 acceptance: a whole
// lecture's Chinese title came back too long); the prompt is unchanged.
func TestTitleRetryFeedback(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("x.mp4")
	long := strings.Repeat("密", 33)
	n := 0
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
		n++
		if n == 1 {
			return `{"title_en": "Caches", "title_zh": "` + long + `", "blurb_en": "First.", "blurb_zh": "第一。"}`
		}
		return `{"title_en": "Caches", "title_zh": "缓存", "blurb_en": "Second.", "blurb_zh": "第二。"}`
	})
	if r := e.runTitle(e.deps(nil), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	reqs := e.fake.Requests(llmtest.RoleTitle)
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	if reqs[1].System != reqs[0].System || reqs[0].System != ctxbuild.TitleSystem {
		t.Error("the retry changed the system prompt")
	}
	if strings.Contains(reqs[0].User, "REJECTED") {
		t.Errorf("first request has feedback: %q", reqs[0].User)
	}
	fb := strings.TrimPrefix(reqs[1].User, reqs[0].User)
	if fb == reqs[1].User {
		t.Fatalf("the retry does not start with the original request: %q", reqs[1].User)
	}
	for _, want := range []string{"YOUR PREVIOUS ANSWER WAS REJECTED", "- title_zh has 33 characters; at most 18 are allowed", long} {
		if !strings.Contains(fb, want) {
			t.Errorf("feedback lacks %q:\n%s", want, fb)
		}
	}
	if strings.Contains(fb, "blurb") {
		t.Errorf("feedback names valid fields:\n%s", fb)
	}
	got := e.lec(l.ID)
	if got.Title != "Caches" || ptrStr(got.TitleZh) != "缓存" || ptrStr(got.BlurbEn) != "Second." {
		t.Errorf("lecture %+v", got)
	}
}

// TestTitleRetryFeedbackBlurbTooLong: a Chinese description over the limit
// (the T9b case: 52 characters, limit 50) is asked again once with feedback
// naming the field, the limit and the count; a valid second answer
// completes the task. Two over-long answers fail it after exactly two
// requests (TestTitleInvalidOutput "blurb too long").
func TestTitleRetryFeedbackBlurbTooLong(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("x.mp4")
	long := strings.Repeat("长", ctxbuild.BlurbZhLen+2)
	n := 0
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
		n++
		if n == 1 {
			return `{"title_en": "Caches", "title_zh": "缓存", "blurb_en": "First.", "blurb_zh": "` + long + `"}`
		}
		return `{"title_en": "Caches", "title_zh": "缓存", "blurb_en": "Second.", "blurb_zh": "第二。"}`
	})
	r := e.runTitle(e.deps(nil), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("err %v, job %s", r.err, r.job.Status)
	}
	reqs := e.fake.Requests(llmtest.RoleTitle)
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	fb := strings.TrimPrefix(reqs[1].User, reqs[0].User)
	if fb == reqs[1].User {
		t.Fatalf("the retry does not start with the original request: %q", reqs[1].User)
	}
	want := fmt.Sprintf("- blurb_zh has %d characters; at most %d are allowed", ctxbuild.BlurbZhLen+2, ctxbuild.BlurbZhLen)
	for _, w := range []string{"YOUR PREVIOUS ANSWER WAS REJECTED", want, long} {
		if !strings.Contains(fb, w) {
			t.Errorf("feedback lacks %q:\n%s", w, fb)
		}
	}
	if strings.Contains(fb, "title_") || strings.Contains(fb, "blurb_en") {
		t.Errorf("feedback names valid fields:\n%s", fb)
	}
	got := e.lec(l.ID)
	if got.Title != "Caches" || ptrStr(got.TitleZh) != "缓存" || ptrStr(got.BlurbEn) != "Second." || ptrStr(got.BlurbZh) != "第二。" {
		t.Errorf("lecture %+v", got)
	}
}

// TestTitleBlurbsOfLastValidAnswer: both answers fail on the titles; the
// descriptions of the second are written, the titles stay, a pending
// "regenerate" (force) stays for the next run.
func TestTitleBlurbsOfLastValidAnswer(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	l := e.titleLecture("x.mp4")
	e.must(RequestTitleForce(e.ctx, e.st, l.ID))
	n := 0
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
		n++
		return fmt.Sprintf(`{"title_en": "One Two Three Four Five Six Seven Eight Nine", "title_zh": "缓存", "blurb_en": "Answer %d.", "blurb_zh": "第%d次。"}`, n, n)
	})
	r := e.runTitle(e.deps(nil), l.ID)
	if errCode(r.err) != "title.invalid_output" || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v, job %s", r.err, r.job.Status)
	}
	if !strings.Contains(r.err.Error(), "title_en has 9 words") {
		t.Errorf("error does not say why: %v", r.err)
	}
	got := e.lec(l.ID)
	if got.Title != "x.mp4" || got.TitleZh != nil || ptrStr(got.BlurbEn) != "Answer 2." || ptrStr(got.BlurbZh) != "第2次。" {
		t.Errorf("lecture %+v", got)
	}
	if e.kv(KVTitleForcePrefix+l.ID) != "1" {
		t.Error("the pending force was cleared without writing the titles")
	}
}

// TestTitleSyllabusWeek: the request carries the Weekly Topics entry of the
// lecture's teaching week only (P13.5, G6 rule 5).
func TestTitleSyllabusWeek(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	md := "# CS101\n\n## Weekly Topics\n\n| Week | Topic | Notes |\n|---|---|---|\n| 1 | Introduction | |\n| 2 | Cache hierarchy | write-back |\n| 3 | Virtual memory | |\n\n## Key Terms\n\ncache\n"
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "s.md", Path: "s.md", Text: &md}))
	l := e.titleLecture("x.mp4")
	e.must(e.st.SetLectureRecordedAt(e.ctx, l.ID, strp("2026-10-07T10:00"), model.RecordedAtUser))

	// no term start: the week is unknown, no entry
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if u := e.fake.Requests(llmtest.RoleTitle)[0].User; strings.Contains(u, "SYLLABUS") {
		t.Errorf("entry without a term start:\n%s", u)
	}

	ts := "2026-09-28"
	e.must(e.st.SetCourseCalendar(e.ctx, e.course.ID, &ts, nil))
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	u := e.fake.Requests(llmtest.RoleTitle)[1].User
	want := "SYLLABUS ENTRY FOR THIS WEEK:\n| Week | Topic | Notes |\n| 2 | Cache hierarchy | write-back |"
	if !strings.Contains(u, want) {
		t.Errorf("request lacks the week 2 entry:\n%s", u)
	}
	if strings.Contains(u, "Introduction") || strings.Contains(u, "Virtual memory") {
		t.Errorf("other weeks given to the model:\n%s", u)
	}
}

// TestTitleNoSummary: without a summary the task fails without a request.
func TestTitleNoSummary(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	l := e.upload("x.mp4", lt(2026, 10, 20, 9, 0))
	r := e.runTitle(e.deps(nil), l.ID)
	if errCode(r.err) != "title.no_summary" {
		t.Errorf("err %v", r.err)
	}
	if n := len(e.fake.Requests("")); n != 0 {
		t.Errorf("%d requests", n)
	}
}

// TestTitleUserEditDuringRun: a title the user sets while the request runs
// is kept (title_source is read when the result is written).
func TestTitleUserEditDuringRun(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := e.titleLecture("x.mp4")
	e.fake.Handle(llmtest.RoleTitle, func(string, string) string {
		e.must(e.st.SetLectureTitles(e.ctx, l.ID, "Typed meanwhile", nil, model.TitleSourceUser))
		return `{"title_en": "Caches", "title_zh": "缓存", "blurb_en": "About caches.", "blurb_zh": "关于缓存。"}`
	})
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := e.lec(l.ID); got.Title != "Typed meanwhile" || got.TitleSource != model.TitleSourceUser || ptrStr(got.BlurbEn) != "About caches." {
		t.Errorf("%q (%s) %s", got.Title, got.TitleSource, ptrStr(got.BlurbEn))
	}
}
