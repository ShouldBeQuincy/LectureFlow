// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

// v1.1 state operations (SPEC-v1.1 A2, A3, A4, A7).
//
// Signatures FROZEN by the lead at the start of v1.1 so that the scheduler,
// the API and the stages can be built in parallel. Bodies: core-builder,
// TEST FIRST (CLAUDE.md / user guardrail 2). Every status write still goes
// through TransitionJob; every text write through RecomputeText.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
)

// ErrAlreadyLaunched: Launch of a lecture that is not a draft (409).
var ErrAlreadyLaunched = fmt.Errorf("讲座已经启动: %w", ErrConflict)

// ErrDraft: the operation needs a launched lecture (409), e.g. rerun or
// update of a draft.
var ErrDraft = fmt.Errorf("讲座还是草稿，请先启动: %w", ErrConflict)

// ErrSnapshotUnusable: model_choice "snapshot" was requested but the
// snapshot's provider is gone or has no key (A4.3). 409.
var ErrSnapshotUnusable = fmt.Errorf("原模型不可用: %w", ErrConflict)

// ---------- A3 launch ----------

// Launch implements A3.1 "启动": in ONE transaction, lecture.launched_at =
// now (only while it is NULL) and, for each of the lecture's six current
// jobs, config_snapshot = snaps[stage] (model.EmptySnapshot for stages not
// in snaps). Auxiliary-task jobs (v2) keep their empty snapshot: they use
// their role's current setting. Errors: db.ErrNotFound, ErrAlreadyLaunched.
func Launch(ctx context.Context, st db.Store, lectureID string, snaps map[string]string) error {
	return st.Tx(ctx, func(tx db.Store) error {
		ok, err := tx.SetLectureLaunched(ctx, lectureID, db.Now())
		if err != nil {
			return err
		}
		if !ok {
			return ErrAlreadyLaunched
		}
		jobs, err := tx.ListCurrentJobs(ctx, model.ScopeLecture, lectureID)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if model.IsAuxStage(j.Stage) {
				continue
			}
			snap := snaps[j.Stage]
			if model.IsEmptySnapshot(snap) {
				snap = model.EmptySnapshot
			}
			if err := tx.SetJobSnapshot(ctx, j.ID, snap); err != nil {
				return err
			}
		}
		return nil
	})
}

// IsDraft reports whether a lecture is a draft (launched_at IS NULL).
func IsDraft(l model.Lecture) bool { return l.LaunchedAt == nil }

// ---------- A2 display status, stale reason, update ----------

// DisplayStatuses implements A2.1 for the current jobs of ONE scope (a
// lecture's six jobs): a done job with any upstream job (any earlier stage
// of the scope, not only the immediate one) in stale, pending, queued,
// running, failed or cancelled → model.DisplayOutdated; otherwise its
// status. Keyed by stage; auxiliary tasks (v2) are not listed. Pure.
func DisplayStatuses(jobs []model.Job) map[string]string {
	out := make(map[string]string, len(jobs))
	for _, j := range jobs {
		if model.IsAuxStage(j.Stage) {
			continue
		}
		st := j.Status
		if st == model.StatusDone && upstreamUnsettled(jobs, j.Stage) {
			st = model.DisplayOutdated
		}
		out[j.Stage] = st
	}
	return out
}

// unsettled are the upstream statuses that make a done job outdated (A2.1).
var unsettled = map[string]bool{
	model.StatusStale: true, model.StatusPending: true, model.StatusQueued: true,
	model.StatusRunning: true, model.StatusFailed: true, model.StatusCancelled: true,
}

// upstreamUnsettled reports whether any job of an earlier stage of stage's
// scope (not only the immediate upstream) is in an unsettled status.
func upstreamUnsettled(jobs []model.Job, stage string) bool {
	order := StagesOf(model.ScopeOfStage(stage))
	i := indexOf(order, stage)
	for _, u := range jobs {
		if k := indexOf(order, u.Stage); k >= 0 && k < i && unsettled[u.Status] {
			return true
		}
	}
	return false
}

// inPipelineOrder returns a copy of jobs sorted by the position of their
// stage in its scope (SPEC §4.1 order). Pure.
func inPipelineOrder(jobs []model.Job) []model.Job {
	out := append([]model.Job(nil), jobs...)
	rank := func(stage string) int { return indexOf(StagesOf(model.ScopeOfStage(stage)), stage) }
	sort.SliceStable(out, func(a, b int) bool { return rank(out[a].Stage) < rank(out[b].Stage) })
	return out
}

// StaleStages returns the stages whose job is stale, in pipeline order;
// auxiliary tasks (v2) never count. Pure.
func StaleStages(jobs []model.Job) []string {
	var out []string
	for _, j := range inPipelineOrder(jobs) {
		if j.Status == model.StatusStale && !model.IsAuxStage(j.Stage) {
			out = append(out, j.Stage)
		}
	}
	return out
}

// StaleText implements the lecture-row text of A2.2:
// "{原因}，需要更新：{过期阶段列表}", e.g.
// "术语表已更新（v3 → v4），需要更新：修复、翻译". Distinct reasons of the
// stale jobs are joined with "；" in pipeline order (a stale job without a
// reason contributes "输入已变化"); stage names come from model.StageLabel.
// Reasons are rendered with StaleReasonText (stored as JSON since P14.3).
// Auxiliary tasks (v2) never count. "" when no job is stale. Pure.
//
// TRANSITIONAL (P14.3, phase 2 ruling 5): the API keeps stale_text until
// the i18n extraction; the front end then builds the text from the
// structured reasons (StaleReasonOf) itself.
func StaleText(jobs []model.Job) string {
	var reasons, stages []string
	seen := map[string]bool{}
	for _, j := range inPipelineOrder(jobs) {
		if j.Status != model.StatusStale || model.IsAuxStage(j.Stage) {
			continue
		}
		r := reasonInputChanged
		if j.StaleReason != nil {
			r = StaleReasonText(*j.StaleReason)
		}
		if !seen[r] {
			seen[r] = true
			reasons = append(reasons, r)
		}
		stages = append(stages, model.StageLabel(j.Stage))
	}
	if len(stages) == 0 {
		return ""
	}
	return strings.Join(reasons, "；") + "，需要更新：" + strings.Join(stages, "、")
}

// reasonInputChanged is the stale reason when the cause is unknown (SPEC
// §4.4 "输入已变化"), also shown for stale jobs recorded without a reason.
const reasonInputChanged = "输入已变化"

// causeStages are the stages whose input_hash contains each kind of cause
// (SPEC §4.4, ctxbuild/hash.go and translate's prefix hash): glossary →
// repair, translate; material slides → summary, translate; summary →
// repair, translate. Any other stage whose hash changed during a check with
// that cause gets input_changed instead of a reason that does not apply to it
// (spec review after lead batch 5: a summary made stale by a material
// merged into a glossary check must not say "术语表已更新").
var causeStages = map[string]map[string]bool{
	StaleGlossary: {model.StageRepair: true, model.StageTranslate: true},
	StaleJanitor:  {model.StageRepair: true, model.StageTranslate: true}, // v2 G3.1: same as the glossary
	StaleMaterial: {model.StageSummary: true, model.StageTranslate: true},
	StaleSummary:  {model.StageRepair: true, model.StageTranslate: true},
	// Lecture kinds (K1): a changed material set feeds what a material
	// change feeds (the slides of summary and translate).
	StaleMaterials: {model.StageSummary: true, model.StageTranslate: true},
}

// causeCovers: a cause of kind applies to stage (unknown kind: never).
func causeCovers(kind, stage string) bool { return causeStages[kind][stage] }

// StaleCause tells the invalidation check why it runs (A2.2), so the jobs it
// marks stale get a human-readable stale_reason.
type StaleCause struct {
	Kind     string // StaleGlossary | StaleJanitor (v2) | StaleMaterial | StaleMaterials (K1) | StaleSummary | "" (unknown)
	Material string // filename, for StaleMaterial
	// (StaleCause stays comparable: the added/removed file names of
	// StaleMaterials are computed per lecture by InvalidateCourseWith.)
}

const (
	StaleGlossary = "glossary"
	StaleMaterial = "material"
	StaleSummary  = "summary"
)

// StaleReason formats the stale_reason written when job j becomes stale
// because of cause, as P14.3 JSON (model.StaleReasonJSON, phase 2 ruling
// 5). glossaryVersion is the course's current glossary_version. Glossary:
// glossary_updated {from: j.Stats.GlossaryVersion (absent when not
// recorded), to: glossaryVersion}; janitor (v2 G3.1): glossary_janitor with
// the same params; material: material_changed {filename} (no params without
// a name); materials (K1): materials_changed without params (the
// materials run of InvalidateCourseWith writes {added, removed} itself:
// materialsReason); summary: summary_updated; unknown kind, or a cause that
// does not feed j's stage: input_changed. Pure.
func StaleReason(cause StaleCause, j model.Job, glossaryVersion int) string {
	if !causeCovers(cause.Kind, j.Stage) {
		return reasonInput()
	}
	switch cause.Kind {
	case StaleGlossary:
		return model.StaleReasonJSON(model.StaleCodeGlossary, versionParams(j.Stats.GlossaryVersion, glossaryVersion))
	case StaleJanitor:
		return model.StaleReasonJSON(model.StaleCodeJanitor, versionParams(j.Stats.GlossaryVersion, glossaryVersion))
	case StaleMaterial:
		if cause.Material == "" {
			return model.StaleReasonJSON(model.StaleCodeMaterial, nil)
		}
		return model.StaleReasonJSON(model.StaleCodeMaterial, map[string]any{"filename": cause.Material})
	case StaleMaterials:
		return materialsReason(j, nil, nil)
	case StaleSummary:
		return model.StaleReasonJSON(model.StaleCodeSummary, nil)
	}
	return reasonInput()
}

// EarliestStale returns the first stale stage of a lecture in pipeline
// order ("" when none): the stage A2.3 "更新" reruns.
func EarliestStale(jobs []model.Job) string {
	if s := StaleStages(jobs); len(s) > 0 {
		return s[0]
	}
	return ""
}

// ---------- A4 snapshots and the rerun decision ----------

// ModelDiff is one stage whose snapshot differs from the current settings
// (A4.3 step 3).
type ModelDiff struct {
	LectureID string // filled by callers that aggregate several lectures
	Stage     string
	Snapshot  string // raw config_snapshot of the current job
	Current   string // raw snapshot of the current settings
	// SnapshotUsable is false when "沿用原模型" cannot be chosen (the
	// snapshot's provider was deleted or has no key); ReasonInfo says why
	// (v2 P14.3) and Reason is its Chinese detail (transitional).
	SnapshotUsable bool
	Reason         string
	ReasonInfo     *model.ErrorInfo
}

// NeedsModelChoiceError is returned when a rerun/update/retry needs the
// user's model choice (A4.3). The API answers 409
// {"error": "needs_model_choice", "diff": [...]}.
type NeedsModelChoiceError struct{ Diffs []ModelDiff }

func (e *NeedsModelChoiceError) Error() string { return model.ErrNeedsModelChoice }

// Is makes errors.Is(err, ErrConflict) true.
func (e *NeedsModelChoiceError) Is(target error) bool { return target == ErrConflict }

// UsableFunc reports whether a raw snapshot of stage can still be used
// (provider exists and has a key) and, if not, why: a coded reason whose
// Detail is the Chinese text (v2 P14.3). Implemented by
// settings.Service.SnapshotUsableInfo.
type UsableFunc func(stage, raw string) (ok bool, reason *model.ErrorInfo)

// SnapshotDiffs implements A4.3 step 1-3 for one lecture: compare the
// config_snapshot of stage and of every downstream stage (current jobs in
// jobs) with current[stage] using model.SameSnapshot; return one ModelDiff
// per stage that differs, in pipeline order (nil when all match).
// usable fills SnapshotUsable/Reason (nil = always usable). Pure.
func SnapshotDiffs(jobs []model.Job, stage string, current map[string]string, usable UsableFunc) []ModelDiff {
	order := StagesOf(model.ScopeOfStage(stage))
	i := indexOf(order, stage)
	if i < 0 {
		return nil
	}
	byStage := make(map[string]model.Job, len(jobs))
	for _, j := range jobs {
		byStage[j.Stage] = j
	}
	var out []ModelDiff
	for _, s := range order[i:] {
		j, ok := byStage[s]
		if !ok || model.SameSnapshot(s, j.ConfigSnapshot, current[s]) {
			continue
		}
		d := ModelDiff{Stage: s, Snapshot: j.ConfigSnapshot, Current: current[s], SnapshotUsable: true}
		if usable != nil {
			d.SnapshotUsable, d.ReasonInfo = usable(s, j.ConfigSnapshot)
			if d.ReasonInfo != nil {
				d.Reason = d.ReasonInfo.Detail
			}
		}
		out = append(out, d)
	}
	return out
}

// RerunOpts are the v1.1 options of a rerun.
type RerunOpts struct {
	// NewSnapshots, when non-nil, is the "使用当前设置" choice of A4.3: the
	// new job of every stage listed gets that snapshot. Stages not listed
	// (and every stage when nil) keep the config_snapshot of the job they
	// replace (A4.2: downstream keeps its own snapshot).
	NewSnapshots map[string]string
	// TextRepair (textrepair-int) is the asr rerun "修复断句" (text_only):
	// the lecture's sentences are kept for the new asr job to repair (it
	// replaces them in one transaction when it finishes, keeping the
	// human_text of the sentences it does not change); every downstream
	// stage is cleaned up and rerun as by a rerun of the stage after asr
	// (summary: summary and patches deleted, repaired_text reset,
	// translations cleared, text recomputed). The new asr job carries
	// model.ASRModeTextRepair and what describes the transcription it builds
	// on (textRepairStats); it keeps the asr snapshot (NewSnapshots must not
	// list asr). Only for the asr stage of a lecture.
	TextRepair bool
}

// textRepairStats is the stats of the new asr job of a "修复断句" rerun: its
// mode, and from the job it replaces what describes the transcription it
// repairs (the prompt and terms used, the glossary version, the input hash
// formula, the timing), so the stage card and the A2.2 / §4.4 checks see the
// same transcription.
func textRepairStats(old model.JobStats) model.JobStats {
	return model.JobStats{
		ASRMode: model.ASRModeTextRepair, PromptTermsUsed: old.PromptTermsUsed, ASRPrompt: old.ASRPrompt,
		GlossaryVersion: old.GlossaryVersion, HashVersion: old.HashVersion, ASRTiming: old.ASRTiming,
	}
}

// RerunWith is Rerun (SPEC §4.6, unchanged semantics) plus the v1.1 rule
// that every new pending job carries a config_snapshot: o.NewSnapshots[stage]
// when listed, otherwise the snapshot of the job it replaces with its
// prompt_version set to the current one (v2 G5.5, lead Gate 1: a stage
// always runs the current prompt, so "沿用原模型" keeps provider, model and
// params only). Rerun(...) is RerunWith(..., RerunOpts{}). A draft lecture → ErrDraft.
func RerunWith(ctx context.Context, st db.Store, dataDir, scope, scopeID, stage string, o RerunOpts) error {
	return rerun(ctx, st, dataDir, scope, scopeID, stage, o)
}

// RetryWith is Retry (SPEC §4.6: same job, attempt+1). When newSnaps is
// non-nil it first replaces the config_snapshot of the retried job and of
// every downstream current job listed in newSnaps (used when the
// snapshot's provider is gone: lead extension of A4.3, NOTES.md). nil keeps
// every snapshot (A4.2), except that the retried job's LLM snapshot gets the
// current prompt_version (v2 G5.5, lead Gate 1: the retry runs the current
// prompt; provider, model and params are kept).
//
// Only pending downstream jobs are re-snapshotted: below a failed or
// cancelled stage every downstream job is pending anyway, and a job that has
// run must keep the snapshot it ran with. When the translate snapshot is
// replaced, the lecture's translations are cleared in the same transaction
// (zh, zh_status=none, zh_hash, qa_error): the whole lecture is translated
// again by the new model and window, never a mix of two models or of two
// zh_hash conventions (A4.2, lead decision 6 addendum). The translate job
// then also loses an incremental stats.note: the run is a full translation.
//
// Power management (backend Gate round 1, fix 4): when the asr snapshot is
// replaced by one with another effective configuration (model.SameSnapshot
// false: device, backend, model, precision or language), the user changed
// the model, so the run must not continue from the old checkpoint: the
// asr job's stats.asr_resume and stats.power_event are cleared in the same
// transaction, and asr.ckpt.json (with its .tmp) is removed from the
// lecture's directory under dataDir after the commit (the Rerun precedent;
// the scheduler holds its scan lock meanwhile, so the job cannot start in
// between). A replacement with the same configuration keeps the checkpoint
// (e.g. "使用当前设置" chosen for another stage's sake).
func RetryWith(ctx context.Context, st db.Store, dataDir, scope, scopeID, stage string, newSnaps map[string]string) error {
	var lec model.Lecture
	dropCkpt := false
	err := st.Tx(ctx, func(tx db.Store) error {
		j, err := tx.GetCurrentJob(ctx, scope, scopeID, stage)
		if err != nil {
			return err
		}
		if j.Status != model.StatusFailed && j.Status != model.StatusCancelled {
			return fmt.Errorf("%w: %s is %s, retry needs failed or cancelled", ErrConflict, stage, j.Status)
		}
		replaced := map[string]bool{}
		// asrChanged: the asr job's snapshot gets another configuration.
		asrChanged := func(old, snap string) bool {
			return scope == model.ScopeLecture && !model.SameSnapshot(model.StageASR, old, snap)
		}
		if snap, ok := newSnaps[stage]; ok {
			if err := tx.SetJobSnapshot(ctx, j.ID, snap); err != nil {
				return err
			}
			replaced[stage] = true
			dropCkpt = stage == model.StageASR && asrChanged(j.ConfigSnapshot, snap)
		} else if snap := withPromptVersion(stage, j.ConfigSnapshot, model.PromptVersionOf(stage)); snap != j.ConfigSnapshot {
			// The retry keeps the model (A4.2) but runs the current prompt
			// (v2 G5.5, lead Gate 1): only prompt_version changes.
			if err := tx.SetJobSnapshot(ctx, j.ID, snap); err != nil {
				return err
			}
		}
		for _, d := range Downstream(stage) {
			snap, ok := newSnaps[d]
			if !ok {
				continue
			}
			dj, err := tx.GetCurrentJob(ctx, scope, scopeID, d)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					continue
				}
				return err
			}
			if dj.Status != model.StatusPending {
				continue
			}
			if err := tx.SetJobSnapshot(ctx, dj.ID, snap); err != nil {
				return err
			}
			replaced[d] = true
			if d == model.StageASR && asrChanged(dj.ConfigSnapshot, snap) {
				dropCkpt = true
			}
		}
		if dropCkpt {
			if lec, err = tx.GetLecture(ctx, scopeID); err != nil {
				return err
			}
			aj, err := tx.GetCurrentJob(ctx, scope, scopeID, model.StageASR)
			if err != nil {
				return err
			}
			if aj.Stats.ASRResume != nil || aj.Stats.PowerEvent != nil {
				stats := aj.Stats
				stats.ASRResume, stats.PowerEvent = nil, nil
				if err := tx.SetJobStats(ctx, aj.ID, stats); err != nil {
					return err
				}
			}
		}
		if scope == model.ScopeLecture && replaced[model.StageTranslate] {
			if err := tx.ClearTranslations(ctx, scopeID); err != nil {
				return err
			}
			// With every translation cleared the run is a full translation,
			// not an incremental one: drop the incremental marker and its base
			// hash, which the stage would compare with the prefix hash of the
			// new window and fail permanently on every retry. The models of
			// the cleared translations (stats.mixed_models, v3.2.x P1-b) go
			// with them.
			tr, err := tx.GetCurrentJob(ctx, scope, scopeID, model.StageTranslate)
			if err != nil {
				return err
			}
			if IsIncremental(tr.Stats.Note) || tr.Stats.MixedModels != nil {
				stats := tr.Stats
				stats.Note, stats.MixedModels = "", nil
				if err := tx.SetJobStats(ctx, tr.ID, stats); err != nil {
					return err
				}
			}
		}
		return requeue(ctx, tx, j, true)
	})
	if err != nil {
		return err
	}
	if dropCkpt && dataDir != "" {
		if LectureFilesOK(dataDir, lec) {
			removeASRCheckpoint(model.LectureDir(dataDir, lec.CourseID, lec.ID), lec.ID)
		} else {
			slog.Warn("retry: the lecture's directory is not one of its own (its ids, or a link on the way); the asr checkpoint is not removed")
		}
	}
	return nil
}

// LectureFilesOK reports whether the lecture's directory under the data
// directory dataDir, built from its course and lecture ids as the database
// holds them (an imported library supplies them), is a directory of its own
// the app may read, write and remove files in: both ids one path element
// (media.IDElement: no separator, no trailing dot or space, no device name;
// R77b "not covered", SEC44 low-2) and no link at any folder from below
// dataDir down to it (media.PlainPath; SEC49 L1). The rerun's and the
// retry's file cleanup and the scheduler's look at the audio before a run
// use it; a lecture that fails it keeps every file.
func LectureFilesOK(dataDir string, l model.Lecture) bool {
	return media.IDElement(l.CourseID) && media.IDElement(l.ID) &&
		media.PlainPath(dataDir, model.LectureDir(dataDir, l.CourseID, l.ID))
}

// removeASRCheckpoint removes a lecture's asr checkpoint (model
// ASRCheckpointFile and its .tmp); a missing file is fine, another error
// is logged.
func removeASRCheckpoint(dir, lectureID string) {
	for _, name := range []string{model.ASRCheckpointFile, model.ASRCheckpointFile + ".tmp"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			slog.Warn("retry: remove asr checkpoint", "lecture", lectureID, "file", name, "err", err)
		}
	}
}

// TranslateWindow returns the context window x of the lecture's current
// translate job snapshot (model.SnapshotWindow): the x of the sentence-level
// invalidation of review operations (SPEC §7.3) must be the x the lecture
// was translated with, not the current setting.
func TranslateWindow(ctx context.Context, st db.Store, lectureID string) (int, error) {
	j, err := st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageTranslate)
	if err != nil {
		return 0, err
	}
	return model.SnapshotWindow(j.ConfigSnapshot), nil
}

// deferredLLMSnapshots: a lecture started with no provider for any LLM
// role (“只转写”, 3.1.0; NOTES “决定（3.1.0 只转写…）”): its asr job has a
// snapshot, the summary, repair and translate jobs have "{}" until the
// roles are configured. A lecture started by v1 has no snapshot at all, so
// BackfillSnapshots leaves the former alone: its LLM snapshots are written
// with the current prompt versions when they run (the scheduler).
func deferredLLMSnapshots(jobs []model.Job) bool {
	asr, llm := false, 0
	for _, j := range jobs {
		switch j.Stage {
		case model.StageASR:
			asr = !model.IsEmptySnapshot(j.ConfigSnapshot)
		case model.StageSummary, model.StageRepair, model.StageTranslate:
			if !model.IsEmptySnapshot(j.ConfigSnapshot) {
				return false
			}
			llm++
		}
	}
	return asr && llm == 3
}

// BackfillSnapshots is the one-time v1.1 migration of lectures started by
// v1 (lead, NOTES.md): every CURRENT job of a launched lecture whose
// config_snapshot is empty gets current[stage] (stages not in current keep
// "{}"), LLM snapshots without a prompt_version: those lectures ran with
// the v1 prompts (v2 lead Gate 1). Guarded by kv model.KVSnapshotsBackfill:
// the second call does nothing. Returns the number of jobs updated.
func BackfillSnapshots(ctx context.Context, st db.Store, current map[string]string) (int, error) {
	n := 0
	err := st.Tx(ctx, func(tx db.Store) error {
		if _, done, err := tx.GetKV(ctx, model.KVSnapshotsBackfill); err != nil || done {
			return err
		}
		courses, err := tx.ListCourses(ctx)
		if err != nil {
			return err
		}
		for _, c := range courses {
			lecs, err := tx.ListLectures(ctx, c.ID)
			if err != nil {
				return err
			}
			for _, l := range lecs {
				if IsDraft(l) {
					continue
				}
				jobs, err := tx.ListCurrentJobs(ctx, model.ScopeLecture, l.ID)
				if err != nil {
					return err
				}
				if deferredLLMSnapshots(jobs) {
					continue // started by 3.1.0 without providers, not by v1
				}
				for _, j := range jobs {
					snap := current[j.Stage]
					if !model.IsEmptySnapshot(j.ConfigSnapshot) || model.IsEmptySnapshot(snap) {
						continue
					}
					snap = withPromptVersion(j.Stage, snap, 0) // v1 prompts
					if err := tx.SetJobSnapshot(ctx, j.ID, snap); err != nil {
						return err
					}
					n++
				}
			}
		}
		return tx.SetKV(ctx, model.KVSnapshotsBackfill, "1")
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ---------- A2.2 invalidation with a cause ----------

// InvalidateCourseWith is InvalidateCourse (SPEC §4.4) where every job that
// becomes stale gets stale_reason = StaleReason(cause, job, course
// glossary_version) in the same done → stale transition. A glossary cause
// also rewrites the glossary reason of jobs already stale for the glossary
// (status untouched); those jobs are returned too, so the change is announced.
// InvalidateCourse(...) is InvalidateCourseWith(..., StaleCause{}).
//
// Cause StaleMaterials (lecture kinds, K1) runs the material-set comparison
// instead (see StaleMaterials): only lectures whose material set changed
// since the last such run are checked, with materials_changed {added,
// removed}; materials and course jobs are not checked.
func InvalidateCourseWith(ctx context.Context, st db.Store, hashers map[string]Hasher, courseID string, cause StaleCause) ([]model.Job, error) {
	if cause.Kind == StaleMaterials {
		return invalidateMaterialSets(ctx, st, hashers, courseID)
	}
	reason, err := causeReason(ctx, st, courseID, cause)
	if err != nil {
		return nil, err
	}
	return invalidateCourse(ctx, st, hashers, courseID, reason, causeRefresher(courseID, cause))
}

// causeRefresher: a glossary or janitor cause also refreshes the glossary
// reason of jobs that are already stale ("v1 → v4" after later edits); other
// causes only write a reason with done → stale.
func causeRefresher(courseID string, cause StaleCause) *refresher {
	if isGlossaryKind(cause.Kind) {
		return &refresher{courseID: courseID}
	}
	return nil
}

// CheckStaleWith is CheckStale for one lecture with a cause.
func CheckStaleWith(ctx context.Context, st db.Store, hashers map[string]Hasher, lectureID string, cause StaleCause) ([]model.Job, error) {
	reason := reasonFunc(func(j model.Job) string { return StaleReason(cause, j, 0) })
	var rf *refresher
	if isGlossaryKind(cause.Kind) { // only the glossary reasons need the course's version
		lec, err := st.GetLecture(ctx, lectureID)
		if err != nil {
			return nil, err
		}
		if reason, err = causeReason(ctx, st, lec.CourseID, cause); err != nil {
			return nil, err
		}
		rf = causeRefresher(lec.CourseID, cause)
	}
	return checkStaleScope(ctx, st, hashers, model.ScopeLecture, lectureID, reason, rf)
}
