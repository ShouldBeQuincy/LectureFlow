// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 7 (the user simulations T22–T25), the course page, the transcript
// and the player (web/r7_wc_test.go starts it with the web directory as a
// file: URL):
//   T22 B1  the course settings hold the course name and code; "rename…"
//           opens them on the name;
//   T24 B1  a lecture row whose failure needs another model (llm.refusal,
//           llm.time_limit) offers "retry with another model…", not "retry";
// The DOM stand-in, timers and fetch are those of a11y_test.mjs (fetch also
// keeps the request bodies).


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
// takes the children off one by one
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

const base = process.argv[2];

// fetch: the UI catalogs from the web directory; the requests of the
// actions, answered by `answer` after a short delay and recorded in `posts`
const posts = [];
const bodies = []; // the request bodies of the posts, in the same order
let answer = () => ({ status: 200, body: {} });
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const cat = JSON.parse(await readFile(new URL(u, base), 'utf8'));
    const text = JSON.stringify(cat);
    return { ok: true, status: 200, json: async () => cat, text: async () => text };
  }
  const method = init.method || 'GET';
  if (method !== 'GET') {
    posts.push(`${method} ${u}`);
    bodies.push(init.body == null ? null : String(init.body));
  }
  await new Promise((r) => setTimeout(r, 5));
  const a = answer(method, u);
  const text = JSON.stringify(a.body);
  // headers: an answer's {name: value}, names in lower case
  const headers = { get: (k) => (a.headers && a.headers[String(k).toLowerCase()]) ?? null };
  return { ok: a.status < 300, status: a.status, headers, text: async () => text };
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

const openOnes = () => dialogs.filter((d) => d.open && d.isConnected);
// replaceWith (view_transcript.js redraws one row in place)
El.prototype.replaceWith = function (n) {
  const p = this.parentNode;
  if (!p) return;
  n.remove();
  const i = p.children.indexOf(this);
  this.remove();
  n.parentNode = p;
  p.children.splice(i, 0, n);
};
El.prototype.setSelectionRange = function () {};
// select() of a text field: Chrome gives it the focus too
let selected = null;
El.prototype.select = function () {
  selected = this;
  this.focus();
};
const input = (el, v) => {
  el.value = v;
  el.dispatchEvent(event('input'));
};

// ---- T22 B1: the name and the code of a course made earlier ----

{
  const Cal = await import(new URL('calendar_ui.js', base).href);
  const course = { id: 'C1', name: 'Cryptograhpy', code: '', term_start: '', skip_weeks: [], numbering: 'auto' };
  answer = (m, u) => (m === 'PATCH' && u === '/api/courses/C1' ? { status: 200, body: { ...course, name: 'Cryptography', code: 'CSC1003' } } : { status: 404, body: {} });
  posts.length = 0;
  bodies.length = 0;
  let saved = null;
  const opening = Cal.openCourseSettings(course, { lectures: [], focus: 'name', onSaved: (c) => (saved = c) });
  const dlg = openOnes().at(-1);
  assert.ok(dlg, 'the course settings are open');
  const name = dlg.querySelector('input[data-fkey="cset:name"]');
  const code = dlg.querySelector('input[data-fkey="cset:code"]');
  const save = dlg.querySelector('button[data-fkey="cset:save"]');
  assert.ok(name && code, 'the course settings have the course name and the course code');
  assert.equal(name.value, 'Cryptograhpy', 'the name as it is');
  assert.equal(code.value, '', 'no code yet');
  assert.ok(name.getAttribute('autofocus') != null, '"rename…" opens the settings on the name');
  await settle(5);
  assert.ok(selected === name, 'selected, to be typed over');
  assert.ok(save.disabled, 'nothing changed: nothing to save');
  // the server's rules, said beside the field (course.code_has_space, name_empty)
  input(code, 'CSC 1003');
  assert.ok(save.disabled, 'a code with a space is not sent');
  assert.equal(code.getAttribute('aria-invalid'), 'true');
  assert.ok(dlg.textContent.includes(I.t('wizard.basics.code_has_space')), 'with the reason');
  input(code, 'CSC1003');
  assert.ok(!save.disabled, 'a code added: it can be saved');
  input(name, '   ');
  assert.ok(save.disabled, 'an empty name is not sent');
  assert.equal(name.getAttribute('aria-invalid'), 'true');
  assert.ok(dlg.textContent.includes(I.t('errors.course.name_empty')));
  input(name, ' Cryptography ');
  assert.ok(!save.disabled);
  dlg.querySelector('form.cset').dispatchEvent(event('submit'));
  await settle();
  assert.deepEqual(posts, ['PATCH /api/courses/C1'], 'one PATCH, as for the calendar');
  assert.deepEqual(JSON.parse(bodies[0]), { name: 'Cryptography', code: 'CSC1003' }, 'the trimmed name and the code');
  assert.ok((await opening) && saved && saved.name === 'Cryptography', 'the saved course reaches the page');

  // the code cleared: "" (the server clears it); the name unchanged is not sent
  posts.length = 0;
  bodies.length = 0;
  const withCode = { ...course, name: 'Cryptography', code: 'CSC1003' };
  answer = () => ({ status: 200, body: { ...withCode, code: null } });
  selected = null;
  const again = Cal.openCourseSettings(withCode, { lectures: [] });
  const dlg2 = openOnes().at(-1);
  assert.ok(dlg2.querySelector('input[data-fkey="cset:name"]').getAttribute('autofocus') == null, 'plain "course settings": the name has no autofocus of its own');
  await settle(5);
  assert.equal(selected, null, 'nor is it selected');
  input(dlg2.querySelector('input[data-fkey="cset:code"]'), '');
  dlg2.querySelector('form.cset').dispatchEvent(event('submit'));
  await settle();
  assert.deepEqual(JSON.parse(bodies[0]), { code: '' }, 'only the code, cleared');
  await again;
  assert.equal(openOnes().length, 0, 'no dialog left open');
  answer = () => ({ status: 200, body: {} });
  body.focus();
}


// ---- T24 B1: a row whose failure another model gets past ----

const stage = (name, status, o = {}) => ({ stage: name, status, display_status: status, progress: status === 'done' ? 1 : 0, attempt: 0, error: null, allowed_actions: [], started_at: null, finished_at: null, duration_ms: null, stats: {}, ...o });
const names = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
function lecture(id, o = {}) {
  return {
    id, course_id: 'C1', title: `Lecture ${id}`, label: 'L01', recorded_at: '', numbered: false, week: 0, kind: 'lecture', draft: false, created_at: '2026-10-27T10:00:00Z',
    duration_ms: 0, counts: { sentences: 0, patches_pending: 0 }, exports: [], materials: [], playable: false, lecture_actions: [],
    stages: names.map((n) => stage(n, 'pending')), ...o,
  };
}
const ctx = { system: () => null, resources: () => null };
let shown = [];
const list = L.createLectureList({
  ctx,
  onLectures: (ls) => {
    for (const l of ls) shown = shown.map((x) => (x.id === l.id ? l : x));
    list.render(shown);
  },
  onDeleted() {},
  course: () => null,
  filtered: () => false,
});
body.append(list.table);
const failedAt = (code) =>
  lecture('A', { playable: true, stages: names.map((n) => (n === 'translate' ? stage(n, 'failed', { error: 'x', error_info: { code }, allowed_actions: ['retry'] }) : stage(n, n === 'export' ? 'pending' : 'done'))) });

{
  for (const code of ['llm.refusal', 'llm.time_limit']) {
    shown = [failedAt(code)];
    list.render(shown);
    assert.equal(fk('lec:A:retry'), null, `${code}: no plain "retry" as the row's action`);
    const b = fk('lec:A:retry_model');
    assert.ok(b, `${code}: "retry with another model…" in its place`);
    assert.equal(b.textContent.trim(), I.t('common.action.retry_model'));
    assert.ok(b.getAttribute('title').includes(U.stageName('translate')), 'its hover names the stage');
    assert.ok(b.dataset.fnext.split(' ').includes('lec:A:play'), 'gone after the retry: the focus goes to the row\'s new action');
  }
  // it opens the model dialog; nothing is sent before a choice
  posts.length = 0;
  answer = (m, u) => (u.startsWith('/api/settings') ? { status: 200, body: { providers: [], roles: [] } } : { status: 404, body: {} });
  const before = openOnes().length;
  fk('lec:A:retry_model').click();
  await settle(60);
  assert.equal(openOnes().length, before + 1, 'the dialog of the retry with another model');
  assert.deepEqual(posts, [], 'no retry sent on its own');
  openOnes().at(-1).dispatchEvent(event('cancel'));
  await settle();
  // any other failure: "retry" as before
  shown = [failedAt('llm.rate_limited')];
  list.render(shown);
  assert.ok(fk('lec:A:retry'), 'a failure a retry can get past: "retry"');
  assert.equal(fk('lec:A:retry_model'), null);
  answer = () => ({ status: 200, body: {} });
  list.table.remove();
  body.focus();
}


// ---- T25 X1, T22 G3: "update" and "start all drafts" ask first ----

{
  const C = await import(new URL('update_confirm.js', base).href);
  const usage = (cost) => ({ prompt_tokens: 1000, cache_hit_tokens: 0, completion_tokens: 100, estimated_cost: cost });
  const glossary = { code: 'glossary_updated', params: { from: 0, to: 1 } };
  const staleLec = (id, o = {}) =>
    lecture(id, {
      lecture_actions: ['update'],
      stale: { reasons: [glossary], stages: ['repair', 'translate'] },
      stages: names.map((n) => (n === 'repair' || n === 'translate' ? stage(n, 'stale', { duration_ms: 60000, stats: usage(0.1) }) : stage(n, 'done', { duration_ms: n === 'export' ? 1000 : 5000 }))),
      ...o,
    });
  // the plan: what the back end reruns (the earliest stale stage, the later ones follow)
  const p = C.updatePlan([staleLec('U')]);
  assert.deepEqual(p.stages, ['repair', 'translate', 'export'], 'the earliest stale stage and every later one');
  assert.equal(p.usage.estimated_cost.toFixed(2), '0.20', 'their estimated cost the last time');
  assert.equal(p.usage.prompt_tokens, 2000);
  assert.equal(p.ms, 121000, 'and their time');
  assert.ok(p.keepsEdits, 'the transcription is not rerun: hand edits stay');
  assert.ok(!C.updatePlan([lecture('X', { stages: names.map((n) => (n === 'asr' ? stage(n, 'stale') : stage(n, 'done'))) })]).keepsEdits, 'a rerun from the transcription does not say so');
  assert.deepEqual(C.updatePlan([lecture('Y')]).lectures, [], 'nothing stale: nothing to ask');

  const okOf = (d) => d.querySelectorAll('button').find((b) => /btn-primary/.test(b.className));
  // the row's "update": the dialog first, nothing sent before "update"
  body.append(list.table);
  shown = [staleLec('U')];
  list.render(shown);
  answer = (m, u) => (u === '/api/lectures/U/update' ? { status: 200, body: staleLec('U', { lecture_actions: [], stale: null, stages: names.map((n) => stage(n, n === 'repair' ? 'queued' : n === 'translate' || n === 'export' ? 'pending' : 'done')) }) } : { status: 404, body: {} });
  posts.length = 0;
  const before = openOnes().length;
  fk('lec:U:update').click();
  await settle();
  assert.equal(openOnes().length, before + 1, 'the row\'s "update" asks first');
  let dlg = openOnes().at(-1);
  assert.deepEqual(posts, [], 'nothing is sent while it asks');
  const text = dlg.textContent;
  for (const s of ['repair', 'translate', 'export']) assert.ok(text.includes(U.stageName(s)), `it names ${s}`);
  assert.ok(text.includes(U.staleReasonText(glossary)), 'and why');
  assert.ok(text.includes(U.usageText(p.usage)), 'and what those stages took the last time');
  assert.ok(text.includes(U.fmtDuration(121000)));
  assert.ok(text.includes(I.t('course.confirm.update_replace_keep')), 'and that the results are replaced, the hand edits kept');
  const cancelBtn = dlg.querySelectorAll('button').find((b) => b.className === 'btn');
  assert.ok(cancelBtn.getAttribute('autofocus') != null, 'it costs money: the focus starts on "cancel"');
  dlg.dispatchEvent(event('cancel'));
  await settle();
  assert.deepEqual(posts, [], 'cancelled: nothing sent');
  fk('lec:U:update').click();
  await settle();
  dlg = openOnes().at(-1);
  okOf(dlg).click();
  await settle(60);
  assert.deepEqual(posts, ['POST /api/lectures/U/update'], 'confirmed: the update is sent as before');
  // the selection's "update": one dialog for all, then one request
  body.append(list.bar);
  shown = [staleLec('U'), staleLec('V')];
  list.render(shown);
  for (const id of ['U', 'V']) {
    const box = fk(`lec:${id}:check`);
    box.checked = true; // as the click leaves it
    box.click();
  }
  posts.length = 0;
  answer = (m, u) => (u === '/api/lectures/update' ? { status: 200, body: { results: [] } } : { status: 404, body: {} });
  fk('batch:update').click();
  await settle();
  dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes(I.t('course.confirm.update_title_many', { n: 2 })), 'the batch asks once, for 2 lectures');
  assert.ok(dlg.textContent.includes('Lecture U') && dlg.textContent.includes('Lecture V'), 'naming them');
  assert.ok(dlg.textContent.includes(U.usageText({ prompt_tokens: 4000, cache_hit_tokens: 0, completion_tokens: 400, estimated_cost: 0.4 })), 'with their stages\' last cost added up');
  okOf(dlg).click();
  await settle(60);
  assert.equal(posts.length, 1, 'one request');
  fk('batch:clear').click();

  // the course's "update all" with one lecture to update names it
  let asked = C.confirmUpdate([staleLec('U')], { named: true });
  dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes('Lecture U'), 'from the course\'s "update all": which lecture');
  dlg.dispatchEvent(event('cancel'));
  assert.equal(await asked, false);

  // "start all drafts": how many, every stage, that it costs; the length when known
  const drafts = [lecture('D1', { draft: true, lecture_actions: ['launch'], duration_ms: 3600000 }), lecture('D2', { draft: true, lecture_actions: ['launch'], duration_ms: 1800000 })];
  asked = C.confirmLaunchDrafts(drafts, 2);
  dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes(I.t('course.confirm.launch_title', { n: 2 })));
  assert.ok(dlg.textContent.includes(U.stageName('asr')) && dlg.textContent.includes(U.stageName('translate')), 'every stage is named');
  assert.ok(dlg.textContent.includes(U.fmtDuration(5400000)), 'the recordings\' length');
  assert.ok(dlg.textContent.includes(I.t('course.confirm.launch_cost')));
  dlg.dispatchEvent(event('cancel'));
  assert.equal(await asked, false);
  // the list not loaded, or a length unknown: the count from the course
  asked = C.confirmLaunchDrafts([], 3);
  dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes(I.t('course.confirm.launch_title', { n: 3 })));
  okOf(dlg).click();
  assert.equal(await asked, true);
  answer = () => ({ status: 200, body: {} });
  list.table.remove();
  list.bar.remove();
  body.focus();
}


// ---- T25 B3: the English files made on demand show their size too ----

{
  const E = await import(new URL('lecture_english.js', base).href);
  const lec = lecture('E', {
    playable: true,
    exports: [
      { name: 'W01L1 X.bilingual.md', url: '/api/lectures/E/exports/W01L1%20X.bilingual.md', size: 2048 },
      { name: 'W01L1 X.en.txt', url: '/api/lectures/E/exports/W01L1%20X.en.txt', size: 900 },
    ],
  });
  answer = (m, u) => {
    if (m === 'HEAD' && u.endsWith('/english/vtt')) return { status: 200, body: '', headers: { 'content-length': '1325' } };
    if (m === 'HEAD' && u.endsWith('/english/txt')) return { status: 500, body: {} };
    return { status: 404, body: {} };
  };
  posts.length = 0;
  const items = E.lectureDownloadItems(lec);
  const vtt = items.find((x) => x && x.id === 'english:vtt');
  const txt = items.find((x) => x && x.id === 'english:txt');
  // R74 low 4: a lecture's menu (with the download submenu in it) is built
  // on every open; the sizes are asked only when the items are drawn
  // (menu.js reads the hint as it draws an item: a submenu when it opens)
  await settle();
  assert.deepEqual(posts, [], 'building the menu asks nothing');
  assert.ok(vtt && vtt.hint && txt && txt.hint, 'both English files have a size, as the export files do');
  assert.equal(txt.hint.textContent, U.fmtBytes(900), 'the transcript: the exported one\'s size at once (the same text)');
  await settle();
  assert.equal(vtt.hint.textContent, U.fmtBytes(1325), 'the subtitles: the size the server gives (HEAD)');
  assert.equal(txt.hint.textContent, U.fmtBytes(900), 'a size that could not be asked stays as it was');
  assert.deepEqual(posts.sort(), ['HEAD /api/lectures/E/english/txt', 'HEAD /api/lectures/E/english/vtt'], 'asked with HEAD: nothing is downloaded');
  // the menu again, the lecture as it was: the size known is not asked again
  // (one that could not be asked is)
  posts.length = 0;
  const again = E.lectureDownloadItems({ ...lec });
  const vtt2 = again.find((x) => x && x.id === 'english:vtt');
  void vtt2.hint;
  void again.find((x) => x && x.id === 'english:txt').hint;
  await settle();
  assert.deepEqual(posts, ['HEAD /api/lectures/E/english/txt'], 'the subtitles are not asked again');
  assert.equal(vtt2.hint.textContent, U.fmtBytes(1325), 'their size from before');
  // the lecture changed (a stage ran again): asked anew
  posts.length = 0;
  const changed = { ...lec, stages: lec.stages.map((s) => (s.stage === 'translate' ? { ...s, status: 'done', finished_at: '2026-10-27T11:00:00Z' } : s)) };
  void E.lectureDownloadItems(changed).find((x) => x && x.id === 'english:vtt').hint;
  await settle();
  assert.deepEqual(posts, ['HEAD /api/lectures/E/english/vtt'], 'the lecture changed: asked again');
  answer = () => ({ status: 200, body: {} });
}


// ---- T25 S3: "translate again" that gives back the same translation says so ----

const toasts = D.h('div', { id: 'toasts' });
body.append(toasts);
doc.getElementById = (id) => (id === 'toasts' ? toasts : null);
// the toasts shown, as "kind: message"; taken off for the next check
const told = () => {
  const out = toasts.children.map((el) => `${el.dataset.kind}: ${el.dataset.msg}`);
  for (const el of [...toasts.children]) {
    clearTimeout(el._timer);
    el.remove();
  }
  return out;
};
const sentence = (idx, o = {}) => ({ idx, start_ms: idx * 1000, end_ms: idx * 1000 + 900, text: `Sentence ${idx}.`, asr_text: `Sentence ${idx}.`, human_text: null, zh: `句子 ${idx}。`, zh_status: 'ok', patch: null, low_conf_words: [], qa_error: null, ...o });
// the menu item of a row's ⋯ (the lecture page) with this label, clicked
async function pick(moreKey, label) {
  fk(moreKey).click();
  await settle(10);
  const item = doc.querySelectorAll('.ctx-item').find((b) => b.textContent.includes(label));
  assert.ok(item, `the menu has "${label}"`);
  item.click();
  await settle(30);
}

{
  globalThis.IntersectionObserver ??= class {
    observe() {}
    disconnect() {}
  };
  const T = await import(new URL('view_transcript.js', base).href);
  let rows = [sentence(0), sentence(1)];
  let next = null; // the translation the "model" gives on the next run
  answer = (m, u) => {
    if (m === 'GET' && u.includes('/api/lectures/T/sentences')) return { status: 200, body: { total: rows.length, sentences: rows } };
    if (m === 'POST' && u === '/api/lectures/T/sentences/1/retranslate') {
      rows = [rows[0], { ...rows[1], zh_status: 'stale' }];
      return { status: 200, body: rows[1] };
    }
    if (m === 'POST' && u === '/api/lectures/T/sentences/1/edit') {
      rows = [rows[0], { ...rows[1], text: 'Edited.', human_text: 'Edited.', zh_status: 'stale' }];
      return { status: 200, body: rows[1] };
    }
    return { status: 404, body: {} };
  };
  // the incremental translation ran: the sentence is translated again
  const translated = (view) => {
    rows = [rows[0], { ...rows[1], zh: next, zh_status: 'ok' }];
    view.onChanged({ kind: 'sentences', id: 'T' });
  };
  const host = D.h('div');
  body.append(host);
  const lec = { ...lecture('T'), reviewable: true, counts: { sentences: 2, patches_pending: 0 } };
  const view = T.mountTranscript(host, lec, { courses: () => [], system: () => null, resources: () => null });
  await settle(60);
  assert.ok(fk('more:1'), 'the transcript shows its sentences');
  told();

  // the server's "changed sentences" right after the POST: the page reads
  // the sentence waiting (stale)
  const refreshed = async () => {
    view.onChanged({ kind: 'sentences', id: 'T' });
    await settle(500);
  };

  // the same translation back: told, with the ways to another one
  await pick('more:1', I.t('player.menu.retranslate'));
  assert.deepEqual(told(), [`success: ${I.t('player.menu.retranslate_done')}`], 'marked, as before');
  await refreshed();
  next = '句子 1。';
  translated(view);
  await settle(500);
  // R74 low 5: it stays until closed (no timer), as the error toasts do
  assert.ok(toasts.children.length === 1 && toasts.children[0]._timer === undefined && toasts.children[0].querySelector('.toast-close'), 'it stays until the user closes it');
  assert.deepEqual(told(), [`info: ${I.t('transcript.retranslate.same')}`], 'the same translation came back: said so');
  // again, once: a later refresh says nothing more
  await refreshed();
  assert.deepEqual(told(), [], 'told once');

  // a different translation: it shows in the row, nothing more is said
  await pick('more:1', I.t('player.menu.retranslate'));
  told();
  await refreshed();
  next = '第一句话。';
  translated(view);
  await settle(500);
  assert.deepEqual(told(), [], 'a new translation: no note');
  assert.ok(host.textContent.includes('第一句话。'));

  // R74 low 5 (1): a read sent before the POST, answered after it, still
  // has the old translation as done: it is not the result
  await pick('more:1', I.t('player.menu.retranslate'));
  told();
  const waiting = rows[1];
  rows = [rows[0], { ...waiting, zh_status: 'ok' }]; // the old read
  await refreshed();
  assert.deepEqual(told(), [], 'a read from before the retranslation: no note');
  rows = [rows[0], waiting];
  await refreshed(); // now waiting
  next = '第一句话。';
  translated(view);
  await settle(500);
  assert.deepEqual(told(), [`info: ${I.t('transcript.retranslate.same')}`], 'the result after it is still compared');

  // (3) the retranslation never came (failed, cancelled): a whole update
  // much later with the same translation is not taken for it
  await pick('more:1', I.t('player.menu.retranslate'));
  told();
  await refreshed();
  clock += 31 * 60 * 1000;
  translated(view);
  await settle(500);
  assert.deepEqual(told(), [], 'long after: not the retranslation asked for');

  // (2) a sentence without a translation translated again without one: not "the same"
  rows = [rows[0], { ...rows[1], zh: '', zh_status: 'qa_failed' }];
  await refreshed();
  await pick('more:1', I.t('player.menu.retranslate'));
  told();
  await refreshed();
  next = '';
  rows = [rows[0], { ...rows[1], zh: '', zh_status: 'qa_failed' }];
  await refreshed();
  assert.deepEqual(told(), [], 'no translation before or after: no note');
  rows = [rows[0], { ...rows[1], zh: '第一句话。', zh_status: 'ok' }];
  await refreshed();

  // retranslated, then the English edited before the result: nothing to compare
  posts.length = 0;
  await pick('more:1', I.t('player.menu.retranslate'));
  told();
  await refreshed();
  await pick('more:1', I.t('player.menu.edit'));
  const ta = host.querySelector('textarea');
  ta.value = 'Edited.';
  ta.dispatchEvent(event('input'));
  ta.dispatchEvent(event('keydown', { key: 'Enter' }));
  await settle(40);
  assert.deepEqual(posts, ['POST /api/lectures/T/sentences/1/retranslate', 'POST /api/lectures/T/sentences/1/edit']);
  told();
  next = '第一句话。';
  translated(view);
  await settle(500);
  assert.deepEqual(told(), [], 'edited meanwhile: the old translation is not compared');
  view.destroy();
  host.remove();
  answer = () => ({ status: 200, body: {} });
  body.focus();
}


// ---- T22 S2, T23 general 7, T22 G15: "edit" changes the English; the
// translation is made from it and the glossary, and the edit says so ----

{
  const T = await import(new URL('view_transcript.js', base).href);
  const K = await import(new URL('player_keys.js', base).href);
  const rows = [sentence(0)];
  answer = (m, u) => (m === 'GET' && u.includes('/api/lectures/T/sentences') ? { status: 200, body: { total: rows.length, sentences: rows } } : { status: 404, body: {} });
  const host = D.h('div');
  body.append(host);
  const lec = { ...lecture('T'), reviewable: true, counts: { sentences: 1, patches_pending: 0 } };
  const view = T.mountTranscript(host, lec, { courses: () => [], system: () => null, resources: () => null });
  await settle(60);
  await pick('more:0', I.t('player.menu.edit'));
  const box = host.querySelector('textarea');
  assert.ok(box, 'the inline edit of the English sentence');
  assert.ok(host.textContent.includes(I.t('player.edit.zh_note')), 'it says how the translation is made and how to change it');
  box.dispatchEvent(event('keydown', { key: 'Escape' }));
  view.destroy();
  host.remove();
  // the player's shortcut list names the sentence menu, which has no button of its own
  assert.ok(
    K.shortcutRows().some(([, label]) => label === 'player.help.sentence_menu'),
    "the player's ? list has the sentence menu",
  );
  answer = () => ({ status: 200, body: {} });
  body.focus();
}


// ---- T23 serious 3: "copy citation" from a sentence's menu keeps the focus ----
// With the clipboard API refused (a page without the focus: the hidden
// pane of the test, a background window) dom.js copies through a hidden
// text field, which takes the focus and leaves it on the page's body.

{
  const T = await import(new URL('view_transcript.js', base).href);
  const rows = [sentence(0), sentence(1)];
  answer = (m, u) => (m === 'GET' && u.includes('/api/lectures/T/sentences') ? { status: 200, body: { total: rows.length, sentences: rows } } : { status: 404, body: {} });
  const host = D.h('div');
  body.append(host);
  const lec = { ...lecture('T'), reviewable: true, counts: { sentences: 2, patches_pending: 0 } };
  const view = T.mountTranscript(host, lec, { courses: () => [{ id: 'C1', code: 'CSC1003', name: 'Cryptography' }], system: () => null, resources: () => null });
  await settle(60);
  let copied = null;
  doc.execCommand = (cmd) => {
    copied = cmd === 'copy' ? selected && selected.value : null;
    return cmd === 'copy';
  };
  realFocus = true; // a removed focused element leaves the focus on the body
  try {
    const more = fk('more:1');
    more.focus();
    told();
    await pick('more:1', I.t('player.menu.copy_cite'));
    assert.ok(copied && copied.startsWith('[CSC1003 '), 'copied through the hidden field');
    assert.equal(told().length, 1, 'with its toast');
    assert.ok(doc.activeElement === fk('more:1'), 'the focus is back on the sentence\'s ⋯, not on the page');
    // the helper the player uses too: the same element, focus moved meanwhile left alone
    const b = fk('more:0');
    b.focus();
    assert.equal(await T.copyKeepingFocus('x'), true);
    assert.ok(doc.activeElement === b, 'back where it was');
  } finally {
    realFocus = false;
    delete doc.execCommand;
    view.destroy();
    host.remove();
    told();
    answer = () => ({ status: 200, body: {} });
    body.focus();
  }
}


// ---- T23 general 17: the player's transcript says which sentence plays ----

{
  const PT = await import(new URL('player_transcript.js', base).href);
  const tr = PT.createTranscript({ onSeek() {}, menuItems: () => null, duration: () => 10000 });
  body.append(tr.el);
  tr.el.querySelector('input').value = ''; // the search field, empty
  tr.setSentences([sentence(0), sentence(1), sentence(2)]);
  const rowsOf = () => tr.el.querySelectorAll('.pl-row');
  const current = () => rowsOf().filter((r) => r.getAttribute('aria-current') === 'true').map((r) => r.dataset.idx);
  tr.setCurrent(1);
  assert.deepEqual(current(), ['1'], 'the sentence playing is the current one (aria-current)');
  assert.ok(rowsOf()[1].tabIndex === 0 && rowsOf()[0].tabIndex === -1, 'Tab into the list lands on it, not on the first sentence');
  tr.setCurrent(2);
  assert.deepEqual(current(), ['2'], 'it follows the playback');
  // redrawn (an edit came back): still marked
  tr.updateSentence(sentence(2, { zh: '新的。' }));
  assert.deepEqual(current(), ['2'], 'a redrawn current sentence keeps the mark');
  // while the keyboard is in the list, the tab stop stays where the user is
  rowsOf()[0].focus();
  tr.setCurrent(1);
  assert.deepEqual(current(), ['1']);
  assert.ok(rowsOf()[1].tabIndex !== 0, 'the focus is in the list: its tab stop is not moved');
  tr.el.remove();
  body.focus();
}


// ---- P2 hotspot 2: the player's transcript redraws only the sentences that changed ----
// (a "changed sentences" event, or the refresh after an edit, read every
// sentence again and rebuilt the whole list: 0.5–1 s at 1500 sentences)

{
  const PT = await import(new URL('player_transcript.js', base).href);
  const tr = PT.createTranscript({ onSeek() {}, menuItems: () => null, duration: () => 10000 });
  body.append(tr.el);
  const search = tr.el.querySelector('input');
  search.value = '';
  const five = () => [0, 1, 2, 3, 4].map((i) => sentence(i));
  tr.setSentences(five());
  const rowsOf = () => tr.el.querySelectorAll('.pl-row');
  tr.setCurrent(1);
  const before = rowsOf();
  // the keyboard on sentence 3, a search for "Sentence 3"
  before[3].focus();
  search.value = 'sentence 3';
  search.dispatchEvent(event('keydown', { key: 'Enter' }));
  assert.ok(before[3].classList.contains('is-hit') && before[3].classList.contains('is-hit-cur'), 'the search found sentence 3');
  before[3].focus();

  // sentence 3 translated again, the others as they were
  const next = five();
  next[3] = sentence(3, { zh: '新的译文。' });
  tr.setSentences(next);
  const after = rowsOf();
  assert.equal(after.length, 5);
  for (const i of [0, 1, 2, 4]) assert.ok(after[i] === before[i], `sentence ${i} unchanged: its row is not rebuilt`);
  assert.ok(after[3] !== before[3] && after[3].textContent.includes('新的译文。'), 'the changed sentence is redrawn');
  assert.ok(doc.activeElement === after[3], 'the keyboard stays on it');
  assert.equal(after[3].getAttribute('tabindex'), '0', 'and it is the tab stop');
  assert.ok(after[3].classList.contains('is-hit') && after[3].classList.contains('is-hit-cur') && after[3].querySelector('mark'), 'its search mark stays');
  assert.equal(after[1].getAttribute('aria-current'), 'true', 'the sentence playing stays marked');
  // the same sentences again: nothing redrawn
  tr.setSentences(next.map((s) => ({ ...s })));
  assert.ok(rowsOf().every((r, i) => r === after[i]), 'nothing changed: nothing redrawn');
  // a sentence whose time moved, or another number of sentences: the whole list, as before
  const moved = next.map((s) => ({ ...s }));
  moved[2] = { ...moved[2], start_ms: moved[2].start_ms + 100 };
  tr.setSentences(moved);
  assert.ok(rowsOf()[0] !== after[0], 'a time moved (the chapters follow the times): the whole list');
  const fewer = moved.slice(0, 4);
  const kept = rowsOf();
  tr.setSentences(fewer);
  assert.equal(rowsOf().length, 4);
  assert.ok(rowsOf()[0] !== kept[0], 'another number of sentences: the whole list');
  assert.equal(rowsOf()[1].getAttribute('aria-current'), 'true', 'the sentence playing is marked after a whole redraw too');
  tr.el.remove();
  body.focus();
}


// ---- P2 hotspot 3: one lecture changed, only its row is redrawn ----
// (the course page rebuilt its whole list on every lecture's SSE refresh:
// 40 rows, some 8 000 elements, 180–640 ms at x4)

{
  let shownC = [];
  const listC = L.createLectureList({
    ctx,
    onLectures: (ls) => {
      for (const l of ls) shownC = shownC.map((x) => (x.id === l.id ? l : x));
      listC.render(shownC);
    },
    onDeleted() {},
    course: () => null,
    filtered: () => false,
  });
  body.append(listC.table, listC.bar);
  const row = (id) => listC.table.querySelectorAll('.lrow').find((r) => r.dataset.id === id);
  shownC = [lecture('A', { seq: 1 }), lecture('B', { seq: 2, lecture_actions: ['launch'] }), lecture('C', { seq: 3 })];
  listC.render(shownC);
  // B selected, the keyboard on C's ⋯
  const box = fk('lec:B:check');
  box.checked = true;
  box.click();
  const [a0, b0, c0] = ['A', 'B', 'C'].map(row);
  const group0 = listC.table.querySelector('.lt-group');
  fk('lec:C:more').focus();
  // A runs now
  const running = lecture('A', { seq: 1, title: 'Lecture A (running)', stages: names.map((n) => stage(n, n === 'prep' ? 'running' : 'queued')) });
  assert.equal(listC.updateRow(running), true, 'one lecture changed in place: its row only');
  assert.ok(row('A') !== a0 && row('A').textContent.includes('Lecture A (running)'), 'its row is redrawn');
  assert.ok(row('B') === b0 && row('C') === c0, 'the other rows stay as they are');
  assert.ok(listC.table.querySelector('.lt-group') === group0, 'and the group row');
  assert.ok(fk('lec:B:check').checked !== false && b0.className.includes('is-selected'), 'the selection stays');
  assert.ok(doc.activeElement === fk('lec:C:more'), 'the keyboard stays where it was');
  assert.equal(listC.setProgress({ scope_id: 'A', stage: 'prep', status: 'running', progress: 0.5 }), true, 'progress goes to the new row');
  // the keyboard in the row redrawn: on the same control in the new row
  fk('lec:A:more').focus();
  assert.equal(listC.updateRow(lecture('A', { seq: 1, title: 'Lecture A again' })), true);
  assert.ok(doc.activeElement === fk('lec:A:more') && fk('lec:A:more').isConnected, 'its control again, in the new row');
  // a selected lecture changed: the selection bar counts again (B can no longer be started)
  assert.ok(fk('batch:launch'), 'the bar offers "start" for B');
  assert.equal(listC.updateRow(lecture('B', { seq: 2, lecture_actions: [] })), true);
  assert.equal(fk('batch:launch'), null, 'the bar follows the redrawn lecture');
  // moved (another week: another group), or another place in the list: the
  // caller renders the whole list
  assert.equal(listC.updateRow(lecture('C', { seq: 3, recorded_at: '2026-10-05', numbered: true, week: 2 })), false, 'another group: not in place');
  assert.equal(listC.updateRow(lecture('C', { seq: 0 })), false, 'another place in the list: not in place');
  assert.equal(listC.updateRow(lecture('Z', { seq: 9 })), false, 'not in the list: not in place');
  // the course page changes its own array in place first (upsertLecture):
  // the list compares with what it drew, not with that array
  shownC = [lecture('A', { seq: 1 }), lecture('B', { seq: 2 }), lecture('C', { seq: 3 })];
  listC.render(shownC);
  const movedC = lecture('C', { seq: 3, recorded_at: '2026-10-05', numbered: true, week: 2 });
  shownC[2] = movedC;
  assert.equal(listC.updateRow(movedC), false, "the caller's array changed in place: still seen as moved");
  listC.table.remove();
  listC.bar.remove();
  body.focus();
}


// ---- R78 medium 1: a sentence edited (updateSentence) while a search is on ----
// is found or no longer found at once; the refresh after it reads the same
// data and redraws nothing, so it would not mend the hits

{
  const PT = await import(new URL('player_transcript.js', base).href);
  const tr = PT.createTranscript({ onSeek() {}, menuItems: () => null, duration: () => 10000 });
  body.append(tr.el);
  const search = tr.el.querySelector('input');
  search.value = '';
  const text = (i, w) => sentence(i, { text: w, asr_text: w });
  const list0 = [text(0, 'The cache is small.'), text(1, 'A page table.'), text(2, 'The cache line.'), text(3, 'Registers.')];
  tr.setSentences(list0);
  search.value = 'cache';
  search.dispatchEvent(event('keydown', { key: 'Enter' }));
  const hitIdx = () => tr.el.querySelectorAll('.pl-row').filter((r) => r.classList.contains('is-hit')).map((r) => r.dataset.idx);
  const count = () => tr.el.querySelector('.pl-search-count').textContent;
  assert.deepEqual(hitIdx(), ['0', '2']);
  // sentence 1 edited to hold the word, sentence 2 edited not to
  tr.updateSentence(text(1, 'A cache table.'));
  tr.updateSentence(text(2, 'The line.'));
  assert.deepEqual(hitIdx(), ['0', '1'], 'the edited sentences are found or not found now');
  assert.ok(!tr.el.querySelector('.pl-row[data-idx="2"]').querySelector('mark'), 'no mark left where the word went');
  assert.ok(count().includes('2'), `the count follows: ${count()}`);
  // the refresh with the same data keeps it so
  tr.setSentences([list0[0], text(1, 'A cache table.'), text(2, 'The line.'), list0[3]]);
  assert.deepEqual(hitIdx(), ['0', '1']);
  // a refresh that changes a sentence (another one's edit, a repair): the same
  tr.setSentences([list0[0], text(1, 'A cache table.'), text(2, 'The line.'), text(3, 'Cache registers.')]);
  assert.deepEqual(hitIdx(), ['0', '1', '3'], 'a sentence changed by a refresh is found');
  tr.el.remove();
  body.focus();
}


// ---- R78 low 1, low 2: the player's transcript after the chapters were
// made again, and when the length crosses an hour ----

El.prototype.insertBefore = function (n, ref) {
  n.remove();
  const i = ref ? this.children.indexOf(ref) : -1;
  n.parentNode = this;
  this.children.splice(i < 0 ? this.children.length : i, 0, n);
  return n;
};

{
  const PT = await import(new URL('player_transcript.js', base).href);
  let dur = 3599000; // 59:59
  const tr = PT.createTranscript({ onSeek() {}, menuItems: () => null, duration: () => dur });
  body.append(tr.el);
  tr.el.querySelector('input').value = '';
  const six = [0, 1, 2, 3, 4, 5].map((i) => sentence(i));
  tr.setSentences(six);
  const fns = { title: (c) => c.title, lang: () => 'en', onSeek() {} };
  const chap = (i) => ({ start_ms: six[i].start_ms, title: `Chapter at ${i}` });
  tr.setChapters([chap(0), chap(3)], fns);
  tr.el.querySelector('.pl-fold-all').click();
  const rowsOf = () => tr.el.querySelectorAll('.pl-row');
  assert.ok(rowsOf().every((r) => r.hidden), 'all folded');
  // made again: the first chapter now starts at sentence 2
  tr.setChapters([chap(2), chap(4)], fns);
  assert.deepEqual(
    rowsOf().filter((r) => r.hidden).map((r) => r.dataset.idx),
    [],
    'the sentences before the first chapter are shown (no heading folds them)',
  );
  tr.setChapters([], fns);

  // the times: "00:05" while the length is under an hour, "0:00:05" from an
  // hour on, the whole column alike
  const time = (i) => rowsOf()[i].querySelector('.pl-row-time').textContent;
  assert.equal(time(1), '00:01');
  dur = 3601000;
  const next = six.map((s) => ({ ...s }));
  next[3] = { ...next[3], zh: '新的。' };
  tr.setSentences(next);
  assert.deepEqual([0, 1, 3, 5].map(time), ['0:00:00', '0:00:01', '0:00:03', '0:00:05'], 'one format for the whole column');
  // the length known anew (the media's own) before an edit comes back: the same
  dur = 3599000;
  tr.updateSentence({ ...next[2], zh: '又一句。' });
  assert.deepEqual([0, 2, 5].map(time), ['00:00', '00:02', '00:05'], 'an edit back after the length changed: one format');
  tr.el.remove();
  body.focus();
}


// ---- R78 low 3, low 5: the course list's in-place row update ----

El.prototype.closest = function (sel) {
  const parts = sel.split(',').map((x) => x.trim());
  return up(this, (n) => n instanceof El && parts.some((p) => n.matches(p)));
};

{
  let filtered = false;
  let shownD = [];
  const listD = L.createLectureList({ ctx, onLectures() {}, onDeleted() {}, course: () => null, filtered: () => filtered });
  body.append(listD.table, listD.bar);
  const wk = (id, seq, o = {}) => lecture(id, { seq, recorded_at: '2026-10-05', numbered: true, week: 2, ...o });
  shownD = [wk('A', 1), wk('B', 2), lecture('C', { seq: 3 })];
  listD.render(shownD);
  // C1: a slide file bound to the week shows in the week's group row
  const slide = { id: 'M1', filename: 'week2.pdf', via: 'week' };
  assert.equal(listD.updateRow(wk('A', 1, { materials: [slide] })), false, "the week's slide files changed: the group row too, the whole list");
  assert.equal(listD.updateRow(wk('A', 1, { materials: [{ ...slide, via: 'course' }] })), true, 'a course-wide file is not in the group row: in place');
  // C6: a search narrows the list: the caller's filter decides what shows
  filtered = true;
  assert.equal(listD.updateRow(wk('A', 1, { title: 'Found?' })), false, 'a search is on: the whole list (the lecture may show or not now)');
  filtered = false;
  // C5: a rename in progress in the list
  listD.render(shownD);
  const rowB = listD.table.querySelectorAll('.lrow').find((r) => r.dataset.id === 'B');
  rowB.dispatchEvent(event('keydown', { key: 'F2', target: rowB }));
  assert.ok(listD.table.querySelector('.lrow-title-input'), 'B is being renamed');
  assert.equal(listD.updateRow(lecture('C', { seq: 3, title: 'C changed' })), false, 'a rename is on: not in place');
  listD.table.querySelector('.lrow-title-input').dispatchEvent(event('keydown', { key: 'Escape' }));
  await settle(5);

  // low 3: texts that change with time ("running for …", "uploaded … ago")
  // in rows not redrawn go on (refreshTimes, the course page's minute tick)
  const t0 = new Date(clock).toISOString();
  shownD = [lecture('R', { seq: 1, stages: names.map((n) => stage(n, n === 'prep' ? 'running' : 'queued', n === 'prep' ? { started_at: t0 } : {})) }), lecture('D', { seq: 2, draft: true, created_at: t0, lecture_actions: ['launch'] })];
  listD.render(shownD);
  const rowOf = (id) => listD.table.querySelectorAll('.lrow').find((r) => r.dataset.id === id);
  const status = () => rowOf('R').querySelector('.lrow-status').textContent;
  const meta = () => rowOf('D').querySelector('.lrow-meta').textContent;
  const before = [status(), meta()];
  const rows0 = [rowOf('R'), rowOf('D')];
  clock += 5 * 60 * 1000;
  listD.refreshTimes();
  assert.ok(status() !== before[0], `the running time goes on: ${before[0]} → ${status()}`);
  assert.ok(meta() !== before[1], `"uploaded … ago" goes on: ${before[1]} → ${meta()}`);
  assert.ok(rowOf('R') === rows0[0] && rowOf('D') === rows0[1], 'without redrawing the rows');
  listD.table.remove();
  listD.bar.remove();
  body.focus();
}


// ---- R74 medium 3: after an edited sentence the translation's stats are
// the incremental run's (a few sentences): not shown as the last cost ----

{
  const C = await import(new URL('update_confirm.js', base).href);
  const usage = (cost) => ({ prompt_tokens: 1000, cache_hit_tokens: 0, completion_tokens: 100, estimated_cost: cost });
  const inc = { code: 'translate.incremental' };
  const lec = (noteInfo) =>
    lecture('I', {
      lecture_actions: ['update'],
      stale: { reasons: [{ code: 'glossary_updated', params: { from: 1, to: 2 } }], stages: ['repair', 'translate'] },
      stages: names.map((n) =>
        n === 'repair' ? stage(n, 'stale', { duration_ms: 60000, stats: usage(0.2) }) : n === 'translate' ? stage(n, 'stale', { duration_ms: 2000, stats: usage(0.003), note_info: noteInfo }) : stage(n, 'done'),
      ),
    });
  const textOf = async (l) => {
    const asked = C.confirmUpdate([l]);
    const dlg = openOnes().at(-1);
    const s = dlg.textContent;
    dlg.dispatchEvent(event('cancel'));
    await asked;
    return s;
  };
  let s = await textOf(lec(inc));
  assert.ok(s.includes(I.t('course.confirm.last_partial')), 'the last translation was incremental: said so');
  assert.ok(!s.includes(I.t('common.usage.cost', { cost: U.fmtCost(0.203) })), 'and no cost that stands for the whole lecture');
  // the incremental note among others (params.extra)
  s = await textOf(lec({ code: 'budget.unpriced', params: { n: 2, extra: [inc] } }));
  assert.ok(s.includes(I.t('course.confirm.last_partial')), 'found among the notes too');
  // a whole run: the numbers, as before
  s = await textOf(lec(null));
  assert.ok(!s.includes(I.t('course.confirm.last_partial')) && s.includes(U.usageText({ prompt_tokens: 2000, cache_hit_tokens: 0, completion_tokens: 200, estimated_cost: 0.203 })), 'a whole run: its numbers');
  body.focus();
}


// ---- R74 medium 4: every batch operation of the course page asks first ----

{
  const C = await import(new URL('update_confirm.js', base).href);
  const M = await import(new URL('menu.js', base).href);
  const okOf = (d) => d.querySelectorAll('button').find((b) => /btn-primary/.test(b.className));
  const cancelOf = (d) => d.querySelectorAll('button').find((b) => b.className === 'btn');
  // the course's "update all" while its list is not read: the course's count
  let asked = C.confirmUpdate([], { named: true, count: 3 });
  let dlg = openOnes().at(-1);
  assert.ok(dlg && dlg.textContent.includes(I.t('course.confirm.update_title_many', { n: 3 })), 'no list: it still asks, with the count');
  assert.ok(dlg.textContent.includes(I.t('course.confirm.update_unknown')));
  assert.ok(cancelOf(dlg).getAttribute('autofocus') != null, 'the focus starts on cancel');
  dlg.dispatchEvent(event('cancel'));
  assert.equal(await asked, false);
  assert.equal(await C.confirmUpdate([], { count: 0 }), true, 'nothing to update anywhere: nothing to ask');
  // "start all drafts": the focus starts on cancel too (it costs money)
  asked = C.confirmLaunchDrafts([], 2);
  dlg = openOnes().at(-1);
  assert.ok(cancelOf(dlg).getAttribute('autofocus') != null && okOf(dlg).getAttribute('autofocus') == null, '"start all drafts": the focus starts on cancel');
  dlg.dispatchEvent(event('cancel'));
  await asked;

  // the selection: its "start" (bar and menu) and its menu's "update"
  const usage = { prompt_tokens: 10, cache_hit_tokens: 0, completion_tokens: 1, estimated_cost: 0.01 };
  const staleOne = (id) => lecture(id, { lecture_actions: ['update'], stale: { reasons: [], stages: ['translate'] }, stages: names.map((n) => stage(n, n === 'translate' ? 'stale' : 'done', { stats: usage })) });
  const draft = (id) => lecture(id, { draft: true, lecture_actions: ['launch'] });
  let shownE = [draft('D1'), draft('D2'), staleOne('S1'), staleOne('S2')];
  const listE = L.createLectureList({ ctx, onLectures() {}, onDeleted() {}, course: () => null, filtered: () => false });
  body.append(listE.table, listE.bar);
  listE.render(shownE);
  const select = (ids) => {
    for (const id of ids) {
      const box = fk(`lec:${id}:check`);
      box.checked = true;
      box.click();
    }
  };
  answer = () => ({ status: 200, body: { results: [] } });
  select(['D1', 'D2']);
  posts.length = 0;
  fk('batch:launch').click();
  await settle();
  dlg = openOnes().at(-1);
  assert.ok(dlg && dlg.textContent.includes(I.t('course.confirm.launch_title', { n: 2 })), '"start selected" asks as "start all drafts" does');
  dlg.dispatchEvent(event('cancel'));
  await settle();
  assert.deepEqual(posts, [], 'cancelled: nothing started');
  // its menu (right click on a selected row)
  const pickMenu = async (id, label) => {
    listE.openMenuFor(shownE.find((l) => l.id === id), fk(`lec:${id}:more`));
    await settle(10);
    const item = doc.querySelectorAll('.ctx-item').find((b) => b.textContent.includes(label));
    assert.ok(item, `the selection's menu has "${label}"`);
    item.click();
    await settle(20);
  };
  await pickMenu('D1', I.t('course.lectures.batch.launch_n', { n: 2 }));
  dlg = openOnes().at(-1);
  assert.ok(dlg && dlg.textContent.includes(I.t('course.confirm.launch_title', { n: 2 })), "the menu's start asks too");
  dlg.dispatchEvent(event('cancel'));
  await settle();
  fk('batch:clear').click();
  select(['S1', 'S2']);
  await pickMenu('S1', I.t('course.lectures.batch.update_n', { n: 2 }));
  dlg = openOnes().at(-1);
  assert.ok(dlg && dlg.textContent.includes(I.t('course.confirm.update_title_many', { n: 2 })), "the selection menu's update asks");
  dlg.dispatchEvent(event('cancel'));
  await settle();
  assert.deepEqual(posts, [], 'cancelled: nothing sent');
  M.closeMenu({ focus: false });
  fk('batch:clear').click();
  answer = () => ({ status: 200, body: {} });
  listE.table.remove();
  listE.bar.remove();
  body.focus();
}


// ---- R74 low 2, low 3: the dialogs name the lectures they act on ----

{
  const C = await import(new URL('update_confirm.js', base).href);
  // "start all drafts" with one draft: which one
  let asked = C.confirmLaunchDrafts([lecture('D9', { draft: true, lecture_actions: ['launch'], title: 'The only draft' })], 1);
  let dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes('The only draft'), 'one draft: named');
  dlg.dispatchEvent(event('cancel'));
  await asked;
  // the selection's "update" with one of two selected lectures to update: which one
  const usage = { prompt_tokens: 10, cache_hit_tokens: 0, completion_tokens: 1, estimated_cost: 0.01 };
  const staleOne = (id) => lecture(id, { title: `Stale ${id}`, lecture_actions: ['update'], stale: { reasons: [], stages: ['translate'] }, stages: names.map((n) => stage(n, n === 'translate' ? 'stale' : 'done', { stats: usage })) });
  const listF = L.createLectureList({ ctx, onLectures() {}, onDeleted() {}, course: () => null, filtered: () => false });
  body.append(listF.table, listF.bar);
  listF.render([staleOne('S7'), lecture('P7', { title: 'Plain P7' })]);
  for (const id of ['S7', 'P7']) {
    const box = fk(`lec:${id}:check`);
    box.checked = true;
    box.click();
  }
  fk('batch:update').click();
  await settle();
  dlg = openOnes().at(-1);
  assert.ok(dlg && dlg.textContent.includes(I.t('course.confirm.update_title')) && dlg.textContent.includes('Stale S7'), 'one of the selection to update: named');
  dlg.dispatchEvent(event('cancel'));
  await settle();
  fk('batch:clear').click();
  listF.table.remove();
  listF.bar.remove();
  body.focus();
}


// ---- R74 low 6, low 8: the course settings ----

{
  const Cal = await import(new URL('calendar_ui.js', base).href);
  // the header's "term start" link opens the settings on the term start
  const course = { id: 'C2', name: 'Course', code: '', term_start: '', skip_weeks: [], numbering: 'auto' };
  let opening = Cal.openCourseSettings(course, { lectures: [], focus: 'term' });
  let dlg = openOnes().at(-1);
  assert.ok(dlg.querySelector('[data-fkey="cset:term"]').getAttribute('autofocus') != null, '"term start": the focus starts on it');
  assert.ok(dlg.querySelector('input[data-fkey="cset:name"]').getAttribute('autofocus') == null);
  dlg.dispatchEvent(event('cancel'));
  await opening;
  // a course made before codes could not hold spaces: its settings can
  // still be saved while the code is left as it is
  answer = () => ({ status: 200, body: { ...course, code: 'OLD CODE', numbering: 'never' } });
  posts.length = 0;
  bodies.length = 0;
  opening = Cal.openCourseSettings({ ...course, code: 'OLD CODE' }, { lectures: [] });
  dlg = openOnes().at(-1);
  dlg.querySelector('input[data-fkey="cset:num:never"]').dispatchEvent(event('change'));
  const save = dlg.querySelector('button[data-fkey="cset:save"]');
  assert.ok(!save.disabled, 'an old code with a space, left as it is: the rest can be saved');
  input(dlg.querySelector('input[data-fkey="cset:code"]'), 'NEW CODE');
  assert.ok(save.disabled, 'changed to another code with a space: not sent');
  input(dlg.querySelector('input[data-fkey="cset:code"]'), 'OLD CODE');
  dlg.querySelector('form.cset').dispatchEvent(event('submit'));
  await settle();
  assert.deepEqual(JSON.parse(bodies[0]), { numbering: 'never' }, 'the code is not sent');
  await opening;
  answer = () => ({ status: 200, body: {} });
  body.focus();
}


// ---- R78b medium 2: while the lecture's translation is held (stale,
// failed, cancelled) an edit or "translate again" is translated only when
// the translation runs again (sched processIncremental drops the request);
// the edit box, the toasts and the notice say so, and no "is translated
// again" is promised ----

{
  const T = await import(new URL('view_transcript.js', base).href);
  const RW = await import(new URL('retranslate_watch.js', base).href);
  const promises = ['transcript.edit.hint', 'player.edit.zh_note', 'player.edit.saved', 'player.menu.retranslate_done', 'player.menu.reset_done'].map((k) => I.t(k));
  const run = async (status) => {
    let rows = [sentence(0), sentence(1, { human_text: 'Sentence 1.' })];
    answer = (m, u) => {
      if (m === 'GET' && u.includes('/api/lectures/H/sentences')) return { status: 200, body: { total: rows.length, sentences: rows } };
      if (m === 'POST' && /\/sentences\/1\/(retranslate|reset)$/.test(u)) {
        rows = [rows[0], { ...rows[1], zh_status: 'stale' }];
        return { status: 200, body: rows[1] };
      }
      if (m === 'POST' && u.endsWith('/sentences/1/edit')) {
        rows = [rows[0], { ...rows[1], text: 'Edited.', human_text: 'Edited.', zh_status: 'stale' }];
        return { status: 200, body: rows[1] };
      }
      return { status: 404, body: {} };
    };
    const host = D.h('div');
    body.append(host);
    const lec = { ...lecture('H'), reviewable: true, counts: { sentences: 2, patches_pending: 0 }, stages: names.map((n) => stage(n, n === 'translate' ? status : n === 'export' ? 'pending' : 'done')) };
    const view = T.mountTranscript(host, lec, { courses: () => [], system: () => null, resources: () => null });
    await settle(60);
    told();
    const out = {};
    await pick('more:1', I.t('player.menu.edit'));
    out.box = host.querySelector('.s-edit').textContent;
    const ta = host.querySelector('textarea');
    ta.value = 'Edited.';
    ta.dispatchEvent(event('input'));
    ta.dispatchEvent(event('keydown', { key: 'Enter' }));
    await settle(40);
    out.saved = told();
    rows = [rows[0], { ...rows[1], zh_status: 'ok' }];
    view.onChanged({ kind: 'sentences', id: 'H' });
    await settle(500);
    await pick('more:1', I.t('player.menu.retranslate'));
    out.retranslated = told();
    await pick('more:1', I.t('player.menu.reset'));
    out.reset = told();
    view.destroy();
    host.remove();
    return out;
  };
  // held: what it says, and that it promises nothing
  for (const status of ['stale', 'failed', 'cancelled']) {
    const o = await run(status);
    assert.ok(o.box.includes(I.t('transcript.edit.hint_held')) && o.box.includes(I.t('player.edit.zh_note_held')), `${status}: the edit box says the translation waits`);
    // round 9 WV-01: held, an info toast that stays (r9_views_lecture_test.mjs)
    assert.deepEqual(o.saved, [`info: ${I.t('player.edit.saved_held')}`], `${status}: saved, translated only when the translation runs again`);
    assert.deepEqual(o.retranslated, [`info: ${I.t('player.menu.retranslate_held')}`], `${status}: marked, translated only then`);
    assert.deepEqual(o.reset, [`info: ${I.t('player.menu.reset_done_held')}`], `${status}: restored, the same`);
    const said = [o.box, ...o.saved, ...o.retranslated, ...o.reset].join('\n');
    for (const p of promises) assert.ok(!said.includes(p), `${status}: no promise of a translation now: ${p}`);
  }
  // not held (done, or running: the request waits for that run): as before
  for (const status of ['done', 'running']) {
    const o = await run(status);
    assert.ok(o.box.includes(I.t('transcript.edit.hint')) && o.box.includes(I.t('player.edit.zh_note')), `${status}: the edit box as before`);
    assert.deepEqual(o.saved, [`success: ${I.t('player.edit.saved')}`]);
    assert.deepEqual(o.retranslated, [`success: ${I.t('player.menu.retranslate_done')}`]);
    assert.deepEqual(o.reset, [`success: ${I.t('player.menu.reset_done')}`]);
  }
  assert.equal(RW.translationHeld({ stages: [stage('translate', 'stale')] }), true);
  assert.equal(RW.translationHeld({ stages: [stage('translate', 'queued')] }), false, 'queued: the request waits for that run');
  answer = () => ({ status: 200, body: {} });
  body.focus();
}

console.log('r7 wc checks: ok');
