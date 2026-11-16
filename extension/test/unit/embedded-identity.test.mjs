// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A profile that matches through an embedded frame (security audit AUD3
// 低-1, the auditor's probe P3 made a test): the task's identity (platform,
// session id), its title and its tracks all come from the frame that
// matched. A page that embeds a frame whose address looks like a Panopto
// player cannot send its own media, or its own title, as "Panopto + that
// session"; an LMS page embedding the real player still sends the
// player's lecture. src/background/detector.js and actions.js driven as a
// whole against a fake `browser`; marker values only.

import { test, beforeEach } from 'node:test';
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

const world = { tabUrl: '', frames: [], reads: {}, native: [] };

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

globalThis.browser = {
  runtime: { getURL: (rel) => `ext://lf/${rel}`, getManifest: () => ({ version: '0.0.0' }), id: 'lf', connectNative: () => fakePort() },
  i18n: { getUILanguage: () => 'en', getMessage: (k) => k },
  storage: {
    local: { get: async () => ({}), set: async () => {}, remove: async () => {} },
    session: { get: async () => ({}), set: async () => {}, remove: async () => {} },
  },
  permissions: { contains: async () => false, getAll: async () => ({ origins: [] }) },
  tabs: { get: async () => ({ url: world.tabUrl }) },
  action: { setBadgeText: async () => {}, setBadgeBackgroundColor: async () => {}, setBadgeTextColor: async () => {} },
  cookies: { getAll: async () => [] },
  scripting: {
    async executeScript({ target, func, args }) {
      if (func.name === 'scanFrame') {
        const pick = target.allFrames ? world.frames : world.frames.filter((f) => target.frameIds.includes(f.frameId));
        const wanted = Array.isArray(args[0]) ? args[0] : [];
        return pick.map((f) => {
          const result = structuredClone(f.result);
          // the second pass: each frame answers the profile's selectors from its own document
          const answers = world.reads[f.frameId] || {};
          result.reads = wanted.map((rd) => ({ key: rd.key, selector: rd.selector, attr: rd.attr, count: rd.key in answers ? 1 : 0, value: answers[rd.key] || '', nodes: [] }));
          return { frameId: f.frameId, documentId: f.documentId, result };
        });
      }
      if (func.name === 'frameTitle') return [{ frameId: 0, result: { url: world.tabUrl, title: 'The embedding page', history: [], ancestors: [] } }];
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

const { detectTab } = await import('../../src/background/detector.js');
const { sendResult, refreshResult } = await import('../../src/background/actions.js');
const { listedTracks } = await import('../../src/lib/send.js');

let nextTab = 1;
beforeEach(() => Object.assign(world, { tabUrl: '', frames: [], reads: {}, native: [] }));

const frame = (frameId, url, elements = []) => ({ frameId, result: { url, elements, resources: [], reads: [], encryptedEvents: 0, observed: [], opaque: [], docs: [], ancestors: [] } });
const video = (s, duration = 3000) => ({ tag: 'video', src: s, currentSrc: s, sources: [], mediaKeys: false, videoWidth: 1280, videoHeight: 720, duration, readyState: 4 });
const sent = (type) => world.native.filter((m) => m.type === type);

const GUID = '11111111-2222-3333-4444-555555555555';
const TENANT = 'https://tenant.hosted.panopto.com';
const EMBED = `${TENANT}/Panopto/Pages/Embed.aspx?id=${GUID}`;
const X = 'https://attacker.example.org/watch';
const EVIL = 'https://media.attacker.example.org/fake-lecture.mp4';

test('a page embeds a frame at a Panopto player address and shows its own video: generic, the page’s own identity (AUD3 低-1, P3)', async () => {
  world.tabUrl = X;
  world.frames = [frame(0, X, [video(EVIL)]), frame(5, EMBED, [])];
  // the embedding page dresses its own heading up as the profile's title selector
  world.reads = { 0: { 'lecture.title.0': 'CSC1003 Week 5 (written by the embedding page)' } };
  const r = await detectTab(nextTab++, { force: true });
  assert.equal(r.status, 'ok');
  assert.equal(r.profile, null, 'no track of the Panopto frame: not a Panopto task');
  assert.equal(r.sessionSource, 'generic');
  assert.notEqual(r.sessionId, GUID);
  assert.equal(r.lecture.title, 'The embedding page', 'the page’s own title, as generic detection names it');
  assert.deepEqual(listedTracks(r).map((t) => t.url), [EVIL]);
  const id = listedTracks(r)[0].id;
  const where = { tabId: r.tabId, topUrl: r.pageUrl, storeId: '' };
  assert.equal((await sendResult(r, { trackIds: [id], courseId: 'c1' }, where)).ok, true);
  const [enq] = sent('enqueue');
  assert.deepEqual([enq.platform, enq.profile.id, enq.sessionId === GUID], ['generic', 'generic', false]);
  assert.equal((await refreshResult(r, { trackIds: [id] }, where)).ok, true);
  const [ref] = sent('refresh');
  assert.deepEqual([ref.platform, ref.sessionId === GUID], ['generic', false], '续传 cannot take over the Panopto session either');
});

test('an LMS page embedding the real Panopto player: the player’s session, title, duration and tracks only', async () => {
  const lms = 'https://lms.example.edu/course/1';
  const own = `${TENANT}/Panopto/Content/sessions/${GUID}/lecture.mp4`;
  const lmsVideo = 'https://lms.example.edu/media/welcome.mp4';
  world.tabUrl = lms;
  world.frames = [frame(0, lms, [video(lmsVideo, 9999)]), frame(5, EMBED, [video(own, 3000)])];
  world.reads = {
    0: { 'lecture.title.0': 'Not the lecture (the LMS page’s own heading)' },
    5: { 'lecture.title.0': 'CSC1003 Week 5 Oct 21 9:35 AM CEST 2026' },
  };
  const r = await detectTab(nextTab++, { force: true });
  assert.equal(r.profile && r.profile.id, 'panopto');
  assert.equal(r.sessionId, GUID);
  assert.equal(r.lecture.title, 'CSC1003 Week 5 Oct 21 9:35 AM CEST 2026', 'the player frame’s title, not the LMS page’s');
  assert.equal(r.lecture.durationSec, 3000, 'the player frame’s media, not the LMS page’s');
  assert.deepEqual(listedTracks(r).map((t) => t.url), [own], 'the LMS page’s own video is not part of the Panopto task');
  assert.ok(listedTracks(r).every((t) => t.frameIds.includes(5)));
  const where = { tabId: r.tabId, topUrl: r.pageUrl, storeId: '' };
  assert.equal((await sendResult(r, { trackIds: [listedTracks(r)[0].id], courseId: 'c1' }, where)).ok, true);
  const [enq] = sent('enqueue');
  assert.deepEqual([enq.platform, enq.sessionId, enq.tracks.map((t) => t.url)], ['panopto', GUID, [own]]);
});

test('the top page itself is the Panopto viewer: as before, all of its tracks', async () => {
  const viewer = `${TENANT}/Panopto/Pages/Viewer.aspx?id=${GUID}`;
  const a = `${TENANT}/Panopto/Content/a.mp4`;
  world.tabUrl = viewer;
  world.frames = [frame(0, viewer, [video(a)])];
  const r = await detectTab(nextTab++, { force: true });
  assert.equal(r.profile && r.profile.id, 'panopto');
  assert.equal(r.sessionId, GUID);
  assert.deepEqual(listedTracks(r).map((t) => t.url), [a]);
});
