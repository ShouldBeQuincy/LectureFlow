// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Scheduler tests of v2 phase 2: auxiliary tasks (SPEC-v2 P13.5, P7, P8.1,
// P13.3.2; lead rulings 1 and 2 of phase 2), coded job errors (P14.3,
// ruling 6) and dispatching only off-peak (P12.6, ruling 10; the pool side
// is llm-p2's). Fake stages only. Written before the implementation.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// ---------- helpers ----------

// configureOrganizer assigns the organizer role (title, filename_rule) to a
// new provider with a fake key.
func (e *env) configureOrganizer() model.Provider {
	e.t.Helper()
	p := e.provider("organizer", 4)
	e.assign(p.ID, "organizer-model", model.StageTitle)
	return p
}

func (e *env) hasJob(scope, id, stage string) bool {
	e.t.Helper()
	_, err := e.st.GetCurrentJob(e.ctx, scope, id, stage)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		e.t.Fatal(err)
	}
	return err == nil
}

func (e *env) enqueueAux(scope, id, stage string) model.Job {
	e.t.Helper()
	if _, err := e.s.EnqueueAux(e.ctx, scope, id, stage); err != nil {
		e.t.Fatalf("EnqueueAux %s: %v", stage, err)
	}
	return e.job(scope, id, stage)
}

// namedLecture creates a launched lecture with an original file name whose
// recording time is unknown (P13.3.2 candidate).
func (e *env) namedLecture(name string) model.Lecture {
	e.t.Helper()
	l := e.lecture()
	if err := e.st.SetLectureOriginalFilename(e.ctx, l.ID, name); err != nil {
		e.t.Fatal(err)
	}
	return l
}

// mediaFile writes the lecture's original media into its directory.
func (e *env) mediaFile(l model.Lecture) string {
	e.t.Helper()
	dir := model.LectureDir(e.dataDir, l.CourseID, l.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	p := filepath.Join(dir, l.MediaPath)
	if err := os.WriteFile(p, []byte("media"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// ---------- auxiliary tasks ----------

// A failed auxiliary task is never retried automatically (ruling 1): it
// stays failed with attempt 0 until the user retries it.
func TestAuxFailureNotRetried(t *testing.T) {
	var runs atomic.Int32
	title := &fakeStage{name: model.StageTitle, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		runs.Add(1)
		return errors.New("organizer: 500 internal error") // retryable for a stage
	}}
	e := newEnv(t, nil, title)
	e.configureOrganizer()
	l := e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR, model.StageSummary)
	e.enqueueAux(model.ScopeLecture, l.ID, model.StageTitle)
	e.start()
	j := e.waitStatus(l.ID, model.StageTitle, model.StatusFailed)
	time.Sleep(250 * time.Millisecond) // well past both retry backoffs (60, 120 ms)
	j = e.lj(l.ID, model.StageTitle)
	if j.Status != model.StatusFailed || j.Attempt != 0 || runs.Load() != 1 {
		t.Fatalf("title = %s attempt %d, runs %d", j.Status, j.Attempt, runs.Load())
	}
	if j.Error == nil || *j.Error != "organizer: 500 internal error" {
		t.Fatalf("error = %v", j.Error)
	}
	if w := e.s.WaitingFor(j); w != "" {
		t.Fatalf("waiting_for = %q", w)
	}
	// The user's retry runs it again (DoAction retry, no model question).
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageTitle, model.ActionRetry); err != nil {
		t.Fatal(err)
	}
	e.waitFor("second run", 5*time.Second, func() bool { return runs.Load() == 2 })
	e.waitStatus(l.ID, model.StageTitle, model.StatusFailed)
	// No rerun for an auxiliary task.
	if err := e.s.DoAction(e.ctx, model.ScopeLecture, l.ID, model.StageTitle, model.ActionRerun); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("rerun of an auxiliary task: %v", err)
	}
}

// Lecture-scope auxiliary tasks use their role's current setting, not a
// snapshot (ruling 1).
func TestAuxProviderIsRoleCurrent(t *testing.T) {
	e := newEnv(t, nil)
	org := e.configureOrganizer()
	chap := e.provider("chapters", 2)
	e.assign(chap.ID, "chapters-model", model.StageChapters)
	l := e.lecture()
	stg := &fakeStage{name: "x", res: ResourceAPI, provider: "fallback"}
	cases := map[string]string{model.StageTitle: org.ID, model.StageChapters: chap.ID}
	for stage, want := range cases {
		j := e.enqueueAux(model.ScopeLecture, l.ID, stage)
		if got := e.s.providerOf(stg, j); got != want {
			t.Errorf("%s: provider %q, want %q", stage, got, want)
		}
	}
	fr := e.enqueueAux(model.ScopeCourse, e.course.ID, model.StageFilenameRule)
	if got := e.s.providerOf(stg, fr); got != org.ID {
		t.Errorf("filename_rule: provider %q, want %q", got, org.ID)
	}
	// A pipeline stage still uses its snapshot.
	if got := e.s.providerOf(stg, e.lj(l.ID, model.StageSummary)); got != "deepseek" {
		t.Errorf("summary: provider %q", got)
	}
}

// ruling 2: every completion of summary (also after a rerun) queues title;
// nothing when the organizer role is not configured.
func TestSummaryDoneQueuesTitle(t *testing.T) {
	var titles atomic.Int32
	title := &fakeStage{name: model.StageTitle, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		titles.Add(1)
		return nil
	}}
	e := newEnv(t, nil, instant(model.StageSummary, ResourceAPI), title)
	if ok, err := state.RoleConfigured(e.ctx, e.st, model.RoleOrganizer); err != nil || ok {
		t.Fatalf("organizer must start unconfigured: %v %v", ok, err)
	}
	l := e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR)
	e.start()
	e.waitStatus(l.ID, model.StageSummary, model.StatusDone)
	time.Sleep(60 * time.Millisecond)
	if e.hasJob(model.ScopeLecture, l.ID, model.StageTitle) {
		t.Fatal("title queued without an organizer role")
	}

	e.configureOrganizer()
	l2 := e.lecture()
	e.stagesDone(l2.ID, model.StagePrep, model.StageASR)
	e.s.Kick()
	e.waitStatus(l2.ID, model.StageSummary, model.StatusDone)
	e.waitFor("title queued and run", 5*time.Second, func() bool {
		return e.hasJob(model.ScopeLecture, l2.ID, model.StageTitle) && e.lj(l2.ID, model.StageTitle).Status == model.StatusDone
	})
	first := e.lj(l2.ID, model.StageTitle)
	if titles.Load() != 1 {
		t.Fatalf("title runs = %d", titles.Load())
	}
	// Rerun the summary: done again → a new title job.
	if err := e.s.Rerun(e.ctx, model.ScopeLecture, l2.ID, model.StageSummary, model.ModelChoiceSnapshot); err != nil {
		t.Fatal(err)
	}
	e.waitFor("second title", 5*time.Second, func() bool {
		j := e.lj(l2.ID, model.StageTitle)
		return j.ID != first.ID && j.Status == model.StatusDone
	})
	if titles.Load() != 2 {
		t.Fatalf("title runs = %d", titles.Load())
	}
	// The title job never shows up in the pipeline.
	jobs, _ := e.st.ListCurrentJobs(e.ctx, model.ScopeLecture, l2.ID)
	if got := state.EarliestStale(jobs); got != "" {
		t.Fatalf("EarliestStale = %q", got)
	}
}

// ---------- P13.3.2 filename_rule trigger ----------

// fnSettled waits until the course's filename-rule request was evaluated
// and dropped by a scan (a request kept for a running rule never settles).
func (e *env) fnSettled(courseID string) {
	e.t.Helper()
	e.waitFor("the filename-rule request evaluated", 5*time.Second, func() bool {
		e.s.mu.Lock()
		defer e.s.mu.Unlock()
		return e.s.fnReq[courseID] == nil
	})
}

func TestRequestFilenameRuleDebounced(t *testing.T) {
	// The debounce runs on the scheduler's clock, which only moves when the
	// test moves it: real 15 ms sleeps between the requests could add up to
	// more than the debounce on a loaded machine.
	clk := &gpuTestClock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	e := newEnvOpts(t, nil, func(o *Options) { o.FilenameRuleDebounce = 80 * time.Millisecond; o.Now = clk.Now })
	e.configureOrganizer()
	e.namedLecture("CSC1003_2026-09-30_1000.mp4")
	e.namedLecture("CSC1003_2026-10-07_1000.mp4")
	e.start()
	for range 3 { // one batch upload
		e.s.RequestFilenameRule(e.course.ID)
		clk.Advance(15 * time.Millisecond)
		time.Sleep(15 * time.Millisecond) // several scans
	}
	clk.Advance(64 * time.Millisecond) // 79 ms after the last request
	time.Sleep(15 * time.Millisecond)
	if e.hasJob(model.ScopeCourse, e.course.ID, model.StageFilenameRule) {
		t.Fatal("queued before the debounce elapsed")
	}
	clk.Advance(time.Millisecond) // 80 ms: due
	e.waitFor("filename_rule job", 5*time.Second, func() bool {
		return e.hasJob(model.ScopeCourse, e.course.ID, model.StageFilenameRule)
	})
	first := e.job(model.ScopeCourse, e.course.ID, model.StageFilenameRule)
	e.fnSettled(e.course.ID)
	clk.Advance(200 * time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if j := e.job(model.ScopeCourse, e.course.ID, model.StageFilenameRule); j.ID != first.ID {
		t.Fatal("one batch must queue one run")
	}
	// No stage registered: it waits queued, without snapshot.
	if first.Status != model.StatusPending && first.Status != model.StatusQueued || !model.IsEmptySnapshot(first.ConfigSnapshot) {
		t.Fatalf("job = %+v", first)
	}
}

func TestRequestFilenameRuleConditions(t *testing.T) {
	debounce := func(o *Options) { o.FilenameRuleDebounce = 20 * time.Millisecond }
	// Wait for a scan to evaluate the request, not a fixed 150 ms (too short
	// on a loaded machine for the job to be queued in time).
	settle := func(e *env) {
		e.s.RequestFilenameRule(e.course.ID)
		e.fnSettled(e.course.ID)
	}
	// 3.1.0 "只转写": without a model the task still runs, with the
	// built-in Panopto pattern only (internal/stages filename_fallback.go).
	t.Run("organizer not configured", func(t *testing.T) {
		e := newEnvOpts(t, nil, debounce)
		if ok, err := state.RoleConfigured(e.ctx, e.st, model.RoleOrganizer); err != nil || ok {
			t.Fatalf("organizer must start unconfigured: %v %v", ok, err)
		}
		e.namedLecture("a_2026-09-30.mp4")
		e.start()
		settle(e)
		if !e.hasJob(model.ScopeCourse, e.course.ID, model.StageFilenameRule) {
			t.Fatal("not queued without the organizer role")
		}
	})
	t.Run("names already used", func(t *testing.T) {
		e := newEnvOpts(t, nil, debounce)
		e.configureOrganizer()
		e.namedLecture("a_2026-09-30.mp4")
		b, _ := json.Marshal([]string{"a_2026-09-30.mp4"})
		e.st.SetKV(e.ctx, model.KVFilenameSamples+e.course.ID, string(b))
		e.start()
		settle(e)
		if e.hasJob(model.ScopeCourse, e.course.ID, model.StageFilenameRule) {
			t.Fatal("queued for names already used")
		}
	})
	t.Run("user rule", func(t *testing.T) {
		e := newEnvOpts(t, nil, debounce)
		e.configureOrganizer()
		e.namedLecture("a_2026-09-30.mp4")
		rule := `{"regex":"^a","source":"user"}`
		e.st.SetCourseFilenameRule(e.ctx, e.course.ID, &rule)
		e.start()
		settle(e)
		if e.hasJob(model.ScopeCourse, e.course.ID, model.StageFilenameRule) {
			t.Fatal("a user rule must not be regenerated automatically")
		}
	})
	t.Run("unknown course", func(t *testing.T) {
		e := newEnvOpts(t, nil, debounce)
		e.configureOrganizer()
		e.start()
		e.s.RequestFilenameRule("no-such-course")
		e.fnSettled("no-such-course") // nothing to check but that nothing breaks
	})
}

// A request that arrives while the rule is being generated waits for that
// run: after a done run it is evaluated again (the new upload's name was
// not a sample), after a failed run it is dropped (no automatic retry).
func TestRequestFilenameRuleDuringRun(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "done"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			var runs atomic.Int32
			release := make(chan struct{})
			stg := &fakeStage{name: model.StageFilenameRule, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
				if runs.Add(1) == 1 {
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					if fail {
						return errors.New("rule did not validate")
					}
				}
				return nil
			}}
			e := newEnvOpts(t, nil, func(o *Options) { o.FilenameRuleDebounce = 20 * time.Millisecond }, stg)
			e.configureOrganizer()
			e.namedLecture("a_2026-09-30.mp4")
			e.start()
			e.s.RequestFilenameRule(e.course.ID)
			e.waitFor("first run", 5*time.Second, func() bool { return runs.Load() == 1 })
			first := e.job(model.ScopeCourse, e.course.ID, model.StageFilenameRule)
			e.namedLecture("a_2026-10-07.mp4") // uploaded while the rule is generated
			e.s.RequestFilenameRule(e.course.ID)
			// The request is due while the run is running: a scan finds the
			// run and keeps the request for its end (fnRequest.waited). Not a
			// fixed sleep: on a loaded machine the run could end first, and a
			// request that never waited is a new trigger (flake-hunt #8).
			e.waitFor("the request to wait for the run", 5*time.Second, func() bool {
				e.s.mu.Lock()
				defer e.s.mu.Unlock()
				r := e.s.fnReq[e.course.ID]
				return r != nil && r.waited
			})
			if j := e.job(model.ScopeCourse, e.course.ID, model.StageFilenameRule); j.ID != first.ID || j.Status != model.StatusRunning {
				t.Fatalf("while running: %+v", j)
			}
			close(release)
			if fail {
				e.waitFor("failed", 5*time.Second, func() bool {
					return e.job(model.ScopeCourse, e.course.ID, model.StageFilenameRule).Status == model.StatusFailed
				})
				time.Sleep(150 * time.Millisecond)
				if j := e.job(model.ScopeCourse, e.course.ID, model.StageFilenameRule); j.ID != first.ID || j.Status != model.StatusFailed || runs.Load() != 1 {
					t.Fatalf("after a failed run: %+v, runs %d", j, runs.Load())
				}
				// A later upload is a new trigger.
				e.namedLecture("a_2026-10-14.mp4")
				e.s.RequestFilenameRule(e.course.ID)
				e.waitFor("new trigger", 5*time.Second, func() bool { return runs.Load() == 2 })
				return
			}
			e.waitFor("second run", 5*time.Second, func() bool { return runs.Load() == 2 })
			time.Sleep(150 * time.Millisecond)
			if runs.Load() != 2 {
				t.Fatalf("runs = %d, want exactly 2", runs.Load())
			}
		})
	}
}

// ---------- P8.1 stream for lectures prepared before v2 ----------

func TestEnsureStream(t *testing.T) {
	e := newEnv(t, nil)
	ctx := e.ctx
	ensure := func(what string, id string, want bool) {
		t.Helper()
		got, err := e.s.EnsureStream(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got != want {
			t.Fatalf("%s: EnsureStream = %v, want %v", what, got, want)
		}
	}
	if _, err := e.s.EnsureStream(ctx, "no-such-lecture"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown lecture: %v", err)
	}
	l := e.lecture()
	e.mediaFile(l)
	ensure("prep not done", l.ID, false)
	if e.hasJob(model.ScopeLecture, l.ID, model.StageStream) {
		t.Fatal("stream queued before prep")
	}
	e.stagesDone(l.ID, model.StagePrep)
	ensure("old lecture without stream.mp4", l.ID, true)
	j := e.lj(l.ID, model.StageStream)
	if j.Status != model.StatusPending || !model.IsEmptySnapshot(j.ConfigSnapshot) {
		t.Fatalf("stream job = %+v", j)
	}
	ensure("asked again", l.ID, true)
	if e.lj(l.ID, model.StageStream).ID != j.ID {
		t.Fatal("a second request must not queue a second job")
	}

	// failed: only the user's retry starts it again.
	e.drive(j.ID, db.JobFinish{}, model.StatusQueued, model.StatusRunning, model.StatusFailed)
	ensure("failed", l.ID, false)
	if e.lj(l.ID, model.StageStream).ID != j.ID {
		t.Fatal("a failed stream must not be replaced")
	}
	if err := e.s.DoAction(ctx, model.ScopeLecture, l.ID, model.StageStream, model.ActionRetry); err != nil {
		t.Fatal(err)
	}
	e.drive(j.ID, db.JobFinish{}, model.StatusCancelled)
	ensure("cancelled", l.ID, false)

	// done without stream.mp4: a recorded result without a file (audio
	// only, no space, failed — by prep or a stream task) is never retried by
	// itself; "ready" with the file gone (or no record) runs again.
	e.drive(e.enqueueAux(model.ScopeLecture, l.ID, model.StageStream).ID, db.JobFinish{}, model.StatusQueued, model.StatusRunning, model.StatusDone)
	setState := func(id, status string) {
		b, _ := json.Marshal(model.StreamState{Status: status})
		if err := e.st.SetKV(ctx, model.KVStreamPrefix+id, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"audio_only", "no_space", "failed"} {
		setState(l.ID, status)
		ensure(status, l.ID, false)
	}
	setState(l.ID, "ready")
	ensure("ready but the file is gone", l.ID, true)
	// The same for a lecture whose prep recorded the result (no stream task).
	lp := e.lecture()
	e.mediaFile(lp)
	e.stagesDone(lp.ID, model.StagePrep)
	setState(lp.ID, "failed")
	ensure("prep recorded a failure", lp.ID, false)
	if e.hasJob(model.ScopeLecture, lp.ID, model.StageStream) {
		t.Fatal("stream queued after prep recorded a failure")
	}

	// stream.mp4 exists → nothing to do.
	dir := model.LectureDir(e.dataDir, l.CourseID, l.ID)
	os.WriteFile(filepath.Join(dir, model.StreamFile), []byte("mp4"), 0o644)
	ensure("stream.mp4 now exists", l.ID, false)
	l2 := e.lecture()
	e.mediaFile(l2)
	e.stagesDone(l2.ID, model.StagePrep)
	os.WriteFile(filepath.Join(model.LectureDir(e.dataDir, l2.CourseID, l2.ID), model.StreamFile), []byte("mp4"), 0o644)
	ensure("stream.mp4 exists", l2.ID, false)

	// The original media is gone (keep_original_media = false).
	l3 := e.lecture()
	e.stagesDone(l3.ID, model.StagePrep)
	ensure("no original media", l3.ID, false)

	// A draft never.
	d := e.draft()
	e.mediaFile(d)
	ensure("draft", d.ID, false)
}

// ---------- cancel and delete take the auxiliary tasks along ----------

func TestDeleteLectureCancelsAux(t *testing.T) {
	var stopped atomic.Bool
	entered := make(chan struct{})
	stream := &fakeStage{name: model.StageStream, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		close(entered)
		<-ctx.Done()
		stopped.Store(true)
		return ctx.Err()
	}}
	e := newEnvOpts(t, nil, func(o *Options) { o.DeleteWait = 3 * time.Second }, stream)
	l := e.lecture()
	e.mediaFile(l)
	e.stagesDone(l.ID, model.StagePrep)
	e.enqueueAux(model.ScopeLecture, l.ID, model.StageStream)
	e.configureOrganizer()
	e.enqueueAux(model.ScopeLecture, l.ID, model.StageTitle) // pending: waits for summary
	e.start()
	<-entered
	jobs, _ := e.st.ListCurrentJobs(e.ctx, model.ScopeLecture, l.ID)
	if !state.CanDelete(jobs) {
		t.Fatal("a running auxiliary task must not block deletion")
	}
	// No force needed: the auxiliary task is cancelled and waited for.
	if err := e.s.DeleteLecture(e.ctx, l.ID, false); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() {
		t.Fatal("the stream task was not cancelled")
	}
	if _, err := e.st.GetLecture(e.ctx, l.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("lecture: %v", err)
	}
	if exists(model.LectureDir(e.dataDir, l.CourseID, l.ID)) {
		t.Fatal("lecture directory left behind")
	}
}

func TestCancelLectureCancelsAux(t *testing.T) {
	g := newGate()
	e := newEnv(t, nil, &fakeStage{name: model.StageStream, res: ResourceCPU, run: g.run})
	l := e.lecture()
	e.mediaFile(l)
	e.stagesDone(l.ID, model.StagePrep)
	e.enqueueAux(model.ScopeLecture, l.ID, model.StageStream)
	e.start()
	e.waitStatus(l.ID, model.StageStream, model.StatusRunning)
	n, err := e.s.CancelLecture(e.ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j := e.lj(l.ID, model.StageStream); j.Status != model.StatusCancelled {
		t.Fatalf("stream = %s", j.Status)
	}
	if n < 1 {
		t.Fatalf("cancelled %d jobs", n)
	}
}

// An auxiliary task never becomes stale after its run (no invalidation).
func TestAuxNoPostRunStale(t *testing.T) {
	var h atomic.Int32
	chapters := &fakeStage{name: model.StageChapters, res: ResourceAPI,
		hash: func(ctx context.Context, id string) (string, error) {
			return string(rune('a' + h.Add(1))), nil // differs on every call
		}}
	e := newEnv(t, nil, chapters)
	p := e.provider("chap", 2)
	e.assign(p.ID, "m", model.StageChapters)
	l := e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR, model.StageSummary)
	e.enqueueAux(model.ScopeLecture, l.ID, model.StageChapters)
	e.start()
	e.waitStatus(l.ID, model.StageChapters, model.StatusDone)
	time.Sleep(50 * time.Millisecond)
	if j := e.lj(l.ID, model.StageChapters); j.Status != model.StatusDone {
		t.Fatalf("chapters = %s", j.Status)
	}
}

// ---------- P14.3 coded job errors ----------

func TestCodedErrorStoredAsJSON(t *testing.T) {
	inner := errors.New("ffmpeg: Invalid data found when processing input")
	var calls atomic.Int32
	prep := &fakeStage{name: model.StagePrep, res: ResourceCPU, run: func(ctx context.Context, id string, p func(float64)) error {
		if calls.Add(1) == 1 {
			return Permanent(model.Errorf("media.corrupt", map[string]any{"file": "a.mp4"}, inner))
		}
		return Permanent(errors.New("plain failure"))
	}}
	e := newEnv(t, nil, prep)
	l1 := e.lecture()
	e.start()
	j := e.waitStatus(l1.ID, model.StagePrep, model.StatusFailed)
	if j.Error == nil {
		t.Fatal("no error stored")
	}
	info, text := state.ParseJobError(*j.Error)
	if info == nil || info.Code != "media.corrupt" || info.Params["file"] != "a.mp4" || info.Detail != inner.Error() || text != inner.Error() {
		t.Fatalf("stored %q → %+v %q", *j.Error, info, text)
	}
	l2 := e.lecture()
	j = e.waitStatus(l2.ID, model.StagePrep, model.StatusFailed)
	if j.Error == nil || *j.Error != "plain failure" {
		t.Fatalf("plain error stored as %v", j.Error)
	}
	if info, _ := state.ParseJobError(*j.Error); info != nil {
		t.Fatalf("plain error parsed as %+v", info)
	}
}

// ---------- P12.6 dispatch only off-peak (scheduler side) ----------

// A job whose provider is offpeak_only and in its peak period stays queued
// with waiting_for "offpeak:{HH:MM}"; an always-peak tariff never changes
// ("--:--").
func TestOffpeakOnlyHoldsAPIJobs(t *testing.T) {
	var started atomic.Bool
	sum := &fakeStage{name: model.StageSummary, res: ResourceAPI, run: func(ctx context.Context, id string, p func(float64)) error {
		started.Store(true)
		return nil
	}}
	e := newEnv(t, nil, sum)
	name, typ, url, key := "peaky", model.ProviderOpenAI, "https://peaky.example.test/v1", "sk-test-peaky"
	tariff := `{"timezone":"UTC","peak_windows":[{"days":[1,2,3,4,5,6,7],"from":"00:00","to":"24:00"}],"offpeak_multiplier":0.5}`
	only := true
	p, err := e.s.set.CreateProvider(e.ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key, Tariff: &tariff, OffpeakOnly: &only})
	if err != nil {
		t.Fatal(err)
	}
	e.assign(p.ID, "m", model.StageSummary)
	l := e.lecture()
	e.stagesDone(l.ID, model.StagePrep, model.StageASR)
	e.start()
	var j model.Job
	e.waitFor("summary held off-peak", 5*time.Second, func() bool {
		j = e.lj(l.ID, model.StageSummary)
		return j.Status == model.StatusQueued && e.s.WaitingFor(j) == model.WaitOffpeak("--:--")
	})
	time.Sleep(50 * time.Millisecond)
	if started.Load() {
		t.Fatal("a job of an offpeak_only provider started in its peak period")
	}
}
