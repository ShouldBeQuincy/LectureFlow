// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
)

// span is when an item ran.
type span struct{ start, end time.Time }

type spans struct {
	mu       sync.Mutex
	s        map[int]span
	inflight int
	peak     int
}

func (r *spans) run(i int, body func()) {
	r.mu.Lock()
	if r.s == nil {
		r.s = map[int]span{}
	}
	r.inflight++
	r.peak = max(r.peak, r.inflight)
	r.s[i] = span{start: time.Now()}
	r.mu.Unlock()
	body()
	r.mu.Lock()
	sp := r.s[i]
	sp.end = time.Now()
	r.s[i] = sp
	r.inflight--
	r.mu.Unlock()
}

// TestParallelWarmWaitsForTheFirstItem (perf-llm): no other item starts
// before item 0 finished; afterwards up to workers run at once.
func TestParallelWarmWaitsForTheFirstItem(t *testing.T) {
	var r spans
	err := parallelWarm(context.Background(), 12, 4, warmup{max: 10 * time.Second}, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {
			if i == 0 {
				time.Sleep(40 * time.Millisecond)
			} else {
				time.Sleep(10 * time.Millisecond)
			}
		})
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.s) != 12 {
		t.Fatalf("%d items ran", len(r.s))
	}
	for i := 1; i < 12; i++ {
		if r.s[i].start.Before(r.s[0].end) {
			t.Errorf("item %d started before item 0 finished", i)
		}
	}
	if r.peak < 2 || r.peak > 4 {
		t.Errorf("peak %d, want 2..4", r.peak)
	}
}

// Fewer than warmMinItems items, or one worker: plain parallel.
func TestParallelWarmSmallRuns(t *testing.T) {
	var r spans
	both := make(chan struct{})
	var once sync.Once
	n := 0
	var mu sync.Mutex
	err := parallelWarm(context.Background(), 2, 4, translateWarmup, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {
			mu.Lock()
			n++
			if n == 2 {
				once.Do(func() { close(both) })
			}
			mu.Unlock()
			select { // both are in flight together, or this times out
			case <-both:
			case <-time.After(2 * time.Second):
			}
		})
		return false, nil
	})
	if err != nil || r.peak != 2 {
		t.Fatalf("two items: peak %d, err %v", r.peak, err)
	}
}

// The cap: a slow first item holds the others back at most w.max.
func TestParallelWarmCap(t *testing.T) {
	var r spans
	err := parallelWarm(context.Background(), 5, 5, warmup{max: 30 * time.Millisecond}, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {
			if i == 0 {
				time.Sleep(400 * time.Millisecond)
			}
		})
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 5; i++ {
		if !r.s[i].start.Before(r.s[0].end) {
			t.Errorf("item %d waited for the slow first item past the cap", i)
		}
		if d := r.s[i].start.Sub(r.s[0].start); d < 25*time.Millisecond {
			t.Errorf("item %d started %v after item 0, before the cap", i, d)
		}
	}
}

// afterSend: the others start that long after item 0's request was sent,
// without waiting for its answer (repair, whose chunks take seconds).
func TestParallelWarmAfterSend(t *testing.T) {
	fake := llmtest.New(t)
	fake.SetDelay(400 * time.Millisecond)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	fake.Configure(cfg)
	c := llm.New(cfg, nil, "", nil) // no pool: item 0 is sent as soon as it starts
	var r spans
	err := parallelWarm(context.Background(), 4, 4, warmup{afterSend: 50 * time.Millisecond, max: 10 * time.Second}, func(ctx context.Context, i int) (bool, error) {
		var err error
		r.run(i, func() {
			if i == 0 {
				_, err = c.Complete(ctx, config.RoleSummary, ctxbuild.SummarySystemTemplate, "u", false)
			}
		})
		return err != nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 4; i++ {
		if !r.s[i].start.Before(r.s[0].end) {
			t.Errorf("item %d waited for item 0's answer", i)
		}
		if d := r.s[i].start.Sub(r.s[0].start); d < 45*time.Millisecond {
			t.Errorf("item %d started %v after item 0, want the 50ms head start", i, d)
		}
	}
}

// A fatal error of item 0 stops the run: no other item runs.
func TestParallelWarmFirstItemFails(t *testing.T) {
	boom := errors.New("boom")
	var r spans
	err := parallelWarm(context.Background(), 10, 4, translateWarmup, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {})
		if i == 0 {
			return true, boom
		}
		return false, nil
	})
	if !errors.Is(err, boom) || len(r.s) != 1 {
		t.Fatalf("err %v, %d items ran", err, len(r.s))
	}
	// cancelled while the others wait: they give up at once
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	err = parallelWarm(ctx, 10, 4, translateWarmup, func(ctx context.Context, i int) (bool, error) {
		if i == 0 {
			cancel()
			<-ctx.Done()
			return true, ctx.Err()
		}
		return false, nil
	})
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancel: %v after %v", err, time.Since(start))
	}
}

// The waiting items are parked at the gate when item 0 fails: none of them
// runs. (If item 0 opened the gate on its way out, ahead of parallel's
// cancel, parked waiters would go through it.) The sleep only gives the
// waiters time to park; the assertion holds whatever the timing.
func TestParallelWarmFirstItemFailsWhileOthersWait(t *testing.T) {
	boom := errors.New("boom")
	for round := range 20 {
		var r spans
		err := parallelWarm(context.Background(), 40, 16, translateWarmup, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				if i == 0 {
					time.Sleep(5 * time.Millisecond)
				}
			})
			if i == 0 {
				return true, boom
			}
			return false, nil
		})
		if !errors.Is(err, boom) || len(r.s) != 1 {
			t.Fatalf("round %d: err %v, %d items ran", round, err, len(r.s))
		}
	}
}

// The second step (batch translation on DeepSeek): item 1 starts when item 0
// finished, the others when item 1 finished; then up to workers at once.
func TestParallelWarmSecondStep(t *testing.T) {
	var r spans
	err := parallelWarm(context.Background(), 12, 4, translateBatchDeepSeekWarmup, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {
			if i <= 1 {
				time.Sleep(30 * time.Millisecond)
			} else {
				time.Sleep(10 * time.Millisecond)
			}
		})
		return false, nil
	})
	if err != nil || len(r.s) != 12 {
		t.Fatalf("err %v, %d items ran", err, len(r.s))
	}
	if r.s[1].start.Before(r.s[0].end) {
		t.Error("item 1 started before item 0 finished")
	}
	for i := 2; i < 12; i++ {
		if r.s[i].start.Before(r.s[1].end) {
			t.Errorf("item %d started before item 1 finished", i)
		}
	}
	if r.peak < 2 || r.peak > 4 {
		t.Errorf("peak %d, want 2..4", r.peak)
	}
}

// Fewer than secondMinRest items left after it: no second step.
func TestParallelWarmSecondStepSmallRun(t *testing.T) {
	both := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	n, overlap := 0, false
	err := parallelWarm(context.Background(), 3, 4, translateBatchDeepSeekWarmup, func(ctx context.Context, i int) (bool, error) {
		if i == 0 {
			return false, nil
		}
		mu.Lock()
		n++
		if n == 2 {
			once.Do(func() { close(both) })
		}
		mu.Unlock()
		select {
		case <-both:
			mu.Lock()
			overlap = true
			mu.Unlock()
		case <-time.After(2 * time.Second):
		}
		return false, nil
	})
	if err != nil || !overlap {
		t.Fatalf("err %v; items 1 and 2 in flight together: %v", err, overlap)
	}
}

// The cap of the second step: a slow item 1 holds the others back at most
// w.max after it started.
func TestParallelWarmSecondStepCap(t *testing.T) {
	var r spans
	err := parallelWarm(context.Background(), 6, 6, warmup{max: 30 * time.Millisecond, second: 1}, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {
			if i == 1 {
				time.Sleep(400 * time.Millisecond)
			}
		})
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 6; i++ {
		if !r.s[i].start.Before(r.s[1].end) {
			t.Errorf("item %d waited for the slow item 1 past the cap", i)
		}
		if d := r.s[i].start.Sub(r.s[1].start); d < 25*time.Millisecond {
			t.Errorf("item %d started %v after item 1, before the cap", i, d)
		}
	}
}

// The first step opened by its cap (a slow item 0): item 1 starts then,
// without waiting for item 0, and the second step's cap counts from when
// item 1 started; the others do not wait for item 0 either.
func TestParallelWarmSecondStepAfterFirstCap(t *testing.T) {
	var r spans
	err := parallelWarm(context.Background(), 6, 6, warmup{max: 30 * time.Millisecond, second: 1}, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {
			switch i {
			case 0:
				time.Sleep(400 * time.Millisecond)
			case 1:
				time.Sleep(200 * time.Millisecond)
			}
		})
		return false, nil
	})
	if err != nil || len(r.s) != 6 {
		t.Fatalf("err %v, %d items ran", err, len(r.s))
	}
	if !r.s[1].start.Before(r.s[0].end) {
		t.Error("item 1 waited for the slow item 0 past the cap")
	}
	if d := r.s[1].start.Sub(r.s[0].start); d < 25*time.Millisecond {
		t.Errorf("item 1 started %v after item 0, before the cap", d)
	}
	for i := 2; i < 6; i++ {
		if !r.s[i].start.Before(r.s[1].end) || !r.s[i].start.Before(r.s[0].end) {
			t.Errorf("item %d waited for a slow warm-up item past the cap", i)
		}
		if d := r.s[i].start.Sub(r.s[1].start); d < 25*time.Millisecond {
			t.Errorf("item %d started %v after item 1, before the second cap", i, d)
		}
	}
}

// Cancelling the run while item 1 (the second step) is in flight: no item
// after it runs, whether item 1 then fails with the cancel or finishes. (When
// it finishes, the run reports the cancel only if an item was waiting at the
// gate when it came; as in parallel, the workers start nothing after it.)
func TestParallelWarmSecondStepCancelled(t *testing.T) {
	for _, fails := range []bool{true, false} {
		for round := range 10 {
			ctx, cancel := context.WithCancel(context.Background())
			var r spans
			err := parallelWarm(ctx, 10, 4, translateBatchDeepSeekWarmup, func(ctx context.Context, i int) (bool, error) {
				var stop bool
				var err error
				r.run(i, func() {
					if i != 1 {
						return
					}
					cancel()
					<-ctx.Done()
					if fails {
						stop, err = true, ctx.Err()
					}
				})
				return stop, err
			})
			cancel()
			if fails && !errors.Is(err, context.Canceled) || err != nil && !errors.Is(err, context.Canceled) || len(r.s) != 2 {
				t.Fatalf("item 1 fails %v, round %d: err %v, %d items ran", fails, round, err, len(r.s))
			}
		}
	}
}

// warmSteps is what parallelWarm does (the warmup_steps of the batch plan
// log line): no warm-up for small runs or one worker, no second step when
// too few items would be left after it.
func TestWarmSteps(t *testing.T) {
	for _, c := range []struct {
		n, workers int
		w          warmup
		want       int
	}{
		{2, 4, translateBatchDeepSeekWarmup, 0},
		{10, 1, translateBatchDeepSeekWarmup, 0},
		{3, 4, translateBatchDeepSeekWarmup, 1},
		{4, 4, translateBatchDeepSeekWarmup, 2},
		{43, 8, translateBatchDeepSeekWarmup, 2},
		{43, 8, translateWarmup, 1},
		{2, 4, translateWarmup, 0},
	} {
		if got := warmSteps(c.n, c.workers, c.w); got != c.want {
			t.Errorf("warmSteps(%d, %d, second %d) = %d, want %d", c.n, c.workers, c.w.second, got, c.want)
		}
	}
}

// A fatal error of item 1 stops the run: no item after it runs; a
// non-fatal one releases them.
func TestParallelWarmSecondStepItemFails(t *testing.T) {
	boom := errors.New("boom")
	for round := range 20 {
		var r spans
		err := parallelWarm(context.Background(), 40, 16, translateBatchDeepSeekWarmup, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				if i == 1 {
					time.Sleep(5 * time.Millisecond)
				}
			})
			if i == 1 {
				return true, boom
			}
			return false, nil
		})
		if !errors.Is(err, boom) || len(r.s) != 2 {
			t.Fatalf("round %d: err %v, %d items ran", round, err, len(r.s))
		}
	}
	var r spans
	err := parallelWarm(context.Background(), 10, 4, translateBatchDeepSeekWarmup, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {})
		if i == 1 {
			return false, errors.New("unusable")
		}
		return false, nil
	})
	if err != nil || len(r.s) != 10 {
		t.Fatalf("non-fatal: err %v, %d items ran", err, len(r.s))
	}
}

// A non-fatal result of item 0 (stop == false, e.g. an unusable answer
// recorded as a failed sentence) still opens the gate: every item runs.
func TestParallelWarmFirstItemNonFatalErrorReleases(t *testing.T) {
	var r spans
	err := parallelWarm(context.Background(), 10, 4, translateWarmup, func(ctx context.Context, i int) (bool, error) {
		r.run(i, func() {})
		if i == 0 {
			return false, errors.New("unusable")
		}
		return false, nil
	})
	if err != nil || len(r.s) != 10 {
		t.Fatalf("err %v, %d items ran", err, len(r.s))
	}
}
