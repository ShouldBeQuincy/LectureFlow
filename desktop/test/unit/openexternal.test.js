// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// window.lectureflow.openExternal only after a click or key press, and not
// too often (security audit FB1 L-6: an outward channel after an XSS).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const SRC = path.join(__dirname, '..', '..', 'src');

test('rate limit: a few links in a short time, then none until the window has passed', () => {
  const { createRateLimit, OPEN_EXTERNAL_LIMIT } = require('../../src/main/lib/ratelimit');
  assert.deepEqual(OPEN_EXTERNAL_LIMIT, { max: 3, windowMs: 10000 });
  let now = 1000;
  const take = createRateLimit({ max: 3, windowMs: 10000, now: () => now });
  assert.equal(take(), true);
  now += 100;
  assert.equal(take(), true);
  assert.equal(take(), true);
  assert.equal(take(), false, 'the fourth within 10 s');
  now += 5000;
  assert.equal(take(), false, 'still within 10 s of the first');
  now = 1000 + 10000;
  assert.equal(take(), true, 'the first has left the window');
  assert.equal(take(), false, 'the other two are still in it');
  now += 1_000_000;
  for (let i = 0; i < 3; i++) assert.equal(take(), true);
  assert.equal(take(), false);
  // a refused call does not count
  now += 10000;
  assert.equal(take(), true);
  // a clock that goes back does not open the gate
  now -= 60000;
  assert.equal(take(), true);
  assert.equal(take(), true);
  assert.equal(take(), false);
  assert.throws(() => createRateLimit({ max: 0, windowMs: 1 }), /max/);
  assert.throws(() => createRateLimit({ max: 1, windowMs: 0 }), /windowMs/);
});

/**
 * loadPreload(activation) → { api, calls }: preload.js run in a sandbox
 * like the app page's (http://127.0.0.1), with a stand-in electron module.
 * activation is navigator.userActivation (or undefined).
 */
function loadPreload(activation) {
  const calls = [];
  const exposed = {};
  const electron = {
    contextBridge: { exposeInMainWorld: (name, api) => { exposed[name] = api; } },
    ipcRenderer: {
      invoke: (...args) => {
        calls.push(args);
        return Promise.resolve(true);
      },
      send: () => {},
    },
    webUtils: { getPathForFile: () => '' },
  };
  const navigator = activation === undefined ? {} : { userActivation: activation };
  const sandbox = {
    require: (m) => {
      if (m !== 'electron') throw new Error(`unexpected require ${m}`);
      return electron;
    },
    window: { location: { protocol: 'http:', hostname: '127.0.0.1' } },
    navigator,
    File: class File {},
    Error,
    TypeError,
    Promise,
    Object,
    Array,
    Number,
    String,
  };
  vm.runInNewContext(fs.readFileSync(path.join(SRC, 'preload.js'), 'utf8'), sandbox, { filename: 'preload.js', timeout: 5000 });
  return { api: exposed.lectureflow, calls };
}

test('preload: openExternal goes to the main process only while the page has a user activation', async () => {
  for (const [what, activation] of [['no activation', { isActive: false, hasBeenActive: true }], ['no API', undefined], ['odd value', { isActive: 'yes' }]]) {
    const { api, calls } = loadPreload(activation);
    await assert.rejects(api.openExternal('https://example.com/help'), /click/, what);
    assert.equal(calls.length, 0, `${what}: nothing sent`);
  }
  const { api, calls } = loadPreload({ isActive: true, hasBeenActive: true });
  assert.equal(await api.openExternal('https://example.com/help'), true);
  assert.deepEqual(calls, [['lf:open-external', 'https://example.com/help']]);
});

test('preload: the activation is read in the preload world, never from the page', () => {
  const src = fs.readFileSync(path.join(SRC, 'preload.js'), 'utf8');
  assert.match(src, /navigator\.userActivation/);
  assert.doesNotMatch(src, /window\.navigator/, 'the page may replace window.navigator in its own world only, but read the global');
});

test('ipc.js: lf:open-external checks the rate limit after the URL check, before the system browser', () => {
  const src = fs.readFileSync(path.join(SRC, 'main', 'ipc.js'), 'utf8');
  const start = src.indexOf("handle('lf:open-external'");
  assert.ok(start > 0);
  const body = src.slice(start, src.indexOf('});', start));
  assert.match(body, /validateOpenExternal/, 'the URL is checked first (by handle)');
  const limit = body.indexOf('openExternalLimit()');
  const open = body.indexOf('shell.openExternal(url)');
  assert.ok(limit > 0 && open > limit, 'the limit comes before shell.openExternal');
  // review R44: one limit shared with the navigation / window.open path, made by the shell
  assert.match(src, /const \{ openExternalLimit \} = ctx;/);
  // ipc.js makes no openExternal limit of its own; the only limit it makes
  // is lf:notify's (final security audit AUD2 L3)
  assert.doesNotMatch(src, /OPEN_EXTERNAL_LIMIT/, 'ipc.js makes no openExternal limit of its own');
  assert.equal((src.match(/createRateLimit\(/g) || []).length, 1, 'one limit made in ipc.js');
  assert.match(src, /const notifyLimit = createRateLimit\(NOTIFY_LIMIT\);/);
});

test('shell.js: links from navigation and window.open share the same limit, dropped quietly (review R44)', () => {
  const src = fs.readFileSync(path.join(SRC, 'main', 'shell.js'), 'utf8');
  assert.match(src, /const \{ OPEN_EXTERNAL_LIMIT, createRateLimit \} = require\('\.\/lib\/ratelimit'\);/);
  const ctor = src.slice(src.indexOf('  constructor('), src.indexOf('this.api = createApiClient'));
  assert.match(ctor, /this\.openExternalLimit = createRateLimit\(OPEN_EXTERNAL_LIMIT\);/, 'made once, in the constructor');
  assert.equal((src.match(/createRateLimit\(/g) || []).length, 1);
  const reg = src.slice(src.indexOf('registerIpc({'), src.indexOf('});', src.indexOf('registerIpc({')));
  assert.match(reg, /openExternalLimit: this\.openExternalLimit,/, 'the IPC handler gets the same limit');
  const fn = src.slice(src.indexOf('  openExternal(url) {'), src.indexOf('  // ---- activity polling'));
  const check = fn.indexOf('isExternalWebUrl(url)');
  const limit = fn.indexOf('this.openExternalLimit()');
  const open = fn.indexOf('shell.openExternal(url)');
  assert.ok(check > 0 && limit > check && open > limit, 'URL check, then the limit, then the system browser');
  assert.match(fn, /this\.log\.warn\(/, 'a log line when dropped');
  assert.doesNotMatch(fn, /dialog|Notification|showErrorBox/, 'no message box');
  // no user gesture check on this path (navigation carries no reliable one)
  assert.doesNotMatch(fn, /userActivation|gesture/i);
});
