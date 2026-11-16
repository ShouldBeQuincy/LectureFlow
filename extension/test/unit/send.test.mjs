// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Sending (SPEC-ext 用户流程 3–4): the default ticks, at most one audio and
// one video, which URLs' cookies are read, the enqueue and refresh
// requests; and the popup's choice of view (five states plus 续传).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  defaultSelection, toggleSelection, selectionError, cookieTargets, toBridgeCookie, mergeCookies, buildEnqueue, buildRefresh,
  checkOutcome, refererFor, trackName, trackNames, listedTracks, groupOf, presetSelection, accessOffer,
  cookiePlan, cookieAllowed, cookieGoesTo, mayShareCookies, quietHostsOf, sameSiteProven, withAnswerDomains, ANSWER_DOMAINS,
} from '../../src/lib/send.js';
import { popupView, progressOf, isFinal, sendSticks, SEND_ROOM } from '../../src/lib/popupstate.js';
import { isBlockedHost, isBlockedUrl, wildcardTouchesBlocked } from '../../src/lib/blocklist.js';

const msg = (k) => ({ kind_video: 'Video', kind_audio: 'Audio', kind_unknown: 'Unknown' })[k] || k;

const V1 = { id: 'v1', url: 'https://cdn.example.net/v1.mp4', kind: 'video', format: 'file', sizeBytes: 100, protected: false, quality: '720p' };
const V2 = { id: 'v2', url: 'https://cdn.example.net/hls/master.m3u8', kind: 'video', format: 'hls', sizeBytes: -1, protected: false };
const A1 = { id: 'a1', url: 'https://cdn.example.net/a1.m4a', kind: 'audio', format: 'file', sizeBytes: 50, protected: false, language: 'en' };
const A2 = { id: 'a2', url: 'https://cdn.example.net/a2.m4a', kind: 'audio', format: 'file', sizeBytes: 40, protected: false };
const U1 = { id: 'u1', url: 'https://cdn.example.net/u1', kind: 'unknown', format: 'hls', sizeBytes: -1, protected: false, unreadable: true };
const P1 = { id: 'p1', url: 'https://cdn.example.net/drm.mpd#lf=video::0', kind: 'video', format: 'dash', sizeBytes: -1, protected: true, selector: 'video::0' };

test('three choices instead of ticking (user decision): 主要音视频 / 双机位 / 仅音频', () => {
  const presets = { full: ['podcast'], audio: ['primary-audio'] };
  const S1 = { ...V1, id: 's1', role: 'screen' };
  const S2 = { ...V2, id: 's2', role: 'screen' };
  const POD = { ...V1, id: 'pod', role: 'podcast' };
  const AUD = { ...A1, id: 'aud', role: 'primary-audio' };
  // Panopto with the full MP4: main is the MP4 alone
  const withPod = [POD, AUD, S1, S2];
  assert.deepEqual(presetSelection(withPod, 'main', presets), ['pod']);
  assert.deepEqual(presetSelection(withPod, 'dual', presets), ['aud', 's1', 's2'], 'audio and two pictures, the MP4 is not a picture');
  assert.deepEqual(presetSelection(withPod, 'audio', presets), ['aud']);
  // without it: audio + the first picture; one picture → no 双机位
  assert.deepEqual(presetSelection([AUD, S1], 'main', presets), ['aud', 's1']);
  assert.equal(presetSelection([AUD, S1], 'dual', presets), null);
  assert.equal(presetSelection([POD, S1], 'audio', presets), null, 'no separate audio');
  // generic: the first audio track is the audio; a muxed file alone is main
  assert.deepEqual(presetSelection([V1, A1, A2], 'main', null), ['a1', 'v1']);
  assert.deepEqual(presetSelection([V1], 'main', null), ['v1']);
  assert.deepEqual(presetSelection([P1, V1, V2, A1], 'dual', null), ['a1', 'v1', 'v2'], 'protected tracks never count');
  assert.equal(presetSelection([P1], 'main', null), null);
  // the starting ticks: main, or nothing when the platform says restricted
  assert.deepEqual(defaultSelection(withPod, { presets }), ['pod']);
  assert.deepEqual(defaultSelection(withPod, { presets, downloadPolicy: 'disabled' }), []);
  assert.deepEqual(defaultSelection([V1, A1], {}), ['a1', 'v1']);
  // 双机位 sends one audio and two pictures; the advanced list keeps one
  assert.equal(selectionError(withPod, ['aud', 's1', 's2'], { maxVideo: 2 }), '');
  assert.equal(selectionError(withPod, ['aud', 's1', 's2']), 'too_many_video');
});

test('ticking: video and audio are each single choice; protected never ticks', () => {
  const all = [V1, V2, A1, A2, U1, P1];
  let s = ['v1', 'a1'];
  s = toggleSelection(all, s, 'v2', true);
  assert.deepEqual(s.sort(), ['a1', 'v2']);
  s = toggleSelection(all, s, 'u1', true);
  assert.deepEqual(s.sort(), ['a1', 'u1'], 'unknown counts with video');
  s = toggleSelection(all, s, 'a2', true);
  assert.deepEqual(s.sort(), ['a2', 'u1']);
  s = toggleSelection(all, s, 'p1', true);
  assert.deepEqual(s.sort(), ['a2', 'u1']);
  s = toggleSelection(all, s, 'a2', false);
  assert.deepEqual(s, ['u1']);
  assert.equal(groupOf(U1), 'video');
});

test('what can be sent: at least one, known, unprotected, one audio and one video at most', () => {
  const all = [V1, V2, A1, A2, P1];
  assert.equal(selectionError(all, ['v1', 'a1']), '');
  assert.equal(selectionError(all, []), 'empty');
  assert.equal(selectionError(all, ['zz']), 'unknown_track');
  assert.equal(selectionError(all, ['p1']), 'protected');
  assert.equal(selectionError(all, ['v1', 'v2']), 'too_many_video');
  assert.equal(selectionError(all, ['a1', 'a2']), 'too_many_audio');
  assert.equal(selectionError([{ ...V1, url: 'https://rr1.googlevideo.com/x.mp4' }], ['v1']), 'protected', 'a do-not-handle host is never sent');
});

test('cookies: of the ticked tracks’ URLs only (cookieUrls ["track"], the default); none for []', () => {
  assert.deepEqual(cookieTargets({}, [V1, A1]), ['https://cdn.example.net/v1.mp4', 'https://cdn.example.net/a1.m4a']);
  assert.deepEqual(cookieTargets(undefined, [V1, V1]), ['https://cdn.example.net/v1.mp4']);
  assert.deepEqual(cookieTargets({ cookieUrls: ['track'] }, [P1]), ['https://cdn.example.net/drm.mpd'], 'without the fragment');
  assert.deepEqual(cookieTargets({ cookieUrls: [] }, [V1, A1]), []);
});

test('which cookies a send may hand over (AUD3 中-1): the page’s site, a profile’s declared hosts, and only what an embedded player’s own requests carried', () => {
  // hosts that could share a cookie at all (only decides whether to read)
  assert.equal(mayShareCookies('lms.uni.example', 'lms.uni.example'), true);
  assert.equal(mayShareCookies('www.uni.example', 'media.uni.example'), true, 'a common parent of two labels');
  assert.equal(mayShareCookies('attacker.example', 'lms.uni.example'), false);
  assert.equal(mayShareCookies('www.uni.example', 'media.uni.example.'), true, 'a trailing dot is the same name');
  assert.equal(mayShareCookies('a.example', 'b.example'), false, 'a common top-level label only');
  assert.equal(mayShareCookies('a.test', 'b.org'), false);
  assert.equal(mayShareCookies('10.0.0.1', '10.0.0.2'), false, 'addresses are never a common domain');
  assert.equal(mayShareCookies('127.0.0.1', '127.0.0.1'), true);
  assert.equal(mayShareCookies('', 'x.example'), false);
  // the browser's domain matching
  const hostOnly = { domain: 'media.uni.example', hostOnly: true };
  const site = { domain: '.uni.example', hostOnly: false };
  assert.equal(cookieGoesTo(hostOnly, 'media.uni.example'), true);
  assert.equal(cookieGoesTo(hostOnly, 'www.uni.example'), false);
  assert.equal(cookieGoesTo(site, 'www.uni.example'), true);
  assert.equal(cookieGoesTo(site, 'uni.example'), true);
  assert.equal(cookieGoesTo(site, 'notuni.example'), false, 'a suffix that is not a whole label');
  // a host-only cookie never goes to a name under its host (SEC46 信-3, M8)
  const apexOnly = { domain: 'uni.example', hostOnly: true };
  assert.equal(cookieGoesTo(apexOnly, 'uni.example'), true);
  assert.equal(cookieGoesTo(apexOnly, 'www.uni.example'), false);
  assert.equal(cookieGoesTo({ domain: '.uni.example', hostOnly: true }, 'www.uni.example'), false, 'whatever the leading dot says');

  const tr = (id, url, sources, frameIds = [0]) => ({ id, url, kind: 'video', format: 'file', sizeBytes: 1, protected: false, sources, frameIds });
  // generic detection on a page that names another site's media: nothing to read
  // thirdPartyCookies: what the detection says the browser sends an embedded frame (the Chrome build)
  const evil = { status: 'ok', pageUrl: 'https://attacker.example/x', request: {}, profile: null, frameHosts: { 0: 'attacker.example' }, thirdPartyCookies: 'shared' };
  const named = tr('n', 'https://lms.uni.example/pluginfile.php/1/a.mp4', ['element', 'webrequest']);
  assert.deepEqual(cookiePlan(evil, [named], evil.pageUrl), []);
  // the page's own site: the cookies it also sends to the page
  const own = { ...evil, pageUrl: 'https://lms.uni.example/view.php', frameHosts: { 0: 'lms.uni.example' } };
  assert.deepEqual(cookiePlan(own, [named], own.pageUrl), [{ url: named.url, all: false, topHost: 'lms.uni.example', siteHosts: ['lms.uni.example'], frameHosts: ['lms.uni.example'], thirdParty: 'shared', grantedHosts: [] }]);
  // ... but only when a frame on the page's site referenced or requested it (SEC46 低-1, B1):
  // a scanned frame of another site that requested an LMS address gets nothing
  const viaEvilFrame = tr('e', named.url, ['webrequest'], [9]);
  assert.deepEqual(cookiePlan({ ...own, frameHosts: { 0: 'lms.uni.example', 9: 'evil.example' } }, [viaEvilFrame], own.pageUrl), []);
  assert.deepEqual(cookiePlan({ ...own, frameHosts: { 0: 'lms.uni.example' } }, [viaEvilFrame], own.pageUrl), [], 'nor one the scan did not read');
  // an LMS page, another site's player in frame 5 whose request the browser made
  const embed = { ...evil, pageUrl: 'https://lms.uni.example/course/1', frameHosts: { 0: 'lms.uni.example', 5: 'video.example.com' } };
  const played = tr('p', 'https://video.example.com/m/1.mp4', ['webrequest'], [5]);
  const written = tr('w', 'https://video.example.com/m/2.mp4', ['element'], [5]);
  assert.deepEqual(cookiePlan(embed, [played, written], embed.pageUrl), [{ url: played.url, all: false, topHost: '', siteHosts: [], frameHosts: ['video.example.com'], thirdParty: 'shared', grantedHosts: [] }]);
  // Firefox (SEC46 低-2): the entry stays, so the tab's partition is read; ② hands over no unpartitioned cookie
  assert.deepEqual(cookiePlan({ ...embed, thirdPartyCookies: 'partitioned' }, [played], embed.pageUrl), [{ url: played.url, all: false, topHost: '', siteHosts: [], frameHosts: ['video.example.com'], thirdParty: 'partitioned', grantedHosts: [] }]);
  assert.equal(cookiePlan({ ...embed, thirdPartyCookies: undefined }, [played], embed.pageUrl)[0].thirdParty, 'partitioned', 'a result that does not say: partitioned');
  // a profile's declared hosts: everything, as before; other hosts as generic
  const profiled = { ...evil, request: { cookieUrls: ['track'] }, profile: { id: 'p', origins: ['https://*.platform.example/*'] } };
  const declared = tr('d', 'https://cdn.platform.example/v.mp4', ['element']);
  assert.deepEqual(cookiePlan(profiled, [declared, named], 'https://lecture.platform.example/x'), [{ url: declared.url, all: true }]);
  assert.deepEqual(cookiePlan({ ...profiled, request: { cookieUrls: [] } }, [declared], 'https://lecture.platform.example/x'), [], 'cookieUrls [] reads none');
  assert.deepEqual(cookiePlan(null, [declared], ''), [], 'no top page, no frames: nothing');
  // a host whose media a profile says need no cookie (Panopto's cookieUrls []): none, on any page
  const quietHosts = quietHostsOf([
    { profile: { match: { hosts: ['*.panopto.eu'], mediaHosts: ['*.cdn.example'] }, request: { cookieUrls: [] } } },
    { profile: { match: { hosts: ['*.platform.example'] }, request: { cookieUrls: ['track'] } } },
    { profile: { match: { hosts: ['*.defaulted.example'] } } },
  ]);
  assert.deepEqual(quietHosts, ['*.cdn.example', '*.panopto.eu']);
  const tenantPage = { ...evil, pageUrl: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Home.aspx', frameHosts: { 0: 'tenant.cloud.panopto.eu' } };
  const tenantMp4 = tr('t', 'https://tenant.cloud.panopto.eu/Panopto/Content/x.mp4', ['element', 'webrequest']);
  assert.equal(cookiePlan(tenantPage, [tenantMp4], tenantPage.pageUrl).length, 1, 'without the profiles’ word, the page’s own site');
  assert.deepEqual(cookiePlan(tenantPage, [tenantMp4], tenantPage.pageUrl, { quietHosts }), []);

  // cookie by cookie
  const lax = (domain, extra = {}) => ({ name: 'c', domain, hostOnly: !domain.startsWith('.'), sameSite: 'lax', ...extra });
  const none = (domain) => lax(domain, { sameSite: 'no_restriction' });
  const fromTop = { url: 'x', all: false, topHost: 'www.uni.example', siteHosts: ['www.uni.example'], frameHosts: [] };
  assert.equal(cookieAllowed(lax('.uni.example'), fromTop), true, 'the page also gets it: same site, any SameSite');
  assert.equal(cookieAllowed(lax('media.uni.example'), fromTop), false);
  assert.equal(cookieAllowed(none('media.uni.example'), fromTop), false, 'SameSite=None alone is not enough');
  assert.equal(cookieAllowed(lax('.uni.example'), { ...fromTop, siteHosts: [] }), false, 'no frame on the page’s site asked for it');
  assert.equal(cookieAllowed(lax('uni.example'), fromTop), false, 'the apex’s host-only cookie does not go to www (M8)');
  // a frame of another site under a public suffix of two labels: the page's own cookie stays
  const githubIo = { url: 'x', all: false, topHost: 'victim.github.io', siteHosts: ['evil.github.io'], frameHosts: [] };
  assert.equal(cookieAllowed(lax('victim.github.io'), githubIo), false, 'goes to the page, not to the frame that asked');
  assert.equal(cookieAllowed(lax('.lms.uni.example'), { url: 'x', all: false, topHost: 'www.lms.uni.example', siteHosts: ['media.uni.example'], frameHosts: [] }), false);
  const fromFrame = { url: 'x', all: false, topHost: '', frameHosts: ['video.example.com'], thirdParty: 'shared' };
  assert.equal(cookieAllowed(none('video.example.com'), fromFrame), true);
  assert.equal(cookieAllowed(none('video.example.com'), { ...fromFrame, thirdParty: 'partitioned' }), false, 'Firefox: the tab’s partition only (SEC46 低-2)');
  assert.equal(cookieAllowed(none('video.example.com'), { ...fromFrame, thirdParty: undefined }), false);
  // ① is not affected
  assert.equal(cookieAllowed(lax('.uni.example'), { ...fromTop, thirdParty: 'partitioned' }), true);
  assert.equal(cookieAllowed(lax('video.example.com'), fromFrame), false, 'a cross-site frame never sent it Lax');
  assert.equal(cookieAllowed(lax('video.example.com', { sameSite: 'unspecified' }), fromFrame), false);
  assert.equal(cookieAllowed(lax('anything.example'), { url: 'x', all: true }), true);
  assert.equal(cookieAllowed(lax('anything.example'), undefined), false, 'a URL without an entry: none');
});

test('the track host’s own host-only cookie, when a domain cookie proves it on the page’s or frame’s site (SEC46 低-3)', () => {
  // the proof: a domain cookie on the hosts' nearest common parent, each host at most one label below it
  assert.equal(sameSiteProven('www.uni.example', 'media.uni.example', ['uni.example']), true);
  assert.equal(sameSiteProven('uni.example', 'media.uni.example', ['uni.example']), true, 'the parent itself');
  assert.equal(sameSiteProven('media.uni.example', 'media.uni.example', []), true, 'the same host');
  assert.equal(sameSiteProven('www.uni.example', 'media.uni.example', []), false, 'no domain cookie: no proof');
  assert.equal(sameSiteProven('www.uni.example', 'media.uni.example', ['media.uni.example']), false);
  assert.equal(sameSiteProven('www.lms.uni.example', 'media.uni.example', ['uni.example']), false, 'two labels below: a public suffix could sit in between');
  assert.equal(sameSiteProven('bucketx.s3.storage.example', 'victim.s3.storage.example', ['storage.example']), false, 'a parent above the nearest one proves nothing');
  // a public suffix never holds a domain cookie (the browser refuses it), so evil.github.io /
  // a.co.uk have none to show; the list below is what a browser can hold
  assert.equal(sameSiteProven('evil.github.io', 'victim.github.io', []), false);
  assert.equal(sameSiteProven('a.co.uk', 'b.co.uk', ['b.co.uk']), false);
  assert.equal(sameSiteProven('a.example', 'b.example', ['example']), false, 'a single label is never a cookie domain');
  assert.equal(sameSiteProven('10.0.0.5', '10.0.0.6', ['0.0.6']), false, 'addresses');

  // the answer's cookies carry its domain cookies' Domain values, not enumerable
  const answer = withAnswerDomains([
    { name: 'sso', domain: '.uni.example', hostOnly: false, sameSite: 'lax' },
    { name: 'media', domain: 'media.uni.example', hostOnly: true, sameSite: 'lax' },
    { name: 'mediaNone', domain: 'media.uni.example', hostOnly: true, sameSite: 'no_restriction' },
  ]);
  assert.deepEqual(answer[1][ANSWER_DOMAINS], ['uni.example']);
  assert.deepEqual(Object.keys(answer[1]), ['name', 'domain', 'hostOnly', 'sameSite']);
  assert.equal(JSON.stringify(answer).includes('answerDomains'), false);
  const [, media, mediaNone] = answer;
  const url = 'https://media.uni.example/v/1.mp4';
  // ① the page www.uni.example (granted) shows the track: its host-only cookies, any SameSite
  const top = { url, all: false, topHost: 'www.uni.example', siteHosts: ['www.uni.example'], frameHosts: [], thirdParty: 'shared', grantedHosts: ['www.uni.example'] };
  assert.equal(cookieAllowed(media, top), true);
  assert.equal(cookieAllowed(mediaNone, top), true);
  assert.equal(cookieAllowed(media, { ...top, grantedHosts: [] }), false, 'a page the user only opened (activeTab) proves nothing');
  assert.equal(cookieAllowed(media, { ...top, siteHosts: [] }), false, 'no frame on the page’s site asked for it');
  assert.equal(cookieAllowed({ ...media }, top), false, 'a cookie without its answer’s marks: no proof');
  assert.equal(cookieAllowed(withAnswerDomains([{ name: 'other', domain: 'other.uni.example', hostOnly: true, sameSite: 'lax' }, { name: 'sso', domain: '.uni.example', hostOnly: false }])[0], top), false, 'another host’s host-only cookie');
  // a frame of another site that asked (B1 shape): ① has no top host, so nothing of this
  assert.equal(cookieAllowed(media, { ...top, topHost: '', siteHosts: [], grantedHosts: [] }), false);
  // ② an embedded player (granted) whose CDN subdomain serves the track: host-only SameSite=None only, Chrome only
  const cdn = withAnswerDomains([
    { name: 'parentNone', domain: '.video.example', hostOnly: false, sameSite: 'no_restriction' },
    { name: 'cdnNone', domain: 'cdn.video.example', hostOnly: true, sameSite: 'no_restriction' },
    { name: 'cdnLax', domain: 'cdn.video.example', hostOnly: true, sameSite: 'lax' },
  ]);
  const frame = { url: 'https://cdn.video.example/m/1.mp4', all: false, topHost: '', siteHosts: [], frameHosts: ['player.video.example'], thirdParty: 'shared', grantedHosts: ['player.video.example'] };
  assert.deepEqual(cdn.map((c) => cookieAllowed(c, frame)), [true, true, false]);
  assert.deepEqual(cdn.map((c) => cookieAllowed(c, { ...frame, thirdParty: 'partitioned' })), [false, false, false], 'Firefox: the tab’s partition only');
  assert.deepEqual(cdn.map((c) => cookieAllowed(c, { ...frame, grantedHosts: [] })), [true, false, false], 'the frame’s host not granted: as before');
});

test('cookies become bridge.Cookie, each once', () => {
  const c = { name: 'sid', value: 'v', domain: '.example.net', path: '/', secure: true, httpOnly: true, hostOnly: false, expirationDate: 1893456000.7, sameSite: 'lax', storeId: '0' };
  assert.deepEqual(toBridgeCookie(c), { name: 'sid', value: 'v', domain: '.example.net', path: '/', expires: 1893456000, secure: true, httpOnly: true, hostOnly: false });
  assert.equal(toBridgeCookie({ name: 's', value: 'x', domain: 'a', session: true }).expires, 0);
  const merged = mergeCookies([[c], [c, { ...c, path: '/x' }]]);
  assert.deepEqual(merged.map((x) => x.path), ['/', '/x']);
});

const result = (over = {}) => ({
  status: 'ok',
  pageUrl: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=abc#t=10',
  profile: { id: 'panopto', version: '1.1.0', name: 'Panopto', reviewed: true },
  request: { sendReferer: true, cookieUrls: [], maxConnectionsPerFile: 4 },
  sessionId: 'abc',
  lecture: { title: 'CSC1003 L1', recordedAt: '2026-10-21T09:35', durationSec: 3299.6 },
  downloadPolicy: 'enabled',
  tracks: [V1, A1, P1],
  named: { tracks: [{ ...V1, role: 'podcast', label: 'Full recording (MP4)', recommended: true }], others: [A1, P1] },
  ...over,
});

test('the enqueue request', () => {
  const r = result();
  assert.deepEqual(listedTracks(r).map((t) => t.id), ['v1', 'a1', 'p1']);
  const cookies = [{ name: 'a', value: 'b', domain: 'cdn.example.net', path: '/', expires: 0, secure: true, httpOnly: true, hostOnly: true }];
  const e = buildEnqueue({ result: r, ids: ['a1', 'v1'], courseId: '01JCOURSE', cookies, overrideDisabled: true, msg });
  assert.deepEqual(e.profile, { id: 'panopto', version: '1.1.0', reviewed: true });
  assert.equal(e.platform, 'panopto');
  assert.equal(e.sessionId, 'abc');
  assert.deepEqual(e.lecture, { title: 'CSC1003 L1', recordedAt: '2026-10-21T09:35', durationSec: 3300 });
  assert.equal(e.courseId, '01JCOURSE');
  assert.deepEqual(e.tracks, [
    { url: V1.url, kind: 'video', format: 'file', role: 'podcast', label: 'Full recording (MP4)', sizeBytes: 100, quality: '720p' },
    { url: A1.url, kind: 'audio', format: 'file', role: '', label: 'Audio · en', sizeBytes: 50 },
  ]);
  assert.equal(e.referer, 'https://tenant.cloud.panopto.eu/', 'tracks on another origin: the page’s origin only (strict-origin-when-cross-origin)');
  assert.deepEqual(e.cookies, cookies);
  assert.equal(e.maxConnections, 4);
  assert.equal(e.overrideDisabled, false, 'only when downloads are off');
  assert.equal(buildEnqueue({ result: result({ downloadPolicy: 'disabled' }), ids: ['v1'], courseId: 'c', overrideDisabled: true, msg }).overrideDisabled, true);
  // Generic detection: the page's own title (T048; empty when it has none and
  // LectureFlow names it), "generic", no connection cap.
  const g = buildEnqueue({ result: result({ profile: null, request: {}, named: null, lecture: { title: 'Lecture 7: Hashing', recordedAt: '', durationSec: 0 } }), ids: ['p1', 'v1'].slice(1), courseId: 'c', msg });
  assert.deepEqual(g.profile, { id: 'generic', version: '', reviewed: false });
  assert.equal(g.platform, 'generic');
  assert.equal(g.lecture.title, 'Lecture 7: Hashing');
  assert.equal(buildEnqueue({ result: result({ profile: null, request: {}, named: null, lecture: { title: '', recordedAt: '', durationSec: 0 } }), ids: ['v1'], courseId: 'c', msg }).lecture.title, '');
  assert.equal(g.maxConnections, 0);
  assert.equal(g.tracks[0].label, 'Video · 720p');
  // A DASH track carries its selector.
  assert.equal(buildEnqueue({ result: result(), ids: ['p1'], courseId: 'c', msg }).tracks[0].selector, 'video::0');
  assert.equal(refererFor(result({ request: { sendReferer: false } })), '');
  assert.equal(trackName({ kind: 'unknown' }, msg), 'Unknown');
});

test('track names: the playlist’s own name before a language code (T26 J5); the same name twice is numbered (T26 G12)', () => {
  const m = (k) => ({ kind_audio: 'Audio', kind_video: 'Video', kind_unknown: 'Unknown' })[k] || k;
  assert.equal(trackName({ kind: 'audio', name: 'English', language: 'en' }, m), 'Audio · English');
  assert.equal(trackName({ kind: 'audio', name: '', language: 'zh' }, m), 'Audio · zh');
  assert.equal(trackName({ kind: 'video', quality: '720p' }, m), 'Video · 720p');
  assert.equal(trackName({ kind: 'audio', label: 'Lecture audio', name: 'x' }, m), 'Lecture audio', 'a profile’s label wins');
  const names = trackNames(
    [
      { id: 'v', kind: 'video' },
      { id: 'a1', kind: 'audio' },
      { id: 'a2', kind: 'audio' },
      { id: 'e', kind: 'audio', name: 'English' },
      { id: 'a3', kind: 'audio' },
    ],
    m,
  );
  assert.deepEqual([...names], [['v', 'Video'], ['a1', 'Audio 1'], ['a2', 'Audio 2'], ['e', 'Audio · English'], ['a3', 'Audio 3']]);
  assert.deepEqual([...trackNames([{ id: 'x', kind: 'audio' }], m)], [['x', 'Audio']], 'a single one keeps its name');
});

test('the refresh request', () => {
  const r = buildRefresh({ result: result(), ids: ['v1'], cookies: [], msg });
  assert.deepEqual(Object.keys(r).sort(), ['cookies', 'platform', 'referer', 'sessionId', 'tracks']);
  assert.equal(r.tracks[0].role, 'podcast');
  assert.equal(r.referer, 'https://tenant.cloud.panopto.eu/', 'the same rule as the enqueue');
});

test('Referer as a browser’s default policy would send it (security review L2)', () => {
  const page = 'https://user:pw@tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=abc&token=SECRET#t=10';
  const same = { url: 'https://tenant.cloud.panopto.eu/Panopto/Podcast/x.mp4?sig=1' };
  const cdn = { url: 'https://d0000.cloudfront.net/sessions/x/master.m3u8' };
  const plainHttp = { url: 'http://media.example.edu/x.mp4' };
  const r = { request: {}, pageUrl: page };
  // every track on the page's origin: the address without fragment or user info
  assert.equal(refererFor(r, [same]), 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=abc&token=SECRET');
  // any track elsewhere: the origin only, no path, no query
  assert.equal(refererFor(r, [cdn]), 'https://tenant.cloud.panopto.eu/');
  assert.equal(refererFor(r, [same, cdn]), 'https://tenant.cloud.panopto.eu/', 'mixed: the stricter one');
  // another port or scheme is another origin
  assert.equal(refererFor(r, [{ url: 'https://tenant.cloud.panopto.eu:8443/x.mp4' }]), 'https://tenant.cloud.panopto.eu/');
  // HTTPS page, HTTP track: nothing (no-referrer-when-downgrade part of the policy)
  assert.equal(refererFor(r, [plainHttp]), '');
  assert.equal(refererFor(r, [same, plainHttp]), '');
  // ... but an HTTP loopback track is potentially trustworthy (Secure
  // Contexts): browsers send it the origin, as LectureFlow does
  // (internal/fetch refererFor; the native end-to-end test's media is there)
  for (const url of ['http://127.0.0.1:8080/x.mp4', 'http://127.9.9.9/x.mp4', 'http://[::1]:9000/x.mp4', 'http://localhost/x.mp4', 'http://media.localhost./x.mp4']) {
    assert.equal(refererFor(r, [{ url }]), 'https://tenant.cloud.panopto.eu/', url);
    assert.equal(refererFor(r, [same, { url }]), 'https://tenant.cloud.panopto.eu/', `mixed: ${url}`);
  }
  for (const url of ['http://10.0.0.1/x.mp4', 'http://128.0.0.1/x.mp4', 'http://localhost.example.edu/x.mp4', 'http://[::2]/x.mp4']) {
    assert.equal(refererFor(r, [{ url }]), '', url);
  }
  assert.equal(refererFor(r, [{ url: 'http://127.0.0.1/x.mp4' }, plainHttp]), '', 'one untrustworthy HTTP track: nothing');
  // an HTTP page to HTTPS media elsewhere: the origin
  assert.equal(refererFor({ request: {}, pageUrl: 'http://video.example.edu/watch?v=1' }, [cdn]), 'http://video.example.edu/');
  // no tracks known (nothing to compare): as before, the page without fragment
  assert.equal(refererFor({ request: {}, pageUrl: 'https://a.example/p?q=1#f' }), 'https://a.example/p?q=1');
  assert.equal(refererFor({ request: { sendReferer: false }, pageUrl: page }, [same]), '');
  assert.equal(refererFor({ request: {}, pageUrl: 'not a url' }, [same]), '');
});

test('site access the popup offers: the page’s origin; the tracks’ hosts apart, only when cookies are read (security review L4)', () => {
  const t = (id, url, extra = {}) => ({ id, url, kind: 'video', format: 'file', sizeBytes: 1, protected: false, ...extra });
  const page = 'https://video.example.edu/watch/1?x=y';
  const tracks = [
    t('a', 'https://cdn.example.net/a.mp4'),
    t('b', 'https://video.example.edu/b.mp4'),
    t('c', 'https://drm.example.org/c.mpd', { protected: true }),
    t('d', 'https://rr1.googlevideo.com/x'),
    t('e', 'http://plain.example.com/e.mp4'),
    t('f', 'https://cdn.example.net/f.m4a'),
    t('g', 'https://media.example.edu/g.mp4'),
    t('h', 'https://player.example.org/h.mp4', { sources: ['webrequest'], frameIds: [3] }),
  ];
  // generic detection reads the tracks' cookies: their other hosts, each once, sorted — only
  // those a send could take cookies of (AUD3 中-1): a host that may share a cookie with the
  // page, the site of an embedded player whose request the browser made; not cdn.example.net,
  // which the page itself names on another site (before: offered)
  assert.deepEqual(accessOffer({ status: 'ok', pageUrl: page, request: {}, tracks, frameHosts: { 0: 'video.example.edu', 3: 'player.example.org' } }), {
    page: 'https://video.example.edu/*',
    cookieHosts: ['https://media.example.edu/*', 'https://player.example.org/*'],
  });
  // a host whose media a profile says need no cookie (Panopto's): a send reads none there, so
  // it is not offered as a cookie site either (SEC46 信-2; before: offered)
  assert.deepEqual(accessOffer({ status: 'ok', pageUrl: page, request: {}, tracks, frameHosts: { 0: 'video.example.edu', 3: 'player.example.org' }, quietHosts: ['*.example.edu'] }).cookieHosts, ['https://player.example.org/*']);
  // an embedded player's CDN whose request is not on record yet (the browser reports it only once
  // granted): offered, so the first send from the LMS page can be set up (SEC46 低-3, D2; before: not)
  const lms = 'https://lms.uni.example/course/1';
  const cdn = t('cdn', 'https://cdn.video.example/m/1.mp4', { sources: ['element'], frameIds: [5] });
  const d2 = { status: 'ok', pageUrl: lms, request: {}, tracks: [cdn], frameHosts: { 0: 'lms.uni.example', 5: 'player.video.example' } };
  assert.deepEqual(accessOffer(d2), { page: 'https://lms.uni.example/*', cookieHosts: ['https://cdn.video.example/*'] });
  assert.deepEqual(cookiePlan(d2, [cdn], lms), [], 'offering it hands over nothing: no request on record, no cookie read');
  assert.deepEqual(accessOffer({ ...d2, frameHosts: { 0: 'lms.uni.example' } }).cookieHosts, [], 'a frame the scan did not read');
  assert.deepEqual(accessOffer({ ...d2, quietHosts: ['*.video.example'] }).cookieHosts, []);
  assert.deepEqual(accessOffer({ ...d2, request: { cookieUrls: [] } }).cookieHosts, []);
  // an attacker page (or its frame) that only names another site's media: not offered
  const named = t('n', 'https://lms.victim-uni.example/pluginfile.php/1/a.mp4', { sources: ['element'], frameIds: [0, 5] });
  assert.deepEqual(accessOffer({ ...d2, pageUrl: 'https://attacker.example/x', tracks: [named], frameHosts: { 0: 'attacker.example', 5: 'player.video.example' } }).cookieHosts, []);
  // a profile's declared media host is offered as before
  const declared = { status: 'ok', pageUrl: page, request: { cookieUrls: ['track'] }, profile: { id: 'p', origins: ['https://video.example.edu/*', 'https://*.example.net/*'] }, tracks };
  assert.deepEqual(accessOffer(declared).cookieHosts, ['https://cdn.example.net/*', 'https://media.example.edu/*']);
  // a profile that reads no cookies (Panopto): the page only
  assert.deepEqual(accessOffer({ status: 'ok', pageUrl: page, request: { cookieUrls: [] }, tracks }), { page: 'https://video.example.edu/*', cookieHosts: [] });
  // a plain-HTTP page cannot be granted at all; a result that is not ok neither
  assert.deepEqual(accessOffer({ status: 'ok', pageUrl: 'http://video.example.edu/', request: {}, tracks }), { page: '', cookieHosts: [] });
  assert.deepEqual(accessOffer({ status: 'no_access' }), { page: '', cookieHosts: [] });
  assert.deepEqual(accessOffer(null), { page: '', cookieHosts: [] });
});

test('check items', () => {
  assert.equal(checkOutcome(null), 'new');
  assert.equal(checkOutcome({ state: 'imported' }), 'imported');
  for (const s of ['queued', 'downloading', 'merging', 'importing']) assert.equal(checkOutcome({ state: s }), 'active');
  assert.equal(checkOutcome({ state: 'needs_refresh' }), 'needs_refresh');
  assert.equal(checkOutcome({ state: 'failed' }), 'again');
  assert.equal(checkOutcome({ state: 'cancelled' }), 'again');
});

test('the popup’s view', () => {
  const ok = { status: 'ok', tracks: [V1, A1], downloadPolicy: 'enabled' };
  const conn = { status: 'connected' };
  const v = (o) => popupView({ detection: ok, connection: conn, checked: true, ...o });
  assert.equal(popupView({ detection: null }).view, 'loading');
  assert.equal(popupView({ detection: { status: 'unsupported' } }).view, 'unsupported');
  assert.deepEqual(popupView({ detection: { status: 'no_access' } }), { view: 'none', reason: 'no_access' });
  assert.deepEqual(popupView({ detection: { ...ok, tracks: [] }, connection: conn }), { view: 'none', reason: 'no_tracks' });
  assert.equal(popupView({ detection: ok, connection: { status: 'checking' } }).view, 'loading');
  assert.deepEqual(popupView({ detection: ok, connection: { status: 'unavailable', reason: 'app.not_running' } }), { view: 'disconnected', reason: 'app.not_running' });
  assert.equal(v({ checked: false }).view, 'loading');
  assert.deepEqual(v({}), { view: 'ready', reason: '' });
  assert.deepEqual(v({ checkItem: { state: 'failed' } }), { view: 'ready', reason: 'failed' });
  assert.deepEqual(v({ checkItem: { state: 'imported' } }), { view: 'blocked', reason: 'imported' });
  assert.equal(v({ checkItem: { state: 'downloading' } }).view, 'queued');
  assert.equal(v({ checkItem: { state: 'needs_refresh' } }).view, 'refresh');
  assert.deepEqual(v({ detection: { ...ok, tracks: [P1] } }), { view: 'blocked', reason: 'protected' });
  assert.deepEqual(v({ detection: { ...ok, downloadPolicy: 'disabled' } }), { view: 'blocked', reason: 'disabled' });
  assert.deepEqual(v({ detection: { ...ok, downloadPolicy: 'disabled' }, override: true }), { view: 'ready', reason: '' });
  assert.equal(v({ task: { taskId: 3, state: 'queued' } }).view, 'queued');
  assert.deepEqual(progressOf({ bytesDone: 50, bytesTotal: 200 }), { percent: 25, done: 50, total: 200 });
  assert.deepEqual(progressOf({ bytesDone: 50, bytesTotal: -1 }), { percent: -1, done: 50, total: -1 });
  assert.equal(isFinal('done') && isFinal('needs_refresh') && !isFinal('downloading'), true);
});

test('the popup’s view while it waits says for what (T26 G7): tracks, LectureFlow connecting or starting, the check', () => {
  const ok = { status: 'ok', tracks: [V1, A1], downloadPolicy: 'enabled' };
  assert.deepEqual(popupView({ detection: null }), { view: 'loading', reason: '' }, 'still detecting');
  assert.deepEqual(popupView({ detection: { status: 'loading' }, connection: { status: 'checking', launching: true } }), { view: 'loading', reason: '' }, 'still detecting, whatever the connection');
  assert.deepEqual(popupView({ detection: ok, connection: null }), { view: 'loading', reason: 'connecting' });
  assert.deepEqual(popupView({ detection: ok, connection: { status: 'checking' } }), { view: 'loading', reason: 'connecting' });
  assert.deepEqual(popupView({ detection: ok, connection: { status: 'checking', launching: true } }), { view: 'loading', reason: 'launching' });
  assert.deepEqual(popupView({ detection: ok, connection: { status: 'connected' }, checked: false }), { view: 'loading', reason: 'checking' });
});

test('the send block stays in sight only where enough of the card is left above it (SEC42 中-1)', () => {
  assert.equal(SEND_ROOM, 260);
  // the popup at Chrome's 600 px, English or Russian with no course yet
  assert.equal(sendSticks(600, 126), true);
  assert.equal(sendSticks(600, 239), true);
  // an in-page card in a low window (SEC42: 184 px with a 126 px block; 420 px viewport)
  assert.equal(sendSticks(184, 126), false);
  assert.equal(sendSticks(360, 126), false);
  assert.equal(sendSticks(386, 126), true, 'exactly SEND_ROOM left');
  // nothing to keep in sight, or nothing measured
  assert.equal(sendSticks(600, 0), false);
  assert.equal(sendSticks(Number.NaN, 126), false);
  assert.equal(sendSticks(600, undefined), false);
});

test('do-not-handle sites (lead ruling 10): YouTube and its media hosts, subdomains included', () => {
  for (const h of ['youtube.com', 'www.youtube.com', 'm.youtube.com', 'youtu.be', 'www.youtube-nocookie.com', 'rr3---sn-abc.googlevideo.com', 'YOUTUBE.COM.']) assert.equal(isBlockedHost(h), true, h);
  for (const h of ['notyoutube.com', 'youtube.com.example.org', 'video.example.edu', 'tenant.cloud.panopto.eu', '']) assert.equal(isBlockedHost(h), false, h);
  assert.equal(isBlockedUrl('https://www.youtube.com/watch?v=x'), true);
  assert.equal(isBlockedUrl('not a url'), false);
  assert.equal(wildcardTouchesBlocked('*.youtube.com'), true);
  assert.equal(wildcardTouchesBlocked('*.googlevideo.com'), true);
  assert.equal(wildcardTouchesBlocked('music.youtube.com'), true);
  assert.equal(wildcardTouchesBlocked('*.panopto.eu'), false);
});
