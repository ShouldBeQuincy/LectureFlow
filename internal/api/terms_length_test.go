// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/ctxbuild"
)

// AUD5 low-1: a manual term has the length limits of an extracted one
// (ctxbuild.TermEnMaxChars / TermZhMaxChars, in characters); over them the
// request is refused with a coded error naming the field, and nothing is
// added.
func TestCreateTermLengthLimits(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	en100 := strings.Repeat("a", ctxbuild.TermEnMaxChars)
	zh100 := strings.Repeat("密", ctxbuild.TermZhMaxChars)

	for _, tc := range []struct {
		en, zh, field string
	}{
		{en100 + "a", "钥", "en"},
		{"key", zh100 + "钥", "zh"},
	} {
		info := e.callErrInfo("POST", "/api/courses/"+c.ID+"/terms", map[string]string{"en": tc.en, "zh": tc.zh}, http.StatusBadRequest)
		if info.Code != "upload.field_too_long" || info.Params["field"] != tc.field {
			t.Errorf("%s too long: %+v", tc.field, info)
		}
	}
	var terms []Term
	e.call("GET", "/api/courses/"+c.ID+"/terms", nil, http.StatusOK, &terms)
	if len(terms) != 0 {
		t.Fatalf("refused terms were added: %+v", terms)
	}

	// at the limits (and with surrounding space, which is trimmed): added
	var term Term
	e.call("POST", "/api/courses/"+c.ID+"/terms", map[string]string{"en": " " + en100 + " ", "zh": zh100 + "\n"}, http.StatusCreated, &term)
	if term.En != en100 || term.Zh != zh100 {
		t.Errorf("term at the limits: %+v", term)
	}
}

// PATCH /api/terms/{id} has the same limit on zh (en cannot be changed): a
// longer zh is refused and the term is left as it was.
func TestPatchTermLengthLimit(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	var term Term
	e.call("POST", "/api/courses/"+c.ID+"/terms", map[string]string{"en": "key", "zh": "密钥"}, http.StatusCreated, &term)
	var before Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &before)

	zh100 := strings.Repeat("密", ctxbuild.TermZhMaxChars)
	info := e.callErrInfo("PATCH", "/api/terms/"+term.ID, map[string]any{"zh": zh100 + "钥"}, http.StatusBadRequest)
	if info.Code != "upload.field_too_long" || info.Params["field"] != "zh" {
		t.Errorf("zh too long: %+v", info)
	}
	// with another change in the same request: nothing is applied
	e.callErrInfo("PATCH", "/api/terms/"+term.ID, map[string]any{"zh": zh100 + "钥", "enabled": false}, http.StatusBadRequest)
	var after Course
	e.call("GET", "/api/courses/"+c.ID, nil, http.StatusOK, &after)
	var terms []Term
	e.call("GET", "/api/courses/"+c.ID+"/terms", nil, http.StatusOK, &terms)
	if len(terms) != 1 || terms[0].Zh != "密钥" || !terms[0].Enabled || after.GlossaryVersion != before.GlossaryVersion {
		t.Fatalf("a refused patch changed the term: %+v, version %d → %d", terms, before.GlossaryVersion, after.GlossaryVersion)
	}

	var patched Term
	e.call("PATCH", "/api/terms/"+term.ID, map[string]any{"zh": " " + zh100 + " "}, http.StatusOK, &patched)
	if patched.Zh != zh100 {
		t.Errorf("zh at the limit: %+v", patched)
	}
}
