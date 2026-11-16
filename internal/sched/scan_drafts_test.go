// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// BenchmarkScanDrafts measures one scheduler scan with n draft lectures
// waiting (performance audit F08: a draft's six pending jobs used to cost
// one lecture lookup each per scan). Like the audit's probe: a temporary
// file database, no stage registered, the scan called directly.
//
//	go test -run XXX -bench ScanDrafts -benchtime 20x -timeout 5m ./internal/sched/
func BenchmarkScanDrafts(b *testing.B) {
	for _, n := range []int{0, 100, 1000} {
		b.Run("drafts="+strconv.Itoa(n), func(b *testing.B) {
			s, st := benchScheduler(b)
			ctx := context.Background()
			c := model.Course{ID: db.NewID(), Name: "CS"}
			if err := st.CreateCourse(ctx, c); err != nil {
				b.Fatal(err)
			}
			for i := 0; i < n; i++ {
				l := model.Lecture{ID: db.NewID(), CourseID: c.ID, Title: "D", MediaPath: "m.mp4"}
				if err := st.CreateLecture(ctx, l); err != nil {
					b.Fatal(err)
				}
			}
			s.scan() // warm up (settings, prepared statements)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.scan()
			}
		})
	}
}

// benchScheduler is a scheduler on a temporary file database whose scan can
// be called without Start (no loop, no flusher).
func benchScheduler(tb testing.TB) (*Scheduler, db.Store) {
	return scanOnlyScheduler(tb, nil)
}

// scanOnlyScheduler is benchScheduler with the scheduler's store wrapped by
// wrap (nil: none). No stage is registered: a job that could start stays
// queued.
func scanOnlyScheduler(tb testing.TB, wrap func(db.Store) db.Store) (*Scheduler, db.Store) {
	tb.Helper()
	st, err := db.Open(filepath.Join(tb.TempDir(), "t.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	cfg := config.Default()
	g := &fakeGPU{}
	g.set(20000, 24000, 1000)
	sst := st
	if wrap != nil {
		sst = wrap(st)
	}
	s, err := New(Options{Store: sst, Config: cfg, GPUQuery: g.query, CPUSlots: 4})
	if err != nil {
		tb.Fatal(err)
	}
	s.baseCtx = context.Background()
	return s, st
}

// lookupCountingStore counts the per-job lookups of a scan.
type lookupCountingStore struct {
	db.Store
	n atomic.Int64
}

func (c *lookupCountingStore) GetLecture(ctx context.Context, id string) (model.Lecture, error) {
	c.n.Add(1)
	return c.Store.GetLecture(ctx, id)
}

func (c *lookupCountingStore) GetCurrentJob(ctx context.Context, scope, scopeID, stage string) (model.Job, error) {
	c.n.Add(1)
	return c.Store.GetCurrentJob(ctx, scope, scopeID, stage)
}

func (c *lookupCountingStore) ListCurrentJobs(ctx context.Context, scope, scopeID string) ([]model.Job, error) {
	c.n.Add(1)
	return c.Store.ListCurrentJobs(ctx, scope, scopeID)
}

// A scan does no lookup for a draft (performance audit F08): with 100
// drafts it does as many as with none. This is the deterministic side of
// BenchmarkScanDrafts' 2 ms target.
func TestScanDoesNoLookupPerDraft(t *testing.T) {
	ctx := context.Background()
	counts := map[int]int64{}
	for _, n := range []int{0, 100} {
		var cs *lookupCountingStore
		s, st := scanOnlyScheduler(t, func(st db.Store) db.Store { cs = &lookupCountingStore{Store: st}; return cs })
		c := model.Course{ID: db.NewID(), Name: "CS"}
		if err := st.CreateCourse(ctx, c); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if err := st.CreateLecture(ctx, model.Lecture{ID: db.NewID(), CourseID: c.ID, Title: "D", MediaPath: "m.mp4"}); err != nil {
				t.Fatal(err)
			}
		}
		s.scan() // warm up
		cs.n.Store(0)
		s.scan()
		counts[n] = cs.n.Load()
	}
	if counts[100] != counts[0] {
		t.Fatalf("lookups per scan: %d with 100 drafts, %d with none", counts[100], counts[0])
	}
}

// draftHeldStages holds exactly what state.Deps (and the scheduler's deps,
// which adds the "只转写" rules after it) does: for a draft, every listed
// stage waits with "not_started" whatever else holds — here even with every
// upstream job done.
func TestDraftHeldStagesMatchDeps(t *testing.T) {
	e := newEnv(t, nil)
	d := e.draft()
	for _, stg := range model.LectureStages {
		j := e.lj(d.ID, stg)
		for _, step := range [][2]string{{model.StatusPending, model.StatusQueued}, {model.StatusQueued, model.StatusRunning}, {model.StatusRunning, model.StatusDone}} {
			if err := state.TransitionJob(e.ctx, e.st, j.ID, step[0], step[1], db.JobFinish{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, stg := range draftHeldStages {
		j := model.Job{ID: db.NewID(), Scope: model.ScopeLecture, ScopeID: d.ID, Stage: stg, Status: model.StatusPending}
		ok, w, err := state.Deps(e.ctx, e.st, j)
		if err != nil || ok || w != model.WaitNotStarted {
			t.Errorf("state.Deps %s: %v %q %v", stg, ok, w, err)
		}
		ok, w, err = e.s.deps(e.ctx, j)
		if err != nil || ok || w != model.WaitNotStarted {
			t.Errorf("scheduler deps %s: %v %q %v", stg, ok, w, err)
		}
	}
}

// A draft launched between two scans is seen by the next one: its prep is
// queued then (no stage registered: it stays queued), the other drafts,
// in this and another course, stay pending with "not_started", and a
// launched lecture is scanned as before.
func TestScanSeesDraftLaunchedSinceTheLastScan(t *testing.T) {
	e := newEnv(t, nil) // no stage registered, not started: scans are called directly
	e.s.baseCtx = e.ctx
	other := model.Course{ID: db.NewID(), Name: "Math"}
	if err := e.st.CreateCourse(e.ctx, other); err != nil {
		t.Fatal(err)
	}
	d1 := e.draft()
	d2 := model.Lecture{ID: db.NewID(), CourseID: other.ID, Title: "D", MediaPath: "m.mp4"}
	if err := e.st.CreateLecture(e.ctx, d2); err != nil {
		t.Fatal(err)
	}
	l := e.lecture()
	e.s.scan()
	if got := e.lj(l.ID, model.StagePrep).Status; got != model.StatusQueued {
		t.Fatalf("launched lecture's prep %s", got)
	}
	for _, d := range []model.Lecture{d1, d2} {
		j := e.lj(d.ID, model.StagePrep)
		if j.Status != model.StatusPending || e.s.WaitingFor(j) != model.WaitNotStarted {
			t.Fatalf("draft %s prep %s %q", d.ID, j.Status, e.s.WaitingFor(j))
		}
	}
	if err := e.s.Launch(e.ctx, d2.ID); err != nil {
		t.Fatal(err)
	}
	e.s.scan()
	if j := e.lj(d2.ID, model.StagePrep); j.Status != model.StatusQueued || e.s.WaitingFor(j) != "" {
		t.Fatalf("launched draft's prep %s %q after one scan", j.Status, e.s.WaitingFor(j))
	}
	if j := e.lj(d2.ID, model.StageASR); j.Status != model.StatusPending || e.s.WaitingFor(j) != model.WaitUpstream(model.StagePrep) {
		t.Fatalf("launched draft's asr %s %q", j.Status, e.s.WaitingFor(j))
	}
	j := e.lj(d1.ID, model.StagePrep)
	if j.Status != model.StatusPending || e.s.WaitingFor(j) != model.WaitNotStarted {
		t.Fatalf("other draft's prep %s %q", j.Status, e.s.WaitingFor(j))
	}
}

// colab-batch's "work about to come" (remoteImminent) is unchanged by
// leaving the drafts out of the scan: a draft's asr job never kept the
// runtime (its prep never leaves pending), so with only drafts waiting the
// runtime is released as soon as the queue empties, long before
// RemoteImminentMax; a launched lecture in prep still keeps it
// (TestBatchImminentPrepKeepsTheRuntime).
func TestBatchImminentNotForDrafts(t *testing.T) {
	cuda := devRow(model.DeviceCUDA0, 300, 1)
	cuda.Enabled = false
	asr := newDevStage()
	be := newBatchEnv(t, []model.ASRDevice{cuda}, colabOn, func(o *Options) { o.RemoteImminentMax = time.Hour },
		asr.stage())
	be.asr = asr
	be.asr.hook = be.remoteEnv.hook
	be.power.set(onACLaptop)
	l1 := be.lectureOn(autoOn(cuda))
	be.prepDone(l1)
	for i := 0; i < 3; i++ {
		be.draft()
	}
	be.start()
	be.waitRunningOn(l1, model.DeviceColab)
	be.asr.release(l1.ID)
	be.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	be.waitFor("drained with only drafts waiting", 5*time.Second, func() bool { d, _, _, _ := be.fr.counts(); return d >= 1 })
}
