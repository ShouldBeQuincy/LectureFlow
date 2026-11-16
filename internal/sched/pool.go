// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// RoleOfStage maps an API stage to its [llm.roles] entry (SPEC §2).
// material_terms uses the glossary role. Non-API stages return "".
func RoleOfStage(stage string) string {
	switch stage {
	case model.StageGlossarySeed, model.StageMaterialTerms:
		return config.RoleGlossary
	case model.StageSummary:
		return config.RoleSummary
	case model.StageRepair:
		return config.RoleRepair
	case model.StageTranslate:
		return config.RoleTranslate
	}
	return ""
}

// EstimateCost returns the estimated cost of a token usage (SPEC §17.7)
// with the legacy config.toml prices. st.EstimatedCost wins when the stage
// recorded it; otherwise the price of the stage role's provider/model is
// applied (input to non-cached prompt tokens, cache_hit to cached ones,
// output to completion tokens; prices are per million tokens). ok is false
// when no price applies. v1.1 callers with a provider id use
// Pool.ProviderCost (the provider's own prices, A5.1).
func EstimateCost(cfg *config.Config, stage string, st model.JobStats) (cost float64, ok bool) {
	if st.EstimatedCost > 0 {
		return st.EstimatedCost, true
	}
	role, found := cfg.LLM.Roles[RoleOfStage(stage)]
	if !found {
		return 0, false
	}
	p, found := cfg.PriceFor(role.Provider, role.Model)
	if !found {
		return 0, false
	}
	miss := st.PromptTokens - st.CacheHitTokens
	if miss < 0 {
		miss = 0
	}
	return (float64(miss)*p.Input + float64(st.CacheHitTokens)*p.CacheHit + float64(st.CompletionTokens)*p.Output) / 1e6, true
}

// Usage is the token usage (and estimated cost) of today. EstimatedCost is
// nil without any price, and when every request of the day was unpriced
// (D3: only tokens are shown).
type Usage struct {
	PromptTokens     int64
	CacheHitTokens   int64
	CompletionTokens int64
	EstimatedCost    *float64
	Requests         int64 // responses recorded today
	Unpriced         int64 // of them without any price (settings.UnpricedRequests)
}

// APIStat is one provider's row in GET /api/system (SPEC §17.1) and the
// "当前并发" of the resource watcher (v1.1 A10.2).
type APIStat struct {
	Name     string // display name (the config key in legacy mode)
	Inflight int
	Max      int
	RPM      int
}

// PoolOptions tune the API pool; zero values give the defaults.
type PoolOptions struct {
	Now            func() time.Time // clock for the local day and the peak periods (default time.Now)
	BudgetPoll     time.Duration    // re-check interval while over budget (default 30s)
	BudgetCacheTTL time.Duration    // how long a read "spent today" is reused (default 2s; <0 = no cache)
	// OffpeakPoll is the longest single wait of a request held for the
	// off-peak period (v2 P12.6) before the peak state is checked again; it
	// guards against a clock that jumped (sleep, hibernation). Default 1 min.
	OffpeakPoll time.Duration
}

// Request classes of the API pool (perf-llm, NOTES.md): the few requests
// on a lecture's critical path (summary, repair, title, chapters, the
// course stages) go before the bulk per-sentence translation of other
// lectures, which would otherwise hold them behind hundreds of requests.
const (
	classUrgent = 0 // every API stage but translate
	classBulk   = 1 // translate: one request per sentence
)

// UrgentBurst is how many requests of the urgent class the pool grants in a
// row while bulk (translate) requests wait; the next grant then goes to the
// best bulk request, so translation keeps at least 1/(UrgentBurst+1) of the
// grants and is never starved.
const UrgentBurst = 3

// classOf returns the request class of an API stage.
func classOf(stage string) int {
	if stage == model.StageTranslate {
		return classBulk
	}
	return classUrgent
}

// prio orders waiting API requests: jobs before requests without a job,
// then the urgent class before the bulk class (perf-llm), then the FIFO of
// SPEC §5 (scope object creation time, then job creation time), arrival
// order as the final tie-breaker. pickLocked lets a bulk request through
// after UrgentBurst urgent grants in a row.
type prio struct {
	noJob        bool
	class        int
	scopeCreated string
	scopeID      string
	jobCreated   string
	jobID        string
	seq          uint64
}

func (a prio) less(b prio) bool {
	if a.noJob != b.noJob {
		return !a.noJob
	}
	if a.class != b.class {
		return a.class < b.class
	}
	if a.scopeCreated != b.scopeCreated {
		return a.scopeCreated < b.scopeCreated
	}
	if a.scopeID != b.scopeID {
		return a.scopeID < b.scopeID
	}
	if a.jobCreated != b.jobCreated {
		return a.jobCreated < b.jobCreated
	}
	if a.jobID != b.jobID {
		return a.jobID < b.jobID
	}
	return a.seq < b.seq
}

// Pool implements APIPool (SPEC §5): per provider max_concurrency slots and
// a token bucket of requests_per_minute (burst = one second's worth, at
// least 1; rpm 0 = no per-minute limit). A request is granted a slot and a
// token together; waiting requests are served in priority order (prio), so
// the oldest lecture's requests go first, and a lecture's summary / repair /
// title go before other lectures' translation (perf-llm). The daily budget
// (SPEC §17.7) is checked before queueing. Acquire never touches the
// database except for the budget check, which is cached.
//
// Adaptive concurrency (perf-llm): the llm client reports throttling (HTTP
// 429 / 503 / 529) and answers (Throttled, Succeeded). A throttle halves the
// provider's effective limit (from what was in flight, at least 1); every
// `limit` answers in a row raise it by one, back up to max_concurrency
// (AIMD). The retries of the throttled requests then queue in the pool
// instead of hammering the provider.
//
// Providers are keyed by provider id. NewPoolWithSettings follows the
// settings service: edits of max_concurrency/rpm apply immediately (A5.4)
// and a deleted provider's new requests fail at once. NewPool (legacy)
// takes them from config.toml, keyed by the config key.
//
// Usage: the llm client reports every response with RecordRequest
// (llm.CostRecorder; RecordUsage for other callers), which prices it by the
// period the request started in (v2 P12.5) and adds tokens and cost to
// usage_daily for the local day (A10.3). Today's spending for the budget =
// the sum of today's usage_daily.estimated_cost.
//
// v2 P12.6: a request of a job to a provider with offpeak_only waits in
// Acquire while its tariff is in a peak period (OffpeakWait, OffpeakBlocked).
type Pool struct {
	cfg *config.Config    // legacy mode
	set *settings.Service // settings mode (nil in legacy mode)
	st  db.Store
	o   PoolOptions

	pmu       sync.RWMutex
	providers map[string]*providerPool

	mu       sync.Mutex
	prios    map[string]prio // job id → priority, registered by the scheduler at job start
	spent    float64
	unpriced int64 // today's unpriced requests, cached with spent
	spentDay string
	spentAt  time.Time
	spentOK  bool
	blocked  map[string]int // job id → Acquire calls waiting on the budget
	offpeak  map[string]*offpeakHold
	// changed is closed (and replaced) on every settings change, waking the
	// requests held for the off-peak period to check again.
	changed chan struct{}
}

// offpeakHold: Acquire calls of one job waiting for the off-peak period.
type offpeakHold struct {
	n     int
	until time.Time
}

type waiter struct {
	p       prio
	ch      chan struct{}
	granted bool
	err     error // set (with ch closed) when the provider was deleted while waiting
}

type providerPool struct {
	mu       sync.Mutex
	name     string
	max      int
	rpm      int
	inflight int
	seq      uint64
	waiters  []*waiter
	rate     float64 // tokens per second (0 = unlimited)
	burst    float64
	tokens   float64
	last     time.Time
	timer    *time.Timer // re-dispatch when the next token is due
	removed  bool        // the provider was deleted (settings mode)

	// adaptive concurrency (perf-llm): limit is the effective number of
	// slots, 1..max; cutAt the last time it was lowered; okRun the answers
	// since the last change.
	limit int
	cutAt time.Time
	okRun int
	// urgentRun counts urgent grants in a row while bulk requests waited.
	urgentRun int
}

func newProviderPool(name string, max, rpm int) *providerPool {
	pp := &providerPool{name: name}
	pp.mu.Lock()
	pp.setLimitsLocked(max, rpm)
	pp.mu.Unlock()
	return pp
}

// setLimitsLocked applies max_concurrency and requests_per_minute. Requests
// in flight are not affected; a larger limit lets waiters through at once.
// A changed max_concurrency also resets the adaptive limit to it.
func (pp *providerPool) setLimitsLocked(max, rpm int) {
	if max < 1 {
		max = 1
	}
	if max != pp.max {
		pp.limit, pp.okRun = max, 0
	}
	pp.max = max
	if rpm == pp.rpm && (pp.rate > 0) == (rpm > 0) {
		return
	}
	now := time.Now()
	if pp.rate > 0 { // settle the tokens earned at the old rate
		pp.tokens = min(pp.tokens+now.Sub(pp.last).Seconds()*pp.rate, pp.burst)
	}
	pp.rpm = rpm
	if rpm <= 0 {
		pp.rate, pp.burst, pp.tokens = 0, 0, 0
	} else {
		first := pp.rate == 0
		pp.rate = float64(rpm) / 60
		pp.burst = max1(pp.rate)
		if first {
			pp.tokens = pp.burst
		}
		pp.tokens = min(pp.tokens, pp.burst)
	}
	pp.last = now
	if pp.timer != nil { // computed with the old rate
		pp.timer.Stop()
		pp.timer = nil
	}
}

func max1(v float64) float64 {
	if v < 1 {
		return 1
	}
	return v
}

func newPool(st db.Store, o PoolOptions) *Pool {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.BudgetPoll <= 0 {
		o.BudgetPoll = 30 * time.Second
	}
	if o.BudgetCacheTTL == 0 {
		o.BudgetCacheTTL = 2 * time.Second
	}
	if o.OffpeakPoll <= 0 {
		o.OffpeakPoll = time.Minute
	}
	return &Pool{st: st, o: o, providers: map[string]*providerPool{}, prios: map[string]prio{}, blocked: map[string]int{},
		offpeak: map[string]*offpeakHold{}, changed: make(chan struct{})}
}

// NewPool builds a legacy pool from cfg.LLM.Providers (provider id = the
// config key). st holds usage_daily (budget, usage_today); it may be nil
// (no budget, no usage).
func NewPool(cfg *config.Config, st db.Store, o PoolOptions) *Pool {
	p := newPool(st, o)
	p.cfg = cfg
	for name, pc := range cfg.LLM.Providers {
		p.providers[name] = newProviderPool(name, pc.MaxConcurrency, pc.RequestsPerMinute)
	}
	return p
}

// NewPoolWithSettings builds the v1.1 pool from the providers of set and
// follows its changes (set.OnChange): max_concurrency and rpm edits apply
// to the next grant (A5.4), new providers appear, deleted providers fail
// their waiting and new requests. The daily budget is
// set.General().DailyBudget. st holds usage_daily; it may be nil.
func NewPoolWithSettings(st db.Store, set *settings.Service, o PoolOptions) *Pool {
	p := newPool(st, o)
	p.set = set
	p.syncProviders()
	set.OnChange(p.syncProviders)
	return p
}

// syncProviders applies the settings' provider list (settings mode) and
// wakes the requests held for the off-peak period (the switch, the tariff
// or the holidays may have changed).
func (p *Pool) syncProviders() {
	defer p.notifyChanged()
	provs := p.set.Providers()
	keep := make(map[string]bool, len(provs))
	p.pmu.Lock()
	defer p.pmu.Unlock()
	for _, pr := range provs {
		keep[pr.ID] = true
		if pp := p.providers[pr.ID]; pp != nil {
			pp.mu.Lock()
			pp.name = pr.Name
			pp.setLimitsLocked(pr.MaxConcurrency, pr.RPM)
			pp.dispatchLocked()
			pp.mu.Unlock()
			continue
		}
		p.providers[pr.ID] = newProviderPool(pr.Name, pr.MaxConcurrency, pr.RPM)
	}
	for id, pp := range p.providers {
		if keep[id] {
			continue
		}
		delete(p.providers, id)
		pp.mu.Lock()
		pp.removed = true
		for _, w := range pp.waiters {
			w.err = Permanent(model.Errorf(CodeProviderDeleted, map[string]any{"provider": pp.name}, fmt.Errorf("提供商「%s」已删除", pp.name)))
			close(w.ch)
		}
		pp.waiters = nil
		if pp.timer != nil {
			pp.timer.Stop()
			pp.timer = nil
		}
		pp.mu.Unlock()
	}
}

func (p *Pool) provider(id string) *providerPool {
	p.pmu.RLock()
	defer p.pmu.RUnlock()
	return p.providers[id]
}

// setPriority registers the FIFO key of a running job (scheduler, at start).
func (p *Pool) setPriority(j model.Job, scopeCreated string) {
	p.mu.Lock()
	p.prios[j.ID] = prio{class: classOf(j.Stage), scopeCreated: scopeCreated, scopeID: j.ScopeID, jobCreated: j.CreatedAt, jobID: j.ID}
	p.mu.Unlock()
}

func (p *Pool) clearPriority(jobID string) {
	p.mu.Lock()
	delete(p.prios, jobID)
	p.mu.Unlock()
}

func (p *Pool) priorityOf(ctx context.Context) prio {
	j, ok := JobFrom(ctx)
	if !ok {
		return prio{noJob: true}
	}
	p.mu.Lock()
	pr, found := p.prios[j.ID]
	p.mu.Unlock()
	if found {
		return pr
	}
	// Not started by this scheduler: same order as ListCurrentJobsByStatus's
	// COALESCE(..., job.created_at).
	return prio{class: classOf(j.Stage), scopeCreated: j.CreatedAt, scopeID: j.ScopeID, jobCreated: j.CreatedAt, jobID: j.ID}
}

// Acquire implements APIPool: it waits for the daily budget and, for a
// job's request to an offpeak_only provider, for the off-peak period (v2
// P12.6), then for a concurrency slot plus a rate token (in priority order)
// of the provider (by id). release frees the slot (idempotent). An unknown
// or deleted provider is a permanent error.
func (p *Pool) Acquire(ctx context.Context, provider string) (func(), error) {
	pp := p.provider(provider)
	if pp == nil {
		return nil, Permanent(model.Errorf(CodeProviderDeleted, map[string]any{"provider": provider}, fmt.Errorf("LLM 提供商 %q 不存在或已删除", provider)))
	}
	for {
		if err := p.waitBudget(ctx); err != nil {
			return nil, err
		}
		waited, err := p.waitOffpeak(ctx, provider)
		if err != nil {
			return nil, err
		}
		if !waited {
			break
		}
		// Hours may have passed: check the budget again.
	}
	if err := pp.acquire(ctx, p.priorityOf(ctx)); err != nil {
		return nil, err
	}
	// The grant and the cancellation can be ready at the same moment (select
	// picks either): a cancelled request never leaves with a slot.
	if err := ctx.Err(); err != nil {
		pp.release()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(pp.release) }, nil
}

func (pp *providerPool) acquire(ctx context.Context, pr prio) error {
	w := &waiter{p: pr, ch: make(chan struct{})}
	pp.mu.Lock()
	if pp.removed {
		pp.mu.Unlock()
		return Permanent(model.Errorf(CodeProviderDeleted, map[string]any{"provider": pp.name}, fmt.Errorf("提供商「%s」已删除", pp.name)))
	}
	pp.seq++
	w.p.seq = pp.seq
	pp.waiters = append(pp.waiters, w)
	pp.dispatchLocked()
	pp.mu.Unlock()
	select {
	case <-w.ch:
		return w.err
	case <-ctx.Done():
		pp.mu.Lock()
		defer pp.mu.Unlock()
		if w.granted {
			pp.inflight--
			pp.dispatchLocked()
		} else {
			pp.removeLocked(w)
		}
		return ctx.Err()
	}
}

func (pp *providerPool) release() {
	pp.mu.Lock()
	pp.inflight--
	pp.dispatchLocked()
	pp.mu.Unlock()
}

func (pp *providerPool) removeLocked(w *waiter) {
	for i, x := range pp.waiters {
		if x == w {
			pp.waiters = append(pp.waiters[:i], pp.waiters[i+1:]...)
			return
		}
	}
}

// dispatchLocked grants slots+tokens to the best waiters while both are
// available (slots: the adaptive limit); when only the token is missing it
// schedules itself for the moment the next token is due.
func (pp *providerPool) dispatchLocked() {
	for len(pp.waiters) > 0 && pp.inflight < pp.limit {
		if pp.rate > 0 {
			now := time.Now()
			pp.tokens += now.Sub(pp.last).Seconds() * pp.rate
			if pp.tokens > pp.burst {
				pp.tokens = pp.burst
			}
			pp.last = now
			if pp.tokens < 1 {
				if pp.timer == nil {
					wait := time.Duration((1 - pp.tokens) / pp.rate * float64(time.Second))
					pp.timer = time.AfterFunc(wait, func() {
						pp.mu.Lock()
						pp.timer = nil
						pp.dispatchLocked()
						pp.mu.Unlock()
					})
				}
				return
			}
			pp.tokens--
		}
		best := pp.pickLocked()
		w := pp.waiters[best]
		pp.waiters = append(pp.waiters[:best], pp.waiters[best+1:]...)
		w.granted = true
		pp.inflight++
		close(w.ch)
	}
}

// pickLocked returns the index of the waiter to grant next: the best by
// prio, except that after UrgentBurst urgent grants in a row while bulk
// requests wait, the best bulk request (perf-llm: translation is never
// starved by a stream of critical-path requests).
func (pp *providerPool) pickLocked() int {
	best, bestBulk := 0, -1
	for i, w := range pp.waiters {
		if i > 0 && w.p.less(pp.waiters[best].p) {
			best = i
		}
		if !w.p.noJob && w.p.class == classBulk && (bestBulk < 0 || w.p.less(pp.waiters[bestBulk].p)) {
			bestBulk = i
		}
	}
	b := pp.waiters[best].p
	switch {
	case b.noJob || b.class != classUrgent || bestBulk < 0:
		pp.urgentRun = 0
	case pp.urgentRun >= UrgentBurst:
		pp.urgentRun = 0
		return bestBulk
	default:
		pp.urgentRun++
	}
	return best
}

// throttled lowers the effective limit after a throttling answer to a
// request sent at sent (perf-llm). A request sent before the last cut does
// not cut again: the burst it belonged to was already answered for.
func (pp *providerPool) throttled(sent time.Time) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if !sent.After(pp.cutAt) {
		return
	}
	// The request has already released its slot: count it as in flight.
	from := pp.limit
	pp.limit = max(1, min(pp.limit, pp.inflight+1)/2)
	pp.cutAt, pp.okRun = time.Now(), 0
	slog.Warn("LLM provider is throttling; lowering the concurrency", "provider", pp.name, "from", from, "to", pp.limit, "max", pp.max)
}

// succeeded counts an answer; every `limit` answers in a row raise a lowered
// limit by one (perf-llm).
func (pp *providerPool) succeeded() {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if pp.limit >= pp.max {
		return
	}
	if pp.okRun++; pp.okRun < pp.limit {
		return
	}
	pp.limit++
	pp.okRun = 0
	if pp.limit == pp.max {
		slog.Info("LLM provider concurrency restored", "provider", pp.name, "max", pp.max)
	}
	pp.dispatchLocked()
}

// Throttled implements llm.RateFeedback: provider answered a request sent at
// sent with HTTP 429 (or 503 / 529); its effective concurrency is halved.
func (p *Pool) Throttled(provider string, sent time.Time) {
	if pp := p.provider(provider); pp != nil {
		pp.throttled(sent)
	}
}

// Succeeded implements llm.RateFeedback: provider answered a request.
func (p *Pool) Succeeded(provider string) {
	if pp := p.provider(provider); pp != nil {
		pp.succeeded()
	}
}

// Limit returns the effective (adaptive) concurrency limit of provider and
// its max_concurrency; ok is false for an unknown provider.
func (p *Pool) Limit(provider string) (limit, max int, ok bool) {
	pp := p.provider(provider)
	if pp == nil {
		return 0, 0, false
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return pp.limit, pp.max, true
}

func (pp *providerPool) status() (inflight, waiting int) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return pp.inflight, len(pp.waiters)
}

func (pp *providerPool) stat() APIStat {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return APIStat{Name: pp.name, Inflight: pp.inflight, Max: pp.max, RPM: pp.rpm}
}

// Full reports whether every concurrency slot of provider is in use (the
// adaptive limit while the provider is throttling).
func (p *Pool) Full(provider string) bool {
	pp := p.provider(provider)
	if pp == nil {
		return false
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return pp.inflight >= pp.limit
}

// Status returns name/inflight/max/rpm per provider id (diagnostics page;
// the map key is the provider id, Name the display name).
func (p *Pool) Status() map[string]APIStat {
	p.pmu.RLock()
	defer p.pmu.RUnlock()
	out := make(map[string]APIStat, len(p.providers))
	for id, pp := range p.providers {
		out[id] = pp.stat()
	}
	return out
}

// ProviderStatus returns the current concurrency of one provider ("3 / 8",
// A10.2). ok is false for an unknown provider.
func (p *Pool) ProviderStatus(providerID string) (inflight, max int, ok bool) {
	pp := p.provider(providerID)
	if pp == nil {
		return 0, 0, false
	}
	st := pp.stat()
	return st.Inflight, st.Max, true
}

// Providers returns the provider ids, sorted.
func (p *Pool) Providers() []string {
	p.pmu.RLock()
	defer p.pmu.RUnlock()
	out := make([]string, 0, len(p.providers))
	for id := range p.providers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ---------- usage_daily and budget (SPEC §17.1, §17.7; v1.1 A10.3) ----------

func dayKey(now time.Time) string { return now.In(time.Local).Format("2006-01-02") }

// nextMidnight returns the next local midnight after now.
func nextMidnight(now time.Time) time.Time {
	l := now.In(time.Local)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, 1)
}

// RecordRequest implements llm.CostRecorder (v2 P12.5): one LLM response of
// providerID with model, whose request was sent at start, is priced in the
// period start falls in (settings.RequestCost: the tariff's price of the
// model, else the provider's price; legacy mode: the [llm.prices] price) and
// added with its tokens to usage_daily of the local day (A10.3). Spending
// therefore counts on the day it happened, whatever the job's later status
// (failed, cancelled, retried, rerun). An unpriced request (no price
// applies) is also counted in settings.UnpricedRequests (D3; lead ruling at
// the phase-2 gate), so the amount shown can say "partly unpriced". The
// budget cache is refreshed. A database error is logged (the response
// itself succeeded). It returns the cost (priced false: no price applies,
// only tokens are counted).
func (p *Pool) RecordRequest(ctx context.Context, providerID, modelName string, start time.Time, promptTokens, cacheHitTokens, completionTokens int64) (cost float64, priced bool) {
	cost, priced = p.requestCost(providerID, modelName, start, promptTokens, cacheHitTokens, completionTokens)
	if p.st == nil {
		return cost, priced
	}
	day := dayKey(p.o.Now())
	u := model.UsageDaily{ProviderID: providerID, Day: day, Requests: 1,
		PromptTokens: promptTokens, CacheHitTokens: cacheHitTokens, CompletionTokens: completionTokens, EstimatedCost: cost}
	if err := p.st.AddUsageDaily(ctx, u); err != nil {
		slog.Warn("usage_daily", "provider", providerID, "err", err)
	}
	if !priced {
		if err := settings.AddUnpricedRequest(ctx, p.st, providerID, day); err != nil {
			slog.Warn("usage_unpriced", "provider", providerID, "err", err)
		}
	}
	p.mu.Lock()
	p.spentOK = false
	p.mu.Unlock()
	return cost, priced
}

// RecordUsage implements llm.UsageRecorder: RecordRequest without a model
// (the provider-level price), started now.
func (p *Pool) RecordUsage(ctx context.Context, providerID string, promptTokens, cacheHitTokens, completionTokens int64) {
	p.RecordRequest(ctx, providerID, "", p.o.Now(), promptTokens, cacheHitTokens, completionTokens)
}

// requestCost prices one request (see RecordRequest).
func (p *Pool) requestCost(providerID, modelName string, start time.Time, promptTokens, cacheHitTokens, completionTokens int64) (float64, bool) {
	if p.set != nil {
		cost, _, ok := p.set.RequestCost(providerID, modelName, start, promptTokens, cacheHitTokens, completionTokens)
		return cost, ok
	}
	pr, ok := p.providerPrice(providerID)
	if !ok {
		return 0, false
	}
	return pr.EstimateCost(promptTokens, cacheHitTokens, completionTokens)
}

// providerPrice returns the price of a provider: its own prices (settings
// mode) or the legacy [llm.prices] entry of the model its roles use.
func (p *Pool) providerPrice(providerID string) (model.Provider, bool) {
	if p.set != nil {
		pr, ok := p.set.Provider(providerID)
		return pr, ok && pr.HasPrices()
	}
	if p.cfg == nil {
		return model.Provider{}, false
	}
	price, ok := settings.LegacyPrice(p.cfg, providerID)
	if !ok {
		return model.Provider{}, false
	}
	in, hit, out := price.Input, price.CacheHit, price.Output
	return model.Provider{ID: providerID, PriceInput: &in, PriceCacheHit: &hit, PriceOutput: &out}, true
}

// ProviderCost estimates the cost of a usage with the provider's prices
// (SPEC §17.7) for a request started now, without a model (v2: the
// provider-level price, in the current period of its tariff). ok is false
// when the provider has no prices. The llm client prices its requests with
// RecordRequest instead; this is the scheduler's fallback.
func (p *Pool) ProviderCost(providerID string, promptTokens, cacheHitTokens, completionTokens int64) (float64, bool) {
	return p.requestCost(providerID, "", p.o.Now(), promptTokens, cacheHitTokens, completionTokens)
}

// anyPrices reports whether some price is configured at all (otherwise
// costs are unknown, not zero): provider-level prices or tariff model
// prices (D3).
func (p *Pool) anyPrices() bool {
	if p.set != nil {
		for _, pr := range p.set.Providers() {
			if settings.HasAnyPrice(pr) {
				return true
			}
		}
		return false
	}
	return p.cfg != nil && len(p.cfg.LLM.Prices) > 0
}

func (p *Pool) dailyBudget() float64 {
	if p.set != nil {
		return p.set.General().DailyBudget
	}
	if p.cfg != nil {
		return p.cfg.LLM.DailyBudget
	}
	return 0
}

// UsageToday returns today's usage_daily summed over providers (local
// date). EstimatedCost is the sum of the per-request estimates recorded
// with the usage (v2 P12.5), nil when no price is configured at all or when
// every request of the day was unpriced (D3: tokens only). Unpriced counts
// the requests without a price (settings.UnpricedRequests).
func (p *Pool) UsageToday(ctx context.Context) (Usage, error) {
	var u Usage
	if p.st == nil {
		return u, nil
	}
	day := dayKey(p.o.Now())
	rows, err := p.st.ListUsageDaily(ctx, day)
	if err != nil {
		return u, err
	}
	cost := 0.0
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Day != day {
			continue
		}
		u.Requests += r.Requests
		u.PromptTokens += r.PromptTokens
		u.CacheHitTokens += r.CacheHitTokens
		u.CompletionTokens += r.CompletionTokens
		cost += r.EstimatedCost
		if !seen[r.ProviderID] {
			seen[r.ProviderID] = true
			n, err := settings.UnpricedRequests(ctx, p.st, r.ProviderID, day)
			if err != nil {
				return u, err
			}
			u.Unpriced += n
		}
	}
	u.Unpriced = min(u.Unpriced, u.Requests)
	if p.anyPrices() && (u.Requests == 0 || u.Unpriced < u.Requests) {
		u.EstimatedCost = &cost
	}
	return u, nil
}

// budgetEnabled: SPEC §17.7 only applies with a price configuration and a
// positive daily budget.
func (p *Pool) budgetEnabled() bool {
	return p.st != nil && p.dailyBudget() > 0 && p.anyPrices()
}

// spentToday returns today's estimated cost and number of unpriced
// requests, cached for BudgetCacheTTL.
func (p *Pool) spentToday(ctx context.Context) (float64, int64, error) {
	now := p.o.Now()
	day := dayKey(now)
	p.mu.Lock()
	if p.spentOK && p.spentDay == day && p.o.BudgetCacheTTL > 0 && now.Sub(p.spentAt) < p.o.BudgetCacheTTL {
		v, n := p.spent, p.unpriced
		p.mu.Unlock()
		return v, n, nil
	}
	p.mu.Unlock()
	u, err := p.UsageToday(ctx)
	if err != nil {
		return 0, 0, err
	}
	v := 0.0
	if u.EstimatedCost != nil {
		v = *u.EstimatedCost
	}
	p.mu.Lock()
	p.spent, p.unpriced, p.spentDay, p.spentAt, p.spentOK = v, u.Unpriced, day, now, true
	p.mu.Unlock()
	return v, u.Unpriced, nil
}

// BudgetUnpriced returns today's number of unpriced requests while the
// daily budget applies, 0 otherwise. Their cost is unknown and not counted
// against the budget (they never block), so the budget may then be
// underestimated: the API says so in the notes of the running LLM stages
// (D3; lead ruling at the phase-2 gate). A database error counts as 0.
func (p *Pool) BudgetUnpriced(ctx context.Context) int64 {
	if !p.budgetEnabled() {
		return 0
	}
	_, n, err := p.spentToday(ctx)
	if err != nil {
		slog.Warn("budget check failed", "err", err)
		return 0
	}
	return n
}

// BudgetExceeded reports whether today's estimated cost reached the daily
// budget. A database error is logged and treated as not exceeded (an error
// caused by a cancelled ctx is not logged: nobody waits for the answer).
func (p *Pool) BudgetExceeded(ctx context.Context) bool {
	exceeded, _ := p.budgetState(ctx)
	return exceeded
}

// budgetState is BudgetExceeded for waitBudget: it returns ctx's error when
// ctx is done (before or during the lookup), so a cancelled request is never
// mistaken for one under budget. A real database error keeps the documented
// fail-open behaviour (not exceeded, nil error).
func (p *Pool) budgetState(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !p.budgetEnabled() {
		return false, nil
	}
	v, _, err := p.spentToday(ctx)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return false, cerr
		}
		slog.Warn("budget check failed", "err", err)
		return false, nil
	}
	return v >= p.dailyBudget(), nil
}

// BudgetBlocked reports whether a running job is currently held in Acquire
// by the daily budget (for waiting_for = budget).
func (p *Pool) BudgetBlocked(jobID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.blocked[jobID] > 0
}

// waitBudget blocks while the daily budget is exhausted, until the next
// local midnight (re-checked every BudgetPoll) or ctx is cancelled.
// Requests already in flight are never interrupted.
func (p *Pool) waitBudget(ctx context.Context) error {
	if !p.budgetEnabled() {
		return ctx.Err()
	}
	exceeded, err := p.budgetState(ctx)
	if err != nil || !exceeded {
		return err
	}
	j, hasJob := JobFrom(ctx)
	if hasJob {
		p.mu.Lock()
		p.blocked[j.ID]++
		p.mu.Unlock()
		defer func() {
			p.mu.Lock()
			if p.blocked[j.ID]--; p.blocked[j.ID] <= 0 {
				delete(p.blocked, j.ID)
			}
			p.mu.Unlock()
		}()
	}
	slog.Info("daily LLM budget reached; holding API requests until tomorrow", "budget", p.dailyBudget())
	for {
		exceeded, err := p.budgetState(ctx)
		if err != nil {
			return err
		}
		if !exceeded {
			return nil
		}
		wait := nextMidnight(p.o.Now()).Sub(p.o.Now())
		if wait > p.o.BudgetPoll || wait <= 0 {
			wait = p.o.BudgetPoll
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// ---------- v2 P12.6: dispatch only off-peak ----------

// notifyChanged wakes every request held for the off-peak period.
func (p *Pool) notifyChanged() {
	p.mu.Lock()
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

// OffpeakWait reports whether new requests to provider are held at now (v2
// P12.6): the provider has offpeak_only and a tariff with peak windows, and
// now is in a peak period. until is when the off-peak period starts (the
// tariff's next change; zero when it never does). The scheduler does not
// start a job of that provider meanwhile: waiting_for = OffpeakReason(until).
func (p *Pool) OffpeakWait(provider string, now time.Time) (waiting bool, until time.Time) {
	if p.set == nil {
		return false, time.Time{}
	}
	peak, next, only, ok := p.set.PeakState(provider, now)
	if !ok || !only || !peak {
		return false, time.Time{}
	}
	return true, next
}

// OffpeakBlocked reports whether a running job has a request held in Acquire
// for the off-peak period, and until when (for waiting_for, like
// BudgetBlocked).
func (p *Pool) OffpeakBlocked(jobID string) (until time.Time, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h := p.offpeak[jobID]; h != nil && h.n > 0 {
		return h.until, true
	}
	return time.Time{}, false
}

// OffpeakReason is the waiting_for of a job held for the off-peak period:
// model.WaitOffpeak with the local wall-clock time the off-peak period
// starts ("--:--" when it never does).
func OffpeakReason(until time.Time) string {
	if until.IsZero() {
		return model.WaitOffpeak("--:--")
	}
	return model.WaitOffpeak(until.In(time.Local).Format("15:04"))
}

// waitOffpeak holds a job's request while provider is offpeak_only and in a
// peak period (OffpeakWait), until the off-peak period starts, the settings
// change or ctx is done; requests in flight are never interrupted. Requests
// without a job (an interactive call the user is waiting for, e.g. 大纲整理)
// are not held. waited reports whether it waited at all.
func (p *Pool) waitOffpeak(ctx context.Context, provider string) (waited bool, err error) {
	j, hasJob := JobFrom(ctx)
	if !hasJob || p.set == nil {
		return false, nil
	}
	defer func() {
		if !waited {
			return
		}
		p.mu.Lock()
		if h := p.offpeak[j.ID]; h != nil {
			if h.n--; h.n <= 0 {
				delete(p.offpeak, j.ID)
			}
		}
		p.mu.Unlock()
	}()
	for {
		now := p.o.Now()
		waiting, until := p.OffpeakWait(provider, now)
		if !waiting {
			return waited, nil
		}
		p.mu.Lock()
		h := p.offpeak[j.ID]
		if h == nil {
			h = &offpeakHold{}
			p.offpeak[j.ID] = h
		}
		if !waited {
			h.n++
		}
		h.until = until
		changed := p.changed
		p.mu.Unlock()
		if !waited {
			waited = true
			slog.Info("provider dispatches only off-peak; holding the request", "provider", provider, "job", j.ID, "until", until)
		}
		wait := p.o.OffpeakPoll
		if !until.IsZero() {
			if d := until.Sub(now); d < wait {
				wait = d
			}
		}
		if wait <= 0 {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return waited, ctx.Err()
		case <-changed:
			t.Stop()
		case <-t.C:
		}
	}
}

var _ APIPool = (*Pool)(nil)
