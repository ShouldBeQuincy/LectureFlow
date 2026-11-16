// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// HLS playlists from the page (SEC23c): a 2 MiB playlist must not resolve
// into far more memory than that. Each limit is pinned and checked at
// exactly its value and one more, with inputs of a few MB at most.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseHls, hlsTracks, HLS_LIMITS } from '../../src/lib/hls.js';

const BASE = 'https://media.example.org/v/';
const PLAYLIST = `${BASE}index.m3u8`;
/** A directory URL of exactly n characters, ending in "/". */
const dir = (n) => `${BASE}${'p'.repeat(n - BASE.length - 1)}/`;

test('HLS limits: the values are fixed here, so raising one means changing this test', () => {
  assert.deepEqual({ ...HLS_LIMITS }, {
    urlLength: 4096,
    variants: 256,
    renditions: 256,
    segments: 30000,
    initUris: 256,
    urlBytes: 16 * 1024 * 1024,
    attrLength: 256,
  });
  assert.ok(Object.isFrozen(HLS_LIMITS));
});

const master = (variants, renditions = 0) => '#EXTM3U\n'
  + Array.from({ length: renditions }, (_, i) => `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="n${i}",LANGUAGE="l${i}",URI="a${i}.m3u8"\n`).join('')
  + Array.from({ length: variants }, (_, i) => `#EXT-X-STREAM-INF:BANDWIDTH=${1000 + i},RESOLUTION=640x${100 + i},AUDIO="a"\nv${i}.m3u8\n`).join('');
const media = (n, { maps = 0, name = (i) => `s${i}.ts` } = {}) => '#EXTM3U\n#EXT-X-TARGETDURATION:2\n'
  + Array.from({ length: maps }, (_, i) => `#EXT-X-MAP:URI="init${i}.mp4"\n`).join('')
  + Array.from({ length: n }, (_, i) => `#EXTINF:2,\n${name(i)}\n`).join('')
  + '#EXT-X-ENDLIST\n';

test('HLS limits: variants and renditions, exactly and one more; the tracks stay bounded', () => {
  const L = { variants: 256, renditions: 256 };
  assert.equal(parseHls(master(L.variants), PLAYLIST).variants.length, L.variants);
  const over = parseHls(master(L.variants + 1), PLAYLIST);
  assert.equal(over.variants.length, L.variants);
  assert.equal(over.variants[L.variants - 1].uri, `${BASE}v${L.variants - 1}.m3u8`); // the first ones are kept
  assert.equal(parseHls(master(1, L.renditions), PLAYLIST).renditions.length, L.renditions);
  const many = parseHls(master(1, L.renditions + 1), PLAYLIST);
  assert.equal(many.renditions.length, L.renditions);
  assert.equal(hlsTracks(many, PLAYLIST).length, 1 + L.renditions); // one video, one per language
  assert.equal(HLS_LIMITS.variants, L.variants);
  assert.equal(HLS_LIMITS.renditions, L.renditions);
});

test('HLS limits: segments and #EXT-X-MAP, exactly and one more; the duration still counts every segment', () => {
  const L = { segments: 30000, initUris: 256 };
  assert.equal(parseHls(media(L.segments), PLAYLIST).segments.length, L.segments);
  const over = parseHls(media(L.segments + 1), PLAYLIST);
  assert.equal(over.segments.length, L.segments);
  assert.equal(over.durationSec, 2 * (L.segments + 1));
  assert.equal(over.endList, true);
  assert.equal(parseHls(media(1, { maps: L.initUris }), PLAYLIST).initUris.length, L.initUris);
  assert.equal(parseHls(media(1, { maps: L.initUris + 1 }), PLAYLIST).initUris.length, L.initUris);
  assert.equal(HLS_LIMITS.segments, L.segments);
  assert.equal(HLS_LIMITS.initUris, L.initUris);
});

test('HLS limits: an address, or the playlist’s own address as a base, is at most urlLength', () => {
  const L = { urlLength: 4096 };
  // an address of exactly the limit is kept, one more is left out
  const p = parseHls(media(2, { name: (i) => `${dir(L.urlLength - 4 + i)}s.ts` }), PLAYLIST);
  assert.deepEqual(p.segments, [`${dir(L.urlLength - 4)}s.ts`]);
  assert.equal(p.segments[0].length, L.urlLength);
  // a playlist address past the limit is no base: relative addresses are left out, absolute ones kept
  const own = (n) => `${dir(n - 6)}i.m3u8`;
  assert.equal(own(L.urlLength).length, L.urlLength);
  const text = media(2, { name: (i) => (i === 0 ? 'rel.ts' : `${BASE}abs.ts`) });
  assert.deepEqual(parseHls(text, own(L.urlLength)).segments, [`${dir(L.urlLength - 6)}rel.ts`, `${BASE}abs.ts`]);
  assert.deepEqual(parseHls(text, own(L.urlLength + 1)).segments, [`${BASE}abs.ts`]);
  assert.equal(HLS_LIMITS.urlLength, L.urlLength);
});

test('HLS limits: addresses of one playlist add up to urlBytes at most, exactly and one more', () => {
  const L = { urlBytes: 16 * 1024 * 1024, urlLength: 4096 };
  // every segment address is exactly urlLength characters: dir + "sNNNN.ts"
  const base = dir(L.urlLength - 8);
  const own = `${base}i.m3u8`;
  const fit = L.urlBytes / L.urlLength;
  const name = (i) => `s${String(i).padStart(4, '0')}.ts`;
  const all = parseHls(media(fit, { name }), own);
  assert.equal(all.segments.length, fit);
  assert.ok(all.segments.every((u) => u.length === L.urlLength));
  const over = parseHls(media(fit + 1, { name }), own);
  assert.equal(over.segments.length, fit);
  assert.equal(over.segments[fit - 1], `${base}${name(fit - 1)}`);
  assert.equal(over.durationSec, 2 * (fit + 1));
  assert.equal(HLS_LIMITS.urlBytes, L.urlBytes);
});

test('HLS limits: a real-sized lecture playlist is far below every limit', () => {
  // 4 hours in 2 s segments, one map
  const p = parseHls(media(7200, { maps: 1 }), PLAYLIST);
  assert.equal(p.segments.length, 7200);
  assert.equal(p.initUris.length, 1);
  assert.equal(p.durationSec, 14400);
});
