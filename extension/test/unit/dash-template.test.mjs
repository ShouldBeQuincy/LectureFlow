// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// DASH SegmentTemplate → pattern (SEC18 低-9, SEC23): the page controls
// both the template and the request URLs, so each pattern must stay linear
// and their number bounded. Every adversarial case first checks the
// pattern's shape (bounded digit quantifiers, never two with only digits
// between them, no unescaped metacharacter) and only then runs it, so a
// backtracking pattern fails the shape check instead of hanging the test.
// Counts are checked by structure (how many patterns, how many reads),
// never with inputs large enough to need timing.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseMpd, parseMpdTree, dashTracks, templateToRegex, resolvedTemplateRegex, childMatchers, isTemplatePattern, TEMPLATE_LIMITS } from '../../src/lib/dash.js';
import { detectTracks } from '../../src/lib/detect.js';
import { parseXml, childElements } from '../../src/lib/xml.js';
import * as S from '../support/synth.mjs';

const BASE = 'https://media.example.org/v/';
const SLOW_MS = 1000; // a linear pattern on these inputs takes well under 1 ms

/**
 * Shape of a pattern source built from a template: anchors, escaped
 * literals, plain literals and bounded digit quantifiers (\d{k,n} with
 * n <= 32k) only, and a non-digit literal between any two quantifiers.
 * Returns the number of quantifiers; throws on anything else.
 */
function assertLinearShape(src) {
  let body = src;
  if (body.startsWith('^')) body = body.slice(1);
  if (body.endsWith('$') && !body.endsWith('\\$')) body = body.slice(0, -1);
  let quantifiers = 0;
  let since = null; // literal text since the last quantifier
  for (let i = 0; i < body.length; ) {
    const q = /^\\d(?:\+|\{(\d+),(\d*)\})/.exec(body.slice(i, i + 16));
    if (q) {
      assert.ok(q[2] && Number(q[2]) <= Number(q[1]) * TEMPLATE_LIMITS.digits, `unbounded digit quantifier ${q[0]} in ${src}`);
      assert.ok(since === null || /[^0-9]/.test(since), `two digit quantifiers with only digits between them in ${src}`);
      quantifiers++;
      since = '';
      i += q[0].length;
      continue;
    }
    let ch;
    if (body[i] === '\\') {
      ch = body[i + 1];
      assert.ok(ch !== undefined && /[.*+?^${}()|[\]\\/-]/.test(ch), `unexpected escape \\${ch} in ${src}`);
      i += 2;
    } else {
      ch = body[i];
      assert.ok(!/[.*+?^${}()|[\]\\]/.test(ch), `unescaped metacharacter ${ch} in ${src}`);
      i += 1;
    }
    if (since !== null) since += ch;
  }
  return quantifiers;
}

/** Shape check first, then a timed run on each URL. */
function assertSafe(src, urls) {
  if (src === null) return [];
  assertLinearShape(src);
  const re = new RegExp(src);
  return urls.map((u) => {
    const t0 = performance.now();
    const r = re.test(u);
    const ms = performance.now() - t0;
    assert.ok(ms < SLOW_MS, `pattern ${src.slice(0, 80)} took ${ms.toFixed(0)} ms on a ${u.length}-character URL`);
    return r;
  });
}

const digits = (n) => '1'.repeat(n);

test('template patterns: the SEC18 case (six adjacent $Number$, 200 digits) returns at once', () => {
  const src = resolvedTemplateRegex('$Number$'.repeat(6) + '.m4s', BASE, 'r', 1);
  const [miss, hit, long] = assertSafe(src, [`${BASE}${digits(200)}.mp4`, `${BASE}${digits(100)}.m4s`, `${BASE}${digits(200)}.m4s`]);
  assert.equal(miss, false);
  assert.equal(hit, true);
  assert.equal(long, false); // six numbers have at most 6 × 32 digits
  assert.equal(src, '^https://media\\.example\\.org/v/\\d{6,192}\\.m4s$');
});

test('template patterns: adjacent identifiers of every digit kind merge into one quantifier', () => {
  const src = resolvedTemplateRegex('s$Number$$Time$$SubNumber$$Bandwidth$$Foo$x.m4s', BASE, 'r', null);
  assert.equal(assertLinearShape(src), 1);
  const [a, b, c] = assertSafe(src, [`${BASE}s${digits(300)}y.m4s`, `${BASE}s12345x.m4s`, `${BASE}s1234x.m4s`]);
  assert.deepEqual([a, b, c], [false, true, false]); // \d+ ×5 needs five digits, as before
});

test('template patterns: an identifier repeated dozens of times is refused', () => {
  assert.equal(resolvedTemplateRegex('$Number$'.repeat(40) + '.m4s', BASE, 'r', 1), null);
  assert.equal(resolvedTemplateRegex('$Number$/'.repeat(40) + '.m4s', BASE, 'r', 1), null);
  assert.equal(templateToRegex('$Time$'.repeat(TEMPLATE_LIMITS.identifiers + 1), 'r', 1), null);
  // up to the limit is fine when separated
  const ok = resolvedTemplateRegex('$Number$_'.repeat(TEMPLATE_LIMITS.identifiers) + '.m4s', BASE, 'r', 1);
  assert.equal(assertLinearShape(ok), TEMPLATE_LIMITS.identifiers);
  const [hit, miss] = assertSafe(ok, [`${BASE}${'7_'.repeat(TEMPLATE_LIMITS.identifiers)}.m4s`, `${BASE}${`${digits(50)}_`.repeat(7)}${digits(500)}.mp4`]);
  assert.deepEqual([hit, miss], [true, false]);
});

test('template patterns: quantifiers separated only by digits are refused', () => {
  assert.equal(resolvedTemplateRegex('$Number$0$Time$0$Number$0$Time$.m4s', BASE, 'r', 1), null);
  assert.equal(resolvedTemplateRegex('$Number$$RepresentationID$$Time$.m4s', BASE, '7', 1), null); // numeric id between
  assert.equal(resolvedTemplateRegex('$Number$$Bandwidth$$Time$.m4s', BASE, 'r', 500000), null); // known bandwidth between
  assert.equal(templateToRegex('$Number$12$Time$', 'r', 1), null);
  // a non-digit id between is fine
  const src = resolvedTemplateRegex('$Number$$RepresentationID$$Time$.m4s', BASE, 'v7', 1);
  const [hit, miss] = assertSafe(src, [`${BASE}12v734.m4s`, `${BASE}${digits(400)}v7${digits(400)}.mp4`]);
  assert.deepEqual([hit, miss], [true, false]);
  // digits after a quantifier, then a non-digit, are fine
  const tail = resolvedTemplateRegex('$Number$000.m4s', BASE, 'r', 1);
  const [t1, t2, t3] = assertSafe(tail, [`${BASE}5000.m4s`, `${BASE}000.m4s`, `${BASE}${digits(2000)}.mp4`]);
  assert.deepEqual([t1, t2, t3], [true, false, false]);
});

test('template patterns: an over-long literal, id or base is refused', () => {
  assert.equal(resolvedTemplateRegex('a'.repeat(TEMPLATE_LIMITS.templateLength) + '$Number$', BASE, 'r', 1), null);
  assert.equal(templateToRegex('a'.repeat(TEMPLATE_LIMITS.templateLength + 1), 'r', 1), null);
  assert.equal(resolvedTemplateRegex('$RepresentationID$/$Number$.m4s', BASE, 'x'.repeat(TEMPLATE_LIMITS.repIdLength + 1), 1), null);
  assert.equal(resolvedTemplateRegex('$Number$.m4s', `${BASE}${'p/'.repeat(TEMPLATE_LIMITS.resolvedLength)}`, 'r', 1), null);
  // a long but allowed literal still works
  const lit = 'q'.repeat(900);
  const src = resolvedTemplateRegex(`${lit}$Number$.m4s`, BASE, 'r', 1);
  const [hit, miss] = assertSafe(src, [`${BASE}${lit}42.m4s`, `${BASE}${lit}${digits(3000)}.mp4`]);
  assert.deepEqual([hit, miss], [true, false]);
});

test('template patterns: regex metacharacters in the template are literal text', () => {
  const tpl = 'seg(.*)+[x]{2}^|\\d+$Number$a+b?c*.m4s?x=(a|b)*&y=$Time$';
  const src = resolvedTemplateRegex(tpl, BASE, 'r', 1);
  assert.equal(assertLinearShape(src), 2);
  const real = new URL(tpl.replace('$Number$', '42').replace('$Time$', '9'), BASE).href;
  const [hit, a, b] = assertSafe(src, [real, real.replace('42', ''), `${BASE}seg${'x'.repeat(3000)}`]);
  assert.deepEqual([hit, a, b], [true, false, false]);
  // a RepresentationID full of metacharacters, too
  const rep = resolvedTemplateRegex('$RepresentationID$/$Number$.m4s', BASE, '(a+)+$', 1);
  assertLinearShape(rep);
  const [r1, r2] = assertSafe(rep, [`${BASE}(a+)+$/3.m4s`, `${BASE}aaaa/3.m4s`]);
  assert.deepEqual([r1, r2], [true, false]);
});

test('template patterns: an unpaired $ stays literal text; $$ is a dollar', () => {
  const src = resolvedTemplateRegex('seg$Number.m4s$Time$', BASE, 'r', 1);
  assert.equal(src, '^https://media\\.example\\.org/v/seg\\$Number\\.m4s\\d{1,32}$');
  const [hit, miss] = assertSafe(src, [`${BASE}seg$Number.m4s12`, `${BASE}seg12.m4s`]);
  assert.deepEqual([hit, miss], [true, false]);
  const dd = resolvedTemplateRegex('a$$$Number$$$.m4s', BASE, 'r', 1);
  assert.equal(dd, '^https://media\\.example\\.org/v/a\\$\\d{1,32}\\$\\.m4s$');
  const odd = resolvedTemplateRegex('$'.repeat(301) + '$Number$', BASE, 'r', 1);
  assert.equal(odd, null); // 150 "$$" identifiers is past the limit
  const lone = resolvedTemplateRegex(`x$${'N'.repeat(900)}$Number$`, BASE, 'r', 1);
  assertSafe(lone, [`${BASE}x$${'N'.repeat(900)}${digits(2000)}.mp4`]);
});

test('template patterns: text that looks like our markers is refused, not mixed up', () => {
  assert.equal(resolvedTemplateRegex('lfvar0lf$Number$.m4s', BASE, 'r', 1), null);
  assert.equal(resolvedTemplateRegex('$Number$.m4s', 'https://lfvar1lf.example.org/', 'r', 1), null);
  // SEC23 低-1: these were silently rewritten (the previous builders gave
  // \d+var0lf0a… and ^https://\d+\.example\.org/x$, matching no real
  // segment); now any "lfvar" in the template or base refuses it
  assert.equal(resolvedTemplateRegex('lfvar0$Number$0a$Time$.m4s', BASE, 'r', 1), null);
  assert.equal(resolvedTemplateRegex('https://LFVAR0LF.example.org/x#$Number$', BASE, 'r', 1), null);
  assert.equal(resolvedTemplateRegex('$Number$.m4s', 'https://media.example.org/LfVaR/', 'r', 1), null);
});

test('template patterns: a number has at most TEMPLATE_LIMITS.digits digits', () => {
  const src = resolvedTemplateRegex('$Number$.m4s', BASE, 'r', 1);
  const [ok, over] = assertSafe(src, [`${BASE}${digits(TEMPLATE_LIMITS.digits)}.m4s`, `${BASE}${digits(TEMPLATE_LIMITS.digits + 1)}.m4s`]);
  assert.deepEqual([ok, over], [true, false]);
  const two = resolvedTemplateRegex('$Number$$Time$.m4s', BASE, 'r', 1);
  const [ok2, over2] = assertSafe(two, [`${BASE}${digits(2 * TEMPLATE_LIMITS.digits)}.m4s`, `${BASE}${digits(2 * TEMPLATE_LIMITS.digits + 1)}.m4s`]);
  assert.deepEqual([ok2, over2], [true, false]);
});

test('child matchers: only template-shaped patterns, each once, at most TEMPLATE_LIMITS.matchers', () => {
  const src = resolvedTemplateRegex('$Number$.m4s', BASE, 'r', 1);
  const m = childMatchers();
  assert.equal(m.add(src, 'A'), true);
  assert.equal(m.add(src, 'B'), false); // the first owner keeps it
  assert.equal(m.size, 1);
  assert.equal(m.find(`${BASE}${digits(30)}.m4s`), 'A');
  const long = `${BASE}${digits(TEMPLATE_LIMITS.urlLength)}.m4s`;
  assert.ok(long.length > TEMPLATE_LIMITS.urlLength);
  assert.equal(m.find(long), undefined); // past the length limit, never tested
  assert.equal(m.find(null), undefined);
  for (const bad of ['(', '^(a+)+$', '^\\d+\\d+$', '^\\d{1,32}\\d{1,32}$', '^\\d{1,32}0\\d{1,32}$', 'x', '^a\\$', '^a.b$']) {
    assert.equal(isTemplatePattern(bad), false, bad);
    assert.equal(m.add(bad, 'C'), false, bad);
  }
  assert.equal(isTemplatePattern(src), true);
  for (let i = 0; i < TEMPLATE_LIMITS.matchers + 50; i++) m.add(resolvedTemplateRegex(`r${i}/$Number$.m4s`, BASE, 'r', 1), i);
  assert.equal(m.size, TEMPLATE_LIMITS.matchers);
});

// The pattern builder before SEC18 低-9, kept to check that real templates
// still match exactly the same URLs. Run only on short, safe inputs.
function oldResolvedTemplateRegex(template, base, repId, bandwidth) {
  const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const one = (t) => {
    let out = '';
    const re = /\$([A-Za-z]*)(%0\d+d)?\$/g;
    let last = 0;
    let m;
    while ((m = re.exec(t))) {
      out += esc(t.slice(last, m.index));
      const id = m[1];
      if (id === '') out += '\\$';
      else if (id === 'RepresentationID') out += esc(repId || '');
      // a known bandwidth zero-padded to the format's width (P6 D5; the old builder did not pad)
      else if (id === 'Bandwidth') out += bandwidth ? String(bandwidth).padStart(m[2] ? Number(m[2].slice(2, -1)) : 0, '0') : '\\d+';
      else out += '\\d+';
      last = re.lastIndex;
    }
    return out + esc(t.slice(last));
  };
  const vars = [];
  const marked = template.replace(/\$([A-Za-z]*)(%0\d+d)?\$/g, (m) => {
    vars.push(m);
    return `lfvar${vars.length - 1}lf`;
  });
  const abs = new URL(marked, base);
  abs.hash = '';
  let out = esc(abs.href);
  vars.forEach((v, i) => {
    out = out.replace(`lfvar${i}lf`, () => one(v));
  });
  return '^' + out + '$';
}

test('template patterns: real templates match the same URLs as before', () => {
  const cases = [
    ['$RepresentationID$/seg_$Number%05d$.mp4', 'v720', 1, ['v720/seg_00012.mp4', 'v720/seg_7.mp4', 'v1080/seg_00012.mp4', 'v720/seg_.mp4']],
    ['$RepresentationID$/init.mp4', 'v720', 1, ['v720/init.mp4', 'v720/init.mp5']],
    ['v/$RepresentationID$/seg-$Number%05d$.m4s', 'video=1', 2, ['v/video=1/seg-00001.m4s', 'v/video=2/seg-00001.m4s']],
    ['$RepresentationID$/$Number$.m4s?sig=SIGSIG', 'a', 1, ['a/3.m4s?sig=SIGSIG', 'a/3.m4s?sig=X', 'a/3.m4s']],
    ['a_$Bandwidth$_$Time$.m4s', 'r', 500, ['a_500_123.m4s', 'a_501_123.m4s', 'a_500_.m4s']],
    ['a_$Bandwidth$_$Time$.m4s', 'r', null, ['a_500_123.m4s', 'a__123.m4s']],
    ['$RepresentationID$/$Bandwidth%08d$/$Time$.m4s', 'r', 96000, ['r/96000/0.m4s', 'r/00096000/0.m4s']],
    ['chunk-stream$RepresentationID$-$Number%05d$.m4s', '0', 1, ['chunk-stream0-00001.m4s', 'chunk-stream1-00001.m4s']],
    ['$Number$$Time$.m4s', 'r', 1, ['1.m4s', '12.m4s', '123456.m4s', 'x.m4s', `${'9'.repeat(40)}.m4s`]],
    ['seg$$x$Number$.m4s', 'r', 1, ['seg$x5.m4s', 'segx5.m4s']],
    ['../up/$Number$.m4s#frag$Time$', 'r', 1, ['up/5.m4s', 'v/up/5.m4s']],
    ['https://cdn.example.net/$RepresentationID$/$SubNumber$-$Number$.m4s', 'r', 1, ['https://cdn.example.net/r/1-2.m4s', 'https://cdn.example.net/r/-2.m4s']],
  ];
  for (const [tpl, rep, bw, paths] of cases) {
    const now = resolvedTemplateRegex(tpl, BASE, rep, bw);
    const before = oldResolvedTemplateRegex(tpl, BASE, rep, bw);
    assertLinearShape(now);
    for (const p of paths) {
      const url = new URL(p, BASE).href;
      assert.equal(new RegExp(now).test(url), new RegExp(before).test(url), `${tpl} on ${url}`);
    }
  }
});

test('a known $Bandwidth%0<w>d$ is zero-padded to w digits; an unknown one is still any number (P6 D5)', () => {
  const at = (p) => new URL(p, BASE).href;
  const re = new RegExp(resolvedTemplateRegex('a/$Bandwidth%08d$/$Number%05d$.m4s', BASE, 'r', 500000));
  assertLinearShape(re.source);
  assert.ok(re.test(at('a/00500000/00001.m4s')));
  assert.ok(!re.test(at('a/500000/00001.m4s')), 'not the unpadded value');
  // wider than the width: the value as it is (printf does not cut)
  assert.ok(new RegExp(resolvedTemplateRegex('a/$Bandwidth%03d$.m4s', BASE, 'r', 500000)).test(at('a/500000.m4s')));
  // no format, or an unknown bandwidth: as before
  assert.ok(new RegExp(resolvedTemplateRegex('a/$Bandwidth$.m4s', BASE, 'r', 500000)).test(at('a/500000.m4s')));
  const unknown = new RegExp(resolvedTemplateRegex('a/$Bandwidth%08d$.m4s', BASE, 'r', null));
  assert.ok(unknown.test(at('a/00500000.m4s')) && unknown.test(at('a/7.m4s')));
  // the unresolved form too
  assert.equal(templateToRegex('a_$Bandwidth%08d$_$Time$.m4s', 'r', 500000), 'a_00500000_\\d{1,32}\\.m4s');
  // a width past TEMPLATE_LIMITS.digits (LectureFlow refuses those) pads nothing, and allocates nothing
  assert.equal(templateToRegex(`a_$Bandwidth%0${TEMPLATE_LIMITS.digits + 1}d$.m4s`, 'r', 500000), 'a_500000\\.m4s');
  assert.equal(templateToRegex(`a_$Bandwidth%0${TEMPLATE_LIMITS.digits}d$.m4s`, 'r', 500000), `a_${'500000'.padStart(TEMPLATE_LIMITS.digits, '0')}\\.m4s`);
  assert.equal(templateToRegex('a_$Bandwidth%0999999999d$.m4s', 'r', 500000), 'a_500000\\.m4s');
  // a padded known bandwidth is literal digits: between two numbers it is still refused
  assert.equal(resolvedTemplateRegex('$Number$$Bandwidth%08d$$Time$.m4s', BASE, 'r', 500000), null);
  // and next to one, it joins the literal text before the quantifier as before
  assert.equal(templateToRegex('$Bandwidth%08d$$Number$.m4s', 'r', 500000), '00500000\\d{1,32}\\.m4s');
});

test('detection: a crafted MPD and page URL do not stall detection; a normal template still absorbs', async () => {
  const mpdUrl = `${BASE}manifest.mpd`;
  const mpd = `<MPD mediaPresentationDuration="PT10M"><BaseURL>${BASE}</BaseURL><Period>
      <AdaptationSet contentType="video"><SegmentTemplate media="${'$Number$'.repeat(6)}.m4s" initialization="$RepresentationID$/init.mp4"/><Representation id="v1" bandwidth="1" height="720"/></AdaptationSet>
      <AdaptationSet contentType="audio"><SegmentTemplate media="${'$Time$0'.repeat(6)}.m4s" initialization="${'$Number$'.repeat(40)}.mp4"/><Representation id="a1" bandwidth="1"/></AdaptationSet>
    </Period></MPD>`;
  // shape first: nothing in the parsed MPD may backtrack
  for (const s of parseMpd(mpd, mpdUrl).adaptationSets) for (const p of s.childPatterns) assertLinearShape(p);
  const crafted = `${BASE}${digits(200)}.mp4`;
  const init = `${BASE}v1/init.mp4`;
  const frames = [{ frameId: 0, url: 'https://h.example.org/p', encryptedEvents: 0, reads: [], resources: [
    { url: mpdUrl, initiatorType: 'fetch' },
    { url: init, initiatorType: 'fetch' },
    { url: crafted, initiatorType: 'fetch' },
  ], elements: [] }];
  const io = S.fakeIo({ [mpdUrl]: mpd }, { [crafted]: { contentType: 'video/mp4', size: 10 } });
  const t0 = performance.now();
  const r = await detectTracks({ frames }, io);
  assert.ok(performance.now() - t0 < 5 * SLOW_MS);
  assert.ok(!r.tracks.some((t) => t.url === init), 'the init segment belongs to the video set');
  assert.ok(r.tracks.some((t) => t.url === crafted && t.format === 'file'), 'the crafted URL stays a file of its own');
  const video = dashTracks(parseMpd(mpd, mpdUrl), mpdUrl).find((t) => t.kind === 'video');
  assert.equal(video.childPatterns.length, 2);
});

// ---------------------------------------------------------------- counts (SEC23)

const reps = (n, prefix = 'r') => Array.from({ length: n }, (_, i) => `<Representation id="${prefix}${i}" bandwidth="${1000 + i}" height="${100 + i}"/>`).join('');
const TEMPLATE = '<SegmentTemplate media="$RepresentationID$/seg-$Number$.m4s" initialization="$RepresentationID$/init.mp4"/>';

test('counts: a few hundred Representations give at most patternRepsPerSet / patternsPerMpd patterns; all stay variants', () => {
  const one = `<MPD><BaseURL>${BASE}</BaseURL><Period><AdaptationSet contentType="video">${TEMPLATE}${reps(300)}</AdaptationSet></Period></MPD>`;
  const [set] = parseMpd(one, BASE).adaptationSets;
  assert.equal(set.representations.length, 300);
  assert.equal(set.childPatterns.length, 2 * TEMPLATE_LIMITS.patternRepsPerSet);
  assert.equal(set.maxHeight, 399);
  assert.equal(set.maxBandwidth, 1299);
  // the first ones keep their patterns, the later ones none
  const [track] = dashTracks(parseMpd(one, BASE), BASE);
  const owns = (u) => track.childPatterns.some((p) => new RegExp(p).test(u));
  assert.ok(owns(`${BASE}r0/seg-1.m4s`) && owns(`${BASE}r${TEMPLATE_LIMITS.patternRepsPerSet - 1}/init.mp4`));
  assert.ok(!owns(`${BASE}r${TEMPLATE_LIMITS.patternRepsPerSet}/init.mp4`));
  const many = `<MPD><BaseURL>${BASE}</BaseURL><Period>${Array.from({ length: 8 }, (_, k) => `<AdaptationSet contentType="video" lang="l${k}">${TEMPLATE}${reps(60, `s${k}r`)}</AdaptationSet>`).join('')}</Period></MPD>`;
  const sets = parseMpd(many, BASE).adaptationSets;
  assert.equal(sets.length, 8);
  assert.ok(sets.every((s) => s.childPatterns.length <= 2 * TEMPLATE_LIMITS.patternRepsPerSet));
  assert.equal(sets.reduce((n, s) => n + s.childPatterns.length, 0), TEMPLATE_LIMITS.patternsPerMpd);
  assert.equal(sets.reduce((n, s) => n + s.representations.length, 0), 480);
});

test('counts: an MPD is read up to TEMPLATE_LIMITS.representations Representations', () => {
  const n = TEMPLATE_LIMITS.representations + 100;
  const xml = `<MPD><Period><AdaptationSet contentType="video">${reps(n)}</AdaptationSet><AdaptationSet contentType="audio">${reps(5, 'a')}</AdaptationSet></Period></MPD>`;
  const sets = parseMpd(xml, BASE).adaptationSets;
  assert.equal(sets.length, 1); // the audio set comes after the limit
  assert.equal(sets[0].representations.length, TEMPLATE_LIMITS.representations);
});

test('counts: parsing reads each AdaptationSet a fixed number of times, not once per Representation (SEC23 中-2)', () => {
  const read = (n) => {
    const root = parseXml(`<MPD><Period><AdaptationSet contentType="video"><SegmentList><SegmentURL media="s1.mp4"/><SegmentURL media="s2.mp4"/></SegmentList>${reps(n)}</AdaptationSet></Period></MPD>`);
    const as = childElements(childElements(root, 'Period')[0], 'AdaptationSet')[0];
    const kids = as.children;
    let reads = 0;
    Object.defineProperty(as, 'children', { get() { reads++; return kids; } });
    const parsed = parseMpdTree(root, BASE);
    return { reads, parsed };
  };
  const small = read(10);
  const big = read(300);
  assert.equal(big.parsed.adaptationSets[0].representations.length, 300);
  assert.equal(big.reads, small.reads, `reads of the AdaptationSet grow with Representations: ${small.reads} → ${big.reads}`);
  // the shared list is resolved once per base, not once per Representation
  assert.deepEqual(big.parsed.adaptationSets[0].childUrls, [`${BASE}s1.mp4`, `${BASE}s2.mp4`]);
});

test('counts: merging Periods with long segment lists does not spread arguments (SEC23 低-2)', () => {
  const urls = new Array(200000).fill(`${BASE}s.mp4`);
  const set = (period) => ({ period, ordinal: 0, kind: 'video', lang: '', protected: false, maxHeight: 720, maxBandwidth: 1, representations: [{ id: 'v', bandwidth: 1, width: null, height: 720, codecs: '' }], childUrls: urls, childPatterns: [] });
  const tracks = dashTracks({ type: 'static', durationSec: null, adaptationSets: [set(0), set(1)] }, `${BASE}m.mpd`);
  assert.equal(tracks.length, 1);
  assert.deepEqual(tracks[0].childUrls, [`${BASE}s.mp4`]);
});

test('counts: the bundled fixtures keep every pattern (real MPDs are far below the limits)', () => {
  const mpd = S.genericTexts[`${S.GEN_HOST}/dash/protected.mpd`];
  const sets = parseMpd(mpd, `${S.GEN_HOST}/dash/protected.mpd`).adaptationSets;
  const repsTotal = sets.reduce((n, s) => n + s.representations.length, 0);
  assert.equal(sets.reduce((n, s) => n + s.childPatterns.length, 0), 2 * repsTotal); // media + init each
  assert.ok(repsTotal < TEMPLATE_LIMITS.patternRepsPerSet);
});

test('detection: many MPDs with many Representations stay within TEMPLATE_LIMITS.matchers and still absorb the first ones', async () => {
  const mpds = {};
  const resources = [];
  for (let k = 0; k < 3; k++) {
    const url = `${BASE}m${k}.mpd`;
    mpds[url] = `<MPD><BaseURL>${BASE}m${k}/</BaseURL><Period>${Array.from({ length: 5 }, (_, j) => `<AdaptationSet contentType="video" lang="l${j}">${TEMPLATE}${reps(40, `s${j}r`)}</AdaptationSet>`).join('')}</Period></MPD>`;
    resources.push({ url, initiatorType: 'fetch' });
  }
  const first = `${BASE}m0/s0r0/init.mp4`; // owned: first set, first Representation
  const late = `${BASE}m0/s0r39/init.mp4`; // past patternRepsPerSet: a file of its own
  const shared = Array.from({ length: 50 }, (_, i) => `${BASE}m2/s4r39/x${i}${digits(40)}.mp4`);
  for (const u of [first, late, ...shared]) resources.push({ url: u, initiatorType: 'fetch' });
  const frames = [{ frameId: 0, url: 'https://h.example.org/p', encryptedEvents: 0, reads: [], resources, elements: [] }];
  for (const s of parseMpd(mpds[`${BASE}m0.mpd`], `${BASE}m0.mpd`).adaptationSets) for (const p of s.childPatterns) assertLinearShape(p);
  const r = await detectTracks({ frames }, S.fakeIo(mpds, {}));
  assert.ok(!r.tracks.some((t) => t.url === first), 'the first Representation’s init segment is absorbed');
  assert.ok(r.tracks.some((t) => t.url === late && t.format === 'file'), 'a Representation past the limit has no pattern');
  assert.equal(r.tracks.filter((t) => t.format === 'dash').length, 15);
});

// ---------------------------------------------------------------- limit values (SEC23b)

test('limits: the values are fixed here, so raising one means changing this test', () => {
  assert.deepEqual({ ...TEMPLATE_LIMITS }, {
    templateLength: 1024,
    identifiers: 8,
    digits: 32,
    repIdLength: 256,
    resolvedLength: 4096,
    urlLength: 4096,
    patternRepsPerSet: 32,
    patternsPerMpd: 128,
    matchers: 256,
    representations: 2000,
    adaptationSets: 512,
    segmentUrls: 50000,
    urlBytes: 16 * 1024 * 1024,
    attrLength: 256,
  });
  assert.ok(Object.isFrozen(TEMPLATE_LIMITS));
});

test('limits: each one admits exactly its value and refuses one more', () => {
  const L = TEMPLATE_LIMITS;
  // templateLength
  const tpl = (n) => 'a'.repeat(n - '$Number$.m4s'.length) + '$Number$.m4s';
  assert.notEqual(resolvedTemplateRegex(tpl(L.templateLength), BASE, 'r', 1), null);
  assert.equal(resolvedTemplateRegex(tpl(L.templateLength + 1), BASE, 'r', 1), null);
  // identifiers
  assert.notEqual(resolvedTemplateRegex('$Number$_'.repeat(L.identifiers), BASE, 'r', 1), null);
  assert.equal(resolvedTemplateRegex('$Number$_'.repeat(L.identifiers + 1), BASE, 'r', 1), null);
  // repIdLength
  assert.notEqual(resolvedTemplateRegex('$RepresentationID$/$Number$.m4s', BASE, 'x'.repeat(L.repIdLength), 1), null);
  assert.equal(resolvedTemplateRegex('$RepresentationID$/$Number$.m4s', BASE, 'x'.repeat(L.repIdLength + 1), 1), null);
  // resolvedLength: the resolved URL (with its 12-character marked tail)
  const base = (n) => `${BASE}${'p'.repeat(n - BASE.length - 1)}/`;
  assert.equal(base(L.resolvedLength - 12).length + 12, L.resolvedLength);
  assert.notEqual(resolvedTemplateRegex('$Number$.m4s', base(L.resolvedLength - 12), 'r', 1), null);
  assert.equal(resolvedTemplateRegex('$Number$.m4s', base(L.resolvedLength - 11), 'r', 1), null);
  // urlLength: the same pattern, a URL of exactly the limit and one longer
  const own = childMatchers();
  own.add(resolvedTemplateRegex('$Number$.m4s', base(L.urlLength - 12), 'r', 1), 'B');
  const at = `${base(L.urlLength - 12)}12345678.m4s`;
  const over = `${base(L.urlLength - 12)}123456789.m4s`;
  assert.deepEqual([at.length, over.length], [L.urlLength, L.urlLength + 1]);
  assert.equal(own.find(at), 'B');
  assert.equal(own.find(over), undefined);
  // matchers
  const many = childMatchers();
  for (let i = 0; i < L.matchers; i++) assert.equal(many.add(resolvedTemplateRegex(`r${i}/$Number$.m4s`, BASE, 'r', 1), i), true);
  assert.equal(many.add(resolvedTemplateRegex('one-more/$Number$.m4s', BASE, 'r', 1), 'x'), false);
  assert.equal(many.size, L.matchers);
});

const setXml = (n, prefix, extra = '') => `<AdaptationSet contentType="video" lang="${prefix}">${extra}${reps(n, prefix)}</AdaptationSet>`;
const patternsOf = (xml) => parseMpd(xml, BASE).adaptationSets.reduce((n, s) => n + s.childPatterns.length, 0);

test('limits: patternRepsPerSet and patternsPerMpd, exactly and one more', () => {
  const L = TEMPLATE_LIMITS;
  const one = (n) => `<MPD><BaseURL>${BASE}</BaseURL><Period>${setXml(n, 'a', TEMPLATE)}</Period></MPD>`;
  assert.equal(patternsOf(one(L.patternRepsPerSet)), 2 * L.patternRepsPerSet);
  assert.equal(patternsOf(one(L.patternRepsPerSet + 1)), 2 * L.patternRepsPerSet);
  // sets of patternRepsPerSet each, up to patternsPerMpd distinct patterns, then one Representation more
  const full = L.patternsPerMpd / (2 * L.patternRepsPerSet);
  const sets = Array.from({ length: full }, (_, k) => setXml(L.patternRepsPerSet, `s${k}`, TEMPLATE)).join('');
  assert.equal(patternsOf(`<MPD><BaseURL>${BASE}</BaseURL><Period>${sets}</Period></MPD>`), L.patternsPerMpd);
  assert.equal(patternsOf(`<MPD><BaseURL>${BASE}</BaseURL><Period>${sets}${setXml(1, 'extra', TEMPLATE)}</Period></MPD>`), L.patternsPerMpd);
});

test('limits: representations and adaptationSets, exactly and one more', () => {
  const L = { ...TEMPLATE_LIMITS, adaptationSets: 512 }; // the value pinned above; checked against the module below
  const repsOf = (xml) => parseMpd(xml, BASE).adaptationSets.reduce((n, s) => n + s.representations.length, 0);
  assert.equal(repsOf(`<MPD><Period>${setXml(L.representations, 'v')}</Period></MPD>`), L.representations);
  assert.equal(repsOf(`<MPD><Period>${setXml(L.representations + 1, 'v')}</Period></MPD>`), L.representations);
  const empty = (n) => `<MPD><Period>${'<AdaptationSet contentType="video"/>'.repeat(n)}</Period></MPD>`;
  assert.equal(parseMpd(empty(L.adaptationSets), BASE).adaptationSets.length, L.adaptationSets);
  assert.equal(parseMpd(empty(L.adaptationSets + 1), BASE).adaptationSets.length, L.adaptationSets);
  // text sets count too, and the limit spans Periods
  const mixed = `<MPD><Period>${'<AdaptationSet contentType="text"/>'.repeat(L.adaptationSets - 1)}</Period><Period>${'<AdaptationSet contentType="audio"/>'.repeat(2)}</Period></MPD>`;
  assert.equal(parseMpd(mixed, BASE).adaptationSets.length, 1);
  assert.equal(dashTracks(parseMpd(empty(10 * L.adaptationSets), BASE), BASE).length, L.adaptationSets);
  assert.equal(TEMPLATE_LIMITS.adaptationSets, L.adaptationSets);
});

// A SegmentList built as a tree whose children are one shared element,
// so a list of tens of thousands of addresses costs no XML text.
function listTree(sizes, { shared = null } = {}) {
  const el = (name, attrs = [], children = []) => ({ name, attrs, children });
  const seg = el('SegmentURL', [['media', 's.mp4']]);
  const list = (n) => el('SegmentList', [], [el('Initialization', [['sourceURL', 'init.mp4']]), ...new Array(n).fill(seg)]);
  const repEls = sizes.map((n, i) => el('Representation', [['id', `r${i}`], ['bandwidth', '1']], [el('BaseURL', [], [{ text: `r${i}/` }]), ...(n === null ? [] : [list(n)])]));
  const as = el('AdaptationSet', [['contentType', 'video']], [...(shared === null ? [] : [list(shared)]), ...repEls]);
  return el('MPD', [], [el('BaseURL', [], [{ text: BASE }]), el('Period', [], [as])]);
}

test('limits: segmentUrls, exactly and one more; the budget spans the MPD, shared lists included (SEC23b 中-3)', () => {
  const L = { ...TEMPLATE_LIMITS, segmentUrls: 50000 }; // the value pinned above; checked against the module below
  const urlsOf = (tree) => parseMpdTree(tree, BASE).adaptationSets[0].childUrls.length;
  assert.equal(urlsOf(listTree([L.segmentUrls - 1])), L.segmentUrls); // segments + init
  assert.equal(urlsOf(listTree([L.segmentUrls])), 0); // one more: the list is not read
  // two own lists: the second one no longer fits
  assert.equal(urlsOf(listTree([30000, 30000])), 30001);
  // a shared list against many Representation bases: read until the budget is spent
  const perBase = 2000;
  const sizes = new Array(100).fill(null);
  const n = urlsOf(listTree(sizes, { shared: perBase }));
  assert.equal(n, Math.floor(L.segmentUrls / (perBase + 1)) * (perBase + 1));
  assert.ok(n <= L.segmentUrls);
  assert.equal(TEMPLATE_LIMITS.segmentUrls, L.segmentUrls);
});

// ---------------------------------------------------------------- address size (SEC23c 中-4)

/** A directory URL of exactly n characters, ending in "/". */
const dirOf = (n) => `${BASE}${'p'.repeat(n - BASE.length - 1)}/`;
const bareRep = '<Period><AdaptationSet contentType="video"><Representation id="v" bandwidth="1"><BaseURL>v.mp4</BaseURL></Representation></AdaptationSet></Period>';

test('addresses: the MPD address, a <BaseURL> and each address are used only up to urlLength', () => {
  const urlLength = 4096; // the value pinned above
  const urlsOf = (xml, base) => parseMpd(xml, base).adaptationSets[0].childUrls;
  // the MPD's own address as a base: exactly the limit, and one more
  const mpdAt = (n) => `${dirOf(n - 5)}m.mpd`;
  assert.deepEqual(urlsOf(`<MPD>${bareRep}</MPD>`, mpdAt(urlLength)), [`${dirOf(urlLength - 5)}v.mp4`]);
  assert.deepEqual(urlsOf(`<MPD>${bareRep}</MPD>`, mpdAt(urlLength + 1)), []);
  // a <BaseURL> past the limit is not taken: the parent base stays
  const longBase = `<BaseURL>${dirOf(urlLength + 1)}</BaseURL>`;
  assert.deepEqual(urlsOf(`<MPD>${longBase}${bareRep}</MPD>`, `${BASE}m.mpd`), [`${BASE}v.mp4`]);
  const okBase = `<BaseURL>${dirOf(urlLength - 5)}</BaseURL>`;
  assert.deepEqual(urlsOf(`<MPD>${okBase}${bareRep}</MPD>`, `${BASE}m.mpd`), [`${dirOf(urlLength - 5)}v.mp4`]);
  // SegmentURLs: each address on its own
  const list = `<SegmentList><SegmentURL media="${dirOf(urlLength - 5)}a.mp4"/><SegmentURL media="${dirOf(urlLength - 4)}b.mp4"/><SegmentURL media="c.mp4"/></SegmentList>`;
  const xml = `<MPD><Period><AdaptationSet contentType="video">${list}<Representation id="v" bandwidth="1"/></AdaptationSet></Period></MPD>`;
  assert.deepEqual(urlsOf(xml, `${BASE}m.mpd`), [`${dirOf(urlLength - 5)}a.mp4`, `${BASE}c.mp4`]);
  assert.equal(TEMPLATE_LIMITS.urlLength, urlLength);
});

test('addresses: one MPD gives at most urlBytes characters of addresses, exactly and one more', () => {
  const urlBytes = 16 * 1024 * 1024; // the value pinned above
  const urlLength = 4096;
  // every address is exactly urlLength characters: a 4091-character base and "s.mp4"
  const repBase = dirOf(urlLength - 5);
  const el = (name, attrs = [], children = []) => ({ name, attrs, children });
  const seg = el('SegmentURL', [['media', 's.mp4']]);
  const tree = (n) => el('MPD', [], [el('BaseURL', [], [{ text: repBase }]), el('Period', [], [el('AdaptationSet', [['contentType', 'video']], [
    el('Representation', [['id', 'v'], ['bandwidth', '1']], [el('SegmentList', [], new Array(n).fill(seg))]),
  ])])]);
  const fit = urlBytes / urlLength;
  const at = parseMpdTree(tree(fit), `${BASE}m.mpd`).adaptationSets[0].childUrls;
  assert.equal(at.length, fit);
  assert.equal(at[0], `${repBase}s.mp4`);
  assert.equal(at[0].length, urlLength);
  // one more: the list is not read at all
  assert.equal(parseMpdTree(tree(fit + 1), `${BASE}m.mpd`).adaptationSets[0].childUrls.length, 0);
  assert.equal(TEMPLATE_LIMITS.urlBytes, urlBytes);
});
