// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"net/http"
	"testing"

	"lectureflow/internal/api"
)

// installdir.go: the install_dir row of the scenarios, with the real
// server's codes, after the data folder's rows; no row elsewhere.
func TestMockInstallDirRows(t *testing.T) {
	srv := testServer(t)
	codes := catalogCodes(t)
	for sc, d := range installDirScenarios {
		c := systemChecks(t, srv.URL, sc)["install_dir"]
		if c.Status != d.status || c.DetailInfo == nil || c.DetailInfo.Code != d.code {
			t.Errorf("%s: %+v %+v", sc, c, c.DetailInfo)
			continue
		}
		wantCatalogCode(t, sc+" detail", c.DetailInfo, codes)
		if d.status == "ok" {
			if c.HintInfo != nil {
				t.Errorf("%s: a hint with the ok row", sc)
			}
		} else if want := map[bool]string{true: d.hint, false: "check.hint_install_dir"}[d.hint != ""]; c.HintInfo == nil || c.HintInfo.Code != want {
			t.Errorf("%s: hint %+v", sc, c.HintInfo)
		} else {
			wantCatalogCode(t, sc+" hint", c.HintInfo, codes)
		}
		var sys api.System
		doJSON(t, "GET", srv.URL+"/api/system?mock="+sc, nil, http.StatusOK, &sys)
		at := -1
		for i, x := range sys.Checks {
			if x.Name == "install_dir" {
				at = i
			}
		}
		for i, x := range sys.Checks {
			if (x.Name == "data_dir" || x.Name == "credentials" || x.Name == "data_dir_location") && i > at {
				t.Errorf("%s: %s after install_dir", sc, x.Name)
			}
		}
	}
	for _, sc := range []string{"1", "diag", "datadir_shared"} {
		if _, ok := systemChecks(t, srv.URL, sc)["install_dir"]; ok {
			t.Errorf("%s: an install_dir row", sc)
		}
	}
}
