// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package relocate moves the data directory to another place and imports
// the data directory of a development install (SPEC-v3 V1.3, V4.2 step 1,
// V11 "应用", V12.2). The source data is only ever read: everything is
// copied, the database through SQLite's online backup, and the copy's
// absolute paths are rewritten to the new place and verified before
// anything switches over.
package relocate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"lectureflow/internal/components"
	"lectureflow/internal/dirperm"
)

// caseInsensitive: Windows paths compare without ASCII case.
var caseInsensitive = runtime.GOOS == "windows"

// KVOldDataDir is the kv key (in the new database) holding the directory
// the data was moved from; the settings page offers to delete it.
const KVOldDataDir = "app.old_data_dir"

// KVOldDataDirScrubbed (in the new database) is the old data directory
// whose database has had its secret rows removed (ScrubOldDataDir): the
// scrub runs once per move. cred-enc renamed the key ("_v2"): a directory
// scrubbed before the providers' API keys were scrubbed too is scrubbed
// once more.
const KVOldDataDirScrubbed = "app.old_data_dir_scrubbed_v2"

// LocationName is the file in the default data directory that points the
// desktop shell at a moved data directory ({"data_dir": "..."}).
const LocationName = "location.json"

// ShellProfileDir is the desktop shell's Chromium profile under the default
// data directory (desktop/src/main/lib/datadir.js): never copied or deleted.
const ShellProfileDir = "electron"

// The browser extension's native messaging files also live under
// {UserConfigDir}/LectureFlow, which is the default data directory
// (internal/nmhost): the manifests and launch.json, and the macOS socket.
// Like the shell profile they belong to the installation, not to the data.
const (
	NMHostDir = "nmhost"
	NMRunDir  = "run"
)

// IsInstallationEntry reports whether a top-level entry of the default data
// directory belongs to the installation (location.json, the shell profile,
// the native messaging files): never copied, deleted, or counted as data.
func IsInstallationEntry(name string) bool {
	for _, n := range []string{LocationName, ShellProfileDir, NMHostDir, NMRunDir} {
		if strings.EqualFold(name, n) {
			return true
		}
	}
	return false
}

// StagingDir is the import staging area inside the data directory.
const StagingDir = "import-staging"

// spaceMargin: the target needs the data size × 1.1 free.
const spaceMargin = 1.1

// Phases of a copy (Progress.Phase).
const (
	PhaseCopying   = "copying"
	PhaseVerifying = "verifying"
)

// Progress is reported while copying (bytes of files and database pages).
type Progress struct {
	Phase       string
	CopiedBytes int64
	TotalBytes  int64
}

// freeSpace is diskFree; tests replace it.
var freeSpace = diskFree

// MoveOptions are the inputs of MoveDataDir.
type MoveOptions struct {
	Src string // the current data directory
	Dst string // the new data directory: missing or empty
	// LocationFile is {default data dir}/location.json, written last.
	LocationFile string
	// FixComponents rewrites the absolute paths inside the copied component
	// environments (internal/components); nil = skipped.
	FixComponents func(oldRoot, newRoot string) error
	// SetKV rows are written into the new database after verification
	// (KVOldDataDir = Src is always written).
	SetKV map[string]string
	// BeforeCommit runs after verification, before location.json is
	// written; an error aborts the move (and cleans up).
	BeforeCommit func() error
	Progress     func(Progress)
}

// MovePlan is the result of CheckMove.
type MovePlan struct {
	Src, Dst   string // absolute, clean
	TotalBytes int64  // files + database
	FreeBytes  int64
}

// rootSkip leaves out what a data directory copy never takes: the live
// database files (copied through the backup API), the import staging area,
// and the desktop shell's own files in the default data directory.
func rootSkip(rel string, dir bool) bool {
	if strings.ContainsRune(rel, filepath.Separator) {
		return false
	}
	name := rel
	if caseInsensitive {
		name = strings.ToLower(name)
	}
	if dir {
		return name == StagingDir || name == ShellProfileDir || name == NMHostDir || name == NMRunDir
	}
	// lectureflow.lock: the running service's instance lock
	// (cmd/lectureflow/instancelock.go), meaningful only where it was taken.
	if name == DBName || name == LocationName || name == LocationName+".tmp" || name == "lectureflow.lock" {
		return true
	}
	for _, s := range dbSidecars {
		if name == DBName+s {
			return true
		}
	}
	return false
}

// targetEmpty reports whether dst is missing or holds nothing but the
// installation's files (IsInstallationEntry) when it is the default data
// directory.
func targetEmpty(dst, locationFile string) (bool, error) {
	ents, err := os.ReadDir(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	isDefault := locationFile != "" && samePath(filepath.Clean(filepath.Dir(locationFile)), dst)
	for _, e := range ents {
		n := e.Name()
		if isDefault && IsInstallationEntry(n) {
			continue
		}
		return false, nil
	}
	return true, nil
}

// cleanAbs validates a user-given directory path.
func cleanAbs(p string) (string, error) {
	t := strings.TrimSpace(p)
	if t == "" || !filepath.IsAbs(t) || strings.ContainsRune(t, 0) {
		return "", coded(CodePathInvalid, map[string]any{"path": p}, nil)
	}
	return filepath.Clean(t), nil
}

// existingAncestor is the nearest existing directory at or above p.
func existingAncestor(p string) string {
	for {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// CodePathTooLong {path_len, max, data_dir}: on Windows the target is so
// long that the component environments' deepest files would exceed the
// path length Windows loads DLLs from (components.MaxPathLen). Defined
// here, next to its only check; the HTTP layer lists it with the other
// data_dir codes.
const CodePathTooLong = "data_dir.path_too_long"

// CheckMove runs the checks of a move without copying anything: the target
// is absolute, not the source, neither inside it nor containing it, missing
// or empty, writable, short enough for the component environments (Windows,
// checkMovePaths), and its volume has the data size × 1.1 free.
func CheckMove(ctx context.Context, src, dst, locationFile string) (MovePlan, error) {
	return checkMove(ctx, src, dst, locationFile, runtime.GOOS == "windows")
}

// checkMovePaths: the longest path the moved data directory needs below
// dst (its separator included) is the longest file of src/components; dst
// plus that must not exceed components.MaxPathLen. Only components/ counts:
// everything else is read by the Go service, which handles long paths.
// Components not installed yet are not counted (a user who transcribes on
// Colab may never install one): installing one later under a too long data
// directory is refused by components.Manager.Plan with
// components.path_too_long, which names the same remedy.
func checkMovePaths(src, dst string) error {
	longest := 0
	root := filepath.Join(src, "components")
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, rerr := filepath.Rel(src, path); rerr == nil {
			longest = max(longest, components.PathLen(rel))
		}
		return nil
	})
	if longest == 0 {
		return nil
	}
	total := components.PathLen(strings.TrimRight(dst, `\/`)) + 1 + longest
	if total <= components.MaxPathLen {
		return nil
	}
	return coded(CodePathTooLong, map[string]any{"path_len": total, "max": components.MaxPathLen, "data_dir": dst},
		fmt.Errorf("the deepest component file would have a %d-character path under %s (at most %d)", total, dst, components.MaxPathLen))
}

func checkMove(ctx context.Context, src, dst, locationFile string, limitPaths bool) (MovePlan, error) {
	var p MovePlan
	var err error
	if p.Src, err = cleanAbs(src); err != nil {
		return p, err
	}
	if p.Dst, err = cleanAbs(dst); err != nil {
		return p, err
	}
	if samePath(p.Src, p.Dst) {
		return p, coded(CodeSameDir, nil, nil)
	}
	if within(p.Dst, p.Src) {
		return p, coded(CodeTargetInside, nil, nil)
	}
	if within(p.Src, p.Dst) {
		return p, coded(CodeTargetContains, nil, nil)
	}
	if fi, err := os.Stat(p.Dst); err == nil && !fi.IsDir() {
		return p, coded(CodePathInvalid, map[string]any{"path": p.Dst}, errors.New("not a directory"))
	}
	empty, err := targetEmpty(p.Dst, locationFile)
	if err != nil {
		return p, coded(CodeTargetNotWritable, map[string]any{"path": p.Dst}, err)
	}
	if !empty {
		return p, coded(CodeTargetNotEmpty, map[string]any{"path": p.Dst}, nil)
	}
	if err := probeWritable(p.Dst); err != nil {
		return p, coded(CodeTargetNotWritable, map[string]any{"path": p.Dst}, err)
	}
	if limitPaths {
		if err := checkMovePaths(p.Src, p.Dst); err != nil {
			return p, err
		}
	}
	files, err := treeBytes(p.Src, rootSkip)
	if err != nil {
		return p, coded(CodeCopyFailed, map[string]any{"path": p.Src}, err)
	}
	dbSize := int64(0)
	if fi, err := os.Stat(filepath.Join(p.Src, DBName)); err == nil {
		dbSize = fi.Size()
		if w, err := os.Stat(filepath.Join(p.Src, DBName+"-wal")); err == nil {
			dbSize += w.Size()
		}
	}
	p.TotalBytes = files + dbSize
	if err := checkSpace(p.Dst, p.TotalBytes, &p.FreeBytes); err != nil {
		return p, err
	}
	return p, ctx.Err()
}

// checkSpace: the volume of dir has need × 1.1 bytes free.
func checkSpace(dir string, need int64, free *int64) error {
	f, err := freeSpace(existingAncestor(dir))
	if err != nil {
		return coded(CodeTargetNotWritable, map[string]any{"path": dir}, err)
	}
	*free = f
	want := int64(float64(need) * spaceMargin)
	if f < want {
		return coded(CodeNoSpace, map[string]any{"need_bytes": want, "free_bytes": f}, nil)
	}
	return nil
}

// probeWritable creates and removes a file in dir (or, when dir does not
// exist yet, in its nearest existing ancestor), leaving nothing behind.
func probeWritable(dir string) error {
	base := existingAncestor(dir)
	f, err := os.CreateTemp(base, ".lectureflow-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// progress counts copied bytes and reports them.
type progress struct {
	phase  atomic.Value
	copied atomic.Int64
	total  int64
	fn     func(Progress)
}

func (p *progress) add(n int64) {
	c := p.copied.Add(n)
	if p.fn != nil {
		ph, _ := p.phase.Load().(string)
		p.fn(Progress{Phase: ph, CopiedBytes: c, TotalBytes: p.total})
	}
}

func (p *progress) setPhase(ph string) {
	p.phase.Store(ph)
	p.add(0)
}

// MoveDataDir copies the data directory Src to Dst and, once the copy is
// verified, points the desktop shell at it by writing LocationFile. Src is
// never changed. Steps: CheckMove; copy every file (components/, backups/,
// logs/ ... included; the database through the backup API from the live
// database, a consistent snapshot); rewrite Src → Dst in the copy's text
// values; FixComponents; verify (integrity_check, every table's row count
// against the snapshot, every file's size, no old prefix left, the source
// database unchanged since the snapshot); SetKV; BeforeCommit; write
// LocationFile atomically. On any failure everything this run created in
// Dst is removed, LocationFile is not written and a coded error returns.
func MoveDataDir(ctx context.Context, o MoveOptions) (err error) {
	if o.LocationFile == "" {
		return coded(CodeLocationFailed, nil, errors.New("no location file"))
	}
	plan, err := CheckMove(ctx, o.Src, o.Dst, o.LocationFile)
	if err != nil {
		return err
	}
	src, dst := plan.Src, plan.Dst
	t := &tracker{}
	// The new data directory admits only this user (sec-update M2: a place
	// like D:\ lets every account of the computer modify what is created
	// there). Set before anything is copied, so everything inherits it; a
	// directory the user chose gets its old permissions back if the move
	// fails.
	var restorePerm func() error
	defer func() {
		if err != nil {
			t.cleanup()
			if restorePerm != nil {
				if rerr := restorePerm(); rerr != nil {
					slog.Warn("relocate: restoring the permissions of the target", "dir", dst, "err", rerr)
				}
			}
		}
	}()
	if fi, serr := os.Stat(dst); serr == nil && fi.IsDir() {
		if restorePerm, err = dirperm.Save(dst); err != nil {
			return coded(CodeTargetNotWritable, map[string]any{"path": dst}, err)
		}
	}
	if err := t.mkdirAll(dst); err != nil {
		return coded(CodeTargetNotWritable, map[string]any{"path": dst}, err)
	}
	if perr := dirperm.Restrict(dst); perr != nil {
		// e.g. a FAT or exFAT drive, which keeps no permissions: the move
		// goes on; the diagnostics then say that other accounts can reach
		// the data
		slog.Warn("relocate: the new data directory could not be made private", "dir", dst, "err", perr)
	}
	files, dirs, fileBytes, err := scanTree(src, rootSkip)
	if err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": src}, err)
	}
	srcDB := filepath.Join(src, DBName)
	s, err := openSource(ctx, srcDB)
	if err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	defer s.close()
	pr := &progress{total: fileBytes + s.bytes, fn: o.Progress}
	pr.setPhase(PhaseCopying)

	dstDB := filepath.Join(dst, DBName)
	t.file(dstDB)
	for _, sfx := range dbSidecars {
		t.file(dstDB + sfx)
	}
	if err := s.snapshot(ctx, dstDB, pr.add); err != nil {
		if ctx.Err() != nil {
			return coded(CodeCancelled, nil, ctx.Err())
		}
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := copyTree(ctx, t, src, dst, dirs, files, pr.add); err != nil {
		return err
	}

	pr.setPhase(PhaseVerifying)
	rw := newRewriter(src, dst)
	d, err := openDB(dstDB, false)
	if err != nil {
		return verifyErr("open", err)
	}
	defer d.Close()
	if _, err := rewriteDB(ctx, d, rw); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if o.FixComponents != nil {
		if err := o.FixComponents(src, dst); err != nil {
			return coded(CodeComponentsFailed, nil, err)
		}
	}
	if err := verifyDB(ctx, d, s.counts, rw); err != nil {
		return err
	}
	if err := verifyFiles(dst, files); err != nil {
		return err
	}
	if ch, err := s.changed(ctx); err != nil {
		return verifyErr("source", err)
	} else if ch {
		return coded(CodeSourceChanged, nil, nil)
	}
	kv := map[string]string{KVOldDataDir: src}
	for k, v := range o.SetKV {
		kv[k] = v
	}
	if err := setKV(ctx, d, kv); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := finishDB(ctx, d); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := d.Close(); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if o.BeforeCommit != nil {
		if err := o.BeforeCommit(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return coded(CodeCancelled, nil, err)
	}
	b, err := json.Marshal(struct {
		DataDir string `json:"data_dir"`
	}{dst})
	if err != nil {
		return coded(CodeLocationFailed, nil, err)
	}
	if err := atomicWrite(o.LocationFile, b); err != nil {
		return coded(CodeLocationFailed, nil, fmt.Errorf("%s: %w", o.LocationFile, err))
	}
	return nil
}

// ScrubOldDataDir removes the credentials (the Colab credential,
// secretKVKeys, and the providers' API keys) from the database a move left
// in the old data directory oldDir, leaving no copy in free pages or the
// WAL (db.ScrubSecrets: secure_delete, VACUUM, TRUNCATE checkpoint;
// closing the connection removes the WAL), and so are the database a
// rollback of an in-app update kept beside it (BeforeRollbackName, r6) and
// the databases imports replaced in the old backups/ (sec3 L1). Everything
// else in oldDir stays until the user deletes it (v3 Gate round 2, lead
// ruling). It runs after the switch, in the service started on the new
// directory
// currentDir: the old service, the only other user of that database, has
// exited by then. Missing databases are nothing to do; oldDir must be
// absolute and must not be currentDir or hold it.
func ScrubOldDataDir(ctx context.Context, oldDir, currentDir string) error {
	old, err := cleanAbs(oldDir)
	if err != nil {
		return err
	}
	if cur, err := filepath.Abs(currentDir); err == nil {
		cur = filepath.Clean(cur)
		if samePath(old, cur) || within(cur, old) {
			return fmt.Errorf("relocate: %s is the current data directory or holds it", old)
		}
	}
	path := filepath.Join(old, DBName)
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case fi.IsDir():
		return fmt.Errorf("relocate: %s is a directory", path)
	default:
		if err := scrubDB(ctx, path); err != nil { // cred-enc: provider keys too (L5)
			return err
		}
	}
	// r6: the database a rollback of an in-app update kept beside it
	// (BeforeRollbackName, with its -wal) held the same credentials. One
	// that is not a regular file is not the shell's: left alone. One that
	// cannot be scrubbed (not a usable database) is reported after the
	// backups below were tried, like one of them.
	var first error
	kept := filepath.Join(old, BeforeRollbackName)
	if fi, err := os.Lstat(kept); err == nil && fi.Mode().IsRegular() {
		if err := scrubDB(ctx, kept); err != nil {
			first = fmt.Errorf("%s: %w", BeforeRollbackName, err)
		}
	}
	// sec3 L1: the databases imports replaced, kept in the old backups/
	if _, err := ScrubImportBackups(ctx, old); err != nil && first == nil {
		first = err
	}
	return first
}
