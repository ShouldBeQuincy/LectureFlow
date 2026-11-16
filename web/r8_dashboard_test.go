// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/fetch"
)

// R76 L-10 (round 8 WD-12): a download that failed keeps its credentials
// for a retry for internal/fetch's DefaultFailedCredentialTTL, and app.js
// says how long in the notification ("{minutes} 分钟内…", its
// FAILED_CREDENTIAL_MINUTES). The back end gives that time nowhere with a
// failed task (only errors.downloads.credentials_cleared's minutes, once it
// has passed), so the two constants are held together here.
func TestR8FailedCredentialMinutes(t *testing.T) {
	m := regexp.MustCompile(`(?m)^const FAILED_CREDENTIAL_MINUTES = (\d+);`).FindStringSubmatch(readWeb(t, "app.js"))
	if m == nil {
		t.Fatal("app.js: no `const FAILED_CREDENTIAL_MINUTES = <minutes>;`")
	}
	got, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	ttl := fetch.DefaultFailedCredentialTTL
	if ttl%time.Minute != 0 {
		t.Fatalf("fetch.DefaultFailedCredentialTTL is %v, not whole minutes: the notification's minutes cannot say it", ttl)
	}
	if want := int(ttl / time.Minute); got != want {
		t.Errorf("app.js FAILED_CREDENTIAL_MINUTES is %d, fetch.DefaultFailedCredentialTTL %d minutes: the notification would say the wrong time", got, want)
	}
}

// Round 8, the workbench line (r8-web-dash): the workbench, the course card
// and the page shell. The behaviour is checked on the DOM stand-in of
// testdata/r7_dom.mjs: the workbench and the course card by
// testdata/r8_dashboard_test.mjs, the page shell (app.js, booted whole) by
// testdata/r8_app_test.mjs.
func TestR8Dashboard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r8_dashboard_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r8_dashboard_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r8 dashboard checks: ok") {
		t.Fatalf("r8_dashboard_test.mjs: %v\n%s", err, out)
	}
}

func TestR8App(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r8_app_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r8_app_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r8 app checks: ok") {
		t.Fatalf("r8_app_test.mjs: %v\n%s", err, out)
	}
}
