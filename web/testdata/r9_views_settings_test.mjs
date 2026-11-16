// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 9, the r9-web-views line, its settings side (the settings page, the
// setup guide, the syllabus editor); each section names its item (WV-06 …).
// The DOM stand-in is r8_settings_test.mjs's (focus as a browser moves it
// where it matters; nodes watched for being taken out of their place).
// web/r9_views_test.go starts it with the web directory as a file: URL.
// Nodes are compared by identity (assert.ok(a === b)), never with
// assert.equal (testdata/README.md). Every wait is bounded (until(): tries).
// No API key is typed into any field here.

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

// ---- what these checks ask of the stand-in beyond r8_settings_test.mjs's ----
{
  // a real Event (provider_limits.js fillAdvice dispatches one) as the
  // stand-in's plain event: its target is set on the way
  const dispatch = El.prototype.dispatchEvent;
  El.prototype.dispatchEvent = function (ev) {
    if (typeof Event === 'function' && ev instanceof Event) ev = event(ev.type, { bubbles: ev.bubbles });
    return dispatch.call(this, ev);
  };
}

// ---- WV-06 (T28 suggestion 5): the provider dialog asks "放弃修改？"
// before Esc, a click on the backdrop or "取消" drop a change ----
{
  routes = { 'GET /api/settings': settingsJSON, 'GET /api/onboarding': { step: 'done', done: true, data: {} } };
  const m = await mountSettings();
  const ds = settingsJSON.providers.find((p) => p.id === 'deepseek');
  const anth = settingsJSON.providers.find((p) => p.type === 'anthropic' && p.rate_limits);
  const providerDlg = () => openDialogs().find((d) => d.className.includes('dlg-provider')) || null;
  const questions = () => openDialogs().filter((d) => d.getAttribute('aria-label') === t('settings.provider.discard_title'));
  const esc = (d) => d.dispatchEvent(event('cancel', { bubbles: false }));
  // a press outside the dialog's box (the stand-in's box is empty)
  const backdrop = (d) => d.dispatchEvent(event('mousedown', { clientX: 5, clientY: 5 }));
  const open = async (fkey) => {
    fk(fkey).click();
    await settle(60);
    const d = providerDlg();
    assert.ok(d && d.open, `${fkey}: the provider dialog`);
    return d;
  };
  const urlField = (d) => d.querySelectorAll('input').find((x) => x.getAttribute('type') === 'url');
  const type = (input, v) => {
    input.value = v;
    input.dispatchEvent(event('input'));
  };
  const patches = () => calls.filter((c) => c.method === 'PATCH' || (c.method === 'POST' && c.path === '/api/providers'));
  const sentBefore = patches().length;

  // unchanged: Esc, the backdrop and "取消" close it at once
  for (const [what, how] of [
    ['Esc', esc],
    ['the backdrop', backdrop],
    ['"取消"', (d) => press(d, t('common.cancel'))],
  ]) {
    const d = await open(`set:prov:${ds.id}:edit`);
    how(d);
    await settle(60);
    assert.ok(!d.open, `unchanged, ${what}: closed`);
    assert.equal(questions().length, 0, `unchanged, ${what}: without asking`);
  }

  // the address changed: Esc asks first
  let d = await open(`set:prov:${ds.id}:edit`);
  const base = urlField(d);
  base.focus();
  type(base, 'https://api.deepseek.example/v1');
  esc(d);
  await settle(60);
  assert.equal(questions().length, 1, 'changed, Esc: asked first');
  let q = questions()[0];
  assert.ok(q.textContent.includes(t('settings.provider.discard_message')), 'that what was entered is not saved');
  assert.ok(d.open, 'the dialog stays open while it asks');
  esc(d);
  backdrop(d);
  await settle(30);
  assert.equal(questions().length, 1, 'Esc or the backdrop again while it asks: still one question');
  // "继续编辑": back in the dialog, the change kept, the focus where it was
  press(q, t('wizard.close.keep'));
  await settle(60);
  assert.ok(!q.open && d.open, '"继续编辑": the dialog stays open');
  assert.ok(doc.activeElement === base, `the focus is back on the address field (${doc.activeElement.tagName})`);
  assert.equal(base.value, 'https://api.deepseek.example/v1', 'the change is kept');
  // the backdrop asks too; "放弃并关闭" closes it, nothing saved
  backdrop(d);
  await settle(60);
  assert.equal(questions().length, 1, 'changed, the backdrop: asked first');
  q = questions()[0];
  press(q, t('wizard.close.discard'));
  await settle(60);
  assert.ok(!q.open && !d.open, '"放弃并关闭": closed');
  assert.equal(patches().length, sentBefore, 'nothing saved');

  // "取消" asks the same once changed; changed back, it closes at once
  d = await open(`set:prov:${ds.id}:edit`);
  const nameField = d.querySelectorAll('input')[0];
  assert.equal(nameField.value, ds.name, 'the name field');
  type(nameField, `${ds.name} 2`);
  press(d, t('common.cancel'));
  await settle(60);
  assert.equal(questions().length, 1, 'changed, "取消": asked first');
  press(questions()[0], t('wizard.close.keep'));
  await settle(60);
  assert.ok(d.open, '"继续编辑": still open');
  type(nameField, ds.name);
  press(d, t('common.cancel'));
  await settle(60);
  assert.ok(!d.open && questions().length === 0, 'changed back: closed without asking');

  // a new provider: the address and limits its type fills in are no change
  d = await open('set:prov:add');
  esc(d);
  await settle(60);
  assert.ok(!d.open && questions().length === 0, 'a new provider untouched: Esc closes it');
  // its type switched: a change
  d = await open('set:prov:add');
  const radio = d.querySelector('[data-fkey="dlg-ptype:anthropic"]');
  radio.checked = true;
  radio.dispatchEvent(event('change'));
  esc(d);
  await settle(60);
  assert.equal(questions().length, 1, 'the type switched: asked first');
  press(questions()[0], t('wizard.close.discard'));
  await settle(60);
  assert.ok(!d.open, 'discarded');

  // opened with the suggested limits filled in (the row's banner): no
  // change of the user's, Esc closes it at once
  d = await open(`set:prov:${anth.id}:rate_limits`);
  const nums = d.querySelectorAll('input').filter((x) => x.getAttribute('type') === 'number');
  assert.equal(nums[0].value, String(anth.rate_limits.advice.max_concurrency), 'opened with the suggestion filled in');
  assert.notEqual(nums[0].value, String(anth.max_concurrency), '(which differs from what is saved)');
  esc(d);
  await settle(60);
  assert.ok(!d.open && questions().length === 0, 'the suggestion filled in on opening: Esc closes it without asking');
  // the same suggestion filled in by "填入建议值" in the dialog: a change
  d = await open(`set:prov:${anth.id}:edit`);
  fk('set:prov:limits_fill').click();
  esc(d);
  await settle(60);
  assert.equal(questions().length, 1, '"填入建议值" pressed in the dialog: asked first');
  press(questions()[0], t('wizard.close.discard'));
  await settle(60);
  assert.ok(!d.open, 'discarded');
  assert.equal(patches().length, sentBefore, 'nothing was saved all along');

  // R91 low-2: a key typed in, nothing else: a change (asked first). The
  // key is an obviously made-up test value; no attribute or text holds it.
  const FAKE_KEY = 'not-a-real-key-test-value';
  d = await open(`set:prov:${ds.id}:edit`);
  type(d.querySelector('input[id="pv-key"]'), FAKE_KEY);
  esc(d);
  await settle(60);
  assert.equal(questions().length, 1, 'only a key typed: asked first');
  const elements = (n, out = []) => {
    for (const c of n.children || []) {
      if (c.attrs) out.push(c);
      elements(c, out);
    }
    return out;
  };
  const holds = (root) => root.textContent.includes(FAKE_KEY) || elements(root).some((el) => Object.values(el.attrs).some((v) => String(v).includes(FAKE_KEY)) || Object.values(el.dataset || {}).some((v) => String(v).includes(FAKE_KEY)));
  assert.ok(!holds(d) && !holds(questions()[0]), 'the key is in no text or attribute');
  press(questions()[0], t('wizard.close.discard'));
  await settle(60);
  assert.ok(!d.open, 'discarded');

  // R91 low-2: while the save runs, Esc closes at once (no question; the
  // request goes on, as before), although the fields differ
  let release = null;
  routes[`PATCH /api/providers/${ds.id}`] = () => new Promise((r) => (release = () => r({ status: 200, body: { ...ds, base_url: 'https://api.deepseek.example/v2' } })));
  d = await open(`set:prov:${ds.id}:edit`);
  type(urlField(d), 'https://api.deepseek.example/v2');
  d.querySelector('form').dispatchEvent(event('submit'));
  await until(() => release, 'the save is sent and runs');
  esc(d);
  await settle(60);
  assert.ok(!d.open, 'saving: Esc closes it');
  assert.equal(questions().length, 0, 'saving: without asking');
  release();
  await settle(150);
  assert.equal(questions().length, 0, 'nothing asked after the save came back');
  delete routes[`PATCH /api/providers/${ds.id}`];
  m.end();
}

// ---- WV-07 (R87 S-L2): the provider dialog's live notes (plain http to
// another computer; the key that a new host clears) change only when
// what they say changes, not on every key typed ----
{
  routes = { 'GET /api/settings': settingsJSON, 'GET /api/onboarding': { step: 'done', done: true, data: {} } };
  const m = await mountSettings();
  const ds = settingsJSON.providers.find((p) => p.id === 'deepseek');
  const type = (input, v) => {
    input.value = v;
    input.dispatchEvent(event('input'));
  };
  const typeChars = (input, chars) => {
    for (const c of chars) type(input, input.value + c);
  };
  // counts the replaceChildren of el
  const counted = (el) => {
    const real = el.replaceChildren;
    el.replaced = 0;
    el.replaceChildren = function (...cs) {
      el.replaced++;
      return real.apply(this, cs);
    };
    return el;
  };
  fk(`set:prov:${ds.id}:edit`).click();
  await settle(60);
  const d = openDialogs().at(-1);
  const base = d.querySelectorAll('input').find((x) => x.getAttribute('type') === 'url');
  const plain = counted(d.querySelector('span[id="pv-url-plain"]'));
  const moved = counted(d.querySelector('span[id="pv-key-moved"]'));
  assert.ok(plain.getAttribute('role') === 'status' && moved.getAttribute('role') === 'status', 'both are live regions');
  const describes = () => (base.getAttribute('aria-describedby') || '').split(' ').includes('pv-url-plain');
  // http to another computer: the note shows, once
  type(base, 'http://example.com');
  assert.ok(!plain.hidden && plain.textContent.includes(t('settings.provider.plain_http_note')) && describes(), 'http://example.com: the note shows and describes the field');
  assert.ok(!moved.hidden && moved.textContent === t('settings.provider.key_moved_hint'), 'another host: the stored key is cleared on save, said');
  const plainKids = [...plain.children];
  const movedText = moved.firstChild;
  const n = plain.replaced;
  const nm = moved.replaced;
  typeChars(base, '/v1/x');
  assert.equal(plain.replaced, n, 'five more characters, still far: the note is not replaced');
  assert.ok(plain.children.length === plainKids.length && plain.children.every((c, i) => c === plainKids[i]), 'the same nodes in it');
  assert.ok(!plain.hidden && describes(), 'still shown, still describing the field');
  assert.equal(moved.replaced, nm, 'the key note is not replaced either');
  assert.ok(moved.firstChild === movedText, 'its text node is the same');
  // https: gone; http again: back (once each)
  type(base, 'https://example.com/v1/x');
  assert.ok(plain.hidden && !plain.textContent && !describes(), 'https://example.com: the note goes');
  assert.equal(plain.replaced, n + 1, 'changed once');
  typeChars(base, 'yz');
  assert.equal(plain.replaced, n + 1, 'more typed, still https: unchanged');
  type(base, 'http://example.com/v1/x');
  assert.ok(!plain.hidden && plain.textContent.includes(t('settings.provider.plain_http_note')) && describes(), 'http again: back');
  assert.equal(plain.replaced, n + 2, 'changed once more');
  // back to the provider's own address: the key note goes
  type(base, ds.base_url);
  assert.ok(moved.hidden && moved.textContent === '', 'its own host again: the key note goes');
  // closed unsaved (WV-06 asks: the address was changed and back, so no)
  press(d, t('common.cancel'));
  await settle(60);
  assert.ok(!d.open, 'closed');
  m.end();
}

// ---- WV-08 (R87 S-L3): the syllabus editor's preview shows the outline
// as it is now: drawn again after an edit, and after a format (round 8
// WS-06 checked only that the same outline is not drawn again) ----
{
  const Wz = await import(new URL('wizard.js', base).href);
  const course = { id: 'c-wv08', name: 'Example course', code: 'CSC0008', stages: [] };
  routes = {
    'GET /api/courses/c-wv08/syllabus': { raw: 'the raw text', markdown: '# Week 1\n\nthe first version', filename: 'syllabus.pdf' },
    'POST /api/courses/c-wv08/syllabus/format': () => ({ status: 200, body: { markdown: '# Formatted\n\nfresh from the model', usage: null } }),
  };
  Wz.openSyllabusEditor(course);
  let preview = null;
  await until(() => (preview = doc.querySelector('div.syl-preview')) && preview.textContent.includes('the first version'), 'the preview shows the outline');
  const seg = (label) => doc.querySelectorAll('button.syl-seg-btn').find((b) => b.textContent === label);
  // edited, then "preview": the edited outline
  seg(t('common.edit')).click();
  await settle(30);
  const editor = doc.querySelector('textarea.syl-editor');
  editor.value = '# Week 1\n\nthe second version, edited';
  editor.dispatchEvent(event('input'));
  seg(t('wizard.format.preview')).click();
  await settle(30);
  assert.ok(doc.querySelector('div.syl-preview') === preview && !preview.hidden, 'the preview is shown');
  assert.ok(preview.textContent.includes('the second version, edited'), 'edited, then "preview": the edited outline is shown');
  assert.ok(!preview.textContent.includes('the first version'), 'not the outline from before');
  // formatted again (the edits are unsaved: it asks first): the model's outline
  const regen = doc.querySelector('span.syl-pane-tools').querySelectorAll('button').find((b) => b.textContent.includes(t('wizard.format.regenerate')));
  assert.ok(regen, 'the "re-format" button');
  regen.click();
  await settle(60);
  press(openDialogs().at(-1), t('wizard.format.regenerate'));
  await until(() => calls.some((c) => c.method === 'POST' && c.path === '/api/courses/c-wv08/syllabus/format'), 'formatted');
  await until(() => preview.textContent.includes('fresh from the model'), 'the formatted outline is shown');
  assert.ok(!preview.textContent.includes('the second version'), 'formatted: not the edited outline from before');
  // closed (the formatted outline is unsaved: discarded)
  doc.querySelector('button.wiz-close').click();
  await settle(60);
  const lost = openDialogs().find((x) => x.getAttribute('aria-label') === t('wizard.close.title'));
  if (lost) press(lost, t('wizard.close.discard'));
  await settle(60);
  assert.equal(doc.querySelector('dialog.syl-dlg'), null, 'the editor closed');
}

// ---- WV-10 (R87 S-R1, R81 L8): Settings → Components and Settings →
// Colab keep a task's view where it is when they are drawn again: its
// status line and its alert are never taken out and put back (the
// guide's two places were done in round 8, WS-04) ----

const ctask = (id, ids, state, extra = {}) => ({ id, kind: 'install', state, ids, total_bytes: 100, done_bytes: 40, speed_bps: 10, eta_s: 6, items: [], ...extra });
const netErr = { code: 'components.network', params: {} };
// GET /api/colab of a Colab not signed in (the mock's colab_off)
const colabOff = {
  status: { service: 'colab', configured: false, auth: { present: false, remaining_sec: 0 }, accelerator: 'T4', tier: 'free', state: 'none', window_used_sec: 0, window_total_sec: 14400, lectures_left: 4, planned: 0, planned_fit: 0, slots: 1, uploaded_ahead: 0, cu_per_hour: 1.07, plan_cost_cu: 0, session_spent_cu: 0, shutdown_after_current: false, account: { read_at: null, read_ok: false, level: 'free', paid_balance_cu: null, account_rate_cu_per_hour: null, account_runtimes: null, free_quota_cu: null, free_refill_at: null, eligible_gpus: [], ineligible_gpus: [], eligibility_known: false }, selectable_accelerators: ['T4'], preferred_accelerator: '', effective_accelerator: 'T4', locked_reason: '' },
  settings: { accelerator: 'T4', tier: 'free', max_queue_wait_min: 30, model: 'large-v3', compute_type: 'float16', idle_release_min: 5, window_cooldown_min: 60, high_mem: false, memory_mode: true, enabled: false, battery_prefer: false },
  adc: { detected: false, path: 'application_default_credentials.json', found: false, problem: 'colab.adc_not_found', login_command: 'gcloud auth application-default login' },
  held: false,
};
// The settings changed elsewhere ("changed settings" with other settings):
// the settings page draws every block again (setData → render). drawnAnew
// is a part the block draws anew each time: proof that it was drawn.
let bumps = 0;
async function redrawPage(m, sec, drawnAnew, what) {
  const before = sec.querySelector(drawnAnew);
  bumps++;
  routes['GET /api/settings'] = { ...settingsJSON, general: { ...settingsJSON.general, daily_budget: Number(settingsJSON.general.daily_budget) + bumps } };
  m.view.onChanged({ kind: 'settings' });
  await until(() => sec.querySelector(drawnAnew) && sec.querySelector(drawnAnew) !== before, `${what}: the block drawn again`, 300);
  await settle(100);
}
const settingsRoutes = () => ({
  'GET /api/settings': settingsJSON,
  'GET /api/onboarding': { step: 'done', done: true, data: {} },
  'GET /api/components/sources': SOURCES,
  'GET /api/colab': colabOff,
  'GET /api/colab/auth/login': { state: 'idle' },
});

// Settings → Components
{
  const comps = [{ id: 'asr-cuda', state: 'available', kind: 'runtime', version: '1.0', install_bytes: 100 }];
  const serve = (state, extra) => {
    const tk = ctask('tk1', ['asr-cuda'], state, extra);
    routes['GET /api/components'] = { components: comps, tasks: [tk] };
    routes['GET /api/components/tasks'] = [tk];
  };
  routes = settingsRoutes();
  serve('running');
  const m = await mountSettings();
  const sec = m.page.querySelector('section[id="set-components"]');
  await until(() => sec.querySelector('div.cu-task'), 'the task view', 400);
  const el = sec.querySelector('div.cu-task');
  const card = el.parentNode;
  assert.ok(card && card.className.includes('comp-task'), 'in its card');
  const line = el.querySelector('p.visually-hidden');
  watched.set(el, { detached: 0 });
  const same = (what) => {
    assert.ok(sec.querySelector('div.cu-task') === el && el.parentNode === card && card.isConnected, `${what}: the same task view in the same card`);
    assert.ok(el.querySelector('p.visually-hidden') === line, `${what}: with the same status line`);
    assert.equal(watched.get(el).detached, 0, `${what}: never taken out and put back`);
  };
  // the whole page drawn again (the settings changed elsewhere)
  await redrawPage(m, sec, 'div.comp-form', 'components');
  same('the page drawn again');
  // paused (a state change: the section drawn again)
  serve('paused');
  await until(() => line.textContent === t('compui.task.state.paused'), 'paused is told', 400);
  await settle(250);
  same('paused');
  // failed: its alert put in once and kept through the redraws
  serve('failed', { error: netErr });
  await until(() => el.querySelector('div.cu-error'), 'the failure is shown', 400);
  await settle(250);
  const alert = el.querySelector('div.cu-error');
  watched.set(alert, { detached: 0 });
  same('failed');
  // a control in it keeps the focus through a redraw
  const retry = fk('cu:task:retry');
  retry.focus();
  await redrawPage(m, sec, 'div.comp-form', 'components, failed');
  assert.ok(doc.activeElement === retry, `the page drawn again: the focus stays on "重试" (${doc.activeElement.tagName})`);
  same('failed, the page drawn again');
  assert.equal(watched.get(alert).detached, 0, 'the alert stays where it is');
  // "重试" fails again at once: the section drawn again, still failed
  routes['POST /api/components/tasks/tk1/retry'] = { task: ctask('tk1', ['asr-cuda'], 'failed', { error: netErr }) };
  retry.click();
  await until(() => sent('POST', '/api/components/tasks/tk1/retry').length, 'retry asked for');
  await settle(300);
  assert.ok(el.querySelector('div.cu-error') === alert, 'still failed after the redraws: the same alert');
  assert.equal(watched.get(alert).detached, 0, 'the alert was put in once');
  same('retried');
  // done: "隐藏" offered in the card's head; hidden, the card goes (as before)
  routes['POST /api/components/tasks/tk1/retry'] = { task: ctask('tk1', ['asr-cuda'], 'done') };
  serve('done');
  fk('cu:task:retry').click();
  await until(() => fk('comp:task:tk1:hide'), 'done: "隐藏" offered', 400);
  same('done');
  assert.ok(card.querySelector('div.comp-task-hd').contains(fk('comp:task:tk1:hide')), 'in the card\'s head');
  fk('comp:task:tk1:hide').click();
  await settle(100);
  assert.equal(sec.querySelector('div.cu-task'), null, 'hidden: the card goes');
  watched.clear();
  m.end();
}

// Settings → Colab: the Google Cloud CLI's install (colab_login.js gcloudBlock)
{
  const serve = (state, extra) => {
    const tk = ctask('tg1', ['gcloud'], state, extra);
    routes['GET /api/components'] = { components: [{ id: 'gcloud', state: 'available', kind: 'tool', version: '1.0', download_bytes: 1000 }], tasks: [tk] };
    routes['GET /api/components/tasks'] = [tk];
  };
  routes = settingsRoutes();
  serve('running');
  const m = await mountSettings();
  const sec = m.page.querySelector('section[id="set-colab"]');
  await until(() => sec.querySelector('div.cu-task'), 'the install view in the Colab section', 400);
  const el = sec.querySelector('div.cu-task');
  const parent = el.parentNode;
  assert.ok(parent && parent.className.includes('colab-login'), 'in the sign-in block');
  const line = el.querySelector('p.visually-hidden');
  watched.set(el, { detached: 0 });
  const same = (what) => {
    assert.ok(sec.querySelector('div.cu-task') === el && el.parentNode === parent && parent.isConnected, `${what}: the same install view in the same place`);
    assert.ok(el.querySelector('p.visually-hidden') === line, `${what}: with the same status line`);
    assert.equal(watched.get(el).detached, 0, `${what}: never taken out and put back`);
  };
  // the page and the section drawn again (the settings changed: Colab read again)
  await redrawPage(m, sec, 'div.colab-privacy', 'Colab');
  same('the page drawn again');
  // paused (the sign-in follows the task: the section drawn again)
  serve('paused');
  await until(() => line.textContent === t('compui.task.state.paused'), 'paused is told', 400);
  await settle(250);
  same('paused');
  // running again, and the page drawn again once more
  serve('running');
  await until(() => line.textContent === t('compui.task.state.running'), 'running is told', 400);
  await settle(250);
  same('running again');
  await redrawPage(m, sec, 'div.colab-privacy', 'Colab, running again');
  same('the page drawn again once more');
  // (a failed install is not shown as the task here: the section offers
  // "install" again, colab_login.js installBusy; as before)
  watched.clear();
  m.end();
}

// ---- R91 low-5: the Colab sign-in's failure (role="alert") and its
// waiting status (role="status", colab_login.js loginBlock) are the same
// nodes, where they were, through redraws that do not change them, in
// Settings → Colab and in the guide's remote step: taken out and put back,
// or drawn anew, they may be read again (as R81 L8, WV-10). One that
// changed, or comes back after it was gone, is new ----

const loginFailed = { state: 'failed', error: { code: 'colab.login_failed', params: {}, detail: 'gcloud: made-up refusal' } };
const loginWaiting = { state: 'waiting', url: 'https://accounts.google.com/o/oauth2/auth?client_id=made-up' };
const gcloudInstalled = { components: [{ id: 'gcloud', state: 'installed', kind: 'tool', version: '1.0' }], tasks: [] };
const callouts = (root, role) => root.querySelectorAll('div.ob-callout').filter((x) => x.getAttribute('role') === role);

// Settings → Colab
{
  routes = settingsRoutes();
  routes['GET /api/components'] = gcloudInstalled;
  routes['GET /api/colab/auth/login'] = loginFailed;
  const m = await mountSettings();
  const sec = m.page.querySelector('section[id="set-colab"]');
  await until(() => callouts(sec, 'alert').length === 1, 'the sign-in failure in the Colab section', 400);
  const alert = callouts(sec, 'alert')[0];
  const parent = alert.parentNode;
  assert.ok(parent && parent.className.includes('colab-login'), 'in the sign-in block');
  assert.ok(alert.textContent.includes('made-up refusal'), 'with gcloud\'s message');
  watched.set(alert, { detached: 0 });
  await redrawPage(m, sec, 'div.colab-privacy', 'Colab, the sign-in failed');
  assert.ok(callouts(sec, 'alert').length === 1 && callouts(sec, 'alert')[0] === alert && alert.parentNode === parent && parent.isConnected, 'the page drawn again: the same failure in the same place');
  assert.equal(watched.get(alert).detached, 0, 'the failure: never taken out and put back');
  // waiting (a sign-in started elsewhere: "changed colab")
  routes['GET /api/colab/auth/login'] = loginWaiting;
  m.view.onChanged({ kind: 'colab' });
  await until(() => callouts(sec, 'status').length === 1, 'waiting is shown', 400);
  assert.equal(callouts(sec, 'alert').length, 0, 'waiting: the failure is gone');
  const waiting = callouts(sec, 'status')[0];
  assert.ok(waiting.querySelector('a.ob-remote-link'), 'with the sign-in link');
  watched.set(waiting, { detached: 0 });
  await redrawPage(m, sec, 'div.colab-privacy', 'Colab, waiting');
  assert.ok(callouts(sec, 'status')[0] === waiting && waiting.parentNode === parent && parent.isConnected, 'the page drawn again: the same waiting status in the same place');
  assert.equal(watched.get(waiting).detached, 0, 'the waiting status: never taken out and put back');
  // another link found: told anew
  routes['GET /api/colab/auth/login'] = { ...loginWaiting, url: loginWaiting.url + '&again=1' };
  m.view.onChanged({ kind: 'colab' });
  await until(() => callouts(sec, 'status').length === 1 && callouts(sec, 'status')[0] !== waiting, 'another link: a new status', 400);
  // the same failure again after the waiting: a new alert, read out again
  routes['GET /api/colab/auth/login'] = loginFailed;
  m.view.onChanged({ kind: 'colab' });
  await until(() => callouts(sec, 'alert').length === 1, 'failed again', 400);
  assert.ok(callouts(sec, 'alert')[0] !== alert, 'failed again: a new alert');
  watched.clear();
  m.end();
}

// the guide's remote step: "sign in to Google" refused at once, the earlier
// failure stays where it is, the refusal is a new alert beside it
{
  const O = await import(new URL('onboarding.js', base).href);
  routes = {
    'GET /api/onboarding': { step: 'download', done: false, data: { asr_mode: 'remote' } },
    'PUT /api/onboarding': (b) => ({ status: 200, body: b }),
    'GET /api/colab': { status: { state: 'idle' }, adc: { present: false } },
    'GET /api/colab/auth/login': loginFailed,
    'GET /api/components': gcloudInstalled,
    'GET /api/components/sources': SOURCES,
    'POST /api/colab/auth/login': () => ({ status: 502, body: { error: { code: 'colab.login_timeout', params: {}, detail: 'gcloud: made-up timeout' } } }),
  };
  const main = new El('main');
  body.append(main);
  const ob = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
  await until(() => callouts(main, 'alert').length === 1, 'the sign-in failure in the guide', 400);
  const alert = callouts(main, 'alert')[0];
  const parent = alert.parentNode;
  watched.set(alert, { detached: 0 });
  const signIn = main.querySelectorAll('button').find((b) => b.textContent.trim() === t('onboarding.remote.login'));
  assert.ok(signIn && !signIn.disabled, '"sign in to Google" offered');
  signIn.click();
  await until(() => sent('POST', '/api/colab/auth/login').length === 1, 'the sign-in asked for');
  await until(() => callouts(main, 'alert').length === 2, 'the refusal is shown', 400);
  await settle(30);
  const [first, second] = callouts(main, 'alert');
  assert.ok(second === alert && alert.parentNode === parent && parent.isConnected, 'redrawn: the earlier failure is the same node in the same place');
  assert.equal(watched.get(alert).detached, 0, 'the earlier failure: never taken out and put back');
  assert.ok(first !== alert && first.textContent.includes('made-up timeout'), 'the refusal: a new alert');
  // refused again in the same words: gone while the request ran, so a new
  // alert again (read out again)
  main.querySelectorAll('button').find((b) => b.textContent.trim() === t('onboarding.remote.login')).click();
  await until(() => sent('POST', '/api/colab/auth/login').length === 2, 'the sign-in asked for again');
  await until(() => callouts(main, 'alert').length === 2 && callouts(main, 'alert')[0] !== first, 'refused again: a new alert', 400);
  assert.ok(callouts(main, 'alert')[0].textContent.includes('made-up timeout') && callouts(main, 'alert')[1] === alert, 'the same words; the earlier failure still the same node');
  watched.clear();
  ob.destroy();
  main.remove();
}

// R93: the guide's remote step, the components not read (500): its "could
// not start" alert is the same node, where it was, through the redraws of
// "sign in to Google"; "retry" failing again draws a new one (read again)
{
  const O = await import(new URL('onboarding.js', base).href);
  routes = {
    'GET /api/onboarding': { step: 'download', done: false, data: { asr_mode: 'remote' } },
    'PUT /api/onboarding': (b) => ({ status: 200, body: b }),
    'GET /api/colab': { status: { state: 'idle' }, adc: { present: false } },
    'GET /api/colab/auth/login': { state: 'idle' },
    'GET /api/components': () => ({ status: 500, body: { error: { code: 'api.internal', params: {} } } }),
    'GET /api/components/sources': SOURCES,
    'POST /api/colab/auth/login': () => ({ status: 502, body: { error: { code: 'colab.login_timeout', params: {}, detail: 'gcloud: made-up timeout' } } }),
  };
  const todo = t('onboarding.download.start_failed_todo');
  const startAlert = (root) => callouts(root, 'alert').filter((x) => x.textContent.includes(todo));
  const main = new El('main');
  body.append(main);
  const ob = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
  await until(() => startAlert(main).length === 1, 'the start failure in the guide', 400);
  const alert = startAlert(main)[0];
  const parent = alert.parentNode;
  watched.set(alert, { detached: 0 });
  const signIn = main.querySelectorAll('button').find((b) => b.textContent.trim() === t('onboarding.remote.login'));
  assert.ok(signIn && !signIn.disabled, '"sign in to Google" offered');
  const asked = sent('POST', '/api/colab/auth/login').length; // the checks before asked too
  signIn.click();
  await until(() => sent('POST', '/api/colab/auth/login').length === asked + 1, 'the sign-in asked for');
  await until(() => callouts(main, 'alert').some((x) => x.textContent.includes('made-up timeout')), 'the refusal is shown', 400);
  await settle(30);
  assert.ok(startAlert(main).length === 1 && startAlert(main)[0] === alert && alert.parentNode === parent && parent.isConnected, 'redrawn twice: the start failure is the same node in the same place');
  assert.equal(watched.get(alert).detached, 0, 'the start failure: never taken out and put back');
  // "retry": the components fail again in the same words, a new alert
  const reads = sent('GET', '/api/components').length;
  fk('ob:remote:restart').click();
  await until(() => sent('GET', '/api/components').length > reads && startAlert(main).length === 1 && startAlert(main)[0] !== alert, 'retried, failed again: a new alert', 400);
  watched.clear();
  ob.destroy();
  main.remove();
}

// ---- WV-11 (R86 L3; convention C3): disk space in decimal units on the
// remote server's card and the guide's "computer" step, as the sizes
// beside it (memory and video memory stay binary) ----
{
  const R = await import(new URL('settings_remote.js', base).href);
  const freeMB = 8200; // 8,598,323,200 bytes
  // the install step's sentence: what it downloads, takes and the free space, one unit
  const v = { kit_size: { download_bytes: 8.6e9, installed_bytes: 8.6e9 }, probe: { disk_free_mb: freeMB } };
  const want = t('settings.remote.install_size', { download: U.fmtBytes(8.6e9), installed: U.fmtBytes(8.6e9), free: U.fmtBytes(freeMB * 1048576) });
  assert.equal(R.installSizeText(v), want, 'the install sentence: the free space by fmtBytes');
  assert.equal(U.fmtBytes(freeMB * 1048576), U.fmtBytes(8.6e9), '8200 MiB free reads as the 8.6e9 bytes to install do');
  assert.ok(!R.installSizeText(v).includes(U.fmtMB(freeMB)), `not the binary "${U.fmtMB(freeMB)}"`);
  assert.equal(R.installSizeText({ kit_size: v.kit_size, probe: { disk_free_mb: -1 } }), t('settings.remote.install_size', { download: U.fmtBytes(8.6e9), installed: U.fmtBytes(8.6e9), free: '--' }), 'free space unknown: "--" as before');
  assert.equal(R.installSizeText({ probe: v.probe }), '', 'no kit size: nothing, as before');
  assert.equal(R.diskText(freeMB), U.fmtBytes(freeMB * 1048576));
  // the diagnostics row of the disk; its ok / warn still by MB
  const server = (o) => ({ id: 's1', name: 'gpu-box', state: 'ready', latency_ms: 0, agent_version: '1.0', agent_current: true, kit_ready: true, model: 'large-v3', precision: 'float16', probe: { disk_free_mb: freeMB, platform: 'linux', gpu_name: 'RTX', driver: '550', mem_total_mb: 65536, vram_total_mb: 24576 }, ...o });
  const disk = (s) => R.diagRows(s).find((r) => r.key === 'disk');
  assert.equal(disk(server()).value, U.fmtBytes(freeMB * 1048576), 'the diagnostics row: fmtBytes');
  assert.equal(disk(server()).status, 'ok', '8200 MiB free, 2048 MiB needed: ok, as before');
  assert.equal(disk(server({ probe: { ...server().probe, disk_free_mb: 1000 } })).status, 'warn', 'below 2048 MiB: warn, as before');
  assert.equal(disk(server({ kit_ready: false, kit_size: { download_bytes: 8.6e9, installed_bytes: 8.6e9 } })).status, 'warn', 'below what the install takes (8.6e9 bytes = 8201.6 MiB): warn, compared in MB as before');
}

// the guide: the "computer" step and the "storage" step show the same disk alike
{
  const O = await import(new URL('onboarding.js', base).href);
  const diskMB = Math.floor(500e9 / 1048576); // a 500 GB disk as the hardware probe reports it
  routes = {
    'GET /api/onboarding': { step: 'computer', done: false, data: {} },
    'PUT /api/onboarding': (b) => ({ status: 200, body: b }),
    'GET /api/hardware': { hw: { ram_mb: 16384, disk_free_mb: diskMB }, recommendation: { components: [], reasons: [{ code: 'hw.reason.cpu_only', params: {} }], notes: [] } },
    'GET /api/components': { components: [], tasks: [] },
    'GET /api/components/sources': SOURCES,
    'POST /api/components/plan': { ids: [], download_bytes: 0, install_bytes: 0, free_bytes: 500e9, enough_space: true },
  };
  const main = new El('main');
  body.append(main);
  const ob = O.mountOnboarding(main, { courses: () => [], newCourse() {} });
  const diskLine = t('onboarding.computer.disk', { size: U.fmtBytes(diskMB * 1048576) });
  await until(() => main.textContent.includes(diskLine), 'the computer step shows the disk', 400);
  assert.equal(U.fmtBytes(diskMB * 1048576), U.fmtBytes(500e9), 'the same number as the storage step\'s fmtBytes of the same disk');
  assert.ok(!main.textContent.includes(t('onboarding.computer.disk', { size: U.fmtMB(diskMB) })), `not the binary "${U.fmtMB(diskMB)}"`);
  assert.ok(main.textContent.includes(t('onboarding.computer.ram', { size: U.fmtMB(16384) })), 'the memory as before (fmtMB)');
  ob.destroy();
  main.remove();
}

// ---- handed over (components_ui.js): a disk that filled up while writing
// does not know the size needed (internal/components withDiskFree:
// required_mb -1); it is said unknown, never "-1 MB" ----
{
  const CU = await import(new URL('components_ui.js', base).href);
  const unknown = t('compui.unknown_size');
  const full = (required_mb, free_mb) => CU.taskErrorText({ code: 'components.disk_full', params: { required_mb, free_mb } });
  assert.equal(full(-1, -1).what, t('compui.err.disk_full', { required: unknown, free: unknown }), 'neither known: both unknown');
  assert.equal(full(-1, 300).what, t('compui.err.disk_full', { required: unknown, free: U.fmtMB(300) }), 'the free space known (withDiskFree fills it), the size needed not: unknown');
  assert.ok(!full(-1, 300).what.includes(U.fmtMB(-1)), `not "${U.fmtMB(-1)}"`);
  assert.equal(full(2048, 300).what, t('compui.err.disk_full', { required: U.fmtMB(2048), free: U.fmtMB(300) }), 'both known: as before');
  assert.equal(full(0, 300).what, t('compui.err.disk_full', { required: U.fmtMB(0), free: U.fmtMB(300) }), 'zero is a size, as before');
  // in a task view's alert
  const view = CU.taskView({ components: () => [], onAction() {} });
  const page = new El('div');
  body.append(page);
  page.append(view.el);
  view.update(ctask('tk9', ['asr-cuda'], 'failed', { error: { code: 'components.disk_full', params: { required_mb: -1, free_mb: 300 } } }));
  const alert = view.el.querySelector('div.cu-error');
  assert.ok(alert && alert.textContent.includes(full(-1, 300).what), 'the task\'s alert says it');
  assert.ok(!view.el.textContent.includes(U.fmtMB(-1)), 'no negative size in the task view');
  page.remove();
}

console.log('r9 views settings checks: ok');
process.exit(0);
