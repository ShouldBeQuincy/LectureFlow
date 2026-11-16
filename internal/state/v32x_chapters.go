// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"errors"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
)

// ChaptersInput reads what a lecture's chapters are made from (SPEC-v2 P7):
// the summary ("" when none) and every sentence, in one transaction so
// that a hash of them describes exactly what was read. An unknown lecture
// → db.ErrNotFound. It may run inside or outside a transaction.
//
// The chapters stage builds its request and stored input_hash from it, and
// the scheduler (round 2 N39) compares that hash after an export; one
// shared read keeps the two from drifting apart.
func ChaptersInput(ctx context.Context, st db.Store, lectureID string) (string, []ctxbuild.Sentence, error) {
	var (
		summary string
		ss      []ctxbuild.Sentence
	)
	err := st.Tx(ctx, func(tx db.Store) error {
		if _, err := tx.GetLecture(ctx, lectureID); err != nil {
			return err
		}
		sum, err := tx.GetSummary(ctx, lectureID)
		switch {
		case err == nil:
			summary = sum.Text
		case errors.Is(err, db.ErrNotFound):
			summary = ""
		default:
			return err
		}
		rows, err := tx.ListSentences(ctx, lectureID, 0, 0)
		if err != nil {
			return err
		}
		ss = ctxbuild.FromModel(rows)
		return nil
	})
	return summary, ss, err
}
