// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// r4: a link the desktop shell refuses because of its limit (3 links in 10
// seconds, desktop/src/main/ipc.js) says so in a notice; any other refusal
// of window.lectureflow.openExternal stays silent. A fake openExternal
// rejects with the error as the page receives it through IPC; the notices
// land in a stand-in #toasts. web/link_throttled_test.go starts it with
// the web directory as a file: URL. Only strings and counts are compared.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const base = process.argv[2];

// ---- DOM stand-in ----

class Node {
  constructor() {
    this.parentNode = null;
    this.listeners = {};
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  removeEventListener() {}
  dispatchEvent(ev) {
    for (const fn of [...(this.listeners[ev.type] || [])]) fn(ev);
    return true;
  }
  remove() {
    if (this.parentNode) this.parentNode.children = this.parentNode.children.filter((c) => c !== this);
    this.parentNode = null;
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
    this.classList = { add() {}, remove() {}, toggle() {}, contains: () => false };
  }
  append(...cs) {
    for (const c of cs) {
      const n = typeof c === 'string' ? new Text(c) : c;
      n.parentNode = this;
      this.children.push(n);
    }
  }
  replaceChildren(...cs) {
    this.children = [];
    this.append(...cs);
  }
  set textContent(v) {
    this.replaceChildren(new Text(v));
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join('');
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
  // the first descendant with the class (.name only), bounded by the tree
  querySelector(sel) {
    const cls = String(sel).replace(/^\./, '');
    for (const c of this.children) {
      if (!(c instanceof El)) continue;
      if (c.className.split(/\s+/).includes(cls)) return c;
      const d = c.querySelector(sel);
      if (d) return d;
    }
    return null;
  }
}
globalThis.Node = Node;
const toasts = new El('div');
toasts.setAttribute('id', 'toasts');
const store = { getItem: () => null, setItem() {}, removeItem() {} };
Object.assign(globalThis, {
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: {
    documentElement: new El('html'),
    body: new El('body'),
    addEventListener() {},
    querySelector: () => null,
    getElementById: (id) => (id === 'toasts' ? toasts : null),
    createElement: (tag) => new El(tag),
    createElementNS: (_, tag) => new El(tag),
    createTextNode: (s) => new Text(s),
  },
  addEventListener() {},
  dispatchEvent() {
    return true;
  },
  matchMedia: () => ({ matches: false, addEventListener() {} }),
});
globalThis.window = globalThis;
globalThis.fetch = async (url) => {
  const m = /^i18n\/([\w-]+)\.json$/.exec(String(url));
  if (!m) throw new Error('unexpected fetch ' + url);
  const text = await readFile(new URL(`i18n/${m[1]}.json`, base), 'utf8');
  return { ok: true, json: async () => JSON.parse(text) };
};

// the shell's bridge: openExternal answers what the case sets
const THROTTLED = "Error invoking remote method 'lf:open-external': Error: openExternal: too many links in a short time";
let answer = () => Promise.resolve(true);
let calls = 0;
window.lectureflow = {
  openExternal: (url) => {
    calls++;
    assert.equal(typeof url, 'string');
    return answer();
  },
};

const I = await import(new URL('i18n.js', base).href);
await I.setLocale('zh-CN');
const O = await import(new URL('onboarding_api.js', base).href);

// the rejection settles after a few turns of the microtask queue
const settle = () => new Promise((r) => setTimeout(r, 0));
const texts = () => toasts.children.map((c) => c.textContent);

// ---- which errors are the limit ----
assert.equal(O.linkThrottled(new Error(THROTTLED)), true, 'as the page receives it');
assert.equal(O.linkThrottled(new Error('openExternal: too many links in a short time')), true, 'the main process words alone');
assert.equal(O.linkThrottled(THROTTLED), true, 'a string');
assert.equal(O.linkThrottled(new Error('openExternal: only from a click or key press')), false, 'no user activation');
assert.equal(O.linkThrottled(new Error("Error invoking remote method 'lf:open-external': TypeError: bad url")), false);
assert.equal(O.linkThrottled(null), false);
assert.equal(O.linkThrottled(undefined), false);
assert.equal(O.linkThrottled({}), false);

// ---- opened: no notice ----
O.openExternal('https://example.com/a');
await settle();
assert.equal(calls, 1);
assert.equal(toasts.children.length, 0, 'nothing to say when the link opened');

// ---- refused by the limit: one notice, in the UI language ----
answer = () => Promise.reject(new Error(THROTTLED));
O.openExternal('https://example.com/b');
await settle();
assert.equal(calls, 2);
assert.equal(toasts.children.length, 1, 'one notice');
const zh = I.t('common.link_throttled');
assert.equal(zh, '链接打开得太频繁，请过几秒再试。');
assert.equal(texts()[0], zh);
assert.equal(toasts.children[0].dataset.kind, 'info', 'an ordinary notice, not an error');
assert.equal(toasts.children[0].getAttribute('role'), 'status');
toasts.replaceChildren();

// ---- refused for any other reason: silent ----
answer = () => Promise.reject(new Error('openExternal: only from a click or key press'));
O.openExternal('https://example.com/c');
await settle();
answer = () => Promise.reject(new Error("Error invoking remote method 'lf:open-external': TypeError: openExternal: http, https and mailto only"));
O.openExternal('https://example.com/d');
await settle();
assert.equal(calls, 4);
assert.equal(toasts.children.length, 0, 'other failures stay silent');

// ---- the shared catch, as settings_update.js and update_hint.js use it ----
await I.setLocale('en');
await Promise.resolve(Promise.reject(new Error(THROTTLED))).catch(O.externalLinkFailed);
assert.equal(texts()[0], 'Links are being opened too often. Try again in a few seconds.');
O.externalLinkFailed(new Error('something else'));
assert.equal(toasts.children.length, 1);

console.log('link throttled checks: ok');
// the notices' timers (4 s) would keep node waiting
process.exit(0);
