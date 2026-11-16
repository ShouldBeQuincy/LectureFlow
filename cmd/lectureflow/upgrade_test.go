// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/secrets"
)

// Test values only (never real keys).
const (
	upgKey     = "sk-test-upgrade-cmd-5150"
	upgRefresh = "1//test-refresh-upgrade-cmd-61"
)

// buildOldDB writes a database of schema version upTo at path, with the
// repository's migration files (as an older LectureFlow left it): a
// course, a provider with a plain API key, a plain Colab credential.
func buildOldDB(t *testing.T, path string, upTo int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(path)
	d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	dir := filepath.Join("..", "..", "internal", "db", "migrations")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		v, _ := strconv.Atoi(n[:3])
		if v > upTo {
			break
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if _, err := d.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO course (id, name, created_at) VALUES ('c1','CSC3631','2026-10-01T00:00:00Z')`,
		`INSERT INTO provider (id, name, type, base_url, api_key, created_at) VALUES ('p1','DeepSeek','openai','https://api.deepseek.com','` + upgKey + `','2026-10-01T00:00:00Z')`,
		`INSERT INTO kv (key, value) VALUES ('colab.credential','{"refresh_token":"` + upgRefresh + `"}')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func schemaOf(t *testing.T, path string) db.FileInfo {
	t.Helper()
	fi, err := db.InspectFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func upgradeBackups(t *testing.T, dataDir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dataDir, "backups", upgradeBackupPrefix+"*"))
	var out []string
	for _, p := range m {
		out = append(out, filepath.Base(p))
	}
	return out
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// failureOf returns the code line and the last line of stderr.
func failureOf(t *testing.T, stderr string) (code string, params map[string]any, last string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	last = lines[len(lines)-1]
	for i, l := range lines {
		if !strings.HasPrefix(l, FailurePrefix) {
			continue
		}
		if i != len(lines)-2 {
			t.Fatalf("the failure line is not just before the ERROR line:\n%s", stderr)
		}
		var f struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(l, FailurePrefix)), &f); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		return f.Code, f.Params, last
	}
	return "", nil, last
}

// F1: the first start of a new version on an older database copies it to
// backups/ (no credential in the copy, the old schema version and rows)
// before it migrates and seals; a later start makes no other copy; an
// unfinished copy a killed start left is removed.
func TestUpgradeBackupBeforeMigration(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, dataDir := writeTestConfig(t)
	path := filepath.Join(dataDir, "lectureflow.db")
	buildOldDB(t, path, 9)
	stale := filepath.Join(dataDir, "backups", upgradeBackupPrefix+"schema9-to-x-20261001-000000.db"+db.PartialSuffix)
	os.MkdirAll(filepath.Dir(stale), 0o755)
	if err := os.WriteFile(stale, []byte("plain "+upgKey), 0o600); err != nil {
		t.Fatal(err)
	}

	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-app-version", "3.2.0"}, "", AppOptions{})
	p.ready()
	<-p.app
	baks := upgradeBackups(t, dataDir)
	if len(baks) != 1 || !strings.HasPrefix(baks[0], upgradeBackupPrefix+"schema9-to-3.2.0-") || !strings.HasSuffix(baks[0], ".db") {
		t.Fatalf("backups %v", baks)
	}
	bak := filepath.Join(dataDir, "backups", baks[0])
	for _, needle := range []string{upgKey, upgRefresh, "enc:v1:"} {
		if fileHolds(t, bak, needle) {
			t.Fatalf("the backup holds %q", needle)
		}
	}
	bi := schemaOf(t, bak)
	if bi.Version != 9 || bi.HasSecrets {
		t.Fatalf("backup %+v", bi)
	}
	mi := schemaOf(t, path)
	if mi.Version != db.SchemaVersion() || mi.UpgradeBackup != fmt.Sprintf("%d|%s", db.SchemaVersion(), baks[0]) {
		t.Fatalf("main %+v", mi)
	}
	p.sigc <- os.Interrupt
	if c := p.wait(15 * time.Second); c != exitOK {
		t.Fatalf("exit %d", c)
	}
	if fileHolds(t, path, upgKey) {
		t.Fatal("the database still holds the key in the clear")
	}

	secrets.ResetForTest()
	p = startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-app-version", "3.2.1"}, "", AppOptions{})
	p.ready()
	<-p.app
	if got := upgradeBackups(t, dataDir); len(got) != 1 {
		t.Fatalf("a start with nothing to migrate made a backup: %v", got)
	}
}

// F1: without the space for the backup, nothing is migrated: the start
// fails with startup.upgrade_backup_no_space, the database is unchanged.
func TestUpgradeBackupNoSpace(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	old := diskFreeFn
	diskFreeFn = func(string) (int64, error) { return 10 << 20, nil }
	t.Cleanup(func() { diskFreeFn = old })
	cfgPath, dataDir := writeTestConfig(t)
	path := filepath.Join(dataDir, "lectureflow.db")
	buildOldDB(t, path, 9)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	if c := p.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("exit %d", c)
	}
	code, params, last := failureOf(t, p.stderr.String())
	if code != failUpgradeBackupSpace || params["dir"] != filepath.Join(dataDir, "backups") || params["free_bytes"].(float64) != 10<<20 ||
		params["need_bytes"].(float64) <= 64<<20 {
		t.Fatalf("failure %q %v", code, params)
	}
	if !strings.HasPrefix(last, "ERROR: ") || !strings.Contains(last, "空闲空间") || !strings.Contains(last, "free space") {
		t.Fatalf("ERROR line %q", last)
	}
	if fi := schemaOf(t, path); fi.Version != 9 || fi.UpgradeBackup != "" {
		t.Fatalf("the database changed: %+v", fi)
	}
	if !fileHolds(t, path, upgKey) {
		t.Fatal("the credentials changed (sealed) although nothing should have")
	}
	if b := upgradeBackups(t, dataDir); len(b) != 0 {
		t.Fatalf("files in backups: %v", b)
	}
}

// F1: a backup that cannot be written stops the start before the migration.
func TestUpgradeBackupFailureStopsUpgrade(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, dataDir := writeTestConfig(t)
	path := filepath.Join(dataDir, "lectureflow.db")
	buildOldDB(t, path, 9)
	if err := os.WriteFile(filepath.Join(dataDir, "backups"), []byte("not a folder"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	if c := p.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("exit %d", c)
	}
	code, _, last := failureOf(t, p.stderr.String())
	if code != failUpgradeBackupFailed || !strings.Contains(last, "没有升级") {
		t.Fatalf("failure %q / %q", code, last)
	}
	if fi := schemaOf(t, path); fi.Version != 9 {
		t.Fatalf("migrated without a backup: %+v", fi)
	}
}

// F2: a database from a newer LectureFlow: the start is refused, the
// database file is not changed, no staged import is applied, no key file
// is made. Web mode has the reason in Chinese and English, no code line.
func TestNewerDatabaseRefused(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, dataDir := writeTestConfig(t)
	path := filepath.Join(dataDir, "lectureflow.db")
	os.MkdirAll(dataDir, 0o755)
	d, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	newer := db.SchemaVersion() + 1
	if _, err := d.Exec(fmt.Sprintf("PRAGMA user_version = %d", newer)); err != nil {
		t.Fatal(err)
	}
	d.Close()
	before := fileSum(t, path)
	staged := false
	oldStaged := applyStaged
	applyStaged = func(context.Context, string) error { staged = true; return nil }
	t.Cleanup(func() { applyStaged = oldStaged })

	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	if c := p.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("exit %d", c)
	}
	code, params, last := failureOf(t, p.stderr.String())
	if code != failDataDirNewer || params["found"].(float64) != float64(newer) || params["known"].(float64) != float64(db.SchemaVersion()) ||
		params["backups"] != filepath.Join(dataDir, "backups") {
		t.Fatalf("failure %q %v", code, params)
	}
	if !strings.Contains(last, "更新版本") || !strings.Contains(last, "newer LectureFlow") {
		t.Fatalf("ERROR line %q", last)
	}
	if fileSum(t, path) != before {
		t.Fatal("the database file changed")
	}
	if staged {
		t.Fatal("the staged import was applied")
	}
	if _, err := os.Stat(filepath.Join(dataDir, secrets.KeyFileName)); err == nil {
		t.Fatal("a key file was made")
	}

	w := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	if c := w.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("web mode exit %d", c)
	}
	if code, _, last := failureOf(t, w.stderr.String()); code != "" || !strings.Contains(last, "newer LectureFlow") {
		t.Fatalf("web mode: %q %q", code, last)
	}
}

// Security review M1: a database from a newer LectureFlow whose schema
// this SQLite cannot parse (a newer table option) fails to be read, which
// SQLite reports as malformed. It is still refused as from a newer version
// (startup.data_dir_newer), by checkNotNewer and by prepareDatabase, never
// as a damaged database to replace or move away; the file is not changed.
func TestNewerDatabaseUnparseableSchemaRefused(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	dataDir := t.TempDir()
	path := dbFile(dataDir)
	d, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	newer := db.SchemaVersion() + 5
	abs, _ := filepath.Abs(path)
	raw, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	for _, q := range []string{
		"CREATE TABLE future(x)",
		"PRAGMA writable_schema = ON",
		"UPDATE sqlite_master SET sql = 'CREATE TABLE future(x) WITH FUTURE_OPTION' WHERE name = 'future'",
		"PRAGMA writable_schema = OFF",
		fmt.Sprintf("PRAGMA user_version = %d", newer),
	} {
		if _, err := raw.Exec(q); err != nil {
			raw.Close()
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	if _, err := db.InspectFile(context.Background(), path); err == nil {
		t.Fatal("the schema was parsed: the test does not reach the case")
	}
	if v, ok := db.HeaderUserVersion(path); !ok || v != newer {
		t.Fatalf("header user_version %d %v, want %d", v, ok, newer)
	}
	before := fileSum(t, path)

	check := func(name string, err error) {
		t.Helper()
		var sf *startFailure
		if !errors.As(err, &sf) || sf.Code != failDataDirNewer {
			t.Fatalf("%s: %v, want %s", name, err, failDataDirNewer)
		}
		if sf.Params["found"] != newer || sf.Params["known"] != db.SchemaVersion() || sf.Params["backups"] != backupsDir(dataDir) {
			t.Fatalf("%s: %v", name, sf.Params)
		}
		if strings.Contains(sf.Msg, "不是可用的数据库文件") || strings.Contains(sf.Msg, "not a usable database") {
			t.Fatalf("%s: %s", name, sf.Msg)
		}
	}
	check("checkNotNewer", checkNotNewer(context.Background(), dataDir, nil))
	check("prepareDatabase", prepareDatabase(context.Background(), dataDir, "3.2.0", nil))
	if fileSum(t, path) != before {
		t.Fatal("the database file changed")
	}
}

// D15 (R9 suggestion 1): as above, but the newer LectureFlow was stopped
// hard after its migrations, before a checkpoint: the newer version and
// schema are only in the -wal, the file header still has the version
// this one knows. It is refused as from a newer version all the same,
// not called "not a usable database" (whose text says to move it away).
func TestNewerDatabaseOnlyInWALRefused(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	src := filepath.Join(t.TempDir(), "lectureflow.db")
	d, err := db.OpenDB(src)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	newer := db.SchemaVersion() + 5
	abs, _ := filepath.Abs(src)
	raw, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	for _, q := range []string{
		"CREATE TABLE future(x)",
		"PRAGMA writable_schema = ON",
		"UPDATE sqlite_master SET sql = 'CREATE TABLE future(x) WITH FUTURE_OPTION' WHERE name = 'future'",
		"PRAGMA writable_schema = OFF",
		fmt.Sprintf("PRAGMA user_version = %d", newer),
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// the files as a process killed now leaves them
	dataDir := t.TempDir()
	path := dbFile(dataDir)
	for _, s := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(src + s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+s, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok := db.HeaderUserVersion(path); !ok || v != db.SchemaVersion() {
		t.Fatalf("header user_version %d %v: the test does not reach the case", v, ok)
	}
	if _, err := db.InspectFile(context.Background(), path); err == nil {
		t.Fatal("the schema was parsed: the test does not reach the case")
	}
	before := fileSum(t, path)
	beforeWAL := fileSum(t, path+"-wal")
	for name, f := range map[string]func() error{
		"checkNotNewer":   func() error { return checkNotNewer(context.Background(), dataDir, nil) },
		"prepareDatabase": func() error { return prepareDatabase(context.Background(), dataDir, "3.2.0", nil) },
	} {
		err := f()
		var sf *startFailure
		if !errors.As(err, &sf) || sf.Code != failDataDirNewer || sf.Params["found"] != newer {
			t.Errorf("%s: %v, want %s (found %d)", name, err, failDataDirNewer, newer)
		}
	}
	if fileSum(t, path) != before || fileSum(t, path+"-wal") != beforeWAL {
		t.Fatal("the database or its -wal changed")
	}
}

// walUserVersion takes the version of the last committed page 1 whose
// frames check out, as SQLite does: a damaged last frame drops that
// transaction, a damaged WAL header the whole log.
func TestWALUserVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.db")
	abs, _ := filepath.Abs(path)
	raw, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	known := db.SchemaVersion()
	for _, q := range []string{
		"CREATE TABLE a(x)",
		fmt.Sprintf("PRAGMA user_version = %d", known),
		"INSERT INTO a VALUES (1)",
		fmt.Sprintf("PRAGMA user_version = %d", known+3),
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	try := func(name string, b []byte) (int, bool) {
		t.Helper()
		p := filepath.Join(t.TempDir(), name+"-wal")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return walUserVersion(p)
	}
	if v, ok := try("whole", wal); !ok || v != known+3 {
		t.Fatalf("whole log: %d %v", v, ok)
	}
	torn := append([]byte(nil), wal...)
	torn[len(torn)-1] ^= 0xff // the last transaction's commit frame
	if v, ok := try("torn", torn); !ok || v != known {
		t.Fatalf("torn last frame: %d %v", v, ok)
	}
	if v, ok := try("cut", wal[:len(wal)-10]); !ok || v != known {
		t.Fatalf("cut last frame: %d %v", v, ok)
	}
	badHeader := append([]byte(nil), wal...)
	badHeader[17] ^= 0xff // salt-1
	if v, ok := try("salt", badHeader); ok {
		t.Fatalf("damaged header: %d", v)
	}
	if _, ok := try("empty", nil); ok {
		t.Fatal("a version from an empty log")
	}
	if _, ok := walUserVersion(filepath.Join(t.TempDir(), "none-wal")); ok {
		t.Fatal("a version from no log")
	}
}

// Not an SQLite file, or no file: no version from the header.
func TestHeaderUserVersionNotSQLite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.db")
	os.WriteFile(p, []byte(strings.Repeat("not a database ", 20)), 0o644)
	if _, ok := db.HeaderUserVersion(p); ok {
		t.Fatal("a version from a file that is not a database")
	}
	if _, ok := db.HeaderUserVersion(filepath.Join(dir, "none.db")); ok {
		t.Fatal("a version from no file")
	}
}

// A damaged database whose header kept the magic but nothing else (bytes
// 16-99 overwritten, user_version reading as 1094795585) is a damaged
// database, not one from a newer version (R9 suggestion 2).
func TestDamagedHeaderNotNewer(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	dataDir := t.TempDir()
	path := dbFile(dataDir)
	d, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 16; i < 100; i++ {
		b[i] = 'A'
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InspectFile(context.Background(), path); err == nil {
		t.Fatal("the damaged database was read: the test does not reach the case")
	}
	if nf := checkNotNewer(context.Background(), dataDir, nil); nf != nil {
		t.Fatalf("checkNotNewer: %v", nf)
	}
	var sf *startFailure
	err = prepareDatabase(context.Background(), dataDir, "3.2.0", nil)
	if !errors.As(err, &sf) || sf.Code == failDataDirNewer {
		t.Fatalf("prepareDatabase: %v, want a database failure, not %s", err, failDataDirNewer)
	}
	if sf.Code != failDBNotDB {
		t.Fatalf("prepareDatabase: %s, want %s", sf.Code, failDBNotDB)
	}
}

// F1: a migration that failed after the backup was made: the next start
// finds the backup recorded and makes no other one (the oldest state is
// the one worth keeping).
func TestUpgradeBackupOncePerUpgrade(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "lectureflow.db")
	buildOldDB(t, path, 9)
	bdir := filepath.Join(dataDir, "backups")
	os.MkdirAll(bdir, 0o755)
	name := upgradeBackupPrefix + "schema9-to-3.2.0-20261027-100000.db"
	if err := os.WriteFile(filepath.Join(bdir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUpgradeBackup(ctx, path, fmt.Sprintf("%d|%s", db.SchemaVersion(), name)); err != nil {
		t.Fatal(err)
	}
	if err := prepareDatabase(ctx, dataDir, "3.2.0", nil); err != nil {
		t.Fatal(err)
	}
	if got := upgradeBackups(t, dataDir); len(got) != 1 || got[0] != name {
		t.Fatalf("backups %v", got)
	}
	if fi := schemaOf(t, path); fi.Version != db.SchemaVersion() {
		t.Fatalf("not migrated: %+v", fi)
	}

	// a mark for another upgrade, or naming a file that is gone: a new backup
	dataDir2 := t.TempDir()
	path2 := filepath.Join(dataDir2, "lectureflow.db")
	buildOldDB(t, path2, 9)
	if err := db.SetUpgradeBackup(ctx, path2, fmt.Sprintf("%d|%s", db.SchemaVersion(), name)); err != nil {
		t.Fatal(err)
	}
	if err := prepareDatabase(ctx, dataDir2, "3.2.0", nil); err != nil {
		t.Fatal(err)
	}
	if got := upgradeBackups(t, dataDir2); len(got) != 1 || got[0] == name {
		t.Fatalf("backups %v", got)
	}
}

func TestRotateUpgradeBackups(t *testing.T) {
	bdir := t.TempDir()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var names []string
	for i := 0; i < 5; i++ {
		n := fmt.Sprintf("%sschema%d-to-3.2.0-2026100%d-120000.db", upgradeBackupPrefix, 9, i+1)
		p := filepath.Join(bdir, n)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(p+"-wal", []byte("w"), 0o600)
		os.Chtimes(p, base.Add(time.Duration(i)*time.Hour), base.Add(time.Duration(i)*time.Hour))
		names = append(names, n)
	}
	other := filepath.Join(bdir, "lectureflow-before-import-20261001-120000.db")
	os.WriteFile(other, []byte("x"), 0o600)
	// the one just made is kept even with an older time
	os.Chtimes(filepath.Join(bdir, names[0]), base.Add(-time.Hour), base.Add(-time.Hour))
	rotateUpgradeBackups(bdir, names[0])
	var left []string
	ents, _ := os.ReadDir(bdir)
	for _, e := range ents {
		left = append(left, e.Name())
	}
	want := []string{filepath.Base(other), names[0], names[3], names[3] + "-wal", names[4], names[4] + "-wal", names[0] + "-wal"}
	sort.Strings(want)
	sort.Strings(left)
	if strings.Join(left, ",") != strings.Join(want, ",") {
		t.Fatalf("left %v\nwant %v", left, want)
	}
}

// SEC12 L-1: a read-only database (the record of the backup cannot be
// written, the migrations fail): "Try again" three times makes one backup,
// not one per start; a database changed since gets a new one.
func TestUpgradeBackupOnceWhenReadOnly(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	dataDir := t.TempDir()
	path := dbFile(dataDir)
	buildOldDB(t, path, 9)
	// an older upgrade's backup (the way back to an older LectureFlow)
	bdir := backupsDir(dataDir)
	os.MkdirAll(bdir, 0o700)
	older := filepath.Join(bdir, upgradeBackupPrefix+"schema8-to-3.1.0-20261001-120000.db")
	os.WriteFile(older, []byte("x"), 0o600)
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(older, past, past)
	os.Chtimes(path, past.Add(time.Hour), past.Add(time.Hour))
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })
	clock := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	saved := upgradeNow
	upgradeNow = func() time.Time { clock = clock.Add(time.Minute); return clock }
	t.Cleanup(func() { upgradeNow = saved })

	for i := 1; i <= 3; i++ {
		err := prepareDatabase(ctx, dataDir, "3.2.0", nil)
		asFailure(t, err, failDBReadOnly)
		got := upgradeBackups(t, dataDir)
		if len(got) != 2 { // the older one and one of this upgrade
			t.Fatalf("start %d: backups %v", i, got)
		}
	}
	if _, err := os.Stat(older); err != nil {
		t.Fatal("the older upgrade's backup was removed")
	}

	// the database changed since that backup: a new one
	os.Chmod(path, 0o644)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o444)
	asFailure(t, prepareDatabase(ctx, dataDir, "3.2.0", nil), failDBReadOnly)
	if got := upgradeBackups(t, dataDir); len(got) != 3 {
		t.Fatalf("after a change: backups %v", got)
	}
}

// A backup of the same upgrade that is not a database of that schema, or
// of another upgrade, does not count as made.
func TestUpgradeBackupUnrecordedOnlySameUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lectureflow.db")
	buildOldDB(t, path, 9)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(path, past, past)
	bdir := filepath.Join(dir, "backups")
	os.MkdirAll(bdir, 0o700)
	put := func(name string, src string) {
		b, _ := os.ReadFile(src)
		os.WriteFile(filepath.Join(bdir, name), b, 0o600)
	}
	put(upgradeBackupPrefix+"schema9-to-3.3.0-20261111-090000.db", path) // another version
	put(upgradeBackupPrefix+"schema8-to-3.2.0-20261111-090000.db", path) // another schema in the name
	os.WriteFile(filepath.Join(bdir, upgradeBackupPrefix+"schema9-to-3.2.0-20261111-090100.db"), []byte("not a database"), 0o600)
	if name, ok := upgradeBackupUnrecorded(path, bdir, 9, "3.2.0"); ok {
		t.Fatalf("taken: %s", name)
	}
	put(upgradeBackupPrefix+"schema9-to-3.2.0-20261111-090200.db", path)
	if name, ok := upgradeBackupUnrecorded(path, bdir, 9, "3.2.0"); !ok || name != upgradeBackupPrefix+"schema9-to-3.2.0-20261111-090200.db" {
		t.Fatalf("%q %v", name, ok)
	}
}

// SEC12 review L-4: a backup an unrecorded attempt left is not taken for
// the backup of another copy of the database put in its place afterwards,
// although that copy keeps an older modification time (a copy in Explorer,
// a restore from a sync folder) and has no -wal (journal_mode DELETE, as
// a copy taken out of backups/ can be).
//
// Overwritten in place, the file keeps its creation time, and its
// modification time is set back to the old one: only the change time
// (ctime) tells (SEC31 L4). Deleted and written again under the same name
// at once, NTFS may give it the old creation time back (file name
// tunneling), and then again only the change time tells; where that does
// not happen, the case shows only that a new file is not taken for the
// old one.
func TestUpgradeBackupNotReusedForAReplacedCopy(t *testing.T) {
	t.Run("overwritten in place", func(t *testing.T) {
		dataDir, path, past, first, copyBytes := replacedCopySetup(t)
		created, haveCreated := testCreationTime(path)
		for _, s := range []string{"-wal", "-shm", "-journal"} {
			if err := os.Remove(path + s); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(path, copyBytes, 0o644); err != nil { // truncates the same file
			t.Fatal(err)
		}
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
		// the case: neither the creation nor the modification time changed
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if d := fi.ModTime().Sub(past); d < -time.Microsecond || d > time.Microsecond {
			t.Fatalf("modification time %v, set to %v: the test does not reach the case", fi.ModTime(), past)
		}
		if haveCreated {
			if now, ok := testCreationTime(path); !ok || !now.Equal(created) {
				t.Fatalf("creation time %v, was %v: the test does not reach the case", now, created)
			}
		} else {
			t.Logf("creation time not read on %s; the modification time is the old one", runtime.GOOS)
		}
		checkReplacedCopyBackedUp(t, dataDir, first)
	})
	t.Run("deleted and written again", func(t *testing.T) {
		dataDir, path, past, first, copyBytes := replacedCopySetup(t)
		created, haveCreated := testCreationTime(path)
		if err := db.RemoveDBFiles(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, copyBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		older := past.Add(time.Hour)
		if err := os.Chtimes(path, older, older); err != nil {
			t.Fatal(err)
		}
		if now, ok := testCreationTime(path); haveCreated && ok && now.Equal(created) {
			t.Logf("file name tunneling: the new file has the old creation time; only the change time tells")
		} else {
			t.Logf("no file name tunneling here (creation time %v, was %v): this case does not reach the change time", now, created)
		}
		checkReplacedCopyBackedUp(t, dataDir, first)
	})
}

// replacedCopySetup: a schema-9 database whose upgrade backup was made but
// not recorded (read-only), and the bytes of another copy of a schema-9
// database with a course only it has (journal_mode DELETE, no -wal).
func replacedCopySetup(t *testing.T) (dataDir, path string, past time.Time, first []string, copyBytes []byte) {
	t.Helper()
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	dataDir = t.TempDir()
	path = dbFile(dataDir)
	buildOldDB(t, path, 9)
	past = time.Now().Add(-48 * time.Hour)
	os.Chtimes(path, past, past)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })
	asFailure(t, prepareDatabase(ctx, dataDir, "3.2.0", nil), failDBReadOnly)
	first = upgradeBackups(t, dataDir)
	if len(first) != 1 {
		t.Fatalf("backups %v", first)
	}
	os.Chmod(path, 0o644)

	other := filepath.Join(t.TempDir(), "copy.db")
	buildOldDB(t, other, 9)
	d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(other)}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	for _, q := range []string{
		`INSERT INTO course (id, name, created_at) VALUES ('c2','ONLY-IN-THE-COPY','2026-10-02T00:00:00Z')`,
		`PRAGMA journal_mode = DELETE`,
	} {
		if _, err := d.Exec(q); err != nil {
			d.Close()
			t.Fatal(err)
		}
	}
	d.Close()
	if _, err := os.Stat(other + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the copy has a -wal: %v", err)
	}
	if copyBytes, err = os.ReadFile(other); err != nil {
		t.Fatal(err)
	}
	return dataDir, path, past, first, copyBytes
}

// checkReplacedCopyBackedUp: the start migrates the copy now in place
// after a backup of its own, holding its course.
func checkReplacedCopyBackedUp(t *testing.T, dataDir string, first []string) {
	t.Helper()
	if err := prepareDatabase(context.Background(), dataDir, "3.2.0", nil); err != nil {
		t.Fatal(err)
	}
	got := upgradeBackups(t, dataDir)
	if len(got) != 2 {
		t.Fatalf("the earlier backup was taken for this copy's: backups %v", got)
	}
	for _, n := range got {
		if n == first[0] {
			continue
		}
		bd, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(filepath.Join(backupsDir(dataDir), n))}).EscapedPath()+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var c int
		err = bd.QueryRow(`SELECT COUNT(*) FROM course WHERE name = 'ONLY-IN-THE-COPY'`).Scan(&c)
		bd.Close()
		if err != nil || c != 1 {
			t.Fatalf("the new backup %s does not hold the copy's course: %d %v", n, c, err)
		}
	}
}

// SEC12 L-1: the rotation keeps the newest backup of every source schema.
func TestRotateKeepsTheNewestOfEverySchema(t *testing.T) {
	bdir := t.TempDir()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mk := func(schema, i int) string {
		n := fmt.Sprintf("%sschema%d-to-3.2.0-2026100%d-120000.db", upgradeBackupPrefix, schema, i)
		p := filepath.Join(bdir, n)
		os.WriteFile(p, []byte("x"), 0o600)
		ts := base.Add(time.Duration(i) * time.Hour)
		os.Chtimes(p, ts, ts)
		return n
	}
	old8a, old8b := mk(8, 1), mk(8, 2)
	var nine []string
	for i := 3; i <= 7; i++ {
		nine = append(nine, mk(9, i))
	}
	rotateUpgradeBackups(bdir, nine[4])
	var left []string
	ents, _ := os.ReadDir(bdir)
	for _, e := range ents {
		left = append(left, e.Name())
	}
	want := []string{old8b, nine[2], nine[3], nine[4]}
	sort.Strings(want)
	if strings.Join(left, ",") != strings.Join(want, ",") {
		t.Fatalf("left %v\nwant %v (removed %s)", left, want, old8a)
	}
}

func TestUpgradeBackupName(t *testing.T) {
	bdir := t.TempDir()
	now := time.Date(2026, 10, 27, 15, 30, 12, 0, time.Local)
	n := upgradeBackupName(bdir, 9, "3.2.0", now)
	if n != "lectureflow-before-upgrade-schema9-to-3.2.0-20261027-153012.db" {
		t.Fatal(n)
	}
	os.WriteFile(filepath.Join(bdir, n), nil, 0o600)
	if n2 := upgradeBackupName(bdir, 9, "3.2.0", now); n2 != "lectureflow-before-upgrade-schema9-to-3.2.0-20261027-153012-2.db" {
		t.Fatal(n2)
	}
	if n3 := upgradeBackupName(bdir, 4, `..\x/y z`, now); n3 != "lectureflow-before-upgrade-schema4-to-.._x_y_z-20261027-153012.db" {
		t.Fatal(n3)
	}
}

// F3: in desktop mode the long steps before the ready line print
// LECTUREFLOW_STARTING lines (the shell waits while they come); none come
// after the ready line, and web mode prints none.
func TestStartingLinesDuringLongSteps(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	oldFirst := heartbeatFirst
	heartbeatFirst = 0 // the first line at once: no race with a quick step
	t.Cleanup(func() { heartbeatFirst = oldFirst })
	cfgPath, dataDir := writeTestConfig(t)
	buildOldDB(t, filepath.Join(dataDir, "lectureflow.db"), 9)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	p.ready()
	<-p.app
	out := p.stdout()
	ready := strings.Index(out, ReadyPrefix)
	for _, step := range []string{stepUpgradeBackup, stepMigrate, stepSeal} {
		i := strings.Index(out, StartingPrefix+step+"\n")
		if i < 0 || i > ready {
			t.Fatalf("no %q line before the ready line:\n%s", step, out)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(p.stdout()[ready:], StartingPrefix) {
		t.Fatal("a starting line after the ready line")
	}

	secrets.ResetForTest()
	cfg2, dataDir2 := writeTestConfig(t)
	buildOldDB(t, filepath.Join(dataDir2, "lectureflow.db"), 9)
	w := startProc(t, []string{"-config", cfg2, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	w.ready()
	<-w.app
	if strings.Contains(w.stdout(), StartingPrefix) {
		t.Fatal("starting lines in web mode")
	}
}

type lineBuf struct {
	mu    sync.Mutex
	lines []string
}

func (b *lineBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, strings.Split(strings.TrimRight(string(p), "\n"), "\n")...)
	return len(p), nil
}

func (b *lineBuf) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.lines)
}

func TestProgressStep(t *testing.T) {
	oldFirst, oldEvery := heartbeatFirst, heartbeatEvery
	heartbeatFirst, heartbeatEvery = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { heartbeatFirst, heartbeatEvery = oldFirst, oldEvery })
	var b lineBuf
	p := &progress{w: &b}
	end := p.step("quick")
	end()
	end() // twice is fine
	if b.count() != 0 {
		t.Fatalf("a step shorter than the first beat printed %v", b.lines)
	}
	end = p.step("long")
	time.Sleep(150 * time.Millisecond)
	end()
	n := b.count()
	if n < 3 {
		t.Fatalf("%d lines", n)
	}
	for _, l := range b.lines {
		if l != StartingPrefix+"long" {
			t.Fatalf("line %q", l)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if b.count() != n {
		t.Fatal("a line after the step ended")
	}
	var nilProg *progress
	nilProg.step("x")() // nil-safe
}

func TestSlowTimeoutGrowsWithSize(t *testing.T) {
	if slowTimeout(0) < 10*time.Minute || slowTimeout(3<<30) < slowTimeout(0)+50*time.Minute {
		t.Fatalf("%v %v", slowTimeout(0), slowTimeout(3<<30))
	}
	if !isDiskFull(errors.New("database or disk is full (13)")) || isDiskFull(errors.New("database is locked (5)")) {
		t.Fatal("isDiskFull")
	}
	if humanBytes(3<<30+(1<<29)) != "3.5 GB" || humanBytes(5<<20) != "5 MB" || humanBytes(1) != "1 MB" {
		t.Fatal(humanBytes(3<<30+(1<<29)), humanBytes(5<<20))
	}
}

// N33: a schema-14 database (as 3.2.0 left it) started with this version:
// one backup without any credential is made first (schema 14, its rows),
// then it is migrated past 14 (migration 015, the schema marker) and its
// credentials are sealed in the bound format 3.2.0 does not know; its rows
// are kept.
func TestUpgradeFrom14BacksUpThenMarks(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, dataDir := writeTestConfig(t)
	path := filepath.Join(dataDir, "lectureflow.db")
	buildOldDB(t, path, 14)
	if fi := schemaOf(t, path); fi.Version != 14 || !fi.HasSecrets {
		t.Fatalf("built %+v", fi)
	}

	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-app-version", "3.2.1"}, "", AppOptions{})
	p.ready()
	<-p.app
	baks := upgradeBackups(t, dataDir)
	if len(baks) != 1 || !strings.HasPrefix(baks[0], upgradeBackupPrefix+"schema14-to-3.2.1-") {
		t.Fatalf("backups %v", baks)
	}
	bak := filepath.Join(dataDir, "backups", baks[0])
	for _, needle := range []string{upgKey, upgRefresh, "enc:v1:", secrets.PrefixV2} {
		if fileHolds(t, bak, needle) {
			t.Fatalf("the backup holds %q", needle)
		}
	}
	if bi := schemaOf(t, bak); bi.Version != 14 || bi.HasSecrets {
		t.Fatalf("backup %+v", bi)
	}
	p.sigc <- os.Interrupt
	if c := p.wait(15 * time.Second); c != exitOK {
		t.Fatalf("exit %d", c)
	}
	if mi := schemaOf(t, path); mi.Version < 15 || mi.Version != db.SchemaVersion() {
		t.Fatalf("main %+v", mi)
	}
	if fileHolds(t, path, upgKey) || !fileHolds(t, path, secrets.PrefixV2) {
		t.Fatal("the credentials are not sealed in the bound format")
	}
	for _, f := range []string{path, bak} {
		d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(f)}).EscapedPath()+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var name string
		var providers int
		d.QueryRow(`SELECT name FROM course WHERE id = 'c1'`).Scan(&name)
		d.QueryRow(`SELECT COUNT(*) FROM provider`).Scan(&providers)
		d.Close()
		if name != "CSC3631" || providers != 1 {
			t.Fatalf("%s: course %q, %d providers", filepath.Base(f), name, providers)
		}
	}
}
