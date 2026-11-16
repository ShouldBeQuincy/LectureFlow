// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// datadir-ui (web-builder, 2026-10-27): the mock side of the data folder's
// permissions (security review sec-update M2). The real API is
// internal/api/system_datadir.go, whose codes the mock uses:
//
//	POST /api/system/data-dir/restrict  200 {"check": the data_dir row, ok};
//	                                    409 data_dir.restrict_failed {path, error}
//	                                    when other accounts can still reach it;
//	                                    "rollback_reset": true with either when
//	                                    other accounts could change the folder
//
// Scenarios (GET /api/system turns the data_dir row into the server's
// warning, with the hint check.hint_data_dir_restrict):
//
//	datadir_shared    D:\LectureFlow, other accounts can change it
//	                  (check.data_dir_shared); restricting succeeds and the
//	                  row turns ok
//	datadir_readable  other accounts can read it (check.data_dir_readable);
//	                  restricting succeeds
//	datadir_fat       a USB stick without permissions (FAT, exFAT): Everyone
//	                  can change it; restricting answers 409 and the row stays
//
// A restricted folder stays so until the server restarts. Elsewhere the
// endpoint answers 200 with the scenario's data_dir row (its folder is
// private already). Like the real handler, no "changed" event follows: the
// page reads the diagnostics again itself.

import (
	"net/http"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// restrictDelay is how long POST /api/system/data-dir/restrict takes (tests
// shorten it).
var restrictDelay = 400 * time.Millisecond

// mockDataDir is a scenario's data folder and who else can reach it.
type mockDataDir struct {
	path  string
	code  string // check.data_dir_shared / check.data_dir_readable
	who   string
	stuck bool // the permissions do not hold (FAT): 409
}

var dataDirScenarios = map[string]mockDataDir{
	"datadir_shared":   {path: `D:\LectureFlow`, code: "check.data_dir_shared", who: `NT AUTHORITY\Authenticated Users, BUILTIN\Users`},
	"datadir_readable": {path: `C:\LectureFlow`, code: "check.data_dir_readable", who: `BUILTIN\Users`},
	"datadir_fat":      {path: `E:\LectureFlow`, code: "check.data_dir_shared", who: "Everyone", stuck: true},
}

// dataDirState: the scenarios whose folder was restricted, per mock.
type dataDirState struct {
	mu   sync.Mutex
	done map[string]bool
}

var dataDirStates sync.Map // *mock → *dataDirState

func (m *mock) dataDirs() *dataDirState {
	v, _ := dataDirStates.LoadOrStore(m, &dataDirState{done: map[string]bool{}})
	return v.(*dataDirState)
}

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		mux.HandleFunc("POST /api/system/data-dir/restrict", m.api(m.restrictDataDir))
	})
}

// dataDirSystem turns the data_dir row into the warning of
// internal/api/system_datadir.go dataDirAccessRow (or, once restricted,
// the ok row with the scenario's folder). Called with s.mu held.
func (m *mock) dataDirSystem(r *http.Request, sys *api.System) {
	sc := scenario(r)
	d, inScenario := dataDirScenarios[sc]
	if !inScenario {
		return
	}
	st := m.dataDirs()
	st.mu.Lock()
	exposed := !st.done[sc]
	st.mu.Unlock()
	for i, c := range sys.Checks {
		if c.Name != "data_dir" {
			continue
		}
		if exposed {
			c = codedCheck("data_dir", "warn", d.code, map[string]any{"path": d.path, "who": d.who})
			c.HintInfo = &model.ErrorInfo{Code: "check.hint_data_dir_restrict"}
		} else {
			c = dataDirOK(d.path)
		}
		sys.Checks[i] = c
	}
	if cfg, ok := sys.Config.(map[string]any); ok { // a clone per request (mockConfigView)
		cfg["data_dir"] = d.path
	}
}

func dataDirOK(path string) api.SystemCheck {
	return codedCheck("data_dir", "ok", "check.dir_writable", map[string]any{"path": path})
}

func (m *mock) restrictDataDir(w http.ResponseWriter, r *http.Request, s *store) {
	select { // setting the permissions and checking them again
	case <-time.After(restrictDelay):
	case <-r.Context().Done():
		return
	}
	sc := scenario(r)
	d, inScenario := dataDirScenarios[sc]
	if !inScenario {
		// the folder of the scenario's own data_dir row (private already)
		s.mu.Lock()
		sys, err := m.system(r, s)
		s.mu.Unlock()
		if err != nil {
			writeErr(w, err)
			return
		}
		path := "./data"
		for _, c := range sys.Checks {
			if c.Name == "data_dir" && c.DetailInfo != nil {
				if p, ok := c.DetailInfo.Params["path"].(string); ok && p != "" {
					path = p
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"check": dataDirOK(path)})
		return
	}
	// other accounts could change it, the shell's profile lies in it (the
	// default folder): the update's rollback state goes, also with a 409
	rollbackReset := d.code == "check.data_dir_shared"
	if d.stuck {
		// internal/api/system_datadir.go restrictIncomplete
		reason := "other accounts can still reach it (" + d.who + "): another account owns the folder, or the drive keeps no permissions (FAT, exFAT)"
		info := model.ErrorInfo{Code: "data_dir.restrict_failed", Params: map[string]any{"path": d.path, "error": reason}, Detail: reason}
		writeJSON(w, http.StatusConflict, map[string]any{"error": info, "rollback_reset": rollbackReset})
		return
	}
	st := m.dataDirs()
	st.mu.Lock()
	st.done[sc] = true
	st.mu.Unlock()
	out := map[string]any{"check": dataDirOK(d.path)}
	if rollbackReset {
		out["rollback_reset"] = true
	}
	writeJSON(w, http.StatusOK, out)
}
