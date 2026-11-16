// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// Batch translation (v3.2.x P2, Q14a; .agent-log/plan-batch-translate.md).
//
// A translate job whose snapshot says batch > 1 sends n consecutive
// sentences per request (ctxbuild.BuildTranslateBatchRequest) and takes the
// answer {"items": [{"idx", "zh"}]} all or nothing. The fallback ladder of
// one chunk (batchRun.chunk):
//
//  1. the whole chunk, up to 1+batchRetries tries; the retry carries what
//     was wrong (askJSONTries feedback). A split half gets one try;
//  2. still unusable, truncated (llm.ErrTruncated: the client's truncation
//     ladder is done) or declined (llm.ErrRefusal) → split in two halves,
//     each again through 1–2;
//  3. a single sentence goes through translateOne, unchanged: its business
//     retries, its truncation and refusal handling, its QA retranslation.
//
// Transport errors, permanent errors other than those two, and a cancel are
// returned at once, as on the single path. QA stays per sentence: a batch
// sentence that fails it is retranslated alone (retryAfterQA). The
// sentences of a batch that pass QA are written in one transaction.
//
// Batch 1, snapshots without batch and incremental runs never come here
// (effectiveBatch): they take the single path, byte for byte as before.
//
// When the requests of a chunk are sent (v3.2.x r2 T-PERF-04; acceptance T4
// spent 5–8 of 22.8 s waiting where nothing had to wait). The requests
// themselves, their number and the conditions of the ladder are untouched.
//
//   - The run has as many slots as the provider's max_concurrency (workers).
//     A chunk of the plan takes one for as long as it runs; the two halves
//     of a split, and the QA retranslations of one chunk, run side by side
//     as far as the run has free slots (fan) and otherwise one after the
//     other on the chunk's own slot, in the order of before. So at most
//     workers request sequences of the run are in flight, as before; with
//     one worker, or every worker busy, the order of sending is the old one.
//   - A warm-up chunk ends its warm-up step when its batch request was
//     answered usably (warmed), not when its retranslations and writes are
//     done. A warm-up chunk without a usable answer ends it as before.
//   - The QA retranslations are single-sentence requests: their prefix is
//     the single path's system prompt, which no batch request warms. The
//     run's first one goes alone, on DeepSeek the second one too, and the
//     others wait for these (lateWarm), as the batches wait for the first
//     chunks. Before, retranslations of one chunk went one after the other
//     and so warmed that prefix by the way (T4: 0, 2688, then 4480 cached
//     tokens); sent together they would each pay for it.
//
// What one sentence ends with depends on the answers to its own requests
// only (requests are built from the run's snapshot, never from another
// sentence's translation), so the order of sending does not change results.

// batchTimingOff switches all of that off (tests: the sending order before
// T-PERF-04, for the comparisons in translate_batch_timing_test.go and
// perf_llm_test.go).
var batchTimingOff = false

// batchRetries is the number of extra tries of a whole chunk with an
// unusable answer before it is split (split halves get none).
const batchRetries = 1

// batchValveFails: after this many first-level chunks failed and none
// succeeded, the run's remaining sentences go one by one (the model does not
// answer in the batch format; plan §3.2 item 4).
const batchValveFails = 3

// translateBatchPromptVersion is the translate prompt version of a batch
// run (ctxbuild.TranslateBatchSystemTemplate; plan "要用户决定 #1" b). Only
// logged: recording it in the job's snapshot (state.RecordPromptVersion)
// would change the rerun comparison, which is outside the stage.
const translateBatchPromptVersion = 3

// effectiveBatch is the number of sentences per request of a translate
// run: the snapshot's batch clamped to [1, settings.MaxBatch]; 1 without
// one (a snapshot written before the option) and for an incremental run
// (the few sentences a review touched go one by one, as before).
func effectiveBatch(job model.Job, snap model.LLMSnapshot) int {
	if state.IsIncremental(job.Stats.Note) || snap.Params.Batch == nil {
		return 1
	}
	return min(max(*snap.Params.Batch, 1), settings.MaxBatch)
}

// chunkTodo cuts the sentences of a run (ascending indexes) into chunks of
// at most n consecutive sentences: every run of consecutive indexes of
// length L > n is split into ceil(L/n) chunks whose sizes differ by at most
// one; sentences that are not consecutive are never put in one chunk.
func chunkTodo(todo []int, n int) [][]int {
	n = max(n, 1)
	var out [][]int
	for start := 0; start < len(todo); {
		end := start + 1
		for end < len(todo) && todo[end] == todo[end-1]+1 {
			end++
		}
		run := todo[start:end]
		k := (len(run) + n - 1) / n
		size, extra := len(run)/k, len(run)%k
		for p := 0; p < len(run); {
			sz := size
			if extra > 0 {
				sz++
				extra--
			}
			out = append(out, run[p:p+sz])
			p += sz
		}
		start = end
	}
	return out
}

// translateBatchItem is one item of a batch answer.
type translateBatchItem struct {
	Idx *int    `json:"idx"`
	Zh  *string `json:"zh"`
}

// translateBatchAnswer is a batch answer: {"items": […]}, or the items
// array alone (models often answer with the array itself).
type translateBatchAnswer struct {
	Items    []translateBatchItem
	hasItems bool
	bare     bool // the items array alone (counted in the batch summary)
}

func (a *translateBatchAnswer) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	*a = translateBatchAnswer{}
	if len(t) > 0 && t[0] == '[' {
		a.hasItems, a.bare = true, true
		return json.Unmarshal(t, &a.Items)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(t, &obj); err != nil {
		return err
	}
	raw, ok := obj["items"]
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	a.hasItems = true
	return json.Unmarshal(raw, &a.Items)
}

// batchProblems is the check error of an unusable batch answer: what was
// wrong, for the retry feedback (retryProblems), and a kind for the log.
type batchProblems struct {
	kind string // no_items | no_field | wrong_ids | extra | duplicate | missing | n_mismatch
	list []string
}

func (e *batchProblems) Error() string      { return "batch answer: " + strings.Join(e.list, " ") }
func (e *batchProblems) Problems() []string { return e.list }

// idList renders "#3, #17".
func idList(ids []int) string { return pendingIDs(ids, ", ") }

// checkBatch accepts an answer with exactly one item for every idx of want,
// in any order. Anything else — no items, an item without idx or zh, an idx
// that is not a target, an idx twice, a target without an item — rejects
// the whole answer (all or nothing).
func checkBatch(want []int) func(a *translateBatchAnswer) error {
	return func(a *translateBatchAnswer) error {
		if !a.hasItems {
			return &batchProblems{kind: "no_items", list: []string{`The answer has no "items" array.`}}
		}
		isTarget := make(map[int]bool, len(want))
		for _, idx := range want {
			isTarget[idx] = true
		}
		var (
			kinds               []string
			list                []string
			extra, dup, missing []int
			seen                = map[int]int{}
			noField             bool
		)
		for _, it := range a.Items {
			if it.Idx == nil || it.Zh == nil {
				noField = true
				continue
			}
			seen[*it.Idx]++
			switch {
			case !isTarget[*it.Idx]:
				if seen[*it.Idx] == 1 { // once in the feedback, however often it came (review D17)
					extra = append(extra, *it.Idx)
				}
			case seen[*it.Idx] == 2:
				dup = append(dup, *it.Idx)
			}
		}
		for _, idx := range want {
			if seen[idx] == 0 {
				missing = append(missing, idx)
			}
		}
		if noField {
			kinds = append(kinds, "no_field")
			list = append(list, `An item has no "idx" or no "zh".`)
		}
		switch len(extra) {
		case 0:
		case 1:
			list = append(list, fmt.Sprintf("Item #%d is not a TARGET sentence of this request.", extra[0]))
		default:
			list = append(list, fmt.Sprintf("Items %s are not TARGET sentences of this request.", idList(extra)))
		}
		switch len(dup) {
		case 0:
		case 1:
			list = append(list, fmt.Sprintf("Item #%d appears more than once.", dup[0]))
		default:
			list = append(list, fmt.Sprintf("Items %s appear more than once.", idList(dup)))
		}
		if len(missing) > 0 {
			list = append(list, fmt.Sprintf("No item for TARGET %s. Every TARGET sentence needs exactly one item.", idList(missing)))
		}
		switch {
		case len(extra) > 0 && len(missing) > 0:
			kinds = append(kinds, "wrong_ids")
		case len(extra) > 0:
			kinds = append(kinds, "extra")
		}
		if len(dup) > 0 {
			kinds = append(kinds, "duplicate")
		}
		if len(missing) > 0 && len(extra) == 0 {
			kinds = append(kinds, "missing")
		}
		if len(list) == 0 && len(a.Items) != len(want) { // cannot happen once the rest holds; kept as a guard
			kinds = append(kinds, "n_mismatch")
			list = append(list, fmt.Sprintf("Expected %d items, got %d.", len(want), len(a.Items)))
		}
		if len(list) == 0 {
			return nil
		}
		return &batchProblems{kind: kinds[0], list: list}
	}
}

// batchProblemKind names what was wrong with an unusable batch answer, for
// the service log (never the answer itself: it is lecture content).
func batchProblemKind(err error) string {
	var bp *batchProblems
	if errors.As(err, &bp) {
		return bp.kind
	}
	return unusableKind(err)
}

// answerLog is an llm.Client that keeps the answers of one chunk's requests
// (the requests of a chunk are sent one after the other), so that the log
// can say why a retried chunk was retried.
type answerLog struct {
	llm.Client
	mu    sync.Mutex
	texts []string
}

func (c *answerLog) Complete(ctx context.Context, role, system, user string, jsonMode bool) (llm.Result, error) {
	res, err := c.Client.Complete(ctx, role, system, user, jsonMode)
	if err == nil {
		c.mu.Lock()
		c.texts = append(c.texts, res.Text)
		c.mu.Unlock()
	}
	return res, err
}

// batchRun is one translate run in batches.
type batchRun struct {
	s         *translateStage
	lec       ctxbuild.LectureSnapshot
	x         int
	lectureID string
	// record writes one sentence (pending handling included); recordBatch
	// writes sentences that passed QA in one transaction.
	record      func(idx int, tr translation) error
	recordBatch func(idxs []int, trs []translation) error
	// deepSeek: the run's provider is DeepSeek's API; its warm-up has a
	// second step (translateBatchDeepSeekWarmup).
	deepSeek bool

	// slots holds one token per request sequence of the run in flight; its
	// capacity is the run's workers. Set once by run, before any chunk.
	slots chan struct{}
	// redoWarm is the warm-up of the QA retranslations' prefix. Set once by
	// run, before any chunk; it guards its own state.
	redoWarm *lateWarm

	wholeOK, wholeFail atomic.Int64 // first-level chunks answered / failed (the valve)
	off                atomic.Bool  // the valve: the rest of the run goes one by one
	// for the summary line
	nOK, nRetried, nSplit, nSingles, nQARetries atomic.Int64
	// accepted answers that were the items array alone / out of order
	nBare, nShuffled atomic.Int64
}

// run translates todo in chunks of n with workers request sequences in
// flight (the first chunk's batch request alone first, as the single path
// warms the prompt cache; on DeepSeek the second chunk's alone next,
// translateBatchDeepSeekWarmup).
func (b *batchRun) run(ctx context.Context, todo []int, n, workers int) error {
	chunks := chunkTodo(todo, n)
	w := translateWarmup
	if b.deepSeek {
		w = translateBatchDeepSeekWarmup
	}
	steps := warmSteps(len(chunks), workers, w)
	slog.Info("translate: batch plan", "lecture", b.lectureID, "sentences", len(todo), "chunks", len(chunks), "size", n,
		"concurrency", workers, "prompt_version", translateBatchPromptVersion, "warmup_steps", steps)
	b.slots = make(chan struct{}, max(workers, 1))
	if batchTimingOff {
		steps = 0
	}
	b.redoWarm = newLateWarm(steps, w.max)
	defer b.redoWarm.stop()
	err := parallelWarm(ctx, len(chunks), workers, w, func(ctx context.Context, k int) (bool, error) {
		// After the warm-up gate: a chunk waiting there holds no slot.
		if err := b.acquire(ctx); err != nil {
			return true, err
		}
		defer b.release()
		c := chunks[k]
		if err := b.chunk(ctx, c[0], len(c), 0); err != nil {
			return true, err
		}
		return false, nil
	})
	slog.Info("translate: batch summary", "lecture", b.lectureID, "batches_ok", b.nOK.Load(), "batches_retried", b.nRetried.Load(),
		"batches_split", b.nSplit.Load(), "singles_after_split", b.nSingles.Load(), "qa_single_retries", b.nQARetries.Load(),
		"bare_array", b.nBare.Load(), "shuffled", b.nShuffled.Load(), "switched_off", b.off.Load())
	return err
}

// chunk translates the n consecutive sentences [first, first+n); level is
// the number of splits above it (0: a chunk of the plan).
func (b *batchRun) chunk(ctx context.Context, first, n, level int) error {
	if n == 1 || b.off.Load() {
		return b.singles(ctx, first, n, level)
	}
	req := ctxbuild.BuildTranslateBatchRequest(b.lec, first, n, b.x)
	tries := 1
	if level == 0 {
		tries += batchRetries
	}
	logc := &answerLog{Client: b.s.d.LLM}
	// A batch request that runs out of its attempt timeout is not sent again
	// by the client (review R14): it is split below (batchTimedOut).
	bctx := llm.WithNoDeadlineRetry(llm.WithJSONSchema(ctx, ctxbuild.SchemaTranslateBatch))
	ans, last, err := askJSONTries(bctx, logc, config.RoleTranslate,
		req.System, req.User, tries, checkBatch(req.Idxs))
	attrs := []any{"lecture", b.lectureID, "first_idx", req.Idxs[0], "n", n, "level", level}
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		switch {
		case errors.Is(err, llm.ErrTruncated):
			// Asking again for the same chunk is cut off the same way.
			slog.Warn("translate: batch truncated, split", attrs...)
			b.failed(level, req.Idxs[0], n)
		case errors.Is(err, llm.ErrRefusal):
			// Split to find the sentence it was about; that one ends
			// pending (translateOne), the others are translated. A refused
			// chunk counts for the valve too (review R11 B2): when the topic
			// itself is refused, splitting costs about three times the
			// single path, so after batchValveFails such chunks (and none
			// answered) the rest goes one by one.
			slog.Warn("translate: batch refused, split", append(attrs, "category", refusalCategory(err))...)
			b.failed(level, req.Idxs[0], n)
		case batchTimedOut(err):
			// Review R11 B1, R14: the request ran out of its attempt timeout
			// (llm.CodeTimeout, sent once: WithNoDeadlineRetry above; a
			// connect timeout with the client's retries used up is not
			// split since review D8, it goes to the default case), or its
			// stream was still coming at that timeout (llm.CodeTimeLimit):
			// the answer is too long for one attempt, like a truncated one.
			// Smaller halves. One sentence alone goes to translateOne, which
			// is not marked: the client retries its timeouts as on the single
			// path, and a timeout there fails the stage as before. Its
			// attempt timeout, like the halves', is still batchTimeout of
			// the configured batch (the request's target carries it), not
			// the single path's: longer, never shorter.
			slog.Warn("translate: batch timed out, split", attrs...)
			b.failed(level, req.Idxs[0], n)
		default:
			// network errors, 5xx, 429 with its retries used up, permanent
			// errors (401, 402, ...): as on the single path, no split
			return err
		}
		return b.split(ctx, first, n, level)
	}
	if last != nil {
		slog.Warn("translate: batch answer unusable", append(attrs, "problem", batchProblemKind(last), "action", "split")...)
		b.failed(level, req.Idxs[0], n)
		return b.split(ctx, first, n, level)
	}
	if len(logc.texts) > 1 {
		// Answered on the retry: why the first answer was rejected.
		var a translateBatchAnswer
		kind := ""
		if perr := llm.ParseJSON(logc.texts[0], &a); perr != nil {
			kind = unusableKind(perr)
		} else if cerr := checkBatch(req.Idxs)(&a); cerr != nil {
			kind = batchProblemKind(cerr)
		}
		slog.Warn("translate: batch answer unusable", append(attrs, "problem", kind, "action", "retry")...)
		b.nRetried.Add(1)
	} else {
		b.nOK.Add(1)
	}
	if level == 0 {
		b.wholeOK.Add(1)
		if !batchTimingOff {
			// The batch request of a chunk of the plan was answered: if this
			// is a warm-up chunk, the chunks waiting for it go now, not after
			// the retranslations and writes below.
			warmed(ctx)
		}
	}

	zhOf := make(map[int]string, n)
	shuffled := false
	for k, it := range ans.Items {
		zhOf[*it.Idx] = strings.TrimSpace(*it.Zh)
		shuffled = shuffled || *it.Idx != req.Idxs[k]
	}
	if shuffled {
		slog.Info("translate: batch order shuffled", attrs...)
		b.nShuffled.Add(1)
	}
	if ans.bare {
		b.nBare.Add(1)
	}
	type redo struct {
		i          int
		zh, reason string
	}
	var (
		okIdx []int
		okTr  []translation
		redos []redo
	)
	for k, sidx := range req.Idxs {
		i := first + k
		zh := zhOf[sidx]
		if ok, reason := qa.Check(b.lec.Sentences[i].Text, zh, b.lec.Terms); ok {
			okIdx = append(okIdx, i)
			okTr = append(okTr, translation{zh: zh, status: model.ZhOK, hash: req.Hashes[k]})
		} else {
			redos = append(redos, redo{i: i, zh: zh, reason: reason})
		}
	}
	if len(okIdx) > 0 {
		if err := b.recordBatch(okIdx, okTr); err != nil {
			return err
		}
	}
	// A sentence that failed QA is retranslated alone (SPEC §7.4) and
	// written when that is done: written before, its current hash would
	// keep a resumed run from retranslating it.
	ask := func(ctx context.Context, r redo) (translation, error) {
		b.nQARetries.Add(1)
		return b.s.retryAfterQA(ctx, b.lec, r.i, ctxbuild.BuildTranslateRequest(b.lec, r.i, b.x), r.zh, r.reason)
	}
	// While the retranslations' prefix is not warm, they go one at a time
	// (redoWarm): this chunk's first ones, in order, if they are the run's.
	for len(redos) > 0 {
		lead, err := b.redoWarm.enter(ctx)
		if err != nil {
			return err
		}
		if !lead {
			break
		}
		tr, err := ask(ctx, redos[0])
		b.redoWarm.leave(err == nil) // answered: before the write
		if err != nil {
			return err
		}
		if err := b.record(redos[0].i, tr); err != nil {
			return err
		}
		redos = redos[1:]
	}
	// The others side by side, as far as the run has free slots.
	return b.fan(ctx, len(redos), func(ctx context.Context, k int) error {
		tr, err := ask(ctx, redos[k])
		if err != nil {
			return err
		}
		return b.record(redos[k].i, tr)
	})
}

// acquire takes a slot of the run for a chunk of the plan, waiting for one
// when retranslations or halves of other chunks hold them all. It returns
// ctx.Err() when the run was stopped or cancelled, and then holds no slot:
// the chunk must not run.
func (b *batchRun) acquire(ctx context.Context) error {
	select {
	case b.slots <- struct{}{}:
		if err := ctx.Err(); err != nil { // both were ready
			b.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *batchRun) release() { <-b.slots }

// fan runs fn(ctx, 0..n-1), the parts of one chunk (the halves of a split,
// its QA retranslations), for a caller that holds a slot. The caller runs
// the parts one after the other on its own slot, in order; beside it, up
// to n-1 helpers each wait for a free slot of the run and, once they have
// one, take the parts not started yet. So the parts run side by side as far
// as the run's slots allow at any moment, and with no free slot (one
// worker; every worker busy) that is fn(0), fn(1), … in turn: the order
// before T-PERF-04. A helper that waits holds nothing, and the caller never
// waits for a slot, so no chunk waits for a slot held by a chunk that waits
// for it.
//
// The first error stops the others as parallel does: no further part is
// started, the parts in flight are cancelled through ctx, and the error is
// returned once they have ended. Nor is a part started once the run was
// stopped or cancelled; fan then returns ctx's error.
func (b *batchRun) fan(ctx context.Context, n int, fn func(ctx context.Context, k int) error) error {
	switch n {
	case 0:
		return nil
	case 1:
		return fn(ctx, 0)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex // guards next and first
		next  int        // the first part not started yet
		first error
		wg    sync.WaitGroup
	)
	// work runs parts not started yet, one after the other, until none is
	// left or the chunk is stopping.
	work := func() {
		for {
			mu.Lock()
			if first == nil && next < n {
				first = ctx.Err() // stopped or cancelled: nothing new
			}
			if first != nil || next >= n {
				mu.Unlock()
				return
			}
			k := next
			next++
			mu.Unlock()
			if err := fn(ctx, k); err != nil {
				mu.Lock()
				if first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
				return
			}
		}
	}
	started := make(chan struct{}) // closed when no part is left to start
	if !batchTimingOff {
		for range n - 1 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case b.slots <- struct{}{}:
					defer b.release()
					work()
				case <-started:
				}
			}()
		}
	}
	work()
	close(started)
	wg.Wait()
	return first // every writer has ended
}

// batchTimedOut reports whether err is the LLM client's timeout of one
// request (llm.CodeTimeout) or the time limit of a stream still coming
// (llm.CodeTimeLimit, review R14). Other transport errors are not, nor is
// the idle timeout of a stream (llm.CodeIdleTimeout): nothing arrived for
// a while, the connection or a gateway is at fault, not the batch's size;
// nor is a timeout connecting to the server (llm.ErrConnectTimeout, review
// D8): the request never got there.
func batchTimedOut(err error) bool {
	var ce *model.CodedError
	return errors.As(err, &ce) && (ce.Info.Code == llm.CodeTimeout || ce.Info.Code == llm.CodeTimeLimit) &&
		!errors.Is(err, llm.ErrConnectTimeout)
}

// failed counts a failed first-level chunk and switches batching off for
// the rest of the run once batchValveFails failed and none succeeded.
//
// With chunks in flight side by side, "failed" and "succeeded" are counted
// when a chunk of the plan has ended its own tries, in the order these end
// (not the order of the chunks in the lecture); the halves of a split and
// single sentences never count, whether they run in turn or side by side
// (T-PERF-04). Once off, the valve stays off. A chunk looks at it when it
// starts: one already sent is not taken back, so the second half of a
// split, which now starts with the first, may still go out as a batch when
// the valve closes a moment later (before, it started after the first half
// and went sentence by sentence). The sentences end the same either way.
func (b *batchRun) failed(level, firstIdx, n int) {
	if level != 0 {
		return
	}
	if b.wholeFail.Add(1) >= batchValveFails && b.wholeOK.Load() == 0 && b.off.CompareAndSwap(false, true) {
		slog.Warn("translate: batching switched off for this run", "lecture", b.lectureID, "first_idx", firstIdx, "n", n,
			"fails", batchValveFails)
	}
}

// split translates the two halves of [first, first+n): side by side when
// the run has a free slot, else one after the other (fan).
func (b *batchRun) split(ctx context.Context, first, n, level int) error {
	b.nSplit.Add(1)
	mid := n / 2
	return b.fan(ctx, 2, func(ctx context.Context, k int) error {
		if k == 0 {
			return b.chunk(ctx, first, mid, level+1)
		}
		return b.chunk(ctx, first+mid, n-mid, level+1)
	})
}

// singles translates [first, first+n) one sentence per request, on the
// single path (translateOne).
func (b *batchRun) singles(ctx context.Context, first, n, level int) error {
	for i := first; i < first+n; i++ {
		tr, err := b.s.translateOne(ctx, b.lec, i, b.x)
		if err != nil {
			return err
		}
		if level > 0 || n > 1 {
			b.nSingles.Add(1)
		}
		if err := b.record(i, tr); err != nil {
			return err
		}
	}
	return nil
}
