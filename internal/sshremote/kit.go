// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// A server's kit (remote/kit.go) from the component manifest: the python
// runtime of the server's platform, the engine's venv lock (asr-cuda on
// linux-x64, asr-mlx on darwin-arm64), the model component of the device
// row's model and precision, and the worker scripts of this build. URLs use
// the manifest's default sources (MVP; the mirror choice is plan step 6).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// WorkerFiles are the scripts a remote needs (the imports of asr.py and
// asr_mlx.py).
var WorkerFiles = []string{"asr.py", "asr_common.py", "asr_mlx.py", "asr_openvino.py", "textrepair.py"}

// engineOf is the engine a platform runs.
func engineOf(platform string) string {
	if platform == remote.PlatformDarwinArm64 {
		return remote.EngineMLX
	}
	return remote.EngineFasterWhisper
}

// venvComponent is the component whose lock an engine installs.
func venvComponent(engine string) string {
	if engine == remote.EngineMLX {
		return "asr-mlx"
	}
	return "asr-cuda"
}

// modelPrecision is the model component's precision for a device
// precision (faster-whisper "auto" picks by the remote's VRAM; the files
// of float16 and int8_float16 are the same).
func modelPrecision(engine, precision string, vramMB int) string {
	if engine == remote.EngineFasterWhisper {
		if precision == "" || precision == model.PrecisionAuto {
			return config.ResolveComputeType(model.PrecisionAuto, "cuda", vramMB)
		}
	}
	return precision
}

// kitFromManifest builds the kit of platform / engine / model / precision.
func kitFromManifest(manifestPath, workerDir, agentVersion, platform, engine, modelName, precision string) (remote.KitSpec, error) {
	var k remote.KitSpec
	m, err := components.LoadManifest(manifestPath)
	if err != nil {
		return k, err
	}
	base := filepath.Dir(manifestPath)
	expand := func(tmpl string) (string, error) {
		if !strings.HasPrefix(tmpl, "{") {
			return tmpl, nil
		}
		end := strings.IndexByte(tmpl, '}')
		if end < 0 {
			return "", fmt.Errorf("bad URL template")
		}
		ph := tmpl[1:end]
		for _, g := range m.Sources {
			if v, ok := g.Default.Values[ph]; ok && v != "" {
				return strings.TrimRight(v, "/") + tmpl[end+1:], nil
			}
		}
		return "", fmt.Errorf("no source for {%s}", ph)
	}
	py := m.Component("python")
	if py == nil || py.Build(platform) == nil || len(py.Build(platform).Files) != 1 {
		return k, fmt.Errorf("the manifest has no python runtime for %s", platform)
	}
	pb := py.Build(platform)
	pf := pb.Files[0]
	pyURL, err := expand(pf.URL)
	if err != nil {
		return k, err
	}
	venv := m.Component(venvComponent(engine))
	if venv == nil || venv.Build(platform) == nil || venv.Build(platform).Lock == nil {
		return k, fmt.Errorf("the manifest has no %s environment for %s", venvComponent(engine), platform)
	}
	lock := venv.Build(platform).Lock
	lockText, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(lock.Path)))
	if err != nil {
		return k, fmt.Errorf("reading the lock file: %w", err)
	}
	if sum := sha256.Sum256(lockText); hex.EncodeToString(sum[:]) != lock.SHA256 {
		return k, fmt.Errorf("the lock file %s does not match the manifest", lock.Path)
	}
	modelID := components.ModelID(engine, modelName, precision)
	mc := m.Component(modelID)
	if mc == nil || mc.Build(platform) == nil {
		return k, fmt.Errorf("the manifest has no model %s for %s", modelID, platform)
	}
	k = remote.KitSpec{
		Version: agentVersion, Engine: engine,
		Python:  remote.KitArchive{URL: pyURL, SHA256: pf.SHA256, Size: pf.Size, Exe: pb.Exe},
		Lock:    remote.KitLock{Name: filepath.Base(lock.Path), SHA256: lock.SHA256, Text: string(lockText)},
		Imports: venv.CheckImports,
		Model:   remote.KitModel{ID: modelID},
	}
	if g, ok := m.Sources[components.GroupPyPI]; ok {
		k.IndexURL = g.Default.Values[g.Primary]
	}
	for _, f := range mc.Build(platform).Files {
		u, err := expand(f.URL)
		if err != nil {
			return k, err
		}
		k.Model.Files = append(k.Model.Files, remote.KitDownload{Name: f.Name, URL: u, SHA256: f.SHA256, Size: f.Size})
	}
	if err := addWorker(&k, workerDir); err != nil {
		return k, err
	}
	return k, k.Validate()
}

// KitSize is what installing a kit downloads and takes on the server
// (ssh-1b: the wizard's install step says it before starting).
type KitSize struct {
	DownloadBytes  int64 `json:"download_bytes"`
	InstalledBytes int64 `json:"installed_bytes"`
}

// kitSizeFromManifest adds up the manifest's sizes of the python runtime,
// the engine's environment and the model of platform (the worker scripts
// are a few hundred kB and left out).
func kitSizeFromManifest(manifestPath, platform, engine, modelName, precision string) (KitSize, error) {
	var ks KitSize
	m, err := components.LoadManifest(manifestPath)
	if err != nil {
		return ks, err
	}
	for _, id := range []string{"python", venvComponent(engine), components.ModelID(engine, modelName, precision)} {
		b := m.Component(id).Build(platform)
		if b == nil {
			return ks, fmt.Errorf("the manifest has no %s for %s", id, platform)
		}
		ks.DownloadBytes += b.DownloadSize
		ks.InstalledBytes += max(b.InstalledSize, b.DownloadSize)
	}
	return ks, nil
}

// addWorker adds this build's worker scripts.
func addWorker(k *remote.KitSpec, workerDir string) error {
	for _, name := range WorkerFiles {
		b, err := os.ReadFile(filepath.Join(workerDir, name))
		if err != nil {
			return fmt.Errorf("reading the worker script %s: %w", name, err)
		}
		sum := sha256.Sum256(b)
		k.Worker = append(k.Worker, remote.KitFile{Name: name, SHA256: hex.EncodeToString(sum[:]), Data: b})
	}
	return nil
}
