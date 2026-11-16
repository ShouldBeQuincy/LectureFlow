// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Onboarding step "data folder" (3.1.0 M1, .claude/v31-brief.md §1), shown
// only in the desktop app when the folder can be moved (onboarding.js):
// where the data lives, how much room its disk has (GET /api/app/data-dir
// free_bytes), and what this onboarding is about to download there (POST
// /api/components/plan of the local components, or of ["gcloud"] for the
// remote GPU). When it does not fit, that is said prominently and "change
// location" becomes the main action.
//
// "Change location": the system folder dialog (window.lectureflow.pickFolder)
// → POST /api/app/data-dir/check (the same refusals as the move, nothing
// moved) → a candidate that is refused or too small is said here; one that
// fits is confirmed (the settings page's move dialog) → POST
// /api/app/data-dir, the existing move (a fresh install has hardly any data
// to copy). The onboarding state goes along (kv onboarding.*), the server
// restarts, the page reloads and this step shows the new place
// (data.storage_moved_to). "Keep the default" just continues.
//
// Leaving this step forwards (keep, continue, or skip) starts the chosen
// mode's install in the background at once (startChosenInstall, lead
// 3.1.0): the downloads go to the folder confirmed here.

import { h, btn, confirmDialog } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { api } from './api.js';
import { obApi, desktop } from './onboarding_api.js';
import { planSummary, planSpaceText } from './components_ui.js';
import { localComponents } from './onboarding_mode.js';
import { startChosenInstall } from './onboarding_download.js';

const MOVING = new Set(['copying', 'verifying']);

export function storageStep(env) {
  const S = {
    loading: true,
    dd: env.cache.dataDir || null, // DataDirStatus (+ free_bytes)
    plan: null,
    ids: [],
    loadError: '',
    picking: false,
    checking: false,
    cand: null, // the last refused / too small candidate {path, free_bytes, ok, error}
    moving: null, // DataDirStatus of the move
    moveError: '',
    restarting: false,
    starting: false, // the install is being started (leaving the step)
  };
  let alive = true;
  let pollTimer = null;
  let restartTimer = null;

  load();

  // the components this onboarding downloads next
  function planIDs() {
    if (env.data.asr_mode === 'remote') return ['gcloud'];
    const hw = env.cache.hardware;
    // the local branch (or no choice: the components "your computer" chose)
    if (env.data.asr_mode === 'local') return localComponents(env.data, hw && hw.recommendation);
    return Array.isArray(env.data.components) ? env.data.components : [];
  }

  async function load() {
    S.ids = planIDs();
    const [dd, plan] = await Promise.all([
      obApi.dataDir({ quiet: true }).catch((e) => ((S.loadError = e.message), null)),
      S.ids.length ? obApi.plan(S.ids, { quiet: true }).catch(() => null) : null,
    ]);
    if (!alive) return;
    if (dd) {
      S.dd = dd;
      env.cache.dataDir = dd;
      S.loadError = '';
    }
    S.plan = plan;
    S.loading = false;
    env.rerender();
    // the page opened while a move runs (or just finished)
    if (dd && MOVING.has(dd.state) && dd.kind !== 'import') {
      S.moving = dd;
      schedulePoll();
    } else if (dd && dd.state === 'done' && dd.kind === 'move') waitForRestart();
  }

  const need = () => (S.plan && S.plan.download_bytes ? S.plan.need_bytes || 0 : 0);
  const fits = (free) => !need() || !(free >= 0) || free >= need();

  function freeHere() {
    return S.dd && typeof S.dd.free_bytes === 'number' ? S.dd.free_bytes : -1;
  }

  async function change() {
    const d = desktop();
    if (S.picking || !d || typeof d.pickFolder !== 'function') return;
    S.picking = true;
    S.cand = null;
    env.rerender();
    let path = null;
    try {
      path = await d.pickFolder();
    } catch {
      path = null;
    }
    S.picking = false;
    if (!alive) return;
    if (!path) {
      env.rerender();
      return;
    }
    S.checking = true;
    env.rerender();
    let r = null;
    try {
      r = await obApi.checkDataDir(path);
    } catch {
      r = null; // shown by the error handler
    }
    S.checking = false;
    if (!alive) return;
    // A pick that will not be used (the check failed or refused it, or the
    // user declines below) is given up at once, not left to expire.
    if (!r) {
      api.forgetFolderPick(path);
      env.rerender();
      return;
    }
    if (!r.ok || !fits(r.free_bytes)) {
      api.forgetFolderPick(path);
      S.cand = r;
      env.rerender();
      return;
    }
    env.rerender();
    const ok = await confirmDialog({
      title: t('settings.app.move_title'),
      message: t('settings.app.move_intro', { path: r.path || path }),
      body: h(
        'ul',
        { class: 'app-move-points' },
        r.free_bytes >= 0 ? h('li', null, icon('hard-drive', { size: 14 }), h('span', null, t('settings.app.free', { free: U.fmtBytes(r.free_bytes) }))) : null,
        h('li', null, icon('shield-check', { size: 14 }), h('span', null, t('settings.app.move_point_verify'))),
        h('li', null, icon('rotate-cw', { size: 14 }), h('span', null, t('settings.app.move_point_restart'))),
      ),
      okText: t('onboarding.storage.change'),
    });
    if (!ok) api.forgetFolderPick(path);
    if (!ok || !alive) return;
    // saved first: the onboarding state goes into the moved database
    await env.setData({ storage_moved_to: r.path || path });
    S.moveError = '';
    try {
      const st = await obApi.moveDataDir(r.path || path);
      if (!alive) return;
      S.moving = st && st.state ? st : { state: 'copying', copied_bytes: 0, total_bytes: 0, path };
      schedulePoll();
    } catch {
      delete env.data.storage_moved_to; // shown by the error handler
      env.save();
      // refused: the service returned the pick's grant, and "change
      // location" asks for the folder again, so the pick is given up
      api.forgetFolderPick(path);
    }
    env.rerender();
  }

  function schedulePoll() {
    clearTimeout(pollTimer);
    pollTimer = setTimeout(poll, 800);
  }

  async function poll() {
    if (!alive) return;
    let st = null;
    try {
      st = await obApi.dataDir({ quiet: true });
    } catch {
      // the server went away while copying or verifying: it restarts
      if (S.moving && MOVING.has(S.moving.state)) waitForRestart(true);
      return;
    }
    if (!alive || !st) return;
    if (st.state === 'failed') {
      S.moving = null;
      S.moveError = U.errorInfoText(st.error, t('common.unknown_error'));
      delete env.data.storage_moved_to;
      env.save();
    } else if (st.state === 'done') {
      S.moving = st;
      waitForRestart();
    } else if (MOVING.has(st.state)) {
      S.moving = st;
      schedulePoll();
    } else {
      // idle with the new folder: the restart already happened
      S.moving = null;
      S.dd = st;
    }
    env.rerender();
  }

  // After the move the server restarts; reload once it answers again (the
  // onboarding continues at this step), at the latest after 60 s.
  function waitForRestart(away = false) {
    if (S.restarting) return;
    S.restarting = true;
    env.rerender();
    let wentAway = away;
    const started = Date.now();
    const tick = async () => {
      if (!alive) return;
      try {
        await obApi.health({ quiet: true });
        if (wentAway || Date.now() - started > 60000) {
          location.reload();
          return;
        }
      } catch {
        wentAway = true;
      }
      restartTimer = setTimeout(tick, 1000);
    };
    restartTimer = setTimeout(tick, 1000);
  }

  // ---- render ----

  function movingBlock() {
    if (S.restarting || (S.moving && S.moving.state === 'done')) {
      return h('p', { class: 'ob-callout is-info', role: 'status' }, icon('loader-circle', { size: 16, class: 'spin' }), h('span', null, t('settings.app.move_restarting')));
    }
    const mv = S.moving;
    if (!mv || !MOVING.has(mv.state)) return null;
    const total = mv.total_bytes || 0;
    const frac = mv.state === 'verifying' ? 1 : total > 0 ? Math.min(1, (mv.copied_bytes || 0) / total) : 0;
    const text = mv.state === 'verifying' ? t('settings.app.move_verifying') : t('settings.app.move_copying', { copied: U.fmtBytes(mv.copied_bytes || 0), total: U.fmtBytes(total), percent: U.fmtPercent(frac, 0) });
    return h(
      'div',
      { class: 'ob-callout is-info', role: 'status' },
      icon('loader-circle', { size: 16, class: 'spin' }),
      h(
        'div',
        { class: 'ob-grow' },
        h('p', null, text),
        h('div', { class: 'cu-bar', role: 'progressbar', 'aria-label': text, 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(frac * 100)) }, h('div', { class: 'cu-bar-fill', style: { width: `${Math.round(frac * 100)}%` } })),
      ),
    );
  }

  function candidateBlock() {
    const c = S.cand;
    if (!c) return null;
    const text = !c.ok ? t('onboarding.storage.check_failed', { path: c.path, error: U.errorInfoText(c.error, t('common.unknown_error')) }) : t('onboarding.storage.cand_no_space', { path: c.path, free: U.fmtBytes(c.free_bytes), need: U.fmtBytes(need()) });
    return h('p', { class: 'ob-callout is-warn', role: 'alert' }, icon('circle-alert', { size: 16 }), h('span', null, text));
  }

  function body() {
    if (S.loading && !S.dd) return h('div', { class: 'ob-loading' }, icon('loader-circle', { size: 18, class: 'spin' }), h('span', null, t('onboarding.loading')));
    if (!S.dd) return h('p', { class: 'ob-callout is-warn', role: 'alert' }, icon('circle-alert', { size: 16 }), h('span', null, t('onboarding.storage.load_failed', { error: S.loadError || t('common.unknown_error') })));
    const dir = S.dd.data_dir || '';
    const free = freeHere();
    const moved = env.data.storage_moved_to && dir && env.data.storage_moved_to.toLowerCase() === dir.toLowerCase();
    const plan = S.plan;
    const space = plan && !fits(free) ? planSpaceText({ ...plan, enough_space: false, free_bytes: free }) : '';
    return [
      moved ? h('p', { class: 'ob-callout is-ok', role: 'status' }, icon('circle-check', { size: 16 }), h('span', null, t('onboarding.storage.moved'))) : null,
      h(
        'div',
        { class: 'ob-section ob-store' },
        h('h2', { class: 'ob-h2' }, icon('folder-open', { size: 16 }), t('onboarding.storage.current')),
        h('code', { class: 'ob-store-path mono', title: dir }, dir),
        free >= 0 ? h('p', { class: 'ob-hint' }, t('settings.app.free', { free: U.fmtBytes(free) })) : null,
      ),
      h(
        'div',
        { class: 'ob-section' },
        h('h2', { class: 'ob-h2' }, icon('download', { size: 16 }), t('onboarding.storage.plan_label')),
        h('p', { class: 'ob-sum' }, plan && plan.download_bytes ? planSummary(plan) : S.ids.length && !plan && S.loading ? t('onboarding.computer.planning') : t('onboarding.storage.no_plan')),
      ),
      space
        ? h('div', { class: 'ob-callout is-warn', role: 'alert' }, icon('hard-drive', { size: 16 }), h('div', null, h('p', null, space), h('p', { class: 'dim' }, t('onboarding.storage.no_space_todo'))))
        : null,
      candidateBlock(),
      S.moveError ? h('p', { class: 'ob-callout is-warn', role: 'alert' }, icon('circle-alert', { size: 16 }), h('span', null, t('settings.app.move_failed', { error: S.moveError }))) : null,
      movingBlock(),
    ];
  }

  const busy = () => S.picking || S.checking || S.restarting || S.starting || (!!S.moving && (MOVING.has(S.moving.state) || S.moving.state === 'done'));

  // the location is confirmed: start the downloads there, then go on
  async function proceed() {
    if (S.starting) return;
    S.starting = true;
    env.rerender();
    await startChosenInstall(env);
    S.starting = false;
    if (alive) env.next();
  }
  const canPick = () => {
    const d = desktop();
    return !!d && typeof d.pickFolder === 'function';
  };
  const changeLabel = () => (S.picking ? t('settings.app.picking') : S.checking ? t('onboarding.storage.checking') : t('onboarding.storage.change'));
  const tooSmall = () => !!S.dd && !!S.plan && !fits(freeHere());

  return {
    title: () => t('onboarding.storage.title'),
    desc: () => t('onboarding.storage.desc'),
    body,
    secondary() {
      if (!S.dd || !canPick()) return [];
      // too small here: "change location" is the main action, keeping is the quiet one
      if (tooSmall()) return [{ ...keepButton(), kind: 'ghost' }].map(asBtn);
      return [asBtn({ label: changeLabel(), icon: 'folder-open', kind: 'ghost', disabled: busy(), fkey: 'ob:store:change', onclick: change })];
    },
    primary() {
      if (S.dd && canPick() && tooSmall()) return { label: changeLabel(), icon: 'folder-open', disabled: busy(), onclick: change };
      return { ...keepButton(), disabled: S.loading || busy() };
    },
    // skipping keeps the current folder too
    onSkip: () => startChosenInstall(env),
    destroy() {
      alive = false;
      clearTimeout(pollTimer);
      clearTimeout(restartTimer);
    },
  };

  function keepButton() {
    const moved = env.data.storage_moved_to && S.dd && (S.dd.data_dir || '').toLowerCase() === env.data.storage_moved_to.toLowerCase();
    return { label: moved ? t('onboarding.continue') : t('onboarding.storage.keep'), icon: S.starting ? 'loader-circle' : 'arrow-right', fkey: 'ob:store:keep', onclick: proceed, disabled: busy() };
  }
}

// a footer button from {label, icon, kind, disabled, fkey, onclick}
function asBtn(o) {
  return btn(o.label, { kind: o.kind, icon: o.icon, disabled: o.disabled, fkey: o.fkey, onclick: o.onclick });
}
