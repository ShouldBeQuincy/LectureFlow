// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
)

// Earlier data keys (cred-enc, siblings of the frozen API). A value sealed
// with one of them still opens with OpenAny, and the database layer seals
// it again with the current key (db.SealSecrets): the macOS desktop app
// taking over a data directory whose key file web mode made, or an import
// of another data directory of the same account (OpenWith).

var fallbacks []cipher.AEAD // guarded by mu

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, errors.New("secrets: the data key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SetFallbackKeys sets the earlier keys OpenAny also tries (none clears
// them). The data key itself is set with SetKey.
func SetFallbackKeys(keys ...[]byte) error {
	gs := make([]cipher.AEAD, 0, len(keys))
	for _, k := range keys {
		g, err := newAEAD(k)
		if err != nil {
			return err
		}
		gs = append(gs, g)
	}
	mu.Lock()
	fallbacks = gs
	mu.Unlock()
	return nil
}

// openWith decrypts sealed (a Seal value: Prefix) with g.
func openWith(g cipher.AEAD, sealed string) ([]byte, error) {
	return openAAD(g, sealed[len(Prefix):], []byte(Prefix))
}

// openAAD decrypts body (base64 of nonce ‖ ciphertext, the part after the
// prefix) with g and the additional data aad.
func openAAD(g cipher.AEAD, body string, aad []byte) ([]byte, error) {
	raw, err := base64.RawStdEncoding.DecodeString(body)
	if err != nil || len(raw) < g.NonceSize()+g.Overhead() {
		return nil, ErrUndecryptable
	}
	plain, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], aad)
	if err != nil {
		return nil, ErrUndecryptable
	}
	return plain, nil
}

// OpenAny decrypts a Seal value with the data key or, failing that, with
// a fallback key (SetFallbackKeys); a SealFor value is refused (errBound,
// see OpenAnyFor). current is false when only a fallback
// key opened it: the caller seals it again. ErrNoKey without a data key;
// ErrUndecryptable when no key opens it.
func OpenAny(sealed string) (plain []byte, current bool, err error) {
	if IsBound(sealed) {
		return nil, false, errBound
	}
	if !IsSealed(sealed) {
		return nil, false, errors.New("secrets: not a sealed value")
	}
	mu.RLock()
	g, fb := aead, fallbacks
	mu.RUnlock()
	if g == nil {
		return nil, false, ErrNoKey
	}
	if p, err := openWith(g, sealed); err == nil {
		return p, true, nil
	}
	for _, f := range fb {
		if p, err := openWith(f, sealed); err == nil {
			return p, false, nil
		}
	}
	return nil, false, ErrUndecryptable
}

// OpenWith decrypts a Seal value with key instead of the process's data
// key (a SealFor value: OpenWithFor) (the import of another data directory: its values are sealed again
// with this installation's key).
func OpenWith(key []byte, sealed string) ([]byte, error) {
	if IsBound(sealed) {
		return nil, errBound
	}
	if !IsSealed(sealed) {
		return nil, errors.New("secrets: not a sealed value")
	}
	g, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return openWith(g, sealed)
}
