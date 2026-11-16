// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package api

import "testing"

// This machine's own names of BUILTIN and NT AUTHORITY (in its language)
// come from their SIDs, and their principals stay in a who value.
func TestSystemDomainsOfSIDs(t *testing.T) {
	names := systemDomainsOfSIDs()
	if len(names) < 2 {
		t.Fatalf("names %q: want BUILTIN's and NT AUTHORITY's at least", names)
	}
	r := newAccountRedactor([]string{"WS-FICTION42"}, nil)
	for _, n := range names {
		in := `who="` + n + `\\Qelt Group"`
		if got := r.apply(in, false); got != in {
			t.Errorf("%q: %q", in, got)
		}
	}
}
