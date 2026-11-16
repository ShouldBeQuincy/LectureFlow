// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Synthetic pages for the tests and the committed fixtures. Every host,
// GUID, signature and title here is made up (example.* or obviously fake
// names); nothing comes from a real site.

export const PANOPTO_HOST = 'lf-fixture.cloud.panopto.eu';
export const CDN = 'https://d0000fixture00.cloudfront.net';
const FOLDER = '1a2b3c4d-0000-4000-8000-0000000000aa';
const SESSION = '5e6f7a8b-0000-4000-8000-0000000000bb';
const PRIMARY = `${SESSION}-9c8d7e6f-0000-4000-8000-0000000000cc`;
const SCREEN1 = 'abcdef01-0000-4000-8000-0000000000d1';
const SCREEN2 = 'abcdef02-0000-4000-8000-0000000000d2';
const SIG = 'Policy=eyJTdGF0ZW1lbnQiOlt7IlJlc291cmNlIjoiaHR0cHM6Ly9leGFtcGxlIn1dfQ__&Signature=Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1ub3A_&Key-Pair-Id=K0000FAKEKEYPAIR';

export const panoptoPageUrl = `https://${PANOPTO_HOST}/Panopto/Pages/Viewer.aspx?id=${SESSION}&start=0`;
export const panoptoUrls = {
  audioMaster: `${CDN}/sessions/${FOLDER}/${PRIMARY}.hls/master.m3u8?${SIG}`,
  audioIndex: `${CDN}/sessions/${FOLDER}/${PRIMARY}.hls/index_0.m3u8?${SIG}`,
  screen1Master: `${CDN}/sessions/${FOLDER}/${SCREEN1}.object.hls/master.m3u8?${SIG}`,
  screen1Index: `${CDN}/sessions/${FOLDER}/${SCREEN1}.object.hls/index_0.m3u8?${SIG}`,
  screen2Master: `${CDN}/sessions/${FOLDER}/${SCREEN2}.object.hls/master.m3u8?${SIG}`,
};

const segs = (n, dur, ext) => Array.from({ length: n }, (_, i) => `#EXTINF:${dur.toFixed(3)},\nseg_${String(i).padStart(5, '0')}.${ext}?${SIG}`).join('\n');

export const panoptoTexts = {
  [panoptoUrls.audioMaster]: `#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=135000,AVERAGE-BANDWIDTH=128000,CODECS="mp4a.40.2"\nindex_0.m3u8?${SIG}\n`,
  [panoptoUrls.audioIndex]: `#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:550\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n${segs(6, 550, 'ts')}\n#EXT-X-ENDLIST\n`,
  [panoptoUrls.screen1Master]: `#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=1920x1080,CODECS="avc1.640028"\nindex_0.m3u8?${SIG}\n`,
  [panoptoUrls.screen1Index]: `#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:550\n#EXT-X-PLAYLIST-TYPE:VOD\n${segs(6, 550, 'ts')}\n#EXT-X-ENDLIST\n`,
  [panoptoUrls.screen2Master]: `#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=700000,RESOLUTION=1280x720,CODECS="avc1.64001f"\nindex_0.m3u8?${SIG}\n`,
};

export const panoptoTitle = 'CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026';

// The full MP4 named in the page's own DeliveryInfo response (Panopto
// "PodcastStreams"); the extension never requests it.
export const panoptoPodcastUrl = `${CDN}/sessions/${FOLDER}/${PRIMARY}.mp4?${SIG}`;

/** What the observer took from DeliveryInfo (downloads off, one podcast stream). */
export function panoptoObserved(key, { downloadUrl = null, podcast = true } = {}) {
  return [
    {
      key,
      index: 0,
      url: `https://${PANOPTO_HOST}/Panopto/Pages/Viewer/DeliveryInfo.aspx`,
      values: { downloadUrl, podcastUrl: podcast ? [panoptoPodcastUrl] : [] },
    },
  ];
}

/**
 * A Panopto viewer page as the scan would report it (MSE: blob: sources),
 * with what the observer took from DeliveryInfo when `observedKey` (the
 * profile's id@version) is given.
 */
export function panoptoFrames(observedKey = null, observedOpts = {}) {
  return [
    {
      frameId: 0,
      url: panoptoPageUrl,
      encryptedEvents: 0,
      ...(observedKey ? { observed: panoptoObserved(observedKey, observedOpts) } : {}),
      elements: [
        { tag: 'video', src: 'blob:https://lf-fixture.cloud.panopto.eu/0000', currentSrc: 'blob:https://lf-fixture.cloud.panopto.eu/0000', sources: [], mediaKeys: false, videoWidth: 1920, videoHeight: 1080, duration: 3300, readyState: 4 },
        { tag: 'video', src: 'blob:https://lf-fixture.cloud.panopto.eu/0001', currentSrc: 'blob:https://lf-fixture.cloud.panopto.eu/0001', sources: [], mediaKeys: false, videoWidth: 0, videoHeight: 0, duration: 3300, readyState: 4 },
      ],
      resources: [
        { url: `https://${PANOPTO_HOST}/Panopto/Scripts/viewer.js?v=12345`, initiatorType: 'script', responseStatus: 200 },
        { url: `https://${PANOPTO_HOST}/Panopto/Services/Data.svc/GetDeliveryInfo`, initiatorType: 'xmlhttprequest', responseStatus: 200 },
        { url: panoptoUrls.audioMaster, initiatorType: 'xmlhttprequest', responseStatus: 200 },
        { url: panoptoUrls.audioIndex, initiatorType: 'xmlhttprequest', responseStatus: 200 },
        { url: panoptoUrls.screen1Master, initiatorType: 'xmlhttprequest', responseStatus: 200 },
        { url: panoptoUrls.screen1Index, initiatorType: 'xmlhttprequest', responseStatus: 200 },
        { url: panoptoUrls.screen2Master, initiatorType: 'xmlhttprequest', responseStatus: 200 },
        { url: `${CDN}/sessions/${FOLDER}/${PRIMARY}.hls/seg_00000.ts?${SIG}`, initiatorType: 'xmlhttprequest', responseStatus: 200 },
        { url: `${CDN}/sessions/${FOLDER}/${SCREEN1}.object.hls/seg_00000.ts?${SIG}`, initiatorType: 'xmlhttprequest', responseStatus: 200 },
      ],
      reads: [
        { key: 'lecture.title.0', selector: 'h1#deliveryTitle', attr: '', count: 1, value: panoptoTitle, nodes: [{ path: [{ tag: 'html', id: '', classes: [] }, { tag: 'body', id: '', classes: ['viewer'] }, { tag: 'h1', id: 'deliveryTitle', classes: [] }], attrNames: ['id'] }] },
        { key: 'lecture.title.1', selector: 'h1#title .title-text', attr: '', count: 0, value: '', nodes: [] },
        { key: 'lecture.title.2', selector: 'meta[property="og:title"]', attr: 'content', count: 1, value: panoptoTitle.replace(' Wed', '  Wed'), nodes: [{ path: [{ tag: 'html', id: '', classes: [] }, { tag: 'head', id: '', classes: [] }, { tag: 'meta', id: '', classes: [] }], attrNames: ['property', 'content'] }] },
        { key: 'lecture.title.3', selector: 'title', attr: '', count: 1, value: panoptoTitle, nodes: [{ path: [{ tag: 'html', id: '', classes: [] }, { tag: 'head', id: '', classes: [] }, { tag: 'title', id: '', classes: [] }], attrNames: [] }] },
        { key: 'lecture.recordedAt.0', selector: 'h1#deliveryTitle', attr: '', count: 1, value: panoptoTitle, nodes: [{ path: [{ tag: 'html', id: '', classes: [] }, { tag: 'body', id: '', classes: ['viewer'] }, { tag: 'h1', id: 'deliveryTitle', classes: [] }], attrNames: ['id'] }] },
        { key: 'lecture.recordedAt.1', selector: 'h1#title .title-text', attr: '', count: 0, value: '', nodes: [] },
        { key: 'lecture.recordedAt.2', selector: 'meta[property="og:title"]', attr: 'content', count: 1, value: panoptoTitle.replace(' Wed', '  Wed'), nodes: [] },
        { key: 'lecture.recordedAt.3', selector: 'title', attr: '', count: 1, value: panoptoTitle, nodes: [] },
      ],
    },
  ];
}

/** io answering from a URL → text table, with probe answers by URL. */
export function fakeIo(texts, files = {}) {
  const calls = [];
  return {
    calls,
    async fetchText(url, ctx) {
      calls.push({ kind: 'text', url, purpose: ctx?.purpose });
      if (Object.hasOwn(texts, url)) return { ok: true, status: 200, contentType: 'application/vnd.apple.mpegurl', text: texts[url] };
      return { ok: false, status: 404, contentType: 'text/html', text: '' };
    },
    async probeFile(url) {
      calls.push({ kind: 'file', url });
      if (Object.hasOwn(files, url)) return { ok: true, status: 200, ...files[url] };
      return { ok: false, status: 0, contentType: '', size: -1, error: 'TypeError' };
    },
  };
}

// ---------------------------------------------------------------- generic

export const GEN_HOST = 'https://lectures.example.edu';

/**
 * The generic page's document.title: the lecture, then the site's name
 * (what src/content/scan.js frameTitle reads after the scan; the scan's
 * frames carry no title).
 */
export const GEN_PAGE_TITLE = 'Lecture 7: Hashing | Example University';

/** A generic page: an MP4 file, an HLS master with an audio group, DASH with DRM. */
export function genericFrames() {
  return [
    {
      frameId: 0,
      url: `${GEN_HOST}/watch/lecture-07`,
      encryptedEvents: 0,
      elements: [
        { tag: 'video', src: `${GEN_HOST}/media/lecture-07.mp4`, currentSrc: `${GEN_HOST}/media/lecture-07.mp4`, sources: [], mediaKeys: false, videoWidth: 1280, videoHeight: 720, duration: 3000.5, readyState: 4 },
        { tag: 'audio', src: '', currentSrc: `${GEN_HOST}/media/lecture-07.m4a`, sources: [{ src: `${GEN_HOST}/media/lecture-07.m4a`, type: 'audio/mp4' }], mediaKeys: false, videoWidth: null, videoHeight: null, duration: 3000.5, readyState: 1 },
      ],
      resources: [
        { url: `${GEN_HOST}/hls/master.m3u8`, initiatorType: 'fetch', responseStatus: 200 },
        { url: `${GEN_HOST}/hls/v720/index.m3u8`, initiatorType: 'fetch', responseStatus: 200 },
        { url: `${GEN_HOST}/hls/v720/seg0.ts`, initiatorType: 'fetch', responseStatus: 200 },
      ],
      reads: [],
    },
    {
      frameId: 3,
      url: `${GEN_HOST}/embed/drm`,
      encryptedEvents: 0,
      elements: [],
      resources: [{ url: `${GEN_HOST}/dash/protected.mpd`, initiatorType: 'xmlhttprequest', responseStatus: 200 }],
      reads: [],
    },
  ];
}

export const genericWebRequests = [
  { url: `${GEN_HOST}/media/lecture-07.mp4`, frameId: 0, resourceType: 'media', statusCode: 206, contentType: 'video/mp4', contentLength: 65536, contentRangeTotal: 734003200 },
  { url: `${GEN_HOST}/files/12345/notes.pdf`, frameId: 0, resourceType: 'xmlhttprequest', statusCode: 200, contentType: 'application/pdf', contentLength: 1000, contentRangeTotal: null },
];

export const genericTexts = {
  [`${GEN_HOST}/hls/master.m3u8`]: [
    '#EXTM3U',
    '#EXT-X-VERSION:6',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="a-en/index.m3u8"',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="Cantonese",LANGUAGE="yue",URI="a-yue/index.m3u8"',
    '#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",AUDIO="aac"',
    'v360/index.m3u8',
    '#EXT-X-STREAM-INF:BANDWIDTH=2500000,AVERAGE-BANDWIDTH=2000000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="aac"',
    'v720/index.m3u8',
    '#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=90000,RESOLUTION=1280x720,URI="v720/iframes.m3u8"',
    '',
  ].join('\n'),
  [`${GEN_HOST}/hls/v720/index.m3u8`]: '#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nseg0.ts\n#EXTINF:10,\nseg1.ts\n#EXT-X-ENDLIST\n',
  [`${GEN_HOST}/hls/a-en/index.m3u8`]: '#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\na0.aac\n#EXT-X-ENDLIST\n',
  [`${GEN_HOST}/hls/a-yue/index.m3u8`]: '#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\na0.aac\n#EXT-X-ENDLIST\n',
  [`${GEN_HOST}/dash/protected.mpd`]: `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013" type="static" mediaPresentationDuration="PT50M" profiles="urn:mpeg:dash:profile:isoff-live:2011">
  <Period id="p0">
    <AdaptationSet contentType="video" mimeType="video/mp4" segmentAlignment="true">
      <ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" cenc:default_KID="0f0e0d0c-0000-4000-8000-000000000001"/>
      <ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"><cenc:pssh>AAAAbHBzc2gAAAAA</cenc:pssh></ContentProtection>
      <SegmentTemplate timescale="1000" media="v/$RepresentationID$/seg-$Number%05d$.m4s" initialization="v/$RepresentationID$/init.mp4" startNumber="1" duration="4000"/>
      <Representation id="v1080" bandwidth="4000000" width="1920" height="1080" codecs="avc1.640028"/>
      <Representation id="v540" bandwidth="1200000" width="960" height="540" codecs="avc1.64001f"/>
    </AdaptationSet>
    <AdaptationSet contentType="audio" mimeType="audio/mp4" lang="en">
      <ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>
      <SegmentTemplate timescale="1000" media="a/$RepresentationID$/seg-$Number%05d$.m4s" initialization="a/$RepresentationID$/init.mp4" startNumber="1" duration="4000"/>
      <Representation id="a128" bandwidth="128000" codecs="mp4a.40.2" audioSamplingRate="48000"/>
    </AdaptationSet>
    <AdaptationSet contentType="text" mimeType="text/vtt" lang="en">
      <Representation id="sub" bandwidth="1000"><BaseURL>subs/en.vtt</BaseURL></Representation>
    </AdaptationSet>
  </Period>
</MPD>
`,
};

export const genericFiles = {
  [`${GEN_HOST}/media/lecture-07.m4a`]: { contentType: 'audio/mp4', size: 48000000 },
};
