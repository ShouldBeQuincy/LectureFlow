// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// r6 path-grants: a folder the user picked in the system dialog
// (window.lectureflow.pickFolder) is granted to the page by the desktop main
// process. When that pick is not going to be used, the page gives it up at
// once (DELETE /api/app/folder-pick {path}, api.forgetFolderPick) instead of
// leaving it to expire:
//   - the settings page's "change the data folder": cancel in the move dialog;
//   - the setup guide's "data folder" step: the check refuses the folder (or
//     fails), and cancel in the move dialog;
//   - the setup guide's "import existing data": cancel in the import dialog.
// A confirmed pick is used (the move, the import) and not given up, unless
// the service refuses that request at once (409 busy, 507 no room, …): it
// returns the grant then, and all three pages ask for the folder again, so
// the page gives the pick up as well (security review SEC38 low-1). Giving
// up never waits and never shows anything: a failed request is silent, the
// pick just expires; the refusal itself is shown as before.
// The three pages run on the DOM stand-in of a11y_test.mjs (trimmed); fetch
// records every request and answers from `routes`. Nodes are compared by
// identity. web/folder_pick_test.go starts it with the web directory as a
// file: URL.

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
  remove() {
    if (!this.parentNode) return;
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
  // the guide's redraw keeps a focused step title in place (onboarding.js rerender)
  get childNodes() {
    return this.children;
  }
  insertBefore(n, ref) {
    if (!ref) return this.append(n);
    n.remove();
    n.parentNode = this;
    this.children.splice(this.children.indexOf(ref), 0, n);
    return n;
  }
  before(...ns) {
    for (const n of ns) this.parentNode.insertBefore(n, this);
  }
  after(...ns) {
    let ref = this;
    for (const n of ns) {
      const p = ref.parentNode;
      p.insertBefore(n, p.children[p.children.indexOf(ref) + 1] || null);
      ref = n;
    }
  }
  get disabled() {
    return 'disabled' in this.attrs;
  }
  set disabled(v) {
    if (v) this.attrs.disabled = '';
    else delete this.attrs.disabled;
  }
  setAttribute(k, v) {
    if (k === 'class') this.className = String(v);
    else this.attrs[k] = String(v);
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
    n.remove();
    this.parentNode = null;
    n.parentNode = p;
    p.children.splice(i, 1, n);
  }
  contains(n) {
    return !!up(n, (x) => x === this);
  }
  focus() {
    const prev = doc.activeElement;
    if (prev === this) return;
    doc.activeElement = this;
    if (prev) prev.dispatchEvent(event('focusout', { relatedTarget: this }));
    this.dispatchEvent(event('focusin', { relatedTarget: prev }));
  }
  // a disabled button takes no click, as in a browser
  click() {
    if (this.disabled) return;
    this.dispatchEvent(event('click'));
  }
  blur() {}
  showModal() {
    this.open = true;
    dialogs.push(this);
  }
  close() {
    this.open = false;
  }
  getBoundingClientRect() {
    return { top: 0, bottom: 0, left: 0, right: 0 };
  }
  matches(sel) {
    if (sel === ':focus-visible') return false;
    const m = /^([a-z]*)((?:\.[\w-]+)*)(?:\[([\w-]+)(?:="([^"]*)")?\])?$/.exec(sel);
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
  createElement: (t) => new El(t),
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

// A rejected promise nobody handles (a request that fails in the
// background) is a failure of this test.
process.on('unhandledRejection', (e) => {
  console.error('unhandled rejection:', e && e.message ? e.message : String(e));
  process.exit(1);
});

const base = process.argv[2];

// ---- fetch: the catalogs from the web directory; every other request is
// recorded in `calls` and answered by routes['METHOD /path'] (a body, or a
// function of the request's body giving {status, body} or a thrown error) ----

const calls = []; // {method, path, body}
let routes = {};
const GIVE_UP = 'DELETE /api/app/folder-pick';
let giveUp = () => ({ status: 204, body: null }); // how the server answers the give-up
let refuse = null; // {status, body}: how the service refuses the move or the import; null = it starts
const coded = (status, code, params = {}) => ({ status, body: { error: { code, params } } });
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const text = await readFile(new URL(u, base), 'utf8');
    return { ok: true, status: 200, json: async () => JSON.parse(text), text: async () => text };
  }
  const method = init.method || 'GET';
  const path = u.split('?')[0];
  const reqBody = init.body ? JSON.parse(init.body) : undefined;
  calls.push({ method, path, body: reqBody });
  await new Promise((r) => setTimeout(r, 2));
  const key = `${method} ${path}`;
  let a = key === GIVE_UP ? giveUp : routes[key];
  if (typeof a === 'function') a = a(reqBody);
  else if (a !== undefined) a = { status: 200, body: a };
  else a = { status: 404, body: { error: { code: 'api.not_found', params: { method, path } } } };
  const text = a.body == null ? '' : JSON.stringify(a.body);
  return { ok: a.status < 300, status: a.status, headers: { get: () => null }, text: async () => text, json: async () => JSON.parse(text) };
};
const settle = (ms = 60) => new Promise((r) => setTimeout(r, ms));
const sent = (method, path) => calls.filter((c) => c.method === method && c.path === path);
const givenUp = () => sent('DELETE', '/api/app/folder-pick').map((c) => c.body);

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
await I.setLocale('en');
const t = I.t;

// the page's handler of failed requests (app.js): a toast
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });

// ---- the system folder dialog and the page's confirmation ----

const PICK = 'D:\\Lectures Data\\LectureFlow';
let picks = 0;
globalThis.lectureflow = {
  pickFolder: async () => {
    picks++;
    return PICK;
  },
};

const fk = (k) => doc.querySelector(`[data-fkey="${k}"]`);
function openDialog() {
  const open = dialogs.filter((d) => d.open && d.isConnected);
  assert.equal(open.length, 1, `one confirmation open (${open.length})`);
  return open[0];
}
// answers the open confirmation: its "Cancel" button, or the one labelled okText
async function answer(okText) {
  await settle();
  const want = okText || t('common.cancel');
  const b = openDialog()
    .querySelectorAll('button')
    .find((x) => x.textContent.trim() === want);
  assert.ok(b, `the confirmation has a "${want}" button`);
  b.click();
  await settle();
}
function noDialog() {
  assert.equal(dialogs.filter((d) => d.open && d.isConnected).length, 0, 'no confirmation is open');
}
const quiet = (what) => assert.equal(toasts.children.length, 0, `${what}: nothing is shown to the user (${toasts.textContent})`);

// ---- api.forgetFolderPick: the request, and silence whatever comes back ----
{
  assert.equal(typeof A.api.forgetFolderPick, 'function', 'api.forgetFolderPick exists');
  for (const [name, how] of [
    ['204', () => ({ status: 204, body: null })],
    ['500', () => ({ status: 500, body: { error: { code: 'internal', params: {} } } })],
    ['404 (an older server)', () => ({ status: 404, body: { error: { code: 'api.not_found', params: {} } } })],
    [
      'no answer',
      () => {
        throw new TypeError('Failed to fetch');
      },
    ],
  ]) {
    calls.length = 0;
    giveUp = how;
    const got = await A.api.forgetFolderPick(PICK); // never rejects
    assert.equal(got, null, `${name}: resolves to null`);
    assert.deepEqual(calls, [{ method: 'DELETE', path: '/api/app/folder-pick', body: { path: PICK } }], `${name}: one DELETE with the path`);
    quiet(`give-up answered ${name}`);
  }
  giveUp = () => ({ status: 204, body: null });
}

const DD = { state: 'idle', data_dir: 'C:\\Users\\Student\\AppData\\Roaming\\LectureFlow', can_move: true, free_bytes: 500e9, copied_bytes: 0, total_bytes: 0 };
const MOVING = { ...DD, kind: 'move', state: 'copying', path: PICK, total_bytes: 10 };

// ---- the settings page: "change the data folder" ----
{
  const S = await import(new URL('settings_app.js', base).href);
  routes = {
    'GET /api/app': { mode: 'desktop', version: '3.2.0', platform: 'windows', data_dir: DD.data_dir },
    'GET /api/app/settings': { close_behavior: 'ask', prevent_sleep: true, update_channel: 'stable' },
    'GET /api/app/storage': { data_dir: DD.data_dir, used_bytes: 1e9, free_bytes: 500e9 },
    'GET /api/app/data-dir': DD,
    'GET /api/app/old-data-dir': { path: '', exists: false, bytes: 0 },
    'POST /api/app/data-dir': () => refuse || { status: 202, body: MOVING },
  };
  const shell = new El('div');
  body.append(shell);
  // view_settings.js's helpers: plain boxes that keep what they are given
  const box = (...xs) => D.h('div', null, xs.filter((x) => x instanceof Node || Array.isArray(x) || typeof x === 'string'));
  let section = null;
  const ui = new Proxy(
    {
      rerender() {
        shell.replaceChildren();
        section.render(shell);
      },
    },
    { get: (o, k) => (k in o ? o[k] : box) },
  );
  section = S.createAppSection(ui);
  section.load();
  await settle(120);
  ui.rerender();
  const move = () => fk('set:app:move');
  assert.ok(move(), 'the settings page has "change the data folder"');

  // cancel: the pick is given up, nothing is moved
  calls.length = 0;
  picks = 0;
  move().click();
  await answer();
  assert.equal(picks, 1, 'the folder dialog was shown');
  assert.deepEqual(givenUp(), [{ path: PICK }], 'settings, cancel: the pick is given up');
  assert.equal(sent('POST', '/api/app/data-dir').length, 0, 'settings, cancel: nothing is moved');
  quiet('settings, cancel');
  assert.ok(!move().disabled, 'settings, cancel: the button is ready again');

  // cancel while the give-up does not come through: the same for the user
  for (const how of [
    () => ({ status: 500, body: { error: { code: 'internal', params: {} } } }),
    () => {
      throw new TypeError('Failed to fetch');
    },
  ]) {
    calls.length = 0;
    giveUp = how;
    move().click();
    await answer();
    assert.deepEqual(givenUp(), [{ path: PICK }], 'settings, cancel, the give-up fails: it was sent');
    quiet('settings, cancel, the give-up fails');
    assert.ok(!move().disabled, 'settings, cancel, the give-up fails: the button is ready again');
  }
  // the page does not wait for the answer
  calls.length = 0;
  let release = null;
  const held = new Promise((r) => (release = r));
  let answered = false;
  giveUp = () => ({ status: 204, body: null });
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    if ((init.method || 'GET') === 'DELETE' && String(url).split('?')[0] === '/api/app/folder-pick') {
      calls.push({ method: 'DELETE', path: '/api/app/folder-pick', body: JSON.parse(init.body) });
      await held;
      answered = true;
      return { ok: true, status: 204, headers: { get: () => null }, text: async () => '' };
    }
    return realFetch(url, init);
  };
  move().click();
  await answer();
  assert.deepEqual(givenUp(), [{ path: PICK }], 'settings, cancel, a slow give-up: it was sent');
  assert.ok(!answered && !move().disabled, 'settings: the button is ready again before the give-up is answered');
  release();
  await settle();
  globalThis.fetch = realFetch;

  // confirmed, and the service refuses the move at once: the refusal is
  // shown as before, and the pick (returned by the service, never used
  // again by this page) is given up after it; also when the give-up fails
  for (const how of [
    () => ({ status: 204, body: null }),
    () => {
      throw new TypeError('Failed to fetch');
    },
  ]) {
    calls.length = 0;
    giveUp = how;
    refuse = coded(409, 'data_dir.busy', { running: 1, queued: 0 });
    move().click();
    await answer(t('settings.app.move_ok'));
    assert.deepEqual(
      calls.map((c) => `${c.method} ${c.path}`),
      ['POST /api/app/data-dir', GIVE_UP],
      'settings, the move is refused: the pick is given up after the refusal',
    );
    assert.deepEqual(givenUp(), [{ path: PICK }], 'settings, the move is refused: the picked path');
    assert.equal(toasts.children.length, 1, 'settings, the move is refused: the refusal is shown, once');
    assert.equal(toasts.children[0].dataset.kind, 'error');
    assert.equal(toasts.children[0].dataset.msg, t('errors.data_dir.busy', { running: 1, queued: 0 }), "settings: the refusal's own text");
    toasts.replaceChildren();
    assert.ok(!move().disabled, 'settings, the move is refused: the button is ready again');
  }
  giveUp = () => ({ status: 204, body: null });
  refuse = null;

  // confirmed: the move starts with the pick, which is not given up
  calls.length = 0;
  move().click();
  await answer(t('settings.app.move_ok'));
  assert.deepEqual(
    sent('POST', '/api/app/data-dir').map((c) => c.body),
    [{ path: PICK }],
    'settings, confirmed: the move starts',
  );
  assert.deepEqual(givenUp(), [], 'settings, confirmed: the pick is used, not given up');
  noDialog();
  section.destroy?.();
  shell.remove();
}

// ---- the setup guide, "data folder": the check, then the move dialog ----
{
  const O = await import(new URL('onboarding_storage.js', base).href);
  let check = null; // the answer of POST /api/app/data-dir/check
  routes = {
    'GET /api/app/data-dir': DD,
    'POST /api/app/data-dir/check': () => check(),
    'POST /api/app/data-dir': () => refuse || { status: 202, body: MOVING },
  };
  const page = new El('div');
  body.append(page);
  let step = null;
  const env = {
    cache: {},
    data: {},
    alive: () => true,
    rerender() {
      page.replaceChildren(D.h('div', null, step.body()), D.h('div', null, step.secondary()));
    },
    save: async () => {},
    setData: async (patch) => void Object.assign(env.data, patch),
    next() {},
  };
  step = O.storageStep(env);
  await settle(120);
  env.rerender();
  const change = () => fk('ob:store:change');
  assert.ok(change(), 'the step has "change location"');
  const okCheck = () => ({ status: 200, body: { path: PICK, free_bytes: 800e9, ok: true, error: null } });

  // the check refuses the folder: it is said, and the pick is given up
  calls.length = 0;
  check = () => ({ status: 200, body: { path: PICK, free_bytes: 800e9, ok: false, error: { code: 'data_dir.target_not_empty', params: { path: PICK } } } });
  change().click();
  await settle(120);
  noDialog();
  assert.equal(sent('POST', '/api/app/data-dir/check').length, 1, 'guide: the picked folder is checked');
  assert.deepEqual(givenUp(), [{ path: PICK }], 'guide, the check refuses: the pick is given up');
  assert.ok(page.querySelector('.ob-callout'), 'guide, the check refuses: the reason is shown');
  quiet('guide, the check refuses');
  assert.ok(!change().disabled, 'guide, the check refuses: another folder can be chosen');

  // the check itself fails: the pick will not be used either
  calls.length = 0;
  check = () => ({ status: 500, body: { error: { code: 'internal', params: {} } } });
  change().click();
  await settle(120);
  noDialog();
  assert.deepEqual(givenUp(), [{ path: PICK }], 'guide, the check fails: the pick is given up');

  // the check passes, cancel in the move dialog
  calls.length = 0;
  check = okCheck;
  change().click();
  await settle(60);
  await answer();
  assert.deepEqual(givenUp(), [{ path: PICK }], 'guide, cancel: the pick is given up');
  assert.equal(sent('POST', '/api/app/data-dir').length, 0, 'guide, cancel: nothing is moved');
  assert.equal(env.data.storage_moved_to, undefined, 'guide, cancel: no move is noted');
  quiet('guide, cancel');
  assert.ok(!change().disabled, 'guide, cancel: the button is ready again');

  // cancel while the give-up does not come through
  calls.length = 0;
  giveUp = () => {
    throw new TypeError('Failed to fetch');
  };
  change().click();
  await settle(60);
  await answer();
  assert.deepEqual(givenUp(), [{ path: PICK }], 'guide, cancel, the give-up fails: it was sent');
  quiet('guide, cancel, the give-up fails');
  assert.ok(!change().disabled, 'guide, cancel, the give-up fails: the button is ready again');
  giveUp = () => ({ status: 204, body: null });

  // confirmed, and the service refuses the move at once: the pick is
  // given up after the refusal; also when the give-up fails
  for (const how of [() => ({ status: 204, body: null }), () => coded(500, 'internal')]) {
    calls.length = 0;
    giveUp = how;
    refuse = coded(507, 'data_dir.no_space', { need_bytes: 2, free_bytes: 1 });
    change().click();
    await settle(60);
    await answer(t('onboarding.storage.change'));
    assert.deepEqual(
      calls.map((c) => `${c.method} ${c.path}`),
      ['POST /api/app/data-dir/check', 'POST /api/app/data-dir', GIVE_UP],
      'guide, the move is refused: the pick is given up after the refusal',
    );
    assert.deepEqual(givenUp(), [{ path: PICK }], 'guide, the move is refused: the picked path');
    assert.equal(env.data.storage_moved_to, undefined, 'guide, the move is refused: no move is noted');
    quiet("guide, the move is refused (the guide's own handler, not set here, shows the refusal)");
    assert.ok(!change().disabled, 'guide, the move is refused: the button is ready again');
  }
  giveUp = () => ({ status: 204, body: null });
  refuse = null;

  // confirmed: the move starts, the pick is not given up
  calls.length = 0;
  change().click();
  await settle(60);
  await answer(t('onboarding.storage.change'));
  assert.deepEqual(
    sent('POST', '/api/app/data-dir').map((c) => c.body),
    [{ path: PICK }],
    'guide, confirmed: the move starts',
  );
  assert.deepEqual(givenUp(), [], 'guide, confirmed: the pick is used, not given up');
  step.destroy();
  page.remove();
}

// ---- the setup guide, step 1: "import existing data" ----
{
  const O = await import(new URL('onboarding.js', base).href);
  let saved = null;
  routes = {
    'GET /api/onboarding': { step: 'welcome', done: false, data: {} },
    'PUT /api/onboarding': (b) => ({ status: 200, body: (saved = b) }),
    'GET /api/app': { mode: 'desktop', version: '3.2.0', platform: 'windows', data_dir: DD.data_dir },
    'GET /api/app/data-dir': DD,
    'POST /api/app/import': () => refuse || { status: 202, body: { kind: 'import', state: 'copying', path: PICK, copied_bytes: 0, total_bytes: 10 } },
    'GET /api/app/import': { kind: 'import', state: 'copying', path: PICK, copied_bytes: 1, total_bytes: 10 },
  };
  const main = new El('main');
  body.append(main);
  const view = O.mountOnboarding(main, {});
  await settle(150);
  const imp = () => fk('ob:import');
  assert.ok(imp(), 'step 1 has "import existing data"');

  // cancel: the pick is given up, nothing is imported or saved
  calls.length = 0;
  imp().click();
  await answer();
  assert.deepEqual(givenUp(), [{ path: PICK }], 'import, cancel: the pick is given up');
  assert.equal(sent('POST', '/api/app/import').length, 0, 'import, cancel: nothing is imported');
  assert.equal(sent('PUT', '/api/onboarding').length, 0, 'import, cancel: the guide notes no import');
  quiet('import, cancel');
  assert.ok(!imp().disabled, 'import, cancel: the button is ready again');

  // cancel while the give-up does not come through
  calls.length = 0;
  giveUp = () => ({ status: 500, body: { error: { code: 'internal', params: {} } } });
  imp().click();
  await answer();
  assert.deepEqual(givenUp(), [{ path: PICK }], 'import, cancel, the give-up fails: it was sent');
  quiet('import, cancel, the give-up fails');
  giveUp = () => ({ status: 204, body: null });

  // confirmed, and the service refuses the import at once: the failure is
  // shown in the step as before, and the pick is given up after it; also
  // when the give-up fails
  for (const how of [
    () => ({ status: 204, body: null }),
    () => {
      throw new TypeError('Failed to fetch');
    },
  ]) {
    calls.length = 0;
    giveUp = how;
    refuse = coded(409, 'data_import.not_fresh', { courses: 1, lectures: 2 });
    imp().click();
    await answer(t('onboarding.import.confirm_ok'));
    const order = calls.map((c) => `${c.method} ${c.path}`);
    assert.equal(sent('POST', '/api/app/import').length, 1, 'import, refused: the import was asked for');
    assert.deepEqual(givenUp(), [{ path: PICK }], 'import, refused: the pick is given up');
    assert.ok(order.indexOf('POST /api/app/import') < order.indexOf(GIVE_UP), 'import, refused: given up after the refusal');
    const shown = main.querySelector('.ob-callout');
    assert.ok(shown && shown.textContent.includes(t('errors.data_import.not_fresh', { courses: 1, lectures: 2 })), 'import, refused: the refusal is shown in the step');
    quiet('import, refused');
    assert.ok(!imp().disabled, 'import, refused: "import again" is ready');
    assert.ok(!saved.data.imported, 'import, refused: the guide forgot the import');
  }
  giveUp = () => ({ status: 204, body: null });
  refuse = null;

  // confirmed: the import starts, the pick is not given up
  calls.length = 0;
  imp().click();
  await answer(t('onboarding.import.confirm_ok'));
  assert.deepEqual(
    sent('POST', '/api/app/import').map((c) => c.body),
    [{ path: PICK }],
    'import, confirmed: the import starts',
  );
  assert.deepEqual(givenUp(), [], 'import, confirmed: the pick is used, not given up');
  assert.equal(saved && saved.data && saved.data.imported, true, 'import, confirmed: the guide noted the import first');
  view.destroy();
  main.remove();
}

console.log('folder pick checks: ok');
process.exit(0);
