// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package secrets seals the credentials LectureFlow keeps (provider API
// keys, the Colab credential, SSH private keys) with a per-installation
// data key, so a copied, synced or backed-up data directory does not carry
// them in the clear (security review 2, user decision 2026-10-27).
//
// A sealed value is Prefix + base64(nonce ‖ AES-256-GCM ciphertext), kept
// in the same TEXT column the plain value used to be in (no DDL change).
// Since sec3 L3 the credentials are sealed with SealFor instead (PrefixV2,
// bound.go): the ciphertext is bound to its purpose and to where it
// belongs, so it does not open in another row or for another host.
// Where the data key comes from (Windows DPAPI, the desktop shell's
// safeStorage, a 0600 file) is the key provider's business; this file is
// only the frozen API (lead, 2026-10-27): Seal, Open, IsSealed, SetKey,
// HasKey and the errors may gain siblings, never change.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
)

// Prefix marks a value Seal produced (no binding; PrefixV2 for SealFor).
const Prefix = "enc:v1:"

// KeySize is the data key's length in bytes.
const KeySize = 32

var (
	// ErrNoKey: no data key has been set in this process.
	ErrNoKey = errors.New("secrets: no data key")
	// ErrUndecryptable: the value was sealed with another key (another
	// computer or account, or a lost key) or was altered; the user has to
	// enter the credential again.
	ErrUndecryptable = errors.New("secrets: the value cannot be decrypted with this installation's key")
)

var (
	mu   sync.RWMutex
	aead cipher.AEAD
)

// SetKey sets the process's data key (KeySize bytes). Calling it again
// replaces the key; values sealed with the old one no longer open.
func SetKey(key []byte) error {
	if len(key) != KeySize {
		return errors.New("secrets: the data key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	mu.Lock()
	aead = g
	mu.Unlock()
	return nil
}

// HasKey reports whether a data key has been set.
func HasKey() bool {
	mu.RLock()
	defer mu.RUnlock()
	return aead != nil
}

// IsSealed reports whether s is a sealed value (it may still fail to open):
// one Seal produced (Prefix) or, since sec3 L3, one SealFor produced
// (PrefixV2). Every caller treats both as "not a plain credential".
func IsSealed(s string) bool { return strings.HasPrefix(s, Prefix) || IsBound(s) }

// Seal encrypts plain with the data key.
func Seal(plain []byte) (string, error) {
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
	out := g.Seal(nonce, nonce, plain, []byte(Prefix))
	return Prefix + base64.RawStdEncoding.EncodeToString(out), nil
}

// Open decrypts a value Seal produced. A value SealFor produced is refused
// (errBound): it opens only with its purpose and binding (OpenFor).
func Open(sealed string) ([]byte, error) {
	if IsBound(sealed) {
		return nil, errBound
	}
	if !IsSealed(sealed) {
		return nil, errors.New("secrets: not a sealed value")
	}
	mu.RLock()
	g := aead
	mu.RUnlock()
	if g == nil {
		return nil, ErrNoKey
	}
	raw, err := base64.RawStdEncoding.DecodeString(sealed[len(Prefix):])
	if err != nil || len(raw) < g.NonceSize()+g.Overhead() {
		return nil, ErrUndecryptable
	}
	plain, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], []byte(Prefix))
	if err != nil {
		return nil, ErrUndecryptable
	}
	return plain, nil
}
