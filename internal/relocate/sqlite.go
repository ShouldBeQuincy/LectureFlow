// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"

	"modernc.org/sqlite"

	"lectureflow/internal/db"
)

// DBName is the database file of a data directory.
const DBName = "lectureflow.db"

// dbSidecars are the files SQLite keeps next to the database.
var dbSidecars = []string{"-wal", "-shm", "-journal"}

// BeforeRollbackName is the database as it was when a rollback of an
// in-app update put the update's snapshot back: the desktop shell copies
// the database files to this name (with -wal and -shm after it) in the
// data directory and keeps one generation
// (desktop/src/main/selfupdate.js _restoreDb, DB_BEFORE_ROLLBACK_NAME).
// LectureFlow never opens it; it holds the credentials the database held,
// so the one a move leaves in the old directory is scrubbed with the old
// database (ScrubOldDataDir). A move copies it like any other file.
const BeforeRollbackName = DBName + ".lf-before-rollback"

// dsn builds the connection string of path; readOnly opens it with
// mode=ro (the source may be in use by another LectureFlow instance).
func dsn(path string, readOnly bool) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	s := "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?_pragma=busy_timeout(5000)"
	if readOnly {
		s += "&mode=ro"
	}
	return s, nil
}

// openDB opens path with a single connection; foreign keys stay off so the
// rewrite of a key (never expected: keys are ids) cannot trip them.
func openDB(path string, readOnly bool) (*sql.DB, error) {
	s, err := dsn(path, readOnly)
	if err != nil {
		return nil, err
	}
	d, err := sql.Open("sqlite", s)
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

var (
	supportedOnce sync.Once
	supportedVer  int
	supportedErr  error
)

// SupportedVersion is the newest schema version (PRAGMA user_version) this
// program knows: the number of its last migration, found by migrating an
// empty in-memory database.
func SupportedVersion() (int, error) {
	supportedOnce.Do(func() {
		d, err := sql.Open("sqlite", "file:relocate-version?mode=memory")
		if err != nil {
			supportedErr = err
			return
		}
		defer d.Close()
		d.SetMaxOpenConns(1)
		ctx := context.Background()
		if err := db.Migrate(ctx, d); err != nil {
			supportedErr = err
			return
		}
		supportedErr = d.QueryRowContext(ctx, "PRAGMA user_version").Scan(&supportedVer)
	})
	return supportedVer, supportedErr
}

// tableCounts returns the row count of every table.
func tableCounts(ctx context.Context, q interface {
	queryer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (map[string]int64, error) {
	tables, err := userTables(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		if err := q.QueryRowContext(ctx, "SELECT count(*) FROM "+quoteIdent(t)).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

// dbBytes is the size of the database as SQLite sees it (pages × page size).
func dbBytes(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (int64, error) {
	var pages, size int64
	if err := q.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := q.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return 0, err
	}
	return pages * size, nil
}

// source is an open connection on the database being copied. It stays open
// for the whole copy so changed() can tell whether another connection
// committed after the snapshot (PRAGMA data_version).
type source struct {
	db      *sql.DB
	conn    *sql.Conn
	version int64 // data_version before the snapshot
	counts  map[string]int64
	bytes   int64
}

func openSource(ctx context.Context, path string) (*source, error) {
	d, err := openDB(path, true)
	if err != nil {
		return nil, err
	}
	c, err := d.Conn(ctx)
	if err != nil {
		d.Close()
		return nil, err
	}
	s := &source{db: d, conn: c}
	if err := c.QueryRowContext(ctx, "PRAGMA data_version").Scan(&s.version); err != nil {
		s.close()
		return nil, err
	}
	if s.bytes, err = dbBytes(ctx, c); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *source) close() {
	if s.conn != nil {
		s.conn.Close()
	}
	s.db.Close()
}

// changed reports whether another connection committed to the source since
// openSource (then the snapshot may miss those changes).
func (s *source) changed(ctx context.Context) (bool, error) {
	var v int64
	if err := s.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v); err != nil {
		return false, err
	}
	return v != s.version, nil
}

type backuper interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

// snapshot copies the source database to dst with the SQLite online backup
// API inside one read transaction, so the copy and the row counts recorded
// in s.counts are the same consistent state, even while the source is in
// use. step reports pages copied as bytes.
func (s *source) snapshot(ctx context.Context, dst string, step func(bytes int64)) error {
	if _, err := s.conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			s.conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	counts, err := tableCounts(ctx, s.conn)
	if err != nil {
		return err
	}
	s.counts = counts
	var pageSize int64
	if err := s.conn.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return err
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	err = s.conn.Raw(func(dc any) error {
		bk, ok := dc.(backuper)
		if !ok {
			return errors.New("sqlite driver has no backup API")
		}
		b, err := bk.NewBackup(absDst)
		if err != nil {
			return err
		}
		done := 0
		for {
			if err := ctx.Err(); err != nil {
				b.Finish()
				return err
			}
			more, err := b.Step(256)
			if err != nil {
				b.Finish()
				return err
			}
			if n := b.PageCount() - b.Remaining(); n > done && step != nil {
				step(int64(n-done) * pageSize)
				done = n
			}
			if !more {
				break
			}
		}
		return b.Finish()
	})
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if _, err := s.conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// verifyDB checks the copied database at path: integrity, the row count of
// every table against want, and no old prefix left. It returns a coded error.
func verifyDB(ctx context.Context, d *sql.DB, want map[string]int64, r *rewriter) error {
	var res string
	if err := d.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&res); err != nil {
		return verifyErr("integrity", err)
	}
	if res != "ok" {
		return verifyErr("integrity", errors.New(res))
	}
	got, err := tableCounts(ctx, d)
	if err != nil {
		return verifyErr("rows", err)
	}
	if len(got) != len(want) {
		return verifyErr("rows", fmt.Errorf("%d tables, the source has %d", len(got), len(want)))
	}
	for t, n := range want {
		if m, ok := got[t]; !ok || m != n {
			return verifyErr("rows:"+t, fmt.Errorf("table %s: %d rows, the source has %d", t, m, n))
		}
	}
	if afterRewrite != nil {
		afterRewrite(d)
	}
	where, err := findResidual(ctx, d, r)
	if err != nil {
		return verifyErr("old_prefix", err)
	}
	if where != "" {
		return verifyErr("old_prefix:"+where, fmt.Errorf("%s still holds the old data directory", where))
	}
	return nil
}

// afterRewrite is a test hook run on the copy before the residual check.
var afterRewrite func(d *sql.DB)

// finishDB leaves the copy as one self-contained file (journal_mode DELETE;
// the application switches it back to WAL when it opens it).
func finishDB(ctx context.Context, d *sql.DB) error {
	var mode string
	return d.QueryRowContext(ctx, "PRAGMA journal_mode = DELETE").Scan(&mode)
}

// setKV writes kv rows into the copy (after verification).
func setKV(ctx context.Context, d *sql.DB, kv map[string]string) error {
	for k, v := range kv {
		if _, err := d.ExecContext(ctx, "INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", k, v); err != nil {
			return err
		}
	}
	return nil
}
