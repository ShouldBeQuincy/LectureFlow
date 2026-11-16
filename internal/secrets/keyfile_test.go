// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func sealWithCurrent(t *testing.T) string {
	t.Helper()
	s, err := Seal([]byte(fakeAPIKey))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, KeyFileName+".unreadable-*"))
	return m
}

// First start: a key file is made; the next start opens the same key.
func TestSetupCreatesAndReopens(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	st, err := Setup(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := ProtectionFile
	if runtime.GOOS == "windows" {
		want = ProtectionDPAPI
	}
	if !st.Created || st.Protection != want || st.KeyFile != filepath.Join(dir, KeyFileName) || st.Replaced != "" || !HasKey() {
		t.Fatalf("first start: %+v", st)
	}
	if CurrentStatus() != st {
		t.Fatalf("CurrentStatus %+v", CurrentStatus())
	}
	sealed := sealWithCurrent(t)
	b, err := os.ReadFile(st.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	var kf keyFile
	if err := json.Unmarshal(b, &kf); err != nil || kf.Format != keyFileFormat || kf.Protection != string(want) {
		t.Fatalf("key file %s: %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(st.KeyFile); fi.Mode().Perm() != 0o600 {
			t.Errorf("key file mode %v", fi.Mode().Perm())
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, KeyFileName+".tmp-*")); len(m) > 0 {
		t.Errorf("temporary files left: %v", m)
	}

	reset(t)
	st2, err := Setup(dir, nil)
	if err != nil || st2.Created || st2.Protection != want || st2.Replaced != "" {
		t.Fatalf("second start: %+v %v", st2, err)
	}
	if p, err := Open(sealed); err != nil || string(p) != fakeAPIKey {
		t.Fatalf("the reopened key does not open the value: %v", err)
	}
	if k, err := PeekKeyFile(dir); err != nil || len(k) != KeySize {
		t.Fatalf("PeekKeyFile: %v", err)
	}
}

// A key file that cannot be opened (another computer or account, damaged)
// is kept under another name, never overwritten; a new key is made and the
// old values no longer open (the user enters them again).
func TestSetupUnreadableKeyFileIsKept(t *testing.T) {
	for name, content := range map[string]string{
		"garbage": "not json at all",
		"other":   `{"format":"lectureflow-datakey","version":1,"protection":"dpapi","key":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 230)) + `"}`,
		"short":   `{"format":"lectureflow-datakey","version":1,"protection":"file","key":"` + base64.StdEncoding.EncodeToString([]byte("short")) + `"}`,
		"unknown": `{"format":"lectureflow-datakey","version":1,"protection":"tpm","key":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			reset(t)
			dir := t.TempDir()
			path := filepath.Join(dir, KeyFileName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := Setup(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !st.Created || st.Replaced == "" || !HasKey() {
				t.Fatalf("status %+v", st)
			}
			old, err := os.ReadFile(st.Replaced)
			if err != nil || string(old) != content {
				t.Fatalf("the old key file was not kept as it was: %q %v", old, err)
			}
			if got := backups(t, dir); len(got) != 1 || got[0] != st.Replaced {
				t.Fatalf("backups %v", got)
			}
			if _, err := PeekKeyFile(dir); err != nil {
				t.Fatalf("the new key file: %v", err)
			}
			// the next start opens the new key: nothing more is replaced
			reset(t)
			if st2, err := Setup(dir, nil); err != nil || st2.Replaced != "" || st2.Created {
				t.Fatalf("next start %+v %v", st2, err)
			}
			if got := backups(t, dir); len(got) != 1 {
				t.Fatalf("backups after the next start %v", got)
			}
		})
	}
}

// Values sealed under a key file copied in from elsewhere no longer open.
func TestSetupKeyFromAnotherInstallation(t *testing.T) {
	reset(t)
	a, b := t.TempDir(), t.TempDir()
	if _, err := Setup(a, nil); err != nil {
		t.Fatal(err)
	}
	sealed := sealWithCurrent(t)
	reset(t)
	if _, err := Setup(b, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(sealed); err == nil {
		t.Fatal("another installation's value opened")
	}
}

// A read error that is not "missing" never replaces the key file.
func TestSetupReadErrorKeepsTheFile(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, KeyFileName), 0o700); err != nil { // reading a directory fails
		t.Fatal(err)
	}
	st, err := Setup(dir, nil)
	if err == nil || st.Protection != ProtectionNone || st.Err == "" || HasKey() {
		t.Fatalf("status %+v %v", st, err)
	}
	if CurrentStatus().Protection != ProtectionNone {
		t.Fatalf("CurrentStatus %+v", CurrentStatus())
	}
	if fi, err := os.Stat(filepath.Join(dir, KeyFileName)); err != nil || !fi.IsDir() || len(backups(t, dir)) > 0 {
		t.Fatalf("the entry was touched: %v %v", fi, err)
	}
}

// macOS desktop: the shell's key is used as it is; a key file web mode left
// in the data directory becomes a fallback (its values open and are sealed
// again), and nothing is written.
func TestSetupShellKey(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	if _, err := Setup(dir, nil); err != nil { // web mode first
		t.Fatal(err)
	}
	webSealed := sealWithCurrent(t)
	before, _ := os.ReadFile(filepath.Join(dir, KeyFileName))
	reset(t)
	shell := testKey(42)
	st, err := Setup(dir, shell)
	if err != nil || st.Protection != ProtectionKeychain || st.KeyFile != "" || st.Created {
		t.Fatalf("status %+v %v", st, err)
	}
	if p, cur, err := OpenAny(webSealed); err != nil || cur || string(p) != fakeAPIKey {
		t.Fatalf("web mode's value through the fallback: %q %v %v", p, cur, err)
	}
	s := sealWithCurrent(t)
	if p, err := OpenWith(shell, s); err != nil || string(p) != fakeAPIKey {
		t.Fatalf("not sealed with the shell's key: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, KeyFileName))
	if !bytes.Equal(before, after) {
		t.Fatal("the key file was changed")
	}
	reset(t)
	if _, err := Setup(t.TempDir(), []byte("short")); err == nil || HasKey() {
		t.Fatal("a short shell key was accepted")
	}
}

// A plain key file (e.g. from a Mac) is upgraded to DPAPI on Windows,
// keeping the key; elsewhere it stays plain, readable by the account only.
func TestSetupPlainKeyFile(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFileName)
	key := testKey(77)
	b, _ := json.Marshal(keyFile{Format: keyFileFormat, Version: 1, Protection: string(ProtectionFile), Key: base64.StdEncoding.EncodeToString(key)})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Setup(dir, nil)
	if err != nil || st.Created || st.Replaced != "" {
		t.Fatalf("status %+v %v", st, err)
	}
	s := sealWithCurrent(t)
	if _, err := OpenWith(key, s); err != nil {
		t.Fatal("the plain file's key is not the data key")
	}
	now, _ := os.ReadFile(path)
	if runtime.GOOS == "windows" {
		if st.Protection != ProtectionDPAPI || bytes.Contains(now, []byte(base64.StdEncoding.EncodeToString(key))) {
			t.Fatalf("not upgraded: %+v %s", st, now)
		}
		if k, err := PeekKeyFile(dir); err != nil || !bytes.Equal(k, key) {
			t.Fatalf("upgraded file: %v", err)
		}
	} else {
		if st.Protection != ProtectionFile {
			t.Fatalf("status %+v", st)
		}
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", fi.Mode().Perm())
		}
	}
}

func TestSetupBackupNameCollision(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFileName)
	now := time.Date(2026, 10, 27, 12, 0, 0, 5000, time.UTC)
	taken := path + ".unreadable-" + now.Format("20060102-150405")
	if err := os.WriteFile(taken, []byte("earlier"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := setup(dir, nil, now)
	if err != nil || st.Replaced == "" || st.Replaced == taken {
		t.Fatalf("%+v %v", st, err)
	}
	if b, _ := os.ReadFile(taken); string(b) != "earlier" {
		t.Fatal("an earlier backup was overwritten")
	}
	if b, _ := os.ReadFile(st.Replaced); string(b) != "broken" {
		t.Fatal("the replaced file was not kept")
	}
	if !strings.HasPrefix(filepath.Base(st.Replaced), KeyFileName+".unreadable-") {
		t.Fatalf("name %s", st.Replaced)
	}
}

// fakeStore stands in for DPAPI: blobs are bound to an account tag; down
// makes every call fail (the store itself unavailable).
type fakeStore struct {
	account string
	down    bool
}

func (f *fakeStore) protect(b []byte) ([]byte, error) {
	if f.down {
		return nil, errors.New("fake DPAPI: unavailable")
	}
	out := append([]byte(f.account+":"), b...)
	for i := len(f.account) + 1; i < len(out); i++ {
		out[i] ^= 0xa5
	}
	return out, nil
}

func (f *fakeStore) unprotect(b []byte) ([]byte, error) {
	if f.down {
		return nil, errors.New("fake DPAPI: unavailable")
	}
	p := []byte(f.account + ":")
	if !bytes.HasPrefix(b, p) {
		return nil, errors.New("fake DPAPI: another account")
	}
	out := append([]byte(nil), b[len(p):]...)
	for i := range out {
		out[i] ^= 0xa5
	}
	return out, nil
}

func useFakeStore(t *testing.T, f *fakeStore) {
	t.Helper()
	op, ou, oo := sysProtect, sysUnprotect, osProtection
	sysProtect, sysUnprotect, osProtection = f.protect, f.unprotect, ProtectionDPAPI
	t.Cleanup(func() { sysProtect, sysUnprotect, osProtection = op, ou, oo })
}

// sec3 L4: when the store itself does not work now (DPAPI failing for the
// account), the key file is left alone and no key is used this time; only
// a key that the working store cannot open (another account) is replaced.
func TestSetupStoreUnavailableKeepsKeyFile(t *testing.T) {
	reset(t)
	f := &fakeStore{account: "alice"}
	useFakeStore(t, f)
	dir := t.TempDir()
	if _, err := Setup(dir, nil); err != nil {
		t.Fatal(err)
	}
	sealed := sealWithCurrent(t)
	before, _ := os.ReadFile(filepath.Join(dir, KeyFileName))

	reset(t)
	f.down = true
	st, err := Setup(dir, nil)
	if !errors.Is(err, ErrUnavailable) || !st.Unavailable || st.Protection != ProtectionNone || st.Replaced != "" || HasKey() {
		t.Fatalf("store down: %+v %v", st, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, KeyFileName))
	if !bytes.Equal(before, after) || len(backups(t, dir)) > 0 {
		t.Fatal("the key file was touched while the store was down")
	}
	// a fresh data directory: no key file is written without the store
	empty := t.TempDir()
	if st, err := Setup(empty, nil); !errors.Is(err, ErrUnavailable) || !st.Unavailable {
		t.Fatalf("new key while down: %+v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(empty, KeyFileName)); err == nil {
		t.Fatal("a key file was written without the store")
	}

	reset(t)
	f.down = false // back: the same key
	if st, err := Setup(dir, nil); err != nil || st.Created || st.Replaced != "" {
		t.Fatalf("store back: %+v %v", st, err)
	}
	if p, err := Open(sealed); err != nil || string(p) != fakeAPIKey {
		t.Fatalf("the key changed: %v", err)
	}

	reset(t)
	f.account = "bob" // the store works, the file is another account's
	st, err = Setup(dir, nil)
	if err != nil || st.Replaced == "" || !st.Created {
		t.Fatalf("another account: %+v %v", st, err)
	}
}

// A kept key file that opens again (the data directory is back with the
// account that made it) becomes a fallback: its values open, and the
// database seals them again with the data key.
func TestSetupRecoversKeptKeyFile(t *testing.T) {
	reset(t)
	f := &fakeStore{account: "alice"}
	useFakeStore(t, f)
	dir := t.TempDir()
	if _, err := Setup(dir, nil); err != nil {
		t.Fatal(err)
	}
	sealedAlice := sealWithCurrent(t)
	reset(t)
	f.account = "bob" // copied to bob: alice's file is kept, a new key made
	if st, err := Setup(dir, nil); err != nil || st.Replaced == "" || st.Recovered != 0 {
		t.Fatalf("bob: %+v %v", st, err)
	}
	reset(t)
	f.account = "alice" // and back: bob's file is kept, alice's opens again
	st, err := Setup(dir, nil)
	if err != nil || st.Replaced == "" || st.Recovered != 1 {
		t.Fatalf("alice again: %+v %v", st, err)
	}
	if p, cur, err := OpenAny(sealedAlice); err != nil || cur || string(p) != fakeAPIKey {
		t.Fatalf("alice's value: %v %v", cur, err)
	}
	if got := backups(t, dir); len(got) != 2 {
		t.Fatalf("kept files %v", got)
	}
}

// sec3 I2: a plain key file where DPAPI protects keys is reported.
func TestSetupReportsPlainKeyFile(t *testing.T) {
	reset(t)
	useFakeStore(t, &fakeStore{account: "alice"})
	dir := t.TempDir()
	b, _ := json.Marshal(keyFile{Format: keyFileFormat, Version: 1, Protection: string(ProtectionFile), Key: base64.StdEncoding.EncodeToString(testKey(8))})
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Setup(dir, nil)
	if err != nil || !st.PlainKeyFile || st.Protection != ProtectionDPAPI {
		t.Fatalf("%+v %v", st, err)
	}
	reset(t)
	if st, err := Setup(dir, nil); err != nil || st.PlainKeyFile {
		t.Fatalf("second start: %+v %v", st, err)
	}
}

// Security review SEC3 T5: a kept key file (secrets.key.unreadable-*)
// without the system's protection is reported, by its own name (SEC31
// L5: not as the data key file, which is protected): its key opens values
// just the same. It is left as it is. One the system protects is not
// reported.
func TestSetupReportsPlainKeptKeyFile(t *testing.T) {
	reset(t)
	f := &fakeStore{account: "alice"}
	useFakeStore(t, f)
	dir := t.TempDir()
	if _, err := Setup(dir, nil); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(dir, KeyFileName+".unreadable-planted")
	b, _ := json.Marshal(keyFile{Format: keyFileFormat, Version: 1, Protection: string(ProtectionFile), Key: base64.StdEncoding.EncodeToString(testKey(9))})
	if err := os.WriteFile(kept, b, 0o600); err != nil {
		t.Fatal(err)
	}
	reset(t)
	st, err := Setup(dir, nil)
	if err != nil || st.PlainKeptFile != kept || st.PlainKeyFile || st.Recovered != 1 || st.Protection != ProtectionDPAPI {
		t.Fatalf("plain kept file: %+v %v", st, err)
	}
	if now, _ := os.ReadFile(kept); !bytes.Equal(now, b) {
		t.Fatal("the kept file was rewritten")
	}
	// protected by the store: no report
	blob, _ := f.protect(testKey(9))
	b, _ = json.Marshal(keyFile{Format: keyFileFormat, Version: 1, Protection: string(ProtectionDPAPI), Key: base64.StdEncoding.EncodeToString(blob)})
	if err := os.WriteFile(kept, b, 0o600); err != nil {
		t.Fatal(err)
	}
	reset(t)
	if st, err := Setup(dir, nil); err != nil || st.PlainKeyFile || st.PlainKeptFile != "" || st.Recovered != 1 {
		t.Fatalf("protected kept file: %+v %v", st, err)
	}
}

func TestSetupUnavailableFromShell(t *testing.T) {
	reset(t)
	st := SetupUnavailable("access refused")
	if st.Protection != ProtectionNone || !st.Unavailable || !strings.Contains(st.Err, "access refused") || HasKey() || CurrentStatus() != st {
		t.Fatalf("%+v", st)
	}
}
