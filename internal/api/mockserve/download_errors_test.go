// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/fetch"
)

// The scenario download_errors has a task for every task error code of
// internal/fetch (the HTTP answers aside) and the four upload.media_*
// refusals; "downloads" keeps its ten tasks.
func TestMockDownloadErrorsScenario(t *testing.T) {
	srv := testServer(t)
	u := srv.URL

	if n := len(dlGet(t, u, "?mock=downloads")); n != 10 {
		t.Fatalf("downloads scenario: %d tasks, want 10", n)
	}
	items := dlGet(t, u, "?mock="+dlErrScenario)
	httpAnswers := map[string]bool{
		fetch.CodeNotFound: true, fetch.CodeActive: true, fetch.CodeNeedsRefresh: true, fetch.CodeNotRetryable: true,
		fetch.CodeNotCancellable: true, fetch.CodeConcurrencyInvalid: true, fetch.CodeConnectionsInvalid: true,
	}
	want := map[string]bool{"upload.media_empty": true, "upload.media_html": true, "upload.media_playlist": true, "upload.media_format": true}
	for _, c := range fetch.ErrorCodes {
		if !httpAnswers[c] {
			want[c] = true
		}
	}
	got := map[string]int{}
	retryable := map[bool]int{}
	schemes := map[string]bool{}
	for i, d := range items {
		if i > 0 && items[i-1].CreatedAt < d.CreatedAt {
			t.Errorf("not newest first at %d", d.ID)
		}
		if d.Error == nil {
			t.Errorf("task %d (%s) has no error", d.ID, d.State)
			continue
		}
		got[d.Error.Code]++
		refresh := strings.HasPrefix(d.Error.Code, "downloads.credentials_") || d.Error.Code == fetch.CodeRestarted
		switch {
		case refresh && (d.State != "needs_refresh" || len(d.AllowedActions) != 1 || d.AllowedActions[0] != "cancel"):
			t.Errorf("task %d %s: %s %v", d.ID, d.Error.Code, d.State, d.AllowedActions)
		case !refresh && (d.State != "failed" || !slices.Equal(d.AllowedActions, fetch.AllowedActions("failed", d.Error.Code))):
			t.Errorf("task %d %s: %s %v", d.ID, d.Error.Code, d.State, d.AllowedActions)
		}
		if !refresh {
			retryable[fetch.Retryable(d.Error.Code)]++
		}
		if d.Error.Code == fetch.CodeProxyUnsupported {
			s, _ := d.Error.Params["scheme"].(string)
			schemes[s] = true
		}
		for _, k := range []string{"host"} {
			if h, ok := d.Error.Params[k].(string); ok && !reservedHost(h) {
				t.Errorf("task %d: host %q is not a reserved name", d.ID, h)
			}
		}
	}
	for c := range want {
		if got[c] == 0 {
			t.Errorf("no task with %s", c)
		}
	}
	for c := range got {
		if !want[c] {
			t.Errorf("unexpected code %s", c)
		}
	}
	if !schemes["socks5h"] || !schemes["https"] {
		t.Errorf("proxy_unsupported schemes %v", schemes)
	}
	// both kinds of failure are there (T18 N2, M1: internal/fetch failedRetry)
	if retryable[true] == 0 || retryable[false] == 0 {
		t.Errorf("retryable / not: %v", retryable)
	}
	// the two scenarios do not share their tasks
	code, b, _ := mockDo(t, "DELETE", u+"/api/downloads/60?mock="+dlErrScenario, "")
	if code != http.StatusConflict { // needs_refresh: cancel first
		t.Fatalf("delete needs_refresh: %d %s", code, b)
	}
	if code, b, _ := mockDo(t, "DELETE", u+"/api/downloads/64?mock="+dlErrScenario, ""); code != http.StatusNoContent {
		t.Fatalf("delete failed: %d %s", code, b)
	}
	if n := len(dlGet(t, u, "?mock="+dlErrScenario)); n != len(items)-1 {
		t.Errorf("after remove: %d", n)
	}
	if n := len(dlGet(t, u, "?mock=downloads")); n != 10 {
		t.Errorf("downloads scenario after a remove in download_errors: %d", n)
	}
	if n := len(dlGet(t, u, "")); n != 0 {
		t.Errorf("default scenario: %d downloads", n)
	}
}

// T18 N2, M1: the mock offers and refuses retries as internal/fetch does.
func TestMockDownloadErrorsRetry(t *testing.T) {
	srv := testServer(t)
	u := srv.URL
	items := dlGet(t, u, "?mock="+dlErrScenario)
	// a retry the task does not offer is refused with its reason, as
	// internal/fetch does (409); an offered one goes on
	for _, d := range items {
		if d.State != "failed" || d.Error == nil {
			continue
		}
		code, b, _ := mockDo(t, "POST", fmt.Sprintf("%s/api/downloads/%d/retry?mock=%s", u, d.ID, dlErrScenario), "")
		if fetch.Retryable(d.Error.Code) {
			if code != http.StatusOK {
				t.Errorf("retry %d %s: %d %s", d.ID, d.Error.Code, code, b)
			}
			continue
		}
		var body struct {
			Error struct {
				Code   string         `json:"code"`
				Params map[string]any `json:"params"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(b), &body)
		if code != http.StatusConflict || body.Error.Code != d.Error.Code || len(body.Error.Params) != len(d.Error.Params) {
			t.Errorf("retry %d %s: %d %s", d.ID, d.Error.Code, code, b)
		}
	}
	items = dlGet(t, u, "?mock="+dlErrScenario)
	for _, d := range items {
		if d.State == "failed" && fetch.Retryable(d.Error.Code) {
			t.Errorf("task %d %s was not retried", d.ID, d.Error.Code)
		}
	}
}

func reservedHost(h string) bool {
	h = strings.TrimPrefix(h, "ftp://")
	if h == "localhost" {
		return true
	}
	for _, suf := range []string{".example", ".test", ".invalid", "example.com"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}
