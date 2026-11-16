// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The settings page's "App" and "About" sections in the mock (SPEC-v3 V11):
// application settings, the data directory's size, moving it (a copy whose
// progress advances with time, then a simulated restart), the old data
// directory, opening the logs, a small diagnostics zip, the licence and the
// third-party notices (the real embedded texts).
//
// GET /api/app comes from v3_onboarding.go (web mode). The data directory
// endpoints answer as the desktop server does; the page only calls them in
// desktop mode. Moving to a path containing "fail" fails at 40 % with
// data_dir.copy_failed; one containing "busy" is refused with data_dir.busy;
// one containing "notempty" with data_dir.target_not_empty.
//
// State is per mock instance and shared by every scenario; restart the
// server to get it back.

import (
	"archive/zip"
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"

	lectureflow "lectureflow"
	"lectureflow/internal/api"
	"lectureflow/internal/model"
)

// The simulated copy: copyDuration for the bytes, then verifying, then a
// restart during which /api/health does not answer.
var (
	setCopyDuration   = 8 * time.Second
	setVerifyDuration = 1500 * time.Millisecond
	setRestartGap     = 2 * time.Second
	// setNow is the clock of the simulated move (a test steps it instead of
	// racing the wall clock: a busy machine could otherwise poll past the
	// short verifying / restart windows and never see them).
	setNow = time.Now
)

const (
	setDataDir  = `C:\Users\Student\AppData\Roaming\LectureFlow`
	setOldDir   = `D:\LectureFlow-old`
	setDataSize = int64(3_650_000_000)
)

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		a := m.api
		handle := func(pattern string, h http.HandlerFunc) {
			// another v3 mock file may already serve it (no duplicate panics)
			method, path, _ := strings.Cut(pattern, " ")
			if req, err := http.NewRequest(method, path, nil); err == nil {
				if _, p := mux.Handler(req); p == pattern {
					return
				}
			}
			mux.HandleFunc(pattern, h)
		}
		handle("GET /api/health", a(m.setHealth))
		handle("GET /api/app/settings", a(m.setGetAppSettings))
		handle("PATCH /api/app/settings", a(m.setPatchAppSettings))
		handle("GET /api/app/storage", a(m.setStorage))
		handle("GET /api/app/data-dir", a(m.setDataDirStatus))
		handle("POST /api/app/data-dir", a(m.setMove))
		handle("POST /api/app/data-dir/check", a(m.setCheckDir)) // 3.1.0 (v31_onboarding.go)
		handle("GET /api/app/old-data-dir", a(m.setGetOld))
		handle("DELETE /api/app/old-data-dir", a(m.setDeleteOld))
		handle("DELETE /api/app/folder-pick", a(m.setForgetPick)) // r6 path-grants
		handle("POST /api/app/open-logs", a(m.setOpenLogs))
		handle("POST /api/app/diagnostics", a(m.setDiagnostics))
		handle("GET /api/app/license", a(m.setLicense))
		handle("GET /api/app/notices", a(m.setNotices))
	})
}

type setState struct {
	mu       sync.Mutex
	settings api.AppSettings
	dataDir  string
	oldDir   string // "" = none
	oldBytes int64
	move     *setMoveRun
}

type setMoveRun struct {
	path    string
	started time.Time
	fail    bool
	total   int64         // bytes to copy
	copyDur time.Duration // how long the copy takes
}

var setStates sync.Map // *mock → *setState

func (m *mock) appSet() *setState {
	if v, ok := setStates.Load(m); ok {
		return v.(*setState)
	}
	v, _ := setStates.LoadOrStore(m, &setState{
		settings: api.AppSettings{CloseBehavior: "ask", PreventSleep: true, UpdateChannel: "stable"},
		dataDir:  setDataDir,
		oldDir:   setOldDir,
		oldBytes: 2_140_000_000,
	})
	return v.(*setState)
}

func codedError(w http.ResponseWriter, status int, code string, params map[string]any) {
	writeJSON(w, status, api.ErrorBody{Error: model.ErrorInfo{Code: code, Params: params}})
}

// ---------- settings ----------

func (m *mock) setGetAppSettings(w http.ResponseWriter, r *http.Request, _ *store) {
	st := m.appSet()
	st.mu.Lock()
	defer st.mu.Unlock()
	writeJSON(w, http.StatusOK, st.settings)
}

func (m *mock) setPatchAppSettings(w http.ResponseWriter, r *http.Request, _ *store) {
	var in api.AppSettingsPatch
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.CloseBehavior == nil && in.PreventSleep == nil && in.UpdateChannel == nil {
		codedError(w, http.StatusBadRequest, "request.nothing_to_change", map[string]any{"fields": "close_behavior, prevent_sleep, update_channel"})
		return
	}
	if v := in.CloseBehavior; v != nil && *v != "ask" && *v != "background" && *v != "quit" {
		codedError(w, http.StatusBadRequest, "app_settings.close_behavior_invalid", map[string]any{"value": *v})
		return
	}
	if v := in.UpdateChannel; v != nil && *v != "stable" && *v != "beta" {
		codedError(w, http.StatusBadRequest, "app_settings.update_channel_invalid", map[string]any{"value": *v})
		return
	}
	time.Sleep(250 * time.Millisecond) // the "saving" state is visible
	st := m.appSet()
	st.mu.Lock()
	if in.CloseBehavior != nil {
		st.settings.CloseBehavior = *in.CloseBehavior
	}
	if in.PreventSleep != nil {
		st.settings.PreventSleep = *in.PreventSleep
	}
	if in.UpdateChannel != nil {
		st.settings.UpdateChannel = *in.UpdateChannel
	}
	out := st.settings
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// ---------- storage, data directory ----------

func (m *mock) setStorage(w http.ResponseWriter, r *http.Request, _ *store) {
	st := m.appSet()
	st.mu.Lock()
	defer st.mu.Unlock()
	writeJSON(w, http.StatusOK, api.AppStorage{DataDir: st.dataDir, UsedBytes: setDataSize, FreeBytes: setFreeBytes(st.dataDir, obMode(r))})
}

// status advances the simulated move; after the restart gap the "new
// server" has the new directory and remembers the old one.
func (st *setState) status(now time.Time) (api.DataDirStatus, bool) {
	out := api.DataDirStatus{State: "idle", DataDir: st.dataDir, CanMove: true}
	mv := st.move
	if mv == nil {
		return out, true
	}
	out.Kind, out.Path, out.TotalBytes = "move", mv.path, mv.total
	el := now.Sub(mv.started)
	frac := float64(el) / float64(mv.copyDur)
	switch {
	case mv.fail && frac >= 0.4:
		out.State, out.CopiedBytes = "failed", int64(0.4*float64(mv.total))
		out.Error = &model.ErrorInfo{Code: "data_dir.copy_failed", Params: map[string]any{"path": mv.path + `\lectures\01JD3631\media.mp4`}}
	case frac < 1:
		out.State, out.CopiedBytes = "copying", int64(frac*float64(mv.total))
	case el < mv.copyDur+setVerifyDuration:
		out.State, out.CopiedBytes = "verifying", mv.total
	case el < mv.copyDur+setVerifyDuration+setRestartGap:
		out.State, out.CopiedBytes = "done", mv.total
		return out, false // restarting
	default: // the restarted server
		st.oldDir, st.oldBytes = st.dataDir, mv.total
		st.dataDir = mv.path
		st.move = nil
		return api.DataDirStatus{State: "idle", DataDir: st.dataDir, CanMove: true}, true
	}
	return out, true
}

func (m *mock) setDataDirStatus(w http.ResponseWriter, r *http.Request, _ *store) {
	st := m.appSet()
	st.mu.Lock()
	out, up := st.status(setNow())
	st.mu.Unlock()
	if !up {
		codedError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "restart"})
		return
	}
	// 3.1.0: free_bytes of the data folder's disk (v31_onboarding.go)
	writeJSON(w, http.StatusOK, setDataDirView{DataDirStatus: out, FreeBytes: setFreeBytes(out.DataDir, obMode(r))})
}

func (m *mock) setHealth(w http.ResponseWriter, r *http.Request, _ *store) {
	st := m.appSet()
	st.mu.Lock()
	_, up := st.status(setNow())
	st.mu.Unlock()
	if !up {
		codedError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "restart"})
		return
	}
	writeJSON(w, http.StatusOK, api.Health{OK: true, Version: "3.0.0-mock", Mode: "web"})
}

func (m *mock) setMove(w http.ResponseWriter, r *http.Request, _ *store) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	p := strings.TrimSpace(in.Path)
	st := m.appSet()
	st.mu.Lock()
	defer st.mu.Unlock()
	if status, info := st.checkTarget(p); info != nil {
		writeJSON(w, status, api.ErrorBody{Error: *info})
		return
	}
	mv := &setMoveRun{path: p, started: setNow(), fail: strings.Contains(strings.ToLower(p), "fail"), total: setDataSize, copyDur: setCopyDuration}
	if strings.HasPrefix(scenario(r), "ob_") {
		// the onboarding's move: a fresh install has hardly any data
		mv.total, mv.copyDur = setFreshSize, setFreshCopy
	}
	st.move = mv
	out, _ := st.status(setNow())
	writeJSON(w, http.StatusAccepted, out)
}

// checkTarget applies the refusals of POST /api/app/data-dir to path p (also
// POST /api/app/data-dir/check): the HTTP status and the error, or nil.
// Paths containing "busy" / "notempty" are refused like a running job / a
// folder with files. Must hold st.mu.
func (st *setState) checkTarget(p string) (int, *model.ErrorInfo) {
	lp := strings.ToLower(p)
	switch {
	case p == "" || !(strings.Contains(p, `:\`) || strings.HasPrefix(p, "/")):
		return http.StatusBadRequest, &model.ErrorInfo{Code: "data_dir.path_invalid", Params: map[string]any{"path": p}}
	case strings.Contains(lp, "busy"):
		return http.StatusConflict, &model.ErrorInfo{Code: "data_dir.busy", Params: map[string]any{"running": 1, "queued": 2}}
	case strings.Contains(lp, "notempty"):
		return http.StatusConflict, &model.ErrorInfo{Code: "data_dir.target_not_empty", Params: map[string]any{"path": p}}
	}
	if cur, _ := st.status(setNow()); cur.State == "copying" || cur.State == "verifying" {
		return http.StatusConflict, &model.ErrorInfo{Code: "data_dir.in_progress"}
	}
	if strings.EqualFold(p, st.dataDir) {
		return http.StatusConflict, &model.ErrorInfo{Code: "data_dir.same_dir"}
	}
	return 0, nil
}

func (m *mock) setGetOld(w http.ResponseWriter, r *http.Request, _ *store) {
	st := m.appSet()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.oldDir == "" {
		writeJSON(w, http.StatusOK, api.OldDataDir{})
		return
	}
	writeJSON(w, http.StatusOK, api.OldDataDir{Path: st.oldDir, Exists: true, Bytes: st.oldBytes})
}

func (m *mock) setDeleteOld(w http.ResponseWriter, r *http.Request, _ *store) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	st := m.appSet()
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case st.oldDir == "":
		codedError(w, http.StatusNotFound, "old_data_dir.none", nil)
		return
	case !strings.EqualFold(in.Path, st.oldDir):
		codedError(w, http.StatusConflict, "old_data_dir.mismatch", map[string]any{"path": st.oldDir})
		return
	}
	time.Sleep(600 * time.Millisecond)
	st.oldDir, st.oldBytes = "", 0
	writeJSON(w, http.StatusOK, api.OldDataDir{})
}

// setForgetPick is DELETE /api/app/folder-pick {"path"}: 204 with no body,
// as the desktop server answers whether or not the path had a pick
// (internal/api/handlers_datadir.go deleteFolderPick). The page sends it
// when the user cancels the confirmation of a folder they picked, or the
// check refuses it. The mock keeps no picks (its data directory endpoints
// take any path), so there is nothing to forget.
func (m *mock) setForgetPick(w http.ResponseWriter, r *http.Request, _ *store) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- logs, diagnostics, legal texts ----------

func (m *mock) setOpenLogs(w http.ResponseWriter, r *http.Request, _ *store) {
	st := m.appSet()
	st.mu.Lock()
	dir := st.dataDir + `\logs`
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
}

func (m *mock) setDiagnostics(w http.ResponseWriter, r *http.Request, _ *store) {
	time.Sleep(700 * time.Millisecond) // "collecting" is visible
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"README.txt":           "LectureFlow diagnostics (mock)\n",
		"info.json":            `{"version": "3.0.0-mock", "mode": "web", "platform": "windows"}` + "\n",
		"logs/lectureflow.log": "time=2026-10-24T10:00:00Z level=INFO msg=\"lectureflow started\" (mock)\n",
	} {
		f, err := zw.Create(name)
		if err == nil {
			_, err = f.Write([]byte(body))
		}
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="lectureflow-diagnostics-`+time.Now().Format("2006-01-02")+`.zip"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (m *mock) setLicense(w http.ResponseWriter, r *http.Request, _ *store) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(lectureflow.License))
}

func (m *mock) setNotices(w http.ResponseWriter, r *http.Request, _ *store) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(lectureflow.ThirdPartyNotices))
}
