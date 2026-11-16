// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/settings"
	"lectureflow/internal/tariff"
)

// Settings page and providers (SPEC-v1.1 A5). No response ever carries an
// API key: providers are shown with key_hint only (A5.2).

// snapshotRefs counts, per provider id, the current lecture jobs whose
// snapshot names it (one query for all providers). The SQLite store counts
// the summary, repair and translate jobs by raw snapshot in one GROUP BY
// (db.JobReader, r7 perf: about 1 ms instead of 15 ms on a 400-lecture
// library), and each distinct snapshot is parsed once; another store is
// read in full, with the same counts.
func (s *Server) snapshotRefs(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	if jr, ok := s.st.(db.JobReader); ok {
		bySnap, err := jr.LectureSnapshotCounts(ctx, model.StageSummary, model.StageRepair, model.StageTranslate)
		if err != nil {
			return nil, err
		}
		for raw, n := range bySnap {
			if snap, ok := model.ParseLLMSnapshot(raw); ok {
				out[snap.ProviderID] += n
			}
		}
		return out, nil
	}
	jobs, err := s.st.ListCurrentJobsByStatus(ctx, allStatuses...)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.Scope != model.ScopeLecture {
			continue
		}
		switch j.Stage {
		case model.StageSummary, model.StageRepair, model.StageTranslate:
			if snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot); ok {
				out[snap.ProviderID]++
			}
		}
	}
	return out, nil
}

func (s *Server) providerView(ctx context.Context, p model.Provider, refs map[string]int) ProviderView {
	v := ProviderView{
		ID: p.ID, Name: p.Name, Type: p.Type, BaseURL: p.BaseURL,
		KeyHint:        model.KeyHint(p.APIKey),
		MaxConcurrency: p.MaxConcurrency, RPM: p.RPM, BalanceAPI: p.BalanceAPI,
		PriceInput: p.PriceInput, PriceCacheHit: p.PriceCacheHit, PriceOutput: p.PriceOutput,
		Currency: p.Currency, CreatedAt: p.CreatedAt,
		UsedByRoles:  s.set.UsedByRoles(p.ID),
		SnapshotRefs: refs[p.ID],
		Models:       []string{},
		// thinking per role: DeepSeek and Anthropic only (settings.SupportsThinking)
		SupportsThinking:   settings.SupportsThinking(p),
		DefaultBatchClaude: settings.ClaudeBatch(p), // translate batch for a Claude model when the role leaves it empty
		// cred-enc: "需要重新输入 key" (settings/keys.go)
		KeyUnreadable:  s.set.KeyUnreadable(p.ID),
		KeyErrorInfo:   s.set.KeyErrorInfo(p),
		KeyUnprotected: s.set.KeyUnprotected(p.ID), // sec3 L2
	}
	if v.UsedByRoles == nil {
		v.UsedByRoles = []string{}
	}
	// v2 P12: the tariff as stored (raw JSON, null = none), the off-peak
	// switch and the current peak state (the same computation as the
	// resource watcher's provider rows).
	if p.Tariff != nil && strings.TrimSpace(*p.Tariff) != "" {
		v.Tariff = json.RawMessage(*p.Tariff)
	}
	v.OffpeakOnly = p.OffpeakOnly
	if ts := resources.TariffStateOf(p, s.now(), s.set.HolidaySet()); ts != nil {
		x := TariffState(*ts)
		v.TariffState = &x
	}
	if pm, ok, err := s.set.ProbedModels(ctx, p.ID); err == nil && ok {
		if pm.Models != nil {
			v.Models = pm.Models
		}
		if pm.ProbedAt != "" {
			at := pm.ProbedAt
			v.ModelsProbedAt = &at
		}
		v.ModelsError, v.ModelsErrorInfo = pm.Error, pm.ErrorInfo
	}
	// T-PERF-03: the account limits the provider's answers reported (the
	// llm client keeps them; a test's or legacy client may not)
	if src, ok := s.llmc.(rateLimitSource); ok {
		if l, ok := src.RateLimits(p.ID); ok {
			v.RateLimits = RateLimitsView(l, p.MaxConcurrency, p.RPM)
		}
	}
	return v
}

// rateLimitSource is the part of the llm client (*llm.HTTPClient) that
// knows the limits its answers reported.
type rateLimitSource interface {
	RateLimits(providerID string) (llm.RateLimits, bool)
}

// RateLimitsView turns observed limits into the provider view's field for
// the provider's current settings (nil when there is nothing to show).
func RateLimitsView(l llm.RateLimits, maxConcurrency, rpm int) *ProviderRateLimits {
	adv, ok := llm.AdviseLimits(l, maxConcurrency, rpm)
	if !ok {
		return nil
	}
	opt := func(v int64) *int64 {
		if v <= 0 {
			return nil
		}
		return &v
	}
	return &ProviderRateLimits{
		Model: l.Model, RequestsPerMinute: l.Requests,
		InputTokensPerMinute: opt(l.InputTokens), OutputTokensPerMinute: opt(l.OutputTokens), TokensPerMinute: opt(l.Tokens),
		ObservedAt: l.ObservedAt.UTC().Format(time.RFC3339),
		Advice: ProviderLimitAdvice{MaxConcurrency: adv.MaxConcurrency, RPM: adv.RPM, Below: adv.Below,
			ConcurrencyBelow: adv.ConcurrencyBelow, RPMBelow: adv.RPMBelow, Measured: adv.Measured},
	}
}

func (s *Server) providerViews(ctx context.Context) ([]ProviderView, error) {
	refs, err := s.snapshotRefs(ctx)
	if err != nil {
		return nil, err
	}
	ps := s.set.Providers()
	out := make([]ProviderView, 0, len(ps))
	for _, p := range ps {
		out = append(out, s.providerView(ctx, p, refs))
	}
	return out, nil
}

func parseRoleParams(raw string) model.RoleParams {
	var p model.RoleParams
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p
}

// roleViews lists the roles in settings-page order (A5.3 角色分配; v2 G3.1
// glossary_janitor with its hint, appendix D1 "默认" tag).
func (s *Server) roleViews() []RoleView {
	rows := s.set.Roles()
	out := make([]RoleView, 0, len(model.Roles))
	for _, role := range model.Roles {
		r := rows[role]
		raw := parseRoleParams(r.Params)
		params := settings.ResolveParams(role, raw)
		v := RoleView{Role: role, Label: settings.RoleLabel(role), ProviderID: strOrNil(r.ProviderID), Model: strOrNil(r.Model),
			Default: s.set.RoleDefault(role), Hint: settings.RoleHint(role), Thinking: model.ThinkingLevel(raw)}
		if params.Temperature != nil {
			v.Temperature = *params.Temperature
		}
		v.Window = params.Window
		v.MaxTokens = raw.MaxTokens // v3.2.x: as set, nil = automatic
		if role == model.RoleTranslate {
			v.Batch = raw.Batch
		}
		if _, err := s.set.RoleTarget(role); err != nil {
			v.Error = err.Error()
			// P14.3: the coded form (settings.role_not_configured,
			// settings.role_provider_deleted) for the front end
			var ce *model.CodedError
			if errors.As(err, &ce) {
				info := ce.Info
				v.ErrorInfo = &info
			}
		} else {
			v.Configured = true
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) settingsView(ctx context.Context) (Settings, error) {
	provs, err := s.providerViews(ctx)
	if err != nil {
		return Settings{}, err
	}
	v := Settings{
		Providers: provs,
		Roles:     s.roleViews(),
		ASR:       s.set.ASR(),
		General:   s.set.General(),
		DefaultBaseURLs: map[string]string{
			model.ProviderOpenAI:    settings.DefaultBaseURL(model.ProviderOpenAI),
			model.ProviderAnthropic: settings.DefaultBaseURL(model.ProviderAnthropic),
		},
	}
	v.CredentialsErrorInfo = settings.CredentialsErrorInfo() // sec3 L2
	if s.sc != nil {
		v.GPUTotalMB = s.sc.GPUTotalMB(ctx) // perf-fix: no nvidia-smi run per request
	}
	v.Devices = s.deviceViews(ctx) // SPEC-v2 I6: the 转写 section's device list
	on, err := s.redecodeOn(ctx)
	if err != nil {
		return Settings{}, err
	}
	v.ASRRedecode = &on
	return v, nil
}

// redecodeOn reads the switch "修复退化的转写窗口" (model.KVASRRedecode;
// on unless switched off).
func (s *Server) redecodeOn(ctx context.Context) (bool, error) {
	v, ok, err := s.st.GetKV(ctx, model.KVASRRedecode)
	if err != nil {
		return true, err
	}
	return model.RedecodeEnabled(v, ok), nil
}

// asrSwitches are the fields of PUT /api/settings/asr that are not
// model.ASRSettings (kept in kv, not in settings.asr).
type asrSwitches struct {
	// Redecode is Settings.ASRRedecode; nil = unchanged.
	Redecode *bool `json:"redecode"`
}

func (s *Server) respondSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.settingsView(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// GET /api/settings.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	s.respondSettings(w, r)
}

// PUT /api/settings/roles/{role} RoleInput.
func (s *Server) putRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	var in RoleInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.set.SetRole(r.Context(), r.PathValue("role"), in.ProviderID, in.Model, in.Params); err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondSettings(w, r)
}

// PUT /api/settings/asr ASRSettings. The body is merged into the current
// settings before validation (lead ruling 15): a field the body leaves out
// keeps its value, so a client that does not know the 3b fields
// (device_mode, multi_device, battery_aware, mem_reserve_mb) cannot reset
// the booleans to false.
//
// "redecode" (Settings.ASRRedecode) is written to kv model.KVASRRedecode
// after the settings were stored: model.ASRRedecodeOff or ASRRedecodeOn
// (the switch turned on again; any value but off means on). It applies to
// the transcriptions that start after it (no lecture becomes stale).
func (s *Server) putASR(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.fail(w, r, apiErr(http.StatusBadRequest, "request.bad_json", nil))
		return
	}
	var sw asrSwitches
	if json.Unmarshal(body, &sw) != nil {
		s.fail(w, r, apiErr(http.StatusBadRequest, "request.bad_json", nil))
		return
	}
	// r4 D9: merged into the settings as they are under the settings'
	// write lock, so a concurrent PUT that changes another field keeps it.
	err = s.set.UpdateASR(r.Context(), func(in *model.ASRSettings) error {
		if json.Unmarshal(body, in) != nil {
			return apiErr(http.StatusBadRequest, "request.bad_json", nil)
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if sw.Redecode != nil {
		v := model.ASRRedecodeOn
		if !*sw.Redecode {
			v = model.ASRRedecodeOff
		}
		if err := s.st.SetKV(r.Context(), model.KVASRRedecode, v); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.kick() // device mode, multi-device, battery and memory rules apply at the next scan
	s.respondSettings(w, r)
}

// PUT /api/settings/general GeneralSettings. The body is merged into the
// current settings (as PUT /api/settings/asr, lead ruling 15; v3.2.x): a
// field the body leaves out keeps its value, so a page that does not know
// auto_chapters and llm_idle_timeout_s cannot reset them; null sets one
// back to its default (on, model.DefaultLLMIdleTimeoutS).
func (s *Server) putGeneral(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	// Read before the merge: the merge runs under the settings' write lock
	// (r4 D9), which a slow client must not hold.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.fail(w, r, apiErr(http.StatusBadRequest, "request.bad_json", nil))
		return
	}
	err = s.set.UpdateGeneral(r.Context(), func(in *model.GeneralSettings) error {
		// as decodeJSON: the first JSON value of the body
		if json.NewDecoder(bytes.NewReader(body)).Decode(in) != nil {
			return apiErr(http.StatusBadRequest, "request.bad_json", nil)
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.kick() // a raised daily budget releases waiting requests
	s.respondSettings(w, r)
}

// GET /api/providers.
func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	out, err := s.providerViews(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// providerInput converts the request. tariff (v2 P12): absent = unchanged,
// null clears it, an object replaces it (validated by settings).
func providerInput(in ProviderInput) settings.ProviderInput {
	out := settings.ProviderInput{
		Name: in.Name, Type: in.Type, BaseURL: in.BaseURL, APIKey: in.APIKey,
		MaxConcurrency: in.MaxConcurrency, RPM: in.RPM, BalanceAPI: in.BalanceAPI,
		PriceInput: in.PriceInput, PriceCacheHit: in.PriceCacheHit, PriceOutput: in.PriceOutput,
		Currency: in.Currency, ClearPrices: in.ClearPrices, OffpeakOnly: in.OffpeakOnly,
	}
	switch t := strings.TrimSpace(string(in.Tariff)); {
	case t == "":
	case t == "null":
		out.ClearTariff = true
	default:
		out.Tariff = &t
	}
	return out
}

func (s *Server) respondProvider(w http.ResponseWriter, r *http.Request, p model.Provider, status int) {
	refs, err := s.snapshotRefs(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, s.providerView(r.Context(), p, refs))
}

// POST /api/providers ProviderInput → 201 ProviderView.
func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	var in ProviderInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.set.CreateProvider(r.Context(), providerInput(in))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondProvider(w, r, p, http.StatusCreated)
}

// PATCH /api/providers/{id} ProviderInput (absent fields and an empty
// api_key are unchanged, A5.2).
func (s *Server) patchProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	var in ProviderInput
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.set.UpdateProvider(r.Context(), r.PathValue("id"), providerInput(in))
	if err != nil {
		s.fail(w, r, notFoundAs(err, "provider.not_found"))
		return
	}
	s.respondProvider(w, r, p, http.StatusOK)
}

// DELETE /api/providers/{id}: 409 while a role uses it; otherwise 200 with
// the number of old snapshots that can no longer "沿用原模型" (A5.3).
func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	info, err := s.set.DeleteProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, notFoundAs(err, "provider.not_found"))
		return
	}
	out := ProviderDeleteResult{SnapshotRefs: info.SnapshotRefs}
	if info.SnapshotRefs > 0 {
		// Transitional Chinese text (frozen string field); the front end
		// words it from snapshot_refs.
		out.Warning = "使用该提供商的旧讲座将无法沿用原模型重跑"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) providerOr404(w http.ResponseWriter, r *http.Request) (model.Provider, bool) {
	p, ok := s.set.Provider(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "provider.not_found", nil)
	}
	return p, ok
}

// POST /api/providers/{id}/test: one minimal request (A5.3 测试连接).
func (s *Server) testProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	p, ok := s.providerOr404(w, r)
	if !ok {
		return
	}
	modelName := s.set.TestModel(r.Context(), p.ID)
	res := s.testConn(r.Context(), p, modelName)
	if r.Context().Err() != nil {
		return
	}
	writeJSON(w, http.StatusOK, ProviderTest{OK: res.OK, LatencyMS: res.Latency.Milliseconds(), Error: res.Error, Model: modelName,
		ErrorInfo: res.ErrorInfo})
}

// POST /api/providers/{id}/models: probe the model list (A5.3 探测模型).
// The result is cached; a failed probe caches its error and keeps the
// models of the last successful probe (the role dropdowns keep working,
// and a model can always be typed by hand).
func (s *Server) probeProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	ctx := r.Context()
	p, ok := s.providerOr404(w, r)
	if !ok {
		return
	}
	models, err := s.probe(ctx, p)
	if ctx.Err() != nil {
		return
	}
	pm := model.ProbedModels{Models: models, ProbedAt: s.now().UTC().Format(time.RFC3339)}
	if err != nil {
		pm.Error = "探测模型失败：" + err.Error()
		// v2 P14.3: the probe's code (llm.probe.*), the text as the detail
		pm.ErrorInfo = &model.ErrorInfo{Code: llm.CodeProbeFailed, Detail: pm.Error}
		var ce *model.CodedError
		if errors.As(err, &ce) {
			pm.ErrorInfo.Code, pm.ErrorInfo.Params = ce.Info.Code, ce.Info.Params
		}
		pm.Models = []string{}
		if old, ok, perr := s.set.ProbedModels(ctx, p.ID); perr == nil && ok && old.Models != nil {
			pm.Models = old.Models
		}
	}
	if pm.Models == nil {
		pm.Models = []string{}
	}
	stored, err := s.set.SetProbedModelsFor(ctx, p, pm)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			err = notFoundAs(err, "provider.not_found")
		}
		s.fail(w, r, err)
		return
	}
	if !stored {
		// r4 D3: the provider moved to another type, address or key while
		// the probe ran; its result belongs to the old one and was dropped.
		// Answer with what is stored now (the page shows the refreshed
		// settings either way); the provider change already told the pages.
		cur, _, cerr := s.set.ProbedModels(ctx, p.ID)
		if cerr != nil {
			s.fail(w, r, cerr)
			return
		}
		if cur.Models == nil {
			cur.Models = []string{}
		}
		writeJSON(w, http.StatusOK, ProviderModels{Models: cur.Models, ProbedAt: cur.ProbedAt, Error: cur.Error, ErrorInfo: cur.ErrorInfo})
		return
	}
	s.hub.Changed("settings", "")
	writeJSON(w, http.StatusOK, ProviderModels{Models: pm.Models, ProbedAt: pm.ProbedAt, Error: pm.Error, ErrorInfo: pm.ErrorInfo})
}

// ---- v2 P12.2 holidays, P14.6/P14.7 UI language ----

func (s *Server) holidaysView(ctx context.Context) (HolidaysView, error) {
	h, err := s.set.Holidays(ctx)
	if err != nil {
		return HolidaysView{}, err
	}
	years := h.Years
	if years == nil {
		years = map[string][]string{}
	}
	return HolidaysView{Years: years, Edited: h.Edited, Source: h.Source, SourceInfo: holidaysSourceInfo(h.Source)}, nil
}

// holidaysSourceInfo is the coded note on where the built-in holiday dates
// come from (P14.3: holidays.source_cn with the years of the built-in file;
// the file's own Chinese note as detail).
func holidaysSourceInfo(source string) *model.ErrorInfo {
	years := make([]string, 0)
	for y := range settings.HolidayYears(tariff.HolidaysCN()) {
		years = append(years, y)
	}
	sort.Strings(years)
	return &model.ErrorInfo{Code: "holidays.source_cn", Params: map[string]any{"years": strings.Join(years, ", ")}, Detail: source}
}

func (s *Server) respondHolidays(w http.ResponseWriter, r *http.Request) {
	v, err := s.holidaysView(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// GET /api/settings/holidays → HolidaysView: the Chinese public holidays in
// effect (the built-in file, or the user's data replacing it).
func (s *Server) getHolidays(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	s.respondHolidays(w, r)
}

// PUT /api/settings/holidays {years}: the user's data replaces the built-in
// file; years null or {} restores the built-in file. Invalid data → 400
// tariff.invalid_holiday_data with the offending year/date.
func (s *Server) putHolidays(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	var in struct {
		Years map[string][]string `json:"years"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.set.SetHolidays(r.Context(), in.Years); err != nil {
		s.fail(w, r, err)
		return
	}
	s.kick() // off-peak waits are re-checked
	s.respondHolidays(w, r)
}

func (s *Server) respondUI(w http.ResponseWriter, r *http.Request) {
	locale, zh, err := s.set.UISettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, UISettings{Locale: locale, ZhDisplay: zh})
}

// GET /api/settings/ui → UISettings (kv ui.locale, ui.zh_display; "" = not
// chosen yet).
func (s *Server) getUI(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	s.respondUI(w, r)
}

// PUT /api/settings/ui UISettings: both values are written (validated by
// settings: 400 with its code); other open pages follow via SSE
// changed {kind: "settings"}.
func (s *Server) putUI(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	var in UISettings
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.set.SetUISettings(r.Context(), in.Locale, in.ZhDisplay); err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("settings", "")
	s.respondUI(w, r)
}
