// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

// Before a database file is opened for use (NOTES "决定（升级前备份与降级保护：
// core-builder，2026-10-27）"): the schema version it carries is read
// without changing it, a database from a newer LectureFlow is refused
// (NewerSchemaError; Migrate refuses it too), and before the migrations of
// an older one run, a copy of it without any credential is written
// (SnapshotWithoutSecrets).

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var (
	schemaOnce    sync.Once
	schemaVersion int
)

// SchemaVersion is the newest schema version (PRAGMA user_version) this
// program knows: the number of its last migration.
func SchemaVersion() int {
	schemaOnce.Do(func() {
		entries, err := fs.ReadDir(migrationFS, "migrations")
		if err != nil {
			panic(err) // embedded: cannot fail
		}
		for _, e := range entries {
			if n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil && n > schemaVersion {
				schemaVersion = n
			}
		}
	})
	return schemaVersion
}

// NewerSchemaError: the database was written by a newer LectureFlow (its
// user_version is above SchemaVersion). Nothing was changed: this program
// cannot know what the newer migrations did, and an older program working
// on such a database loses data (3.1.0 deleted the Colab authorisation it
// could not read and sent sealed keys as API keys: test-upgrade F2).
type NewerSchemaError struct {
	Found, Known int
}

func (e *NewerSchemaError) Error() string {
	return fmt.Sprintf("the database has schema version %d, newer than the %d this program knows: it comes from a newer LectureFlow", e.Found, e.Known)
}

// roDSN is the connection string of a read-only connection to path.
func roDSN(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(5000)", nil
}

// openFile opens a database file that is not the application's (a copy
// being made), without the application's pragmas and without migrating.
func openFile(path string, readOnly bool) (*sql.DB, error) {
	var dsn string
	if readOnly {
		var err error
		if dsn, err = roDSN(path); err != nil {
			return nil, err
		}
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		dsn = "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?_pragma=busy_timeout(5000)"
	}
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// FileInfo describes a database file before it is opened for use.
type FileInfo struct {
	Version int // PRAGMA user_version
	// LiveBytes: the pages in use × the page size, about the size of a
	// VACUUM INTO copy.
	LiveBytes int64
	// HasSecrets: a provider API key or a credential kv row is stored (plain
	// or sealed).
	HasSecrets bool
	// UpgradeBackup is kv KVUpgradeBackup ("" when none).
	UpgradeBackup string
}

// KVUpgradeBackup records the backup made before an upgrade:
// "<target schema version>|<file name in backups/>". The next start that
// finds the same upgrade still to do (a migration failed or was
// interrupted) does not make another one.
const KVUpgradeBackup = "upgrade.backup"

// HeaderVersionHeadroom: how far above SchemaVersion a user_version read
// from a file header is still believed (HeaderUserVersion). A newer
// LectureFlow adds a few migrations per release; a number far beyond is a
// damaged header, not a newer database.
const HeaderVersionHeadroom = 1000

// HeaderUserVersion reads the schema version (user_version) straight from
// the header of the database file at path: the 4 bytes big-endian at offset
// 60 of a file that starts with "SQLite format 3\x00". It parses no schema,
// so it answers for a database whose schema this SQLite cannot read (one
// written by a newer LectureFlow). ok is false when the file cannot be read,
// is not an SQLite database, or its header is damaged: the fixed bytes at
// offsets 18–23 (format versions 1 or 2; payload fractions 64, 32, 32) and
// the page size at offset 16 (a power of two from 512 to 32768, or 1 for
// 65536) must be valid, and the version must lie in 0 to SchemaVersion +
// HeaderVersionHeadroom; otherwise a damaged file whose magic survived
// would be taken for one from a newer version. A version changed in a WAL
// that was not checkpointed yet is not seen here.
func HeaderUserVersion(path string) (version int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var h [100]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return 0, false
	}
	if string(h[:16]) != "SQLite format 3\x00" || !validHeader(h) {
		return 0, false
	}
	v := int(int32(binary.BigEndian.Uint32(h[60:64])))
	if v < 0 || v > SchemaVersion()+HeaderVersionHeadroom {
		return 0, false
	}
	return v, true
}

// validHeader checks the fields of an SQLite file header that have fixed or
// few valid values (https://www.sqlite.org/fileformat.html, 1.3).
func validHeader(h [100]byte) bool {
	if h[18] < 1 || h[18] > 2 || h[19] < 1 || h[19] > 2 {
		return false
	}
	if h[21] != 64 || h[22] != 32 || h[23] != 32 {
		return false
	}
	ps := binary.BigEndian.Uint16(h[16:18])
	return ps == 1 || (ps >= 512 && ps <= 32768 && ps&(ps-1) == 0)
}

// InspectFile reads FileInfo of the database at path on a read-only
// connection: the file is not changed.
func InspectFile(ctx context.Context, path string) (FileInfo, error) {
	var fi FileInfo
	d, err := openFile(path, true)
	if err != nil {
		return fi, err
	}
	defer d.Close()
	c, err := d.Conn(ctx)
	if err != nil {
		return fi, err
	}
	defer c.Close()
	// one read transaction: version, size and rows of the same state
	if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
		return fi, err
	}
	defer c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	if err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&fi.Version); err != nil {
		return fi, err
	}
	var pages, free, size int64
	if err := c.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return fi, err
	}
	if err := c.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return fi, err
	}
	if err := c.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return fi, err
	}
	fi.LiveBytes = (pages - free) * size
	n, err := countSecretRows(ctx, c, SealedKVKeys())
	if err != nil {
		return fi, err
	}
	fi.HasSecrets = n > 0
	if ok, err := hasTable(ctx, c, "kv"); err != nil {
		return fi, err
	} else if ok {
		err := c.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, KVUpgradeBackup).Scan(&fi.UpgradeBackup)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fi, err
		}
	}
	return fi, nil
}

// SetUpgradeBackup writes kv KVUpgradeBackup = value into the database at
// path, opened as it is (not migrated). A database without a kv table is
// left as it is.
func SetUpgradeBackup(ctx context.Context, path, value string) error {
	d, err := openRaw(path)
	if err != nil {
		return err
	}
	defer d.Close()
	c, err := d.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, err := hasTable(ctx, c, "kv"); err != nil || !ok {
		return err
	}
	_, err = c.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, KVUpgradeBackup, value)
	return err
}

// queryer is what the credential queries need (a *sql.Conn or *sql.Tx).
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func hasTable(ctx context.Context, q queryer, table string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
	return n > 0, err
}

// countSecretRows counts what scrubSecretRows removes: providers with an
// API key, the kv rows kvKeys and every kv row holding a sealed value.
func countSecretRows(ctx context.Context, q queryer, kvKeys []string) (int, error) {
	total := 0
	if ok, err := hasTable(ctx, q, "provider"); err != nil {
		return 0, err
	} else if ok {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM provider WHERE api_key <> ''`).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	if ok, err := hasTable(ctx, q, "kv"); err != nil {
		return 0, err
	} else if ok {
		var n int
		// sealed by its owner, either format (secrets.Prefixes, sec3 L3)
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM kv WHERE `+sealedPrefixSQL("value"), sealedPrefixArgs()...).Scan(&n); err != nil {
			return 0, err
		}
		total += n
		for _, k := range kvKeys {
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM kv WHERE key = ? AND NOT `+sealedPrefixSQL("value"), append([]any{k}, sealedPrefixArgs()...)...).Scan(&n); err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}

// scrubSecretRows deletes the kv rows kvKeys and every kv row holding a
// sealed value, and empties provider.api_key. Missing tables are skipped.
// (ScrubSecrets runs it on the secure path.)
func scrubSecretRows(ctx context.Context, q queryer, kvKeys []string) error {
	if ok, err := hasTable(ctx, q, "kv"); err != nil {
		return err
	} else if ok {
		for _, k := range kvKeys {
			if _, err := q.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, k); err != nil {
				return err
			}
		}
		// any row its owner sealed (either format, secrets.Prefixes; e.g.
		// ssh-mvp's remote.ssh.key.*) is a credential too
		if _, err := q.ExecContext(ctx, `DELETE FROM kv WHERE `+sealedPrefixSQL("value"), sealedPrefixArgs()...); err != nil {
			return err
		}
	}
	if ok, err := hasTable(ctx, q, "provider"); err != nil {
		return err
	} else if ok {
		if _, err := q.ExecContext(ctx, `UPDATE provider SET api_key = '' WHERE api_key <> ''`); err != nil {
			return err
		}
	}
	return nil
}

// PartialSuffix ends the names of the unfinished copies SnapshotWithoutSecrets
// writes next to dst (dst+PartialSuffix, dst+PartialSuffix+"2"). They are
// removed when it fails; one a killed process left may still hold the
// credentials and is removed by the next start (the caller).
const PartialSuffix = ".partial"

// sidecars are the files SQLite may keep next to a database.
var sidecars = []string{"", "-wal", "-shm", "-journal"}

// RemoveDBFiles removes the database file path and its WAL, shared-memory
// and journal files; missing ones are fine.
func RemoveDBFiles(path string) error {
	var first error
	for _, s := range sidecars {
		if err := os.Remove(path + s); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}

// SnapshotWithoutSecrets writes to dst a consistent copy of the database at
// src (read-only: src is not changed, its WAL included) that holds no
// credential: the providers' API keys are emptied, the Colab credential
// and every sealed kv row are deleted (as ScrubSecrets does). The copy is
// built from the live rows only (VACUUM INTO): no free page of src comes
// along. With credentials in src, they are removed from a first copy with
// secure_delete on and the result is copied again (VACUUM INTO), so dst is
// rebuilt from rows that never held them; the first copy is deleted. dst
// appears only once complete (renamed from dst+PartialSuffix).
//
// Why remove rather than keep them sealed: the copy exists to go back to
// an older LectureFlow, and those (3.1.0 and earlier) cannot read sealed
// values: a sealed API key reads as "configured" and is sent as the key
// (HTTP 401, "invalid key"), a sealed Colab credential is deleted. An
// empty key asks for the key again, which is the honest state.
func SnapshotWithoutSecrets(ctx context.Context, src, dst string) error {
	first, second := dst+PartialSuffix, dst+PartialSuffix+"2"
	for _, p := range []string{first, second} {
		if err := RemoveDBFiles(p); err != nil {
			return err
		}
	}
	ok := false
	defer func() {
		if !ok {
			RemoveDBFiles(first)
			RemoveDBFiles(second)
		}
	}()
	if err := vacuumInto(ctx, src, first, true); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	result := first
	scrubbed, err := scrubCopy(ctx, first)
	if err != nil {
		return fmt.Errorf("remove the credentials from the copy: %w", err)
	}
	if scrubbed {
		if err := vacuumInto(ctx, first, second, false); err != nil {
			return fmt.Errorf("copy without the credentials: %w", err)
		}
		if err := RemoveDBFiles(first); err != nil {
			return err
		}
		result = second
	}
	if err := os.Rename(result, dst); err != nil {
		return err
	}
	ok = true
	return nil
}

// vacuumInto writes a vacuumed copy of the database at src to dst.
func vacuumInto(ctx context.Context, src, dst string, readOnly bool) error {
	d, err := openFile(src, readOnly)
	if err != nil {
		return err
	}
	defer d.Close()
	abs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, "VACUUM INTO ?", abs)
	return err
}

// scrubCopy removes the credentials from the copy at path with
// secure_delete on; false when it held none (nothing written).
func scrubCopy(ctx context.Context, path string) (bool, error) {
	d, err := openFile(path, false)
	if err != nil {
		return false, err
	}
	defer d.Close()
	c, err := d.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer c.Close()
	n, err := countSecretRows(ctx, c, SealedKVKeys())
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := c.ExecContext(ctx, "PRAGMA secure_delete = 1"); err != nil {
		return false, err
	}
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	if err := scrubSecretRows(ctx, tx, SealedKVKeys()); err != nil {
		tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ScrubSecretsFile removes every credential from the database file at path
// (not the application's: a database an import replaced, kept in
// backups/) as ScrubSecrets does: secure_delete, VACUUM, TRUNCATE
// checkpoint.
func ScrubSecretsFile(ctx context.Context, path string, kvKeys ...string) error {
	d, err := openFile(path, false)
	if err != nil {
		return err
	}
	if err := ScrubSecrets(ctx, d, kvKeys...); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
