// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// SPEC-v3 V8: the update relay between the desktop main process and the page.

func updateStatusOf(t *testing.T, b []byte) UpdateStatus {
	t.Helper()
	var st UpdateStatus
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("status %s: %v", b, err)
	}
	return st
}

func TestUpdateRelayFlow(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	do := func(method, path, body string) (*http.Response, []byte) {
		var b []byte
		if body != "" {
			b = []byte(body)
		}
		return e.doHeaders(method, path, b, mainProcessHeaders(path))
	}

	res, b := do("GET", "/api/app/update", "")
	if res.StatusCode != 200 || updateStatusOf(t, b).State != UpdateNone {
		t.Fatalf("initial: %d %s", res.StatusCode, b)
	}
	// Nothing to download or install yet.
	if res, b := do("POST", "/api/app/update/download", ""); res.StatusCode != 409 || errCode(t, b) != "update.not_available" {
		t.Fatalf("download without update: %d %s", res.StatusCode, b)
	}
	if res, b := do("POST", "/api/app/update/install-on-quit", ""); res.StatusCode != 409 || errCode(t, b) != "update.not_available" {
		t.Fatalf("install-on-quit without update: %d %s", res.StatusCode, b)
	}

	// The main process found 3.1.0; the page asks for it and to install on quit.
	if res, b := do("POST", "/api/app/update/state", `{"state":"available","version":"3.1.0"}`); res.StatusCode != 200 {
		t.Fatalf("report available: %d %s", res.StatusCode, b)
	}
	res, b = do("POST", "/api/app/update/download", "")
	if st := updateStatusOf(t, b); res.StatusCode != 202 || !st.DownloadRequested || st.Version != "3.1.0" {
		t.Fatalf("download: %d %s", res.StatusCode, b)
	}
	res, b = do("POST", "/api/app/update/install-on-quit", `{"enabled":true}`)
	if st := updateStatusOf(t, b); res.StatusCode != 200 || !st.InstallOnQuit {
		t.Fatalf("install-on-quit: %d %s", res.StatusCode, b)
	}
	// A repeated "available" report (the 6-hourly check) keeps both requests.
	_, b = do("POST", "/api/app/update/state", `{"state":"available","version":"3.1.0"}`)
	if st := updateStatusOf(t, b); !st.DownloadRequested || !st.InstallOnQuit {
		t.Fatalf("repeated report lost the requests: %s", b)
	}
	// The main process starts downloading: the request is consumed.
	_, b = do("POST", "/api/app/update/state", `{"state":"downloading","version":"3.1.0","percent":42.5}`)
	if st := updateStatusOf(t, b); st.DownloadRequested || !st.InstallOnQuit || st.Percent != 42.5 {
		t.Fatalf("downloading: %s", b)
	}
	_, b = do("POST", "/api/app/update/state", `{"state":"ready","version":"3.1.0"}`)
	if st := updateStatusOf(t, b); st.State != UpdateReady || st.Percent != 100 || !st.InstallOnQuit {
		t.Fatalf("ready: %s", b)
	}
	// The page can take the choice back.
	_, b = do("POST", "/api/app/update/install-on-quit", `{"enabled":false}`)
	if updateStatusOf(t, b).InstallOnQuit {
		t.Fatalf("install-on-quit off: %s", b)
	}
	_, b = do("GET", "/api/app/update", "")
	if st := updateStatusOf(t, b); st.State != UpdateReady || st.InstallOnQuit || st.UpdatedAt == "" {
		t.Fatalf("final: %s", b)
	}
}

func TestUpdateRelayNewVersionResetsRequests(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	post := func(path, body string) []byte {
		_, b := e.doHeaders("POST", path, []byte(body), mainProcessHeaders(path))
		return b
	}
	post("/api/app/update/state", `{"state":"available","version":"3.1.0"}`)
	post("/api/app/update/download", `{}`)
	post("/api/app/update/install-on-quit", `{}`)
	b := post("/api/app/update/state", `{"state":"available","version":"3.2.0"}`)
	if st := updateStatusOf(t, b); st.DownloadRequested || st.InstallOnQuit {
		t.Fatalf("a newer version needs a new decision: %s", b)
	}
	// A failed download can be asked for again; the version is kept.
	post("/api/app/update/download", `{}`)
	b = post("/api/app/update/state", `{"state":"error","error":{"code":"update.download_failed","detail":"C:\\Users\\x\\secret"}}`)
	st := updateStatusOf(t, b)
	if st.State != UpdateError || st.Version != "3.2.0" || st.Error == nil || st.Error.Code != "update.download_failed" || st.Error.Detail != "" {
		t.Fatalf("error report: %s", b)
	}
	if res, b := e.doWith("POST", "/api/app/update/download", nil, testToken, ""); res.StatusCode != 202 {
		t.Fatalf("retry after a failed download: %d %s", res.StatusCode, b)
	}
	b = post("/api/app/update/state", `{"state":"none"}`)
	if st := updateStatusOf(t, b); st.Version != "" || st.DownloadRequested || st.InstallOnQuit {
		t.Fatalf("none clears everything: %s", b)
	}
}

func TestUpdateStateValidation(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	for body, field := range map[string]string{
		`{"state":"installing"}`:                                                         "state",
		`{"state":"available"}`:                                                          "version",
		`{"state":"ready","version":"../../evil"}`:                                       "version",
		`{"state":"downloading","version":"3.1.0","percent":101}`:                        "percent",
		`{"state":"downloading","version":"3.1.0","percent":-1}`:                         "percent",
		`{"state":"error"}`:                                                              "error",
		`{"state":"error","error":{"code":"server.internal"}}`:                           "error",
		`{"state":"available","version":"3.1.0","error":{"code":"update.check_failed"}}`: "error",
	} {
		res, b := e.doHeaders("POST", "/api/app/update/state", []byte(body), mainProcessHeaders("/api/app/update/state"))
		if res.StatusCode != 400 || errCode(t, b) != "update.state_invalid" {
			t.Errorf("%s: %d %s", body, res.StatusCode, b)
			continue
		}
		var eb struct {
			Error struct {
				Params map[string]any `json:"params"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &eb)
		if eb.Error.Params["field"] != field {
			t.Errorf("%s: field %v, want %s", body, eb.Error.Params["field"], field)
		}
	}
	if res, b := e.doHeaders("POST", "/api/app/update/state", []byte(`not json`), mainProcessHeaders("/api/app/update/state")); res.StatusCode != 400 || errCode(t, b) != "request.bad_json" {
		t.Errorf("bad json: %d %s", res.StatusCode, b)
	}
}

func TestUpdateRelayNeedsTokenAndDesktopMode(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	if res, _ := e.doWith("POST", "/api/app/update/state", []byte(`{"state":"none"}`), "", ""); res.StatusCode != 401 {
		t.Fatalf("no token: %d", res.StatusCode)
	}
	web := newEnv(t)
	for _, p := range []string{"/api/app/update"} {
		if res, b := web.doWith("GET", p, nil, "", ""); res.StatusCode != 404 || errCode(t, b) != "api.not_found" {
			t.Fatalf("web mode %s: %d %s", p, res.StatusCode, b)
		}
	}
	for _, p := range []string{"/api/app/update/state", "/api/app/update/download", "/api/app/update/install-on-quit"} {
		if res, b := web.doWith("POST", p, []byte(`{}`), "", ""); res.StatusCode != 404 || errCode(t, b) != "api.not_found" {
			t.Fatalf("web mode %s: %d %s", p, res.StatusCode, b)
		}
	}
}

// mainProcessHeaders are the headers the desktop main process sends to path
// (desktop/src/main/apiclient.js): the token, and on POST
// /api/app/update/state the grant only the main process can compute
// (PathGrantHeader, as on POST /api/app/path-grants; AUD1 I-1).
func mainProcessHeaders(path string) map[string]string {
	h := map[string]string{TokenHeader: testToken}
	if path == "/api/app/update/state" {
		h[PathGrantHeader] = pathGrantSecret(testToken)
	}
	return h
}

// AUD1 I-1, SEC44 information-1: POST /api/app/update/state is the main
// process's report of the update mechanism; the page's requests carry the
// token too, so the main process's own grant is required as well: without
// it, or with a wrong one, 403 and nothing changes (a script in the page
// cannot show "a new version is ready"); with it, as before.
func TestUpdateStateNeedsTheMainProcessGrant(t *testing.T) {
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	const body = `{"state":"ready","version":"9.9.9"}`
	for name, grant := range map[string]string{
		"no grant":            "",
		"wrong grant":         pathGrantSecret("another-token"),
		"the token as grant":  testToken,
		"grant in upper case": strings.ToUpper(pathGrantSecret(testToken)),
	} {
		h := map[string]string{TokenHeader: testToken}
		if grant != "" {
			h[PathGrantHeader] = grant
		}
		res, b := e.doHeaders("POST", "/api/app/update/state", []byte(body), h)
		if res.StatusCode != http.StatusForbidden || errCode(t, b) != "auth.token_required" {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
		}
	}
	_, b := e.doWith("GET", "/api/app/update", nil, testToken, "")
	if st := updateStatusOf(t, b); st.State != UpdateNone || st.Version != "" {
		t.Fatalf("a refused report changed the state: %s", b)
	}
	res, b := e.doHeaders("POST", "/api/app/update/state", []byte(body), mainProcessHeaders("/api/app/update/state"))
	if st := updateStatusOf(t, b); res.StatusCode != http.StatusOK || st.State != UpdateReady || st.Version != "9.9.9" {
		t.Fatalf("the main process's report: %d %s", res.StatusCode, b)
	}
}
