// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What the background keeps of a detection and how many tracks a card
// lists (src/lib/retain.js, src/lib/send.js shownTracks; SEC25 低-2):
// within the limits nothing changes; past them the card lists at most
// tracksShown and says how many it left out, the three choices still pick
// what they pick from all tracks, cookies are offered only for shown
// tracks, and manifest texts past the per-tab budget are left out of
// memory (marked `truncated`, as the report marks them) while every track
// stays sendable. Large values are compared by length or identity, never
// expanded by an assertion.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { RETAIN_LIMITS, reportTextLength, keptTextChars, retainDetection, dropTexts, evictTexts, detachedCopy, DEDUP_MAX_LENGTH } from '../../src/lib/retain.js';
import { shownTracks, shownOf, presetSelection, PRESETS, accessOffer, listedTracks, selectionError, buildEnqueue, defaultSelection, sendable } from '../../src/lib/send.js';
import { buildReport } from '../../src/lib/report.js';
import { runSynthetic } from '../support/reports.mjs';

const msg = (k) => k;

const video = (i, extra = {}) => ({ id: `v${i}`, url: `https://cdn${i % 7}.example.net/v${i}.mp4`, kind: 'video', format: 'file', sizeBytes: 1000 - (i % 1000), protected: false, ...extra });
const audio = (i, extra = {}) => ({ id: `a${i}`, url: `https://audio${i % 5}.example.net/a${i}.m4a`, kind: 'audio', format: 'file', sizeBytes: 10, protected: false, ...extra });
const range = (n, f) => Array.from({ length: n }, (_, i) => f(i));

test('the limits are these (contributor docs list them: docs/extension/limits.md)', () => {
  assert.deepEqual({ ...RETAIN_LIMITS }, { tracksShown: 200, tracksKept: 1000, textChars: 512 * 1024, tabTextChars: 8 * 1024 * 1024, totalTextChars: 32 * 1024 * 1024, totalScanChars: 64 * 1024 * 1024, frameResults: 16 });
  assert.ok(RETAIN_LIMITS.tracksShown < RETAIN_LIMITS.tracksKept, 'every shown track is kept');
  assert.ok(Object.isFrozen(RETAIN_LIMITS));
  assert.ok(RETAIN_LIMITS.tabTextChars < RETAIN_LIMITS.totalTextChars, 'one tab never fills the total alone');
  assert.ok(RETAIN_LIMITS.totalScanChars > 3 * (16 + 4) * 1024 * 1024, 'three scans at detect.js LIMITS fit');
});

test('shown tracks: a list within the limit is shown as it is', () => {
  const list = [...range(150, video), ...range(50, audio)];
  const s = shownTracks(list, null);
  assert.ok(s.tracks === list, 'the same list');
  assert.equal(s.hidden, 0);
  assert.equal(shownTracks([], null).hidden, 0);
  assert.equal(shownTracks(undefined, null).tracks.length, 0);
});

test('shown tracks: past the limit, at most tracksShown in list order, the rest counted', () => {
  const list = [...range(1000, video), ...range(300, audio)];
  const s = shownTracks(list, null);
  assert.equal(s.tracks.length, RETAIN_LIMITS.tracksShown);
  assert.equal(s.hidden, list.length - RETAIN_LIMITS.tracksShown);
  const pos = s.tracks.map((t) => list.indexOf(t));
  assert.ok(pos.every((p, i) => i === 0 || p > pos[i - 1]), 'list order kept');
  // generic detection lists video first: the main audio, far past the
  // limit, is shown all the same
  assert.ok(s.tracks.some((t) => t.id === 'a0'), 'the first audio track is shown');
  assert.equal(shownTracks(list, null, 10).tracks.length, 10);
});

test('shown tracks: the three choices, the default ticks and 续传 pick the same from the shown as from all', () => {
  const presets = { full: ['podcast'], audio: ['primary-audio'] };
  const lists = [
    { list: [...range(400, video), ...range(400, audio)], presets: null },
    // a profile's roles placed past the limit
    { list: [...range(500, (i) => video(i, { role: 'screen' })), video(9001, { role: 'podcast' }), audio(9002, { role: 'primary-audio' })], presets },
    // protected tracks first: the sendable ones further down
    { list: [...range(450, (i) => video(i, { protected: true })), video(800), video(801), audio(5)], presets: null },
  ];
  for (const { list, presets: p } of lists) {
    const s = shownTracks(list, p);
    assert.equal(s.tracks.length, RETAIN_LIMITS.tracksShown);
    for (const preset of PRESETS) {
      assert.deepEqual(presetSelection(s.tracks, preset, p), presetSelection(list, preset, p), preset);
      for (const id of presetSelection(list, preset, p) || []) assert.ok(s.tracks.some((t) => t.id === id), `${preset} pick ${id} shown`);
    }
    assert.deepEqual(defaultSelection(s.tracks, { presets: p }), defaultSelection(list, { presets: p }));
    // a page with any sendable track shows one (the popup does not say "protected only")
    assert.equal(s.tracks.some(sendable), list.some(sendable));
  }
  // all protected: nothing to pick, still at most the limit
  const locked = shownTracks(range(400, (i) => video(i, { protected: true })), null);
  assert.equal(locked.tracks.length, RETAIN_LIMITS.tracksShown);
  assert.equal(locked.tracks.some(sendable), false);
});

test('shown tracks: a do-not-handle host is never a choice that forces a track into view', () => {
  const yt = { id: 'yt', url: 'https://rr1---sn-abc.googlevideo.com/videoplayback?x=1', kind: 'audio', format: 'file', sizeBytes: 1, protected: false };
  const list = [...range(300, video), yt, audio(1)];
  const s = shownTracks(list, null);
  assert.ok(!s.tracks.some((t) => t.id === 'yt'), 'not sendable, not picked, past the limit');
  assert.ok(s.tracks.some((t) => t.id === 'a1'), 'the sendable audio is');
});

test('a send takes only shown tracks; the shown ones, as before', () => {
  const tracks = [...range(300, video), audio(1)];
  const result = { status: 'ok', pageUrl: 'https://lectures.example.edu/watch/1', profile: null, request: {}, presets: null, sessionId: 's', lecture: { title: 'T' }, tracks, named: null };
  const shown = shownOf(result);
  const hiddenOne = tracks.find((t) => !shown.includes(t));
  assert.ok(hiddenOne, 'some track is hidden');
  assert.equal(selectionError(shown, [hiddenOne.id], { maxVideo: 2 }), 'unknown_track');
  const ids = presetSelection(shown, 'main', null);
  assert.deepEqual(ids, ['a1', 'v0']);
  assert.equal(selectionError(shown, ids, { maxVideo: 2 }), '');
  const enq = buildEnqueue({ result, ids, courseId: 'c', cookies: [], overrideDisabled: false, msg });
  assert.deepEqual(enq.tracks.map((t) => t.url), [tracks[0].url, tracks[300].url]);
});

test('site access: cookie hosts only of the shown tracks', () => {
  // 300 tracks on 300 hosts: only the shown ones' hosts are offered (hosts of the page's own
  // site: one on another site the page names is not offered at all, AUD3 中-1)
  const tracks = range(300, (i) => ({ ...video(i), url: `https://h${i}.example.edu/v.mp4` }));
  const result = { status: 'ok', pageUrl: 'https://lectures.example.edu/watch/1', request: {}, presets: null, tracks, named: null };
  const { cookieHosts } = accessOffer(result);
  assert.equal(cookieHosts.length, RETAIN_LIMITS.tracksShown);
  assert.ok(!cookieHosts.some((o) => o.includes('h299.')), 'a hidden track’s host is not offered');
  assert.ok(cookieHosts.some((o) => o.includes('h0.')));
  // within the limit, every host as before
  assert.equal(accessOffer({ ...result, tracks: tracks.slice(0, 20) }).cookieHosts.length, 20);
});

// ------------------------------------------------------------ manifest texts

const fetchOf = (purpose, url, text) => ({ purpose, url, ok: true, status: 200, contentType: 'application/vnd.apple.mpegurl', text });
const body = (n) => `#EXTM3U\n${'#'.repeat(Math.max(0, n - 8))}`;

test('retained texts: all of them within the allowance, a copy otherwise equal', async () => {
  const run = await runSynthetic('generic');
  const before = JSON.stringify({ detection: run.detection, named: run.named });
  const kept = retainDetection({ detection: run.detection, named: run.named }, RETAIN_LIMITS.tabTextChars);
  assert.ok(kept.detection !== run.detection && kept.detection.tracks !== run.detection.tracks, 'a copy');
  assert.ok(JSON.stringify({ detection: kept.detection, named: kept.named }) === before, 'nothing changed');
  assert.equal(kept.droppedTexts, 0);
  assert.equal(kept.keptChars, keptTextChars(run.detection));
  assert.ok(kept.keptChars > 0);
  // the report from what is kept is the report from what was found
  const rep = (detection) => JSON.stringify(buildReport({ ...run, detection, extension: { version: '0', browser: 'chrome' }, now: new Date('2026-11-13T00:00:00Z') }).report);
  assert.ok(rep(kept.detection) === rep(run.detection), 'same report');
});

test('retained texts: named tracks stay the same objects as the detection’s', async () => {
  // a profile that names one of three tracks: the other two are the detection's own objects
  const tracks = [video(1), video(2), audio(3)];
  const named = { tracks: [{ ...tracks[0], role: 'screen', label: 'Screen' }], others: [tracks[1], tracks[2]] };
  const kept = retainDetection({ detection: { tracks, evidence: {}, segmentRuns: [], fetches: [], ignored: [] }, named }, 0);
  assert.equal(kept.named.others.length, 2, 'others to check');
  assert.ok(kept.named.others[0] === kept.detection.tracks[1] && kept.named.others[1] === kept.detection.tracks[2], 'shared');
  assert.ok(kept.named.tracks[0] !== kept.detection.tracks[0], 'a named copy is its own object, as before');
  assert.ok(kept.detection.tracks[1] !== tracks[1], 'a copy');
  assert.equal(listedTracks({ named: kept.named }).length, 3);
});

test('the stored copy: strings longer than DEDUP_MAX_LENGTH bypass the table, so many of one length cost no pairwise comparisons', () => {
  assert.equal(DEDUP_MAX_LENGTH, 16383);
  const run = (n) => {
    // n distinct addresses of one length, each held twice (url and id), as file tracks are
    const tracks = Array.from({ length: n }, (_, i) => {
      const url = `https://cdn.example.net/${String(i).padStart(6, '0')}/${'x'.repeat(DEDUP_MAX_LENGTH)}.mp4`;
      return { id: url, url, kind: 'video', format: 'file' };
    });
    let copies = 0;
    const out = detachedCopy({ tracks }, (s) => {
      copies++;
      return structuredClone(s);
    });
    return { out, copies, len: tracks[0].url.length };
  };
  const a = run(40);
  const b = run(80);
  assert.ok(a.len > DEDUP_MAX_LENGTH);
  // the table holds only the short strings ('video', 'file'): the long ones never enter it
  assert.equal(a.out.strings, 2);
  assert.equal(b.out.strings, 2);
  // one copy per place, linear in n
  assert.equal(a.out.long, 80);
  assert.equal(b.out.long, 160);
  assert.equal(a.copies, 82);
  assert.equal(b.copies, 162);
  assert.ok(a.out.value.tracks.every((t) => t.url.length === a.len && t.id === t.url), 'equal values');
  // at the limit a string is still deduplicated
  const edge = 'e'.repeat(DEDUP_MAX_LENGTH);
  const c = detachedCopy({ a: edge, b: edge });
  assert.equal(c.strings, 1);
  assert.equal(c.long, 0);
});

test('retained texts: past the allowance, in call order; a text the report would leave out is never kept', () => {
  const L = { ...RETAIN_LIMITS, textChars: 1000 };
  const fetches = [
    fetchOf('manifest', 'https://e.example/1.m3u8', body(400)),
    fetchOf('manifest', 'https://e.example/2.m3u8', body(1001)), // longer than textChars
    fetchOf('manifest', 'https://e.example/3.m3u8', body(500)),
    fetchOf('probe', 'https://e.example/4.m3u8', body(300)), // over the allowance once 1 and 3 are in
    fetchOf('probe', 'https://e.example/5.m3u8', body(50)),
    { purpose: 'file', url: 'https://e.example/6.mp4', ok: true, status: 200, contentType: 'video/mp4', size: 9 },
    fetchOf('manifest', 'https://e.example/7.m3u8', ''), // nothing to keep
  ];
  const tracks = [{ id: 't', url: 'https://e.example/1.m3u8', kind: 'video', format: 'hls', sizeBytes: -1, protected: false }];
  const kept = retainDetection({ detection: { tracks, evidence: {}, segmentRuns: [], fetches, ignored: [] } }, 1000, L);
  const out = kept.detection.fetches;
  assert.deepEqual(out.map((f) => [(f.text || '').length || f.size || 0, !!f.truncated]), [[400, false], [0, true], [500, false], [0, true], [50, false], [9, false], [0, false]]);
  assert.equal(kept.keptChars, 950);
  assert.equal(kept.droppedTexts, 2);
  assert.ok(kept.keptChars <= 1000);
  assert.deepEqual(kept.detection.tracks, tracks, 'tracks untouched');
  // the report marks them truncated, as it marks a text that is too long
  const { report } = buildReport({ pageUrl: 'https://e.example/watch', frames: [], webRequests: [], detection: kept.detection, match: null, named: null, extension: { version: '0', browser: 'chrome' }, now: new Date() });
  assert.deepEqual(report.inputs.fetches.map((f) => !!f.truncated), [false, true, false, true, false, false, false]);
  // white space before the text does not count against textChars, as in the report
  assert.equal(reportTextLength(`\ufeff  \n${body(1000)}`), 1000);
  const padded = retainDetection({ detection: { tracks: [], fetches: [fetchOf('manifest', 'https://e.example/p.m3u8', `   ${body(1000)}`)] } }, 5000, L);
  assert.equal(padded.keptChars, 1003);
});

test('retained texts: allowance 0 (an in-page frame’s detection) keeps none, and every track is still sendable', async () => {
  const run = await runSynthetic('generic');
  const kept = retainDetection({ detection: run.detection, named: null }, 0);
  assert.equal(kept.keptChars, 0);
  assert.equal(keptTextChars(kept.detection), 0);
  assert.ok(kept.detection.fetches.filter((f) => f.purpose !== 'file' && f.ok).every((f) => f.truncated && f.text === ''));
  const before = { status: 'ok', pageUrl: run.pageUrl, profile: null, request: {}, presets: null, sessionId: 's', lecture: run.lecture, tracks: run.detection.tracks, named: null };
  const after = { ...before, tracks: kept.detection.tracks };
  const ids = shownOf(after).filter(sendable).map((t) => t.id);
  assert.ok(ids.length > 0);
  for (const id of ids) {
    assert.equal(selectionError(shownOf(after), [id]), '');
    const a = buildEnqueue({ result: after, ids: [id], courseId: 'c', cookies: [], overrideDisabled: false, msg });
    const b = buildEnqueue({ result: before, ids: [id], courseId: 'c', cookies: [], overrideDisabled: false, msg });
    assert.ok(JSON.stringify(a) === JSON.stringify(b), `${id}: the same request`);
  }
  // given up later (evicted): the same
  const dropped = dropTexts(run.detection);
  assert.equal(keptTextChars(dropped), 0);
  assert.ok(dropped.tracks === run.detection.tracks, 'tracks untouched');
});

test('all tabs: the tab just stored gives up its other ones first, then the largest (the oldest of equals), never the one just stored (SEC36 低-10)', () => {
  const e = [
    { key: 1, at: 30, chars: 400 },
    { key: '1:5', at: 10, chars: 0 },
    { key: 2, at: 10, chars: 300 },
    { key: 3, at: 20, chars: 300 },
    { key: 4, at: 5, chars: 500 }, // oldest, but just stored
  ];
  assert.deepEqual(evictTexts(e, 4, 1500), []);
  assert.deepEqual(evictTexts(e, 4, 1200), [1], 'the largest, not the oldest');
  assert.deepEqual(evictTexts(e, 4, 1000), [1, 2], 'of equals the older');
  assert.deepEqual(evictTexts(e, 4, 700), [1, 2, 3]);
  assert.deepEqual(evictTexts(e, 4, 0), [1, 2, 3], 'the one just stored stays, even alone over the cap');
  assert.deepEqual(evictTexts(e, 1, 1000), [4]);
  // the tab just stored (a frame of it) gives its own up before anyone else's, however small
  const f = [...e, { key: '4:7', at: 40, chars: 100 }];
  assert.deepEqual(evictTexts(f, '4:9', 1050), [4, '4:7']);
});

// ------------------------------------------------------------ the copy (SEC28 高-1, 中-2)

test('the stored copy: a string shared by many places is copied once', () => {
  // as long as a deduplicated string may be (DEDUP_MAX_LENGTH)
  const codecs = `avc1.64001f,${'z'.repeat(DEDUP_MAX_LENGTH - 12)}`;
  const url = `https://cdn.example.net/${'p'.repeat(8000)}/manifest.mpd`;
  const variants = Array.from({ length: 1000 }, (_, i) => ({ id: `r${i}`, bandwidth: i, codecs }));
  const tracks = Array.from({ length: 50 }, (_, i) => ({ id: url, url, selector: `video:-:${i}`, variants }));
  const detection = { tracks, evidence: { mediaKeysUrls: [], mediaDurations: [] }, segmentRuns: [], fetches: [], ignored: [] };
  let calls = 0;
  const { value, strings, chars } = detachedCopy({ detection }, (s) => {
    calls++;
    return structuredClone(s);
  });
  assert.equal(calls, strings, 'one copy per distinct string');
  // distinct strings: codecs, url, 1000 ids, 50 selectors (keys are not values)
  assert.equal(strings, 2 + 1000 + 50);
  assert.ok(chars < codecs.length + url.length + 20000, `${chars} characters copied, not ${50 * 1000 * codecs.length}`);
  const out = value.detection;
  assert.ok(out.tracks[0].variants === out.tracks[49].variants, 'shared objects stay shared');
  assert.ok(out.tracks.every((t) => t.url === url && t.id === t.url), 'equal values');
  assert.ok(out.tracks[0].variants.every((v) => v.codecs.length === codecs.length && v.codecs === codecs));
  assert.ok(out.tracks !== tracks && out.tracks[0] !== tracks[0], 'a copy');
  // what retainDetection reports, on the same input
  const kept = retainDetection({ detection }, 0);
  assert.equal(kept.copiedStrings, strings);
  assert.equal(kept.copiedChars, chars);
  assert.ok(kept.detection.tracks[0].variants === kept.detection.tracks[1].variants);
});

test('the stored copy: plain data only; anything else is still copied', () => {
  const d = new Date(0);
  const { value } = detachedCopy({ a: [1, 'x', null, undefined, true], d, s: new Set(['q']) });
  assert.deepEqual(value.a, [1, 'x', null, undefined, true]);
  assert.ok(value.d instanceof Date && value.d !== d && value.d.getTime() === 0);
  assert.ok(value.s instanceof Set && value.s.has('q'));
});

test('the stored copy no longer holds the manifest texts, and a shared value is not multiplied (heap, bounded)', { timeout: 60000 }, async () => {
  const v8 = await import('node:v8');
  const vm = await import('node:vm');
  v8.setFlagsFromString('--expose-gc');
  const gc = vm.runInNewContext('gc');
  const used = () => {
    gc();
    gc();
    return process.memoryUsage().heapUsed / 1048576;
  };
  const { detectTracks } = await import('../../src/lib/detect.js');
  // three master playlists of 8 MiB each; their CODECS values end up in the tracks
  async function detect() {
    const texts = new Map();
    for (let i = 0; i < 3; i++) {
      const head = `#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2"\nv${i}/index.m3u8\n`;
      texts.set(`https://e.example/m${i}.m3u8`, head + '#'.repeat(8 * 1048576 - head.length - 8));
    }
    const io = {
      fetchText: async (url) => (texts.has(url) ? { ok: true, status: 200, contentType: '', text: texts.get(url) } : { ok: false, status: 404, contentType: '', text: '' }),
      probeFile: async () => ({ ok: false, status: 404, contentType: '', size: -1 }),
    };
    const elements = [...texts.keys()].map((u) => ({ tag: 'video', src: u, currentSrc: u, sources: [], mediaKeys: false, videoWidth: 0, videoHeight: 0, duration: 60, readyState: 0 }));
    return detectTracks({ frames: [{ frameId: 0, url: 'https://e.example/watch', elements, resources: [], reads: [], encryptedEvents: 0 }], webRequests: [] }, io);
  }
  const base = used();
  const kept = await (async () => retainDetection({ detection: await detect() }, 0).detection)();
  /e/.exec('reset the last match');
  const after = used() - base;
  assert.equal(kept.tracks.length, 3);
  assert.ok(kept.tracks.every((t) => t.variants[0].codecs === 'avc1.64001f,mp4a.40.2'));
  assert.ok(after < 4, `${after.toFixed(1)} MiB kept of 24 MiB of text`);
  // one value of DEDUP_MAX_LENGTH characters in 4096 places: kept once
  // (structuredClone kept 64 MiB)
  const big = 'c'.repeat(DEDUP_MAX_LENGTH);
  const before = used();
  const many = await (async () => retainDetection({ detection: { tracks: Array.from({ length: 4096 }, (_, i) => ({ id: `t${i}`, codecs: big })), fetches: [] } }, 0).detection)();
  const grown = used() - before;
  assert.equal(many.tracks.length, 4096);
  assert.ok(grown < 4, `${grown.toFixed(1)} MiB for one shared value of ${big.length} characters`);
});
