// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// r7 (user simulations T22-T26): the setup guide, the settings page and the
// diagnostics page, run on a DOM stand-in whose focus behaves as a
// browser's where it matters here: a removed focused element, or one made
// disabled, leaves the focus on the page (body); a disabled or removed
// element takes no focus. web/r7_settings_test.go starts it with the web
// directory as a file: URL. Nodes are compared by identity (assert.ok(a ===
// b)), never with assert.equal (testdata/README.md).
//   - T22 S3: the guide's "your computer" step with too little disk space:
//     with the guide's own "data folder" step to come, the main button stays
//     usable and the text points to that step, not out of the guide;
//   - T23 general 16: entering a step that loads first (3-7) leaves the
//     focus on the new step's title, through the redraws of its loading;
//   - T23 serious 3: "save" of a settings block keeps the focus after the
//     save (aria-disabled, not disabled).

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

// ---- DOM stand-in ----

const MAX_DEPTH = 10000;
function up(n, fn) {
  for (let i = 0; n; n = n.parentNode) {
    if (++i > MAX_DEPTH) throw new Error('stand-in DOM: parent chain too long (a cycle?)');
    if (fn(n)) return n;
  }
  return null;
}

class Node {
  constructor() {
    this.parentNode = null;
    this.listeners = {};
  }
  get isConnected() {
    return !!up(this, (n) => n === root);
  }
  // a removed focused element (or one holding the focus) leaves the focus on the page
  remove() {
    if (!this.parentNode) return;
    if (this.contains && this.contains(doc.activeElement)) doc.activeElement = body;
    const c = this.parentNode.children;
    c.splice(c.indexOf(this), 1);
    this.parentNode = null;
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  removeEventListener(type, fn) {
    const l = this.listeners[type] || [];
    if (l.includes(fn)) l.splice(l.indexOf(fn), 1);
  }
  dispatchEvent(ev) {
    ev.target ??= this;
    let hops = 0;
    for (let n = this; n && !ev.stopped; n = n === root ? doc : n.parentNode) {
      if (++hops > MAX_DEPTH) throw new Error('stand-in DOM: event path too long (a cycle?)');
      ev.currentTarget = n;
      for (const fn of [...(n.listeners[ev.type] || [])]) fn(ev);
      if (!ev.bubbles) break;
    }
    return !ev.defaultPrevented;
  }
}
class Text extends Node {
  constructor(s) {
    super();
    this.data = String(s);
    this.children = [];
  }
  get textContent() {
    return this.data;
  }
}
class El extends Node {
  constructor(tag) {
    super();
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.attrs = {};
    this.dataset = {};
    this.style = {};
    this.className = '';
    const self = this;
    this.classList = {
      toggle(c, on) {
        const set = new Set(self.className.split(/\s+/).filter(Boolean));
        if (on === undefined ? !set.has(c) : on) set.add(c);
        else set.delete(c);
        self.className = [...set].join(' ');
      },
      add(c) {
        this.toggle(c, true);
      },
      remove(c) {
        this.toggle(c, false);
      },
      contains(c) {
        return self.className.split(/\s+/).includes(c);
      },
    };
  }
  set textContent(v) {
    this.replaceChildren(new Text(v));
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join('');
  }
  get firstChild() {
    return this.children[0] || null;
  }
  get childNodes() {
    return this.children;
  }
  // disabled reflects the attribute; a focused control made disabled loses the focus (focus fixup)
  get disabled() {
    return 'disabled' in this.attrs;
  }
  set disabled(v) {
    if (v) {
      this.attrs.disabled = '';
      if (doc.activeElement === this) doc.activeElement = body;
    } else delete this.attrs.disabled;
  }
  get hidden() {
    return 'hidden' in this.attrs;
  }
  set hidden(v) {
    if (v) this.attrs.hidden = '';
    else delete this.attrs.hidden;
  }
  setAttribute(k, v) {
    if (k === 'class') this.className = String(v);
    else this.attrs[k] = String(v);
    if (k === 'disabled' && doc.activeElement === this) doc.activeElement = body;
  }
  getAttribute(k) {
    return k in this.attrs ? this.attrs[k] : null;
  }
  removeAttribute(k) {
    delete this.attrs[k];
  }
  append(...cs) {
    for (const c0 of cs) {
      const c = c0 instanceof Node ? c0 : new Text(c0);
      c.remove();
      c.parentNode = this;
      this.children.push(c);
    }
  }
  insertBefore(n, ref) {
    if (!ref) return this.append(n);
    n.remove();
    n.parentNode = this;
    this.children.splice(this.children.indexOf(ref), 0, n);
    return n;
  }
  before(...ns) {
    for (const n0 of ns) this.parentNode.insertBefore(n0 instanceof Node ? n0 : new Text(n0), this);
  }
  after(...ns) {
    let ref = this;
    for (const n0 of ns) {
      const n = n0 instanceof Node ? n0 : new Text(n0);
      const p = ref.parentNode;
      const next = p.children[p.children.indexOf(ref) + 1] || null;
      p.insertBefore(n, next);
      ref = n;
    }
  }
  removeChild(c) {
    c.remove();
    return c;
  }
  replaceChildren(...cs) {
    for (const c of [...this.children]) c.remove();
    this.append(...cs);
  }
  replaceWith(n) {
    const p = this.parentNode;
    if (!p) return;
    const i = p.children.indexOf(this);
    this.remove();
    n.remove();
    n.parentNode = p;
    p.children.splice(i, 0, n);
  }
  contains(n) {
    return !!up(n, (x) => x === this);
  }
  closest(sel) {
    return up(this, (n) => n instanceof El && n.matches(sel));
  }
  focus() {
    const prev = doc.activeElement;
    if (prev === this) return;
    // a disabled or removed element takes no focus
    if (this.disabled || !this.isConnected) return;
    doc.activeElement = this;
    if (prev) prev.dispatchEvent(event('focusout', { relatedTarget: this }));
    this.dispatchEvent(event('focusin', { relatedTarget: prev }));
  }
  // a disabled button takes no click, as in a browser
  click() {
    if (this.disabled) return;
    this.dispatchEvent(event('click'));
  }
  blur() {
    if (doc.activeElement !== this) return;
    doc.activeElement = body;
    this.dispatchEvent(event('focusout', { relatedTarget: null }));
  }
  showModal() {
    this.open = true;
    dialogs.push(this);
  }
  close() {
    this.open = false;
  }
  scrollIntoView() {}
  getBoundingClientRect() {
    return { top: 0, bottom: 0, left: 0, right: 0 };
  }
  matches(sel) {
    if (sel === ':focus-visible') return true; // a keyboard user
    const m = /^([a-z0-9]*)((?:\.[\w-]+)*)(?:\[([\w-]+)(?:="([^"]*)")?\])?$/.exec(sel);
    if (!m) throw new Error('selector not supported here: ' + sel);
    const [, tag, classes, attr, val] = m;
    if (tag && this.tagName !== tag.toUpperCase()) return false;
    const own = this.className.split(/\s+/);
    for (const c of classes.split('.').filter(Boolean)) if (!own.includes(c)) return false;
    if (attr) {
      const v = attr.startsWith('data-') ? this.dataset[attr.slice(5).replace(/-(\w)/g, (_, c) => c.toUpperCase())] : this.getAttribute(attr);
      if (v == null || (val !== undefined && v !== val)) return false;
    }
    return true;
  }
  querySelectorAll(sel) {
    const out = [];
    const walk = (n) => {
      for (const c of n.children) {
        if (c instanceof El) {
          if (c.matches(sel)) out.push(c);
          walk(c);
        }
      }
    };
    walk(this);
    return out;
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
}

function event(type, o = {}) {
  return {
    type,
    bubbles: true,
    defaultPrevented: false,
    stopped: false,
    preventDefault() {
      this.defaultPrevented = true;
    },
    stopPropagation() {
      this.stopped = true;
    },
    ...o,
  };
}

const dialogs = [];
const store = { getItem: () => null, setItem() {}, removeItem() {} };
const root = new El('html');
const body = new El('body');
const toasts = new El('div'); // where dom.js toast() shows a message
root.append(body);
body.append(toasts);
const doc = {
  documentElement: root,
  body,
  activeElement: body,
  listeners: {},
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  },
  removeEventListener(type, fn) {
    const l = this.listeners[type] || [];
    if (l.includes(fn)) l.splice(l.indexOf(fn), 1);
  },
  querySelector: (sel) => root.querySelector(sel),
  querySelectorAll: (sel) => root.querySelectorAll(sel),
  getElementById: (id) => (id === 'toasts' ? toasts : null),
  createElement: (t) => {
    const e = new El(t);
    if (/^(input|textarea|select)$/i.test(t)) e.value = '';
    return e;
  },
  createElementNS: (_, t) => new El(t),
  createTextNode: (s) => new Text(s),
  visibilityState: 'visible',
};

Object.assign(globalThis, {
  Node,
  location: { search: '', hash: '#/onboarding', pathname: '/', origin: 'http://127.0.0.1', reload() {} },
  history: { replaceState() {} },
  localStorage: store,
  sessionStorage: store,
  document: doc,
  addEventListener() {},
  removeEventListener() {},
  dispatchEvent: () => true,
  scrollTo() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  innerHeight: 900,
  CSS: { escape: (s) => String(s) },
});
globalThis.window = globalThis;
globalThis.requestAnimationFrame = (fn) => setTimeout(fn, 0);

process.on('unhandledRejection', (e) => {
  console.error('unhandled rejection:', e && e.message ? e.message : String(e));
  process.exit(1);
});

const base = process.argv[2];

// ---- fetch: the catalogs from the web directory; every other request is
// recorded in `calls` and answered by routes['METHOD /path'] (a body, or a
// function of the request's body giving {status, body}); 404 otherwise ----

const calls = [];
let routes = {};
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const text = await readFile(new URL(u, base), 'utf8');
    return { ok: true, status: 200, json: async () => JSON.parse(text), text: async () => text };
  }
  const method = init.method || 'GET';
  const path = u.split('?')[0];
  const reqBody = typeof init.body === 'string' && init.body ? JSON.parse(init.body) : undefined;
  calls.push({ method, path, body: reqBody });
  await new Promise((r) => setTimeout(r, 2));
  let a = routes[`${method} ${path}`];
  if (typeof a === 'function') a = await a(reqBody); // a route may answer later (a held request)
  else if (a !== undefined) a = { status: 200, body: a };
  else a = { status: 404, body: { error: { code: 'api.not_found', params: { method, path } } } };
  const text = a.body == null ? '' : JSON.stringify(a.body);
  return { ok: a.status < 300, status: a.status, headers: { get: () => null }, text: async () => text, json: async () => JSON.parse(text), blob: async () => text };
};
const settle = (ms = 60) => new Promise((r) => setTimeout(r, ms));
const sent = (method, path) => calls.filter((c) => c.method === method && c.path === path);

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });

const fk = (k) => doc.querySelector(`[data-fkey="${k}"]`);
const toastTexts = () => toasts.children.map((x) => x.dataset.msg);
const clearToasts = () => toasts.replaceChildren();

// the hardware, components and plans of a guide with a 2 GB data disk
const HW = {
  hw: { ram_mb: 16384, disk_free_mb: 2048 },
  recommendation: { components: ['core', 'asr-cuda', 'model-large-v3'], reasons: [{ code: 'hw.reason.cpu_only', params: {} }], notes: [] },
};
const COMPS = { components: [{ id: 'asr-cuda', state: 'available' }, { id: 'model-large-v3', state: 'available' }], tasks: [] };
const SOURCES = { pypi: { mode: 'default' }, models: { mode: 'default' }, options: {}, suggest_mirror: false };
const NO_ROOM = { ids: ['asr-cuda', 'model-large-v3'], download_bytes: 6.3e9, install_bytes: 8.68e9, free_bytes: 2e9, enough_space: false, need_bytes: 14.98e9 };
const guideRoutes = () => ({
  'GET /api/hardware': HW,
  'GET /api/components': COMPS,
  'GET /api/components/sources': SOURCES,
  'POST /api/components/plan': NO_ROOM,
  'GET /api/components/presets': { tiers: [], providers: [] },
  'GET /api/onboarding/env-keys': [],
  'GET /api/providers': [],
  'GET /api/asr/devices': [],
  'PUT /api/onboarding': (b) => ({ status: 200, body: b }),
});

// ---- T22 S3: "your computer" with too little disk space ----
{
  const OC = await import(new URL('onboarding_computer.js', base).href);
  routes = guideRoutes();
  for (const storageShown of [true, false]) {
    const page = new El('div');
    body.append(page);
    let step = null;
    const env = {
      cache: {},
      data: {},
      storageShown,
      alive: () => true,
      rerender() {
        if (step) page.replaceChildren(D.h('div', null, step.body()));
      },
      save: async () => {},
      setData: async (patch) => void Object.assign(env.data, patch),
      next() {},
    };
    step = OC.computerStep(env);
    await settle(150);
    env.rerender();
    const what = storageShown ? 'with the data folder step' : 'without the data folder step';
    const callout = page.querySelectorAll('div.ob-callout').find((c) => c.textContent.includes(t('compui.plan.no_space', { need: '@', free: '@' }).split('@')[0]));
    assert.ok(callout, `${what}: the space problem is said`);
    const toSettings = callout.querySelectorAll('a').filter((a) => a.getAttribute('href') === '#/settings');
    const p = step.primary();
    if (storageShown) {
      assert.equal(p.disabled, false, `${what}: "install as recommended" stays usable`);
      assert.equal(toSettings.length, 0, `${what}: no way out of the guide to the settings`);
      assert.ok(callout.textContent.includes(t('onboarding.computer.space_todo_storage')), `${what}: points to the data folder step: ${callout.textContent}`);
      assert.ok(t('onboarding.computer.space_todo_storage').includes(t('onboarding.step.storage')), 'the text names the step as the step rail does');
    } else {
      assert.equal(p.disabled, true, `${what}: the main button is off, as before`);
      assert.equal(toSettings.length, 1, `${what}: the settings are the way out`);
    }
    page.remove();
  }
}

const settingsJSON = JSON.parse(await readFile(new URL('../testdata/api/v11_settings.json', base), 'utf8'));
const S = await import(new URL('view_settings.js', base).href);
const settingsCtx = { resources: () => null, system: () => null, subscribe: () => () => {} };

// ---- T22 G10: leaving the guide early says where it goes on; the
// settings page offers "continue setup" while it is left unfinished ----
{
  const O = await import(new URL('onboarding.js', base).href);
  routes = { ...guideRoutes(), 'GET /api/onboarding': { step: 'computer', done: false, data: {} } };
  const main = new El('main');
  body.append(main);
  const view = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
  await settle(200);
  clearToasts();
  fk('ob:leave').click();
  assert.deepEqual(toastTexts(), [t('onboarding.leave_toast')], 'leaving early: where to continue');
  assert.ok(t('onboarding.leave_toast').includes(t('settings.alert.guide_continue')), 'the toast names the button as the settings page shows it');
  assert.equal(location.hash, '#/', 'and the workbench opens');
  view.destroy();
  main.remove();
  clearToasts();
  // R75 (mutation M2 survived): a guide gone through to its end (run again
  // from the settings, or opened by a link) says nothing on leaving
  routes = { ...guideRoutes(), 'GET /api/onboarding': { step: 'done', done: true, data: {} } };
  {
    const m2 = new El('main');
    body.append(m2);
    const v2 = O.mountOnboarding(m2, { courses: () => [], newCourse() {} });
    await settle(200);
    fk('ob:leave').click();
    assert.deepEqual(toastTexts(), [], 'a finished guide: nothing to continue, nothing said');
    v2.destroy();
    m2.remove();
  }

  for (const [what, st, shown] of [
    ['left at a step', { step: 'providers', done: false, data: { skipped: ['computer'] } }, true],
    ['left on the first step', { step: 'welcome', done: false, data: { skipped: [] } }, true],
    ['never started (the default state)', { step: 'welcome', done: false, data: {} }, false],
    ['done', { step: 'done', done: true, data: {} }, false],
  ]) {
    routes = { 'GET /api/settings': settingsJSON, 'GET /api/onboarding': st };
    const page = new El('main');
    body.append(page);
    const sv = S.mountSettings(page, settingsCtx, null);
    await settle(250);
    const b = fk('set:alert:guide');
    assert.equal(!!b, shown, `${what}: "continue setup" ${shown ? 'is' : 'is not'} offered`);
    if (b) {
      assert.ok(b.closest('div.banner').textContent.includes(t('settings.alert.guide_left')), `${what}: with its reason`);
      location.hash = '#/settings';
      b.click();
      assert.equal(location.hash, '#/onboarding', `${what}: it opens the guide (where it was left)`);
    }
    sv.destroy();
    page.remove();
  }
}

// ---- T22 G13: what "skip" does; "install as recommended" only chose, and
// the start of the download is told; the download step once all is done ----
{
  const O = await import(new URL('onboarding.js', base).href);
  for (const [stepName, key] of [
    ['welcome', 'onboarding.skip_tip.same'],
    ['privacy', 'onboarding.skip_tip.same'],
    ['computer', 'onboarding.skip_tip.computer'],
    ['asr_mode', 'onboarding.skip_tip.asr_mode'],
    ['providers', 'onboarding.skip_tip.providers'],
    ['selftest', 'onboarding.skip_tip.selftest'],
  ]) {
    routes = { ...guideRoutes(), 'GET /api/onboarding': { step: stepName, done: false, data: {} } };
    const main = new El('main');
    body.append(main);
    const view = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
    await settle(200);
    const skip = fk('ob:skip');
    assert.equal(skip.getAttribute('title'), t(key), `${stepName}: the skip button says what it does`);
    view.destroy();
    main.remove();
  }

  // R75 M5: "skip" on the remote branch's "download" step (the Google
  // sign-in) says what skipping that leaves out; on the local download step
  // with nothing running, it is the same as "continue"
  // R81 L5: the remote step's skip follows its main button: "continue" once
  // signed in (R81 mutation M5) or when this server has no Colab at all
  const colabIdle = { status: { state: 'idle' }, adc: { present: true } };
  const colabSignedIn = { status: { state: 'idle', auth: { present: true } }, adc: { present: true } };
  const colabNone = () => ({ status: 503, body: { error: { code: 'server.unavailable', params: {} } } });
  for (const [what, data, comps, key, colab] of [
    ['remote', { asr_mode: 'remote' }, { components: [{ id: 'gcloud', state: 'installed' }], tasks: [] }, 'onboarding.skip_tip.remote', colabIdle],
    ['remote, signed in', { asr_mode: 'remote' }, { components: [{ id: 'gcloud', state: 'installed' }], tasks: [] }, 'onboarding.skip_tip.continue', colabSignedIn],
    ['remote, no Colab here', { asr_mode: 'remote' }, { components: [{ id: 'gcloud', state: 'installed' }], tasks: [] }, 'onboarding.skip_tip.continue', colabNone],
    ['local, downloading', { components: ['asr-cuda'], task_id: 'tk1' }, { components: [{ id: 'asr-cuda', state: 'available' }], tasks: [{ id: 'tk1', kind: 'install', state: 'running', ids: ['asr-cuda'], items: [] }] }, 'onboarding.skip_tip.download', colabIdle],
    ['local, all installed', { components: ['asr-cuda'] }, { components: [{ id: 'asr-cuda', state: 'installed' }], tasks: [] }, 'onboarding.skip_tip.continue', colabIdle],
  ]) {
    routes = { ...guideRoutes(), 'GET /api/components': comps, 'GET /api/onboarding': { step: 'download', done: false, data }, 'GET /api/colab': colab, 'GET /api/colab/auth/login': { state: 'idle' } };
    const main = new El('main');
    body.append(main);
    const view = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
    await settle(250);
    assert.equal(fk('ob:skip').getAttribute('title'), t(key), `${what}: what skipping does`);
    if (key === 'onboarding.skip_tip.continue') assert.equal(fk('ob:primary').textContent, t('onboarding.continue'), `${what}: the main button is "continue" too`);
    view.destroy();
    main.remove();
  }

  const OD = await import(new URL('onboarding_download.js', base).href);
  for (const [mode, key] of [
    ['local', 'onboarding.download.started'],
    ['remote', 'onboarding.download.started_remote'],
  ]) {
    clearToasts();
    routes = {
      'GET /api/components': { components: [{ id: 'asr-cuda', state: 'available' }, { id: 'gcloud', state: 'available' }], tasks: [] },
      'POST /api/components/install': () => ({ status: 202, body: { task: { id: 'tk1', kind: 'install', state: 'queued', ids: ['asr-cuda'] } } }),
    };
    const env = { data: { asr_mode: mode, components: ['asr-cuda'] }, save: async () => {} };
    await OD.startChosenInstall(env);
    assert.deepEqual(toastTexts(), [t(key)], `${mode}: the start of the download is told`);
    // nothing started (all installed): nothing told
    clearToasts();
    routes['GET /api/components'] = { components: [{ id: 'asr-cuda', state: 'installed' }, { id: 'gcloud', state: 'installed' }], tasks: [] };
    await OD.startChosenInstall({ data: { asr_mode: mode, components: ['asr-cuda'] }, save: async () => {} });
    assert.deepEqual(toastTexts(), [], `${mode}: nothing to install, nothing told`);
  }
  clearToasts();

  // the "transcription mode" step says when the download starts
  const OM = await import(new URL('onboarding_mode.js', base).href);
  routes = guideRoutes();
  for (const [storageShown, key] of [
    [false, 'onboarding.asr_mode.download_after'],
    [true, 'onboarding.asr_mode.download_after_storage'],
  ]) {
    const page = new El('div');
    body.append(page);
    let step = null;
    const env = { cache: { hardware: HW }, data: { components: ['asr-cuda'] }, storageShown, modeHint: '', alive: () => true, rerender: () => step && page.replaceChildren(D.h('div', null, step.body())), save: async () => {}, setData: async () => {}, next() {} };
    step = OM.asrModeStep(env);
    await settle(120);
    env.rerender();
    assert.ok(page.textContent.includes(t(key)), `storage step ${storageShown}: when the download starts`);
    step.destroy();
    page.remove();
  }

  // the download step's description once everything is installed
  routes = { 'GET /api/components': { components: [{ id: 'asr-cuda', state: 'installed' }], tasks: [] } };
  {
    let step = null;
    const env = { cache: {}, data: { components: ['asr-cuda'] }, alive: () => true, rerender() {}, save: async () => {}, next() {} };
    step = OD.downloadStep(env);
    await settle(80);
    assert.equal(step.desc(), t('onboarding.download.desc_done'), 'all installed: no "runs in the background"');
    step.destroy();
  }
}

// ---- T22 G9: the device test's recommendation in plain words ----
{
  const OD = await import(new URL('onboarding_download.js', base).href);
  const U = await import(new URL('util.js', base).href);
  const combo = 'large-v3 · float16';
  const run = (rtf) => [
    { model: 'small', precision: 'int8', ok: true, rtf: 0.05 },
    { model: 'large-v3', precision: 'float16', ok: true, rtf },
  ];
  const best = OD.pickText({ pick: { model: 'large-v3', precision: 'float16', rule: 'wer_rtf_le_0_5' }, results: run(0.25) }, combo);
  assert.equal(best, t('onboarding.selftest.pick_best', { combo, n: U.count(15) }), 'an hour takes 15 minutes');
  assert.ok(best.includes('15') && !best.includes('RTF'), best);
  const fastest = OD.pickText({ pick: { model: 'large-v3', precision: 'float16', rule: 'lowest_rtf' }, results: run(0.9) }, combo);
  assert.equal(fastest, t('onboarding.selftest.pick_fastest', { combo, n: U.count(54) }));
  // no speed of the recommended run: the settings page's sentence
  assert.equal(OD.pickText({ pick: { model: 'large-v3', precision: 'float16', rule: 'wer_rtf_le_0_5' }, results: [] }, combo), t('selftest.pick_best', { combo }));
  await I.setLocale('en');
  assert.ok(OD.pickText({ pick: { model: 'large-v3', precision: 'float16', rule: 'wer_rtf_le_0_5' }, results: run(0.01) }, combo).includes('about 1 minute to'), 'en: one minute, singular');
  await I.setLocale('zh-CN');
}

// ---- T23 general 16: a step that loads first keeps the focus on its title ----
{
  const O = await import(new URL('onboarding.js', base).href);
  for (const stepName of ['computer', 'asr_mode', 'providers', 'download', 'selftest']) {
    routes = { ...guideRoutes(), 'GET /api/onboarding': { step: stepName, done: false, data: { components: ['asr-cuda'] } } };
    const main = new El('main');
    body.append(main);
    doc.activeElement = body;
    const view = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
    await settle(250);
    const title = main.querySelector('h1.ob-title');
    assert.ok(title, `${stepName}: the step has its title`);
    assert.ok(doc.activeElement === title, `${stepName}: the focus is on the step's title after it loaded, not on ${doc.activeElement.tagName}`);
    assert.equal(title.textContent, t(`onboarding.${stepName}.title`), `${stepName}: the new step's title`);
    view.destroy();
    main.remove();
  }
  // a redraw of the step while its title has the focus keeps that very
  // node (a screen reader does not read it again); the rest is new
  routes = { ...guideRoutes(), 'GET /api/onboarding': { step: 'computer', done: false, data: {} } };
  const main = new El('main');
  body.append(main);
  const view = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
  await settle(250);
  const title = main.querySelector('h1.ob-title');
  const card = main.querySelector('section.ob-card');
  const oldFoot = card.querySelector('footer.ob-foot');
  assert.ok(doc.activeElement === title, 'computer: the title has the focus');
  fk('ob:custom').click(); // "customise": the step draws itself again
  await settle(120);
  assert.ok(doc.activeElement === title && title.isConnected, 'the same title keeps the focus');
  assert.ok(main.querySelector('section.ob-card') === card, 'in the same card');
  assert.ok(!oldFoot.isConnected && card.querySelector('footer.ob-foot'), 'the other parts are drawn anew');
  assert.ok(card.children.indexOf(title) === 1, 'the title in its place (after the icon)');
  assert.equal(fk('ob:custom').textContent, t('onboarding.computer.use_recommended'), 'the redraw shows the new state');
  // focus elsewhere: the card is simply replaced
  fk('ob:skip').focus();
  fk('ob:custom').click();
  await settle(120);
  assert.ok(main.querySelector('section.ob-card') !== card, 'without the focus on the title: a new card');
  assert.ok(doc.activeElement === fk('ob:skip'), 'and the focus stays on its control (data-fkey)');
  view.destroy();
  main.remove();
}

// ---- T23 general 18: the download's figures are not a live region; the
// state is told once when it changes ----
{
  const OD = await import(new URL('onboarding_download.js', base).href);
  const task = (state, done) => ({ id: 'tk1', kind: 'install', state, ids: ['asr-cuda'], total_bytes: 100, done_bytes: done, speed_bps: 10, eta_s: 9, items: [] });
  routes = { 'GET /api/components': { components: [{ id: 'asr-cuda', state: 'available' }], tasks: [task('running', 10)] }, 'GET /api/components/tasks': [task('running', 20)] };
  const page = new El('div');
  body.append(page);
  let step = null;
  const env = { cache: {}, data: { components: ['asr-cuda'], task_id: 'tk1' }, alive: () => true, rerender: () => step && page.replaceChildren(D.h('div', null, step.body())), save: async () => {}, next() {} };
  step = OD.downloadStep(env);
  await settle(80);
  const lives = () => page.querySelectorAll('[aria-live]');
  assert.equal(lives().length, 0, 'no live region around the figures');
  const status = page.querySelector('p.visually-hidden');
  assert.ok(status && status.getAttribute('role') === 'status', 'one quiet status line');
  assert.equal(status.textContent, t('compui.task.state.running'), 'it says the state');
  // progress only: the line does not change
  routes['GET /api/components/tasks'] = [task('running', 60)];
  await settle(1150);
  assert.equal(page.querySelector('p.visually-hidden').textContent, t('compui.task.state.running'), 'progress: the same line');
  routes['GET /api/components/tasks'] = [task('done', 100)];
  await settle(1150);
  assert.equal(page.querySelector('p.visually-hidden').textContent, t('compui.task.state.done'), 'done: told');
  step.destroy();
  page.remove();
}

// ---- T23 serious 3: "save" of "storage and other" keeps the focus ----
{
  routes = {
    'GET /api/settings': settingsJSON,
    'PUT /api/settings/general': (b) => ({ status: 200, body: { ...settingsJSON, general: { ...settingsJSON.general, ...b } } }),
  };
  const page = new El('main');
  body.append(page);
  const view = S.mountSettings(page, settingsCtx, null);
  await settle(300);
  const budget = fk('set:gen:budget');
  assert.ok(budget, 'the daily budget field');
  budget.value = String(Number(settingsJSON.general.daily_budget) + 5);
  budget.dispatchEvent(event('input'));
  const save = fk('set:general:save');
  assert.ok(save && !save.disabled, 'save is ready after a change');
  save.focus();
  save.click();
  await settle(120);
  assert.equal(sent('PUT', '/api/settings/general').length, 1, 'the block was saved');
  const now = fk('set:general:save');
  assert.ok(doc.activeElement === now, `the focus stays on "save" after the save, not on ${doc.activeElement.tagName}`);
  assert.equal(now.getAttribute('aria-disabled'), 'true', 'nothing to save: aria-disabled');
  assert.equal(now.disabled, false, 'not disabled while it has the focus');
  // Enter on it again does nothing
  now.click();
  await settle(60);
  assert.equal(sent('PUT', '/api/settings/general').length, 1, 'a press without changes sends nothing');
  // once the focus is elsewhere, the button is disabled as before
  const budget2 = fk('set:gen:budget');
  budget2.focus();
  budget2.dispatchEvent(event('input'));
  assert.equal(fk('set:general:save').disabled, true, 'no changes, not focused: disabled');
  assert.equal(fk('set:general:save').getAttribute('aria-disabled'), null);
  view.destroy();
  page.remove();
}

// ---- T23 general 19: the notification switch shows what is in effect ----
{
  routes = { 'GET /api/settings': { ...settingsJSON, general: { ...settingsJSON.general, notifications: true } } };
  for (const [perm, on, text] of [
    ['granted', 'true', t('settings.switch.on')],
    ['denied', 'false', t('settings.general.notify_blocked')],
  ]) {
    globalThis.Notification = { permission: perm, requestPermission: async () => perm };
    const page = new El('main');
    body.append(page);
    const view = S.mountSettings(page, settingsCtx, null);
    await settle(250);
    const sw = fk('set:gen:notify');
    assert.equal(sw.getAttribute('aria-checked'), on, `${perm}: the switch is ${on === 'true' ? 'on' : 'off'} in effect`);
    assert.equal(sw.querySelector('span.set-switch-text').textContent, text, `${perm}: its state text`);
    if (perm === 'denied') {
      clearToasts();
      sw.click();
      assert.deepEqual(toastTexts(), [t('nav.notify.denied_toast')], 'blocked: a press says how to allow them');
      assert.equal(fk('set:general:save').disabled, true, 'blocked: the setting is not changed');
      clearToasts();
    }
    view.destroy();
    page.remove();
  }
  delete globalThis.Notification;
}

// ---- T22 S6: "add from a preset" in the settings: the guide's cost plans
// and API key steps in a dialog ----
{
  const PRESETS = {
    default: 'standard',
    tiers: [
      {
        id: 'standard',
        name_key: 'onboarding.tier.standard.name',
        desc_key: 'onboarding.tier.standard.desc',
        cost_key: 'onboarding.tier.standard.cost',
        roles: { translate: { provider: 'deepseek', model: 'deepseek-chat' }, repair: { provider: 'deepseek', model: 'deepseek-chat' }, glossary: { provider: 'deepseek', model: 'deepseek-chat' }, summary: { provider: 'deepseek', model: 'deepseek-chat' } },
      },
      { id: 'transcribe_only', name_key: 'onboarding.tier.transcribe_only.name', desc_key: 'onboarding.tier.transcribe_only.desc', transcribe_only: true, roles: {} },
    ],
    providers: [
      {
        id: 'deepseek',
        name: 'DeepSeek',
        type: 'openai',
        base_url: 'https://api.deepseek.test',
        steps_keys: ['onboarding.preset.deepseek.step1', 'onboarding.preset.deepseek.step2', 'onboarding.preset.deepseek.step3'],
        signup_url: 'https://platform.deepseek.test/sign_up',
        keys_url: 'https://platform.deepseek.test/api_keys',
        cost_key: 'onboarding.preset.deepseek.cost',
        default_model: 'deepseek-chat',
        max_concurrency: 8,
      },
    ],
  };
  const created = { id: 'p-new', name: 'DeepSeek', type: 'openai', base_url: 'https://api.deepseek.test', key_hint: 'sk-…0000', used_by_roles: [], models: ['deepseek-chat'] };
  let provs = [];
  let refuse = null;
  // what the server holds (another window may change it: R81 N1)
  let live = settingsJSON;
  const withRole = (role, patch) => ({ ...live, roles: live.roles.map((r) => (r.role === role ? { ...r, ...patch } : r)) });
  routes = {
    'GET /api/settings': () => ({ status: 200, body: live }),
    'GET /api/onboarding': { step: 'done', done: true, data: {} },
    'GET /api/components/presets': PRESETS,
    'GET /api/onboarding/env-keys': [],
    'GET /api/components/sources': SOURCES,
    'GET /api/providers': () => ({ status: 200, body: provs }),
    'POST /api/providers': (b) => refuse || ((provs = [created]), { status: 201, body: { ...created, name: b.name } }),
    'POST /api/providers/p-new/test': { ok: true, latency_ms: 321 },
    'PUT /api/settings/roles/translate': settingsJSON,
    'PUT /api/settings/roles/repair': settingsJSON,
    'PUT /api/settings/roles/glossary': settingsJSON,
    'PUT /api/settings/roles/summary': settingsJSON,
  };
  calls.length = 0;
  const page = new El('main');
  body.append(page);
  const view = S.mountSettings(page, settingsCtx, null);
  await settle(250);
  const open = () => dialogs.filter((d) => d.open && d.isConnected);
  const entry = fk('set:prov:preset');
  assert.ok(entry, 'the providers section offers "add from a preset"');
  assert.equal(entry.textContent, t('settings.provider.add_preset'));
  assert.ok(fk('set:prov:add'), 'next to the plain form');
  entry.click();
  await settle(150);
  assert.equal(open().length, 1, 'a dialog opens');
  const dlg = open()[0];
  assert.equal(dlg.getAttribute('aria-label'), t('settings.provider.preset_title'));
  assert.ok(dlg.textContent.includes(t('settings.provider.preset_intro')), 'it says what saving a key and using a plan do');
  assert.ok(dlg.querySelector('[data-fkey="ob:tier:standard"]'), "the guide's cost plans");
  assert.equal(dlg.querySelector('[data-fkey="ob:tier:transcribe_only"]'), null, '"transcription only" is no choice here');
  assert.ok(dlg.textContent.includes(t('onboarding.preset.deepseek.step1')) && dlg.textContent.includes(t('onboarding.preset.deepseek.cost')), 'how to get the key, what it costs');
  assert.ok(dlg.querySelector('[data-fkey="ob:prov:deepseek:signup"]') && dlg.querySelector('[data-fkey="ob:prov:deepseek:keys"]'), 'the sign-up and key pages');
  // a refused key: the error is told (the guide's requests report while the dialog is open)
  clearToasts();
  refuse = { status: 400, body: { error: { code: 'request.bad_json', params: {} } } };
  const key = () => dlg.querySelector('[data-fkey="ob:prov:deepseek:key"]');
  key().value = 'sk-test-0000';
  key().dispatchEvent(event('input'));
  key().closest('form').dispatchEvent(event('submit'));
  await settle(120);
  assert.equal(toastTexts().length, 1, `a refused key is told (${toastTexts()})`);
  clearToasts();
  // saved: the service is added with the preset's address, then tested
  refuse = null;
  key().value = 'sk-test-0000';
  key().dispatchEvent(event('input'));
  key().closest('form').dispatchEvent(event('submit'));
  await settle(200);
  const posted = sent('POST', '/api/providers').at(-1).body;
  assert.equal(posted.base_url, 'https://api.deepseek.test', "the preset's address");
  assert.equal(posted.type, 'openai');
  assert.ok(dlg.textContent.includes(t('onboarding.providers.test_ok', { ms: '321' })), 'the connection is tested');
  // R75 M1: adding the service changes no role; the dialog has no "use this
  // plan" of the guide, the roles have a step of their own
  const roleWrites = () => calls.filter((c) => c.method === 'PUT' && c.path.startsWith('/api/settings/roles/'));
  assert.equal(roleWrites().length, 0, 'saving a key changes no role');
  assert.equal(dlg.querySelector('[data-fkey="set:preset:apply"]'), null, "no apply-the-plan of the guide");
  const rolesBtn = () => dlg.querySelector('[data-fkey="set:preset:roles"]');
  assert.equal(rolesBtn().textContent, t('settings.provider.preset_roles'));
  // the roles step: a confirmation lists each change; cancel changes nothing
  rolesBtn().click();
  await settle(80);
  let conf = open().at(-1);
  assert.ok(conf !== dlg, 'a confirmation opens on top');
  const roleName = (id) => t(`role.${id}`);
  const anth = settingsJSON.providers.find((p) => p.id === settingsJSON.roles.find((r) => r.role === 'translate').provider_id);
  assert.ok(conf.textContent.includes(t('settings.provider.preset_roles_row', { role: roleName('translate'), from: `${anth.name} · claude-sonnet-4-5`, to: 'DeepSeek · deepseek-chat' })), `translate: now → then (${conf.textContent})`);
  assert.ok(conf.textContent.includes(t('settings.provider.preset_roles_row', { role: roleName('glossary'), from: 'DeepSeek · deepseek-chat', to: 'DeepSeek · deepseek-chat' })), 'glossary: another provider of the same name');
  assert.ok(conf.textContent.includes(t('settings.provider.preset_roles_message')), 'the parameters stay');
  assert.ok(conf.textContent.includes(t('settings.provider.preset_roles_thinking', { roles: t('settings.quote', { name: roleName('glossary') }) })), "glossary's thinking goes off (the new service has none): said");
  conf.querySelectorAll('button').find((b) => b.textContent === t('common.cancel')).click();
  await settle(80);
  assert.equal(roleWrites().length, 0, 'cancel: no role changes');
  assert.equal(open().length, 1, 'the dialog stays');
  // R81 L3: the rows in the order of the Roles section
  const rowAt = (role) => conf.textContent.indexOf(`${roleName(role)}${t('settings.provider.preset_roles_row', { role: '', from: '@', to: '@' }).split('@')[0]}`);
  const order = settingsJSON.roles.map((r) => r.role).filter((r) => ['translate', 'repair', 'glossary', 'summary'].includes(r));
  assert.ok(order.every((r, i) => i === 0 || rowAt(order[i - 1]) < rowAt(r)), `rows as in the Roles section: ${order}`);
  // R81 N1: another window changes the translation's temperature; the page
  // has it (event stream) before the button is pressed: that is what is written
  live = withRole('translate', { temperature: 0.35 });
  await view.refresh();
  await settle(60);
  rolesBtn().click();
  await settle(80);
  conf = open().at(-1);
  // ... and once more while the confirmation is open: read again on confirm,
  // shown again with a word, nothing written yet
  live = withRole('translate', { temperature: 0.4 });
  const reads = sent('GET', '/api/settings').length;
  conf.querySelectorAll('button').find((b) => b.textContent === t('settings.provider.preset_roles_ok')).click();
  await settle(150);
  assert.ok(sent('GET', '/api/settings').length > reads, 'confirmed: the settings are read again before writing');
  assert.equal(roleWrites().length, 0, 'changed meanwhile: nothing written over');
  conf = open().at(-1);
  assert.ok(conf !== dlg && conf.textContent.includes(t('settings.provider.preset_roles_changed')), 'shown again, saying the settings just changed');
  // confirmed again, nothing changed since: written with what is there now
  conf.querySelectorAll('button').find((b) => b.textContent === t('settings.provider.preset_roles_ok')).click();
  await settle(250);
  const sentFor = (role) => roleWrites().find((c) => c.path === `/api/settings/roles/${role}`).body;
  assert.deepEqual(sentFor('translate'), { provider_id: 'p-new', model: 'deepseek-chat', params: { thinking: 'off', temperature: 0.4, window: 3, batch: 4 } }, "translate keeps its window and batch, and the temperature as it is now");
  assert.deepEqual(sentFor('repair'), { provider_id: 'p-new', model: 'deepseek-chat', params: { thinking: 'off', temperature: 0.1 } }, 'repair keeps its temperature');
  assert.deepEqual(sentFor('glossary'), { provider_id: 'p-new', model: 'deepseek-chat', params: { thinking: 'off', temperature: 0.2 } }, 'glossary: thinking off, as said');
  assert.deepEqual(sentFor('summary'), { provider_id: 'p-new', model: 'deepseek-chat', params: { thinking: 'off', temperature: 0.2, max_tokens: 6000 } }, 'summary keeps its output limit (R81 M1a)');
  assert.equal(roleWrites().length, 4, 'only the roles of the plan');
  assert.equal(open().length, 0, 'the dialog closed');
  assert.ok(toastTexts().includes(t('settings.provider.preset_applied', { n: 4 })), `and it says how many (${toastTexts()})`);
  live = settingsJSON;
  // the guide's own state was not touched
  assert.equal(sent('PUT', '/api/onboarding').length, 0, 'no PUT /api/onboarding');
  clearToasts();

  // the zero-cost plan: a free service's own peak pricing is kept; one
  // without any gets the night window; both are said first
  const free = (id, tariff) => ({ id, name: id, type: 'openai', base_url: `https://${id}.test/v1`, key_hint: 'sk-…0000', used_by_roles: [], models: ['m'], tariff });
  provs = [free('freeA', null), free('freeB', { timezone: 'UTC', peak_windows: [], offpeak_multiplier: 0.5 })];
  routes['GET /api/components/presets'] = {
    default: 'free',
    tiers: [{ id: 'free', name_key: 'onboarding.tier.free.name', night_processing: true, roles: { repair: { provider: 'freeA', model: 'm' }, summary: { provider: 'freeB', model: 'm' } } }],
    providers: ['freeA', 'freeB'].map((id) => ({ id, name: id, type: 'openai', base_url: `https://${id}.test/v1`, free: true, steps_keys: [] })),
  };
  routes['PUT /api/settings/roles/summary'] = settingsJSON;
  routes['PATCH /api/providers/freeA'] = settingsJSON.providers[0];
  calls.length = 0;
  fk('set:prov:preset').click();
  await settle(150);
  const dlg2 = open()[0];
  dlg2.querySelector('[data-fkey="set:preset:roles"]').click();
  await settle(80);
  conf = open().at(-1);
  const q = (n) => t('settings.quote', { name: n });
  assert.ok(conf.textContent.includes(t('settings.provider.preset_roles_night', { names: q('freeA'), from: '23:00', to: '08:00' })), `the night window is said (${conf.textContent})`);
  assert.ok(conf.textContent.includes(t('settings.provider.preset_roles_night_kept', { names: q('freeB') })), 'and the pricing kept');
  conf.querySelectorAll('button').find((b) => b.textContent === t('settings.provider.preset_roles_ok')).click();
  await settle(250);
  const patches = calls.filter((c) => c.method === 'PATCH');
  assert.deepEqual(patches.map((c) => c.path), ['/api/providers/freeA'], 'only the service without pricing of its own');
  assert.equal(patches[0].body.offpeak_only, true);
  clearToasts();
  view.destroy();
  page.remove();
}

// ---- R75 M2: the preset dialog says what happened, and nothing more ----
{
  const solo = { id: 'p-solo', name: 'Solo', type: 'openai', base_url: 'https://solo.test/v1', key_hint: 'sk-…0000', used_by_roles: [], models: ['s1'] };
  let provs = [];
  let roles = settingsJSON.roles;
  routes = {
    'GET /api/settings': () => ({ status: 200, body: { ...settingsJSON, providers: [...settingsJSON.providers, ...provs], roles } }),
    'GET /api/onboarding': { step: 'done', done: true, data: {} },
    'GET /api/components/presets': {
      default: 'standard',
      tiers: [{ id: 'standard', name_key: 'onboarding.tier.standard.name', roles: { repair: { provider: 'solo', model: 's1' } } }],
      providers: [{ id: 'solo', name: 'Solo', type: 'openai', base_url: 'https://solo.test/v1', steps_keys: [] }],
    },
    'GET /api/onboarding/env-keys': [],
    'GET /api/components/sources': SOURCES,
    'GET /api/providers': () => ({ status: 200, body: provs }),
    'POST /api/providers': () => ((provs = [solo]), { status: 201, body: solo }),
    'POST /api/providers/p-solo/test': { ok: true, latency_ms: 5 },
  };
  calls.length = 0;
  const page = new El('main');
  body.append(page);
  const view = S.mountSettings(page, settingsCtx, null);
  await settle(250);
  const open = () => dialogs.filter((d) => d.open && d.isConnected);
  const roleWrites = () => calls.filter((c) => c.method === 'PUT' && c.path.startsWith('/api/settings/roles/'));
  const openPreset = async () => {
    fk('set:prov:preset').click();
    await settle(150);
    return open().at(-1);
  };
  const closePreset = async (dlg) => {
    dlg.querySelector('[data-fkey="set:preset:close"]').click();
    await settle(200);
  };
  clearToasts();
  // no key yet: the roles step says so, asks nothing, changes nothing
  let dlg = await openPreset();
  dlg.querySelector('[data-fkey="set:preset:roles"]').click();
  await settle(80);
  assert.equal(open().length, 1, 'no confirmation to nothing');
  assert.deepEqual(toastTexts(), [t('settings.provider.preset_roles_nokey')], 'no key: said');
  clearToasts();
  // closed with nothing done: nothing is said
  await closePreset(dlg);
  assert.deepEqual(toastTexts(), [], 'nothing done, nothing said');
  // a key saved, closed without the roles step: the service added, the roles as they were
  dlg = await openPreset();
  const key = dlg.querySelector('[data-fkey="ob:prov:solo:key"]');
  key.value = 'sk-test-0000';
  key.dispatchEvent(event('input'));
  key.closest('form').dispatchEvent(event('submit'));
  await settle(200);
  clearToasts();
  // (meanwhile the repair role is set to the new service elsewhere: read on close)
  roles = settingsJSON.roles.map((r) => (r.role === 'repair' ? { ...r, provider_id: 'p-solo', model: 's1' } : r));
  await closePreset(dlg);
  assert.deepEqual(toastTexts(), [t('settings.provider.preset_added', { names: t('settings.quote', { name: 'Solo' }) })], 'added, roles unchanged: said so');
  assert.ok(!toastTexts().some((x) => x.startsWith(t('settings.provider.preset_applied', { n: 1 }).slice(0, 4))), 'not "applied"');
  assert.equal(roleWrites().length, 0, 'no role written');
  clearToasts();
  // the roles already use the plan's services: said, nothing asked
  dlg = await openPreset();
  await settle(150);
  dlg.querySelector('[data-fkey="set:preset:roles"]').click();
  await settle(80);
  assert.equal(open().length, 1, 'no confirmation');
  assert.ok(toastTexts().includes(t('settings.provider.preset_roles_same')), `already in use: said (${toastTexts()})`);
  await closePreset(dlg);
  assert.equal(roleWrites().length, 0, 'still no role written');
  clearToasts();
  view.destroy();
  page.remove();
}

// ---- R81 L1, L2: the roles step stopped half way, or the dialog closed
// (Esc) while it writes: what was changed and what not is said ----
{
  const mk = (id) => ({ id, name: id, type: 'openai', base_url: `https://${id}.test/v1`, key_hint: 'sk-…0000', used_by_roles: [], models: ['m'] });
  const provs = [mk('one')];
  routes = {
    'GET /api/settings': () => ({ status: 200, body: settingsJSON }),
    'GET /api/onboarding': { step: 'done', done: true, data: {} },
    'GET /api/components/presets': {
      default: 'standard',
      tiers: [{ id: 'standard', name_key: 'onboarding.tier.standard.name', roles: { repair: { provider: 'one', model: 'm' }, translate: { provider: 'one', model: 'm' }, summary: { provider: 'one', model: 'm' } } }],
      providers: [{ id: 'one', name: 'one', type: 'openai', base_url: 'https://one.test/v1', steps_keys: [] }],
    },
    'GET /api/onboarding/env-keys': [],
    'GET /api/components/sources': SOURCES,
    'GET /api/providers': () => ({ status: 200, body: provs }),
  };
  const ok = () => ({ status: 200, body: settingsJSON });
  const page = new El('main');
  body.append(page);
  const view = S.mountSettings(page, settingsCtx, null);
  await settle(250);
  const open = () => dialogs.filter((d) => d.open && d.isConnected);
  const roleWrites = () => calls.filter((c) => c.method === 'PUT' && c.path.startsWith('/api/settings/roles/'));
  const confirmRoles = async (dlg) => {
    dlg.querySelector('[data-fkey="set:preset:roles"]').click();
    await settle(80);
    open().at(-1).querySelectorAll('button').find((b) => b.textContent === t('settings.provider.preset_roles_ok')).click();
  };
  // the second write is refused: the loop stops there, the dialog stays,
  // and it says one of three changed (R81 mutation M1c: no going on after a failure)
  routes['PUT /api/settings/roles/summary'] = ok; // summary comes first (the Roles section's order)
  routes['PUT /api/settings/roles/repair'] = () => ({ status: 409, body: { error: { code: 'state.conflict', params: {} } } });
  routes['PUT /api/settings/roles/translate'] = ok;
  calls.length = 0;
  clearToasts();
  fk('set:prov:preset').click();
  await settle(150);
  let dlg = open().at(-1);
  await confirmRoles(dlg);
  await settle(250);
  assert.deepEqual(roleWrites().map((c) => c.path.split('/').pop()), ['summary', 'repair'], 'stops at the refused one');
  assert.ok(open().includes(dlg), 'the dialog stays: the rest can be tried again');
  assert.ok(toastTexts().includes(t('settings.provider.preset_roles_partial', { n: 1, total: 3 })), `one of three changed, said (${toastTexts()})`);
  assert.ok(!toastTexts().some((x) => x === t('settings.provider.preset_applied', { n: 1 })), 'no plain success');
  clearToasts();
  dlg.querySelector('[data-fkey="set:preset:close"]').click();
  await settle(250);
  assert.ok(!toastTexts().some((x) => x === t('settings.provider.preset_applied', { n: 1 })), 'closing then: no success either');
  clearToasts();
  // Esc while it writes: the writes go on, and their outcome is said once they end
  let release = null;
  const held = new Promise((r) => (release = r));
  routes['PUT /api/settings/roles/repair'] = () => held.then(ok);
  calls.length = 0;
  fk('set:prov:preset').click();
  await settle(150);
  dlg = open().at(-1);
  await confirmRoles(dlg);
  await settle(120);
  dlg.dispatchEvent(event('cancel')); // Esc
  await settle(60);
  assert.ok(!open().includes(dlg), 'Esc closed the dialog');
  release();
  await settle(300);
  assert.deepEqual(roleWrites().map((c) => c.path.split('/').pop()), ['summary', 'repair', 'translate'], 'the confirmed writes went on');
  assert.ok(toastTexts().includes(t('settings.provider.preset_applied', { n: 3 })), `and their outcome is said (${toastTexts()})`);
  clearToasts();
  view.destroy();
  page.remove();
}

// ---- R75 L2, L3: the preset dialog warns as the plain form does; the
// focus after the empty state's entry is gone; it puts back only its own
// error handler ----
{
  const OA = await import(new URL('onboarding_api.js', base).href);
  const U_errText = (await import(new URL('util.js', base).href)).errorInfoText;
  const solo = { id: 'p-solo', name: 'Solo', type: 'openai', base_url: 'https://solo.test/v1', key_hint: 'sk-…0000', used_by_roles: [], models: ['s1'] };
  let provs = [];
  const credErr = { code: 'settings.credentials_unavailable', params: { error: 'no keyring' } };
  routes = {
    'GET /api/settings': () => ({ status: 200, body: { ...settingsJSON, providers: provs, credentials_error_info: credErr } }),
    'GET /api/onboarding': { step: 'done', done: true, data: {} },
    'GET /api/components/presets': {
      default: 'standard',
      tiers: [{ id: 'standard', name_key: 'onboarding.tier.standard.name', roles: { repair: { provider: 'solo', model: 's1' } } }],
      providers: [{ id: 'solo', name: 'Solo', type: 'openai', base_url: 'https://solo.test/v1', steps_keys: [] }],
    },
    'GET /api/onboarding/env-keys': [],
    'GET /api/components/sources': SOURCES,
    'GET /api/providers': () => ({ status: 200, body: provs }),
    'POST /api/providers': () => ((provs = [solo]), { status: 201, body: solo }),
    'POST /api/providers/p-solo/test': { ok: true, latency_ms: 5 },
  };
  const page = new El('main');
  body.append(page);
  const view = S.mountSettings(page, settingsCtx, null);
  await settle(250);
  const open = () => dialogs.filter((d) => d.open && d.isConnected);
  const entry = fk('set:prov:preset2');
  assert.ok(entry, 'no provider yet: the empty state offers it');
  entry.focus();
  entry.click();
  await settle(150);
  const dlg = open().at(-1);
  assert.ok(dlg.textContent.includes(U_errText(credErr)), 'the key would be stored in the clear: said, as in the plain form');
  // a route to the guide meanwhile sets the guide's handler
  const guides = () => {};
  OA.setErrorHandler(guides);
  const key = dlg.querySelector('[data-fkey="ob:prov:solo:key"]');
  key.value = 'sk-test-0000';
  key.dispatchEvent(event('input'));
  key.closest('form').dispatchEvent(event('submit'));
  await settle(200);
  dlg.querySelector('[data-fkey="set:preset:close"]').click();
  await settle(250);
  assert.ok(OA.errorHandler() === guides, "the guide's handler stays");
  OA.setErrorHandler(null);
  assert.equal(fk('set:prov:preset2'), null, 'the empty state is gone');
  assert.ok(doc.activeElement === fk('set:prov:preset'), `the focus is on the section's entry, not on ${doc.activeElement.tagName}`);
  clearToasts();
  view.destroy();
  page.remove();
}

// ---- the diagnostics page: T24 G9 (a device's VRAM hint counts in the
// summary and its two pieces of advice are apart), T24 G10 / T25 G8
// ("export diagnostics" there too, with what it holds), T26 J9 / T24 J3
// (browsers listed in the language's way) ----
{
  const V = await import(new URL('view_system.js', base).href);
  const sysJSON = JSON.parse(await readFile(new URL('../testdata/api/system.json', base), 'utf8'));
  const devs = JSON.parse(await readFile(new URL('../testdata/api/system_devices_lowvram.json', base), 'utf8'));
  const okChecks = sysJSON.checks.map((c) => (c.name === 'disk' ? { ...c, status: 'warn' } : { ...c, status: 'ok', hint: '', hint_info: null }));
  const extRow = {
    name: 'browser_ext',
    status: 'warn',
    detail: 'Chrome, Edge',
    detail_info: { code: 'check.browser_ext_not_registered', params: { browsers: 'Chrome, Edge' } },
    hint_info: { code: 'check.hint_browser_ext_register', params: {} },
  };
  const sys = { ...sysJSON, checks: [...okChecks, extRow], devices: devs };
  let downloaded = null;
  routes = {
    'GET /api/app': { mode: 'web', version: '3.2.0' },
    'POST /api/app/diagnostics': () => ({ status: 200, body: 'zip' }),
  };
  globalThis.URL.createObjectURL = () => 'blob:diag';
  globalThis.URL.revokeObjectURL = () => {};
  const page = new El('main');
  body.append(page);
  const ctx = {
    system: () => sys,
    setSystem() {},
    refreshSystem: async () => sys,
    resources: () => null,
    courses: () => [{ id: 'c1' }],
    coursesLoaded: () => true,
    newCourse() {},
  };
  // dom.js download(): an <a download> clicked; record it
  const createEl = doc.createElement;
  doc.createElement = (tag) => {
    const e = createEl(tag);
    if (tag === 'a') e.click = () => (downloaded = e.getAttribute('download') || e.download || 'yes');
    return e;
  };
  const view = V.mountSystem(page, ctx);
  await settle(120);
  // the summary: the disk warning, the browser row and the device's hint
  const warns = sys.checks.filter((c) => c.status === 'warn').length;
  const title = page.querySelector('div.sys-summary-title');
  assert.equal(title.textContent, t('system.summary.warn_title', { n: warns + 1 }), `the device's VRAM hint counts: ${title.textContent}`);
  assert.equal(V.vramHintCount(sys), 1);
  const pill = page.querySelectorAll('span.sys-count').find((s) => s.className.includes('is-warn'));
  assert.equal(pill.querySelector('b').textContent, String(warns + 1), 'the warning count too');
  // the hint's two pieces of advice on lines of their own
  const two = V.vramHintRow({ id: 'cuda:0', vram_hint: { ...devs[0].vram_hint, params: { ...devs[0].vram_hint.params, extra: [{ code: 'system.devices.vram_queue', params: { per_job_mb: 3000, reserve_mb: 1024 } }] } } });
  const span = two.querySelector('span');
  assert.equal(span.querySelectorAll('br').length, 1, 'a line break between the two');
  // the export, at the top of the page, with what it holds
  const exp = fk('sys:diag');
  assert.ok(exp, 'the diagnostics page has "export diagnostics"');
  assert.equal(exp.textContent, t('settings.app.diag_export'), 'named as in Settings → App');
  assert.ok(page.querySelector('header.page-head').contains(exp), 'at the top');
  assert.ok(page.textContent.includes(t('settings.app.diag_privacy')), 'with what it holds and leaves out');
  // R75 L8: folded into one line (the summary stays the first thing on the page)
  const note = page.querySelector('details.sys-diag-note');
  assert.ok(note && !note.hasAttribute?.('open') && note.getAttribute('open') === null, 'the privacy note is folded');
  assert.equal(note.querySelector('summary').textContent, t('system.diag_note'), 'one line saying what it is');
  assert.ok(note.textContent.includes(t('settings.app.diag_privacy')), 'the whole note inside');
  // R81 L4: its line keeps the keyboard focus through the page's redraws,
  // and an opened note stays open (R81 mutation L8)
  note.querySelector('summary').focus();
  note.open = true;
  note.setAttribute('open', '');
  note.dispatchEvent(event('toggle', { target: note, bubbles: false }));
  view.onSystemChanged();
  await settle(60);
  const note2 = page.querySelector('details.sys-diag-note');
  assert.ok(note2 !== note, 'the page was drawn anew');
  assert.ok(note2.getAttribute('open') !== null, 'opened: still open after a redraw');
  assert.ok(doc.activeElement === note2.querySelector('summary'), `the focus stays on its line, not on ${doc.activeElement.tagName}`);
  clearToasts();
  exp.click();
  await settle(120);
  assert.equal(sent('POST', '/api/app/diagnostics').length >= 1, true, 'the zip is asked for');
  assert.ok(downloaded, 'and handed to the browser');
  assert.deepEqual(toastTexts(), [t('settings.app.diag_done')]);
  clearToasts();
  // the browsers as a list of the language
  const extText = page.querySelectorAll('div.sys-check-detail').map((d) => d.textContent).find((x) => x.includes('Chrome'));
  assert.ok(extText && extText.includes(`Chrome${t('common.list_sep')}Edge`) && !extText.includes('Chrome, Edge'), `zh-CN: Chrome、Edge (${extText})`);
  doc.createElement = createEl;
  view.destroy();
  page.remove();
  // the names: the page is "诊断" as in the navigation
  assert.equal(t('system.title'), t('nav.system'), 'one name for the page');
}

// ---- T24 J2: a host key to check, on the server's own card, names its button ----
{
  const R = await import(new URL('settings_remote.js', base).href);
  const server = { id: 's1', name: 'gpu-box', trusted: true, state: 'host_key_changed', kit_ready: true, last_error: { code: 'remote.host_key_mismatch', params: { fingerprint: 'SHA256:abc' } }, settings: {} };
  routes = {};
  R.noteServer(server); // the list with this server (the switch on)
  const shell = new El('section');
  body.append(shell);
  const box = (...xs) => D.h('div', null, xs.filter((x) => x instanceof Node || Array.isArray(x) || typeof x === 'string'));
  const ui = new Proxy({ rerender() {}, head: (ic, title, extra, tools) => D.h('header', null, title, extra, tools) }, { get: (o, k) => (k in o ? o[k] : box) });
  const sec = R.createRemoteSection(ui);
  sec.render(shell);
  const card = shell.querySelector('li.rs-card');
  assert.ok(card, 'the server card');
  const want = t('settings.remote.card.host_key_mismatch', { device: 'gpu-box', fingerprint: 'SHA256:abc' });
  assert.ok(card.textContent.includes(want), `the card says to press its button: ${card.textContent}`);
  assert.ok(want.includes(t('settings.remote.continue')), 'naming it as it is shown');
  assert.ok(fk('set:remote:s1:continue'), 'and the button is there');
  // elsewhere (stage card, diagnostics) the same error keeps "in Settings"
  assert.equal(R.lastErrorText(server), t('errors.remote.host_key_mismatch', { device: 'gpu-box', fingerprint: 'SHA256:abc' }));
  // R75 (mutation M5b survived): a card without "continue setup" (the setup
  // needs nothing) does not tell to press it
  const done = { ...server, state: 'ready', kit_ready: true };
  R.noteServer(done);
  shell.replaceChildren();
  sec.render(shell);
  assert.equal(fk('set:remote:s1:continue'), null, 'no "continue setup" on the card');
  const text = shell.querySelector('li.rs-card').textContent;
  assert.ok(!text.includes(t('settings.remote.continue')), `the card does not name it: ${text}`);
  assert.ok(text.includes(t('errors.remote.host_key_mismatch', { device: 'gpu-box', fingerprint: 'SHA256:abc' })), 'the error as elsewhere');
  sec.destroy?.();
  shell.remove();
}

// ---- T24 G8, T26 J8: an update that cannot be installed here points to
// its release page with a link; the outcomes say what to do next ----
{
  const SU = await import(new URL('settings_update.js', base).href);
  const notes = 'https://github.test/lectureflow/releases/tag/v3.3.0';
  for (const [what, available] of [
    ['too old', { version: '3.3.0', direct: false, min_version: '3.2.0', file: 'LectureFlow-Setup-3.3.0-x64.exe', notes_url: notes }],
    ['no package', { version: '3.3.0', direct: true, file: '', notes_url: notes }],
  ]) {
    routes = { 'GET /api/selfupdate': { state: 'available', current_version: '3.1.0', platform: 'windows', kind: 'nsis', available } };
    const shell = new El('div');
    body.append(shell);
    let sec = null;
    const ui = { formRow: (label, ...xs) => D.h('div', null, label, xs), rerender: () => sec && shell.replaceChildren(...sec.rows()) };
    sec = SU.createUpdateSection(ui);
    sec.load();
    await settle(120);
    const link = fk('set:upd:release');
    assert.ok(link, `${what}: a link to the release page`);
    assert.equal(link.getAttribute('href'), notes, `${what}: the version's page`);
    assert.equal(link.textContent, t('update.open_release'));
    assert.ok(link.closest('p').textContent.includes(t('update.open_release')) && link.closest('p').className.includes('is-warn'), `${what}: next to the reason`);
    sec.destroy();
    shell.remove();
  }
  // without a page to link to: the reason alone
  routes = { 'GET /api/selfupdate': { state: 'available', current_version: '3.1.0', available: { version: '3.3.0', direct: false, min_version: '3.2.0' } } };
  {
    const shell = new El('div');
    body.append(shell);
    let sec = null;
    sec = SU.createUpdateSection({ formRow: (label, ...xs) => D.h('div', null, label, xs), rerender: () => sec && shell.replaceChildren(...sec.rows()) });
    sec.load();
    await settle(120);
    assert.equal(fk('set:upd:release'), null, 'no notes page: no link');
    sec.destroy();
    shell.remove();
  }
  // the outcomes say what to do next (where to download again)
  for (const outcome of ['rolled_back', 'failed']) {
    const text = SU.resultText({ outcome, version: '3.3.0', from: '3.1.0', at: '2026-11-14T10:00:00Z' });
    assert.ok(text.includes(t('settings.update.label')), `${outcome}: names where to try again: ${text}`);
  }
  assert.ok(t('errors.selfupdate.checksum', { file: 'x.exe' }).includes(t('settings.update.retry_download')), 'a refused download names "download again"');
  assert.ok(t('errors.selfupdate.checksum', { file: 'x.exe' }).includes(t('settings.update.file_label')), 'and "update from file"');
  // R75 M4: the same refusal of files the user chose has nothing to download again
  const A_ = await import(new URL('api.js', base).href);
  const refused = new A_.ApiError(422, 'x', { error: { code: 'selfupdate.checksum', params: { file: 'LectureFlow-Setup-3.3.0-x64.exe' }, detail: 'no windows package of update.json has them' } });
  const info = SU.importErrorInfo(refused);
  assert.equal(info.text, t('settings.update.file_checksum', { file: 'LectureFlow-Setup-3.3.0-x64.exe' }), 'its own words');
  assert.ok(!info.text.includes(`“${t('settings.update.retry_download')}”`) && !info.text.includes(`“${t('settings.update.file_label')}”`), 'no button "download again" or "update from file" (the user is there already)');
  assert.equal(info.detail, 'no windows package of update.json has them', 'the details as they came');
  assert.equal(SU.importErrorInfo(new A_.ApiError(400, 'y', { error: { code: 'selfupdate.import_incomplete', params: {} } })), null, 'other refusals: the shared text');
  // a notes page that is not https gives no link (R75: M6 survived)
  for (const notes_url of ['http://github.test/r', 'javascript:alert(1)', '']) {
    routes = { 'GET /api/selfupdate': { state: 'available', current_version: '3.1.0', available: { version: '3.3.0', direct: false, min_version: '3.2.0', notes_url } } };
    const shell = new El('div');
    body.append(shell);
    let sec = null;
    sec = SU.createUpdateSection({ formRow: (label, ...xs) => D.h('div', null, label, xs), rerender: () => sec && shell.replaceChildren(...sec.rows()) });
    sec.load();
    await settle(120);
    assert.equal(fk('set:upd:release'), null, `notes_url ${JSON.stringify(notes_url)}: no link`);
    assert.ok(shell.querySelectorAll('a').every((a) => /^https:/i.test(a.getAttribute('href') || '')), `notes_url ${JSON.stringify(notes_url)}: no other link either`);
    sec.destroy();
    shell.remove();
  }
}

// ---- AUD5 info-1: a preset's links are links only when they are https
// (the guide's step and the settings' "add from a preset" alike) ----
{
  const OP = await import(new URL('onboarding_providers.js', base).href);
  const bad = ['javascript:void(0)//AUD5', 'http://plain.test/keys', 'data:text/html,x', 'file:///C:/x', ' https://space.test', 'HTTPS://ok.test/terms'];
  const pp = (id, urls) => ({ id, name: id, type: 'openai', base_url: `https://${id}.test/v1`, free: true, terms_notice: true, steps_keys: [], ...urls });
  const presets = {
    default: 'standard',
    tiers: [{ id: 'standard', name_key: 'onboarding.tier.standard.name', roles: { translate: { provider: 'evil' }, repair: { provider: 'good' } } }],
    providers: [
      pp('evil', { signup_url: bad[0], keys_url: bad[1], pricing_url: bad[2], terms_url: bad[3] }),
      pp('good', { signup_url: 'https://good.test/signup', keys_url: bad[4], pricing_url: 'https://good.test/pricing', terms_url: bad[5] }),
    ],
  };
  routes = { 'GET /api/components/presets': presets, 'GET /api/onboarding/env-keys': [], 'GET /api/providers': [], 'GET /api/components/sources': SOURCES };
  for (const inSettings of [false, true]) {
    const page = new El('div');
    body.append(page);
    let step = null;
    const env = { inSettings, cache: {}, data: {}, alive: () => true, rerender: () => step && page.replaceChildren(D.h('div', null, step.body())), save: async () => {}, setData: async () => {}, next() {} };
    step = OP.providersStep(env);
    await settle(120);
    env.rerender();
    const hrefs = page.querySelectorAll('a').map((a) => a.getAttribute('href'));
    for (const u of bad.slice(0, 5)) assert.ok(!hrefs.includes(u), `${inSettings ? 'settings' : 'guide'}: ${u} is no link`);
    assert.ok(hrefs.includes('https://good.test/signup') && hrefs.includes('https://good.test/pricing'), `${inSettings ? 'settings' : 'guide'}: https links stay (${hrefs})`);
    assert.ok(hrefs.includes('HTTPS://ok.test/terms'), 'the scheme in any case');
    assert.equal(page.querySelector('[data-fkey="ob:prov:evil:signup"]'), null, 'nothing drawn in its place');
    page.remove();
  }
}

// ---- T23 general 18 at the source: components_ui.js taskView (the guide's
// download step, Settings → Components, the Colab sign-in) is no live region;
// one status line says the state when it changes ----
{
  const CU = await import(new URL('components_ui.js', base).href);
  const task = (state, done, extra = {}) => ({ id: 'tk9', kind: 'install', state, ids: ['asr-cuda'], total_bytes: 100, done_bytes: done, speed_bps: 10, eta_s: 9, items: [], ...extra });
  const view = CU.taskView({ components: () => [] });
  const page = new El('div');
  body.append(page);
  page.append(view.el);
  const status = () => view.el.querySelectorAll('p').filter((p) => p.getAttribute('role') === 'status');
  view.update(task('running', 10));
  assert.equal(view.el.getAttribute('aria-live'), null, 'the task view is no live region');
  assert.equal(page.querySelectorAll('[aria-live]').length, 0, 'nor anything in it');
  assert.equal(status().length, 1, 'one status line');
  const line = status()[0];
  assert.equal(line.className, 'visually-hidden');
  assert.equal(line.textContent, t('compui.task.state.running'), 'it says the state');
  view.update(task('running', 50));
  view.update(task('running', 70));
  assert.ok(status()[0] === line && line.isConnected, 'progress: the same line stays');
  assert.equal(line.textContent, t('compui.task.state.running'), 'progress: nothing new to say');
  assert.ok(view.el.textContent.includes('70'), 'the figures are still shown');
  view.update(task('paused', 70));
  assert.equal(line.textContent, t('compui.task.state.paused'), 'paused: told');
  view.update(task('failed', 70, { error: { code: 'components.network', params: {} } }));
  assert.equal(line.textContent, '', 'failed: the alert says it (not twice)');
  assert.ok(view.el.querySelector('div.cu-error').getAttribute('role') === 'alert', 'the failure is an alert');
  view.update(task('done', 100));
  assert.equal(line.textContent, t('compui.task.state.done'), 'done: told');
  assert.equal(view.el.dataset.state, 'done');
  page.remove();
  // the space a plan needs is explained (download and unpacked files side by side)
  assert.ok(CU.planSpaceText({ enough_space: false, need_bytes: 15e9, free_bytes: 2e9 }).includes('解包'), 'zh-CN: why the need is more than the download');
}

console.log('r7 settings checks: ok');
process.exit(0);
