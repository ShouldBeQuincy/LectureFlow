// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/glossary"
	"lectureflow/internal/model"
)

func termNamed(ts []api.Term, en string) *api.Term {
	for i := range ts {
		if ts[i].En == en {
			return &ts[i]
		}
	}
	return nil
}

// The example glossary, report and course status agree (janitor.go).
func TestJanitorFixtures(t *testing.T) {
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3631)
	var c api.Course
	doJSON(t, "GET", base, nil, 200, &c)
	var rep model.JanitorReport
	doJSON(t, "GET", base+"/glossary/report", nil, 200, &rep)
	// the last run changed nothing; the change list is the earlier report's
	var run model.JanitorReport
	doJSON(t, "GET", base+"/glossary/report?run=last", nil, 200, &run)
	j := c.Janitor
	if !j.Configured || j.Running || j.LastVersion != run.Version || rep.Version != c.GlossaryVersion || j.LastModel != "Anthropic / claude-opus-5-5" ||
		j.LastChanges != len(rep.Items) || len(rep.Rejected) == 0 || j.LastAt != run.CreatedAt || len(run.Items) != 0 || run.CreatedAt <= rep.CreatedAt || len(run.Rejected) == 0 {
		t.Fatalf("janitor %+v / report v%d, %d items / last run %+v", j, rep.Version, len(rep.Items), run)
	}
	var terms []api.Term
	doJSON(t, "GET", base+"/terms", nil, 200, &terms)
	for _, it := range rep.Items {
		tm := termNamed(terms, it.En)
		if tm == nil || tm.ID != it.TermID || tm.JanitorNote != it.Reason || tm.Tier != it.After.Tier || tm.Enabled != it.After.Enabled || tm.Zh != it.After.Zh {
			t.Errorf("item %+v vs term %+v", it, tm)
		}
	}
	if iv := termNamed(terms, "IV"); iv == nil || iv.ZhSource != model.ZhSourceJanitor {
		t.Errorf("IV %+v", iv)
	}
	if k := termNamed(terms, "Kerckhoffs's principle"); k == nil || !k.Locked || k.ZhSource != model.ZhSourceUser {
		t.Errorf("manual term %+v", k)
	}
	if st := stageOf(c.Stages, model.StageGlossaryReview); st != model.StatusDone {
		t.Errorf("glossary_review %q", st)
	}
	var e api.ErrorBody
	doJSON(t, "GET", base+"/glossary/report?version=1", nil, 404, &e)
	doJSON(t, "GET", base+"/glossary/report?run=first", nil, 400, &e)
	doJSON(t, "GET", srv.URL+"/api/courses/"+courseID(c2120)+"/glossary/report", nil, 404, &e)
	doJSON(t, "GET", srv.URL+"/api/courses/"+courseID(c2120)+"/glossary/report?run=last", nil, 404, &e)
}

// "整理术语表" with a janitor queues a run that finishes with a report;
// under ?mock=unset the rule runs at once (ruling 5).
func TestJanitorReviewAndFallback(t *testing.T) {
	fastSyllabus(t)
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c2120)
	var before api.Course
	doJSON(t, "GET", base, nil, 200, &before)
	var res api.GlossaryReviewResult
	doJSON(t, "POST", base+"/glossary/review", nil, http.StatusAccepted, &res)
	if !res.Queued || res.Report != nil {
		t.Fatalf("review %+v", res)
	}
	get := func() api.Course {
		var x api.Course
		doJSON(t, "GET", base, nil, 200, &x)
		return x
	}
	eventually(t, "glossary_review done", func() bool { return stageOf(get().Stages, model.StageGlossaryReview) == model.StatusDone })
	after := get()
	var rep model.JanitorReport
	doJSON(t, "GET", base+"/glossary/report", nil, 200, &rep)
	if rep.Fallback || rep.Model != "claude-opus-5-5" || len(rep.Items) == 0 || after.GlossaryVersion != before.GlossaryVersion+1 || after.Janitor.LastVersion != after.GlossaryVersion {
		t.Fatalf("report %+v, course %+v", rep, after.Janitor)
	}
	// A locked common word is reported as rejected, not changed.
	var terms []api.Term
	doJSON(t, "GET", base+"/terms", nil, 200, &terms)
	if p := termNamed(terms, "process"); p == nil || p.Tier != model.TierReference || p.JanitorNote == "" {
		t.Errorf("process %+v", p)
	}

	// Without a janitor (?mock=unset): the rule, at once.
	cs := srv.URL + "/api/courses/" + courseID(c3121)
	var c api.Course
	doJSON(t, "GET", cs+"?mock=unset", nil, 200, &c)
	if c.Janitor.Configured {
		t.Fatal("unset scenario: janitor configured")
	}
	doJSON(t, "POST", cs+"/glossary/review?mock=unset", nil, http.StatusOK, &res)
	if res.Queued || res.Report == nil || !res.Report.Fallback || res.Report.Note == "" {
		t.Fatalf("fallback %+v", res)
	}
	// the coded note, as internal/glossary writes it (the page shows it in
	// the interface language)
	if ni := res.Report.NoteInfo; ni == nil || ni.Code != glossary.CodeNoteFallbackManual {
		t.Errorf("fallback note_info %+v", ni)
	}
	if rep.NoteInfo != nil {
		t.Errorf("janitor run note_info %+v", rep.NoteInfo)
	}
	for _, it := range res.Report.Items {
		if it.Action != model.JanitorReference && it.Action != model.JanitorDisable {
			t.Errorf("fallback action %+v", it)
		}
	}
	doJSON(t, "GET", cs+"?mock=unset", nil, 200, &c)
	if !c.Janitor.LastFallback || c.Janitor.LastModel != "常用词规则" {
		t.Errorf("janitor after the rule %+v", c.Janitor)
	}
}

// Undo (G3.5): the term goes back and is locked, the version + 1; again 409.
func TestJanitorUndo(t *testing.T) {
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3631)
	var c api.Course
	doJSON(t, "GET", base, nil, 200, &c)
	var rep model.JanitorReport
	doJSON(t, "GET", base+"/glossary/report", nil, 200, &rep)
	it := rep.Items[0]
	var out model.JanitorReport
	doJSON(t, "POST", base+"/glossary/report/undo", api.JanitorUndoInput{Version: rep.Version, TermID: it.TermID}, 200, &out)
	if !out.Items[0].Undone || out.Items[0].UndoneAt == "" {
		t.Fatalf("undone %+v", out.Items[0])
	}
	var terms []api.Term
	doJSON(t, "GET", base+"/terms", nil, 200, &terms)
	tm := termNamed(terms, it.En)
	if tm == nil || !tm.Locked || tm.Tier != it.Before.Tier || tm.Enabled != it.Before.Enabled || tm.JanitorNote != "" {
		t.Fatalf("term after undo %+v", tm)
	}
	var c2 api.Course
	doJSON(t, "GET", base, nil, 200, &c2)
	if c2.GlossaryVersion != c.GlossaryVersion+1 || c2.Janitor.LastChanges != c.Janitor.LastChanges-1 {
		t.Errorf("course after undo: v%d, %+v", c2.GlossaryVersion, c2.Janitor)
	}
	var e api.ErrorBody
	doJSON(t, "POST", base+"/glossary/report/undo", api.JanitorUndoInput{Version: rep.Version, TermID: it.TermID}, 409, &e)
	doJSON(t, "POST", base+"/glossary/report/undo", api.JanitorUndoInput{Version: rep.Version, TermID: "nope"}, 404, &e)
	doJSON(t, "POST", base+"/glossary/report/undo", api.JanitorUndoInput{Version: 99, TermID: it.TermID}, 404, &e)
	doJSON(t, "POST", base+"/glossary/report/undo", api.JanitorUndoInput{}, 400, &e)
	doJSON(t, "GET", base+"/glossary/report?version="+strconv.Itoa(rep.Version), nil, 200, &out)
	if !out.Items[0].Undone {
		t.Error("undo not kept in the report")
	}
}

// Ruling 8 in the mock: edits lock (zh → zh_source user), unlock changes
// nothing else and keeps the version.
func TestMockTermLocking(t *testing.T) {
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3631)
	var terms []api.Term
	doJSON(t, "GET", base+"/terms", nil, 200, &terms)
	aes := termNamed(terms, "AES")
	if aes == nil || aes.Locked {
		t.Fatalf("AES %+v", aes)
	}
	var c api.Course
	doJSON(t, "GET", base, nil, 200, &c)
	var p api.Term
	doJSON(t, "PATCH", srv.URL+"/api/terms/"+aes.ID, map[string]any{"tier": "reference"}, 200, &p)
	if !p.Locked || p.Tier != model.TierReference || p.ZhSource != model.ZhSourceExtractor {
		t.Errorf("after tier %+v", p)
	}
	doJSON(t, "PATCH", srv.URL+"/api/terms/"+aes.ID, map[string]any{"zh": "高级加密标准"}, 200, &p)
	if p.ZhSource != model.ZhSourceUser || p.Zh != "高级加密标准" {
		t.Errorf("after zh %+v", p)
	}
	doJSON(t, "PATCH", srv.URL+"/api/terms/"+aes.ID, map[string]any{"locked": false}, 200, &p)
	if p.Locked || p.Tier != model.TierReference || p.Zh != "高级加密标准" {
		t.Errorf("after unlock %+v", p)
	}
	var c2 api.Course
	doJSON(t, "GET", base, nil, 200, &c2)
	if c2.GlossaryVersion != c.GlossaryVersion+2 {
		t.Errorf("version %d → %d (unlock must not bump it)", c.GlossaryVersion, c2.GlossaryVersion)
	}
	var e api.ErrorBody
	doJSON(t, "PATCH", srv.URL+"/api/terms/"+aes.ID, map[string]any{"locked": true}, 400, &e)
	doJSON(t, "PATCH", srv.URL+"/api/terms/"+aes.ID, map[string]any{"locked": false, "zh": "x"}, 400, &e)
	doJSON(t, "PATCH", srv.URL+"/api/terms/"+aes.ID, map[string]any{"tier": "strict"}, 400, &e)
	var added api.Term
	doJSON(t, "POST", base+"/terms", map[string]string{"en": "Kasiski examination", "zh": "卡西斯基试验"}, 201, &added)
	if !added.Locked || added.ZhSource != model.ZhSourceUser || added.Tier != model.TierEnforce {
		t.Errorf("manual %+v", added)
	}
}

// The course stage endpoint takes glossary_review (cancel, retry). The
// default timing keeps the run queued long enough to cancel it.
func TestJanitorStageActions(t *testing.T) {
	srv := testServer(t)
	base := srv.URL + "/api/courses/" + courseID(c3311)
	var res api.GlossaryReviewResult
	doJSON(t, "POST", base+"/glossary/review", nil, http.StatusAccepted, &res)
	var c api.Course
	doJSON(t, "POST", base+"/stages/glossary_review/cancel", nil, 200, &c)
	if stageOf(c.Stages, model.StageGlossaryReview) != model.StatusCancelled || c.Janitor.Running || c.GlossaryPending {
		t.Fatalf("cancelled %+v", c)
	}
	doJSON(t, "POST", base+"/stages/glossary_review/retry", nil, 200, &c)
	if stageOf(c.Stages, model.StageGlossaryReview) != model.StatusQueued || !c.Janitor.Running || !c.GlossaryPending {
		t.Fatalf("retried %+v", c)
	}
	var e api.ErrorBody
	doJSON(t, "POST", base+"/stages/glossary_review/rerun", nil, 409, &e)
}

// A merge fills the target's empty zh as its own fix_zh item (Into = the
// merged term); undoing the merge restores both, with one version bump.
func TestJanitorMergeFillAndUndo(t *testing.T) {
	fastSyllabus(t)
	root := filepath.Join("..", "..", "..")
	h := newHandler(filepath.Join(root, "web"), filepath.Join(root, "testdata", "api"), 10*time.Millisecond)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	st := h.(*handler).m.main
	cid := courseID(c3311)
	st.mu.Lock()
	c, sc, err := st.sylCourse(cid)
	if err != nil {
		st.mu.Unlock()
		t.Fatal(err)
	}
	for _, x := range []api.Term{{En: "query plan"}, {En: "query plans", Zh: "查询计划"}} {
		x.ID, x.Source, x.Enabled, x.Tier, x.ZhSource = st.newID("01JTTEST"), model.SourceMaterial, true, model.TierEnforce, model.ZhSourceExtractor
		sc.terms = append(sc.terms, x)
	}
	sortTerms(sc.terms)
	c.TermCount = len(sc.terms)
	st.mu.Unlock()

	base := srv.URL + "/api/courses/" + cid
	doJSON(t, "POST", base+"/glossary/review", nil, http.StatusAccepted, nil)
	eventually(t, "glossary_review done", func() bool {
		var x api.Course
		doJSON(t, "GET", base, nil, 200, &x)
		return stageOf(x.Stages, model.StageGlossaryReview) == model.StatusDone
	})
	var rep model.JanitorReport
	doJSON(t, "GET", base+"/glossary/report", nil, 200, &rep)
	var merge, fill *model.JanitorItem
	for i := range rep.Items {
		switch it := &rep.Items[i]; {
		case it.Action == model.JanitorMerge && it.En == "query plans":
			merge = it
		case it.Action == model.JanitorFixZh && it.En == "query plan":
			fill = it
		}
	}
	if merge == nil || fill == nil || fill.Into != merge.TermID || fill.After.Zh != "查询计划" || fill.Before.Zh != "" {
		t.Fatalf("items %+v", rep.Items)
	}
	var before api.Course
	doJSON(t, "GET", base, nil, 200, &before)
	var out model.JanitorReport
	doJSON(t, "POST", base+"/glossary/report/undo", api.JanitorUndoInput{Version: rep.Version, TermID: merge.TermID}, 200, &out)
	undone := 0
	for _, it := range out.Items {
		if (it.TermID == merge.TermID || it.TermID == fill.TermID) && it.Undone {
			undone++
		}
	}
	var terms []api.Term
	doJSON(t, "GET", base+"/terms", nil, 200, &terms)
	qp := termNamed(terms, "query plan")
	var after api.Course
	doJSON(t, "GET", base, nil, 200, &after)
	if undone != 2 || qp == nil || qp.Zh != "" || !qp.Locked || after.GlossaryVersion != before.GlossaryVersion+1 {
		t.Fatalf("after undo: %d undone, target %+v, version %d → %d", undone, qp, before.GlossaryVersion, after.GlossaryVersion)
	}
}
