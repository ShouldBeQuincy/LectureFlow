// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package secrets

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const (
	testPurpose = "provider.api_key"
	testBind    = "deepseek\x00https://api.deepseek.com:443"
)

// sec3 L3: a SealFor value opens with its purpose and binding only.
func TestSealForRoundTripAndBinding(t *testing.T) {
	reset(t)
	if _, err := SealFor(testPurpose, testBind, []byte(fakeAPIKey)); !errors.Is(err, ErrNoKey) {
		t.Fatalf("SealFor without a key: %v", err)
	}
	if err := SetKey(testKey(1)); err != nil {
		t.Fatal(err)
	}
	a, err := SealFor(testPurpose, testBind, []byte(fakeAPIKey))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := SealFor(testPurpose, testBind, []byte(fakeAPIKey))
	if !strings.HasPrefix(a, PrefixV2) || !IsBound(a) || !IsSealed(a) || a == b || strings.Contains(a, fakeAPIKey) {
		t.Fatalf("sealed %q / %q", a, b)
	}
	for _, s := range []string{a, b} {
		if p, err := OpenFor(testPurpose, testBind, s); err != nil || string(p) != fakeAPIKey {
			t.Fatalf("OpenFor: %q %v", p, err)
		}
		if p, cur, err := OpenAnyFor(testPurpose, testBind, s); err != nil || !cur || string(p) != fakeAPIKey {
			t.Fatalf("OpenAnyFor: %q %v %v", p, cur, err)
		}
	}

	// another purpose or another binding: as if sealed with another key
	for _, tc := range []struct{ purpose, bind string }{
		{"kv", testBind}, // moved into a kv row
		{testPurpose, "other\x00https://api.deepseek.com:443"}, // another provider's row
		{testPurpose, "deepseek\x00https://evil.example:443"},  // base_url moved to another host
		{testPurpose, "deepseek"},                              // binding cut short
		{testPurpose, testBind + "\x00"},                       // binding extended
	} {
		if _, err := OpenFor(tc.purpose, tc.bind, a); !errors.Is(err, ErrUndecryptable) {
			t.Errorf("OpenFor(%q, %q): %v, want ErrUndecryptable", tc.purpose, tc.bind, err)
		}
		if _, _, err := OpenAnyFor(tc.purpose, tc.bind, a); !errors.Is(err, ErrUndecryptable) {
			t.Errorf("OpenAnyFor(%q, %q): %v, want ErrUndecryptable", tc.purpose, tc.bind, err)
		}
	}
	// the purpose / binding split cannot be moved: a purpose holding NUL is
	// refused outright
	if _, err := OpenFor(testPurpose+"\x00deepseek", "https://api.deepseek.com:443", a); err == nil {
		t.Error("the split moved into the purpose opened it")
	}

	// any change of the ciphertext
	raw, _ := base64.RawStdEncoding.DecodeString(a[len(PrefixV2):])
	for i := range raw {
		c := append([]byte(nil), raw...)
		c[i] ^= 0x01
		if _, err := OpenFor(testPurpose, testBind, PrefixV2+base64.RawStdEncoding.EncodeToString(c)); !errors.Is(err, ErrUndecryptable) {
			t.Fatalf("byte %d changed: %v", i, err)
		}
	}
	// the unbound prefix on a bound body does not open either
	if _, err := Open(Prefix + a[len(PrefixV2):]); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("v2 body under the v1 prefix: %v", err)
	}

	// another data key
	if err := SetKey(testKey(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFor(testPurpose, testBind, a); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("another key: %v", err)
	}
}

// The purpose must not be empty or hold NUL (the additional data's split).
func TestSealForPurposeChecked(t *testing.T) {
	reset(t)
	if err := SetKey(testKey(1)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "a\x00b"} {
		if _, err := SealFor(p, "x", []byte("v")); err == nil {
			t.Errorf("SealFor(%q) accepted", p)
		}
	}
	s, _ := SealFor("kv", "", nil) // an empty binding and value are fine
	if p, err := OpenFor("kv", "", s); err != nil || len(p) != 0 {
		t.Fatalf("empty: %q %v", p, err)
	}
	if _, err := OpenFor("", "", s); err == nil {
		t.Fatal("OpenFor with an empty purpose")
	}
}

// The two formats do not mix: the unbound functions refuse a bound value,
// OpenFor refuses an unbound one, OpenAnyFor opens an unbound value for
// resealing (current false).
func TestBoundAndUnboundFormats(t *testing.T) {
	reset(t)
	if err := SetKey(testKey(3)); err != nil {
		t.Fatal(err)
	}
	v1, _ := Seal([]byte(fakeAPIKey))
	v2, _ := SealFor(testPurpose, testBind, []byte(fakeAPIKey))
	if IsBound(v1) || !IsSealed(v1) || !IsSealed(v2) || IsSealed(fakeAPIKey) {
		t.Fatal("IsSealed / IsBound")
	}
	if got := Prefixes(); len(got) != 2 || got[0] != Prefix || got[1] != PrefixV2 || len(Prefix) != len(PrefixV2) {
		t.Fatalf("Prefixes = %q", got)
	}
	if _, err := Open(v2); err == nil {
		t.Fatal("Open opened a bound value")
	}
	if _, _, err := OpenAny(v2); err == nil {
		t.Fatal("OpenAny opened a bound value")
	}
	if _, err := OpenWith(testKey(3), v2); err == nil {
		t.Fatal("OpenWith opened a bound value")
	}
	if _, err := OpenFor(testPurpose, testBind, v1); err == nil {
		t.Fatal("OpenFor opened an unbound value")
	}
	if p, cur, err := OpenAnyFor(testPurpose, testBind, v1); err != nil || cur || string(p) != fakeAPIKey {
		t.Fatalf("OpenAnyFor of an unbound value: %q %v %v", p, cur, err)
	}
	if _, _, err := OpenAnyFor(testPurpose, testBind, fakeAPIKey); err == nil {
		t.Fatal("OpenAnyFor of a plain value")
	}
}

// Fallback keys and OpenWithFor (an imported data directory's key).
func TestBoundFallbackAndOpenWith(t *testing.T) {
	reset(t)
	if err := SetKey(testKey(4)); err != nil {
		t.Fatal(err)
	}
	old, _ := SealFor(testPurpose, testBind, []byte(fakeAPIKey))
	oldV1, _ := Seal([]byte(fakeAPIKey))
	if err := SetKey(testKey(5)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenAnyFor(testPurpose, testBind, old); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("no fallback: %v", err)
	}
	if err := SetFallbackKeys(testKey(4)); err != nil {
		t.Fatal(err)
	}
	if p, cur, err := OpenAnyFor(testPurpose, testBind, old); err != nil || cur || string(p) != fakeAPIKey {
		t.Fatalf("fallback: %q %v %v", p, cur, err)
	}
	if _, _, err := OpenAnyFor(testPurpose, "other", old); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("fallback key, other binding: %v", err)
	}
	if p, cur, err := OpenAnyFor(testPurpose, testBind, oldV1); err != nil || cur || string(p) != fakeAPIKey {
		t.Fatalf("fallback, unbound: %q %v %v", p, cur, err)
	}
	if err := SetFallbackKeys(); err != nil {
		t.Fatal(err)
	}
	if p, err := OpenWithFor(testKey(4), testPurpose, testBind, old); err != nil || string(p) != fakeAPIKey {
		t.Fatalf("OpenWithFor: %q %v", p, err)
	}
	if _, err := OpenWithFor(testKey(4), "kv", testBind, old); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("OpenWithFor, other purpose: %v", err)
	}
	if p, err := OpenWithFor(testKey(4), testPurpose, testBind, oldV1); err != nil || string(p) != fakeAPIKey {
		t.Fatalf("OpenWithFor, unbound: %q %v", p, err)
	}
	if _, err := OpenWithFor(testKey(5), testPurpose, testBind, old); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("OpenWithFor, wrong key: %v", err)
	}
}
