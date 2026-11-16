// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GPUInfo.estimate_raised (C108, R37 recheck E1): the per-task estimate was
// raised in memory after a transcription ran out of GPU memory. Optional:
// left out when false, so the older examples and front ends are unchanged.
// testdata/api/system_gpu_raised.json is the GPU card of GET /api/system in
// that state and decodes without unknown fields.
func TestGPUInfoEstimateRaised(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "api", "system_gpu_raised.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g GPUInfo
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		t.Fatalf("system_gpu_raised.json: %v", err)
	}
	if !g.EstimateRaised || g.EstimatedJobMB <= 0 {
		t.Errorf("system_gpu_raised.json: %+v", g)
	}
	out, err := json.Marshal(GPUInfo{TotalMB: 12227, EstimatedJobMB: 3100})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "estimate_raised") {
		t.Errorf("not raised: estimate_raised is sent: %s", out)
	}
	out, err = json.Marshal(GPUInfo{TotalMB: 12227, EstimatedJobMB: 3900, EstimateRaised: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"estimate_raised":true`) {
		t.Errorf("raised: %s", out)
	}
}
