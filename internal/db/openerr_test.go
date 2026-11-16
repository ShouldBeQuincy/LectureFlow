// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oldDBFile writes a database stopped after migration upTo at a new path
// and closes it.
func oldDBFile(t *testing.T, upTo int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	d, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(context.Background(), d, upTo); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// F5: another program holding the write lock while the migrations run.
func TestClassifyOpenErrorBusy(t *testing.T) {
	path := oldDBFile(t, 3)
	holder, err := openRaw(path)
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
	d, err := OpenDB(path) // waits busy_timeout, then gives up
	if err == nil {
		d.Close()
		t.Fatal("migrated while another connection held the write lock")
	}
	if k := ClassifyOpenError(fmt.Errorf("open db: %w", err)); k != OpenFailBusy {
		t.Fatalf("%v: kind %q", err, k)
	}
}

// F5: a database file marked read-only (Windows: the read-only attribute;
// elsewhere: no write permission). Not verified on macOS.
func TestClassifyOpenErrorReadOnly(t *testing.T) {
	path := oldDBFile(t, 3)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })
	d, err := OpenDB(path)
	if err == nil {
		d.Close()
		t.Fatal("migrated a read-only database")
	}
	if k := ClassifyOpenError(err); k != OpenFailReadOnly {
		t.Fatalf("%v: kind %q", err, k)
	}
}

// F5: a file that is not a database (damaged, or replaced by another file).
func TestClassifyOpenErrorNotADatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	junk := make([]byte, 8192)
	for i := range junk {
		junk[i] = byte(i*7 + 3)
	}
	if err := os.WriteFile(path, junk, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDB(path)
	if err == nil {
		d.Close()
		t.Fatal("opened a file that is not a database")
	}
	if k := ClassifyOpenError(err); k != OpenFailNotDB {
		t.Fatalf("%v: kind %q", err, k)
	}
	if _, err := InspectFile(context.Background(), path); ClassifyOpenError(err) != OpenFailNotDB {
		t.Fatalf("InspectFile: %v", err)
	}
}

func TestClassifyOpenErrorOther(t *testing.T) {
	if k := ClassifyOpenError(nil); k != "" {
		t.Fatalf("nil: %q", k)
	}
	if k := ClassifyOpenError(errors.New("bad migration name x")); k != "" {
		t.Fatalf("plain error: %q", k)
	}
	if k := ClassifyOpenError(&fs.PathError{Op: "open", Path: "x", Err: fs.ErrPermission}); k != OpenFailReadOnly {
		t.Fatalf("permission: %q", k)
	}
}

// SEC12 M-1: "database table is locked" (SQLITE_LOCKED) comes from a
// conflict inside one connection, so it is this program's own fault: not
// "another program holds the database" (busy), which the desktop shell
// treats as outside the version on probation.
func TestClassifyOpenErrorLockedIsNotBusy(t *testing.T) {
	ctx := context.Background()
	d, err := openRaw(filepath.Join(t.TempDir(), "locked.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, "CREATE TABLE a(x); INSERT INTO a VALUES (1), (2), (3)"); err != nil {
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
	// a statement still reading the table while the same connection drops it
	_, err = tx.ExecContext(ctx, "DROP TABLE a")
	if err == nil {
		t.Fatal("DROP TABLE while a query reads it: no error")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("not the table lock: %v", err)
	}
	if k := ClassifyOpenError(fmt.Errorf("migration 015: %w", err)); k != "" {
		t.Fatalf("%v: kind %q, want none (a plain failure)", err, k)
	}
}
