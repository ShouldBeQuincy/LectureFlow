// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// AUD1 M-1: deleting a material removes its directory only where the
// material's own directory belongs. The ids come from the database, which
// an imported library supplies; an id that climbs out of the materials
// directory (to a folder outside the data directory, or to another course
// inside it) must not make DELETE /api/materials/{id} remove that folder.
// Everything happens under t.TempDir().
func TestDeleteMaterialStaysInItsDirectory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("Operating Systems")
	other := e.course("Networks")

	mkdir := func(dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, "keep.txt")
		if err := os.WriteFile(f, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	addMaterial := func(id string) {
		t.Helper()
		if err := e.st.CreateMaterial(ctx, model.Material{ID: id, CourseID: c.ID, Filename: "slides.pdf", Path: "slides.pdf",
			CreatedAt: db.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// The handler is called with the id set directly: the router never
	// passes such ids, an imported database does.
	del := func(id string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/materials/x", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		e.srv.deleteMaterial(rec, req)
		return rec.Code
	}

	// outside the data directory: data/courses/{c}/materials/../../../../victim
	victim := mkdir(filepath.Join(e.dir, "victim"))
	escape := "../../../../victim"
	if got := model.MaterialDir(e.cfg.DataDir, c.ID, escape); filepath.Clean(got) != filepath.Dir(victim) {
		t.Fatalf("test setup: %s does not resolve to the victim folder (%s)", got, filepath.Dir(victim))
	}
	addMaterial(escape)
	if code := del(escape); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a folder outside the data directory was removed: %v", err)
	}

	// inside the data directory, but another course's folder
	otherFile := mkdir(model.CourseDir(e.cfg.DataDir, other.ID))
	sideways := "../../" + other.ID
	addMaterial(sideways)
	if code := del(sideways); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if _, err := os.Stat(otherFile); err != nil {
		t.Errorf("another course's folder was removed: %v", err)
	}

	// an id that differs from a sibling material's directory only in case
	// (one directory on a file system that ignores case; R77 low-2)
	lowID := "own0000000000000000000000"
	addMaterial(lowID)
	lowFile := mkdir(model.MaterialDir(e.cfg.DataDir, c.ID, lowID))
	upID := "OWN0000000000000000000000"
	addMaterial(upID)
	if code := del(upID); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if _, err := os.Stat(lowFile); err != nil {
		t.Errorf("the directory of the material that differs only in case was removed: %v", err)
	}

	// an ordinary material: its directory is removed as before
	ok := db.NewID()
	addMaterial(ok)
	own := mkdir(model.MaterialDir(e.cfg.DataDir, c.ID, ok))
	if code := del(ok); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if _, err := os.Stat(filepath.Dir(own)); !os.IsNotExist(err) {
		t.Errorf("the material's own directory was not removed: %v", err)
	}
	if _, err := e.st.GetMaterial(ctx, ok); err == nil {
		t.Errorf("the material row is still there")
	}
}
