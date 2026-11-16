// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/colab"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

// Test values only (never real keys).
const (
	relocKeyA = "sk-test-relocate-aaaa-1111"
	relocKeyB = "sk-test-relocate-bbbb-2222"
)

func insertProvider(t *testing.T, d *sql.DB, id, key string) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO provider (id, name, type, base_url, api_key, max_concurrency, rpm, balance_api, created_at)
		VALUES (?, ?, 'openai', 'https://llm.example.org', ?, 4, 60, 'none', '2026-10-27T00:00:00Z')`, id, "P "+id, key); err != nil {
		t.Fatal(err)
	}
}

func providerKey(t *testing.T, path, id string) string {
	t.Helper()
	d, err := openDB(path, true)
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

// sec2-app L5: the database an import replaced, kept in backups/, keeps no
// provider key (nor the Colab credential) — not even in free pages or the
// WAL; its provider rows stay.
func TestApplyStagedScrubsProviderKeys(t *testing.T) {
	secrets.ResetForTest() // plain keys, as before cred-enc: the scan can see them
	ctx := context.Background()
	e := newImportEnv(t)
	d, err := db.OpenDB(filepath.Join(e.cur, DBName))
	if err != nil {
		t.Fatal(err)
	}
	insertProvider(t, d, "p1", relocKeyB)
	if _, err := d.Exec(`UPDATE provider SET api_key = ? WHERE id = 'p1'`, relocKeyA); err != nil { // an older copy too
		t.Fatal(err)
	}
	d.Close()
	if !slices.Contains(secretKVKeys, colab.KVCredential) {
		t.Fatalf("secretKVKeys %v", secretKVKeys)
	}
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyStaged(ctx, e.cur); err != nil {
		t.Fatal(err)
	}
	baks, _ := filepath.Glob(filepath.Join(e.cur, "backups", "lectureflow-before-import-*.db"))
	if len(baks) != 1 {
		t.Fatalf("backups %v", baks)
	}
	if got := rawHolds(t, baks[0], relocKeyA, relocKeyB); len(got) > 0 {
		t.Errorf("the backup still holds %v", got)
	}
	if v := providerKey(t, baks[0], "p1"); v != "" {
		t.Errorf("backup provider key %q", v)
	}
}

// A move leaves no provider key in the old directory's database.
func TestScrubOldDataDirClearsProviderKeys(t *testing.T) {
	secrets.ResetForTest()
	ctx := context.Background()
	e := newMoveEnv(t)
	insertProvider(t, e.fx.db, "p1", relocKeyA)
	if err := MoveDataDir(ctx, e.opts()); err != nil {
		t.Fatal(err)
	}
	e.fx.db.Close()
	oldDB := filepath.Join(e.src, DBName)
	if got := rawHolds(t, oldDB, relocKeyA); len(got) == 0 {
		t.Fatal("control: the old database does not hold the key before the scrub")
	}
	if err := ScrubOldDataDir(ctx, e.src, e.dst); err != nil {
		t.Fatal(err)
	}
	if got := rawHolds(t, oldDB, relocKeyA); len(got) > 0 {
		t.Errorf("the old directory still holds %v", got)
	}
	if v := providerKey(t, filepath.Join(e.dst, DBName), "p1"); v != relocKeyA {
		t.Errorf("the new database lost the key: %q", v)
	}
}

// The data key file moves with the data: a move copies it, so the moved
// database's sealed values still open.
func TestMoveCarriesDataKey(t *testing.T) {
	secrets.ResetForTest()
	t.Cleanup(secrets.ResetForTest)
	ctx := context.Background()
	e := newMoveEnv(t)
	if _, err := secrets.Setup(e.src, nil); err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.Seal([]byte(relocKeyA))
	if err != nil {
		t.Fatal(err)
	}
	insertProvider(t, e.fx.db, "p1", sealed)
	if err := MoveDataDir(ctx, e.opts()); err != nil {
		t.Fatal(err)
	}
	secrets.ResetForTest()
	if st, err := secrets.Setup(e.dst, nil); err != nil || st.Created || st.Replaced != "" {
		t.Fatalf("the moved key file: %+v %v", st, err)
	}
	if b, _, err := secrets.OpenAny(providerKey(t, filepath.Join(e.dst, DBName), "p1")); err != nil || string(b) != relocKeyA {
		t.Fatalf("the moved value does not open: %v", err)
	}
}

// An import seals the imported credentials with this installation's key:
// plain ones (an older install), and ones sealed with the imported
// directory's key when that key opens here, in 3.2.0's unbound format or
// bound (sec3 L3); every value it writes is bound to its row. Ones no key
// opens are kept as they are ("需要重新输入").
func TestStageImportResealsCredentials(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(secrets.ResetForTest)
	const base = "https://llm.example.org" // insertProvider's
	for _, tc := range []struct {
		name    string
		srcKey  bool // the source has its own key file
		keepKey bool // ... and it stays readable (same account)
		bound   bool // ... and its values are bound (enc:v2:), else 3.2.0's enc:v1:
	}{{"plain", false, false, false}, {"same account", true, true, false}, {"same account, bound", true, true, true},
		{"other computer", true, false, false}, {"other computer, bound", true, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			secrets.ResetForTest()
			e := newImportEnv(t)
			d, err := db.OpenDB(filepath.Join(e.src, DBName))
			if err != nil {
				t.Fatal(err)
			}
			cred := fmt.Sprintf(`{"refresh_token":%q}`, "1//test-refresh-reseal-77")
			stored, storedCred := relocKeyA, cred
			if tc.srcKey {
				if _, err := secrets.Setup(e.src, nil); err != nil {
					t.Fatal(err)
				}
				if tc.bound {
					stored, _ = secrets.SealFor(db.SealPurposeProviderKey, db.ProviderKeyBinding("p1", base), []byte(relocKeyA))
					storedCred, _ = secrets.SealFor(db.SealPurposeKV, colab.KVCredential, []byte(cred))
				} else {
					stored, _ = secrets.Seal([]byte(relocKeyA))
					storedCred, _ = secrets.Seal([]byte(cred))
				}
				if !tc.keepKey {
					os.Remove(filepath.Join(e.src, secrets.KeyFileName))
				}
			}
			insertProvider(t, d, "p1", stored)
			if _, err := d.Exec(`INSERT INTO kv (key, value) VALUES (?, ?)`, colab.KVCredential, storedCred); err != nil {
				t.Fatal(err)
			}
			d.Close()

			secrets.ResetForTest()
			if _, err := secrets.Setup(e.cur, nil); err != nil { // this installation
				t.Fatal(err)
			}
			if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
				t.Fatal(err)
			}
			staged := filepath.Join(e.cur, StagingDir, DBName)
			got := providerKey(t, staged, "p1")
			if !tc.srcKey || tc.keepKey {
				if b, cur, err := secrets.OpenAnyFor(db.SealPurposeProviderKey, db.ProviderKeyBinding("p1", base), got); err != nil || !cur || string(b) != relocKeyA {
					t.Fatalf("staged key not sealed (bound) with this installation's key: %v %v", cur, err)
				}
				for _, sfx := range []string{"", "-wal", "-journal"} {
					if b, err := os.ReadFile(staged + sfx); err == nil && bytes.Contains(b, []byte(relocKeyA)) {
						t.Errorf("the plain key is in the staged %s", filepath.Base(staged+sfx))
					}
				}
			} else if got != stored {
				t.Fatalf("an unreadable key was changed: %q", got)
			}
			if err := ApplyStaged(ctx, e.cur); err != nil {
				t.Fatal(err)
			}
			st, err := db.Open(filepath.Join(e.cur, DBName))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			p, err := st.GetProvider(ctx, "p1")
			if err != nil {
				t.Fatal(err)
			}
			v, ok, _ := st.GetKV(ctx, colab.KVCredential)
			if !tc.srcKey || tc.keepKey {
				if p.APIKey != relocKeyA || !ok || v != cred {
					t.Fatalf("imported credentials: %q %v %q", p.APIKey, ok, v)
				}
			} else {
				ids, kv, err := st.(db.SecretInspector).UnreadableSecrets(ctx)
				if p.APIKey != "" || ok || err != nil || strings.Join(ids, ",") != "p1" || strings.Join(kv, ",") != colab.KVCredential {
					t.Fatalf("unreadable import: %q %v %v %v %v", p.APIKey, ok, ids, kv, err)
				}
			}
			if o, _, _ := st.GetKV(ctx, model.KVOnboardingState); !strings.Contains(o, "welcome") {
				t.Errorf("onboarding not carried: %q", o)
			}
		})
	}
}

// makeOldBackup writes, as 3.1 left it, a database an import replaced:
// the Colab credential removed, a provider key in the clear.
func makeOldBackup(t *testing.T, dataDir, name, key string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	d, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	insertProvider(t, d, "p1", relocKeyB)
	if _, err := d.Exec(`UPDATE provider SET api_key = ? WHERE id = 'p1'`, key); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// sec3 L1: the databases earlier imports replaced lose their provider keys;
// other backups (the lead's stage copies, say) are left alone.
func TestScrubImportBackups(t *testing.T) {
	secrets.ResetForTest()
	ctx := context.Background()
	dir := t.TempDir()
	if n, err := ScrubImportBackups(ctx, dir); err != nil || n != 0 {
		t.Fatalf("no backups/: %d %v", n, err)
	}
	a := makeOldBackup(t, dir, "lectureflow-before-import-20261001-120000.db", relocKeyA)
	b := makeOldBackup(t, dir, "lectureflow-before-import-20261002-120000.db", relocKeyA)
	other := makeOldBackup(t, dir, "lectureflow-stage-v3.db", relocKeyA)
	if got := rawHolds(t, a, relocKeyA, relocKeyB); len(got) == 0 {
		t.Fatal("control: the backup does not hold the key")
	}
	n, err := ScrubImportBackups(ctx, dir)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	for _, p := range []string{a, b} {
		if got := rawHolds(t, p, relocKeyA, relocKeyB); len(got) > 0 {
			t.Errorf("%s still holds %v", filepath.Base(p), got)
		}
		if v := providerKey(t, p, "p1"); v != "" {
			t.Errorf("%s: key %q", filepath.Base(p), v)
		}
	}
	if v := providerKey(t, other, "p1"); v != relocKeyA {
		t.Errorf("another backup was changed: %q", v)
	}
	// again: nothing to find, nothing fails
	if n, err := ScrubImportBackups(ctx, dir); err != nil || n != 2 {
		t.Fatalf("second run: %d %v", n, err)
	}
}

// A move: the old directory's backups/ is scrubbed with its database.
func TestScrubOldDataDirScrubsBackups(t *testing.T) {
	secrets.ResetForTest()
	ctx := context.Background()
	e := newMoveEnv(t)
	bak := makeOldBackup(t, e.src, "lectureflow-before-import-20261001-120000.db", relocKeyA)
	if err := MoveDataDir(ctx, e.opts()); err != nil {
		t.Fatal(err)
	}
	e.fx.db.Close()
	if !exists(filepath.Join(e.dst, "backups", filepath.Base(bak))) {
		t.Fatal("the move did not take backups/ along")
	}
	if err := ScrubOldDataDir(ctx, e.src, e.dst); err != nil {
		t.Fatal(err)
	}
	if got := rawHolds(t, bak, relocKeyA); len(got) > 0 {
		t.Errorf("the old backup still holds %v", got)
	}
	// without the old database, its backups are still scrubbed
	os.Remove(filepath.Join(e.src, DBName))
	bak2 := makeOldBackup(t, e.src, "lectureflow-before-import-20261003-120000.db", relocKeyA)
	if err := ScrubOldDataDir(ctx, e.src, e.dst); err != nil {
		t.Fatal(err)
	}
	if got := rawHolds(t, bak2, relocKeyA); len(got) > 0 {
		t.Errorf("the second backup still holds %v", got)
	}
}
