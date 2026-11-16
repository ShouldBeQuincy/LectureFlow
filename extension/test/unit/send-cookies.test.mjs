// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Which cookies a send hands to LectureFlow (security audit AUD3 中-1, the
// auditor's probe P1 made a test): src/background/detector.js and
// actions.js driven as a whole against a fake `browser` whose cookie jar
// answers cookies.getAll as a browser does (domain, path, partitions, host
// permission). A page must not get another granted site's cookies handed
// over by naming its address; the page's own site, and a player embedded
// from another granted site whose requests the browser itself made, still
// send what the browser would. Marker values only; nothing leaves the
// process.

import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { registerHooks } from 'node:module';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
// the build's target: Chrome, unless a test sets globalThis.__lfBuildTarget (the Firefox build)
const STUBS = {
  '../build-info.js': "export const BUILD = Object.freeze({ get target() { return globalThis.__lfBuildTarget || 'chrome'; }, version: '0.0.0', e2e: false });",
  '../ui/tokens.js': "export const tokens = { light: { '--accent': '#000000', '--text-on-accent': '#ffffff' }, dark: {} };",
  '../vendor/browser-polyfill.js': '',
};
registerHooks({
  resolve(specifier, context, next) {
    if (Object.hasOwn(STUBS, specifier) && String(context.parentURL || '').includes('/src/')) {
      return { url: `data:text/javascript,${encodeURIComponent(STUBS[specifier])}`, shortCircuit: true };
    }
    return next(specifier, context);
  },
});

// ------------------------------------------------------------ the fake browser

const world = { tabUrl: '', frames: [], granted: new Set(), jar: [], cookieCalls: [], native: [] };
const listeners = {};

function fakePort() {
  const onMsg = [];
  return {
    onMessage: { addListener: (f) => onMsg.push(f) },
    onDisconnect: { addListener: () => {} },
    disconnect() {},
    postMessage(m) {
      world.native.push(JSON.parse(JSON.stringify(m)));
      let reply;
      if (m.type === 'hello') reply = { type: 'hello', id: m.id, ok: true, protocol: 1, app: { version: '9.9.9' }, courses: [{ id: 'c1', name: 'Course' }] };
      else if (m.type === 'check') reply = { type: 'check', id: m.id, ok: true, items: [] };
      else if (m.type === 'enqueue') reply = { type: 'enqueue', id: m.id, ok: true, taskId: 7, position: 1 };
      else reply = { type: m.type, id: m.id, ok: true, taskId: 7, state: 'queued' };
      setTimeout(() => onMsg.forEach((f) => f(reply)), 0);
    },
  };
}

const domainMatch = (c, host) => {
  const d = c.domain.replace(/^\./, '');
  return c.hostOnly ? host === d : host === d || host.endsWith(`.${d}`);
};

globalThis.browser = {
  runtime: { getURL: (rel) => `ext://lf/${rel}`, getManifest: () => ({ version: '0.0.0' }), id: 'lf', connectNative: () => fakePort() },
  i18n: { getUILanguage: () => 'en', getMessage: (k) => k },
  storage: {
    local: { get: async () => ({}), set: async () => {}, remove: async () => {} },
    session: { get: async () => ({}), set: async () => {}, remove: async () => {} },
  },
  permissions: {
    contains: async ({ origins }) => origins.every((o) => world.granted.has(o)),
    getAll: async () => ({ origins: [...world.granted] }),
  },
  tabs: { get: async () => ({ url: world.tabUrl }) },
  action: { setBadgeText: async () => {}, setBadgeBackgroundColor: async () => {}, setBadgeTextColor: async () => {} },
  webRequest: {
    onHeadersReceived: { addListener: (f) => (listeners.headers = f) },
    onBeforeRequest: { addListener: (f) => (listeners.main = f) },
  },
  // The browser: only for hosts the extension has access to; unpartitioned
  // only without partitionKey, one partition with a key, every partition
  // and the unpartitioned ones with {} (Chrome 119+).
  cookies: {
    getAll: async (details) => {
      world.cookieCalls.push(JSON.parse(JSON.stringify(details)));
      const u = new URL(details.url);
      if (!world.granted.has(`${u.protocol}//${u.hostname}/*`)) return [];
      const fits = world.jar.filter((c) => domainMatch(c, u.hostname) && u.pathname.startsWith(c.path || '/'));
      if (!('partitionKey' in details)) return fits.filter((c) => !c.partitionKey).map((c) => structuredClone(c));
      const key = details.partitionKey || {};
      if (!key.topLevelSite) return fits.map((c) => structuredClone(c));
      return fits.filter((c) => c.partitionKey && c.partitionKey.topLevelSite === key.topLevelSite).map((c) => structuredClone(c));
    },
  },
  scripting: {
    async executeScript({ target, func }) {
      if (func.name === 'scanFrame') {
        const pick = target.allFrames ? world.frames : world.frames.filter((f) => target.frameIds.includes(f.frameId));
        return pick.map((f) => ({ frameId: f.frameId, documentId: f.documentId, result: structuredClone(f.result) }));
      }
      if (func.name === 'frameTitle') return [{ frameId: 0, result: { url: world.tabUrl, title: 'Week 3 lecture recording', history: [], ancestors: [] } }];
      if (func.name === 'pageFetch') return [{ frameId: 0, result: [{ ok: false, status: 0, contentType: '', size: -1, text: '', error: 'TypeError' }] }];
      throw new Error(`unexpected injection ${func.name}`);
    },
  },
};

const extFile = (rel) => (rel === 'profiles/index.json' ? JSON.stringify({ profiles: ['panopto.json'] }) : fs.readFileSync(path.join(root, rel), 'utf8'));
globalThis.fetch = async (url) => {
  const u = String(url);
  if (u.startsWith('ext://lf/')) return new Response(extFile(u.slice('ext://lf/'.length)), { status: 200 });
  return new Response(null, { status: 403 });
};

const { detectTab, cookieHostOrigins } = await import('../../src/background/detector.js');
const { sendResult, refreshResult } = await import('../../src/background/actions.js');
const requests = await import('../../src/background/requests.js');
requests.install(() => {});

// ------------------------------------------------------------ helpers

let nextTab = 1;
beforeEach(() => {
  Object.assign(world, { tabUrl: '', frames: [], granted: new Set(), jar: [], cookieCalls: [], native: [] });
});

// documentId: Chrome names each document; Firefox gives none
const frame = (frameId, url, elements = [], documentId = undefined) => ({ frameId, documentId, result: { url, elements, resources: [], reads: [], encryptedEvents: 0, observed: [], opaque: [], docs: [], ancestors: [] } });
const video = (s) => ({ tag: 'video', src: s, currentSrc: s, sources: [], mediaKeys: false, videoWidth: 1280, videoHeight: 720, duration: 3000, readyState: 4 });
// the browser saw the player's request (frameId: the frame that made it). As
// Firefox reports it by default: no document id, the document's address
// (documentUrl) — the frame's own document unless `extra` says otherwise;
// Chrome's shape is { documentId, initiator }.
const frameUrlOf = (frameId) => world.frames.find((f) => f.frameId === frameId)?.result.url || '';
const saw = (tabId, url, frameId, extra = {}) =>
  listeners.headers({ tabId, frameId, method: 'GET', type: 'media', statusCode: 206, url, responseHeaders: [{ name: 'Content-Type', value: 'video/mp4' }, { name: 'Content-Range', value: 'bytes 0-1/123456' }], documentUrl: frameUrlOf(frameId), ...extra });
const cookie = (name, domain, extra = {}) => ({ name, value: `MARKER-${name}`, domain, path: '/', secure: true, httpOnly: true, hostOnly: !domain.startsWith('.'), sameSite: 'lax', ...extra });
const getAllUrls = () => world.cookieCalls.map((c) => c.url);
const sentCookies = (type) => world.native.filter((m) => m.type === type).flatMap((m) => m.cookies.map((c) => c.name));

async function sendFirst(tabId, type = 'enqueue') {
  const r = await detectTab(tabId, { force: true });
  assert.equal(r.status, 'ok');
  assert.ok(r.tracks.length > 0, 'a track was found');
  const where = { tabId, topUrl: r.pageUrl, storeId: '' };
  const out = type === 'enqueue' ? await sendResult(r, { trackIds: [r.tracks[0].id], courseId: 'c1' }, where) : await refreshResult(r, { trackIds: [r.tracks[0].id] }, where);
  assert.equal(out.ok, true, JSON.stringify(out));
  return r;
}

// ------------------------------------------------------------ tests

const PAGE_X = 'https://lecture-mirror.attacker.example/week3.html';
const LMS = 'lms.victim-uni.example';

test('a page names an address on another granted site: none of that site’s cookies are read or sent, even once the browser loaded it (AUD3 中-1, P1)', async () => {
  for (const target of [`https://${LMS}/pluginfile.php/99/recording.mp4`, `https://${LMS}/mod/forum/post.php?delete=42&confirm=1&x=.mp4`, `https://${LMS}/mod/forum/post.php?delete=42&confirm=1`]) {
    for (const type of ['enqueue', 'refresh']) {
      const tab = nextTab++;
      Object.assign(world, { tabUrl: PAGE_X, frames: [frame(0, PAGE_X, [video(target)])], cookieCalls: [], native: [] });
      // the user granted the LMS some time ago; its session cookie, and one for embedding (SameSite=None)
      world.granted = new Set([`https://${LMS}/*`]);
      world.jar = [cookie('MoodleSession', LMS), cookie('LtiSession', LMS, { sameSite: 'no_restriction' }), cookie('Partitioned', LMS, { sameSite: 'no_restriction', partitionKey: { topLevelSite: 'https://attacker.example' } })];
      // the page's <video> made the browser request it, and the browser reported it
      saw(tab, target, 0);
      await sendFirst(tab, type);
      assert.deepEqual(sentCookies(type), [], `${type} of ${target}: no cookie`);
      assert.deepEqual(getAllUrls(), [], `${type} of ${target}: the LMS cookies were not even read`);
    }
  }
});

test('the page’s own site: its cookies as before, SameSite=Lax too; a sibling host’s only where a cookie is shared with the page', async () => {
  const page = `https://${LMS}/mod/page/view.php?id=1`;
  const own = `https://${LMS}/pluginfile.php/99/recording.mp4`;
  let tab = nextTab++;
  Object.assign(world, { tabUrl: page, frames: [frame(0, page, [video(own)])] });
  world.granted = new Set([`https://${LMS}/*`]);
  world.jar = [cookie('MoodleSession', LMS), cookie('LtiSession', LMS, { sameSite: 'no_restriction' }), cookie('Elsewhere', 'other.victim-uni.example')];
  await sendFirst(tab);
  assert.deepEqual(sentCookies('enqueue').sort(), ['LtiSession', 'MoodleSession']);
  assert.deepEqual([...new Set(getAllUrls())], [own], 'the track’s URL only, as before (unpartitioned, then the tab’s partition)');

  // www page, media on a sibling host: a cookie for the whole site goes; the media host's own
  // too, now that that site-wide cookie proves the two hosts one site (SEC46 低-3, D3; before:
  // CloudFront-Policy only) — the page is one the user granted
  const www = 'https://www.victim-uni.example/lectures/3';
  const media = 'https://media.victim-uni.example/v/3.mp4';
  tab = nextTab++;
  Object.assign(world, { tabUrl: www, frames: [frame(0, www, [video(media)])], cookieCalls: [], native: [] });
  world.granted = new Set(['https://www.victim-uni.example/*', 'https://media.victim-uni.example/*']);
  world.jar = [cookie('CloudFront-Policy', '.victim-uni.example'), cookie('MediaOnly', 'media.victim-uni.example'), cookie('MediaNone', 'media.victim-uni.example', { sameSite: 'no_restriction' })];
  saw(tab, media, 0);
  await sendFirst(tab);
  assert.deepEqual(sentCookies('enqueue'), ['CloudFront-Policy', 'MediaOnly', 'MediaNone']);
  // the same page, not granted (opened with activeTab only): the media host's own stay
  tab = nextTab++;
  Object.assign(world, { tabUrl: www, frames: [frame(0, www, [video(media)])], cookieCalls: [], native: [] });
  world.granted = new Set(['https://media.victim-uni.example/*']);
  saw(tab, media, 0);
  await sendFirst(tab);
  assert.deepEqual(sentCookies('enqueue'), ['CloudFront-Policy']);
});

test('an LMS page embedding another granted site’s player: the cookies the browser itself sent from that frame (SameSite=None, the tab’s partition)', async () => {
  const lms = 'https://lms.uni.example/course/1';
  const embed = 'https://video.example.com/embed/1';
  const mp4 = 'https://video.example.com/media/1.mp4';
  const tab = nextTab++;
  Object.assign(world, { tabUrl: lms, frames: [frame(0, lms, []), frame(5, embed, [video(mp4)])] });
  world.granted = new Set(['https://lms.uni.example/*', 'https://video.example.com/*']);
  world.jar = [
    cookie('VideoNone', 'video.example.com', { sameSite: 'no_restriction' }),
    cookie('VideoLax', 'video.example.com'),
    cookie('VideoPart', 'video.example.com', { sameSite: 'no_restriction', partitionKey: { topLevelSite: 'https://uni.example' } }),
    cookie('VideoPartElsewhere', 'video.example.com', { sameSite: 'no_restriction', partitionKey: { topLevelSite: 'https://attacker.example' } }),
  ];
  saw(tab, mp4, 5);
  const r = await sendFirst(tab);
  assert.deepEqual(r.frameHosts, { 0: 'lms.uni.example', 5: 'video.example.com' });
  assert.deepEqual(sentCookies('enqueue').sort(), ['VideoNone', 'VideoPart']);
});

test('the Firefox build: an embedded player’s unpartitioned SameSite=None cookie stays, the tab’s partition goes (SEC46 低-2)', async () => {
  const lms = 'https://lms.uni.example/course/1';
  const embed = 'https://video.example.com/embed/1';
  const mp4 = 'https://video.example.com/media/1.mp4';
  const jar = [
    cookie('VideoNone', 'video.example.com', { sameSite: 'no_restriction' }),
    cookie('VideoPart', 'video.example.com', { sameSite: 'no_restriction', partitionKey: { topLevelSite: 'https://uni.example' } }),
  ];
  for (const target of ['firefox', 'chrome']) {
    globalThis.__lfBuildTarget = target;
    try {
      const tab = nextTab++;
      Object.assign(world, { tabUrl: lms, frames: [frame(0, lms), frame(5, embed, [video(mp4)])], cookieCalls: [], native: [] });
      world.granted = new Set(['https://lms.uni.example/*', 'https://video.example.com/*']);
      world.jar = jar;
      saw(tab, mp4, 5);
      const r = await sendFirst(tab);
      assert.equal(r.thirdPartyCookies, target === 'firefox' ? 'partitioned' : 'shared');
      assert.deepEqual(sentCookies('enqueue').sort(), target === 'firefox' ? ['VideoPart'] : ['VideoNone', 'VideoPart'], target);
    } finally {
      delete globalThis.__lfBuildTarget;
    }
  }
  // the page's own site (rule ①) is the same in both builds
  globalThis.__lfBuildTarget = 'firefox';
  try {
    const page = `https://${LMS}/mod/page/view.php?id=1`;
    const own = `https://${LMS}/pluginfile.php/99/recording.mp4`;
    const tab = nextTab++;
    Object.assign(world, { tabUrl: page, frames: [frame(0, page, [video(own)])], cookieCalls: [], native: [] });
    world.granted = new Set([`https://${LMS}/*`]);
    world.jar = [cookie('MoodleSession', LMS), cookie('LtiSession', LMS, { sameSite: 'no_restriction' })];
    await sendFirst(tab);
    assert.deepEqual(sentCookies('enqueue').sort(), ['LtiSession', 'MoodleSession']);
  } finally {
    delete globalThis.__lfBuildTarget;
  }
});

test('a Panopto page the profile does not cover, sent by generic detection: no Panopto cookie read (privacy statement)', async () => {
  const home = 'https://tenant.cloud.panopto.eu/Panopto/Pages/Home.aspx';
  const mp4 = 'https://tenant.cloud.panopto.eu/Panopto/Content/intro.mp4';
  const tab = nextTab++;
  Object.assign(world, { tabUrl: home, frames: [frame(0, home, [video(mp4)])] });
  world.granted = new Set(['https://tenant.cloud.panopto.eu/*']);
  world.jar = [cookie('.ASPXAUTH', 'tenant.cloud.panopto.eu')];
  saw(tab, mp4, 0);
  const r = await sendFirst(tab);
  assert.equal(r.profile, null, 'generic detection');
  assert.deepEqual(sentCookies('enqueue'), []);
  assert.deepEqual(getAllUrls(), []);
});

test('a Panopto page with media on another Panopto host: the popup does not offer it as a cookie site, whose cookies a send never reads (SEC46 信-2)', async () => {
  const home = 'https://tenant.cloud.panopto.eu/Panopto/Pages/Home.aspx';
  const mp4 = 'https://other.cloud.panopto.eu/Panopto/Content/intro.mp4';
  const tab = nextTab++;
  Object.assign(world, { tabUrl: home, frames: [frame(0, home, [video(mp4)])] });
  world.granted = new Set(['https://tenant.cloud.panopto.eu/*', 'https://other.cloud.panopto.eu/*']);
  world.jar = [cookie('.ASPXAUTH', '.cloud.panopto.eu')];
  saw(tab, mp4, 0);
  const r = await sendFirst(tab);
  assert.equal(r.profile, null, 'generic detection');
  assert.deepEqual(cookieHostOrigins(r), [], 'before: https://other.cloud.panopto.eu/*');
  assert.deepEqual(getAllUrls(), []);
});

// ------------------------------------------------------------ request time, not scan time (SEC46 低-1)

const TARGET = `https://${LMS}/pluginfile.php/99/private-recording.mp4`;
// with a cookie for the whole site, which would prove a sibling host on the LMS's site (SEC46
// 低-3): the request-time checks above hold all the same
const moodle = () => [cookie('MoodleSession', LMS, { sameSite: 'no_restriction' }), cookie('StrictCsrf', LMS, { sameSite: 'strict' }), cookie('Unspecified', LMS, { sameSite: 'unspecified' }), cookie('SiteWide', '.victim-uni.example', { sameSite: 'no_restriction' })];
const listedUrls = (r) => (r.named ? [...r.named.tracks, ...r.named.others] : r.tracks).map((t) => t.url);

async function sendIfListed(tabId, url) {
  const r = await detectTab(tabId, { force: true });
  assert.equal(r.status, 'ok');
  const t = (r.named ? [...r.named.tracks, ...r.named.others] : r.tracks).find((x) => x.url === url);
  if (t) assert.equal((await sendResult(r, { trackIds: [t.id], courseId: 'c1' }, { tabId, topUrl: r.pageUrl, storeId: '' })).ok, true);
  return r;
}

test('SEC46 A1 (Firefox: no document ids): a frame requested a granted site’s address as another site’s document, then moved to that site; none of its cookies', async () => {
  // the record: frame 5, made by the attacker's document; at the click frame 5 shows LMS/robots.txt
  let tab = nextTab++;
  Object.assign(world, { tabUrl: PAGE_X, frames: [frame(0, PAGE_X), frame(5, `https://${LMS}/robots.txt`)] });
  world.granted = new Set([`https://${LMS}/*`]);
  world.jar = moodle();
  saw(tab, TARGET, 5, { documentUrl: 'https://evil-frame.attacker.example/a.html' });
  let r = await sendIfListed(tab, TARGET);
  assert.deepEqual(listedUrls(r), [], 'the record belongs to a document no longer there: not the frame’s');
  assert.deepEqual(sentCookies('enqueue'), []);
  assert.deepEqual(getAllUrls(), []);
  // the same, and the attacker's top page also writes the address: a track, but no cookie
  tab = nextTab++;
  Object.assign(world, { tabUrl: PAGE_X, frames: [frame(0, PAGE_X, [video(TARGET)]), frame(5, `https://${LMS}/robots.txt`)], cookieCalls: [], native: [] });
  saw(tab, TARGET, 5, { documentUrl: 'https://evil-frame.attacker.example/a.html' });
  r = await sendIfListed(tab, TARGET);
  assert.deepEqual(listedUrls(r), [TARGET]);
  assert.deepEqual(sentCookies('enqueue'), [], 'before: MoodleSession (SameSite=None, HttpOnly)');
  assert.deepEqual(getAllUrls(), []);
});

test('SEC46 A2 (Chrome): the same with document ids is dropped as before', async () => {
  const tab = nextTab++;
  Object.assign(world, { tabUrl: PAGE_X, frames: [frame(0, PAGE_X, [], 'D0'), frame(5, `https://${LMS}/robots.txt`, [], 'D5b')] });
  world.granted = new Set([`https://${LMS}/*`]);
  world.jar = moodle();
  saw(tab, TARGET, 5, { documentId: 'D5a', initiator: 'https://evil-frame.attacker.example', documentUrl: undefined });
  const r = await sendIfListed(tab, TARGET);
  assert.deepEqual(listedUrls(r), []);
  assert.deepEqual(getAllUrls(), []);
});

const DELETE = `https://${LMS}/mod/forum/post.php?delete=42&confirm=1`;
const sawRedirect = (tabId, frameId, extra) =>
  listeners.headers({ tabId, frameId, method: 'GET', type: 'media', statusCode: 302, url: DELETE, responseHeaders: [{ name: 'Location', value: `https://${LMS}/login/index.php` }], ...extra });

test('SEC46 B1 (Firefox): an unscanned cross-site frame on a granted LMS page requests an LMS address; none of the LMS cookies', async () => {
  const page = `https://${LMS}/mod/forum/discuss.php?d=1`;
  const tab = nextTab++;
  Object.assign(world, { tabUrl: page, frames: [frame(0, page)] });
  world.granted = new Set([`https://${LMS}/*`]);
  world.jar = moodle();
  sawRedirect(tab, 9, { documentUrl: 'https://evil.example/student-embed.html' });
  const r = await sendIfListed(tab, DELETE);
  assert.deepEqual(listedUrls(r), [], 'frame 9 was not scanned: its records are nobody’s');
  assert.deepEqual(sentCookies('enqueue'), [], 'before: MoodleSession, StrictCsrf, Unspecified');
  assert.deepEqual(getAllUrls(), []);
});

test('SEC46 B2 (Chrome): the same with the unscanned frame’s document id is dropped as before', async () => {
  const page = `https://${LMS}/mod/forum/discuss.php?d=1`;
  const tab = nextTab++;
  Object.assign(world, { tabUrl: page, frames: [frame(0, page, [], 'D0')] });
  world.granted = new Set([`https://${LMS}/*`]);
  world.jar = moodle();
  sawRedirect(tab, 9, { documentId: 'D9', initiator: 'https://evil.example' });
  const r = await sendIfListed(tab, DELETE);
  assert.deepEqual(listedUrls(r), []);
  assert.deepEqual(getAllUrls(), []);
});

test('rule ① needs a frame on the page’s site that referenced or requested the track: a cross-site frame’s request on an LMS page gets no LMS cookie (Chrome and Firefox)', async () => {
  const page = `https://${LMS}/mod/forum/discuss.php?d=1`;
  const evil = 'https://evil.example/student-embed.html';
  for (const shape of ['chrome', 'firefox']) {
    // the cross-site frame was scanned (the user granted evil.example too): its record is its own
    const tab = nextTab++;
    const chrome = shape === 'chrome';
    Object.assign(world, { tabUrl: page, frames: [frame(0, page, [], chrome ? 'D0' : undefined), frame(9, evil, [], chrome ? 'D9' : undefined)], cookieCalls: [], native: [] });
    world.granted = new Set([`https://${LMS}/*`, 'https://evil.example/*']);
    world.jar = moodle();
    sawRedirect(tab, 9, chrome ? { documentId: 'D9', initiator: 'https://evil.example' } : { documentUrl: evil });
    const r = await sendIfListed(tab, DELETE);
    assert.deepEqual(listedUrls(r), [DELETE], shape);
    assert.deepEqual(sentCookies('enqueue'), [], `${shape}: the browser sends a cross-site frame’s request no Lax or Strict cookie (before: all three)`);
    assert.deepEqual(getAllUrls(), [], shape);
  }
});

test('legitimate shapes in Firefox’s report form (documentUrl, no document id) send as before', async () => {
  // the page's own media, requested by the page
  const page = `https://${LMS}/mod/page/view.php?id=1`;
  const own = `https://${LMS}/pluginfile.php/99/recording.mp4`;
  let tab = nextTab++;
  Object.assign(world, { tabUrl: page, frames: [frame(0, page, [video(own)])] });
  world.granted = new Set([`https://${LMS}/*`]);
  world.jar = [cookie('MoodleSession', LMS), cookie('LtiSession', LMS, { sameSite: 'no_restriction' })];
  saw(tab, own, 0);
  await sendFirst(tab);
  assert.deepEqual(sentCookies('enqueue').sort(), ['LtiSession', 'MoodleSession']);
  // an embedded player's request from its own document: rule ② as before (Chrome build)
  const lms = 'https://lms.uni.example/course/1';
  const embed = 'https://video.example.com/embed/1';
  const mp4 = 'https://video.example.com/media/1.mp4';
  tab = nextTab++;
  Object.assign(world, { tabUrl: lms, frames: [frame(0, lms), frame(5, embed, [video(mp4)])], cookieCalls: [], native: [] });
  world.granted = new Set(['https://lms.uni.example/*', 'https://video.example.com/*']);
  world.jar = [cookie('VideoNone', 'video.example.com', { sameSite: 'no_restriction' }), cookie('VideoLax', 'video.example.com')];
  saw(tab, mp4, 5);
  await sendFirst(tab);
  assert.deepEqual(sentCookies('enqueue'), ['VideoNone']);
  // the same player whose request Firefox ties to the LMS document instead: not the frame's
  tab = nextTab++;
  Object.assign(world, { tabUrl: lms, frames: [frame(0, lms), frame(5, embed, [video(mp4)])], cookieCalls: [], native: [] });
  saw(tab, mp4, 5, { documentUrl: lms });
  await sendFirst(tab);
  assert.deepEqual(sentCookies('enqueue'), []);
});

test('the same player whose media the browser never requested (only written in the frame): no cookie read', async () => {
  const lms = 'https://lms.uni.example/course/1';
  const embed = 'https://video.example.com/embed/1';
  const mp4 = 'https://video.example.com/media/2.mp4';
  const tab = nextTab++;
  Object.assign(world, { tabUrl: lms, frames: [frame(0, lms, []), frame(5, embed, [video(mp4)])] });
  world.granted = new Set(['https://lms.uni.example/*', 'https://video.example.com/*']);
  world.jar = [cookie('VideoNone', 'video.example.com', { sameSite: 'no_restriction' })];
  await sendFirst(tab);
  assert.deepEqual(sentCookies('enqueue'), []);
  assert.deepEqual(getAllUrls(), []);
});

test('the frame detection runs in has the host the browser gave, not the one its scan reports (SEC46 信-3, M11)', async () => {
  // tabs.get says the attacker's page; the scan of frame 0 claims an LMS address
  const tab = nextTab++;
  Object.assign(world, { tabUrl: PAGE_X, frames: [frame(0, `https://${LMS}/mod/page/view.php?id=1`, [video(TARGET)])] });
  world.granted = new Set([`https://${LMS}/*`]);
  world.jar = moodle();
  saw(tab, TARGET, 0, { documentUrl: PAGE_X });
  const r = await sendIfListed(tab, TARGET);
  assert.deepEqual(r.frameHosts, { 0: 'lecture-mirror.attacker.example' });
  assert.deepEqual(sentCookies('enqueue'), []);
  assert.deepEqual(getAllUrls(), []);
});

// ------------------------------------------------------------ the track host's own cookies (SEC46 低-3)

async function sendOne(tabId) {
  const r = await detectTab(tabId, { force: true });
  assert.equal(r.status, 'ok');
  const t = r.tracks[0];
  assert.ok(t, 'a track');
  assert.equal((await sendResult(r, { trackIds: [t.id], courseId: 'c1' }, { tabId, topUrl: r.pageUrl, storeId: '' })).ok, true);
  return r;
}

test('SEC46 D3: a granted page, media on a sibling host with its own host-only cookie: it goes when a site-wide cookie proves the two hosts one site', async () => {
  const page = 'https://lecture.uni.example/watch/1';
  const media = 'https://stream.uni.example/v/1.mp4';
  const tab = nextTab++;
  Object.assign(world, { tabUrl: page, frames: [frame(0, page, [video(media)], 'D0')] });
  world.granted = new Set(['https://lecture.uni.example/*', 'https://stream.uni.example/*']);
  world.jar = [cookie('StreamSession', 'stream.uni.example'), cookie('SsoSite', '.uni.example')];
  saw(tab, media, 0, { documentId: 'D0', initiator: 'https://lecture.uni.example', documentUrl: undefined });
  await sendOne(tab);
  assert.deepEqual(sentCookies('enqueue').sort(), ['SsoSite', 'StreamSession'], 'before: SsoSite only');
});

test('SEC46 D1: an LMS page, a granted player whose CDN subdomain serves the track: the CDN’s own host-only SameSite=None cookie goes (Chrome build); Firefox the tab’s partition only', async () => {
  const lms = 'https://lms.uni.example/course/1';
  const embed = 'https://player.video.example/embed/1';
  const mp4 = 'https://cdn.video.example/m/1.mp4';
  const jar = [
    cookie('ParentNone', '.video.example', { sameSite: 'no_restriction' }),
    cookie('CdnHostOnlyNone', 'cdn.video.example', { sameSite: 'no_restriction' }),
    cookie('CdnHostOnlyLax', 'cdn.video.example'),
    cookie('ParentUnspecified', '.video.example', { sameSite: 'unspecified' }),
    cookie('CdnPart', 'cdn.video.example', { sameSite: 'no_restriction', partitionKey: { topLevelSite: 'https://uni.example' } }),
  ];
  for (const target of ['chrome', 'firefox']) {
    globalThis.__lfBuildTarget = target;
    try {
      const tab = nextTab++;
      Object.assign(world, { tabUrl: lms, frames: [frame(0, lms), frame(5, embed, [video(mp4)])], cookieCalls: [], native: [] });
      world.granted = new Set(['https://player.video.example/*', 'https://cdn.video.example/*']);
      world.jar = jar;
      saw(tab, mp4, 5);
      await sendOne(tab);
      assert.deepEqual(sentCookies('enqueue').sort(), target === 'chrome' ? ['CdnHostOnlyNone', 'CdnPart', 'ParentNone'] : ['CdnPart'], `${target} (before, Chrome: CdnPart, ParentNone)`);
    } finally {
      delete globalThis.__lfBuildTarget;
    }
  }
});

test('SEC46 D2: the same player before its CDN is granted: the popup offers the CDN; the send reads nothing until then', async () => {
  const lms = 'https://lms.uni.example/course/1';
  const embed = 'https://player.video.example/embed/1';
  const mp4 = 'https://cdn.video.example/m/1.mp4';
  const tab = nextTab++;
  Object.assign(world, { tabUrl: lms, frames: [frame(0, lms), frame(5, embed, [video(mp4)])] });
  world.granted = new Set(['https://player.video.example/*']);
  world.jar = [cookie('ParentNone', '.video.example', { sameSite: 'no_restriction' })];
  // not granted: the browser does not report the player's request (no record)
  const r = await sendOne(tab);
  assert.deepEqual(cookieHostOrigins(r), ['https://cdn.video.example/*'], 'before: none (the first send from the LMS page could not be set up)');
  assert.deepEqual(sentCookies('enqueue'), []);
  assert.deepEqual(getAllUrls(), []);
});

test('SEC46 D5: media on another site that needs a SameSite=None cookie: still none (by design)', async () => {
  const page = 'https://lecture.uni.example/watch/1';
  const media = 'https://media.uni-cdn.example/v/1.mp4';
  const tab = nextTab++;
  Object.assign(world, { tabUrl: page, frames: [frame(0, page, [video(media)])] });
  world.granted = new Set(['https://lecture.uni.example/*', 'https://media.uni-cdn.example/*']);
  world.jar = [cookie('CdnNone', '.uni-cdn.example', { sameSite: 'no_restriction' }), cookie('CdnHost', 'media.uni-cdn.example', { sameSite: 'no_restriction' })];
  saw(tab, media, 0);
  await sendOne(tab);
  assert.deepEqual(sentCookies('enqueue'), []);
  assert.deepEqual(getAllUrls(), []);
});

test('the proof cannot hand over another site’s own cookies: public suffixes of two labels, a parent further up, two labels below, an attacker page the user did not grant', async () => {
  const shapes = [
    // the page, the track, the jar (what a browser can hold), the granted origins
    ['https://evil.github.io/x', 'https://victim.github.io/v.mp4', [cookie('VictimHostOnly', 'victim.github.io', { sameSite: 'no_restriction' }), cookie('VictimStrict', 'victim.github.io', { sameSite: 'strict' })], ['https://evil.github.io/*', 'https://victim.github.io/*']],
    ['https://a.co.uk/x', 'https://b.co.uk/v.mp4', [cookie('BHostOnly', 'b.co.uk', { sameSite: 'no_restriction' })], ['https://a.co.uk/*', 'https://b.co.uk/*']],
    // bucket sites under a public suffix (s3.storage.example), a cookie on the provider's own domain above it
    ['https://bucketx.s3.storage.example/x', 'https://victim.s3.storage.example/v.mp4', [cookie('VictimBucket', 'victim.s3.storage.example'), cookie('Provider', '.storage.example')], ['https://bucketx.s3.storage.example/*', 'https://victim.s3.storage.example/*']],
    // a page two labels below the common parent
    ['https://www.lms.uni.example/x', 'https://media.uni.example/v.mp4', [cookie('MediaOwn', 'media.uni.example'), cookie('Site', '.uni.example')], ['https://www.lms.uni.example/*', 'https://media.uni.example/*']],
    // a page on the same site the user did not grant (a student's site, opened with activeTab)
    ['https://club.uni.example/x', 'https://lms.uni.example/v.mp4', [cookie('SiteWideLax', '.uni.example'), cookie('__Host-LmsSession', 'lms.uni.example', { sameSite: 'no_restriction' })], ['https://lms.uni.example/*']],
  ];
  const expected = [[], [], ['Provider'], ['Site'], ['SiteWideLax']];
  for (const [i, [page, track, jar, granted]] of shapes.entries()) {
    const tab = nextTab++;
    Object.assign(world, { tabUrl: page, frames: [frame(0, page, [video(track)])], cookieCalls: [], native: [], jar, granted: new Set(granted) });
    saw(tab, track, 0);
    await sendOne(tab);
    assert.deepEqual(sentCookies('enqueue'), expected[i], `${page} → ${track}`);
  }
  // the same student's site, granted: it is on the LMS's site, and the browser sends a request
  // from it the LMS's host-only cookies too (SEC46 table 3 row 5: no more than the page could
  // already make the browser send)
  const tab = nextTab++;
  const club = 'https://club.uni.example/x';
  Object.assign(world, { tabUrl: club, frames: [frame(0, club, [video('https://lms.uni.example/v.mp4')])], cookieCalls: [], native: [], jar: shapes[4][2], granted: new Set(['https://club.uni.example/*', 'https://lms.uni.example/*']) });
  saw(tab, 'https://lms.uni.example/v.mp4', 0);
  await sendOne(tab);
  assert.deepEqual(sentCookies('enqueue'), ['SiteWideLax', '__Host-LmsSession']);
});
