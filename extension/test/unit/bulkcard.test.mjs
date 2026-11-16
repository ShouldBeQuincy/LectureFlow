// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The bulk card ("更新这门课程到 LectureFlow", src/ui/bulkcard.js) on a
// minimal stand-in DOM: when a session's restriction switch could not be
// read because the profile was stopped as too slow, the card says so, as
// the send card's chip does (SEC18f 低-18). The browser renders it in the
// e2e tests; here only what the card builds is looked at. Nodes are only
// ever compared by identity (assert.ok), never with assert.equal.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { registerHooks } from 'node:module';

// ui/icons.js is written by the build
registerHooks({
  resolve(specifier, context, next) {
    if (specifier === './icons.js' && String(context.parentURL || '').includes('/src/ui/')) {
      return { url: `data:text/javascript,${encodeURIComponent('export const ICONS = {};')}`, shortCircuit: true };
    }
    return next(specifier, context);
  },
});

class FakeBase {}
class FakeText extends FakeBase {
  constructor(s) {
    super();
    this.text = String(s);
  }
  get textContent() {
    return this.text;
  }
}
class FakeElement extends FakeBase {
  constructor(tag) {
    super();
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.attrs = {};
    this.dataset = {};
    this.listeners = {};
    this.className = '';
    this.id = '';
    this.checked = false;
    this.disabled = false;
    this.value = '';
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
    if (k === 'id') this.id = String(v);
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  append(...cs) {
    this.children.push(...cs);
  }
  replaceChildren(...cs) {
    this.children = [...cs];
  }
  set textContent(v) {
    this.children = [new FakeText(v)];
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join('');
  }
}

globalThis.Node = FakeBase;
globalThis.document = {
  createElement: (tag) => new FakeElement(tag),
  createElementNS: (ns, tag) => new FakeElement(tag),
  createTextNode: (s) => new FakeText(s),
};
globalThis.browser = { i18n: { getMessage: (k) => k, getUILanguage: () => 'en' } };

const { createBulkCard } = await import('../../src/ui/bulkcard.js');

/** The first element with this id under root, or null (at most 10000 nodes looked at). */
function byId(root, id) {
  const todo = [root];
  for (let seen = 0; todo.length; seen++) {
    if (seen > 10000) throw new Error('tree too large');
    const n = todo.shift();
    if (n instanceof FakeElement) {
      if (n.id === id) return n;
      todo.push(...n.children.filter(Boolean));
    }
  }
  return null;
}

async function settle() {
  for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
}

async function sendOne(view) {
  const mount = new FakeElement('div');
  const asked = [];
  const ask = async (cmd, extra) => {
    asked.push(cmd);
    if (cmd === 'connection') return { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'Course' }], lastCourseId: 'c1' } };
    if (cmd === 'checkMany') return { ok: true, items: [] };
    if (cmd === 'session.load') return { ok: true, view: { remembered: false, tracks: [{ id: 'a', kind: 'audio', role: '', protected: false }], ...view } };
    if (cmd === 'send') return { ok: true, taskId: 't1' };
    throw new Error(`unexpected ${cmd} ${JSON.stringify(extra)}`);
  };
  const card = createBulkCard({ mount, ask, rows: [{ sessionId: 's1', title: 'Lecture 1' }], fetchInfo: async () => ({ ok: true, values: {}, url: 'https://tenant.example.com/x' }) });
  await card.start();
  await settle();
  const primary = byId(mount, 'primary');
  assert.ok(primary !== null, 'the send button');
  assert.equal(primary.disabled, false);
  for (const fn of primary.listeners.click || []) fn();
  await settle();
  assert.equal(card.state.phase, 'done');
  assert.deepEqual(asked, ['connection', 'checkMany', 'session.load', 'send']);
  return byId(mount, 'bulk-policy-stopped');
}

test('the bulk card says why the restriction is unknown when the profile was stopped as too slow (SEC18f 低-18)', async () => {
  const note = await sendOne({ downloadPolicy: 'unknown', policyStopped: true });
  assert.ok(note !== null, 'the note is shown');
  assert.equal(note.textContent, 'policyStopped');
  assert.equal(note.dataset.flag, 'policy-stopped');
});

test('without a stopped profile the bulk card has no such note', async () => {
  assert.ok((await sendOne({ downloadPolicy: 'unknown', policyStopped: false })) === null);
  assert.ok((await sendOne({ downloadPolicy: 'enabled' })) === null);
});

test('a profile the background stopped: session.load says so, the row is not sent and the card says why (SEC22 L-D)', async () => {
  const fs = await import('node:fs');
  const path = await import('node:path');
  const { fileURLToPath } = await import('node:url');
  const { sessionLoadReply } = await import('../../src/lib/inpage.js');
  const { guardedRegex, setRegexGuard, resetRegexGuard } = await import('../../src/lib/regexguard.js');
  const { profileKey } = await import('../../src/lib/observe.js');
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
  const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
  const LIST = 'https://tenant.cloud.panopto.eu/Panopto/Pages/Sessions/List.aspx';
  const ID = 'a0000000-0000-4000-8000-0000000000a1';
  const found = { profile: panopto, reviewed: true, mode: 'list' };
  // the background's guard stops Panopto: one match it timed at 300 ms
  resetRegexGuard();
  let t = 0;
  setRegexGuard({ now: () => (t += 300) });
  try {
    guardedRegex('x', { owner: profileKey(panopto) }).test('x');
    setRegexGuard({ now: () => performance.now() });
    const mount = new FakeElement('div');
    const asked = [];
    const ask = async (cmd, extra) => {
      asked.push(cmd);
      if (cmd === 'connection') return { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'Course' }], lastCourseId: 'c1' } };
      if (cmd === 'checkMany') return { ok: true, items: [] };
      // what src/background/main.js answers, through the port as it is
      if (cmd === 'session.load') {
        const reply = sessionLoadReply({ found, sessionId: extra.sessionId, values: extra.values, frameUrl: LIST, locale: 'en' });
        return structuredClone(reply.ok ? { ok: true, view: {} } : reply);
      }
      throw new Error(`unexpected ${cmd}`);
    };
    const card = createBulkCard({ mount, ask, rows: [{ sessionId: ID, title: 'Lecture 1' }], fetchInfo: async () => ({ ok: true, values: { DownloadUrl: '' }, url: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer/DeliveryInfo.aspx' }) });
    await card.start();
    await settle();
    for (const fn of byId(mount, 'primary').listeners.click || []) fn();
    await settle();
    assert.equal(card.state.phase, 'done');
    assert.deepEqual(asked, ['connection', 'checkMany', 'session.load'], 'nothing sent');
    assert.equal(card.state.results.get(ID).state, 'failed');
    assert.equal(card.state.results.get(ID).error, 'policyStopped');
    const note = byId(mount, 'bulk-policy-stopped');
    assert.ok(note !== null, 'the note is shown');
    assert.equal(note.textContent, 'policyStopped');
  } finally {
    resetRegexGuard();
    setRegexGuard({ now: () => performance.now(), onTrip: null });
  }
});

/** Every text under root, in tree order (at most 10000 nodes looked at). */
function textsOf(root) {
  const out = [];
  const todo = [root];
  for (let seen = 0; todo.length; seen++) {
    if (seen > 10000) throw new Error('tree too large');
    const n = todo.shift();
    if (n instanceof FakeText) out.push(n.text);
    else if (n instanceof FakeElement) todo.push(...n.children.filter(Boolean));
  }
  return out;
}

test('G5: lectures that need a refresh are not "being fetched": the card says why they cannot be ticked and where to resume them', async () => {
  const mount = new FakeElement('div');
  const ask = async (cmd) => {
    if (cmd === 'connection') return { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'Course' }], lastCourseId: 'c1' } };
    if (cmd === 'checkMany') return { ok: true, items: [{ sessionId: 's2', state: 'needs_refresh', taskId: 2 }, { sessionId: 's3', state: 'needs_refresh', taskId: 3 }] };
    throw new Error(`unexpected ${cmd}`);
  };
  const rows = [{ sessionId: 's1', title: 'L1' }, { sessionId: 's2', title: 'L2' }, { sessionId: 's3', title: 'L3' }];
  const card = createBulkCard({ mount, ask, rows, fetchInfo: async () => ({ ok: false, status: 0 }) });
  await card.start();
  await settle();
  const note = byId(mount, 'bulk-refresh-note');
  assert.ok(note !== null, 'the note is shown');
  assert.equal(note.textContent, 'bulkRefreshNote');
  assert.deepEqual(byId(mount, 'bulk-rows').children.map((li) => [li.dataset.state, li.dataset.disabled]), [['new', 'false'], ['refresh', 'true'], ['refresh', 'true']]);
});

/** A bulk card whose connection answers are given in turn (the last one repeats); records the requests. */
async function bulkWith(connections, items = []) {
  const mount = new FakeElement('div');
  const asked = [];
  let i = 0;
  const ask = async (cmd, extra = {}) => {
    asked.push({ cmd, ...extra });
    if (cmd === 'connection') return connections[Math.min(i++, connections.length - 1)];
    if (cmd === 'checkMany') return { ok: true, items };
    throw new Error(`unexpected ${cmd}`);
  };
  const card = createBulkCard({ mount, ask, rows: [{ sessionId: 's1', title: 'L1' }], fetchInfo: async () => ({ ok: false, status: 0 }) });
  return { mount, card, asked, ask };
}

test('G8: the bulk card names the diagnostics page only when the browser could not reach the host; a refused request says why (SEC42 低-3)', async () => {
  for (const conn of [{ reason: 'app.not_running', params: { reason: 'not_installed' } }, { reason: 'ext.timeout', params: {} }]) {
    const b = await bulkWith([{ ok: true, connection: { status: 'unavailable', ...conn } }]);
    await b.card.start();
    await settle();
    assert.ok(byId(b.mount, 'disc-register') === null, JSON.stringify(conn));
  }
  const refused = await bulkWith([{ ok: false, error: { code: 'ext.site_blocked', params: { category: 'youtube' } } }]);
  await refused.card.start();
  await settle();
  const texts = textsOf(refused.mount);
  assert.ok(texts.includes('unsupportedYoutube'), texts.join(' | '));
  assert.ok(!texts.includes('discNotInstalled') && byId(refused.mount, 'disc-register') === null, texts.join(' | '));
});

// a timeout: with the card's old behaviour nothing here ends (SEC42 低-6)
test('G7: the bulk card waiting for LectureFlow says it connects, not "正在探测…"', { timeout: 5000 }, async () => {
  let hold = null;
  const mount = new FakeElement('div');
  const card = createBulkCard({ mount, ask: (cmd) => (cmd === 'connection' ? new Promise((r) => (hold = r)) : Promise.resolve({ ok: true, items: [] })), rows: [{ sessionId: 's1', title: 'L1' }], fetchInfo: async () => ({ ok: false, status: 0 }) });
  card.start();
  await settle();
  const texts = textsOf(mount);
  assert.ok(texts.includes('connectingApp') && !texts.includes('detecting'), texts.join(' | '));
  hold({ ok: true, connection: { status: 'unavailable', reason: 'ext.host_unavailable', params: {} } });
  await settle();
});

test('G8: the bulk card not connected: the reason as the popup says it, the diagnostics page for a broken registration, and "重新检测"', async () => {
  const notFound = { ok: true, connection: { status: 'unavailable', reason: 'ext.host_unavailable', params: {} } };
  const b = await bulkWith([notFound, { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'Course' }], lastCourseId: '' } }]);
  await b.card.start();
  await settle();
  let texts = textsOf(b.mount);
  assert.ok(texts.includes('discNotInstalled') && texts.includes('discRegister'), texts.join(' | '));
  const recheck = byId(b.mount, 'bulk-recheck');
  assert.ok(recheck !== null, 'the button the sentence names');
  for (const fn of recheck.listeners.click || []) fn();
  await settle();
  assert.deepEqual(b.asked.filter((a) => a.cmd === 'connection').map((a) => a.recheck), [false, true]);
  assert.equal(b.card.state.phase, 'idle');
  assert.ok(byId(b.mount, 'bulk-summary') !== null, 'connected now');
  // LectureFlow could not be started: said so, not "not found"
  const failed = await bulkWith([{ ok: true, connection: { status: 'unavailable', reason: 'app.not_running', params: { reason: 'start_failed' } } }]);
  await failed.card.start();
  await settle();
  texts = textsOf(failed.mount);
  assert.ok(texts.includes('discNotRunning') && !texts.includes('discNotInstalled') && !texts.includes('discRegister'), texts.join(' | '));
});

test('G9: the bulk card with no course in LectureFlow: where to make one, and "重新检测"', async () => {
  const none = { ok: true, connection: { status: 'connected', courses: [], lastCourseId: '' } };
  const b = await bulkWith([none, { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'Course' }], lastCourseId: '' } }]);
  await b.card.start();
  await settle();
  const texts = textsOf(b.mount);
  assert.ok(texts.includes('courseNone') && texts.includes('courseCreate'), texts.join(' | '));
  const recheck = byId(b.mount, 'course-recheck');
  assert.ok(recheck !== null);
  for (const fn of recheck.listeners.click || []) fn();
  await settle();
  assert.ok(byId(b.mount, 'course') !== null, 'the course list is there now');
  assert.equal(b.card.state.courseId, 'c1');
});

test('G6: while the user decides on the reminder the button does not say it is sending; once done, no "发送前会再提醒一次"', async () => {
  const mount = new FakeElement('div');
  const ask = async (cmd) => {
    if (cmd === 'connection') return { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'Course' }], lastCourseId: 'c1' } };
    if (cmd === 'checkMany') return { ok: true, items: [] };
    if (cmd === 'session.load') return { ok: true, view: { remembered: false, downloadPolicy: 'disabled', tracks: [{ id: 'a', kind: 'audio', role: '', protected: false }] } };
    if (cmd === 'send') return { ok: true, taskId: 9 };
    throw new Error(`unexpected ${cmd}`);
  };
  const card = createBulkCard({ mount, ask, rows: [{ sessionId: 's1', title: 'L1' }], platformName: 'Panopto', fetchInfo: async () => ({ ok: true, values: {}, url: 'https://tenant.example.com/x' }) });
  await card.start();
  await settle();
  for (const fn of byId(mount, 'primary').listeners.click || []) fn();
  // the reading of each session waits a little between sessions; one session here
  for (let i = 0; i < 50 && card.state.phase !== 'confirm'; i++) await settle();
  assert.equal(card.state.phase, 'confirm');
  const primary = byId(mount, 'primary');
  assert.ok(primary !== null);
  assert.deepEqual(textsOf(primary), ['bulkWaiting']);
  for (const fn of byId(mount, 'override-confirm').listeners.click || []) fn();
  await settle();
  assert.equal(card.state.phase, 'done');
  const texts = textsOf(mount);
  assert.ok(!texts.includes('restrictedNotice'), texts.join(' | '));
  assert.ok(texts.includes('restrictedNoticeQuiet'), 'the rules, without "will remind once more"');
  assert.ok(byId(mount, 'remembered-note') === null, 'not turned off for the platform');
});
