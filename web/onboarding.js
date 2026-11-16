// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The onboarding (SPEC-v3 V4; 3.1.0: .claude/v31-brief.md §1): one screen
// per step — a title, a sentence or two, one main action; "skip" and "back"
// are always there. The step and the choices are saved on the server (PUT
// /api/onboarding, kv onboarding.state) every time a step is entered, so
// closing the app and opening it again continues where the user was.
// Nothing is changed locally: every screen shows what the server returned.
//
// Two steps are not always there: "data folder" only in the desktop app
// when the folder can be moved (GET /api/app mode desktop and GET
// /api/app/data-dir can_move), "test the devices" not when the user chose
// the remote GPU. The step rail, the "step n of m" count, back, skip and
// resuming all go by the steps shown; a saved step that is not shown (an
// older state, another mode) continues with the next one that is.
// Older saved states (3.0, without asr_mode) continue as before: their step
// names all still exist, and no asr_mode means the local download.
//
// #/onboarding/{step}/{local|remote} opens a step directly with the
// transcription mode chosen (the "cannot transcribe yet" notice of
// asr_ready.js); the hash then goes back to #/onboarding.
//
// Steps 3-8 live in their own modules (onboarding_computer.js,
// onboarding_mode.js, onboarding_storage.js, onboarding_providers.js,
// onboarding_download.js, onboarding_remote.js); this file is the shell,
// the welcome (language, importing old data), privacy and first-course steps.

import { h, clear, btn, toast, preserveFocus, confirmDialog } from './dom.js';
import { icon } from './icons.js';
import { t, has, getLocale } from './i18n.js';
import { localeChoices, chooseLocale, langAttr } from './language.js';
import * as U from './util.js';
import { api } from './api.js';
import { obApi, setErrorHandler, desktop } from './onboarding_api.js';
import { computerStep } from './onboarding_computer.js';
import { providersStep } from './onboarding_providers.js';
import { downloadStep, selftestStep } from './onboarding_download.js';
import { asrModeStep } from './onboarding_mode.js';
import { storageStep } from './onboarding_storage.js';
import { remoteStep } from './onboarding_remote.js';
import { asrNotice } from './asr_ready.js';
import { keepChildren } from './components_ui.js';

export const STEPS = ['welcome', 'privacy', 'computer', 'asr_mode', 'storage', 'providers', 'download', 'selftest', 'first_course'];

const STEP_ICONS = {
  welcome: 'audio-lines',
  privacy: 'shield-check',
  computer: 'cpu',
  asr_mode: 'mic',
  storage: 'hard-drive',
  providers: 'key-round',
  download: 'download',
  selftest: 'gauge',
  first_course: 'graduation-cap',
};

const FACTORIES = {
  welcome: welcomeStep,
  privacy: privacyStep,
  computer: computerStep,
  asr_mode: asrModeStep,
  storage: storageStep,
  providers: providersStep,
  // the remote GPU: the gcloud tool and the Google sign-in instead
  download: (env) => (env.data.asr_mode === 'remote' ? remoteStep(env) : downloadStep(env)),
  selftest: selftestStep,
  first_course: firstCourseStep,
};

/**
 * Mounts the onboarding page (#/onboarding). ctx is app.js's context
 * (newCourse opens the A8 wizard); route may name a step and a mode
 * (#/onboarding/asr_mode/local). Returns the view object.
 */
export function mountOnboarding(main, ctx, route = {}) {
  let alive = true;
  const S = { step: null, done: false, data: {}, loading: true, loadError: null };
  let cur = null; // the current step object
  let saveChain = Promise.resolve();
  let saveFailed = false;
  // data shared by the steps (fetched once per page)
  const cache = {};

  setErrorHandler((err) => toast(err.message, err.status >= 500 || err.status === 0 ? 'error' : 'warn'));

  const page = h('div', { class: 'ob-page' });
  main.append(page);

  const env = {
    ctx,
    cache,
    get data() {
      return S.data;
    },
    alive: () => alive,
    rerender,
    save,
    saveAt: (step) => save(step),
    // true when this page resumed the onboarding after a data import (step 3 says so)
    imported: false,
    // the "data folder" step is shown (desktop app, the folder can be moved)
    storageShown: false,
    // the mode the "transcription mode" step starts with (a link of the notice)
    modeHint: '',
    next,
    back,
    goto: go,
    // back to the "transcription mode" step with mode chosen
    gotoMode(mode) {
      env.modeHint = mode;
      go('asr_mode');
    },
    finish,
    setData(patch) {
      Object.assign(S.data, patch);
      return save();
    },
  };

  load();

  async function load() {
    S.loading = true;
    S.loadError = null;
    render();
    let st;
    try {
      [st, env.storageShown] = await Promise.all([obApi.onboarding({ quiet: true }), storageShown()]);
    } catch (e) {
      if (!alive) return;
      S.loading = false;
      S.loadError = e.message;
      render();
      return;
    }
    if (!alive) return;
    S.loading = false;
    S.done = !!(st && st.done);
    S.data = st && st.data && typeof st.data === 'object' && !Array.isArray(st.data) ? { ...st.data } : {};
    if (!Array.isArray(S.data.skipped)) S.data.skipped = [];
    // done (or re-run from the settings): start from the beginning
    let step = !S.done && STEPS.includes(st && st.step) ? st.step : 'welcome';
    // after "import existing data" (step 1) and the restart that applies it:
    // continue with "your computer" — the imported library has courses but
    // this install has no components yet (kv onboarding.* is carried into
    // the imported database, relocate.carriedKVPrefixes)
    if (S.data.import_resume === true && !S.done && !(await importRunning())) {
      if (!alive) return;
      delete S.data.import_resume;
      step = 'computer';
      env.imported = true;
    }
    // a link to a step (the "cannot transcribe yet" notice): that step, that mode
    if (STEPS.includes(route.step)) {
      step = route.step;
      if (route.branch === 'local' || route.branch === 'remote') env.modeHint = route.branch;
      route = {};
      history.replaceState(null, '', '#/onboarding');
    }
    if (!alive) return;
    go(step, { initial: true });
  }

  // The "data folder" step: desktop app, and the folder is not given by
  // LECTUREFLOW_DATA_DIR (can_move). The status is kept for the step.
  async function storageShown() {
    try {
      const app = await obApi.app({ quiet: true });
      if (!app || app.mode !== 'desktop') return false;
      const dd = await obApi.dataDir({ quiet: true });
      cache.dataDir = dd;
      return !!dd && dd.can_move === true;
    } catch {
      return false;
    }
  }

  // The steps shown with the current choices.
  function shown(step) {
    if (step === 'storage') return env.storageShown;
    if (step === 'selftest') return S.data.asr_mode !== 'remote';
    return STEPS.includes(step);
  }

  function steps() {
    return STEPS.filter(shown);
  }

  // step, or the first shown step after it
  function resolve(step) {
    for (let i = Math.max(0, STEPS.indexOf(step)); i < STEPS.length; i++) if (shown(STEPS[i])) return STEPS[i];
    return 'first_course';
  }

  function next() {
    const i = STEPS.indexOf(S.step);
    go(i >= 0 && i + 1 < STEPS.length ? STEPS[i + 1] : 'first_course');
  }

  // An import of step 1 still copying (desktop mode): the page was mounted
  // again meanwhile, the restart has not come yet.
  async function importRunning() {
    if (!desktop()) return false;
    try {
      const st = await obApi.importStatus({ quiet: true });
      return !!st && (st.state === 'copying' || st.state === 'verifying');
    } catch {
      return false;
    }
  }

  // PUT /api/onboarding in order; a failure is said once, quietly (the next
  // step's save tries again). step: save another step than the current one.
  function save(step) {
    const body = { step: step || S.step || 'welcome', done: S.done, data: S.data };
    saveChain = saveChain
      .then(() => obApi.saveOnboarding(body, { quiet: true }))
      .then(
        () => {
          saveFailed = false;
        },
        (e) => {
          if (!saveFailed && alive) toast(t('onboarding.save_failed', { error: e.message }), 'warn');
          saveFailed = true;
        },
      );
    return saveChain;
  }

  function go(step, { initial = false } = {}) {
    if (!alive || !STEPS.includes(step)) return;
    step = resolve(step);
    if (cur && cur.destroy) cur.destroy();
    S.step = step;
    cur = FACTORIES[step](env);
    save(); // every step entered is saved (V4.1)
    render();
    const title = page.querySelector('.ob-title');
    if (title) title.focus({ preventScroll: true });
    window.scrollTo(0, 0);
  }

  function back() {
    const list = steps();
    const i = list.indexOf(S.step);
    if (i > 0) go(list[i - 1]);
  }

  async function skip() {
    if (S.step === 'first_course') {
      finish('later');
      return;
    }
    if (cur && cur.onSkip) await cur.onSkip();
    if (!alive) return;
    if (!S.data.skipped.includes(S.step)) S.data.skipped.push(S.step);
    env.next();
  }

  // The end: done = true (step 7's action, or "later"); then the workbench,
  // and for "course" the A8 wizard on top of it.
  async function finish(how) {
    if (cur && cur.destroy) cur.destroy();
    cur = null;
    S.done = true;
    S.step = 'done';
    if (how === 'later' && !S.data.skipped.includes('first_course')) S.data.skipped.push('first_course');
    await save();
    if (!alive) return;
    location.hash = '#/';
    if (how === 'course') setTimeout(() => ctx.newCourse(), 0);
  }

  // r7 (T22 G10): leaving before the end says where the guide goes on from
  // (the settings page's "continue setup", view_settings.js)
  function leave() {
    if (!S.done && !S.loading && !S.loadError) toast(t('onboarding.leave_toast'), 'info');
    location.hash = '#/';
  }

  // ---- rendering ----

  function render() {
    if (!alive) return;
    preserveFocus(() => {
      clear(page);
      page.append(topBar());
      if (S.loading) {
        page.append(h('div', { class: 'ob-card is-loading' }, h('div', { class: 'ob-loading' }, icon('loader-circle', { size: 18, class: 'spin' }), h('span', null, t('onboarding.loading')))));
        return;
      }
      if (S.loadError) {
        page.append(
          h(
            'div',
            { class: 'ob-card' },
            h('h1', { class: 'ob-title', tabindex: '-1' }, t('onboarding.load_failed_title')),
            h('p', { class: 'ob-desc' }, t('onboarding.load_failed', { error: S.loadError })),
            h('div', { class: 'ob-foot' }, h('span'), h('div', { class: 'ob-foot-r' }, btn(t('onboarding.later'), { kind: 'ghost', onclick: leave }), btn(t('common.retry'), { kind: 'primary', icon: 'rotate-cw', onclick: load }))),
          ),
        );
        return;
      }
      const c = card();
      keepChildren(c.body, c.kids);
      page.append(stepper(), c.el);
    });
  }

  // A step is entered with the focus on its title (go). Steps 3-7 draw
  // themselves again as their data comes (loading, then the content); the
  // focused title stays the same node through these redraws (T23 general
  // 16): the card's other parts are replaced around it, so the focus is not
  // lost to the page and a screen reader does not read the title again.
  // R81 L8: likewise a node the step keeps across its redraws (the
  // download's task view with its status line and alert, the Colab
  // sign-in's install): the card and its body stay, and the body's other
  // parts are replaced around that node, which is never taken out and put
  // back.
  function rerender() {
    if (!alive || !cur) return;
    const old = page.querySelector('.ob-card');
    if (!old) return render();
    preserveFocus(() => {
      const next = card();
      const sameStep = old.dataset.step === next.el.dataset.step;
      const keep = old.querySelector('.ob-title');
      const fresh = next.el.querySelector('.ob-title');
      const oldBody = old.querySelector('.ob-body');
      const titleStays = sameStep && !!keep && !!fresh && document.activeElement === keep;
      const bodyStays = sameStep && !!oldBody && next.kids.some((k) => k instanceof Node && k.parentNode === oldBody);
      if (!titleStays && !bodyStays) {
        keepChildren(next.body, next.kids);
        old.replaceWith(next.el);
        return;
      }
      if (titleStays && keep.textContent !== fresh.textContent) keep.textContent = fresh.textContent;
      keepChildren(bodyStays ? oldBody : next.body, next.kids);
      keepChildren(
        old,
        [...next.el.childNodes].map((c) => (titleStays && c === fresh ? keep : bodyStays && c === next.body ? oldBody : c)),
      );
    });
  }

  function topBar() {
    const list = steps();
    const i = list.indexOf(S.step);
    return h(
      'header',
      { class: 'ob-top' },
      h('span', { class: 'ob-brand' }, h('span', { class: 'brand-mark' }, icon('audio-lines', { size: 16, stroke: 2.4 })), h('span', { class: 'ob-brand-name' }, 'LectureFlow')),
      i >= 0 ? h('span', { class: 'ob-count' }, t('onboarding.step_of', { n: i + 1, total: list.length })) : null,
      h('button', { type: 'button', class: 'btn btn-ghost btn-sm ob-leave', dataset: { fkey: 'ob:leave' }, onclick: leave }, t('onboarding.leave')),
    );
  }

  function stepper() {
    const list = steps();
    const i = list.indexOf(S.step);
    return h(
      'nav',
      { class: 'ob-steps', 'aria-label': t('onboarding.steps_label') },
      h(
        'ol',
        null,
        list.map((s, k) =>
          h(
            'li',
            { class: 'ob-step' + (k === i ? ' is-current' : k < i ? ' is-past' : '') },
            h(
              'button',
              { type: 'button', class: 'ob-step-btn', 'aria-current': k === i ? 'step' : undefined, dataset: { fkey: 'ob:step:' + s }, onclick: () => k !== i && go(s) },
              h('span', { class: 'ob-step-dot' }, k < i ? icon('check', { size: 11, stroke: 3 }) : String(k + 1)),
              h('span', { class: 'ob-step-label' }, t(`onboarding.step.${s}`)),
            ),
          ),
        ),
      ),
    );
  }

  // r7 (T22 G13): what "skip" does on this step, next to a main action that
  // may look the same (its title, also the button's description): the
  // steps that only explain, or whose main action also just goes on, say
  // that skipping goes on the same way; the others what is left for later.
  // A step whose skip depends on its branch or state says it itself
  // (cur.skipTip: the remote branch's sign-in, the download; R75 M5).
  function skipTip(step) {
    if (cur && typeof cur.skipTip === 'function') return cur.skipTip();
    return has(`onboarding.skip_tip.${step}`) ? t(`onboarding.skip_tip.${step}`) : t('onboarding.skip_tip.same');
  }

  // The step's card → {el, body, kids}: its body is empty, kids are what
  // the step has in it (the caller puts them in: keepChildren).
  function card() {
    const step = S.step;
    const i = steps().indexOf(step);
    const primary = cur.primary ? cur.primary() : null;
    const secondary = cur.secondary ? cur.secondary() : [];
    const last = step === 'first_course';
    const titleText = cur.title();
    const descText = cur.desc();
    const kids = [cur.body()].flat(Infinity);
    const body = h('div', { class: 'ob-body' });
    const el = h(
      'section',
      { class: 'ob-card', dataset: { step }, 'aria-labelledby': 'ob-title' },
      h('div', { class: 'ob-icon', 'aria-hidden': 'true' }, icon(STEP_ICONS[step], { size: 22 })),
      h('h1', { class: 'ob-title', id: 'ob-title', tabindex: '-1' }, titleText),
      h('p', { class: 'ob-desc' }, descText),
      body,
      h(
        'footer',
        { class: 'ob-foot' },
        h('div', { class: 'ob-foot-l' }, i > 0 ? btn(t('onboarding.back'), { kind: 'ghost', icon: 'arrow-left', fkey: 'ob:back', onclick: back }) : null),
        h(
          'div',
          { class: 'ob-foot-r' },
          secondary,
          btn(last ? t('onboarding.later') : t('onboarding.skip'), { kind: 'quiet', fkey: 'ob:skip', title: last ? undefined : skipTip(step), onclick: skip }),
          primary ? btn(primary.label, { kind: 'primary', icon: primary.icon, disabled: primary.disabled, fkey: 'ob:primary', onclick: primary.onclick }) : null,
        ),
      ),
    );
    return { el, body, kids };
  }

  return {
    // SSE "changed" (colab, settings, devices): the current step may follow it
    onChanged(ev) {
      if (cur && cur.onChanged) cur.onChanged(ev);
    },
    destroy() {
      alive = false;
      if (cur && cur.destroy) cur.destroy();
      setErrorHandler(null);
    },
  };
}

// ---- step 1: welcome ----

function welcomeStep(env) {
  const imp = { state: 'idle', status: null, error: '', path: '' };
  let timer = null;
  const d = desktop();

  async function pickLanguage(code) {
    if (code === getLocale()) return;
    env.data.locale = code;
    await env.save();
    chooseLocale(code); // app.js re-mounts the page in the new language
  }

  async function startImport() {
    let path = null;
    try {
      path = await d.pickFolder();
    } catch {
      path = null;
    }
    if (!path || !env.alive()) return;
    const ok = await confirmDialog({ title: t('onboarding.import.confirm_title'), message: t('onboarding.import.confirm', { path }), okText: t('onboarding.import.confirm_ok') });
    if (!ok) api.forgetFolderPick(path); // declined: the pick is given up, not left to expire
    if (!ok || !env.alive()) return;
    imp.path = path;
    imp.state = 'starting';
    imp.error = '';
    env.rerender();
    // Saved before the copy starts: the import takes the onboarding state
    // of this install into the imported database, so after the restart the
    // onboarding opens again (app.js firstLaunch: data.imported) and
    // continues with "your computer" (data.import_resume).
    env.data.imported = true;
    env.data.import_resume = true;
    await env.saveAt('computer');
    try {
      imp.status = await obApi.startImport(path, { quiet: true });
      imp.state = 'copying';
    } catch (e) {
      imp.state = 'failed';
      imp.error = e.message;
      forgetImport();
      // refused: the service returned the pick's grant, and "import again"
      // asks for the folder again, so the pick is given up
      api.forgetFolderPick(path);
    }
    if (!env.alive()) return;
    env.rerender();
    if (imp.state === 'copying') poll();
  }

  // The import did not happen: back to the plain state of step 1.
  function forgetImport() {
    delete env.data.imported;
    delete env.data.import_resume;
    env.save();
  }

  function poll() {
    clearTimeout(timer);
    timer = setTimeout(async () => {
      if (!env.alive()) return;
      try {
        const st = await obApi.importStatus({ quiet: true });
        if (st) imp.status = st;
        const s = st && st.state;
        if (s === 'failed') {
          imp.state = 'failed';
          imp.error = st.error ? U.errorInfoText(st.error) : t('onboarding.import.failed_unknown');
          forgetImport();
        } else if (s === 'done' || s === 'restarting') imp.state = 'done';
      } catch {
        // the server stops to restart after copying: that is the end
        if (imp.state === 'copying' && imp.status && imp.status.copied_bytes >= imp.status.total_bytes && imp.status.total_bytes > 0) imp.state = 'done';
      }
      env.rerender();
      if (imp.state === 'copying') poll();
    }, 1000);
  }

  function importBlock() {
    if (!d || typeof d.pickFolder !== 'function') return null;
    if (imp.state === 'idle') return null;
    if (imp.state === 'failed') {
      return h(
        'div',
        { class: 'ob-callout is-warn', role: 'alert' },
        icon('circle-alert', { size: 16 }),
        h('div', null, h('p', null, t('onboarding.import.failed', { error: imp.error || t('onboarding.import.failed_unknown') })), h('p', { class: 'dim' }, t('onboarding.import.failed_todo'))),
      );
    }
    const st = imp.status || {};
    const total = st.total_bytes || 0;
    const copied = Math.min(st.copied_bytes || 0, total || Infinity);
    const done = imp.state === 'done';
    const pct = done ? 100 : total ? Math.round((copied / total) * 100) : 0;
    return h(
      'div',
      { class: 'ob-callout', 'aria-live': 'polite' },
      icon(done ? 'circle-check' : 'loader-circle', { size: 16, class: done ? '' : 'spin' }),
      h(
        'div',
        { class: 'ob-grow' },
        h('p', null, done ? t('onboarding.import.done') : st.state === 'verifying' ? t('app.data_dir.state.verifying') : t('onboarding.import.copying', { path: imp.path })),
        total ? h('p', { class: 'dim small' }, t('onboarding.import.progress', { copied: U.fmtBytes(copied), total: U.fmtBytes(total) })) : null,
        h('div', { class: 'cu-bar', role: 'progressbar', 'aria-label': t('onboarding.import.progress_label'), 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(pct) }, h('div', { class: 'cu-bar-fill', style: { width: `${pct}%` } })),
      ),
    );
  }

  return {
    title: () => t('onboarding.welcome.title'),
    desc: () => t('onboarding.welcome.desc'),
    body() {
      const cur = getLocale();
      // the import ends with a restart: the language waits for it
      const importing = imp.state === 'starting' || imp.state === 'copying' || imp.state === 'done';
      return [
        h(
          'fieldset',
          { class: 'ob-field' },
          h('legend', { class: 'ob-label' }, t('onboarding.welcome.language')),
          h(
            'div',
            { class: 'ob-langs', role: 'radiogroup' },
            localeChoices().map((l) =>
              h(
                'label',
                { class: 'ob-lang' + (l.code === cur ? ' is-on' : '') },
                h('input', { type: 'radio', name: 'ob-lang', value: l.code, checked: l.code === cur, disabled: importing, dataset: { fkey: 'ob:lang:' + l.code }, onchange: () => pickLanguage(l.code) }),
                h('span', { lang: langAttr(l.code) }, l.name),
              ),
            ),
          ),
          h('p', { class: 'ob-hint' }, t('onboarding.welcome.language_hint')),
        ),
        importBlock(),
      ];
    },
    secondary() {
      if (!d || typeof d.pickFolder !== 'function') return [];
      const busy = imp.state === 'starting' || imp.state === 'copying' || imp.state === 'done';
      return [btn(imp.state === 'failed' ? t('onboarding.import.again') : t('onboarding.import.button'), { kind: 'ghost', icon: 'hard-drive', disabled: busy, fkey: 'ob:import', title: t('onboarding.import.hint'), onclick: startImport })];
    },
    primary: () => ({ label: t('onboarding.welcome.start'), icon: 'arrow-right', onclick: env.next }),
    destroy() {
      clearTimeout(timer);
    },
  };
}

// ---- step 2: privacy ----

function privacyStep(env) {
  const points = [
    ['mic', 'onboarding.privacy.local'],
    ['server', 'onboarding.privacy.providers'],
    ['shield-check', 'onboarding.privacy.no_telemetry'],
    ['key-round', 'onboarding.privacy.keys'],
  ];
  return {
    title: () => t('onboarding.privacy.title'),
    desc: () => t('onboarding.privacy.desc'),
    body: () => [
      h(
        'ul',
        { class: 'ob-points' },
        points.map(([ic, key]) => h('li', null, icon(ic, { size: 16 }), h('span', null, t(key)))),
      ),
      h('a', { class: 'ob-link', href: '#/privacy', dataset: { fkey: 'ob:privacy' } }, t('onboarding.privacy.more'), icon('chevron-right', { size: 14 })),
    ],
    primary: () => ({ label: t('onboarding.privacy.ok'), icon: 'arrow-right', onclick: env.next }),
  };
}

// ---- step 7: the first course ----

function firstCourseStep(env) {
  const has = (env.ctx.courses() || []).length > 0;
  // 3.1.0 M2: nothing to transcribe with yet — said here, before the end
  const notice = asrNotice({ variant: 'onboarding', onLocal: () => env.gotoMode('local'), onRemote: () => env.gotoMode('remote') });
  return {
    title: () => t('onboarding.first_course.title'),
    desc: () => (has ? t('onboarding.first_course.desc_has') : t('onboarding.first_course.desc')),
    body: () => [
      notice.el,
      h(
        'ul',
        { class: 'ob-points' },
        [
          ['pencil', 'name'],
          ['file-text', 'syllabus'],
          ['upload', 'recordings'],
        ].map(([ic, k]) => h('li', null, icon(ic, { size: 16 }), h('span', null, t(`onboarding.first_course.point.${k}`)))),
      ),
    ],
    primary: () => ({ label: t('onboarding.first_course.add'), icon: 'plus', onclick: () => env.finish('course') }),
    onChanged: (ev) => notice.onChanged(ev),
    destroy: () => notice.destroy(),
  };
}
