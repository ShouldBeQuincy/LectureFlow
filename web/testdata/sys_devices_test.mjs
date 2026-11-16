// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// The diagnostics page's device card (view_system.js), r4:
//   - the device table's sideways-scrolling wrapper is in the Tab order,
//     a region named by the card's title, and keeps the focus when the page
//     is drawn anew (preserveFocus finds it by its fkey; R48 M1);
//   - a VRAM hint and its extra sentence are joined by the text's end mark:
//     nothing after a full-width stop (zh-CN), one space otherwise (en, hi;
//     R48 S1).
// Rendered on the DOM stand-in of ui320_test.mjs (trimmed); fetch answers
// the catalog files only and fails the test on anything else. Nodes are
// compared by identity (assert.ok(a === b)). web/sys_dev_scroll_test.go
// starts it with the web directory as a file: URL.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

// ---- DOM stand-in ----

class Node {
  constructor() {
    this.parentNode = null;
    this.listeners = {};
  }
  get isConnected() {
    let n = this;
    while (n.parentNode) n = n.parentNode;
    return n === root;
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
  // events bubble up to the document (the html element's parent here)
  dispatchEvent(ev) {
    ev.target ??= this;
    for (let n = this; n && !ev.stopped; n = n === root ? doc : n.parentNode) {
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
    for (; n; n = n.parentNode) if (n === this) return true;
    return false;
  }
  focus() {
    const prev = doc.activeElement;
    if (prev === this) return;
    doc.activeElement = this;
    if (prev) prev.dispatchEvent(event('focusout', { relatedTarget: this }));
    this.dispatchEvent(event('focusin', { relatedTarget: prev }));
  }
  getBoundingClientRect() {
    return { top: 0, bottom: 0, left: 0, right: 0 };
  }
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
  removeEventListener() {},
  querySelector: (sel) => root.querySelector(sel),
  querySelectorAll: (sel) => root.querySelectorAll(sel),
  getElementById: () => null,
  createElement: (t) => new El(t),
  createElementNS: (_, t) => new El(t),
  createTextNode: (s) => new Text(s),
  visibilityState: 'visible',
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
});
globalThis.window = globalThis;

const base = process.argv[2];
let otherFetches = 0;
globalThis.fetch = async (url) => {
  const u = String(url);
  if (!u.startsWith('i18n/')) {
    otherFetches++;
    throw new Error('unexpected fetch ' + u);
  }
  const cat = JSON.parse(await readFile(new URL(u, base), 'utf8'));
  return { ok: true, status: 200, json: async () => cat, text: async () => JSON.stringify(cat) };
};

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const S = await import(new URL('view_system.js', base).href);
const U = await import(new URL('util.js', base).href);
await I.setLocale('zh-CN');

// ---- the table's wrapper: focusable, named, keeps the focus ----
{
  const page = new El('div');
  body.append(page);
  const draw = () => {
    const wrap = S.deviceTableWrap(D.h('table', { class: 'sys-dev-table' }));
    page.replaceChildren(wrap);
    return wrap;
  };
  const first = draw();
  assert.equal(first.getAttribute('tabindex'), '0', 'in the Tab order');
  assert.equal(first.getAttribute('role'), 'region');
  assert.equal(first.getAttribute('aria-labelledby'), 'sys-devices-title', 'named by the card title');
  assert.equal(first.getAttribute('aria-label'), null, 'no name of its own');
  assert.equal(first.dataset.fkey, S.DEVICE_TABLE_FKEY);
  assert.equal(S.DEVICE_TABLE_FKEY, 'sys:dev:table');
  first.focus();
  assert.ok(doc.activeElement === first, 'focused');
  // the page drawn anew (system info, resources, servers changed)
  let second = null;
  D.preserveFocus(() => {
    second = draw();
  });
  assert.ok(second !== null && second !== first, 'a new wrapper');
  assert.ok(!first.isConnected, 'the old one is gone');
  assert.ok(doc.activeElement === second, 'the focus stays on the table wrapper after a re-render');
  page.remove();
  body.focus?.();
}

// ---- VRAM hint + extra sentence ----
const hint = {
  code: 'system.devices.vram_low',
  params: {
    vram_gb: 4,
    model: 'large-v3',
    need_gb: 6,
    suggest: 'small',
    extra: [{ code: 'system.devices.vram_queue', params: { per_job_mb: 3000, reserve_mb: 1024 } }],
  },
};
for (const [loc, sep] of [
  ['zh-CN', ''],
  ['en', ' '],
  ['hi', ' '],
]) {
  await I.setLocale(loc);
  const main = I.t('system.devices.vram_low', { vram_gb: U.fmtInt(4), model: 'large-v3', need_gb: U.fmtInt(6), suggest: 'small' });
  const extra = I.t('system.devices.vram_queue', { per_job_mb: U.fmtInt(3000), reserve_mb: U.fmtInt(1024) });
  assert.ok(main && extra && !main.includes('{') && !extra.includes('{'), `${loc}: both texts`);
  const got = S.vramHintText(hint);
  assert.equal(got, main + sep + extra, `${loc}: the hint's two sentences`);
  if (loc === 'zh-CN') assert.ok(!/[。！？] /u.test(got), `zh-CN: no space after a full stop: ${got}`);
  // the main sentence alone
  assert.equal(S.vramHintText({ code: 'system.devices.vram_low', params: { vram_gb: 4, model: 'large-v3', need_gb: 6, suggest: 'small' } }), main, `${loc}: one sentence`);
}

// ---- r6 (T043): the model the card is too small for is not installed ----
// Two hints of their own (the card's recommendation installed / still to
// install), shown in the warning style with the link to the transcription
// settings; the one that says to install a model also links to Components.
{
  const params = { vram_gb: 4, model: 'large-v3', need_gb: 6, suggest: 'large-v3-turbo' };
  const shown = { vram_gb: U.fmtInt(4), model: 'large-v3', need_gb: U.fmtInt(6), suggest: 'large-v3-turbo' };
  // the sentence the server may still add to these two (R67: never vram_queue, the one about lowering the reserve)
  const queue = {
    code: 'system.devices.vram_queue_switch',
    params: { per_job_mb: 5000, reserve_mb: 1000, avail_mb: 3396, suggest: 'large-v3-turbo', suggest_precision: 'int8_float16', suggest_mb: 1600 },
  };
  const queueShown = { ...queue.params, per_job_mb: U.fmtInt(5000), reserve_mb: U.fmtInt(1000), avail_mb: U.fmtInt(3396), suggest_mb: U.fmtInt(1600) };
  const hrefs = (row) => row.querySelectorAll('a').map((a) => `${a.getAttribute('href')} ${a.dataset.fkey}`);
  for (const [loc, sep] of [
    ['zh-CN', ''],
    ['en', ' '],
    ['hi', ' '], // not translated yet: the fallback language's text
  ]) {
    await I.setLocale(loc);
    const texts = [];
    for (const [code, links] of [
      ['system.devices.vram_low_missing', ['#/settings/asr sys:vram:cuda:0']],
      ['system.devices.vram_low_missing_install', ['#/settings/asr sys:vram:cuda:0', '#/settings/components sys:vramcomp:cuda:0']],
    ]) {
      const main = I.t(code, shown);
      assert.ok(main && !main.includes(code) && !main.includes('{'), `${loc} ${code}: a text with every parameter filled: ${main}`);
      assert.ok(main.includes('large-v3-turbo') && main.split('large-v3').length - main.split('large-v3-turbo').length >= 1, `${loc} ${code}: names the model and the suggestion`);
      texts.push(main);
      assert.equal(S.vramHintText({ code, params }), main, `${loc} ${code}: one sentence`);
      const extra = I.t(queue.code, queueShown);
      assert.ok(extra && !extra.includes('{'), `${loc}: the switch sentence`);
      assert.equal(S.vramHintText({ code, params: { ...params, extra: [queue] } }), main + sep + extra, `${loc} ${code}: with the switch sentence`);
      const row = S.vramHintRow({ id: 'cuda:0', vram_hint: { code, params } });
      assert.ok(row !== null && row.className === 'sys-hint is-warn', `${loc} ${code}: a row in the warning style`);
      assert.equal(row.textContent.includes(main), true, `${loc} ${code}: the row shows the text`);
      assert.deepEqual(hrefs(row), links, `${loc} ${code}: the row's links`);
    }
    assert.notEqual(texts[0], texts[1], `${loc}: the two hints differ`);
  }
  // the earlier hints keep their one link; no hint, or a code this page does not know, draws nothing
  await I.setLocale('zh-CN');
  const low = S.vramHintRow({ id: 'cuda:0', vram_hint: { code: 'system.devices.vram_low', params } });
  assert.deepEqual(hrefs(low), ['#/settings/asr sys:vram:cuda:0'], 'vram_low: the settings link only');
  assert.ok(S.vramHintRow({ id: 'cuda:0' }) === null, 'no hint, no row');
  assert.ok(S.vramHintRow({ id: 'cuda:0', vram_hint: { code: 'system.devices.vram_newer', params } }) === null, 'an unknown code draws nothing');
}
assert.equal(otherFetches, 0);

console.log('sys devices checks: ok');
process.exit(0);
