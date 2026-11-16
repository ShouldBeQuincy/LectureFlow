// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"strings"

	"lectureflow/internal/api"
	"lectureflow/internal/calendar"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// The mock server keeps lectures as api.Lecture values and recomputes the
// derived fields after every change, with the same rules the real back end
// uses (SPEC §4.5 allowed actions, v1.1 A2.1 display_status, A2.2
// stale_text, A3 drafts). Only the raw stage status, progress, timing and
// usage are stored; everything below is derived.

// stageActions is SPEC §4.5.
func stageActions(status string) []string {
	switch status {
	case model.StatusQueued, model.StatusRunning:
		return []string{model.ActionCancel}
	case model.StatusDone, model.StatusStale:
		return []string{model.ActionRerun}
	case model.StatusFailed, model.StatusCancelled:
		return []string{model.ActionRetry}
	}
	return []string{}
}

// blocksDownstream: an upstream stage in one of these statuses makes a done
// downstream stage "outdated" (A2.1).
func blocksDownstream(status string) bool {
	switch status {
	case model.StatusStale, model.StatusPending, model.StatusQueued, model.StatusRunning, model.StatusFailed, model.StatusCancelled:
		return true
	}
	return false
}

// mockNoWordsLecture is the example lecture transcribed "before v2": its
// transcript has no word timestamps until asr is rerun (v2 P4.2).
const mockNoWordsLecture = "01JD2120000000000000000001"

// finalize is finalizeLecture plus what depends on the store's settings:
// v2 P13.5 regenerate_title, offered like the real views.go (summary done
// or stale, organizer role configured); v2 P4.2 has_words and fill_words
// (a transcript without word timestamps, asr done or stale; the mock's asr
// rerun is always allowed then, see stageActions). Must hold s.mu.
func (s *store) finalize(l *api.Lecture) {
	finalizeLecture(l)
	if sum := stageStatus(l, model.StageSummary); (sum == model.StatusDone || sum == model.StatusStale) && s.organizerConfigured() {
		l.LectureActions = append(l.LectureActions, model.LectureActionRegenerateTitle)
	}
	asr := stageStatus(l, model.StageASR)
	transcribed := asr == model.StatusDone || asr == model.StatusStale
	l.HasWords = transcribed && !s.noWords[l.ID]
	if transcribed && !l.HasWords && !l.Draft && l.Counts.Sentences > 0 {
		l.LectureActions = append(l.LectureActions, model.LectureActionFillWords)
	}
}

// finalizeLecture recomputes every derived field of l in place.
func finalizeLecture(l *api.Lecture) {
	l.Draft = l.LaunchedAt == nil
	// v2 P13.4: the number comes from the course calendar (numberCourse,
	// run by finalizeCourse); without one it is W??
	if l.Label == "" || !l.Numbered {
		l.Label = calendar.Unknown
	}
	running, active, stale := false, false, false
	var staleNames, staleIDs []string
	var staleReasons []string
	var staleInfos []model.StaleReasonCode
	upstreamBlocked := false
	usage := api.UsageStats{}
	var cost *float64
	for i := range l.Stages {
		st := &l.Stages[i]
		if st.Status == model.StatusDone {
			st.Progress = 1
		}
		if st.Status != model.StatusStale {
			st.StaleReason, st.StaleInfo = nil, nil
		}
		// only a queued asr stage shows its checkpoint (asr_checkpoint.go)
		if st.Status != model.StatusQueued {
			st.Checkpoint = nil
		}
		switch st.Status {
		case model.StatusPending:
			if l.Draft {
				w := model.WaitNotStarted
				st.WaitingFor = &w
			} else if i > 0 && (st.WaitingFor == nil || *st.WaitingFor == model.WaitNotStarted || l.Stages[i-1].Status != model.StatusDone) {
				w := model.WaitUpstream(l.Stages[i-1].Stage)
				st.WaitingFor = &w
			}
		case model.StatusQueued:
			// keep the scheduler's reason (fixture or mutation sets it)
		default:
			st.WaitingFor = nil
		}
		if l.Draft {
			st.AllowedActions = []string{}
			st.Model = nil
		} else {
			st.AllowedActions = stageActions(st.Status)
		}
		st.DisplayStatus = st.Status
		if st.Status == model.StatusDone && upstreamBlocked {
			st.DisplayStatus = model.DisplayOutdated
		}
		if blocksDownstream(st.Status) {
			upstreamBlocked = true
		}
		switch st.Status {
		case model.StatusRunning:
			running, active = true, true
		case model.StatusQueued:
			active = true
		case model.StatusStale:
			stale = true
			staleNames = append(staleNames, model.StageLabel(st.Stage))
			// every distinct reason, in pipeline order, as state.StaleText
			r := "输入已变化"
			if st.StaleReason != nil && *st.StaleReason != "" {
				r = *st.StaleReason
			}
			if !containsStr(staleReasons, r) {
				staleReasons = append(staleReasons, r)
				staleInfos = append(staleInfos, staleInfoOf(r))
			}
			// P14.3: the coded reason, as views.go decodes the stored one
			info := staleInfoOf(r)
			st.StaleInfo = &info
			staleIDs = append(staleIDs, st.Stage)
		}
		usage.PromptTokens += st.Stats.PromptTokens
		usage.CacheHitTokens += st.Stats.CacheHitTokens
		usage.CompletionTokens += st.Stats.CompletionTokens
		if st.Stats.EstimatedCost != nil {
			c := *st.Stats.EstimatedCost
			if cost != nil {
				c += *cost
			}
			cost = &c
		}
	}
	if cost != nil {
		c := round4(*cost)
		cost = &c
	}
	usage.EstimatedCost = cost
	l.Usage = usage
	l.CanDelete = !running
	l.Reviewable = !(stageStatus(l, model.StageRepair) == model.StatusRunning || stageStatus(l, model.StageTranslate) == model.StatusRunning)
	l.StaleText, l.Stale = nil, nil
	if stale {
		t := strings.Join(staleReasons, "；") + "，需要更新：" + strings.Join(staleNames, "、")
		l.StaleText = &t
		l.Stale = &api.StaleSummary{Reasons: staleInfos, Stages: staleIDs}
	}
	acts := []string{}
	if l.Draft {
		acts = append(acts, model.LectureActionLaunch)
	}
	if stale {
		acts = append(acts, model.LectureActionUpdate)
	}
	if active {
		acts = append(acts, model.LectureActionCancel)
	}
	// always, as the real views.go: a running lecture is deleted with force
	// (cancel first); can_delete tells the UI which case it is (A7)
	acts = append(acts, model.LectureActionDelete)
	l.LectureActions = acts
	if l.Exports == nil {
		l.Exports = []api.ExportFile{}
	}
	// v2 P1: playable once asr has a result; P8.1: the playback file is
	// ready unless the example data says otherwise (player.go simulates the
	// background generation of a "pending" one).
	asr := stageStatus(l, model.StageASR)
	l.Playable = asr == model.StatusDone || asr == model.StatusStale
	if l.Playable && l.Stream == nil {
		l.Stream = &api.StreamInfo{Status: "ready"}
	}
}

// staleInfoOf is the coded form of a stale reason text of the example data
// (state.StaleReasonFromText, the conversion the real back end applies to
// reasons stored before v2).
func staleInfoOf(text string) model.StaleReasonCode {
	return state.StaleReasonFromText(text)
}

func stageStatus(l *api.Lecture, stage string) string {
	for _, st := range l.Stages {
		if st.Stage == stage {
			return st.Status
		}
	}
	return ""
}

func stageIndex(l *api.Lecture, stage string) int {
	for i, st := range l.Stages {
		if st.Stage == stage {
			return i
		}
	}
	return -1
}

func round4(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}

// allDone reports whether every stage of the lecture is done.
func allDone(l *api.Lecture) bool {
	if len(l.Stages) == 0 {
		return false
	}
	for _, st := range l.Stages {
		if st.Status != model.StatusDone {
			return false
		}
	}
	return true
}

// finalFailure is a failed stage without a pending automatic retry.
func finalFailure(st api.StageView) bool {
	return st.Status == model.StatusFailed && st.WaitingFor == nil
}

// failedLecture: Course.failed_count counts it (v3.2.x r7, T24 S2) — a
// launched lecture with a stage that failed for good, at any time.
func failedLecture(l *api.Lecture) bool {
	if l.Draft {
		return false
	}
	for _, st := range l.Stages {
		if finalFailure(st) {
			return true
		}
	}
	return false
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
