// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"lectureflow/internal/model"
)

// The scheduler owns job.stats while a job runs (StatsSink, stage.go). Each
// run keeps its stats in memory: AddUsage/Update only touch that copy and
// never block on I/O; the flusher persists it (SetJobStats) every flush
// interval, and finish does a last write before the final status
// transition, whatever the outcome.
//
// v1.1: the daily usage (usage_daily, budget, usage_today) is no longer
// derived from job.stats here. The llm client records every response with
// Pool.RecordUsage the moment it arrives (A10.3); the v1 kv usage_day:*
// ledger is not written any more.

// AddUsage implements StatsSink: tokens of one LLM response, Requests+1,
// and the estimated cost of that response when a price is configured.
func (r *run) AddUsage(promptTokens, completionTokens, cacheHitTokens int64) {
	d := model.JobStats{PromptTokens: promptTokens, CompletionTokens: completionTokens, CacheHitTokens: cacheHitTokens}
	cost, priced := 0.0, false
	if r.cfgCost != nil {
		cost, priced = r.cfgCost(d)
	}
	r.addUsage(promptTokens, completionTokens, cacheHitTokens, cost, priced)
}

// addUsage adds one response's tokens, Requests+1 and, when priced, its cost.
func (r *run) addUsage(promptTokens, completionTokens, cacheHitTokens int64, cost float64, priced bool) {
	now := r.clock()
	r.smu.Lock()
	r.stats.PromptTokens += promptTokens
	r.stats.CompletionTokens += completionTokens
	r.stats.CacheHitTokens += cacheHitTokens
	r.stats.Requests++
	if priced {
		r.stats.EstimatedCost += cost
	}
	r.statsDirty = true
	r.touchLocked(now)
	r.smu.Unlock()
}

// ReportUsageCost is ReportUsage with the estimated cost of the response
// as the API pool priced it (v2 P12.5: by the model's price in the period the
// request started in). With priced false — or a sink that is not the
// scheduler's run — the sink prices the tokens itself, as ReportUsage does.
func ReportUsageCost(ctx context.Context, promptTokens, completionTokens, cacheHitTokens int64, cost float64, priced bool) {
	s, ok := ctx.Value(statsKey{}).(StatsSink)
	if !ok {
		return
	}
	if r, isRun := s.(*run); isRun && priced {
		r.addUsage(promptTokens, completionTokens, cacheHitTokens, cost, true)
		return
	}
	s.AddUsage(promptTokens, completionTokens, cacheHitTokens)
}

// Update implements StatsSink: mutates non-token fields of the in-memory
// stats. Token counters and the estimated cost are owned by AddUsage, the
// request details and the last activity (v3.2.x P1-c: stats.llm,
// stats.last_activity_at) by ReportRequest and the run itself, the models of
// a lecture's translations (stats.mixed_models, v3.2.x P1-b) by state;
// changes fn makes to them are discarded. fn gets its own copy of stats.llm, so it
// cannot reach the scheduler's through the pointer either.
func (r *run) Update(fn func(*model.JobStats)) {
	now := r.clock()
	r.smu.Lock()
	defer r.smu.Unlock()
	owned := r.stats
	if owned.LLM != nil {
		c := *owned.LLM
		c.Attempts = append([]model.LLMAttempt(nil), c.Attempts...)
		if c.Current != nil {
			cur := *c.Current
			c.Current = &cur
		}
		r.stats.LLM = &c
	}
	r.stats.MixedModels = slices.Clone(owned.MixedModels)
	fn(&r.stats)
	r.stats.PromptTokens, r.stats.CompletionTokens, r.stats.CacheHitTokens = owned.PromptTokens, owned.CompletionTokens, owned.CacheHitTokens
	r.stats.Requests, r.stats.EstimatedCost = owned.Requests, owned.EstimatedCost
	r.stats.LLM, r.stats.LastActivityAt = owned.LLM, owned.LastActivityAt
	// v3.2.x P1-b: the models of the lecture's translations are recorded by
	// state (sentence retranslation, incremental runs), not by the stage.
	r.stats.MixedModels = owned.MixedModels
	r.statsDirty = true
	r.touchLocked(now)
}

func (r *run) statsSnapshot() (model.JobStats, bool) {
	r.smu.Lock()
	defer r.smu.Unlock()
	st, dirty := r.stats, r.statsDirty
	r.statsDirty = false
	return st, dirty
}

// persistStatsLocked writes the in-memory stats (after applying final, if
// non-nil). Returns the stats it wrote. Caller holds r.wmu.
func (s *Scheduler) persistStatsLocked(ctx context.Context, r *run, final func(*model.JobStats)) model.JobStats {
	st, _ := s.persistStats(ctx, r, final)
	return st
}

// persistStats is persistStatsLocked that also reports whether the stats
// had changed since the last call (and so were written, or will be tried
// again). Caller holds r.wmu.
func (s *Scheduler) persistStats(ctx context.Context, r *run, final func(*model.JobStats)) (model.JobStats, bool) {
	st, dirty := r.statsSnapshot()
	if final != nil {
		final(&st)
		dirty = true
	}
	if dirty {
		err := s.st.SetJobStats(ctx, r.job.ID, st)
		statsWriteResult(r, err, final != nil)
		if err != nil {
			r.smu.Lock()
			r.statsDirty = true // try again on the next flush
			r.smu.Unlock()
		}
	}
	return st, dirty
}

// statsWarnEvery is how often a write of a running job's stats or progress
// that keeps failing for the same reason is logged again (review R2 item 9:
// the flusher tries every flush interval, 500 ms, and used to log every
// failure).
const statsWarnEvery = time.Minute

// writeLog names the log lines of one kind of flusher write.
type writeLog struct {
	fail, again string // the WARN of a failure; the INFO of a success after unlogged failures
	suffix      string // of the statsWarns key: "" for the stats (the job id alone)
}

var (
	statsWriteLog    = writeLog{fail: "job stats", again: "job stats written again"}
	progressWriteLog = writeLog{fail: "progress write", again: "progress written again", suffix: "/progress"}
)

// statsWarns holds, by job id (plus the writeLog's suffix), the write
// failures of a run that are being counted instead of logged. An entry goes
// when a write succeeds, with the run's last stats write (finish), or for
// the progress when the run finishes (endProgressWriteLog), so it does not
// outlive the run.
var statsWarns = struct {
	mu sync.Mutex
	m  map[string]*statsWarn
}{m: map[string]*statsWarn{}}

type statsWarn struct {
	reason string    // err.Error() of the last logged failure
	logged time.Time // when it was logged (the run's clock)
	quiet  int       // failures since then that were not logged
}

// statsWriteResult logs the outcome of a stats write (writeResult).
func statsWriteResult(r *run, err error, last bool) { writeResult(r, statsWriteLog, err, last) }

// writeResult logs the outcome of a flusher write with restraint: the
// first failure, a failure for another reason, or one at least
// statsWarnEvery after the last logged one is logged with the number of
// failures not logged since ("unlogged"); the others are only counted. A
// success after unlogged failures says how many there were. The run's last
// write (last) logs a failure always and ends the count.
func writeResult(r *run, wl writeLog, err error, last bool) {
	id := r.job.ID
	key := id + wl.suffix
	statsWarns.mu.Lock()
	defer statsWarns.mu.Unlock()
	w := statsWarns.m[key]
	if err == nil {
		if w != nil {
			if w.quiet > 0 {
				slog.Info(wl.again, "job", id, "unlogged_failures", w.quiet)
			}
			delete(statsWarns.m, key)
		}
		return
	}
	now := r.clock()
	reason := err.Error()
	switch {
	case w == nil:
		slog.Warn(wl.fail, "job", id, "err", err)
	case last || w.reason != reason || now.Sub(w.logged) >= statsWarnEvery:
		slog.Warn(wl.fail, "job", id, "err", err, "unlogged", w.quiet)
	default:
		w.quiet++
		return
	}
	if last {
		delete(statsWarns.m, key)
		return
	}
	statsWarns.m[key] = &statsWarn{reason: reason, logged: now}
}

// endProgressWriteLog ends the count of r's progress write failures when
// the run finishes (no progress write comes after the flusher's): failures
// not logged yet are told with their number.
func endProgressWriteLog(r *run) {
	key := r.job.ID + progressWriteLog.suffix
	statsWarns.mu.Lock()
	defer statsWarns.mu.Unlock()
	if w := statsWarns.m[key]; w != nil {
		if w.quiet > 0 {
			slog.Warn(progressWriteLog.fail, "job", r.job.ID, "err", w.reason, "unlogged", w.quiet, "run_finished", true)
		}
		delete(statsWarns.m, key)
	}
}
