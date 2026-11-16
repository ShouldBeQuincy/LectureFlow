// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The do-not-handle list (src/lib/blocklist.js; user 2026-10-27: no YouTube
// in Chrome, no MOOC or paid course platform in any build): the list's
// shape, host and path matching, frames and requests of blocked sites
// taken out of a scan (src/lib/blockedframes.js) so that detection finds
// nothing there, profiles refused, the last check before a send.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  BLOCKLIST, BLOCKED_HOSTS, BLOCKED_PATHS, CATEGORY_YOUTUBE, CATEGORY_COURSES, blockedCategory, isBlockedUrl, isBlockedHost,
  hostCategory, originCategory, normalizeHost, normalizePath, wildcardCategory, wildcardTouchesBlocked, pathEntriesFor,
  pathPatternMayReach, schemaHostPattern, excludeMatchPatterns,
} from '../../src/lib/blocklist.js';
import { screenScan, contextCategory, documentCategory, titleOfFrame } from '../../src/lib/blockedframes.js';
import { scanFrame, frameTitle } from '../../src/content/scan.js';
import { checkProfile, blockedMatchErrors } from '../../src/lib/profile.js';
import { detectTracks } from '../../src/lib/detect.js';
import { sendBlockedCategory, sendable } from '../../src/lib/send.js';
import { popupView } from '../../src/lib/popupstate.js';
import { validate } from '../../src/lib/jsonschema.js';
import * as S from '../support/synth.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const schema = JSON.parse(fs.readFileSync(path.join(root, 'schema', 'profile.schema.json'), 'utf8'));
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
const clone = (o) => JSON.parse(JSON.stringify(o));

// ------------------------------------------------------------- the list

test('the list: two categories, well-formed entries, each once; YouTube and the platforms the user named', () => {
  assert.ok(BLOCKLIST.length > 90);
  const seen = new Set();
  for (const [category, entry] of BLOCKLIST) {
    assert.ok([CATEGORY_YOUTUBE, CATEGORY_COURSES].includes(category), entry);
    assert.match(entry, /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(\/[a-z0-9_-]+)?$/, entry);
    assert.ok(!seen.has(entry), `${entry} twice`);
    seen.add(entry);
  }
  // no entry is covered by another (a host under a host entry, a path under its host)
  for (const h of BLOCKED_HOSTS) assert.ok(!BLOCKED_HOSTS.some((b) => b !== h && h.endsWith(`.${b}`)), h);
  for (const e of BLOCKED_PATHS) assert.equal(hostCategory(e.host), '', `${e.host}${e.path}: the host is blocked whole`);
  for (const h of ['youtube.com', 'youtu.be', 'youtube-nocookie.com', 'youtubekids.com', 'youtube.googleapis.com', 'youtubeeducation.com', 'googlevideo.com']) assert.equal(hostCategory(h), CATEGORY_YOUTUBE, h);
  for (const e of ['udemy.com', 'coursera.org', 'edx.org', 'linkedin.com/learning', 'icourse163.org', 'xuetangx.com', 'study.163.com', 'ke.qq.com', 'bilibili.com/cheese', 'stepik.org', 'mooc.fi', 'mitxonline.mit.edu', 'openhpi.de', 'openlearning.com', 'smartedu.cn', 'edx-video.net', 'ximalaya.com', 'gumroad.com', 'skool.com', 'practicum.yandex.ru', 'swayam.gov.in', 'nptel.ac.in', 'byjus.com']) {
    assert.ok(BLOCKLIST.some(([c, x]) => c === CATEGORY_COURSES && x === e), e);
  }
  // left out by the user's decision (2026-10-27): schools' own recordings live there too, or "complaints first"
  for (const h of ['yuketang.cn', 'www.yuketang.cn', 'open.edu', 'www.open.edu', 'chaoxing.com', 'mooc1.chaoxing.com', 'zhihuishu.com', 'www.zhihuishu.com', 'mit.edu', 'ocw.mit.edu']) assert.equal(hostCategory(h), '', h);
  assert.equal(hostCategory('mitxonline.mit.edu'), CATEGORY_COURSES);
});

test('the list is one entry per line between the markers (internal/blocklist reads it that way)', () => {
  const src = fs.readFileSync(path.join(root, 'src', 'lib', 'blocklist.js'), 'utf8');
  const block = src.slice(src.indexOf('// blocklist:begin'), src.indexOf('// blocklist:end'));
  const lines = [...block.matchAll(/^\s*\['(youtube|courses)', '([^']+)'\],/gm)].map((m) => [m[1], m[2]]);
  assert.deepEqual(lines, BLOCKLIST.map(([c, e]) => [c, e]));
});

test('hosts: subdomains, case, trailing dot, IDN, port and user info; look-alikes are not blocked', () => {
  for (const h of ['youtube.com', 'www.youtube.com', 'm.youtube.com', 'music.youtube.com', 'youtu.be', 'www.youtube-nocookie.com', 'rr3---sn-abc.googlevideo.com', 'YOUTUBE.COM.', 'youtube.com..', 'ＹＯＵＴＵＢＥ.com', 'www.youtube.com:8443', 'user:pw@www.udemy.com', 'mp4-c.udemycdn.com', 'company.udemy.com', 'learning.oreilly.com', 'www.Coursera.Org']) {
    assert.equal(isBlockedHost(h), true, h);
  }
  for (const h of ['notyoutube.com', 'youtube.com.example.org', 'udemy.com.evil.example', 'evil-udemy.com', 'video.example.edu', 'tenant.cloud.panopto.eu', 'oreilly.com', 'www.oreilly.com', '163.com', 'qq.com', 'linkedin.com', 'www.linkedin.com', 'bilibili.com', 'xn--fiqs8s.example', '', '   ', 'http://']) {
    assert.equal(isBlockedHost(h), false, h);
  }
  // YouTube's embed host for schools (AUD3 信-4): the host and its subdomains, not look-alikes
  for (const h of ['youtubeeducation.com', 'www.youtubeeducation.com', 'WWW.YouTubeEducation.com.']) assert.equal(hostCategory(h), CATEGORY_YOUTUBE, h);
  for (const h of ['notyoutubeeducation.com', 'youtubeeducation.com.example.org', 'youtubeeducation.co']) assert.equal(hostCategory(h), '', h);
  assert.equal(blockedCategory('https://www.youtubeeducation.com/embed/abc?origin=https://lms.example.edu'), CATEGORY_YOUTUBE);
  assert.ok(excludeMatchPatterns().includes('*://*.youtubeeducation.com/*'), 'no content script there');
  assert.ok(new RegExp(schema.$defs.host.not.pattern, 'u').test('*.youtubeeducation.com'), 'a profile cannot match it');
  assert.equal(normalizeHost('WWW.YouTube.COM.:443'), 'www.youtube.com');
  assert.equal(normalizeHost('bücher.example'), 'xn--bcher-kva.example');
  assert.equal(hostCategory('www.udemy.com'), CATEGORY_COURSES);
});

test('URLs: host entries, path entries at a segment boundary, normalised paths; the rest of those sites is not blocked', () => {
  const blocked = {
    'https://www.youtube.com/watch?v=x': CATEGORY_YOUTUBE,
    'https://youtu.be/abc': CATEGORY_YOUTUBE,
    'https://WWW.UDEMY.COM.:443/course/x/': CATEGORY_COURSES,
    'https://evil.example@udemy.com/': CATEGORY_COURSES,
    'https://www.linkedin.com/learning': CATEGORY_COURSES,
    'https://www.linkedin.com/learning/': CATEGORY_COURSES,
    'https://www.linkedin.com/learning/some-course/intro': CATEGORY_COURSES,
    'https://LinkedIn.com/LEARNING/x': CATEGORY_COURSES,
    'https://www.linkedin.com//learning/x': CATEGORY_COURSES,
    'https://www.linkedin.com/%6Cearning/x': CATEGORY_COURSES,
    'https://www.linkedin.com/%2Flearning/x': CATEGORY_COURSES,
    'https://www.linkedin.com/feed/../learning/x': CATEGORY_COURSES,
    'https://www.linkedin.com/feed/%2E%2E/learning/x': CATEGORY_COURSES,
    'https://www.linkedin.com/learning;jsessionid=1': CATEGORY_COURSES,
    'https://www.bilibili.com/cheese/play/ep123': CATEGORY_COURSES,
    'https://m.bilibili.com/cheese/': CATEGORY_COURSES,
    'blob:https://www.udemy.com/0000-1111': CATEGORY_COURSES,
    'http://www.coursera.org:8080/learn/x': CATEGORY_COURSES,
  };
  for (const [u, c] of Object.entries(blocked)) {
    assert.equal(blockedCategory(u), c, u);
    assert.equal(isBlockedUrl(u), true, u);
  }
  for (const u of ['https://www.linkedin.com/', 'https://www.linkedin.com/feed/', 'https://www.linkedin.com/learning-login/', 'https://www.linkedin.com/in/learning', 'https://www.bilibili.com/video/BV1xx', 'https://www.bilibili.com/cheesecake', 'http://udemy.com.evil.example/course', 'https://www.oreilly.com/library/', 'https://lms.example.edu/courses/1/?u=https://www.udemy.com/', 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=1', 'not a url', 'ftp://www.udemy.com/', '']) {
    assert.equal(blockedCategory(u), '', u);
  }
  assert.equal(normalizePath('/A//B/./c/../D/'), '/a/b/d/');
  assert.equal(normalizePath('\\learning\\x'), '/learning/x');
  assert.equal(normalizePath(''), '/');
});

test('origins (location.ancestorOrigins) only block hosts blocked as a whole', () => {
  assert.equal(originCategory('https://www.coursera.org'), CATEGORY_COURSES);
  assert.equal(originCategory('https://www.youtube-nocookie.com'), CATEGORY_YOUTUBE);
  assert.equal(originCategory('https://www.linkedin.com'), '');
  assert.equal(originCategory('null'), '');
  assert.equal(originCategory('https://lms.example.edu'), '');
});

test('host patterns: under a blocked host, or a wildcard over one; path entries a pattern reaches', () => {
  for (const p of ['*.youtube.com', 'youtube.com', 'music.youtube.com', '*.googlevideo.com', '*.udemy.com', '*.oreilly.com', '*.163.com', '*.qq.com', '*.yandex.ru', 'learning.oreilly.com']) assert.ok(wildcardCategory(p), p);
  for (const p of ['*.panopto.eu', 'tenant.cloud.panopto.eu', 'oreilly.com', 'www.oreilly.com', '*.linkedin.com', 'www.bilibili.com', 'notudemy.com']) assert.equal(wildcardTouchesBlocked(p), false, p);
  assert.deepEqual(pathEntriesFor('*.linkedin.com').map((e) => e.path), ['/learning']);
  assert.deepEqual(pathEntriesFor('www.linkedin.com').map((e) => e.path), ['/learning']);
  assert.deepEqual(pathEntriesFor('m.bilibili.com').map((e) => e.path), ['/cheese']);
  assert.deepEqual(pathEntriesFor('*.panopto.eu'), []);
  assert.deepEqual(pathEntriesFor('notlinkedin.com'), []);
});

test('a path pattern keeps out of a blocked prefix only with a fixed first segment that is not it', () => {
  const reach = {
    '^/learning/': true,
    '^/learn': true,
    '^/(?:feed|learning)/': true,
    '^/feed/|^/x': true,
    '^/feed/|/learning': true,
    '^/l': true,
    '^/[a-z]+/': true,
    '.*': true,
    '/video/': true,
    '^/learningx?': true,
    '^/feed/': false,
    '^/feed$': false,
    '^/video/BV[0-9A-Za-z]+': false,
    '^/videos?/': false,
    '^/learningcenter/': false,
    '^/Panopto/Pages/Viewer\\.aspx$': false,
    '(': true,
  };
  for (const [p, want] of Object.entries(reach)) assert.equal(pathPatternMayReach(p, '/learning'), want, p);
  assert.equal(pathPatternMayReach('^/cheese', '/cheese'), true);
  assert.equal(pathPatternMayReach('^/cheesecake/', '/cheese'), false);
});

test('the schema refuses the hosts blocked as a whole with the same pattern', () => {
  assert.equal(schema.$defs.host.not.pattern, schemaHostPattern(), 'run the node one-liner in NOTES (ext blocklist) to regenerate');
  const re = new RegExp(schema.$defs.host.not.pattern, 'u');
  for (const h of ['youtube.com', '*.youtube.com', 'm.youtube.com', '*.m.youtube.com', '*.udemy.com', 'learning.oreilly.com', '*.oreilly.com', '*.yandex.ru']) assert.ok(re.test(h), h);
  for (const h of ['notyoutube.com', 'oreilly.com', 'www.oreilly.com', '*.linkedin.com', '*.panopto.eu', 'udemy.com.evil.example']) assert.ok(!re.test(h), h);
});

test('content scripts are excluded from every blocked host and path', () => {
  const pats = excludeMatchPatterns();
  assert.equal(pats.length, BLOCKED_HOSTS.length + BLOCKED_PATHS.length);
  for (const p of pats) assert.match(p, /^\*:\/\/\*\.[a-z0-9.-]+\/[a-z0-9/_-]*\*$/, p);
  assert.ok(pats.includes('*://*.youtube.com/*'));
  assert.ok(pats.includes('*://*.linkedin.com/learning*'));
});

// ---------------------------------------------------- frames of a scan

const frame = (over = {}) => ({ frameId: 0, url: 'https://lms.example.edu/courses/1', encryptedEvents: 0, elements: [], resources: [], reads: [], observed: [], opaque: [], ...over });
const el = (src, doc = 0, over = {}) => ({ tag: 'video', src, currentSrc: src, sources: [], mediaKeys: false, videoWidth: 1280, videoHeight: 720, duration: 60, readyState: 4, doc, ...over });
const res = (url, doc = 0) => ({ url, initiatorType: 'video', responseStatus: 200, doc });

test('an embedded course platform (LTI in an LMS): its frame, what it embeds and its requests go, whichever CDN serves them', async () => {
  const LMS = 'https://lms.example.edu';
  const frames = [
    frame({ url: `${LMS}/courses/1/modules`, docs: [{ url: `${LMS}/courses/1/modules`, parent: -1, history: [] }], ancestors: [], elements: [el(`${LMS}/files/talk.m4a`, 0, { tag: 'audio', videoWidth: null, videoHeight: null })], resources: [{ url: 'https://www.coursera.org/learn/x/lecture/1', initiatorType: 'iframe', responseStatus: 200, doc: 0 }], opaque: ['https://www.coursera.org/learn/x/lecture/1'] }),
    frame({ frameId: 5, url: 'https://www.coursera.org/learn/x/lecture/1', docs: [{ url: 'https://www.coursera.org/learn/x/lecture/1', parent: -1, history: [] }], ancestors: [LMS], elements: [el('https://cdn.example.net/v/lecture.mp4')], resources: [res('https://cdn.example.net/v/lecture.mp4')] }),
    // a player the course platform embeds, on a general host
    frame({ frameId: 7, url: 'https://player.example.net/embed/1', ancestors: ['https://www.coursera.org', LMS], elements: [el('https://cdn.example.net/v/other.mp4')] }),
  ];
  const webRequests = [
    { url: 'https://cdn.example.net/v/lecture.mp4', frameId: 5, resourceType: 'media', statusCode: 200, contentType: 'video/mp4', contentLength: 100, initiator: 'https://www.coursera.org' },
    { url: 'https://cdn.example.net/v/third.mp4', frameId: 11, resourceType: 'media', statusCode: 200, contentType: 'video/mp4', contentLength: 100, initiator: 'https://www.coursera.org' },
    { url: 'https://rr1---sn-x.googlevideo.com/videoplayback', frameId: 0, resourceType: 'media', statusCode: 200, contentType: 'video/mp4', contentLength: 100, initiator: LMS },
    { url: `${LMS}/files/talk.m4a`, frameId: 0, resourceType: 'media', statusCode: 200, contentType: 'audio/mp4', contentLength: 50, initiator: LMS },
  ];
  const s = screenScan({ frames, webRequests });
  assert.deepEqual(s.frames.map((f) => f.frameId), [0]);
  assert.deepEqual([...s.blockedFrameIds].sort(), [5, 7]);
  assert.deepEqual(s.categories, [CATEGORY_COURSES]);
  assert.equal(s.frameCategory(5), CATEGORY_COURSES);
  assert.equal(s.frameCategory(0), '');
  assert.deepEqual(s.webRequests.map((w) => w.url), [`${LMS}/files/talk.m4a`]);
  // nothing of the screening is left for detection, the report or a fixture
  for (const f of s.frames) {
    assert.ok(!('docs' in f) && !('ancestors' in f), 'docs, ancestors');
    assert.ok(f.elements.every((e) => !('doc' in e)) && f.resources.every((r) => !('doc' in r)), 'doc');
    assert.deepEqual(f.opaque, []);
    assert.deepEqual(f.resources, [], 'the platform page’s own load in the LMS page’s resource timing');
  }
  assert.ok(s.webRequests.every((w) => !('initiator' in w)));
  const files = { [`${LMS}/files/talk.m4a`]: { contentType: 'audio/mp4', size: 50 }, 'https://cdn.example.net/v/lecture.mp4': { contentType: 'video/mp4', size: 100 }, 'https://cdn.example.net/v/other.mp4': { contentType: 'video/mp4', size: 100 } };
  const r = await detectTracks({ frames: s.frames, webRequests: s.webRequests }, S.fakeIo({}, files));
  assert.deepEqual(r.tracks.map((t) => t.url), [`${LMS}/files/talk.m4a`]);
  // without the screening the same scan would offer the platform's video
  const unscreened = await detectTracks({ frames: frames.map(({ docs, ancestors, ...f }) => f), webRequests }, S.fakeIo({}, files));
  assert.ok(unscreened.tracks.some((t) => t.url === 'https://cdn.example.net/v/lecture.mp4'));
});

test('a blocked top page, by its address or by what the document showed before (a single-page site)', () => {
  const top = screenScan({ frames: [frame({ url: 'https://www.udemy.com/course/x/learn/lecture/1', elements: [el('https://cdn.example.net/a.mp4')] })] });
  assert.equal(top.frameCategory(0), CATEGORY_COURSES);
  assert.deepEqual(top.frames, []);
  const spa = screenScan({ frames: [frame({ url: 'https://www.bilibili.com/video/BV1', docs: [{ url: 'https://www.bilibili.com/video/BV1', parent: -1, history: ['https://www.bilibili.com/cheese/play/ep1', 'https://www.bilibili.com/video/BV1'] }], elements: [el('https://upos.example.net/a.mp4')] })] });
  assert.equal(spa.frameCategory(0), CATEGORY_COURSES);
  const fine = screenScan({ frames: [frame({ url: 'https://www.bilibili.com/video/BV1', docs: [{ url: 'https://www.bilibili.com/video/BV1', parent: -1, history: ['https://www.bilibili.com/video/BV0'] }] })] });
  assert.equal(fine.frameCategory(0), '');
  assert.equal(documentCategory({ url: 'https://lms.example.edu/', history: ['https://www.youtube.com/watch?v=1'] }), CATEGORY_YOUTUBE);
});

test('same-origin child documents scanned with their parent: a blocked one and those below it drop out', () => {
  const L = 'https://www.linkedin.com';
  const s = screenScan({
    frames: [
      frame({
        url: `${L}/feed/`,
        docs: [
          { url: `${L}/feed/`, parent: -1, history: [] },
          { url: `${L}/learning/embed/x`, parent: 0, history: [] },
          { url: `${L}/inner/frame`, parent: 1, history: [] },
          { url: `${L}/other/frame`, parent: 0, history: [] },
        ],
        elements: [el(`${L}/a.mp4`, 0), el(`${L}/b.mp4`, 1), el(`${L}/c.mp4`, 2), el(`${L}/d.mp4`, 3)],
        resources: [res(`${L}/b.m3u8`, 1), res(`${L}/d.m3u8`, 3)],
      }),
      // a cross-origin frame inside that LinkedIn page: its origin embeds a blocked document
      frame({ frameId: 3, url: 'https://player.example.net/x', ancestors: [L] }),
    ],
    webRequests: [{ url: 'https://media.example.net/learning.mp4', frameId: 0, initiator: L }],
  });
  assert.deepEqual(s.frames.map((f) => f.frameId), [0]);
  assert.deepEqual(s.frames[0].elements.map((e) => e.src), [`${L}/a.mp4`, `${L}/d.mp4`]);
  assert.deepEqual(s.frames[0].resources.map((r) => r.url), [`${L}/d.m3u8`]);
  assert.deepEqual(s.webRequests, [], 'requests from a blocked origin of this tab');
  assert.deepEqual(s.categories, [CATEGORY_COURSES]);
});

test('a scan with nothing blocked passes through unchanged but for the screening fields', () => {
  const frames = S.genericFrames();
  const s = screenScan({ frames: frames.map((f) => ({ ...f, docs: [{ url: f.url, parent: -1, history: [f.url] }], ancestors: [] })), webRequests: S.genericWebRequests.map((w) => ({ ...w, initiator: 'https://lectures.example.edu' })) });
  assert.deepEqual(s.frames, frames);
  assert.deepEqual(s.webRequests, S.genericWebRequests);
  assert.deepEqual(s.categories, []);
});

test('in-page and observer frames: their address, the tab’s and the origins they are embedded in', () => {
  assert.equal(contextCategory({ frameUrl: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Embed.aspx?id=1', topUrl: 'https://lms.example.edu/' }), '');
  assert.equal(contextCategory({ frameUrl: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Embed.aspx?id=1', topUrl: 'https://www.udemy.com/course/x' }), CATEGORY_COURSES);
  assert.equal(contextCategory({ frameUrl: 'https://tenant.cloud.panopto.eu/x', ancestors: ['https://player.example.net', 'https://www.youtube.com'] }), CATEGORY_YOUTUBE);
  assert.equal(contextCategory({ frameUrl: 'https://www.linkedin.com/learning/x' }), CATEGORY_COURSES);
  assert.equal(contextCategory({ frameUrl: 'https://tenant.cloud.panopto.eu/x', ancestors: ['https://www.linkedin.com'] }), '', 'an origin has no path');
  assert.equal(contextCategory({}), '');
});

// ------------------------------------------------------------ profiles

function profileWith(match, extra = {}) {
  const p = clone(panopto);
  p.match = match;
  return { ...p, ...extra };
}

test('profiles: hosts or media hosts on a blocked site are refused, first and plainly, by the extension and the schema', () => {
  for (const [field, h, category] of [['hosts', '*.youtube.com', CATEGORY_YOUTUBE], ['hosts', '*.udemy.com', CATEGORY_COURSES], ['hosts', 'learning.oreilly.com', CATEGORY_COURSES], ['hosts', '*.oreilly.com', CATEGORY_COURSES], ['mediaHosts', '*.googlevideo.com', CATEGORY_YOUTUBE], ['mediaHosts', 'mp4-c.udemycdn.com', CATEGORY_COURSES]]) {
    const match = field === 'hosts' ? { hosts: [h] } : { hosts: ['video.example.edu'], mediaHosts: [h] };
    const errs = checkProfile(schema, profileWith(match));
    assert.ok(errs.length > 0, h);
    assert.deepEqual(errs[0].blocked, { host: h, category }, h);
    assert.equal(errs[0].path, `/match/${field}/0`);
    assert.match(errs[0].message, /LectureFlow does not handle/);
    assert.ok(!errs.some((e) => e.message === 'must not match'), 'the schema’s own refusal is not repeated');
    // the schema alone refuses it too (an editor validating against it)
    assert.ok(validate(schema, profileWith(match)).some((e) => e.message === 'must not match'), h);
  }
  assert.deepEqual(checkProfile(schema, profileWith({ hosts: ['video.example.edu'] })), []);
});

test('profiles: a host with a blocked path needs a pathPattern that keeps out of it (page, list, sessionInfo)', () => {
  const paths = (p) => blockedMatchErrors(p).map((e) => e.path);
  assert.deepEqual(paths(profileWith({ hosts: ['www.linkedin.com'] })), ['/match/pathPattern']);
  assert.deepEqual(paths(profileWith({ hosts: ['*.linkedin.com'], pathPattern: '^/learning/' })), ['/match/pathPattern']);
  assert.deepEqual(paths(profileWith({ hosts: ['*.linkedin.com'], pathPattern: '^/(feed|learning)/' })), ['/match/pathPattern']);
  assert.deepEqual(paths(profileWith({ hosts: ['www.bilibili.com'], pathPattern: '^/video/' })), []);
  const err = blockedMatchErrors(profileWith({ hosts: ['www.bilibili.com'], pathPattern: '.*' }))[0];
  assert.deepEqual(err.blocked, { host: 'bilibili.com/cheese', category: CATEGORY_COURSES });
  assert.match(err.message, /bilibili\.com\/cheese/);
  // the list page and the sessionInfo request of such a profile too
  const p = profileWith({ hosts: ['www.linkedin.com'], pathPattern: '^/feed/' });
  p.inpage = { ...clone(panopto.inpage), list: { ...clone(panopto.inpage.list), pathPattern: '^/learning/' } };
  p.sessionInfo = { ...clone(panopto.sessionInfo), path: '/learning/api/x' };
  assert.deepEqual(paths(p), ['/inpage/list/pathPattern', '/sessionInfo/path']);
  // checkProfile reports them as well (CI and the extension's import)
  assert.ok(checkProfile(schema, profileWith({ hosts: ['www.linkedin.com'] })).some((e) => e.blocked && e.path === '/match/pathPattern'));
  // whatever the file holds
  for (const bad of [null, 1, 'x', {}, { match: null }, { match: { hosts: 'x' } }, { match: { hosts: [1, null] } }]) assert.deepEqual(blockedMatchErrors(bad), []);
});

// --------------------------------------------------------- sending

test('the last check before a send: the page, the tab’s page or a ticked track on a blocked site', () => {
  const result = { status: 'ok', pageUrl: 'https://lms.example.edu/x', tracks: [{ id: 'a', url: 'https://cdn.example.net/a.mp4', kind: 'video', protected: false }, { id: 'y', url: 'https://rr1.googlevideo.com/videoplayback', kind: 'video', protected: false }] };
  assert.equal(sendBlockedCategory(result, ['a']), '');
  assert.equal(sendBlockedCategory(result, ['a', 'y']), CATEGORY_YOUTUBE);
  assert.equal(sendBlockedCategory(result, ['a'], 'https://www.coursera.org/learn/x'), CATEGORY_COURSES);
  assert.equal(sendBlockedCategory({ ...result, pageUrl: 'https://www.linkedin.com/learning/x' }, ['a']), CATEGORY_COURSES);
  assert.equal(sendable(result.tracks[1]), false);
  assert.equal(sendBlockedCategory(null, null), '');
});

test('the popup’s unsupported view carries the category', () => {
  assert.deepEqual(popupView({ detection: { status: 'unsupported', category: 'youtube' } }), { view: 'unsupported', reason: 'youtube' });
  assert.deepEqual(popupView({ detection: { status: 'unsupported', category: 'courses' } }), { view: 'unsupported', reason: 'courses' });
  assert.deepEqual(popupView({ detection: { status: 'unsupported' } }), { view: 'unsupported', reason: '' });
});

// ------------------------------------------------------ the page title

test('the page title (generic detection, T048) is read only after the blocklist checks, never from a blocked site', () => {
  const page = 'https://lectures.example.edu/watch/7';
  const read = (o = {}) => ({ url: page, title: 'Week 7 | Example', history: [page], ancestors: [], ...o });
  assert.equal(titleOfFrame(read(), { scannedUrl: page, topUrl: page }), 'Week 7 | Example');
  assert.equal(titleOfFrame(read({ url: `${page}#t=60` }), { scannedUrl: page }), 'Week 7 | Example', 'a fragment is the same document');
  // the frame itself, what its document showed, the tab's page, an origin it sits inside
  for (const [o, topUrl] of [
    [{ url: 'https://www.youtube.com/watch?v=x' }, ''],
    [{ url: 'https://www.coursera.org/learn/x/lecture/1' }, ''],
    [{ history: ['https://www.linkedin.com/learning/x', page] }, ''],
    [{ ancestors: ['https://www.udemy.com'] }, ''],
    [{}, 'https://www.edx.org/course/x'],
  ]) {
    const r = read(o);
    assert.equal(titleOfFrame(r, { scannedUrl: r.url, topUrl }), '', JSON.stringify([o, topUrl]));
  }
  // not the document the screened scan saw (it navigated in between), or nothing usable
  assert.equal(titleOfFrame(read({ url: 'https://lectures.example.edu/watch/8' }), { scannedUrl: page }), '');
  for (const bad of [null, undefined, 'x', {}, { url: page }, { url: page, title: 1 }, read({ url: 'javascript:alert(1)' })]) {
    assert.equal(titleOfFrame(bad, { scannedUrl: page }), '');
  }
});

test('the scan reads no title; frameTitle reads it only while the frame still shows the screened address (SEC15 L3)', () => {
  assert.ok(!scanFrame.toString().includes('document.title'), 'scanFrame reads no title');
  const keys = ['location', 'document', 'performance', 'navigation'];
  const saved = Object.fromEntries(keys.map((k) => [k, Object.getOwnPropertyDescriptor(globalThis, k)]));
  const def = (k, v) => Object.defineProperty(globalThis, k, { value: v, configurable: true, writable: true });
  let reads = 0;
  try {
    def('location', { href: 'https://lectures.example.edu/watch/7#t=5', ancestorOrigins: ['https://lms.example.edu'] });
    def('document', {
      get title() {
        reads += 1;
        return '  Week 7 \n | Example  ';
      },
    });
    def('performance', { getEntriesByType: () => [{ name: 'https://lectures.example.edu/watch/7' }] });
    def('navigation', undefined);
    assert.deepEqual(frameTitle('https://lectures.example.edu/watch/7'), { url: 'https://lectures.example.edu/watch/7#t=5', title: 'Week 7 | Example', history: ['https://lectures.example.edu/watch/7'], ancestors: ['https://lms.example.edu'] });
    assert.equal(reads, 1);
    // The page moved on (to a blocked path of the same site, say) after the
    // scan: the title is not read at all, not merely dropped later.
    def('location', { href: 'https://www.linkedin.com/learning/x', ancestorOrigins: [] });
    for (const expected of ['https://www.linkedin.com/feed/', undefined, '', 42]) {
      const r = frameTitle(expected);
      assert.equal(r.title, '', String(expected));
    }
    assert.equal(reads, 1, 'document.title was not touched');
  } finally {
    for (const k of keys) {
      if (saved[k]) Object.defineProperty(globalThis, k, saved[k]);
      else delete globalThis[k];
    }
  }
});
