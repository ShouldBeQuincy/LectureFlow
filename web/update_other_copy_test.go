// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/selfupdate"
)

// The refusal "another copy of LectureFlow has an update waiting"
// (selfupdate.other_copy_pending, r6; review SEC40): the text says the
// update is ready (it may have come from files, not a download), which
// copy it belongs to — {kind} is a code of its own
// (selfupdate.copy_installer / copy_portable) that util.js renders in the
// same namespace — and when this copy stops waiting, with the number of
// days the back end uses. zh-CN and en; the other locales fall back to en
// until translated. The rendered sentences are checked in
// testdata/w4_text_test.mjs.
func TestOtherCopyPendingText(t *testing.T) {
	days := strconv.Itoa(selfupdate.OtherPendingMaxDays)
	key := "errors." + selfupdate.CodeOtherCopyPending
	parts := []string{"errors." + selfupdate.CodeCopyInstaller, "errors." + selfupdate.CodeCopyPortable}
	for _, loc := range []string{"zh-CN", "en"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, "i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		text, _ := m[key].(string)
		for _, want := range []string{"{kind}", "{version}", days} {
			if !strings.Contains(text, want) {
				t.Errorf("%s %s lacks %q: %q", loc, key, want, text)
			}
		}
		for _, gone := range []string{"下载", "download"} {
			if strings.Contains(strings.ToLower(text), gone) {
				t.Errorf("%s %s still says the update was downloaded: %q", loc, key, text)
			}
		}
		seen := map[string]bool{}
		for _, p := range parts {
			name, _ := m[p].(string)
			if name == "" || strings.Contains(name, "{") || seen[name] {
				t.Errorf("%s %s: %q", loc, p, name)
			}
			seen[name] = true
		}
	}
	var ctx map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readWeb(t, "i18n/context.json")), &ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range append([]string{key}, parts...) {
		if _, ok := ctx[k]; !ok {
			t.Errorf("context.json has no %s", k)
		}
	}
	// util.js shows a param that is a coded text as that code's text
	if util := readWeb(t, "util.js"); !strings.Contains(util, "typeof v.code === 'string') return codedText(ns, v, '');") {
		t.Error("util.js no longer renders a coded param in its namespace")
	}
}
