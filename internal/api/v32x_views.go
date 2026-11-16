// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"slices"

	"lectureflow/internal/model"
)

// llmView is StageView.llm (and JobEvent.llm) of job j (v3.2.x P1-c): a
// copy of job.stats.llm. The request in flight (in_flight, current) belongs
// to a running job only: a job that is not running shows neither — a crash
// may have left them in the stored stats of a job that recovery put back in
// line (R2 review, question 4) — and keeps its finished attempts and
// retries. nil when nothing is left to show.
func llmView(j model.Job) *model.LLMStats {
	src := j.Stats.LLM
	if src == nil {
		return nil
	}
	out := model.LLMStats{InFlight: src.InFlight, Retries: src.Retries}
	if len(src.Attempts) > 0 {
		out.Attempts = make([]model.LLMAttempt, len(src.Attempts))
		for i, a := range src.Attempts {
			out.Attempts[i] = copyAttempt(a)
		}
	}
	if j.Status == model.StatusRunning {
		if src.Current != nil {
			c := copyAttempt(*src.Current)
			out.Current = &c
		}
	} else {
		out.InFlight = 0
	}
	if out.InFlight == 0 && out.Current == nil && len(out.Attempts) == 0 && out.Retries == 0 {
		return nil
	}
	return &out
}

// copyAttempt copies a, including the value behind its pointer.
func copyAttempt(a model.LLMAttempt) model.LLMAttempt {
	if a.ReasoningTokens != nil {
		v := *a.ReasoningTokens
		a.ReasoningTokens = &v
	}
	return a
}

// lastActivity is StageView.last_activity_at (and JobEvent's): when the run
// last showed life (job.stats.last_activity_at), nil without one.
func lastActivity(j model.Job) *string {
	if j.Stats.LastActivityAt == "" {
		return nil
	}
	v := j.Stats.LastActivityAt
	return &v
}

// withRequestStats fills the v3.2.x fields of a stage view of job j.
func withRequestStats(v *StageView, j model.Job) {
	v.LLM = llmView(j)
	v.LastActivityAt = lastActivity(j)
}

// jobEventStats fills the v3.2.x fields of the SSE job event of j (SPEC
// §8.2 as revised for v3.2.x P1-c): only for a running job whose stats the
// scheduler sent with it (tokens, request details or a last activity). The
// stats are those of StageView.stats (s.stageUsage: the same estimated
// cost, the Colab sub-step of a running asr job).
func (s *Server) jobEventStats(ev *JobEvent, j model.Job) {
	if j.Status != model.StatusRunning || !hasRunStats(j.Stats) {
		return
	}
	u := s.stageUsage(j)
	ev.Stats = &u
	ev.LLM = llmView(j)
	ev.LastActivityAt = lastActivity(j)
}

// hasRunStats reports whether st holds anything of a run: tokens, request
// details, a last activity or a Colab sub-step.
func hasRunStats(st model.JobStats) bool {
	return st.PromptTokens > 0 || st.CompletionTokens > 0 || st.CacheHitTokens > 0 ||
		st.LLM != nil || st.LastActivityAt != "" || st.Remote != nil
}

// stageUsage is StageView.stats of job j: usageOf with the Colab sub-step
// of a running asr job (SPEC-v3 V14.5) and the pending sentences of a
// translate job (v3.2.x P1; whatever its status: a finished run keeps
// them until a later run clears them) and the models its translations
// come from when they mix (v3.2.x P1-b, the same way).
func (s *Server) stageUsage(j model.Job) UsageStats {
	u := s.usageOf(j)
	if j.Stage == model.StageASR && j.Status == model.StatusRunning && j.Stats.Remote != nil {
		rs := *j.Stats.Remote
		if rs.Reconnect != nil { // v3.2.x: the reconnect state, as the stats hold it
			rc := *rs.Reconnect
			rs.Reconnect = &rc
		}
		u.Remote = &rs
	}
	if j.Stage == model.StageTranslate && j.Stats.PendingSentences > 0 {
		u.PendingSentences = j.Stats.PendingSentences
	}
	if j.Stage == model.StageTranslate && len(j.Stats.MixedModels) > 0 { // v3.2.x P1-b
		u.MixedModels = slices.Clone(j.Stats.MixedModels)
	}
	return u
}
