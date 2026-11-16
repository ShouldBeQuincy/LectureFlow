// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 3.2.0 fix: the hover card of a running remote asr stage follows the
// device's card in the resources, the other lines that change while a stage
// runs or waits (time, waiting reason, tokens) are redrawn when the card
// opens and with each progress event, and a card the resources cannot bring
// up to date reads the lecture again, throttled
// (testdata/pipeline_remote_test.mjs); every view that draws a lecture's
// pipeline gives it the lecture's id.
func TestPipelineRemoteStepsFollowTheCard(t *testing.T) {
	for _, f := range []string{"view_lecture.js", "lecture_list.js", "view_dashboard.js"} {
		if src := readWeb(t, f); !strings.Contains(src, "lecture: lec.id,") {
			t.Errorf("%s does not give pipelineBar the lecture's id", f)
		}
	}
	src := readWeb(t, "pipeline.js")
	for _, want := range []string{"export function remoteNow(st, opts)", "wrap.refresh = () => refreshCard(card, st, opts);", "if (wrap.refresh) wrap.refresh();",
		"const rs = remoteNow(st, opts);", "export function freshStage(st, opts)", "const fresh = freshStage(st, opts);"} {
		if !strings.Contains(src, want) {
			t.Errorf("pipeline.js lacks %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/pipeline_remote_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "pipeline_remote_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "pipeline remote checks: ok") {
		t.Fatalf("pipeline_remote_test.mjs: %v\n%s", err, out)
	}
}
