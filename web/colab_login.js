// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The in-app Google sign-in for Colab (3.1.0, .claude/v31-brief.md §2.2),
// shared by the onboarding's remote-GPU step (onboarding_remote.js) and the
// settings page's Colab section (settings_colab.js):
//   - the gcloud tool component: its state (GET /api/components), its
//     install (POST /api/components/install ["gcloud"]) with the progress
//     widget of components_ui.js (pause, cancel, retry, change source);
//   - the sign-in: POST /api/colab/auth/login starts it (gcloud opens the
//     system browser), GET follows it every second (and at once on SSE
//     changed {kind: colab}), DELETE cancels it. While it waits, the sign-in
//     link the server found is offered as a fallback (desktop:
//     window.lectureflow.openExternal, web: a plain link; https only).
//     A failure shows the server's sentence (errors.colab.*) with gcloud's
//     own message behind "details".
// The controller only reports through rerender(); the caller draws the
// blocks it returns and decides what "done" means (onAuthorized).

import { h, btn } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { obApi, desktop, openExternal } from './onboarding_api.js';
import { onComponentsEvent, componentsEventFresh } from './api.js';
import { taskView, taskActive, chooseSourcesDialog } from './components_ui.js';

const POLL_MS = 1000;
const LOGIN_ACTIVE = new Set(['waiting', 'importing']);

/**
 * opts:
 *   rerender()        redraw the caller's view (state changed)
 *   onAuthorized()    the sign-in ended with "done" (reload the authorization)
 *   taskId()          the id of a gcloud install task to follow (saved earlier), or ''
 *   saveTaskId(id)    remember the id of the install task started here
 *   fkey              the prefix of the data-fkey of the controls
 */
export function createColabLogin({ rerender, onAuthorized = () => {}, taskId = () => '', saveTaskId = () => {}, fkey = 'colab' } = {}) {
  const S = {
    loading: true,
    comps: [],
    task: null, // the gcloud install task
    startError: '',
    login: null, // ColabLogin
    loginBusy: false,
    loginError: '', // POST refused: its text
    loginDetail: '', // and its detail
    acting: false,
    installing: false, // the install request is in flight
  };
  let alive = true;
  let taskTimer = null;
  let loginTimer = null;
  const view = taskView({ components: () => S.comps, busy: () => S.acting, onAction: act });
  const unsubscribe = onComponentsEvent((ev) => {
    const tk = S.task && ((ev && ev.tasks) || []).find((x) => x.id === S.task.id);
    if (alive && tk && !S.acting) applyTask(tk);
  });
  const redraw = () => alive && rerender();

  const gcloud = () => S.comps.find((c) => c.id === 'gcloud') || null;
  // not in this platform's manifest: the server looks for gcloud on PATH
  const gcloudReady = () => {
    const g = gcloud();
    return !g || g.state === 'installed';
  };
  const loginActive = () => !!S.login && LOGIN_ACTIVE.has(S.login.state);

  /** Loads the component and the sign-in; install: start the gcloud install when it is missing. */
  async function start({ install: autoInstall = false } = {}) {
    S.loading = true;
    S.startError = '';
    const [list, login] = await Promise.all([obApi.components({ quiet: true }).catch((e) => ((S.startError = e.message), null)), obApi.colabLogin({ quiet: true }).catch(() => null)]);
    if (!alive) return;
    S.comps = (list && list.components) || [];
    S.login = login;
    if (!S.startError && !gcloudReady()) {
      const tasks = (list && list.tasks) || [];
      S.task = tasks.find((x) => x.id === taskId()) || tasks.filter((x) => taskActive(x) && (x.ids || []).includes('gcloud')).pop() || null;
      if (!S.task && autoInstall) await install();
    }
    S.loading = false;
    redraw();
    scheduleTask();
    scheduleLogin();
  }

  // ---- the gcloud tool ----

  async function install() {
    if (S.installing) return;
    S.installing = true;
    S.startError = '';
    redraw();
    try {
      const r = await obApi.install(['gcloud'], { quiet: true });
      S.task = r && r.task ? r.task : null;
    } catch (e) {
      if (e.status === 409) {
        const tasks = await obApi.tasks({ quiet: true }).catch(() => []);
        S.task = (tasks || []).filter((x) => taskActive(x) && (x.ids || []).includes('gcloud')).pop() || null;
        if (!S.task && e.code === 'components.up_to_date') await reloadComponents();
        else if (!S.task) S.startError = e.message;
      } else S.startError = e.message;
    }
    S.installing = false;
    if (S.task) saveTaskId(S.task.id);
    redraw();
    scheduleTask();
  }

  async function reloadComponents() {
    const list = await obApi.components({ quiet: true }).catch(() => null);
    if (!alive || !list) return;
    S.comps = list.components || S.comps;
  }

  function scheduleTask() {
    clearTimeout(taskTimer);
    if (!alive || !taskActive(S.task)) return;
    taskTimer = setTimeout(pollTask, POLL_MS);
  }

  async function pollTask() {
    if (!alive || !S.task) return;
    if (componentsEventFresh()) {
      scheduleTask(); // the event stream delivers the progress
      return;
    }
    let tk = null;
    try {
      const tasks = await obApi.tasks({ quiet: true });
      tk = (tasks || []).find((x) => x.id === S.task.id) || null;
    } catch {
      // the next tick
    }
    if (!alive) return;
    if (tk) applyTask(tk);
    else scheduleTask();
  }

  async function applyTask(tk) {
    const before = S.task.state;
    S.task = tk;
    if (tk.state !== before || !taskActive(tk)) {
      if (tk.state === 'done' && before !== 'done') await reloadComponents();
      redraw();
    } else view.update(tk);
    scheduleTask();
  }

  async function act(name) {
    if (!S.task || S.acting) return;
    if (name === 'change_source') {
      let src = null;
      try {
        src = await obApi.sources();
      } catch {
        return;
      }
      const next = await chooseSourcesDialog(src);
      if (!next || !alive) return;
      try {
        await obApi.saveSources(next);
      } catch {
        return;
      }
      name = 'retry';
    }
    S.acting = true;
    view.update(S.task);
    try {
      const r = await obApi.taskAction(S.task.id, name);
      if (r && r.task) S.task = r.task;
    } catch {
      // shown by the error handler
    }
    S.acting = false;
    redraw();
    scheduleTask();
  }

  // ---- the Google sign-in ----

  async function login() {
    if (S.loginBusy || loginActive()) return;
    S.loginBusy = true;
    S.loginError = '';
    S.loginDetail = '';
    redraw();
    try {
      S.login = await obApi.startColabLogin({ quiet: true });
    } catch (e) {
      if (e.status === 409 && e.code === 'colab.login_running') S.login = await obApi.colabLogin({ quiet: true }).catch(() => S.login);
      else {
        S.loginError = e.message;
        S.loginDetail = e.detail || '';
        if (e.code === 'colab.gcloud_missing') await reloadComponents();
      }
    }
    S.loginBusy = false;
    redraw();
    scheduleLogin();
  }

  async function cancel() {
    if (S.loginBusy) return;
    S.loginBusy = true;
    redraw();
    try {
      S.login = await obApi.cancelColabLogin();
    } catch {
      // shown by the error handler
    }
    S.loginBusy = false;
    redraw();
  }

  function scheduleLogin(ms = POLL_MS) {
    clearTimeout(loginTimer);
    if (!alive || !loginActive()) return;
    loginTimer = setTimeout(pollLogin, ms);
  }

  async function pollLogin() {
    if (!alive) return;
    const l = await obApi.colabLogin({ quiet: true }).catch(() => null);
    if (!alive) return;
    if (l) {
      const was = S.login ? S.login.state : '';
      const changed = !S.login || l.state !== S.login.state || l.url !== S.login.url;
      S.login = l;
      if (l.state === 'done' && was !== 'done') await onAuthorized();
      if (!alive) return;
      if (changed) redraw();
    }
    scheduleLogin();
  }

  // ---- render ----

  function linkFallback(url) {
    // the server passes only Google's sign-in page; never another scheme
    if (!url || !/^https:\/\//i.test(url)) return null;
    const d = desktop();
    const label = t('onboarding.remote.open_link');
    return d && typeof d.openExternal === 'function'
      ? h('button', { type: 'button', class: 'ob-linkbtn ob-remote-link', dataset: { fkey: `${fkey}:link` }, onclick: () => openExternal(url) }, label, ' ', icon('external-link', { size: 13 }))
      : h('a', { class: 'ob-ext ob-remote-link', href: url, target: '_blank', rel: 'noopener noreferrer', dataset: { fkey: `${fkey}:link` } }, label, icon('external-link', { size: 13 }));
  }

  // A failure: the server's sentence, and gcloud's own message behind "details".
  function failure(text, detail) {
    return h(
      'div',
      { class: 'ob-callout is-warn', role: 'alert' },
      icon('circle-alert', { size: 16 }),
      h(
        'div',
        { class: 'ob-grow' },
        h('p', null, text),
        detail ? h('details', { class: 'ob-detail' }, h('summary', { dataset: { fkey: `${fkey}:detail` } }, t('onboarding.remote.details')), h('pre', { class: 'ob-detail-text mono', lang: 'en' }, detail)) : null,
      ),
    );
  }

  /** The install's progress, or "installed" (null when not installed and no task). */
  function gcloudBlock() {
    if (S.task && S.task.state !== 'done') {
      view.update(S.task);
      return view.el;
    }
    if (gcloud() && gcloudReady()) return h('p', { class: 'ob-callout is-ok' }, icon('circle-check', { size: 16 }), h('span', null, t('onboarding.remote.gcloud_ready')));
    return null;
  }

  // R91 low-5: the sign-in's live regions (a failure's alert; waiting and
  // importing, status) stay the same nodes while they are shown and say
  // the same, so that a caller putting them in place (keepChildren) leaves
  // them where they are through its redraws and a screen reader does not
  // read them again (as R81 L8 for the install's view). One that says
  // something else, or is shown again after it was gone, is a new node.
  // Two places: the refused request's failure, the sign-in's state.
  const live = { error: null, state: null }; // place → {key, el}
  function kept(place, used, key, make) {
    used.add(place);
    const k = live[place];
    if (k && k.key === key) return k.el;
    const el = make();
    live[place] = { key, el };
    return el;
  }

  /** The sign-in's state: waiting (with the link), importing, failed, cancelled, or a hint. */
  function loginBlock({ idleHint = true } = {}) {
    const l = S.login || { state: 'idle' };
    const out = [];
    const used = new Set();
    const failureKey = (text, detail) => JSON.stringify(['failed', text, detail, t('onboarding.remote.details')]);
    if (S.loginError) out.push(kept('error', used, failureKey(S.loginError, S.loginDetail), () => failure(S.loginError, S.loginDetail)));
    if (l.state === 'waiting') {
      out.push(
        kept('state', used, JSON.stringify(['waiting', t('onboarding.remote.waiting'), l.url || '', t('onboarding.remote.open_link')]), () =>
          h(
            'div',
            { class: 'ob-callout is-info', role: 'status' },
            icon('loader-circle', { size: 16, class: 'spin' }),
            h('div', null, h('p', null, t('onboarding.remote.waiting')), l.url ? h('p', null, linkFallback(l.url)) : null),
          ),
        ),
      );
    } else if (l.state === 'importing') {
      out.push(kept('state', used, JSON.stringify(['importing', t('onboarding.remote.importing')]), () => h('p', { class: 'ob-callout is-info', role: 'status' }, icon('loader-circle', { size: 16, class: 'spin' }), h('span', null, t('onboarding.remote.importing')))));
    } else if (l.state === 'failed') {
      const text = U.errorInfoText(l.error, t('common.unknown_error'));
      const detail = (l.error && l.error.detail) || '';
      out.push(kept('state', used, failureKey(text, detail), () => failure(text, detail)));
    } else if (!S.loginError) {
      if (l.state === 'cancelled') out.push(h('p', { class: 'ob-hint' }, t('onboarding.remote.cancelled')));
      if (idleHint) out.push(h('p', { class: 'ob-hint' }, gcloudReady() ? t('onboarding.remote.login_hint') : t('onboarding.remote.wait_download')));
    }
    // not shown now: shown again later, it is new
    for (const place of Object.keys(live)) if (!used.has(place)) live[place] = null;
    return out;
  }

  /** "Sign in to Google" as {label, icon, disabled, onclick} (the same label after a failure: the error texts name it). */
  function loginAction() {
    return { label: t('onboarding.remote.login'), icon: 'log-in', disabled: S.loading || S.loginBusy || loginActive() || !gcloudReady(), onclick: login };
  }

  /** "Cancel sign-in" while one waits, else null. */
  function cancelButton(opts = {}) {
    if (!loginActive()) return null;
    return btn(t('onboarding.remote.cancel'), { kind: 'ghost', icon: 'ban', disabled: S.loginBusy, fkey: `${fkey}:cancel`, onclick: cancel, ...opts });
  }

  return {
    start,
    install,
    login,
    cancel,
    get loading() {
      return S.loading;
    },
    get startError() {
      return S.startError;
    },
    get installBusy() {
      return S.installing || taskActive(S.task);
    },
    gcloud,
    gcloudReady,
    loginActive,
    gcloudBlock,
    loginBlock,
    loginAction,
    cancelButton,
    // SSE "changed" colab / settings: follow the sign-in at once
    onChanged(ev) {
      if (!ev || (ev.kind !== 'colab' && ev.kind !== 'settings')) return;
      if (loginActive()) scheduleLogin(50);
      else if (ev.kind === 'colab') pollLogin();
    },
    destroy() {
      alive = false;
      clearTimeout(taskTimer);
      clearTimeout(loginTimer);
      unsubscribe();
    },
  };
}
