// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
)

// datadir-ui (datadir.go): the data_dir row of the scenarios whose data
// folder other accounts can reach, and "收紧权限" (the real server's route,
// codes and answers).

func fastRestrict(t *testing.T) {
	old := restrictDelay
	restrictDelay = 5 * time.Millisecond
	t.Cleanup(func() { restrictDelay = old })
}

type restrictAnswer struct {
	Check         api.SystemCheck `json:"check"`
	RollbackReset bool            `json:"rollback_reset"`
}

func TestMockDataDirRows(t *testing.T) {
	fastRestrict(t)
	srv := testServer(t)
	codes := catalogCodes(t)
	for sc, d := range dataDirScenarios {
		c := systemChecks(t, srv.URL, sc)["data_dir"]
		if c.Status != "warn" || c.DetailInfo == nil || c.DetailInfo.Code != d.code || c.HintInfo == nil || c.HintInfo.Code != "check.hint_data_dir_restrict" {
			t.Errorf("%s: %+v %+v %+v", sc, c, c.DetailInfo, c.HintInfo)
			continue
		}
		wantCatalogCode(t, sc+" detail", c.DetailInfo, codes)
		wantCatalogCode(t, sc+" hint", c.HintInfo, codes)
		if c.DetailInfo.Params["path"] != d.path || c.DetailInfo.Params["who"] == "" {
			t.Errorf("%s: params %v", sc, c.DetailInfo.Params)
		}
	}
	// the other scenarios keep their ok row
	for _, sc := range []string{"1", "diag", "ext_ok"} {
		if c := systemChecks(t, srv.URL, sc)["data_dir"]; c.Status != "ok" || c.HintInfo != nil {
			t.Errorf("%s: %+v", sc, c)
		}
	}
	// the configuration names the scenario's folder
	var sys api.System
	doJSON(t, "GET", srv.URL+"/api/system?mock=datadir_shared", nil, http.StatusOK, &sys)
	if cfg, _ := sys.Config.(map[string]any); cfg == nil || cfg["data_dir"] != `D:\LectureFlow` {
		t.Errorf("config: %v", sys.Config)
	}
}

func TestMockRestrictDataDir(t *testing.T) {
	fastRestrict(t)
	srv := testServer(t)
	codes := catalogCodes(t)
	restrict := func(sc string, want int, out any) {
		t.Helper()
		doJSON(t, "POST", srv.URL+"/api/system/data-dir/restrict?mock="+sc, nil, want, out)
	}
	// restricting turns the row ok, until the server restarts
	for _, sc := range []string{"datadir_shared", "datadir_readable"} {
		var got restrictAnswer
		restrict(sc, http.StatusOK, &got)
		if got.Check.Name != "data_dir" || got.Check.Status != "ok" || got.Check.DetailInfo == nil || got.Check.DetailInfo.Code != "check.dir_writable" ||
			got.Check.DetailInfo.Params["path"] != dataDirScenarios[sc].path {
			t.Errorf("%s: %+v", sc, got.Check)
		}
		wantCatalogCode(t, sc+" answer", got.Check.DetailInfo, codes)
		// the rollback state goes when others could change the folder
		if got.RollbackReset != (sc == "datadir_shared") {
			t.Errorf("%s: rollback_reset %v", sc, got.RollbackReset)
		}
		if c := systemChecks(t, srv.URL, sc)["data_dir"]; c.Status != "ok" || c.HintInfo != nil || c.DetailInfo.Code != "check.dir_writable" {
			t.Errorf("%s after restricting: %+v", sc, c)
		}
	}
	// a drive without permissions: 409 with the real server's code, the row stays
	// (and the rollback state went with the old permissions, as the real
	// server says next to the error)
	var body struct {
		api.ErrorBody
		RollbackReset bool `json:"rollback_reset"`
	}
	restrict("datadir_fat", http.StatusConflict, &body)
	reason, _ := body.Error.Params["error"].(string)
	if body.Error.Code != "data_dir.restrict_failed" || body.Error.Params["path"] != `E:\LectureFlow` || !strings.Contains(reason, "Everyone") || !body.RollbackReset {
		t.Errorf("datadir_fat: %+v %v", body.Error, body.RollbackReset)
	}
	wantCatalogCode(t, "datadir_fat", &body.Error, codes)
	if c := systemChecks(t, srv.URL, "datadir_fat")["data_dir"]; c.Status != "warn" || c.HintInfo == nil {
		t.Errorf("datadir_fat after the failure: %+v", c)
	}
	// elsewhere the folder is private already: 200 with the scenario's row
	var got restrictAnswer
	restrict("1", http.StatusOK, &got)
	if got.Check.Status != "ok" || got.Check.DetailInfo == nil || got.Check.DetailInfo.Params["path"] != "./data" {
		t.Errorf("scenario 1: %+v", got.Check)
	}
	// the route and the codes are the real server's
	src, err := os.ReadFile(filepath.Join("..", "system_datadir.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`h("POST /api/system/data-dir/restrict"`, `"data_dir.restrict_failed"`, `"check.hint_data_dir_restrict"`, `"check.data_dir_shared"`, `"check.data_dir_readable"`, "http.StatusConflict"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("internal/api/system_datadir.go has no %s", want)
		}
	}
}
