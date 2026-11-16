// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// v3.2.x additions of the API layer (web-builder; .claude/v32x-design.md
// 3.2, 3.3). Only optional fields: a client that does not know them sees
// the responses it saw before.

// RetranslateRequest is the optional body of
// POST /api/lectures/{id}/sentences/{idx}/retranslate (v3.2.x P1-b, Q10a).
// UseCurrent sends the sentence — one the model left without a usable
// translation ("待处理") — to the translate role's CURRENT model; the
// lecture's other translations are kept, so the lecture may then mix two
// models. Without it the operation is the one of SPEC §17.1, unchanged.
type RetranslateRequest struct {
	UseCurrent bool `json:"use_current,omitempty"`
}

// RetranslateResult is the response of that operation: the sentence and,
// only when use_current was asked for, whether the lecture's translate model
// changed by it (mixed: its earlier translations come from another model).
type RetranslateResult struct {
	Sentence
	Mixed *bool `json:"mixed,omitempty"`
}
