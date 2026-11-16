// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 8 (the settings line): the settings page, the setup guide and the
// new-course wizard, run on a DOM stand-in whose focus behaves as a
// browser's where it matters here (as in r7_settings_test.mjs: a removed
// focused element, or one made disabled, leaves the focus on the page; a
// disabled or removed element takes no focus). web/r8_settings_test.go
// starts it with the web directory as a file: URL. Nodes are compared by
// identity (assert.ok(a === b)), never with assert.equal (testdata/README.md).
// API keys here are made-up test values only.

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

// Nodes watched for being taken out of their place (WS-04): every remove()
// of a watched node, or of a node that holds one, is counted.
const watched = new Map(); // node → {detached}

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
    if (watched.size) for (const [w, rec] of watched) if (up(w, (n) => n === this)) rec.detached++;
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
class Frag extends Node {
  constructor() {
    super();
    this.children = [];
  }
  append(...cs) {
    El.prototype.append.apply(this, cs);
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join('');
  }
  get firstChild() {
    return this.children[0] || null;
  }
  get lastChild() {
    return this.children[this.children.length - 1] || null;
  }
  get childNodes() {
    return this.children;
  }
  contains(n) {
    return !!up(n, (x) => x === this);
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
      add(...cs) {
        for (const c of cs) this.toggle(c, true);
      },
      remove(...cs) {
        for (const c of cs) this.toggle(c, false);
      },
      contains(c) {
        return self.className.split(/\s+/).includes(c);
      },
    };
  }
  set textContent(v) {
    this.replaceChildren(...(v === '' ? [] : [new Text(v)]));
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join('');
  }
  get firstChild() {
    return this.children[0] || null;
  }
  get lastChild() {
    return this.children[this.children.length - 1] || null;
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
  hasAttribute(k) {
    return k in this.attrs;
  }
  removeAttribute(k) {
    delete this.attrs[k];
  }
  toggleAttribute(k, on) {
    if (on === undefined ? !(k in this.attrs) : on) this.attrs[k] = '';
    else delete this.attrs[k];
  }
  append(...cs) {
    for (const c0 of cs) {
      const c = c0 instanceof Node ? c0 : new Text(c0);
      if (c instanceof Frag) {
        const kids = c.children.splice(0);
        for (const k of kids) {
          k.parentNode = this;
          this.children.push(k);
        }
        continue;
      }
      c.remove();
      c.parentNode = this;
      this.children.push(c);
    }
  }
  insertBefore(n, ref) {
    if (!ref) {
      this.append(n);
      return n;
    }
    if (n instanceof Frag) {
      for (const k of n.children.splice(0)) {
        k.parentNode = this;
        this.children.splice(this.children.indexOf(ref), 0, k);
      }
      return n;
    }
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
  scrollIntoView(o) {
    scrolled.push(this);
  }
  setSelectionRange() {}
  getBoundingClientRect() {
    return { top: 0, bottom: 0, left: 0, right: 0 };
  }
  // WS-06: the syllabus preview's scroll box. Its content is 20 px per
  // block of the preview, the box 400 px high; the place scrolled to is
  // held within what the content allows, as a browser's layout does.
  get scrollHeight() {
    const pv = this.className.includes('syl-md-body') ? this.children.find((c) => c instanceof El && c.className.includes('syl-preview')) : null;
    return pv && !pv.hidden ? 20 * pv.children.length : 0;
  }
  get clientHeight() {
    return 400;
  }
  get scrollTop() {
    const max = Math.max(0, this.scrollHeight - this.clientHeight);
    if ((this._top || 0) > max) this._top = max;
    return this._top || 0;
  }
  set scrollTop(v) {
    this._top = Math.max(0, Math.min(Number(v) || 0, Math.max(0, this.scrollHeight - this.clientHeight)));
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
const scrolled = []; // elements scrollIntoView() was called on
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
  createDocumentFragment: () => new Frag(),
  visibilityState: 'visible',
};

Object.assign(globalThis, {
  Node,
  location: { search: '', hash: '#/settings', pathname: '/', origin: 'http://127.0.0.1', reload() {} },
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
  console.error('unhandled rejection:', e && e.stack ? e.stack : String(e));
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
// waits until fn() holds, at most tries × 10 ms (fails fast, never hangs)
async function until(fn, what, tries = 300) {
  for (let i = 0; i < tries; i++) {
    if (fn()) return;
    await settle(10);
  }
  throw new assert.AssertionError({ message: `timed out waiting for: ${what}` });
}

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
const U = await import(new URL('util.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });

const fk = (k) => doc.querySelector(`[data-fkey="${k}"]`);
const toastTexts = () => toasts.children.map((x) => x.dataset.msg);
const clearToasts = () => toasts.replaceChildren();
const openDialogs = () => dialogs.filter((d) => d.open && d.isConnected);
const press = (dlg, label) => {
  const b = dlg.querySelectorAll('button').find((x) => x.textContent === label);
  assert.ok(b, `the dialog has a button “${label}”`);
  b.click();
};

const settingsJSON = JSON.parse(await readFile(new URL('../testdata/api/v11_settings.json', base), 'utf8'));
const S = await import(new URL('view_settings.js', base).href);
const settingsCtx = { resources: () => null, system: () => null, subscribe: () => () => {} };
const SOURCES = { pypi: { mode: 'default' }, models: { mode: 'default' }, options: {}, suggest_mirror: false };

async function mountSettings(route = null) {
  const page = new El('main');
  body.append(page);
  const view = S.mountSettings(page, settingsCtx, route);
  await settle(250);
  return {
    page,
    view,
    // the page goes, and the dialogs left open with it (Esc)
    end() {
      for (const d of openDialogs()) d.dispatchEvent(event('cancel', { bubbles: false }));
      view.destroy();
      page.remove();
      clearToasts();
    },
  };
}

// ---- WS-12 (F3): the settings page's "continue the setup guide" banner
// follows util.js guideLeft ----
{
  for (const [what, st, shown] of [
    ['left at a step', { step: 'providers', done: false, data: { skipped: ['computer'] } }, true],
    ['never started (the default state)', { step: 'welcome', done: false, data: {} }, false],
    ['done', { step: 'done', done: true, data: { skipped: [] } }, false],
  ]) {
    routes = { 'GET /api/settings': settingsJSON, 'GET /api/onboarding': st };
    const m = await mountSettings();
    assert.equal(!!fk('set:alert:guide'), shown, `${what}: the banner ${shown ? 'is' : 'is not'} shown`);
    assert.equal(!!fk('set:alert:guide'), U.guideLeft(st), `${what}: as util.js guideLeft says`);
    m.end();
  }
}

// ---- WS-09 (R79 L12): "the model is not in the provider's list" on a
// role row says what the lecture page says (util.js modelListed: case,
// a dated id's alias, "-latest") ----
{
  const dated = 'claude-sonnet-4-5-20250929';
  const anth = settingsJSON.providers.find((p) => p.type === 'anthropic');
  for (const [what, model, list, listed] of [
    ['another case', 'Claude-Sonnet-4-5-20250929', [dated, 'claude-haiku-4-5'], true],
    ["a dated id's alias", 'claude-sonnet-4-5', [dated, 'claude-haiku-4-5'], true],
    ['-latest', 'claude-sonnet-4-5-latest', [dated, 'claude-haiku-4-5'], true],
    ['not listed', 'claude-sonnet-9', [dated, 'claude-haiku-4-5'], false],
  ]) {
    const s = {
      ...settingsJSON,
      providers: settingsJSON.providers.map((p) => (p.id === anth.id ? { ...p, models: list } : p)),
      roles: settingsJSON.roles.map((r) => (r.role === 'translate' ? { ...r, model } : r)),
    };
    routes = { 'GET /api/settings': s, 'GET /api/onboarding': { step: 'done', done: true, data: {} } };
    const m = await mountSettings();
    const hint = fk('set:role:translate:unlisted');
    assert.equal(!!hint, !listed, `${what}: the hint ${listed ? 'is not' : 'is'} shown`);
    // the lecture page's verdict on the same model (util.js unlistedModel)
    const st = { error_info: { code: 'llm.request_failed', params: { status: 404 } }, model: { provider_id: anth.id, model } };
    assert.equal(!!hint, !!U.unlistedModel(st, s.providers), `${what}: the same verdict as the lecture page`);
    m.end();
  }
}

// ---- the preset dialog's roles step (R81b L9, L10; R81 I8) ----
const free = (id, tariff) => ({ id, name: id, type: 'openai', base_url: `https://${id}.test/v1`, key_hint: 'sk-…0000', used_by_roles: [], models: ['m'], tariff, offpeak_only: false, max_concurrency: 4, rpm: 60, balance_api: 'none', snapshot_refs: 0 });
const peak = { timezone: 'UTC', peak_windows: [{ days: [1, 2, 3, 4, 5], from: '09:00', to: '18:00' }], offpeak_multiplier: 0.5 };
const roleWrites = () => calls.filter((c) => c.method === 'PUT' && c.path.startsWith('/api/settings/roles/'));
const okRole = () => ({ status: 200, body: settingsJSON });

// WS-01 (R81b L9): the zero-cost plan's night window goes to a free
// service by its pricing in the settings as they are now, not as the
// guide's step read them when the dialog opened: one given its own
// pricing meanwhile keeps it, said in the confirmation, and no PATCH
{
  const freeTier = {
    default: 'free',
    tiers: [{ id: 'free', name_key: 'onboarding.tier.free.name', night_processing: true, roles: { repair: { provider: 'freeA', model: 'm' } } }],
    providers: [{ id: 'freeA', name: 'freeA', type: 'openai', base_url: 'https://freeA.test/v1', free: true, steps_keys: [] }],
  };
  const withFree = (tariff) => ({ ...settingsJSON, providers: [...settingsJSON.providers, free('freeA', tariff)] });
  const q = (n) => t('settings.quote', { name: n });
  for (const when of ['before the button', 'while the confirmation is open']) {
    let live = withFree(null);
    routes = {
      'GET /api/settings': () => ({ status: 200, body: live }),
      'GET /api/onboarding': { step: 'done', done: true, data: {} },
      'GET /api/components/presets': freeTier,
      'GET /api/onboarding/env-keys': [],
      'GET /api/components/sources': SOURCES,
      // the guide's step reads the services once, when the dialog opens: no pricing then
      'GET /api/providers': [free('freeA', null)],
      'PUT /api/settings/roles/repair': okRole,
      'PATCH /api/providers/freeA': () => ({ status: 200, body: free('freeA', null) }),
    };
    calls.length = 0;
    const m = await mountSettings();
    fk('set:prov:preset').click();
    await settle(150);
    const dlg = openDialogs().at(-1);
    // another window gives the free service its own peak pricing
    if (when === 'before the button') {
      live = withFree(peak);
      await m.view.refresh();
      await settle(60);
    }
    dlg.querySelector('[data-fkey="set:preset:roles"]').click();
    await settle(80);
    let conf = openDialogs().at(-1);
    if (when === 'while the confirmation is open') {
      assert.ok(conf.textContent.includes(t('settings.provider.preset_roles_night', { names: q('freeA'), from: '23:00', to: '08:00' })), `${when}: the night window is said first (${conf.textContent})`);
      live = withFree(peak);
      press(conf, t('settings.provider.preset_roles_ok'));
      await settle(150);
      assert.equal(roleWrites().length, 0, `${when}: read again on confirm, nothing written yet`);
      conf = openDialogs().at(-1);
      assert.ok(conf !== dlg && conf.textContent.includes(t('settings.provider.preset_roles_changed')), `${when}: shown again, saying the settings changed`);
    }
    assert.ok(conf.textContent.includes(t('settings.provider.preset_roles_night_kept', { names: q('freeA') })), `${when}: its own pricing is kept, said (${conf.textContent})`);
    assert.ok(!conf.textContent.includes(t('settings.provider.preset_roles_night', { names: q('freeA'), from: '23:00', to: '08:00' })), `${when}: no night window promised`);
    press(conf, t('settings.provider.preset_roles_ok'));
    await settle(250);
    assert.equal(roleWrites().length, 1, `${when}: the role is written`);
    assert.equal(calls.filter((c) => c.method === 'PATCH').length, 0, `${when}: the pricing set elsewhere is not written over`);
    m.end();
  }
}

// WS-02 (R81b L10): stopped half way, then pressed again at once (before
// any event refreshed the page): the roles already changed are not listed
// again, and nothing claims the settings were changed elsewhere
{
  const mk = (id) => ({ id, name: id, type: 'openai', base_url: `https://${id}.test/v1`, key_hint: 'sk-…0000', used_by_roles: [], models: ['m'] });
  let live = settingsJSON;
  const setLive = (role) => (live = { ...live, roles: live.roles.map((r) => (r.role === role ? { ...r, provider_id: 'one', model: 'm' } : r)), providers: live.providers.some((p) => p.id === 'one') ? live.providers : [...live.providers, { ...free('one', null), name: 'one' }] });
  routes = {
    'GET /api/settings': () => ({ status: 200, body: live }),
    'GET /api/onboarding': { step: 'done', done: true, data: {} },
    'GET /api/components/presets': {
      default: 'standard',
      tiers: [{ id: 'standard', name_key: 'onboarding.tier.standard.name', roles: { repair: { provider: 'one', model: 'm' }, translate: { provider: 'one', model: 'm' }, summary: { provider: 'one', model: 'm' } } }],
      providers: [{ id: 'one', name: 'one', type: 'openai', base_url: 'https://one.test/v1', steps_keys: [] }],
    },
    'GET /api/onboarding/env-keys': [],
    'GET /api/components/sources': SOURCES,
    'GET /api/providers': [mk('one')],
    // the server holds what was written (no event stream here: the page
    // only learns of it by reading the settings again)
    'PUT /api/settings/roles/summary': () => (setLive('summary'), okRole()),
    'PUT /api/settings/roles/repair': () => ({ status: 409, body: { error: { code: 'state.conflict', params: {} } } }),
    'PUT /api/settings/roles/translate': () => (setLive('translate'), okRole()),
  };
  calls.length = 0;
  const m = await mountSettings();
  fk('set:prov:preset').click();
  await settle(150);
  const dlg = openDialogs().at(-1);
  dlg.querySelector('[data-fkey="set:preset:roles"]').click();
  await settle(80);
  press(openDialogs().at(-1), t('settings.provider.preset_roles_ok'));
  await settle(250);
  assert.deepEqual(roleWrites().map((c) => c.path.split('/').pop()), ['summary', 'repair'], 'the second write is refused: stopped there');
  assert.ok(openDialogs().includes(dlg), 'the dialog stays');
  // pressed again at once; the refused one goes through now
  routes['PUT /api/settings/roles/repair'] = () => (setLive('repair'), okRole());
  clearToasts();
  calls.length = 0;
  dlg.querySelector('[data-fkey="set:preset:roles"]').click();
  await settle(80);
  const conf = openDialogs().at(-1);
  const row = (role, from) => t('settings.provider.preset_roles_row', { role: t(`role.${role}`), from, to: 'one · m' });
  assert.ok(conf !== dlg, 'a confirmation');
  assert.ok(!conf.textContent.includes(row('summary', 'DeepSeek · deepseek-chat')), `the role changed already is not listed again (${conf.textContent})`);
  assert.ok(conf.textContent.includes(row('repair', 'DeepSeek · deepseek-chat')) && conf.textContent.includes(row('translate', 'Anthropic · claude-sonnet-4-5')), 'the two left are');
  assert.ok(!conf.textContent.includes(t('settings.provider.preset_roles_changed')), 'no word about a change made elsewhere');
  press(conf, t('settings.provider.preset_roles_ok'));
  await settle(250);
  assert.deepEqual(roleWrites().map((c) => c.path.split('/').pop()), ['repair', 'translate'], 'only the two left are written');
  assert.equal(openDialogs().filter((d) => d !== dlg && d.textContent.includes(t('settings.provider.preset_roles_changed'))).length, 0, 'and never a second confirmation about it');
  m.end();

  // the settings cannot be read after the writes (the server answers an
  // error once): pressed again, the confirmation comes back once the
  // settings are read, saying they just changed, without "elsewhere"
  live = settingsJSON;
  let failRead = false;
  routes['GET /api/settings'] = () => (failRead ? ((failRead = false), { status: 500, body: { error: { code: 'server.internal', params: {} } } }) : { status: 200, body: live });
  routes['PUT /api/settings/roles/repair'] = () => ((failRead = true), { status: 409, body: { error: { code: 'state.conflict', params: {} } } });
  calls.length = 0;
  const m2 = await mountSettings();
  fk('set:prov:preset').click();
  await settle(150);
  const dlg2 = openDialogs().at(-1);
  dlg2.querySelector('[data-fkey="set:preset:roles"]').click();
  await settle(80);
  press(openDialogs().at(-1), t('settings.provider.preset_roles_ok'));
  await settle(250);
  assert.deepEqual(roleWrites().map((c) => c.path.split('/').pop()), ['summary', 'repair'], 'stopped at the refused one again');
  routes['PUT /api/settings/roles/repair'] = () => (setLive('repair'), okRole());
  calls.length = 0;
  dlg2.querySelector('[data-fkey="set:preset:roles"]').click();
  await settle(80);
  let conf2 = openDialogs().at(-1);
  assert.ok(conf2.textContent.includes(row('summary', 'DeepSeek · deepseek-chat')), 'the page could not read the settings: the changed role is still listed');
  press(conf2, t('settings.provider.preset_roles_ok'));
  await settle(150);
  assert.equal(roleWrites().length, 0, 'read on confirm: different, nothing written yet');
  conf2 = openDialogs().at(-1);
  assert.ok(conf2.textContent.includes(t('settings.provider.preset_roles_changed_any')), `shown again: the settings just changed (${conf2.textContent})`);
  assert.ok(!conf2.textContent.includes(t('settings.provider.preset_roles_changed')), 'not said to be changed elsewhere');
  assert.ok(!conf2.textContent.includes(row('summary', 'DeepSeek · deepseek-chat')), 'and the changed role is gone from it');
  press(conf2, t('settings.provider.preset_roles_ok'));
  await settle(250);
  assert.deepEqual(roleWrites().map((c) => c.path.split('/').pop()), ['repair', 'translate'], 'the two left are written');
  m2.end();
}

// R81 I8: a confirmation with nothing more to note has no empty list
{
  const mk = (id) => ({ id, name: id, type: 'openai', base_url: `https://${id}.test/v1`, key_hint: 'sk-…0000', used_by_roles: [], models: ['m'] });
  routes = {
    'GET /api/settings': () => ({ status: 200, body: settingsJSON }),
    'GET /api/onboarding': { step: 'done', done: true, data: {} },
    'GET /api/components/presets': {
      default: 'standard',
      tiers: [{ id: 'standard', name_key: 'onboarding.tier.standard.name', roles: { repair: { provider: 'one', model: 'm' } } }],
      providers: [{ id: 'one', name: 'one', type: 'openai', base_url: 'https://one.test/v1', steps_keys: [] }],
    },
    'GET /api/onboarding/env-keys': [],
    'GET /api/components/sources': SOURCES,
    'GET /api/providers': [mk('one')],
  };
  const m = await mountSettings();
  fk('set:prov:preset').click();
  await settle(150);
  openDialogs().at(-1).querySelector('[data-fkey="set:preset:roles"]').click();
  await settle(80);
  const conf = openDialogs().at(-1);
  const box = conf.querySelector('div.set-preset-roles');
  assert.equal(box.querySelectorAll('ul').length, 1, 'one list: the rows; no empty list of notes');
  assert.ok(box.querySelectorAll('ul').every((u) => u.children.length > 0), 'no list without items');
  press(conf, t('common.cancel'));
  await settle(60);
  m.end();
}

// ---- WS-10 (AUD1 I-3): a provider's address on plain http to another
// computer off the local network carries a note, on its row and under the
// address in its dialog (saving is not blocked) ----
{
  const note = t('settings.provider.plain_http_note');
  const cases = [
    ['http://127.0.0.1:8000/v1', false],
    ['http://192.168.1.5:8000/v1', false],
    ['http://example.com/v1', true],
    ['https://example.com/v1', false],
    // more of the same rule
    ['http://localhost:11434/v1', false],
    ['http://10.0.0.2/v1', false],
    ['http://172.20.1.1/v1', false],
    ['http://172.32.0.1/v1', true],
    ['http://100.64.0.9/v1', false],
    ['http://169.254.1.1/v1', false],
    ['http://8.8.8.8/v1', true],
    ['http://[fd00::1]:8080/v1', false],
    ['http://[fe80::1]/v1', false],
    ['http://[::ffff:192.168.1.5]/v1', false],
    ['http://[2001:db8::1]/v1', true],
    ['http://gpu-box:8000/v1', false],
    ['http://gpu-box.local:8000/v1', false],
    ['http://api.example.org.:8000/v1', true],
    ['HTTP://EXAMPLE.COM/v1', true],
  ];
  const provs = cases.map(([url], i) => ({ ...free(`p${i}`, null), name: `p${i}`, base_url: url }));
  routes = { 'GET /api/settings': { ...settingsJSON, providers: provs }, 'GET /api/onboarding': { step: 'done', done: true, data: {} } };
  const m = await mountSettings();
  const rows = m.page.querySelectorAll('li.pv-row');
  assert.equal(rows.length, cases.length, 'a row per provider');
  cases.forEach(([url, want], i) => {
    const row = rows.find((r) => r.querySelector('div.pv-url').textContent === url);
    assert.ok(row, `the row of ${url}`);
    assert.equal(row.textContent.includes(note), want, `${url}: the note ${want ? 'is' : 'is not'} on its row`);
  });
  // the provider dialog: the note follows the address typed
  fk('set:prov:add').click();
  await settle(60);
  const dlg = openDialogs().at(-1);
  const base = dlg.querySelectorAll('input').find((x) => x.getAttribute('type') === 'url');
  assert.ok(base, 'the address field');
  const shown = () => dlg.querySelectorAll('span').some((s) => !s.hidden && s.textContent.includes(note));
  for (const [url, want] of cases.slice(0, 4)) {
    base.value = url;
    base.dispatchEvent(event('input'));
    assert.equal(shown(), want, `dialog, ${url}: the note ${want ? 'is' : 'is not'} shown`);
    if (want) {
      const n = dlg.querySelectorAll('span').find((s) => !s.hidden && s.textContent.includes(note));
      assert.ok((base.getAttribute('aria-describedby') || '').split(' ').includes(n.getAttribute('id')), 'and describes the address field');
    }
  }
  press(dlg, t('common.cancel'));
  await settle(60);
  // round 9 WV-06: the address changed, so "取消" asks "放弃修改？" first
  press(openDialogs().at(-1), t('wizard.close.discard'));
  await settle(60);
  assert.ok(!dlg.open, 'discarded: closed');
  m.end();
}

// ---- WS-11 (round 8 F4): #/settings/providers/preset opens "add from a
// preset", #/settings/providers/{provider id} goes to that provider's row ----
{
  const presets = {
    default: 'standard',
    tiers: [{ id: 'standard', name_key: 'onboarding.tier.standard.name', roles: { repair: { provider: 'one', model: 'm' } } }],
    providers: [{ id: 'one', name: 'one', type: 'openai', base_url: 'https://one.test/v1', steps_keys: [] }],
  };
  let live = settingsJSON;
  routes = {
    'GET /api/settings': () => ({ status: 200, body: live }),
    'GET /api/onboarding': { step: 'done', done: true, data: {} },
    'GET /api/components/presets': presets,
    'GET /api/onboarding/env-keys': [],
    'GET /api/components/sources': SOURCES,
    'GET /api/providers': [],
  };
  const presetDialogs = () => openDialogs().filter((d) => d.getAttribute('aria-label') === t('settings.provider.preset_title'));
  // the address on opening the page
  doc.activeElement = body;
  let m = await mountSettings({ name: 'settings', section: 'providers', device: 'preset' });
  await settle(100);
  assert.equal(presetDialogs().length, 1, 'preset: the dialog is open once the page is ready');
  // the page drawn again (settings changed meanwhile): no second dialog
  live = { ...settingsJSON, general: { ...settingsJSON.general, daily_budget: Number(settingsJSON.general.daily_budget) + 1 } };
  await m.view.refresh();
  await settle(100);
  m.view.onChanged({ kind: 'settings' });
  await settle(300);
  assert.equal(presetDialogs().length, 1, 'redrawn: still the one dialog');
  presetDialogs()[0].querySelector('[data-fkey="set:preset:close"]').click();
  await settle(250);
  assert.equal(presetDialogs().length, 0, 'closed');
  assert.ok(doc.activeElement === fk('set:prov:preset'), `closed: the focus is on the section's "add from a preset", not on ${doc.activeElement.tagName}`);
  await m.view.refresh();
  await settle(150);
  assert.equal(presetDialogs().length, 0, 'not opened again by a later redraw');
  // the same address while the page is open (another view's link)
  assert.equal(m.view.update({ name: 'settings', section: 'providers', device: 'preset' }), true, 'the page takes the address itself');
  await settle(100);
  assert.equal(presetDialogs().length, 1, 'the dialog opens');
  presetDialogs()[0].querySelector('[data-fkey="set:preset:close"]').click();
  await settle(250);
  m.end();

  // a provider's row: scrolled to, its edit button has the focus
  live = settingsJSON;
  const target = settingsJSON.providers[1];
  doc.activeElement = body;
  scrolled.length = 0;
  m = await mountSettings({ name: 'settings', section: 'providers', device: target.id });
  const edit = fk(`set:prov:${target.id}:edit`);
  assert.ok(doc.activeElement === edit, `${target.name}: its row's edit button has the focus, not ${doc.activeElement.tagName}`);
  assert.ok(scrolled.includes(edit.closest('li.pv-row')), 'its row is scrolled to');
  assert.equal(presetDialogs().length, 0, 'no dialog');
  m.view.onChanged({ kind: 'settings' });
  fk('set:prov:add').focus();
  await settle(300);
  assert.ok(doc.activeElement === fk('set:prov:add'), 'handled once: a later redraw does not take the focus back');
  // the same while the page is open
  fk('set:prov:add').focus();
  m.view.update({ name: 'settings', section: 'providers', device: settingsJSON.providers[0].id });
  await settle(30);
  assert.ok(doc.activeElement === fk(`set:prov:${settingsJSON.providers[0].id}:edit`), 'while open: the other row');
  // an id that is no provider: the section's top
  m.view.update({ name: 'settings', section: 'providers', device: 'p-gone' });
  await settle(30);
  const title = m.page.querySelector('section.set-card').querySelector('h2.card-title');
  assert.ok(doc.activeElement === title && title.closest('section.set-card').getAttribute('id') === 'set-providers', `unknown: the providers section's title has the focus (${doc.activeElement.tagName} ${doc.activeElement.textContent})`);
  m.end();
}

// ---- WS-03 (R81 L6, M4's call site): files dropped on "update from file"
// that match no package of the update: the row says so in its own words,
// not the download's "download again" ----
{
  const SU = await import(new URL('settings_update.js', base).href);
  const file = 'LectureFlow-Setup-3.3.0-x64.exe';
  routes = {
    'GET /api/selfupdate': { state: 'idle', current_version: '3.2.0', platform: 'windows', kind: 'nsis' },
    'POST /api/selfupdate/import': () => ({ status: 422, body: { error: { code: 'selfupdate.checksum', params: { file }, detail: 'no windows package of update.json has them' } } }),
  };
  const shell = new El('div');
  body.append(shell);
  let sec = null;
  sec = SU.createUpdateSection({ formRow: (label, ...xs) => D.h('div', null, label, xs), rerender: () => sec && shell.replaceChildren(...sec.rows()) });
  sec.load();
  await settle(120);
  const zone = shell.querySelector('div.dropzone');
  assert.ok(zone, '"update from file" takes dropped files');
  zone.dispatchEvent(event('drop', { dataTransfer: { types: ['Files'], files: [new File(['MZ'], file)] } }));
  await until(() => sent('POST', '/api/selfupdate/import').length && shell.querySelector('div.su-err'), 'the refusal is shown');
  const err = shell.querySelector('div.su-err');
  assert.equal(err.getAttribute('role'), 'alert');
  assert.ok(err.textContent.includes(t('settings.update.file_checksum', { file })), `the row's own words for files that match no package (${err.textContent})`);
  assert.ok(!err.textContent.includes(t('errors.selfupdate.checksum', { file })), "not the download's text");
  assert.ok(err.textContent.includes('no windows package of update.json has them'), 'with the details');
  sec.destroy();
  shell.remove();
}

// ---- WS-08 (R75 L6): the guide's "transcription mode" step says the
// download starts after "continue" only when the chosen way has something
// to download ----
{
  const OM = await import(new URL('onboarding_mode.js', base).href);
  const HW = { hw: { ram_mb: 16384, disk_free_mb: 200000 }, recommendation: { components: ['core', 'asr-cuda', 'model-large-v3'], reasons: [], notes: [] } };
  const plan = (bytes) => ({ ids: [], download_bytes: bytes, install_bytes: bytes, free_bytes: 2e11, enough_space: true, need_bytes: bytes });
  for (const [what, mode, local, remote, shown] of [
    ['local, to download', 'local', 6.3e9, 0, true],
    ['local, all installed (the guide run again)', 'local', 0, 0, false],
    ['remote, gcloud to download', 'remote', 0, 1.2e8, true],
    ['remote, gcloud installed', 'remote', 6.3e9, 0, false],
  ]) {
    for (const storageShown of [false, true]) {
      routes = { 'POST /api/components/plan': (b) => ({ status: 200, body: plan(b.ids.includes('gcloud') ? remote : local) }) };
      const page = new El('div');
      body.append(page);
      let step = null;
      const env = { cache: { hardware: HW }, data: { components: ['asr-cuda'] }, storageShown, modeHint: mode, alive: () => true, rerender: () => step && page.replaceChildren(D.h('div', null, step.body())), save: async () => {}, setData: async () => {}, next() {} };
      step = OM.asrModeStep(env);
      await settle(120);
      env.rerender();
      const key = storageShown ? 'onboarding.asr_mode.download_after_storage' : 'onboarding.asr_mode.download_after';
      assert.equal(page.textContent.includes(t(key)), shown, `${what} (data folder step ${storageShown}): "the download starts after continue" ${shown ? 'is' : 'is not'} said`);
      step.destroy();
      page.remove();
    }
  }
}

// ---- WS-06 (R80 low-3): an outline over 256 KiB is rendered in slices;
// drawn again, the place scrolled to in the preview was held to the first
// slice's height and did not come back. "Preview" pressed again (or a save
// that stays) keeps the place ----
const Wz = await import(new URL('wizard.js', base).href);
{
  const unit = [
    '# CSC0000 Example — Week {i}',
    '',
    'This week: **graphs**, *paths* and `bfs`. See [notes](https://example.org/{i}).',
    '',
    '1. Search',
    '   - pre-order',
    '2. Sort',
    '',
    '> Quiz on _Friday_ &mdash; bring a pen.',
    '',
  ].join('\n');
  let md = '';
  for (let i = 1; md.length < 600 * 1024; i++) md += unit.replaceAll('{i}', String(i)) + '\n';
  const course = { id: 'c1', name: 'Example course', code: 'CSC0000', stages: [] };
  routes = { 'GET /api/courses/c1/syllabus': { raw: 'the raw text', markdown: md, filename: 'syllabus.pdf' } };
  Wz.openSyllabusEditor(course);
  const isMark = (n) => n instanceof Text && n.data === '';
  let box = null;
  let preview = null;
  await until(() => (box = doc.querySelector('div.syl-md-body')) && (preview = box.querySelector('div.syl-preview')) && preview.children.length && !isMark(preview.lastChild), 'the outline is rendered to its end');
  assert.ok(box.scrollHeight > 20000, `a long outline (${box.scrollHeight} px)`);
  const bottom = box.scrollHeight - box.clientHeight;
  box.scrollTop = bottom; // read to its end
  assert.equal(box.scrollTop, bottom, 'scrolled to the end');
  const first = preview.firstChild;
  const segPreview = doc.querySelectorAll('button.syl-seg-btn').find((b) => b.textContent === t('wizard.format.preview'));
  assert.ok(segPreview, 'the "preview" button');
  segPreview.click();
  // laid out between the slices, as a browser does every frame (the place
  // is held to the height there is then)
  for (let k = 0; k < 3 || isMark(preview.lastChild); k++) {
    if (k > 100000) throw new Error('the rendering did not finish');
    void box.scrollTop;
    await new Promise((r) => setTimeout(r, 0));
  }
  await settle(50);
  assert.equal(box.scrollTop, bottom, 'the place scrolled to is kept');
  assert.ok(preview.firstChild === first, 'the same outline is not drawn again');
  doc.querySelector('button.wiz-close').click();
  await settle(60);
  assert.equal(doc.querySelector('dialog.syl-dlg'), null, 'the editor closed');
}

// ---- WS-07 (R74 info, low 9): the new-course wizard's slides step says
// what slides are for; its last step says term extraction runs in the
// background only while slides that can be extracted (uploaded, or still
// uploading) or a syllabus are there, not for slides that failed ----
{
  const xhrs = [];
  globalThis.XMLHttpRequest = class {
    constructor() {
      this.upload = {};
      this.status = 0;
      this.responseText = '';
      xhrs.push(this);
    }
    open() {}
    send() {}
    setRequestHeader() {}
    abort() {
      if (this.onabort) this.onabort();
    }
  };
  const Up = await import(new URL('uploads.js', base).href);
  const descOf = (dlg) => dlg.querySelector('p.wiz-desc').textContent;
  const stepOf = (dlg) => dlg.querySelector('div.wiz-body').dataset.step;
  // a new course (no syllabus: skipped) and the slides step
  async function toSlides(id) {
    const course = { id, name: 'Wizard course', code: '', stages: [], materials: [] };
    routes = { 'POST /api/courses': (b) => ({ status: 201, body: { ...course, name: b.name } }), [`GET /api/courses/${id}`]: course };
    Wz.openWizard({ courses: () => [], refreshCourses: async () => {} });
    await settle(30);
    const dlg = doc.querySelector('dialog.wiz-add');
    const name = dlg.querySelector('input[id="wiz-name"]');
    name.value = 'Wizard course';
    name.dispatchEvent(event('input'));
    fk('wiz:primary').click();
    await until(() => stepOf(dlg) === '2', 'the syllabus step');
    dlg.querySelectorAll('input').find((x) => x.getAttribute('type') === 'radio' && x.value === 'skip').dispatchEvent(event('change'));
    fk('wiz:primary').click();
    await until(() => stepOf(dlg) === '4', 'the slides step');
    return dlg;
  }
  const dropPDF = (dlg, fname) => dlg.querySelector('div.dropzone').dispatchEvent(event('drop', { dataTransfer: { types: ['Files'], files: [new File(['%PDF-1.7'], fname, { type: 'application/pdf' })] } }));
  const close = async (dlg) => {
    dlg.querySelector('button.wiz-close').click();
    await settle(60);
    for (const d of openDialogs()) d.dispatchEvent(event('cancel', { bubbles: false }));
    await settle(30);
  };

  // every slide file failed to upload, no syllabus: no extraction promised
  Up.setUploadGuard(() => 'refused here');
  let dlg = await toSlides('c-wz1');
  assert.equal(descOf(dlg), t('wizard.materials.desc_week'), 'the slides step says what slides are for');
  dropPDF(dlg, 'week1.pdf');
  dropPDF(dlg, 'week2.pdf');
  await settle(60);
  fk('wiz:primary').click();
  await until(() => stepOf(dlg) === '5', 'the last step');
  assert.equal(descOf(dlg), t('wizard.done.desc_no_terms'), 'all slides failed, no syllabus: no term extraction said');
  await close(dlg);

  // a slide file still uploading: it is extracted once it is in; it fails
  // while the step is open: the sentence follows
  Up.setUploadGuard(null);
  dlg = await toSlides('c-wz2');
  dropPDF(dlg, 'week1.pdf');
  await until(() => xhrs.length > 0, 'the upload started');
  fk('wiz:primary').click();
  await until(() => stepOf(dlg) === '5', 'the last step');
  assert.equal(descOf(dlg), t('wizard.done.desc'), 'a slide file on its way: term extraction is said');
  xhrs.at(-1).onerror();
  await settle(60);
  assert.equal(descOf(dlg), t('wizard.done.desc_no_terms'), 'it failed: no term extraction said any more');
  await close(dlg);
  clearToasts();
}

// ---- WS-04 (R81 L8): a component task's view (components_ui.js taskView)
// is not taken out and put back when it is drawn again: its status line
// changes its text in place, and a failure's alert is put in once ----
const CU = await import(new URL('components_ui.js', base).href);
const ctask = (state, extra = {}) => ({ id: 'tk1', kind: 'install', state, ids: ['asr-cuda'], total_bytes: 100, done_bytes: 40, speed_bps: 10, eta_s: 6, items: [], ...extra });
const netErr = { code: 'components.network', params: {} };
{
  let busy = false;
  const view = CU.taskView({ components: () => [], busy: () => busy, onAction() {} });
  const page = new El('div');
  body.append(page);
  page.append(view.el);
  const line = view.el.querySelector('p.visually-hidden');
  view.update(ctask('failed', { error: netErr }));
  const alert = view.el.querySelector('div.cu-error');
  assert.ok(alert && alert.getAttribute('role') === 'alert', 'a failure: its alert');
  watched.set(alert, { detached: 0 });
  view.update(ctask('failed', { error: netErr }));
  busy = true;
  view.update(ctask('failed', { error: netErr, done_bytes: 41 }));
  assert.ok(view.el.querySelector('div.cu-error') === alert, 'updates while it lasts: the same alert');
  assert.equal(watched.get(alert).detached, 0, 'never taken out and put back');
  assert.ok(alert.querySelectorAll('button').every((b) => b.disabled), 'its buttons follow busy() in place');
  busy = false;
  view.update(ctask('failed', { error: netErr }));
  assert.ok(alert.querySelectorAll('button').every((b) => !b.disabled), 'and back');
  assert.ok(view.el.children.indexOf(alert) > view.el.children.indexOf(view.el.querySelector('div.cu-task-head')), 'below the state and the bar');
  watched.delete(alert);
  // another failure: its own alert, put in once
  view.update(ctask('failed', { error: { code: 'components.checksum', params: {} } }));
  const alert2 = view.el.querySelector('div.cu-error');
  assert.ok(alert2 && alert2 !== alert, 'another failure: a new alert');
  view.update(ctask('running'));
  assert.equal(view.el.querySelector('div.cu-error'), null, 'running again: no alert');
  assert.ok(view.el.querySelector('p.visually-hidden') === line, 'the same status line all along');
  page.remove();
}

// the guide's "download" step: the task view and its status line stay where
// they are through the step's redraws (state changes, an action), and the
// failure's alert is put in once
{
  const O = await import(new URL('onboarding.js', base).href);
  routes = {
    'GET /api/onboarding': { step: 'download', done: false, data: { components: ['asr-cuda'], task_id: 'tk1' } },
    'PUT /api/onboarding': (b) => ({ status: 200, body: b }),
    'GET /api/components': { components: [{ id: 'asr-cuda', state: 'available' }], tasks: [ctask('running')] },
    'GET /api/components/tasks': [ctask('running')],
    'GET /api/hardware': { hw: {}, recommendation: { components: [], reasons: [], notes: [] } },
    'GET /api/components/sources': SOURCES,
  };
  const main = new El('main');
  body.append(main);
  const ob = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
  await until(() => main.querySelector('div.cu-task'), 'the task view');
  const el = main.querySelector('div.cu-task');
  const parent = el.parentNode;
  const line = el.querySelector('p.visually-hidden');
  watched.set(el, { detached: 0 });
  assert.equal(line.textContent, t('compui.task.state.running'));
  // paused (a state change: the step is drawn again)
  routes['GET /api/components/tasks'] = [ctask('paused')];
  await until(() => line.textContent === t('compui.task.state.paused'), 'paused is told', 400);
  await settle(30);
  assert.ok(main.querySelector('div.cu-task') === el && el.parentNode === parent, 'redrawn: the same task view in the same place');
  assert.ok(el.querySelector('p.visually-hidden') === line, 'with the same status line');
  // failed
  routes['GET /api/components/tasks'] = [ctask('failed', { error: netErr })];
  await until(() => el.querySelector('div.cu-error'), 'the failure is shown', 400);
  const alert = el.querySelector('div.cu-error');
  watched.set(alert, { detached: 0 });
  // "retry" fails again at once: the step is drawn again, still failed
  routes['POST /api/components/tasks/tk1/retry'] = { task: ctask('failed', { error: netErr }) };
  fk('cu:task:retry').click();
  await until(() => sent('POST', '/api/components/tasks/tk1/retry').length, 'retry asked for');
  await settle(80);
  assert.ok(el.querySelector('div.cu-error') === alert, 'still failed after a redraw: the same alert');
  assert.equal(watched.get(alert).detached, 0, 'the alert was put in once');
  assert.equal(watched.get(el).detached, 0, 'the task view was never taken out and put back');
  assert.ok(main.querySelector('div.cu-task') === el && el.parentNode === parent, 'and is where it was');
  watched.clear();
  ob.destroy();
  main.remove();
}

// the guide's remote branch: the Google Cloud CLI's install in the sign-in
// step (colab_login.js) stays where it is through the step's redraws
{
  const O = await import(new URL('onboarding.js', base).href);
  const gtask = (state, extra = {}) => ({ ...ctask(state, extra), id: 'tg1', ids: ['gcloud'] });
  routes = {
    'GET /api/onboarding': { step: 'download', done: false, data: { asr_mode: 'remote', gcloud_task_id: 'tg1' } },
    'PUT /api/onboarding': (b) => ({ status: 200, body: b }),
    'GET /api/colab': { status: { state: 'idle' }, adc: { present: false } },
    'GET /api/colab/auth/login': { state: 'idle' },
    'GET /api/components': { components: [{ id: 'gcloud', state: 'available' }], tasks: [gtask('running')] },
    'GET /api/components/tasks': [gtask('running')],
    'GET /api/components/sources': SOURCES,
  };
  const main = new El('main');
  body.append(main);
  const ob = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
  await until(() => main.querySelector('div.cu-task'), 'the install view');
  const el = main.querySelector('div.cu-task');
  const parent = el.parentNode;
  const line = el.querySelector('p.visually-hidden');
  watched.set(el, { detached: 0 });
  routes['GET /api/components/tasks'] = [gtask('paused')];
  await until(() => line.textContent === t('compui.task.state.paused'), 'paused is told', 400);
  await settle(30);
  assert.ok(main.querySelector('div.cu-task') === el && el.parentNode === parent, 'redrawn: the same install view in the same place');
  assert.equal(watched.get(el).detached, 0, 'never taken out and put back');
  watched.clear();
  ob.destroy();
  main.remove();
}

// ---- C179b: components.redirect_refused (the source sent the download to
// an address LectureFlow does not follow; a retry does the same): no "you
// can retry" suggestion, "change the download source" is the way out, not
// a retry — in a task view and in the guide's download step ----
{
  const refused = { code: 'components.redirect_refused', params: { host: 'mirror.example', file: 'asr.whl' }, detail: 'redirect to http://mirror.example/asr.whl refused' };
  const check = (root, what) => {
    const alert = root.querySelector('div.cu-error');
    assert.ok(alert, `${what}: the failure is shown`);
    assert.ok(!alert.textContent.includes(t('compui.err.generic_todo')), `${what}: no "you can retry" suggestion`);
    assert.equal(alert.querySelector('p.cu-error-todo'), null, `${what}: no empty suggestion line`);
    assert.equal(alert.querySelector('[data-fkey="cu:task:retry"]'), null, `${what}: no retry button`);
    const src = alert.querySelector('[data-fkey="cu:task:source"]');
    assert.ok(src && src.className.includes('btn-primary'), `${what}: "change the download source" is the main action`);
  };
  const view = CU.taskView({ components: () => [], onAction() {} });
  const page = new El('div');
  body.append(page);
  page.append(view.el);
  view.update(ctask('failed', { error: refused }));
  check(view.el, 'task view');
  // the other failures as before: retry first, with their suggestion
  view.update(ctask('failed', { error: netErr }));
  const a = view.el.querySelector('div.cu-error');
  assert.ok(a.querySelector('[data-fkey="cu:task:retry"]').className.includes('btn-primary'), 'a network failure: retry is the main action');
  assert.ok(a.querySelector('p.cu-error-todo').textContent === t('compui.err.network_todo'), 'with its suggestion');
  view.update(ctask('failed', { error: { code: 'components.something_else', params: {} } }));
  assert.ok(view.el.querySelector('p.cu-error-todo').textContent === t('compui.err.generic_todo'), 'another code: the general suggestion');
  page.remove();

  const O = await import(new URL('onboarding.js', base).href);
  routes = {
    'GET /api/onboarding': { step: 'download', done: false, data: { components: ['asr-cuda'], task_id: 'tk1' } },
    'PUT /api/onboarding': (b) => ({ status: 200, body: b }),
    'GET /api/components': { components: [{ id: 'asr-cuda', state: 'available' }], tasks: [ctask('failed', { error: refused })] },
    'GET /api/components/sources': SOURCES,
  };
  const main = new El('main');
  body.append(main);
  const ob = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
  await until(() => main.querySelector('div.cu-error'), 'the failure in the guide');
  check(main, "the guide's download step");
  ob.destroy();
  main.remove();
}

console.log('r8 settings checks: ok');
process.exit(0);
