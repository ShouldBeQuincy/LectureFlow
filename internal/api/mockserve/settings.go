// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Settings page endpoints of the mock (SPEC-v1.1 A5.3): providers, role
// assignments, ASR and general settings, "测试连接" and "探测模型". The
// state lives in store.settings (so the A4.3 model-choice diffs of the
// lecture endpoints see every change) and follows the validation rules and
// Chinese messages of internal/settings and internal/api/handlers_settings.go.
//
// The mock never keeps an API key: a key sent with POST/PATCH only updates
// key_hint, so no response can contain it (A5.2).
//
// Scenario "unset" (?mock=unset) serves a separate settings world in which
// 大纲整理 has no provider and 翻译 points at a deleted provider (the
// settings page's error states; v2: no janitor, chapters or organizer);
// everything else uses the default data. Scenario "nolimits" is the
// default data without the observed account limits (T-PERF-03; the
// default data's Anthropic provider has them).
//
// v2 P12 (web-course-p2): providers carry a tariff (a new DeepSeek provider
// gets tariff.DeepSeekPreset, like the real settings), offpeak_only and the
// current tariff_state (course_p2.go).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/tariff"
)

// settingsLatency is how long "测试连接" and "探测模型" take in the mock
// (long enough to see the busy state; tests set it to 0).
var settingsLatency = 700 * time.Millisecond

const mockProviderNotFound = "找不到这个提供商（可能已被删除），请刷新" // internal/api providerNotFound

func (m *mock) registerSettings(mux *http.ServeMux) {
	a := m.api
	mux.HandleFunc("GET /api/settings", a(m.getSettings))
	mux.HandleFunc("PUT /api/settings/roles/{role}", a(m.putRole))
	mux.HandleFunc("PUT /api/settings/asr", a(m.putASR))
	mux.HandleFunc("PUT /api/settings/general", a(m.putGeneral))
	mux.HandleFunc("GET /api/providers", a(m.listProviders))
	mux.HandleFunc("POST /api/providers", a(m.createProvider))
	mux.HandleFunc("PATCH /api/providers/{id}", a(m.patchProvider))
	mux.HandleFunc("DELETE /api/providers/{id}", a(m.deleteProvider))
	mux.HandleFunc("POST /api/providers/{id}/test", a(m.testProvider))
	mux.HandleFunc("POST /api/providers/{id}/models", a(m.probeProvider))
}

// ---------- scenario "unset" ----------

type unsetWorld struct {
	once sync.Once
	s    *store
	err  error
}

var unsetWorlds sync.Map // *mock → *unsetWorld

var oldModelWorlds sync.Map // *mock → *unsetWorld (scenario "oldmodel")

var noLimitsWorlds sync.Map // *mock → *unsetWorld (scenario "nolimits")

// settingsStore is the store whose settings a request reads and writes.
func (m *mock) settingsStore(r *http.Request, s *store) (*store, error) {
	switch scenario(r) {
	case "unset":
	case "oldmodel":
		return m.oldModelStore()
	case "nolimits":
		return m.noLimitsStore()
	default:
		return s, nil
	}
	v, _ := unsetWorlds.LoadOrStore(m, &unsetWorld{})
	u := v.(*unsetWorld)
	u.once.Do(func() {
		if u.s, u.err = loadStore(m.dataDir, time.Now(), false); u.err == nil {
			for i := range u.s.settings.Roles {
				rv := &u.s.settings.Roles[i]
				switch rv.Role {
				case model.RoleSyllabusFormat:
					rv.ProviderID, rv.Model = nil, nil
				case model.RoleTranslate:
					gone, mdl := "01JPRV00000000000000000DEL", "gpt-4.1"
					rv.ProviderID, rv.Model = &gone, &mdl
				case model.RoleGlossaryJanitor: // v2 G2: no janitor, the common-word rule
					rv.ProviderID, rv.Model, rv.Default = nil, nil, false
				case model.RoleChapters, model.RoleOrganizer: // v2 P7, P13: optional features off
					rv.ProviderID, rv.Model, rv.Default = nil, nil, false
				}
			}
		}
	})
	return u.s, u.err
}

// oldModelStore is the settings world of scenario "oldmodel": DeepSeek's
// probed list is the current one (NOTES "DeepSeek 官方文档核对": the old
// name deepseek-chat is past its retirement date), the roles keep the
// default data's deepseek-chat.
func (m *mock) oldModelStore() (*store, error) {
	v, _ := oldModelWorlds.LoadOrStore(m, &unsetWorld{})
	u := v.(*unsetWorld)
	u.once.Do(func() {
		if u.s, u.err = loadStore(m.dataDir, time.Now(), false); u.err == nil {
			for i := range u.s.settings.Providers {
				if p := &u.s.settings.Providers[i]; p.ID == "deepseek" {
					p.Models = []string{"deepseek-flash", "deepseek-v4-pro"} // what the DeepSeek API lists now
				}
			}
		}
	})
	return u.s, u.err
}

// noLimitsStore is the settings world of scenario "nolimits" (T-PERF-03):
// the default data before any answer reported the account's limits (no
// provider has rate_limits; the settings page shows no suggestion).
func (m *mock) noLimitsStore() (*store, error) {
	v, _ := noLimitsWorlds.LoadOrStore(m, &unsetWorld{})
	u := v.(*unsetWorld)
	u.once.Do(func() {
		if u.s, u.err = loadStore(m.dataDir, time.Now(), false); u.err == nil {
			for i := range u.s.settings.Providers {
				u.s.settings.Providers[i].RateLimits = nil
			}
		}
	})
	return u.s, u.err
}

// withSettings runs fn with the request's settings store locked.
func (m *mock) withSettings(w http.ResponseWriter, r *http.Request, s *store, fn func(st *store) (int, any, error)) {
	st, err := m.settingsStore(r, s)
	if err != nil {
		writeErr(w, err)
		return
	}
	st.mu.Lock()
	status, v, err := fn(st)
	st.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, status, v)
}

// ---------- views ----------

// settingsView is GET /api/settings with the derived fields recomputed
// (used_by_roles, snapshot_refs, role configured/error), like the real
// handler. Must be called with st.mu held; slices are copied, and mutations
// always assign new inner slices and pointers, so the result can be encoded
// after the lock is released.
func (st *store) settingsView() api.Settings {
	v := st.settings
	v.Providers = st.providerViews()
	v.Roles = make([]api.RoleView, 0, len(st.settings.Roles))
	for _, rv := range st.settings.Roles {
		rv.Label = settings.RoleLabel(rv.Role)
		rv.Hint = settings.RoleHint(rv.Role) // v2 G3.1
		if rv.Thinking == "" {
			rv.Thinking = model.ThinkingOff
		}
		rv.Configured, rv.Error, rv.ErrorInfo = true, "", nil
		// the real texts and codes of settings.RoleTarget (P14.3: error_info)
		roleErr := func(code string) *model.ErrorInfo {
			return &model.ErrorInfo{Code: code, Params: map[string]any{"role": rv.Role}}
		}
		switch {
		case rv.ProviderID == nil || *rv.ProviderID == "" || rv.Model == nil || strings.TrimSpace(*rv.Model) == "":
			rv.Configured, rv.Error = false, fmt.Sprintf("请先在设置中为「%s」选择提供商和模型", rv.Label)
			rv.ErrorInfo = roleErr(settings.CodeRoleNotConfigured)
		default:
			if _, ok := st.providerByID(*rv.ProviderID); !ok {
				rv.Configured, rv.Error = false, fmt.Sprintf("「%s」使用的提供商已删除，请先在设置中为它重新选择提供商和模型", rv.Label)
				rv.ErrorInfo = roleErr(settings.CodeRoleProviderDeleted)
			}
		}
		v.Roles = append(v.Roles, rv)
	}
	v.DefaultBaseURLs = map[string]string{}
	for k, u := range st.settings.DefaultBaseURLs {
		v.DefaultBaseURLs[k] = u
	}
	if st.dev != nil { // SPEC-v2 I6: = GET /api/asr/devices (devices.go)
		v.Devices = st.deviceViews(time.Now())
	}
	on := !st.redecodeOff // the real server always sends the switch
	v.ASRRedecode = &on
	return v
}

func (st *store) providerViews() []api.ProviderView {
	out := make([]api.ProviderView, 0, len(st.settings.Providers))
	for _, p := range st.settings.Providers {
		out = append(out, st.providerView(p))
	}
	return out
}

func (st *store) providerView(p api.ProviderView) api.ProviderView {
	p.UsedByRoles = []string{}
	for _, role := range model.Roles {
		for _, rv := range st.settings.Roles {
			if rv.Role == role && rv.ProviderID != nil && *rv.ProviderID == p.ID {
				p.UsedByRoles = append(p.UsedByRoles, role)
			}
		}
	}
	p.SnapshotRefs = 0
	for _, l := range st.lectures {
		for _, sv := range l.Stages {
			switch sv.Stage {
			case model.StageSummary, model.StageRepair, model.StageTranslate:
				if sv.Model != nil && sv.Model.ProviderID == p.ID {
					p.SnapshotRefs++
				}
			}
		}
	}
	if p.Models == nil {
		p.Models = []string{}
	}
	// the real rule (DeepSeek and Anthropic; the other OpenAI-compatible
	// provider, OpenRouter, only "off")
	p.SupportsThinking = settings.SupportsThinking(model.Provider{ID: p.ID, Name: p.Name, Type: p.Type, BaseURL: p.BaseURL, BalanceAPI: p.BalanceAPI})
	p.DefaultBatchClaude = settings.ClaudeBatch(model.Provider{Type: p.Type, BaseURL: p.BaseURL, BalanceAPI: p.BalanceAPI})
	p.TariffState = nil
	if ts := st.tariffStateOf(p, time.Now()); ts != nil {
		v := api.TariffState(*ts)
		p.TariffState = &v
	}
	p.RateLimits = mockRateLimits(p)
	return p
}

// mockRateLimits is the provider's observed limits (T-PERF-03) with the
// suggestion for its current settings, as the real server computes it
// (llm.AdviseLimits, with mockMeasured standing in for measured answers);
// none for a provider that is not Anthropic at api.anthropic.com.
func mockRateLimits(p api.ProviderView) *api.ProviderRateLimits {
	rl := p.RateLimits
	if rl == nil || p.Type != model.ProviderAnthropic || !llm.OfficialAnthropicHost(p.BaseURL) {
		return nil
	}
	val := func(v *int64) int64 {
		if v == nil {
			return 0
		}
		return *v
	}
	at, _ := time.Parse(time.RFC3339, rl.ObservedAt)
	return api.RateLimitsView(mockMeasured(llm.RateLimits{Model: rl.Model, Requests: rl.RequestsPerMinute, InputTokens: val(rl.InputTokensPerMinute),
		OutputTokens: val(rl.OutputTokensPerMinute), Tokens: val(rl.TokensPerMinute), ObservedAt: at}), p.MaxConcurrency, p.RPM)
}

// mockMeasured adds the measured answers the mock pretends to have seen:
// one-sentence translations as in the performance audit's T5 run (about
// 2.3 s, 2000 uncached input and 300 output tokens each).
func mockMeasured(l llm.RateLimits) llm.RateLimits {
	l.Samples, l.MeanSeconds, l.MeanUncachedInput, l.MeanOutput = 20, 2.3, 2000, 300
	return l
}

func (st *store) providerIndex(id string) int {
	for i, p := range st.settings.Providers {
		if p.ID == id {
			return i
		}
	}
	return -1
}

func (st *store) settingsChanged() {
	st.notify(api.ChangedEvent{Kind: "settings"})
}

// ---------- settings ----------

func (m *mock) getSettings(w http.ResponseWriter, r *http.Request, s *store) {
	if scenario(r) == "error" { // the settings page's card error state
		writeError(w, http.StatusInternalServerError, "服务器内部错误：mock 模拟的失败（?mock=error）")
		return
	}
	m.withSettings(w, r, s, func(st *store) (int, any, error) { return http.StatusOK, st.settingsView(), nil })
}

// putRole mirrors settings.SetRole.
func (m *mock) putRole(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.RoleInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	role := r.PathValue("role")
	m.withSettings(w, r, s, func(st *store) (int, any, error) {
		idx := -1
		for i, rv := range st.settings.Roles {
			if rv.Role == role {
				idx = i
			}
		}
		valid := false
		for _, rr := range model.Roles {
			valid = valid || rr == role
		}
		if !valid {
			return 0, nil, errBad(fmt.Sprintf("未知的角色 %q", role))
		}
		label := settings.RoleLabel(role)
		if _, ok := st.providerByID(in.ProviderID); !ok || in.ProviderID == "" {
			return 0, nil, errBad(fmt.Sprintf("请为「%s」选择一个已存在的提供商", label))
		}
		name := strings.TrimSpace(in.Model)
		if name == "" {
			return 0, nil, errBad(fmt.Sprintf("请为「%s」选择或填写模型名", label))
		}
		params := in.Params
		if t := params.Temperature; t != nil && !(*t >= 0 && *t <= 2) {
			return 0, nil, errBad("温度必须在 0 到 2 之间")
		}
		if role != model.RoleTranslate {
			params.Window = nil
		} else if wn := params.Window; wn != nil && (*wn < 0 || *wn > 10) {
			return 0, nil, errBad("翻译的上下文窗口必须在 0 到 10 之间")
		}
		// thinking (settings.SetRole): one of the four levels, and not "off"
		// only on a provider that supports it
		thinking := model.ThinkingOf(params)
		if !model.ValidThinking(thinking) {
			return 0, nil, codedErr(http.StatusBadRequest, settings.CodeThinkingInvalid, map[string]any{"value": thinking},
				fmt.Sprintf("深度思考的强度「%s」无效，只能是 off、low、high 或 max", thinking))
		}
		if p, _ := st.providerByID(in.ProviderID); thinking != model.ThinkingOff && !st.providerView(p).SupportsThinking {
			return 0, nil, codedErr(http.StatusBadRequest, settings.CodeThinkingUnsupported, map[string]any{"provider": p.Name},
				fmt.Sprintf("提供商「%s」不支持深度思考，请选择“关闭”", p.Name))
		}
		// v3.2.x (settings.checkParams): max_tokens 1..1e6, batch 1..20 and
		// translate only; both shown as set, absent while unset
		if role != model.RoleTranslate {
			params.Batch = nil
		} else if b := params.Batch; b != nil && (*b < 1 || *b > settings.MaxBatch) {
			return 0, nil, codedErr(http.StatusBadRequest, settings.CodeBatchInvalid, map[string]any{"min": 1, "max": settings.MaxBatch},
				fmt.Sprintf("每批翻译的句数必须在 1 到 %d 之间", settings.MaxBatch))
		}
		if mt := params.MaxTokens; mt != nil && (*mt < 1 || *mt > settings.MaxTokensLimit) {
			return 0, nil, codedErr(http.StatusBadRequest, settings.CodeMaxTokensInvalid, map[string]any{"min": 1, "max": settings.MaxTokensLimit},
				fmt.Sprintf("输出上限（max_tokens）必须是 1 到 %d 之间的整数", settings.MaxTokensLimit))
		}
		res := settings.ResolveParams(role, params)
		pid := in.ProviderID
		rv := api.RoleView{Role: role, Label: label, ProviderID: &pid, Model: &name, Temperature: *res.Temperature, Window: res.Window, Thinking: thinking,
			MaxTokens: res.MaxTokens, Batch: res.Batch}
		if idx < 0 {
			st.settings.Roles = append(st.settings.Roles, rv)
		} else {
			st.settings.Roles[idx] = rv
		}
		st.settingsChanged()
		return http.StatusOK, st.settingsView(), nil
	})
}

// putASR mirrors settings.SetASR. The body is decoded over the current
// settings (lead ruling 15): fields an older client leaves out keep their
// value instead of becoming false or 0.
func (m *mock) putASR(w http.ResponseWriter, r *http.Request, s *store) {
	st, err := m.settingsStore(r, s)
	if err != nil {
		writeErr(w, err)
		return
	}
	st.mu.Lock()
	a := st.settings.ASR
	st.mu.Unlock()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, errBad("无法读取请求体"))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err := decodeBody(r, &a); err != nil {
		writeErr(w, err)
		return
	}
	// "redecode" (api.Settings.ASRRedecode), like the real handler: kept
	// apart from settings.asr; absent = unchanged
	var sw struct {
		Redecode *bool `json:"redecode"`
	}
	_ = json.Unmarshal(body, &sw)
	a.Model = strings.TrimSpace(a.Model)
	a.Language = strings.TrimSpace(a.Language)
	if a.DeviceMode = strings.TrimSpace(a.DeviceMode); a.DeviceMode == "" {
		a.DeviceMode = model.ASRDeviceModeAuto
	}
	var msg string
	switch {
	case a.Model == "":
		msg = "请填写转写模型（例如 large-v3）"
	case a.ComputeType != "auto" && a.ComputeType != "float16" && a.ComputeType != "int8_float16":
		msg = "compute_type 只能是 auto、float16 或 int8_float16"
	case a.Device != "cuda" && a.Device != "cpu":
		msg = "设备只能是 cuda 或 cpu"
	case a.Language == "":
		msg = "请填写语言（例如 en）"
	case a.MaxParallel < 1 || a.MaxParallel > 8:
		msg = "最大并行数必须在 1 到 8 之间"
	case a.VRAMReserveMB < 0 || a.VRAMReserveMB > 65536:
		msg = "保留显存必须在 0 到 65536 MB 之间"
	case a.VRAMPerJobMB < 0 || a.VRAMPerJobMB > 65536:
		msg = "每个转写任务的预估显存必须在 0 到 65536 MB 之间"
	}
	if msg != "" {
		writeErr(w, errBad(msg))
		return
	}
	if a.MemReserveMB < 0 || a.MemReserveMB > settings.MaxMemReserveMB {
		writeErr(w, codedErr(http.StatusBadRequest, settings.CodeMemReserveInvalid, map[string]any{"min": 0, "max": settings.MaxMemReserveMB},
			fmt.Sprintf("保留内存必须在 0 到 %d MB 之间", settings.MaxMemReserveMB)))
		return
	}
	m.withSettings(w, r, s, func(st *store) (int, any, error) {
		if st.dev != nil {
			if err := st.checkDeviceMode(a); err != nil {
				return 0, nil, err
			}
		}
		a = model.NormalizeASRDeviceMode(a) // power management: auto | manual + manual_devices
		if a.VRAMPerJobMB == 0 {
			a.VRAMPerJobMB = st.settings.ASR.VRAMPerJobMB
		}
		st.settings.ASR = a
		if sw.Redecode != nil {
			st.redecodeOff = !*sw.Redecode
		}
		st.settingsChanged()
		return http.StatusOK, st.settingsView(), nil
	})
}

// putGeneral mirrors settings.SetGeneral and the real handler's merge
// (v3.2.x): a field the body leaves out keeps its value, null resets
// auto_chapters / llm_idle_timeout_s to their defaults (absent).
func (m *mock) putGeneral(w http.ResponseWriter, r *http.Request, s *store) {
	var patch map[string]json.RawMessage
	if err := decodeBody(r, &patch); err != nil {
		writeErr(w, err)
		return
	}
	m.withSettings(w, r, s, func(st *store) (int, any, error) {
		g := st.settings.General
		g.AutoChapters, g.LLMIdleTimeoutS = clonePtr(g.AutoChapters), clonePtr(g.LLMIdleTimeoutS)
		b, _ := json.Marshal(patch)
		if err := json.Unmarshal(b, &g); err != nil {
			return 0, nil, errBad("请求体不是有效的 JSON：" + err.Error())
		}
		// r4 T5: the checks and codes of settings.SetGeneral
		if !(g.DailyBudget >= 0) || g.DailyBudget > 1e12 {
			return 0, nil, codedErr(http.StatusBadRequest, settings.CodeBudgetInvalid, nil, "每日预算必须是不小于 0 的数（0 表示不限制）")
		}
		if v := g.LLMIdleTimeoutS; v != nil && *v != 0 && (*v < settings.MinLLMIdleTimeoutS || *v > settings.MaxLLMIdleTimeoutS) {
			return 0, nil, codedErr(http.StatusBadRequest, settings.CodeIdleTimeoutInvalid,
				map[string]any{"min": settings.MinLLMIdleTimeoutS, "max": settings.MaxLLMIdleTimeoutS},
				fmt.Sprintf("模型请求的空闲超时必须是 0（关闭）或 %d 到 %d 之间的整数秒", settings.MinLLMIdleTimeoutS, settings.MaxLLMIdleTimeoutS))
		}
		st.settings.General = g
		st.settingsChanged()
		return http.StatusOK, st.settingsView(), nil
	})
}

// clonePtr copies the value behind p (nil stays nil).
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// ---------- providers ----------

func (m *mock) listProviders(w http.ResponseWriter, r *http.Request, s *store) {
	m.withSettings(w, r, s, func(st *store) (int, any, error) { return http.StatusOK, st.providerViews(), nil })
}

// createProvider mirrors settings.CreateProvider (201 like the real handler).
func (m *mock) createProvider(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.ProviderInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Name == nil || in.Type == nil || in.BaseURL == nil {
		writeErr(w, errBad("请填写提供商的名称、类型和 base_url"))
		return
	}
	m.withSettings(w, r, s, func(st *store) (int, any, error) {
		p := api.ProviderView{
			ID:             st.newID("01JPRV"),
			MaxConcurrency: settings.DefaultMaxConcurrency,
			RPM:            settings.DefaultRPM,
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
			UsedByRoles:    []string{},
			Models:         []string{},
		}
		if in.BalanceAPI == nil {
			// Prefill like the settings page does: DeepSeek has a balance API.
			b := model.BalanceNone
			if isDeepSeekURL(*in.BaseURL) {
				b = model.BalanceDeepSeek
			}
			in.BalanceAPI = &b
		}
		if err := applyProviderInput(&p, in); err != nil {
			return 0, nil, err
		}
		if len(in.Tariff) == 0 && (isDeepSeekHost(p.BaseURL) || p.BalanceAPI == model.BalanceDeepSeek) {
			// v2 P12.2 / D3: a new DeepSeek provider gets the preset
			b, _ := json.Marshal(tariff.DeepSeekPreset())
			p.Tariff = b
		}
		if err := st.checkProviderName(p); err != nil {
			return 0, nil, err
		}
		st.settings.Providers = append(st.settings.Providers, p)
		st.settingsChanged()
		return http.StatusCreated, st.providerView(p), nil
	})
}

// patchProvider mirrors settings.UpdateProvider (absent fields and an empty
// api_key are unchanged).
func (m *mock) patchProvider(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.ProviderInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	m.withSettings(w, r, s, func(st *store) (int, any, error) {
		i := st.providerIndex(r.PathValue("id"))
		if i < 0 {
			return 0, nil, &httpError{status: http.StatusNotFound, msg: mockProviderNotFound}
		}
		p := st.settings.Providers[i]
		if err := applyProviderInput(&p, in); err != nil {
			return 0, nil, err
		}
		if err := st.checkProviderName(p); err != nil {
			return 0, nil, err
		}
		st.settings.Providers[i] = p
		st.settingsChanged()
		return http.StatusOK, st.providerView(p), nil
	})
}

// deleteProvider mirrors settings.DeleteProvider: 409 while a role uses it,
// otherwise 200 with the old snapshot references (A5.3).
func (m *mock) deleteProvider(w http.ResponseWriter, r *http.Request, s *store) {
	m.withSettings(w, r, s, func(st *store) (int, any, error) {
		i := st.providerIndex(r.PathValue("id"))
		if i < 0 {
			return 0, nil, &httpError{status: http.StatusNotFound, msg: mockProviderNotFound}
		}
		v := st.providerView(st.settings.Providers[i])
		if len(v.UsedByRoles) > 0 {
			labels := make([]string, len(v.UsedByRoles))
			for k, role := range v.UsedByRoles {
				labels[k] = "「" + settings.RoleLabel(role) + "」"
			}
			return 0, nil, errConflict(fmt.Sprintf("提供商正被%s使用，请先在角色分配中改用其他提供商", strings.Join(labels, "、")))
		}
		st.settings.Providers = append(append([]api.ProviderView{}, st.settings.Providers[:i]...), st.settings.Providers[i+1:]...)
		out := api.ProviderDeleteResult{SnapshotRefs: v.SnapshotRefs}
		if v.SnapshotRefs > 0 {
			out.Warning = "使用该提供商的旧讲座将无法沿用原模型重跑"
		}
		st.settingsChanged()
		return http.StatusOK, out, nil
	})
}

// testProvider imitates llm.TestConnection: no key → the same message
// without a request; a name or base_url containing "fail" → the server's
// 401 text; otherwise a latency derived from the id.
func (m *mock) testProvider(w http.ResponseWriter, r *http.Request, s *store) {
	st, err := m.settingsStore(r, s)
	if err != nil {
		writeErr(w, err)
		return
	}
	st.mu.Lock()
	i := st.providerIndex(r.PathValue("id"))
	var p api.ProviderView
	var modelName string
	if i >= 0 {
		p = st.settings.Providers[i]
		modelName = st.testModel(p)
	}
	st.mu.Unlock()
	if i < 0 {
		writeError(w, http.StatusNotFound, mockProviderNotFound)
		return
	}
	if p.KeyHint == "" && !settings.IsLocal(p.BaseURL) {
		writeJSON(w, http.StatusOK, api.ProviderTest{Error: fmt.Sprintf("提供商「%s」没有设置 API key", p.Name), Model: modelName})
		return
	}
	if !sleepCtx(r, settingsLatency) {
		return
	}
	latency := int64(180 + hashOf(p.ID)%520)
	switch {
	case simulatedFailure(p.Name) || simulatedFailure(p.BaseURL):
		writeJSON(w, http.StatusOK, api.ProviderTest{LatencyMS: latency, Model: modelName,
			Error: `HTTP 401：{"error":{"message":"Authentication Fails, Your api key: ` + p.KeyHint + ` is invalid","type":"authentication_error","param":null,"code":"invalid_request_error"}}`})
	case modelName == "":
		writeJSON(w, http.StatusOK, api.ProviderTest{Error: "没有可用于测试的模型名：请先为某个角色选择模型，或手动填写模型名"})
	default:
		writeJSON(w, http.StatusOK, api.ProviderTest{OK: true, LatencyMS: latency, Model: modelName})
	}
}

// probeProvider imitates POST /api/providers/{id}/models: the result is
// cached on the provider; a failed probe keeps the last models.
func (m *mock) probeProvider(w http.ResponseWriter, r *http.Request, s *store) {
	st, err := m.settingsStore(r, s)
	if err != nil {
		writeErr(w, err)
		return
	}
	st.mu.Lock()
	i := st.providerIndex(r.PathValue("id"))
	st.mu.Unlock()
	if i < 0 {
		writeError(w, http.StatusNotFound, mockProviderNotFound)
		return
	}
	if !sleepCtx(r, settingsLatency) {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if i = st.providerIndex(r.PathValue("id")); i < 0 {
		writeError(w, http.StatusNotFound, mockProviderNotFound)
		return
	}
	p := st.settings.Providers[i]
	now := time.Now().UTC().Format(time.RFC3339)
	out := api.ProviderModels{ProbedAt: now}
	switch {
	case p.KeyHint == "" && !settings.IsLocal(p.BaseURL):
		out.Error = `探测模型失败：HTTP 401：{"error":{"message":"Missing API key in the Authorization header","type":"authentication_error"}}`
		out.ErrorInfo = &model.ErrorInfo{Code: "llm.probe.http", Params: map[string]any{"status": 401}, Detail: out.Error}
	case simulatedFailure(p.Name) || simulatedFailure(p.BaseURL):
		out.Error = "探测模型失败：HTTP 404：404 page not found"
		out.ErrorInfo = &model.ErrorInfo{Code: "llm.probe.http", Params: map[string]any{"status": 404}, Detail: out.Error}
	default:
		out.Models = mockModels(p)
	}
	if out.Error != "" {
		out.Models = append([]string{}, p.Models...) // keep the last successful probe
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	p.Models, p.ModelsProbedAt, p.ModelsError, p.ModelsErrorInfo = out.Models, &now, out.Error, out.ErrorInfo
	st.settings.Providers[i] = p
	st.settingsChanged()
	writeJSON(w, http.StatusOK, out)
}

// testModel is settings.TestModel: the model of the first role using the
// provider, else the first probed model.
func (st *store) testModel(p api.ProviderView) string {
	for _, role := range model.Roles {
		for _, rv := range st.settings.Roles {
			if rv.Role == role && rv.ProviderID != nil && *rv.ProviderID == p.ID && rv.Model != nil && strings.TrimSpace(*rv.Model) != "" {
				return strings.TrimSpace(*rv.Model)
			}
		}
	}
	if len(p.Models) > 0 {
		return p.Models[0]
	}
	return ""
}

func mockModels(p api.ProviderView) []string {
	u, _ := url.Parse(p.BaseURL)
	host := ""
	if u != nil {
		host = strings.ToLower(u.Hostname())
	}
	switch {
	case p.Type == model.ProviderAnthropic:
		return []string{"claude-haiku-4-5", "claude-opus-4-5", "claude-opus-5-5", "claude-sonnet-4-5"}
	case host == "api.deepseek.com":
		return []string{"deepseek-flash", "deepseek-v4-pro"} // SPEC-v2 P12.1 / appendix D3
	case strings.HasSuffix(host, "openai.com"):
		return []string{"gpt-4.1", "gpt-4.1-mini", "gpt-4o", "gpt-4o-mini", "o3", "o4-mini"}
	case settings.IsLocal(p.BaseURL):
		return []string{"llama3.1:8b", "qwen2.5:14b"}
	}
	return []string{"deepseek/deepseek-chat", "google/gemini-2.5-flash", "meta-llama/llama-3.3-70b-instruct", "qwen/qwen-2.5-72b-instruct"}
}

// ---------- validation (internal/settings applyInput / validateProvider) ----------

func applyProviderInput(p *api.ProviderView, in api.ProviderInput) error {
	if in.Name != nil {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if in.Type != nil {
		p.Type = strings.TrimSpace(*in.Type)
	}
	oldBase := p.BaseURL
	if in.BaseURL != nil {
		p.BaseURL = strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
	}
	newKey := false
	if in.APIKey != nil {
		if k := strings.TrimSpace(*in.APIKey); k != "" {
			p.KeyHint = model.KeyHint(k) // the key itself is not kept
			newKey = true
			p.RateLimits = nil // another key may be another account (llm.RateLimits)
		}
	}
	if oldBase != "" && !newKey && !settings.SameOrigin(oldBase, p.BaseURL) {
		p.KeyHint = "" // settings.UpdateProvider: the key stays with its host (sec2-app L2)
	}
	if in.MaxConcurrency != nil {
		p.MaxConcurrency = *in.MaxConcurrency
	}
	if in.RPM != nil {
		p.RPM = *in.RPM
	}
	if in.BalanceAPI != nil {
		p.BalanceAPI = strings.TrimSpace(*in.BalanceAPI)
	}
	if in.ClearPrices {
		p.PriceInput, p.PriceCacheHit, p.PriceOutput = nil, nil, nil
	}
	for _, f := range []struct {
		in  *float64
		dst **float64
	}{{in.PriceInput, &p.PriceInput}, {in.PriceCacheHit, &p.PriceCacheHit}, {in.PriceOutput, &p.PriceOutput}} {
		if f.in != nil {
			v := *f.in
			*f.dst = &v
		}
	}
	if in.Currency != nil {
		if c := strings.TrimSpace(*in.Currency); c != "" {
			p.Currency = &c
		} else {
			p.Currency = nil
		}
	}
	if p.BalanceAPI == "" {
		p.BalanceAPI = model.BalanceNone
	}
	// v2 P12: absent = unchanged, null = none, an object is validated like
	// settings does (tariff.Parse; the error code of P14.3)
	if len(in.Tariff) > 0 {
		raw := strings.TrimSpace(string(in.Tariff))
		if raw == "null" {
			p.Tariff = nil
		} else {
			if _, err := tariff.Parse(raw); err != nil {
				info := model.ErrorInfo{Code: tariff.CodeInvalidJSON, Detail: err.Error()}
				if te, ok := err.(*tariff.Error); ok {
					info.Code, info.Params, info.Detail = te.Code, te.Params, te.Msg
				}
				return &httpError{status: http.StatusBadRequest, msg: info.Code, body: api.ErrorBody{Error: info}}
			}
			p.Tariff = json.RawMessage(raw)
		}
	}
	if in.OffpeakOnly != nil {
		p.OffpeakOnly = *in.OffpeakOnly
	}
	// like settings.applyTariff: the switch exists only with peak windows
	if tf, err := tariff.Parse(string(p.Tariff)); err != nil || tf == nil || len(tf.PeakWindows) == 0 {
		if in.OffpeakOnly != nil && *in.OffpeakOnly {
			return &httpError{status: http.StatusBadRequest, msg: "settings.offpeak_needs_peak", body: api.ErrorBody{Error: model.ErrorInfo{Code: "settings.offpeak_needs_peak"}}}
		}
		p.OffpeakOnly = false
	}
	return validateProviderView(*p)
}

func validateProviderView(p api.ProviderView) error {
	if p.Name == "" {
		return errBad("请填写提供商名称")
	}
	if len([]rune(p.Name)) > 64 {
		return errBad("提供商名称不能超过 64 个字符")
	}
	if p.Type != model.ProviderOpenAI && p.Type != model.ProviderAnthropic {
		return errBad("提供商类型只能是 OpenAI 兼容（openai）或 Anthropic（anthropic）")
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errBad("base_url 必须是 http:// 或 https:// 开头的完整地址")
	}
	if p.MaxConcurrency < 1 || p.MaxConcurrency > settings.MaxConcurrencyLimit {
		return errBad(fmt.Sprintf("并发上限必须在 1 到 %d 之间", settings.MaxConcurrencyLimit))
	}
	if p.RPM < 0 || p.RPM > settings.MaxRPM {
		return errBad(fmt.Sprintf("每分钟请求数必须在 0 到 %d 之间（0 表示不限）", settings.MaxRPM))
	}
	if p.BalanceAPI != model.BalanceNone && p.BalanceAPI != model.BalanceDeepSeek {
		return errBad("余额接口只能是 none 或 deepseek")
	}
	for _, f := range []*float64{p.PriceInput, p.PriceCacheHit, p.PriceOutput} {
		if f != nil && (!(*f >= 0) || math.IsInf(*f, 0)) {
			return errBad("价格必须是不小于 0 的数")
		}
	}
	return nil
}

// checkProviderName is the UNIQUE constraint on provider.name (409).
func (st *store) checkProviderName(p api.ProviderView) error {
	for _, o := range st.settings.Providers {
		if o.ID != p.ID && o.Name == p.Name {
			return errConflict(fmt.Sprintf("已有名为「%s」的提供商，请换一个名称", p.Name))
		}
	}
	return nil
}

func isDeepSeekURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && strings.EqualFold(u.Hostname(), "api.deepseek.com")
}

// isDeepSeekHost: the host is deepseek.com or a subdomain (lead ruling 10).
func isDeepSeekHost(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "deepseek.com" || strings.HasSuffix(h, ".deepseek.com")
}

func hashOf(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// sleepCtx waits d unless the client goes away (false).
func sleepCtx(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
		return false
	}
}
