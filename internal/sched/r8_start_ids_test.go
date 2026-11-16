// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// failingReads is a Store whose lecture and material reads fail with err.
type failingReads struct {
	db.Store
	err error
}

func (f failingReads) GetLecture(context.Context, string) (model.Lecture, error) {
	return model.Lecture{}, f.err
}

func (f failingReads) GetMaterial(context.Context, string) (model.Material, error) {
	return model.Material{}, f.err
}

// SEC49 information-2: the start check lets a run through unchecked only
// when its record does not exist (the stage then fails reading it). Any
// other read error ends the run with an error too — a retryable one, not a
// permanent one: a database that is busy for a moment must not fail a good
// lecture for good; the usual automatic retry runs the check again.
func TestStartCheckOnAReadError(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	for _, scope := range []string{model.ScopeLecture, model.ScopeMaterial} {
		j := model.Job{ID: "J1", Scope: scope, ScopeID: "R1", Stage: model.StagePrep}
		busy := &Scheduler{st: failingReads{Store: e.st, err: errors.New("database is locked (SQLITE_BUSY)")}, dataDir: e.dataDir}
		if err := busy.checkScopeIDs(ctx, j); err == nil || IsPermanent(err) {
			t.Errorf("%s read fails: %v, want a retryable error", scope, err)
		}
		gone := &Scheduler{st: failingReads{Store: e.st, err: fmt.Errorf("read: %w", db.ErrNotFound)}, dataDir: e.dataDir}
		if err := gone.checkScopeIDs(ctx, j); err != nil {
			t.Errorf("%s not found: %v, want the run left to its stage", scope, err)
		}
	}
}

// R77b "not covered", SEC44 low-2: every stage reads and writes in the
// directory of its lecture, material or course, built from ids the
// database holds (an imported library supplies them). A job whose record
// has an id that names no directory of its own — "..", "x." (the directory
// of lecture "x" on Windows), a device name — fails before its stage runs
// (the scheduler's usual failure path: running → failed, not retried, the
// existing internal error), so no stage touches another record's files.
// The other lectures run as before.
func TestJobOfARecordWithAPathLikeIDFailsBeforeItsStage(t *testing.T) {
	var mu sync.Mutex
	touched := map[string]int{}
	note := func(id string) {
		mu.Lock()
		touched[id]++
		mu.Unlock()
	}
	var dataDir string
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU,
		hash: func(_ context.Context, id string) (string, error) { note(id); return "", nil },
		run: func(_ context.Context, id string, _ func(float64)) error {
			note(id)
			// what a stage does: write into the lecture's directory
			return os.WriteFile(filepath.Join(dataDir, "touched-"+strings.Trim(id, ". ")), []byte("x"), 0o644)
		}}
	extract := &fakeStage{name: model.StageMaterialExtract, res: ResourceCPU,
		run: func(_ context.Context, id string, _ func(float64)) error { note(id); return nil }}
	e := newEnv(t, nil, prep, extract)
	dataDir = e.dataDir
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	bad := []string{"..", "x.", "CON"}
	for _, id := range bad {
		e.launched(model.Lecture{ID: id, CourseID: e.course.ID, Title: "L", MediaPath: "m.mp4"})
	}
	good := e.lecture()
	badMat := model.Material{ID: "..", CourseID: e.course.ID, Filename: "a.pdf", Path: "p"}
	if err := e.st.CreateMaterial(e.ctx, badMat); err != nil {
		t.Fatal(err)
	}
	goodMat := e.material()
	first := e.job(model.ScopeMaterial, badMat.ID, model.StageMaterialExtract).Attempt
	e.start()

	e.waitStatus(good.ID, model.StagePrep, model.StatusDone)
	e.waitStatusScope(model.ScopeMaterial, goodMat.ID, model.StageMaterialExtract, model.StatusDone)
	for _, id := range bad {
		j := e.waitStatus(id, model.StagePrep, model.StatusFailed)
		if j.Error == nil || !strings.Contains(*j.Error, `"server.internal"`) {
			t.Errorf("lecture %q: error %v, want the internal error", id, j.Error)
		}
	}
	mj := e.waitStatusScope(model.ScopeMaterial, badMat.ID, model.StageMaterialExtract, model.StatusFailed)
	if mj.Attempt != first || mj.Error == nil || !strings.Contains(*mj.Error, `"server.internal"`) {
		t.Errorf("the material's job: attempt %d (first %d), error %v; want one attempt failed with the internal error", mj.Attempt, first, mj.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range bad {
		if touched[id] != 0 {
			t.Errorf("the stage ran for lecture %q (%d calls)", id, touched[id])
		}
	}
	if touched[good.ID] == 0 || touched[goodMat.ID] == 0 {
		t.Errorf("the good records' stages did not run: %v", touched)
	}
	if entries, _ := os.ReadDir(dataDir); len(entries) != 1 {
		var names []string
		for _, en := range entries {
			names = append(names, en.Name())
		}
		t.Errorf("data directory holds %v, want only the good lecture's file", names)
	}
}
