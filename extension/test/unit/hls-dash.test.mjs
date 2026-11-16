// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseAttributeList, parseHls, hlsTracks, estimateSize } from '../../src/lib/hls.js';
import { parseMpd, dashTracks, parseIsoDuration, templateToRegex } from '../../src/lib/dash.js';
import { parseXml, serializeXml, XmlError, textOf } from '../../src/lib/xml.js';

const BASE = 'https://cdn.example.org/v/abc/master.m3u8?sig=XYZ';

test('attribute lists: quoted values keep their commas', () => {
  const m = parseAttributeList('BANDWIDTH=800000,CODECS="avc1.4d401e,mp4a.40.2",RESOLUTION=640x360,NAME="A, B",IV=0x0A');
  assert.equal(m.get('BANDWIDTH').value, '800000');
  assert.equal(m.get('CODECS').value, 'avc1.4d401e,mp4a.40.2');
  assert.equal(m.get('CODECS').quoted, true);
  assert.equal(m.get('NAME').value, 'A, B');
  assert.equal(m.get('IV').value, '0x0A');
});

test('HLS master: variants are one video track, each audio rendition one audio track', () => {
  const text = [
    '#EXTM3U',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="lo",NAME="English",LANGUAGE="en",URI="audio/en-lo.m3u8"',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="hi",NAME="English",LANGUAGE="en",URI="audio/en-hi.m3u8"',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="hi",NAME="Deutsch",LANGUAGE="de",URI="audio/de-hi.m3u8"',
    '#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="s",NAME="English",URI="subs/en.m3u8"',
    '#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360,CODECS="avc1.4d401e",AUDIO="lo"',
    'v360.m3u8?sig=XYZ',
    '#EXT-X-STREAM-INF:BANDWIDTH=3000000,AVERAGE-BANDWIDTH=2500000,RESOLUTION=1920x1080,CODECS="avc1.640028",AUDIO="hi"',
    'https://other.example.org/v1080.m3u8',
    '#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=90000,URI="iframes.m3u8"',
  ].join('\n');
  const p = parseHls(text, BASE);
  assert.equal(p.type, 'master');
  assert.equal(p.variants.length, 2);
  assert.equal(p.variants[0].uri, 'https://cdn.example.org/v/abc/v360.m3u8?sig=XYZ');
  assert.equal(p.variants[1].uri, 'https://other.example.org/v1080.m3u8');
  const tracks = hlsTracks(p, BASE);
  const video = tracks.filter((t) => t.kind === 'video');
  const audio = tracks.filter((t) => t.kind === 'audio');
  assert.equal(video.length, 1);
  assert.equal(video[0].quality, '1080p');
  assert.equal(video[0].bandwidth, 2500000);
  assert.equal(video[0].probeUrl, 'https://other.example.org/v1080.m3u8');
  assert.equal(video[0].url, BASE);
  // English in two groups is one track, taken from the best variant's group
  assert.equal(audio.length, 2);
  const en = audio.find((t) => t.language === 'en');
  assert.equal(en.url, 'https://cdn.example.org/v/abc/audio/en-hi.m3u8');
  assert.deepEqual(en.childUrls.sort(), ['https://cdn.example.org/v/abc/audio/en-hi.m3u8', 'https://cdn.example.org/v/abc/audio/en-lo.m3u8']);
});

test('HLS: renditions without a URI are muxed into the variants, not tracks', () => {
  const p = parseHls('#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="Main",DEFAULT=YES\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1280x720,AUDIO="a"\nv.m3u8\n', BASE);
  const tracks = hlsTracks(p, BASE);
  assert.equal(tracks.length, 1);
  assert.equal(tracks[0].kind, 'video');
});

test('HLS: an audio-only master (Panopto primary stream) is an audio track', () => {
  const p = parseHls('#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=135000,CODECS="mp4a.40.2"\nindex_0.m3u8\n', BASE);
  const tracks = hlsTracks(p, BASE);
  assert.equal(tracks.length, 1);
  assert.equal(tracks[0].kind, 'audio');
  assert.equal(tracks[0].url, BASE);
});

test('HLS: audio-only variants next to video variants give one extra audio track', () => {
  const p = parseHls('#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=64000,CODECS="mp4a.40.5"\na.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2"\nv.m3u8\n', BASE);
  const tracks = hlsTracks(p, BASE);
  assert.deepEqual(tracks.map((t) => t.kind).sort(), ['audio', 'video']);
  assert.equal(tracks.find((t) => t.kind === 'audio').url, 'https://cdn.example.org/v/abc/a.m3u8');
});

test('HLS: variants with neither resolution nor codecs are one track of unknown kind', () => {
  const p = parseHls('#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\na.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2\nb.m3u8\n', BASE);
  const tracks = hlsTracks(p, BASE);
  assert.equal(tracks.length, 1);
  assert.equal(tracks[0].kind, 'unknown');
});

test('HLS: only trick-play streams means no track', () => {
  const p = parseHls('#EXTM3U\n#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=1,URI="i.m3u8"\n', BASE);
  assert.equal(p.type, 'master');
  assert.equal(hlsTracks(p, BASE).length, 0);
});

test('HLS media playlist: duration, segments, BOM and CRLF, comments', () => {
  const text = '\ufeff#EXTM3U\r\n# a comment\r\n#EXT-X-TARGETDURATION:10\r\n#EXTINF:9.5,\r\nseg0.aac\r\n#EXTINF:10.25,title\r\nseg1.aac\r\n#EXT-X-ENDLIST\r\n';
  const p = parseHls(text, BASE);
  assert.equal(p.type, 'media');
  assert.equal(p.durationSec, 19.75);
  assert.equal(p.segments.length, 2);
  assert.equal(p.endList, true);
  const [t] = hlsTracks(p, BASE);
  assert.equal(t.kind, 'audio'); // all segments are .aac
  assert.equal(t.durationSec, 19.75);
});

test('HLS DRM signals: SAMPLE-AES and DRM key formats protect, AES-128 does not', () => {
  const media = (key) => `#EXTM3U\n${key}\n#EXTINF:10,\ns.ts\n#EXT-X-ENDLIST\n`;
  assert.equal(parseHls(media('#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://x",KEYFORMAT="com.apple.streamingkeydelivery"'), BASE).drm, true);
  assert.equal(parseHls(media('#EXT-X-KEY:METHOD=SAMPLE-AES-CTR,URI="data:x",KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"'), BASE).drm, true);
  const aes = parseHls(media('#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x01'), BASE);
  assert.equal(aes.drm, false);
  assert.equal(aes.aesKeys, 1);
  assert.equal(parseHls(media('#EXT-X-KEY:METHOD=NONE'), BASE).drm, false);
  const master = parseHls('#EXTM3U\n#EXT-X-SESSION-KEY:METHOD=SAMPLE-AES,URI="skd://k",KEYFORMAT="com.apple.streamingkeydelivery"\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1x1\nv.m3u8\n', BASE);
  assert.equal(hlsTracks(master, BASE)[0].protected, true);
});

test('HLS: not a playlist', () => {
  assert.equal(parseHls('<html>error</html>', BASE).type, 'invalid');
  assert.equal(parseHls('', BASE).type, 'invalid');
  assert.deepEqual(hlsTracks(parseHls('nope', BASE), BASE), []);
});

test('size estimate from bit rate and duration', () => {
  assert.equal(estimateSize(2000000, 3000), 750000000);
  assert.equal(estimateSize(null, 3000), -1);
  assert.equal(estimateSize(1000, 0), -1);
});

test('ISO 8601 durations', () => {
  assert.equal(parseIsoDuration('PT1H2M3.5S'), 3723.5);
  assert.equal(parseIsoDuration('PT50M'), 3000);
  assert.equal(parseIsoDuration('P1DT1S'), 86401);
  assert.equal(parseIsoDuration('PT'), null);
  assert.equal(parseIsoDuration('P'), null);
  assert.equal(parseIsoDuration('1H'), null);
  assert.equal(parseIsoDuration(undefined), null);
});

test('XML reader: entities, CDATA, comments, declarations; malformed input throws', () => {
  const root = parseXml('<?xml version="1.0"?><!DOCTYPE MPD><!-- c --><a x="1 &amp; 2" y=\'q\'><b>t &lt;x&gt; &#65;&#x42;</b><c/><![CDATA[<raw>]]></a>');
  assert.equal(root.name, 'a');
  assert.deepEqual(root.attrs, [['x', '1 & 2'], ['y', 'q']]);
  assert.equal(textOf(root.children[0]), 't <x> AB');
  assert.equal(root.children[2].text, '<raw>');
  assert.throws(() => parseXml('<a><b></a>'), XmlError);
  assert.throws(() => parseXml('<a>'), XmlError);
  assert.throws(() => parseXml('<a x=1/>'), XmlError);
  assert.throws(() => parseXml('<!DOCTYPE a [<!ENTITY e "x">]><a/>'), XmlError);
  assert.throws(() => parseXml('text<a/>'), XmlError);
  assert.throws(() => parseXml('<a/><b/>'), XmlError);
  assert.equal(serializeXml(parseXml('<a q="&quot;"><b>x</b></a>')), '<a q="&quot;">\n  <b>x</b>\n</a>');
});

const MPD = 'https://cdn.example.org/d/manifest.mpd';

test('DASH: AdaptationSets by contentType, mimeType or codecs; text ignored', () => {
  const xml = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT10M">
    <Period>
      <AdaptationSet contentType="video"><Representation id="v1" bandwidth="3000000" width="1920" height="1080"/><Representation id="v2" bandwidth="800000" height="480"/></AdaptationSet>
      <AdaptationSet mimeType="audio/mp4" lang="en"><Representation id="a1" bandwidth="128000"/></AdaptationSet>
      <AdaptationSet><Representation id="a2" bandwidth="64000" codecs="mp4a.40.5"/></AdaptationSet>
      <AdaptationSet><ContentComponent contentType="video"/><Representation id="v3" bandwidth="1" mimeType="video/mp4"/></AdaptationSet>
      <AdaptationSet contentType="text" mimeType="text/vtt"><Representation id="s" bandwidth="1"/></AdaptationSet>
      <AdaptationSet mimeType="application/mp4" codecs="stpp"><Representation id="s2" bandwidth="1"/></AdaptationSet>
    </Period>
  </MPD>`;
  const p = parseMpd(xml, MPD);
  assert.equal(p.type, 'static');
  assert.equal(p.durationSec, 600);
  const tracks = dashTracks(p, MPD);
  assert.deepEqual(tracks.map((t) => t.kind), ['video', 'audio', 'audio', 'video']);
  assert.equal(tracks[0].quality, '1080p');
  assert.equal(tracks[0].bandwidth, 3000000);
  assert.equal(tracks[0].selector, 'video:-:0');
  assert.equal(tracks[3].selector, 'video:-:1');
  assert.equal(new Set(tracks.map((t) => `${t.url}#${t.selector}`)).size, 4);
  assert.ok(tracks.every((t) => t.format === 'dash' && !t.protected));
});

test('DASH: ContentProtection marks the set protected; Periods merge', () => {
  const xml = `<MPD mediaPresentationDuration="PT1H"><Period duration="PT30M">
      <AdaptationSet contentType="video"><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/><Representation id="v" bandwidth="1" height="720"/></AdaptationSet>
      <AdaptationSet contentType="audio" lang="en"><Representation id="a" bandwidth="1"><ContentProtection schemeIdUri="urn:uuid:9a04f079-9840-4286-ab92-e65be0885f95"/></Representation></AdaptationSet>
    </Period><Period duration="PT30M">
      <AdaptationSet contentType="video"><Representation id="v" bandwidth="1" height="1080"/></AdaptationSet>
      <AdaptationSet contentType="audio" lang="en"><Representation id="a" bandwidth="1"/></AdaptationSet>
    </Period></MPD>`;
  const tracks = dashTracks(parseMpd(xml, MPD), MPD);
  assert.equal(tracks.length, 2);
  assert.ok(tracks.every((t) => t.protected));
  assert.equal(tracks.find((t) => t.kind === 'video').quality, '1080p');
});

test('DASH: segment URLs belong to their set (templates, lists, BaseURL files)', () => {
  const xml = `<MPD><BaseURL>https://media.example.org/x/</BaseURL><Period>
      <AdaptationSet contentType="video"><SegmentTemplate media="$RepresentationID$/seg_$Number%05d$.mp4" initialization="$RepresentationID$/init.mp4"/><Representation id="v720" bandwidth="1" height="720"/></AdaptationSet>
      <AdaptationSet contentType="audio"><Representation id="a" bandwidth="1"><BaseURL>audio_128.mp4</BaseURL><SegmentBase indexRange="0-100"/></Representation></AdaptationSet>
      <AdaptationSet contentType="audio" lang="de"><Representation id="b" bandwidth="1"><SegmentList><Initialization sourceURL="b/init.mp4"/><SegmentURL media="b/1.mp4"/></SegmentList></Representation></AdaptationSet>
    </Period></MPD>`;
  const [video, audio, de] = dashTracks(parseMpd(xml, MPD), MPD);
  assert.ok(video.childPatterns.some((p) => new RegExp(p).test('https://media.example.org/x/v720/seg_00012.mp4')));
  assert.ok(video.childPatterns.some((p) => new RegExp(p).test('https://media.example.org/x/v720/init.mp4')));
  assert.ok(!video.childPatterns.some((p) => new RegExp(p).test('https://media.example.org/x/v1080/seg_00012.mp4')));
  assert.deepEqual(audio.childUrls, ['https://media.example.org/x/audio_128.mp4']);
  assert.deepEqual(de.childUrls.sort(), ['https://media.example.org/x/b/1.mp4', 'https://media.example.org/x/b/init.mp4']);
  assert.equal(templateToRegex('a_$Bandwidth$_$Time$.m4s', 'r', 500), 'a_500_\\d{1,32}\\.m4s');
  assert.equal(templateToRegex('$$x', 'r'), '\\$x');
});

test('DASH: not an MPD', () => {
  assert.equal(parseMpd('<html/>', MPD).type, 'invalid');
  assert.equal(parseMpd('#EXTM3U', MPD).type, 'invalid');
  assert.deepEqual(dashTracks(parseMpd('<MPD', MPD), MPD), []);
});
