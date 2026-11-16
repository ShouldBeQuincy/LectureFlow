// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/relocate"
)

// Network paths in an imported data directory (security review 2, M3).
// The database stores absolute file paths; an import rewrites those inside
// the imported directory to the current data directory and keeps the
// others. A path to another computer (a UNC path \\server\share\…,
// //server/share/…, \\?\UNC\…, or a URL; media.UnsafeStoredPath) would be
// opened by the app on its own — state.MediaExists stats a lecture's media
// on every list, prep's ffmpeg reads it — and on Windows that sends the
// user's NTLM credentials to the server. The app never stores such a path
// itself (uploads are copied into the data directory), so the import is
// refused (data_import.network_path {count, path}) rather than cleared:
// nothing is imported halfway, and the message says what to do. Checked
// on the source before the copy and again on the staged copy (the source
// may change in between). A data directory that is itself on a network
// share is fine: its paths lie inside it and are rewritten. "Inside" is
// media.ForeignStoredPath, the rule the app applies to the current data
// directory at run time (state.StoredPathUnsafe), so the staged check
// refuses exactly what the app would not open after the restart.

// importPathColumns are the stored file paths the app opens.
var importPathColumns = []struct{ table, column string }{
	{"lecture", "media_path"},
	{"material", "path"},
	{"syllabus", "path"},
}

// foreignStoredPaths counts the stored paths of the database file dbPath
// that are unsafe and do not lie inside dir (media.ForeignStoredPath);
// first is one of them. Nothing is opened but the database (read only).
func foreignStoredPaths(ctx context.Context, dbPath, dir string) (count int, first string, err error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return 0, "", err
	}
	if a, err := filepath.Abs(dir); err == nil {
		dir = a // lexical: nothing is touched on disk
	}
	d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, "", err
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	for _, c := range importPathColumns {
		rows, err := d.QueryContext(ctx, "SELECT "+c.column+" FROM "+c.table)
		if err != nil {
			if msg := err.Error(); strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column") {
				continue
			}
			return 0, "", err
		}
		for rows.Next() {
			var p sql.NullString
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return 0, "", err
			}
			if p.Valid && media.ForeignStoredPath(p.String, dir) {
				count++
				if first == "" {
					first = p.String
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, "", err
		}
	}
	return count, first, nil
}

// checkImportPaths refuses an import whose database (dbPath) has paths to
// other computers outside dir (data_import.network_path; the path it
// names cleaned and cut: relocate.ShownPath, SEC44c information-1).
func checkImportPaths(ctx context.Context, dbPath, dir string) error {
	n, first, err := foreignStoredPaths(ctx, dbPath, dir)
	if err != nil {
		return err
	}
	if n > 0 {
		return model.Errorf(codeImportNetworkPath, map[string]any{"count": n, "path": relocate.ShownPath(first)},
			errors.New("the database stores paths on other computers"))
	}
	return nil
}

// checkStagedPaths is checkImportPaths on the staged copy in dataDir
// (after the rewrite, so against the data directory); a refusal removes
// the staging area, so the next start applies nothing. Without a staged
// database (a test's fake staging) there is nothing to check.
func checkStagedPaths(ctx context.Context, dataDir string) error {
	staging := filepath.Join(dataDir, relocate.StagingDir)
	staged := filepath.Join(staging, relocate.DBName)
	if _, err := os.Stat(staged); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := checkImportPaths(ctx, staged, dataDir); err != nil {
		os.RemoveAll(staging)
		return err
	}
	return nil
}
