// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"math/rand"
	"net/http"
	"reflect"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// r7 perf (P1 report H1): the providers' snapshot counts come from one
// GROUP BY over the current lecture jobs (db.JobReader) instead of every
// current job; the counts are exactly those of the full listing (a store
// without JobReader, as before).

// noJobReader hides db.JobReader: snapshotRefs reads every current job
// (the implementation before r7).
type noJobReader struct{ db.Store }

func TestSnapshotRefsLeanMatchesFullListing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, ok := e.st.(db.JobReader); !ok {
		t.Fatal("the SQLite store does not implement db.JobReader")
	}
	lean, full := &Server{st: e.st}, &Server{st: noJobReader{e.st}}
	compare := func(what string) map[string]int {
		t.Helper()
		a, err1 := lean.snapshotRefs(ctx)
		b, err2 := full.snapshotRefs(ctx)
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: %v / %v", what, err1, err2)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: lean %v, full %v", what, a, b)
		}
		return a
	}
	if refs := compare("empty database"); len(refs) != 0 {
		t.Fatalf("empty database: %v", refs)
	}

	p2, err := e.llm.ConfigureSettings(ctx, e.set, "Other", model.ProviderOpenAI, "other-model")
	if err != nil {
		t.Fatal(err)
	}
	c := e.course("C")
	var lecs []Lecture
	for _, title := range []string{"L1", "L2", "L3"} {
		lecs = append(lecs, e.lecture(c.ID, title)) // LLM stages snapshot "Other"
	}
	e.allDone(lecs[0].ID)
	if refs := compare("three launched lectures"); refs[p2.ID] != 9 {
		t.Fatalf("refs %v, want 9 for %s", refs, p2.ID)
	}

	// Superseded jobs only: retire every current job of the lectures and
	// put back superseded rows that name a provider.
	retireAll := func() {
		t.Helper()
		for _, l := range lecs {
			js, err := e.st.ListCurrentJobs(ctx, model.ScopeLecture, l.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, j := range js {
				if err := e.st.RetireJob(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	retireAll()
	named := model.MarshalSnapshot(model.LLMSnapshot{ProviderID: p2.ID, Model: "other-model"})
	for _, l := range lecs {
		if err := e.st.InsertJob(ctx, model.Job{ID: db.NewID(), Scope: model.ScopeLecture, ScopeID: l.ID, Stage: model.StageRepair,
			Status: model.StatusDone, ConfigSnapshot: named, CreatedAt: db.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if refs := compare("only superseded jobs"); len(refs) != 0 {
		t.Fatalf("superseded jobs counted: %v", refs)
	}

	// Mixed: every status, LLM and other stages, usable, unusable and
	// invalid snapshots, byte-different spellings of the same snapshot, a
	// lecture job whose lecture is gone, course and material jobs.
	snaps := []string{
		model.EmptySnapshot,
		named,
		`{"provider_id":"` + p2.ID + `","model":"other-model","params":{}}`, // same provider, other bytes
		model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "deepseek", Model: "deepseek-chat"}),
		model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "gone-provider", Model: "m", Source: model.SnapshotSourceManual}),
		`{"provider_id":"deepseek"}`,           // no model: not counted
		`{"provider_id":"deepseek","model":7}`, // invalid: not counted
		`{"Provider_ID":"deepseek","Model":"x"}`,
		`not json`,
	}
	statuses := []string{model.StatusPending, model.StatusQueued, model.StatusRunning, model.StatusDone,
		model.StatusFailed, model.StatusCancelled, model.StatusStale}
	stages := []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageASR, model.StageExport, model.StagePrep}
	scopes := []string{lecs[0].ID, lecs[1].ID, lecs[2].ID}
	rng := rand.New(rand.NewSource(5))
	insert := func(scope, scopeID, stage string) {
		t.Helper()
		if err := e.st.InsertJob(ctx, model.Job{ID: db.NewID(), Scope: scope, ScopeID: scopeID, Stage: stage,
			Status: statuses[rng.Intn(len(statuses))], ConfigSnapshot: snaps[rng.Intn(len(snaps))], IsCurrent: true,
			CreatedAt: db.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, stage := range stages {
		insert(model.ScopeLecture, "gone-lecture", stage)
	}
	insert(model.ScopeCourse, c.ID, model.StageRepair)
	insert(model.ScopeMaterial, "M1", model.StageSummary)
	for round := 0; round < 5; round++ {
		retireAll()
		for _, id := range scopes {
			for _, stage := range stages {
				if rng.Intn(6) != 0 {
					insert(model.ScopeLecture, id, stage)
				}
			}
		}
		compare("mixed round")
	}

	// Through the API: every provider's snapshot_refs is the full count.
	want, err := full.snapshotRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ps []ProviderView
	e.call("GET", "/api/providers", nil, http.StatusOK, &ps)
	if len(ps) < 2 {
		t.Fatalf("providers %+v", ps)
	}
	for _, p := range ps {
		if p.SnapshotRefs != want[p.ID] {
			t.Errorf("provider %s: snapshot_refs %d, want %d", p.ID, p.SnapshotRefs, want[p.ID])
		}
	}
}
