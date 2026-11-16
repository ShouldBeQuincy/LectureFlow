// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"fmt"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// R77 info-1: a lecture listed twice in one request (the dashboard lists a
// lecture both in progress and among the recent ones) is counted once, also
// when its two places fall into different chunks of the batch counts (400
// ids each): the sentence count used to be added twice.
func TestLectureCountsDuplicateIDs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("Operating Systems")
	l := e.lecture(c.ID, "Deadlocks")
	if err := e.st.Tx(ctx, func(tx db.Store) error {
		return tx.InsertSentences(ctx, l.ID, []model.Sentence{
			{Idx: 0, EndMS: 1000, ASRText: "a", ZhStatus: model.ZhNone},
			{Idx: 1, StartMS: 1000, EndMS: 2000, ASRText: "b", ZhStatus: model.ZhNone}})
	}); err != nil {
		t.Fatal(err)
	}
	ids := []string{l.ID}
	for i := 1; i < 400; i++ {
		ids = append(ids, fmt.Sprintf("none-%d", i))
	}
	ids = append(ids, l.ID, l.ID) // the next chunk, and twice in it
	got, err := e.srv.lectureCounts(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if n := got[l.ID].Sentences; n != 2 {
		t.Errorf("sentences = %d, want 2", n)
	}
	if n := got[l.ID].Zh[model.ZhNone]; n != 2 {
		t.Errorf("zh none = %d, want 2", n)
	}
}
