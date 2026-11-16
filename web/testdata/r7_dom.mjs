// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// The DOM stand-in of the round 7 checks (r7_dashboard_test.mjs,
// r7_app_test.mjs), as in a11y_test.mjs, with what app.js needs besides: a
// page found by id, window events, a working localStorage, a controllable
// clock and interval ticks, EventSource and Notification stand-ins. Importing
// it sets the globals; the web modules are imported by each check after it.
// Stand-in nodes are compared by identity (assert.ok(a === b)), never with
// equal / deepEqual.

import nodeAssert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

export const assert = {
  ok: nodeAssert.ok,
  // objects (stand-in nodes) by identity, with a short message
  equal(a, b, msg) {
    if ((a !== null && typeof a === 'object') || (b !== null && typeof b === 'object')) {
      if (a !== b) throw new nodeAssert.AssertionError({ message: `${msg || 'not equal'} (got ${a === null ? 'null' : typeof a === 'object' ? 'an object' : String(a)})` });
      return;
    }
    nodeAssert.equal(a, b, msg);
  },
  deepEqual(a, b, msg) {
    const plain = (v) => Array.isArray(v) && v.every((x) => x === null || typeof x !== 'object');
    if (!plain(a) || !plain(b)) throw new Error('deepEqual: arrays of plain values only');
    nodeAssert.deepEqual(a, b, msg);
  },
};

// ---- DOM stand-in (as in a11y_test.mjs) ----

const MAX_DEPTH = 10000;
function up(n, fn) {
  for (let i = 0; n; n = n.parentNode) {
    if (++i > MAX_DEPTH) throw new Error('stand-in DOM: parent chain too long (a cycle?)');
    if (fn(n)) return n;
  }
  return null;
}

// MutationObserver: every observer hears of every change of the tree, once
// per microtask (the targets and options are not modelled); the deliveries
// are bounded, so an observer that changes the tree each time fails fast
const observers = new Set();
let deliveries = 0;
function mutated() {
  for (const o of observers) {
    if (o.queued) continue;
    o.queued = true;
    queueMicrotask(() => {
      o.queued = false;
      if (++deliveries > 200000) throw new Error('stand-in DOM: MutationObserver deliveries without end');
      if (observers.has(o)) o.fn([], o);
    });
  }
}

export class Node {
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
    mutated();
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
  get nextSibling() {
    if (!this.parentNode) return null;
    const c = this.parentNode.children;
    return c[c.indexOf(this) + 1] || null;
  }
  replaceWith(n) {
    const p = this.parentNode;
    if (!p) return;
    if (n === this) return;
    n.remove();
    const c = p.children;
    c.splice(c.indexOf(this), 1, n);
    n.parentNode = p;
    this.parentNode = null;
    mutated();
  }
}
export class Text extends Node {
  constructor(s) {
    super();
    this.data = String(s);
    this.children = [];
  }
  get textContent() {
    return this.data;
  }
}
export class El extends Node {
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
  hasAttribute(k) {
    return k in this.attrs;
  }
  append(...cs) {
    for (const c0 of cs) {
      const c = c0 instanceof Node ? c0 : new Text(c0);
      c.remove();
      c.parentNode = this;
      this.children.push(c);
    }
    mutated();
  }
  prepend(...cs) {
    const old = [...this.children];
    for (const c of old) c.remove();
    this.append(...cs, ...old);
  }
  insertBefore(n, ref) {
    if (!ref) return this.append(n);
    n.remove();
    this.children.splice(this.children.indexOf(ref), 0, n);
    n.parentNode = this;
    mutated();
    return n;
  }
  replaceChildren(...cs) {
    for (const c of [...this.children]) c.remove();
    this.append(...cs);
  }
  contains(n) {
    return !!up(n, (x) => x === this);
  }
  focus() {
    doc.activeElement = this;
  }
  click() {
    this.dispatchEvent(event('click'));
  }
  blur() {
    if (doc.activeElement === this) doc.activeElement = body;
  }
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
    if (sel === ':focus-visible') return true;
    const m = /^([a-z][a-z0-9]*)?((?:\.[\w-]+)*)(?:\[([\w-]+)(?:="([^"]*)")?\])?(:checked)?$/.exec(sel);
    if (!m) throw new Error('selector not supported here: ' + sel);
    const [, tag, classes, attr, val, checked] = m;
    if (checked && !('checked' in this.attrs)) return false;
    if (tag && this.tagName !== tag.toUpperCase()) return false;
    const own = this.className.split(/\s+/);
    for (const c of classes.split('.').filter(Boolean)) if (!own.includes(c)) return false;
    if (attr) {
      const v = attr.startsWith('data-') ? this.dataset[attr.slice(5).replace(/-(\w)/g, (_, c) => c.toUpperCase())] : this.getAttribute(attr);
      if (v == null || (val !== undefined && v !== val)) return false;
    }
    return true;
  }
  closest(sel) {
    return up(this, (n) => n instanceof El && n.matches(sel));
  }
  querySelectorAll(sel) {
    if (sel.startsWith(':scope > ')) return this.children.filter((c) => c instanceof El && c.matches(sel.slice(9)));
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
  get firstChild() {
    return this.children[0] || null;
  }
  // components_ui.js keepChildren reads it (round 9 WV-10: the settings
  // page's components and Colab sections)
  get childNodes() {
    return this.children;
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
}

// the element with that id in the page (the first in document order)
function byId(from, id) {
  for (const c of from.children) {
    if (!(c instanceof El)) continue;
    if (c.attrs.id === id) return c;
    const found = byId(c, id);
    if (found) return found;
  }
  return null;
}

export function event(type, o = {}) {
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

export const dialogs = [];
// a working storage (app.js keeps the notified keys there)
const storage = () => {
  const m = new Map();
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k), clear: () => m.clear() };
};
export const root = new El('html');
export const body = new El('body');
root.append(body);
export const toastsEl = new El('div');
toastsEl.attrs.id = 'toasts';
body.append(toastsEl);
export const doc = {
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
  getElementById: (id) => byId(root, id),
  createElement: (t) => new El(t),
  createElementNS: (_, t) => new El(t),
  createTextNode: (s) => new Text(s),
  visibilityState: 'visible',
  focused: true,
  hasFocus() {
    return this.focused;
  },
  title: 'LectureFlow',
};

// a clock the checks move (Date.now; new Date() stays real), and the
// interval callbacks, fired by tick() only (each tick is one second)
export const clock = { now: Date.now() };
Date.now = () => clock.now;
export const intervals = new Map();
let nextTimer = 1;
export function tick(seconds = 1) {
  for (let i = 0; i < seconds; i++) {
    clock.now += 1000;
    for (const fn of [...intervals.values()]) fn();
  }
}

// window events (hashchange, resize, localechange …), fired by fireWindow
const winListeners = {};
export function fireWindow(type, ev = {}) {
  for (const fn of [...(winListeners[type] || [])]) fn({ type, ...ev });
}

// EventSource: every stream opened, to be opened (open()) or dropped (drop())
export const sources = [];
class EventSourceStandIn {
  constructor(url) {
    this.url = url;
    this.listeners = {};
    this.closed = false;
    this.onerror = null;
    sources.push(this);
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  close() {
    this.closed = true;
  }
  emit(type, data) {
    for (const fn of [...(this.listeners[type] || [])]) fn({ type, data: data === undefined ? '' : JSON.stringify(data) });
  }
  open() {
    this.emit('open');
  }
  drop() {
    if (this.onerror) this.onerror(new Error('dropped'));
  }
}

// Notification: the system notifications shown ({title, body, tag})
export const notes = [];
class NotificationStandIn {
  constructor(title, o = {}) {
    notes.push({ title, body: o.body, tag: o.tag });
  }
  close() {}
}
NotificationStandIn.permission = 'granted';
NotificationStandIn.requestPermission = async () => 'granted';

Object.assign(globalThis, {
  Node,
  location: { search: '', hash: '#/', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: storage(),
  sessionStorage: storage(),
  document: doc,
  addEventListener(type, fn) {
    (winListeners[type] ||= []).push(fn);
  },
  removeEventListener(type, fn) {
    const l = winListeners[type] || [];
    if (l.includes(fn)) l.splice(l.indexOf(fn), 1);
  },
  dispatchEvent(ev) {
    fireWindow(ev.type, ev);
    return true;
  },
  CustomEvent: class {
    constructor(type, o = {}) {
      this.type = type;
      this.detail = o.detail;
    }
  },
  MutationObserver: class {
    constructor(fn) {
      this.fn = fn;
    }
    observe() {
      observers.add(this);
    }
    disconnect() {
      observers.delete(this);
    }
  },
  history: {
    replaceState(_, __, url) {
      if (url) globalThis.location.hash = url;
    },
  },
  scrollTo() {},
  scrollBy() {},
  scrollY: 0,
  focus() {},
  // the views' observers of what is on screen: never fire here
  IntersectionObserver: class {
    observe() {}
    unobserve() {}
    disconnect() {}
  },
  ResizeObserver: class {
    observe() {}
    unobserve() {}
    disconnect() {}
  },
  EventSource: EventSourceStandIn,
  Notification: NotificationStandIn,
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  innerHeight: 900,
  innerWidth: 1280,
  CSS: { escape: (s) => String(s) },
  setInterval: (fn) => {
    const id = nextTimer++;
    intervals.set(id, fn);
    return id;
  },
  clearInterval: (id) => intervals.delete(id),
  requestAnimationFrame: (fn) => setTimeout(fn, 0),
});
Object.defineProperty(globalThis, 'navigator', { value: { languages: ['zh-CN'], language: 'zh-CN', userAgent: 'node' }, configurable: true });
globalThis.window = globalThis;

export const base = process.argv[2];

// fetch: the UI catalogs from the web directory; every other request is
// answered by net.answer(method, url, body) after a short delay and recorded
// in `sent` ("METHOD url"); net.down makes every request fail as a network
// failure does (the server gone)
export const sent = [];
export const net = { answer: () => ({ status: 200, body: {} }), down: false };
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const cat = JSON.parse(await readFile(new URL(u, base), 'utf8'));
    const text = JSON.stringify(cat);
    return { ok: true, status: 200, json: async () => cat, text: async () => text };
  }
  const method = init.method || 'GET';
  sent.push(`${method} ${u}`);
  await new Promise((r) => setTimeout(r, 2));
  if (net.down) throw new TypeError('Failed to fetch');
  // an answer may be a promise: the request is in flight until it settles
  const a = await net.answer(method, u, init.body ? JSON.parse(init.body) : undefined);
  const text = JSON.stringify(a.body);
  return { ok: a.status < 300, status: a.status, text: async () => text };
};
export const settle = (ms = 30) => new Promise((r) => setTimeout(r, ms));
// waits until fn() holds, at most tries × 10 ms (fails fast, never hangs)
export async function until(fn, what, tries = 200) {
  for (let i = 0; i < tries; i++) {
    if (fn()) return;
    await settle(10);
  }
  throw new nodeAssert.AssertionError({ message: `timed out waiting for: ${what}` });
}

export const fk = (k) => doc.querySelector(`[data-fkey="${k}"]`);
// the toasts shown, as "kind: message"; taken off for the next check
export const told = () => {
  const out = toastsEl.children.map((el) => `${el.dataset.kind}: ${el.dataset.msg}`);
  for (const el of [...toastsEl.children]) {
    clearTimeout(el._timer);
    el.remove();
  }
  return out;
};
// closes the open dialogs with their cancel (not the primary / danger) button
export async function cancelDialogs() {
  await settle();
  for (const d of dialogs.filter((x) => x.open)) {
    const no = d.querySelectorAll('button').find((b) => !/btn-(danger|primary)/.test(b.className) && b.textContent.trim());
    if (no) no.click();
    d.open = false;
  }
  await settle();
}
// confirms the open dialogs: a choice dialog's form is submitted (its
// preselected choice), another dialog's primary / danger button clicked
export async function confirmDialogs() {
  await settle();
  for (const d of dialogs.filter((x) => x.open)) {
    const ok = d.querySelectorAll('button').find((b) => /btn-(danger|primary)/.test(b.className));
    const form = ok && ok.getAttribute('type') === 'submit' ? ok.closest('form') : null;
    if (form) form.dispatchEvent(event('submit'));
    else if (ok) ok.click();
    d.open = false;
  }
  await settle();
}
