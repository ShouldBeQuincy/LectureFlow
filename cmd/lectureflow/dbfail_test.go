// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/secrets"
)

// asFailure is err as a *startFailure with code, the ERROR text in both
// languages and the technical cause at its end.
func asFailure(t *testing.T, err error, code string) *startFailure {
	t.Helper()
	var sf *startFailure
	if !errors.As(err, &sf) {
		t.Fatalf("not a coded failure: %v", err)
	}
	if sf.Code != code {
		t.Fatalf("code %q, want %q (%v)", sf.Code, code, err)
	}
	if !strings.Contains(sf.Msg, " / ") || !strings.HasSuffix(sf.Msg, ")") || !strings.Contains(sf.Msg, "open db: ") {
		t.Fatalf("ERROR text %q", sf.Msg)
	}
	return sf
}

// F5: another program holds the write lock while the start migrates.
func TestDBFailureBusy(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	dataDir := t.TempDir()
	path := dbFile(dataDir)
	buildOldDB(t, path, 9)
	abs, _ := filepath.Abs(path)
	holder, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	c, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	err = prepareDatabase(context.Background(), dataDir, "3.2.0", nil)
	sf := asFailure(t, err, failDBBusy)
	if sf.Params["path"] != path || !strings.Contains(sf.Msg, "占用") || !strings.Contains(sf.Msg, "in use") {
		t.Fatalf("%+v", sf)
	}
}

// F5: the database file is read-only (Windows: the read-only attribute).
// Not verified on macOS.
func TestDBFailureReadOnly(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	dataDir := t.TempDir()
	path := dbFile(dataDir)
	buildOldDB(t, path, 9)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })
	err := prepareDatabase(context.Background(), dataDir, "3.2.0", nil)
	var sf *startFailure
	if errors.As(err, &sf) && sf.Code == failUpgradeBackupFailed {
		t.Fatalf("the backup failed first: %v", err)
	}
	sf = asFailure(t, err, failDBReadOnly)
	if sf.Params["path"] != path || !strings.Contains(sf.Msg, "只读") || !strings.Contains(sf.Msg, "read-only") {
		t.Fatalf("%+v", sf)
	}
}

// F5, end to end in desktop mode: a lectureflow.db that is not a database
// fails the start with startup.db_not_a_database just before the ERROR
// line, and the file is left as it was; web mode prints no code line.
func TestDBFailureNotADatabaseStart(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, dataDir := writeTestConfig(t)
	os.MkdirAll(dataDir, 0o755)
	path := filepath.Join(dataDir, "lectureflow.db")
	junk := []byte(strings.Repeat("not a database, just some bytes. ", 300))
	if err := os.WriteFile(path, junk, 0o644); err != nil {
		t.Fatal(err)
	}
	before := fileSum(t, path)

	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	if c := p.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("exit %d", c)
	}
	code, params, last := failureOf(t, p.stderr.String())
	if code != failDBNotDB || params["path"] != path || params["backups"] != filepath.Join(dataDir, "backups") {
		t.Fatalf("failure %q %v\n%s", code, params, p.stderr.String())
	}
	if !strings.HasPrefix(last, "ERROR: ") || !strings.Contains(last, "不是可用的数据库文件") || !strings.Contains(last, "not a usable database") ||
		!strings.Contains(last, "open db: ") {
		t.Fatalf("ERROR line %q", last)
	}
	if fileSum(t, path) != before {
		t.Fatal("the file changed")
	}

	secrets.ResetForTest()
	w := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	if c := w.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("web mode exit %d", c)
	}
	if code, _, last := failureOf(t, w.stderr.String()); code != "" || !strings.Contains(last, "not a usable database") {
		t.Fatalf("web mode: %q %q", code, last)
	}
}

// Errors of no known kind keep their text.
func TestDBFailureOtherErrors(t *testing.T) {
	err := dbFailure(t.TempDir(), "open db", errors.New("bad migration name x"))
	var sf *startFailure
	if errors.As(err, &sf) || err.Error() != "open db: bad migration name x" {
		t.Fatalf("%v", err)
	}
}

// SEC12 M-1: "database table is locked" is this program's own fault (a
// conflict inside one connection): no startup.db_busy, which the desktop
// shell would not count against a freshly installed version.
func TestDBFailureTableLockedIsNoCode(t *testing.T) {
	ctx := context.Background()
	abs, _ := filepath.Abs(filepath.Join(t.TempDir(), "locked.db"))
	d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, "CREATE TABLE a(x); INSERT INTO a VALUES (1), (2)"); err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT x FROM a")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	_, locked := tx.ExecContext(ctx, "DROP TABLE a")
	if locked == nil || !strings.Contains(locked.Error(), "locked") {
		t.Fatalf("not the table lock: %v", locked)
	}
	err = dbFailure(t.TempDir(), "open db", locked)
	var sf *startFailure
	if errors.As(err, &sf) {
		t.Fatalf("coded %s: %v", sf.Code, err)
	}
	if !strings.HasPrefix(err.Error(), "open db: ") {
		t.Fatalf("%v", err)
	}
}

// Every startup.db_* code has its text (desktop/locales "startfail.db_*")
// in the six languages of the desktop shell, with the placeholders the
// shell fills ({path}, and {backups} for the damaged database).
func TestDBFailureCodesHaveShellTexts(t *testing.T) {
	codes := map[string][]string{
		failDBBusy: {"path"}, failDBReadOnly: {"path"}, failDBNotDB: {"path", "backups"},
		failDBCantOpen: {"path"}, failDBFull: {"path"}, failDBIO: {"path"},
	}
	for _, loc := range []string{"zh-CN", "en", "zh-TW", "yue-Hant-HK", "ru", "hi"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "desktop", "locales", loc+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", loc, err)
		}
		for code, ps := range codes {
			key := "startfail." + strings.TrimPrefix(code, "startup.")
			text := m[key]
			if text == "" {
				t.Errorf("%s.json has no %s", loc, key)
				continue
			}
			for _, p := range ps {
				if !strings.Contains(text, "{"+p+"}") {
					t.Errorf("%s.json %s: no {%s}", loc, key, p)
				}
			}
		}
		// Security review M2: replacing or moving lectureflow.db away
		// takes its -wal and -shm along, or the leftover WAL is replayed
		// onto the restored backup.
		for _, key := range []string{"startfail.db_not_a_database", "startfail.data_dir_newer"} {
			if !strings.Contains(m[key], "lectureflow.db-wal") || !strings.Contains(m[key], "lectureflow.db-shm") {
				t.Errorf("%s.json %s: no -wal / -shm", loc, key)
			}
		}
		// Security review M3: an open refused or a write refused may be
		// another program (backup, sync, antivirus) holding the file.
		for _, key := range []string{"startfail.db_cant_open", "startfail.db_readonly"} {
			if want := map[string]string{"zh-CN": "杀毒软件", "en": "antivirus"}[loc]; want != "" && !strings.Contains(m[key], want) {
				t.Errorf("%s.json %s: no other program (%s)", loc, key, want)
			}
		}
	}
}

// The ERROR texts give the same guidance (security review M2, M3).
func TestDBFailureTextsGuidance(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.db")
	os.WriteFile(junk, []byte(strings.Repeat("not a database, just some bytes. ", 300)), 0o644)
	_, notDB := db.InspectFile(context.Background(), junk)
	_, cantOpen := db.InspectFile(context.Background(), filepath.Join(dir, "no such folder", "lectureflow.db"))
	readOnly := &fs.PathError{Op: "open", Path: junk, Err: fs.ErrPermission}
	for _, e := range []struct {
		err  error
		want string
	}{{notDB, db.OpenFailNotDB}, {cantOpen, db.OpenFailCantOpen}, {readOnly, db.OpenFailReadOnly}} {
		if got := db.ClassifyOpenError(e.err); got != e.want {
			t.Fatalf("%v: %q, want %q", e.err, got, e.want)
		}
	}
	for _, tc := range []struct {
		failure *startFailure
		want    []string
	}{
		{dbFailure(dir, "open db", notDB).(*startFailure), []string{"lectureflow.db-wal", "lectureflow.db-shm", "复制到数据目录并改名为 lectureflow.db（只放这一个文件）", "into the data folder and rename it to lectureflow.db (only that one file)"}},
		{newerFailure(dir, 99, 1, nil), []string{"lectureflow.db-wal", "lectureflow.db-shm", "复制到数据目录并改名为 lectureflow.db（只放这一个文件）", "into the data folder and rename it to lectureflow.db (only that one file)"}},
		{dbFailure(dir, "open db", cantOpen).(*startFailure), []string{"杀毒软件", "antivirus"}},
		{dbFailure(dir, "open db", readOnly).(*startFailure), []string{"杀毒软件", "antivirus"}},
	} {
		for _, w := range tc.want {
			if !strings.Contains(tc.failure.Msg, w) {
				t.Errorf("%s: no %q in %s", tc.failure.Code, w, tc.failure.Msg)
			}
		}
	}
}
