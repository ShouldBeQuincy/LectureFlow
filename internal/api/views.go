// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// Building the response structs of types.go / types_v11.go from database
// rows (SPEC §8.1, SPEC-v1.1).

func strOrNil(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	v := *p
	return &v
}

// viewStages are the stages a scope's view lists, in order: the scope's
// chain, and for a course also the v2 glossary_review (lead ruling 2: a
// course job outside the glossary_seed chain).
func viewStages(scope string) []string {
	if scope == model.ScopeCourse {
		return append(stagesOf(scope), model.StageGlossaryReview)
	}
	return stagesOf(scope)
}

// orderJobs returns the jobs in the dependency order of their scope.
func orderJobs(scope string, jobs []model.Job) []model.Job {
	byStage := make(map[string]model.Job, len(jobs))
	for _, j := range jobs {
		byStage[j.Stage] = j
	}
	out := make([]model.Job, 0, len(jobs))
	for _, st := range viewStages(scope) {
		if j, ok := byStage[st]; ok {
			out = append(out, j)
		}
	}
	return out
}

// allStatuses lists every job status (one query loads every current job).
var allStatuses = []string{model.StatusPending, model.StatusQueued, model.StatusRunning,
	model.StatusDone, model.StatusFailed, model.StatusCancelled, model.StatusStale}

// jobIndex holds the current jobs of every scope, loaded with ONE query, so
// course lists and the dashboard do not query the jobs of each lecture.
// Auxiliary tasks (title, chapters, stream, filename_rule) are kept apart:
// of() returns the pipeline jobs only, aux() the auxiliary ones.
type jobIndex map[string][]model.Job

func (s *Server) loadJobs(ctx context.Context) (jobIndex, error) {
	jobs, err := s.st.ListCurrentJobsByStatus(ctx, allStatuses...)
	if err != nil {
		return nil, err
	}
	ix := jobIndex{}
	ix.add(jobs)
	return ix, nil
}

// add files jobs under their scope (aux tasks apart), keeping their order.
func (ix jobIndex) add(jobs []model.Job) {
	for _, j := range jobs {
		k := j.Scope + "/" + j.ScopeID
		if model.IsAuxStage(j.Stage) {
			k = "aux:" + k
		}
		ix[k] = append(ix[k], j)
	}
}

func (ix jobIndex) of(scope, id string) []model.Job  { return ix[scope+"/"+id] }
func (ix jobIndex) aux(scope, id string) []model.Job { return ix["aux:"+scope+"/"+id] }

// scopeJobs reads the current jobs of one scope, split into pipeline jobs
// and auxiliary tasks.
func (s *Server) scopeJobs(ctx context.Context, scope, id string) (pipeline, aux []model.Job, err error) {
	jobs, err := s.st.ListCurrentJobs(ctx, scope, id)
	if err != nil {
		return nil, nil, err
	}
	pipeline, aux = state.SplitAux(jobs)
	return pipeline, aux, nil
}

func findJob(jobs []model.Job, stage string) (model.Job, bool) {
	for _, j := range jobs {
		if j.Stage == stage {
			return j, true
		}
	}
	return model.Job{}, false
}

func anyStatus(jobs []model.Job, statuses ...string) bool {
	for _, j := range jobs {
		for _, st := range statuses {
			if j.Status == st {
				return true
			}
		}
	}
	return false
}

// providerName is the display name of a provider: the current name in the
// settings, else the name recorded in the snapshot (deleted provider), else
// the id.
func (s *Server) providerName(id, recorded string) string {
	if s.set != nil {
		if p, ok := s.set.Provider(id); ok {
			return p.Name
		}
	}
	if recorded != "" {
		return recorded
	}
	return id
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// llmLabel is "DeepSeek / deepseek-chat · 温度 0.3 · 窗口 3" (types_v11.go),
// with " · 深度思考 高" when the snapshot has thinking on (off is not shown).
// The front end writes the same label in the interface language from the
// structured fields (StageModel.params, SnapshotView.params); this one is the
// fallback.
func llmLabel(name, modelName string, p model.RoleParams) string {
	l := name + " / " + modelName
	// With thinking on, the temperature is not sent (DeepSeek ignores it,
	// Anthropic rejects it), so the label does not show it either.
	if p.Temperature != nil && !model.ThinkingOn(p) {
		l += " · 温度 " + fmtFloat(*p.Temperature)
	}
	if p.Window != nil {
		l += " · 窗口 " + strconv.Itoa(*p.Window)
	}
	if lv, ok := thinkingNames[model.ThinkingLevel(p)]; ok {
		l += " · 深度思考 " + lv
	}
	return l
}

// thinkingNames are the labels' names of the thinking levels other than off.
var thinkingNames = map[string]string{model.ThinkingLow: "低", model.ThinkingHigh: "高", model.ThinkingMax: "最高"}

// asrLabel is the asr snapshot's label (SPEC-v2 I6, phase 3b): "device ·
// model · precision · language", e.g. "Intel(R) AI Boost · large-v3-turbo ·
// fp16 · en", the device's own name from the registry (its id when there is
// no row; a v1.1 snapshot is read as the cuda:0 / fw:cpu it was migrated
// to). Without the registry (builds and tests before 3b) a v1.1 snapshot
// keeps the v1.1 label "large-v2 · auto · cuda · en". Every part is a name
// or code, so the label needs no translation.
func (s *Server) asrLabel(a model.ASRSnapshot) string {
	if s.dev == nil && a.DeviceID == "" {
		return legacyASRLabel(a)
	}
	n := model.NormalizeASRSnapshot(a)
	parts := []string{s.deviceName(n.DeviceID), n.Model}
	for _, v := range []string{n.Precision, n.Language} {
		if v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " · ")
}

// ASRPendingLabel is the first part of the asr label while no device is
// chosen yet (3b Gate round 2, suggestion 8): the launch could not name a
// device and wrote the fallback auto snapshot (settings.FallbackASRSnapshot),
// whose fw:cpu configuration is only a placeholder until the job starts
// and is bound. The label (a fallback) is "自动（等待可用设备） · {language}";
// the view's asr (StageModel.ASR, SnapshotView.ASR) is {auto: true,
// language} without a device, model or precision and its model is empty:
// the front end's signal for its own text (i18n common.model.asr_auto_waiting).
const ASRPendingLabel = "自动（等待可用设备）"

// asrAwaitsDevice reports whether raw is the fallback auto snapshot.
func asrAwaitsDevice(raw string) bool {
	a, ok := model.ParseASRSnapshot(raw)
	return ok && a.Auto && model.EqualASRSnapshot(a, settings.FallbackASRSnapshot(a.Language))
}

// asrPendingLabel is the label of a fallback snapshot in language lang.
func asrPendingLabel(lang string) string {
	if lang == "" {
		return ASRPendingLabel
	}
	return ASRPendingLabel + " · " + lang
}

// legacyASRLabel is the v1.1 label "large-v2 · auto · cuda · en".
func legacyASRLabel(a model.ASRSnapshot) string {
	parts := []string{a.Model}
	for _, v := range []string{a.ComputeType, a.Device, a.Language} {
		if v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " · ")
}

// asrSnapshotView is the snapshot a view shows: normalized to the I6 form
// (device_id, backend, precision) once devices exist, as stored otherwise.
func (s *Server) asrSnapshotView(a model.ASRSnapshot) model.ASRSnapshot {
	if s.dev == nil {
		return a
	}
	return model.NormalizeASRSnapshot(a)
}

// stageModel is the snapshot summary of a lecture stage (A4); nil for
// prep/export, drafts and course/material stages (empty snapshots).
func (s *Server) stageModel(stage, raw string) *StageModel {
	switch stage {
	case model.StageSummary, model.StageRepair, model.StageTranslate:
		snap, ok := model.ParseLLMSnapshot(raw)
		if !ok {
			return nil
		}
		name := s.providerName(snap.ProviderID, snap.ProviderName)
		p := snap.Params
		// Source (v3.2.x P1-b): "manual" for a model named for one retry.
		return &StageModel{Label: llmLabel(name, snap.Model, p), ProviderID: snap.ProviderID, ProviderName: name, Model: snap.Model, Params: &p,
			Source: snap.Source}
	case model.StageASR:
		a, ok := model.ParseASRSnapshot(raw)
		if !ok {
			return nil
		}
		sv := s.asrSnapshotView(a)
		return &StageModel{Label: s.asrLabel(a), Model: a.Model, ASR: &sv}
	}
	return nil
}

// snapshotView describes one side of a model diff (A4.3); "（无）" when empty.
func (s *Server) snapshotView(stage, raw string) SnapshotView {
	switch stage {
	case model.StageSummary, model.StageRepair, model.StageTranslate:
		if snap, ok := model.ParseLLMSnapshot(raw); ok {
			name := s.providerName(snap.ProviderID, snap.ProviderName)
			p := snap.Params
			// v2 G5.5: the dialog shows "提示词 v1 → v2" when the versions differ.
			return SnapshotView{Label: llmLabel(name, snap.Model, p), ProviderID: snap.ProviderID, ProviderName: name, Model: snap.Model, Params: &p,
				PromptVersion: model.SnapshotPromptVersion(snap)}
		}
	case model.StageASR:
		if a, ok := model.ParseASRSnapshot(raw); ok {
			v := s.asrSnapshotView(a)
			return SnapshotView{Label: s.asrLabel(a), Model: a.Model, ASR: &v}
		}
	}
	return SnapshotView{Label: "（无）"}
}

// currentView is the "current settings" side of a model diff: the fallback
// auto snapshot (no device can be named now) reads ASRPendingLabel.
func (s *Server) currentView(stage, raw string) SnapshotView {
	v := s.snapshotView(stage, raw)
	if stage == model.StageASR && asrAwaitsDevice(raw) {
		a, _ := model.ParseASRSnapshot(raw)
		v.Label, v.Model = asrPendingLabel(a.Language), ""
		v.ASR = &model.ASRSnapshot{Language: a.Language, Auto: true}
	}
	return v
}

// needsChoiceView is the 409 body of A4.3. withLecture fills lecture_id and
// lecture_title (course and batch updates).
func (s *Server) needsChoiceView(ctx context.Context, e *state.NeedsModelChoiceError, withLecture bool) NeedsModelChoice {
	out := NeedsModelChoice{Error: model.ErrorInfo{Code: model.ErrNeedsModelChoice}, Diff: make([]ModelDiffView, 0, len(e.Diffs))}
	titles := map[string]string{}
	for _, d := range e.Diffs {
		v := ModelDiffView{
			Stage:          d.Stage,
			Snapshot:       s.snapshotView(d.Stage, d.Snapshot),
			Current:        s.currentView(d.Stage, d.Current),
			SnapshotUsable: d.SnapshotUsable,
			Reason:         d.Reason,
			ReasonInfo:     d.ReasonInfo,
		}
		if withLecture && d.LectureID != "" {
			v.LectureID = d.LectureID
			t, ok := titles[d.LectureID]
			if !ok {
				if l, err := s.st.GetLecture(ctx, d.LectureID); err == nil {
					t = l.Title
				}
				titles[d.LectureID] = t
			}
			v.LectureTitle = t
		}
		out.Diff = append(out.Diff, v)
	}
	return out
}

// jobProvider is the provider whose prices apply to a job: the snapshot's
// provider for lecture LLM stages, the role's current provider otherwise.
func (s *Server) jobProvider(j model.Job) string {
	if snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot); ok {
		return snap.ProviderID
	}
	if s.set != nil && model.RoleOfStage(j.Stage) != "" {
		if t, err := s.set.RoleTarget(model.RoleOfStage(j.Stage)); err == nil {
			return t.ProviderID
		}
	}
	return ""
}

// usageOf is the token usage of a job with its estimated cost (SPEC §17.7):
// stats.estimated_cost when the run recorded it, otherwise the prices of
// the job's provider (A5.1), null without prices.
func (s *Server) usageOf(j model.Job) UsageStats {
	st := j.Stats
	u := UsageStats{PromptTokens: st.PromptTokens, CacheHitTokens: st.CacheHitTokens, CompletionTokens: st.CompletionTokens}
	if st.EstimatedCost > 0 {
		c := st.EstimatedCost
		u.EstimatedCost = &c
		return u
	}
	if s.set == nil {
		if s.cfg != nil {
			if c, ok := sched.EstimateCost(s.cfg, j.Stage, st); ok {
				u.EstimatedCost = &c
			}
		}
		return u
	}
	if model.RoleOfStage(j.Stage) == "" {
		return u
	}
	if p, ok := s.set.Provider(s.jobProvider(j)); ok {
		if c, ok := p.EstimateCost(st.PromptTokens, st.CacheHitTokens, st.CompletionTokens); ok {
			u.EstimatedCost = &c
		}
	}
	return u
}

func addUsage(sum *UsageStats, u UsageStats) {
	sum.PromptTokens += u.PromptTokens
	sum.CacheHitTokens += u.CacheHitTokens
	sum.CompletionTokens += u.CompletionTokens
	if u.EstimatedCost != nil {
		c := *u.EstimatedCost
		if sum.EstimatedCost != nil {
			c += *sum.EstimatedCost
		}
		sum.EstimatedCost = &c
	}
}

// stageViews builds the StageView of every job of one scope (in stage order).
// title is the lecture's title (the power notes' lecture param; "" for the
// other scopes).
func (s *Server) stageViews(scope string, jobs []model.Job, mediaDeleted bool, title string) []StageView {
	actions := state.StageActions(jobs, mediaDeleted)
	display := state.DisplayStatuses(jobs)
	out := make([]StageView, 0, len(jobs))
	for _, j := range orderJobs(scope, jobs) {
		v := StageView{
			Stage:          j.Stage,
			Status:         j.Status,
			Progress:       j.Progress,
			Attempt:        j.Attempt,
			AllowedActions: actions[j.Stage],
			StartedAt:      strOrNil(j.StartedAt),
			FinishedAt:     strOrNil(j.FinishedAt),
			Stats:          s.stageUsage(j), // the same as the job events' stats
			DisplayStatus:  display[j.Stage],
			Model:          s.stageModel(j.Stage, j.ConfigSnapshot),
		}
		// Not started yet with the fallback snapshot: no device chosen (the
		// same view as the dialog's current side, currentView).
		if j.Stage == model.StageASR && v.Model != nil && (j.Status == model.StatusPending || j.Status == model.StatusQueued) &&
			asrAwaitsDevice(j.ConfigSnapshot) {
			a, _ := model.ParseASRSnapshot(j.ConfigSnapshot)
			v.Model.Label, v.Model.Model = asrPendingLabel(a.Language), ""
			v.Model.ASR = &model.ASRSnapshot{Language: a.Language, Auto: true}
		}
		if v.DisplayStatus == "" {
			v.DisplayStatus = j.Status
		}
		v.Error, v.ErrorInfo = jobErrorView(j)
		if j.Status == model.StatusStale {
			// P14.3: the decoded reason; stale_reason keeps the Chinese text
			// until the i18n extraction (a stale job recorded without a
			// reason reads "输入已变化", like state.StaleText).
			info := state.StaleReasonOf(j)
			raw := ""
			if j.StaleReason != nil {
				raw = *j.StaleReason
			}
			r := state.StaleReasonText(raw)
			v.StaleReason, v.StaleInfo = &r, &info
		}
		if v.AllowedActions == nil {
			v.AllowedActions = []string{}
		}
		if j.Status == model.StatusDone {
			v.Progress = 1
		}
		var wait string
		if s.sc != nil {
			wait = s.sc.WaitingFor(j)
		}
		if (j.Status == model.StatusPending || j.Status == model.StatusQueued) && wait != "" {
			w := wait
			v.WaitingFor = &w
		}
		v.AllowedActions = withUseColab(j, wait, v.AllowedActions)
		switch j.Status {
		case model.StatusDone, model.StatusFailed, model.StatusCancelled, model.StatusStale:
			if j.Stats.DurationMS > 0 {
				d := j.Stats.DurationMS
				v.DurationMS = &d
			} else if d, ok := elapsed(j.StartedAt, j.FinishedAt); ok {
				v.DurationMS = &d
			}
		}
		// The hover-card note and its coded form (v2 P14.3): the API's own
		// sentences and the stage's note (job.stats.note_info; a note stored
		// without one is legacy {text}), joined as one coded note.
		var notes []string
		var parts []model.ErrorInfo
		add := func(text string, info model.ErrorInfo) {
			notes, parts = append(notes, text), append(parts, info)
		}
		// Power management: the last power-policy event of a queued or
		// running asr job ("已切换 / 已挂起 / 已恢复：电源感知 …").
		if j.Stage == model.StageASR && (j.Status == model.StatusQueued || j.Status == model.StatusRunning) && j.Stats.PowerEvent != nil {
			if text, info, ok := s.powerNote(*j.Stats.PowerEvent, title); ok {
				add(text, info)
			}
		}
		// SPEC-v3 V14: Colab handed the lecture back to the local devices.
		if j.Stage == model.StageASR && j.Status == model.StatusQueued && j.Stats.RemoteFallback != nil {
			add("Colab 远端转写没有完成，已转回本机排队",
				model.ErrorInfo{Code: sched.CodeRemoteBackToLocal, Params: map[string]any{"cause": j.Stats.RemoteFallback.Code}})
		}
		if j.Status == model.StatusRunning {
			switch j.Stats.Status {
			case model.ASRStatusDownloadingModel:
				add("首次运行，正在下载模型", model.ErrorInfo{Code: state.CodeNoteDownloadingModel})
			case model.ASRStatusPreparingModel: // SPEC-v2 I7: OpenVINO downloads and converts the model
				add("首次使用，正在准备模型", model.ErrorInfo{Code: state.CodeNotePreparingModel})
			case model.ASRStatusCompilingModel: // lead ruling 3: first compile on this GPU / NPU
				add("首次在这台设备上使用，正在编译模型（只需一次，NPU 约需 4 分钟）", model.ErrorInfo{Code: state.CodeNoteCompilingModel})
			}
		}
		if j.Status == model.StatusRunning && wait == model.WaitBudget {
			add("已达到今日预算上限：暂停发送新请求，明天 0 点（本地时间）自动恢复", model.ErrorInfo{Code: state.CodeNoteBudgetPaused})
		}
		// SPEC-v2 D3 (lead ruling at the phase-2 gate): requests without a
		// price are not counted against the daily budget, which may then be
		// too low; a running LLM stage says so (it is not held back).
		if j.Status == model.StatusRunning && wait != model.WaitBudget && model.RoleOfStage(j.Stage) != "" && s.sc != nil {
			if n := s.sc.BudgetUnpriced(context.Background()); n > 0 {
				add(fmt.Sprintf("今天有 %d 个请求没有价格（未计价），每日预算可能低估了实际花费", n),
					model.ErrorInfo{Code: state.CodeNoteBudgetUnpriced, Params: map[string]any{"n": n}})
			}
		}
		switch n := j.Stats.Note; {
		case state.IsIncremental(n):
			add("增量翻译：只处理需要重翻的句子", model.ErrorInfo{Code: state.CodeNoteIncremental})
		case n != "" && j.Stats.NoteInfo != nil:
			add(n, *j.Stats.NoteInfo)
		case n != "":
			add(n, model.ErrorInfo{Code: state.CodeNoteLegacy, Params: map[string]any{"text": n}})
		}
		if j.Stage == model.StagePrep && mediaDeleted && (j.Status == model.StatusDone || j.Status == model.StatusStale) {
			add("原始媒体文件已按设置删除（不保留原始媒体），不能重跑预处理", model.ErrorInfo{Code: state.CodeNoteMediaDeleted})
		}
		if len(notes) > 0 {
			n := strings.Join(notes, "；")
			v.Note, v.NoteInfo = &n, state.JoinNote(parts...)
		}
		// A done stage's warning (job.error holds its text) in coded form.
		if j.Status == model.StatusDone && j.Stats.WarningInfo != nil {
			w := *j.Stats.WarningInfo
			v.WarningInfo = &w
		}
		// Phase 3b: the asr worker's timing (the stage clears it when an
		// attempt starts, so it always belongs to the last finished run).
		if j.Stage == model.StageASR && j.Stats.ASRTiming != nil {
			tm := *j.Stats.ASRTiming
			v.ASRTiming = &tm
		}
		withRequestStats(&v, j) // v3.2.x P1-c
		out = append(out, v)
	}
	return out
}

// powerNote is the stage note of job.stats.power_event (power-brief §5):
// notes.{power.switched | power.suspended | power.resumed} with the
// lecture's title and the device's label (the device it continues on:
// to_device, else from_device). ok is false for another code.
func (s *Server) powerNote(ev model.PowerEvent, lecture string) (string, model.ErrorInfo, bool) {
	dev := ev.ToDevice
	if dev == "" {
		dev = ev.FromDevice
	}
	name := s.deviceName(dev)
	switch ev.Code {
	case sched.CodePowerSwitched:
		return fmt.Sprintf("已切换：电源感知 - 已拔掉电源，《%s》转到 %s 继续", lecture, name),
			model.ErrorInfo{Code: ev.Code, Params: map[string]any{"lecture": lecture, "device": name}}, true
	case sched.CodePowerSuspended:
		return fmt.Sprintf("已挂起：电源感知 - 已拔掉电源，《%s》等待电源适配器", lecture),
			model.ErrorInfo{Code: ev.Code, Params: map[string]any{"lecture": lecture}}, true
	case sched.CodePowerResumed:
		return fmt.Sprintf("已恢复：电源感知 - 已接通电源，《%s》在 %s 上继续", lecture, name),
			model.ErrorInfo{Code: ev.Code, Params: map[string]any{"lecture": lecture, "device": name}}, true
	}
	return "", model.ErrorInfo{}, false
}

func elapsed(start, end *string) (int64, bool) {
	if start == nil || end == nil || *start == "" || *end == "" {
		return 0, false
	}
	a, err1 := time.Parse(time.RFC3339, *start)
	b, err2 := time.Parse(time.RFC3339, *end)
	if err1 != nil || err2 != nil || b.Before(a) {
		return 0, false
	}
	return b.Sub(a).Milliseconds(), true
}

// keepMedia is storage "保留原始媒体" (A5.3 settings; legacy config without them).
func (s *Server) keepMedia() bool {
	if s.set != nil {
		return s.set.General().KeepOriginalMedia
	}
	return s.cfg == nil || s.cfg.Storage.KeepMedia()
}

func (s *Server) mediaDeleted(l model.Lecture) bool {
	return !s.keepMedia() && !state.MediaExists(s.dataDir, l)
}

func (s *Server) lectureDir(l model.Lecture) string {
	return model.LectureDir(s.dataDir, l.CourseID, l.ID)
}

// withUseColab adds model.ActionUseColab ("改用 Colab 处理") to a queued asr
// stage's actions when the scheduler says it waits with
// model.WaitRemoteFallback (Colab handed it back) or model.WaitRemoteHeld
// (a shutdown holds the automatic dispatch) and no local device can take
// it, so without the action it would wait for good (colabfix, colabfix2).
// The front end sends it to POST /api/lectures/{id}/use-colab.
func withUseColab(j model.Job, wait string, acts []string) []string {
	if j.Stage != model.StageASR || j.Status != model.StatusQueued ||
		(wait != model.WaitRemoteFallback && wait != model.WaitRemoteHeld) || slices.Contains(acts, model.ActionUseColab) {
		return acts
	}
	return append(slices.Clone(acts), model.ActionUseColab)
}

// lectureActions are the lecture-level actions of v1.1 (A3, A2.3, A7):
// launch for drafts; update when the earliest stale stage can be rerun now
// (the rerun A2.3 performs — not offered while a downstream stage runs,
// which would be 409); cancel while some stage is queued or running;
// delete always (A7: a running lecture is cancelled first, can_delete tells
// the UI to send force and say so). jobs are the pipeline jobs: an
// auxiliary task waiting or running alone offers no lecture-level cancel.
// SPEC-v2 P13.5: regenerate_title once the summary is done or stale, when
// the organizer role is configured (organizer), as POST .../title accepts.
// SPEC-v2 P4.2: fill_words when there is a transcript without word
// timestamps (noWords) and asr is done or stale and can be rerun now — the
// front end reruns asr, so it is not offered while that would be 409.
func lectureActions(l model.Lecture, jobs []model.Job, actions map[string][]string, organizer, noWords bool) []string {
	out := []string{}
	if state.IsDraft(l) {
		out = append(out, model.LectureActionLaunch)
	}
	if e := state.EarliestStale(jobs); e != "" && !state.IsDraft(l) {
		for _, a := range actions[e] {
			if a == model.ActionRerun {
				out = append(out, model.LectureActionUpdate)
				break
			}
		}
	}
	if anyStatus(jobs, model.StatusQueued, model.StatusRunning) {
		out = append(out, model.LectureActionCancel)
	}
	out = append(out, model.LectureActionDelete)
	// the title task's dependency (lead ruling 1), as summaryReady checks it
	if sum, ok := findJob(jobs, model.StageSummary); ok && organizer && (sum.Status == model.StatusDone || sum.Status == model.StatusStale) {
		out = append(out, model.LectureActionRegenerateTitle)
	}
	if asr, ok := findJob(jobs, model.StageASR); ok && noWords && !state.IsDraft(l) &&
		(asr.Status == model.StatusDone || asr.Status == model.StatusStale) && slices.Contains(actions[model.StageASR], model.ActionRerun) {
		out = append(out, model.LectureActionFillWords)
	}
	return out
}

// lectureInfo is what a lecture view needs besides the lecture row: its
// current jobs (pipeline and auxiliary), whether a bound material runs
// (boundBusy), its calendar number within the course and the slides it
// uses (lecture kinds: Lecture.materials; nil = none).
type lectureInfo struct {
	jobs, aux []model.Job
	boundBusy bool
	num       lectureNumber
	materials []MaterialUse
	// counts: the lecture's counts read with its list (lecture_counts.go);
	// nil = lectureViewJobs reads them.
	counts *db.LectureCounts
}

// lectureView is GET /api/lectures/{id} (SPEC §8.1).
func (s *Server) lectureView(ctx context.Context, l model.Lecture) (Lecture, error) {
	jobs, aux, err := s.scopeJobs(ctx, model.ScopeLecture, l.ID)
	if err != nil {
		return Lecture{}, err
	}
	busy, err := s.boundBusy(ctx, l.CourseID, nil)
	if err != nil {
		return Lecture{}, err
	}
	c, err := s.st.GetCourse(ctx, l.CourseID)
	if err != nil {
		return Lecture{}, err
	}
	nums, _, err := s.courseNumbering(ctx, c)
	if err != nil {
		return Lecture{}, err
	}
	mats, err := s.courseMaterialUses(ctx, c.ID, nums)
	if err != nil {
		return Lecture{}, err
	}
	counts, err := s.lectureCounts(ctx, []string{l.ID})
	if err != nil {
		return Lecture{}, err
	}
	return s.lectureViewJobs(ctx, l, lectureInfo{jobs: jobs, aux: aux, boundBusy: busy[l.ID], num: nums[l.ID], materials: mats[l.ID],
		counts: countsFor(counts, l.ID)})
}

// courseMaterialUses is Lecture.materials of every lecture of a course
// (lectureMaterials over the course's materials, read once): the bindings
// without the page texts (ListMaterialRefs) and the page counts from SQL
// (MaterialPageCounts), so a lecture view never loads the slides' text.
func (s *Server) courseMaterialUses(ctx context.Context, courseID string, nums numbering) (map[string][]MaterialUse, error) {
	ms, err := s.st.ListMaterialRefs(ctx, courseID)
	if err != nil {
		return nil, err
	}
	pages, err := s.st.MaterialPageCounts(ctx, courseID)
	if err != nil {
		return nil, err
	}
	return lectureMaterials(ms, pages, nums), nil
}

// lectureViewJobs builds a Lecture from its row and info.
func (s *Server) lectureViewJobs(ctx context.Context, l model.Lecture, in lectureInfo) (Lecture, error) {
	jobs := in.jobs
	deleted := s.mediaDeleted(l)
	actions := state.StageActions(jobs, deleted)
	v := Lecture{
		ID: l.ID, CourseID: l.CourseID, Title: l.Title, CreatedAt: l.CreatedAt,
		Stages:     s.stageViews(model.ScopeLecture, jobs, deleted, l.Title),
		CanDelete:  state.CanDelete(jobs) && !in.boundBusy,
		Reviewable: state.Reviewable(jobs),
		Exports:    s.exports(l, in.num),
		LaunchedAt: strOrNil(l.LaunchedAt),
		Draft:      state.IsDraft(l),
		Materials:  in.materials,
	}
	if v.Materials == nil {
		v.Materials = []MaterialUse{}
	}
	s.calendarFields(&v, l, in.num)
	if t := state.StaleText(jobs); t != "" {
		v.StaleText = &t
	}
	v.Stale = staleSummary(jobs)
	for _, st := range v.Stages {
		addUsage(&v.Usage, st.Stats)
	}
	v.Aux = s.auxViews(in.aux)
	var err error
	cnt := in.counts
	if cnt == nil {
		c, err := s.lectureCountsOne(ctx, l.ID)
		if err != nil {
			return Lecture{}, err
		}
		cnt = &c
	}
	v.Counts.Sentences = cnt.Sentences
	if d, ok := s.mediaDurationMS(l, jobs); ok {
		v.DurationMS = &d // the media's length (audio.wav, media_duration.go)
	}
	if v.Counts.Sentences > 0 {
		// idx runs 0..n-1: without audio.wav, the last sentence's end is the
		// best known length (what was transcribed).
		if last := cnt.Last; last.Err != nil {
			return Lecture{}, last.Err
		} else if last.Found {
			if v.DurationMS == nil {
				d := last.EndMS
				v.DurationMS = &d
			}
			v.HasWords = last.HasWords
		}
		// P4.2: only asr writes words, for every sentence of its result at
		// once, so the first and the last sentence tell whether the
		// transcript has them (without reading the whole lecture per view).
		if !v.HasWords {
			if first := cnt.First; first.Err != nil {
				return Lecture{}, first.Err
			} else if first.Found {
				v.HasWords = first.HasWords
			}
		}
	}
	// "修复断句" on a transcript worth repairing (textrepair.go)
	s.withTextRepairAction(ctx, &v, l, jobs)
	// a queued asr stage keeps its checkpoint's position (asr_checkpoint.go)
	if ck := s.asrCheckpointView(l, jobs, v.DurationMS); ck != nil {
		for i := range v.Stages {
			if v.Stages[i].Stage == model.StageASR {
				v.Stages[i].Checkpoint = ck
			}
		}
	}
	v.LectureActions = lectureActions(l, jobs, actions, s.roleConfigured(model.RoleOrganizer), v.Counts.Sentences > 0 && !v.HasWords)
	v.Counts.PatchesPending = cnt.PatchesPending
	zh := cnt.Zh
	v.Counts.ZhOK, v.Counts.ZhQAFailed, v.Counts.ZhStale = zh[model.ZhOK], zh[model.ZhQAFailed], zh[model.ZhStale]
	v.Playable = playable(jobs) && v.Counts.Sentences > 0
	if v.Stream, err = s.streamInfo(ctx, l, jobs, in.aux); err != nil {
		return Lecture{}, err
	}
	v.Angles = s.lectureAngles(l)
	return v, nil
}

// calendarFields fills the P13 fields of a lecture view: the calendar
// number (computed, ruling 4), recording time, titles and blurbs. Label is
// the number (W02L1, or calendar.Unknown), Seq the position in the
// course's list order.
func (s *Server) calendarFields(v *Lecture, l model.Lecture, n lectureNumber) {
	v.Seq = n.Seq
	if v.Seq == 0 {
		v.Seq = l.Seq // not numbered with its course (cannot happen for a stored lecture)
	}
	v.Label = n.Label
	if v.Label == "" {
		v.Label = calendar.Unknown
	}
	v.Week, v.N, v.Numbered, v.SameDayByName = n.Week, n.N, n.OK, n.SameDayByName
	v.CiteLabel = n.CiteLabel
	if v.CiteLabel == "" {
		v.CiteLabel = v.Label
	}
	// lecture kinds: the effective kind and the display counter
	v.Kind, v.KindSource = n.Kind, n.KindSource
	if v.Kind == "" {
		v.Kind = model.KindLecture // not numbered with its course
	}
	if v.KindSource == "" {
		v.KindSource = viewKindSource(l, v.Kind)
	}
	v.KindN, v.KindCount, v.ShowN = n.KindN, n.KindCount, n.ShowN
	v.WeekOverride = weekOverrideView(l, n) // course-ux
	if !n.OK {
		v.CalendarHint = n.Hint
		if v.CalendarHint == "" {
			v.CalendarHint = hintRecordedAt
		}
	}
	v.RecordedAt = strOrNil(l.RecordedAt)
	if t := recordedTime(l.RecordedAt); t != nil {
		v.RecordedHasTime = calendar.HasClock(*t)
	}
	v.RecordedAtSource = l.RecordedAtSource
	if v.RecordedAtSource == "" {
		v.RecordedAtSource = model.RecordedAtNone
	}
	v.TitleZh, v.BlurbEn, v.BlurbZh = strOrNil(l.TitleZh), strOrNil(l.BlurbEn), strOrNil(l.BlurbZh)
	v.TitleSource = l.TitleSource
	if v.TitleSource == "" {
		v.TitleSource = model.TitleSourceFilename
	}
	v.OriginalFilename = l.OriginalFilename
}

// playable: the lecture can be played once asr is done (P1); a stale asr
// still has its sentences.
func playable(jobs []model.Job) bool {
	j, ok := findJob(jobs, model.StageASR)
	return ok && (j.Status == model.StatusDone || j.Status == model.StatusStale)
}

// staleSummary is Lecture.Stale (P14.3): the distinct reasons of the stale
// pipeline jobs and the stale stages, both in pipeline order; nil when
// nothing is stale.
func staleSummary(jobs []model.Job) *StaleSummary {
	stale := state.StaleStages(jobs)
	if len(stale) == 0 {
		return nil
	}
	out := &StaleSummary{Reasons: []model.StaleReasonCode{}, Stages: stale}
	seen := map[string]bool{}
	for _, st := range stale {
		j, _ := findJob(jobs, st)
		r := state.StaleReasonOf(j)
		key, _ := json.Marshal(r)
		if !seen[string(key)] {
			seen[string(key)] = true
			out.Reasons = append(out.Reasons, r)
		}
	}
	return out
}

// jobErrorView is StageView.Error and ErrorInfo of a job (P14.3): a coded
// error stored as ErrorInfo JSON gives both (Error = its detail), legacy
// text only Error.
func jobErrorView(j model.Job) (*string, *model.ErrorInfo) {
	if j.Error == nil || *j.Error == "" {
		return nil, nil
	}
	info, text := state.ParseJobError(*j.Error)
	return &text, info
}

// auxViews are the StageViews of a scope's auxiliary tasks (Lecture.Aux):
// status, progress, error and their own actions (retry, cancel), in
// model.AuxStages order. nil when there are none.
func (s *Server) auxViews(aux []model.Job) []StageView {
	if len(aux) == 0 {
		return nil
	}
	out := make([]StageView, 0, len(aux))
	for _, stage := range model.AuxStages {
		if j, ok := findJob(aux, stage); ok {
			out = append(out, s.auxView(j))
		}
	}
	return out
}

// auxView is the StageView of one auxiliary task.
func (s *Server) auxView(j model.Job) StageView {
	v := StageView{
		Stage: j.Stage, Status: j.Status, Progress: j.Progress, Attempt: j.Attempt,
		AllowedActions: state.AuxActions(j), DisplayStatus: j.Status,
		StartedAt: strOrNil(j.StartedAt), FinishedAt: strOrNil(j.FinishedAt), Stats: s.usageOf(j),
	}
	v.Error, v.ErrorInfo = jobErrorView(j)
	if j.Status == model.StatusDone {
		v.Progress = 1
	}
	if s.sc != nil && (j.Status == model.StatusPending || j.Status == model.StatusQueued) {
		if w := s.sc.WaitingFor(j); w != "" {
			v.WaitingFor = &w
		}
	}
	switch j.Status {
	case model.StatusDone, model.StatusFailed, model.StatusCancelled:
		if d, ok := elapsed(j.StartedAt, j.FinishedAt); ok {
			v.DurationMS = &d
		}
	}
	withRequestStats(&v, j) // v3.2.x P1-c (title, chapters)
	return v
}

// exports lists the finished files in export/ (temporary files excluded).
// Name is what the file is downloaded as (SPEC-v2 P13.6, phase 2
// acceptance): a transcript or subtitle file shows the lecture's current
// number and English title (stages.ExportDownloadName, the
// Content-Disposition of exportFile), although the file on disk keeps the
// name of its export run until export runs again; a citation JSON shows
// its own name (fix-p2: its content carries the numbers of that run). URL
// is the file on disk. num is the lecture's number; without one (not
// numbered with its course) the names on disk are shown.
func (s *Server) exports(l model.Lecture, num lectureNumber) []ExportFile {
	label := num.CiteLabel
	if label == "" {
		label = num.Label
	}
	out := []ExportFile{}
	dir := filepath.Join(s.lectureDir(l), model.ExportDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()
		if label != "" {
			name = stages.ExportDownloadName(label, l.Title, name)
		}
		out = append(out, ExportFile{
			Name: name,
			URL:  "/api/lectures/" + url.PathEscape(l.ID) + "/export/" + url.PathEscape(e.Name()),
			Size: info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// boundBusy returns the lectures of a course that have a bound material
// with a running job. Those jobs are deleted with the lecture, so
// Scheduler.DeleteLecture refuses a delete without force while they run
// (A7): can_delete must say so too, or the UI sends no force and gets 409
// with nothing to cancel. ix: the request's loaded jobs (nil: read them).
func (s *Server) boundBusy(ctx context.Context, courseID string, ix jobIndex) (map[string]bool, error) {
	binds, err := s.st.ListMaterialBindings(ctx, courseID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for mid, lid := range binds {
		var jobs []model.Job
		if ix != nil {
			jobs = ix.of(model.ScopeMaterial, mid)
		} else if jobs, err = s.st.ListCurrentJobs(ctx, model.ScopeMaterial, mid); err != nil {
			return nil, err
		}
		if !state.CanDelete(jobs) {
			out[lid] = true
		}
	}
	return out, nil
}

// lectureViews is GET /api/courses/{id}/lectures in the list order of
// P13.4: by recorded_at, lectures without one last.
func (s *Server) lectureViews(ctx context.Context, c model.Course) ([]Lecture, error) {
	nums, lecs, err := s.courseNumbering(ctx, c)
	if err != nil {
		return nil, err
	}
	// the jobs of this course's lectures and bound materials only
	// (course_reads.go), not every current job of the library
	binds, err := s.st.ListMaterialBindings(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	bound := make([]string, 0, len(binds))
	for mid := range binds {
		bound = append(bound, mid)
	}
	ix, err := s.scopeJobsOf(ctx, nil, bound, lecs)
	if err != nil {
		return nil, err
	}
	busy, err := s.boundBusy(ctx, c.ID, ix)
	if err != nil {
		return nil, err
	}
	mats, err := s.courseMaterialUses(ctx, c.ID, nums)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(lecs))
	for i, l := range lecs {
		ids[i] = l.ID
	}
	counts, err := s.lectureCounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Lecture, 0, len(lecs))
	for _, l := range orderLectures(lecs, nums) {
		v, err := s.lectureViewJobs(ctx, l, lectureInfo{jobs: ix.of(model.ScopeLecture, l.ID), aux: ix.aux(model.ScopeLecture, l.ID),
			boundBusy: busy[l.ID], num: nums[l.ID], materials: mats[l.ID], counts: countsFor(counts, l.ID)})
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) materialView(ctx context.Context, m model.Material) (Material, error) {
	jobs, err := s.st.ListCurrentJobs(ctx, model.ScopeMaterial, m.ID)
	if err != nil {
		return Material{}, err
	}
	return s.materialViewJobs(m, jobs), nil
}

func (s *Server) materialViewJobs(m model.Material, jobs []model.Job) Material {
	return Material{
		ID: m.ID, CourseID: m.CourseID, LectureID: m.LectureID, Filename: m.Filename,
		Pages: len(m.PagesMD), CreatedAt: m.CreatedAt,
		Stages:        s.stageViews(model.ScopeMaterial, jobs, false, ""),
		CanDelete:     state.CanDelete(jobs),
		Bind:          materialBind(m),
		WeekSuggested: weekSuggested(m.Filename),
		TextWarning:   materialTextWarning(m.PagesMD),
	}
}

// syllabusMDPath is data/courses/{id}/SYLLABUS.md (A8.2).
func (s *Server) syllabusMDPath(courseID string) string {
	return filepath.Join(model.CourseDir(s.dataDir, courseID), "SYLLABUS.md")
}

// syllabusRawPath is data/courses/{id}/syllabus.raw.txt (A8.2).
func (s *Server) syllabusRawPath(courseID string) string {
	return filepath.Join(model.CourseDir(s.dataDir, courseID), "syllabus.raw.txt")
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// courseView is GET /api/courses/{id}: glossary_seed stage, materials with
// their stages, counts (SPEC §8.1), the v1.1 aggregates and the v2 janitor.
// It reads the jobs of this course only (courseJobs).
func (s *Server) courseView(ctx context.Context, c model.Course) (Course, error) {
	rd, err := s.readCourses(ctx, []string{c.ID}, true)
	if err != nil {
		return Course{}, err
	}
	ix, err := s.courseJobs(ctx, c, rd)
	if err != nil {
		return Course{}, err
	}
	return s.courseViewJobs(ctx, c, ix, s.janitorConfigured(ctx), rd)
}

// courseAgg are the lecture-derived aggregates of a course (Course and CourseCard).
type courseAgg struct {
	lectures       int
	drafts         int
	stale          int
	running        int
	failed         int // launched lectures with a failed stage (failedLecture)
	patchesPending int
	lastActivity   string
	anyExportDone  bool
}

// failedLecture reports whether a lecture counts as failed (v3.2.x r7,
// T24 S2): launched (a draft is not on the dashboard either) and some
// current stage failed for good — the dashboard's failures, without their
// 24-hour window. Cancelled stages are not failures.
func failedLecture(l model.Lecture, jobs []model.Job) bool {
	return !state.IsDraft(l) && anyStatus(jobs, model.StatusFailed)
}

func maxTime(cur string, ts ...*string) string {
	for _, t := range ts {
		if t != nil && *t > cur {
			cur = *t
		}
	}
	return cur
}

// aggregate computes courseAgg over the course's lectures; pend holds the
// pending patch counts by lecture id (courseReads.pending, absent = 0) and
// pending[l.ID] gets each lecture's count (filled here).
func aggregate(c model.Course, lecs []model.Lecture, ix jobIndex, pend, pending map[string]int) courseAgg {
	a := courseAgg{lectures: len(lecs), lastActivity: c.CreatedAt}
	for _, l := range lecs {
		jobs := ix.of(model.ScopeLecture, l.ID)
		if state.IsDraft(l) {
			a.drafts++
		}
		if anyStatus(jobs, model.StatusStale) {
			a.stale++
		}
		if anyStatus(jobs, model.StatusQueued, model.StatusRunning) {
			a.running++
		}
		if failedLecture(l, jobs) {
			a.failed++
		}
		if e, ok := findJob(jobs, model.StageExport); ok && e.Status == model.StatusDone {
			a.anyExportDone = true
		}
		n := pend[l.ID]
		if pending != nil {
			pending[l.ID] = n
		}
		a.patchesPending += n
		created := l.CreatedAt
		a.lastActivity = maxTime(a.lastActivity, &created, l.LaunchedAt)
		for _, j := range jobs {
			a.lastActivity = maxTime(a.lastActivity, j.StartedAt, j.FinishedAt)
		}
	}
	for _, j := range ix.of(model.ScopeCourse, c.ID) {
		a.lastActivity = maxTime(a.lastActivity, j.FinishedAt)
	}
	return a
}

func (a courseAgg) actions() []string {
	out := []string{}
	if a.drafts > 0 {
		out = append(out, CourseActionLaunchDrafts)
	}
	if a.stale > 0 {
		out = append(out, CourseActionUpdateStale)
	}
	if a.anyExportDone {
		out = append(out, CourseActionExportCite)
	}
	return out
}

// courseViewJobs builds a Course; janitor: state.JanitorConfigured, read
// once per request (course lists); rd: the course's reads (readCourses with
// views, possibly for many courses).
func (s *Server) courseViewJobs(ctx context.Context, c model.Course, ix jobIndex, janitor bool, rd *courseReads) (Course, error) {
	v := Course{ID: c.ID, Name: c.Name, CreatedAt: c.CreatedAt, GlossaryVersion: c.GlossaryVersion, Stages: []StageView{},
		Materials: []Material{}, Code: strOrNil(c.Code), Numbering: c.Numbering}
	if !model.ValidNumbering(v.Numbering) {
		v.Numbering = model.NumberingAuto
	}
	cjobs := ix.of(model.ScopeCourse, c.ID)
	// glossary_seed is shown with a syllabus only (SPEC §8.1); the v2
	// glossary_review whenever the course has that job (G3.1).
	shown := make([]model.Job, 0, len(cjobs))
	if sy, ok := rd.syllabi[c.ID]; ok {
		chars := 0
		if sy.Text != nil {
			chars = utf8.RuneCountInString(*sy.Text)
		}
		v.Syllabus = &Syllabus{Filename: sy.Filename, Chars: chars, Formatted: fileExists(s.syllabusMDPath(c.ID))}
		shown = cjobs
	} else if j, ok := findJob(cjobs, model.StageGlossaryReview); ok {
		shown = append(shown, j)
	}
	if len(shown) > 0 {
		v.Stages = s.stageViews(model.ScopeCourse, shown, false, "")
	}
	// Like state.Deps for asr: with a syllabus, glossary_seed must be done
	// (or stale); every material_terms likewise; and the v2 glossary_review
	// must not be waiting or running (lead ruling 3). Without a term
	// extraction role asr does not wait for the term jobs (3.1.0 "只转写",
	// sched transcribe_only.go), so they do not count.
	pending := false
	termsWait := s.set == nil || s.roleConfigured(model.RoleGlossary)
	if v.Syllabus != nil && termsWait {
		g, ok := findJob(cjobs, model.StageGlossarySeed)
		pending = !ok || (g.Status != model.StatusDone && g.Status != model.StatusStale)
	}
	review, hasReview := findJob(cjobs, model.StageGlossaryReview)
	if hasReview {
		switch review.Status {
		case model.StatusPending, model.StatusQueued, model.StatusRunning:
			pending = true
		}
	}
	for _, m := range rd.materials[c.ID] {
		jobs := ix.of(model.ScopeMaterial, m.ID)
		v.Materials = append(v.Materials, s.materialViewJobs(m, jobs))
		if mt, ok := findJob(jobs, model.StageMaterialTerms); termsWait && ok && mt.Status != model.StatusDone && mt.Status != model.StatusStale {
			pending = true
		}
	}
	v.GlossaryPending = pending
	v.TermCount = rd.terms[c.ID]
	a := aggregate(c, rd.lectures[c.ID], ix, rd.pending, nil)
	v.LectureCount = a.lectures
	v.DraftCount, v.StaleLectureCount, v.RunningCount, v.PatchesPending = a.drafts, a.stale, a.running, a.patchesPending
	v.FailedCount = a.failed
	v.LastActivityAt = a.lastActivity
	v.CourseActions = a.actions()
	v.Janitor = s.janitorStatus(ctx, rd.kv, c.ID, review, hasReview, janitor)
	if err := s.courseCalendarFields(ctx, rd.kv, &v, c, ix); err != nil {
		return Course{}, err
	}
	return v, nil
}

// courseCalendarFields fills the v2 P13 fields of a course view: the
// calendar, the file-name rule, whether a generated rule waits for the user
// (P13.3.3 "已识别文件名中的录制时间格式"), whether an automatic apply can be
// undone, the filename_rule task, and whether citations were exported
// (P13.6). kv values "" count as absent. kv is the store the kv entries
// are read from (courseReads.kv).
func (s *Server) courseCalendarFields(ctx context.Context, kv db.Store, v *Course, c model.Course, ix jobIndex) error {
	v.TermStart = strOrNil(c.TermStart)
	v.SkipWeeks = c.SkipWeeks
	if v.SkipWeeks == nil {
		v.SkipWeeks = []string{}
	}
	v.FilenameRule = parseRule(c.FilenameRule)
	var err error
	if _, v.FilenameProposal, err = kvValueIn(ctx, kv, model.KVFilenameProposal+c.ID); err != nil {
		return err
	}
	if _, v.FilenameUndo, err = kvValueIn(ctx, kv, model.KVFilenameUndo+c.ID); err != nil {
		return err
	}
	if j, ok := findJob(ix.aux(model.ScopeCourse, c.ID), model.StageFilenameRule); ok {
		t := s.auxView(j)
		v.FilenameTask = &t
	}
	if v.CiteExported, err = citeExportedIn(ctx, kv, c.ID); err != nil {
		return err
	}
	notices, err := stages.RenumberNotices(ctx, kv, c.ID)
	if len(notices) > 0 {
		v.RenumberNotices = notices
	}
	return err
}

// termView is a Term with the v2 fields (G1, G3.5, G6); unset values read as
// the column defaults (enforce, extractor), like the store writes them.
func termView(t model.Term) Term {
	tier, src := t.Tier, t.ZhSource
	if tier == "" {
		tier = model.TierEnforce
	}
	if src == "" {
		src = model.ZhSourceExtractor
	}
	return Term{ID: t.ID, En: t.En, Zh: t.Zh, Source: t.Source, Enabled: t.Enabled, Freq: t.Freq,
		Tier: tier, Locked: t.Locked, CommonWord: t.CommonWord, JanitorNote: t.JanitorNote, ZhSource: src}
}

func sentenceView(x model.Sentence, p *model.Patch) Sentence {
	v := Sentence{
		Idx: x.Idx, StartMS: x.StartMS, EndMS: x.EndMS, ASRText: x.ASRText, RepairedText: x.RepairedText,
		HumanText: x.HumanText, Text: x.Text, LowConfWords: make([]LowConfWord, 0, len(x.LowConfWords)),
		Zh: x.Zh, ZhStatus: x.ZhStatus, QAError: x.QAError,
	}
	if x.QAError != nil && *x.QAError != "" {
		v.QAErrorInfo = qa.ErrorInfo(*x.QAError)
	}
	for _, w := range x.LowConfWords {
		v.LowConfWords = append(v.LowConfWords, LowConfWord{I: w.I, W: w.W})
	}
	if v.ZhStatus == "" {
		v.ZhStatus = model.ZhNone
	}
	if p != nil {
		v.Patch = &PatchRef{ID: p.ID, Class: p.Class, Review: p.Review, Reason: p.Reason}
	}
	return v
}

// latestPatches maps sentence idx → its patch (the newest when several,
// like Store.PatchForSentence).
func latestPatches(ps []model.Patch) map[int]model.Patch {
	m := make(map[int]model.Patch, len(ps))
	for _, p := range ps {
		if cur, ok := m[p.SentenceIdx]; !ok || p.CreatedAt > cur.CreatedAt || (p.CreatedAt == cur.CreatedAt && p.ID > cur.ID) {
			m[p.SentenceIdx] = p
		}
	}
	return m
}

// patchView adds the review-card context (SPEC §9.7). sent returns the
// sentence at an index (ok=false when out of range).
func patchView(p model.Patch, sent func(idx int) (model.Sentence, bool)) Patch {
	v := Patch{
		ID: p.ID, LectureID: p.LectureID, SentenceIdx: p.SentenceIdx, Before: p.Before, After: p.After,
		Class: p.Class, Reason: p.Reason, Review: p.Review, EditedText: p.EditedText,
	}
	if x, ok := sent(p.SentenceIdx); ok {
		v.StartMS, v.EndMS, v.LowConfCount = x.StartMS, x.EndMS, len(x.LowConfWords)
	}
	if x, ok := sent(p.SentenceIdx - 1); ok {
		t := x.Text
		v.PrevText = &t
	}
	if x, ok := sent(p.SentenceIdx + 1); ok {
		t := x.Text
		v.NextText = &t
	}
	return v
}

// singlePatchView loads the three sentences around one patch.
func (s *Server) singlePatchView(ctx context.Context, p model.Patch) (Patch, error) {
	cache := map[int]*model.Sentence{}
	var firstErr error
	get := func(idx int) (model.Sentence, bool) {
		if idx < 0 {
			return model.Sentence{}, false
		}
		if x, ok := cache[idx]; ok {
			if x == nil {
				return model.Sentence{}, false
			}
			return *x, true
		}
		x, err := s.st.GetSentence(ctx, p.LectureID, idx)
		if err != nil {
			if !errors.Is(err, db.ErrNotFound) && firstErr == nil {
				firstErr = err
			}
			cache[idx] = nil
			return model.Sentence{}, false
		}
		cache[idx] = &x
		return x, true
	}
	v := patchView(p, get)
	return v, firstErr
}
