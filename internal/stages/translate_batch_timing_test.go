// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// v3.2.x r2 T-PERF-04: when the requests of a batch run are sent. The
// timelines run in a testing/synctest bubble on a scripted client: a batch
// request is answered tlBatch after it was sent, a single-sentence request
// tlSingle after, on the bubble's fake clock, so the times below are exact
// and local work (QA, SQLite) takes none. "before" is the sending order
// before T-PERF-04 (batchTimingOff).

const (
	tlBatch  = 5 * time.Second
	tlSingle = 2 * time.Second
)

// timedCall is one request the timeline client received.
type timedCall struct {
	batch      bool
	idxs       []int         // its TARGET sentences
	start, end time.Duration // since the run began; end < 0: not answered
}

// timeline scripts a translate run and records when its requests were sent
// and answered.
type timeline struct {
	// bad: sentences whose translation in a batch answer is empty (QA fails
	// and the sentence is retranslated alone); single requests answer well.
	bad map[int]bool
	// delay, when set and > 0, is a request's answer time.
	delay func(c timedCall) time.Duration
	// raw, when set and it handles a request, gives its answer (after the
	// answer time).
	raw func(c timedCall) (handled bool, text string, err error)

	mu       sync.Mutex
	begin    time.Time
	calls    []*timedCall
	inflight int
	peak     int
}

func badSet(idxs ...int) map[int]bool {
	m := map[int]bool{}
	for _, i := range idxs {
		m[i] = true
	}
	return m
}

func (tl *timeline) client() *scriptLLM {
	return batchScript(func(idx int, batch bool) string {
		if batch && tl.bad[idx] {
			return ""
		}
		return cacheZh(idx, batch)
	}, func(ctx context.Context, _ int, c llmCall) (bool, string, error) {
		tc := &timedCall{batch: llmtest.IsBatchSystem(c.System), end: -1}
		d := tlSingle
		if tc.batch {
			tc.idxs, d = llmtest.TargetIdxs(c.User), tlBatch
		} else {
			tc.idxs = []int{llmtest.TargetIdx(c.User)}
		}
		tl.mu.Lock()
		tc.start = time.Since(tl.begin)
		if tl.delay != nil {
			if v := tl.delay(*tc); v > 0 {
				d = v
			}
		}
		tl.calls = append(tl.calls, tc)
		tl.inflight++
		tl.peak = max(tl.peak, tl.inflight)
		tl.mu.Unlock()
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		tl.mu.Lock()
		tc.end = time.Since(tl.begin)
		tl.inflight--
		tl.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return true, "", err
		}
		if tl.raw != nil {
			return tl.raw(*tc)
		}
		return false, "", nil
	})
}

// all returns the requests in the order they were sent.
func (tl *timeline) all() []timedCall {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	out := make([]timedCall, len(tl.calls))
	for i, c := range tl.calls {
		out[i] = *c
	}
	return out
}

// batches returns the batch requests for exactly the sentences [first,
// first+n), in the order they were sent.
func (tl *timeline) batches(first, n int) []timedCall {
	var out []timedCall
	for _, c := range tl.all() {
		if c.batch && len(c.idxs) == n && c.idxs[0] == first {
			out = append(out, c)
		}
	}
	return out
}

// singles returns the single-sentence requests for sentence idx.
func (tl *timeline) singles(idx int) []timedCall {
	var out []timedCall
	for _, c := range tl.all() {
		if !c.batch && c.idxs[0] == idx {
			out = append(out, c)
		}
	}
	return out
}

// count returns the number of batch and of single-sentence requests.
func (tl *timeline) count() (batch, single int) {
	for _, c := range tl.all() {
		if c.batch {
			batch++
		} else {
			single++
		}
	}
	return
}

// cold counts the requests of a kind (batch / single: each kind has its own
// system prompt, the prefix a provider caches) that were sent before need
// requests of that kind had been answered: the requests that cannot read the
// whole cached prefix. need is 2 on DeepSeek (acceptance T4: the first
// request of a prefix hit 0 tokens, the second a part, the later ones all
// of it), 1 elsewhere.
func (tl *timeline) cold(batch bool, need int) int {
	calls := tl.all()
	n := 0
	for _, c := range calls {
		if c.batch != batch {
			continue
		}
		answered := 0
		for _, o := range calls {
			if o.batch == batch && o.end >= 0 && o.end <= c.start {
				answered++
			}
		}
		if answered < need {
			n++
		}
	}
	return n
}

// lastBatchEnd is when the last batch request was answered.
func (tl *timeline) lastBatchEnd() time.Duration {
	var last time.Duration
	for _, c := range tl.all() {
		if c.batch {
			last = max(last, c.end)
		}
	}
	return last
}

type timingRun struct {
	n, batch, conc int
	deepSeek       bool
	off            bool          // batchTimingOff: the order before T-PERF-04
	cancelAt       time.Duration // > 0: the run is cancelled then
	log            bool          // capture the service log
}

type timingResult struct {
	err       error
	elapsed   time.Duration
	sentences []model.Sentence
	stats     model.JobStats
	log       string
}

// run translates a lecture of r.n generated sentences on the timeline's
// client, in a synctest bubble.
func (tl *timeline) run(t *testing.T, r timingRun) timingResult {
	t.Helper()
	old := batchTimingOff
	batchTimingOff = r.off
	defer func() { batchTimingOff = old }()
	fake := llmtest.New(t) // outside the bubble (runTranslatePerf); unused: the client is scripted
	var res timingResult
	synctest.Test(t, func(t *testing.T) {
		e := newPerfEnv(t, fake)
		var log *syncBuf
		if r.log {
			log = captureLog(t)
		}
		l := e.cacheLecture(r.n, r.batch, r.deepSeek)
		d := e.deps(tl.client())
		d.Concurrency = r.conc
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		if r.cancelAt > 0 {
			tm := time.AfterFunc(r.cancelAt, cancel)
			defer tm.Stop()
		}
		tl.begin = time.Now()
		rr := e.runCtx(ctx, e.stage(d, model.StageTranslate), l.ID)
		res = timingResult{err: rr.err, elapsed: time.Since(tl.begin), sentences: e.sentences(l.ID), stats: rr.stats}
		if log != nil {
			res.log = log.String()
		}
	})
	return res
}

// allTranslated fails unless every sentence but except is ok with cacheZh.
func (r timingResult) allTranslated(t *testing.T, except ...int) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("run: %v", r.err)
	}
	for i, s := range r.sentences {
		if slices.Contains(except, i) {
			continue
		}
		if s.ZhStatus != model.ZhOK || s.Zh == nil || *s.Zh != cacheZh(i, true) {
			t.Fatalf("sentence %d: %s %v", i, s.ZhStatus, s.Zh)
		}
	}
}

func secs(ds ...time.Duration) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = fmt.Sprintf("%gs", d.Seconds())
	}
	return strings.Join(out, " ")
}

func starts(cs []timedCall) []time.Duration {
	out := make([]time.Duration, len(cs))
	for i, c := range cs {
		out[i] = c.start
	}
	return out
}

// The warm-up gate opens when the warm-up chunk's batch request was
// answered, not when its QA retranslation is done. Six chunks of 10, six at
// a time; sentence 3 (chunk 0) and sentence 13 (chunk 1) fail QA in their
// batch. One step (every provider but DeepSeek, Claude included): chunks
// 1–5 are sent at 5 s, when chunk 0 was answered, while sentence 3 is
// retranslated from 5 to 7 s; before, at 7 s. DeepSeek: chunk 1 at 5 s and
// chunks 2–5 at 10 s, when chunk 1 was answered; before, at 7 and 14 s.
func TestTranslateBatchTimingGateOpensAtBatchAnswer(t *testing.T) {
	const s = time.Second
	for _, c := range []struct {
		name          string
		deepSeek, off bool
		second, rest  time.Duration // when chunk 1 and chunks 2–5 are sent
		redo13        time.Duration // when sentence 13 is retranslated
		total         time.Duration
	}{
		{"one step", false, false, 5 * s, 5 * s, 10 * s, 12 * s},
		{"one step, before", false, true, 7 * s, 7 * s, 12 * s, 14 * s},
		{"DeepSeek", true, false, 5 * s, 10 * s, 10 * s, 15 * s},
		{"DeepSeek, before", true, true, 7 * s, 14 * s, 12 * s, 19 * s},
	} {
		t.Run(c.name, func(t *testing.T) {
			tl := &timeline{bad: badSet(3, 13)}
			res := tl.run(t, timingRun{n: 60, batch: 10, conc: 6, deepSeek: c.deepSeek, off: c.off})
			res.allTranslated(t)
			if b, sg := tl.count(); b != 6 || sg != 2 {
				t.Fatalf("%d batch and %d single requests", b, sg)
			}
			redo3 := tl.singles(3)[0]
			if redo3.start != 5*s || redo3.end != 7*s {
				t.Errorf("sentence 3 retranslated %s–%s, want 5s–7s", secs(redo3.start), secs(redo3.end))
			}
			first := tl.batches(10, 10)[0]
			if first.start != c.second {
				t.Errorf("chunk 1 sent at %s, want %s", secs(first.start), secs(c.second))
			}
			for k := 2; k < 6; k++ {
				if got := tl.batches(k*10, 10)[0].start; got != c.rest {
					t.Errorf("chunk %d sent at %s, want %s", k, secs(got), secs(c.rest))
				}
			}
			if !c.off && first.start >= redo3.end {
				t.Error("the gate waited for chunk 0's retranslation")
			}
			if got := tl.singles(13)[0].start; got != c.redo13 {
				t.Errorf("sentence 13 retranslated at %s, want %s", secs(got), secs(c.redo13))
			}
			if res.elapsed != c.total {
				t.Errorf("the run took %s, want %s", secs(res.elapsed), secs(c.total))
			}
			// no batch request before the warm-up batches were answered
			need := 1
			if c.deepSeek {
				need = 2
			}
			if cold := tl.cold(true, need); cold != need {
				t.Errorf("%d batch requests sent before %d were answered, want the warm-up ones only", cold, need)
			}
		})
	}
}

// The QA retranslations of one chunk are sent side by side. Six chunks of
// 10, six at a time; sentences 40–43 (chunk 4) fail QA in their batch.
//
//   - Prefix warm (sentence 3 of chunk 0 was retranslated before): all four
//     are sent when chunk 4 was answered and the chunk takes one
//     retranslation's time, not four.
//   - Prefix cold: the run's first retranslation goes alone, on DeepSeek the
//     second one too, then the others together (lateWarm).
func TestTranslateBatchTimingRetranslationsSideBySide(t *testing.T) {
	const s = time.Second
	for _, c := range []struct {
		name          string
		deepSeek      bool
		bad           []int
		want          []time.Duration // when 40, 41, 42, 43 are retranslated
		total, before time.Duration
		cold          int // single requests sent before the prefix was warm
	}{
		{"prefix warm", false, []int{3, 40, 41, 42, 43}, []time.Duration{10 * s, 10 * s, 10 * s, 10 * s}, 12 * s, 20 * s, 1},
		{"cold, one step", false, []int{40, 41, 42, 43}, []time.Duration{10 * s, 12 * s, 12 * s, 12 * s}, 14 * s, 18 * s, 1},
		{"cold, DeepSeek", true, []int{40, 41, 42, 43}, []time.Duration{15 * s, 17 * s, 19 * s, 19 * s}, 21 * s, 23 * s, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			tl := &timeline{bad: badSet(c.bad...)}
			res := tl.run(t, timingRun{n: 60, batch: 10, conc: 6, deepSeek: c.deepSeek})
			res.allTranslated(t)
			var got []time.Duration
			for idx := 40; idx <= 43; idx++ {
				cs := tl.singles(idx)
				if len(cs) != 1 {
					t.Fatalf("%d single requests for sentence %d", len(cs), idx)
				}
				got = append(got, cs[0].start)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("sentences 40–43 retranslated at %s, want %s", secs(got...), secs(c.want...))
			}
			if res.elapsed != c.total {
				t.Errorf("the run took %s, want %s", secs(res.elapsed), secs(c.total))
			}
			need := 1
			if c.deepSeek {
				need = 2
			}
			if cold := tl.cold(false, need); cold != c.cold {
				t.Errorf("%d retranslations sent before their prefix was warm, want %d", cold, c.cold)
			}
			if tl.peak > 6 {
				t.Errorf("%d requests in flight with 6 at a time", tl.peak)
			}

			// before: one after the other
			old := &timeline{bad: badSet(c.bad...)}
			was := old.run(t, timingRun{n: 60, batch: 10, conc: 6, deepSeek: c.deepSeek, off: true})
			was.allTranslated(t)
			var at []time.Duration
			for idx := 40; idx <= 43; idx++ {
				at = append(at, old.singles(idx)[0].start)
			}
			for k := 1; k < 4; k++ {
				if at[k] != at[k-1]+tlSingle {
					t.Errorf("before: sentences 40–43 retranslated at %s, want one after the other", secs(at...))
					break
				}
			}
			if was.elapsed != c.before {
				t.Errorf("before: the run took %s, want %s", secs(was.elapsed), secs(c.before))
			}
			b1, s1 := tl.count()
			if b0, s0 := old.count(); b0 != b1 || s0 != s1 {
				t.Errorf("requests: %d + %d, before %d + %d", b1, s1, b0, s0)
			}
		})
	}
}

// The two halves of a split chunk are sent side by side. Chunk 4's batch
// answer is unusable twice (5–10 s, 10–15 s); its halves 40–44 and 45–49
// are both sent at 15 s and the run ends at 20 s. Before: 45–49 at 20 s,
// the run ends at 25 s.
func TestTranslateBatchTimingSplitHalvesSideBySide(t *testing.T) {
	const s = time.Second
	unusable := func(c timedCall) (bool, string, error) {
		if c.batch && len(c.idxs) == 10 && c.idxs[0] == 40 {
			return true, `{"items": []}`, nil
		}
		return false, "", nil
	}
	for _, c := range []struct {
		name   string
		off    bool
		second time.Duration // when 45–49 is sent
		total  time.Duration
	}{
		{"after", false, 15 * s, 20 * s},
		{"before", true, 20 * s, 25 * s},
	} {
		t.Run(c.name, func(t *testing.T) {
			tl := &timeline{raw: unusable}
			res := tl.run(t, timingRun{n: 60, batch: 10, conc: 6, off: c.off})
			res.allTranslated(t)
			if got := starts(tl.batches(40, 10)); !slices.Equal(got, []time.Duration{5 * s, 10 * s}) {
				t.Fatalf("chunk 4 sent at %s, want 5s 10s", secs(got...))
			}
			lo, hi := tl.batches(40, 5), tl.batches(45, 5)
			if len(lo) != 1 || len(hi) != 1 {
				t.Fatalf("%d and %d requests for the halves", len(lo), len(hi))
			}
			if lo[0].start != 15*s || hi[0].start != c.second {
				t.Errorf("halves sent at %s and %s, want 15s and %s", secs(lo[0].start), secs(hi[0].start), secs(c.second))
			}
			if res.elapsed != c.total {
				t.Errorf("the run took %s, want %s", secs(res.elapsed), secs(c.total))
			}
			if b, sg := tl.count(); b != 9 || sg != 0 {
				t.Errorf("%d batch and %d single requests, want 9 and 0", b, sg)
			}
		})
	}
}

// A run shaped like acceptance T4 (DeepSeek, more workers than chunks): 8
// chunks of 10; sentence 12 (chunk 1, the second warm-up chunk), sentences
// 30–32 (chunk 3) and 55 (chunk 5) fail QA in their batch.
//
//	before: chunk 0 0–5, chunk 1 5–10, sentence 12 10–12, chunks 2–7 12–17,
//	        sentences 30, 31, 32 17–19–21–23 (55: 17–19)        → 23 s
//	after:  chunk 0 0–5, chunk 1 5–10, chunks 2–7 10–15 (sentence 12 10–12),
//	        one retranslation 15–17, the other three 17–19      → 19 s
//
// The same requests are sent; none that read the cache before is sent
// before its prefix is warm now.
func TestTranslateBatchTimingTail(t *testing.T) {
	const s = time.Second
	run := func(off bool) (*timeline, timingResult) {
		tl := &timeline{bad: badSet(12, 30, 31, 32, 55)}
		res := tl.run(t, timingRun{n: 80, batch: 10, conc: 8, deepSeek: true, off: off})
		res.allTranslated(t)
		if b, sg := tl.count(); b != 8 || sg != 5 {
			t.Fatalf("off %v: %d batch and %d single requests, want 8 and 5", off, b, sg)
		}
		return tl, res
	}
	before, was := run(true)
	after, now := run(false)
	t.Logf("before: %s in all, gate open at %s, last batch answered at %s, tail %s; requests sent before their prefix was warm: %d batch, %d single",
		secs(was.elapsed), secs(before.batches(20, 10)[0].start), secs(before.lastBatchEnd()), secs(was.elapsed-before.lastBatchEnd()),
		before.cold(true, 2), before.cold(false, 2))
	t.Logf("after:  %s in all, gate open at %s, last batch answered at %s, tail %s; requests sent before their prefix was warm: %d batch, %d single",
		secs(now.elapsed), secs(after.batches(20, 10)[0].start), secs(after.lastBatchEnd()), secs(now.elapsed-after.lastBatchEnd()),
		after.cold(true, 2), after.cold(false, 2))

	if was.elapsed != 23*s || now.elapsed != 19*s {
		t.Errorf("the run took %s before and %s after, want 23s and 19s", secs(was.elapsed), secs(now.elapsed))
	}
	if g0, g1 := before.batches(20, 10)[0].start, after.batches(20, 10)[0].start; g0 != 12*s || g1 != 10*s {
		t.Errorf("chunks 2–7 sent at %s before and %s after, want 12s and 10s", secs(g0), secs(g1))
	}
	if t0, t1 := was.elapsed-before.lastBatchEnd(), now.elapsed-after.lastBatchEnd(); t0 != 6*s || t1 != 4*s {
		t.Errorf("tail after the last batch answer: %s before and %s after, want 6s and 4s", secs(t0), secs(t1))
	}
	// The cache: the two warm-up batches and the two first retranslations
	// are the only requests sent before their prefix was warm. Before, the
	// retranslations of chunks 3 and 5 were sent together at 17 s with one
	// answered before them.
	if after.cold(true, 2) != 2 || after.cold(false, 2) != 2 {
		t.Errorf("after: %d batch and %d single requests sent before their prefix was warm, want 2 and 2",
			after.cold(true, 2), after.cold(false, 2))
	}
	if after.cold(false, 2) > before.cold(false, 2) || after.cold(true, 2) > before.cold(true, 2) {
		t.Errorf("more requests miss the cache than before: %d + %d, before %d + %d",
			after.cold(true, 2), after.cold(false, 2), before.cold(true, 2), before.cold(false, 2))
	}
	if after.peak > 8 || before.peak > 8 {
		t.Errorf("%d (before %d) requests in flight with 8 at a time", after.peak, before.peak)
	}
}

// The run never has more request sequences in flight than its workers,
// whatever is retranslated or split, and uses the workers that are free at
// the end. Three at a time, six chunks of 10.
func TestTranslateBatchTimingSlots(t *testing.T) {
	const s = time.Second
	// Sentence 3 (chunk 0) and 50–52 (chunk 5): the three retranslations of
	// chunk 5 go together at 15 s, on the slots of the workers that are done.
	tl := &timeline{bad: badSet(3, 50, 51, 52)}
	res := tl.run(t, timingRun{n: 60, batch: 10, conc: 3})
	res.allTranslated(t)
	for idx := 50; idx <= 52; idx++ {
		if got := tl.singles(idx)[0].start; got != 15*s {
			t.Errorf("sentence %d retranslated at %s, want 15s", idx, secs(got))
		}
	}
	if res.elapsed != 17*s || tl.peak != 3 {
		t.Errorf("the run took %s with at most %d in flight, want 17s and 3", secs(res.elapsed), tl.peak)
	}
	old := &timeline{bad: badSet(3, 50, 51, 52)}
	was := old.run(t, timingRun{n: 60, batch: 10, conc: 3, off: true})
	was.allTranslated(t)
	if was.elapsed != 23*s || old.peak != 3 {
		t.Errorf("before: the run took %s with at most %d in flight, want 23s and 3", secs(was.elapsed), old.peak)
	}

	// Retranslations in every chunk and a split, with every worker busy:
	// never more than the workers in flight, one or two workers included
	// (one worker: no warm-up, everything in turn).
	for _, conc := range []int{1, 2, 3} {
		tl := &timeline{bad: badSet(3, 4, 10, 11, 12, 25, 26, 27, 30, 41, 42, 50, 51, 52, 59),
			raw: func(c timedCall) (bool, string, error) {
				if c.batch && len(c.idxs) == 10 && c.idxs[0] == 20 {
					return true, `{"items": []}`, nil
				}
				return false, "", nil
			}}
		res := tl.run(t, timingRun{n: 60, batch: 10, conc: conc})
		res.allTranslated(t)
		if tl.peak > conc || conc > 1 && tl.peak < conc {
			t.Errorf("%d at a time: at most %d requests in flight", conc, tl.peak)
		}
		if b, sg := tl.count(); b != 9 || sg != 15 {
			t.Errorf("%d at a time: %d batch and %d single requests, want 9 and 15", conc, b, sg)
		}
	}
}

// A request that fails for good stops the run at once: the requests in
// flight are cancelled, and nothing is sent after it — not the other parts
// of its chunk, not the retranslations other chunks would have needed.
func TestTranslateBatchTimingFatalErrorSendsNothingMore(t *testing.T) {
	const s = time.Second
	boom := errors.New("boom")
	check := func(t *testing.T, tl *timeline, res timingResult, at time.Duration, inflight int) {
		t.Helper()
		if !errors.Is(res.err, boom) {
			t.Fatalf("err %v, want boom", res.err)
		}
		if res.elapsed != at {
			t.Errorf("the run returned at %s, want %s", secs(res.elapsed), secs(at))
		}
		cancelled := 0
		for _, c := range tl.all() {
			if c.start >= at {
				t.Errorf("a request for %v was sent at %s, after the failure at %s", c.idxs, secs(c.start), secs(at))
			}
			if c.end > at || c.end < 0 {
				t.Errorf("the request for %v ended at %s", c.idxs, secs(c.end))
			}
			if c.end == at {
				cancelled++
			}
		}
		if cancelled != inflight+1 {
			t.Errorf("%d requests ended at the failure, want %d in flight and the failing one", cancelled, inflight)
		}
	}

	// A retranslation fails. Chunk 2 (5–10 s) has three: 20 and 22 would
	// take until 12 s, 21 fails at 11 s. Chunk 1 is slow (5–17 s) and would
	// retranslate sentence 13 after it; chunks 3–5 are slow too (5–25 s).
	t.Run("retranslation", func(t *testing.T) {
		tl := &timeline{bad: badSet(3, 13, 20, 21, 22),
			delay: func(c timedCall) time.Duration {
				switch {
				case c.batch && c.idxs[0] == 10:
					return 12 * s
				case c.batch && c.idxs[0] >= 30:
					return 20 * s
				case !c.batch && c.idxs[0] == 21:
					return 1 * s
				}
				return 0
			},
			raw: func(c timedCall) (bool, string, error) {
				if !c.batch && c.idxs[0] == 21 {
					return true, "", boom
				}
				return false, "", nil
			}}
		res := tl.run(t, timingRun{n: 60, batch: 10, conc: 8})
		// in flight at 11 s: chunks 1, 3, 4, 5 and the retranslations of 20, 22
		check(t, tl, res, 11*s, 6)
		for idx := 20; idx <= 22; idx++ {
			if cs := tl.singles(idx); len(cs) != 1 || cs[0].start != 10*s {
				t.Errorf("sentence %d: retranslations at %s, want one at 10s", idx, secs(starts(cs)...))
			}
		}
		if len(tl.singles(13)) != 0 {
			t.Error("sentence 13 was retranslated after the failure")
		}
		for i, sen := range res.sentences {
			// written: chunk 0 (sentence 3 after its retranslation) and what
			// passed QA of chunk 2
			want := model.ZhNone
			if i < 10 || i >= 23 && i < 30 {
				want = model.ZhOK
			}
			if sen.ZhStatus != want {
				t.Errorf("sentence %d is %s, want %s", i, sen.ZhStatus, want)
			}
		}
	})

	// One half of a split fails while the other is in flight. Chunk 2 is
	// unusable twice (5–10–15 s); of its halves, sent at 15 s, 25–29 fails
	// at 16 s and 20–24 would take until 20 s. Chunks 3–5 are slow.
	t.Run("split half", func(t *testing.T) {
		tl := &timeline{
			delay: func(c timedCall) time.Duration {
				switch {
				case c.batch && c.idxs[0] >= 30:
					return 30 * s
				case c.batch && len(c.idxs) == 5 && c.idxs[0] == 25:
					return 1 * s
				}
				return 0
			},
			raw: func(c timedCall) (bool, string, error) {
				switch {
				case c.batch && len(c.idxs) == 10 && c.idxs[0] == 20:
					return true, `{"items": []}`, nil
				case c.batch && len(c.idxs) == 5 && c.idxs[0] == 25:
					return true, "", boom
				}
				return false, "", nil
			}}
		res := tl.run(t, timingRun{n: 60, batch: 10, conc: 8})
		// in flight at 16 s: chunks 3, 4, 5 and the half 20–24
		check(t, tl, res, 16*s, 4)
		if lo := tl.batches(20, 5); len(lo) != 1 || lo[0].start != 15*s || lo[0].end != 16*s {
			t.Errorf("half 20–24: %+v, want sent at 15s and cancelled at 16s", lo)
		}
		for i, sen := range res.sentences {
			if want := i < 20; (sen.ZhStatus == model.ZhOK) != want {
				t.Errorf("sentence %d is %s", i, sen.ZhStatus)
			}
		}
	})

	// The first retranslation of the run (the warm-up of its prefix) fails
	// while others wait for it: none of them is sent. Chunk 0 has none;
	// chunks 1–5 are answered together at 10 s and each has one.
	t.Run("warm-up retranslation", func(t *testing.T) {
		tl := &timeline{bad: badSet(10, 20, 30, 40, 50),
			raw: func(c timedCall) (bool, string, error) {
				if !c.batch {
					return true, "", boom
				}
				return false, "", nil
			}}
		res := tl.run(t, timingRun{n: 60, batch: 10, conc: 8})
		check(t, tl, res, 12*s, 0)
		if _, sg := tl.count(); sg != 1 {
			t.Errorf("%d retranslations sent, want the first one only", sg)
		}
	})
}

// A cancel takes effect at once, whatever is in flight or waiting: the run
// returns when it comes and sends nothing after it. Sentences 20–22 are
// being retranslated (10–12 s) and chunks 1, 3, 4, 5 are in flight when the
// cancel comes at 11 s; a second case cancels while retranslations wait for
// the warm-up of their prefix.
func TestTranslateBatchTimingCancel(t *testing.T) {
	const s = time.Second
	slow := func(c timedCall) time.Duration {
		if c.batch && (c.idxs[0] == 10 || c.idxs[0] >= 30) {
			return 20 * s
		}
		return 0
	}
	for _, c := range []struct {
		name     string
		tl       *timeline
		inflight int
		written  func(i int) bool
	}{
		{"during retranslations", &timeline{bad: badSet(3, 13, 20, 21, 22), delay: slow}, 7,
			func(i int) bool { return i < 10 || i >= 23 && i < 30 }},
		// chunks 1–5 answered at 10 s, one retranslation each: one is the
		// warm-up (10–12 s), four wait for it
		{"while waiting for the warm-up", &timeline{bad: badSet(10, 20, 30, 40, 50)}, 1,
			func(i int) bool { return i%10 != 0 || i == 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := c.tl.run(t, timingRun{n: 60, batch: 10, conc: 8, cancelAt: 11 * s})
			if !errors.Is(res.err, context.Canceled) {
				t.Fatalf("err %v, want the cancel", res.err)
			}
			if res.elapsed != 11*s {
				t.Errorf("the run returned at %s, want at the cancel, 11s", secs(res.elapsed))
			}
			cancelled := 0
			for _, call := range c.tl.all() {
				if call.start >= 11*s || call.end > 11*s || call.end < 0 {
					t.Errorf("request for %v: %s–%s", call.idxs, secs(call.start), secs(call.end))
				}
				if call.end == 11*s {
					cancelled++
				}
			}
			if cancelled != c.inflight {
				t.Errorf("%d requests cancelled, want %d", cancelled, c.inflight)
			}
			for i, sen := range res.sentences {
				if want := c.written(i); (sen.ZhStatus == model.ZhOK) != want {
					t.Errorf("sentence %d is %s", i, sen.ZhStatus)
				}
			}
		})
	}
}

// The valve with chunks, halves and retranslations side by side. It counts
// the chunks of the plan only, each once, when its own tries ended, in the
// order these end: "three failed and none answered" is about the chunks of
// the plan that have ended their tries so far, whatever their place in the
// lecture. Halves and single sentences never count, so sending them side by
// side does not change the count.
//
// What can differ from before is one request: a chunk looks at the valve
// when it starts, and the second half of a split now starts with the first.
// When the valve closes while the first half is in flight, the second half
// has already been sent as a batch (before, it started later, saw the valve
// and went sentence by sentence). The sentences end the same.
func TestTranslateBatchTimingValve(t *testing.T) {
	noBatch := func(c timedCall) (bool, string, error) {
		if c.batch {
			return true, `{"items": []}`, nil
		}
		return false, "", nil
	}
	noBatchAfterFirst := func(c timedCall) (bool, string, error) {
		if c.batch && c.idxs[0] != 0 {
			return true, `{"items": []}`, nil
		}
		return false, "", nil
	}
	// Short answers: a warm-up chunk that goes all the way down ends before
	// the warm-up's cap (15 s), as it would on a provider that answers.
	fast := func(c timedCall) time.Duration {
		if c.batch {
			return 500 * time.Millisecond
		}
		return 100 * time.Millisecond
	}
	// chunks 2–7 answer a little later than chunk 1: the order in which the
	// chunks fail is fixed
	stagger := func(c timedCall) time.Duration {
		if c.batch && len(c.idxs) == 10 && c.idxs[0] >= 20 {
			return 600 * time.Millisecond
		}
		return fast(c)
	}
	for _, c := range []struct {
		name          string
		raw           func(c timedCall) (bool, string, error)
		delay         func(c timedCall) time.Duration
		deepSeek      bool
		before, after int // batch requests
		single        int
		valve         int // "batching switched off" lines
	}{
		// Chunk 0 alone: 2 tries, then 5+5, 2+3+2+3, 2+2 → 10 batch requests
		// and its 10 sentences alone. Then the other 7 together, 2 tries
		// each: chunk 1 is the second to fail and is split with the valve
		// open; a moment later chunk 2 is the third and closes it, so the
		// sentences of chunks 2–7 go alone, and so do those of chunk 1's
		// halves once these failed. Before, chunk 1 had sent its first half
		// only by then; now both.
		{"no batch answer", noBatch, stagger, false, 10 + 7*2 + 1, 10 + 7*2 + 2, 80, 1},
		// DeepSeek: chunk 1 goes alone too, all the way down (10 requests);
		// the first of the other 6 to fail closes the valve before it is
		// split.
		{"no batch answer, DeepSeek", noBatch, fast, true, 10 + 10 + 6*2, 10 + 10 + 6*2, 80, 1},
		// Chunk 0 answered: the valve stays open whatever fails later, and
		// every other chunk goes all the way down.
		{"first chunk answered", noBatchAfterFirst, fast, false, 1 + 7*10, 1 + 7*10, 70, 0},
		{"first chunk answered, DeepSeek", noBatchAfterFirst, fast, true, 1 + 7*10, 1 + 7*10, 70, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, off := range []bool{true, false} {
				tl := &timeline{raw: c.raw, delay: c.delay}
				res := tl.run(t, timingRun{n: 80, batch: 10, conc: 8, deepSeek: c.deepSeek, off: off, log: true})
				res.allTranslated(t)
				want := c.after
				if off {
					want = c.before
				}
				if b, sg := tl.count(); b != want || sg != c.single {
					t.Errorf("off %v: %d batch and %d single requests, want %d and %d", off, b, sg, want, c.single)
				}
				if n := strings.Count(res.log, "batching switched off"); n != c.valve {
					t.Errorf("off %v: batching switched off %d times, want %d", off, n, c.valve)
				}
				if tl.peak > 8 {
					t.Errorf("off %v: %d requests in flight with 8 at a time", off, tl.peak)
				}
			}
		})
	}
}

// ---------- the same results as one request after the other ----------

// mixedScript answers by the content of a request alone, with everything
// the ladder knows: a chunk that is unusable down to halves, sentences that
// fail QA once or twice, a refused sentence.
func mixedScript() *scriptLLM {
	refused := sched.Permanent(model.Errorf(llm.CodeRefusal, map[string]any{"role": config.RoleTranslate, "category": "test"},
		fmt.Errorf("LLM 拒绝了这次请求：%w", llm.ErrRefusal)))
	return batchScript(func(idx int, batch bool) string {
		switch {
		case idx == 33 && batch, idx == 5 && batch, idx == 6 && batch, idx == 61 && batch, idx == 62 && batch:
			return "" // fails QA in the batch, fine alone
		case idx == 34, idx == 63:
			return "" // fails QA both times: left without a translation
		case idx == 47:
			return "现在我们来看列表。" // its number is missing both times: kept, flagged
		}
		return cacheZh(idx, batch)
	}, func(_ context.Context, _ int, c llmCall) (bool, string, error) {
		if !llmtest.IsBatchSystem(c.System) {
			if llmtest.TargetIdx(c.User) == 50 {
				return true, "", refused
			}
			return false, "", nil
		}
		idxs := llmtest.TargetIdxs(c.User)
		switch {
		case slices.Contains(idxs, 20) && len(idxs) >= 4:
			return true, "no JSON here", nil // 20–29 twice, 20–24; 20–21 and 22–24 answer
		case slices.Contains(idxs, 50):
			return true, "", refused // down to sentence 50
		}
		return false, "", nil
	})
}

type mixedResult struct {
	sentences []string // one line per sentence: status, zh, qa_error, hash
	requests  []string // every request, in the order received
	stats     model.JobStats
}

func runMixed(t *testing.T, conc int, deepSeek, off bool) mixedResult {
	t.Helper()
	old := batchTimingOff
	batchTimingOff = off
	defer func() { batchTimingOff = old }()
	e, l := cacheLecture(t, 70, 10, deepSeek)
	fake := mixedScript()
	d := e.deps(fake)
	d.Concurrency = conc
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil {
		t.Fatalf("%d at a time, off %v: %v", conc, off, r.err)
	}
	var out mixedResult
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	for _, s := range e.sentences(l.ID) {
		out.sentences = append(out.sentences, fmt.Sprintf("%d|%s|%s|%s|%s", s.Idx, s.ZhStatus, str(s.Zh), str(s.QAError), str(s.ZhHash)))
	}
	for _, c := range fake.Calls() {
		out.requests = append(out.requests, c.System+"\x00"+c.User)
	}
	out.stats = r.stats
	return out
}

// The same scripted answers give the same sentences, and the same requests,
// whether the requests of a chunk go one after the other (before T-PERF-04)
// or side by side, with one worker or many, on DeepSeek or elsewhere. With
// one worker even the order of the requests is the old one.
func TestTranslateBatchTimingSameResultsAsBefore(t *testing.T) {
	ref := runMixed(t, 1, false, true)
	// what the script leads to: 34, 63 and 50 without a translation, 47 flagged
	if ref.stats.PendingSentences != 3 {
		t.Fatalf("reference run: %d pending sentences, want 3", ref.stats.PendingSentences)
	}
	flagged := 0
	for _, s := range ref.sentences {
		if strings.Contains(s, "|"+model.ZhQAFailed+"|") {
			flagged++
		}
	}
	if flagged != 4 || len(ref.sentences) != 70 {
		t.Fatalf("reference run: %d of %d sentences flagged, want 4 of 70", flagged, len(ref.sentences))
	}
	sorted := func(in []string) []string {
		out := slices.Clone(in)
		sort.Strings(out)
		return out
	}
	for _, c := range []struct {
		conc          int
		deepSeek, off bool
		rounds        int
	}{
		{1, false, false, 1},
		{1, true, false, 1},
		{3, false, false, 3},
		{8, false, false, 5},
		{8, true, false, 5},
		{8, true, true, 1},
	} {
		for round := range c.rounds {
			got := runMixed(t, c.conc, c.deepSeek, c.off)
			name := fmt.Sprintf("%d at a time, DeepSeek %v, off %v, round %d", c.conc, c.deepSeek, c.off, round)
			if !slices.Equal(got.sentences, ref.sentences) {
				for i := range ref.sentences {
					if i >= len(got.sentences) || got.sentences[i] != ref.sentences[i] {
						t.Fatalf("%s: sentence %d differs:\n%s\nwant\n%s", name, i, got.sentences[i], ref.sentences[i])
					}
				}
			}
			if got.stats.PendingSentences != ref.stats.PendingSentences || got.stats.Requests != ref.stats.Requests {
				t.Errorf("%s: %d pending, %d requests; want %d and %d", name, got.stats.PendingSentences, got.stats.Requests,
					ref.stats.PendingSentences, ref.stats.Requests)
			}
			if !slices.Equal(sorted(got.requests), sorted(ref.requests)) {
				t.Errorf("%s: %d requests, not the %d of the reference run", name, len(got.requests), len(ref.requests))
			}
			if c.conc == 1 && !slices.Equal(got.requests, ref.requests) {
				t.Errorf("%s: the requests were sent in another order", name)
			}
		}
	}
}
