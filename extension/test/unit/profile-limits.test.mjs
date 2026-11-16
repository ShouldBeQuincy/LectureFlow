// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What a profile (above all a locally imported one) may not do (security
// review L7): wildcards over a whole registry or shared host, regular
// expressions that backtrack badly or run on long inputs, a sessionInfo
// request anywhere but its declared same-origin path, and quietly taking
// over a packaged profile's id.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { checkProfile, wideWildcard, regexComplexity, profileRegex, MAX_REGEX_INPUT, profilePermissionOrigins } from '../../src/lib/profile.js';
import { captureGroupCount } from '../../src/lib/regexcost.js';
import { sessionInfoRequest, sessionInfoSpec, inpageConfig, sessionResult } from '../../src/lib/inpage.js';
import { localEntry, activeLocalEntries, confirmedReplacement, withLocalEntry } from '../../src/lib/localprofiles.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const schema = JSON.parse(fs.readFileSync(path.join(root, 'schema', 'profile.schema.json'), 'utf8'));
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
const clone = (o) => JSON.parse(JSON.stringify(o));
const errorsOf = (mutate) => {
  const p = clone(panopto);
  mutate(p);
  return checkProfile(schema, p);
};

test('wildcards over a country registry or a shared host are refused; the platform’s own domain is fine', () => {
  for (const wide of ['*.co.uk', '*.ac.uk', '*.com.au', '*.edu.au', '*.edu.cn', '*.ac.jp', '*.gov.in', '*.github.io', '*.cloudfront.net', '*.amazonaws.com', '*.s3.amazonaws.com', '*.s3.eu-west-1.amazonaws.com', '*.s3-eu-west-1.amazonaws.com', '*.azurewebsites.net', '*.blob.core.windows.net', '*.akamaized.net', '*.netlify.app']) {
    assert.notEqual(wideWildcard(wide), '', wide);
  }
  for (const fine of ['*.panopto.com', '*.cloud.panopto.eu', '*.ox.ac.uk', '*.unimelb.edu.au', 'd1234abcd.cloudfront.net', '*.d1234abcd.cloudfront.net', 'video.example.co.uk', '*.kaltura.com', '*.mediasite.example.edu', 'github.io']) {
    assert.equal(wideWildcard(fine), '', fine);
  }
  assert.ok(errorsOf((p) => p.match.hosts.push('*.co.uk')).some((e) => e.path === `/match/hosts/${panopto.match.hosts.length}` && /^"\*\.co\.uk" covers a whole public suffix/.test(e.message)));
  assert.ok(errorsOf((p) => (p.match.mediaHosts = ['*.cloudfront.net'])).some((e) => e.path === '/match/mediaHosts/0' && /^"\*\.cloudfront\.net" is a shared service \(cloudfront\.net\)/.test(e.message)), 'the words of scripts/profilecheck');
  // one label after "*." the schema refuses already
  assert.ok(errorsOf((p) => p.match.hosts.push('*.com')).length > 0);
});

test('regular expressions: no backreferences, no repeated group of variable width or with an alternation, at most two unbounded quantifiers', () => {
  // what the packaged profile uses passes
  for (const src of ['^/Panopto/Pages/(Viewer|Embed)\\.aspx$', '/sessions/[0-9a-f-]{36}/[0-9a-f-]{36}(-[0-9a-f-]{36})?\\.hls/master\\.m3u8$', '^(?!Panopto Viewer$)(.+)$', '([A-Z][a-z]{2} \\d{1,2} \\d{1,2}:\\d{2}(?::\\d{2})? [A-Z]{2,5} \\d{4})\\s*$', '^(?<id>[0-9a-f]{8})$', '[(+*)]+x']) {
    assert.equal(regexComplexity(src), '', src);
  }
  for (const [src, why] of [
    ['(a+)+$', /catastrophic/],
    ['^(\\w+\\s?)*$', /catastrophic/],
    ['(a|aa)*b', /catastrophic/],
    ['(?:x[a-z]*){2,}', /catastrophic/],
    ['((ab)*c)+', /catastrophic/],
    ['(.*a){3}', /catastrophic/],
    ['(a)\\1', /backreferences/],
    ['(?<x>a)\\k<x>', /backreferences/],
    ['.*a.*b.*c.*d', /too many/],
  ]) {
    assert.match(regexComplexity(src), why, src);
  }
  // in a profile, at every regex field
  assert.ok(errorsOf((p) => (p.tracks[1].detect.urlPattern = '(a+)+$')).some((e) => e.path === '/tracks/1/detect/urlPattern' && /catastrophic/.test(e.message)));
  assert.ok(errorsOf((p) => (p.match.pathPattern = '^(/[a-z]+)*$')).some((e) => e.path === '/match/pathPattern'));
  assert.ok(errorsOf((p) => (p.lecture.title[3].pattern = '(.+)+x')).some((e) => e.path === '/lecture/title/3/pattern'));
  assert.ok(errorsOf((p) => (p.inpage.list.idPattern = '^((a|b)+)+$')).some((e) => e.path === '/inpage/list/idPattern'));
  assert.ok(errorsOf((p) => (p.observe[0].urlPattern = 'x'.repeat(301))).some((e) => e.path === '/observe/0/urlPattern'), 'longer than the schema allows');
});

test('a profile regex never runs on more than MAX_REGEX_INPUT characters', () => {
  const re = profileRegex('^/Panopto/.*$');
  assert.equal(re.test('/Panopto/x'), true);
  assert.equal(re.test(`/Panopto/${'x'.repeat(MAX_REGEX_INPUT)}`), false);
  assert.equal(re.exec(`/Panopto/${'x'.repeat(MAX_REGEX_INPUT)}`), null);
  assert.deepEqual([...profileRegex('^/v/(\\d+)$').exec('/v/42')], ['/v/42', '42']);
});

test('sessionInfo: exactly the declared path on the frame’s own origin, on a profile host, matching its observe pattern', () => {
  const spec = sessionInfoSpec(panopto, { reviewed: true });
  assert.equal(spec.urlPattern, panopto.observe[0].urlPattern);
  assert.deepEqual(spec.hosts, panopto.match.hosts);
  const ID = 'a0000000-0000-4000-8000-0000000000a1';
  const LIST = 'https://tenant.cloud.panopto.eu/Panopto/Pages/Sessions/List.aspx';
  assert.equal(sessionInfoRequest(spec, ID, LIST).url, 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer/DeliveryInfo.aspx');
  // a frame on another host (the profile's list path served elsewhere)
  assert.equal(sessionInfoRequest(spec, ID, 'https://lms.example.edu/Panopto/Pages/Sessions/List.aspx'), null);
  // a path that does not match the observe pattern, dot segments, encoded tricks, a query
  for (const p of ['/Panopto/Pages/Viewer/Other.aspx', '/Panopto/../Pages/Viewer/DeliveryInfo.aspx', '/./Panopto/Pages/Viewer/DeliveryInfo.aspx', '/Panopto/Pages/Viewer/DeliveryInfo.aspx?x=1', '//evil.example.com/Panopto/Pages/Viewer/DeliveryInfo.aspx', '/Panopto/Pages/Viewer/DeliveryInfo.aspx#f', '/a b/DeliveryInfo.aspx', '/%2e%2e/Panopto/Pages/Viewer/DeliveryInfo.aspx', 'Panopto/Pages/Viewer/DeliveryInfo.aspx']) {
    assert.equal(sessionInfoRequest({ ...spec, path: p }, ID, LIST), null, p);
  }
  // only GET or POST
  assert.equal(sessionInfoRequest({ ...spec, method: 'PUT' }, ID, LIST), null);
  // the schema says the same about the path
  const errs = (p) => checkProfile(schema, { ...clone(panopto), sessionInfo: { ...panopto.sessionInfo, path: p } });
  assert.deepEqual(errs('/Panopto/Pages/Viewer/DeliveryInfo.aspx'), []);
  for (const p of ['/Panopto/../x/DeliveryInfo.aspx', '/Panopto/Pages/Viewer/DeliveryInfo.aspx?x=1', '//evil.example.com/x', '/a b', '/x\\y']) assert.ok(errs(p).length > 0, p);
});

test('a local file with a packaged profile’s id is used only once the user confirmed it replaces it', () => {
  const mine = { ...clone(panopto), version: '1.2.0-dev' };
  const other = { ...clone(panopto), id: 'echo360', name: 'Echo360' };
  const unconfirmed = localEntry(mine, { now: new Date('2026-10-27T01:00:00Z') });
  assert.deepEqual(Object.keys(unconfirmed).sort(), ['importedAt', 'profile']);
  assert.equal(unconfirmed.importedAt, '2026-10-27');
  assert.equal(confirmedReplacement(unconfirmed), false);
  const confirmed = localEntry(mine, { replaces: '1.1.1' });
  assert.equal(confirmed.replaces, '1.1.1');
  assert.equal(confirmedReplacement(confirmed), true);
  const fresh = localEntry(other);
  // with a packaged "panopto": the unconfirmed one is not in force, the confirmed one is; other ids always are
  assert.deepEqual(activeLocalEntries([unconfirmed, fresh], ['panopto']).map((e) => e.profile.id), ['echo360']);
  assert.deepEqual(activeLocalEntries([confirmed, fresh], ['panopto']).map((e) => e.profile.id), ['panopto', 'echo360']);
  assert.deepEqual(activeLocalEntries([unconfirmed], []).length, 1, 'no packaged profile of that id: nothing to confirm');
  assert.deepEqual(activeLocalEntries([null, {}, { profile: 'x' }, 'x'], ['panopto']), []);
  assert.deepEqual(activeLocalEntries(undefined, ['panopto']), []);
  // importing again replaces the earlier entry of that id
  assert.deepEqual(withLocalEntry([unconfirmed, fresh], confirmed).map((e) => [e.profile.id, !!e.replaces]), [['echo360', false], ['panopto', true]]);
});

test('profile site access: page hosts, and media hosts only when asked for', () => {
  const p = { match: { hosts: ['*.video.example.edu'], mediaHosts: ['media.example.net'] } };
  assert.deepEqual(profilePermissionOrigins(p), ['https://*.video.example.edu/*', 'https://media.example.net/*']);
  assert.deepEqual(profilePermissionOrigins(p, { media: false }), ['https://*.video.example.edu/*']);
});

// The largest allowed member of each family tried against the cost model
// (SEC18c): [family, pattern, the same with one more repetition]. The
// allowed ones were each timed once on 1024 characters while the rule was
// written (at most 0.86 s); the next one up is refused. Warmed up as the
// extension does (regexguard.js warmRegex), 999 characters cost the same
// (SEC22 L-A: "x.*.{511}y" 0.42 s, ".?.?.?.?x.*.{31}y" 0.55 s, ".*.{512}.*y"
// 0.44 s, "^.*x.*.{383}y" 0.38 s, ".?" x 12 ".{50}y" 0.53 s); a first,
// unwarmed match on 999 characters would be 5 to 8 times slower.
const rep = (s, n) => s.repeat(n);
// refused for the cost of matching: the choices, or the lookarounds' width
const COSTLY = /ways to match|lookaheads and lookbehinds may hold/;
// SEC22c R-1, with its rewrite
const ZERO_WIDTH = /two alternatives that take no character .* in more than one group, or three in one group.*To fix it, put such a choice inside one lookahead, not "\(\?:\(\?=\/\)\|\$\)" but "\(\?=\/\|\$\)"/;
const SEC18E_BRANCH = '(?:x|[xb]|[xc]|[xd]|[xe]|[xf]|[xg]|[xh])';
const SEC18D_GROUP = (look) => `(?:${['x', '[xb]', '[xc]', '[xd]', '[xe]', '[xf]', '[xg]', '[xh]'].map((c) => c + look).join('|')})`;
const BOUNDARY = [
  ['fixed-width group between choices', `${rep('.?(?:.{8}){2}', 13)}x`, `${rep('.?(?:.{8}){2}', 14)}x`],
  ['required content before the choice', `${rep('.{20}.?', 13)}x`, `${rep('.{20}.?', 14)}x`],
  ['nested groups, required inside', `${rep('(?:.?(?:.{10}))', 14)}x`, `${rep('(?:.?(?:.{10}))', 15)}x`],
  ['branches of unequal width', `${rep('(?:a|.{20})', 12)}x`, `${rep('(?:a|.{20})', 13)}x`],
  ['overlapping branches, then required', `${rep('(?:a|[ab]).{30}', 12)}x`, `${rep('(?:a|[ab]).{30}', 13)}x`],
  ['anchored, with width', `^${rep('.?.{8}', 22)}x`, `^${rep('.?.{8}', 23)}x`],
  ['one wide quantifier, with width', `.*x${rep('.?.{8}', 5)}y`, `.*x${rep('.?.{8}', 6)}y`],
  ['anchored, one wide quantifier, with width', `^.*x${rep('.?.{8}', 13)}y`, `^.*x${rep('.?.{8}', 14)}y`],
  ['anchored, two wide quantifiers, with width', `^.*x${rep('.?.{8}', 5)}.*y`, `^.*x${rep('.?.{8}', 6)}.*y`],
  ['lookbehind width', `${rep('.?(?<=.{4})', 15)}x`, `${rep('.?(?<=.{4})', 16)}x`],
  ['a lookbehind of 64 after a wide quantifier', `${rep('.?', 3)}x.*(?<=.{64})${SEC18E_BRANCH}.?.?z`, `${rep('.?', 4)}x.*(?<=.{64})${SEC18E_BRANCH}.?.?z`],
  ['anchored lookahead width', `^${rep('.?(?=.{6})', 10)}x`, `^${rep('.?(?=.{6})', 11)}x`],
  ['{n,m} then required', `${rep('.{0,3}.{20}', 6)}x`, `${rep('.{0,3}.{20}', 7)}x`],
  ['many medium segments', `${rep('.?.{3}', 15)}x`, `${rep('.?.{3}', 16)}x`],
  ['exact repeat in a segment', `${rep('.?(?:.{10}){5}', 12)}x`, `${rep('.?(?:.{10}){5}', 13)}x`],
  ['choices, then a long tail', `${rep('.?', 12)}.{50}x`, `${rep('.?', 13)}.{50}x`],
  // SEC18f 低-17: what a wide quantifier requires after it (0.53 s at most)
  ['required after a wide quantifier', 'x.*.{511}y', 'x.*.{512}y'],
  ['choices, then required after a wide quantifier', '.?.?.?.?x.*.{31}y', '.?.?.?.?x.*.{32}y'],
  ['required between two wide quantifiers', '.*.{512}.*y', '.*.{513}.*y'],
  ['anchored, required after the second wide quantifier', '^.*x.*.{383}y', '^.*x.*.{384}y'],
];

// FB1 L-1. Only regexComplexity runs on these: none is ever matched here.
test('regular expressions: the audit probes are refused (quantifier in a repeated group, a class hiding a group, adjacent and wide quantifiers)', () => {
  for (const [src, why] of [
    // a bounded quantifier inside a repeated group (minutes on 200 "a")
    ['(?:a{1,30}){1,30}x', /catastrophic/],
    ['(a?){24}a{24}$', /catastrophic/],
    ['(?:a?){3}', /catastrophic/],
    ['(?:a|b){3}', /catastrophic/],
    ['(?:a{1,2}){3}', /catastrophic/],
    ['(?:a{0}){3}', /catastrophic/],
    ['(?:a(?=b)){3}', /catastrophic/],
    ['(?:^a){2}', /catastrophic/],
    ['(?:\\ba){2}', /catastrophic/],
    ['(?:){2}', /catastrophic/],
    ['(?:(?:a?)b){2}', /catastrophic/],
    // "[^]" is a whole class in JavaScript: the group after it is read
    ['[^](a+)+b', /catastrophic/],
    ['[](a+)+b', /catastrophic/],
    // unbounded quantifiers in a row
    ['\\d*\\d*\\d*x', /in a row/],
    ['\\d*\\d*x', /in a row/],
    ['\\w+\\s?\\w+', /in a row/],
    ['(\\d+)\\d+', /in a row/],
    ['\\d+(\\d+)', /in a row/],
    ['\\d+(?:x)?\\d+', /in a row/],
    ['(?:a|\\d+)\\d+', /in a row/],
    ['.*(?=.*x)', /in a row/],
    // a wide bounded quantifier is an unbounded one
    ['.{0,2000}.{0,2000}.{0,2000}x', /in a row/],
    ['.{0,2000}x.{0,2000}y.{0,2000}z', /too many/],
    // three, even apart
    ['.*x.*x.*y', /too many/],
    // exponential without any "*" or "+"
    [`${'(?:a|a)'.repeat(26)}x`, /ways to match/],
    [`${'a?'.repeat(30)}x`, /ways to match/],
    // SEC18 中-1: allowed by the first version, and slow (seconds to minutes)
    [`${'.?'.repeat(25)}x`, /ways to match/],
    [`${'.{0,16}'.repeat(6)}x`, /ways to match/],
    [`${'(?:a|a|a|a)'.repeat(12)}x`, /ways to match/],
    [`${'.?'.repeat(19)}x`, /ways to match/],
    [`.*x${'.?'.repeat(14)}y`, /ways to match/],
    ['.*x.?.?.?.*y', /ways to match/],
    ['.{0,7}.*x.*y', /ways to match/],
    [`.*x${'.?'.repeat(10)}y`, /ways to match/],
    [`^.*x${'.?'.repeat(18)}y`, /ways to match/],
    [`^.*x${'.?'.repeat(9)}.*y`, /ways to match/],
    // SEC18b 中-3: wide choice units (allowed by 83c66ad, 1.8 to 19.2 s on 1024 characters)
    [`${'(?:.{14})?'.repeat(18)}x`, /ways to match/],
    [`.*x${'(?:.{30})?'.repeat(9)}y`, /ways to match/],
    [`^.*x${'(?:.{20})?'.repeat(17)}y`, /ways to match/],
    [`^${'(?:.{9})?'.repeat(26)}x`, /ways to match/],
    [`${'(?:.{8}){0,16}'.repeat(4)}.?x`, /ways to match/],
    [`${'(?:(?:.{6}){5})?'.repeat(18)}x`, /ways to match/],
    [`${'(?:.{8})?'.repeat(16)}x`, /ways to match/],
    [`.*x${'(?:.{8})?'.repeat(7)}y`, /ways to match/],
    // SEC18c 中-4: width after the choice, or in a lookaround (all over 13 s on 1024 characters)
    [`${'.?.{28}'.repeat(18)}x`, /ways to match/],
    [`${'.?(?=.{100})'.repeat(18)}x`, COSTLY],
    [`.*x${'.?.{60}'.repeat(9)}y`, /ways to match/],
    [`^${'.?.{30}'.repeat(26)}x`, /ways to match/],
    [`${'(?:.(?=.{100}))?'.repeat(18)}x`, COSTLY],
    // SEC18d: lookarounds inside a choice group right after a wide quantifier (14.9 s and 18.2 s)
    [`.?.?.?x.*${SEC18D_GROUP('(?<=.{300})')}.?.?z`, COSTLY],
    [`${SEC18D_GROUP('(?=.{300})')}.*${SEC18D_GROUP('(?=.{300})')}.?.?z`, COSTLY],
    // SEC18e 低-14: lookarounds after a wide quantifier, before any choice (2.5 s, 4.8 s, 11.7 s)
    [`.?.?.?x.*(?<=.{300})${SEC18E_BRANCH}.?.?z`, /lookaheads and lookbehinds may hold at most 64/],
    [`.?.?.?x.*(?<=(?<=.{300}).{300})${SEC18E_BRANCH}.?.?z`, /lookaheads and lookbehinds may hold at most 64/],
    [`.?.?.?x.*${'(?<=.{999})'.repeat(18)}${SEC18E_BRANCH}.?.?z`, /lookaheads and lookbehinds may hold at most 64/],
    [`.?.?.?x.*(?<=.{65})${SEC18E_BRANCH}.?.?z`, /lookaheads and lookbehinds may hold at most 64/],
    ['(?<=a.*)x', /a lookbehind may not hold/],
    // SEC22 L-B: empty alternatives stack choices past the check (1.5 s with "(?:|)")
    [`x.*${'(?:|).{7}'.repeat(6)}y`, /an empty alternative/],
    ['(?:a|)x', /an empty alternative/],
    ['(?:|a)x', /an empty alternative/],
    ['(?:a||b)x', /an empty alternative/],
    ['a|', /an empty alternative/],
    ['|a', /an empty alternative/],
    // SEC22c R-1: alternatives that take no character, in more than one group
    // or more than two in one group. "^" + "(?:^|$)" x 26: 4.6 s on "" and
    // 3.5 s on "/v/1" (the first runs, each compiling it), 7.4 s through the
    // guard; the stacks below 1.25 s and 1.4 s on 1024 characters, warmed up,
    // past the empty-alternative rule (SEC22b S-2)
    [`^${'(?:^|$)'.repeat(26)}\\b\\B`, ZERO_WIDTH],
    [`x.*${'(?:(?=)|(?=)).{7}'.repeat(6)}y`, ZERO_WIDTH],
    [`x.*${'(?:a{0}|b{0}).{7}'.repeat(6)}y`, ZERO_WIDTH],
    [`x.*${'(?:(?:)|(?:)).{7}'.repeat(6)}y`, ZERO_WIDTH],
    ['^(?:^|a{0}$)(?:^|a{0}$)\\b\\B', ZERO_WIDTH],
    ['(?:^|$)(?:^|$)', ZERO_WIDTH],
    ['(?:^|$|\\b)', ZERO_WIDTH],
    ['(?:\\b|\\B)x(?:(?=a)|(?!b))', ZERO_WIDTH],
    ['(?:(?:^|$)|x)(?:$|(?=/))', ZERO_WIDTH], // nested: the inner group counts
    ['(?<id>^|$)(?:(?<=a)|(?<!b))', ZERO_WIDTH], // capture and named groups too
    ['(?=(?:^|$)(?:^|$)\\b\\B)', ZERO_WIDTH], // groups inside a lookaround too
    ['.*x(?:(?=y)|$)(?:(?=y)|$)y', ZERO_WIDTH],
    // the empty-alternative rule comes first where both apply
    ['(?:|(?=))(?:|(?=))', /an empty alternative/],
    ['(?:(?=)|)(?:(?=)|)', /an empty alternative/],
    // SEC22 I-1: one character written as an escape is one atom
    ['.*\\x41*z', /in a row/],
    ['\\d+\\u0030*z', /in a row/],
    ['.*\\cJ*z', /in a row/],
    // SEC18f 低-17: required content after a wide quantifier, before any choice (3.1 s, over 20 s, 7.4 s, 1.9 s)
    [`.?.?.?x.*.{341}${SEC18E_BRANCH}.?.?z`, /after the first "\*", "\+" or wide "\{n,m\}", with the choices before it, what it requires next may weigh at most 64 .*, here 341 .*To fix it, start the regex with "\^"/],
    ['.?.?.?.?.?.?x.*.{300}y', /after the first .*at most 8 .*here 301 /],
    ['.*x.*.{20}y', /after the second .*at most 1 .*here 21 .*leave out a leading "\.\*"/],
    ['.*x.*.{4}y', /after the second/],
    // SEC22 L-C: text written out that repeats itself counts (3.1 s on 999 characters, warmed up)
    [`.*x.*${'xX'.repeat(120)}[^x]`, /after the second .*here 240/],
    ['.*x.*....y', /after the second/],
    ['^.*x.*.{20}y.*', /too many "\*"/],
    // a lookahead, a capture group, required content before a choice group: all in the segment
    ['.?.?.?x.*(?=.{64})y', /after the first/],
    ['.?.?.?.?.?.?x.*(.{300})y', /after the first/],
    ['.?.?.?.?.?.?x.*(.{300}(?:a|b))y', /after the first/],
    // one repetition past each boundary
    ...BOUNDARY.map(([, , src]) => [src, COSTLY]),
    // single characters, now with the closing "x" / "y" counted in the last segment
    [`${'.?'.repeat(18)}x`, /ways to match/],
    [`.*x${'.?'.repeat(9)}y`, /ways to match/],
    // SEC18 低-1: modifier groups (and anything else after "(?") are not read
    ['a*(?i:)a*x', /unsupported group syntax/],
    ['\\d*(?i:\\d*)x', /unsupported group syntax/],
    ['a*(?-i:)a*x', /unsupported group syntax/],
    ['a*(?s:)a*y', /unsupported group syntax/],
    ['(?<1x>a)', /unsupported group syntax/],
    ['(?P<x>a)', /unsupported group syntax/],
  ]) {
    assert.match(regexComplexity(src), why, src);
  }
  // still fine: what the packaged profile and the contribution sample use, and
  // ordinary shapes a contributor writes
  const packaged = [];
  const collect = (v) => {
    if (Array.isArray(v)) v.forEach(collect);
    else if (v && typeof v === 'object') for (const [k, x] of Object.entries(v)) (/^(pattern|pathPattern|urlPattern|idPattern)$/.test(k) && typeof x === 'string' ? packaged.push(x) : collect(x));
  };
  collect(panopto);
  collect(JSON.parse(fs.readFileSync(path.join(root, '..', 'scripts', 'profilecheck', 'testdata', 'profiles', 'demo.json'), 'utf8')));
  assert.ok(packaged.length >= 10, String(packaged.length));
  for (const src of [
    ...packaged, '^/v/[^/]+/s/[^/]+(?:/embed)?$', '^/v/(\\d+)/(\\d+)$', '\\d+x\\d+y', '[]a+]', '(?:mp4|m4a|webm|mp3|aac|ogg|wav|flac)$', '^/watch\\?v=([A-Za-z0-9_-]{11})$',
    // fixed-width repeated groups (SEC18 低-2): one way to take each repetition
    '(?:[0-9a-f]{4}-){3}', '[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}', '(?:\\d{2}:){2}\\d{2}', '(?:[0-9a-f]{2}){16}', '^/v/(?:[a-z]{2}\\d{2}/){1,3}([^/]+)$','(?:(?:ab){2}c)+x',
    // at the budget's edges (measured on 1024 characters: 0.86 s at worst, see BOUNDARY below)
    ...BOUNDARY.map(([, src]) => src),
    `${'.?'.repeat(17)}x`, `.*x${'.?'.repeat(8)}y`, '.*x.*y', `^.*x${'.?'.repeat(8)}.*y`, `${'.{0,16}'.repeat(3)}x`,
    // lookarounds and named groups
    '^(?!Panopto Viewer$)(.+)$', '(?<=/)(?<id>[0-9a-f]{8})(?=/|$)', '(?:/|$)', '\\x41+\\u0042',
    // SEC22 L-C: common and cheap (0 to 0.26 s on adversarial input, first match unwarmed)
    '.*/a/.*\\.mp4', '/hls/.*/.*\\.m3u8$', '/v/[^/]+/[^/]+\\.m3u8', '\\d+_\\d+\\.ts', '/seg-\\d+-\\d+\\.m4s$', '/lecture/\\d+/.*\\.mp4$', '/api/.+/sessions/.+/delivery$', '[a-f0-9]+-[a-f0-9]+\\.m3u8',
    '/sessions/.*/.*-audio\\.m4a$', '([^/]+)/([^/]+)\\.m3u8', '/s\\d+q\\d+\\.m3u8', '/media/.*\\.mp4$', '/video/[^/]+/[^/]+/index\\.m3u8$', '/content/.+/.+\\.mpd$', '/(\\d+)/(\\d+)/playlist\\.m3u8', '/download/.*/.*\\.mp4\\?', '^/watch/([^/]+)/.*$', '/hls/[^/]+/.*/index\\.m3u8',
    '.*x.*.y', `^.*x.*${'xX'.repeat(120)}[^x]`,
    // SEC22c R-1 leaves alone: one alternative that takes no character per
    // group, in as many groups as the choices allow ("^" + "(?:/|$)" x 22: 0.5 ms on "");
    // a lookaround's own alternatives; one group with two (0.6 s at most on
    // 1024 characters, warmed up, as wide as the other rules allow). These
    // take 0 to 0.4 ms on 1000 adversarial characters, first match, not warmed up.
    '^https?://[^/]+/v/(\\d+)(?:/|$)', '^/v/([^/]+)/?$', '^/?watch/(\\d+)', '^/video/(\\d+)(?:\\?.*)?$', '/play/([A-Za-z0-9_-]+)(?:/|\\?|#|$)',
    '(?:^|/)master\\.m3u8$', '(?:^|/)index(?:_\\d+)?\\.m3u8', '/(?:audio|video)?/?seg\\d+\\.ts', '/media/(?:hls|dash)?/.+\\.(?:m3u8|mpd)$',
    '/v/(\\d+)(?=/|$)', '/v/(\\d+)(?=[/?#]|$)', '/sessions/([0-9a-f-]{36})(?=/|\\?|$)', '(?<=^|/)lecture-(\\d+)', '(?<=/)(\\d+)(?=/|$)',
    '/stream(?:s)?/([^/]+)(?:/playlist\\.m3u8)?$', '(?:\\.mp4|\\.m4a)(?:\\?.*)?$', '^/(?:[a-z]{2}/)?courses?/(\\d+)/?(?:\\?.*)?$',
    '(?:\\?|$)', '(?:[/?#]|$)', '^(?:/[a-z]+)?/embed/([^/?#]+)(?:[/?#]|$)', '\\b(\\d{6,})\\b', '^/(?:e|embed)/(?:\\?|$)', '^/a/(?:b|c)?/?(\\d+)(?:/|$)',
    '(?=\\b|$)', '(?<=^|\\b)x', '(?!^|$)x', `^${'(?:/|$)'.repeat(22)}\\b\\B`, '(?:a|b)?(?:c|$)(?:^|d)',
    '/v/\\d+(?:\\b|$)', '^/v/(\\d+)(?:(?=/)|$)', '/a(?:$|(?=\\?))', '(?:^|(?<=/))v(\\d+)', '(?:^|$)', '^|$', '(?:(?:)|x)y',
    'x.*(?:(?=)|(?=)).{255}y', `^(?:^|$)${'(?:a?|b?)'.repeat(8)}\\b\\B`,
    // the two exemptions (SEC27 L-1): a lookaround's own alternatives, and
    // the top level's, next to the one group allowed
    '(?<=^|\\b)x(?=\\b|$)(?=^|$)', '(?:(?=/)|$)a|^|$',
  ]) {
    assert.equal(regexComplexity(src), '', src);
  }
  // the packaged profile passes as a whole, in the import and in the contribution check
  assert.deepEqual(checkProfile(schema, panopto), []);
});

test('an unreviewed profile requests sessionInfo only with GET (FB1 L-2)', () => {
  // the import and the loading of local profiles: a validation error
  const post = checkProfile(schema, clone(panopto), { unreviewed: true });
  assert.deepEqual(post.map((e) => e.path), ['/sessionInfo/method']);
  assert.match(post[0].message, /only with GET, not POST/);
  const get = clone(panopto);
  get.sessionInfo.method = 'GET';
  assert.deepEqual(checkProfile(schema, get, { unreviewed: true }), []);
  // the packaged, reviewed profile keeps its POST
  assert.deepEqual(checkProfile(schema, clone(panopto)), []);
  // and at request time, whatever got stored
  const ID = 'a0000000-0000-4000-8000-0000000000a1';
  const LIST = 'https://tenant.cloud.panopto.eu/Panopto/Pages/Sessions/List.aspx';
  assert.equal(sessionInfoRequest(sessionInfoSpec(panopto), ID, LIST), null, 'not said to be reviewed: no POST');
  assert.equal(sessionInfoRequest(sessionInfoSpec(panopto, { reviewed: false }), ID, LIST), null);
  assert.equal(sessionInfoRequest(sessionInfoSpec(panopto, { reviewed: true }), ID, LIST).method, 'POST');
  const viaGet = sessionInfoRequest(sessionInfoSpec(get, { reviewed: false }), ID, LIST);
  assert.equal(viaGet.method, 'GET');
  assert.equal(viaGet.body, null);
  assert.equal(new URL(viaGet.url).searchParams.get('deliveryId'), ID);
  // the content script's template and the background's check follow `reviewed`
  assert.equal(sessionInfoRequest(inpageConfig({ profile: panopto, reviewed: false, mode: 'list' }).sessionInfo, ID, LIST), null);
  assert.equal(sessionInfoRequest(inpageConfig({ profile: panopto, reviewed: true, mode: 'list' }).sessionInfo, ID, LIST).method, 'POST');
  assert.equal(sessionResult({ found: { profile: panopto, reviewed: false }, sessionId: ID, values: {}, frameUrl: LIST }), null);
});

// SEC18 中-2: checkProfile never runs a regex that fails the static check.
// On the first version these took 10 s and more than 20 s.
test('a profile with a costly regex is refused within milliseconds; the regex is never run', () => {
  const timed = (p, opts) => {
    const t = performance.now();
    const errors = checkProfile(schema, p, opts);
    return { errors, ms: performance.now() - t };
  };
  // an observe pattern the sessionInfo path would be tested against
  const a = clone(panopto);
  a.observe[0].urlPattern = '(a+)+$';
  a.sessionInfo = { ...a.sessionInfo, method: 'GET', path: `/${'a'.repeat(40)}!` };
  delete a.inpage;
  const ra = timed(a, { unreviewed: true });
  assert.ok(ra.errors.some((e) => e.path === '/observe/0/urlPattern' && /catastrophic/.test(e.message)), JSON.stringify(ra.errors));
  assert.ok(ra.ms < 500, `${ra.ms} ms`);
  // a pathPattern on a host with a blocked path (tested against sample paths)
  const b = clone(panopto);
  b.match = { hosts: ['www.linkedin.com'], pathPattern: '^/(?:[a-z0-9/]?[a-z0-9/]?)*!$' };
  const rb = timed(b);
  assert.ok(rb.errors.some((e) => e.path === '/match/pathPattern' && e.blocked), JSON.stringify(rb.errors));
  assert.ok(rb.ms < 500, `${rb.ms} ms`);
  const c = clone(panopto);
  c.match = { hosts: ['www.linkedin.com'], pathPattern: '^/feed/' };
  c.inpage.list.pathPattern = '^/(?:[a-z0-9/]?[a-z0-9/]?)*!$';
  const rc = timed(c);
  assert.ok(rc.errors.some((e) => e.path === '/inpage/list/pathPattern'), JSON.stringify(rc.errors));
  assert.ok(rc.ms < 500, `${rc.ms} ms`);
});

test('nothing in a profile check runs a regex the static check refuses, nor one that is slow only on "" (SEC22b M-2)', () => {
  // M-2: on a host with a blocked path, the pathPattern used to be compiled
  // and run before its static check ("(?:|)" x 24: 3.1 s, x 59 endless)
  const SLOW = `${'(?:|)'.repeat(20)}\\b\\B`;
  // and the capture groups of idFrom.pathPattern used to be counted by
  // running it on "" (a regex slow only there: "^" + "(?:^|$)" x 26 takes
  // 9 s; 22 here, so that a run on the old code stays short). The static
  // check refuses it now (SEC22c R-1); one it lets through, with a single
  // such group, must not run either.
  const ONLY_EMPTY = `^${'(?:^|$)'.repeat(22)}(\\d)\\b\\B`;
  assert.match(regexComplexity(ONLY_EMPTY), /alternatives that take no character/);
  const PASSES = '^/v/(\\d+)(?:(?=/)|$)';
  assert.equal(regexComplexity(PASSES), '', 'the static check lets it through');
  // compared as RegExp.prototype.source writes them ("/" as "\/"; SEC27 M-1)
  const watched = (src) => [SLOW, ONLY_EMPTY, PASSES].some((p) => src.startsWith(new RegExp(p).source));
  const ran = [];
  const original = { test: RegExp.prototype.test, exec: RegExp.prototype.exec };
  RegExp.prototype.test = function (x) {
    if (watched(this.source)) ran.push(['test', this.source, x]);
    return original.test.call(this, x);
  };
  RegExp.prototype.exec = function (x) {
    if (watched(this.source)) ran.push(['exec', this.source, x]);
    return original.exec.call(this, x);
  };
  try {
    const b = clone(panopto);
    b.match = { hosts: ['www.linkedin.com'], pathPattern: SLOW };
    const rb = checkProfile(schema, b);
    assert.ok(rb.some((e) => e.path === '/match/pathPattern' && e.blocked), JSON.stringify(rb));
    assert.ok(rb.some((e) => e.path === '/match/pathPattern' && /empty alternative/.test(e.message)), JSON.stringify(rb));
    const c = clone(panopto);
    c.session = { ...c.session, idFrom: { pathPattern: ONLY_EMPTY } };
    const rz = checkProfile(schema, c);
    assert.ok(rz.some((e) => e.path === '/session/idFrom/pathPattern' && /alternatives that take no character/.test(e.message)), JSON.stringify(rz));
    c.session.idFrom.pathPattern = PASSES;
    const rc = checkProfile(schema, c);
    assert.ok(!rc.some((e) => e.path === '/session/idFrom/pathPattern'), JSON.stringify(rc));
    c.session.idFrom.pathPattern = '^/v/(\\d+)/(\\d+)$';
    assert.ok(checkProfile(schema, c).some((e) => e.path === '/session/idFrom/pathPattern' && /exactly one capture group/.test(e.message)), 'two groups');
    assert.deepEqual(ran, [], 'never run');
  } finally {
    RegExp.prototype.test = original.test;
    RegExp.prototype.exec = original.exec;
  }
  // captureGroupCount reads groups the way JavaScript does
  for (const [src, n] of [['(a)(?:b)(?<x>c)(?=d)(?<=e)(?!f)(?<!g)', 2], ['\\(a\\)[(](b)', 1], ['[]](c)', 1], ['[^](d)', 1], ['[\\]()](e)', 1]]) {
    assert.equal(captureGroupCount(src), n, src);
    assert.equal(new RegExp(`${src}|`).exec('').length - 1, n, `${src}: as V8 counts`);
  }
});

test('every refusal for cost says how to rewrite, and the rewrite it suggests passes (SEC22b S-4)', () => {
  for (const [refused, rewritten] of [
    ['.*/videos?/.*\\.mp4', '/videos?/.*\\.mp4'], // a leading ".*"
    ['/lessons?/(\\d+)/.*', '/lessons?/(\\d+)/'], // a trailing ".*"
    ['/sessions/.*/.*\\.(?:mp4|m3u8)$', '/sessions/.*\\.(?:mp4|m3u8)$'], // two joined
    ['/content/.*/.*_(?:audio|video)\\.mp4', '/content/.*_(?:audio|video)\\.mp4'],
    ['/(?:audio|video)/.+/.+\\.mp4$', '/(?:audio|video)/.+\\.mp4$'],
    ['/recordings/.*/(?:audio|video)/.*\\.m4s', '/(?:audio|video)/.*\\.m4s'], // the part before
    ['https?://[^/]+/.*/master\\.m3u8', '/.*/master\\.m3u8'],
    ['/api/v\\d+/.*/.*\\.json$', '/api/v\\d+/.*\\.json$'], // three wide quantifiers
    ['/media/[^/]+/[^/]+/[^/]+\\.mp4', '/media/.*/[^/]+\\.mp4'],
    ['/hls/[^/]+/.+/playlist\\.m3u8\\?.*', '/hls/[^/]+/.+/playlist\\.m3u8\\?'],
    ['.*x.*.{20}y', 'x.*.{20}y'], // the wide segment
  ]) {
    assert.match(regexComplexity(refused), /\. To fix it, /, refused);
    assert.equal(regexComplexity(rewritten), '', rewritten);
  }
  // anchored already: no "start with ^"
  assert.doesNotMatch(regexComplexity(`^${'.?'.repeat(27)}x`), /start a pathPattern with/);
  assert.match(regexComplexity(`${'.?'.repeat(19)}x`), /start a pathPattern with "\^"/);
});
