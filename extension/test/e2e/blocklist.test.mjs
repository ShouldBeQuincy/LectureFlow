// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// End to end, the do-not-handle list (src/lib/blocklist.js; user
// 2026-10-27: no YouTube, no MOOC or paid course platform): on such a page
// the popup says LectureFlow does not handle it (YouTube and course
// platforms in their own words) and nothing is detected or counted; a
// course platform embedded in an LMS page is left out with everything it
// loads, although its video comes from a general CDN host the extension
// may read; on LinkedIn only /learning is refused; a Panopto player inside
// a course platform's page gets no in-page button. The test browser maps
// www.youtube-nocookie.com, www.udemy.com, www.edx.org and
// www.linkedin.com to the local test server (test/e2e/harness.mjs; hosts
// on Chromium's HSTS preload list, www.youtube.com or coursera.org, cannot
// be served over plain HTTP), and the e2e build is granted three of them
// (scripts/build.mjs) — access no user can give — to show the extension
// refuses them all the same.
//
//   npm run test:e2e

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import * as H from './harness.mjs';
import { PANOPTO } from './server.mjs';

let B;
let server;
let context;
let worker;
let extensionId;

before(async () => {
  B = await H.startBrowser();
  ({ server, context, worker, extensionId } = B);
  await H.fakeLectureFlow(worker);
});

// a test that failed leaves nothing behind for the next one (harness.mjs)
H.isolateTests(() => B);

after(async () => {
  if (B) await B.close();
});

const port = () => server.port;
const tabIdOf = (page) => H.tabIdOf(worker, page);
// a detection as plain data (the result itself holds Sets and the like)
const detect = (tabId) =>
  worker.evaluate(async (id) => {
    const r = await globalThis.__lectureflowTest.detectTab(id, { force: true });
    return { status: r.status, category: r.category || '', tracks: (r.tracks || []).map((t) => ({ url: t.url })), blockedEmbeds: r.blockedEmbeds || [], raw: JSON.stringify(r.raw || null) };
  }, tabId);
const frameCommand = (tabId, frameUrl, msg) => worker.evaluate(({ tabId, frameUrl, msg }) => globalThis.__lectureflowTest.inpageCommand(tabId, 1, frameUrl, msg), { tabId, frameUrl, msg });
const badge = (tabId) => worker.evaluate((id) => chrome.action.getBadgeText({ tabId: id }), tabId);

async function openPage(url, wait = 1500) {
  const page = await context.newPage();
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(wait);
  return page;
}

async function openPopup(tabId, view) {
  const popup = await context.newPage();
  await popup.setViewportSize({ width: 360, height: 120 });
  await popup.goto(`chrome-extension://${extensionId}/popup/popup.html?tabId=${tabId}`);
  await popup.waitForSelector(`body[data-view="${view}"]`, { timeout: 15000 });
  return popup;
}

test('content scripts are never registered on a do-not-handle site, whatever was granted', async () => {
  const scripts = await worker.evaluate(async () => (await chrome.scripting.getRegisteredContentScripts()).map((s) => ({ id: s.id, matches: s.matches, exclude: s.excludeMatches || [] })));
  assert.ok(scripts.length >= 1);
  for (const s of scripts) {
    for (const p of ['*://*.youtube.com/*', '*://*.edx.org/*', '*://*.udemy.com/*', '*://*.linkedin.com/learning*']) assert.ok(s.exclude.includes(p), `${s.id}: ${p}`);
  }
  assert.ok(scripts.find((s) => s.id === 'lf-observe').matches.includes('http://*.edx.org/*'), 'granted in the e2e build');
});

test('YouTube: "LectureFlow 不处理 YouTube", no tracks, no number on the badge', async () => {
  const page = await openPage(`http://www.youtube-nocookie.com:${port()}/blocked/course.html`);
  const tabId = await tabIdOf(page);
  const r = await detect(tabId);
  assert.deepEqual({ status: r.status, category: r.category }, { status: 'unsupported', category: 'youtube' });
  const popup = await openPopup(tabId, 'unsupported');
  assert.equal(await popup.getAttribute('body', 'data-reason'), 'youtube');
  assert.match(await popup.textContent('#state-title'), /这个网站不在 LectureFlow 的支持范围内/);
  assert.equal(await popup.textContent('#unsupported-text'), 'LectureFlow 不处理 YouTube，只处理学校的讲座录播。');
  assert.equal(await popup.locator('#primary').count(), 0);
  assert.equal(await badge(tabId), '');
  await H.shoot(popup, 'popup-unsupported-youtube');
  await popup.close();
  await page.close();
});

test('LectureFlow installed but not open: the popup on a do-not-handle site never starts it (T26 G1)', async () => {
  await H.fakeLectureFlow(worker, { mode: 'stopped' });
  try {
    for (const site of ['www.youtube-nocookie.com', 'www.edx.org']) {
      const page = await openPage(`http://${site}:${port()}/blocked/course.html`);
      const popup = await openPopup(await tabIdOf(page), 'unsupported');
      await popup.waitForFunction(() => document.getElementById('conn').dataset.nmState !== 'checking', null, { timeout: 15000 });
      await popup.waitForTimeout(300);
      assert.equal(await H.launches(worker), 0, site);
      assert.ok((await H.hellos(worker)).every((m) => m.noLaunch === true), `${site}: every hello says noLaunch`);
      await popup.close();
      await page.close();
    }
  } finally {
    await H.fakeLectureFlow(worker);
  }
});

test('a course platform page: "不处理在线课程平台（MOOC、付费课程）", although its video is on a CDN the extension may read', async () => {
  const page = await openPage(`http://www.edx.org:${port()}/blocked/course.html`);
  const tabId = await tabIdOf(page);
  assert.ok(server.requests.some((q) => q.host === 'cdn.lf-e2e.test' && q.path === '/media/lecture.mp4'), 'the page loaded its video');
  const r = await detect(tabId);
  assert.deepEqual({ status: r.status, category: r.category }, { status: 'unsupported', category: 'courses' });
  const popup = await openPopup(tabId, 'unsupported');
  assert.equal(await popup.getAttribute('body', 'data-reason'), 'courses');
  assert.equal(await popup.textContent('#unsupported-text'), 'LectureFlow 不处理在线课程平台（MOOC、付费课程），只处理学校的讲座录播。');
  assert.equal(await badge(tabId), '');
  await H.shoot(popup, 'popup-unsupported-courses');
  await popup.close();
  await page.close();
});

test('LinkedIn: /learning is refused, the rest of the site is not', async () => {
  const learning = await openPage(`http://www.linkedin.com:${port()}/learning/some-course/intro`);
  const r1 = await detect(await tabIdOf(learning));
  assert.deepEqual({ status: r1.status, category: r1.category }, { status: 'unsupported', category: 'courses' });
  await learning.close();
  const feed = await openPage(`http://www.linkedin.com:${port()}/feed/update/1`);
  const r2 = await detect(await tabIdOf(feed));
  assert.equal(r2.status, 'ok');
  assert.deepEqual(r2.tracks.map((t) => new URL(t.url).pathname), ['/media/lecture.mp4']);
  await feed.close();
});

test('a course platform embedded in an LMS page: its frame and its requests are left out, the page’s own lecture stays', async () => {
  const frameUrl = `http://www.edx.org:${port()}/blocked/course.html`;
  const before = server.requests.length;
  const page = await openPage(`http://course.lf-e2e.test:${port()}/blocked/lms.html?frame=${encodeURIComponent(frameUrl)}`, 2500);
  const tabId = await tabIdOf(page);
  // the embedded platform did load its video, from the general CDN host
  assert.ok(server.requests.slice(before).some((q) => q.host === 'cdn.lf-e2e.test' && q.path === '/media/lecture.mp4'));
  const r = await detect(tabId);
  assert.equal(r.status, 'ok');
  assert.deepEqual(r.tracks.map((t) => `${new URL(t.url).host.split(':')[0]}${new URL(t.url).pathname}`), ['course.lf-e2e.test/media/talk.m4a']);
  assert.deepEqual(r.blockedEmbeds, ['courses']);
  // what the extension keeps knows nothing of the platform's video
  assert.ok(!(await H.storageDump(worker)).includes('lecture.mp4'), 'no request of the blocked frame was recorded');
  // (the page's own address names the frame in its query, escaped: that is this test's doing)
  for (const bit of ['lecture.mp4', 'edx.org:', 'edx.org/']) assert.ok(!r.raw.includes(bit), `nor kept for the report: ${r.raw.slice(Math.max(0, r.raw.indexOf(bit) - 200), r.raw.indexOf(bit) + 100)}`);
  const popup = await openPopup(tabId, 'ready');
  await popup.click('#advanced');
  await popup.waitForSelector('#tracks .track');
  assert.deepEqual(await popup.$$eval('#tracks .track', (xs) => xs.map((x) => x.dataset.kind)), ['audio']);
  await popup.close();
  await page.close();
  // only the embedded platform: nothing found, and the popup says why
  const only = await openPage(`http://course.lf-e2e.test:${port()}/blocked/lms.html?own=0&frame=${encodeURIComponent(frameUrl)}`, 2500);
  const onlyTab = await tabIdOf(only);
  await detect(onlyTab);
  const p2 = await openPopup(onlyTab, 'none');
  assert.equal(await p2.textContent('#blocked-embed'), '这一页里嵌入的 YouTube 或在线课程平台内容已跳过，LectureFlow 不处理这些内容。');
  await H.shoot(p2, 'popup-none-blocked-embed');
  await p2.close();
  await only.close();
});

test('a Panopto player inside a course platform’s page gets no in-page button; inside an LMS page it does', async () => {
  const embed = `http://${PANOPTO.host}:${port()}/Panopto/Pages/Embed.aspx?id=${PANOPTO.deliveryId}`;
  const button = 'h1#title lectureflow-ui[data-kind="player"]';
  // the control: the same page on an LMS host
  const lms = await openPage(`http://canvas.lf-e2e.test:${port()}/blocked/lms.html?own=0&frame=${encodeURIComponent(embed)}`, 0);
  const lf = lms.frameLocator('iframe[name="tool_content"]');
  await lf.locator('body[data-ready="1"]').waitFor({ timeout: 10000 });
  await lf.locator(button).waitFor({ timeout: 10000 });
  await lms.close();
  // a page of www.udemy.com (not granted: the browser gives the extension
  // only location.ancestorOrigins of the Panopto frame)
  const udemy = await openPage(`http://www.udemy.com:${port()}/blocked/lms.html?own=0&frame=${encodeURIComponent(embed)}`, 0);
  const uf = udemy.frameLocator('iframe[name="tool_content"]');
  await uf.locator('body[data-ready="1"]').waitFor({ timeout: 10000 });
  await udemy.waitForTimeout(2500);
  assert.equal(await uf.locator('lectureflow-ui').count(), 0);
  // nor in a page of www.edx.org (granted here: the tab's own address says it)
  const edx = await openPage(`http://www.edx.org:${port()}/blocked/lms.html?own=0&frame=${encodeURIComponent(embed)}`, 0);
  const cf = edx.frameLocator('iframe[name="tool_content"]');
  await cf.locator('body[data-ready="1"]').waitFor({ timeout: 10000 });
  await edx.waitForTimeout(2500);
  assert.equal(await cf.locator('lectureflow-ui').count(), 0);
  // and the in-page commands of such a frame are refused outright
  const udemyTab = await tabIdOf(udemy);
  assert.deepEqual(await frameCommand(udemyTab, embed, { cmd: 'route', ancestors: [`http://www.udemy.com:${port()}`] }), { ok: false, error: { code: 'ext.site_blocked', params: { category: 'courses' } } });
  assert.deepEqual(await frameCommand(udemyTab, embed, { cmd: 'detect', ancestors: [`http://player.lf-e2e.test:${port()}`, `http://www.youtube-nocookie.com:${port()}`] }), { ok: false, error: { code: 'ext.site_blocked', params: { category: 'youtube' } } });
  assert.equal((await frameCommand(udemyTab, embed, { cmd: 'route', ancestors: [`http://canvas.lf-e2e.test:${port()}`] })).ok, true);
  await udemy.close();
  await edx.close();
});

test('nothing reached any host but the local test server', () => {
  const allowed = ['127.0.0.1', 'course.lf-e2e.test', 'canvas.lf-e2e.test', 'cdn.lf-e2e.test', PANOPTO.host, 'www.youtube-nocookie.com', 'www.udemy.com', 'www.edx.org', 'www.linkedin.com'];
  for (const h of new Set(server.requests.map((r) => r.host))) assert.ok(allowed.includes(h), h);
});
