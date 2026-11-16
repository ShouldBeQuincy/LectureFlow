// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"testing"

	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// User 2026-11-11: an Anthropic provider's translate role without a batch
// size runs settings.DefaultAnthropicBatch sentences per request. The
// default is fixed in the snapshot of the current settings; the stage reads
// only the snapshot.

// currentClaudeDeps is claudeDeps with the lecture's LLM snapshots taken
// from the current settings (settings.CurrentSnapshots, as launch writes
// them) after translate is set to params.
func (e *env) currentClaudeDeps(lectureID, modelName string, params model.RoleParams) Deps {
	e.t.Helper()
	d := e.claudeDeps(lectureID, modelName)
	tr := d.Settings.Roles()[model.RoleTranslate]
	e.must(d.Settings.SetRole(e.ctx, model.RoleTranslate, *tr.ProviderID, modelName, params))
	cur, err := d.Settings.CurrentSnapshots()
	e.must(err)
	for _, stage := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		e.setSnapshot(lectureID, stage, cur[stage])
	}
	return d
}

func (e *env) translateSnapBatch(lectureID string) *int {
	e.t.Helper()
	snap, ok := model.ParseLLMSnapshot(e.job(model.ScopeLecture, lectureID, model.StageTranslate).ConfigSnapshot)
	if !ok {
		e.t.Fatal("no translate snapshot")
	}
	return snap.Params.Batch
}

// Empty batch on Anthropic: the new snapshot says 10 and the run takes the
// batch path (14 sentences: two requests of 7).
func TestTranslateAnthropicDefaultBatch(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.overridePlainZh()
	d := e.currentClaudeDeps(l.ID, "claude-sonnet-5-5", model.RoleParams{})
	if b := e.translateSnapBatch(l.ID); b == nil || *b != settings.DefaultAnthropicBatch {
		t.Fatalf("snapshot batch %v, want %d", b, settings.DefaultAnthropicBatch)
	}
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	breqs := e.fake.BatchRequests()
	if len(breqs) != 2 || r.stats.Requests != 2 {
		t.Fatalf("%d batch requests (stats %d), want 2: %v", len(breqs), r.stats.Requests, batchTargets(breqs))
	}
	for _, q := range breqs {
		if len(q.TargetIdxs) > settings.DefaultAnthropicBatch {
			t.Errorf("batch of %d", len(q.TargetIdxs))
		}
	}
}

// One sentence per request on Anthropic: the user setting 1, and a
// snapshot written before the default (no batch) while the current setting
// now resolves to 10 (a rerun that keeps the original snapshot).
func TestTranslateAnthropicBatchOneAndOldSnapshot(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(e *env, l model.Lecture) Deps
	}{
		{"batch set to 1", func(e *env, l model.Lecture) Deps {
			d := e.currentClaudeDeps(l.ID, "claude-sonnet-5-5", model.RoleParams{Batch: ptrInt(1)})
			if b := e.translateSnapBatch(l.ID); b == nil || *b != 1 {
				t.Fatalf("snapshot batch %v, want 1", b)
			}
			return d
		}},
		{"old snapshot without batch", func(e *env, l model.Lecture) Deps {
			d := e.currentClaudeDeps(l.ID, "claude-sonnet-5-5", model.RoleParams{})
			cur, err := d.Settings.CurrentSnapshots()
			e.must(err)
			snap, _ := model.ParseLLMSnapshot(cur[model.StageTranslate])
			if snap.Params.Batch == nil || *snap.Params.Batch != settings.DefaultAnthropicBatch {
				t.Fatalf("current setting %s, want batch %d", cur[model.StageTranslate], settings.DefaultAnthropicBatch)
			}
			// the snapshot launch wrote before the default: the same without batch
			snap.Params.Batch = nil
			old := model.MarshalSnapshot(snap)
			e.setSnapshot(l.ID, model.StageTranslate, old)
			if e.translateSnapBatch(l.ID) != nil || !model.SameSnapshot(model.StageTranslate, old, cur[model.StageTranslate]) {
				t.Fatalf("old snapshot %s vs current %s", old, cur[model.StageTranslate])
			}
			return d
		}},
	} {
		e := newEnv(t)
		l := e.sampleLecture()
		e.overridePlainZh()
		d := c.setup(e, l)
		r := e.run(e.stage(d, model.StageTranslate), l.ID)
		if r.err != nil || r.job.Status != model.StatusDone {
			t.Fatalf("%s: err %v status %s", c.name, r.err, r.job.Status)
		}
		reqs := e.fake.Requests(llmtest.RoleTranslate)
		if len(reqs) != 14 || len(e.fake.BatchRequests()) != 0 || r.stats.Requests != 14 {
			t.Errorf("%s: %d requests, %d batch, stats %d; want 14 single", c.name, len(reqs), len(e.fake.BatchRequests()), r.stats.Requests)
		}
	}
}

func ptrInt(n int) *int { return &n }
