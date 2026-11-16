// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// mkDirLinkAPI makes link a link to the directory target: a junction on
// Windows (mklink /J needs no privilege), a symbolic link elsewhere. The
// link itself is removed first at the end, never what it leads to.
func mkDirLinkAPI(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J %s %s: %v %s", link, target, err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
}

// SEC49 L1: a link (a junction) above the record's directory — the course's
// materials folder, its lectures folder — leading outside the data
// directory: deleting a material leaves what is behind the link; replacing
// a draft's media writes nothing there and removes nothing (500, the
// internal error, as for a record id that names no directory of its own).
func TestRecordFilesBehindALinkAbove(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("Operating Systems")

	mid := db.NewID()
	if err := e.st.CreateMaterial(ctx, model.Material{ID: mid, CourseID: c.ID, Filename: "slides.pdf", Path: "slides.pdf",
		CreatedAt: db.Now()}); err != nil {
		t.Fatal(err)
	}
	outMat := t.TempDir()
	matMarker := filepath.Join(outMat, mid, "source.pdf")
	if err := os.MkdirAll(filepath.Dir(matMarker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(matMarker, []byte("r8 marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	mkDirLinkAPI(t, filepath.Dir(model.MaterialDir(e.cfg.DataDir, c.ID, mid)), outMat)
	req := httptest.NewRequest(http.MethodDelete, "/api/materials/x", nil)
	req.SetPathValue("id", mid)
	rec := httptest.NewRecorder()
	e.srv.deleteMaterial(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete material: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(matMarker); err != nil {
		t.Errorf("the material's file behind the link was removed: %v", err)
	}

	l := e.draft(c.ID, "L")
	lecDir := model.LectureDir(e.cfg.DataDir, c.ID, l.ID)
	outLec := t.TempDir()
	media := filepath.Join(outLec, l.ID, "media.mp4")
	if err := os.MkdirAll(filepath.Dir(media), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, []byte("r8 marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(lecDir)); err != nil {
		t.Fatal(err)
	}
	mkDirLinkAPI(t, filepath.Dir(lecDir), outLec)
	if err := e.st.SetLectureMedia(ctx, l.ID, filepath.Join(lecDir, "media.mp4")); err != nil {
		t.Fatal(err)
	}
	res, b := e.tokenUpload("PUT", "/api/lectures/"+l.ID+"/media", nil, "better.mkv", []byte("better media"))
	if res.StatusCode != http.StatusInternalServerError || errCode(t, b) != "server.internal" {
		t.Fatalf("replace media: %d %s", res.StatusCode, b)
	}
	if got, err := os.ReadFile(media); err != nil || string(got) != "r8 marker" {
		t.Errorf("the media behind the link: %q %v", got, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(media)); len(entries) != 1 {
		t.Errorf("something was written behind the link (%d entries)", len(entries))
	}
}

// R77b low-3, R77 info-3: a material directory that is a link (a junction)
// named exactly like the material and leading outside the data directory
// is not the material's own directory (media.OwnDir): deleting the
// material deletes its row and leaves the link and what it leads to. An
// ordinary material's directory is removed as before.
func TestDeleteMaterialLeavesALinkedDirectory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("Operating Systems")
	outside := t.TempDir()
	marker := filepath.Join(outside, "source.pdf")
	if err := os.WriteFile(marker, []byte("r8 marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	del := func(id string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/materials/x", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		e.srv.deleteMaterial(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete %s: %d %s", id, rec.Code, rec.Body)
		}
	}
	add := func() string {
		t.Helper()
		id := db.NewID()
		if err := e.st.CreateMaterial(ctx, model.Material{ID: id, CourseID: c.ID, Filename: "slides.pdf", Path: "slides.pdf",
			CreatedAt: db.Now()}); err != nil {
			t.Fatal(err)
		}
		return id
	}

	linked := add()
	link := model.MaterialDir(e.cfg.DataDir, c.ID, linked)
	mkDirLinkAPI(t, link, outside)
	del(linked)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the file behind the link was removed: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the link was removed: %v", err)
	}

	plain := add()
	dir := model.MaterialDir(e.cfg.DataDir, c.ID, plain)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.pdf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	del(plain)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("an ordinary material's directory was not removed: %v", err)
	}
}
