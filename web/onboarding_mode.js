// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Onboarding step "transcription mode" (3.1.0, .claude/v31-brief.md §1):
// two cards, one choice —
//   - local: transcribe with this computer's hardware; the download step
//     installs the components of "your computer" (the recommended ones when
//     that step chose none);
//   - remote: Google Colab's GPU; the download step installs only the
//     gcloud tool and signs in to Google.
// The card the hardware detection recommends carries "recommended": local
// when it recommends a local back end other than the CPU fallback, remote
// when this computer has only the CPU. Each card says how much it
// downloads (POST /api/components/plan). Saved as data.asr_mode
// ("local" | "remote"; skipping saves "") for the guide's later steps, and
// (ext-step1) as the application's transcription mode, PUT /api/asr-mode,
// which the settings page's switch shows and the diagnostics follow (a
// failure there does not stop the guide: the server falls back to
// data.asr_mode). The recommended card (or the
// mode a link chose, env.modeHint) starts selected; nothing is saved until
// "continue". Without the data folder step (web mode, or a folder given by
// LECTUREFLOW_DATA_DIR) "continue" also starts the install in the
// background (startChosenInstall); otherwise the data folder step does.

import { h } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { obApi, recommendedMode, recommendedComponents } from './onboarding_api.js';
import { planSummary } from './components_ui.js';
import { hwText } from './onboarding_computer.js';
import { startChosenInstall } from './onboarding_download.js';
import { saveASRMode } from './settings_asrmode.js';

const MODES = [
  ['local', 'cpu'],
  ['remote', 'cloud'],
];

/** The ids of the local branch's download (the chosen ones, else the recommendation). */
export function localComponents(data, rec) {
  if (Array.isArray(data.components) && data.components.length) return data.components;
  return recommendedComponents(rec);
}

export function asrModeStep(env) {
  const S = { loading: true, rec: null, plans: {}, busy: false };
  const saved = env.data.asr_mode === 'local' || env.data.asr_mode === 'remote' ? env.data.asr_mode : '';
  let choice = env.modeHint || saved;
  env.modeHint = '';
  let alive = true;

  load();

  async function load() {
    // the hardware detection of "your computer" (cached by the server)
    const hw = env.cache.hardware || (await obApi.hardware({ quiet: true }).catch(() => null));
    if (!alive) return;
    if (hw) env.cache.hardware = hw;
    S.rec = hw ? hw.recommendation : null;
    if (!choice) choice = recommendedMode(S.rec);
    S.loading = false;
    env.rerender();
    const local = localComponents(env.data, S.rec);
    const [lp, rp] = await Promise.all([local.length ? obApi.plan(local, { quiet: true }).catch(() => null) : null, obApi.plan(['gcloud'], { quiet: true }).catch(() => null)]);
    if (!alive) return;
    S.plans = { local: lp, remote: rp };
    env.rerender();
  }

  async function proceed() {
    if (!choice || S.busy) return;
    S.busy = true;
    env.rerender();
    const patch = { asr_mode: choice };
    // "install as recommended" when "your computer" chose nothing
    if (choice === 'local' && !(Array.isArray(env.data.components) && env.data.components.length) && S.rec) {
      patch.components = recommendedComponents(S.rec);
      patch.custom = false;
    }
    await env.setData(patch);
    await saveASRMode(choice).catch(() => {});
    if (!env.storageShown) await startChosenInstall(env);
    S.busy = false;
    if (alive) env.next();
  }

  function card(mode, ic) {
    const rec = recommendedMode(S.rec) === mode;
    const on = choice === mode;
    const plan = S.plans[mode];
    return h(
      'label',
      { class: 'ob-mode' + (on ? ' is-on' : ''), dataset: { mode } },
      h('input', {
        type: 'radio',
        name: 'ob-mode',
        value: mode,
        checked: on,
        disabled: S.busy,
        dataset: { fkey: 'ob:mode:' + mode },
        onchange: () => {
          choice = mode;
          env.rerender();
        },
      }),
      h('span', { class: 'ob-mode-icon', 'aria-hidden': 'true' }, icon(ic, { size: 18 })),
      h(
        'span',
        { class: 'ob-mode-text' },
        h('span', { class: 'ob-mode-head' }, h('span', { class: 'ob-mode-name' }, t(`onboarding.asr_mode.${mode}.name`)), rec ? h('span', { class: 'chip chip-run' }, t('onboarding.asr_mode.recommended')) : null),
        h('span', { class: 'ob-mode-desc' }, t(`onboarding.asr_mode.${mode}.desc`)),
        plan && plan.download_bytes ? h('span', { class: 'ob-mode-size' }, planSummary(plan)) : null,
      ),
    );
  }

  return {
    title: () => t('onboarding.asr_mode.title'),
    desc: () => t('onboarding.asr_mode.desc'),
    body() {
      if (S.loading) return h('div', { class: 'ob-loading' }, icon('loader-circle', { size: 18, class: 'spin' }), h('span', null, t('onboarding.loading')));
      // only the CPU: why the remote GPU is recommended
      const notes = recommendedMode(S.rec) === 'remote' ? ((S.rec && S.rec.notes) || []).map(hwText).filter(Boolean) : [];
      const group = h('div', { class: 'ob-modes', role: 'radiogroup', 'aria-label': t('onboarding.asr_mode.title') }, MODES.map(([m, ic]) => card(m, ic)));
      // r7 (T22 G13): "install as recommended" of the step before only chose
      // the components; when the download starts is said here — only when
      // the chosen way has something to download (R75 L6: all installed,
      // the guide run again from the settings, nothing starts)
      const plan = S.plans[choice];
      const when = plan && plan.download_bytes > 0 ? h('p', { class: 'ob-hint' }, icon('download', { size: 14 }), ' ', t(env.storageShown ? 'onboarding.asr_mode.download_after_storage' : 'onboarding.asr_mode.download_after')) : null;
      return [group, notes.map((n) => h('p', { class: 'ob-callout is-info' }, icon('info', { size: 16 }), h('span', null, n))), when];
    },
    primary: () => ({ label: t('onboarding.continue'), icon: 'arrow-right', disabled: S.loading || S.busy || !choice, onclick: proceed }),
    onSkip() {
      env.data.asr_mode = '';
    },
    destroy() {
      alive = false;
    },
  };
}
