// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"testing"

	"lectureflow/internal/model"
)

// User 2026-11-11: a snapshot of the current settings on an Anthropic
// provider now names batch 10 when the role leaves it empty. A lecture
// launched before that (translate snapshot without batch) must not be told
// that its settings changed (A4.3 rerun decision): the batch size is not
// compared, so the old snapshot is kept and still runs one sentence per
// request.
func TestSnapshotDiffsIgnoreDefaultBatch(t *testing.T) {
	old := map[string]string{
		model.StageASR:       asrSnap("large-v2"),
		model.StageSummary:   llmSnap("anthropic", "claude-x", 0.2),
		model.StageRepair:    llmSnap("anthropic", "claude-x", 0.1),
		model.StageTranslate: llmSnap("anthropic", "claude-x", 0.3, 3),
	}
	var jobs []model.Job
	for _, st := range model.LectureStages {
		jobs = append(jobs, model.Job{Scope: model.ScopeLecture, Stage: st, Status: model.StatusDone, ConfigSnapshot: mergeSnaps(old)[st]})
	}
	for _, n := range []int{10, 1} {
		cur := mergeSnaps(old)
		s, _ := model.ParseLLMSnapshot(cur[model.StageTranslate])
		s.Params.Batch = &n
		cur[model.StageTranslate] = model.MarshalSnapshot(s)
		for _, st := range model.LectureStages {
			if d := SnapshotDiffs(jobs, st, cur, nil); d != nil {
				t.Errorf("batch %d, rerun %s: %+v", n, st, d)
			}
		}
	}
}
