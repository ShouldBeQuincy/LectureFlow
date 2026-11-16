// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

// Fake credentials only (never real keys).
const (
	fakeKeyA = "sk-test-cred-enc-aaaa0001aaaa"
	fakeKeyB = "sk-test-cred-enc-bbbb0002bbbb"
	fakeKeyC = "sk-test-cred-enc-cccc0003cccc"
)

func dataKey(b byte) []byte { return bytes.Repeat([]byte{b}, secrets.KeySize) }

// withKey sets the process's data key for one test and clears it after
// (the other tests of the package run without one).
func withKey(t *testing.T, key []byte) {
	t.Helper()
	if err := secrets.SetKey(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secrets.ResetForTest)
}

// logs captures slog output for one test.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLogs(t *testing.T) *logs {
	t.Helper()
	l := &logs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	warned.Range(func(k, _ any) bool { warned.Delete(k); return true })
	return l
}

func openAt(t *testing.T, path string) *sqlStore {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st.(*sqlStore)
}

func provider(id, key string) model.Provider {
	return model.Provider{ID: id, Name: "Provider " + id, Type: model.ProviderOpenAI, BaseURL: "https://llm.example.org",
		APIKey: key, MaxConcurrency: 4, RPM: 60, BalanceAPI: model.BalanceNone}
}

func rawKey(t *testing.T, st *sqlStore, id string) string {
	t.Helper()
	v, _, err := st.storedProviderKey(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func rawKV(t *testing.T, st *sqlStore, key string) string {
	t.Helper()
	var v string
	if err := st.db.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// With a data key, provider keys and the Colab credential are written
// sealed and read back plain; the plain text is in no database file.
func TestCredentialsSealedAtRest(t *testing.T) {
	withKey(t, dataKey(1))
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st := openAt(t, path)
	if err := st.InsertProvider(ctx, provider("p1", fakeKeyA)); err != nil {
		t.Fatal(err)
	}
	if raw := rawKey(t, st, "p1"); !secrets.IsSealed(raw) || strings.Contains(raw, fakeKeyA) {
		t.Fatalf("stored %q", raw)
	}
	if p, err := st.GetProvider(ctx, "p1"); err != nil || p.APIKey != fakeKeyA {
		t.Fatalf("GetProvider %q %v", p.APIKey, err)
	}
	if ps, err := st.ListProviders(ctx); err != nil || len(ps) != 1 || ps[0].APIKey != fakeKeyA {
		t.Fatalf("ListProviders %+v %v", ps, err)
	}
	cred := testCredential(testRefresh)
	if err := st.SetKV(ctx, "colab.credential", cred); err != nil {
		t.Fatal(err)
	}
	if raw := rawKV(t, st, "colab.credential"); !secrets.IsSealed(raw) {
		t.Fatalf("kv stored %q", raw)
	}
	if v, ok, err := st.GetKV(ctx, "colab.credential"); err != nil || !ok || v != cred {
		t.Fatalf("GetKV %q %v %v", v, ok, err)
	}
	if err := st.SetSecretKV(ctx, "colab.credential", cred); err != nil {
		t.Fatal(err)
	}
	if raw := rawKV(t, st, "colab.credential"); !secrets.IsSealed(raw) {
		t.Fatalf("SetSecretKV stored %q", raw)
	}
	// a value sealed already is not sealed twice
	pre, _ := secrets.SealFor(SealPurposeKV, "colab.credential", []byte("pre-sealed"))
	if err := st.SetKV(ctx, "colab.credential", pre); err != nil || rawKV(t, st, "colab.credential") != pre {
		t.Fatalf("pre-sealed value changed: %v", err)
	}
	// other kv rows are untouched
	if err := st.SetKV(ctx, "app.locale", "en"); err != nil || rawKV(t, st, "app.locale") != "en" {
		t.Fatal("a plain kv row was sealed")
	}
	if err := st.SetSecretKV(ctx, "other.secret", "as-is"); err != nil || rawKV(t, st, "other.secret") != "as-is" {
		t.Fatal("SetSecretKV of a key that is not sealed changed the value")
	}
	for _, needle := range []string{fakeKeyA, testRefresh, testSecret} {
		if got := fileHolds(t, path, needle); len(got) > 0 {
			t.Errorf("%q is in %v", needle, got)
		}
	}
	if n, err := st.SealStoredSecrets(ctx); err != nil || n.Changed() || n.Unreadable != 0 {
		t.Fatalf("nothing to seal: %+v %v", n, err)
	}
}

// The first start with a data key seals the plain credentials in place:
// afterwards neither the database nor its WAL holds them (also not the
// copies earlier writes left in free pages and old frames), and the next
// start changes nothing.
func TestSealStoredSecretsMigration(t *testing.T) {
	ctx := context.Background()
	secrets.ResetForTest()
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st := openAt(t, path)
	// written before this (no data key): plain, rewritten a few times
	for i, k := range []string{fakeKeyC, fakeKeyB, fakeKeyA} {
		p := provider("p1", k)
		if i == 0 {
			if err := st.InsertProvider(ctx, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := st.db.Exec(`UPDATE provider SET api_key = ? WHERE id = 'p1'`, k); err != nil { // the old plain UPDATE
			t.Fatal(err)
		}
	}
	if err := st.InsertProvider(ctx, provider("p2", fakeKeyB)); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertProvider(ctx, provider("local", "")); err != nil {
		t.Fatal(err)
	}
	churn(t, ctx, st, "colab.credential")
	if got := fileHolds(t, path, fakeKeyC); len(got) == 0 {
		t.Fatal("control: the replaced plain key left no copy; the scan proves nothing")
	}
	if got := fileHolds(t, path, testRefresh); len(got) == 0 {
		t.Fatal("control: no plain credential in the files")
	}

	l := captureLogs(t)
	withKey(t, dataKey(2))
	res, err := st.SealStoredSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sealed != 3 || res.Resealed != 0 || res.Unreadable != 0 {
		t.Fatalf("result %+v", res)
	}
	for _, needle := range []string{fakeKeyA, fakeKeyB, fakeKeyC, testRefresh, testSecret} {
		if got := fileHolds(t, path, needle); len(got) > 0 {
			t.Errorf("%q is still in %v", needle, got)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("the WAL was not truncated: %d bytes", fi.Size())
	}
	for id, want := range map[string]string{"p1": fakeKeyA, "p2": fakeKeyB, "local": ""} {
		if p, err := st.GetProvider(ctx, id); err != nil || p.APIKey != want {
			t.Errorf("%s: %q %v", id, p.APIKey, err)
		}
		if raw := rawKey(t, st, id); want != "" && !secrets.IsSealed(raw) {
			t.Errorf("%s stored %q", id, raw)
		}
	}
	if v, ok, _ := st.GetKV(ctx, "colab.credential"); !ok || !strings.Contains(v, testRefresh) {
		t.Errorf("credential %q", v)
	}
	if v, ok, _ := st.GetKV(ctx, "filler.2.39"); !ok || len(v) != 339 {
		t.Error("other rows changed")
	}
	before := rawKey(t, st, "p1")
	if res, err := st.SealStoredSecrets(ctx); err != nil || res.Changed() {
		t.Fatalf("second run: %+v %v", res, err)
	}
	if rawKey(t, st, "p1") != before {
		t.Fatal("the second run rewrote a sealed value")
	}
	assertNoSecretInLogs(t, l.String(), st, "p1", "p2")
}

func assertNoSecretInLogs(t *testing.T, out string, st *sqlStore, ids ...string) {
	t.Helper()
	for _, needle := range []string{fakeKeyA, fakeKeyB, fakeKeyC, testRefresh, testSecret, secrets.Prefix, secrets.PrefixV2} {
		if strings.Contains(out, needle) {
			t.Errorf("the log holds %q:\n%s", needle, out)
		}
	}
	for _, id := range ids {
		if raw := rawKey(t, st, id); raw != "" && strings.Contains(out, raw[len(raw)-12:]) {
			t.Errorf("the log holds the ciphertext of %s", id)
		}
	}
}

// Another installation's key (the data directory came from another
// computer or account): the provider reads without a key and is listed as
// unreadable, the Colab credential reads as absent; nothing is overwritten,
// so the right key opens them again. A new key replaces the value.
func TestUnreadableCredentials(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st := openAt(t, path)
	withKey(t, dataKey(3))
	for _, id := range []string{"p1", "p2"} {
		if err := st.InsertProvider(ctx, provider(id, fakeKeyA)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetSecretKV(ctx, "colab.credential", testCredential(testRefresh)); err != nil {
		t.Fatal(err)
	}
	sealedP1, sealedCred := rawKey(t, st, "p1"), rawKV(t, st, "colab.credential")

	l := captureLogs(t)
	withKey(t, dataKey(4)) // this computer's key
	p, err := st.GetProvider(ctx, "p1")
	if err != nil || p.APIKey != "" {
		t.Fatalf("an unreadable key reads as %q (%v): the ciphertext must never be handed out", p.APIKey, err)
	}
	ps, _ := st.ListProviders(ctx)
	for _, p := range ps {
		if p.APIKey != "" {
			t.Fatalf("ListProviders %s: %q", p.ID, p.APIKey)
		}
	}
	if v, ok, err := st.GetKV(ctx, "colab.credential"); err != nil || ok || v != "" {
		t.Fatalf("GetKV of an unreadable credential: %q %v %v", v, ok, err)
	}
	provs, kv, err := st.UnreadableSecrets(ctx)
	if err != nil || strings.Join(provs, ",") != "p1,p2" || strings.Join(kv, ",") != "colab.credential" {
		t.Fatalf("UnreadableSecrets %v %v %v", provs, kv, err)
	}
	if res, err := st.SealStoredSecrets(ctx); err != nil || res.Changed() || res.Unreadable != 3 {
		t.Fatalf("SealStoredSecrets %+v %v", res, err)
	}
	// another field changes, the key stays as stored
	p = provider("p1", "")
	p.RPM = 99
	if err := st.UpdateProviderKeepKey(ctx, p); err != nil {
		t.Fatal(err)
	}
	if rawKey(t, st, "p1") != sealedP1 || rawKV(t, st, "colab.credential") != sealedCred {
		t.Fatal("an unreadable value was overwritten")
	}
	if err := st.UpdateProviderKeepKey(ctx, provider("nope", "")); err != ErrNotFound {
		t.Fatalf("missing row: %v", err)
	}
	// the user enters the key again
	p.APIKey = fakeKeyB
	if err := st.UpdateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetProvider(ctx, "p1"); got.APIKey != fakeKeyB || got.RPM != 99 {
		t.Fatalf("re-entered: %+v", got)
	}
	if provs, _, _ := st.UnreadableSecrets(ctx); strings.Join(provs, ",") != "p2" {
		t.Fatalf("after re-entering: %v", provs)
	}
	if got := fileHolds(t, path, sealedP1[len(secrets.Prefix):len(secrets.Prefix)+24]); len(got) > 0 {
		t.Errorf("the replaced ciphertext is still in %v", got)
	}
	// the original key opens what was left alone
	withKey(t, dataKey(3))
	if got, _ := st.GetProvider(ctx, "p2"); got.APIKey != fakeKeyA {
		t.Fatalf("the original key does not open p2: %q", got.APIKey)
	}
	if v, ok, _ := st.GetKV(ctx, "colab.credential"); !ok || !strings.Contains(v, testRefresh) {
		t.Fatal("the original key does not open the credential")
	}
	out := l.String()
	if !strings.Contains(out, "cannot be decrypted") || strings.Count(out, "provider=p2") != 1 {
		t.Errorf("one warning per credential expected:\n%s", out)
	}
	assertNoSecretInLogs(t, out, st, "p2")
}

// Values sealed with an earlier key (a fallback) open, and the next start
// seals them with the current key.
func TestSealStoredSecretsReseals(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st := openAt(t, path)
	withKey(t, dataKey(5))
	if err := st.InsertProvider(ctx, provider("p1", fakeKeyA)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, "colab.credential", testCredential(testRefresh)); err != nil {
		t.Fatal(err)
	}
	// a row its owner sealed itself (ssh-mvp), and a plain row that is no credential
	ownSealed, _ := secrets.Seal([]byte("-----BEGIN OPENSSH PRIVATE KEY----- test"))
	if err := st.SetKV(ctx, "remote.ssh.key.s1", ownSealed); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, "app.locale", "en"); err != nil {
		t.Fatal(err)
	}
	old := rawKey(t, st, "p1")
	withKey(t, dataKey(6))
	if err := secrets.SetFallbackKeys(dataKey(5)); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != fakeKeyA {
		t.Fatalf("through the fallback: %q", p.APIKey)
	}
	res, err := st.SealStoredSecrets(ctx)
	if err != nil || res.Resealed != 3 || res.Sealed != 0 || res.Unreadable != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if b, err := secrets.OpenFor(SealPurposeKV, "remote.ssh.key.s1", rawKV(t, st, "remote.ssh.key.s1")); err != nil || !strings.HasPrefix(string(b), "-----BEGIN") {
		t.Fatalf("the owner-sealed row was not sealed again (bound to its row): %v", err)
	}
	if rawKV(t, st, "app.locale") != "en" {
		t.Fatal("a plain row was touched")
	}
	if b, err := secrets.OpenWithFor(dataKey(6), SealPurposeProviderKey, ProviderKeyBinding("p1", "https://llm.example.org"), rawKey(t, st, "p1")); err != nil || string(b) != fakeKeyA {
		t.Fatalf("not sealed with the current key: %v", err)
	}
	if got := fileHolds(t, path, old[len(secrets.Prefix):len(secrets.Prefix)+24]); len(got) > 0 {
		t.Errorf("the old ciphertext is still in %v", got)
	}
}

// SealSecrets on another database (the import's staging copy): values of
// the imported directory's key are sealed with this one; a database
// without the provider table is fine.
func TestSealSecretsExtraKeys(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "staged.db")
	src := openAt(t, path)
	withKey(t, dataKey(7))
	if err := src.InsertProvider(ctx, provider("p1", fakeKeyA)); err != nil {
		t.Fatal(err)
	}
	src.Close()
	withKey(t, dataKey(8))
	d, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	res, err := SealSecrets(ctx, d, dataKey(9), dataKey(7))
	if err != nil || res.Resealed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	var raw string
	if err := d.QueryRow(`SELECT api_key FROM provider WHERE id = 'p1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if b, cur, err := secrets.OpenAnyFor(SealPurposeProviderKey, ProviderKeyBinding("p1", "https://llm.example.org"), raw); err != nil || !cur || string(b) != fakeKeyA {
		t.Fatalf("resealed: %v", err)
	}
	empty, err := openRaw(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if res, err := SealSecrets(ctx, empty); err != nil || res.Changed() {
		t.Fatalf("no tables: %+v %v", res, err)
	}
	secrets.ResetForTest()
	if res, err := SealSecrets(ctx, d); err != nil || res.Changed() {
		t.Fatalf("no data key: %+v %v", res, err)
	}
}

// sec2-app L5: a changed or deleted API key leaves no copy in the files,
// with a data key (the old ciphertext) and without one (the old plain key).
func TestProviderKeyChangeLeavesNoCopy(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		t.Run(fmt.Sprintf("sealed=%v", sealed), func(t *testing.T) {
			ctx := context.Background()
			secrets.ResetForTest()
			if sealed {
				withKey(t, dataKey(10))
			}
			path := filepath.Join(t.TempDir(), "lectureflow.db")
			st := openAt(t, path)
			churn(t, ctx, st, "filler")
			if err := st.InsertProvider(ctx, provider("p1", fakeKeyA)); err != nil {
				t.Fatal(err)
			}
			if err := st.InsertProvider(ctx, provider("p2", fakeKeyC)); err != nil {
				t.Fatal(err)
			}
			needle := func(id string) string {
				raw := rawKey(t, st, id)
				if sealed {
					return raw[len(secrets.Prefix) : len(secrets.Prefix)+24]
				}
				return raw
			}
			n1, n2 := needle("p1"), needle("p2")
			// an unchanged key is not rewritten
			p := provider("p1", fakeKeyA)
			p.RPM = 120
			before := rawKey(t, st, "p1")
			if err := st.UpdateProvider(ctx, p); err != nil {
				t.Fatal(err)
			}
			if rawKey(t, st, "p1") != before {
				t.Fatal("an unchanged key was rewritten")
			}
			p.APIKey = fakeKeyB
			if err := st.UpdateProvider(ctx, p); err != nil {
				t.Fatal(err)
			}
			if got, _ := st.GetProvider(ctx, "p1"); got.APIKey != fakeKeyB || got.RPM != 120 {
				t.Fatalf("updated %+v", got)
			}
			if got := fileHolds(t, path, n1); len(got) > 0 {
				t.Errorf("the replaced key is still in %v", got)
			}
			if err := st.DeleteProvider(ctx, "p2"); err != nil {
				t.Fatal(err)
			}
			if got := fileHolds(t, path, n2); len(got) > 0 {
				t.Errorf("the deleted key is still in %v", got)
			}
			if err := st.DeleteProvider(ctx, "p2"); err != ErrNotFound {
				t.Fatalf("second delete: %v", err)
			}
			if err := st.UpdateProvider(ctx, provider("nope", fakeKeyA)); err != ErrNotFound {
				t.Fatalf("missing row: %v", err)
			}
			// inside a transaction: a plain write, then ScrubFreedSecrets
			n3 := needle("p1")
			if err := st.Tx(ctx, func(tx Store) error { return tx.DeleteProvider(ctx, "p1") }); err != nil {
				t.Fatal(err)
			}
			if err := st.ScrubFreedSecrets(ctx); err != nil {
				t.Fatal(err)
			}
			if got := fileHolds(t, path, n3); len(got) > 0 {
				t.Errorf("the key deleted in a transaction is still in %v", got)
			}
		})
	}
}

// ScrubSecrets (relocate: the replaced database in backups/, the old data
// directory): the Colab credential is deleted and every provider key
// emptied, leaving no copy.
func TestScrubSecrets(t *testing.T) {
	ctx := context.Background()
	secrets.ResetForTest() // plain values: the scan can see them
	path := filepath.Join(t.TempDir(), "backup.db")
	st := openAt(t, path)
	churn(t, ctx, st, "colab.credential")
	if err := st.InsertProvider(ctx, provider("p1", fakeKeyA)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, "remote.ssh.key.s1", secrets.Prefix+"c2VhbGVkLWJ5LWl0cy1vd25lcg"); err != nil { // sealed by its owner
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, "remote.ssh.key.s2", secrets.PrefixV2+"Ym91bmQtYnktaXRzLW93bmVy"); err != nil { // bound (sec3 L3)
		t.Fatal(err)
	}
	st.Close()
	d, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ScrubSecrets(ctx, d, SealedKVKeys()...); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM provider WHERE api_key <> ''`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("keys left: %d %v", n, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM provider`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("providers: %d %v", n, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM kv WHERE key LIKE 'remote.ssh.key.%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("an owner-sealed row was kept: %d %v", n, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM kv WHERE key LIKE 'filler.%'`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("plain rows were deleted: %d %v", n, err)
	}
	d.Close()
	for _, needle := range []string{fakeKeyA, testRefresh, testSecret} {
		if got := fileHolds(t, path, needle); len(got) > 0 {
			t.Errorf("%q is still in %v", needle, got)
		}
	}
	empty, err := openRaw(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if err := ScrubSecrets(ctx, empty, "colab.credential"); err != nil {
		t.Fatalf("no tables: %v", err)
	}
}

// sec3 I3: a secure write marks the database until its VACUUM and
// checkpoint are done; a start that finds the mark (the process was killed
// in between, or the VACUUM failed) runs the clean-up again.
func TestScrubPendingIsFinishedAtStart(t *testing.T) {
	ctx := context.Background()
	secrets.ResetForTest() // plain values: the scan can see them
	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st := openAt(t, path)
	if err := st.SetSecretKV(ctx, "colab.credential", testCredential(testRefresh)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetKV(ctx, kvScrubPending); ok {
		t.Fatal("the mark stayed after a finished secure write")
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("the WAL was not truncated: %d bytes", fi.Size())
	}
	// an interrupted secure replacement: the old key is still in free pages
	if err := st.InsertProvider(ctx, provider("p1", fakeKeyA)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE provider SET api_key = ? WHERE id = 'p1'`, fakeKeyB); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(ctx, kvScrubPending, "1"); err != nil {
		t.Fatal(err)
	}
	if got := fileHolds(t, path, fakeKeyA); len(got) == 0 {
		t.Fatal("control: the replaced key left no copy")
	}
	res, err := st.SealStoredSecrets(ctx)
	if err != nil || !res.Scrubbed || res.Changed() {
		t.Fatalf("%+v %v", res, err)
	}
	if got := fileHolds(t, path, fakeKeyA); len(got) > 0 {
		t.Errorf("the replaced key is still in %v", got)
	}
	if _, ok, _ := st.GetKV(ctx, kvScrubPending); ok {
		t.Fatal("the mark was not cleared")
	}
	if res, err := st.SealStoredSecrets(ctx); err != nil || res.Scrubbed {
		t.Fatalf("second start: %+v %v", res, err)
	}
}
