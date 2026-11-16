// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

// The IPC surface the app page can reach: what the preload exposes as
// window.lectureflow (the whitelist of SPEC-v3 V1.2) and which channels the
// main process answers. The desktop e2e test (shell.spec.js) checks the
// same whitelist in the real app; this one runs without Electron. Also:
// lf:notify is rate limited (final security audit AUD2 L3).

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const Module = require('node:module');

const { NOTIFY_LIMIT } = require('../../src/main/lib/ratelimit');

const SRC = path.join(__dirname, '..', '..', 'src');

const WHITELIST = ['components', 'getAppVersion', 'getPowerState', 'notify', 'openExternal', 'pathsForDroppedFiles', 'pickFiles', 'pickFolder'];
const HANDLED = ['lf:app-version', 'lf:components-import-file', 'lf:grant-dropped', 'lf:notify', 'lf:open-external', 'lf:pick-files', 'lf:pick-folder', 'lf:power-state'];
const SHELL_PAGE = ['lf-shell:open-logs', 'lf-shell:restore-default', 'lf-shell:retry'];

const appEvent = { senderFrame: { parent: null, url: 'http://127.0.0.1:4000/' }, sender: {} };

function loadPreload() {
  const exposed = {};
  const electron = {
    contextBridge: { exposeInMainWorld: (name, api) => { exposed[name] = api; } },
    ipcRenderer: { invoke: () => Promise.resolve(true), send: () => {} },
    webUtils: { getPathForFile: () => '' },
  };
  const sandbox = {
    require: (m) => {
      if (m !== 'electron') throw new Error(`unexpected require ${m}`);
      return electron;
    },
    window: { location: { protocol: 'http:', hostname: '127.0.0.1' } },
    navigator: {},
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
  return exposed;
}

/**
 * loadIpc() → { handled, listened, handlers, shown, logs }: ipc.js with a
 * stand-in electron module; handlers[channel](event, ...args) runs like
 * ipcMain.handle, shown lists the notifications shown.
 */
function loadIpc() {
  const handled = [];
  const listened = [];
  const handlers = {};
  const shown = [];
  const logs = [];
  class FakeNotification {
    constructor(opts) { this.opts = opts; }
    static isSupported() { return true; }
    on() {}
    show() { shown.push(this.opts); }
  }
  const electron = {
    app: { getVersion: () => '0.0.0-test' },
    BrowserWindow: { fromWebContents: (wc) => (wc ? { id: 1 } : null) },
    dialog: {},
    ipcMain: {
      handle: (ch, fn) => { handled.push(ch); handlers[ch] = fn; },
      on: (ch) => { listened.push(ch); },
    },
    Notification: FakeNotification,
    powerMonitor: { isOnBatteryPower: () => false },
    shell: {},
  };
  const file = require.resolve('../../src/main/ipc');
  delete require.cache[file];
  const load = Module._load;
  Module._load = function (request, ...rest) {
    if (request === 'electron') return electron;
    return load.call(this, request, ...rest);
  };
  let registerIpc;
  try {
    ({ registerIpc } = require('../../src/main/ipc'));
  } finally {
    Module._load = load;
    delete require.cache[file];
  }
  const log = { warn: (m) => logs.push(m), info: (m) => logs.push(m), error: (m) => logs.push(m) };
  registerIpc({
    getPort: () => 4000,
    shellRoot: SRC,
    t: (k) => k,
    showMainWindow: () => {},
    onRetry: () => {},
    onOpenLogs: () => {},
    onRestoreDefault: () => {},
    openExternalLimit: () => true,
    grantPaths: async () => true,
    log,
  });
  return { handled: handled.sort(), listened: listened.sort(), handlers, shown, logs };
}

/** withClock(fn): runs fn with Date.now under the test's control (restored after). */
async function withClock(fn) {
  const real = Date.now;
  const clock = { t: 1_800_000_000_000 };
  Date.now = () => clock.t;
  try {
    await fn(clock);
  } finally {
    Date.now = real;
  }
}

test('window.lectureflow exposes exactly the whitelist', () => {
  const exposed = loadPreload();
  assert.deepEqual(Object.keys(exposed), ['lectureflow']);
  assert.deepEqual(Object.keys(exposed.lectureflow).sort(), WHITELIST);
  assert.deepEqual(Object.keys(exposed.lectureflow.components), ['importFile']);
});

test('the main process answers exactly these channels', () => {
  const { handled, listened } = loadIpc();
  assert.deepEqual(handled, HANDLED);
  assert.deepEqual(listened, SHELL_PAGE);
});

test('lf:notify: one notification per 3 seconds; the rest are dropped and logged once', async () => {
  assert.deepEqual({ ...NOTIFY_LIMIT }, { max: 1, windowMs: 3000 });
  await withClock(async (clock) => {
    const { handlers, shown, logs } = loadIpc();
    const notify = (title) => handlers['lf:notify'](appEvent, { title, body: 'b' });
    assert.equal(await notify('first'), true);
    assert.equal(await notify('second'), false, 'dropped');
    clock.t += 2999;
    assert.equal(await notify('third'), false, 'still within 3 s');
    assert.deepEqual(shown.map((n) => n.title), ['first']);
    assert.equal(logs.filter((m) => /notification dropped/.test(m)).length, 1, 'one log line per burst, not one per drop');
    clock.t += 1;
    assert.equal(await notify('fourth'), true, '3 s after the first');
    assert.equal(await notify('fifth'), false);
    assert.deepEqual(shown.map((n) => n.title), ['first', 'fourth']);
    assert.equal(logs.filter((m) => /notification dropped/.test(m)).length, 2, 'a new burst is logged again');
  });
});

test('lf:notify: invalid calls are refused before the limit and do not use it up', async () => {
  await withClock(async () => {
    const { handlers, shown } = loadIpc();
    await assert.rejects(handlers['lf:notify'](appEvent, { title: 'x'.repeat(201) }), /long/);
    await assert.rejects(handlers['lf:notify'](appEvent, { title: 'a', body: 'x'.repeat(1001) }), /long/);
    await assert.rejects(handlers['lf:notify']({ ...appEvent, senderFrame: { parent: null, url: 'https://example.com/' } }, { title: 'a' }), /untrusted/);
    assert.equal(await handlers['lf:notify'](appEvent, { title: 'ok' }), true);
    assert.deepEqual(shown.map((n) => n.title), ['ok']);
  });
});
