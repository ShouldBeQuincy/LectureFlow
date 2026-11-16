// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"testing"

	"lectureflow/internal/db"
)

// TestChaptersInputHashPinnedFromStore pins ChaptersInputHash as the API
// computes it (review R26 A-7, backlog T2): the summary and the sentences
// read from the database (state.ChaptersInput), hashed by
// ctxbuild.ChaptersInputHash. ctxbuild pins the hash function on its own
// (TestChaptersInputHashPinnedCases); this test also pins what is read —
// the stored summary text and every sentence.text (not asr_text) of the
// sample lecture (the repository's testdata/sample_sentences.json, 14
// sentences), and that a human edit counts. (Each value is the SHA-256 of
// "8:chapters" + len:summary + len:"14" + len:text per sentence, lengths in
// bytes; checked once against an independent computation.)
//
// Every lecture stores this hash with its chapters; the API compares it with
// this value. If it changes, the chapters of every existing lecture show
// "章节可能已过时" — change the input or the hash only on purpose. (A change
// of the sample file changes these values too; that is not a hash change.)
func TestChaptersInputHashPinnedFromStore(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	l := e.chaptersLecture()
	h, err := ChaptersInputHash(e.ctx, d, l.ID)
	e.must(err)
	if want := "2ec714d99422081a26ac518041b4435e8e69a2f40d56badb2099f8941c10487a"; h != want {
		t.Errorf("sample lecture: ChaptersInputHash changed: %s", h)
	}

	human := "Ein Cache — 缓存 — speichert\nZeilen."
	e.must(e.st.Tx(e.ctx, func(tx db.Store) error {
		if err := tx.SetHumanText(e.ctx, l.ID, 3, &human); err != nil {
			return err
		}
		return tx.RecomputeText(e.ctx, l.ID, []int{3})
	}))
	h, err = ChaptersInputHash(e.ctx, d, l.ID)
	e.must(err)
	if want := "4b3c1f26e355266598b595f83f5fab727e0a1c2e092c248bb887016c6aa78d9d"; h != want {
		t.Errorf("sample lecture with a human edit: ChaptersInputHash changed: %s", h)
	}
}
