// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/pyenv"
)

// The OpenVINO probe runs `python_openvino -s asr_openvino.py --probe`
// (no user site-packages; the script's own directory stays on sys.path)
// without PYTHONPATH and the like (security audit AUD4 L-4).
func TestProbeRunsIsolated(t *testing.T) {
	cfg := probeConfig(t)
	r := NewRegistry(regStore(t), cfg, t.TempDir(), RegistryOptions{CUDAName: func(context.Context) (string, error) { return "", errors.New("none") }})
	r.Load(tctx())
	marker := t.TempDir()
	if err := os.WriteFile(filepath.Join(marker, "json.py"), []byte("raise SystemExit('LF-MARKER imported')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, k := range pyenv.Dropped {
		t.Setenv(k, marker)
	}
	t.Setenv("LF_ISOLATION_MARKER", "kept")
	out := filepath.Join(t.TempDir(), "run.json")
	t.Setenv(fakeProbeEnv, "argsenv")
	t.Setenv(fakeProbeEnvOut, out)
	if _, err := r.workerProbe(tctx()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Args []string `json:"args"`
		Env  []string `json:"env"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(cfg.WorkerDir, "asr_openvino.py")
	if len(got.Args) != 3 || got.Args[0] != "-s" || got.Args[1] != script || got.Args[2] != "--probe" {
		t.Errorf("args %q, want -s %s --probe", got.Args, script)
	}
	has := func(key string) (string, bool) {
		val, found := "", false
		for _, kv := range got.Env {
			k, v, ok := strings.Cut(kv, "=")
			if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
				val, found = v, true
			}
		}
		return val, found
	}
	for _, k := range pyenv.Dropped {
		if v, ok := has(k); ok {
			t.Errorf("inherited %s=%s", k, v)
		}
	}
	if v, _ := has("LF_ISOLATION_MARKER"); v != "kept" || !pyenv.Has(got.Env) {
		t.Errorf("environment %q", got.Env)
	}
}
