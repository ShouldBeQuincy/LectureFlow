// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Page-response observation (profile `observe`, lead ruling 1): pointer
// rules, configuration, re-validation, combining, registration patterns —
// and the two content scripts themselves, run in a sandbox with a fake page:
// only the declared values leave the page, other fields of the same
// response (the viewer's e-mail and name) never do, and the page gets its
// responses untouched.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';
import {
  parsePointer, readPointer, isEmptyObserved, observeConfig, specMatches, cleanObservedRecord, combineObserved,
  observerMatches, hostWildcardCovers, profileKey, MAX_STRING,
} from '../../src/lib/observe.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
const KEY = profileKey(panopto);
const PAGE = 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=1d2e3f40-0000-4000-8000-0000000000e1';
const INFO = 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer/DeliveryInfo.aspx';
const SECRET_EMAIL = 'unit-student@example.edu';
const SECRET_NAME = 'UnitStudentName';

/** A DeliveryInfo-like answer: what the profile wants, and what it must never take. */
function deliveryInfo({ downloadUrl = null, podcasts = ['https://cdn.example.net/sessions/s/p.mp4?sig=1'] } = {}) {
  return {
    Delivery: {
      SessionName: 'CSC1003 Lecture',
      PodcastStreams: podcasts.map((u) => ({ StreamUrl: u, Name: SECRET_NAME, Owner: { Email: SECRET_EMAIL } })),
      Streams: [{ StreamUrl: 'https://cdn.example.net/a/master.m3u8' }],
    },
    DownloadUrl: downloadUrl,
    UserEmail: SECRET_EMAIL,
    UserName: SECRET_NAME,
  };
}

// ------------------------------------------------------------- pointers

test('JSON pointers: RFC 6901 plus a "*" segment; no root, no empty segment', () => {
  assert.deepEqual(parsePointer('/DownloadUrl'), [{ key: 'DownloadUrl' }]);
  assert.deepEqual(parsePointer('/a~1b/c~0d'), [{ key: 'a/b' }, { key: 'c~d' }]);
  assert.deepEqual(parsePointer('/Delivery/PodcastStreams/*/StreamUrl'), [{ key: 'Delivery' }, { key: 'PodcastStreams' }, { any: true }, { key: 'StreamUrl' }]);
  for (const bad of ['', '/', 'DownloadUrl', '/a//b', '/a~2', '/a~', '/a*b', '/*x', '/1/2/3/4/5/6/7/8/9', null, 3]) assert.equal(parsePointer(bad), null, String(bad));
});

test('reading a pointer yields scalars only: an object or a whole array never leaves', () => {
  const doc = deliveryInfo();
  assert.equal(readPointer(doc, parsePointer('/DownloadUrl')), null);
  assert.deepEqual(readPointer(doc, parsePointer('/Delivery/PodcastStreams/*/StreamUrl')), ['https://cdn.example.net/sessions/s/p.mp4?sig=1']);
  assert.equal(readPointer(doc, parsePointer('/Delivery')), undefined, 'an object is dropped');
  assert.equal(readPointer(doc, parsePointer('/Delivery/PodcastStreams')), undefined, 'an array without "*" is dropped');
  assert.deepEqual(readPointer(doc, parsePointer('/Delivery/PodcastStreams/*/Owner')), [], 'objects under "*" are dropped');
  assert.equal(readPointer(doc, parsePointer('/Delivery/PodcastStreams/0/StreamUrl')), 'https://cdn.example.net/sessions/s/p.mp4?sig=1');
  assert.equal(readPointer(doc, parsePointer('/Missing/Field')), undefined);
  assert.equal(readPointer({ a: 'x'.repeat(MAX_STRING + 1) }, parsePointer('/a')), undefined, 'over-long strings are dropped');
  assert.equal(readPointer({ a: NaN }, parsePointer('/a')), undefined);
  assert.equal(readPointer({ a: [1, 2, 3] }, parsePointer('/a/5')), undefined);
  const many = { a: Array.from({ length: 40 }, (_, i) => ({ u: `u${i}` })) };
  assert.equal(readPointer(many, parsePointer('/a/*/u')).length, 16);
  // inherited properties are not own fields
  assert.equal(readPointer({}, parsePointer('/constructor')), undefined);
});

test('empty observed values', () => {
  for (const v of [undefined, null, '', false, [], [null, '']]) assert.equal(isEmptyObserved(v), true, JSON.stringify(v));
  for (const v of ['x', 0, true, ['https://x']]) assert.equal(isEmptyObserved(v), false, JSON.stringify(v));
});

// --------------------------------------------------------- configuration

test('configuration: the declared requests, on the profile’s own hosts only', () => {
  const cfg = observeConfig(panopto);
  assert.equal(cfg.key, KEY);
  assert.deepEqual(cfg.specs[0].pointers, panopto.observe[0].pointers);
  assert.equal(cfg.specs[0].pointers.podcastUrl, '/Delivery/PodcastStreams/*/StreamUrl');
  assert.equal(specMatches(cfg.specs[0], INFO), true);
  assert.equal(specMatches(cfg.specs[0], `${INFO}?x=1`), true, 'the query does not matter');
  assert.equal(specMatches(cfg.specs[0], INFO.replace('tenant.cloud.panopto.eu', 'evil.example.com')), false, 'another host');
  assert.equal(specMatches(cfg.specs[0], 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer/Other.aspx'), false);
  assert.equal(observeConfig({ ...panopto, observe: undefined }), null);
});

test('re-validation: undeclared fields, other profiles, other requests and odd shapes are dropped', () => {
  const ok = { key: KEY, index: 0, url: `${INFO}?q=1`, values: { downloadUrl: null, podcastUrl: ['https://cdn.example.net/x.mp4'], UserEmail: SECRET_EMAIL } };
  const clean = cleanObservedRecord(panopto, ok);
  assert.deepEqual(clean, { key: KEY, index: 0, url: INFO, values: { downloadUrl: null, podcastUrl: ['https://cdn.example.net/x.mp4'] } });
  assert.ok(!JSON.stringify(clean).includes(SECRET_EMAIL));
  assert.equal(cleanObservedRecord(panopto, { ...ok, key: 'panopto:0.9.0' }), null);
  assert.equal(cleanObservedRecord(panopto, { ...ok, index: 3 }), null);
  assert.equal(cleanObservedRecord(panopto, { ...ok, url: 'https://evil.example.com/Panopto/Pages/Viewer/DeliveryInfo.aspx' }), null);
  assert.deepEqual(cleanObservedRecord(panopto, { ...ok, values: { downloadUrl: { a: 1 }, podcastUrl: [{ a: 1 }, 'x'.repeat(5000), 'ok'] } }).values, { podcastUrl: ['ok'] });
});

test('combining frames: the top frame first, "seen" only for answered requests', () => {
  const frames = [
    { frameId: 4, observed: [{ key: KEY, index: 0, url: INFO, values: { downloadUrl: 'https://tenant.cloud.panopto.eu/dl', podcastUrl: [] } }] },
    { frameId: 0, observed: [{ key: KEY, index: 0, url: INFO, values: { downloadUrl: null, podcastUrl: ['https://cdn.example.net/x.mp4'] } }] },
  ];
  const c = combineObserved(panopto, frames);
  assert.deepEqual([...c.seen], [0]);
  assert.equal(c.values.downloadUrl, null);
  assert.equal(c.urls.podcastUrl, INFO);
  assert.equal(combineObserved(panopto, [{ frameId: 0 }]).seen.size, 0);
});

test('registration: the main-world observer goes on granted origins that belong to an observing profile', () => {
  assert.equal(hostWildcardCovers('*.panopto.eu', '*.cloud.panopto.eu'), true);
  assert.equal(hostWildcardCovers('*.cloud.panopto.eu', '*.panopto.eu'), false);
  assert.equal(hostWildcardCovers('x.cloud.panopto.eu', 'x.cloud.panopto.eu'), true);
  const other = { ...panopto, id: 'other', observe: undefined, match: { hosts: ['*.example.edu'] } };
  // one tenant granted: only that tenant
  assert.deepEqual(observerMatches(['https://myuni.cloud.panopto.eu/*'], [panopto, other]), ['https://myuni.cloud.panopto.eu/*']);
  // the profile's own wide grant: its hosts
  assert.ok(observerMatches(['https://*.panopto.eu/*'], [panopto]).includes('https://*.cloud.panopto.eu/*'));
  // a site of a profile that observes nothing, or of no profile: nothing
  assert.deepEqual(observerMatches(['https://video.example.edu/*', 'https://lms.example.org/*'], [panopto, other]), []);
  assert.deepEqual(observerMatches([], [panopto]), []);
});

// ------------------------------------------------- the content scripts

const MAIN_SRC = fs.readFileSync(path.join(root, 'src', 'content', 'observe-main.js'), 'utf8');
const ISOLATED_SRC = fs.readFileSync(path.join(root, 'src', 'content', 'observe.js'), 'utf8');

class FakeXHR {
  constructor() {
    this.listeners = {};
    this.status = 0;
    this.responseType = '';
    this.responseURL = '';
    this.responseText = '';
    this.response = null;
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
}
FakeXHR.prototype.open = function (method, url) {
  this.url = url;
  this.opened = true;
};
FakeXHR.prototype.send = function () {
  this.sent = true;
};

/** A page: document events, fetch answering from a table, XHR, timers under control. */
function makePage(answers) {
  const document = new EventTarget();
  const events = [];
  const origDispatch = document.dispatchEvent.bind(document);
  document.dispatchEvent = (ev) => {
    events.push({ type: ev.type, detail: ev.detail });
    return origDispatch(ev);
  };
  const timers = [];
  const fetchCalls = [];
  const sandbox = {
    document,
    location: { href: PAGE },
    URL,
    CustomEvent,
    EventTarget,
    JSON,
    Promise,
    Response,
    console,
    isFinite,
    setTimeout: (fn, ms) => timers.push({ fn, ms }),
    XMLHttpRequest: FakeXHR,
    fetch: (input) => {
      fetchCalls.push(String(input));
      const a = answers[String(input)];
      return Promise.resolve(new Response(a ? JSON.stringify(a) : 'nope', { status: a ? 200 : 404, headers: { 'content-type': 'application/json; charset=utf-8' } }));
    },
  };
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  return { sandbox, document, events, timers, fetchCalls, run: (src) => vm.runInContext(src, sandbox) };
}

// Objects made inside the sandbox belong to another realm.
const plain = (v) => JSON.parse(JSON.stringify(v));
const tick = () => new Promise((r) => setTimeout(r, 5));
const configEvent = (page, cfg) => page.document.dispatchEvent(new CustomEvent('lectureflow-observe-config', { detail: JSON.stringify(cfg) }));
const results = (page) => page.events.filter((e) => e.type === 'lectureflow-observed').map((e) => JSON.parse(e.detail));

test('main world, fetch: only the declared values leave; the page gets the response whole', async () => {
  const page = makePage({ [INFO]: deliveryInfo() });
  page.run(MAIN_SRC);
  assert.equal(page.events[0].type, 'lectureflow-observe-ready');
  configEvent(page, observeConfig(panopto));
  const res = await page.sandbox.fetch(INFO, { method: 'POST' });
  const body = await res.json(); // the page reads its answer as usual
  assert.equal(body.UserEmail, SECRET_EMAIL);
  await tick();
  const out = results(page);
  assert.equal(out.length, 1);
  assert.deepEqual(out[0], { key: KEY, index: 0, url: INFO, values: { downloadUrl: null, podcastUrl: ['https://cdn.example.net/sessions/s/p.mp4?sig=1'], streamUrls: ['https://cdn.example.net/a/master.m3u8'], sessionName: 'CSC1003 Lecture' } });
  const all = JSON.stringify(page.events);
  assert.ok(!all.includes(SECRET_EMAIL) && !all.includes(SECRET_NAME), 'the viewer’s data never crosses');
  // another request of the page: passed through, not read
  const other = await page.sandbox.fetch('https://tenant.cloud.panopto.eu/api/other');
  assert.equal(other.status, 404);
  await tick();
  assert.equal(results(page).length, 1);
});

test('main world, before the configuration arrives: JSON answers wait, then only a match is read', async () => {
  const page = makePage({ [INFO]: deliveryInfo({ downloadUrl: 'https://tenant.cloud.panopto.eu/dl/x.mp4' }), 'https://tenant.cloud.panopto.eu/api/x': { UserEmail: SECRET_EMAIL } });
  page.run(MAIN_SRC);
  await (await page.sandbox.fetch(INFO)).text();
  await (await page.sandbox.fetch('https://tenant.cloud.panopto.eu/api/x')).text();
  await tick();
  assert.equal(results(page).length, 0);
  configEvent(page, observeConfig(panopto));
  await tick();
  const out = results(page);
  assert.equal(out.length, 1);
  assert.equal(out[0].values.downloadUrl, 'https://tenant.cloud.panopto.eu/dl/x.mp4');
  assert.ok(!JSON.stringify(page.events).includes(SECRET_EMAIL));
});

test('main world, no configuration: after 15 s the wrappers only pass through', async () => {
  const page = makePage({ [INFO]: deliveryInfo() });
  page.run(MAIN_SRC);
  const t = page.timers.find((x) => x.ms === 15000);
  assert.ok(t);
  t.fn();
  configEvent(page, observeConfig(panopto)); // too late: ignored
  await (await page.sandbox.fetch(INFO)).json();
  await tick();
  assert.equal(results(page).length, 0);
});

test('main world, XHR: text and json response types; failures and other URLs are not read', async () => {
  const page = makePage({});
  page.run(MAIN_SRC);
  configEvent(page, observeConfig(panopto));
  const XHR = page.sandbox.XMLHttpRequest;
  const fire = (x) => (x.listeners.load || []).forEach((f) => f());
  const a = new XHR();
  a.open('POST', '/Panopto/Pages/Viewer/DeliveryInfo.aspx');
  a.send('x=1');
  assert.ok(a.opened && a.sent, 'the page’s own open and send ran');
  Object.assign(a, { status: 200, responseText: JSON.stringify(deliveryInfo()) });
  fire(a);
  const b = new XHR();
  b.open('POST', INFO);
  b.responseType = 'json';
  b.send();
  Object.assign(b, { status: 200, response: deliveryInfo({ downloadUrl: 'https://tenant.cloud.panopto.eu/dl' }), responseURL: INFO });
  fire(b);
  const c = new XHR();
  c.open('POST', INFO);
  c.send();
  Object.assign(c, { status: 403, responseText: JSON.stringify(deliveryInfo()) });
  fire(c);
  const d = new XHR();
  d.open('GET', 'https://tenant.cloud.panopto.eu/Panopto/api/user');
  d.send();
  Object.assign(d, { status: 200, responseText: JSON.stringify({ UserEmail: SECRET_EMAIL }) });
  fire(d);
  await tick();
  const out = results(page);
  assert.deepEqual(out.map((o) => o.values.downloadUrl), [null, 'https://tenant.cloud.panopto.eu/dl']);
  assert.ok(!JSON.stringify(page.events).includes(SECRET_EMAIL));
});

test('isolated world: asks the background, hands the page its configuration, keeps checked values only', async () => {
  const page = makePage({ [INFO]: deliveryInfo() });
  const asked = [];
  page.sandbox.chrome = { runtime: { sendMessage: (m) => (asked.push(m), Promise.resolve(observeConfig(panopto))) } };
  page.run(ISOLATED_SRC);
  page.run(MAIN_SRC); // the page-world part starting after it: it says "ready" and gets the answer
  await tick();
  // the top frame: no origins it is embedded in (the background checks them against the do-not-handle list)
  assert.deepEqual(plain(asked), [{ cmd: 'observeConfig', ancestors: null }]);
  assert.ok(page.events.some((e) => e.type === 'lectureflow-observe-config'));
  await (await page.sandbox.fetch(INFO)).json();
  await tick();
  const store = page.sandbox.__lectureflowObserver;
  assert.deepEqual(plain(store.observed), [{ key: KEY, index: 0, url: INFO, values: { downloadUrl: null, podcastUrl: ['https://cdn.example.net/sessions/s/p.mp4?sig=1'], streamUrls: ['https://cdn.example.net/a/master.m3u8'], sessionName: 'CSC1003 Lecture' } }]);
  // A page posting its own "result": a declared request already answered is
  // not looked at again (SEC18e 中-5); undeclared names, other requests and
  // other profile versions are refused here too.
  const before = JSON.stringify(store.observed);
  const forge = (r) => page.document.dispatchEvent(new CustomEvent('lectureflow-observed', { detail: JSON.stringify(r) }));
  forge({ key: KEY, index: 0, url: INFO, values: { downloadUrl: 'https://tenant.cloud.panopto.eu/x', UserEmail: SECRET_EMAIL } });
  assert.equal(JSON.stringify(store.observed), before, 'answered once, kept');
  forge({ key: KEY, index: 0, url: 'https://evil.example.com/Panopto/Pages/Viewer/DeliveryInfo.aspx', values: { downloadUrl: 'y' } });
  forge({ key: 'panopto:0.0.1', index: 0, url: INFO, values: { downloadUrl: 'z' } });
  forge({ key: KEY, index: 2, url: INFO, values: { downloadUrl: 'z' } });
  assert.equal(store.observed.length, 1);
  assert.equal(store.observed[0].values.downloadUrl, null);
  assert.ok(!JSON.stringify(store).includes(SECRET_EMAIL));
  // before an answer: a forged result of the declared request is checked as usual
  const page2 = makePage({});
  page2.sandbox.chrome = { runtime: { sendMessage: () => Promise.resolve(observeConfig(panopto)) } };
  page2.run(ISOLATED_SRC);
  await tick();
  page2.document.dispatchEvent(new CustomEvent('lectureflow-observed', { detail: JSON.stringify({ key: KEY, index: 0, url: INFO, values: { downloadUrl: 'https://tenant.cloud.panopto.eu/x', UserEmail: SECRET_EMAIL } }) }));
  assert.deepEqual(plain(Object.keys(page2.sandbox.__lectureflowObserver.observed[0].values)), ['downloadUrl']);
});

/** A clock for the sandbox: every match (two reads) takes `cost` ms. */
function sandboxClock(page, cost) {
  let t = 0;
  let odd = false;
  let reads = 0;
  page.sandbox.performance = {
    now: () => {
      reads++;
      odd = !odd;
      if (!odd) t += cost.ms;
      return t;
    },
  };
  return { reads: () => reads };
}

test('isolated world: forged results are looked at 64 times a page at most (SEC18e 中-5)', async () => {
  const page = makePage({});
  const sent = [];
  page.sandbox.chrome = { runtime: { sendMessage: (m) => (sent.push(m), Promise.resolve(m.cmd === 'observeConfig' ? observeConfig(panopto) : null)) } };
  const cost = { ms: 1 };
  const clock = sandboxClock(page, cost);
  page.run(ISOLATED_SRC);
  await tick();
  // a page script floods results on the platform host that never match: each one would run the regex
  for (let i = 0; i < 1000; i++) {
    page.document.dispatchEvent(new CustomEvent('lectureflow-observed', { detail: JSON.stringify({ key: KEY, index: 0, url: `https://tenant.cloud.panopto.eu/x/${i}`, values: {} }) }));
  }
  assert.ok(clock.reads() <= 64 * 2, `${clock.reads()} clock reads: 64 events at most`);
  // 64 cheap matches are far within the budget: nothing to report. (Had they
  // tripped it, the background would record a content script's trip only
  // for a local profile: src/lib/health.js recordable.)
  assert.deepEqual(sent.filter((m) => m.cmd === 'regexSlow'), []);
});

test('isolated world: a regex the main world found slow runs once more here, under this clock, and is reported only if slow (SEC18e 中-6)', async () => {
  const page = makePage({});
  const sent = [];
  page.sandbox.chrome = { runtime: { sendMessage: (m) => (sent.push(m), Promise.resolve(m.cmd === 'observeConfig' ? observeConfig(panopto) : null)) } };
  const cost = { ms: 1 };
  const clock = sandboxClock(page, cost);
  page.run(ISOLATED_SRC);
  await tick();
  const notice = (r) => page.document.dispatchEvent(new CustomEvent('lectureflow-observe-slow', { detail: JSON.stringify(r) }));
  // fast here: nothing reported (a forged notice cannot frame a fast profile)
  notice({ key: KEY, index: 0, url: 'https://tenant.cloud.panopto.eu/a' });
  assert.equal(clock.reads(), 2, 'one match');
  assert.deepEqual(sent.filter((m) => m.cmd === 'regexSlow'), []);
  // the same regex again: not retested; others refused
  cost.ms = 300;
  notice({ key: KEY, index: 0, url: 'https://tenant.cloud.panopto.eu/b' });
  notice({ key: 'other:1', index: 0, url: 'https://tenant.cloud.panopto.eu/b' });
  notice({ key: KEY, index: 3, url: 'https://tenant.cloud.panopto.eu/b' });
  notice({ key: KEY, index: 0, url: 'https://evil.example.com/b' });
  assert.equal(clock.reads(), 2);
  assert.ok(page.sandbox.__lectureflowObserver.stopped === undefined, 'not stopped: the scan has nothing to say (SEC18f 低-18)');
  // a page with a second declared request: slow here, so reported
  const two = { ...observeConfig(panopto) };
  two.specs = [two.specs[0], { ...two.specs[0] }];
  const page2 = makePage({});
  const sent2 = [];
  page2.sandbox.chrome = { runtime: { sendMessage: (m) => (sent2.push(m), Promise.resolve(m.cmd === 'observeConfig' ? two : null)) } };
  sandboxClock(page2, { ms: 300 });
  page2.run(ISOLATED_SRC);
  await tick();
  page2.document.dispatchEvent(new CustomEvent('lectureflow-observe-slow', { detail: JSON.stringify({ key: KEY, index: 1, url: 'https://tenant.cloud.panopto.eu/a' }) }));
  const slow = plain(sent2.filter((m) => m.cmd === 'regexSlow'));
  assert.equal(slow.length, 1);
  assert.deepEqual({ ...slow[0], ms: 0 }, { cmd: 'regexSlow', owner: KEY, source: panopto.observe[0].urlPattern, ms: 0, reason: 'slow' });
  assert.equal(slow[0].ms, 300);
  // and kept for the scan: the background's card says why the switch is unknown (SEC18f 低-18)
  assert.ok(page2.sandbox.__lectureflowObserver.stopped === KEY, String(page2.sandbox.__lectureflowObserver.stopped));
});

test('main world: a regex too slow stops there and names the regex and address to the isolated world (SEC18e 中-6)', async () => {
  const page = makePage({ [INFO]: deliveryInfo() });
  sandboxClock(page, { ms: 300 });
  page.run(MAIN_SRC);
  configEvent(page, observeConfig(panopto));
  await (await page.sandbox.fetch(INFO)).json();
  await tick();
  const notices = page.events.filter((e) => e.type === 'lectureflow-observe-slow').map((e) => JSON.parse(e.detail));
  assert.deepEqual(notices, [{ key: KEY, index: 0, url: INFO }]);
  // stopped: the next answer is not matched again
  await (await page.sandbox.fetch(INFO)).json();
  await tick();
  assert.equal(page.events.filter((e) => e.type === 'lectureflow-observe-slow').length, 1);
});


test('isolated world: no profile observes here → the page part is told "nothing" and stays inert', async () => {
  const page = makePage({ [INFO]: deliveryInfo() });
  page.sandbox.chrome = { runtime: { sendMessage: () => Promise.resolve(null) } };
  page.run(MAIN_SRC);
  page.run(ISOLATED_SRC);
  await tick();
  await (await page.sandbox.fetch(INFO)).json();
  await tick();
  assert.equal(results(page).length, 0);
  assert.deepEqual(plain(page.sandbox.__lectureflowObserver.observed), []);
});
