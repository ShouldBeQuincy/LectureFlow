// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

// Test values only (never real keys).
const (
	setupFakeKey     = "sk-test-setup-secrets-0042"
	setupFakeRefresh = "1//test-refresh-setup-secrets-77"
)

var testDataKey = bytes.Repeat([]byte{0x3c}, secrets.KeySize)

func TestReadDataKey(t *testing.T) {
	hexKey := hex.EncodeToString(testDataKey)
	k, err := readDataKey(bufio.NewReader(strings.NewReader(" " + hexKey + "\r\nrest")))
	if err != nil || !bytes.Equal(k, testDataKey) {
		t.Fatalf("%x %v", k, err)
	}
	for _, bad := range []string{"", "\n", hexKey[:62] + "\n", strings.Repeat("zz", 32) + "\n", hexKey + "00\n"} {
		_, err := readDataKey(bufio.NewReader(strings.NewReader(bad)))
		if err == nil {
			t.Fatalf("%q accepted", bad)
		}
		if line := strings.TrimSpace(bad); line != "" && strings.Contains(err.Error(), line) {
			t.Fatalf("the error repeats the line: %v", err)
		}
	}
	old := tokenReadTimeout
	tokenReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { tokenReadTimeout = old })
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close(); pr.Close() })
	if _, err := readDataKey(bufio.NewReader(pr)); err == nil {
		t.Fatal("no key line accepted")
	}
}

// The first start after the update: the plain credentials of an existing
// database are sealed in place, no copy stays in the files, nothing secret
// reaches the log; the next start changes nothing.
func TestSetupSecretsSealsStoredCredentials(t *testing.T) {
	secrets.ResetForTest()
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "lectureflow.db")
	st, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertProvider(ctx, model.Provider{ID: "p1", Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com",
		APIKey: setupFakeKey, MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceDeepSeek}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, colab.KVCredential, `{"refresh_token":"`+setupFakeRefresh+`"}`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if !fileHolds(t, path, setupFakeKey) {
		t.Fatal("control: the plain key is not in the file")
	}

	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	setupSecrets(ctx, dir, nil, false)
	if s := secrets.CurrentStatus(); !secrets.HasKey() || !s.Created || s.KeyFile != filepath.Join(dir, secrets.KeyFileName) {
		t.Fatalf("status %+v", s)
	}
	for _, needle := range []string{setupFakeKey, setupFakeRefresh} {
		if fileHolds(t, path, needle) {
			t.Errorf("%q is still in the database files", needle)
		}
	}
	st, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if p, err := st.GetProvider(ctx, "p1"); err != nil || p.APIKey != setupFakeKey {
		t.Fatalf("provider %q %v", p.APIKey, err)
	}
	if v, ok, _ := st.GetKV(ctx, colab.KVCredential); !ok || !strings.Contains(v, setupFakeRefresh) {
		t.Fatalf("credential %q", v)
	}
	out := logs.String()
	for _, needle := range []string{setupFakeKey, setupFakeRefresh, secrets.Prefix, secrets.PrefixV2, hex.EncodeToString(testDataKey)} {
		if strings.Contains(out, needle) {
			t.Errorf("the log holds %q:\n%s", needle, out)
		}
	}
	if !strings.Contains(out, "sealed=2") {
		t.Errorf("the sealing is not logged:\n%s", out)
	}
	// the next start: the same key, nothing rewritten
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, false)
	if s := secrets.CurrentStatus(); s.Created || s.Replaced != "" {
		t.Fatalf("second start %+v", s)
	}
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != setupFakeKey {
		t.Fatal("the second start lost the key")
	}
}

// sec3 L3: the first start of this version on a 3.2.0 data directory seals
// its credentials (enc:v1:, no binding) again bound to their rows, on the
// secure path; the log says how many, and nothing secret.
func TestSetupSecretsBindsUnboundCredentials(t *testing.T) {
	secrets.ResetForTest()
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	dir := t.TempDir()
	setupSecrets(ctx, dir, nil, false) // 3.2.0's first start: the key file
	path := filepath.Join(dir, "lectureflow.db")
	st, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := secrets.Seal([]byte(setupFakeKey)) // what 3.2.0 stored
	c, _ := secrets.Seal([]byte(`{"refresh_token":"` + setupFakeRefresh + `"}`))
	if err := st.InsertProvider(ctx, model.Provider{ID: "p1", Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com",
		APIKey: k, MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceDeepSeek}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, colab.KVCredential, c); err != nil {
		t.Fatal(err)
	}
	st.Close()
	needle := k[len(secrets.Prefix) : len(secrets.Prefix)+24]
	if !fileHolds(t, path, needle) {
		t.Fatal("control: the enc:v1: value is not in the file")
	}

	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, false) // this version's first start
	out := logs.String()
	if !strings.Contains(out, "bound=2") {
		t.Errorf("the binding is not logged:\n%s", out)
	}
	for _, n := range []string{setupFakeKey, setupFakeRefresh, secrets.Prefix, secrets.PrefixV2} {
		if strings.Contains(out, n) {
			t.Errorf("the log holds %q", n)
		}
	}
	if fileHolds(t, path, needle) {
		t.Error("the enc:v1: ciphertext is still in the database files")
	}
	st, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if p, err := st.GetProvider(ctx, "p1"); err != nil || p.APIKey != setupFakeKey {
		t.Fatalf("provider %q %v", p.APIKey, err)
	}
	if v, ok, _ := st.GetKV(ctx, colab.KVCredential); !ok || !strings.Contains(v, setupFakeRefresh) {
		t.Fatalf("credential %q", v)
	}
}

// A data directory from another computer or account: the key file is kept
// under another name, a new key is made, the start goes on; the provider
// reads without a key (it has to be entered again).
func TestSetupSecretsForeignKeyFile(t *testing.T) {
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
	// another installation's key file in its place
	if err := os.WriteFile(filepath.Join(dir, secrets.KeyFileName), []byte(`{"format":"lectureflow-datakey","version":1,"protection":"dpapi","key":"AAAA"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, false)
	s := secrets.CurrentStatus()
	if s.Replaced == "" || !secrets.HasKey() {
		t.Fatalf("status %+v", s)
	}
	if _, err := os.Stat(s.Replaced); err != nil {
		t.Fatalf("the old key file was not kept: %v", err)
	}
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != "" {
		t.Fatalf("an undecryptable key reads as %q", p.APIKey)
	}
	if ids, _, _ := st.(db.SecretInspector).UnreadableSecrets(ctx); strings.Join(ids, ",") != "p1" {
		t.Fatalf("unreadable %v", ids)
	}
}

// Desktop mode on macOS: the shell sends the data key on the line after
// the token (-datakey-stdin). Credentials are sealed with it, and nothing
// is written to the data directory for it.
func TestRunDesktopDataKeyStdin(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, _ := writeTestConfig(t)
	dataDir := filepath.Join(t.TempDir(), "desk")
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-data-dir", dataDir, "-desktop", "-datakey-stdin"},
		testToken+"\n"+hex.EncodeToString(testDataKey)+"\n", AppOptions{})
	p.ready()
	a := <-p.app
	if s := secrets.CurrentStatus(); s.Protection != secrets.ProtectionKeychain || s.KeyFile != "" {
		t.Fatalf("status %+v", s)
	}
	if _, err := os.Stat(filepath.Join(dataDir, secrets.KeyFileName)); err == nil {
		t.Fatal("a key file was written although the shell holds the key")
	}
	ctx := context.Background()
	if err := a.Store.InsertProvider(ctx, model.Provider{ID: "p1", Name: "X", Type: model.ProviderOpenAI, BaseURL: "https://llm.example.org",
		APIKey: setupFakeKey, MaxConcurrency: 1, RPM: 60, BalanceAPI: model.BalanceNone}); err != nil {
		t.Fatal(err)
	}
	if p, _ := a.Store.GetProvider(ctx, "p1"); p.APIKey != setupFakeKey {
		t.Fatalf("read back %q", p.APIKey)
	}
	if fileHolds(t, filepath.Join(dataDir, "lectureflow.db"), setupFakeKey) {
		t.Fatal("the key is stored plain")
	}
	for _, out := range []string{p.stdout(), p.stderr.String()} {
		if strings.Contains(out, hex.EncodeToString(testDataKey)) || strings.Contains(out, setupFakeKey) {
			t.Fatal("the data key or the API key reached the output")
		}
	}
	p.stdinW.Close()
	if c := p.wait(10 * time.Second); c != exitOK {
		t.Fatalf("exit code %d", c)
	}
}

// A bad key line, or -datakey-stdin without -desktop, fails the start
// without repeating what was sent.
func TestRunDataKeyStdinRefusals(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, _ := writeTestConfig(t)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop", "-datakey-stdin"}, testToken+"\nnot-a-key-0123\n", AppOptions{})
	if c := p.wait(10 * time.Second); c != exitFailure {
		t.Fatalf("exit code %d", c)
	}
	if e := p.stderr.String(); !strings.Contains(e, "data key") || strings.Contains(e, "not-a-key-0123") {
		t.Fatalf("stderr %q", e)
	}
	p2 := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-datakey-stdin"}, "", AppOptions{})
	if c := p2.wait(10 * time.Second); c != exitFailure {
		t.Fatalf("web mode with -datakey-stdin: exit code %d", c)
	}
}

// sec3 L4: the shell could not use the Keychain now. The service runs
// without a data key, writes no key file and replaces nothing; the stored
// credentials read as to be entered again.
func TestSetupSecretsShellUnavailable(t *testing.T) {
	secrets.ResetForTest()
	t.Cleanup(secrets.ResetForTest)
	if k, err := readDataKey(bufio.NewReader(strings.NewReader("unavailable\n"))); k != nil || !errors.Is(err, errShellKeyUnavailable) {
		t.Fatalf("%x %v", k, err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "lectureflow.db")
	if err := secrets.SetKey(testDataKey); err != nil { // the Keychain's key, earlier
		t.Fatal(err)
	}
	st, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertProvider(ctx, model.Provider{ID: "p1", Name: "X", Type: model.ProviderOpenAI, BaseURL: "https://llm.example.org",
		APIKey: setupFakeKey, MaxConcurrency: 1, RPM: 60, BalanceAPI: model.BalanceNone}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, true)
	if s := secrets.CurrentStatus(); !s.Unavailable || s.Protection != secrets.ProtectionNone || secrets.HasKey() {
		t.Fatalf("status %+v", s)
	}
	if _, err := os.Stat(filepath.Join(dir, secrets.KeyFileName)); err == nil {
		t.Fatal("a key file was written")
	}
	st, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != "" {
		t.Fatalf("read as %q", p.APIKey)
	}
	// the Keychain is back: the same key opens it
	if err := secrets.SetKey(testDataKey); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != setupFakeKey {
		t.Fatalf("after the Keychain is back: %q", p.APIKey)
	}
}

// sec3 L1: the first start removes the providers' keys from the databases
// earlier imports replaced, once.
func TestSetupSecretsScrubsImportBackupsOnce(t *testing.T) {
	secrets.ResetForTest()
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	bdir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(bdir, "lectureflow-before-import-20261001-120000.db")
	b, err := db.Open(bak)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.InsertProvider(ctx, model.Provider{ID: "p1", Name: "X", Type: model.ProviderOpenAI, BaseURL: "https://llm.example.org",
		APIKey: setupFakeKey, MaxConcurrency: 1, RPM: 60, BalanceAPI: model.BalanceNone}); err != nil { // no data key: plain, as 3.1 left it
		t.Fatal(err)
	}
	b.Close()
	if !fileHolds(t, bak, setupFakeKey) {
		t.Fatal("control: the backup does not hold the key")
	}
	setupSecrets(ctx, dir, nil, false)
	if fileHolds(t, bak, setupFakeKey) {
		t.Fatal("the import backup still holds the key")
	}
	st, err = db.Open(filepath.Join(dir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, _, _ := st.GetKV(ctx, kvBackupsScrubbed); v != "1" {
		t.Fatalf("marker %q", v)
	}
	// once: a backup that appears later (it is scrubbed when it is made) is not looked at again
	before, _ := os.Stat(bak)
	secrets.ResetForTest()
	setupSecrets(ctx, dir, nil, false)
	if after, _ := os.Stat(bak); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the backups were scrubbed again")
	}
}

// The shell's "unavailable" line in a desktop start.
func TestRunDesktopDataKeyUnavailable(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, _ := writeTestConfig(t)
	dataDir := filepath.Join(t.TempDir(), "desk")
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-data-dir", dataDir, "-desktop", "-datakey-stdin"},
		testToken+"\nunavailable\n", AppOptions{})
	p.ready()
	<-p.app
	if s := secrets.CurrentStatus(); !s.Unavailable || secrets.HasKey() {
		t.Fatalf("status %+v", s)
	}
	if _, err := os.Stat(filepath.Join(dataDir, secrets.KeyFileName)); err == nil {
		t.Fatal("a key file was written")
	}
	p.stdinW.Close()
	if c := p.wait(10 * time.Second); c != exitOK {
		t.Fatalf("exit code %d", c)
	}
}

// test-upgrade F3: a pass over the import backups that a stop interrupted
// goes on with the backups left; the ones it had scrubbed (recorded with
// their size and time) are not done again. The record ends with the pass.
func TestSetupSecretsImportBackupsResume(t *testing.T) {
	secrets.ResetForTest()
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	bdir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) string {
		p := filepath.Join(bdir, name)
		b, err := db.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.InsertProvider(ctx, model.Provider{ID: "p1", Name: "X", Type: model.ProviderOpenAI, BaseURL: "https://llm.example.org",
			APIKey: setupFakeKey, MaxConcurrency: 1, RPM: 60, BalanceAPI: model.BalanceNone}); err != nil {
			t.Fatal(err)
		}
		b.Close()
		return p
	}
	first := mk("lectureflow-before-import-20261001-120000.db")
	second := mk("lectureflow-before-import-20261002-120000.db")
	// the interrupted pass had scrubbed the first one (here: recorded as if)
	fi, _ := os.Stat(first)
	rec, _ := json.Marshal(map[string]string{filepath.Base(first): fileStamp(fi)})
	if err := st.SetKV(ctx, kvBackupsScrubbedFiles, string(rec)); err != nil {
		t.Fatal(err)
	}
	st.Close()

	setupSecrets(ctx, dir, nil, false)
	if after, _ := os.Stat(first); !after.ModTime().Equal(fi.ModTime()) || after.Size() != fi.Size() {
		t.Fatal("the backup recorded as scrubbed was done again")
	}
	if fileHolds(t, second, setupFakeKey) {
		t.Fatal("the backup left holds the key")
	}
	st, err = db.Open(filepath.Join(dir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, _, _ := st.GetKV(ctx, kvBackupsScrubbed); v != "1" {
		t.Fatalf("marker %q", v)
	}
	if v, _, _ := st.GetKV(ctx, kvBackupsScrubbedFiles); v != "" {
		t.Fatalf("the per-file record stays: %q", v)
	}
}
