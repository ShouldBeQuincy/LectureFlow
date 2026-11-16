// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// src/background/detector.js driven as a whole (detectTab) against a fake
// `browser`: what it injects where, and which of its own requests carry
// cookies (security reviews FB1 M-2 / L-4 and SEC15 L1–L3, I3). The
// build-generated modules it imports (build-info.js, ui/tokens.js) and the
// browser polyfill are replaced by stubs through a module hook.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { registerHooks } from 'node:module';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const STUBS = {
  '../build-info.js': "export const BUILD = Object.freeze({ target: 'chrome', version: '0.0.0', e2e: false });",
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

const world = {
  tabUrl: '',
  frames: [], // what scanFrame returns: [{ frameId, documentId?, result }]
  title: null, // (args, target) → what frameTitle returns
  pageFetch: () => ({ ok: false, status: 401, contentType: '', size: -1, text: '' }),
  granted: () => true,
  hang: new Set(), // names of injected functions that never answer ("name:top": only when injected into frame 0 alone)
  refuse: new Set(), // names of injected functions the browser refuses to run ("name:all": only into all frames)
  goneDocs: new Set(), // document ids (Chrome) whose document is gone: an injection pinned to one is refused
  injections: [], // { name, target, args }
  requests: [], // the background's own fetches: { url, method, credentials, redirect }
};

function reset(o) {
  Object.assign(world, { title: null, pageFetch: () => ({ ok: false, status: 401, contentType: '', size: -1, text: '' }), granted: () => true, hang: new Set(), refuse: new Set(), goneDocs: new Set(), ...o, injections: [], requests: [] });
}

const extFile = (rel) => {
  if (rel === 'profiles/index.json') return JSON.stringify({ profiles: ['panopto.json'] });
  return fs.readFileSync(path.join(root, rel), 'utf8');
};

globalThis.browser = {
  runtime: { getURL: (rel) => `ext://lf/${rel}`, getManifest: () => ({ version: '0.0.0' }), id: 'lf' },
  i18n: { getUILanguage: () => 'en', getMessage: (k) => k },
  storage: {
    local: { get: async () => ({}), set: async () => {} },
    session: { get: async () => ({}), set: async () => {}, remove: async () => {} },
  },
  permissions: {
    contains: async ({ origins }) => origins.every((o) => world.granted(o)),
    getAll: async () => ({ origins: [] }),
  },
  tabs: { get: async () => ({ url: world.tabUrl }) },
  action: { setBadgeText: async () => {}, setBadgeBackgroundColor: async () => {}, setBadgeTextColor: async () => {} },
  scripting: {
    async executeScript({ target, func, args }) {
      world.injections.push({ name: func.name, target, args });
      const top = !target.allFrames && Array.isArray(target.frameIds) && target.frameIds.length === 1 && target.frameIds[0] === 0;
      if (world.refuse.has(func.name) || (target.allFrames && world.refuse.has(`${func.name}:all`))) throw new Error('Cannot access contents of the page');
      if (world.hang.has(func.name) || (top && world.hang.has(`${func.name}:top`))) return new Promise(() => {});
      if (func.name === 'scanFrame') {
        const pick = target.allFrames ? world.frames : world.frames.filter((f) => target.frameIds.includes(f.frameId));
        return pick.map((f) => ({ frameId: f.frameId, documentId: f.documentId, result: structuredClone(f.result) }));
      }
      if (func.name === 'frameTitle') return [{ frameId: 0, result: world.title ? world.title(args, target) : null }];
      if (func.name === 'pageFetch') {
        // Chrome: pinned to a document (documentIds), refused once that document is gone
        let frameId = Array.isArray(target.frameIds) ? target.frameIds[0] : undefined;
        if (Array.isArray(target.documentIds)) {
          const doc = target.documentIds[0];
          const f = world.frames.find((x) => x.documentId === doc);
          if (!f || world.goneDocs.has(doc)) throw new Error(`No document with id ${doc}`);
          frameId = f.frameId;
        }
        return [{ frameId, result: [world.pageFetch(args[0][0], target, args)] }];
      }
      throw new Error(`unexpected injection ${func.name}`);
    },
  },
};

globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  if (u.startsWith('ext://lf/')) return new Response(extFile(u.slice('ext://lf/'.length)), { status: 200 });
  world.requests.push({ url: u, method: init.method || 'GET', credentials: init.credentials, redirect: init.redirect });
  return new Response(init.method === 'HEAD' ? null : '#EXTM3U\n', { status: 200, headers: { 'content-length': '4096' } });
};

const { detectTab, setInjectTimeout, lastResult } = await import('../../src/background/detector.js');

// ------------------------------------------------------------ helpers

let nextTab = 1;
const detect = (o = {}) => detectTab(nextTab++, { force: true, ...o });
const frame = (frameId, url, elements = [], extra = {}) => ({ frameId, ...extra, result: { url, elements, resources: [], reads: [], encryptedEvents: 0, observed: [], opaque: [], docs: [], ancestors: [] } });
const video = (src) => ({ tag: 'video', src, currentSrc: src, sources: [], mediaKeys: false, videoWidth: 1280, videoHeight: 720, duration: 60, readyState: 4 });
const injected = (name) => world.injections.filter((i) => i.name === name);
const credsFor = (url) => world.requests.filter((r) => r.url === url).map((r) => r.credentials);

const LMS = 'https://lms.example.edu/course/1';
const TENANT = 'https://tenant.hosted.panopto.com';
const EMBED = `${TENANT}/Panopto/Pages/Embed.aspx?id=00000000-0000-0000-0000-000000000000`;

// ------------------------------------------------------------ cookies (M-2, L1)

test('the fallback’s origin comes from the browser, not from the scan: another origin goes without cookies', async () => {
  // tabs.get says LMS; the scanned frame 0 claims to be lectures.example.edu
  const target = 'https://lectures.example.edu/media/a.mp4';
  reset({ tabUrl: LMS, frames: [frame(0, 'https://lectures.example.edu/watch/7', [video(target)])] });
  const r = await detect();
  assert.equal(r.status, 'ok');
  assert.deepEqual(credsFor(target), ['omit']);
  // the same origin as the page the browser named: with cookies, redirects not followed
  const own = 'https://lms.example.edu/media/a.mp4';
  reset({ tabUrl: LMS, frames: [frame(0, LMS, [video(own)])] });
  await detect();
  assert.deepEqual(world.requests.filter((q) => q.url === own).map((q) => [q.method, q.credentials, q.redirect]), [['HEAD', 'include', 'manual']]);
});

test('a page embedding a Panopto tenant’s player cannot send signed-in requests to the tenant (SEC15 L1, L2)', async () => {
  const logout = `${TENANT}/Panopto/Pages/Auth/Logout.aspx?x=.mp4`;
  const target = `${TENANT}/Panopto/Content/x.mp4`;
  reset({ tabUrl: 'https://attacker.example.org/p', frames: [frame(0, 'https://attacker.example.org/p', [video(target), video(logout)]), frame(5, EMBED, [])] });
  const r = await detect();
  assert.equal(r.status, 'ok');
  // the precondition (SEC46 信-4): the profile did match through the embedded frame —
  // only a profile found makes detection scan a second time, for its selectors
  const scans = injected('scanFrame');
  assert.equal(scans.length, 2, 'a second scan, for the profile’s selectors');
  assert.deepEqual(scans[0].args, [[]], 'the first scan reads no selector');
  assert.ok(Array.isArray(scans[1].args[0]) && scans[1].args[0].length > 0, 'the second reads the profile’s selectors');
  // the profile matched through the embedded frame, which decides the fallback's
  // credentials below; but that frame gave no track, so the result is the page's
  // own, generic (AUD3 低-1; before: "panopto")
  assert.equal(r.profile, null, 'the embedded frame gave no track: generic');
  for (const u of [target, logout]) {
    assert.ok(credsFor(u).length > 0, `a fallback for ${u}`);
    assert.ok(credsFor(u).every((c) => c === 'omit'), `no cookies to ${u}`);
  }
  // L2: the page fetch ran in the frame that referenced the URL (the page), not in the tenant's frame
  const pf = injected('pageFetch');
  assert.ok(pf.length > 0);
  assert.ok(pf.every((i) => i.target.frameIds.length === 1 && i.target.frameIds[0] === 0), JSON.stringify(pf.map((i) => i.target)));
});

test('the embedded player’s own media, on its own origin, still carries cookies; a Panopto page itself, its declared hosts', async () => {
  const own = `${TENANT}/Panopto/Content/own.mp4`;
  reset({ tabUrl: LMS, frames: [frame(0, LMS, []), frame(5, EMBED, [video(own)])] });
  await detect();
  assert.deepEqual(credsFor(own), ['include']);
  assert.ok(injected('pageFetch').every((i) => i.target.frameIds[0] === 5));

  const viewer = `${TENANT}/Panopto/Pages/Viewer.aspx?id=00000000-0000-0000-0000-000000000000`;
  const other = 'https://other.hosted.panopto.com/Panopto/Content/x.mp4';
  reset({ tabUrl: viewer, frames: [frame(0, viewer, [video(other)])] });
  await detect();
  assert.deepEqual(credsFor(other), ['include'], 'the page itself is a Panopto page: its declared hosts');
});

test('a track only the embedded player’s page response named belongs to that frame (SEC46 信-3, M14)', async () => {
  // what cookiePlan's frames and the partition key go by (src/lib/send.js, partition.js)
  const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
  const podcast = 'https://d0000.cloudfront.net/sessions/x/podcast.mp4';
  const player = frame(5, EMBED, []);
  player.result.observed = [{ key: `${panopto.id}:${panopto.version}`, index: 0, url: `${TENANT}/Panopto/Pages/Viewer/DeliveryInfo.aspx`, values: { podcastUrl: [podcast] } }];
  reset({ tabUrl: LMS, frames: [frame(0, LMS, []), player] });
  const r = await detect();
  assert.equal(r.profile && r.profile.id, 'panopto');
  const named = [...r.named.tracks, ...r.named.others].find((t) => t.url === podcast);
  assert.ok(named, 'the player frame’s response named it');
  assert.deepEqual(named.sources, ['response']);
  assert.deepEqual(named.frameIds, [5], 'the frame whose response named it, not the embedding page (frame 0)');
});

test('nothing for an origin the user did not grant', async () => {
  const target = 'https://cdn.example.org/a.mp4';
  reset({ tabUrl: LMS, frames: [frame(0, LMS, [video(target)])], granted: (o) => !o.includes('cdn.example.org') });
  await detect();
  assert.deepEqual(credsFor(target), []);
});

// ------------------------------------------------------------ the page title (N23, L3)

test('the title: read after the screening, from the scanned document, with the screened address', async () => {
  const page = 'https://lectures.example.edu/watch/7';
  reset({ tabUrl: page, frames: [frame(0, page, [], { documentId: 'doc-7' })], title: ([expected]) => ({ url: expected, title: 'Week 7 | Example University', history: [], ancestors: [] }) });
  let r = await detect();
  assert.equal(r.lecture.title, 'Week 7');
  assert.deepEqual(injected('frameTitle').map((i) => [i.target, i.args]), [[{ tabId: r.tabId, documentIds: ['doc-7'] }, [page]]], 'Chrome: pinned to the scanned document');
  // Firefox gives no document ids: the frame, and the address check in the page
  reset({ tabUrl: page, frames: [frame(0, page)], title: ([expected]) => ({ url: expected, title: 'Week 7', history: [], ancestors: [] }) });
  r = await detect();
  assert.deepEqual(injected('frameTitle').map((i) => i.target), [{ tabId: r.tabId, frameIds: [0] }]);
  assert.equal(r.lecture.title, 'Week 7');
  // the frame shows another address by then: no title
  reset({ tabUrl: page, frames: [frame(0, page)], title: () => ({ url: 'https://lectures.example.edu/watch/8', title: 'Week 8', history: [], ancestors: [] }) });
  assert.equal((await detect()).lecture.title, '');
});

test('the title is never asked of a blocked page or frame, nor when a profile matched', async () => {
  const title = () => ({ url: '', title: 'x', history: [], ancestors: [] });
  reset({ tabUrl: 'https://www.youtube.com/watch?v=x', frames: [frame(0, 'https://www.youtube.com/watch?v=x')], title });
  assert.equal((await detect()).status, 'unsupported');
  assert.equal(world.injections.length, 0, 'a blocked page is not even scanned');

  // the top document showed a blocked path before (a single-page site): the scan finds it, no title
  const feed = 'https://www.linkedin.com/feed/';
  reset({ tabUrl: feed, frames: [frame(0, feed, [], {})], title });
  world.frames[0].result.docs = [{ url: feed, parent: -1, history: ['https://www.linkedin.com/learning/x', feed] }];
  assert.equal((await detect()).status, 'unsupported');
  assert.equal(injected('frameTitle').length, 0);

  const viewer = `${TENANT}/Panopto/Pages/Viewer.aspx?id=00000000-0000-0000-0000-000000000000`;
  reset({ tabUrl: viewer, frames: [frame(0, viewer)], title });
  const r = await detect();
  assert.equal(r.profile && r.profile.id, 'panopto');
  assert.equal(injected('frameTitle').length, 0);
});

// ------------------------------------------------------------ many media elements (SEC23b 信息-9)

test('a page with 200,000 media elements: detection ends, the lecture takes the longest duration', { timeout: 20000 }, async () => {
  const page = 'https://lectures.example.edu/watch/7';
  const elements = Array.from({ length: 200000 }, (_, i) => ({ tag: 'video', duration: i === 0 ? 5400 : 1 + (i % 3600) }));
  reset({ tabUrl: page, frames: [frame(0, page, elements)] });
  const r = await detect();
  assert.equal(r.status, 'ok');
  assert.equal(r.lecture.durationSec, 5400);
});

test('200,000 media elements past the scan limits too: the detector’s own page duration takes the longest (SEC33 信-6)', { timeout: 30000 }, async () => {
  // boundScan would keep 1000 of them; raised here, so that the detector's
  // own longestDuration (not only detect.js's) sees every one
  const { setScanLimits } = await import('../../src/background/detector.js');
  const page = 'https://lectures.example.edu/watch/7';
  const elements = Array.from({ length: 200000 }, (_, i) => ({ tag: 'video', duration: i === 199999 ? 5400 : 1 + (i % 3600) }));
  setScanLimits({ elements: 300000 });
  try {
    reset({ tabUrl: page, frames: [frame(0, page, elements)] });
    const r = await detect();
    assert.equal(r.status, 'ok');
    assert.equal(r.raw.frames[0].elements.length, 200000, 'every element reached detection');
    assert.equal(r.lecture.durationSec, 5400, 'the last one is the longest');
  } finally {
    setScanLimits();
  }
});

// ------------------------------------------------------------ a frame address with a long fragment (SEC33 低-1)

test('an embedded player whose address has a long fragment stays: the profile matches, the duration is read; in-page too', { timeout: 20000 }, async () => {
  const { forgetTab } = await import('../../src/background/detector.js');
  const { LIMITS } = await import('../../src/lib/detect.js');
  const embed = `${EMBED}#${'x'.repeat(LIMITS.urlLength + 100)}`;
  const own = `${TENANT}/Panopto/Content/own.mp4`;
  const tabs = [];
  try {
    // the popup: the top page embeds the player
    reset({ tabUrl: LMS, frames: [frame(0, LMS, []), frame(5, embed, [video(own)])] });
    tabs.push(nextTab);
    const r = await detect();
    assert.equal(r.status, 'ok');
    assert.equal(r.profile && r.profile.id, 'panopto');
    assert.deepEqual(r.raw.frames.map((f) => f.frameId), [0, 5]);
    assert.equal(r.lecture.durationSec, 60);
    assert.equal(r.raw.limits, null, 'nothing left out');
    // the in-page button in that frame: the frame it detects in always stays
    reset({ tabUrl: LMS, frames: [frame(0, LMS, []), frame(5, embed, [video(own)])] });
    tabs.push(nextTab);
    const p = await detect({ frameId: 5, frameUrl: embed });
    assert.equal(p.status, 'ok');
    assert.equal(p.profile && p.profile.id, 'panopto');
    assert.deepEqual(p.raw.frames, [], 'an in-page detection keeps no scan (SEC36 低-10); the frame stayed: its track and duration below');
    assert.equal(p.lecture.durationSec, 60);
    assert.ok(p.tracks.some((t) => t.url === own));
    // an address that is too long even without its fragment: another frame
    // would be left out, the one the button detects in is not
    const query = `${EMBED}&pad=${'q'.repeat(LIMITS.urlLength)}`;
    reset({ tabUrl: LMS, frames: [frame(0, LMS, []), frame(5, query, [video(own)])] });
    tabs.push(nextTab);
    const q = await detect({ frameId: 5, frameUrl: query });
    assert.equal(q.status, 'ok');
    assert.deepEqual(q.raw.frames, []);
    assert.equal(q.lecture.durationSec, 60);
    assert.ok(q.tracks.some((t) => t.url === own));
  } finally {
    for (const t of tabs) forgetTab(t);
  }
});

test('embedded frames with long fragments do not use up the scan budget of the top page’s media (SEC33 低-4)', { timeout: 20000 }, async () => {
  // the review's shape at a smaller scale: frames whose whole addresses
  // add up to exactly scanChars, the top page's media after them
  const { setScanLimits, forgetTab } = await import('../../src/background/detector.js');
  const page = 'https://lectures.example.edu/watch/7';
  const budget = 8 * 8192;
  const embed = (i) => {
    const head = `https://lectures.example.edu/embed/${i}#`;
    return head + 'f'.repeat(8192 - head.length);
  };
  const media = [video('https://lectures.example.edu/a.mp4'), video('https://lectures.example.edu/b.mp4')];
  const top = () => frame(0, page, media.map((m) => ({ ...m })), {});
  const tabs = [];
  setScanLimits({ scanChars: budget });
  try {
    reset({ tabUrl: page, frames: [top()] });
    tabs.push(nextTab);
    const alone = await detect();
    reset({ tabUrl: page, frames: [top(), ...Array.from({ length: 8 }, (_, i) => frame(i + 1, embed(i)))] });
    world.frames[0].result.resources = [{ url: 'https://lectures.example.edu/c.mp4', initiatorType: 'video', responseStatus: 200 }];
    tabs.push(nextTab);
    const r = await detect();
    assert.equal(alone.tracks.length, 2);
    assert.deepEqual(r.tracks.map((t) => t.url).sort(), ['https://lectures.example.edu/a.mp4', 'https://lectures.example.edu/b.mp4', 'https://lectures.example.edu/c.mp4']);
    assert.equal(r.raw.limits, null, 'nothing left out');
    assert.deepEqual(r.raw.frames.slice(1).map((f) => f.url), Array.from({ length: 8 }, (_, i) => `https://lectures.example.edu/embed/${i}`), 'kept without their fragments');
  } finally {
    setScanLimits();
    for (const t of tabs) forgetTab(t);
  }
});

// ------------------------------------------------------------ what stays first (SEC33 低-2)

test('an LMS page cannot crowd an embedded player out of the scan: empty frames before it, address-less elements on top', { timeout: 20000 }, async () => {
  const { setScanLimits, forgetTab } = await import('../../src/background/detector.js');
  const Syn = await import('../support/synth.mjs');
  const [{ frameId: _f, ...player }] = Syn.panoptoFrames();
  const playerFrame = (id) => ({ frameId: id, result: { elements: [], resources: [], reads: [], encryptedEvents: 0, observed: [], opaque: [], docs: [], ancestors: [], ...player, url: Syn.panoptoPageUrl } });
  const pageFetch = (req) => (Syn.panoptoTexts[req.url] !== undefined ? { ok: true, status: 200, contentType: 'application/vnd.apple.mpegurl', text: Syn.panoptoTexts[req.url] } : { ok: false, status: 404, contentType: '', size: -1, text: '' });
  const empty = () => ({ tag: 'video', src: '', currentSrc: '', sources: [], mediaKeys: false, duration: 1, readyState: 0 });
  const tabs = [];
  const run = async (frames) => {
    reset({ tabUrl: LMS, frames, pageFetch });
    tabs.push(nextTab);
    return detect();
  };
  try {
    const base = await run([frame(0, LMS), playerFrame(9)]);
    assert.equal(base.profile && base.profile.id, 'panopto');
    assert.ok(base.named.tracks.length > 0 && base.lecture.durationSec > 1);
    // (the shapes at a smaller scale: 4 frames, 4 elements)
    setScanLimits({ frames: 4, elements: player.elements.length + 1 });
    // three empty frames of the LMS listed before the player
    const frames = await run([frame(0, LMS), frame(1, `${LMS}/a`), frame(2, `${LMS}/b`), frame(3, `${LMS}/c`), playerFrame(9)]);
    assert.equal(frames.profile && frames.profile.id, 'panopto', 'the player frame is kept');
    assert.deepEqual(frames.raw.limits, { frames: 1 });
    assert.equal(frames.named.tracks.length, base.named.tracks.length);
    // the LMS page's own address-less media elements fill the element count
    const els = await run([frame(0, LMS, Array.from({ length: player.elements.length + 1 }, empty)), playerFrame(9)]);
    assert.equal(els.lecture.durationSec, base.lecture.durationSec, 'the player’s elements are kept, so is its duration');
    assert.equal(els.named.tracks.length, base.named.tracks.length);
  } finally {
    setScanLimits();
    for (const t of tabs) forgetTab(t);
  }
});

test('a same-origin widget frame read inside the top page’s scan cannot crowd the top page’s lecture out; `doc` is not kept (SEC36 低-7)', { timeout: 20000 }, async () => {
  const { setScanLimits, forgetTab } = await import('../../src/background/detector.js');
  const top = 'https://lectures.example.edu/watch/9';
  const lecture = 'https://lectures.example.edu/media/lecture-09.mp4';
  let uid = 0;
  const junkUrl = (len) => {
    const h = `https://lectures.example.edu/j/${(uid++).toString(36)}/`;
    return `${h}${'a'.repeat(len - h.length - 5)}.m3u8`;
  };
  // 140 manifest-named entries of 8 KB, then smaller ones that drain what is left of the budget
  const junk = [...Array.from({ length: 140 }, () => 8192), 8192, 4096, 2048, 1024, 512, 256, 128, 64, 48].map((n) => ({ url: junkUrl(n), initiatorType: 'fetch', responseStatus: 0 }));
  const topFrame = frame(0, top, [{ ...video(lecture), doc: 0 }]);
  // as scanFrame gives it: the top document's entries (doc 0), then the same-origin child's (doc 1)
  topFrame.result.resources = [{ url: lecture, initiatorType: 'video', responseStatus: 206, doc: 0 }, ...junk.map((r) => ({ ...r, doc: 1 }))];
  topFrame.result.docs = [{ url: top, parent: -1, history: [] }, { url: `${top}/widget`, parent: 0, history: [] }];
  const widget = frame(1, `${top}/widget`);
  widget.result.resources = junk.map((r) => ({ ...r, doc: 0 }));
  const tab = nextTab;
  setScanLimits({ scanChars: 1024 * 1024 });
  try {
    reset({ tabUrl: top, frames: [topFrame, widget], pageFetch: (req) => (req.mode === 'probe' && req.url === lecture ? { ok: true, status: 200, contentType: 'video/mp4', size: 4096 } : { ok: false, status: 404, contentType: '', size: -1, text: '' }) });
    const r = await detect();
    assert.equal(r.status, 'ok');
    assert.ok(r.tracks.some((t) => t.url === lecture), 'the lecture is a track');
    assert.ok(r.raw.limits && r.raw.limits.scanChars > 0, 'the budget was used up');
    assert.ok(r.raw.frames.every((f) => [...f.elements, ...f.resources].every((x) => !('doc' in x))));
  } finally {
    setScanLimits();
    forgetTab(tab);
  }
});

// ------------------------------------------------------------ what is kept besides the scan (SEC33 低-3)

test('a stored detection keeps no list of non-media addresses and of segment runs only their counts; the report as before', { timeout: 20000 }, async () => {
  const { forgetTab, reportFor } = await import('../../src/background/detector.js');
  const { detectTracks } = await import('../../src/lib/detect.js');
  const page = 'https://lectures.example.edu/watch/7';
  // a script (not media) and a run of three numbered files (a stream's segments)
  const resources = [
    { url: 'https://lectures.example.edu/app.js', initiatorType: 'script', responseStatus: 200 },
    ...[1, 2, 3].map((i) => ({ url: `https://cdn.example.edu/v/chunk_${i}.mp4`, initiatorType: 'xmlhttprequest', responseStatus: 200 })),
  ];
  const keyed = 'https://lectures.example.edu/k.mp4';
  reset({ tabUrl: page, frames: [frame(0, page, [video('https://lectures.example.edu/a.mp4'), { ...video(keyed), mediaKeys: true, duration: 5400 }])] });
  world.frames[0].result.resources = resources;
  const tab = nextTab;
  try {
    const r = await detect();
    // the evidence: only its flags are kept (SEC33 低-5); what detection drew from it stays
    assert.deepEqual(r.raw.detection.evidence, { mse: false, mediaKeys: true, mediaKeysOnStream: false, encrypted: false });
    assert.equal(r.tracks.find((t) => t.url === keyed).protected, true);
    assert.equal(r.lecture.durationSec, 5400);
    // what detection itself gives (the pure function, as the fixture replay runs it)
    const lib = await detectTracks({ frames: r.raw.frames, webRequests: r.raw.webRequests }, { async fetchText() { return { ok: false, status: 404, contentType: '', text: '' }; }, async probeFile() { return { ok: false, status: 404, contentType: '', size: -1 }; } });
    assert.ok(lib.ignored.length > 0 && lib.segmentRuns.length === 1 && typeof lib.segmentRuns[0].template === 'string');
    assert.deepEqual(lib.evidence.mediaKeysUrls, [keyed]);
    assert.deepEqual(lib.evidence.mediaDurations, [60, 5400]);
    assert.ok(!('ignored' in r.raw.detection), 'no ignored list kept');
    assert.deepEqual(r.raw.detection.segmentRuns, [{ count: 3 }]);
    const rep = await reportFor(tab);
    assert.ok(rep.ok);
    assert.deepEqual(rep.report.expected.segmentRuns, [{ count: 3 }]);
    assert.ok(!('limits' in rep.report));
  } finally {
    forgetTab(tab);
  }
});

test('selector reads past attrLength / attrNames are kept cut; the report is the same as from uncut ones; counted in limits', { timeout: 20000 }, async () => {
  const { setScanLimits, forgetTab, reportFor } = await import('../../src/background/detector.js');
  const { LIMITS } = await import('../../src/lib/detect.js');
  const Syn = await import('../support/synth.mjs');
  const long = (c) => c.repeat(LIMITS.attrLength + 50);
  const names = Array.from({ length: LIMITS.attrNames + 8 }, (_, i) => `data-n${i}`);
  const page = () => {
    const [{ frameId: _f, ...res }] = Syn.panoptoFrames();
    res.reads = res.reads.map((rd) => (rd.nodes.length ? { ...rd, nodes: [{ path: [{ tag: long('t'), id: long('i'), classes: [long('c'), 'short'] }], attrNames: [...names, long('a')] }] } : rd));
    return [{ frameId: 0, result: { elements: [], resources: [], reads: [], encryptedEvents: 0, observed: [], opaque: [], docs: [], ancestors: [], ...res } }];
  };
  const tabs = [];
  const run = async () => {
    reset({ tabUrl: Syn.panoptoPageUrl, frames: page() });
    world.pageFetch = (req) => (Syn.panoptoTexts[req.url] !== undefined ? { ok: true, status: 200, contentType: 'application/vnd.apple.mpegurl', text: Syn.panoptoTexts[req.url] } : { ok: false, status: 404, contentType: '', size: -1, text: '' });
    tabs.push(nextTab);
    const r = await detect();
    const rep = await reportFor(r.tabId);
    return { r, rep };
  };
  try {
    const cut = await run();
    setScanLimits({ attrLength: 1e9, attrNames: 1e9 });
    const whole = await run();
    const nodes = cut.r.raw.frames[0].reads.flatMap((rd) => rd.nodes);
    assert.ok(nodes.length > 0);
    for (const n of nodes) {
      assert.ok(n.path.every((p) => p.tag.length === LIMITS.attrLength && p.id.length === LIMITS.attrLength && p.classes[0].length === LIMITS.attrLength && p.classes[1] === 'short'));
      assert.equal(n.attrNames.length, LIMITS.attrNames);
    }
    assert.equal(whole.r.raw.frames[0].reads.flatMap((rd) => rd.nodes)[0].attrNames.length, names.length + 1);
    assert.ok(cut.rep.ok && whole.rep.ok);
    assert.deepEqual(cut.rep.report.dom, whole.rep.report.dom, 'the report keeps less than attrLength anyway');
    // per node: tag, id, one class, 9 names past attrNames (8 + the long one)
    assert.deepEqual(cut.rep.report.limits, { longValues: nodes.length * (3 + 9) });
    assert.ok(!('limits' in whole.rep.report));
    assert.equal(cut.r.lecture.title, whole.r.lecture.title);
    assert.equal(cut.r.tracks.length, whole.r.tracks.length);
  } finally {
    setScanLimits();
    for (const t of tabs) forgetTab(t);
  }
});

// ------------------------------------------------------------ hung pages (I3)

test('an injection that never answers does not hang detection', { timeout: 10000 }, async () => {
  setInjectTimeout(50);
  try {
    const page = 'https://lectures.example.edu/watch/7';
    reset({ tabUrl: page, frames: [frame(0, page, [video('https://lectures.example.edu/a.mp4')])], hang: new Set(['frameTitle', 'pageFetch']) });
    const t0 = Date.now();
    const r = await detect();
    assert.equal(r.status, 'ok');
    assert.equal(r.lecture.title, '');
    assert.ok(Date.now() - t0 < 5000);
    // the scan itself never answers (a page still loading, SEC15 R2-5): its
    // own status, tried on all frames and then the top one, and nothing stored
    reset({ tabUrl: page, frames: [frame(0, page)], hang: new Set(['scanFrame']) });
    const tab = nextTab;
    assert.equal((await detect()).status, 'page_loading');
    assert.deepEqual(injected('scanFrame').map((i) => i.target), [{ tabId: tab, allFrames: true }, { tabId: tab, frameIds: [0] }]);
    assert.equal(lastResult(tab), null, 'the next look detects again');
    // an in-page frame's detection likewise
    reset({ tabUrl: page, frames: [frame(0, page), frame(4, page)], hang: new Set(['scanFrame']) });
    assert.equal((await detect({ frameId: 4, frameUrl: page })).status, 'page_loading');
  } finally {
    setInjectTimeout(15000);
  }
});

test('a page that refuses the scan (rather than not answering) is still “cannot read this page”', async () => {
  const page = 'https://lectures.example.edu/watch/7';
  reset({ tabUrl: page, frames: [frame(0, page)], refuse: new Set(['scanFrame']) });
  const tab = nextTab;
  const r = await detect();
  assert.equal(r.status, 'no_access');
  assert.ok(lastResult(tab) === r);
  // all frames refused, the top one does not answer: the page is loading after all
  reset({ tabUrl: page, frames: [frame(0, page)], refuse: new Set(['scanFrame:all']), hang: new Set(['scanFrame:top']) });
  setInjectTimeout(50);
  try {
    assert.equal((await detect()).status, 'page_loading');
  } finally {
    setInjectTimeout(15000);
  }
});

// ------------------------------------------------------------ the tab moved on meanwhile (SEC15 I1)

test('a tab that moved to another page while detection ran keeps nothing of it; a fragment change is the same page', async () => {
  const { lastResult } = await import('../../src/background/detector.js');
  const page = 'https://lectures.example.edu/watch/7';
  const media = video('https://lectures.example.edu/a.mp4');
  const badges = [];
  const setBadgeText = globalThis.browser.action.setBadgeText;
  globalThis.browser.action.setBadgeText = async (o) => {
    badges.push(o);
  };
  try {
    // the address changes while the page's title is read (after the scan, before the store)
    reset({ tabUrl: page, frames: [frame(0, page, [media])], title: () => ((world.tabUrl = 'https://lectures.example.edu/watch/8'), null) });
    const tab = nextTab;
    const r = await detect();
    assert.equal(r.status, 'navigated');
    assert.equal(lastResult(tab), null, 'nothing stored for the page it shows now');
    assert.deepEqual(badges.filter((b) => b.tabId === tab), [], 'no number from the page before on the badge');

    // a single-page site that changes only the fragment: still the same page
    reset({ tabUrl: page, frames: [frame(0, page, [media])], title: () => ((world.tabUrl = `${page}#t=60`), null) });
    const same = nextTab;
    const s = await detect();
    assert.equal(s.status, 'ok');
    assert.ok(lastResult(same) === s);
    assert.deepEqual(badges.filter((b) => b.tabId === same).map((b) => b.text), ['1']);

    // a tab that shows no address any more
    reset({ tabUrl: page, frames: [frame(0, page, [media])], title: () => ((world.tabUrl = ''), null) });
    const gone = nextTab;
    assert.equal((await detect()).status, 'navigated');
    assert.equal(lastResult(gone), null);
  } finally {
    globalThis.browser.action.setBadgeText = setBadgeText;
  }
});

// ------------------------------------------------------------ page fetches stay with the scanned document (SEC33 信-1)

const MOVED = { ok: false, status: 0, contentType: '', text: '', size: -1, error: 'moved' };

test('page fetches are pinned to the scanned document (Chrome) and carry the address it was scanned at (both)', async () => {
  const page = 'https://lectures.example.edu/watch/7';
  const media = [video('https://lectures.example.edu/a.mp4'), video('https://lectures.example.edu/b.mp4')];
  reset({ tabUrl: page, frames: [frame(0, page, media, { documentId: 'doc-7' })] });
  let r = await detect();
  assert.equal(r.status, 'ok');
  let pf = injected('pageFetch');
  assert.ok(pf.length >= 2);
  assert.ok(pf.every((i) => JSON.stringify(i.target) === JSON.stringify({ tabId: r.tabId, documentIds: ['doc-7'] })), JSON.stringify(pf.map((i) => i.target)));
  assert.ok(pf.every((i) => i.args.length === 3 && i.args[1] === null && JSON.stringify(i.args[2]) === JSON.stringify({ url: page })), 'the scanned address, for the check in the page');
  // Firefox: no document ids, the frame and the check in the page
  reset({ tabUrl: LMS, frames: [frame(0, LMS), frame(5, `${EMBED}#t=10`, [video(`${TENANT}/Panopto/Content/own.mp4`)])] });
  r = await detect();
  pf = injected('pageFetch');
  assert.ok(pf.length > 0);
  assert.ok(pf.every((i) => JSON.stringify(i.target) === JSON.stringify({ tabId: r.tabId, frameIds: [5] }) && i.args[2].url.split('#')[0] === EMBED));
  // a scan that marked its document (scanFrame's token): the check is of that mark, and the mark is not kept
  const marked = frame(0, page, media);
  marked.result.token = 'tok-7';
  reset({ tabUrl: page, frames: [marked] });
  r = await detect();
  pf = injected('pageFetch');
  assert.ok(pf.length >= 2 && pf.every((i) => JSON.stringify(i.args[2]) === JSON.stringify({ token: 'tok-7' })));
  assert.ok(r.raw.frames.every((f) => !('token' in f) && !('documentId' in f)));
});

test('the tab moves on while detection fetches: nothing more is fetched, from the page or the background, and nothing kept', async () => {
  const page = 'https://lectures.example.edu/watch/7';
  const media = ['a', 'b', 'c', 'd'].map((n) => video(`https://lectures.example.edu/${n}.mp4`));
  // the first page fetch is the last thing the old page does
  reset({
    tabUrl: page,
    frames: [frame(0, page, media)],
    pageFetch: () => {
      world.tabUrl = 'https://other.example.net/home';
      return { ok: false, status: 404, contentType: '', size: -1, text: '' };
    },
  });
  const tab = nextTab;
  const r = await detect();
  assert.equal(r.status, 'navigated');
  assert.equal(lastResult(tab), null);
  assert.equal(injected('pageFetch').length, 1, 'no page fetch into the new page');
  assert.deepEqual(world.requests, [], 'nor from the background');
});

test('Firefox: the in-page button’s frame shows another document (the page fetch says so): nothing more is fetched, nothing kept', async () => {
  const own = (n) => `${TENANT}/Panopto/Content/${n}.mp4`;
  reset({ tabUrl: LMS, frames: [frame(0, LMS), frame(5, EMBED, [video(own('a')), video(own('b')), video(own('c'))])], pageFetch: () => MOVED });
  const tab = nextTab;
  const r = await detect({ frameId: 5, frameUrl: EMBED });
  assert.equal(r.status, 'navigated');
  assert.equal(lastResult(tab, 5), null);
  assert.equal(injected('pageFetch').length, 1);
  assert.deepEqual(world.requests, []);
});

test('an embedded frame that went on to another document (or keeps doing so) costs only its own fetches and tracks; the top page’s detection is kept (SEC36 低-9)', async () => {
  const page = 'https://lectures.example.edu/watch/7';
  const own = 'https://lectures.example.edu/media/lecture.mp4';
  const widget = (n) => `https://lectures.example.edu/widget/${n}.mp4`;
  const fileOk = { ok: true, status: 200, contentType: 'video/mp4', size: 4096, text: '' };
  for (const browserName of ['firefox', 'chrome']) {
    const chrome = browserName === 'chrome';
    const frames = [frame(0, page, [video(own)], chrome ? { documentId: 'd0' } : {}), frame(7, `${page}/widget`, [video(widget('a')), video(widget('b'))], chrome ? { documentId: 'd7' } : {})];
    reset({
      tabUrl: page,
      frames,
      goneDocs: new Set(chrome ? ['d7'] : []),
      // Firefox: the widget frame answers that it shows another document, every time
      pageFetch: (req, target) => (!chrome && target.frameIds && target.frameIds[0] === 7 ? MOVED : fileOk),
    });
    const tab = nextTab;
    const r = await detect();
    assert.equal(r.status, 'ok', browserName);
    assert.ok(lastResult(tab) === r, 'kept');
    assert.deepEqual(r.tracks.map((t) => t.url), [own], 'the widget’s tracks are left out, the lecture stays');
    const into7 = injected('pageFetch').filter((i) => (i.target.frameIds && i.target.frameIds[0] === 7) || (i.target.documentIds && i.target.documentIds[0] === 'd7'));
    assert.equal(into7.length, 1, 'one fetch found the frame moved, no more go there');
    assert.deepEqual(world.requests.filter((q) => q.url.includes('/widget/')), [], 'nor from the background for its addresses');
  }
});

test('Chrome: the scanned document is gone (the pinned injection is refused): nothing more is fetched, nothing kept', async () => {
  const own = (n) => `${TENANT}/Panopto/Content/${n}.mp4`;
  reset({ tabUrl: LMS, frames: [frame(0, LMS, [], { documentId: 'doc-0' }), frame(5, EMBED, [video(own('a')), video(own('b'))], { documentId: 'doc-5' })], goneDocs: new Set(['doc-5']) });
  const tab = nextTab;
  const r = await detect({ frameId: 5, frameUrl: EMBED });
  assert.equal(r.status, 'navigated');
  assert.equal(lastResult(tab, 5), null);
  assert.equal(injected('pageFetch').length, 1);
  assert.deepEqual(world.requests, []);
  // a page that does not answer in time is not gone: the background still fetches, as before
  reset({ tabUrl: LMS, frames: [frame(0, LMS, [], { documentId: 'doc-0' }), frame(5, EMBED, [video(own('a'))], { documentId: 'doc-5' })], hang: new Set(['pageFetch']) });
  setInjectTimeout(30);
  try {
    const s = await detect({ frameId: 5, frameUrl: EMBED });
    assert.equal(s.status, 'ok');
    assert.ok(world.requests.length > 0);
  } finally {
    setInjectTimeout(15000);
  }
});

// ------------------------------------------------------------ a profile stopped in the page only (SEC18f 低-18)

test('a profile the page\'s observer stopped as too slow: the restriction is unknown, and the card says why', async () => {
  const panopto = JSON.parse(extFile('profiles/panopto.json'));
  const key = `${panopto.id}:${panopto.version}`; // profileKey
  const viewer = `${TENANT}/Panopto/Pages/Viewer.aspx?id=00000000-0000-0000-0000-000000000000`;
  reset({ tabUrl: viewer, frames: [frame(0, viewer)] });
  world.frames[0].result.observeStopped = key;
  let r = await detect();
  assert.equal(r.profile && r.profile.id, 'panopto');
  assert.equal(r.downloadPolicy, 'unknown');
  assert.equal(r.policyStopped, true);
  // another profile's key, or none: unknown as before, with no reason given
  for (const other of ['other:1.0.0', undefined]) {
    reset({ tabUrl: viewer, frames: [frame(0, viewer)] });
    if (other) world.frames[0].result.observeStopped = other;
    r = await detect();
    assert.equal(r.downloadPolicy, 'unknown');
    assert.equal(r.policyStopped, false, String(other));
  }
});

// ------------------------------------------------------------ the background's regex budget (SEC18f 低-19)

test('the background\'s regex budget also starts again every minute, not only with a detection', async () => {
  const { watchRegexes } = await import('../../src/background/profiles.js');
  const { setRegexGuard, resetRegexGuard, guardedRegex, regexTrip } = await import('../../src/lib/regexguard.js');
  resetRegexGuard();
  watchRegexes();
  let t = 0;
  const steps = [];
  setRegexGuard({ now: () => (steps.length ? steps.shift() : t) });
  try {
    const re = guardedRegex('z', { owner: 'w:1' });
    // 99 ms every 10 s, no detection in between: 0.6 s a minute, 3 s in all
    for (let i = 0; i < 30; i++) {
      t += 10000;
      steps.push(t, t + 99);
      re.test('z');
    }
    assert.equal(regexTrip('w:1', 'z'), null);
  } finally {
    resetRegexGuard();
    setRegexGuard({ now: () => performance.now(), onTrip: null, windowMs: 0, windowBudgetMs: 0 });
  }
});

test('the background spends at most BACKGROUND_WINDOW_BUDGET_MS a minute on one profile, however many detections a page starts (SEC22 L-E)', async () => {
  const { watchRegexes } = await import('../../src/background/profiles.js');
  const { setRegexGuard, resetRegexGuard, resetRegexBudget, guardedRegex, regexTrip, BUDGET_MS, BACKGROUND_WINDOW_BUDGET_MS, PAGE_WINDOW_MS } = await import('../../src/lib/regexguard.js');
  resetRegexGuard();
  watchRegexes();
  let t = 0;
  const steps = [];
  setRegexGuard({ now: () => (steps.length ? steps.shift() : t) });
  try {
    const re = guardedRegex('z', { owner: 'e:1' });
    // a detection every 1.5 s, each just under its own budget: 9 matches of 100 ms
    let spent = 0;
    let detections = 0;
    while (!regexTrip('e:1', 'z')) {
      if (detections++ > 100) throw new Error('never tripped');
      resetRegexBudget();
      for (let i = 0; i < 9 && !regexTrip('e:1', 'z'); i++) {
        steps.push(t, t + 100);
        re.test('z');
        t += 100;
        spent += 100;
      }
      t += 600;
    }
    assert.ok(t < PAGE_WINDOW_MS, `within one window: ${t} ms`);
    assert.equal(spent, BACKGROUND_WINDOW_BUDGET_MS + 100, 'it trips at the first match over the cap');
    assert.ok(spent / detections < BUDGET_MS, 'no detection went over its own budget');
    // the next window starts afresh: 0.9 s a detection, 3 detections, no trip
    resetRegexGuard();
    t = PAGE_WINDOW_MS * 2;
    for (let d = 0; d < 3; d++) {
      resetRegexBudget();
      for (let i = 0; i < 9; i++) {
        steps.push(t, t + 100);
        re.test('z');
        t += 100;
      }
    }
    assert.equal(regexTrip('e:1', 'z'), null, '2.7 s in a minute is within the cap');
  } finally {
    resetRegexGuard();
    setRegexGuard({ now: () => performance.now(), onTrip: null, windowMs: 0, windowBudgetMs: 0 });
  }
});

// ------------------------------------------------------------ what stays in memory (SEC25 低-2)

const PAGE = 'https://lectures.example.edu/watch/7';
const m3u8 = (i) => `https://lectures.example.edu/hls/${i}/index.m3u8`;
// a media playlist of n characters: one segment, the rest a comment
const playlist = (i, n) => {
  const head = `#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\nseg${i}.ts\n#EXT-X-ENDLIST\n`;
  return head + '#'.repeat(Math.max(0, n - head.length));
};

/** A page playing playlists of these sizes (in frame 0, or in frameId), answered by the page itself. */
function playlistPage(sizes, frameId = 0) {
  const texts = new Map(sizes.map((n, i) => [m3u8(i), playlist(i, n)]));
  const media = sizes.map((_, i) => video(m3u8(i)));
  reset({
    tabUrl: PAGE,
    frames: [frame(0, PAGE, frameId ? [] : media), ...(frameId ? [frame(frameId, PAGE, media)] : [])],
    pageFetch: (req) => (texts.has(req.url) && req.mode === 'text' ? { ok: true, status: 200, contentType: 'application/vnd.apple.mpegurl', text: texts.get(req.url) } : { ok: false, status: 404, contentType: '', size: -1, text: '' }),
  });
}

test('manifest texts: a tab keeps at most tabTextChars of them, in call order; its tracks all stay', async () => {
  const { setRetainLimits, keptTexts, lastResult, reportFor } = await import('../../src/background/detector.js');
  setRetainLimits({ textChars: 2000, tabTextChars: 3000, totalTextChars: 1e6 });
  try {
    playlistPage([1500, 1500, 2500, 1000]);
    const tab = nextTab++;
    const r = await detectTab(tab, { force: true });
    assert.equal(r.status, 'ok');
    assert.equal(r.tracks.length, 4, 'every playlist is a track');
    assert.deepEqual(keptTexts().get(tab), { chars: 3000, evicted: false });
    const texts = lastResult(tab).raw.detection.fetches.filter((f) => f.purpose === 'manifest');
    assert.deepEqual(texts.map((f) => [f.text.length, !!f.truncated]), [[1500, false], [1500, false], [0, true], [0, true]]);
    const rep = await reportFor(tab);
    assert.equal(rep.ok, true);
    assert.deepEqual(rep.report.inputs.fetches.filter((f) => f.purpose === 'manifest').map((f) => !!f.truncated), [false, false, true, true]);
    assert.equal(rep.report.expected.tracks.length, 4);
  } finally {
    setRetainLimits();
  }
});

test('manifest texts: an in-page frame’s detection keeps none, its tracks are listed; closing the tab forgets both', async () => {
  const { setRetainLimits, keptTexts, lastResult, forgetTab, viewOf } = await import('../../src/background/detector.js');
  setRetainLimits({ textChars: 2000, tabTextChars: 3000, totalTextChars: 1e6 });
  try {
    playlistPage([1000, 1000], 4);
    const tab = nextTab++;
    const top = await detectTab(tab, { force: true });
    playlistPage([1000, 1000], 4);
    const inFrame = await detectTab(tab, { force: true, frameId: 4, frameUrl: PAGE });
    assert.equal(top.status, 'ok');
    assert.equal(inFrame.status, 'ok');
    assert.equal(inFrame.tracks.length, 2);
    assert.deepEqual(keptTexts().get(`${tab}:4`), { chars: 0, evicted: false });
    assert.ok(keptTexts().get(tab).chars > 0, 'the top-level detection keeps its texts');
    assert.equal(viewOf(lastResult(tab, 4)).tracks.length, 2);
    forgetTab(tab);
    assert.ok(lastResult(tab) === null && lastResult(tab, 4) === null);
    assert.ok(!keptTexts().has(tab) && !keptTexts().has(`${tab}:4`), 'released');
  } finally {
    setRetainLimits();
  }
});

test('manifest texts: all tabs keep at most totalTextChars, the oldest giving theirs up; its report detects again; its tracks still send', async () => {
  const { setRetainLimits, keptTexts, lastResult, reportFor, forgetTab, viewOf } = await import('../../src/background/detector.js');
  const { sendResult } = await import('../../src/background/actions.js');
  const { shownOf, selectionError, presetSelection } = await import('../../src/lib/send.js');
  setRetainLimits({ textChars: 2000, tabTextChars: 3000, totalTextChars: 7000 });
  const tabs = [nextTab++, nextTab++, nextTab++];
  try {
    for (const tab of tabs) {
      playlistPage([1500, 1500]);
      await detectTab(tab, { force: true });
    }
    const [a, b, c] = tabs;
    assert.deepEqual(keptTexts().get(a), { chars: 0, evicted: true }, 'the oldest gave its texts up');
    assert.deepEqual(keptTexts().get(b), { chars: 3000, evicted: false });
    assert.deepEqual(keptTexts().get(c), { chars: 3000, evicted: false });
    // the tracks it showed are all there, and a send takes them
    const ra = lastResult(a);
    assert.equal(ra.tracks.length, 2);
    const ids = presetSelection(shownOf(ra), 'main', null);
    assert.ok(ids && ids.length > 0);
    assert.equal(selectionError(shownOf(ra), ids, { maxVideo: 2 }), '');
    assert.deepEqual(viewOf(ra).tracks.map((t) => t.id), ra.tracks.map((t) => t.id));
    // a track id it does not show is refused before anything else happens
    const refused = await sendResult(ra, { trackIds: ['https://lectures.example.edu/other.mp4'], courseId: 'c1' }, { tabId: a, topUrl: PAGE });
    assert.equal(refused.ok, false);
    assert.equal(refused.error.code, 'ext.selection.unknown_track');
    // its report detects again, whole; another tab gives up instead
    playlistPage([1500, 1500]);
    const rep = await reportFor(a);
    assert.equal(rep.ok, true);
    assert.deepEqual(rep.report.inputs.fetches.filter((f) => f.purpose === 'manifest').map((f) => !!f.truncated), [false, false]);
    assert.deepEqual(keptTexts().get(a), { chars: 3000, evicted: false });
    assert.deepEqual(keptTexts().get(b), { chars: 0, evicted: true });
  } finally {
    setRetainLimits();
    for (const tab of tabs) forgetTab(tab);
  }
});

test('scans kept for reports: all tabs keep at most totalScanChars of addresses, the largest (of equal ones the oldest) giving its scan (and texts) up; its report detects again; its tracks stay (limits.md)', async () => {
  const { setRetainLimits, keptTexts, keptScans, lastResult, reportFor, forgetTab } = await import('../../src/background/detector.js');
  const tabs = [nextTab++, nextTab++, nextTab++];
  // what other tests left stored would count too
  for (const k of keptScans().keys()) forgetTab(Number(String(k).split(':')[0]));
  try {
    playlistPage([1500, 1500]);
    await detectTab(tabs[0], { force: true });
    const one = keptScans().get(tabs[0]).chars;
    // the page address, two elements with src and currentSrc
    assert.equal(one, PAGE.length + 4 * m3u8(0).length);
    // room for two such scans
    setRetainLimits({ totalScanChars: 2 * one + 1 });
    for (const tab of tabs.slice(1)) {
      playlistPage([1500, 1500]);
      await detectTab(tab, { force: true });
    }
    const [a, b, c] = tabs;
    assert.deepEqual(keptScans().get(a), { chars: 0, evicted: true }, 'the oldest gave its scan up');
    assert.deepEqual(keptTexts().get(a), { chars: 0, evicted: true }, 'and its texts: its report detects again anyway');
    assert.deepEqual([lastResult(a).raw.frames, lastResult(a).raw.webRequests], [[], []]);
    assert.deepEqual(keptScans().get(b), { chars: one, evicted: false });
    assert.deepEqual(keptScans().get(c), { chars: one, evicted: false });
    assert.equal(lastResult(a).tracks.length, 2, 'its tracks stay');
    playlistPage([1500, 1500]);
    const rep = await reportFor(a);
    assert.equal(rep.ok, true);
    assert.equal(rep.report.expected.tracks.length, 2);
    assert.deepEqual(keptScans().get(a), { chars: one, evicted: false });
    assert.equal(keptScans().get(b).evicted, true, 'another tab gives up instead');
  } finally {
    setRetainLimits();
    for (const tab of tabs) forgetTab(tab);
  }
});

test('scans kept for reports: past totalScanChars the largest give theirs up first, not the small tabs opened earlier (SEC36 低-10)', async () => {
  const { setRetainLimits, keptScans, forgetTab } = await import('../../src/background/detector.js');
  const tabs = [nextTab++, nextTab++, nextTab++, nextTab++];
  for (const k of keptScans().keys()) forgetTab(Number(String(k).split(':')[0]));
  const sizes = (n) => Array.from({ length: n }, () => 100);
  try {
    playlistPage(sizes(1));
    await detectTab(tabs[0], { force: true });
    const small = keptScans().get(tabs[0]).chars;
    forgetTab(tabs[0]);
    playlistPage(sizes(30));
    await detectTab(tabs[2], { force: true });
    const big = keptScans().get(tabs[2]).chars;
    forgetTab(tabs[2]);
    assert.ok(big > 10 * small);
    // room for the two small ones and one big one
    setRetainLimits({ totalScanChars: 2 * small + big + 1 });
    const [a, b, c, d] = tabs;
    for (const [tab, n] of [[a, 1], [b, 1], [c, 30], [d, 30]]) {
      playlistPage(sizes(n));
      await detectTab(tab, { force: true });
    }
    assert.deepEqual([a, b, c, d].map((t) => keptScans().get(t).evicted), [false, false, true, false], 'the other big one gave its scan up');
  } finally {
    setRetainLimits();
    for (const tab of tabs) forgetTab(tab);
  }
});

test('in-page detections keep no scan (no report is made of them), so they never count against totalScanChars (SEC36 低-10)', async () => {
  const { forgetTab, keptScans } = await import('../../src/background/detector.js');
  const tab = nextTab++;
  try {
    playlistPage([1000, 1000], 4);
    const r = await detectTab(tab, { force: true, frameId: 4, frameUrl: PAGE });
    assert.equal(r.status, 'ok');
    assert.equal(r.tracks.length, 2);
    assert.deepEqual([r.raw.frames, r.raw.webRequests], [[], []]);
    assert.deepEqual(keptScans().get(`${tab}:4`), { chars: 0, evicted: false });
  } finally {
    forgetTab(tab);
  }
});

test('in-page detections: a tab keeps at most frameResults of them, the oldest going first; the tab’s own detection and other tabs’ are untouched (limits.md)', async () => {
  const { setRetainLimits, lastResult, forgetTab } = await import('../../src/background/detector.js');
  setRetainLimits({ frameResults: 2 });
  const [tab, other] = [nextTab++, nextTab++];
  try {
    playlistPage([1000]);
    await detectTab(tab, { force: true });
    playlistPage([1000], 4);
    await detectTab(other, { force: true, frameId: 4, frameUrl: PAGE });
    for (const f of [4, 5, 6]) {
      playlistPage([1000], f);
      assert.equal((await detectTab(tab, { force: true, frameId: f, frameUrl: PAGE })).status, 'ok');
    }
    assert.equal(lastResult(tab, 4), null, 'the oldest in-page detection is given up');
    assert.ok(lastResult(tab, 5) && lastResult(tab, 6));
    assert.ok(lastResult(tab) && lastResult(other, 4));
    // detecting a kept frame again keeps the count
    playlistPage([1000], 5);
    await detectTab(tab, { force: true, frameId: 5, frameUrl: PAGE });
    assert.ok(lastResult(tab, 5) && lastResult(tab, 6));
  } finally {
    setRetainLimits();
    forgetTab(tab);
    forgetTab(other);
  }
});

test('a stored detection keeps at most tracksKept tracks: the shown ones are the same, the counts are of all, the report says how many it gave up (SEC28 低-1)', { timeout: 20000 }, async () => {
  const { setRetainLimits, viewOf, reportFor, forgetTab } = await import('../../src/background/detector.js');
  const { validate } = await import('../../src/lib/jsonschema.js');
  const reportSchema = JSON.parse(fs.readFileSync(path.join(root, 'schema', 'report.schema.json'), 'utf8'));
  const page = 'https://lectures.example.edu/watch/7';
  // 450 video files with names, not numbers (numbered ones would be a run
  // of segments), and one audio file, listed last (video sorts first) but
  // shown: it is the main audio of the choices
  const name = (i) => String.fromCharCode(97 + (Math.floor(i / 676) % 26), 97 + (Math.floor(i / 26) % 26), 97 + (i % 26));
  const audio = 'https://lectures.example.edu/talk.m4a';
  const elements = [...Array.from({ length: 450 }, (_, i) => video(`https://lectures.example.edu/v-${name(i)}.mp4`)), { tag: 'audio', src: audio, currentSrc: audio, sources: [], mediaKeys: false, duration: 60, readyState: 4 }];
  const badges = [];
  const setBadgeText = globalThis.browser.action.setBadgeText;
  globalThis.browser.action.setBadgeText = async (o) => {
    badges.push(o);
  };
  const tabs = [];
  const run = async () => {
    reset({ tabUrl: page, frames: [frame(0, page, elements)] });
    tabs.push(nextTab);
    const r = await detect();
    return { r, badge: badges.filter((b) => b.tabId === r.tabId).map((b) => b.text) };
  };
  try {
    const whole = await run(); // the real limit (1000): every track stays
    setRetainLimits({ tracksKept: 300 });
    const cut = await run();
    assert.equal(whole.r.tracks.length, 451);
    assert.equal(whole.r.tracksDropped, 0);
    assert.equal(whole.r.tracks[450].url, audio, 'the audio is listed last');
    assert.equal(cut.r.tracks.length, 300);
    assert.equal(cut.r.tracksDropped, 151);
    assert.equal(cut.r.summary.total, 451, 'the header counts every track');
    assert.deepEqual(cut.badge, ['451'], 'so does the badge');
    const [vw, vc] = [viewOf(whole.r), viewOf(cut.r)];
    assert.ok(vw.tracks.some((t) => t.id === audio));
    assert.deepEqual(vc.tracks.map((t) => t.id), vw.tracks.map((t) => t.id), 'the same tracks shown, the audio among them');
    assert.equal(vc.hiddenTracks, vw.hiddenTracks);
    assert.equal(vc.hiddenTracks, 251);
    const rw = await reportFor(whole.r.tabId);
    const rc = await reportFor(cut.r.tabId);
    assert.ok(rw.ok && rc.ok);
    assert.ok(!('limits' in rw.report), 'within the limits: no `limits`');
    assert.deepEqual(rc.report.limits, { tracks: 151 });
    assert.equal(rc.report.expected.tracks.length, 300);
    assert.deepEqual(validate(reportSchema, rc.report), []);
  } finally {
    setRetainLimits();
    globalThis.browser.action.setBadgeText = setBadgeText;
    for (const t of tabs) forgetTab(t);
  }
});

test('the scan a detection keeps is held to LIMITS; the report says what was left out', { timeout: 20000 }, async () => {
  const { lastResult, reportFor, forgetTab } = await import('../../src/background/detector.js');
  const { LIMITS } = await import('../../src/lib/detect.js');
  const page = 'https://lectures.example.edu/watch/7';
  const long = `https://lectures.example.edu/${'l'.repeat(LIMITS.urlLength)}.mp4`;
  const elements = Array.from({ length: LIMITS.elements + 5 }, () => ({ tag: 'video', src: '', currentSrc: '', sources: [], duration: 60 }));
  elements[0] = video(long);
  elements[1] = video('https://lectures.example.edu/talk.mp4');
  reset({ tabUrl: page, frames: [frame(0, page, elements)] });
  const tab = nextTab;
  try {
    const r = await detect();
    assert.equal(r.status, 'ok');
    assert.equal(r.raw.frames[0].elements.length, LIMITS.elements);
    assert.equal(r.raw.frames[0].elements[0].src, '', 'the long address is not kept');
    assert.deepEqual(r.tracks.map((t) => t.url), ['https://lectures.example.edu/talk.mp4']);
    const rep = await reportFor(tab);
    assert.ok(rep.ok);
    assert.deepEqual(rep.report.limits, { elements: 5, longUrls: 2 });
    assert.ok(lastResult(tab) === r);
  } finally {
    forgetTab(tab);
  }
});

test('the card’s view: at most tracksShown tracks and how many were left out; the choices’ tracks among them', { timeout: 10000 }, async () => {
  const { viewOf } = await import('../../src/background/detector.js');
  const { RETAIN_LIMITS } = await import('../../src/lib/retain.js');
  const tr = (i, kind) => ({ id: `${kind}${i}`, url: `https://cdn.example.net/${kind}${i}.mp4`, kind, format: 'file', sizeBytes: 10, sizeEstimated: false, sources: ['element'], protected: false });
  const base = { status: 'ok', authorized: true, profile: null, pageUrl: PAGE, lecture: {}, downloadPolicy: 'unknown', observe: { declared: false, seen: false }, presets: null, named: null };
  const many = [...Array.from({ length: 450 }, (_, i) => tr(i, 'video')), tr(0, 'audio')];
  const v = viewOf({ ...base, tracks: many, summary: { total: many.length } });
  assert.equal(v.tracks.length, RETAIN_LIMITS.tracksShown);
  assert.equal(v.hiddenTracks, many.length - RETAIN_LIMITS.tracksShown);
  assert.equal(v.summary.total, many.length, 'the counts are of all tracks');
  assert.ok(v.tracks.some((t) => t.id === 'audio0'), 'the main audio is shown');
  const few = many.slice(0, 30);
  const w = viewOf({ ...base, tracks: few, summary: { total: 30 } });
  assert.equal(w.tracks.length, 30);
  assert.equal(w.hiddenTracks, 0);
  // of a track's address the view has its host name only (the card's "在 <主机> 上")
  const odd = [
    { ...tr(1, 'audio'), id: 'x1', url: 'https://user:secret@Media.Example.org:8443/sessions/123456/a.m4a?Signature=MARKER#t=1' },
    { ...tr(2, 'audio'), id: 'x2', url: 'blob:https://lectures.example.edu/0000' },
    { ...tr(3, 'audio'), id: 'x3', url: '' },
  ];
  const hv = viewOf({ ...base, tracks: odd, summary: { total: 3 } });
  assert.deepEqual(hv.tracks.map((t) => [t.id, t.host]), [['x1', 'media.example.org'], ['x2', ''], ['x3', '']]);
  const text = JSON.stringify(hv.tracks);
  for (const part of ['secret', 'user', '8443', 'sessions', '123456', 'a.m4a', 'Signature', 'MARKER', '0000']) assert.ok(!text.includes(part), part);
  // a send (and "续传") takes only what the view showed: a track left out
  // is unknown there, before LectureFlow is asked or a cookie is read
  const { sendResult, refreshResult } = await import('../../src/background/actions.js');
  const hiddenId = many.find((t) => !v.tracks.some((s) => s.id === t.id)).id;
  const result = { ...base, tracks: many, summary: { total: many.length }, sessionId: 's', request: {} };
  for (const r of [await sendResult(result, { trackIds: [hiddenId], courseId: 'c1' }, { tabId: 1, topUrl: PAGE }), await refreshResult(result, { trackIds: [hiddenId] }, { tabId: 1, topUrl: PAGE })]) {
    assert.equal(r.ok, false);
    assert.equal(r.error.code, 'ext.selection.unknown_track');
  }
});
