// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// v3.2.x r2 T-PERF-04: a warm-up item ends its step when it says its request
// was answered (warmed), and the warm-up of requests that turn up during a
// run (lateWarm). On the fake clock of a synctest bubble.

// since returns when the items started, from begin.
func (r *spans) since(begin time.Time, n int) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]time.Duration, n)
	for i := range out {
		sp, ok := r.s[i]
		if !ok {
			out[i] = -1
			continue
		}
		out[i] = sp.start.Sub(begin)
	}
	return out
}

// Item 0 says warmed at 3 s and runs on until 10 s: the others start at
// 3 s. warmed in an item that is not a warm-up item does nothing.
func TestParallelWarmWarmed(t *testing.T) {
	const s = time.Second
	synctest.Test(t, func(t *testing.T) {
		var r spans
		begin := time.Now()
		err := parallelWarm(context.Background(), 6, 6, translateWarmup, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				if i == 0 {
					time.Sleep(3 * s)
					warmed(ctx)
					time.Sleep(7 * s)
					warmed(ctx) // again: nothing
					return
				}
				warmed(ctx) // not a warm-up item: nothing
				time.Sleep(s)
			})
			return false, nil
		})
		if err != nil || time.Since(begin) != 10*s {
			t.Fatalf("err %v after %v", err, time.Since(begin))
		}
		if got := r.since(begin, 6); !slices.Equal(got, []time.Duration{0, 3 * s, 3 * s, 3 * s, 3 * s, 3 * s}) {
			t.Errorf("items started at %v, want the others at 3s", got)
		}
	})
	// warmed outside parallelWarm does nothing
	warmed(context.Background())
}

// Two steps: item 1 starts when item 0 said warmed (3 s), the others when
// item 1 did (7 s), though items 0 and 1 run on. With two second-step items
// the others wait for both, and an item that says warmed twice counts once.
func TestParallelWarmWarmedSecondStep(t *testing.T) {
	const s = time.Second
	synctest.Test(t, func(t *testing.T) {
		var r spans
		begin := time.Now()
		err := parallelWarm(context.Background(), 6, 6, translateBatchDeepSeekWarmup, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				switch i {
				case 0:
					time.Sleep(3 * s)
					warmed(ctx)
					time.Sleep(7 * s)
				case 1:
					time.Sleep(4 * s)
					warmed(ctx)
					time.Sleep(13 * s)
				}
			})
			return false, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.since(begin, 6); !slices.Equal(got, []time.Duration{0, 3 * s, 7 * s, 7 * s, 7 * s, 7 * s}) {
			t.Errorf("items started at %v, want item 1 at 3s and the others at 7s", got)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		var r spans
		begin := time.Now()
		w := warmup{max: 15 * s, second: 2}
		err := parallelWarm(context.Background(), 6, 6, w, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				switch i {
				case 0:
					time.Sleep(s)
				case 1:
					time.Sleep(s)
					warmed(ctx)
					warmed(ctx) // counts once: item 2 is not done
					time.Sleep(10 * s)
				case 2:
					time.Sleep(5 * s) // never says warmed: its part ends when it finishes
				}
			})
			return false, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.since(begin, 6); !slices.Equal(got, []time.Duration{0, s, s, 6 * s, 6 * s, 6 * s}) {
			t.Errorf("items started at %v, want items 1, 2 at 1s and the others at 6s", got)
		}
	})
}

// A warm-up item that fails for good after it said warmed stops the run
// like any item: the items it let through are cancelled and no other item
// runs. The same for a second-step item. An item that fails before it said
// warmed lets nothing through (TestParallelWarmFirstItemFails).
func TestParallelWarmWarmedThenFails(t *testing.T) {
	const s = time.Second
	boom := errors.New("boom")
	for _, c := range []struct {
		name string
		w    warmup
		item int // the warm-up item that fails
		ran  int // items that ran
	}{
		{"item 0", translateWarmup, 0, 4},
		// item 0 is done at once and its worker waits with item 4
		{"second-step item", translateBatchDeepSeekWarmup, 1, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var r spans
				begin := time.Now()
				err := parallelWarm(context.Background(), 10, 4, c.w, func(ctx context.Context, i int) (bool, error) {
					var err error
					r.run(i, func() {
						switch {
						case i < c.item: // the step before
						case i == c.item:
							time.Sleep(3 * s)
							warmed(ctx)
							time.Sleep(2 * s)
							err = boom
						default: // let through at 3 s; would take until 13 s
							select {
							case <-time.After(10 * s):
							case <-ctx.Done():
								err = ctx.Err()
							}
						}
					})
					return err != nil, err
				})
				if !errors.Is(err, boom) || time.Since(begin) != 5*s {
					t.Fatalf("err %v after %v, want boom at 5s", err, time.Since(begin))
				}
				r.mu.Lock()
				defer r.mu.Unlock()
				if len(r.s) != c.ran {
					t.Errorf("%d items ran, want %d", len(r.s), c.ran)
				}
				for i, sp := range r.s {
					if i > c.item && (sp.start.Sub(begin) != 3*s || sp.end.Sub(begin) != 5*s) {
						t.Errorf("item %d ran %v–%v, want 3s–5s", i, sp.start.Sub(begin), sp.end.Sub(begin))
					}
				}
			})
		})
	}
}

// lateEntry is one request at a lateWarm.
type lateEntry struct {
	at   time.Duration
	lead bool
	err  error
}

// enterAll sends n requests to l at once; a lead holds its step for hold
// and leaves with ok.
func enterAll(ctx context.Context, l *lateWarm, n int, hold time.Duration, ok bool) []lateEntry {
	begin := time.Now()
	out := make([]lateEntry, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lead, err := l.enter(ctx)
			out[i] = lateEntry{at: time.Since(begin), lead: lead, err: err}
			if lead {
				time.Sleep(hold)
				l.leave(ok)
			}
		}()
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b lateEntry) int { return int(a.at - b.at) })
	return out
}

// The first steps go one at a time, the others when the last step ended.
func TestLateWarmSteps(t *testing.T) {
	const s = time.Second
	for steps := range 3 {
		synctest.Test(t, func(t *testing.T) {
			l := newLateWarm(steps, 15*s)
			defer l.stop()
			got := enterAll(context.Background(), l, 5, 2*s, true)
			for k, e := range got {
				// request k of the steps leads at 2k s; the others pass when
				// the steps are over
				want := lateEntry{at: time.Duration(steps) * 2 * s}
				if k < steps {
					want = lateEntry{at: time.Duration(k) * 2 * s, lead: true}
				}
				if e != want {
					t.Errorf("%d steps: request %d: %+v, want %+v", steps, k, e, want)
				}
			}
			// afterwards nothing waits or leads
			for _, e := range enterAll(context.Background(), l, 3, s, true) {
				if e != (lateEntry{}) {
					t.Errorf("%d steps: after the warm-up: %+v", steps, e)
				}
			}
		})
	}
}

// The cap: a step that takes longer than max holds the others back max, and
// its late end changes nothing.
func TestLateWarmCap(t *testing.T) {
	const s = time.Second
	synctest.Test(t, func(t *testing.T) {
		l := newLateWarm(2, 15*s)
		defer l.stop()
		got := enterAll(context.Background(), l, 4, time.Minute, true)
		want := []lateEntry{{at: 0, lead: true}, {at: 15 * s}, {at: 15 * s}, {at: 15 * s}}
		if !slices.Equal(got, want) {
			t.Errorf("entries %+v, want %+v", got, want)
		}
		if lead, err := l.enter(context.Background()); lead || err != nil {
			t.Errorf("after the cap and the late end: lead %v, err %v", lead, err)
		}
	})
}

// A step that failed for good shuts the warm-up: the waiting requests are
// not let through, not at the cap either; they leave when the run is
// cancelled (as parallel does right after the failure), with its error.
func TestLateWarmShut(t *testing.T) {
	const s = time.Second
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tm := time.AfterFunc(40*s, cancel) // long after the cap (15 s)
		defer tm.Stop()
		l := newLateWarm(2, 15*s)
		defer l.stop()
		got := enterAll(ctx, l, 4, 2*s, false)
		if got[0] != (lateEntry{at: 0, lead: true}) {
			t.Errorf("first request %+v", got[0])
		}
		for _, e := range got[1:] {
			if e.lead || e.at != 40*s || !errors.Is(e.err, context.Canceled) {
				t.Errorf("waiting request %+v, want the cancel at 40s", e)
			}
		}
	})
}

// A cancelled run: a waiting request leaves at the cancel, a new one is
// turned away and takes no step.
func TestLateWarmCancelled(t *testing.T) {
	const s = time.Second
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tm := time.AfterFunc(s, cancel)
		defer tm.Stop()
		l := newLateWarm(1, 15*s)
		defer l.stop()
		got := enterAll(ctx, l, 3, 5*s, true)
		for _, e := range got[1:] {
			if e.lead || e.at != s || !errors.Is(e.err, context.Canceled) {
				t.Errorf("waiting request %+v, want the cancel at 1s", e)
			}
		}
		fresh := newLateWarm(1, 15*s)
		defer fresh.stop()
		if lead, err := fresh.enter(ctx); lead || !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled before: lead %v, err %v", lead, err)
		}
		if lead, err := fresh.enter(context.Background()); !lead || err != nil {
			t.Errorf("the turned-away request took the step: lead %v, err %v", lead, err)
		}
		fresh.leave(true)
	})
}
