// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// r7 perf (P1 report H2): the startup resume of incremental translates
// (resumeIncremental, every lecture's CountZh) no longer holds up Start and
// the ready line; it runs at the beginning of the first scan, before
// anything is dispatched. These tests pin that the outcome is the one of
// the synchronous resume.

// trCounter is a translate stage that counts its runs per lecture and, like
// the real stage, leaves the lecture's sentences translated (zh ok).
type trCounter struct {
	mu   sync.Mutex
	runs map[string]int
}

func newTrCounter() *trCounter { return &trCounter{runs: map[string]int{}} }

func (c *trCounter) stage(st func() db.Store) *fakeStage {
	return &fakeStage{name: model.StageTranslate, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		c.mu.Lock()
		c.runs[id]++
		c.mu.Unlock()
		n, err := st().CountSentences(ctx, id)
		if err != nil || n == 0 {
			return err
		}
		idx := make([]int, n)
		for i := range idx {
			idx[i] = i
		}
		return st().SetZhStatus(ctx, id, idx, model.ZhOK)
	}}
}

func (c *trCounter) get(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[id]
}

// zhSentences inserts one sentence per status (translated text present).
func (e *env) zhSentences(lectureID string, statuses ...string) {
	e.t.Helper()
	zh := "中文"
	var ss []model.Sentence
	for i, z := range statuses {
		ss = append(ss, model.Sentence{Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i+1) * 1000, ASRText: "x", RepairedText: "x",
			Zh: &zh, ZhStatus: z})
	}
	if err := e.st.Tx(e.ctx, func(tx db.Store) error {
		if err := tx.InsertSentences(e.ctx, lectureID, ss); err != nil {
			return err
		}
		return tx.RecomputeText(e.ctx, lectureID, nil)
	}); err != nil {
		e.t.Fatal(err)
	}
}

// quiet waits until no pipeline job of the lectures is queued or running,
// or pending with its previous stage done (about to be queued); a pending
// job behind a failed or stale stage never starts.
func (e *env) quiet(ids ...string) {
	e.t.Helper()
	e.waitFor("lectures settled", 10*time.Second, func() bool {
		for _, id := range ids {
			prev := model.StatusDone // the first stage has no dependency
			for _, stage := range model.LectureStages {
				j := e.lj(id, stage)
				switch {
				case j.Status == model.StatusQueued, j.Status == model.StatusRunning:
					return false
				case j.Status == model.StatusPending && prev == model.StatusDone:
					return false
				}
				prev = j.Status
			}
		}
		return true
	})
}

// gatedZh blocks the first CountZh until release is closed (or the call's
// context ends): the startup resume stands still there.
type gatedZh struct {
	db.Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	// lateLookups counts the lookups of a lecture's translate job (the
	// resume's per-lecture read) made with a context already ended.
	lateLookups atomic.Int32
}

func (g *gatedZh) GetCurrentJob(ctx context.Context, scope, scopeID, stage string) (model.Job, error) {
	if ctx.Err() != nil && scope == model.ScopeLecture && stage == model.StageTranslate {
		g.lateLookups.Add(1)
	}
	return g.Store.GetCurrentJob(ctx, scope, scopeID, stage)
}

func newGatedZh(st db.Store) *gatedZh {
	return &gatedZh{Store: st, entered: make(chan struct{}), release: make(chan struct{})}
}

// startGated starts e.s, whose store is g, and waits until the resume
// stands in g's first CountZh. Start must return meanwhile: it fails
// (rather than hangs) when Start waits for the resume. release lets the
// resume go on (idempotent).
func (e *env) startGated(g *gatedZh) (release func()) {
	e.t.Helper()
	var once sync.Once
	release = func() { once.Do(func() { close(g.release) }) }
	done := make(chan error, 1)
	go func() { done <- e.s.Start(e.ctx) }()
	select {
	case err := <-done:
		if err != nil {
			release()
			e.t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		release()
		<-done
		e.t.Fatal("Start waited for the incremental-translate resume")
	}
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		release()
		e.t.Fatal("the first scan did not resume the incremental translates")
	}
	return release
}

func (g *gatedZh) CountZh(ctx context.Context, id string) (map[string]int, error) {
	first := false
	g.once.Do(func() { first = true; close(g.entered) })
	if first {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.Store.CountZh(ctx, id)
}

// startupFixture is the state the resume tests restart from, by name:
// lectures whose translate is done with stale, untranslated, translated or
// QA-failed sentences or none at all; one whose translate went stale; one
// whose translate failed for good; one not processed yet (its translate
// pending). The scheduler is stopped.
func startupFixture(t *testing.T, tr *trCounter) (*env, map[string]string) {
	var e *env
	failing := map[string]bool{}
	var fmu sync.Mutex
	trStage := tr.stage(func() db.Store { return e.st })
	run := trStage.run
	trStage.run = func(ctx context.Context, id string, p func(float64)) error {
		fmu.Lock()
		f := failing[id]
		fmu.Unlock()
		if f {
			return Permanent(fmt.Errorf("fixture: translate fails"))
		}
		return run(ctx, id, p)
	}
	e = newEnv(t, nil, allInstant(trStage)...)
	names := []string{"stale", "none", "ok", "qafail", "nosent", "staleJob", "failedJob"}
	ids := map[string]string{}
	for _, n := range names {
		ids[n] = e.lecture().ID
	}
	fmu.Lock()
	failing[ids["failedJob"]] = true
	fmu.Unlock()
	e.start()
	for _, n := range names {
		if n == "failedJob" {
			e.waitStatus(ids[n], model.StageTranslate, model.StatusFailed)
			continue
		}
		e.waitStatus(ids[n], model.StageExport, model.StatusDone)
	}
	e.s.Stop()
	fmu.Lock()
	delete(failing, ids["failedJob"])
	fmu.Unlock()
	// A debounced request was pending for these when the process stopped.
	e.zhSentences(ids["stale"], model.ZhOK, model.ZhStale, model.ZhOK)
	e.zhSentences(ids["none"], model.ZhOK, model.ZhNone)
	e.zhSentences(ids["ok"], model.ZhOK, model.ZhOK)
	e.zhSentences(ids["qafail"], model.ZhOK, model.ZhQAFailed)
	e.zhSentences(ids["staleJob"], model.ZhStale, model.ZhOK)
	e.zhSentences(ids["failedJob"], model.ZhNone, model.ZhStale)
	tj := e.lj(ids["staleJob"], model.StageTranslate)
	if err := state.TransitionJob(e.ctx, e.st, tj.ID, model.StatusDone, model.StatusStale, db.JobFinish{}); err != nil {
		t.Fatal(err)
	}
	ids["fresh"] = e.lecture().ID // created while stopped: nothing ran yet
	e.zhSentences(ids["fresh"], model.ZhNone)
	return e, ids
}

// outcome is what a restart left: per lecture name, the translate runs
// since the restart, every current job's status and the current
// translate's note.
func outcome(t *testing.T, e *env, ids map[string]string, tr *trCounter, before map[string]int) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, id := range ids {
		js, err := e.st.ListCurrentJobs(e.ctx, model.ScopeLecture, id)
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, j := range js {
			parts = append(parts, j.Stage+"="+j.Status)
			if j.Stage == model.StageTranslate {
				parts = append(parts, "note="+j.Stats.Note)
			}
		}
		sort.Strings(parts)
		out[name] = fmt.Sprintf("runs=%d %v", tr.get(id)-before[id], parts)
	}
	return out
}

// The resume in the first scan leaves exactly what the synchronous resume
// in Start left (the implementation before r7, run here by hand before
// Start; Start's own pass then finds nothing left to do).
func TestStartupResumeMatchesSynchronousResume(t *testing.T) {
	results := map[string]map[string]string{}
	for _, mode := range []string{"synchronous", "first scan"} {
		tr := newTrCounter()
		e, ids := startupFixture(t, tr)
		before := map[string]int{}
		for _, id := range ids {
			before[id] = tr.get(id)
		}
		e.s = e.newScheduler(allInstant(tr.stage(func() db.Store { return e.st }))...)
		if mode == "synchronous" {
			if err := e.s.resumeIncrementalNow(e.ctx); err != nil {
				t.Fatal(err)
			}
		}
		e.start()
		var all []string
		for _, id := range ids {
			all = append(all, id)
		}
		e.quiet(all...)
		time.Sleep(100 * time.Millisecond) // a wrongly started run has time to show
		e.quiet(all...)
		results[mode] = outcome(t, e, ids, tr, before)
	}
	if !reflect.DeepEqual(results["synchronous"], results["first scan"]) {
		for name := range results["synchronous"] {
			if a, b := results["synchronous"][name], results["first scan"][name]; a != b {
				t.Errorf("%s:\n synchronous %s\n first scan  %s", name, a, b)
			}
		}
	}
	got := results["first scan"]
	for name, runs := range map[string]int{"stale": 1, "none": 1, "fresh": 1, "ok": 0, "qafail": 0, "nosent": 0, "staleJob": 0, "failedJob": 0} {
		if want := fmt.Sprintf("runs=%d ", runs); len(got[name]) < len(want) || got[name][:len(want)] != want {
			t.Errorf("%s: %s, want %d translate run(s)", name, got[name], runs)
		}
	}
	for _, name := range []string{"stale", "none"} {
		if want := "note=" + state.NoteIncremental; !strings.Contains(got[name], want) {
			t.Errorf("%s: %s, want an incremental translate", name, got[name])
		}
	}
}

// Start returns while the resume still reads the zh counts (the ready line
// no longer waits for it), and the first scan dispatches nothing before the
// resume is through: a lecture not processed yet does not start meanwhile.
func TestStartDoesNotWaitForIncrementalResume(t *testing.T) {
	tr := newTrCounter()
	e, ids := startupFixture(t, tr)
	before := tr.get(ids["stale"])
	g := newGatedZh(e.st)
	e.optMut = func(o *Options) { o.Store = g }
	e.s = e.newScheduler(allInstant(tr.stage(func() db.Store { return e.st }))...)
	release := e.startGated(g)
	defer release()
	time.Sleep(100 * time.Millisecond)
	if j := e.lj(ids["fresh"], model.StagePrep); j.Status != model.StatusPending {
		t.Fatalf("a job was dispatched before the resume finished: prep %s", j.Status)
	}
	if n := tr.get(ids["stale"]); n != before {
		t.Fatalf("translate ran %d times before the resume finished", n-before)
	}
	release()
	e.waitFor("the resumed incremental translate", 5*time.Second, func() bool {
		return tr.get(ids["stale"]) == before+1 && e.lj(ids["stale"], model.StageExport).Status == model.StatusDone
	})
	e.waitStatus(ids["fresh"], model.StageExport, model.StatusDone)
}

// Review R73 (low 3, 4): Stop while the resume stands in its first lecture
// returns at once; the resume goes no further (no lecture is looked at
// with the ended context) and logs no error for the interruption; the next
// starts resume every lecture exactly once (reviewer's probe).
func TestStopDuringStartupResume(t *testing.T) {
	buf := &logBuf{}
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLog) })
	tr := newTrCounter()
	e, ids := startupFixture(t, tr)
	before := map[string]int{}
	for n, id := range ids {
		before[n] = tr.get(id)
	}
	g := newGatedZh(e.st)
	e.optMut = func(o *Options) { o.Store = g }
	e.s = e.newScheduler(allInstant(tr.stage(func() db.Store { return e.st }))...)
	release := e.startGated(g)
	defer release()
	done := make(chan struct{})
	go func() { e.s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		release()
		<-done
		t.Fatal("Stop waited for the resume")
	}
	if n := g.lateLookups.Load(); n != 0 {
		t.Errorf("the resume went on after Stop: %d lecture(s) looked at with the ended context", n)
	}
	if ls := buf.lines("resume incremental translate"); len(ls) != 0 {
		t.Errorf("the interrupted resume logged:\n%s", strings.Join(ls, "\n"))
	}
	// Two more starts, ungated: every lecture that needs it resumes once.
	for round := 0; round < 2; round++ {
		e.optMut = nil
		e.s = e.newScheduler(allInstant(tr.stage(func() db.Store { return e.st }))...)
		e.start()
		var all []string
		for _, id := range ids {
			all = append(all, id)
		}
		e.quiet(all...)
		time.Sleep(150 * time.Millisecond)
		e.quiet(all...)
		for n, want := range map[string]int{"stale": 1, "none": 1, "fresh": 1, "ok": 0, "qafail": 0, "nosent": 0, "staleJob": 0, "failedJob": 0} {
			if got := tr.get(ids[n]) - before[n]; got != want {
				t.Errorf("start %d, %s: %d translate runs since the stop, want %d", round+2, n, got, want)
			}
		}
		e.s.Stop()
	}
}

// A user's edit while the resume runs (the server answers already): each
// edited lecture gets its sentences translated once, nothing twice, nothing
// lost.
func TestEditWhileStartupResumeRuns(t *testing.T) {
	tr := newTrCounter()
	var e *env
	e = newEnv(t, nil, allInstant(tr.stage(func() db.Store { return e.st }))...)
	a, b := e.lecture(), e.lecture()
	e.start()
	e.waitStatus(a.ID, model.StageExport, model.StatusDone)
	e.waitStatus(b.ID, model.StageExport, model.StatusDone)
	e.s.Stop()
	e.zhSentences(a.ID, model.ZhOK, model.ZhStale, model.ZhOK) // a request was pending at the stop
	e.zhSentences(b.ID, model.ZhOK, model.ZhOK)
	ra, rb := tr.get(a.ID), tr.get(b.ID)

	g := newGatedZh(e.st)
	e.optMut = func(o *Options) { o.Store = g }
	e.s = e.newScheduler(allInstant(tr.stage(func() db.Store { return e.st }))...)
	release := e.startGated(g)
	defer release()
	// The user edits both lectures meanwhile (what the review and sentence
	// handlers do after their transaction).
	if err := e.st.SetZhStatus(e.ctx, a.ID, []int{2}, model.ZhStale); err != nil {
		t.Fatal(err)
	}
	e.s.RequestIncrementalTranslate(a.ID)
	if err := e.st.SetZhStatus(e.ctx, b.ID, []int{0}, model.ZhStale); err != nil {
		t.Fatal(err)
	}
	e.s.RequestIncrementalTranslate(b.ID)
	release()
	e.waitFor("both incremental runs", 5*time.Second, func() bool {
		return tr.get(a.ID) == ra+1 && tr.get(b.ID) == rb+1
	})
	e.quiet(a.ID, b.ID)
	time.Sleep(300 * time.Millisecond) // past the 60 ms debounce: a second run would show
	e.quiet(a.ID, b.ID)
	if tr.get(a.ID) != ra+1 || tr.get(b.ID) != rb+1 {
		t.Fatalf("translate runs a %d, b %d; want one each", tr.get(a.ID)-ra, tr.get(b.ID)-rb)
	}
	for _, id := range []string{a.ID, b.ID} {
		c, err := e.st.CountZh(e.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if c[model.ZhStale] != 0 || c[model.ZhNone] != 0 {
			t.Fatalf("lecture %s left untranslated: %v", id, c)
		}
	}
}

// Review R73 (low 1): an edit in a lecture the resume has not reached yet
// keeps its debounce: the resume leaves a lecture with a request since the
// start to processIncremental instead of starting it at once. The
// scheduler's clock stands still until the test moves it past the
// debounce.
func TestEditDuringStartupResumeKeepsDebounce(t *testing.T) {
	clk := &gpuTestClock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	tr := newTrCounter()
	var e *env
	e = newEnvOpts(t, nil, func(o *Options) { o.Now = clk.Now }, allInstant(tr.stage(func() db.Store { return e.st }))...)
	a, b := e.lecture(), e.lecture() // the resume reads a first
	e.start()
	e.waitStatus(a.ID, model.StageExport, model.StatusDone)
	e.waitStatus(b.ID, model.StageExport, model.StatusDone)
	e.s.Stop()
	e.zhSentences(a.ID, model.ZhOK, model.ZhStale) // a request was pending at the stop
	e.zhSentences(b.ID, model.ZhOK, model.ZhOK)
	ra, rb := tr.get(a.ID), tr.get(b.ID)
	oldB := e.lj(b.ID, model.StageTranslate)

	g := newGatedZh(e.st)
	e.optMut = func(o *Options) { o.Now = clk.Now; o.Store = g }
	e.s = e.newScheduler(allInstant(tr.stage(func() db.Store { return e.st }))...)
	release := e.startGated(g)
	defer release()
	if err := e.st.SetZhStatus(e.ctx, b.ID, []int{0}, model.ZhStale); err != nil {
		t.Fatal(err)
	}
	e.s.RequestIncrementalTranslate(b.ID) // b not reached yet
	release()
	e.waitFor("a's resumed run", 5*time.Second, func() bool {
		return tr.get(a.ID) == ra+1 && e.lj(a.ID, model.StageExport).Status == model.StatusDone
	})
	time.Sleep(150 * time.Millisecond) // many scans; the clock has not moved
	if j := e.lj(b.ID, model.StageTranslate); j.ID != oldB.ID || tr.get(b.ID) != rb {
		t.Fatalf("b's incremental run started before its debounce: translate %s %s, runs %d", j.ID, j.Status, tr.get(b.ID)-rb)
	}
	clk.Advance(100 * time.Millisecond) // past the 60 ms debounce
	e.waitFor("b's debounced run", 5*time.Second, func() bool {
		return tr.get(b.ID) == rb+1 && e.lj(b.ID, model.StageExport).Status == model.StatusDone
	})
	e.quiet(a.ID, b.ID)
	time.Sleep(150 * time.Millisecond)
	if tr.get(a.ID) != ra+1 || tr.get(b.ID) != rb+1 {
		t.Fatalf("translate runs a %d, b %d; want one each", tr.get(a.ID)-ra, tr.get(b.ID)-rb)
	}
}

// While a PauseDispatch holds (a data directory copy), the resume waits
// with the scans: the scheduler writes nothing; it runs once released.
func TestStartupResumeWaitsForPausedDispatch(t *testing.T) {
	tr := newTrCounter()
	e, ids := startupFixture(t, tr)
	before := tr.get(ids["stale"])
	e.s = e.newScheduler(allInstant(tr.stage(func() db.Store { return e.st }))...)
	old := e.lj(ids["stale"], model.StageTranslate)
	resume := e.s.PauseDispatch()
	e.start()
	time.Sleep(150 * time.Millisecond)
	if j := e.lj(ids["stale"], model.StageTranslate); j.ID != old.ID || j.Status != model.StatusDone {
		t.Fatalf("the resume wrote while dispatch was paused: translate %s %s", j.ID, j.Status)
	}
	resume()
	e.waitFor("the resumed incremental translate", 5*time.Second, func() bool {
		return tr.get(ids["stale"]) == before+1 && e.lj(ids["stale"], model.StageExport).Status == model.StatusDone
	})
}

// When resumeReviewFlags may apply a glossary fallback at this start (a
// review cancelled without its fallback), whose invalidation can turn a
// done translate stale, the incremental translates are started before it,
// inside Start, as before r7: with dispatch paused (no scan runs), the
// translate is replaced already when Start returns.
func TestStartupResumeBeforeReviewFallback(t *testing.T) {
	tr := newTrCounter()
	rs := newReviewStage()
	var e *env
	e = newEnv(t, nil, append(allInstant(tr.stage(func() db.Store { return e.st })), rs)...)
	l := e.lecture()
	e.start()
	e.waitStatus(l.ID, model.StageExport, model.StatusDone)
	e.s.Stop()
	e.zhSentences(l.ID, model.ZhOK, model.ZhStale)
	e.configureJanitor()
	pub := e.term("public")
	e.enqueueReview()
	e.drive(e.review().ID, db.JobFinish{}, model.StatusQueued, model.StatusCancelled)
	before := tr.get(l.ID)
	old := e.lj(l.ID, model.StageTranslate)

	e.s = e.newScheduler(append(allInstant(tr.stage(func() db.Store { return e.st })), rs)...)
	if !e.s.fallbackMayRun(e.ctx) {
		t.Fatal("a cancelled review without its fallback: fallbackMayRun is false")
	}
	resume := e.s.PauseDispatch()
	e.start()
	j := e.lj(l.ID, model.StageTranslate)
	if j.ID == old.ID || j.Status != model.StatusPending || !strings.Contains(j.Stats.Note, state.NoteIncremental) {
		t.Fatalf("translate after Start: %s %s %q, want the incremental run started inside Start", j.ID, j.Status, j.Stats.Note)
	}
	if e.tier(pub.ID) != model.TierReference {
		t.Fatalf("the fallback did not run at Start: tier %s", e.tier(pub.ID))
	}
	resume()
	e.waitFor("the incremental translate", 5*time.Second, func() bool {
		return tr.get(l.ID) == before+1 && e.lj(l.ID, model.StageExport).Status == model.StatusDone
	})
	// Its fallback recorded, the next start leaves the resume to the scan.
	e.s.Stop()
	e.s = e.newScheduler(append(allInstant(tr.stage(func() db.Store { return e.st })), rs)...)
	if e.s.fallbackMayRun(e.ctx) {
		t.Fatal("fallbackMayRun after the fallback ran")
	}
}

// Review R73 (middle 1): for every state of the course's review at a start,
// the fallback runs at Start exactly when fallbackMayRun says it may, and
// then the incremental resume ran before it, inside Start (with dispatch
// paused no scan runs, so a replaced translate means the resume ran in
// Start). Probe by the reviewer, kept as a table test.
func TestStartupResumeBeforeAnyPendingReviewFallback(t *testing.T) {
	type tc struct {
		name   string
		path   []string // the review's transitions from pending
		marker string   // the fallback marker: "", "old" (another end) or "match"
		due    bool     // the fallback is due at this start
	}
	for _, c := range []tc{
		{"cancelled, no marker", []string{model.StatusQueued, model.StatusCancelled}, "", true},
		{"failed, no marker", []string{model.StatusQueued, model.StatusRunning, model.StatusFailed}, "", true},
		{"cancelled, old marker", []string{model.StatusQueued, model.StatusCancelled}, "old", true},
		{"failed, old marker", []string{model.StatusQueued, model.StatusRunning, model.StatusFailed}, "old", true},
		{"failed, matching marker", []string{model.StatusQueued, model.StatusRunning, model.StatusFailed}, "match", false},
		{"cancelled, matching marker", []string{model.StatusQueued, model.StatusCancelled}, "match", false},
		{"done", []string{model.StatusQueued, model.StatusRunning, model.StatusDone}, "", false},
		{"queued", []string{model.StatusQueued}, "", false},
		{"pending", nil, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := newTrCounter()
			rs := newReviewStage()
			var e *env
			e = newEnv(t, nil, append(allInstant(tr.stage(func() db.Store { return e.st })), rs)...)
			l := e.lecture()
			e.start()
			e.waitStatus(l.ID, model.StageExport, model.StatusDone)
			e.s.Stop()
			e.zhSentences(l.ID, model.ZhOK, model.ZhStale)
			e.configureJanitor()
			pub := e.term("public")
			e.enqueueReview()
			if len(c.path) > 0 {
				e.drive(e.review().ID, db.JobFinish{}, c.path...)
			}
			switch c.marker {
			case "old":
				if err := e.st.SetKV(e.ctx, kvReviewFallbackPrefix+e.course.ID, "someoldjob/0/2026-01-01T00:00:00Z"); err != nil {
					t.Fatal(err)
				}
			case "match":
				if err := e.st.SetKV(e.ctx, kvReviewFallbackPrefix+e.course.ID, reviewStamp(e.review())); err != nil {
					t.Fatal(err)
				}
			}
			old := e.lj(l.ID, model.StageTranslate)
			tier0 := e.tier(pub.ID)
			e.s = e.newScheduler(append(allInstant(tr.stage(func() db.Store { return e.st })), rs)...)
			may := e.s.fallbackMayRun(e.ctx)
			resume := e.s.PauseDispatch()
			defer resume()
			e.start()
			j := e.lj(l.ID, model.StageTranslate)
			fellBack := e.tier(pub.ID) != tier0
			syncResumed := j.ID != old.ID
			if may != c.due || fellBack != c.due || syncResumed != c.due {
				t.Errorf("fallbackMayRun %v, fallback ran %v (tier %s → %s), resume inside Start %v (translate %s %q); want all %v",
					may, fellBack, tier0, e.tier(pub.ID), syncResumed, j.Status, j.Stats.Note, c.due)
			}
		})
	}
}
