// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Tests of the profile and fixture check, on fixed samples (testdata/) and
// on the repository itself. Leaky variants are made from the clean sample
// in memory, so no file here contains anything that looks like a secret.
//
//   node --test "scripts/profilecheck/*.test.mjs"

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { checkRepository, scanForLeaks, hostProblem, hostsOverlap, formatResult, summaryMarkdown, PROFILES_DIR, FIXTURES_DIR, SCHEMA_FILE } from './check.mjs';
import { checkProfile } from '../../extension/src/lib/profile.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.resolve(here, '..', '..');
const schema = JSON.parse(fs.readFileSync(path.join(repo, SCHEMA_FILE), 'utf8'));
const sampleProfile = JSON.parse(fs.readFileSync(path.join(here, 'testdata', 'profiles', 'demo.json'), 'utf8'));
const sampleReport = JSON.parse(fs.readFileSync(path.join(here, 'testdata', 'fixtures', 'demo', 'basic.report.json'), 'utf8'));
const clone = (v) => JSON.parse(JSON.stringify(v));

/** A temporary repository with the given files (relative path → JSON value or text). */
function makeRoot(t, files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-profilecheck-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const all = { [`${PROFILES_DIR}/LICENSE`]: 'CC0 1.0 Universal\n', ...files };
  for (const [rel, v] of Object.entries(all)) {
    if (v === undefined) continue;
    const p = path.join(root, rel);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    fs.writeFileSync(p, typeof v === 'string' ? v : JSON.stringify(v, null, 2));
  }
  return root;
}

const sampleFiles = (over = {}) => ({
  [`${PROFILES_DIR}/demo.json`]: sampleProfile,
  [`${FIXTURES_DIR}/demo/basic.report.json`]: sampleReport,
  ...over,
});

const run = (t, files) => checkRepository({ root: makeRoot(t, files), schema, checkProfile });
const messages = (res) => res.errors.map((e) => `${e.file} ${e.where}: ${e.message}`);

function assertError(res, file, re) {
  assert.ok(
    res.errors.some((e) => e.file === file && re.test(`${e.where}: ${e.message}`)),
    `no error on ${file} matching ${re}; errors:\n${messages(res).join('\n')}`,
  );
}

test('the repository itself passes', () => {
  const res = checkRepository({ root: repo, schema, checkProfile });
  assert.deepEqual(messages(res), []);
  assert.ok(res.profiles >= 1 && res.fixtures >= 2);
});

test('the clean sample passes, including byte ranges, DRM system ids, track selectors and fakes', (t) => {
  const res = run(t, sampleFiles());
  assert.deepEqual(messages(res), []);
  assert.deepEqual(res.warnings, []);
  assert.equal(res.profiles, 1);
  assert.equal(res.fixtures, 1);
});

// Each leak: a change to the clean report, where the finding must be, and
// the secret that must not appear in the output.
const LEAKS = [
  ['cookie header text', (r) => (r.inputs.fetches[0].text += 'Cookie: sid=q9TzK2'), 'inputs.fetches[0].text', /cookie or authorisation header/, 'q9TzK2'],
  ['cookie field', (r) => (r.inputs.resources[0].Cookie = 'q9TzK2'), 'inputs.resources[0].Cookie', /cookie or authorisation field/, 'q9TzK2'],
  ['HAR-style header', (r) => (r.inputs.headers = [{ name: 'Set-Cookie', value: 'q9TzK2' }]), 'inputs.headers[0].name', /header name/, 'q9TzK2'],
  ['session cookie', (r) => (r.dom[0].valueShape = 'JSESSIONID=q9TzK2wX'), 'dom[0].valueShape', /session cookie/, 'q9TzK2wX'],
  ['e-mail address', (r) => (r.dom[0].valueShape = ['lecturer', 'example.org'].join('@')), 'dom[0].valueShape', /"@"/, 'lecturer'],
  ['encoded @', (r) => (r.page.url = 'https://tenant.lectures.example.org/watch/a%40b'), 'page.url', /"@"/, 'a%40b'],
  ['JSON Web Token', (r) => (r.dom[0].valueShape = ['eyJhbGciOiJub25lIn0', 'eyJzdWIiOiJ4In0', ''].join('.')), 'dom[0].valueShape', /JSON Web Token/, 'eyJzdWIiOiJ4In0'],
  ['bearer token', (r) => (r.dom[0].valueShape = 'Bearer q9TzK2wXm4Lp'), 'dom[0].valueShape', /bearer/, 'q9TzK2wXm4Lp'],
  ['signed query value', (r) => (r.inputs.resources[1].url = r.inputs.resources[1].url.replace('{tok52-4}', 'q9TzK2wXm4Lp7')), 'inputs.resources[1].url', /query value that is not a redaction placeholder/, 'q9TzK2wXm4Lp7'],
  ['query value in manifest text', (r) => (r.inputs.fetches[0].text = r.inputs.fetches[0].text.replace('Policy={tok64-3}', 'Policy=q9TzK2wX')), 'inputs.fetches[0].text', /query value/, 'q9TzK2wX'],
  ['identity query parameter', (r) => (r.page.url += '&studentId={str8-9}'), 'page.url', /query parameter that names a person/, null],
  ['identity field', (r) => (r.dom[0].userName = 'Xxxx'), 'dom[0].userName', /field that names a person/, null],
  ['real UUID', (r) => (r.inputs.resources[0].url = r.inputs.resources[0].url.replace('00000000-0000-4000-8000-00000000000a', '3f2b8c1e-9d4a-4b7e-8c21-5a6f0e9d1b37')), 'inputs.resources[0].url', /unredacted UUID/, '3f2b8c1e'],
  ['real hex id', (r) => (r.inputs.resources[0].url = r.inputs.resources[0].url.replace('0000000000000003', '9f3a7c2e1b4d6a8f')), 'inputs.resources[0].url', /hexadecimal/, '9f3a7c2e1b4d6a8f'],
  ['student number in a path', (r) => (r.inputs.resources[0].url = r.inputs.resources[0].url.replace('/00042/', '/20231234/')), 'inputs.resources[0].url', /number in an address path/, '20231234'],
  ['token in a path', (r) => (r.inputs.resources[0].url = r.inputs.resources[0].url.replace('1AAaaaa0000aaAAAAaaa0aaa', 'Kq7ZtR2mWx9LpB4vNc8sHd3J')), 'inputs.resources[0].url', /unredacted token/, 'Kq7ZtR2mWx9LpB4vNc8sHd3J'],
  ['token outside an address', (r) => (r.dom[0].valueShape = 'x Kq7ZtR2mWx9LpB4vNc8sHd3JfG6yTa1E x'), 'dom[0].valueShape', /unredacted token/, 'Kq7ZtR2mWx9LpB4vNc8sHd3JfG6yTa1E'],
  ['fragment', (r) => (r.page.url = 'https://tenant.lectures.example.org/watch/{tok24-1}#access=x9'), 'page.url', /fragment/, 'access=x9'],
  ['user info', (r) => (r.page.url = 'https://someone:pw@tenant.lectures.example.org/watch/x'), 'page.url', /"@"|user name or password/, 'someone'],
  ['number in a host', (r) => (r.page.url = 'https://u20231234.lectures.example.org/watch/x'), 'page.url', /long number in a host/, '20231234'],
];

for (const [name, change, where, kind, secret] of LEAKS) {
  test(`a fixture with a leak fails: ${name}`, (t) => {
    const report = clone(sampleReport);
    change(report);
    const findings = scanForLeaks(report);
    assert.ok(findings.some((f) => f.where === where && kind.test(f.kind)), JSON.stringify(findings));
    const res = run(t, sampleFiles({ [`${FIXTURES_DIR}/demo/basic.report.json`]: report }));
    assertError(res, `${FIXTURES_DIR}/demo/basic.report.json`, kind);
    if (secret) {
      const out = formatResult(res, { github: true }) + summaryMarkdown(res);
      assert.ok(!out.includes(secret), 'the output repeats the value');
    }
  });
}

test('profiles: "@" only as a maintainer handle; headers and tokens never', () => {
  assert.deepEqual(scanForLeaks(sampleProfile, { profile: true }), []);
  const p = clone(sampleProfile);
  p.name = ['someone', 'example.org'].join('@');
  p.lecture.title.selector = 'meta[name="cookie: x"]';
  const kinds = scanForLeaks(p, { profile: true }).map((f) => `${f.where} ${f.kind}`);
  assert.ok(kinds.some((k) => k.startsWith('name ') && k.includes('"@"')), kinds.join('\n'));
  assert.ok(kinds.some((k) => k.startsWith('lecture.title.selector') && k.includes('header')), kinds.join('\n'));
});

test('layout and profile rules', async (t) => {
  const cases = [
    ['stray file in profiles', { [`${PROFILES_DIR}/notes.txt`]: 'hi' }, `${PROFILES_DIR}/notes.txt`, /only <id>\.json profiles and LICENSE/],
    ['HAR file in fixtures', { [`${FIXTURES_DIR}/demo/session.har`]: '{}' }, `${FIXTURES_DIR}/demo/session.har`, /no HAR files/],
    ['file at the fixtures top level', { [`${FIXTURES_DIR}/readme.json`]: '{}' }, `${FIXTURES_DIR}/readme.json`, /nothing else belongs/],
    ['fixtures without a profile', { [`${FIXTURES_DIR}/other/basic.report.json`]: sampleReport }, `${FIXTURES_DIR}/other`, /no profile extension\/profiles\/other\.json/],
    ['not a report', { [`${FIXTURES_DIR}/demo/basic.report.json`]: { hello: 1 } }, `${FIXTURES_DIR}/demo/basic.report.json`, /^kind: not a detection report/],
    ['report made with another profile', { [`${FIXTURES_DIR}/demo/basic.report.json`]: { ...sampleReport, profile: { id: 'panopto' } } }, `${FIXTURES_DIR}/demo/basic.report.json`, /profile "panopto"/],
    ['generic fixture with a profile', { [`${FIXTURES_DIR}/generic/a.report.json`]: sampleReport }, `${FIXTURES_DIR}/generic/a.report.json`, /profile must be null/],
    ['broken JSON', { [`${PROFILES_DIR}/demo.json`]: '{ "id": "demo", }' }, `${PROFILES_DIR}/demo.json`, /not valid JSON/],
    ['byte order mark', { [`${PROFILES_DIR}/demo.json`]: '\ufeff' + JSON.stringify(sampleProfile) }, `${PROFILES_DIR}/demo.json`, /byte order mark/],
    ['schema error', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, tracks: undefined } }, `${PROFILES_DIR}/demo.json`, /tracks/],
    ['course field', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, lecture: { courseCode: { selector: '.code' } } } }, `${PROFILES_DIR}/demo.json`, /lecture/],
    ['bad regex', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { ...sampleProfile.match, pathPattern: '^/watch/(' } } }, `${PROFILES_DIR}/demo.json`, /\/match\/pathPattern: invalid regular expression/],
    // regexes that backtrack out of control (FB1 L-1): the extension's own rule
    ['quantifier in a repeated group', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { ...sampleProfile.match, pathPattern: '^/watch/(?:a{1,30}){1,30}x' } } }, `${PROFILES_DIR}/demo.json`, /\/match\/pathPattern: a repeated group must have a fixed width/],
    ['group hidden behind [^]', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { ...sampleProfile.match, pathPattern: '^/watch/[^](a+)+b' } } }, `${PROFILES_DIR}/demo.json`, /\/match\/pathPattern: a repeated group must have a fixed width/],
    ['adjacent unbounded quantifiers', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, tracks: [{ ...sampleProfile.tracks[0], detect: { ...sampleProfile.tracks[0].detect, urlPattern: '\\d*\\d*\\d*x' } }, ...sampleProfile.tracks.slice(1)] } }, `${PROFILES_DIR}/demo.json`, /\/tracks\/0\/detect\/urlPattern: two .* in a row/],
    ['modifier group', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { ...sampleProfile.match, pathPattern: '^/watch/a*(?i:)a*x' } } }, `${PROFILES_DIR}/demo.json`, /\/match\/pathPattern: unsupported group syntax/],
    ['too many choices', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { ...sampleProfile.match, pathPattern: `/watch/${'.?'.repeat(25)}x` } } }, `${PROFILES_DIR}/demo.json`, /\/match\/pathPattern: too many ways to match/],
    ['costly regex on a host with a blocked path', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { hosts: ['www.linkedin.com'], pathPattern: '^/(?:[a-z0-9/]?[a-z0-9/]?)*!$' } } }, `${PROFILES_DIR}/demo.json`, /^\/match\/pathPattern: .*linkedin\.com\/learning/],
    ['wide bounded quantifiers',{ [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { ...sampleProfile.match, pathPattern: '^/watch/.{0,200}.{0,200}x' } } }, `${PROFILES_DIR}/demo.json`, /\/match\/pathPattern: two .* in a row/],
    ['file name differs from id', { [`${PROFILES_DIR}/demo.json`]: undefined, [`${PROFILES_DIR}/demo2.json`]: sampleProfile }, `${PROFILES_DIR}/demo2.json`, /must be named demo\.json/],
    ['fixtures field', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, fixtures: 'fixtures/other/' } }, `${PROFILES_DIR}/demo.json`, /\/fixtures: must be "fixtures\/demo\/"/],
    ['shared CDN host', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { ...sampleProfile.match, mediaHosts: ['*.cloudfront.net'] } } }, `${PROFILES_DIR}/demo.json`, /\/match\/mediaHosts\/0: "\*\.cloudfront\.net" is a shared service/],
    ['video site', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { hosts: ['*.youtube.com'] } } }, `${PROFILES_DIR}/demo.json`, /youtube\.com/],
    // the do-not-handle list (extension/src/lib/blocklist.js): YouTube, MOOCs and online / paid course platforms
    ['YouTube', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { hosts: ['www.youtube.com'] } } }, `${PROFILES_DIR}/demo.json`, /^\/match\/hosts\/0: "www\.youtube\.com" covers YouTube, which LectureFlow does not handle/],
    ['course platform', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { hosts: ['*.udemy.com'] } } }, `${PROFILES_DIR}/demo.json`, /^\/match\/hosts\/0: "\*\.udemy\.com" covers an online course platform \(MOOC or paid courses\), which LectureFlow does not handle/],
    ['course platform media host', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { ...sampleProfile.match, mediaHosts: ['mp4-c.udemycdn.com'] } } }, `${PROFILES_DIR}/demo.json`, /^\/match\/mediaHosts\/0: .*does not handle/],
    ['blocked path of a host', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { hosts: ['www.linkedin.com'], pathPattern: '^/(feed|learning)/' } } }, `${PROFILES_DIR}/demo.json`, /^\/match\/pathPattern: "www\.linkedin\.com" covers linkedin\.com\/learning .*must keep out of \/learning/],
    ['blocked path, no pathPattern', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { hosts: ['*.bilibili.com'] } } }, `${PROFILES_DIR}/demo.json`, /^\/match\/pathPattern: .*bilibili\.com\/cheese.*add a pathPattern/],
    ['public suffix', { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, match: { hosts: ['*.ac.uk'] } } }, `${PROFILES_DIR}/demo.json`, /whole public suffix/],
    ['no fixtures', { [`${FIXTURES_DIR}/demo/basic.report.json`]: undefined }, `${PROFILES_DIR}/demo.json`, /has no fixtures/],
  ];
  for (const [name, over, file, re] of cases) {
    await t.test(name, (tt) => assertError(run(tt, sampleFiles(over)), file, re));
  }
});

test('a draft may come without fixtures', (t) => {
  const res = run(t, { [`${PROFILES_DIR}/demo.json`]: { ...sampleProfile, draft: true } });
  assert.deepEqual(messages(res), []);
});

test('overlapping hosts between profiles are a warning, not an error', (t) => {
  const other = { ...clone(sampleProfile), id: 'demo-two', fixtures: 'fixtures/demo-two/', match: { hosts: ['media.lectures.example.org'] } };
  const res = run(t, sampleFiles({ [`${PROFILES_DIR}/demo-two.json`]: other, [`${FIXTURES_DIR}/demo-two/basic.report.json`]: { ...sampleReport, profile: { id: 'demo-two' } } }));
  assert.deepEqual(messages(res), []);
  assert.equal(res.warnings.length, 1);
  assert.match(res.warnings[0].message, /"(\*\.lectures|media\.lectures)\.example\.org" overlaps "(\*\.lectures|media\.lectures)\.example\.org" of extension\/profiles\/demo(-two)?\.json/);
});

test('host helpers', () => {
  assert.equal(hostProblem('*.hosted.panopto.com'), '');
  assert.equal(hostProblem('*.example.ac.uk'), '');
  assert.match(hostProblem('d1abcd.cloudfront.net'), /shared service/);
  assert.match(hostProblem('*.co.uk'), /public suffix/);
  assert.ok(hostsOverlap('*.panopto.com', '*.hosted.panopto.com'));
  assert.ok(hostsOverlap('a.example.org', '*.example.org'));
  assert.ok(!hostsOverlap('*.panopto.eu', '*.panopto.com'));
});

test('output: readable lines and escaped GitHub annotations', () => {
  const res = { profiles: 1, fixtures: 0, warnings: [], errors: [{ file: 'extension/profiles/a,b.json', where: '/match', message: 'line 1\nline 2: 100%' }] };
  const out = formatResult(res, { github: true });
  assert.match(out, /^extension\/profiles\/a,b\.json\n {2}error: \/match: line 1\nline 2: 100%/);
  assert.match(out, /::error file=extension\/profiles\/a%2Cb\.json,title=Profile check::\/match: line 1%0Aline 2: 100%25/);
  assert.match(out, /1 errors, 0 warnings$/);
  assert.match(summaryMarkdown(res), /\| error \| `extension\/profiles\/a,b\.json` \| `\/match` \| line 1 line 2: 100% \|/);
});
