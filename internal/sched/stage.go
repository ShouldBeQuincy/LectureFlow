// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package sched runs stages under resource pools (SPEC §5).
//
// The Stage interface, ResourceKind, the job context helpers and the error
// classification below are FROZEN in Phase 0 (SPEC §16.2).
package sched

import (
	"context"
	"errors"
	"sync"
	"time"

	"lectureflow/internal/model"
)

type ResourceKind string

const (
	ResourceCPU ResourceKind = "cpu"
	ResourceGPU ResourceKind = "gpu"
	ResourceAPI ResourceKind = "api"
)

// Stage is one kind of automatic task. scopeID is a lecture, material or
// course id depending on model.ScopeOfStage(Name()).
type Stage interface {
	Name() string
	Resource() ResourceKind
	// InputHash computes the SHA-256 of the stage's inputs (SPEC §4.4). It is
	// called when the stage starts (stored in job.input_hash) and by the
	// invalidation check. Stages without a defined hash return "".
	InputHash(ctx context.Context, scopeID string) (string, error)
	// Run does the work. progress takes values in [0,1]. ctx is cancelled on
	// user cancel; Run must then return promptly (within seconds).
	// Run must write its outputs atomically (tmp file + rename / one DB
	// transaction where SPEC requires it).
	Run(ctx context.Context, scopeID string, progress func(float64)) error
}

// Provider is implemented by API stages to tell the scheduler which LLM
// provider they will use (for api_slot / budget waiting).
type ProviderStage interface {
	Provider() string
}

// APIPool is the per-provider concurrency semaphore + token bucket of SPEC §5,
// shared by every request of every API stage. Implemented by sched; used by
// the llm client around each HTTP request.
type APIPool interface {
	// Acquire blocks until the provider has a free slot, a rate token, and
	// the daily budget (SPEC §17.7) is not exhausted. It returns ctx.Err() on cancel.
	Acquire(ctx context.Context, provider string) (release func(), err error)
}

// Events receives notifications for SSE (SPEC §8.2). Implemented by api.
// Calls must not block.
type Events interface {
	Job(j model.Job)         // status or progress changed (throttling is the receiver's job)
	Changed(kind, id string) // kind: lecture|course|sentences|patches|terms|system|devices (SPEC-v2 I8, id "")
}

// NopEvents discards events (tests).
type NopEvents struct{}

func (NopEvents) Job(model.Job)          {}
func (NopEvents) Changed(string, string) {}

type jobKey struct{}

// WithJob attaches the running job to ctx. The scheduler does this before Run.
func WithJob(ctx context.Context, j model.Job) context.Context {
	return context.WithValue(ctx, jobKey{}, j)
}

// JobFrom returns the job the stage is running for (stats, logs, file names).
func JobFrom(ctx context.Context) (model.Job, bool) {
	j, ok := ctx.Value(jobKey{}).(model.Job)
	return j, ok
}

// StatsSink receives the running job's statistics. The scheduler installs one
// per run (lead decision after Gate A round 2, NOTES.md): it is the ONLY
// writer of job.stats while a job runs and persists the accumulated values on
// every flush and before the final status, whatever the outcome. Stages must
// not call Store.SetJobStats for a running job; they use UpdateStats.
type StatsSink interface {
	// AddUsage adds the token usage of one LLM response (called by the llm client).
	AddUsage(promptTokens, completionTokens, cacheHitTokens int64)
	// Update mutates non-token fields (status, note, prompt_terms_used, ...).
	Update(fn func(*model.JobStats))
}

type statsKey struct{}

// WithStatsSink attaches sink to ctx (done by the scheduler before Run).
func WithStatsSink(ctx context.Context, sink StatsSink) context.Context {
	return context.WithValue(ctx, statsKey{}, sink)
}

// ReportUsage forwards one response's usage to the run's sink, if any.
func ReportUsage(ctx context.Context, promptTokens, completionTokens, cacheHitTokens int64) {
	if s, ok := ctx.Value(statsKey{}).(StatsSink); ok {
		s.AddUsage(promptTokens, completionTokens, cacheHitTokens)
	}
}

// UpdateStats mutates the running job's stats through the run's sink, if any.
func UpdateStats(ctx context.Context, fn func(*model.JobStats)) {
	if s, ok := ctx.Value(statsKey{}).(StatsSink); ok {
		s.Update(fn)
	}
}

// Phases of a RequestEvent.
const (
	RequestStart   = "start"   // an HTTP attempt is about to be sent
	RequestEnd     = "end"     // the attempt is over (Outcome and usage are set)
	RequestBackoff = "backoff" // the call waits RetryIn before its next attempt
	RequestPhase   = "phase"   // a streamed attempt entered StreamPhase
	// RequestActivity: events of a streamed attempt keep arriving (sent at
	// most every few seconds); only the run's last activity follows it.
	RequestActivity = "activity"
)

// RequestEvent is one moment of an LLM request (v3.2.x P1-c, lead freeze):
// the llm client reports the same moments it writes to its call log, and
// the scheduler turns them into job.stats.llm (model.LLMStats) and the
// run's last activity. Usage still goes through ReportUsage / AddUsage; the
// token fields here only label the attempt.
type RequestEvent struct {
	Phase  string    // RequestStart | RequestEnd | RequestBackoff | RequestPhase
	At     time.Time // when it happened
	CallID string    // one Complete call
	N      int       // attempt within the call, 1 = first
	Try    int       // the stage's request for the same answer, 1 = first, 0 = unknown

	Role, ProviderID, Model string
	MaxTokens               int    // 0 = not sent
	Thinking                string // level sent (model.Thinking*)
	Timeout                 time.Duration

	// RequestEnd
	Outcome          string // model.LLMOutcome*
	Status           int    // HTTP status of an http_error
	EmptyText        bool
	PromptTokens     int64
	CompletionTokens int64
	CacheHitTokens   int64
	ReasoningTokens  *int64

	// RequestBackoff
	RetryIn time.Duration
	// RequestPhase
	StreamPhase string // model.LLMPhase*
}

// RequestSink is the optional extension of StatsSink that takes request
// events. The scheduler's sink implements it; a sink that does not (test
// doubles written before it) simply gets none.
type RequestSink interface {
	ReportRequest(ev RequestEvent)
}

// ReportRequest forwards a request event to the run's sink, if it takes them.
func ReportRequest(ctx context.Context, ev RequestEvent) {
	if s, ok := ctx.Value(statsKey{}).(RequestSink); ok {
		s.ReportRequest(ev)
	}
}

// StatsRecorder is an in-memory StatsSink for tests of stages run directly.
// It also records request events (RequestSink).
type StatsRecorder struct {
	mu     sync.Mutex
	Stats  model.JobStats
	Events []RequestEvent
}

func (r *StatsRecorder) ReportRequest(ev RequestEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Events = append(r.Events, ev)
}

func (r *StatsRecorder) AddUsage(p, c, h int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Stats.PromptTokens += p
	r.Stats.CompletionTokens += c
	r.Stats.CacheHitTokens += h
	r.Stats.Requests++
}

func (r *StatsRecorder) Update(fn func(*model.JobStats)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.Stats)
}

// Snapshot returns a copy of the recorded stats.
func (r *StatsRecorder) Snapshot() model.JobStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Stats
}

// permanentError marks a failure that must not be retried automatically:
// worker exit code 2 (SPEC §6.0), HTTP 400/401/403 (SPEC §12).
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the scheduler skips automatic retries.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

// IsPermanent reports whether err (or anything it wraps) is permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}
