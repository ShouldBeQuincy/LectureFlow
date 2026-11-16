// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  checkProfile, profileMatchesUrl, findProfile, profilePermissionOrigins, selectorReads, sessionIdFor,
  lectureFor, downloadPolicyFor, applyProfile, localizedLabel, readResults, cleanText, pageTitleFor, isPlaceholderTitle,
} from '../../src/lib/profile.js';
import { normalizeRecordedAt, parseDurationSec } from '../../src/lib/dates.js';
import { detectTracks } from '../../src/lib/detect.js';
import { genericSessionId } from '../../src/lib/url.js';
import * as S from '../support/synth.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const schema = JSON.parse(fs.readFileSync(path.join(root, 'schema', 'profile.schema.json'), 'utf8'));
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
const clone = (o) => JSON.parse(JSON.stringify(o));

test('every profile in profiles/ passes the schema and the extra checks, file named by id', () => {
  for (const f of fs.readdirSync(path.join(root, 'profiles')).filter((x) => x.endsWith('.json'))) {
    const p = JSON.parse(fs.readFileSync(path.join(root, 'profiles', f), 'utf8'));
    assert.deepEqual(checkProfile(schema, p), [], f);
    assert.equal(f, `${p.id}.json`);
  }
});

test('the schema rules out course data and scripts (反例)', () => {
  const bad = (mutate) => {
    const p = clone(panopto);
    mutate(p);
    return checkProfile(schema, p);
  };
  assert.ok(bad((p) => (p.lecture.course = { selector: '.course' })).some((e) => e.path === '/lecture/course'));
  assert.ok(bad((p) => (p.lecture.folder = { selector: '.folder' })).length > 0);
  assert.ok(bad((p) => (p.courseCode = 'X')).some((e) => e.path === '/courseCode'));
  assert.ok(bad((p) => (p.lecture.title = { selector: 'h1', script: 'alert(1)' })).length > 0);
  assert.ok(bad((p) => (p.tracks[0].detect.urlPattern = '([')).some((e) => /invalid regular expression/.test(e.message)));
  assert.ok(bad((p) => (p.session.idFrom = { pathPattern: '^/v/[^/]+$' })).some((e) => /capture group/.test(e.message)));
  assert.ok(bad((p) => (p.session.idFrom = { query: 'id', selector: 'x' })).length > 0);
  assert.ok(bad((p) => (p.id = 'generic')).length > 0);
  assert.ok(bad((p) => (p.match.hosts = ['*'])).length > 0);
  assert.ok(bad((p) => (p.match.hosts = ['https://x.org'])).length > 0);
  assert.ok(bad((p) => (p.request.cookieUrls = ['all'])).length > 0);
  assert.ok(bad((p) => (p.request.maxConnectionsPerFile = 64)).length > 0);
  assert.ok(bad((p) => (p.tracks[1].role = p.tracks[0].role)).some((e) => /duplicate role/.test(e.message)));
  assert.ok(bad((p) => delete p.tracks[0].label.en).length > 0);
  assert.ok(bad((p) => (p.version = '1.0')).length > 0);
  assert.deepEqual(bad((p) => (p.session.idFrom = { pathPattern: '^/v/([^/]+)$' })), []);
  assert.deepEqual(bad((p) => (p.session.idFrom = { selector: 'meta[name=x]', attr: 'content', pattern: 'id=(\\w+)' })), []);
});

test('Panopto profile: hosts in every region, Viewer and Embed pages only', () => {
  for (const u of [
    'https://example.hosted.panopto.com/Panopto/Pages/Viewer.aspx?id=1',
    'https://example.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=1',
    'https://example.ap.panopto.com/Panopto/Pages/Embed.aspx?id=1&autoplay=false',
    'https://example.ca.panopto.com/panopto/pages/viewer.aspx?id=1',
    'https://example.au.panopto.com/Panopto/Pages/Viewer.aspx?id=1',
  ]) assert.equal(profileMatchesUrl(panopto, u), true, u);
  for (const u of [
    'https://example.cloud.panopto.eu/Panopto/Pages/Sessions/List.aspx',
    'https://example.cloud.panopto.eu/Panopto/Pages/Viewer.aspx.evil',
    'https://panopto.example.edu/Panopto/Pages/Viewer.aspx?id=1',
    'https://evilpanopto.eu/Panopto/Pages/Viewer.aspx?id=1',
  ]) assert.equal(profileMatchesUrl(panopto, u), false, u);
  assert.ok(profilePermissionOrigins(panopto).includes('https://*.panopto.eu/*'));
  assert.ok(profilePermissionOrigins(panopto).every((o) => o !== '<all_urls>' && !o.startsWith('*://*/')));
});

test('profile priority: local imports first, then packaged; frames after the page', () => {
  const local = { ...clone(panopto), version: '1.1.0' };
  const m = findProfile([{ profile: local, reviewed: false }, { profile: panopto, reviewed: true }], S.panoptoPageUrl);
  assert.equal(m.profile.version, '1.1.0');
  assert.equal(m.reviewed, false);
  const lms = 'https://lms.example.edu/course/view.php?id=3';
  const embed = 'https://x.cloud.panopto.eu/Panopto/Pages/Embed.aspx?id=abc';
  const viaFrame = findProfile([{ profile: panopto, reviewed: true }], lms, [embed]);
  assert.equal(viaFrame.url, embed);
  assert.equal(findProfile([{ profile: panopto, reviewed: true }], lms), null);
  // a broken local profile does not stop matching
  const broken = { ...clone(panopto), match: { hosts: ['*.panopto.eu'], pathPattern: '(' } };
  assert.equal(findProfile([{ profile: broken, reviewed: false }, { profile: panopto, reviewed: true }], S.panoptoPageUrl).reviewed, true);
});

test('selector reads are plain data, keyed by field and position', () => {
  assert.deepEqual(selectorReads(panopto).map((r) => r.key), [
    'lecture.title.0', 'lecture.title.1', 'lecture.title.2', 'lecture.title.3',
    'lecture.recordedAt.0', 'lecture.recordedAt.1', 'lecture.recordedAt.2', 'lecture.recordedAt.3',
  ]);
  assert.deepEqual(selectorReads(panopto)[2], { key: 'lecture.title.2', selector: 'meta[property="og:title"]', attr: 'content' });
  assert.deepEqual(selectorReads(null), []);
});

test('session id: query, path pattern, selector, and the generic fallback', async () => {
  const match = (idFrom, url) => ({ profile: { ...clone(panopto), session: { idFrom } }, url });
  const page = 'https://x.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=ABC-123&start=5';
  assert.deepEqual(await sessionIdFor(match({ query: 'id' }, page), page), { sessionId: 'ABC-123', source: 'profile' });
  assert.equal((await sessionIdFor(match({ pathPattern: '/Pages/([A-Za-z]+)\\.aspx' }, page), page)).sessionId, 'Viewer');
  const sel = { 'session.idFrom': { count: 1, value: '  data-id: 77  ' } };
  assert.equal((await sessionIdFor(match({ selector: '#x', pattern: '(\\d+)' }, page), page, sel)).sessionId, '77');
  // nothing found → the generic hash of the page address
  const fallback = await sessionIdFor(match({ query: 'missing' }, page), page);
  assert.deepEqual(fallback, { sessionId: await genericSessionId(page), source: 'generic' });
  assert.equal((await sessionIdFor(null, page)).source, 'generic');
});

test('Panopto lecture fields: title (og:title fallback, whitespace collapsed), recording time from the title, duration from the media', () => {
  const m = { profile: panopto, url: S.panoptoPageUrl };
  const results = readResults(S.panoptoFrames());
  assert.deepEqual(lectureFor(m, results, 3300), {
    title: 'CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026',
    recordedAt: '2026-10-21T09:35', // the page's 09:35 BST, as the page shows it
    durationSec: 3300,
  });
  // h1 missing: og:title, whose double space is collapsed
  const og = { 'lecture.title.2': { count: 1, value: 'CSC1003/L01/02  Fri Oct 23 14:00:00 GMT 2026' }, 'lecture.recordedAt.2': { count: 1, value: 'CSC1003/L01/02  Fri Oct 23 14:00:00 GMT 2026' } };
  const l = lectureFor(m, og, 0);
  assert.equal(l.title, 'CSC1003/L01/02 Fri Oct 23 14:00:00 GMT 2026');
  assert.equal(l.recordedAt, '2026-10-23T14:00');
  assert.equal(l.durationSec, 0);
  // a title the instructor renamed: no date, no guess
  const renamed = { 'lecture.title.0': { count: 1, value: 'Week 3: Hash functions' }, 'lecture.recordedAt.0': { count: 1, value: 'Week 3: Hash functions' } };
  assert.deepEqual(lectureFor(m, renamed, 3300.4), { title: 'Week 3: Hash functions', recordedAt: '', durationSec: 3300 });
  // Embed.aspx: the server-rendered title; <title> while still "Panopto Viewer" is not a title
  const embed = { 'lecture.title.1': { count: 1, value: 'Week 4' }, 'lecture.title.3': { count: 1, value: 'Panopto Viewer' } };
  assert.equal(lectureFor(m, embed, 0).title, 'Week 4');
  assert.equal(lectureFor(m, { 'lecture.title.3': { count: 1, value: 'Panopto Viewer' } }, 0).title, '');
  // no profile: no title (generic detection reads no page text), media duration only
  assert.deepEqual(lectureFor(null, {}, 61.6), { title: '', recordedAt: '', durationSec: 62 });
});

test('download switch: unknown without a rule; disabled only when the rule matches', () => {
  assert.equal(downloadPolicyFor({ profile: panopto }, {}), 'unknown');
  const p = { ...clone(panopto), downloadPolicy: { disabledWhen: [{ selector: '.no-dl' }, { selector: 'meta[name=dl]', attr: 'content', pattern: '^off$' }] } };
  assert.equal(downloadPolicyFor({ profile: p }, {}), 'enabled');
  assert.equal(downloadPolicyFor({ profile: p }, { 'downloadPolicy.disabledWhen.0': { count: 1, value: '' } }), 'disabled');
  assert.equal(downloadPolicyFor({ profile: p }, { 'downloadPolicy.disabledWhen.1': { count: 1, value: 'on' } }), 'enabled');
  assert.equal(downloadPolicyFor({ profile: p }, { 'downloadPolicy.disabledWhen.1': { count: 1, value: 'off' } }), 'disabled');
});

test('naming: Panopto roles, numbered screens, one recommendation per rule', async () => {
  const r = await detectTracks({ frames: S.panoptoFrames(), webRequests: [] }, S.fakeIo(S.panoptoTexts));
  const zh = applyProfile(panopto, r.tracks, 'zh-CN');
  assert.deepEqual(zh.tracks.map((t) => [t.role, t.label, t.recommended]), [
    ['primary-audio', '主讲音频', true],
    ['screen', '屏幕录像 1', false],
    ['screen', '屏幕录像 2', false],
  ]);
  assert.equal(zh.others.length, 0);
  const en = applyProfile(panopto, r.tracks, 'en-GB');
  assert.equal(en.tracks[0].label, 'Lecture audio');
  assert.equal(en.tracks[2].label, 'Screen capture 2');
  // a protected track is never recommended
  const prot = applyProfile(panopto, r.tracks.map((t) => ({ ...t, protected: true })), 'en');
  assert.ok(prot.tracks.every((t) => !t.recommended));
  // tracks no rule wants stay, unnamed
  const extra = [...r.tracks, { id: 'https://cdn.example.org/other.mp4', url: 'https://cdn.example.org/other.mp4', kind: 'video', format: 'file', protected: false }];
  assert.equal(applyProfile(panopto, extra, 'en').others.length, 1);
});

test('a primary stream with video is "Lecture video", not "Lecture audio"', () => {
  const t = { id: 'a', url: 'https://cdn.example.org/Sessions/00000000-0000-4000-8000-000000000001/00000000-0000-4000-8000-000000000002.hls/master.m3u8?Policy=x', kind: 'video', format: 'hls', protected: false };
  const named = applyProfile(panopto, [t], 'en').tracks;
  assert.deepEqual(named.map((x) => x.role), ['primary-video']);
  assert.equal(named[0].recommended, true);
});

test('Panopto track rules: GUID paths only, either case; the screen rule never takes the primary stream', () => {
  const G = '3f2504e0-4f89-41d3-9a0c-0305e82c3301';
  const mk = (p, kind) => ({ id: p, url: `https://cdn.example.org${p}?InvocationID=x`, kind, format: 'hls', protected: false });
  const roles = (list) => applyProfile(panopto, list, 'en').tracks.map((t) => t.role);
  assert.deepEqual(roles([mk(`/sessions/${G}/${G}.hls/master.m3u8`, 'audio')]), ['primary-audio']);
  assert.deepEqual(roles([mk(`/Sessions/${G.toUpperCase()}/${G}-${G}.hls/master.m3u8`, 'audio')]), ['primary-audio']);
  assert.deepEqual(roles([mk(`/sessions/${G}/${G}.object.hls/master.m3u8`, 'video')]), ['screen']);
  assert.deepEqual(roles([mk(`/sessions/${G}/${G}.object.hls/master.m3u8`, 'audio')]), []);
  assert.deepEqual(roles([mk('/Sessions/_hlsbc/abc/master.m3u8', 'video')]), []);
  assert.deepEqual(roles([mk(`/sessions/${G}/x.hls/master.m3u8`, 'audio')]), []);
});

test('labels fall back by language, then to English', () => {
  const label = { 'zh-CN': '主讲音频', en: 'Lecture audio', 'zh-TW': '主講音訊' };
  assert.equal(localizedLabel(label, 'zh_TW'), '主講音訊');
  // Hong Kong and Cantonese: Cantonese, then Traditional Chinese (as LectureFlow), never Simplified
  assert.equal(localizedLabel(label, 'zh-HK'), '主講音訊');
  assert.equal(localizedLabel(label, 'yue-Hant-HK'), '主講音訊');
  assert.equal(localizedLabel({ ...label, 'yue-Hant-HK': '主講聲音' }, 'yue-Hant-HK'), '主講聲音');
  assert.equal(localizedLabel({ ...label, 'yue-Hant-HK': '主講聲音' }, 'zh_HK'), '主講聲音');
  assert.equal(localizedLabel({ 'zh-CN': '主讲音频', en: 'Lecture audio' }, 'yue-Hant-HK'), 'Lecture audio');
  assert.equal(localizedLabel(label, 'zh-SG'), '主讲音频');
  assert.equal(localizedLabel(label, 'ru'), 'Lecture audio');
  assert.equal(localizedLabel({ ...label, ru: 'Аудио лекции' }, 'ru-RU'), 'Аудио лекции');
  assert.equal(cleanText('  a \n\t b  '), 'a b');
});

test('recording times: the wall time the page states, never converted (see also dates.test.mjs)', () => {
  assert.equal(normalizeRecordedAt('2026-10-21'), '2026-10-21');
  assert.equal(normalizeRecordedAt('2026/10/21'), '2026-10-21');
  assert.equal(normalizeRecordedAt('2026-02-30'), '');
  assert.equal(normalizeRecordedAt('2026-10-21 9:05'), '2026-10-21T09:05');
  // RFC 3339: the clock of the stated offset, the offset dropped
  assert.equal(normalizeRecordedAt('2026-10-21T09:35:00Z'), '2026-10-21T09:35');
  assert.equal(normalizeRecordedAt('2026-10-21T09:35:00+01:00'), '2026-10-21T09:35');
  assert.equal(normalizeRecordedAt('2026-10-21t23:35:00.250-05:00'), '2026-10-21T23:35');
  assert.equal(normalizeRecordedAt('2026-10-21T09:35+11:00'), '2026-10-21T09:35');
  assert.equal(normalizeRecordedAt('2026-10-21T24:00:00Z'), '');
  assert.equal(normalizeRecordedAt('2026-10-21T09:35:00+25:00'), '');
  // zone abbreviations and GMT offsets: the clock as written
  assert.equal(normalizeRecordedAt('Wed Oct 21 09:35:00 BST 2026'), '2026-10-21T09:35');
  assert.equal(normalizeRecordedAt('Oct 21 09:35:00 EDT 2026'), '2026-10-21T09:35');
  assert.equal(normalizeRecordedAt('Oct 21 23:35:00 GMT-5 2026'), '2026-10-21T23:35');
  assert.equal(normalizeRecordedAt('Oct 1 00:30:00 AEST 2026'), '2026-10-01T00:30');
  assert.equal(normalizeRecordedAt('Oct 21 09:35:00 CST 2026'), '2026-10-21T09:35');
  assert.equal(normalizeRecordedAt('Oct 21 09:35:00 XYZT 2026'), '2026-10-21T09:35');
  assert.equal(normalizeRecordedAt('Wed Oct 21 2026 09:35:00 GMT+0100 (British Summer Time)'), '2026-10-21T09:35');
  assert.equal(normalizeRecordedAt('Wed, 21 Oct 2026 09:35:00 +0100'), '2026-10-21T09:35');
  assert.equal(normalizeRecordedAt('21 October 2026'), '2026-10-21');
  assert.equal(normalizeRecordedAt('Oct 21, 2026 9:35 PM'), '2026-10-21T21:35');
  assert.equal(normalizeRecordedAt('Oct 21, 2026 12:10 AM'), '2026-10-21T00:10');
  assert.equal(normalizeRecordedAt('Monday 21'), '');
  assert.equal(normalizeRecordedAt(''), '');
  assert.equal(normalizeRecordedAt('Feb 29 10:00:00 UTC 2026'), '');
});

test('durations', () => {
  assert.equal(parseDurationSec('1:23:45'), 5025);
  assert.equal(parseDurationSec('55:00'), 3300);
  assert.equal(parseDurationSec('3300'), 3300);
  assert.equal(parseDurationSec('3300.6'), 3301);
  assert.equal(parseDurationSec('PT55M'), 3300);
  assert.equal(parseDurationSec('55 min'), 3300);
  assert.equal(parseDurationSec('1h 5m'), 3900);
  assert.equal(parseDurationSec('about an hour'), 0);
  assert.equal(parseDurationSec(''), 0);
});

test('generic detection names the lecture with the page title, without the site name (T048)', () => {
  // archive.org: the item page's title ends with the site's name; nothing scraped said more than "Recording"
  assert.equal(pageTitleFor('Chemical History of a Candle : Michael Faraday : Internet Archive', 'https://archive.org/details/x'), 'Chemical History of a Candle : Michael Faraday');
  assert.equal(pageTitleFor('Week 3 – Hashing | Example University', 'https://lectures.example.ac.uk/watch/3'), 'Week 3 – Hashing');
  assert.equal(pageTitleFor('Lecture 7: Hashing - Example', 'https://www.example.com/v/7'), 'Lecture 7: Hashing');
  // a colon inside the title is not a separator; a last part that is not the site stays
  assert.equal(pageTitleFor('Lecture 7: Hashing', 'https://example.com/'), 'Lecture 7: Hashing');
  assert.equal(pageTitleFor('Part 1 - Introduction', 'https://example.com/'), 'Part 1 - Introduction');
  // only the site's name, or a placeholder: no title (LectureFlow names it)
  for (const t of ['', '   ', 'Recording', 'video', 'UNTITLED', 'Audio', 'Media', 'Player', 'Example', 'Recording | Example']) {
    assert.equal(pageTitleFor(t, 'https://example.com/watch'), '', JSON.stringify(t));
  }
  assert.ok(isPlaceholderTitle(' Recording ') && !isPlaceholderTitle('Recording 3'));
  // whitespace collapsed, 300 characters at most; an address host has no site name to drop
  assert.equal(pageTitleFor('  Week\n 4 ', 'http://127.0.0.1:8080/'), 'Week 4');
  assert.equal(pageTitleFor('x'.repeat(400), '').length, 300);
  // lectureFor: generic detection takes it; a profile keeps its own selectors (Panopto's
  // "Panopto Viewer" stays rejected), and without a page nothing changes
  assert.deepEqual(lectureFor(null, {}, 61.6, null, { title: 'Recording', url: 'https://archive.org/details/x' }), { title: '', recordedAt: '', durationSec: 62 });
  assert.equal(lectureFor(null, {}, 0, null, { title: 'Week 4 | Example', url: 'https://example.com/' }).title, 'Week 4');
  const m = { profile: panopto, url: S.panoptoPageUrl };
  assert.equal(lectureFor(m, { 'lecture.title.3': { count: 1, value: 'Panopto Viewer' } }, 0, null, { title: 'Panopto Viewer', url: S.panoptoPageUrl }).title, '');
});

// SEC18b 4: a profile regex runs on at most MAX_REGEX_INPUT (1024)
// characters of the address without its query; a longer query changes
// nothing, a longer host and path is not truncated but does not match.
test('long addresses: a long query still names the track and matches the page; a host and path over 1024 characters do not', () => {
  const GUID = 'a0000000-0000-4000-8000-0000000000a1';
  const track = (url) => ({ id: url, url, kind: 'video', format: 'hls', sizeBytes: -1, sizeEstimated: false, sources: ['request'], frameIds: [0], protected: false });
  const base = `https://cdn.example.org/sessions/${GUID}/${GUID}.hls/master.m3u8`;
  const longQuery = `${base}?Policy=${'p'.repeat(1500)}&Signature=${'s'.repeat(1000)}&Key-Pair-Id=K1`;
  assert.ok(longQuery.length > 2500);
  const named = applyProfile(panopto, [track(longQuery)], 'en');
  assert.deepEqual(named.tracks.map((t) => t.role), ['primary-video']);
  assert.equal(profileMatchesUrl(panopto, `https://x.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=1&x=${'q'.repeat(2000)}`), true);
  // host and path over 1024: generic name, no truncation
  const longPath = `https://cdn.example.org/${'p/'.repeat(520)}sessions/${GUID}/${GUID}.hls/master.m3u8`;
  assert.ok(longPath.length > 1024);
  const unnamed = applyProfile(panopto, [track(longPath)], 'en');
  assert.deepEqual(unnamed.tracks, []);
  assert.equal(unnamed.others.length, 1);
  assert.equal(profileMatchesUrl(panopto, `https://x.cloud.panopto.eu/${'a/'.repeat(520)}Panopto/Pages/Viewer.aspx`), false);
});
