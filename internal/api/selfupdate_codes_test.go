// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"slices"
	"testing"

	"lectureflow/internal/selfupdate"
)

// Every selfupdate code has its HTTP status (one without would go through
// the general error path); another copy's waiting update is a 409 like the
// other reasons why no update is started now (r6).
func TestSelfUpdateCodesHaveHTTPStatus(t *testing.T) {
	for _, c := range selfupdate.ErrorCodes {
		if slices.Contains(selfupdate.PartCodes, c) {
			// a name inside another code's text, never an answer
			if selfUpdateHTTP[c] != 0 {
				t.Errorf("%s is only part of a text; it has an HTTP status", c)
			}
			continue
		}
		if selfUpdateHTTP[c] == 0 {
			t.Errorf("%s has no HTTP status", c)
		}
	}
	if got := selfUpdateHTTP[selfupdate.CodeOtherCopyPending]; got != http.StatusConflict {
		t.Errorf("%s: status %d, want 409", selfupdate.CodeOtherCopyPending, got)
	}
}
