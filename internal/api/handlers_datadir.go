// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// Changing the data directory and importing an existing one (SPEC-v3 V1.3,
// V4.2 step 1, V11 "应用"; internal/relocate does the work). Desktop mode
// only: in web mode every endpoint here answers 404 api.not_found.
//
//   POST   /api/app/data-dir {"path"}   copy everything to path, verify, write
//                                       location.json, restart (exit code 3)
//   POST   /api/app/data-dir/check {"path"} the same checks, nothing moved (3.1.0)
//   GET    /api/app/data-dir            DataDirStatus (also GET /api/app/import)
//   POST   /api/app/import {"path"}     stage a development data directory,
//                                       restart; applied before the database opens
//   DELETE /api/app/folder-pick {"path"} the user declined the confirmation of
//                                       the folder they picked: forget the pick
//   GET    /api/app/old-data-dir        the directory the data was moved from
//   DELETE /api/app/old-data-dir {"path"} delete it after the user confirmed

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/relocate"
	"lectureflow/internal/sched"
)

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/app/data-dir", s.getDataDirStatus)
		h("POST /api/app/data-dir", s.postDataDir)
		h("POST /api/app/data-dir/check", s.postDataDirCheck)
		h("GET /api/app/import", s.getDataDirStatus)
		h("POST /api/app/import", s.postImport)
		h("DELETE /api/app/folder-pick", s.deleteFolderPick)
		h("GET /api/app/old-data-dir", s.getOldDataDir)
		h("DELETE /api/app/old-data-dir", s.deleteOldDataDir)
	})
}

// RelocateHooks connect the data directory move to other parts of the
// application; cmd sets them with SetRelocateHooks before serving.
type RelocateHooks struct {
	// FixComponents rewrites the absolute paths inside the copied component
	// environments (internal/components) after the copy; nil = skipped.
	FixComponents func(oldRoot, newRoot string) error
	// PauseDispatch stops the scheduler from starting jobs until resume is
	// called (not persisted). nil = the fallback: the global GPU pause
	// (persisted, so the new database gets the previous value back) plus
	// the job checks before and after the copy.
	PauseDispatch func() (resume func())
}

// DataDirStatus is GET /api/app/data-dir and GET /api/app/import.
type DataDirStatus struct {
	Kind        string           `json:"kind,omitempty"` // move | import ("" before the first run)
	State       string           `json:"state"`          // idle | copying | verifying | done | failed
	Path        string           `json:"path,omitempty"` // the target (move) or the source (import)
	CopiedBytes int64            `json:"copied_bytes"`
	TotalBytes  int64            `json:"total_bytes"`
	Error       *model.ErrorInfo `json:"error,omitempty"`
	DataDir     string           `json:"data_dir"` // the current data directory
	// CanMove is false when the data directory was given by
	// LECTUREFLOW_DATA_DIR (no location.json to write).
	CanMove bool `json:"can_move"`
	// FreeBytes is the free space of the data directory's volume (-1 =
	// unknown; 3.1.0, the setup guide's "数据存放位置" step).
	FreeBytes int64 `json:"free_bytes"`
}

// DataDirCheck is POST /api/app/data-dir/check (3.1.0): whether
// POST /api/app/data-dir would accept path now — the same checks and error
// codes, nothing copied. Path is the cleaned absolute target (the input
// when it is not a usable path); FreeBytes the free space of its volume
// (-1 = unknown).
type DataDirCheck struct {
	Path      string           `json:"path"`
	FreeBytes int64            `json:"free_bytes"`
	OK        bool             `json:"ok"`
	Error     *model.ErrorInfo `json:"error"`
}

// OldDataDir is GET /api/app/old-data-dir.
type OldDataDir struct {
	Path   string `json:"path"` // "" = none recorded
	Exists bool   `json:"exists"`
	Bytes  int64  `json:"bytes"`
}

// States of DataDirStatus.
const (
	ddIdle      = "idle"
	ddCopying   = "copying"
	ddVerifying = "verifying"
	ddDone      = "done"
	ddFailed    = "failed"
)

// dataDirRun is the move or import of one server (at most one at a time).
type dataDirRun struct {
	mu    sync.Mutex
	hooks RelocateHooks
	st    DataDirStatus
	busy  bool // a run is in progress or finished and waiting for the restart

	// Injected in tests.
	move         func(ctx context.Context, o relocate.MoveOptions) error
	stage        func(ctx context.Context, o relocate.ImportOptions) error
	restartDelay time.Duration // lets the window see "done" before the restart
	finished     chan struct{} // closed when the background run ends
}

// dataDirRuns maps *Server → *dataDirRun (the Server struct is shared; the
// state lives here).
var dataDirRuns sync.Map

func (s *Server) dataDirRun() *dataDirRun {
	if v, ok := dataDirRuns.Load(s); ok {
		return v.(*dataDirRun)
	}
	v, _ := dataDirRuns.LoadOrStore(s, &dataDirRun{
		st:           DataDirStatus{State: ddIdle},
		move:         relocate.MoveDataDir,
		stage:        relocate.StageImport,
		restartDelay: time.Second,
	})
	return v.(*dataDirRun)
}

// SetRelocateHooks sets the hooks of the data directory move (cmd, desktop mode).
func (s *Server) SetRelocateHooks(h RelocateHooks) {
	r := s.dataDirRun()
	r.mu.Lock()
	r.hooks = h
	r.mu.Unlock()
}

// relocateStatus is the HTTP status of a relocate error code.
func relocateStatus(code string) int {
	switch code {
	case relocate.CodeNoSpace:
		return http.StatusInsufficientStorage
	case codeDataDirPathNotAllowed, codeDataDirPathNotGranted:
		return http.StatusForbidden
	case relocate.CodeNotFresh, relocate.CodeSourceChanged, "data_dir.busy", "data_dir.in_progress", "data_dir.env_override":
		return http.StatusConflict
	case relocate.CodePathInvalid, relocate.CodePathTooLong, relocate.CodeSameDir, relocate.CodeTargetNotEmpty, relocate.CodeTargetInside,
		relocate.CodeTargetContains, relocate.CodeTargetNotWritable, relocate.CodeNotLectureFlow, relocate.CodeTooNew,
		relocate.CodeImportSameDir, codeImportNetworkPath, relocate.CodeOutsidePath, relocate.CodeModified:
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// relocateInfo is the ErrorInfo of a relocate error (unexpected errors are
// data_dir.copy_failed with the text as detail).
func relocateInfo(err error) model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		info := ce.Info
		if info.Detail == "" && ce.Err != nil {
			info.Detail = ce.Err.Error()
		}
		return info
	}
	return model.ErrorInfo{Code: relocate.CodeCopyFailed, Params: map[string]any{"path": ""}, Detail: err.Error()}
}

func (s *Server) writeRelocateError(w http.ResponseWriter, r *http.Request, err error) {
	info := relocateInfo(err)
	status := relocateStatus(info.Code)
	if status == http.StatusInternalServerError {
		slog.Error("api", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeErrorInfo(w, status, info)
}

// desktopOnly answers 404 in web mode.
func (s *Server) desktopOnly(w http.ResponseWriter, r *http.Request) bool {
	if s.desktop == nil {
		writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
		return false
	}
	return true
}

// busyJobs counts current running and queued jobs and running device
// self-tests (the activity of GET /api/app/activity).
func (s *Server) busyJobs(ctx context.Context) (running, queued int, err error) {
	jobs, err := s.st.ListCurrentJobsByStatus(ctx, model.StatusRunning, model.StatusQueued)
	if err != nil {
		return 0, 0, err
	}
	for _, j := range jobs {
		if j.Status == model.StatusQueued {
			queued++
		} else {
			running++
		}
	}
	if s.dsc != nil {
		for _, d := range s.dsc.DeviceStatuses() {
			if d.SelfTest != nil && d.SelfTest.State == sched.DeviceTestRunning {
				running++
			}
		}
	}
	return running, queued, nil
}

// errIfBusy is data_dir.busy {running, queued} when anything runs or waits.
func (s *Server) errIfBusy(ctx context.Context) error {
	running, queued, err := s.busyJobs(ctx)
	if err != nil {
		return err
	}
	if running > 0 || queued > 0 {
		return model.Errorf("data_dir.busy", map[string]any{"running": running, "queued": queued}, nil)
	}
	return nil
}

func (s *Server) dataDirStatus() DataDirStatus {
	r := s.dataDirRun()
	r.mu.Lock()
	st := r.st
	r.mu.Unlock()
	st.DataDir = s.absDataDir()
	st.CanMove = s.desktop != nil && s.desktop.LocationFile != ""
	st.FreeBytes = s.volumeFree(st.DataDir)
	return st
}

// volumeFree is the free space of the volume of dir or of its nearest
// existing parent (-1 = unknown).
func (s *Server) volumeFree(dir string) int64 {
	if dir == "" || !filepath.IsAbs(dir) || s.freeSpace == nil {
		return -1
	}
	dir = filepath.Clean(dir)
	for {
		if fi, err := s.statDir(dir); err == nil && fi.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return -1
		}
		dir = parent
	}
	if f, err := s.freeSpace(dir); err == nil {
		return f
	}
	return -1
}

// codeDataDirPathNotAllowed: a network folder the folder dialog did not pick.
const codeDataDirPathNotAllowed = "data_dir.path_not_allowed"

// codeDataDirPathNotGranted: any other folder the folder dialog did not pick
// (a local path, a mapped drive; r4 path-grants).
const codeDataDirPathNotGranted = "data_dir.path_not_granted"

// folderRefused is the first check of the data directory endpoints on the
// page's path, before anything touches it. Every folder they take comes
// from the folder dialog (window.lectureflow.pickFolder), whose pick the
// desktop main process grants (pathgrant.go, folder grants), so a path
// without a grant is refused:
//
//   - a network or device path (networkPath: \\server\share,
//     //server/share, \\?\UNC\…, \\.\…; security review SEC24 R1): os.Stat
//     of \\server\… makes Windows authenticate to that server and send the
//     user's NTLM credentials. Refused with data_dir.path_not_allowed. A
//     data directory on a share stays possible (NOTES netpath-datadir), but
//     only one the user chose.
//   - any other absolute path (D:\Foo, a mapped drive Z:\, /Volumes/x):
//     refused with data_dir.path_not_granted (r4; SEC24b residual: without
//     it, page script could probe whether a local folder exists, is empty
//     and how much room it has, and move or import the data directory to
//     or from any local folder).
//   - a path that is not absolute (empty, relative, C:x, \x) is
//     data_dir.path_invalid as it always was (relocate's cleanAbs rule),
//     decided here without any file system call: the dialog never returns
//     one, so no grant can name it.
//
// consume uses the grant up (the move, the import; returned when the
// request fails, grantScope); the check only looks, leaving it to the move
// that follows. A pick is good for folderGrantTTL (pathgrant.go, "Folder
// grants"): after that it is refused like a folder that was never picked.
func (s *Server) folderRefused(ctx context.Context, p string, consume bool) error {
	network := networkPath(p)
	if !network {
		if t := strings.TrimSpace(p); t == "" || !filepath.IsAbs(t) {
			return model.Errorf(relocate.CodePathInvalid, map[string]any{"path": p}, nil)
		}
	}
	if consume && s.useFolderGrant(ctx, p) || !consume && s.grants != nil && s.grants.hasFolder(p) {
		return nil
	}
	if network {
		slog.Warn("data directory: refused a network path the user did not pick in the folder dialog")
		return model.Errorf(codeDataDirPathNotAllowed, map[string]any{"path": p}, nil)
	}
	slog.Warn("data directory: refused a folder the user did not pick in the folder dialog")
	return model.Errorf(codeDataDirPathNotGranted, map[string]any{"path": p}, nil)
}

// DELETE /api/app/folder-pick {"path"} → 204: the user picked path in the
// folder dialog and it will not be used (cancel in the page's move or
// import dialog, or the setup guide's check refused the folder; web/api.js
// forgetFolderPick), so the pick is forgotten now instead of when it
// expires (security review SEC26 low-1): a move or an import of that folder
// needs the dialog again. 204 for a JSON object (or null; a missing path is
// the empty one), whether or not path had a pick; 400 request.bad_json for
// any other body (not JSON, an array, a string, a path that is not a
// string). Neither answer depends on the picks, and nothing is asked of the
// file system: the path is only compared with the picks (pathGrantKey).
// The page's own request (the access token, no grant header): giving a
// pick up widens nothing. 404 in web mode.
func (s *Server) deleteFolderPick(w http.ResponseWriter, r *http.Request) {
	if !s.desktopOnly(w, r) {
		return
	}
	var in pathBody
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.grants != nil {
		s.grants.forgetFolder(in.Path)
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/app/data-dir, GET /api/app/import.
func (s *Server) getDataDirStatus(w http.ResponseWriter, r *http.Request) {
	if !s.desktopOnly(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.dataDirStatus())
}

type pathBody struct {
	Path string `json:"path"`
}

// beginRun claims the run (409 data_dir.in_progress when one is going on or
// waiting for the restart) and checks that nothing runs or waits (409
// data_dir.busy). The caller must call release on a refusal after begin.
func (s *Server) beginRun(ctx context.Context) (*dataDirRun, error) {
	run := s.dataDirRun()
	run.mu.Lock()
	if run.busy {
		run.mu.Unlock()
		return nil, model.Errorf("data_dir.in_progress", nil, nil)
	}
	run.busy = true
	run.mu.Unlock()
	if err := s.errIfBusy(ctx); err != nil {
		run.release()
		return nil, err
	}
	return run, nil
}

func (run *dataDirRun) release() {
	run.mu.Lock()
	run.busy = false
	run.mu.Unlock()
}

// start sets the status of a new run and returns the progress callback.
func (run *dataDirRun) start(kind, path string, total int64) func(relocate.Progress) {
	run.mu.Lock()
	run.st = DataDirStatus{Kind: kind, State: ddCopying, Path: path, TotalBytes: total}
	run.finished = make(chan struct{})
	run.mu.Unlock()
	return func(p relocate.Progress) {
		run.mu.Lock()
		defer run.mu.Unlock()
		if p.Phase == relocate.PhaseVerifying {
			run.st.State = ddVerifying
		} else {
			run.st.State = ddCopying
		}
		run.st.CopiedBytes, run.st.TotalBytes = p.CopiedBytes, p.TotalBytes
	}
}

// end records the outcome; a failure frees the run for another attempt.
func (run *dataDirRun) end(err error) {
	run.mu.Lock()
	if err != nil {
		info := relocateInfo(err)
		run.st.State = ddFailed
		run.st.Error = &info
		run.busy = false
	} else {
		run.st.State = ddDone
		run.st.CopiedBytes = run.st.TotalBytes
	}
	fin := run.finished
	run.mu.Unlock()
	if fin != nil {
		close(fin)
	}
}

// restart asks the desktop shell to start the service again (exit code 3):
// on the next start the shell reads location.json, or cmd applies the
// staged import before opening the database.
func (s *Server) restartAfter(run *dataDirRun) {
	run.mu.Lock()
	delay := run.restartDelay
	run.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if s.desktop != nil && s.desktop.Restart != nil {
		s.desktop.Restart()
		return
	}
	slog.Warn("data directory: restart requested but no restart function; restart the application")
}

// pauseDispatch stops new jobs for the copy. It returns the resume
// function and the kv rows the new database must get (the GPU pause flag
// is persisted: the copy, taken while paused, gets the previous value).
func (s *Server) pauseDispatch(h RelocateHooks) (func(), map[string]string, error) {
	if h.PauseDispatch != nil {
		return h.PauseDispatch(), nil, nil
	}
	if s.sc == nil {
		return func() {}, nil, nil
	}
	prev := s.sc.GPUPaused()
	if err := s.sc.SetGPUPaused(true, false); err != nil {
		return nil, nil, err
	}
	v := "0"
	if prev {
		v = "1"
	}
	resume := func() {
		if err := s.sc.SetGPUPaused(prev, false); err != nil {
			slog.Error("data directory: restore the GPU pause", "err", err)
		}
	}
	return resume, map[string]string{model.KVGPUPaused: v}, nil
}

// POST /api/app/data-dir {"path"}: 409 data_dir.env_override (the data
// directory comes from LECTUREFLOW_DATA_DIR); 403
// data_dir.path_not_allowed / data_dir.path_not_granted {path} (not the
// folder dialog's pick: folderRefused); 409 data_dir.in_progress,
// data_dir.busy {running, queued}; 400/507 for the checks of
// relocate.CheckMove; else 202 DataDirStatus and the copy runs in the
// background, followed by the restart.
func (s *Server) postDataDir(w http.ResponseWriter, r *http.Request) {
	if !s.desktopOnly(w, r) {
		return
	}
	if s.desktop.LocationFile == "" {
		writeError(w, http.StatusConflict, "data_dir.env_override", nil)
		return
	}
	var in pathBody
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	if err := s.folderRefused(ctx, in.Path, true); err != nil {
		s.writeRelocateError(w, r, err)
		return
	}
	run, err := s.beginRun(ctx)
	if err != nil {
		s.writeRelocateError(w, r, err)
		return
	}
	src := s.absDataDir()
	plan, err := s.checkMove(ctx, src, in.Path, s.desktop.LocationFile)
	if err != nil {
		run.release()
		s.writeRelocateError(w, r, err)
		return
	}
	progress := run.start("move", plan.Dst, plan.TotalBytes)
	run.mu.Lock()
	hooks, move := run.hooks, run.move
	run.mu.Unlock()
	go func() {
		err := s.runMove(run, hooks, move, src, plan.Dst, progress)
		if err != nil {
			slog.Error("data directory: move failed", "to", plan.Dst, "err", err)
		}
		run.end(err)
		if err == nil {
			slog.Info("data directory: moved; restarting", "from", src, "to", plan.Dst)
			s.restartAfter(run)
		}
	}()
	writeJSON(w, http.StatusAccepted, s.dataDirStatus())
}

// POST /api/app/data-dir/check {"path"} → 200 DataDirCheck: the refusals of
// POST /api/app/data-dir (data_dir.env_override, data_dir.in_progress,
// data_dir.busy {running, queued}, the checks of relocate.CheckMove) as
// {ok: false, error}; nothing is copied or claimed. 404 in web mode, 400
// request.bad_json.
func (s *Server) postDataDirCheck(w http.ResponseWriter, r *http.Request) {
	if !s.desktopOnly(w, r) {
		return
	}
	var in pathBody
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out := DataDirCheck{Path: in.Path, FreeBytes: -1}
	refuse := func(err error) {
		info := relocateInfo(err)
		if relocateStatus(info.Code) == http.StatusInternalServerError {
			slog.Error("data directory check", "path", out.Path, "err", err)
		}
		out.Error = &info
		writeJSON(w, http.StatusOK, out)
	}
	ctx := r.Context()
	if err := s.folderRefused(ctx, in.Path, false); err != nil {
		refuse(err)
		return
	}
	if p := strings.TrimSpace(in.Path); p != "" && filepath.IsAbs(p) {
		out.Path = filepath.Clean(p)
		out.FreeBytes = s.volumeFree(out.Path)
	}
	if s.desktop.LocationFile == "" {
		refuse(model.Errorf("data_dir.env_override", nil, nil))
		return
	}
	run := s.dataDirRun()
	run.mu.Lock()
	busy := run.busy
	run.mu.Unlock()
	if busy {
		refuse(model.Errorf("data_dir.in_progress", nil, nil))
		return
	}
	if err := s.errIfBusy(ctx); err != nil {
		refuse(err)
		return
	}
	plan, err := s.checkMove(ctx, s.absDataDir(), in.Path, s.desktop.LocationFile)
	if err != nil {
		refuse(err)
		return
	}
	out.Path, out.OK = plan.Dst, true
	if plan.FreeBytes > 0 {
		out.FreeBytes = plan.FreeBytes
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) runMove(run *dataDirRun, hooks RelocateHooks, move func(context.Context, relocate.MoveOptions) error,
	src, dst string, progress func(relocate.Progress)) error {
	ctx := context.Background()
	resume, kv, err := s.pauseDispatch(hooks)
	if err != nil {
		return err
	}
	defer resume()
	// A job may have been launched between the check and the pause.
	if err := s.errIfBusy(ctx); err != nil {
		return err
	}
	return move(ctx, relocate.MoveOptions{
		Src: src, Dst: dst, LocationFile: s.desktop.LocationFile,
		FixComponents: hooks.FixComponents,
		SetKV:         kv,
		// Nothing may have started during the copy (the copy's database
		// would miss its work; relocate also refuses when the source
		// database changed after the snapshot).
		BeforeCommit: func() error { return s.errIfBusy(ctx) },
		Progress:     progress,
	})
}

// POST /api/app/import {"path"}: the same refusals as the move (no
// env_override: an import stays in the current data directory), 400
// data_import.not_lectureflow / too_new / same_dir, 400
// data_import.network_path {count, path} (import_paths.go), 409
// data_import.not_fresh {courses, lectures}; else 202 and the staging copy
// runs in the background, followed by the restart (cmd applies it before
// the database opens: relocate.ApplyStaged).
func (s *Server) postImport(w http.ResponseWriter, r *http.Request) {
	if !s.desktopOnly(w, r) {
		return
	}
	var in pathBody
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	if err := s.folderRefused(ctx, in.Path, true); err != nil {
		s.writeRelocateError(w, r, err)
		return
	}
	run, err := s.beginRun(ctx)
	if err != nil {
		s.writeRelocateError(w, r, err)
		return
	}
	refuse := func(err error) {
		run.release()
		s.writeRelocateError(w, r, err)
	}
	info, err := s.checkImportSource(ctx, in.Path)
	if err != nil {
		refuse(err)
		return
	}
	dataDir := s.absDataDir()
	if insideOS(info.Dir, dataDir) || insideOS(dataDir, info.Dir) {
		refuse(model.Errorf(relocate.CodeImportSameDir, nil, nil))
		return
	}
	if err := checkImportPaths(ctx, filepath.Join(info.Dir, relocate.DBName), info.Dir); err != nil {
		refuse(err)
		return
	}
	if err := s.libraryFresh(ctx); err != nil {
		refuse(err)
		return
	}
	progress := run.start("import", info.Dir, info.Bytes)
	run.mu.Lock()
	stage := run.stage
	run.mu.Unlock()
	go func() {
		err := stage(context.Background(), relocate.ImportOptions{
			Src: info.Dir, DataDir: dataDir, Fresh: s.libraryFresh, Progress: progress,
		})
		if err == nil {
			err = checkStagedPaths(context.Background(), dataDir)
		}
		if err != nil {
			slog.Error("data import: staging failed", "from", info.Dir, "err", err)
		}
		run.end(err)
		if err == nil {
			slog.Info("data import: staged; restarting to apply it", "from", info.Dir)
			s.restartAfter(run)
		}
	}()
	writeJSON(w, http.StatusAccepted, s.dataDirStatus())
}

// libraryFresh: the current library has no course (lectures belong to
// courses); 409 data_import.not_fresh otherwise.
func (s *Server) libraryFresh(ctx context.Context) error {
	cs, err := s.st.ListCourses(ctx)
	if err != nil {
		return err
	}
	if len(cs) > 0 {
		lectures := 0
		for _, c := range cs {
			ls, err := s.st.ListLectures(ctx, c.ID)
			if err != nil {
				return err
			}
			lectures += len(ls)
		}
		return model.Errorf(relocate.CodeNotFresh, map[string]any{"courses": len(cs), "lectures": lectures}, nil)
	}
	return nil
}

// samePathOS compares clean absolute paths (case-insensitively on Windows).
func samePathOS(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// insideOS reports whether p is dir or inside it.
func insideOS(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	if filepath.Separator == '\\' {
		p, dir = strings.ToLower(p), strings.ToLower(dir)
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (s *Server) oldDataDir(ctx context.Context) (string, error) {
	v, _, err := s.kvGet(ctx, relocate.KVOldDataDir)
	return strings.TrimSpace(v), err
}

// GET /api/app/old-data-dir: the directory of the last move (kv
// app.old_data_dir in the new database), whether it still exists and its size.
// A recorded value that is not an absolute path on this computer's drives
// (relocate.LocalAbsPath; an import by an older version could bring a
// share along, and opening one sends it the user's credentials) is none,
// never stat'ed (SEC44 low-4).
func (s *Server) getOldDataDir(w http.ResponseWriter, r *http.Request) {
	if !s.desktopOnly(w, r) {
		return
	}
	p, err := s.oldDataDir(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !relocate.LocalAbsPath(p) {
		p = ""
	}
	out := OldDataDir{Path: p}
	if p != "" {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out.Exists = true
			out.Bytes = dirBytes(r.Context(), p)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /api/app/old-data-dir {"path"} (after the user confirmed): only
// the directory recorded in kv app.old_data_dir (404 old_data_dir.none; 409
// old_data_dir.mismatch for another path), never the current data
// directory or one containing it (409 old_data_dir.is_current), and only a
// LectureFlow data directory (409 old_data_dir.not_lectureflow: no
// lectureflow.db in it). In the default data directory the desktop shell's
// location.json and electron/ profile stay. 200 OldDataDir (now empty).
// A recorded value that is not an absolute path on this computer's drives
// is none (as for GET; SEC44 low-4).
func (s *Server) deleteOldDataDir(w http.ResponseWriter, r *http.Request) {
	if !s.desktopOnly(w, r) {
		return
	}
	var in pathBody
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	rec, err := s.oldDataDir(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if rec == "" || !relocate.LocalAbsPath(rec) {
		writeError(w, http.StatusNotFound, "old_data_dir.none", nil)
		return
	}
	if strings.TrimSpace(in.Path) == "" || !filepath.IsAbs(strings.TrimSpace(in.Path)) || !samePathOS(strings.TrimSpace(in.Path), rec) {
		writeError(w, http.StatusConflict, "old_data_dir.mismatch", map[string]any{"path": rec})
		return
	}
	if cur := s.absDataDir(); insideOS(cur, rec) || insideOS(rec, cur) {
		writeError(w, http.StatusConflict, "old_data_dir.is_current", nil)
		return
	}
	if fi, err := os.Stat(filepath.Join(rec, relocate.DBName)); err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusConflict, "old_data_dir.not_lectureflow", map[string]any{"path": rec})
		return
	}
	if err := removeOldDataDir(rec, s.desktop.LocationFile); err != nil {
		slog.Error("delete old data directory", "dir", rec, "err", err)
		writeErrorInfo(w, http.StatusInternalServerError, model.ErrorInfo{Code: "old_data_dir.delete_failed",
			Params: map[string]any{"path": rec}, Detail: err.Error()})
		return
	}
	if err := s.st.SetKV(ctx, relocate.KVOldDataDir, ""); err != nil {
		s.fail(w, r, err)
		return
	}
	slog.Info("deleted the old data directory", "dir", rec)
	writeJSON(w, http.StatusOK, OldDataDir{})
}

// removeOldDataDir deletes dir; when it is the default data directory
// (where location.json lives) it keeps the installation's entries
// (location.json, the shell's electron/ profile, the native messaging
// nmhost/ and run/) and empties the rest.
func removeOldDataDir(dir, locationFile string) error {
	if locationFile == "" || !samePathOS(dir, filepath.Dir(locationFile)) {
		return os.RemoveAll(dir)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range ents {
		n := e.Name()
		if relocate.IsInstallationEntry(n) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, n)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
