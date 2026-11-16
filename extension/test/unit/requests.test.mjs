// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// src/background/requests.js (the webRequest route) against a fake
// `browser`: what a tab's request records may hold (security audit AUD3
// 中-2): an address longer than LIMITS.urlLength is not recorded at all, a
// Content-Type is cut, a tab's records hold at most a set number of address
// characters, and the time a response takes does not grow with the
// addresses already kept. Within the limits, the records are as before.

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { registerHooks } from 'node:module';

const STUBS = {
  '../build-info.js': "export const BUILD = Object.freeze({ target: 'chrome', version: '0.0.0', e2e: false });",
  '../vendor/browser-polyfill.js': '',
};
registerHooks({
  resolve(specifier, context, next) {
    if (Object.hasOwn(STUBS, specifier) && String(context.parentURL || '').includes('/src/')) {
      return { url: `data:text/javascript,${encodeURIComponent(STUBS[specifier])}`, shortCircuit: true };
    }
    return next(specifier, context);
  },
});

const listeners = {};
const session = { stored: new Map(), sets: [] };
globalThis.browser = {
  runtime: { getURL: (r) => `ext://lf/${r}`, getManifest: () => ({ version: '0.0.0' }), id: 'lf' },
  i18n: { getUILanguage: () => 'en', getMessage: (k) => k },
  storage: {
    local: { get: async () => ({}), set: async () => {} },
    session: {
      get: async (k) => (session.stored.has(k) ? { [k]: structuredClone(session.stored.get(k)) } : {}),
      set: async (o) => {
        for (const [k, v] of Object.entries(o)) {
          session.sets.push({ key: k, chars: JSON.stringify(v).length });
          session.stored.set(k, structuredClone(v));
        }
      },
      remove: async (k) => {
        session.stored.delete(k);
      },
    },
  },
  webRequest: {
    onHeadersReceived: { addListener: (f) => (listeners.headers = f) },
    onBeforeRequest: { addListener: (f) => (listeners.main = f) },
  },
};

const requests = await import('../../src/background/requests.js');
const { LIMITS } = await import('../../src/lib/detect.js');
let candidates = [];
requests.install((tabId) => candidates.push(tabId));

let nextTab = 100;
beforeEach(() => {
  candidates = [];
  session.sets = [];
  requests.setBufferLimits();
});

const response = (tabId, url, { type = 'video/mp4', length = '1000', range = '', status = 200, frameId = 0 } = {}) => {
  const responseHeaders = [{ name: 'Content-Type', value: type }];
  if (length) responseHeaders.push({ name: 'Content-Length', value: length });
  if (range) responseHeaders.push({ name: 'Content-Range', value: range });
  listeners.headers({ tabId, frameId, method: 'GET', type: 'xmlhttprequest', statusCode: status, url, responseHeaders, initiator: 'https://granted.example' });
};
const persisted = () => new Promise((r) => setTimeout(r, 600));
// an address of exactly n characters
const urlOf = (n, i = 0) => {
  const head = `https://granted.example/v/${i}.mp4?p=`;
  return head + 'a'.repeat(n - head.length);
};

test('an address longer than LIMITS.urlLength is not recorded: not kept, not stored, no detection started', async () => {
  const tab = nextTab++;
  response(tab, urlOf(LIMITS.urlLength + 1));
  assert.deepEqual(await requests.requestsFor(tab), []);
  assert.deepEqual(candidates, [], 'no automatic detection for it');
  await persisted();
  assert.equal(session.sets.filter((s) => s.key === `wr:${tab}`).length, 0, 'nothing written to storage.session');
  // exactly the limit: recorded as ever
  response(tab, urlOf(LIMITS.urlLength));
  const kept = await requests.requestsFor(tab);
  assert.deepEqual(kept.map((e) => e.url.length), [LIMITS.urlLength]);
  assert.deepEqual(candidates, [tab]);
});

test('the audit’s probe: 300 responses with 32 K-character addresses keep nothing; ordinary ones after them all fit', async () => {
  const tab = nextTab++;
  const t0 = performance.now();
  for (let i = 0; i < 400; i++) response(tab, urlOf(32768, i));
  const spent = performance.now() - t0;
  assert.deepEqual(await requests.requestsFor(tab), []);
  // before: about 13 ms a response once 300 long ones were kept (P4), 4 s or more for these
  assert.ok(spent < 2000, `400 long responses took ${spent.toFixed(0)} ms`);
  for (let i = 0; i < 300; i++) response(tab, `https://granted.example/sessions/0123456789abcdef/v/${i}.mp4?Expires=1924992000&Signature=MARKER`);
  const kept = await requests.requestsFor(tab);
  assert.equal(kept.length, 300);
  await persisted();
  const sets = session.sets.filter((s) => s.key === `wr:${tab}`);
  // 300 ordinary records, about 90 K characters of JSON (before: 9.4 Mi, P4)
  assert.ok(sets.length >= 1 && sets.every((s) => s.chars < 256 * 1024), JSON.stringify(sets));
});

test('a Content-Type is kept to 256 characters', async () => {
  const tab = nextTab++;
  response(tab, 'https://granted.example/v/a.mp4', { type: `video/mp4; codecs="${'x'.repeat(5000)}"` });
  const [e] = await requests.requestsFor(tab);
  assert.equal(e.contentType.length, 256);
  assert.ok(e.contentType.startsWith('video/mp4; codecs="'));
});

test('a tab’s records hold at most a set number of address characters; a URL already kept is still updated', async () => {
  requests.setBufferLimits({ tabChars: 1000 });
  const tab = nextTab++;
  const a = urlOf(400, 1);
  const b = urlOf(400, 2);
  const c = urlOf(400, 3);
  response(tab, a, { length: '' });
  response(tab, b);
  response(tab, c);
  assert.deepEqual((await requests.requestsFor(tab)).map((e) => e.url), [a, b], 'the third would pass 1000 characters');
  // the same URL again, with a length this time: the more informative response replaces it
  response(tab, a, { length: '777' });
  assert.deepEqual((await requests.requestsFor(tab)).map((e) => [e.url, e.contentLength]), [[a, 777], [b, 1000]]);
  // navigation forgets the records and the count
  listeners.main({ tabId: tab, type: 'main_frame', url: 'https://granted.example/next' });
  response(tab, c);
  assert.deepEqual((await requests.requestsFor(tab)).map((e) => e.url), [c]);
});

test('the count limit as before: 300 a tab; a 206 without a total does not replace a 200 with a length', async () => {
  const tab = nextTab++;
  for (let i = 0; i < 301; i++) response(tab, `https://granted.example/v/${i}.mp4`);
  const kept = await requests.requestsFor(tab);
  assert.equal(kept.length, 300);
  assert.ok(!kept.some((e) => e.url.endsWith('/300.mp4')));
  response(tab, 'https://granted.example/v/7.mp4', { status: 206, length: '' });
  const seven = (await requests.requestsFor(tab)).find((e) => e.url.endsWith('/7.mp4'));
  assert.deepEqual([seven.statusCode, seven.contentLength], [200, 1000]);
  response(tab, 'https://granted.example/v/7.mp4', { status: 206, length: '', range: 'bytes 0-1/999999' });
  const again = (await requests.requestsFor(tab)).find((e) => e.url.endsWith('/7.mp4'));
  assert.deepEqual([again.statusCode, again.contentRangeTotal], [206, 999999], 'a 206 with a total does');
});

test('records of the documents scanned: Chrome by document id, Firefox (no id) by the origin the request came from (SEC46 低-1)', async () => {
  const tab = nextTab++;
  const raw = (url, frameId, extra) =>
    listeners.headers({ tabId: tab, frameId, method: 'GET', type: 'media', statusCode: 200, url, responseHeaders: [{ name: 'Content-Type', value: 'video/mp4' }], ...extra });
  raw('https://granted.example/v/ff-own.mp4', 5, { documentUrl: 'https://granted.example/embed/1?x=1' });
  raw('https://granted.example/v/ff-before.mp4', 5, { documentUrl: 'https://attacker.example/a.html' });
  raw('https://granted.example/v/ff-unscanned.mp4', 9, { documentUrl: 'https://evil.example/frame.html' });
  raw('https://granted.example/v/ff-opaque.mp4', 5, { documentUrl: 'about:srcdoc' });
  raw('https://granted.example/v/ff-none.mp4', 5, {});
  raw('https://granted.example/v/cr-own.mp4', 5, { documentId: 'D5', initiator: 'https://granted.example' });
  raw('https://granted.example/v/cr-gone.mp4', 5, { documentId: 'D5-old', initiator: 'https://granted.example' });
  const names = (list) => list.map((e) => e.url.replace('https://granted.example/v/', ''));
  // the frame shows a granted.example document now (Chrome: D5; Firefox: no id)
  const frameOrigins = new Map([[0, 'https://lms.example'], [5, 'https://granted.example']]);
  assert.deepEqual(names(await requests.requestsFor(tab, ['D0', 'D5'], { frameOrigins })), ['ff-own.mp4', 'cr-own.mp4']);
  assert.deepEqual(names(await requests.requestsFor(tab, [], { frameOrigins })), ['ff-own.mp4', 'cr-own.mp4', 'cr-gone.mp4'], 'no ids scanned (Firefox): ids are not checked, as before');
  assert.deepEqual(names(await requests.requestsFor(tab, [], { frameOrigins: new Map([[5, 'https://attacker.example']]) })), ['ff-before.mp4', 'cr-own.mp4', 'cr-gone.mp4']);
  assert.deepEqual(names(await requests.requestsFor(tab, [], { frameOrigins: {} })), ['cr-own.mp4', 'cr-gone.mp4'], 'not a map: no frame has an origin');
  // the records themselves: the initiator stays with them for the screening (blockedframes.js)
  assert.equal((await requests.requestsFor(tab, [], { frameOrigins })).find((e) => e.url.endsWith('ff-own.mp4')).initiator, 'https://granted.example');
  // without frameOrigins (the end-to-end build's look at the records): as before
  assert.equal((await requests.requestsFor(tab)).length, 7);
});

test('records restored from storage.session (a restarted background) are held to the same limits', async () => {
  const tab = nextTab++;
  const ok = 'https://granted.example/v/ok.mp4';
  session.stored.set(`wr:${tab}`, [
    { url: ok, frameId: 0, resourceType: 'media', statusCode: 200, contentType: 'video/mp4', contentLength: 1, contentRangeTotal: null, documentId: null, initiator: '' },
    { url: urlOf(LIMITS.urlLength + 1, 9), frameId: 0, resourceType: 'media', statusCode: 200, contentType: 'video/mp4', contentLength: 1, contentRangeTotal: null, documentId: null, initiator: '' },
    { url: 42, frameId: 0 },
    null,
    { url: 'https://granted.example/v/long-type.mp4', frameId: 0, resourceType: 'media', statusCode: 200, contentType: 'x'.repeat(3000), contentLength: 1, contentRangeTotal: null, documentId: null, initiator: '' },
  ]);
  const kept = await requests.requestsFor(tab);
  assert.deepEqual(kept.map((e) => e.url), [ok, 'https://granted.example/v/long-type.mp4']);
  assert.equal(kept[1].contentType.length, 256);
});
