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

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/glossary"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// glossaryReviewStage is glossary_review: the glossary janitor of SPEC-v2
// G3. A course stage (scope = course) of the API pool; like glossary_seed it
// has no snapshot and the llm client uses the current setting of the
// glossary_janitor role. Its dependencies are computed by state.Deps.
//
// The janitor only curates (G6 rule 2): it changes tiers, disables, merges
// and fixes translations of existing, unlocked terms; every action is
// validated, all of them are applied in one transaction (glossary_version
// + 1, even with several batches) and a report is stored for review and
// undo (G3.3, G3.5).
type glossaryReviewStage struct{ apiStage }

func (s *glossaryReviewStage) Name() string { return model.StageGlossaryReview }

// InputHash: none. The janitor changes the glossary it reads, so a hash of
// its input would make every finished run look stale at once.
func (s *glossaryReviewStage) InputHash(context.Context, string) (string, error) { return "", nil }

// janitorAction is one entry of the G3.3 answer.
type janitorAction struct {
	ID     string  `json:"id"`
	Action string  `json:"action"`
	Into   string  `json:"into"`
	Zh     *string `json:"zh"` // nil: missing (fix_zh needs it; "" = keep in English)
	Reason string  `json:"reason"`
}

type janitorAnswer struct {
	Actions *[]janitorAction `json:"actions"`
}

func checkJanitorAnswer(a *janitorAnswer) error {
	if a.Actions == nil {
		return errors.New(`answer has no "actions" array`)
	}
	return nil
}

func (s *glossaryReviewStage) Run(ctx context.Context, courseID string, progress func(float64)) error {
	d := s.d
	in, err := d.janitorInput(ctx, courseID)
	if err != nil {
		return err
	}
	b := ctxbuild.NewJanitorBuilder(in)
	batches := b.Batches()
	providerName, modelName := d.roleModel(s.role)
	progress(0.05)

	total := 0
	for _, bt := range batches {
		total += len(bt)
	}
	var (
		mu       sync.Mutex
		answered int // entries whose batch was answered
		requests int // answered requests (batches after splitting)
		split    bool
	)
	// solve asks for one batch; an answer cut off at the output limit
	// (llm.ErrTruncated) is asked again as two halves in en_key order, in
	// parallel, down to janitorMinSplit entries (lead ruling after the real
	// acceptance: the model may act on nearly every entry). A batch that can
	// still be split skips the client's truncation ladder (T-PERF-07, user
	// 2026-11-11): its first truncation splits it at once instead of sending
	// it twice more with a doubled limit, from scratch, while the course's
	// transcription waits. A batch below janitorMinSplit, which cannot be
	// split, keeps the ladder (a lower thinking level, then a doubled limit)
	// before the run fails as before.
	var solve func(ctx context.Context, entries []model.Term) ([]janitorAction, error)
	solve = func(ctx context.Context, entries []model.Term) ([]janitorAction, error) {
		req := b.Request(entries)
		actx := ctx
		if len(entries) >= janitorMinSplit {
			actx = llm.WithNoTruncationRetry(ctx)
		}
		a, err := askJSON(actx, d.LLM, s.role, req.System, req.User, checkJanitorAnswer)
		if err == nil {
			mu.Lock()
			answered += len(entries)
			requests++
			p := float64(answered) / float64(max(total, 1))
			mu.Unlock()
			progress(0.05 + p*0.85)
			return *a.Actions, nil
		}
		if !errors.Is(err, llm.ErrTruncated) || len(entries) < janitorMinSplit || ctx.Err() != nil {
			return nil, err
		}
		mu.Lock()
		split = true
		mu.Unlock()
		halves := [][]model.Term{entries[:len(entries)/2], entries[len(entries)/2:]}
		out := make([][]janitorAction, 2)
		err = parallel(ctx, 2, 2, func(ctx context.Context, i int) (bool, error) {
			acts, err := solve(ctx, halves[i])
			if err != nil {
				return true, err
			}
			out[i] = acts
			return false, nil
		})
		if err != nil {
			return nil, err
		}
		return append(out[0], out[1]...), nil
	}

	answers := make([][]janitorAction, len(batches))
	err = parallel(ctx, len(batches), d.concurrency(s.role, ""), func(ctx context.Context, i int) (bool, error) {
		acts, err := solve(ctx, batches[i])
		if err != nil {
			return true, err
		}
		answers[i] = acts
		return false, nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if sched.IsPermanent(err) {
			// Appendix D2: the janitor cannot be used (no provider, key
			// refused, no credit, output cut off even for small batches).
			// The course goes on with the G2 rule; asr does not wait for a
			// failed review.
			return s.fallback(ctx, courseID, err)
		}
		if isUnusable(err) {
			return fmt.Errorf("整理术语表失败：%w", err)
		}
		return err
	}
	var acts []janitorAction
	for _, a := range answers { // batch order (en_key): a deterministic application order
		acts = append(acts, a...)
	}
	splitNote := ""
	var splitPart []model.ErrorInfo
	if split {
		splitNote = fmt.Sprintf("输出过长，自动拆成 %d 批", requests)
		splitPart = []model.ErrorInfo{part(NoteJanitorSplit, map[string]any{"batches": requests})}
	}
	res, err := applyJanitor(context.WithoutCancel(ctx), d.Store, courseID, acts, providerName, modelName, d.now(), splitNote, splitPart)
	if err != nil {
		return err
	}
	if res.changed {
		d.invalidate(ctx, courseID, state.StaleCause{Kind: state.StaleJanitor})
	}
	d.events().Changed("terms", courseID)
	d.events().Changed("course", courseID)
	note := janitorNote(res.rep)
	parts := []model.ErrorInfo{janitorNotePart(res.rep)}
	if res.noops > 0 {
		note += fmt.Sprintf("；无改动 %d 条", res.noops)
		parts = append(parts, part(NoteJanitorNoops, map[string]any{"n": res.noops}))
	}
	switch {
	case len(batches) == 0:
		note = "术语表中没有需要整理的条目（已锁定和已停用的条目不参与整理）"
		parts = []model.ErrorInfo{part(NoteJanitorNothing, nil)}
	case split:
		note += "；" + splitNote
		parts = append(parts, splitPart...)
	case len(batches) > 1:
		note += fmt.Sprintf("（共 %d 批）", len(batches))
		parts = append(parts, part(NoteJanitorBatches, map[string]any{"batches": len(batches)}))
	}
	sched.UpdateStats(ctx, func(st *model.JobStats) { st.Note, st.NoteInfo = note, noteInfo(parts...) })
	progress(1)
	return nil
}

// janitorMinSplit: a batch with fewer entries whose answer is still cut off
// at the output limit is not split again; the run fails (appendix D2 path).
const janitorMinSplit = 20

// fallback runs the G2 rule after a permanent janitor failure and returns
// the permanent error the job fails with (appendix D2, ruling 6).
func (s *glossaryReviewStage) fallback(ctx context.Context, courseID string, cause error) error {
	d := s.d
	reason := cause.Error()
	if r := []rune(reason); len(r) > 300 {
		reason = string(r[:300]) + "…"
	}
	rep, changed, err := glossary.RunFallbackNote(context.WithoutCancel(ctx), d.Store, courseID, d.now(),
		glossary.NoteFallbackFailed(reason), glossary.NoteInfoFallbackFailed(codeOf(cause), reason))
	if err != nil {
		slog.Warn("glossary fallback after a janitor failure", "course", courseID, "err", err)
		return model.Errorf(CodeJanitorFallbackFailed, causeParams(cause),
			fmt.Errorf("术语表清道夫调用失败（%s），按常用词规则整理也失败：%w", detailOf(cause), err))
	}
	if changed {
		d.invalidate(ctx, courseID, state.StaleCause{Kind: state.StaleJanitor})
	}
	d.events().Changed("terms", courseID)
	d.events().Changed("course", courseID)
	sched.UpdateStats(ctx, func(st *model.JobStats) {
		st.Note = fmt.Sprintf("术语表清道夫不可用，已按常用词规则整理 %d 条", len(rep.Items))
		st.NoteInfo = noteInfo(part(NoteJanitorUnavailable, map[string]any{"n": len(rep.Items)}))
	})
	// The janitor's own error (e.g. llm.rejected) is the cause param; its
	// text stays in the detail.
	return sched.Permanent(model.Errorf(CodeJanitorFailedFallback, causeParams(cause),
		&withText{text: "整理术语表失败，已改用常用词规则整理术语表：" + detailOf(cause), err: cause}))
}

// causeParams is {"cause": code of cause} when cause is coded.
func causeParams(cause error) map[string]any {
	if c := codeOf(cause); c != "" {
		return map[string]any{"cause": c}
	}
	return nil
}

// janitorInput loads the janitor's input in one read transaction: course
// name and code, SYLLABUS.md, the terms and the materials. Nothing from any
// lecture is read (G6 rule 7).
func (d Deps) janitorInput(ctx context.Context, courseID string) (ctxbuild.JanitorInput, error) {
	var in ctxbuild.JanitorInput
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		in = ctxbuild.JanitorInput{}
		c, err := tx.GetCourse(ctx, courseID)
		if err != nil {
			return err
		}
		in.CourseName = c.Name
		if c.Code != nil {
			in.CourseCode = *c.Code
		}
		switch sy, err := tx.GetSyllabus(ctx, courseID); {
		case err == nil:
			if sy.Text != nil {
				in.Syllabus = *sy.Text
			}
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		if in.Terms, err = tx.ListTerms(ctx, courseID); err != nil {
			return err
		}
		in.Materials, err = tx.ListMaterials(ctx, courseID)
		return err
	})
	return in, err
}

// roleModel names the provider and model of role's current setting (report header).
func (d Deps) roleModel(role string) (provider, modelName string) {
	if d.Settings != nil {
		if t, err := d.Settings.RoleTarget(role); err == nil {
			return t.ProviderName, t.Model
		}
		return "", ""
	}
	return llm.RoleProvider(d.Cfg, role)
}

// enqueueGlossaryReview asks for a glossary_review run after new terms were
// merged (G3.1, ruling 4); state.EnqueueGlossaryReview does nothing when no
// janitor is configured.
func (d Deps) enqueueGlossaryReview(ctx context.Context, courseID string) error {
	queued, err := state.EnqueueGlossaryReview(context.WithoutCancel(ctx), d.Store, courseID)
	if err != nil {
		return model.Errorf(CodeGlossaryReviewEnqueue, nil, fmt.Errorf("排队整理术语表失败：%w", err))
	}
	if queued {
		d.events().Changed("course", courseID)
	}
	return nil
}

// fallbackNote is the part of a term-extraction note about the G2 fallback
// MergeTerms applied ("" when it changed nothing).
func fallbackNote(res MergeResult) string {
	if len(res.Fallback) == 0 {
		return ""
	}
	ref, dis := 0, 0
	for _, it := range res.Fallback {
		if it.Action == model.JanitorDisable {
			dis++
		} else {
			ref++
		}
	}
	return fmt.Sprintf("；按常用词规则设为参考 %d、停用复数形式 %d", ref, dis)
}

// fallbackParts is the coded part of fallbackNote (none when it is "").
func fallbackParts(res MergeResult) []model.ErrorInfo {
	if len(res.Fallback) == 0 {
		return nil
	}
	ref, dis := 0, 0
	for _, it := range res.Fallback {
		if it.Action == model.JanitorDisable {
			dis++
		} else {
			ref++
		}
	}
	return []model.ErrorInfo{part(NoteFallbackApplied, map[string]any{"reference": ref, "disabled": dis})}
}

// janitorNote is the job note of a janitor run:
// "整理 N 条：强制 a、参考 b、停用 c、合并 d、改译法 e；丢弃 f 条".
func janitorNote(rep model.JanitorReport) string {
	n := map[string]int{}
	for _, it := range rep.Items {
		n[it.Action]++
	}
	return fmt.Sprintf("整理 %d 条：强制 %d、参考 %d、停用 %d、合并 %d、改译法 %d；丢弃 %d 条",
		len(rep.Items), n[model.JanitorEnforce], n[model.JanitorReference], n[model.JanitorDisable],
		n[model.JanitorMerge], n[model.JanitorFixZh], len(rep.Rejected))
}

// janitorNotePart is the coded form of janitorNote.
func janitorNotePart(rep model.JanitorReport) model.ErrorInfo {
	n := map[string]int{}
	for _, it := range rep.Items {
		n[it.Action]++
	}
	return part(NoteJanitorDone, map[string]any{
		"n": len(rep.Items), "enforce": n[model.JanitorEnforce], "reference": n[model.JanitorReference],
		"disable": n[model.JanitorDisable], "merge": n[model.JanitorMerge], "fix_zh": n[model.JanitorFixZh],
		"rejected": len(rep.Rejected)})
}

// whyText is the Chinese text of each reason an action was dropped
// (JanitorRejected.Why, shown in the report until the i18n sweep ends; the
// code is JanitorRejected.WhyCode).
var whyText = map[string]string{
	WhyUnknownTerm:   "条目不存在（清道夫只能整理已有条目，不能新增）",
	WhyLocked:        "条目已被用户锁定",
	WhyDuplicate:     "同一条目只能有一个动作，已采用第一个",
	WhyUnknownAction: "未知的动作",
	WhyNoTarget:      "合并目标不存在",
	WhySelf:          "不能合并到自身",
	WhyTargetOff:     "合并目标已停用",
	WhyTargetGone:    "合并目标在本次整理中被停用或合并",
	WhyNoZh:          "缺少译法（zh）",
	WhyUserZh:        "译法由用户设定，清道夫不能修改",
	WhyZhTooLong:     "译法太长，超过术语译名的长度上限",
}

// applyJanitor validates the actions against the glossary as it is now (it
// may have changed while the model was answering) and applies the valid ones
// in one transaction (G3.3): glossary_version + 1 when something changed; a
// report is stored either way (glossary.SaveRunReport). Validation: the id
// must exist and be unlocked; one action per id (the first counts); merge
// needs an existing, enabled target other than itself that this run does
// not disable or merge away; fix_zh needs a zh, never replaces a user's
// zh (G6 rule 3) and gives none over ctxbuild.TermZhMaxChars (AUD5 low-1,
// the limit of every other path that writes a zh). Actions that would
// change nothing (same tier, already
// disabled, the same zh after fixZh) are dropped and only counted.
// Merges are applied after the other actions; a merge fills the target's
// empty zh when the target is unlocked and its zh came from the extractor:
// into the target's own item when this run changed the target ("; zh filled
// from merged 'X'." is appended to its reason), else as an item of its own
// (fix_zh, Into = the merged term's id).
func applyJanitor(ctx context.Context, st db.Store, courseID string, acts []janitorAction,
	providerName, modelName string, now time.Time, runNote string, runParts []model.ErrorInfo) (janitorApplied, error) {
	var rep model.JanitorReport
	changed, noops := false, 0
	err := st.Tx(ctx, func(tx db.Store) error {
		changed, noops = false, 0
		c, err := tx.GetCourse(ctx, courseID)
		if err != nil {
			return err
		}
		terms, err := tx.ListTerms(ctx, courseID)
		if err != nil {
			return err
		}
		byID := make(map[string]*model.Term, len(terms))
		for i := range terms {
			byID[terms[i].ID] = &terms[i]
		}
		rep = model.JanitorReport{
			CourseID: courseID, FromVersion: c.GlossaryVersion, Version: c.GlossaryVersion,
			CreatedAt: glossary.Timestamp(now), ProviderName: providerName, Model: modelName,
		}
		reject := func(a janitorAction, why string) {
			rep.Rejected = append(rep.Rejected, model.JanitorRejected{TermID: a.ID, Action: a.Action, Reason: a.Reason,
				Why: whyText[why], WhyCode: why})
		}

		// validation
		seen := map[string]bool{}
		var valid []janitorAction
		for _, a := range acts {
			a.ID, a.Into, a.Reason = strings.TrimSpace(a.ID), strings.TrimSpace(a.Into), strings.TrimSpace(a.Reason)
			a.Action = strings.ToLower(strings.TrimSpace(a.Action))
			t, ok := byID[a.ID]
			if !ok {
				reject(a, WhyUnknownTerm)
				continue
			}
			if seen[a.ID] {
				reject(a, WhyDuplicate)
				continue
			}
			seen[a.ID] = true
			if t.Locked {
				reject(a, WhyLocked)
				continue
			}
			why := ""
			switch a.Action {
			case model.JanitorEnforce, model.JanitorReference, model.JanitorDisable:
			case model.JanitorMerge:
				target, ok := byID[a.Into]
				switch {
				case a.Into == a.ID:
					why = WhySelf
				case !ok:
					why = WhyNoTarget
				case !target.Enabled:
					why = WhyTargetOff
				}
			case model.JanitorFixZh:
				switch {
				case a.Zh == nil:
					why = WhyNoZh
				case t.ZhSource == model.ZhSourceUser:
					why = WhyUserZh
				case ctxbuild.TermTooLong("", *a.Zh):
					why = WhyZhTooLong
				}
			default:
				why = WhyUnknownAction
			}
			if why != "" {
				reject(a, why)
				continue
			}
			valid = append(valid, a)
		}
		gone := map[string]bool{}
		for _, a := range valid {
			if a.Action == model.JanitorDisable || a.Action == model.JanitorMerge {
				gone[a.ID] = true
			}
		}
		var plain, merges []janitorAction
		for _, a := range valid {
			switch {
			case a.Action != model.JanitorMerge:
				plain = append(plain, a)
			case gone[a.Into]:
				reject(a, WhyTargetGone)
			default:
				merges = append(merges, a)
			}
		}

		// application
		var items []model.JanitorItem
		itemOf := map[string]int{} // term id → its item (terms with an actual change)
		for _, a := range append(plain, merges...) {
			t := byID[a.ID]
			before := model.StateOf(normTerm(*t))
			switch a.Action {
			case model.JanitorEnforce, model.JanitorReference:
				t.Tier = a.Action
			case model.JanitorDisable, model.JanitorMerge:
				t.Enabled = false
			case model.JanitorFixZh:
				if zh := fixZh(*a.Zh, t.En); zh != strings.TrimSpace(t.Zh) {
					t.Zh, t.ZhSource = zh, model.ZhSourceJanitor
				}
			}
			after := model.StateOf(normTerm(*t))
			if after == before {
				noops++ // already so: dropped, neither an item nor rejected
				continue
			}
			t.JanitorNote = a.Reason
			if t.JanitorNote == "" {
				t.JanitorNote = "Changed by the glossary janitor."
			}
			if err := tx.UpdateTerm(ctx, *t); err != nil {
				return err
			}
			it := model.JanitorItem{TermID: t.ID, En: t.En, Action: a.Action, Reason: a.Reason, Before: before, After: after}
			itemOf[t.ID] = len(items)
			items = append(items, it)
			if a.Action != model.JanitorMerge {
				continue
			}
			target := byID[a.Into]
			items[len(items)-1].Into, items[len(items)-1].IntoEn = target.ID, target.En
			// The merged term's zh goes to a target without one (never to a
			// locked target or a zh the user or an earlier janitor run chose:
			// G1, G6 rule 3). When this run already changed the target, the
			// fill joins the target's own item; otherwise the target becomes
			// an item of its own (fix_zh, Into = the merged term) that undoing
			// the merge restores too (lead, Gate review rounds 1 and 2).
			if target.Zh != "" || t.Zh == "" || target.Locked || normTerm(*target).ZhSource != model.ZhSourceExtractor {
				continue
			}
			tb := model.StateOf(normTerm(*target))
			target.Zh, target.ZhSource = t.Zh, normTerm(*t).ZhSource
			fill := fmt.Sprintf("zh filled from merged '%s'.", t.En)
			if k, ok := itemOf[target.ID]; ok {
				own := &items[k]
				own.After = model.StateOf(normTerm(*target))
				own.Reason = strings.TrimSuffix(own.Reason, ".") + "; " + fill
				target.JanitorNote = strings.TrimSuffix(target.JanitorNote, ".") + "; " + fill
			} else {
				target.JanitorNote = fmt.Sprintf("Filled from merged '%s'.", t.En)
				itemOf[target.ID] = len(items)
				items = append(items, model.JanitorItem{
					TermID: target.ID, En: target.En, Action: model.JanitorFixZh, Into: t.ID, IntoEn: t.En,
					Reason: target.JanitorNote, Before: tb, After: model.StateOf(normTerm(*target)),
				})
			}
			if err := tx.UpdateTerm(ctx, *target); err != nil {
				return err
			}
		}
		sort.SliceStable(items, func(i, j int) bool {
			return ctxbuild.EnKey(items[i].En) < ctxbuild.EnKey(items[j].En)
		})
		rep.Items = items
		var notes []string
		var parts []model.ErrorInfo
		if noops > 0 {
			notes = append(notes, fmt.Sprintf("模型对 %d 个条目给出的动作不会带来改动，已忽略", noops))
			parts = append(parts, part(NoteJanitorIgnored, map[string]any{"n": noops}))
		}
		if runNote != "" {
			notes = append(notes, runNote)
		}
		rep.Note, rep.NoteInfo = strings.Join(notes, "；"), noteInfo(append(parts, runParts...)...)
		if len(items) > 0 {
			if rep.Version, err = tx.BumpGlossaryVersion(ctx, courseID); err != nil {
				return err
			}
			changed = true
		}
		return glossary.SaveRunReport(ctx, tx, rep)
	})
	if err != nil {
		return janitorApplied{}, err
	}
	return janitorApplied{rep: rep, noops: noops, changed: changed}, nil
}

// janitorApplied is the outcome of applyJanitor: the stored report, the
// number of actions dropped because they would change nothing, and whether
// glossary_version was bumped.
type janitorApplied struct {
	rep     model.JanitorReport
	noops   int
	changed bool
}

// fixZh normalises the zh of a fix_zh action: trimmed; a "translation" that
// is the English term itself (case and spacing ignored) means "keep it in
// English", i.e. "" (lead ruling after the real acceptance).
func fixZh(zh, en string) string {
	zh = strings.TrimSpace(zh)
	if strings.EqualFold(strings.Join(strings.Fields(zh), " "), strings.Join(strings.Fields(en), " ")) {
		return ""
	}
	return zh
}

// normTerm reads the defaults of the v2 columns ("" tier → enforce,
// "" zh_source → extractor), as the database stores them.
func normTerm(t model.Term) model.Term {
	if t.Tier == "" {
		t.Tier = model.TierEnforce
	}
	if t.ZhSource == "" {
		t.ZhSource = model.ZhSourceExtractor
	}
	return t
}
