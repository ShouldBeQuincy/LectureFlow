// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Tests for the Gate A fixes (NOTES.md "Gate A").

import (
	"context"
	"errors"
	"os"
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

// lectureAt creates a launched lecture with an explicit created_at.
func (e *env) lectureAt(createdAt string) model.Lecture {
	e.t.Helper()
	return e.launched(model.Lecture{ID: db.NewID(), CourseID: e.course.ID, Title: "L", MediaPath: "m.mp4", CreatedAt: createdAt})
}

func allInstant(except ...Stage) []Stage {
	byName := map[string]Stage{}
	for _, s := range except {
		byName[s.Name()] = s
	}
	res := map[string]ResourceKind{
		model.StagePrep: ResourceCPU, model.StageASR: ResourceGPU, model.StageSummary: ResourceAPI,
		model.StageRepair: ResourceAPI, model.StageTranslate: ResourceAPI, model.StageExport: ResourceCPU,
	}
	var out []Stage
	for _, name := range model.LectureStages {
		if s, ok := byName[name]; ok {
			out = append(out, s)
		} else {
			out = append(out, instant(name, res[name]))
		}
	}
	return out
}

// ---------- CPU pool + FIFO (item 1, 10) ----------

func TestCPUSlotLimitAndFIFO(t *testing.T) {
	var mu sync.Mutex
	var order []string
	g := newGate()
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
		return g.run(ctx, id, p)
	}}
	e := newEnvOpts(t, nil, func(o *Options) { o.CPUSlots = 1 }, prep)
	// Created first but with the later created_at: must run second.
	younger := e.lectureAt("2026-01-02T00:00:00Z")
	older := e.lectureAt("2026-01-01T00:00:00Z")
	e.start()
	e.waitStatus(older.ID, model.StagePrep, model.StatusRunning)
	e.waitFor("cpu_slot", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(younger.ID, model.StagePrep)) == model.WaitCPUSlot
	})
	time.Sleep(30 * time.Millisecond)
	if _, m, _ := g.stats(); m != 1 {
		t.Fatalf("max concurrent CPU jobs = %d with CPUSlots=1", m)
	}
	close(g.release)
	e.waitStatus(younger.ID, model.StagePrep, model.StatusDone)
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != older.ID || order[1] != younger.ID {
		t.Fatalf("FIFO by lecture created_at violated: %v (older=%s)", order, older.ID)
	}
}

// ---------- post-run invalidation (item 2) ----------

func TestInputChangeDuringRunMarksStale(t *testing.T) {
	var summaryHash atomic.Value
	summaryHash.Store("v1")
	summary := &fakeStage{name: model.StageSummary, res: ResourceAPI,
		hash: func(ctx context.Context, id string) (string, error) { return summaryHash.Load().(string), nil },
		run: func(ctx context.Context, id string, p func(float64)) error {
			summaryHash.Store("v2") // e.g. a material was uploaded while summarising
			return nil
		}}
	asr := &fakeStage{name: model.StageASR, res: ResourceGPU,
		hash: func(ctx context.Context, id string) (string, error) { return "constant", nil }}
	e := newEnv(t, nil, allInstant(summary, asr)...)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	e.waitStatus(l.ID, model.StageSummary, model.StatusStale)
	if j := e.lj(l.ID, model.StageASR); j.Status != model.StatusDone {
		t.Fatalf("asr = %s, unchanged inputs must stay done", j.Status)
	}
	if j := e.lj(l.ID, model.StageSummary); j.InputHash == nil || *j.InputHash != "v1" {
		t.Fatalf("recorded hash = %v", j.InputHash)
	}
}

// ---------- deps re-checked before queued → running (item 4) ----------

func TestQueuedJobRechecksDepsBeforeRunning(t *testing.T) {
	var asrRuns atomic.Int32
	entered := make(chan struct{})
	asr := &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(ctx context.Context, id string, p func(float64)) error {
		if asrRuns.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	gs := newGate()
	glossary := &fakeStage{name: model.StageGlossarySeed, res: ResourceAPI, run: gs.run}
	e := newEnv(t, nil, append(allInstant(asr), glossary)...)
	l := e.lecture()
	e.start()
	j := e.waitStatus(l.ID, model.StageASR, model.StatusRunning)
	<-entered
	if err := e.s.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	// The syllabus arrives after asr was cancelled.
	e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "s.md", Path: "p"})
	if err := state.ResetGlossarySeed(e.ctx, e.st, e.course.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageASR, model.ActionRetry); err != nil {
		t.Fatal(err)
	}
	e.waitFor("asr waits for glossary_seed", 5*time.Second, func() bool {
		return e.s.WaitingFor(e.lj(l.ID, model.StageASR)) == model.WaitGlossarySeed
	})
	time.Sleep(40 * time.Millisecond)
	if q := e.lj(l.ID, model.StageASR); q.Status != model.StatusQueued || asrRuns.Load() != 1 {
		t.Fatalf("asr must stay queued until glossary_seed is done: %s runs=%d", q.Status, asrRuns.Load())
	}
	close(gs.release)
	e.waitStatus(l.ID, model.StageASR, model.StatusDone)
	if asrRuns.Load() != 2 {
		t.Fatalf("asr runs = %d", asrRuns.Load())
	}
}

// ---------- API FIFO priority (item 5) ----------

func priorityPool(t *testing.T, max, rpm int) (*Pool, *providerPool) {
	t.Helper()
	cfg := config.Default()
	p := cfg.LLM.Providers["deepseek"]
	p.MaxConcurrency, p.RequestsPerMinute = max, rpm
	cfg.LLM.Providers["deepseek"] = p
	pool := NewPool(cfg, nil, PoolOptions{})
	return pool, pool.providers["deepseek"]
}

func waitWaiters(t *testing.T, pp *providerPool, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, w := pp.status(); w == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	_, w := pp.status()
	t.Fatalf("waiters = %d, want %d", w, n)
}

func TestAPIPoolServesOldestLectureFirst(t *testing.T) {
	pool, pp := priorityPool(t, 1, 600000)
	older := model.Job{ID: "j-old", ScopeID: "L-old", CreatedAt: "2026-01-02T00:00:00Z"}
	younger := model.Job{ID: "j-young", ScopeID: "L-young", CreatedAt: "2026-01-01T00:00:00Z"}
	pool.setPriority(older, "2026-01-01T00:00:00Z") // lecture created first
	pool.setPriority(younger, "2026-01-05T00:00:00Z")
	holder, err := pool.Acquire(context.Background(), "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	req := func(ctx context.Context, label string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := pool.Acquire(ctx, "deepseek")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, label)
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			rel()
		}()
	}
	req(context.Background(), "none") // no job: always last
	waitWaiters(t, pp, 1)
	for i := 0; i < 3; i++ {
		req(WithJob(context.Background(), younger), "young")
	}
	waitWaiters(t, pp, 4)
	for i := 0; i < 3; i++ {
		req(WithJob(context.Background(), older), "old")
	}
	waitWaiters(t, pp, 7)
	holder()
	wg.Wait()
	want := []string{"old", "old", "old", "young", "young", "young", "none"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("grant order = %v, want %v", order, want)
		}
	}
}

func TestAPIPoolTokensFollowPriority(t *testing.T) {
	pool, pp := priorityPool(t, 10, 120) // slots are plenty; 2 tokens/s, burst 2
	for i := 0; i < 2; i++ {             // drain the bucket: the next token is ~500ms away
		rel, err := pool.Acquire(context.Background(), "deepseek")
		if err != nil {
			t.Fatal(err)
		}
		rel()
	}
	older := model.Job{ID: "a", ScopeID: "L1", CreatedAt: "2026-01-01T00:00:00Z"}
	younger := model.Job{ID: "b", ScopeID: "L2", CreatedAt: "2026-01-01T00:00:00Z"}
	pool.setPriority(older, "2026-01-01T00:00:00Z")
	pool.setPriority(younger, "2026-01-03T00:00:00Z")
	got := make(chan string, 2)
	go func() {
		rel, _ := pool.Acquire(WithJob(context.Background(), younger), "deepseek")
		got <- "young"
		rel()
	}()
	waitWaiters(t, pp, 1)
	go func() {
		rel, _ := pool.Acquire(WithJob(context.Background(), older), "deepseek")
		got <- "old"
		rel()
	}()
	waitWaiters(t, pp, 2)
	if first, second := <-got, <-got; first != "old" || second != "young" {
		t.Fatalf("token order = %s, %s; the older lecture queued later but must get the next token", first, second)
	}
}

func TestAPIPriorityRegisteredPerLecture(t *testing.T) {
	var barrier sync.WaitGroup
	barrier.Add(2)
	var mu sync.Mutex
	var order []string
	var e *env
	summary := &fakeStage{name: model.StageSummary, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		barrier.Done()
		barrier.Wait() // both summaries are running now
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rel, err := e.s.Pool().Acquire(ctx, "deepseek")
				if err != nil {
					return
				}
				mu.Lock()
				order = append(order, id)
				mu.Unlock()
				time.Sleep(25 * time.Millisecond)
				rel()
			}()
		}
		wg.Wait()
		return nil
	}}
	e = newEnv(t, func(c *config.Config) {
		p := c.LLM.Providers["deepseek"]
		p.MaxConcurrency, p.RequestsPerMinute = 1, 600000
		c.LLM.Providers["deepseek"] = p
	}, allInstant(summary)...)
	younger := e.lectureAt("2026-01-02T00:00:00Z")
	older := e.lectureAt("2026-01-01T00:00:00Z")
	e.start()
	e.waitStatus(younger.ID, model.StageSummary, model.StatusDone)
	e.waitStatus(older.ID, model.StageSummary, model.StatusDone)
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 8 {
		t.Fatalf("grants = %v", order)
	}
	// The very first grant may go to either lecture (the slot was free);
	// after that every older-lecture request must precede the younger ones.
	seenYoung := false
	for _, id := range order[1:] {
		if id == younger.ID {
			seenYoung = true
		} else if seenYoung {
			t.Fatalf("younger lecture's request served before an older one: %v (older=%s)", order, older.ID)
		}
	}
}

// ---------- usage ledger (item 6) ----------

func TestUsageCountsWhateverTheJobStatus(t *testing.T) {
	var runs atomic.Int32
	g := newGate()
	var e *env
	addTokens := func(ctx context.Context) {
		// What the llm client does for every response: job.stats and usage_daily.
		ReportUsage(ctx, 1000, 100, 0)
		e.s.Pool().RecordUsage(context.WithoutCancel(ctx), "deepseek", 1000, 0, 100)
	}
	terms := &fakeStage{name: model.StageMaterialTerms, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		addTokens(ctx)
		switch runs.Add(1) {
		case 1:
			return errors.New("HTTP 503") // auto retry
		case 2:
			if err := g.run(ctx, id, p); err != nil {
				return err
			}
			return Permanent(errors.New("HTTP 401"))
		default:
			<-ctx.Done() // cancelled by the user
			return ctx.Err()
		}
	}}
	// The scheduler's clock stands still until the test moves it, so the
	// retry backoff lasts until the test has read the usage in it (polling
	// for a 300 ms state can miss it on a loaded machine).
	clk := &gpuTestClock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	e = newEnvOpts(t, nil, func(o *Options) {
		o.RetryBackoff = []time.Duration{300 * time.Millisecond, 300 * time.Millisecond}
		o.Now = clk.Now
	}, instant(model.StageMaterialExtract, ResourceCPU), terms)
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "a.pdf", Path: "p"}
	e.st.CreateMaterial(e.ctx, m)
	tj := func() model.Job { return e.job(model.ScopeMaterial, m.ID, model.StageMaterialTerms) }
	usage := func() int64 {
		u, err := e.s.UsageToday(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return u.PromptTokens
	}
	e.start()
	// 1) failed and requeued (in backoff): the first attempt's tokens count.
	e.waitFor("queued for retry", 5*time.Second, func() bool {
		j := tj()
		return j.Status == model.StatusQueued && j.Attempt == 1 && strings.HasPrefix(e.s.WaitingFor(j), model.WaitRetryBackoffPrefix)
	})
	if u := usage(); u != 1000 {
		t.Fatalf("usage while in retry backoff = %d, want 1000", u)
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("runs during the backoff = %d", n)
	}
	clk.Advance(300 * time.Millisecond) // the backoff ends
	// 2) running: flushed within the flush interval.
	e.waitFor("second run counted", 5*time.Second, func() bool { return usage() == 2000 })
	close(g.release)
	e.waitFor("final failure", 5*time.Second, func() bool { return tj().Status == model.StatusFailed })
	if u := usage(); u != 2000 {
		t.Fatalf("usage after failure = %d", u)
	}
	// 3) user retry, then cancel.
	if err := e.s.DoAction(e.ctx, model.ScopeMaterial, m.ID, model.StageMaterialTerms, model.ActionRetry); err != nil {
		t.Fatal(err)
	}
	e.waitFor("third run", 5*time.Second, func() bool { return runs.Load() == 3 && tj().Status == model.StatusRunning })
	if err := e.s.Cancel(tj().ID); err != nil {
		t.Fatal(err)
	}
	if j := tj(); j.Status != model.StatusCancelled || j.FinishedAt == nil {
		t.Fatalf("after cancel: %+v", j)
	}
	if u := usage(); u != 3000 {
		t.Fatalf("usage after cancel = %d, want 3000", u)
	}
	u, _ := e.s.UsageToday(e.ctx)
	if u.CompletionTokens != 300 || u.EstimatedCost != nil {
		t.Fatalf("usage = %+v", u)
	}
	// 4) the next local day starts from zero.
	tomorrow := NewPool(e.cfg, e.st, PoolOptions{Now: func() time.Time { return time.Now().Add(48 * time.Hour) }})
	if u, _ := tomorrow.UsageToday(e.ctx); u.PromptTokens != 0 {
		t.Fatalf("tomorrow's usage = %+v", u)
	}
}

// ---------- non-blocking progress (item 7) ----------

func TestProgressCallbackNeverTouchesTheDatabase(t *testing.T) {
	ready := make(chan struct{})
	locked := make(chan struct{})
	took := make(chan time.Duration, 1)
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		close(ready)
		<-locked
		t0 := time.Now()
		for i := 0; i <= 10000; i++ {
			p(float64(i) / 10000)
		}
		took <- time.Since(t0)
		return nil
	}}
	e := newEnv(t, nil, prep)
	l := e.lecture()
	e.start()
	<-ready
	release := make(chan struct{})
	txDone := make(chan error, 1)
	go func() {
		// Hold the only connection, like a long transaction elsewhere.
		txDone <- e.st.Tx(context.Background(), func(tx db.Store) error {
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case d := <-took:
		if d > 500*time.Millisecond {
			t.Fatalf("10001 progress calls took %v while the database was busy", d)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("progress callback blocked on the database")
	}
	close(release)
	if err := <-txDone; err != nil {
		t.Fatal(err)
	}
	j := e.waitStatus(l.ID, model.StagePrep, model.StatusDone)
	if j.Progress != 1 {
		t.Fatalf("progress = %v", j.Progress)
	}
}

// ---------- incremental translate debounce (item 8) ----------

// seedSentences gives the lecture three translated sentences, one stale.
func (e *env) seedSentences(lectureID string) {
	e.t.Helper()
	zh := "中文"
	var ss []model.Sentence
	for i := 0; i < 3; i++ {
		ss = append(ss, model.Sentence{Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i+1) * 1000, ASRText: "x", RepairedText: "x",
			Zh: &zh, ZhStatus: model.ZhOK})
	}
	if err := e.st.InsertSentences(e.ctx, lectureID, ss); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.SetZhStatus(e.ctx, lectureID, []int{1}, model.ZhStale); err != nil {
		e.t.Fatal(err)
	}
}

func TestIncrementalTranslateDebounce(t *testing.T) {
	// The debounce runs on the scheduler's clock (Options.Now), which only
	// moves when the test moves it: real sleeps of 25 ms between requests
	// could last longer than the 60 ms debounce on a loaded machine. The
	// real sleeps left below only give a wrongly started run time to show.
	clk := &gpuTestClock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	var trStarts []time.Time
	hold := make(chan struct{})
	var holdThird atomic.Bool
	tr := &fakeStage{name: model.StageTranslate, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		mu.Lock()
		trStarts = append(trStarts, clk.Now())
		n := len(trStarts)
		mu.Unlock()
		if n == 2 && holdThird.Load() {
			select {
			case <-hold:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}}
	e := newEnvOpts(t, nil, func(o *Options) { o.Now = clk.Now }, allInstant(tr)...) // debounce 60ms
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	e.seedSentences(l.ID)
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(trStarts) }

	// A burst of review operations → one run, after the quiet period.
	holdThird.Store(true)
	var last time.Time
	for i := 0; i < 3; i++ {
		last = clk.Now()
		e.s.RequestIncrementalTranslate(l.ID)
		clk.Advance(25 * time.Millisecond)
		time.Sleep(25 * time.Millisecond) // several scans
		if count() != 1 {
			t.Fatal("incremental translate started before the debounce interval")
		}
	}
	// 50 ms since the last request: still within the debounce.
	clk.Advance(25 * time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	if count() != 1 {
		t.Fatal("incremental translate started before the debounce interval")
	}
	clk.Advance(10 * time.Millisecond) // 60 ms: due
	e.waitFor("incremental run", 5*time.Second, func() bool { return count() == 2 })
	mu.Lock()
	if d := trStarts[1].Sub(last); d < 60*time.Millisecond {
		mu.Unlock()
		t.Fatalf("started %v after the last request, debounce is 60ms", d)
	}
	mu.Unlock()
	// A request while that run is in progress is kept and served after it.
	e.waitStatus(l.ID, model.StageTranslate, model.StatusRunning)
	e.s.RequestIncrementalTranslate(l.ID)
	clk.Advance(60 * time.Millisecond) // past the debounce
	time.Sleep(120 * time.Millisecond)
	if count() != 2 {
		t.Fatal("request must wait for the running translate")
	}
	close(hold)
	e.waitFor("third run after the running one", 5*time.Second, func() bool { return count() == 3 })
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	time.Sleep(120 * time.Millisecond)
	if count() != 3 {
		t.Fatalf("translate runs = %d, want 3", count())
	}
	// Nothing stale → a request is dropped without a run.
	e.st.SetZhStatus(e.ctx, l.ID, []int{1}, model.ZhOK)
	e.s.RequestIncrementalTranslate(l.ID)
	clk.Advance(60 * time.Millisecond) // past the debounce
	time.Sleep(150 * time.Millisecond)
	if count() != 3 {
		t.Fatal("no stale sentence: no incremental run expected")
	}
}

func TestIncrementalTranslateResumedAtStart(t *testing.T) {
	var trRuns atomic.Int32
	tr := &fakeStage{name: model.StageTranslate, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		trRuns.Add(1)
		return nil
	}}
	e := newEnv(t, nil, allInstant(tr)...)
	l := e.lecture()
	other := e.lecture() // done, nothing stale: untouched
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	e.waitStatus(other.ID, model.StageExport, model.StatusDone)
	e.seedSentences(l.ID) // a debounced request was pending when the process stopped
	e.s.Stop()
	if trRuns.Load() != 2 {
		t.Fatalf("setup translate runs = %d", trRuns.Load())
	}
	e.s = e.newScheduler(allInstant(tr)...)
	e.start()
	e.waitFor("resumed incremental translate", 5*time.Second, func() bool {
		return trRuns.Load() == 3 && e.lj(l.ID, model.StageExport).Status == model.StatusDone
	})
	time.Sleep(50 * time.Millisecond)
	if trRuns.Load() != 3 {
		t.Fatalf("translate runs = %d, only the lecture with stale sentences may rerun", trRuns.Load())
	}
}

// ---------- rerun file cleanup vs scan (item 9) ----------

func TestRerunCleanupFinishesBeforeNewJobsStart(t *testing.T) {
	var e *env
	var dirty atomic.Int32
	lecDir := func(id string) string { return model.LectureDir(e.dataDir, e.course.ID, id) }
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		dir := lecDir(id)
		if _, err := os.Stat(filepath.Join(dir, model.AudioFile)); err == nil {
			dirty.Add(1) // the previous run's audio.wav was still there
		}
		return os.WriteFile(filepath.Join(dir, model.AudioFile), []byte("wav"), 0o644)
	}}
	export := &fakeStage{name: model.StageExport, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		return os.MkdirAll(filepath.Join(lecDir(id), model.ExportDirName), 0o755)
	}}
	e = newEnvOpts(t, nil, func(o *Options) { o.ScanInterval = time.Millisecond }, allInstant(prep, export)...)
	l := e.lecture()
	os.MkdirAll(lecDir(l.ID), 0o755)
	os.WriteFile(filepath.Join(lecDir(l.ID), "m.mp4"), []byte("m"), 0o644)
	e.start()
	for i := 0; i < 15; i++ {
		e.waitStatus(l.ID, model.StageExport, model.StatusDone)
		if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StagePrep, model.ActionRerun); err != nil {
			t.Fatal(err)
		}
	}
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	if n := dirty.Load(); n != 0 {
		t.Fatalf("%d prep runs started before the rerun had deleted audio.wav", n)
	}
}

// §17.2 calibration: a run that overlapped another GPU run must not update
// vram_observed (its increase includes the other job's memory); a run alone
// does, and only the maximum is kept.
func TestVRAMCalibrationIgnoresOverlappingRuns(t *testing.T) {
	var e *env
	var mode sync.Map // lecture id → "overlap" | int delta MB
	var cur atomic.Int32
	release := make(chan struct{})
	asr := &fakeStage{name: model.StageASR, res: ResourceGPU, run: func(ctx context.Context, id string, p func(float64)) error {
		cur.Add(1)
		defer cur.Add(-1)
		v, _ := mode.Load(id)
		if v == "overlap" {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		d := v.(int)
		e.gpu.set(20000-d, 24000, 1000+d)
		err := e.waitSampledPeak(id, d) // not a fixed sleep: it may see no sample under load
		e.gpu.set(20000, 24000, 1000)
		return err
	}}
	e = newEnv(t, func(c *config.Config) { c.ASR.MaxParallel = 2 }, instant(model.StagePrep, ResourceCPU), asr)
	key := model.KVVRAMObservedPrefix + "large-v3:float16"
	kv := func() (string, bool) {
		v, ok, err := e.st.GetKV(e.ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return v, ok
	}

	// 1) Two concurrent jobs, each really ~4.3 GB: together +8.6 GB over
	// either job's baseline. Neither may calibrate.
	l1, l2 := e.lecture(), e.lecture()
	mode.Store(l1.ID, "overlap")
	mode.Store(l2.ID, "overlap")
	e.start()
	e.waitFor("both asr running", 5*time.Second, func() bool { return cur.Load() == 2 })
	e.gpu.set(11400, 24000, 1000+8600)
	// Both runs' samplers saw the combined use (not a fixed sleep: under
	// load it could pass without a sample).
	for _, l := range []model.Lecture{l1, l2} {
		if err := e.waitSampledPeak(l.ID, 8600); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	j1 := e.waitStatus(l1.ID, model.StageASR, model.StatusDone)
	e.waitStatus(l2.ID, model.StageASR, model.StatusDone)
	if v, ok := kv(); ok {
		t.Fatalf("overlapping runs wrote %s = %s", key, v)
	}
	if j1.Stats.VRAMPeakMB == 0 {
		t.Fatal("the observed peak is still reported in the job's stats")
	}
	e.gpu.set(20000, 24000, 1000)

	// 2) A job alone calibrates.
	l3 := e.lecture()
	mode.Store(l3.ID, 4300)
	e.waitStatus(l3.ID, model.StageASR, model.StatusDone)
	if v, ok := kv(); !ok || v != "4300" {
		t.Fatalf("single run: %s = %q (ok=%v), want 4300", key, v, ok)
	}

	// 3) A smaller single observation keeps the maximum.
	l4 := e.lecture()
	mode.Store(l4.ID, 3000)
	e.waitStatus(l4.ID, model.StageASR, model.StatusDone)
	if v, _ := kv(); v != "4300" {
		t.Fatalf("maximum not kept: %s = %s", key, v)
	}
	// The estimate uses the clean calibration: max(5000, ceil(4300*1.1)) = 5000.
	if est := e.s.GPUStatus(e.ctx).EstimatedJobMB; est != 5000 {
		t.Fatalf("estimate = %d", est)
	}
}
