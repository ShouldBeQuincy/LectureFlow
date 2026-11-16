// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// End to end, the in-page entry points (user requests 2026-10-27): the
// player's "发送至 LectureFlow" button (where it goes, that it survives the
// header re-rendering, its card), the folder list in the main site's
// layout and inside a Canvas iframe (row buttons and states, check boxes,
// the button for the whole page above the bar's background layer, the
// selection bar, the bulk card with the dedup by title), the sessionInfo
// request made only after a click and only for the sessions sent, one
// failing session not stopping the others, the embed player in a Canvas
// page, and no buttons on generic sites. The shadow roots are open in the
// e2e build only, so the test can look in. The Canvas test site is on the
// trusted embedding sites here (test/e2e/embed.test.mjs covers the rest):
// its frames confirm on the page, with the controls armed after 600 ms.

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import * as H from './harness.mjs';
import { PANOPTO, LIST } from './server.mjs';

const { COURSES } = H;
let B;
let server;
let context;
let worker;

before(async () => {
  B = await H.startBrowser({ viewport: { width: 1280, height: 800 } });
  ({ server, context, worker } = B);
  await worker.evaluate((o) => chrome.storage.local.set({ trustedEmbedders: { [o]: new Date().toISOString() } }), `http://canvas.lf-e2e.test:${server.port}`);
});

// a test that failed leaves nothing behind for the next one (harness.mjs)
H.isolateTests(() => B);

/** A confirming control in a frame inside another site: it acts once it has been shown (enabled) for 600 ms. */
async function clickArmed(locator) {
  await locator.waitFor({ state: 'visible', timeout: 15000 });
  for (let i = 0; i < 150 && !(await locator.isEnabled()); i++) await new Promise((r) => setTimeout(r, 100));
  await new Promise((r) => setTimeout(r, 700));
  await locator.click();
}

after(async () => {
  if (B) await B.close();
});

const received = () => H.received(worker);
const infoRequests = () => server.requests.filter((r) => r.path.endsWith('/DeliveryInfo.aspx')).map((r) => r.deliveryId);
const listIds = new Set(LIST.map((s) => s.id));
const listInfoRequests = () => infoRequests().filter((id) => listIds.has(id));
const S = (n) => LIST[n - 1].id; // session n of the list
const port = () => server.port;
const listUrl = () => `http://${PANOPTO.host}:${port()}/Panopto/Pages/Sessions/List.aspx`;

async function open(url, ready) {
  const page = await context.newPage();
  await page.goto(url);
  if (ready) await page.waitForSelector(ready, { timeout: 10000 });
  return page;
}

/** The fake LectureFlow of the list tests: one imported, one being fetched, one imported by title. */
async function listLectureFlow(extra = {}) {
  await H.fakeLectureFlow(worker, {
    checkMap: { [S(2)]: { state: 'imported', taskId: 2, lectureId: '01JE2ELECTURE0000000000002' }, [S(3)]: { state: 'downloading', taskId: 33 } },
    titles: { [COURSES[0].id]: [LIST[3].title.replace(/\//g, ' ')] },
    nextTaskId: 70,
    ...extra,
  });
  await worker.evaluate((id) => chrome.storage.local.set({ lastCourseId: id }), COURSES[0].id);
}

const rowHost = (scope, n, kind) => scope.locator(`tr[id="${S(n)}"] lectureflow-ui[data-kind="${kind}"]`);

test('player: the button sits in the header before #header-right-react, survives a re-render, and its card sends', async () => {
  server.panopto.downloadUrl = '/Panopto/Podcast/Download/whole.mp4';
  await H.fakeLectureFlow(worker, { nextTaskId: 50 });
  const page = await open(`http://${PANOPTO.host}:${port()}/Panopto/Pages/Viewer.aspx?id=${PANOPTO.deliveryId}`, 'body[data-ready="1"]');
  await page.waitForSelector('lectureflow-ui[data-kind="player"]', { timeout: 10000 });
  const where = () => page.evaluate(() => {
    const h = document.querySelector('lectureflow-ui[data-kind="player"]');
    return { parent: h.parentElement.id, next: h.nextElementSibling && h.nextElementSibling.id, count: document.querySelectorAll('lectureflow-ui[data-kind="player"]').length };
  });
  assert.deepEqual(await where(), { parent: 'viewerHeader', next: 'header-right-react', count: 1 });
  assert.match(await page.locator('lectureflow-ui[data-kind="player"] .ip-btn').textContent(), /发送至 LectureFlow/);
  // Panopto's React header re-renders: the button stays, once.
  await page.evaluate(() => window.__rerenderHeader());
  await page.waitForTimeout(400);
  assert.deepEqual(await where(), { parent: 'viewerHeader', next: 'header-right-react', count: 1 });

  await page.locator('lectureflow-ui[data-kind="player"] .ip-btn').click();
  const card = page.locator('lectureflow-ui[data-kind="card"]');
  await card.locator('.lf-card[data-view="ready"]').waitFor({ timeout: 15000 });
  assert.match(await card.locator('#lecture').textContent(), /CSC1003\/L01\/01/);
  assert.equal(await card.locator('#presets [data-preset="main"]').getAttribute('aria-pressed'), 'true');
  assert.match(await card.locator('#primary').textContent(), /确认发送/);
  assert.match(await card.locator('#data-note').textContent(), /Cookie 只在你点“确认发送”时读取，只交给本机的 LectureFlow/);
  await H.shoot(page, 'inpage-player', null, { fullPage: false });
  // the page's own handlers never saw our clicks; Escape closes the card
  await card.locator('#primary').click();
  await card.locator('.lf-card[data-view="queued"]').waitFor({ timeout: 10000 });
  const enq = (await received()).find((m) => m.type === 'enqueue');
  assert.equal(enq.platform, 'panopto');
  assert.equal(enq.sessionId, PANOPTO.deliveryId);
  assert.deepEqual(enq.tracks.map((t) => t.role), ['podcast']);
  await page.keyboard.press('Escape');
  server.panopto.downloadUrl = null;
  await page.close();
});

test('list, main layout: row buttons and states; the header button is above the bar’s background layer; nothing requested before a click', async () => {
  await listLectureFlow();
  const page = await open(listUrl(), 'body[data-ready="1"]');
  await page.waitForSelector(`tr[id="${S(6)}"] lectureflow-ui[data-kind="row"]`, { timeout: 10000 });
  await page.waitForSelector(`tr[id="${S(2)}"] [data-row-state="imported"]`, { timeout: 10000 });
  // states from LectureFlow: imported (with "open"), being fetched (from a status push)
  assert.match(await rowHost(page, 2, 'row').locator('.lf-root').textContent(), /已导入.*在 LectureFlow 中打开/);
  await worker.evaluate((sid) => globalThis.__fakeLF.push({ taskId: 33, platform: 'panopto', sessionId: sid, state: 'downloading', bytesDone: 42, bytesTotal: 100 }), S(3));
  await page.waitForSelector(`tr[id="${S(3)}"] [data-row-state="downloading"]`, { timeout: 5000 });
  assert.match(await rowHost(page, 3, 'row').locator('.lf-root').textContent(), /刮削中 42%/);
  for (const n of [1, 4, 5, 6]) assert.match(await rowHost(page, n, 'row').locator('.lf-root').textContent(), /发送至 LectureFlow/, `row ${n}`);
  // check boxes before the titles; imported and fetching rows cannot be ticked
  for (const n of [2, 3]) assert.equal(await rowHost(page, n, 'check').locator('input').isDisabled(), true);
  for (const n of [1, 4, 5, 6]) assert.equal(await rowHost(page, n, 'check').locator('input').isDisabled(), false);
  assert.equal(await page.evaluate((sid) => document.getElementById(sid).querySelector('.item-title').firstElementChild.tagName, S(1)), 'LECTUREFLOW-UI');
  // the button for the whole page: right before Subscribe, not covered
  const hdr = page.locator('lectureflow-ui[data-kind="header"]');
  assert.equal(await hdr.evaluate((h) => h.nextElementSibling.className), 'subscribe-button headerButton');
  const covered = await hdr.evaluate((h) => {
    const r = h.getBoundingClientRect();
    return document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2) !== h;
  });
  assert.equal(covered, false, 'the background layer does not cover the button');
  assert.match(await hdr.locator('.ip-btn').textContent(), /更新这门课程到 LectureFlow/);
  // no sessionInfo request yet, and the row's own click handler saw nothing
  await page.waitForTimeout(300);
  assert.deepEqual(listInfoRequests(), []);
  await H.shoot(page, 'inpage-list-rows', null, { fullPage: false, clip: { x: 0, y: 0, width: 1280, height: 640 } });

  // One row's button: its sessionInfo, once, then the card.
  await rowHost(page, 1, 'row').locator('.ip-btn').click();
  const card = page.locator('lectureflow-ui[data-kind="card"]');
  await card.locator('.lf-card[data-view="ready"]').waitFor({ timeout: 10000 });
  assert.deepEqual(listInfoRequests(), [S(1)]);
  assert.equal(await page.evaluate(() => window.__rowClicks || 0), 0, 'the row did not see the click');
  assert.match(await card.locator('#lecture').textContent(), /CSC1003\/L01\/01 Wed Oct 21/);
  assert.match(await card.locator('#chosen').textContent(), /完整录像（MP4）/);
  const info = server.requests.find((r) => r.deliveryId === S(1));
  assert.equal(info.form.deliveryId, S(1));
  assert.equal(info.form.responseType, 'json');
  assert.equal(info.host, PANOPTO.host, 'same origin as the list');
  await H.shoot(page, 'inpage-list-card', null, { fullPage: false, clip: { x: 0, y: 0, width: 1280, height: 800 } });
  await card.locator('#ip-close').click();
  await card.waitFor({ state: 'detached' });

  // The whole page: one check with titles and the course, dedup by title.
  await hdr.locator('.ip-btn').click();
  const bulk = page.locator('lectureflow-ui[data-kind="card"]');
  await bulk.locator('.lf-card[data-phase="idle"]').waitFor({ timeout: 10000 });
  const chk = (await received()).filter((m) => m.type === 'check' && m.titles).pop();
  assert.deepEqual(chk.sessionIds, LIST.map((s) => s.id));
  assert.deepEqual(chk.titles, LIST.map((s) => s.title));
  assert.equal(chk.courseId, COURSES[0].id);
  assert.equal(
    (await bulk.locator('#bulk-summary').textContent()).trim(),
    '共 6 节：2 节已导入（按会话与标题去重），1 节正在刮削，3 节将发送',
  );
  const states = await bulk.locator('#bulk-rows .bulk-row').evaluateAll((xs) => xs.map((x) => `${x.dataset.state}:${x.querySelector('input').checked}:${x.querySelector('input').disabled}`));
  assert.deepEqual(states, ['new:true:false', 'imported:false:true', 'active:false:true', 'imported:false:true', 'new:true:false', 'new:true:false']);
  assert.match(await bulk.locator('#primary').textContent(), /发送 3 节/);
  await H.shoot(page, 'inpage-bulk', null, { fullPage: false, clip: { x: 0, y: 0, width: 1280, height: 800 } });
  assert.deepEqual(listInfoRequests(), [S(1)], 'still nothing for the others');

  // Send: sessionInfo for the three, one after another; one fails, one is
  // restricted (one reminder), the others go on.
  await bulk.locator('#primary').click();
  await bulk.locator('#override').waitFor({ timeout: 15000 });
  assert.match(await bulk.locator('#override').textContent(), /该媒体流可能受政策限制/);
  assert.match(await bulk.locator('#override').textContent(), /其中 1 节可能受讲师或大学政策限制/);
  // nothing is being sent while the user decides (T26 G6)
  assert.equal((await bulk.locator('#primary').textContent()).trim(), '等待确认…');
  await bulk.locator('#override-confirm').click();
  await bulk.locator('.lf-card[data-phase="done"]').waitFor({ timeout: 15000 });
  // the reminder is behind: no "发送前会再提醒一次" left on the card (T26 G6)
  assert.ok(!(await bulk.locator('#bulk-warning').textContent()).includes('发送前会再提醒一次'));
  assert.deepEqual(listInfoRequests(), [S(1), S(1), S(5), S(6)], 'only the sessions sent, in order, after the click');
  assert.match(await bulk.locator('#bulk-done').textContent(), /已发送 2 节；1 节没有发送/);
  const rows = await bulk.locator('#bulk-rows .bulk-row').evaluateAll((xs) => xs.map((x) => x.dataset.state));
  assert.deepEqual(rows, ['queued', 'imported', 'active', 'imported', 'failed', 'queued']);
  assert.match(await bulk.locator(`.bulk-row[data-session-id="${S(5)}"] .bulk-error`).textContent(), /没能从录播平台读到这节课的信息（500）/);
  const enqs = (await received()).filter((m) => m.type === 'enqueue');
  assert.deepEqual(enqs.map((m) => [m.sessionId, m.overrideDisabled, m.tracks.map((t) => t.role).join()]), [[S(1), false, 'podcast'], [S(6), true, 'podcast']]);
  assert.ok(enqs.every((m) => m.courseId === COURSES[0].id && m.platform === 'panopto' && m.lecture.title && m.cookies.length === 0));
  // nothing of the viewer leaves the page
  const kept = JSON.stringify(await received()) + JSON.stringify(await worker.evaluate(() => [...globalThis.__lectureflowTest.sessions.values()])) + (await H.storageDump(worker));
  assert.ok(!kept.includes(PANOPTO.userEmail) && !kept.includes(PANOPTO.userName));
  // the rows follow
  await page.waitForSelector(`tr[id="${S(1)}"] [data-row-state="queued"]`, { timeout: 5000 });
  await page.close();
});

test('the player’s card in a low window: the send block scrolls with the card instead of covering it; in a tall one it stays in sight (SEC42 中-1)', async () => {
  server.panopto.downloadUrl = null; // possibly restricted: the notice is part of the card
  await H.fakeLectureFlow(worker, { nextTaskId: 300 });
  const page = await context.newPage();
  try {
    for (const [height, sticky] of [[420, 'false'], [800, 'true']]) {
      await page.setViewportSize({ width: 1280, height });
      await page.goto(`http://${PANOPTO.host}:${port()}/Panopto/Pages/Viewer.aspx?id=${PANOPTO.deliveryId}`);
      await page.waitForSelector('body[data-ready="1"]', { timeout: 10000 });
      await page.locator('lectureflow-ui[data-kind="player"] .ip-btn').click();
      const card = page.locator('lectureflow-ui[data-kind="card"]');
      await card.locator('.lf-card[data-view="blocked"]').waitFor({ timeout: 15000 });
      await page.waitForTimeout(300);
      const m = await card.locator('.ip-card').evaluate((f) => {
        const r = f.getBoundingClientRect();
        const send = f.querySelector('.send').getBoundingClientRect();
        const lec = f.querySelector('#lecture').getBoundingClientRect();
        return { sticky: f.dataset.sendSticky, card: [r.top, r.bottom], send: [send.top, send.bottom], lecture: [lec.top, lec.bottom], pos: getComputedStyle(f.querySelector('.send')).position };
      });
      assert.equal(m.sticky, sticky, `${height}: ${JSON.stringify(m)}`);
      if (sticky === 'false') {
        assert.equal(m.pos, 'static', JSON.stringify(m));
        // the lecture is what one sees first, not covered by the send block
        assert.ok(m.lecture[1] <= m.card[1] && m.lecture[1] <= m.send[0], JSON.stringify(m));
      } else assert.equal(m.pos, 'sticky');
      await card.locator('#ip-close').click();
      await card.waitFor({ state: 'detached' });
    }
  } finally {
    await page.close();
  }
});

test('list, a lecture that needs a refresh: not counted as being fetched; the bulk card says why it cannot be ticked and where to resume it (T26 G5)', async () => {
  await listLectureFlow({ checkMap: { [S(2)]: { state: 'imported', taskId: 2, lectureId: '01JE2ELECTURE0000000000002' }, [S(3)]: { state: 'needs_refresh', taskId: 33 } } });
  const page = await open(listUrl(), 'body[data-ready="1"]');
  const hdr = page.locator('lectureflow-ui[data-kind="header"]');
  await hdr.locator('.ip-btn').waitFor({ timeout: 10000 });
  await hdr.locator('.ip-btn').click();
  const bulk = page.locator('lectureflow-ui[data-kind="card"]');
  await bulk.locator('.lf-card[data-phase="idle"]').waitFor({ timeout: 10000 });
  assert.equal((await bulk.locator('#bulk-summary').textContent()).trim(), '共 6 节：2 节已导入（按会话与标题去重），0 节正在刮削，3 节将发送');
  assert.equal((await bulk.locator('#bulk-refresh-note').textContent()).trim(), '1 节需要刷新，不能在这里勾选：请点列表中那一行的“发送至 LectureFlow”续传。');
  assert.equal(await bulk.locator(`.bulk-row[data-session-id="${S(3)}"] input`).isDisabled(), true);
  await H.shoot(page, 'inpage-bulk-refresh', null, { fullPage: false, clip: { x: 0, y: 0, width: 1280, height: 800 } });
  await page.close();
});

test('list, LectureFlow not running: the page’s own row check never starts it; a click does, and the rows follow', async () => {
  await listLectureFlow({ mode: 'stopped' });
  const page = await open(listUrl(), 'body[data-ready="1"]');
  await page.waitForSelector(`tr[id="${S(6)}"] lectureflow-ui[data-kind="row"]`, { timeout: 10000 });
  // the passive hello was answered "not running": no states, no check, nothing started
  for (let i = 0; i < 50 && (await H.hellos(worker)).length === 0; i++) await page.waitForTimeout(100);
  await page.waitForTimeout(500);
  const passive = await H.hellos(worker);
  assert.ok(passive.length >= 1);
  assert.ok(passive.every((m) => m.noLaunch === true), 'every hello the page caused says noLaunch');
  assert.equal(await H.launches(worker), 0);
  assert.deepEqual((await received()).filter((m) => m.type === 'check'), []);
  assert.equal(await page.locator('[data-row-state]').count(), 0);
  for (const n of [1, 2, 3, 4, 5, 6]) assert.match(await rowHost(page, n, 'row').locator('.lf-root').textContent(), /发送至 LectureFlow/, `row ${n}`);
  for (const n of [1, 2, 3]) assert.equal(await rowHost(page, n, 'check').locator('input').isDisabled(), false);
  // the user clicks the button for the whole page: that may start LectureFlow
  const hdr = page.locator('lectureflow-ui[data-kind="header"]');
  await hdr.locator('.ip-btn').click();
  const bulk = page.locator('lectureflow-ui[data-kind="card"]');
  await bulk.locator('.lf-card[data-phase="idle"]').waitFor({ timeout: 10000 });
  assert.equal(await H.launches(worker), 1);
  const active = (await H.hellos(worker)).slice(passive.length);
  assert.equal(active.length, 1);
  assert.equal('noLaunch' in active[0], false);
  assert.match((await bulk.locator('#bulk-summary').textContent()).trim(), /共 6 节：2 节已导入/);
  // closing the card checks the rows again: now they show LectureFlow's states
  await bulk.locator('#ip-close').click();
  await bulk.waitFor({ state: 'detached' });
  await page.waitForSelector(`tr[id="${S(2)}"] [data-row-state="imported"]`, { timeout: 10000 });
  await page.waitForSelector(`tr[id="${S(3)}"] [data-row-state="downloading"]`, { timeout: 10000 });
  assert.equal(await H.launches(worker), 1);
  await page.close();
});

test('list inside a Canvas iframe (Panopto’s embedded layout): check boxes, the selection bar, 仅音频', async () => {
  await listLectureFlow({ nextTaskId: 90 });
  const page = await open(`http://canvas.lf-e2e.test:${port()}/canvas/list.html`);
  const frame = page.frameLocator('iframe[name="tool_content"]');
  await frame.locator(`tr[id="${S(6)}"] lectureflow-ui[data-kind="row"]`).waitFor({ timeout: 10000 });
  // the light toolbar: the button goes right before Subscribe too
  assert.equal(await frame.locator('lectureflow-ui[data-kind="header"]').evaluate((h) => h.nextElementSibling.className), 'subscribe-button');
  const before = listInfoRequests().length;
  await rowHost(frame, 1, 'check').locator('input').check();
  await rowHost(frame, 6, 'check').locator('input').check();
  const bar = frame.locator('lectureflow-ui[data-kind="bar"]');
  await bar.waitFor({ timeout: 5000 });
  assert.match(await bar.locator('.ip-bar').textContent(), /已选 2 节.*清除.*发送选中的 2 节/);
  assert.equal(await bar.evaluate((h) => getComputedStyle(h).position), 'fixed');
  assert.ok(await bar.locator('#ip-bar-send').isVisible());
  await H.shoot(page, 'inpage-canvas-selection', null, { fullPage: false });
  await bar.locator('#ip-bar-send').click();
  const bulk = frame.locator('lectureflow-ui[data-kind="card"]');
  await bulk.locator('.lf-card[data-phase="idle"]').waitFor({ timeout: 10000 });
  assert.deepEqual(await bulk.locator('#bulk-rows .bulk-row').evaluateAll((xs) => xs.map((x) => x.dataset.sessionId)), [S(1), S(6)]);
  await bulk.locator('#presets [data-preset="audio"]').click();
  await clickArmed(bulk.locator('#primary'));
  await clickArmed(bulk.locator('#override-confirm'));
  await bulk.locator('.lf-card[data-phase="done"]').waitFor({ timeout: 15000 });
  assert.deepEqual(listInfoRequests().slice(before), [S(1), S(6)]);
  const enqs = (await received()).filter((m) => m.type === 'enqueue');
  assert.deepEqual(enqs.map((m) => [m.sessionId, m.tracks.map((t) => `${t.role}:${t.kind}:${t.format}`).join()]), [[S(1), 'primary-audio:audio:hls'], [S(6), 'primary-audio:audio:hls']]);
  await page.close();
});

test('a Canvas iframe taller than the window: the selection bar sits under the toolbar', async () => {
  await listLectureFlow();
  const page = await open(`http://canvas.lf-e2e.test:${port()}/canvas/list.html?tall=1`);
  const frame = page.frameLocator('iframe[name="tool_content"]');
  await frame.locator(`tr[id="${S(1)}"] lectureflow-ui[data-kind="check"]`).waitFor({ timeout: 10000 });
  await page.waitForTimeout(300);
  await rowHost(frame, 1, 'check').locator('input').check();
  const bar = frame.locator('lectureflow-ui[data-kind="bar"]');
  await bar.waitFor({ timeout: 5000 });
  const placed = await bar.evaluate((h) => ({ position: h.style.position, after: h.previousElementSibling && h.previousElementSibling.className, before: h.nextElementSibling && h.nextElementSibling.tagName }));
  assert.deepEqual(placed, { position: 'sticky', after: 'css-embed-toolbar', before: 'TABLE' });
  await page.close();
});

test('Embed.aspx in a Canvas page: an icon button in the title; the card detects within that frame', async () => {
  server.panopto.downloadUrl = '/Panopto/Podcast/Download/whole.mp4';
  await H.fakeLectureFlow(worker, { nextTaskId: 120 });
  const page = await open(`http://canvas.lf-e2e.test:${port()}/canvas/embed.html`);
  const frame = page.frameLocator('iframe[name="tool_content"]');
  await frame.locator('body[data-ready="1"]').waitFor({ timeout: 10000 });
  const btn = frame.locator('h1#title lectureflow-ui[data-kind="player"] .ip-btn.compact');
  await btn.waitFor({ timeout: 10000 });
  assert.equal(await btn.getAttribute('aria-label'), '发送至 LectureFlow');
  assert.equal(await btn.getAttribute('title'), '发送至 LectureFlow');
  await btn.click();
  const card = frame.locator('lectureflow-ui[data-kind="card"]');
  await card.locator('.lf-card[data-view="ready"]').waitFor({ timeout: 15000 });
  assert.match(await card.locator('#lecture').textContent(), /CSC1003\/L01\/01/);
  await H.shoot(page, 'inpage-embed', null, { fullPage: false });
  await clickArmed(card.locator('#primary'));
  await card.locator('.lf-card[data-view="queued"]').waitFor({ timeout: 10000 });
  const enq = (await received()).find((m) => m.type === 'enqueue');
  assert.equal(enq.sessionId, PANOPTO.deliveryId);
  assert.deepEqual(enq.tracks.map((t) => t.role), ['podcast']);
  assert.equal(enq.referer.startsWith(`http://${PANOPTO.host}:`), true, 'the embed page, not the LMS page');
  server.panopto.downloadUrl = null;
  await page.close();
});

test('"对这个平台不再提醒" in the bulk reminder: later bulk sends and the player card go without it', async () => {
  const QUIET = '检测到该媒体流或受讲师或大学政策限制。LectureFlow 仅将您有权访问的媒体流拉取至本地用于个人无障碍学习（转录/翻译）。请确保您遵守学校规定。';
  const remembered = () => worker.evaluate(async () => (await chrome.storage.local.get('restrictedRemembered')).restrictedRemembered || null);
  const bulkSend = async () => {
    const page = await open(listUrl(), 'body[data-ready="1"]');
    const hdr = page.locator('lectureflow-ui[data-kind="header"]');
    await hdr.locator('.ip-btn').waitFor({ timeout: 10000 });
    await hdr.locator('.ip-btn').click();
    const bulk = page.locator('lectureflow-ui[data-kind="card"]');
    await bulk.locator('.lf-card[data-phase="idle"]').waitFor({ timeout: 10000 });
    await bulk.locator('#primary').click();
    return { page, bulk };
  };
  assert.equal(await remembered(), null, 'nothing remembered by default');

  // 1. The reminder offers the check box, unticked; ticked and confirmed.
  await listLectureFlow({ nextTaskId: 140 });
  let { page, bulk } = await bulkSend();
  await bulk.locator('#override').waitFor({ timeout: 15000 });
  assert.match(await bulk.locator('#override-remember-line').textContent(), /对这个平台（Panopto）不再提醒/);
  assert.equal(await bulk.locator('#override-remember').isChecked(), false);
  await bulk.locator('#override-remember').check();
  await H.shoot(page, 'inpage-bulk-remember', null, { fullPage: false, clip: { x: 0, y: 0, width: 1280, height: 800 } });
  await bulk.locator('#override-confirm').click();
  await bulk.locator('.lf-card[data-phase="done"]').waitFor({ timeout: 15000 });
  let enqs = (await received()).filter((m) => m.type === 'enqueue');
  assert.deepEqual(enqs.map((m) => [m.sessionId, m.overrideDisabled]), [[S(1), false], [S(6), true]]);
  const stored = await remembered();
  assert.deepEqual(Object.keys(stored), ['panopto'], 'the platform only');
  assert.match(stored.panopto, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/);
  assert.ok(!JSON.stringify(stored).includes(COURSES[0].id) && !JSON.stringify(stored).includes(S(6)), 'no course, no lecture');
  await page.close();

  // 2. The next bulk send: no reminder; the notice without its last sentence.
  await listLectureFlow({ nextTaskId: 150 });
  ({ page, bulk } = await bulkSend());
  await bulk.locator('.lf-card[data-phase="done"]').waitFor({ timeout: 15000 });
  assert.equal(await bulk.locator('#override').count(), 0, 'no reminder');
  enqs = (await received()).filter((m) => m.type === 'enqueue');
  assert.deepEqual(enqs.map((m) => [m.sessionId, m.overrideDisabled]), [[S(1), false], [S(6), true]]);
  assert.equal((await bulk.locator('#bulk-warning').textContent()).trim(), QUIET);
  assert.match(await bulk.locator('#remembered-note').textContent(), /你已对 Panopto 关闭这个提醒，可以在扩展设置里恢复/);
  await page.close();

  // 3. The player's card on a restricted recording: nothing chosen at first, no reminder.
  server.panopto.downloadUrl = null;
  await H.fakeLectureFlow(worker, { nextTaskId: 160 });
  page = await open(`http://${PANOPTO.host}:${port()}/Panopto/Pages/Viewer.aspx?id=${PANOPTO.deliveryId}`, 'body[data-ready="1"]');
  await page.locator('lectureflow-ui[data-kind="player"] .ip-btn').click({ timeout: 10000 });
  const card = page.locator('lectureflow-ui[data-kind="card"]');
  await card.locator('.lf-card[data-view="blocked"]').waitFor({ timeout: 15000 });
  assert.equal((await card.locator('#reason').textContent()).trim(), QUIET);
  assert.equal(await card.locator('#presets [aria-pressed="true"]').count(), 0, 'SPEC-ext: 默认不勾选');
  await card.locator('#presets [data-preset="main"]').click();
  await card.locator('#primary').click();
  await card.locator('.lf-card[data-view="queued"]').waitFor({ timeout: 10000 });
  assert.equal(await card.locator('#override').count(), 0);
  const enq = (await received()).find((m) => m.type === 'enqueue');
  assert.equal(enq.overrideDisabled, true);
  assert.equal(enq.platform, 'panopto');
  await page.close();
  await worker.evaluate(() => chrome.storage.local.remove('restrictedRemembered'));
});

test('generic sites get no in-page button', async () => {
  const page = await open(`http://course.lf-e2e.test:${port()}/mp4.html`);
  await page.waitForTimeout(800);
  assert.equal(await page.locator('lectureflow-ui').count(), 0);
  await page.close();
});

test('nothing reached any host but the local test server', () => {
  const hosts = new Set(server.requests.map((r) => r.host));
  for (const h of hosts) assert.ok(['127.0.0.1', 'course.lf-e2e.test', 'canvas.lf-e2e.test', PANOPTO.host].includes(h), h);
});
