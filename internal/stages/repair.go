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
	"unicode/utf8"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// repairStage implements SPEC §6.4: chunked AI repair, classification and
// automatic application, written atomically at the end.
type repairStage struct{ apiStage }

func (s *repairStage) Name() string { return model.StageRepair }

// InputHash = all asr_text + summary + enabled glossary (SPEC §4.4).
func (s *repairStage) InputHash(ctx context.Context, lectureID string) (string, error) {
	ld, err := s.d.loadLecture(ctx, lectureID)
	if err != nil {
		return "", err
	}
	return ctxbuild.RepairInputHash(ld.snap), nil
}

type repairPatch struct {
	Idx    *int   `json:"idx"` // nil when the model left it out: the entry is dropped
	After  string `json:"after"`
	Class  string `json:"class"`
	Reason string `json:"reason"`
}

type repairAnswer struct {
	Patches *[]repairPatch `json:"patches"`
}

func checkRepairAnswer(a *repairAnswer) error {
	if a.Patches == nil {
		return errors.New(`answer has no "patches" array`)
	}
	return nil
}

// repairPlan is the outcome of applying the §6.4 rules to the model answers,
// before it is written.
type repairPlan struct {
	Patches  []model.Patch
	Repaired map[int]string // idx → new repaired_text
}

// planRepair applies the rules of SPEC §6.4 to the answers of every chunk
// (answers[i] belongs to chunks[i]; nil = failed chunk):
//   - a patch without idx, or whose idx is not a TARGET sentence of its
//     chunk, is dropped;
//   - per sentence only the last patch of its own chunk is kept;
//   - before is always asr_text; ⟦ ⟧ marks are removed from after; an empty
//     after or one equal to asr_text (no change) is dropped;
//   - auto/absorb → review none; decide → review pending; an unknown class
//     counts as decide; an edit distance above 50% of the sentence length
//     downgrades to decide;
//   - a sentence with human_text keeps its class but gets review none (the
//     human decision wins; repaired_text is still updated).
func planRepair(lectureID string, sentences []model.Sentence, chunks []ctxbuild.RepairChunk, answers [][]repairPatch) repairPlan {
	byIdx := make(map[int]model.Sentence, len(sentences))
	for _, s := range sentences {
		byIdx[s.Idx] = s
	}
	final := map[int]repairPatch{}
	for i, c := range chunks {
		if i >= len(answers) {
			break
		}
		for _, p := range answers[i] {
			if p.Idx == nil {
				// the length only: "after" is a sentence of the lecture (SEC1 L4)
				slog.Warn("repair: patch without idx dropped", "lecture", lectureID, "chunk", i, "after_chars", utf8.RuneCountInString(p.After))
				continue
			}
			if c.Contains(*p.Idx) {
				final[*p.Idx] = p
			}
		}
	}
	idxs := make([]int, 0, len(final))
	for idx := range final {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	plan := repairPlan{Repaired: map[int]string{}}
	for _, idx := range idxs {
		p := final[idx]
		s, ok := byIdx[idx]
		if !ok {
			continue
		}
		after := strings.TrimSpace(ctxbuild.StripMarks(p.After))
		if after == "" || after == s.ASRText {
			continue
		}
		class := strings.ToLower(strings.TrimSpace(p.Class))
		if class != model.ClassAuto && class != model.ClassAbsorb {
			class = model.ClassDecide
		}
		if ctxbuild.EditExceedsHalf(s.ASRText, after) {
			class = model.ClassDecide
		}
		review := model.ReviewNone
		if class == model.ClassDecide {
			review = model.ReviewPending
		}
		if s.HumanText != nil {
			review = model.ReviewNone
		}
		plan.Patches = append(plan.Patches, model.Patch{
			ID: db.NewID(), LectureID: lectureID, SentenceIdx: idx, Before: s.ASRText, After: after,
			Class: class, Reason: strings.TrimSpace(p.Reason), Review: review, CreatedAt: db.Now(),
		})
		plan.Repaired[idx] = after
	}
	return plan
}

func (s *repairStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	ctx, _, target, err := s.llmTarget(ctx, lectureID, model.StageRepair)
	if err != nil {
		return err
	}
	ld, err := d.loadLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	if !ld.hasSum {
		return sched.Permanent(model.Errorf(CodeRepairNoSummary, nil, errors.New("找不到本讲座的摘要（summary 的产物），请重跑 summary 阶段")))
	}
	recordGlossaryVersion(ctx, ld.glossaryVersion)
	chunks := ctxbuild.BuildRepairRequest(ld.snap)
	answers := make([][]repairPatch, len(chunks))
	var (
		mu     sync.Mutex
		failed []int
		// refused: chunks the provider declined (llm.ErrRefusal); asking
		// again gives the same, so like an unusable chunk they are skipped
		// with a warning (review fix of v3.2.x P1). firstRefusal is the
		// stage's error when every chunk was declined.
		refused      []int
		firstRefusal error
	)
	pc := &progressCounter{total: len(chunks), report: progress}
	// The first chunk gets a head start to warm the prompt cache (perf-llm,
	// warmup.go; on Anthropic with a streamed model until its answer begins,
	// v3.2.x r2 N46).
	err = parallelWarm(ctx, len(chunks), d.concurrency(config.RoleRepair, target.ProviderID), d.repairWarmupFor(target), func(ctx context.Context, i int) (bool, error) {
		c := chunks[i]
		a, err := askJSON(llm.WithJSONSchema(ctx, ctxbuild.SchemaRepair), d.LLM, config.RoleRepair, c.System, c.User, checkRepairAnswer)
		if err != nil {
			if isUnusable(err) {
				// No error text: it quotes the model's answer (lecture content).
				slog.Warn("repair chunk unusable", "lecture", lectureID, "chunk", i, "problem", unusableKind(err))
				mu.Lock()
				failed = append(failed, i)
				mu.Unlock()
				pc.inc()
				return false, nil
			}
			if errors.Is(err, llm.ErrRefusal) && ctx.Err() == nil {
				slog.Warn("repair chunk refused by the provider", "lecture", lectureID, "chunk", i, "category", refusalCategory(err))
				mu.Lock()
				refused = append(refused, i)
				if firstRefusal == nil {
					firstRefusal = err
				}
				mu.Unlock()
				pc.inc()
				return false, nil
			}
			return true, err // transport, permanent or cancel: the whole stage stops
		}
		answers[i] = *a.Patches
		pc.inc()
		return false, nil
	})
	if ctx.Err() != nil {
		return ctx.Err() // nothing written (SPEC §6.4 atomicity)
	}
	if err != nil {
		return err
	}
	sort.Ints(failed)
	sort.Ints(refused)
	if len(chunks) > 0 && len(refused) == len(chunks) {
		return firstRefusal // llm.refusal, permanent: every chunk was declined
	}
	// SPEC §6.4: a chunk without patches is recorded in job.error. The
	// warning goes through stats.warning; the scheduler writes it into
	// job.error when the job becomes done (Gate B ruling, NOTES.md).
	// Sentence numbers are 1-based, as people count them.
	note, warning := "", ""
	var noteI, warningI *model.ErrorInfo
	if len(failed) > 0 {
		parts := make([]string, len(failed))
		ranges := make([]string, len(failed))
		nums, spans := make([]int, len(failed)), make([]string, len(failed))
		for k, i := range failed {
			ranges[k] = fmt.Sprintf("第 %d–%d 句", chunks[i].Start+1, chunks[i].End)
			parts[k] = fmt.Sprintf("第 %d 块（%s）", i+1, ranges[k])
			nums[k], spans[k] = i+1, fmt.Sprintf("%d–%d", chunks[i].Start+1, chunks[i].End)
		}
		note = fmt.Sprintf("共 %d 块，其中%s的模型输出连续 %d 次无法解析，这些句子未做修复", len(chunks), strings.Join(parts, "、"), businessRetries+1)
		warning = fmt.Sprintf("%s的修复请求 %d 次都无法解析，这些句子保留原始识别结果", strings.Join(ranges, "、"), businessRetries+1)
		attempts := businessRetries + 1
		noteI = noteInfo(part(NoteRepairChunksFailed, map[string]any{
			"total": len(chunks), "chunks": numList(nums), "ranges": strings.Join(spans, ", "), "attempts": attempts}))
		warningI = &model.ErrorInfo{Code: WarnRepairUnrepaired, Params: map[string]any{"ranges": strings.Join(spans, ", "), "attempts": attempts}}
	}
	if len(refused) > 0 {
		ranges := make([]string, len(refused))
		nums, spans := make([]int, len(refused)), make([]string, len(refused))
		for k, i := range refused {
			ranges[k] = fmt.Sprintf("第 %d–%d 句", chunks[i].Start+1, chunks[i].End)
			nums[k], spans[k] = i+1, fmt.Sprintf("%d–%d", chunks[i].Start+1, chunks[i].End)
		}
		rnote := fmt.Sprintf("共 %d 块，其中第 %s 块（%s）被服务商的安全策略拒绝，这些句子未做修复", len(chunks), numList(nums), strings.Join(ranges, "、"))
		rwarn := fmt.Sprintf("%s的修复请求被服务商的安全策略拒绝，这些句子保留原始识别结果", strings.Join(ranges, "、"))
		rnoteI := part(NoteRepairChunksRefused, map[string]any{"total": len(chunks), "chunks": numList(nums), "ranges": strings.Join(spans, ", ")})
		rwarnI := model.ErrorInfo{Code: WarnRepairRefused, Params: map[string]any{"ranges": strings.Join(spans, ", ")}}
		if note == "" {
			note, warning = rnote, rwarn
			noteI, warningI = noteInfo(rnoteI), noteInfo(rwarnI)
		} else {
			note, warning = note+"；"+rnote, warning+"；"+rwarn
			noteI, warningI = noteInfo(*noteI, rnoteI), noteInfo(*warningI, rwarnI)
		}
	}
	if len(failed) > 0 && len(failed)+len(refused) == len(chunks) {
		// No chunk got an answer (all declined is the refusal error above).
		return model.Errorf(CodeRepairAllFailed, map[string]any{"n": len(chunks), "attempts": businessRetries + 1}, fmt.Errorf("修复失败：%s", note))
	}

	plan := planRepair(lectureID, ld.sentences, chunks, answers)
	// Everything in one transaction (SPEC §6.4): leftovers of an interrupted
	// earlier attempt are cleared first.
	wctx := context.WithoutCancel(ctx)
	err = d.Store.Tx(wctx, func(tx db.Store) error {
		if err := tx.DeletePatches(wctx, lectureID); err != nil {
			return err
		}
		if err := tx.ResetRepairedText(wctx, lectureID); err != nil {
			return err
		}
		if len(plan.Patches) > 0 {
			if err := tx.InsertPatches(wctx, plan.Patches); err != nil {
				return err
			}
		}
		for idx, after := range plan.Repaired {
			if err := tx.SetRepairedText(wctx, lectureID, idx, after); err != nil {
				return err
			}
		}
		return tx.RecomputeText(wctx, lectureID, nil)
	})
	if err != nil {
		return err
	}
	sched.UpdateStats(ctx, func(st *model.JobStats) {
		st.Note, st.NoteInfo = note, noteI
		st.Warning, st.WarningInfo = warning, warningI // cleared on a clean run (e.g. a retry)
	})
	ev := d.events()
	ev.Changed("patches", lectureID)
	ev.Changed("sentences", lectureID)
	ev.Changed("lecture", lectureID)
	progress(1)
	return nil
}
