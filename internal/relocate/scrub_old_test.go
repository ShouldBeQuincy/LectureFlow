// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/colab"
)

// Test values only (never a real credential).
const oldDirRefresh, oldDirSecret = "1//test-refresh-token-olddir-7a2b", "test-client-secret-olddir-5d8e"

// rawHolds reports which of the needles occur in the raw bytes of the
// database at path and its sidecar files.
func rawHolds(t *testing.T, path string, needles ...string) []string {
	t.Helper()
	var found []string
	for _, sfx := range append([]string{""}, dbSidecars...) {
		b, err := os.ReadFile(path + sfx)
		if err != nil {
			continue
		}
		for _, n := range needles {
			if strings.Contains(string(b), n) {
				found = append(found, filepath.Base(path+sfx)+": "+n)
			}
		}
	}
	return found
}

// TestScrubOldDataDirAfterMove (v3 Gate round 2, lead ruling): after a
// move has switched over, the old directory's database loses the Colab
// credential — no byte of the refresh token or the client secret stays in
// the database file or its WAL — while its other rows and every other
// file of the old directory stay. The new directory keeps the credential.
func TestScrubOldDataDirAfterMove(t *testing.T) {
	ctx := context.Background()
	e := newMoveEnv(t)
	cred := fmt.Sprintf(`{"client_secret":%q,"refresh_token":%q}`, oldDirSecret, oldDirRefresh)
	for i := 0; i < 3; i++ { // rewritten: older copies in the pages and the WAL as well
		if _, err := e.fx.db.Exec("INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			colab.KVCredential, cred+strings.Repeat(" ", i*40)); err != nil {
			t.Fatal(err)
		}
	}
	if err := MoveDataDir(ctx, e.opts()); err != nil {
		t.Fatal(err)
	}
	// The old service stops without a clean close: its database and WAL
	// are left as they were while it ran (the WAL holds the credential).
	oldDB := filepath.Join(e.src, DBName)
	saved := map[string][]byte{}
	for _, sfx := range []string{"", "-wal"} {
		b, err := os.ReadFile(oldDB + sfx)
		if err != nil {
			t.Fatalf("reading %s: %v", oldDB+sfx, err)
		}
		saved[sfx] = b
	}
	e.fx.db.Close()
	for sfx, b := range saved {
		if err := os.WriteFile(oldDB+sfx, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(oldDB + "-shm")
	if got := rawHolds(t, oldDB, oldDirRefresh, oldDirSecret); len(got) == 0 {
		t.Fatal("the fixture does not hold the credential before the scrub")
	}

	if err := ScrubOldDataDir(ctx, e.src, e.dst); err != nil {
		t.Fatal(err)
	}
	if got := rawHolds(t, oldDB, oldDirRefresh, oldDirSecret); len(got) != 0 {
		t.Errorf("the old directory still holds the credential: %v", got)
	}
	d, err := openDB(oldDB, true)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow("SELECT count(*) FROM kv WHERE key = ?", colab.KVCredential).Scan(&n); err != nil || n != 0 {
		t.Errorf("credential rows %d %v", n, err)
	}
	var media string
	if err := d.QueryRow("SELECT media_path FROM lecture WHERE id = 'L1'").Scan(&media); err != nil || !strings.HasPrefix(media, e.src) {
		t.Errorf("the old database's other rows: %q %v", media, err)
	}
	d.Close()
	for rel, body := range fixtureFiles {
		if b, err := os.ReadFile(filepath.Join(e.src, filepath.FromSlash(rel))); err != nil || string(b) != body {
			t.Errorf("old directory file %s changed: %q %v", rel, b, err)
		}
	}
	// The new directory is the one in use: its credential stays.
	nd, err := openDB(filepath.Join(e.dst, DBName), true)
	if err != nil {
		t.Fatal(err)
	}
	defer nd.Close()
	var v string
	if err := nd.QueryRow("SELECT value FROM kv WHERE key = ?", colab.KVCredential).Scan(&v); err != nil || !strings.Contains(v, oldDirRefresh) {
		t.Errorf("the new database lost the credential: %v", err)
	}

	// Twice is harmless; a missing database is nothing to do.
	if err := ScrubOldDataDir(ctx, e.src, e.dst); err != nil {
		t.Errorf("second scrub: %v", err)
	}
	if err := ScrubOldDataDir(ctx, filepath.Join(t.TempDir(), "gone"), e.dst); err != nil {
		t.Errorf("missing directory: %v", err)
	}
}

// leaveUncleanOldDB makes the old directory's database and WAL what an
// unclean stop leaves (the WAL holds the credential) and returns their
// bytes by suffix ("" and "-wal"); the fixture's handle is closed.
func leaveUncleanOldDB(t *testing.T, e *moveEnv) map[string][]byte {
	t.Helper()
	oldDB := filepath.Join(e.src, DBName)
	saved := map[string][]byte{}
	for _, sfx := range []string{"", "-wal"} {
		b, err := os.ReadFile(oldDB + sfx)
		if err != nil {
			t.Fatalf("reading %s: %v", oldDB+sfx, err)
		}
		saved[sfx] = b
	}
	e.fx.db.Close()
	for sfx, b := range saved {
		if err := os.WriteFile(oldDB+sfx, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(oldDB + "-shm")
	return saved
}

// credentialOldDir puts the Colab credential into the fixture's database
// (rewritten, so that older copies lie in its pages and its WAL).
func credentialOldDir(t *testing.T, e *moveEnv) {
	t.Helper()
	cred := fmt.Sprintf(`{"client_secret":%q,"refresh_token":%q}`, oldDirSecret, oldDirRefresh)
	for i := 0; i < 3; i++ {
		if _, err := e.fx.db.Exec("INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			colab.KVCredential, cred+strings.Repeat(" ", i*40)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestScrubOldDataDirBeforeRollbackCopy (r6): the database a rollback of
// an in-app update kept beside the old one (BeforeRollbackName, with its
// -wal; the desktop shell's _restoreDb) held the same credentials. After
// a move it loses them like the old database, in the file and in its WAL;
// its other rows stay. A copy that is not a regular file is left alone.
func TestScrubOldDataDirBeforeRollbackCopy(t *testing.T) {
	if BeforeRollbackName != "lectureflow.db.lf-before-rollback" {
		t.Fatalf("BeforeRollbackName %q is not the name the desktop shell writes", BeforeRollbackName)
	}
	ctx := context.Background()
	e := newMoveEnv(t)
	credentialOldDir(t, e)
	if err := MoveDataDir(ctx, e.opts()); err != nil {
		t.Fatal(err)
	}
	saved := leaveUncleanOldDB(t, e)
	kept := filepath.Join(e.src, BeforeRollbackName)
	for sfx, b := range saved { // what the rollback renamed: the database with its log
		if err := os.WriteFile(kept+sfx, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := rawHolds(t, kept, oldDirRefresh, oldDirSecret); len(got) == 0 {
		t.Fatal("the fixture's kept copy does not hold the credential before the scrub")
	}

	if err := ScrubOldDataDir(ctx, e.src, e.dst); err != nil {
		t.Fatal(err)
	}
	if got := rawHolds(t, kept, oldDirRefresh, oldDirSecret); len(got) != 0 {
		t.Errorf("the copy a rollback kept still holds the credential: %v", got)
	}
	if got := rawHolds(t, filepath.Join(e.src, DBName), oldDirRefresh, oldDirSecret); len(got) != 0 {
		t.Errorf("the old database still holds the credential: %v", got)
	}
	d, err := openDB(kept, true)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow("SELECT count(*) FROM kv WHERE key = ?", colab.KVCredential).Scan(&n); err != nil || n != 0 {
		t.Errorf("credential rows in the kept copy %d %v", n, err)
	}
	var media string
	if err := d.QueryRow("SELECT media_path FROM lecture WHERE id = 'L1'").Scan(&media); err != nil || media == "" {
		t.Errorf("the kept copy's other rows: %q %v", media, err)
	}
	d.Close()
	if err := ScrubOldDataDir(ctx, e.src, e.dst); err != nil {
		t.Errorf("second scrub: %v", err)
	}

	// Not a regular file (a folder of that name): left alone, no failure.
	e2 := newMoveEnv(t)
	if err := MoveDataDir(ctx, e2.opts()); err != nil {
		t.Fatal(err)
	}
	e2.fx.db.Close()
	dir := filepath.Join(e2.src, BeforeRollbackName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ScrubOldDataDir(ctx, e2.src, e2.dst); err != nil {
		t.Errorf("a folder named like the kept copy: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "note.txt")); err != nil || string(b) != "keep" {
		t.Errorf("the folder was touched: %q %v", b, err)
	}
}

// TestScrubOldDataDirBeforeRollbackCopyUnusable (r6): a kept copy that is
// not a database cannot be scrubbed: the failure is returned (the caller
// tries again at the next start), the file is left as it is, and the old
// database and the databases imports replaced are scrubbed all the same.
func TestScrubOldDataDirBeforeRollbackCopyUnusable(t *testing.T) {
	ctx := context.Background()
	e := newMoveEnv(t)
	credentialOldDir(t, e)
	if err := MoveDataDir(ctx, e.opts()); err != nil {
		t.Fatal(err)
	}
	saved := leaveUncleanOldDB(t, e)
	bak := filepath.Join(e.src, "backups", "lectureflow-before-import-20261001-120000.db")
	if err := os.MkdirAll(filepath.Dir(bak), 0o755); err != nil {
		t.Fatal(err)
	}
	for sfx, b := range saved {
		if err := os.WriteFile(bak+sfx, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kept := filepath.Join(e.src, BeforeRollbackName)
	junk := []byte("not a database: " + oldDirRefresh)
	if err := os.WriteFile(kept, junk, 0o644); err != nil {
		t.Fatal(err)
	}

	err := ScrubOldDataDir(ctx, e.src, e.dst)
	if err == nil || !strings.Contains(err.Error(), BeforeRollbackName) {
		t.Errorf("an unusable kept copy: err = %v, want one naming %s", err, BeforeRollbackName)
	}
	if b, rerr := os.ReadFile(kept); rerr != nil || string(b) != string(junk) {
		t.Errorf("the unusable kept copy was changed: %q %v", b, rerr)
	}
	if got := rawHolds(t, filepath.Join(e.src, DBName), oldDirRefresh, oldDirSecret); len(got) != 0 {
		t.Errorf("the old database still holds the credential: %v", got)
	}
	if got := rawHolds(t, bak, oldDirRefresh, oldDirSecret); len(got) != 0 {
		t.Errorf("the database an import replaced still holds the credential: %v", got)
	}
}

// TestScrubOldDataDirRefusesCurrent: never the directory in use (or one
// holding it), never a relative path.
func TestScrubOldDataDirRefusesCurrent(t *testing.T) {
	ctx := context.Background()
	e := newMoveEnv(t)
	if err := ScrubOldDataDir(ctx, e.src, e.src); err == nil {
		t.Error("scrubbed the current data directory")
	}
	if err := ScrubOldDataDir(ctx, filepath.Dir(e.src), e.src); err == nil {
		t.Error("scrubbed a directory holding the current one")
	}
	if err := ScrubOldDataDir(ctx, "relative/dir", e.src); err == nil {
		t.Error("accepted a relative path")
	}
}
