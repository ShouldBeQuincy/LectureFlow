// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What one scan may hand detection, and how many single-file tracks it
// makes (src/lib/detect.js LIMITS, boundScan; SEC28 低-1, SEC28b 中-4):
// past each limit the first ones stay and the rest are counted; within
// them nothing changes. Inputs stay small (a few MiB at most).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildCandidates, mergeCandidates, MAX_URL_LENGTH } from '../../src/lib/merge.js';
import { detectTracks, boundScan, boundReads, scanHint, LIMITS } from '../../src/lib/detect.js';
import { classifyUrl, C_HLS, C_DASH, C_FILE } from '../../src/lib/classify.js';

const PAGE = 'https://h.example.org/p';
const frame = (frameId, over = {}) => ({ frameId, url: frameId ? `https://f${frameId}.example.org/` : PAGE, encryptedEvents: 0, elements: [], resources: [], reads: [], ...over });
const el = (src, over = {}) => ({ tag: 'video', src, currentSrc: src, sources: [], mediaKeys: false, videoWidth: null, videoHeight: null, duration: null, readyState: 0, ...over });
const res = (url) => ({ url, initiatorType: 'xmlhttprequest' });
// an address of exactly n characters
const addr = (n, ext = '.mp4', prefix = 'https://cdn.example.org/') => `${prefix}${'a'.repeat(n - prefix.length - ext.length)}${ext}`;
const noIo = { async fetchText() { return { ok: false, status: 404, contentType: '', text: '' }; }, async probeFile() { return { ok: false, status: 404, contentType: '', size: -1 }; } };

test('candidates: an address longer than LIMITS.urlLength (without its fragment) is left out and counted, from every route', () => {
  assert.equal(MAX_URL_LENGTH, LIMITS.urlLength);
  const fits = addr(LIMITS.urlLength);
  const over = addr(LIMITS.urlLength + 1, '.m4a');
  assert.equal(fits.length, LIMITS.urlLength);
  // a long fragment does not count: detection keys the address without it
  const fragment = `${addr(100, '.webm')}#${'t'.repeat(LIMITS.urlLength)}`;
  const { candidates, longUrls } = buildCandidates({
    frames: [frame(0, { elements: [el(fits), el(over), el(fragment)], resources: [res(over), res(fits)] })],
    webRequests: [{ url: over, frameId: 0, resourceType: 'media', statusCode: 200 }],
  });
  assert.deepEqual([...new Set(candidates.map((c) => c.url.length))].sort((a, b) => a - b), [fits.length, fragment.length]);
  assert.equal(longUrls, 3, 'one per route (an element counts its address once)');
  const merged = mergeCandidates(candidates);
  assert.ok(merged.every((m) => m.url.length <= LIMITS.urlLength));
  // the default is the same limit
  assert.equal(buildCandidates({ frames: [frame(0, { resources: [res(over)] })] }).longUrls, 1);
});

test('detection: many long addresses of one length make no tracks and no keyed tables; the count is in limited', async () => {
  // 300 distinct addresses of 16400 characters (V8 hashes those by their length alone)
  const resources = Array.from({ length: 300 }, (_, i) => res(addr(16400, `-${String(i).padStart(4, '0')}.mp4`)));
  const r = await detectTracks({ frames: [frame(0, { resources })], webRequests: [] }, noIo);
  assert.equal(r.tracks.length, 0);
  assert.equal(r.ignored.length, 0);
  assert.deepEqual(r.limited, { longUrls: 300 });
  // within the limits there is no `limited`
  const fine = await detectTracks({ frames: [frame(0, { resources: [res(addr(LIMITS.urlLength))] })], webRequests: [] }, noIo);
  assert.equal(fine.tracks.length, 1);
  assert.ok(!('limited' in fine));
});

test('detection: at most fileTracks single-file tracks, the ones a media element refers to first, then in order; probes as before', async () => {
  // names without numbers, so that they are not a run of segments
  const f = (i) => `https://cdn.example.org/${'abcdefg'[i]}.mp4`;
  // a–e from resource timing only, f and g played by media elements
  const input = { frames: [frame(0, { elements: [el(f(5)), el(f(6))], resources: [0, 1, 2, 3, 4].map((i) => res(f(i))) })], webRequests: [] };
  const calls = [];
  const io = { ...noIo, async probeFile(url) { calls.push(url); return { ok: false, status: 404, contentType: '', size: -1 }; } };
  const r = await detectTracks(input, io, { ...LIMITS, fileTracks: 4 });
  assert.deepEqual(r.tracks.map((t) => t.url).sort(), [f(0), f(1), f(5), f(6)]);
  assert.deepEqual(r.limited, { fileTracks: 3 });
  assert.deepEqual(calls, [f(0), f(1), f(5), f(6)], 'only kept files are probed, in the usual order');
  // at the limit: everything, no `limited`, the same as with no limit at all
  const all = await detectTracks(input, noIo, { ...LIMITS, fileTracks: 7 });
  const unlimited = await detectTracks(input, noIo, { ...LIMITS, fileTracks: Infinity });
  assert.equal(all.tracks.length, 7);
  assert.ok(!('limited' in all));
  assert.deepEqual(all, unlimited);
  assert.equal(LIMITS.fileTracks, 1000);
});

test('boundScan: within the limits the scan comes back as it was (the same objects)', () => {
  const frames = [frame(0, { elements: [el('https://h.example.org/a.mp4', { sources: [{ src: 'https://h.example.org/b.mp4', type: 'video/mp4' }] })], resources: [res('https://h.example.org/m.m3u8')] }), frame(3)];
  const webRequests = [{ url: 'https://h.example.org/a.mp4', frameId: 0 }];
  const b = boundScan({ frames, webRequests });
  assert.ok(b.frames.length === 2 && b.frames[0] === frames[0] && b.frames[1] === frames[1]);
  assert.ok(b.webRequests.length === 1 && b.webRequests[0] === webRequests[0]);
  assert.deepEqual(b.left, { frames: 0, elements: 0, sources: 0, resources: 0, webRequests: 0, longUrls: 0, scanChars: 0, longValues: 0 });
});

test('boundScan: frames, elements, sources and resources past their counts; the top frame always stays and is served first, order kept', () => {
  const L = { frames: 3, elements: 4, sources: 2, resources: 5 };
  const frames = [frame(1), frame(2), frame(4), frame(0), frame(5)].map((f, i) => ({
    ...f,
    elements: [el(`https://h.example.org/${i}a.mp4`, { duration: 60 + i }), el(`https://h.example.org/${i}b.mp4`)],
    resources: [0, 1, 2].map((j) => res(`https://h.example.org/${i}-${j}.ts`)),
  }));
  frames[0].elements[0].sources = [1, 2, 3, 4].map((j) => ({ src: `https://h.example.org/s${j}.mp4`, type: '' }));
  const b = boundScan({ frames, webRequests: [] }, L);
  assert.deepEqual(b.frames.map((f) => f.frameId), [1, 2, 0], 'the first two others and the top frame, in scan order');
  // the frames take turns, the top frame first in each (SEC33d 低-6):
  // elements a, a, a, then the top's b; resources (.ts: no evidence, after
  // every frame's elements) r0, r0, r0, then the top's r1 and the next frame's r1
  assert.deepEqual(b.frames.map((f) => f.elements.length), [1, 1, 2]);
  assert.deepEqual(b.frames.map((f) => f.resources.length), [2, 1, 2]);
  assert.deepEqual(b.frames[0].elements[0].sources.map((s) => s.src), ['https://h.example.org/s1.mp4', 'https://h.example.org/s2.mp4']);
  assert.equal(b.frames[0].elements[0].duration, 60, 'the rest of the element as it was');
  assert.deepEqual(b.left, { frames: 2, elements: 2, sources: 2, resources: 4, webRequests: 0, longUrls: 0, scanChars: 0, longValues: 0 });
  // the input is untouched
  assert.equal(frames[0].elements[0].sources.length, 4);
});

test('boundScan: long addresses and the address budget; an element keeps its evidence without its address', () => {
  const long = addr(LIMITS.urlLength + 1);
  const frames = [
    frame(0, {
      elements: [el(long, { duration: 3600, mediaKeys: true }), el('https://h.example.org/ok.mp4', { sources: [{ src: long, type: '' }] })],
      resources: [res(long), res('https://h.example.org/r.ts')],
    }),
    frame(7, { url: `https://f.example.org/${'x'.repeat(LIMITS.urlLength)}` }),
  ];
  const b = boundScan({ frames, webRequests: [{ url: long, frameId: 0 }, { url: 'https://h.example.org/w.mp4', frameId: 0 }] });
  const [top] = b.frames;
  assert.equal(b.frames.length, 1, 'a frame with a long address is left out');
  assert.deepEqual([top.elements[0].src, top.elements[0].currentSrc, top.elements[0].duration, top.elements[0].mediaKeys], ['', '', 3600, true]);
  assert.deepEqual(top.elements[1].sources, []);
  assert.deepEqual(top.resources.map((r) => r.url), ['https://h.example.org/r.ts']);
  assert.deepEqual(b.webRequests.map((w) => w.url), ['https://h.example.org/w.mp4']);
  assert.equal(b.left.longUrls, 6);
  assert.equal(b.left.frames, 1);
  assert.equal(b.left.webRequests, 1);

  // the budget: addresses are taken in scan order while they fit
  const u = (i) => addr(1000, `-${i}.ts`);
  const many = boundScan({ frames: [frame(0, { resources: [0, 1, 2, 3, 4].map((i) => res(u(i))) })], webRequests: [{ url: u(9), frameId: 0 }] }, { scanChars: 3500 });
  assert.deepEqual(many.frames[0].resources.map((r) => r.url), [u(0), u(1), u(2)]);
  // the requests have an allowance of their own (SEC33 低-2): not crowded out
  assert.deepEqual(many.webRequests.map((w) => w.url), [u(9)]);
  assert.equal(many.left.scanChars, 2);
  // and are held to it
  const req = boundScan({ frames: [frame(0)], webRequests: [0, 1, 2].map((i) => ({ url: u(i), frameId: 0 })) }, { requestChars: 2500 });
  assert.deepEqual(req.webRequests.map((w) => w.url), [u(0), u(1)]);
  assert.deepEqual([req.left.webRequests, req.left.scanChars], [1, 1]);
});

test('boundScan: a frame address counts without its fragment; the frame detection runs in always stays (SEC33 低-1)', () => {
  const long = (n) => `https://f.example.org/${'p'.repeat(n)}`;
  const fragment = `https://f.example.org/player#${'x'.repeat(LIMITS.urlLength + 100)}`;
  const b = boundScan({ frames: [frame(0), frame(5, { url: fragment, elements: [el('https://f.example.org/a.mp4')] })], webRequests: [] });
  assert.deepEqual(b.frames.map((f) => f.frameId), [0, 5], 'a long fragment does not make a frame address too long');
  assert.equal(b.left.longUrls, 0);
  // the frame keeps its address without the fragment, and only that counts against the budget (SEC33 低-4)
  const small = boundScan({ frames: [frame(0), frame(5, { url: fragment })], webRequests: [] }, { scanChars: 1000 });
  assert.deepEqual(small.frames.map((f) => [f.frameId, f.url]), [[0, PAGE], [5, 'https://f.example.org/player']]);
  assert.equal(small.left.scanChars, 0);
  // a longer path is still too long ...
  assert.deepEqual(boundScan({ frames: [frame(0), frame(5, { url: long(LIMITS.urlLength) })], webRequests: [] }).frames.map((f) => f.frameId), [0]);
  // ... except for the frame an in-page button detects in, which stays past every frame limit and is served first
  const inpage = boundScan(
    { frames: [frame(2, { resources: [res('https://f.example.org/x.ts'), res('https://f.example.org/y.ts')] }), frame(4, { url: long(LIMITS.urlLength), resources: [res('https://f.example.org/own.m3u8')] })], webRequests: [] },
    { frames: 1, resources: 1 },
    { frameId: 4 },
  );
  assert.deepEqual(inpage.frames.map((f) => f.frameId), [4]);
  assert.deepEqual(inpage.frames[0].resources.map((r) => r.url), ['https://f.example.org/own.m3u8']);
  // with the top frame too: both stay, the in-page one first in line for the counts
  const both = boundScan({ frames: [frame(0, { resources: [res('https://h.example.org/t.ts')] }), frame(4, { resources: [res('https://f.example.org/own.m3u8')] }), frame(6)], webRequests: [] }, { frames: 2, resources: 1 }, { frameId: 4 });
  assert.deepEqual(both.frames.map((f) => f.frameId), [0, 4]);
  assert.deepEqual(both.frames.map((f) => f.resources.length), [0, 1]);
});

test('boundScan: a <source> type longer than attrLength counts as absent and is counted (SEC33 低-3)', () => {
  const ok = { src: 'https://h.example.org/a.mp4', type: 'video/mp4; codecs="avc1.4d401f, mp4a.40.2"' };
  const atLimit = { src: 'https://h.example.org/b.mp4', type: 'v'.repeat(LIMITS.attrLength) };
  const over = { src: 'https://h.example.org/c.mp4', type: 'v'.repeat(LIMITS.attrLength + 1) };
  const e = el('', { sources: [ok, atLimit, over] });
  const b = boundScan({ frames: [frame(0, { elements: [e] })], webRequests: [] });
  const sources = b.frames[0].elements[0].sources;
  assert.ok(sources[0] === ok && sources[1] === atLimit, 'the others as they were');
  assert.deepEqual(sources[2], { src: over.src, type: '' });
  assert.equal(b.left.longValues, 1);
  assert.equal(over.type.length, LIMITS.attrLength + 1, 'the input is untouched');
  // within the limit: the same objects
  const same = boundScan({ frames: [frame(0, { elements: [el('', { sources: [ok, atLimit] })] })], webRequests: [] });
  assert.equal(same.left.longValues, 0);
  assert.ok(same.frames[0].elements[0].sources[1] === atLimit);
});

test('boundReads: node tags, ids and classes cut to attrLength, attribute names to the first attrNames; within the limits the same objects (SEC33 低-3)', () => {
  const long = (c, n = LIMITS.attrLength + 10) => c.repeat(n);
  const fine = [{ key: 'k', selector: 'h1', attr: '', count: 1, value: 'v', nodes: [{ path: [{ tag: 'h1', id: 'x'.repeat(LIMITS.attrLength), classes: ['a'] }], attrNames: Array.from({ length: LIMITS.attrNames }, (_, i) => `n${i}`) }] }];
  const same = boundReads(fine);
  assert.ok(same.reads === fine);
  assert.equal(same.longValues, 0);
  const node = { path: [{ tag: long('t'), id: 'ok', classes: [long('c'), 'b'] }, { tag: 'div', id: '', classes: [] }], attrNames: [...Array.from({ length: LIMITS.attrNames + 2 }, (_, i) => `n${i}`)] };
  node.attrNames[0] = long('a');
  const reads = [fine[0], { key: 'k2', selector: 'div', attr: '', count: 2, value: 'w', nodes: [node] }];
  const b = boundReads(reads);
  assert.ok(b.reads[0] === fine[0], 'an untouched read stays the same object');
  const [n] = b.reads[1].nodes;
  assert.deepEqual(n.path[0], { tag: long('t', LIMITS.attrLength), id: 'ok', classes: [long('c', LIMITS.attrLength), 'b'] });
  assert.ok(n.path[1] === node.path[1]);
  assert.equal(n.attrNames.length, LIMITS.attrNames);
  assert.equal(n.attrNames[0], long('a', LIMITS.attrLength));
  assert.equal(b.reads[1].value, 'w');
  // tag, one class, the long name, two names past attrNames
  assert.equal(b.longValues, 5);
  assert.equal(node.attrNames.length, LIMITS.attrNames + 2, 'the input is untouched');
  assert.deepEqual(boundReads(undefined), { reads: undefined, longValues: 0 });
});

// ------------------------------------------------------------ what stays first (SEC33 低-2)

const PLAYER = 'https://player.example.org/watch/1';
const manifest = res('https://player.example.org/hls/master.m3u8');
const junk = (n, tag = 'j') => Array.from({ length: n }, (_, i) => res(`https://h.example.org/static/${tag}${'abcdefghij'[i % 10]}${'abcdefghij'[Math.floor(i / 10) % 10]}.png`));
const empty = () => ({ tag: 'video', src: '', currentSrc: '', sources: [], mediaKeys: false, duration: 1, readyState: 0 });

test('boundScan order: frames that look like media are picked before empty ones listed earlier (SEC33 低-2)', () => {
  const frames = [frame(0), frame(1), frame(2), frame(3), frame(4, { url: PLAYER, elements: [el('https://player.example.org/v.mp4', { duration: 3300 })] }), frame(6, { resources: junk(1) })];
  const b = boundScan({ frames, webRequests: [] }, { frames: 3 });
  assert.deepEqual(b.frames.map((f) => f.frameId), [0, 4, 6], 'the top, the player, then a frame with something in it; output in scan order');
  assert.equal(b.left.frames, 3);
  // a frame whose only media is a manifest entry counts as media too
  const m = boundScan({ frames: [frame(0), frame(1, { resources: junk(3) }), frame(2, { resources: [manifest] })], webRequests: [] }, { frames: 2 });
  assert.deepEqual(m.frames.map((f) => f.frameId), [0, 2]);
});

test('boundScan order: manifest entries, then playing elements, then media files, then other addressed elements, then the rest (SEC33 低-2)', () => {
  const playing = el('https://player.example.org/v.mp4', { duration: 3300 });
  const top = frame(0, { elements: [empty(), empty(), el('', { src: 'https://h.example.org/x.mp4', currentSrc: '' }), empty()], resources: [...junk(5), res('https://h.example.org/f.mp4')] });
  const player = frame(5, { url: PLAYER, elements: [playing], resources: [res('https://player.example.org/app.js'), manifest] });
  // element and resource counts: the top page's address-less elements and images go first
  const b = boundScan({ frames: [top, player], webRequests: [] }, { elements: 2, resources: 2 });
  const [t, p] = b.frames;
  assert.deepEqual(p.resources.map((r) => r.url), [manifest.url]);
  assert.ok(p.elements[0] === playing);
  assert.deepEqual(t.elements.map((e) => e.src), ['https://h.example.org/x.mp4'], 'an addressed element before address-less ones');
  assert.deepEqual(t.resources.map((r) => r.url), ['https://h.example.org/f.mp4'], 'a media file before images');
  assert.deepEqual([b.left.elements, b.left.resources], [3, 6]);
  // room for one element: the player's playing one, before the top page's addressed one (which comes first in frame order)
  const one = boundScan({ frames: [top, player], webRequests: [] }, { elements: 1, resources: 2 });
  assert.ok(one.frames[1].elements.length === 1 && one.frames[1].elements[0] === playing);
  assert.deepEqual(one.frames[0].elements, []);
  assert.equal(one.left.elements, 4);
});

test('boundScan order: the top page’s long playing addresses cannot use up the budget before the player’s manifest; the requests keep theirs (SEC33 低-2)', () => {
  const long = (i) => addr(4000, `-${'abcdefghij'[i]}.mp4`);
  const top = frame(0, { elements: [0, 1, 2, 3].map((i) => el(long(i))) });
  const player = frame(5, { url: PLAYER, resources: [manifest] });
  const requests = [{ url: 'https://player.example.org/hls/audio.m3u8', frameId: 5, contentType: 'application/vnd.apple.mpegurl' }];
  // room for the player frame's address, the manifest and one character
  // short of two long addresses: in scan order the top page's first element
  // would take it all and leave the manifest out
  const b = boundScan({ frames: [top, player], webRequests: requests }, { scanChars: PLAYER.length + manifest.url.length + 8000 - 1 });
  assert.deepEqual(b.frames[1].resources.map((r) => r.url), [manifest.url]);
  assert.deepEqual(b.frames[0].elements.map((e) => [e.src === '' ? 0 : 1, e.currentSrc === '' ? 0 : 1]), [[1, 0], [0, 0], [0, 0], [0, 0]], 'after the manifest, one long address fits');
  assert.ok(b.webRequests.length === 1 && b.webRequests[0] === requests[0]);
});

// ------------------------------------------------------------ frame by frame in turn (SEC33d 低-6)

// n entries named like manifests, of exactly len characters each
const junkManifests = (tag, n, len) => Array.from({ length: n }, (_, i) => res(addr(len, `-${'abcdefghij'[i % 10]}${'abcdefghij'[Math.floor(i / 10) % 10]}.m3u8`, `https://w.example.org/${tag}/`)));

test('boundScan turns: embedded frames’ manifest-named entries cannot use the budget up before the top page’s playing lecture (SEC33d 低-6)', () => {
  const lecture = 'https://h.example.org/media/lecture-09.mp4';
  const playing = el(lecture, { duration: 3600 });
  const file = { url: lecture, initiatorType: 'video' };
  const top = frame(0, { elements: [playing], resources: [file] });
  // two readable widget frames whose junk, round ① first, is exactly the budget
  const w1 = frame(1, { url: 'https://h.example.org/w1', resources: junkManifests('a', 20, 1000) });
  const w2 = frame(2, { url: 'https://h.example.org/w2', resources: junkManifests('b', 20, 1000) });
  const budget = w1.url.length + w2.url.length + 40 * 1000;
  const b = boundScan({ frames: [top, w1, w2], webRequests: [] }, { scanChars: budget });
  const [t, f1, f2] = b.frames;
  assert.ok(t.elements[0] === playing, 'the lecture element keeps its addresses');
  assert.ok(t.resources.length === 1 && t.resources[0] === file);
  // the widgets took turns: one entry each per turn, so they share what is left evenly
  assert.equal(f1.resources.length, f2.resources.length + 1);
  assert.equal(f1.resources.length + f2.resources.length, 40 - Math.ceil((2 * lecture.length + lecture.length) / 1000));
  assert.ok(f1.resources.every((r, i) => r === w1.resources[i]), 'each frame’s first entries, in scan order');
  assert.equal(b.left.scanChars, 40 - f1.resources.length - f2.resources.length);
});

test('boundScan turns: the top page’s manifest-named junk cannot use the budget up before an embedded player’s manifest either (SEC33d 信-8)', () => {
  const top = frame(0, { resources: junkManifests('t', 30, 1000) });
  const player = frame(5, { url: PLAYER, elements: [el('https://player.example.org/v.mp4')], resources: [res('https://player.example.org/app.js'), manifest] });
  const b = boundScan({ frames: [top, player], webRequests: [] }, { scanChars: PLAYER.length + 10 * 1000 });
  const p = b.frames[1];
  assert.ok(p.resources.length === 2 && p.resources[1] === manifest, 'the player’s manifest stays (and its other entry, a turn later)');
  assert.ok(p.elements[0] === player.elements[0], 'and its playing element, with its addresses');
  // the top page had one entry a turn: 9 of 1000 characters fit beside the player's 140
  assert.equal(b.frames[0].resources.length, 9);
  assert.equal(b.left.scanChars, 21);
});

test('boundScan turns: counts shared out a frame at a time, the strongest of each frame first; the frames that always stay first in every turn (SEC33d 低-6)', () => {
  const files = (tag, n) => Array.from({ length: n }, (_, i) => res(`https://h.example.org/${tag}/${i}.mp4`));
  const frames = [frame(3, { resources: [...junk(4, 'x'), ...files('c', 4)] }), frame(0, { resources: files('t', 4) }), frame(4, { resources: [manifest, ...files('d', 4)] })];
  const b = boundScan({ frames, webRequests: [] }, { resources: 7 });
  const byId = Object.fromEntries(b.frames.map((f) => [f.frameId, f.resources.map((r) => r.url)]));
  // turn 1: top t0, frame 3 c0, frame 4 the manifest; turn 2: t1, c1, d0; turn 3: t2 (then the room is gone)
  assert.deepEqual(byId[0], files('t', 3).map((r) => r.url));
  assert.deepEqual(byId[3], files('c', 2).map((r) => r.url), 'media files before the frame’s own images');
  assert.deepEqual(byId[4], [manifest.url, files('d', 1)[0].url]);
  assert.equal(b.left.resources, 17 - 7);
  // the frame an in-page button detects in goes first in every turn, then the top frame
  const inpage = boundScan({ frames, webRequests: [] }, { resources: 4 }, { frameId: 4 });
  const ids = Object.fromEntries(inpage.frames.map((f) => [f.frameId, f.resources.length]));
  assert.deepEqual(ids, { 3: 1, 0: 1, 4: 2 });
});

test('boundScan turns: an entry pays by its address characters, so frames each offering one element of many long addresses cannot use the budget up in the first turns (SEC36 低-8)', () => {
  // (at a smaller scale: urlLength 1000, the 16 <source>s of real limits)
  const L = { urlLength: 1000, scanChars: 200000 };
  const a = (tag, i) => addr(900, `-${tag}${i}.mp4`, `https://v.example.org/${tag}/`);
  const m = (tag, i) => res(addr(900, `-${tag}${i}.m3u8`, `https://v.example.org/${tag}/`));
  // a lecture's evidence: 5 manifests, 2 playing elements, 2 media files
  const victim = (id, url) => frame(id, { url, resources: [0, 1, 2, 3, 4].map((i) => m(`m${id}-`, i)).concat([0, 1].map((i) => res(a(`f${id}-`, i)))), elements: [0, 1].map((i) => el(a(`p${id}-`, i))) });
  // junk frames: one element each, src, currentSrc and 16 <source>s of 1000 characters
  const junkEl = (j) => el(addr(1000, `-${j}a.mp4`), { currentSrc: addr(1000, `-${j}b.mp4`), sources: Array.from({ length: 16 }, (_, k) => ({ src: addr(1000, `-${j}s${k}.mp4`), type: '' })) });
  const junkFrames = (n, from) => Array.from({ length: n }, (_, j) => frame(from + j, { url: `https://w.example.org/${j}`, elements: [junkEl(j)] }));
  const kept = (f) => [f.resources.length, f.elements.filter((e) => e.src && e.currentSrc).length];
  // the top page as the victim
  let b = boundScan({ frames: [victim(0, PAGE), ...junkFrames(20, 1)], webRequests: [] }, L);
  assert.deepEqual(kept(b.frames[0]), [7, 2], 'all of the top page’s evidence');
  assert.ok(b.left.scanChars > 0, 'the junk did use the budget up');
  // an embedded player, listed after the junk frames
  b = boundScan({ frames: [frame(0), ...junkFrames(20, 1), victim(50, PLAYER)], webRequests: [] }, L);
  assert.deepEqual(kept(b.frames.at(-1)), [7, 2], 'all of the player’s evidence');
});

test('boundScan turns: a frame’s own document and the same-origin documents it read inside it take turns (SEC36 低-7)', () => {
  // the scan reads same-origin child frames too: their entries come in the
  // parent's lists (doc > 0) besides their own frame's
  const lecture = 'https://h.example.org/media/lecture-09.mp4';
  const playing = { ...el(lecture, { duration: 3600 }), doc: 0 };
  const file = { url: lecture, initiatorType: 'video', doc: 0 };
  const junk = junkManifests('c', 40, 1000).map((r) => ({ ...r, doc: 1 }));
  const top = frame(0, { resources: [...junk, file], elements: [playing] });
  const widget = frame(1, { url: 'https://h.example.org/w', resources: junkManifests('c', 40, 1000).map((r) => ({ ...r, doc: 0 })) });
  const b = boundScan({ frames: [top, widget], webRequests: [] }, { scanChars: widget.url.length + 10 * 1000 });
  const t = b.frames[0];
  assert.ok(t.elements[0] === playing, 'the lecture element keeps its addresses');
  assert.ok(t.resources.includes(file), 'and its file entry');
  assert.ok(t.resources.filter((r) => r.doc === 1).length >= 3, 'the child document had its turns too');
  // entries without a document (or all of doc 0) are one queue, as before
  const same = boundScan({ frames: [frame(0, { resources: junk.map(({ doc, ...r }) => r), elements: [el(lecture)] })], webRequests: [] }, { scanChars: 3000 });
  assert.equal(same.frames[0].elements[0].src, '', 'one document: its manifests first');
});

test('boundScan turns: a child document’s element of many long addresses waits in its own queue, not its frame’s own document’s (SEC36b 低-11)', () => {
  // (at a smaller scale: urlLength 1000) 20 same-origin child frames, each
  // one element of 18 addresses of 1000 characters: separate frames, and
  // child documents in the top frame's lists
  const L = { urlLength: 1000, scanChars: 200000 };
  const junkEl = (j, doc) => ({ ...el(addr(1000, `-${j}a.mp4`), { currentSrc: addr(1000, `-${j}b.mp4`), sources: Array.from({ length: 16 }, (_, k) => ({ src: addr(1000, `-${j}s${k}.mp4`), type: '' })) }), doc });
  const v = (tag, i, ext) => addr(100, `-${tag}${i}${ext}`, 'https://v.example.org/');
  const own = {
    resources: [...[0, 1, 2, 3, 4].map((i) => ({ ...res(v('m', i, '.m3u8')), doc: 0 })), ...[0, 1].map((i) => ({ ...res(v('f', i, '.mp4')), doc: 0 }))],
    elements: [0, 1].map((i) => ({ ...el(v('p', i, '.mp4')), doc: 0 })),
  };
  const top = frame(0, { resources: own.resources, elements: [...own.elements, ...Array.from({ length: 20 }, (_, j) => junkEl(j, j + 1))] });
  const kids = Array.from({ length: 20 }, (_, j) => frame(j + 1, { url: `https://h.example.org/k${j}`, elements: [junkEl(j, 0)] }));
  const b = boundScan({ frames: [top, ...kids], webRequests: [] }, L);
  const t = b.frames[0];
  assert.equal(t.resources.length, 7, 'all of the top document’s entries');
  assert.equal(t.elements.filter((e) => e.doc === 0 && e.src && e.currentSrc).length, 2);
  assert.ok(b.left.scanChars > 0, 'the junk did use the budget up');
});

// (at a smaller scale: urlLength 1000) an element of 18 addresses of 1000 characters: 18 turns
const fatEl = (tag, j, doc = 0) => ({ ...el(addr(1000, `-${tag}${j}a.mp4`), { currentSrc: addr(1000, `-${tag}${j}b.mp4`), sources: Array.from({ length: 16 }, (_, k) => ({ src: addr(1000, `-${tag}${j}s${k}.mp4`), type: '' })) }), doc });
const shortM = (tag, i, len, doc = 0) => ({ ...res(addr(len, `-${tag}${i}.m3u8`, 'https://v.example.org/')), doc });

test('boundScan turns: every long element pays, not only a queue’s first (SEC37 信-22 ①)', () => {
  // the victim: 30 manifests of 100 characters, one a turn; another frame
  // has two long elements: the first is due on turn 18, the second on 36
  const victim = frame(0, { resources: Array.from({ length: 30 }, (_, i) => shortM('v', i, 100)) });
  const other = frame(1, { url: 'https://w.example.org/1', elements: [fatEl('x', 0), fatEl('x', 1)] });
  const b = boundScan({ frames: [victim, other], webRequests: [] }, { urlLength: 1000, scanChars: other.url.length + 18000 + 3000 + 500 });
  assert.equal(b.frames[0].resources.length, 30, 'the second long element came after the victim’s 30 turns');
});

test('boundScan turns: a frame with child documents takes about urlLength a turn at most, both sides waiting for long elements (SEC37 低-13, 信-22 ②)', () => {
  // the victim: 40 manifests of 900 characters, one a turn (first in each
  // turn). The other frame: long elements in its own document and in a
  // child document. Taking turns, each side pays its 18 turns on its own
  // turns only: one long element every 18 turns, two by turn 40 (36000).
  // Both sides paying in the same turns would take four by turn 38.
  const victim = frame(0, { resources: Array.from({ length: 40 }, (_, i) => shortM('v', i, 900)) });
  const other = frame(1, { url: 'https://w.example.org/1', elements: [...[0, 1, 2, 3].map((j) => fatEl('o', j, 0)), ...[0, 1, 2, 3].map((j) => fatEl('k', j, 1))] });
  const b = boundScan({ frames: [victim, other], webRequests: [] }, { urlLength: 1000, scanChars: other.url.length + 36000 + 36000 + 500 });
  assert.equal(b.frames[0].resources.length, 40);
  assert.equal(b.frames[1].elements.filter((e) => e.src && e.currentSrc).length, 2, 'two long elements in the victim’s 40 turns');
});

test('boundScan turns: a side waiting for its long elements gives the other side every other turn, both ways (SEC37 信-22 ③)', () => {
  const L = { urlLength: 1000, scanChars: 9 * 100 + 2 * 18000 + 500 };
  // the frame's own document full of long elements, the lecture's 9 manifests in a child document
  const ownLong = frame(0, { resources: Array.from({ length: 9 }, (_, i) => shortM('k', i, 100, 1)), elements: Array.from({ length: 50 }, (_, j) => fatEl('o', j, 0)) });
  let b = boundScan({ frames: [ownLong], webRequests: [] }, L);
  assert.equal(b.frames[0].resources.length, 9, 'the child document’s entries, on turns 2, 4, … 18');
  // and the other way round
  const kidLong = frame(0, { resources: Array.from({ length: 9 }, (_, i) => shortM('m', i, 100, 0)), elements: Array.from({ length: 50 }, (_, j) => fatEl('k', j, 1)) });
  b = boundScan({ frames: [kidLong], webRequests: [] }, L);
  assert.equal(b.frames[0].resources.length, 9, 'the own document’s entries, on turns 1, 3, … 17');
});

test('boundScan: a urlLength of 0 (tests only) leaves every address out and still returns (SEC37 信-21)', { timeout: 5000 }, () => {
  const b = boundScan({ frames: [frame(0, { elements: [fatEl('z', 0)], resources: [shortM('z', 0, 100)] })], webRequests: [] }, { urlLength: 0 });
  assert.deepEqual([b.frames[0].elements[0].src, b.frames[0].resources.length], ['', 0]);
});

test('boundScan turns: a frame’s own document cannot keep every turn from its child documents either (SEC36b)', () => {
  const lecture = 'https://h.example.org/media/lecture-09.mp4';
  const playing = { ...el(lecture, { duration: 3600 }), doc: 1 };
  const file = { url: lecture, initiatorType: 'video', doc: 1 };
  const top = frame(0, { resources: [...junkManifests('o', 40, 1000).map((r) => ({ ...r, doc: 0 })), file], elements: [playing] });
  const b = boundScan({ frames: [top], webRequests: [] }, { scanChars: 10 * 1000 });
  assert.ok(b.frames[0].elements[0] === playing, 'the child document’s lecture element, on its turn');
  assert.ok(b.frames[0].resources.includes(file));
});

test('boundScan: an unreadable frame’s address (opaque) longer than urlLength loses its fragment, or is left out and counted (SEC33d 信-9)', () => {
  const ok = 'https://lms.example.edu/launch?x=1';
  const fragment = `https://tenant.example.org/Embed.aspx?id=1#${'f'.repeat(LIMITS.urlLength)}`;
  const long = `https://tenant.example.org/${'p'.repeat(LIMITS.urlLength)}`;
  const top = frame(0, { opaque: [ok, `${ok}#t=1`] });
  const same = boundScan({ frames: [top], webRequests: [] });
  assert.ok(same.frames[0] === top, 'within the limit: as it was, fragments too');
  const b = boundScan({ frames: [frame(0, { opaque: [ok, fragment, long] }), frame(3, { opaque: [long] })], webRequests: [] });
  assert.deepEqual(b.frames.map((f) => f.opaque), [[ok, 'https://tenant.example.org/Embed.aspx?id=1'], []]);
  assert.equal(b.left.longUrls, 2);
  assert.equal(b.left.scanChars, 0, 'not charged: the background keeps 20 of them at most');
});

test('scanHint never ranks a resource entry below what detection makes of it (SEC33d: names without an extension, encoded Azure paths, failed responses)', () => {
  // a resource entry carries no Content-Type: detection classifies it by its
  // path and how it was loaded, and so does the hint
  assert.equal(scanHint('https://h.example.org/x.ism/manifest%28format%3Dm3u8-aapl%29'), 'manifest', 'percent-encoded, as classifyUrl decodes it');
  assert.equal(scanHint('https://h.example.org/x.ISM/Manifest(format=mpd-time-csf)/'), 'manifest');
  assert.equal(scanHint('https://h.example.org/hls/master', 'xmlhttprequest'), '', 'no extension: detection cannot tell it either (only a request record can, on its own allowance)');
  assert.equal(scanHint('https://h.example.org/junk.m3u8', 'fetch', 404), '', 'a failed response is never a track');
  assert.equal(scanHint('https://h.example.org/a.MP4?x=1.m3u8#.mpd'), 'file');
  // boundScan reads the entry's status: a failed manifest goes after a media file
  const failed = { url: 'https://h.example.org/junk.m3u8', initiatorType: 'fetch', responseStatus: 404 };
  const fine = { url: 'https://h.example.org/a.mp4', initiatorType: 'fetch', responseStatus: 200 };
  assert.ok(boundScan({ frames: [frame(0, { resources: [failed, fine] })], webRequests: [] }, { resources: 1 }).frames[0].resources[0] === fine);
  let seed = 11;
  const rnd = (n) => {
    seed = (seed * 1103515245 + 12345) % 2147483648;
    return seed % n;
  };
  const pick = (a) => a[rnd(a.length)];
  const names = ['x.m3u8', 'x.M3U', 'x.mpd', 'x.mp4', 'X.WEBM', 'x.m4a', 'x.ts', 'x.m4s', 'x.js', 'x', '.mp4', 'x.', 'x.mp4.png', 'x.png.mp4', 'a.ism/manifest(format=m3u8-aapl)', 'a.isml/Manifest%28format%3Dmpd-time-csf%29', 'a.ism/manifest', 'manifest', 'x.mp4;v=1', 'x.m3u8%3F', 'x.vtt', 'p\\x.m3u8', 'q\\X.Mp3', 'a.ism/%6Danifest(format=m3u8-aapl)', 'x(1).mp4', 'x%20y.mpd'];
  for (let i = 0; i < 3000; i++) {
    const url = `https://h${rnd(3)}.example.org/${pick(['', 'p/', 'p/q/', 'x.mp4/'])}${pick(names)}${pick(['', '?a=1', '?f=x.m3u8', '#t=1', '?a=b#c.mpd'])}`;
    const initiatorType = pick(['', 'fetch', 'xmlhttprequest', 'video', 'audio', 'img', 'script']);
    const status = pick([undefined, 200, 206, 404, 500]);
    const { cls } = classifyUrl(url, { initiatorType, status });
    const hint = scanHint(url, initiatorType, status);
    if (cls === C_HLS || cls === C_DASH) assert.equal(hint, 'manifest', `${url} ${initiatorType} ${status}`);
    if (cls === C_FILE) assert.equal(hint, 'file', `${url} ${initiatorType} ${status}`);
  }
});

test('boundScan with the real limits: a page far past them is held to them, quickly', () => {
  const big = 'https://h.example.org/';
  // 20 frames × 2000 resources, 2000 media elements, addresses of 400 characters: ~17 Mi characters offered
  const url = (i) => `${big}${String(i).padStart(8, '0')}${'q'.repeat(400 - big.length - 8 - 3)}.ts`;
  let n = 0;
  const frames = Array.from({ length: 20 }, (_, fi) => frame(fi, { elements: fi === 0 ? Array.from({ length: 2000 }, () => el(url(n++))) : [], resources: Array.from({ length: 2000 }, () => res(url(n++))) }));
  const t0 = performance.now();
  const b = boundScan({ frames, webRequests: [] });
  const ms = performance.now() - t0;
  let chars = 0;
  let resources = 0;
  let elements = 0;
  for (const f of b.frames) {
    elements += f.elements.length;
    resources += f.resources.length;
    for (const e of f.elements) chars += e.src.length + e.currentSrc.length;
    for (const r of f.resources) chars += r.url.length;
  }
  assert.equal(elements, LIMITS.elements);
  assert.ok(resources <= LIMITS.resources);
  assert.ok(chars <= LIMITS.scanChars, `${chars} characters kept`);
  assert.ok(b.left.elements === 1000 && b.left.resources + b.left.scanChars > 0);
  assert.ok(ms < 2000, `${ms} ms`);
});
