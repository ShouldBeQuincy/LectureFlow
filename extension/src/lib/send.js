// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Sending a lecture to LectureFlow (SPEC-ext 用户流程 3–4, Native Messaging
// 协议): which tracks are ticked by default, the selection rules (one
// enqueue takes at most one audio track and two other tracks — 双机位,
// ext-brief §3),
// which URLs' cookies are read when the user clicks, and the enqueue and
// refresh requests. Pure: the background reads the cookies and talks to the
// native host (src/background/actions.js).

import { urlKey, parseHttpUrl, requestableOrigin, potentiallyTrustworthy, hostMatches } from './url.js';
import { isBlockedUrl, blockedCategory } from './blocklist.js';
import { RETAIN_LIMITS } from './retain.js';
import { SRC_WEBREQUEST } from './merge.js';

export const GENERIC = 'generic';

// Task states (internal/bridge). A session that is "active" cannot be sent
// again; "imported" means the lecture exists; failed and cancelled tasks may
// be sent again (the queue reuses the row).
export const ACTIVE_STATES = Object.freeze(['queued', 'downloading', 'merging', 'importing']);

/** The tracks of a detection as listed: named ones in rule order, then the rest. */
export function listedTracks(result) {
  if (!result) return [];
  if (result.named) return [...result.named.tracks, ...result.named.others];
  return result.tracks || [];
}

/**
 * The tracks a card lists, at most limit (RETAIN_LIMITS.tracksShown) of
 * them, in list order: whatever the three choices (presetSelection) pick
 * over the whole list, then the first others. So the choices, the default
 * ticks and "续传" pick the same tracks from the shown ones as from all,
 * and a page with any sendable track shows one. Returns { tracks, hidden }
 * (hidden: how many were left out).
 */
export function shownTracks(list, presets = null, limit = RETAIN_LIMITS.tracksShown) {
  const all = Array.isArray(list) ? list : [];
  if (all.length <= limit) return { tracks: all, hidden: 0 };
  const must = new Set();
  for (const p of PRESETS) for (const id of presetSelection(all, p, presets) || []) must.add(id);
  let slots = Math.max(0, limit - must.size);
  const tracks = [];
  for (const t of all) {
    if (must.has(t.id)) tracks.push(t);
    else if (slots > 0) {
      tracks.push(t);
      slots--;
    }
  }
  return { tracks, hidden: all.length - tracks.length };
}

/** The tracks of a detection a card shows, and so the only ones a send may take. */
export function shownOf(result) {
  return shownTracks(listedTracks(result), (result && result.presets) || null).tracks;
}

/** audio, or video (unknown counts with video: LectureFlow takes one audio and up to two other tracks). */
export function groupOf(track) {
  return track.kind === 'audio' ? 'audio' : 'video';
}

/** Whether a track can be sent at all: not DRM-protected, not from a do-not-handle host. */
export function sendable(track) {
  return !track.protected && !(track.url && isBlockedUrl(track.url));
}

/**
 * The three choices the cards offer instead of ticking tracks (user
 * decision, NOTES "用户决定（ext 文案）" 3):
 *   main  主要音视频: the profile's full track (picture and sound in one,
 *         e.g. Panopto's MP4) when there is one; else the main audio plus
 *         the first picture.
 *   dual  双机位: the main audio plus two pictures; only with two or more.
 *   audio 仅音频: the main audio.
 * presets (profile.presets) names the roles of the full and the main audio
 * tracks; without them the first audio track is the main audio. Pictures
 * are the video tracks in list order. Returns track ids, or null when the
 * choice is not possible here.
 */
export const PRESETS = Object.freeze(['main', 'dual', 'audio']);

export function presetSelection(tracks, preset, presets = null) {
  const ok = tracks.filter(sendable);
  const byRoles = (roles) => {
    for (const r of roles || []) {
      const t = ok.find((x) => x.role === r);
      if (t) return t;
    }
    return null;
  };
  const full = presets ? byRoles(presets.full) : null;
  const audio = (presets ? byRoles(presets.audio) : null) || ok.find((t) => t.kind === 'audio') || null;
  const pictures = ok.filter((t) => t.kind === 'video' && t !== full);
  const ids = (list) => list.filter(Boolean).map((t) => t.id);
  if (preset === 'main') {
    if (full) return [full.id];
    const got = ids([audio, pictures[0]]);
    if (got.length) return got;
    return ok[0] ? [ok[0].id] : null;
  }
  if (preset === 'dual') return pictures.length >= 2 ? ids([audio, pictures[0], pictures[1]]) : null;
  if (preset === 'audio') return audio ? [audio.id] : null;
  return null;
}

/**
 * The ticks a card starts with: "主要音视频", or none when the platform says
 * the recording may be restricted (SPEC-ext 默认不勾选; the user picks, and
 * is reminded once before sending).
 */
export function defaultSelection(tracks, { downloadPolicy = 'unknown', presets = null } = {}) {
  if (downloadPolicy === 'disabled') return [];
  return presetSelection(tracks, 'main', presets) || [];
}

/**
 * Ticks or unticks a track. Ticking a track unticks the other track of its
 * group (video is single choice, so is audio). Protected tracks never tick.
 */
export function toggleSelection(tracks, selected, id, on) {
  const t = tracks.find((x) => x.id === id);
  if (!t) return [...selected];
  if (!on) return selected.filter((x) => x !== id);
  if (!sendable(t)) return [...selected];
  const g = groupOf(t);
  const rest = selected.filter((x) => {
    const o = tracks.find((y) => y.id === x);
    return o && x !== id && groupOf(o) !== g;
  });
  return [...rest, id];
}

/**
 * '' when the selection can be sent, else why not. One audio track; one
 * picture, or two with maxVideo 2 (双机位).
 */
export function selectionError(tracks, ids, { maxVideo = 1 } = {}) {
  if (!Array.isArray(ids) || ids.length === 0) return 'empty';
  const picked = [];
  for (const id of ids) {
    const t = tracks.find((x) => x.id === id);
    if (!t) return 'unknown_track';
    if (!sendable(t)) return 'protected';
    if (picked.includes(t)) continue;
    picked.push(t);
  }
  if (picked.filter((t) => groupOf(t) === 'audio').length > 1) return 'too_many_audio';
  if (picked.filter((t) => groupOf(t) === 'video').length > maxVideo) return 'too_many_video';
  return '';
}

/**
 * The URLs whose cookies are read when the user sends (SPEC-ext: 只读轨道所在
 * 域名的 Cookie). request.cookieUrls of the profile: ["track"] (also the
 * default, and for generic detection) reads the cookies the browser would
 * send to each selected track URL; [] reads none.
 */
export function cookieTargets(request, tracks) {
  const kinds = request && Array.isArray(request.cookieUrls) ? request.cookieUrls : ['track'];
  if (!kinds.includes('track')) return [];
  const out = [];
  for (const t of tracks) {
    const k = urlKey(t.url);
    if (k && !out.includes(k)) out.push(k);
  }
  return out;
}

// An IPv4 or IPv6 literal: a cookie for one is only ever host-only.
const isIpLiteral = (host) => /^[0-9.]+$/.test(host) || host.includes(':') || host.startsWith('[');

/**
 * Whether any cookie could be sent to both hosts: the same host, or two
 * names under a common parent of at least two labels (a cookie's Domain
 * cannot be a single label; whether the parent is a public suffix the
 * browser decided when it took the cookie, so a cookie that reaches both
 * really is shared). Only decides whether reading is worth it at all
 * (cookiePlan); cookieAllowed decides cookie by cookie.
 */
export function mayShareCookies(a, b) {
  const x = String(a || '').toLowerCase().replace(/\.$/, '');
  const y = String(b || '').toLowerCase().replace(/\.$/, '');
  if (!x || !y) return false;
  if (x === y) return true;
  if (isIpLiteral(x) || isIpLiteral(y)) return false;
  const p = x.split('.');
  const q = y.split('.');
  let n = 0;
  while (n < p.length && n < q.length && p[p.length - 1 - n] === q[q.length - 1 - n]) n++;
  return n >= 2;
}

/**
 * Whether the browser would send this cookie (cookies.getAll) to that host,
 * as far as its domain goes (RFC 6265 domain matching): a host-only cookie
 * to its own host only, a domain cookie to its domain and every name under
 * it.
 */
export function cookieGoesTo(cookie, host) {
  const d = String((cookie && cookie.domain) || '').toLowerCase().replace(/^\./, '').replace(/\.$/, '');
  const h = String(host || '').toLowerCase().replace(/\.$/, '');
  if (!d || !h) return false;
  if (cookie.hostOnly) return h === d;
  return h === d || h.endsWith(`.${d}`);
}

const hostOf = (raw) => parseHttpUrl(raw)?.hostname.toLowerCase().replace(/\.$/, '') || '';
const bareDomain = (d) => String(d || '').toLowerCase().replace(/^\./, '').replace(/\.$/, '');

// The Domain values of the domain cookies (not host-only) of the
// cookies.getAll answer a cookie came in (withAnswerDomains): a property
// the browser's cookie objects never have, not enumerable, so never copied
// into a message (toBridgeCookie takes named fields only).
export const ANSWER_DOMAINS = Symbol('answerDomains');

/**
 * Marks each cookie of one cookies.getAll answer with the Domain values of
 * that answer's domain cookies (ANSWER_DOMAINS), for cookieAllowed's
 * same-site proof (sameSiteProven); src/background/cookies.js marks every
 * answer. A cookie that cannot be marked has no proof. Returns the list.
 */
export function withAnswerDomains(list) {
  if (!Array.isArray(list)) return list;
  const domains = Object.freeze([...new Set(list.filter((c) => c && typeof c === 'object' && !c.hostOnly).map((c) => bareDomain(c.domain)).filter(Boolean))].sort());
  for (const c of list) {
    try {
      if (c && typeof c === 'object') Object.defineProperty(c, ANSWER_DOMAINS, { value: domains, configurable: true, enumerable: false });
    } catch {
      // a frozen object: no proof from it
    }
  }
  return list;
}

const answerDomains = (cookie) => (cookie && Array.isArray(cookie[ANSWER_DOMAINS]) ? cookie[ANSWER_DOMAINS] : []);

/**
 * Whether two hosts are proven the same site without a public suffix list
 * (SEC46 低-3): the same host; or, among `domains` (the Domain values of
 * domain cookies the browser holds for the track: ANSWER_DOMAINS), the
 * hosts' nearest common parent itself, of two labels or more, with each
 * host that parent or one label below it. The browser took a cookie for
 * that Domain only because the parent is not a public suffix (no public
 * suffix can take a domain cookie, evil.github.io and a.co.uk included),
 * and with no label between the parent and either host there is no name
 * for a public suffix in between either: both hosts are on the parent's
 * site. A common parent further up (storage.example above the public
 * suffix s3.storage.example) proves nothing. What it does not cover: a
 * public suffix list entry that makes every child of the parent a public
 * suffix ("*.parent") while the parent itself is not one, or a host that is
 * itself a public suffix — then a domain cookie on the parent, which one
 * child can set for all, joins two sites; cookieAllowed takes the proof
 * only for a page or frame on a host the user granted, so that case needs
 * the user to have granted both sites.
 */
export function sameSiteProven(a, b, domains) {
  const x = bareDomain(a);
  const y = bareDomain(b);
  if (!x || !y) return false;
  if (x === y) return true;
  if (isIpLiteral(x) || isIpLiteral(y)) return false;
  const p = x.split('.');
  const q = y.split('.');
  let n = 0;
  while (n < p.length && n < q.length && p[p.length - 1 - n] === q[q.length - 1 - n]) n++;
  if (n < 2 || p.length - n > 1 || q.length - n > 1) return false;
  const parent = p.slice(p.length - n).join('.');
  return Array.isArray(domains) && domains.includes(parent);
}

/**
 * The tab's top-level site as the browser computed it, from Chrome's
 * cookies.getPartitionKey({ tabId, frameId: 0 }) (132+): { partitionKey:
 * { topLevelSite: "https://uni.example", hasCrossSiteAncestor } } → its
 * host name, lower case ("uni.example"): the top page's registrable domain
 * by the browser's own public suffix list, or the host itself when it has
 * none (an address, a public suffix). '' for anything else — no answer,
 * another shape, not an http(s) origin — and then a send goes by the cookie
 * proof alone, as before (cookieAllowed; src/background/actions.js).
 * Chrome answers only with host access to that site's own origin
 * ("https://uni.example/", not the page's "https://lms.uni.example/"): on
 * most pages, granted by their host, it throws (SEC51 低-1); it answers
 * where the top host is its own site (evil.github.io, an address) or the
 * user granted the bare site.
 */
export function topSiteOf(answer) {
  const key = answer && typeof answer === 'object' ? answer.partitionKey : null;
  const site = key && typeof key === 'object' ? key.topLevelSite : null;
  if (typeof site !== 'string' || !site || site.length > 300) return '';
  const u = parseHttpUrl(site);
  if (!u || !u.hostname || u.username || u.password || u.pathname !== '/' || u.search || u.hash) return '';
  return bareDomain(u.hostname);
}

// How many entries of location.ancestorOrigins a scan keeps
// (src/content/scan.js): a list that long may have lost its top-level page.
export const MAX_ANCESTORS = 64;

/**
 * The hosts of a frame's location.ancestorOrigins as the scan read it
 * (nearest first, the top-level page last; Chrome, Firefox 153+), lower
 * case, '' for an entry that is not an http(s) origin ("null": a sandboxed
 * frame; per the HTML standard also one embedded with
 * referrerpolicy="no-referrer", which Chrome 156 does not hide, SEC51).
 * null when the frame reported none (the top frame; Firefox before 153).
 * For cookiePlan (result.frameAncestors, src/background/detector.js) only.
 */
export function ancestorHosts(list) {
  if (!Array.isArray(list) || list.length === 0) return null;
  return list.slice(0, MAX_ANCESTORS).map((o) => {
    const s = typeof o === 'string' ? o : '';
    const u = s && s !== 'null' && s.length <= 2048 ? parseHttpUrl(s) : null;
    return u ? bareDomain(u.hostname) : '';
  });
}

/** Whether a host is the site (a host name, topSiteOf) or under it; an address only when it is the same. */
function onSite(host, site) {
  const h = bareDomain(host);
  const s = bareDomain(site);
  if (!h || !s) return false;
  if (h === s) return true;
  if (isIpLiteral(h) || isIpLiteral(s)) return false;
  return h.endsWith(`.${s}`);
}

/** Whether a host is one of a profile's declared hosts (its permission patterns, profile.origins). */
function declaredHost(profile, host) {
  for (const o of (profile && Array.isArray(profile.origins) ? profile.origins : [])) {
    const m = /^[a-z*]+:\/\/([^/]+)\/\*$/.exec(String(o));
    if (m && hostMatches(m[1], host)) return true;
  }
  return false;
}

/**
 * The hosts whose media, a profile says, need no cookie (request.cookieUrls
 * without "track", as Panopto's []): its match.hosts and match.mediaHosts.
 * A generic send reads no cookie of them either (cookiePlan), so that what
 * the profile promises holds on pages it does not cover too.
 * profiles: [{ profile }] (src/background/profiles.js allProfiles).
 */
export function quietHostsOf(profiles) {
  const out = new Set();
  for (const e of Array.isArray(profiles) ? profiles : []) {
    const p = e && e.profile;
    const kinds = p && p.request && Array.isArray(p.request.cookieUrls) ? p.request.cookieUrls : null;
    if (!kinds || kinds.includes('track') || !p.match) continue;
    for (const h of [...(p.match.hosts || []), ...(p.match.mediaHosts || [])]) if (typeof h === 'string') out.add(h.toLowerCase());
  }
  return [...out].sort();
}

/** The frames that referenced or requested a track (detect.js: the top frame when it says none). */
const framesOf = (t) => (Array.isArray(t.frameIds) && t.frameIds.length ? t.frameIds : [0]);

/**
 * Which cookies a send may hand over (security audit AUD3 中-1): the page
 * alone must not be able to name an address and so get another site's
 * cookies handed to LectureFlow. For each URL cookieTargets names, one of:
 *   { url, all: true } — a host the matching profile declares (match.hosts,
 *     match.mediaHosts; profile.origins): every cookie the browser has for
 *     the URL, as before;
 *   { url, all: false, topHost, siteHosts, frameHosts, thirdParty, grantedHosts, topSite? } — any other host
 *     (generic detection always): only the cookies the browser would itself
 *     attach to the request in this tab, as cookieAllowed reads them:
 *     ① those it also sends to the tab's top-level page (topHost: same
 *       site, so even SameSite=Lax or Strict) and to a frame on that site
 *       that referenced or requested the track (siteHosts) — a request from
 *       a frame of another site gets no Lax or Strict cookie from the
 *       browser, whatever page it sits in (SEC46 低-1, B1);
 *     ② SameSite=None ones that the site of a frame that referenced the
 *       track shares (frameHosts) — only when the browser itself requested
 *       the track (or the manifest it came from) in this tab, the request
 *       records ("webrequest" among its sources) — and only where the
 *       browser sends such a frame its unpartitioned cookies at all
 *       (thirdParty "shared", from result.thirdPartyCookies: the Chrome
 *       build). Firefox keys an embedded frame's cookies by the top-level
 *       site (Total Cookie Protection, on by default; SEC46 低-2): there
 *       (thirdParty "partitioned", also when a result does not say) ② hands
 *       over the tab's partition only, which the entry still has read.
 *     Under ① and ② also the track host's own host-only cookies, where the
 *     browser would send them: when the track host is proven on the site
 *     of the page or frame (sameSiteProven: a domain cookie of the browser's
 *     on their nearest common parent) and that page or frame is on a host
 *     the user granted (grantedHosts, from result.grantedHosts; SEC46 低-3).
 *     The tab's partition (src/lib/partition.js) is read as before.
 *   topSite (Chrome 132+: the tab's top-level site as the browser computed
 *   it, topSiteOf, where the extension has host access to the site's own
 *   origin — rarely, SEC51 低-1; '' or absent elsewhere): rides along in the entry, and a
 *   frame counts as on the page's site (siteHosts) only when it is on that
 *   site too; under ① the track host's own host-only cookies then also need
 *   the track host, the page and those frames on it (cookieAllowed; SEC48
 *   低-1: a parent-domain cookie no longer passes for a page the browser
 *   makes a site of its own). It only ever takes away.
 * The frames are the track's frameIds: documents the scan read that showed
 * the address, and the frames of request records, which belong to a
 * scanned document (Chrome: its document id) or came at request time from
 * the origin of the document the scan read in that frame (Firefox: the
 * record's initiator; src/background/requests.js), so a frame's host here
 * is the host the request came from. A URL for which no cookie could pass
 * (no host above shares a cookie domain with it: mayShareCookies), or on a
 * host whose media a profile says need no cookie (quietHosts, from
 * quietHostsOf), is left out, so its cookies are not read at all. A page
 * that only writes another site's address, or loads it from its own
 * document, gets none of that site's cookies. topUrl: the tab's top-level
 * address (the popup's detection, the in-page card's sender);
 * result.frameHosts: frame id → host of the frames the detection scanned
 * (src/background/detector.js); a result without it (a listed session's,
 * src/lib/inpage.js) has the host of its pageUrl for every frame, for ①
 * only. result.frameAncestors: frame id → the hosts of the frame's
 * location.ancestorOrigins (ancestorHosts; Chrome, Firefox 153+): a frame
 * that reported them counts as on the page's site (siteHosts) only when
 * every one of them is too (SEC48 低-2: Chrome judges SameSite by the whole
 * ancestor chain), a chain of MAX_ANCESTORS never; a frame without them, as
 * before. An unreadable entry ("null"): in the Chrome build (result
 * thirdPartyCookies "shared") off the site — Chrome sends a frame under an
 * opaque ancestor no Lax or Strict cookie and does not hide an ancestor for
 * referrerpolicy="no-referrer" (SEC51); elsewhere (the Firefox build, a
 * result that does not say) it says nothing, and the frame counts as one
 * that reported no ancestors (SEC51 低-2: Firefox might hide a same-site
 * embedding's ancestor and still send its cookies). It only ever takes
 * away.
 */
export function cookiePlan(result, tracks, topUrl, { quietHosts = [], topSite = '' } = {}) {
  const r = result || {};
  const urls = cookieTargets(r.request, tracks || []);
  const topHost = hostOf(topUrl);
  const site = typeof topSite === 'string' ? bareDomain(topSite) : '';
  // on the page's site as far as can be told: may share a cookie with the
  // top page and, with the browser's top-level site, on it
  const onTopSite = (h) => !!h && !!topHost && mayShareCookies(h, topHost) && (!site || onSite(h, site));
  // and every ancestor a frame reported with it (SEC48 低-2: a frame of the
  // page's site inside another site's frame gets no Lax or Strict cookie
  // from the browser); a frame that reported none, as before
  const ancestry = r.frameAncestors && typeof r.frameAncestors === 'object' ? r.frameAncestors : null;
  const unreadableIsOff = r.thirdPartyCookies === 'shared'; // the Chrome build
  const ancestorsOnTopSite = (id) => {
    const chain = ancestry && Array.isArray(ancestry[id]) ? ancestry[id] : [];
    if (chain.length === 0) return true;
    // the Firefox build: a hidden ancestor makes the chain say nothing
    if (!unreadableIsOff && chain.includes('')) return true;
    return chain.length < MAX_ANCESTORS && chain.every(onTopSite);
  };
  const scanned = r.frameHosts && typeof r.frameHosts === 'object' ? r.frameHosts : null;
  const frameHost = (id) => (scanned && typeof scanned[id] === 'string' ? scanned[id] : '');
  const pageHost = scanned ? '' : hostOf(r.pageUrl);
  const thirdParty = r.thirdPartyCookies === 'shared' ? 'shared' : 'partitioned';
  const granted = new Set(Array.isArray(r.grantedHosts) ? r.grantedHosts : []);
  const plan = [];
  for (const url of urls) {
    const host = hostOf(url);
    const same = (tracks || []).filter((t) => urlKey(t.url) === url);
    if (r.profile && declaredHost(r.profile, host)) {
      plan.push({ url, all: true });
      continue;
    }
    if ((quietHosts || []).some((h) => hostMatches(h, host))) continue;
    const siteHosts = new Set();
    const frameHosts = new Set();
    for (const t of same) {
      const requested = Array.isArray(t.sources) && t.sources.includes(SRC_WEBREQUEST);
      for (const id of framesOf(t)) {
        const h = frameHost(id);
        const own = h || pageHost;
        if (onTopSite(own) && ancestorsOnTopSite(id)) siteHosts.add(own);
        if (requested && h && mayShareCookies(h, host)) frameHosts.add(h);
      }
    }
    const top = topHost && siteHosts.size > 0 && mayShareCookies(topHost, host) ? topHost : '';
    if (!top && frameHosts.size === 0) continue;
    const sites = top ? [...siteHosts].sort() : [];
    const frames = [...frameHosts].sort();
    const grantedHosts = [...new Set([top, ...sites, ...frames])].filter((h) => h && granted.has(h)).sort();
    const entry = { url, all: false, topHost: top, siteHosts: sites, frameHosts: frames, thirdParty, grantedHosts };
    if (site) entry.topSite = site;
    plan.push(entry);
  }
  return plan;
}

/**
 * Whether one (unpartitioned) cookie that cookies.getAll gave for a URL of
 * cookiePlan may be handed over under its entry. The proof for a track
 * host's own host-only cookie comes from the same answer (ANSWER_DOMAINS,
 * marked by src/background/cookies.js); an unmarked cookie has none.
 */
export function cookieAllowed(cookie, entry) {
  if (!cookie || !entry) return false;
  if (entry.all) return true;
  const sites = entry.siteHosts || [];
  // ① the page's site: a cookie of the top page that also goes to the frame that asked
  if (entry.topHost && cookieGoesTo(cookie, entry.topHost) && sites.some((h) => cookieGoesTo(cookie, h))) return true;
  // The track host's own host-only cookie (SEC46 低-3), which the browser
  // sends wherever it sends that host's cookies of the same SameSite: under
  // ① and ② when the track host is proven on the site of the page or frame
  // (sameSiteProven), and that page or frame is on a host the user granted
  // (grantedHosts; an attacker's page the user only opened is not one).
  const host = hostOf(entry.url);
  const own = !!cookie.hostOnly && !!host && bareDomain(cookie.domain) === host;
  const granted = entry.grantedHosts || [];
  const proof = own ? answerDomains(cookie) : [];
  const proven = (h) => granted.includes(h) && sameSiteProven(host, h, proof);
  // Under ① also, with the tab's top-level site as the browser computed it
  // (entry.topSite, Chrome 132+; SEC48 低-1): the track host, the page and
  // every frame on its site that asked are on that site — on top of the
  // proof, never instead of it.
  const onTop = !entry.topSite || (onSite(host, entry.topSite) && onSite(entry.topHost, entry.topSite) && sites.every((h) => onSite(h, entry.topSite)));
  if (own && entry.topHost && onTop && proven(entry.topHost) && sites.some((h) => h === entry.topHost || h === host || proven(h))) return true;
  // ② an embedded player's site: SameSite=None only, and none unpartitioned in Firefox
  if (entry.thirdParty !== 'shared' || cookie.sameSite !== 'no_restriction') return false;
  const frames = entry.frameHosts || [];
  return frames.some((h) => cookieGoesTo(cookie, h)) || (own && frames.some(proven));
}

/**
 * Site access the popup offers ("在此网站自动探测"; security review L4): the
 * page's own origin, and apart from it the hosts whose cookies a send would
 * read — the sendable tracks' hosts other than the page's, when the profile
 * reads cookies at all (cookieUrls "track", also generic detection), of
 * the tracks a card shows (shownTracks), and only those whose cookies the
 * send could hand over at all (cookiePlan, from the page: a host the
 * profile declares, one that may share a cookie with the page, or the site
 * of an embedded player whose request the browser made; AUD3 中-1), and a
 * host that may share a cookie with a scanned frame that showed or
 * requested the track, though the browser's request is not on record yet
 * (SEC46 低-3: an embedded player's CDN, which the browser reports only
 * once the user granted it; offering it reads and hands over nothing — a
 * send still goes by cookiePlan); not a host whose media a profile says
 * need no cookie (result.quietHosts, from quietHostsOf: a send reads none
 * there; SEC46 信-2) — for the user to tick one by one. HTTPS only, as the
 * manifest offers. Returns { page, cookieHosts }.
 */
export function accessOffer(result) {
  if (!result || result.status !== 'ok') return { page: '', cookieHosts: [] };
  const page = requestableOrigin(result.pageUrl);
  if (!page) return { page: '', cookieHosts: [] };
  const hosts = new Set();
  const quietHosts = Array.isArray(result.quietHosts) ? result.quietHosts : [];
  const offer = (u) => {
    const o = requestableOrigin(u);
    if (o && o !== page) hosts.add(o);
  };
  // the shown tracks only: a track the card does not list cannot be sent
  const shown = shownOf(result).filter(sendable);
  for (const { url } of cookiePlan(result, shown, result.pageUrl, { quietHosts })) offer(url);
  // a scanned frame's own host (never the page's address for a frame the
  // scan did not read) that may share a cookie with the track's
  const scanned = result.frameHosts && typeof result.frameHosts === 'object' ? result.frameHosts : {};
  for (const url of cookieTargets(result.request, shown)) {
    const host = hostOf(url);
    if (quietHosts.some((h) => hostMatches(h, host))) continue;
    const frames = shown.filter((t) => urlKey(t.url) === url).flatMap(framesOf);
    if (frames.some((id) => typeof scanned[id] === 'string' && mayShareCookies(scanned[id], host))) offer(url);
  }
  return { page, cookieHosts: [...hosts].sort() };
}

/** A browser cookie (cookies.getAll) as bridge.Cookie. */
export function toBridgeCookie(c) {
  const exp = typeof c.expirationDate === 'number' && Number.isFinite(c.expirationDate) ? Math.floor(c.expirationDate) : 0;
  return {
    name: String(c.name ?? ''),
    value: String(c.value ?? ''),
    domain: String(c.domain ?? ''),
    path: String(c.path || '/'),
    expires: exp > 0 ? exp : 0,
    secure: !!c.secure,
    httpOnly: !!c.httpOnly,
    hostOnly: !!c.hostOnly,
  };
}

/** Cookies of several URLs, each once (name, domain and path). */
export function mergeCookies(lists) {
  const seen = new Set();
  const out = [];
  for (const list of lists) {
    for (const c of list || []) {
      const b = toBridgeCookie(c);
      const k = `${b.name}\u0000${b.domain}\u0000${b.path}`;
      if (seen.has(k)) continue;
      seen.add(k);
      out.push(b);
    }
  }
  return out;
}

/**
 * Referer, as a browser with its default policy (strict-origin-when-cross-
 * origin) would send it for these tracks (security review L2): the page
 * address without its fragment when every track is on the page's origin;
 * only the page's origin when any track is on another; nothing from an
 * HTTPS page to an HTTP track that is not potentially trustworthy (a
 * loopback host is: browsers send it the origin, and so does LectureFlow,
 * internal/fetch refererFor), nor when the profile says not to send it.
 */
export function refererFor(result, tracks = []) {
  if (result.request && result.request.sendReferer === false) return '';
  const u = parseHttpUrl(result.pageUrl);
  if (!u) return '';
  u.hash = '';
  u.username = '';
  u.password = '';
  let crossOrigin = false;
  for (const t of tracks || []) {
    const v = parseHttpUrl(t.url);
    if (!v) continue;
    if (u.protocol === 'https:' && !potentiallyTrustworthy(v)) return '';
    if (v.origin !== u.origin) crossOrigin = true;
  }
  return crossOrigin ? `${u.origin}/` : u.href;
}

/**
 * A track name for people: the profile's label, else "Video · 720p" or
 * "Audio · English" — the playlist's own name for a rendition (HLS NAME)
 * before its language code (T26 J5).
 */
export function trackName(track, msg) {
  if (track.label) return track.label;
  const parts = [msg(`kind_${track.kind}`)];
  if (track.quality) parts.push(track.quality);
  if (track.name || track.language) parts.push(track.name || track.language);
  return parts.join(' · ');
}

/**
 * The names a card shows for its tracks (id → name): trackName, with tracks
 * of the same name numbered in list order ("Audio 1", "Audio 2"), so that
 * two of them can be told apart (T26 G12).
 */
export function trackNames(tracks, msg) {
  const base = (tracks || []).map((t) => [t.id, trackName(t, msg)]);
  const count = new Map();
  for (const [, n] of base) count.set(n, (count.get(n) || 0) + 1);
  const seen = new Map();
  return new Map(
    base.map(([id, n]) => {
      if (count.get(n) < 2) return [id, n];
      seen.set(n, (seen.get(n) || 0) + 1);
      return [id, `${n} ${seen.get(n)}`];
    }),
  );
}

function bridgeTrack(t, msg) {
  const out = {
    url: t.url,
    kind: t.kind,
    format: t.format,
    role: t.role || '',
    label: trackName(t, msg).slice(0, 200),
    sizeBytes: t.sizeBytes > 0 ? t.sizeBytes : -1,
  };
  if (t.quality) out.quality = t.quality;
  if (t.selector) out.selector = t.selector;
  return out;
}

/** The selected tracks, in list order. */
export function pickTracks(result, ids) {
  return listedTracks(result).filter((t) => ids.includes(t.id));
}

/**
 * Why a send or "续传" must not go out at all: the page, the tab's
 * top-level page (topUrl) or a ticked track is on a do-not-handle site
 * (src/lib/blocklist.js) — the category, or ''. Detection already keeps
 * such pages and tracks out; this is the last check before cookies are
 * read.
 */
export function sendBlockedCategory(result, ids, topUrl = '') {
  const c = blockedCategory(result && result.pageUrl) || blockedCategory(topUrl);
  if (c) return c;
  for (const t of pickTracks(result || {}, ids || [])) {
    const k = blockedCategory(t.url);
    if (k) return k;
  }
  return '';
}

/** The platform half of the dedup key: the profile id, or "generic". */
export function platformOf(result) {
  return result.profile ? result.profile.id : GENERIC;
}

/**
 * The enqueue request (bridge.EnqueueRequest without type and id). Generic
 * detection sends the page's own title (src/lib/profile.js pageTitleFor),
 * or an empty one when the page has none worth using and LectureFlow names
 * the lecture (lead ruling 9).
 */
export function buildEnqueue({ result, ids, courseId, cookies, overrideDisabled, msg }) {
  const tracks = pickTracks(result, ids);
  const lecture = result.lecture || { title: '', recordedAt: '', durationSec: 0 };
  return {
    profile: result.profile
      ? { id: result.profile.id, version: result.profile.version, reviewed: !!result.profile.reviewed }
      : { id: GENERIC, version: '', reviewed: false },
    platform: platformOf(result),
    sessionId: result.sessionId,
    lecture: {
      title: String(lecture.title || ''),
      recordedAt: String(lecture.recordedAt || ''),
      durationSec: Number.isFinite(lecture.durationSec) && lecture.durationSec > 0 ? Math.round(lecture.durationSec) : 0,
    },
    courseId: String(courseId),
    tracks: tracks.map((t) => bridgeTrack(t, msg)),
    referer: refererFor(result, tracks),
    cookies: cookies || [],
    maxConnections: (result.request && result.request.maxConnectionsPerFile) || 0,
    overrideDisabled: result.downloadPolicy === 'disabled' && !!overrideDisabled,
  };
}

/** The refresh request (bridge.RefreshRequest without type and id). */
export function buildRefresh({ result, ids, cookies, msg }) {
  const tracks = pickTracks(result, ids);
  return {
    platform: platformOf(result),
    sessionId: result.sessionId,
    tracks: tracks.map((t) => bridgeTrack(t, msg)),
    referer: refererFor(result, tracks),
    cookies: cookies || [],
  };
}

/**
 * What a check item means for the popup:
 * new | imported | active | needs_refresh | again (failed or cancelled:
 * may be sent again).
 */
export function checkOutcome(item) {
  if (!item) return 'new';
  if (item.state === 'imported') return 'imported';
  if (item.state === 'needs_refresh') return 'needs_refresh';
  if (ACTIVE_STATES.includes(item.state)) return 'active';
  if (item.state === 'failed' || item.state === 'cancelled') return 'again';
  return 'new';
}
