// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"path/filepath"
	"strings"
	"testing"
)

// C108 (R37 recheck E1): after a transcription ran out of GPU memory the
// diagnostics' GPU card says why "每个任务预估" is above the table's value
// (gpu.estimate_raised); the sentence names the label as the page shows it.
// Looked at in the browser with the mock scenario vram_raised.
func TestGPUCardEstimateRaised(t *testing.T) {
	src := readWeb(t, "view_system.js")
	if !strings.Contains(src, "g.estimate_raised ? h('p', { class: 'sys-gpu-note' }, icon('info', { size: 13 }), t('system.gpu.per_job_raised')) : null") {
		t.Error("view_system.js: the GPU card no longer notes a raised per-task estimate")
	}
	css := readWeb(t, "style.css")
	hasDecls(t, ".sys-gpu-note", cssRule(t, css, ".sys-gpu-note"), "margin: 0;", "font-size: var(--fs-sm);")
	for _, lang := range []string{"zh-CN", "en"} {
		c := loadCatalog(t, filepath.Join(catalogDir, lang+".json"))
		label, note := text(t, c, "system.gpu.per_job"), text(t, c, "system.gpu.per_job_raised")
		if label == "" || note == "" {
			t.Fatalf("%s: system.gpu.per_job %q, per_job_raised %q", lang, label, note)
		}
		if !strings.Contains(note, "“"+label+"”") {
			t.Errorf("%s: system.gpu.per_job_raised does not quote %q: %q", lang, label, note)
		}
	}
}
