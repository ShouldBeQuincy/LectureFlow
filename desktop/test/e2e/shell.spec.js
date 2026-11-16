// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Contract tests that run against both the fake and the real service
// (SPEC-v3 V12.1: preload whitelist, navigation guard, token check).
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const net = require('node:net');
const { test, expect, waitForApp, request, filesContaining } = require('./fixtures');

const WHITELIST = ['components', 'getAppVersion', 'getPowerState', 'notify', 'openExternal', 'pathsForDroppedFiles', 'pickFiles', 'pickFolder'];

test('the window loads the app from the local service; its API calls carry the token', async ({ launch }) => {
  const { app, page } = await launch();
  const problems = [];
  page.on('console', (m) => {
    if (/Content Security Policy|Refused to|net::ERR_/i.test(m.text())) problems.push(m.text());
  });
  const url = await waitForApp(page);
  expect(url.hostname).toBe('127.0.0.1');
  await page.waitForLoadState('load');
  await page.waitForTimeout(1500); // let the app's first API calls and SSE connect
  const health = await page.evaluate(async () => {
    const r = await fetch('/api/health');
    return { status: r.status, body: await r.json() };
  });
  expect(health.status).toBe(200);
  expect(health.body).toMatchObject({ ok: true, mode: 'desktop', version: require('../../package.json').version });
  expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].isVisible())).toBe(true);
  expect(problems, 'the page loads without CSP or network errors').toEqual([]);
});

test('window.lectureflow exposes exactly the whitelist; the page has no Node', async ({ launch }) => {
  const { page } = await launch();
  await waitForApp(page);
  expect(await page.evaluate(() => Object.keys(window.lectureflow).sort())).toEqual(WHITELIST);
  expect(await page.evaluate(() => Object.keys(window.lectureflow.components))).toEqual(['importFile']);
  expect(await page.evaluate(() => Object.values(window.lectureflow).map((v) => typeof v).sort()))
    .toEqual(['function', 'function', 'function', 'function', 'function', 'function', 'function', 'object']);
  expect(await page.evaluate(() => typeof window.lectureflowShell)).toBe('undefined');
  expect(await page.evaluate(() => [typeof require, typeof process, typeof module, typeof Buffer, typeof global, typeof ipcRenderer, typeof electron]))
    .toEqual(['undefined', 'undefined', 'undefined', 'undefined', 'undefined', 'undefined', 'undefined']);
  // The bridge object cannot be swapped out by page code.
  expect(await page.evaluate(() => {
    try {
      window.lectureflow.openExternal = () => 'hijacked';
    } catch {
      // frozen
    }
    return typeof window.lectureflow.openExternal === 'function' && !String(window.lectureflow.openExternal).includes('hijacked');
  })).toBe(true);
});

test('IPC arguments are checked; valid calls work', async ({ launch }) => {
  const { app, page } = await launch();
  await waitForApp(page);
  await app.evaluate(({ dialog, shell }) => {
    globalThis.__lf = { dialogs: [], opened: [] };
    dialog.showOpenDialog = async (win, opts) => {
      globalThis.__lf.dialogs.push(opts);
      return { canceled: false, filePaths: ['C:\\lectures\\week1.mp4'] };
    };
    shell.openExternal = async (url) => {
      globalThis.__lf.opened.push(url);
    };
  });
  const r = await page.evaluate(async () => {
    const L = window.lectureflow;
    const out = {};
    const run = async (name, fn) => {
      try {
        out[name] = { ok: true, value: await fn() };
      } catch (e) {
        out[name] = { ok: false, message: String(e && e.message) };
      }
    };
    await run('version', () => L.getAppVersion());
    await run('power', () => L.getPowerState());
    await run('pick', () => L.pickFiles({ title: 'Choose a recording', multiple: true, filters: [{ name: 'Video', extensions: ['mp4'] }] }));
    await run('folder', () => L.pickFolder());
    await run('component', () => L.components.importFile());
    await run('external', () => L.openExternal('https://example.com/help'));
    await run('extJs', () => L.openExternal('javascript:alert(1)'));
    await run('extFile', () => L.openExternal('file:///C:/Windows/win.ini'));
    await run('extType', () => L.openExternal({ href: 'https://example.com' }));
    await run('notifyType', () => L.notify({ title: 42 }));
    await run('notifyLong', () => L.notify({ title: 'x'.repeat(201) }));
    await run('notifyKey', () => L.notify({ title: 'a', icon: 'C:\\x.png' }));
    await run('pickKey', () => L.pickFiles({ properties: ['openDirectory'] }));
    await run('pickExt', () => L.pickFiles({ filters: [{ name: 'x', extensions: ['../exe'] }] }));
    await run('dropType', () => L.pathsForDroppedFiles('C:\\Windows'));
    await run('dropItem', () => L.pathsForDroppedFiles([{ name: 'a.txt' }]));
    await run('dropMem', () => L.pathsForDroppedFiles([new File(['a'], 'a.txt')]));
    return out;
  });
  expect(r.version).toEqual({ ok: true, value: require('../../package.json').version });
  expect(r.power.ok).toBe(true);
  expect(typeof r.power.value.onBattery).toBe('boolean');
  expect(r.pick).toEqual({ ok: true, value: ['C:\\lectures\\week1.mp4'] });
  expect(r.folder).toEqual({ ok: true, value: 'C:\\lectures\\week1.mp4' });
  expect(r.component).toEqual({ ok: true, value: 'C:\\lectures\\week1.mp4' });
  expect(r.external).toEqual({ ok: true, value: true });
  for (const k of ['extJs', 'extFile', 'extType', 'notifyType', 'notifyLong', 'notifyKey', 'pickKey', 'pickExt', 'dropType', 'dropItem']) {
    expect(r[k].ok, k).toBe(false);
  }
  expect(r.dropMem).toEqual({ ok: true, value: [] });
  const calls = await app.evaluate(() => globalThis.__lf);
  expect(calls.opened).toEqual(['https://example.com/help']);
  expect(calls.dialogs[0]).toMatchObject({ title: 'Choose a recording', properties: ['openFile', 'multiSelections'], filters: [{ name: 'Video', extensions: ['mp4'] }] });
  expect(calls.dialogs[1].properties).toEqual(['openDirectory', 'createDirectory']);
  expect(calls.dialogs[2].properties).toEqual(['openFile']);
});

test('dropped or chosen files resolve to their paths on disk', async ({ launch }) => {
  const { page, dataDir } = await launch();
  await waitForApp(page);
  const file = path.join(dataDir, 'week 1 lecture.mp4');
  fs.writeFileSync(file, 'not really a video');
  await page.evaluate(() => {
    const input = document.createElement('input');
    input.type = 'file';
    input.multiple = true;
    input.id = 'lf-test-input';
    document.body.append(input);
  });
  await page.setInputFiles('#lf-test-input', file);
  const paths = await page.evaluate(() => {
    const { files } = document.getElementById('lf-test-input');
    return [window.lectureflow.pathsForDroppedFiles(files), window.lectureflow.pathsForDroppedFiles(Array.from(files))];
  });
  expect(paths).toEqual([[file], [file]]); // FileList (as from a drop event) and array
});

test('navigation away from the service is blocked; web links go to the system browser', async ({ launch }) => {
  test.setTimeout(120_000); // about 12 s of waiting for the link limit's window
  const { app, page } = await launch();
  const start = await waitForApp(page);
  await app.evaluate(({ shell }) => {
    globalThis.__opened = [];
    shell.openExternal = async (url) => {
      globalThis.__opened.push(url);
    };
  });
  await page.evaluate(() => { location.href = 'https://example.com/'; });
  await page.waitForTimeout(700);
  await page.evaluate(() => { window.open('https://example.org/x', '_blank', 'noopener'); });
  await page.evaluate(() => { location.href = 'file:///C:/Windows/win.ini'; }).catch(() => {});
  await page.evaluate(() => { window.open('file:///C:/Windows/win.ini'); });
  await page.evaluate(() => { window.open('javascript:alert(1)'); });
  await page.evaluate(() => { location.href = 'ms-settings:privacy'; }).catch(() => {});
  await page.evaluate(() => { window.open('mailto:lecturer@uni.example?subject=CSC1003', '_blank', 'noopener'); });
  await page.waitForTimeout(700);
  // A fourth link within 10 s is dropped (one limit for every link to the
  // system browser: 3 in 10 s, security audit FB1 L-6 / review R44) ...
  await page.evaluate(() => { location.href = 'mailto:office@uni.example'; }).catch(() => {});
  await page.waitForTimeout(1000);
  const threeFirst = ['https://example.com/', 'https://example.org/x', 'mailto:lecturer@uni.example?subject=CSC1003'];
  expect(await app.evaluate(() => globalThis.__opened)).toEqual(threeFirst);
  // ... and goes through once the first ones have left the window.
  await page.waitForTimeout(10_500);
  await page.evaluate(() => { location.href = 'mailto:office@uni.example'; }).catch(() => {});
  await page.waitForTimeout(1000);
  expect(new URL(page.url()).origin).toBe(start.origin);
  expect(await app.evaluate(() => globalThis.__opened)).toEqual([...threeFirst, 'mailto:office@uni.example']);
  expect(app.windows().length).toBe(1);
  expect(await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().length)).toBe(1);
});

test('content security policy: no inline scripts, no eval, no remote requests', async ({ launch }) => {
  const { page } = await launch();
  await waitForApp(page);
  const r = await page.evaluate(() => new Promise((resolve) => {
    window.__inline = 0;
    const s = document.createElement('script');
    s.textContent = 'window.__inline = 1';
    document.head.appendChild(s);
    let timerString = 'not run';
    try {
      setTimeout('window.__timer = 1', 0);
    } catch {
      timerString = 'threw';
    }
    fetch('https://example.com/').then(() => 'loaded', () => 'blocked').then((remote) => {
      setTimeout(() => resolve({ inline: window.__inline, timer: window.__timer === 1 ? 'ran' : timerString, remote }), 200);
    });
  }));
  expect(r.inline).toBe(0);
  expect(r.timer).not.toBe('ran');
  expect(r.remote).toBe('blocked');
});

test('the API refuses requests without the token (401) and with a foreign Host (403)', async ({ launch }) => {
  const { page } = await launch();
  const { port } = await waitForApp(page);
  const none = await request(port, '/api/health');
  expect(none.status).toBe(401);
  expect(none.json && none.json.error && none.json.error.code).toBe('auth.token_required');
  const wrong = await request(port, '/api/health', { headers: { 'X-LF-Token': 'b'.repeat(64) } });
  expect(wrong.status).toBe(401);
  const shutdown = await request(port, '/api/app/shutdown', { method: 'POST' });
  expect(shutdown.status).toBe(401);
  const host = await request(port, '/api/health', { headers: { Host: 'evil.example:80' } });
  expect(host.status).toBe(403);
  // The service is still there for the window.
  expect(await page.evaluate(async () => (await fetch('/api/health')).status)).toBe(200);
});

test('the token never reaches the disk; preferences and the port survive a restart', async ({ launch }) => {
  test.setTimeout(150_000);
  const first = await launch();
  const tokens = new Set();
  first.page.on('request', async (req) => {
    if (!req.url().includes('/api/')) return;
    const h = await req.allHeaders().catch(() => ({}));
    if (h['x-lf-token']) tokens.add(h['x-lf-token']);
  });
  const url1 = await waitForApp(first.page);
  await first.page.evaluate(async () => {
    localStorage.setItem('lectureflow.e2e', 'kept');
    await fetch('/api/health');
    await fetch('/api/app/settings');
  });
  await expect.poll(() => tokens.size).toBeGreaterThan(0);
  expect(tokens.size).toBe(1);
  const [token] = tokens;
  expect(token).toMatch(/^[0-9a-f]{64}$/);
  await first.app.close();

  // Everything the app and the service wrote: the Chromium profile (local
  // storage, preferences, logs), port.json, desktop.log, the service's
  // database and logs.
  const scan = filesContaining(first.dataDir, token);
  expect(scan.scanned).toBeGreaterThan(5);
  expect(scan.hits).toEqual([]);
  const portFile = path.join(first.dataDir, 'electron', 'port.json');
  expect(JSON.parse(fs.readFileSync(portFile, 'utf8'))).toEqual({ port: Number(url1.port) });

  // Same port, same origin: the page's preferences are still there.
  const second = await launch({ dataDir: first.dataDir });
  const url2 = await waitForApp(second.page);
  expect(url2.port).toBe(url1.port);
  expect(await second.page.evaluate(() => localStorage.getItem('lectureflow.e2e'))).toBe('kept');
  await second.app.close();

  // The remembered port is taken: another one is used and remembered.
  const blocker = net.createServer();
  await new Promise((r) => blocker.listen(Number(url1.port), '127.0.0.1', r));
  try {
    const third = await launch({ dataDir: first.dataDir });
    const url3 = await waitForApp(third.page);
    expect(url3.port).not.toBe(url1.port);
    expect(await third.page.evaluate(async () => (await fetch('/api/health')).status)).toBe(200);
    await third.app.close();
    expect(JSON.parse(fs.readFileSync(portFile, 'utf8'))).toEqual({ port: Number(url3.port) });
  } finally {
    await new Promise((r) => blocker.close(r));
  }
});
