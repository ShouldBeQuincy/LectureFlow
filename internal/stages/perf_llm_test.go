// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// perf-llm: translation throughput and prompt-cache use, measured on the
// fake LLM (SPEC §14) through the real llm client, http.Client and API pool.
//
// Each run is a testing/synctest bubble. Time there is a fake clock that
// moves only when every goroutine of the run waits — for the fake's answer
// delay, the pool's next rate-limit token, the warm-up gate, a retry
// backoff — so the elapsed time is exactly what the scheduling costs on the
// provider (the rounds of serial waiting), whatever the machine's load. The
// fake answers in perfLatency, the ~1.2 s of a translate request profiled
// in .claude/research/pipeline-perf.md §3.2, and the limits are the real
// ones (300 per minute, not scaled; the old test scaled time 10 times and
// so also the token bucket's burst). The local work per request (SQLite
// write, JSON, QA; about 1 ms) takes no time on the fake clock; the real
// time of each run is logged beside it. Requests go to the fake in-process
// (llmtest.Server.Transport): a connection would stop the bubble's clock.

// perfLatency is the fake's answer delay: one translate request on DeepSeek.
const perfLatency = 1200 * time.Millisecond

type translateRun struct {
	elapsed   time.Duration // on the fake clock: the time the run takes on the provider
	wall      time.Duration // real time of the run (local work only; logged, not asserted)
	requests  int
	misses    int // requests whose system prompt was not cached yet
	peak      int // most requests in flight at the fake
	stats     model.JobStats
	throttled int // HTTP 429 answers
	limit     int // the pool's adaptive limit afterwards
}

// runTranslatePerf translates a lecture of n sentences with max_concurrency
// conc and rpm (0 = none), the warm-up on or off; serverLimit > 0 makes the
// fake refuse more than that many requests in flight (HTTP 429).
func runTranslatePerf(t *testing.T, n, conc, rpm int, warm bool, serverLimit int) translateRun {
	t.Helper()
	old := warmupOff
	warmupOff = !warm
	defer func() { warmupOff = old }()

	// The fake is created outside the bubble: its (unused) listener's
	// goroutine would never be durably blocked inside it.
	fake := llmtest.New(t)
	var r translateRun
	wall := time.Now()
	synctest.Test(t, func(t *testing.T) {
		e := newPerfEnv(t, fake)
		e.fake.SetDelay(perfLatency)
		e.fake.SetCacheAfterAnswer(true)
		e.fake.SetConcurrencyLimit(serverLimit)
		e.fake.Handle(llmtest.RoleTranslate, func(_, user string) string {
			return fmt.Sprintf(`{"idx": %d, "zh": "我们现在来看存储层次结构怎样让数据靠近处理器。"}`, llmtest.TargetIdx(user))
		})
		p := e.cfg.LLM.Providers["deepseek"]
		p.MaxConcurrency, p.RequestsPerMinute = conc, rpm
		e.cfg.LLM.Providers["deepseek"] = p
		pool := sched.NewPool(e.cfg, nil, sched.PoolOptions{})
		// The client's default backoff (SPEC §12: 2, 4, 8, 16 s) costs
		// nothing real on the fake clock.
		c := llm.New(e.cfg, pool, "", &http.Client{Transport: e.fake.Transport()})

		l := e.lecture("Perf", "m.mp4")
		ss := make([]model.Sentence, n)
		for i := range ss {
			text := "We now look at how the memory hierarchy keeps data close to the processor."
			ss[i] = model.Sentence{LectureID: l.ID, Idx: i, StartMS: int64(i) * 4000, EndMS: int64(i)*4000 + 3500,
				ASRText: text, RepairedText: text, ZhStatus: model.ZhNone}
		}
		e.insertSentences(l.ID, ss)
		e.setSummary(l.ID, "A lecture about the memory hierarchy: caches, main memory and how locality keeps data close.")

		start := time.Now() // the bubble's clock
		res := e.run(e.stage(e.deps(c), model.StageTranslate), l.ID)
		elapsed := time.Since(start)
		if res.err != nil {
			t.Fatalf("translate (%d / %d, warm %v): %v", conc, rpm, warm, res.err)
		}
		for _, s := range e.sentences(l.ID) {
			if s.ZhStatus != model.ZhOK {
				t.Fatalf("sentence %d: %s", s.Idx, s.ZhStatus)
			}
		}
		limit, _, _ := pool.Limit("deepseek")
		r = translateRun{elapsed: elapsed, requests: fake.Count(llmtest.RoleTranslate), misses: fake.CacheMisses(),
			peak: fake.MaxInflight(), stats: res.stats, throttled: fake.Throttled(), limit: limit}
	})
	r.wall = time.Since(wall)
	return r
}

// newPerfEnv is newEnv inside a synctest bubble, with the fake created
// outside it (runTranslatePerf).
func newPerfEnv(t *testing.T, fake *llmtest.Server) *env {
	t.Helper()
	root := t.TempDir()
	st, err := db.Open(filepath.Join(root, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.DataDir = filepath.Join(root, "data")
	e := &env{t: t, ctx: context.Background(), root: root, st: st, cfg: cfg, ev: &recEvents{}, gpuMB: 12282, fake: fake}
	fake.Configure(cfg)
	e.course = model.Course{ID: db.NewID(), Name: "Computer Architecture", CreatedAt: db.Now()}
	if err := st.CreateCourse(e.ctx, e.course); err != nil {
		t.Fatal(err)
	}
	return e
}

func (r translateRun) String() string {
	return fmt.Sprintf("≈ %.1f s on DeepSeek (real %v), %d requests, at most %d in flight, %d cache misses, prompt %d tokens of which %d cached",
		r.elapsed.Seconds(), r.wall.Round(time.Millisecond), r.requests, r.peak, r.misses, r.stats.PromptTokens, r.stats.CacheHitTokens)
}

// TestTranslatePerfBeforeAfter compares one lecture of 290 sentences (the
// median of the profiled lectures, .claude/research/pipeline-perf.md):
// before perf-llm (8 at a time, 300 per minute, no warm-up), the new
// DeepSeek limits without the warm-up, and with it.
func TestTranslatePerfBeforeAfter(t *testing.T) {
	const n = 290
	before := runTranslatePerf(t, n, 8, 300, false, 0)
	noWarm := runTranslatePerf(t, n, 64, 0, false, 0)
	after := runTranslatePerf(t, n, 64, 0, true, 0)
	t.Logf("before (8 / 300 rpm):       %s", before)
	t.Logf("64 / no limit, no warm-up:  %s", noWarm)
	t.Logf("64 / no limit, warm-up:     %s", after)

	for _, r := range []translateRun{before, noWarm, after} {
		if r.requests != n {
			t.Errorf("%d translate requests for %d sentences: %s", r.requests, n, r)
		}
	}
	if after.elapsed*4 > before.elapsed {
		t.Errorf("the new limits should translate at least 4 times faster: %v vs %v", after.elapsed, before.elapsed)
	}
	// The warm-up holds the others back for the first answer only: it costs
	// one request's time over sending everything at once.
	if after.elapsed > noWarm.elapsed+perfLatency {
		t.Errorf("the warm-up should cost at most one request (%v): %v with it, %v without", perfLatency, after.elapsed, noWarm.elapsed)
	}
	// The old limits held; the new ones are used in full.
	if before.peak > 8 {
		t.Errorf("8 / 300 rpm: %d requests in flight", before.peak)
	}
	if noWarm.peak != 64 || after.peak != 64 {
		t.Errorf("64 / no limit: at most %d (no warm-up) and %d (warm-up) in flight, want 64", noWarm.peak, after.peak)
	}
	// Sent all at once, the first wave misses the cache; the warm-up
	// leaves only the first request.
	if noWarm.misses < 32 {
		t.Errorf("without warm-up %d misses; the fake's cache model should make the first wave miss", noWarm.misses)
	}
	if after.misses > 2 {
		t.Errorf("with warm-up %d cache misses, want the first request only", after.misses)
	}
	if after.stats.CacheHitTokens <= noWarm.stats.CacheHitTokens {
		t.Errorf("cached tokens: warm-up %d, without %d", after.stats.CacheHitTokens, noWarm.stats.CacheHitTokens)
	}
}

// ---------- batch translation: when its requests are sent (T-PERF-04) ----------

// batchPerfBad are the sentences of the batch perf lecture whose translation
// in a batch answer fails QA (it is empty), so that they are retranslated
// alone: 14 in 10 of 42 chunks, as in acceptance T4 — one in chunk 1 (the
// second warm-up chunk on DeepSeek), three in one chunk, two in two chunks.
var batchPerfBad = map[int]bool{13: true, 50: true, 51: true, 52: true, 90: true, 91: true, 120: true, 121: true,
	150: true, 180: true, 210: true, 250: true, 300: true, 350: true}

const perfZh = "我们现在来看存储层次结构怎样让数据靠近处理器。"

type batchPerfRun struct {
	elapsed  time.Duration // on the fake clock
	requests int           // translate requests
	batches  int           // of which batch requests
	misses   int           // requests whose system prompt was not cached yet (OpenAI-compatible fake only)
	peak     int           // most requests in flight at the fake
}

func (r batchPerfRun) String() string {
	return fmt.Sprintf("%.1f s, %d requests (%d batches), at most %d in flight, %d cache misses",
		r.elapsed.Seconds(), r.requests, r.batches, r.peak, r.misses)
}

// runBatchPerf translates a lecture of 420 sentences in batches of 10 with
// 64 at a time, every request answered in perfLatency, through the real
// client and the fake's in-process transport. provider:
//
//   - "config": the config.toml provider (OpenAI-compatible, not DeepSeek's
//     host) through the API pool: the one-step warm-up;
//   - "deepseek": a settings provider with DeepSeek's balance API: the
//     two-step warm-up;
//   - "claude": a settings provider of type anthropic with a streamed
//     Claude model: the one-step warm-up, as Claude runs.
//
// off is the sending order before T-PERF-04 (batchTimingOff).
func runBatchPerf(t *testing.T, provider string, off bool) batchPerfRun {
	t.Helper()
	old := batchTimingOff
	batchTimingOff = off
	defer func() { batchTimingOff = old }()

	const n, size, conc = 420, 10, 64
	fake := llmtest.New(t) // outside the bubble (runTranslatePerf)
	var r batchPerfRun
	synctest.Test(t, func(t *testing.T) {
		e := newPerfEnv(t, fake)
		fake.SetDelay(perfLatency)
		fake.SetCacheAfterAnswer(true)
		fake.Handle(llmtest.RoleTranslate, func(system, user string) string {
			if !llmtest.IsBatchSystem(system) {
				b, _ := json.Marshal(map[string]any{"idx": llmtest.TargetIdx(user), "zh": perfZh})
				return string(b)
			}
			var items []map[string]any
			for _, idx := range llmtest.TargetIdxs(user) {
				zh := perfZh
				if batchPerfBad[idx] {
					zh = ""
				}
				items = append(items, map[string]any{"idx": idx, "zh": zh})
			}
			b, _ := json.Marshal(map[string]any{"items": items})
			return string(b)
		})
		l := e.lecture("Perf", "m.mp4")
		ss := make([]model.Sentence, n)
		for i := range ss {
			text := "We now look at how the memory hierarchy keeps data close to the processor."
			ss[i] = model.Sentence{LectureID: l.ID, Idx: i, StartMS: int64(i) * 4000, EndMS: int64(i)*4000 + 3500,
				ASRText: text, RepairedText: text, ZhStatus: model.ZhNone}
		}
		e.insertSentences(l.ID, ss)
		e.setSummary(l.ID, "A lecture about the memory hierarchy: caches, main memory and how locality keeps data close.")
		e.setTranslateBatch(l.ID, size)

		hc := &http.Client{Transport: fake.Transport()}
		var d Deps
		if provider == "config" {
			p := e.cfg.LLM.Providers["deepseek"]
			p.MaxConcurrency, p.RequestsPerMinute = conc, 0
			e.cfg.LLM.Providers["deepseek"] = p
			d = e.deps(llm.New(e.cfg, sched.NewPool(e.cfg, nil, sched.PoolOptions{}), "", hc))
		} else {
			typ, balance, modelName := model.ProviderOpenAI, model.BalanceDeepSeek, "deepseek-chat"
			if provider == "claude" {
				typ, balance, modelName = model.ProviderAnthropic, model.BalanceNone, "claude-opus-5-5"
			}
			set, err := settings.Open(e.ctx, e.st, e.cfg)
			e.must(err)
			str := func(s string) *string { return &s }
			p, err := set.CreateProvider(e.ctx, settings.ProviderInput{Name: str("P"), Type: str(typ),
				BaseURL: str(fake.URL), APIKey: str(llmtest.TestKey), BalanceAPI: str(balance)})
			e.must(err)
			snap, ok := model.ParseLLMSnapshot(e.job(model.ScopeLecture, l.ID, model.StageTranslate).ConfigSnapshot)
			if !ok {
				t.Fatal("no translate snapshot")
			}
			snap.ProviderID, snap.ProviderName, snap.Model = p.ID, p.Name, modelName
			e.setSnapshot(l.ID, model.StageTranslate, model.MarshalSnapshot(snap))
			d = e.deps(llm.NewWithSettings(set, nil, nil, "", hc))
			d.Settings = set
			d.Concurrency = conc
			if d.deepSeek(p.ID) != (provider == "deepseek") || d.anthropic(p.ID) != (provider == "claude") {
				t.Fatalf("%s: deepSeek %v, anthropic %v", provider, d.deepSeek(p.ID), d.anthropic(p.ID))
			}
		}

		start := time.Now() // the bubble's clock
		res := e.run(e.stage(d, model.StageTranslate), l.ID)
		elapsed := time.Since(start)
		if res.err != nil {
			t.Fatalf("translate (%s, off %v): %v", provider, off, res.err)
		}
		for _, s := range e.sentences(l.ID) {
			if s.ZhStatus != model.ZhOK || s.Zh == nil || *s.Zh != perfZh {
				t.Fatalf("sentence %d: %s", s.Idx, s.ZhStatus)
			}
		}
		reqs := fake.Requests(llmtest.RoleTranslate)
		if provider == "claude" && (len(reqs) == 0 || !reqs[0].Stream) {
			t.Fatal("claude: the requests are not streamed")
		}
		r = batchPerfRun{elapsed: elapsed, requests: len(reqs), batches: len(fake.BatchRequests()),
			misses: fake.CacheMisses(), peak: fake.MaxInflight()}
	})
	return r
}

// TestTranslateBatchPerfBeforeAfter compares the sending order before and
// after T-PERF-04 on a lecture shaped like acceptance T4 (42 batches of 10,
// 14 sentences retranslated after QA), with every request answered in
// perfLatency (L = 1.2 s). The requests are the same 56; what changes is
// when they are sent:
//
//	DeepSeek (two warm-up steps)
//	  before: chunk 0, chunk 1, chunk 1's retranslation, the 40 others,
//	          then up to three retranslations in a row            7 L = 8.4 s
//	  after:  chunk 0, chunk 1, the 40 others beside chunk 1's
//	          retranslation, one retranslation, the other twelve  5 L = 6.0 s
//	one step (the config.toml provider, Claude)
//	  before: chunk 0, the 41 others, three retranslations in a row  5 L = 6.0 s
//	  after:  chunk 0, the 41 others, one retranslation, the other
//	          thirteen                                               4 L = 4.8 s
//
// On the one-step path no warm-up chunk has a retranslation here: before,
// the first retranslations of ten chunks went out together and each missed
// the cache of their own prefix (11 misses with chunk 0); now the first one
// goes alone (2 misses). The fake answers every request in the same time; a
// real provider answers a batch in about 5 s and a retranslation in about
// 2 s (TestTranslateBatchTimingTail has that shape).
func TestTranslateBatchPerfBeforeAfter(t *testing.T) {
	const L = perfLatency
	for _, c := range []struct {
		provider                  string
		before, after             time.Duration
		missesBefore, missesAfter int // -1: not counted by the fake (Anthropic)
	}{
		{"deepseek", 7 * L, 5 * L, 2, 2},
		{"config", 5 * L, 4 * L, 11, 2},
		{"claude", 5 * L, 4 * L, -1, -1},
	} {
		t.Run(c.provider, func(t *testing.T) {
			before := runBatchPerf(t, c.provider, true)
			after := runBatchPerf(t, c.provider, false)
			t.Logf("before: %s", before)
			t.Logf("after:  %s", after)
			for _, r := range []batchPerfRun{before, after} {
				if r.requests != 56 || r.batches != 42 {
					t.Errorf("%d requests, %d batches, want 56 and 42: %s", r.requests, r.batches, r)
				}
				if r.peak > 64 {
					t.Errorf("%d requests in flight with 64 at a time", r.peak)
				}
			}
			if before.elapsed != c.before || after.elapsed != c.after {
				t.Errorf("the run took %v before and %v after, want %v and %v", before.elapsed, after.elapsed, c.before, c.after)
			}
			if c.missesAfter >= 0 && (before.misses != c.missesBefore || after.misses != c.missesAfter) {
				t.Errorf("cache misses: %d before and %d after, want %d and %d", before.misses, after.misses, c.missesBefore, c.missesAfter)
			}
		})
	}
}

// TestTranslateSurvivesThrottling (perf-llm): with 64 at a time against a
// provider that takes only 16 (HTTP 429 above), the pool lowers its limit
// and the retries succeed: the lecture is translated, nothing fails.
func TestTranslateSurvivesThrottling(t *testing.T) {
	r := runTranslatePerf(t, 200, 64, 0, true, 16)
	t.Logf("throttled: %s; %d × 429, adaptive limit now %d", r, r.throttled, r.limit)
	if r.throttled == 0 || r.limit >= 64 {
		t.Errorf("429 answers %d, limit %d: the provider's limit should have been hit and the pool's lowered", r.throttled, r.limit)
	}
}
