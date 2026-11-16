// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/db"
	"lectureflow/internal/relocate"
	"lectureflow/internal/secrets"
)

// The stored credentials' data key (cred-enc, user decision 2026-10-27;
// internal/secrets): set up before the application opens the database,
// then the credentials still stored plain are sealed in place.

// errShellKeyUnavailable: the macOS desktop shell sent "unavailable"
// instead of a key: it could not use the Keychain now (access refused,
// locked). There is no data key in this run, and nothing is replaced
// (sec3 L4).
var errShellKeyUnavailable = errors.New("desktop mode: the shell could not use the Keychain now")

// shellUnavailableLine is the line the shell sends then.
const shellUnavailableLine = "unavailable"

// readDataKey reads the data key the macOS desktop shell sends with
// -datakey-stdin: the line after the access token, 64 hex characters
// (32 bytes), or "unavailable" (errShellKeyUnavailable). Like the token it
// must arrive within tokenReadTimeout.
func readDataKey(br *bufio.Reader) ([]byte, error) {
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := br.ReadString('\n')
		ch <- res{line, err}
	}()
	var got res
	select {
	case got = <-ch:
	case <-time.After(tokenReadTimeout):
		return nil, errors.New("desktop mode: no data key on stdin")
	}
	if got.err != nil && !errors.Is(got.err, io.EOF) {
		return nil, fmt.Errorf("desktop mode: read data key: %w", got.err)
	}
	line := strings.TrimSpace(got.line)
	if line == shellUnavailableLine {
		return nil, errShellKeyUnavailable
	}
	key, err := hex.DecodeString(line)
	if err != nil || len(key) != secrets.KeySize {
		// never the line itself in the message
		return nil, fmt.Errorf("desktop mode: the data key on stdin is not %d hex characters", 2*secrets.KeySize)
	}
	return key, nil
}

// kvBackupsScrubbed marks that the databases earlier imports replaced
// (backups/lectureflow-before-import-*.db) have had their credentials
// removed (sec3 L1: 3.1 and earlier left the providers' keys in them). Not
// an "app." key: an import brings a database without it, and the pass
// runs again (it is idempotent).
const kvBackupsScrubbed = "secrets.backups_scrubbed"

// kvBackupsScrubbedFiles records, while that pass is unfinished, the
// backups it has scrubbed: {name: "<size>:<mtime ns>"} (test-upgrade F3: a
// start stopped in the middle of a large backup went through all of them
// again; now it goes on with the ones left). Cleared when the pass ends.
const kvBackupsScrubbedFiles = "secrets.backups_scrubbed_files"

// setupSecrets establishes the data key for dataDir (shellKey: the macOS
// desktop shell's, nil otherwise; shellUnavailable: the shell could not
// use the Keychain now) and seals the credentials of the database that are
// not sealed with it yet (db.SealSecrets on the secure path, which also
// finishes a clean-up an interrupted start left, sec3 I3); then, once, it
// removes the credentials from the databases earlier imports replaced
// (sec3 L1). Problems are logged and never stop the start: without a key
// the stored credentials read as to be entered again, a key entered now is
// stored unsealed with a warning on the settings page, and the
// diagnostics' credentials row shows an error (§0.1: the app keeps
// working, the failure is visible).
//
// The VACUUMs take time with a large database (test-upgrade F3: 20 s for
// 1.5 GB, as much again per import backup of that size): prog reports
// them to the desktop shell, which keeps waiting, and their bounds grow
// with the size (slowTimeout) instead of a fixed two minutes that a large
// database would exceed at every start.
func setupSecrets(ctx context.Context, dataDir string, shellKey []byte, shellUnavailable bool) {
	setupSecretsWith(ctx, dataDir, shellKey, shellUnavailable, nil)
}

// setupSecretsWith is setupSecrets reporting its long steps on prog.
func setupSecretsWith(ctx context.Context, dataDir string, shellKey []byte, shellUnavailable bool, prog *progress) {
	var st secrets.Status
	var err error
	if shellUnavailable {
		st = secrets.SetupUnavailable("access refused or locked")
		err = secrets.ErrUnavailable
	} else {
		st, err = secrets.Setup(dataDir, shellKey)
	}
	switch {
	case st.Unavailable:
		slog.Warn("credentials: the system credential store is not available now (DPAPI failing, or the Keychain refused or locked); saved API keys and the Colab authorisation are not used in this run, the key file is left as it is, and the next start tries again",
			"err", err)
	case err != nil:
		slog.Error("credentials: no data key; API keys and the Colab authorisation entered now are stored without encryption until this is fixed", "err", err)
	case st.Replaced != "":
		slog.Warn("credentials: the data key file cannot be opened on this computer or account (damaged, or the data directory came from another one); a new key was made and the old file kept. Saved API keys and the Colab authorisation have to be entered again, unless a working copy of the original file is put back as secrets.key.unreadable-<anything> and LectureFlow restarted",
			"kept", st.Replaced)
	case st.Created:
		slog.Info("credentials: a new data key was made", "protection", string(st.Protection))
	default:
		slog.Info("credentials: data key loaded", "protection", string(st.Protection))
	}
	if st.PlainKeyFile {
		slog.Warn("credentials: the data key file was not protected by the system (copied from a Mac, or put there); it is protected from now on", "file", st.KeyFile)
	}
	if st.PlainKeptFile != "" {
		slog.Warn("credentials: an earlier key file kept in the data directory is not protected by the system (copied from a Mac, or put there); its key still opens older stored values, which are sealed again with the data key. LectureFlow leaves that file as it is: delete it once nothing needs it",
			"file", st.PlainKeptFile)
	}
	if st.Recovered > 0 {
		slog.Info("credentials: earlier key files open again; their values are sealed with the data key", "count", st.Recovered)
	}
	path := filepath.Join(dataDir, "lectureflow.db")
	fi, err := os.Stat(path)
	if err != nil {
		return // no database yet: nothing stored
	}
	store, err := db.Open(path)
	if err != nil {
		slog.Warn("credentials: opening the database to seal the stored credentials", "err", err)
		return
	}
	defer store.Close()
	if sealer, ok := store.(db.SecretSealer); ok {
		size := fi.Size()
		if wal, err := os.Stat(path + "-wal"); err == nil {
			size += wal.Size()
		}
		sctx, cancel := context.WithTimeout(ctx, slowTimeout(2*size))
		end := prog.step(stepSeal)
		res, err := sealer.SealStoredSecrets(sctx)
		end()
		cancel()
		switch {
		case err != nil:
			slog.Warn("credentials: sealing the stored credentials failed; tried again at the next start", "err", err)
		default:
			if res.Changed() {
				// bound: 3.2.0's values sealed again bound to their rows (sec3 L3)
				slog.Info("credentials: stored credentials sealed with the data key", "sealed", res.Sealed, "resealed", res.Resealed, "bound", res.Bound)
			}
			if res.Scrubbed {
				slog.Info("credentials: finished the clean-up of freed credentials an earlier start left unfinished")
			}
			if res.Unreadable > 0 {
				slog.Warn("credentials: stored credentials that cannot be decrypted here have to be entered again", "count", res.Unreadable)
			}
			recordKeyNotice(ctx, store, st, res.Unreadable)
		}
	}
	scrubImportBackupsOnce(ctx, store, dataDir, prog)
}

// recordKeyNotice keeps, for the diagnostics of later starts, why stored
// credentials cannot be decrypted when this start made a new key file
// (api.KVCredentialKeyNotice, test-upgrade F6): the file was missing, or
// it could not be opened and was kept under another name. Cleared once no
// credential is left that cannot be decrypted (restored, or entered
// again). Only the reason and the kept file's path are written; the data
// key's handling is not touched. A start without the data key (the
// system's credential store not available now, or no key) cannot tell
// which credentials open: it changes nothing (security review SEC3 L1).
func recordKeyNotice(ctx context.Context, store db.Store, st secrets.Status, unreadable int) {
	if st.Unavailable || st.Protection == secrets.ProtectionNone {
		return
	}
	if unreadable == 0 {
		if v, ok, err := store.GetKV(ctx, api.KVCredentialKeyNotice); err == nil && ok && v != "" {
			if err := store.SetKV(ctx, api.KVCredentialKeyNotice, ""); err != nil {
				slog.Warn("credentials: clearing the note about the replaced key file", "err", err)
			}
		}
		return
	}
	var note api.CredentialKeyNotice
	switch {
	case st.Replaced != "":
		note = api.CredentialKeyNotice{Kind: api.KeyNoticeReplaced, Kept: st.Replaced}
	case st.Created && st.KeyFile != "":
		note = api.CredentialKeyNotice{Kind: api.KeyNoticeMissing}
		slog.Warn("credentials: the data key file was missing (deleted, or not copied with the data directory); a new one was made. Stored credentials sealed with the old key stay as they are: putting the old file back as secrets.key.unreadable-<anything> and restarting opens them again",
			"count", unreadable)
	default:
		return // nothing new: an earlier note stays
	}
	b, err := json.Marshal(note)
	if err != nil {
		return
	}
	if err := store.SetKV(ctx, api.KVCredentialKeyNotice, string(b)); err != nil {
		slog.Warn("credentials: recording why stored credentials cannot be decrypted", "err", err)
	}
}

// scrubImportBackupsOnce removes the credentials from the databases earlier
// imports replaced (backups/lectureflow-before-import-*.db) the way
// relocate.ScrubImportBackups does, once per database (kvBackupsScrubbed).
// Each backup scrubbed is recorded (kvBackupsScrubbedFiles), so a pass a
// stop interrupted goes on with the others at the next start; a failure is
// logged and tried again then.
func scrubImportBackupsOnce(ctx context.Context, store db.Store, dataDir string, prog *progress) {
	if v, ok, err := store.GetKV(ctx, kvBackupsScrubbed); err != nil || (ok && v != "") {
		return
	}
	paths, err := filepath.Glob(filepath.Join(dataDir, "backups", relocate.ImportBackupGlob))
	if err != nil {
		return
	}
	done := map[string]string{}
	if v, ok, err := store.GetKV(ctx, kvBackupsScrubbedFiles); err == nil && ok && v != "" {
		_ = json.Unmarshal([]byte(v), &done)
	}
	end := prog.step(stepScrubBackups)
	defer end()
	n := 0
	var first error
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		name := filepath.Base(p)
		if done[name] == fileStamp(fi) {
			continue // scrubbed by an earlier, interrupted pass
		}
		size := fi.Size()
		if wal, err := os.Stat(p + "-wal"); err == nil {
			size += wal.Size()
		}
		fctx, cancel := context.WithTimeout(ctx, slowTimeout(2*size))
		err = db.ScrubSecretsFile(fctx, p, db.SealedKVKeys()...)
		cancel()
		if err != nil {
			if first == nil {
				first = fmt.Errorf("%s: %w", name, err)
			}
			continue
		}
		n++
		if fi, err := os.Lstat(p); err == nil {
			done[name] = fileStamp(fi)
			if b, err := json.Marshal(done); err == nil {
				if err := store.SetKV(ctx, kvBackupsScrubbedFiles, string(b)); err != nil {
					slog.Warn("credentials: recording the scrub of an import backup", "err", err)
				}
			}
		}
	}
	if n > 0 {
		slog.Info("credentials: removed the credentials from earlier import backups", "count", n)
	}
	if first != nil {
		slog.Warn("credentials: removing the credentials from earlier import backups failed; tried again at the next start", "scrubbed", n, "err", first)
		return
	}
	if err := store.SetKV(ctx, kvBackupsScrubbed, "1"); err != nil {
		slog.Warn("credentials: recording the scrub of the import backups", "err", err)
		return
	}
	if len(done) > 0 {
		_ = store.SetKV(ctx, kvBackupsScrubbedFiles, "")
	}
}

// fileStamp identifies a backup file's content: size and modification time.
func fileStamp(fi os.FileInfo) string {
	return strconv.FormatInt(fi.Size(), 10) + ":" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
}
