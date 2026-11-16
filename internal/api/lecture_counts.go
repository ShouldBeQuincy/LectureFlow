// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Lecture counts of the lists (perf-fix): the dashboard, a course's lecture
// list and the course list read the sentence, translation and patch counts
// of many lectures at once through db.Counter (a few GROUP BY queries)
// instead of four queries per lecture. A store without it (tests'
// wrappers) is read lecture by lecture, with the same results.

// counter is the store's batch counts, nil when it has none.
func (s *Server) counter() db.Counter {
	c, _ := s.st.(db.Counter)
	return c
}

// lectureCounts returns the counts of the lectures ids for their views
// (lectureInfo.counts); nil without a db.Counter, and lectureViewJobs then
// reads each lecture itself (lectureCountsOne).
// Each id is asked once (R77 info-1: the dashboard may list a lecture twice,
// and the store adds the sentence counts of an id met in two chunks).
func (s *Server) lectureCounts(ctx context.Context, ids []string) (map[string]db.LectureCounts, error) {
	c := s.counter()
	if c == nil || len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(ids))
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	return c.LectureCounts(ctx, uniq)
}

// countsFor is the entry of id in m, nil when m has none.
func countsFor(m map[string]db.LectureCounts, id string) *db.LectureCounts {
	c, ok := m[id]
	if !ok {
		return nil
	}
	return &c
}

// lectureCountsOne is db.LectureCounts of one lecture through the Store's
// per-lecture methods. As the views always did, the first sentence is
// read only when the last one has no words; a sentence that cannot be read
// (other than not found) is the view's error only when the view reads it
// (SentenceEnd.Err).
func (s *Server) lectureCountsOne(ctx context.Context, id string) (db.LectureCounts, error) {
	var c db.LectureCounts
	var err error
	if c.Sentences, err = s.st.CountSentences(ctx, id); err != nil {
		return c, err
	}
	end := func(idx int) db.SentenceEnd {
		x, err := s.st.GetSentence(ctx, id, idx)
		switch {
		case errors.Is(err, db.ErrNotFound):
			return db.SentenceEnd{}
		case err != nil:
			return db.SentenceEnd{Err: err}
		}
		return db.SentenceEnd{Found: true, EndMS: x.EndMS, HasWords: len(x.Words) > 0}
	}
	if c.Sentences > 0 {
		c.Last = end(c.Sentences - 1)
		if c.Last.Err == nil && !c.Last.HasWords {
			c.First = end(0)
		}
	}
	if c.PatchesPending, err = s.st.CountPatches(ctx, id, model.ReviewPending); err != nil {
		return c, err
	}
	if c.Zh, err = s.st.CountZh(ctx, id); err != nil {
		return c, err
	}
	return c, nil
}

// pendingPatchCounts is CountPatches(…, pending) of every lecture in lecs
// (absent = 0).
func (s *Server) pendingPatchCounts(ctx context.Context, lecs []model.Lecture) (map[string]int, error) {
	ids := make([]string, len(lecs))
	for i, l := range lecs {
		ids[i] = l.ID
	}
	if c := s.counter(); c != nil {
		if len(ids) == 0 {
			return map[string]int{}, nil
		}
		return c.PendingPatchCounts(ctx, ids)
	}
	out := make(map[string]int, len(ids))
	for _, id := range ids {
		n, err := s.st.CountPatches(ctx, id, model.ReviewPending)
		if err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, nil
}

// termCounts is len(ListTerms) of every course in ids (absent = 0).
func (s *Server) termCounts(ctx context.Context, ids []string) (map[string]int, error) {
	if c := s.counter(); c != nil {
		if len(ids) == 0 {
			return map[string]int{}, nil
		}
		return c.TermCounts(ctx, ids)
	}
	out := make(map[string]int, len(ids))
	for _, id := range ids {
		terms, err := s.st.ListTerms(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = len(terms)
	}
	return out, nil
}
