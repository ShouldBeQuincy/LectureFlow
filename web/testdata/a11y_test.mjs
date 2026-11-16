// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Keyboard and screen-reader fixes from the UI sweep (.claude/research/test-ui.md):
//   L14  an inline action of a course-page row ("启动", "重试", a stage
//        card's "取消") re-renders the list; the focus stays in the row: on
//        the same control when it is still there, else on the row's new
//        action in its place or the next control of the row (dom.js
//        preserveFocus, data-fnext), not on the page;
//   L16  a stage card holds buttons, so it is no tooltip: the segment
//        controls it (aria-controls, aria-expanded that follows the card),
//        the card is a group named by its stage; Tab enters it, Esc closes
//        it and gives the focus back to the segment; an upload's <progress>
//        is named by its file.
// The modules run on the DOM stand-in of ui320_test.mjs; web/a11y_test.go
// starts it with the web directory as a file: URL.

import nodeAssert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

// ---- assertions that stay small ----
// Node's own message for a failed equal() of two objects inspects both
// whole (depth 1000, every array item, no custom inspect) and diffs the
// texts. Two nodes of this stand-in DOM are the whole page each (parents,
// children, listeners): with the transcript's 450 sentences that took
// hundreds of gigabytes (2026-11-11). Objects are compared by identity here
// and named briefly when they differ; other values go to node's assert.
function brief(v) {
  if (v === null || typeof v !== 'object') return String(v);
  if (v === doc) return 'document';
  if (!(v instanceof Node)) return Object.prototype.toString.call(v);
  if (!(v instanceof El)) return `#text(${String(v.data).slice(0, 30)})`;
  const ds = Object.entries(v.dataset || {}).map(([k, x]) => ` data-${k}="${x}"`).join('');
  const cls = v.className ? ` class="${v.className}"` : '';
  return `<${v.tagName.toLowerCase()}${cls}${ds}>${v.isConnected ? '' : ' (removed)'}`;
}
function fail(how, a, b, msg) {
  throw new nodeAssert.AssertionError({ message: `${msg || 'assertion failed'} (${how}: ${brief(a)}, ${brief(b)})` });
}
const isObj = (v) => v !== null && typeof v === 'object';
const assert = {
  ok: nodeAssert.ok,
  deepEqual: nodeAssert.deepEqual,
  equal(a, b, msg) {
    if (isObj(a) || isObj(b)) {
      if (a !== b) fail('got, want', a, b, msg);
    } else nodeAssert.equal(a, b, msg);
  },
  notEqual(a, b, msg) {
    if (isObj(a) || isObj(b)) {
      if (a === b) fail('both', a, b, msg);
    } else nodeAssert.notEqual(a, b, msg);
  },
};

// ---- DOM stand-in (as in ui320_test.mjs) ----


// ---- DOM stand-in ----

// The way up a stand-in tree is bounded: a cycle (a node appended into its
// own subtree) fails at once instead of looping.
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
    // realFocus: a removed focused element leaves the focus on the page, as in a browser
    if (realFocus && this.contains && this.contains(doc.activeElement)) doc.activeElement = body;
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
  // events bubble up to the document (the html element's parent here)
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
  // what components_ui.js keepChildren asks (round 9 WV-10: the settings
  // page's components and Colab sections put their children in place;
  // removeChild is El.prototype's, below)
  get childNodes() {
    return this.children;
  }
  insertBefore(n, ref) {
    if (!ref) {
      this.append(n);
      return n;
    }
    // a browser throws NotFoundError, before moving anything (R91 low-6)
    if (ref.parentNode !== this) throw new Error('stand-in DOM: insertBefore: the reference node is not a child');
    n.remove();
    this.children.splice(this.children.indexOf(ref), 0, n);
    n.parentNode = this;
    return n;
  }
  contains(n) {
    return !!up(n, (x) => x === this);
  }
  focus(opts) {
    lastFocusOpts = opts;
    const prev = doc.activeElement;
    if (prev === this) return;
    if (!focusable(this)) return;
    doc.activeElement = this;
    if (prev) prev.dispatchEvent(event('focusout', { relatedTarget: this }));
    this.dispatchEvent(event('focusin', { relatedTarget: prev }));
  }
  click() {
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
  getBoundingClientRect() {
    return { top: 0, bottom: 0, left: 0, right: 0 };
  }
  matches(sel) {
    // how the focus came (R19): focusBy 'keyboard' or 'mouse'; null throws,
    // as a browser that does not know the pseudo-class would
    if (sel === ':focus-visible') {
      if (!focusBy) throw new Error('selector not supported here: ' + sel);
      return focusBy === 'keyboard';
    }
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
    // ":scope > x": the children only (view_transcript.js)
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
}

function event(type, o = {}) {
  return {
    type,
    bubbles: type !== 'pointerenter' && type !== 'pointerleave',
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
let focusBy = null;
// realFocus (T6 #1): focus as a browser has it where it matters here. A
// control in a stage card is shown (and focusable) only while the focus is
// in the card's stage (:focus-within; the pointer is not modelled) and Esc
// did not close it; a disabled control takes no focus; a removed focused
// element leaves the focus on the page.
let realFocus = false;
let lastFocusOpts; // the options of the last focus() call
function focusable(el) {
  if (!realFocus) return true;
  if (el.disabled) return false; // a disabled control takes no focus
  for (let n = el; n; n = n.parentNode) {
    if (n.classList && n.classList.contains('seg-card')) {
      const wrap = n.parentNode;
      return !!wrap && !wrap.classList.contains('is-dismissed') && wrap.contains(doc.activeElement);
    }
  }
  return true;
}
const store = { getItem: () => null, setItem() {}, removeItem() {} };
const root = new El('html');
const body = new El('body');
root.append(body);
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
  getElementById: () => null,
  createElement: (t) => new El(t),
  createElementNS: (_, t) => new El(t),
  createTextNode: (s) => new Text(s),
  visibilityState: 'visible',
};

// fake timers (setInterval) and clock (Date.now); setTimeout stays real
const intervals = new Map();
let nextTimer = 1;
let clock = Date.UTC(2026, 9, 27, 12, 0, 0);
const realNow = Date.now;
Date.now = () => clock;
const second = () => {
  clock += 1000;
  for (const fn of [...intervals.values()]) fn();
};

Object.assign(globalThis, {
  Node,
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: doc,
  addEventListener() {},
  removeEventListener() {},
  dispatchEvent: () => true,
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  innerHeight: 900,
  CSS: { escape: (s) => String(s) },
  setInterval: (fn) => {
    const id = nextTimer++;
    intervals.set(id, fn);
    return id;
  },
  clearInterval: (id) => intervals.delete(id),
});
globalThis.window = globalThis;

// what lecture_list.js needs beyond ui320's stand-in: clear() (dom.js)
// takes the children off one by one (removeChild: keepChildren too)
Object.defineProperty(El.prototype, 'firstChild', {
  get() {
    return this.children[0] || null;
  },
});
El.prototype.removeChild = function (c) {
  c.remove();
  return c;
};
// disabled reflects the attribute, as in a browser (the dashboard's buttons
// are built disabled while their request runs)
Object.defineProperty(El.prototype, 'disabled', {
  get() {
    return 'disabled' in this.attrs;
  },
  set(v) {
    if (v) this.attrs.disabled = '';
    else delete this.attrs.disabled;
  },
});

// the stand-in itself (R91 low-6): insertBefore with a reference node that
// is not a child throws, as a browser's does (NotFoundError), and moves
// nothing; before a child, or null, it inserts as before
{
  const p = new El('div');
  const a = new El('i');
  p.append(a);
  const stranger = new El('b');
  new El('div').append(stranger);
  const n = new El('span');
  let threw = null;
  try {
    p.insertBefore(n, stranger);
  } catch (e) {
    threw = e;
  }
  assert.ok(threw && /not a child/.test(threw.message), 'stand-in: insertBefore with a reference that is not a child throws');
  assert.ok(n.parentNode === null && p.children.length === 1 && p.children[0] === a, 'stand-in: and moves nothing');
  assert.ok(p.insertBefore(n, a) === n && p.children[0] === n && p.children[1] === a && n.parentNode === p, 'stand-in: before a child, inserted there');
  const u = new El('u');
  assert.ok(p.insertBefore(u, null) === u && p.children[2] === u && u.parentNode === p, 'stand-in: before null, appended');
}

const base = process.argv[2];

// fetch: the UI catalogs from the web directory; the requests of the
// actions, answered by `answer` after a short delay and recorded in `posts`
const posts = [];
let answer = () => ({ status: 200, body: {} });
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const cat = JSON.parse(await readFile(new URL(u, base), 'utf8'));
    const text = JSON.stringify(cat);
    return { ok: true, status: 200, json: async () => cat, text: async () => text };
  }
  const method = init.method || 'GET';
  if (method !== 'GET') posts.push(`${method} ${u}`);
  await new Promise((r) => setTimeout(r, 5));
  const a = answer(method, u);
  const text = JSON.stringify(a.body);
  return { ok: a.status < 300, status: a.status, text: async () => text };
};
globalThis.requestAnimationFrame = (fn) => setTimeout(fn, 0);
const settle = (ms = 40) => new Promise((r) => setTimeout(r, ms));

const D = await import(new URL('dom.js', base).href);
const P = await import(new URL('pipeline.js', base).href);
const L = await import(new URL('lecture_list.js', base).href);
const W = await import(new URL('widgets.js', base).href);
const U = await import(new URL('util.js', base).href);
const I = await import(new URL('i18n.js', base).href);
await I.setLocale('zh-CN');

const fk = (k) => doc.querySelector(`[data-fkey="${k}"]`);

// ---- dom.js preserveFocus: the fallbacks of data-fnext ----

{
  const box = new El('div');
  body.append(box);
  const draw = (keys) => {
    box.replaceChildren();
    for (const k of keys) {
      const b = new El('button');
      b.dataset.fkey = k;
      if (k === 'b') b.dataset.fnext = 'x y c';
      if (k === 'y') b.disabled = true;
      box.append(b);
    }
  };
  draw(['a', 'b', 'c']);
  fk('b').focus();
  D.preserveFocus(() => draw(['a', 'b', 'c']));
  assert.equal(doc.activeElement, fk('b'), 'the same control while it is there');
  D.preserveFocus(() => draw(['a', 'y', 'c']));
  assert.equal(doc.activeElement, fk('c'), 'the first fallback present and enabled');
  const a = fk('a');
  a.focus();
  D.preserveFocus(() => draw(['c']));
  assert.equal(doc.activeElement, a, 'no key and no fallback present: nothing is focused');
  // R19: the fallbacks are for keyboard focus; a mouse click's focus does not move
  focusBy = 'keyboard';
  draw(['a', 'b', 'c']);
  fk('b').focus();
  D.preserveFocus(() => draw(['a', 'y', 'c']));
  assert.equal(doc.activeElement, fk('c'), 'keyboard focus (:focus-visible): the fallback');
  // still there but disabled (the dashboard, while the request runs, R21):
  // no fallback takes the focus (Enter there would do something else); it
  // waits on the page until the control is enabled again or gone
  const busy = () => {
    draw(['a', 'b', 'x', 'c']);
    fk('b').disabled = true;
    fk('x').hidden = true;
  };
  draw(['a', 'b', 'c']);
  fk('b').focus();
  D.preserveFocus(busy);
  assert.equal(doc.activeElement, body, 'disabled in place: the focus waits on the page, on no fallback');
  D.preserveFocus(busy);
  assert.equal(doc.activeElement, body, 'still disabled: still waiting');
  D.preserveFocus(() => draw(['a', 'b', 'c']));
  assert.equal(doc.activeElement, fk('b'), 'enabled again (the request failed): the control itself');
  D.preserveFocus(busy);
  assert.equal(doc.activeElement, body);
  D.preserveFocus(() => {
    draw(['a', 'x', 'c']);
    fk('x').hidden = true;
  });
  assert.equal(doc.activeElement, fk('c'), 'gone once done: the first fallback shown and enabled');
  // focus taken elsewhere meanwhile ends the wait
  draw(['a', 'b', 'c']);
  fk('b').focus();
  D.preserveFocus(busy);
  fk('a').focus();
  D.preserveFocus(() => draw(['a', 'b', 'c']));
  assert.equal(doc.activeElement, fk('a'), 'focus moved by the user stays');
  body.focus();
  D.preserveFocus(() => draw(['a', 'b', 'c']));
  assert.equal(doc.activeElement, body, 'and the wait is over: the control does not take it back');
  // R22 (2): the wait ends as soon as the focus moves (focusin), so a return
  // to the page with no re-render in between (a click on its background)
  // does not bring it back to life
  draw(['a', 'b', 'c']);
  fk('b').focus();
  D.preserveFocus(busy);
  assert.equal(doc.activeElement, body);
  fk('a').focus();
  body.focus();
  D.preserveFocus(() => draw(['a', 'b', 'c']));
  assert.equal(doc.activeElement, body, 'moved away and back to the page: the wait is over');
  // another part of the page re-rendered meanwhile (the settings' other
  // blocks, T6 #7) keeps the wait; enabled again, the focus is back
  draw(['a', 'b', 'c']);
  fk('b').focus();
  D.preserveFocus(busy);
  D.preserveFocus(() => {});
  assert.equal(doc.activeElement, body, 'another block re-rendered: still waiting');
  D.preserveFocus(() => draw(['a', 'b', 'c']));
  assert.equal(doc.activeElement, fk('b'), 'the test is over: back on the control');
  // R22 (3): disabled in place, not drawn anew: a browser may leave the
  // focus on the disabled control; it waits there, enabled again it is
  // still there
  draw(['a', 'b', 'c']);
  const inPlace = fk('b');
  inPlace.focus();
  inPlace.disabled = true;
  D.preserveFocus(() => {});
  assert.equal(doc.activeElement, inPlace, 'disabled in place: the focus stays on it');
  inPlace.disabled = false;
  D.preserveFocus(() => {});
  assert.equal(doc.activeElement, inPlace, 'enabled again in place: still on it');
  // the focus on the disabled control is the wait's, not a new start: the
  // control may no longer say how it got the focus (no :focus-visible), the
  // wait keeps the fallbacks it started with (R22 mutation M4)
  inPlace.disabled = true;
  D.preserveFocus(() => {});
  focusBy = 'mouse';
  D.preserveFocus(() => {});
  assert.equal(doc.activeElement, inPlace);
  D.preserveFocus(() => draw(['a', 'c']));
  assert.equal(doc.activeElement, fk('c'), 'gone once done: the fallback the wait started with');
  focusBy = 'keyboard';
  // R22 (1): a control inside a dialog (the new-course wizard): the wait
  // ends when the focus moves within that dialog (the next step's title);
  // a confirmation in another dialog keeps it
  {
    const dlg = new El('dialog');
    const title = new El('h2');
    const inner = new El('div');
    dlg.append(title, inner);
    body.append(dlg);
    const drawIn = (off) => {
      inner.replaceChildren();
      for (const k of ['wiz:primary', 'wiz:back']) {
        const b = new El('button');
        b.dataset.fkey = k;
        inner.append(b);
      }
      if (off) fk('wiz:primary').disabled = true;
    };
    drawIn(false);
    fk('wiz:primary').focus();
    D.preserveFocus(() => drawIn(true));
    assert.equal(doc.activeElement, body, 'in a dialog: waiting');
    title.focus(); // the next step
    body.focus(); // a row of the step removed with the keyboard
    D.preserveFocus(() => drawIn(false));
    assert.equal(doc.activeElement, body, 'the focus moved within its own dialog: the wait is over');
    drawIn(false);
    fk('wiz:primary').focus();
    D.preserveFocus(() => drawIn(true));
    const ask = new El('dialog');
    const yes = new El('button');
    ask.append(yes);
    body.append(ask);
    yes.focus();
    ask.remove();
    body.focus();
    D.preserveFocus(() => drawIn(false));
    assert.equal(doc.activeElement, fk('wiz:primary'), 'a confirmation in another dialog keeps the wait');
    // R29 (suggestion 1): the control's dialog closed while it waits: the
    // wait is void; the same wizard opened again does not take the focus
    // lost on the page
    drawIn(false);
    fk('wiz:primary').focus();
    D.preserveFocus(() => drawIn(true));
    dlg.remove();
    const again = new El('dialog');
    body.append(again);
    again.append(inner);
    body.focus();
    D.preserveFocus(() => drawIn(false));
    assert.equal(doc.activeElement, body, 'its dialog closed: the wait is void');
    again.remove();
    body.focus();
  }
  // R29 (3): a pointer press on what takes no focus (text, the page's
  // background) ends the wait; one in another dialog (a confirmation) does not
  {
    const text = new El('p');
    body.append(text);
    draw(['a', 'b', 'c']);
    fk('b').focus();
    D.preserveFocus(busy);
    text.dispatchEvent(event('pointerdown'));
    D.preserveFocus(() => draw(['a', 'b', 'c']));
    assert.equal(doc.activeElement, body, 'a press on the page: the wait is over');
    draw(['a', 'b', 'c']);
    fk('b').focus();
    D.preserveFocus(busy);
    const ask = new El('dialog');
    const yes = new El('button');
    ask.append(yes);
    body.append(ask);
    yes.dispatchEvent(event('pointerdown'));
    ask.remove();
    D.preserveFocus(() => draw(['a', 'b', 'c']));
    assert.equal(doc.activeElement, fk('b'), 'a press in another dialog keeps the wait');
    text.remove();
  }
  // R29 (suggestion 2): the view moves the focus itself while it re-renders
  // (focusin ends the wait): the focus stays where the view put it
  draw(['a', 'b', 'c']);
  fk('b').focus();
  D.preserveFocus(busy);
  D.preserveFocus(() => {
    draw(['a', 'b', 'c']);
    fk('a').focus();
  });
  assert.equal(doc.activeElement, fk('a'), 'moved by the view: not taken back');
  draw(['a', 'b', 'c']);
  fk('b').focus();
  D.preserveFocus(busy);
  D.preserveFocus(() => {
    draw(['a', 'b', 'c']);
    fk('b').disabled = true;
    fk('c').focus();
  });
  assert.equal(doc.activeElement, fk('c'), 'moved by the view, the control still disabled');
  body.focus();
  D.preserveFocus(() => draw(['a', 'b', 'c']));
  assert.equal(doc.activeElement, body, 'and no new wait was started');
  body.focus();
  focusBy = 'mouse';
  draw(['a', 'b', 'c']);
  const clicked = fk('b');
  clicked.focus();
  D.preserveFocus(() => draw(['a', 'y', 'c']));
  assert.equal(doc.activeElement, clicked, 'mouse focus: no fallback, nothing is focused');
  focusBy = 'mouse';
  draw(['a', 'b', 'c']);
  fk('b').focus();
  D.preserveFocus(() => draw(['a', 'b', 'c']));
  assert.equal(doc.activeElement, fk('b'), 'mouse focus: the same control while it is there');
  focusBy = null;
  box.remove();
  body.focus();
}

// ---- L14: the course page's rows ----

const stage = (name, status, o = {}) => ({ stage: name, status, display_status: status, progress: status === 'done' ? 1 : 0, attempt: 0, error: null, allowed_actions: [], started_at: null, finished_at: null, duration_ms: null, ...o });
const names = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
function lecture(id, o = {}) {
  return {
    id, course_id: 'C1', title: `Lecture ${id}`, label: 'L01', recorded_at: '', numbered: false, week: 0, kind: 'lecture', draft: false, created_at: '2026-10-27T10:00:00Z',
    duration_ms: 0, counts: { sentences: 0, patches_pending: 0 }, exports: [], materials: [], playable: false, lecture_actions: [],
    stages: names.map((n) => stage(n, 'pending')), ...o,
  };
}
const ctx = { system: () => null, resources: () => null };
let list;
let shown = [];
const upsert = (ls) => {
  for (const l of ls) shown = shown.map((x) => (x.id === l.id ? l : x));
  list.render(shown);
};
list = L.createLectureList({ ctx, onLectures: upsert, onDeleted() {}, course: () => null, filtered: () => false });
body.append(list.table);

async function confirmDialogs() {
  await settle();
  for (const d of dialogs.filter((x) => x.open)) {
    const ok = d.querySelectorAll('button').find((b) => /btn-(danger|primary)/.test(b.className));
    if (ok) ok.click();
    d.open = false;
  }
  await settle();
}

{
  // "启动" by keyboard: the lecture is queued and its row has no action in
  // that place any more; the focus goes to the row's "更多操作", not the page
  shown = [lecture('A', { lecture_actions: ['launch'] }), lecture('B', { lecture_actions: ['launch'] })];
  list.render(shown);
  const launch = fk('lec:A:launch');
  assert.ok(launch, 'a draft row has 启动');
  launch.focus();
  answer = (m, u) => (u.endsWith('/A/launch') ? { status: 200, body: lecture('A', { stages: names.map((n, i) => stage(n, i ? 'queued' : 'running')) }) } : { status: 404, body: {} });
  posts.length = 0;
  launch.click();
  await confirmDialogs();
  assert.deepEqual(posts, ['POST /api/lectures/A/launch']);
  assert.equal(fk('lec:A:launch'), null, 'launched: 启动 is gone');
  assert.ok(fk('lec:B:launch'), 'the other row keeps its 启动');
  assert.equal(doc.activeElement, fk('lec:A:more'), 'the focus stays in the row (更多操作)');

  // a failed stage's "重试": the row's action becomes "播放" (same place)
  shown = [lecture('A', { playable: true, stages: names.map((n) => (n === 'translate' ? stage(n, 'failed', { error: 'x', allowed_actions: ['retry'] }) : stage(n, n === 'export' ? 'pending' : 'done'))) })];
  list.render(shown);
  const retry = fk('lec:A:retry');
  assert.ok(retry, 'a failed row has 重试');
  retry.focus();
  answer = () => ({ status: 200, body: lecture('A', { playable: true, stages: names.map((n) => stage(n, n === 'translate' ? 'queued' : n === 'export' ? 'pending' : 'done')) }) });
  retry.click();
  await confirmDialogs();
  assert.equal(fk('lec:A:retry'), null);
  assert.equal(doc.activeElement, fk('lec:A:play'), "the row's new action in the same place");

  // a background refresh with the control still there keeps it
  list.render([...shown]);
  assert.equal(doc.activeElement, fk('lec:A:play'));

  // a stage card's "取消": the card's buttons are gone with the stage; the
  // focus goes back to the stage's segment
  shown = [lecture('A', { stages: names.map((n) => (n === 'asr' ? stage(n, 'running', { allowed_actions: ['cancel'] }) : stage(n, n === 'prep' ? 'done' : 'queued'))) })];
  list.render(shown);
  fk('lec:A:seg:asr').focus();
  const cancel = fk('lec:A:act:asr:cancel');
  assert.ok(cancel, "the running stage's card has 取消");
  cancel.focus();
  answer = () => ({ status: 200, body: lecture('A', { stages: names.map((n) => stage(n, n === 'prep' ? 'done' : n === 'asr' ? 'cancelled' : 'pending', n === 'asr' ? { allowed_actions: ['retry'] } : {})) }) });
  cancel.click();
  await confirmDialogs();
  assert.equal(fk('lec:A:act:asr:cancel'), null, 'cancelled: the card has no 取消');
  assert.equal(doc.activeElement, fk('lec:A:seg:asr'), "the focus is on the stage's segment");
  body.focus();

  // R19: the same 取消 clicked with the mouse: the focus does not go to the
  // segment, so the card does not stay open (:focus-within) once the pointer
  // has left
  focusBy = 'mouse';
  shown = [lecture('A', { stages: names.map((n) => (n === 'asr' ? stage(n, 'running', { allowed_actions: ['cancel'] }) : stage(n, n === 'prep' ? 'done' : 'queued'))) })];
  list.render(shown);
  // the pointer over the segment opens its card first (the course list
  // builds a card when it opens: lazyCards, round 8 WL-02)
  fk('lec:A:seg:asr').parentNode.dispatchEvent(event('pointerenter'));
  const mcancel = fk('lec:A:act:asr:cancel');
  mcancel.focus();
  mcancel.click();
  await confirmDialogs();
  assert.equal(fk('lec:A:act:asr:cancel'), null);
  const mseg = fk('lec:A:seg:asr');
  assert.notEqual(doc.activeElement, mseg, 'a mouse click: the focus is not moved to the segment');
  assert.ok(!mseg.parentNode.contains(doc.activeElement), 'nothing in the stage holds the focus');
  assert.equal(mseg.getAttribute('aria-expanded'), 'false', 'the card is not kept open');
  focusBy = null;
  body.focus();
  list.table.remove();
}

// ---- W5 (T11 general 5): the course table's header row has a column
// header for each cell of a lecture row (the select-all box's column
// included), so a screen reader's column headers line up ----

{
  body.append(list.table);
  shown = [lecture('A', { lecture_actions: ['launch'] })];
  list.render(shown);
  const headRow = list.table.querySelector('div.lt-head');
  assert.ok(headRow, 'the header row');
  const heads = headRow.children.filter((c) => c instanceof El);
  assert.ok(
    heads.every((c) => c.getAttribute('role') === 'columnheader'),
    `every cell of the header row is a column header: ${heads.map((c) => `${c.className}=${c.getAttribute('role')}`).join(', ')}`,
  );
  const row = list.table.querySelectorAll('div[role="row"]').find((r) => r.querySelector('[data-fkey="lec:A:launch"]'));
  assert.ok(row, "lecture A's row");
  const cells = row.children.filter((c) => c instanceof El && c.getAttribute('role') === 'cell');
  assert.equal(heads.length, cells.length, 'as many column headers as cells in a lecture row');
  assert.ok(heads[0].contains(headRow.querySelector('input')), 'the first column header holds the select-all box');
  list.table.remove();
}

// ---- L16: the stage card is a disclosure, not a tooltip ----

{
  const st = stage('asr', 'running', { allowed_actions: ['cancel'], progress: 0.4 });
  const bar = P.pipelineBar([stage('prep', 'done'), st], { key: 'lec:L1', lecture: '', sys: () => null, res: () => null, onAction: () => Promise.resolve() });
  body.append(bar);
  const wrap = bar.querySelector('.seg-wrap[data-stage="asr"]');
  const seg = wrap.querySelector('.seg');
  const card = wrap.querySelector('.seg-card');
  // no tooltip anywhere in the bar, and no description pointing at a card with buttons
  assert.equal(bar.querySelectorAll('[role="tooltip"]').length, 0);
  assert.equal(seg.getAttribute('aria-describedby'), null);
  assert.ok(card.querySelector('button'), 'the card has buttons');
  assert.equal(card.getAttribute('role'), 'group');
  assert.equal(card.getAttribute('aria-label'), U.stageName('asr'), 'the card is named by its stage');
  assert.ok(U.stageName('asr'));
  assert.ok(card.getAttribute('id') && seg.getAttribute('aria-controls') === card.getAttribute('id'), 'the segment controls the card');
  assert.equal(seg.getAttribute('aria-expanded'), 'false', 'closed at first');

  // the pointer opens and closes it
  wrap.dispatchEvent(event('pointerenter'));
  assert.equal(seg.getAttribute('aria-expanded'), 'true');
  wrap.dispatchEvent(event('pointerleave'));
  assert.equal(seg.getAttribute('aria-expanded'), 'false');

  // keyboard: focus on the segment opens it; Tab into the card keeps it
  // open; Esc closes it and puts the focus back on the segment
  seg.focus();
  assert.equal(seg.getAttribute('aria-expanded'), 'true', 'focus opens it');
  const cancel = card.querySelector('button');
  cancel.focus();
  assert.equal(seg.getAttribute('aria-expanded'), 'true', 'the focus in the card keeps it open');
  const esc = event('keydown', { key: 'Escape' });
  cancel.dispatchEvent(esc);
  assert.equal(doc.activeElement, seg, 'Esc gives the focus back to the segment');
  assert.equal(seg.getAttribute('aria-expanded'), 'false', 'Esc closes it');
  // R19: Enter or Space (a click) on the segment opens it again; on an open
  // card it changes nothing
  seg.click();
  assert.equal(seg.getAttribute('aria-expanded'), 'true', 'the segment opens the card Esc closed');
  assert.ok(!wrap.classList.contains('is-dismissed'));
  seg.click();
  assert.equal(seg.getAttribute('aria-expanded'), 'true', 'a click on an open card keeps it open');
  // the focus leaves: closed; comes back: open again
  body.focus();
  assert.equal(seg.getAttribute('aria-expanded'), 'false');
  seg.focus();
  assert.equal(seg.getAttribute('aria-expanded'), 'true');
  body.focus();
  // the hover card under the pointer, Esc with the focus on the page
  wrap.dispatchEvent(event('pointerenter'));
  body.dispatchEvent(event('keydown', { key: 'Escape' }));
  assert.equal(seg.getAttribute('aria-expanded'), 'false');
  wrap.dispatchEvent(event('pointerleave'));
  // a click with the card closed by the pointer leaving (not by Esc) does nothing
  seg.click();
  assert.equal(seg.getAttribute('aria-expanded'), 'false');

  // D2 (T16): one card at a time. The focus on one segment, the pointer
  // over another: the newer opens, the older closes (as Esc closes it, the
  // focus stays on its segment); Enter or Space opens it again, closing
  // the hovered one; the next focus or hover opens it as before
  {
    const prepWrap = bar.querySelector('.seg-wrap[data-stage="prep"]');
    const prepSeg = prepWrap.querySelector('.seg');
    seg.focus();
    assert.equal(seg.getAttribute('aria-expanded'), 'true', 'focus opens the asr card');
    prepWrap.dispatchEvent(event('pointerenter'));
    assert.equal(prepSeg.getAttribute('aria-expanded'), 'true', 'the hovered card opens');
    assert.equal(seg.getAttribute('aria-expanded'), 'false', 'the focused card closes');
    assert.ok(wrap.classList.contains('is-dismissed'), 'closed as Esc closes it');
    assert.ok(doc.activeElement === seg, 'the focus stays on its segment');
    seg.click();
    assert.equal(seg.getAttribute('aria-expanded'), 'true', 'Enter on the segment opens it again');
    assert.equal(prepSeg.getAttribute('aria-expanded'), 'false', 'and closes the hovered one');
    prepWrap.dispatchEvent(event('pointerleave'));
    assert.ok(!prepWrap.classList.contains('is-dismissed'), 'the pointer gone: the next hover opens it again');
    assert.equal(seg.getAttribute('aria-expanded'), 'true');
    // the other way round: a hovered card closes when the focus opens another
    body.focus();
    prepWrap.dispatchEvent(event('pointerenter'));
    seg.focus();
    assert.equal(seg.getAttribute('aria-expanded'), 'true', 'the focused card opens');
    assert.equal(prepSeg.getAttribute('aria-expanded'), 'false', 'the hovered card closes');
    prepWrap.dispatchEvent(event('pointerleave'));
    body.focus();
    prepWrap.dispatchEvent(event('pointerenter'));
    assert.equal(prepSeg.getAttribute('aria-expanded'), 'true', 'hovered again: open');
    prepWrap.dispatchEvent(event('pointerleave'));
    assert.equal(prepSeg.getAttribute('aria-expanded'), 'false');
  }

  // W4 (T7, T8): the card opens upwards only when it does not fit below
  // and does fit above; a tall card near the top of the page opens downwards
  assert.equal(P.cardOpensUp({ top: 100, bottom: 120 }, 300, 900), false, 'room below');
  assert.equal(P.cardOpensUp({ top: 700, bottom: 720 }, 300, 900), true, 'no room below, room above');
  assert.equal(P.cardOpensUp({ top: 400, bottom: 420 }, 500, 700), false, 'a tall card fits neither way: downwards');
  assert.equal(P.cardOpensUp({ top: 600, bottom: 620 }, 500, 700), true, 'a tall card with room above');
  assert.equal(P.cardOpensUp({ top: 330, bottom: 350 }, 0, 600), true, 'not measured: about 300');
  assert.equal(P.cardOpensUp({ top: 300, bottom: 320 }, 0, 600), false, 'not measured, too close to the top');
  const height = globalThis.innerHeight;
  globalThis.innerHeight = 700;
  wrap.getBoundingClientRect = () => ({ top: 400, bottom: 420, left: 0, right: 0 });
  card.offsetHeight = 500;
  wrap.dispatchEvent(event('pointerenter'));
  assert.ok(!wrap.classList.contains('seg-up'), 'the tall card opens downwards');
  wrap.dispatchEvent(event('pointerleave'));
  card.offsetHeight = 320;
  wrap.dispatchEvent(event('pointerenter'));
  assert.ok(wrap.classList.contains('seg-up'), 'a shorter one fits above');
  wrap.dispatchEvent(event('pointerleave'));
  globalThis.innerHeight = height;
  bar.remove();
}

// ---- T6 #1: a dialog opened from a stage card gives the focus back ----

{
  realFocus = true;
  focusBy = 'keyboard';
  let acted = 0;
  const host = new El('div');
  body.append(host);
  const draw = (st) => host.replaceChildren(P.pipelineBar([stage('prep', 'done'), st], { key: 'lec:G', lecture: '', sys: () => null, res: () => null, onAction: () => (acted++, Promise.resolve()) }));
  const running = () => stage('asr', 'running', { allowed_actions: ['cancel'] });
  const seg = () => fk('lec:G:seg:asr');
  const lastDialog = () => dialogs.filter((d) => d.open).at(-1);
  const dlgButton = (danger) => lastDialog().querySelectorAll('button').find((b) => /btn-danger/.test(b.className) === danger);
  draw(running());
  // the card shows only with the focus in its stage: Tab from the segment into it
  assert.ok(!fk('lec:G:act:asr:cancel').focus() && doc.activeElement !== fk('lec:G:act:asr:cancel'), 'the stand-in hides a closed card');
  seg().focus();
  const cancel = fk('lec:G:act:asr:cancel');
  cancel.focus();
  assert.equal(doc.activeElement, cancel, 'Tab into the open card');
  // 取消 asks first; the dialog takes the focus and the card closes
  cancel.click();
  await settle();
  dlgButton(false).focus();
  assert.equal(seg().getAttribute('aria-expanded'), 'false', 'the dialog has the focus: the card is closed');
  dlgButton(false).click(); // "不取消"
  await settle();
  assert.equal(acted, 0);
  assert.equal(doc.activeElement, cancel, 'not cancelled: back on 取消 in its card');
  assert.equal(seg().getAttribute('aria-expanded'), 'true', 'the card is open again');
  // the bar redrawn while the dialog is open (a job event): the new 取消
  cancel.click();
  await settle();
  dlgButton(false).focus();
  draw(running());
  dlgButton(false).click();
  await settle();
  assert.equal(doc.activeElement, fk('lec:G:act:asr:cancel'), 'redrawn meanwhile: the new 取消');
  // confirmed: on 取消 while the request runs, then the stage is cancelled
  // and its card has no 取消: the segment (data-fnext)
  fk('lec:G:act:asr:cancel').click();
  await settle();
  dlgButton(true).focus();
  dlgButton(true).click();
  await settle();
  assert.equal(acted, 1, 'cancelled');
  assert.equal(doc.activeElement, fk('lec:G:act:asr:cancel'), 'confirmed: back on 取消');
  D.preserveFocus(() => draw(stage('asr', 'cancelled', { allowed_actions: ['retry'] })));
  assert.equal(doc.activeElement, seg(), 'the stage cancelled: its segment');
  // a mouse click's focus is not moved to the segment (R19: the card would
  // stay open under no pointer)
  draw(running());
  seg().focus();
  fk('lec:G:act:asr:cancel').focus();
  focusBy = 'mouse';
  fk('lec:G:act:asr:cancel').click();
  await settle();
  dlgButton(false).focus();
  dlgButton(false).click();
  await settle();
  assert.equal(doc.activeElement, body, 'mouse: the focus on the page, as before');
  assert.equal(seg().getAttribute('aria-expanded'), 'false', 'mouse: the card stays closed');
  focusBy = 'keyboard';
  host.remove();

  // any dialog: its opener redrawn while it was open takes the focus back;
  // gone, its data-fnext fallbacks; disabled (its request runs), the focus
  // waits for it on the page
  const box = new El('div');
  body.append(box);
  const drawBox = (keys, off) => {
    box.replaceChildren();
    for (const k of keys) {
      const b = new El('button');
      b.dataset.fkey = k;
      if (k === 'o') b.dataset.fnext = 'n';
      box.append(b);
    }
    if (off) fk('o').disabled = true;
  };
  const viaDialog = async (redraw) => {
    fk('o').focus();
    let close;
    const done = D.openDialog('t', (c) => ((close = c), new El('button')));
    lastDialog().querySelector('button').focus();
    redraw();
    close(null);
    await done;
  };
  drawBox(['o', 'n']);
  await viaDialog(() => drawBox(['o', 'n']));
  assert.equal(doc.activeElement, fk('o'), 'the opener drawn anew');
  assert.ok(lastFocusOpts && lastFocusOpts.preventScroll, 'drawn anew: focused without scrolling (R29)');
  await viaDialog(() => drawBox(['n']));
  assert.equal(doc.activeElement, fk('n'), 'the opener gone: its fallback');
  drawBox(['o', 'n']);
  await viaDialog(() => drawBox(['o', 'n'], true));
  assert.equal(doc.activeElement, body, 'the opener disabled: waiting on the page');
  D.preserveFocus(() => drawBox(['o', 'n']));
  assert.equal(doc.activeElement, fk('o'), 'enabled again: back on it');
  // R29 (suggestion 3): disabled in place while the dialog was open (not
  // drawn anew): it waits the same way
  await viaDialog(() => (fk('o').disabled = true));
  assert.equal(doc.activeElement, body, 'disabled in place meanwhile: waiting on the page');
  D.preserveFocus(() => (fk('o').disabled = false));
  assert.equal(doc.activeElement, fk('o'), 'enabled again in place: back on it');
  box.remove();
  realFocus = false;
  focusBy = null;
  body.focus();
}

// ---- R19: Tab hears why a stage failed or waits ----

{
  const failed = stage('translate', 'failed', { error: 'deepseek: 402 insufficient balance', allowed_actions: ['retry'] });
  const waiting = stage('asr', 'queued', { waiting_for: 'gpu_slot' });
  const queued = stage('summary', 'queued');
  const bar = P.pipelineBar([stage('prep', 'done'), waiting, queued, failed], { key: 'lec:L2', lecture: '', sys: () => null, res: () => null, onAction: () => Promise.resolve() });
  body.append(bar);
  const seg = (s) => bar.querySelector(`.seg-wrap[data-stage="${s}"]`).querySelector('.seg');
  const card = (s) => bar.querySelector(`.seg-wrap[data-stage="${s}"]`).querySelector('.seg-card');
  const byId = (c, id) => id && [c, ...c.querySelectorAll('div'), ...c.querySelectorAll('p'), ...c.querySelectorAll('pre'), ...c.querySelectorAll('span')].find((e) => e.getAttribute('id') === id);
  // the value of a waiting line, without its label (T6 #8: "等待" was heard twice)
  const waitValue = (e) => !!e && e.className.includes('card-v') && !!e.parentNode && e.parentNode.className.includes('card-wait');
  // the failed stage: its error label and line, not the card
  const fd = seg('translate').getAttribute('aria-describedby');
  assert.ok(fd, 'a failed segment has a description');
  assert.ok(!fd.split(' ').includes(card('translate').getAttribute('id')), 'not the whole card');
  const [fd1, fd2, ...frest] = fd.split(' ');
  assert.equal(frest.length, 0, 'the label and the line');
  const flabel = byId(card('translate'), fd1);
  assert.ok(flabel && flabel.className.includes('card-k'), 'the label first (R21)');
  assert.equal(flabel.textContent, I.t('common.pipe.k_error'));
  const err = byId(card('translate'), fd2);
  assert.ok(err, 'then the error line of the card');
  assert.ok(err.textContent.includes('insufficient balance'), 'the error line');
  assert.equal(err.querySelectorAll('button').length, 0, 'no button in the description');
  // the waiting stage: its waiting line
  const wd = seg('asr').getAttribute('aria-describedby');
  const wl = byId(card('asr'), wd);
  assert.ok(waitValue(wl), "the waiting line's value");
  assert.ok(U.stageWaitingText(waiting, null, null) && wl.textContent.includes(U.stageWaitingText(waiting, null, null)));
  assert.equal(wl.textContent, U.stageWaitingText(waiting, null, null), 'the reason alone, not its label');
  // no error and no reason: no description; a reason that appears later adds it
  assert.equal(seg('summary').getAttribute('aria-describedby'), null);
  assert.equal(seg('prep').getAttribute('aria-describedby'), null);
  queued.waiting_for = 'gpu_slot';
  bar.querySelector('.seg-wrap[data-stage="summary"]').refresh();
  const sd = seg('summary').getAttribute('aria-describedby');
  assert.ok(sd, 'a reason that appears describes the segment');
  assert.ok(byId(card('summary'), sd), 'with the waiting line');
  assert.ok(waitValue(byId(card('summary'), sd)), "the waiting line's value");
  queued.waiting_for = null;
  bar.querySelector('.seg-wrap[data-stage="summary"]').refresh();
  assert.equal(seg('summary').getAttribute('aria-describedby'), null, 'and goes with it');
  bar.remove();
  // R21: a queued stage with the last run's error: the waiting reason first,
  // then the error with its label ("上次错误"), not heard as a failure now
  const again = stage('asr', 'queued', { waiting_for: 'gpu_slot', error: 'CUDA out of memory', attempt: 1 });
  const bar2 = P.pipelineBar([stage('prep', 'done'), again], { key: 'lec:L3', lecture: '', sys: () => null, res: () => null, onAction: () => Promise.resolve() });
  body.append(bar2);
  const s2 = bar2.querySelector('.seg-wrap[data-stage="asr"]');
  const ids = s2.querySelector('.seg').getAttribute('aria-describedby').split(' ');
  const c2 = s2.querySelector('.seg-card');
  const parts = ids.map((id) => byId(c2, id));
  assert.equal(parts.length, 3, 'waiting line, label, error');
  assert.ok(parts.every(Boolean), 'each a line of the card');
  assert.ok(waitValue(parts[0]), 'the waiting reason first');
  assert.ok(parts[1].className.includes('card-k'), 'then the label');
  assert.equal(parts[1].textContent, I.t('common.pipe.k_last_error'));
  assert.ok(parts[2].textContent.includes('CUDA out of memory'), 'then the error');
  bar2.remove();
}

// ---- L14 / R19: the dashboard's inline actions keep the focus ----

{
  const Dash = await import(new URL('view_dashboard.js', base).href);
  const fail = (id) => ({ lecture_id: id, stage: 'translate', course_id: 'C1', course_code: 'CSC1', course_name: 'Course', lecture_title: `Lecture ${id}`, kind: 'lecture', week: 0, allowed_actions: ['retry'], error: 'x', finished_at: null });
  const course = { id: 'C1', code: 'CSC1', name: 'Course', lecture_count: 2, term_count: 0 };
  let failures = [fail('A'), fail('B')];
  let ip = [];
  let drafts = {};
  let recent = [];
  const dash = () => ({ courses: [course], in_progress: ip, draft_total: 0, drafts: [], review_total: 0, reviews: [], stale_total: 0, stale: [], failures, recent_done: recent, usage_today: [], device_prompts: [], ...drafts });
  answer = (m, u) => (u.startsWith('/api/dashboard') ? { status: 200, body: dash() } : u.includes('/stages/') ? { status: 200, body: lecture('A') } : { status: 200, body: {} });
  const page = new El('main');
  body.append(page);
  const dctx = { system: () => null, resources: () => null, courses: () => [course], newCourse() {}, subscribe: () => () => {} };
  const view = Dash.mountDashboard(page, dctx);
  await settle();
  // Enter on the focused control, as a browser does: a button that is
  // enabled is clicked, a link is followed (recorded here)
  const followed = [];
  const press = () => {
    const a = doc.activeElement;
    a.dispatchEvent(event('keydown', { key: 'Enter' }));
    if (a.tagName === 'A') followed.push(a.getAttribute('href'));
    else if (a.tagName === 'BUTTON' && !a.disabled) a.click();
  };
  let added = 0;
  dctx.newCourse = () => added++;
  // a failure's "重试" by keyboard: disabled while it runs; the focus waits on
  // the page, on no control Enter would set off (R21: Enter pressed twice or
  // held down does nothing else), then the row is gone: the next failure's title
  const retry = fk('dash:retry:A:translate');
  assert.ok(retry, 'a failure row has 重试');
  retry.focus();
  failures = [fail('B')];
  posts.length = 0;
  retry.click();
  assert.ok(fk('dash:retry:A:translate').disabled, 'disabled while it runs');
  assert.equal(doc.activeElement, body, 'while it runs the focus waits on the page');
  const open = dialogs.filter((x) => x.open).length;
  press();
  press();
  assert.deepEqual(followed, [], 'Enter twice: no lecture or review page opened');
  assert.equal(added, 0, 'Enter twice: no 新建课程');
  assert.equal(dialogs.filter((x) => x.open).length, open, 'Enter twice: no dialog');
  await confirmDialogs();
  await settle();
  assert.equal(posts.length, 1, 'one request');
  assert.equal(fk('dash:fail:A:translate'), null, 'the failure is gone');
  assert.equal(doc.activeElement, fk('dash:fail:B:translate'), 'the next failure');
  // a request that fails: the focus is back on 重试 once it is enabled again
  const rb = fk('dash:retry:B:translate');
  rb.focus();
  const ok = answer;
  answer = (m, u) => (m === 'POST' ? { status: 500, body: { error: 'boom' } } : ok(m, u));
  rb.click();
  assert.equal(doc.activeElement, body, 'while it runs: waiting');
  press();
  assert.deepEqual(followed, []);
  await confirmDialogs();
  await settle();
  answer = ok;
  assert.ok(fk('dash:fail:B:translate'), 'the failure is still there');
  assert.ok(!fk('dash:retry:B:translate').disabled);
  assert.equal(doc.activeElement, fk('dash:retry:B:translate'), 'failed: back on 重试');
  // with the mouse: no fallback (the focus is not moved)
  focusBy = 'mouse';
  const mretry = fk('dash:retry:B:translate');
  mretry.focus();
  failures = [];
  mretry.click();
  assert.notEqual(doc.activeElement, fk('dash:fail:B:translate'), 'a mouse click: no fallback');
  assert.equal(doc.activeElement, body, 'the focus on the page, as a browser leaves it');
  await confirmDialogs();
  await settle();
  assert.equal(doc.activeElement, body, 'done: still no fallback');
  focusBy = null;
  // "启动全部草稿" asks first (a glossary still being extracted); cancelled,
  // the focus is back on the button, also after a re-render while the
  // dialog was open
  course.glossary_pending = true;
  drafts = { draft_total: 1, drafts: [{ course_id: 'C1', course_code: 'CSC1', course_name: 'Course', count: 1 }] };
  view.refresh();
  await settle();
  const launchAll = fk('dash:drafts');
  assert.ok(launchAll, 'the drafts group has its button');
  launchAll.focus();
  posts.length = 0;
  launchAll.click();
  await settle();
  assert.equal(doc.activeElement, body, 'while it asks: waiting');
  const dlg = dialogs.find((x) => x.open);
  assert.ok(dlg, 'the confirmation');
  const cancelBtn = dlg.querySelectorAll('button').find((b) => !/btn-(danger|primary)/.test(b.className));
  cancelBtn.focus();
  view.refresh();
  await settle();
  assert.equal(doc.activeElement, cancelBtn, 'the dialog keeps the focus');
  cancelBtn.click();
  dlg.open = false;
  await settle();
  assert.deepEqual(posts, [], 'cancelled: nothing launched');
  assert.equal(doc.activeElement, fk('dash:drafts'), 'cancelled: back on 启动全部草稿');
  // launched (T6 #6): the group is gone with its button; the focus goes to
  // the next to-do item, not the page
  course.glossary_pending = false;
  failures = [fail('C')];
  view.refresh();
  await settle();
  fk('dash:drafts').focus();
  posts.length = 0;
  drafts = {};
  fk('dash:drafts').click();
  assert.equal(doc.activeElement, body, 'launching: waiting');
  // round 8 WD-01 (T25 X1): it asks first; confirmed
  await confirmDialogs();
  await settle();
  await settle();
  assert.deepEqual(posts, ['POST /api/courses/C1/launch-drafts']);
  assert.equal(fk('dash:drafts'), null, 'launched: the group is gone');
  assert.equal(doc.activeElement, fk('dash:fail:C:translate'), 'launched: the next to-do item');
  failures = [];
  view.refresh();
  await settle();
  // an in-progress row's card "取消" that takes the lecture off the list:
  // the next row's link
  const running = (id) => ({ ...lecture(id), course_code: 'CSC1', course_name: 'Course', stages: names.map((n) => (n === 'asr' ? stage(n, 'running', { allowed_actions: ['cancel'] }) : stage(n, n === 'prep' ? 'done' : 'queued'))) });
  ip = [running('P'), running('Q')];
  view.refresh();
  await settle();
  // round 8 WD-02: the card is built when it opens (its segment focused)
  fk('dash:P:seg:asr').focus();
  const cancel = fk('dash:P:act:asr:cancel');
  assert.ok(cancel, "the running stage's card has 取消");
  cancel.focus();
  ip = [running('Q')];
  answer = (m, u) => (u.startsWith('/api/dashboard') ? { status: 200, body: dash() } : { status: 200, body: lecture('P') });
  cancel.click();
  await confirmDialogs();
  await settle();
  assert.equal(fk('dash:P:act:asr:cancel'), null);
  assert.equal(doc.activeElement, fk('dash:open:Q'), 'the row is gone: the next row');
  // C108 (R42 suggestion 1): a recently finished row with "N to review" has
  // the has-review class the narrow layout selects on (no :has()); a row
  // without the badge does not
  ip = [];
  const done = (id, pending) => ({ ...lecture(id, { counts: { sentences: 0, patches_pending: pending } }), course_code: 'CSC1', course_name: 'Course', stages: names.map((n) => stage(n, 'done')) });
  recent = [done('R1', 2), done('R2', 0)];
  view.refresh();
  await settle();
  const rrows = page.querySelectorAll('div.rd-row');
  assert.equal(rrows.length, 2, 'two recently finished rows');
  const rowOf = (id) => rrows.find((r) => r.querySelector('a.rd-title') && r.querySelector('a.rd-title').getAttribute('href') === `#/lecture/${id}`);
  const r1 = rowOf('R1');
  const r2 = rowOf('R2');
  assert.ok(r1 && r2, 'each lecture has its row');
  assert.ok(r1.querySelector('span.rd-badge').querySelector('a.chip-review'), 'R1 shows its badge');
  assert.ok(r1.className.split(/\s+/).includes('has-review'), `the row with a badge has has-review: ${r1.className}`);
  assert.ok(!r2.querySelector('.chip-review'), 'R2 has no badge');
  assert.ok(!r2.className.split(/\s+/).includes('has-review'), `the row without a badge has no has-review: ${r2.className}`);
  recent = [];
  view.destroy();
  page.remove();
  body.focus();
}

// ---- L14 / R19: the lecture page's header and notices keep the focus ----

{
  const Lec = await import(new URL('view_lecture.js', base).href);
  const draft = { ...lecture('D', { draft: true, lecture_actions: ['launch'] }), course_code: 'CSC1' };
  const queued = { ...lecture('D', { lecture_actions: ['cancel'], stages: names.map((n, i) => stage(n, i ? 'queued' : 'running')) }), course_code: 'CSC1' };
  let cur = draft;
  // an action answers with the lecture as it is now (one lecture's launch),
  // or with a batch result (cancel)
  const batch = (to) => (m, u) =>
    m === 'POST'
      ? { status: 200, body: u.startsWith('/api/lectures/D/') ? (cur = to) : { results: [{ id: 'D', ok: true, lecture: (cur = to) }] } }
      : u.startsWith('/api/lectures/D') && !u.includes('/sentences') ? { status: 200, body: cur } : { status: 200, body: {} };
  answer = batch(queued);
  const page = new El('main');
  body.append(page);
  const lctx = { system: () => null, resources: () => null, courses: () => [{ id: 'C1', code: 'CSC1', name: 'Course' }], setCurrentCourse() {}, subscribe: () => () => {} };
  let view;
  try {
    view = Lec.mountLecture(page, { id: 'D', tab: 'review' }, lctx);
  } catch (e) {
    throw new Error('mountLecture: ' + e.stack);
  }
  await settle();
  // the draft notice's "启动": gone once launched; the header's "取消" takes its place
  const launch2 = fk('lecd:D:launch2');
  assert.ok(launch2, 'the draft notice has 启动');
  launch2.focus();
  posts.length = 0;
  launch2.click();
  // (the lecture page does not disable its controls: while the request runs
  // the focus stays on 启动, on no control that does something else, R21)
  assert.equal(doc.activeElement, launch2, 'while it runs: still on 启动');
  await confirmDialogs();
  await settle();
  assert.deepEqual(posts, ['POST /api/lectures/D/launch']);
  assert.equal(fk('lecd:D:launch2'), null, 'launched: the notice is gone');
  assert.equal(doc.activeElement, fk('lecd:D:cancel'), "the header's 取消");
  // and the header's "取消": back to a draft-like state with 启动
  const back = { ...lecture('D', { lecture_actions: ['launch'] }), course_code: 'CSC1' };
  answer = batch(back);
  posts.length = 0;
  fk('lecd:D:cancel').click();
  await confirmDialogs();
  await settle();
  assert.deepEqual(posts, ['POST /api/lectures/cancel']);
  assert.equal(fk('lecd:D:cancel'), null, 'cancelled: 取消 is gone');
  assert.ok(fk('lecd:D:launch'));
  assert.equal(doc.activeElement, fk('lecd:D:launch'), "the header's 启动 in its place");
  if (view && view.destroy) view.destroy();
  page.remove();
  body.focus();
}

// ---- T6 #4: a failure notice's hover is in the UI's language ----

{
  const Lec = await import(new URL('view_lecture.js', base).href);
  await I.setLocale('en');
  const raw = 'LLM 请求被拒绝（translate，HTTP 401）{"error":"example"}';
  const bad = { code: 'llm.no_api_key', params: { provider: 'Example' } };
  const failed = { ...lecture('F', { stages: names.map((n) => (n === 'translate' ? stage(n, 'failed', { error: raw, error_info: bad, allowed_actions: ['retry'] }) : stage(n, n === 'export' ? 'pending' : 'done'))) }), course_code: 'CSC1' };
  answer = (m, u) => (u.startsWith('/api/lectures/F') && !u.includes('/sentences') ? { status: 200, body: failed } : { status: 200, body: {} });
  const page = new El('main');
  body.append(page);
  const view = Lec.mountLecture(page, { id: 'F', tab: 'review' }, { system: () => null, resources: () => null, courses: () => [{ id: 'C1', code: 'CSC1', name: 'Course' }], setCurrentCourse() {}, subscribe: () => () => {} });
  await settle();
  const err = page.querySelector('.lec-notice.is-failed').querySelector('.lec-notice-err');
  const text = U.stageErrorText(failed.stages.find((s) => s.stage === 'translate'));
  assert.ok(text && !text.includes('请求'), 'the coded text is in English');
  assert.equal(err.textContent, text);
  assert.equal(err.getAttribute('title'), text, 'the hover: the same text, not the server detail');
  // the server's text stays in the stage card, with its copy button
  assert.ok(page.querySelector('.seg-wrap[data-stage="translate"]').querySelector('.card-errtext').textContent.includes('HTTP 401'));
  if (view && view.destroy) view.destroy();
  page.remove();
  // R29: a task without a stage card (the title) keeps the server's text in
  // the hover, after the translated text
  const rawTitle = 'LLM 请求被拒绝（title，HTTP 401）{"error":"example"}';
  const ta = { stage: 'title', status: 'failed', error: rawTitle, error_info: bad, allowed_actions: ['retry'] };
  const titled = { ...failed, id: 'F2', aux: [ta] };
  answer = (m, u) => (u.startsWith('/api/lectures/F2') && !u.includes('/sentences') ? { status: 200, body: titled } : { status: 200, body: {} });
  const page2 = new El('main');
  body.append(page2);
  const view2 = Lec.mountLecture(page2, { id: 'F2', tab: 'review' }, { system: () => null, resources: () => null, courses: () => [{ id: 'C1', code: 'CSC1', name: 'Course' }], setCurrentCourse() {}, subscribe: () => () => {} });
  await settle();
  const note = page2.querySelectorAll('div.lec-notice').find((n) => n.textContent.includes(I.t('lecture.detail.title_failed')));
  assert.ok(note, 'the title task has its notice');
  const taText = U.stageErrorText(ta);
  const tip = note.querySelector('.lec-notice-err').getAttribute('title');
  assert.ok(tip.startsWith(`${taText}\n\n`), 'the translated text first');
  assert.ok(tip.includes('title，HTTP 401'), "then the server's text");
  assert.equal(page2.querySelector('.lec-notice.is-failed').querySelector('.lec-notice-err').getAttribute('title'), text, "a stage's notice: the translated text alone");
  if (view2 && view2.destroy) view2.destroy();
  page2.remove();
  await I.setLocale('zh-CN');
  body.focus();
}

// ---- T6 #5: "加载更多" gone after the last page leaves the focus in the list ----

{
  globalThis.IntersectionObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
  const Tx = await import(new URL('view_transcript.js', base).href);
  const sent = (i) => ({ idx: i, start_ms: i * 1000, end_ms: i * 1000 + 900, text: `s${i}`, asr_text: `s${i}`, zh: `z${i}`, zh_status: 'done', human_text: null, patch: null, low_conf_words: [] });
  const total = 450;
  // at most a few pages: the view asking for more than that (paging on by
  // itself) fails the test instead of growing it
  let pages = 0;
  answer = (m, u) => {
    const q = /\/api\/lectures\/T\/sentences\?offset=(\d+)&limit=(\d+)/.exec(u);
    if (!q) return { status: 200, body: {} };
    if (++pages > 6) throw new Error('transcript: more pages asked for than the test loads');
    const from = Number(q[1]);
    const to = Math.min(total, from + Number(q[2]));
    return { status: 200, body: { total, sentences: Array.from({ length: Math.max(0, to - from) }, (_, k) => sent(from + k)) } };
  };
  // the bottom is not on screen: no page loads by itself
  const height = globalThis.innerHeight;
  globalThis.innerHeight = -1000;
  const lec = { ...lecture('T', { reviewable: true, counts: { sentences: total, patches_pending: 0 } }), stages: names.map((n) => stage(n, 'done')) };
  // a transcript with its first page in (the page count starts again)
  const mount = async () => {
    pages = 0;
    const root = new El('div');
    body.append(root);
    const tx = Tx.mountTranscript(root, lec, { system: () => null, resources: () => null });
    await settle();
    const more = root.querySelector('.tx-footer').querySelector('button');
    assert.ok(fk('ts:199') && !fk('ts:200') && !more.hidden, 'one page loaded, more to come');
    return { root, tx, more };
  };
  const unmount = ({ root, tx }) => {
    tx.destroy();
    root.remove();
    body.focus();
  };
  let view = await mount();
  let more = view.more;
  more.focus();
  more.click();
  more.blur(); // disabled while the page loads: a browser may leave the focus on the page
  await settle();
  assert.ok(fk('ts:399') && !more.hidden);
  assert.equal(doc.activeElement, more, 'a page loaded, more to come: back on the button');
  more.click();
  await settle();
  assert.ok(fk('ts:449'), 'all loaded');
  assert.ok(more.hidden, 'the button is gone');
  assert.equal(doc.activeElement, fk('ts:400'), 'the first sentence just loaded');
  assert.ok(lastFocusOpts && lastFocusOpts.preventScroll, 'without scrolling');
  unmount(view);
  // R29: a press with the pointer while the page loads (the user went on):
  // the focus is left where it is
  view = await mount();
  more = view.more;
  more.focus();
  more.click();
  more.blur();
  view.root.dispatchEvent(event('pointerdown'));
  await settle();
  assert.equal(doc.activeElement, body, 'a press meanwhile: the focus is not moved');
  unmount(view);
  // R29: by the mouse, the last page in: the focus is not put on a
  // sentence (Space would play it), it stays on the page as before
  view = await mount();
  more = view.more;
  focusBy = 'mouse';
  more.focus();
  more.click();
  await settle();
  assert.equal(doc.activeElement, more, 'by the mouse, more to come: still on the button');
  more.click();
  await settle();
  assert.ok(more.hidden, 'the button is gone');
  assert.ok(!doc.activeElement.className.includes('ts'), 'by the mouse: no sentence takes the focus');
  focusBy = null;
  unmount(view);
  // W4 (R29 detail b): a filter hides every sentence of the last page: the
  // focus goes to the last sentence shown, which may be far off screen, so
  // the browser may scroll to it (no preventScroll)
  view = await mount();
  more = view.more;
  more.focus();
  more.click();
  await settle();
  assert.ok(fk('ts:399') && doc.activeElement === more, 'two pages in, back on the button');
  const search = view.root.querySelector('.tx-search-input');
  search.value = 's19'; // s19, s190–s199: none of the last page (s400–s449)
  search.dispatchEvent(event('input'));
  await settle(250); // the search waits 150 ms, then loads the rest
  await settle();
  assert.ok(fk('ts:449') && more.hidden, 'all loaded');
  assert.ok(doc.activeElement === fk('ts:199'), 'the last sentence the filter shows');
  assert.equal(lastFocusOpts, undefined, 'scrolled into view (it may be off screen)');
  unmount(view);
  // W4 (R29 detail a): the view destroyed while a page loads leaves no
  // pointerdown listener on the document
  const before = (doc.listeners.pointerdown || []).length;
  view = await mount();
  more = view.more;
  more.focus();
  more.click();
  assert.equal((doc.listeners.pointerdown || []).length, before + 1, 'a keyboard load waits for a pointer press');
  view.tx.destroy();
  assert.equal((doc.listeners.pointerdown || []).length, before, 'destroy() takes the listener off');
  await settle();
  view.root.remove();
  body.focus();
  globalThis.innerHeight = height;
}

// ---- T6 #10: the model dialogs' choices have a group name ----

{
  const A = await import(new URL('actions.js', base).href);
  const done = A.retryModelDialog({ lec: lecture('R'), stage: 'translate', st: stage('translate', 'failed'), diff: null, settings: { providers: [], roles: [] } });
  const dlg = dialogs.filter((d) => d.open).at(-1);
  const group = dlg.querySelector('div[role="radiogroup"]');
  assert.ok(group.getAttribute('aria-labelledby'), 'the retry dialog: the choices have a name');
  const q = dlg.querySelectorAll('p').find((p) => p.getAttribute('id') === group.getAttribute('aria-labelledby'));
  assert.ok(q, 'the retry dialog: the choices are named by its question');
  assert.equal(q.textContent, I.t('common.model_choice.message_retry', { stage: U.stageName('translate') }));
  dlg.querySelectorAll('button').find((b) => b.className === 'btn').click();
  assert.equal(await done, null);
  // W4 (needs-human 08:37, a): from "换模型重试…" a snapshot whose provider
  // has no key is not offered; the dialog starts on the current settings
  // and says why. With a key it starts on the snapshot (SPEC-v1.1 A4.3).
  const snapModel = { provider_id: 'ds', provider_name: 'DeepSeek', model: 'deepseek-chat' };
  const role = { role: 'translate', configured: true, provider_id: 'an', model: 'claude-x', temperature: 0.3, thinking: 'off' };
  const radios = (d) => Object.fromEntries(d.querySelectorAll('input').filter((i) => i.getAttribute('type') === 'radio').map((i) => [i.value, i]));
  for (const [keyHint, wantUsable] of [['', false], ['sk-…abcd', true]]) {
    const providers = [
      { id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com', key_hint: keyHint },
      { id: 'an', name: 'Anthropic', base_url: 'https://api.anthropic.com', key_hint: 'sk-…wxyz' },
    ];
    const run = A.retryModelDialog({ lec: lecture('R'), stage: 'translate', st: stage('translate', 'failed', { model: snapModel }), diff: null, settings: { providers, roles: [role] } });
    const d = dialogs.filter((x) => x.open).at(-1);
    const r = radios(d);
    assert.ok(r.snapshot && r.current && r.manual, 'three choices');
    assert.equal(r.snapshot.getAttribute('disabled') !== null, !wantUsable, `key "${keyHint}": the snapshot choice`);
    assert.equal(r.snapshot.getAttribute('checked') !== null, wantUsable, `key "${keyHint}": starts on the snapshot`);
    assert.equal(r.current.getAttribute('checked') !== null, !wantUsable, `key "${keyHint}": starts on the current settings`);
    const why = I.t('errors.snapshot.unusable.no_api_key', { provider: 'DeepSeek' });
    assert.ok(why && !why.startsWith('errors.'));
    assert.equal(d.textContent.includes(why), !wantUsable, `key "${keyHint}": the reason is shown`);
    d.querySelectorAll('button').find((b) => b.className === 'btn').click();
    assert.equal(await run, null);
  }
  // G4 (T16, T18): the current settings cannot be used either (the role's
  // provider was deleted): "使用当前设置" is not offered, says why, and the
  // dialog starts on "临时指定"; with the snapshot usable it starts there
  for (const snapKey of ['', 'sk-…abcd']) {
    const providers = [
      { id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com', key_hint: snapKey },
      { id: 'an', name: 'Anthropic', base_url: 'https://api.anthropic.com', key_hint: 'sk-…wxyz' },
    ];
    const gone = { role: 'translate', configured: false, provider_id: 'xx', model: 'm', error: 'gone', error_info: { code: 'settings.role_provider_deleted', params: { role: 'translate' } } };
    const run = A.retryModelDialog({ lec: lecture('R'), stage: 'translate', st: stage('translate', 'failed', { model: snapModel }), diff: null, settings: { providers, roles: [gone] } });
    const d = dialogs.filter((x) => x.open).at(-1);
    const r = radios(d);
    const want = snapKey ? 'snapshot' : 'manual';
    assert.ok(r.current.getAttribute('disabled') !== null, `snapshot key "${snapKey}": the current settings are not offered`);
    for (const v of ['snapshot', 'current', 'manual']) assert.equal(r[v].getAttribute('checked') !== null, v === want, `snapshot key "${snapKey}": ${v} checked`);
    const why = I.t('common.model_choice.unusable', { reason: U.errorInfoText(gone.error_info) });
    assert.ok(!U.errorInfoText(gone.error_info).startsWith('settings.'), 'the reason has a text');
    assert.ok(d.textContent.includes(why), `snapshot key "${snapKey}": the reason is shown`);
    assert.ok(r.current.parentNode.className.includes('is-disabled'), 'drawn as not selectable');
    d.querySelectorAll('button').find((b) => b.className === 'btn').click();
    assert.equal(await run, null);
  }
  // R62 N2: the retry of summary moves the later translate stage to the
  // settings too: its role's provider deleted, "使用当前设置" is not offered
  {
    const providers = [{ id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com', key_hint: 'sk-…abcd' }];
    const ok = (r) => ({ role: r, configured: true, provider_id: 'ds', model: 'deepseek-chat', temperature: 0.3, thinking: 'off' });
    const gone = { role: 'translate', configured: false, provider_id: 'xx', model: 'm', error_info: { code: 'settings.role_provider_deleted', params: { role: 'translate' } } };
    const sm = { provider_id: 'ds', provider_name: 'DeepSeek', model: 'deepseek-chat' };
    const lecS = lecture('S', { stages: names.map((n) => stage(n, n === 'summary' ? 'failed' : n === 'prep' || n === 'asr' ? 'done' : 'pending', ['summary', 'repair', 'translate'].includes(n) ? { model: sm } : {})) });
    for (const [roles, want] of [[[ok('summary'), ok('repair'), ok('translate')], true], [[ok('summary'), ok('repair'), gone], false]]) {
      const run = A.retryModelDialog({ lec: lecS, stage: 'summary', st: lecS.stages.find((s) => s.stage === 'summary'), diff: null, settings: { providers, roles } });
      const d = dialogs.filter((x) => x.open).at(-1);
      const r = radios(d);
      assert.equal(r.current.getAttribute('disabled') === null, want, `translate role ${want ? 'set' : 'deleted'}: the current settings offered`);
      if (!want) assert.ok(d.textContent.includes(U.errorInfoText(gone.error_info)), "the later stage's reason is shown");
      d.querySelectorAll('button').find((b) => b.className === 'btn').click();
      assert.equal(await run, null);
    }
  }
  // C4 (R6 A8): "临时指定" + thinking off + a model that cannot switch it
  // off says it runs at the lowest level, under the select and describing it
  {
    const providers = [
      { id: 'an', name: 'Anthropic', type: 'anthropic', supports_thinking: true, base_url: 'https://api.anthropic.com', key_hint: 'sk-…wxyz', models: ['claude-opus-5-5', 'claude-sonnet-4-5'] },
      { id: 'oa', name: 'Router', type: 'openai', supports_thinking: true, base_url: 'https://openrouter.ai/api/v1', key_hint: 'sk-…abcd' },
    ];
    const role = { role: 'translate', configured: true, provider_id: 'an', model: 'claude-sonnet-4-5', temperature: 0.3, thinking: 'off' };
    const run = A.retryModelDialog({ lec: lecture('R'), stage: 'translate', st: stage('translate', 'failed'), diff: null, settings: { providers, roles: [role] } });
    const d = dialogs.filter((x) => x.open).at(-1);
    const selects = d.querySelectorAll('select');
    const prov = selects.find((s) => s.getAttribute('aria-label') === I.t('common.model_choice.f_provider'));
    const think = selects.find((s) => s.getAttribute('aria-label') === I.t('common.model_choice.f_thinking'));
    const model = d.querySelectorAll('input').find((i) => i.getAttribute('aria-label') === I.t('common.model_choice.f_model'));
    assert.ok(prov && think && model, 'the manual fields');
    const note = () => {
      const id = think.getAttribute('aria-describedby');
      const el = id ? d.querySelectorAll('span').find((s) => s.getAttribute('id') === id) : null;
      return el && !el.hidden ? el.textContent : '';
    };
    const typeModel = (m) => {
      model.value = m;
      model.dispatchEvent(event('input'));
    };
    prov.value = 'an';
    prov.dispatchEvent(event('change'));
    think.value = 'off';
    typeModel('claude-sonnet-4-5');
    assert.equal(note(), '', 'a model that can switch it off: no note');
    typeModel('claude-opus-5-5');
    assert.equal(note(), I.t('settings.roles.thinking_floor_note', { model: 'claude-opus-5-5' }), 'claude-opus-5-5, off: the note');
    assert.ok(!I.t('settings.roles.thinking_floor_note', { model: 'x' }).startsWith('settings.'));
    think.value = 'low';
    think.dispatchEvent(event('change'));
    assert.equal(note(), '', 'thinking low: no note');
    think.value = 'off';
    think.dispatchEvent(event('change'));
    assert.ok(note(), 'off again: the note');
    // an OpenAI-compatible provider sends off as it is
    prov.value = 'oa';
    prov.dispatchEvent(event('change'));
    typeModel('claude-opus-5-5');
    assert.equal(note(), '', 'an OpenAI-compatible provider: no note');
    // T18 M4: the output limit is read as on the settings page (1..1000000,
    // digits only); a refused value marks the field and takes the focus
    const radio = d.querySelectorAll('input').find((i) => i.getAttribute('type') === 'radio' && i.value === 'manual');
    radio.checked = true;
    radio.dispatchEvent(event('change'));
    const maxIn = d.querySelectorAll('input').find((i) => i.getAttribute('aria-label') === I.t('common.model_choice.f_max_tokens'));
    const form = d.querySelector('form');
    const errP = d.querySelectorAll('p').find((p) => p.className === 'dlg-field-error');
    let sent = 0;
    const before = answer;
    answer = (m, u) => {
      sent++;
      return { status: 500, body: { error: { code: 'api.internal' } } };
    };
    for (const bad of ['1e3', '1000001', '1.0']) {
      maxIn.value = bad;
      maxIn.dispatchEvent(event('input'));
      assert.equal(maxIn.getAttribute('aria-invalid'), null, `${bad}: not marked while typing`);
      body.focus();
      form.dispatchEvent(event('submit'));
      await settle();
      assert.equal(errP.textContent, I.t('common.model_choice.max_tokens_bad'), `${bad}: refused with the dialog's message`);
      assert.equal(maxIn.getAttribute('aria-invalid'), 'true', `${bad}: the field is marked`);
      assert.ok(doc.activeElement === maxIn, `${bad}: the field takes the focus`);
    }
    assert.equal(sent, 0, 'nothing sent');
    // R58c: another choice clears the error and the mark with it (no field
    // marked without a message when "临时指定" is chosen again)
    const other = d.querySelectorAll('input').find((i) => i.getAttribute('type') === 'radio' && i.value === 'current');
    radio.checked = false;
    other.checked = true;
    other.dispatchEvent(event('change'));
    assert.equal(errP.textContent, '', 'another choice: no error');
    assert.equal(maxIn.getAttribute('aria-invalid'), null, 'another choice: the output limit is no longer marked');
    model.setAttribute('aria-invalid', 'true');
    other.checked = false;
    radio.checked = true;
    radio.dispatchEvent(event('change'));
    assert.equal(model.getAttribute('aria-invalid'), null, 'a change of choice: the model is no longer marked');
    maxIn.value = '1000000';
    maxIn.dispatchEvent(event('input'));
    assert.equal(maxIn.getAttribute('aria-invalid'), null, 'editing clears the mark');
    answer = before;
    d.querySelectorAll('button').find((b) => b.className === 'btn').click();
    assert.equal(await run, null);
  }
  const asked = D.choiceDialog({ title: 'T', message: 'Q?', choices: [{ value: 'a', label: 'A' }] });
  const d2 = dialogs.filter((d) => d.open).at(-1);
  const g2 = d2.querySelector('div[role="radiogroup"]');
  assert.ok(g2.getAttribute('aria-labelledby'), 'choiceDialog: the choices have a name');
  assert.equal(d2.querySelectorAll('p').find((p) => p.getAttribute('id') === g2.getAttribute('aria-labelledby')).textContent, 'Q?', 'choiceDialog: named by its message');
  d2.querySelectorAll('button').find((b) => b.className === 'btn').click();
  await asked;
  const plain = D.choiceDialog({ title: 'T', choices: [{ value: 'a', label: 'A' }] });
  const d3 = dialogs.filter((d) => d.open).at(-1);
  assert.equal(d3.querySelector('div[role="radiogroup"]').getAttribute('aria-label'), 'T', 'no message: named by the title');
  d3.querySelectorAll('button').find((b) => b.className === 'btn').click();
  await plain;
  body.focus();
}

// ---- L16: an upload's progress bar has a name ----

{
  const file = { name: 'week3.mp4', size: 1024 };
  const named = (item) => {
    const li = W.uploadRow(item, { onRetry() {}, onRemove() {} });
    return li.querySelector('progress');
  };
  const p1 = named({ key: 'u1', kind: 'lecture', file, status: 'uploading', fraction: 0.45 });
  assert.equal(p1.getAttribute('aria-label'), 'week3.mp4', 'named by its file');
  // a lecture whose title is not its file name: the title, as the row shows it first
  const p2 = named({ key: 'u2', kind: 'lecture', title: 'Deadlocks', file, status: 'waiting' });
  assert.equal(p2.getAttribute('aria-label'), 'Deadlocks');
  // the value is the element's own (no aria-valuenow by hand)
  assert.equal(p1.getAttribute('aria-valuenow'), null);
}

// ---- W5 (T11): the settings page, rendered from the example settings ----

{
  const settingsJSON = JSON.parse(await readFile(new URL('../testdata/api/v11_settings.json', base), 'utf8'));
  const before = answer;
  answer = (m, u) => (m === 'GET' && u.startsWith('/api/settings') ? { status: 200, body: settingsJSON } : { status: 404, body: { error: { code: 'api.not_found' } } });
  const S = await import(new URL('view_settings.js', base).href);
  const Lim = await import(new URL('provider_limits.js', base).href);
  // a form field starts with an empty value, as in a browser (the settings
  // page reads fields built without one)
  const createElement = doc.createElement;
  doc.createElement = (tag) => {
    const e = createElement(tag);
    if (/^(input|textarea|select)$/i.test(tag)) e.value = '';
    return e;
  };
  const page = new El('main');
  body.append(page);
  const view = S.mountSettings(page, { resources: () => null, system: () => null, subscribe: () => () => {} }, null);
  await settle(300);
  const change = (el, value) => {
    if (value !== undefined) el.value = value;
    el.dispatchEvent(event('change'));
  };

  // general 3: the provider dialog's type switched to OpenAI and back to
  // Anthropic: the account limits are shown and describe the two fields
  // again (they were synced before the address went back to Anthropic's)
  {
    const anth = settingsJSON.providers.find((p) => p.type === 'anthropic' && p.rate_limits);
    assert.ok(anth, 'the example has an Anthropic provider with observed limits');
    fk(`set:prov:${anth.id}:edit`).click();
    await settle();
    const dlg = dialogs.filter((d) => d.open).at(-1);
    assert.ok(dlg, 'the provider dialog');
    const note = dlg.querySelector('[data-fkey="set:prov:limits_observed"]');
    assert.ok(note && note.querySelector(`[id="${Lim.LIMITS_HINT_ID}"]`), 'the limits block and its text');
    const described = () => dlg.querySelectorAll('input').filter((i) => (i.getAttribute('aria-describedby') || '').split(' ').includes(Lim.LIMITS_HINT_ID)).length;
    const baseInput = dlg.querySelectorAll('input').find((i) => i.getAttribute('type') === 'url');
    const pick = (v) => {
      const r = dlg.querySelector(`[data-fkey="dlg-ptype:${v}"]`);
      r.checked = true;
      change(r);
    };
    assert.equal(note.hidden, false, "shown for Anthropic's own host");
    assert.equal(described(), 2, 'concurrency and per-minute described by it');
    pick('openai');
    assert.equal(baseInput.value, settingsJSON.default_base_urls.openai);
    assert.equal(note.hidden, true, 'hidden for OpenAI');
    assert.equal(described(), 0, 'no description for OpenAI');
    pick('anthropic');
    assert.equal(baseInput.value, settingsJSON.default_base_urls.anthropic, "the address back at Anthropic's");
    assert.equal(note.hidden, false, 'shown again after switching back');
    assert.equal(described(), 2, 'the description back after switching back');
    dlg.dispatchEvent(event('cancel'));
    await settle();
  }

  // general 4: "cannot switch thinking off" follows the model and the
  // thinking level picked in a role's row (no re-render of the row), in
  // the thinking cell and under the row; the thinking select is described
  // by the full note (detail 6)
  {
    const role = 'syllabus_format'; // Anthropic provider, claude-sonnet-4-5, thinking off
    const rowOf = () => page.querySelector(`div.rl-row[data-role="${role}"]`);
    const floorText = (m) => I.t('settings.roles.thinking_floor_note', { model: m });
    // the note's words after the model name (any model)
    const floorTail = I.t('settings.roles.thinking_floor_note', { model: '@@' }).split('@@').pop();
    assert.ok(floorTail.length > 5, floorTail);
    const msgs = () => rowOf().querySelectorAll('p.rl-msg').filter((p) => p.textContent.includes(floorTail));
    const shown = (m) => {
      const short = fk(`set:role:${role}:think_floor`);
      assert.ok(short, `${m}: the short note in the thinking cell`);
      assert.ok(rowOf().querySelector('div.rl-think').contains(short), `${m}: in the thinking cell`);
      assert.equal(short.getAttribute('title'), floorText(m));
      const ms = msgs();
      assert.equal(ms.length, 1, `${m}: one full note under the row`);
      assert.ok(ms[0].textContent.includes(floorText(m)), ms[0].textContent);
      const id = ms[0].getAttribute('id');
      assert.ok(id, `${m}: the full note has an id`);
      assert.equal(fk(`set:role:${role}:thinking`).getAttribute('aria-describedby'), id, `${m}: the thinking select is described by it`);
    };
    const gone = (m) => {
      assert.equal(fk(`set:role:${role}:think_floor`), null, `${m}: no short note`);
      assert.equal(msgs().length, 0, `${m}: no full note`);
      assert.equal(fk(`set:role:${role}:thinking`).getAttribute('aria-describedby'), null, `${m}: the thinking select not described`);
    };
    gone('claude-sonnet-4-5');
    const sel = fk(`set:role:${role}:model`);
    assert.equal(sel.tagName, 'SELECT', 'the model from the probed list');
    change(sel, 'claude-opus-5-5');
    shown('claude-opus-5-5');
    change(sel, 'claude-opus-4-1');
    gone('claude-opus-4-1');
    change(sel, 'claude-opus-5-5');
    shown('claude-opus-5-5');
    // a thinking level other than off: no note; off again: the note
    const think = fk(`set:role:${role}:thinking`);
    change(think, 'low');
    await settle();
    gone('low');
    change(fk(`set:role:${role}:thinking`), 'off');
    await settle();
    shown('claude-opus-5-5');
    // typed by hand
    change(fk(`set:role:${role}:model`), '__manual__');
    const typed = fk(`set:role:${role}:model`);
    assert.equal(typed.tagName, 'INPUT', 'typed by hand');
    for (const [m, on] of [['claude-fable-1', true], ['claude-opus-5', false], ['claude-mythos-2', true], ['', false]]) {
      typed.value = m;
      typed.dispatchEvent(event('input'));
      if (on) shown(m);
      else gone(m || '(empty)');
    }
    // the saved rows as rendered: a role saved with claude-opus-5-5 and
    // thinking off has the note at once
    assert.ok(fk('set:role:glossary_janitor:think_floor'), 'glossary_janitor as saved');
    // undo the drafts
    typed.value = 'claude-sonnet-4-5';
    typed.dispatchEvent(event('input'));
  }

  // C3 (R42 suggestion 4): "cannot switch thinking off" only on an
  // Anthropic provider; an OpenAI-compatible one with the same model and
  // thinking off has no note
  {
    const role = 'repair'; // the DeepSeek provider (OpenAI-compatible), thinking off
    const prov = settingsJSON.providers.find((p) => p.id === settingsJSON.roles.find((r) => r.role === role).provider_id);
    assert.equal(prov.type, 'openai', 'an OpenAI-compatible provider');
    let model = fk(`set:role:${role}:model`);
    if (model.tagName === 'SELECT') {
      change(model, '__manual__');
      model = fk(`set:role:${role}:model`);
    }
    assert.equal(model.tagName, 'INPUT', 'typed by hand');
    model.value = 'claude-opus-5-5';
    model.dispatchEvent(event('input'));
    assert.equal(fk(`set:role:${role}:think_floor`), null, 'no note for an OpenAI-compatible provider');
    assert.equal(fk(`set:role:${role}:thinking`).getAttribute('aria-describedby'), null);
    model.value = 'deepseek-chat';
    model.dispatchEvent(event('input'));
  }

  // C3 (R28 A6): the batch chip shows the number ("1.0" is 1), as saved
  {
    const vchip = fk('set:role:translate:batch_value_chip');
    const batch = fk('set:role:translate:batch');
    assert.ok(vchip && batch, 'the translate row has its batch field and chip');
    for (const [typed, shownAs] of [['1.0', '1'], ['04', '4'], ['12', '12']]) {
      batch.value = typed;
      batch.dispatchEvent(event('input'));
      assert.equal(vchip.hidden, false, `${typed}: a chip`);
      assert.equal(vchip.textContent, I.t('settings.roles.batch_value_chip', { value: shownAs }), `${typed}: shown as ${shownAs}`);
    }
    batch.value = '4';
    batch.dispatchEvent(event('input'));
  }

  // T18: a refused batch size: its error describes the field, first
  {
    let batch = fk('set:role:translate:batch');
    const own = 'rl-translate-batch_default-hint rl-translate-batch-hint';
    assert.equal(batch.getAttribute('aria-describedby'), own, 'described by its default and explanation');
    batch.value = '0';
    batch.dispatchEvent(event('input'));
    fk('set:roles:save').click();
    await settle();
    batch = fk('set:role:translate:batch');
    assert.equal(batch.getAttribute('aria-invalid'), 'true', 'the batch size is refused');
    const alert = page.querySelector('div.rl-row[data-role="translate"]').querySelectorAll('p.rl-msg').find((p) => p.getAttribute('role') === 'alert');
    assert.ok(alert && alert.getAttribute('id'), 'its error line has an id');
    assert.equal(batch.getAttribute('aria-describedby'), `${alert.getAttribute('id')} ${own}`, 'the error first');
    batch.value = '4';
    batch.dispatchEvent(event('input'));
    assert.equal(batch.getAttribute('aria-invalid'), null, 'valid again');
    assert.equal(batch.getAttribute('aria-describedby'), own, 'the error is no longer referred to');
  }

  // C3 (R28 A5): the output limit's error goes as soon as the value is
  // valid, in place (no re-render: the field keeps the focus), as the batch's
  {
    const role = 'repair';
    const rowOf = () => page.querySelector(`div.rl-row[data-role="${role}"]`);
    const PR = await import(new URL('provider_rules.js', base).href);
    const errMsg = I.t('errors.settings.max_tokens_invalid', { min: 1, max: PR.MAX_TOKENS_LIMIT });
    const alerts = () => rowOf().querySelectorAll('p.rl-msg').filter((p) => p.getAttribute('role') === 'alert');
    let max = fk(`set:role:${role}:max_tokens`);
    max.value = '0';
    max.dispatchEvent(event('input'));
    fk('set:roles:save').click();
    await settle();
    max = fk(`set:role:${role}:max_tokens`);
    assert.equal(max.getAttribute('aria-invalid'), 'true', 'the invalid output limit is marked');
    assert.equal(alerts().length, 1, 'with its error under the row');
    assert.ok(alerts()[0].textContent.includes(errMsg), alerts()[0].textContent);
    // T18: the error describes the field, before its explanation
    const described = () => (max.getAttribute('aria-describedby') || '').split(' ').filter(Boolean);
    const errId = alerts()[0].getAttribute('id');
    assert.ok(errId, 'the error line has an id');
    assert.deepEqual(described(), [errId, `rl-${role}-max_tokens-hint`], 'the field is described by its error, then its explanation');
    const row = rowOf();
    max.focus();
    // still invalid: the error stays
    max.value = '-3';
    max.dispatchEvent(event('input'));
    assert.ok(rowOf() === row, 'no re-render');
    assert.equal(max.getAttribute('aria-invalid'), 'true', 'still invalid: still marked');
    assert.equal(alerts().length, 1, 'still invalid: the error stays');
    // valid: gone at once, the focus where it was
    max.value = '4000';
    max.dispatchEvent(event('input'));
    assert.ok(rowOf() === row, 'no re-render');
    assert.equal(max.getAttribute('aria-invalid'), null, 'valid: no longer marked');
    assert.equal(alerts().length, 0, 'valid: the error is gone');
    assert.deepEqual(described(), [`rl-${role}-max_tokens-hint`], 'valid: described by its explanation only');
    assert.ok(!row.classList.contains('is-error'), 'the row is no longer marked');
    assert.ok(doc.activeElement === max, 'the field keeps the focus');
    // T18 M4: the retry dialog's rule: "1e3" and above 1000000 are refused
    for (const bad of ['1e3', '1000001']) {
      max.value = bad;
      max.dispatchEvent(event('input'));
      fk('set:roles:save').click();
      await settle();
      max = fk(`set:role:${role}:max_tokens`);
      assert.equal(max.getAttribute('aria-invalid'), 'true', `${bad}: refused and marked`);
      assert.ok(alerts()[0] && alerts()[0].textContent.includes(errMsg), `${bad}: with the error`);
    }
    max.value = '';
    max.dispatchEvent(event('input'));
  }

  // C3 (R34 suggestion 4): an IPv4-mapped loopback address is a local
  // server, as settings.IsLocal (and the retry dialog) read it
  {
    const ds = settingsJSON.providers.find((p) => p.id === 'deepseek');
    fk(`set:prov:${ds.id}:edit`).click();
    await settle();
    const dlg = dialogs.filter((d) => d.open).at(-1);
    assert.ok(dlg, 'the provider dialog');
    const baseInput = dlg.querySelectorAll('input').find((i) => i.getAttribute('type') === 'url');
    const hint = dlg.querySelector('[id="pv-key-hint"]');
    for (const [u, local] of [
      ['http://[::ffff:127.0.0.1]:8080/v1', true],
      ['http://127.0.0.1:8080/v1', true],
      ['http://[::1]:8080/v1', true],
      ['http://localhost:11434/v1', true],
      ['https://api.deepseek.com', false],
      ['http://[::ffff:10.0.0.1]:8080/v1', false],
    ]) {
      baseInput.value = u;
      baseInput.dispatchEvent(event('input'));
      assert.equal(hint.textContent, I.t(local ? 'settings.provider.key_hint_local' : 'settings.provider.key_hint'), u);
    }
    // round 9 WV-06: the address changed, so Esc asks "放弃修改？" first
    dlg.dispatchEvent(event('cancel'));
    await settle();
    const ask = dialogs.filter((d) => d.open).at(-1);
    assert.ok(ask && ask !== dlg && ask.getAttribute('aria-label') === I.t('settings.provider.discard_title'), 'the changed dialog asks first');
    ask.querySelectorAll('button').find((b) => b.textContent === I.t('wizard.close.discard')).click();
    await settle();
    assert.ok(!dlg.open, 'discarded: closed');
  }

  view.destroy();
  page.remove();
  doc.createElement = createElement;
  answer = before;
}

// ---- C2 (R19 §5): a glossary marker's card holds a button, so it is no
// tooltip: the marker controls it, the card is a named group, Esc closes it ----

{
  const G = await import(new URL('glossary.js', base).href);
  let unlocked = 0;
  const wrap = G.markCard({
    ic: 'lock',
    cls: 'mk-lock',
    label: 'Locked: you changed this term',
    title: 'Locked',
    fkey: 'term:t1:lock',
    body: [D.h('div', { class: 'mk-title' }, 'Locked'), D.h('p', { class: 'mk-text' }, 'You changed this term.')],
    action: { label: 'Unlock', icon: 'lock-open', fkey: 'term:t1:unlock', onclick: () => unlocked++ },
  });
  body.append(wrap);
  const mk = wrap.querySelector('.mk');
  const card = wrap.querySelector('.mk-card');
  const unlock = card.querySelector('button');
  assert.ok(unlock, 'the card has its button');
  assert.equal(wrap.querySelectorAll('[role="tooltip"]').length, 0, 'no tooltip');
  assert.equal(card.getAttribute('role'), 'group');
  // named by its title line, not by an aria-label of the same words (heard twice, R58)
  const head = card.querySelector('.mk-title');
  assert.ok(head.getAttribute('id') && card.getAttribute('aria-labelledby') === head.getAttribute('id'), 'the card is named by its title line');
  assert.equal(card.getAttribute('aria-label'), null, 'no aria-label beside it');
  // a body without a title line: named by title
  const bare = G.markCard({ ic: 'lock', cls: 'mk-lock', label: 'L', title: 'Bare', fkey: 'term:t2:lock', body: [D.h('p', { class: 'mk-text' }, 'x')] });
  assert.equal(bare.querySelector('.mk-card').getAttribute('aria-label'), 'Bare');
  assert.equal(bare.querySelector('.mk-card').getAttribute('aria-labelledby'), null);
  // the marker's label says the card's text: no description (heard twice)
  assert.equal(mk.getAttribute('aria-describedby'), null, 'the marker is not described by the card');
  assert.equal(mk.getAttribute('aria-label'), 'Locked: you changed this term');
  assert.ok(card.getAttribute('id') && mk.getAttribute('aria-controls') === card.getAttribute('id'), 'the marker controls the card');
  assert.equal(mk.getAttribute('aria-expanded'), 'false', 'closed at first');
  wrap.dispatchEvent(event('pointerenter'));
  assert.equal(mk.getAttribute('aria-expanded'), 'true', 'the pointer opens it');
  wrap.dispatchEvent(event('pointerleave'));
  assert.equal(mk.getAttribute('aria-expanded'), 'false');
  // keyboard: the marker's focus opens it, Tab reaches the button, Esc
  // closes it and leaves the focus on the marker (not on the page)
  mk.focus();
  assert.equal(mk.getAttribute('aria-expanded'), 'true', 'focus opens it');
  unlock.focus();
  assert.ok(doc.activeElement === unlock, 'Tab reaches the button in the card');
  assert.equal(mk.getAttribute('aria-expanded'), 'true', 'the focus in the card keeps it open');
  unlock.click();
  assert.equal(unlocked, 1, 'the button works');
  unlock.dispatchEvent(event('keydown', { key: 'Escape' }));
  assert.ok(doc.activeElement === mk, 'Esc gives the focus back to the marker');
  assert.equal(mk.getAttribute('aria-expanded'), 'false', 'Esc closes it');
  assert.ok(wrap.classList.contains('is-dismissed'));
  // Enter or Space (a click) opens it again
  mk.click();
  assert.equal(mk.getAttribute('aria-expanded'), 'true', 'the marker opens the card Esc closed');
  mk.dispatchEvent(event('keydown', { key: 'Escape' }));
  assert.equal(mk.getAttribute('aria-expanded'), 'false');
  // the focus leaves and comes back: open again
  body.focus();
  mk.focus();
  assert.equal(mk.getAttribute('aria-expanded'), 'true', 'a new focus opens it again');
  body.focus();
  assert.equal(mk.getAttribute('aria-expanded'), 'false');
  // under the pointer with the focus on the page: Esc closes it (WCAG 1.4.13)
  wrap.dispatchEvent(event('pointerenter'));
  assert.equal(mk.getAttribute('aria-expanded'), 'true');
  body.dispatchEvent(event('keydown', { key: 'Escape' }));
  assert.equal(mk.getAttribute('aria-expanded'), 'false', 'Esc closes the hover card');
  wrap.dispatchEvent(event('pointerleave'));
  assert.ok(!wrap.classList.contains('is-dismissed'), 'the next hover opens it again');
  wrap.remove();
}

// ---- G3 (T16, T18): a route change closes the dialogs of the page left ----
// (app.js onRoute calls closeDialogsOnRoute: web/a11y_test.go pins it)

{
  const openOnes = () => dialogs.filter((d) => d.open && d.isConnected);
  const before = openOnes().length;
  const opener = D.h('button', { type: 'button' }, 'open');
  body.append(opener);
  opener.focus();
  location.hash = '#/lecture/A';
  // a plain dialog: closed as cancelled, removed, nothing focused on the old page
  const asked = D.confirmDialog({ title: 'T', message: 'Q?' });
  const dlg = openOnes().at(-1);
  assert.ok(dlg && dlg.open, 'the dialog is open');
  assert.equal(D.closeDialogsOnRoute().closed, 0, 'the same route: nothing closed');
  assert.ok(dlg.open && dlg.isConnected, 'still open on its own route');
  location.hash = '#/settings';
  const r = D.closeDialogsOnRoute();
  assert.equal(r.closed, 1, 'one dialog closed');
  assert.ok(r.opener === opener, 'its opener is named for the caller');
  assert.ok(!dlg.open && !dlg.isConnected, 'closed and removed');
  assert.equal(await asked, false, 'resolved as cancelled (confirmDialog: false)');
  // a dialog opened after the route changed (one the dialog's own action
  // opens with the navigation, e.g. the onboarding's new course) stays
  const later = D.confirmDialog({ title: 'T2', message: 'Q2?' });
  const dlg2 = openOnes().at(-1);
  assert.equal(D.closeDialogsOnRoute().closed, 0, 'opened on the new route: kept');
  assert.ok(dlg2.open, 'still open');
  dlg2.querySelectorAll('button').find((b) => b.className === 'btn').click();
  assert.equal(await later, false);
  // a dialog whose request runs: hidden at once, the request is not
  // dropped, and the dialog resolves with its answer when it comes
  let finish;
  const sending = D.openDialog('Busy', (close, busy) =>
    D.h('button', { type: 'button', class: 'btn-send', onclick: () => busy(new Promise((res) => (finish = res))).then((v) => close(v)) }, 'send'),
  );
  const dlg3 = openOnes().at(-1);
  dlg3.querySelector('button.btn-send').click();
  location.hash = '#/lecture/A';
  assert.equal(D.closeDialogsOnRoute().closed, 1);
  assert.ok(!dlg3.open && !dlg3.isConnected, 'the busy dialog is off the page');
  let settled = false;
  sending.then(() => (settled = true));
  await settle(10);
  assert.ok(!settled, 'not resolved while its request runs');
  finish('sent');
  assert.equal(await sending, 'sent', 'the answer of the request still comes back');
  // a busy dialog whose request fails (its message would be shown in it):
  // resolved as cancelled once the request ended
  let fail;
  const failing = D.openDialog('Busy2', (close, busy) =>
    D.h('button', { type: 'button', class: 'btn-send', onclick: () => busy(new Promise((_, rej) => (fail = rej))).catch(() => {}) }, 'send'),
  );
  openOnes().at(-1).querySelector('button.btn-send').click();
  location.hash = '#/settings';
  assert.equal(D.closeDialogsOnRoute().closed, 1);
  fail(new Error('refused'));
  assert.equal(await failing, null, 'resolved as cancelled after the failed request');
  // R62 N3: the same view after the route change (view.update): restore()
  // gives the focus back only when it is still on the page's body, to the
  // opener found again by its data-fkey (redrawn meanwhile), else to the
  // fallback; a focus the view placed itself is left where it is
  {
    const fallback = D.h('main', { tabindex: '-1' });
    const mk = () => D.h('button', { type: 'button', dataset: { fkey: 'g3:opener' } }, 'open');
    body.append(fallback);
    let op = mk();
    body.append(op);
    op.focus();
    location.hash = '#/settings';
    let run = D.confirmDialog({ title: 'T', message: 'Q' });
    location.hash = '#/settings/asr/dev1';
    let res = D.closeDialogsOnRoute();
    assert.equal(res.closed, 1);
    await run;
    // the view redrew the opener and the focus fell to the page
    op.remove();
    op = mk();
    body.append(op);
    body.focus();
    res.restore(fallback);
    assert.ok(doc.activeElement === op, 'the redrawn opener takes the focus (data-fkey)');
    // the view put the focus on a control of its own (a device's "Test")
    location.hash = '#/settings';
    run = D.confirmDialog({ title: 'T', message: 'Q' });
    location.hash = '#/settings/asr/dev2';
    res = D.closeDialogsOnRoute();
    await run;
    const test = D.h('button', { type: 'button' }, 'Test');
    body.append(test);
    test.focus();
    res.restore(fallback);
    assert.ok(doc.activeElement === test, "the view's own focus is kept");
    // the opener is gone for good: the fallback
    op.focus();
    location.hash = '#/settings';
    run = D.confirmDialog({ title: 'T', message: 'Q' });
    location.hash = '#/settings/x';
    res = D.closeDialogsOnRoute();
    await run;
    op.remove();
    test.remove();
    body.focus();
    res.restore(fallback);
    assert.ok(doc.activeElement === fallback, 'no opener: the fallback (the view)');
    // nothing closed: restore does nothing
    body.focus();
    D.closeDialogsOnRoute().restore(fallback);
    assert.ok(doc.activeElement === body, 'nothing closed: the focus is not moved');
    fallback.remove();
  }
  // R62 N1: a dialog that belongs to no page (route: false; the uploads'
  // "which file to keep", whose cancel means "none" and the flow goes on)
  // stays across a route change
  const kept = D.choiceDialog({ title: 'Keep', message: 'Which?', choices: [{ value: 'a', label: 'A' }], route: false });
  const dlgK = openOnes().at(-1);
  location.hash = '#/lecture/B';
  assert.equal(D.closeDialogsOnRoute().closed, 0, 'route: false is not closed');
  assert.ok(dlgK.open && dlgK.isConnected, 'still open on the new page');
  dlgK.querySelectorAll('button').find((b) => b.className === 'btn').click();
  assert.equal(await kept, null);
  // the uploads' dialog is one
  {
    const Up = await import(new URL('uploads.js', base).href);
    const f = (name) => ({ name, size: 10, type: name.endsWith('.mp4') ? 'video/mp4' : 'audio/mpeg' });
    const adding = Up.addLectureFiles('C1', [f('w1.mp4'), f('w1.mp3')]);
    await settle(5);
    const dlgU = openOnes().at(-1);
    assert.ok(dlgU && dlgU.textContent.includes('w1.mp3'), 'the same-name dialog');
    location.hash = '#/settings';
    assert.equal(D.closeDialogsOnRoute().closed, 0, "the uploads' choice survives a route change");
    assert.ok(dlgU.open, 'still asking');
    dlgU.querySelectorAll('button').find((b) => b.className === 'btn').click();
    assert.equal((await adding).length, 0, 'none kept, as answered');
  }
  assert.equal(openOnes().length, before, 'no dialog left open');
  opener.remove();
  location.hash = '';
  body.focus();
}

// ---- T18e: a button's menu opens upwards when it does not fit below and
// fits above; it never covers its own button. T21 M2 / R68: with room
// neither below nor above, a menu that fits the window is shown whole
// beside the button; without room there either (a narrow window, a wide
// anchor), or taller than the window, it is limited in height on the
// larger side and scrolls ----

{
  const M = await import(new URL('menu.js', base).href);
  const btnAt = (top) => ({ top, bottom: top + 28, left: 900, right: 928 });
  // room below: under the button, 4px apart
  assert.deepEqual(M.anchoredMenuPlace(btnAt(197), 393, 800), { top: 229, maxHeight: null }, 'room below');
  // no room below, room above: above the button, 4px apart
  assert.deepEqual(M.anchoredMenuPlace(btnAt(400), 380, 800), { top: 16, maxHeight: null }, 'no room below, room above');
  // neither (T18e: 399px menu, button 400–428, view 800): the larger side,
  // the menu as tall as that side allows (it scrolls), off the button
  let p = M.anchoredMenuPlace(btnAt(400), 399, 800);
  assert.deepEqual(p, { top: 8, maxHeight: 388 }, 'neither side: above, the larger');
  assert.ok(p.top + p.maxHeight <= 400 - 4, 'above the button');
  p = M.anchoredMenuPlace(btnAt(300), 499, 800);
  assert.deepEqual(p, { top: 332, maxHeight: 460 }, 'neither side: below, the larger');
  // R68: before it is limited, a menu that fits the window goes beside the
  // button, whole (menuBeside → {left, top}): 4px from the button, level
  // with it or as far up as it must go to end 8px from the bottom edge. A
  // right-aligned menu (the ⋯ of a row) goes to the button's left, else to
  // its right; a left-aligned one the other way round
  const wide = { width: 1280, height: 700 };
  const m220 = (height) => ({ width: 220, height });
  // T21 M2's rows (view 700): 11 items (369px) at a button 300–328 lacked
  // 9px below and were cut by them; 12 items (399px) at 380–408
  assert.deepEqual(M.menuBeside(btnAt(300), m220(369), wide, true), { left: 676, top: 300 }, 'to the left of the button, level with it');
  assert.deepEqual(M.menuBeside(btnAt(380), m220(399), wide, true), { left: 676, top: 293 }, 'moved up to end 8px from the bottom edge');
  assert.deepEqual(M.menuBeside(btnAt(300), m220(684), wide, true), { left: 676, top: 8 }, 'exactly the window less its margins: still whole');
  assert.equal(M.menuBeside(btnAt(300), m220(685), wide, true), null, 'taller than the window: not beside (limited instead)');
  const near = { top: 300, bottom: 328, left: 200, right: 228 }; // no room on its left
  assert.deepEqual(M.menuBeside(near, m220(369), wide, true), { left: 232, top: 300 }, 'no room on the left: to the right');
  assert.deepEqual(M.menuBeside(near, m220(369), wide, false), { left: 232, top: 300 }, 'left-aligned: to the right first');
  assert.deepEqual(M.menuBeside(btnAt(300), m220(369), { width: 1000, height: 700 }, false), { left: 676, top: 300 }, 'left-aligned, no room on the right: to the left');
  assert.deepEqual(M.menuBeside({ top: 300, bottom: 328, left: 232, right: 260 }, m220(369), wide, true), { left: 8, top: 300 }, 'the left side fits to the pixel');
  assert.equal(M.menuBeside({ top: 300, bottom: 328, left: 231, right: 259 }, m220(369), { width: 490, height: 700 }, true), null, 'one pixel short on the left, and on the right: neither');
  assert.equal(M.menuBeside({ top: 300, bottom: 328, left: 247, right: 275 }, { width: 313, height: 369 }, { width: 360, height: 700 }, true), null, 'a narrow window: neither side');
  assert.equal(M.menuBeside({ top: 300, bottom: 328, left: 20, right: 1260 }, m220(369), wide, false), null, 'an anchor as wide as the page: neither side');

  // in place: openMenu from an anchor
  const realRect = El.prototype.getBoundingClientRect;
  const anchor = D.h('button', { type: 'button' }, '⋯');
  body.append(anchor);
  let btn = btnAt(400);
  anchor.getBoundingClientRect = () => btn;
  El.prototype.getBoundingClientRect = function () {
    return this.className === 'ctx-menu' ? { top: 0, bottom: 399, left: 0, right: 220, width: 220, height: 399 } : realRect.call(this);
  };
  // the menu's laid-out size, which is what menu.js measures (T21 M3): its
  // content's size, or in height the limit it was given, where it also has
  // a scroll bar (17px wider)
  let menuH = 399;
  let menuW = 220;
  const limitOf = (el) => parseFloat(el.style.maxHeight) || Infinity;
  Object.defineProperty(El.prototype, 'offsetWidth', { configurable: true, get: function () { return this.className === 'ctx-menu' ? (limitOf(this) < menuH ? menuW + 17 : menuW) : 0; } });
  Object.defineProperty(El.prototype, 'offsetHeight', { configurable: true, get: function () { return this.className === 'ctx-menu' ? Math.min(menuH, limitOf(this)) : 0; } });
  const vh = globalThis.innerHeight;
  const vw = globalThis.innerWidth;
  globalThis.innerHeight = 800;
  globalThis.innerWidth = 1280;
  // → where the menu opened from the anchor lies, as laid out
  const openAt = (rect, o = {}) => {
    btn = rect;
    assert.ok(M.openMenu([{ label: 'A', onSelect() {} }], { anchor }, { returnFocus: anchor, align: o.align === undefined ? 'right' : o.align }), 'the menu opens');
    const menu = body.querySelector('div.ctx-menu');
    assert.ok(menu, 'the menu is on the page');
    const left = parseFloat(menu.style.left);
    const top = parseFloat(menu.style.top);
    const at = { left, top, right: left + menu.offsetWidth, bottom: top + menu.offsetHeight, limited: !!menu.style.maxHeight, scrolls: menu.style.overflowY === 'auto', style: menu.style };
    M.closeMenu({ focus: false });
    return at;
  };
  const apart = (a, b) => a.right <= b.left || a.left >= b.right || a.bottom <= b.top || a.top >= b.bottom;
  const inWindow = (a) => a.top >= 8 && a.bottom <= globalThis.innerHeight - 8 && a.left >= 8 && a.right <= globalThis.innerWidth - 8;
  try {
    // T18e's case in place, a wide window: beside the button, whole
    let at = openAt(btnAt(400));
    assert.ok(!at.limited && !at.scrolls, 'it fits the window: whole, no scrolling (T21 M2)');
    assert.equal(at.style.left, '676px', "its right edge 4px from the button's left");
    assert.equal(at.style.top, '393px', 'ending 8px from the bottom edge');
    assert.ok(apart(at, btn), 'not over its button');
    // R68: the rows where neither side has room (T21's table: 11 items
    // 369px, 12 items 399px; views 700 and 800 high): the menu is whole, in
    // the window, and never over its button — a second click on the button
    // closes it, it must not land on an item
    for (const [viewH, h, top] of [[700, 369, 300], [700, 369, 340], [700, 369, 380], [700, 399, 262], [700, 399, 300], [700, 399, 340], [700, 399, 380], [700, 399, 410], [800, 399, 390], [800, 399, 409]]) {
      globalThis.innerHeight = viewH;
      menuH = h;
      at = openAt(btnAt(top));
      const where = `${h}px menu, button at ${top}, view ${viewH}`;
      assert.ok(!at.limited && !at.scrolls, `${where}: whole`);
      assert.ok(apart(at, btn), `${where}: not over its button`);
      assert.ok(inWindow(at), `${where}: 8px from the window's edges`);
      assert.equal(at.left, 676, `${where}: beside the button`);
    }
    // with room below or above it stays under or over the button, its
    // right edge on the button's
    for (const [viewH, h, top] of [[700, 399, 100], [700, 399, 261], [700, 399, 411], [700, 399, 560], [800, 399, 361]]) {
      globalThis.innerHeight = viewH;
      menuH = h;
      at = openAt(btnAt(top));
      assert.ok(!at.limited && apart(at, btn) && inWindow(at), `${h}px menu, button at ${top}, view ${viewH}: whole, off the button`);
      assert.equal(at.right, 928, 'right-aligned to its button');
    }
    // a menu opened from the keyboard on a focused title (left-aligned):
    // to its right
    globalThis.innerHeight = 700;
    menuH = 399;
    at = openAt({ top: 320, bottom: 340, left: 320, right: 520 }, { align: null });
    assert.ok(!at.limited && apart(at, btn) && inWindow(at), 'beside the title, whole');
    assert.equal(at.left, 524);
    // no room beside either: a narrow window (360 wide, a 313px menu), and
    // an anchor as wide as the page (the whole row). Limited on the larger
    // side, scrolling, off the anchor, as before T21 M2
    globalThis.innerWidth = 360;
    menuW = 313;
    menuH = 369;
    at = openAt({ top: 300, bottom: 328, left: 247, right: 275 });
    assert.ok(at.limited && at.scrolls, 'a narrow window: limited, scrolling');
    assert.equal(at.style.maxHeight, '360px', 'as tall as the room below');
    assert.equal(at.style.top, '332px', 'under the button');
    assert.ok(apart(at, btn), 'not over its button');
    globalThis.innerWidth = 1280;
    menuW = 220;
    menuH = 399;
    at = openAt({ top: 320, bottom: 340, left: 20, right: 1260 }, { align: null });
    assert.ok(at.limited && at.scrolls && apart(at, btn), 'an anchor as wide as the page: limited, off it');
    assert.equal(at.style.maxHeight, '348px');
    // taller than the window: limited to the larger side, and it scrolls
    globalThis.innerHeight = 800;
    menuH = 900;
    at = openAt(btnAt(400));
    assert.equal(at.style.top, '8px', 'placed above');
    assert.equal(at.style.maxHeight, '388px', 'as tall as the room above');
    assert.equal(at.style.overflowY, 'auto', 'and scrolls');
    assert.equal(at.style.left, '691px', 'right-aligned by its width with the scroll bar');
    assert.ok(apart(at, btn), 'not over its button');
    menuH = 399;
    btn = btnAt(400);
    // T21 M7: a menu may open on one of its items (the playback rate menu
    // opens on the rate in use, not on 0.5x); the first item otherwise
    const abc = () => ['A', 'B', 'C'].map((label) => ({ label, onSelect() {} }));
    assert.ok(M.openMenu(abc(), { anchor }, { returnFocus: anchor, focus: 2 }), 'the menu opens on an item');
    assert.ok(doc.activeElement && doc.activeElement.textContent.includes('C'), 'the focus is on the item asked for');
    M.closeMenu({ focus: false });
    assert.ok(M.openMenu(abc(), { anchor }, { returnFocus: anchor }), 'the menu opens');
    assert.ok(doc.activeElement && doc.activeElement.textContent.includes('A'), 'the first item without the option');
    M.closeMenu({ focus: false });
    assert.ok(M.openMenu(abc(), { anchor }, { returnFocus: anchor, focus: 9 }), 'the menu opens with an index past its end');
    assert.ok(doc.activeElement && doc.activeElement.textContent.includes('A'), 'an index it has no item for falls back to the first');
    M.closeMenu({ focus: false });
  } finally {
    El.prototype.getBoundingClientRect = realRect;
    delete El.prototype.offsetWidth;
    delete El.prototype.offsetHeight;
    globalThis.innerHeight = vh;
    globalThis.innerWidth = vw;
    anchor.remove();
  }
}

// ---- T21 M3: a menu is measured as it is laid out, not as its opening
// animation draws it (lf-pop starts at scale 0.985); a submenu of a menu
// that scrolls starts at that menu's outer edge, off its scroll bar, and
// closes when the menu is scrolled away under it ----

{
  const M = await import(new URL('menu.js', base).href);
  // the stand-in's layout: a menu is where its style puts it, with the size
  // of its level; while it opens (popping) its rect is 1.5% smaller than
  // its layout size (offsetWidth / offsetHeight)
  const SIZE = { 0: { w: 220, h: 300 }, 1: { w: 222, h: 200 } };
  let popping = true;
  const isMenu = (el) => el.className === 'ctx-menu';
  const realRect = El.prototype.getBoundingClientRect;
  El.prototype.getBoundingClientRect = function () {
    if (this.rect) return this.rect;
    if (!isMenu(this)) return realRect.call(this);
    const s = SIZE[this.dataset.level];
    const k = popping ? 0.985 : 1;
    const left = parseFloat(this.style.left) || 0;
    const top = parseFloat(this.style.top) || 0;
    return { left, top, right: left + s.w * k, bottom: top + s.h * k, width: s.w * k, height: s.h * k };
  };
  for (const [prop, key] of [['offsetWidth', 'w'], ['offsetHeight', 'h']]) {
    Object.defineProperty(El.prototype, prop, {
      configurable: true,
      get() {
        return isMenu(this) ? SIZE[this.dataset.level][key] : 0;
      },
    });
  }
  const vh = globalThis.innerHeight;
  const vw = globalThis.innerWidth;
  globalThis.innerHeight = 800;
  globalThis.innerWidth = 1280;
  const menus = () => body.querySelectorAll('div.ctx-menu');
  const items = () => [{ label: 'A', onSelect() {} }, { label: 'More', items: [{ label: 'S1', onSelect() {} }, { label: 'S2', onSelect() {} }] }, { label: 'B', onSelect() {} }];
  const anchor = D.h('button', { type: 'button' }, '⋯');
  body.append(anchor);
  try {
    // at the pointer, near the bottom: moved up by its whole height
    assert.ok(M.openMenu(items(), { x: 100, y: 700 }, { returnFocus: anchor }), 'the menu opens');
    assert.equal(menus()[0].style.top, '492px', 'ends 8px from the bottom edge by its laid-out height (300), not the drawn one (295.5)');
    M.closeMenu({ focus: false });
    // near the right edge: by its laid-out width
    assert.ok(M.openMenu(items(), { x: 1200, y: 100 }, { returnFocus: anchor }), 'the menu opens');
    assert.equal(menus()[0].style.left, '1052px', 'ends 8px from the right edge by its laid-out width (220)');
    M.closeMenu({ focus: false });
    // from a button with 298px below it: the drawn height (295.5) would
    // fit there, the menu (300) does not; it goes above the button
    anchor.rect = { top: 462, bottom: 490, left: 900, right: 928 };
    assert.ok(M.openMenu(items(), { anchor }, { returnFocus: anchor, align: 'right' }), 'the menu opens');
    assert.equal(menus()[0].style.top, '158px', 'above the button: its laid-out height decides');
    assert.equal(menus()[0].style.left, '708px', 'right-aligned to the button by its laid-out width');
    assert.ok(!menus()[0].style.maxHeight, 'not limited');
    M.closeMenu({ focus: false });

    // a submenu, the animation over
    popping = false;
    const open = (y) => {
      assert.ok(M.openMenu(items(), { x: 703, y }, { returnFocus: anchor }), 'the menu opens');
      const rootMenu = menus()[0];
      const more = rootMenu.querySelectorAll('button.ctx-item')[1];
      more.rect = { left: 709, right: 902, top: y + 36, bottom: y + 66 };
      return { rootMenu, more };
    };
    const right = (el) => el.dispatchEvent(event('keydown', { key: 'ArrowRight' }));
    // a menu that does not scroll: beside the item, 2px over it (as before)
    let { rootMenu, more } = open(100);
    right(more);
    let sub = menus()[1];
    assert.ok(sub && sub.dataset.level === '1', 'the submenu is open');
    assert.equal(sub.style.left, '900px', 'no scroll bar: from the item');
    assert.equal(sub.style.top, '131px', 'level with the item');
    M.closeMenu({ focus: false });
    // not limited, its height a fraction of a pixel: the rounded scroll
    // height can be one more than the rounded client height, which does
    // not make it a menu that scrolls (R68)
    ({ rootMenu, more } = open(100));
    rootMenu.scrollHeight = 299;
    rootMenu.clientHeight = 298;
    right(more);
    assert.equal(menus()[1].style.left, '900px', 'a pixel of rounding is no scroll bar: from the item');
    M.closeMenu({ focus: false });
    // one that scrolls (limited in height, as openMenu limits it): its
    // scroll bar is between the items and its edge; the submenu starts at
    // the edge
    ({ rootMenu, more } = open(100));
    rootMenu.style.maxHeight = '260px';
    rootMenu.scrollTop = 38;
    popping = true; // the edge as laid out too, were the menu still opening
    right(more);
    popping = false;
    sub = menus()[1];
    assert.equal(sub.style.left, '923px', "a menu that scrolls: from the menu's outer edge, off its scroll bar");
    assert.ok(sub.contains(doc.activeElement), 'the focus is in the submenu');
    assert.equal(more.getAttribute('aria-expanded'), 'true');
    // a scroll event without a move (the scroll that brought the item into
    // view, reported late): the submenu stays
    rootMenu.dispatchEvent(event('scroll', { bubbles: false }));
    assert.ok(sub.isConnected, 'the menu did not move: the submenu stays');
    // the menu scrolled under it (the wheel): the submenu closes, the focus
    // goes to its item, without scrolling the menu back
    rootMenu.scrollTop = 0;
    lastFocusOpts = undefined;
    rootMenu.dispatchEvent(event('scroll', { bubbles: false }));
    assert.ok(!sub.isConnected, 'the menu scrolled: the submenu is closed');
    assert.equal(more.getAttribute('aria-expanded'), 'false');
    assert.ok(doc.activeElement === more, 'the focus is back on the item of the submenu');
    assert.ok(lastFocusOpts && lastFocusOpts.preventScroll === true, 'without scrolling the menu back');
    assert.ok(rootMenu.isConnected, 'the menu itself stays open');
    // opened by the pointer (the focus stays on the menu's items): closed
    // the same way, the focus where it is
    right(more);
    sub = menus()[1];
    const first = rootMenu.querySelectorAll('button.ctx-item')[0];
    first.focus();
    rootMenu.scrollTop = 20;
    rootMenu.dispatchEvent(event('scroll', { bubbles: false }));
    assert.ok(!sub.isConnected, 'closed');
    assert.ok(doc.activeElement === first, 'the focus is not moved when it was not in the submenu');
    M.closeMenu({ focus: false });
    // a submenu near the bottom ends 8px from the edge by its laid-out height
    popping = true;
    ({ rootMenu, more } = open(400));
    more.rect = { left: 709, right: 902, top: 700, bottom: 730 };
    right(more);
    assert.equal(menus()[1].style.top, '592px', 'the submenu (200 high) ends at 792, not 3px lower');
    M.closeMenu({ focus: false });
    assert.equal(menus().length, 0, 'no menu left');
  } finally {
    M.closeMenu({ focus: false });
    El.prototype.getBoundingClientRect = realRect;
    delete El.prototype.offsetWidth;
    delete El.prototype.offsetHeight;
    globalThis.innerHeight = vh;
    globalThis.innerWidth = vw;
    anchor.remove();
    body.focus();
  }
}

// ---- T21 M4: the wizard stays across a route change (it asks before it
// drops unsaved work); closed on another page, its opener is found again by
// its data-fkey (the navigation's "add course" is redrawn on every route),
// else the focus goes to the view, not to the page's body ----

{
  const Wz = await import(new URL('wizard.js', base).href);
  // what the wizard needs beyond the stand-in
  El.prototype.toggleAttribute ??= function (k, on) {
    if (on) this.setAttribute(k, '');
    else this.removeAttribute(k);
  };
  const view = D.h('main', { tabindex: '-1' });
  body.append(view);
  const realById = doc.getElementById;
  doc.getElementById = (id) => (id === 'main' ? view : null);
  const mkAdd = () => D.h('button', { type: 'button', dataset: { fkey: 'nav:add' } }, 'add');
  const wizard = () => body.querySelector('dialog.wiz-add');
  const closeWizard = async () => {
    const wiz = wizard();
    assert.ok(wiz && wiz.open, 'the wizard is open');
    assert.ok(wiz.contains(doc.activeElement), 'the focus is in the wizard');
    wiz.querySelector('button.wiz-close').click();
    await settle(10);
    assert.ok(!wiz.isConnected, 'the wizard is closed');
  };
  realFocus = true;
  focusBy = 'keyboard';
  try {
    // opened from the navigation, then the route changes (the back button):
    // the navigation is drawn anew, the wizard stays
    let add = mkAdd();
    body.append(add);
    add.focus();
    location.hash = '#/courses';
    Wz.openWizard({});
    location.hash = '#/';
    add.remove();
    add = mkAdd();
    body.append(add);
    await closeWizard();
    assert.ok(doc.activeElement === add, 'the redrawn opener takes the focus (data-fkey)');
    assert.ok(lastFocusOpts && lastFocusOpts.preventScroll === true, 'without scrolling');
    // an opener of the page left (no key, gone with its view): the new view
    const plain = D.h('button', { type: 'button' }, 'add');
    view.append(plain);
    plain.focus();
    location.hash = '#/courses';
    Wz.openWizard({});
    location.hash = '#/';
    plain.remove();
    await closeWizard();
    assert.ok(doc.activeElement === view, 'no opener on the new page: the view takes the focus');
    // closed on its own page: the opener in place, as before
    add.focus();
    Wz.openWizard({});
    lastFocusOpts = undefined;
    await closeWizard();
    assert.ok(doc.activeElement === add, 'the opener in place takes the focus');
    assert.ok(lastFocusOpts && lastFocusOpts.preventScroll === true, 'without scrolling');
    // its own page, the opener gone without a key: nothing else takes the
    // focus (as after a dialog)
    const once = D.h('button', { type: 'button' }, 'add');
    view.append(once);
    once.focus();
    Wz.openWizard({});
    once.remove();
    await closeWizard();
    assert.ok(doc.activeElement === body, 'the same page: the focus is not moved to the view');
    add.remove();
  } finally {
    realFocus = false;
    focusBy = null;
    doc.getElementById = realById;
    view.remove();
    location.hash = '';
    body.focus();
  }
}

// ---- R62 N4 / N5, T21 M6: the dialogs that send a write let it finish
// when a route change takes them off the page (busy), and an error of such
// a request, which the dialog would have shown, is told as a toast ----

{
  const Cal = await import(new URL('calendar_ui.js', base).href);
  const Kd = await import(new URL('kind_dialog.js', base).href);
  const Fn = await import(new URL('filename_rule.js', base).href);
  const Api = await import(new URL('api.js', base).href);
  const openOnes = () => dialogs.filter((d) => d.open && d.isConnected);
  const toasts = D.h('div', { id: 'toasts' });
  body.append(toasts);
  const realById = doc.getElementById;
  let banner = null; // index.html's "connection lost" banner, when a check has one
  doc.getElementById = (id) => (id === 'toasts' ? toasts : id === 'conn-banner' ? banner : null);
  // the toasts shown, as "kind: message"; taken off for the next check
  const told = () => {
    const out = toasts.children.map((el) => `${el.dataset.kind}: ${el.dataset.msg}`);
    for (const el of [...toasts.children]) {
      clearTimeout(el._timer);
      el.remove();
    }
    return out;
  };
  // writes wait for the test's answer; reads are answered by `answer`
  const realFetch = globalThis.fetch;
  const realAnswer = answer;
  let reply = null;
  const sent = [];
  globalThis.fetch = async (url, init = {}) => {
    const method = init.method || 'GET';
    if (String(url).startsWith('i18n/') || method === 'GET') return realFetch(url, init);
    sent.push(`${method} ${url}`);
    const a = await new Promise((res) => (reply = res));
    const text = JSON.stringify(a.body);
    return { ok: a.status < 300, status: a.status, text: async () => text };
  };
  const respond = async (status, bodyOf) => {
    assert.ok(reply, 'a write is waiting for its answer');
    const r = reply;
    reply = null;
    r({ status, body: bodyOf });
    await settle(20);
  };
  // the page is left while the request runs (the back button)
  const leave = (n = 1) => {
    location.hash = location.hash === '#/settings' ? '#/courses' : '#/settings';
    assert.equal(D.closeDialogsOnRoute().closed, n, 'the route change takes the dialog off the page');
  };
  try {
    location.hash = '#/course/C1';

    // dom.js: a request that fails after its dialog left the page
    {
      const run = () => {
        let ctl;
        const done = D.openDialog('Busy', (close, busy) =>
          D.h('button', { type: 'button', class: 'btn-send', onclick: () => busy(new Promise((res, rej) => (ctl = { res, rej }))).then(close, () => {}) }, 'send'),
        );
        openOnes().at(-1).querySelector('button.btn-send').click();
        return { done, fail: (e) => ctl.rej(e), ok: (v) => ctl.res(v) };
      };
      // still on the page: the dialog shows it itself, no toast
      let r = run();
      r.fail(new Error('refused in place'));
      await settle(10);
      assert.deepEqual(told(), [], 'an error while the dialog is open is not toasted');
      openOnes().at(-1).dispatchEvent(event('cancel'));
      assert.equal(await r.done, null);
      // R68: closed by the user while its request runs (Esc, the backdrop,
      // "cancel"): the request goes on; its error is told as when a route
      // change closed the dialog, a success says nothing (as before)
      r = run();
      openOnes().at(-1).dispatchEvent(event('cancel'));
      assert.equal(await r.done, null, 'closed as cancelled');
      r.fail(new Error('refused after Esc'));
      await settle(10);
      assert.deepEqual(told(), ['error: refused after Esc'], 'the error of a request whose dialog the user closed is toasted');
      r = run();
      openOnes().at(-1).dispatchEvent(event('cancel'));
      await r.done;
      r.fail(Object.assign(new Error('told once'), { reported: true }));
      await settle(10);
      assert.deepEqual(told(), [], 'not when the API layer told it already');
      r = run();
      openOnes().at(-1).dispatchEvent(event('cancel'));
      await r.done;
      r.ok('sent');
      await settle(10);
      assert.deepEqual(told(), [], 'a success after Esc says nothing');
      // off the page: told as an error toast, which stays until closed
      r = run();
      leave();
      r.fail(new Error('the model was refused'));
      assert.equal(await r.done, null, 'resolved as cancelled');
      assert.deepEqual(told(), ['error: the model was refused'], 'the error of a request whose dialog left is toasted');
      // an error without a text: the general one
      r = run();
      leave();
      r.fail(new Error(''));
      await r.done;
      assert.deepEqual(told(), [`error: ${I.t('common.unknown_error')}`]);
      // one the API layer told already (a request not sent quiet), an
      // aborted request, and a success: nothing more
      for (const e of [Object.assign(new Error('told once'), { reported: true }), Object.assign(new Error('aborted'), { name: 'AbortError' }), Object.assign(new Error('cancelled'), { status: -1 })]) {
        r = run();
        leave();
        r.fail(e);
        await r.done;
        assert.deepEqual(told(), [], `not toasted again: ${e.message}`);
      }
      r = run();
      leave();
      r.ok('sent');
      assert.equal(await r.done, 'sent');
      assert.deepEqual(told(), [], 'a success is not toasted here');
      // R68, R76 L-4: a network failure (status 0) while the "connection
      // lost" banner shows is told as the app tells the user's requests
      // then (app.js onError, T24 G6): the banner says the connection is
      // gone, a warning that the action did not happen; any other error
      // then, and a network failure without the banner, are told as errors
      const unreachable = () => Object.assign(new Error('unreachable'), { status: 0 });
      banner = D.h('div', { id: 'conn-banner' });
      banner.hidden = false;
      r = run();
      leave();
      r.fail(unreachable());
      await r.done;
      assert.deepEqual(told(), [`warn: ${I.t('nav.conn_lost_action')}`], 'the banner shows: the network failure is the app\'s "not done" warning');
      r = run();
      leave();
      r.fail(Object.assign(new Error('refused'), { status: 400 }));
      await r.done;
      assert.deepEqual(told(), ['error: refused'], 'the banner shows: another error is still told');
      banner.hidden = true;
      r = run();
      leave();
      r.fail(unreachable());
      await r.done;
      assert.deepEqual(told(), ['error: unreachable'], 'no banner showing: the network failure is told');
      banner = null;
      r = run();
      leave();
      r.fail(unreachable());
      await r.done;
      assert.deepEqual(told(), ['error: unreachable'], 'a page without the banner: told');
    }

    // api.js marks the errors it hands to the app's handler (a toast)
    {
      const seen = [];
      Api.setHandlers({ onError: (e) => seen.push(e) });
      answer = () => ({ status: 500, body: { error: 'boom' } });
      const loud = await Api.api.lecture('L1').catch((e) => e);
      const quiet = await Api.api.lecture('L1', { quiet: true }).catch((e) => e);
      Api.setHandlers({});
      answer = realAnswer;
      assert.ok(seen.length === 1 && seen[0] === loud && loud.reported === true, 'a reported error says so');
      assert.ok(quiet.message === 'boom' && !quiet.reported, 'a quiet one does not');
    }

    const lec = { id: 'L1', title: 'Week 1', course_id: 'C1', kind: 'lecture', kind_source: 'user', recorded_at: '2026-10-05T14:00', recorded_has_time: true, recorded_at_source: 'user' };
    location.hash = '#/course/C1';

    // "change type…": left while saving, the answer still comes back
    {
      let saved = null;
      let opening = Kd.openKindDialog(lec, { onSaved: (l) => (saved = l) });
      await settle(30);
      openOnes().at(-1).querySelector('button.rec-auto').click();
      await settle(10);
      assert.deepEqual(sent.splice(0), ['PUT /api/lectures/L1/kind'], 'the kind is sent');
      leave();
      await respond(200, { ...lec, kind_source: 'auto' });
      assert.ok((await opening) && saved && saved.kind_source === 'auto', 'the saved lecture reaches the caller');
      assert.equal(told().filter((x) => x.startsWith('success: ')).length, 1, 'and the "saved" toast is shown');
      // refused after the dialog left: the server's text as a toast
      opening = Kd.openKindDialog(lec, {});
      await settle(30);
      openOnes().at(-1).querySelector('button.rec-auto').click();
      await settle(10);
      sent.splice(0);
      leave();
      await respond(400, { error: 'kind refused' });
      assert.equal(await opening, null);
      assert.deepEqual(told(), ['error: kind refused'], "the kind dialog's error after it left");
    }

    // "set recording time"
    {
      let saved = null;
      let opening = Cal.openRecordedAtDialog(lec, { onSaved: (l) => (saved = l) });
      openOnes().at(-1).querySelector('button.rec-auto').click();
      await settle(10);
      assert.deepEqual(sent.splice(0), ['PATCH /api/lectures/L1'], 'the recording time is sent');
      leave();
      await respond(200, { ...lec, recorded_at_source: 'filename' });
      assert.ok((await opening) && saved && saved.recorded_at_source === 'filename', 'the saved lecture reaches the caller');
      assert.equal(told().filter((x) => x.startsWith('success: ')).length, 1);
      opening = Cal.openRecordedAtDialog(lec, {});
      openOnes().at(-1).querySelector('button.rec-auto').click();
      await settle(10);
      sent.splice(0);
      leave();
      await respond(500, { error: 'time refused' });
      assert.equal(await opening, null);
      assert.deepEqual(told(), ['error: time refused'], "the recording time dialog's error after it left");
    }

    // the file-name rule: ignore the proposal, recognize again
    {
      const course = { id: 'C1', name: 'Course', code: 'C1' };
      const rule = { regex: '^(?P<y>\\d{4})', source: 'llm', created_at: '2026-10-01T10:00:00Z', samples_matched: 1, samples_total: 1 };
      const row = { filename: '2026 a.mp4', status: 'ok', recorded_at: '2026-10-05', has_time: false, title: 'a' };
      answer = (m, u) => (u.includes('/filename-rule') ? { status: 200, body: { rule: null, kind_map: {}, preview: [], proposal: { rule, kind_map: {}, preview: [row], matched: 1, total: 1, ok: true, problems: [], explanation: 'by year' } } } : realAnswer(m, u));
      const press = async (key) => {
        const b = openOnes().at(-1).querySelector(`button[data-fkey="${key}"]`);
        assert.ok(b && !b.disabled, `${key} can be pressed`);
        b.click();
        await settle(10);
      };
      let changed = 0;
      let opening = Fn.openFilenameRuleDialog(course, { onChanged: () => changed++ });
      await settle(40);
      await press('fn:ignore');
      assert.deepEqual(sent.splice(0), ['DELETE /api/courses/C1/filename-rule/proposal']);
      leave();
      await respond(200, course);
      assert.equal(await opening, 'ignored', 'the answer of "ignore" comes back after the dialog left');
      assert.equal(changed, 1, 'and the caller hears of the change');
      told();
      opening = Fn.openFilenameRuleDialog(course, {});
      await settle(40);
      await press('fn:ignore');
      sent.splice(0);
      leave();
      await respond(500, { error: 'ignore refused' });
      assert.equal(await opening, null);
      assert.deepEqual(told(), ['error: ignore refused'], '"ignore" refused after the dialog left');
      // apply
      opening = Fn.openFilenameRuleDialog(course, {});
      await settle(40);
      await press('fn:apply');
      assert.deepEqual(sent.splice(0), ['PUT /api/courses/C1/filename-rule']);
      leave();
      await respond(200, course);
      assert.ok((await opening) && typeof (await opening) === 'object', 'the answer of "apply" comes back after the dialog left');
      told();
      opening = Fn.openFilenameRuleDialog(course, {});
      await settle(40);
      await press('fn:apply');
      sent.splice(0);
      leave();
      await respond(400, { error: 'apply refused' });
      assert.equal(await opening, null);
      assert.deepEqual(told(), ['error: apply refused'], '"apply" refused after the dialog left');
      // recognize again: its failure is a reason, told the same way
      opening = Fn.openFilenameRuleDialog(course, {});
      await settle(40);
      await press('fn:regen');
      assert.deepEqual(sent.splice(0), ['POST /api/courses/C1/filename-rule/generate']);
      leave();
      await respond(200, {});
      assert.equal(await opening, 'regenerate', 'the answer of "recognize again" comes back after the dialog left');
      told();
      opening = Fn.openFilenameRuleDialog(course, {});
      await settle(40);
      await press('fn:regen');
      sent.splice(0);
      leave();
      await respond(500, { error: 'no samples' });
      assert.equal(await opening, null);
      assert.deepEqual(told(), ['error: no samples'], '"recognize again" refused after the dialog left');
      // in place the reason is shown in the dialog, as before
      opening = Fn.openFilenameRuleDialog(course, {});
      await settle(40);
      await press('fn:regen');
      sent.splice(0);
      await respond(500, { error: 'no samples' });
      const dlg = openOnes().at(-1);
      assert.ok(dlg.textContent.includes('no samples'), 'shown in the dialog');
      assert.deepEqual(told(), [], 'and not toasted');
      assert.ok(!dlg.querySelector('button[data-fkey="fn:regen"]').disabled, 'the button can be pressed again');
      dlg.dispatchEvent(event('cancel'));
      assert.equal(await opening, null);
      answer = realAnswer;
    }

    // the course settings: save, and "recognize again" of its recording
    // times section (the settings dialog's busy)
    {
      const course = { id: 'C1', name: 'Course', code: 'C1', term_start: '', skip_weeks: [], numbering: 'auto' };
      const section = (close, busy) => Fn.recordingSection(course, { close, busy });
      const open = (o = {}) => Cal.openCourseSettings(course, { lectures: [], recordingSection: section, ...o });
      const dlg = () => openOnes().at(-1);
      const save = async () => {
        dlg().querySelector('input[data-fkey="cset:num:never"]').dispatchEvent(event('change'));
        dlg().querySelector('form.cset').dispatchEvent(event('submit'));
        await settle(10);
      };
      let saved = null;
      let opening = open({ onSaved: (c) => (saved = c) });
      await settle(20);
      await save();
      assert.deepEqual(sent.splice(0), ['PATCH /api/courses/C1'], 'the settings are sent');
      leave();
      await respond(200, { ...course, numbering: 'never' });
      assert.ok((await opening) && saved && saved.numbering === 'never', 'the saved course reaches the caller');
      assert.equal(told().filter((x) => x.startsWith('success: ')).length, 1, 'and the "saved" toast is shown');
      opening = open();
      await settle(20);
      await save();
      sent.splice(0);
      leave();
      await respond(400, { error: 'settings refused' });
      assert.equal(await opening, null);
      assert.deepEqual(told(), ['error: settings refused'], "the course settings' error after the dialog left");
      // its "recognize again"
      opening = open();
      await settle(20);
      dlg().querySelector('button[data-fkey="cset:fn-regen"]').click();
      await settle(10);
      assert.deepEqual(sent.splice(0), ['POST /api/courses/C1/filename-rule/generate']);
      leave();
      let settled = false;
      opening.then(() => (settled = true));
      await settle(10);
      assert.ok(!settled, 'the settings dialog waits for the request it sent');
      await respond(500, { error: 'no samples' });
      assert.equal(await opening, null);
      assert.deepEqual(told(), ['error: no samples'], 'the reason is toasted after the dialog left');
      // in place: beside the button, as before
      opening = open();
      await settle(20);
      dlg().querySelector('button[data-fkey="cset:fn-regen"]').click();
      await settle(10);
      sent.splice(0);
      await respond(500, { error: 'no samples' });
      assert.ok(dlg().textContent.includes('no samples'), 'shown in the dialog');
      assert.deepEqual(told(), [], 'and not toasted');
      dlg().dispatchEvent(event('cancel'));
      assert.equal(await opening, null);
    }
    assert.equal(openOnes().length, 0, 'no dialog left open');
  } finally {
    globalThis.fetch = realFetch;
    answer = realAnswer;
    doc.getElementById = realById;
    told();
    toasts.remove();
    location.hash = '';
    body.focus();
  }
}

// ---- R6: the two-choice model dialog (update, rerun) does not start on,
// nor offer, "use the current settings" when one lecture's operation would
// be refused with it (the current provider of a stage of the diff has no
// key); a batch is left as it was ----

{
  const A = await import(new URL('actions.js', base).href);
  const realAnswer = answer;
  const openOnes = () => dialogs.filter((d) => d.open && d.isConnected);
  const an = { label: 'Anthropic / claude-x', provider_id: 'an', provider_name: 'Anthropic', model: 'claude-x' };
  const or = { label: 'OpenRouter / m', provider_id: 'or', provider_name: 'OpenRouter', model: 'm' };
  const ds = { label: 'DeepSeek / m', provider_id: 'ds', provider_name: 'DeepSeek', model: 'm' };
  const providers = [
    { id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com', key_hint: 'sk-…abcd' },
    { id: 'or', name: 'OpenRouter', base_url: 'https://openrouter.ai/api/v1', key_hint: '' },
  ];
  let reads = 0;
  let settingsStatus = 200;
  answer = (m, u) => {
    if (!u.startsWith('/api/settings')) return realAnswer(m, u);
    reads++;
    return { status: settingsStatus, body: settingsStatus === 200 ? { providers, roles: [] } : { error: 'down' } };
  };
  // → the dialog's two radios, its "continue" button and its promise
  const open = async (diff, what = 'rerun:summary') => {
    const before = openOnes().length;
    const asked = A.modelChoiceDialog(diff, what);
    await settle(40);
    assert.equal(openOnes().length, before + 1, 'the dialog is open');
    const dlg = openOnes().at(-1);
    const [snapshot, current] = dlg.querySelectorAll('input');
    const ok = dlg.querySelectorAll('button').at(-1);
    const state = (r) => `${r.disabled ? 'disabled' : 'enabled'}${r.getAttribute('checked') != null ? ' checked' : ''}`;
    return { dlg, asked, snapshot: state(snapshot), current: state(current), ok, cancel: () => dlg.dispatchEvent(event('cancel')) };
  };
  const gone = { code: 'snapshot.unusable.provider_deleted', params: { provider: 'Anthropic' } };
  try {
    // the snapshot's provider is gone and the current one has no key:
    // neither can work, nothing is preselected, "continue" stays off
    let d = await open([{ stage: 'translate', snapshot: an, current: or, snapshot_usable: false, reason_info: gone }]);
    assert.equal(d.snapshot, 'disabled');
    assert.equal(d.current, 'disabled', 'the current settings cannot work: not offered');
    assert.ok(d.ok.disabled, 'nothing to continue with');
    assert.ok(d.dlg.querySelectorAll('input').every((r) => r.getAttribute('autofocus') == null), 'no disabled choice is given the focus');
    assert.ok(d.dlg.querySelectorAll('button').find((b) => b.className === 'btn').getAttribute('autofocus') != null, 'the focus starts on cancel');
    assert.ok(d.dlg.textContent.includes(I.t('errors.snapshot.unusable.no_api_key', { provider: 'OpenRouter' })), 'with the reason');
    d.cancel();
    assert.equal(await d.asked, null);
    // the snapshot's provider is gone, the current one works: as before
    d = await open([{ stage: 'translate', snapshot: an, current: ds, snapshot_usable: false, reason_info: gone }]);
    assert.equal(d.snapshot, 'disabled');
    assert.equal(d.current, 'enabled checked');
    assert.ok(!d.ok.disabled);
    d.cancel();
    await d.asked;
    // the snapshot works, the current provider has no key: the snapshot,
    // and the current settings are not offered
    d = await open([{ stage: 'summary', snapshot: ds, current: or, snapshot_usable: true }]);
    assert.equal(d.snapshot, 'enabled checked');
    assert.equal(d.current, 'disabled');
    assert.ok(!d.ok.disabled);
    assert.ok(d.dlg.querySelectorAll('input')[0].getAttribute('autofocus') != null && d.dlg.querySelectorAll('button').find((b) => b.className === 'btn').getAttribute('autofocus') == null, 'the focus starts on the preselected choice');
    d.cancel();
    await d.asked;
    // the settings cannot be read: nothing is decided here
    settingsStatus = 500;
    d = await open([{ stage: 'translate', snapshot: an, current: or, snapshot_usable: false, reason_info: gone }]);
    assert.equal(d.current, 'enabled checked', 'no settings: left to the back end');
    d.cancel();
    await d.asked;
    settingsStatus = 200;
    // a batch (its diff names the lectures): the choice may still work for
    // some of them; not judged, and the settings are not read for it
    reads = 0;
    d = await open([{ lecture_id: 'L1', lecture_title: 'One', stage: 'translate', snapshot: an, current: or, snapshot_usable: false, reason_info: gone }], 'update');
    assert.equal(d.current, 'enabled checked', 'a batch is not judged');
    assert.equal(reads, 0, 'and reads no settings');
    d.cancel();
    await d.asked;
  } finally {
    answer = realAnswer;
    body.focus();
  }
}

console.log('a11y checks: ok');
