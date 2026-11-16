// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// r7 perf (P1 report H1): FindClipSource reads its candidates with the
// store's lean query (db.JobReader) instead of every done job; the
// candidates, their order and the chosen source are exactly those of the
// full listing (a store without JobReader, as before).

// noJobReader hides db.JobReader: FindClipSource falls back to
// ListCurrentJobsByStatus (the implementation before r7).
type noJobReader struct{ db.Store }

// leanWAV writes a minimal 8-bit mono WAV of sec seconds at 100 Hz (the
// header is all wav.DurationMS reads).
func leanWAV(t *testing.T, path string, sec int) {
	t.Helper()
	data := make([]byte, sec*100)
	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, uint32(36+len(data)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1)   // PCM
	b = binary.LittleEndian.AppendUint16(b, 1)   // mono
	b = binary.LittleEndian.AppendUint32(b, 100) // sample rate
	b = binary.LittleEndian.AppendUint32(b, 100) // byte rate
	b = binary.LittleEndian.AppendUint16(b, 1)   // block align
	b = binary.LittleEndian.AppendUint16(b, 8)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindClipSourceLeanMatchesFullListing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := db.Open(filepath.Join(root, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.(db.JobReader); !ok {
		t.Fatal("the SQLite store does not implement db.JobReader")
	}
	dataDir := filepath.Join(root, "data")
	full := noJobReader{st}
	compare := func(what string) (ClipSource, bool) {
		t.Helper()
		lean, err1 := clipCandidates(ctx, st)
		ref, err2 := clipCandidates(ctx, full)
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: candidates: %v / %v", what, err1, err2)
		}
		if len(lean) != len(ref) || (len(lean) > 0 && !reflect.DeepEqual(lean, ref)) {
			t.Fatalf("%s: candidates\n lean %v\n full %v", what, lean, ref)
		}
		src1, ok1, err1 := FindClipSource(ctx, st, dataDir)
		src2, ok2, err2 := FindClipSource(ctx, full, dataDir)
		if err1 != nil || err2 != nil || ok1 != ok2 || src1 != src2 {
			t.Fatalf("%s: lean %+v %v %v, full %+v %v %v", what, src1, ok1, err1, src2, ok2, err2)
		}
		return src1, ok1
	}
	if _, ok := compare("empty database"); ok {
		t.Fatal("a source in an empty database")
	}

	courses := []model.Course{{ID: "C1", Name: "A", CreatedAt: db.Now()}, {ID: "C2", Name: "B", CreatedAt: db.Now()}}
	for _, c := range courses {
		if err := st.CreateCourse(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	rng := rand.New(rand.NewSource(11))
	var lecs []model.Lecture
	for i := 0; i < 24; i++ {
		c := courses[i%2]
		l := model.Lecture{ID: fmt.Sprintf("L%02d", rng.Intn(100)) + fmt.Sprint(i), CourseID: c.ID, Title: fmt.Sprintf("Lecture %d", i),
			MediaPath: "m.mp4", CreatedAt: db.Now()}
		if err := st.CreateLecture(ctx, l); err != nil {
			t.Fatal(err)
		}
		lecs = append(lecs, l)
		// audio: none, too short (<= 2 min), exactly 2 min, long
		switch i % 4 {
		case 1:
			leanWAV(t, filepath.Join(model.LectureDir(dataDir, c.ID, l.ID), model.AudioFile), 110)
		case 2:
			leanWAV(t, filepath.Join(model.LectureDir(dataDir, c.ID, l.ID), model.AudioFile), 120)
		case 3:
			leanWAV(t, filepath.Join(model.LectureDir(dataDir, c.ID, l.ID), model.AudioFile), 600+i)
		}
	}
	// replace sets the current repair job of a lecture (status, finish):
	// the old one is superseded, as a rerun does.
	replace := func(scopeID, status string, fin *string) {
		t.Helper()
		if j, err := st.GetCurrentJob(ctx, model.ScopeLecture, scopeID, model.StageRepair); err == nil {
			if err := st.RetireJob(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.InsertJob(ctx, model.Job{ID: db.NewID(), Scope: model.ScopeLecture, ScopeID: scopeID, Stage: model.StageRepair,
			Status: status, FinishedAt: fin, IsCurrent: true, CreatedAt: db.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	compare("no repair done")
	// only superseded done repairs: no source
	fin := "2026-10-20T10:00:00Z"
	for _, l := range lecs[:6] {
		replace(l.ID, model.StatusDone, &fin)
		replace(l.ID, model.StatusPending, nil)
	}
	if _, ok := compare("only superseded done repairs"); ok {
		t.Fatal("a superseded repair counted")
	}
	// Mixed statuses and finish times (NULL, "", ties); a done repair of a
	// lecture whose row is missing; done jobs of other stages and scopes.
	fins := []*string{nil, ptr(""), ptr("2026-10-01T10:00:00Z"), ptr("2026-10-15T10:00:00Z"), ptr("2026-10-15T10:00:00Z"),
		ptr("2026-10-15T10:00:01Z"), ptr("2026-11-01T00:00:00Z")}
	statuses := []string{model.StatusDone, model.StatusDone, model.StatusDone, model.StatusStale, model.StatusFailed, model.StatusRunning}
	for _, l := range lecs {
		replace(l.ID, statuses[rng.Intn(len(statuses))], fins[rng.Intn(len(fins))])
	}
	replace("zz-deleted-lecture", model.StatusDone, ptr("2026-12-01T00:00:00Z"))
	for _, x := range []model.Job{
		{Scope: model.ScopeLecture, ScopeID: lecs[3].ID, Stage: model.StageTranslate},
		{Scope: model.ScopeCourse, ScopeID: "C1", Stage: model.StageRepair},
		{Scope: model.ScopeMaterial, ScopeID: "M1", Stage: model.StageRepair},
	} {
		x.ID, x.Status, x.FinishedAt, x.IsCurrent, x.CreatedAt = db.NewID(), model.StatusDone, ptr("2026-12-31T00:00:00Z"), true, db.Now()
		if j, err := st.GetCurrentJob(ctx, x.Scope, x.ScopeID, x.Stage); err == nil {
			if err := st.RetireJob(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.InsertJob(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	// Take away the chosen source's audio until none is left: every step
	// walks further down the same candidate order.
	found := 0
	for step := 0; ; step++ {
		src, ok := compare(fmt.Sprintf("mixed, step %d", step))
		if !ok {
			break
		}
		found++
		if step > len(lecs) {
			t.Fatal("no end")
		}
		if err := os.Remove(src.Audio); err != nil {
			t.Fatal(err)
		}
	}
	if found < 2 {
		t.Fatalf("the fixture yields %d sources; the walk needs several", found)
	}
}
