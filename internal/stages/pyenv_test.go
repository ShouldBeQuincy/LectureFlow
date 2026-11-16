// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/pyenv"
)

// Every worker run has pyenv's switches, even when the service's own
// environment turns them off (NOTES.md "无遥测").
func TestWorkerRunsWithoutTelemetry(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("env", "")
	t.Setenv("HF_HUB_DISABLE_TELEMETRY", "0")
	t.Setenv("HF_HUB_DISABLE_IMPLICIT_TOKEN", "")
	for _, name := range []string{"asr", "asr_openvino", "extract_pdf"} {
		w := workerCall{name: name, label: "x", code: workerASR, dir: filepath.Join(t.TempDir(), name), job: map[string]any{}, out: "result.json"}
		raw, err := e.deps(nil).runWorker(e.ctx, w)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var env []string
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !pyenv.Has(env) {
			t.Fatalf("%s ran without %q: %q", name, pyenv.TelemetryOff, env)
		}
		has := func(kv string) bool {
			for _, x := range env {
				if x == kv {
					return true
				}
			}
			return false
		}
		if !has("PYTHONIOENCODING=utf-8") || !has("PYTHONUNBUFFERED=1") {
			t.Fatalf("%s lost its own variables: %q", name, env)
		}
	}
}

// The components' import check blocks openvino's telemetry package before
// importing anything (`import openvino` alone reported to Google
// Analytics) and has pyenv's environment.
func TestImportCheckWithoutTelemetry(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "0")
	cmd := importCheckCmd(context.Background(), "python", "", []string{"openvino", "openvino_genai"})
	if len(cmd.Args) != 4 || cmd.Args[1] != "-I" || cmd.Args[2] != "-c" {
		t.Fatalf("args %q", cmd.Args)
	}
	code := cmd.Args[3]
	if !strings.HasPrefix(code, pyenv.BlockTelemetryModules+";") || !strings.HasSuffix(code, "import openvino, openvino_genai") {
		t.Fatalf("code %q", code)
	}
	if !pyenv.Has(cmd.Env) {
		t.Fatalf("env without %q: %q", pyenv.TelemetryOff, cmd.Env)
	}
}
