// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/relocate"
)

// SEC44b medium-1: a library copied to another folder imports (its stored
// paths name the folder it was written in); one with a stored path that
// really points elsewhere is refused with data_import.outside_path {count,
// path} (400), not as "not a LectureFlow data folder".
func TestDataImportMovedAndOutside(t *testing.T) {
	e := newDDEnv(t)
	src := devDataDir(t)
	moved := filepath.Join(t.TempDir(), "copied", "data")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, moved); err != nil {
		t.Fatal(err)
	}
	e.pick(moved)
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": moved})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("moved library: %d %s", res.StatusCode, b)
	}
	e.waitRun()
	if st := e.status(); st.State == ddFailed {
		t.Fatalf("moved library: %+v", st.Error)
	}

	e = newDDEnv(t)
	src = devDataDir(t)
	outside := filepath.Join(t.TempDir(), "Documents", "sec44b-marker.mp4")
	d, err := db.OpenDB(filepath.Join(src, relocate.DBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES ('L2', 'C1', 'W2', ?, '2026-01-03T00:00:00Z')", outside); err != nil {
		t.Fatal(err)
	}
	d.Close()
	e.pick(src)
	res, b = e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodeOutsidePath {
		t.Fatalf("outside path: %d %s", res.StatusCode, b)
	}
	if p := errParamsOf(t, b); p["count"] != float64(1) || p["path"] != outside {
		t.Fatalf("params %v", p)
	}
	if pathExists(filepath.Join(e.cfg.DataDir, relocate.StagingDir)) || e.restarts.Load() != 0 {
		t.Fatal("a refused import was staged")
	}
}

// SEC44b low-1: a library someone changed with a database tool (here a
// view) is refused with data_import.modified {path} (400), which says so.
func TestDataImportModifiedLibrary(t *testing.T) {
	e := newDDEnv(t)
	src := devDataDir(t)
	d, err := db.OpenDB(filepath.Join(src, relocate.DBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("CREATE VIEW sec44b_view AS SELECT id FROM lecture"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	e.pick(src)
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodeModified {
		t.Fatalf("modified library: %d %s", res.StatusCode, b)
	}
	if p := errParamsOf(t, b); p["path"] != src {
		t.Fatalf("params %v", p)
	}
}
