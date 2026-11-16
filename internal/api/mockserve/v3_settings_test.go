// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/api"
)

func mockDo(t *testing.T, method, url, body string) (int, []byte, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, res.Header
}

func TestMockAppSettingsSection(t *testing.T) {
	oldCopy, oldVerify, oldGap := setCopyDuration, setVerifyDuration, setRestartGap
	setCopyDuration, setVerifyDuration, setRestartGap = 200*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { setCopyDuration, setVerifyDuration, setRestartGap = oldCopy, oldVerify, oldGap })
	// The move's clock is stepped by the test, 20 ms per poll: polling the
	// wall clock, a loaded machine could sleep past the 100 ms verifying or
	// restart window and miss that state (flaky under go test -p 4).
	var clockMu sync.Mutex
	clock := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	oldNow := setNow
	setNow = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	t.Cleanup(func() { setNow = oldNow })
	advance := func(d time.Duration) { clockMu.Lock(); clock = clock.Add(d); clockMu.Unlock() }
	srv := testServer(t)
	u := srv.URL

	code, b, _ := mockDo(t, "PATCH", u+"/api/app/settings?mock=1", `{"close_behavior":"quit","prevent_sleep":false}`)
	var s api.AppSettings
	if code != 200 || json.Unmarshal(b, &s) != nil || s.CloseBehavior != "quit" || s.PreventSleep || s.UpdateChannel != "stable" {
		t.Fatalf("patch: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "PATCH", u+"/api/app/settings?mock=1", `{"update_channel":"nightly"}`); code != 400 || !strings.Contains(string(b), "update_channel_invalid") {
		t.Errorf("bad channel: %d %s", code, b)
	}

	if code, b, _ = mockDo(t, "POST", u+"/api/app/data-dir?mock=1", `{"path":"E:\\busy"}`); code != 409 || !strings.Contains(string(b), "data_dir.busy") {
		t.Errorf("busy: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "POST", u+"/api/app/data-dir?mock=1", `{"path":"E:\\LectureFlow"}`); code != 202 || !strings.Contains(string(b), `"copying"`) {
		t.Fatalf("move: %d %s", code, b)
	}
	seen := map[string]bool{}
	deadline := setNow().Add(3 * time.Second)
	for setNow().Before(deadline) {
		code, b, _ = mockDo(t, "GET", u+"/api/app/data-dir?mock=1", "")
		if code == 503 {
			seen["restart"] = true
		} else {
			var st api.DataDirStatus
			if err := json.Unmarshal(b, &st); err != nil {
				t.Fatal(err)
			}
			seen[st.State] = true
			if st.State == "idle" && seen["restart"] {
				if st.DataDir != `E:\LectureFlow` {
					t.Errorf("after the restart: %+v", st)
				}
				break
			}
		}
		advance(20 * time.Millisecond)
	}
	if !seen["copying"] || !seen["verifying"] || !seen["restart"] || !seen["idle"] {
		t.Errorf("states seen: %v", seen)
	}
	var old api.OldDataDir
	_, b, _ = mockDo(t, "GET", u+"/api/app/old-data-dir?mock=1", "")
	if json.Unmarshal(b, &old) != nil || old.Path != setDataDir || !old.Exists {
		t.Errorf("old dir: %s", b)
	}
	if code, b, _ = mockDo(t, "DELETE", u+"/api/app/old-data-dir?mock=1", `{"path":"C:\\elsewhere"}`); code != 409 {
		t.Errorf("mismatch: %d %s", code, b)
	}
	if code, b, _ = mockDo(t, "DELETE", u+"/api/app/old-data-dir?mock=1", `{"path":`+string(mustJSON(t, old.Path))+`}`); code != 200 || !strings.Contains(string(b), `"path":""`) {
		t.Errorf("delete: %d %s", code, b)
	}

	code, b, hd := mockDo(t, "POST", u+"/api/app/diagnostics?mock=1", "")
	if code != 200 || !strings.HasPrefix(hd.Get("Content-Disposition"), `attachment; filename="lectureflow-diagnostics-`) {
		t.Fatalf("diagnostics: %d %v", code, hd)
	}
	if _, err := zip.NewReader(bytes.NewReader(b), int64(len(b))); err != nil {
		t.Errorf("not a zip: %v", err)
	}
	if code, b, _ = mockDo(t, "GET", u+"/api/app/license?mock=1", ""); code != 200 || !bytes.Contains(b, []byte("GNU AFFERO")) {
		t.Errorf("license: %d", code)
	}
	if code, b, _ = mockDo(t, "GET", u+"/api/app/notices?mock=1", ""); code != 200 || !bytes.Contains(b, []byte("Third-Party Notices")) {
		t.Errorf("notices: %d", code)
	}
	if code, _, _ = mockDo(t, "POST", u+"/api/app/open-logs?mock=1", ""); code != 200 {
		t.Errorf("open logs: %d", code)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
