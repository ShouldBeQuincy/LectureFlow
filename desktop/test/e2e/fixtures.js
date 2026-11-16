// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Shared fixtures: launch the unpackaged shell with its own temporary data
// directory (LECTUREFLOW_DATA_DIR) against the fake or the real service.
'use strict';

const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const base = require('@playwright/test');

const { expect } = base;

const DESKTOP = path.resolve(__dirname, '..', '..');
const REPO = path.resolve(DESKTOP, '..');
const FAKE = path.join(DESKTOP, 'test', 'fixtures', 'fake-server.js');
const GO_BIN = path.join(DESKTOP, '.e2e-tmp', 'bin', process.platform === 'win32' ? 'lectureflow.exe' : 'lectureflow');
const APP_URL = /^http:\/\/127\.0\.0\.1:\d+\/(#.*)?$/;

// cleanEnv: the caller's environment without the app's own variables and
// without any variable whose name ends in _API_KEY (DEEPSEEK_API_KEY and
// the other model service keys): the app under test would otherwise offer
// the caller's real key (GET /api/onboarding/env-keys). A test that wants
// one passes it in launch({ env }). The values are never read.
function cleanEnv() {
  const env = { ...process.env };
  for (const k of Object.keys(env)) {
    if (/^(LECTUREFLOW_|FAKE_|ELECTRON_RUN_AS_NODE$)|_API_KEY$/i.test(k)) delete env[k];
  }
  return env;
}

const test = base.test.extend({
  serverKind: ['fake', { option: true }],

  // launch({ env, dataDir, lang, appData, args }) → { app, page, dataDir, env }
  // With appData set, LECTUREFLOW_DATA_DIR is not used: the data dir is
  // resolved like in a real installation, below {appData}/LectureFlow.
  // args are extra command-line arguments of the shell (e.g. --background).
  // The service never serves the per-user browser extension endpoint here
  // (LECTUREFLOW_NM_ENDPOINT=off), so a test run never stands in for a real
  // LectureFlow.
  launch: async ({ serverKind }, use, testInfo) => {
    const apps = [];
    const dirs = [];
    // electron.launch has been seen to hang (flake-hunt #12): Playwright was
    // attached to the main process's inspector, but Electron's "DevTools
    // listening" line did not come within 60 s, twice in a row while the
    // whole machine stalled; the launches before and after took seconds. A
    // hung Electron does not recover by waiting longer, so it is ended
    // (Playwright does that on the timeout) and started once more, with the
    // test's time extended by the same 60 s. Any other launch error fails
    // the test at once, as before.
    const electronLaunch = async (opts) => {
      try {
        return await base._electron.launch({ ...opts, timeout: 60_000 });
      } catch (err) {
        if (!err || err.name !== 'TimeoutError') throw err;
        testInfo.setTimeout(testInfo.timeout + 60_000);
        testInfo.annotations.push({ type: 'electron.launch', description: 'timed out after 60 s; launched again' });
        return base._electron.launch({ ...opts, timeout: 60_000 });
      }
    };
    const launch = async ({ env = {}, dataDir, lang = 'en-US', appData, args = [] } = {}) => {
      const dir = appData ? path.join(appData, 'LectureFlow') : dataDir || fs.mkdtempSync(path.join(os.tmpdir(), 'lf-e2e-'));
      if (!dataDir && !appData) dirs.push(dir);
      const fullEnv = {
        ...cleanEnv(),
        ...(appData ? { LECTUREFLOW_TEST_APPDATA: appData } : { LECTUREFLOW_DATA_DIR: dir }),
        LECTUREFLOW_SERVER: serverKind === 'go' ? GO_BIN : FAKE,
        LECTUREFLOW_RESOURCES: REPO,
        LECTUREFLOW_NM_ENDPOINT: 'off',
        ...env,
      };
      const app = await electronLaunch({ args: [DESKTOP, `--lang=${lang}`, ...args], env: fullEnv, cwd: DESKTOP });
      apps.push(app);
      const page = await app.firstWindow();
      return { app, page, dataDir: dir, env: fullEnv };
    };
    await use(launch);
    for (const a of apps) await a.close().catch(() => {});
    for (const d of dirs) fs.rmSync(d, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
  },
});

/** waitForApp: the window shows the service's page. */
async function waitForApp(page, timeout = 45_000) {
  await expect.poll(() => page.url(), { timeout }).toMatch(APP_URL);
  await page.waitForLoadState('domcontentloaded');
  return new URL(page.url());
}

/** request: plain Node HTTP request (no Electron, so no injected token). */
function request(port, pathname, { method = 'GET', headers = {}, body } = {}) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port, path: pathname, method, headers }, (res) => {
      let data = '';
      res.setEncoding('utf8');
      res.on('data', (c) => { data += c; });
      res.on('end', () => {
        let json = null;
        try {
          json = JSON.parse(data);
        } catch {
          json = null;
        }
        resolve({ status: res.statusCode, headers: res.headers, json, text: data });
      });
    });
    req.on('error', reject);
    if (body !== undefined) req.end(JSON.stringify(body));
    else req.end();
  });
}

function fakeState(dataDir) {
  return JSON.parse(fs.readFileSync(path.join(dataDir, 'fake-state.json'), 'utf8'));
}

async function mainWindowVisible(app) {
  return app.evaluate(({ BrowserWindow }) => {
    const w = BrowserWindow.getAllWindows()[0];
    return !!w && !w.isDestroyed() && w.isVisible();
  });
}

/** filesContaining(dir, secret) → files under dir whose bytes contain secret (ASCII or UTF-16LE). */
function filesContaining(dir, secret) {
  const needles = [Buffer.from(secret, 'latin1'), Buffer.from(secret, 'utf16le')];
  const hits = [];
  let scanned = 0;
  const walk = (d) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.isFile()) {
        scanned++;
        let b;
        try {
          b = fs.readFileSync(p);
        } catch {
          continue; // locked by a process that is still exiting
        }
        if (needles.some((n) => b.includes(n))) hits.push(path.relative(dir, p));
      }
    }
  };
  walk(dir);
  return { hits, scanned };
}

module.exports = { test, expect: base.expect, waitForApp, request, fakeState, mainWindowVisible, filesContaining, DESKTOP, REPO, FAKE, GO_BIN, APP_URL };
