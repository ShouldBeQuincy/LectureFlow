// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"sync"
)

// KV is the part of the database the backend uses (db.Store satisfies
// it). An empty value means "absent": db.Store has no delete, so deleting
// writes "".
type KV interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, value string) error
}

// kv keys of the Colab backend.
const (
	// KVCredential is the imported gcloud ADC credential with the user's
	// lifecycle (storedCredential JSON). Plain text like the AI provider
	// keys (V14.2: "存储方式与 AI 服务商的 API key 相同"); no interface ever
	// returns it.
	KVCredential = "colab.credential"
	// KVSession is the current remote session (sessionRecord JSON, no
	// tokens), so that the next start releases a session a crash left
	// behind (V14.3 step 5).
	KVSession = "colab.session"
	// KVLost lists runtimes no session stands for any more that could not
	// be released yet (lostRuntime JSON array, no tokens; lost.go): Tick and
	// the next start release them (security review SEC2 H1).
	KVLost = "colab.lost"
	// KVSettings are the backend settings (Settings JSON).
	KVSettings = "colab.settings"
	// KVRates are the measured speeds per accelerator, model and precision
	// (map of Rate JSON), so a later session starts with a measured value.
	KVRates = "colab.rates"
	// KVCooldown is the time (RFC 3339) until which no new session starts
	// after a session ended because its fair-use window was used up.
	KVCooldown = "colab.window_cooldown_until"
	// KVMemoryMode is memory mode (SPEC-ext "Colab 内存运行", memory.go):
	// MemoryModeAuto (the default since 2026-10-27: absent and any value
	// but "off" mean auto) or MemoryModeOff, written by the switch on the
	// settings page (Backend.SetMemoryMode). Read at the start of every
	// lecture. Not part of colab.settings, so that "never set" (follows the
	// default) and "switched off by the user" stay apart.
	KVMemoryMode = "colab.memory_mode"
)

// MemKV is an in-memory KV for tests and tools.
type MemKV struct {
	mu sync.Mutex
	m  map[string]string
}

// NewMemKV returns an empty MemKV.
func NewMemKV() *MemKV { return &MemKV{m: map[string]string{}} }

func (k *MemKV) GetKV(_ context.Context, key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *MemKV) SetKV(_ context.Context, key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = value
	return nil
}

// Snapshot returns a copy of all entries (tests).
func (k *MemKV) Snapshot() map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]string, len(k.m))
	for a, b := range k.m {
		out[a] = b
	}
	return out
}

// getKV reads key, treating "" as absent.
func getKV(ctx context.Context, kv KV, key string) (string, bool, error) {
	v, ok, err := kv.GetKV(ctx, key)
	if err != nil || !ok || v == "" {
		return "", false, err
	}
	return v, true, nil
}

func deleteKV(ctx context.Context, kv KV, key string) error { return kv.SetKV(ctx, key, "") }

// SecretKV is implemented by a KV that can replace or delete a value
// without leaving copies of the old one behind (db.Store's SQLite
// implementation: secure_delete, VACUUM and a TRUNCATE checkpoint, so a
// refresh_token or client_secret stays neither in free pages nor in the
// WAL; Gate round 1 item 9). value "" deletes the key.
type SecretKV interface {
	SetSecretKV(ctx context.Context, key, value string) error
}

// setSecret writes (or, with "", deletes) a secret: through SecretKV when
// kv has it, else a plain SetKV (MemKV).
func setSecret(ctx context.Context, kv KV, key, value string) error {
	if s, ok := kv.(SecretKV); ok {
		return s.SetSecretKV(ctx, key, value)
	}
	return kv.SetKV(ctx, key, value)
}
