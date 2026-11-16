// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"sync"
	"time"

	"lectureflow/internal/llm"
)

// Prompt-cache warm-up (perf-llm, NOTES.md). The requests of a translate or
// repair run share one long system prompt (the §13 instructions, the
// glossary, the summary, for repair also the slides). DeepSeek caches a
// prefix only once it has processed a request with it; requests sent
// together with the first one all pay the full price for it. So the first
// request goes out alone and the others follow when the warm-up ends. Only
// the order of sending changes: requests, contexts, QA and the per-sentence
// writes are exactly as before (SPEC §7, §13 untouched).

// warmup says when the other items of a run may start.
type warmup struct {
	// afterSend > 0 ends the warm-up this long after the first item's
	// first request was sent (llm.WithSentHook), without waiting for its
	// answer; 0 waits for the first item to finish.
	afterSend time.Duration
	// afterFirstEvent ends the warm-up when the first item's streamed
	// answer begins (llm.WithFirstEventHook: its message_start), without
	// waiting for the rest of the answer. A first item whose answer never
	// begins that way (not streamed, failed or retried before it) ends the
	// warm-up as without it: when it finishes, or at max.
	afterFirstEvent bool
	// max ends the warm-up at the latest this long after the first item
	// started (it may wait in the API pool, or be retried); with a second
	// step, it also caps that step, counted from when it started.
	max time.Duration
	// second > 0 adds a second step (batch translation on DeepSeek, see
	// translateBatchDeepSeekWarmup): when the warm-up ends, items 1..second
	// run, and the others start only when all of them finished. 0: no
	// second step, the others start when the warm-up ends.
	second int
}

var (
	// translateWarmup: a translate request answers in about a second and its
	// answer means the prefix was processed; production data (8 / 300) showed
	// the requests sent after the first answer hitting the cache.
	translateWarmup = warmup{max: 15 * time.Second}
	// repairWarmup: a repair chunk takes 6–10 s to generate, but its prefix
	// is read in the first second; the other chunks follow 2 s after the
	// first was sent instead of waiting for its answer.
	repairWarmup = warmup{afterSend: 2 * time.Second, max: 15 * time.Second}
	// repairAnthropicWarmup: repair on an Anthropic provider with a streamed
	// model (v3.2.x r2 N46; Deps.repairWarmupFor). Anthropic makes a cache
	// entry readable only once the response of the request that writes it
	// has begun; with Claude's time to the first event on a repair prompt,
	// 2 s after sending was too early: in acceptance T5 the first three of
	// six concurrent chunks each wrote the cache. The other chunks follow
	// when chunk 0's streamed answer begins (message_start). Models that are
	// not streamed have no such moment and keep repairWarmup.
	repairAnthropicWarmup = warmup{afterFirstEvent: true, max: 15 * time.Second}
	// translateBatchDeepSeekWarmup: batch translation on DeepSeek (v3.2.x P2,
	// acceptance T2, review R20). What this rests on, by source:
	//
	// DeepSeek's context caching guide says: a request stores prefix units at
	// the end of its input and of its output, and at fixed intervals of long
	// inputs (the interval is not given); when it detects a common prefix
	// between requests it stores that prefix as a unit of its own (its A+B,
	// A+C, A+D example: the second request misses, the third hits A); a later
	// request hits only a unit it matches completely; caching is best-effort
	// and building a unit "takes seconds".
	//
	// Inferred from the call log of 2026-11-11 (not documented): every
	// non-zero hit is a multiple of 128 tokens, so units seem to be whole
	// 128-token blocks, the end-of-input unit rounded down to one. The
	// batches of a run share the system prompt (and the [BEFORE] label)
	// only; the first batch's input runs hundreds of tokens past that, so
	// its end-of-input unit matches no other batch, which fits the 42
	// batches sent after it all hitting 0 tokens. (The single path hits,
	// on that reading, because sentence 0's input happened to end in the
	// 128-token block where the shared prefix ends.)
	//
	// So one more batch goes alone and the other batches start when it has
	// finished: it makes the shared prefix a detected common prefix in time
	// for the batches sent right after it. Verified by a real run on a cold
	// cache (acceptance T4, 2026-11-11): the first batch hit 0 tokens, the
	// second 2688, and from the third on all 40 batches, sent in one wave,
	// hit (4608 tokens each; 90.8 % of their input). On Claude (acceptance
	// T5; translateWarmup, no second step) only the first translate request
	// wrote the cache and every later one read it.
	//
	// Since v3.2.x r2 T-PERF-04 a warm-up batch ends its step when its batch
	// request was answered (warmed), not when its QA retranslations are done
	// too; in T4 that was 2.75 s later for the second batch. What T4's call
	// log says about when the unit exists, on the single-sentence requests (a
	// prefix of their own, the single path's system prompt): the first hit 0
	// tokens, the second, sent at 16.10 s and answered at 17.80 s, hit 2688,
	// and the third, sent at 16.54 s while the second was in flight, already
	// hit the whole prefix (4480), like the ten after it. So the unit was
	// there 0.44 s after the second request was sent; the batches now wait
	// for the second batch's answer, 5.4 s after it was sent in T4. Not
	// documented by DeepSeek: a cold run has to confirm that the batches from
	// the third on still hit (T4: 86.9 % of the stage's input tokens).
	translateBatchDeepSeekWarmup = warmup{max: 15 * time.Second, second: 1}
)

// warmMinItems: runs with fewer items do not warm up (one serial request to
// save at most one cache miss is not worth it).
const warmMinItems = 3

// secondMinRest: the second step runs only when at least this many items
// are left to start after it (it holds them back one request's time).
const secondMinRest = 2

// warmupOff switches the warm-up off (tests: the behaviour before perf-llm,
// for the comparison in perf_llm_test.go).
var warmupOff = false

// secondStep is the number of second-step items a run of n items gets: 0
// when w has none or fewer than secondMinRest items would be left after it.
func (w warmup) secondStep(n int) int {
	if w.second < 0 || n-1-w.second < secondMinRest {
		return 0
	}
	return w.second
}

// warmSteps is the number of warm-up steps parallelWarm takes for a run of
// n items with workers in flight: 0 (no warm-up), 1, or 2 with a second
// step (the translate: batch plan log line).
func warmSteps(n, workers int, w warmup) int {
	switch {
	case n < warmMinItems || workers < 2 || warmupOff:
		return 0
	case w.secondStep(n) > 0:
		return 2
	}
	return 1
}

// parallelWarm is parallel with a warm-up: item 0 runs first; the other
// items wait until it finishes, or w says otherwise. With w.second, items
// 1..w.second run next, and the others wait until those finished too (or
// w.max after they started). A warm-up item that calls warmed ends its part
// of its step then, without the rest of the item (batch translation: when
// its batch request was answered). At most workers items are in flight at
// any time, waiting ones included. A fatal error of item 0 or of a
// second-step item stops the run like in parallel: if it comes before its
// step ended, no item waiting for that step runs.
func parallelWarm(ctx context.Context, n, workers int, w warmup, fn func(ctx context.Context, i int) (stop bool, err error)) error {
	if warmSteps(n, workers, w) == 0 {
		return parallel(ctx, n, workers, fn)
	}
	second := w.secondStep(n)
	first := newWarmGate()
	defer first.stop()
	// rest is the gate of the items after the warm-up: first without a
	// second step.
	rest := first
	var (
		mu      sync.Mutex
		pending = second // second-step items not finished yet
		capped  bool     // the second step's cap is running
	)
	if second > 0 {
		rest = newWarmGate()
		defer rest.stop()
	}
	return parallel(ctx, n, workers, func(ctx context.Context, i int) (bool, error) {
		switch {
		case i == 0:
			if w.max > 0 {
				first.after(w.max)
			}
			if w.afterSend > 0 {
				var sent sync.Once
				ctx = llm.WithSentHook(ctx, func() { sent.Do(func() { first.after(w.afterSend) }) })
			}
			if w.afterFirstEvent {
				// settle is idempotent: a later attempt's event, or one after
				// the gate settled (cap, end of item 0), changes nothing.
				ctx = llm.WithFirstEventHook(ctx, func() { first.settle(true) })
			}
			ctx = context.WithValue(ctx, warmedKey{}, func() { first.settle(true) })
			stop, err := fn(ctx, 0)
			first.settle(!stop) // before parallel sees stop and cancels ctx
			return stop, err
		case i <= second:
			if err := first.wait(ctx); err != nil {
				return true, err
			}
			mu.Lock()
			if !capped && w.max > 0 {
				capped = true
				rest.after(w.max)
			}
			mu.Unlock()
			// ended: this item's part of the second step is over, when it
			// says so (warmed) or when it finishes, whichever comes first.
			var once sync.Once
			ended := func() {
				once.Do(func() {
					mu.Lock()
					pending--
					done := pending == 0
					mu.Unlock()
					if done {
						rest.settle(true)
					}
				})
			}
			stop, err := fn(context.WithValue(ctx, warmedKey{}, ended), i)
			if stop {
				rest.settle(false) // before parallel sees stop and cancels ctx
				return stop, err
			}
			ended()
			return stop, err
		}
		if err := rest.wait(ctx); err != nil {
			return true, err
		}
		return fn(ctx, i)
	})
}

// warmedKey is the context key of the hook parallelWarm gives its warm-up
// items (warmed).
type warmedKey struct{}

// warmed says, from inside an item of parallelWarm, that the request that
// warms the cache has been answered (v3.2.x r2 T-PERF-04): a warm-up item's
// part of its step ends now instead of when the item finishes. What a
// provider caches is settled once it has answered that request; the QA
// retranslations and the writes that follow in the item add nothing to it,
// and in acceptance T4 they held 40 batches back 2.6 s. In any other item,
// and when the step ended before (its cap, a hook of w), warmed does
// nothing. An item that never calls it ends its step as before, when it
// finishes: that is what a warm-up item without a usable answer does.
func warmed(ctx context.Context) {
	if f, ok := ctx.Value(warmedKey{}).(func()); ok {
		f()
	}
}

// warmGate is one step of a warm-up: the items behind it wait until it
// opens. It settles once. settle(true) opens it; settle(false) (the item
// before it stopped the run) keeps it shut for good, including against a
// timer firing later, and the waiting items leave through ctx, which
// parallel cancels as soon as that item has returned. Opening the gate on a
// fatal error would let the waiting items race that cancel and run after it
// (perf-llm, NOTES.md).
type warmGate struct {
	ch      chan struct{}
	mu      sync.Mutex
	settled bool
	timers  []*time.Timer
}

func newWarmGate() *warmGate { return &warmGate{ch: make(chan struct{})} }

func (g *warmGate) settle(release bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.settled {
		return
	}
	g.settled = true
	if release {
		close(g.ch)
	}
}

// after opens the gate in d, unless it settled before.
func (g *warmGate) after(d time.Duration) {
	g.mu.Lock()
	g.timers = append(g.timers, time.AfterFunc(d, func() { g.settle(true) }))
	g.mu.Unlock()
}

func (g *warmGate) stop() {
	g.mu.Lock()
	for _, t := range g.timers {
		t.Stop()
	}
	g.mu.Unlock()
}

// wait returns nil once the gate opened, or ctx.Err() when the run was
// stopped or cancelled. Both may be ready (select picks at random): a
// stopped or cancelled run starts nothing new, as in parallel.
func (g *warmGate) wait(ctx context.Context) error {
	select {
	case <-g.ch:
	case <-ctx.Done():
	}
	return ctx.Err()
}

// lateWarm is a warm-up for requests that turn up during a run, no one
// knows when or how many: the single-sentence retranslations of a batch run
// (translate_batch.go). They share a prefix of their own, the single path's
// system prompt, which no batch request warms. The first of them goes alone
// and is a step, the next one likewise while steps are left; the others
// wait until the last step's request was answered and then go together.
// The steps are the run's warm-up steps (warmSteps: 0 none, 1, 2 on
// DeepSeek). A step holds the others back at most max.
//
// All fields but max are guarded by mu.
type lateWarm struct {
	max time.Duration

	mu    sync.Mutex
	steps int           // steps not ended yet; 0: nothing waits any more
	busy  bool          // a step is running
	shut  bool          // a step failed for good: nothing passes (as warmGate.settle(false))
	turn  chan struct{} // closed, and replaced, whenever steps or busy changed
	timer *time.Timer   // the cap of the running step
}

func newLateWarm(steps int, max time.Duration) *lateWarm {
	return &lateWarm{max: max, steps: steps, turn: make(chan struct{})}
}

// enter is called before a request. lead == true: the request is a warm-up
// step; it goes now and the caller calls leave when it was answered. lead ==
// false with a nil error: the warm-up is over, the request goes with the
// others. While another request's step runs, enter waits; when the run was
// stopped or cancelled it returns ctx.Err(), and the request must not be
// sent.
func (l *lateWarm) enter(ctx context.Context) (lead bool, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		l.mu.Lock()
		switch {
		case l.shut:
		case l.steps == 0:
			l.mu.Unlock()
			return false, nil
		case !l.busy:
			l.busy = true
			if l.max > 0 {
				l.timer = time.AfterFunc(l.max, l.open)
			}
			l.mu.Unlock()
			return true, nil
		}
		turn := l.turn
		l.mu.Unlock()
		select {
		case <-turn:
		case <-ctx.Done():
		}
	}
}

// leave ends the step of the caller (enter returned lead == true). ok ==
// false: its request failed for good or was cancelled, so the run is
// stopping; nothing passes any more and the waiting requests leave through
// ctx, as at a warmGate that settled shut.
func (l *lateWarm) leave(ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if !ok {
		l.shut = true
		return
	}
	l.busy = false
	if l.steps > 0 {
		l.steps--
	}
	l.wake()
}

// open ends the warm-up at the cap of a step.
func (l *lateWarm) open() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shut || l.steps == 0 {
		return
	}
	l.steps = 0
	l.wake()
}

// wake lets the waiting requests look again; l.mu is held.
func (l *lateWarm) wake() {
	close(l.turn)
	l.turn = make(chan struct{})
}

// stop stops the cap's timer when the run ends.
func (l *lateWarm) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
}
