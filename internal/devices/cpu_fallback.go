// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// The CPU device of a computer without an accelerator (next-33, the
// fresh-cuda follow-up; NOTES "决定（next-33：core-builder）"). Such a
// computer gets the built-in fw:cpu enabled (Detect: no CUDA, Intel GPU,
// NPU or Apple row) and ov:CPU disabled (SPEC-v2 I6: "ov:CPU 默认禁用，作为最
// 后的兜底"). The hardware recommendation for it is the OpenVINO CPU mode
// (SPEC-v3 V3.4: asr-openvino + small int8), not faster-whisper: after the
// recommended install fw:cpu waits for an environment that is not there
// (component_missing:fw:cpu, fresh-cuda) and ov:CPU stays off, so nothing
// transcribes, and ov:CPU's default model (large-v3-turbo fp16,
// recommendation.json) is not the one installed either.
//
// applyCPUFallback settles that by default (§0.1 默认自动、省心), with a
// component manager only (the packaged app; development and test mode keep
// the earlier behaviour), and only on a computer whose device table has no
// accelerator row at all (present or not, as Detect's fw:cpu default): when
// no enabled, present CPU device can run now or once an unfinished install
// ends (WorkerState ready or installing), the highest-priority present CPU
// device whose backend environment is installed is enabled, once
// (kvCPUFallback: a device the rule enabled and the user turned off is
// never turned on again, nor is another one), and given an installed model
// when its own is neither installed nor being installed: the first of
// InstalledCandidates, else the first installed of ModelChoices ×
// PrecisionChoices. When no model is installed yet (the recommended install
// puts the environment in before the model), the model is matched once
// more after one is (model_pending; the device detection runs again when an
// installed model changes, cmd subscribeRedetect). An enabled CPU device
// the rule did not enable is left as it is (its settings are the user's or
// the defaults); another CPU device is enabled instead.
//
// Upgrades keep the user's settings (R15 item 1): before the rule first
// acts, a database where a lecture's asr job ever started (asrRan: an
// existing user, whose disabled ov:CPU may be the user's choice) gets the
// mark markSettled and nothing changes, then or later; while a Colab or
// SSH device is enabled (remoteEnabled) the rule waits without a mark.
// Only a fresh install that never transcribed and has no enabled remote
// device has its CPU device turned on.

// kvCPUFallback records what applyCPUFallback did (cpuFallbackMark, JSON).
const kvCPUFallback = "asr_devices.cpu_fallback"

// markSettled is the Device of a mark that enabled nothing and ends the
// rule: written when transcription ran before the rule first had to act
// (an upgrade), or read for a mark that cannot be parsed.
const markSettled = "?"

// remoteEnabled reports whether a Colab or SSH device row is enabled. The
// stored rows count, not List: colab:remote is listed only once the
// scheduler reports it present and an SSH row only with the SSH servers
// on, which may come after a detection at startup; an enabled remote row is
// the user's choice of where to transcribe either way.
func (r *Registry) remoteEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.rows {
		if d.Enabled && model.IsRemoteDevice(d.ID) {
			return true
		}
	}
	return false
}

// cpuFallbackMark is the value of kvCPUFallback.
type cpuFallbackMark struct {
	Device string `json:"device"` // the device the rule enabled
	// ModelPending: no model of its backend was installed when it was
	// enabled; the next run matches its model to an installed one, once.
	ModelPending bool `json:"model_pending,omitempty"`
}

// envInstalled reports whether the backend's worker environment is there
// (WorkerState's first check).
func (r *Registry) envInstalled(backend string) bool {
	py, _, component := r.cfg.WorkerPython(backend)
	return component || executableExists(py)
}

// installedModel is a model of d's backend that is installed (ok false:
// none): the first of InstalledCandidates(d), else the first available of
// ModelChoices × PrecisionChoices.
func (r *Registry) installedModel(d model.ASRDevice) (mdl, precision string, ok bool) {
	if c := InstalledCandidates(r.cfg, r.dataDir, d); len(c) > 0 {
		return c[0].Model, c[0].Precision, true
	}
	for _, m := range ModelChoices(d.Backend) {
		for _, p := range PrecisionChoices(d.Backend) {
			if ModelAvailable(r.cfg, r.dataDir, d.Backend, m, p) {
				return m, p, true
			}
		}
	}
	return "", "", false
}

// modelMissing: the device's own model is neither installed nor being
// installed.
func (r *Registry) modelMissing(d model.ASRDevice) bool {
	return WorkerState(r.cfg, r.dataDir, d.Backend, d.Model, d.Precision) == WorkerNoModel
}

// applyCPUFallback applies the rule above; it returns the id of the device
// it changed ("" when nothing changed). Detect calls it after every
// detection (detectMu held, so two runs never race), before its OnChange
// hooks. It runs WorkerState (file stats, the component manager's lock):
// never call it with r.mu or r.wmu held.
func (r *Registry) applyCPUFallback(ctx context.Context) (string, error) {
	if r.cfg == nil || r.cfg.Components == nil || r.testMode() {
		return "", nil
	}
	var cpus []model.ASRDevice // priority order (List)
	for _, d := range r.List() {
		if model.IsRemoteDevice(d.ID) {
			continue // Colab, SSH (an SSH row's backend is its engine): not this computer
		}
		switch d.Backend {
		case model.BackendFasterWhisper, model.BackendOpenVINO, model.BackendMLX:
		default:
			continue // Colab, SSH: not this computer
		}
		if isAccelerator(d.ID) {
			return "", nil // not a computer without an accelerator
		}
		if model.DeviceKind(d.ID) == model.DeviceKindCPU {
			cpus = append(cpus, d)
		}
	}
	var mark cpuFallbackMark
	if v, ok, err := r.st.GetKV(ctx, kvCPUFallback); err != nil {
		return "", err
	} else if ok && strings.TrimSpace(v) != "" {
		if json.Unmarshal([]byte(v), &mark) != nil || mark.Device == "" {
			mark = cpuFallbackMark{Device: markSettled} // unreadable: the rule ran before
		}
	}
	for _, d := range cpus {
		if d.Enabled && r.Present(d.ID) {
			st := WorkerState(r.cfg, r.dataDir, d.Backend, d.Model, d.Precision)
			if st == WorkerReady && d.ID == mark.Device && mark.ModelPending {
				// its model is there now (installed, or one the user chose):
				// nothing left to match
				return "", r.st.SetKV(ctx, kvCPUFallback, mustJSON(cpuFallbackMark{Device: d.ID}))
			}
			if st == WorkerReady || st == WorkerInstalling {
				return "", nil // a CPU device works, or will once its install ends
			}
		}
	}
	if mark.Device == "" {
		// The rule has not acted yet. Only a fresh install gets it (R15
		// item 1: an upgrade never changes the user's device settings).
		ran, err := asrRan(ctx, r.st)
		if err != nil {
			return "", err // nothing changed; the next detection tries again
		}
		if ran {
			// transcription ran here before (an existing user, whose ov:CPU
			// may be off on purpose): settled for good, nothing changed
			if err := r.st.SetKV(ctx, kvCPUFallback, mustJSON(cpuFallbackMark{Device: markSettled})); err != nil {
				return "", err
			}
			slog.Info("asr devices: transcription ran before; the CPU device settings are left as they are (one time)")
			return "", nil
		}
		if r.remoteEnabled() {
			return "", nil // the user transcribes on Colab or an SSH server
		}
	}
	for _, d := range cpus {
		if !r.Present(d.ID) || !r.envInstalled(d.Backend) {
			continue
		}
		var p model.ASRDevicePatch
		next := mark
		switch {
		case !d.Enabled && mark.Device != "":
			return "", nil // enabled once already; the user's "off" stands
		case !d.Enabled:
			on := true
			p.Enabled, next.Device = &on, d.ID
		case d.ID != mark.Device || !mark.ModelPending:
			continue // enabled by default or by the user: theirs
		}
		next.ModelPending = false
		if r.modelMissing(d) {
			if m, prec, ok := r.installedModel(d); ok {
				p.Model, p.Precision = &m, &prec
			} else {
				next.ModelPending = true
			}
		}
		if p.Enabled == nil && p.Model == nil && next == mark {
			return "", nil // waiting for a model to be installed
		}
		if err := r.applyFallback(ctx, d.ID, p, next); err != nil {
			return "", err
		}
		got, _ := r.Get(d.ID)
		msg := "asr devices: no device of this computer could transcribe; turned on its CPU device (one time)"
		if p.Enabled == nil {
			msg = "asr devices: the CPU device turned on earlier gets the model that is installed now (one time)"
		}
		slog.Info(msg, "device", d.ID, "model", got.Model, "precision", got.Precision, "model_pending", next.ModelPending)
		return d.ID, nil
	}
	return "", nil
}

func mustJSON(m cpuFallbackMark) string {
	b, _ := json.Marshal(m) // two plain fields: cannot fail
	return string(b)
}

// applyFallback writes the patch and the mark in one transaction (the row
// is read again inside it, so the fields the patch does not set keep any
// change made meanwhile).
func (r *Registry) applyFallback(ctx context.Context, id string, p model.ASRDevicePatch, mark cpuFallbackMark) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	err := r.st.Tx(ctx, func(tx db.Store) error {
		d, err := tx.GetASRDevice(ctx, id)
		if errors.Is(err, db.ErrNotFound) {
			return notFound(id)
		} else if err != nil {
			return err
		}
		if err := applyPatch(&d, p); err != nil {
			return err
		}
		if err := tx.UpdateASRDevice(ctx, d); err != nil {
			return err
		}
		return tx.SetKV(ctx, kvCPUFallback, mustJSON(mark))
	})
	if err == nil {
		err = r.reload(ctx)
	}
	if err != nil {
		return fmt.Errorf("启用本机的 CPU 转写设备失败: %w", err)
	}
	return nil
}
