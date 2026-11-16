// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// End to end, the popup (SPEC-ext 界面设计, 用户流程): its five states, one
// click and one confirmation, "已导入", the three choices, the reminder
// before sending a possibly restricted recording, needs_refresh → "续传",
// cookies read only on the click and only for the tracks' host, and the
// Panopto page-response observation keeping the viewer's e-mail and name
// out of everything the extension keeps or sends. test/e2e/harness.mjs
// has the browser, the local site and the fake LectureFlow.
//
//   npm run test:e2e
//   LF_EXT_SCREENSHOTS=1 npm run test:e2e   (also writes docs/screenshots/)

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import * as H from './harness.mjs';
import { PANOPTO } from './server.mjs';

const { COOKIE_TRACK, COOKIE_OTHER, COURSES } = H;
let B;
let server;
let context;
let worker;
let extensionId;

before(async () => {
  B = await H.startBrowser();
  ({ server, context, worker, extensionId } = B);
  const ids = await worker.evaluate(async () => (await chrome.scripting.getRegisteredContentScripts()).map((s) => `${s.id}:${s.world || 'ISOLATED'}:${s.matches.join(' ')}`));
  assert.ok(ids.some((s) => s.startsWith('lf-observe-main:MAIN:') && s.includes('*.cloud.panopto.eu')), ids.join('\n'));
  assert.ok(!ids.some((s) => s.startsWith('lf-observe-main:') && s.includes('lf-e2e.test')), 'the page wrapper only where a profile observes');
  assert.ok(ids.some((s) => s.startsWith('lf-inpage:ISOLATED:') && s.includes('*.cloud.panopto.eu')), ids.join('\n'));
  assert.ok(!ids.some((s) => s.startsWith('lf-inpage:') && s.includes('lf-e2e.test')), 'no in-page buttons on generic sites');
});

// a test that failed leaves nothing behind for the next one (harness.mjs)
H.isolateTests(() => B);

after(async () => {
  if (B) await B.close();
});

// ------------------------------------------------------------- helpers

const fakeLectureFlow = (opts) => H.fakeLectureFlow(worker, opts);
const received = () => H.received(worker);
const cookieCalls = () => H.cookieCalls(worker);
const resetCookieCalls = () => H.resetCookieCalls(worker);
const storageDump = () => H.storageDump(worker);
const tabIdOf = (page) => H.tabIdOf(worker, page);
const shoot = (page, name) => H.shoot(page, `popup-${name}`);

/**
 * The getAll calls of one send (src/lib/partition.js): each track URL
 * unpartitioned, then the same URLs in the tab's own partition (the
 * frame's key from getPartitionKey, or every partition filtered to the
 * tab's site), and nothing else; the browser API saw the same calls.
 */
function assertCookieReads(calls, urls) {
  assert.deepEqual(calls.module.filter((c) => !('partitionKey' in c)), urls.map((url) => ({ url })));
  const parted = calls.module.filter((c) => 'partitionKey' in c);
  assert.deepEqual([...new Set(parted.map((c) => c.url))], urls);
  for (const c of parted) {
    assert.deepEqual(Object.keys(c).sort(), ['partitionKey', 'url']);
    const site = c.partitionKey.topLevelSite;
    assert.ok(site === undefined || /^http:\/\/(?:[a-z0-9-]+\.)*lf-e2e\.test$/.test(site), `the test site's partition only: ${site}`);
  }
  assert.deepEqual(calls.raw, calls.module, 'the browser API itself saw the same calls');
}

async function openPage(url, ready) {
  const page = await context.newPage();
  await page.goto(url);
  if (ready) await page.waitForSelector(ready, { timeout: 10000 });
  await page.waitForTimeout(400);
  return page;
}

async function openPopup(tabId, view) {
  const popup = await context.newPage();
  await popup.setViewportSize({ width: 360, height: 120 });
  await popup.goto(`chrome-extension://${extensionId}/popup/popup.html?tabId=${tabId}`);
  if (view) await popup.waitForSelector(`body[data-view="${view}"]`, { timeout: 15000 });
  return popup;
}

const genericUrl = () => `http://course.lf-e2e.test:${server.port}/mp4.html`;
const panoptoUrl = (via = 'xhr') => `http://${PANOPTO.host}:${server.port}/Panopto/Pages/Viewer.aspx?id=${PANOPTO.deliveryId}&via=${via}`;

// --------------------------------------------------------------- tests

test('no tracks: one sentence and "重新探测"', async () => {
  await fakeLectureFlow();
  const page = await openPage(`${server.origin}/empty.html`);
  const popup = await openPopup(await tabIdOf(page), 'none');
  assert.match(await popup.textContent('#state-title'), /没探测到轨道/);
  assert.match(await popup.textContent('#empty'), /打开一节录播并开始播放后再点/);
  assert.equal(await popup.locator('#primary').count(), 0);
  await shoot(popup, '1-none');
  await popup.close();
  await page.close();
});

test('LectureFlow not connected: install or start it, "重新检测" finds it', async () => {
  await fakeLectureFlow({ mode: 'absent' });
  const page = await openPage(genericUrl());
  const popup = await openPopup(await tabIdOf(page), 'disconnected');
  assert.match(await popup.textContent('#state-title'), /LectureFlow 未连接/);
  assert.match(await popup.textContent('#disc-text'), /安装/);
  // installed but the browser's registration broken: LectureFlow's diagnostics page (T26 G8)
  assert.equal((await popup.textContent('#disc-register')).trim(), '已经安装并打开了？在 LectureFlow 的“诊断”页找到“浏览器扩展连接”，点“重新登记”，再回到这里点“重新检测”。');
  assert.match(await popup.textContent('#count'), /探测到 3 条轨道/);
  assert.equal(await popup.getAttribute('#conn', 'data-nm-state'), 'unavailable');
  await shoot(popup, '2-disconnected');
  // LectureFlow comes up; "重新检测" connects without reopening the popup.
  await worker.evaluate(() => {
    globalThis.__fakeLF.mode = 'ok';
  });
  await popup.click('#recheck');
  await popup.waitForSelector('body[data-view="ready"]', { timeout: 10000 });
  assert.equal(await popup.getAttribute('#conn', 'data-nm-state'), 'connected');
  await popup.close();
  await page.close();
});

test('LectureFlow installed but not open: opening the popup never starts it (an empty page, a recording); "打开 LectureFlow" does (T26 G1)', async () => {
  // do-not-handle sites: blocklist.test.mjs
  await fakeLectureFlow({ mode: 'stopped' });
  const empty = await openPage(`${server.origin}/empty.html`);
  const blank = await openPopup(await tabIdOf(empty), 'none');
  await blank.waitForFunction(() => document.getElementById('conn').dataset.nmState !== 'checking', null, { timeout: 15000 });
  assert.equal(await blank.getAttribute('#conn', 'data-nm-state'), 'unavailable');
  await blank.waitForTimeout(300);
  assert.equal(await H.launches(worker), 0, 'an empty page: not started');
  await blank.close();
  await empty.close();
  const page = await openPage(genericUrl());
  const popup = await openPopup(await tabIdOf(page), 'disconnected');
  assert.match(await popup.textContent('#disc-text'), /已经安装，但还没有打开/);
  assert.equal(await popup.locator('#get-app').count(), 0, 'installed: nothing to get');
  assert.match(await popup.textContent('#launch'), /打开 LectureFlow/);
  assert.equal(await H.launches(worker), 0, 'not started by opening the popup');
  // "重新检测" beside it only looks again (SEC42 低-2)
  const before = (await H.hellos(worker)).length;
  await popup.click('#recheck');
  await popup.waitForFunction(() => document.body.dataset.view === 'disconnected' && document.getElementById('conn').dataset.nmState === 'unavailable', null, { timeout: 10000 });
  for (let i = 0; i < 50 && (await H.hellos(worker)).length === before; i++) await popup.waitForTimeout(100);
  assert.ok((await H.hellos(worker)).length > before, 'it asked again');
  assert.equal(await H.launches(worker), 0, 'not started by "重新检测"');
  const passive = await H.hellos(worker);
  assert.ok(passive.length >= 1 && passive.every((m) => m.noLaunch === true), 'every hello the popup sent on its own, or for "重新检测", says noLaunch');
  await shoot(popup, '2-not-open');
  await popup.click('#launch');
  await popup.waitForSelector('body[data-view="ready"]', { timeout: 10000 });
  assert.equal(await H.launches(worker), 1, 'started by the click');
  const active = (await H.hellos(worker)).slice(passive.length);
  assert.equal(active.length, 1);
  assert.equal('noLaunch' in active[0], false);
  await popup.close();
  await page.close();
});

test('the popup’s check of the lecture never starts LectureFlow, also after it quit (SEC42 低-1)', async () => {
  await fakeLectureFlow();
  const page = await openPage(genericUrl());
  const tabId = await tabIdOf(page);
  const popup = await openPopup(tabId, 'ready');
  // LectureFlow quits; the next check needs a new connection to the host
  await worker.evaluate(() => {
    globalThis.__fakeLF.mode = 'stopped';
    globalThis.__lectureflowTest.resetNative();
  });
  const reply = await popup.evaluate(
    (id) =>
      new Promise((resolve) => {
        const port = chrome.runtime.connect({ name: 'popup' });
        port.onMessage.addListener((m) => m && m.id === 'sec42' && resolve(m));
        port.postMessage({ id: 'sec42', cmd: 'check', tabId: id });
      }),
    tabId,
  );
  assert.equal(reply.ok, false);
  assert.equal(reply.error.code, 'app.not_running');
  assert.equal(await H.launches(worker), 0, 'not started by a check');
  assert.ok((await H.hellos(worker)).every((m) => m.noLaunch === true));
  await popup.close();
  await page.close();
});

test('no course in LectureFlow yet: where to make one; "重新检测" shows it without reopening the popup (T26 G9)', async () => {
  await fakeLectureFlow({ courses: [] });
  const page = await openPage(genericUrl());
  const popup = await openPopup(await tabIdOf(page), 'ready');
  assert.match(await popup.textContent('#no-course'), /还没有课程.*点“添加课程”新建一门，然后点“重新检测”。/);
  assert.equal(await popup.isDisabled('#primary'), true);
  await shoot(popup, '3-no-course');
  await worker.evaluate((c) => {
    globalThis.__fakeLF.courses = c;
  }, COURSES);
  await popup.click('#course-recheck');
  await popup.waitForSelector('#course', { timeout: 10000 });
  assert.equal(await popup.isDisabled('#primary'), false);
  await popup.close();
  await page.close();
});

test('ready: "主要音视频" is chosen; one click sends; cookies read only then, only for the tracks’ host', async () => {
  await fakeLectureFlow();
  await worker.evaluate((id) => chrome.storage.local.set({ lastCourseId: id }), COURSES[1].id);
  await resetCookieCalls();
  const page = await openPage(genericUrl());
  const tabId = await tabIdOf(page);
  const popup = await openPopup(tabId, 'ready');

  // Generic detection: the page's own title (T048), read after the blocklist checks.
  assert.match(await popup.textContent('#lecture'), /Plain files/, 'the page title (T048)');
  assert.match(await popup.textContent('#lecture'), /在页面上找到/, 'not the developer word "通用探测" (T26 G11), nor the footer’s “自动探测” (EU-04)');
  // connected, said in words (T26 J1); what the report is, before it is made (T26 G14)
  assert.equal((await popup.textContent('#conn-chip')).trim(), '已连接 · 3.2.0');
  // what the redaction does, not a promise it cannot keep (SEC42 低-5)
  const exportHint = await popup.getAttribute('#export', 'title');
  assert.match(exportHint, /给开发者的文件.*不会自动发送。文件已脱敏：不含 Cookie；网址保留网站名和路径（路径里较长的编号换成同形的假值）.*分享前请先看一遍。$/);
  assert.ok(!exportHint.includes('已去掉 Cookie 和个人信息'), exportHint);
  // Three choices instead of ticking: main (audio + the first picture here),
  // no 双机位 with one picture; the course is the one used last.
  assert.equal(await popup.getAttribute('#presets [data-preset="main"]', 'aria-pressed'), 'true');
  assert.equal(await popup.isDisabled('#presets [data-preset="dual"]'), true);
  assert.equal(await popup.isDisabled('#presets [data-preset="audio"]'), false);
  // the choice that cannot be made looks it, and says why on the card (T26 G4)
  const look = (sel) => popup.$eval(sel, (b) => ({ color: getComputedStyle(b).color, bg: getComputedStyle(b).backgroundColor, cursor: getComputedStyle(b).cursor }));
  const dualLook = await look('#presets [data-preset="dual"]');
  const audioLook = await look('#presets [data-preset="audio"]');
  assert.equal(dualLook.cursor, 'not-allowed');
  assert.equal(audioLook.cursor, 'pointer');
  assert.notEqual(dualLook.color, audioLook.color);
  assert.notEqual(dualLook.bg, audioLook.bg);
  assert.equal((await popup.textContent('#presets-why')).trim(), '“双机位”不可选：只探测到一路画面。');
  assert.match(await popup.textContent('#chosen'), /视频 \+ 音频/);
  assert.equal(await popup.locator('#tracks').count(), 0, 'the full list only on request');
  assert.equal(await popup.inputValue('#course'), COURSES[1].id);
  assert.match(await popup.textContent('#course'), /（上次）/);
  assert.match(await popup.textContent('#data-note'), /Cookie/);
  assert.equal(await popup.locator('.btn-primary').count(), 1, 'one solid button');
  assert.match(await popup.textContent('#primary'), /发送至 LectureFlow/);
  await shoot(popup, '3-ready');
  await popup.click('#advanced');
  const ticked = await popup.$$eval('#tracks input[type=checkbox]', (xs) => xs.filter((x) => x.checked).map((x) => x.closest('.track').dataset.kind));
  assert.deepEqual(ticked, ['video', 'audio']);
  await shoot(popup, '3-ready-tracks');

  // Nothing read cookies yet: not the detection, not the popup, not the check.
  assert.deepEqual(await cookieCalls(), { module: [], raw: [] });
  assert.deepEqual((await received()).map((m) => m.type), ['check']);

  await popup.click('#primary');
  await popup.waitForSelector('body[data-view="queued"]', { timeout: 10000 });
  const msgs = await received();
  assert.deepEqual(msgs.map((m) => m.type), ['check', 'check', 'enqueue'], 'check again, then one enqueue');
  const enq = msgs[2];
  assert.equal(enq.platform, 'generic');
  assert.deepEqual(enq.profile, { id: 'generic', version: '', reviewed: false });
  assert.match(enq.sessionId, /^[0-9a-f]{32}$/);
  assert.equal(enq.lecture.title, 'Plain files');
  assert.equal(enq.courseId, COURSES[1].id);
  assert.equal(enq.referer, genericUrl());
  assert.equal(enq.overrideDisabled, false);
  assert.equal(enq.maxConnections, 0);
  const videoUrl = `http://course.lf-e2e.test:${server.port}/media/lecture.mp4`;
  const audioUrl = `http://course.lf-e2e.test:${server.port}/media/talk.m4a`;
  assert.deepEqual(enq.tracks.map((t) => [t.url, t.kind, t.format, t.sizeBytes]), [[videoUrl, 'video', 'file', 123456], [audioUrl, 'audio', 'file', 54321]]);
  // Only the cookie of the tracks' host, and getAll asked for those URLs only.
  assert.deepEqual(enq.cookies.map((c) => [c.name, c.value, c.domain]), [['lf_track', COOKIE_TRACK, 'course.lf-e2e.test']]);
  assert.ok(!JSON.stringify(enq).includes(COOKIE_OTHER));
  assertCookieReads(await cookieCalls(), [videoUrl, audioUrl]);

  // Queue position, then progress from status pushes.
  assert.match(await popup.textContent('#task-state'), /排队中/);
  assert.match(await popup.textContent('#position'), /第 2 位/);
  await worker.evaluate((id) => globalThis.__fakeLF.push({ taskId: id, state: 'downloading', bytesDone: 45 * 1024 * 1024, bytesTotal: 100 * 1024 * 1024 }), 41);
  await popup.waitForSelector('#task-state[data-state="downloading"]', { timeout: 5000 });
  assert.match(await popup.textContent('#task-state'), /刮削中/);
  assert.match(await popup.textContent('#progress'), /45%/);
  await shoot(popup, '5-queued');

  // The course is remembered; no cookie value is kept anywhere.
  const dump = await storageDump();
  assert.ok(dump.includes(COURSES[1].id));
  assert.ok(!dump.includes(COOKIE_TRACK) && !dump.includes(COOKIE_OTHER), 'no cookie in storage');
  const local = await worker.evaluate(async () => JSON.stringify(await chrome.storage.local.get(null)));
  assert.ok(!local.includes('lecture.mp4') && !local.includes('lf-e2e.test'), 'no track or page address in storage.local');

  // "在 LectureFlow 中打开" asks for the task.
  await popup.click('#open');
  await popup.waitForSelector('#msg', { timeout: 5000 });
  const open = (await received()).filter((m) => m.type === 'open');
  assert.deepEqual(open.map((m) => m.taskId), [41]);
  // done: the title says what came of it (T26 J2)
  await worker.evaluate((id) => globalThis.__fakeLF.push({ taskId: id, state: 'done', lectureId: '01JE2ELECTURE0000000000041', bytesDone: 100, bytesTotal: 100 }), 41);
  await popup.waitForSelector('#task-state[data-state="done"]', { timeout: 5000 });
  assert.equal((await popup.textContent('#state-title')).trim(), '刮削完成');
  await popup.close();
  await page.close();
});

test('cannot send: already imported, with "在 LectureFlow 中打开"', async () => {
  await fakeLectureFlow({ checkItem: { state: 'imported', taskId: 9, lectureId: '01JE2ELECTURE0000000000009' } });
  await resetCookieCalls();
  const page = await openPage(genericUrl());
  const popup = await openPopup(await tabIdOf(page), 'blocked');
  assert.equal(await popup.getAttribute('body', 'data-reason'), 'imported');
  assert.match(await popup.textContent('#reason'), /已经导入/);
  assert.equal(await popup.isDisabled('#primary'), true);
  await shoot(popup, '4-blocked-imported');
  await popup.click('#open');
  await popup.waitForSelector('#msg', { timeout: 5000 });
  const msgs = await received();
  assert.deepEqual(msgs.map((m) => m.type), ['check', 'open']);
  assert.equal(msgs[1].lectureId, '01JE2ELECTURE0000000000009');
  assert.deepEqual(await cookieCalls(), { module: [], raw: [] }, 'nothing sent, no cookie read');
  await popup.close();
  await page.close();
});

test('Panopto, possibly restricted: nothing chosen, the user’s notice, one reminder before sending; the viewer data never leaves the page', async () => {
  server.panopto.downloadUrl = null;
  await fakeLectureFlow();
  await resetCookieCalls();
  const page = await openPage(panoptoUrl('xhr'), 'body[data-ready="1"]');
  // the page still got the whole answer (the wrapper passes it through)
  assert.equal(await page.evaluate(() => window.__pageSawUser), PANOPTO.userName);
  const tabId = await tabIdOf(page);
  const popup = await openPopup(tabId, 'blocked');
  assert.equal(await popup.getAttribute('body', 'data-reason'), 'disabled');
  assert.match(await popup.textContent('#lecture'), /CSC1003\/L01\/01/);
  // the platform's name; the profile and its version only in the tooltip (T26 G11)
  assert.match(await popup.textContent('#lecture .lec-meta'), /Panopto$/);
  assert.ok(!(await popup.textContent('#lecture')).includes('对策文件'));
  assert.equal(await popup.getAttribute('#lecture .lec-meta', 'title'), 'Panopto 对策文件 1.1.2');
  assert.match(await popup.textContent('#lecture'), /可能受政策限制/);
  // the user's text, verbatim
  assert.equal(
    (await popup.textContent('#reason')).trim(),
    '检测到该媒体流或受讲师或大学政策限制。LectureFlow 仅将您有权访问的媒体流拉取至本地用于个人无障碍学习（转录/翻译）。请确保您遵守学校规定。发送前会再提醒一次。',
  );
  // nothing chosen at first (SPEC-ext 默认不勾选)
  assert.equal(await popup.locator('#presets [aria-pressed="true"]').count(), 0);
  assert.equal(await popup.isDisabled('#primary'), true);
  await popup.click('#advanced');
  const rows = await popup.$$eval('#tracks .track', (xs) => xs.map((x) => `${x.dataset.role}:${x.dataset.checked}`));
  assert.deepEqual(rows, ['podcast:false', 'primary-audio:false', 'screen:false']);
  assert.match(await popup.textContent('#tracks .track[data-role="podcast"]'), /完整录像（MP4）/);
  await popup.click('#advanced');
  await shoot(popup, '4-blocked-restricted');

  // The user picks "主要音视频"; sending reminds once.
  await popup.click('#presets [data-preset="main"]');
  assert.match(await popup.textContent('#chosen'), /完整录像（MP4）/);
  await popup.click('#primary');
  await popup.waitForSelector('#override');
  assert.match(await popup.textContent('#override'), /该媒体流可能受政策限制/);
  assert.match(await popup.textContent('#override'), /个人无障碍学习（转录\/翻译）/);
  assert.match(await popup.textContent('#override'), /请确保您遵守学校规定/);
  assert.match(await popup.textContent('#override-confirm'), /仍然发送/);
  assert.equal((await received()).filter((m) => m.type === 'enqueue').length, 0, 'not before the reminder');
  await shoot(popup, '4-restricted-reminder');
  await popup.click('#override-confirm');
  await popup.waitForSelector('body[data-view="queued"]', { timeout: 10000 });

  const enq = (await received()).find((m) => m.type === 'enqueue');
  assert.equal(enq.overrideDisabled, true);
  assert.equal(enq.platform, 'panopto');
  assert.equal(enq.sessionId, PANOPTO.deliveryId);
  assert.deepEqual(enq.profile, { id: 'panopto', version: '1.1.2', reviewed: true });
  assert.equal(enq.lecture.title, PANOPTO.title);
  assert.equal(enq.lecture.recordedAt, '2026-10-21T09:35', 'the title’s 09:35 BST as written, whatever this computer’s zone (tz-fix)');
  assert.equal(enq.lecture.durationSec, 3300);
  assert.equal(enq.maxConnections, 4);
  assert.deepEqual(enq.tracks.map((t) => [t.role, t.kind, t.format, t.label]), [['podcast', 'video', 'file', '完整录像（MP4）']]);
  assert.equal(enq.tracks[0].url, `http://${PANOPTO.host}:${server.port}/sessions/${PANOPTO.session}/${PANOPTO.primary}.mp4`);
  // Panopto's media needs no cookie (cookieUrls: []): none read, none sent.
  assert.deepEqual(enq.cookies, []);
  assert.deepEqual(await cookieCalls(), { module: [], raw: [] });
  // The extension never requested the MP4 or DeliveryInfo itself.
  assert.ok(!server.requests.some((r) => r.path.endsWith('.mp4') && r.host === PANOPTO.host));
  assert.equal(server.requests.filter((r) => r.path.endsWith('DeliveryInfo.aspx') && r.deliveryId === PANOPTO.deliveryId).length, 1);

  // The viewer's e-mail and name (other fields of the same response) are
  // nowhere: not in the messages to LectureFlow, the storage, the detection
  // kept in memory, the popup, the report.
  const secrets = [PANOPTO.userEmail, PANOPTO.userName];
  const everything = [
    JSON.stringify(await worker.evaluate(() => globalThis.__fakeLF.received)),
    await storageDump(),
    await worker.evaluate((id) => JSON.stringify(globalThis.__lectureflowTest.lastResult(id)), tabId),
    await popup.content(),
    JSON.stringify(await worker.evaluate((id) => globalThis.__lectureflowTest.reportFor(id), tabId)),
  ];
  for (const s of secrets) for (const text of everything) assert.ok(!text.includes(s), `${s} leaked`);
  const report = await worker.evaluate((id) => globalThis.__lectureflowTest.reportFor(id), tabId);
  assert.equal(report.ok, true, JSON.stringify(report.findings));
  assert.equal(new URL(report.report.page.url).hostname, 'tenant.cloud.panopto.eu');
  await popup.close();
  await page.close();
});

test('a popup taller than Chrome’s 600 px (English, possibly restricted, all tracks shown): the send button stays in sight (T26 J6)', async () => {
  server.panopto.downloadUrl = null;
  await fakeLectureFlow();
  await worker.evaluate(() => chrome.storage.local.set({ uiLanguage: 'en' }));
  try {
    const page = await openPage(panoptoUrl('xhr'), 'body[data-ready="1"]');
    const popup = await context.newPage();
    await popup.setViewportSize({ width: 360, height: 600 });
    await popup.goto(`chrome-extension://${extensionId}/popup/popup.html?tabId=${await tabIdOf(page)}`);
    await popup.waitForSelector('body[data-view="blocked"]', { timeout: 15000 });
    await popup.click('#advanced');
    await popup.waitForSelector('#tracks');
    const at = () => popup.evaluate(() => ({ page: document.scrollingElement.scrollHeight, view: innerHeight, button: document.getElementById('primary').getBoundingClientRect().bottom }));
    let m = await at();
    assert.ok(m.page > m.view, `taller than the popup: ${JSON.stringify(m)}`);
    assert.ok(m.button <= m.view, `the button in sight at the top: ${JSON.stringify(m)}`);
    // scrolled to the middle and to the end it is still there, and the end is reachable
    await popup.evaluate(() => window.scrollTo(0, (document.scrollingElement.scrollHeight - innerHeight) / 2));
    m = await at();
    assert.ok(m.button <= m.view && m.button > 0, `in the middle: ${JSON.stringify(m)}`);
    await popup.evaluate(() => window.scrollTo(0, document.scrollingElement.scrollHeight));
    m = await at();
    assert.ok(m.button <= m.view && m.button > 0, `at the end: ${JSON.stringify(m)}`);
    assert.ok(await popup.isVisible('#options'), 'the foot is reachable');
    await H.shoot(popup, 'popup-tall-en', null, { fullPage: false });
    // Tab through the tracks from the top: each one the keyboard reaches is
    // brought out from under the send block (SEC42 中-2)
    await popup.evaluate(() => {
      window.scrollTo(0, 0);
      document.activeElement && document.activeElement.blur();
    });
    let reached = 0;
    const covered = [];
    for (let i = 0; i < 40 && reached < 3; i++) {
      await popup.keyboard.press('Tab');
      const f = await popup.evaluate(() => {
        const a = document.activeElement;
        if (!a || !a.matches('#tracks input[type=checkbox]')) return null;
        return { box: a.getBoundingClientRect().bottom, send: document.querySelector('.send').getBoundingClientRect().top };
      });
      if (!f) continue;
      reached++;
      if (f.box > f.send) covered.push(f);
    }
    assert.equal(reached, 3, 'the three track boxes');
    assert.deepEqual(covered, [], 'no focused box under the send block');
    await popup.close();
    await page.close();
  } finally {
    await worker.evaluate(() => chrome.storage.local.remove('uiLanguage'));
  }
});

test('Panopto, possibly restricted: "对这个平台不再提醒" — later sends go without the reminder; the settings undo it', async () => {
  server.panopto.downloadUrl = null;
  await fakeLectureFlow();
  const remembered = () => worker.evaluate(async () => (await chrome.storage.local.get('restrictedRemembered')).restrictedRemembered || null);
  const enqueues = async () => (await received()).filter((m) => m.type === 'enqueue');
  const page = await openPage(panoptoUrl('xhr'), 'body[data-ready="1"]');
  const tabId = await tabIdOf(page);

  // 1. The reminder has the check box, unticked; ticked and confirmed, Panopto is remembered.
  let popup = await openPopup(tabId, 'blocked');
  await popup.click('#presets [data-preset="main"]');
  await popup.click('#primary');
  await popup.waitForSelector('#override');
  assert.match(await popup.textContent('#override-remember-line'), /对这个平台（Panopto）不再提醒/);
  assert.equal(await popup.isChecked('#override-remember'), false, 'not remembered unless ticked');
  await popup.check('#override-remember');
  await shoot(popup, '4-restricted-remember');
  await popup.click('#override-confirm');
  await popup.waitForSelector('body[data-view="queued"]', { timeout: 10000 });
  assert.deepEqual((await enqueues()).map((m) => m.overrideDisabled), [true]);
  const stored = await remembered();
  assert.deepEqual(Object.keys(stored), ['panopto']);
  assert.match(stored.panopto, /^\d{4}-\d{2}-\d{2}T/);
  await popup.close();

  // 2. Next time: the user's notice without "发送前会再提醒一次", a line saying why, no reminder.
  await fakeLectureFlow();
  popup = await openPopup(tabId, 'blocked');
  assert.equal(
    (await popup.textContent('#reason')).trim(),
    '检测到该媒体流或受讲师或大学政策限制。LectureFlow 仅将您有权访问的媒体流拉取至本地用于个人无障碍学习（转录/翻译）。请确保您遵守学校规定。',
  );
  assert.match(await popup.textContent('#remembered-note'), /你已对 Panopto 关闭这个提醒，可以在扩展设置里恢复/);
  assert.equal(await popup.locator('#presets [aria-pressed="true"]').count(), 0, 'still nothing chosen at first');
  await shoot(popup, '4-restricted-remembered');
  await popup.click('#presets [data-preset="main"]');
  await popup.click('#primary');
  await popup.waitForSelector('body[data-view="queued"]', { timeout: 10000 });
  assert.equal(await popup.locator('#override').count(), 0, 'no reminder');
  assert.deepEqual((await enqueues()).map((m) => m.overrideDisabled), [true]);
  await popup.close();

  // 3. The settings list Panopto; "恢复提醒" undoes it and the reminder is back.
  const opts = await context.newPage();
  await opts.goto(`chrome-extension://${extensionId}/options/options.html`);
  await opts.waitForSelector('#restricted [data-platform="panopto"]', { timeout: 10000 });
  assert.match(await opts.textContent('#restricted [data-platform="panopto"]'), /Panopto.*起不再提醒.*恢复提醒/);
  assert.equal(await opts.isHidden('#restricted-empty'), true);
  await shoot(opts, 'options-restricted');
  await opts.click('#restricted [data-forget="panopto"]');
  await opts.waitForSelector('#restricted-empty:not([hidden])', { timeout: 5000 });
  assert.equal(await opts.locator('#restricted [data-platform]').count(), 0);
  assert.equal(await remembered(), null);
  await opts.close();
  await fakeLectureFlow();
  popup = await openPopup(tabId, 'blocked');
  assert.match(await popup.textContent('#reason'), /发送前会再提醒一次。$/);
  await popup.click('#presets [data-preset="main"]');
  await popup.click('#primary');
  await popup.waitForSelector('#override');
  await popup.click('#override-cancel');
  assert.deepEqual(await enqueues(), []);
  await popup.close();
  await page.close();
});

test('Panopto, allowed, two screens (fetch): main is the MP4; 双机位 sends audio and two pictures; 仅音频 the audio', async () => {
  server.panopto.downloadUrl = '/Panopto/Podcast/Download/whole.mp4';
  server.panopto.twoScreens = true;
  await fakeLectureFlow({ nextTaskId: 60 });
  const page = await openPage(panoptoUrl('fetch'), 'body[data-ready="1"]');
  const popup = await openPopup(await tabIdOf(page), 'ready');
  assert.equal(await popup.getAttribute('#presets [data-preset="main"]', 'aria-pressed'), 'true');
  assert.match(await popup.textContent('#chosen'), /^完整录像（MP4）/);
  assert.ok(!(await popup.textContent('#lecture')).includes('可能受政策限制'));
  await popup.click('#presets [data-preset="audio"]');
  assert.match(await popup.textContent('#chosen'), /^主讲音频/);
  await popup.click('#presets [data-preset="dual"]');
  assert.match(await popup.textContent('#chosen'), /主讲音频 \+ 屏幕录像 1 \+ 屏幕录像 2/);
  await shoot(popup, '3-ready-dual');
  // the advanced list shows the same ticks; ticking by hand leaves the choices
  await popup.click('#advanced');
  assert.deepEqual(await popup.$$eval('#tracks .track', (xs) => xs.map((x) => `${x.dataset.role}:${x.dataset.checked}`)), ['podcast:false', 'primary-audio:true', 'screen:true', 'screen:true']);
  await popup.click('#primary');
  await popup.waitForSelector('body[data-view="queued"]', { timeout: 10000 });
  const enq = (await received()).find((m) => m.type === 'enqueue');
  assert.equal(enq.overrideDisabled, false);
  assert.deepEqual(enq.tracks.map((t) => `${t.role}:${t.kind}`), ['primary-audio:audio', 'screen:video', 'screen:video'], 'one audio, two pictures');
  const all = JSON.stringify(await worker.evaluate(() => globalThis.__fakeLF.received)) + (await storageDump());
  assert.ok(!all.includes(PANOPTO.userEmail) && !all.includes(PANOPTO.userName));
  await popup.close();

  // "续传" starts with the choice the lecture was sent with: 双机位 again
  // (LectureFlow needs both pictures of the task back).
  await fakeLectureFlow({ checkItem: { state: 'needs_refresh', taskId: 60, courseId: COURSES[0].id } });
  const again = await openPopup(await tabIdOf(page), 'refresh');
  assert.equal(await again.getAttribute('#presets [data-preset="dual"]', 'aria-pressed'), 'true');
  assert.match(await again.textContent('#chosen'), /主讲音频 \+ 屏幕录像 1 \+ 屏幕录像 2/);
  await again.click('#primary');
  await again.waitForSelector('body[data-view="queued"]', { timeout: 10000 });
  const ref = (await received()).find((m) => m.type === 'refresh');
  assert.deepEqual(ref.tracks.map((t) => `${t.role}:${t.kind}`), ['primary-audio:audio', 'screen:video', 'screen:video']);
  // what is kept for it: a hash and the choice's name, nothing of the lecture
  const kept = await worker.evaluate(async () => (await chrome.storage.local.get('sentPresets')).sentPresets);
  assert.ok(Object.entries(kept).every(([k, v]) => /^[0-9a-f]{32}$/.test(k) && ['main', 'dual', 'audio'].includes(v)));
  assert.ok(Object.values(kept).includes('dual'));
  assert.ok(!JSON.stringify(kept).includes(PANOPTO.deliveryId) && !JSON.stringify(kept).includes(PANOPTO.title));
  server.panopto.downloadUrl = null;
  server.panopto.twoScreens = false;
  await again.close();
  await page.close();
});

test('needs_refresh: the same page offers "续传", one click sends new tracks and cookies', async () => {
  await fakeLectureFlow({ checkItem: { state: 'needs_refresh', taskId: 7, courseId: COURSES[0].id } });
  await resetCookieCalls();
  const page = await openPage(genericUrl());
  const popup = await openPopup(await tabIdOf(page), 'refresh');
  assert.match(await popup.textContent('#reason'), /刮削中断/);
  assert.match(await popup.textContent('#primary'), /续传/);
  assert.equal(await popup.locator('#course').count(), 0, 'the course is the task’s');
  await shoot(popup, '6-refresh');
  assert.deepEqual(await cookieCalls(), { module: [], raw: [] });
  await popup.click('#primary');
  await popup.waitForSelector('body[data-view="queued"]', { timeout: 10000 });
  const msgs = await received();
  assert.deepEqual(msgs.map((m) => m.type), ['check', 'refresh']);
  const ref = msgs[1];
  const videoUrl = `http://course.lf-e2e.test:${server.port}/media/lecture.mp4`;
  const audioUrl = `http://course.lf-e2e.test:${server.port}/media/talk.m4a`;
  assert.equal(ref.platform, 'generic');
  assert.match(ref.sessionId, /^[0-9a-f]{32}$/);
  assert.deepEqual(ref.tracks.map((t) => t.url), [videoUrl, audioUrl]);
  assert.deepEqual(ref.cookies.map((c) => c.value), [COOKIE_TRACK]);
  assert.equal(ref.referer, genericUrl());
  assertCookieReads(await cookieCalls(), [videoUrl, audioUrl]);
  await popup.close();
  await page.close();
});

test('a status push of needs_refresh while the popup is open turns it into "续传"', async () => {
  await fakeLectureFlow({ checkItem: { state: 'downloading', taskId: 12, courseId: COURSES[0].id } });
  const page = await openPage(genericUrl());
  const popup = await openPopup(await tabIdOf(page), 'queued');
  await worker.evaluate(() => globalThis.__fakeLF.push({ taskId: 12, state: 'needs_refresh' }));
  await popup.waitForSelector('body[data-view="refresh"]', { timeout: 5000 });
  await popup.close();
  await page.close();
});

test('a failed fetch: the reason in words; "重试" only where a retry can fix it; an unknown code in brackets inside a sentence (T26 G3)', async () => {
  await fakeLectureFlow({ checkItem: { state: 'downloading', taskId: 13, courseId: COURSES[0].id } });
  const page = await openPage(genericUrl());
  const popup = await openPopup(await tabIdOf(page), 'queued');
  const push = (error) => worker.evaluate((e) => globalThis.__fakeLF.push({ taskId: 13, state: 'failed', error: e }), error);
  await push({ code: 'downloads.no_space', params: {} });
  await popup.waitForSelector('#task-state[data-state="failed"]', { timeout: 5000 });
  assert.match(await popup.textContent('#status-note'), /^磁盘空间不足.*点“重试”。$/);
  await push({ code: 'downloads.drm_protected', params: {} });
  await popup.waitForFunction(() => /DRM/.test(document.getElementById('status-note').textContent), null, { timeout: 5000 });
  assert.ok(!(await popup.textContent('#status-note')).includes('重试'), 'not retryable: no "重试"');
  await push({ code: 'downloads.brand_new', params: {} });
  await popup.waitForFunction(() => /brand_new/.test(document.getElementById('status-note').textContent), null, { timeout: 5000 });
  assert.equal(await popup.textContent('#status-note'), '刮削没有完成（代码 downloads.brand_new）。可以在 LectureFlow 里查看原因并重试。');
  await shoot(popup, '5-failed');
  await popup.close();
  await page.close();
});

test('cannot send: protected (EME) tracks', async () => {
  await fakeLectureFlow();
  const page = await context.newPage();
  await page.goto(`${server.origin}/eme.html`);
  await page.waitForFunction(() => document.title.startsWith('eme-'), null, { timeout: 10000 });
  await page.waitForTimeout(400);
  const popup = await openPopup(await tabIdOf(page), 'blocked');
  assert.equal(await popup.getAttribute('body', 'data-reason'), 'protected');
  assert.equal(await popup.isDisabled('#primary'), true);
  assert.match(await popup.textContent('#reason'), /数字版权保护（DRM），LectureFlow 不处理受保护的内容/);
  assert.equal(await popup.locator('#presets button:not([disabled])').count(), 0, 'no choice can be made');
  assert.ok(!(await popup.textContent('#chosen')).includes('选一项要发送的内容'), 'no "choose" under choices that cannot be made (T26 G4)');
  assert.equal(await popup.$eval('#presets [data-preset="main"]', (b) => getComputedStyle(b).cursor), 'not-allowed');
  await popup.close();
  await page.close();
});

test('settings: profiles (import, 未审核, remove; YouTube and course platforms refused), sites (the same), exporting a page’s report', async () => {
  const fs = await import('node:fs');
  const os = await import('node:os');
  const path = await import('node:path');
  // a page with a detection, so the report list has something
  const page = await openPage(genericUrl());
  await worker.evaluate(async (id) => globalThis.__lectureflowTest.detectTab(id, { force: true }), await tabIdOf(page));
  const opts = await context.newPage();
  await opts.goto(`chrome-extension://${extensionId}/options/options.html`);
  await opts.waitForSelector('#profiles .row[data-profile="panopto"]');
  assert.match(await opts.textContent('#profiles .row[data-profile="panopto"]'), /Panopto 1\.1\.2.*已审核/);
  assert.match(await opts.textContent('#profiles .row[data-profile="panopto"]'), /站点已允许|允许这些站点/);
  // import: not JSON, not a valid profile, then a valid one (未审核, first, removable)
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-opt-'));
  const write = (name, text) => {
    const f = path.join(dir, name);
    fs.writeFileSync(f, text);
    return f;
  };
  await opts.setInputFiles('#import-file', write('x.json', 'not json'));
  await opts.waitForSelector('#import-msg.note-bad');
  assert.match(await opts.textContent('#import-msg'), /不是 JSON/);
  await opts.setInputFiles('#import-file', write('y.json', JSON.stringify({ schemaVersion: 1, id: 'demo', version: '0.1.0', name: 'Demo', maintainers: ['@someone'], match: { hosts: ['*.youtube.com'] }, session: { idFrom: { query: 'v' } }, tracks: [{ role: 'a', label: { en: 'A' }, detect: { urlPattern: 'x' } }] })));
  // a profile for a do-not-handle site (src/lib/blocklist.js): refused, and said why
  await opts.waitForFunction(() => /不能导入/.test(document.getElementById('import-msg').textContent));
  assert.match(await opts.textContent('#import-msg'), /^不能导入这个对策文件：它匹配了 \*\.youtube\.com。LectureFlow 不处理 YouTube，只处理学校的讲座录播。$/);
  await opts.setInputFiles('#import-file', write('u.json', JSON.stringify({ schemaVersion: 1, id: 'demo', version: '0.1.0', name: 'Demo', maintainers: ['@someone'], match: { hosts: ['*.linkedin.com'], pathPattern: '^/learning/' }, session: { idFrom: { query: 'v' } }, tracks: [{ role: 'a', label: { en: 'A' }, detect: { urlPattern: 'x' } }] })));
  await opts.waitForFunction(() => /linkedin\.com\/learning/.test(document.getElementById('import-msg').textContent));
  assert.match(await opts.textContent('#import-msg'), /^不能导入这个对策文件：它匹配了 linkedin\.com\/learning。LectureFlow 不处理在线课程平台（MOOC、付费课程），只处理学校的讲座录播。$/);
  await opts.setInputFiles('#import-file', write('z.json', JSON.stringify({ schemaVersion: 1, id: 'demo', version: '0.1.0', name: 'Demo', maintainers: ['@someone'], match: { hosts: ['video.example.edu'] }, session: { idFrom: { query: 'v' } }, tracks: [{ role: 'a', label: { en: 'A' }, detect: { urlPattern: 'x' } }] })));
  await opts.waitForSelector('#profiles .row[data-profile="demo"][data-reviewed="false"]');
  assert.match(await opts.textContent('#profiles .row[data-profile="demo"]'), /未审核/);
  await opts.click('#profiles .row[data-profile="demo"] [data-remove="demo"]');
  await opts.waitForSelector('#profiles .row[data-profile="demo"]', { state: 'detached' });
  // A file with the packaged profile's id replaces it only once the user
  // confirms; it is then "未审核" (security review L7).
  const packagedPanopto = JSON.parse(fs.readFileSync(path.join(H.root, 'profiles', 'panopto.json'), 'utf8'));
  const stored = () => worker.evaluate(async () => (await chrome.storage.local.get('localProfiles')).localProfiles || []);
  // An unreviewed file may request sessionInfo only with GET (FB1 L-2): the
  // packaged profile's POST, imported as it is, is refused.
  await opts.setInputFiles('#import-file', write('panopto-post.json', JSON.stringify({ ...packagedPanopto, version: '1.2.0-dev' })));
  await opts.waitForFunction(() => /\/sessionInfo\/method/.test(document.getElementById('import-msg').textContent));
  assert.deepEqual(await stored(), [], 'refused: nothing stored');
  const mine = { ...packagedPanopto, version: '1.2.0-dev', sessionInfo: { ...packagedPanopto.sessionInfo, method: 'GET' } };
  await opts.setInputFiles('#import-file', write('panopto-dev.json', JSON.stringify(mine)));
  await opts.waitForSelector('#import-confirm:not([hidden])');
  assert.match(await opts.textContent('#import-confirm-text'), /与内置的对策文件“Panopto 1\.1\.2”同名.*未审核/);
  await opts.click('#import-cancel');
  await opts.waitForSelector('#import-confirm', { state: 'hidden' });
  assert.deepEqual(await stored(), [], 'cancelled: nothing stored');
  await opts.setInputFiles('#import-file', write('panopto-dev2.json', JSON.stringify(mine)));
  await opts.waitForSelector('#import-confirm:not([hidden])');
  await shoot(opts, 'options-replace');
  await opts.click('#import-replace');
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="false"]');
  assert.match(await opts.textContent('#profiles .row[data-profile="panopto"][data-reviewed="false"]'), /Panopto 1\.2\.0-dev.*未审核.*代替内置的 1\.1\.2 版/);
  assert.deepEqual((await stored()).map((e) => [e.profile.id, e.profile.version, e.replaces]), [['panopto', '1.2.0-dev', '1.1.2']]);
  // an entry stored without the confirmation (an older version of the extension) is not used
  await worker.evaluate((p) => chrome.storage.local.set({ localProfiles: [{ profile: p, importedAt: '2026-10-01' }] }), mine);
  await opts.reload();
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="false"] [data-inactive="true"]');
  await opts.click('#profiles .row[data-profile="panopto"][data-reviewed="false"] [data-remove="panopto"]');
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="false"]', { state: 'detached' });
  // a confirmed replacement stored before local profiles were limited to GET
  // (SEC18 低-3): not in use, and the page says so and why instead of "代替内置的"
  await worker.evaluate((p) => chrome.storage.local.set({ localProfiles: [{ profile: p, importedAt: '2026-10-01', replaces: '1.1.1' }] }), { ...packagedPanopto, version: '1.2.0-dev' });
  await opts.reload();
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="false"] [data-invalid="true"]');
  assert.match(await opts.textContent('#profiles .row[data-profile="panopto"][data-reviewed="false"]'), /未启用：这个本地对策文件已不再通过检查（\/sessionInfo\/method /);
  assert.equal(await opts.locator('#profiles .row[data-profile="panopto"][data-reviewed="false"] [data-replaces]').count(), 0);
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="true"]');
  await opts.click('#profiles .row[data-profile="panopto"][data-reviewed="false"] [data-remove="panopto"]');
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="false"]', { state: 'detached' });
  // Profiles stopped by the runtime regex guard (SEC18d, src/lib/health.js):
  // a local one says so until imported again; the built-in one asks for a
  // report and can be enabled again here. The records hold no address.
  const extVersion = await worker.evaluate(() => chrome.runtime.getManifest().version);
  const slowRec = (source, version) => ({ id: 'panopto', version, source, rule: '/tracks/1/detect/urlPattern', ms: 812, reason: 'slow', trips: 3, disabled: true, extVersion, at: '2026-11-11' });
  await worker.evaluate(([p, rec]) => chrome.storage.local.set({ localProfiles: [{ profile: p, importedAt: '2026-11-01', replaces: '1.1.1' }], profileHealth: { 'local:panopto': rec } }), [mine, slowRec('local', mine.version)]);
  await opts.reload();
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="false"] [data-slow="true"]');
  assert.match(await opts.textContent('#profiles .row[data-profile="panopto"][data-reviewed="false"]'), /未启用：规则 \/tracks\/1\/detect\/urlPattern 匹配太慢（812 毫秒）。修改文件后重新导入即可再次启用/);
  // importing the file again clears the record
  await opts.setInputFiles('#import-file', write('panopto-again.json', JSON.stringify(mine)));
  await opts.waitForSelector('#import-confirm:not([hidden])');
  await opts.click('#import-replace');
  await opts.waitForFunction(() => !document.querySelector('#profiles .row[data-reviewed="false"] [data-slow="true"]'));
  assert.deepEqual(await worker.evaluate(async () => (await chrome.storage.local.get('profileHealth')).profileHealth || {}), {});
  await opts.click('#profiles .row[data-profile="panopto"][data-reviewed="false"] [data-remove="panopto"]');
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="false"]', { state: 'detached' });
  await worker.evaluate((rec) => chrome.storage.local.set({ profileHealth: { [`bundled:panopto@${rec.version}`]: rec } }), slowRec('bundled', packagedPanopto.version));
  await opts.reload();
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="true"] [data-slow="true"]');
  assert.match(await opts.textContent('#profiles .row[data-profile="panopto"][data-reviewed="true"]'), /未启用：内置对策文件的规则 \/tracks\/1\/detect\/urlPattern 在这台电脑上匹配太慢（812 毫秒）。这是 LectureFlow 的问题/);
  await opts.click('#profiles .row[data-profile="panopto"][data-reviewed="true"] [data-reenable="panopto"]');
  await opts.waitForSelector('#profiles .row[data-profile="panopto"][data-reviewed="true"] [data-slow="true"]', { state: 'detached' });
  assert.deepEqual(await worker.evaluate(async () => (await chrome.storage.local.get('profileHealth')).profileHealth || {}), {});
  fs.rmSync(dir, { recursive: true, force: true });
  // sites: the granted ones are listed; YouTube cannot be added
  assert.ok((await opts.locator('#sites .row').count()) >= 1);
  await opts.fill('#site-input', 'https://www.youtube.com/');
  await opts.click('#add-site button[type=submit]');
  await opts.waitForSelector('#site-msg.note-bad');
  assert.match(await opts.textContent('#site-msg'), /不处理 YouTube/);
  await opts.fill('#site-input', 'https://www.udemy.com/course/x/');
  await opts.click('#add-site button[type=submit]');
  await opts.waitForFunction(() => /在线课程平台/.test(document.getElementById('site-msg').textContent));
  // reports: the page detected above is listed by host
  await opts.waitForSelector('#reports .row');
  assert.match(await opts.textContent('#reports'), /course\.lf-e2e\.test · 3 条轨道/);
  await shoot(opts, 'options');
  await opts.close();
  await page.close();
});

test('nothing reached any host but the local test server', () => {
  const hosts = new Set(server.requests.map((r) => r.host));
  for (const h of hosts) assert.ok(['127.0.0.1', 'course.lf-e2e.test', PANOPTO.host].includes(h), h);
});
