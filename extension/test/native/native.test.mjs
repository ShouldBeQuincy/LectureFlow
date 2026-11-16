// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// M2 acceptance, automated (user decision 2026-10-27 03:40: "只做自动化",
// never the user's own Chrome): the store build of the extension
// (dist/chrome) in Playwright's Chrome for Testing, the real native
// messaging host and a real, isolated LectureFlow, against a made-up
// Panopto on local servers. Every scenario is checked on LectureFlow's side
// (GET /api/downloads, /api/lectures/{id}, the files):
//
//   1  the popup's three choices (主要音视频 / 双机位 / 仅音频), sent once
//      each: fetched, the lecture made, the second angle kept;
//   2  in-page: the player's button, the folder list's "更新这门课程"
//      (dedup by title; a second run sends only what is new), a row's
//      button and the embed player inside an LMS iframe (an LMS not yet
//      trusted: the extension's own confirmation window; trusted: the card
//      in the page, armed after 600 ms — ext-step9, click-jacking; a small
//      embedded player with no room for the card: the window, without the
//      click-jacking note);
//   3  a possibly restricted recording (reminder → overrideDisabled), DRM
//      (not sent; and what LectureFlow does with a DRM track sent anyway);
//   4  the same session again (已导入, queue.duplicate), the passive check
//      that must not start LectureFlow, "未连接", the host starting it,
//      needs_refresh after a restart and "续传" with new cookies;
//   5  no cookie, signed query, viewer data or media path on disk.
//
// Windows only. It writes one registry value, HKCU\Software\Chromium\
// NativeMessagingHosts\org.lectureflow.nmhost (the user agreed to that one
// only), and removes it at the end, also after a failure (lib/registry.mjs).
//
//   cd extension
//   node scripts/build.mjs --target chrome
//   node --test --test-concurrency=1 "test/native/*.test.mjs"
//
// Environment: LF_NATIVE_WORKDIR (keep everything there; default a new
// temporary directory, removed afterwards unless LF_NATIVE_KEEP=1),
// LF_NATIVE_BIN (a directory with lectureflow.exe and
// lectureflow-nmhost.exe; default: go build from this checkout),
// LF_NATIVE_EXT (an unpacked Chrome build; default dist/chrome),
// LF_FFMPEG, LF_FFPROBE, LF_ARIA2C (default desktop/build/resources/bin).

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { selfSignedCert } from './lib/cert.mjs';
import { makeMedia, streamKinds } from './lib/media.mjs';
import { startSite, PANOPTO_HOST, CANVAS_HOST, VIEWER_EMAIL, VIEWER_NAME } from './lib/site.mjs';
import { readHostKey, createHostKey, deleteHostKey, HOST_KEY, MARKER } from './lib/registry.mjs';
import * as I from './lib/instance.mjs';
import { prepareExtension, launchBrowser, waitContentScripts, tabIdOf } from './lib/browser.mjs';
import { shadowUI } from './lib/shadow.mjs';
import { exchange, HELLO } from './lib/nmdirect.mjs';
import { scan } from './lib/scan.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const repo = path.resolve(root, '..');
const binDir = path.join(repo, 'desktop', 'build', 'resources', 'bin');
const tool = (env, name) => process.env[env] || path.join(binDir, name);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const CARD = 'lectureflow-ui[data-kind="card"]';
const rowHost = (s) => `tr[id="${s.deliveryId}"] lectureflow-ui[data-kind="row"]`;

if (process.platform !== 'win32') {
  test('native end-to-end (Windows only: registry and named pipes)', { skip: 'Windows only' }, () => {});
} else {
  register();
}

function register() {
  const WORK = process.env.LF_NATIVE_WORKDIR ? path.resolve(process.env.LF_NATIVE_WORKDIR) : fs.mkdtempSync(path.join(os.tmpdir(), `${MARKER}-`));
  const KEEP = !!process.env.LF_NATIVE_WORKDIR || process.env.LF_NATIVE_KEEP === '1';
  let P; // work paths
  let site;
  let api;
  let course;
  let B; // browser: { context, extensionId, worker }
  let ffprobe;
  const sent = {}; // sessions of the earlier scenarios, by name
  const evidence = [];
  const note = (t, line) => {
    evidence.push(line);
    t.diagnostic(line);
  };

  // ------------------------------------------------------------ helpers

  async function openPage(url, ready) {
    const page = await B.context.newPage();
    await page.goto(url);
    if (ready) await page.waitForSelector(ready, { timeout: 20000, state: 'attached' });
    return page;
  }

  async function openPopup(page, view) {
    const tabId = await tabIdOf(B.worker, page);
    assert.ok(tabId, 'the page has a tab');
    const popup = await B.context.newPage();
    await popup.setViewportSize({ width: 360, height: 640 });
    await popup.goto(`chrome-extension://${B.extensionId}/popup/popup.html?tabId=${tabId}`);
    if (view) await popup.waitForSelector(`body[data-view="${view}"]`, { timeout: 40000 });
    return popup;
  }

  async function frameOf(page, urlPart) {
    for (let i = 0; i < 100; i++) {
      const f = page.frames().find((x) => x.url().includes(urlPart));
      if (f) return f;
      await sleep(100);
    }
    throw new Error(`no frame ${urlPart}`);
  }

  const downloads = async () => (await api('GET', '/api/downloads')).items;

  /** Polls GET /api/downloads until the session's task satisfies pred (default: done with a lecture). */
  async function waitTask(sessionId, pred = (d) => d.state === 'done' && d.lecture_id, timeout = 90000) {
    const end = Date.now() + timeout;
    let last = null;
    while (Date.now() < end) {
      last = (await downloads()).find((d) => d.session_id === sessionId) || null;
      if (last && pred(last)) return last;
      if (last && ['failed', 'cancelled'].includes(last.state) && !pred(last)) break;
      await sleep(300);
    }
    throw new Error(`task of ${sessionId}: ${last ? JSON.stringify({ state: last.state, error: last.error, lecture: last.lecture_id }) : 'none'}`);
  }

  /** The lecture's directory and its media file (media.*; prep adds audio.wav, stream.mp4, …). */
  function lectureFiles(lec) {
    const dir = path.join(P.data, 'courses', lec.course_id, 'lectures', lec.id);
    const files = fs.readdirSync(dir);
    const media = files.filter((f) => /^media\.[a-z0-9]+$/i.test(f)).map((f) => path.join(dir, f));
    return { dir, files, media };
  }

  async function recordCookieCalls() {
    await (await B.worker()).evaluate(() => {
      if (!globalThis.__lfNativeCookieCalls) {
        globalThis.__lfNativeCookieCalls = [];
        const real = chrome.cookies.getAll;
        chrome.cookies.getAll = function (details, ...rest) {
          globalThis.__lfNativeCookieCalls.push(JSON.parse(JSON.stringify(details)));
          return real.call(this, details, ...rest);
        };
      }
      globalThis.__lfNativeCookieCalls.length = 0;
    });
  }
  const cookieCalls = async () => (await B.worker()).evaluate(() => [...(globalThis.__lfNativeCookieCalls || [])]);

  const readText = (f) => {
    try {
      return fs.readFileSync(f, 'utf8');
    } catch {
      return '';
    }
  };
  const appStdout = () =>
    fs
      .readdirSync(P.logs)
      .filter((f) => f.endsWith('.stdout.log'))
      .map((f) => readText(path.join(P.logs, f)))
      .join('\n');

  /**
   * The Referer that strict-origin-when-cross-origin gives aria2c's requests
   * for the platform's media (another origin than the platform's pages):
   * the pages' origin only, "https://tenant.cloud.panopto.eu:P/" (security
   * review L2; the media is on http://127.0.0.1, which is potentially
   * trustworthy, so the origin is sent and not nothing).
   */
  const platformOriginReferer = (r) => r.refererScheme === 'https:' && r.referer === `${PANOPTO_HOST}:${site.pagesPort}` && r.refererPath === '/' && !r.refererQuery;
  const referers = (reqs) => JSON.stringify([...new Set(reqs.map((r) => `${r.refererScheme}//${r.referer}${r.refererPath}${r.refererQuery ? '?…' : ''}`))]);

  /** The extension's confirmation window (src/confirm/), opened by an in-page button inside an untrusted site. */
  const isConfirm = (p) => p.url().startsWith(`chrome-extension://${B.extensionId}/confirm/confirm.html`);
  const trustedSites = async () => (await B.worker()).evaluate(async () => Object.keys((await chrome.storage.local.get('trustedEmbedders')).trustedEmbedders || {}));

  /** A confirming control acts only once it has been shown, enabled, for 600 ms (src/ui/clickguard.js). */
  async function clickArmed(locator) {
    await locator.waitFor({ state: 'visible', timeout: 30000 });
    for (let i = 0; i < 300 && !(await locator.isEnabled()); i++) await sleep(100);
    await sleep(700);
    await locator.click();
  }

  async function until(cond, what, timeout = 20000) {
    const end = Date.now() + timeout;
    while (Date.now() < end) {
      if (await cond()) return;
      await sleep(150);
    }
    throw new Error(`timed out: ${what}`);
  }

  // -------------------------------------------------------------- setup

  before(async () => {
    P = I.workPaths(WORK);
    console.log(`# work directory: ${WORK}`);
    // The one registry value: absent at the start (or left by an earlier run of this test).
    const existing = readHostKey();
    if (existing !== null && !existing.includes(MARKER)) throw new Error(`${HOST_KEY} exists and is not this test's: stopping`);
    const ffmpeg = tool('LF_FFMPEG', 'ffmpeg.exe');
    ffprobe = tool('LF_FFPROBE', 'ffprobe.exe');
    const aria2c = tool('LF_ARIA2C', 'aria2c.exe');
    for (const f of [ffmpeg, ffprobe, aria2c]) assert.ok(fs.existsSync(f), `missing ${f} (stage the desktop build, or set LF_FFMPEG / LF_FFPROBE / LF_ARIA2C)`);
    const distChrome = process.env.LF_NATIVE_EXT || path.join(root, 'dist', 'chrome');
    assert.ok(fs.existsSync(path.join(distChrome, 'manifest.json')), `run "node scripts/build.mjs --target chrome" first (${distChrome})`);

    const bins = I.buildBinaries(repo, P.bin);
    const media = makeMedia(ffmpeg, P.media);
    site = await startSite({ tls: selfSignedCert([PANOPTO_HOST, '*.cloud.panopto.eu', CANVAS_HOST]), media });
    const { pipe, token } = I.writeInstance(P, { app: bins.app, ffmpeg, aria2c, fakeAsr: path.join(repo, 'testdata', 'fake_asr.json') });
    I.writeHost(P, { host: bins.host, pipe });

    // From here on, whatever happens, the instance is stopped and the value removed.
    const cleanup = () => {
      I.killInstance(P);
      deleteHostKey();
    };
    process.once('exit', cleanup);
    for (const sig of ['SIGINT', 'SIGTERM', 'SIGBREAK']) {
      process.once(sig, () => {
        cleanup();
        process.exit(130);
      });
    }
    createHostKey(P.manifest);
    console.log(`# ${HOST_KEY} → ${readHostKey()}`);

    await I.startInstance(P, 'test');
    api = I.apiClient(P, token);
    course = await api('POST', '/api/courses', { name: 'CSC1003 Native E2E (test)' });

    const extDir = prepareExtension(distChrome, P.ext);
    B = await launchBrowser({ extDir, userDataDir: P.browser, repo });
    assert.equal(B.extensionId, I.DEV_EXTENSION_ID, 'the development id the host manifest allows');
    await waitContentScripts(B.worker);
  });

  after(async () => {
    if (B) await B.context.close().catch(() => {});
    if (P) await I.stopInstance(P).catch(() => I.killInstance(P));
    if (site) await site.close().catch(() => {});
    const q = deleteHostKey();
    console.log(`# after removing the value, reg query ${HOST_KEY}: ${q.split(/\r?\n/).filter(Boolean).join(' | ') || '(nothing)'}`);
    assert.equal(readHostKey(), null, 'the registry value is gone');
    if (P) fs.writeFileSync(path.join(P.root, 'evidence.log'), evidence.join('\n') + '\n');
    if (!KEEP) fs.rmSync(WORK, { recursive: true, force: true });
  });

  // ---------------------------------------------------------- scenarios

  test('1 popup: 主要音视频, 双机位 and 仅音频 each sent once; fetched, lecture made, second angle kept', async (t) => {
    await recordCookieCalls();
    const cases = [
      { name: 'main', title: 'CSC1003/L01/01 Mon Oct 19 09:35:00 BST 2026', roles: ['podcast'], streams: ['audio', 'video'] },
      { name: 'dual', title: 'CSC1003/L01/02 Tue Oct 20 09:35:00 BST 2026', twoScreens: true, roles: ['primary-audio', 'screen', 'screen'], streams: ['audio', 'video'], angle: true },
      { name: 'audio', title: 'CSC1003/L01/03 Wed Oct 21 09:35:00 BST 2026', roles: ['primary-audio'], streams: ['audio'] },
    ];
    for (const c of cases) {
      const s = site.addSession({ title: c.title, twoScreens: !!c.twoScreens });
      sent[c.name] = s;
      const page = await openPage(site.viewerUrl(s), 'body[data-ready="1"]');
      const popup = await openPopup(page, 'ready');
      assert.equal(await popup.getAttribute('#conn', 'data-nm-state'), 'connected', 'the real host reached the isolated LectureFlow');
      // the platform's name; its profile in the tooltip (T26 G11)
      assert.match(await popup.textContent('#lecture .lec-meta'), /Panopto$/);
      assert.match(await popup.getAttribute('#lecture .lec-meta', 'title'), /^Panopto 对策文件 /);
      assert.equal(await popup.inputValue('#course'), course.id, 'LectureFlow’s own course list');
      if (c.name !== 'main') await popup.click(`#presets [data-preset="${c.name}"]`);
      assert.equal(await popup.getAttribute(`#presets [data-preset="${c.name}"]`, 'aria-pressed'), 'true');
      await popup.click('#primary');
      await popup.waitForSelector('body[data-view="queued"]', { timeout: 20000 });
      if (c.name === 'audio') {
        // left open: LectureFlow's status pushes reach it through the host
        await popup.waitForSelector('#task-state[data-state="done"]', { timeout: 60000 });
      }
      // closing the popup does not stop the fetch (M2)
      await popup.close();
      const d = await waitTask(s.deliveryId);
      const fetched = site.state.requests.filter((r) => r.aria2 && r.path.startsWith(`/sessions/${s.ids.media}/`));
      assert.ok(fetched.length >= c.roles.length, 'aria2c fetched the media');
      assert.ok(fetched.every((r) => platformOriginReferer(r) && !r.hasCookie), `with the page’s origin as Referer (cross-origin), without a cookie: ${referers(fetched)}`);
      assert.equal(d.platform, 'panopto');
      assert.equal(d.course_id, course.id);
      assert.equal(d.override_disabled, false);
      assert.equal(d.title, c.title);
      // the title's own clock, whatever this computer's zone (tz-fix)
      const [, dd, hm] = c.title.match(/Oct (\d\d) (\d\d:\d\d)/);
      assert.equal(d.recorded_at, `2026-10-${dd}T${hm}`);
      assert.deepEqual(d.tracks.map((x) => x.role), c.roles);
      const lec = await api('GET', `/api/lectures/${d.lecture_id}`);
      assert.equal(lec.course_id, course.id);
      assert.deepEqual((lec.angles || []).map((a) => a.name), c.angle ? ['angle2'] : []);
      const f = lectureFiles(lec);
      assert.equal(f.media.length, 1, f.files.join(', '));
      assert.deepEqual(streamKinds(ffprobe, f.media[0]).sort(), c.streams);
      if (c.angle) {
        assert.deepEqual(streamKinds(ffprobe, path.join(f.dir, 'angle2.mp4')), ['video']);
        const res = await fetch(`${I.instanceUrl(P)}/api/lectures/${lec.id}/angles/angle2`, { headers: { 'X-LF-Token': JSON.parse(readText(path.join(P.state, 'instance.json'))).token, Range: 'bytes=0-15' } });
        assert.equal(res.status, 206);
      }
      note(t, `1 ${c.name}: task #${d.id} ${d.state} tracks=${d.tracks.map((x) => `${x.role}:${x.kind}/${x.format}`).join(',')} lecture=${lec.id} files=[${f.files.join(', ')}] media streams=${streamKinds(ffprobe, f.media[0]).join('+')}${c.angle ? ` angles=${lec.angles.map((a) => a.name)}` : ''}`);
      await page.close();
    }
    assert.deepEqual(await cookieCalls(), [], 'Panopto’s media needs no cookie (cookieUrls: []): none read');
    assert.ok(!site.state.requests.some((r) => r.server === 'media' && r.hasCookie), 'no cookie reached the media server');
  });

  test('2a in-page: the player’s “发送至 LectureFlow” (closed shadow root) sends', async (t) => {
    const s = site.addSession({ title: 'CSC1003/L02/01 Mon Oct 12 09:35:00 BST 2026' });
    const page = await openPage(site.viewerUrl(s), 'body[data-ready="1"]');
    const ui = shadowUI(B.context, page);
    await ui.waitFor('lectureflow-ui[data-kind="player"]', '.ip-btn', { timeout: 20000 });
    assert.deepEqual(
      await page.evaluate(() => {
        const h = document.querySelector('lectureflow-ui[data-kind="player"]');
        return { parent: h.parentElement.id, next: h.nextElementSibling && h.nextElementSibling.id, open: h.shadowRoot !== null };
      }),
      { parent: 'viewerHeader', next: 'header-right-react', open: false },
    );
    await ui.click('lectureflow-ui[data-kind="player"]', '.ip-btn');
    await ui.waitFor(CARD, '.lf-card[data-view="ready"]', { timeout: 30000 });
    assert.match((await ui.read(CARD, '#primary')).text, /确认发送/);
    await ui.click(CARD, '#primary');
    await ui.waitFor(CARD, '.lf-card[data-view="queued"]', { timeout: 20000 });
    const d = await waitTask(s.deliveryId);
    assert.deepEqual(d.tracks.map((x) => x.role), ['podcast']);
    note(t, `2a player button: task #${d.id} ${d.state} lecture=${d.lecture_id}`);
    await page.close();
  });

  test('2b folder list: “更新这门课程” sends the new ones (dedup by title); a second run sends only the one added', async (t) => {
    const L = [
      site.addSession({ title: 'CSC1003/L03/01 Mon Sep 28 09:35:00 BST 2026', list: true }),
      site.addSession({ title: 'CSC1003/L03/02 Tue Sep 29 09:35:00 BST 2026', list: true }),
      site.addSession({ title: 'CSC1003/L03/03 Wed Sep 30 09:35:00 BST 2026', list: true }),
    ];
    // another session whose title is that of a lecture already in the course (scenario 1)
    const T = site.addSession({ title: sent.main.title, list: true });
    sent.list = L;
    const before = (await downloads()).length;
    const page = await openPage(site.listUrl(), 'body[data-ready="1"]');
    const ui = shadowUI(B.context, page);
    await ui.waitFor(rowHost(T), '.ip-btn', { timeout: 20000 });
    await ui.click('lectureflow-ui[data-kind="header"]', '.ip-btn');
    await ui.waitFor(CARD, '.lf-card[data-phase="idle"]', { timeout: 30000 });
    const summary1 = (await ui.read(CARD, '#bulk-summary')).text;
    assert.match(summary1, /^共 4 节：1 节已导入（按会话与标题去重）.*3 节将发送$/);
    const rows1 = await ui.readAll(CARD, '#bulk-rows .bulk-row');
    assert.deepEqual(rows1.map((r) => [r.dataset.sessionId, r.dataset.state]), [...L.map((s) => [s.deliveryId, 'new']), [T.deliveryId, 'imported']]);
    await ui.click(CARD, '#primary');
    await ui.waitFor(CARD, '.lf-card[data-phase="done"]', { timeout: 90000 });
    for (const s of L) await waitTask(s.deliveryId);
    assert.ok(!(await downloads()).some((d) => d.session_id === T.deliveryId), 'the title match was not sent');
    note(t, `2b first run: "${summary1}" → ${L.length} tasks done`);

    // the folder gets one more lecture
    const L4 = site.addSession({ title: 'CSC1003/L03/04 Thu Oct 01 09:35:00 BST 2026', list: true });
    sent.list.push(L4);
    await page.reload();
    await page.waitForSelector('body[data-ready="1"]', { state: 'attached' });
    // the page's own row check shows LectureFlow's states
    await ui.waitFor(rowHost(L[0]), '[data-row-state="imported"]', { timeout: 30000 });
    await ui.click('lectureflow-ui[data-kind="header"]', '.ip-btn');
    await ui.waitFor(CARD, '.lf-card[data-phase="idle"]', { timeout: 30000 });
    const summary2 = (await ui.read(CARD, '#bulk-summary')).text;
    assert.match(summary2, /^共 5 节：4 节已导入（按会话与标题去重）.*1 节将发送$/);
    await ui.click(CARD, '#primary');
    await ui.waitFor(CARD, '.lf-card[data-phase="done"]', { timeout: 90000 });
    await waitTask(L4.deliveryId);
    assert.equal((await downloads()).length, before + 4, 'three, then only the new one');
    note(t, `2b second run: "${summary2}" → 1 task done; tasks ${before} → ${before + 4}`);
    await page.close();
  });

  test('2c LMS iframe (LMS site not granted): not trusted, the row button confirms in the extension’s window; trusted, the embed player’s card confirms in the page', async (t) => {
    assert.deepEqual(await trustedSites(), [], 'no embedding site trusted yet');
    const L5 = site.addSession({ title: 'CSC1003/L03/05 Fri Oct 02 09:35:00 BST 2026', list: true });
    const page = await openPage(`${site.canvasOrigin}/canvas/list.html`);
    const frame = await frameOf(page, '/Panopto/Pages/Sessions/List.aspx');
    await frame.waitForSelector('body[data-ready="1"]', { state: 'attached', timeout: 20000 });
    const ui = shadowUI(B.context, page, frame);
    // the rows already in LectureFlow say so, inside the LMS page too
    await ui.waitFor(rowHost(sent.list[0]), '[data-row-state="imported"]', { timeout: 30000 });
    // An LMS the user has not trusted (ext-step9, click-jacking): the row's
    // button opens the extension's own window; nothing opens in the page.
    const [win] = await Promise.all([B.context.waitForEvent('page', { predicate: isConfirm, timeout: 20000 }), ui.click(rowHost(L5), '.ip-btn')]);
    await win.waitForSelector('body[data-view="ready"]', { timeout: 30000 });
    assert.equal(await ui.read(CARD, '.lf-card'), null, 'no card in the LMS page');
    const why = await win.textContent('#embed-why');
    assert.ok(why.includes(`嵌在 ${site.canvasOrigin} 的网页里`), why);
    assert.equal((await win.textContent('#embed-from')).trim(), `录播页面：${PANOPTO_HOST}:${site.pagesPort}`);
    assert.equal(await win.isVisible('#trust-line'), true);
    assert.equal(await win.isChecked('#trust'), false);
    assert.ok((await win.textContent('#trust-line')).includes(`以后在 ${site.canvasOrigin} 里直接在页面上确认`));
    assert.match(await win.textContent('#lecture'), /CSC1003\/L03\/05/);
    assert.equal(await win.inputValue('#course'), course.id, 'LectureFlow’s own course list, in the window');
    assert.match(await win.textContent('#primary'), /确认发送/);
    await clickArmed(win.locator('#primary'));
    await win.waitForSelector('body[data-view="queued"]', { timeout: 20000 });
    const d1 = await waitTask(L5.deliveryId);
    assert.equal(d1.course_id, course.id);
    assert.deepEqual(d1.tracks.map((x) => x.role), ['podcast']);
    note(t, `2c row button in the untrusted LMS iframe → the extension’s window: task #${d1.id} ${d1.state} lecture=${d1.lecture_id}`);
    // trusting the LMS from the window: kept as its origin only
    await win.check('#trust');
    await until(async () => (await trustedSites()).length > 0, 'the LMS trusted');
    assert.deepEqual(await trustedSites(), [site.canvasOrigin]);
    await win.close();
    await page.close();

    // Trusted now: the embed player's card opens in the LMS page; its
    // "确认发送" acts once it has been ready for 600 ms (and the card is
    // visible: IntersectionObserver v2), and no window opens.
    const E = site.addSession({ title: 'CSC1003/L04/01 Mon Oct 05 09:35:00 BST 2026' });
    const page2 = await openPage(`${site.canvasOrigin}/canvas/embed.html?id=${E.deliveryId}`);
    const frame2 = await frameOf(page2, '/Panopto/Pages/Embed.aspx');
    await frame2.waitForSelector('body[data-ready="1"]', { state: 'attached', timeout: 20000 });
    const ui2 = shadowUI(B.context, page2, frame2);
    await ui2.click('lectureflow-ui[data-kind="player"]', '.ip-btn.compact', { timeout: 20000 });
    await ui2.waitFor(CARD, '.lf-card[data-view="ready"]', { timeout: 30000 });
    await ui2.waitFor(CARD, '#primary', { pred: (x) => !x.disabled, timeout: 30000 });
    await sleep(700);
    await ui2.click(CARD, '#primary');
    await ui2.waitFor(CARD, '.lf-card[data-view="queued"]', { timeout: 20000 });
    assert.equal(B.context.pages().filter(isConfirm).length, 0, 'confirmed in the page: no window');
    const d2 = await waitTask(E.deliveryId);
    const fetchedE = site.state.requests.filter((r) => r.aria2 && r.path.startsWith(`/sessions/${E.ids.media}/`));
    assert.ok(fetchedE.length >= 1 && fetchedE.every(platformOriginReferer), `the Referer is the embed page’s origin, not the LMS page: ${referers(fetchedE)}`);
    assert.deepEqual(d2.tracks.map((x) => x.role), ['podcast']);
    note(t, `2c embed player in the trusted LMS iframe → the card in the page: task #${d2.id} ${d2.state} lecture=${d2.lecture_id}`);
    await page2.close();
  });

  test('2d a small embedded player in the trusted LMS: no room for the card, so the extension’s window (without the click-jacking note) sends it', async (t) => {
    // trusted by 2c through the window; on its own, trust it here
    if (!(await trustedSites()).includes(site.canvasOrigin)) {
      await (await B.worker()).evaluate(async (o) => {
        const cur = (await chrome.storage.local.get('trustedEmbedders')).trustedEmbedders || {};
        await chrome.storage.local.set({ trustedEmbedders: { ...cur, [o]: new Date().toISOString() } });
      }, site.canvasOrigin);
    }
    const S = site.addSession({ title: 'CSC1003/L04/02 Tue Oct 06 09:35:00 BST 2026' });
    const page = await openPage(`${site.canvasOrigin}/canvas/embed.html?id=${S.deliveryId}&small=1`);
    const frame = await frameOf(page, '/Panopto/Pages/Embed.aspx');
    await frame.waitForSelector('body[data-ready="1"]', { state: 'attached', timeout: 20000 });
    assert.deepEqual(await frame.evaluate(() => [innerWidth, innerHeight]), [380, 214], 'the player’s frame is small');
    const ui = shadowUI(B.context, page, frame);
    const [win] = await Promise.all([B.context.waitForEvent('page', { predicate: isConfirm, timeout: 20000 }), ui.click('lectureflow-ui[data-kind="player"]', '.ip-btn.compact', { timeout: 20000 })]);
    await win.waitForSelector('body[data-view="ready"]', { timeout: 30000 });
    assert.equal(await ui.read(CARD, '.lf-card'), null, 'no card cut off in the player');
    assert.equal(await win.getAttribute('body', 'data-notice'), 'plain');
    assert.equal(await win.isVisible('#embed-why'), false, 'no click-jacking note: the LMS is trusted');
    assert.equal(await win.isVisible('#trust-line'), false, 'no “以后直接在页面上确认”: it already is');
    assert.equal((await win.textContent('#embed-from')).trim(), `录播页面：${PANOPTO_HOST}:${site.pagesPort}`);
    assert.match(await win.textContent('#lecture'), /CSC1003\/L04\/02/);
    assert.equal(await win.inputValue('#course'), course.id);
    await clickArmed(win.locator('#primary'));
    await win.waitForSelector('body[data-view="queued"]', { timeout: 20000 });
    const d = await waitTask(S.deliveryId);
    assert.equal(d.course_id, course.id);
    assert.deepEqual(d.tracks.map((x) => x.role), ['podcast']);
    const fetched = site.state.requests.filter((r) => r.aria2 && r.path.startsWith(`/sessions/${S.ids.media}/`));
    assert.ok(fetched.length >= 1 && fetched.every(platformOriginReferer), `the Referer is the embed page’s origin: ${referers(fetched)}`);
    note(t, `2d small player (380×214) in the trusted LMS → the extension’s window, no note: task #${d.id} ${d.state} lecture=${d.lecture_id}`);
    await win.close();
    await page.close();
  });

  test('3a possibly restricted: nothing chosen, the reminder, then sent with overrideDisabled', async (t) => {
    const R = site.addSession({ title: 'CSC1003/L05/01 Tue Oct 06 09:35:00 BST 2026', downloads: false });
    const page = await openPage(site.viewerUrl(R), 'body[data-ready="1"]');
    const popup = await openPopup(page, 'blocked');
    assert.equal(await popup.getAttribute('body', 'data-reason'), 'disabled');
    assert.match(await popup.textContent('#reason'), /检测到该媒体流或受讲师或大学政策限制/);
    assert.equal(await popup.locator('#presets [aria-pressed="true"]').count(), 0, 'nothing chosen (SPEC-ext 默认不勾选)');
    assert.equal(await popup.isDisabled('#primary'), true);
    await popup.click('#presets [data-preset="main"]');
    await popup.click('#primary');
    await popup.waitForSelector('#override', { timeout: 10000 });
    assert.match(await popup.textContent('#override'), /该媒体流可能受政策限制/);
    assert.equal((await downloads()).some((d) => d.session_id === R.deliveryId), false, 'nothing sent before the reminder is confirmed');
    if (await popup.locator('#override-remember').count()) assert.equal(await popup.isChecked('#override-remember'), false, '“不再提醒” is off by default');
    await popup.click('#override-confirm');
    await popup.waitForSelector('body[data-view="queued"]', { timeout: 20000 });
    const d = await waitTask(R.deliveryId);
    assert.equal(d.override_disabled, true);
    assert.deepEqual(d.tracks.map((x) => x.role), ['podcast']);
    note(t, `3a restricted: task #${d.id} ${d.state} override_disabled=${d.override_disabled}`);
    await popup.close();
    await page.close();
  });

  test('3b DRM: an EME page and a SAMPLE-AES stream are refused in the popup; LectureFlow gets nothing', async (t) => {
    const before = (await downloads()).length;
    for (const [url, ready] of [
      [`${site.mediaOrigin}/g/eme.html`, null],
      [`${site.mediaOrigin}/g/drm.html`, 'body[data-ready="1"]'],
    ]) {
      const page = await openPage(url, ready);
      if (!ready) {
        await page.waitForFunction(() => document.title.startsWith('eme-'), null, { timeout: 15000 });
        assert.equal(await page.title(), 'eme-ready', 'Clear Key attached');
      }
      const popup = await openPopup(page, 'blocked');
      assert.equal(await popup.getAttribute('body', 'data-reason'), 'protected');
      assert.match(await popup.textContent('#reason'), /数字版权保护（DRM），LectureFlow 不处理受保护的内容/);
      assert.equal(await popup.isDisabled('#primary'), true);
      assert.equal(await popup.locator('#presets button:not([disabled])').count(), 0);
      note(t, `3b ${new URL(url).pathname}: popup blocked (protected)`);
      await popup.close();
      await page.close();
    }
    assert.equal((await downloads()).length, before, 'no task');
  });

  test('3c LectureFlow’s side of DRM: a SAMPLE-AES track sent anyway is accepted, then fails as downloads.drm_protected', async (t) => {
    const sessionId = 'lf-native-e2e-drm-direct';
    const { replies } = await exchange(P.bat, [
      HELLO,
      {
        type: 'enqueue',
        profile: { id: 'generic', version: '', reviewed: false },
        platform: 'generic',
        sessionId,
        lecture: { title: 'DRM stream (test)', recordedAt: '', durationSec: 0 },
        courseId: course.id,
        tracks: [{ url: `${site.mediaOrigin}/g/drm/master.m3u8`, kind: 'video', format: 'hls', role: '', label: '', sizeBytes: -1 }],
        referer: `${site.mediaOrigin}/g/drm.html`,
        cookies: [],
        maxConnections: 0,
        overrideDisabled: false,
      },
    ]);
    assert.equal(replies[0].ok, true, JSON.stringify(replies[0]));
    const enq = replies[1];
    if (enq.ok) {
      const d = await waitTask(sessionId, (x) => x.state === 'failed');
      assert.equal(d.error && d.error.code, 'downloads.drm_protected');
      note(t, `3c enqueue of a SAMPLE-AES track: ok (taskId ${enq.taskId}); the task failed with ${d.error.code}; enqueue.protected was not answered (bridge.Track has no protected flag)`);
    } else {
      assert.equal(enq.error.code, 'enqueue.protected');
      note(t, `3c enqueue of a SAMPLE-AES track: ${enq.error.code}`);
    }
  });

  test('4a the same session again: 已导入, and “在 LectureFlow 中打开” reaches the app', async (t) => {
    const page = await openPage(site.viewerUrl(sent.main), 'body[data-ready="1"]');
    const popup = await openPopup(page, 'blocked');
    assert.equal(await popup.getAttribute('body', 'data-reason'), 'imported');
    assert.match(await popup.textContent('#reason'), /已经导入/);
    assert.equal(await popup.isDisabled('#primary'), true);
    const d = (await downloads()).find((x) => x.session_id === sent.main.deliveryId);
    await popup.click('#open');
    await popup.waitForSelector('#msg', { timeout: 10000 });
    await until(() => appStdout().includes(`LECTUREFLOW_OPEN #/lecture/${d.lecture_id}`), 'the open line on the app’s stdout');
    note(t, `4a popup blocked (imported); open → stdout "LECTUREFLOW_OPEN #/lecture/${d.lecture_id}"`);
    await popup.close();
    await page.close();
  });

  test('4b two popups on one session: the second send gets queue.duplicate and shows the task; one task only', async (t) => {
    const D = site.addSession({ title: 'CSC1003/L06/01 Wed Oct 07 09:35:00 BST 2026' });
    const page = await openPage(site.viewerUrl(D), 'body[data-ready="1"]');
    const a = await openPopup(page, 'ready');
    const b = await openPopup(page, 'ready');
    await a.click('#primary');
    await a.waitForSelector('body[data-view="queued"]', { timeout: 20000 });
    await b.click('#primary');
    await b.waitForFunction(() => ['queued', 'blocked'].includes(document.body.dataset.view), null, { timeout: 20000 });
    const viewB = await b.evaluate(() => `${document.body.dataset.view}/${document.body.dataset.reason || ''}`);
    assert.equal(await b.locator('#msg.note-bad').count(), 0, 'not an error');
    const d = await waitTask(D.deliveryId);
    assert.equal((await downloads()).filter((x) => x.session_id === D.deliveryId).length, 1);
    note(t, `4b second popup after queue.duplicate: view ${viewB}; one task #${d.id} ${d.state}`);
    await a.close();
    await b.close();
    await page.close();
  });

  test('4c LectureFlow answers queue.duplicate to an enqueue of an imported session', async (t) => {
    const d = (await downloads()).find((x) => x.session_id === sent.main.deliveryId);
    const { replies } = await exchange(P.bat, [
      HELLO,
      { type: 'check', platform: 'panopto', sessionIds: [sent.main.deliveryId] },
      {
        type: 'enqueue',
        profile: { id: 'panopto', version: '1.1.1', reviewed: true },
        platform: 'panopto',
        sessionId: sent.main.deliveryId,
        lecture: { title: sent.main.title, recordedAt: '', durationSec: 0 },
        courseId: course.id,
        tracks: [{ url: `${site.mediaOrigin}/sessions/${sent.main.ids.media}/${sent.main.ids.media}-${sent.main.ids.stream}.mp4`, kind: 'video', format: 'file', role: 'podcast', label: '', sizeBytes: -1 }],
        referer: site.viewerUrl(sent.main),
        cookies: [],
        maxConnections: 4,
        overrideDisabled: false,
      },
    ]);
    assert.deepEqual(replies[1].items, [{ sessionId: sent.main.deliveryId, state: 'imported', taskId: d.id, lectureId: d.lecture_id, courseId: course.id, matchedBy: 'session' }]);
    assert.equal(replies[2].ok, false);
    assert.equal(replies[2].error.code, 'queue.duplicate');
    assert.deepEqual(replies[2].error.params, { state: 'imported', taskId: d.id, lectureId: d.lecture_id });
    note(t, `4c check → imported (session); enqueue → ${replies[2].error.code} ${JSON.stringify(replies[2].error.params)}`);
  });

  test('4d restart: the page’s own check does not start LectureFlow; “未连接”; the host starts it; needs_refresh; “续传” with new cookies completes', async (t) => {
    site.stall();
    const page = await openPage(site.genericUrl, 'video');
    const popup = await openPopup(page, 'ready');
    assert.match(await popup.textContent('#lecture'), /在页面上找到/);
    await recordCookieCalls();
    await popup.click('#primary');
    await popup.waitForSelector('body[data-view="queued"]', { timeout: 20000 });
    const calls = await cookieCalls();
    const trackUrl = calls.length ? calls[0].url : '';
    assert.ok(calls.length >= 1 && calls.every((c) => c.url === trackUrl && new URL(c.url).pathname === '/g/lecture.mp4'), JSON.stringify(calls.map((c) => Object.keys(c))));
    const generic = (await downloads()).find((x) => x.platform === 'generic' && x.state !== 'failed');
    assert.ok(generic, 'a generic task');
    const sid = generic.session_id;
    sent.genericSession = sid;
    // aria2c got the cookie and the signed query, and is now held by the server
    await until(() => site.state.requests.some((r) => r.path === '/g/lecture.mp4' && r.aria2 && r.stalled && r.cookieOk && r.signatureOk), 'aria2c fetching with the cookie and the signature', 30000);
    await waitTask(sid, (x) => x.state === 'downloading', 20000);
    await popup.close();

    const startsBefore = I.launches(P).length;
    const stopped = await I.stopInstance(P);
    assert.match(stopped, /requested=true/);
    assert.equal(I.instanceUrl(P), '');
    note(t, `4d stopped LectureFlow while fetching: ${stopped}`);

    // 1. a page the user merely opens checks its rows passively: LectureFlow must not start
    const NOT_STARTED = '(not_running): the extension asked not to start it';
    const refusedBefore = I.hostStderrCount(P, NOT_STARTED);
    const list = await openPage(site.listUrl(), 'body[data-ready="1"]');
    const lui = shadowUI(B.context, list);
    await lui.waitFor(rowHost(sent.list[0]), '.ip-btn', { timeout: 20000 });
    await until(() => I.hostStderrCount(P, NOT_STARTED) > refusedBefore, 'the host refusing to start LectureFlow for a passive check');
    await sleep(1500);
    assert.equal(I.launches(P).length, startsBefore, 'not started');
    assert.equal(I.instanceUrl(P), '');
    assert.deepEqual(await lui.readAll(rowHost(sent.list[0]), '[data-row-state]'), [], 'no row states');
    assert.match((await lui.read(rowHost(sent.list[0]), '.lf-root')).text, /发送至 LectureFlow/);
    note(t, '4d passive row check with LectureFlow stopped: host stderr "(not_running): the extension asked not to start it"; not started; rows without state, buttons there');
    await list.close();

    // 2. the user opens the popup while LectureFlow cannot be started: 未连接
    I.removeLaunch(P);
    const p2 = await openPopup(page, 'disconnected');
    assert.equal(await p2.getAttribute('#conn', 'data-nm-state'), 'unavailable');
    assert.match(await p2.textContent('#state-title'), /LectureFlow 未连接/);
    assert.equal(I.launches(P).length, startsBefore);
    note(t, `4d popup without launch.json: "${(await p2.textContent('#state-title')).trim()}"`);

    // 3. "重新检测": the host starts LectureFlow (launch.json), the task needs new credentials
    I.writeLaunch(P);
    site.release();
    await p2.click('#recheck');
    await p2.waitForSelector('body[data-view="refresh"]', { timeout: 60000 });
    const starts = I.launches(P);
    assert.equal(starts.length, startsBefore + 1);
    assert.match(starts.at(-1), /by launch\.json/);
    assert.ok(I.instanceUrl(P));
    const nr = await waitTask(sid, (x) => x.state === 'needs_refresh', 10000);
    assert.equal(nr.error && nr.error.code, 'downloads.restarted');
    assert.equal(await p2.getAttribute('#conn', 'data-nm-state'), 'connected');
    assert.match(await p2.textContent('#primary'), /续传/);
    note(t, `4d recheck → the host started LectureFlow (${starts.at(-1).trim()}); task #${nr.id} ${nr.state} ${nr.error ? nr.error.code : ''}`);

    // 4. "续传": the same page's tracks and cookies, read again
    await recordCookieCalls();
    const fetchesBefore = site.state.requests.filter((r) => r.path === '/g/lecture.mp4' && r.aria2).length;
    await p2.click('#primary');
    await p2.waitForSelector('body[data-view="queued"]', { timeout: 20000 });
    const d = await waitTask(sid);
    const after = site.state.requests.filter((r) => r.path === '/g/lecture.mp4' && r.aria2).slice(fetchesBefore);
    assert.ok(after.length >= 1 && after.every((r) => r.cookieOk && r.signatureOk), 'resumed with the new cookie and the signed query');
    assert.ok((await cookieCalls()).length >= 1, 'the cookies were read again on “续传”');
    // the generic page and its MP4 share an origin: the whole page address is the Referer, before and after the restart
    const genericFetches = site.state.requests.filter((r) => r.path === '/g/lecture.mp4' && r.aria2);
    assert.ok(genericFetches.every((r) => r.refererScheme === 'http:' && r.referer === `127.0.0.1:${site.mediaPort}` && r.refererPath === '/g/lecture.html' && !r.refererQuery), `same origin: the page as Referer: ${referers(genericFetches)}`);
    const lec = await api('GET', `/api/lectures/${d.lecture_id}`);
    const f = lectureFiles(lec);
    assert.deepEqual(streamKinds(ffprobe, f.media[0]).sort(), ['audio', 'video']);
    note(t, `4d 续传 → task #${d.id} ${d.state} lecture=${d.lecture_id} (aria2c requests after the restart: ${after.length}, all with cookie and signature)`);
    await p2.close();
    await page.close();
  });

  test('5 nothing on disk: no cookie, signed query, viewer data or media path in the data directory, logs, temporary files', async (t) => {
    // The extension's storage: storage.local (on disk) holds none of these;
    // storage.session (memory only, gone with the browser) may hold request
    // URLs of open tabs (the webRequest records), never a cookie or the
    // viewer's data.
    const secrets = site.secrets();
    const [local, session] = await (await B.worker()).evaluate(async () => [JSON.stringify(await chrome.storage.local.get(null)), JSON.stringify(await chrome.storage.session.get(null))]);
    const named = { 'track cookie': site.creds.cookie, 'platform sign-in cookie': site.creds.panoptoCookie, 'viewer e-mail': VIEWER_EMAIL, 'viewer name': VIEWER_NAME };
    assert.deepEqual(secrets.filter((v) => local.includes(v)).length, 0, 'nothing of these in storage.local');
    assert.deepEqual(Object.keys(named).filter((k) => session.includes(named[k])), [], 'no cookie or viewer data in storage.session');
    note(t, `5 extension storage: storage.local ${local.length} bytes, none of the ${secrets.length} values; storage.session ${session.length} bytes, no cookie or viewer data`);
    await B.context.close();
    B = null;
    const stopped = await I.stopInstance(P);
    note(t, `5 LectureFlow stopped: ${stopped}`);
    const needles = {
      'track cookie': site.creds.cookie,
      'signed query (Signature)': site.creds.signature,
      'platform sign-in cookie': site.creds.panoptoCookie,
      'viewer e-mail': VIEWER_EMAIL,
      'viewer name': VIEWER_NAME,
    };
    for (const v of secrets.slice(5)) needles[`media path id ${v.slice(0, 8)}…${v.slice(-4)}`] = v;
    const roots = [P.data, P.logs, P.tmp, P.state, P.host];
    const r = scan(roots, needles);
    note(t, `5 scanned ${r.files} files (${r.bytes} bytes) under data, logs, temp, state, host dir for ${Object.keys(needles).length} values (UTF-8 and UTF-16LE): ${r.hits.length} hits`);
    assert.deepEqual(r.unreadable, []);
    assert.deepEqual(r.hits, []);
    assert.ok(r.files > 10, 'the scan saw the data directory');
    // the scan does find what is there
    const canary = path.join(P.root, 'canary');
    fs.mkdirSync(canary, { recursive: true });
    fs.writeFileSync(path.join(canary, 'a.bin'), Buffer.from(`x${site.creds.cookie}x`, 'utf16le'));
    assert.deepEqual(scan([canary], { c: site.creds.cookie }).hits.map((h) => h.encoding), ['utf16le']);
    fs.rmSync(canary, { recursive: true, force: true });
  });
}
