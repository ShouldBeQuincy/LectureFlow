// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

// Secrets in kv that must not outlive their deletion (v3 Gate round 1
// item 9: the Colab credential's refresh_token and client_secret). SQLite
// keeps deleted or replaced content in free pages, in the unused space of
// pages and in old WAL frames; a plain UPDATE or DELETE leaves it readable
// in the file. Here the write runs with PRAGMA secure_delete=ON, then
// VACUUM rebuilds every page from the live rows only, then a TRUNCATE
// checkpoint copies the WAL into the database and cuts the WAL to zero
// bytes. NOTES "决定（v3 gate-fix-core）".

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetSecretKV writes kv key = value, or deletes key when value is "", so
// that the previous value leaves no copy in the database file or its WAL.
// It is not part of the Store interface (frozen); callers reach it with a
// type assertion (colab.SecretKV). It is rare and slow (a VACUUM of the
// whole database), so only secrets go through it. Inside a transaction
// (a Store from Tx) it is refused: VACUUM cannot run there.
func (s *sqlStore) SetSecretKV(ctx context.Context, key, value string) error {
	if s.tx != nil {
		return errors.New("db: SetSecretKV inside a transaction")
	}
	if isSealedKV(key) { // cred-enc: stored sealed (sealed.go)
		var err error
		if value, err = sealKV(key, value); err != nil {
			return err
		}
	}
	return withSecureConn(ctx, s.db, func(ctx context.Context, c *sql.Conn) error {
		var err error
		if value == "" {
			_, err = c.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key)
		} else {
			_, err = c.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
		}
		return err
	})
}

// ScrubKV deletes keys from the kv table of an opened database d the same
// way. A database without a kv table is left as it is. relocate uses
// ScrubSecrets, which also empties the providers' API keys.
func ScrubKV(ctx context.Context, d *sql.DB, keys ...string) error {
	return withSecureConn(ctx, d, func(ctx context.Context, c *sql.Conn) error {
		var n int
		if err := c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'kv'`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		for _, k := range keys {
			if _, err := c.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, k); err != nil {
				return err
			}
		}
		return nil
	})
}

// ScrubSecrets removes every credential from the opened database d the same
// way: the kv rows kvKeys and every kv row holding a sealed value are
// deleted, and provider.api_key is emptied
// (cred-enc, sec2-app L5: the database an import replaced, kept in
// backups/, and the one a move left in the old data directory). Missing
// tables are skipped.
func ScrubSecrets(ctx context.Context, d *sql.DB, kvKeys ...string) error {
	return withSecureConn(ctx, d, func(ctx context.Context, c *sql.Conn) error {
		return scrubSecretRows(ctx, c, kvKeys) // upgrade.go
	})
}

// ScrubFreedSecrets runs the secure path's VACUUM and TRUNCATE checkpoint
// alone: after a transaction deleted or replaced a credential (settings'
// DeleteProvider), no copy of it stays in free pages or the WAL.
func (s *sqlStore) ScrubFreedSecrets(ctx context.Context) error {
	if s.tx != nil {
		return errors.New("db: ScrubFreedSecrets inside a transaction")
	}
	return withSecureConn(ctx, s.db, func(context.Context, *sql.Conn) error { return nil })
}

// SecretScrubber is the optional Store method ScrubFreedSecrets.
type SecretScrubber interface {
	ScrubFreedSecrets(ctx context.Context) error
}

// withSecureConn runs write on one connection of d with secure_delete on
// (restored afterwards), then VACUUM and a TRUNCATE checkpoint.
func withSecureConn(ctx context.Context, d *sql.DB, write func(context.Context, *sql.Conn) error) error {
	compactErr, err := secureWrite(ctx, d, write)
	if err != nil {
		return err
	}
	return compactErr
}

// secureWrite is withSecureConn reporting the failure of write (err:
// nothing or only part was written) apart from the failure of the
// VACUUM or the checkpoint after it (compactErr: the write stands, a copy
// of the old value may remain until the next checkpoint).
func secureWrite(ctx context.Context, d *sql.DB, write func(context.Context, *sql.Conn) error) (compactErr, err error) {
	c, err := d.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var prev int
	if err := c.QueryRowContext(ctx, "PRAGMA secure_delete").Scan(&prev); err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, "PRAGMA secure_delete = 1"); err != nil {
		return nil, err
	}
	defer c.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA secure_delete = %d", prev))
	// sec3 I3: marked before the write, cleared after the checkpoint, so a
	// start after an interruption (or a failed VACUUM) runs the clean-up
	// again (SealSecrets).
	marked := setScrubPending(ctx, c)
	if err := write(ctx, c); err != nil {
		return nil, err
	}
	// VACUUM writes a fresh copy of the live rows: no free page, no unused
	// space of a page can still hold the old value.
	if _, err := c.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("db: vacuum after deleting a secret: %w", err), nil
	}
	if err := checkpointTruncate(ctx, c); err != nil {
		return err, nil
	}
	if marked {
		if _, err := c.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, kvScrubPending); err == nil {
			_ = checkpointTruncate(ctx, c) // the marker's own frame; no secret in it
		}
	}
	return nil, nil
}

// kvScrubPending is set while a secure write's clean-up (VACUUM, TRUNCATE
// checkpoint) has not completed: freed pages or old WAL frames may still
// hold a replaced credential (sec3 I3).
const kvScrubPending = "secrets.scrub_pending"

// KVScrubPending is kvScrubPending for the diagnostics (test-upgrade F9:
// while it is set, the credentials row says the clean-up is pending).
const KVScrubPending = kvScrubPending

// setScrubPending sets kvScrubPending when the database has a kv table.
func setScrubPending(ctx context.Context, c *sql.Conn) bool {
	var n int
	if err := c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'kv'`).Scan(&n); err != nil || n == 0 {
		return false
	}
	_, err := c.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, '1') ON CONFLICT(key) DO UPDATE SET value = excluded.value`, kvScrubPending)
	return err == nil
}

// scrubPending reports whether kvScrubPending is set in d.
func scrubPending(ctx context.Context, d *sql.DB) bool {
	var v string
	err := d.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, kvScrubPending).Scan(&v)
	return err == nil && v != ""
}

// checkpointTruncate copies the WAL into the database and truncates it to
// zero bytes (old frames hold earlier versions of the pages). Another
// connection reading at the same time keeps the WAL busy: tried a few
// times, then an error (the WAL is truncated by the next checkpoint or
// deleted when the last connection closes). Not in WAL mode it does
// nothing.
func checkpointTruncate(ctx context.Context, c *sql.Conn) error {
	for attempt := 0; ; attempt++ {
		var busy, logFrames, done int
		if err := c.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &done); err != nil {
			return err
		}
		if busy == 0 {
			return nil
		}
		if attempt == 4 {
			return errors.New("db: the WAL could not be truncated: another connection is reading the database")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
}
