// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/relocate"
)

// r4 path-grants (SEC24b residual): the data directory endpoints (the
// move, its check, the data import) take any folder only as the folder
// dialog's pick, local ones included. Without the folder grant the page's
// script could probe whether a local folder exists, whether it is empty and
// how much room its disk has, and move or import the data directory to or
// from any local folder. An ungranted local path is refused with
// data_dir.path_not_granted before any file system call; a network path
// keeps data_dir.path_not_allowed (datadir_netpath_test.go).

// localFolders are local spellings a page could send (all absolute).
func localFolders(t *testing.T) []string {
	ps := []string{
		filepath.Join(t.TempDir(), "Big Disk", "LectureFlow"), // not there yet
		t.TempDir(), // there and empty
	}
	if runtime.GOOS == "windows" {
		// a mapped network drive looks local; it needs the grant all the same
		ps = append(ps, `Z:\LectureFlow`, `z:/LectureFlow/`)
	} else {
		ps = append(ps, "/Volumes/Share/LectureFlow")
	}
	return ps
}

func TestDataDirEndpointsRefuseUngrantedLocalPathBeforeFS(t *testing.T) {
	quietLog(t)
	e := newDDEnv(t)
	spy := &ddSpy{}
	spy.install(e.srv, nil)
	run := e.srv.dataDirRun()
	run.move = func(context.Context, relocate.MoveOptions) error {
		t.Error("a refused path was moved to")
		return nil
	}
	run.stage = func(context.Context, relocate.ImportOptions) error {
		t.Error("a refused path was imported")
		return nil
	}
	paths := localFolders(t)
	// Grants that do not help: a file grant of the folder itself, a folder
	// grant of its parent (a grant is for one folder, not what is below it)
	// and of a sibling.
	for _, p := range paths {
		e.grant(p)
		e.pick(filepath.Dir(filepath.Clean(p)), filepath.Join(filepath.Dir(filepath.Clean(p)), "Other"))
	}
	for _, p := range paths {
		c := e.check(p)
		if c.OK || c.Error == nil || c.Error.Code != codeDataDirPathNotGranted || c.FreeBytes != -1 || c.Path != p {
			t.Errorf("check %q: %+v / %+v", p, c, c.Error)
		}
		if c.Error != nil && c.Error.Params["path"] != p {
			t.Errorf("check %q: params %v", p, c.Error.Params)
		}
		for _, ep := range []string{"/api/app/data-dir", "/api/app/import"} {
			res, b := e.req("POST", ep, map[string]string{"path": p})
			if res.StatusCode != http.StatusForbidden || errCode(t, b) != codeDataDirPathNotGranted {
				t.Errorf("POST %s %q: %d %s", ep, p, res.StatusCode, b)
			}
			if pp := errParamsOf(t, b); pp["path"] != p {
				t.Errorf("POST %s %q: params %v", ep, p, pp)
			}
		}
		// nothing asked about the page's path: no stat (free space,
		// existence), no CheckMove (empty? writable? it writes a probe
		// file), no CheckImportSource
		key, raw := strings.ToLower(filepath.Clean(p)), strings.ToLower(p)
		for _, c := range spy.take() {
			lc := strings.ToLower(c)
			if !strings.HasPrefix(c, "stat ") || strings.Contains(lc, key) || strings.Contains(lc, raw) {
				t.Errorf("%q: the file system was asked: %s", p, c)
			}
		}
	}
	if pathExists(paths[0]) {
		t.Fatal("a refused move created the target")
	}
	if ents, err := os.ReadDir(paths[1]); err != nil || len(ents) != 0 {
		t.Fatalf("a refused request touched an existing folder: %v %v", ents, err)
	}
	if st := e.status(); st.State != ddIdle || e.restarts.Load() != 0 || pathExists(e.loc) {
		t.Fatalf("a refusal changed something: %+v", st)
	}
	// the decoys are still there: a refusal used nothing up
	if !e.srv.grants.hasFolder(filepath.Dir(filepath.Clean(paths[0]))) {
		t.Fatal("a refused request used up another folder's grant")
	}

	// web mode: no such endpoints, nothing asked
	spy.take() // the status above stats the data directory
	e.asWeb(func() {
		for _, ep := range []string{"/api/app/data-dir/check", "/api/app/data-dir", "/api/app/import"} {
			res, b := e.req("POST", ep, map[string]string{"path": paths[0]})
			if res.StatusCode != http.StatusNotFound || errCode(t, b) != "api.not_found" {
				t.Errorf("web mode %s: %d %s", ep, res.StatusCode, b)
			}
		}
	})
	if calls := spy.take(); len(calls) != 0 {
		t.Errorf("web mode asked the file system: %v", calls)
	}
}

// A path that is not absolute is data_dir.path_invalid as before, without a
// grant (the dialog never returns one) and without a file system call.
func TestDataDirInvalidPathNeedsNoGrant(t *testing.T) {
	quietLog(t)
	e := newDDEnv(t)
	spy := &ddSpy{}
	spy.install(e.srv, nil)
	bad := []string{"", "  ", "relative", filepath.Join("a", "b")}
	if runtime.GOOS == "windows" {
		bad = append(bad, `C:relative`, `\rooted\no\drive`)
	}
	for _, p := range bad {
		c := e.check(p)
		if c.OK || c.Error == nil || c.Error.Code != relocate.CodePathInvalid || c.FreeBytes != -1 {
			t.Errorf("check %q: %+v / %+v", p, c, c.Error)
		}
		for _, ep := range []string{"/api/app/data-dir", "/api/app/import"} {
			res, b := e.req("POST", ep, map[string]string{"path": p})
			if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodePathInvalid {
				t.Errorf("POST %s %q: %d %s", ep, p, res.StatusCode, b)
			}
		}
		if calls := spy.take(); len(calls) != 0 {
			t.Errorf("%q: the file system was asked: %v", p, calls)
		}
	}
}

// A local folder the folder dialog picked: the check only looks, a failed
// move or import returns the grant, the move or import that starts uses it
// up, and the same grant never serves a second request.
func TestDataDirLocalFolderGrant(t *testing.T) {
	quietLog(t)
	e := newDDEnv(t)
	spy := &ddSpy{}
	spy.install(e.srv, nil)
	dst := filepath.Join(t.TempDir(), "Big Disk", "LectureFlow")
	e.pick(dst)

	for i := 0; i < 2; i++ { // the check does not use the grant up
		c := e.check(dst)
		if c.Error == nil || c.Error.Code != relocate.CodeTargetNotWritable {
			t.Fatalf("check %d of a picked folder: %+v", i+1, c)
		}
		if !e.srv.grants.hasFolder(dst) {
			t.Fatalf("check %d used the grant up", i+1)
		}
	}
	// a failed import and a failed move return the grant
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": dst})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodeNotLectureFlow || !e.srv.grants.hasFolder(dst) {
		t.Fatalf("failed import: %d %s (grant kept %v)", res.StatusCode, b, e.srv.grants.hasFolder(dst))
	}
	res, b = e.req("POST", "/api/app/data-dir", map[string]string{"path": dst})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodeTargetNotWritable || !e.srv.grants.hasFolder(dst) {
		t.Fatalf("failed move: %d %s (grant kept %v)", res.StatusCode, b, e.srv.grants.hasFolder(dst))
	}
	calls := strings.Join(spy.take(), "\n")
	for _, want := range []string{"checkMove " + dst, "checkImportSource " + dst} {
		if !strings.Contains(calls, want) {
			t.Errorf("picked folder: %q not asked; calls:\n%s", want, calls)
		}
	}

	// the move that starts uses the grant up; a second one is refused
	// before anything else (not data_dir.in_progress: the grant comes first)
	e.srv.checkMove = func(_ context.Context, src, d, _ string) (relocate.MovePlan, error) {
		spy.rec("checkMove " + d)
		return relocate.MovePlan{Src: src, Dst: d, TotalBytes: 1}, nil
	}
	e.srv.dataDirRun().move = func(context.Context, relocate.MoveOptions) error {
		return errors.New("stand-in move: stopped")
	}
	res, b = e.req("POST", "/api/app/data-dir", map[string]string{"path": dst})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("move: %d %s", res.StatusCode, b)
	}
	e.waitRun()
	if e.srv.grants.hasFolder(dst) {
		t.Fatal("a move that started kept its grant (a failure in the background is no refusal)")
	}
	spy.take()
	for _, ep := range []string{"/api/app/data-dir", "/api/app/import"} {
		res, b = e.req("POST", ep, map[string]string{"path": dst})
		if res.StatusCode != http.StatusForbidden || errCode(t, b) != codeDataDirPathNotGranted {
			t.Fatalf("POST %s replaying the used grant: %d %s", ep, res.StatusCode, b)
		}
	}
	if c := e.check(dst); c.Error == nil || c.Error.Code != codeDataDirPathNotGranted {
		t.Fatalf("check after the grant was used: %+v", c)
	}
	if calls := spy.take(); len(calls) != 0 {
		t.Fatalf("a replay reached the file system: %v", calls)
	}
}

// A real import of a picked folder: staged, restarted; the grant is used up,
// so the same request again is refused for the grant before anything else.
func TestDataImportLocalGrantUsedOnce(t *testing.T) {
	quietLog(t)
	e := newDDEnv(t)
	src := devDataDir(t)
	e.pick(src)
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("import: %d %s", res.StatusCode, b)
	}
	e.waitRun()
	if st := e.status(); st.State != ddDone || e.restarts.Load() != 1 {
		t.Fatalf("status %+v restarts %d", st, e.restarts.Load())
	}
	res, b = e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusForbidden || errCode(t, b) != codeDataDirPathNotGranted {
		t.Fatalf("the same import again: %d %s", res.StatusCode, b)
	}
	// picked again, it gets as far as the run (waiting for the restart)
	e.pick(src)
	res, b = e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusConflict || errCode(t, b) != "data_dir.in_progress" {
		t.Fatalf("picked again: %d %s", res.StatusCode, b)
	}
	if !e.srv.grants.hasFolder(src) {
		t.Fatal("a refused import did not return the grant")
	}
}

// The new code is a 403 like data_dir.path_not_allowed and has its
// catalogue entry (the page shows errors.data_dir.path_not_granted).
func TestDataDirPathNotGrantedCode(t *testing.T) {
	if relocateStatus(codeDataDirPathNotGranted) != http.StatusForbidden || relocateStatus(codeDataDirPathNotAllowed) != http.StatusForbidden {
		t.Fatal("not 403")
	}
	if _, ok := errorCatalog[codeDataDirPathNotGranted]; !ok {
		t.Fatal("no catalogue entry")
	}
	info := relocateInfo(model.Errorf(codeDataDirPathNotGranted, map[string]any{"path": `D:\x`}, nil))
	if info.Code != codeDataDirPathNotGranted || info.Params["path"] != `D:\x` {
		t.Fatalf("%+v", info)
	}
}
