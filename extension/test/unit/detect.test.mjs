// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildCandidates, mergeCandidates, collapseSegmentRuns, sizeFromHeaders, contentRangeTotal, urlTemplate, longestDuration } from '../../src/lib/merge.js';
import { detectTracks, summarize, LIMITS } from '../../src/lib/detect.js';
import * as S from '../support/synth.mjs';

const frame = (over = {}) => ({ frameId: 0, url: 'https://h.example.org/p', encryptedEvents: 0, elements: [], resources: [], reads: [], ...over });
const el = (over = {}) => ({ tag: 'video', src: '', currentSrc: '', sources: [], mediaKeys: false, videoWidth: null, videoHeight: null, duration: null, readyState: 0, ...over });

test('candidates: the three routes merge by URL without fragment, sources in a fixed order', () => {
  const { candidates, evidence } = buildCandidates({
    frames: [
      frame({
        elements: [el({ src: 'https://h.example.org/v.mp4#t=10', currentSrc: 'https://h.example.org/v.mp4#t=10', videoWidth: 1280, videoHeight: 720, readyState: 4, duration: 60 })],
        resources: [{ url: 'https://h.example.org/v.mp4', initiatorType: 'video', responseStatus: 206 }],
      }),
    ],
    webRequests: [{ url: 'https://h.example.org/v.mp4', frameId: 0, resourceType: 'media', statusCode: 206, contentType: 'video/mp4', contentLength: 100, contentRangeTotal: 5000 }],
  });
  assert.equal(candidates.length, 3);
  assert.deepEqual(evidence.mediaDurations, [60]);
  const merged = mergeCandidates(candidates);
  assert.equal(merged.length, 1);
  assert.equal(merged[0].url, 'https://h.example.org/v.mp4');
  assert.deepEqual(merged[0].sources, ['element', 'performance', 'webrequest']);
  assert.equal(merged[0].hints.size, 5000);
  assert.equal(merged[0].hints.contentType, 'video/mp4');
  assert.equal(merged[0].hints.videoHeight, 720);
});

test('candidates: blob: sources are MSE evidence, not URLs; MediaKeys recorded', () => {
  const { candidates, evidence } = buildCandidates({
    frames: [frame({ elements: [el({ src: 'blob:https://h/1', currentSrc: 'blob:https://h/1', mediaKeys: true }), el({ src: 'data:video/mp4;base64,AA' })] })],
  });
  assert.equal(candidates.length, 0);
  assert.equal(evidence.mse, true);
  assert.equal(evidence.mediaKeysOnStream, true);
  const enc = buildCandidates({ frames: [frame({ encryptedEvents: 2 })] }).evidence;
  assert.equal(enc.encrypted, true);
  assert.equal(enc.mediaKeysOnStream, true);
});

test('merge: a success from any route outweighs an error from another', () => {
  const merged = mergeCandidates([
    { url: 'https://h/a.mp4', source: 'performance', frameId: 0, hints: { status: 403 } },
    { url: 'https://h/a.mp4', source: 'webrequest', frameId: 0, hints: { status: 200, contentType: 'video/mp4' } },
  ]);
  assert.equal(merged[0].cls, 'file');
});

test('size from response headers', () => {
  assert.equal(sizeFromHeaders({ statusCode: 200, contentLength: 1234 }), 1234);
  assert.equal(sizeFromHeaders({ statusCode: 206, contentLength: 1, contentRangeTotal: 9999 }), 9999);
  assert.equal(sizeFromHeaders({ statusCode: 206, contentLength: 1, contentRangeTotal: null }), -1);
  assert.equal(sizeFromHeaders({ statusCode: 304, contentLength: 5 }), -1);
  assert.equal(contentRangeTotal('bytes 0-0/734003200'), 734003200);
  assert.equal(contentRangeTotal('bytes 0-0/*'), null);
  assert.equal(contentRangeTotal(''), null);
});

test('runs of numbered files are segments, not tracks; element sources never collapse', () => {
  const urls = [1, 2, 3].map((n) => `https://cdn.example.org/v/seg_${n}.mp4?n=${n}`);
  const merged = mergeCandidates(urls.map((url) => ({ url, source: 'performance', frameId: 0, hints: {} })));
  const runs = collapseSegmentRuns(merged);
  assert.equal(runs.length, 1);
  assert.equal(runs[0].count, 3);
  assert.ok(merged.every((m) => m.cls === 'segment'));
  assert.equal(urlTemplate(urls[0]), 'cdn.example.org/v/seg_#.mp4?n');
  // two are not a run; element sources are kept
  const two = mergeCandidates(urls.slice(0, 2).map((url) => ({ url, source: 'performance', frameId: 0, hints: {} })));
  assert.equal(collapseSegmentRuns(two).length, 0);
  const four = [...urls, 'https://cdn.example.org/v/seg_4.mp4?n=4'];
  const withEl = mergeCandidates(four.map((url, i) => ({ url, source: i === 0 ? 'element' : 'performance', frameId: 0, hints: {} })));
  collapseSegmentRuns(withEl);
  assert.deepEqual(withEl.filter((m) => m.cls === 'file').map((m) => m.url), [four[0]]);
});

test('generic page: files, HLS master with audio group, DRM DASH; variant playlists absorbed', async () => {
  const io = S.fakeIo(S.genericTexts, S.genericFiles);
  const r = await detectTracks({ frames: S.genericFrames(), webRequests: S.genericWebRequests }, io);
  assert.deepEqual(summarize(r.tracks), { total: 7, audio: 4, video: 3, unknown: 0, protected: 2 });
  const ids = r.tracks.map((t) => t.id);
  // the HLS variant playlist the player loaded is part of the master's track
  assert.ok(!ids.includes(`${S.GEN_HOST}/hls/v720/index.m3u8`));
  const hlsVideo = r.tracks.find((t) => t.format === 'hls' && t.kind === 'video');
  assert.equal(hlsVideo.quality, '720p');
  assert.equal(hlsVideo.durationSec, 20); // from the probed media playlist
  assert.equal(hlsVideo.sizeBytes, 5000000); // 2 Mb/s average × 20 s
  assert.equal(hlsVideo.sizeEstimated, true);
  const mp4 = r.tracks.find((t) => t.id === `${S.GEN_HOST}/media/lecture-07.mp4`);
  assert.equal(mp4.sizeBytes, 734003200); // Content-Range total from webRequest
  assert.equal(mp4.quality, '720p');
  assert.deepEqual(mp4.sources, ['element', 'webrequest']);
  const m4a = r.tracks.find((t) => t.id === `${S.GEN_HOST}/media/lecture-07.m4a`);
  assert.equal(m4a.kind, 'audio');
  assert.equal(m4a.sizeBytes, 48000000); // probed
  const dash = r.tracks.filter((t) => t.format === 'dash');
  assert.equal(dash.length, 2);
  assert.ok(dash.every((t) => t.protected && t.durationSec === 3000));
  // the PDF the page loaded is ignored
  assert.ok(r.ignored.some((i) => i.url.endsWith('notes.pdf')));
  // the m4a was probed (no size from a route), the mp4 was not
  const probed = io.calls.filter((c) => c.kind === 'file').map((c) => c.url);
  assert.deepEqual(probed, [`${S.GEN_HOST}/media/lecture-07.m4a`]);
});

test('Panopto page (MSE): two screen masters and the audio master from resource timing', async () => {
  const io = S.fakeIo(S.panoptoTexts);
  const r = await detectTracks({ frames: S.panoptoFrames(), webRequests: [] }, io);
  assert.deepEqual(summarize(r.tracks), { total: 3, audio: 1, video: 2, unknown: 0, protected: 0 });
  assert.equal(r.evidence.mse, true);
  assert.ok(r.tracks.every((t) => t.format === 'hls' && t.durationSec === 3300 && t.sources.join() === 'performance'));
  // the audio master's variant was requested by the player and not counted
  assert.ok(!r.tracks.some((t) => t.id.includes('index_0.m3u8')));
  // the master fetched once each, the screen-2 variant probed once
  assert.equal(io.calls.filter((c) => c.url === S.panoptoUrls.audioMaster).length, 1);
  assert.equal(io.calls.filter((c) => c.purpose === 'probe').length, 1);
});

test('an unreadable manifest still counts as one stream of unknown kind', async () => {
  const r = await detectTracks(
    { frames: [frame({ resources: [{ url: 'https://h.example.org/a/master.m3u8', initiatorType: 'xmlhttprequest' }] })] },
    S.fakeIo({}),
  );
  assert.equal(r.tracks.length, 1);
  assert.equal(r.tracks[0].kind, 'unknown');
  assert.equal(r.tracks[0].unreadable, true);
  assert.equal(r.fetches[0].ok, false);
});

test('a body that disagrees with its name decides the format', async () => {
  const url = 'https://h.example.org/play?id=1';
  const r = await detectTracks(
    { webRequests: [{ url, frameId: 0, resourceType: 'xmlhttprequest', statusCode: 200, contentType: 'application/vnd.apple.mpegurl' }] },
    { async fetchText() { return { ok: true, status: 200, contentType: 'application/dash+xml', text: '<MPD><Period><AdaptationSet contentType="audio"><Representation id="a" bandwidth="1"/></AdaptationSet></Period></MPD>' }; }, async probeFile() { return { ok: false }; } },
  );
  assert.equal(r.tracks.length, 1);
  assert.equal(r.tracks[0].format, 'dash');
});

test('MediaKeys on a blob: element protect every adaptive track, not plain files', async () => {
  const frames = S.genericFrames();
  frames[0].elements.push(el({ src: 'blob:https://x/1', currentSrc: 'blob:https://x/1', mediaKeys: true }));
  const r = await detectTracks({ frames, webRequests: S.genericWebRequests }, S.fakeIo(S.genericTexts, S.genericFiles));
  assert.ok(r.tracks.filter((t) => t.format !== 'file').every((t) => t.protected));
  assert.ok(r.tracks.filter((t) => t.format === 'file').every((t) => !t.protected));
});

test('MediaKeys on an element with a file source protect that file', async () => {
  const frames = [frame({ elements: [el({ src: 'https://h.example.org/x.mp4', currentSrc: 'https://h.example.org/x.mp4', mediaKeys: true })] })];
  const r = await detectTracks({ frames }, S.fakeIo({}, { 'https://h.example.org/x.mp4': { contentType: 'video/mp4', size: 10 } }));
  assert.equal(r.tracks[0].protected, true);
});

test('limits bound the requests detection makes', async () => {
  const resources = Array.from({ length: LIMITS.manifests + 3 }, (_, i) => ({ url: `https://h.example.org/m${i}/master.m3u8`, initiatorType: 'fetch' }));
  const io = S.fakeIo({});
  const r = await detectTracks({ frames: [frame({ resources })] }, io);
  assert.equal(io.calls.length, LIMITS.manifests);
  assert.equal(r.tracks.length, LIMITS.manifests + 3);
});

test('an audio element with metadata and no picture is audio; a video with height gets a quality', async () => {
  const frames = [frame({ elements: [
    el({ tag: 'video', src: 'https://h.example.org/a.mp4', currentSrc: 'https://h.example.org/a.mp4', readyState: 1, videoWidth: 0, videoHeight: 0 }),
    el({ tag: 'video', src: 'https://h.example.org/b.webm', currentSrc: 'https://h.example.org/b.webm', readyState: 4, videoWidth: 854, videoHeight: 480 }),
  ] })];
  const files = { 'https://h.example.org/a.mp4': { contentType: 'video/mp4', size: 1 }, 'https://h.example.org/b.webm': { contentType: 'video/webm', size: 2 } };
  const r = await detectTracks({ frames }, S.fakeIo({}, files));
  assert.equal(r.tracks.find((t) => t.url.endsWith('a.mp4')).kind, 'audio');
  assert.equal(r.tracks.find((t) => t.url.endsWith('b.webm')).quality, '480p');
});

test('an io that throws is a failed fetch, not a failed detection', async () => {
  const r = await detectTracks(
    { frames: [frame({ resources: [{ url: 'https://h.example.org/x.mpd', initiatorType: 'fetch' }], elements: [el({ src: 'https://h.example.org/y.mp4' })] })] },
    { async fetchText() { throw new Error('boom'); }, async probeFile() { throw new Error('boom'); } },
  );
  assert.equal(r.tracks.length, 2);
  assert.ok(r.fetches.every((f) => f.ok === false && f.error === 'boom'));
});

test('the page duration is the longest of any number of media elements: 200,000 do not throw (SEC23b 信息-9)', async () => {
  const N = 200000;
  // the longest first (any cap on elements keeps it), the rest shorter
  const durations = Array.from({ length: N }, (_, i) => (i === 0 ? 5400 : 1 + (i % 3600)));
  assert.equal(longestDuration(durations), 5400);
  assert.equal(longestDuration([...durations].reverse()), 5400);
  assert.equal(longestDuration([]), null);
  assert.equal(longestDuration([], 0), 0);
  assert.equal(longestDuration([60]), 60);

  // through detection: a page with that many media elements, one of them an HLS manifest
  const elements = durations.map((duration) => ({ tag: 'video', duration }));
  elements.push(el({ src: 'https://h.example.org/m.m3u8', currentSrc: 'https://h.example.org/m.m3u8', duration: 60 }));
  const r = await detectTracks(
    { frames: [frame({ elements })], webRequests: [] },
    { async fetchText() { return { ok: false, status: 404, contentType: '', text: '' }; }, async probeFile() { return { ok: false, status: 404, contentType: '', size: -1 }; } },
  );
  assert.equal(r.tracks.length, 1);
  assert.equal(r.tracks[0].durationSec, 5400, 'the unreadable manifest takes the page duration');
});
