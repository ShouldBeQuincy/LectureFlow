// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// IncrementalPrefixChanged is the error of an incremental translate run whose
// prefix (glossary / summary) changed after it was requested.
const IncrementalPrefixChanged = "审阅后术语表或摘要发生了变化，增量重翻已停止；请重跑翻译阶段以按新术语重新翻译整节课"

// translateStage implements SPEC §7: per-sentence translation with QA,
// written sentence by sentence.
type translateStage struct{ apiStage }

func (s *translateStage) Name() string { return model.StageTranslate }

// InputHash = SHA-256(translate prefix + x): a glossary or summary change
// makes the whole stage stale (SPEC §7.3 third rule); sentence-level changes
// are handled by zh_status (state's sentence-level invalidation). x is the
// window of the current translate job's snapshot (v1.1 lead ruling 4), the
// x the run used — never the current setting.
func (s *translateStage) InputHash(ctx context.Context, lectureID string) (string, error) {
	ld, err := s.d.loadLecture(ctx, lectureID)
	if err != nil {
		return "", err
	}
	j, err := s.d.Store.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageTranslate)
	if err != nil {
		return "", err
	}
	return ctxbuild.TranslateStageHash(ld.snap, model.SnapshotWindow(j.ConfigSnapshot)), nil
}

type translateAnswer struct {
	Idx *int    `json:"idx"`
	Zh  *string `json:"zh"`
}

// translation is the result for one sentence.
type translation struct {
	zh      string
	status  string // ok | qa_failed
	qaError string
	hash    string
	// pending: no usable answer came (zh is "", status qa_failed, qaError
	// says why): the sentence is left for the user (v3.2.x P1, Q2a).
	pending bool
	// refusal: the provider declined the request of a pending sentence —
	// the client's error (coded llm.refusal, permanent). When every
	// sentence of a run was declined it is the stage's error (Run).
	refusal error
}

// isPending reports a sentence left without a translation by a finished
// attempt ("待处理", v3.2.x P1): zh = "" with zh_status qa_failed. Its
// zh_hash is current, so no run sends it again by itself (translateTodo);
// "重翻此句" (state.RetranslateSentence) marks it stale and the incremental
// run translates it.
func isPending(s model.Sentence) bool {
	return s.ZhStatus == model.ZhQAFailed && s.Zh != nil && strings.TrimSpace(*s.Zh) == ""
}

// pendingIDs renders sentence numbers for a message: "#3, #17" (at most 20,
// then "…"); sep joins them.
func pendingIDs(idxs []int, sep string) string {
	shown, more := idxs, ""
	if len(shown) > 20 {
		shown, more = shown[:20], "…"
	}
	ids := make([]string, len(shown))
	for i, idx := range shown {
		ids[i] = fmt.Sprintf("#%d", idx)
	}
	return strings.Join(ids, sep) + more
}

// translateTodo returns the sentences a run must translate (lead ruling at
// Gate B, NOTES.md; SPEC §4.6, §7.3, §11): zh_status none or stale, plus ok
// or qa_failed sentences whose zh_hash differs from the current hash (their
// inputs changed: §7.3 "哈希不同即 stale"). The same set for every run —
// first run, retry and incremental run; stats.note = incremental does not
// change it. qa_failed with an unchanged hash is not redone (§4.6).
func translateTodo(ss []model.Sentence, hashes []string) []int {
	var todo []int
	for i, s := range ss {
		switch s.ZhStatus {
		case model.ZhNone, model.ZhStale:
			todo = append(todo, i)
		case model.ZhOK, model.ZhQAFailed:
			if s.ZhHash == nil || i >= len(hashes) || *s.ZhHash != hashes[i] {
				todo = append(todo, i)
			}
		}
	}
	return todo
}

func (s *translateStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	// The job's snapshot gives the model and the window x (v1.1 A4.1). An
	// incremental run carries the snapshot of the translate job it follows
	// (A4.2), so a lecture's translations come from one model.
	ctx, job, target, err := s.llmTarget(ctx, lectureID, model.StageTranslate)
	if err != nil {
		return err
	}
	ld, err := d.loadLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	if !ld.hasSum {
		return sched.Permanent(model.Errorf(CodeTranslateNoSummary, nil, errors.New("找不到本讲座的摘要（summary 的产物），请重跑 summary 阶段")))
	}
	x := model.SnapshotWindow(job.ConfigSnapshot)
	// An incremental run (SPEC §7.3) only redoes what a review touched. If the
	// glossary or summary changed while it waited, every sentence hash has
	// changed: that is the whole-stage stale case of §7.3 rule 3, which must
	// not retranslate the lecture automatically (lead ruling, NOTES.md).
	if base, ok := state.IncrementalBaseHash(job.Stats.Note); ok && base != "" && base != ctxbuild.TranslateStageHash(ld.snap, x) {
		return sched.Permanent(model.Errorf(CodeTranslatePrefixChanged, nil, errors.New(IncrementalPrefixChanged)))
	}
	recordGlossaryVersion(ctx, ld.glossaryVersion)
	todo := translateTodo(ld.sentences, ctxbuild.TranslateHashes(ld.snap, x))
	// pending: the sentences without a translation when the run ends — those
	// an earlier run left and this one does not redo, plus its own.
	inTodo := make(map[int]bool, len(todo))
	for _, i := range todo {
		inTodo[i] = true
	}
	var pending []int
	for i, sen := range ld.sentences {
		if !inTodo[i] && isPending(sen) {
			pending = append(pending, sen.Idx)
		}
	}
	if len(todo) == 0 {
		recordPending(ctx, pending)
		progress(1)
		return nil
	}

	var (
		mu     sync.Mutex
		failed []int
		// refused counts the pending sentences the provider declined;
		// firstRefusal is the first such error (see the end of Run).
		refused      int
		firstRefusal error
	)
	pc := &progressCounter{total: len(todo), report: progress}
	ev := d.events()
	notify := newThrottle(time.Second)
	wctx := context.WithoutCancel(ctx)
	// record writes the translation of one sentence (SPEC §4.6: written
	// immediately, so a cancel or failure keeps it).
	record := func(idx int, tr translation) error {
		if !tr.pending && tr.status == model.ZhQAFailed && strings.TrimSpace(tr.zh) == "" {
			// The answers parsed but their translation is empty (QA: "the
			// translation is empty"): the sentence has no translation all the
			// same, so it counts as pending like the others (isPending), with
			// the warning and the guard below (SEC1 L5). qa_error keeps the
			// QA reason (SPEC §7).
			tr.pending = true
		}
		if tr.pending {
			// No usable answer: the sentence is written as pending and the
			// run goes on (v3.2.x P1, Q2a).
			// The code only: the reason may quote the model's answer
			// (lecture content); it is in qa_error and rejected-*.jsonl.
			code, category := "", ""
			if info := qa.PendingInfo(tr.qaError); info != nil {
				code = info.Code
				category, _ = info.Params["category"].(string)
			} else if strings.TrimSpace(tr.zh) == "" {
				code = qa.CodeEmpty
			}
			slog.Warn("translate: sentence left pending", "lecture", lectureID, "idx", idx, "code", code,
				"attempts", businessRetries+1, "category", category)
			mu.Lock()
			failed = append(failed, idx)
			if tr.refusal != nil {
				refused++
				if firstRefusal == nil {
					firstRefusal = tr.refusal
				}
			}
			mu.Unlock()
		}
		// Written immediately (SPEC §4.6): a cancel or failure keeps it.
		if err := d.Store.SetTranslation(wctx, lectureID, idx, sentenceTr(tr)); err != nil {
			return err
		}
		pc.inc()
		if notify.ok() {
			ev.Changed("sentences", lectureID)
		}
		return nil
	}
	n := effectiveBatch(job, target)
	if n == 1 && target.Params.Batch != nil {
		// One sentence per request although the snapshot has a batch size
		// (an incremental run of a batch job, or batch 1): the client must
		// not size max_tokens for a batch, so that the requests are exactly
		// the single-sentence requests of before.
		single := target
		single.Params.Batch = nil
		ctx = llm.WithTarget(ctx, single)
	}
	if n > 1 {
		// v3.2.x P2 (Q14a): n consecutive sentences per request, with the
		// split-and-fallback ladder of translate_batch.go. QA, pending and
		// the writes stay per sentence (record, recordBatch).
		recordBatch := func(idxs []int, trs []translation) error {
			if err := d.Store.Tx(wctx, func(tx db.Store) error {
				for k, idx := range idxs {
					if err := tx.SetTranslation(wctx, lectureID, idx, sentenceTr(trs[k])); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			pc.add(len(idxs))
			if notify.ok() {
				ev.Changed("sentences", lectureID)
			}
			return nil
		}
		b := &batchRun{s: s, lec: ld.snap, x: x, lectureID: lectureID, record: record, recordBatch: recordBatch,
			deepSeek: d.deepSeek(target.ProviderID)}
		err = b.run(ctx, todo, n, d.concurrency(config.RoleTranslate, target.ProviderID))
	} else {
		// The first sentence goes out alone to warm the provider's prompt cache
		// (perf-llm, warmup.go); the order of sending is all that changes.
		err = parallelWarm(ctx, len(todo), d.concurrency(config.RoleTranslate, target.ProviderID), translateWarmup, func(ctx context.Context, k int) (bool, error) {
			idx := todo[k]
			tr, err := s.translateOne(ctx, ld.snap, idx, x)
			if err != nil {
				return true, err
			}
			if err := record(idx, tr); err != nil {
				return true, err
			}
			return false, nil
		})
	}
	ev.Changed("sentences", lectureID)
	ev.Changed("lecture", lectureID)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	sort.Ints(failed)
	if len(failed) == len(todo) && len(todo) > 1 {
		// Not one sentence of the run got an answer: the model or its setup is
		// wrong, not a few sentences. The stage fails as it did before
		// v3.2.x, and the sentences go back to stale so that a retry sends
		// them again (pending ones would be left alone).
		if err := d.Store.SetZhStatus(wctx, lectureID, failed, model.ZhStale); err != nil {
			return err
		}
		ev.Changed("sentences", lectureID)
		if refused == len(failed) && firstRefusal != nil {
			// Every sentence was declined (review R1 suggestion 8): the
			// reason is the provider's refusal, not unusable output — the
			// client's llm.refusal (permanent: asking again gives the same;
			// the user can choose another model), as repair and material
			// do when every chunk was declined.
			return firstRefusal
		}
		return model.Errorf(CodeTranslateFailed, map[string]any{"n": len(failed), "ids": pendingIDs(failed, ", "), "attempts": businessRetries + 1},
			fmt.Errorf("%d 句没有得到译文（模型输出连续 %d 次无法使用）：%s。点击重试只会重做这些句子",
				len(failed), businessRetries+1, pendingIDs(failed, "、")))
	}
	for _, i := range failed {
		pending = append(pending, ld.sentences[i].Idx)
	}
	sort.Ints(pending)
	recordPending(ctx, pending)
	progress(1)
	return nil
}

// recordPending writes the pending sentences of a finished run into the
// job's stats: stats.pending_sentences and the warning the stage ends with
// (the scheduler shows a done job with a warning as "完成，有警告"). Without
// pending sentences both are cleared (a run after the user had them
// translated again).
func recordPending(ctx context.Context, pending []int) {
	warning := ""
	var info *model.ErrorInfo
	if n := len(pending); n > 0 {
		warning = fmt.Sprintf("%d 句待处理（没有得到可用的译文，导出中显示为“%s”）：%s。可以在播放器中对这些句子点击“重翻此句”", n, MissingZh, pendingIDs(pending, "、"))
		info = &model.ErrorInfo{Code: WarnTranslatePending, Params: map[string]any{"n": n, "ids": pendingIDs(pending, ", ")}}
	}
	sched.UpdateStats(ctx, func(st *model.JobStats) {
		st.PendingSentences = len(pending)
		st.Warning, st.WarningInfo = warning, info
	})
}

// pendingTranslation is the translation of a sentence whose request ended
// with err, when err means "no answer for this sentence" rather than a
// failure of the run: every answer was unusable (errUnusable), or the
// provider declined the request (llm.ErrRefusal: asking again gives the
// same, and one declined sentence must not stop the lecture).
func pendingTranslation(err error, hash string) (translation, bool) {
	reason := ""
	var refusal error
	switch {
	case isUnusable(err):
		var u *errUnusable
		errors.As(err, &u)
		reason = qa.UnusableReason(businessRetries+1, fmt.Sprint(u.last))
	case errors.Is(err, llm.ErrRefusal):
		reason = qa.RefusedReason(refusalCategory(err))
		refusal = err
	default:
		return translation{}, false
	}
	return translation{zh: "", status: model.ZhQAFailed, qaError: reason, hash: hash, pending: true, refusal: refusal}, true
}

// sentenceTr is the row written for a translation.
func sentenceTr(tr translation) db.SentenceTr {
	var qe *string
	if tr.qaError != "" {
		qe = &tr.qaError
	}
	zh, h := tr.zh, tr.hash
	return db.SentenceTr{Zh: &zh, ZhStatus: tr.status, ZhHash: &h, QAError: qe}
}

// translateCheck rejects a single-sentence answer that is not for target idx
// or has no "zh".
func translateCheck(idx int) func(a *translateAnswer) error {
	return func(a *translateAnswer) error {
		if a.Idx == nil || *a.Idx != idx {
			return fmt.Errorf("answer idx does not match target #%d", idx)
		}
		if a.Zh == nil {
			return errors.New(`answer has no "zh"`)
		}
		return nil
	}
}

// translateOne translates sentence idx with its business retries, runs QA
// and, on a QA failure, one retranslation with the reason (SPEC §7.4). A
// sentence that gets no usable answer comes back pending (pendingTranslation).
func (s *translateStage) translateOne(ctx context.Context, lec ctxbuild.LectureSnapshot, idx, x int) (translation, error) {
	req := ctxbuild.BuildTranslateRequest(lec, idx, x)
	ctx = llm.WithJSONSchema(ctx, ctxbuild.SchemaTranslate)
	target := lec.Sentences[idx].Text
	a, err := askJSON(ctx, s.d.LLM, config.RoleTranslate, req.System, req.User, translateCheck(req.Idx))
	if err != nil {
		if tr, ok := pendingTranslation(err, req.Hash); ok {
			return tr, nil
		}
		return translation{}, err
	}
	zh := strings.TrimSpace(*a.Zh)
	ok, reason := qa.Check(target, zh, lec.Terms)
	if ok {
		return translation{zh: zh, status: model.ZhOK, hash: req.Hash}, nil
	}
	return s.retryAfterQA(ctx, lec, idx, req, zh, reason)
}

// retryAfterQA is the retranslation of SPEC §7.4 for a first answer zh that
// failed QA with reason: the single-sentence request req again with the
// reason (ctxbuild.TranslateRetryUser). A second answer that fails QA is
// kept, flagged qa_failed; when the second request gives no usable answer or
// is declined, the first answer is kept, flagged. Shared by translateOne and
// the batch path (a batch sentence that fails QA is retranslated alone).
// idx is the sentence's index in lec.Sentences.
func (s *translateStage) retryAfterQA(ctx context.Context, lec ctxbuild.LectureSnapshot, idx int, req ctxbuild.TranslateRequest, zh, reason string) (translation, error) {
	ctx = llm.WithJSONSchema(ctx, ctxbuild.SchemaTranslate)
	target := lec.Sentences[idx].Text
	a2, err := askJSON(ctx, s.d.LLM, config.RoleTranslate, req.System, ctxbuild.TranslateRetryUser(req.User, reason), translateCheck(req.Idx))
	if err != nil {
		if isUnusable(err) || errors.Is(err, llm.ErrRefusal) { // keep the first answer, flagged
			return translation{zh: zh, status: model.ZhQAFailed, qaError: reason, hash: req.Hash}, nil
		}
		return translation{}, err
	}
	zh2 := strings.TrimSpace(*a2.Zh)
	if ok2, reason2 := qa.Check(target, zh2, lec.Terms); !ok2 {
		return translation{zh: zh2, status: model.ZhQAFailed, qaError: reason2, hash: req.Hash}, nil
	}
	return translation{zh: zh2, status: model.ZhOK, hash: req.Hash}, nil
}

// throttle lets an action through at most once per interval.
type throttle struct {
	mu    sync.Mutex
	every time.Duration
	last  time.Time
}

func newThrottle(every time.Duration) *throttle { return &throttle{every: every} }

func (t *throttle) ok() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if now.Sub(t.last) < t.every {
		return false
	}
	t.last = now
	return true
}
