// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Onboarding steps 5 and 6 (SPEC-v3 V4.2):
//
// 5, download: installs the components chosen in step 3 (POST
// /api/components/install) and shows the overall and per-item progress
// (downloaded / total, speed, time left). A failure says what happened
// (network, disk space, checksum) and offers "retry" and "change the
// download source"; a mirror that fell back to the default source is said
// too. The main action, "continue in the background", never waits for the
// download (V3.3: downloads run in the server, not in the page).
//
// 6, test the devices (optional): once a transcription back end is
// installed, the device self-test of SPEC-v2 I9 for each device. While any
// component task is still running the list is fetched again (every 3 s,
// and at once when the event stream says an item or a task changed state),
// and twice more after the last one ended: the server detects the devices
// again after each install, so a back end installed later (the integrated
// GPU or NPU after CUDA) shows its devices here without leaving the step. Without a
// lecture the server uses the bundled sample audio; when neither exists
// (selftest.no_audio) the test button is not offered, only "skip".
//
// Progress follows the SSE "components" event (api.js onComponentsEvent);
// while the stream is not delivering it, the task is polled every second.
//
// 3.1.0 (lead, after reproducing M2 on a Mac): the install no longer waits
// for step 5. startChosenInstall starts it in the background as soon as the
// data folder is confirmed (or, without that step, the transcription mode
// is chosen), so leaving the onboarding at the API-key step still installs
// everything; step 5 follows that task (data.task_id / data.gcloud_task_id)
// and starts one itself only when none was started (older saved states,
// skipped steps).

import { h, btn, toast } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { obApi } from './onboarding_api.js';
import { onComponentsEvent, componentsEventFresh } from './api.js';
import { taskView, taskActive, chooseSourcesDialog } from './components_ui.js';

const POLL_MS = 1000;
const BACKENDS = ['asr-cuda', 'asr-openvino', 'asr-mlx'];

// ---- starting the install early (3.1.0) ----

/**
 * Starts the downloads of the chosen transcription mode in the background:
 * local (or no mode) → data.components, remote → ["gcloud"]. Nothing when
 * there is nothing to install, everything is installed, or the task it
 * started before is still running. The task id is saved (data.task_id,
 * data.gcloud_task_id). Failures are quiet: step 5 starts the install again
 * and says what happened.
 */
export async function startChosenInstall(env) {
  const remote = env.data.asr_mode === 'remote';
  const ids = remote ? ['gcloud'] : Array.isArray(env.data.components) ? env.data.components : [];
  if (!ids.length) return;
  const key = remote ? 'gcloud_task_id' : 'task_id';
  const list = await obApi.components({ quiet: true }).catch(() => null);
  if (!list) return;
  const comps = list.components || [];
  const tasks = list.tasks || [];
  const known = new Set(comps.map((c) => c.id));
  const installed = new Set(comps.filter((c) => c.state === 'installed').map((c) => c.id));
  if (ids.every((id) => installed.has(id) || !known.has(id))) return;
  if (tasks.some((x) => x.id === env.data[key] && taskActive(x))) return;
  try {
    const r = await obApi.install(ids, { quiet: true });
    if (r && r.task) {
      env.data[key] = r.task.id;
      // r7 (T22 G13): "install as recommended" (step 3) only chose; this is
      // where the download starts, and it says so
      toast(t(remote ? 'onboarding.download.started_remote' : 'onboarding.download.started'), 'info');
    }
  } catch (e) {
    // already being installed by another task: follow that one
    const tk = e.status === 409 ? ((await obApi.tasks({ quiet: true }).catch(() => [])) || []).filter((x) => taskActive(x) && (x.ids || []).some((id) => ids.includes(id))).pop() : null;
    if (!tk) return;
    env.data[key] = tk.id;
  }
  await env.save();
}

// ---- step 5 ----

export function downloadStep(env) {
  const S = { loading: true, comps: [], task: null, sources: null, nothing: false, none: false, acting: false, startError: '' };
  let timer = null;
  let alive = true;
  // the task view says the task's state when it changes (no live figures:
  // components_ui.js taskView, T23 general 18)
  const view = taskView({ components: () => S.comps, busy: () => S.acting, onAction: act });
  const unsubscribe = onComponentsEvent((ev) => {
    const tk = S.task && ((ev && ev.tasks) || []).find((x) => x.id === S.task.id);
    if (alive && tk && !S.acting) apply(tk);
  });
  const redraw = () => env.rerender();

  start();

  async function start() {
    let list = null;
    try {
      list = await obApi.components({ quiet: true });
    } catch (e) {
      S.startError = e.message;
    }
    if (!alive) return;
    S.comps = (list && list.components) || [];
    env.cache.components = S.comps;
    const tasks = (list && list.tasks) || [];
    S.task = tasks.find((x) => x.id === env.data.task_id) || tasks.filter((x) => x.kind === 'install' && taskActive(x)).pop() || null;
    const chosen = Array.isArray(env.data.components) ? env.data.components : null;
    if (!S.task && !S.startError) {
      if (!chosen || !chosen.length) S.none = true;
      else {
        const installed = new Set(S.comps.filter((c) => c.state === 'installed').map((c) => c.id));
        if (chosen.every((id) => installed.has(id))) S.nothing = true;
        else await install(chosen);
      }
    }
    S.loading = false;
    if (!alive) return;
    redraw();
    schedule();
  }

  async function install(ids) {
    try {
      const r = await obApi.install(ids, { quiet: true });
      S.task = r && r.task ? r.task : null;
    } catch (e) {
      // a component is already being installed: follow that task
      if (e.status === 409) {
        const tasks = await obApi.tasks({ quiet: true }).catch(() => []);
        S.task = (tasks || []).filter((x) => taskActive(x)).pop() || null;
        if (!S.task && e.code === 'components.up_to_date') S.nothing = true;
        else if (!S.task) S.startError = e.message;
      } else S.startError = e.message;
    }
    if (S.task) {
      env.data.task_id = S.task.id;
      env.save();
    }
  }

  function schedule() {
    clearTimeout(timer);
    if (!alive || !taskActive(S.task)) return;
    timer = setTimeout(poll, POLL_MS);
  }

  // A newer task status (SSE or poll): redraw the step when its state
  // changed, else only the progress.
  function apply(tk) {
    const before = S.task.state;
    S.task = tk;
    if (tk.state !== before || !taskActive(tk)) redraw();
    else view.update(tk);
    if (tk.state === 'done' && before !== 'done') followUp(tk);
    schedule();
  }

  // The task followed here may have been another one (an install already
  // running when the step started: 409): once it is done, the chosen
  // components it did not cover are installed too, so the step only ends
  // when every chosen component is.
  async function followUp(done) {
    const chosen = Array.isArray(env.data.components) ? env.data.components : [];
    if (!chosen.length || S.acting) return;
    let list = null;
    try {
      list = await obApi.components({ quiet: true });
    } catch {
      return;
    }
    if (!alive || S.task !== done) return;
    S.comps = (list && list.components) || S.comps;
    env.cache.components = S.comps;
    const byId = new Map(S.comps.map((c) => [c.id, c]));
    const left = chosen.filter((id) => byId.has(id) && byId.get(id).state === 'available' && !(done.ids || []).includes(id));
    if (!left.length) return;
    S.acting = true;
    await install(left);
    S.acting = false;
    if (!alive) return;
    redraw();
    schedule();
  }

  async function poll() {
    if (!alive || !S.task) return;
    if (componentsEventFresh()) {
      schedule(); // the event stream delivers the progress
      return;
    }
    let tk = null;
    try {
      const tasks = await obApi.tasks({ quiet: true });
      tk = (tasks || []).find((x) => x.id === S.task.id) || null;
    } catch {
      // try again on the next tick
    }
    if (!alive) return;
    if (tk) apply(tk);
    else schedule();
  }

  async function act(name) {
    if (!S.task || S.acting) return;
    if (name === 'change_source') {
      let src = S.sources;
      if (!src) {
        try {
          src = S.sources = await obApi.sources();
        } catch {
          return;
        }
      }
      const next = await chooseSourcesDialog(src);
      if (!next || !alive) return;
      try {
        S.sources = await obApi.saveSources(next);
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
    if (!alive) return;
    redraw();
    schedule();
  }

  function body() {
    if (S.loading) return h('div', { class: 'ob-loading' }, icon('loader-circle', { size: 18, class: 'spin' }), h('span', null, t('onboarding.download.starting')));
    if (S.startError) {
      return h(
        'div',
        { class: 'ob-callout is-warn', role: 'alert' },
        icon('circle-alert', { size: 16 }),
        h('div', null, h('p', null, t('onboarding.download.start_failed', { error: S.startError })), h('p', { class: 'dim' }, t('onboarding.download.start_failed_todo'))),
      );
    }
    if (S.none) return h('p', { class: 'ob-callout is-info' }, icon('info', { size: 16 }), h('span', null, t('onboarding.download.none')));
    if (S.nothing) return h('p', { class: 'ob-callout is-ok' }, icon('circle-check', { size: 16 }), h('span', null, t('onboarding.download.nothing')));
    view.update(S.task);
    return [view.el, taskActive(S.task) ? h('p', { class: 'ob-hint' }, t('onboarding.download.background_note')) : null];
  }

  return {
    title: () => t('onboarding.download.title'),
    // r7 (T22 G13): once everything is installed, no "runs in the background" any more
    desc: () => (S.nothing || (S.task && S.task.state === 'done') ? t('onboarding.download.desc_done') : t('onboarding.download.desc')),
    // R75 M5: only a download that runs goes on in the background; else skipping is "continue"
    skipTip: () => t(taskActive(S.task) ? 'onboarding.skip_tip.download' : 'onboarding.skip_tip.continue'),
    body,
    secondary() {
      if (S.none) return [btn(t('onboarding.download.choose'), { kind: 'ghost', icon: 'cpu', fkey: 'ob:dl:choose', onclick: () => env.goto('computer') })];
      if (S.startError) return [btn(t('common.retry'), { kind: 'ghost', icon: 'rotate-cw', fkey: 'ob:dl:restart', onclick: () => ((S.loading = true), (S.startError = ''), env.rerender(), start()) })];
      return [];
    },
    primary() {
      const running = taskActive(S.task);
      return {
        label: running ? t('onboarding.download.background') : t('onboarding.continue'),
        icon: 'arrow-right',
        disabled: S.loading,
        onclick: env.next,
      };
    },
    destroy() {
      alive = false;
      clearTimeout(timer);
      unsubscribe();
    },
  };
}

// ---- step 6 ----

const TEST_ACTIVE = new Set(['waiting', 'running']);

/**
 * r7 (T22 G9): the self-test's recommendation in plain words — how long an
 * hour of recording takes with it on this computer (its RTF × 60 minutes),
 * instead of "RTF ≤ 0.5". Without the recommended run's speed, the settings
 * page's sentence (selftest.pick_*).
 */
export function pickText(b, combo) {
  const fastest = b.pick.rule === 'lowest_rtf';
  const run = (b.results || []).find((r) => r && r.ok && r.model === b.pick.model && r.precision === b.pick.precision);
  if (!run || !(run.rtf > 0)) return t(fastest ? 'selftest.pick_fastest' : 'selftest.pick_best', { combo });
  const n = U.count(Math.max(1, Math.round(run.rtf * 60)));
  return t(fastest ? 'onboarding.selftest.pick_fastest' : 'onboarding.selftest.pick_best', { combo, n });
}

export function selftestStep(env) {
  const S = { loading: true, ready: false, downloading: false, devices: [], busy: new Set(), error: '' };
  let timer = null; // the self-tests' progress
  let reloadTimer = null; // the list again while components are installing
  let alive = true;
  let loading = false;
  let settle = 0; // reloads still to do after the last install ended
  let compSig = '';
  const testing = (d) => !!d.selftest && TEST_ACTIVE.has(d.selftest.state);
  // A component installed (or a task ended) on the event stream: the server
  // detects the devices again after an install, so the list is fetched again.
  const unsubscribe = onComponentsEvent((ev) => {
    const sig = tasksSig((ev && ev.tasks) || []);
    if (!alive || sig === compSig) return;
    compSig = sig;
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(() => load({ quiet: true }), 500);
  });

  load();

  // The states of the tasks and of their items: what an install changes.
  function tasksSig(tasks) {
    return tasks.map((x) => `${x.id}:${x.state}:${(x.items || []).map((i) => i.state).join(',')}`).join(';');
  }

  // Loads the components and the devices. quiet: a reload while the page is
  // shown (no spinner; the card is redrawn only when something changed).
  async function load({ quiet = false } = {}) {
    if (loading) return;
    loading = true;
    clearTimeout(reloadTimer);
    if (!quiet) S.loading = true;
    let failed = '';
    const [list, devices] = await Promise.all([obApi.components({ quiet: true }).catch(() => null), obApi.devices({ quiet: true }).catch((e) => ((failed = e.message), null))]);
    loading = false;
    if (!alive) return;
    const before = quiet ? viewSig() : '';
    const comps = (list && list.components) || [];
    const tasks = (list && list.tasks) || [];
    if (list) {
      compSig = tasksSig(tasks);
      const downloading = tasks.some((x) => taskActive(x));
      // the server detects the devices again once an install ends: look a
      // couple more times after the last task finished
      if (S.downloading && !downloading) settle = 2;
      else if (!downloading && settle > 0) settle--;
      S.downloading = downloading;
      S.ready = comps.some((c) => BACKENDS.includes(c.id) && c.state === 'installed');
    }
    if (devices) {
      S.devices = devices.filter((d) => d.present && d.enabled);
      S.error = '';
    } else if (!quiet || !S.devices.length) S.error = failed;
    S.loading = false;
    if (!quiet || viewSig() !== before) env.rerender();
    schedule();
    scheduleReload();
  }

  function viewSig() {
    return JSON.stringify([S.ready, S.downloading, S.error, S.devices]);
  }

  // tests: follow them every second
  function schedule() {
    clearTimeout(timer);
    if (!alive) return;
    if (S.devices.some(testing)) timer = setTimeout(poll, POLL_MS);
  }

  // components still installing (each new back end can bring new devices),
  // or just finished: look again in a few seconds — until every task ended
  function scheduleReload() {
    clearTimeout(reloadTimer);
    if (!alive) return;
    if (S.downloading || settle > 0) reloadTimer = setTimeout(() => load({ quiet: true }), 3000);
  }

  async function poll() {
    await Promise.all(
      S.devices.filter(testing).map(async (d) => {
        try {
          const st = await obApi.selfTest(d.id, { quiet: true });
          if (!st) return;
          d.selftest = st;
          if (!TEST_ACTIVE.has(st.state) && st.result && (st.result.results || []).length) d.benchmark = st.result;
        } catch {
          // next tick
        }
      }),
    );
    if (!alive) return;
    env.rerender();
    schedule();
  }

  async function startTest(d) {
    S.busy.add(d.id);
    env.rerender();
    try {
      const st = await obApi.startSelfTest(d.id);
      if (st) d.selftest = st;
    } catch {
      // shown by the error handler (selftest.running, selftest.no_audio)
    }
    S.busy.delete(d.id);
    if (!alive) return;
    env.rerender();
    schedule();
  }

  async function cancelTest(d) {
    S.busy.add(d.id);
    try {
      const st = await obApi.cancelSelfTest(d.id);
      if (st) d.selftest = st;
    } catch {
      // shown by the error handler
    }
    S.busy.delete(d.id);
    if (alive) env.rerender();
  }

  async function apply(d, m, p) {
    S.busy.add(d.id);
    env.rerender();
    try {
      const v = await obApi.applySelfTest(d.id, m, p);
      if (v) Object.assign(d, v);
    } catch {
      // shown by the error handler
    }
    S.busy.delete(d.id);
    if (alive) env.rerender();
  }

  function deviceRow(d) {
    const st = d.selftest || { state: 'idle' };
    const blocked = d.selftest_blocked;
    const noAudio = blocked && blocked.code === 'selftest.no_audio';
    const busy = S.busy.has(d.id);
    const parts = [];
    if (testing(d)) {
      let phase = st.state === 'waiting' ? t('selftest.waiting') : ['clip', 'preparing_model', 'compiling_model', 'transcribing'].includes(st.phase) ? t(`selftest.phase.${st.phase}`) : t('selftest.phase.loading');
      if (st.state === 'running' && st.candidate) phase = t('selftest.running', { step: st.step, steps: st.steps, combo: U.comboText(st.candidate.model, st.candidate.precision) });
      const frac = st.steps ? (Math.max(0, st.step - 1) + (st.progress || 0)) / st.steps : 0;
      const pct = Math.round(frac * 100);
      parts.push(
        h('p', { class: 'ob-dev-phase' }, phase),
        h('div', { class: 'cu-bar', role: 'progressbar', 'aria-label': t('selftest.progress_label'), 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(pct) }, h('div', { class: 'cu-bar-fill', style: { width: `${pct}%` } })),
      );
    } else {
      const b = d.benchmark;
      if (b && b.pick) {
        const combo = U.comboText(b.pick.model, b.pick.precision);
        const inUse = b.pick.model === d.model && b.pick.precision === d.precision;
        parts.push(
          h(
            'p',
            { class: 'ob-dev-pick' },
            icon(inUse ? 'circle-check' : 'sparkles', { size: 14 }),
            h('span', null, pickText(b, combo)),
            inUse ? h('span', { class: 'dim' }, t('selftest.pick_in_use')) : btn(t('selftest.apply_pick'), { size: 'sm', kind: 'primary', disabled: busy, fkey: `ob:dev:${d.id}:apply`, onclick: () => apply(d, b.pick.model, b.pick.precision) }),
          ),
        );
        if (b.audio && b.audio.source === 'bundled') parts.push(h('p', { class: 'ob-hint' }, t('onboarding.selftest.sample_used')));
      } else if (b && b.error) parts.push(h('p', { class: 'ob-test is-fail' }, icon('circle-alert', { size: 14 }), h('span', null, U.errorInfoText(b.error))));
      if (blocked && !noAudio) parts.push(h('p', { class: 'ob-hint' }, U.errorInfoText(blocked)));
    }
    const action = testing(d)
      ? btn(t('selftest.cancel'), { size: 'sm', kind: 'ghost', icon: 'ban', disabled: busy, fkey: `ob:dev:${d.id}:cancel`, onclick: () => cancelTest(d) })
      : noAudio
        ? null
        : btn(d.benchmark ? t('selftest.again') : t('selftest.start'), { size: 'sm', icon: 'gauge', disabled: busy || !!blocked, fkey: `ob:dev:${d.id}:test`, onclick: () => startTest(d) });
    return h(
      'li',
      { class: 'ob-dev' },
      h('div', { class: 'ob-dev-head' }, h('span', { class: 'ob-dev-name' }, d.label), h('span', { class: 'ob-dev-kind' }, [U.deviceKindName(d.kind), U.backendName(d)].filter(Boolean).join(' · ')), h('span', { class: 'ob-grow' }), action),
      parts,
    );
  }

  function body() {
    if (S.loading) return h('div', { class: 'ob-loading' }, icon('loader-circle', { size: 18, class: 'spin' }), h('span', null, t('onboarding.loading')));
    if (!S.ready) return h('p', { class: 'ob-callout is-info' }, icon('info', { size: 16 }), h('span', null, S.downloading ? t('onboarding.selftest.wait_download') : t('onboarding.selftest.no_components')));
    if (S.error) return h('p', { class: 'ob-callout is-warn' }, icon('circle-alert', { size: 16 }), h('span', null, t('onboarding.selftest.load_failed', { error: S.error })));
    if (!S.devices.length) return h('p', { class: 'ob-callout is-info' }, icon('info', { size: 16 }), h('span', null, t('onboarding.selftest.no_devices')));
    const allNoAudio = S.devices.every((d) => d.selftest_blocked && d.selftest_blocked.code === 'selftest.no_audio' && !d.benchmark);
    return [
      allNoAudio ? h('p', { class: 'ob-callout is-info' }, icon('info', { size: 16 }), h('span', null, t('onboarding.selftest.no_audio'))) : null,
      h('ul', { class: 'ob-devs' }, S.devices.map(deviceRow)),
      h('a', { class: 'ob-link', href: '#/settings/asr', dataset: { fkey: 'ob:dev:settings' } }, t('onboarding.selftest.details'), icon('chevron-right', { size: 14 })),
    ];
  }

  return {
    title: () => t('onboarding.selftest.title'),
    desc: () => t('onboarding.selftest.desc'),
    body,
    primary: () => ({ label: t('onboarding.continue'), icon: 'arrow-right', disabled: S.loading, onclick: env.next }),
    destroy() {
      alive = false;
      clearTimeout(timer);
      clearTimeout(reloadTimer);
      unsubscribe();
    },
  };
}
