// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Where the data key comes from (cred-enc, user decision 2026-10-27):
//
//   - Windows (desktop app and web mode): a random key in KeyFileName in
//     the data directory, protected with DPAPI for the current account
//     (CryptProtectData, no cgo). Copied to another computer or account,
//     the file no longer opens.
//   - macOS desktop app: the shell keeps the key in its profile, encrypted
//     with Electron's safeStorage (the Keychain item "LectureFlow Safe
//     Storage"), and hands it over on stdin with the access token
//     (-datakey-stdin). Nothing is written to the data directory.
//   - macOS web / development mode and other systems: the key itself in
//     KeyFileName, readable by the account only (0600); the diagnostics
//     say that the system's credential store does not protect it.
//
// A key file that cannot be opened (another computer or account, damaged)
// is never overwritten: it is renamed to KeyFileName.unreadable-<time> and
// a new key is made. The credentials sealed with the old key then ask to
// be entered again (db: "需要重新输入").

// KeyFileName is the data key file in the data directory.
const KeyFileName = "secrets.key"

// Protection says how the data key is protected.
type Protection string

const (
	// ProtectionDPAPI: Windows DPAPI, bound to the current account.
	ProtectionDPAPI Protection = "dpapi"
	// ProtectionKeychain: the macOS desktop shell's safeStorage (Keychain).
	ProtectionKeychain Protection = "keychain"
	// ProtectionFile: a file only the account can read; no credential store.
	ProtectionFile Protection = "file"
	// ProtectionNone: there is no data key; credentials are stored unsealed.
	ProtectionNone Protection = "none"
)

// Status is the outcome of Setup (the diagnostics' credentials row).
type Status struct {
	Protection Protection
	// KeyFile is the data key file ("" when the desktop shell holds the key).
	KeyFile string
	// Created: a new key was made in this start.
	Created bool
	// Replaced is where the earlier key file, which could not be opened,
	// was kept ("" when nothing was replaced).
	Replaced string
	// Err is why there is no data key (ProtectionNone).
	Err string
	// Unavailable: the system's credential store could not be used now
	// (ErrUnavailable: DPAPI failing for this account, the macOS Keychain
	// locked or access refused). The key file was left as it is; the next
	// start tries again (sec3 L4).
	Unavailable bool
	// PlainKeyFile: a key file without system protection was found where
	// the system protects keys (Windows); it is protected from now on (sec3
	// I2: someone who can write the data directory could have put it there).
	PlainKeyFile bool
	// PlainKeptFile is a kept key file (Recovered) without that protection
	// where the system protects keys ("" when none; the last one found):
	// its key opens values just the same, and it stays as it is (sec3 T5).
	PlainKeptFile string
	// Recovered counts the kept, earlier key files (KeyFileName.unreadable-*)
	// that open again: their keys are fallbacks, the values sealed with them
	// are sealed again with the data key.
	Recovered int
}

var (
	statusMu sync.RWMutex
	status   Status
)

// CurrentStatus returns the last Setup's outcome; the zero Status (empty
// Protection) when Setup has not run in this process (tests, tools).
func CurrentStatus() Status {
	statusMu.RLock()
	defer statusMu.RUnlock()
	return status
}

// SetStatusForTest replaces CurrentStatus (tests of the diagnostics).
func SetStatusForTest(s Status) {
	statusMu.Lock()
	status = s
	statusMu.Unlock()
}

// keyFile is the JSON of KeyFileName.
type keyFile struct {
	Format     string `json:"format"`     // keyFileFormat
	Version    int    `json:"version"`    // 1
	Protection string `json:"protection"` // dpapi | file
	// Key is base64: the DPAPI blob, or with "file" the key itself.
	Key  string `json:"key"`
	Note string `json:"note,omitempty"`
}

const (
	keyFileFormat = "lectureflow-datakey"
	keyFileNote   = "LectureFlow's data key: it encrypts the API keys and authorisations stored in lectureflow.db. Do not share or delete it."
)

// errNoOSProtection: this system has no credential store Go can use
// (protect_other.go).
var errNoOSProtection = errors.New("secrets: no system credential store")

// ErrUnavailable: the system's credential store cannot be used right now.
// It is told apart from a key that is protected for another computer or
// account by a round trip of a probe value (protectionWorks): when even
// that fails, the store itself is unavailable, and nothing is replaced.
var ErrUnavailable = errors.New("secrets: the system credential store is not available now")

// The system protection in use (tests replace them).
var (
	sysProtect   = protect
	sysUnprotect = unprotect
)

// protectionWorks reports whether the system protection round-trips a probe
// value now.
func protectionWorks() bool {
	probe := []byte("lectureflow-probe")
	blob, err := sysProtect(probe)
	if err != nil {
		return false
	}
	back, err := sysUnprotect(blob)
	return err == nil && bytes.Equal(back, probe)
}

// SetupUnavailable records that the macOS desktop shell could not use the
// Keychain now (access refused, locked): there is no data key in this run,
// the stored credentials read as to be entered again, and no key file is
// made or replaced; the next start tries again.
func SetupUnavailable(reason string) Status {
	st := Status{Protection: ProtectionNone, Unavailable: true, Err: "the macOS Keychain is not available now: " + reason}
	statusMu.Lock()
	status = st
	statusMu.Unlock()
	return st
}

// Setup establishes the process's data key for dataDir (SetKey) and
// records CurrentStatus. shellKey is the key the macOS desktop shell sent
// (nil otherwise). An error leaves the process without a data key
// (ProtectionNone, Status.Err): credentials are then stored unsealed, and
// the diagnostics show an error.
func Setup(dataDir string, shellKey []byte) (Status, error) {
	st, err := setup(dataDir, shellKey, time.Now())
	if err != nil {
		st.Protection, st.Err = ProtectionNone, err.Error()
	}
	statusMu.Lock()
	status = st
	statusMu.Unlock()
	return st, err
}

func setup(dataDir string, shellKey []byte, now time.Time) (Status, error) {
	path := filepath.Join(dataDir, KeyFileName)
	if shellKey != nil {
		if err := SetKey(shellKey); err != nil {
			return Status{}, fmt.Errorf("the desktop shell's data key: %w", err)
		}
		// A key file web mode made in this data directory: the values it
		// sealed open, and are sealed again with the shell's key.
		var fb [][]byte
		if k, err := PeekKeyFile(dataDir); err == nil && !bytes.Equal(k, shellKey) {
			fb = append(fb, k)
		}
		st := Status{Protection: ProtectionKeychain}
		return st, useFallbacks(path, shellKey, fb, &st)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		st, err := create(path)
		if err != nil {
			return st, err
		}
		return st, useFallbacks(path, nil, nil, &st)
	}
	if err != nil {
		// Not readable now (locked, I/O): never replaced for that.
		return Status{KeyFile: path}, fmt.Errorf("read %s: %w", path, err)
	}
	key, prot, derr := decodeKeyFile(b)
	if errors.Is(derr, ErrUnavailable) {
		// DPAPI does not work now at all: the file is left as it is.
		return Status{KeyFile: path, Unavailable: true}, derr
	}
	if derr == nil {
		if err := SetKey(key); err != nil {
			return Status{KeyFile: path}, err
		}
		st := Status{Protection: prot, KeyFile: path}
		if prot == ProtectionFile {
			if osProtection != ProtectionNone {
				// a plain key file (from a Mac, or put there): protect it
				// from now on, keeping the key; staying plain is no failure
				st.PlainKeyFile = true
				if err := writeKeyFile(path, key); err == nil {
					st.Protection = osProtection
				}
			} else {
				tightenKeyFile(path)
			}
		}
		return st, useFallbacks(path, key, nil, &st)
	}
	// Unreadable: kept under another name (it may still be restored), and
	// a new key made.
	backup := path + ".unreadable-" + now.Format("20060102-150405")
	if _, err := os.Lstat(backup); err == nil {
		backup += fmt.Sprintf("-%d", now.UnixNano()%1e6)
	}
	if err := os.Rename(path, backup); err != nil {
		return Status{KeyFile: path}, fmt.Errorf("keep the unreadable %s (%v): %w", KeyFileName, derr, err)
	}
	st, err := create(path)
	st.Replaced = backup
	if err != nil {
		return st, err
	}
	return st, useFallbacks(path, nil, nil, &st)
}

// useFallbacks sets the fallback keys: extra, plus the kept key files
// (path.unreadable-*) that open now (the data directory came back to the
// account that made them, or the store failed for a moment in an earlier
// start); current is the data key (nil: just made, so none of them).
func useFallbacks(path string, current []byte, extra [][]byte, st *Status) error {
	fb := extra
	kept, _ := filepath.Glob(path + ".unreadable-*")
	for _, f := range kept {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if k, prot, err := decodeKeyFile(b); err == nil && !bytes.Equal(k, current) {
			fb = append(fb, k)
			st.Recovered++
			if prot == ProtectionFile && osProtection != ProtectionNone {
				// reported like a plain data key file (sec3 I2, T5): its
				// key opens values just the same
				st.PlainKeptFile = f
			}
		}
	}
	return SetFallbackKeys(fb...)
}

// create makes a new data key, writes it to path and sets it.
func create(path string) (Status, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return Status{KeyFile: path}, err
	}
	prot, err := writeKeyFileProt(path, key)
	if err != nil {
		if osProtection != ProtectionNone && !protectionWorks() {
			// no key file without the system's protection
			return Status{KeyFile: path, Unavailable: true}, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return Status{KeyFile: path}, fmt.Errorf("write %s: %w", path, err)
	}
	if err := SetKey(key); err != nil {
		return Status{KeyFile: path}, err
	}
	return Status{Protection: prot, KeyFile: path, Created: true}, nil
}

// PeekKeyFile reads the data key of the data directory dir without
// changing anything (the import of another data directory; the macOS
// desktop app's predecessor key).
func PeekKeyFile(dir string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, KeyFileName))
	if err != nil {
		return nil, err
	}
	key, _, err := decodeKeyFile(b)
	return key, err
}

func decodeKeyFile(b []byte) ([]byte, Protection, error) {
	var kf keyFile
	if err := json.Unmarshal(b, &kf); err != nil || kf.Format != keyFileFormat || kf.Version != 1 {
		return nil, "", errors.New("not a LectureFlow data key file")
	}
	raw, err := base64.StdEncoding.DecodeString(kf.Key)
	if err != nil {
		return nil, "", errors.New("the data key file is damaged")
	}
	var key []byte
	switch Protection(kf.Protection) {
	case ProtectionFile:
		key = raw
	case ProtectionDPAPI:
		if key, err = sysUnprotect(raw); err != nil {
			if osProtection == ProtectionDPAPI && !protectionWorks() {
				return nil, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
			}
			return nil, "", fmt.Errorf("the data key is protected for another computer or account: %w", err)
		}
	default:
		return nil, "", fmt.Errorf("unknown data key protection %q", kf.Protection)
	}
	if len(key) != KeySize {
		return nil, "", errors.New("the data key file holds no 32-byte key")
	}
	return key, Protection(kf.Protection), nil
}

func writeKeyFile(path string, key []byte) error {
	_, err := writeKeyFileProt(path, key)
	return err
}

// writeKeyFileProt writes key to path, protected by the system when it can
// (osProtection), else as a 0600 file: a temporary file in the same
// directory, synced, then renamed over path.
func writeKeyFileProt(path string, key []byte) (Protection, error) {
	kf := keyFile{Format: keyFileFormat, Version: 1, Note: keyFileNote}
	prot := ProtectionFile
	if osProtection != ProtectionNone {
		blob, err := sysProtect(key)
		if err != nil {
			return "", err
		}
		prot, kf.Key = osProtection, base64.StdEncoding.EncodeToString(blob)
	} else {
		kf.Key = base64.StdEncoding.EncodeToString(key)
	}
	kf.Protection = string(prot)
	b, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), KeyFileName+".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if runtime.GOOS != "windows" {
		if err := f.Chmod(0o600); err != nil {
			return "", err
		}
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	ok = true
	return prot, nil
}

// tightenKeyFile makes a plain key file readable by the account only (a
// copy may have come with wider permissions).
func tightenKeyFile(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, 0o600)
	}
}
