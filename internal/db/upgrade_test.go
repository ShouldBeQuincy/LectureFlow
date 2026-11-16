// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Test values only (never real keys).
const (
	upgFakeKey  = "sk-test-upgrade-backup-7731"
	upgFakeKey2 = "sk-ant-test-upgrade-backup-0420"
	upgSealed   = "enc:v1:dGVzdC1zZWFsZWQtdmFsdWUtdXBncmFkZQ"
)

func TestSchemaVersion(t *testing.T) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	last := entries[len(entries)-1].Name()
	if want, _ := strconv.Atoi(last[:3]); SchemaVersion() != want || want < 14 {
		t.Fatalf("SchemaVersion %d, last migration %s", SchemaVersion(), last)
	}
	d, err := openRaw(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := Migrate(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var v int
	d.QueryRow("PRAGMA user_version").Scan(&v)
	if v != SchemaVersion() {
		t.Fatalf("migrated to %d, SchemaVersion %d", v, SchemaVersion())
	}
}

// oldDB builds a database of schema version upTo at path with a course, two
// providers holding plain API keys, a plain Colab credential and a sealed
// kv row (ssh-mvp style), and returns it open (its WAL still holding the
// last writes).
func oldDB(t *testing.T, path string, upTo int) *sql.DB {
	t.Helper()
	ctx := context.Background()
	d, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrateUpTo(ctx, d, upTo); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO course (id, name, created_at) VALUES ('c1','CSC3631','2026-10-01T00:00:00Z')`,
		`INSERT INTO provider (id, name, type, base_url, api_key, created_at) VALUES ('p1','DeepSeek','openai','https://api.deepseek.com','` + upgFakeKey + `','2026-10-01T00:00:00Z')`,
		`INSERT INTO provider (id, name, type, base_url, api_key, created_at) VALUES ('p2','Anthropic','anthropic','https://api.anthropic.com','` + upgFakeKey2 + `','2026-10-01T00:00:00Z')`,
		`INSERT INTO kv (key, value) VALUES ('colab.credential','` + testCredential(testRefresh) + `')`,
		`INSERT INTO kv (key, value) VALUES ('remote.ssh.key.x','` + upgSealed + `')`,
		`INSERT INTO kv (key, value) VALUES ('ui.locale','zh-CN')`,
	} {
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func sha(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// A database written by a newer LectureFlow is refused and left as it is.
func TestMigrateRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "newer.db")
	d, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	newer := SchemaVersion() + 1
	if _, err := d.Exec(`CREATE TABLE future_table (x TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`PRAGMA user_version = ` + strconv.Itoa(newer)); err != nil {
		t.Fatal(err)
	}
	d.Close()
	before := sha(t, path)

	if _, err := OpenDB(path); err == nil {
		t.Fatal("a newer database was opened")
	} else {
		var ne *NewerSchemaError
		if !errors.As(err, &ne) || ne.Found != newer || ne.Known != SchemaVersion() {
			t.Fatalf("error %v", err)
		}
	}
	info, err := InspectFile(ctx, path)
	if err != nil || info.Version != newer {
		t.Fatalf("inspect %+v %v", info, err)
	}
	if sha(t, path) != before {
		t.Fatal("the database file changed")
	}
}

func TestInspectFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	c := oldDB(t, path, 9)
	info, err := InspectFile(ctx, path) // the WAL still holds the rows
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != 9 || !info.HasSecrets || info.LiveBytes <= 0 || info.UpgradeBackup != "" {
		t.Fatalf("%+v", info)
	}
	if err := SetUpgradeBackup(ctx, path, "14|x.db"); err != nil {
		t.Fatal(err)
	}
	if info, err = InspectFile(ctx, path); err != nil || info.UpgradeBackup != "14|x.db" {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := c.Exec(`UPDATE provider SET api_key = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(`DELETE FROM kv WHERE key IN ('colab.credential', 'remote.ssh.key.x')`); err != nil {
		t.Fatal(err)
	}
	if info, err = InspectFile(ctx, path); err != nil || info.HasSecrets {
		t.Fatalf("no credential left: %+v %v", info, err)
	}
	if _, err := InspectFile(ctx, filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("a missing file inspected")
	}
}

// The backup before an upgrade: a consistent copy of the old database (its
// WAL included), same schema version and rows, without any credential in
// the file; the source is not changed; no unfinished copy is left.
func TestSnapshotWithoutSecrets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "lectureflow.db")
	c := oldDB(t, src, 9)
	// churn: freed copies of the credential in the source's free pages
	for i := 0; i < 3; i++ {
		if _, err := c.Exec(`UPDATE kv SET value = ? WHERE key = 'colab.credential'`, testCredential(testRefresh)+strings.Repeat(" ", i*40)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(`INSERT INTO kv (key, value) VALUES (?, ?)`, "filler."+strconv.Itoa(i), strings.Repeat("y", 2000)); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(dir, "backups", "lectureflow-before-upgrade-schema9-test.db")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SnapshotWithoutSecrets(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{upgFakeKey, upgFakeKey2, testRefresh, testSecret, upgSealed, "enc:v1:"} {
		if where := fileHolds(t, dst, needle); len(where) > 0 {
			t.Fatalf("the backup holds %q in %v", needle, where)
		}
	}
	if len(fileHolds(t, src, upgFakeKey)) == 0 {
		t.Fatal("control: the source holds the key")
	}
	left, _ := filepath.Glob(filepath.Join(dir, "backups", "*"+PartialSuffix+"*"))
	if len(left) > 0 {
		t.Fatalf("unfinished copies left: %v", left)
	}
	b, err := openFile(dst, true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var v, providers, courses, fillers int
	var ok, locale, keys string
	b.QueryRow(`PRAGMA user_version`).Scan(&v)
	b.QueryRow(`PRAGMA integrity_check`).Scan(&ok)
	b.QueryRow(`SELECT count(*) FROM provider`).Scan(&providers)
	b.QueryRow(`SELECT group_concat(api_key, '|') FROM provider`).Scan(&keys)
	b.QueryRow(`SELECT count(*) FROM course`).Scan(&courses)
	b.QueryRow(`SELECT count(*) FROM kv WHERE key LIKE 'filler.%'`).Scan(&fillers)
	b.QueryRow(`SELECT value FROM kv WHERE key = 'ui.locale'`).Scan(&locale)
	if v != 9 || ok != "ok" || providers != 2 || keys != "|" || courses != 1 || fillers != 3 || locale != "zh-CN" {
		t.Fatalf("backup: version %d integrity %s providers %d keys %q courses %d fillers %d locale %q", v, ok, providers, keys, courses, fillers, locale)
	}
	var creds int
	b.QueryRow(`SELECT count(*) FROM kv WHERE key IN ('colab.credential','remote.ssh.key.x')`).Scan(&creds)
	if creds != 0 {
		t.Fatalf("%d credential rows in the backup", creds)
	}
	// the source is unchanged
	var srcKey string
	var srcV int
	c.QueryRow(`SELECT api_key FROM provider WHERE id = 'p1'`).Scan(&srcKey)
	c.QueryRow(`PRAGMA user_version`).Scan(&srcV)
	if srcKey != upgFakeKey || srcV != 9 {
		t.Fatalf("source changed: %q %d", srcKey, srcV)
	}
}

// Without any credential the copy is made once and matches the source.
func TestSnapshotWithoutSecretsNothingToRemove(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "lectureflow.db")
	d, err := openRaw(src)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrateUpTo(ctx, d, 6); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO course (id, name, created_at) VALUES ('c1','CSC3631','2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy.db")
	if err := SnapshotWithoutSecrets(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := openFile(dst, true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var v, n int
	b.QueryRow(`PRAGMA user_version`).Scan(&v)
	b.QueryRow(`SELECT count(*) FROM course`).Scan(&n)
	if v != 6 || n != 1 {
		t.Fatalf("copy: version %d courses %d", v, n)
	}
}

// A bound (enc:v2:, sec3 L3) kv row is a credential too: counted by
// InspectFile and removed from the backup, also when it is the only one.
func TestSnapshotWithoutSecretsBoundRow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "lectureflow.db")
	d, err := openRaw(src)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrateUpTo(ctx, d, 9); err != nil {
		t.Fatal(err)
	}
	const bound = "enc:v2:dGVzdC1ib3VuZC12YWx1ZS11cGdyYWRl" // test value only
	if _, err := d.Exec(`INSERT INTO kv (key, value) VALUES ('remote.ssh.key.y', ?), ('ui.locale', 'zh-CN')`, bound); err != nil {
		t.Fatal(err)
	}
	info, err := InspectFile(ctx, src)
	if err != nil || !info.HasSecrets {
		t.Fatalf("a bound row is a credential: %+v %v", info, err)
	}
	dst := filepath.Join(dir, "copy.db")
	if err := SnapshotWithoutSecrets(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{bound, "enc:v2:"} {
		if where := fileHolds(t, dst, needle); len(where) > 0 {
			t.Fatalf("the backup holds %q in %v", needle, where)
		}
	}
	b, err := openFile(dst, true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var creds int
	var locale string
	b.QueryRow(`SELECT count(*) FROM kv WHERE key = 'remote.ssh.key.y'`).Scan(&creds)
	b.QueryRow(`SELECT value FROM kv WHERE key = 'ui.locale'`).Scan(&locale)
	if creds != 0 || locale != "zh-CN" {
		t.Fatalf("backup: %d credential rows, locale %q", creds, locale)
	}
}

// A failed copy leaves no file behind (the destination directory is missing;
// a cancelled context).
func TestSnapshotWithoutSecretsFailureLeavesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "lectureflow.db")
	oldDB(t, src, 9)
	dst := filepath.Join(dir, "missing-dir", "copy.db")
	if err := SnapshotWithoutSecrets(ctx, src, dst); err == nil {
		t.Fatal("copy into a missing directory succeeded")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	dst2 := filepath.Join(dir, "copy.db")
	if err := SnapshotWithoutSecrets(cctx, src, dst2); err == nil {
		t.Fatal("cancelled copy succeeded")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "lectureflow.db" && !strings.HasPrefix(e.Name(), "lectureflow.db-") && e.Name() != "missing-dir" {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
}

// HeaderUserVersion reads the version of a valid newer database, but a
// damaged header whose magic survived gives no version (R9 suggestion 2):
// it must not be taken for one from a newer LectureFlow.
func TestHeaderUserVersionDamagedHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "newer.db")
	d, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	newer := SchemaVersion() + 3
	if _, err := d.Exec(`PRAGMA user_version = ` + strconv.Itoa(newer)); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if v, ok := HeaderUserVersion(path); !ok || v != newer {
		t.Fatalf("valid newer database: %d %v, want %d", v, ok, newer)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	variant := func(name string, change func(h []byte)) string {
		b := append([]byte(nil), good...)
		change(b)
		p := filepath.Join(dir, name+".db")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	setVersion := func(v uint32) func(h []byte) {
		return func(h []byte) { binary.BigEndian.PutUint32(h[60:64], v) }
	}
	bad := map[string]func(h []byte){
		// bytes 16-99 overwritten: user_version reads as 0x41414141
		"garbage": func(h []byte) {
			for i := 16; i < 100; i++ {
				h[i] = 'A'
			}
		},
		"max-fraction":  func(h []byte) { h[21] = 65 },
		"min-fraction":  func(h []byte) { h[22] = 0 },
		"leaf-fraction": func(h []byte) { h[23] = 33 },
		"write-version": func(h []byte) { h[18] = 7 },
		"read-version":  func(h []byte) { h[19] = 0 },
		"page-size-odd": func(h []byte) { binary.BigEndian.PutUint16(h[16:18], 1000) },
		"page-size-low": func(h []byte) { binary.BigEndian.PutUint16(h[16:18], 256) },
		"page-size-0":   func(h []byte) { binary.BigEndian.PutUint16(h[16:18], 0) },
		"too-new":       setVersion(uint32(SchemaVersion() + HeaderVersionHeadroom + 1)),
		"negative":      setVersion(0xFFFFFFFF),
	}
	for name, change := range bad {
		if v, ok := HeaderUserVersion(variant(name, change)); ok {
			t.Errorf("%s: version %d from a damaged header", name, v)
		}
	}
	// still valid: page size 1 (65536) and the version at the headroom's edge
	edge := SchemaVersion() + HeaderVersionHeadroom
	p := variant("edge", func(h []byte) {
		binary.BigEndian.PutUint16(h[16:18], 1)
		binary.BigEndian.PutUint32(h[60:64], uint32(edge))
	})
	if v, ok := HeaderUserVersion(p); !ok || v != edge {
		t.Fatalf("edge: %d %v, want %d", v, ok, edge)
	}
}
