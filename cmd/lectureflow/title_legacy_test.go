// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"path/filepath"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// TestBackfillKeepsPreV2UserTitle is test-upgrade F7 on a newly upgraded
// database: a lecture uploaded before v2 with a title typed in the form
// (title_source the migration default filename, no original_filename) gets
// its title recorded by the backfill, and the title task that runs after
// its summary keeps it (the blurbs are written).
func TestBackfillKeepsPreV2UserTitle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := db.Open(filepath.Join(root, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(root, "data")
	fake := llmtest.New(t)
	fake.Configure(cfg) // never the real API
	cfg.LLM.Roles[model.RoleOrganizer] = cfg.LLM.Roles[model.RoleSummary]

	c := model.Course{ID: db.NewID(), Name: "CS", CreatedAt: db.Now()}
	if err := st.CreateCourse(ctx, c); err != nil {
		t.Fatal(err)
	}
	now := db.Now()
	l := model.Lecture{ID: db.NewID(), CourseID: c.ID, Title: "Week 2 - Hazards", MediaPath: "media.mp4",
		CreatedAt: now, LaunchedAt: &now}
	if err := st.CreateLecture(ctx, l); err != nil {
		t.Fatal(err)
	}
	if _, err := backfillOriginalFilenames(ctx, st); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := st.GetKV(ctx, stages.KVLegacyTitlePrefix+l.ID); v != "Week 2 - Hazards" {
		t.Fatalf("legacy title record = %q", v)
	}

	// the summary is done; the title task runs
	if err := st.UpsertSummary(ctx, model.Summary{LectureID: l.ID, Text: "This lecture introduces pipeline hazards.", InputHash: "x"}); err != nil {
		t.Fatal(err)
	}
	j := model.Job{ID: db.NewID(), Scope: model.AuxScope(model.StageTitle), ScopeID: l.ID, Stage: model.StageTitle,
		Status: model.StatusPending, IsCurrent: true, CreatedAt: db.Now()}
	if err := st.InsertJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{model.StatusPending, model.StatusQueued}, {model.StatusQueued, model.StatusRunning}} {
		if err := state.TransitionJob(ctx, st, j.ID, step[0], step[1], db.JobFinish{}); err != nil {
			t.Fatal(err)
		}
	}
	d := stages.Deps{Store: st, Cfg: cfg, LLM: llm.New(cfg, nil, "", nil), DataDir: cfg.DataDir,
		Invalidate: func(context.Context, string, state.StaleCause) error { return nil }}
	ran := false
	for _, stg := range stages.All(d) {
		if stg.Name() == model.StageTitle {
			if err := stg.Run(ctx, l.ID, func(float64) {}); err != nil {
				t.Fatal(err)
			}
			ran = true
		}
	}
	if !ran {
		t.Fatal("no title stage")
	}
	if n := len(fake.Requests(llmtest.RoleTitle)); n != 1 {
		t.Fatalf("%d title requests", n)
	}
	got, err := st.GetLecture(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Week 2 - Hazards" || got.TitleZh != nil || got.TitleSource != model.TitleSourceFilename {
		t.Errorf("user title rewritten: %q (%s)", got.Title, got.TitleSource)
	}
	if got.BlurbEn == nil || *got.BlurbEn == "" {
		t.Error("blurb not written")
	}
}
