// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/secrets"
)

// sec3 L3: a ciphertext moved to another provider's row, into the Colab
// credential, or from the credential into a provider's row, does not open
// there (it reads as "enter it again"); put back where it belongs, it opens.
func TestSealedValuesCannotMove(t *testing.T) {
	ctx := context.Background()
	withKey(t, dataKey(20))
	st := openAt(t, filepath.Join(t.TempDir(), "lectureflow.db"))
	if err := st.InsertProvider(ctx, provider("p1", fakeKeyA)); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertProvider(ctx, provider("p2", fakeKeyB)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecretKV(ctx, "colab.credential", testCredential(testRefresh)); err != nil {
		t.Fatal(err)
	}
	k1, k2, cred := rawKey(t, st, "p1"), rawKey(t, st, "p2"), rawKV(t, st, "colab.credential")
	for _, v := range []string{k1, k2, cred} {
		if !secrets.IsBound(v) {
			t.Fatalf("not stored bound: %.12s…", v)
		}
	}
	setKey := func(id, v string) {
		t.Helper()
		if _, err := st.db.Exec(`UPDATE provider SET api_key = ? WHERE id = ?`, v, id); err != nil {
			t.Fatal(err)
		}
	}
	setCred := func(v string) {
		t.Helper()
		if _, err := st.db.Exec(`UPDATE kv SET value = ? WHERE key = 'colab.credential'`, v); err != nil {
			t.Fatal(err)
		}
	}
	captureLogs(t)

	// p1's key copied into p2's row (a provider of the attacker's choosing)
	setKey("p2", k1)
	if p, _ := st.GetProvider(ctx, "p2"); p.APIKey != "" {
		t.Fatalf("p1's key opened in p2's row: %q", p.APIKey)
	}
	if provs, _, _ := st.UnreadableSecrets(ctx); strings.Join(provs, ",") != "p2" {
		t.Fatalf("UnreadableSecrets %v", provs)
	}
	// the provider key copied into the Colab credential, and back
	setCred(k1)
	if v, ok, _ := st.GetKV(ctx, "colab.credential"); ok || v != "" {
		t.Fatalf("a provider key opened as the Colab credential: %q", v)
	}
	setKey("p2", cred)
	if p, _ := st.GetProvider(ctx, "p2"); p.APIKey != "" {
		t.Fatal("the Colab credential opened as a provider key")
	}
	if provs, kv, _ := st.UnreadableSecrets(ctx); strings.Join(provs, ",") != "p2" || strings.Join(kv, ",") != "colab.credential" {
		t.Fatalf("UnreadableSecrets %v %v", provs, kv)
	}
	// the start leaves them alone (a misplaced value is not re-bound)
	if res, err := st.SealStoredSecrets(ctx); err != nil || res.Changed() || res.Unreadable != 2 {
		t.Fatalf("SealStoredSecrets %+v %v", res, err)
	}
	// back where they belong, they open
	setKey("p2", k2)
	setCred(cred)
	if p, _ := st.GetProvider(ctx, "p2"); p.APIKey != fakeKeyB {
		t.Fatalf("p2 after putting its key back: %q", p.APIKey)
	}
	if v, ok, _ := st.GetKV(ctx, "colab.credential"); !ok || !strings.Contains(v, testRefresh) {
		t.Fatal("the credential after putting it back")
	}
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != fakeKeyA {
		t.Fatalf("p1: %q", p.APIKey)
	}
}

// sec3 L3: base_url changed to another origin in the database itself (not
// through the API, which clears the key): the key no longer opens. The
// same origin written differently (path, case, default port) still opens
// it; set back, it opens again.
func TestBaseURLMovedInTheDatabase(t *testing.T) {
	ctx := context.Background()
	withKey(t, dataKey(21))
	st := openAt(t, filepath.Join(t.TempDir(), "lectureflow.db"))
	if err := st.InsertProvider(ctx, provider("p1", fakeKeyA)); err != nil { // https://llm.example.org
		t.Fatal(err)
	}
	captureLogs(t)
	for _, tc := range []struct {
		base string
		want string
	}{
		{"https://llm.example.org/v1", fakeKeyA},
		{"HTTPS://LLM.Example.ORG:443/", fakeKeyA},
		{"  https://llm.example.org  ", fakeKeyA},
		{"https://evil.example", ""},
		{"https://llm.example.org.evil.example", ""},
		{"http://llm.example.org", ""},       // another scheme
		{"https://llm.example.org:8443", ""}, // another port
		{"not a url", ""},
		{"https://llm.example.org", fakeKeyA},
	} {
		if _, err := st.db.Exec(`UPDATE provider SET base_url = ? WHERE id = 'p1'`, tc.base); err != nil {
			t.Fatal(err)
		}
		p, err := st.GetProvider(ctx, "p1")
		if err != nil || p.APIKey != tc.want {
			t.Errorf("base_url %q: key %q (%v), want %q", tc.base, p.APIKey, err, tc.want)
		}
		provs, _, _ := st.UnreadableSecrets(ctx)
		if unreadable := len(provs) == 1; unreadable != (tc.want == "") {
			t.Errorf("base_url %q: UnreadableSecrets %v", tc.base, provs)
		}
	}
	if BaseURLOrigin("https://LLM.example.org/v1") != "https://llm.example.org:443" || BaseURLOrigin("http://h") != "http://h:80" ||
		BaseURLOrigin(" x ") != "raw:x" {
		t.Fatal("BaseURLOrigin")
	}
}

// The first start of this version seals the 3.2.0 values (enc:v1:, no
// binding) again bound to their rows, on the secure path: no enc:v1:
// ciphertext stays in the files; the second start changes nothing.
func TestUnboundValuesAreBoundAtStart(t *testing.T) {
	ctx := context.Background()
	withKey(t, dataKey(22))
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st := openAt(t, path)
	churn(t, ctx, st, "filler")
	v1 := func(plain string) string {
		t.Helper()
		v, err := secrets.Seal([]byte(plain))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// what 3.2.0 wrote
	k1, k2, cred, own := v1(fakeKeyA), v1(fakeKeyB), v1(testCredential(testRefresh)), v1("-----BEGIN OPENSSH PRIVATE KEY----- test")
	for _, p := range []struct{ id, v string }{{"p1", k1}, {"p2", k2}} {
		if err := st.InsertProvider(ctx, provider(p.id, p.v)); err != nil { // stored as it is (sealed already)
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{"colab.credential": cred, "remote.ssh.key.s1": own} {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	// 3.2.0's values read until then
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != fakeKeyA {
		t.Fatalf("enc:v1: p1 reads %q", p.APIKey)
	}
	if v, ok, _ := st.GetKV(ctx, "colab.credential"); !ok || !strings.Contains(v, testRefresh) {
		t.Fatal("enc:v1: credential")
	}
	needle := func(v string) string { return v[len(secrets.Prefix) : len(secrets.Prefix)+24] }
	if got := fileHolds(t, path, needle(k1)); len(got) == 0 {
		t.Fatal("control: the enc:v1: value is not in the files")
	}

	l := captureLogs(t)
	res, err := st.SealStoredSecrets(ctx)
	if err != nil || res.Bound != 4 || res.Sealed != 0 || res.Resealed != 0 || res.Unreadable != 0 || !res.Changed() {
		t.Fatalf("%+v %v", res, err)
	}
	for id, want := range map[string]string{"p1": fakeKeyA, "p2": fakeKeyB} {
		raw := rawKey(t, st, id)
		if !secrets.IsBound(raw) {
			t.Errorf("%s still unbound", id)
		}
		if p, _ := st.GetProvider(ctx, id); p.APIKey != want {
			t.Errorf("%s: %q", id, p.APIKey)
		}
	}
	for _, k := range []string{"colab.credential", "remote.ssh.key.s1"} {
		raw := rawKV(t, st, k)
		if _, err := secrets.OpenFor(SealPurposeKV, k, raw); err != nil {
			t.Errorf("%s not bound to its row: %v", k, err)
		}
	}
	for _, v := range []string{k1, k2, cred, own} {
		if got := fileHolds(t, path, needle(v)); len(got) > 0 {
			t.Errorf("an enc:v1: ciphertext is still in %v", got)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("the WAL was not truncated: %d bytes", fi.Size())
	}
	before := rawKey(t, st, "p1")
	if res, err := st.SealStoredSecrets(ctx); err != nil || res.Changed() {
		t.Fatalf("second start: %+v %v", res, err)
	}
	if rawKey(t, st, "p1") != before {
		t.Fatal("the second start rewrote a bound value")
	}
	assertNoSecretInLogs(t, l.String(), st, "p1", "p2")
}

// An unchanged enc:v1: key written back by UpdateProvider (before the
// start converted it) is sealed bound, its old copy on the secure path.
func TestUpdateProviderBindsAnUnboundKey(t *testing.T) {
	ctx := context.Background()
	withKey(t, dataKey(23))
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st := openAt(t, path)
	churn(t, ctx, st, "filler")
	old, _ := secrets.Seal([]byte(fakeKeyA))
	if err := st.InsertProvider(ctx, provider("p1", old)); err != nil {
		t.Fatal(err)
	}
	p := provider("p1", fakeKeyA)
	p.RPM = 77
	if err := st.UpdateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if raw := rawKey(t, st, "p1"); !secrets.IsBound(raw) {
		t.Fatal("still unbound")
	}
	if got := fileHolds(t, path, old[len(secrets.Prefix):len(secrets.Prefix)+24]); len(got) > 0 {
		t.Errorf("the enc:v1: copy is still in %v", got)
	}
	if got, _ := st.GetProvider(ctx, "p1"); got.APIKey != fakeKeyA || got.RPM != 77 {
		t.Fatalf("%+v", got)
	}
}

// UpdateProvider with the key and base_url on another origin: the caller
// keeps the key on purpose (settings clears it instead), so it is sealed
// again for the new origin and still opens there.
func TestUpdateProviderRebindsOnAnotherOrigin(t *testing.T) {
	ctx := context.Background()
	withKey(t, dataKey(24))
	st := openAt(t, filepath.Join(t.TempDir(), "lectureflow.db"))
	if err := st.InsertProvider(ctx, provider("p1", fakeKeyA)); err != nil {
		t.Fatal(err)
	}
	before := rawKey(t, st, "p1")
	p := provider("p1", fakeKeyA)
	p.BaseURL = "https://other.example.net/v1"
	if err := st.UpdateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if rawKey(t, st, "p1") == before {
		t.Fatal("not sealed again for the new origin")
	}
	if got, _ := st.GetProvider(ctx, "p1"); got.APIKey != fakeKeyA {
		t.Fatalf("after the move: %q", got.APIKey)
	}
	// the same origin written differently keeps the stored value
	before = rawKey(t, st, "p1")
	p.BaseURL = "https://OTHER.example.net:443/v2"
	if err := st.UpdateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if rawKey(t, st, "p1") != before {
		t.Fatal("the same origin rewrote the key")
	}
}

// sec3 I4: UpdateProvider with an empty key keeps a stored key that cannot
// be decrypted here (also inside a transaction, as settings' tariff preset
// writes it), so the right key opens it again; UpdateProviderClearKey
// removes any key, without a copy left in the files. An empty key still
// removes a readable key (UpdateProvider writes every column).
func TestEmptyKeyKeepsAnUnreadableKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st := openAt(t, path)
	churn(t, ctx, st, "filler")
	withKey(t, dataKey(25))
	for _, id := range []string{"p1", "p2", "p3"} {
		if err := st.InsertProvider(ctx, provider(id, fakeKeyA)); err != nil {
			t.Fatal(err)
		}
	}
	sealed1, sealed2 := rawKey(t, st, "p1"), rawKey(t, st, "p2")
	captureLogs(t)

	withKey(t, dataKey(26)) // another computer: the keys do not open
	p, _ := st.GetProvider(ctx, "p1")
	if p.APIKey != "" {
		t.Fatalf("unreadable key reads %q", p.APIKey)
	}
	p.RPM = 33
	if err := st.UpdateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if rawKey(t, st, "p1") != sealed1 {
		t.Fatal("UpdateProvider with an empty key removed the key to enter again")
	}
	if got, _ := st.GetProvider(ctx, "p1"); got.RPM != 33 {
		t.Fatalf("the other fields: %+v", got)
	}
	// inside a transaction (applyTariffPreset)
	if err := st.Tx(ctx, func(tx Store) error {
		q, err := tx.GetProvider(ctx, "p2")
		if err != nil {
			return err
		}
		v := `{"x":1}`
		q.Tariff = &v
		return tx.UpdateProvider(ctx, q)
	}); err != nil {
		t.Fatal(err)
	}
	if rawKey(t, st, "p2") != sealed2 {
		t.Fatal("UpdateProvider in a transaction removed the key to enter again")
	}
	// a new key replaces it
	p.APIKey = fakeKeyB
	if err := st.UpdateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetProvider(ctx, "p1"); got.APIKey != fakeKeyB {
		t.Fatalf("re-entered: %q", got.APIKey)
	}
	// the explicit clear removes an unreadable key, leaving no copy
	q, _ := st.GetProvider(ctx, "p2")
	q.BaseURL = "https://moved.example.com"
	if err := st.UpdateProviderClearKey(ctx, q); err != nil {
		t.Fatal(err)
	}
	if raw := rawKey(t, st, "p2"); raw != "" {
		t.Fatalf("cleared key stored as %.12s…", raw)
	}
	if got, _ := st.GetProvider(ctx, "p2"); got.BaseURL != "https://moved.example.com" || got.APIKey != "" {
		t.Fatalf("after the clear: %+v", got)
	}
	if got := fileHolds(t, path, sealed2[len(secrets.PrefixV2):len(secrets.PrefixV2)+24]); len(got) > 0 {
		t.Errorf("the cleared ciphertext is still in %v", got)
	}
	if provs, _, _ := st.UnreadableSecrets(ctx); strings.Join(provs, ",") != "p3" {
		t.Fatalf("UnreadableSecrets %v", provs)
	}
	// the right key opens what was kept
	withKey(t, dataKey(25))
	if got, _ := st.GetProvider(ctx, "p3"); got.APIKey != fakeKeyA {
		t.Fatalf("p3 with the original key: %q", got.APIKey)
	}
	// a readable key: the explicit clear (and, as before, an empty key)
	// removes it
	r, _ := st.GetProvider(ctx, "p3")
	if err := st.UpdateProviderClearKey(ctx, r); err != nil {
		t.Fatal(err)
	}
	if rawKey(t, st, "p3") != "" {
		t.Fatal("a readable key was not cleared")
	}
	if err := st.InsertProvider(ctx, provider("p4", fakeKeyC)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProvider(ctx, provider("p4", "")); err != nil {
		t.Fatal(err)
	}
	if rawKey(t, st, "p4") != "" {
		t.Fatal("an empty key no longer removes a readable key")
	}
	// inside a transaction a plain write; ScrubFreedSecrets after it
	if err := st.InsertProvider(ctx, provider("p5", fakeKeyC)); err != nil {
		t.Fatal(err)
	}
	n5 := rawKey(t, st, "p5")[len(secrets.PrefixV2) : len(secrets.PrefixV2)+24]
	if err := st.Tx(ctx, func(tx Store) error {
		return tx.(ProviderKeyClearer).UpdateProviderClearKey(ctx, provider("p5", ""))
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ScrubFreedSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	if rawKey(t, st, "p5") != "" || len(fileHolds(t, path, n5)) > 0 {
		t.Fatal("the key cleared in a transaction")
	}
	if err := st.UpdateProviderClearKey(ctx, provider("nope", "")); err != ErrNotFound {
		t.Fatalf("missing row: %v", err)
	}
}
