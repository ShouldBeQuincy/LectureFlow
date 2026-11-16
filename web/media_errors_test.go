// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// internalPrep is the stage's internal name as a word ("preprocessing" is fine).
var internalPrep = regexp.MustCompile(`(^|[^A-Za-z])prep([^A-Za-z]|$)`)

// test-media I6: a stage that failed because the media file is damaged
// (media.corrupt) keeps its retry (allowed_actions, SPEC §4), but no view
// offers it as the primary action, and the stage card and the lecture page
// say to import a complete file instead.
func TestCorruptMediaRetryNotPrimary(t *testing.T) {
	read := func(f string) string {
		t.Helper()
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	util := read("util.js")
	for _, want := range []string{
		"const FUTILE_RETRY_CODES = new Set(['media.corrupt']);",
		"export function retryFutile(st) {",
	} {
		if !strings.Contains(util, want) {
			t.Errorf("util.js lacks %q", want)
		}
	}
	for f, wants := range map[string][]string{
		"pipeline.js": {
			"const futile = U.retryFutile(st);",
			"(a === 'retry' && !futile) || a === 'use_colab' ? 'btn btn-sm btn-primary'",
			"st.status === 'failed' && U.retryFutile(st) ? h('p', { class: 'card-errhint' }, t('common.pipe.corrupt_hint')) : null",
		},
		"view_lecture.js": {
			"const futile = failed && U.retryFutile(st);",
			"futile ? h('span', { class: 'lec-notice-hint' }, t('common.pipe.corrupt_hint')) : null",
			"kind: failed && !futile && !modelFirst && !fix ? 'primary' : ''",
		},
		"lecture_list.js":   {"(s.allowed_actions || []).includes('retry') && !U.retryFutile(s)"},
		"view_dashboard.js": {"(f.allowed_actions || []).includes('retry') && !U.retryFutile(f)"},
	} {
		src := read(f)
		for _, w := range wants {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %q", f, w)
			}
		}
	}
	// the hint and the import refusals have texts in every locale
	for _, loc := range []string{"zh-CN", "en", "zh-TW", "yue-Hant-HK", "ru", "hi"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(read("i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"common.pipe.corrupt_hint", "errors.upload.media_empty", "errors.upload.media_html"} {
			if s, _ := m[k].(string); strings.TrimSpace(s) == "" {
				t.Errorf("%s: no text for %s", loc, k)
			}
		}
		// the internal stage name stays out of the error texts
		for _, k := range []string{"errors.asr.no_speech", "errors.asr.audio_missing", "errors.colab.audio_missing"} {
			if s, _ := m[k].(string); internalPrep.MatchString(s) {
				t.Errorf("%s: %s names the internal stage: %q", loc, k, s)
			}
		}
	}
}
