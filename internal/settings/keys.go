// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

// Provider keys that cannot be decrypted (cred-enc, user decision
// 2026-10-27). The database keeps the keys sealed with the installation's
// data key (internal/secrets, internal/db sealed.go); a data directory
// brought from another computer or account holds keys this one cannot
// open. Such a provider reads with an empty APIKey, so nothing ever sends
// the ciphertext as a key (the llm client refuses an empty key with
// llm.no_api_key), and:
//
//   - KeyUnreadable / KeyErrorInfo report it (the API's provider view:
//     key_unreadable, key_error_info "settings.provider_key_unreadable";
//     the settings page shows "需要重新输入 key");
//   - SnapshotUsableInfo gives snapshot.unusable.api_key_unreadable;
//   - UpdateProvider keeps the stored key while other fields change (a key
//     file that is put back opens it again), unless a new key is entered or
//     base_url moves to another origin (the key is cleared, sec2-app L2).
//     db.UpdateProvider itself keeps such a key when it is given an empty
//     one (sec3 I4: also the tariff preset's transaction); clearing it is
//     db.ProviderKeyClearer.

import (
	"context"
	"fmt"
	"log/slog"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

// Without a data key (sec3 L2): a key entered on the settings page is
// still saved, in the clear, as before cred-enc. Refusing it would leave
// translation, summaries and repair unusable until the system's credential
// store works again (the keys saved earlier cannot be read without the key
// either), which §0.1 ranks below a loud warning. So:
//
//   - CredentialsErrorInfo (settings.credentials_unavailable {error}) is in
//     GET /api/settings while there is no data key; the provider dialog
//     shows it beside the key field;
//   - KeyUnprotected / KeyErrorInfo (settings.provider_key_unprotected
//     {provider}) mark each provider whose key is stored in the clear, also
//     in the response of the save; the next start with a data key seals it
//     (db.SealSecrets) and the mark goes away;
//   - the diagnostics' credentials row is an error with the reason.

// unreadableKeys reads which providers' keys no data key opens (a store
// without db.SecretInspector has none).
func unreadableKeys(ctx context.Context, st db.Store) (map[string]bool, error) {
	in, ok := st.(db.SecretInspector)
	if !ok {
		return nil, nil
	}
	ids, _, err := in.UnreadableSecrets(ctx)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// unsealedKeys reads which providers' keys are stored in the clear.
func unsealedKeys(ctx context.Context, st db.Store) (map[string]bool, error) {
	in, ok := st.(db.SecretPlainInspector)
	if !ok {
		return nil, nil
	}
	ids, _, err := in.UnsealedSecrets(ctx)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// KeyUnprotected reports whether the provider's key is stored in the clear
// (saved while there was no data key).
func (s *Service) KeyUnprotected(providerID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.unsealed[providerID]
}

// CredentialsErrorInfo is settings.credentials_unavailable while the
// process has no data key (Setup failed, or the system's credential store
// is unavailable now); nil otherwise, and when Setup never ran (tests).
func CredentialsErrorInfo() *model.ErrorInfo {
	st := secrets.CurrentStatus()
	if st.Protection != secrets.ProtectionNone {
		return nil
	}
	why := st.Err
	if why == "" {
		why = "-"
	}
	return &model.ErrorInfo{Code: CodeCredentialsUnavailable, Params: map[string]any{"error": why},
		Detail: fmt.Sprintf("现在无法加密保存凭据（%s）：新填写的 API key 会以明文存入数据库，诊断页有详情", why)}
}

// KeyUnreadable reports whether the provider's stored key cannot be
// decrypted here and has to be entered again.
func (s *Service) KeyUnreadable(providerID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.unreadable[providerID]
}

// KeyErrorInfo is the coded "需要重新输入 key" of a provider whose key cannot
// be decrypted, or the "以明文保存" of one whose key is stored in the clear
// (nil otherwise).
func (s *Service) KeyErrorInfo(p model.Provider) *model.ErrorInfo {
	switch {
	case s.KeyUnreadable(p.ID):
		return &model.ErrorInfo{Code: CodeProviderKeyUnreadable, Params: map[string]any{"provider": p.Name},
			Detail: fmt.Sprintf("提供商「%s」保存的 API key 无法在这台电脑或这个账户上解密（数据目录可能来自别的电脑或账户），需要重新输入。输入之前，请先确认这个提供商的地址是你设置的；如果不是，先把地址改回正确的再输入", p.Name)}
	case s.KeyUnprotected(p.ID):
		return &model.ErrorInfo{Code: CodeProviderKeyUnprotected, Params: map[string]any{"provider": p.Name},
			Detail: fmt.Sprintf("提供商「%s」的 API key 以明文保存（保存时无法建立加密密钥），诊断页有详情", p.Name)}
	}
	return nil
}

// updateProviderRow writes p. clearKey: base_url moved to another origin
// without a new key (sec2-app L2): the stored key is removed whatever it
// is, also one that cannot be decrypted here (db.ProviderKeyClearer, sec3
// I4). Otherwise db.UpdateProvider keeps a key that cannot be decrypted
// when p carries none (the empty APIKey it was read with; sec3 I4), so the
// right key file still opens it.
func (s *Service) updateProviderRow(ctx context.Context, p model.Provider, clearKey bool) error {
	if clearKey {
		p.APIKey = ""
		if c, ok := s.st.(db.ProviderKeyClearer); ok {
			return c.UpdateProviderClearKey(ctx, p)
		}
		// a Store without the method (a test's wrapper): a readable key is
		// removed by the empty APIKey; an unreadable one stays bound to the
		// old origin, where it no longer opens (sec3 L3)
	}
	return s.st.UpdateProvider(ctx, p)
}

// scrubFreedKey runs the database's secure VACUUM after a transaction
// deleted a provider with a key (sec2-app L5). A failure is only a warning:
// the provider is deleted, a copy of the key may stay in the WAL until the
// next checkpoint.
func (s *Service) scrubFreedKey(ctx context.Context) {
	sc, ok := s.st.(db.SecretScrubber)
	if !ok {
		return
	}
	if err := sc.ScrubFreedSecrets(ctx); err != nil {
		slog.Warn("settings: the deleted provider's API key may remain in the database's WAL until the next checkpoint", "err", err)
	}
}
