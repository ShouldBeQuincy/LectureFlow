// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Onboarding step 3, "your computer" (SPEC-v3 V4.2, V3.4): what the
// hardware detection found, in plain words; the recommended components and
// how much they download; "install as recommended", or "customise" (add or
// remove components and models, pick the download sources). Users in
// mainland China get the mirror sources preselected once (suggest_mirror),
// with a quiet way back to the default. The download itself starts in step 5.
// 3.1.0 M2: when the plan fails (GET /api/hardware plan_error, or POST
// /api/components/plan answering an error) the error is shown instead of a
// size, with "detect again"; nothing is estimated silently.

import { h, btn, errorBlock } from './dom.js';
import { icon } from './icons.js';
import { t, has } from './i18n.js';
import * as U from './util.js';
import { obApi } from './onboarding_api.js';
import { componentPicker, componentList, planSummary, planSpaceText, sourcePicker, usesMirror, mirrorSources, defaultSources } from './components_ui.js';

/** The text of a hardware reason or note (codes are full i18n keys: hw.reason.*, hw.note.*). */
export function hwText(info) {
  if (!info || !info.code) return '';
  if (!has(info.code)) return info.detail || '';
  const p = { ...(info.params || {}) };
  for (const [k, v] of Object.entries(p)) {
    if (typeof v === 'number' && !Number.isInteger(v)) p[k] = U.fmtDecimal(v, 1);
    else if (typeof v === 'number') p[k] = U.fmtInt(v);
  }
  return t(info.code, p);
}

export function computerStep(env) {
  const S = {
    loading: true,
    hwError: '',
    hw: null,
    rec: null,
    comps: null,
    sources: null,
    plan: null,
    planning: false,
    planError: '', // POST /api/components/plan failed: its text
    hwPlanError: null, // GET /api/hardware plan_error (ErrorInfo)
    custom: !!env.data.custom,
    selected: new Set(Array.isArray(env.data.components) ? env.data.components : []),
    fromRec: !Array.isArray(env.data.components),
    busy: false,
  };
  let planSeq = 0;

  load();

  async function load(detect = false) {
    S.loading = true;
    S.hwError = '';
    env.rerender();
    const [hw, comps, sources] = await Promise.all([
      (detect ? obApi.detectHardware({ quiet: true }) : obApi.hardware({ quiet: true })).catch((e) => {
        S.hwError = e.message;
        return null;
      }),
      obApi.components({ quiet: true }).catch(() => null),
      obApi.sources({ quiet: true }).catch(() => null),
    ]);
    if (!env.alive()) return;
    S.hw = hw ? hw.hw : null;
    S.rec = hw ? hw.recommendation : null;
    S.hwPlanError = (hw && hw.plan_error) || null;
    if (hw) env.cache.hardware = hw; // the "transcription mode" step uses it too
    S.comps = comps ? comps.components || [] : null;
    S.sources = sources;
    env.cache.components = S.comps;
    if (S.fromRec && S.rec) S.selected = new Set((S.rec.components || []).filter((id) => id !== 'core'));
    S.loading = false;
    await suggestMirror();
    await replan();
  }

  // V3.3: users in mainland China get the mirrors preselected once.
  async function suggestMirror() {
    const src = S.sources;
    if (!src || !src.suggest_mirror || env.data.sources_decided || usesMirror(src)) return;
    try {
      S.sources = await obApi.saveSources(mirrorSources(src), { quiet: true });
      env.data.mirror_suggested = true;
    } catch {
      return; // the default source still works
    }
    env.data.sources_decided = true;
    env.save();
  }

  async function setSources(next, { suggested = false } = {}) {
    if (!next) return;
    try {
      S.sources = await obApi.saveSources(next);
    } catch {
      return; // shown by the error handler
    }
    env.data.sources_decided = true;
    env.data.mirror_suggested = suggested;
    env.save();
    if (env.alive()) env.rerender();
  }

  async function replan() {
    const seq = ++planSeq;
    S.planning = true;
    env.rerender();
    let plan = null;
    let error = '';
    try {
      plan = await obApi.plan([...S.selected], { quiet: true });
    } catch (e) {
      plan = null;
      error = e.message || t('common.unknown_error');
    }
    if (!env.alive() || seq !== planSeq) return;
    S.plan = plan;
    S.planError = error;
    S.planning = false;
    env.rerender();
  }

  function toggle(id, on) {
    if (on) S.selected.add(id);
    else S.selected.delete(id);
    S.fromRec = false;
    replan();
  }

  function implied() {
    const out = new Set();
    for (const id of (S.plan && S.plan.ids) || []) if (!S.selected.has(id)) out.add(id);
    return out;
  }

  async function proceed() {
    S.busy = true;
    env.rerender();
    await env.setData({ components: [...S.selected], custom: S.custom });
    S.busy = false;
    env.next();
  }

  function detected() {
    if (S.hwError) return errorBlock(t('onboarding.computer.detect_failed', { error: S.hwError }), () => load(true));
    if (!S.hw || !S.rec) return null;
    const reasons = (S.rec.reasons || []).map(hwText).filter(Boolean);
    const notes = (S.rec.notes || []).map(hwText).filter(Boolean);
    const facts = [];
    if (S.hw.ram_mb) facts.push(t('onboarding.computer.ram', { size: U.fmtMB(S.hw.ram_mb) }));
    // round 9 WV-11 (R86 L3; C3): the disk in decimal units, as the
    // "storage" step shows the same disk (fmtBytes); the memory stays fmtMB
    if (S.hw.disk_free_mb) facts.push(t('onboarding.computer.disk', { size: U.fmtBytes(S.hw.disk_free_mb * 1048576) }));
    return h(
      'div',
      { class: 'ob-section' },
      h('h2', { class: 'ob-h2' }, t('onboarding.computer.found')),
      h('ul', { class: 'ob-points' }, reasons.map((r) => h('li', null, icon('cpu', { size: 16 }), h('span', null, r)))),
      notes.map((n) => h('p', { class: 'ob-callout is-info' }, icon('info', { size: 16 }), h('span', null, n))),
      facts.length ? h('p', { class: 'ob-hint' }, facts.join(' · '), ' ', h('button', { type: 'button', class: 'ob-linkbtn', dataset: { fkey: 'ob:redetect' }, onclick: () => load(true) }, t('onboarding.computer.redetect'))) : null,
    );
  }

  // The plan failed: what the server said (no estimated size instead).
  function planErrorBlock() {
    const text = S.planError || (!S.plan && S.hwPlanError ? U.errorInfoText(S.hwPlanError, t('common.unknown_error')) : '');
    if (!text) return null;
    return h(
      'div',
      { class: 'ob-callout is-warn', role: 'alert' },
      icon('circle-alert', { size: 16 }),
      h(
        'div',
        null,
        h('p', null, t('onboarding.computer.plan_failed', { error: text })),
        h('p', { class: 'dim' }, t('onboarding.computer.plan_failed_todo'), ' ', h('button', { type: 'button', class: 'ob-linkbtn', dataset: { fkey: 'ob:plan:retry' }, onclick: () => load(true) }, t('onboarding.computer.redetect'))),
      ),
    );
  }

  // r7 (T22 S3): not enough room on the data folder's disk. With the guide's
  // own "data folder" step to come (env.storageShown: desktop app, the folder
  // can be moved), the user continues as usual and changes the folder there:
  // nothing is downloaded before that step (startChosenInstall). Without it
  // (web mode, a folder set by LECTUREFLOW_DATA_DIR) the main action stays
  // off and the settings are the way out, as before.
  function spaceTodo() {
    if (env.storageShown) return h('p', { class: 'dim' }, t('onboarding.computer.space_todo_storage'));
    return h('p', { class: 'dim' }, t('onboarding.computer.space_todo'), ' ', h('a', { href: '#/settings', class: 'ob-inline-link' }, t('onboarding.computer.open_settings')));
  }

  function planBlock() {
    const plan = S.plan;
    const space = planSpaceText(plan);
    return h(
      'div',
      { class: 'ob-section' },
      h('h2', { class: 'ob-h2' }, S.custom ? t('onboarding.computer.chosen') : t('onboarding.computer.recommended')),
      S.custom && S.comps
        ? componentPicker({ components: S.comps, selected: S.selected, implied: implied(), recommended: new Set((S.rec && S.rec.components) || []), onToggle: toggle })
        : plan
          ? componentList(plan.ids, S.comps)
          : S.selected.size
            ? componentList([...S.selected], S.comps) // the plan failed: what was chosen, without a total
            : null,
      h('p', { class: 'ob-sum', 'aria-live': 'polite' }, S.planning && !plan ? t('onboarding.computer.planning') : planSummary(plan)),
      S.planning ? null : planErrorBlock(),
      space
        ? h(
            'div',
            { class: 'ob-callout is-warn', role: 'alert' },
            icon('hard-drive', { size: 16 }),
            h('div', null, h('p', null, space), spaceTodo()),
          )
        : null,
    );
  }

  function sourcesBlock() {
    if (!S.sources) return null;
    if (S.custom) {
      const picker = sourcePicker(S.sources, { onChange: () => setSources(picker.value()) });
      return h('div', { class: 'ob-section' }, h('h2', { class: 'ob-h2' }, t('onboarding.computer.sources')), picker.el);
    }
    if (env.data.mirror_suggested && usesMirror(S.sources)) {
      return h(
        'p',
        { class: 'ob-hint ob-source-line' },
        icon('server', { size: 14 }),
        h('span', null, t('onboarding.computer.mirror_chosen')),
        ' ',
        h('button', { type: 'button', class: 'ob-linkbtn', dataset: { fkey: 'ob:src:default' }, onclick: () => setSources(defaultSources()) }, t('onboarding.computer.mirror_undo')),
      );
    }
    return null;
  }

  function nothingToDo() {
    return !!S.plan && !S.plan.download_bytes;
  }

  return {
    title: () => t('onboarding.computer.title'),
    desc: () => t('onboarding.computer.desc'),
    body() {
      // resumed after "import existing data": the courses are here, the components not yet
      const imported = env.imported ? h('p', { class: 'ob-callout is-ok', role: 'status' }, icon('circle-check', { size: 16 }), h('span', null, t('onboarding.computer.imported'))) : null;
      if (S.loading) return [imported, h('div', { class: 'ob-loading' }, icon('loader-circle', { size: 18, class: 'spin' }), h('span', null, t('onboarding.computer.detecting')))];
      return [imported, detected(), S.comps || S.plan ? planBlock() : null, sourcesBlock()];
    },
    secondary() {
      if (S.loading || !S.comps) return [];
      return [
        btn(S.custom ? t('onboarding.computer.use_recommended') : t('onboarding.computer.customise'), {
          kind: 'ghost',
          icon: S.custom ? 'undo-2' : 'sliders-horizontal',
          fkey: 'ob:custom',
          onclick: () => {
            S.custom = !S.custom;
            if (!S.custom && S.rec) {
              S.selected = new Set((S.rec.components || []).filter((id) => id !== 'core'));
              S.fromRec = true;
              replan();
            }
            env.rerender();
          },
        }),
      ];
    },
    primary() {
      if (S.loading) return { label: t('onboarding.computer.install'), icon: 'download', disabled: true };
      if (nothingToDo()) return { label: t('onboarding.continue'), icon: 'arrow-right', disabled: S.busy, onclick: proceed };
      // too little room only stops the guide here when it has no "data folder" step to change it in
      const noSpace = !!S.plan && S.plan.enough_space === false && !env.storageShown;
      const empty = S.selected.size === 0;
      return {
        label: S.custom ? t('onboarding.computer.install_chosen') : t('onboarding.computer.install'),
        icon: 'download',
        disabled: S.busy || S.planning || noSpace || empty || !S.plan,
        onclick: proceed,
      };
    },
  };
}
