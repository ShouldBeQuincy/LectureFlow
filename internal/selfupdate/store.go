// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// The updates directory, {data_dir}/updates (README.md "updates 目录"):
//
//	<version>/update.json, update.json.minisig, <package>   verified (Go)
//	<version>/<package>.part                                 a download in progress (Go)
//	pending.json   Go → Electron: install this at the next start
//	previous.json  Go (Electron may add "backup"): what a failed update returns to
//	result.json    Electron → Go: the outcome of the last installation
//	latest.json, latest.json.minisig   the last verified check result (Go)
//	.import-*/     offline update staging (Go)
//	backup/        Electron's copies of the replaced installation (Go never touches it)

const (
	manifestFile   = "update.json"
	signatureFile  = "update.json.minisig"
	pendingFile    = "pending.json"
	previousFile   = "previous.json"
	resultFile     = "result.json"
	latestFile     = "latest.json"
	latestSigFile  = "latest.json.minisig"
	importPrefix   = ".import-"
	fileSchema     = 1
	OriginDownload = "download"
	OriginFile     = "file"
)

// Pending is pending.json: a verified package to install at the next start.
type Pending struct {
	Schema      int    `json:"schema"`
	Version     string `json:"version"`
	FromVersion string `json:"from_version"`
	Channel     string `json:"channel"`
	Platform    string `json:"platform"`
	Kind        string `json:"kind"`
	File        string `json:"file"`      // relative to the updates directory, "/"-separated
	Size        int64  `json:"size"`      // bytes
	SHA256      string `json:"sha256"`    // hex, lower case
	Manifest    string `json:"manifest"`  // relative: <version>/update.json
	Signature   string `json:"signature"` // relative: <version>/update.json.minisig
	KeyID       string `json:"key_id"`    // the minisign key id that verified the signature (as minisign prints it)
	Origin      string `json:"origin"`    // download | file
	VerifiedAt  string `json:"verified_at"`
}

// Previous is previous.json: the version a failed update returns to.
type Previous struct {
	Schema   int    `json:"schema"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Kind     string `json:"kind"`
	// File is that version's verified package ("" = none on disk: Electron
	// backs up the installation before replacing it).
	File   string `json:"file,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Backup is written by Electron: its copy of the replaced installation
	// (relative to the updates directory).
	Backup    string `json:"backup,omitempty"`
	CreatedAt string `json:"created_at"`
}

// Result is result.json, written by Electron after an installation.
type Result struct {
	Version string `json:"version"`         // the version the update installed or tried to
	From    string `json:"from"`            // the version before
	Outcome string `json:"outcome"`         // installed | rolled_back | failed
	At      string `json:"at"`              // RFC 3339
	Error   string `json:"error,omitempty"` // a short reason (English, for "查看详情")
}

// Outcomes of Result.
const (
	OutcomeInstalled  = "installed"
	OutcomeRolledBack = "rolled_back"
	OutcomeFailed     = "failed"
)

// FailedInstall is an installation of a newer version that did not work
// (result.json rolled_back or failed while the running version is still
// older): that version is not offered again, and its package is removed,
// until a strictly newer version appears; the user may still try it again
// (Status.InstallFailed).
type FailedInstall struct {
	Version string `json:"version"`
	At      string `json:"at"`              // result.json's "at": the failure this is about
	Outcome string `json:"outcome"`         // rolled_back | failed
	Error   string `json:"error,omitempty"` // result.json's reason
}

// failedRecord is kv selfupdate.failed_install. Retry: the user asked to
// try that version again (a download or an offline update of it): not in
// effect, kept only so that the same result.json does not record it again.
type failedRecord struct {
	FailedInstall
	Retry bool `json:"retry,omitempty"`
}

// relPath joins parts into a "/"-separated path relative to the updates dir.
func relPath(parts ...string) string { return path.Join(parts...) }

// absPath resolves a relative path of a JSON file inside dir; ok is false
// for anything that would leave dir.
func absPath(dir, rel string) (string, bool) {
	if rel == "" || strings.Contains(rel, "\\") || path.IsAbs(rel) || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", false
	}
	clean := path.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || !filepath.IsLocal(filepath.FromSlash(clean)) {
		// (IsLocal also refuses, on Windows, device names such as NUL
		// and colons: a stream of another file)
		return "", false
	}
	return filepath.Join(dir, filepath.FromSlash(clean)), true
}

// writeFileAtomic writes data to a temporary file in the same directory,
// syncs it and renames it over path.
func writeFileAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = renameRetry(tmp, p)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}

func writeJSONAtomic(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, append(b, '\n'))
}

// readJSON reads a small JSON file; ok is false when it does not exist.
func readJSON(p string, v any) (bool, error) {
	f, err := os.Open(p)
	if isNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return true, fmt.Errorf("%s: %w", filepath.Base(p), err)
	}
	return true, nil
}

// renameRetry renames, retrying briefly: on Windows a virus scanner may
// hold a file it has just seen being written.
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if isNotExist(err) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// hashFile returns the SHA-256 (hex) and size of a file.
func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, f, make([]byte, 1<<20))
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// fileMatches reports whether p holds exactly the asset's bytes.
func fileMatches(p string, a Asset) bool {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != a.Size {
		return false
	}
	sum, n, err := hashFile(p)
	return err == nil && n == a.Size && sum == a.SHA256
}

// copyHash copies src to dst (created) and returns the SHA-256 and size of
// the bytes written; at most limit bytes are copied (more = errTooLarge).
func copyHash(dst string, src io.Reader, limit int64) (string, int64, error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", 0, &localError{err}
	}
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(out, h), io.LimitReader(src, limit+1), make([]byte, 1<<20))
	if cerr := out.Close(); err == nil && cerr != nil {
		err = &localError{cerr}
	}
	if err != nil {
		os.Remove(dst)
		return "", n, err
	}
	if n > limit {
		os.Remove(dst)
		return "", n, errTooLarge
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

var errTooLarge = errors.New("the file is larger than the package can be")
