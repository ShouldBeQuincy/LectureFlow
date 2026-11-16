// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/secrets"
)

// SEC8 low-4, R15 suggestion 1: a way back through 3.2.0 nests the bound
// values. 3.2.0 does not know enc:v2: and seals it as plain text
// (enc:v1:(enc:v2:…)); the first start after it bound that as it was
// (enc:v2:(enc:v2:…)). Fake credentials only.

func mustSeal(t *testing.T, plain string) string {
	t.Helper()
	v, err := secrets.Seal([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustSealFor(t *testing.T, purpose, bind, plain string) string {
	t.Helper()
	v, err := secrets.SealFor(purpose, bind, []byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The inner credential is read (never the sealed text), and the next start
// seals it once, bound to its row; the start after that changes nothing.
func TestNestedValuesOpenToTheInnerCredential(t *testing.T) {
	ctx := context.Background()
	withKey(t, dataKey(31))
	st := openAt(t, filepath.Join(t.TempDir(), "lectureflow.db"))
	base := provider("p1", "").BaseURL
	// p1: what 3.2.0 left (v1 around v2); p2: what the start then made of
	// it before this fix (v2 around v2); the Colab credential: v1 around v2
	p1 := mustSeal(t, mustSealFor(t, SealPurposeProviderKey, ProviderKeyBinding("p1", base), fakeKeyA))
	p2 := mustSealFor(t, SealPurposeProviderKey, ProviderKeyBinding("p2", base),
		mustSealFor(t, SealPurposeProviderKey, ProviderKeyBinding("p2", base), fakeKeyB))
	cred := mustSeal(t, mustSealFor(t, SealPurposeKV, "colab.credential", testCredential(testRefresh)))
	for id, v := range map[string]string{"p1": p1, "p2": p2} {
		if err := st.InsertProvider(ctx, provider(id, v)); err != nil { // stored as it is (sealed already)
			t.Fatal(err)
		}
	}
	if err := st.SetKV(ctx, "colab.credential", cred); err != nil {
		t.Fatal(err)
	}
	l := captureLogs(t)

	check := func(when string) {
		t.Helper()
		for id, want := range map[string]string{"p1": fakeKeyA, "p2": fakeKeyB} {
			if p, err := st.GetProvider(ctx, id); err != nil || p.APIKey != want {
				t.Fatalf("%s: %s reads %q (%v)", when, id, p.APIKey, err)
			}
		}
		if v, ok, _ := st.GetKV(ctx, "colab.credential"); !ok || v != testCredential(testRefresh) {
			t.Fatalf("%s: the credential reads %q", when, v)
		}
		if provs, kv, err := st.UnreadableSecrets(ctx); err != nil || len(provs)+len(kv) != 0 {
			t.Fatalf("%s: UnreadableSecrets %v %v %v", when, provs, kv, err)
		}
	}
	check("before the start")

	res, err := st.SealStoredSecrets(ctx)
	if err != nil || res.Resealed != 3 || res.Sealed != 0 || res.Bound != 0 || res.Unreadable != 0 {
		t.Fatalf("start: %+v %v", res, err)
	}
	check("after the start")
	// one layer now, bound to its row
	for id, want := range map[string]string{"p1": fakeKeyA, "p2": fakeKeyB} {
		b, err := secrets.OpenFor(SealPurposeProviderKey, ProviderKeyBinding(id, base), rawKey(t, st, id))
		if err != nil || string(b) != want {
			t.Fatalf("%s is not sealed once around its key: %v", id, err)
		}
	}
	if b, err := secrets.OpenFor(SealPurposeKV, "colab.credential", rawKV(t, st, "colab.credential")); err != nil || string(b) != testCredential(testRefresh) {
		t.Fatalf("the credential is not sealed once: %v", err)
	}
	before := rawKey(t, st, "p1")
	if res, err := st.SealStoredSecrets(ctx); err != nil || res.Changed() {
		t.Fatalf("second start: %+v %v", res, err)
	}
	if rawKey(t, st, "p1") != before {
		t.Fatal("the second start rewrote p1")
	}
	assertNoSecretInLogs(t, l.String(), st, "p1", "p2")
}

// A nested value whose inner layer does not open (another binding, another
// key) or opens to sealed text again (one layer only) is unreadable: it
// reads as no key (never as the sealed text), is listed as unreadable, is
// left as it is by the start, and an empty key does not remove it (I4).
func TestNestedValueThatDoesNotOpenIsUnreadable(t *testing.T) {
	ctx := context.Background()
	withKey(t, dataKey(32))
	base := provider("p1", "").BaseURL
	bind := ProviderKeyBinding("p1", base)
	otherKey := func() string {
		if err := secrets.SetKey(dataKey(33)); err != nil {
			t.Fatal(err)
		}
		v := mustSealFor(t, SealPurposeProviderKey, bind, fakeKeyA)
		if err := secrets.SetKey(dataKey(32)); err != nil {
			t.Fatal(err)
		}
		return v
	}()
	for _, tc := range []struct{ name, stored string }{
		{"inner bound to another provider", mustSeal(t, mustSealFor(t, SealPurposeProviderKey, ProviderKeyBinding("p9", base), fakeKeyA))},
		{"inner sealed for the kv purpose", mustSeal(t, mustSealFor(t, SealPurposeKV, bind, fakeKeyA))},
		{"inner of another key", mustSeal(t, otherKey)},
		{"two layers inside", mustSeal(t, mustSealFor(t, SealPurposeProviderKey, bind, mustSealFor(t, SealPurposeProviderKey, bind, fakeKeyA)))},
		{"v2 around two layers", mustSealFor(t, SealPurposeProviderKey, bind,
			mustSealFor(t, SealPurposeProviderKey, bind, mustSeal(t, fakeKeyA)))},
		{"inner not a sealed value", mustSeal(t, secrets.PrefixV2+"bm90LWEtc2VhbGVkLXZhbHVl")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openAt(t, filepath.Join(t.TempDir(), "lectureflow.db"))
			if err := st.InsertProvider(ctx, provider("p1", tc.stored)); err != nil {
				t.Fatal(err)
			}
			captureLogs(t)
			read := func(when string) {
				t.Helper()
				p, err := st.GetProvider(ctx, "p1")
				if err != nil || p.APIKey != "" {
					t.Fatalf("%s: p1 reads %q (%v)", when, p.APIKey, err)
				}
				if provs, _, _ := st.UnreadableSecrets(ctx); strings.Join(provs, ",") != "p1" {
					t.Fatalf("%s: UnreadableSecrets %v", when, provs)
				}
				if rawKey(t, st, "p1") != tc.stored {
					t.Fatalf("%s: the stored value changed", when)
				}
			}
			read("before the start")
			if res, err := st.SealStoredSecrets(ctx); err != nil || res.Changed() || res.Unreadable != 1 {
				t.Fatalf("start: %+v %v", res, err)
			}
			read("after the start")
			p, _ := st.GetProvider(ctx, "p1")
			p.RPM = 30
			if err := st.UpdateProvider(ctx, p); err != nil {
				t.Fatal(err)
			}
			read("after an update without a key")
		})
	}
}

// The import of another data directory: a nested value of its key opens
// with that key (inner layer too) and is sealed once with this one.
func TestNestedValueOfAnImportedDirectory(t *testing.T) {
	ctx := context.Background()
	theirs := dataKey(35)
	withKey(t, theirs)
	base := provider("p1", "").BaseURL
	nested := mustSeal(t, mustSealFor(t, SealPurposeProviderKey, ProviderKeyBinding("p1", base), fakeKeyC))
	if err := secrets.SetKey(dataKey(34)); err != nil {
		t.Fatal(err)
	}
	st := openAt(t, filepath.Join(t.TempDir(), "lectureflow.db"))
	if err := st.InsertProvider(ctx, provider("p1", nested)); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != "" {
		t.Fatalf("another directory's value opened with this key: %q", p.APIKey)
	}
	res, err := SealSecrets(ctx, st.db, theirs)
	if err != nil || res.Resealed != 1 || res.Unreadable != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if p, _ := st.GetProvider(ctx, "p1"); p.APIKey != fakeKeyC {
		t.Fatalf("p1 reads %q", p.APIKey)
	}
	if b, err := secrets.OpenFor(SealPurposeProviderKey, ProviderKeyBinding("p1", base), rawKey(t, st, "p1")); err != nil || string(b) != fakeKeyC {
		t.Fatalf("not sealed once with this key: %v", err)
	}
}
