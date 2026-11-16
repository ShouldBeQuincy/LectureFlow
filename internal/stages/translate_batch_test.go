// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// v3.2.x P2 (Q14a): batch translation, its fallback ladder and the single
// path it leaves untouched for batch 1.

// setTranslateBatch sets params.batch of the lecture's translate snapshot.
func (e *env) setTranslateBatch(lectureID string, n int) {
	e.t.Helper()
	j := e.job(model.ScopeLecture, lectureID, model.StageTranslate)
	snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot)
	if !ok {
		e.t.Fatalf("no translate snapshot: %q", j.ConfigSnapshot)
	}
	snap.Params.Batch = &n
	e.setSnapshot(lectureID, model.StageTranslate, model.MarshalSnapshot(snap))
}

// sampleLecture is a launched lecture with the 14 sample sentences and a summary.
func (e *env) sampleLecture() model.Lecture {
	e.t.Helper()
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "S")
	return l
}

// overridePlainZh makes the fake LLM answer plainZh (answers that pass QA).
func (e *env) overridePlainZh() {
	for i, zh := range plainZh {
		e.fake.SetTranslation(i, zh)
	}
}

// batchScript answers translate requests: a batch request with one item per
// TARGET (zh(idx, true)), a single one with zhJSON(idx, zh(idx, false)).
// raw, when it handles request n, gives its answer instead.
func batchScript(zh func(idx int, batch bool) string, raw func(ctx context.Context, n int, c llmCall) (handled bool, text string, err error)) *scriptLLM {
	return &scriptLLM{fn: func(ctx context.Context, n int, c llmCall) (string, error) {
		if raw != nil {
			if ok, text, err := raw(ctx, n, c); ok {
				return text, err
			}
		}
		if llmtest.IsBatchSystem(c.System) {
			idxs := llmtest.TargetIdxs(c.User)
			items := make([]map[string]any, len(idxs))
			for k, i := range idxs {
				items[k] = map[string]any{"idx": i, "zh": zh(i, true)}
			}
			b, _ := json.Marshal(map[string]any{"items": items})
			return string(b), nil
		}
		i := llmtest.TargetIdx(c.User)
		return zhJSON(i, zh(i, false)), nil
	}}
}

func plainAnswer(idx int, _ bool) string { return plainZh[idx] }

// split returns the batch and the single-sentence requests of s.
func splitCalls(s *scriptLLM) (batch, single []llmCall) {
	for _, c := range s.Calls() {
		if llmtest.IsBatchSystem(c.System) {
			batch = append(batch, c)
		} else {
			single = append(single, c)
		}
	}
	return
}

func batchTargets(reqs []llmtest.Request) [][]int {
	out := make([][]int, len(reqs))
	for i, q := range reqs {
		out[i] = q.TargetIdxs
	}
	return out
}

func idxRange(lo, hi int) []int { // [lo, hi]
	var out []int
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

// captureLog sends the service log to a buffer for the test.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// ---------- unit tests ----------

func TestEffectiveBatch(t *testing.T) {
	if settings.MaxBatch != 20 {
		t.Fatalf("settings.MaxBatch %d: update the plan's assumptions", settings.MaxBatch)
	}
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		batch *int
		note  string
		want  int
	}{
		{nil, "", 1}, {n(0), "", 1}, {n(-3), "", 1}, {n(1), "", 1}, {n(10), "", 10}, {n(20), "", 20}, {n(99), "", 20},
		{n(10), state.NoteIncremental, 1}, {n(10), state.NoteIncremental + ":abc", 1},
	} {
		j := model.Job{Stats: model.JobStats{Note: c.note}}
		if got := effectiveBatch(j, model.LLMSnapshot{Params: model.RoleParams{Batch: c.batch}}); got != c.want {
			t.Errorf("batch %v note %q: %d, want %d", c.batch, c.note, got, c.want)
		}
	}
}

func TestChunkTodo(t *testing.T) {
	for _, c := range []struct {
		todo []int
		n    int
		want [][]int
	}{
		{idxRange(0, 13), 5, [][]int{idxRange(0, 4), idxRange(5, 9), idxRange(10, 13)}},
		{idxRange(0, 10), 10, [][]int{idxRange(0, 5), idxRange(6, 10)}}, // 11 / 10 → 6 + 5, no one-sentence tail
		{idxRange(0, 9), 10, [][]int{idxRange(0, 9)}},
		{[]int{5, 6, 8, 9, 10}, 10, [][]int{{5, 6}, {8, 9, 10}}}, // gaps are never bridged
		{[]int{3, 7, 8}, 4, [][]int{{3}, {7, 8}}},
		{idxRange(0, 13), 1, func() [][]int {
			var o [][]int
			for i := range 14 {
				o = append(o, []int{i})
			}
			return o
		}()},
		{nil, 4, nil},
	} {
		if got := chunkTodo(c.todo, c.n); !slices.EqualFunc(got, c.want, slices.Equal) {
			t.Errorf("chunkTodo(%v, %d) = %v, want %v", c.todo, c.n, got, c.want)
		}
	}
}

func TestCheckBatch(t *testing.T) {
	want := []int{14, 15, 16, 17}
	check := checkBatch(want)
	parse := func(s string) *translateBatchAnswer {
		var a translateBatchAnswer
		if err := llm.ParseJSON(s, &a); err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return &a
	}
	for _, c := range []struct {
		answer, kind string
		problems     []string
	}{
		{`{"items":[{"idx":14,"zh":"a"},{"idx":15,"zh":"b"},{"idx":16,"zh":"c"},{"idx":17,"zh":"d"}]}`, "", nil},
		{`{"items":[{"idx":17,"zh":"d"},{"idx":15,"zh":"b"},{"idx":16,"zh":"c"},{"idx":14,"zh":"a"}]}`, "", nil}, // order is free
		{`[{"idx":14,"zh":"a"},{"idx":15,"zh":"b"},{"idx":16,"zh":"c"},{"idx":17,"zh":"d"}]`, "", nil},           // bare array
		{`{"items":[{"idx":14,"zh":""},{"idx":15,"zh":"b"},{"idx":16,"zh":"c"},{"idx":17,"zh":"d"}]}`, "", nil},  // empty zh: QA's business
		{`{"answer":[]}`, "no_items", []string{`The answer has no "items" array.`}},
		{`{"items":null}`, "no_items", []string{`The answer has no "items" array.`}},
		{`{"items":[{"idx":14,"zh":"a"},{"idx":16,"zh":"c"}]}`, "missing",
			[]string{"No item for TARGET #15, #17. Every TARGET sentence needs exactly one item."}},
		{`{"items":[{"idx":14,"zh":"a"},{"idx":15,"zh":"b"},{"idx":16,"zh":"c"},{"idx":17,"zh":"d"},{"idx":20,"zh":"e"}]}`, "extra",
			[]string{"Item #20 is not a TARGET sentence of this request."}},
		// review D17: an extra idx named once, however often it came
		{`{"items":[{"idx":14,"zh":"a"},{"idx":15,"zh":"b"},{"idx":16,"zh":"c"},{"idx":17,"zh":"d"},{"idx":20,"zh":"e"},{"idx":20,"zh":"f"}]}`, "extra",
			[]string{"Item #20 is not a TARGET sentence of this request."}},
		{`{"items":[{"idx":14,"zh":"a"},{"idx":20,"zh":"e"},{"idx":15,"zh":"b"},{"idx":21,"zh":"g"},{"idx":16,"zh":"c"},{"idx":20,"zh":"f"},{"idx":17,"zh":"d"}]}`, "extra",
			[]string{"Items #20, #21 are not TARGET sentences of this request."}},
		{`{"items":[{"idx":14,"zh":"a"},{"idx":15,"zh":"b"},{"idx":15,"zh":"b"},{"idx":16,"zh":"c"},{"idx":17,"zh":"d"}]}`, "duplicate",
			[]string{"Item #15 appears more than once."}},
		{`{"items":[{"idx":1014,"zh":"a"},{"idx":1015,"zh":"b"},{"idx":1016,"zh":"c"},{"idx":1017,"zh":"d"}]}`, "wrong_ids",
			[]string{"Items #1014, #1015, #1016, #1017 are not TARGET sentences of this request.",
				"No item for TARGET #14, #15, #16, #17. Every TARGET sentence needs exactly one item."}},
		{`{"items":[{"idx":14},{"idx":15,"zh":"b"},{"idx":16,"zh":"c"},{"idx":17,"zh":"d"}]}`, "no_field",
			[]string{`An item has no "idx" or no "zh".`, "No item for TARGET #14. Every TARGET sentence needs exactly one item."}},
	} {
		err := check(parse(c.answer))
		if c.kind == "" {
			if err != nil {
				t.Errorf("%s: %v", c.answer, err)
			}
			continue
		}
		var bp *batchProblems
		if !errors.As(err, &bp) || bp.kind != c.kind || !slices.Equal(bp.Problems(), c.problems) {
			t.Errorf("%s: %#v, want %s %q", c.answer, err, c.kind, c.problems)
		}
	}
	// Feedback holds idx numbers and fixed English only, never the answer.
	if err := check(parse(`{"items":[{"idx":14,"zh":"SECRET"},{"idx":99,"zh":"SECRET"}]}`)); err == nil ||
		strings.Contains(strings.Join(err.(*batchProblems).Problems(), " "), "SECRET") {
		t.Errorf("feedback %v", err)
	}
}

// ---------- the single path is untouched for batch 1 ----------

// Batch absent, 1, 0 (clamped) and an incremental run with batch 10 send
// exactly the single-sentence requests of before: system, user, schema,
// max_tokens and thinking of every request equal, one request per
// sentence, each equal to ctxbuild.BuildTranslateRequest (whose output the
// existing goldens pin).
func TestTranslateBatchOneIsTheSinglePath(t *testing.T) {
	type req struct {
		System, User, Schema, Thinking, Model string
		MaxTokens                             int
		Temperature                           float64
	}
	run := func(name string, setup func(e *env, l model.Lecture)) map[int]req {
		e := newEnv(t)
		l := e.sampleLecture()
		d := e.claudeDeps(l.ID, "claude-sonnet-5-5")
		if setup != nil {
			setup(e, l)
		}
		r := e.run(e.stage(d, model.StageTranslate), l.ID)
		if r.err != nil {
			t.Fatalf("%s: %v", name, r.err)
		}
		reqs := e.fake.Requests(llmtest.RoleTranslate)
		if len(reqs) != 14 || len(e.fake.BatchRequests()) != 0 || r.stats.Requests != 14 {
			t.Fatalf("%s: %d requests, %d batch, stats %d", name, len(reqs), len(e.fake.BatchRequests()), r.stats.Requests)
		}
		ld, err := e.deps(nil).loadLecture(e.ctx, l.ID)
		e.must(err)
		x := model.SnapshotWindow(e.job(model.ScopeLecture, l.ID, model.StageTranslate).ConfigSnapshot)
		out := map[int]req{}
		for _, q := range reqs {
			if q.Batch || len(q.TargetIdxs) != 1 {
				t.Fatalf("%s: batch request %v", name, q.TargetIdxs)
			}
			want := ctxbuild.BuildTranslateRequest(ld.snap, q.TargetIdx, x)
			if q.System != want.System || q.User != want.User {
				t.Errorf("%s: request #%d differs from BuildTranslateRequest", name, q.TargetIdx)
			}
			out[q.TargetIdx] = req{q.System, q.User, q.Schema, q.Thinking, q.Model, q.MaxTokens, q.Temperature}
		}
		return out
	}
	base := run("no batch", nil)
	if len(base) != 14 {
		t.Fatalf("%d sentences", len(base))
	}
	for _, c := range []struct {
		name  string
		setup func(e *env, l model.Lecture)
	}{
		{"batch 1", func(e *env, l model.Lecture) { e.setTranslateBatch(l.ID, 1) }},
		{"batch 0", func(e *env, l model.Lecture) { e.setTranslateBatch(l.ID, 0) }},
		{"incremental, batch 10", func(e *env, l model.Lecture) {
			e.setTranslateBatch(l.ID, 10)
			newPendingTranslate(t, e, l.ID, state.NoteIncremental)
		}},
	} {
		if got := run(c.name, c.setup); !maps.Equal(got, base) {
			for i, q := range got {
				if q != base[i] {
					t.Errorf("%s: request #%d differs from the run without batch:\n%+v\n%+v", c.name, i, q, base[i])
					break
				}
			}
		}
	}
}

// ---------- the batch path ----------

// Batch 5 on 14 sentences: three requests (5, 5, 4), every sentence ok with
// the translation and zh_hash a single-sentence run gives; progress and
// events by sentence; usage by request. Changing the batch size afterwards
// makes nothing stale.
func TestTranslateBatchHappyPath(t *testing.T) {
	setup := func(e *env) model.Lecture {
		e.fixtureTerms()
		l := e.lecture("L1", "m.mp4")
		e.insertSample(l.ID)
		e.setSummary(l.ID, "A lecture about caches.")
		if r := e.run(e.stage(e.deps(nil), model.StageRepair), l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		return l
	}
	single := newEnv(t)
	ls := setup(single)
	if r := single.run(single.stage(single.deps(nil), model.StageTranslate), ls.ID); r.err != nil {
		t.Fatal(r.err)
	}

	e := newEnv(t)
	l := setup(e)
	e.setTranslateBatch(l.ID, 5)
	d := e.deps(nil)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := batchTargets(e.fake.BatchRequests()); !slices.EqualFunc(got, [][]int{idxRange(0, 4), idxRange(5, 9), idxRange(10, 13)}, slices.Equal) {
		t.Errorf("batches %v", got)
	}
	if n := e.fake.Count(llmtest.RoleTranslate); n != 3 || r.stats.Requests != 3 {
		t.Errorf("%d requests, stats %d", n, r.stats.Requests)
	}
	for _, q := range e.fake.BatchRequests() {
		if !q.JSONMode || q.Temperature != 0.3 {
			t.Errorf("batch request parameters %+v", q)
		}
	}
	want := single.sentences(ls.ID)
	hashes := e.currentHashes(l.ID)
	for i, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhOK || s.Zh == nil || *s.Zh != *want[i].Zh || s.QAError != nil {
			t.Errorf("sentence %d: %s %v %v", i, s.ZhStatus, s.Zh, s.QAError)
		}
		if s.ZhHash == nil || *s.ZhHash != hashes[i] || *s.ZhHash != *want[i].ZhHash {
			t.Errorf("sentence %d: zh_hash differs from the single run", i)
		}
	}
	vals := r.progress.vals
	if !slices.IsSorted(vals) || r.progress.last() != 1 || !slices.Contains(vals, 5.0/14) {
		t.Errorf("progress %v", vals)
	}
	if !e.ev.has("sentences", l.ID) {
		t.Error("no sentences event")
	}

	// A new run with another batch size has nothing to translate.
	e.fake.Reset()
	newPendingTranslate(t, e, l.ID, "")
	e.setTranslateBatch(l.ID, 7)
	if r := e.run(e.stage(d, model.StageTranslate), l.ID); r.err != nil || e.fake.Count(llmtest.RoleTranslate) != 0 {
		t.Errorf("rerun with batch 7: %v, %d requests", r.err, e.fake.Count(llmtest.RoleTranslate))
	}
}

// batchFixture is a sample lecture on the fake server with batch 4 (chunks
// 0–3, 4–7, 8–10, 11–13), one chunk in flight, answering plainZh.
func batchFixture(t *testing.T) (*env, model.Lecture, Deps) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.overridePlainZh()
	e.setTranslateBatch(l.ID, 4)
	d := e.deps(nil)
	d.Concurrency = 1
	return e, l, d
}

func (e *env) allOK(lectureID string, except ...int) {
	e.t.Helper()
	h := e.currentHashes(lectureID)
	for i, s := range e.sentences(lectureID) {
		if slices.Contains(except, i) {
			continue
		}
		if s.ZhStatus != model.ZhOK || s.Zh == nil || *s.Zh != plainZh[i] || s.ZhHash == nil || *s.ZhHash != h[i] {
			e.t.Errorf("sentence %d: %s %v", i, s.ZhStatus, s.Zh)
		}
	}
}

var plainChunks = [][]int{idxRange(0, 3), idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}

// Fallbacks of one chunk on the fake server: retry with feedback, split,
// accepted variants. Every case ends with all 14 sentences translated.
func TestTranslateBatchFallbacks(t *testing.T) {
	missing3 := "No item for TARGET #3. Every TARGET sentence needs exactly one item."
	for _, c := range []struct {
		name     string
		fail     llmtest.Failure
		times    int
		batches  [][]int
		feedback []string // the problems the second request carries (nil = no check)
	}{
		{"missing once: whole-chunk retry", llmtest.FailBatchMissing, 1, append([][]int{idxRange(0, 3)}, plainChunks...), []string{missing3}},
		{"missing twice: split", llmtest.FailBatchMissing, 2,
			[][]int{idxRange(0, 3), idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}, []string{missing3}},
		{"extra twice: split", llmtest.FailBatchExtra, 2,
			[][]int{idxRange(0, 3), idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)},
			[]string{"Item #1003 is not a TARGET sentence of this request."}},
		{"duplicate twice: split", llmtest.FailBatchDuplicate, 2,
			[][]int{idxRange(0, 3), idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)},
			[]string{"Item #0 appears more than once."}},
		{"wrong ids twice: split", llmtest.FailBatchWrongIds, 2,
			[][]int{idxRange(0, 3), idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)},
			[]string{"Items #1000, #1001, #1002, #1003 are not TARGET sentences of this request.",
				"No item for TARGET #0, #1, #2, #3. Every TARGET sentence needs exactly one item."}},
		{"not JSON twice: split", llmtest.FailNotJSON, 2,
			[][]int{idxRange(0, 3), idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}, nil},
		{"second JSON once: retry", llmtest.FailSecondJSON, 1, append([][]int{idxRange(0, 3)}, plainChunks...), []string{ctxbuild.SecondJSONProblem}},
		{"truncated: split at once", llmtest.FailTruncated, 1,
			[][]int{idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}, nil},
		{"shuffled: accepted", llmtest.FailBatchShuffled, 1, plainChunks, nil},
		{"bare array: accepted", llmtest.FailBatchBareArray, 1, plainChunks, nil},
		{"trailing text: accepted", llmtest.FailTrailingText, 1, plainChunks, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, l, d := batchFixture(t)
			log := captureLog(t)
			e.fake.FailNextBatch(c.times, c.fail)
			r := e.run(e.stage(d, model.StageTranslate), l.ID)
			if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 0 {
				t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
			}
			reqs := e.fake.BatchRequests()
			if got := batchTargets(reqs); !slices.EqualFunc(got, c.batches, slices.Equal) {
				t.Errorf("batches %v, want %v", got, c.batches)
			}
			if n := e.fake.Count(llmtest.RoleTranslate); n != len(c.batches) {
				t.Errorf("%d translate requests, want %d batch requests only", n, len(c.batches))
			}
			if c.feedback != nil {
				if want := ctxbuild.WithRetryFeedback(reqs[0].User, c.feedback); reqs[1].User != want {
					t.Errorf("retry user:\n%s\nwant\n%s", reqs[1].User, want)
				}
			}
			for _, q := range reqs[1:] {
				if q.System != reqs[0].System {
					t.Error("a fallback request changed the system prompt")
				}
			}
			e.allOK(l.ID)
			out := log.String()
			for _, zh := range plainZh {
				if strings.Contains(out, zh) {
					t.Fatalf("the service log holds a translation:\n%s", out)
				}
			}
			if len(c.batches) > 4 && !strings.Contains(out, "first_idx=0 n=4 level=0") {
				t.Errorf("no fallback log line:\n%s", out)
			}
			// the batch summary counts the accepted variants (review R11)
			bare, shuffled := 0, 0
			switch c.fail {
			case llmtest.FailBatchBareArray:
				bare = 1
			case llmtest.FailBatchShuffled:
				shuffled = 1
			}
			if want := fmt.Sprintf("bare_array=%d shuffled=%d", bare, shuffled); !strings.Contains(out, want) {
				t.Errorf("batch summary without %q:\n%s", want, out)
			}
		})
	}
}

// A chunk whose halves fail too ends on the single path: sentence 0 gets
// three unusable answers there and is pending; the others are translated
// and the stage finishes with the warning.
func TestTranslateBatchDownToSinglePending(t *testing.T) {
	e, l, d := batchFixture(t)
	e.fake.FailNextBatch(4, llmtest.FailNotJSON) // 0–3 twice, 0–1, then 2–3
	e.fake.FailNext(llmtest.RoleTranslate, 3, llmtest.FailNotJSON)
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 1 || infoCode(r.stats.WarningInfo) != WarnTranslatePending {
		t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
	}
	if got := batchTargets(e.fake.BatchRequests()); !slices.EqualFunc(got,
		[][]int{idxRange(0, 3), idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}, slices.Equal) {
		t.Errorf("batches %v", got)
	}
	var singles []int
	for _, q := range e.fake.Requests(llmtest.RoleTranslate) {
		if !q.Batch {
			singles = append(singles, q.TargetIdx)
		}
	}
	if !slices.Equal(singles, []int{0, 0, 0, 1, 2, 3}) {
		t.Errorf("single requests %v", singles)
	}
	p := e.sentences(l.ID)[0]
	if !isPending(p) || p.QAError == nil || qa.ErrorInfo(*p.QAError).Code != qa.CodeSentenceUnusable {
		t.Errorf("sentence 0: %+v", p)
	}
	e.allOK(l.ID, 0)
}

// No sentence gets a usable answer: the valve switches batching off after
// three failed chunks, every sentence ends unusable on the single path, and
// the stage fails as before with the sentences back to stale.
func TestTranslateBatchEverySentenceUnusableFailsStage(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "no JSON here", nil }}
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if codeOf(r.err) != CodeTranslateFailed || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhStale {
			t.Errorf("sentence %d is %s, want stale", s.Idx, s.ZhStatus)
		}
	}
	// chunk 0–3: 2 + (1 + 3 + 3) + (1 + 3 + 3); 4–7 the same; 8–10: 2, then
	// the valve: 3 × 3 singles; 11–13: 3 × 3 singles.
	batch, single := splitCalls(fake)
	if len(batch) != 2+1+1+2+1+1+2 || len(single) != 14*3 {
		t.Errorf("%d batch and %d single requests", len(batch), len(single))
	}
}

// The valve: three first-level chunks failed and none succeeded → the rest
// of the run goes one by one, with one warning in the log.
func TestTranslateBatchValve(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	log := captureLog(t)
	fake := batchScript(plainAnswer, func(_ context.Context, _ int, c llmCall) (bool, string, error) {
		if llmtest.IsBatchSystem(c.System) {
			return true, `{"items": []}`, nil
		}
		return false, "", nil
	})
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.stats.PendingSentences != 0 {
		t.Fatalf("err %v stats %+v", r.err, r.stats)
	}
	e.allOK(l.ID)
	// 0–3: 2 + 1 + 1; 4–7: 2 + 1 + 1; 8–10: 2, then the valve; 11–13: none.
	batch, single := splitCalls(fake)
	if len(batch) != 10 || len(single) != 14 {
		t.Errorf("%d batch and %d single requests, want 10 and 14", len(batch), len(single))
	}
	out := log.String()
	if strings.Count(out, "batching switched off") != 1 || !strings.Contains(out, "problem=missing action=split") {
		t.Errorf("log:\n%s", out)
	}

	// One chunk answered before the failures: the valve stays open.
	e2 := newEnv(t)
	l2 := e2.sampleLecture()
	e2.setTranslateBatch(l2.ID, 2)
	fake2 := batchScript(plainAnswer, func(_ context.Context, n int, c llmCall) (bool, string, error) {
		if n > 0 && llmtest.IsBatchSystem(c.System) {
			return true, `{"items": []}`, nil
		}
		return false, "", nil
	})
	d2 := e2.deps(fake2)
	d2.Concurrency = 1
	if r := e2.run(e2.stage(d2, model.StageTranslate), l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if batch, _ := splitCalls(fake2); len(batch) != 1+6*2 {
		t.Errorf("%d batch requests, want 13 (every chunk tried twice)", len(batch))
	}
}

// Truncated again on the single path: the stage fails as it does today.
func TestTranslateBatchTruncatedSingleFails(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.overridePlainZh()
	e.setTranslateBatch(l.ID, 2)
	d := e.deps(nil)
	d.Concurrency = 1
	e.fake.FailNextBatch(1, llmtest.FailTruncated)
	e.fake.FailNext(llmtest.RoleTranslate, 1, llmtest.FailTruncated)
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if !errors.Is(r.err, llm.ErrTruncated) || !sched.IsPermanent(r.err) || jobErrorCode(r.job) != llm.CodeTruncated {
		t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
	}
	reqs := e.fake.Requests(llmtest.RoleTranslate)
	if len(reqs) != 2 || !reqs[0].Batch || reqs[1].Batch || reqs[1].TargetIdx != 0 {
		t.Errorf("requests %v", batchTargets(reqs))
	}
}

// A refused chunk is split until the refused sentence is alone: that one is
// pending (asked once alone), the rest of the lecture is translated.
func TestTranslateBatchRefusalIsolated(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	d := e.claudeDeps(l.ID, "claude-sonnet-5-5")
	e.overridePlainZh()
	e.setTranslateBatch(l.ID, 4)
	e.fake.FailNextBatch(2, llmtest.FailRefusal)                   // 0–3, then 0–1
	e.fake.FailNext(llmtest.RoleTranslate, 1, llmtest.FailRefusal) // 0 alone
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 1 {
		t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
	}
	if got := batchTargets(e.fake.BatchRequests()); !slices.EqualFunc(got,
		[][]int{idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}, slices.Equal) {
		t.Errorf("batches %v", got)
	}
	var singles []int
	for _, q := range e.fake.Requests(llmtest.RoleTranslate) {
		if !q.Batch {
			singles = append(singles, q.TargetIdx)
		} else if q.Schema == "" || !strings.Contains(q.Schema, `"items"`) {
			t.Errorf("batch request schema %q", q.Schema)
		}
	}
	if !slices.Equal(singles, []int{0, 1}) {
		t.Errorf("single requests %v", singles)
	}
	s := e.sentences(l.ID)[0]
	if !isPending(s) || s.QAError == nil || *s.QAError != qa.RefusedReason(llmtest.RefusalCategory) {
		t.Errorf("sentence 0: %+v", s)
	}
	e.allOK(l.ID, 0)
}

// The content filter of an OpenAI-compatible provider (finish_reason
// "content_filter", half an answer) is a refusal like Claude's: the chunk
// is split until the filtered sentence is alone, that one is pending
// (asked once alone, no category), the cut answer is never used (N50).
func TestTranslateBatchContentFilterIsolated(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.overridePlainZh()
	e.setTranslateBatch(l.ID, 4)
	d := e.deps(nil)
	d.Concurrency = 1
	e.fake.FailNextBatch(2, llmtest.FailRefusal)                   // 0–3, then 0–1
	e.fake.FailNext(llmtest.RoleTranslate, 1, llmtest.FailRefusal) // 0 alone
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 1 {
		t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
	}
	reqs := e.fake.Requests(llmtest.RoleTranslate)
	if len(reqs) == 0 || reqs[0].API != model.ProviderOpenAI {
		t.Fatalf("requests %+v", reqs)
	}
	if got := batchTargets(e.fake.BatchRequests()); !slices.EqualFunc(got,
		[][]int{idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}, slices.Equal) {
		t.Errorf("batches %v", got)
	}
	var singles []int
	for _, q := range reqs {
		if !q.Batch {
			singles = append(singles, q.TargetIdx)
		}
	}
	if !slices.Equal(singles, []int{0, 1}) {
		t.Errorf("single requests %v", singles)
	}
	s := e.sentences(l.ID)[0]
	if !isPending(s) || s.QAError == nil || *s.QAError != qa.RefusedReason("") {
		t.Errorf("sentence 0: %+v", s)
	}
	if info := qa.ErrorInfo(*s.QAError); info == nil || info.Code != qa.CodeSentenceRefused {
		t.Errorf("qa_error_info %+v", info)
	}
	if r.stats.PromptTokens == 0 {
		t.Errorf("stats %+v", r.stats)
	}
	e.allOK(l.ID, 0)
}

// Transport and permanent errors stop the run as on the single path: no
// split, no single request.
func TestTranslateBatchPermanentErrorStops(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) {
		return "", sched.Permanent(errors.New("HTTP 401，请检查 API key 或模型名"))
	}}
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if !sched.IsPermanent(r.err) {
		t.Fatalf("err %v", r.err)
	}
	if batch, single := splitCalls(fake); len(batch) != 1 || len(single) != 0 {
		t.Errorf("%d batch and %d single requests after a permanent error", len(batch), len(single))
	}
	for _, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhNone {
			t.Errorf("sentence %d is %s", s.Idx, s.ZhStatus)
		}
	}
}

// timeoutErr is the LLM client's final error of a request that timed out
// (llm.CodeTimeout, as finalError builds it).
func timeoutErr() error {
	return model.Errorf(llm.CodeTimeout, map[string]any{"role": config.RoleTranslate, "retries": 4, "seconds": 168.0},
		fmt.Errorf("LLM 请求超时（translate，单次请求超过 2m48s，已重试 4 次）：%w", context.DeadlineExceeded))
}

// Review R11 B1: a batch request that timed out is split like a truncated
// one; the halves are answered and the stage finishes without any single
// request.
func TestTranslateBatchTimeoutSplits(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	log := captureLog(t)
	fake := batchScript(plainAnswer, func(_ context.Context, n int, c llmCall) (bool, string, error) {
		if n == 0 { // chunk 0–3
			return true, "", timeoutErr()
		}
		return false, "", nil
	})
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 0 {
		t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
	}
	batch, single := splitCalls(fake)
	var got [][]int
	for _, c := range batch {
		got = append(got, llmtest.TargetIdxs(c.User))
	}
	if want := [][]int{idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}; !slices.EqualFunc(got, want, slices.Equal) || len(single) != 0 {
		t.Errorf("batches %v and %d single requests, want %v and none", got, len(single), want)
	}
	e.allOK(l.ID)
	if out := log.String(); !strings.Contains(out, "batch timed out, split") || !strings.Contains(out, "batches_split=1") {
		t.Errorf("log:\n%s", out)
	}
}

// Timed out down to one sentence: that one goes to translateOne, whose
// timeout fails the stage as on the single path (no pending, no further
// split).
func TestTranslateBatchTimeoutSingleFails(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", timeoutErr() }}
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if jobErrorCode(r.job) != llm.CodeTimeout || r.job.Status != model.StatusFailed {
		t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
	}
	batch, single := splitCalls(fake)
	if len(batch) != 2 || len(single) != 1 || llmtest.TargetIdx(single[0].User) != 0 {
		t.Errorf("%d batch and %d single requests, want 0–3, 0–1 and sentence 0 alone", len(batch), len(single))
	}
}

// shortLLMAttempts caps every LLM attempt at d for the test (the fake
// server answers in milliseconds; llmtest.FailHang runs into the cap).
func shortLLMAttempts(t *testing.T, d time.Duration) {
	t.Helper()
	prev := llm.MaxAttemptTimeout
	llm.MaxAttemptTimeout = d
	t.Cleanup(func() { llm.MaxAttemptTimeout = prev })
}

// Review R14: a batch request that runs out of its attempt timeout is sent
// once (the client does not retry it: llm.WithNoDeadlineRetry) and split;
// the halves are answered. Not streamed (OpenAI-compatible) and streamed
// (claude-sonnet-5-5, no response headers) alike.
func TestTranslateBatchDeadlineSentOnceThenSplit(t *testing.T) {
	shortLLMAttempts(t, 300*time.Millisecond)
	for _, streamed := range []bool{false, true} {
		name := "not streamed"
		if streamed {
			name = "streamed"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			l := e.sampleLecture()
			e.overridePlainZh()
			var d Deps
			if streamed {
				d = e.claudeDeps(l.ID, "claude-sonnet-5-5")
				e.fake.StreamNext(llmtest.RoleTranslate, 1, llmtest.StreamHangHeaders) // chunk 0–3
			} else {
				d = e.deps(nil)
				d.Concurrency = 1
				e.fake.FailNextBatch(1, llmtest.FailHang) // chunk 0–3
			}
			e.setTranslateBatch(l.ID, 4)
			log := captureLog(t)
			r := e.run(e.stage(d, model.StageTranslate), l.ID)
			if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 0 {
				t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
			}
			want := [][]int{idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}
			if got := batchTargets(e.fake.Requests(llmtest.RoleTranslate)); !slices.EqualFunc(got, want, slices.Equal) {
				t.Errorf("requests %v, want %v (chunk 0–3 once)", got, want)
			}
			e.allOK(l.ID)
			if out := log.String(); !strings.Contains(out, "batch timed out, split") || !strings.Contains(out, "batches_split=1") {
				t.Errorf("log:\n%s", out)
			}
		})
	}
}

// Review R14: single-sentence requests are not marked: their attempt
// timeouts are retried as before (five attempts), in a batch run (sentence
// 0 after chunks 0–3 and 0–1 timed out) and with batch 1 (the single path).
func TestTranslateBatchSingleDeadlineRetried(t *testing.T) {
	shortLLMAttempts(t, 300*time.Millisecond)
	for _, tc := range []struct {
		name    string
		batch   int
		batches [][]int
	}{
		{"batch 4", 4, [][]int{idxRange(0, 3), {0, 1}}},
		{"batch 1", 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.sampleLecture()
			e.overridePlainZh()
			e.setTranslateBatch(l.ID, tc.batch)
			d := e.deps(nil)
			d.Concurrency = 1
			e.fake.FailNextBatch(len(tc.batches), llmtest.FailHang)
			e.fake.FailNext(llmtest.RoleTranslate, 5, llmtest.FailHang)
			r := e.run(e.stage(d, model.StageTranslate), l.ID)
			if jobErrorCode(r.job) != llm.CodeTimeout || r.job.Status != model.StatusFailed {
				t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
			}
			if info, _ := state.ParseJobError(*r.job.Error); !paramsMatch(info, "retries", 4) {
				t.Errorf("job error %v", *r.job.Error)
			}
			var batches [][]int
			var singles []int
			for _, q := range e.fake.Requests(llmtest.RoleTranslate) {
				if q.Batch {
					batches = append(batches, q.TargetIdxs)
				} else {
					singles = append(singles, q.TargetIdx)
				}
			}
			if !slices.EqualFunc(batches, tc.batches, slices.Equal) || !slices.Equal(singles, []int{0, 0, 0, 0, 0}) {
				t.Errorf("batches %v, singles %v; want %v and sentence 0 five times", batches, singles, tc.batches)
			}
		})
	}
}

// Review R14: a streamed batch answer still coming when its attempt
// timeout runs out (llm.CodeTimeLimit, permanent, sent once by the client)
// is split like a timeout; the idle timeout of a stream (llm.CodeIdleTimeout,
// the client's retries used up) is not: nothing arrived for a while, which
// a smaller batch does not fix. The stage fails as on the single path.
func TestTranslateBatchTimeLimitSplitsIdleDoesNot(t *testing.T) {
	timeLimit := sched.Permanent(model.Errorf(llm.CodeTimeLimit, map[string]any{"role": config.RoleTranslate, "retries": 0, "seconds": 240.0,
		"provider": "Claude", "model": "claude-sonnet-5-5"}, errors.New("the attempt timeout of 4m0s ran out while the answer was still streaming")))
	idle := model.Errorf(llm.CodeIdleTimeout, map[string]any{"role": config.RoleTranslate, "retries": 4, "seconds": 180.0},
		errors.New("LLM 请求空闲超时（translate，已重试 4 次）"))
	t.Run("time limit", func(t *testing.T) {
		e := newEnv(t)
		l := e.sampleLecture()
		e.setTranslateBatch(l.ID, 4)
		log := captureLog(t)
		fake := batchScript(plainAnswer, func(_ context.Context, n int, c llmCall) (bool, string, error) {
			if n == 0 { // chunk 0–3
				return true, "", timeLimit
			}
			return false, "", nil
		})
		d := e.deps(fake)
		d.Concurrency = 1
		r := e.run(e.stage(d, model.StageTranslate), l.ID)
		if r.err != nil || r.job.Status != model.StatusDone || r.stats.PendingSentences != 0 {
			t.Fatalf("err %v status %s stats %+v", r.err, r.job.Status, r.stats)
		}
		batch, single := splitCalls(fake)
		var got [][]int
		for _, c := range batch {
			got = append(got, llmtest.TargetIdxs(c.User))
		}
		if want := [][]int{idxRange(0, 3), {0, 1}, {2, 3}, idxRange(4, 7), idxRange(8, 10), idxRange(11, 13)}; !slices.EqualFunc(got, want, slices.Equal) || len(single) != 0 {
			t.Errorf("batches %v and %d single requests, want %v and none", got, len(single), want)
		}
		e.allOK(l.ID)
		if out := log.String(); !strings.Contains(out, "batch timed out, split") || !strings.Contains(out, "batches_split=1") {
			t.Errorf("log:\n%s", out)
		}
	})
	t.Run("idle timeout", func(t *testing.T) {
		e := newEnv(t)
		l := e.sampleLecture()
		e.setTranslateBatch(l.ID, 4)
		fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", idle }}
		d := e.deps(fake)
		d.Concurrency = 1
		r := e.run(e.stage(d, model.StageTranslate), l.ID)
		if jobErrorCode(r.job) != llm.CodeIdleTimeout || r.job.Status != model.StatusFailed {
			t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
		}
		if batch, single := splitCalls(fake); len(batch) != 1 || len(single) != 0 {
			t.Errorf("%d batch and %d single requests, want the one batch request", len(batch), len(single))
		}
		for _, s := range e.sentences(l.ID) {
			if s.ZhStatus != model.ZhNone {
				t.Errorf("sentence %d is %s", s.Idx, s.ZhStatus)
			}
		}
	})
}

// Transport errors other than the timeout are not split (review R11): the
// client's retries are used up, the stage fails as on the single path.
func TestTranslateBatchTransportErrorsNotSplit(t *testing.T) {
	t.Run("5xx on the fake server", func(t *testing.T) {
		e, l, d := batchFixture(t)
		// the client's 4 retries (1 ms backoff) and one more: 5 attempts
		e.fake.FailNextBatch(5, llmtest.FailHTTP500)
		r := e.run(e.stage(d, model.StageTranslate), l.ID)
		if jobErrorCode(r.job) != llm.CodeServerError || r.job.Status != model.StatusFailed {
			t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
		}
		reqs := e.fake.Requests(llmtest.RoleTranslate)
		if got := batchTargets(reqs); len(reqs) != 5 || !slices.EqualFunc(got, slices.Repeat([][]int{idxRange(0, 3)}, 5), slices.Equal) {
			t.Errorf("requests %v, want chunk 0–3 five times and nothing else", got)
		}
		for _, s := range e.sentences(l.ID) {
			if s.ZhStatus != model.ZhNone {
				t.Errorf("sentence %d is %s", s.Idx, s.ZhStatus)
			}
		}
	})
	t.Run("5xx recovered by the client's retry", func(t *testing.T) {
		e, l, d := batchFixture(t)
		e.fake.FailNextBatch(4, llmtest.FailHTTP500)
		r := e.run(e.stage(d, model.StageTranslate), l.ID)
		if r.err != nil || r.job.Status != model.StatusDone {
			t.Fatalf("err %v status %s", r.err, r.job.Status)
		}
		want := append(slices.Repeat([][]int{idxRange(0, 3)}, 4), plainChunks...)
		if got := batchTargets(e.fake.Requests(llmtest.RoleTranslate)); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("requests %v, want %v", got, want)
		}
		e.allOK(l.ID)
	})
	for _, c := range []struct {
		name string
		err  error
		code string
	}{
		{"network", model.Errorf(llm.CodeNetwork, map[string]any{"role": config.RoleTranslate, "retries": 4},
			errors.New("LLM 请求失败（translate，已重试 4 次）：网络不可用：dial tcp: connection refused")), llm.CodeNetwork},
		{"429 retries used up", model.Errorf(llm.CodeRateLimited, map[string]any{"role": config.RoleTranslate, "retries": 4},
			errors.New("LLM 请求被限流（translate，HTTP 429，已重试 4 次）")), llm.CodeRateLimited},
		{"5xx retries used up", model.Errorf(llm.CodeServerError, map[string]any{"role": config.RoleTranslate, "retries": 4, "status": 503},
			errors.New("LLM 服务端错误（translate，HTTP 503，已重试 4 次）")), llm.CodeServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.sampleLecture()
			e.setTranslateBatch(l.ID, 4)
			fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", c.err }}
			d := e.deps(fake)
			d.Concurrency = 1
			r := e.run(e.stage(d, model.StageTranslate), l.ID)
			if jobErrorCode(r.job) != c.code || r.job.Status != model.StatusFailed {
				t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
			}
			if batch, single := splitCalls(fake); len(batch) != 1 || len(single) != 0 {
				t.Errorf("%d batch and %d single requests, want the one batch request", len(batch), len(single))
			}
		})
	}
}

// Review R11 B2: refused chunks count for the valve. Every batch request is
// refused, every single one answered: after three refused first-level
// chunks the rest of the run goes one by one.
func TestTranslateBatchRefusalsOpenValve(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	log := captureLog(t)
	refused := sched.Permanent(model.Errorf(llm.CodeRefusal, map[string]any{"role": config.RoleTranslate, "category": "test"},
		fmt.Errorf("LLM 拒绝了这次请求：%w", llm.ErrRefusal)))
	fake := batchScript(plainAnswer, func(_ context.Context, _ int, c llmCall) (bool, string, error) {
		if llmtest.IsBatchSystem(c.System) {
			return true, "", refused
		}
		return false, "", nil
	})
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.stats.PendingSentences != 0 {
		t.Fatalf("err %v stats %+v", r.err, r.stats)
	}
	e.allOK(l.ID)
	// 0–3, 0–1, 2–3; 4–7, 4–5, 6–7; 8–10, then the valve: 8, 9, 10 and
	// 11–13 alone (without counting refusals: 9–10, 11–13 and 12–13 too).
	batch, single := splitCalls(fake)
	if len(batch) != 7 || len(single) != 14 {
		t.Errorf("%d batch and %d single requests, want 7 and 14", len(batch), len(single))
	}
	if out := log.String(); strings.Count(out, "batching switched off") != 1 {
		t.Errorf("log:\n%s", out)
	}
}

// QA stays per sentence: a batch sentence that fails QA is retranslated
// alone with the single request and the reason; the others are not asked
// again.
func TestTranslateBatchQARetry(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	fake := batchScript(func(idx int, batch bool) string {
		switch {
		case idx == 3 && batch:
			return "在大多数机器上，每个缓存行是六十四字节。" // numbers 64, 86 missing
		case idx == 2 && batch:
			return "" // empty
		case idx == 10:
			return "缺失率下降了。" // fails both times
		}
		return plainZh[idx]
	}, nil)
	d := e.deps(fake)
	d.Concurrency = 1
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil || r.stats.PendingSentences != 0 {
		t.Fatalf("err %v stats %+v", r.err, r.stats)
	}
	batch, single := splitCalls(fake)
	if len(batch) != 4 || len(single) != 3 {
		t.Fatalf("%d batch and %d single requests", len(batch), len(single))
	}
	ld, err := e.deps(nil).loadLecture(e.ctx, l.ID)
	e.must(err)
	x := e.cfg.LLM.Roles["translate"].Window
	reasons := map[int]string{
		2: "the translation is empty", 3: "the numbers 64, 86 from the source sentence are missing",
	}
	var got []int
	for _, c := range single {
		idx := llmtest.TargetIdx(c.User)
		got = append(got, idx)
		req := ctxbuild.BuildTranslateRequest(ld.snap, idx, x)
		if c.System != req.System || !strings.HasPrefix(c.User, req.User+"\n\nYour previous translation was rejected: ") {
			t.Errorf("QA retry of %d is not the single retry request:\n%s", idx, c.User)
		}
		if want, ok := reasons[idx]; ok && !strings.Contains(c.User, want) {
			t.Errorf("QA retry of %d without its reason %q:\n%s", idx, want, c.User)
		}
	}
	if !slices.Equal(got, []int{2, 3, 10}) {
		t.Errorf("QA retries for %v", got)
	}
	ss := e.sentences(l.ID)
	if ss[10].ZhStatus != model.ZhQAFailed || *ss[10].Zh != "缺失率下降了。" || ss[10].QAError == nil || !strings.Contains(*ss[10].QAError, "numbers 5, 3") {
		t.Errorf("sentence 10: %s %v %v", ss[10].ZhStatus, ss[10].Zh, ss[10].QAError)
	}
	e.allOK(l.ID, 10)
}

// A cancel while a batch is in flight keeps what was written and drops the
// chunk in flight; the resumed run asks only for the sentences not written.
func TestTranslateBatchCancelAndResume(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	first := batchScript(plainAnswer, func(cctx context.Context, n int, _ llmCall) (bool, string, error) {
		if n == 2 { // chunk 8–10
			cancel()
			<-cctx.Done()
			return true, "", cctx.Err()
		}
		return false, "", nil
	})
	d := e.deps(first)
	d.Concurrency = 1
	if r := e.runCtx(ctx, e.stage(d, model.StageTranslate), l.ID); !errors.Is(r.err, context.Canceled) || r.job.Status != model.StatusCancelled {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	for i, s := range e.sentences(l.ID) {
		if want := i < 8; (s.ZhStatus == model.ZhOK) != want {
			t.Errorf("sentence %d is %s after the cancel", i, s.ZhStatus)
		}
	}
	second := batchScript(plainAnswer, nil)
	if r := e.run(e.stage(e.deps(second), model.StageTranslate), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	var asked []int
	for _, c := range second.Calls() {
		asked = append(asked, llmtest.TargetIdxs(c.User)...)
	}
	slices.Sort(asked)
	if !slices.Equal(asked, idxRange(8, 13)) {
		t.Errorf("resumed run asked for %v, want 8–13", asked)
	}
	e.allOK(l.ID)
}

// A cancel during the QA retranslation of a batch sentence: the sentences
// of the batch that passed QA are written; the one being retranslated is
// not, and the resumed run translates it.
func TestTranslateBatchCancelDuringQARetry(t *testing.T) {
	e := newEnv(t)
	l := e.sampleLecture()
	e.setTranslateBatch(l.ID, 4)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	first := batchScript(func(idx int, batch bool) string {
		if idx == 3 {
			return "在大多数机器上，每个缓存行是六十四字节。"
		}
		return plainZh[idx]
	}, func(cctx context.Context, n int, _ llmCall) (bool, string, error) {
		if n == 1 { // the QA retry of sentence 3
			cancel()
			<-cctx.Done()
			return true, "", cctx.Err()
		}
		return false, "", nil
	})
	d := e.deps(first)
	d.Concurrency = 1
	if r := e.runCtx(ctx, e.stage(d, model.StageTranslate), l.ID); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err %v", r.err)
	}
	ss := e.sentences(l.ID)
	for i := range 3 {
		if ss[i].ZhStatus != model.ZhOK {
			t.Errorf("sentence %d is %s", i, ss[i].ZhStatus)
		}
	}
	if ss[3].ZhStatus != model.ZhNone {
		t.Errorf("sentence 3 is %s, want none", ss[3].ZhStatus)
	}
	second := batchScript(plainAnswer, nil)
	if r := e.run(e.stage(e.deps(second), model.StageTranslate), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	var asked []int
	for _, c := range second.Calls() {
		asked = append(asked, llmtest.TargetIdxs(c.User)...)
	}
	slices.Sort(asked)
	if !slices.Equal(asked, idxRange(3, 13)) {
		t.Errorf("resumed run asked for %v, want 3–13", asked)
	}
	e.allOK(l.ID)
}

// A lecture of 400 sentences in batches of 20 with four chunks in flight.
func TestTranslateBatchLargeLecture(t *testing.T) {
	const n = 400
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	ss := make([]model.Sentence, n)
	for i := range ss {
		text := fmt.Sprintf("Now we look at item %d of the list.", i)
		ss[i] = model.Sentence{LectureID: l.ID, Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i)*1000 + 900, ASRText: text,
			RepairedText: text, ZhStatus: model.ZhNone}
	}
	e.insertSentences(l.ID, ss)
	e.setSummary(l.ID, "S")
	e.setTranslateBatch(l.ID, 20)
	zh := func(idx int, _ bool) string { return fmt.Sprintf("现在我们来看列表中的第 %d 项。", idx) }
	fake := batchScript(zh, nil)
	d := e.deps(fake)
	d.Concurrency = 4
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	batch, single := splitCalls(fake)
	if len(batch) != n/20 || len(single) != 0 || r.stats.Requests != n/20 {
		t.Errorf("%d batch, %d single requests, stats %d", len(batch), len(single), r.stats.Requests)
	}
	hashes := e.currentHashes(l.ID)
	for i, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhOK || s.Zh == nil || *s.Zh != zh(i, true) || s.ZhHash == nil || *s.ZhHash != hashes[i] {
			t.Fatalf("sentence %d: %s %v", i, s.ZhStatus, s.Zh)
		}
	}
	if r.progress.last() != 1 {
		t.Errorf("progress %v", r.progress.last())
	}
}

// Batches on the fake server with four chunks in flight and rate-limit
// answers (HTTP 429) on the way: the client retries them as for any
// request, nothing is split, every sentence is translated.
func TestTranslateBatchConcurrentWith429(t *testing.T) {
	const n = 200
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	ss := make([]model.Sentence, n)
	for i := range ss {
		text := fmt.Sprintf("Now we look at item %d of the list.", i)
		ss[i] = model.Sentence{LectureID: l.ID, Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i)*1000 + 900, ASRText: text,
			RepairedText: text, ZhStatus: model.ZhNone}
		e.fake.SetTranslation(i, fmt.Sprintf("现在我们来看列表中的第 %d 项。", i))
	}
	e.insertSentences(l.ID, ss)
	e.setSummary(l.ID, "S")
	e.setTranslateBatch(l.ID, 10)
	e.fake.FailNextBatch(3, llmtest.FailHTTP429)
	d := e.deps(nil)
	d.Concurrency = 4
	r := e.run(e.stage(d, model.StageTranslate), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	reqs := e.fake.Requests(llmtest.RoleTranslate)
	if b := len(e.fake.BatchRequests()); b != len(reqs) || b != n/10+3 {
		t.Errorf("%d requests, %d batch, want %d batch requests only", len(reqs), b, n/10+3)
	}
	for i, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhOK || s.Zh == nil || *s.Zh != fmt.Sprintf("现在我们来看列表中的第 %d 项。", i) {
			t.Fatalf("sentence %d: %s %v", i, s.ZhStatus, s.Zh)
		}
	}
}
