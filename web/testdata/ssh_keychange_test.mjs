// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// SEC7 M1 / L1: the SSH server wizard when a host key needs a ticked box —
// a confirmed key that changed ("主机密钥已变化": both fingerprints, the
// warning announced and focused, the box, then trust with replace and
// previous) and a first key the user's own known_hosts disagrees with (the
// box, then a plain trust). Settings → 远程服务器 is rendered on a DOM
// stand-in (as in a11y_test.mjs) with the zh-CN catalog; fetch answers the
// API. web/ssh_keychange_test.go starts it with the web directory as a
// file: URL.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

// ---- DOM stand-in ----

class Node {
  constructor() {
    this.parentNode = null;
    this.listeners = {};
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
    for (let n = this; n && !ev.stopped; n = n.parentNode) {
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
  // boolean attributes reflected as properties (as a browser does)
  get disabled() {
    return 'disabled' in this.attrs;
  }
  set disabled(v) {
    if (v) this.attrs.disabled = '';
    else delete this.attrs.disabled;
  }
  get checked() {
    return 'checked' in this.attrs;
  }
  set checked(v) {
    if (v) this.attrs.checked = '';
    else delete this.attrs.checked;
  }
  get firstChild() {
    return this.children[0] || null;
  }
  removeChild(c) {
    c.remove();
    return c;
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
    doc.activeElement = this;
  }
  click() {
    // a disabled button does not fire (as in a browser)
    if (this.disabled) return;
    this.dispatchEvent(event('click'));
  }
  getBoundingClientRect() {
    return { top: 0, bottom: 0, left: 0, right: 0 };
  }
  matches(sel) {
    if (sel === ':focus-visible') return true;
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

const store = { getItem: () => null, setItem() {}, removeItem() {} };
const root = new El('html');
const body = new El('body');
root.append(body);
const doc = {
  documentElement: root,
  body,
  activeElement: body,
  addEventListener() {},
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
globalThis.requestAnimationFrame = (fn) => setTimeout(fn, 0);

const base = process.argv[2];

// fetch: the UI catalogs from the web directory; the API answered by
// `answer(method, path, body)`, every request recorded in `calls`
const calls = [];
let answer = () => ({ status: 404, body: { error: { code: 'api.not_found' } } });
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const cat = JSON.parse(await readFile(new URL(u, base), 'utf8'));
    const text = JSON.stringify(cat);
    return { ok: true, status: 200, json: async () => cat, text: async () => text };
  }
  const method = init.method || 'GET';
  const b = init.body ? JSON.parse(init.body) : undefined;
  calls.push({ method, path: u, body: b });
  await new Promise((r) => setTimeout(r, 2));
  const a = answer(method, u, b);
  const text = JSON.stringify(a.body);
  return { ok: a.status < 300, status: a.status, text: async () => text, json: async () => a.body, headers: { get: () => null } };
};
const settle = (ms = 60) => new Promise((r) => setTimeout(r, ms));

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const R = await import(new URL('settings_remote.js', base).href);
await I.setLocale('zh-CN');
const zh = JSON.parse(await readFile(new URL('i18n/zh-CN.json', base), 'utf8'));

const OLD = 'SHA256:0ldConfirmedKeyFingerprintForTheTest000000';
const NEW = 'SHA256:n3wPresentedKeyFingerprintForTheTest00000';
const CHECK = 'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub';
const changedServer = {
  id: 'gpu-box', device_id: 'ssh:gpu-box', name: 'gpu-box', host: 'gpu.example.com', port: 22, username: 'alice', state: 'host_key_changed',
  trusted: true, host_key_fingerprint: OLD, kit_ready: true, platform: 'linux-x64', settings: { group: 'prefer' },
  last_error: { code: 'remote.host_key_mismatch', params: { device: 'gpu-box', fingerprint: NEW } },
};
const freshServer = { id: 'lab', device_id: 'ssh:lab', name: 'lab', host: 'lab.example.org', port: 22, username: 'alice', state: 'untrusted', trusted: false, kit_ready: false, settings: {} };
const readyServer = { ...changedServer, state: 'ready', host_key_fingerprint: NEW, last_error: null };

const fk = (k) => doc.querySelector(`[data-fkey="${k}"]`);
const text = (n) => (n ? n.textContent : '');
const posts = (path) => calls.filter((c) => c.method === 'POST' && c.path === path);

// Settings → 远程服务器 as view_settings.js renders it (a section re-rendered
// whole, keeping the focus by data-fkey).
const shell = new El('section');
body.append(shell);
const ui = {
  head: (_, title, ...rest) => D.h('header', null, title, ...rest),
  segmented: () => new El('div'),
  formRow: () => new El('div'),
  switchBtn: () => new El('button'),
  rerender: () =>
    D.preserveFocus(() => {
      D.clear(shell);
      section.render(shell);
    }),
};

// ---- pure helpers ----
{
  assert.equal(R.keyNeedsCheck(null), false);
  assert.equal(R.keyNeedsCheck({ fingerprint: NEW }), false, 'a first key without a known_hosts verdict: one click');
  assert.equal(R.keyNeedsCheck({ fingerprint: NEW, known_hosts: 'match' }), false);
  assert.equal(R.keyNeedsCheck({ fingerprint: NEW, known_hosts: 'mismatch' }), true, 'L1: known_hosts disagrees');
  assert.equal(R.keyNeedsCheck({ fingerprint: NEW, changed: true }), true, 'M1: a confirmed key changed');
  assert.equal(R.presentedKey({}), null);
  assert.deepEqual(R.presentedKey({ host_key: { fingerprint: NEW } }), { fingerprint: NEW });
  assert.deepEqual(R.presentedKey({ host_key: { fingerprint: NEW }, changed: true, previous_fingerprint: OLD }), { fingerprint: NEW, changed: true, previous_fingerprint: OLD });
  assert.deepEqual(R.trustBody({ fingerprint: NEW }, 0), { hop: 0, fingerprint: NEW });
  assert.deepEqual(R.trustBody({ fingerprint: NEW, known_hosts: 'mismatch' }, 1), { hop: 1, fingerprint: NEW });
  assert.deepEqual(R.trustBody({ fingerprint: NEW, changed: true, previous_fingerprint: OLD }, 0), { hop: 0, fingerprint: NEW, replace: true, previous: OLD });
  // SEC39 L-A: the machine a hop's key belongs to (a jump host, or the server)
  const behind = { host: 'gpu.example.com', port: 22, username: 'alice', jumps: [{ host: 'jump.example.net', port: 2222, username: 'bob' }, { host: 'inner.example.net', port: 22, username: 'carol' }] };
  assert.equal(R.endpointText(R.hopEndpoint(behind, 0)), 'bob@jump.example.net:2222');
  assert.equal(R.endpointText(R.hopEndpoint(behind, 1)), 'carol@inner.example.net');
  assert.equal(R.endpointText(R.hopEndpoint(behind, 2)), 'alice@gpu.example.com');
  assert.equal(R.endpointText(R.hopEndpoint({ host: 'gpu.example.com', port: 22, username: 'alice' }, 0)), 'alice@gpu.example.com', 'no jump hosts: the server');
  assert.equal(R.endpointText(R.hopEndpoint({ host: 'gpu.example.com', port: 22, username: 'alice', jumps: [] }, undefined)), 'alice@gpu.example.com');
}

// ---- the error names the presented key: no "{fingerprint}" left ----
{
  const s = R.lastErrorText(changedServer);
  assert.ok(s.includes(NEW) && s.includes('gpu-box'), s);
  assert.ok(!s.includes('{fingerprint}') && !s.includes('{device}'), s);
}

// ---- M1: a confirmed key that changed ----
let section;
{
  let servers = [changedServer];
  answer = (method, path) => {
    if (method === 'GET' && path === '/api/remote/servers') return { status: 200, body: { servers } };
    if (method === 'POST' && path === '/api/remote/servers/gpu-box/connect')
      return {
        status: 200,
        body: { server: changedServer, host_key: { algorithm: 'ssh-ed25519', fingerprint: NEW, key: 'ssh-ed25519 AAAA', check: CHECK, known_hosts: '' }, host_key_hop: 0, previous_fingerprint: OLD, changed: true },
      };
    if (method === 'POST' && path === '/api/remote/servers/gpu-box/trust') {
      servers = [readyServer];
      return { status: 200, body: { server: readyServer, host_key_hop: 0 } };
    }
    if (method === 'GET' && path === '/api/asr/devices') return { status: 200, body: [] };
    return { status: 404, body: { error: { code: 'api.not_found' } } };
  };
  section = R.createRemoteSection(ui);
  await section.load();
  await settle();
  const card = doc.querySelector('[data-server="gpu-box"]');
  assert.ok(card, 'the server card');
  assert.ok(text(card).includes(NEW) && !text(card).includes('{fingerprint}'), 'the card names the presented key: ' + text(card));
  fk('set:remote:gpu-box:continue').click();
  await settle();
  assert.equal(posts('/api/remote/servers/gpu-box/connect').length, 1, '继续设置 goes to the host key step and connects');
  const bd = doc.querySelector('.rs-wiz-bd');
  assert.equal(bd.dataset.step, 'hostkey');
  assert.equal(text(doc.querySelector('.rs-wiz-title')), zh['settings.remote.step.hostkey_changed']);
  const alert = doc.querySelector('.rs-keychange');
  assert.ok(alert, 'the warning');
  assert.equal(alert.getAttribute('role'), 'alert', 'the warning is announced');
  assert.equal(alert.getAttribute('tabindex'), '-1');
  assert.equal(doc.activeElement, alert, 'the focus goes to the warning');
  assert.ok(text(alert).includes(zh['settings.remote.keychange.title']) && text(alert).includes(zh['settings.remote.keychange.warning']));
  const cmp = doc.querySelector('.rs-fpcmp');
  assert.ok(text(cmp.querySelector('.rs-fpcmp-row.is-old')).includes(OLD), 'the confirmed fingerprint');
  assert.ok(text(cmp.querySelector('.rs-fpcmp-row.is-new')).includes(NEW), 'the presented fingerprint');
  assert.ok(text(bd).includes(CHECK), 'the command to check it on the server');
  assert.ok(!text(bd).includes('⟦') && !text(bd).includes('{'), 'every text resolved: ' + text(bd));
  // SEC39 L-A: the page names the machine whose key changed
  const which = doc.querySelector('.rs-keyhop');
  assert.ok(which, 'the machine whose key changed');
  assert.equal(text(which), zh['settings.remote.host'] + 'alice@gpu.example.com');
  const box = fk('set:remote:wiz:keycheck');
  const go = fk('set:remote:wiz:replace');
  assert.ok(box && go, 'the box and the button');
  assert.equal(box.attrs.type, 'checkbox');
  assert.equal(box.checked, false);
  assert.equal(go.disabled, true, 'not ticked: the button is disabled');
  assert.ok(text(box.parentNode).includes(zh['settings.remote.keychange.confirm']), 'the box has its label');
  // order for the keyboard: warning, command, box, back, replace
  const order = doc.querySelectorAll('[data-fkey]').map((n) => n.dataset.fkey).filter((k) => k.startsWith('set:remote:wiz:') || k === 'set:remote:copy:check');
  assert.deepEqual(order.slice(order.indexOf('set:remote:wiz:keychanged')), ['set:remote:wiz:keychanged', 'set:remote:copy:check', 'set:remote:wiz:keycheck', 'set:remote:wiz:back', 'set:remote:wiz:replace']);
  go.click();
  await settle(20);
  assert.equal(posts('/api/remote/servers/gpu-box/trust').length, 0, 'nothing is sent before the box is ticked');
  // tick it: the button is enabled; a background re-render keeps both, and announces nothing again
  box.checked = true;
  box.dispatchEvent(event('change'));
  assert.equal(go.disabled, false, 'ticked: the button is enabled');
  box.focus();
  ui.rerender();
  const box2 = fk('set:remote:wiz:keycheck');
  const go2 = fk('set:remote:wiz:replace');
  assert.equal(box2.checked, true, 'the tick survives a re-render');
  assert.equal(go2.disabled, false);
  assert.equal(doc.activeElement, box2, 'the focus stays on the box');
  assert.notEqual(doc.querySelector('.rs-keychange').getAttribute('role'), 'alert', 'announced once, not on every re-render');
  // untick: disabled again
  box2.checked = false;
  box2.dispatchEvent(event('change'));
  assert.equal(go2.disabled, true);
  box2.checked = true;
  box2.dispatchEvent(event('change'));
  go2.click();
  await settle();
  const sent = posts('/api/remote/servers/gpu-box/trust');
  assert.equal(sent.length, 1);
  assert.deepEqual(sent[0].body, { hop: 0, fingerprint: NEW, replace: true, previous: OLD }, 'replace names the confirmed key');
  assert.equal(doc.querySelector('.rs-wiz-bd').dataset.step, 'detect', 'replaced: on to the next step');
  section.destroy();
}

// ---- L1: a first key the user's known_hosts disagrees with ----
{
  calls.length = 0;
  D.clear(shell);
  const servers = [freshServer];
  let verdict = 'mismatch';
  answer = (method, path) => {
    if (method === 'GET' && path === '/api/remote/servers') return { status: 200, body: { servers } };
    if (method === 'GET' && path === '/api/remote/servers/lab/pubkey') return { status: 200, body: { line: 'restrict ssh-ed25519 AAAA lectureflow-lab', command: 'echo', fingerprint: 'SHA256:x' } };
    if (method === 'POST' && path === '/api/remote/servers/lab/connect')
      return { status: 200, body: { server: freshServer, host_key: { algorithm: 'ssh-ed25519', fingerprint: NEW, key: 'ssh-ed25519 AAAA', check: CHECK, known_hosts: verdict }, host_key_hop: 0 } };
    if (method === 'POST' && path === '/api/remote/servers/lab/trust') return { status: 200, body: { server: { ...freshServer, trusted: true, state: 'setup' }, host_key_hop: 0 } };
    if (method === 'GET' && path === '/api/asr/devices') return { status: 200, body: [] };
    return { status: 404, body: { error: { code: 'api.not_found' } } };
  };
  section = R.createRemoteSection(ui);
  await section.load();
  await settle();
  fk('set:remote:lab:continue').click();
  await settle();
  assert.equal(doc.querySelector('.rs-wiz-bd').dataset.step, 'authorize');
  fk('set:remote:wiz:authorized').click();
  await settle();
  assert.equal(doc.querySelector('.rs-wiz-bd').dataset.step, 'hostkey');
  assert.equal(text(doc.querySelector('.rs-wiz-title')), zh['settings.remote.step.hostkey'], 'a first key: the usual step');
  assert.equal(doc.querySelector('.rs-keychange'), null, 'no "changed" warning for a first key');
  const go = fk('set:remote:wiz:trust');
  const box = fk('set:remote:wiz:keycheck');
  assert.ok(box, 'known_hosts disagrees: a box to tick');
  assert.ok(text(box.parentNode).includes(zh['settings.remote.hostkey.mismatch_confirm']));
  assert.equal(go.disabled, true, 'not ticked: trust is disabled');
  go.click();
  await settle(20);
  assert.equal(posts('/api/remote/servers/lab/trust').length, 0);
  box.checked = true;
  box.dispatchEvent(event('change'));
  assert.equal(go.disabled, false);
  go.click();
  await settle();
  const sent = posts('/api/remote/servers/lab/trust');
  assert.equal(sent.length, 1);
  assert.deepEqual(sent[0].body, { hop: 0, fingerprint: NEW }, 'a first key is no replacement');

  // no known_hosts entry: one click, as before
  verdict = '';
  section.destroy();
  calls.length = 0;
  D.clear(shell);
  section = R.createRemoteSection(ui);
  await section.load();
  await settle();
  fk('set:remote:lab:continue').click();
  await settle();
  fk('set:remote:wiz:authorized').click();
  await settle();
  assert.equal(fk('set:remote:wiz:keycheck'), null, 'no verdict: no box');
  assert.equal(fk('set:remote:wiz:trust').disabled, false);
  section.destroy();
}

// ---- SEC39 L-A: a jump host's confirmed key changed while the server
// itself is not confirmed yet (trusted false, host_key_changed). 继续设置
// opens the changed key's page at once (not the authorization: nothing
// there has to be done again), and the page names the jump host, the
// machine to check the fingerprint on. ----
{
  calls.length = 0;
  D.clear(shell);
  const JUMP = { host: 'jump.example.net', port: 2222, username: 'bob', host_key: 'ssh-ed25519 AAAA' };
  const behind = {
    id: 'behind', device_id: 'ssh:behind', name: 'behind', host: 'gpu.example.com', port: 22, username: 'alice', jumps: [JUMP], state: 'host_key_changed',
    trusted: false, kit_ready: false, settings: {}, last_error: { code: 'remote.host_key_mismatch', params: { device: 'behind', fingerprint: NEW } },
  };
  const TARGET = 'SHA256:t4rgetKeyFingerprintForTheTest0000000000000';
  let servers = [behind];
  answer = (method, path) => {
    if (method === 'GET' && path === '/api/remote/servers') return { status: 200, body: { servers } };
    if (method === 'POST' && path === '/api/remote/servers/behind/connect')
      return {
        status: 200,
        body: { server: behind, host_key: { algorithm: 'ssh-ed25519', fingerprint: NEW, key: 'ssh-ed25519 AAAA', check: CHECK, known_hosts: '' }, host_key_hop: 0, previous_fingerprint: OLD, changed: true },
      };
    if (method === 'POST' && path === '/api/remote/servers/behind/trust') {
      // replaced: the server itself comes next, a first key (hop 1)
      const next = { ...behind, state: 'untrusted', last_error: null };
      servers = [next];
      return { status: 200, body: { server: next, host_key: { algorithm: 'ssh-ed25519', fingerprint: TARGET, key: 'ssh-ed25519 AAAA', check: CHECK, known_hosts: '' }, host_key_hop: 1 } };
    }
    return { status: 404, body: { error: { code: 'api.not_found' } } };
  };
  section = R.createRemoteSection(ui);
  await section.load();
  await settle();
  fk('set:remote:behind:continue').click();
  await settle();
  assert.equal(doc.querySelector('.rs-wiz-bd').dataset.step, 'hostkey', 'an unconfirmed server with a changed jump host key: the host key step, not the authorization');
  assert.equal(calls.filter((c) => c.path.endsWith('/pubkey')).length, 0, 'the authorization step was not entered');
  assert.equal(posts('/api/remote/servers/behind/connect').length, 1);
  assert.equal(text(doc.querySelector('.rs-wiz-title')), zh['settings.remote.step.hostkey_changed']);
  assert.ok(doc.querySelector('.rs-keychange'), 'the warning');
  const which = doc.querySelector('.rs-keyhop');
  assert.ok(which, 'the machine whose key changed');
  assert.equal(text(which), zh['settings.remote.host'] + 'bob@jump.example.net:2222', 'the jump host, not the server');
  assert.ok(!text(which).includes('gpu.example.com'));
  const box = fk('set:remote:wiz:keycheck');
  box.checked = true;
  box.dispatchEvent(event('change'));
  fk('set:remote:wiz:replace').click();
  await settle();
  const sent = posts('/api/remote/servers/behind/trust');
  assert.equal(sent.length, 1);
  assert.deepEqual(sent[0].body, { hop: 0, fingerprint: NEW, replace: true, previous: OLD });
  // on to the server's own key: the usual first check, no "changed" page
  assert.equal(doc.querySelector('.rs-wiz-bd').dataset.step, 'hostkey');
  assert.equal(text(doc.querySelector('.rs-wiz-title')), zh['settings.remote.step.hostkey']);
  assert.equal(doc.querySelector('.rs-keychange'), null);
  assert.equal(doc.querySelector('.rs-keyhop'), null);
  assert.ok(text(doc.querySelector('.rs-wiz-bd')).includes(TARGET));
  section.destroy();

  // the server's own key changed behind a confirmed jump host (hop 1): the server
  calls.length = 0;
  D.clear(shell);
  const changedBehind = { ...behind, trusted: true, host_key_fingerprint: OLD, kit_ready: true };
  servers = [changedBehind];
  answer = (method, path) => {
    if (method === 'GET' && path === '/api/remote/servers') return { status: 200, body: { servers } };
    if (method === 'POST' && path === '/api/remote/servers/behind/connect')
      return {
        status: 200,
        body: { server: changedBehind, host_key: { algorithm: 'ssh-ed25519', fingerprint: NEW, key: 'ssh-ed25519 AAAA', check: CHECK, known_hosts: '' }, host_key_hop: 1, previous_fingerprint: OLD, changed: true },
      };
    return { status: 404, body: { error: { code: 'api.not_found' } } };
  };
  section = R.createRemoteSection(ui);
  await section.load();
  await settle();
  fk('set:remote:behind:continue').click();
  await settle();
  assert.equal(text(doc.querySelector('.rs-keyhop')), zh['settings.remote.host'] + 'alice@gpu.example.com', 'hop 1 of one jump host: the server');
  section.destroy();
}

console.log('ssh keychange checks: ok');
