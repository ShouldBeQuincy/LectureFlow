// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// v3.2.x P1-c: the card's request lines and the job events' stats
// (testdata/stage_card_llm_test.mjs, which also checks the P1-b parts of
// the card); app.js merges every job event before the view handles it.
func TestStageCardAttempts(t *testing.T) {
	app := readWeb(t, "app.js")
	if !strings.Contains(app, "mergeJobEvent(ev);\n      if (state.view && state.view.onJob) state.view.onJob(ev);") {
		t.Error("app.js does not merge the job event's stats before the view's handler")
	}
	src := readWeb(t, "pipeline.js")
	for _, want := range []string{"export function mergeJobEvent(ev)", "export function liveRows(st, now = Date.now())", "llm.replaceChildren(...liveRows(st));", "llm: f.llm, last_activity_at: f.last_activity_at"} {
		if !strings.Contains(src, want) {
			t.Errorf("pipeline.js lacks %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/stage_card_llm_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "stage_card_llm_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "stage card llm checks: ok") {
		t.Fatalf("stage_card_llm_test.mjs: %v\n%s", err, out)
	}
}

// v3.2.x P1-b: the stage card and the lecture page say when a lecture's
// translations mix models (stats.mixed_models), in a neutral note: the
// secondary text color, no warning or purple tone (the card's checks are in
// stage_card_llm_test.mjs, run by TestStageCardAttempts).
func TestMixedModelsNote(t *testing.T) {
	src := readWeb(t, "pipeline.js")
	for _, want := range []string{"export function mixedModelsText(st)", "const mixed = mixedModelsNote(st);", "t('common.pipe.mixed_models', { models: U.listJoin(list) })"} {
		if !strings.Contains(src, want) {
			t.Errorf("pipeline.js lacks %q", want)
		}
	}
	lec := readWeb(t, "view_lecture.js")
	for _, want := range []string{"mixedModelsText } from './pipeline.js';", "stats(lec),\n          mixedModels(lec),", "const text = mixedModelsText(st);"} {
		if !strings.Contains(lec, want) {
			t.Errorf("view_lecture.js lacks %q", want)
		}
	}
	css := readWeb(t, "style.css")
	for _, sel := range []string{".card-note.card-mixed-models {", ".lec-mixed-models {"} {
		i := strings.Index(css, sel)
		if i < 0 {
			t.Errorf("style.css lacks %q", sel)
			continue
		}
		rule := css[i : i+strings.Index(css[i:], "}")]
		if !strings.Contains(rule, "color: var(--text-2)") {
			t.Errorf("%s is not in the secondary text color: %s", sel, rule)
		}
		for _, bad := range []string{"--warn", "--err", "purple", "violet", "background"} {
			if strings.Contains(rule, bad) {
				t.Errorf("%s has %q: %s", sel, bad, rule)
			}
		}
	}
}

// v3.2.x P1-c: every frozen phase and outcome of an attempt
// (internal/model/llmstats.go, LLMPhase* and LLMOutcome*) has its text in
// every catalog and its own case on the card (pipeline.js outcomeText,
// liveRows), so a value added there cannot reach the card as a raw code.
func TestStageCardKnowsEveryPhaseAndOutcome(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "internal", "model", "llmstats.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*(LLMOutcome|LLMPhase)\w+\s*=\s*"([a-z_]+)"`)
	var outcomes, phases []string
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		if m[1] == "LLMOutcome" {
			outcomes = append(outcomes, m[2])
		} else {
			phases = append(phases, m[2])
		}
	}
	if len(outcomes) < 9 || len(phases) < 2 {
		t.Fatalf("llmstats.go: found outcomes %v, phases %v", outcomes, phases)
	}
	var keys []string
	for _, o := range outcomes {
		keys = append(keys, "common.pipe.outcome."+o)
	}
	for _, p := range phases {
		keys = append(keys, "common.pipe.phase_"+p)
	}
	keys = append(keys, "common.pipe.outcome.truncated_thinking")
	entries, err := os.ReadDir(catalogDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		c := loadCatalog(t, filepath.Join(catalogDir, e.Name()))
		for _, k := range keys {
			if _, ok := c[k]; !ok {
				t.Errorf("%s lacks %s", e.Name(), k)
			}
		}
	}
	src := readWeb(t, "pipeline.js")
	start := strings.Index(src, "export function outcomeText(a) {")
	end := strings.Index(src, "export function lastOutcomeText(")
	if start < 0 || end < start {
		t.Fatal("pipeline.js: outcomeText not found")
	}
	body := src[start:end]
	for _, o := range outcomes {
		if !strings.Contains(body, "case '"+o+"':") {
			t.Errorf("outcomeText has no case for the outcome %q", o)
		}
	}
	for _, p := range phases {
		if !strings.Contains(src, "cur.phase === '"+p+"'") {
			t.Errorf("liveRows does not show the phase %q", p)
		}
	}
}
