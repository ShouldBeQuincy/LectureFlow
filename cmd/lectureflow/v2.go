// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"path/filepath"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
)

// backfillOriginalFilenames gives the lectures created before v2 phase 2 an
// original_filename (P13.3.1; they have ""), once (kv
// model.KVOriginalFilenames). Best effort (NOTES.md, core-p2): the upload
// stored the file as media{ext} and, unless the user typed a title, used
// the file name without its extension as the title, so the name is the
// title plus the media's extension (the title alone when it already ends
// with it or there is no media path). The user may have renamed a lecture
// since; the result only serves as a sample for the filename rule
// (P13.3.2) and in the lecture details. Lectures that already have a name
// are never changed. Each lecture without a name also gets its title
// recorded as one of unknown origin (stages.MarkLegacyTitle). One
// transaction. Returns the number of lectures named.
func backfillOriginalFilenames(ctx context.Context, st db.Store) (int, error) {
	n := 0
	err := st.Tx(ctx, func(tx db.Store) error {
		if v, ok, err := tx.GetKV(ctx, model.KVOriginalFilenames); err != nil || (ok && v == "1") {
			return err
		}
		courses, err := tx.ListCourses(ctx)
		if err != nil {
			return err
		}
		for _, c := range courses {
			lecs, err := tx.ListLectures(ctx, c.ID)
			if err != nil {
				return err
			}
			for _, l := range lecs {
				if l.OriginalFilename != "" {
					continue
				}
				// created before v2 phase 2: a title with source filename
				// may be one the user typed (test-upgrade F7); the title
				// task keeps it while it is unchanged
				if err := stages.MarkLegacyTitle(ctx, tx, l); err != nil {
					return err
				}
				name := guessOriginalFilename(l)
				if name == "" {
					continue
				}
				if err := tx.SetLectureOriginalFilename(ctx, l.ID, name); err != nil {
					return err
				}
				n++
			}
		}
		return tx.SetKV(ctx, model.KVOriginalFilenames, "1")
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// guessOriginalFilename is the best-effort original file name of a lecture
// uploaded before v2: its title plus the extension of its media file.
func guessOriginalFilename(l model.Lecture) string {
	title := strings.TrimSpace(l.Title)
	ext := filepath.Ext(l.MediaPath)
	if title == "" || ext == "" || strings.HasSuffix(strings.ToLower(title), strings.ToLower(ext)) {
		return title
	}
	return title + ext
}
