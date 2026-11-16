// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"lectureflow/internal/media"
)

// test-media I8 (R10 S5): the inline rename of a lecture (title or
// title_zh, the only place the UI edits a title) caps its input at the
// server's limit, so a long title is not cut silently on save. maxlength
// counts UTF-16 units and the server characters, so the input is never
// looser than the server.
func TestLectureTitleInputMaxLength(t *testing.T) {
	b, err := os.ReadFile("lecture_list.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		fmt.Sprintf("const TITLE_MAX = %d;", media.MaxTitleRunes),
		"class: 'input lrow-title-input',\n      value: draft,",
		"maxlength: String(TITLE_MAX),",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("lecture_list.js lacks %q", want)
		}
	}
	// the maxlength belongs to the rename input, not some other element
	i := strings.Index(src, "class: 'input lrow-title-input',")
	j := strings.Index(src, "maxlength: String(TITLE_MAX),")
	if i < 0 || j < i || strings.Contains(src[i:j], "h(") {
		t.Error("maxlength is not on the lecture title input")
	}
}
