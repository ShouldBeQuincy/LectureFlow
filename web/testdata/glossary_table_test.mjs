// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// R7-PF1 (P2 hotspot 1): the course page's glossary table (glossary.js
// createGlossary) is kept up to date instead of being built anew. Editing
// one term's rendering used to build the whole table four times (start,
// save, the course read again, the server's "changed terms"); now it draws
// that term's row twice (the editor, then the saved row) and nothing else.
// A reload or a course that changes nothing draws nothing; the filter takes
// rows out and puts the same rows back. What must not change is pinned
// beside it: the focus (and the editor's text and caret), the keys, the
// groups and their counts, the filter's results and empty states, the
// server's refusals. web/glossary_table_test.go starts it with the web
// directory as a file: URL. Nodes are compared by identity only
// (assert.ok(a === b)); every wait is bounded.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const base = process.argv[2];

// ---- DOM stand-in (the parts glossary.js, dom.js and menu.js use) ----

const MAX_DEPTH = 10000;
function up(n, fn) {
  for (let i = 0; n; n = n.parentNode) {
    if (++i > MAX_DEPTH) throw new Error('stand-in DOM: parent chain too long (a cycle?)');
    if (fn(n)) return n;
  }
  return null;
}

let focusBy = 'keyboard';
let rowRemovals = 0; // term rows taken off a parent (removed, or moved: insertBefore takes them off first)

class Node {
  constructor() {
    this.parentNode = null;
    this.listeners = {};
  }
  get isConnected() {
    return !!up(this, (n) => n === root);
  }
  get nextSibling() {
    const p = this.parentNode;
    return p ? p.children[p.children.indexOf(this) + 1] || null : null;
  }
  // As Chromium does (R71 M1): a focused element taken off the page,
  // removed or moved elsewhere, first gets its blur and focusout while it is
  // still on the page (the focus is on the page's body by then); a handler
  // that moved the node meanwhile makes the removal throw (NotFoundError).
  remove() {
    const p = this.parentNode;
    if (!p) return;
    const a = doc.activeElement;
    if (a !== body && this.contains && this.contains(a)) {
      doc.activeElement = body;
      a.dispatchEvent(event('blur', { bubbles: false }));
      a.dispatchEvent(event('focusout', { relatedTarget: null }));
      if (this.parentNode !== p) throw new Error("NotFoundError: the node to be removed is no longer a child of this node (moved in a 'blur' event handler)");
    }
    const c = p.children;
    c.splice(c.indexOf(this), 1);
    this.parentNode = null;
    if (this.tagName === 'TR' && this.dataset.term) rowRemovals++;
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  removeEventListener(type, fn) {
    const l = this.listeners[type] || [];
    if (l.includes(fn)) l.splice(l.indexOf(fn), 1);
  }
  // events bubble up to the document
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
    this.hidden = false;
    const self = this;
    this.classList = {
      add(c) {
        if (!this.contains(c)) self.className = (self.className + ' ' + c).trim();
      },
      remove(...cs) {
        self.className = self.className
          .split(/\s+/)
          .filter((x) => x && !cs.includes(x))
          .join(' ');
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
  get lastChild() {
    return this.children[this.children.length - 1] || null;
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
  hasAttribute(k) {
    return k in this.attrs;
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
    if (!ref) {
      this.append(n);
      return n;
    }
    if (ref.parentNode !== this) throw new Error('stand-in DOM: insertBefore with a reference node of another parent');
    n.remove();
    n.parentNode = this;
    this.children.splice(this.children.indexOf(ref), 0, n);
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
    const prev = doc.activeElement;
    if (prev === this) return;
    doc.activeElement = this;
    if (prev && prev !== body) {
      prev.dispatchEvent(event('blur', { bubbles: false }));
      prev.dispatchEvent(event('focusout', { relatedTarget: this }));
    }
    this.dispatchEvent(event('focus', { bubbles: false }));
    this.dispatchEvent(event('focusin', { relatedTarget: prev }));
  }
  blur() {
    if (doc.activeElement !== this) return;
    doc.activeElement = body;
    this.dispatchEvent(event('blur', { bubbles: false }));
    this.dispatchEvent(event('focusout', { relatedTarget: null }));
  }
  click() {
    this.dispatchEvent(event('click'));
  }
  select() {
    this.selectionStart = 0;
    this.selectionEnd = String(this.value ?? '').length;
  }
  setSelectionRange(a, b) {
    this.selectionStart = a;
    this.selectionEnd = b;
  }
  showModal() {
    this.open = true;
  }
  close() {
    this.open = false;
  }
  getBoundingClientRect() {
    return { top: 0, bottom: 0, left: 0, right: 0, width: 0, height: 0 };
  }
  // tag, .class and [attr="v"] selectors, lists of them (a, b), and
  // :focus-visible (how the focus came)
  matches(sel) {
    if (sel.includes(',')) return sel.split(',').some((s) => this.matches(s.trim()));
    if (sel === ':focus-visible') return focusBy === 'keyboard';
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
  closest(sel) {
    return up(this, (n) => n instanceof El && n.matches(sel));
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

const root = new El('html');
const body = new El('body');
root.append(body);
const toasts = new El('div');
toasts.setAttribute('id', 'toasts');
body.append(toasts);
const built = []; // the <tr> elements built since the last reset
// the term rows among them (not a group's header or placeholder row)
const rowsBuilt = () => built.filter((r) => r.dataset.term).length;
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
  getElementById: (id) => root.querySelector(`[id="${id}"]`),
  createElement: (tag) => {
    const el = new El(tag);
    if (el.tagName === 'TR') built.push(el);
    return el;
  },
  createElementNS: (_, tag) => new El(tag),
  createTextNode: (s) => new Text(s),
  visibilityState: 'visible',
};
const store = { getItem: () => null, setItem() {}, removeItem() {} };
Object.assign(globalThis, {
  Node,
  location: { search: '', hash: '#/course/C1', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: doc,
  addEventListener() {},
  removeEventListener() {},
  dispatchEvent: () => true,
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  innerHeight: 900,
  innerWidth: 1440,
  CSS: { escape: (s) => String(s) },
});
globalThis.window = globalThis;
globalThis.requestAnimationFrame = (fn) => setTimeout(fn, 0);
const settle = (ms = 30) => new Promise((r) => setTimeout(r, ms));

// ---- the server ----

const WORDS = ['adder', 'buffer', 'cache', 'cache line', 'cipher', 'commit', 'disk', 'entropy', 'gradient', 'hash', 'index', 'kernel', 'latency', 'lock', 'matrix', 'node', 'operator', 'packet', 'page table', 'parser', 'pipeline', 'pointer', 'process', 'query', 'register', 'scheduler', 'schema', 'thread', 'token', 'vector'];
const SOURCES = ['syllabus', 'material', 'review', 'manual'];
const id = (i) => `T${String(i).padStart(2, '0')}`;
const server = {
  terms: WORDS.map((en, i) => ({
    id: id(i),
    en,
    zh: `译${i}`,
    source: SOURCES[i % 4],
    enabled: i % 7 !== 6,
    freq: 40 - i,
    tier: i % 3 === 2 ? 'reference' : 'enforce',
    locked: i % 5 === 0,
    common_word: false,
    janitor_note: i % 9 === 4 ? 'Renamed by the janitor.' : '',
    zh_source: 'extractor',
  })),
  refuse: null, // {status, body}: the next PATCH answers this
};
const clone = (o) => JSON.parse(JSON.stringify(o));
const sent = []; // "METHOD path body"
function answer(method, path, bodyText) {
  const b = bodyText ? JSON.parse(bodyText) : null;
  if (method === 'GET' && path === '/api/courses/C1/terms') return { status: 200, body: clone(server.terms) };
  let m = /^\/api\/terms\/(\w+)$/.exec(path);
  if (m && method === 'PATCH') {
    if (server.refuse) {
      const r = server.refuse;
      server.refuse = null;
      return r;
    }
    const term = server.terms.find((x) => x.id === m[1]);
    if (b.locked === false) term.locked = false;
    if (b.zh !== undefined && b.zh !== term.zh) Object.assign(term, { zh: b.zh, zh_source: 'user', locked: true });
    if (b.enabled !== undefined) Object.assign(term, { enabled: b.enabled, locked: true });
    if (b.tier !== undefined) Object.assign(term, { tier: b.tier, locked: true });
    return { status: 200, body: clone(term) };
  }
  if (m && method === 'DELETE') {
    server.terms = server.terms.filter((x) => x.id !== m[1]);
    return { status: 204, body: null };
  }
  if (method === 'POST' && path === '/api/courses/C1/terms') {
    const term = { id: 'TNEW', en: b.en, zh: b.zh, source: 'manual', enabled: true, freq: 0, tier: 'enforce', locked: true, common_word: false, janitor_note: '', zh_source: 'user' };
    server.terms.push(term);
    server.terms.sort((x, y) => (x.en < y.en ? -1 : 1));
    return { status: 201, body: clone(term) };
  }
  return { status: 404, body: { error: 'not here' } };
}
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const cat = JSON.parse(await readFile(new URL(u, base), 'utf8'));
    const text = JSON.stringify(cat);
    return { ok: true, status: 200, json: async () => cat, text: async () => text };
  }
  const method = init.method || 'GET';
  const path = u.replace(/\?.*$/, '');
  if (method !== 'GET') sent.push(`${method} ${path} ${init.body || ''}`.trim());
  await new Promise((r) => setTimeout(r, 3));
  const a = answer(method, path, init.body);
  const text = a.status === 204 ? '' : JSON.stringify(a.body);
  return { ok: a.status < 300, status: a.status, text: async () => text };
};

const I = await import(new URL('i18n.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;
const A = await import(new URL('api.js', base).href);
const G = await import(new URL('glossary.js', base).href);
// as the app: a failed request is told (app.js toasts it)
const told = [];
A.setHandlers({ onError: (err) => told.push(err.status) });

// ---- the glossary of a course page ----

let version = 3;
const course = () => ({ id: 'C1', name: 'Systems', term_count: server.terms.length, glossary_version: version, stages: [], materials: [], janitor: { configured: true } });
let courseReads = 0;
const gl = G.createGlossary({
  courseId: 'C1',
  ctx: { system: () => null, resources: () => null },
  onCourse() {},
  // as the course page: the course read again, then handed to the glossary
  reloadCourse: async () => {
    courseReads++;
    await settle(3);
    version++;
    gl.setCourse(course());
  },
});
body.append(gl.el);
gl.setCourse(course());
const wrap = gl.el.querySelector('.terms-table-wrap');
const foot = gl.el.querySelector('.terms-foot');
const filterInput = gl.el.querySelector('.term-search');
const sourceSel = gl.el.querySelector('.term-source');
const rowOf = (tid) => wrap.querySelector(`tr[data-term="${tid}"]`);
const zhBtn = (tid) => rowOf(tid).querySelector('.zh-btn');
const shownIds = () => wrap.querySelectorAll('tr[data-term]').map((r) => r.dataset.term);
const snapshot = () => new Map(wrap.querySelectorAll('tr[data-term]').map((r) => [r.dataset.term, r]));
// every row of snap still on the page is the same element, but those of except
function sameRows(snap, except, what) {
  let n = 0;
  for (const [tid, r] of snap) {
    if (except.includes(tid)) continue;
    const now = rowOf(tid);
    if (!now) continue;
    assert.ok(now === r, `${what}: the row of ${tid} was drawn anew`);
    n++;
  }
  return n;
}
const groupCount = (g) => wrap.querySelector(`tbody[data-group="${g}"]`).querySelector('.tg-count').textContent;
const groupIds = (g) => wrap.querySelector(`tbody[data-group="${g}"]`).querySelectorAll('tr[data-term]').map((r) => r.dataset.term);
const inGroup = (g) => server.terms.filter((x) => (g === 'off' ? !x.enabled : x.enabled && (g === 'reference' ? x.tier === 'reference' : x.tier !== 'reference')));
// the order of the table: the groups in turn, the server's order within each
const tableIds = () => ['enforce', 'reference', 'off'].flatMap((g) => inGroup(g).map((x) => x.id));
const key = (target, k, o = {}) => target.dispatchEvent(event('keydown', { key: k, ...o }));
const active = () => doc.activeElement;

// ---- loading: every term, in its group, in the server's order ----

await gl.reload();
await settle();
assert.equal(shownIds().join(' '), tableIds().join(' '), 'every term is shown, in order');
for (const g of ['enforce', 'reference', 'off']) {
  assert.equal(groupIds(g).join(' '), inGroup(g).map((x) => x.id).join(' '), `the ${g} group`);
  assert.equal(groupCount(g), String(inGroup(g).length), `the ${g} count`);
}
assert.equal(foot.querySelector('span').textContent, t('glossary.foot.total', { n: 30 }));

// ---- a reload that brings the same list, a course read again: nothing is drawn ----

{
  const snap = snapshot();
  built.length = 0;
  await gl.reload();
  await settle();
  assert.equal(rowsBuilt(), 0, 'the same list again: no row is built');
  assert.equal(sameRows(snap, [], 'the same list again'), 30);
  gl.setCourse({ ...course(), glossary_version: 99 });
  assert.equal(rowsBuilt(), 0, 'the course read again: no row is built');
  sameRows(snap, [], 'the course read again');
}

// ---- any field a row shows changed: that row is drawn anew, no other (R71 L2) ----
// (whatever decides when a row is reused must see each of these)

{
  const term = (tid) => server.terms.find((x) => x.id === tid);
  const cases = [
    ['en', 'T22', { en: 'processes' }, () => rowOf('T22').querySelector('.term-en').textContent === 'processes'],
    ['zh', 'T22', { zh: '进程们' }, () => zhBtn('T22').textContent === '进程们'],
    ['source', 'T22', { source: 'syllabus' }, () => rowOf('T22').querySelector('.src').className.includes('src-syllabus')],
    ['zh_source', 'T22', { zh_source: 'janitor' }, () => rowOf('T22').querySelector('.zh-src').className.includes('zh-src-janitor')],
    ['freq', 'T22', { freq: 123 }, () => rowOf('T22').querySelector('td.num').textContent === '123'],
    ['locked', 'T25', { locked: false }, () => !rowOf('T25').querySelector('.mk-lock')],
    ['janitor_note', 'T22', { janitor_note: 'Merged by the janitor.' }, () => !!rowOf('T22').querySelector('.mk-janitor')],
    ['enabled', 'T22', { enabled: false }, () => rowOf('T22').classList.contains('term-disabled') && groupIds('off').includes('T22')],
    ['tier (enabled)', 'T22', { tier: 'reference' }, () => groupIds('reference').includes('T22')],
    ['tier (disabled)', 'T27', { tier: 'reference' }, () => rowOf('T27').querySelector('.tier-tag').textContent.includes(I.t('glossary.tier.reference'))],
  ];
  for (const [field, tid, change, shows] of cases) {
    const was = clone(term(tid));
    const snap = snapshot();
    built.length = 0;
    rowRemovals = 0;
    Object.assign(term(tid), change);
    await gl.reload();
    await settle();
    assert.ok(rowOf(tid) !== snap.get(tid), `${field} changed: the row is drawn anew`);
    assert.ok(shows(), `${field} changed: the row shows it`);
    assert.equal(rowsBuilt(), 1, `${field} changed: that row only`);
    assert.equal(rowRemovals, 1, `${field} changed: no other row taken off the page`);
    sameRows(snap, [tid], `${field} changed`);
    Object.assign(term(tid), was);
    await gl.reload();
    await settle();
    assert.equal(shownIds().join(' '), tableIds().join(' '), `${field} changed back`);
  }
}

// ---- editing one rendering: its row only, twice; the focus and the text stay ----

{
  const snap = snapshot();
  built.length = 0;
  courseReads = 0;
  rowRemovals = 0;
  zhBtn('T03').focus();
  zhBtn('T03').click();
  assert.equal(rowsBuilt(), 1, 'the editor: one row built');
  assert.equal(rowRemovals, 1, 'the editor: no other row taken off the page');
  sameRows(snap, ['T03'], 'the editor opened');
  let input = rowOf('T03').querySelector('.input-term');
  assert.ok(input && active() === input, 'the editor has the focus');
  assert.equal(input.value, '译3');
  assert.equal(input.selectionStart, 0, 'its text is selected');
  assert.equal(input.selectionEnd, 2);
  assert.equal(rowOf('T03').getAttribute('draggable'), 'false', 'a row being edited is not dragged');

  // typing draws nothing
  input.value = '译3改';
  input.setSelectionRange(2, 2);
  input.dispatchEvent(event('input'));
  assert.equal(rowsBuilt(), 1);

  // the server changed another term meanwhile: that row only; the editor stays as it is
  server.terms[10].zh = '索引';
  const before10 = rowOf('T10');
  let blurs = 0;
  input.addEventListener('blur', () => blurs++);
  sent.length = 0;
  await gl.reload();
  await settle();
  assert.equal(rowsBuilt(), 2, 'another term changed: its row only');
  assert.equal(rowRemovals, 2, 'another term changed: no other row taken off the page');
  assert.ok(rowOf('T10') !== before10 && zhBtn('T10').textContent === '索引', 'the changed row shows the change');
  sameRows(snap, ['T03', 'T10'], 'another term changed');
  assert.ok(rowOf('T03').querySelector('.input-term') === input, 'the editor is the same input');
  assert.ok(active() === input, 'it keeps the focus');
  assert.equal(input.value, '译3改', 'and the text typed');
  assert.equal(input.selectionStart, 2, 'and the caret');
  assert.equal(blurs, 0, 'the editor was not taken off the page');

  // the server changed the term being edited: its row is drawn anew with the
  // text typed so far, the focus and the caret; the old editor's blur (it
  // comes while it is taken off, R71 M1) neither saves nor closes it
  server.terms[3].freq = 77;
  await gl.reload();
  await settle();
  assert.equal(sent.length, 0, 'the term being edited changed: nothing is sent');
  assert.equal(rowsBuilt(), 3, 'the term being edited changed: its row only');
  const input2 = rowOf('T03').querySelector('.input-term');
  assert.ok(input2 && input2 !== input, 'the editor is drawn anew');
  assert.ok(active() === input2, 'the new editor has the focus');
  assert.equal(input2.value, '译3改', 'the text typed is kept');
  assert.equal(input2.selectionStart, 2, 'the caret is kept');
  assert.equal(input2.selectionEnd, 2);
  input = input2;

  // Enter saves: the PATCH, the saved row, the focus on its rendering
  sent.length = 0;
  built.length = 0;
  key(input, 'Enter');
  await settle(60);
  assert.deepEqual(sent, ['PATCH /api/terms/T03 {"zh":"译3改"}']);
  assert.equal(rowsBuilt(), 1, 'saved: its row only (the course read again draws nothing)');
  assert.equal(courseReads, 1, 'the course is read again (glossary_version)');
  assert.equal(zhBtn('T03').textContent, '译3改');
  assert.ok(rowOf('T03').querySelector('.mk-lock'), 'the saved term shows it is locked now');
  assert.ok(active() === zhBtn('T03'), 'the focus is on the saved rendering');
  // the server's "changed terms" that follows: the same list, nothing drawn
  await gl.reload();
  await settle();
  assert.equal(rowsBuilt(), 1, 'the echo of the change draws nothing');
  sameRows(snap, ['T03', 'T10'], 'after the save');
}

// ---- the row being edited moves, is filtered out, is deleted (R71 M1) ----
// Moved: the same editor, its text, focus and caret, nothing sent. Not shown
// any more (filtered out, deleted on the server): the edit ends with it,
// nothing typed in it is saved, and no editor comes back with the row.

{
  // moved: the server's order changed (the term renamed by someone else
  // would also move it; here only the order)
  sent.length = 0;
  zhBtn('T04').focus();
  zhBtn('T04').click();
  let input = rowOf('T04').querySelector('.input-term');
  input.value = '移动中';
  input.setSelectionRange(1, 1);
  input.dispatchEvent(event('input'));
  // R82 G-L1: moved up, so that the rows are put in order around it and its
  // own row is taken off the page and put back (moved down, the rows after
  // it go before it and it never leaves its place): its blur comes while it
  // is taken off (Chromium), once, and does nothing
  let blurs = 0;
  input.addEventListener('blur', () => blurs++);
  const at = server.terms.findIndex((x) => x.id === 'T04');
  assert.ok(at >= 3, 'room to move it up');
  const [moved] = server.terms.splice(at, 1);
  server.terms.splice(at - 3, 0, moved);
  await gl.reload();
  await settle();
  assert.equal(shownIds().join(' '), tableIds().join(' '), 'moved: in its new place');
  assert.equal(blurs, 1, 'moved: its row was taken off the page and put back');
  assert.ok(rowOf('T04').querySelector('.input-term') === input, 'moved: the same editor');
  assert.ok(active() === input, 'moved: with the focus');
  assert.equal(input.value, '移动中', 'moved: and the text typed');
  assert.equal(input.selectionStart, 1, 'moved: and the caret');
  assert.equal(sent.length, 0, 'moved: nothing is sent');
  key(input, 'Escape');
  await settle();
  server.terms.splice(at - 3, 1);
  server.terms.splice(at, 0, moved);
  await gl.reload();
  await settle();

  // filtered out: a filter typed just before the editor was opened applies
  filterInput.value = 'cipher';
  filterInput.dispatchEvent(event('input'));
  zhBtn('T09').focus();
  zhBtn('T09').click();
  input = rowOf('T09').querySelector('.input-term');
  input.value = '半截';
  input.dispatchEvent(event('input'));
  await settle(300);
  assert.equal(rowOf('T09'), null, 'filtered out: the row is gone');
  assert.equal(sent.length, 0, 'filtered out: nothing typed is saved');
  filterInput.value = '';
  filterInput.dispatchEvent(event('input'));
  await settle(300);
  assert.ok(rowOf('T09') && rowOf('T09').querySelector('.zh-btn') && zhBtn('T09').textContent === '译9', 'back: the rendering, not an editor');
  assert.equal(wrap.querySelector('.input-term'), null, 'back: no editor anywhere');

  // deleted on the server while being edited; then the same id comes back
  zhBtn('T14').focus();
  zhBtn('T14').click();
  input = rowOf('T14').querySelector('.input-term');
  input.value = '已删';
  input.dispatchEvent(event('input'));
  const saved = server.terms;
  server.terms = saved.filter((x) => x.id !== 'T14');
  await gl.reload();
  await settle();
  assert.equal(rowOf('T14'), null, 'deleted: the row is gone');
  assert.equal(sent.length, 0, 'deleted: nothing is sent');
  server.terms = saved;
  await gl.reload();
  await settle();
  assert.ok(rowOf('T14') && rowOf('T14').querySelector('.zh-btn') && zhBtn('T14').textContent === '译14', 'back again: the rendering, not an editor');
  assert.equal(wrap.querySelector('.input-term'), null, 'back again: no editor anywhere');
}

// ---- Esc cancels; leaving the editor saves an unchanged text without a request ----

{
  const snap = snapshot();
  built.length = 0;
  sent.length = 0;
  zhBtn('T05').focus();
  zhBtn('T05').click();
  const input = rowOf('T05').querySelector('.input-term');
  input.value = 'x';
  input.dispatchEvent(event('input'));
  key(input, 'Escape');
  await settle();
  assert.equal(sent.length, 0, 'Esc sends nothing');
  assert.equal(zhBtn('T05').textContent, '译5', 'and keeps the rendering');
  assert.ok(active() === zhBtn('T05'), 'the focus is back on the rendering');
  assert.equal(rowsBuilt(), 2, 'Esc: the row drawn twice, nothing else');
  sameRows(snap, ['T05'], 'Esc');

  // a click on another row's rendering: the editor closes (blur) and the
  // button clicked is still the one on the page, so the click opens its editor
  zhBtn('T07').focus();
  zhBtn('T07').click();
  const other = zhBtn('T08');
  other.focus(); // the press on the other button takes the focus first
  await settle();
  assert.equal(sent.length, 0, 'leaving an unchanged editor sends nothing');
  assert.ok(rowOf('T07').querySelector('.zh-btn'), 'the editor left is closed');
  assert.ok(zhBtn('T08') === other, 'the button pressed is still on the page');
  other.click();
  assert.ok(rowOf('T08').querySelector('.input-term'), 'so the click opens its editor');
  key(rowOf('T08').querySelector('.input-term'), 'Escape');
  await settle();
  sameRows(snap, ['T05', 'T07', 'T08'], 'switching editors');
}

// ---- the server refuses a change (409, a validation error): told, nothing kept ----

for (const refusal of [
  { status: 409, body: { error: { code: 'term.duplicate', params: { en: 'cache' } } } },
  { status: 400, body: { error: { code: 'request.nothing_to_change', params: { fields: 'zh' } } } },
]) {
  const snap = snapshot();
  told.length = 0;
  built.length = 0;
  sent.length = 0;
  server.refuse = refusal;
  zhBtn('T11').focus();
  zhBtn('T11').click();
  const input = rowOf('T11').querySelector('.input-term');
  input.value = '核心';
  input.dispatchEvent(event('input'));
  key(input, 'Enter');
  await settle(60);
  assert.equal(sent.length, 1, `${refusal.status}: the change was sent`);
  assert.deepEqual(told, [refusal.status], `${refusal.status}: the refusal is told`);
  assert.equal(zhBtn('T11').textContent, '译11', `${refusal.status}: the rendering shown is the server's`);
  assert.ok(active() === zhBtn('T11'), `${refusal.status}: the focus is on the rendering`);
  assert.equal(rowsBuilt(), 2, `${refusal.status}: the row drawn twice, nothing else`);
  sameRows(snap, ['T11'], `refused ${refusal.status}`);
}

// ---- another term's editor opened while a change is sent stays open ----

{
  const snap = snapshot();
  sent.length = 0;
  zhBtn('T15').focus();
  zhBtn('T15').click();
  const input = rowOf('T15').querySelector('.input-term');
  input.value = '译15改';
  input.dispatchEvent(event('input'));
  key(input, 'Enter'); // the PATCH is on its way
  zhBtn('T16').focus(); // the first editor is finished already: its blur does nothing
  zhBtn('T16').click();
  const other = rowOf('T16').querySelector('.input-term');
  assert.ok(other && active() === other, 'the other editor is open');
  await settle(60);
  assert.deepEqual(sent, ['PATCH /api/terms/T15 {"zh":"译15改"}']);
  assert.equal(zhBtn('T15').textContent, '译15改', 'the first change is saved');
  assert.ok(rowOf('T16').querySelector('.input-term') === other, 'the other editor stays open');
  assert.ok(active() === other, 'with the focus');
  key(other, 'Escape');
  await settle();
  sameRows(snap, ['T15', 'T16'], 'two editors');
}

// ---- moving a term (drag and drop): busy while it is sent, then in its new group ----

{
  const snap = snapshot();
  built.length = 0;
  sent.length = 0;
  const enforce = inGroup('enforce').length;
  const off = inGroup('off').length;
  const dt = { setData() {}, effectAllowed: '', dropEffect: '' };
  rowOf('T01').dispatchEvent(event('dragstart', { dataTransfer: dt }));
  const offBody = wrap.querySelector('tbody[data-group="off"]');
  offBody.dispatchEvent(event('dragover', { dataTransfer: dt }));
  offBody.dispatchEvent(event('drop', { dataTransfer: dt }));
  assert.equal(rowOf('T01').getAttribute('aria-busy'), 'true', 'busy while the move is sent');
  assert.equal(rowsBuilt(), 1, 'busy: its row only');
  sameRows(snap, ['T01'], 'the move sent');
  await settle(60);
  assert.deepEqual(sent, ['PATCH /api/terms/T01 {"enabled":false}']);
  assert.ok(groupIds('off').includes('T01') && !groupIds('enforce').includes('T01'), 'the term is in the disabled group');
  assert.equal(rowOf('T01').getAttribute('aria-busy'), null);
  assert.equal(groupCount('enforce'), String(enforce - 1), 'the counts follow');
  assert.equal(groupCount('off'), String(off + 1));
  assert.equal(groupIds('off').join(' '), inGroup('off').map((x) => x.id).join(' '), 'in the server order within its group');
  assert.equal(rowsBuilt(), 2, 'moved: its row only');
  sameRows(snap, ['T01'], 'the move done');
  await gl.reload();
  await settle();
  assert.equal(rowsBuilt(), 2, 'the echo of the move draws nothing');
}

// ---- keys: ↑ / ↓ across the groups, F2 edits ----

{
  const lastEnforce = groupIds('enforce').at(-1);
  const firstRef = groupIds('reference')[0];
  zhBtn(lastEnforce).focus();
  key(zhBtn(lastEnforce), 'ArrowDown');
  assert.ok(active() === zhBtn(firstRef), '↓ from the last enforced term goes to the first reference term');
  key(zhBtn(firstRef), 'ArrowUp');
  assert.ok(active() === zhBtn(lastEnforce), '↑ goes back');
  const snap = snapshot();
  built.length = 0;
  key(rowOf('T12').querySelector('.td-grip'), 'F2');
  assert.ok(active() === rowOf('T12').querySelector('.input-term'), 'F2 opens the editor of its row');
  assert.equal(rowsBuilt(), 1);
  key(active(), 'Escape');
  await settle();
  sameRows(snap, ['T12'], 'F2');
}

// ---- Delete: asked first, then the row goes and the focus moves to the next one ----

{
  const snap = snapshot();
  const ids = shownIds();
  const next = ids[ids.indexOf('T13') + 1];
  built.length = 0;
  sent.length = 0;
  zhBtn('T13').focus();
  key(zhBtn('T13'), 'Delete');
  await settle();
  const dlg = body.querySelectorAll('dialog').at(-1);
  assert.ok(dlg && dlg.open, 'Delete asks first');
  dlg.querySelectorAll('button').find((b) => b.className.includes('btn-danger-solid')).click();
  await settle(80);
  assert.deepEqual(sent, ['DELETE /api/terms/T13']);
  assert.equal(rowOf('T13'), null, 'the row is gone');
  assert.ok(active() === zhBtn(next), 'the focus is on the next row, same control');
  assert.equal(rowsBuilt(), 0, 'a deleted term: no row built');
  sameRows(snap, ['T13'], 'deleted');
}

// ---- a term added: its row only, in its place ----

{
  const snap = snapshot();
  built.length = 0;
  const [en, zh] = gl.el.querySelector('.term-add').querySelectorAll('input');
  en.value = 'cache miss';
  zh.value = '缓存缺失';
  gl.el.querySelector('.term-add').dispatchEvent(event('submit'));
  await settle(80);
  assert.ok(rowOf('TNEW'), 'the new term is shown');
  assert.equal(shownIds().join(' '), tableIds().join(' '), 'in its place');
  assert.equal(rowsBuilt(), 1, 'added: its row only');
  sameRows(snap, [], 'added');
}

// ---- typing in the filter: nothing is drawn until a pause (debounced) ----

{
  const total = server.terms.length;
  const said = foot.querySelector('span').textContent;
  for (const q of ['c', 'ca', 'cac']) {
    filterInput.value = q;
    filterInput.dispatchEvent(event('input'));
    await settle(40);
    assert.equal(shownIds().length, total, `"${q}" while typing: the table is as it was`);
    assert.equal(foot.querySelector('span').textContent, said, `"${q}" while typing: so is the foot`);
  }
  await settle(300);
  const want = tableIds().filter((tid) => server.terms.find((x) => x.id === tid).en.includes('cac'));
  assert.equal(shownIds().join(' '), want.join(' '), 'after the pause: the terms matching what was typed');
  // the empty state's "clear" works at once, a keystroke still waiting included
  filterInput.value = 'zzz';
  filterInput.dispatchEvent(event('input'));
  await settle(300);
  filterInput.value = 'zzzz';
  filterInput.dispatchEvent(event('input'));
  wrap.querySelector('.terms-empty').querySelector('button').click();
  assert.equal(shownIds().length, total, 'cleared at once');
  await settle(300);
  assert.equal(shownIds().length, total, 'and the keystroke that was waiting is dropped');
  assert.equal(filterInput.value, '');
}

// ---- the filter: the same rows taken out and put back; counts, foot and empty states ----

{
  const total = server.terms.length;
  const snap = snapshot();
  built.length = 0;
  rowRemovals = 0;
  filterInput.value = 'cache';
  filterInput.dispatchEvent(event('input'));
  await settle(300);
  const want = tableIds().filter((tid) => server.terms.find((x) => x.id === tid).en.includes('cache'));
  assert.equal(shownIds().join(' '), want.join(' '), 'the filter shows the matching terms');
  assert.equal(rowsBuilt(), 0, 'filtering builds no row');
  assert.equal(rowRemovals, total - want.length, 'filtering takes off the rows it hides, no other');
  assert.equal(sameRows(snap, [], 'filtered'), want.length);
  for (const g of ['enforce', 'reference', 'off']) {
    const n = inGroup(g).filter((x) => want.includes(x.id)).length;
    assert.equal(groupCount(g), `${n} / ${inGroup(g).length}`, `the ${g} count while filtered`);
  }
  assert.equal(groupIds('off').length, 0, 'no disabled term matches');
  const empty = wrap.querySelector('tbody[data-group="off"]').querySelector('.tg-empty');
  assert.ok(empty && empty.textContent === t('glossary.group.no_match'), 'a group without a match says so');
  assert.equal(foot.querySelector('span').textContent, t('glossary.foot.shown', { shown: want.length, n: total }));
  assert.ok(foot.querySelector('button'), 'the foot offers to clear the filter');

  rowRemovals = 0;
  filterInput.value = '';
  filterInput.dispatchEvent(event('input'));
  await settle(300);
  assert.equal(shownIds().join(' '), tableIds().join(' '), 'cleared: every term again');
  assert.equal(rowsBuilt(), 0, 'cleared: the same rows put back');
  assert.equal(rowRemovals, 0, 'cleared: the rows shown all along stay where they are');
  assert.equal(sameRows(snap, [], 'cleared'), total);
  assert.equal(wrap.querySelector('.tg-empty'), null, 'no placeholder left');
  assert.equal(groupCount('enforce'), String(inGroup('enforce').length));
  assert.equal(foot.querySelector('span').textContent, t('glossary.foot.total', { n: total }));

  // nothing matches: the empty state and its button; the button brings the rows back
  filterInput.value = 'zzz';
  filterInput.dispatchEvent(event('input'));
  await settle(300);
  assert.ok(wrap.classList.contains('is-flat'), 'nothing matches: no table');
  const none = wrap.querySelector('.terms-empty');
  assert.ok(none && none.textContent.includes(t('glossary.filter.none')), 'it says nothing matches');
  assert.equal(foot.children.length, 0, 'and has no foot');
  none.querySelector('button').click();
  assert.equal(filterInput.value, '');
  assert.ok(active() === filterInput, 'the focus goes to the filter');
  assert.equal(shownIds().length, total, 'every term again');
  assert.equal(rowsBuilt(), 0, 'the same rows again');
  sameRows(snap, [], 'after nothing matched');

  // the source: at once
  sourceSel.value = 'manual';
  sourceSel.dispatchEvent(event('change'));
  const manual = tableIds().filter((tid) => server.terms.find((x) => x.id === tid).source === 'manual');
  assert.equal(shownIds().join(' '), manual.join(' '), 'the source filter');
  sourceSel.value = '';
  sourceSel.dispatchEvent(event('change'));
  assert.equal(shownIds().length, total);
  assert.equal(rowsBuilt(), 0, 'the source filter builds no row');
  sameRows(snap, [], 'the source filter');
}

// ---- a marker card open under the pointer when its row is filtered out (R71 L1) ----
// The row taken off the page gets no pointerleave; put back, its marker is
// closed and a later Esc is not taken by it.

{
  const row = rowOf('T00');
  const mkWrap = row.querySelector('.mk-lock').parentNode;
  const mk = mkWrap.querySelector('.mk');
  mkWrap.dispatchEvent(event('pointerenter', { bubbles: false }));
  assert.equal(mk.getAttribute('aria-expanded'), 'true', 'the pointer opens the card');
  filterInput.value = 'kernel';
  filterInput.dispatchEvent(event('input'));
  await settle(300);
  assert.equal(rowOf('T00'), null, 'the row is filtered out');
  filterInput.value = '';
  filterInput.dispatchEvent(event('input'));
  await settle(300);
  assert.ok(rowOf('T00') === row, 'the same row is back');
  assert.equal(mk.getAttribute('aria-expanded'), 'false', 'its card is closed');
  const esc = event('keydown', { key: 'Escape' });
  body.dispatchEvent(esc);
  assert.ok(!esc.stopped, 'a later Esc goes on to the page');
  mkWrap.dispatchEvent(event('pointerenter', { bubbles: false }));
  assert.equal(mk.getAttribute('aria-expanded'), 'true', 'the next hover opens it again');
  mkWrap.dispatchEvent(event('pointerleave', { bubbles: false }));
  assert.equal(mk.getAttribute('aria-expanded'), 'false');
}

// ---- a group folded and unfolded: the same rows, the focus on its toggle ----

{
  const snap = snapshot();
  built.length = 0;
  const toggle = () => wrap.querySelector('tbody[data-group="reference"]').querySelector('.tg-toggle');
  toggle().focus();
  toggle().click();
  assert.equal(groupIds('reference').length, 0, 'folded: no rows');
  assert.equal(toggle().getAttribute('aria-expanded'), 'false');
  assert.ok(wrap.querySelector('tbody[data-group="reference"]').classList.contains('is-collapsed'));
  assert.equal(groupCount('reference'), String(inGroup('reference').length), 'the count stays');
  assert.ok(active() === toggle(), 'the focus stays on the toggle');
  active().click();
  assert.equal(toggle().getAttribute('aria-expanded'), 'true');
  assert.ok(!wrap.querySelector('tbody[data-group="reference"]').classList.contains('is-collapsed'));
  assert.equal(groupIds('reference').join(' '), inGroup('reference').map((x) => x.id).join(' '), 'unfolded: its rows again');
  assert.equal(rowsBuilt(), 0, 'folding builds no row');
  sameRows(snap, [], 'folded and unfolded');
}

// ---- an empty glossary: the course says whether terms are being extracted ----

{
  const saved = server.terms;
  server.terms = [];
  const g2 = G.createGlossary({ courseId: 'C1', ctx: { system: () => null, resources: () => null }, onCourse() {}, reloadCourse() {} });
  body.append(g2.el);
  g2.setCourse(course());
  await g2.reload();
  await settle();
  const w2 = g2.el.querySelector('.terms-table-wrap');
  assert.ok(w2.textContent.includes(t('glossary.empty.title')), 'empty');
  g2.setCourse({ ...course(), stages: [{ stage: 'glossary_seed', status: 'running', progress: 0.3 }] });
  assert.ok(w2.textContent.includes(t('glossary.empty.extracting')), 'empty, terms being extracted');
  g2.setCourse(course());
  assert.ok(w2.textContent.includes(t('glossary.empty.title')), 'empty again');
  g2.destroy();
  g2.el.remove();
  server.terms = saved;
}

gl.destroy();
console.log('glossary table checks: ok');
process.exit(0);
