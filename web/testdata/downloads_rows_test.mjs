// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Review R61 F2 and F4: the download rows of a course page (downloads.js
// downloadsSlot) draw exactly the task's allowed_actions as buttons — a
// failure no retry can fix has "remove" only, its one way out; a retryable
// one has "retry" and "remove" — each a focusable, named <button>. A retry
// the row still showed but the server no longer offers answers 409 with
// the task's own reason: for a refused file the notice uses the row's
// fetch text, not the upload's. web/downloads_rows_test.go starts it with
// the web directory as a file: URL. Only strings, counts and identities
// (assert.ok(a === b)) are compared; every wait is bounded.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const base = process.argv[2];

// ---- DOM stand-in (the parts downloads.js and dom.js use) ----

const MAX_DEPTH = 10000;
class Node {
  constructor() {
    this.parentNode = null;
    this.listeners = {};
  }
  get isConnected() {
    let n = this;
    for (let i = 0; n; n = n.parentNode) {
      if (++i > MAX_DEPTH) throw new Error('stand-in DOM: parent chain too long');
      if (n === root) return true;
    }
    return false;
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
    for (const fn of [...(this.listeners[ev.type] || [])]) fn(ev);
    return true;
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
    this.hidden = false;
    const self = this;
    this.classList = {
      add(c) {
        if (!this.contains(c)) self.className = (self.className + ' ' + c).trim();
      },
      remove(c) {
        self.className = self.className.split(/\s+/).filter((x) => x && x !== c).join(' ');
      },
      toggle(c, on) {
        if (on === undefined ? !this.contains(c) : on) this.add(c);
        else this.remove(c);
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
  removeChild(c) {
    c.remove();
    return c;
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
  replaceChildren(...cs) {
    for (const c of [...this.children]) c.remove();
    this.append(...cs);
  }
  click() {
    this.dispatchEvent({ type: 'click', preventDefault() {}, stopPropagation() {} });
  }
  // tag, .class and [data-x="v"] selectors only
  matches(sel) {
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
    const walk = (n, depth) => {
      if (depth > MAX_DEPTH) throw new Error('stand-in DOM: tree too deep');
      for (const c of n.children) {
        if (c instanceof El) {
          if (c.matches(sel)) out.push(c);
          walk(c, depth + 1);
        }
      }
    };
    walk(this, 0);
    return out;
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
}
const root = new El('html');
const body = new El('body');
root.append(body);
const toasts = new El('div');
body.append(toasts);
const store = { getItem: () => null, setItem() {}, removeItem() {} };
Object.assign(globalThis, {
  Node,
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: {
    documentElement: root,
    body,
    activeElement: body,
    addEventListener() {},
    removeEventListener() {},
    querySelector: (sel) => root.querySelector(sel),
    querySelectorAll: (sel) => root.querySelectorAll(sel),
    getElementById: (id) => (id === 'toasts' ? toasts : null),
    createElement: (tag) => new El(tag),
    createElementNS: (_, tag) => new El(tag),
    createTextNode: (s) => new Text(s),
    visibilityState: 'visible',
  },
  addEventListener() {},
  removeEventListener() {},
  dispatchEvent: () => true,
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  CSS: { escape: (s) => String(s) },
});
globalThis.window = globalThis;

// ---- the server ----

const failed = (id, code, params, actions) => ({
  id, platform: 'panopto', session_id: 's' + id, course_id: 'C1', title: `Task ${id}`, recorded_at: '', state: 'failed',
  position: 0, bytes_done: 0, bytes_total: 600, lecture_id: '', error: { code, params }, override_disabled: false,
  tracks: [{ kind: 'audio', format: 'file', role: 'primary', label: '' }], allowed_actions: actions, created_at: '', updated_at: '',
});
let items = [];
let retryAnswer = null; // {status, body} of POST …/retry
const requests = [];
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const text = await readFile(new URL(u, base), 'utf8');
    return { ok: true, status: 200, json: async () => JSON.parse(text), text: async () => text };
  }
  requests.push(`${init.method || 'GET'} ${u}`);
  const reply = (status, body) => ({ ok: status >= 200 && status < 300, status, text: async () => (body == null ? '' : JSON.stringify(body)) });
  if (u.startsWith('/api/downloads?')) return reply(200, { items });
  if (/^\/api\/downloads\/\d+\/retry$/.test(u)) return reply(retryAnswer.status, retryAnswer.body);
  throw new Error('unexpected fetch ' + u);
};

const I = await import(new URL('i18n.js', base).href);
await I.setLocale('en');
const D = await import(new URL('downloads.js', base).href);

// a few turns of the event loop, bounded
const settle = async () => {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
};
async function until(what, cond) {
  for (let i = 0; i < 100; i++) {
    if (cond()) return;
    await new Promise((r) => setTimeout(r, 10));
  }
  throw new Error('timed out waiting for ' + what);
}

items = [
  failed(1, 'downloads.drm_protected', null, ['remove']),
  failed(2, 'upload.media_html', { name: 'x.mp4' }, ['remove']),
  failed(3, 'downloads.network', null, ['retry', 'remove']),
  { ...failed(4, '', null, ['cancel']), state: 'queued', error: null, position: 1, bytes_total: -1 },
  { ...failed(5, '', null, []), state: 'importing', error: null },
];
const slot = D.downloadsSlot({ ctx: { subscribe: () => () => {} }, courseId: 'C1' });
body.append(slot.el);
await until('the rows', () => slot.el.children.length === 5);

const row = (id) => slot.el.querySelector(`li[data-dl="${id}"]`);
const buttons = (id) => row(id).querySelectorAll('button');
const names = (id) => buttons(id).map((b) => b.getAttribute('aria-label') || b.textContent.trim());
const remove = I.t('common.remove');
const retry = I.t('downloads.action.retry');
const cancel = I.t('downloads.action.cancel');

assert.deepEqual(names(1), [remove], 'DRM: remove only');
assert.deepEqual(names(2), [remove], 'a refused file: remove only');
assert.deepEqual(names(3), [retry, remove], 'a network failure: retry and remove');
assert.deepEqual(names(4), [cancel], 'queued: cancel');
assert.deepEqual(names(5), [], 'importing: nothing');
for (const id of [1, 2, 3, 4]) {
  for (const b of buttons(id)) {
    // a native button that is not disabled and not taken out of the tab order
    assert.equal(b.tagName, 'BUTTON');
    assert.equal(b.getAttribute('type'), 'button');
    assert.ok(!b.disabled && b.getAttribute('tabindex') !== '-1' && !b.hidden, `task ${id}: a focusable button`);
    assert.ok((b.getAttribute('aria-label') || b.textContent).trim().length > 0, `task ${id}: a named button`);
  }
}
// the refused file's reason is the fetch's text, not the upload's
const main = (id) => row(id).querySelector('.dl-note-main').textContent;
assert.equal(main(2), I.t('downloads.refused.html'));
assert.ok(!main(2).includes(I.t('errors.upload.media_html', { name: 'x.mp4' })), 'no upload text');

// ---- F4: a retry the server no longer offers ----
// the row is out of date: it still shows retry, the server answers the
// task's own reason
items[2] = failed(3, 'upload.media_playlist', { name: 'x.mp4', kind: 'm3u8' }, ['retry', 'remove']);
slot.refresh();
await until('the stale row', () => main(3) === I.t('downloads.refused.playlist', { kind: 'm3u8' }));
retryAnswer = { status: 409, body: { error: { code: 'upload.media_playlist', params: { name: 'x.mp4', kind: 'm3u8' } } } };
const before = requests.length;
buttons(3)[0].click();
await until('the notice', () => toasts.children.length > 0);
assert.ok(requests.slice(before).includes('POST /api/downloads/3/retry'), 'the retry was asked for');
assert.equal(toasts.children.length, 1);
const notice = toasts.children[0].querySelector('.toast-msg').textContent;
assert.equal(notice, I.t('downloads.refused.playlist', { kind: 'm3u8' }), 'the row\'s words');
assert.ok(notice !== I.t('errors.upload.media_playlist', { name: 'x.mp4', kind: 'm3u8' }), 'not the upload text');
toasts.replaceChildren();
// any other refusal keeps the server's text
retryAnswer = { status: 409, body: { error: { code: 'downloads.drm_protected' } } };
buttons(3)[0].click();
await until('the second notice', () => toasts.children.length > 0);
assert.equal(toasts.children[0].querySelector('.toast-msg').textContent, I.t('errors.downloads.drm_protected'));

slot.destroy();
await settle();
console.log('download rows checks: ok');
process.exit(0);
