// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// SEC25: what a page's manifest costs once a limit is reached. Work is
// counted structurally (URL constructions), never timed; a long signed
// query on a base must neither cost room nor change what relative
// addresses resolve to.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { resolveRef, resolveCost, parseHttpUrl } from '../../src/lib/url.js';
import { parseHls, HLS_LIMITS } from '../../src/lib/hls.js';
import { parseMpd, parseMpdTree, TEMPLATE_LIMITS } from '../../src/lib/dash.js';
import { detectTracks, MAX_MANIFEST_URL_LENGTH, LIMITS } from '../../src/lib/detect.js';
import * as S from '../support/synth.mjs';

const BASE = 'https://media.example.org/v/';
/** A directory URL of exactly n characters, ending in "/". */
const dir = (n) => `${BASE}${'p'.repeat(n - BASE.length - 1)}/`;
const SIG = `Policy=${'P'.repeat(3000)}&Signature=${'s'.repeat(1500)}&Key-Pair-Id=K0000000000000`;

/** Runs fn with every URL construction counted (failed ones too). */
function countUrls(fn) {
  const Original = globalThis.URL;
  let n = 0;
  globalThis.URL = class extends Original {
    constructor(...args) {
      n++;
      super(...args);
    }
  };
  try {
    return { result: fn(), urls: n };
  } finally {
    globalThis.URL = Original;
  }
}

// RFC 3986 §5.4 (normal and abnormal examples) and what the URL parser
// adds: whitespace, backslashes, empty and query-only forms.
const REFS = [
  'g', './g', 'g/', '/g', '//g', '//g/x?y', '?y', 'g?y', '#s', 'g#s', 'g?y#s', ';x', 'g;x', 'g;x?y#s', '', '.', './', '..', '../',
  '../g', '../..', '../../', '../../g', '../../../g', '../../../../g', '/./g', '/../g', 'g.', '.g', 'g..', '..g', './../g', './g/.',
  'g/./h', 'g/../h', 'g;x=1/./y', 'g;x=1/../y', 'g?y/./x', 'g?y/../x', 'g#s/./x', 'g#s/../x', '?', '#', '?#', '?y#s', '  g', '\tg',
  ' ', '\t', '\n', ' #s', '\t#s', '\\g', '\\\\host\\g', 'g\n?y', 'https://other.example.net/a?b#c', 'HTTPS://Other.Example.NET/%7e',
  'http:g', 'mailto:x@example.com', 'data:,x', '%2e%2e/g', 'g%3Fy', 'g?y=%zz', '€/ü?ß#ç',
];
const BASES = [
  'https://a.example.org/b/c/d;p?q',
  `https://a.example.org/b/c/d;p?q&${SIG}`,
  `https://a.example.org/b/c/d;p?q&${SIG}#frag`,
  'https://a.example.org/b/c/d;p#frag',
  'https://a.example.org/b/c/',
  'https://a.example.org',
  `https://user:pw@a.example.org:8443/b/c/manifest.mpd?${SIG}`,
];

test('resolveRef: every relative form against every base is byte for byte new URL(ref, base)', () => {
  let compared = 0;
  for (const base of BASES) {
    for (const ref of REFS) {
      const want = parseHttpUrl(ref, base);
      const got = resolveRef(ref, base);
      assert.equal(got ? got.href : null, want ? want.href : null, `${JSON.stringify(ref)} against ${base.slice(0, 60)}`);
      compared++;
    }
  }
  assert.equal(compared, REFS.length * BASES.length);
});

test('resolveRef: past maxBase only what the base would lend is refused; the long query does not count', () => {
  const longQuery = `https://a.example.org/b/c/d;p?q&${SIG}`;
  assert.ok(longQuery.length > 4096);
  for (const ref of REFS) {
    const want = parseHttpUrl(ref, longQuery);
    const got = resolveRef(ref, longQuery, 4096);
    const trimmed = ref.replace(/^[\u0000- ]+/, '');
    if (trimmed === '' || trimmed.startsWith('#')) {
      // these keep the whole base, which is past the limit: only absolute refs
      assert.equal(got, null, JSON.stringify(ref));
    } else {
      assert.equal(got ? got.href : null, want ? want.href : null, JSON.stringify(ref));
    }
  }
  // a long path is still past the limit
  assert.equal(resolveRef('g', `${dir(4097)}x`, 4096), null);
  assert.equal(resolveRef('https://b.example.org/g', `${dir(4097)}x`, 4096).href, 'https://b.example.org/g');
});

const SIGNED_PLAYLIST = `${BASE}master.m3u8?${SIG}`;
const SIGNED_MPD = `${BASE}manifest.mpd?${SIG}`;

test('HLS: a playlist address with a long signed query keeps its variants, renditions and segments (SEC25 中-4)', () => {
  assert.ok(SIGNED_PLAYLIST.length > HLS_LIMITS.urlLength);
  const master = '#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="en",LANGUAGE="en",URI="audio/en.m3u8?t=1"\n'
    + '#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=1280x720,AUDIO="a"\n720/index.m3u8\n'
    + '#EXT-X-STREAM-INF:BANDWIDTH=500,RESOLUTION=640x360,AUDIO="a"\n../360/index.m3u8\n';
  const p = parseHls(master, SIGNED_PLAYLIST);
  const want = (rel) => new URL(rel, SIGNED_PLAYLIST).href;
  assert.deepEqual(p.variants.map((v) => v.uri), [want('720/index.m3u8'), want('../360/index.m3u8')]);
  assert.deepEqual(p.renditions.map((r) => r.uri), [want('audio/en.m3u8?t=1')]);
  const media = parseHls('#EXTM3U\n#EXT-X-MAP:URI="init.mp4"\n#EXTINF:2,\ns0.m4s\n#EXTINF:2,\n/abs/s1.m4s\n#EXT-X-ENDLIST\n', SIGNED_PLAYLIST);
  assert.deepEqual(media.segments, [want('s0.m4s'), want('/abs/s1.m4s')]);
  assert.deepEqual(media.initUris, [want('init.mp4')]);
});

test('DASH: an MPD address with a long signed query keeps relative lists, files and templates (SEC25 中-4)', () => {
  assert.ok(SIGNED_MPD.length > TEMPLATE_LIMITS.urlLength);
  const mpd = `<MPD><Period>
      <AdaptationSet contentType="video"><SegmentTemplate media="v/$Number$.m4s" initialization="v/init.mp4"/><Representation id="v" bandwidth="1"/></AdaptationSet>
      <AdaptationSet contentType="audio"><SegmentList><Initialization sourceURL="a/init.mp4"/><SegmentURL media="a/1.mp4"/></SegmentList><Representation id="a" bandwidth="1"/></AdaptationSet>
      <AdaptationSet contentType="audio" lang="de"><Representation id="d" bandwidth="1"><BaseURL>de.mp4</BaseURL></Representation></AdaptationSet>
    </Period></MPD>`;
  const [video, audio, de] = parseMpd(mpd, SIGNED_MPD).adaptationSets;
  const want = (rel) => new URL(rel, SIGNED_MPD).href;
  assert.ok(video.childPatterns.some((p) => new RegExp(p).test(want('v/12.m4s'))));
  assert.ok(video.childPatterns.some((p) => new RegExp(p).test(want('v/init.mp4'))));
  assert.deepEqual(audio.childUrls, [want('a/1.mp4'), want('a/init.mp4')]);
  assert.deepEqual(de.childUrls, [want('de.mp4')]);
  // a <BaseURL> whose query makes it longer than urlLength is still a base: its path is short
  const parent = `${dir(2000)}m.mpd`;
  const sub = `sub/?${'x'.repeat(3000)}`;
  assert.ok(new URL(sub, parent).href.length > TEMPLATE_LIMITS.urlLength);
  const q = parseMpd(`<MPD><BaseURL>${sub}</BaseURL><Period><AdaptationSet contentType="video"><Representation id="v" bandwidth="1"><BaseURL>v.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`, parent);
  assert.deepEqual(q.adaptationSets[0].childUrls, [new URL('v.mp4', new URL(sub, parent)).href]);
});

test('HLS: once the byte budget is spent, later addresses are not resolved (SEC25 中-2)', () => {
  const L = { urlBytes: 16 * 1024 * 1024, urlLength: 4096 };
  const base = dir(L.urlLength - 8);
  const own = `${base}i.m3u8`;
  const fit = L.urlBytes / L.urlLength;
  const extra = 500;
  const text = '#EXTM3U\n' + Array.from({ length: fit + 1 + extra }, (_, i) => `#EXTINF:2,\ns${String(i).padStart(4, '0')}.ts\n`).join('');
  const { result, urls } = countUrls(() => parseHls(text, own));
  assert.equal(result.segments.length, fit);
  assert.equal(result.durationSec, 2 * (fit + 1 + extra));
  // two constructions per resolved address (resolve, then the key), the base once
  assert.ok(urls <= 2 * (fit + 1) + 4, `${urls} URL constructions for ${fit + 1 + extra} lines`);
  // past the segment count nothing is resolved either
  const many = '#EXTM3U\n' + Array.from({ length: HLS_LIMITS.segments + extra }, (_, i) => `#EXTINF:2,\ns${i}.ts\n`).join('');
  const counted = countUrls(() => parseHls(many, `${BASE}i.m3u8`));
  assert.equal(counted.result.segments.length, HLS_LIMITS.segments);
  assert.ok(counted.urls <= 2 * HLS_LIMITS.segments + 4, `${counted.urls} URL constructions`);
});

test('DASH: a SegmentList refused for bytes spends the budget; later Representations read nothing (SEC25 中-1)', () => {
  const urlLength = 4096;
  const fit = TEMPLATE_LIMITS.urlBytes / urlLength;
  const el = (name, attrs = [], children = []) => ({ name, attrs, children });
  const seg = el('SegmentURL', [['media', 's.mp4']]);
  // a shared list one address too long for the budget; ten Representations, each its own base
  const reps = Array.from({ length: 10 }, (_, i) => el('Representation', [['id', `r${i}`], ['bandwidth', '1']], [el('BaseURL', [], [{ text: `r${i}/` }])]));
  const tree = el('MPD', [], [el('BaseURL', [], [{ text: dir(urlLength - 8) }]), el('Period', [], [el('AdaptationSet', [['contentType', 'video']], [
    el('SegmentList', [], new Array(fit + 1).fill(seg)),
    ...reps,
  ])])]);
  const { result, urls } = countUrls(() => parseMpdTree(tree, `${BASE}m.mpd`));
  assert.equal(result.adaptationSets[0].childUrls.length, 0);
  // one attempt (two constructions per address) and a few per base, not ten attempts
  assert.ok(urls <= 2 * (fit + 1) + 10 * 6 + 10, `${urls} URL constructions`);
});

test('detection: a manifest address past MAX_MANIFEST_URL_LENGTH is not read (SEC25 中-3)', async () => {
  assert.equal(MAX_MANIFEST_URL_LENGTH, 8192);
  const at = (n) => `${BASE}${'p'.repeat(n - BASE.length - 4)}.mpd`;
  const frame = (url) => [{ frameId: 0, url: 'https://h.example.org/p', encryptedEvents: 0, reads: [], elements: [], resources: [{ url, initiatorType: 'fetch' }] }];
  const ok = S.fakeIo({});
  const r1 = await detectTracks({ frames: frame(at(MAX_MANIFEST_URL_LENGTH)) }, ok);
  assert.equal(at(MAX_MANIFEST_URL_LENGTH).length, MAX_MANIFEST_URL_LENGTH);
  assert.equal(ok.calls.length, 1);
  assert.equal(r1.tracks.length, 1);
  const over = S.fakeIo({});
  const r2 = await detectTracks({ frames: frame(at(MAX_MANIFEST_URL_LENGTH + 1)) }, over);
  assert.equal(over.calls.length, 0);
  assert.equal(r2.tracks.length, 0);
});

test('budgets: addresses refused for their length are charged too, so their work is bounded as well', () => {
  const urlLength = 4096;
  // HLS: every address comes out one character too long (base dir of 4089 + "sNNNN.ts")
  const base = dir(urlLength - 7);
  const own = `${base}i.m3u8`;
  const lines = 6000;
  const text = '#EXTM3U\n' + Array.from({ length: lines }, (_, i) => `#EXTINF:2,\ns${String(i).padStart(4, '0')}.ts\n`).join('');
  const hls = countUrls(() => parseHls(text, own));
  assert.equal(hls.result.segments.length, 0);
  const attempts = Math.floor(HLS_LIMITS.urlBytes / (urlLength + 1)) + 1;
  assert.ok(hls.urls <= 2 * attempts + 4, `${hls.urls} URL constructions for ${lines} refused lines`);
  // DASH: a shared list whose addresses all come out too long, ten Representations with their own base
  const el = (name, attrs = [], children = []) => ({ name, attrs, children });
  const seg = el('SegmentURL', [['media', 'ss.mp4']]);
  const reps = Array.from({ length: 10 }, (_, i) => el('Representation', [['id', `r${i}`], ['bandwidth', '1']], [el('BaseURL', [], [{ text: `r${i}/` }])]));
  const tree = el('MPD', [], [el('BaseURL', [], [{ text: dir(urlLength - 8) }]), el('Period', [], [el('AdaptationSet', [['contentType', 'video']], [
    el('SegmentList', [], new Array(3000).fill(seg)),
    ...reps,
  ])])]);
  const dash = countUrls(() => parseMpdTree(tree, `${BASE}m.mpd`));
  assert.equal(dash.result.adaptationSets[0].childUrls.length, 0);
  const dashAttempts = Math.floor(TEMPLATE_LIMITS.urlBytes / (urlLength + 1)) + 1;
  assert.ok(dash.urls <= 2 * dashAttempts + 10 * 6 + 10, `${dash.urls} URL constructions`);
});

// ---------------------------------------------------------------- SEC25b

// Bases of every shape the review listed, http(s) and not.
const DIFF_BASES = [
  'https://h.example.org',
  'https://h.example.org/',
  'https://h.example.org/a/b?',
  'https://h.example.org/a/b#',
  'https://h.example.org/a/b?#',
  'https://h.example.org/a/b/c?x=/y/../z&k=?#?f',
  'https://h.example.org/a/b#f?g',
  'https://user:pw@h.example.org:8443/a/b;p?q#f',
  'https://[2001:db8::1]:8443/a/b?q',
  'http://192.0.2.7/a/b?q',
  'https://bücher.example/ä/%41?q=ü',
  'https://h.example.org/a;x/b%2Fc/d;p?q',
  `https://h.example.org/a/b?${'q'.repeat(5000)}`,
  'HTTPS://H.EXAMPLE.ORG/A/B?Q#F',
  'http://h.example.org/a/b?q#f',
  `https://h.example.org/ks/${'t'.repeat(3900)}/m.m3u8?sig=1`,
  'https://h.example.org/a/b/',
  'https://h.example.org/a/..%2e/b?q',
  'blob:https://h.example.org/0f0e0d0c-0000-4000-8000-000000000001',
  'data:,x',
  'file:///a/b?q',
  'about:blank',
];

// Same-scheme and other-scheme prefixes (with case, whitespace, tab,
// newline and C0 variants) × the forms the URL parser tells apart.
const PREFIXES = ['', 'https:', 'HTTPS:', 'Https:', 'http:', 'HTTP:', ' https:', 'https: ', '\thttps:', 'ht\ttps:', 'htt\nps:', '\u0001https:', 'https:\u0001',
  'ftp:', 'file:', 'ws:', 'data:', 'blob:', 'javascript:', 'mailto:', 'skd:', 'g:', '1http:', 'h+t.t-p:'];
const SUFFIXES = ['', '#', '#f', '#f?g', '?', '?q', '?q#f', 'g', 'g?q#f', '/', '/g', '//', '//g', '//g/x?y#z', '///g', '\\g', '\\\\g', '/\\g', '\\/g',
  ':', ' ', '\t', '.', '..', '../g', '%2e%2e/g', '[::1]', '//[::1]:99999/x', '//u:p@g:8080/x', '//%67/x', ' #f ', '\u0000', '\u007f', 'ü/€?ß#ç'];

/** new URL(ref, base) as an http(s) href, null when it throws or is not http(s). */
function parserHref(ref, base) {
  try {
    const u = new URL(ref, base);
    return u.protocol === 'http:' || u.protocol === 'https:' ? u.href : null;
  } catch {
    return null;
  }
}

test('resolveRef: a generated differential against the URL parser (SEC25b 必-1)', () => {
  const refs = [...new Set([...REFS, ...PREFIXES.flatMap((p) => SUFFIXES.map((s) => p + s))])];
  assert.ok(DIFF_BASES.length >= 20 && refs.length >= 150);
  const diffs = [];
  for (const base of DIFF_BASES) {
    for (const ref of refs) {
      const want = parserHref(ref, base);
      const got = resolveRef(ref, base);
      if ((got ? got.href : null) !== want) diffs.push(`${JSON.stringify(ref)} against ${base.slice(0, 50)}: ${got && got.href.slice(0, 80)} != ${want && want.slice(0, 80)}`);
    }
  }
  assert.deepEqual(diffs.slice(0, 10), [], `${diffs.length} of ${refs.length * DIFF_BASES.length} pairs differ`);
});

test('budgets: an absolute address costs no base, a root-relative one only the origin (SEC25b 必-2)', () => {
  // a 3.9 KB token in the playlist's path, a four-hour lecture of short addresses
  const tokenBase = `https://h.example.org/ks/${'t'.repeat(3900)}/index.m3u8`;
  const n = 7200;
  const playlist = (name) => '#EXTM3U\n' + Array.from({ length: n }, (_, i) => `#EXTINF:2,\n${name(i)}\n`).join('') + '#EXT-X-ENDLIST\n';
  assert.equal(parseHls(playlist((i) => `https://cdn.example.net/s${i}.ts`), tokenBase).segments.length, n);
  const root = parseHls(playlist((i) => `/seg/s${i}.ts`), tokenBase);
  assert.equal(root.segments.length, n);
  assert.equal(root.segments[0], 'https://h.example.org/seg/s0.ts');
  assert.equal(parseHls(playlist((i) => `//cdn.example.net/s${i}.ts`), tokenBase).segments.length, n);
  // DASH: the same addresses in one SegmentList under an MPD address with a path token
  const mpdUrl = `https://h.example.org/ks/${'t'.repeat(3900)}/manifest.mpd`;
  const segs = Array.from({ length: n }, (_, i) => `<SegmentURL media="https://cdn.example.net/s${i}.mp4"/>`).join('');
  const mpd = `<MPD><Period><AdaptationSet contentType="video"><SegmentList>${segs}</SegmentList><Representation id="v" bandwidth="1"/></AdaptationSet></Period></MPD>`;
  assert.equal(parseMpd(mpd, mpdUrl).adaptationSets[0].childUrls.length, n);
  // what each form is charged
  assert.equal(resolveCost('https://cdn.example.net/s1.ts', tokenBase, 4096, 'https://cdn.example.net/s1.ts'), 'https://cdn.example.net/s1.ts'.length);
  assert.equal(resolveCost('/s1.ts', tokenBase, 4096, 'https://h.example.org/s1.ts'), 'https://h.example.org/s1.ts'.length);
  assert.ok(resolveCost('s1.ts', tokenBase, 4096, null) > 3900); // a relative one reads the path
});

test('DASH: a Period\'s base is read only when one of its AdaptationSets is (SEC25b 低-3)', () => {
  const empty = Array.from({ length: 500 }, (_, i) => `<Period><BaseURL>p${i}/</BaseURL></Period>`).join('');
  const mpd = `<MPD><Period><BaseURL>first/</BaseURL><AdaptationSet contentType="video"><Representation id="v" bandwidth="1"><BaseURL>v.mp4</BaseURL></Representation></AdaptationSet></Period>${empty}</MPD>`;
  const { result, urls } = countUrls(() => parseMpd(mpd, `${BASE}m.mpd`));
  assert.deepEqual(result.adaptationSets[0].childUrls, [`${BASE}first/v.mp4`]);
  assert.ok(urls < 30, `${urls} URL constructions for 501 Periods`);
});

// ---------------------------------------------------------------- one budget per detection (SEC25c)

/** countUrls for an async fn. */
async function countUrlsAsync(fn) {
  const Original = globalThis.URL;
  let n = 0;
  globalThis.URL = class extends Original {
    constructor(...args) {
      n++;
      super(...args);
    }
  };
  try {
    return { result: await fn(), urls: n };
  } finally {
    globalThis.URL = Original;
  }
}

test('detection budget: the value is fixed here', () => {
  assert.deepEqual({ ...LIMITS }, {
    manifests: 12,
    probes: 8,
    files: 8,
    urlBytes: 48 * 1024 * 1024,
    urlLength: 8192,
    frames: 128,
    elements: 1000,
    sources: 16,
    resources: 10000,
    scanChars: 16 * 1024 * 1024,
    requestChars: 4 * 1024 * 1024,
    attrLength: 256,
    attrNames: 32,
    fileTracks: 1000,
  });
  // every address detection keys is hashed by its content in V8 (SEC28b 中-4)
  assert.ok(LIMITS.urlLength <= 16383);
  assert.equal(LIMITS.urlLength, MAX_MANIFEST_URL_LENGTH);
  assert.ok(Object.isFrozen(LIMITS));
});

test('detection budget: a parser spends at most min(own limit, budget.left) and takes it off', () => {
  const text = '#EXTM3U\n' + Array.from({ length: 100 }, (_, i) => `#EXTINF:2,\nhttps://cdn.example.net/s${String(i).padStart(3, '0')}.ts\n`).join('');
  const len = 'https://cdn.example.net/s000.ts'.length;
  const budget = { left: 10 * len };
  assert.equal(parseHls(text, `${BASE}i.m3u8`, { budget }).segments.length, 10);
  assert.ok(budget.left >= 0 && budget.left < len);
  assert.equal(parseHls(text, `${BASE}i.m3u8`, { budget: { left: 0 } }).segments.length, 0);
  assert.equal(parseHls(text, `${BASE}i.m3u8`).segments.length, 100); // no budget: as before
  const mpd = `<MPD><Period><AdaptationSet contentType="video"><SegmentList>${Array.from({ length: 100 }, (_, i) => `<SegmentURL media="https://cdn.example.net/s${String(i).padStart(3, '0')}.mp4"/>`).join('')}</SegmentList><Representation id="v" bandwidth="1"/></AdaptationSet></Period></MPD>`;
  const dashBudget = { left: 100 * (len + 1) };
  assert.equal(parseMpd(mpd, `${BASE}m.mpd`, { budget: dashBudget }).adaptationSets[0].childUrls.length, 100);
  assert.equal(dashBudget.left, 0);
  assert.equal(parseMpd(mpd, `${BASE}m.mpd`, { budget: { left: 99 * (len + 1) } }).adaptationSets[0].childUrls.length, 0); // the list does not fit
  assert.equal(parseMpd(mpd, `${BASE}m.mpd`).adaptationSets[0].childUrls.length, 100);
});

test('detection budget: manifests that could each spend it share one budget; tracks are still found', async () => {
  const n = 2000;
  const texts = {};
  const resources = [];
  const add = (url, text) => {
    texts[url] = text;
    resources.push({ url, initiatorType: 'fetch' });
  };
  for (let k = 0; k < 3; k++) {
    const segs = Array.from({ length: n }, (_, i) => `<SegmentURL media="https://cdn.example.net/d${k}/s${i}.mp4"/>`).join('');
    add(`${BASE}d${k}.mpd`, `<MPD><Period><AdaptationSet contentType="video" lang="l${k}"><SegmentList>${segs}</SegmentList><Representation id="v" bandwidth="1"/></AdaptationSet></Period></MPD>`);
  }
  for (let k = 0; k < 2; k++) {
    add(`${BASE}h${k}.m3u8`, '#EXTM3U\n' + Array.from({ length: n }, (_, i) => `#EXTINF:2,\nhttps://cdn.example.net/h${k}/s${i}.ts\n`).join('') + '#EXT-X-ENDLIST\n');
  }
  // a master read after the budget is gone still makes its track
  add(`${BASE}master.m3u8`, '#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=1280x720\nv720/index.m3u8\n');
  const frames = [{ frameId: 0, url: 'https://h.example.org/p', encryptedEvents: 0, reads: [], elements: [], resources }];
  const small = 3000; // characters, about a hundred addresses in all
  const { result, urls } = await countUrlsAsync(() => detectTracks({ frames }, S.fakeIo(texts, {}), { ...LIMITS, urlBytes: small }));
  assert.equal(result.tracks.filter((t) => t.format === 'dash').length, 3);
  assert.equal(result.tracks.filter((t) => t.format === 'hls').length, 3);
  assert.ok(result.tracks.some((t) => t.format === 'hls' && t.quality === '720p'));
  // without the shared budget this is 5 × 2000 addresses, two constructions each
  assert.ok(urls < 1000, `${urls} URL constructions for six manifests under a ${small}-character budget`);
});
