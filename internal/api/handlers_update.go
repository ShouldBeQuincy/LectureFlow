// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// Application update relay (SPEC-v3 V8). The desktop main process runs
// electron-updater and reports what it found here; the web page shows it and
// asks for a download, or for installing when the app quits; the main
// process reads the requests back in its 5-second poll. This keeps the
// preload whitelist unchanged (v3-brief §3). Desktop mode only (404 in web
// mode, like /api/app/shutdown); the state lives in memory.
//
//	POST /api/app/update/state           main process (with PathGrantHeader): {state, version, percent, error}
//	GET  /api/app/update                 UpdateStatus (web page and main process)
//	POST /api/app/update/download        web page: download the available update
//	POST /api/app/update/install-on-quit web page: {"enabled": bool} (default true)
//
// Installing never happens while tasks run: the main process installs only
// in its quit flow, after the service (and with it every task) has stopped.

import (
	"net/http"
	"regexp"
	"sync"
	"time"

	"lectureflow/internal/model"
)

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/app/update", s.getUpdate)
		h("POST /api/app/update/state", s.postUpdateState)
		h("POST /api/app/update/download", s.postUpdateDownload)
		h("POST /api/app/update/install-on-quit", s.postUpdateInstallOnQuit)
	})
}

// Update states reported by the main process.
const (
	UpdateNone        = "none"        // no update (or not checked yet)
	UpdateAvailable   = "available"   // a newer version exists; not downloaded
	UpdateDownloading = "downloading" // percent is the progress
	UpdateReady       = "ready"       // downloaded; installed when the app quits (if asked)
	UpdateError       = "error"       // error says which step failed
)

var updateStates = []string{UpdateNone, UpdateAvailable, UpdateDownloading, UpdateReady, UpdateError}

// Error codes the main process may report in state "error".
var updateErrorCodes = []string{"update.check_failed", "update.download_failed", "update.install_failed"}

var updateVersionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)

// UpdateStatus is GET /api/app/update.
type UpdateStatus struct {
	State             string           `json:"state"`
	Version           string           `json:"version,omitempty"`
	Percent           float64          `json:"percent"` // 0–100, meaningful while downloading
	Error             *model.ErrorInfo `json:"error,omitempty"`
	DownloadRequested bool             `json:"download_requested"` // the page asked; the main process has not started yet
	InstallOnQuit     bool             `json:"install_on_quit"`    // install the downloaded update when the app quits
	UpdatedAt         string           `json:"updated_at,omitempty"`
}

// UpdateStateReport is the body of POST /api/app/update/state.
type UpdateStateReport struct {
	State   string           `json:"state"`
	Version string           `json:"version"`
	Percent *float64         `json:"percent"`
	Error   *model.ErrorInfo `json:"error"`
}

type updateRelay struct {
	mu sync.Mutex
	st UpdateStatus
}

// One relay per Server (tests run several servers in one process).
var updateRelays sync.Map // *Server → *updateRelay

func (s *Server) updateRelay() *updateRelay {
	v, _ := updateRelays.LoadOrStore(s, &updateRelay{st: UpdateStatus{State: UpdateNone}})
	return v.(*updateRelay)
}

func (s *Server) updateDesktopOnly(w http.ResponseWriter, r *http.Request) bool {
	if s.desktop == nil {
		writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
		return false
	}
	return true
}

func (s *Server) getUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.updateDesktopOnly(w, r) {
		return
	}
	u := s.updateRelay()
	u.mu.Lock()
	st := u.st
	u.mu.Unlock()
	writeJSON(w, http.StatusOK, st)
}

// validateUpdateReport returns the name of the first invalid field, or "".
func validateUpdateReport(in UpdateStateReport) string {
	if !oneOf(in.State, updateStates) {
		return "state"
	}
	needVersion := in.State == UpdateAvailable || in.State == UpdateDownloading || in.State == UpdateReady
	if (needVersion || in.Version != "") && !updateVersionRe.MatchString(in.Version) {
		return "version"
	}
	if in.Percent != nil && (*in.Percent < 0 || *in.Percent > 100 || *in.Percent != *in.Percent) {
		return "percent"
	}
	if in.State == UpdateError {
		if in.Error == nil || !oneOf(in.Error.Code, updateErrorCodes) {
			return "error"
		}
	} else if in.Error != nil {
		return "error"
	}
	return ""
}

// postUpdateState takes the main process's report. The page's requests
// carry the access token too, so the report also needs the grant only the
// main process can compute (PathGrantHeader, the one POST
// /api/app/path-grants checks; desktop/src/main/apiclient.js sends it):
// without it a script in the page could show "a new version is ready"
// (AUD1 I-1, SEC44 information-1). 403 auth.token_required otherwise.
func (s *Server) postUpdateState(w http.ResponseWriter, r *http.Request) {
	if !s.updateDesktopOnly(w, r) {
		return
	}
	if !s.desktop.pathGrantOK(r.Header.Get(PathGrantHeader)) {
		writeError(w, http.StatusForbidden, "auth.token_required", nil)
		return
	}
	var in UpdateStateReport
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if f := validateUpdateReport(in); f != "" {
		writeError(w, http.StatusBadRequest, "update.state_invalid", map[string]any{"field": f})
		return
	}
	u := s.updateRelay()
	u.mu.Lock()
	prev := u.st
	next := UpdateStatus{State: in.State, Version: in.Version, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if in.Percent != nil {
		next.Percent = *in.Percent
	}
	if in.State == UpdateReady {
		next.Percent = 100
	}
	if in.Error != nil {
		e := model.ErrorInfo{Code: in.Error.Code} // params and detail from the updater are not shown
		next.Error = &e
	}
	sameVersion := prev.Version != "" && prev.Version == in.Version
	switch in.State {
	case UpdateAvailable:
		// A request stays pending until the main process acts on it.
		next.DownloadRequested = sameVersion && prev.DownloadRequested
		next.InstallOnQuit = sameVersion && prev.InstallOnQuit
	case UpdateDownloading, UpdateReady:
		next.InstallOnQuit = sameVersion && prev.InstallOnQuit
	case UpdateError:
		// A failed download can be asked for again; keep the choice to
		// install on quit for when it succeeds.
		next.InstallOnQuit = (sameVersion || in.Version == "") && prev.InstallOnQuit
		if next.Version == "" {
			next.Version = prev.Version
		}
	}
	u.st = next
	u.mu.Unlock()
	s.hub.Changed("update", "")
	writeJSON(w, http.StatusOK, next)
}

func (s *Server) postUpdateDownload(w http.ResponseWriter, r *http.Request) {
	if !s.updateDesktopOnly(w, r) {
		return
	}
	u := s.updateRelay()
	u.mu.Lock()
	ok := u.st.State == UpdateAvailable || (u.st.State == UpdateError && u.st.Version != "")
	if ok {
		u.st.DownloadRequested = true
	}
	st := u.st
	u.mu.Unlock()
	if !ok {
		writeError(w, http.StatusConflict, "update.not_available", nil)
		return
	}
	s.hub.Changed("update", "")
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) postUpdateInstallOnQuit(w http.ResponseWriter, r *http.Request) {
	if !s.updateDesktopOnly(w, r) {
		return
	}
	in := struct {
		Enabled *bool `json:"enabled"`
	}{}
	if err := decodeOptionalJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	enabled := in.Enabled == nil || *in.Enabled
	u := s.updateRelay()
	u.mu.Lock()
	ok := !enabled || u.st.Version != "" && u.st.State != UpdateNone
	if ok {
		u.st.InstallOnQuit = enabled
	}
	st := u.st
	u.mu.Unlock()
	if !ok {
		writeError(w, http.StatusConflict, "update.not_available", nil)
		return
	}
	s.hub.Changed("update", "")
	writeJSON(w, http.StatusOK, st)
}
