// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The Firefox build in a real Firefox (SPEC-ext M4): Mozilla's web-ext
// starts the tests' own copy of Firefox with a new, temporary profile
// (never the user's own; it is deleted at the end and the test checks
// that, and that no Firefox process of this run is left) and installs
// dist/firefox-e2e as a temporary add-on; the test drives that Firefox over
// WebDriver BiDi (test/firefox/bidi.mjs). Pages come from the local test
// server of the end-to-end tests: Firefox resolves the test host names to
// 127.0.0.1 (network.dns.localDomains) and sends everything else to a proxy
// that does not exist, so nothing reaches the internet. LectureFlow is the
// same fake as in the Chromium tests (no native host, no registry).
//
// The tests' own copy, never the installed Firefox (test/firefox/provision.mjs):
// Firefox ESR at a pinned version, downloaded once from archive.mozilla.org
// into <checkout>/tools/firefox/ (ignored by git; LF_FIREFOX_CACHE puts it
// elsewhere), checked against a pinned SHA-512, with updates switched off by
// its own distribution/policies.json (DisableAppUpdate) and its updater
// removed. A run of the installed Firefox shares that installation with the
// user's browser (its update directory and any pending update): on
// 2026-10-27 the user's Firefox could not open new tabs after a run until it
// was restarted. The profile also gets the update prefs that still have an
// effect (UPDATE_PREFS). With no copy and no way to download one, the test
// fails with instructions; it never falls back to the installed Firefox.
//
//   npm run test:firefox        (not part of npm test; the first run downloads the copy, about 90 MB)
//   npm run firefox:prepare     downloads the copy ahead of time (or unpacks an offline download)
//   LF_FIREFOX=<path to firefox> to use another Firefox: a disposable one (a CI runner's, say);
//                               only the profile prefs keep it from updating
//   LF_EXT_SCREENSHOTS=1        also writes docs/screenshots/firefox-*.png

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import net from 'node:net';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { startServer, PANOPTO, LIST } from '../e2e/server.mjs';
import { installFakeLectureFlow, COURSES, COOKIE_TRACK } from '../e2e/harness.mjs';
import { Bidi } from './bidi.mjs';
import { firefoxForTests, UPDATE_PREFS } from './provision.mjs';
import { runProcesses, isInside } from './run-processes.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const extDir = path.join(root, 'dist', 'firefox-e2e');
const SHOTS = process.env.LF_EXT_SCREENSHOTS === '1';
const GECKO_ID = 'lectureflow@qu1ncy.dev';
// The extension's internal host name in this profile (moz-extension://<uuid>/).
const UUID = '6c1f0e2a-4b7d-4c55-9a3e-0d2b8f1e7a90';
const EXT = `moz-extension://${UUID}`;
// attacker.lf-e2e-x.test: a site the e2e build has no access to (a page embedding the player).
const HOSTS = ['course.lf-e2e.test', 'other.lf-e2e.test', 'canvas.lf-e2e.test', 'attacker.lf-e2e-x.test', PANOPTO.host];
// The shortcut a Firefox installation may add to the user's Start menu (see
// the prefs below); the run must leave it as it was.
const START_MENU_SHORTCUT = process.platform === 'win32' && process.env.APPDATA ? path.join(process.env.APPDATA, 'Microsoft', 'Windows', 'Start Menu', 'Programs', 'Firefox Private Browsing.lnk') : null;
const fileState = (p) => {
  try {
    const s = fs.statSync(p);
    return `${s.size}:${s.mtimeMs}`;
  } catch {
    return 'none';
  }
};
const startMenuBefore = START_MENU_SHORTCUT && fileState(START_MENU_SHORTCUT);

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once('error', reject);
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

/**
 * The process table: [{ ProcessId, ParentProcessId, Name, CommandLine,
 * Created }]. Created, the creation time, is only compared between rows
 * (run-processes.mjs): on Windows in microseconds since the epoch (the
 * precision Win32_Process gives; well below 2^53, so exact in a JS number);
 * elsewhere in whole seconds, from ps's elapsed time (to a second either way,
 * enough: the snapshot, not the time, is what keeps other processes out).
 */
function processTable() {
  if (process.platform === 'win32') {
    const out = execFileSync('powershell', ['-NoProfile', '-Command', "Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId,Name,CommandLine,@{n='Created';e={if ($_.CreationDate) { [long][math]::Floor(([DateTimeOffset]::new($_.CreationDate).UtcTicks - 621355968000000000) / 10) }}} | ConvertTo-Json -Compress"], { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 }).trim();
    return out ? [].concat(JSON.parse(out)) : [];
  }
  const out = execFileSync('ps', ['-axo', 'pid=,ppid=,etime=,args='], { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 });
  const now = Math.floor(Date.now() / 1000);
  const seconds = (etime) => {
    const m = /^(?:(?:(\d+)-)?(\d+):)?(\d+):(\d+)$/.exec(etime);
    return m ? ((Number(m[1] || 0) * 24 + Number(m[2] || 0)) * 60 + Number(m[3])) * 60 + Number(m[4]) : null;
  };
  return out.split('\n').map((l) => {
    const m = /^\s*(\d+)\s+(\d+)\s+(\S+)\s+(.*)$/.exec(l);
    if (!m) return null;
    const age = seconds(m[3]);
    return { ProcessId: Number(m[1]), ParentProcessId: Number(m[2]), Name: m[4].split(/\s/)[0], CommandLine: m[4], Created: age === null ? undefined : now - age };
  }).filter(Boolean);
}

/**
 * Processes of this run, whatever their name (Firefox also starts
 * crashhelper.exe, which can outlive it), with whether the run may stop them:
 * the rules are in run-processes.mjs (and its unit test).
 */
function ourFirefoxProcesses(mainPid, dirs) {
  return runProcesses({ rows: processTable(), mainPid, dirs, selfPid: process.pid, selfStart: before0.selfStart, preexisting: before0.preexisting });
}

/** Removes the entries of `dir` that `pick(name)` selects and are over `minAgeMs` old; best effort: [removed, stuck]. */
function sweep(dir, pick, minAgeMs) {
  let removed = 0;
  let stuck = 0;
  for (const name of fs.readdirSync(dir)) {
    const p = path.join(dir, name);
    try {
      if (!pick(name) || Date.now() - fs.statSync(p).mtimeMs < minAgeMs) continue;
      fs.rmSync(p, { recursive: true, force: true });
      removed++;
    } catch {
      stuck++;
    }
  }
  return [removed, stuck];
}

/** Removes what earlier runs could not (lf-ext-firefox-* over an hour old in `dir`); best effort. */
function sweepEarlierRuns(dir) {
  const [removed, stuck] = sweep(dir, (name) => name.startsWith('lf-ext-firefox-'), 3600 * 1000);
  if (removed || stuck) console.log(`# firefox: temporary directories of earlier runs: ${removed} removed, ${stuck} still locked`);
}

let server;
let runner;
let bidi;
let control; // a tab kept on the extension's settings page: the way into the background
let mainPid = 0;
let profileDir = '';
let tmpRoot = '';
let fxTemp = ''; // Firefox's own temp directory (see before)
let ownDirs = []; // the copy's directory: every Firefox process started from it is this run's
// Taken just before Firefox is started: the ids of every process then, and this
// process's creation time from the same table (null when either is unknown,
// and then no process is stopped on the strength of its parent id).
const before0 = { preexisting: null, selfStart: null };
const saved = {};
const ours = () => (mainPid || tmpRoot ? ourFirefoxProcesses(mainPid, [tmpRoot, fxTemp, ...ownDirs]) : []);

before(async () => {
  assert.ok(fs.existsSync(path.join(extDir, 'manifest.json')), 'run "node scripts/build.mjs --target firefox --e2e" first');
  const firefox = await firefoxForTests({ top: path.resolve(root, '..'), log: (m) => console.log(`# firefox: ${m}`) });
  const binary = firefox.binary;
  ownDirs = firefox.ownDirs;
  console.log(firefox.source === 'LF_FIREFOX' ? `# firefox: LF_FIREFOX=${binary}` : `# firefox: Firefox ${firefox.version}, the tests' own copy (${{ download: 'downloaded now', unpacked: 'unpacked now', cache: 'cached' }[firefox.source]}): ${binary}`);
  server = await startServer();
  // This run's directory holds web-ext's temporary profile (and, except on
  // Windows with the copy, Firefox's own temp files); it is removed at the
  // end and the test checks that. With the copy, it is in <cache>/tmp:
  // should a run's directory resist removal (on the development machine the
  // security software often keeps a Firefox profile's files from other
  // programs after Firefox has exited; see NOTES), it stays in that ignored
  // place and a later run removes it.
  const tmpParent = firefox.cacheRoot ? path.join(firefox.cacheRoot, 'tmp') : os.tmpdir();
  fs.mkdirSync(tmpParent, { recursive: true });
  if (firefox.cacheRoot) sweepEarlierRuns(tmpParent);
  tmpRoot = fs.mkdtempSync(path.join(tmpParent, 'lf-ext-firefox-'));
  // Firefox's own temp directory on Windows, except for LF_FIREFOX: one fixed
  // directory of the copy (<cache>/firefox-temp), reused by every run; its
  // entries are removed after each run as far as possible (not checked), the
  // directory itself never. On the development machine nothing could delete
  // the directory a Firefox used as its temp directory for minutes after
  // that Firefox had exited (an empty directory; RemoveDirectory: access
  // denied; no process left; the cause is not fully established, likely the
  // security software), so with it inside the run's directory the run's
  // directory could not be removed (NOTES, ext r4). Firefox reads TMP before
  // TEMP (GetTempPathW); Node reads TEMP first (os.tmpdir(), where web-ext
  // puts the profile), so the profile stays in the run's directory.
  fxTemp = process.platform === 'win32' && firefox.cacheRoot ? path.join(firefox.cacheRoot, 'firefox-temp') : tmpRoot;
  fs.mkdirSync(fxTemp, { recursive: true });
  // The processes before Firefox starts (see run-processes.mjs).
  try {
    const rows = processTable();
    const me = rows.find((r) => r.ProcessId === process.pid);
    before0.preexisting = new Set(rows.map((r) => r.ProcessId));
    before0.selfStart = me && typeof me.Created === 'number' ? me.Created : null;
  } catch {
    before0.preexisting = null;
  }
  for (const k of ['TEMP', 'TMP', 'TMPDIR', 'MOZ_HEADLESS', 'MOZ_CRASHREPORTER_DISABLE']) saved[k] = process.env[k];
  process.env.TEMP = process.env.TMPDIR = tmpRoot;
  process.env.TMP = fxTemp;
  process.env.MOZ_HEADLESS = '1'; // no window: the user is asleep
  process.env.MOZ_CRASHREPORTER_DISABLE = '1';
  const port = await freePort();
  const webExt = (await import('web-ext')).default;
  const pref = {
    // the test host names → 127.0.0.1; every other host → a proxy that is not there
    'network.dns.localDomains': HOSTS.join(','),
    'network.proxy.type': 1,
    'network.proxy.http': '127.0.0.1',
    'network.proxy.http_port': 9,
    'network.proxy.ssl': '127.0.0.1',
    'network.proxy.ssl_port': 9,
    'network.proxy.share_proxy_settings': true,
    'network.proxy.no_proxies_on': 'localhost, 127.0.0.1, .lf-e2e.test, .lf-e2e-x.test, .cloud.panopto.eu',
    'network.proxy.allow_hijacking_localhost': false,
    // and no way around that proxy: no direct connection when it fails (on
    // 2026-11-13 a Firefox of a failed start, left running for three minutes,
    // fetched the Widevine and OpenH264 plugins, about 23 MB, past the proxy
    // into its profile), none for requests that may skip proxies
    'network.proxy.failover_direct': false,
    'network.proxy.allow_bypass': false,
    // nor Firefox's plugin downloads themselves (GMPInstallManager.checkForAddons
    // checks the first); the tests use no DRM, and Clear Key comes with Firefox
    'media.gmp-manager.updateEnabled': false,
    'media.gmp-widevinecdm.enabled': false,
    'media.gmp-gmpopenh264.enabled': false,
    'network.trr.mode': 5,
    'network.captive-portal-service.enabled': false,
    'network.connectivity-service.enabled': false,
    'browser.safebrowsing.malware.enabled': false,
    'browser.safebrowsing.phishing.enabled': false,
    'browser.shell.checkDefaultBrowser': false,
    'datareporting.healthreport.uploadEnabled': false,
    'toolkit.telemetry.enabled': false,
    // no update of this Firefox (on top of the copy's policy; see provision.mjs)
    ...UPDATE_PREFS,
    // No "Firefox Private Browsing" shortcut in the user's Start menu: on
    // Windows, Firefox creates one at startup when its installation has none
    // (StartupOSIntegration.ensurePrivateBrowsingShortcutExists), which the
    // copy never has; this pref tells it that was done already.
    'browser.privacySegmentation.createdShortcut': true,
    // a fixed moz-extension:// host for the extension (the value is written into user.js as is)
    'extensions.webextensions.uuids': JSON.stringify({ [GECKO_ID]: UUID }).replace(/"/g, '\\"'),
    // Ask for a Hong Kong interface: Firefox has none installed, which shows
    // what it does with such a request (the test asserts what it found).
    'intl.locale.requested': 'zh-HK,en-US',
  };
  try {
    runner = await webExt.cmd.run(
      { sourceDir: extDir, firefox: binary, noInput: true, noReload: true, pref, args: ['--remote-debugging-port', String(port)], target: ['firefox-desktop'] },
      { shouldExitProgram: false },
    );
  } finally {
    for (const [k, v] of Object.entries(saved)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  }
  const fx = runner.extensionRunners[0];
  mainPid = fx.runningInfo.firefox.pid;
  profileDir = fx.profile.path();
  assert.ok(path.resolve(profileDir).startsWith(path.resolve(tmpRoot)), `profile ${profileDir} is a new temporary one`);
  bidi = await Bidi.connect(`ws://127.0.0.1:${port}/session`);
  await bidi.send('session.new', { capabilities: {} });
  // BiDi may not navigate a tab to moz-extension:// itself: the e2e build
  // opens its settings page when installed (src/background/main.js).
  control = await contextOf(`${EXT}/options/options.html`);
  await bidi.waitFor(control, 'const bg = await browser.runtime.getBackgroundPage(); return !!(bg && bg.__lectureflowTest);', null, { what: 'the background test hooks' });
});

after(async () => {
  let rmError = '';
  let profileInside = true;
  try {
    if (bidi) {
      await bidi.send('browser.close', {}, { timeoutMs: 10000 }).catch(() => {});
      bidi.close();
    }
  } finally {
    // Wait for Firefox to go; make sure it does (also after a start that
    // failed half-way, when only the directories tell this run's processes).
    const waitGone = async (ms) => {
      const until = Date.now() + ms;
      while (Date.now() < until && ours().length) await new Promise((r) => setTimeout(r, 500));
    };
    await waitGone(15000);
    if (ours().length && runner) await runner.exit().catch(() => {});
    await waitGone(15000);
    // What is left and certainly this run's is stopped (run-processes.mjs):
    // the suite's own programs that name this run's directories and are new
    // since the snapshot (crashhelper.exe, which keeps the test runner's
    // output pipes open, so node would never exit) and, after a start that
    // failed half-way, the
    // Firefox this process started with what it started in turn (on the
    // development machine the security software hides those processes'
    // command lines, so a left crashhelper would not be found by name).
    for (const p of ours().filter((x) => x.stop)) {
      try {
        process.kill(p.ProcessId);
        console.log(`# firefox: stopped ${p.Name} (${p.ProcessId}), left behind by this run's Firefox`);
      } catch {}
    }
    await waitGone(10000);
    if (server) await server.close();
    // Removed with a few spaced attempts (about 23 s in all), the profile
    // first, and only when it is inside this run's directory (the assertion
    // in before checks that; should it have failed, the profile is not
    // touched and the run fails below); the last error is kept for the
    // assertion. On the development machine the security software keeps
    // other programs out of a Firefox profile while that Firefox runs, and
    // often for good after it has exited; no wait or retry gets past that
    // (NOTES, ext r4).
    profileInside = !profileDir || isInside(profileDir, tmpRoot, path);
    for (let i = 0, wait = 1000; i < 6 && tmpRoot && fs.existsSync(tmpRoot) && profileInside; i++, wait *= 2) {
      try {
        if (profileDir) fs.rmSync(profileDir, { recursive: true, force: true });
        fs.rmSync(tmpRoot, { recursive: true, force: true });
      } catch (e) {
        const at = e.path ? path.relative(tmpRoot, String(e.path).replace(/^\\\\\?\\/, '')) : '';
        rmError = `${e.code || e.message} on ${at || 'the run directory'} (attempt ${i + 1})`;
        if (i < 5) await new Promise((r) => setTimeout(r, Math.min(wait, 8000)));
      }
    }
    // Firefox's own temp directory, when it is not the run's (see before):
    // its entries, as far as they go; not checked.
    if (fxTemp && fxTemp !== tmpRoot && fs.existsSync(fxTemp)) {
      try {
        const [, stuck] = sweep(fxTemp, () => true, 0);
        if (stuck) console.log(`# firefox: ${stuck} entries of ${fxTemp} not removed yet (a later run tries again)`);
      } catch {}
    }
  }
  const left = ours();
  console.log(`# firefox: temporary profile ${profileDir} ${fs.existsSync(profileDir) ? 'STILL THERE' : 'removed'}; processes of this run left: ${left.length}${rmError && fs.existsSync(tmpRoot) ? `; last removal error: ${rmError}` : ''}`);
  assert.deepEqual(left, [], 'no Firefox process of this run is left');
  if (START_MENU_SHORTCUT) assert.equal(fileState(START_MENU_SHORTCUT), startMenuBefore, `${START_MENU_SHORTCUT} is as it was before the run`);
  assert.ok(profileInside, `the profile ${profileDir} is inside this run's directory ${tmpRoot} (it is not, so it was left alone)`);
  assert.equal(fs.existsSync(tmpRoot), false, `this run's directory, the temporary profile in it included, is gone${rmError ? ` (${rmError})` : ''}`);
});

// ------------------------------------------------------------- helpers

/** Runs in the settings tab with `bg` (the background page) at hand. */
const inBg = (body, args = null) => bidi.eval(control, `const bg = await browser.runtime.getBackgroundPage(); const T = bg.__lectureflowTest; ${body}`, args);

async function fakeLectureFlow(opts = {}) {
  await bidi.eval(control, `const bg = await browser.runtime.getBackgroundPage(); (${installFakeLectureFlow})(args, bg); return true;`, { courses: COURSES, ...opts });
}
const fakeReceived = () => inBg('return JSON.parse(JSON.stringify(bg.__fakeLF.received));');

/** The BiDi context of the tab showing `url`, once there is one. */
async function contextOf(url, timeoutMs = 20000) {
  const until = Date.now() + timeoutMs;
  while (Date.now() < until) {
    const tree = await bidi.send('browsingContext.getTree', { maxDepth: 0 });
    const c = tree.contexts.find((x) => x.url === url);
    if (c) return c.context;
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`no tab shows ${url}`);
}

/** An extension page in a new tab, opened by the extension (see contextOf). */
async function openExtTab(rel, readyBody = null) {
  const url = `${EXT}/${rel}`;
  await bidi.eval(control, 'await browser.tabs.create({ url: args, active: false }); return true;', url);
  const ctx = await contextOf(url);
  if (readyBody) await bidi.waitFor(ctx, readyBody, null, { what: `${rel} ready` });
  return ctx;
}

async function openTab(url, readyBody = null) {
  const ctx = await bidi.newTab();
  await bidi.navigate(ctx, url);
  if (readyBody) await bidi.waitFor(ctx, readyBody, null, { what: `${url} ready` });
  return ctx;
}

async function tabIdOf(url) {
  return inBg('const tabs = await browser.tabs.query({}); const t = tabs.find((x) => x.url === args); return t ? t.id : null;', url);
}

async function shot(ctx, name) {
  if (!SHOTS) return;
  const dir = path.join(root, 'docs', 'screenshots');
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, `firefox-${name}.png`), await bidi.screenshot(ctx));
}

const port = () => server.port;
const msgIn = (key, subs) => inBg('return browser.i18n.getMessage(args[0], args[1]);', [key, subs || []]);

// --------------------------------------------------------------- tests

test('the extension loads as a temporary add-on: manifest, settings page, interface language', async () => {
  const info = await bidi.eval(control, 'return { id: browser.runtime.id, version: browser.runtime.getManifest().version, mv: browser.runtime.getManifest().manifest_version, h1: document.querySelector("h1").textContent, options: document.querySelectorAll("#language option").length, ui: browser.i18n.getUILanguage(), send: browser.i18n.getMessage("sendButton"), perms: await browser.permissions.getAll() };');
  assert.equal(info.id, GECKO_ID);
  assert.equal(info.mv, 3);
  assert.equal(info.options, 7);
  assert.ok(info.h1.includes('LectureFlow'));
  // host access of the e2e build was granted at install (no prompt)
  assert.ok(info.perms.origins.includes('http://*.lf-e2e.test/*'), JSON.stringify(info.perms));
  // Firefox picks the add-on's locale among its own interface languages: a
  // requested zh-HK it has no language pack for does not reach _locales/zh_HK.
  const sendHK = JSON.parse(fs.readFileSync(path.join(root, '_locales', 'zh_HK', 'messages.json'), 'utf8')).sendButton.message;
  console.log(`# firefox: interface ${info.ui}; requested zh-HK; "sendButton" = ${info.send}`);
  assert.notEqual(info.send, sendHK);
});

test('generic detection and the badge on an authorized site; cookies only for the tracks’ host when sending', async () => {
  await fakeLectureFlow({ nextTaskId: 300 });
  await inBg(`await browser.cookies.set({ url: 'http://course.lf-e2e.test/', name: 'lf_track', value: args[0] });
    await browser.cookies.set({ url: 'http://other.lf-e2e.test/', name: 'lf_other', value: 'LF-FX-OTHER' }); return true;`, [COOKIE_TRACK]);
  const url = `http://course.lf-e2e.test:${port()}/mp4.html`;
  const page = await openTab(url, 'return document.readyState === "complete"');
  const tabId = await tabIdOf(url);
  assert.ok(tabId, 'tab id');
  // the badge counts by itself (authorized site, after the page settles)
  const badge = await bidi.waitFor(control, 'return (await browser.action.getBadgeText({ tabId: args })) || false;', tabId, { what: 'the badge', timeoutMs: 20000 });
  assert.equal(badge, '3');
  const r = await inBg('const r = await T.detectTab(args, { force: true }); const v = T.viewOf(r); return { status: r.status, total: r.summary.total, kinds: v.tracks.map((t) => t.kind + "/" + t.format).sort(), sources: v.tracks.map((t) => t.sources.join("+")) };', tabId);
  assert.equal(r.status, 'ok');
  assert.equal(r.total, 3);
  assert.deepEqual(r.kinds, ['audio/file', 'audio/file', 'video/file']);
  assert.ok(r.sources.some((s) => s.includes('webrequest')), `webRequest saw the media: ${r.sources}`);
  assert.ok(r.sources.some((s) => s.includes('element')));

  // The popup page, on that tab: ready, one click sends with the track's cookie only.
  const popup = await openExtTab(`popup/popup.html?tabId=${tabId}`, 'return document.body.dataset.view === "ready"');
  const sendLabel = await msgIn('sendButton');
  assert.equal(await bidi.eval(popup, 'return document.getElementById("primary").textContent.trim();'), sendLabel);
  await shot(popup, 'popup-generic');
  await bidi.eval(popup, 'document.getElementById("primary").click(); return true;');
  await bidi.waitFor(popup, 'return document.body.dataset.view === "queued"', null, { what: 'queued' });
  const enq = (await fakeReceived()).find((m) => m.type === 'enqueue');
  assert.equal(enq.platform, 'generic');
  assert.equal(enq.courseId, COURSES[0].id);
  assert.deepEqual(enq.cookies.map((c) => `${c.name}=${c.value}@${c.domain}`), [`lf_track=${COOKIE_TRACK}@course.lf-e2e.test`]);
  assert.ok(!JSON.stringify(enq).includes('LF-FX-OTHER'), 'no cookie of another host');
  await bidi.send('browsingContext.close', { context: popup });
  await bidi.send('browsingContext.close', { context: page });
});

test('Panopto: the main-world response observation works (the full MP4 is offered), the in-page button appears and its card opens', async () => {
  server.panopto.downloadUrl = '/Panopto/Podcast/Download/whole.mp4';
  try {
    await fakeLectureFlow({ nextTaskId: 310 });
    const scripts = await inBg('return (await browser.scripting.getRegisteredContentScripts()).map((s) => s.id + ":" + (s.world || "ISOLATED"));');
    assert.ok(scripts.includes('lf-observe-main:MAIN'), scripts.join());
    assert.ok(scripts.includes('lf-inpage:ISOLATED'), scripts.join());
    const url = `http://${PANOPTO.host}:${port()}/Panopto/Pages/Viewer.aspx?id=${PANOPTO.deliveryId}`;
    const page = await openTab(url, 'return document.body && document.body.dataset.ready === "1"');
    // the page still got its whole answer through the wrapper
    assert.equal(await bidi.eval(page, 'return window.__pageSawUser || null;'), PANOPTO.userName);
    const tabId = await tabIdOf(url);
    const r = await inBg('const r = await T.detectTab(args, { force: true }); const v = T.viewOf(r); return { roles: v.tracks.map((t) => t.role + ":" + t.format), policy: r.downloadPolicy, title: r.lecture && r.lecture.title };', tabId);
    assert.ok(r.roles.includes('podcast:file'), `observed MP4: ${r.roles}`);
    assert.equal(r.title, PANOPTO.title);
    // nothing of the viewer reached the extension
    const kept = JSON.stringify(await inBg('return [T.lastResult(args), await browser.storage.local.get(null), await browser.storage.session.get(null)];', tabId));
    assert.ok(!kept.includes(PANOPTO.userEmail) && !kept.includes(PANOPTO.userName));

    // The in-page button (open shadow root in the e2e build) and its card.
    const label = await bidi.waitFor(page, 'const h = document.querySelector(\'lectureflow-ui[data-kind="player"]\'); const b = h && h.shadowRoot && h.shadowRoot.querySelector(".ip-btn"); return b ? b.textContent.trim() : false;', null, { what: 'the in-page button' });
    assert.equal(label, await msgIn('sendButton'));
    assert.equal(await bidi.eval(page, 'const h = document.querySelector(\'lectureflow-ui[data-kind="player"]\'); return h.parentElement.id + ">" + (h.nextElementSibling && h.nextElementSibling.id);'), 'viewerHeader>header-right-react');
    // styles reached the shadow root (adoptedStyleSheets, or the <style> fallback)
    const styled = await bidi.eval(page, 'const h = document.querySelector(\'lectureflow-ui[data-kind="player"]\'); return { adopted: h.shadowRoot.adoptedStyleSheets.length, style: !!h.shadowRoot.querySelector("style"), radius: getComputedStyle(h.shadowRoot.querySelector(".ip-btn")).borderTopLeftRadius };');
    console.log(`# firefox: in-page styles ${JSON.stringify(styled)}`);
    assert.ok(styled.adopted > 0 || styled.style, JSON.stringify(styled));
    assert.equal(styled.radius, '8px', 'the design tokens apply (controls: radius 8px)');
    await bidi.eval(page, 'document.querySelector(\'lectureflow-ui[data-kind="player"]\').shadowRoot.querySelector(".ip-btn").click(); return true;');
    const chosen = await bidi.waitFor(page, 'const c = document.querySelector(\'lectureflow-ui[data-kind="card"]\'); const f = c && c.shadowRoot.querySelector(\'.lf-card[data-view="ready"]\'); return f ? f.querySelector("#chosen").textContent : false;', null, { what: 'the in-page card' });
    const podcastLabel = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8')).tracks[0].label;
    assert.ok(Object.values(podcastLabel).some((l) => chosen.startsWith(l)), chosen);
    await shot(page, 'inpage-player');
    await bidi.eval(page, 'document.querySelector(\'lectureflow-ui[data-kind="card"]\').shadowRoot.querySelector("#primary").click(); return true;');
    await bidi.waitFor(page, 'const c = document.querySelector(\'lectureflow-ui[data-kind="card"]\'); return !!(c && c.shadowRoot.querySelector(\'.lf-card[data-view="queued"]\'));', null, { what: 'queued' });
    const enq = (await fakeReceived()).find((m) => m.type === 'enqueue');
    assert.deepEqual(enq.tracks.map((t) => t.role), ['podcast']);
    assert.equal(enq.sessionId, PANOPTO.deliveryId);
    assert.deepEqual(enq.cookies, []);
    await bidi.send('browsingContext.close', { context: page });
  } finally {
    server.panopto.downloadUrl = null;
  }
});

test('Panopto list, LectureFlow not running: row buttons appear, the page’s own check says noLaunch and starts nothing', async () => {
  await fakeLectureFlow({ mode: 'stopped' });
  const page = await openTab(`http://${PANOPTO.host}:${port()}/Panopto/Pages/Sessions/List.aspx`, 'return document.body && document.body.dataset.ready === "1"');
  const rows = await bidi.waitFor(page, 'const n = document.querySelectorAll(\'lectureflow-ui[data-kind="row"]\').length; return n >= args ? n : false;', LIST.length, { what: 'row buttons' });
  assert.equal(rows, LIST.length);
  await new Promise((r) => setTimeout(r, 1000));
  const hellos = (await fakeReceived()).filter((m) => m.type === 'hello');
  assert.ok(hellos.length >= 1 && hellos.every((m) => m.noLaunch === true), JSON.stringify(hellos));
  assert.equal(await inBg('return bg.__fakeLF.launches;'), 0);
  assert.equal(await bidi.eval(page, 'return [...document.querySelectorAll(\'lectureflow-ui[data-kind="row"]\')].filter((h) => h.shadowRoot.querySelector("[data-row-state]")).length;'), 0);
  await shot(page, 'inpage-list');
  await bidi.send('browsingContext.close', { context: page });
});

test('the toolbar popup itself opens on the active tab and is ready (action.openPopup)', async () => {
  await fakeLectureFlow({ nextTaskId: 320 });
  const url = `http://course.lf-e2e.test:${port()}/mp4.html`;
  const page = await openTab(url, 'return document.readyState === "complete"');
  const tabId = await tabIdOf(url);
  const r = await inBg(`await browser.tabs.update(args, { active: true });
    let err = '';
    try { await browser.action.openPopup(); } catch (e) { err = String((e && e.message) || e); }
    for (let i = 0; i < 50; i++) {
      const v = browser.extension.getViews({ type: 'popup' })[0];
      if (v && v.document.body.dataset.view === 'ready') break;
      await new Promise((r) => setTimeout(r, 100));
    }
    const views = browser.extension.getViews({ type: 'popup' });
    const d = views[0] ? views[0].document : null;
    return { err, views: views.length, view: d && d.body.dataset.view, primary: d && d.getElementById('primary').textContent.trim(), width: d && d.documentElement.scrollWidth };`, tabId);
  assert.equal(r.err, '');
  assert.equal(r.views, 1);
  assert.equal(r.view, 'ready', 'no ?tabId: the popup took the active tab');
  assert.equal(r.primary, await msgIn('sendButton'));
  assert.ok(r.width <= 360, `width ${r.width}`);
  await inBg('for (const v of browser.extension.getViews({ type: "popup" })) v.close(); return true;');
  await bidi.send('browsingContext.close', { context: page });
});

test('a video inside a closed shadow root is found by the page scan (Element.openOrClosedShadowRoot)', async () => {
  const url = `http://course.lf-e2e.test:${port()}/empty.html`;
  const page = await openTab(url, 'return document.readyState === "complete"');
  await bidi.eval(page, 'const h = document.createElement("div"); document.body.append(h); const sr = h.attachShadow({ mode: "closed" }); const v = document.createElement("video"); v.preload = "metadata"; v.src = "/media/lecture.mp4"; sr.append(v); return true;');
  const tabId = await tabIdOf(url);
  const r = await inBg('const r = await T.detectTab(args, { force: true }); return T.viewOf(r).tracks.map((t) => ({ kind: t.kind, size: t.sizeBytes, sources: t.sources }));', tabId);
  const video = r.find((t) => t.size === 123456);
  assert.ok(video && video.sources.includes('element'), JSON.stringify(r));
  await bidi.send('browsingContext.close', { context: page });
});

test('the player inside a page the extension cannot see: the top-level site from ancestorOrigins, the confirmation in the extension’s window (security review M4)', async () => {
  server.panopto.downloadUrl = '/Panopto/Podcast/Download/whole.mp4';
  try {
    await fakeLectureFlow({ nextTaskId: 340 });
    await inBg('await browser.storage.local.remove("trustedEmbedders"); return true;');
    const top = `http://attacker.lf-e2e-x.test:${port()}`;
    const page = await openTab(`${top}/canvas/embed.html?hidden=1`, 'return document.readyState === "complete"');
    // the Panopto frame inside it
    let child = null;
    for (let i = 0; i < 50 && !child; i++) {
      const tree = await bidi.send('browsingContext.getTree', { root: page });
      child = (tree.contexts[0].children || []).find((c) => c.url.includes('/Panopto/Pages/Embed.aspx'));
      if (!child) await new Promise((r) => setTimeout(r, 200));
    }
    assert.ok(child, 'the embedded player frame');
    // what Firefox tells the frame about where it is embedded
    assert.deepEqual(await bidi.eval(child.context, 'return location.ancestorOrigins ? [...location.ancestorOrigins] : null;'), [top]);
    await bidi.waitFor(child.context, 'const h = document.querySelector(\'lectureflow-ui[data-kind="player"]\'); return !!(h && h.shadowRoot.querySelector(".ip-btn"));', null, { what: 'the in-page button' });
    await bidi.eval(child.context, 'document.querySelector(\'lectureflow-ui[data-kind="player"]\').shadowRoot.querySelector(".ip-btn").click(); return true;');
    // the extension's window opens; no card in the page
    let win = null;
    for (let i = 0; i < 75 && !win; i++) {
      const tree = await bidi.send('browsingContext.getTree', { maxDepth: 0 });
      const c = tree.contexts.find((x) => x.url.startsWith(`${EXT}/confirm/confirm.html`));
      if (c) win = c.context;
      else await new Promise((r) => setTimeout(r, 200));
    }
    assert.ok(win, 'the confirmation window');
    await bidi.waitFor(win, 'return document.body.dataset.view === "ready"', null, { what: 'the window ready' });
    assert.equal(await bidi.eval(child.context, 'return document.querySelectorAll(\'lectureflow-ui[data-kind="card"]\').length;'), 0);
    const why = await bidi.eval(win, 'return document.getElementById("embed-why").textContent;');
    assert.ok(why.includes(top), why);
    assert.equal(await bidi.eval(win, 'return document.getElementById("trust-line").hidden;'), false);
    await shot(win, 'confirm-window');
    // after the delay, the window sends for that frame
    await new Promise((r) => setTimeout(r, 700));
    await bidi.eval(win, 'document.getElementById("primary").click(); return true;');
    await bidi.waitFor(win, 'return document.body.dataset.view === "queued"', null, { what: 'queued' });
    const enq = (await fakeReceived()).find((m) => m.type === 'enqueue');
    assert.equal(enq.sessionId, PANOPTO.deliveryId);
    assert.ok(enq.referer.startsWith(`http://${PANOPTO.host}:`), enq.referer);
    await bidi.send('browsingContext.close', { context: win });
    await bidi.send('browsingContext.close', { context: page });
  } finally {
    server.panopto.downloadUrl = null;
  }
});

test('the language chosen in the settings reaches Cantonese, which Firefox itself never picks', async () => {
  await fakeLectureFlow({ nextTaskId: 330 });
  const url = `http://course.lf-e2e.test:${port()}/mp4.html`;
  const page = await openTab(url, 'return document.readyState === "complete"');
  const tabId = await tabIdOf(url);
  await inBg('await browser.storage.local.set({ uiLanguage: "zh_HK" }); await new Promise((r) => setTimeout(r, 200)); return true;');
  try {
    const popup = await openExtTab(`popup/popup.html?tabId=${tabId}`, 'return document.body.dataset.view === "ready"');
    const hk = JSON.parse(fs.readFileSync(path.join(root, '_locales', 'zh_HK', 'messages.json'), 'utf8'));
    assert.equal(await bidi.eval(popup, 'return document.getElementById("primary").textContent.trim();'), hk.sendButton.message);
    assert.equal(await bidi.eval(popup, 'return document.documentElement.lang;'), 'yue-Hant-HK');
    await shot(popup, 'popup-zh_HK');
    await bidi.send('browsingContext.close', { context: popup });
  } finally {
    await inBg('await browser.storage.local.remove("uiLanguage"); return true;');
    await bidi.send('browsingContext.close', { context: page });
  }
});

test('nothing reached any host but the local test server', () => {
  const hosts = new Set(server.requests.map((r) => r.host));
  for (const h of hosts) assert.ok(['127.0.0.1', ...HOSTS].includes(h), h);
});
