// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// v1.1 scheduler operations (SPEC-v1.1 A2.3, A3, A4.3, A7). Signatures FROZEN
// by the lead; bodies: core-builder (TEST FIRST).
//
// Wiring: Options.Settings (*settings.Service) is the source of the current
// settings. When nil, New opens one on Options.Store with Options.Config
// (settings.Open: legacy import + load), so tests that only set Config keep
// working; legacy providers keep their config key as id ("deepseek").
//
// Rules the implementation must keep:
//   - drafts (launched_at IS NULL) never leave pending; WaitingFor reports
//     model.WaitNotStarted for every job of a draft (state.Deps does the check);
//   - API stages acquire the pool of the provider of the JOB'S SNAPSHOT
//     (lecture jobs) or of the role's current setting (course/material jobs);
//   - GPU admission uses settings.ASR() (max_parallel, vram_reserve_mb,
//     vram_per_job_mb) read at scan time, and the model/compute_type of the
//     job's asr snapshot for the VRAM calibration key;
//   - every state change below emits the usual SSE events and kicks the loop.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// ErrDeleteTimeout: a forced delete cancelled the lecture's running jobs but
// they did not stop within DeleteWait (A7: 15 s). errors.Is(err,
// state.ErrConflict) is true (409).
var ErrDeleteTimeout = fmt.Errorf("讲座的任务没有在 15 秒内停止，未删除: %w", state.ErrConflict)

// DefaultDeleteWait is how long a forced delete waits for running jobs (A7).
const DefaultDeleteWait = 15 * time.Second

// Launch implements A3 "启动" for one draft: current snapshots from
// LaunchSnapshots (settings.CurrentSnapshots; with no lecture role
// configured, SPEC-v3 V4.2 "只转写", empty LLM snapshots that are recorded
// when those stages can start, transcribe_only.go; ErrRoleNotConfigured is
// returned as is), then state.Launch. Errors: db.ErrNotFound,
// state.ErrAlreadyLaunched, settings.ErrRoleNotConfigured.
func (s *Scheduler) Launch(ctx context.Context, lectureID string) error {
	l, err := s.st.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	if !state.IsDraft(l) {
		return state.ErrAlreadyLaunched
	}
	snaps, err := s.LaunchSnapshots()
	if err != nil {
		return err
	}
	if err := state.Launch(ctx, s.st, lectureID, snaps); err != nil {
		return err
	}
	slog.Info("lecture launched", "lecture", lectureID)
	s.emitScope(ctx, model.ScopeLecture, lectureID)
	s.ev.Changed("course", l.CourseID) // the course's draft count changed
	s.Kick()
	return nil
}

// ---------- A4.3 helpers ----------

func checkChoice(choice string) error {
	switch choice {
	case "", model.ModelChoiceSnapshot, model.ModelChoiceCurrent:
		return nil
	case model.ModelChoiceManual:
		// v3.2.x P1-b: only a retry takes it, with its target (RetryManual).
		return fmt.Errorf("%w: model_choice %q needs a model and is only for the retry of summary, repair or translate", state.ErrBadInput, choice)
	}
	return fmt.Errorf("%w: unknown model_choice %q", state.ErrBadInput, choice)
}

// withDownstream returns stage followed by its downstream stages.
func withDownstream(stage string) []string {
	return append([]string{stage}, state.Downstream(stage)...)
}

func isSnapshotStage(stage string) bool {
	for _, s := range model.SnapshotStages {
		if s == stage {
			return true
		}
	}
	return false
}

// usable is settings.SnapshotUsable (provider exists and has a key) plus: a
// snapshot stage without any snapshot cannot "沿用原模型" — such a stage
// fails permanently (decision 4), so only "使用当前设置" can help. With
// Options.Devices an asr snapshot is usable per DeviceSource.Usable
// (SPEC-v2 I6, lead ruling 2: a fixed device must exist, be enabled and
// present; auto needs some enabled, present device).
func (s *Scheduler) usable(stage, raw string) (bool, *model.ErrorInfo) {
	if isSnapshotStage(stage) && model.IsEmptySnapshot(raw) {
		return false, &model.ErrorInfo{Code: settings.CodeSnapshotNotRecorded, Detail: "该阶段没有模型配置快照"}
	}
	if stage == model.StageASR && s.devs != nil {
		if snap, ok := model.ParseASRSnapshot(raw); ok {
			return s.devs.Usable(model.NormalizeASRSnapshot(snap))
		}
	}
	return s.set.SnapshotUsableInfo(stage, raw)
}

// pinned is the "沿用原模型" choice for the asr job among stages (lead
// ruling 2): its auto snapshot fixed to the device it last ran on
// (model.PinASRSnapshot). nil when there is nothing to change, so the
// caller keeps every snapshot as before — also when that device cannot run
// any more (a retry then keeps the auto snapshot and runs elsewhere; a
// rerun has refused the choice before, see autoASRCompare).
func (s *Scheduler) pinned(jobs []model.Job, stages []string) map[string]string {
	for _, st := range stages {
		if st != model.StageASR {
			continue
		}
		j := findJob(jobs, st)
		if j == nil {
			continue
		}
		p := model.PinASRSnapshot(j.ConfigSnapshot)
		if p == j.ConfigSnapshot {
			continue
		}
		if s.devs != nil {
			if snap, ok := model.ParseASRSnapshot(p); ok {
				if usable, _ := s.devs.Usable(model.NormalizeASRSnapshot(snap)); !usable {
					return nil
				}
			}
		}
		return map[string]string{model.StageASR: p}
	}
	return nil
}

// asrCompare is how the rerun decision compares an auto-mode asr snapshot
// (lead ruling 2, refined 2026-10-24): the device the scheduler picked is
// not the user's change.
type asrCompare struct {
	applies bool
	current string // the raw "current" the snapshot is compared with (and the dialog shows)
	force   bool   // a difference even when the effective configurations match
	// usable / reason: "沿用原模型" fixes the snapshot to its bound device,
	// so it is available only while that device exists, is enabled and present.
	usable bool
	reason *model.ErrorInfo
}

// autoASRCompare: for an auto snapshot (Auto true), "current" is the bound
// device's effective configuration now (its row's model and precision,
// the settings' language), so only a change of that device's model or
// precision, or of the language, asks. The bound device missing, disabled
// or not present asks (current = the settings' first device; "沿用原模型"
// unavailable). Device mode manual (power management; lead ruling 9.2 and
// decision A, 2026-10-24: A4.3 compares effective configurations only): a
// bound device of the manual list compares as in auto mode (the scheduler
// chose among the user's devices); a bound device outside the list
// compares with CurrentASR, the list's effective device — so it asks when
// that device, model or precision differs, and not when the run already
// had that configuration. A fixed or v1.1 snapshot, or no device registry:
// not applicable (compared as before).
func (s *Scheduler) autoASRCompare(j *model.Job, current string) asrCompare {
	var c asrCompare
	if s.devs == nil || j == nil {
		return c
	}
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	if !ok || !snap.Auto {
		return c
	}
	snap = model.NormalizeASRSnapshot(snap)
	c.applies, c.current = true, current
	bound := snap
	bound.Auto = false
	c.usable, c.reason = s.devs.Usable(bound)
	a := model.NormalizeASRDeviceMode(s.set.ASR())
	if !c.usable {
		c.force = true
		return c
	}
	if a.DeviceMode == model.ASRDeviceModeManual && !slices.Contains(a.ManualDevices, snap.DeviceID) {
		return c // current: the manual list's effective device (CurrentASR)
	}
	row, _ := s.devs.Get(snap.DeviceID)
	c.current = model.MarshalSnapshot(model.ASRDeviceSnapshot(row, a.Language, true))
	return c
}

// currentSnapshots returns the snapshot the current settings give each of
// stages, in the format launch writes (settings.CurrentSnapshots). When some
// role elsewhere in the pipeline is not configured, only the listed stages
// are resolved; an error (settings.ErrRoleNotConfigured) means the role of
// one of the listed stages is not configured. deferred (a "只转写" lecture
// whose LLM snapshots are still to be recorded, transcribe_only.go): the
// LLM stages get the empty snapshot, they are recorded when they start.
func (s *Scheduler) currentSnapshots(stages []string, deferred bool) (map[string]string, error) {
	all, err := s.set.CurrentSnapshots()
	out := make(map[string]string, len(stages))
	for _, st := range stages {
		if deferred && isLectureLLMStage(st) {
			out[st] = model.EmptySnapshot
			continue
		}
		if err == nil {
			out[st] = all[st]
			continue
		}
		switch st {
		case model.StageSummary, model.StageRepair, model.StageTranslate:
			t, rerr := s.set.SnapshotTarget(model.RoleOfStage(st)) // translate: the provider's default batch
			if rerr != nil {
				return nil, rerr
			}
			out[st] = model.MarshalSnapshot(t)
		case model.StageASR:
			out[st] = s.set.CurrentASR() // SPEC-v2 I6: the device registry's when one is set
		default:
			out[st] = model.EmptySnapshot
		}
	}
	return out, nil
}

// diffs is state.SnapshotDiffs with the lecture id filled in, and an auto
// asr snapshot compared per autoASRCompare. "使用当前设置" still writes
// current (the settings' snapshot), not the comparison value.
func (s *Scheduler) diffs(lectureID string, jobs []model.Job, stage string, current map[string]string) []state.ModelDiff {
	asrJob := findJob(jobs, model.StageASR)
	c := s.autoASRCompare(asrJob, current[model.StageASR])
	cmp := current
	if c.applies {
		cmp = maps.Clone(current)
		cmp[model.StageASR] = c.current
	}
	ds := state.SnapshotDiffs(jobs, stage, cmp, s.usable)
	if c.applies && slices.Contains(withDownstream(stage), model.StageASR) {
		found := false
		for i := range ds {
			if ds[i].Stage == model.StageASR {
				found = true
				ds[i].SnapshotUsable, ds[i].ReasonInfo, ds[i].Reason = c.usable, c.reason, ""
				if c.reason != nil {
					ds[i].Reason = c.reason.Detail
				}
			}
		}
		if !found && c.force {
			d := state.ModelDiff{Stage: model.StageASR, Snapshot: asrJob.ConfigSnapshot, Current: c.current,
				SnapshotUsable: c.usable, ReasonInfo: c.reason}
			if c.reason != nil {
				d.Reason = c.reason.Detail
			}
			// asr comes first: every other stage with a snapshot is downstream of it.
			ds = append([]state.ModelDiff{d}, ds...)
		}
	}
	for i := range ds {
		ds[i].LectureID = lectureID
	}
	return ds
}

// unusable lists the jobs of stages whose snapshot cannot be used.
func (s *Scheduler) unusable(lectureID string, jobs []model.Job, stages []string, current map[string]string) []state.ModelDiff {
	var out []state.ModelDiff
	for _, st := range stages {
		for _, j := range jobs {
			if j.Stage != st {
				continue
			}
			if ok, reason := s.usable(st, j.ConfigSnapshot); !ok {
				d := state.ModelDiff{LectureID: lectureID, Stage: st, Snapshot: j.ConfigSnapshot,
					Current: current[st], SnapshotUsable: false, ReasonInfo: reason}
				if reason != nil {
					d.Reason = reason.Detail
				}
				out = append(out, d)
			}
		}
	}
	return out
}

func snapshotUnusable(bad []state.ModelDiff) error {
	var parts []string
	for _, d := range bad {
		parts = append(parts, model.StageLabel(d.Stage)+"："+d.Reason)
	}
	return fmt.Errorf("%w（%s）", state.ErrSnapshotUnusable, strings.Join(parts, "；"))
}

// currentUnusable checks "使用当前设置" before any snapshot is replaced
// (v3.2.x P1-b): the current setting of every LLM stage of stages must be
// usable (settings.SnapshotUsableInfo: its provider exists and has a key —
// the check a model named by hand gets too). The first one that is not is
// returned as a coded error with that reason's code (snapshot.unusable.*)
// wrapping state.ErrSnapshotUnusable (a conflict); nil when all are
// usable. stages excludes the LLM stages of a "只转写" lecture (their
// settings are taken when they start); asr and the other stages are not
// checked here.
func (s *Scheduler) currentUnusable(stages []string, cur map[string]string) error {
	for _, st := range stages {
		if !isLectureLLMStage(st) {
			continue
		}
		if ok, reason := s.set.SnapshotUsableInfo(st, cur[st]); !ok && reason != nil {
			info := *reason
			return &model.CodedError{Info: info,
				Err: fmt.Errorf("%w（%s：%s）", state.ErrSnapshotUnusable, model.StageLabel(st), info.Detail)}
		}
	}
	return nil
}

func findJob(jobs []model.Job, stage string) *model.Job {
	for i := range jobs {
		if jobs[i].Stage == stage {
			return &jobs[i]
		}
	}
	return nil
}

// rerunnable mirrors the preconditions of state.RerunWith so a rerun that
// would be refused anyway is refused before the model question is asked.
func rerunnable(jobs []model.Job, stage string) error {
	self := findJob(jobs, stage)
	if self == nil {
		return fmt.Errorf("%w: no current %s job", state.ErrConflict, stage)
	}
	if self.Status != model.StatusDone && self.Status != model.StatusStale {
		return fmt.Errorf("%w: %s is %s, rerun needs done or stale", state.ErrConflict, stage, self.Status)
	}
	for _, st := range withDownstream(stage) {
		if j := findJob(jobs, st); j != nil && j.Status == model.StatusRunning {
			return fmt.Errorf("%w (%s)", state.ErrBusy, st)
		}
	}
	return nil
}

// rerunSnapshots resolves the A4.3 decision for a rerun of stage of one
// lecture: the NewSnapshots of state.RerunOpts (nil = keep every snapshot).
func (s *Scheduler) rerunSnapshots(lectureID string, jobs []model.Job, stage, choice string) (map[string]string, error) {
	stages := withDownstream(stage)
	deferred := deferredLLM(jobs)
	cur, curErr := s.currentSnapshots(stages, deferred)
	if deferred {
		// "只转写": no recorded model to keep or to compare for the LLM
		// stages (their empty snapshots equal cur's); they take the
		// settings when they start (transcribe_only.go).
		stages = withoutLLM(stages)
	}
	switch choice {
	case model.ModelChoiceCurrent:
		if curErr != nil {
			return nil, curErr
		}
		if err := s.currentUnusable(stages, cur); err != nil {
			return nil, err
		}
		return cur, nil
	case model.ModelChoiceSnapshot:
		var bad []state.ModelDiff
		if curErr != nil {
			bad = s.unusable(lectureID, jobs, stages, nil)
		} else {
			for _, d := range s.diffs(lectureID, jobs, stage, cur) {
				if !d.SnapshotUsable {
					bad = append(bad, d)
				}
			}
		}
		if len(bad) > 0 {
			return nil, snapshotUnusable(bad)
		}
		return s.pinned(jobs, stages), nil
	}
	if curErr != nil {
		// No current settings to offer for these stages: keep the snapshots
		// when they can be used, otherwise the settings must be fixed first.
		if len(s.unusable(lectureID, jobs, stages, nil)) > 0 {
			return nil, curErr
		}
		return nil, nil
	}
	if ds := s.diffs(lectureID, jobs, stage, cur); len(ds) > 0 {
		return nil, &state.NeedsModelChoiceError{Diffs: ds}
	}
	return nil, nil
}

// Rerun is the v1.1 rerun of one stage with the A4.3 decision:
//   - choice "": compare the snapshots of stage and its downstream with the
//     current settings (state.SnapshotDiffs with settings.SnapshotUsable);
//     any difference → *state.NeedsModelChoiceError; none → rerun keeping
//     every snapshot;
//   - model.ModelChoiceSnapshot: rerun keeping every snapshot; when a
//     differing stage's snapshot is unusable → state.ErrSnapshotUnusable;
//   - model.ModelChoiceCurrent: rerun with NewSnapshots = current settings
//     for stage and every downstream stage.
//
// Holds the scan lock like DoAction(rerun). Course/material scopes have no
// snapshots: choice is ignored. DoAction(rerun) == Rerun(..., "").
func (s *Scheduler) Rerun(ctx context.Context, scope, scopeID, stage, choice string) error {
	if err := checkChoice(choice); err != nil {
		return err
	}
	if model.IsAuxStage(stage) {
		// v2 ruling 1: retry or EnqueueAux, never a rerun.
		return fmt.Errorf("%w: %s is an auxiliary task and has no rerun", state.ErrConflict, stage)
	}
	var o state.RerunOpts
	if scope == model.ScopeLecture && model.ScopeOfStage(stage) == scope {
		l, err := s.st.GetLecture(ctx, scopeID)
		if err != nil {
			return err
		}
		if state.IsDraft(l) {
			return state.ErrDraft
		}
		jobs, err := s.st.ListCurrentJobs(ctx, scope, scopeID)
		if err != nil {
			return err
		}
		if err := rerunnable(jobs, stage); err != nil {
			return err
		}
		if o.NewSnapshots, err = s.rerunSnapshots(scopeID, jobs, stage, choice); err != nil {
			return err
		}
	}
	s.scanMu.Lock()
	err := state.RerunWith(ctx, s.st, s.dataDir, scope, scopeID, stage, o)
	s.scanMu.Unlock()
	if err != nil {
		return err
	}
	if scope == model.ScopeLecture {
		s.ev.Changed("sentences", scopeID)
		s.ev.Changed("patches", scopeID)
	}
	s.emitScope(ctx, scope, scopeID)
	s.Kick()
	return nil
}

// Retry is the user retry with the lead extension of A4.3 (NOTES.md) and
// the v3.2.x P1-b rule (Q9a):
// choice "" → *state.NeedsModelChoiceError when the retried job's snapshot
// (or a downstream pending job's) is unusable (those stages are listed with
// SnapshotUsable=false), or — retry of summary, repair or translate only —
// when the snapshot of the retried stage or of a pending downstream stage
// differs from the current settings (retryDiffs); otherwise
// state.RetryWith(nil). The retry of any other stage (asr, prep, export)
// never asks because of a difference.
// ModelChoiceCurrent → RetryWith(current settings for stage and downstream).
// ModelChoiceSnapshot → RetryWith(nil), or ErrSnapshotUnusable when unusable.
// ModelChoiceManual is RetryManual's (ErrBadInput here: it needs a target).
// DoAction(retry) == Retry(..., "").
func (s *Scheduler) Retry(ctx context.Context, scope, scopeID, stage, choice string) error {
	if err := checkChoice(choice); err != nil {
		return err
	}
	return s.retry(ctx, scope, scopeID, stage, choice, nil)
}

// RetryManual is the retry with model choice manual (v3.2.x P1-b): the
// failed or cancelled job of stage (summary, repair or translate of a
// lecture) is retried with the model the user names for this one retry.
// Only that job's snapshot is replaced (Source manual; a translate job
// keeps its context window and batch size); downstream jobs keep theirs
// and the role's setting is not written. The target is validated like the
// settings page does (settings.ManualSnapshot; errors.Is settings.ErrInvalid).
// As with ModelChoiceCurrent, replacing the translate snapshot clears the
// lecture's translations: the whole lecture is translated by the new model.
// Any other stage or scope → state.ErrBadInput.
func (s *Scheduler) RetryManual(ctx context.Context, scope, scopeID, stage string, target settings.ManualTarget) error {
	if scope != model.ScopeLecture || !isLectureLLMStage(stage) {
		return fmt.Errorf("%w: a model can only be named for the retry of summary, repair or translate", state.ErrBadInput)
	}
	return s.retry(ctx, scope, scopeID, stage, model.ModelChoiceManual, &target)
}

// retryDiffs lists what a retry without a choice has to ask about, in
// pipeline order: every stage of check whose snapshot is unusable, and —
// when the retried stage is an LLM stage and cur is known — every LLM stage
// of check whose snapshot differs from the current settings. The prompt
// version is not part of that comparison: a retry always runs the current
// prompts (state.RetryWith, state.RecordPromptVersion), whatever is chosen.
func (s *Scheduler) retryDiffs(lectureID string, jobs []model.Job, stage string, check []string, cur map[string]string) (diffs []state.ModelDiff, unusable bool) {
	compare := isLectureLLMStage(stage) && cur != nil
	for _, st := range check {
		j := findJob(jobs, st)
		if j == nil {
			continue
		}
		ok, reason := s.usable(st, j.ConfigSnapshot)
		differs := compare && isLectureLLMStage(st) && !sameModel(st, j.ConfigSnapshot, cur[st])
		if ok && !differs {
			continue
		}
		d := state.ModelDiff{LectureID: lectureID, Stage: st, Snapshot: j.ConfigSnapshot, Current: cur[st],
			SnapshotUsable: ok, ReasonInfo: reason}
		if reason != nil {
			d.Reason = reason.Detail
		}
		unusable = unusable || !ok
		diffs = append(diffs, d)
	}
	return diffs, unusable
}

// sameModel is model.SameSnapshot for two LLM snapshots of stage without
// the prompt version: provider, model and params only.
func sameModel(stage, a, b string) bool {
	sa, okA := model.ParseLLMSnapshot(a)
	sb, okB := model.ParseLLMSnapshot(b)
	if !okA || !okB {
		return model.SameSnapshot(stage, a, b)
	}
	sa.PromptVersion, sb.PromptVersion = 0, 0
	return model.SameSnapshot(stage, model.MarshalSnapshot(sa), model.MarshalSnapshot(sb))
}

func (s *Scheduler) retry(ctx context.Context, scope, scopeID, stage, choice string, manual *settings.ManualTarget) error {
	var newSnaps map[string]string
	// An auxiliary task (v2) has no snapshot: its retry asks nothing.
	if scope == model.ScopeLecture && model.ScopeOfStage(stage) == scope && !model.IsAuxStage(stage) {
		jobs, err := s.st.ListCurrentJobs(ctx, scope, scopeID)
		if err != nil {
			return err
		}
		self := findJob(jobs, stage)
		if self == nil {
			if _, err := s.st.GetLecture(ctx, scopeID); err != nil {
				return err
			}
			return fmt.Errorf("%w: no current %s job", db.ErrNotFound, stage)
		}
		if self.Status != model.StatusFailed && self.Status != model.StatusCancelled {
			return fmt.Errorf("%w: %s is %s, retry needs failed or cancelled", state.ErrConflict, stage, self.Status)
		}
		// The retried job and the downstream jobs that will run after it
		// (pending ones: RetryWith re-snapshots only those).
		check := []string{stage}
		for _, d := range state.Downstream(stage) {
			if j := findJob(jobs, d); j != nil && j.Status == model.StatusPending {
				check = append(check, d)
			}
		}
		deferred := deferredLLM(jobs)
		if deferred {
			check = withoutLLM(check) // "只转写": recorded when they start (transcribe_only.go)
		}
		cur, curErr := s.currentSnapshots(withDownstream(stage), deferred)
		switch choice {
		case "":
			var known map[string]string
			if curErr == nil {
				known = cur
			}
			if ds, bad := s.retryDiffs(scopeID, jobs, stage, check, known); len(ds) > 0 {
				if bad && curErr != nil {
					return curErr // neither choice could work: the settings must be fixed
				}
				slog.Info("retry asks for the model choice", "lecture", scopeID, "stage", stage, "stages", len(ds), "unusable", bad)
				return &state.NeedsModelChoiceError{Diffs: ds}
			}
		case model.ModelChoiceSnapshot:
			if bad := s.unusable(scopeID, jobs, check, cur); len(bad) > 0 {
				return snapshotUnusable(bad)
			}
			// lead ruling 2: an auto asr snapshot is fixed to its device.
			// Only pending downstream jobs are re-snapshotted (RetryWith).
			newSnaps = s.pinned(jobs, check)
		case model.ModelChoiceCurrent:
			if curErr != nil {
				return curErr
			}
			// only the snapshots RetryWith replaces: the retried job's and
			// the pending downstream jobs' (check)
			if err := s.currentUnusable(check, cur); err != nil {
				return err
			}
			newSnaps = cur
		case model.ModelChoiceManual:
			if manual == nil {
				return fmt.Errorf("%w: model_choice manual needs a model", state.ErrBadInput)
			}
			snap, err := s.set.ManualSnapshot(stage, *manual, self.ConfigSnapshot)
			if err != nil {
				return err
			}
			newSnaps = map[string]string{stage: model.MarshalSnapshot(snap)}
		}
	} else if choice == model.ModelChoiceManual {
		return fmt.Errorf("%w: %s has no model to choose", state.ErrBadInput, stage)
	}
	// Power management (backend Gate round 1, fix 4): a new asr snapshot
	// may drop the lecture's checkpoint file; the scan lock keeps the job
	// from starting before the file is gone (the Rerun precedent).
	_, asrSnap := newSnaps[model.StageASR]
	if asrSnap {
		s.scanMu.Lock()
	}
	err := state.RetryWith(ctx, s.st, s.dataDir, scope, scopeID, stage, newSnaps)
	if asrSnap {
		s.scanMu.Unlock()
	}
	if err != nil {
		return err
	}
	if choice != "" {
		// No key and no lecture content: ids and the model name only.
		attrs := []any{"scope_id", scopeID, "stage", stage, "choice", choice}
		if snap, ok := model.ParseLLMSnapshot(newSnaps[stage]); ok && choice == model.ModelChoiceManual {
			attrs = append(attrs, "provider", snap.ProviderID, "model", snap.Model)
		}
		slog.Info("retry with a model choice", attrs...)
	}
	if _, ok := newSnaps[model.StageTranslate]; ok {
		s.ev.Changed("sentences", scopeID) // RetryWith cleared the translations
	}
	s.emitScope(ctx, scope, scopeID)
	s.Kick()
	return nil
}

// RetranslateSentence is the sentence operation "重新翻译这句" through the
// scheduler (v3.2.x P1-b, Q10a). useCurrent false: state.RetranslateSentence.
// useCurrent true: the sentence (one the model left without a usable
// translation) is sent to the model of the translate role's CURRENT
// setting; the lecture's translate job takes that snapshot without its
// other translations being cleared (state.RetranslateSentenceWith), so the
// incremental run — and the later ones — use it. mixed reports whether the
// job's model changed that way. The debounced incremental translate is
// requested in both cases. Errors: settings.ErrRoleNotConfigured (also when
// the current setting cannot be used: no key, an unreadable key — coded
// snapshot.unusable.*; nothing is changed), state.ErrBusy,
// state.ErrConflict, db.ErrNotFound.
func (s *Scheduler) RetranslateSentence(ctx context.Context, lectureID string, idx int, useCurrent bool) (sent model.Sentence, mixed bool, err error) {
	var o state.RetranslateOpts
	if useCurrent {
		t, err := s.set.SnapshotTarget(model.RoleTranslate) // the lecture's snapshot, if it changes (mixTranslateSnapshot)
		if err != nil {
			return model.Sentence{}, false, err
		}
		t.PromptVersion = model.PromptVersionOf(model.StageTranslate)
		o.Snapshot = model.MarshalSnapshot(t)
		// The setting must be usable before the lecture's snapshot is
		// switched to it (R2 issue 6): a provider without a key (not a local
		// server) or with a key that cannot be read here would only leave
		// the lecture on a model that fails.
		if ok, reason := s.set.SnapshotUsableInfo(model.StageTranslate, o.Snapshot); !ok {
			return model.Sentence{}, false, currentUnusable(reason)
		}
	}
	sent, mixed, err = state.RetranslateSentenceWith(ctx, s.st, lectureID, idx, o)
	if err != nil {
		return model.Sentence{}, false, err
	}
	if mixed {
		slog.Info("translate model switched for the sentences still to translate; the other translations are kept",
			"lecture", lectureID, "sentence", idx)
		s.emitCurrent(ctx, model.ScopeLecture, lectureID, model.StageTranslate)
	}
	s.RequestIncrementalTranslate(lectureID)
	return sent, mixed, nil
}

// currentUnusable is the error for a role's current setting that cannot be
// used (settings.SnapshotUsableInfo's reason): coded with that reason's code
// and params (snapshot.unusable.*, as RetryManual reports them), and
// errors.Is settings.ErrRoleNotConfigured (409: the settings must be fixed).
func currentUnusable(reason *model.ErrorInfo) error {
	info := model.ErrorInfo{Code: settings.CodeSnapshotInvalid}
	detail := "当前设置不可用"
	if reason != nil {
		info = model.ErrorInfo{Code: reason.Code, Params: reason.Params}
		detail = reason.Detail
	}
	return &model.CodedError{Info: info, Err: fmt.Errorf("%w: %s", settings.ErrRoleNotConfigured, detail)}
}

// UpdateResult is the per-lecture result of UpdateLectures.
type UpdateResult struct {
	LectureID string
	Stage     string // the stage that was rerun ("" when nothing was stale)
	Err       error
}

// UpdateLecture implements A2.3 "更新" for one lecture: Rerun(earliest stale
// stage, choice). Returns the rerun stage ("" with nil error when nothing is
// stale). A draft → state.ErrDraft.
func (s *Scheduler) UpdateLecture(ctx context.Context, lectureID, choice string) (string, error) {
	l, err := s.st.GetLecture(ctx, lectureID)
	if err != nil {
		return "", err
	}
	if state.IsDraft(l) {
		return "", state.ErrDraft
	}
	jobs, err := s.st.ListCurrentJobs(ctx, model.ScopeLecture, lectureID)
	if err != nil {
		return "", err
	}
	stage := state.EarliestStale(jobs)
	if stage == "" {
		return "", nil
	}
	return stage, s.Rerun(ctx, model.ScopeLecture, lectureID, stage, choice)
}

// UpdateLectures applies UpdateLecture to several lectures with ONE
// decision (A2.3: "课程级批量更新时只询问一次"): with choice "" it first
// collects the diffs of every lecture's earliest stale stage; if any lecture
// needs a choice it returns one *state.NeedsModelChoiceError with all diffs
// (LectureID filled) and changes nothing. Otherwise (or with a choice) it
// updates each lecture and reports per-lecture results.
func (s *Scheduler) UpdateLectures(ctx context.Context, lectureIDs []string, choice string) ([]UpdateResult, error) {
	if err := checkChoice(choice); err != nil {
		return nil, err
	}
	if choice == "" {
		var all []state.ModelDiff
		for _, id := range lectureIDs {
			l, err := s.st.GetLecture(ctx, id)
			if err != nil || state.IsDraft(l) {
				continue // reported per lecture below
			}
			jobs, err := s.st.ListCurrentJobs(ctx, model.ScopeLecture, id)
			if err != nil {
				return nil, err
			}
			stage := state.EarliestStale(jobs)
			if stage == "" || rerunnable(jobs, stage) != nil {
				continue
			}
			var nm *state.NeedsModelChoiceError
			if _, err := s.rerunSnapshots(id, jobs, stage, ""); errors.As(err, &nm) {
				all = append(all, nm.Diffs...)
			}
		}
		if len(all) > 0 {
			return nil, &state.NeedsModelChoiceError{Diffs: all}
		}
	}
	res := make([]UpdateResult, 0, len(lectureIDs))
	for _, id := range lectureIDs {
		stage, err := s.UpdateLecture(ctx, id, choice)
		res = append(res, UpdateResult{LectureID: id, Stage: stage, Err: err})
	}
	return res, nil
}

// CancelLecture cancels every queued or running stage of a lecture (bulk
// "取消所选", A7). Returns the number of jobs cancelled.
func (s *Scheduler) CancelLecture(ctx context.Context, lectureID string) (int, error) {
	if _, err := s.st.GetLecture(ctx, lectureID); err != nil {
		return 0, err
	}
	jobs, err := s.st.ListCurrentJobs(ctx, model.ScopeLecture, lectureID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if j.Status != model.StatusQueued && j.Status != model.StatusRunning {
			continue
		}
		switch err := s.Cancel(j.ID); {
		case err == nil:
			n++
		case !errors.Is(err, state.ErrConflict): // ErrConflict: it finished meanwhile
			return n, err
		}
	}
	return n, nil
}

// DeleteLecture implements A7 with SPEC §4.6 delete: holding the scan lock
// (no job of the lecture can start meanwhile),
//   - force=false: any running stage → state.ErrBusy (SPEC §4.5 unchanged);
//   - force=true: cancel every queued/running job, wait up to DeleteWait for
//     the running ones to stop, ErrDeleteTimeout when they don't;
//
// then delete the rows (db.DeleteLecture, cascades) and remove the lecture
// directory and the directories of materials bound to it — only paths
// inside data_dir, never the original media's external location.
// Auxiliary tasks (v2 ruling 1) never need force: running ones are
// cancelled and waited for like a forced delete does with stages.
// Errors: db.ErrNotFound.
func (s *Scheduler) DeleteLecture(ctx context.Context, lectureID string, force bool) error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	l, err := s.st.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	mats, err := s.st.ListMaterials(ctx, l.CourseID)
	if err != nil {
		return err
	}
	var bound []model.Material
	for _, m := range mats {
		if m.LectureID != nil && *m.LectureID == l.ID {
			bound = append(bound, m)
		}
	}
	// The lecture's jobs and those of the materials bound to it: all of
	// them are deleted with the lecture (ON DELETE CASCADE), so none may
	// still be writing.
	jobs, err := s.st.ListCurrentJobs(ctx, model.ScopeLecture, l.ID)
	if err != nil {
		return err
	}
	for _, m := range bound {
		mj, err := s.st.ListCurrentJobs(ctx, model.ScopeMaterial, m.ID)
		if err != nil {
			return err
		}
		jobs = append(jobs, mj...)
	}
	var running, busy []model.Job
	for _, j := range jobs {
		if j.Status == model.StatusRunning {
			running = append(running, j)
			if !model.IsAuxStage(j.Stage) {
				busy = append(busy, j)
			}
		}
	}
	if len(busy) > 0 && !force {
		return fmt.Errorf("%w (%s)", state.ErrBusy, busy[0].Stage)
	}
	if force {
		for _, j := range jobs {
			if j.Status != model.StatusQueued {
				continue
			}
			if err := state.CancelQueued(ctx, s.st, j.ID); err != nil && !errors.Is(err, state.ErrConflict) {
				return err
			}
		}
	}
	// Without force only auxiliary tasks can be running here.
	if err := s.stopRuns(ctx, running); err != nil {
		return err
	}
	// The tombstone first (test-chaos 2): if the directory cannot be
	// removed below, or the program is killed in between, it marks the
	// directory as one whose lecture was deleted, which a later try (and
	// the next start: state.SweepLectureDirs) may remove.
	ldir := model.LectureDir(s.dataDir, l.CourseID, l.ID)
	// AUD1 M-1, R77 mid-1: the ids come from the database (an imported
	// library supplies them). Only the lecture's own directory is marked and
	// removed: both ids single path elements, and the directory its parent's
	// entry of exactly that name (not "own" for the lecture "OWN").
	ownLecture := recordDirOwn(s.dataDir, ldir, l.CourseID, l.ID)
	tombstoned := false
	if s.dataDir != "" && ownLecture {
		switch err := state.WriteTombstone(ldir, l.ID, s.now()); {
		case err == nil:
			tombstoned = true
		case !errors.Is(err, fs.ErrNotExist):
			slog.Warn("lecture delete: no tombstone written; a directory left behind is only removed by this delete's retries", "lecture", l.ID, "err", err)
		}
	}
	if err := s.st.DeleteLecture(ctx, l.ID); err != nil {
		if tombstoned {
			if rerr := state.RemoveTombstone(ldir); rerr != nil {
				slog.Warn("lecture delete failed; its tombstone could not be removed (the next start removes it)", "lecture", l.ID, "err", rerr)
			}
		}
		return err
	}
	// the kv record of its kind judgement goes with it (no cascade for kv)
	if err := state.ForgetLectureKind(ctx, s.st, l.ID); err != nil {
		slog.Warn("lecture deleted, its kind record not cleared", "lecture", l.ID, "err", err)
	}
	s.mu.Lock()
	delete(s.incrReq, l.ID)
	delete(s.deferred, l.ID)
	for _, j := range jobs {
		delete(s.backoff, j.ID)
		delete(s.reasons, j.ID)
	}
	s.mu.Unlock()
	// Only directories inside data_dir; the original media's own location
	// (media_path may point anywhere) is never touched.
	if ownLecture {
		s.removeDir(ldir, l.ID, tombstoned)
	} else {
		slog.Warn("lecture deleted; its directory is not removed (not the lecture's own place)", "lecture", l.ID, "path", ldir)
	}
	for _, m := range bound {
		if mdir := model.MaterialDir(s.dataDir, m.CourseID, m.ID); recordDirOwn(s.dataDir, mdir, m.CourseID, m.ID) {
			s.removeInDataDir(mdir)
		} else {
			slog.Warn("lecture deleted; a bound material's directory is not removed (not its own place)", "material", m.ID, "path", mdir)
		}
	}
	slog.Info("lecture deleted", "lecture", l.ID, "force", force, "cancelled_running", len(running))
	s.ev.Changed("course", l.CourseID)
	s.Kick()
	return nil
}

// stopRuns cancels the jobs the caller saw running and waits up to
// deleteWait for all of them to stop. Caller holds scanMu (no new job can
// start meanwhile). A job that is no longer tracked has finished since the
// caller listed it (finish drops it from s.running only after its final
// transition): it is re-read and skipped, or cancelled when an automatic
// retry put it back to queued. Only a job still running in the database
// without a tracked run (e.g. during shutdown) is refused, before anything
// is cancelled.
func (s *Scheduler) stopRuns(ctx context.Context, jobs []model.Job) error {
	if len(jobs) == 0 {
		return nil
	}
	s.mu.Lock()
	var runs []*run
	var untracked []string
	for _, j := range jobs {
		if r := s.running[j.ID]; r != nil {
			runs = append(runs, r)
		} else {
			untracked = append(untracked, j.ID)
		}
	}
	s.mu.Unlock()
	for _, id := range untracked { // database reads never under s.mu
		cur, err := s.st.GetJob(ctx, id)
		switch {
		case errors.Is(err, db.ErrNotFound):
			continue
		case err != nil:
			return err
		case cur.Status == model.StatusRunning:
			return fmt.Errorf("%w: job %s is running but not tracked by the scheduler", state.ErrBusy, id)
		case cur.Status == model.StatusQueued:
			if err := state.CancelQueued(ctx, s.st, id); err != nil && !errors.Is(err, state.ErrConflict) {
				return err
			}
		}
	}
	s.mu.Lock()
	for _, r := range runs {
		r.userCancel = true
		r.cancel()
	}
	s.mu.Unlock()
	t := time.NewTimer(s.deleteWait)
	defer t.Stop()
	for _, r := range runs {
		select {
		case <-r.done:
		case <-t.C:
			slog.Warn("forced delete: jobs did not stop in time", "wait", s.deleteWait)
			return ErrDeleteTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// recordDirOwn reports whether dir, a record directory built from ids
// under the data directory root, may be marked and removed for that record
// (media.IDElement, media.OwnDirIn): every id a single path element, and
// dir — when anything is there — an ordinary directory, its parent's entry
// of exactly that name, with no link on the way down from root (SEC49 L1).
// Nothing there: true (there is nothing to remove, and nothing of another
// record either).
func recordDirOwn(root, dir string, ids ...string) bool {
	for _, id := range ids {
		if !media.IDElement(id) {
			return false
		}
	}
	own, exists := media.OwnDirIn(root, dir)
	return own || !exists
}

// removeInDataDir removes p only when it lies strictly inside data_dir.
// The rows are gone already (§4.6: database first), so a directory that
// cannot be removed now — a file in it is still open: the player streaming
// stream.mp4, an external program (test-chaos 2) — is tried again later
// (retryRemove).
func (s *Scheduler) removeInDataDir(p string) { s.removeDir(p, "", false) }

// removeDir is removeInDataDir; for the directory of lecture lectureID
// (lectureID non-empty) the tries go through state.RemoveDeletedLectureDir:
// only while the lecture does not exist and, when tombstoned, with the
// tombstone removed last (so that a directory still left at the end is
// removed at the next start by state.SweepLectureDirs).
func (s *Scheduler) removeDir(p, lectureID string, tombstoned bool) {
	if s.dataDir == "" {
		slog.Warn("no data_dir configured; directory not removed", "path", p)
		return
	}
	root, err1 := filepath.Abs(s.dataDir)
	abs, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		slog.Warn("directory not removed", "path", p)
		return
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		slog.Warn("refusing to remove a path outside data_dir", "path", abs)
		return
	}
	remove := func(ctx context.Context) error {
		if lectureID == "" {
			return os.RemoveAll(abs)
		}
		return state.RemoveDeletedLectureDir(ctx, s.st, abs, lectureID, tombstoned)
	}
	switch err := remove(context.Background()); {
	case err == nil:
	case errors.Is(err, state.ErrKeepDir):
		slog.Warn("directory of a deleted lecture kept", "path", abs, "err", err)
	default:
		slog.Warn("remove directory (a file in it may still be open); tried again later", "path", abs, "err", err)
		s.retryRemove(abs, remove)
	}
}

// DefaultRemoveRetry are the delays after which removeDir tries a
// directory again (Options.RemoveRetry): long enough for a player to be
// closed or a download to end.
var DefaultRemoveRetry = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// retryRemove calls remove (of the directory abs) after each delay of
// s.removeRetry until it succeeds or says the directory must stay
// (state.ErrKeepDir). Only while the scheduler runs: a stopped or not yet
// started scheduler leaves a tombstoned lecture directory to the next
// start's sweep.
func (s *Scheduler) retryRemove(abs string, remove func(context.Context) error) {
	s.mu.Lock()
	ctx := s.baseCtx
	if ctx == nil || s.stopped || len(s.removeRetry) == 0 {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1) // under s.mu: Stop sets stopped under it before it waits
	s.mu.Unlock()
	delays := s.removeRetry
	go func() {
		defer s.wg.Done()
		for i, d := range delays {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			switch err := remove(ctx); {
			case err == nil:
				slog.Info("removed a directory left by a delete", "path", abs, "tries", i+2)
				return
			case errors.Is(err, state.ErrKeepDir):
				slog.Warn("directory of a deleted lecture kept", "path", abs, "err", err)
				return
			default:
				slog.Warn("directory still not removed", "path", abs, "err", err, "last", i == len(delays)-1)
			}
		}
	}()
}

// GPUSlots reports the GPU slot usage for the resource observer (A10.1).
// It only counts running GPU jobs under s.mu: no nvidia-smi, no kv read.
// With Options.Devices it totals every device (SPEC-v2 I10): used =
// pipeline asr jobs and self-tests holding a slot, max = the slots of the
// enabled, present devices; paused is the global switch.
func (s *Scheduler) GPUSlots() (used, max int, paused bool) {
	if s.devs != nil {
		for _, d := range s.devs.List() {
			if d.Enabled && s.devs.Present(d.ID) {
				max += s.deviceSlots(d)
			}
		}
	} else {
		max = s.maxGPU()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.running {
		if r.res == ResourceGPU {
			used++
		}
	}
	for _, t := range s.tests {
		if t.holding {
			used++
		}
	}
	return used, max, s.paused
}

// InvalidateCourseWith is InvalidateCourse with a stale cause (A2.2).
func (s *Scheduler) InvalidateCourseWith(ctx context.Context, courseID string, cause state.StaleCause) error {
	changed, err := state.InvalidateCourseWith(ctx, s.st, s.Hashers(), courseID, cause)
	for _, j := range changed {
		s.emitJob(ctx, j)
	}
	return err
}
