// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"time"

	"lectureflow/internal/model"
)

// v3.2.x P1-c: the run is the RequestSink of its job. From the llm client's
// request events it keeps job.stats.llm (model.LLMStats: what is in flight,
// the attempt a single-request stage waits for, the last attempts worth
// showing, the retries) and job.stats.last_activity_at. Both belong to the
// scheduler: a stage's UpdateStats cannot change them (stats.go), and the
// flusher persists and announces them like the token counters.
//
// Everything here runs under r.smu and never blocks on I/O. The LLMStats
// value in r.stats is replaced, never changed in place: a snapshot handed
// to the flusher (statsSnapshot) stays consistent while events keep coming.

// llmCall is the latest attempt of one Complete call that is not over.
type llmCall struct {
	att      model.LLMAttempt
	inFlight bool      // started and not ended
	retryAt  time.Time // the call waits for its next attempt until then (zero: no backoff)
	seq      uint64    // order of the starts, for ties
}

// llmCallsPruneAt: above this many tracked calls, those that ended without
// a next attempt in sight are dropped (the client reports no "gave up").
const llmCallsPruneAt = 32

// llmBackoffGrace: how long past its retry time a waiting call is still
// shown before it counts as abandoned (a cancel during the backoff).
const llmBackoffGrace = time.Minute

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// clock is the run's time source (the scheduler's; time.Now for a bare run).
func (r *run) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// touchLocked records that the run showed life at t. The stats only become
// dirty when the stored second changes, so a stage that reports progress
// many times a second does not cause a write per call. Caller holds r.smu.
func (r *run) touchLocked(t time.Time) {
	if t.IsZero() || !t.After(r.activity) {
		return
	}
	r.activity = t
	if s := stamp(t); s != r.stats.LastActivityAt {
		r.stats.LastActivityAt = s
		r.statsDirty = true
	}
}

// touch is touchLocked(now) for callers outside r.smu (the progress callback).
func (r *run) touch() {
	now := r.clock()
	r.smu.Lock()
	r.touchLocked(now)
	r.smu.Unlock()
}

// ReportRequest implements RequestSink.
func (r *run) ReportRequest(ev RequestEvent) {
	at := ev.At
	if at.IsZero() {
		at = r.clock()
	}
	r.smu.Lock()
	defer r.smu.Unlock()
	r.touchLocked(at)
	if ev.CallID == "" || ev.Phase == RequestActivity {
		return
	}
	if r.calls == nil {
		r.calls = map[string]*llmCall{}
	}
	prev := r.stats.LLM
	next := model.LLMStats{}
	if prev != nil {
		next.Attempts, next.Retries = prev.Attempts, prev.Retries
	}
	switch ev.Phase {
	case RequestStart:
		r.pruneCallsLocked(at)
		r.callSeq++
		r.calls[ev.CallID] = &llmCall{att: attemptOf(ev, at), inFlight: true, seq: r.callSeq}
		if ev.N > 1 {
			next.Retries++
		}
	case RequestPhase:
		if c := r.calls[ev.CallID]; c != nil && c.inFlight {
			c.att.Phase = ev.StreamPhase
		}
	case RequestEnd:
		c := r.calls[ev.CallID]
		if c == nil || !c.inFlight || c.att.N != ev.N {
			// the start was not seen (or belongs to another attempt)
			r.callSeq++
			c = &llmCall{att: attemptOf(ev, time.Time{}), seq: r.callSeq}
			r.calls[ev.CallID] = c
		}
		c.inFlight, c.retryAt = false, time.Time{}
		a := &c.att
		a.Phase, a.RetryAt = "", ""
		a.EndedAt, a.Outcome, a.Status, a.EmptyText = stamp(at), ev.Outcome, ev.Status, ev.EmptyText
		a.PromptTokens, a.CompletionTokens, a.CacheHitTokens = ev.PromptTokens, ev.CompletionTokens, ev.CacheHitTokens
		a.ReasoningTokens = nil
		if ev.ReasoningTokens != nil {
			v := *ev.ReasoningTokens
			a.ReasoningTokens = &v
		}
		unbilledCancel := a.Outcome == model.LLMOutcomeCanceled && !hasTokens(a)
		if !unbilledCancel && (a.Outcome != model.LLMOutcomeOK || a.N > 1) {
			// worth showing: not ok, or a later attempt of its call. Not a
			// cancel that cost nothing (R8 BC-01): closing the window or
			// canceling the job cuts every request in flight short, and a
			// row of "1st attempt · canceled" tells the user nothing.
			next.Attempts = appendAttempt(next.Attempts, *a)
		}
		if a.Outcome == model.LLMOutcomeOK || unbilledCancel {
			delete(r.calls, ev.CallID) // the call is over (a cancel is never retried)
		}
	case RequestBackoff:
		c := r.calls[ev.CallID]
		if c == nil {
			r.callSeq++
			c = &llmCall{att: attemptOf(ev, time.Time{}), seq: r.callSeq}
			r.calls[ev.CallID] = c
		}
		if !c.inFlight {
			c.retryAt = at.Add(ev.RetryIn)
			c.att.RetryAt = stamp(c.retryAt)
		}
	default:
		return
	}
	next.InFlight, next.Current = r.currentLocked(at)
	r.setLLMLocked(next)
}

// attemptOf is the attempt an event describes; started is its start time
// (zero: not known).
func attemptOf(ev RequestEvent, started time.Time) model.LLMAttempt {
	a := model.LLMAttempt{CallID: ev.CallID, N: ev.N, Try: ev.Try, Role: ev.Role, ProviderID: ev.ProviderID,
		Model: ev.Model, MaxTokens: ev.MaxTokens, Thinking: ev.Thinking, TimeoutMS: ev.Timeout.Milliseconds()}
	if !started.IsZero() {
		a.StartedAt = stamp(started)
	}
	return a
}

// hasTokens reports whether a finished attempt counted any token (the
// provider bills it).
func hasTokens(a *model.LLMAttempt) bool {
	return a.PromptTokens > 0 || a.CompletionTokens > 0 || a.CacheHitTokens > 0 ||
		(a.ReasoningTokens != nil && *a.ReasoningTokens > 0)
}

// appendAttempt returns list plus a, limited to the last
// model.LLMAttemptsKept, in a new slice (list is shared with snapshots).
func appendAttempt(list []model.LLMAttempt, a model.LLMAttempt) []model.LLMAttempt {
	out := make([]model.LLMAttempt, 0, len(list)+1)
	out = append(append(out, list...), a)
	if n := len(out) - model.LLMAttemptsKept; n > 0 {
		out = out[n:]
	}
	return out
}

// currentLocked counts the attempts in flight and picks LLMStats.Current:
// among the attempts in flight and the calls waiting out a backoff, the one
// with the highest attempt number (then one in flight, then the latest
// start). A copy, so later events do not change a snapshot.
func (r *run) currentLocked(now time.Time) (inFlight int, cur *model.LLMAttempt) {
	var best *llmCall
	for _, c := range r.calls {
		if c.inFlight {
			inFlight++
		} else if c.retryAt.IsZero() || now.After(c.retryAt.Add(llmBackoffGrace)) {
			continue
		}
		switch {
		case best == nil, c.att.N > best.att.N:
			best = c
		case c.att.N < best.att.N:
		case c.inFlight != best.inFlight:
			if c.inFlight {
				best = c
			}
		case c.seq > best.seq:
			best = c
		}
	}
	if best != nil {
		a := best.att
		cur = &a
	}
	return inFlight, cur
}

// pruneCallsLocked drops calls that ended without a next attempt in sight
// once many are tracked: the client reports no "gave up".
func (r *run) pruneCallsLocked(now time.Time) {
	if len(r.calls) < llmCallsPruneAt {
		return
	}
	for id, c := range r.calls {
		if !c.inFlight && (c.retryAt.IsZero() || now.After(c.retryAt.Add(llmBackoffGrace))) {
			delete(r.calls, id)
		}
	}
}

// setLLMLocked stores v as stats.llm (nil when it says nothing).
func (r *run) setLLMLocked(v model.LLMStats) {
	if v.InFlight == 0 && v.Current == nil && len(v.Attempts) == 0 && v.Retries == 0 {
		if r.stats.LLM != nil {
			r.stats.LLM, r.statsDirty = nil, true
		}
		return
	}
	r.stats.LLM, r.statsDirty = &v, true
}

// settledLLM is stats.llm of a job that is not running: nothing in flight,
// no current attempt; the finished attempts and the retry count stay. The
// result never shares its struct with in.
func settledLLM(in *model.LLMStats) *model.LLMStats {
	if in == nil || (len(in.Attempts) == 0 && in.Retries == 0) {
		return nil
	}
	return &model.LLMStats{Attempts: in.Attempts, Retries: in.Retries}
}
