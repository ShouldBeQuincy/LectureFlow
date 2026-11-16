// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
)

// GET /api/hardware and POST /api/hardware/detect (SPEC-v3 V3.4, V4.2 step
// 3; .claude/v3-brief.md §7.2): the hardware is detected on the first call
// and cached for the life of the server; "重新检测" detects again. The
// recommendation is recomputed on every call, and its download size comes
// from the component manager's plan (what is already installed is not
// counted).

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/hardware", s.getHardware)
		h("POST /api/hardware/detect", s.detectHardware)
	})
}

// hardwareDetectTimeout bounds one detection run (each probe has its own
// shorter timeout; PowerShell's first start can take 20 s).
const hardwareDetectTimeout = 90 * time.Second

// HardwareRecommendation is hwdetect.Recommendation with the download size
// in bytes from the component plan.
type HardwareRecommendation struct {
	Components    []string          `json:"components"`
	Reasons       []model.ErrorInfo `json:"reasons"`
	Notes         []model.ErrorInfo `json:"notes"`
	DownloadBytes int64             `json:"download_bytes"`
}

// HardwareView is the body of both hardware endpoints.
type HardwareView struct {
	HW             hwdetect.HW            `json:"hw"`
	Recommendation HardwareRecommendation `json:"recommendation"`
	// PlanError (3.1.0, M2) is why the component manager cannot plan the
	// recommendation as it stands (components.unknown,
	// components.unsupported_platform, components.unavailable,
	// components.path_too_long, …); null when it can, or when there is no
	// component manager. With an error, recommendation.download_bytes is
	// only what could be planned (or the recommendation's estimate) and the
	// page shows the error instead of the size.
	PlanError *model.ErrorInfo `json:"plan_error"`
}

func (s *Server) getHardware(w http.ResponseWriter, r *http.Request) {
	s.respondHardware(w, r, false)
}

func (s *Server) detectHardware(w http.ResponseWriter, r *http.Request) {
	s.respondHardware(w, r, true)
}

func (s *Server) respondHardware(w http.ResponseWriter, r *http.Request, fresh bool) {
	hw, err := s.hardware(r.Context(), fresh)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := s.hardwareView(hw)
	if v.PlanError != nil {
		slog.Info("hardware: the recommendation cannot be planned", "code", v.PlanError.Code, "params", v.PlanError.Params)
	}
	writeJSON(w, http.StatusOK, v)
}

// hardware returns the cached detection, detecting when there is none or
// fresh is set. Concurrent callers wait for one run. The run does not end
// with the request (a closed page still fills the cache). Every detection
// is remembered in kv hardware.last (ext-step1, rememberHardware).
func (s *Server) hardware(ctx context.Context, fresh bool) (hwdetect.HW, error) {
	return s.detectHardwareWith(context.WithoutCancel(ctx), fresh)
}

// detectHardwareWith is hardware with the detection bound to ctx (the
// startup detection ends with the server).
func (s *Server) detectHardwareWith(ctx context.Context, fresh bool) (hwdetect.HW, error) {
	cs := s.compState()
	cs.hwMu.Lock()
	defer cs.hwMu.Unlock()
	if cs.hw != nil && !fresh {
		return *cs.hw, nil
	}
	dctx, cancel := context.WithTimeout(ctx, hardwareDetectTimeout)
	defer cancel()
	hw, err := cs.detect(dctx, s.dataDir)
	if err != nil {
		return hwdetect.HW{}, err
	}
	if ctx.Err() != nil { // cut short (shutdown): the probes' results are incomplete
		return hwdetect.HW{}, ctx.Err()
	}
	if hw.NVIDIA == nil {
		hw.NVIDIA = []hwdetect.GPU{}
	}
	cs.hw = &hw
	s.rememberHardware(hw)
	return hw, nil
}

// ---- hardware.last (ext-step1, .claude/ext-brief.md §2) ----

// KVHardwareLast keeps the last hardware detection across restarts, so the
// diagnostics (and the resource bar, cmd/lectureflow/nvidia.go) know
// whether this computer has an NVIDIA GPU without running anything (split
// so the error-code scan does not take it for a code). The only definition
// of the key (r4 D10): cmd reads it from here.
const KVHardwareLast = "hardware" + ".last"

// kvHardwareLast is KVHardwareLast under the package's older name.
const kvHardwareLast = KVHardwareLast

// HardwareLast is the JSON of kv hardware.last: what the diagnostics page
// needs to pick its checks, never a serial number or another identifier.
type HardwareLast struct {
	NVIDIA       []string `json:"nvidia"` // the NVIDIA GPUs' names
	IntelGPU     bool     `json:"intel_gpu"`
	IntelNPU     bool     `json:"intel_npu"`
	AppleSilicon bool     `json:"apple_silicon"`
	DetectedAt   string   `json:"detected_at"` // RFC 3339, UTC
}

// hardwareLastOf is the record of one detection.
func hardwareLastOf(hw hwdetect.HW, at time.Time) HardwareLast {
	h := HardwareLast{NVIDIA: []string{}, IntelGPU: hw.IntelGPU, IntelNPU: hw.IntelNPU, AppleSilicon: hw.AppleSilicon,
		DetectedAt: at.UTC().Format(time.RFC3339)}
	for _, g := range hw.NVIDIA {
		if g.Name != "" && !slices.Contains(h.NVIDIA, g.Name) {
			h.NVIDIA = append(h.NVIDIA, g.Name)
		}
	}
	return h
}

// rememberHardware stores the detection in kv hardware.last and, when that
// changes what the diagnostics check (an NVIDIA GPU, an Intel GPU or NPU),
// runs them again.
func (s *Server) rememberHardware(hw hwdetect.HW) {
	h := hardwareLastOf(hw, time.Now())
	prev, had := s.hardwareLast(context.Background())
	b, err := json.Marshal(h)
	if err == nil && s.st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = s.st.SetKV(ctx, kvHardwareLast, string(b))
		cancel()
	}
	if err != nil {
		slog.Warn("hardware: the detection could not be remembered", "err", err)
	}
	if d := s.diag; d != nil {
		d.hwMu.Lock()
		d.hwLast, d.hwRead = &h, true
		d.hwMu.Unlock()
		same := had && (len(prev.NVIDIA) > 0) == (len(h.NVIDIA) > 0) && prev.IntelGPU == h.IntelGPU && prev.IntelNPU == h.IntelNPU
		if !same {
			d.envChanged()
		}
	}
}

// hardwareLast reads kv hardware.last (cached after the first read); false
// when no detection was ever remembered (or the value is unreadable).
func (s *Server) hardwareLast(ctx context.Context) (HardwareLast, bool) {
	d := s.diag
	if d != nil {
		d.hwMu.Lock()
		defer d.hwMu.Unlock()
		if d.hwRead {
			if d.hwLast == nil {
				return HardwareLast{}, false
			}
			return *d.hwLast, true
		}
	}
	if s.st == nil {
		return HardwareLast{}, false
	}
	v, ok, err := s.st.GetKV(ctx, kvHardwareLast)
	if err != nil {
		return HardwareLast{}, false // not cached: read again next time
	}
	var h *HardwareLast
	if ok {
		var x HardwareLast
		if json.Unmarshal([]byte(v), &x) == nil {
			h = &x
		}
	}
	if d != nil {
		d.hwLast, d.hwRead = h, true
	}
	if h == nil {
		return HardwareLast{}, false
	}
	return *h, true
}

// startupHardwareDetect: when no detection was ever remembered, the server
// detects the hardware once in the background at startup (not in test mode,
// asr.fake_result; the package's tests turn it off).
var startupHardwareDetect = true

// detectHardwareAtStartup runs that detection (bound to ctx: shutdown
// cancels it).
func (s *Server) detectHardwareAtStartup(ctx context.Context) {
	if !startupHardwareDetect || s.st == nil || (s.cfg != nil && s.cfg.ASR.FakeResult != "") {
		return
	}
	if _, ok := s.hardwareLast(ctx); ok {
		return
	}
	go func() {
		if _, err := s.detectHardwareWith(ctx, false); err != nil && ctx.Err() == nil {
			slog.Warn("hardware: the startup detection failed", "err", err)
		}
	}()
}

func (s *Server) hardwareView(hw hwdetect.HW) HardwareView {
	rec := hwdetect.Recommend(hw)
	v := HardwareView{HW: hw, Recommendation: HardwareRecommendation{
		Components: append([]string{}, rec.Components...),
		Reasons:    append([]model.ErrorInfo{}, rec.Reasons...),
		Notes:      append([]model.ErrorInfo{}, rec.Notes...),
	}}
	v.Recommendation.DownloadBytes = int64(rec.DownloadMB) << 20
	if s.comps == nil || len(rec.Components) == 0 {
		return v
	}
	// The whole recommendation first: this is what "按推荐安装" installs.
	p, err := s.comps.Plan(rec.Components)
	if err == nil {
		v.Recommendation.DownloadBytes = p.DownloadBytes
		return v
	}
	_, info := classify(context.Background(), err, "", nil)
	if info.Code == "" || info.Code == "server.internal" {
		info = model.ErrorInfo{Code: "server.internal", Detail: err.Error()}
	}
	v.PlanError = &info
	// The size of what this platform's manifest has (the recommendation may
	// name a component the manifest lists as unavailable or not at all).
	known := map[string]bool{}
	for _, c := range s.comps.List() {
		if c.Available || c.Installed {
			known[c.ID] = true
		}
	}
	var ids []string
	for _, id := range rec.Components {
		if known[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return v
	}
	if p, err := s.comps.Plan(ids); err == nil {
		v.Recommendation.DownloadBytes = p.DownloadBytes
	}
	return v
}
