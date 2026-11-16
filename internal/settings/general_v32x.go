// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import "lectureflow/internal/model"

// MaxLLMIdleTimeoutS is the largest general llm_idle_timeout_s (v3.2.x
// P1-c): one hour; 0 turns the idle timeout off.
const MaxLLMIdleTimeoutS = 3600

// MinLLMIdleTimeoutS is the shortest llm_idle_timeout_s other than 0 (off;
// SEC5 N4): SetGeneral accepts 0 or MinLLMIdleTimeoutS..MaxLLMIdleTimeoutS,
// a shorter value saved by an earlier version reads as it, and the LLM
// client raises a shorter one to it (llm.MinIdleTimeout, SEC4 L10). A
// variable only so tests of the LLM client can store a shorter one; nothing
// else changes it.
var MinLLMIdleTimeoutS = 30

// normalizeGeneral raises a stored llm_idle_timeout_s between 0 and
// MinLLMIdleTimeoutS (accepted before SEC5 N4) to the minimum, the
// value the LLM client has always used for it, so the settings page shows
// what applies and saving the block again passes the check.
func normalizeGeneral(g model.GeneralSettings) model.GeneralSettings {
	if v := g.LLMIdleTimeoutS; v != nil && *v > 0 && *v < MinLLMIdleTimeoutS {
		m := MinLLMIdleTimeoutS
		g.LLMIdleTimeoutS = &m
	}
	return g
}

// validIdleTimeoutS reports an allowed llm_idle_timeout_s: 0 (off) or
// MinLLMIdleTimeoutS..MaxLLMIdleTimeoutS.
func validIdleTimeoutS(v int) bool {
	return v == 0 || (v >= MinLLMIdleTimeoutS && v <= MaxLLMIdleTimeoutS)
}

// cloneGeneral returns g with its own copies of the optional values, so a
// caller of General cannot change the service's settings through them.
func cloneGeneral(g model.GeneralSettings) model.GeneralSettings {
	if g.AutoChapters != nil {
		v := *g.AutoChapters
		g.AutoChapters = &v
	}
	if g.LLMIdleTimeoutS != nil {
		v := *g.LLMIdleTimeoutS
		g.LLMIdleTimeoutS = &v
	}
	return g
}
