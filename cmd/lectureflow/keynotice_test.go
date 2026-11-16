// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

// test-upgrade F6: the key file deleted after credentials were sealed with
// it. The start makes a new key and notes why the credentials cannot be
// decrypted (api.KVCredentialKeyNotice), also for the starts after it;
// the original file put back as secrets.key.unreadable-restored opens
// them again (the existing fallback of internal/secrets) and the note is
// cleared. No key, ciphertext or key file content in the note or the log.
func TestSetupSecretsKeyFileMissingNotice(t *testing.T) {
	secrets.ResetForTest()
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	dir := t.TempDir()
	setupSecrets(ctx, dir, nil, false)
	path := filepath.Join(dir, "lectureflow.db")
	st, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InsertProvider(ctx, model.Provider{ID: "p1", Name: "X", Type: model.ProviderOpenAI, BaseURL: "https://llm.example.org",
		APIKey: setupFakeKey, MaxConcurrency: 1, RPM: 60, BalanceAPI: model.BalanceNone}); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, secrets.KeyFileName)
	original, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}

	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	note := func(what string) api.CredentialKeyNotice {
		t.Helper()
		v, _, err := st.GetKV(ctx, api.KVCredentialKeyNotice)
		if err != nil {
			t.Fatal(err)
		}
		var n api.CredentialKeyNotice
		if v != "" {
			if err := json.Unmarshal([]byte(v), &n); err != nil {
				t.Fatalf("%s: %q", what, v)
			}
		}
		return n
	}
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, false)
	if s := secrets.CurrentStatus(); !s.Created || s.Replaced != "" {
		t.Fatalf("status %+v", s)
	}
	if n := note("missing"); n.Kind != api.KeyNoticeMissing || n.Kept != "" {
		t.Fatalf("note %+v", n)
	}
	if !strings.Contains(logs.String(), "the data key file was missing") {
		t.Fatalf("not logged:\n%s", logs.String())
	}
	// the next start keeps the note
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, false)
	if n := note("next start"); n.Kind != api.KeyNoticeMissing {
		t.Fatalf("next start: %+v", n)
	}
	// the original file put back under the name the hint gives
	if err := os.WriteFile(keyFile+".unreadable-restored", original, 0o600); err != nil {
		t.Fatal(err)
	}
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, false)
	if s := secrets.CurrentStatus(); s.Recovered != 1 {
		t.Fatalf("restored: %+v", s)
	}
	if n := note("restored"); n.Kind != "" {
		t.Fatalf("the note stays after the restore: %+v", n)
	}
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != setupFakeKey {
		t.Fatal("the restored key file did not open the key")
	}
	out := logs.String()
	for _, needle := range []string{setupFakeKey, secrets.Prefix, string(original)} {
		if strings.Contains(out, needle) {
			t.Errorf("the log holds %q", needle)
		}
	}
}

// test-upgrade F6: a key file that could not be opened is noted with the
// kept file's path; a start that reports no replacement keeps an earlier
// note while credentials still cannot be decrypted; the macOS desktop
// app's Keychain key (no key file) is never noted as a missing file.
func TestRecordKeyNotice(t *testing.T) {
	ctx := context.Background()
	st, err := db.Open(filepath.Join(t.TempDir(), "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	get := func() string { v, _, _ := st.GetKV(ctx, api.KVCredentialKeyNotice); return v }
	recordKeyNotice(ctx, st, secrets.Status{Protection: secrets.ProtectionKeychain, Created: true}, 1)
	if get() != "" {
		t.Fatalf("keychain: %q", get())
	}
	kept := filepath.Join("d", secrets.KeyFileName+".unreadable-20261111-100000")
	recordKeyNotice(ctx, st, secrets.Status{Protection: secrets.ProtectionDPAPI, KeyFile: "k", Created: true, Replaced: kept}, 2)
	var n api.CredentialKeyNotice
	if err := json.Unmarshal([]byte(get()), &n); err != nil || n.Kind != api.KeyNoticeReplaced || n.Kept != kept {
		t.Fatalf("replaced: %q %v", get(), err)
	}
	recordKeyNotice(ctx, st, secrets.Status{Protection: secrets.ProtectionDPAPI, KeyFile: "k"}, 2)
	if err := json.Unmarshal([]byte(get()), &n); err != nil || n.Kind != api.KeyNoticeReplaced || n.Kept != kept {
		t.Fatalf("kept: %q", get())
	}
	recordKeyNotice(ctx, st, secrets.Status{Protection: secrets.ProtectionDPAPI, KeyFile: "k"}, 0)
	if get() != "" {
		t.Fatalf("cleared: %q", get())
	}
}

// Security review SEC3 L1: a start without the data key (the system's
// credential store not available now, or no key at all) cannot tell which
// stored credentials open, so it keeps the note an earlier start wrote.
func TestRecordKeyNoticeKeptWithoutKey(t *testing.T) {
	ctx := context.Background()
	st, err := db.Open(filepath.Join(t.TempDir(), "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	get := func() string { v, _, _ := st.GetKV(ctx, api.KVCredentialKeyNotice); return v }
	kept := filepath.Join("d", secrets.KeyFileName+".unreadable-20261111-100000")
	recordKeyNotice(ctx, st, secrets.Status{Protection: secrets.ProtectionDPAPI, KeyFile: "k", Created: true, Replaced: kept}, 2)
	want := get()
	if want == "" {
		t.Fatal("no note")
	}
	for name, s := range map[string]secrets.Status{
		"unavailable": {Protection: secrets.ProtectionNone, KeyFile: "k", Unavailable: true, Err: "DPAPI failing"},
		"no key":      {Protection: secrets.ProtectionNone, KeyFile: "k", Err: "read k: locked"},
	} {
		recordKeyNotice(ctx, st, s, 0)
		if get() != want {
			t.Fatalf("%s: the note became %q", name, get())
		}
	}
}

// The same through a start: the credential store not available now
// (the macOS shell's report; DPAPI failing is the same status) after a
// start that made a new key file keeps that start's note.
func TestSetupSecretsUnavailableKeepsKeyNotice(t *testing.T) {
	secrets.ResetForTest()
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	dir := t.TempDir()
	setupSecrets(ctx, dir, nil, false)
	path := filepath.Join(dir, "lectureflow.db")
	st, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InsertProvider(ctx, model.Provider{ID: "p1", Name: "X", Type: model.ProviderOpenAI, BaseURL: "https://llm.example.org",
		APIKey: setupFakeKey, MaxConcurrency: 1, RPM: 60, BalanceAPI: model.BalanceNone}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, secrets.KeyFileName)); err != nil {
		t.Fatal(err)
	}
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, false)
	want, _, _ := st.GetKV(ctx, api.KVCredentialKeyNotice)
	if want == "" {
		t.Fatal("no note after the key file went missing")
	}
	secrets.ResetForTest()
	setupSecretsWith(ctx, dir, nil, true, nil)
	if v, _, _ := st.GetKV(ctx, api.KVCredentialKeyNotice); v != want {
		t.Fatalf("the note became %q", v)
	}
}

// SEC31 L5: a kept key file without the system's protection is named in
// its own warning, which says it stays as it is (not the data key file's
// "protected from now on"); the file is not rewritten. Where the system
// protects no key (not Windows), a plain file is the usual form: no
// warning.
func TestSetupSecretsWarnsPlainKeptKeyFile(t *testing.T) {
	secrets.ResetForTest()
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	dir := t.TempDir()
	setupSecrets(ctx, dir, nil, false)
	kept := filepath.Join(dir, secrets.KeyFileName+".unreadable-planted")
	plain, _ := json.Marshal(map[string]any{"format": "lectureflow-datakey", "version": 1, "protection": "file",
		"key": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, secrets.KeySize))})
	if err := os.WriteFile(kept, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, false)
	if s := secrets.CurrentStatus(); s.Recovered != 1 {
		t.Fatalf("the kept file's key is not a fallback: %+v", s)
	}
	out := logs.String()
	if strings.Contains(out, "protected from now on") {
		t.Fatalf("the data key file's warning:\n%s", out)
	}
	warned := strings.Contains(out, "an earlier key file kept in the data directory is not protected") &&
		strings.Contains(out, "leaves that file as it is") && strings.Contains(out, kept)
	if want := runtime.GOOS == "windows"; warned != want {
		t.Fatalf("warned %v, want %v:\n%s", warned, want, out)
	}
	if now, _ := os.ReadFile(kept); !bytes.Equal(now, plain) {
		t.Fatal("the kept file was rewritten")
	}
}
