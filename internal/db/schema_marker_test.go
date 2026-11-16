// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// dumpDB renders the schema (sqlite_master) and every row of every table,
// in a fixed order, for comparing two states of a database.
func dumpDB(t *testing.T, d *sql.DB) string {
	t.Helper()
	var b strings.Builder
	rows, err := d.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var typ, name, q string
		if err := rows.Scan(&typ, &name, &q); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %s: %s\n", typ, name, q)
		if typ == "table" && !strings.HasPrefix(name, "sqlite_") {
			tables = append(tables, name)
		}
	}
	rows.Close()
	for _, tbl := range tables {
		r, err := d.Query(`SELECT * FROM "` + tbl + `" ORDER BY rowid`)
		if err != nil {
			// a WITHOUT ROWID table: its primary key orders it
			if r, err = d.Query(`SELECT * FROM "` + tbl + `" ORDER BY 1`); err != nil {
				t.Fatal(err)
			}
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %v\n", tbl, vals)
		}
		r.Close()
	}
	return b.String()
}

// N33: migration 015 is a schema marker. This version seals credentials in
// the bound format (enc:v2:) that 3.2.0 (schema 14) does not know; a
// schema version above 14 makes 3.2.0 refuse the database
// (NewerSchemaError) instead of misreading the sealed values.
func TestSchemaMarker015(t *testing.T) {
	if _, err := fs.Stat(migrationFS, "migrations/015_schema_marker.sql"); err != nil {
		t.Fatal(err)
	}
	if SchemaVersion() < 15 {
		t.Fatalf("SchemaVersion %d, want at least 15", SchemaVersion())
	}
}

// A schema-14 database (as 3.2.0 left it) upgraded by migration 015: its
// user_version is 15, its tables and rows unchanged.
func TestSchemaMarkerUpgradeFrom14(t *testing.T) {
	ctx := context.Background()
	d := oldDB(t, filepath.Join(t.TempDir(), "v14.db"), 14)
	var v int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 14 {
		t.Fatalf("built version %d", v)
	}
	before := dumpDB(t, d)
	if err := migrateUpTo(ctx, d, 15); err != nil {
		t.Fatal(err)
	}
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 15 {
		t.Fatalf("user_version %d after migration 015", v)
	}
	if after := dumpDB(t, d); after != before {
		t.Fatalf("migration 015 changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if err := Migrate(ctx, d); err != nil { // and on to the newest
		t.Fatal(err)
	}
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != SchemaVersion() {
		t.Fatalf("user_version %d, SchemaVersion %d", v, SchemaVersion())
	}
}

// A program that knows schema 14 (3.2.0) refuses a database migration 015
// was applied to: NewerSchemaError, the file left as it is.
func TestSchemaMarkerRefusedBy14(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v15.db")
	d := oldDB(t, path, 15)
	d.Close()
	before := sha(t, path)

	SchemaVersion() // fill it before replacing it
	saved := schemaVersion
	schemaVersion = 14
	t.Cleanup(func() { schemaVersion = saved })

	_, err := OpenDB(path)
	var ne *NewerSchemaError
	if !errors.As(err, &ne) || ne.Found != 15 || ne.Known != 14 {
		t.Fatalf("error %v", err)
	}
	if sha(t, path) != before {
		t.Fatal("the database file changed")
	}
}
