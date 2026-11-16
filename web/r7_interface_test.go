// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// T24 B1: the failures that a plain retry cannot get past and that another
// model can (the model refused, the total time limit) have one name in
// util.js, which the lecture page, the stage cards, the dashboard and the
// lecture rows share. The names are an interface: codes may be added.
func TestRetryNeedsModelInterface(t *testing.T) {
	b, err := os.ReadFile("util.js")
	if err != nil {
		t.Fatal(err)
	}
	util := string(b)
	for _, want := range []string{
		"export const MODEL_RETRY_CODES = new Set([",
		"'llm.refusal'",
		"'llm.time_limit'",
		"export function retryNeedsModel(st) {",
		"return !!(st && st.error_info && MODEL_RETRY_CODES.has(st.error_info.code));",
	} {
		if !strings.Contains(util, want) {
			t.Errorf("util.js lacks %q", want)
		}
	}
	// the codes are ones the back end reports: each has its text
	for _, loc := range []string{"zh-CN", "en"} {
		b, err := os.ReadFile("i18n/" + loc + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, code := range []string{"llm.refusal", "llm.time_limit"} {
			if _, ok := m["errors."+code]; !ok {
				t.Errorf("%s.json has no text for errors.%s", loc, code)
			}
		}
	}
}
