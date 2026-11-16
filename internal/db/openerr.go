// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"errors"
	"io/fs"
	"syscall"

	"modernc.org/sqlite"
)

// Why the database could not be opened or migrated (test-upgrade F5): the
// start failure names the cause the user can act on instead of SQLite's
// English text (cmd/lectureflow, startup.db_*). Only a classification:
// opening, migrating and the error values themselves are unchanged.
const (
	OpenFailBusy     = "busy"           // SQLITE_BUSY: another connection (another program) holds the database
	OpenFailReadOnly = "readonly"       // SQLITE_READONLY / SQLITE_PERM, or the file system refused the write
	OpenFailNotDB    = "not_a_database" // SQLITE_NOTADB / SQLITE_CORRUPT: damaged, or not a database
	OpenFailCantOpen = "cant_open"      // SQLITE_CANTOPEN: the file (or its -wal / -shm) cannot be opened
	OpenFailFull     = "full"           // SQLITE_FULL: the disk is full
	OpenFailIO       = "io_error"       // SQLITE_IOERR: the disk failed to read or write
)

// SQLite's primary result codes (stable across versions).
const (
	sqlitePerm     = 3
	sqliteBusy     = 5
	sqliteLocked   = 6
	sqliteReadOnly = 8
	sqliteIOErr    = 10
	sqliteCorrupt  = 11
	sqliteFull     = 13
	sqliteCantOpen = 14
	sqliteNotADB   = 26
)

// ClassifyOpenError names the kind of err (one of OpenFail*), "" for an
// error that is none of them. err may wrap a SQLite error (the driver's
// *sqlite.Error) or an error of the file system.
func ClassifyOpenError(err error) string {
	if err == nil {
		return ""
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff { // extended codes keep the primary code in the low byte
		case sqliteBusy:
			return OpenFailBusy
		case sqliteLocked:
			// "database table is locked": a conflict inside one connection
			// (or a shared cache, not used here), so always this program's
			// own fault, never another program's: no code, a plain failure
			// (a freshly installed version that meets it fails its
			// probation; security review SEC12 M-1)
			return ""
		case sqliteReadOnly, sqlitePerm:
			return OpenFailReadOnly
		case sqliteNotADB, sqliteCorrupt:
			return OpenFailNotDB
		case sqliteCantOpen:
			return OpenFailCantOpen
		case sqliteFull:
			return OpenFailFull
		case sqliteIOErr:
			return OpenFailIO
		}
		return ""
	}
	switch {
	case errors.Is(err, fs.ErrPermission):
		return OpenFailReadOnly
	case errors.Is(err, syscall.ENOSPC):
		return OpenFailFull
	}
	return ""
}
