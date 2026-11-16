// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"

	"lectureflow/internal/media"
)

// Record directories built from database ids (AUD1 M-1, R77 mid-1, SEC44
// table B): an imported library supplies the ids, so every handler that
// writes into or removes a course, lecture or material directory first
// checks that the ids it is built from are single path elements
// (media.IDElement); a removal also checks that the directory is the
// record's own (media.OwnDir: not a sibling that differs only in case).

// checkRecordIDs is nil when every id is a single path element, else the
// error a write answers with (the request fails, nothing is written).
func checkRecordIDs(ids ...string) error {
	for _, id := range ids {
		if !media.IDElement(id) {
			return apiErrDetail(http.StatusInternalServerError, "server.internal", nil,
				"record id is not a single path element: "+strings.Join(ids, " / "))
		}
	}
	return nil
}

// removableRecordDir reports whether dir, built from ids, may be removed
// for its record: the ids single path elements, dir strictly inside
// data_dir, an ordinary directory, its parent's entry of exactly that name,
// with no link on the way down from data_dir (media.OwnDirIn; SEC49 L1).
// Nothing there: false (nothing to remove).
func (s *Server) removableRecordDir(dir string, ids ...string) bool {
	if checkRecordIDs(ids...) != nil || !s.inDataDir(dir) {
		return false
	}
	own, _ := media.OwnDirIn(s.dataDir, dir)
	return own
}

// checkRecordPath is nil when dir, a record directory under data_dir, has
// no link on its way down from data_dir (media.PlainPath; SEC49 L1), else
// the error a write answers with (the request fails, nothing is written).
func (s *Server) checkRecordPath(dir string) error {
	if !media.PlainPath(s.dataDir, dir) {
		return apiErrDetail(http.StatusInternalServerError, "server.internal", nil,
			"a folder on the way to the record's directory is not an ordinary directory (a link)")
	}
	return nil
}
