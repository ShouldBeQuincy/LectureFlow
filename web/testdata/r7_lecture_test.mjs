// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 7 (user walk-throughs T22, T24, T25): the lecture page and the stage
// cards. Where a failure is shown its way out can be reached: "换模型重试…"
// first for a failure a plain retry cannot get past (T24 B1), "打开设置"
// where the fix is a key, a provider or a model name (T24 S3, T22 G7, G8);
// the card shows the translated sentence and folds the server's text away
// (T24 G2); an automatic retry is not told to be clicked (T24 G5); a wait
// for the power adapter or the daily budget says how to go on (T24 S4, G3);
// the lecture's usage over every run is shown beside its latest (T25 S1).
// The modules run on the DOM stand-in of a11y_test.mjs (copied);
// web/r7_lecture_test.go starts it with the web directory as a file: URL.


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
const gets = []; // the GET requests' paths
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
  else gets.push(u);
  await new Promise((r) => setTimeout(r, 5));
  const a = answer(method, u);
  const text = JSON.stringify(a.body);
  return { ok: a.status < 300, status: a.status, text: async () => text };
};
globalThis.requestAnimationFrame = (fn) => setTimeout(fn, 0);
const settle = (ms = 40) => new Promise((r) => setTimeout(r, ms));

const P = await import(new URL('pipeline.js', base).href);
const U = await import(new URL('util.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const Lec = await import(new URL('view_lecture.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;

const fk = (k) => doc.querySelector(`[data-fkey="${k}"]`);

// ---- lectures and the lecture page ----

const stage = (name, status, o = {}) => ({ stage: name, status, display_status: status, progress: status === 'done' ? 1 : 0, attempt: 0, error: null, allowed_actions: [], started_at: null, finished_at: null, duration_ms: null, ...o });
const names = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
function lecture(id, o = {}) {
  return {
    id, course_id: 'C1', title: `Lecture ${id}`, label: 'L01', recorded_at: '', numbered: false, week: 0, kind: 'lecture', draft: false, created_at: '2026-10-27T10:00:00Z',
    duration_ms: 0, counts: { sentences: 0, patches_pending: 0 }, exports: [], materials: [], playable: false, lecture_actions: [],
    usage: { prompt_tokens: 0, cache_hit_tokens: 0, completion_tokens: 0, estimated_cost: null },
    stages: names.map((n) => stage(n, 'pending')), ...o,
  };
}
// a lecture whose stages are done up to `at`, which is st, the rest pending
function lectureAt(id, st, o = {}) {
  const i = names.indexOf(st.stage);
  return lecture(id, { stages: names.map((n, j) => (j < i ? stage(n, 'done') : j === i ? st : stage(n, 'pending'))), ...o });
}
const llmInfo = (code, params = {}) => ({ code, params: { role: 'translate', retries: 0, provider: 'Anthropic', model: 'claude-opus-5-5', ...params } });

// settings as GET /api/settings answers them (the retry dialog, the
// unlisted-model check): providers with their probed models
let settingsBody = { providers: [], roles: [] };
let settingsReads = 0;

// Mounts the lecture page on lec (GET /api/lectures/{id} answers it) with
// the resources res (resNow, which a check may change before calling the
// view's onResources); returns {page, view, done()}.
let resNow = null;
async function mount(lec, { res = null, sys = null, tab = 'review' } = {}) {
  resNow = res;
  answer = (m, u) => {
    if (u.startsWith('/api/settings')) {
      settingsReads++;
      return { status: 200, body: settingsBody };
    }
    if (u.startsWith(`/api/lectures/${lec.id}/sentences`)) return { status: 200, body: { total: 0, sentences: [] } };
    if (m === 'GET' && u.startsWith(`/api/lectures/${lec.id}`) && !u.includes('/sentences') && !u.includes('/patches')) return { status: 200, body: lec };
    return { status: 200, body: {} };
  };
  const page = new El('main');
  body.append(page);
  const lctx = { system: () => sys, resources: () => resNow, courses: () => [{ id: 'C1', code: 'CSC1', name: 'Course' }], setCurrentCourse() {}, subscribe: () => () => {} };
  let view;
  try {
    view = Lec.mountLecture(page, { id: lec.id, tab }, lctx);
  } catch (e) {
    throw new Error('mountLecture: ' + e.stack);
  }
  await settle();
  return {
    page,
    view,
    done() {
      if (view && view.destroy) view.destroy();
      page.remove();
      body.focus();
    },
  };
}
const notice = (page, cls) => page.querySelector(`div.lec-notice.${cls}`);
const isPrimary = (el) => !!el && /\bbtn-primary\b/.test(el.className);

// ---- T24 B1: a failure a plain retry cannot get past offers another model first ----

{
  // the frozen interface (util.js): the model refused, the total time limit
  assert.ok(U.retryNeedsModel(stage('translate', 'failed', { error_info: llmInfo('llm.refusal') })));
  assert.ok(U.retryNeedsModel(stage('translate', 'failed', { error_info: llmInfo('llm.time_limit', { seconds: 600 }) })));
  assert.ok(!U.retryNeedsModel(stage('translate', 'failed', { error_info: llmInfo('llm.server_error', { status: 503 }) })));

  // the card: "换模型重试…" first and primary, "重试" after it, plain
  const refused = stage('translate', 'failed', { error: 'raw', error_info: llmInfo('llm.refusal'), allowed_actions: ['retry'] });
  assert.deepEqual(P.cardActions(refused), ['retry_model', 'retry']);
  const bar = P.pipelineBar([refused], { key: 'b1', sys: () => null, res: () => null, onAction: () => Promise.resolve(null) });
  assert.ok(isPrimary(bar.querySelector('button[data-fkey="b1:act:translate:retry_model"]')), '换模型重试… is the primary action');
  assert.ok(!isPrimary(bar.querySelector('button[data-fkey="b1:act:translate:retry"]')), '重试 is not');
  // other failures keep 重试 first and primary (stage_card_llm_test.mjs)
  const other = stage('translate', 'failed', { error: 'raw', error_info: llmInfo('llm.server_error', { status: 503 }), allowed_actions: ['retry'] });
  assert.deepEqual(P.cardActions(other), ['retry', 'retry_model']);
  const bar2 = P.pipelineBar([other], { key: 'b1o', sys: () => null, res: () => null, onAction: () => Promise.resolve(null) });
  assert.ok(isPrimary(bar2.querySelector('button[data-fkey="b1o:act:translate:retry"]')));
  // a stage without "换模型重试…" (a course's material terms): another
  // model is chosen in the roles, so "打开设置" (the roles) comes first and
  // primary, 重试 a plain button (r8 WH-05; it was the primary action)
  const mat = stage('material_terms', 'failed', { error: 'raw', error_info: llmInfo('llm.refusal', { role: 'glossary' }), allowed_actions: ['retry'] });
  assert.deepEqual(P.cardActions(mat), ['retry']);
  const bar3 = P.pipelineBar([mat], { key: 'b1m', sys: () => null, res: () => null, onAction: () => Promise.resolve(null) });
  assert.ok(!isPrimary(bar3.querySelector('button[data-fkey="b1m:act:material_terms:retry"]')), '重试 is not the primary action');
  const goMat = bar3.querySelector('a[data-fkey="b1m:act:material_terms:settings"]');
  assert.ok(isPrimary(goMat) && goMat.getAttribute('href') === '#/settings/roles', '打开设置 (the roles) is');

  // the lecture page's notice: the same order
  const st = stage('translate', 'failed', { error: 'raw', error_info: llmInfo('llm.time_limit', { seconds: 600 }), allowed_actions: ['retry'] });
  const m = await mount(lectureAt('B1', st));
  const n = notice(m.page, 'is-failed');
  assert.ok(n, 'the failure notice');
  const model = n.querySelector('button[data-fkey="lecd:B1:retry_model:translate"]');
  const retry = n.querySelector('button[data-fkey="lecd:B1:retry:translate"]');
  assert.ok(isPrimary(model), 'the notice offers 换模型重试… as its primary action');
  assert.ok(model.textContent.includes(t('common.action.retry_model')));
  assert.ok(retry && !isPrimary(retry), '重试 is there, not primary');
  assert.ok(n.children.indexOf(model) < n.children.indexOf(retry), '换模型重试… comes first');
  // it opens the retry dialog
  settingsReads = 0;
  model.click();
  await settle();
  assert.ok(settingsReads > 0, 'the dialog reads the settings');
  const dlg = dialogs.find((d) => d.open && d.className.includes('dlg-retry-model'));
  assert.ok(dlg, 'the retry model dialog opened');
  dlg.querySelectorAll('button').find((b) => b.textContent === t('common.cancel')).click();
  dlg.open = false;
  await settle();
  m.done();
  // other failures: 重试 alone, primary
  const m2 = await mount(lectureAt('B1b', stage('translate', 'failed', { error: 'raw', error_info: llmInfo('llm.server_error', { status: 503 }), allowed_actions: ['retry'] })));
  const n2 = notice(m2.page, 'is-failed');
  assert.equal(n2.querySelector('button[data-fkey="lecd:B1b:retry_model:translate"]'), null);
  assert.ok(isPrimary(n2.querySelector('button[data-fkey="lecd:B1b:retry:translate"]')));
  m2.done();

  // the texts point at the button (its own words), not at Settings alone
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    const label = t('common.action.retry_model');
    for (const code of ['llm.refusal', 'llm.time_limit']) {
      const text = U.errorInfoText(llmInfo(code, { seconds: 600 }));
      assert.ok(text.includes(`“${label}”`), `${loc} ${code} names the button: ${text}`);
    }
  }
  await I.setLocale('zh-CN');
}

// ---- T24 S3, T22 G7, G8: a failure fixed in Settings links there ----

{
  const fix = (code, params) => U.settingsFix(stage('translate', 'failed', { error_info: llmInfo(code, params) }), null);
  // a key, a provider or a model name: the providers; a role's model: the roles
  for (const code of ['llm.no_api_key', 'llm.provider_not_configured', 'llm.rejected', 'llm.request_failed']) assert.equal((fix(code, { status: 401 }) || {}).href, '#/settings/providers', code);
  for (const code of ['llm.no_model', 'llm.provider_deleted', 'settings.role_not_configured', 'settings.role_provider_deleted']) assert.equal((fix(code) || {}).href, '#/settings/roles', code);
  // not for failures Settings does not fix
  for (const code of ['llm.server_error', 'llm.refusal', 'llm.insufficient_balance', 'media.corrupt']) assert.equal(fix(code, { status: 503 }), null, code);
  assert.equal(U.settingsFix(stage('translate', 'failed'), null), null, 'no coded error');

  // a model the provider's last probe does not list (HTTP 404, or 400 as
  // DeepSeek answers an unknown model): "may have been retired", and the
  // fix is the role's model
  const providers = [{ id: 'deepseek', name: 'DeepSeek', models: ['deepseek-flash', 'deepseek-v4-pro'] }, { id: 'or', name: 'OpenRouter', models: [] }];
  const snap = (provider_id, model) => ({ label: `${provider_id} / ${model}`, provider_id, provider_name: provider_id, model });
  const gone = stage('translate', 'failed', { error_info: llmInfo('llm.request_failed', { status: 404, provider: 'DeepSeek', model: 'deepseek-chat' }), model: snap('deepseek', 'deepseek-chat') });
  assert.deepEqual(U.unlistedModel(gone, providers), { model: 'deepseek-chat', provider: 'DeepSeek' });
  assert.deepEqual(U.settingsFix(gone, providers), { href: '#/settings/roles', unlisted: { model: 'deepseek-chat', provider: 'DeepSeek' } });
  const bad400 = { ...gone, error_info: llmInfo('llm.rejected', { status: 400, provider: 'DeepSeek', model: 'deepseek-chat' }) };
  assert.ok(U.unlistedModel(bad400, providers), 'a 400 for an unlisted model');
  for (const [why, st, list] of [
    ['listed', { ...gone, model: snap('deepseek', 'deepseek-flash'), error_info: llmInfo('llm.request_failed', { status: 404, model: 'deepseek-flash' }) }, providers],
    ['a 401 is the key', { ...gone, error_info: llmInfo('llm.rejected', { status: 401, provider: 'DeepSeek', model: 'deepseek-chat' }) }, providers],
    ['no probed list', { ...gone, model: snap('or', 'x/y'), error_info: llmInfo('llm.request_failed', { status: 404, provider: 'OpenRouter', model: 'x/y' }) }, providers],
    ['no settings', gone, null],
  ]) assert.equal(U.unlistedModel(st, list), null, why);
  // without the snapshot's provider id, by the provider's name in the error
  assert.ok(U.unlistedModel({ ...gone, model: null }, providers), 'by name');
  assert.equal(U.settingsFix(gone, null).href, '#/settings/providers', 'unknown: the providers');

  // the lecture page: "打开设置" (the primary action), then 重试; the
  // settings are not read for it
  settingsBody = { providers, roles: [] };
  settingsReads = 0;
  const m = await mount(lectureAt('S3', stage('translate', 'failed', { error: 'raw', error_info: llmInfo('llm.no_api_key', { provider: 'OpenRouter' }), allowed_actions: ['retry'] })));
  const n = notice(m.page, 'is-failed');
  const go = n.querySelector('a[data-fkey="lecd:S3:settings:translate"]');
  assert.ok(go, 'the notice links to Settings');
  assert.equal(go.getAttribute('href'), '#/settings/providers');
  assert.ok(go.textContent.includes(t('common.pipe.open_settings')));
  assert.ok(isPrimary(go), 'the way out is the primary action');
  assert.ok(!isPrimary(n.querySelector('button[data-fkey="lecd:S3:retry:translate"]')), '重试 is not');
  assert.equal(settingsReads, 0, 'no settings read for a key');
  // and the stage card
  const card = m.page.querySelector('.seg-wrap[data-stage="translate"]');
  const cgo = card.querySelector('a[data-fkey="lecd:S3:act:translate:settings"]');
  assert.ok(cgo && cgo.getAttribute('href') === '#/settings/providers', 'the card links to Settings');
  m.done();

  // a model the provider no longer lists: said in full where it shows
  // (not in a hover), "换模型重试…" first, Settings at the roles
  settingsReads = 0;
  const m2 = await mount(lectureAt('S3b', { ...gone, error: 'raw', allowed_actions: ['retry'] }));
  await settle();
  assert.equal(settingsReads, 1, 'the settings read once for the model list');
  const n2 = notice(m2.page, 'is-failed');
  const said = t('common.pipe.model_unlisted', { model: 'deepseek-chat', provider: 'DeepSeek' });
  const hint = n2.querySelector('.lec-notice-hint');
  assert.ok(hint && hint.textContent === said, `the notice says it: ${hint && hint.textContent}`);
  assert.ok(isPrimary(n2.querySelector('button[data-fkey="lecd:S3b:retry_model:translate"]')), '换模型重试… first');
  const go2 = n2.querySelector('a[data-fkey="lecd:S3b:settings:translate"]');
  assert.ok(go2 && go2.getAttribute('href') === '#/settings/roles' && !isPrimary(go2), 'Settings at the roles');
  const card2 = m2.page.querySelector('.seg-wrap[data-stage="translate"]');
  assert.ok(card2.querySelector('.card-errhint') && card2.querySelector('.card-errhint').textContent === said, 'the card says it too');
  const acts = card2.querySelector('.card-actions').children;
  assert.ok(isPrimary(acts[0]) && acts[0].dataset.fkey === 'lecd:S3b:act:translate:retry_model', 'the card: 换模型重试… first');
  assert.ok(acts.some((a) => a.dataset.fkey === 'lecd:S3b:act:translate:settings' && a.getAttribute('href') === '#/settings/roles'), 'and Settings at the roles');
  assert.ok(acts.every((a, i) => i === 0 || !isPrimary(a)), 'one primary action');
  m2.done();
  settingsBody = { providers: [], roles: [] };
}

// ---- T24 G2: the card shows the translated sentence; the server's text is folded ----

{
  // the text a closed <details> leaves shown: its summary only
  const shownText = (n) => (n instanceof El ? (n.tagName === 'DETAILS' ? n.children.filter((c) => c.tagName === 'SUMMARY').map(shownText).join('') : n.children.map(shownText).join('')) : n.textContent);
  const raw = 'LLM 请求被拒绝（translate，HTTP 401），请检查 API key 或模型名（provider OpenRouter，model deepseek/deepseek-chat）：{"error":"example"}';
  const failed = stage('translate', 'failed', { error: raw, error_info: llmInfo('llm.rejected', { status: 401, provider: 'OpenRouter', model: 'deepseek/deepseek-chat' }), allowed_actions: ['retry'] });
  await I.setLocale('en');
  const bar = P.pipelineBar([failed], { key: 'g2', sys: () => null, res: () => null, onAction: () => Promise.resolve(null) });
  const box = bar.querySelector('.card-error');
  assert.equal(box.querySelector('.card-errmsg').textContent, U.stageErrorText(failed), 'the translated sentence');
  const det = box.querySelector('details.card-errdetail');
  assert.ok(det, 'the server text is folded');
  assert.equal(det.querySelector('summary').textContent, t('common.pipe.tech_details'));
  assert.ok(det.querySelector('pre.card-errtext').textContent.includes('HTTP 401'), 'its first 300 characters inside (SPEC §9.8)');
  assert.ok(!/[一-鿿]/.test(shownText(bar.querySelector('.seg-card'))), `nothing in Chinese shows on the English card: ${shownText(bar.querySelector('.seg-card'))}`);
  assert.ok(box.querySelector('button[data-fkey="g2:copy:translate"]'), '复制完整错误 stays');
  // described by the translated sentence (R19), not the folded text
  const seg = bar.querySelector('button.seg');
  const ids = (seg.getAttribute('aria-describedby') || '').split(' ');
  assert.ok(ids.includes(box.querySelector('.card-errmsg').getAttribute('id')), 'the segment is described by the sentence');
  assert.ok(!det.querySelector('pre').getAttribute('id'), 'not by the folded text');
  await I.setLocale('zh-CN');
  // an error without a code: the text is all there is, shown as before
  const plain = P.pipelineBar([stage('translate', 'failed', { error: 'something broke', allowed_actions: ['retry'] })], { key: 'g2p', sys: () => null, res: () => null });
  assert.equal(plain.querySelector('details'), null);
  assert.equal(plain.querySelector('.card-error').querySelector('pre.card-errtext').textContent, 'something broke');
  // a coded text the same as the server's: shown once, not folded
  const same = P.pipelineBar([stage('translate', 'failed', { error: 'x', error_info: { code: 'no.such.code', detail: 'x' } })], { key: 'g2s', sys: () => null, res: () => null });
  assert.equal(same.querySelector('details'), null);
  // a warning (done): its coded text shown, the server's folded as well
  const warn = P.pipelineBar([stage('repair', 'done', { error: '第 3-5 句的修复请求 3 次都无法解析：chunk 2 HTTP 500', warning_info: { code: 'repair.sentences_unrepaired', params: { ranges: '3-5', attempts: 3 } } })], { key: 'g2w', sys: () => null, res: () => null });
  assert.ok(warn.querySelector('.card-errmsg'), 'the coded warning');
  assert.ok(warn.querySelector('details.card-errdetail'), 'a coded warning folds the server text');
  assert.ok(warn.querySelector('details.card-errdetail').querySelector('pre.card-errtext').textContent.includes('chunk 2 HTTP 500'), 'whole');
}

// ---- T24 G5: an automatic retry is not told to be clicked ----

{
  const codes = [
    ['llm.network', {}],
    ['llm.timeout', { seconds: 120 }],
    ['llm.idle_timeout', { seconds: 90 }],
    ['llm.rate_limited', {}],
    ['llm.overloaded', {}],
    ['llm.server_error', { status: 503 }],
    ['prep.file_in_use', { name: 'a.mp4' }],
    ['export.file_in_use', { name: 'a.zh.vtt' }],
  ];
  const retrying = (code, params) => stage('summary', 'queued', { error: 'raw', error_info: { code, params: { role: 'summary', retries: 3, ...params } }, waiting_for: 'retry_backoff:18' });
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    const click = loc === 'en' ? `“${t('common.retry')}”` : '点击重试';
    const clickShort = loc === 'en' ? `“${t('common.retry')}”` : '点重试';
    for (const [code, params] of codes) {
      const final = U.stageErrorText({ ...retrying(code, params), status: 'failed', waiting_for: null });
      assert.ok(final.includes(click) || final.includes(clickShort), `${loc} ${code}: a final failure still says to retry: ${final}`);
      const text = U.stageErrorText(retrying(code, params));
      assert.ok(text && !text.includes(click) && !text.includes(clickShort), `${loc} ${code}: not while it retries by itself: ${text}`);
      // the same failure, the start of its sentence
      const head = final.split(loc === 'en' ? '.' : '，')[0].replace(/[：(（].*$/, '');
      assert.ok(text.startsWith(head), `${loc} ${code}: "${text}" says what failed ("${head}")`);
    }
    // a failed stage that still waits for its retry (failed + waiting_for)
    const f = U.stageErrorText({ ...retrying('llm.server_error', { status: 503 }), status: 'failed' });
    assert.ok(!f.includes(click), `${loc}: failed with a retry pending: ${f}`);
  }
  await I.setLocale('zh-CN');
  // a code without such a text: as it was
  const other = stage('summary', 'queued', { error: 'raw', error_info: { code: 'media.encoder_failed', params: {} }, waiting_for: 'retry_backoff:18' });
  assert.equal(U.stageErrorText(other), U.errorInfoText(other.error_info));
  // the card: the wait line says it retries; the last error does not say "click"
  const st = retrying('llm.server_error', { status: 503 });
  const bar = P.pipelineBar([st], { key: 'g5', sys: () => null, res: () => null, onAction: () => Promise.resolve(null) });
  assert.ok(bar.querySelector('.card-wait').textContent.includes('18 秒后自动重试'), bar.querySelector('.card-wait').textContent);
  const msg = bar.querySelector('.card-errmsg').textContent;
  assert.ok(msg.includes('HTTP 503') && !msg.includes('点击重试'), msg);
  assert.equal(bar.querySelector('.card-actions'), null, 'no button to click');
}

// ---- T24 S4, G3: a wait for the power adapter or the budget says how to go on ----

{
  const dev = (id, kind, o = {}) => ({ id, label: id, kind, backend: 'openvino', present: true, enabled: true, paused: false, active: false, tasks: [], slots_used: 0, slots_max: 1, ...o });
  const battery = (devices) => ({ devices, policy: { battery_machine: true, mode: 'auto' }, power: { known: true, has_battery: true, on_battery: true }, providers: [] });
  const asrWait = (w) => lectureAt('S4', stage('asr', 'queued', { waiting_for: w }));
  // Each code says only what ends it (internal/devices/policy.go
  // placeBattery; R72 H1). The notice's text for a code and devices:
  const noticeText = async (w, devices) => {
    const m = await mount(asrWait(w), { res: devices ? battery(devices) : null });
    const n = notice(m.page, 'is-wait');
    const go = n && n.querySelector('a[data-fkey="lecd:S4:power"]');
    assert.ok(go && go.getAttribute('href') === '#/settings/asr', `${w}: to the transcription devices`);
    assert.ok(go.textContent.includes(t('lecture.notice.power_settings')));
    const text = n.querySelector('.lec-notice-text').textContent;
    m.done();
    return text;
  };
  const cuda = dev('cuda:0', 'cuda', { backend: 'faster-whisper' });
  const npuOff = dev('ov:NPU', 'npu', { enabled: false });
  const gpuOff = dev('ov:GPU', 'igpu', { enabled: false });
  // power_wait_ac: bound to a device that is not the battery device (a
  // CUDA checkpoint is never handed to the NPU or Intel GPU,
  // policy_test.go): only the charger, whatever Intel device is off
  for (const devices of [[cuda, npuOff], [cuda, gpuOff], [cuda], null]) {
    assert.equal(await noticeText('power_wait_ac', devices), t('lecture.notice.power_wait_ac'), 'power_wait_ac: the charger only');
  }
  assert.ok(!t('lecture.notice.power_wait_ac').includes('启用'), 'it does not suggest turning a device on');
  // power_no_saver_device: on battery, no usable NPU or Intel GPU
  assert.equal(await noticeText('power_no_saver_device', [cuda, npuOff]), t('lecture.notice.power_wait_other'), 'the charger (CUDA on AC), or the NPU now');
  assert.equal(await noticeText('power_no_saver_device', [cuda, { ...npuOff, present: false }]), t('lecture.notice.power_wait'), 'a device not present cannot take over');
  assert.equal(await noticeText('power_no_saver_device', [cuda]), t('lecture.notice.power_wait'), 'nothing to turn on: the charger');
  assert.equal(await noticeText('power_no_saver_device', [{ ...cuda, enabled: false }, gpuOff]), t('lecture.notice.power_enable_saver'), 'no device on AC either: turning the Intel GPU on is the way');
  assert.equal(await noticeText('power_no_saver_device', [{ ...cuda, enabled: false }]), t('lecture.notice.power_no_device'), 'no device at all');
  assert.equal(await noticeText('power_no_saver_device', null), t('lecture.notice.power_wait'), 'devices not known yet: the rule');
  // power_no_device: on AC (or pinned to a CPU device): the charger is no way out
  for (const devices of [[{ ...cuda, enabled: false }, npuOff], null]) {
    const text = await noticeText('power_no_device', devices);
    assert.equal(text, t('lecture.notice.power_no_device'));
  }
  assert.ok(!t('lecture.notice.power_no_device').includes(t('lecture.notice.power_wait')), 'never “once plugged in”');
  // on_battery (before power management): the charger
  assert.equal(await noticeText('on_battery', [cuda, npuOff]), t('lecture.notice.power_wait'));
  // the devices arrive later (the resources): the notice follows them
  const m = await mount(asrWait('power_no_saver_device'), { res: null });
  const textOf = () => notice(m.page, 'is-wait').querySelector('.lec-notice-text').textContent;
  assert.equal(textOf(), t('lecture.notice.power_wait'));
  resNow = battery([cuda, gpuOff]);
  m.view.onResources();
  assert.equal(textOf(), t('lecture.notice.power_wait_other'), 'redrawn with the resources');
  m.done();
  // other waits: no such notice
  const m2 = await mount(lectureAt('S4b', stage('asr', 'queued', { waiting_for: 'gpu_slot' })), { res: battery([]) });
  assert.equal(notice(m2.page, 'is-wait'), null);
  m2.done();

  // the daily budget: when it comes back, and where to raise it
  const m3 = await mount(lectureAt('G3', stage('translate', 'queued', { waiting_for: 'budget' })));
  const b = notice(m3.page, 'is-wait');
  assert.ok(b && b.textContent.includes(t('lecture.notice.budget_wait')), b && b.textContent);
  const bgo = b.querySelector('a[data-fkey="lecd:G3:budget"]');
  assert.ok(bgo && bgo.getAttribute('href') === '#/settings/general', 'to the daily budget');
  assert.ok(bgo.textContent.includes(t('lecture.notice.budget_settings')));
  m3.done();
  // a draft says nothing of it
  const m4 = await mount({ ...lectureAt('G3b', stage('translate', 'queued', { waiting_for: 'budget' })), draft: true });
  assert.equal(notice(m4.page, 'is-wait'), null);
  m4.done();
}

// ---- T25 S1: the lecture's usage over every run, beside its latest ----

{
  const statOf = (page, key) => page.querySelectorAll('.lec-stat').find((s) => s.querySelector('.lec-stat-k').textContent === t(key));
  const sub = (s) => (s.querySelector('.lec-stat-sub') || { textContent: null }).textContent;
  const usage = { prompt_tokens: 71400, cache_hit_tokens: 0, completion_tokens: 2000, estimated_cost: 0.07 };
  const total = { prompt_tokens: 1290000, cache_hit_tokens: 0, completion_tokens: 25000, estimated_cost: 0.27 };
  const done = names.map((n) => stage(n, 'done'));
  const m = await mount(lecture('S1', { stages: done, usage, usage_total: total }));
  const tok = statOf(m.page, 'lecture.stat.tokens');
  const cost = statOf(m.page, 'lecture.stat.cost');
  assert.equal(tok.querySelector('.lec-stat-v').textContent, `${U.fmtTokens(71400)} / ${U.fmtTokens(2000)}`, 'the latest runs, as before');
  assert.equal(sub(tok), t('lecture.stat.tokens_total_sub', { input: U.fmtTokens(1290000), output: U.fmtTokens(25000) }));
  assert.equal(cost.querySelector('.lec-stat-v').textContent, U.fmtMoney(0.07, null));
  assert.equal(sub(cost), t('lecture.stat.cost_total_sub', { total: U.fmtMoney(0.27, null) }));
  for (const s of [tok, cost]) assert.equal(s.getAttribute('title'), t('lecture.stat.usage_tip'), 'the difference explained');
  assert.ok(t('lecture.stat.cost_total_sub', { total: 'X' }).includes('X'));
  m.done();
  // nothing in the latest runs, something in all: the total still shows
  const m2 = await mount(lecture('S1b', { stages: done, usage: { prompt_tokens: 0, cache_hit_tokens: 0, completion_tokens: 0, estimated_cost: null }, usage_total: total }));
  assert.equal(sub(statOf(m2.page, 'lecture.stat.tokens')), t('lecture.stat.tokens_total_sub', { input: U.fmtTokens(1290000), output: U.fmtTokens(25000) }));
  assert.equal(sub(statOf(m2.page, 'lecture.stat.cost')), t('lecture.stat.cost_total_sub', { total: U.fmtMoney(0.27, null) }));
  m2.done();
  // without the field (a lecture list's object, an older back end): as before
  const m3 = await mount(lecture('S1c', { stages: done, usage }));
  const tok3 = statOf(m3.page, 'lecture.stat.tokens');
  const cost3 = statOf(m3.page, 'lecture.stat.cost');
  assert.equal(sub(tok3), t('lecture.stat.tokens_sub'));
  assert.equal(sub(cost3), t('lecture.stat.cost_sub'));
  assert.equal(tok3.getAttribute('title'), null);
  m3.done();
  // a total without prices: tokens only, the cost as before
  const m4 = await mount(lecture('S1d', { stages: done, usage: { ...usage, estimated_cost: null }, usage_total: { ...total, estimated_cost: null } }));
  assert.equal(sub(statOf(m4.page, 'lecture.stat.cost')), t('lecture.stat.cost_none'));
  assert.equal(sub(statOf(m4.page, 'lecture.stat.tokens')), t('lecture.stat.tokens_total_sub', { input: U.fmtTokens(1290000), output: U.fmtTokens(25000) }));
  m4.done();

  // usage_total comes only when asked for (GET …?usage_total=1 scans the
  // jobs): when the page opens and after a job ended, not with every
  // event; the reads between keep the last total
  const Api = await import(new URL('api.js', base).href);
  const realLecture = Api.api.lecture;
  const asked = [];
  const running = lecture('S1e', { stages: names.map((n) => stage(n, n === 'translate' ? 'running' : n === 'export' ? 'pending' : 'done')), usage });
  let now = running;
  Api.api.lecture = async (lid, opts = {}) => {
    asked.push(!!opts.usageTotal);
    await settle(5);
    return opts.usageTotal ? { ...now, usage_total: total } : { ...now };
  };
  try {
    const m5 = await mount(running);
    assert.deepEqual(asked, [true], 'the first read asks for the total');
    const totalSub = t('lecture.stat.tokens_total_sub', { input: U.fmtTokens(1290000), output: U.fmtTokens(25000) });
    assert.equal(sub(statOf(m5.page, 'lecture.stat.tokens')), totalSub);
    // a change of the lecture: read again without it; the total stays shown
    m5.view.onChanged({ kind: 'lecture', id: 'S1e' });
    await settle(400);
    assert.deepEqual(asked, [true, false], 'a change does not ask for it');
    assert.equal(sub(statOf(m5.page, 'lecture.stat.tokens')), totalSub, 'the last total stays');
    // progress of the running stage: no read at all
    m5.view.onJob({ scope: 'lecture', scope_id: 'S1e', stage: 'translate', status: 'running', progress: 0.5 });
    await settle(400);
    assert.deepEqual(asked, [true, false]);
    // the stage finished: read with the total
    now = lecture('S1e', { stages: names.map((n) => stage(n, n === 'export' ? 'queued' : 'done')), usage });
    m5.view.onJob({ scope: 'lecture', scope_id: 'S1e', stage: 'translate', status: 'done', progress: 1 });
    m5.view.onChanged({ kind: 'lecture', id: 'S1e' }); // within the same delay: one read
    await settle(400);
    assert.deepEqual(asked, [true, false, true], 'a job that ended asks for it once');
    m5.done();
  } finally {
    Api.api.lecture = realLecture;
  }
  // api.js puts it in the query only when asked
  answer = () => ({ status: 200, body: running });
  gets.length = 0;
  await Api.api.lecture('S1e', { quiet: true, usageTotal: true });
  await Api.api.lecture('S1e', { quiet: true });
  await Api.api.lecture('a b');
  assert.deepEqual(gets, ['/api/lectures/S1e?usage_total=1', '/api/lectures/S1e', '/api/lectures/a%20b']);
}

// ---- T23 severe 2: a bar is one Tab stop; the arrow keys move between its segments ----

{
  // document order for the bar's "Shift+Tab from after it" (the stand-in has none)
  El.prototype.compareDocumentPosition = function (other) {
    if (this.contains(other)) return 16 | 4;
    if (other instanceof El && other.contains(this)) return 8 | 2;
    const order = [];
    const walk = (n) => {
      order.push(n);
      for (const c of n.children || []) if (c instanceof El) walk(c);
    };
    walk(root);
    return order.indexOf(other) > order.indexOf(this) ? 4 : 2;
  };
  try {
    const before = new El('button');
    const after = new El('button');
    const sts = [
      stage('prep', 'done', { allowed_actions: ['rerun'] }),
      stage('asr', 'done', { allowed_actions: ['rerun'] }),
      stage('summary', 'failed', { error: 'boom', allowed_actions: ['retry'] }),
      stage('repair', 'pending'),
      stage('translate', 'pending'),
      stage('export', 'pending'),
    ];
    const bar = P.pipelineBar(sts, { key: 'r12', sys: () => null, res: () => null, onAction: () => Promise.resolve(null) });
    body.append(before, bar, after);
    assert.equal(bar.getAttribute('role'), 'toolbar', 'a toolbar: its segments are moved through with the arrow keys');
    const segs = bar.querySelectorAll('button.seg');
    const stops = () => segs.filter((b) => b.getAttribute('tabindex') === '0');
    assert.equal(segs.length, 6);
    assert.equal(stops().length, 1, 'one Tab stop');
    assert.ok(stops()[0] === segs[2], 'on the failed stage');
    assert.ok(segs.every((b) => b.getAttribute('tabindex') === '0' || b.getAttribute('tabindex') === '-1'), 'the others out of the Tab order');
    // each segment says its stage and its state
    segs.forEach((b, i) => assert.equal(b.getAttribute('aria-label'), t('common.pipe.seg_label', { stage: U.stageName(sts[i].stage), status: U.stageStatusText(sts[i]) })));
    // the arrow keys, Home and End; the Tab stop follows the focus
    segs[2].focus();
    const key = (el, k) => {
      const ev = event('keydown', { key: k });
      el.dispatchEvent(ev);
      return ev;
    };
    assert.ok(key(segs[2], 'ArrowRight').defaultPrevented);
    assert.ok(doc.activeElement === segs[3], '→ the next segment');
    assert.ok(stops()[0] === segs[3] && stops().length === 1, 'which takes the Tab stop');
    key(segs[3], 'ArrowLeft');
    assert.ok(doc.activeElement === segs[2], '← the previous one');
    key(segs[2], 'End');
    assert.ok(doc.activeElement === segs[5], 'End: the last');
    key(segs[5], 'ArrowRight');
    assert.ok(doc.activeElement === segs[5], 'no wrapping past the end');
    key(segs[5], 'Home');
    assert.ok(doc.activeElement === segs[0], 'Home: the first');
    // the focus on a segment opens its card (L16): Tab goes on into it, and
    // an arrow key there is the card's, not the bar's
    segs[2].focus();
    const card = segs[2].parentNode.querySelector('.seg-card');
    const retry = card.querySelector('button[data-fkey="r12:act:summary:retry"]');
    assert.ok(retry, "the failed stage's card has 重试");
    retry.focus();
    assert.ok(!key(retry, 'ArrowRight').defaultPrevented && doc.activeElement === retry, 'an arrow key in the card does nothing to the bar');
    // Shift+Tab from after the bar: back through the card's controls, not past them
    after.focus();
    segs[2].focus(); // the browser's Shift+Tab lands on the Tab stop
    const ctrls = card.querySelectorAll('button');
    assert.ok(doc.activeElement === ctrls[ctrls.length - 1], `the card's last control: ${doc.activeElement && doc.activeElement.textContent}`);
    // from before the bar, or by the pointer: the segment itself
    before.focus();
    segs[2].focus();
    assert.ok(doc.activeElement === segs[2], 'Tab from before: the segment');
    after.focus();
    segs[2].dispatchEvent(event('pointerdown'));
    segs[2].focus();
    assert.ok(doc.activeElement === segs[2], 'a click: the segment');
    // a bar redrawn keeps one Tab stop (on the stage that matters: running)
    const run = P.pipelineBar([stage('prep', 'done'), stage('asr', 'running', { progress: 0.3 }), stage('summary', 'pending')], { key: 'r12b', sys: () => null, res: () => null });
    assert.deepEqual(run.querySelectorAll('button.seg').map((b) => b.getAttribute('tabindex')), ['-1', '0', '-1']);
    assert.equal(P.tabStopIndex(names.map((n) => stage(n, 'done'))), 0, 'all done: the first');
    bar.remove();
    before.remove();
    after.remove();
    body.focus();
  } finally {
    delete El.prototype.compareDocumentPosition;
  }
}

// ---- P2 hotspot 3: a card is built when it opens (opts.lazyCards) ----

{
  const count = (n) => (n instanceof El ? 1 + n.children.reduce((s, c) => s + count(c), 0) : 0);
  const done = () => names.map((n) => stage(n, 'done', { allowed_actions: ['rerun'], duration_ms: 42000, stats: { prompt_tokens: 1000, completion_tokens: 200, cache_hit_tokens: 0, estimated_cost: 0.01 }, model: { label: 'DeepSeek / deepseek-chat', provider_id: 'deepseek', model: 'deepseek-chat' } }));
  const eager = P.pipelineBar(done(), { key: 'e13', sys: () => null, res: () => null, onAction: () => Promise.resolve(null) });
  const lazy = P.pipelineBar(done(), { key: 'l13', sys: () => null, res: () => null, onAction: () => Promise.resolve(null), lazyCards: true });
  console.log(`a bar of six done stages: ${count(eager)} elements with its cards built, ${count(lazy)} with lazyCards`);
  assert.ok(count(lazy) * 4 < count(eager), `far fewer elements: ${count(lazy)} vs ${count(eager)}`);
  body.append(lazy);
  const wrap = lazy.querySelector('.seg-wrap[data-stage="asr"]');
  const seg = wrap.querySelector('.seg');
  const card = wrap.querySelector('.seg-card');
  // the card itself is there (L16: controlled by the segment, a named group), empty
  assert.ok(card && seg.getAttribute('aria-controls') === card.getAttribute('id'));
  assert.equal(card.getAttribute('role'), 'group');
  assert.equal(card.getAttribute('aria-label'), U.stageName('asr'));
  assert.equal(card.children.length, 0, 'not built yet');
  assert.equal(seg.getAttribute('aria-expanded'), 'false');
  // the pointer builds and opens it, with the same content as built at once
  wrap.dispatchEvent(event('pointerenter'));
  assert.equal(seg.getAttribute('aria-expanded'), 'true');
  assert.ok(card.querySelector('button[data-fkey="l13:act:asr:rerun"]'), 'built: its buttons');
  const eagerCard = eager.querySelector('.seg-wrap[data-stage="asr"]').querySelector('.seg-card');
  assert.equal(card.textContent, eagerCard.textContent, 'the same card');
  wrap.dispatchEvent(event('pointerleave'));
  // the focus builds another; Esc, then Enter on the segment, as before
  const prep = lazy.querySelector('.seg-wrap[data-stage="prep"]');
  prep.querySelector('.seg').focus();
  assert.ok(prep.querySelector('.seg-card').children.length > 0, 'the focus builds it');
  assert.equal(prep.querySelector('.seg').getAttribute('aria-expanded'), 'true');
  prep.dispatchEvent(event('keydown', { key: 'Escape' }));
  assert.equal(prep.querySelector('.seg').getAttribute('aria-expanded'), 'false');
  prep.querySelector('.seg').click();
  assert.equal(prep.querySelector('.seg').getAttribute('aria-expanded'), 'true', 'Enter opens it again');
  lazy.remove();
  body.focus();

  // a stage its card describes (R19, R21) is built at once, as before
  const failed = stage('translate', 'failed', { error: 'deepseek: 402 insufficient balance', allowed_actions: ['retry'] });
  const waiting = stage('asr', 'queued', { waiting_for: 'gpu_slot' });
  const queued = stage('summary', 'queued');
  const running = stage('repair', 'running', { progress: 0.2, allowed_actions: ['cancel'], started_at: new Date(clock - 60000).toISOString() });
  const bar = P.pipelineBar([stage('prep', 'done'), waiting, queued, running, failed], { key: 'l13b', sys: () => null, res: () => null, onAction: () => Promise.resolve(null), lazyCards: true });
  body.append(bar);
  const w = (s) => bar.querySelector(`.seg-wrap[data-stage="${s}"]`);
  const ids = (s) => (w(s).querySelector('.seg').getAttribute('aria-describedby') || '').split(' ').filter(Boolean);
  const byId = (s, id) => [w(s).querySelector('.seg-card'), ...w(s).querySelectorAll('div'), ...w(s).querySelectorAll('span'), ...w(s).querySelectorAll('p'), ...w(s).querySelectorAll('pre')].find((e) => e && e.getAttribute('id') === id);
  assert.equal(ids('translate').length, 2, 'the failed stage: its label and error line');
  assert.ok(ids('translate').every((id) => byId('translate', id)), 'lines of its card');
  assert.ok(byId('translate', ids('translate')[1]).textContent.includes('insufficient balance'));
  assert.equal(ids('asr').length, 1, 'the waiting stage: its reason');
  assert.equal(byId('asr', ids('asr')[0]).textContent, U.stageWaitingText(waiting, null, null));
  assert.equal(w('summary').querySelector('.seg-card').children.length, 0, 'a queued stage without a reason: not built');
  assert.equal(w('repair').querySelector('.seg-card').children.length, 0, 'a running stage: not built');
  // a reason that appears later builds it and describes the segment
  queued.waiting_for = 'gpu_slot';
  w('summary').refresh();
  assert.equal(ids('summary').length, 1, 'the reason describes it');
  assert.ok(byId('summary', ids('summary')[0]), 'a line of the card, built now');
  // progress of a card not built: nothing to redraw; opened, it is current
  bar.setProgress('repair', 0.6);
  assert.equal(w('repair').querySelector('.seg-card').children.length, 0);
  w('repair').dispatchEvent(event('pointerenter'));
  assert.equal(w('repair').querySelector('.card-status-text').textContent, U.stageStatusText(running));
  assert.ok(U.stageStatusText(running).includes(U.pct(0.6)));
  w('repair').dispatchEvent(event('pointerleave'));
  bar.remove();
}

// ---- T25 G9: the share next to the bar names its stage ----

{
  const sts = () => [stage('prep', 'done'), stage('asr', 'running', { progress: 0.42 }), stage('summary', 'pending')];
  const lg = P.pipelineBar(sts(), { key: 'g9', size: 'lg', sys: () => null, res: () => null });
  const pctOf = (b) => b.querySelector('span.pipe-pct');
  assert.equal(pctOf(lg).textContent, t('common.pipe.pct_stage', { stage: U.stageName('asr'), pct: U.pct(0.42) }), 'the lecture page names the stage');
  assert.ok(pctOf(lg).textContent.includes(U.stageName('asr')) && pctOf(lg).textContent.includes(U.pct(0.42)));
  assert.equal(pctOf(lg).getAttribute('title'), t('common.pipe.pct_tip', { stage: U.stageName('asr'), pct: U.pct(0.42) }));
  // it follows the next stage's progress
  const next = [stage('prep', 'done'), stage('asr', 'done'), stage('summary', 'running', { progress: 0.05 })];
  const lg2 = P.pipelineBar(next, { key: 'g9b', size: 'lg', sys: () => null, res: () => null });
  assert.equal(pctOf(lg2).textContent, t('common.pipe.pct_stage', { stage: U.stageName('summary'), pct: U.pct(0.05) }));
  lg2.setProgress('summary', 0.3);
  assert.equal(pctOf(lg2).textContent, t('common.pipe.pct_stage', { stage: U.stageName('summary'), pct: U.pct(0.3) }));
  // a row's small bar: the number, the stage on hover
  const sm = P.pipelineBar(sts(), { key: 'g9c', sys: () => null, res: () => null });
  assert.equal(pctOf(sm).textContent, U.pct(0.42));
  assert.equal(pctOf(sm).getAttribute('title'), t('common.pipe.pct_tip', { stage: U.stageName('asr'), pct: U.pct(0.42) }));
  // no progress reported yet: no "0%"
  const zero = P.pipelineBar([stage('prep', 'done'), stage('asr', 'running', { progress: 0 })], { key: 'g9d', size: 'lg', sys: () => null, res: () => null });
  assert.equal(pctOf(zero).textContent, '');
  // a queued stage's checkpoint, muted, as before (named on the large bar)
  const ck = P.pipelineBar([stage('prep', 'done'), stage('asr', 'queued', { checkpoint: { resume_ms: 1000, duration_ms: 4000, progress: 0.25 } })], { key: 'g9e', size: 'lg', sys: () => null, res: () => null });
  assert.equal(pctOf(ck).textContent, t('common.pipe.pct_stage', { stage: U.stageName('asr'), pct: U.pct(0.25) }));
  assert.ok(pctOf(ck).className.includes('is-ckpt'));
}

// ---- T25 G1, G2, G3, G5: a run that goes on says so, from its start ----

{
  const run = (stageName, o = {}) => stage(stageName, 'running', { progress: 0, ...o });
  // no progress reported yet: not "运行中 0%"
  assert.equal(U.stageStatusText(run('asr', { model: { asr: { device_id: 'cuda:0' } } })), t('common.status.running_preparing_asr'), 'a local transcription loads its model and finds the speech');
  assert.equal(U.stageStatusText(run('asr', { model: { asr: { device_id: 'colab:remote' } } })), t('common.status.running_preparing'), 'a remote one: getting ready (its card has the steps)');
  assert.equal(U.stageStatusText(run('asr', { model: { asr: { device_id: 'ssh:lab' } } })), t('common.status.running_preparing'));
  assert.equal(U.stageStatusText(run('summary')), t('common.status.running_preparing'));
  // run again (a retry, the restart): a transcription on this computer may
  // go on from its checkpoint (R79 M3: said without a promise — another
  // device or model starts over); a remote one never continues the local
  // checkpoint; a translation retried with another model starts over, so
  // its state promises nothing (the lecture page says how many sentences
  // it already has)
  assert.equal(U.stageStatusText(run('asr', { attempt: 1, model: { asr: { device_id: 'cuda:0' } } })), t('common.status.running_resuming'));
  assert.equal(U.stageStatusText(run('asr', { attempt: 1, model: { asr: { device_id: 'colab:remote' } } })), t('common.status.running_preparing'), 'Colab: not from the local checkpoint');
  assert.equal(U.stageStatusText(run('asr', { attempt: 1, model: { asr: { device_id: 'ssh:lab' } } })), t('common.status.running_preparing'), 'an SSH server: likewise');
  assert.equal(U.stageStatusText(run('translate', { attempt: 1 })), t('common.status.running_preparing'), 'a translation promises nothing');
  assert.equal(U.stageStatusText(run('summary', { attempt: 1 })), t('common.status.running_preparing'), 'a summary is asked for again whole');
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    const text = t('common.status.running_resuming');
    assert.ok(loc === 'en' ? /may continue/.test(text) : text.includes('可能'), `${loc}: no promise: ${text}`);
  }
  await I.setLocale('zh-CN');
  // once it reports progress: the share, as before
  assert.equal(U.stageStatusText(run('asr', { progress: 0.11, attempt: 1 })), t('common.status.running_pct', { pct: U.pct(0.11) }));
  // the cancel dialogs say what is kept and what a retry does (per stage
  // since R79 H1: only the transcription and the translation go on)
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    for (const text of [t('common.lectures.cancel_message', { title: 'X' }), t('common.lectures.cancel_message_many'), t('common.stage_cancel.message_asr'), t('common.stage_cancel.message_translate')]) {
      assert.ok(text.includes(`“${t('common.retry')}”`), `${loc}: names the retry: ${text}`);
      assert.ok(loc === 'en' ? /kept/.test(text) && /(from where (it|they) stopped|only the others)/.test(text) : text.includes('会保留') && /接着|只翻其余/.test(text), `${loc}: ${text}`);
    }
  }
  await I.setLocale('zh-CN');

  // the lecture page: a translation that goes on says what is already there
  const tr = lectureAt('G5', run('translate', { progress: 0.2, attempt: 1 }), { counts: { sentences: 383, zh_ok: 122, patches_pending: 0 } });
  const m = await mount(tr);
  const status = m.page.querySelector('.lec-status').textContent;
  assert.ok(status.includes(t('lecture.page.translate_have', { summary: t('common.summary.running', { stage: U.stageName('translate') }), n: U.count(122) })), status);
  m.done();

  // G3: the transcript's empty state follows the transcription's progress
  globalThis.IntersectionObserver ??= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
  const asrRun = lectureAt('G3t', run('asr', { progress: 0.11 }));
  const m2 = await mount(asrRun, { tab: 'transcript' });
  await settle();
  const empty = () => (m2.page.querySelector('div.tx-empty') || { textContent: '' }).textContent;
  assert.ok(empty().includes(U.pct(0.11)), `the empty state at first: ${empty()}`);
  m2.view.onJob({ scope: 'lecture', scope_id: 'G3t', stage: 'asr', status: 'running', progress: 0.21 });
  assert.ok(empty().includes(U.pct(0.21)), `then it follows: ${empty()}`);
  m2.done();
}

// ---- T25 S2, T22 G2: about how long it still takes; what closing does ----

{
  const t0 = clock;
  const k = 'eta|asr|1';
  P.noteShare(k, 0, t0);
  assert.equal(P.etaMs(k, t0), null, 'no share yet');
  P.noteShare(k, 0.1, t0);
  P.noteShare(k, 0.15, t0 + 10000);
  assert.equal(P.etaMs(k, t0 + 10000), null, 'too soon to tell');
  P.noteShare(k, 0.2, t0 + 30000);
  assert.equal(P.etaMs(k, t0 + 30000), 240000, '0.1 in 30 s, 0.8 left: 4 minutes');
  assert.equal(P.etaText(P.etaMs(k, t0 + 30000)), t('common.pipe.eta_m', { n: 4 }));
  assert.equal(P.etaMs(k, t0 + 90000), 180000, 'counted down without new shares');
  assert.equal(P.etaText(30000), t('common.pipe.eta_soon'));
  assert.equal(P.etaText(3 * 3600000 + 5 * 60000), t('common.pipe.eta_hm', { h: 3, m: 5 }));
  assert.equal(P.etaText(null), '');
  // a run that goes on from a checkpoint jumps there: measured from there
  const k2 = 'eta|asr|2';
  P.noteShare(k2, 0.49, t0);
  P.noteShare(k2, 0.55, t0 + 30000);
  assert.equal(P.etaMs(k2, t0 + 30000), 225000);
  // a share that went back (another run): a new start
  P.noteShare(k2, 0.1, t0 + 40000);
  assert.equal(P.etaMs(k2, t0 + 40000), null);

  // the card: after the time it has run
  const st = stage('asr', 'running', { progress: 0.1, started_at: new Date(clock - 60000).toISOString() });
  const bar = P.pipelineBar([stage('prep', 'done'), st], { key: 's2', lecture: 'S2', sys: () => null, res: () => null });
  clock += 30000;
  bar.setProgress('asr', 0.2);
  const lineOf = (cls) => {
    const l = bar.querySelector(`.${cls}`);
    return l.style.display === 'none' ? 'hidden' : l.querySelector('.card-v').textContent;
  };
  assert.equal(lineOf('card-time'), t('common.pipe.elapsed', { time: U.fmtDuration(90000) }), 'the time it has run, as before');
  assert.equal(lineOf('card-eta'), t('common.pipe.eta_m', { n: 4 }), 'then the time left');
  const fresh = P.pipelineBar([stage('prep', 'done'), stage('asr', 'running', { progress: 0.1, started_at: new Date(clock).toISOString() })], { key: 's2b', lecture: 'S2b', sys: () => null, res: () => null });
  assert.equal(fresh.querySelector('.card-eta').style.display, 'none', 'not known yet: hidden');

  // the lecture page: in its status, and a note on what closing does
  const lec = lectureAt('S2p', stage('asr', 'running', { progress: 0.1, started_at: new Date(clock - 60000).toISOString() }));
  const m = await mount(lec);
  clock += 30000;
  m.view.onJob({ scope: 'lecture', scope_id: 'S2p', stage: 'asr', status: 'running', progress: 0.2 });
  const status = m.page.querySelector('.lec-status').textContent;
  assert.ok(status.includes(t('common.pipe.eta_m', { n: 4 })), status);
  const note = notice(m.page, 'is-background');
  assert.ok(note && note.textContent === t('lecture.page.background_browser'), note && note.textContent);
  assert.equal(note.getAttribute('role'), 'note');
  m.done();
  // nothing runs or waits: no note; a wait with its own notice: that one only
  const m2 = await mount(lecture('S2d', { stages: names.map((n) => stage(n, 'done')) }));
  assert.equal(notice(m2.page, 'is-background'), null);
  m2.done();
  const m3 = await mount(lectureAt('S2w', stage('translate', 'queued', { waiting_for: 'budget' })));
  assert.ok(notice(m3.page, 'is-wait'));
  assert.equal(notice(m3.page, 'is-background'), null);
  m3.done();
  const m4 = await mount(lectureAt('S2q', stage('summary', 'queued', { waiting_for: 'api_slot' })));
  assert.ok(notice(m4.page, 'is-background'), 'queued: the note');
  m4.done();
}

// ---- T25 X1 (the lecture page's part): "更新" says what it runs again ----

{
  const stale = lecture('X1', {
    lecture_actions: ['update'],
    stale: { reasons: [{ code: 'glossary_updated', params: { from: 0, to: 1 } }], stages: ['repair', 'translate'] },
    stages: names.map((n) => stage(n, n === 'repair' ? 'stale' : n === 'translate' || n === 'export' ? 'done' : 'done')),
  });
  const m = await mount(stale);
  const b = m.page.querySelector('button[data-fkey="lecd:X1:update"]');
  assert.ok(b, 'the stale notice has 更新');
  assert.equal(b.getAttribute('title'), t('lecture.page.update_tip', { stages: U.listJoin([U.stageName('repair'), U.stageName('translate')]) }));
  m.done();
  // without the stale stages listed: the plain text, on the header's button
  const m2 = await mount(lecture('X1b', { lecture_actions: ['update'], stages: names.map((n) => stage(n, 'done')) }));
  const b2 = m2.page.querySelector('button[data-fkey="lecd:X1b:update"]');
  assert.ok(b2 && b2.getAttribute('title') === t('lecture.page.update_tip_plain'), b2 && b2.getAttribute('title'));
  m2.done();
}

// ---- R72 M1: where there is no "换模型重试…", Settings is still a way out ----

{
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    for (const code of ['llm.refusal', 'llm.time_limit']) {
      const text = U.errorInfoText(llmInfo(code, { seconds: 600 }));
      assert.ok(loc === 'en' ? /in Settings/.test(text) : text.includes(`在设置的“${t('settings.block.roles')}”中为该角色`), `${loc} ${code}: the Settings way too: ${text}`);
    }
  }
  await I.setLocale('zh-CN');
}

// ---- R72 M2: "in the list" by the back end's rule (case, dated aliases) ----

{
  // the cases of internal/api/v2_role_models_test.go TestModelListed
  const ds = ['deepseek-flash', 'deepseek-v4-pro'];
  const claude = ['claude-sonnet-4-5-20250929', 'claude-opus-4-1-20250805', 'claude-haiku-4-5'];
  for (const [name, list, want] of [
    ['deepseek-flash', ds, true],
    [' DeepSeek-Flash ', ds, true],
    ['deepseek-chat', ds, false],
    ['deepseek', ds, false],
    ['claude-sonnet-4-5', claude, true],
    ['claude-sonnet-4-5-latest', claude, true],
    ['claude-sonnet-4-5-20250929', claude, true],
    ['claude-sonnet-4', claude, false],
    ['claude-haiku-4-5', claude, true],
    ['claude-haiku-4', claude, false],
    ['claude-opus-4', claude, false],
    ['anything', null, false],
  ]) assert.equal(U.modelListed(name, list), want, `modelListed(${name})`);
  // an alias or another case is not reported as retired
  const providers = [{ id: 'an', name: 'Anthropic', models: claude }];
  const st = (model) => stage('translate', 'failed', { error_info: llmInfo('llm.rejected', { status: 400, provider: 'Anthropic', model }), model: { provider_id: 'an', model } });
  assert.equal(U.unlistedModel(st('claude-sonnet-4-5'), providers), null, 'a dated alias');
  assert.equal(U.unlistedModel(st('Claude-Haiku-4-5'), providers), null, 'another case');
  assert.deepEqual(U.unlistedModel(st('claude-sonnet-4'), providers), { model: 'claude-sonnet-4', provider: 'Anthropic' });
}

// ---- R72 M3: no "click retry" for the whole automatic retry, not only its countdown ----

{
  const info = { code: 'llm.server_error', params: { role: 'summary', retries: 3, status: 503 } };
  const click = '点击重试';
  // the countdown is over: queued for a slot, then running the next attempt
  for (const st of [
    stage('summary', 'queued', { error: 'raw', error_info: info, waiting_for: 'api_slot' }),
    stage('summary', 'queued', { error: 'raw', error_info: info }),
    stage('summary', 'running', { error: 'raw', error_info: info, progress: 0.2 }),
  ]) {
    const text = U.stageErrorText(st);
    assert.ok(text.includes('HTTP 503') && !text.includes(click), `${st.status} ${st.waiting_for || ''}: ${text}`);
    const bar = P.pipelineBar([st], { key: 'm3', sys: () => null, res: () => null });
    const msg = bar.querySelector('.card-errmsg').textContent;
    assert.ok(!msg.includes(click), `the card: ${msg}`);
  }
  // a final failure still says it
  assert.ok(U.stageErrorText(stage('summary', 'failed', { error: 'raw', error_info: info })).includes(click));
}

// ---- R72 M4: the total comes first in its line (a cut line keeps the number) ----

{
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    for (const text of [t('lecture.stat.tokens_total_sub', { input: 'IIII', output: 'OOOO' }), t('lecture.stat.cost_total_sub', { total: 'IIII' })]) {
      const at = text.indexOf('IIII');
      assert.ok(at >= 0 && at <= 6, `${loc}: the number first: ${text}`);
    }
  }
  await I.setLocale('zh-CN');
}

// ---- R72 L3: "技术详情" stays open, and keeps the focus, when the card is drawn again ----

{
  const failed = () => stage('translate', 'failed', { error: 'LLM 请求被拒绝（translate，HTTP 401）', error_info: llmInfo('llm.rejected', { status: 401 }), allowed_actions: ['retry'] });
  const draw = () => P.pipelineBar([failed()], { key: 'l3', sys: () => null, res: () => null, onAction: () => Promise.resolve(null) });
  const bar = draw();
  const det = bar.querySelector('details.card-errdetail');
  const sum = det.querySelector('summary');
  assert.equal(sum.dataset.fkey, 'l3:details:translate', 'the summary has a focus key (dom.js preserveFocus finds it again)');
  assert.ok(!det.open, 'closed at first');
  det.open = true;
  det.dispatchEvent(event('toggle'));
  const again = draw();
  assert.ok(again.querySelector('details.card-errdetail').open, 'drawn again: still open');
  const d3 = again.querySelector('details.card-errdetail');
  d3.open = false;
  d3.dispatchEvent(event('toggle'));
  assert.ok(!draw().querySelector('details.card-errdetail').open, 'closed again: drawn closed');
}

// ---- R72 L5: Settings sections quoted in en; the budget's words ----

{
  await I.setLocale('en');
  assert.ok(t('common.pipe.model_unlisted', { model: 'm', provider: 'p' }).includes('under “Roles”'));
  assert.ok(t('lecture.notice.budget_wait').includes('under “Storage and other”'));
  assert.ok(t('lecture.notice.budget_wait').startsWith("Today's budget is used up."), t('lecture.notice.budget_wait'));
  assert.ok(/^[A-Z][a-z]+ /.test(t('lecture.notice.budget_settings')) && t('lecture.notice.budget_settings') !== 'Daily budget', 'a verb, as the zh-CN button');
  await I.setLocale('zh-CN');
  assert.ok(t('lecture.notice.budget_wait').startsWith('今日预算已用完'), 'as resources.budget_hit says it');
  assert.ok(t('common.pipe.model_unlisted', { model: 'm', provider: 'p' }).includes('名称有误'), 'a wrong name gives the same answer');
}

// ---- R72 L1: the total is asked for on a full refresh, after a failed read, and for a late "done" ----

{
  const Api = await import(new URL('api.js', base).href);
  const realLecture = Api.api.lecture;
  const asked = [];
  let fail = false;
  const lec = lecture('L1t', { stages: names.map((n) => stage(n, n === 'translate' ? 'done' : n === 'export' ? 'running' : 'done', { progress: 0.5 })) });
  Api.api.lecture = async (lid, opts = {}) => {
    asked.push(!!opts.usageTotal);
    await settle(5);
    if (fail) throw new Api.ApiError(500, 'boom', {});
    return { ...lec };
  };
  try {
    const m = await mount(lec);
    assert.deepEqual(asked, [true]);
    // a full refresh (the connection back): asks for it
    m.view.refresh();
    await settle(100);
    assert.deepEqual(asked, [true, true], 'a full refresh asks for the total');
    // a read with the total fails: the next read asks again
    m.view.onJob({ scope: 'lecture', scope_id: 'L1t', stage: 'export', status: 'done', progress: 1 });
    fail = true;
    await settle(400);
    fail = false;
    m.view.onChanged({ kind: 'lecture', id: 'L1t' });
    await settle(400);
    assert.deepEqual(asked, [true, true, true, true], 'after a failed read, the next one asks again');
    // a "done" event for a stage the page already shows done (read before
    // the event came): still a read with the total
    m.view.onJob({ scope: 'lecture', scope_id: 'L1t', stage: 'translate', status: 'done', progress: 1 });
    await settle(400);
    assert.deepEqual(asked, [true, true, true, true, true], 'a late "done" asks for it');
    m.done();
  } finally {
    Api.api.lecture = realLecture;
  }
}

// ---- R79 H1: a stage's cancel dialog says what a retry does for that stage ----

{
  // checked against the back end: asr continues from its checkpoint (same
  // device and model, on this computer); translate translates only the
  // sentences without a translation (translateTodo); repair writes at the
  // end and summary is one request, so they start over and are billed
  // again, as the course's LLM stages; preprocessing and export start over
  assert.equal(P.cancelText('asr'), t('common.stage_cancel.message_asr'));
  assert.equal(P.cancelText('translate'), t('common.stage_cancel.message_translate'));
  for (const s of ['summary', 'repair', 'glossary_seed', 'glossary_review', 'material_terms']) assert.equal(P.cancelText(s), t('common.stage_cancel.message_llm'), s);
  for (const s of ['prep', 'export', 'material_extract']) assert.equal(P.cancelText(s), t('common.stage_cancel.message'), s);
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    const llm = t('common.stage_cancel.message_llm');
    const plain = t('common.stage_cancel.message');
    assert.ok(loc === 'en' ? /start over/.test(llm) && /billed again/.test(llm) : llm.includes('从头') && llm.includes('重新计费'), `${loc} llm: ${llm}`);
    assert.ok(loc === 'en' ? /start (this stage )?over/.test(plain) && !/billed/.test(plain) : plain.includes('从头') && !plain.includes('计费'), `${loc} plain: ${plain}`);
    for (const k of ['common.stage_cancel.message', 'common.stage_cancel.message_llm']) assert.ok(!/接着上次|from where it stopped/.test(t(k)), `${loc} ${k} promises no continuation`);
    // the lecture's cancel: what continues and what starts over, not "all is kept"
    for (const text of [t('common.lectures.cancel_message', { title: 'X' }), t('common.lectures.cancel_message_many')]) {
      assert.ok(loc === 'en' ? /start over/.test(text) && /billed again/.test(text) : text.includes('从头') && text.includes('重新计费'), `${loc}: ${text}`);
    }
  }
  await I.setLocale('zh-CN');
  // the dialog shows it
  const asked = P.confirmStageAction('repair', 'cancel');
  await settle(20);
  const dlg = dialogs.filter((d) => d.open).at(-1);
  assert.ok(dlg && dlg.textContent.includes(t('common.stage_cancel.message_llm')), 'the dialog says it');
  dlg.querySelectorAll('button').find((b) => b.textContent === t('common.dont_cancel')).click();
  dlg.open = false;
  await asked;
}

// ---- R79 M4, L2, L3: the time left when the progress stalls; remote runs; hours ----

{
  const t0 = clock;
  const k = 'eta|m4|1';
  P.noteShare(k, 0.1, t0);
  P.noteShare(k, 0.2, t0 + 30000);
  assert.equal(P.etaMs(k, t0 + 30000), 240000);
  assert.equal(P.etaMs(k, t0 + 30000 + 250000), null, 'the countdown ran out: no estimate (not "less than a minute" for ever)');
  assert.equal(P.etaMs(k, t0 + 30000 + 130000), null, 'no share for over two minutes: stalled, no estimate');
  assert.equal(P.etaMs(k, t0 + 30000 + 100000), 140000, 'before that: counted down');
  P.noteShare(k, 0.3, t0 + 300000);
  assert.ok(P.etaMs(k, t0 + 300000) > 0, 'moving again: estimated again');
  // a remote transcription has its own time left (its card's steps): none here
  const remote = stage('asr', 'running', { progress: 0.3, started_at: new Date(clock).toISOString(), stats: { remote: { step: 'remote_transcribe', steps: [] } } });
  const opts = { lecture: 'M4r' };
  P.noteShare(P.runKey(remote, opts), 0.1, t0);
  P.noteShare(P.runKey(remote, opts), 0.3, t0 + 30000);
  assert.equal(P.stageEta(remote, opts, t0 + 30000), '', 'remote: one time left only');
  // an hour or more: in 5 minutes, no "0 分钟"
  assert.equal(P.etaText(60 * 60000), t('common.pipe.eta_h', { n: 1 }));
  assert.equal(P.etaText(61 * 60000), t('common.pipe.eta_hm', { h: 1, m: 5 }));
  assert.equal(P.etaText(118 * 60000), t('common.pipe.eta_h', { n: 2 }));
  assert.equal(P.etaText(59 * 60000), t('common.pipe.eta_m', { n: 59 }));
}

// ---- R76 M-1: the two-choice model dialog says when translations are cleared ----

{
  const A = await import(new URL('actions.js', base).href);
  const an = { label: 'Anthropic / claude-x', provider_id: 'an', provider_name: 'Anthropic', model: 'claude-x' };
  const ds = { label: 'DeepSeek / m', provider_id: 'ds', provider_name: 'DeepSeek', model: 'm' };
  settingsBody = { providers: [{ id: 'an', name: 'Anthropic', base_url: 'https://api.anthropic.com', key_hint: 'sk-…abcd' }, { id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com', key_hint: 'sk-…abcd' }], roles: [] };
  answer = (m, u) => (u.startsWith('/api/settings') ? { status: 200, body: settingsBody } : { status: 200, body: {} });
  const warnOf = async (diff, what, opts) => {
    const asked = A.modelChoiceDialog(diff, what, opts);
    await settle(40);
    const dlg = dialogs.filter((d) => d.open).at(-1);
    const warn = dlg.querySelectorAll('p').filter((p) => p.className.includes('dlg-warn')).map((p) => p.textContent);
    dlg.dispatchEvent(event('cancel'));
    dlg.open = false;
    await asked;
    return warn;
  };
  const tr = { stage: 'translate', snapshot: an, current: ds, snapshot_usable: true };
  const sum = { stage: 'summary', snapshot: an, current: ds, snapshot_usable: true };
  // a retry whose diff has the translation: by default, the warning of the single retry
  assert.deepEqual(await warnOf([tr], 'retry'), [t('common.model_choice.translate_clears')]);
  assert.deepEqual(await warnOf([{ ...tr, lecture_id: 'L1', lecture_title: 'One' }, { ...tr, lecture_id: 'L2', lecture_title: 'Two' }], 'retry'), [t('common.model_choice.translate_clears_many')], 'several lectures');
  // no translation in it, or not a retry: none
  assert.deepEqual(await warnOf([sum], 'retry'), []);
  assert.deepEqual(await warnOf([tr], 'update'), []);
  // the caller's own sentence, or none
  assert.deepEqual(await warnOf([sum], 'retry', { note: 'NOTE' }), ['NOTE']);
  assert.deepEqual(await warnOf([tr], 'retry', { note: '' }), []);
  settingsBody = { providers: [], roles: [] };
}

// ---- R79 L11 (test gaps) and R72 L3 (the notice drawn once with what the settings tell) ----

{
  // m2b: a segment focused otherwise than by an arrow key (a click, the
  // focus put back) takes the Tab stop
  const bar = P.pipelineBar(names.map((n) => stage(n, n === 'asr' ? 'failed' : 'done', { error: n === 'asr' ? 'x' : null })), { key: 'gap', sys: () => null, res: () => null });
  body.append(bar);
  const segs = bar.querySelectorAll('button.seg');
  assert.ok(segs[1].getAttribute('tabindex') === '0', 'the failed stage at first');
  segs[4].focus();
  assert.ok(segs[4].getAttribute('tabindex') === '0' && segs.filter((b) => b.getAttribute('tabindex') === '0').length === 1, 'the focused segment takes it');
  bar.remove();
  body.focus();
  // m5d: the desktop app's note and the browser's
  assert.equal(Lec.backgroundNoteKey(true), 'lecture.page.background_desktop');
  assert.equal(Lec.backgroundNoteKey(false), 'lecture.page.background_browser');
  // m7c: a device not present is no device on mains power either
  const res = (devices) => ({ devices });
  assert.equal(U.acDeviceUsable(res([{ kind: 'cuda', present: false, enabled: true }])), false);
  assert.equal(U.acDeviceUsable(res([{ kind: 'igpu', present: true, enabled: true }])), true);
  assert.equal(U.acDeviceUsable(res([{ kind: 'npu', present: true, enabled: true }])), false, 'the NPU is not in the AC chain');

  // R72 L3: a failure that may be an unlisted model is drawn once the
  // settings are read, not first with "打开设置" first and then again
  // with "换模型重试…" first (an alert announced twice, a button moving)
  const providers = [{ id: 'deepseek', name: 'DeepSeek', models: ['deepseek-flash'] }];
  settingsBody = { providers, roles: [] };
  let release;
  const gate = new Promise((r) => (release = r));
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    if (String(url).startsWith('/api/settings')) await gate;
    return realFetch(url, init);
  };
  try {
    const st = stage('translate', 'failed', { error: 'raw', error_info: llmInfo('llm.request_failed', { status: 404, provider: 'DeepSeek', model: 'deepseek-chat' }), model: { provider_id: 'deepseek', model: 'deepseek-chat' }, allowed_actions: ['retry'] });
    const page = new El('main');
    body.append(page);
    answer = (m, u) => (u.startsWith('/api/settings') ? { status: 200, body: settingsBody } : m === 'GET' && u.startsWith('/api/lectures/L3b') ? { status: 200, body: lectureAt('L3b', st) } : { status: 200, body: {} });
    const view = Lec.mountLecture(page, { id: 'L3b', tab: 'review' }, { system: () => null, resources: () => null, courses: () => [{ id: 'C1', code: 'CSC1', name: 'Course' }], setCurrentCourse() {}, subscribe: () => () => {} });
    await settle(60);
    assert.equal(page.querySelector('.lec-notice.is-failed'), null, 'not drawn while the settings are read');
    release();
    await settle(60);
    const n = page.querySelector('.lec-notice.is-failed');
    assert.ok(n && isPrimary(n.querySelector('button[data-fkey="lecd:L3b:retry_model:translate"]')), 'drawn once, with 换模型重试… first');
    view.destroy();
    page.remove();
  } finally {
    globalThis.fetch = realFetch;
    settingsBody = { providers: [], roles: [] };
  }
}

// ---- R79 low: power texts, the translation's status, the background note, wording ----

{
  // L1: a lecture pinned to the Intel GPU waits as well: no device named;
  // en says "graphics card" for the NVIDIA one (glossary)
  assert.ok(!t('lecture.notice.power_wait_ac').includes('NPU'), t('lecture.notice.power_wait_ac'));
  await I.setLocale('en');
  assert.ok(!t('lecture.notice.power_wait_ac').includes('NPU'));
  assert.ok(t('lecture.notice.power_no_device').includes('NVIDIA graphics card'), t('lecture.notice.power_no_device'));
  // L10
  assert.ok(t('lecture.notice.budget_wait').includes('sent automatically'), t('lecture.notice.budget_wait'));
  await I.setLocale('zh-CN');
  assert.ok(t('lecture.stat.cost_total_sub', { total: 'X' }).includes('大字为'));
  // L4: "正在翻译 · 已有 N 句译文", not "…，正在翻译其余的" after "正在翻译"
  const have = t('lecture.page.translate_have', { summary: 'S', n: 3 });
  assert.ok(!have.includes('其余'), have);
  // L10: only the export out of date: no model is called
  const m = await mount(lecture('LX', { lecture_actions: ['update'], stale: { reasons: [{ code: 'input_changed' }], stages: ['export'] }, stages: names.map((n) => stage(n, n === 'export' ? 'stale' : 'done')) }));
  assert.equal(m.page.querySelector('button[data-fkey="lecd:LX:update"]').getAttribute('title'), t('lecture.page.update_tip_export'));
  m.done();
  // L5: no "goes on in the background" while it waits for the user
  for (const w of ['gpu_paused', 'device_paused:cuda:0', 'component_missing:cuda:0', 'model_missing:ov:NPU', 'no_asr_device', 'no_provider', 'remote_auth', 'remote_setup']) {
    const mw = await mount(lectureAt('L5w', stage('asr', 'queued', { waiting_for: w })));
    assert.equal(notice(mw.page, 'is-background'), null, `${w}: no background note`);
    mw.done();
  }
}

// ---- R79b M4: the time left beside the lecture's status follows the clock ----

{
  const D = await import(new URL('dom.js', base).href);
  // n3b: the countdown ran out (within the two minutes): no estimate
  const k = 'eta|n3b';
  P.noteShare(k, 0.1, clock);
  P.noteShare(k, 0.9, clock + 30000);
  assert.ok(P.etaMs(k, clock + 30000) > 0);
  assert.equal(P.etaMs(k, clock + 40000), null, 'ran out after 10 s of no progress: none, not "less than a minute"');
  // the status: drawn with the estimate, then the progress stalls
  const lec = lectureAt('M4p', stage('asr', 'running', { progress: 0.1, started_at: new Date(clock - 60000).toISOString() }));
  const before = D.ticking().nodes;
  const m = await mount(lec);
  clock += 30000;
  m.view.onJob({ scope: 'lecture', scope_id: 'M4p', stage: 'asr', status: 'running', progress: 0.2 });
  const status = () => m.page.querySelector('.lec-status').textContent;
  assert.ok(status().includes(t('common.pipe.eta_m', { n: 4 })), status());
  assert.ok(D.ticking().nodes > before, 'the status has a timer while a stage runs');
  const head = m.page.querySelector('.lec-head').children[0];
  clock += 100000;
  second();
  assert.ok(status().includes(t('common.pipe.eta_m', { n: 3 })), `counted down: ${status()}`);
  assert.ok(m.page.querySelector('.lec-head').children[0] === head, 'the header is not drawn again (only the text changes)');
  clock += 150000;
  second();
  assert.ok(!status().includes(t('common.pipe.eta_soon')) && !/约还需/.test(status()), `stalled: no estimate: ${status()}`);
  m.done();
  second();
  second();
  assert.ok(D.ticking().nodes <= before, 'the page left: its timer is gone');
}

// ---- R79b low: texts that promise only what holds; no "clears the translations" for prep / asr ----

{
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    const asr = t('common.stage_cancel.message_asr');
    assert.ok(loc === 'en' ? /Colab/.test(asr) && /precision/.test(asr) : asr.includes('Colab') && asr.includes('精度'), `${loc}: remote and precision start over: ${asr}`);
    const res = t('common.status.running_resuming');
    assert.ok(loc === 'en' ? /may continue/.test(res) : res.includes('可能'), `${loc}: no promise: ${res}`);
  }
  await I.setLocale('zh-CN');
  assert.ok(t('common.model_choice.translate_clears_many').includes(t('common.model_choice.current')), 'the many form names the choice that clears');
  // a prep or asr retry's model dialog: no "clears the translations"
  const A = await import(new URL('actions.js', base).href);
  const an = { label: 'Anthropic / claude-x', provider_id: 'an', provider_name: 'Anthropic', model: 'claude-x' };
  const ds = { label: 'DeepSeek / m', provider_id: 'ds', provider_name: 'DeepSeek', model: 'm' };
  const diff = [{ stage: 'translate', snapshot: an, current: ds, snapshot_usable: true }];
  answer = (m, u) => (m === 'POST' ? { status: 409, body: { error: { code: 'needs_model_choice' }, diff } } : u.startsWith('/api/settings') ? { status: 200, body: { providers: [], roles: [] } } : { status: 200, body: {} });
  const warnFor = async (st) => {
    const done = A.stageAction(lecture('NR'), st, 'retry', true);
    await settle(60);
    const dlg = dialogs.filter((d) => d.open).at(-1);
    const warn = dlg.querySelectorAll('p').filter((p) => p.className.includes('dlg-warn')).length;
    dlg.dispatchEvent(event('cancel'));
    dlg.open = false;
    await done;
    return warn;
  };
  assert.equal(await warnFor('asr'), 0, 'asr: none');
  assert.equal(await warnFor('prep'), 0, 'prep: none');
  assert.equal(await warnFor('export'), 1, 'export: as before');
}

console.log('r7 lecture checks: ok');
process.exit(0);
