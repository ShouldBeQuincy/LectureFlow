// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// v3.2.x r7 (T25 S1): usage_total, only in GET /api/lectures/{id}?usage_total=1
// (the lecture page asks for it). "Joins and Subqueries" has an earlier run
// (more than its usage); other lectures have usage_total = usage; a rerun
// lowers usage but not usage_total; without the parameter, in action
// responses and in lists the field is left out.
func TestR7UsageTotal(t *testing.T) {
	srv := testServer(t)
	id := lecID(c3311, 3)
	var l api.Lecture
	doJSON(t, "GET", srv.URL+"/api/lectures/"+id+"?mock=1&usage_total=1", nil, 200, &l)
	u, tot := l.Usage, l.UsageTotal
	if tot == nil || tot.PromptTokens <= u.PromptTokens || tot.EstimatedCost == nil || u.EstimatedCost == nil || *tot.EstimatedCost <= *u.EstimatedCost {
		t.Fatalf("Joins and Subqueries: usage %+v, usage_total %+v; want a larger total", u, tot)
	}
	var other api.Lecture
	doJSON(t, "GET", srv.URL+"/api/lectures/"+lecID(c3311, 1)+"?mock=1&usage_total=1", nil, 200, &other)
	if ot := other.UsageTotal; ot == nil || ot.PromptTokens != other.Usage.PromptTokens || ot.CompletionTokens != other.Usage.CompletionTokens {
		t.Errorf("a lecture without history: usage %+v, usage_total %+v; want equal", other.Usage, ot)
	}
	hasTotal := func(method, url string, body any) bool {
		t.Helper()
		code, b := rawCall(t, method, url, body)
		if code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, url, code, b)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		_, ok := m["usage_total"]
		return ok
	}
	if hasTotal("GET", srv.URL+"/api/lectures/"+id+"?mock=1", nil) {
		t.Errorf("GET without usage_total=1 carries usage_total")
	}
	if hasTotal("POST", srv.URL+"/api/lectures/"+id+"/stages/translate/rerun?mock=1", api.ModelChoiceRequest{ModelChoice: model.ModelChoiceSnapshot}) {
		t.Errorf("the rerun's response carries usage_total")
	}
	var after api.Lecture
	doJSON(t, "GET", srv.URL+"/api/lectures/"+id+"?mock=1&usage_total=1", nil, 200, &after)
	if after.Usage.PromptTokens >= u.PromptTokens {
		t.Errorf("the rerun did not lower usage: %d → %d", u.PromptTokens, after.Usage.PromptTokens)
	}
	if at := after.UsageTotal; at == nil || at.PromptTokens != tot.PromptTokens || at.CompletionTokens != tot.CompletionTokens {
		t.Errorf("usage_total changed with the rerun: %+v → %+v", tot, at)
	}

	_, b := rawCall(t, "GET", srv.URL+"/api/courses/"+courseID(c3311)+"/lectures?mock=1", nil)
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil || len(items) == 0 {
		t.Fatalf("lecture list: %v", err)
	}
	for _, it := range items {
		if _, ok := it["usage_total"]; ok {
			t.Fatalf("the lecture list carries usage_total")
		}
	}
}

// v3.2.x r7 (T24 S2): the course list, a course and the dashboard's course
// cards count the failed lectures like the real back end (failed_count).
func TestR7CourseFailedCount(t *testing.T) {
	srv := testServer(t)
	var courses []api.Course
	doJSON(t, "GET", srv.URL+"/api/courses?mock=1", nil, 200, &courses)
	want := map[string]int{"COMP2120": 1, "CSC1002": 1} // Deadlocks (translate), RISC-V guest talk (prep)
	byID := map[string]int{}
	for _, c := range courses {
		code := deref(c.Code)
		if c.FailedCount != want[code] {
			t.Errorf("GET /api/courses %s: failed_count = %d, want %d", code, c.FailedCount, want[code])
		}
		byID[c.ID] = c.FailedCount
		var one api.Course
		doJSON(t, "GET", srv.URL+"/api/courses/"+c.ID+"?mock=1", nil, 200, &one)
		if one.FailedCount != c.FailedCount {
			t.Errorf("GET /api/courses/%s: failed_count = %d, list says %d", code, one.FailedCount, c.FailedCount)
		}
	}
	var d api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard?mock=1", nil, 200, &d)
	for _, cc := range d.Courses {
		if cc.FailedCount != byID[cc.ID] {
			t.Errorf("dashboard card %s: failed_count = %d, want %d", deref(cc.Code), cc.FailedCount, byID[cc.ID])
		}
	}
}

// v3.2.x r7 (T24 S2): earlier_failed_total. In the default data both
// failures are recent (0); in "old_failures" the Deadlocks failure is three
// days old: it leaves the 24-hour group and is counted instead.
func TestR7EarlierFailedTotal(t *testing.T) {
	srv := testServer(t)
	failedSum := func(d api.Dashboard) int {
		n := 0
		for _, c := range d.Courses {
			n += c.FailedCount
		}
		return n
	}
	lectures := func(d api.Dashboard) int {
		seen := map[string]bool{}
		for _, f := range d.Failures {
			seen[f.LectureID] = true
		}
		return len(seen)
	}
	var d api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard?mock=1", nil, 200, &d)
	if d.EarlierFailedTotal != 0 || lectures(d) != 2 || failedSum(d) != 2 {
		t.Errorf("mock=1: earlier_failed_total %d, failures of %d lectures, failed_count sum %d; want 0, 2, 2",
			d.EarlierFailedTotal, lectures(d), failedSum(d))
	}
	var o api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard?mock=old_failures", nil, 200, &o)
	if o.EarlierFailedTotal != 1 || lectures(o) != 1 || failedSum(o) != 2 {
		t.Errorf("old_failures: earlier_failed_total %d, failures of %d lectures, failed_count sum %d; want 1, 1, 2",
			o.EarlierFailedTotal, lectures(o), failedSum(o))
	}
	for _, f := range o.Failures {
		if f.Stage == "translate" {
			t.Errorf("old_failures: the three-day-old translate failure is still listed: %+v", f)
		}
	}
	var e api.Dashboard
	doJSON(t, "GET", srv.URL+"/api/dashboard?mock=empty", nil, 200, &e)
	if e.EarlierFailedTotal != 0 {
		t.Errorf("empty: earlier_failed_total %d", e.EarlierFailedTotal)
	}
}
