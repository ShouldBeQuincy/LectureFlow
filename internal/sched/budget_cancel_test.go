// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// failingUsageStore makes the budget lookup fail with a real (non-context)
// database error.
type failingUsageStore struct {
	db.Store
}

var errUsageDB = errors.New("usage_daily: disk I/O error")

func (failingUsageStore) ListUsageDaily(context.Context, string) ([]model.UsageDaily, error) {
	return nil, errUsageDB
}

func budgetEnv(t *testing.T, budget float64) *env {
	t.Helper()
	e := newEnv(t, func(c *config.Config) {
		c.LLM.Prices = map[string]map[string]config.Price{"deepseek": {"deepseek-chat": {Input: 1, CacheHit: 0.1, Output: 2}}}
		c.LLM.DailyBudget = budget
	})
	e.s.Stop()
	return e
}

func inflight(p *Pool, provider string) int {
	return p.Status()[provider].Inflight
}

// A cancelled request never gets a slot, whether or not the daily budget is
// exhausted: the budget lookup fails with the context's error (it must not
// read as "not exceeded"), and a slot granted at the same moment as the
// cancellation is handed back.
func TestAcquireCancelledNeverGranted(t *testing.T) {
	e := budgetEnv(t, 1.0)
	// No cache: every check hits the database with the cancelled ctx.
	var clock atomic.Value
	clock.Store(time.Now())
	pool := NewPool(e.cfg, e.st, PoolOptions{Now: func() time.Time { return clock.Load().(time.Time) }, BudgetPoll: time.Hour, BudgetCacheTTL: -1})

	cancelled, cancel := context.WithCancel(WithJob(e.ctx, model.Job{ID: "J1"}))
	cancel()

	check := func(label string) {
		t.Helper()
		for i := 0; i < 500; i++ {
			rel, err := pool.Acquire(cancelled, "deepseek")
			if !errors.Is(err, context.Canceled) {
				if rel != nil {
					rel()
				}
				t.Fatalf("%s: attempt %d: acquire = %v, want context.Canceled", label, i, err)
			}
			if rel != nil {
				t.Fatalf("%s: attempt %d: release func returned with an error", label, i)
			}
			if n := inflight(pool, "deepseek"); n != 0 {
				t.Fatalf("%s: attempt %d: inflight = %d after a cancelled acquire", label, i, n)
			}
		}
		if pool.BudgetBlocked("J1") {
			t.Fatalf("%s: job still reported as budget-blocked", label)
		}
	}

	// Budget not exceeded.
	if pool.BudgetExceeded(e.ctx) {
		t.Fatal("empty day: budget exceeded")
	}
	check("under budget")

	// Budget exceeded (1.04 >= 1.0).
	pool.RecordUsage(e.ctx, "deepseek", 1_000_000, 400_000, 200_000)
	if !pool.BudgetExceeded(e.ctx) {
		t.Fatal("budget should be exceeded")
	}
	check("over budget")

	// The pool still works for a live context afterwards (under budget on
	// the next day).
	clock.Store(time.Now().Add(48 * time.Hour))
	rel, err := pool.Acquire(e.ctx, "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if n := inflight(pool, "deepseek"); n != 1 {
		t.Fatalf("inflight = %d, want 1", n)
	}
	rel()
	if n := inflight(pool, "deepseek"); n != 0 {
		t.Fatalf("inflight after release = %d", n)
	}
}

// The same with a cached budget lookup (the default): the cache answers
// "exceeded" without touching the database, and the cancelled ctx must
// still win.
func TestAcquireCancelledCachedBudget(t *testing.T) {
	e := budgetEnv(t, 1.0)
	pool := NewPool(e.cfg, e.st, PoolOptions{BudgetPoll: time.Hour, BudgetCacheTTL: time.Hour})
	pool.RecordUsage(e.ctx, "deepseek", 1_000_000, 400_000, 200_000)
	if !pool.BudgetExceeded(e.ctx) {
		t.Fatal("budget should be exceeded")
	}
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	for i := 0; i < 500; i++ {
		if _, err := pool.Acquire(ctx, "deepseek"); !errors.Is(err, context.Canceled) {
			t.Fatalf("attempt %d: acquire = %v", i, err)
		}
	}
	if n := inflight(pool, "deepseek"); n != 0 {
		t.Fatalf("inflight = %d", n)
	}
}

// A real database error while checking the budget keeps the documented
// behaviour (BudgetExceeded: "a database error is logged and treated as not
// exceeded"): the request is not held, and BudgetExceeded reports false.
func TestAcquireBudgetDBErrorFailsOpen(t *testing.T) {
	e := budgetEnv(t, 1.0)
	pool := NewPool(e.cfg, failingUsageStore{e.st}, PoolOptions{BudgetPoll: time.Hour, BudgetCacheTTL: -1})
	if pool.BudgetExceeded(e.ctx) {
		t.Fatal("db error: BudgetExceeded = true")
	}
	if _, _, err := pool.spentToday(e.ctx); !errors.Is(err, errUsageDB) {
		t.Fatalf("spentToday err = %v", err)
	}
	rel, err := pool.Acquire(WithJob(e.ctx, model.Job{ID: "J2"}), "deepseek")
	if err != nil {
		t.Fatalf("acquire with a db error: %v", err)
	}
	rel()
	if pool.BudgetBlocked("J2") {
		t.Fatal("job reported as budget-blocked")
	}
	// Cancelled ctx still loses, even when the database error is real.
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	for i := 0; i < 200; i++ {
		if _, err := pool.Acquire(ctx, "deepseek"); !errors.Is(err, context.Canceled) {
			t.Fatalf("attempt %d: acquire = %v", i, err)
		}
	}
	if n := inflight(pool, "deepseek"); n != 0 {
		t.Fatalf("inflight = %d", n)
	}
}
