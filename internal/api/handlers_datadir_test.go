// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/relocate"
)

// SPEC-v3 V1.3: changing the data directory and importing data over HTTP
// (handlers_datadir.go). The copy itself is tested in internal/relocate;
// here the move and the staging are fakes unless said otherwise.

type ddEnv struct {
	*env
	d        *DesktopOptions
	restarts atomic.Int32
	loc      string
}

// newDDEnv is a desktop env whose data directory exists (with one file) and
// whose location.json is in a separate default directory.
func newDDEnv(t *testing.T) *ddEnv {
	t.Helper()
	de := &ddEnv{}
	de.loc = filepath.Join(t.TempDir(), "LectureFlow", relocate.LocationName)
	de.d = &DesktopOptions{Token: testToken, LocationFile: de.loc, Restart: func() { de.restarts.Add(1) }}
	de.env = newDesktopEnv(t, de.d)
	if err := os.MkdirAll(filepath.Join(de.cfg.DataDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(de.cfg.DataDir, "logs", "a.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(de.loc), 0o755); err != nil {
		t.Fatal(err)
	}
	run := de.srv.dataDirRun()
	run.restartDelay = 0
	return de
}

// pick grants the folders as the desktop main process does for the folder
// dialog's pick (every path the data directory endpoints take; r4
// path-grants). One pick per request that may use it up.
func (e *ddEnv) pick(paths ...string) {
	e.srv.grants.addFolders(paths)
}

// asWeb runs fn with the server in web mode (the env helpers send no token).
func (e *ddEnv) asWeb(fn func()) {
	e.srv.desktop = nil
	defer func() { e.srv.desktop = e.d }()
	fn()
}

func (e *ddEnv) req(method, path string, body any) (*http.Response, []byte) {
	e.t.Helper()
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			e.t.Fatal(err)
		}
	}
	return e.doWith(method, path, b, testToken, "")
}

func (e *ddEnv) status() DataDirStatus {
	e.t.Helper()
	res, b := e.req("GET", "/api/app/data-dir", nil)
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("GET data-dir: %d %s", res.StatusCode, b)
	}
	var st DataDirStatus
	if err := json.Unmarshal(b, &st); err != nil {
		e.t.Fatal(err)
	}
	return st
}

// waitRun waits for the background run to end.
func (e *ddEnv) waitRun() {
	e.t.Helper()
	run := e.srv.dataDirRun()
	run.mu.Lock()
	fin := run.finished
	run.mu.Unlock()
	select {
	case <-fin:
	case <-time.After(10 * time.Second):
		e.t.Fatal("run did not end")
	}
	// end() closes finished before the restart is called.
	for i := 0; i < 400 && e.status().State == ddDone && e.restarts.Load() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDataDirWebModeNotFound(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/app/data-dir"}, {"POST", "/api/app/data-dir"}, {"POST", "/api/app/data-dir/check"}, {"GET", "/api/app/import"},
		{"POST", "/api/app/import"}, {"GET", "/api/app/old-data-dir"}, {"DELETE", "/api/app/old-data-dir"},
	} {
		res, b := e.do(c.method, c.path, map[string]string{"path": "C:\\x"})
		if res.StatusCode != http.StatusNotFound || errCode(t, b) != "api.not_found" {
			t.Errorf("%s %s in web mode: %d %s", c.method, c.path, res.StatusCode, b)
		}
	}
}

func TestDataDirEnvOverride(t *testing.T) {
	e := newDDEnv(t)
	e.d.LocationFile = ""
	if st := e.status(); st.CanMove || st.State != ddIdle {
		t.Fatalf("status %+v", st)
	}
	res, b := e.req("POST", "/api/app/data-dir", map[string]string{"path": t.TempDir()})
	if res.StatusCode != http.StatusConflict || errCode(t, b) != "data_dir.env_override" {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
}

func TestDataDirBusy(t *testing.T) {
	e := newDDEnv(t)
	e.srv.dataDirRun().move = func(context.Context, relocate.MoveOptions) error {
		t.Error("move ran")
		return nil
	}
	e.asWeb(func() {
		c := e.course("Busy")
		l := e.lecture(c.ID, "W1")
		// the scheduler is not started: queue the prep job by hand
		if j := e.moveJob(model.ScopeLecture, l.ID, model.StagePrep); j.Status != model.StatusQueued {
			e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued)
		}
	})
	newDir, impDir := filepath.Join(t.TempDir(), "new"), t.TempDir()
	e.pick(newDir, impDir)
	res, b := e.req("POST", "/api/app/data-dir", map[string]string{"path": newDir})
	if res.StatusCode != http.StatusConflict || errCode(t, b) != "data_dir.busy" {
		t.Fatalf("move: %d %s", res.StatusCode, b)
	}
	res, b = e.req("POST", "/api/app/import", map[string]string{"path": impDir})
	if res.StatusCode != http.StatusConflict || errCode(t, b) != "data_dir.busy" {
		t.Fatalf("import: %d %s", res.StatusCode, b)
	}
	// a 409 is a failure: both folder grants are given back (grantScope)
	if !e.srv.grants.hasFolder(newDir) || !e.srv.grants.hasFolder(impDir) {
		t.Fatalf("a busy refusal kept the grant: move %v, import %v", e.srv.grants.hasFolder(newDir), e.srv.grants.hasFolder(impDir))
	}
	if e.restarts.Load() != 0 || e.status().State != ddIdle {
		t.Fatal("state changed")
	}
}

func TestDataDirChecksBeforeCopy(t *testing.T) {
	e := newDDEnv(t)
	busy := filepath.Join(t.TempDir(), "full")
	os.MkdirAll(busy, 0o755)
	os.WriteFile(filepath.Join(busy, "x"), []byte("x"), 0o644)
	for _, c := range []struct {
		path string
		code string
		want int
	}{
		{"relative", "data_dir.path_invalid", http.StatusBadRequest},
		{e.cfg.DataDir, "data_dir.same_dir", http.StatusBadRequest},
		{filepath.Join(e.cfg.DataDir, "inner"), "data_dir.target_inside_source", http.StatusBadRequest},
		{busy, "data_dir.target_not_empty", http.StatusBadRequest},
	} {
		e.pick(c.path) // "relative": path_invalid all the same (no grant can name it)
		res, b := e.req("POST", "/api/app/data-dir", map[string]string{"path": c.path})
		if res.StatusCode != c.want || errCode(t, b) != c.code {
			t.Errorf("%s: %d %s", c.path, res.StatusCode, b)
		}
	}
	if e.status().State != ddIdle || pathExists(e.loc) {
		t.Fatal("state changed")
	}
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// The move runs in the background: copying → verifying → done, then the
// restart; the GPU fallback pauses dispatch during the copy and gives the
// new database the previous pause flag.
func TestDataDirMoveFlow(t *testing.T) {
	e := newDDEnv(t)
	dst := filepath.Join(t.TempDir(), "Big Disk", "LectureFlow")
	release := make(chan struct{})
	reported := make(chan struct{})
	var got relocate.MoveOptions
	var pausedDuring bool
	e.srv.dataDirRun().move = func(ctx context.Context, o relocate.MoveOptions) error {
		got = o
		pausedDuring = e.sc.GPUPaused()
		o.Progress(relocate.Progress{Phase: relocate.PhaseCopying, CopiedBytes: 40, TotalBytes: 100})
		close(reported)
		<-release
		o.Progress(relocate.Progress{Phase: relocate.PhaseVerifying, CopiedBytes: 100, TotalBytes: 100})
		return o.BeforeCommit()
	}
	e.pick(dst)
	res, b := e.req("POST", "/api/app/data-dir", map[string]string{"path": dst})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	<-reported
	st := e.status()
	if st.Kind != "move" || st.State != ddCopying || st.CopiedBytes != 40 || st.TotalBytes != 100 || st.Path != dst || !st.CanMove {
		t.Fatalf("status %+v", st)
	}
	// One run at a time (the folder picked again: the move used its grant up).
	e.pick(dst)
	res, b = e.req("POST", "/api/app/data-dir", map[string]string{"path": dst})
	if res.StatusCode != http.StatusConflict || errCode(t, b) != "data_dir.in_progress" {
		t.Fatalf("second POST: %d %s", res.StatusCode, b)
	}
	close(release)
	e.waitRun()
	st = e.status()
	if st.State != ddDone || st.Error != nil || e.restarts.Load() != 1 {
		t.Fatalf("status %+v, restarts %d", st, e.restarts.Load())
	}
	if !pausedDuring || e.sc.GPUPaused() {
		t.Fatalf("GPU pause during %v, after %v", pausedDuring, e.sc.GPUPaused())
	}
	if got.Src != e.srv.absDataDir() || got.Dst != dst || got.LocationFile != e.loc || got.SetKV[model.KVGPUPaused] != "0" {
		t.Fatalf("options %+v", got)
	}
	// Waiting for the restart: no new run.
	imp := t.TempDir()
	e.pick(imp)
	res, _ = e.req("POST", "/api/app/import", map[string]string{"path": imp})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("POST after done: %d", res.StatusCode)
	}
}

// A failed move reports its code, does not restart, and can be retried;
// the PauseDispatch hook replaces the GPU fallback.
func TestDataDirMoveFailureAndHooks(t *testing.T) {
	e := newDDEnv(t)
	var paused, resumed atomic.Int32
	var fixer bool
	e.srv.SetRelocateHooks(RelocateHooks{
		FixComponents: func(string, string) error { return nil },
		PauseDispatch: func() func() { paused.Add(1); return func() { resumed.Add(1) } },
	})
	e.srv.dataDirRun().move = func(ctx context.Context, o relocate.MoveOptions) error {
		fixer = o.FixComponents != nil
		if e.sc.GPUPaused() || o.SetKV != nil {
			t.Error("GPU fallback used with a PauseDispatch hook")
		}
		return model.Errorf(relocate.CodeVerifyFailed, map[string]any{"what": "rows:lecture"}, errors.New("2 rows, the source has 3"))
	}
	dst := filepath.Join(t.TempDir(), "new")
	e.pick(dst)
	res, b := e.req("POST", "/api/app/data-dir", map[string]string{"path": dst})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	e.waitRun()
	st := e.status()
	if st.State != ddFailed || st.Error == nil || st.Error.Code != relocate.CodeVerifyFailed || st.Error.Detail == "" {
		t.Fatalf("status %+v", st)
	}
	if e.restarts.Load() != 0 || paused.Load() != 1 || resumed.Load() != 1 || !fixer {
		t.Fatalf("restarts %d paused %d resumed %d fixer %v", e.restarts.Load(), paused.Load(), resumed.Load(), fixer)
	}
	e.srv.dataDirRun().move = func(context.Context, relocate.MoveOptions) error { return nil }
	e.pick(dst) // the retry is a new pick: the move that started used the grant up
	res, b = e.req("POST", "/api/app/data-dir", map[string]string{"path": dst})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("retry: %d %s", res.StatusCode, b)
	}
	e.waitRun()
	if e.status().State != ddDone || e.restarts.Load() != 1 {
		t.Fatalf("retry status %+v", e.status())
	}
}

// devDataDir makes a development data directory with one course.
func devDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dev", "data")
	media := filepath.Join(dir, "courses", "C1", "lectures", "L1", "media.mp4")
	if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.OpenDB(filepath.Join(dir, relocate.DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec("INSERT INTO course (id, name, created_at) VALUES ('C1', 'Dev course', '2026-01-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES ('L1', 'C1', 'W1', ?, '2026-01-02T00:00:00Z')", media); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Import: refusals, then a real staging run and the restart.
func TestDataImportFlow(t *testing.T) {
	e := newDDEnv(t)
	empty := t.TempDir()
	e.pick(empty)
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": empty})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != "data_import.not_lectureflow" {
		t.Fatalf("empty dir: %d %s", res.StatusCode, b)
	}
	src := devDataDir(t)
	e.pick(src)
	res, b = e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	e.waitRun()
	res, b = e.req("GET", "/api/app/import", nil)
	var st DataDirStatus
	json.Unmarshal(b, &st)
	if res.StatusCode != http.StatusOK || st.Kind != "import" || st.State != ddDone || st.Path != src || e.restarts.Load() != 1 {
		t.Fatalf("status %d %+v restarts %d", res.StatusCode, st, e.restarts.Load())
	}
	staging := filepath.Join(e.cfg.DataDir, relocate.StagingDir)
	if !pathExists(filepath.Join(staging, relocate.ReadyMarker)) || !pathExists(filepath.Join(staging, "courses", "C1", "lectures", "L1", "media.mp4")) {
		t.Fatal("not staged")
	}
	if pathExists(e.loc) {
		t.Fatal("an import must not write location.json")
	}
}

func TestDataImportNotFresh(t *testing.T) {
	e := newDDEnv(t)
	e.srv.dataDirRun().stage = func(context.Context, relocate.ImportOptions) error {
		t.Error("staging ran")
		return nil
	}
	e.asWeb(func() { e.course("Mine") })
	src := devDataDir(t)
	e.pick(src)
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusConflict || errCode(t, b) != "data_import.not_fresh" {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
}

func TestOldDataDir(t *testing.T) {
	e := newDDEnv(t)
	ctx := context.Background()
	get := func() OldDataDir {
		res, b := e.req("GET", "/api/app/old-data-dir", nil)
		var o OldDataDir
		if res.StatusCode != http.StatusOK || json.Unmarshal(b, &o) != nil {
			t.Fatalf("GET: %d %s", res.StatusCode, b)
		}
		return o
	}
	del := func(path string) (int, string) {
		res, b := e.req("DELETE", "/api/app/old-data-dir", map[string]string{"path": path})
		if res.StatusCode == http.StatusOK {
			return res.StatusCode, ""
		}
		return res.StatusCode, errCode(t, b)
	}
	if o := get(); o.Path != "" || o.Exists {
		t.Fatalf("none: %+v", o)
	}
	if st, code := del("C:\\x"); st != http.StatusNotFound || code != "old_data_dir.none" {
		t.Fatalf("none: %d %s", st, code)
	}
	old := filepath.Join(t.TempDir(), "old data")
	writeTree(t, old, map[string]string{relocate.DBName: "db", "courses/a.mp4": "video"})
	e.st.SetKV(ctx, relocate.KVOldDataDir, old)
	if o := get(); o.Path != old || !o.Exists || o.Bytes != 7 {
		t.Fatalf("recorded: %+v", o)
	}
	// 1. only the recorded directory
	other := filepath.Join(t.TempDir(), "other")
	writeTree(t, other, map[string]string{relocate.DBName: "db"})
	if st, code := del(other); st != http.StatusConflict || code != "old_data_dir.mismatch" || !pathExists(other) {
		t.Fatalf("mismatch: %d %s", st, code)
	}
	// 2. never the current data directory (or one containing it)
	e.st.SetKV(ctx, relocate.KVOldDataDir, filepath.Dir(e.cfg.DataDir))
	writeTree(t, filepath.Dir(e.cfg.DataDir), map[string]string{relocate.DBName: "db"})
	if st, code := del(filepath.Dir(e.cfg.DataDir)); st != http.StatusConflict || code != "old_data_dir.is_current" {
		t.Fatalf("current: %d %s", st, code)
	}
	// 3. only a LectureFlow data directory
	plain := filepath.Join(t.TempDir(), "plain")
	writeTree(t, plain, map[string]string{"photo.jpg": "x"})
	e.st.SetKV(ctx, relocate.KVOldDataDir, plain)
	if st, code := del(plain); st != http.StatusConflict || code != "old_data_dir.not_lectureflow" || !pathExists(filepath.Join(plain, "photo.jpg")) {
		t.Fatalf("not lectureflow: %d %s", st, code)
	}
	// deleted; the record is cleared
	e.st.SetKV(ctx, relocate.KVOldDataDir, old)
	if st, code := del(old); st != http.StatusOK {
		t.Fatalf("delete: %d %s", st, code)
	}
	if pathExists(old) || get().Path != "" {
		t.Fatal("not deleted")
	}
	// The default data directory keeps location.json and the shell profile.
	def := filepath.Dir(e.loc)
	writeTree(t, def, map[string]string{relocate.DBName: "db", relocate.LocationName: "{}", "electron/Local State": "{}", "courses/a.mp4": "v",
		"nmhost/launch.json": "{}", "run/nm.sock": ""})
	e.st.SetKV(ctx, relocate.KVOldDataDir, def)
	if st, code := del(def); st != http.StatusOK {
		t.Fatalf("delete default: %d %s", st, code)
	}
	if !pathExists(e.loc) || !pathExists(filepath.Join(def, "electron", "Local State")) || !pathExists(filepath.Join(def, "nmhost", "launch.json")) ||
		!pathExists(filepath.Join(def, "run", "nm.sock")) || pathExists(filepath.Join(def, relocate.DBName)) || pathExists(filepath.Join(def, "courses")) {
		t.Fatal("default directory not emptied correctly")
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 3.1.0 (.claude/v31-brief.md §2.2): GET /api/app/data-dir reports the free
// space of the data directory's volume (-1 when it cannot be read).
func TestDataDirFreeBytes(t *testing.T) {
	e := newDDEnv(t)
	var asked []string
	e.srv.freeSpace = func(dir string) (int64, error) {
		asked = append(asked, dir)
		return 123 << 30, nil
	}
	if st := e.status(); st.FreeBytes != 123<<30 || len(asked) != 1 || !samePathOS(asked[0], e.srv.absDataDir()) {
		t.Fatalf("free_bytes %d (asked %v)", st.FreeBytes, asked)
	}
	e.srv.freeSpace = func(string) (int64, error) { return 0, errors.New("no statfs") }
	if st := e.status(); st.FreeBytes != -1 {
		t.Fatalf("unreadable: free_bytes %d", st.FreeBytes)
	}
	// A directory not created yet: its nearest existing parent's volume.
	e.srv.freeSpace = diskFree
	if got := e.srv.volumeFree(filepath.Join(t.TempDir(), "not", "yet")); got <= 0 {
		t.Fatalf("volumeFree of a missing directory: %d", got)
	}
	if got := e.srv.volumeFree("relative"); got != -1 {
		t.Fatalf("volumeFree of a relative path: %d", got)
	}
	_, b := e.req("GET", "/api/app/data-dir", nil)
	if !bytes.Contains(b, []byte(`"free_bytes":`)) {
		t.Fatalf("no free_bytes in %s", b)
	}
}

func (e *ddEnv) check(path string) DataDirCheck {
	e.t.Helper()
	res, b := e.req("POST", "/api/app/data-dir/check", map[string]string{"path": path})
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("check %q: %d %s", path, res.StatusCode, b)
	}
	var c DataDirCheck
	if err := json.Unmarshal(b, &c); err != nil {
		e.t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"error":`)) || c.OK != (c.Error == nil) ||
		(c.Error != nil && checkErrorBody(mustJSONBytes(e.t, ErrorBody{Error: *c.Error})) != "") {
		e.t.Fatalf("check %q: inconsistent body %s", path, b)
	}
	return c
}

// POST /api/app/data-dir/check: the checks of the move, with its codes, as
// {ok, error}; nothing is copied, claimed or restarted.
func TestDataDirCheck(t *testing.T) {
	e := newDDEnv(t)
	e.srv.dataDirRun().move = func(context.Context, relocate.MoveOptions) error {
		t.Error("the check moved the data")
		return nil
	}
	dst := filepath.Join(t.TempDir(), "Big Disk", "LectureFlow")
	e.pick(dst) // the check does not use it up: it serves every check below
	c := e.check(dst + string(filepath.Separator))
	if !c.OK || c.Error != nil || !samePathOS(c.Path, dst) || c.FreeBytes <= 0 {
		t.Fatalf("good target: %+v", c)
	}
	if pathExists(dst) || pathExists(e.loc) || e.status().State != ddIdle || e.restarts.Load() != 0 {
		t.Fatal("the check changed something")
	}

	// The refusals of POST /api/app/data-dir, as 200 {ok: false}.
	full := filepath.Join(t.TempDir(), "full")
	writeTree(t, full, map[string]string{"x": "x"})
	for _, tc := range []struct {
		path, code string
		free       bool // free_bytes known
	}{
		{"relative", "data_dir.path_invalid", false},
		{"", "data_dir.path_invalid", false},
		{e.cfg.DataDir, "data_dir.same_dir", true},
		{filepath.Join(e.cfg.DataDir, "inner"), "data_dir.target_inside_source", true},
		{filepath.Dir(e.cfg.DataDir), "data_dir.target_contains_source", true},
		{full, "data_dir.target_not_empty", true},
	} {
		if tc.path != "" {
			e.pick(tc.path)
		}
		c := e.check(tc.path)
		if c.OK || c.Error == nil || c.Error.Code != tc.code || (c.FreeBytes > 0) != tc.free {
			t.Errorf("%q: %+v / %+v", tc.path, c, c.Error)
		}
	}
	if c := e.check("relative"); c.Path != "relative" || c.FreeBytes != -1 {
		t.Errorf("relative path echoed as %+v", c)
	}

	// A move running or waiting for the restart: data_dir.in_progress.
	run := e.srv.dataDirRun()
	run.mu.Lock()
	run.busy = true
	run.mu.Unlock()
	if c := e.check(dst); c.OK || c.Error.Code != "data_dir.in_progress" {
		t.Errorf("in progress: %+v", c)
	}
	run.release()

	// The data directory from LECTUREFLOW_DATA_DIR: data_dir.env_override.
	e.d.LocationFile = ""
	if c := e.check(dst); c.OK || c.Error.Code != "data_dir.env_override" {
		t.Errorf("env override: %+v", c)
	}
	e.d.LocationFile = e.loc

	// Jobs running or queued: data_dir.busy {running, queued}.
	e.asWeb(func() {
		co := e.course("Busy")
		l := e.lecture(co.ID, "W1")
		if j := e.moveJob(model.ScopeLecture, l.ID, model.StagePrep); j.Status != model.StatusQueued {
			e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued)
		}
	})
	if c := e.check(dst); c.OK || c.Error.Code != "data_dir.busy" || c.Error.Params["queued"] != float64(1) {
		t.Errorf("busy: %+v / %+v", c, c.Error)
	}

	res, b := e.doWith("POST", "/api/app/data-dir/check", []byte("{"), testToken, "")
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != "request.bad_json" {
		t.Fatalf("bad JSON: %d %s", res.StatusCode, b)
	}
	if e.status().State != ddIdle || e.restarts.Load() != 0 || pathExists(e.loc) {
		t.Fatal("a check changed the state")
	}
}
