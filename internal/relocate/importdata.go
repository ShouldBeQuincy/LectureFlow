// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/db"
	"lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

// Import of an existing data directory (SPEC-v3 V1.3 "从开发版迁移", V4.2
// step 1): the database and courses/ are copied into
// {data_dir}/import-staging/, rewritten to the current data directory and
// verified; the READY marker is written last. ApplyStaged swaps them in at
// the next start, before the database is opened. models/, backups/, logs/,
// selftest/ and components/ are not taken (models come from components;
// backups and logs belong to the old install).

// ReadyMarker marks a complete, verified staging area.
const ReadyMarker = "READY"

// importDirs are the directories an import copies.
var importDirs = []string{"courses"}

// requiredTables identify a LectureFlow database.
var requiredTables = []string{"course", "lecture", "job", "kv"}

// ---- kv rows of an imported database (final security audit AUD1 L-1) ----
//
// The imported database brings its whole kv table, and some of its rows
// belong to the installation that wrote them, not to the library: kv
// app.old_data_dir made the next start open (and rewrite) the database of
// whatever directory it named, on another computer too (Windows then
// sends the user's credentials there). An import keeps only the rows
// importedKV lists — the library's own data (per course, lecture,
// provider, server), the one-time conversions already applied to it, and
// the user's settings — and drops the rest (dropUnlistedKV). The rows of
// this installation (installKV: its settings, onboarding progress, update
// and download preferences, key bookkeeping, hardware measurements, live
// Colab sessions) are taken over from the current database instead, so
// an import changes none of them (SEC44 low-1: "check for updates: off"
// stays off). droppedKVKeys name the rows neither side keeps. A test keeps
// the three lists covering every kv key the code defines, so a new key
// gets a decision. The keys defined outside internal/model are written
// out (relocate does not import those packages).

// importedKVKeys and importedKVPrefixes are the kv rows an import keeps.
var (
	importedKVKeys = []string{
		// the user's settings
		model.KVSettingsASR, model.KVSettingsGeneral, model.KVUILocale, model.KVUIZhDisplay,
		model.KVHolidaysCN, "asr.mode", model.KVASRRedecode, model.KVGPUPaused,
		model.KVColabEnabled, model.KVColabBatteryPrefer, "colab.settings", "colab.memory_mode",
		// the Colab account (sealed again with this installation's key or
		// asked for again: resealImported), its measured speeds and its
		// fair-use cooldown
		"colab.credential", "colab.rates", "colab.window_cooldown_until",
		// the remote servers' redaction history (remote_server rows)
		"remote.ssh.redact_history",
		// one-time conversions already applied to the imported rows (run
		// again they would convert them a second time)
		model.KVSettingsImported, model.KVSnapshotsBackfill, "v1_1.usage_migrated",
		model.KVCommonWordBackfill, model.KVASRDevicesMigrated, model.KVStaleReasonJSON,
		model.KVOriginalFilenames, "v2.tariff_preset", "v2.usage_cost_backfill", "past_rule_renumber_checked",
	}
	importedKVPrefixes = []string{
		// per course and lecture
		model.KVMaterialSetsPrefix, model.KVJanitorReportPrefix, model.KVJanitorLatestPrefix,
		model.KVJanitorLastRunPrefix, model.KVGlossaryReviewDirtyPrefix, "glossary_review_fallback:",
		model.KVResumePrefix, model.KVChaptersPrefix, model.KVStreamPrefix, model.KVCiteExportPrefix,
		model.KVFilenameProposal, model.KVFilenameSamples, model.KVFilenameUndo,
		"media_creation_time:", "renumbered_auto:", "title_force:", "title_legacy:", "kind_llm:",
		// per provider and role (provider, role_setting, usage_daily rows)
		model.KVProviderModelsPref, model.KVRoleDefaultPrefix, "usage_unpriced:",
		// per remote server and transcription device (remote_server,
		// asr_device rows): the server's key, the user's pause switch and
		// closed prompts
		"remote.ssh.key.", model.KVASRDevicePausedPrefix, model.KVASRDevicePromptDismissedPrefix,
	}
)

// installKVKeys and installKVPrefixes are the kv rows of the installation:
// the staged database gets the current database's rows of them, never the
// imported database's.
var (
	installKVKeys = []string{
		// this computer's hardware and the CPU fallback done on it
		"hardware.last", "asr_devices.cpu_fallback",
		// the download sources of the components (URLs)
		"components.sources",
		// this installation's live Colab sessions (released at the next start)
		"colab.session", "colab.lost",
	}
	installKVPrefixes = []string{
		// the application settings and the onboarding progress
		// (app.old_data_dir among them: the directory this installation moved from)
		"app.", "onboarding.",
		// the data key's bookkeeping, the update settings and sources
		"secrets.", "selfupdate.",
		// memory peaks measured on this computer's devices
		model.KVVRAMObservedPrefix, model.KVMemObservedPrefix,
	}
)

// droppedKVKeys are the kv rows neither database keeps: the backup an
// upgrade of a database made names a file of that database's directory,
// and the upgrade of the imported database must make its own.
var droppedKVKeys = []string{"upgrade.backup"}

// importedKV reports whether an import keeps the imported kv row key.
func importedKV(key string) bool { return kvListed(key, importedKVKeys, importedKVPrefixes) }

// installKV reports whether the kv row key belongs to the installation.
func installKV(key string) bool { return kvListed(key, installKVKeys, installKVPrefixes) }

// kvListed reports whether key is one of keys or starts with one of prefixes.
func kvListed(key string, keys, prefixes []string) bool {
	for _, k := range keys {
		if key == k {
			return true
		}
	}
	for _, p := range prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// dropUnlistedKV deletes the kv rows of the staged copy d that importedKV
// does not keep and returns how many it deleted.
func dropUnlistedKV(ctx context.Context, d *sql.DB) (int, error) {
	var drop []any
	err := scanRows(ctx, d, "SELECT key FROM kv", 1, func(v []any) error {
		if k, ok := textValue(v[0]); !ok || !importedKV(k) {
			drop = append(drop, v[0])
		}
		return nil
	})
	if err != nil || len(drop) == 0 {
		return 0, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, k := range drop {
		q := "DELETE FROM kv WHERE key = ?"
		if k == nil {
			q, k = "DELETE FROM kv WHERE key IS ?", nil
		}
		if _, err := tx.ExecContext(ctx, q, k); err != nil {
			return 0, err
		}
	}
	return len(drop), tx.Commit()
}

// shownPathMax is how many characters of a stored path a refusal shows.
const shownPathMax = 200

// ShownPath is p, a value read from an imported database, as the {path}
// parameter of a refusal (data_import.outside_path, network_path) shows it
// (SEC44c information-1): without control characters (line breaks, ESC),
// format characters (Unicode Cf: the bidi controls — U+202E would show the
// path's end reversed —, U+200B–U+200F including ZWJ and ZWNJ, U+2060–
// U+2064, the BOM, U+061C, the tag characters, U+FFF9–U+FFFB) and the line
// and paragraph separators U+2028 / U+2029 (SEC49 information-1), and cut
// after shownPathMax characters with "…". The page shows it as text; this
// keeps it from stretching the notice or passing for another path. The
// logs never hold it (their texts are fixed).
func ShownPath(p string) string {
	p = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, media.StripBidi(p))
	if utf8.RuneCountInString(p) <= shownPathMax {
		return p
	}
	return string([]rune(p)[:shownPathMax]) + "…"
}

// LocalAbsPath reports whether p is an absolute path on a drive of this
// computer, written the way the app writes one (filepath.Abs, Clean): on
// Windows a drive letter, a colon and a separator (C:\…), elsewhere a
// leading /. Never a UNC or device path (\\server\share\…, \\?\…, \\.\…),
// a URL, a relative or drive-relative path (C:x, \x), one with leading or
// trailing white space or with a ".." component. Nothing is touched on
// disk. The startup scrub of kv app.old_data_dir (cmd) opens nothing else
// (AUD1 L-1).
func LocalAbsPath(p string) bool {
	if p == "" || strings.TrimSpace(p) != p || media.UnsafeStoredPath(p) {
		return false
	}
	if runtime.GOOS == "windows" {
		c := p[0] | 0x20
		if len(p) < 3 || c < 'a' || c > 'z' || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
			return false
		}
	} else if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return false
	}
	for _, c := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if c == ".." {
			return false
		}
	}
	return true
}

// SourceInfo describes an importable data directory.
type SourceInfo struct {
	Dir      string
	Version  int // PRAGMA user_version
	Courses  int
	Lectures int
	Bytes    int64 // database + courses/
}

// CheckImportSource checks that dir is a LectureFlow data directory: it has
// lectureflow.db, the database has the LectureFlow tables, and its schema
// version is not newer than this program's.
func CheckImportSource(ctx context.Context, dir string) (SourceInfo, error) {
	var info SourceInfo
	d, err := cleanAbs(dir)
	if err != nil {
		return info, err
	}
	info.Dir = d
	notLF := func(err error) error {
		return coded(CodeNotLectureFlow, map[string]any{"path": d}, err)
	}
	p := filepath.Join(d, DBName)
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return info, notLF(err)
	}
	db, err := openDB(p, true)
	if err != nil {
		return info, notLF(err)
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&info.Version); err != nil {
		return info, notLF(err)
	}
	tables, err := userTables(ctx, db)
	if err != nil {
		return info, notLF(err)
	}
	have := map[string]bool{}
	for _, t := range tables {
		have[t] = true
	}
	for _, t := range requiredTables {
		if !have[t] {
			return info, notLF(fmt.Errorf("no table %s", t))
		}
	}
	if info.Version < 1 {
		return info, notLF(errors.New("schema version 0"))
	}
	sup, err := SupportedVersion()
	if err != nil {
		return info, err
	}
	if info.Version > sup {
		return info, coded(CodeTooNew, map[string]any{"version": info.Version, "supported": sup}, nil)
	}
	if err := checkImportDB(ctx, db, d, d); err != nil {
		if isCoded(err) {
			return info, err
		}
		return info, notLF(err)
	}
	if info.Courses, info.Lectures, err = libraryCounts(ctx, db); err != nil {
		return info, notLF(err)
	}
	if info.Bytes, err = dbBytes(ctx, db); err != nil {
		return info, notLF(err)
	}
	for _, sub := range importDirs {
		n, err := treeBytes(filepath.Join(d, sub), nil)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return info, coded(CodeCopyFailed, map[string]any{"path": sub}, err)
		}
		info.Bytes += n
	}
	return info, nil
}

func libraryCounts(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (courses, lectures int, err error) {
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM course").Scan(&courses); err != nil {
		return
	}
	err = q.QueryRowContext(ctx, "SELECT count(*) FROM lecture").Scan(&lectures)
	return
}

// CheckFresh: the data directory's database has no course and no lecture
// (a missing database is fresh).
func CheckFresh(ctx context.Context, dataDir string) error {
	p := filepath.Join(dataDir, DBName)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	d, err := openDB(p, true)
	if err != nil {
		return err
	}
	defer d.Close()
	c, l, err := libraryCounts(ctx, d)
	if err != nil {
		return err
	}
	if c > 0 || l > 0 {
		return coded(CodeNotFresh, map[string]any{"courses": c, "lectures": l}, nil)
	}
	return nil
}

// ImportOptions are the inputs of StageImport.
type ImportOptions struct {
	Src     string // the data directory to import (read only)
	DataDir string // the current data directory
	// Fresh checks that the current library is empty; nil = CheckFresh on
	// DataDir's database file.
	Fresh    func(ctx context.Context) error
	Progress func(Progress)
}

// StageImport copies an existing data directory into
// {DataDir}/import-staging/ for ApplyStaged: the database (a read-only
// online backup: the source may be in use by a development instance) and
// courses/, rewritten from Src to DataDir and verified (integrity, row
// counts, file sizes, no old prefix, record ids: checkImportRecords); then
// the kv rows importedKV does not list are dropped, the installation's kv
// rows (installKV) are taken over from the current database and READY is
// written. A failure removes the staging area.
func StageImport(ctx context.Context, o ImportOptions) (err error) {
	info, err := CheckImportSource(ctx, o.Src)
	if err != nil {
		return err
	}
	src := info.Dir
	dataDir, err := cleanAbs(o.DataDir)
	if err != nil {
		return err
	}
	if samePath(src, dataDir) || within(dataDir, src) || within(src, dataDir) {
		return coded(CodeImportSameDir, nil, nil)
	}
	fresh := o.Fresh
	if fresh == nil {
		fresh = func(ctx context.Context) error { return CheckFresh(ctx, dataDir) }
	}
	if err := fresh(ctx); err != nil {
		return err
	}
	staging := filepath.Join(dataDir, StagingDir)
	if err := os.RemoveAll(staging); err != nil {
		return coded(CodeTargetNotWritable, map[string]any{"path": staging}, err)
	}
	var free int64
	if err := checkSpace(dataDir, info.Bytes, &free); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(staging)
		}
	}()
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return coded(CodeTargetNotWritable, map[string]any{"path": staging}, err)
	}
	t := &tracker{}

	var files []fileEntry
	var dirs []string
	var fileBytes int64
	for _, sub := range importDirs {
		root := filepath.Join(src, sub)
		if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		subFiles, ds, n, err := scanTree(root, nil)
		if err != nil {
			return coded(CodeCopyFailed, map[string]any{"path": sub}, err)
		}
		dirs = append(dirs, sub)
		for _, d := range ds {
			dirs = append(dirs, filepath.Join(sub, d))
		}
		for _, f := range subFiles {
			f.rel = filepath.Join(sub, f.rel)
			files = append(files, f)
		}
		fileBytes += n
	}

	s, err := openSource(ctx, filepath.Join(src, DBName))
	if err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	defer s.close()
	pr := &progress{total: fileBytes + s.bytes, fn: o.Progress}
	pr.setPhase(PhaseCopying)
	stagedDB := filepath.Join(staging, DBName)
	if err := s.snapshot(ctx, stagedDB, pr.add); err != nil {
		if ctx.Err() != nil {
			return coded(CodeCancelled, nil, ctx.Err())
		}
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := copyTree(ctx, t, src, staging, dirs, files, pr.add); err != nil {
		return err
	}

	pr.setPhase(PhaseVerifying)
	rw := newRewriter(src, dataDir)
	d, err := openDB(stagedDB, false)
	if err != nil {
		return verifyErr("open", err)
	}
	defer d.Close()
	// The copy is checked again (the source may have changed since
	// CheckImportSource): its schema before anything writes to it (no
	// trigger may run); its records after the rewrite, moved paths allowed
	// (the ids are what relocateStoredPaths builds paths from); after the
	// relocation strictly; and once more after the import's last write.
	check := func(records, moved bool) error {
		err := checkImportSchema(ctx, d, src)
		if err == nil && records {
			err = checkImportRecords(ctx, d, src, dataDir, moved)
		}
		if err != nil && !isCoded(err) {
			return verifyErr("records", err)
		}
		return err
	}
	if err := check(false, false); err != nil {
		return err
	}
	if _, err := rewriteDB(ctx, d, rw); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := verifyDB(ctx, d, s.counts, rw); err != nil {
		return err
	}
	if err := check(true, true); err != nil {
		return err
	}
	if n, err := relocateStoredPaths(ctx, d, dataDir); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	} else if n > 0 {
		slog.Info("relocate: stored paths of a library written in another folder now name the data directory", "count", n)
	}
	if err := check(true, false); err != nil {
		return err
	}
	if err := verifyFiles(staging, files); err != nil {
		return err
	}
	if n, err := dropUnlistedKV(ctx, d); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	} else if n > 0 {
		slog.Info("relocate: kv rows of the imported database that belong to its installation were not taken", "count", n)
	}
	if n, err := dropOverlongTerms(ctx, d); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	} else if n > 0 {
		slog.Info("relocate: imported glossary terms longer than the limit were not taken", "count", n,
			"max_en_chars", importTermEnMaxChars, "max_zh_chars", importTermZhMaxChars)
	}
	if err := resealImported(ctx, d, src); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	kv, err := carriedKV(ctx, dataDir)
	if err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := setKV(ctx, d, kv); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if afterImportWrites != nil {
		afterImportWrites(d)
	}
	if err := check(true, false); err != nil { // after the last write
		return err
	}
	if err := finishDB(ctx, d); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := d.Close(); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := ctx.Err(); err != nil {
		return coded(CodeCancelled, nil, err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339) + "\n" + src + "\n"
	if err := atomicWrite(filepath.Join(staging, ReadyMarker), []byte(stamp)); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": ReadyMarker}, err)
	}
	return nil
}

// afterImportWrites is a test hook run on the staged copy after the
// import's last write, before the last check.
var afterImportWrites func(d *sql.DB)

// The longest English and Chinese of a glossary term an import takes, in
// characters: the same limits as ctxbuild's (TermEnMaxChars,
// TermZhMaxChars; task SA2), which every term of the glossary must keep
// because the glossary goes into every request of the course. Defined
// here until that line lands; then use those constants.
const (
	importTermEnMaxChars = 100
	importTermZhMaxChars = 100
)

// dropOverlongTerms deletes the terms of the staged copy d whose English
// or Chinese is longer than the limits and returns how many: dropped row
// by row rather than refusing the library, since an older library of the
// user's own may hold such terms from before the limits.
func dropOverlongTerms(ctx context.Context, d *sql.DB) (int, error) {
	var drop []any
	err := selectRows(ctx, d, "SELECT rowid, en, zh FROM term", 3, func(v []any) error {
		en, _ := textValue(v[1])
		zh, _ := textValue(v[2])
		// counted without the surrounding white space, as ctxbuild.TermTooLong
		en, zh = strings.TrimSpace(en), strings.TrimSpace(zh)
		if utf8.RuneCountInString(en) > importTermEnMaxChars || utf8.RuneCountInString(zh) > importTermZhMaxChars {
			drop = append(drop, v[0])
		}
		return nil
	})
	if err != nil || len(drop) == 0 {
		return 0, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, id := range drop {
		if _, err := tx.ExecContext(ctx, "DELETE FROM term WHERE rowid = ?", id); err != nil {
			return 0, err
		}
	}
	return len(drop), tx.Commit()
}

// carriedKV reads the kv rows of the installation (installKV) from the
// current database.
func carriedKV(ctx context.Context, dataDir string) (map[string]string, error) {
	out := map[string]string{}
	p := filepath.Join(dataDir, DBName)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	d, err := openDB(p, true)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	rows, err := d.QueryContext(ctx, "SELECT key, value FROM kv")
	if err != nil {
		// A database without kv (never migrated) carries nothing.
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if installKV(k) {
			out[k] = v
		}
	}
	return out, rows.Err()
}

// rename is os.Rename; tests make it fail.
var rename = os.Rename

// ApplyError is returned by ApplyStaged. Restored reports that the data
// directory is back as it was (the start can go on with the old database);
// when false, a step could not be undone and the start must stop.
type ApplyError struct {
	Restored bool
	Err      error
}

func (e *ApplyError) Error() string {
	if e.Restored {
		return "import not applied (data directory restored): " + e.Err.Error()
	}
	return "import not applied and the data directory could not be restored: " + e.Err.Error()
}

func (e *ApplyError) Unwrap() error { return e.Err }

// ApplyStaged runs at startup before the database is opened. With a READY
// staging area it moves the current lectureflow.db (and -wal, -shm,
// -journal) to backups/lectureflow-before-import-<time>.db, the current
// courses/ (if any) to backups/courses-before-import-<time>, moves the
// staged database and courses/ into place and deletes the staging area. A
// failed step undoes the earlier ones (no half-switched state) and the READY
// marker is removed, so the next start does not try again; the error is an
// *ApplyError. A staging area without READY (a copy interrupted earlier) is
// deleted. Without a staging area nothing happens. An older imported
// database is upgraded by the normal migration when it is opened next.
func ApplyStaged(ctx context.Context, dataDir string) error {
	staging := filepath.Join(dataDir, StagingDir)
	if _, err := os.Stat(staging); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(staging, ReadyMarker)); err != nil {
		slog.Info("relocate: removing an incomplete import staging area", "dir", staging)
		if err := os.RemoveAll(staging); err != nil {
			slog.Warn("relocate: remove staging area", "dir", staging, "err", err)
		}
		return nil
	}
	stamp := time.Now().Format("20060102-150405")
	backups := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		return &ApplyError{Restored: true, Err: err}
	}
	type move struct{ from, to string }
	var done []move
	undo := func(cause error) error {
		for i := len(done) - 1; i >= 0; i-- {
			if err := rename(done[i].to, done[i].from); err != nil {
				slog.Error("relocate: undo import step", "from", done[i].to, "to", done[i].from, "err", err)
				return &ApplyError{Restored: false, Err: fmt.Errorf("%w; undo %s: %v", cause, done[i].to, err)}
			}
		}
		os.Remove(filepath.Join(staging, ReadyMarker))
		return &ApplyError{Restored: true, Err: cause}
	}
	step := func(from, to string) error {
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err := rename(from, to); err != nil {
			return err
		}
		done = append(done, move{from, to})
		return nil
	}
	cur := filepath.Join(dataDir, DBName)
	bak := filepath.Join(backups, "lectureflow-before-import-"+stamp+".db")
	for _, sfx := range append([]string{""}, dbSidecars...) {
		if err := step(cur+sfx, bak+sfx); err != nil {
			return undo(err)
		}
	}
	for _, sub := range importDirs {
		if err := step(filepath.Join(dataDir, sub), filepath.Join(backups, sub+"-before-import-"+stamp)); err != nil {
			return undo(err)
		}
	}
	if err := step(filepath.Join(staging, DBName), cur); err != nil {
		return undo(err)
	}
	for _, sub := range importDirs {
		if err := step(filepath.Join(staging, sub), filepath.Join(dataDir, sub)); err != nil {
			return undo(err)
		}
	}
	if err := os.RemoveAll(staging); err != nil {
		slog.Warn("relocate: remove staging area after import", "dir", staging, "err", err)
	}
	slog.Info("relocate: imported data applied", "backup", bak)
	scrubBackup(ctx, bak)
	return nil
}

// secretKVKeys are the kv rows the database an import replaced must not
// keep in backups/ (nor the one a move leaves behind): the Colab credential
// (colab.KVCredential: a refresh_token and client_secret; v3 Gate round 1
// item 9) and the other kv rows the Store keeps sealed (db.SealedKVKeys).
// db.ScrubSecrets also empties the providers' API keys (sec2-app L5).
var secretKVKeys = db.SealedKVKeys()

// scrubBackup removes the credentials (secretKVKeys and the providers' API
// keys) from the replaced database kept at path (with its -wal), leaving no
// copy in free pages or the WAL (db.ScrubSecrets: secure_delete, VACUUM,
// TRUNCATE checkpoint; closing the last connection removes the WAL). A
// failure is logged and the import stands: the swap is complete, and the
// backup stays where it is.
func scrubBackup(ctx context.Context, path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	d, err := openDB(path, false)
	if err != nil {
		slog.Warn("relocate: opening the replaced database to remove its credentials", "path", path, "err", err)
		return
	}
	defer d.Close()
	if err := db.ScrubSecrets(ctx, d, secretKVKeys...); err != nil {
		slog.Warn("relocate: removing the credentials from the replaced database", "path", path, "err", err)
	}
}

// ImportBackupGlob matches, in a data directory, the databases imports
// replaced (ApplyStaged; with -wal / -shm / -journal beside them).
const ImportBackupGlob = "lectureflow-before-import-*.db"

// ScrubImportBackups removes the credentials from every database an import
// replaced in dataDir/backups (sec3 L1: 3.1 and earlier only removed the
// Colab credential from them, the providers' API keys stayed in the
// clear), the same way as scrubBackup. It returns how many it scrubbed;
// the first failure is returned after trying the others. A missing
// backups/ is nothing to do.
func ScrubImportBackups(ctx context.Context, dataDir string) (int, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "backups", ImportBackupGlob))
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, p := range paths {
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := scrubDB(ctx, p); err != nil {
			if first == nil {
				first = fmt.Errorf("%s: %w", filepath.Base(p), err)
			}
			continue
		}
		n++
	}
	return n, first
}

// scrubDB opens the database at path and removes its credentials
// (db.ScrubSecrets).
func scrubDB(ctx context.Context, path string) error {
	d, err := openDB(path, false)
	if err != nil {
		return err
	}
	if err := db.ScrubSecrets(ctx, d, secretKVKeys...); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// resealImported brings the staged copy's credentials to this
// installation's data key (cred-enc): plain ones (an older install) are
// sealed, ones sealed with the imported directory's key are sealed again
// when that key opens here (secrets.PeekKeyFile: the same account on this
// computer). Credentials no key opens stay as they are: the providers ask
// for their key again, Colab for a new authorisation. Without a data key
// (a failed key setup) nothing changes; the next start seals them.
func resealImported(ctx context.Context, d *sql.DB, src string) error {
	var extra [][]byte
	if k, err := secrets.PeekKeyFile(src); err == nil {
		extra = append(extra, k)
	}
	res, err := db.SealSecrets(ctx, d, extra...)
	if err != nil {
		return err
	}
	if res.Changed() || res.Unreadable > 0 {
		slog.Info("relocate: imported credentials sealed with this installation's key",
			"sealed", res.Sealed, "resealed", res.Resealed, "unreadable", res.Unreadable)
	}
	return nil
}

// ---- record ids and other path parts (final security audit AUD1 M-1) ----
//
// An imported database is someone else's data until it has been checked.
// The app joins record ids into file paths ({data}/courses/{course}/
// lectures/{lecture} and …/materials/{material}: model.CourseDir and the
// others; a job id into the SSH upload's file name) and deletes some of
// those directories, so an id like ..\..\Documents would make it read,
// write or delete outside the data directory. The app itself only writes
// ULIDs (db.NewID: 26 Crockford base-32 characters; the test fixtures use
// C1, L1, M1), so a database holding any other kind of id is refused as a
// whole (data_import.not_lectureflow): nothing is imported halfway. The
// other values that become a part of a path are checked the same way: a
// syllabus' file name (the fallback path beside the syllabus directory)
// and the model and precision of the OpenVINO and MLX devices and asr
// snapshots (models/openvino/whisper-{model}-{precision},
// models/mlx/{model}). A faster-whisper model may be a folder on this
// computer (worker/asr.py loads a directory), but not one on another
// computer (media.UnsafeStoredPath; data_import.network_path, like the
// stored paths of internal/api import_paths.go). CheckImportSource checks
// the source, StageImport the staged copy again (the source may change in
// between).

// importIDRe is the form of a record id an import accepts (with
// recordIDOK): letters, digits, '_' and '-', at most 64. Nothing in it can
// be a separator, a drive, a dot component or a trailing dot or space.
var importIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// codeImportNetworkPath is internal/api's data_import.network_path {count,
// path}: the database names a path on another computer.
const codeImportNetworkPath = "data_import.network_path"

// importIDColumns hold the ids the app joins into paths, and the ids that
// name those records (a lecture's course, a job's scope, a download's
// course and lecture: the app looks the record up by them and builds the
// path from what it finds). Optional columns may be NULL or empty (no
// record). Columns an older schema lacks are skipped. own marks the
// columns that are the record's own key, each value naming one directory
// or file of its own (courses\{course}, …\lectures\{lecture},
// …\materials\{material}, the course's syllabus, ssh-upload-{job}.flac):
// two of them that differ only in case are one directory on a file system
// that ignores case (NTFS, APFS), so removing one record's would remove the
// other's (SEC44c low-1). The app writes upper-case ULIDs only.
var importIDColumns = []struct {
	table, column string
	optional      bool
	own           bool
}{
	{"course", "id", false, true},
	{"lecture", "id", false, true}, {"lecture", "course_id", false, false},
	{"material", "id", false, true}, {"material", "course_id", false, false}, {"material", "lecture_id", true, false},
	{"syllabus", "course_id", false, true},
	{"job", "id", false, true}, {"job", "scope_id", false, false},
	{"download_task", "course_id", false, false}, {"download_task", "lecture_id", true, false},
}

// recordIDOK reports whether s is a record id an import accepts:
// importIDRe and not a Windows device name.
func recordIDOK(s string) bool { return importIDRe.MatchString(s) && !deviceName(s) }

// plainFileName reports whether s can only name a file directly inside
// the directory it is joined to: not empty, "." or "..", no separator, no
// colon (a drive, a stream, a URL scheme), no control character, no
// trailing dot or space (Windows drops them) and not a device name.
func plainFileName(s string) bool {
	if s == "" || strings.TrimRight(s, ". ") != s || deviceName(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || r == ':' {
			return false
		}
	}
	return true
}

// deviceName reports a name Windows takes for a device whatever its
// directory (media.DeviceName: one rule for the import and for the ids
// the app checks before it touches a record's directory).
func deviceName(name string) bool { return media.DeviceName(name) }

// isCoded: err carries an error code (a refusal, not a database failure).
func isCoded(err error) bool {
	var ce *model.CodedError
	return errors.As(err, &ce)
}

// textValue is a column value as the app reads it into a string; ok is
// false for NULL.
func textValue(v any) (s string, ok bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case []byte:
		return string(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), true
	}
	return fmt.Sprint(v), true
}

// scanRows runs query on q and calls fn with the n values of each row.
func scanRows(ctx context.Context, q queryer, query string, n int, fn func(v []any) error) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	v, p := make([]any, n), make([]any, n)
	for i := range v {
		p[i] = &v[i]
	}
	for rows.Next() {
		if err := rows.Scan(p...); err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// selectRows is scanRows for a query naming tables and columns of the
// app's schema directly (unquoted: a missing column is an error, never a
// string literal): a table or column an older schema lacks has nothing
// to check (the rule of internal/api import_paths.go). A refusal fn
// returns is passed on as it is.
func selectRows(ctx context.Context, q queryer, query string, n int, fn func(v []any) error) error {
	err := scanRows(ctx, q, query, n, fn)
	if err != nil && !isCoded(err) {
		if msg := err.Error(); strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column") {
			return nil
		}
	}
	return err
}

// checkImportDB is checkImportSchema, then checkImportRecords on the
// source (moved paths allowed: StageImport relocates them).
func checkImportDB(ctx context.Context, q queryer, dir, root string) error {
	if err := checkImportSchema(ctx, q, dir); err != nil {
		return err
	}
	return checkImportRecords(ctx, q, dir, root, true)
}

// ---- stored file paths (SEC44 medium-2, SEC44b medium-1) ----
//
// lecture.media_path, material.path and syllabus.path name the files the
// stages open (prep's ffmpeg, the PDF and text extraction), and an
// imported library's queued jobs run after the restart: a path to a file
// of the user's (..\..\..\Documents\… relative to the record's directory,
// or any absolute path) would have its content transcribed or extracted
// and sent to the providers the library brought along. What the import
// writes into the staged copy is therefore exactly: empty (no file), or
// an absolute path strictly inside the record's own directory under the
// current data directory (the lecture's, the material's, the course's)
// whose last part is a plain file name — compared as text by
// media.UnderRoot: no ".." component, no relative or drive-relative path
// (the stages would try one against the working directory first, the
// data directory itself in a development run), no long-path or device
// prefix. The last check of StageImport runs on these written values.
//
// The app writes absolute paths inside the record's directory, of the
// data directory the library was in at the time. Imported where it was
// written, the import's rewrite (source directory → current one) makes
// them right. A library copied to another folder or written on another
// computer names that other folder (another drive, user name, system,
// share): such a path that is some folder, then the record's own place
// relative to the data directory, then a plain file name — the place
// worked out from the record's type and ids (courses\{course}\lectures\
// {lecture}, courses\{course}\materials\{material}, courses\{course} or
// courses\{course}\syllabus; never taken from the path) — is the record's
// file: relocateStoredPaths gives it the current data directory as its
// folder (the rewrite's prefix replacement, for whichever folder it was;
// built with model.LectureDir and the others, so the result is in the
// record's directory by construction). Every other path is refused as a
// whole: data_import.network_path for one on another computer (as in
// internal/api import_paths.go), data_import.outside_path {count, path}
// for the rest. A syllabus row with a file name and no path is refused:
// the stage would look the name up in the working directory first, and
// the app always writes the path.

// CodeOutsidePath {count, path}: the imported library has stored file
// paths outside its records' own directories (internal/api maps it to 400).
const CodeOutsidePath = "data_import.outside_path"

// CodeModified {path}: the imported library holds what the app never
// writes — a schema that is not the app's (a view, a trigger, a table or
// column added or changed with a database tool), a record id or file name
// of another form (SEC44b low-1: not "not a LectureFlow data folder").
// internal/api maps it to 400.
const CodeModified = "data_import.modified"

// recordPlace is a directory of a record under the data directory root:
// rel is its location relative to root, as path components.
type recordPlace struct {
	rel []string
	dir string
}

// newPlace is the recordPlace of dir under root.
func newPlace(root, dir string) recordPlace {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return recordPlace{dir: dir}
	}
	return recordPlace{rel: strings.Split(filepath.ToSlash(rel), "/"), dir: dir}
}

// storedPathCols are the stored file paths: query selects the path, the
// course id and the record's key (the update's ? arguments: path, key);
// places are the record's directories under root, the first the one its
// file must lie in.
var storedPathCols = []struct {
	where, query, update string
	places               func(root, course, key string) []recordPlace
}{
	{"lecture.media_path", "SELECT media_path, course_id, id FROM lecture", "UPDATE lecture SET media_path = ? WHERE id = ?",
		func(root, course, id string) []recordPlace {
			return []recordPlace{newPlace(root, model.LectureDir(root, course, id))}
		}},
	{"material.path", "SELECT path, course_id, id FROM material", "UPDATE material SET path = ? WHERE id = ?",
		func(root, course, id string) []recordPlace {
			return []recordPlace{newPlace(root, model.MaterialDir(root, course, id))}
		}},
	{"syllabus.path", "SELECT path, course_id, course_id FROM syllabus", "UPDATE syllabus SET path = ? WHERE course_id = ?",
		func(root, course, _ string) []recordPlace {
			return []recordPlace{newPlace(root, model.CourseDir(root, course)), newPlace(root, model.SyllabusDir(root, course))}
		}},
}

// storedPathOK reports whether p, a stored file path of a record whose
// directory is dir, is one the import writes (above).
func storedPathOK(p, dir string) bool {
	if p == "" {
		return true
	}
	q := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(q, `\\?\`) || strings.HasPrefix(q, `\??\`) || strings.HasPrefix(q, `\\.\`) {
		return false
	}
	return plainFileName(q[strings.LastIndexByte(q, '\\')+1:]) && media.UnderRoot(p, dir) && !media.UnderRoot(dir, p)
}

// movedPath returns, when p is some folder followed by one of places' rel
// and a plain file name (above), the same file in that place's directory.
func movedPath(p string, places []recordPlace) (string, bool) {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) < 2 {
		return "", false
	}
	file := parts[len(parts)-1]
	if !plainFileName(file) {
		return "", false
	}
	for _, pl := range places {
		n := len(pl.rel)
		if n == 0 || len(parts) < n+1 || !slices.Equal(parts[len(parts)-1-n:len(parts)-1], pl.rel) {
			continue
		}
		return filepath.Join(pl.dir, file), true
	}
	return "", false
}

// relocateStoredPaths gives the stored paths of the staged copy d that are
// not yet inside their record's directory under root but name the
// record's own place in another folder (movedPath) the current data
// directory root as their folder, and returns how many it changed. The
// ids are checked before (checkImportRecords).
func relocateStoredPaths(ctx context.Context, d *sql.DB, root string) (int, error) {
	type change struct{ update, path, key string }
	var changes []change
	for _, c := range storedPathCols {
		err := selectRows(ctx, d, c.query, 3, func(v []any) error {
			p, _ := textValue(v[0])
			course, _ := textValue(v[1])
			key, _ := textValue(v[2])
			places := c.places(root, course, key)
			if storedPathOK(p, places[0].dir) {
				return nil
			}
			if np, ok := movedPath(p, places); ok {
				changes = append(changes, change{c.update, np, key})
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	if len(changes) == 0 {
		return 0, nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, ch := range changes {
		if _, err := tx.ExecContext(ctx, ch.update, ch.path, ch.key); err != nil {
			return 0, err
		}
	}
	return len(changes), tx.Commit()
}

// ---- the schema (SEC44 medium-1) ----
//
// The record checks read the app's tables and columns by name, so the
// schema around them must be the app's own: a trigger could change rows
// after the check (on the import's own writes, and later on the app's),
// a view could stand in for a table, a table or column whose name differs
// only in case is the same one to SQLite, and a generated column is not
// stored data. The migrations create none of these (no view, trigger,
// virtual table or generated column in 001–015), so a database with any
// of them, or with a table or column the migrations do not create (names
// compared exactly), is refused as a whole (data_import.modified):
// refusing is simpler to trust than ignoring. So is a table without rowid
// and a column whose default is not the app's (SEC44b information: the
// app names every column it inserts today, but a default is data the
// database could supply later). A database of an older version has fewer
// tables and columns, never others (the migrations only add tables and
// columns; the two rebuilds keep theirs and their defaults). Indexes are
// allowed: they hold no data the app reads.

var (
	appSchemaOnce sync.Once
	appSchemaCols map[string]map[string]xcol
	appSchemaErr  error
)

// appSchema is the tables and columns (exact names, with their defaults)
// of the app's schema, from an empty in-memory database the migrations
// bring to SupportedVersion.
func appSchema() (map[string]map[string]xcol, error) {
	appSchemaOnce.Do(func() {
		d, err := sql.Open("sqlite", "file:relocate-schema?mode=memory")
		if err != nil {
			appSchemaErr = err
			return
		}
		defer d.Close()
		d.SetMaxOpenConns(1)
		ctx := context.Background()
		if err := db.Migrate(ctx, d); err != nil {
			appSchemaErr = err
			return
		}
		tables, err := userTables(ctx, d)
		if err != nil {
			appSchemaErr = err
			return
		}
		out := map[string]map[string]xcol{}
		for _, t := range tables {
			cols, err := tableXInfo(ctx, d, t)
			if err != nil {
				appSchemaErr = err
				return
			}
			out[t] = map[string]xcol{}
			for _, c := range cols {
				out[t][c.name] = c
			}
		}
		appSchemaCols = out
	})
	return appSchemaCols, appSchemaErr
}

// xcol is a column of pragma_table_xinfo: hidden is 0 for a plain column,
// 1 for a virtual table's hidden one, 2 and 3 for generated ones; dflt is
// its default as written in the schema.
type xcol struct {
	name   string
	hidden int64
	dflt   sql.NullString
}

// tableXInfo lists the columns of table t, generated and hidden ones too.
func tableXInfo(ctx context.Context, q queryer, t string) ([]xcol, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, hidden, dflt_value FROM pragma_table_xinfo(?)", t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []xcol
	for rows.Next() {
		var c xcol
		if err := rows.Scan(&c.name, &c.hidden, &c.dflt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// withoutRowid reports whether table t of q's main schema is a WITHOUT
// ROWID table.
func withoutRowid(ctx context.Context, q queryer, t string) (bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT wr FROM pragma_table_list WHERE schema = 'main' AND name = ?", t)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	wr := false
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return false, err
		}
		wr = wr || v != 0
	}
	return wr, rows.Err()
}

// checkImportSchema refuses (data_import.modified {path: dir}) the
// database q when its schema is not the app's (see above).
func checkImportSchema(ctx context.Context, q queryer, dir string) error {
	ref, err := appSchema()
	if err != nil {
		return err
	}
	refuse := func(format string, args ...any) error {
		return coded(CodeModified, map[string]any{"path": dir}, fmt.Errorf("import: "+format, args...))
	}
	var tables []string
	err = scanRows(ctx, q, "SELECT type, name, sql FROM sqlite_master", 3, func(v []any) error {
		typ, _ := textValue(v[0])
		name, _ := textValue(v[1])
		text, _ := textValue(v[2])
		switch typ {
		case "index":
			return nil
		case "table":
			if strings.HasPrefix(name, "sqlite_") { // sqlite_sequence, sqlite_stat1: SQLite's own
				return nil
			}
			if _, ok := ref[name]; !ok {
				return refuse("table %q is not one of LectureFlow's", name)
			}
			if f := strings.Fields(strings.ToUpper(text)); len(f) > 1 && f[1] == "VIRTUAL" {
				return refuse("table %q is a virtual table", name)
			}
			tables = append(tables, name)
			return nil
		}
		return refuse("the database has a %s (%q); LectureFlow creates none", typ, name)
	})
	if err != nil {
		return err
	}
	for _, t := range tables {
		wr, err := withoutRowid(ctx, q, t)
		if err != nil {
			return err
		}
		if wr {
			return refuse("table %q has no rowid", t)
		}
		cols, err := tableXInfo(ctx, q, t)
		if err != nil {
			return err
		}
		for _, c := range cols {
			if c.hidden != 0 {
				return refuse("column %s.%s is generated or hidden", t, c.name)
			}
			rc, ok := ref[t][c.name]
			if !ok {
				return refuse("column %s.%s is not one of LectureFlow's", t, c.name)
			}
			if c.dflt != rc.dflt {
				return refuse("column %s.%s has another default", t, c.name)
			}
		}
	}
	return nil
}

// checkImportRecords checks the database q of the data directory dir (the
// path the refusals name) as described above; the stored file paths must
// lie under root (dir itself for the source, the current data directory
// for the staged copy), or, when moved (the source, before
// relocateStoredPaths), name their record's own place in another folder.
// checkImportSchema has passed. A refusal is a coded error; any other
// error is a failure to read the database.
func checkImportRecords(ctx context.Context, q queryer, dir, root string, moved bool) error {
	refuse := func(format string, args ...any) error {
		return coded(CodeModified, map[string]any{"path": dir}, fmt.Errorf("import: "+format, args...))
	}
	network, first := 0, ""
	foreign := func(p string) bool {
		if !media.ForeignStoredPath(p, root) {
			return false
		}
		network++
		if first == "" {
			first = p
		}
		return true
	}
	for _, c := range importIDColumns {
		err := selectRows(ctx, q, "SELECT "+c.column+" FROM "+c.table, 1, func(v []any) error {
			s, ok := textValue(v[0])
			if c.optional && (!ok || s == "") {
				return nil
			}
			if !ok || !recordIDOK(s) {
				return refuse("%s.%s %q is not a record id LectureFlow writes", c.table, c.column, s)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !c.own {
			continue
		}
		// The ids are checked above (ASCII letters, digits, '_', '-'), so
		// SQLite's lower() folds every case a file system ignores.
		err = selectRows(ctx, q, "SELECT lower("+c.column+") FROM "+c.table+" GROUP BY lower("+c.column+") HAVING count(*) > 1 LIMIT 1", 1,
			func(v []any) error {
				s, _ := textValue(v[0])
				return refuse("%s.%s: two records whose ids differ only in case (%q) would share a directory", c.table, c.column, s)
			})
		if err != nil {
			return err
		}
	}
	err := selectRows(ctx, q, "SELECT filename FROM syllabus", 1, func(v []any) error {
		if s, ok := textValue(v[0]); ok && s != "" && !plainFileName(s) {
			return refuse("syllabus.filename %q is not a plain file name", s)
		}
		return nil
	})
	if err != nil {
		return err
	}

	err = selectRows(ctx, q, "SELECT path, filename FROM syllabus", 2, func(v []any) error {
		p, _ := textValue(v[0])
		if name, _ := textValue(v[1]); p == "" && name != "" {
			return refuse("syllabus.filename %q without a path", name)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// The stored file paths, each against its record's directory (the ids
	// are checked above, so the directory is one of root's); on the source
	// (moved) a path StageImport relocates (movedPath) is fine too. Paths
	// on other computers are reported first (data_import.network_path, the
	// message that says what to do), the paths elsewhere after them.
	outside, firstOutside := 0, ""
	for _, c := range storedPathCols {
		err := selectRows(ctx, q, c.query, 3, func(v []any) error {
			p, _ := textValue(v[0])
			course, _ := textValue(v[1])
			key, _ := textValue(v[2])
			places := c.places(root, course, key)
			if storedPathOK(p, places[0].dir) {
				return nil
			}
			if moved {
				if _, ok := movedPath(p, places); ok {
					return nil
				}
			}
			if !foreign(p) {
				outside++
				if firstOutside == "" {
					firstOutside = p
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	// The transcription model of the devices, the asr snapshots and the
	// legacy ASR settings (converted to a device row once).
	checkModel := func(where, backend, mdl, precision string) error {
		if backend == model.BackendOpenVINO || backend == model.BackendMLX {
			if !recordIDOK(mdl) || (precision != "" && !recordIDOK(precision)) {
				return refuse("%s: model %q, precision %q is not a model LectureFlow writes", where, mdl, precision)
			}
		}
		if media.UnsafeStoredPath(mdl) {
			network++
			if first == "" {
				first = mdl
			}
		}
		return nil
	}
	err = selectRows(ctx, q, "SELECT backend, model, precision FROM asr_device", 3, func(v []any) error {
		b, _ := textValue(v[0])
		m, _ := textValue(v[1])
		p, _ := textValue(v[2])
		return checkModel("asr_device", b, m, p)
	})
	if err != nil {
		return err
	}
	err = selectRows(ctx, q, "SELECT config_snapshot FROM job WHERE stage = '"+model.StageASR+"'", 1, func(v []any) error {
		raw, _ := textValue(v[0])
		snap, ok := model.ParseASRSnapshot(raw)
		if !ok {
			return nil
		}
		run := model.NormalizeASRSnapshot(snap)
		return checkModel("job.config_snapshot", run.Backend, run.Model, run.Precision)
	})
	if err != nil {
		return err
	}
	err = selectRows(ctx, q, "SELECT value FROM kv WHERE key = '"+model.KVSettingsASR+"'", 1, func(v []any) error {
		raw, _ := textValue(v[0])
		var a struct {
			Model string `json:"model"`
		}
		if json.Unmarshal([]byte(raw), &a) != nil {
			return nil
		}
		return checkModel("kv "+model.KVSettingsASR, "", a.Model, "")
	})
	if err != nil {
		return err
	}
	if network > 0 {
		return coded(codeImportNetworkPath, map[string]any{"count": network, "path": ShownPath(first)},
			errors.New("import: a stored file path or a transcription model is a path on another computer"))
	}
	if outside > 0 {
		return coded(CodeOutsidePath, map[string]any{"count": outside, "path": ShownPath(firstOutside)},
			errors.New("import: stored file paths outside their records' directories"))
	}
	return nil
}
