// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Partitioned cookies (Firefox Total Cookie Protection, Chrome CHIPS): a
// player embedded in an LMS page keeps its cookies in the LMS site's
// partition. The fakes below follow the browsers' getAll semantics (read
// from their sources, see src/lib/partition.js) and record the arguments of
// every call, never the answers. All cookie values are made up.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { sameSiteAsTop, sameFirstParty, cookieScope, mergePartitioned, readCookiesWith, isPartitioned } from '../../src/lib/partition.js';
import { cookieTargets } from '../../src/lib/send.js';

const MEDIA = 'https://media.example.net/v/lecture.m3u8';
const LMS_PAGE = 'https://canvas.lms.example.edu/courses/1/modules';
const LMS_SITE = 'https://lms.example.edu';
const OTHER_SITE = 'https://other.example.org';

const plain = (name, value, extra = {}) => ({ name, value, domain: 'media.example.net', path: '/', hostOnly: true, secure: true, httpOnly: false, ...extra });
const parted = (name, value, topLevelSite, hasCrossSiteAncestor = true, extra = {}) => plain(name, value, { partitionKey: { topLevelSite, hasCrossSiteAncestor }, ...extra });

const JAR = [
  plain('sid', 'PLAIN-SID'),
  parted('cf', 'LMS-CF', LMS_SITE),
  parted('cf', 'OTHER-CF', OTHER_SITE),
  parted('lms_only', 'LMS-ONLY', LMS_SITE, true, { path: '/v' }),
  // Another host: never matches the media URL.
  plain('sid', 'ELSEWHERE', { domain: 'elsewhere.example.com' }),
];

function matchesUrl(c, url) {
  const u = new URL(url);
  const d = c.domain.replace(/^\./, '');
  const hostOk = c.hostOnly ? u.hostname === d : u.hostname === d || u.hostname.endsWith(`.${d}`);
  return hostOk && u.pathname.startsWith(c.path) && (!c.secure || u.protocol === 'https:');
}

/**
 * A cookies API. browser: 'chrome' (unpartitioned cookies have no
 * partitionKey) or 'firefox' (null). partitionKeys: frameId → key, or
 * 'throw'; undefined = no getPartitionKey (Firefox, Chrome before 132).
 * noPartitionKey: Chrome before 119 rejects the property. fpi: Firefox
 * with first-party isolation (getAll needs firstPartyDomain).
 */
function fakeApi({ browser = 'chrome', jar = JAR, partitionKeys, noPartitionKey = false, fpi = false } = {}) {
  const calls = [];
  const keyCalls = [];
  const out = (c) => {
    const o = { ...c };
    if (!o.partitionKey) {
      if (browser === 'firefox') o.partitionKey = null;
      else delete o.partitionKey;
    }
    return o;
  };
  const api = {
    calls,
    keyCalls,
    async getAll(details) {
      calls.push(JSON.parse(JSON.stringify(details)));
      if (fpi && !('firstPartyDomain' in details)) throw new Error('First-Party Isolation is enabled');
      const pk = details.partitionKey;
      if (pk !== undefined && noPartitionKey) throw new Error("Error in invocation of cookies.getAll: Unexpected property: 'partitionKey'");
      return jar
        .filter((c) => matchesUrl(c, details.url))
        // the cookie store (Firefox containers): the default one unless named
        .filter((c) => (details.storeId === undefined ? !c.storeId : c.storeId === details.storeId))
        // first-party isolation: null reads every first party, a domain only its own
        .filter((c) => !fpi || details.firstPartyDomain === null || c.firstPartyDomain === details.firstPartyDomain)
        .filter((c) => {
          if (pk === undefined || pk === null) return !c.partitionKey; // unpartitioned only
          if (pk.topLevelSite === undefined) return true; // {}: every partition and the unpartitioned
          // A given partition: only that one, no unpartitioned cookies.
          if (!c.partitionKey || c.partitionKey.topLevelSite !== pk.topLevelSite) return false;
          return pk.hasCrossSiteAncestor === undefined || pk.hasCrossSiteAncestor === c.partitionKey.hasCrossSiteAncestor;
        })
        .map(out);
    },
  };
  if (partitionKeys !== undefined) {
    api.getPartitionKey = async (details) => {
      keyCalls.push(JSON.parse(JSON.stringify(details)));
      const k = partitionKeys[details.frameId];
      if (!k || k === 'throw') throw new Error(`No host permissions for cookies at url: "${LMS_SITE}/".`);
      return { partitionKey: k };
    };
  }
  return api;
}

const values = (cookies) => cookies.map((c) => `${c.name}=${c.value}${c.path === '/' ? '' : `@${c.path}`}`);
const leaksNothing = (api) => {
  const recorded = JSON.stringify([api.calls, api.keyCalls]);
  for (const c of JAR) assert.ok(!recorded.includes(c.value), 'the recorder keeps arguments only');
};

test('sameSiteAsTop: the scheme and the site of the tab’s top-level page, no public suffix list', () => {
  assert.equal(sameSiteAsTop(LMS_SITE, LMS_PAGE), true, 'a subdomain of the site');
  assert.equal(sameSiteAsTop(LMS_SITE, 'https://lms.example.edu/'), true, 'the site itself');
  assert.equal(sameSiteAsTop(LMS_SITE, 'https://a.b.lms.example.edu/x?y=1#z'), true);
  assert.equal(sameSiteAsTop(LMS_SITE, 'http://canvas.lms.example.edu/'), false, 'another scheme');
  assert.equal(sameSiteAsTop('http://lms.example.edu', LMS_PAGE), false, 'another scheme, the other way');
  assert.equal(sameSiteAsTop(LMS_SITE, 'https://evillms.example.edu/'), false, 'a look-alike, not a subdomain');
  assert.equal(sameSiteAsTop(LMS_SITE, 'https://lms.example.edu.evil.net/'), false, 'the site inside another host');
  assert.equal(sameSiteAsTop(OTHER_SITE, LMS_PAGE), false, 'another site’s partition');
  assert.equal(sameSiteAsTop('https://example.edu', 'https://lms.example.edu/'), true, 'the browser decides what the site is');
  // Chrome's sites have no port; Firefox names one for some hosts.
  assert.equal(sameSiteAsTop(LMS_SITE, 'https://canvas.lms.example.edu:8443/'), true);
  assert.equal(sameSiteAsTop('http://localhost:8080', 'http://localhost:8080/x'), true);
  assert.equal(sameSiteAsTop('http://localhost:8080', 'http://localhost:9090/x'), false);
  assert.equal(sameSiteAsTop('https://[::1]', 'https://[::1]/x'), true);
  for (const [site, top] of [['', LMS_PAGE], ['lms.example.edu', LMS_PAGE], ['file:///etc', LMS_PAGE], [LMS_SITE, ''], [LMS_SITE, 'about:blank'], [LMS_SITE, undefined], [undefined, LMS_PAGE]]) {
    assert.equal(sameSiteAsTop(site, top), false, `${site} / ${top}`);
  }
});

test('cookieScope: the popup’s tracks’ frames, an in-page card’s own frame', () => {
  const tracks = [{ frameIds: [3, 0] }, { frameIds: [3] }, {}, { frameIds: [] }];
  assert.deepEqual(cookieScope({ tabId: 7, topUrl: LMS_PAGE, tracks }), { tabId: 7, frameIds: [0, 3], topUrl: LMS_PAGE, storeId: '' });
  assert.deepEqual(cookieScope({ tabId: 7, frameId: 5, topUrl: '', tracks }), { tabId: 7, frameIds: [5], topUrl: '', storeId: '' });
  assert.deepEqual(cookieScope({ tabId: 7, frameId: 0, topUrl: LMS_PAGE, storeId: 'firefox-container-2' }), { tabId: 7, frameIds: [0], topUrl: LMS_PAGE, storeId: 'firefox-container-2' });
  assert.deepEqual(cookieScope({ tabId: Number.NaN, topUrl: 'chrome://newtab/' }), { tabId: null, frameIds: [0], topUrl: '', storeId: '' });
  assert.deepEqual(cookieScope(), { tabId: null, frameIds: [0], topUrl: '', storeId: '' });
  assert.equal(cookieScope({ storeId: 42 }).storeId, '');
});

test('mergePartitioned: a partitioned cookie wins over its unpartitioned twin (name, domain, path)', () => {
  const merged = mergePartitioned(
    [[plain('cf', 'PLAIN-CF'), plain('sid', 'PLAIN-SID')], [plain('sid', 'PLAIN-SID')]],
    [[parted('cf', 'LMS-CF', LMS_SITE), parted('cf', 'LMS-CF-2', LMS_SITE, false)], [parted('cf', 'LMS-CF', LMS_SITE, true, { path: '/v' })]],
  );
  assert.deepEqual(values(merged), ['cf=LMS-CF', 'sid=PLAIN-SID', 'cf=LMS-CF@/v'], 'twin replaced in place, extras after, each once');
  assert.ok(merged.every((c) => !('partitionKey' in c)), 'bridge cookies carry no partition');
  assert.deepEqual(mergePartitioned([[plain('sid', 'A')]], []), [{ name: 'sid', value: 'A', domain: 'media.example.net', path: '/', expires: 0, secure: true, httpOnly: false, hostOnly: true }]);
});

test('Chrome 132+ (getPartitionKey): the frame’s exact partition, merged with the unpartitioned cookies', async () => {
  const api = fakeApi({ partitionKeys: { 3: { topLevelSite: LMS_SITE, hasCrossSiteAncestor: true } } });
  const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: LMS_PAGE }));
  assert.deepEqual(values(got), ['sid=PLAIN-SID', 'cf=LMS-CF', 'lms_only=LMS-ONLY@/v']);
  assert.ok(!values(got).includes('cf=OTHER-CF'), 'never another site’s partition');
  assert.deepEqual(api.keyCalls, [{ tabId: 7, frameId: 3 }]);
  assert.deepEqual(api.calls, [{ url: MEDIA }, { url: MEDIA, partitionKey: { topLevelSite: LMS_SITE, hasCrossSiteAncestor: true } }], 'no read of every partition');
  leaksNothing(api);
});

test('Chrome 132+ with a frame key but no tab address (access to the site, not to the page’s host): the key alone is enough', async () => {
  const api = fakeApi({ partitionKeys: { 3: { topLevelSite: LMS_SITE, hasCrossSiteAncestor: true } } });
  const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: '' }));
  assert.deepEqual(values(got), ['sid=PLAIN-SID', 'cf=LMS-CF', 'lms_only=LMS-ONLY@/v']);
});

test('Chrome without getPartitionKey (119–131), or refused it (no host access to the LMS’s site origin, the usual case): every partition read once, only the tab’s site kept', async () => {
  for (const partitionKeys of [undefined, { 0: 'throw', 3: 'throw' }]) {
    const api = fakeApi({ partitionKeys });
    const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, topUrl: LMS_PAGE, tracks: [{ frameIds: [3] }] }));
    assert.deepEqual(values(got), ['sid=PLAIN-SID', 'cf=LMS-CF', 'lms_only=LMS-ONLY@/v'], String(partitionKeys));
    assert.deepEqual(api.calls, [{ url: MEDIA }, { url: MEDIA, partitionKey: {} }]);
    leaksNothing(api);
  }
});

test('getPartitionKey answers for some frames only: with the tab’s address, the site filter covers them all', async () => {
  const api = fakeApi({ partitionKeys: { 0: { topLevelSite: LMS_SITE, hasCrossSiteAncestor: false }, 3: 'throw' } });
  const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, topUrl: LMS_PAGE, tracks: [{ frameIds: [0, 3] }] }));
  assert.deepEqual(api.keyCalls, [{ tabId: 7, frameId: 0 }, { tabId: 7, frameId: 3 }]);
  assert.deepEqual(api.calls, [{ url: MEDIA }, { url: MEDIA, partitionKey: {} }]);
  assert.deepEqual(values(got), ['sid=PLAIN-SID', 'cf=LMS-CF', 'lms_only=LMS-ONLY@/v']);
  // Without the address: what the frames that answered gave.
  const api2 = fakeApi({ partitionKeys: { 0: { topLevelSite: LMS_SITE, hasCrossSiteAncestor: true }, 3: 'throw' } });
  const got2 = await readCookiesWith(api2, [MEDIA], cookieScope({ tabId: 7, topUrl: '', tracks: [{ frameIds: [0, 3] }] }));
  assert.deepEqual(api2.calls, [{ url: MEDIA }, { url: MEDIA, partitionKey: { topLevelSite: LMS_SITE, hasCrossSiteAncestor: true } }]);
  assert.deepEqual(values(got2), ['sid=PLAIN-SID', 'cf=LMS-CF', 'lms_only=LMS-ONLY@/v']);
});

test('Chrome before 119 rejects partitionKey: the unpartitioned cookies, as before', async () => {
  const api = fakeApi({ noPartitionKey: true });
  const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, topUrl: LMS_PAGE, tracks: [{ frameIds: [3] }] }));
  assert.deepEqual(values(got), ['sid=PLAIN-SID']);
  assert.deepEqual(api.calls, [{ url: MEDIA }, { url: MEDIA, partitionKey: {} }]);
});

test('Firefox (Total Cookie Protection): only the unpartitioned cookies and the tab’s own site’s partition', async () => {
  const jar = [
    ...JAR,
    parted('sub', 'SAME-SITE', LMS_SITE, true),
    parted('http', 'HTTP-PARTITION', 'http://lms.example.edu', true),
    parted('look', 'LOOK-ALIKE', 'https://example.edu.evil.net', true),
    parted('suffix', 'SUFFIX', 'https://mslms.example.edu', true),
  ];
  const api = fakeApi({ browser: 'firefox', jar });
  const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: LMS_PAGE }));
  assert.deepEqual(values(got), ['sid=PLAIN-SID', 'cf=LMS-CF', 'lms_only=LMS-ONLY@/v', 'sub=SAME-SITE']);
  for (const v of ['OTHER-CF', 'HTTP-PARTITION', 'LOOK-ALIKE', 'SUFFIX']) assert.ok(!JSON.stringify(got).includes(v), v);
  assert.deepEqual(api.calls, [{ url: MEDIA }, { url: MEDIA, partitionKey: {} }], 'no getPartitionKey in Firefox');
  // An unpartitioned cookie (partitionKey: null) from the wide read is not a partitioned one.
  assert.equal(isPartitioned({ partitionKey: null }), false);
  assert.equal(isPartitioned({ partitionKey: { topLevelSite: '' } }), false);
  assert.equal(isPartitioned({ partitionKey: { topLevelSite: LMS_SITE } }), true);
});

test('no top-level address and no frame key (in-page card, LMS not granted, Firefox): unpartitioned only, no partition read', async () => {
  const api = fakeApi({ browser: 'firefox' });
  const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: '' }));
  assert.deepEqual(values(got), ['sid=PLAIN-SID']);
  assert.deepEqual(api.calls, [{ url: MEDIA }]);
});

test('sameFirstParty: the first party Firefox recorded is the tab’s own site (plain or use_site form)', () => {
  assert.equal(sameFirstParty('example.edu', LMS_PAGE), true);
  assert.equal(sameFirstParty('lms.example.edu', LMS_PAGE), true);
  assert.equal(sameFirstParty('EXAMPLE.EDU', LMS_PAGE), true);
  assert.equal(sameFirstParty('(https,example.edu)', LMS_PAGE), true);
  assert.equal(sameFirstParty('(http,example.edu)', LMS_PAGE), false, 'another scheme');
  assert.equal(sameFirstParty('(https,example.edu,8443)', 'https://lms.example.edu:8443/x'), true);
  assert.equal(sameFirstParty('(https,example.edu,8443)', LMS_PAGE), false, 'another port');
  for (const [fpd, top] of [['example.org', LMS_PAGE], ['ample.edu', LMS_PAGE], ['', LMS_PAGE], [null, LMS_PAGE], ['example.edu', ''], ['example.edu', 'about:blank']]) {
    assert.equal(sameFirstParty(fpd, top), false, `${fpd} / ${top}`);
  }
});

test('first-party isolation (security review L5): every first party read once, only the tab’s own site kept, never all of them merged', async () => {
  const jar = [
    plain('sid', 'FP-LMS', { firstPartyDomain: 'example.edu' }),
    plain('sid2', 'FP-OTHER', { firstPartyDomain: 'example.org' }),
    plain('sid3', 'FP-SITE', { firstPartyDomain: '(https,example.edu)' }),
    plain('sid4', 'FP-HTTP', { firstPartyDomain: '(http,example.edu)' }),
  ];
  const api = fakeApi({ browser: 'firefox', fpi: true, jar });
  const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: LMS_PAGE }));
  assert.deepEqual(values(got), ['sid=FP-LMS', 'sid3=FP-SITE']);
  // with isolation on there is no partition to read (the {} read fails and counts as none)
  assert.deepEqual(api.calls, [{ url: MEDIA }, { url: MEDIA, firstPartyDomain: null }, { url: MEDIA, partitionKey: {} }]);
  // without the tab's top-level address: nothing, rather than every first party's
  const api2 = fakeApi({ browser: 'firefox', fpi: true, jar });
  assert.deepEqual(await readCookiesWith(api2, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: '' })), []);
  assert.deepEqual(api2.calls, [{ url: MEDIA }]);
  const recorded = JSON.stringify([api.calls, api2.calls]);
  for (const c of jar) assert.ok(!recorded.includes(c.value));
});

test('Firefox containers (security review L5): every read is in the cookie store of the tab sent from', async () => {
  const jar = [
    plain('sid', 'DEFAULT-SID'),
    plain('sid', 'WORK-SID', { storeId: 'firefox-container-1' }),
    parted('cf', 'WORK-LMS-CF', LMS_SITE, true, { storeId: 'firefox-container-1' }),
    parted('cf', 'DEFAULT-LMS-CF', LMS_SITE),
  ];
  const api = fakeApi({ browser: 'firefox', jar });
  const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: LMS_PAGE, storeId: 'firefox-container-1' }));
  assert.deepEqual(values(got), ['sid=WORK-SID', 'cf=WORK-LMS-CF']);
  assert.deepEqual(api.calls, [
    { url: MEDIA, storeId: 'firefox-container-1' },
    { url: MEDIA, storeId: 'firefox-container-1', partitionKey: {} },
  ]);
  // no container (and Chrome): no storeId in the call, the default store's cookies
  const api2 = fakeApi({ browser: 'firefox', jar });
  assert.deepEqual(values(await readCookiesWith(api2, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: LMS_PAGE }))), ['sid=DEFAULT-SID', 'cf=DEFAULT-LMS-CF']);
  assert.ok(api2.calls.every((c) => !('storeId' in c)));
  // Chrome 132+ with frame keys: the exact partition, in that store too
  const api3 = fakeApi({ jar, partitionKeys: { 3: { topLevelSite: LMS_SITE, hasCrossSiteAncestor: true } } });
  assert.deepEqual(values(await readCookiesWith(api3, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: LMS_PAGE, storeId: 'firefox-container-1' }))), ['sid=WORK-SID', 'cf=WORK-LMS-CF']);
  assert.deepEqual(api3.calls[1], { url: MEDIA, storeId: 'firefox-container-1', partitionKey: { topLevelSite: LMS_SITE, hasCrossSiteAncestor: true } });
});

test('an in-page card in an LMS frame: the top-level origin the browser reported to the frame is enough for the partition (ext-sec)', async () => {
  const api = fakeApi({ browser: 'firefox' });
  const got = await readCookiesWith(api, [MEDIA], cookieScope({ tabId: 7, frameId: 3, topUrl: 'https://canvas.lms.example.edu' }));
  assert.deepEqual(values(got), ['sid=PLAIN-SID', 'cf=LMS-CF', 'lms_only=LMS-ONLY@/v']);
});

test('several track URLs: each read once, each cookie once', async () => {
  const audio = 'https://media.example.net/a/talk.m4a';
  const api = fakeApi({ browser: 'firefox' });
  const got = await readCookiesWith(api, [MEDIA, audio, 'not a url'], cookieScope({ tabId: 7, frameId: 3, topUrl: LMS_PAGE }));
  assert.deepEqual(values(got), ['sid=PLAIN-SID', 'cf=LMS-CF', 'lms_only=LMS-ONLY@/v']);
  assert.deepEqual(api.calls, [{ url: MEDIA }, { url: audio }, { url: MEDIA, partitionKey: {} }, { url: audio, partitionKey: {} }]);
});

test('cookieUrls: [] (Panopto): the cookies API is never called, partition keys included', async () => {
  const tracks = [{ url: MEDIA, frameIds: [3] }];
  const api = fakeApi({ partitionKeys: { 3: { topLevelSite: LMS_SITE, hasCrossSiteAncestor: true } } });
  const got = await readCookiesWith(api, cookieTargets({ cookieUrls: [] }, tracks), cookieScope({ tabId: 7, topUrl: LMS_PAGE, tracks }));
  assert.deepEqual(got, []);
  assert.deepEqual(api.calls, []);
  assert.deepEqual(api.keyCalls, []);
  // The default (and generic detection) reads the tracks' URLs.
  const api2 = fakeApi({ browser: 'firefox' });
  assert.deepEqual(values(await readCookiesWith(api2, cookieTargets({}, tracks), cookieScope({ tabId: 7, topUrl: LMS_PAGE, tracks }))), ['sid=PLAIN-SID', 'cf=LMS-CF', 'lms_only=LMS-ONLY@/v']);
});
