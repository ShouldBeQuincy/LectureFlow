// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Path grants (security audit FB1 L-7, T-SEC-08): the paths the system
// dialog or a drop gives the page are registered with the service as
// one-time grants before the page can send them back for a copy by path.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const Module = require('node:module');
const path = require('node:path');
const vm = require('node:vm');

const SRC = path.join(__dirname, '..', '..', 'src');
const TOKEN = '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';

test('the grant header is the HMAC the service expects (internal/api pathgrant_test.go has the same vector)', () => {
  const { pathGrantSecret, PATH_GRANT_HEADER } = require('../../src/main/lib/pathgrant');
  assert.equal(PATH_GRANT_HEADER, 'X-LF-Path-Grant');
  assert.equal(pathGrantSecret(TOKEN), 'a0b9b3e44527fa6409a10ee492828354479d7daf3722999ef862b2410c99d264');
  assert.notEqual(pathGrantSecret(TOKEN), TOKEN);
  assert.throws(() => pathGrantSecret(''), /token/);
  assert.throws(() => pathGrantSecret(undefined), /token/);
});

test('apiclient.grantPaths: POST /api/app/path-grants with the token and the grant header', async () => {
  const { createApiClient } = require('../../src/main/apiclient');
  const { pathGrantSecret } = require('../../src/main/lib/pathgrant');
  const seen = [];
  const api = createApiClient({
    getServer: () => ({ origin: 'http://127.0.0.1:4000', token: TOKEN }),
    fetchImpl: async (url, init) => {
      seen.push({ url, init });
      return { ok: true, status: 204, text: async () => '' };
    },
  });
  assert.equal(await api.grantPaths(['C:\\Users\\alice\\week1.mp4']), null);
  assert.equal(seen.length, 1);
  assert.equal(seen[0].url, 'http://127.0.0.1:4000/api/app/path-grants');
  assert.equal(seen[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(seen[0].init.body), { paths: ['C:\\Users\\alice\\week1.mp4'], kind: 'file' });
  assert.equal(seen[0].init.headers['X-LF-Token'], TOKEN);
  assert.equal(seen[0].init.headers['X-LF-Path-Grant'], pathGrantSecret(TOKEN));
  // the other calls carry no grant header
  await api.activity();
  assert.equal(seen[1].init.headers['X-LF-Path-Grant'], undefined);
  // the folder dialog's pick (SEC24 R1)
  await api.grantPaths(['\\\\fileserver.example\\share\\LectureFlow'], { kind: 'folder' });
  assert.deepEqual(JSON.parse(seen[2].init.body), { paths: ['\\\\fileserver.example\\share\\LectureFlow'], kind: 'folder' });
  assert.equal(seen[2].init.headers['X-LF-Path-Grant'], pathGrantSecret(TOKEN));
});

// SEC24 L5 (defence in depth): the window's session drops a grant header
// the page set itself, as it drops a page-made token. The main process's
// own grants do not pass through that session: apiclient.js calls the
// service with Node's fetch (the default fetchImpl), not Electron's net.
test('websecurity: a page-made X-LF-Path-Grant is dropped (any case); the main process\'s grants use Node\'s fetch', () => {
  const { hardenSession } = require('../../src/main/websecurity');
  const PORT = 4000;
  let beforeSend = null;
  const ses = {
    webRequest: {
      onBeforeRequest() {},
      onBeforeSendHeaders(fn) { beforeSend = fn; },
      onHeadersReceived() {},
    },
    setPermissionRequestHandler() {},
    setPermissionCheckHandler() {},
    setDevicePermissionHandler() {},
    on() {},
    setSpellCheckerEnabled() {},
    setSpellCheckerLanguages() {},
  };
  const log = { warn() {}, info() {} };
  hardenSession(ses, { getPort: () => PORT, getToken: () => TOKEN, shellRoot: SRC, devTools: false, log });
  assert.equal(typeof beforeSend, 'function');
  const send = (url, requestHeaders) => {
    let out = null;
    beforeSend({ url, requestHeaders }, (r) => { out = r; });
    return out.requestHeaders;
  };
  for (const name of ['X-LF-Path-Grant', 'x-lf-path-grant', 'X-LF-PATH-GRANT']) {
    const h = send(`http://127.0.0.1:${PORT}/api/app/path-grants`, { [name]: 'forged', 'X-LF-Token': 'forged', Accept: 'application/json' });
    assert.deepEqual(h, { Accept: 'application/json', 'X-LF-Token': TOKEN }, name);
    // a request elsewhere gets neither
    assert.deepEqual(send('https://example.com/x', { [name]: 'forged', Accept: '*/*' }), { Accept: '*/*' }, name);
  }
  // headers with similar names are forwarded
  assert.deepEqual(send(`http://127.0.0.1:${PORT}/api/x`, { 'X-LF-Path-Grants': 'a', 'X-LF-Path': 'b' }), { 'X-LF-Path-Grants': 'a', 'X-LF-Path': 'b', 'X-LF-Token': TOKEN });

  // the main process's grants: Node's global fetch, nothing from Electron
  const src = fs.readFileSync(path.join(SRC, 'main', 'apiclient.js'), 'utf8');
  assert.match(src, /fetchImpl = fetch\b/);
  assert.doesNotMatch(src, /require\(['"]electron['"]\)|\bnet\.fetch\b/);
  const shell = fs.readFileSync(path.join(SRC, 'main', 'shell.js'), 'utf8');
  const made = shell.match(/createApiClient\(\{[^}]*\}\)/g) || [];
  assert.ok(made.length > 0, 'shell.js makes the client');
  for (const m of made) assert.doesNotMatch(m, /fetchImpl/, 'shell.js uses the default fetch');
});

test('createPathGranter: chunks, never throws, says when nothing was registered', async () => {
  const { createPathGranter, PATH_GRANT_CHUNK } = require('../../src/main/lib/pathgrant');
  const calls = [];
  const warnings = [];
  let fail = false;
  const grant = createPathGranter({
    api: { grantPaths: async (p, opts) => { calls.push(p); kinds.push(opts.kind); if (fail) throw new Error('service not running'); return null; } },
    log: { warn: (m) => warnings.push(m) },
  });
  const kinds = [];
  const many = Array.from({ length: PATH_GRANT_CHUNK * 2 + 1 }, (_, i) => `/home/alice/${i}.mp4`);
  assert.equal(await grant(many), true);
  assert.deepEqual(calls.map((c) => c.length), [PATH_GRANT_CHUNK, PATH_GRANT_CHUNK, 1]);
  assert.deepEqual(calls.flat(), many);
  assert.deepEqual(kinds, ['file', 'file', 'file']);
  calls.length = 0;
  kinds.length = 0;
  assert.equal(await grant([]), true);
  assert.equal(await grant(['', 3, null, '/home/alice/a.mp4']), true);
  assert.deepEqual(calls, [['/home/alice/a.mp4']]);
  assert.equal(await grant(['/mnt/share/LectureFlow'], 'folder'), true);
  assert.deepEqual(kinds, ['file', 'folder']);
  await assert.rejects(grant(['/x'], 'anything'), /kind/);
  fail = true;
  assert.equal(await grant(['/home/alice/a.mp4']), false);
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /not registered/);
  assert.throws(() => createPathGranter({ api: {} }), /grantPaths/);
});

test('validateDroppedPaths: a non-empty list of path strings', () => {
  const { validateDroppedPaths, LIMITS } = require('../../src/main/lib/validate');
  assert.deepEqual(validateDroppedPaths([['C:\\a.mp4', '\\\\fileserver.example\\share\\b.mp4']]), ['C:\\a.mp4', '\\\\fileserver.example\\share\\b.mp4']);
  for (const bad of [[], [[]], ['C:\\a.mp4'], [[3]], [['']], [['a\0b']], [['x'.repeat(LIMITS.path + 1)]], [new Array(LIMITS.droppedPaths + 1).fill('C:\\a.mp4')], [['C:\\a.mp4'], 'extra'], [{ 0: 'C:\\a.mp4' }]]) {
    assert.throws(() => validateDroppedPaths(bad), /grantDropped|arguments/, JSON.stringify(bad).slice(0, 60));
  }
});

/**
 * loadIpc(dialogResult) → { handlers, granted, events, dialogs }: ipc.js
 * with a stand-in electron module; handlers[channel](event, ...args) runs
 * like ipcMain.handle.
 */
function loadIpc(dialogResult) {
  const handlers = {};
  const granted = [];
  const kinds = [];
  const events = [];
  const dialogs = [];
  const electron = {
    app: { getVersion: () => '0.0.0-test' },
    BrowserWindow: { fromWebContents: (wc) => (wc ? { id: 1 } : null) },
    dialog: {
      showOpenDialog: async (win, opts) => {
        dialogs.push(opts);
        events.push('dialog');
        return dialogResult;
      },
    },
    ipcMain: {
      handle: (ch, fn) => { handlers[ch] = fn; },
      on: () => {},
    },
    Notification: { isSupported: () => false },
    powerMonitor: { isOnBatteryPower: () => false },
    shell: { openExternal: async () => {} },
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
  registerIpc({
    getPort: () => 4000,
    shellRoot: SRC,
    t: (k) => k,
    showMainWindow: () => {},
    onRetry: () => {},
    onOpenLogs: () => {},
    onRestoreDefault: () => {},
    openExternalLimit: () => true,
    grantPaths: async (paths, kind = 'file') => {
      await new Promise((r) => setImmediate(r));
      granted.push([...paths]);
      kinds.push(kind);
      events.push('granted');
      return true;
    },
    log: { warn: () => {} },
  });
  return { handlers, granted, kinds, events, dialogs };
}

const appEvent = { senderFrame: { parent: null, url: 'http://127.0.0.1:4000/' }, sender: {} };
const otherEvent = { senderFrame: { parent: null, url: 'https://example.com/' }, sender: {} };

test('ipc.js: the dialog\'s files are granted before the page gets their paths', async () => {
  const picked = ['C:\\Users\\alice\\Videos\\week1.mp4', '\\\\fileserver.example\\share\\week2.mp4'];
  const { handlers, granted, kinds, events } = loadIpc({ canceled: false, filePaths: picked });
  const result = await handlers['lf:pick-files'](appEvent, { multiple: true });
  assert.deepEqual(result, picked);
  assert.deepEqual(granted, [picked]);
  assert.deepEqual(events, ['dialog', 'granted'], 'granted (awaited) before the handler answers');

  const comp = await handlers['lf:components-import-file'](appEvent);
  assert.equal(comp, picked[0]);
  assert.deepEqual(granted[1], [picked[0]], 'only the one file the import uses');
  assert.deepEqual(kinds, ['file', 'file']);

  // the folder dialog's pick: a folder grant, for the data directory
  // endpoints (SEC24 R1), before the page has it
  events.length = 0;
  assert.equal(await handlers['lf:pick-folder'](appEvent), picked[0]);
  assert.deepEqual(granted[2], [picked[0]]);
  assert.deepEqual(kinds, ['file', 'file', 'folder']);
  assert.deepEqual(events, ['dialog', 'granted']);

  // dropped files are files
  await handlers['lf:grant-dropped'](appEvent, ['E:\\x.mp4']);
  assert.equal(kinds[3], 'file');
});

test('ipc.js: a cancelled dialog grants nothing; a foreign sender gets no dialog and no grant', async () => {
  const { handlers, granted, dialogs } = loadIpc({ canceled: true, filePaths: [] });
  assert.deepEqual(await handlers['lf:pick-files'](appEvent, {}), []);
  assert.equal(await handlers['lf:components-import-file'](appEvent), null);
  assert.equal(granted.length, 0);
  const n = dialogs.length;
  await assert.rejects(handlers['lf:pick-files'](otherEvent, {}), /untrusted sender/);
  await assert.rejects(handlers['lf:grant-dropped'](otherEvent, ['C:\\Windows\\win.ini']), /untrusted sender/);
  assert.equal(dialogs.length, n);
  assert.equal(granted.length, 0);
});

test('ipc.js: lf:grant-dropped grants the checked list of a drop', async () => {
  const { handlers, granted } = loadIpc({ canceled: true, filePaths: [] });
  const dropped = ['C:\\Users\\alice\\Desktop\\week3.mp4'];
  assert.equal(await handlers['lf:grant-dropped'](appEvent, dropped), true);
  assert.deepEqual(granted, [dropped]);
  for (const bad of [[], ['C:\\a.mp4', 'extra'], [[3]], ['C:\\a.mp4']]) {
    await assert.rejects(handlers['lf:grant-dropped'](appEvent, ...bad), /grantDropped|arguments/);
  }
  assert.equal(granted.length, 1);
});

test('ipc.js: registerIpc needs the granter', () => {
  const src = fs.readFileSync(path.join(SRC, 'main', 'ipc.js'), 'utf8');
  assert.match(src, /if \(typeof grantPaths !== 'function'\) throw/);
  const shell = fs.readFileSync(path.join(SRC, 'main', 'shell.js'), 'utf8');
  const reg = shell.slice(shell.indexOf('registerIpc({'), shell.indexOf('});', shell.indexOf('registerIpc({')));
  assert.match(reg, /grantPaths: createPathGranter\(\{ api: this\.api, log: this\.log \}\),/);
});

/** preload.js in a sandbox like the app page's, with webUtils giving paths. */
function loadPreload(pathFor) {
  const calls = [];
  const exposed = {};
  class File {
    constructor(p) { this.p = p; }
  }
  const electron = {
    contextBridge: { exposeInMainWorld: (name, api) => { exposed[name] = api; } },
    ipcRenderer: {
      invoke: (...args) => {
        calls.push(args);
        return Promise.resolve(true);
      },
      send: () => {},
    },
    webUtils: { getPathForFile: pathFor },
  };
  const sandbox = {
    require: (m) => {
      if (m !== 'electron') throw new Error(`unexpected require ${m}`);
      return electron;
    },
    window: { location: { protocol: 'http:', hostname: '127.0.0.1' } },
    navigator: {},
    File,
    Error,
    TypeError,
    Promise,
    Object,
    Array,
    Number,
    String,
  };
  vm.runInNewContext(fs.readFileSync(path.join(SRC, 'preload.js'), 'utf8'), sandbox, { filename: 'preload.js', timeout: 5000 });
  return { api: exposed.lectureflow, calls, File };
}

test('preload: the paths of a drop go to the main process for a grant; the page still gets them at once', async () => {
  const { api, calls, File } = loadPreload((f) => f.p);
  const settle = () => new Promise((r) => setImmediate(r));
  // (arrays made in the sandbox: compared as JSON, they have another Array.prototype)
  const plain = (v) => JSON.parse(JSON.stringify(v));
  const files = [new File('C:\\Users\\alice\\week1.mp4'), new File(''), new File('D:\\week2.mp4')];
  assert.deepEqual(plain(api.pathsForDroppedFiles(files)), ['C:\\Users\\alice\\week1.mp4', 'D:\\week2.mp4']);
  await settle();
  assert.deepEqual(plain(calls), [['lf:grant-dropped', ['C:\\Users\\alice\\week1.mp4', 'D:\\week2.mp4']]]);
  // the page asks one file at a time (web/api.js attachDroppedPaths): one batch per drop
  calls.length = 0;
  assert.deepEqual(plain(api.pathsForDroppedFiles([new File('E:\\a.mp4')])), ['E:\\a.mp4']);
  assert.deepEqual(plain(api.pathsForDroppedFiles([new File('E:\\b.mp4')])), ['E:\\b.mp4']);
  await settle();
  assert.deepEqual(plain(calls), [['lf:grant-dropped', ['E:\\a.mp4', 'E:\\b.mp4']]]);
  // more than one IPC message takes: several messages
  calls.length = 0;
  for (let i = 0; i < 1001; i++) api.pathsForDroppedFiles([new File(`E:\\${i}.mp4`)]);
  await settle();
  assert.deepEqual(calls.map((c) => c[1].length), [1000, 1]);
  // files made in the page have no path: nothing to grant, nothing sent
  calls.length = 0;
  assert.deepEqual(plain(api.pathsForDroppedFiles([new File('')])), []);
  await settle();
  assert.equal(calls.length, 0);
  // a path the page wrote itself is no File: refused, nothing granted (SEC24 L4)
  for (const bad of ['C:\\Windows\\win.ini', ['C:\\Windows\\win.ini'], [{ name: 'win.ini', path: 'C:\\Windows\\win.ini' }],
    [new File('E:\\ok.mp4'), '\\\\fileserver.example\\share\\x.mp4'], null, 42]) {
    assert.throws(() => api.pathsForDroppedFiles(bad), /pathsForDroppedFiles/, JSON.stringify(bad));
  }
  await settle();
  assert.equal(calls.length, 0, 'nothing sent for a refused call');
  // the bridge offers no way to grant a path the page chose
  assert.deepEqual(Object.keys(api).filter((k) => /grant/i.test(k)), []);
});
