// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"lectureflow/internal/model"
)

// T25 S1: the lecture's usage keeps its meaning (the current stages' latest
// jobs), and usage_total adds up every job the lecture ever had — a rerun's
// retired job included — so the amount never shrinks after a rerun. Its
// query scans the job table, so only GET /api/lectures/{id}?usage_total=1
// computes it (the lecture page asks for it); every other response leaves
// the field out.
func TestLectureUsageTotal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("Operating Systems")
	l := e.lecture(c.ID, "Deadlocks")
	e.allDone(l.ID)
	setStats := func(stage string, st model.JobStats) {
		t.Helper()
		j, err := e.st.GetCurrentJob(ctx, model.ScopeLecture, l.ID, stage)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.st.SetJobStats(ctx, j.ID, st); err != nil {
			t.Fatal(err)
		}
	}
	setStats(model.StageSummary, model.JobStats{PromptTokens: 100, CacheHitTokens: 40, CompletionTokens: 10, EstimatedCost: 0.01})
	setStats(model.StageTranslate, model.JobStats{PromptTokens: 1000, CacheHitTokens: 500, CompletionTokens: 200, EstimatedCost: 0.20})

	withTotal := "/api/lectures/" + l.ID + "?usage_total=1"
	var lv Lecture
	e.call("GET", withTotal, nil, http.StatusOK, &lv)
	if lv.UsageTotal == nil || lv.UsageTotal.PromptTokens != 1100 || !near(lv.UsageTotal.EstimatedCost, 0.21) ||
		lv.Usage.PromptTokens != 1100 || !near(lv.Usage.EstimatedCost, 0.21) {
		t.Fatalf("before the rerun: usage %+v, usage_total %+v; want both 1100 tokens, 0.21", lv.Usage, lv.UsageTotal)
	}

	// A rerun replaces the translate job: the new one has used little so far.
	// The action's response does not compute the total, even when asked:
	// only GET does (R77 info-4).
	_, b := e.do("POST", "/api/lectures/"+l.ID+"/stages/translate/rerun?usage_total=1", nil)
	if jsonHasKey(t, b, "usage_total") {
		t.Errorf("the rerun's response (POST ?usage_total=1) carries usage_total")
	}
	setStats(model.StageTranslate, model.JobStats{PromptTokens: 70, CacheHitTokens: 0, CompletionTokens: 7, EstimatedCost: 0.003})

	e.call("GET", withTotal, nil, http.StatusOK, &lv)
	if lv.Usage.PromptTokens != 170 || lv.Usage.CompletionTokens != 17 || !near(lv.Usage.EstimatedCost, 0.013) {
		t.Errorf("usage (latest jobs) = %+v, want 170 / 17 tokens, 0.013", lv.Usage)
	}
	u := lv.UsageTotal
	if u == nil || u.PromptTokens != 1170 || u.CacheHitTokens != 540 || u.CompletionTokens != 217 || !near(u.EstimatedCost, 0.213) {
		t.Fatalf("usage_total = %+v, want 1170 / 540 / 217 tokens, 0.213", u)
	}

	// Without the parameter (the SSE refreshes of the course page), and with
	// another value, the lecture is read without the history query.
	for _, p := range []string{"/api/lectures/" + l.ID, "/api/lectures/" + l.ID + "?usage_total=0"} {
		_, b = e.do("GET", p, nil)
		if jsonHasKey(t, b, "usage_total") {
			t.Errorf("GET %s carries usage_total", p)
		}
	}
	// Lists and the dashboard leave it out.
	_, b = e.do("GET", "/api/courses/"+c.ID+"/lectures", nil)
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil || len(items) != 1 {
		t.Fatalf("lecture list: %v %s", err, b)
	}
	if _, ok := items[0]["usage_total"]; ok {
		t.Errorf("the lecture list carries usage_total: %s", items[0]["usage_total"])
	}
	_, b = e.do("GET", "/api/dashboard", nil)
	var d struct {
		RecentDone, InProgress []map[string]json.RawMessage
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	for _, x := range append(d.RecentDone, d.InProgress...) {
		if _, ok := x["usage_total"]; ok {
			t.Errorf("a dashboard lecture carries usage_total")
		}
	}
}

// Without any price usage_total has tokens and a null estimated_cost, like
// usage.
func TestLectureUsageTotalUnpriced(t *testing.T) {
	e := newEnv(t)
	c := e.course("Operating Systems")
	l := e.lecture(c.ID, "Deadlocks")
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID+"?usage_total=1", nil, http.StatusOK, &lv)
	if lv.UsageTotal == nil || lv.UsageTotal.PromptTokens != 0 || (lv.UsageTotal.EstimatedCost == nil) != (lv.Usage.EstimatedCost == nil) {
		t.Fatalf("fresh lecture: usage %+v, usage_total %+v", lv.Usage, lv.UsageTotal)
	}
}

func near(p *float64, want float64) bool {
	return p != nil && math.Abs(*p-want) < 1e-9
}
