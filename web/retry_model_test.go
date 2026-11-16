// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"strings"
	"testing"
)

// v3.2.x P1-b review fixes (R1–R3): the dialog takes its decisions from
// retry_model.js, whose behaviour testdata/retry_model_test.mjs checks with
// node: every snapshot of the diff must be usable for "沿用原模型"; the
// manual fields start from the retried stage's own model only; a provider
// change empties a model name the new provider is not known to offer, and an
// empty model name is refused before sending.
func TestRetryModelDecisions(t *testing.T) {
	src := readWeb(t, "actions.js")
	for _, want := range []string{
		"const { snap, current } = stageModels(stage, asked ? diff : null, st, roleModel(role, providers));",
		// W4: from "换模型重试…" (no diff) the snapshot is judged from the providers the page read
		"const { usable, bad } = asked ? snapshotChoice(diff) : entrySnapshotChoice(stage, snap, settings && Array.isArray(settings.providers) ? settings.providers : null);",
		"reason: (bad && diffReason(bad)) || t('common.model_choice.unusable_default')",
		// G4: the current settings are judged too; the dialog starts on a choice that can work
		"const now = settings ? currentChoiceFor(stage, settings.roles, Array.isArray(settings.providers) ? settings.providers : null, stagesNow) : { usable: true, info: null };",
		"const stagesNow = (lec.stages || []).map((s) => (st && s && s.stage === stage ? st : s));",
		"disabled: !now.usable,",
		"const initial = initialChoice(usable, now.usable);",
		"const start = manualStart(snap, current, providers);",
		"if (!first) model.value = modelAfterProviderChange(model.value, names);",
		"model.required = c === 'manual' && providers.length > 0;",
		"if (!out.model) return t('common.model_choice.model_missing');",
		"if (currentNote) currentNote.hidden = c !== 'current';",
		"{ onsubmit: submit, novalidate: true },",
		"const mt = parseMaxTokens(maxTokens.value, !!(maxTokens.validity && maxTokens.validity.badInput));",
		// only "临时指定" reads the manual fields; another choice never checks them
		"if (c === 'manual') {\n          target = manual();",
		"if (Number.isNaN(mt)) return t('common.model_choice.max_tokens_bad');",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("actions.js lacks %q", want)
		}
	}
	for _, gone := range []string{"|| diff[0]", "names[0]"} {
		if strings.Contains(src, gone) {
			t.Errorf("actions.js still has %q", gone)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/retry_model_test.mjs need it")
	}
	out, err := runNode(t, node, "retry_model_test.mjs")
	if err != nil || !strings.Contains(string(out), "retry model checks: ok") {
		t.Fatalf("retry_model_test.mjs: %v\n%s", err, out)
	}
}

// v3.2.x P1-b: the retry of a summary, repair or translate stage answers
// needs_model_choice with a dialog of three choices (the snapshot, the
// current settings, a model named for this retry), sent from inside the
// dialog so a refused choice is shown there; "换模型重试…" opens the same
// dialog (the card button: cardActions in pipeline.js). The batch
// operations and reruns keep the two-choice dialog.
func TestRetryWithChosenModel(t *testing.T) {
	src := readWeb(t, "actions.js")
	for _, want := range []string{
		"if (action === 'retry_model') return retryWithModel(lec, stage, null);",
		"if (action === 'retry' && MODEL_RETRY_STAGES.has(stage)) return retryLLM(lec, stage);",
		"if (!(e instanceof ApiError) || !e.needsModelChoice) throw e;\n    return retryWithModel(lec, stage, e.body.diff || []);",
		"{ value: 'manual', label: t('common.model_choice.manual'), hint: t('common.model_choice.manual_hint') }",
		"api.lectureRetryManual(lec.id, stage, target, { quiet: true })",
		"api.lectureStageAction(lec.id, stage, 'retry', c, { quiet: true })",
		"err.textContent = (x && x.message) || t('common.unknown_error');",
		"stage === 'translate' ? h('p', { class: 'dlg-warn' }, t('common.model_choice.translate_clears'))",
		"t('common.model_choice.retried_with',",
		"cardActions(s).includes('retry_model')",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("actions.js lacks %q", want)
		}
	}
	// the two-choice dialog of updates, batches and reruns keeps its two
	// choices: the list ends after "current" (R6: which is not offered when
	// one lecture's operation would be refused with it, and the dialog then
	// starts on none; testdata/a11y_test.mjs and retry_model_test.mjs)
	if !strings.Contains(src, "        value: 'current',\n        label: t('common.model_choice.current'),\n        hint: now.usable ? t('common.model_choice.current_hint') : t('common.model_choice.unusable', { reason: errorInfoText(now.info, '') || t('common.unknown_error') }),\n        disabled: !now.usable,\n      },\n    ],\n    initial: initialChoice(!unusable.length, now.usable, null),") {
		t.Error("modelChoiceDialog (update, batch, rerun) should keep its two choices")
	}
	api := readWeb(t, "api.js")
	if !strings.Contains(api, "lectureRetryManual: (id, stage, manual, opts) => post(`/api/lectures/${enc(id)}/stages/${enc(stage)}/retry`, { model_choice: 'manual', manual }, opts)") {
		t.Error("api.js lacks lectureRetryManual with the C10 request body")
	}
	pipe := readWeb(t, "pipeline.js")
	for _, want := range []string{"export const MODEL_RETRY_STAGES = new Set(['summary', 'repair', 'translate']);", "const actions = cardActions(st, fix).map((a) =>", "if (m.source !== 'manual') return label;"} {
		if !strings.Contains(pipe, want) {
			t.Errorf("pipeline.js lacks %q", want)
		}
	}
}

// v3.2.x P1-b (Q10a): "用当前设置重翻此句" in both sentence menus, for a
// sentence without a usable translation, sent with {"use_current": true}
// (retranslate_current.js; its behaviour is checked by
// testdata/stage_card_llm_test.mjs, a refused current setting included).
func TestRetranslateWithCurrentSettings(t *testing.T) {
	api := readWeb(t, "api.js")
	if !strings.Contains(api, "sentenceRetranslateCurrent: (id, idx, opts) => post(`/api/lectures/${enc(id)}/sentences/${idx}/retranslate`, { use_current: true }, opts)") {
		t.Error("api.js lacks sentenceRetranslateCurrent with the use_current body")
	}
	for file, want := range map[string]string{
		"view_player.js":     "can && canRetranslateCurrent(s) ? { label: t('player.menu.retranslate_current'), icon: 'languages', hint: t('player.menu.retranslate_current_hint'), onSelect: () => retranslateCurrent(id, s.idx, S.lec).then((u) => u && applySentence(u)) } : null,", // (round 8 WL-06: with the lecture)
		"view_transcript.js": "can && canRetranslateCurrent(s) ? { label: t('player.menu.retranslate_current'), icon: 'languages', hint: t('player.menu.retranslate_current_hint'), onSelect: () => retranslateCurrentOp(s.idx) } : null,",
	} {
		if !strings.Contains(readWeb(t, file), want) {
			t.Errorf("%s lacks the menu item %q", file, want)
		}
	}
	src := readWeb(t, "retranslate_current.js")
	for _, want := range []string{"return !!s && s.zh_status === 'qa_failed';", "api.sentenceRetranslateCurrent(lectureId, idx, { quiet: true })", "t('player.menu.retranslate_current_unusable', { reason: msg })"} {
		if !strings.Contains(src, want) {
			t.Errorf("retranslate_current.js lacks %q", want)
		}
	}
}
