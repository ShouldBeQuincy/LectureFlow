// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// T-PERF-03: the observed account limits on the settings page (the logic
// is in testdata/provider_limits_test.mjs); the settings page uses the
// module where the concurrency and per-minute fields are.
func TestProviderLimitsLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/provider_limits_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "provider_limits_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "provider limits checks: ok") {
		t.Fatalf("provider_limits_test.mjs: %v\n%s", err, out)
	}
}

func TestProviderLimitsWired(t *testing.T) {
	src := readWeb(t, "view_settings.js")
	for _, want := range []string{
		"import { createLimitsHint, limitsBanner } from './provider_limits.js';",
		"limitsBanner(p, () => providerDialog(p, { fillLimits: true })),",
		"const observed = createLimitsHint(p, conc, rpm, { filled: fillLimits });",
		"observed.sync(type, !!key.value.trim(), base.value);",
		"syncKeyHint();\n          syncLimitsNote();",
		"          typeNote.replaceChildren(...endpointNote(type));\n          syncLimitsNote(); // R38b N1: the limits only for Anthropic's own host",
		"            observed.el,\n            limitsNote,",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_settings.js lacks %q", want)
		}
	}
	// the module never saves: no API call in it
	if lim := readWeb(t, "provider_limits.js"); strings.Contains(lim, "./api.js") || strings.Contains(lim, "api.patch") || strings.Contains(lim, "fetch(") || strings.Contains(lim, "patchProvider") {
		t.Error("provider_limits.js must not send anything")
	}
}
