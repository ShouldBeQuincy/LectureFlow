// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

// Credentials sealed at rest (cred-enc, user decision 2026-10-27; security
// review 2 "数据库明文凭据", L5). provider.api_key and the kv rows of
// sealedKVKeys hold sealed values in the same TEXT columns (no DDL change):
// since sec3 L3 secrets.SealFor values (prefix enc:v2:) bound to what they
// are and where they belong (see the bindings below); the enc:v1: values of
// 3.2.0 (secrets.Seal, no binding) still open and are sealed again bound at
// the next start (SealSecrets). The Store seals on every write and opens on
// every read, so its callers keep seeing the plain credential:
//
//   - a value already sealed is stored as it is (never sealed twice);
//   - without a data key (tests, or a failed key setup: the diagnostics
//     show an error) values are stored as they come, as before;
//   - a stored plain value (written before this, or without a key) reads
//     as it is; SealSecrets seals it at the next start;
//   - a sealed value no key opens (the data directory came from another
//     computer or account) reads as "no credential": a provider has an
//     empty APIKey and UnreadableSecrets lists it ("需要重新输入"), a kv row
//     reads as absent (Colab: authorise again). The sealed text itself never
//     leaves this package, and nothing here logs a value.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"

	"lectureflow/internal/secrets"
)

// The bindings (sec3 L3). A sealed value opens only for its purpose and
// binding, so a ciphertext moved to another row, column or host does not:
//   - provider.api_key: SealPurposeProviderKey, bound to the provider's id
//     and the origin of its base_url (ProviderKeyBinding). Through the API
//     a base_url on another origin clears the key (settings, sec2-app L2:
//     UpdateProviderClearKey); written into the database directly, the key
//     no longer opens and has to be entered again;
//   - kv rows: SealPurposeKV, bound to the row's key ("colab.credential";
//     ssh-mvp's "remote.ssh.key.{server id}", which sshremote seals and
//     opens itself with the same purpose).
const (
	SealPurposeProviderKey = "provider.api_key"
	SealPurposeKV          = "kv"
)

// ProviderKeyBinding is the binding of a provider's sealed API key.
func ProviderKeyBinding(id, baseURL string) string { return id + "\x00" + BaseURLOrigin(baseURL) }

// BaseURLOrigin is the origin of a base_url: scheme, host and port
// (ASCII lower-cased, the default port filled in); an unparsable URL (or
// one without a host) is "raw:" + the trimmed text, so it only matches
// itself. settings.SameOrigin compares base_urls with it (sec2-app L2), so
// the key that stays with a provider and the key that still opens are the
// same.
//
// Equal origins must dial the same host (SEC8 low-1). Only ASCII letters
// are lower-cased: Unicode lower-casing turns U+0130 into an ASCII "i",
// while net/http maps it to "i" + U+0307 by IDNA before dialling, another
// (punycode) domain. A host with a byte outside ASCII (written as is or
// percent-encoded) is not mapped at all, since its IDNA form is net/http's
// (golang.org/x/net/idna, not a dependency here): it is "idn:" + scheme,
// the host's bytes as written and the port, equal only to the same bytes
// and never to an ASCII host's origin.
func BaseURLOrigin(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return "raw:" + strings.TrimSpace(s)
	}
	scheme := asciiLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	host, ascii := HostKey(u.Hostname())
	if !ascii {
		return "idn:" + scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host + ":" + port
}

// HostKey is a host name compared by BaseURLOrigin's rule, for the other
// places that compare hosts (internal/fetch: cookie domains, Referer
// origins, a playlist's signature): ASCII letters lower-cased (ascii
// true); a host with a byte outside ASCII as written (ascii false), so it
// equals only the same bytes, never an ASCII host, and a suffix rule
// (".example.com" also for its subdomains) must not apply to it: net/http
// maps it by IDNA (U+0130 to "i" + U+0307, a full-width dot to ".") to a
// name Unicode lower-casing does not predict.
func HostKey(host string) (key string, ascii bool) {
	if !isASCII(host) {
		return host, false
	}
	return asciiLower(host), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// asciiLower lower-cases the ASCII letters A–Z only (no Unicode mapping).
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// sealedPrefixSQL is a condition: the column holds a sealed value (either
// format; both prefixes are 7 bytes, case-sensitive unlike LIKE).
// Arguments: sealedPrefixArgs.
func sealedPrefixSQL(col string) string {
	return "substr(" + col + ", 1, 7) IN (?, ?)"
}

func sealedPrefixArgs() []any {
	p := secrets.Prefixes()
	return []any{p[0], p[1]}
}

// sealedKVKeys are the kv rows kept sealed: the Colab credential
// (colab.KVCredential: a refresh_token and a client_secret).
var sealedKVKeys = []string{"colab.credential"}

// SealedKVKeys lists the kv keys whose values the Store keeps sealed.
func SealedKVKeys() []string { return slices.Clone(sealedKVKeys) }

func isSealedKV(key string) bool { return slices.Contains(sealedKVKeys, key) }

// sealStored returns what is stored for the credential plain, sealed for
// purpose and bind.
func sealStored(plain, purpose, bind string) (string, error) {
	if plain == "" || secrets.IsSealed(plain) || !secrets.HasKey() {
		return plain, nil
	}
	return secrets.SealFor(purpose, bind, []byte(plain))
}

// openStored returns the credential a stored value holds; unreadable when
// it is sealed and no key opens it for purpose and bind (plain is then "").
func openStored(stored, purpose, bind string) (plain string, unreadable bool) {
	plain, unreadable, _ = openStoredCurrent(stored, purpose, bind)
	return plain, unreadable
}

// openStoredCurrent is openStored that also reports whether the value is
// sealed as it should be now: bound (enc:v2:) with the current key. A
// plain value, an enc:v1: value and one only an earlier key opens are not.
//
// A value nested by a downgrade (openNested) opens to its inner credential
// and is not current; when the inner layer does not open it is unreadable:
// the sealed text is never returned as a credential.
func openStoredCurrent(stored, purpose, bind string) (plain string, unreadable, current bool) {
	if !secrets.IsSealed(stored) {
		return stored, false, false
	}
	b, cur, err := secrets.OpenAnyFor(purpose, bind, stored)
	if err != nil {
		return "", true, false
	}
	if secrets.IsSealed(string(b)) {
		inner, ok := openNested(b, func(v string) ([]byte, error) {
			p, _, err := secrets.OpenAnyFor(purpose, bind, v)
			return p, err
		})
		if !ok {
			return "", true, false
		}
		return string(inner), false, false
	}
	return string(b), false, cur
}

// openNested opens the inner layer of a value that opened to sealed text
// (SEC8 low-4, R15 suggestion 1): 3.2.0 does not know enc:v2: and seals
// it again as plain text (enc:v1:(enc:v2:…)), and the first start after
// the way back bound that as it was (enc:v2:(enc:v2:…)). The inner layer
// was sealed for the same purpose and binding; open opens it for them.
// One layer only: ok is false when the inner value does not open or
// opens to sealed text again (no loop), so the caller treats the value as
// unreadable and keeps it as it is.
func openNested(outer []byte, open func(string) ([]byte, error)) (inner []byte, ok bool) {
	inner, err := open(string(outer))
	if err != nil || secrets.IsSealed(string(inner)) {
		return nil, false
	}
	return inner, true
}

// warned: the unreadable credentials already logged in this process.
var warned sync.Map

// warnUnreadable logs once per credential that it cannot be decrypted
// (what: "provider" with its id, or "kv" with its key; never a value).
func warnUnreadable(what, id string) {
	if _, dup := warned.LoadOrStore(what+"\x00"+id, true); dup {
		return
	}
	slog.Warn("db: a stored credential cannot be decrypted with this installation's key (the data directory came from another computer or account?); it has to be entered again",
		what, id)
}

// sealKV is SetKV's step for a sealed kv key.
func sealKV(key, plain string) (string, error) { return sealStored(plain, SealPurposeKV, key) }

// openKV is GetKV's step for a sealed kv key: ok false when unreadable.
func openKV(key, stored string) (string, bool) {
	plain, unreadable := openStored(stored, SealPurposeKV, key)
	if unreadable {
		warnUnreadable("kv", key)
		return "", false
	}
	return plain, true
}

// openProviderKey is scanProvider's step for provider.api_key.
func openProviderKey(id, baseURL, stored string) string {
	plain, unreadable := openStored(stored, SealPurposeProviderKey, ProviderKeyBinding(id, baseURL))
	if unreadable {
		warnUnreadable("provider", id)
	}
	return plain
}

// UnreadableSecrets lists the stored credentials no key opens: provider ids
// (their key has to be entered again; also one whose base_url was moved to
// another origin behind the API's back, sec3 L3) and kv keys. Not part of
// the Store interface (frozen): settings and the diagnostics reach it with
// a type assertion (SecretInspector).
func (s *sqlStore) UnreadableSecrets(ctx context.Context) (providers, kv []string, err error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, base_url, api_key FROM provider WHERE `+sealedPrefixSQL("api_key")+` ORDER BY id`,
		sealedPrefixArgs()...)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id, base, v string
		if err := rows.Scan(&id, &base, &v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if _, bad := openStored(v, SealPurposeProviderKey, ProviderKeyBinding(id, base)); bad {
			providers = append(providers, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, k := range sealedKVKeys {
		var v string
		err := s.q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, k).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if _, bad := openStored(v, SealPurposeKV, k); bad {
			kv = append(kv, k)
		}
	}
	return providers, kv, nil
}

// SecretInspector is the optional Store method UnreadableSecrets.
type SecretInspector interface {
	UnreadableSecrets(ctx context.Context) (providers, kv []string, err error)
}

// UnsealedSecrets lists the credentials stored in the clear: provider ids
// with a plain API key and sealedKVKeys rows with a plain value (written
// while there was no data key; sealed at the next start that has one).
// sec3 L2: the settings page and the diagnostics show them.
func (s *sqlStore) UnsealedSecrets(ctx context.Context) (providers, kv []string, err error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, api_key FROM provider WHERE api_key <> '' ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if !secrets.IsSealed(v) {
			providers = append(providers, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, k := range sealedKVKeys {
		var v string
		err := s.q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, k).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if v != "" && !secrets.IsSealed(v) {
			kv = append(kv, k)
		}
	}
	return providers, kv, nil
}

// SecretPlainInspector is the optional Store method UnsealedSecrets.
type SecretPlainInspector interface {
	UnsealedSecrets(ctx context.Context) (providers, kv []string, err error)
}

// SealResult counts what SealSecrets changed.
type SealResult struct {
	Sealed int // plain values sealed
	// Resealed: values opened with an earlier key, sealed with the current
	// one; also values nested by a way back through 3.2.0 (openNested),
	// sealed once around their inner credential
	Resealed int
	// Bound: enc:v1: values (no binding) of the current key sealed again
	// bound to their row (sec3 L3; once, at the first start of the version)
	Bound      int
	Unreadable int // sealed values no key opens (left as they are)
	// Scrubbed: an earlier secure write had not finished its clean-up
	// (kvScrubPending); the VACUUM and the checkpoint ran now (sec3 I3).
	Scrubbed bool
}

// Changed reports whether anything was rewritten.
func (r SealResult) Changed() bool { return r.Sealed+r.Resealed+r.Bound > 0 }

type sealUpdate struct {
	query string
	args  []any
}

// SealSecrets brings the stored credentials of the database d to the
// current data key (provider.api_key, the sealedKVKeys rows, and any other
// kv row whose owner sealed it, such as ssh-mvp's private keys, which are
// only sealed again): plain values are sealed, values only an earlier key
// (secrets.SetFallbackKeys, or one of extraKeys: an imported data
// directory's key) opens are sealed again, and enc:v1: values (3.2.0, no
// binding) are sealed again bound to the row they are in (sec3 L3); every
// value it writes is bound (secrets.SealFor). The rewrite runs on the secure
// path (secure_delete, VACUUM, TRUNCATE checkpoint), so neither the plain
// values nor the old ciphertexts stay in free pages or the WAL. Nothing is
// written when everything is sealed already (every start but the first),
// nor without a data key. Values no key opens are left as they are and
// counted. A database without the provider or kv table is fine.
func SealSecrets(ctx context.Context, d *sql.DB, extraKeys ...[]byte) (SealResult, error) {
	var res SealResult
	if !secrets.HasKey() {
		return res, finishPendingScrub(ctx, d, &res)
	}
	tables := map[string]bool{}
	rows, err := d.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ('provider', 'kv')`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return res, err
		}
		tables[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	var updates []sealUpdate
	plan := func(stored, purpose, bind string, update func(v string) sealUpdate) error {
		if stored == "" {
			return nil
		}
		var plain []byte
		if !secrets.IsSealed(stored) {
			plain = []byte(stored)
			res.Sealed++
		} else {
			var p []byte
			var current bool
			var err error
			count := &res.Resealed
			if secrets.IsBound(stored) {
				if p, current, err = secrets.OpenAnyFor(purpose, bind, stored); err == nil && current && !secrets.IsSealed(string(p)) {
					return nil
				}
			} else if p, current, err = secrets.OpenAny(stored); err == nil && current {
				count = &res.Bound // enc:v1: of the current key: only the binding is new
			}
			// the process's keys, then the imported directory's
			open := func(v string) ([]byte, error) {
				p, _, err := secrets.OpenAnyFor(purpose, bind, v)
				for _, k := range extraKeys {
					if err == nil {
						break
					}
					p, err = secrets.OpenWithFor(k, purpose, bind, v)
				}
				return p, err
			}
			if err != nil {
				p, err = open(stored)
			}
			if err != nil {
				res.Unreadable++
				return nil
			}
			if secrets.IsSealed(string(p)) {
				// nested by a way back through 3.2.0 (openNested): sealed
				// once, around the inner credential; left as it is when the
				// inner layer does not open
				inner, ok := openNested(p, open)
				if !ok {
					res.Unreadable++
					return nil
				}
				p, count = inner, &res.Resealed
			}
			plain = p
			*count++
		}
		v, err := secrets.SealFor(purpose, bind, plain)
		if err != nil {
			return err
		}
		updates = append(updates, update(v))
		return nil
	}

	if tables["provider"] {
		rows, err := d.QueryContext(ctx, `SELECT id, base_url, api_key FROM provider WHERE api_key <> ''`)
		if err != nil {
			return res, err
		}
		type row struct{ id, base, v string }
		var list []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.base, &r.v); err != nil {
				rows.Close()
				return res, err
			}
			list = append(list, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		for _, r := range list {
			id := r.id
			if err := plan(r.v, SealPurposeProviderKey, ProviderKeyBinding(r.id, r.base), func(v string) sealUpdate {
				return sealUpdate{`UPDATE provider SET api_key = ? WHERE id = ?`, []any{v, id}}
			}); err != nil {
				return res, err
			}
		}
	}
	if tables["kv"] {
		// the sealed kv keys, and any other row its owner sealed itself
		// (ssh-mvp's remote.ssh.key.*): those are only sealed again, a plain
		// value of another key is no credential and stays as it is
		rows, err := d.QueryContext(ctx, `SELECT key, value FROM kv WHERE `+sealedPrefixSQL("value")+` ORDER BY key`, sealedPrefixArgs()...)
		if err != nil {
			return res, err
		}
		kv := map[string]string{}
		var keys []string
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return res, err
			}
			if secrets.IsSealed(v) {
				kv[k] = v
				keys = append(keys, k)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		for _, k := range sealedKVKeys {
			if _, done := kv[k]; done {
				continue
			}
			var v string
			err := d.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, k).Scan(&v)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return res, err
			}
			kv[k] = v
			keys = append(keys, k)
		}
		for _, k := range keys {
			key := k
			if err := plan(kv[k], SealPurposeKV, k, func(v string) sealUpdate {
				return sealUpdate{`UPDATE kv SET value = ? WHERE key = ?`, []any{v, key}}
			}); err != nil {
				return res, err
			}
		}
	}
	if len(updates) == 0 {
		return res, finishPendingScrub(ctx, d, &res)
	}
	err = withSecureConn(ctx, d, func(ctx context.Context, c *sql.Conn) error {
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, u := range updates {
			if _, err := tx.ExecContext(ctx, u.query, u.args...); err != nil {
				tx.Rollback()
				return err
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return res, fmt.Errorf("db: sealing the stored credentials: %w", err)
	}
	return res, nil
}

// SealStoredSecrets is SealSecrets on the Store's database (startup).
func (s *sqlStore) SealStoredSecrets(ctx context.Context) (SealResult, error) {
	if s.tx != nil {
		return SealResult{}, errors.New("db: SealStoredSecrets inside a transaction")
	}
	return SealSecrets(ctx, s.db)
}

// SecretSealer is the optional Store method SealStoredSecrets.
type SecretSealer interface {
	SealStoredSecrets(ctx context.Context) (SealResult, error)
}

// finishPendingScrub runs the clean-up an earlier secure write left
// unfinished (kvScrubPending: interrupted, or the VACUUM failed): sec3 I3.
func finishPendingScrub(ctx context.Context, d *sql.DB, res *SealResult) error {
	if !scrubPending(ctx, d) {
		return nil
	}
	if err := withSecureConn(ctx, d, func(context.Context, *sql.Conn) error { return nil }); err != nil {
		return fmt.Errorf("db: finishing an interrupted clean-up of freed credentials: %w", err)
	}
	res.Scrubbed = true
	return nil
}
