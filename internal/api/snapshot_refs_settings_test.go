// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// R8 BC-02 (R73 low 6): a provider's "old snapshot" count has two
// implementations. The settings page shows the API's snapshotRefs (one
// GROUP BY through db.JobReader, or the full listing for another store);
// the delete confirmation shows settings.SnapshotRefs (every current job).
// On the same database they must give every provider the same number, so a
// change of either side's definition fails here.
func TestSnapshotRefsMatchSettingsCount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, ok := e.st.(db.JobReader); !ok {
		t.Fatal("the SQLite store does not implement db.JobReader")
	}
	lean, full := &Server{st: e.st}, &Server{st: noJobReader{e.st}}

	p2, err := e.llm.ConfigureSettings(ctx, e.set, "Other", model.ProviderOpenAI, "other-model")
	if err != nil {
		t.Fatal(err)
	}
	// a provider no role uses: it can be deleted at the end
	name, typ, base, key := "Spare", model.ProviderOpenAI, "https://spare.invalid/v1", "sk-test-placeholder-spare"
	spare, err := e.set.CreateProvider(ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	c := e.course("C")
	var lecs []string
	for _, title := range []string{"L1", "L2", "L3", "L4"} {
		lecs = append(lecs, e.lecture(c.ID, title).ID)
	}
	e.allDone(lecs[0])

	providerIDs := func() []string {
		ids := map[string]bool{"gone-provider": true, "deepseek": true, "never-named": true, "": true}
		for _, p := range e.set.Providers() {
			ids[p.ID] = true
		}
		var out []string
		for id := range ids {
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}
	nonZero := 0
	compare := func(what string) {
		t.Helper()
		a, err1 := lean.snapshotRefs(ctx)
		b, err2 := full.snapshotRefs(ctx)
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: %v / %v", what, err1, err2)
		}
		ids := providerIDs()
		for id := range a {
			ids = append(ids, id)
		}
		for id := range b {
			ids = append(ids, id)
		}
		for _, id := range ids {
			n, err := e.set.SnapshotRefs(ctx, id)
			if err != nil {
				t.Fatalf("%s: settings.SnapshotRefs(%q): %v", what, id, err)
			}
			if n != a[id] || n != b[id] {
				t.Fatalf("%s: provider %q: settings %d, page (JobReader) %d, page (full listing) %d", what, id, n, a[id], b[id])
			}
			if n > 0 {
				nonZero++
			}
		}
	}
	compare("launched lectures")

	snaps := []string{
		model.EmptySnapshot,
		model.MarshalSnapshot(model.LLMSnapshot{ProviderID: p2.ID, Model: "other-model"}),
		`{"provider_id":"` + p2.ID + `","model":"other-model","params":{}}`, // same provider, other bytes
		model.MarshalSnapshot(model.LLMSnapshot{ProviderID: spare.ID, Model: "spare-model"}),
		`{"model":"spare-model","provider_id":"` + spare.ID + `"}`,
		model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "deepseek", Model: "deepseek-chat"}),
		model.MarshalSnapshot(model.LLMSnapshot{ProviderID: "gone-provider", Model: "m", Source: model.SnapshotSourceManual}),
		`{"provider_id":"deepseek"}`,           // no model: not counted
		`{"provider_id":"","model":"m"}`,       // no provider: not counted
		`{"provider_id":"deepseek","model":7}`, // invalid: not counted
		`{"Provider_ID":"deepseek","Model":"x"}`,
		`{"provider_id":"` + spare.ID + `","model":"m"`, // cut short
		`not json`,
		`null`,
		`[]`,
	}
	statuses := []string{model.StatusPending, model.StatusQueued, model.StatusRunning, model.StatusDone,
		model.StatusFailed, model.StatusCancelled, model.StatusStale}
	stages := []string{model.StageSummary, model.StageRepair, model.StageTranslate, model.StageASR, model.StageExport,
		model.StagePrep, model.StageMaterialTerms, model.StageMaterialExtract}
	scopes := []string{model.ScopeLecture, model.ScopeCourse, model.ScopeMaterial}
	retire := func(scope, id string) {
		t.Helper()
		js, err := e.st.ListCurrentJobs(ctx, scope, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range js {
			if err := e.st.RetireJob(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	rng := rand.New(rand.NewSource(8))
	insert := func(scope, scopeID, stage string, current bool) {
		t.Helper()
		if err := e.st.InsertJob(ctx, model.Job{ID: db.NewID(), Scope: scope, ScopeID: scopeID, Stage: stage,
			Status: statuses[rng.Intn(len(statuses))], ConfigSnapshot: snaps[rng.Intn(len(snaps))], IsCurrent: current,
			CreatedAt: db.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for round := 0; round < 40; round++ {
		// The real lectures get new current jobs (the old ones superseded);
		// lectures without a row, the course and materials get fresh ids.
		for _, id := range lecs {
			if rng.Intn(3) == 0 {
				continue // keeps what it had
			}
			retire(model.ScopeLecture, id)
			for _, stage := range stages {
				if rng.Intn(4) != 0 {
					insert(model.ScopeLecture, id, stage, true)
				}
			}
		}
		for i := 0; i < 1+rng.Intn(4); i++ {
			scope := scopes[rng.Intn(len(scopes))]
			id := fmt.Sprintf("%s-%d-%d", scope, round, i) // no lecture, course or material row
			for _, stage := range stages {
				switch rng.Intn(3) {
				case 0:
					insert(scope, id, stage, true)
				case 1:
					insert(scope, id, stage, false) // superseded only
				}
			}
		}
		compare(fmt.Sprintf("round %d", round))
	}
	if nonZero == 0 {
		t.Fatal("no provider was ever counted: the comparison proves nothing")
	}

	// Through the API: the page's number for the spare provider is the
	// number its delete returns.
	var ps []ProviderView
	e.call("GET", "/api/providers", nil, http.StatusOK, &ps)
	shown := -1
	for _, p := range ps {
		if p.ID == spare.ID {
			shown = p.SnapshotRefs
		}
	}
	if shown <= 0 {
		t.Fatalf("spare provider shown with %d snapshot refs (want some): %+v", shown, ps)
	}
	var del ProviderDeleteResult
	e.call("DELETE", "/api/providers/"+spare.ID, nil, http.StatusOK, &del)
	if del.SnapshotRefs != shown {
		t.Fatalf("delete says %d snapshot refs, the page showed %d", del.SnapshotRefs, shown)
	}
}
