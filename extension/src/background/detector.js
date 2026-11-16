// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Runs detection on a tab: scans the frames (element and resource-timing
// routes), adds the webRequest route, lets src/lib/detect.js do the rest
// with page-context fetches, then applies the matching profile. Keeps the
// last result per tab in memory for the popup, the badge and the report.

import { browser, BUILD, extensionVersion, browserName, uiLocale } from './env.js';
import { scanFrame, frameTitle, pageFetch } from '../content/scan.js';
import { requestsFor, expectOwn } from './requests.js';
import { allProfiles } from './profiles.js';
import { resetRegexBudget } from '../lib/regexguard.js';
import { isAuthorized } from './sites.js';
import { detectTracks, summarize, boundScan, boundReads, LIMITS } from '../lib/detect.js';
import { findProfile, selectorReads, applyProfile, sessionIdFor, lectureFor, downloadPolicyFor, readResults, profilePermissionOrigins, profileStopped, GENERIC } from '../lib/profile.js';
import { combineObserved, observeConfig } from '../lib/observe.js';
import { blockedCategory, isBlockedUrl } from '../lib/blocklist.js';
import { screenScan, titleOfFrame } from '../lib/blockedframes.js';
import { listedTracks, sendable, accessOffer, shownTracks, quietHostsOf, ancestorHosts } from '../lib/send.js';
import { RETAIN_LIMITS, retainDetection, dropTexts, evictTexts, scanChars } from '../lib/retain.js';
import { buildReport, reportFileName } from '../lib/report.js';
import { longestDuration } from '../lib/merge.js';
import { parseHttpUrl, originPattern, urlKey } from '../lib/url.js';
import { backgroundFetcher, MAX_MANIFEST_BYTES } from '../lib/bgfetch.js';
import { tokens } from '../ui/tokens.js';

// tabId, or "tabId:frameId" for one frame → last detection (memory only;
// what it keeps of the manifests it read is bounded, src/lib/retain.js)
const results = new Map();
const running = new Map(); // same keys → Promise
let retainLimits = RETAIN_LIMITS;

/** Tests only: other retention limits (src/lib/retain.js), or the real ones again with none. */
export function setRetainLimits(limits = null) {
  retainLimits = limits ? Object.freeze({ ...RETAIN_LIMITS, ...limits }) : RETAIN_LIMITS;
}

let scanLimits = LIMITS;

/** Tests only: other scan limits (src/lib/detect.js LIMITS, boundScan), or the real ones again with none. */
export function setScanLimits(limits = null) {
  scanLimits = limits ? Object.freeze({ ...LIMITS, ...limits }) : LIMITS;
}

const keyOf = (tabId, frameId = 0) => (frameId ? `${tabId}:${frameId}` : tabId);

/** Forgets a tab's detections (its frames' too). */
export function forgetTab(tabId) {
  for (const k of [...results.keys()]) if (k === tabId || String(k).startsWith(`${tabId}:`)) results.delete(k);
}

/** The last detection of a tab, or of one frame of it (an in-page button in an LMS frame). */
export function lastResult(tabId, frameId = 0) {
  return results.get(keyOf(tabId, frameId)) || null;
}

// How long an injected function may take: a page whose main thread hangs
// never answers executeScript, and detection gives up on it instead of
// waiting for ever (SEC15 I3).
let injectTimeoutMs = 15000;

/** Tests only: the injection timeout in milliseconds. */
export function setInjectTimeout(ms) {
  injectTimeoutMs = ms;
}

// The browser runs an injection once the frame has loaded (document_idle):
// one that times out is most often a page still loading (SEC15 R2-5).
class InjectionTimeout extends Error {
  constructor() {
    super('injection timed out');
    this.timedOut = true;
  }
}

async function injectInto(target, func, args) {
  let timer;
  const late = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new InjectionTimeout()), injectTimeoutMs);
  });
  try {
    return await Promise.race([browser.scripting.executeScript({ target, func, args }), late]);
  } finally {
    clearTimeout(timer);
  }
}

async function inject(tabId, frameIds, func, args) {
  return injectInto(frameIds === 'all' ? { tabId, allFrames: true } : { tabId, frameIds }, func, args);
}

/**
 * Scans every frame we may read (or just frameIds); [] when the page is off
 * limits. timedOut: the frames scanned last did not answer in time (the
 * page is still loading, or hangs), rather than refusing.
 */
async function scan(tabId, reads, only = null) {
  let res;
  try {
    res = await inject(tabId, only || 'all', scanFrame, [reads]);
  } catch (e) {
    if (only) return { frames: [], error: 'no access', timedOut: !!(e && e.timedOut) };
    // Some browsers refuse the whole call when one frame is off limits:
    // the top frame alone still tells most.
    try {
      res = await inject(tabId, [0], scanFrame, [reads]);
    } catch (e2) {
      return { frames: [], error: String(e2 && e2.message ? e2.message : e2), timedOut: !!(e2 && e2.timedOut) };
    }
  }
  const frames = [];
  for (const r of res || []) {
    if (!r || !r.result) continue;
    frames.push({ frameId: r.frameId ?? 0, documentId: r.documentId || null, ...r.result });
  }
  return { frames, error: '' };
}

/**
 * The detection's fetches: from the page first (pageFetch), then from the
 * extension itself (src/lib/bgfetch.js). frameUrl is the address of the
 * frame detection runs in as the browser gave it (tabs.get, or the
 * sender of an in-page request), never one a page reported. match is the
 * matching profile (or null): its declared hosts count only when it
 * matched that frame itself; when it matched embedded frames, only those
 * frames' own references on their own origin (SEC15 L1).
 *
 * The fetches stay with the documents the scan read (SEC33 信-1): a page
 * fetch is pinned to the scanned document where the browser names it
 * (documentOf: Chrome's documentIds; Firefox has none) and carries the
 * scan's mark of that document (tokenOf; or the address it was scanned
 * at, from a scan without one), which the page checks before sending
 * anything. A frame whose page says it shows another document, or whose
 * pinned injection is refused (that document answered the scan, so it is
 * gone), is gone: nothing more is fetched in it or, from the background,
 * for addresses only gone frames gave (SEC36 低-9: one embedded frame that
 * moves on costs only its own part). When that frame is the one detection
 * runs in (`self`: the top frame, or an in-page button's), or `still` (the
 * top-level detection: the tab still shows its page) says no, nothing more
 * is fetched at all and `moved` is true: the detection belongs to a page no
 * longer there. `gone` lists the other frames that moved on.
 */
function makeIo(tabId, frames, { frameUrl, match, self = 0, documentOf = new Map(), tokenOf = new Map(), still = null }) {
  const frameOrigin = new Map(frames.map((f) => [f.frameId, parseHttpUrl(f.url)?.origin || '']));
  const scannedAt = new Map(frames.map((f) => [f.frameId, typeof f.url === 'string' ? f.url : '']));
  let moved = false;
  const goneFrames = new Set();
  const stopped = async () => {
    if (!moved && still && !(await still())) moved = true;
    return moved;
  };
  const leave = (frameId) => {
    if (frameId === self) moved = true;
    else goneFrames.add(frameId);
  };
  // only gone frames gave this address
  const orphan = (frameIds) => Array.isArray(frameIds) && frameIds.length > 0 && frameIds.every((id) => goneFrames.has(id));
  // A page fetch runs only in a frame that referenced the URL, the one on
  // the URL's origin first; never in an unrelated frame that merely shares
  // the URL's origin (SEC15 L2: a page embedding another site's frame).
  const pickFrame = (url, frameIds) => {
    const origin = parseHttpUrl(url)?.origin;
    const known = (frameIds || []).filter((id) => frameOrigin.has(id) && !goneFrames.has(id));
    return known.find((id) => frameOrigin.get(id) === origin) ?? known[0] ?? null;
  };
  const viaPage = async (url, frameIds, req) => {
    const frameId = pickFrame(url, frameIds);
    if (frameId === null || (await stopped())) return null;
    expectOwn(tabId, url);
    const documentId = documentOf.get(frameId) || null;
    const target = documentId ? { tabId, documentIds: [documentId] } : { tabId, frameIds: [frameId] };
    const token = tokenOf.get(frameId);
    try {
      const res = await injectInto(target, pageFetch, [[req], null, token ? { token } : { url: scannedAt.get(frameId) || '' }]);
      const out = res && res[0] && res[0].result ? res[0].result[0] : null;
      if (out && out.error === 'moved') {
        leave(frameId);
        return null;
      }
      return out;
    } catch (e) {
      // a page that does not answer in time is still there (the fallback below)
      if (documentId && !(e && e.timedOut)) leave(frameId);
      return null;
    }
  };
  const topMatch = !!match && match.url === frameUrl;
  const embedded = match && !topMatch ? frames.filter((f) => f.url === match.url).map((f) => ({ frameId: f.frameId, origin: frameOrigin.get(f.frameId) })) : [];
  // Fallback from the extension itself, only for hosts the user granted;
  // cookies as above, read with a size cap and one timeout (security
  // review M-2, L-4).
  const background = backgroundFetcher({
    fetch: (url, init) => fetch(url, init),
    permitted: async (url) => {
      const pattern = originPattern(url);
      return !!pattern && (await browser.permissions.contains({ origins: [pattern] }).catch(() => false));
    },
    frameUrl,
    profile: topMatch ? match.profile : null,
    embedded,
    maxBytes: MAX_MANIFEST_BYTES,
  });
  const viaBackground = async (url, mode, frameIds) => {
    if ((await stopped()) || orphan(frameIds)) return null;
    expectOwn(tabId, url);
    const ctx = { frameIds: Array.isArray(frameIds) ? frameIds : [] };
    return mode === 'probe' ? background.probe(url, ctx) : background.text(url, ctx);
  };
  return {
    get moved() {
      return moved;
    },
    gone: goneFrames,
    async fetchText(url, ctx) {
      const page = await viaPage(url, ctx?.frameIds, { url, mode: 'text', maxBytes: MAX_MANIFEST_BYTES });
      if (page && page.ok && !moved) return page;
      return (await viaBackground(url, 'text', ctx?.frameIds)) || (moved ? null : page) || { ok: false, status: 0, contentType: '', text: '', error: 'unreachable' };
    },
    async probeFile(url, ctx) {
      const page = await viaPage(url, ctx?.frameIds, { url, mode: 'probe' });
      if (page && page.ok && page.size > 0 && !moved) return page;
      return (await viaBackground(url, 'probe', ctx?.frameIds)) || (moved ? null : page) || { ok: false, status: 0, contentType: '', size: -1, error: 'unreachable' };
    },
  };
}

/**
 * Detects the tracks of a tab. Returns the stored result:
 * { status: 'ok' | 'no_access' | 'not_a_page', tabId, at, pageUrl, authorized,
 *   profile, sessionId, sessionSource, lecture, downloadPolicy, tracks,
 *   named, summary, raw }, or, not stored: { status: 'navigated', tabId }
 *   when the tab moved to another address before detection finished, or
 *   the frame detection runs in (the top one, or an in-page button's)
 *   showed another document when detection fetched from it (makeIo; an
 *   embedded frame that did costs only its own fetches and tracks);
 *   { status: 'page_loading', tabId } when the page did not answer the
 *   scan in time (injectTimeoutMs; it has not finished loading).
 */
export function detectTab(tabId, opts = {}) {
  const key = keyOf(tabId, opts.frameId || 0);
  if (running.has(key) && !opts.force) return running.get(key);
  const p = run(tabId, opts).finally(() => running.delete(key));
  running.set(key, p);
  return p;
}

/**
 * opts.frameId (with opts.frameUrl): detect within that frame only — the
 * in-page button of a player embedded in another site's page (an LMS),
 * where the top page is not ours to read.
 */
async function run(tabId, opts = {}) {
  const frameId = opts.frameId || 0;
  const key = keyOf(tabId, frameId);
  let pageUrl = '';
  if (frameId) pageUrl = opts.frameUrl || '';
  else {
    try {
      pageUrl = (await browser.tabs.get(tabId)).url || '';
    } catch {
      return { status: 'no_access', tabId };
    }
  }
  if (!pageUrl) return store(key, { status: 'no_access', tabId });
  const page = parseHttpUrl(pageUrl);
  if (!page) return store(key, { status: 'not_a_page', tabId });
  // YouTube and course platforms: not handled at all (src/lib/blocklist.js).
  const pageBlocked = blockedCategory(pageUrl);
  if (pageBlocked) return unsupported(key, tabId, frameId, pageBlocked);

  const profiles = await allProfiles();
  // one detection, one budget of the runtime regex guard (src/lib/regexguard.js)
  resetRegexBudget();
  const only = frameId ? [frameId] : null;
  // First pass: media, resources and frame addresses.
  const first = await scan(tabId, [], only);
  // A page that has not finished loading: the popup says so and asks to try
  // again shortly; nothing is stored, so the next look detects again.
  if (first.frames.length === 0 && first.timedOut) return { status: 'page_loading', tabId };
  if (first.frames.length === 0) return store(key, { status: 'no_access', tabId, error: first.error });
  // The request records of the documents scanned (requests.js requestsFor):
  // Chrome's by document id; a record without one (Firefox) only when the
  // origin it came from at request time is that of the document the scan
  // found in its frame — the frame detection runs in has the address the
  // browser gave (tabs.get, or the in-page sender), the others their own
  // documents' (SEC46 低-1).
  const frameOrigins = new Map();
  for (const f of first.frames) {
    const o = parseHttpUrl(f.frameId === (frameId || 0) ? pageUrl : f.url)?.origin;
    if (o) frameOrigins.set(f.frameId, o);
  }
  // Where each scanned frame sits, as it reported (location.ancestorOrigins;
  // the screening below drops the field): the hosts, for which cookies a
  // send may hand over (src/lib/send.js cookiePlan rule ①) only.
  const ancestorsOf = new Map(first.frames.map((f) => [f.frameId, ancestorHosts(f.ancestors)]));
  // Frames of blocked sites (an embedded course platform, a page that
  // showed one before) and the requests they made are taken out here
  // (src/lib/blockedframes.js); the rest of the tab goes on without them.
  const screened = screenScan(
    {
      frames: first.frames,
      webRequests: (await requestsFor(tabId, first.frames.map((f) => f.documentId), { frameOrigins })).filter((w) => !frameId || w.frameId === frameId),
    },
    { keepDoc: true },
  );
  const selfBlocked = screened.frameCategory(frameId || 0);
  if (selfBlocked) return unsupported(key, tabId, frameId, selfBlocked);
  // What detection works on and the report keeps, held to detect.js LIMITS
  // (frames, media elements, resource entries, address length and
  // characters; SEC28 低-1, SEC28b 中-4). Within them it is the scan as is;
  // the frame detection runs in always stays (SEC33 低-1).
  const bounded = boundScan({ frames: screened.frames, webRequests: screened.webRequests }, scanLimits, { frameId: frameId || 0 });
  // which document of a frame's scan holds an entry (`doc`) served
  // boundScan's turns only (SEC36 低-7): it goes now, as screenScan would
  // have taken it out
  const noDoc = (x) => (x && typeof x === 'object' && 'doc' in x ? (({ doc: _d, ...rest }) => rest)(x) : x);
  bounded.frames = bounded.frames.map((f) => {
    const g = { ...f };
    if (Array.isArray(f.elements)) g.elements = f.elements.map(noDoc);
    if (Array.isArray(f.resources)) g.resources = f.resources.map(noDoc);
    return g;
  });
  const frameUrls = bounded.frames.filter((f) => f.frameId !== 0).map((f) => f.url);
  // Embedded frames the scan could not read (another origin, not granted):
  // their addresses only, to offer the right site access in the popup.
  const opaque = [...new Set(bounded.frames.flatMap((f) => f.opaque || []))].slice(0, 20);
  // The profile matching the page or, failing that, an embedded frame;
  // what the result keeps of it is decided after detection (`match` below).
  const found = findProfile(profiles, pageUrl, frameUrls);
  let frames = bounded.frames.map(({ opaque: _o, ...f }) => f);
  // Second pass for the profile's selectors, only when it has any.
  const reads = found ? selectorReads(found.profile) : [];
  if (reads.length) {
    const second = await scan(tabId, reads, only);
    const byId = new Map(second.frames.map((f) => [f.frameId, f.reads]));
    frames = frames.map((f) => ({ ...f, reads: byId.get(f.frameId) || [] }));
  }
  // the selector reads' nodes, held to LIMITS.attrLength / attrNames (SEC33 低-3)
  let longValues = 0;
  frames = frames.map((f) => {
    const b = boundReads(f.reads, scanLimits);
    longValues += b.longValues;
    return b.reads === f.reads ? f : { ...f, reads: b.reads };
  });
  const webRequests = bounded.webRequests;
  // The host of each scanned frame, for which cookies a send may hand over
  // (src/lib/send.js cookiePlan): the frame detection runs in has the one
  // the browser gave (tabs.get, or the in-page sender), the others their
  // own documents'.
  const frameHosts = {};
  for (const f of frames) {
    const h = f.frameId === (frameId || 0) ? page.hostname : parseHttpUrl(f.url)?.hostname;
    if (h) frameHosts[f.frameId] = h.toLowerCase();
  }
  // The ancestors' hosts of the frames that reported them (Chrome, Firefox
  // 153+; not the top frame): a frame of the page's site inside another
  // site's frame is not on the page's site for rule ① (SEC48 低-2).
  const frameAncestors = {};
  for (const f of frames) {
    const list = ancestorsOf.get(f.frameId);
    if (list) frameAncestors[f.frameId] = list;
  }
  // Which of those hosts the user granted (sites.js isAuthorized, each
  // origin asked once): only a page or frame on one of them lets a cookie
  // Domain prove the track host on its site (src/lib/send.js cookieAllowed).
  const grantedHosts = new Set();
  const askedOrigins = new Map();
  for (const f of frames) {
    const u = parseHttpUrl(f.frameId === (frameId || 0) ? pageUrl : f.url);
    if (!u) continue;
    if (!askedOrigins.has(u.origin)) askedOrigins.set(u.origin, await isAuthorized(u.href));
    if (askedOrigins.get(u.origin)) grantedHosts.add(u.hostname.toLowerCase());
  }
  // Chrome names each scanned document; the title is read from that same document.
  const documentOf = new Map(frames.map((f) => [f.frameId, f.documentId || null]));
  // the scan's mark of each document (scanFrame), for pageFetch's check only
  const tokenOf = new Map(frames.map((f) => [f.frameId, typeof f.token === 'string' ? f.token : '']));
  frames = frames.map(({ documentId, token, ...f }) => f);
  // Generic detection names the lecture with the page's own title, read
  // only now: after the blocklist checks above, in the frame detection
  // runs in, and not at all when a profile reads its own title.
  const titleOfPage = () => readTitle(tabId, frameId || 0, frames, frameId ? '' : pageUrl, documentOf.get(frameId || 0));
  let pageTitle = found ? '' : await titleOfPage();
  const io = makeIo(tabId, frames, { frameUrl: pageUrl, match: found, self: frameId || 0, documentOf, tokenOf, still: frameId ? null : () => stillOn(tabId, pageUrl) });
  const detected = await detectTracks({ frames, webRequests }, io);
  // the tab, or the frame detection runs in, went on to another page meanwhile (SEC33 信-1): nothing of it is kept
  if (io.moved) return { status: 'navigated', tabId };
  // Media served from a do-not-handle host (an embedded YouTube player) is
  // not offered; nor are tracks only embedded frames that went on to
  // another page gave (SEC36 低-9).
  const fromGone = (t) => io.gone.size > 0 && Array.isArray(t.frameIds) && t.frameIds.length > 0 && t.frameIds.every((id) => io.gone.has(id));
  let detection = { ...detected, tracks: detected.tracks.filter((t) => !isBlockedUrl(t.url) && !fromGone(t)) };
  const locale = uiLocale();
  // A profile that matched through an embedded frame (found.url is that
  // frame's address, not the page's) names one lecture of that frame: its
  // identity (platform, session id), title, duration, restriction and
  // tracks all come from that frame's own document (AUD3 低-1) — the tracks
  // it referenced or requested, the responses its page observed, its
  // selector reads, its media elements. The embedding page's own tracks are
  // not part of that task; with no track of the frame at all, the page is
  // detected as generic (its own address, its own title). `own`: the
  // matching frame(s), null when the profile matched the page itself.
  const own = found && found.url !== pageUrl ? new Set(frames.filter((f) => f.url === found.url).map((f) => f.frameId)) : null;
  const ofOwn = (t) => Array.isArray(t.frameIds) && t.frameIds.some((id) => own.has(id));
  const profileFrames = own ? frames.filter((f) => own.has(f.frameId)) : frames;
  let match = found;
  let observed = match ? combineObserved(match.profile, profileFrames) : null;
  let named0 = match ? applyProfile(match.profile, own ? detection.tracks.filter(ofOwn) : detection.tracks, locale, observed, { frameIds: own ? [...own].sort((a, b) => a - b) : [0] }) : null;
  if (own && named0.tracks.length + named0.others.length === 0) {
    match = null;
    observed = null;
    named0 = null;
    pageTitle = await titleOfPage();
  } else if (own) {
    detection = { ...detection, tracks: detection.tracks.filter(ofOwn) };
  }
  const presets = match ? match.profile.presets || null : null;
  // The counts (header, badge) are of every track found, kept or not.
  const listed0 = named0 ? [...named0.tracks, ...named0.others] : detection.tracks;
  const summary = summarize(listed0);
  const sendableCount = listed0.filter(sendable).length;
  // At most RETAIN_LIMITS.tracksKept tracks stay (SEC28 低-1): the ones a
  // card shows first, so these are the same as with all of them.
  const cut = shownTracks(listed0, presets, retainLimits.tracksKept);
  // What is kept of the detection besides its tracks (SEC33 低-3): not
  // `ignored` (the non-media addresses; nothing reads them after
  // detection), and of the segment runs only their counts (all the report
  // takes; their templates are read by nothing else); of the evidence only
  // its four flags (all the report takes; the MediaKeys addresses and the
  // durations were used by detection, and above, already: SEC33 低-5).
  const { ignored: _ignored, ...rest } = detection;
  const ev = detection.evidence || {};
  const keptDetection = {
    ...rest,
    evidence: { mse: !!ev.mse, mediaKeys: !!ev.mediaKeys, mediaKeysOnStream: !!ev.mediaKeysOnStream, encrypted: !!ev.encrypted },
    segmentRuns: (detection.segmentRuns || []).map((s) => ({ count: s.count })),
  };
  let all = { detection: keptDetection, named: named0 };
  if (cut.hidden > 0) {
    const ids = new Set(cut.tracks.map((t) => t.id));
    const keep = (list) => list.filter((t) => ids.has(t.id));
    all = { detection: { ...keptDetection, tracks: keep(keptDetection.tracks) }, named: named0 && { ...named0, tracks: keep(named0.tracks), others: keep(named0.others) } };
  }
  // What was left out past a limit, for the report (absent when nothing was).
  const limits = {};
  for (const [k, n] of Object.entries({ ...bounded.left })) if (n > 0) limits[k] = n;
  for (const [k, n] of Object.entries(detected.limited || {})) if (n > 0) limits[k] = (limits[k] || 0) + n;
  if (longValues > 0) limits.longValues = (limits.longValues || 0) + longValues;
  if (cut.hidden > 0) limits.tracks = cut.hidden;
  // What stays in memory (src/lib/retain.js): the tracks kept above; the
  // manifest texts only for the report of the tab's top-level detection, at
  // most RETAIN_LIMITS.tabTextChars of them (an in-page frame's detection
  // keeps none: no report is made of it). A copy, detached from the texts
  // left out.
  const kept = retainDetection(all, frameId ? 0 : retainLimits.tabTextChars, retainLimits);
  const named = kept.named;
  // the matching frame's selector reads and media elements only, when the
  // profile matched through it (AUD3 低-1)
  const sel = readResults(match ? profileFrames : frames);
  const pageDuration = match && own ? longestDuration(profileFrames.flatMap((f) => (f.elements || []).map((e) => (e ? e.duration : NaN)).filter((d) => Number.isFinite(d) && d > 0)), 0) : longestDuration(detection.evidence.mediaDurations, 0);
  const session = await sessionIdFor(match, pageUrl, sel);
  const cfg = match ? observeConfig(match.profile) : null;
  const downloadPolicy = downloadPolicyFor(match, sel, observed);
  const result = {
    status: 'ok',
    tabId,
    at: Date.now(),
    pageUrl,
    authorized: await isAuthorized(pageUrl),
    profile: match
      ? { id: match.profile.id, version: match.profile.version, name: match.profile.name, reviewed: match.reviewed, origins: profilePermissionOrigins(match.profile) }
      : null,
    request: match ? { ...(match.profile.request || {}) } : {},
    presets,
    // Whether the profile reads a page response, and whether it was seen
    // (on an authorized site, from the page's first load after granting).
    observe: { declared: !!cfg, seen: !!(observed && observed.seen.size > 0) },
    sessionId: session.sessionId,
    sessionSource: session.source,
    lecture: lectureFor(match, sel, pageDuration, observed, match ? null : { title: pageTitle, url: pageUrl }),
    downloadPolicy,
    // the runtime regex guard stopped the profile — here, or in the page's
    // observer before it saw the response that holds the switch (SEC18f
    // 低-18): the switch could not be read
    policyStopped: !!match && (profileStopped(match.profile) || (!!observed && observed.stopped && downloadPolicy === 'unknown')),
    tracks: kept.detection.tracks,
    named,
    // of all tracks found, also the ones past tracksKept
    summary,
    // frame id → host of the frames scanned (never shown; cookiePlan)
    frameHosts,
    // frame id → hosts of the frame's ancestors, as it reported them (never
    // shown, never in a report or a message; cookiePlan)
    frameAncestors,
    // what the browser sends an embedded frame of another site (cookiePlan
    // rule ②): Chrome its unpartitioned SameSite=None cookies (third-party
    // cookies allowed by default); Firefox keys them by the top-level site
    // (Total Cookie Protection, on by default), so only the tab's partition
    // (SEC46 低-2)
    thirdPartyCookies: BUILD.target === 'firefox' ? 'partitioned' : 'shared',
    // the hosts of frameHosts the user granted (never shown; cookiePlan)
    grantedHosts: [...grantedHosts].sort(),
    // the hosts whose media a profile says need no cookie: the popup does
    // not offer them as cookie sites (accessOffer; a send reads none there)
    quietHosts: quietHostsOf(profiles),
    // tracks given up past RETAIN_LIMITS.tracksKept (counted as not shown)
    tracksDropped: cut.hidden,
    // For the report only; never leaves the background except redacted.
    // keptChars: the manifest text it holds; evicted: given up for other
    // tabs (capTexts), so a report detects again. limits: what was left
    // out past a limit, or null. An in-page frame's detection keeps no
    // scan: no report is made of it (SEC36 低-10).
    raw: { frames: frameId ? [] : frames, webRequests: frameId ? [] : webRequests, detection: kept.detection, match, observed, keptChars: kept.keptChars, evicted: false, limits: Object.keys(limits).length ? limits : null },
    opaque,
    // what was left out because it belongs to a blocked site (the popup says so)
    blockedEmbeds: screened.categories,
  };
  // The tab may have moved on while detection ran: what it found belongs to
  // the page before, and is neither stored nor counted on the badge.
  if (!frameId && !(await stillOn(tabId, pageUrl))) return { status: 'navigated', tabId };
  // The badge counts what can be sent (SPEC-ext 用户流程 1): DRM-protected
  // tracks do not, and a page with protected tracks only shows no number.
  if (!frameId) await setBadge(tabId, sendableCount);
  return store(key, result);
}

/**
 * Whether the tab still shows the page a detection started on (SEC15 I1).
 * Addresses compare without their fragment: a single-page site that only
 * changes the fragment is still on the same page. A tab that is gone is
 * not.
 */
async function stillOn(tabId, startUrl) {
  let now;
  try {
    now = (await browser.tabs.get(tabId)).url || '';
  } catch {
    return false;
  }
  return (urlKey(now) || now) === (urlKey(startUrl) || startUrl);
}

/**
 * The title of the frame detection runs in (document.title), for generic
 * detection only; '' when the frame is gone, now shows another document,
 * or is (or sits inside) a do-not-handle site. The scan's frames are
 * already screened; this reads the title only from a frame that survived
 * the screening: frameTitle gets the screened address and reads nothing
 * unless the frame still shows it (SEC15 L3); in Chrome the injection is
 * pinned to the scanned document (documentIds; Firefox has no document
 * ids, the address check is all there is); the background checks again
 * (src/lib/blockedframes.js titleOfFrame).
 */
async function readTitle(tabId, frameId, frames, topUrl, documentId) {
  const scanned = frames.find((f) => f.frameId === frameId);
  if (!scanned) return '';
  try {
    const target = documentId ? { tabId, documentIds: [documentId] } : { tabId, frameIds: [frameId] };
    const res = await injectInto(target, frameTitle, [scanned.url]);
    return titleOfFrame(res && res[0] ? res[0].result : null, { scannedUrl: scanned.url, topUrl });
  } catch {
    return '';
  }
}

function store(key, result) {
  results.set(key, result);
  capFrames(key);
  capTexts(key);
  capScans(key);
  return result;
}

/**
 * A tab keeps at most RETAIN_LIMITS.frameResults in-page detections: past
 * it, the oldest other than the one just stored are forgotten (its card
 * detects again when used).
 */
function capFrames(key) {
  if (typeof key !== 'string') return;
  const prefix = key.slice(0, key.indexOf(':') + 1);
  const mine = [];
  for (const [k, r] of results) if (typeof k === 'string' && k.startsWith(prefix) && k !== key) mine.push({ k, at: (r && r.at) || 0 });
  mine.sort((a, b) => a.at - b.at);
  for (let i = 0; i < mine.length + 1 - retainLimits.frameResults; i++) results.delete(mine[i].k);
}

// What the report needs of a detection, given up (src/lib/retain.js): the
// texts and the scan go together, since a report of it detects again.
function evictRaw(r) {
  r.raw = { ...r.raw, frames: [], webRequests: [], detection: dropTexts(r.raw.detection), keptChars: 0, evicted: true };
}

/**
 * All tabs together keep at most RETAIN_LIMITS.totalTextChars of manifest
 * text: past it, the oldest detections other than the one just stored give
 * their texts up (src/lib/retain.js evictTexts); their tracks stay.
 */
function capTexts(key) {
  const entries = [];
  for (const [k, r] of results) if (r && r.raw && r.raw.keptChars > 0) entries.push({ key: k, at: r.at || 0, chars: r.raw.keptChars });
  for (const k of evictTexts(entries, key, retainLimits.totalTextChars)) evictRaw(results.get(k));
}

// the address characters of a stored scan, counted once per result
const scanSizes = new WeakMap();
const scanSizeOf = (r) => {
  if (!r || !r.raw || r.raw.evicted) return 0;
  if (!scanSizes.has(r)) scanSizes.set(r, scanChars(r.raw.frames, r.raw.webRequests));
  return scanSizes.get(r);
};

/**
 * All detections together keep at most RETAIN_LIMITS.totalScanChars of
 * scan addresses for the report: past it, the oldest other than the one
 * just stored give their scans (and texts) up; their tracks stay.
 */
function capScans(key) {
  const entries = [];
  for (const [k, r] of results) {
    const chars = scanSizeOf(r);
    if (chars > 0) entries.push({ key: k, at: r.at || 0, chars });
  }
  for (const k of evictTexts(entries, key, retainLimits.totalScanChars)) evictRaw(results.get(k));
}

/** Tests only: characters of manifest text each stored detection keeps, by key. */
export function keptTexts() {
  const out = new Map();
  for (const [k, r] of results) if (r && r.raw) out.set(k, { chars: r.raw.keptChars || 0, evicted: !!r.raw.evicted });
  return out;
}

/** Tests only: address characters of the scan each stored detection keeps, by key. */
export function keptScans() {
  const out = new Map();
  for (const [k, r] of results) if (r && r.raw) out.set(k, { chars: scanSizeOf(r), evicted: !!r.raw.evicted });
  return out;
}

/** A blocked page or frame: nothing detected, no number on the badge. */
async function unsupported(key, tabId, frameId, category) {
  if (!frameId) await setBadge(tabId, 0);
  return store(key, { status: 'unsupported', tabId, category });
}

export async function setBadge(tabId, count) {
  try {
    await browser.action.setBadgeText({ tabId, text: count > 0 ? String(count) : '' });
    await browser.action.setBadgeBackgroundColor({ tabId, color: tokens.light['--accent'] });
    if (browser.action.setBadgeTextColor) await browser.action.setBadgeTextColor({ tabId, color: tokens.light['--text-on-accent'] });
  } catch {
    // the tab is gone
  }
}

/**
 * A popup-safe view of a result: no URLs, no raw inputs, no session id. Of
 * a track's address only its host name (host), which the card shows as
 * text ("在 <主机> 上") when it is not on the page's site; it is read by
 * nothing else — not by a request, not by the cookie rules
 * (src/lib/send.js cookiePlan works on the result's own tracks).
 */
export function viewOf(result) {
  if (!result || result.status !== 'ok') return { status: result ? result.status : 'none', category: (result && result.category) || '' };
  const strip = (t) => ({
    id: t.id,
    host: parseHttpUrl(t.url)?.hostname || '',
    kind: t.kind,
    format: t.format,
    quality: t.quality || '',
    sizeBytes: t.sizeBytes,
    sizeEstimated: !!t.sizeEstimated,
    sources: t.sources,
    protected: t.protected,
    name: t.name || '',
    language: t.language || '',
    unreadable: !!t.unreadable,
    role: t.role || '',
    label: t.label || '',
    hint: t.hint || '',
    recommended: !!t.recommended,
  });
  const shown = shownTracks(listedTracks(result), result.presets || null);
  return {
    status: 'ok',
    authorized: result.authorized,
    profile: result.profile,
    host: parseHttpUrl(result.pageUrl)?.hostname || '',
    lecture: result.lecture,
    downloadPolicy: result.downloadPolicy,
    policyStopped: !!result.policyStopped,
    observe: result.observe,
    presets: result.presets || null,
    summary: result.summary,
    // at most RETAIN_LIMITS.tracksShown; the card says how many it leaves
    // out (also the ones the background gave up, tracksDropped)
    tracks: shown.tracks.map(strip),
    hiddenTracks: shown.hidden + (result.tracksDropped || 0),
    platform: result.profile ? result.profile.id : GENERIC,
    blockedEmbeds: Array.isArray(result.blockedEmbeds) ? [...result.blockedEmbeds] : [],
  };
}

/**
 * The origin "在此网站自动探测" asks for: the page's own, nothing more
 * (security review L4; a plain-HTTP page cannot be authorized).
 */
export function suggestedOrigins(result) {
  const { page } = accessOffer(result);
  return page ? [page] : [];
}

/**
 * Hosts whose cookies a send from this page may hand over (src/lib/send.js
 * accessOffer: the shown tracks' hosts other than the page's that cookiePlan
 * would read, or that may share a cookie with a scanned frame that showed
 * them; none when the profile reads no cookies, nor a host whose media a
 * profile says need none): listed apart, for the user to tick one by one.
 * Listing reads no cookie.
 */
export function cookieHostOrigins(result) {
  return accessOffer(result).cookieHosts;
}

/** Tabs with a detection the options page can export a report of (no addresses, host only). */
export function reportableTabs() {
  const out = [];
  for (const [tabId, r] of results) {
    if (typeof tabId !== 'number' || !r || r.status !== 'ok' || !r.raw) continue;
    out.push({ tabId, host: parseHttpUrl(r.pageUrl)?.hostname || '', total: r.summary ? r.summary.total : 0, profile: r.profile ? r.profile.name : '', at: r.at });
  }
  return out.sort((a, b) => b.at - a.at);
}

/** Builds the redacted report for a tab, re-detecting if needed. */
export async function reportFor(tabId) {
  let result = results.get(tabId);
  // no texts kept any more (given up for other tabs): detect again for a whole report
  if (!result || result.status !== 'ok' || !result.raw || result.raw.evicted) result = await detectTab(tabId, { force: true });
  if (!result || result.status !== 'ok') return { ok: false, reason: result ? result.status : 'none' };
  const { report, findings } = buildReport({
    extension: { version: extensionVersion(), browser: browserName() },
    pageUrl: result.pageUrl,
    frames: result.raw.frames,
    webRequests: result.raw.webRequests,
    detection: result.raw.detection,
    match: result.raw.match,
    observed: result.raw.observed,
    named: result.named,
    session: { source: result.sessionSource },
    lecture: result.lecture,
    downloadPolicy: result.downloadPolicy,
    limits: result.raw.limits || null,
    now: new Date(),
  });
  if (findings.length) return { ok: false, reason: 'personal_data', findings };
  return { ok: true, report, fileName: reportFileName(report) };
}

export { BUILD };
