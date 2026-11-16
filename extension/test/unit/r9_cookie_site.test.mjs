// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Two tightenings of cookie rule ① (src/lib/send.js cookiePlan,
// cookieAllowed; SEC48 低-1 and 低-2), driven through detector.js and
// actions.js against a fake `browser` as in send-cookies.test.mjs:
//   - EX-01: with the tab's top-level site as the browser computed it
//     (Chrome 132+ cookies.getPartitionKey of the top frame, which answers
//     only with host access to the site's own origin: SEC51 低-1), the track
//     host's own host-only cookies go only when the track host, the page
//     and the frames that asked are all on that site — on top of the cookie
//     proof, never instead of it; without that API, when it fails or
//     answers another shape or too late, exactly as before;
//   - EX-02: a frame counts as on the page's site only when every ancestor
//     it reported (location.ancestorOrigins) is too; a frame that reported
//     none (Firefox before 153, the top frame) as before, and in the Firefox
//     build also one that reported a hidden ancestor ("null"; SEC51 低-2).
// Neither may ever hand over a cookie that would not go without it: every
// scenario below is also sent without the new input, and the cookies with
// it are a subset. Marker values only; nothing leaves the process.

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

// partitionKey: undefined — no cookies.getPartitionKey (Firefox, Chrome
// before 132); else (details) => answer, called for every getPartitionKey
// (the send's own call for the top frame and src/lib/partition.js's):
// site(…) as Chrome 156 answers (SEC51 低-1, SEC51b), answer(…) any shape at all.
const world = { tabUrl: '', frames: [], granted: new Set(), jar: [], cookieCalls: [], native: [], partitionKey: undefined, calls: [] };
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

const cookies = {
  getAll: async (details) => {
    world.calls.push('getAll');
    world.cookieCalls.push(JSON.parse(JSON.stringify(details)));
    const u = new URL(details.url);
    if (!world.granted.has(`${u.protocol}//${u.hostname}/*`)) return [];
    const fits = world.jar.filter((c) => domainMatch(c, u.hostname) && u.pathname.startsWith(c.path || '/'));
    if (!('partitionKey' in details)) return fits.filter((c) => !c.partitionKey).map((c) => structuredClone(c));
    const key = details.partitionKey || {};
    if (!key.topLevelSite) return fits.map((c) => structuredClone(c));
    return fits.filter((c) => c.partitionKey && c.partitionKey.topLevelSite === key.topLevelSite).map((c) => structuredClone(c));
  },
};
Object.defineProperty(cookies, 'getPartitionKey', {
  enumerable: true,
  get() {
    if (world.partitionKey === undefined) return undefined;
    return (details) => {
      world.calls.push(`getPartitionKey:${JSON.stringify(details)}`);
      return world.partitionKey(details);
    };
  },
});

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
  cookies,
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

const S = await import('../../src/lib/send.js');
const { detectTab, viewOf, cookieHostOrigins } = await import('../../src/background/detector.js');
const A = await import('../../src/background/actions.js');
const requests = await import('../../src/background/requests.js');
requests.install(() => {});

// ------------------------------------------------------------ helpers

let nextTab = 1;
beforeEach(() => {
  Object.assign(world, { tabUrl: '', frames: [], granted: new Set(), jar: [], cookieCalls: [], native: [], partitionKey: undefined, calls: [] });
});

// documentId: Chrome names each document, Firefox gives none; ancestors:
// location.ancestorOrigins as the scan read it (nearest first, the top last)
const frame = (frameId, url, elements = [], documentId = undefined, ancestors = []) => ({ frameId, documentId, result: { url, elements, resources: [], reads: [], encryptedEvents: 0, observed: [], opaque: [], docs: [], ancestors } });
const video = (s) => ({ tag: 'video', src: s, currentSrc: s, sources: [], mediaKeys: false, videoWidth: 1280, videoHeight: 720, duration: 3000, readyState: 4 });
const cookie = (name, domain, extra = {}) => ({ name, value: `MARKER-${name}`, domain, path: '/', secure: true, httpOnly: true, hostOnly: !domain.startsWith('.'), sameSite: 'lax', ...extra });
const grant = (...hosts) => new Set(hosts.map((h) => `https://${h}/*`));
// what the browser reports to webRequest: Chrome { documentId, initiator }, Firefox { documentUrl }
const chromeReq = (documentId, docUrl) => ({ documentId, initiator: new URL(docUrl).origin, documentUrl: undefined });
const firefoxReq = (docUrl) => ({ documentUrl: docUrl });
const saw = (tabId, url, frameId, extra) =>
  listeners.headers({ tabId, frameId, method: 'GET', type: 'media', statusCode: 206, url, responseHeaders: [{ name: 'Content-Type', value: 'video/mp4' }, { name: 'Content-Range', value: 'bytes 0-1/123456' }], ...extra });
const sawRedirect = (tabId, url, frameId, extra) => listeners.headers({ tabId, frameId, method: 'GET', type: 'media', statusCode: 302, url, responseHeaders: [{ name: 'Location', value: 'https://login.example/' }], ...extra });
// Chrome (SEC51 低-1, measured on Chromium 156): the answer needs host access to the top-level
// site's own origin ("https://uni.example/*"), not the page's ("https://lms.uni.example/*"), and
// for a sub-frame (src/lib/partition.js asks for those) to the frame's own site's origin too
// (SEC51b: a cross-site lti.vendor.test frame was refused for "vendor.test"). A frame on the
// top-level site's host or under it is on that site; another frame's site is taken as its host's
// last two labels — right for the sub-frame hosts this file has asked about (player.video.example
// → video.example), not in general (victim.github.io is a site of its own).
const frameSiteOf = (frameId, s) => {
  const f = world.frames.find((x) => x.frameId === frameId);
  const host = f ? new URL(f.result.url).hostname : '';
  const top = new URL(s).hostname;
  if (!host) return '';
  if (host === top || host.endsWith(`.${top}`)) return s;
  return `https://${host.split('.').slice(-2).join('.')}`;
};
const site = (s) => async (details) => {
  if (!world.granted.has(`${s}/*`)) throw new Error(`No host permissions for cookies at url: "${s}/".`);
  if (details && details.frameId !== 0) {
    const own = frameSiteOf(details.frameId, s);
    if (!own) throw new Error('Frame not found');
    if (!world.granted.has(`${own}/*`)) throw new Error(`No host permissions for cookies at url: "${own}/".`);
  }
  return { partitionKey: { topLevelSite: s, hasCrossSiteAncestor: false } };
};
// a browser that answers this, whatever it is (shapes Chrome does not give)
const answer = (s) => () => ({ partitionKey: { topLevelSite: s, hasCrossSiteAncestor: false } });
// a scenario whose user also granted these hosts (the bare site, for getPartitionKey)
const alsoGranted = (setup, ...hosts) => (tab) => {
  const url = setup(tab);
  for (const h of hosts) world.granted.add(`https://${h}/*`);
  return url;
};
const sentNames = () => world.native.filter((m) => m.type === 'enqueue').flatMap((m) => m.cookies.map((c) => c.name)).sort();
const enqueues = () => world.native.filter((m) => m.type === 'enqueue');

/**
 * One scenario, sent once: setup(tab) fills the world and returns the URL
 * of the track to send. Returns { names, result, enqueue }.
 */
async function sendOnce(setup, { partitionKey, target = 'chrome' } = {}) {
  if (target === 'firefox') globalThis.__lfBuildTarget = 'firefox';
  try {
    const tab = nextTab++;
    Object.assign(world, { tabUrl: '', frames: [], granted: new Set(), jar: [], cookieCalls: [], native: [], partitionKey, calls: [] });
    const url = setup(tab);
    const r = await detectTab(tab, { force: true });
    assert.equal(r.status, 'ok');
    const t = (r.named ? [...r.named.tracks, ...r.named.others] : r.tracks).find((x) => x.url === url);
    assert.ok(t, `the track ${url} is listed`);
    const out = await A.sendResult(r, { trackIds: [t.id], courseId: 'c1' }, { tabId: tab, topUrl: r.pageUrl, storeId: '' });
    assert.equal(out.ok, true, JSON.stringify(out));
    return { names: sentNames(), result: r, enqueue: enqueues()[0] };
  } finally {
    delete globalThis.__lfBuildTarget;
  }
}

/** sent with the new input ⊆ sent without it (never more than before). */
function assertSubset(withIt, without, label) {
  const extra = withIt.filter((n) => !without.includes(n));
  assert.deepEqual(extra, [], `${label}: never more than without (with ${JSON.stringify(withIt)}, without ${JSON.stringify(without)})`);
}

// ------------------------------------------------------------ the shapes (SEC48 tables)

// D3: a granted page, media on a sibling host with its own host-only Lax cookie, a site-wide proof
const D3 = (tab) => {
  const page = 'https://lecture.uni.example/watch/1';
  const media = 'https://stream.uni.example/v/1.mp4';
  world.tabUrl = page;
  world.frames = [frame(0, page, [video(media)], 'D0')];
  world.granted = grant('lecture.uni.example', 'stream.uni.example');
  world.jar = [cookie('StreamSession', 'stream.uni.example'), cookie('SsoSite', '.uni.example')];
  saw(tab, media, 0, chromeReq('D0', page));
  return media;
};
// D1: an LMS page, a granted player frame (its ancestor the LMS), the track on the player's CDN subdomain
const D1 = (target) => (tab) => {
  const lms = 'https://lms.uni.example/course/1';
  const embed = 'https://player.video.example/embed/1';
  const mp4 = 'https://cdn.video.example/m/1.mp4';
  world.tabUrl = lms;
  const chrome = target === 'chrome';
  world.frames = [frame(0, lms, [], chrome ? 'D0' : undefined), frame(5, embed, [video(mp4)], chrome ? 'D5' : undefined, chrome ? ['https://lms.uni.example'] : [])];
  world.granted = grant('player.video.example', 'cdn.video.example');
  world.jar = [
    cookie('ParentNone', '.video.example', { sameSite: 'no_restriction' }),
    cookie('CdnHostOnlyNone', 'cdn.video.example', { sameSite: 'no_restriction' }),
    cookie('CdnHostOnlyLax', 'cdn.video.example'),
    cookie('ParentUnspecified', '.video.example', { sameSite: 'unspecified' }),
    cookie('CdnPart', 'cdn.video.example', { sameSite: 'no_restriction', partitionKey: { topLevelSite: 'https://uni.example' } }),
  ];
  saw(tab, mp4, 5, chrome ? chromeReq('D5', embed) : firefoxReq(embed));
  return mp4;
};
// R3: the attacker's page a.parent.example (granted) names v.parent.example (granted), whose own
// Strict cookie a .parent.example cookie "proves" on the page's site
const R3 = (tab) => {
  const top = 'https://a.parent.example/p';
  const track = 'https://v.parent.example/v.mp4';
  world.tabUrl = top;
  world.frames = [frame(0, top, [video(track)], 'D0')];
  world.granted = grant('a.parent.example', 'v.parent.example');
  world.jar = [cookie('ParentCookie', '.parent.example'), cookie('VSession', 'v.parent.example', { sameSite: 'strict' })];
  saw(tab, track, 0, chromeReq('D0', top));
  return track;
};
const LMS = 'lms.victim-uni.example';
const DELETE = `https://${LMS}/mod/forum/post.php?delete=42&confirm=1`;
const moodle = () => [cookie('MoodleSession', LMS, { sameSite: 'no_restriction' }), cookie('StrictCsrf', LMS, { sameSite: 'strict' }), cookie('Unspecified', LMS, { sameSite: 'unspecified' }), cookie('SiteLax', '.victim-uni.example')];
// SEC48 probe B3: the LMS top page > an unscanned cross-site frame > a scanned LMS frame 5 with
// attacker content that requests an LMS address; ancestors: what frame 5 reported
const B3 = (target, ancestors) => (tab) => {
  const top = `https://${LMS}/mod/forum/discuss.php?d=1`;
  const nested = `https://${LMS}/mod/page/view.php?id=8`;
  const chrome = target === 'chrome';
  world.tabUrl = top;
  world.frames = [frame(0, top, [], chrome ? 'D0' : undefined), frame(5, nested, [video(DELETE)], chrome ? 'D5' : undefined, ancestors)];
  world.granted = grant(LMS);
  world.jar = moodle();
  sawRedirect(tab, DELETE, 5, chrome ? chromeReq('D5', nested) : firefoxReq(nested));
  return DELETE;
};

// ------------------------------------------------------------ EX-01: the pure parts

test('topSiteOf: the host of an http(s) top-level site the browser gave; anything else is no site', () => {
  assert.equal(typeof S.topSiteOf, 'function', 'send.js exports topSiteOf');
  const of = (topLevelSite) => S.topSiteOf({ partitionKey: { topLevelSite, hasCrossSiteAncestor: false } });
  assert.equal(of('https://uni.example'), 'uni.example');
  assert.equal(of('https://UNI.Example.'), 'uni.example', 'lower case, no trailing dot');
  assert.equal(of('http://10.0.0.5'), '10.0.0.5');
  assert.equal(of('https://uni.example:8443'), 'uni.example', 'a port says nothing about the host');
  assert.equal(of('https://xn--un-1tb.example'), 'xn--un-1tb.example');
  assert.equal(S.topSiteOf({ partitionKey: { topLevelSite: 'https://uni.example' } }), 'uni.example', 'without hasCrossSiteAncestor');
  for (const bad of [undefined, null, 7, 'https://uni.example', [], {}, { partitionKey: null }, { partitionKey: 'https://uni.example' }, { partitionKey: {} }, { topLevelSite: 'https://uni.example' }]) {
    assert.equal(S.topSiteOf(bad), '', JSON.stringify(bad));
  }
  for (const bad of ['', 'null', 'uni.example', 'chrome-extension://abcdef', 'file:///C:/x', 'data:text/html,x', 'https://uni.example/path', 'https://uni.example/?q=1', 'https://uni.example/#x', 'https://u:p@uni.example', `https://${'a'.repeat(400)}.example`, 7, null, {}]) {
    assert.equal(of(bad), '', JSON.stringify(bad));
  }
});

test('cookieAllowed with topSite: the track host’s own host-only cookie only when the track host, the page and the frames are on it — never anything it would not allow without', () => {
  const answer = S.withAnswerDomains([
    { name: 'media', domain: 'media.uni.example', hostOnly: true, sameSite: 'lax' },
    { name: 'mediaNone', domain: 'media.uni.example', hostOnly: true, sameSite: 'no_restriction' },
    { name: 'sso', domain: '.uni.example', hostOnly: false, sameSite: 'lax' },
    { name: 'ssoNone', domain: '.uni.example', hostOnly: false, sameSite: 'no_restriction' },
    { name: 'other', domain: 'other.uni.example', hostOnly: true, sameSite: 'lax' },
  ]);
  // rule ① alone (no frame on record: rule ② is not part of this change)
  const base = { url: 'https://media.uni.example/v.mp4', all: false, topHost: 'www.uni.example', siteHosts: ['www.uni.example'], frameHosts: [], thirdParty: 'shared', grantedHosts: ['www.uni.example'] };
  const allowed = (entry) => answer.filter((c) => S.cookieAllowed(c, entry)).map((c) => c.name);
  assert.deepEqual(allowed(base), ['media', 'mediaNone', 'sso', 'ssoNone']);
  assert.deepEqual(allowed({ ...base, topSite: 'uni.example' }), ['media', 'mediaNone', 'sso', 'ssoNone'], 'on the site: as before');
  // the browser says the page is a site of its own (a public suffix's child): the host-only ones stay
  assert.deepEqual(allowed({ ...base, topSite: 'www.uni.example' }), ['sso', 'ssoNone'], 'the track host is not on the page’s site');
  assert.deepEqual(allowed({ ...base, topSite: 'elsewhere.example' }), ['sso', 'ssoNone'], 'an answer for another page');
  assert.deepEqual(allowed({ ...base, topSite: 'uni.example', siteHosts: ['www.uni.example', 'x.other.example'] }), ['sso', 'ssoNone'], 'a frame that asked is off the site');
  // and the proof is still needed: topSite never stands in for it
  const unproven = answer.map((c) => ({ ...c }));
  assert.deepEqual(unproven.filter((c) => S.cookieAllowed(c, { ...base, topSite: 'uni.example' })).map((c) => c.name), ['sso', 'ssoNone'], 'no answer’s marks: no host-only cookie, topSite or not');
  assert.deepEqual(allowed({ ...base, topSite: 'uni.example', grantedHosts: [] }), ['sso', 'ssoNone'], 'a page the user did not grant: as before');
  // never more, over a grid of entries
  const tops = ['www.uni.example', 'media.uni.example', 'uni.example', 'x.www.uni.example', ''];
  const sitesOf = [[], ['www.uni.example'], ['media.uni.example'], ['www.uni.example', 'club.uni.example'], ['other.example']];
  const topSites = ['uni.example', 'www.uni.example', 'media.uni.example', 'example', 'elsewhere.example', '10.0.0.5'];
  for (const topHost of tops)
    for (const siteHosts of sitesOf)
      for (const grantedHosts of [[], [topHost, ...siteHosts].filter(Boolean)])
        for (const thirdParty of ['shared', 'partitioned'])
          for (const frameHosts of [[], ['player.uni.example'], ['www.uni.example']]) {
            const entry = { ...base, topHost, siteHosts, grantedHosts, thirdParty, frameHosts };
            const without = allowed(entry);
            for (const topSite of topSites) assertSubset(allowed({ ...entry, topSite }), without, JSON.stringify({ ...entry, topSite }));
          }
});

test('cookiePlan with topSite: it rides along only when given; a frame off it is not on the page’s site; never a host more', () => {
  const page = 'https://lms.uni.example/course/1';
  const own = { status: 'ok', pageUrl: page, request: {}, frameHosts: { 0: 'lms.uni.example', 5: 'lms.uni.example' }, thirdPartyCookies: 'shared', grantedHosts: ['lms.uni.example'] };
  const t = { id: 't1', url: 'https://lms.uni.example/v.mp4', sources: ['element', 'webrequest'], frameIds: [5] };
  const plain = S.cookiePlan(own, [t], page);
  assert.deepEqual(plain, [{ url: t.url, all: false, topHost: 'lms.uni.example', siteHosts: ['lms.uni.example'], frameHosts: ['lms.uni.example'], thirdParty: 'shared', grantedHosts: ['lms.uni.example'] }], 'as before');
  assert.deepEqual(S.cookiePlan(own, [t], page, { topSite: 'uni.example' }), [{ ...plain[0], topSite: 'uni.example' }]);
  assert.deepEqual(S.cookiePlan(own, [t], page, { topSite: 'UNI.example.' }), [{ ...plain[0], topSite: 'uni.example' }]);
  assert.deepEqual(S.cookiePlan(own, [t], page, { topSite: '' }), plain, 'no site: as before, no field');
  assert.deepEqual(S.cookiePlan(own, [t], page, { topSite: 7 }), plain, 'not a string: no site');
  // the browser says the top page is on another site: no frame is on the page's site; rule ② stays
  assert.deepEqual(S.cookiePlan(own, [t], page, { topSite: 'elsewhere.example' }), [{ ...plain[0], topHost: '', siteHosts: [], topSite: 'elsewhere.example' }]);
  // a profile's declared host, a page that only names an address: as before
  assert.deepEqual(S.cookiePlan({ ...own, profile: { id: 'p', origins: ['https://*.uni.example/*'] } }, [t], page, { topSite: 'uni.example' }), [{ url: t.url, all: true }]);
  const evil = 'https://attacker.example/x';
  assert.deepEqual(S.cookiePlan({ ...own, pageUrl: evil, frameHosts: { 0: 'attacker.example' } }, [{ ...t, frameIds: [0], sources: ['element'] }], evil, { topSite: 'attacker.example' }), []);
  // never a host more
  for (const topSite of ['', 'uni.example', 'lms.uni.example', 'elsewhere.example', 'example', 'x.lms.uni.example']) {
    const now = S.cookiePlan(own, [t], page, { topSite });
    assert.ok(now.length <= plain.length, 'no URL more');
    for (const k of ['siteHosts', 'frameHosts', 'grantedHosts']) for (const e of now) assertSubset(e[k], plain[0][k], `${k} ${topSite}`);
  }
});

test('EX-02 cookiePlan: a frame under an ancestor off the page’s site is not on it (with topSite: under it); never a host more', () => {
  const page = 'https://lms.uni.example/course/1';
  const own = { status: 'ok', pageUrl: page, request: {}, frameHosts: { 0: 'lms.uni.example', 5: 'lms.uni.example' }, thirdPartyCookies: 'shared', grantedHosts: ['lms.uni.example'] };
  const t = { id: 't1', url: 'https://lms.uni.example/v.mp4', sources: ['element', 'webrequest'], frameIds: [5] };
  const plain = S.cookiePlan(own, [t], page);
  assert.deepEqual(plain, [{ url: t.url, all: false, topHost: 'lms.uni.example', siteHosts: ['lms.uni.example'], frameHosts: ['lms.uni.example'], thirdParty: 'shared', grantedHosts: ['lms.uni.example'] }], 'as before');
  assert.deepEqual(S.cookiePlan(own, [t], page, { topSite: 'uni.example' }), [{ ...plain[0], topSite: 'uni.example' }]);
  assert.deepEqual(S.cookiePlan(own, [t], page, { topSite: '' }), plain, 'no site: as before, no field');
  // what the scan's location.ancestorOrigins becomes (detector.js result.frameAncestors)
  assert.equal(typeof S.ancestorHosts, 'function', 'send.js exports ancestorHosts');
  assert.deepEqual(S.ancestorHosts(['https://Evil.Example:8443', 'null', 'about:blank', 'chrome-extension://x', 7, 'https://lms.uni.example']), ['evil.example', '', '', '', '', 'lms.uni.example']);
  for (const none of [undefined, null, [], 'https://lms.uni.example', {}]) assert.equal(S.ancestorHosts(none), null, JSON.stringify(none));
  assert.equal(S.ancestorHosts(new Array(70).fill('https://a.example')).length, S.MAX_ANCESTORS, 'cut to what a scan keeps');
  // frame 5 reported its ancestors: the LMS itself — as before
  assert.deepEqual(S.cookiePlan({ ...own, frameAncestors: { 5: ['lms.uni.example'] } }, [t], page), plain);
  // a cross-site frame in between (SEC48 低-2): frame 5 is not on the page's site; rule ② stays
  const nested = { ...own, frameAncestors: { 5: ['evil.example', 'lms.uni.example'] } };
  assert.deepEqual(S.cookiePlan(nested, [t], page), [{ ...plain[0], topHost: '', siteHosts: [], grantedHosts: ['lms.uni.example'] }]);
  // an ancestor that is no http(s) origin ("null": a sandboxed frame, a no-referrer embedding), too long a chain
  for (const chain of [[''], ['', 'lms.uni.example'], new Array(64).fill('lms.uni.example')]) {
    assert.deepEqual(S.cookiePlan({ ...own, frameAncestors: { 5: chain } }, [t], page)[0].siteHosts, [], JSON.stringify(chain).slice(0, 60));
  }
  // the Firefox build (thirdPartyCookies "partitioned", or a result that does not say): a hidden
  // ancestor says nothing, the frame counts as one that reported none (SEC51 低-2); a real one still does
  for (const thirdPartyCookies of ['partitioned', undefined]) {
    const ff = { ...own, thirdPartyCookies };
    const asBefore = S.cookiePlan(ff, [t], page)[0].siteHosts;
    assert.deepEqual(asBefore, ['lms.uni.example']);
    for (const chain of [[''], ['', 'lms.uni.example'], ['', 'evil.example', 'lms.uni.example']]) {
      assert.deepEqual(S.cookiePlan({ ...ff, frameAncestors: { 5: chain } }, [t], page)[0].siteHosts, asBefore, `${thirdPartyCookies} ${JSON.stringify(chain)}`);
    }
    assert.deepEqual(S.cookiePlan({ ...ff, frameAncestors: { 5: ['evil.example', 'lms.uni.example'] } }, [t], page)[0].siteHosts, [], 'a real cross-site ancestor');
  }
  // with topSite, "on the site" means under it: an ancestor that only shares labels is not
  const sibling = { ...own, frameAncestors: { 5: ['club.uni.example', 'lms.uni.example'] } };
  assert.deepEqual(S.cookiePlan(sibling, [t], page)[0].siteHosts, ['lms.uni.example'], 'without topSite: may share a cookie');
  assert.deepEqual(S.cookiePlan(sibling, [t], page, { topSite: 'lms.uni.example' })[0].siteHosts, [], 'the browser says the LMS is a site of its own');
  // a result without frameAncestors (a listed session's, Firefox before 153): as before
  assert.deepEqual(S.cookiePlan({ ...own, frameAncestors: undefined }, [t], page), plain);
  assert.deepEqual(S.cookiePlan({ ...own, frameAncestors: { 5: [] } }, [t], page), plain, 'an empty list says nothing');
  // never a host more, never a frame more
  for (const frameAncestors of [undefined, { 5: ['evil.example', 'lms.uni.example'] }, { 5: ['lms.uni.example'] }, { 0: ['x'], 5: [''] }]) {
    for (const topSite of ['', 'uni.example', 'lms.uni.example', 'elsewhere.example', 'example']) {
      const before = S.cookiePlan({ ...own, frameAncestors: undefined }, [t], page);
      const now = S.cookiePlan({ ...own, frameAncestors }, [t], page, { topSite });
      assert.ok(now.length <= before.length, 'no URL more');
      for (const e of now) {
        const b = before.find((x) => x.url === e.url);
        assert.ok(b, 'an URL read before');
        for (const k of ['siteHosts', 'frameHosts', 'grantedHosts']) assertSubset(e[k], b[k], `${k} ${JSON.stringify({ frameAncestors, topSite })}`);
        assert.ok(!e.topHost || e.topHost === b.topHost);
      }
    }
  }
});

// ------------------------------------------------------------ EX-01: sends

test('EX-01 D3 (Chrome 132+): with the page granted as usual Chrome refuses the site (no access to https://uni.example/), so as before; asked once, for the top frame, before any cookie is read', async () => {
  const without = await sendOnce(D3);
  assert.deepEqual(without.names, ['SsoSite', 'StreamSession'], 'without getPartitionKey: as before');
  const withIt = await sendOnce(D3, { partitionKey: site('https://uni.example') });
  assert.deepEqual(withIt.names, ['SsoSite', 'StreamSession']);
  const first = world.calls.findIndex((c) => c === 'getAll');
  assert.ok(first > 0, 'cookies were read after it');
  assert.ok(world.calls.slice(0, first).includes(`getPartitionKey:${JSON.stringify({ tabId: withIt.result.tabId, frameId: 0 })}`), JSON.stringify(world.calls));
});

test('EX-01 D3 where the user also granted the bare site uni.example: Chrome answers it, and the sibling host’s own cookie still goes', async () => {
  const got = await sendOnce(alsoGranted(D3, 'uni.example'), { partitionKey: site('https://uni.example') });
  assert.deepEqual(got.names, ['SsoSite', 'StreamSession']);
});

test('EX-01 C1 (SEC48): evil.github.io is its own site, which Chrome gives with the page’s own grant — an impossible .github.io cookie no longer passes for a proof', async () => {
  const shape = (tab) => {
    const top = 'https://evil.github.io/x';
    const track = 'https://victim.github.io/v.mp4';
    world.tabUrl = top;
    world.frames = [frame(0, top, [video(track)], 'D0')];
    world.granted = grant('evil.github.io', 'victim.github.io');
    world.jar = [cookie('VictimHostOnly', 'victim.github.io'), cookie('ImpossiblePslDomain', '.github.io')];
    saw(tab, track, 0, chromeReq('D0', top));
    return track;
  };
  const without = (await sendOnce(shape)).names;
  assert.deepEqual(without, ['ImpossiblePslDomain', 'VictimHostOnly'], 'as SEC48 probe C1');
  const withIt = (await sendOnce(shape, { partitionKey: site('https://evil.github.io') })).names;
  assert.deepEqual(withIt, ['ImpossiblePslDomain']);
  assertSubset(withIt, without, 'C1');
});

test('EX-01 D1: the player’s CDN — rule ② as before, with and without getPartitionKey; the Firefox build (none) only the tab’s partition, as before', async () => {
  const chromeWithout = await sendOnce(D1('chrome'));
  assert.deepEqual(chromeWithout.names, ['CdnHostOnlyNone', 'CdnPart', 'ParentNone']);
  const chromeWith = await sendOnce(D1('chrome'), { partitionKey: site('https://uni.example') });
  assert.deepEqual(chromeWith.names, ['CdnHostOnlyNone', 'CdnPart', 'ParentNone']);
  const firefox = await sendOnce(D1('firefox'), { target: 'firefox' });
  assert.deepEqual(firefox.names, ['CdnPart']);
  assert.ok(!world.calls.some((c) => c.startsWith('getPartitionKey')), 'Firefox has none to call');
});

test('EX-01 D1 with the bare site uni.example granted: Chrome answers for the top frame but refuses the player frame (no access to video.example), so partition.js reads every partition and keeps the tab’s site’s — the same cookies (SEC51b)', async () => {
  const before = (await sendOnce(D1('chrome'))).names;
  const parted = () => world.cookieCalls.filter((c) => 'partitionKey' in c).map((c) => c.partitionKey);
  const wide = await sendOnce(alsoGranted(D1('chrome'), 'uni.example'), { partitionKey: site('https://uni.example') });
  assert.deepEqual(wide.names, before);
  const tabId = wide.result.tabId;
  for (const frameId of [0, 5]) assert.ok(world.calls.includes(`getPartitionKey:${JSON.stringify({ tabId, frameId })}`), JSON.stringify(world.calls));
  // what those two calls got, asked again in the same world (grants and frames of this send)
  assert.deepEqual(await world.partitionKey({ tabId, frameId: 0 }), { partitionKey: { topLevelSite: 'https://uni.example', hasCrossSiteAncestor: false } }, 'the top frame: answered');
  await assert.rejects(Promise.resolve().then(() => world.partitionKey({ tabId, frameId: 5 })), /No host permissions for cookies at url: "https:\/\/video\.example\/"/, 'the player frame: refused');
  assert.deepEqual(parted(), [{}], 'the player frame gave no key: the wide read, filtered to the tab’s site');
  // with the player's site granted as well, every frame answers: the exact key, the same cookies
  const exact = await sendOnce(alsoGranted(D1('chrome'), 'uni.example', 'video.example'), { partitionKey: site('https://uni.example') });
  assert.deepEqual(exact.names, before);
  assert.deepEqual(parted().map((k) => k.topLevelSite), ['https://uni.example'], 'the frame’s own key, no wide read');
});

test('EX-01 R3 when the browser makes the attacker’s host a site of its own (PSL “*.parent.example”): the victim’s own Strict cookie stays; without getPartitionKey the residual is as before', async () => {
  const without = await sendOnce(R3);
  assert.deepEqual(without.names, ['ParentCookie', 'VSession'], 'Firefox, Chrome before 132: the residual (SEC48 低-1) as before');
  const withIt = await sendOnce(R3, { partitionKey: site('https://a.parent.example') });
  assert.deepEqual(withIt.names, ['ParentCookie'], 'the parent-domain Lax cookie the attacker’s host receives anyway (SEC48 信-4) — not v.parent.example’s own');
  assertSubset(withIt.names, without.names, 'R3');
});

test('EX-01 R3 when only the victim host is listed (PSL “v.parent.example”): the browser’s top-level site is parent.example, the victim host is under it — the residual stays (the top’s site cannot tell)', async () => {
  const refused = await sendOnce(R3, { partitionKey: site('https://parent.example') });
  assert.deepEqual(refused.names, ['ParentCookie', 'VSession'], 'parent.example not granted: Chrome refuses, as before');
  const withIt = await sendOnce(alsoGranted(R3, 'parent.example'), { partitionKey: site('https://parent.example') });
  assert.deepEqual(withIt.names, ['ParentCookie', 'VSession'], 'answered: kept as it is, a residual, not a regression');
});

test('EX-01: getPartitionKey that throws, rejects, answers another shape or too late is the same as none (the cookie proof alone, as before)', async () => {
  assert.equal(typeof A.setPartitionKeyTimeout, 'function', 'actions.js lets a test shorten the wait');
  A.setPartitionKeyTimeout(50);
  try {
    const without = (await sendOnce(R3)).names;
    assert.deepEqual(without, ['ParentCookie', 'VSession']);
    const late = (ms, answer) => () => new Promise((resolve) => setTimeout(() => resolve(answer), ms));
    const odd = [
      ['throws', () => { throw new Error('No host permissions for cookies at url'); }],
      ['rejects', async () => { throw new Error('Frame not found'); }],
      ['undefined', () => undefined],
      ['null', () => null],
      ['{}', () => ({})],
      ['partitionKey null', () => ({ partitionKey: null })],
      ['no access to the site (Chrome’s refusal)', site('https://parent.example')],
      ['empty site', answer('')],
      ['"null" site', answer('null')],
      ['an extension page', answer('chrome-extension://abcdefghijklmnop')],
      ['a path', answer('https://a.parent.example/x')],
      ['a number', answer(7)],
      // the one answer that would hold the cookie back, after the wait is over
      ['too late', late(300, { partitionKey: { topLevelSite: 'https://a.parent.example', hasCrossSiteAncestor: false } })],
    ];
    for (const [label, fn] of odd) {
      const got = await sendOnce(R3, { partitionKey: fn });
      assert.deepEqual(got.names, without, label);
    }
  } finally {
    A.setPartitionKeyTimeout(3000);
  }
});

test('EX-01: an answer for another site (the tab moved on) puts the page off its own site: rule ① gives nothing — less, never more', async () => {
  const without = (await sendOnce(D3)).names;
  const got = (await sendOnce(D3, { partitionKey: answer('https://elsewhere.example') })).names;
  assert.deepEqual(got, [], 'before: SsoSite, StreamSession');
  assertSubset(got, without, 'another site');
});

test('EX-01: no cookie to read, no cookies API at all — not even getPartitionKey', async () => {
  const page = 'https://lecture-mirror.attacker.example/week3.html';
  const target = 'https://lms.victim-uni.example/pluginfile.php/99/recording.mp4';
  const got = await sendOnce(
    (tab) => {
      world.tabUrl = page;
      world.frames = [frame(0, page, [video(target)], 'D0')];
      world.granted = grant('lms.victim-uni.example');
      world.jar = [cookie('MoodleSession', 'lms.victim-uni.example', { sameSite: 'no_restriction' })];
      saw(tab, target, 0, chromeReq('D0', page));
      return target;
    },
    { partitionKey: site('https://attacker.example') },
  );
  assert.deepEqual(got.names, []);
  assert.deepEqual(world.calls, [], 'neither getAll nor getPartitionKey');
});

test('EX-01, EX-02: the message to LectureFlow is the same with and without the new inputs, and names neither the top-level site nor an ancestor', async () => {
  const keys = ['cookies', 'courseId', 'id', 'lecture', 'maxConnections', 'overrideDisabled', 'platform', 'profile', 'referer', 'sessionId', 'tracks', 'type'];
  const strip = (m) => JSON.parse(JSON.stringify({ ...m, id: 0 }));
  const a = await sendOnce(D3);
  const b = await sendOnce(alsoGranted(D3, 'uni.example'), { partitionKey: site('https://uni.example') });
  assert.deepEqual(Object.keys(b.enqueue).sort(), keys);
  assert.deepEqual(strip(b.enqueue), strip(a.enqueue));
  for (const c of b.enqueue.cookies) assert.deepEqual(Object.keys(c).sort(), ['domain', 'expires', 'hostOnly', 'httpOnly', 'name', 'path', 'secure', 'value']);
  const nested = await sendOnce(B3('chrome', ['https://evil-frame.attacker.example', 'https://lms.victim-uni.example']), { partitionKey: site('https://victim-uni.example') });
  const text = JSON.stringify(world.native);
  assert.ok(!text.includes('topSite') && !text.includes('evil-frame') && !text.includes('frameAncestors'), text.slice(0, 200));
  assert.ok(!('frameAncestors' in viewOf(nested.result)) && !JSON.stringify(viewOf(nested.result)).includes('evil-frame'), 'nor the popup’s view');
});

// ------------------------------------------------------------ EX-02: ancestors

test('EX-02 B3 (Chrome): a frame of the LMS inside a cross-site frame gets no Lax or Strict LMS cookie; SameSite=None by rule ② as before — with and without getPartitionKey', async () => {
  const chain = ['https://evil-frame.attacker.example', `https://${LMS}`];
  const before = await sendOnce(B3('chrome', []));
  assert.deepEqual(before.names, ['MoodleSession', 'SiteLax', 'StrictCsrf', 'Unspecified'], 'no ancestors reported: as before (the residual SEC48 低-2)');
  const now = await sendOnce(B3('chrome', chain));
  assert.deepEqual(now.names, ['MoodleSession'], 'before: all four');
  assert.deepEqual(now.result.frameAncestors, { 5: ['evil-frame.attacker.example', LMS] }, 'the ancestors’ hosts, inside the extension only');
  const withSite = await sendOnce(alsoGranted(B3('chrome', chain), 'victim-uni.example'), { partitionKey: site('https://victim-uni.example') });
  assert.deepEqual(withSite.names, ['MoodleSession']);
  assertSubset(now.names, before.names, 'B3');
});

test('EX-02 in the Chrome build: an ancestor reported as "null" (a sandboxed frame) counts as off the site — Chrome itself sends such a frame no Lax or Strict cookie and does not hide an ancestor for no-referrer (SEC51)', async () => {
  const got = await sendOnce(B3('chrome', ['null', `https://${LMS}`]));
  assert.deepEqual(got.names, ['MoodleSession']);
  const opaque = await sendOnce(B3('chrome', ['about:blank', `https://${LMS}`]));
  assert.deepEqual(opaque.names, ['MoodleSession']);
});

test('EX-02 in the Firefox build: an ancestor reported as "null" says nothing — the frame sends as one that reported no ancestors, as before (SEC51 低-2: a same-site no-referrer embedding)', async () => {
  const before = (await sendOnce(B3('firefox', []), { target: 'firefox' })).names;
  assert.deepEqual(before, ['MoodleSession', 'SiteLax', 'StrictCsrf', 'Unspecified']);
  for (const chain of [['null'], ['null', `https://${LMS}`], ['about:blank', `https://${LMS}`], ['null', 'https://evil-frame.attacker.example', `https://${LMS}`]]) {
    const got = (await sendOnce(B3('firefox', chain), { target: 'firefox' })).names;
    assert.deepEqual(got, before, JSON.stringify(chain));
  }
});

test('EX-02: frames nested within the site send as before (one level, two levels)', async () => {
  const one = await sendOnce(B3('chrome', [`https://${LMS}`]));
  assert.deepEqual(one.names, ['MoodleSession', 'SiteLax', 'StrictCsrf', 'Unspecified']);
  const two = await sendOnce(B3('chrome', ['https://media.victim-uni.example', `https://${LMS}`]));
  assert.deepEqual(two.names, ['MoodleSession', 'SiteLax', 'StrictCsrf', 'Unspecified']);
  const twoWithSite = await sendOnce(alsoGranted(B3('chrome', ['https://media.victim-uni.example', `https://${LMS}`]), 'victim-uni.example'), { partitionKey: site('https://victim-uni.example') });
  assert.deepEqual(twoWithSite.names, ['MoodleSession', 'SiteLax', 'StrictCsrf', 'Unspecified']);
});

test('EX-02 in the Firefox build: the B3 shape without ancestors (Firefox before 153) sends as before; with them, the same rule as Chrome', async () => {
  const before = await sendOnce(B3('firefox', []), { target: 'firefox' });
  assert.deepEqual(before.names, ['MoodleSession', 'SiteLax', 'StrictCsrf', 'Unspecified'], 'as SEC48 probe-firefox B3');
  const reported = await sendOnce(B3('firefox', ['https://evil-frame.attacker.example', `https://${LMS}`]), { target: 'firefox' });
  assert.deepEqual(reported.names, [], 'rule ② gives no unpartitioned cookie in Firefox; the frame is off the site for rule ①');
  assertSubset(reported.names, before.names, 'Firefox B3');
});

test('EX-02 with topSite: an ancestor that only shares labels with the page (a sibling the browser makes a site of its own) is off the site', async () => {
  const shape = (tab) => {
    const top = 'https://a.parent.example/p';
    const nested = 'https://a.parent.example/embed';
    const track = 'https://a.parent.example/v.mp4';
    world.tabUrl = top;
    world.frames = [frame(0, top, [], 'D0'), frame(5, nested, [video(track)], 'D5', ['https://b.parent.example', 'https://a.parent.example'])];
    world.granted = grant('a.parent.example');
    world.jar = [cookie('ALax', 'a.parent.example')];
    saw(tab, track, 5, chromeReq('D5', nested));
    return track;
  };
  const without = (await sendOnce(shape)).names;
  assert.deepEqual(without, ['ALax'], 'without getPartitionKey: b.parent.example may share a cookie with the page');
  const withIt = (await sendOnce(shape, { partitionKey: site('https://a.parent.example') })).names;
  assert.deepEqual(withIt, []);
});

test('EX-02: the popup still offers an embedded player’s CDN (SEC48 D2), the player frame reporting the LMS as its ancestor', async () => {
  const tab = nextTab++;
  const lms = 'https://lms.uni.example/course/1';
  const embed = 'https://player.video.example/embed/1';
  const mp4 = 'https://cdn.video.example/m/1.mp4';
  Object.assign(world, { tabUrl: lms, frames: [frame(0, lms, [], 'D0'), frame(5, embed, [video(mp4)], 'D5', ['https://lms.uni.example'])], granted: grant('player.video.example'), jar: [] });
  const r = await detectTab(tab, { force: true });
  assert.deepEqual(r.frameAncestors, { 5: ['lms.uni.example'] });
  assert.deepEqual(cookieHostOrigins(r), ['https://cdn.video.example/*']);
});
