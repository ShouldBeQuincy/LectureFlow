// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// ErrUnknownAction is returned by DoAction for anything but cancel|retry|rerun.
var ErrUnknownAction = errors.New("unknown action")

// Cancel cancels a job (SPEC §5): queued → cancelled directly; running → its
// context is cancelled (Python workers are killed by the stage) and, once
// Run returns, running → cancelled. Cancel waits up to CancelWait for that,
// so the caller usually sees the final status. Other statuses → state.ErrConflict.
func (s *Scheduler) Cancel(jobID string) error {
	ctx := context.Background()
	j, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if !j.IsCurrent {
		return fmt.Errorf("%w: job %s is not current", state.ErrConflict, jobID)
	}
	if j.Status == model.StatusQueued {
		// v2: a cancelled glossary_review gets the G2 fallback under the
		// scan lock, before any asr can start (see finishReview).
		review := j.Stage == model.StageGlossaryReview
		var ran, changed bool
		if review {
			s.scanMu.Lock()
		}
		err := state.CancelQueued(ctx, s.st, jobID)
		if err == nil && review {
			if cj, gerr := s.st.GetJob(ctx, jobID); gerr == nil {
				ran, changed = s.fallbackAfterReview(ctx, cj)
			}
		}
		if review {
			s.scanMu.Unlock()
		}
		if err == nil {
			s.mu.Lock()
			delete(s.backoff, jobID)
			delete(s.reasons, jobID)
			s.mu.Unlock()
			s.emit(ctx, jobID)
			if review {
				if ran {
					s.announceFallback(ctx, j.ScopeID, changed)
				}
				s.reviewEnded(ctx, j.ScopeID) // a cancel drops the dirty flag
			}
			s.Kick()
			return nil
		}
		if !errors.Is(err, state.ErrConflict) {
			return err
		}
		// Lost a race with the scheduler starting it.
		if j, err = s.st.GetJob(ctx, jobID); err != nil {
			return err
		}
	}
	if j.Status != model.StatusRunning {
		return fmt.Errorf("%w: %s is %s, cannot cancel", state.ErrConflict, j.Stage, j.Status)
	}
	s.mu.Lock()
	r := s.running[jobID]
	if r == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: job %s is running but not tracked by the scheduler", state.ErrConflict, jobID)
	}
	r.userCancel = true
	r.cancel()
	done := r.done
	s.mu.Unlock()
	t := time.NewTimer(s.cancelWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		slog.Warn("job did not stop within the cancel wait", "job", jobID, "wait", s.cancelWait)
	}
	return nil
}

// DoAction runs a user action on the current job of a stage (SPEC §8
// POST .../stages/{stage}/{action}): cancel | retry | rerun. retry and
// rerun are Retry/Rerun without a model choice (v1.1 A4.3), so they may
// return *state.NeedsModelChoiceError.
// Errors: state.ErrConflict (409), state.ErrMediaMissing, db.ErrNotFound,
// ErrUnknownAction.
//
// rerun holds the scan lock until state.RerunWith has committed AND removed
// the files (audio.wav, export/), so the new jobs cannot start in between.
// The API must use DoAction/Rerun rather than calling state.Rerun directly.
func (s *Scheduler) DoAction(ctx context.Context, scope, scopeID, stage, action string) error {
	switch action {
	case model.ActionCancel:
		j, err := s.st.GetCurrentJob(ctx, scope, scopeID, stage)
		if err != nil {
			return err
		}
		return s.Cancel(j.ID)
	case model.ActionRetry:
		return s.Retry(ctx, scope, scopeID, stage, "")
	case model.ActionRerun:
		return s.Rerun(ctx, scope, scopeID, stage, "")
	}
	return fmt.Errorf("%w %q", ErrUnknownAction, action)
}

// Hashers returns the registered stages as state.Hasher (for invalidation).
func (s *Scheduler) Hashers() map[string]state.Hasher {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]state.Hasher, len(s.stages))
	for name, stg := range s.stages {
		out[name] = stg
	}
	return out
}

// CheckStale runs the §4.4 invalidation for one lecture with the registered
// stages and emits events for jobs that became stale. Must not be called
// inside a Store transaction.
func (s *Scheduler) CheckStale(ctx context.Context, lectureID string) ([]model.Job, error) {
	changed, err := state.CheckStale(ctx, s.st, s.Hashers(), lectureID)
	for _, j := range changed {
		s.emitJob(ctx, j)
	}
	return changed, err
}

// InvalidateCourse runs the invalidation for every lecture, material and
// course job of a course (term edits, material upload/delete/rebind).
func (s *Scheduler) InvalidateCourse(ctx context.Context, courseID string) ([]model.Job, error) {
	changed, err := state.InvalidateCourse(ctx, s.st, s.Hashers(), courseID)
	for _, j := range changed {
		s.emitJob(ctx, j)
	}
	return changed, err
}

// ---------- incremental translate (SPEC §7.3, NOTES.md debounce) ----------

// RequestIncrementalTranslate is what review / sentence operations call
// after their transaction commits. The incremental run starts once the
// lecture has had no new request for IncrementalDebounce (default 10s), so a
// burst of reviews is handled by one run instead of interrupting the
// reviewer with 409s.
func (s *Scheduler) RequestIncrementalTranslate(lectureID string) {
	s.mu.Lock()
	s.incrReq[lectureID] = s.now()
	s.mu.Unlock()
}

// processIncremental starts the debounced requests that are due. A request
// is kept while translate is pending/queued/running (it is re-evaluated
// once that run is done) and dropped when there is nothing left to do or
// translate is failed/cancelled/stale (retry and rerun cover those).
func (s *Scheduler) processIncremental(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	var due []string
	for id, t := range s.incrReq {
		if now.Sub(t) >= s.debounce {
			due = append(due, id)
		}
	}
	s.mu.Unlock()
	for _, id := range due {
		keep, err := s.startIfNeeded(ctx, id)
		if err != nil && ctx.Err() == nil {
			slog.Error("incremental translate", "lecture", id, "err", err)
		}
		if !keep {
			s.mu.Lock()
			if t, ok := s.incrReq[id]; ok && now.Sub(t) >= s.debounce { // no newer request meanwhile
				delete(s.incrReq, id)
			}
			s.mu.Unlock()
		}
	}
}

// startIfNeeded starts an incremental translate for a lecture whose
// translate is done and has stale or untranslated sentences. keep reports
// whether the request should be kept for later.
func (s *Scheduler) startIfNeeded(ctx context.Context, lectureID string) (keep bool, err error) {
	tr, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageTranslate)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil // lecture deleted
	}
	if err != nil {
		return true, err
	}
	switch tr.Status {
	case model.StatusPending, model.StatusQueued, model.StatusRunning:
		return true, nil
	case model.StatusDone:
	default:
		return false, nil
	}
	counts, err := s.st.CountZh(ctx, lectureID)
	if err != nil {
		return true, err
	}
	if counts[model.ZhStale] == 0 && counts[model.ZhNone] == 0 {
		return false, nil
	}
	_, err = s.StartIncrementalTranslate(ctx, lectureID)
	return false, err
}

// StartIncrementalTranslate starts the §7.3 incremental translate run now
// (no debounce). When export is running at that moment the request is
// remembered and retried on every scan until it can start; it then returns
// false, nil.
func (s *Scheduler) StartIncrementalTranslate(ctx context.Context, lectureID string) (bool, error) {
	started, err := state.StartIncrementalTranslate(ctx, s.st, lectureID)
	if errors.Is(err, state.ErrBusy) {
		s.mu.Lock()
		s.deferred[lectureID] = true
		s.mu.Unlock()
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if started {
		slog.Info("incremental translate started", "lecture", lectureID)
		s.emitScope(ctx, model.ScopeLecture, lectureID)
		s.Kick()
	}
	return started, nil
}

func (s *Scheduler) retryDeferred(ctx context.Context) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.deferred))
	for id := range s.deferred {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		started, err := state.StartIncrementalTranslate(ctx, s.st, id)
		if errors.Is(err, state.ErrBusy) {
			continue
		}
		s.mu.Lock()
		delete(s.deferred, id)
		s.mu.Unlock()
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			slog.Error("deferred incremental translate", "lecture", id, "err", err)
		}
		if started {
			s.emitScope(ctx, model.ScopeLecture, id)
		}
	}
}

// resumeIncremental runs at Start: the debounce timers live in memory, so
// after a restart every lecture whose translate is done but still has stale
// or untranslated sentences gets its incremental run (resumeIncrementalNow)
// before any job is dispatched.
//
// r7 perf (P1 report H2): that pass reads every lecture's zh counts, i.e.
// every sentence (0.4–1 s on a 400-lecture library), and Start, the HTTP
// server and the ready line used to wait for it. It now runs at the
// beginning of the scheduler's first scan instead (resumeDeferred), still
// before anything is dispatched, while the server already answers. Two
// things keep the outcome the same:
//   - Start runs resumeReviewFlags right after this. When that may apply a
//     glossary fallback (fallbackMayRun), whose course invalidation can turn
//     a done translate stale, the pass runs here, before it, as it always
//     did.
//   - The first scan runs it under scanMu once no PauseDispatch is held, so
//     a data directory copy started meanwhile sees no scheduler write; a
//     lecture the user edited meanwhile is left to its debounced request
//     (resumeIncrementalNow), and state.StartIncrementalTranslate decides
//     in its transaction either way.
func (s *Scheduler) resumeIncremental(ctx context.Context) error {
	if s.fallbackMayRun(ctx) {
		return s.resumeIncrementalNow(ctx)
	}
	s.scanMu.Lock()
	s.resumePending = true
	s.scanMu.Unlock()
	return nil
}

// resumeDeferred runs the resume Start left to the first scan. Caller holds
// scanMu (scan, after its dispatch-hold check).
func (s *Scheduler) resumeDeferred(ctx context.Context) {
	if !s.resumePending {
		return
	}
	s.resumePending = false
	if err := s.resumeIncrementalNow(ctx); err != nil && ctx.Err() == nil {
		slog.Error("resume incremental translate", "err", err)
	}
}

// resumeIncrementalNow starts the incremental run of every lecture that
// needs one (startIfNeeded), course by course, lecture by lecture. A
// lecture with a request since the start (an edit while the first scan's
// resume runs) is left to processIncremental, so it keeps its debounce
// (review R73 low 1); that request's startIfNeeded decides the same.
func (s *Scheduler) resumeIncrementalNow(ctx context.Context) error {
	courses, err := s.st.ListCourses(ctx)
	if err != nil {
		return err
	}
	for _, c := range courses {
		lecs, err := s.st.ListLectures(ctx, c.ID)
		if err != nil {
			return err
		}
		for _, l := range lecs {
			if ctx.Err() != nil {
				return ctx.Err() // stopping: the next start resumes the rest
			}
			s.mu.Lock()
			_, requested := s.incrReq[l.ID]
			s.mu.Unlock()
			if requested {
				continue
			}
			if _, err := s.startIfNeeded(ctx, l.ID); err != nil && ctx.Err() == nil {
				slog.Error("resume incremental translate", "lecture", l.ID, "err", err)
			}
		}
	}
	return nil
}

// fallbackMayRun reports whether resumeReviewFlags may apply a glossary
// fallback at this start: some course's current glossary_review has an end
// without its fallback (reviewFallbackDue, the very check
// fallbackAfterReview makes; it may still skip, which only keeps the
// resume before it). A read error counts as yes: the order of Start before
// r7 is the safe one.
func (s *Scheduler) fallbackMayRun(ctx context.Context) bool {
	courses, err := s.st.ListCourses(ctx)
	if err != nil {
		return true
	}
	for _, c := range courses {
		j, err := s.st.GetCurrentJob(ctx, model.ScopeCourse, c.ID, model.StageGlossaryReview)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return true
		}
		if due, err := s.reviewFallbackDue(ctx, j); err != nil || due {
			return true
		}
	}
	return false
}

// ---------- waiting_for & status ----------

// WaitingFor answers "what is this job waiting for" (SPEC §9.5) for pending
// and queued jobs: dependencies (pending), retry backoff, or the reason of
// the last scan (queued: dependencies re-checked, cpu_slot, gpu_*, vram,
// api_slot, budget, offpeak:{HH:MM}). For a running job held in the API
// pool by the daily budget it returns "budget", by the off-peak rule of its
// provider (v2 P12.6) "offpeak:{HH:MM}". Otherwise "".
func (s *Scheduler) WaitingFor(j model.Job) string {
	switch j.Status {
	case model.StatusPending:
		ok, w, err := s.deps(context.Background(), j) // incl. no_provider (transcribe_only.go)
		if err != nil || ok {
			return ""
		}
		return w
	case model.StatusQueued:
		s.mu.Lock()
		defer s.mu.Unlock()
		if nb, ok := s.backoff[j.ID]; ok {
			if left := nb.Sub(s.now()); left > 0 {
				return backoffReason(left)
			}
		}
		return s.reasons[j.ID]
	case model.StatusRunning:
		if s.pool.BudgetBlocked(j.ID) {
			return model.WaitBudget
		}
		// v2 P12.6: its next request is held for the off-peak period.
		if until, ok := s.pool.OffpeakBlocked(j.ID); ok {
			return OffpeakReason(until)
		}
	}
	return ""
}

// SetGPUPaused persists the GPU pause flag (kv gpu_paused, SPEC §17.2).
// Paused: no new GPU job starts; with cancelRunning, running GPU jobs are
// cancelled (→ cancelled) as well. With Options.Devices it is the switch
// of every device (SPEC-v2 I8: the top bar's), and cancelRunning cancels
// the self-tests too; it then also emits Changed("devices", "").
func (s *Scheduler) SetGPUPaused(paused, cancelRunning bool) error {
	v := "0"
	if paused {
		v = "1"
	}
	if err := s.st.SetKV(context.Background(), model.KVGPUPaused, v); err != nil {
		return err
	}
	s.mu.Lock()
	s.paused = paused
	var ids []string
	if paused && cancelRunning {
		for id, r := range s.running {
			if r.res == ResourceGPU {
				ids = append(ids, id)
			}
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := s.Cancel(id); err != nil {
				slog.Warn("cancel gpu job on pause", "job", id, "err", err)
			}
		}(id)
	}
	wg.Wait()
	if paused && cancelRunning {
		s.cancelAllTests()
	}
	s.ev.Changed("system", "")
	if s.devs != nil {
		s.ev.Changed("devices", "")
	}
	s.Kick()
	return nil
}

// GPUPaused reports the pause flag.
func (s *Scheduler) GPUPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// GPUStatusTTL is how long GPUStatus reuses its last nvidia-smi reading
// (perf-fix: GET /api/system and GET /api/settings ran nvidia-smi on every
// request, 60–150 ms each). The free memory shown is at most this old; the
// scan's admission reads nvidia-smi itself, unaffected.
const GPUStatusTTL = 3 * time.Second

// readGPUCached refreshes lastGPU from nvidia-smi unless GPUStatus read it
// less than GPUStatusTTL ago. gpuReadMu is held while nvidia-smi runs, so
// concurrent callers share one run; gpuReadAt is when the last reading
// ended (zero: none yet). A caller whose context ends during the run
// records nothing (the next caller reads again).
func (s *Scheduler) readGPUCached(ctx context.Context) {
	s.gpuReadMu.Lock()
	defer s.gpuReadMu.Unlock()
	if !s.gpuReadAt.IsZero() {
		if age := s.now().Sub(s.gpuReadAt); age >= 0 && age < GPUStatusTTL {
			return
		}
	}
	m, err := s.gpuQ(ctx)
	if ctx.Err() != nil {
		return
	}
	s.rememberGPU(m, err)
	s.gpuReadAt = s.now()
}

// GPUStatus returns the GPU row of the diagnostics page: nvidia-smi's
// reading of at most GPUStatusTTL ago (not in fake ASR mode; concurrent
// callers share one run) with the live pause flag, slots and jobs. With
// Options.Devices it is the view of cuda:0 (its slots and jobs; SlotsMax 0
// without a cuda:0 row).
func (s *Scheduler) GPUStatus(ctx context.Context) GPUStatus {
	if !s.fakeASR() {
		s.readGPUCached(ctx)
	}
	slotsMax := 0
	if s.devs == nil {
		slotsMax = s.maxGPU()
	} else if d, ok := s.devs.Get(model.DeviceCUDA0); ok {
		slotsMax = s.deviceSlots(d)
	}
	s.mu.Lock()
	g := s.lastGPU
	g.Paused = s.paused
	g.SlotsMax = slotsMax
	for _, r := range s.running {
		if r.res == ResourceGPU && (s.devs == nil || r.device == model.DeviceCUDA0) {
			g.SlotsUsed++
		}
	}
	s.mu.Unlock()
	if s.fakeASR() {
		g.OK, g.Error = false, "fake ASR mode"
	}
	// the raise is read once: the estimate and EstimateRaised agree; it is
	// raised when the raise is what the estimate is (a calibration above it
	// wins)
	est, _, bump, _, _ := s.estimateBump(ctx, g.TotalMB, nil)
	g.EstimatedJobMB = est
	g.EstimateRaised = bump > 0 && bump >= est
	return g
}

// GPUTotalMB is the first GPU's total memory in MB (0 unknown) for views
// that need only that (the settings page, the CUDA self-test's compute
// type): the last successful reading's, which does not change while the
// program runs; nvidia-smi is read (GPUStatus) only while none succeeded.
func (s *Scheduler) GPUTotalMB(ctx context.Context) int {
	s.mu.Lock()
	total := s.lastGPU.TotalMB
	s.mu.Unlock()
	if total > 0 {
		return total
	}
	return s.GPUStatus(ctx).TotalMB
}

// APIStatus returns inflight/max/rpm per provider.
func (s *Scheduler) APIStatus() map[string]APIStat { return s.pool.Status() }

// BudgetHit reports whether the daily budget is exhausted (top-bar notice).
func (s *Scheduler) BudgetHit(ctx context.Context) bool { return s.pool.BudgetExceeded(ctx) }

// BudgetUnpriced is today's number of unpriced requests while the daily
// budget applies (Pool.BudgetUnpriced): the budget may be underestimated.
func (s *Scheduler) BudgetUnpriced(ctx context.Context) int64 { return s.pool.BudgetUnpriced(ctx) }

// UsageToday is today's token usage (SPEC §17.1), from the per-day ledger.
func (s *Scheduler) UsageToday(ctx context.Context) (Usage, error) { return s.pool.UsageToday(ctx) }

// RunningJobs returns the ids of jobs this scheduler is running.
func (s *Scheduler) RunningJobs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.running))
	for id := range s.running {
		out = append(out, id)
	}
	return out
}
