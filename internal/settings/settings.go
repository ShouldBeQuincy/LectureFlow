// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package settings owns the settings that v1.1 moved from config.toml into
// the database (SPEC-v1.1 A5): LLM providers (with their API keys), the five
// role assignments, the ASR settings and the general settings, plus the
// one-time import of the legacy config.toml fields.
//
// The exported API below is FROZEN by the lead at the start of v1.1.
// Bodies: settings-builder.
//
// Reads are served from an in-memory cache (never touch the database) so
// the scheduler, the llm client and the API can call them on every request.
// Writes go to the database first, then refresh the cache, then call the
// OnChange hooks (outside any lock).
//
// API keys never leave this package except through Provider/Providers (for
// the llm client and the balance query). Nothing here logs a key.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/tariff"
)

var (
	// ErrRoleNotConfigured: a role has no provider/model, or its provider is
	// gone. The message names the role in Chinese, e.g.
	// "请先在设置中为「翻译」选择提供商和模型".
	ErrRoleNotConfigured = errors.New("role not configured")
	// ErrProviderInUse: DeleteProvider of a provider that a role uses (409).
	ErrProviderInUse = errors.New("provider in use")
	// ErrInvalid: invalid input (400); wrapped with a Chinese message.
	ErrInvalid = errors.New("invalid settings")
)

// RoleLabel is the Chinese name of a role (A5.3): 大纲整理、术语提取、摘要、修复、翻译;
// v2: 术语表清道夫 (G3.1).
func RoleLabel(role string) string {
	switch role {
	case model.RoleSyllabusFormat:
		return "大纲整理"
	case model.RoleGlossary:
		return "术语提取"
	case model.RoleGlossaryJanitor:
		return "术语表清道夫"
	case model.RoleSummary:
		return "摘要"
	case model.RoleRepair:
		return "修复"
	case model.RoleTranslate:
		return "翻译"
	case model.RoleChapters:
		return "章节"
	case model.RoleOrganizer:
		return "标题与简介"
	}
	return role
}

// DefaultBaseURL is the base_url prefilled when a provider type is chosen (A5.3).
func DefaultBaseURL(providerType string) string {
	switch providerType {
	case model.ProviderAnthropic:
		return "https://api.anthropic.com"
	case model.ProviderOpenAI:
		return "https://api.deepseek.com"
	}
	return ""
}

// ProviderInput creates (all of Name, Type, BaseURL required; APIKey may be
// empty for local servers) or patches (nil = unchanged; APIKey nil or ""
// = unchanged, A5.2) a provider.
type ProviderInput struct {
	Name           *string
	Type           *string
	BaseURL        *string
	APIKey         *string
	MaxConcurrency *int
	RPM            *int
	BalanceAPI     *string
	PriceInput     *float64
	PriceCacheHit  *float64
	PriceOutput    *float64
	Currency       *string
	// ClearPrices removes all three prices (a PATCH cannot send null through *float64).
	ClearPrices bool
	// v2 P12.2 / D3: Tariff replaces the time-of-day pricing (raw JSON of
	// tariff.Tariff, validated with tariff.Parse; "" or "null" = none); nil
	// = unchanged. ClearTariff removes it (the API's "tariff": null). A new
	// DeepSeek provider without either gets tariff.DeepSeekPreset().
	Tariff      *string
	ClearTariff bool
	// OffpeakOnly is the P12.6 switch; it needs a tariff with peak windows
	// and is switched off when the tariff is removed. nil = unchanged.
	OffpeakOnly *bool
}

// DeleteInfo is the result of DeleteProvider: SnapshotRefs is the number of
// current lecture jobs whose snapshot still names the provider (A5.3: "使用该
// 提供商的旧讲座将无法沿用原模型重跑").
type DeleteInfo struct {
	SnapshotRefs int
}

// Service is the settings store and cache.
type Service struct {
	st  db.Store
	cfg *config.Config

	mu        sync.RWMutex
	providers []model.Provider
	roles     map[string]model.RoleSetting
	asr       model.ASRSettings
	general   model.GeneralSettings
	hooks     []func()
	// defaults: roles whose value ApplyRoleDefaults filled in and the user
	// has not changed since (kv v2.role_default:{role} = "1"; v2 appendix D1).
	defaults map[string]bool
	// v2 P12: the parsed provider tariffs (by provider id; providers without
	// a valid tariff are absent) and the holiday data in effect (kv
	// tariff.holidays_cn when the user edited it, else the embedded file).
	tariffs        map[string]tariff.Tariff
	holidays       tariff.HolidaySet
	holidaysEdited bool
	// devices is the device registry of SPEC-v2 I6 (SetDeviceSource; nil =
	// the legacy ASR fields, as before phase 3b).
	devices DeviceSource
	// unreadable: providers whose stored key no data key opens (cred-enc,
	// keys.go): their APIKey is "", the user enters the key again.
	unreadable map[string]bool
	// unsealed: providers whose key is stored in the clear (sec3 L2,
	// keys.go: saved while there was no data key).
	unsealed map[string]bool

	// wmu serialises writes (database write + cache reload) so a slower
	// reload can never store an older state over a newer one. It is never
	// held by readers, and mu is never held across database I/O (a caller
	// inside a Store.Tx may read settings while a write waits for the
	// single database connection).
	wmu sync.Mutex
}

// New returns a service on st. cfg supplies the legacy fields for
// ImportLegacy and the defaults; it may be config.Default().
func New(st db.Store, cfg *config.Config) *Service {
	if cfg == nil {
		cfg = config.Default()
	}
	return &Service{st: st, cfg: cfg, roles: map[string]model.RoleSetting{},
		tariffs: map[string]tariff.Tariff{}, holidays: tariff.HolidaysCN()}
}

// Open is New + ImportLegacy + Load (what main and most tests want).
func Open(ctx context.Context, st db.Store, cfg *config.Config) (*Service, error) {
	s := New(st, cfg)
	if _, err := s.ImportLegacy(ctx); err != nil {
		return nil, err
	}
	return s, s.Load(ctx)
}

// ImportLegacy implements the first-start import of A5.1: when kv
// settings.imported_v1 is not set and the database has neither providers
// nor kv settings.asr, it imports cfg.LLM.Providers (id = name = the config
// key, type openai, key = Provider.Key() read from the environment at that
// moment, max_concurrency, rpm; balance_api deepseek when the base_url host
// is api.deepseek.com; prices from cfg.LLM.Prices of the model used by the
// roles), cfg.LLM.Roles (glossary, summary, repair, translate with
// temperature unset and translate window; syllabus_format gets the glossary
// role's provider/model), cfg.ASR into settings.asr, cfg.Storage and
// cfg.LLM.DailyBudget into settings.general. Then sets the kv flag (also
// when nothing was imported). imported reports whether rows were written.
func (s *Service) ImportLegacy(ctx context.Context) (imported bool, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	var noKey []string
	err = s.st.Tx(ctx, func(tx db.Store) error {
		if v, ok, err := tx.GetKV(ctx, model.KVSettingsImported); err != nil {
			return err
		} else if ok && v != "" {
			return nil
		}
		provs, err := tx.ListProviders(ctx)
		if err != nil {
			return err
		}
		_, hasASR, err := tx.GetKV(ctx, model.KVSettingsASR)
		if err != nil {
			return err
		}
		if len(provs) == 0 && !hasASR {
			noKey, err = importLegacy(ctx, tx, s.config())
			if err != nil {
				return err
			}
			imported = true
		}
		return tx.SetKV(ctx, model.KVSettingsImported, "1")
	})
	if err != nil {
		return false, fmt.Errorf("导入 config.toml 中的旧设置失败: %w", err)
	}
	if imported {
		slog.Info("imported legacy settings from config.toml (one time)")
		for _, name := range noKey {
			// Names only: the key itself is never logged (A5.2).
			slog.Warn("imported LLM provider has no API key; set it on the settings page", "provider", name)
		}
	}
	return imported, nil
}

// Load (re)reads providers, roles and kv settings into the cache, filling
// defaults for missing kv settings (ASR: large-v3, auto, cuda, en, 1, 1000,
// 5000, and since phase 3b device_mode auto, multi_device true,
// battery_aware true, mem_reserve_mb 2048; general: keep media true,
// notifications true, budget 0).
func (s *Service) Load(ctx context.Context) error {
	provs, err := s.st.ListProviders(ctx)
	if err != nil {
		return fmt.Errorf("读取提供商失败: %w", err)
	}
	rows, err := s.st.ListRoleSettings(ctx)
	if err != nil {
		return fmt.Errorf("读取角色分配失败: %w", err)
	}
	asr := DefaultASR()
	if v, ok, err := s.st.GetKV(ctx, model.KVSettingsASR); err != nil {
		return err
	} else if ok && strings.TrimSpace(v) != "" {
		if err := json.Unmarshal([]byte(v), &asr); err != nil {
			slog.Warn("kv settings.asr is not valid JSON; using defaults", "err", err)
			asr = DefaultASR()
		}
	}
	asr = fillASR(asr)
	general := DefaultGeneral()
	if v, ok, err := s.st.GetKV(ctx, model.KVSettingsGeneral); err != nil {
		return err
	} else if ok && strings.TrimSpace(v) != "" {
		if err := json.Unmarshal([]byte(v), &general); err != nil {
			slog.Warn("kv settings.general is not valid JSON; using defaults", "err", err)
			general = DefaultGeneral()
		}
	}
	general = normalizeGeneral(general)
	roles := make(map[string]model.RoleSetting, len(rows))
	for _, r := range rows {
		roles[r.Role] = r
	}
	defaults, err := loadDefaultMarks(ctx, s.st)
	if err != nil {
		return err
	}
	tariffs := parseTariffs(provs)
	holidays, edited, err := loadHolidays(ctx, s.st)
	if err != nil {
		return err
	}
	unreadable, err := unreadableKeys(ctx, s.st)
	if err != nil {
		return fmt.Errorf("读取提供商失败: %w", err)
	}
	unsealed, err := unsealedKeys(ctx, s.st)
	if err != nil {
		return fmt.Errorf("读取提供商失败: %w", err)
	}
	s.mu.Lock()
	s.providers, s.roles, s.asr, s.general, s.defaults = provs, roles, asr, general, defaults
	s.tariffs, s.holidays, s.holidaysEdited = tariffs, holidays, edited
	s.unreadable, s.unsealed = unreadable, unsealed
	s.mu.Unlock()
	return nil
}

// OnChange registers fn, called after every successful write (providers,
// roles, ASR, general). The API pool uses it for A5.4 hot updates.
func (s *Service) OnChange(fn func()) {
	s.mu.Lock()
	s.hooks = append(s.hooks, fn)
	s.mu.Unlock()
}

// ---------- reads (cache only) ----------

// Providers returns all providers ordered by created_at.
func (s *Service) Providers() []model.Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Provider, len(s.providers))
	for i, p := range s.providers {
		out[i] = cloneProvider(p)
	}
	return out
}

// Provider returns one provider by id.
func (s *Service) Provider(id string) (model.Provider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.providerLocked(id); ok {
		return cloneProvider(p), true
	}
	return model.Provider{}, false
}

// Roles returns the role settings keyed by role (missing roles absent).
func (s *Service) Roles() map[string]model.RoleSetting {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]model.RoleSetting, len(s.roles))
	for k, r := range s.roles {
		out[k] = cloneRole(r)
	}
	return out
}

// ASR returns the ASR settings with defaults applied. ManualDevices is a
// copy: PUT /api/settings/asr decodes the request into the returned value
// (lead ruling 15), which must never write into the cache's array.
func (s *Service) ASR() model.ASRSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.asr.Model == "" { // before Load
		return DefaultASR()
	}
	a := s.asr
	a.ManualDevices = slices.Clone(a.ManualDevices)
	return a
}

// General returns the general settings with defaults applied.
func (s *Service) General() model.GeneralSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneGeneral(s.general)
}

// RoleTarget resolves the current setting of role into an LLMSnapshot with
// resolved params: temperature = params.temperature or
// model.DefaultTemperature(role); window (translate only) = params.window or
// model.DefaultTranslateWindow. ErrRoleNotConfigured (wrapped with the
// Chinese message) when the role has no provider/model or the provider no
// longer exists. The batch size is only what the role sets (no provider
// default): a request made without a job snapshot (llm.HTTPClient on the
// role's current setting) is sized as before; SnapshotTarget is what a
// job's snapshot records.
func (s *Service) RoleTarget(role string) (model.LLMSnapshot, error) {
	return s.roleTarget(role, false)
}

// SnapshotTarget is RoleTarget as a job's snapshot records it (launch and
// CurrentSnapshots, retranslate with the current setting): a translate role
// without a batch size gets the default of its provider and model when that
// is above 1 (resolveRoleParams, DefaultBatch: 10 for a Claude model on an
// Anthropic provider; user 2026-11-11). The default is fixed
// in the snapshot when it is written; the stage reads only the snapshot.
func (s *Service) SnapshotTarget(role string) (model.LLMSnapshot, error) {
	return s.roleTarget(role, true)
}

func (s *Service) roleTarget(role string, snapshot bool) (model.LLMSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.roles[role]
	if !ok || r.ProviderID == nil || *r.ProviderID == "" || r.Model == nil || strings.TrimSpace(*r.Model) == "" {
		return model.LLMSnapshot{}, userErr(ErrRoleNotConfigured, CodeRoleNotConfigured, map[string]any{"role": role}, "请先在设置中为「%s」选择提供商和模型", RoleLabel(role))
	}
	p, ok := s.providerLocked(*r.ProviderID)
	if !ok {
		return model.LLMSnapshot{}, userErr(ErrRoleNotConfigured, CodeRoleProviderDeleted, map[string]any{"role": role}, "「%s」使用的提供商已删除，请先在设置中为它重新选择提供商和模型", RoleLabel(role))
	}
	params := ResolveParams(role, parseParams(r.Params))
	if snapshot {
		params = resolveRoleParams(role, p, *r.Model, parseParams(r.Params)) // translate: default batch by provider and model
	}
	return model.LLMSnapshot{
		ProviderID:   p.ID,
		ProviderName: p.Name,
		Model:        strings.TrimSpace(*r.Model),
		Params:       params,
	}, nil
}

// CurrentSnapshots returns the current settings as the snapshots launch
// writes (A4.1): summary/repair/translate from SnapshotTarget, asr from
// CurrentASR (SPEC-v2 I6: the device registry's snapshot; without one the
// legacy model, compute_type, device, language of ASR()), prep/export
// model.EmptySnapshot. Error (ErrRoleNotConfigured) when one of the three
// lecture roles is not configured.
func (s *Service) CurrentSnapshots() (map[string]string, error) {
	out := map[string]string{
		model.StagePrep:   model.EmptySnapshot,
		model.StageExport: model.EmptySnapshot,
	}
	for _, stage := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		t, err := s.SnapshotTarget(model.RoleOfStage(stage))
		if err != nil {
			return nil, err
		}
		t.PromptVersion = model.PromptVersionOf(stage) // v2 G5.5
		out[stage] = model.MarshalSnapshot(t)
	}
	out[model.StageASR] = s.CurrentASR()
	return out, nil
}

// SnapshotUsable is SnapshotUsableInfo with the reason as Chinese text
// (e.g. "提供商「X」已删除"; the ErrorInfo's Detail).
func (s *Service) SnapshotUsable(stage, raw string) (ok bool, reason string) {
	ok, info := s.SnapshotUsableInfo(stage, raw)
	if info != nil {
		reason = info.Detail
	}
	return ok, reason
}

// SnapshotUsableInfo implements state.UsableFunc (A4.3): an LLM snapshot is
// usable when its provider exists and has a non-empty key (local servers
// without a key: base_url host is 127.0.0.1/localhost). An empty LLM
// snapshot is not usable ("没有记录原模型配置": keeping it would fail again,
// lead ruling after settings-builder's report). ASR and non-LLM snapshots are
// always usable. The reason is coded (v2 P14.3, codes.go) with the Chinese
// text as its Detail; nil when usable.
func (s *Service) SnapshotUsableInfo(stage, raw string) (ok bool, reason *model.ErrorInfo) {
	switch stage {
	case model.StageSummary, model.StageRepair, model.StageTranslate:
	default:
		return true, nil
	}
	if model.IsEmptySnapshot(raw) {
		return false, &model.ErrorInfo{Code: CodeSnapshotNotRecorded, Detail: "没有记录原模型配置"}
	}
	snap, valid := model.ParseLLMSnapshot(raw)
	if !valid {
		return false, &model.ErrorInfo{Code: CodeSnapshotInvalid, Detail: "模型配置快照无效"}
	}
	p, found := s.Provider(snap.ProviderID)
	if !found {
		name := snapshotProviderName(snap)
		return false, &model.ErrorInfo{Code: CodeSnapshotProviderDeleted, Params: map[string]any{"provider": name},
			Detail: fmt.Sprintf("提供商「%s」已删除", name)}
	}
	if p.APIKey == "" && s.KeyUnreadable(p.ID) { // cred-enc (keys.go)
		return false, &model.ErrorInfo{Code: CodeSnapshotKeyUnreadable, Params: map[string]any{"provider": p.Name},
			Detail: fmt.Sprintf("提供商「%s」保存的 API key 无法在这台电脑或这个账户上解密，需要重新输入。输入之前，请先确认这个提供商的地址是你设置的；如果不是，先把地址改回正确的再输入", p.Name)}
	}
	if p.APIKey == "" && !IsLocal(p.BaseURL) {
		return false, &model.ErrorInfo{Code: CodeSnapshotNoAPIKey, Params: map[string]any{"provider": p.Name},
			Detail: fmt.Sprintf("提供商「%s」没有设置 API key", p.Name)}
	}
	return true, nil
}

// ProbedModels returns the cached "探测模型" result of a provider (kv provider_models:{id}).
func (s *Service) ProbedModels(ctx context.Context, providerID string) (model.ProbedModels, bool, error) {
	return probedModels(ctx, s.st, providerID)
}

// errProbeCache: kv provider_models:{id} is not valid JSON.
var errProbeCache = errors.New("模型探测缓存损坏")

// probedModels reads kv provider_models:{id} through st (the Service's store
// or a transaction).
func probedModels(ctx context.Context, st db.Store, providerID string) (model.ProbedModels, bool, error) {
	v, ok, err := st.GetKV(ctx, model.KVProviderModelsPref+providerID)
	if err != nil || !ok || strings.TrimSpace(v) == "" {
		return model.ProbedModels{}, false, err
	}
	var pm model.ProbedModels
	if err := json.Unmarshal([]byte(v), &pm); err != nil {
		return model.ProbedModels{}, false, fmt.Errorf("%w: %w", errProbeCache, err)
	}
	return pm, true, nil
}

// ---------- writes ----------

// CreateProvider validates (name non-empty and unique, type openai|anthropic,
// base_url http(s), max_concurrency 1..512, rpm 0..10000 with 0 = no
// per-minute limit (perf-llm), balance_api
// none|deepseek, prices >= 0, tariff) and inserts a provider with a new
// ULID. v2 P12.2 / D3: a DeepSeek provider (IsDeepSeek) created without a
// tariff in the input gets tariff.DeepSeekPreset().
func (s *Service) CreateProvider(ctx context.Context, in ProviderInput) (model.Provider, error) {
	if in.Name == nil || in.Type == nil || in.BaseURL == nil {
		return model.Provider{}, userErr(ErrInvalid, CodeProviderFieldsMissing, nil, "请填写提供商的名称、类型和 base_url")
	}
	p := model.Provider{
		ID:             db.NewID(),
		MaxConcurrency: DefaultMaxConcurrency,
		RPM:            DefaultRPM,
		CreatedAt:      db.Now(),
	}
	if strings.TrimSpace(*in.Type) == model.ProviderAnthropic {
		// v3.2.x P2 (Q13a): a new Anthropic-type provider has no per-minute
		// limit of its own (the 429 handling lowers the concurrency); an rpm
		// in the input still wins. Existing providers keep their values.
		p.MaxConcurrency, p.RPM = DefaultAnthropicMaxConcurrency, DefaultAnthropicRPM
	}
	if in.BalanceAPI == nil {
		// Prefill like the settings page does: DeepSeek has a balance API.
		b := model.BalanceNone
		if isDeepSeek(strings.TrimSpace(*in.BaseURL)) {
			b = model.BalanceDeepSeek
		}
		in.BalanceAPI = &b
	}
	if in.Tariff == nil && !in.ClearTariff && IsDeepSeek(*in.BaseURL, *in.BalanceAPI) {
		preset := DeepSeekPresetJSON()
		in.Tariff = &preset
	}
	if err := applyInput(&p, in); err != nil {
		return model.Provider{}, err
	}
	s.wmu.Lock()
	err := s.st.InsertProvider(ctx, p)
	if err == nil {
		err = s.Load(ctx)
	}
	s.wmu.Unlock()
	if err != nil {
		return model.Provider{}, providerWriteErr(err, p.Name)
	}
	s.changed()
	return cloneProvider(p), nil
}

// UpdateProvider applies a patch (same validation). APIKey nil or "" keeps
// the key — unless the patch moves base_url to another origin (scheme, host
// or port): the key is then cleared, so it never follows the provider to a
// host it was not issued for (sec2-app L2); the user enters it again.
// A changed base_url or type also clears the probe cache
// (kv provider_models:{id}); the OnChange hooks then tell the front end.
func (s *Service) UpdateProvider(ctx context.Context, id string, in ProviderInput) (model.Provider, error) {
	s.wmu.Lock()
	p, err := s.st.GetProvider(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		s.wmu.Unlock()
		return model.Provider{}, userErr(db.ErrNotFound, CodeProviderNotFound, nil, "提供商不存在（可能已被删除）")
	}
	if err == nil {
		oldBase, oldType := p.BaseURL, p.Type
		if err = applyInput(&p, in); err != nil {
			s.wmu.Unlock()
			return model.Provider{}, err
		}
		newKey := in.APIKey != nil && strings.TrimSpace(*in.APIKey) != ""
		clearKey := !newKey && !SameOrigin(oldBase, p.BaseURL)
		if clearKey {
			p.APIKey = ""
			slog.Info("provider moved to another host; its API key was cleared", "provider", p.Name)
		}
		// cred-enc / sec3 I4: a key to enter again stays stored while other
		// fields change; moving to another origin clears it explicitly
		err = s.updateProviderRow(ctx, p, clearKey)
		if err == nil && (p.BaseURL != oldBase || p.Type != oldType) {
			// T041: the cached "探测模型" result came from the old address or
			// API type; drop it (as DeleteProvider does) so the settings page
			// shows no stale model list or probe error. No new probe here:
			// that would send a request to the new address unasked.
			err = s.st.SetKV(ctx, model.KVProviderModelsPref+id, "")
		}
	}
	if err == nil {
		err = s.Load(ctx)
	}
	s.wmu.Unlock()
	if err != nil {
		return model.Provider{}, providerWriteErr(err, p.Name)
	}
	s.changed()
	return cloneProvider(p), nil
}

// DeleteProvider deletes a provider unless a role uses it (ErrProviderInUse,
// wrapped with "提供商正被「翻译」等角色使用，请先改用其他提供商"). It also
// deletes the probe cache. DeleteInfo counts old snapshot references.
func (s *Service) DeleteProvider(ctx context.Context, id string) (DeleteInfo, error) {
	refs, err := s.SnapshotRefs(ctx, id)
	if err != nil {
		return DeleteInfo{}, err
	}
	hadKey := false
	if p, ok := s.Provider(id); ok {
		hadKey = p.APIKey != "" || s.KeyUnreadable(id)
	}
	s.wmu.Lock()
	err = s.st.Tx(ctx, func(tx db.Store) error {
		if _, err := tx.GetProvider(ctx, id); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return userErr(db.ErrNotFound, CodeProviderNotFound, nil, "提供商不存在（可能已被删除）")
			}
			return err
		}
		rows, err := tx.ListRoleSettings(ctx)
		if err != nil {
			return err
		}
		if used := rolesUsing(rows, id); len(used) > 0 {
			return inUseErr(used)
		}
		if err := tx.DeleteProvider(ctx, id); err != nil {
			return err
		}
		// The Store has no kv delete: an empty value reads as "no cache".
		return tx.SetKV(ctx, model.KVProviderModelsPref+id, "")
	})
	if err == nil && hadKey {
		s.scrubFreedKey(ctx) // cred-enc / L5: the deleted key leaves no copy (keys.go)
	}
	if err == nil {
		err = s.Load(ctx)
	}
	s.wmu.Unlock()
	if err != nil {
		return DeleteInfo{}, err
	}
	s.changed()
	return DeleteInfo{SnapshotRefs: refs}, nil
}

// UsedByRoles lists the roles whose setting uses the provider (settings page).
func (s *Service) UsedByRoles(providerID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := make([]model.RoleSetting, 0, len(s.roles))
	for _, r := range s.roles {
		rows = append(rows, r)
	}
	return rolesUsing(rows, providerID)
}

// SnapshotRefs counts current lecture jobs whose snapshot names the provider.
func (s *Service) SnapshotRefs(ctx context.Context, providerID string) (int, error) {
	jobs, err := s.st.ListCurrentJobsByStatus(ctx, model.StatusPending, model.StatusQueued, model.StatusRunning,
		model.StatusDone, model.StatusFailed, model.StatusCancelled, model.StatusStale)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if j.Scope != model.ScopeLecture {
			continue
		}
		switch j.Stage {
		case model.StageSummary, model.StageRepair, model.StageTranslate:
		default:
			continue
		}
		if snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot); ok && snap.ProviderID == providerID {
			n++
		}
	}
	return n, nil
}

// SetRole writes one role (A5.3 角色分配). providerID must exist; model
// non-empty; temperature in [0, 2]; window (translate only) in [0, 10];
// thinking one of model.ThinkingLevels, and other than off only for a
// provider that SupportsThinking. Thinking off ("off", "" or absent) is
// stored as absent, so the row is what it was before the option existed.
// v3.2.x: max_tokens absent (automatic) or in [1, MaxTokensLimit]; batch
// (translate only) absent or in [1, MaxBatch] (checkParams, manual.go).
// The params replace the stored ones, so leaving a field out clears it.
// It clears the role's appendix D1 default mark (RoleDefault).
func (s *Service) SetRole(ctx context.Context, role, providerID, modelName string, params model.RoleParams) error {
	if !validRole(role) {
		return userErr(ErrInvalid, CodeRoleUnknown, map[string]any{"role": role}, "未知的角色 %q", role)
	}
	prov, ok := s.Provider(providerID)
	if !ok || providerID == "" {
		return userErr(ErrInvalid, CodeRoleProviderMissing, map[string]any{"role": role}, "请为「%s」选择一个已存在的提供商", RoleLabel(role))
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return userErr(ErrInvalid, CodeRoleModelMissing, map[string]any{"role": role}, "请为「%s」选择或填写模型名", RoleLabel(role))
	}
	if err := checkParams(role, prov, &params); err != nil {
		return err
	}
	b, _ := json.Marshal(params)
	row := model.RoleSetting{Role: role, ProviderID: &providerID, Model: &modelName, Params: string(b)}
	s.wmu.Lock()
	err := s.st.Tx(ctx, func(tx db.Store) error {
		if err := tx.UpsertRoleSetting(ctx, row); err != nil {
			return err
		}
		return clearDefaultMark(ctx, tx, role)
	})
	if err == nil {
		err = s.Load(ctx)
	}
	s.wmu.Unlock()
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

// SetASR validates (compute_type auto|float16|int8_float16, device
// cuda|cpu, language non-empty, max_parallel 1..8, vram_reserve_mb 0..65536;
// VRAMPerJobMB 0 keeps the old value; SPEC-v2 I8: device_mode auto or an
// existing asr_device row, "" meaning auto, mem_reserve_mb 0..65536; power
// management: device_mode manual with manual_devices, checkDeviceMode) and
// writes kv settings.asr, normalized (model.NormalizeASRDeviceMode). It
// replaces the whole value: PUT /api/settings/asr merges the request into
// the current settings with UpdateASR (lead ruling 15).
func (s *Service) SetASR(ctx context.Context, a model.ASRSettings) error {
	return s.UpdateASR(ctx, func(cur *model.ASRSettings) error {
		*cur = a
		return nil
	})
}

// UpdateASR is SetASR on the result of edit applied to the current
// settings (a copy), read and written under wmu (r4 D9): two requests that
// change different fields at the same time both keep their change, where a
// read before the write lock let the later one store its stale copy of the
// other's field. An error from edit is returned as it is, nothing written.
// edit runs while other writes wait: it must not block (decode a body read
// beforehand, never read the request itself).
func (s *Service) UpdateASR(ctx context.Context, edit func(*model.ASRSettings) error) error {
	s.wmu.Lock()
	err := s.updateASRLocked(ctx, edit)
	s.wmu.Unlock()
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

func (s *Service) updateASRLocked(ctx context.Context, edit func(*model.ASRSettings) error) error {
	a := s.ASR()
	oldPerJob := a.VRAMPerJobMB
	if err := edit(&a); err != nil {
		return err
	}
	a.Model = strings.TrimSpace(a.Model)
	a.Language = strings.TrimSpace(a.Language)
	if a.DeviceMode = strings.TrimSpace(a.DeviceMode); a.DeviceMode == "" {
		a.DeviceMode = model.ASRDeviceModeAuto
	}
	switch {
	case a.Model == "":
		return userErr(ErrInvalid, CodeASRModelMissing, nil, "请填写转写模型（例如 large-v3）")
	case a.ComputeType != "auto" && a.ComputeType != "float16" && a.ComputeType != "int8_float16":
		return userErr(ErrInvalid, CodeComputeTypeInvalid, nil, "compute_type 只能是 auto、float16 或 int8_float16")
	case a.Device != "cuda" && a.Device != "cpu":
		return userErr(ErrInvalid, CodeDeviceInvalid, nil, "设备只能是 cuda 或 cpu")
	case a.Language == "":
		return userErr(ErrInvalid, CodeLanguageMissing, nil, "请填写语言（例如 en）")
	case a.MaxParallel < 1 || a.MaxParallel > 8:
		return userErr(ErrInvalid, CodeMaxParallelInvalid, map[string]any{"min": 1, "max": 8}, "最大并行数必须在 1 到 8 之间")
	case a.VRAMReserveMB < 0 || a.VRAMReserveMB > 65536:
		return userErr(ErrInvalid, CodeVRAMReserveInvalid, map[string]any{"min": 0, "max": 65536}, "保留显存必须在 0 到 65536 MB 之间")
	case a.VRAMPerJobMB < 0 || a.VRAMPerJobMB > 65536:
		return userErr(ErrInvalid, CodeVRAMPerJobInvalid, map[string]any{"min": 0, "max": 65536}, "每个转写任务的预估显存必须在 0 到 65536 MB 之间")
	case a.MemReserveMB < 0 || a.MemReserveMB > MaxMemReserveMB:
		return userErr(ErrInvalid, CodeMemReserveInvalid, map[string]any{"min": 0, "max": MaxMemReserveMB},
			"保留内存必须在 0 到 %d MB 之间", MaxMemReserveMB)
	}
	// The device registry has its own lock and never calls back into the
	// settings, so checking it under wmu cannot deadlock.
	if err := s.checkDeviceMode(ctx, a.DeviceMode, a.ManualDevices); err != nil {
		return err
	}
	// Stored normalized (power-brief §3.2): auto without a list, or manual
	// with the trimmed, de-duplicated list; a 3b single id as manual [id].
	a = model.NormalizeASRDeviceMode(a)
	if a.VRAMPerJobMB == 0 {
		a.VRAMPerJobMB = oldPerJob
	}
	b, _ := json.Marshal(a)
	if err := s.st.SetKV(ctx, model.KVSettingsASR, string(b)); err != nil {
		return err
	}
	return s.Load(ctx)
}

// SetGeneral validates (daily_budget >= 0; v3.2.x: llm_idle_timeout_s 0 or
// in MinLLMIdleTimeoutS..MaxLLMIdleTimeoutS when set) and writes kv settings.general. A nil
// auto_chapters or llm_idle_timeout_s is stored as absent (= the default).
func (s *Service) SetGeneral(ctx context.Context, g model.GeneralSettings) error {
	return s.UpdateGeneral(ctx, func(cur *model.GeneralSettings) error {
		*cur = g
		return nil
	})
}

// UpdateGeneral is SetGeneral on the result of edit applied to the current
// settings (a copy with its own optional values), read and written under
// wmu (r4 D9, as UpdateASR): PUT /api/settings/general merges the body into
// the current settings here, so two pages changing different fields at the
// same time both keep their change. An error from edit is returned as it
// is, nothing written; edit must not block.
func (s *Service) UpdateGeneral(ctx context.Context, edit func(*model.GeneralSettings) error) error {
	s.wmu.Lock()
	err := s.updateGeneralLocked(ctx, edit)
	s.wmu.Unlock()
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

func (s *Service) updateGeneralLocked(ctx context.Context, edit func(*model.GeneralSettings) error) error {
	g := s.General() // a clone: edit cannot write through to the cache
	if err := edit(&g); err != nil {
		return err
	}
	if !(g.DailyBudget >= 0) || g.DailyBudget > 1e12 {
		return userErr(ErrInvalid, CodeBudgetInvalid, nil, "每日预算必须是不小于 0 的数（0 表示不限制）")
	}
	if v := g.LLMIdleTimeoutS; v != nil && !validIdleTimeoutS(*v) {
		return userErr(ErrInvalid, CodeIdleTimeoutInvalid, map[string]any{"min": MinLLMIdleTimeoutS, "max": MaxLLMIdleTimeoutS},
			"模型请求的空闲超时必须是 0（关闭）或 %d 到 %d 之间的整数秒", MinLLMIdleTimeoutS, MaxLLMIdleTimeoutS)
	}
	g = cloneGeneral(g)
	b, _ := json.Marshal(g)
	if err := s.st.SetKV(ctx, model.KVSettingsGeneral, string(b)); err != nil {
		return err
	}
	return s.Load(ctx)
}

// SetProbedModels stores a probe result (kv provider_models:{id}).
func (s *Service) SetProbedModels(ctx context.Context, providerID string, pm model.ProbedModels) error {
	b, err := marshalProbed(pm)
	if err != nil {
		return err
	}
	return s.st.SetKV(ctx, model.KVProviderModelsPref+providerID, string(b))
}

// SetProbedModelsFor stores the result of a probe made against from (the
// provider as it was read when the probe started), unless the provider has
// since moved to another type, base_url or key (r4 D3): a probe can take
// many seconds, and its late result must not bring back a model list (or a
// probe error) of an address the user has already left — UpdateProvider
// cleared that cache. stored is false when the result was discarded. The
// comparison and the write run under wmu, the lock every provider write
// holds until its cache reload is done, so the cache read here is the
// committed state. A provider deleted meanwhile is CodeProviderNotFound.
func (s *Service) SetProbedModelsFor(ctx context.Context, from model.Provider, pm model.ProbedModels) (stored bool, err error) {
	b, err := marshalProbed(pm)
	if err != nil {
		return false, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	cur, ok := s.Provider(from.ID)
	if !ok {
		return false, userErr(db.ErrNotFound, CodeProviderNotFound, nil, "提供商不存在（可能已被删除）")
	}
	if cur.Type != from.Type || cur.BaseURL != from.BaseURL || cur.APIKey != from.APIKey {
		slog.Info("probe result discarded: the provider changed while it ran", "provider", cur.Name)
		return false, nil
	}
	if err := s.st.SetKV(ctx, model.KVProviderModelsPref+from.ID, string(b)); err != nil {
		return false, err
	}
	return true, nil
}

// marshalProbed fills the defaults of a probe result (probed_at now, an
// empty model list) and encodes it.
func marshalProbed(pm model.ProbedModels) ([]byte, error) {
	if pm.ProbedAt == "" {
		pm.ProbedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if pm.Models == nil {
		pm.Models = []string{}
	}
	return json.Marshal(pm)
}
