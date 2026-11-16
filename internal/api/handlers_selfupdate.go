// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// In-app update (SPEC-ext "应用内更新（不依赖自有服务器）", internal/selfupdate;
// the update.json format and the contract with Electron are in
// internal/selfupdate/README.md). Next to the electron-updater relay of
// handlers_update.go until the desktop shell switches over.
//
//	GET    /api/selfupdate           selfupdate.Status
//	POST   /api/selfupdate/check     check now → 200 Status (a failed check is Status.error:
//	                                 no_manifest when the latest release has no update.json or
//	                                 signature yet, unreachable, ...; detail lists every source;
//	                                 an update found before stays available / pending)
//	POST   /api/selfupdate/download  download the available version → 202 Status (downloading)
//	DELETE /api/selfupdate/download  cancel the download (the partial file stays) → 200 Status
//	POST   /api/selfupdate/import    offline update ("从文件更新") → 200 Status (pending):
//	                                 desktop mode JSON {"paths": [...]} (or {"path"}), the files
//	                                 the user picked or dropped; or multipart, one to three
//	                                 "file" parts: update.json, update.json.minisig and the
//	                                 package, or one zip holding the three
//	DELETE /api/selfupdate/pending   drop the pending update and its files → 200 Status
//	GET    /api/selfupdate/settings  selfupdate.Settings {auto_check, sources}
//	PATCH  /api/selfupdate/settings  {auto_check?, sources?} → 200 Settings
//
// Every change of the status is announced as SSE changed {kind: "selfupdate"}
// (download progress at most once a second). Errors are selfupdate.* codes
// (testdata/api/error_codes_backend.json); 503 server.unavailable
// {service: "selfupdate"} when the server has no updater.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"lectureflow/internal/selfupdate"
)

// SelfUpdater is the in-app updater (*selfupdate.Updater).
type SelfUpdater interface {
	Status(ctx context.Context) selfupdate.Status
	Check(ctx context.Context) (selfupdate.Status, error)
	Download(ctx context.Context) (selfupdate.Status, error)
	CancelDownload() bool
	ImportPaths(ctx context.Context, paths []string) (selfupdate.Status, error)
	ImportWith(ctx context.Context, fill func(dir string) error) (selfupdate.Status, error)
	DiscardPending(ctx context.Context) (selfupdate.Status, error)
	Settings(ctx context.Context) selfupdate.Settings
	UpdateSettings(ctx context.Context, p selfupdate.SettingsPatch) (selfupdate.Settings, error)
	OnChange(fn func())
}

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/selfupdate", s.getSelfUpdate)
		h("POST /api/selfupdate/check", s.postSelfUpdateCheck)
		h("POST /api/selfupdate/download", s.postSelfUpdateDownload)
		h("DELETE /api/selfupdate/download", s.deleteSelfUpdateDownload)
		h("POST /api/selfupdate/import", s.postSelfUpdateImport)
		h("DELETE /api/selfupdate/pending", s.deleteSelfUpdatePending)
		h("GET /api/selfupdate/settings", s.getSelfUpdateSettings)
		h("PATCH /api/selfupdate/settings", s.patchSelfUpdateSettings)
	})
}

// wireSelfUpdate announces the updater's changes over SSE (newServer).
func (s *Server) wireSelfUpdate() {
	if u, ok := s.su.(*selfupdate.Updater); ok && u == nil {
		s.su = nil
	}
	if s.su != nil {
		s.su.OnChange(func() { s.hub.Changed("selfupdate", "") })
	}
}

func (s *Server) requireSelfUpdate(w http.ResponseWriter) bool {
	if s.su == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "selfupdate"})
		return false
	}
	return true
}

// selfUpdateHTTP is the HTTP status of each selfupdate code.
var selfUpdateHTTP = map[string]int{
	selfupdate.CodeNotConfigured:       http.StatusConflict,
	selfupdate.CodeBusy:                http.StatusConflict,
	selfupdate.CodeNotAvailable:        http.StatusConflict,
	selfupdate.CodeTooOld:              http.StatusConflict,
	selfupdate.CodeNoPackage:           http.StatusConflict,
	selfupdate.CodeNotNewer:            http.StatusConflict,
	selfupdate.CodeCancelled:           http.StatusConflict,
	selfupdate.CodeUntrustedDir:        http.StatusConflict,
	selfupdate.CodeUntrustedInstallDir: http.StatusConflict,
	selfupdate.CodeOtherCopyPending:    http.StatusConflict,
	selfupdate.CodeSignatureInvalid:    http.StatusBadRequest,
	selfupdate.CodeVersionMismatch:     http.StatusBadRequest,
	selfupdate.CodeManifestInvalid:     http.StatusBadRequest,
	selfupdate.CodeChecksum:            http.StatusBadRequest,
	selfupdate.CodeImportIncomplete:    http.StatusBadRequest,
	selfupdate.CodeImportInvalid:       http.StatusBadRequest,
	selfupdate.CodeSourcesInvalid:      http.StatusBadRequest,
	selfupdate.CodeUnreachable:         http.StatusBadGateway,
	selfupdate.CodeNoManifest:          http.StatusBadGateway,
	selfupdate.CodeDownloadFailed:      http.StatusBadGateway,
	selfupdate.CodeIO:                  http.StatusInternalServerError,
}

// failSelfUpdate writes a selfupdate error; other errors (the upload and
// path checks) go through fail.
func (s *Server) failSelfUpdate(w http.ResponseWriter, r *http.Request, err error) {
	status, ok := selfUpdateHTTP[selfupdate.CodeOf(err)]
	if !ok {
		s.fail(w, r, err)
		return
	}
	writeErrorInfo(w, status, selfupdate.InfoOf(err))
}

func (s *Server) getSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfUpdate(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.su.Status(r.Context()))
}

func (s *Server) postSelfUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) || !s.requireSelfUpdate(w) {
		return
	}
	st, err := s.su.Check(r.Context())
	if err != nil {
		s.failSelfUpdate(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) postSelfUpdateDownload(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) || !s.requireSelfUpdate(w) {
		return
	}
	st, err := s.su.Download(r.Context())
	if err != nil {
		s.failSelfUpdate(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) deleteSelfUpdateDownload(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) || !s.requireSelfUpdate(w) {
		return
	}
	s.su.CancelDownload()
	writeJSON(w, http.StatusOK, s.su.Status(r.Context()))
}

func (s *Server) deleteSelfUpdatePending(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) || !s.requireSelfUpdate(w) {
		return
	}
	st, err := s.su.DiscardPending(r.Context())
	if err != nil {
		s.failSelfUpdate(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// POST /api/selfupdate/import: desktop mode JSON {"paths": [...]} or
// {"path": "..."}; otherwise multipart "file" parts.
func (s *Server) postSelfUpdateImport(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) || !s.requireSelfUpdate(w) {
		return
	}
	ctx := r.Context()
	if isJSONBody(r) {
		if s.desktop == nil {
			writeError(w, http.StatusBadRequest, "upload.not_multipart", nil)
			return
		}
		var in struct {
			Path  *string  `json:"path"`
			Paths []string `json:"paths"`
		}
		if err := decodeJSON(r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		paths := in.Paths
		if in.Path != nil {
			paths = append([]string{*in.Path}, paths...)
		}
		if len(paths) == 0 {
			writeError(w, http.StatusBadRequest, "request.invalid", nil)
			return
		}
		if len(paths) > selfupdate.MaxImportFiles {
			writeErrorInfo(w, http.StatusBadRequest, selfupdate.InfoOf(selfupdate.ImportTooMany(paths[selfupdate.MaxImportFiles])))
			return
		}
		resolved := make([]string, 0, len(paths))
		for _, p := range paths {
			src, _, err := s.checkSourcePath(ctx, p)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			resolved = append(resolved, src)
		}
		st, err := s.su.ImportPaths(ctx, resolved)
		if err != nil {
			s.failSelfUpdate(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
		return
	}
	if err := s.checkSpace(r); err != nil {
		s.fail(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, selfupdate.MaxImportFiles*(s.maxUpload+maxFieldBytes))
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "upload.not_multipart", nil)
		return
	}
	st, err := s.su.ImportWith(ctx, func(dir string) error {
		n := 0
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return uploadReadErr(err)
			}
			if p.FormName() != "file" {
				_, err := io.Copy(io.Discard, io.LimitReader(p, maxFieldBytes))
				p.Close()
				if err != nil {
					return uploadReadErr(err)
				}
				continue
			}
			if n == selfupdate.MaxImportFiles {
				p.Close()
				return selfupdate.ImportTooMany(p.FileName())
			}
			if p.FileName() == "" {
				p.Close()
				return apiErr(http.StatusBadRequest, "upload.no_filename", nil)
			}
			err = receivePart(filepath.Join(dir, selfupdate.StagingName(n, p.FileName())), p, s.maxUpload)
			p.Close()
			if err != nil {
				return err
			}
			n++
		}
		if n == 0 {
			return apiErr(http.StatusBadRequest, "upload.no_file", nil)
		}
		return nil
	})
	if err != nil {
		s.failSelfUpdate(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// receivePart writes one uploaded file (at most max bytes).
func receivePart(dst string, src io.Reader, max int64) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(src, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = apiErr(http.StatusRequestEntityTooLarge, "upload.too_large", map[string]any{"max_bytes": max})
	}
	if err != nil {
		os.Remove(dst)
		var he *httpError
		if errors.As(err, &he) {
			return err
		}
		return uploadReadErr(err)
	}
	return nil
}

func (s *Server) getSelfUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfUpdate(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.su.Settings(r.Context()))
}

func (s *Server) patchSelfUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) || !s.requireSelfUpdate(w) {
		return
	}
	var in selfupdate.SettingsPatch
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.AutoCheck == nil && in.Sources == nil {
		writeError(w, http.StatusBadRequest, "request.nothing_to_change", map[string]any{"fields": "auto_check, sources"})
		return
	}
	out, err := s.su.UpdateSettings(r.Context(), in)
	if err != nil {
		s.failSelfUpdate(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
