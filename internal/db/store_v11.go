// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"

	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

// v1.1 queries (migrations/003_v1_1.sql).

func snapshotOrEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return model.EmptySnapshot
	}
	return s
}

// duplicate maps a UNIQUE constraint failure to ErrDuplicate.
func duplicate(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrDuplicate
	}
	return err
}

func (s *sqlStore) SetCourseCode(ctx context.Context, id string, code *string) error {
	var v any
	if code != nil && strings.TrimSpace(*code) != "" {
		v = strings.TrimSpace(*code)
	}
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE course SET code = ? WHERE id = ?`, v, id))
}

func (s *sqlStore) SetCourseName(ctx context.Context, id, name string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE course SET name = ? WHERE id = ?`, name, id))
}

func (s *sqlStore) SetLectureLaunched(ctx context.Context, id, at string) (bool, error) {
	launched := false
	err := s.Tx(ctx, func(tx Store) error {
		t := tx.(*sqlStore)
		res, err := t.q.ExecContext(ctx, `UPDATE lecture SET launched_at = ? WHERE id = ? AND launched_at IS NULL`, at, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			launched = true
			return nil
		}
		_, err = t.GetLecture(ctx, id) // ErrNotFound vs already launched
		return err
	})
	return launched && err == nil, err
}

func (s *sqlStore) SetLectureSeq(ctx context.Context, id string, seq int) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE lecture SET seq = ? WHERE id = ?`, seq, id))
}

func (s *sqlStore) NextLectureSeq(ctx context.Context, courseID string) (int, error) {
	var n sql.NullInt64
	if err := s.q.QueryRowContext(ctx, `SELECT MAX(seq) FROM lecture WHERE course_id = ?`, courseID).Scan(&n); err != nil {
		return 0, err
	}
	return int(n.Int64) + 1, nil
}

func (s *sqlStore) SetJobSnapshot(ctx context.Context, jobID, snapshot string) error {
	return affectedOrNotFound(s.q.ExecContext(ctx, `UPDATE job SET config_snapshot = ? WHERE id = ?`, snapshotOrEmpty(snapshot), jobID))
}

func (s *sqlStore) SetStaleReason(ctx context.Context, jobID, reason string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE job SET stale_reason = ?
		WHERE id = ? AND status = 'stale' AND is_current = 1 AND stale_reason IS NOT ?`, reason, jobID, reason)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ---------- provider ----------

const providerCols = `id, name, type, base_url, api_key, max_concurrency, rpm, balance_api, price_input, price_cache_hit, price_output, currency, created_at, tariff, offpeak_only`

func nullFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func floatPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func scanProvider(sc interface{ Scan(...any) error }) (model.Provider, error) {
	var p model.Provider
	var in, hit, out sql.NullFloat64
	var cur, tariff sql.NullString
	var offpeak int
	var key string
	err := sc.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &key, &p.MaxConcurrency, &p.RPM, &p.BalanceAPI,
		&in, &hit, &out, &cur, &p.CreatedAt, &tariff, &offpeak)
	// cred-enc (sealed.go): the plain key; "" when no key opens it for this
	// provider and the origin of its base_url (sec3 L3)
	p.APIKey = openProviderKey(p.ID, p.BaseURL, key)
	p.PriceInput, p.PriceCacheHit, p.PriceOutput, p.Currency = floatPtr(in), floatPtr(hit), floatPtr(out), strPtr(cur)
	p.Tariff, p.OffpeakOnly = strPtr(tariff), offpeak != 0
	return p, err
}

func (s *sqlStore) ListProviders(ctx context.Context) ([]model.Provider, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+providerCols+` FROM provider ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *sqlStore) GetProvider(ctx context.Context, id string) (model.Provider, error) {
	p, err := scanProvider(s.q.QueryRowContext(ctx, `SELECT `+providerCols+` FROM provider WHERE id = ?`, id))
	return p, notFound(err)
}

func (s *sqlStore) InsertProvider(ctx context.Context, p model.Provider) error {
	if p.CreatedAt == "" {
		p.CreatedAt = Now()
	}
	key, err := sealStored(p.APIKey, SealPurposeProviderKey, ProviderKeyBinding(p.ID, p.BaseURL)) // cred-enc: stored sealed
	if err != nil {
		return err
	}
	_, err = s.q.ExecContext(ctx, `INSERT INTO provider (`+providerCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Type, p.BaseURL, key, p.MaxConcurrency, p.RPM, p.BalanceAPI,
		nullFloat(p.PriceInput), nullFloat(p.PriceCacheHit), nullFloat(p.PriceOutput), nullStr(p.Currency), p.CreatedAt,
		nullStr(p.Tariff), boolInt(p.OffpeakOnly))
	return duplicate(err)
}

// storedProviderKey is the provider's api_key column as stored (sealed)
// and the base_url it is bound to (sec3 L3).
func (s *sqlStore) storedProviderKey(ctx context.Context, id string) (key, baseURL string, err error) {
	err = s.q.QueryRowContext(ctx, `SELECT api_key, base_url FROM provider WHERE id = ?`, id).Scan(&key, &baseURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil // affectedOrNotFound reports the missing row
	}
	return key, baseURL, err
}

const updateProviderSet = `UPDATE provider SET name = ?, type = ?, base_url = ?, max_concurrency = ?,
		rpm = ?, balance_api = ?, price_input = ?, price_cache_hit = ?, price_output = ?, currency = ?,
		tariff = ?, offpeak_only = ?`

func updateProviderArgs(p model.Provider) []any {
	return []any{p.Name, p.Type, p.BaseURL, p.MaxConcurrency, p.RPM, p.BalanceAPI,
		nullFloat(p.PriceInput), nullFloat(p.PriceCacheHit), nullFloat(p.PriceOutput), nullStr(p.Currency),
		nullStr(p.Tariff), boolInt(p.OffpeakOnly)}
}

// UpdateProvider writes p. cred-enc / sec2-app L5: an unchanged key keeps
// its stored (sealed) value; a key that changes, or is removed, replaces
// the old one on the secure path (secure_delete, VACUUM, TRUNCATE
// checkpoint), so the old key leaves no copy in free pages or the WAL.
// Inside a transaction (VACUUM cannot run there) it is a plain UPDATE.
//
// sec3 I4: an empty APIKey never removes a stored key that cannot be
// decrypted here (the provider was read with "" for it): it stays as it
// is, so a key file that is put back still opens it. Removing a key, any
// key, is UpdateProviderClearKey. sec3 L3: the key is sealed bound to the
// origin of p.BaseURL; a key kept while the origin changes is sealed again
// for the new one (the caller passed it on purpose: settings clears it
// instead, sec2-app L2).
func (s *sqlStore) UpdateProvider(ctx context.Context, p model.Provider) error {
	old, oldBase, err := s.storedProviderKey(ctx, p.ID)
	if err != nil {
		return err
	}
	key := old
	plainOld, unreadable, current := openStoredCurrent(old, SealPurposeProviderKey, ProviderKeyBinding(p.ID, oldBase))
	var changed bool
	switch {
	case old == "":
		changed = p.APIKey != ""
	case unreadable:
		changed = p.APIKey != "" // I4: "" keeps the key to enter again
	default:
		// a plain stored key (written without a data key), an enc:v1: one
		// or one of an earlier key is sealed (bound) now, also when it stays
		// the same; its old copy goes on the secure path
		changed = plainOld != p.APIKey || (!current && secrets.HasKey()) ||
			BaseURLOrigin(oldBase) != BaseURLOrigin(p.BaseURL)
	}
	if changed {
		if key, err = sealStored(p.APIKey, SealPurposeProviderKey, ProviderKeyBinding(p.ID, p.BaseURL)); err != nil {
			return err
		}
	}
	return s.writeProvider(ctx, p, key, changed && old != "")
}

// writeProvider writes p's columns and api_key = key; secure: the old key
// leaves the database (secure path unless inside a transaction).
func (s *sqlStore) writeProvider(ctx context.Context, p model.Provider, key string, secure bool) error {
	args := append(updateProviderArgs(p), key, p.ID)
	q := updateProviderSet + `, api_key = ? WHERE id = ?`
	if !secure || s.tx != nil {
		res, err := s.q.ExecContext(ctx, q, args...)
		return affectedOrNotFound(res, duplicate(err))
	}
	var res sql.Result
	compactErr, err := secureWrite(ctx, s.db, func(ctx context.Context, c *sql.Conn) error {
		var err error
		res, err = c.ExecContext(ctx, q, args...)
		return err
	})
	if err != nil {
		return duplicate(err)
	}
	if compactErr != nil {
		// The new key is stored; the old one may stay in the WAL until the
		// next checkpoint (another connection was reading).
		slog.Warn("db: the replaced API key may remain in the database's WAL until the next checkpoint", "provider", p.ID, "err", compactErr)
	}
	return affectedOrNotFound(res, nil)
}

// UpdateProviderKeepKey writes p except its key: the stored key stays as it
// is, also one that cannot be decrypted here (settings: a provider whose key
// has to be entered again keeps it while other fields change, so a key file
// that is put back still opens it). The key stays bound to the origin it
// was sealed for: with p.BaseURL on another origin it no longer opens.
func (s *sqlStore) UpdateProviderKeepKey(ctx context.Context, p model.Provider) error {
	res, err := s.q.ExecContext(ctx, updateProviderSet+` WHERE id = ?`, append(updateProviderArgs(p), p.ID)...)
	return affectedOrNotFound(res, duplicate(err))
}

// ProviderKeyKeeper is the optional Store method UpdateProviderKeepKey.
type ProviderKeyKeeper interface {
	UpdateProviderKeepKey(ctx context.Context, p model.Provider) error
}

// UpdateProviderClearKey writes p except its key and removes the stored
// key, whatever it is (plain, sealed, or one that cannot be decrypted
// here), in the same statement, on the secure path (inside a transaction
// a plain UPDATE: the caller runs ScrubFreedSecrets after it). sec3 I4:
// the explicit "clear the key" (settings: base_url moved to another
// origin, sec2-app L2), since UpdateProvider keeps an unreadable key.
func (s *sqlStore) UpdateProviderClearKey(ctx context.Context, p model.Provider) error {
	old, _, err := s.storedProviderKey(ctx, p.ID)
	if err != nil {
		return err
	}
	return s.writeProvider(ctx, p, "", old != "")
}

// ProviderKeyClearer is the optional Store method UpdateProviderClearKey.
type ProviderKeyClearer interface {
	UpdateProviderClearKey(ctx context.Context, p model.Provider) error
}

// DeleteProvider deletes the provider; one with a key on the secure path
// (L5) unless inside a transaction (the caller then runs
// ScrubFreedSecrets after it).
func (s *sqlStore) DeleteProvider(ctx context.Context, id string) error {
	old, _, err := s.storedProviderKey(ctx, id)
	if err != nil {
		return err
	}
	if old == "" || s.tx != nil {
		return affectedOrNotFound(s.q.ExecContext(ctx, `DELETE FROM provider WHERE id = ?`, id))
	}
	var res sql.Result
	compactErr, err := secureWrite(ctx, s.db, func(ctx context.Context, c *sql.Conn) error {
		var err error
		res, err = c.ExecContext(ctx, `DELETE FROM provider WHERE id = ?`, id)
		return err
	})
	if err != nil {
		return err
	}
	if compactErr != nil {
		slog.Warn("db: the deleted API key may remain in the database's WAL until the next checkpoint", "provider", id, "err", compactErr)
	}
	return affectedOrNotFound(res, nil)
}

// ---------- role_setting ----------

func (s *sqlStore) ListRoleSettings(ctx context.Context) ([]model.RoleSetting, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT role, provider_id, model, params FROM role_setting ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RoleSetting
	for rows.Next() {
		var r model.RoleSetting
		var pid, m sql.NullString
		if err := rows.Scan(&r.Role, &pid, &m, &r.Params); err != nil {
			return nil, err
		}
		r.ProviderID, r.Model = strPtr(pid), strPtr(m)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sqlStore) UpsertRoleSetting(ctx context.Context, r model.RoleSetting) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO role_setting (role, provider_id, model, params) VALUES (?,?,?,?)
		ON CONFLICT(role) DO UPDATE SET provider_id = excluded.provider_id, model = excluded.model, params = excluded.params`,
		r.Role, nullStr(r.ProviderID), nullStr(r.Model), snapshotOrEmpty(r.Params))
	return err
}

// ---------- usage_daily ----------

func (s *sqlStore) AddUsageDaily(ctx context.Context, u model.UsageDaily) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO usage_daily (provider_id, day, requests, prompt_tokens, cache_hit_tokens, completion_tokens, estimated_cost)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(provider_id, day) DO UPDATE SET
			requests = requests + excluded.requests,
			prompt_tokens = prompt_tokens + excluded.prompt_tokens,
			cache_hit_tokens = cache_hit_tokens + excluded.cache_hit_tokens,
			completion_tokens = completion_tokens + excluded.completion_tokens,
			estimated_cost = estimated_cost + excluded.estimated_cost`,
		u.ProviderID, u.Day, u.Requests, u.PromptTokens, u.CacheHitTokens, u.CompletionTokens, u.EstimatedCost)
	return err
}

func (s *sqlStore) ListUsageDaily(ctx context.Context, sinceDay string) ([]model.UsageDaily, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT provider_id, day, requests, prompt_tokens, cache_hit_tokens, completion_tokens, estimated_cost
		FROM usage_daily WHERE day >= ? ORDER BY day, provider_id`, sinceDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.UsageDaily
	for rows.Next() {
		var u model.UsageDaily
		if err := rows.Scan(&u.ProviderID, &u.Day, &u.Requests, &u.PromptTokens, &u.CacheHitTokens, &u.CompletionTokens, &u.EstimatedCost); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
