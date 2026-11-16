// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// Keys (plan §3): every server gets its own ed25519 key. The private key
// exists in the clear only in this process's memory: it is stored sealed
// with internal/secrets in the kv row KeyRef (a seal fails without a data
// key, so a plain key is never written). The API hands out only the public
// key, as an authorized_keys line with "restrict" (no forwarding, no PTY,
// no ~/.ssh/rc) and its fingerprint.
//
// sec3 L3: the key is sealed bound to its server (secrets.SealFor with the
// database's kv purpose and the row KVKeyPrefix + server id, the binding
// db.SealSecrets also gives it), and opened only for the server that uses
// it: a key_ref pointing at another server's row, or a ciphertext copied
// into this server's row, does not open.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	"lectureflow/internal/db"
	"lectureflow/internal/secrets"
)

// KVKeyPrefix starts the kv key of a server's sealed private key.
const KVKeyPrefix = "remote.ssh.key."

// ErrNoDataKey: the data key of internal/secrets is not available, so no
// private key can be stored or read.
var ErrNoDataKey = errors.New("sshremote: the credential store is not available")

// KeyRef is the kv row of a server's sealed private key.
func KeyRef(serverID string) string { return KVKeyPrefix + serverID }

// newKey generates a key pair and returns the private key sealed for the
// kv row ref and the public key's authorized_keys line ("restrict
// ssh-ed25519 AAAA… comment").
func newKey(comment, ref string) (sealed, authorized string, err error) {
	if !secrets.HasKey() {
		return "", "", ErrNoDataKey
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", err
	}
	sealed, err = secrets.SealFor(db.SealPurposeKV, ref, pem.EncodeToMemory(block))
	if err != nil {
		return "", "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return sealed, authorizedLine(sp, comment), nil
}

// authorizedLine is the line the user appends to ~/.ssh/authorized_keys.
func authorizedLine(pub ssh.PublicKey, comment string) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	return "restrict " + line + " " + comment
}

// publicKeyLineRe is exactly what authorizedLine makes for a server:
// "restrict", the ed25519 key and the comment "lectureflow-<slug>", single
// spaces, nothing else.
var publicKeyLineRe = regexp.MustCompile(`^restrict ssh-ed25519 [A-Za-z0-9+/]+={0,2} lectureflow-[a-z0-9-]{1,32}$`)

// checkPublicKeyLine refuses a stored public key line that is not one
// authorizedLine made (SEC7 L2). The line goes between quotes into the
// commands the user pastes into a shell on the server (PublicKeyOf); only
// Add writes it, but it is read back from the database, so no quoting is
// relied on: a line with anything else in it gets no command.
func checkPublicKeyLine(line string) error {
	if !publicKeyLineRe.MatchString(line) || PublicFingerprint(line) == "" {
		return &FieldError{Field: "public_key"}
	}
	return nil
}

// openKey opens a private key sealed for the kv row ref as a signer.
func openKey(ref, sealed string) (ssh.Signer, error) {
	if !secrets.HasKey() {
		return nil, ErrNoDataKey
	}
	// OpenAnyFor: a value sealed with an earlier key, or 3.2.0's unbound
	// format (not sealed again yet), opens too.
	b, _, err := secrets.OpenAnyFor(db.SealPurposeKV, ref, sealed)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(b)
	for i := range b {
		b[i] = 0
	}
	return s, err
}

// keyStore is where the sealed keys live (the kv table).
type keyStore interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, value string) error
}

// loadSigner reads a server's key from the kv row ref (its key_ref) and
// opens it for that server: the key must be sealed for KeyRef(serverID).
func loadSigner(ctx context.Context, st keyStore, serverID, ref string) (ssh.Signer, error) {
	v, ok, err := st.GetKV(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !ok || v == "" {
		return nil, fmt.Errorf("sshremote: the key %s is missing", ref)
	}
	if !secrets.IsSealed(v) {
		return nil, fmt.Errorf("sshremote: the key %s is not sealed", ref)
	}
	return openKey(KeyRef(serverID), v)
}

// PublicFingerprint is the SHA256 fingerprint of an authorized_keys line.
func PublicFingerprint(line string) string {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "restrict "))
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pub)
}
