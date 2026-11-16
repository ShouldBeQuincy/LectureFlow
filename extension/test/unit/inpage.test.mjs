// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// In-page entry points (src/lib/inpage.js) and the profile fields behind
// them: which pages get what, the content script's configuration, the
// sessionInfo request (same origin, only {sessionId}), reading its answer
// (declared pointers only), a listed session as a detection result,
// bulk choices, row states; and the engine's combined rules (generic
// tracks plus URLs a response named), responseWhen, observed lecture
// reads and the schema's own checks.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  inpageModeFor, findInpage, inpageMatches, inpageConfig, rowSessionId, sessionInfoRequest, extractPointers, sessionResult,
  bulkTrackIds, rowState, bulkCounts, sessionInfoPointers,
} from '../../src/lib/inpage.js';
import { checkProfile, applyProfile, lectureFor } from '../../src/lib/profile.js';
import { profileKey } from '../../src/lib/observe.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const schema = JSON.parse(fs.readFileSync(path.join(root, 'schema', 'profile.schema.json'), 'utf8'));
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
const clone = (o) => JSON.parse(JSON.stringify(o));
const LIST = 'https://tenant.cloud.panopto.eu/Panopto/Pages/Sessions/List.aspx';
const ID = 'a0000000-0000-4000-8000-0000000000a1';

/** A DeliveryInfo answer for a listed session; UserEmail must never be taken. */
function info({ downloads = true, podcast = true, two = false } = {}) {
  const base = `https://d0000fixture00.cloudfront.net/sessions/${ID}`;
  return {
    Delivery: {
      SessionName: 'CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026',
      Duration: 3300.4,
      IsPrimaryAudioOnly: true,
      Streams: [
        { StreamUrl: `${base}/${ID}-9c8d7e6f-0000-4000-8000-0000000000e3.hls/master.m3u8` },
        { StreamUrl: `${base}/abcdef01-0000-4000-8000-0000000000e4.object.hls/master.m3u8` },
        ...(two ? [{ StreamUrl: `${base}/abcdef02-0000-4000-8000-0000000000e5.object.hls/master.m3u8` }] : []),
      ],
      PodcastStreams: podcast ? [{ StreamUrl: `${base}/${ID}.mp4` }] : [],
    },
    DownloadUrl: downloads ? 'https://tenant.cloud.panopto.eu/Panopto/Podcast/Download/x.mp4' : null,
    UserEmail: 'unit-student@example.edu',
  };
}

test('the Panopto profile passes, with its in-page fields', () => {
  assert.deepEqual(checkProfile(schema, panopto), []);
  assert.ok(panopto.inpage.player.length >= 2);
  assert.deepEqual(panopto.presets, { full: ['podcast'], audio: ['primary-audio'] });
});

test('the schema and checks refuse a broken sessionInfo or in-page block (反例)', () => {
  const bad = (mutate) => {
    const p = clone(panopto);
    mutate(p);
    return checkProfile(schema, p);
  };
  assert.ok(bad((p) => (p.sessionInfo.path = 'https://evil.example.com/x')).length > 0, 'a path, never a URL');
  assert.ok(bad((p) => (p.sessionInfo.path = '//evil.example.com/x')).length > 0);
  assert.ok(bad((p) => (p.sessionInfo.path = '/Panopto/Other.aspx')).some((e) => /urlPattern/.test(e.message)), 'must be the observed request');
  assert.ok(bad((p) => (p.sessionInfo.form.deliveryId = '{cookie}')).length > 0, 'only {sessionId}');
  assert.ok(bad((p) => (p.sessionInfo.method = 'PUT')).length > 0);
  assert.ok(bad((p) => (p.sessionInfo.observe = 3)).some((e) => /observe entry/.test(e.message)));
  assert.ok(bad((p) => delete p.sessionInfo).some((e) => /needs sessionInfo/.test(e.message)));
  assert.ok(bad((p) => (p.presets.full = ['nope'])).some((e) => /presets/.test(e.path)));
  assert.ok(bad((p) => (p.tracks[1].detect.responseWhen.observed = 'nope')).length > 0);
  assert.ok(bad((p) => (p.lecture.title[4].observed = 'nope')).length > 0);
  assert.ok(bad((p) => (p.inpage.player[0].position = 'inside')).length > 0);
  assert.ok(bad((p) => (p.inpage.list.folder = '.folder-name')).length > 0, 'no folder field');
  assert.ok(bad((p) => (p.tracks[0].detect = { kind: 'video' })).length > 0, 'urlPattern or fromResponse');
});

test('which pages get buttons: the player pages and the list, on the profile’s hosts', () => {
  assert.equal(inpageModeFor(panopto, `https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=${ID}`), 'player');
  assert.equal(inpageModeFor(panopto, `https://tenant.hosted.panopto.com/Panopto/Pages/Embed.aspx?id=${ID}`), 'player');
  assert.equal(inpageModeFor(panopto, `${LIST}#folderID=x`), 'list');
  assert.equal(inpageModeFor(panopto, 'https://tenant.cloud.panopto.eu/Panopto/Pages/Home.aspx'), '');
  assert.equal(inpageModeFor(panopto, 'https://lms.example.edu/Panopto/Pages/Sessions/List.aspx'), '', 'another host');
  assert.equal(findInpage([{ profile: panopto, reviewed: true }], LIST).mode, 'list');
  assert.equal(findInpage([{ profile: { ...panopto, inpage: undefined }, reviewed: true }], LIST), null);
  // registration: granted ∩ hosts of profiles with inpage
  assert.deepEqual(inpageMatches(['https://myuni.cloud.panopto.eu/*', 'https://video.example.edu/*'], [panopto]), ['https://myuni.cloud.panopto.eu/*']);
  assert.deepEqual(inpageMatches(['https://video.example.edu/*'], [panopto]), []);
});

test('the content script’s configuration: placements, the list, the request template, the pointers', () => {
  const cfg = inpageConfig({ profile: panopto, reviewed: true, mode: 'list' });
  assert.equal(cfg.key, profileKey(panopto));
  assert.equal(cfg.list.row, 'tr.list-view-row');
  assert.equal(cfg.list.idAttr, 'id');
  assert.equal(cfg.sessionInfo.method, 'POST');
  assert.deepEqual(cfg.pointers, panopto.observe[0].pointers);
  assert.deepEqual(sessionInfoPointers(panopto), panopto.observe[0].pointers);
  assert.deepEqual(cfg.presets, panopto.presets);
  assert.ok(!JSON.stringify(cfg).includes('folder'), 'nothing about folders');
});

test('row ids: the attribute narrowed by the pattern; anything else is not a session', () => {
  const list = { idPattern: panopto.inpage.list.idPattern };
  assert.equal(rowSessionId(ID, list), ID);
  assert.equal(rowSessionId(ID.toUpperCase(), list), ID.toUpperCase());
  assert.equal(rowSessionId('row-3', list), '');
  assert.equal(rowSessionId('', list), '');
  assert.equal(rowSessionId('a b', {}), '');
  assert.equal(rowSessionId('{x}', {}), '');
});

test('the sessionInfo request: the frame’s own origin, the declared form, {sessionId} only', () => {
  const r = sessionInfoRequest(panopto.sessionInfo, ID, `${LIST}?embedded=1`);
  assert.equal(r.url, 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer/DeliveryInfo.aspx');
  assert.equal(r.method, 'POST');
  const form = new URLSearchParams(r.body);
  assert.equal(form.get('deliveryId'), ID);
  assert.equal(form.get('responseType'), 'json');
  assert.equal(form.get('isEmbed'), 'false');
  const get = sessionInfoRequest({ method: 'GET', path: '/api/session', form: { id: '{sessionId}', x: 'a&b' } }, 's 1', 'https://video.example.edu/list');
  assert.equal(get.url, 'https://video.example.edu/api/session?id=s+1&x=a%26b');
  assert.equal(get.body, null);
  assert.equal(sessionInfoRequest({ method: 'GET', path: '//evil.example.com/x' }, ID, LIST), null);
  assert.equal(sessionInfoRequest(panopto.sessionInfo, '', LIST), null);
  assert.equal(sessionInfoRequest(panopto.sessionInfo, ID, 'not a url'), null);
});

test('reading the answer: the declared pointers, never the rest', () => {
  const v = extractPointers(info(), sessionInfoPointers(panopto));
  assert.equal(v.downloadUrl, 'https://tenant.cloud.panopto.eu/Panopto/Podcast/Download/x.mp4');
  assert.equal(v.podcastUrl.length, 1);
  assert.equal(v.streamUrls.length, 2);
  assert.equal(v.primaryAudioOnly, true);
  assert.equal(v.duration, 3300.4);
  assert.ok(!JSON.stringify(v).includes('unit-student'));
});

test('a listed session as a detection result: tracks from the answer, title, time, switch, presets', () => {
  const found = { profile: panopto, reviewed: true, mode: 'list' };
  const values = extractPointers(info({ downloads: false }), sessionInfoPointers(panopto));
  const r = sessionResult({ found, sessionId: ID, values: { ...values, UserEmail: 'unit-student@example.edu' }, frameUrl: LIST, locale: 'zh-CN' });
  assert.equal(r.status, 'ok');
  assert.equal(r.sessionId, ID);
  assert.equal(r.profile.id, 'panopto');
  assert.equal(r.downloadPolicy, 'disabled');
  assert.equal(r.lecture.title, 'CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026');
  assert.equal(r.lecture.recordedAt, '2026-10-21T09:35', 'the session name’s 09:35 BST as it reads');
  assert.equal(r.lecture.durationSec, 3300);
  assert.deepEqual(r.named.tracks.map((t) => `${t.role}:${t.kind}:${t.format}:${t.recommended}`), ['podcast:video:file:true', 'primary-audio:audio:hls:false', 'screen:video:hls:false']);
  assert.equal(r.named.tracks[0].hint, '画面 + 声音，一个文件');
  assert.deepEqual(r.presets, panopto.presets);
  assert.ok(!JSON.stringify(r).includes('unit-student'), 'undeclared fields never enter the result');
  // values that do not fit (another profile version, bad shapes) give nothing
  assert.equal(sessionResult({ found, sessionId: ID, values: {}, frameUrl: 'not a url' }), null);
});

test('responseWhen: the primary stream is audio or video by the observed flag', () => {
  const values = extractPointers({ ...info(), Delivery: { ...info().Delivery, IsPrimaryAudioOnly: false } }, sessionInfoPointers(panopto));
  const r = sessionResult({ found: { profile: panopto, reviewed: true }, sessionId: ID, values, frameUrl: LIST });
  assert.deepEqual(r.named.tracks.map((t) => t.role), ['podcast', 'primary-video', 'screen']);
});

test('combined rules on a player page: a named stream generic detection found is that track, once', () => {
  const master = `https://d0000fixture00.cloudfront.net/sessions/${ID}/${ID}-9c8d7e6f-0000-4000-8000-0000000000e3.hls/master.m3u8`;
  const generic = [{ id: `${master}?InvocationID=x`, url: `${master}?InvocationID=x`, kind: 'audio', format: 'hls', sizeBytes: 5000, sources: ['performance'], frameIds: [0], protected: false }];
  const observed = { seen: new Set([0]), values: extractPointers(info(), sessionInfoPointers(panopto)), urls: {} };
  const named = applyProfile(panopto, generic, 'en', observed);
  const audio = named.tracks.filter((t) => t.role === 'primary-audio');
  assert.equal(audio.length, 1);
  assert.equal(audio[0].sizeBytes, 5000, 'generic detection’s data kept');
  assert.deepEqual(audio[0].sources, ['performance', 'response']);
  assert.equal(named.others.length, 0);
  // observed lecture reads after the selectors
  const lec = lectureFor({ profile: panopto }, {}, 0, observed);
  assert.equal(lec.title, 'CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026');
});

test('bulk choices per session and row states', () => {
  const found = { profile: panopto, reviewed: true };
  const one = sessionResult({ found, sessionId: ID, values: extractPointers(info(), sessionInfoPointers(panopto)), frameUrl: LIST });
  const tracks = one.named.tracks;
  const id = (role) => tracks.find((t) => t.role === role).id;
  assert.deepEqual(bulkTrackIds(tracks, 'main', panopto.presets), [id('podcast')]);
  assert.deepEqual(bulkTrackIds(tracks, 'audio', panopto.presets), [id('primary-audio')]);
  assert.deepEqual(bulkTrackIds(tracks, 'dual', panopto.presets), [id('podcast')], 'one picture: falls back to main');
  const two = sessionResult({ found, sessionId: ID, values: extractPointers(info({ two: true }), sessionInfoPointers(panopto)), frameUrl: LIST }).named.tracks;
  assert.equal(bulkTrackIds(two, 'dual', panopto.presets).length, 3);
  const noPod = sessionResult({ found, sessionId: ID, values: extractPointers(info({ podcast: false }), sessionInfoPointers(panopto)), frameUrl: LIST }).named.tracks;
  assert.equal(bulkTrackIds(noPod, 'main', panopto.presets).length, 2, 'audio + the first picture');
  assert.equal(rowState(null), 'new');
  assert.equal(rowState({ state: 'imported', matchedBy: 'title' }), 'imported');
  assert.equal(rowState({ state: 'downloading' }), 'active');
  assert.equal(rowState({ state: 'needs_refresh' }), 'refresh');
  assert.equal(rowState({ state: 'failed' }), 'again');
  const rows = [{ sessionId: 'a' }, { sessionId: 'b' }, { sessionId: 'c' }, { sessionId: 'd' }];
  assert.deepEqual(bulkCounts(rows, [{ sessionId: 'b', state: 'imported' }, { sessionId: 'c', state: 'queued' }], new Set(['a', 'd'])), { total: 4, imported: 1, active: 1, refresh: 0, toSend: 2 });
  // a lecture that needs a refresh is not being fetched (T26 G5): counted apart
  assert.deepEqual(bulkCounts(rows, [{ sessionId: 'b', state: 'needs_refresh' }, { sessionId: 'c', state: 'downloading' }, { sessionId: 'd', state: 'needs_refresh' }], new Set(['a'])), { total: 4, imported: 0, active: 1, refresh: 2, toSend: 1 });
});
