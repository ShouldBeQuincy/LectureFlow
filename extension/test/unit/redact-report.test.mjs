// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRedactor, shape, isIdentityField } from '../../src/lib/redact.js';
import { buildReport, scanReport, reportFileName, redactTrack } from '../../src/lib/report.js';
import { replayReport, compareReplay, compareProfile } from '../../src/lib/replay.js';
import { validate } from '../../src/lib/jsonschema.js';
import { parseHls, hlsTracks } from '../../src/lib/hls.js';
import * as S from '../support/synth.mjs';
import { reportFromSynthetic, runSynthetic, panoptoProfile } from '../support/reports.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const reportSchema = JSON.parse(fs.readFileSync(path.join(root, 'schema', 'report.schema.json'), 'utf8'));

test('URLs keep scheme, host, path; query values become placeholders; fragment and user info go', () => {
  const r = createRedactor();
  const out = r.url('https://user:pw@CDN.example.org:8443/v/master.m3u8?Policy=eyJhbGciOiJIUzI1NiJ9abcdefgh&Signature=abc123&Expires=1767225600&lang=en#t=30');
  assert.equal(out, 'https://cdn.example.org:8443/v/master.m3u8?Policy={tok28-1}&Signature={str6-2}&Expires={num10-3}&lang={str2-4}');
  assert.ok(!out.includes('eyJ') && !out.includes('abc123') && !out.includes('1767225600'));
  // the same value is the same placeholder everywhere in one report
  assert.equal(r.url('https://cdn.example.org/x?sig=abc123'), 'https://cdn.example.org/x?sig={str6-2}');
  assert.equal(r.url('blob:https://h/1'), 'blob:');
  assert.equal(r.url('data:video/mp4;base64,AAAA'), 'data:');
  assert.equal(r.url('nonsense'), '(invalid)');
  // a user name in the URL is a finding even though it is dropped
  assert.ok(r.findings.some((f) => f.kind === 'identity_field'));
});

test('ID-like path runs get fakes of the same shape; the same ID the same fake', () => {
  const r = createRedactor();
  const a = r.url('https://h.example.org/sessions/3F2504E0-4F89-41D3-9A0C-0305E82C3301/3f2504e0-4f89-41d3-9a0c-0305e82c3301-0b9a8f77-1111-4222-8333-444455556666.hls/master.m3u8');
  assert.equal(a, 'https://h.example.org/sessions/00000000-0000-4000-8000-000000000001/00000000-0000-4000-8000-000000000001-00000000-0000-4000-8000-000000000002.hls/master.m3u8');
  assert.equal(r.segment('a1b2c3d4e5f60718293a4b5c'), '000000000000000000000001'); // hex run, 24 chars
  assert.equal(r.segment('chunklist_w123456789_b2500000.m3u8'), 'chunklist_w000000001_b0000002.m3u8');
  assert.equal(r.segment('Xk29fLq0Zp7Tn3Rv8Ws1Yb6'), '1a00aAa0Aa0Aa0Aa0Aa0Aa0'); // token: class-preserving mask
  assert.equal(r.segment('seg-$Number%05d$.m4s'), 'seg-$Number%05d$.m4s'); // template variables stay
  // URL-safe base64 tickets in a path (Kaltura /ks/…, JWTs): the whole run
  const ks = 'djJ8MTIzNDU2fGVYcGlyeS1zZWNyZXQtdGlja2V0LXZhbHVlLTEyMzQ1Ng-_Xy';
  const fakeKs = r.segment(ks);
  assert.equal(fakeKs.length, ks.length);
  assert.ok(!/djJ8|ZWNy|_Xy/.test(fakeKs), fakeKs);
  // GUID pairs keep their shape (profile patterns still match them)
  assert.match(r.segment('3f2504e0-4f89-41d3-9a0c-0305e82c3301-0b9a8f77-1111-4222-8333-444455556666.hls'), /^[0-9a-f-]{36}-[0-9a-f-]{36}\.hls$/);
  assert.equal(r.segment('Viewer.aspx'), 'Viewer.aspx');
  assert.equal(r.segment('2026'), '2026'); // a year is not an ID
  assert.equal(r.findings.length, 0);
});

test('tokens without digits and escaped base64 tokens in paths (security review L6); words and slugs stay', () => {
  const r = createRedactor();
  // 34 letters in one run: a token, case shape kept
  const letters = 'QwErTyUiOpAsDfGhJkLzXcVbNmQwErTyUi';
  const fake = r.segment(letters);
  assert.equal(fake.length, letters.length);
  assert.notEqual(fake, letters);
  assert.ok(!fake.includes('QwErTy'), fake);
  assert.equal(r.segment(letters), fake, 'the same token, the same fake');
  assert.notEqual(r.segment('abcdefghijklmnopqrstuvwxyzabcdefgh'), 'abcdefghijklmnopqrstuvwxyzabcdefgh', 'lower case only, 34 letters');
  // base64url without digits, cut by - and _: the case changes give it away
  const b64ish = 'aBcDeFgHiJ-kLmNoPqRsT_uVwXyZaBcDeF';
  assert.notEqual(r.segment(b64ish), b64ish);
  // base64 with "+", "/" or "=" escaped in the path: taken whole, escapes in place
  const esc = 'Ab12Cd34Ef%2BGh56Ij78Kl%2FMn90OpQr%3D%3D';
  const fakeEsc = r.segment(esc);
  assert.ok(!/Ab12|Gh56|Mn90/.test(fakeEsc), fakeEsc);
  assert.equal(fakeEsc.replace(/[^%]/g, ''), '%%%%', 'the escapes stay where they were');
  assert.equal(decodeURIComponent(fakeEsc).length, decodeURIComponent(esc).length);
  assert.notEqual(r.segment('Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2+Ji1Hg0Fe9Dc8Ba'), 'Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2+Ji1Hg0Fe9Dc8Ba', 'a literal "+" too');
  // what is not a token stays
  for (const keep of ['Introduction-To-Computer-Science-Lecture', 'lecture-recording-week-one-introduction', 'C%2B%2B-Programming-2026', 'PanoptoPodcastDownload', 'master.m3u8', 'Viewer.aspx']) {
    assert.equal(r.segment(keep), keep, keep);
  }
  assert.equal(r.findings.length, 0);
});

test('MPD value attributes: short enumerations stay, anything longer or ID-like is shaped, addresses redacted (L6)', () => {
  const r = createRedactor();
  const out = r.mpd(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet contentType="audio">
    <Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>
    <AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="2"/>
    <SupplementalProperty schemeIdUri="urn:example:viewer" value="dGhpcy1pcy1hLXNlY3JldC10b2tlbi12YWx1ZQ"/>
    <EssentialProperty schemeIdUri="urn:example:student" value="20231234"/>
    <SupplementalProperty schemeIdUri="urn:example:licence" value="https://licence.example.org/v1/keys?token=SECRETSECRET"/>
    </AdaptationSet></Period></MPD>`);
  assert.match(out, /<Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"\/>/);
  assert.match(out, /value="2"\/>/);
  assert.ok(!/dGhpcy1p|20231234|SECRETSECRET/.test(out), out);
  assert.match(out, /value="https:\/\/licence\.example\.org\/v1\/keys\?token=\{str12-1\}"/);
});

test('relative manifest URIs redact segment by segment, so a replay can still resolve them', () => {
  const r = createRedactor();
  const base = 'https://cdn.example.org/s/3f2504e0-4f89-41d3-9a0c-0305e82c3301/master.m3u8?sig=QWERTYUIOPASDFGH';
  const child = 'https://cdn.example.org/s/3f2504e0-4f89-41d3-9a0c-0305e82c3301/v/index.m3u8?sig=QWERTYUIOPASDFGH';
  const rel = r.uriRef('v/index.m3u8?sig=QWERTYUIOPASDFGH#frag');
  assert.equal(new URL(rel, r.url(base)).href, r.url(child));
  assert.equal(r.uriRef('//cdn.example.org/s/a.m3u8'), '//cdn.example.org/s/a.m3u8');
  assert.equal(r.uriRef('../../x/12345678.ts'), '../../x/00000001.ts');
});

test('personal data in addresses stops the report (and never survives redaction)', () => {
  const cases = [
    ['https://h.example.org/watch?email=a.b%40uni.example.edu', ['identity_field', 'email']],
    ['https://h.example.org/watch?u=someone@uni.example.edu', ['email']],
    ['https://h.example.org/watch?studentId=12345678', ['identity_field']],
    ['https://h.example.org/watch?user_name=x', ['identity_field']],
    ['https://h.example.org/watch?firstName=x', ['identity_field']],
    ['https://h.example.org/users/alice/video.mp4', ['name']],
    ['https://h.example.org/~bob/lecture.mp4', ['name']],
    ['https://h.example.org/media/jane.doe/lecture.mp4', ['name']],
    ['https://h.example.org/media/jane.doe%40uni.example.edu/x.mp4', ['email']],
    ['https://s12345678.example.org/x.mp4', ['student_id']],
  ];
  for (const [u, kinds] of cases) {
    const r = createRedactor();
    const out = r.url(u);
    const got = new Set(r.findings.map((f) => f.kind));
    for (const k of kinds) assert.ok(got.has(k), `${u} → ${[...got]}`);
    assert.ok(!/alice|bob|jane|someone/.test(out.replace(/users|~/g, '')) || got.size > 0);
    assert.ok(!out.includes('12345678') || u.startsWith('https://s1'), out);
  }
});

test('ordinary lecture addresses are not mistaken for personal data (反例)', () => {
  const r = createRedactor();
  for (const u of [
    'https://x.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=3f2504e0-4f89-41d3-9a0c-0305e82c3301&start=0',
    'https://x.cloud.panopto.eu/Panopto/Pages/Embed.aspx?id=1&autoplay=false&offerviewer=true&showtitle=true&interactivity=all',
    'https://d1.cloudfront.net/sessions/a/b.object.hls/master.m3u8?Policy=x&Signature=y&Key-Pair-Id=K1',
    'https://cdn.example.org/hls/v720/index.m3u8?token=abc&expires=1&sid=99',
    'https://cdn.example.org/media/lecture-07.mp4',
    'https://cdn.example.org/a/stream.mpd',
    'https://cdn.example.org/2026/10/21/seg_00001.ts',
    'https://video.example.edu/watch?v=abc&name=Lecture',
  ]) r.url(u);
  assert.deepEqual(r.findings, []);
  assert.equal(isIdentityField('id'), false);
  assert.equal(isIdentityField('name'), false);
  assert.equal(isIdentityField('Key-Pair-Id'), false);
  assert.equal(isIdentityField('userId'), true);
  assert.equal(isIdentityField('data-student-id'), true);
  assert.equal(isIdentityField('E_Mail'), true);
});

test('HLS text: URIs redacted, free text shaped, comments dropped, byte ranges kept', () => {
  const r = createRedactor();
  const text = [
    '#EXTM3U',
    '# generated for Dr. Jane Doe',
    '#EXT-X-SESSION-DATA:DATA-ID="com.example.title",VALUE="Lecture by Jane Doe"',
    '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",LANGUAGE="en",URI="a/en.m3u8?tok=SECRETSECRETSECRET"',
    '#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="aac"',
    'v/720.m3u8?tok=SECRETSECRETSECRET',
    '#EXT-X-KEY:METHOD=AES-128,URI="https://keys.example.org/k/12345678?u=jdoe",IV=0x0123456789abcdef0123456789abcdef',
    '#EXT-X-PROGRAM-DATE-TIME:2026-10-21T09:35:00.000Z',
    '#EXTINF:10.0,Intro by Jane',
    '#EXT-X-BYTERANGE:1000@2000',
    'seg.ts',
  ].join('\n');
  const out = r.hls(text);
  assert.ok(!/Jane|Doe|SECRET|12345678|jdoe|0123456789abcdef/.test(out), out);
  assert.match(out, /VALUE="Xxxxxxx xx Xxxx Xxx"/);
  assert.match(out, /NAME="Xxxxxxx",LANGUAGE="en",URI="a\/en.m3u8\?tok=\{tok18-1\}"/);
  assert.match(out, /BANDWIDTH=2500000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="aac"/);
  assert.match(out, /#EXT-X-BYTERANGE:1000@2000/);
  assert.match(out, /#EXT-X-PROGRAM-DATE-TIME:0000-00-00X00:00:00.000X/);
  assert.ok(!out.includes('# generated'));
  // the redacted master still parses to the same structure
  const before = hlsTracks(parseHls(text, 'https://cdn.example.org/m.m3u8'), 'https://cdn.example.org/m.m3u8');
  const after = hlsTracks(parseHls(out, 'https://cdn.example.org/m.m3u8'), 'https://cdn.example.org/m.m3u8');
  assert.deepEqual(after.map((t) => [t.kind, t.quality]), before.map((t) => [t.kind, t.quality]));
  assert.deepEqual(r.findings, []);
});

test('MPD text: DRM data opaque, titles shaped, URLs redacted, structure kept', () => {
  const r = createRedactor();
  const out = r.mpd(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" mediaPresentationDuration="PT1H" availabilityStartTime="2026-10-21T09:35:00Z">
    <ProgramInformation><Title>Cryptography — Jane Doe</Title></ProgramInformation>
    <BaseURL>https://media.example.org/u/12345678/</BaseURL>
    <Period><AdaptationSet contentType="video" lang="en" data-owner="jdoe">
      <ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"><cenc:pssh>AAAAQ3Bzc2g=</cenc:pssh></ContentProtection>
      <SegmentTemplate media="$RepresentationID$/$Number$.m4s?sig=SIGSIGSIGSIGSIGSIG"/>
      <Representation id="v-3f2504e0-4f89-41d3-9a0c-0305e82c3301" bandwidth="1000" height="720"/>
    </AdaptationSet></Period></MPD>`);
  assert.ok(!/Jane|Doe|12345678|AAAAQ3|SIGSIG|jdoe|3f2504e0/.test(out), out);
  assert.match(out, /<Title>Xxxxxxxxxxxx — Xxxx Xxx<\/Title>/);
  assert.match(out, /<cenc:pssh>\{opaque\}<\/cenc:pssh>/);
  assert.match(out, /media="\$RepresentationID\$\/\$Number\$\.m4s\?sig=\{tok18-1\}"/);
  assert.match(out, /contentType="video" lang="en"/);
  assert.equal(r.mpd('<not xml'), '');
});

test('shapes', () => {
  assert.equal(shape('CSC1003 Lecture 5 — 讲座'), 'XXX0000 Xxxxxxx 0 — 字字');
  assert.equal(shape('a@b.org'), 'x_x.xxx');
  assert.equal(shape('x'.repeat(200)).length, 121);
});

test('synthetic Panopto report: valid, no findings, no secret left, replays exactly', async () => {
  const { report, findings } = await reportFromSynthetic('panopto');
  assert.deepEqual(findings, []);
  assert.deepEqual(validate(reportSchema, report), []);
  const json = JSON.stringify(report);
  for (const secret of ['5e6f7a8b', 'abcdef01', 'Zm9vYmFy', 'eyJTdGF0', 'K0000FAKE', 'CSC1003', 'Wed Oct', '1a2b3c4d']) {
    assert.ok(!json.includes(secret), secret);
  }
  // resources that are not media (scripts, APIs) are not in the report
  assert.ok(!json.includes('viewer.js') && !json.includes('GetDeliveryInfo'));
  // the school's own subdomain is a placeholder, the region stays (lead ruling 8)
  assert.ok(!json.includes('lf-fixture'));
  assert.equal(new URL(report.page.url).hostname, 'tenant.cloud.panopto.eu');
  // the full MP4 from DeliveryInfo, recommended instead of audio and screen
  assert.deepEqual(report.expected.profile.tracks.map((t) => `${t.role}:${t.recommended}`), ['podcast:true', 'primary-audio:false', 'screen:false', 'screen:false']);
  assert.equal(report.expected.profile.downloadPolicy, 'disabled');
  assert.equal(report.expected.profile.lecture.titleShape, 'XXX0000/X00/00 Xxx Xxx 00 00:00:00 XXX 0000');
  const replay = await replayReport(JSON.parse(json));
  assert.deepEqual(compareReplay(report, replay), []);
  assert.deepEqual(compareProfile(report, panoptoProfile, replay), []);
  assert.equal(reportFileName(report), 'lectureflow-report-panopto-2026-10-26.json');
});

test('synthetic generic report: valid, replays exactly; a changed manifest is caught', async () => {
  const { report, findings } = await reportFromSynthetic('generic');
  assert.deepEqual(findings, []);
  assert.deepEqual(validate(reportSchema, report), []);
  assert.equal(report.profile, null);
  assert.equal(report.expected.profile, null);
  const replay = await replayReport(report);
  assert.deepEqual(compareReplay(report, replay), []);
  // tamper: drop the audio group from the master → the replay disagrees
  const tampered = JSON.parse(JSON.stringify(report));
  const master = tampered.inputs.fetches.find((f) => f.text && f.text.includes('EXT-X-MEDIA'));
  master.text = master.text.split('\n').filter((l) => !l.startsWith('#EXT-X-MEDIA')).join('\n');
  const diffs = compareReplay(tampered, await replayReport(tampered));
  assert.ok(diffs.some((d) => d.startsWith('summary.total')), diffs.join('\n'));
});

test('a report with personal data is refused, with the reason and no value', async () => {
  const run = await runSynthetic('generic');
  run.frames[0].url = 'https://lectures.example.edu/watch/lecture-07?studentId=20261234&email=x%40example.edu';
  const { findings } = buildReport({ ...run, pageUrl: run.frames[0].url, extension: { version: '0', browser: 'chrome' }, now: new Date() });
  const kinds = new Set(findings.map((f) => f.kind));
  assert.ok(kinds.has('identity_field') && kinds.has('email'));
  assert.ok(findings.every((f) => !JSON.stringify(f).includes('20261234')));
  // a DOM attribute named like a person on a selected node
  const frames = run.frames.map((f) => ({ ...f, reads: [{ key: 'lecture.title.0', selector: 'h1', attr: '', count: 1, value: 'x', nodes: [{ path: [{ tag: 'h1', id: '', classes: [] }], attrNames: ['data-username'] }] }] }));
  const r2 = buildReport({ ...run, frames, extension: { version: '0', browser: 'chrome' }, now: new Date() });
  assert.ok(r2.findings.some((f) => f.kind === 'identity_field'));
});

test('the final scan: "@" anywhere but in a byte range', () => {
  assert.deepEqual(scanReport({ a: 'x 1000@2000 y' }), []);
  assert.equal(scanReport({ a: ['ok', { b: 'mail me: a@b.org' }] })[0].kind, 'email');
  assert.equal(scanReport({ a: 'https://h/x?login=1' })[0].kind, 'identity_field');
});

test('host placeholders: a tenant of the profile’s wildcard hosts becomes "tenant", an LMS page is masked (lead ruling 8)', async () => {
  const { hostRedactor, createRedactor, tenantBasesOf } = await import('../../src/lib/redact.js');
  const bases = tenantBasesOf(panoptoProfile);
  const map = hostRedactor({ tenantBases: bases, maskHosts: ['canvas.myuni.example'] });
  assert.equal(map('myuni.cloud.panopto.eu'), 'tenant.cloud.panopto.eu');
  assert.equal(map('a.b.hosted.panopto.com'), 'tenant.hosted.panopto.com', 'the longest suffix');
  assert.equal(map('myuni.panopto.com'), 'tenant.panopto.com');
  assert.equal(map('d0000fixture00.cloudfront.net'), 'd0000fixture00.cloudfront.net', 'other hosts stay');
  assert.equal(map('canvas.myuni.example'), 'site-1.example');
  const r = createRedactor({ tenantBases: bases });
  assert.equal(r.url('https://myuni.cloud.panopto.eu:8443/Panopto/Pages/Viewer.aspx'), 'https://tenant.cloud.panopto.eu:8443/Panopto/Pages/Viewer.aspx');
  // a report of a Panopto player embedded in an LMS page: the LMS host is masked
  const run = await runSynthetic('panopto');
  const lms = 'https://canvas.myuni.example/courses/1/external_tools/2';
  const { report, findings } = buildReport({ ...run, pageUrl: lms, frames: [{ ...run.frames[0], frameId: 3 }, { frameId: 0, url: lms, elements: [], resources: [], encryptedEvents: 0 }], extension: { version: '0', browser: 'chrome' }, now: new Date() });
  assert.deepEqual(findings, []);
  const json = JSON.stringify(report);
  assert.ok(!json.includes('myuni') && !json.includes('lf-fixture'), json.slice(0, 300));
  assert.equal(new URL(report.page.url).hostname, 'site-1.example');
});

test('short tokens in path segments become fakes; names with numbers in them stay (FB1 L-3)', () => {
  const r = createRedactor();
  // 10 to 19 letters and digits, switching case and class like a random key
  for (const tok of ['Xk9pQ2mZ7vL4', 'a8f3k2z9q1', 'Q7wE2rT9yU1iO3pA5s']) {
    const out = r.url(`https://share.example.org/s/${tok}/master.m3u8`);
    assert.ok(!out.includes(tok), out);
    const fake = out.split('/')[4];
    assert.equal(fake.length, tok.length, out);
    assert.equal(r.url(`https://share.example.org/s/${tok}/master.m3u8`), out, 'the same token, the same fake');
  }
  // not tokens: words with a number, codecs, resolutions, counters
  for (const word of ['index1080p', 'H264Video', 'audio2video3', 'Lecture2026', 'chunk1920x1080', 'master_1080p.m3u8']) {
    assert.equal(r.segment(word), word);
  }
  // long digit runs inside such a word are still ids, as before
  assert.equal(r.segment('segment00012'), 'segment00001');
  assert.equal(r.findings.length, 0);
});

test('a query parameter without a value is kept only when it is a word (FB1 L-3)', () => {
  const r = createRedactor();
  const out = r.url('https://cdn.example.org/v/master.m3u8?Xk9pQ2mZ7vL4&embed&noCache&a8f3k2&1767225600&x=1');
  assert.ok(!/Xk9pQ2mZ7vL4|a8f3k2|1767225600/.test(out), out);
  assert.equal(out, 'https://cdn.example.org/v/master.m3u8?{str12-1}&embed&noCache&{str6-2}&{num10-3}&x={num1-4}');
  // relative manifest URIs too
  assert.equal(r.uriRef('a/en.m3u8?QwErTy12UiOp'), 'a/en.m3u8?{str12-5}');
  assert.equal(r.findings.length, 0);
});

test('language fields keep a language tag and shape anything else (FB1 L-3: DASH lang)', () => {
  const r = createRedactor();
  const mpd = r.mpd(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011">
    <Period>
      <AdaptationSet contentType="audio" lang="en"/>
      <AdaptationSet contentType="audio" lang="zh-Hans-CN"/>
      <AdaptationSet contentType="audio" lang="es-419"/>
      <AdaptationSet contentType="audio" lang="jane-doe-12345"/>
      <AdaptationSet contentType="text" lang="Xk9pQ2mZ7vL4"/>
    </Period></MPD>`);
  assert.ok(!/jane|doe|12345|Xk9pQ2/.test(mpd), mpd);
  assert.match(mpd, /lang="en"/);
  assert.match(mpd, /lang="zh-Hans-CN"/);
  assert.match(mpd, /lang="es-419"/);
  assert.match(mpd, /lang="xxxx-xxx-00000"/);
  const hls = r.hls('#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="A",LANGUAGE="jane-doe-12345",ASSOC-LANGUAGE="en-GB",URI="a.m3u8"\n');
  assert.ok(!/jane|doe|12345/.test(hls), hls);
  assert.match(hls, /LANGUAGE="xxxx-xxx-00000",ASSOC-LANGUAGE="en-GB"/);
  // a track's language in the report
  const track = (language) => redactTrack(createRedactor(), { url: 'https://cdn.example.org/a.m3u8', kind: 'audio', format: 'hls', sizeBytes: -1, sources: [], frameIds: [0], language });
  assert.equal(track('yue').language, 'yue');
  assert.equal(track('en-US').language, 'en-US');
  assert.equal(track('jane-doe-12345').language, 'xxxx-xxx-00000');
  assert.equal(track('en-Smith-ab12cd34').language, 'xx-Xxxxx-xx00xx00');
});

test('SEC18: letter-only and separator-cut tokens are masked; words with numbers stay (低-4, 低-6)', () => {
  const r = createRedactor();
  // masked: letters only, cut by "-", "_", "." or a percent escape, all upper case, codes
  for (const tok of ['QwErTyUiOpAsDf', 'AbCdEfGhIjKl', 'ABCDEFGHIJKLMNOPQRSTUVWX', 'aB3dE-5gH7j-K9xQ2', 'Xk9pQ_2mZ7v_L4aB3', 'Xk9p.Q2mZ7vL4', 'Xk9p%51Q2mZ7vL4', '3fa9c01b7e2d', 'CSC3631W10L2']) {
    const out = r.url(`https://share.example.org/s/${tok}/master.m3u8`);
    const fake = out.split('/')[4];
    assert.equal(fake.length, tok.length, out);
    for (const piece of tok.split(/[._-]|%[0-9A-F]{2}/)) assert.ok(piece.length < 3 || !fake.includes(piece), `${tok} → ${out}`);
  }
  // kept: words and numbers, resolutions, frame and bit rates, slugs, codes
  for (const word of ['Week10Lecture2', 'Lecture01Part2', '1920x1080p30', 'Mp4Audio128k', 'aacLC48kHz2ch', 'index1080p', 'H264Video', 'Fall2024CS101', 'DeliveryInfo.aspx', 'PodcastStreams', 'Lecture-Week-One', 'lecture-notes-week-10.pdf', 'video_avc1_1080p', 'master_1080p.m3u8', '2026-10-21', 'C%2B%2B-Programming-2026']) {
    assert.equal(r.segment(word), word, word);
  }
  assert.equal(r.findings.length, 0);
});

test('SEC18: a query parameter without a value stays only when it is a common switch (低-4)', () => {
  const r = createRedactor();
  const out = r.url('https://cdn.example.org/v/m.m3u8?QwErTyUiOpAsDfGh&jsmith&John-Smith&john_smith&embed&noCache&download');
  assert.ok(!/QwErTy|jsmith|John|john/.test(out), out);
  assert.equal(out, 'https://cdn.example.org/v/m.m3u8?{tok16-1}&{str6-2}&{str10-3}&{str10-4}&embed&noCache&download');
});

test('SEC18: language fields keep only tags in their canonical case; short names are shaped (低-5)', () => {
  const r = createRedactor();
  for (const [tag, out] of [['Li-Ming', 'Xx-Xxxx'], ['Wu-Chen', 'Xx-Xxxx'], ['Bob', 'Xxx'], ['Ann-Lee', 'Xxx-Xxx'], ['EN', 'XX'], ['en-gb', 'xx-xx']]) {
    assert.match(r.mpd(`<MPD><Period><AdaptationSet lang="${tag}"/></Period></MPD>`), new RegExp(`lang="${out}"`), tag);
  }
  for (const tag of ['en', 'en-GB', 'yue', 'yue-Hant-HK', 'zh-Hant-HK', 'es-419', 'sr-Latn-RS', 'zh-cmn-Hans-CN', 'und']) {
    assert.match(r.mpd(`<MPD><Period><AdaptationSet lang="${tag}"/></Period></MPD>`), new RegExp(`lang="${tag}"`), tag);
  }
  assert.match(r.hls('#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="A",LANGUAGE="Wu-Chen",URI="a.m3u8"\n'), /LANGUAGE="Xx-Xxxx"/);
});
