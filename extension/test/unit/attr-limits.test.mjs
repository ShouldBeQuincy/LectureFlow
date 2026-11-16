// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Attribute values a track keeps (SEC28 中-1): HLS CODECS, NAME, LANGUAGE,
// GROUP-ID, CHANNELS and DASH codecs, Representation id, lang are at most
// attrLength characters; a longer one counts as absent (never cut, so no
// made-up value looks real). Values up to the limit are kept as they are.
// Lengths are compared, never long strings.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseHls, hlsTracks, HLS_LIMITS } from '../../src/lib/hls.js';
import { parseMpd, dashTracks, TEMPLATE_LIMITS } from '../../src/lib/dash.js';

const BASE = 'https://cdn.example.net/hls/master.m3u8';
const MPD = 'https://cdn.example.net/dash/manifest.mpd';
const val = (n, c = 'a') => c.repeat(n);

test('HLS: attribute values up to attrLength are kept, longer ones count as absent', () => {
  const L = HLS_LIMITS.attrLength;
  assert.equal(L, 256);
  const at = `avc1.64001f,${val(L - 12)}`; // exactly L
  const over = `avc1.64001f,${val(L - 11)}`; // L + 1
  const text = [
    '#EXTM3U',
    `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="${val(L, 'g')}",NAME="${val(L, 'n')}",LANGUAGE="${val(L, 'l')}",CHANNELS="${val(L, '2')}",URI="a1.m3u8"`,
    `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="${val(L + 1, 'g')}",NAME="${val(L + 1, 'n')}",LANGUAGE="${val(L + 1, 'l')}",CHANNELS="${val(L + 1, '2')}",URI="a2.m3u8"`,
    `#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,CODECS="${at}",AUDIO="${val(L, 'g')}"`,
    'v1.m3u8',
    `#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=640x360,CODECS="${over}",AUDIO="${val(L + 1, 'g')}"`,
    'v2.m3u8',
  ].join('\n');
  const p = parseHls(text, BASE);
  assert.deepEqual(p.variants.map((v) => [v.codecs.length, v.audio.length]), [[L, L], [0, 0]]);
  assert.deepEqual(p.renditions.map((r) => [r.groupId.length, r.name.length, r.language.length, r.channels.length]), [[L, L, L, L], [0, 0, 0, 0]]);
  // the tracks: nothing over the limit, the URLs all there
  const tracks = hlsTracks(p, BASE);
  for (const t of tracks) {
    assert.ok(t.name.length <= L && t.language.length <= L && t.quality.length <= L + 2);
    for (const v of t.variants) for (const k of ['codecs', 'groupId']) assert.ok(!(k in v) || v[k].length <= L, k);
  }
  assert.deepEqual(tracks.map((t) => t.url), [BASE, 'https://cdn.example.net/hls/a1.m3u8', 'https://cdn.example.net/hls/a2.m3u8']);
});

test('HLS: a master whose only CODECS are far too long still gives its tracks', () => {
  const text = ['#EXTM3U', `#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS="mp4a.40.2,${val(64 * 1024, 'z')}"`, 'a.m3u8'].join('\n');
  const tracks = hlsTracks(parseHls(text, BASE), BASE);
  assert.equal(tracks.length, 1);
  assert.equal(tracks[0].variants[0].codecs, '', 'absent, not cut');
  assert.equal(tracks[0].kind, 'unknown', 'no codecs: not taken for audio by a made-up list');
});

function mpd({ asCodecs = '', reps = 1, repCodecs = null, repId = null, lang = '' }) {
  let s = `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT1H"><Period>`;
  s += `<AdaptationSet contentType="video" mimeType="video/mp4"${asCodecs ? ` codecs="${asCodecs}"` : ''}${lang ? ` lang="${lang}"` : ''}>`;
  for (let r = 0; r < reps; r++) {
    const id = repId === null ? `r${r}` : repId;
    s += `<Representation id="${id}" bandwidth="${1000 + r}" width="1280" height="720"${repCodecs === null ? '' : ` codecs="${repCodecs}"`}/>`;
  }
  return `${s}</AdaptationSet></Period></MPD>`;
}

test('DASH: an AdaptationSet codecs shared by every Representation is kept only within attrLength', () => {
  const L = TEMPLATE_LIMITS.attrLength;
  assert.equal(L, 256);
  const shared = (codecs) => dashTracks(parseMpd(mpd({ asCodecs: codecs, reps: 1000 }), MPD), MPD)[0].variants;
  const kept = shared(`avc1.64001f,${val(L - 12)}`);
  assert.equal(kept.length, 1000);
  assert.ok(kept.every((v) => v.codecs.length === L));
  // 64 KiB shared by 1000 Representations: absent everywhere (SEC28 高-1's input)
  const long = shared(`avc1.64001f,${val(64 * 1024, 'z')}`);
  assert.equal(long.length, 1000);
  assert.ok(long.every((v) => v.codecs === ''));
});

test('DASH: a Representation’s own codecs override the AdaptationSet’s, also when too long; a long id is not kept', () => {
  const L = TEMPLATE_LIMITS.attrLength;
  const v = (o) => dashTracks(parseMpd(mpd({ asCodecs: 'avc1.64001f', ...o }), MPD), MPD)[0].variants[0];
  assert.equal(v({ repCodecs: 'avc1.4d401f' }).codecs, 'avc1.4d401f');
  assert.equal(v({ repCodecs: val(L + 1) }).codecs, '', 'not the AdaptationSet’s');
  assert.equal(v({}).codecs, 'avc1.64001f');
  assert.equal(v({ repCodecs: '' }).codecs, 'avc1.64001f', 'an empty one falls back, as before');
  assert.equal(v({ repId: val(L) }).id.length, L);
  assert.equal(v({ repId: val(L + 1) }).id, '');
});

test('DASH: an AdaptationSet whose lang is too long is not offered (its selector would not match LectureFlow’s)', () => {
  const L = TEMPLATE_LIMITS.attrLength;
  const ok = dashTracks(parseMpd(mpd({ lang: val(L, 'e') }), MPD), MPD);
  assert.equal(ok.length, 1);
  assert.equal(ok[0].language.length, L);
  assert.equal(ok[0].selector, `video:${val(L, 'e')}:0`);
  assert.equal(dashTracks(parseMpd(mpd({ lang: val(L + 1, 'e') }), MPD), MPD).length, 0);
});
