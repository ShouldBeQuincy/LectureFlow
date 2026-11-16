// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// ---------- fakes ----------

type fakeGPU struct {
	mu    sync.Mutex
	mem   GPUMem
	err   error
	calls int
}

func (g *fakeGPU) query(ctx context.Context) (GPUMem, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.mem, g.err
}

func (g *fakeGPU) set(free, total, used int) {
	g.mu.Lock()
	g.mem = GPUMem{FreeMB: free, TotalMB: total, UsedMB: used}
	g.err = nil
	g.mu.Unlock()
}

// waitSampledPeak is for a fake GPU stage's run: it returns once the VRAM
// sampler recorded at least mb over the baseline for the running GPU job of
// scopeID. A fixed sleep inside the run could see no sample at all on a
// loaded machine. An error after 5 s (the caller's run returns it).
func (e *env) waitSampledPeak(scopeID string, mb int) error {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		e.s.mu.Lock()
		got := false
		for _, r := range e.s.running {
			if r.job.ScopeID == scopeID && r.res == ResourceGPU && r.peakMB >= mb {
				got = true
			}
		}
		e.s.mu.Unlock()
		if got {
			return nil
		}
	}
	return fmt.Errorf("no VRAM sample of %d MB for %s", mb, scopeID)
}

type recEvents struct {
	mu      sync.Mutex
	jobs    []model.Job
	changed []string
}

func (r *recEvents) Job(j model.Job) {
	r.mu.Lock()
	r.jobs = append(r.jobs, j)
	r.mu.Unlock()
}

func (r *recEvents) Changed(kind, id string) {
	r.mu.Lock()
	r.changed = append(r.changed, kind+":"+id)
	r.mu.Unlock()
}

type fakeStage struct {
	name     string
	res      ResourceKind
	provider string
	hash     func(ctx context.Context, id string) (string, error)
	run      func(ctx context.Context, id string, progress func(float64)) error
}

func (f *fakeStage) Name() string           { return f.name }
func (f *fakeStage) Resource() ResourceKind { return f.res }
func (f *fakeStage) Provider() string {
	if f.provider == "" {
		return "deepseek"
	}
	return f.provider
}
func (f *fakeStage) InputHash(ctx context.Context, id string) (string, error) {
	if f.hash == nil {
		return "", nil
	}
	return f.hash(ctx, id)
}
func (f *fakeStage) Run(ctx context.Context, id string, progress func(float64)) error {
	if f.run == nil {
		return nil
	}
	return f.run(ctx, id, progress)
}

func instant(name string, res ResourceKind) *fakeStage { return &fakeStage{name: name, res: res} }

// gate counts concurrent runs and blocks them until released (or cancelled).
type gate struct {
	mu      sync.Mutex
	cur     int
	max     int
	runs    int
	release chan struct{}
}

func newGate() *gate { return &gate{release: make(chan struct{})} }

func (g *gate) run(ctx context.Context, id string, progress func(float64)) error {
	g.mu.Lock()
	g.cur++
	g.runs++
	if g.cur > g.max {
		g.max = g.cur
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.cur--
		g.mu.Unlock()
	}()
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) stats() (cur, max, runs int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cur, g.max, g.runs
}

// ---------- environment ----------

type env struct {
	t       *testing.T
	ctx     context.Context
	st      db.Store
	cfg     *config.Config
	s       *Scheduler
	gpu     *fakeGPU
	ev      *recEvents
	dataDir string
	course  model.Course
	optMut  func(*Options)
}

func newEnv(t *testing.T, mut func(*config.Config), stages ...Stage) *env {
	t.Helper()
	return newEnvOpts(t, mut, nil, stages...)
}

func newEnvOpts(t *testing.T, mut func(*config.Config), optMut func(*Options), stages ...Stage) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ASR.MaxParallel = 1
	// A fake key so the legacy provider imported by settings.Open is usable
	// whatever the machine's environment (v1.1 A4.3 snapshot usability).
	dp := cfg.LLM.Providers["deepseek"]
	dp.APIKey = "sk-test-not-a-real-key"
	cfg.LLM.Providers["deepseek"] = dp
	if mut != nil {
		mut(cfg)
	}
	e := &env{t: t, ctx: context.Background(), st: st, cfg: cfg, gpu: &fakeGPU{}, ev: &recEvents{}, dataDir: filepath.Join(dir, "data"), optMut: optMut}
	e.gpu.set(20000, 24000, 1000)
	e.s = e.newScheduler(stages...)
	e.course = model.Course{ID: db.NewID(), Name: "CS"}
	if err := st.CreateCourse(e.ctx, e.course); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.s.Stop()
		st.Close()
	})
	return e
}

func (e *env) newScheduler(stages ...Stage) *Scheduler {
	m := map[string]Stage{}
	for _, s := range stages {
		m[s.Name()] = s
	}
	o := Options{
		Store: e.st, Config: e.cfg, Stages: m, Events: e.ev, DataDir: e.dataDir, GPUQuery: e.gpu.query,
		ScanInterval: 5 * time.Millisecond, VRAMSampleInterval: 5 * time.Millisecond,
		RetryBackoff: []time.Duration{60 * time.Millisecond, 120 * time.Millisecond},
		CPUSlots:     4, CancelWait: 3 * time.Second, ProgressMinInterval: 5 * time.Millisecond,
		IncrementalDebounce: 60 * time.Millisecond,
	}
	if e.optMut != nil {
		e.optMut(&o)
	}
	s, err := New(o)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) start() {
	if err := e.s.Start(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

// lecture creates a launched lecture (v1.1 A3: v1 lectures started on
// upload) whose jobs carry the snapshots of the current settings.
func (e *env) lecture() model.Lecture {
	e.t.Helper()
	return e.launched(model.Lecture{ID: db.NewID(), CourseID: e.course.ID, Title: "L", MediaPath: "m.mp4"})
}

// draft creates a draft lecture (launched_at IS NULL, v1.1 A3.1).
func (e *env) draft() model.Lecture {
	e.t.Helper()
	l := model.Lecture{ID: db.NewID(), CourseID: e.course.ID, Title: "D", MediaPath: "m.mp4"}
	if err := e.st.CreateLecture(e.ctx, l); err != nil {
		e.t.Fatal(err)
	}
	return l
}

// launched inserts l and launches it with the current settings' snapshots
// (state.Launch directly: the fixture does not depend on Scheduler.Launch).
func (e *env) launched(l model.Lecture) model.Lecture {
	e.t.Helper()
	if err := e.st.CreateLecture(e.ctx, l); err != nil {
		e.t.Fatal(err)
	}
	snaps, err := e.s.set.CurrentSnapshots()
	if err != nil {
		e.t.Fatal(err)
	}
	if err := state.Launch(e.ctx, e.st, l.ID, snaps); err != nil {
		e.t.Fatal(err)
	}
	got, err := e.st.GetLecture(e.ctx, l.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *env) job(scope, id, stage string) model.Job {
	e.t.Helper()
	j, err := e.st.GetCurrentJob(e.ctx, scope, id, stage)
	if err != nil {
		e.t.Fatalf("job %s/%s: %v", id, stage, err)
	}
	return j
}

func (e *env) lj(id, stage string) model.Job { return e.job(model.ScopeLecture, id, stage) }

func (e *env) waitFor(what string, timeout time.Duration, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

func (e *env) waitStatus(id, stage, status string) model.Job {
	e.t.Helper()
	var j model.Job
	e.waitFor(fmt.Sprintf("%s %s=%s", id, stage, status), 5*time.Second, func() bool {
		j = e.lj(id, stage)
		return j.Status == status
	})
	return j
}

func gpuStages(asr *gate) []Stage {
	return []Stage{instant(model.StagePrep, ResourceCPU), &fakeStage{name: model.StageASR, res: ResourceGPU, run: asr.run}}
}

// prepDone completes the lectures' prep before the scheduler starts, so all
// their asr jobs become eligible in the same scan and the GPU slots go out
// in FIFO order. Otherwise whichever instant prep happens to finish first
// takes the slot, and a test that expects a particular lecture to wait is
// flaky under CPU load.
func (e *env) prepDone(ls ...model.Lecture) {
	e.t.Helper()
	for _, l := range ls {
		id := e.lj(l.ID, model.StagePrep).ID
		for _, step := range [][2]string{{model.StatusPending, model.StatusQueued}, {model.StatusQueued, model.StatusRunning}, {model.StatusRunning, model.StatusDone}} {
			if err := state.TransitionJob(e.ctx, e.st, id, step[0], step[1], db.JobFinish{}); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

// ---------- GPU pool ----------

func TestGPUMaxParallel(t *testing.T) {
	g := newGate()
	e := newEnv(t, func(c *config.Config) { c.ASR.MaxParallel = 2 }, gpuStages(g)...)
	ls := []model.Lecture{e.lecture(), e.lecture(), e.lecture()}
	e.prepDone(ls...)
	e.start()
	e.waitFor("2 asr running", 5*time.Second, func() bool { c, _, _ := g.stats(); return c == 2 })
	// A scan after both started refuses the third for the slot (not a fixed
	// sleep: on a loaded machine the scan that started them may not have
	// ended 60 ms later, and its waiting_for is not out yet).
	var third model.Job
	e.waitFor("the third asr waiting for a slot", 5*time.Second, func() bool {
		third = e.lj(ls[2].ID, model.StageASR)
		return e.s.WaitingFor(third) != ""
	})
	if c, m, _ := g.stats(); c != 2 || m != 2 {
		t.Fatalf("running=%d max=%d, want 2", c, m)
	}
	if third.Status != model.StatusQueued || e.s.WaitingFor(third) != model.WaitGPUSlot {
		t.Fatalf("third asr: %s waiting_for=%q", third.Status, e.s.WaitingFor(third))
	}
	if gs := e.s.GPUStatus(e.ctx); gs.SlotsUsed != 2 || gs.SlotsMax != 2 || !gs.OK {
		t.Fatalf("gpu status = %+v", gs)
	}
	close(g.release)
	for _, l := range ls {
		e.waitStatus(l.ID, model.StageASR, model.StatusDone)
	}
	if _, m, runs := g.stats(); m != 2 || runs != 3 {
		t.Fatalf("max=%d runs=%d", m, runs)
	}
}

func TestGPUVRAMShortageQueues(t *testing.T) {
	g := newGate()
	e := newEnv(t, func(c *config.Config) {
		c.ASR.MaxParallel = 2
		c.ASR.VRAMPerJobMB = 5000
		c.ASR.VRAMReserveMB = 1000
	}, gpuStages(g)...)
	e.gpu.set(5500, 24000, 18500) // 5500 - 1000 < 5000
	l1, l2 := e.lecture(), e.lecture()
	e.start()
	e.waitFor("asr queued", 5*time.Second, func() bool { return e.lj(l2.ID, model.StageASR).Status == model.StatusQueued })
	e.waitFor("vram reason", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(l1.ID, model.StageASR)) == model.WaitVRAM && e.s.WaitingFor(e.lj(l2.ID, model.StageASR)) == model.WaitVRAM
	})
	if _, _, runs := g.stats(); runs != 0 {
		t.Fatal("asr started without enough VRAM")
	}
	// Enough for one job: the second must keep waiting even though
	// max_parallel = 2, because the first job's estimate is reserved.
	e.gpu.set(10000, 24000, 14000)
	e.waitStatus(l1.ID, model.StageASR, model.StatusRunning)
	time.Sleep(60 * time.Millisecond)
	j2 := e.lj(l2.ID, model.StageASR)
	if j2.Status != model.StatusQueued || e.s.WaitingFor(j2) != model.WaitVRAM {
		t.Fatalf("second asr: %s %q", j2.Status, e.s.WaitingFor(j2))
	}
	close(g.release)
	e.waitStatus(l2.ID, model.StageASR, model.StatusDone)
}

func TestGPUPause(t *testing.T) {
	g := newGate()
	e := newEnv(t, nil, gpuStages(g)...)
	if err := e.s.SetGPUPaused(true, false); err != nil {
		t.Fatal(err)
	}
	l := e.lecture()
	e.start()
	e.waitFor("gpu_paused", 5*time.Second, func() bool { return e.s.WaitingFor(e.lj(l.ID, model.StageASR)) == model.WaitGPUPaused })
	// Persisted: a new scheduler starts paused.
	if s2 := e.newScheduler(); !s2.GPUPaused() {
		t.Fatal("pause flag not persisted in kv")
	}
	e.s.SetGPUPaused(false, false)
	e.waitStatus(l.ID, model.StageASR, model.StatusRunning)
	// Pausing with cancel_running cancels the running GPU job.
	if err := e.s.SetGPUPaused(true, true); err != nil {
		t.Fatal(err)
	}
	if j := e.lj(l.ID, model.StageASR); j.Status != model.StatusCancelled {
		t.Fatalf("asr = %s after pause+cancel", j.Status)
	}
	if v, _, _ := e.st.GetKV(e.ctx, model.KVGPUPaused); v != "1" {
		t.Fatalf("kv gpu_paused = %q", v)
	}
}

func TestGPUQueryFailureAllowsOne(t *testing.T) {
	g := newGate()
	e := newEnv(t, func(c *config.Config) { c.ASR.MaxParallel = 3 }, gpuStages(g)...)
	e.gpu.mu.Lock()
	e.gpu.err = errors.New("nvidia-smi not found")
	e.gpu.mu.Unlock()
	l1, l2 := e.lecture(), e.lecture()
	e.prepDone(l1, l2)
	e.start()
	e.waitStatus(l1.ID, model.StageASR, model.StatusRunning)
	// "running" is committed before the stage goroutine enters Run.
	e.waitFor("asr inside Run", 5*time.Second, func() bool { c, _, _ := g.stats(); return c == 1 })
	e.waitFor("gpu_slot", 5*time.Second, func() bool { return e.s.WaitingFor(e.lj(l2.ID, model.StageASR)) == model.WaitGPUSlot })
	if _, m, _ := g.stats(); m != 1 {
		t.Fatalf("max concurrent = %d with failed nvidia-smi", m)
	}
	close(g.release)
	e.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	if _, m, _ := g.stats(); m != 1 {
		t.Fatalf("max concurrent = %d", m)
	}
}

func TestFakeASRModeHasOneSlotAndNoGPUQuery(t *testing.T) {
	g := newGate()
	e := newEnv(t, func(c *config.Config) {
		c.ASR.FakeResult = "testdata/fake_asr.json"
		c.ASR.MaxParallel = 3
	}, gpuStages(g)...)
	e.gpu.mu.Lock()
	e.gpu.calls = 0
	e.gpu.mu.Unlock()
	l1, l2 := e.lecture(), e.lecture()
	e.prepDone(l1, l2)
	e.start()
	e.waitStatus(l1.ID, model.StageASR, model.StatusRunning)
	time.Sleep(40 * time.Millisecond)
	if j := e.lj(l2.ID, model.StageASR); j.Status != model.StatusQueued {
		t.Fatalf("second asr = %s", j.Status)
	}
	close(g.release)
	e.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	e.gpu.mu.Lock()
	calls := e.gpu.calls
	e.gpu.mu.Unlock()
	if calls != 0 {
		t.Fatalf("nvidia-smi queried %d times in fake ASR mode", calls)
	}
	if _, m, _ := g.stats(); m != 1 {
		t.Fatalf("max concurrent = %d", m)
	}
}

func TestVRAMCalibration(t *testing.T) {
	var e *env
	asr := &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(ctx context.Context, id string, p func(float64)) error {
		e.gpu.set(14000, 24000, 7000) // +6000 MB over the 1000 MB baseline
		err := e.waitSampledPeak(id, 6000)
		e.gpu.set(20000, 24000, 1000)
		return err
	}}
	e = newEnv(t, nil, instant(model.StagePrep, ResourceCPU), asr)
	l := e.lecture()
	e.start()
	j := e.waitStatus(l.ID, model.StageASR, model.StatusDone)
	key := model.KVVRAMObservedPrefix + "large-v3:float16"
	if v, ok, _ := e.st.GetKV(e.ctx, key); !ok || v != "6000" {
		t.Fatalf("%s = %q (ok=%v)", key, v, ok)
	}
	if j.Stats.VRAMPeakMB != 6000 {
		t.Fatalf("stats.vram_peak_mb = %d", j.Stats.VRAMPeakMB)
	}
	if est := e.s.GPUStatus(e.ctx).EstimatedJobMB; est != 6600 {
		t.Fatalf("estimate = %d, want max(5000, 6000*1.1)=6600", est)
	}
	// A smaller observation never lowers the stored maximum.
	e.s.recordVRAM(&run{sample: true, peakMB: 3000, computeType: "float16"})
	if v, _, _ := e.st.GetKV(e.ctx, key); v != "6000" {
		t.Fatalf("kv lowered to %s", v)
	}
}

// ---------- API pool ----------

func TestAPIPoolConcurrencyAndRate(t *testing.T) {
	cfg := config.Default()
	p := cfg.LLM.Providers["deepseek"]
	p.MaxConcurrency, p.RequestsPerMinute = 3, 1200 // 20 req/s, burst 20
	cfg.LLM.Providers["deepseek"] = p
	pool := NewPool(cfg, nil, PoolOptions{})
	var inflight, maxIn int32
	const n = 50
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := pool.Acquire(context.Background(), "deepseek")
			if err != nil {
				t.Error(err)
				return
			}
			c := atomic.AddInt32(&inflight, 1)
			for {
				m := atomic.LoadInt32(&maxIn)
				if c <= m || atomic.CompareAndSwapInt32(&maxIn, m, c) {
					break
				}
			}
			if st := pool.Status()["deepseek"]; st.Inflight > 3 || st.Max != 3 || st.RPM != 1200 {
				t.Errorf("status = %+v", st)
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&inflight, -1)
			rel()
			rel() // idempotent
		}()
	}
	wg.Wait()
	took := time.Since(start)
	if maxIn > 3 {
		t.Fatalf("max inflight %d > max_concurrency 3", maxIn)
	}
	// 50 requests at 20/s with a burst of 20 need at least 1.5 s.
	if took < 1200*time.Millisecond {
		t.Fatalf("token bucket not enforced: %d requests took %v", n, took)
	}
	if st := pool.Status()["deepseek"]; st.Inflight != 0 {
		t.Fatalf("leaked slots: %+v", st)
	}

	// Cancellation while waiting for a slot.
	var rels []func()
	for i := 0; i < 3; i++ {
		r, err := pool.Acquire(context.Background(), "deepseek")
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, r)
	}
	if !pool.Full("deepseek") {
		t.Fatal("pool should be full")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := pool.Acquire(ctx, "deepseek"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire on full pool: %v", err)
	}
	for _, r := range rels {
		r()
	}
	if _, err := pool.Acquire(context.Background(), "nope"); err == nil || !IsPermanent(err) {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestAPISlotWaiting(t *testing.T) {
	terms := newGate()
	e := newEnv(t, func(c *config.Config) {
		p := c.LLM.Providers["deepseek"]
		p.MaxConcurrency = 2
		c.LLM.Providers["deepseek"] = p
	}, instant(model.StageMaterialExtract, ResourceCPU), &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: terms.run})
	var rels []func()
	for i := 0; i < 2; i++ {
		r, err := e.s.Pool().Acquire(e.ctx, "deepseek")
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, r)
	}
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "w1.pdf", Path: "p"}
	e.st.CreateMaterial(e.ctx, m)
	e.start()
	e.waitFor("api_slot", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms)) == model.WaitAPISlot
	})
	if _, _, runs := terms.stats(); runs != 0 {
		t.Fatal("API job started while the provider was full")
	}
	if st := e.s.APIStatus()["deepseek"]; st.Inflight != 2 || st.Max != 2 {
		t.Fatalf("api status = %+v", st)
	}
	for _, r := range rels {
		r()
	}
	e.waitFor("material_terms running", 5*time.Second, func() bool {
		return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms).Status == model.StatusRunning
	})
	close(terms.release)
	e.waitFor("material_terms done", 5*time.Second, func() bool {
		return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms).Status == model.StatusDone
	})
	found := false
	e.ev.mu.Lock()
	for _, c := range e.ev.changed {
		if c == "course:"+e.course.ID {
			found = true
		}
	}
	e.ev.mu.Unlock()
	if !found {
		t.Fatal("material job changes must emit changed(course)")
	}
}

func TestDailyBudget(t *testing.T) {
	var clock atomic.Value
	clock.Store(time.Now())
	now := func() time.Time { return clock.Load().(time.Time) }
	terms := newGate()
	e := newEnv(t, func(c *config.Config) {
		c.LLM.Prices = map[string]map[string]config.Price{"deepseek": {"deepseek-chat": {Input: 1, CacheHit: 0.1, Output: 2}}}
		c.LLM.DailyBudget = 1.0
	})
	e.s.Stop()
	pool := NewPool(e.cfg, e.st, PoolOptions{Now: now, BudgetPoll: 5 * time.Millisecond, BudgetCacheTTL: -1})
	e.s, _ = New(Options{Store: e.st, Config: e.cfg, Events: e.ev, DataDir: e.dataDir, GPUQuery: e.gpu.query, Pool: pool,
		ScanInterval: 5 * time.Millisecond, Stages: map[string]Stage{
			model.StageMaterialExtract: instant(model.StageMaterialExtract, ResourceCPU),
			model.StageMaterialTerms:   &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: terms.run},
		}})

	usage, _ := pool.UsageToday(e.ctx)
	if usage.EstimatedCost == nil || *usage.EstimatedCost != 0 || pool.BudgetExceeded(e.ctx) {
		t.Fatalf("empty day: %+v", usage)
	}
	// Today's usage_daily: 0.6M uncached prompt + 0.4M cached + 0.2M output
	// = 0.6*1 + 0.4*0.1 + 0.2*2 = 1.04 ≥ budget 1.0 (the deepseek provider's
	// price: the legacy price of the model its roles use).
	pool.RecordUsage(e.ctx, "deepseek", 1_000_000, 400_000, 200_000)
	usage, _ = pool.UsageToday(e.ctx)
	if usage.PromptTokens != 1_000_000 || usage.EstimatedCost == nil || *usage.EstimatedCost < 1.039 || *usage.EstimatedCost > 1.041 {
		t.Fatalf("usage = %+v cost=%v", usage, *usage.EstimatedCost)
	}
	if !pool.BudgetExceeded(e.ctx) || !e.s.BudgetHit(e.ctx) {
		t.Fatal("budget should be exceeded")
	}

	// Acquire blocks (and reports budget for the job) until ctx is cancelled.
	ctx, cancel := context.WithCancel(WithJob(e.ctx, model.Job{ID: "J1"}))
	errc := make(chan error, 1)
	go func() { _, err := pool.Acquire(ctx, "deepseek"); errc <- err }()
	e.waitFor("budget blocked", 2*time.Second, func() bool { return pool.BudgetBlocked("J1") })
	if w := e.s.WaitingFor(model.Job{ID: "J1", Status: model.StatusRunning}); w != model.WaitBudget {
		t.Fatalf("running job waiting_for = %q", w)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire after cancel: %v", err)
	}

	// A queued API job shows waiting_for = budget and does not start.
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "a.pdf", Path: "p"}
	e.st.CreateMaterial(e.ctx, m)
	e.start()
	e.waitFor("budget reason", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms)) == model.WaitBudget
	})

	// A blocked Acquire resumes on the next local day.
	errc2 := make(chan error, 1)
	go func() {
		rel, err := pool.Acquire(e.ctx, "deepseek")
		if err == nil {
			rel()
		}
		errc2 <- err
	}()
	time.Sleep(30 * time.Millisecond)
	select {
	case err := <-errc2:
		t.Fatalf("acquire returned while over budget: %v", err)
	default:
	}
	clock.Store(time.Now().Add(48 * time.Hour))
	select {
	case err := <-errc2:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not resume on the next day")
	}
	close(terms.release)
	e.waitFor("material_terms done next day", 5*time.Second, func() bool {
		return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms).Status == model.StatusDone
	})
}

func TestEstimateCost(t *testing.T) {
	cfg := config.Default()
	if _, ok := EstimateCost(cfg, model.StageTranslate, model.JobStats{PromptTokens: 10}); ok {
		t.Fatal("no price configured")
	}
	// The legacy roles use config.DefaultDeepSeekModel (deepseek-flash since
	// v2, llm-builder; was deepseek-chat): the price is looked up by that model.
	cfg.LLM.Prices = map[string]map[string]config.Price{"deepseek": {config.DefaultDeepSeekModel: {Input: 2, CacheHit: 0.5, Output: 8}}}
	c, ok := EstimateCost(cfg, model.StageTranslate, model.JobStats{PromptTokens: 3_000_000, CacheHitTokens: 2_000_000, CompletionTokens: 500_000})
	if !ok || c != 2+1+4 {
		t.Fatalf("cost = %v %v", c, ok)
	}
	if c, _ := EstimateCost(cfg, model.StageSummary, model.JobStats{EstimatedCost: 0.25}); c != 0.25 {
		t.Fatal("recorded estimated_cost must win")
	}
	if RoleOfStage(model.StageMaterialTerms) != config.RoleGlossary || RoleOfStage(model.StagePrep) != "" {
		t.Fatal("RoleOfStage")
	}
}

// ---------- cancel ----------

func TestCancelRunningAndQueued(t *testing.T) {
	var slowStop atomic.Bool
	asr := &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(ctx context.Context, id string, p func(float64)) error {
		<-ctx.Done()
		if slowStop.Load() {
			time.Sleep(200 * time.Millisecond) // e.g. killing a worker
		}
		return ctx.Err()
	}}
	e := newEnv(t, nil, instant(model.StagePrep, ResourceCPU), asr)
	l := e.lecture()
	e.start()
	j := e.waitStatus(l.ID, model.StageASR, model.StatusRunning)
	slowStop.Store(true)
	t0 := time.Now()
	if err := e.s.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) > 3*time.Second {
		t.Fatal("cancel took too long")
	}
	j = e.lj(l.ID, model.StageASR)
	if j.Status != model.StatusCancelled || j.FinishedAt == nil || j.Error != nil {
		t.Fatalf("after cancel: %+v", j)
	}
	if s := e.lj(l.ID, model.StageSummary).Status; s != model.StatusPending {
		t.Fatalf("downstream = %s, must stay pending", s)
	}
	time.Sleep(40 * time.Millisecond)
	if s := e.lj(l.ID, model.StageASR).Status; s != model.StatusCancelled {
		t.Fatalf("cancelled must not resume automatically, got %s", s)
	}
	if err := e.s.Cancel(j.ID); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("second cancel: %v", err)
	}

	// Queued: GPU paused → asr queued → cancel is immediate.
	e.s.SetGPUPaused(true, false)
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageASR, model.ActionRetry); err != nil {
		t.Fatal(err)
	}
	q := e.lj(l.ID, model.StageASR)
	if q.Status != model.StatusQueued || q.Attempt != 1 {
		t.Fatalf("after retry: %+v", q)
	}
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageASR, model.ActionCancel); err != nil {
		t.Fatal(err)
	}
	if s := e.lj(l.ID, model.StageASR).Status; s != model.StatusCancelled {
		t.Fatalf("queued cancel: %s", s)
	}
	// Pending cannot be cancelled; unknown action.
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, model.ActionCancel); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("cancel pending: %v", err)
	}
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageSummary, "explode"); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("unknown action: %v", err)
	}
}

// ---------- retries ----------

func TestAutoRetryWithBackoff(t *testing.T) {
	// The scheduler's clock (Options.Now: the backoff) stands still until
	// the test moves it, so each backoff lasts until the test has read the
	// waiting state; polling for a 60 ms state missed it on a loaded machine
	// (flake-hunt #8). The runs' start times are read on that clock: a run
	// that started before the backoff ended has the earlier time.
	clk := &gpuTestClock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	var starts []time.Time
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		mu.Lock()
		starts = append(starts, clk.Now())
		n := len(starts)
		mu.Unlock()
		if n <= 2 {
			return fmt.Errorf("transient failure %d", n)
		}
		return nil
	}}
	e := newEnvOpts(t, nil, func(o *Options) { o.Now = clk.Now }, prep)
	l := e.lecture()
	e.start()
	for i, wait := range []time.Duration{60 * time.Millisecond, 120 * time.Millisecond} { // newScheduler's RetryBackoff
		e.waitFor(fmt.Sprintf("retry_backoff %d", i+1), 5*time.Second, func() bool {
			j := e.lj(l.ID, model.StagePrep)
			return j.Attempt == i+1 && strings.HasPrefix(e.s.WaitingFor(j), model.WaitRetryBackoffPrefix)
		})
		j := e.lj(l.ID, model.StagePrep)
		if j.Status != model.StatusQueued || j.Attempt != i+1 || j.Error == nil || !strings.Contains(*j.Error, fmt.Sprintf("transient failure %d", i+1)) {
			t.Fatalf("during backoff %d: %+v", i+1, j)
		}
		clk.Advance(wait)
	}
	j := e.waitStatus(l.ID, model.StagePrep, model.StatusDone)
	if j.Attempt != 2 || j.Error != nil || j.Progress != 1 {
		t.Fatalf("final: %+v", j)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 3 {
		t.Fatalf("runs = %d", len(starts))
	}
	if d := starts[1].Sub(starts[0]); d < 60*time.Millisecond {
		t.Fatalf("first backoff %v < 60ms", d)
	}
	if d := starts[2].Sub(starts[1]); d < 120*time.Millisecond {
		t.Fatalf("second backoff %v < 120ms", d)
	}
	// running → failed → queued is one transaction: "failed" is never
	// committed or emitted for a job that is retried (it means final failure).
	e.ev.mu.Lock()
	for _, ej := range e.ev.jobs {
		if ej.ID == j.ID && ej.Status == model.StatusFailed {
			t.Errorf("intermediate failed event emitted: %+v", ej)
		}
	}
	e.ev.mu.Unlock()
}

func TestRetryLimitAndPermanent(t *testing.T) {
	var runs atomic.Int32
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		runs.Add(1)
		return errors.New("always broken")
	}}
	var permRuns atomic.Int32
	mx := &fakeStage{name: model.StageMaterialExtract, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		permRuns.Add(1)
		return Permanent(errors.New("worker exit code 2: bad pdf"))
	}}
	e := newEnv(t, nil, prep, mx)
	l := e.lecture()
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "a.pdf", Path: "p"}
	e.st.CreateMaterial(e.ctx, m)
	e.start()
	e.waitFor("prep final failure", 5*time.Second, func() bool {
		j := e.lj(l.ID, model.StagePrep)
		return j.Status == model.StatusFailed && j.Attempt == 2
	})
	e.waitFor("permanent failure", 5*time.Second, func() bool {
		return e.job(model.ScopeMaterial, m.ID, model.StageMaterialExtract).Status == model.StatusFailed
	})
	time.Sleep(250 * time.Millisecond) // longer than every backoff
	j := e.lj(l.ID, model.StagePrep)
	if j.Status != model.StatusFailed || runs.Load() != 3 || *j.Error != "always broken" {
		t.Fatalf("prep: %+v runs=%d", j, runs.Load())
	}
	mj := e.job(model.ScopeMaterial, m.ID, model.StageMaterialExtract)
	if mj.Attempt != 0 || permRuns.Load() != 1 || !strings.Contains(*mj.Error, "bad pdf") {
		t.Fatalf("permanent error must not be retried: %+v runs=%d", mj, permRuns.Load())
	}
	if s := e.lj(l.ID, model.StageASR).Status; s != model.StatusPending {
		t.Fatalf("downstream of failed = %s", s)
	}
	if w := e.s.WaitingFor(e.lj(l.ID, model.StageASR)); w != model.WaitUpstream(model.StagePrep) {
		t.Fatalf("waiting_for = %q", w)
	}
}

// ---------- pipeline progression ----------

func TestDependenciesAdvanceAutomatically(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]model.Job{}
	order := []string{}
	mk := func(name string, res ResourceKind, withHash bool) *fakeStage {
		fs := &fakeStage{name: name, res: res, run: func(ctx context.Context, id string, p func(float64)) error {
			j, ok := JobFrom(ctx)
			if !ok || j.Stage != name || j.ScopeID != id {
				return fmt.Errorf("JobFrom missing in %s", name)
			}
			p(0.5)
			mu.Lock()
			seen[name] = j
			order = append(order, name)
			mu.Unlock()
			return nil
		}}
		if withHash {
			fs.hash = func(ctx context.Context, id string) (string, error) { return "hash-" + name, nil }
		}
		return fs
	}
	gsGate := newGate()
	glossary := &fakeStage{name: model.StageGlossarySeed, res: ResourceAPI, run: gsGate.run}
	termsGate := newGate()
	terms := &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: termsGate.run}
	e := newEnv(t, nil,
		mk(model.StagePrep, ResourceCPU, false), mk(model.StageASR, ResourceGPU, true), mk(model.StageSummary, ResourceAPI, true),
		mk(model.StageRepair, ResourceAPI, true), mk(model.StageTranslate, ResourceAPI, false), mk(model.StageExport, ResourceCPU, false),
		glossary, instant(model.StageMaterialExtract, ResourceCPU), terms)
	e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "s.md", Path: "p"})
	state.ResetGlossarySeed(e.ctx, e.st, e.course.ID)
	l := e.lecture()
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, LectureID: &l.ID, Filename: "week1.pdf", Path: "p"}
	e.st.CreateMaterial(e.ctx, m)
	e.start()

	e.waitStatus(l.ID, model.StagePrep, model.StatusDone)
	e.waitFor("asr waits for glossary_seed", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(l.ID, model.StageASR)) == model.WaitGlossarySeed
	})
	close(gsGate.release)
	e.waitFor("asr waits for material_terms", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(l.ID, model.StageASR)) == "material_terms:week1.pdf"
	})
	if s := e.lj(l.ID, model.StageASR).Status; s != model.StatusPending {
		t.Fatalf("asr = %s", s)
	}
	close(termsGate.release)
	for _, s := range model.LectureStages {
		j := e.waitStatus(l.ID, s, model.StatusDone)
		if j.Progress != 1 || j.StartedAt == nil || j.FinishedAt == nil || j.Error != nil {
			t.Fatalf("%s: %+v", s, j)
		}
		switch s {
		case model.StageASR, model.StageSummary, model.StageRepair:
			if j.InputHash == nil || *j.InputHash != "hash-"+s {
				t.Fatalf("%s input_hash = %v", s, j.InputHash)
			}
			mu.Lock()
			if sj := seen[s]; sj.InputHash == nil || *sj.InputHash != "hash-"+s {
				t.Errorf("%s: job in ctx lacks input_hash", s)
			}
			mu.Unlock()
		default:
			if j.InputHash != nil {
				t.Fatalf("%s: no hash expected, got %v", s, *j.InputHash)
			}
		}
	}
	mu.Lock()
	if strings.Join(order, ",") != strings.Join(model.LectureStages, ",") {
		t.Fatalf("run order = %v", order)
	}
	mu.Unlock()

	// Rerun from the API layer: downstream reruns automatically.
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageRepair, model.ActionRerun); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	mu.Lock()
	if len(order) != 9 || strings.Join(order[6:], ",") != "repair,translate,export" {
		t.Fatalf("after rerun: %v", order)
	}
	mu.Unlock()
	// Retry of a done stage is refused.
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StagePrep, model.ActionRetry); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("retry done: %v", err)
	}
}

func TestProgressThrottled(t *testing.T) {
	var e *env
	// progressSeen: a running event with progress was emitted for the
	// lecture's prep (the fixed sleeps alone may see no flush under load).
	progressSeen := func(id string) bool {
		e.ev.mu.Lock()
		defer e.ev.mu.Unlock()
		for _, ej := range e.ev.jobs {
			if ej.ScopeID == id && ej.Stage == model.StagePrep && ej.Status == model.StatusRunning && ej.Progress > 0 {
				return true
			}
		}
		return false
	}
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		for i := 0; i <= 10000; i++ {
			p(float64(i) / 10000)
			if i%500 == 0 {
				time.Sleep(4 * time.Millisecond) // let a few flushes happen
			}
			if i == 5000 {
				for deadline := time.Now().Add(5 * time.Second); !progressSeen(id); time.Sleep(time.Millisecond) {
					if time.Now().After(deadline) {
						return errors.New("no progress flushed within 5 s")
					}
				}
			}
		}
		p(2) // clamped
		time.Sleep(15 * time.Millisecond)
		return nil
	}}
	e = newEnv(t, nil, prep)
	l := e.lecture()
	e.start()
	j := e.waitStatus(l.ID, model.StagePrep, model.StatusDone)
	n := 0
	e.ev.mu.Lock()
	for _, ej := range e.ev.jobs {
		if ej.ID == j.ID && ej.Status == model.StatusRunning && ej.Progress > 0 {
			n++
			if ej.Progress > 1 || ej.Progress < 0 {
				t.Errorf("unclamped progress %v", ej.Progress)
			}
		}
	}
	e.ev.mu.Unlock()
	if n == 0 || n > 40 {
		t.Fatalf("%d progress events for 10001 updates; throttle broken", n)
	}
	if j.Progress != 1 {
		t.Fatalf("final progress = %v", j.Progress)
	}
}

func TestStopLeavesRunningForRecovery(t *testing.T) {
	g := newGate()
	e := newEnv(t, nil, &fakeStage{name: model.StagePrep, res: ResourceCPU, run: g.run})
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StagePrep, model.StatusRunning)
	e.s.Stop()
	if s := e.lj(l.ID, model.StagePrep).Status; s != model.StatusRunning {
		t.Fatalf("after Stop: %s", s)
	}
	if err := state.Recover(e.ctx, e.st, e.dataDir); err != nil {
		t.Fatal(err)
	}
	j := e.lj(l.ID, model.StagePrep)
	if j.Status != model.StatusQueued || j.Attempt != 1 || !state.IsInterrupted(j.Error) {
		t.Fatalf("after recover: %+v", j)
	}
	// A new scheduler picks it up.
	close(g.release)
	e.s = e.newScheduler(&fakeStage{name: model.StagePrep, res: ResourceCPU, run: g.run})
	e.start()
	e.waitStatus(l.ID, model.StagePrep, model.StatusDone)
}

func TestIncrementalTranslateWaitsForRunningExport(t *testing.T) {
	var trRuns atomic.Int32
	exportGate := newGate()
	var exRuns atomic.Int32
	tr := &fakeStage{name: model.StageTranslate, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		trRuns.Add(1)
		return nil
	}}
	ex := &fakeStage{name: model.StageExport, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		if exRuns.Add(1) == 1 {
			return exportGate.run(ctx, id, p)
		}
		return nil
	}}
	e := newEnv(t, nil, instant(model.StagePrep, ResourceCPU), instant(model.StageASR, ResourceGPU),
		instant(model.StageSummary, ResourceAPI), instant(model.StageRepair, ResourceAPI), tr, ex)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusRunning)
	started, err := e.s.StartIncrementalTranslate(e.ctx, l.ID)
	if started || err != nil {
		t.Fatalf("while export runs: %v %v", started, err)
	}
	close(exportGate.release)
	e.waitFor("incremental translate + re-export", 5*time.Second, func() bool {
		return trRuns.Load() == 2 && exRuns.Load() == 2 && e.lj(l.ID, model.StageExport).Status == model.StatusDone
	})
	if s := e.lj(l.ID, model.StageTranslate).Status; s != model.StatusDone {
		t.Fatalf("translate = %s", s)
	}
	// Direct start when nothing runs.
	started, err = e.s.StartIncrementalTranslate(e.ctx, l.ID)
	if !started || err != nil {
		t.Fatalf("direct start: %v %v", started, err)
	}
	e.waitFor("third translate", 5*time.Second, func() bool {
		return trRuns.Load() == 3 && e.lj(l.ID, model.StageExport).Status == model.StatusDone
	})
}

func TestInvalidationThroughScheduler(t *testing.T) {
	var hash atomic.Value
	hash.Store("h1")
	repair := &fakeStage{name: model.StageRepair, res: ResourceAPI,
		hash: func(ctx context.Context, id string) (string, error) { return hash.Load().(string), nil }}
	e := newEnv(t, nil, instant(model.StagePrep, ResourceCPU), instant(model.StageASR, ResourceGPU),
		instant(model.StageSummary, ResourceAPI), repair, instant(model.StageTranslate, ResourceAPI), instant(model.StageExport, ResourceCPU))
	l := e.lecture()
	e.start()
	// The whole pipeline (six stages) first: more than waitStatus's 5 s on a
	// heavily loaded machine (flake-hunt #8).
	e.waitFor("the pipeline", 30*time.Second, func() bool {
		return e.lj(l.ID, model.StageExport).Status == model.StatusDone
	})
	hash.Store("h2")
	changed, err := e.s.InvalidateCourse(e.ctx, e.course.ID)
	if err != nil || len(changed) != 1 || changed[0].Stage != model.StageRepair {
		t.Fatalf("changed = %+v err=%v", changed, err)
	}
	time.Sleep(40 * time.Millisecond)
	if s := e.lj(l.ID, model.StageRepair).Status; s != model.StatusStale {
		t.Fatalf("repair = %s (stale must not rerun automatically)", s)
	}
	if acts := state.AllowedActions(e.lj(l.ID, model.StageRepair), state.ActionOpts{}); len(acts) != 1 || acts[0] != model.ActionRerun {
		t.Fatalf("stale actions = %v", acts)
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	m, err := ParseNvidiaSMI("\r\n10240, 12282, 1800\r\n4000, 8000, 3000\n")
	if err != nil || m != (GPUMem{FreeMB: 10240, TotalMB: 12282, UsedMB: 1800}) {
		t.Fatalf("%+v %v", m, err)
	}
	for _, bad := range []string{"", "N/A, N/A, N/A", "1,2"} {
		if _, err := ParseNvidiaSMI(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}
