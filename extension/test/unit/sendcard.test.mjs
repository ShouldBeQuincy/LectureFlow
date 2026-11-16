// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The send card's track list (src/ui/sendcard.js) on a minimal stand-in
// DOM (SEC25 低-2): it lists at most RETAIN_LIMITS.tracksShown tracks and
// says how many it left out; the counts are of all tracks. The browser
// renders the card in the e2e tests; here only what it builds is looked
// at. Nodes are only ever compared by identity (assert.ok), never with
// assert.equal.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { registerHooks } from 'node:module';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

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
  querySelector() {
    return null;
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
// a message is its key and its substitutions: "tracksHidden:51"
globalThis.browser = { i18n: { getMessage: (k, subs) => (subs && subs.length ? `${k}:${subs.join(',')}` : k), getUILanguage: () => 'en' } };

const { createSendCard } = await import('../../src/ui/sendcard.js');
const { RETAIN_LIMITS } = await import('../../src/lib/retain.js');

/** The first element with this id under root, or null (at most 20000 nodes looked at). */
function byId(root, id) {
  const todo = [root];
  for (let seen = 0; todo.length; seen++) {
    if (seen > 20000) throw new Error('tree too large');
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

const tr = (i, kind) => ({ id: `${kind}${i}`, kind, format: 'file', quality: '', sizeBytes: 10, sizeEstimated: false, sources: ['element'], protected: false, name: '', language: '', unreadable: false, role: '', label: '', hint: '', recommended: false });

/** A popup card on this view, with the full list ("选择轨道…") opened. */
async function openList(viewExtra) {
  const mount = new FakeElement('div');
  const ask = async (cmd) => {
    if (cmd === 'detect') return { ok: true, view: { status: 'ok', authorized: true, profile: null, host: 'lectures.example.edu', lecture: { title: 'L' }, downloadPolicy: 'unknown', policyStopped: false, observe: { declared: false, seen: false }, presets: null, platform: 'generic', blockedEmbeds: [], remembered: false, lastPreset: '', ...viewExtra }, origins: [], cookieHosts: [], offers: [] };
    if (cmd === 'connection') return { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'Course' }], lastCourseId: 'c1' } };
    if (cmd === 'check') return { ok: true, item: null };
    throw new Error(`unexpected ${cmd}`);
  };
  const card = createSendCard({ mount, ask, host: 'popup' });
  await card.start();
  await settle();
  const toggle = byId(mount, 'advanced');
  assert.ok(toggle !== null, 'the "choose tracks" button');
  const closedText = toggle.textContent;
  for (const fn of toggle.listeners.click || []) fn();
  await settle();
  return { mount, card, closedText };
}

test('the track list: a page within the limit lists every track, no note', async () => {
  const tracks = [...Array.from({ length: 12 }, (_, i) => tr(i, 'video')), tr(0, 'audio')];
  const { mount, closedText } = await openList({ tracks, hiddenTracks: 0, summary: { total: 13, audio: 1, video: 12, unknown: 0, protected: 0 } });
  assert.equal(byId(mount, 'tracks').children.length, 13);
  assert.ok(byId(mount, 'tracks-hidden') === null, 'no note');
  assert.equal(byId(mount, 'count').dataset.count, '13');
  assert.equal(closedText, 'chooseTracks:13');
});

test('the track list: the tracks the background sent, and how many it left out; the counts are of all', async () => {
  const shown = [...Array.from({ length: RETAIN_LIMITS.tracksShown - 1 }, (_, i) => tr(i, 'video')), tr(0, 'audio')];
  const { mount, card, closedText } = await openList({ tracks: shown, hiddenTracks: 51, summary: { total: shown.length + 51 } });
  assert.equal(byId(mount, 'tracks').children.length, RETAIN_LIMITS.tracksShown);
  const note = byId(mount, 'tracks-hidden');
  assert.ok(note !== null, 'the note is shown');
  assert.equal(note.textContent, 'tracksHidden:51');
  assert.equal(note.dataset.hidden, '51');
  assert.equal(byId(mount, 'count').dataset.count, String(RETAIN_LIMITS.tracksShown + 51));
  assert.equal(closedText, `chooseTracks:${RETAIN_LIMITS.tracksShown + 51}`);
  assert.deepEqual(card.state.selection, ['audio0', 'video0'], 'the default ticks, as before');
});

test('the track list: a view with more tracks than the limit is held to it here too', async () => {
  const many = [...Array.from({ length: 300 }, (_, i) => tr(i, 'video')), tr(0, 'audio')];
  const { mount } = await openList({ tracks: many, summary: { total: many.length } });
  assert.equal(byId(mount, 'tracks').children.length, RETAIN_LIMITS.tracksShown);
  const ticked = byId(mount, 'tracks').children.filter((li) => li.dataset.checked === 'true').length;
  assert.equal(ticked, 2, 'the main choice’s tracks are among the listed');
  assert.equal(byId(mount, 'tracks-hidden').dataset.hidden, String(many.length - RETAIN_LIMITS.tracksShown));
  assert.equal(byId(mount, 'count').dataset.count, String(many.length));
});

test('a ticked track the background no longer lists: the card says the page changed (SEC28 低-2)', async () => {
  const { errorText } = await import('../../src/ui/sendcard.js');
  assert.equal(errorText({ code: 'ext.selection.unknown_track' }), 'errStale');
  assert.equal(errorText({ code: 'ext.stale' }), 'errStale');
});

/** Every text under root, in tree order (at most 20000 nodes looked at). */
function textsOf(root) {
  const out = [];
  const todo = [root];
  for (let seen = 0; todo.length; seen++) {
    if (seen > 20000) throw new Error('tree too large');
    const n = todo.shift();
    if (n instanceof FakeText) out.push(n.text);
    else if (n instanceof FakeElement) todo.push(...n.children.filter(Boolean));
  }
  return out;
}

test('a page that has not finished loading: the card says so, not that it cannot read the page (SEC15 R2-5)', async () => {
  const shows = async (status) => {
    const mount = new FakeElement('div');
    const ask = async (cmd) => {
      if (cmd === 'detect') return { ok: true, view: { status }, origins: [], cookieHosts: [], offers: [] };
      if (cmd === 'connection') return { ok: true, connection: { status: 'connected', courses: [], lastCourseId: '' } };
      if (cmd === 'check') return { ok: true, item: null };
      throw new Error(`unexpected ${cmd}`);
    };
    const card = createSendCard({ mount, ask, host: 'popup' });
    await card.start();
    await settle();
    return textsOf(mount);
  };
  const loading = await shows('page_loading');
  assert.ok(loading.includes('pageLoading'), loading.join(' | '));
  assert.ok(!loading.includes('noAccess'));
  const refused = await shows('no_access');
  assert.ok(refused.includes('noAccess') && !refused.includes('pageLoading'), refused.join(' | '));
});

const okView = (extra = {}) => ({ status: 'ok', authorized: true, profile: null, host: 'lectures.example.edu', lecture: { title: 'L' }, downloadPolicy: 'unknown', policyStopped: false, observe: { declared: false, seen: false }, presets: null, platform: 'generic', blockedEmbeds: [], remembered: false, lastPreset: '', tracks: [tr(0, 'video'), tr(0, 'audio')], hiddenTracks: 0, summary: { total: 2 }, ...extra });

/**
 * A card on a host whose transport answers from `answers` (cmd → reply, or
 * a function of the request) and records every request.
 */
async function cardWith(answers, { host = 'popup' } = {}) {
  const mount = new FakeElement('div');
  const asked = [];
  const ask = async (cmd, extra = {}) => {
    asked.push({ cmd, ...extra });
    const a = answers[cmd];
    if (a === undefined) throw new Error(`unexpected ${cmd}`);
    return typeof a === 'function' ? a(extra) : a;
  };
  const card = createSendCard({ mount, ask, host });
  await card.start();
  await settle();
  return { mount, card, asked, click: async (id) => {
    const n = byId(mount, id);
    assert.ok(n !== null, `#${id} is shown`);
    for (const fn of n.listeners.click || []) fn();
    await settle();
  } };
}

test('G1: opening the popup asks for the connection passively (it never starts LectureFlow); an in-page card the user opened may', async () => {
  const answers = { detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } }, check: { ok: true, item: null } };
  const popup = await cardWith(answers);
  assert.deepEqual(popup.asked.filter((a) => a.cmd === 'connection'), [{ cmd: 'connection', recheck: false, passive: true }]);
  const inpage = await cardWith(answers, { host: 'inpage' });
  assert.deepEqual(inpage.asked.filter((a) => a.cmd === 'connection'), [{ cmd: 'connection', recheck: false, passive: false }]);
  const win = await cardWith(answers, { host: 'window' });
  assert.deepEqual(win.asked.filter((a) => a.cmd === 'connection'), [{ cmd: 'connection', recheck: false, passive: false }]);
});

test('G1: LectureFlow installed but not open: "未连接" says so, offers "打开 LectureFlow" (which may start it), no "获取 LectureFlow"', async () => {
  let open = false;
  const answers = {
    detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] },
    // a connection that may start LectureFlow (no passive) starts it
    connection: (x) => {
      if (!x.passive) open = true;
      return open ? { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: '' } } : { ok: true, connection: { status: 'unavailable', reason: 'app.not_running', params: { reason: 'not_running' } } };
    },
    check: { ok: true, item: null },
  };
  const c = await cardWith(answers);
  assert.equal(c.mount.children[0].dataset.view, 'disconnected');
  const texts = textsOf(c.mount);
  assert.ok(texts.includes('discNotOpen'), texts.join(' | '));
  assert.ok(!texts.includes('discNotInstalled'));
  assert.ok(byId(c.mount, 'launch') !== null && textsOf(byId(c.mount, 'launch')).includes('openApp'));
  assert.ok(byId(c.mount, 'get-app') === null, 'it is installed: nothing to get');
  await c.click('launch');
  assert.deepEqual(c.asked.filter((a) => a.cmd === 'connection').map((a) => [a.recheck, a.passive]), [[false, true], [true, false]], 'the click asks for a connection that may start LectureFlow');
  assert.equal(c.mount.children[0].dataset.view, 'ready');
});

test('G1: installed but not open also offers "重新检测", which only looks (it never starts LectureFlow), as SPEC-ext’s "未连接" asks (SEC42 低-2)', async () => {
  const notOpen = { ok: true, connection: { status: 'unavailable', reason: 'app.not_running', params: { reason: 'not_running' } } };
  const c = await cardWith({ detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] }, connection: notOpen, check: { ok: true, item: null } });
  assert.ok(byId(c.mount, 'launch') !== null && byId(c.mount, 'recheck') !== null, 'both buttons');
  assert.ok(textsOf(c.mount).includes('discNotOpen'));
  await c.click('recheck');
  assert.deepEqual(c.asked.filter((a) => a.cmd === 'connection').map((a) => [a.recheck, a.passive]), [[false, true], [true, true]], '"重新检测" asks again, passively');
  assert.equal(c.mount.children[0].dataset.view, 'disconnected', 'still not open');
  assert.notEqual(c.mount.children[0].dataset.reason, 'launching');
  await c.click('launch');
  assert.deepEqual(c.asked.filter((a) => a.cmd === 'connection').map((a) => [a.recheck, a.passive]).at(-1), [true, false], '"打开 LectureFlow" may start it');
});

test('G1: not found (no host) still says "install", with "重新检测" and "获取 LectureFlow"', async () => {
  const c = await cardWith({ detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'unavailable', reason: 'ext.host_unavailable', params: {} } }, check: { ok: true, item: null } });
  const texts = textsOf(c.mount);
  assert.ok(texts.includes('discNotInstalled'), texts.join(' | '));
  assert.ok(byId(c.mount, 'recheck') !== null && byId(c.mount, 'get-app') !== null);
  assert.ok(byId(c.mount, 'launch') === null);
});

test('G4: a choice that cannot be made says why where it can be seen, not only in its tooltip', async () => {
  // one picture: "双机位" is off, and the card says why under the choices
  const c = await cardWith({ detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } }, check: { ok: true, item: null } });
  const presets = byId(c.mount, 'presets');
  assert.deepEqual(presets.children.map((b) => [b.dataset.preset, b.disabled]), [['main', false], ['dual', true], ['audio', false]]);
  const why = byId(c.mount, 'presets-why');
  assert.ok(why !== null, 'the reason is on the card');
  assert.equal(why.textContent, 'presetUnavailable:presetDual,presetDualNone');
  // all three choices possible: no such line
  const two = await cardWith({ detect: { ok: true, view: okView({ tracks: [tr(0, 'video'), tr(1, 'video'), tr(0, 'audio')] }), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } }, check: { ok: true, item: null } });
  assert.ok(byId(two.mount, 'presets-why') === null);
});

test('G4: protected (DRM) tracks only: no "选一项要发送的内容" under choices that cannot be made', async () => {
  const drm = (x) => ({ ...x, protected: true });
  const c = await cardWith({ detect: { ok: true, view: okView({ tracks: [drm(tr(0, 'video')), drm(tr(0, 'audio'))], summary: { total: 2, protected: 2 } }), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } }, check: { ok: true, item: null } });
  assert.equal(c.mount.children[0].dataset.reason, 'protected');
  assert.ok(byId(c.mount, 'presets').children.every((b) => b.disabled));
  const texts = textsOf(c.mount);
  assert.ok(!texts.includes('chooseContent'), texts.join(' | '));
  assert.ok(byId(c.mount, 'presets-why') === null, 'the DRM banner says why');
});

// a timeout: with the card's old behaviour a hello here never answers (SEC42 低-6)
test('G7: while LectureFlow is being started the card says so, not "正在探测…"', { timeout: 5000 }, async () => {
  let release = null;
  const answers = {
    detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] },
    connection: (x) => (x.passive ? { ok: true, connection: { status: 'unavailable', reason: 'app.not_running', params: { reason: 'not_running' } } } : new Promise((r) => (release = () => r({ ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: '' } })))),
    check: { ok: true, item: null },
  };
  const c = await cardWith(answers);
  const n = byId(c.mount, 'launch');
  for (const fn of n.listeners.click || []) fn();
  await settle();
  assert.equal(c.mount.children[0].dataset.view, 'loading');
  assert.equal(c.mount.children[0].dataset.reason, 'launching');
  let texts = textsOf(c.mount);
  assert.ok(texts.includes('launchingApp') && !texts.includes('detecting'), texts.join(' | '));
  release();
  await settle();
  assert.equal(c.mount.children[0].dataset.view, 'ready');
  // connecting (the first, passive hello): "正在连接 LectureFlow…"
  let hold = null;
  const mount = new FakeElement('div');
  const slow = createSendCard({ mount, host: 'popup', ask: async (cmd, x) => (cmd === 'connection' ? new Promise((r) => (hold = r)) : answers[cmd]) });
  const started = slow.start(); // not awaited: it waits for the hello
  await settle();
  assert.ok(hold !== null, 'the hello is asked');
  texts = textsOf(mount);
  assert.ok(texts.includes('connectingApp') && !texts.includes('detecting'), texts.join(' | '));
  hold({ ok: true, connection: { status: 'unavailable', reason: 'ext.host_unavailable', params: {} } });
  await started;
});

test('G8: "not found" also points to LectureFlow’s diagnostics page, where a broken browser registration is fixed', async () => {
  const c = await cardWith({ detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'unavailable', reason: 'ext.host_unavailable', params: {} } }, check: { ok: true, item: null } });
  assert.equal(byId(c.mount, 'disc-register').textContent, 'discRegister');
  // not for the other reasons
  // not for the other reasons: the host answered, so the browser's registration is fine (SEC42 低-3)
  for (const conn of [
    { reason: 'app.not_running', params: { reason: 'not_running' } },
    { reason: 'app.not_running', params: { reason: 'start_failed' } },
    { reason: 'app.not_running', params: { reason: 'not_installed' } },
    { reason: 'ext.timeout', params: {} },
    { reason: 'ext.closed', params: {} },
    { reason: 'protocol.unsupported', params: {} },
  ]) {
    const d = await cardWith({ detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'unavailable', ...conn } }, check: { ok: true, item: null } });
    assert.ok(byId(d.mount, 'disc-register') === null, JSON.stringify(conn));
  }
});

test('G9: no course in LectureFlow yet: where to make one, and "重新检测" to see it without reopening', async () => {
  let courses = [];
  const c = await cardWith({ detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] }, connection: () => ({ ok: true, connection: { status: 'connected', courses, lastCourseId: '' } }), check: { ok: true, item: null } });
  const texts = textsOf(c.mount);
  assert.ok(texts.includes('courseNone') && texts.includes('courseCreate'), texts.join(' | '));
  assert.equal(byId(c.mount, 'primary').disabled, true);
  const { errorText } = await import('../../src/ui/sendcard.js');
  assert.equal(errorText({ code: 'ext.no_course' }), 'courseNone courseCreate', 'the way out with the error too');
  courses = [{ id: 'c1', name: 'C' }];
  await c.click('course-recheck');
  assert.deepEqual(c.asked.filter((a) => a.cmd === 'connection').map((a) => a.recheck), [false, true]);
  assert.ok(byId(c.mount, 'course') !== null, 'the course list is there now');
  assert.equal(byId(c.mount, 'primary').disabled, false);
});

test('G11: no developer words on the card: the platform’s name (the profile in a tooltip), no format words on the tracks, an unreadable track said in words', async () => {
  const profile = { id: 'demo', name: 'Demo', version: '1.2.3', reviewed: true };
  const labelled = { ...tr(0, 'video'), format: 'hls', role: 'screen', label: 'Screen', hint: '', quality: '720p' };
  const generic = { ...tr(0, 'audio'), format: 'dash', unreadable: true };
  const c = await cardWith({ detect: { ok: true, view: okView({ profile, platform: 'demo', tracks: [labelled, generic] }), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } }, check: { ok: true, item: null } });
  const lecture = byId(c.mount, 'lecture');
  const meta = lecture.children.find((n) => n.className === 'lec-meta');
  assert.equal(meta.textContent, 'Demo');
  assert.equal(meta.attrs.title, 'profileSource:Demo,1.2.3', 'the profile and its version for whoever wants them');
  await c.click('advanced');
  const texts = textsOf(byId(c.mount, 'tracks'));
  for (const word of ['format_hls', 'format_dash', 'format_file', 'sourceGeneric']) assert.ok(!texts.some((x) => x.includes(word)), `${word}: ${texts.join(' | ')}`);
  const rows = byId(c.mount, 'tracks').children;
  assert.equal(rows[0].children[0].attrs.title, 'kind_video · format_hls', 'the kind and format in the tooltip');
  assert.ok(texts.includes('unreadable'), 'said on the track');
  // generic detection: "在页面上找到" instead of the profile
  const g = await cardWith({ detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } }, check: { ok: true, item: null } });
  assert.equal(byId(g.mount, 'lecture').children.find((n) => n.className === 'lec-meta').textContent, 'sourceGeneric');
});

test('G12: two tracks of the same name are told apart on the card', async () => {
  const c = await cardWith({ detect: { ok: true, view: okView({ tracks: [tr(0, 'video'), tr(0, 'audio'), tr(1, 'audio')] }), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } }, check: { ok: true, item: null } });
  assert.ok(byId(c.mount, 'chosen').textContent.startsWith('kind_video + kind_audio 1 · '), byId(c.mount, 'chosen').textContent);
  await c.click('advanced');
  // the .track-name of each row (breadth first, at most 200 nodes a row)
  const nameOf = (li) => {
    const todo = [li];
    for (let seen = 0; todo.length && seen < 200; seen++) {
      const n = todo.shift();
      if (n.className === 'track-name') return n.textContent;
      todo.push(...(n.children || []).filter((x) => x instanceof FakeElement));
    }
    return null;
  };
  assert.deepEqual(byId(c.mount, 'tracks').children.map(nameOf), ['kind_video', 'kind_audio 1', 'kind_audio 2']);
});

/** The first descendant of n with this class (breadth first, at most 200 nodes), or null. */
function byClass(n, cls) {
  const todo = [n];
  for (let seen = 0; todo.length && seen < 200; seen++) {
    const x = todo.shift();
    if (x.className === cls) return x;
    todo.push(...(x.children || []).filter((c) => c instanceof FakeElement));
  }
  return null;
}

test('EU-03: a track on another site than the page says on which host, in its small line; on the page’s own site it does not', async () => {
  const on = (i, kind, host, extra = {}) => ({ ...tr(i, kind), host, ...extra });
  const tracks = [
    on(0, 'video', 'media.example.net', { label: 'Screen', hint: 'Slides', quality: '720p' }),
    on(0, 'audio', 'lms.example.edu'),
    on(1, 'video', 'cdn.lms.example.edu'),
    // what the background gives is cleaned like any host: no path, query, port or user info
    on(2, 'video', 'User:pw@Media.Example.NET.:8443/v/talk.mp4?Signature=abc&Expires=1'),
    on(1, 'audio', undefined),
    on(3, 'video', 'not a host/'),
  ];
  const c = await cardWith({ detect: { ok: true, view: okView({ host: 'lms.example.edu', tracks, summary: { total: tracks.length } }), origins: [], cookieHosts: [], offers: [] }, connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } }, check: { ok: true, item: null } });
  await c.click('advanced');
  const rows = byId(c.mount, 'tracks').children;
  const sub = (li) => {
    const n = byClass(li, 'track-sub');
    return n ? n.textContent : '';
  };
  assert.equal(sub(rows[0]), 'Slides · 720p · trackOnHost:media.example.net', 'the hint, the quality, then the host');
  assert.ok(!sub(rows[1]).includes('trackOnHost'), 'the page’s own host');
  assert.ok(!sub(rows[2]).includes('trackOnHost'), 'a host under the page’s');
  assert.equal(sub(rows[3]), 'trackOnHost:media.example.net', 'the host name only');
  assert.ok(!sub(rows[4]).includes('trackOnHost') && !sub(rows[5]).includes('trackOnHost'), 'no host, or none that is a host name: nothing');
  for (const li of rows) {
    const box = byClass(li, 'track-row').children[0];
    assert.match(box.attrs['aria-label'], /^selectTrack:[^:]+$/, 'the check box is named by the track as before');
    assert.ok(!box.attrs['aria-label'].includes('example'), box.attrs['aria-label']);
  }
  // the page's host unknown: the host is named
  const { trackHostNote } = await import('../../src/ui/sendcard.js');
  assert.equal(trackHostNote({ host: 'media.example.net' }, ''), 'trackOnHost:media.example.net');
});

test('EU-03: hosts are of one site only when equal or one under the other (no public suffix list: never one host too few)', async () => {
  const { sameSiteHost, cleanHost } = await import('../../src/ui/sendcard.js');
  assert.equal(sameSiteHost('lms.example.edu', 'LMS.example.edu.'), true);
  assert.equal(sameSiteHost('cdn.lms.example.edu', 'lms.example.edu'), true);
  assert.equal(sameSiteHost('lms.example.edu', 'cdn.lms.example.edu'), true);
  for (const [a, b] of [['video.example.edu', 'www.example.edu'], ['evil.github.io', 'victim.github.io'], ['a.co.uk', 'b.co.uk'], ['notexample.edu', 'example.edu'], ['10.0.0.1', '0.0.1'], ['example.edu', ''], ['', '']]) {
    assert.equal(sameSiteHost(a, b), false, `${a} / ${b}`);
  }
  assert.equal(cleanHost('Media.Example.NET.'), 'media.example.net');
  assert.equal(cleanHost('[::1]'), '[::1]');
  for (const bad of [null, undefined, 42, {}, '', '   ', 'a b', 'a'.repeat(254), `${'a'.repeat(250)}.com`, 'http://']) assert.equal(cleanHost(bad), '', String(bad).slice(0, 20));
});

test('J2: the queued card’s title follows the state; an in-page card is a card, not a window', async () => {
  const c = await queuedCard();
  assert.equal(byId(c.mount, 'state-title').textContent, 'queuedTitle');
  assert.equal(byId(c.mount, 'status-note').textContent, 'closeHint');
  c.card.onStatus({ taskId: 7, state: 'done', lectureId: 'L1' });
  assert.equal(byId(c.mount, 'state-title').textContent, 'doneTitle');
  c.card.onStatus({ taskId: 7, state: 'failed', error: { code: 'downloads.network', params: {} } });
  assert.equal(byId(c.mount, 'state-title').textContent, 'failedTitle');
  const inpage = await cardWith({
    detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] },
    connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } },
    check: { ok: true, item: { sessionId: 's', state: 'downloading', taskId: 7 } },
    status: { ok: true, status: null },
  }, { host: 'inpage' });
  assert.equal(byId(inpage.mount, 'status-note').textContent, 'closeHintCard');
});

/** A popup card on a task of this session that is being fetched (the "已加入队列" view). */
async function queuedCard() {
  const c = await cardWith({
    detect: { ok: true, view: okView(), origins: [], cookieHosts: [], offers: [] },
    connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } },
    check: { ok: true, item: { sessionId: 's', state: 'downloading', taskId: 7 } },
    status: { ok: true, status: null },
    open: { ok: false, error: { code: 'open.not_found', params: {} } },
  });
  assert.equal(c.mount.children[0].dataset.view, 'queued');
  return c;
}

test('G3: a failed fetch says why in words, by LectureFlow’s own retryable / not retryable split; never a bare code after a sentence', async () => {
  const { failedText } = await import('../../src/ui/sendcard.js');
  // retryable (internal/fetch/retryable.go): the way out is LectureFlow's "重试"
  assert.equal(failedText({ code: 'downloads.network' }), 'failNetwork');
  assert.equal(failedText({ code: 'downloads.no_space' }), 'failDisk');
  assert.equal(failedText({ code: 'downloads.write_failed' }), 'failDisk');
  assert.equal(failedText({ code: 'downloads.engine_missing' }), 'failEngineMissing');
  for (const code of ['downloads.http_status', 'downloads.engine_failed', 'downloads.tls_failed', 'downloads.playlist_invalid', 'downloads.proxy_unsupported', 'downloads.merge_failed', 'downloads.import_failed']) assert.equal(failedText({ code }), 'failedHint', code);
  // not retryable: the reason and what to do instead, no "重试"
  assert.equal(failedText({ code: 'downloads.drm_protected' }), 'blockedProtected');
  assert.equal(failedText({ code: 'downloads.site_blocked' }), 'errSiteBlocked');
  assert.equal(failedText({ code: 'downloads.playlist_unsupported' }), 'failUnsupported');
  for (const code of ['downloads.local_address', 'downloads.loopback_address', 'downloads.unusual_host']) assert.equal(failedText({ code }), 'failUnsafeAddress', code);
  assert.equal(failedText({ code: 'downloads.angle_no_video' }), 'failAngle');
  for (const code of ['upload.media_empty', 'upload.media_html', 'upload.media_playlist', 'upload.media_format']) assert.equal(failedText({ code }), 'failNotMedia', code);
  // over the size one fetch may hold (C165b): its own sentence, not retryable
  assert.equal(failedText({ code: 'downloads.too_large', params: { max_bytes: 68719476736 } }), 'failTooLarge');
  // out of space with the partial download kept for resuming (r8 F5): the disk sentence
  assert.equal(failedText({ code: 'downloads.no_space_kept', params: { free_bytes: 1, keep_bytes: 2, task_bytes: 3 } }), 'failDisk');
  // unknown: the code in brackets inside a whole sentence; none at all: the general sentence
  assert.equal(failedText({ code: 'downloads.something_new' }), 'failedCode:downloads.something_new');
  assert.equal(failedText(null), 'failedHint');
  assert.equal(failedText({}), 'failedHint');

  const c = await queuedCard();
  c.card.onStatus({ taskId: 7, state: 'failed', error: { code: 'downloads.drm_protected', params: {} } });
  const texts = textsOf(c.mount);
  assert.ok(texts.includes('blockedProtected'), texts.join(' | '));
  assert.ok(!texts.some((x) => /failedHint|\(downloads\./.test(x)), texts.join(' | '));
  c.card.onStatus({ taskId: 7, state: 'failed', error: { code: 'downloads.what', params: {} } });
  assert.ok(textsOf(c.mount).includes('failedCode:downloads.what'));
});

/**
 * LectureFlow's own table of which failures a retry can fix
 * (internal/fetch/retryable.go failedRetry), read from the Go source:
 * [code, retry] for every entry, the constants resolved through codes.go.
 */
function retryTable() {
  const dir = path.join(root, '..', 'internal', 'fetch');
  const consts = new Map([...fs.readFileSync(path.join(dir, 'codes.go'), 'utf8').matchAll(/^\s*(Code\w+)\s*=\s*"([^"]+)"/gm)].map((m) => [m[1], m[2]]));
  const src = fs.readFileSync(path.join(dir, 'retryable.go'), 'utf8');
  const start = src.indexOf('var failedRetry');
  assert.ok(start >= 0, 'retryable.go has failedRetry');
  const body = src.slice(start, src.indexOf('\n}\n', start));
  return [...body.matchAll(/^\s*(Code\w+|"[^"]+"):\s*\{(true|false),/gm)].map((m) => {
    const code = m[1].startsWith('"') ? JSON.parse(m[1]) : consts.get(m[1]);
    assert.ok(code, `${m[1]} resolves to a code`);
    return [code, m[2] === 'true'];
  });
}

test('G3: every code of LectureFlow’s retry table gets a sentence of its class: a retry can fix it → the way to retry; it cannot → its own reason, no “Retry”', async () => {
  const { failedText } = await import('../../src/ui/sendcard.js');
  const catalog = (dir) => JSON.parse(fs.readFileSync(path.join(root, '_locales', dir, 'messages.json'), 'utf8'));
  const en = catalog('en');
  const zh = catalog('zh_CN');
  const table = retryTable();
  // 27 at 573850d1 (downloads.too_large the last added); more as LectureFlow adds codes
  assert.ok(table.length >= 27, `${table.length} codes read`);
  for (const [code, retry] of table) {
    // the sentence itself, without EU-02's addition for an embedding page
    const key = failedText({ code, params: {} }, { embedded: false }).split(':')[0];
    assert.ok(en[key] && zh[key], `${code}: ${key} is a message`);
    const says = (m) => m.message.includes('“Retry”') || m.message.includes('“重试”') || /can retry/.test(m.message) || /查看原因并重试/.test(m.message);
    if (retry) assert.ok(says(en[key]) && says(zh[key]), `${code} (retryable): ${key} says how to retry`);
    else {
      assert.ok(key !== 'failedHint' && key !== 'failedCode', `${code} (not retryable): a sentence of its own, not ${key}`);
      assert.ok(!says(en[key]) && !says(zh[key]), `${code} (not retryable): ${key} never sends the user to “Retry”`);
    }
  }
});

/** A card (popup unless said) on a task of this session being fetched, on this view. */
async function queuedOn(viewExtra, { host = 'popup' } = {}) {
  const c = await cardWith({
    detect: { ok: true, view: okView(viewExtra), origins: [], cookieHosts: [], offers: [] },
    connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } },
    check: { ok: true, item: { sessionId: 's', state: 'downloading', taskId: 7 } },
    status: { ok: true, status: null },
  }, { host });
  assert.equal(c.mount.children[0].dataset.view, 'queued');
  return c;
}

const rejected = { code: 'downloads.credentials_rejected', params: { status: 403 } };
const onHost = (h) => [{ ...tr(0, 'video'), host: h }, { ...tr(0, 'audio'), host: h }];

test('EU-02: the platform refused the fetch (401 / 403) after a send from a page that embeds the recording: send it from the player’s own page', async () => {
  const { failedText, refusedSignIn } = await import('../../src/ui/sendcard.js');
  // which failures: credentials_rejected (LectureFlow's 401 / 403), or http_status with 401 / 403
  assert.equal(failedText(rejected), 'failedHint failEmbeddedHint');
  assert.equal(failedText(rejected, { embedded: false }), 'failedHint');
  assert.equal(failedText({ code: 'downloads.http_status', params: { status: 401 } }), 'failedHint failEmbeddedHint');
  for (const e of [{ code: 'downloads.http_status', params: { status: 500 } }, { code: 'downloads.http_status', params: {} }, { code: 'downloads.http_status' }, { code: 'downloads.credentials_expired', params: {} }, { code: 'downloads.network' }, null]) {
    assert.equal(refusedSignIn(e), false, JSON.stringify(e));
    assert.ok(!failedText(e).includes('failEmbeddedHint'), JSON.stringify(e));
  }

  // the popup: the page (view.host, the tab's top-level page) and the tracks' hosts
  const embedded = await queuedOn({ host: 'lms.example.edu', tracks: onHost('media.example.net') });
  embedded.card.onStatus({ taskId: 7, state: 'failed', error: rejected });
  assert.equal(byId(embedded.mount, 'status-note').textContent, 'failedHint failEmbeddedHint');
  const own = await queuedOn({ host: 'video.example.net', tracks: onHost('cdn.video.example.net') });
  own.card.onStatus({ taskId: 7, state: 'failed', error: rejected });
  assert.equal(byId(own.mount, 'status-note').textContent, 'failedHint', 'on the player’s own site: nothing to add');
  // the tracks' hosts unknown, or an in-page card (its page may be a frame): it may be embedded
  const unknown = await queuedOn({ host: 'video.example.net' });
  unknown.card.onStatus({ taskId: 7, state: 'failed', error: rejected });
  assert.equal(byId(unknown.mount, 'status-note').textContent, 'failedHint failEmbeddedHint');
  const inpage = await queuedOn({ host: 'video.example.net', tracks: onHost('video.example.net') }, { host: 'inpage' });
  inpage.card.onStatus({ taskId: 7, state: 'failed', error: rejected });
  assert.equal(byId(inpage.mount, 'status-note').textContent, 'failedHint failEmbeddedHint');
});

test('EU-02: "续传" after the platform refused the fetch says it too; not when the sign-in data merely expired', async () => {
  const banner = (c) => byId(c.mount, 'reason').textContent;
  const embedded = await queuedOn({ host: 'lms.example.edu', tracks: onHost('media.example.net') });
  embedded.card.onStatus({ taskId: 7, state: 'needs_refresh', error: rejected });
  assert.equal(embedded.mount.children[0].dataset.view, 'refresh');
  assert.equal(banner(embedded), 'refreshText failEmbeddedHint');
  const expired = await queuedOn({ host: 'lms.example.edu', tracks: onHost('media.example.net') });
  expired.card.onStatus({ taskId: 7, state: 'needs_refresh', error: { code: 'downloads.credentials_expired', params: {} } });
  assert.equal(banner(expired), 'refreshText');
  const own = await queuedOn({ host: 'video.example.net', tracks: onHost('video.example.net') });
  own.card.onStatus({ taskId: 7, state: 'needs_refresh', error: rejected });
  assert.equal(banner(own), 'refreshText');
  // a card opened on a task that already needs a refresh: the status the check brought
  const later = await cardWith({
    detect: { ok: true, view: okView({ host: 'lms.example.edu', tracks: onHost('media.example.net') }), origins: [], cookieHosts: [], offers: [] },
    connection: { ok: true, connection: { status: 'connected', courses: [{ id: 'c1', name: 'C' }], lastCourseId: 'c1' } },
    check: { ok: true, item: { sessionId: 's', state: 'needs_refresh', taskId: 7 }, status: { taskId: 7, state: 'needs_refresh', error: rejected } },
  });
  assert.equal(later.mount.children[0].dataset.view, 'refresh');
  assert.equal(banner(later), 'refreshText failEmbeddedHint');
});

test('EU-02: in Chinese the added sentence follows the full stop without a space', async () => {
  const { failedText } = await import('../../src/ui/sendcard.js');
  const { useCatalog } = await import('../../src/ui/dom.js');
  const zh = JSON.parse(fs.readFileSync(path.join(root, '_locales', 'zh_CN', 'messages.json'), 'utf8'));
  const en = JSON.parse(fs.readFileSync(path.join(root, '_locales', 'en', 'messages.json'), 'utf8'));
  try {
    useCatalog({ lang: 'zh-CN', messages: zh });
    assert.equal(failedText(rejected), `${zh.failedHint.message}${zh.failEmbeddedHint.message}`);
    useCatalog({ lang: 'en', messages: en });
    assert.equal(failedText(rejected), `${en.failedHint.message} ${en.failEmbeddedHint.message}`);
  } finally {
    useCatalog(null);
  }
});

test('G3: "在 LectureFlow 中打开" for a lecture or task LectureFlow no longer has says so, not "请重试"', async () => {
  const { errorText } = await import('../../src/ui/sendcard.js');
  assert.equal(errorText({ code: 'open.not_found' }), 'errOpenGone');
  const c = await queuedCard();
  await c.click('open');
  assert.equal(byId(c.mount, 'msg').textContent, 'errOpenGone');
});
