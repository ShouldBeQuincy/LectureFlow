// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

// Values bound to their purpose and target (sec3 L3, siblings of the
// frozen API). A Seal value's additional data is only its prefix, so any
// ciphertext of an installation opens wherever it is put: in another
// provider's row, in place of the Colab credential, or under a provider
// whose base_url was changed to another host. SealFor binds the ciphertext
// to a purpose (what the value is, e.g. "provider.api_key") and a binding
// (where it belongs, e.g. the provider's id and the origin of its
// base_url): the AES-GCM additional data is
//
//	PrefixV2 + purpose + "\x00" + bind
//
// (a purpose never holds NUL, so the split is unambiguous), and OpenFor
// opens it only with the same purpose and binding. A value moved elsewhere,
// or one whose binding changed, fails exactly like one sealed with another
// key: ErrUndecryptable, "enter it again".
//
// Seal values (Prefix, "enc:v1:") carry no binding; OpenAnyFor still opens
// them (current false) so the caller seals them again bound, which the
// database does once at the next start (db.SealSecrets).

// PrefixV2 marks a value SealFor produced.
const PrefixV2 = "enc:v2:"

// errBound: a SealFor value given to a function of the unbound format
// (Open, OpenAny, OpenWith).
var errBound = errors.New("secrets: a bound value (enc:v2:) opens only with its purpose and binding")

// errPurpose: an empty purpose or one holding NUL.
var errPurpose = errors.New("secrets: invalid purpose")

// Prefixes lists the prefixes of sealed values (IsSealed), oldest first.
func Prefixes() []string { return []string{Prefix, PrefixV2} }

// IsBound reports whether s is a value SealFor produced.
func IsBound(s string) bool { return strings.HasPrefix(s, PrefixV2) }

// boundAAD is the additional data of a SealFor value.
func boundAAD(purpose, bind string) ([]byte, error) {
	if purpose == "" || strings.ContainsRune(purpose, 0) {
		return nil, errPurpose
	}
	return []byte(PrefixV2 + purpose + "\x00" + bind), nil
}

// SealFor encrypts plain with the data key, bound to purpose and bind.
func SealFor(purpose, bind string, plain []byte) (string, error) {
	aad, err := boundAAD(purpose, bind)
	if err != nil {
		return "", err
	}
	mu.RLock()
	g := aead
	mu.RUnlock()
	if g == nil {
		return "", ErrNoKey
	}
	nonce := make([]byte, g.NonceSize(), g.NonceSize()+len(plain)+g.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := g.Seal(nonce, nonce, plain, aad)
	return PrefixV2 + base64.RawStdEncoding.EncodeToString(out), nil
}

// OpenFor decrypts a SealFor value with the data key; purpose and bind
// must be the ones it was sealed with (ErrUndecryptable otherwise, as for
// another key). A Seal value is refused: it has no binding to check
// (OpenAnyFor opens it for resealing). ErrNoKey without a data key.
func OpenFor(purpose, bind, sealed string) ([]byte, error) {
	if !IsBound(sealed) {
		return nil, errors.New("secrets: not a bound sealed value")
	}
	aad, err := boundAAD(purpose, bind)
	if err != nil {
		return nil, err
	}
	mu.RLock()
	g := aead
	mu.RUnlock()
	if g == nil {
		return nil, ErrNoKey
	}
	return openAAD(g, sealed[len(PrefixV2):], aad)
}

// OpenAnyFor is OpenFor that also tries the fallback keys
// (SetFallbackKeys), and opens a Seal value (no binding: Open's rules,
// then the fallback keys) as well. current is true only for a SealFor
// value the data key opened; otherwise (a fallback key, or the unbound
// format) the caller seals the value again with SealFor. ErrNoKey without
// a data key; ErrUndecryptable when nothing opens it.
func OpenAnyFor(purpose, bind, sealed string) (plain []byte, current bool, err error) {
	if !IsBound(sealed) {
		p, _, err := OpenAny(sealed)
		return p, false, err
	}
	aad, err := boundAAD(purpose, bind)
	if err != nil {
		return nil, false, err
	}
	mu.RLock()
	g, fb := aead, fallbacks
	mu.RUnlock()
	if g == nil {
		return nil, false, ErrNoKey
	}
	body := sealed[len(PrefixV2):]
	if p, err := openAAD(g, body, aad); err == nil {
		return p, true, nil
	}
	for _, f := range fb {
		if p, err := openAAD(f, body, aad); err == nil {
			return p, false, nil
		}
	}
	return nil, false, ErrUndecryptable
}

// OpenWithFor decrypts a value with key instead of the process's data key
// (the import of another data directory): a SealFor value with purpose and
// bind, a Seal value as OpenWith.
func OpenWithFor(key []byte, purpose, bind, sealed string) ([]byte, error) {
	if !IsBound(sealed) {
		return OpenWith(key, sealed)
	}
	aad, err := boundAAD(purpose, bind)
	if err != nil {
		return nil, err
	}
	g, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return openAAD(g, sealed[len(PrefixV2):], aad)
}
