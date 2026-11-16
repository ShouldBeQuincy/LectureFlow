// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/stages"
)

// The component manager in the service (SPEC-v3 V3, V8; step 5,
// v5-integration-b1). NOTES "决定（v3 v5-integration-b1）".

// componentOptions are the manager's options from the configuration:
// DataDir; the built-in manifest (cfg.ComponentsManifest:
// resources/components.json, or packaging/components/components.json in
// development); uv (cfg.ComponentsUV: resources/bin/uv, the configured one,
// or tools/uv in development — a missing file only fails installs with
// components.uv_missing); the application version; the manifest URL of the
// update channel (cfg.ComponentsManifestURL with {channel} replaced; empty
// = no network check). SelfTest is set by the caller.
func componentOptions(ctx context.Context, cfg *config.Config, st db.Store, appVersion string) components.Options {
	if appVersion == "" {
		appVersion = version
	}
	return components.Options{
		DataDir:         cfg.DataDir,
		AppVersion:      appVersion,
		BuiltinManifest: cfg.ComponentsManifest,
		ManifestURL:     manifestURL(cfg.ComponentsManifestURL, updateChannel(ctx, st)),
		UV:              cfg.ComponentsUV(),
		Logger:          slog.Default(),
	}
}

// updateChannel is kv app.update_channel (stable | beta; anything else,
// missing or unreadable = stable). Read once at start: a channel change
// applies to the manifest URL after the next start.
func updateChannel(ctx context.Context, st db.Store) string {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	v, ok, err := st.GetKV(rctx, model.KVAppUpdateChannel)
	if err == nil && ok && strings.TrimSpace(v) == "beta" {
		return "beta"
	}
	return "stable"
}

// manifestURL replaces "{channel}" in the configured URL template.
func manifestURL(tmpl, channel string) string {
	return strings.ReplaceAll(strings.TrimSpace(tmpl), "{channel}", channel)
}

// componentCheck turns the manager's self-test request into the stages'
// check (stages.ComponentSelfTest). The model's name and precision come
// from the component id (model components) or from the directory of the
// installed model an environment is tested with
// (components/{model__backend__name__precision}/{version}/model). cuda:
// faster-whisper runs on the NVIDIA GPU (the last detection found one).
func componentCheck(req components.SelfTestRequest, cuda bool) stages.ComponentCheck {
	c := stages.ComponentCheck{ID: req.ID, Backend: req.Backend, Python: req.Python, ModelDir: req.ModelDir,
		CUDA: cuda && req.Backend == model.BackendFasterWhisper}
	if req.Kind == components.KindPythonRuntime {
		c.Backend = config.ComponentBase
	}
	id := req.ID
	if req.Kind != components.KindModel && req.ModelDir != "" {
		id = strings.ReplaceAll(filepath.Base(filepath.Dir(filepath.Dir(req.ModelDir))), "__", ":")
	}
	if b, name, precision, ok := components.ParseModelID(id); ok && b == c.Backend {
		c.Model, c.Precision = name, precision
	}
	return c
}

// detectionInputs is what the device detection depends on: the backend
// interpreters (an ASR environment installed, updated or removed) and the
// installed models (next-33: after the detection, the CPU device of a
// computer without an accelerator gets an installed model, which the
// recommended install puts in after the environment; devices
// cpu_fallback.go). A change calls for a new detection.
func detectionInputs(m *components.Manager) string {
	var b strings.Builder
	for _, backend := range []string{model.BackendFasterWhisper, model.BackendOpenVINO, model.BackendMLX} {
		py, _ := m.Python(backend)
		b.WriteString(py)
		b.WriteByte(0)
	}
	for _, s := range m.List() {
		if s.Kind == components.KindModel && s.Installed && !s.Broken {
			b.WriteString(s.ID + "@" + s.InstalledVersion)
			b.WriteByte(0)
		}
	}
	return b.String()
}

// subscribeRedetect asks for a device detection (non-blocking send on
// redetect) whenever an event of m changed the installed backend
// environments or models, so a newly installed backend appears in the
// device table (and a removed one disappears) without a restart. The
// callback runs on the manager's event goroutine, in order, so last needs
// no lock.
func subscribeRedetect(m *components.Manager, redetect chan<- struct{}) (cancel func()) {
	last := detectionInputs(m)
	return m.Subscribe(func(e components.Event) {
		if e.Type != components.EventComponents {
			return
		}
		cur := detectionInputs(m)
		if cur == last {
			return
		}
		last = cur
		slog.Info("components: backend environments or models changed; detecting the transcription devices again")
		select {
		case redetect <- struct{}{}:
		default: // one detection is already pending
		}
	})
}
