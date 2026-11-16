// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Reading the cookies of a send, partitioned ones included. Firefox's Total
// Cookie Protection (and Chrome's CHIPS, for cookies set with the
// Partitioned attribute) keys the cookies a third-party frame sets by the
// tab's top-level site: a player embedded in an LMS page (Canvas…) keeps
// its session in the LMS site's partition, and cookies.getAll({url}) only
// returns unpartitioned cookies. This module adds the partition of the tab
// the user sent from — never another site's — and is pure: the background
// (src/background/cookies.js) passes the browser's cookies API in.
//
// What the browsers do (checked against their sources, NOTES
// “决定（ext-chips：extension-builder）”):
// - Chrome: getAll({url}) → unpartitioned only; getAll({url, partitionKey:
//   {topLevelSite, hasCrossSiteAncestor?}}) → that partition only (no
//   unpartitioned cookies); getAll({url, partitionKey: {}}) → every
//   partition plus the unpartitioned ones (Chrome 119+; older versions
//   reject the property). cookies.getPartitionKey({tabId, frameId}) gives a
//   frame's exact key (Chrome 132+), but only with host access to the
//   top-level site's own origin ("https://<registrable domain>/*", not the
//   page's host) and, for a sub-frame, to the frame's own site's origin too
//   (these origins as measured on Chromium 156, SEC51 低-1 and SEC51b;
//   that activeTab does not count, from Chromium's source, NOTES
//   ext-chips). The per-page grants this extension asks for seldom include
//   those: on a page like lms.uni.example it throws, and partitioned() —
//   when the tab's top-level address is known — reads every partition and
//   keeps the tab's site's instead; without that address, only the
//   partitions of the frames that did give a key, usually none.
// - Firefox: the same three getAll forms (no getPartitionKey); a
//   partitioned cookie carries partitionKey.topLevelSite ("https://site"),
//   an unpartitioned one partitionKey null.

import { mergeCookies } from './send.js';
import { parseHttpUrl } from './url.js';

/** Whether a cookie came from partitioned storage. */
export function isPartitioned(c) {
  return !!(c && c.partitionKey && typeof c.partitionKey.topLevelSite === 'string' && c.partitionKey.topLevelSite);
}

/**
 * Whether a partition's top-level site ("https://example.com", as the
 * browser computed it: scheme and registrable domain, sometimes a port) is
 * the site of the tab's top-level page: same scheme, and the page's host is
 * the site's host or a subdomain of it (a port, when the site names one,
 * must match too). The browser already reduced the site to its registrable
 * domain, so no public suffix list is needed here.
 */
export function sameSiteAsTop(topLevelSite, topUrl) {
  const top = parseHttpUrl(topUrl);
  const site = parseHttpUrl(topLevelSite);
  if (!top || !site || !site.hostname) return false;
  if (site.protocol !== top.protocol) return false;
  if (site.port && site.port !== top.port) return false;
  return top.hostname === site.hostname || top.hostname.endsWith(`.${site.hostname}`);
}

/**
 * Where a send's cookies are read for: { tabId, frameIds, topUrl }.
 * frameId given (an in-page card): that frame. Otherwise (the popup, which
 * sends a whole tab's detection) the frames the tracks were seen in.
 * topUrl: the tab's top-level address — the popup's detection result, or
 * the in-page card's message sender (sender.tab.url, absent when the
 * extension has no access to the top page, or else the top-level origin the
 * browser reported to the frame); '' when unknown. storeId: the tab's cookie
 * store (tab.cookieStoreId, Firefox containers); '' for the default.
 */
export function cookieScope({ tabId, frameId, topUrl, tracks, storeId = '' } = {}) {
  let frameIds;
  if (Number.isInteger(frameId) && frameId >= 0) frameIds = [frameId];
  else {
    const set = new Set();
    for (const t of tracks || []) {
      const ids = Array.isArray(t.frameIds) && t.frameIds.length ? t.frameIds : [0];
      for (const id of ids) if (Number.isInteger(id) && id >= 0) set.add(id);
    }
    frameIds = set.size ? [...set].sort((a, b) => a - b) : [0];
  }
  return {
    tabId: Number.isInteger(tabId) && tabId >= 0 ? tabId : null,
    frameIds,
    topUrl: parseHttpUrl(topUrl) ? String(topUrl) : '',
    storeId: typeof storeId === 'string' && storeId.length <= 100 ? storeId : '',
  };
}

const keyOf = (b) => `${b.name}\u0000${b.domain}\u0000${b.path}`;

/**
 * Unpartitioned and partitioned cookies as one list, each name / domain /
 * path once. On a clash the partitioned cookie wins: it is the one the
 * embedded player set in this very tab's partition, and the only one the
 * browser sends from that frame under Total Cookie Protection or with
 * third-party cookies blocked (Chrome with third-party cookies allowed
 * sends both; an unpartitioned twin comes from a separate top-level visit).
 * The order is the unpartitioned list's, then the partitioned extras.
 */
export function mergePartitioned(plainLists, partitionedLists) {
  const preferred = new Map();
  for (const b of mergeCookies(partitionedLists)) preferred.set(keyOf(b), b);
  const out = [];
  const seen = new Set();
  for (const b of [...mergeCookies(plainLists), ...preferred.values()]) {
    const k = keyOf(b);
    if (seen.has(k)) continue;
    seen.add(k);
    out.push(preferred.get(k) || b);
  }
  return out;
}

/**
 * Whether a cookie's first party (Firefox first-party isolation: the base
 * domain of the top-level page it was set under, "example.edu", or with
 * privacy.firstparty.isolate.use_site "(https,example.edu[,port])") is the
 * site of the tab's top-level page. The browser computed it, so no public
 * suffix list is needed: the page's host is that domain or under it.
 */
export function sameFirstParty(firstPartyDomain, topUrl) {
  const top = parseHttpUrl(topUrl);
  let fpd = String(firstPartyDomain || '').toLowerCase();
  if (!top || !fpd) return false;
  const site = /^\((https?),([^,()]+)(?:,(\d+))?\)$/.exec(fpd);
  if (site) {
    if (`${site[1]}:` !== top.protocol) return false;
    if (site[3] && site[3] !== (top.port || (top.protocol === 'https:' ? '443' : '80'))) return false;
    fpd = site[2];
  }
  const host = top.hostname.toLowerCase();
  return host === fpd || host.endsWith(`.${fpd}`);
}

const inStore = (scope) => (scope.storeId ? { storeId: scope.storeId } : {});

/**
 * Unpartitioned cookies, in the tab's cookie store (Firefox containers).
 * Firefox with first-party isolation insists on firstPartyDomain: then
 * every first party is read once (null) and only the tab's own site's kept,
 * in memory, here (security review L5: null alone would merge the cookies
 * every top-level site keeps for that host). Without the tab's address,
 * nothing.
 */
async function unpartitioned(api, url, scope) {
  try {
    return await api.getAll({ url, ...inStore(scope) });
  } catch {
    if (!scope.topUrl) return [];
    try {
      const list = await api.getAll({ url, ...inStore(scope), firstPartyDomain: null });
      return (Array.isArray(list) ? list : []).filter((c) => sameFirstParty(c.firstPartyDomain, scope.topUrl));
    } catch {
      return [];
    }
  }
}

async function getAllOr(api, details) {
  try {
    const list = await api.getAll(details);
    return Array.isArray(list) ? list : [];
  } catch {
    // Chrome before 119 has no partitionKey; with first-party isolation
    // on, Firefox keeps no partitioned cookies to read.
    return [];
  }
}

/**
 * Chrome 132+: the frames' own partition keys; complete when every frame
 * answered. No time limit here, unlike actions.js topSiteFor (SEC51 信-1):
 * without access Chrome refuses at once (measured), but a call that never
 * settled would hold the send.
 */
async function frameKeys(api, { tabId, frameIds }) {
  if (typeof api.getPartitionKey !== 'function' || tabId === null) return { keys: [], complete: false };
  const keys = [];
  let complete = true;
  for (const frameId of frameIds) {
    let key = null;
    try {
      const res = await api.getPartitionKey({ tabId, frameId });
      key = res && res.partitionKey;
    } catch {
      // no host access to the top-level site's or the frame's site's origin
      // (the usual case), a frame under an opaque ancestor, or the frame is gone
      key = null;
    }
    if (!key || typeof key.topLevelSite !== 'string' || !key.topLevelSite) {
      complete = false;
      continue;
    }
    const k = { topLevelSite: key.topLevelSite };
    if (typeof key.hasCrossSiteAncestor === 'boolean') k.hasCrossSiteAncestor = key.hasCrossSiteAncestor;
    if (!keys.some((x) => x.topLevelSite === k.topLevelSite && x.hasCrossSiteAncestor === k.hasCrossSiteAncestor)) keys.push(k);
  }
  return { keys, complete };
}

/**
 * The partitioned cookies of these URLs that the tab's frames would send:
 * - with every frame's exact key (Chrome 132+, and only where getPartitionKey
 *   answered for every frame: host access to the sites' own origins, see
 *   the top of this file — seldom, so not the usual path), that key's
 *   partition;
 * - otherwise, when the tab's top-level address is known, every partition
 *   read at once (partitionKey: {}) and only the partitions of that site
 *   kept — the other sites' cookies are dropped right here, in memory. With
 *   the address known, this is what Firefox always does and what Chrome
 *   does on most pages. (An in-page card whose top page the extension
 *   cannot see has the top-level origin its frame reported instead,
 *   cookieScope; one that reported none — Firefox before 153, a "null"
 *   origin — has no address, and then the next branch reads at most the
 *   keys some frames gave, in Firefox none.) It is
 *   wider than the exact key: both hasCrossSiteAncestor partitions of the
 *   site, and the site as sameSiteAsTop tells it (no public suffix list: a
 *   partition of a parent domain the browser makes another site, as
 *   github.io for x.github.io, passes too; NOTES ext-chips). Where
 *   getPartitionKey throws, which is most pages, nothing narrows it;
 * - otherwise the keys some frames gave, or none.
 */
async function partitioned(api, urls, scope) {
  const { keys, complete } = await frameKeys(api, scope);
  const lists = [];
  if (!complete && scope.topUrl) {
    for (const url of urls) {
      const list = await getAllOr(api, { url, ...inStore(scope), partitionKey: {} });
      lists.push(list.filter((c) => isPartitioned(c) && sameSiteAsTop(c.partitionKey.topLevelSite, scope.topUrl)));
    }
    return lists;
  }
  for (const url of urls) {
    for (const key of keys) {
      const list = await getAllOr(api, { url, ...inStore(scope), partitionKey: { ...key } });
      lists.push(list.filter((c) => isPartitioned(c) && c.partitionKey.topLevelSite === key.topLevelSite));
    }
  }
  return lists;
}

/**
 * The cookies the browser would send to each URL from the tab the user
 * sent from, each cookie once, as bridge.Cookie. api: { getAll(details),
 * getPartitionKey?(details) } (the browser's cookies API). No URL, no read:
 * a profile with cookieUrls: [] never touches the cookies API.
 */
export async function readCookiesWith(api, urls, scope = {}) {
  const list = (urls || []).filter((u) => parseHttpUrl(u));
  if (list.length === 0) return [];
  const frameIds = (Array.isArray(scope.frameIds) ? scope.frameIds : []).filter((id) => Number.isInteger(id) && id >= 0);
  const s = {
    tabId: Number.isInteger(scope.tabId) && scope.tabId >= 0 ? scope.tabId : null,
    frameIds: frameIds.length ? frameIds : [0],
    topUrl: parseHttpUrl(scope.topUrl) ? String(scope.topUrl) : '',
    storeId: typeof scope.storeId === 'string' && scope.storeId.length <= 100 ? scope.storeId : '',
  };
  const plain = [];
  for (const url of list) plain.push(await unpartitioned(api, url, s));
  const parted = await partitioned(api, list, s);
  return mergePartitioned(plain, parted);
}
