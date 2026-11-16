// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/colab"
	"lectureflow/internal/db"
	"lectureflow/internal/relocate"
)

// writeOldCredential gives the database of dir a Colab credential (test
// values) and closes it.
func writeOldCredential(t *testing.T, dir, token string) string {
	t.Helper()
	path := filepath.Join(dir, relocate.DBName)
	d, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		colab.KVCredential, `{"refresh_token":"`+token+`"}`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileHolds(t *testing.T, path, needle string) bool {
	t.Helper()
	for _, sfx := range []string{"", "-wal", "-journal"} {
		if b, err := os.ReadFile(path + sfx); err == nil && strings.Contains(string(b), needle) {
			return true
		}
	}
	return false
}

// TestScrubOldDataDirAtStartup: the start on the new data directory
// removes the Colab credential from the old directory's database once
// (kv app.old_data_dir_scrubbed), and does nothing without a move.
func TestScrubOldDataDirAtStartup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	oldDir, newDir := filepath.Join(root, "old"), filepath.Join(root, "new")
	for _, d := range []string{oldDir, newDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const token = "1//test-refresh-token-startup-3f9a"
	oldDB := writeOldCredential(t, oldDir, token)
	st, err := db.Open(filepath.Join(newDir, relocate.DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	scrubOldDataDir(ctx, st, newDir) // no move recorded: nothing
	if !fileHolds(t, oldDB, token) {
		t.Fatal("scrubbed without a recorded move")
	}

	if err := st.SetKV(ctx, relocate.KVOldDataDir, oldDir); err != nil {
		t.Fatal(err)
	}
	scrubOldDataDir(ctx, st, newDir)
	if fileHolds(t, oldDB, token) {
		t.Error("the old database still holds the credential")
	}
	if v, _, _ := st.GetKV(ctx, relocate.KVOldDataDirScrubbed); v != oldDir {
		t.Errorf("marker %q", v)
	}

	// Once per move: a later start leaves the old directory alone.
	const again = "1//test-refresh-token-startup-again-6c1e"
	writeOldCredential(t, oldDir, again)
	scrubOldDataDir(ctx, st, newDir)
	if !fileHolds(t, oldDB, again) {
		t.Error("scrubbed again after the marker was set")
	}
}

// TestScrubOldDataDirLeavesNonLocalAlone (AUD1 L-1): a recorded old data
// directory that is not an absolute path on this computer's drives (a
// share an imported database named, a device path, a URL, a relative
// path) is never opened and gets no marker; a local one still is.
func TestScrubOldDataDirLeavesNonLocalAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, relocate.DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var calls []string
	scrubOld = func(_ context.Context, old, _ string) error {
		calls = append(calls, old)
		return nil
	}
	t.Cleanup(func() { scrubOld = relocate.ScrubOldDataDir })
	local := filepath.Join(t.TempDir(), "old")
	for _, v := range []string{`\\server\share\LectureFlow`, "//server/share/LectureFlow", `\\?\UNC\server\share\x`,
		`\\.\pipe\x`, "file://server/share/x", "relative/data", " " + local} {
		if err := st.SetKV(ctx, relocate.KVOldDataDir, v); err != nil {
			t.Fatal(err)
		}
		scrubOldDataDir(ctx, st, dir)
	}
	if len(calls) != 0 {
		t.Errorf("opened %q", calls)
	}
	if v, _, _ := st.GetKV(ctx, relocate.KVOldDataDirScrubbed); v != "" {
		t.Errorf("marker %q", v)
	}
	if err := st.SetKV(ctx, relocate.KVOldDataDir, local); err != nil {
		t.Fatal(err)
	}
	scrubOldDataDir(ctx, st, dir)
	if len(calls) != 1 || calls[0] != local {
		t.Errorf("local directory: calls %q", calls)
	}
}

// TestScrubOldDataDirFailureIsRetried: a failure leaves no marker (the
// next start tries again) and the start goes on.
func TestScrubOldDataDirFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, relocate.DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The recorded directory is the current one: refused.
	if err := st.SetKV(ctx, relocate.KVOldDataDir, dir); err != nil {
		t.Fatal(err)
	}
	scrubOldDataDir(ctx, st, dir)
	if v, _, _ := st.GetKV(ctx, relocate.KVOldDataDirScrubbed); v != "" {
		t.Errorf("marker set after a failure: %q", v)
	}
}
