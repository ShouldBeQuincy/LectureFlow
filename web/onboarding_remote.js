// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The download step when the user chose the remote GPU (3.1.0,
// .claude/v31-brief.md §1, §2.2): instead of the transcription components,
//   1. the gcloud tool component (started in the background when the data
//      folder was confirmed, startChosenInstall; started here otherwise),
//      with the same task progress as the local download;
//   2. "sign in to Google": the in-app sign-in of colab_login.js (shared
//      with the settings page's Colab section);
//   3. once signed in: authorized, Colab is switched on (the user chose
//      it), and the account is shown (colab-obs, user decision 2026-10-27
//      14:50: the tier follows the account, it is not picked here): paid /
//      free, the paid balance, the free quota and the GPUs it may borrow;
//      not read yet: "only the T4 for now, checked again by itself", with
//      "重新查询". The account is read right after the sign-in; its SSE
//      "changed colab" reloads the view.
// Already signed in (GET /api/colab): straight to 3. Closing the app and
// coming back resumes from the server's state (a sign-in still waiting is
// followed again).

import { h, btn } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { obApi } from './onboarding_api.js';
import { createColabLogin } from './colab_login.js';
import * as C from './settings_colab.js';
import { keepChildren } from './components_ui.js';

export function remoteStep(env) {
  const S = {
    loading: true,
    unavailable: false, // this server has no Colab back end (503)
    colab: null, // ColabView
    colabError: '',
    refreshing: false, // "重新查询"
  };
  let alive = true;
  const login = createColabLogin({
    rerender: () => alive && env.rerender(),
    onAuthorized: loadColab,
    taskId: () => env.data.gcloud_task_id || '',
    saveTaskId: (id) => {
      env.data.gcloud_task_id = id;
      env.save();
    },
    fkey: 'ob:remote',
  });

  const authorized = () => !!(S.colab && S.colab.status && S.colab.status.auth && S.colab.status.auth.present);
  // the Google Cloud CLI's section, kept across the step's redraws so that
  // the install's task view stays where it is (R81 L8; onboarding.js rerender)
  const gSection = h('div', { class: 'ob-section' });
  // the sign-in's section, kept likewise: its failure's alert and its
  // waiting status stay where they are (R91 low-5; colab_login.js loginBlock)
  const loginSection = h('div', { class: 'ob-section' });

  start();

  async function start() {
    S.loading = true;
    S.colabError = '';
    let colab = null;
    try {
      colab = await obApi.colab({ quiet: true });
    } catch (e) {
      if (e && (e.status === 503 || e.status === 404)) S.unavailable = true;
      else S.colabError = e.message;
    }
    if (!alive) return;
    S.colab = colab;
    if (authorized()) await ensureEnabled();
    else if (!S.unavailable) await login.start({ install: true });
    S.loading = false;
    if (alive) env.rerender();
  }

  async function loadColab() {
    const v = await obApi.colab({ quiet: true }).catch(() => null);
    if (!alive || !v) return;
    S.colab = v;
    if (authorized()) await ensureEnabled();
  }

  // The user chose the remote GPU: switch Colab on once it is authorized.
  async function ensureEnabled() {
    if (!S.colab || !S.colab.settings || S.colab.settings.enabled) return;
    try {
      const v = await obApi.colabSettings({ enabled: true });
      if (alive && v) S.colab = v;
    } catch {
      // shown by the error handler; the switch stays in the settings
    }
  }

  // "重新查询": the account is read from Colab now (the answer is shown).
  async function recheck() {
    if (S.refreshing) return;
    S.refreshing = true;
    env.rerender();
    try {
      const v = await C.refreshAccount();
      if (alive && v && v.status) S.colab = v;
    } catch {
      // shown by the error handler; Colab reads the account again by itself
    }
    S.refreshing = false;
    if (alive) env.rerender();
  }

  // ---- render ----

  // The account (no choice to make: the tier follows it): paid / free, the
  // paid balance, the free quota, the GPUs it may borrow. Not read: only
  // the T4 for now, read again by itself, and "重新查询".
  function accountBlock() {
    const st = S.colab.status || {};
    const a = C.accountOf(st);
    if (!a) return null; // a back end without the account
    const known = C.accountKnown(a);
    const paid = a.level === 'paid';
    const balance = C.paidBalance(a);
    const quota = C.freeQuotaText(a);
    const sel = Array.isArray(st.selectable_accelerators) && st.selectable_accelerators.length ? st.selectable_accelerators : ['T4'];
    const err = C.accountErrorText(a);
    const notes = [];
    if (!known) {
      notes.push(
        h(
          'div',
          { class: 'ob-callout is-warn', role: 'status', title: (err && (err.detail || err.text)) || undefined, style: { marginTop: '10px' } },
          icon('lock', { size: 16 }),
          h(
            'div',
            null,
            h('p', null, t('onboarding.remote.account_unknown')),
            btn(t('resources.remote.refresh'), { size: 'sm', icon: S.refreshing ? 'loader-circle' : 'refresh-cw', disabled: S.refreshing, fkey: 'ob:remote:recheck', onclick: recheck }),
          ),
        ),
      );
    } else if (!paid) notes.push(h('p', { class: 'ob-hint' }, t('settings.colab.tier_free_hint')));
    return h(
      'fieldset',
      { class: 'ob-field ob-account' },
      h('legend', { class: 'ob-label' }, t('settings.colab.account_label')),
      h(
        'div',
        { class: 'acct-box' },
        h('div', { class: 'acct-line' }, h('span', { class: 'chip ' + (paid ? 'chip-run' : 'chip-quiet') }, C.levelText(st))),
        balance != null ? h('div', { class: 'acct-cu' }, h('span', { class: 'acct-num' }, C.cuText(balance)), h('span', { class: 'acct-unit' }, t('resources.remote.balance_k'))) : null,
        quota ? h('div', { class: 'acct-quota small' }, icon('coins', { size: 13 }), h('span', null, quota)) : null,
        h('div', { class: 'acct-line ob-account-gpus' }, h('span', { class: 'dim' }, t('settings.colab.accel_label')), known ? h('span', { class: 'mono' }, sel.join(' · ')) : h('span', null, t('resources.remote.cards_t4'))),
      ),
      notes,
    );
  }

  // R93 (as R91 low-5): the start's failure (role="alert") is the same node
  // while it is shown and says the same, so that the step's redraws (a
  // click on "sign in to Google" draws it twice) leave it where it is and a
  // screen reader does not read it again. prev: the one drawn last time,
  // or null when it was not shown then (startAlert is reset by body()):
  // shown again after it was gone (a retry that failed again), it is new.
  let startAlert = null; // {key, el}
  function startFailed(prev) {
    const error = S.colabError || login.startError;
    if (!error) return null;
    const text = t('onboarding.download.start_failed', { error });
    const todo = t('onboarding.download.start_failed_todo');
    const key = JSON.stringify([error, text, todo]);
    startAlert =
      prev && prev.key === key
        ? prev
        : { key, el: h('div', { class: 'ob-callout is-warn', role: 'alert' }, icon('circle-alert', { size: 16 }), h('div', null, h('p', null, text), h('p', { class: 'dim' }, todo))) };
    return startAlert.el;
  }

  function body() {
    const prev = startAlert;
    startAlert = null; // drawn again by startFailed only

    if (S.loading || (!authorized() && login.loading)) return h('div', { class: 'ob-loading' }, icon('loader-circle', { size: 18, class: 'spin' }), h('span', null, t('onboarding.download.starting')));
    if (S.unavailable) return h('p', { class: 'ob-callout is-info' }, icon('info', { size: 16 }), h('span', null, t('settings.colab.unavailable')));
    if (authorized()) return [h('p', { class: 'ob-callout is-ok', role: 'status' }, icon('circle-check', { size: 16 }), h('span', null, t('onboarding.remote.done'))), accountBlock()];
    const g = login.gcloudBlock();
    if (g) keepChildren(gSection, [g]);
    keepChildren(loginSection, [h('h2', { class: 'ob-h2' }, icon('key-round', { size: 16 }), t('onboarding.remote.login_title')), ...login.loginBlock()]);
    return [startFailed(prev), g ? gSection : null, loginSection];
  }

  return {
    title: () => t('onboarding.remote.title'),
    desc: () => t('onboarding.remote.desc'),
    // what skipping leaves out here: the sign-in (R75 M5); "continue" where
    // the main button is "continue" (signed in, or no Colab here: R81 L5)
    skipTip: () => t(S.unavailable || authorized() ? 'onboarding.skip_tip.continue' : 'onboarding.skip_tip.remote'),
    body,
    secondary() {
      if (!S.loading && (S.colabError || login.startError)) {
        return [btn(t('common.retry'), { kind: 'ghost', icon: 'rotate-cw', fkey: 'ob:remote:restart', onclick: () => start() })];
      }
      const c = login.cancelButton();
      return c ? [c] : [];
    },
    primary() {
      if (S.loading) return { label: t('onboarding.remote.login'), icon: 'log-in', disabled: true };
      if (S.unavailable || authorized()) return { label: t('onboarding.continue'), icon: 'arrow-right', onclick: env.next };
      return login.loginAction();
    },
    onChanged(ev) {
      login.onChanged(ev);
      // the account read after the sign-in (or again later) arrives as "changed colab"
      if (ev && (ev.kind === 'colab' || ev.kind === 'settings') && authorized() && !S.refreshing) loadColab().then(() => alive && env.rerender());
    },
    destroy() {
      alive = false;
      login.destroy();
    },
  };
}
