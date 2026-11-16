// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

func useDataKey(t *testing.T, b byte) {
	t.Helper()
	if err := secrets.SetKey(bytes.Repeat([]byte{b}, secrets.KeySize)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secrets.ResetForTest)
}

func storedKey(t *testing.T, path, id string) string {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var v string
	if err := d.QueryRow(`SELECT api_key FROM provider WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A data directory from another computer or account: the provider's key
// cannot be decrypted. It reads as no key (never the ciphertext), is
// reported as "需要重新输入" with its own codes, stays stored while other
// fields change, and is replaced when the user enters it again.
func TestProviderKeyUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetKV(bg(), model.KVSettingsImported, "1")
	useDataKey(t, 1) // the other installation
	s, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := create(t, s, "DeepSeek", "openai", "https://api.deepseek.com", fakeKey)
	other := create(t, s, "Other", "openai", "https://llm.example.org", fakeKey)
	sealed := storedKey(t, path, p.ID)
	if !secrets.IsSealed(sealed) {
		t.Fatalf("stored %q", sealed)
	}
	if s.KeyUnreadable(p.ID) || s.KeyErrorInfo(p) != nil {
		t.Fatal("a readable key is reported")
	}

	useDataKey(t, 2) // this installation
	if err := s.Load(bg()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Provider(p.ID)
	if got.APIKey != "" || !s.KeyUnreadable(p.ID) {
		t.Fatalf("APIKey %q unreadable %v", got.APIKey, s.KeyUnreadable(p.ID))
	}
	info := s.KeyErrorInfo(got)
	if info == nil || info.Code != CodeProviderKeyUnreadable || info.Params["provider"] != "DeepSeek" || strings.Contains(info.Detail, secrets.Prefix) {
		t.Fatalf("KeyErrorInfo %+v", info)
	}
	ok, why := s.SnapshotUsableInfo(model.StageTranslate, `{"provider_id":"`+p.ID+`","model":"m"}`)
	if ok || why == nil || why.Code != CodeSnapshotKeyUnreadable {
		t.Fatalf("SnapshotUsableInfo %v %+v", ok, why)
	}

	// another field changes: the stored key stays (the right key file opens it)
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{RPM: ip(77)}); err != nil {
		t.Fatal(err)
	}
	if storedKey(t, path, p.ID) != sealed || !s.KeyUnreadable(p.ID) {
		t.Fatal("the unreadable key was overwritten by an unrelated change")
	}
	// another origin without a new key: cleared (sec2-app L2)
	if _, err := s.UpdateProvider(bg(), other.ID, ProviderInput{BaseURL: sp("https://elsewhere.example.org")}); err != nil {
		t.Fatal(err)
	}
	if storedKey(t, path, other.ID) != "" || s.KeyUnreadable(other.ID) {
		t.Fatal("moving to another origin kept the unreadable key")
	}
	if ok, why := s.SnapshotUsableInfo(model.StageTranslate, `{"provider_id":"`+other.ID+`","model":"m"}`); ok || why.Code != CodeSnapshotNoAPIKey {
		t.Fatalf("cleared key: %v %+v", ok, why)
	}
	// the user enters the key again
	const newKey = "sk-settings-test-NEW-11aa"
	u, err := s.UpdateProvider(bg(), p.ID, ProviderInput{APIKey: sp(newKey)})
	if err != nil || u.APIKey != newKey || u.RPM != 77 {
		t.Fatalf("re-entered %+v %v", u, err)
	}
	if s.KeyUnreadable(p.ID) || s.KeyErrorInfo(u) != nil {
		t.Fatal("still reported after re-entering")
	}
	if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(sealed[len(secrets.Prefix):len(secrets.Prefix)+24])) {
		t.Error("the replaced ciphertext is still in the database file")
	}
	if ok, _ := s.SnapshotUsableInfo(model.StageTranslate, `{"provider_id":"`+p.ID+`","model":"m"}`); !ok {
		t.Fatal("not usable after re-entering")
	}
}

// sec3 I4: the one-time DeepSeek tariff preset writes the providers back
// in a transaction (db.UpdateProvider with the key they were read with); a
// key that cannot be decrypted here (read as "") stays stored, so the right
// key file still opens it.
func TestTariffPresetKeepsAnUnreadableKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetKV(bg(), model.KVSettingsImported, "1")
	useDataKey(t, 3) // the other installation
	ds := model.Provider{ID: "ds", Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: "https://api.deepseek.com",
		APIKey: fakeKey, MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceDeepSeek}
	if err := st.InsertProvider(bg(), ds); err != nil {
		t.Fatal(err)
	}
	sealed := storedKey(t, path, "ds")
	useDataKey(t, 4) // this installation
	s, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.KeyUnreadable("ds") {
		t.Fatal("control: the key opens")
	}
	names, _, err := s.ApplyPhase2Defaults(bg())
	if err != nil || len(names) != 1 || names[0] != "DeepSeek" {
		t.Fatalf("preset %v %v", names, err)
	}
	if got, _ := s.Provider("ds"); got.Tariff == nil {
		t.Fatal("no preset")
	}
	if storedKey(t, path, "ds") != sealed || !s.KeyUnreadable("ds") {
		t.Fatal("the tariff preset removed the key to enter again")
	}
	useDataKey(t, 3) // the key file is put back
	if err := s.Load(bg()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Provider("ds"); got.APIKey != fakeKey || got.Tariff == nil {
		t.Fatalf("with the right key: %q %v", got.APIKey, got.Tariff)
	}
}

// Deleting a provider (in a transaction) leaves no copy of its key.
func TestDeleteProviderScrubsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetKV(bg(), model.KVSettingsImported, "1")
	s, err := Open(bg(), st, nil) // no data key: the key is stored plain, so the scan can see it
	if err != nil {
		t.Fatal(err)
	}
	p := create(t, s, "DeepSeek", "openai", "https://api.deepseek.com", fakeKey)
	if _, err := s.DeleteProvider(bg(), p.ID); err != nil {
		t.Fatal(err)
	}
	for _, sfx := range []string{"", "-wal"} {
		if b, err := os.ReadFile(path + sfx); err == nil && bytes.Contains(b, []byte(fakeKey)) {
			t.Errorf("the deleted key is still in lectureflow.db%s", sfx)
		}
	}
}

// sec3 L2: without a data key a key is still saved (in the clear, so that
// translation keeps working), and every place says so: the settings view's
// CredentialsErrorInfo and each such provider's KeyUnprotected. A start
// with a data key seals it and the marks go away.
func TestKeySavedWithoutDataKeyIsReported(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	secrets.ResetForTest()
	if CredentialsErrorInfo() != nil {
		t.Fatal("reported although Setup never ran")
	}
	secrets.SetStatusForTest(secrets.Status{Protection: secrets.ProtectionNone, Err: "DPAPI CryptProtectData: test failure"})
	info := CredentialsErrorInfo()
	if info == nil || info.Code != CodeCredentialsUnavailable || info.Params["error"] != "DPAPI CryptProtectData: test failure" {
		t.Fatalf("CredentialsErrorInfo %+v", info)
	}
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetKV(bg(), model.KVSettingsImported, "1")
	s, err := Open(bg(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := create(t, s, "DeepSeek", "openai", "https://api.deepseek.com", fakeKey)
	local := create(t, s, "Local", "openai", "http://127.0.0.1:8080/v1", "")
	if got, _ := s.Provider(p.ID); got.APIKey != fakeKey {
		t.Fatalf("the key is not usable: %q", got.APIKey)
	}
	if !s.KeyUnprotected(p.ID) || s.KeyUnprotected(local.ID) {
		t.Fatalf("KeyUnprotected %v %v", s.KeyUnprotected(p.ID), s.KeyUnprotected(local.ID))
	}
	if ki := s.KeyErrorInfo(p); ki == nil || ki.Code != CodeProviderKeyUnprotected || ki.Params["provider"] != "DeepSeek" {
		t.Fatalf("KeyErrorInfo %+v", ki)
	}
	if ok, _ := s.SnapshotUsableInfo(model.StageTranslate, `{"provider_id":"`+p.ID+`","model":"m"}`); !ok {
		t.Fatal("a plain key must stay usable")
	}
	// the next start has a data key: sealed, no more marks
	useDataKey(t, 9)
	secrets.SetStatusForTest(secrets.Status{Protection: secrets.ProtectionDPAPI})
	if _, err := st.(db.SecretSealer).SealStoredSecrets(bg()); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(bg()); err != nil {
		t.Fatal(err)
	}
	if s.KeyUnprotected(p.ID) || s.KeyErrorInfo(p) != nil || CredentialsErrorInfo() != nil {
		t.Fatal("still reported after sealing")
	}
	if !secrets.IsSealed(storedKey(t, path, p.ID)) {
		t.Fatal("not sealed")
	}
}
