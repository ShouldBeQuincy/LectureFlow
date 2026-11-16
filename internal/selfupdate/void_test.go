// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// preparedRollback lays out, in a new private data directory, the rollback
// state an account could prepare while it could write there (security
// review SEC17, 中-3: an "installation on probation" of the running
// version, the shell's record of it and of a backup's hash, the backup,
// previous.json naming it) next to what an update legitimately keeps (a
// pending update with its package, result.json) and to the browser's own
// files in the shell's profile. withProfile false: the shell's profile is
// not in the data directory.
func preparedRollback(t *testing.T, withProfile bool) string {
	t.Helper()
	data := privateDataDir(t)
	files := map[string]string{
		"updates/applying.json":                         `{"schema":1,"version":"3.2.0","from_version":"3.1.0","phase":"installing","tries":0,"target":"C:\\Programs\\LectureFlow","db_snapshot":"backup/3.1.0/db"}`,
		"updates/backup/3.1.0/app/LectureFlow.exe":      "another program",
		"updates/backup/3.1.0/app/resources/app.asar":   "another asar",
		"updates/backup/3.1.0/backup.json":              `{"schema":1,"version":"3.1.0","kind":"installer","item":"."}`,
		"updates/backup/3.1.0/db/lectureflow.db":        "an older database",
		"updates/previous.json":                         `{"schema":1,"version":"3.1.0","platform":"windows-x64","kind":"installer","file":"3.1.0/LectureFlow-Setup-3.1.0-x64.exe","size":7,"sha256":"ab","backup":"backup/3.1.0","created_at":"2026-11-02T12:00:00Z"}`,
		"updates/pending.json":                          `{"schema":1,"version":"3.3.0"}`,
		"updates/3.3.0/LectureFlow-Setup-3.3.0-x64.exe": "installer",
		"updates/result.json":                           `{"version":"3.2.0","from":"3.1.0","outcome":"installed"}`,
		"lectureflow.db":                                "the database",
	}
	if withProfile {
		files["electron/selfupdate-trust.json"] = `{"schema":1,"install":{"version":"3.2.0","from_version":"3.1.0","target":"C:\\Programs\\LectureFlow","kind":"installer"},"backups":{"3.1.0":{"dir":"backup/3.1.0","kind":"installer","item":".","tree":"ab"}}}`
		files["electron/Preferences"] = "{}"
	}
	for rel, body := range files {
		p := filepath.Join(data, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func pathThere(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// The rollback state kept in a data directory that holds the shell's
// profile is removed, all of it and nothing else.
func TestVoidRollback(t *testing.T) {
	data := preparedRollback(t, true)
	in := func(rel string) string { return filepath.Join(data, filepath.FromSlash(rel)) }
	v, err := VoidRollback(data)
	if err != nil || v != (Voided{Record: true, Applying: true, Backups: true, Previous: true}) || !v.Any() {
		t.Fatalf("VoidRollback: %+v %v", v, err)
	}
	for _, gone := range []string{"electron/selfupdate-trust.json", "updates/applying.json", "updates/backup"} {
		if pathThere(in(gone)) {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, kept := range []string{"electron/Preferences", "updates/pending.json", "updates/3.3.0/LectureFlow-Setup-3.3.0-x64.exe", "updates/result.json", "lectureflow.db"} {
		if !pathThere(in(kept)) {
			t.Errorf("%s was removed", kept)
		}
	}
	// previous.json names no backup any more; what Go wrote into it stays
	b, err := os.ReadFile(in("updates/previous.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, has := raw["backup"]; has || raw["version"] != "3.1.0" || raw["file"] != "3.1.0/LectureFlow-Setup-3.1.0-x64.exe" ||
		raw["sha256"] != "ab" || raw["size"] != float64(7) || raw["created_at"] != "2026-11-02T12:00:00Z" || raw["kind"] != "installer" {
		t.Fatalf("previous.json: %s", b)
	}
	// nothing left to remove
	if v, err := VoidRollback(data); err != nil || v.Any() {
		t.Fatalf("again: %+v %v", v, err)
	}
}

// With the shell's profile elsewhere (a moved data directory: the record
// lies under the user's own profile folder) nothing is touched: the record
// vouches for itself, and without one nothing in updates/ is believed.
func TestVoidRollbackProfileElsewhere(t *testing.T) {
	data := preparedRollback(t, false)
	v, err := VoidRollback(data)
	if err != nil || v.Any() {
		t.Fatalf("VoidRollback: %+v %v", v, err)
	}
	for _, kept := range []string{"updates/applying.json", "updates/backup/3.1.0/app/LectureFlow.exe", "updates/backup/3.1.0/db/lectureflow.db"} {
		if !pathThere(filepath.Join(data, filepath.FromSlash(kept))) {
			t.Errorf("%s was removed", kept)
		}
	}
	b, _ := os.ReadFile(filepath.Join(data, "updates", "previous.json"))
	var prev Previous
	if err := json.Unmarshal(b, &prev); err != nil || prev.Backup != "backup/3.1.0" {
		t.Fatalf("previous.json: %s %v", b, err)
	}
	// a data directory without updates/ or without any of the files
	empty := privateDataDir(t)
	if err := os.Mkdir(filepath.Join(empty, ShellProfileDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if v, err := VoidRollback(empty); err != nil || v.Any() {
		t.Fatalf("an empty data directory: %+v %v", v, err)
	}
}

// previous.json that names no backup, or cannot be read, is left as it is.
func TestVoidRollbackKeepsPreviousWithoutBackup(t *testing.T) {
	data := preparedRollback(t, true)
	p := filepath.Join(data, "updates", "previous.json")
	for _, body := range []string{`{"schema":1,"version":"3.1.0","platform":"windows-x64","kind":"installer","created_at":"2026-11-02T12:00:00Z"}`, `not JSON`} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := VoidRollback(data)
		if err != nil || v.Previous {
			t.Fatalf("%q: %+v %v", body, v, err)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Fatalf("previous.json was rewritten: %s", b)
		}
	}
}
