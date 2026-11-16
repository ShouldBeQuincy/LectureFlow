// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"path/filepath"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// legacyLecture is a lecture as an upgrade from before v2 leaves it
// (test-upgrade F7): created without original_filename, title_source the
// migration default filename, then (named = true) given the backfilled
// original_filename (title + media extension) with its title recorded by
// MarkLegacyTitle, as the one-time backfill does. Its summary is done.
func (e *env) legacyLecture(title string, named bool) model.Lecture {
	e.t.Helper()
	l := model.Lecture{ID: db.NewID(), CourseID: e.course.ID, Title: title,
		MediaPath:   filepath.Join(e.t.TempDir(), "media.mp4"),
		CreatedAt:   db.FormatTime(lt(2026, 10, 1, 9, 0)),
		TitleSource: model.TitleSourceFilename}
	e.must(e.st.CreateLecture(e.ctx, l))
	_, err := e.st.SetLectureLaunched(e.ctx, l.ID, l.CreatedAt)
	e.must(err)
	if named {
		e.must(e.st.SetLectureOriginalFilename(e.ctx, l.ID, title+".mp4"))
		e.must(MarkLegacyTitle(e.ctx, e.st, e.lec(l.ID)))
	}
	e.insertSample(l.ID)
	e.setSummary(l.ID, "This lecture introduces the cache hierarchy.")
	e.markDone(model.ScopeLecture, l.ID, model.StageSummary)
	return e.lec(l.ID)
}

// TestTitleLegacyTitleKept is F7: a title from before v2 whose origin is
// unknown (it may be the one the user typed in the upload form) is kept,
// title_zh included, and its source stays filename; the blurbs are written.
// 重新生成 (force) still replaces it.
func TestTitleLegacyTitleKept(t *testing.T) {
	for name, named := range map[string]bool{"backfilled name": true, "no original file name": false} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			d := e.deps(nil)
			l := e.legacyLecture("Week 2 - Hazards", named)
			if r := e.runTitle(d, l.ID); r.err != nil {
				t.Fatal(r.err)
			}
			got := e.lec(l.ID)
			if got.Title != "Week 2 - Hazards" || got.TitleZh != nil || got.TitleSource != model.TitleSourceFilename {
				t.Errorf("legacy title rewritten: %q / %s (%s)", got.Title, ptrStr(got.TitleZh), got.TitleSource)
			}
			if ptrStr(got.BlurbEn) != fixtureBlurbEn || got.BlurbZh == nil || *got.BlurbZh == "" {
				t.Errorf("blurbs %s / %s", ptrStr(got.BlurbEn), ptrStr(got.BlurbZh))
			}
			if !e.ev.has("lecture", l.ID) {
				t.Error("no SSE change")
			}

			e.must(RequestTitleForce(e.ctx, e.st, l.ID))
			if r := e.runTitle(d, l.ID); r.err != nil {
				t.Fatal(r.err)
			}
			got = e.lec(l.ID)
			if got.Title != fixtureTitleEn || ptrStr(got.TitleZh) != fixtureTitleZh || got.TitleSource != model.TitleSourceLLM {
				t.Errorf("forced regeneration: %q / %s (%s)", got.Title, ptrStr(got.TitleZh), got.TitleSource)
			}
		})
	}
}

// TestTitleLegacyTitleChangedSinceUpgrade: a pre-v2 lecture whose title the
// program replaced after the upgrade (a filename rule's title, source still
// filename) no longer has the recorded title, so it gets the model's title.
func TestTitleLegacyTitleChangedSinceUpgrade(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := e.legacyLecture("Week 2 - Hazards", true)
	e.must(e.st.SetLectureTitles(e.ctx, l.ID, "W02 Hazards", nil, model.TitleSourceFilename))
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := e.lec(l.ID); got.Title != fixtureTitleEn || ptrStr(got.TitleZh) != fixtureTitleZh || got.TitleSource != model.TitleSourceLLM {
		t.Errorf("title %q / %s (%s)", got.Title, ptrStr(got.TitleZh), got.TitleSource)
	}
}

// TestTitleNewLectureSameShape: a lecture uploaded since v2 has the same
// shape as a backfilled one (title = file name without its extension,
// media{ext}) but no legacy record, so its file-name title is replaced as
// before (P13.7).
func TestTitleNewLectureSameShape(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := model.Lecture{ID: db.NewID(), CourseID: e.course.ID, Title: "Week 2 - Hazards",
		MediaPath:        filepath.Join(e.t.TempDir(), "media.mp4"),
		CreatedAt:        db.FormatTime(lt(2026, 11, 1, 9, 0)),
		TitleSource:      model.TitleSourceFilename,
		OriginalFilename: "Week 2 - Hazards.mp4"}
	e.must(e.st.CreateLecture(e.ctx, l))
	_, err := e.st.SetLectureLaunched(e.ctx, l.ID, l.CreatedAt)
	e.must(err)
	e.insertSample(l.ID)
	e.setSummary(l.ID, "This lecture introduces the cache hierarchy.")
	e.markDone(model.ScopeLecture, l.ID, model.StageSummary)
	if r := e.runTitle(d, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if got := e.lec(l.ID); got.Title != fixtureTitleEn || ptrStr(got.TitleZh) != fixtureTitleZh || got.TitleSource != model.TitleSourceLLM {
		t.Errorf("title %q / %s (%s)", got.Title, ptrStr(got.TitleZh), got.TitleSource)
	}
}

// TestMarkLegacyTitle: only a non-empty title with source filename is
// recorded.
func TestMarkLegacyTitle(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		l    model.Lecture
		want string
	}{
		{model.Lecture{ID: "a", Title: "Week 2", TitleSource: model.TitleSourceFilename}, "Week 2"},
		{model.Lecture{ID: "b", Title: "Week 2"}, "Week 2"}, // "" = filename
		{model.Lecture{ID: "c", Title: "Mine", TitleSource: model.TitleSourceUser}, ""},
		{model.Lecture{ID: "d", Title: "Gen", TitleSource: model.TitleSourceLLM}, ""},
		{model.Lecture{ID: "e", Title: " ", TitleSource: model.TitleSourceFilename}, ""},
	} {
		e.must(MarkLegacyTitle(e.ctx, e.st, c.l))
		if got := e.kv(KVLegacyTitlePrefix + c.l.ID); got != c.want {
			t.Errorf("%s: %q, want %q", c.l.ID, got, c.want)
		}
	}
}
