// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Turning the three detection routes into candidates and merging them by
// URL (SPEC-ext 通用轨道探测: 结果按 URL 去重后合并). Pure.

import { urlKey, parseHttpUrl } from './url.js';
import { classifyUrl, C_FILE, C_SEGMENT } from './classify.js';

export const SRC_ELEMENT = 'element';
export const SRC_PERFORMANCE = 'performance';
export const SRC_WEBREQUEST = 'webrequest';
export const SOURCE_ORDER = [SRC_ELEMENT, SRC_PERFORMANCE, SRC_WEBREQUEST];

const MAX_RESOURCES_PER_FRAME = 2000;

// The longest address a candidate may have, without its fragment (it is
// keyed so): LIMITS.urlLength in detect.js, where the reasons are.
export const MAX_URL_LENGTH = 8192;

// The address of u as candidates key it (http(s), no fragment) fits within
// urlLength; false for one too long. u is a parsed URL.
const keyFits = (u, urlLength) => u.href.length - u.hash.length <= urlLength;

/**
 * Raw scan results → candidates.
 *
 * frames: [{ frameId, url, elements: [{ tag, src, currentSrc, sources:
 *   [{ src, type }], mediaKeys, videoWidth, videoHeight, duration,
 *   readyState }], resources: [{ url, initiatorType, responseStatus }],
 *   encryptedEvents }]
 * webRequests: [{ url, frameId, resourceType, statusCode, contentType,
 *   contentLength, contentRangeTotal }]
 *
 * Returns { candidates: [{ url, source, frameId, hints }], evidence,
 * longUrls } where evidence records what the page says about MSE and EME,
 * and longUrls counts the addresses left out because, without their
 * fragment, they are longer than opts.urlLength (SEC28b 中-4: every table
 * keyed by address stays linear, see LIMITS.urlLength).
 */
export function buildCandidates({ frames = [], webRequests = [] }, { urlLength = MAX_URL_LENGTH } = {}) {
  const candidates = [];
  const evidence = { mse: false, mediaKeys: false, mediaKeysOnStream: false, encrypted: false, mediaKeysUrls: [], mediaDurations: [] };
  let longUrls = 0;
  // an http(s) address that fits; counts one that does not
  const usable = (raw) => {
    const u = parseHttpUrl(raw);
    if (!u) return false;
    if (keyFits(u, urlLength)) return true;
    longUrls++;
    return false;
  };

  for (const f of frames) {
    // An "encrypted" event (seen by the observer on authorized sites) means
    // the stream the page plays is encrypted, whichever manifest feeds it.
    if (f.encryptedEvents > 0) {
      evidence.encrypted = true;
      evidence.mediaKeysOnStream = true;
    }
    for (const el of f.elements || []) {
      const urls = [];
      const tried = []; // each address of the element once (src and currentSrc are often the same)
      const push = (u, type) => {
        if (!u) return;
        if (/^blob:/i.test(u)) {
          evidence.mse = true;
          if (el.mediaKeys) evidence.mediaKeysOnStream = true;
          return;
        }
        if (tried.includes(u)) return;
        tried.push(u);
        if (usable(u)) urls.push({ u, type });
      };
      push(el.currentSrc);
      push(el.src);
      for (const s of el.sources || []) push(s.src, s.type);
      if (el.mediaKeys) evidence.mediaKeys = true;
      if (Number.isFinite(el.duration) && el.duration > 0) evidence.mediaDurations.push(el.duration);
      for (const { u, type } of urls) {
        if (el.mediaKeys) evidence.mediaKeysUrls.push(urlKey(u));
        candidates.push({
          url: u,
          source: SRC_ELEMENT,
          frameId: f.frameId,
          hints: {
            tag: el.tag,
            sourceType: type || '',
            isCurrent: el.currentSrc === u,
            mediaKeys: !!el.mediaKeys,
            videoWidth: el.videoWidth ?? null,
            videoHeight: el.videoHeight ?? null,
            readyState: el.readyState ?? null,
            duration: Number.isFinite(el.duration) ? el.duration : null,
          },
        });
      }
    }
    for (const r of (f.resources || []).slice(0, MAX_RESOURCES_PER_FRAME)) {
      if (!usable(r.url)) continue;
      candidates.push({
        url: r.url,
        source: SRC_PERFORMANCE,
        frameId: f.frameId,
        hints: {
          initiatorType: r.initiatorType || '',
          status: typeof r.responseStatus === 'number' && r.responseStatus > 0 ? r.responseStatus : undefined,
        },
      });
    }
  }

  for (const w of webRequests) {
    if (!usable(w.url)) continue;
    candidates.push({
      url: w.url,
      source: SRC_WEBREQUEST,
      frameId: w.frameId ?? 0,
      hints: {
        resourceType: w.resourceType || '',
        status: w.statusCode,
        contentType: w.contentType || '',
        size: sizeFromHeaders(w),
      },
    });
  }
  evidence.mediaKeysUrls = [...new Set(evidence.mediaKeysUrls)].sort();
  return { candidates, evidence, longUrls };
}

/**
 * The longest of the page's media durations (evidence.mediaDurations), or
 * `none` when there is none. A loop, not Math.max(...list): spreading one
 * argument per media element throws a RangeError once a page has some
 * hundred thousand of them (SEC23b 信息-9).
 */
export function longestDuration(durations, none = null) {
  let best = -Infinity;
  for (const d of durations || []) if (d > best) best = d;
  return best === -Infinity ? none : best;
}

/** Total size from a response's headers: Content-Range total for 206. */
export function sizeFromHeaders({ statusCode, contentLength, contentRangeTotal }) {
  if (statusCode === 206) return Number.isFinite(contentRangeTotal) && contentRangeTotal > 0 ? contentRangeTotal : -1;
  if (Number.isFinite(contentLength) && contentLength > 0 && (statusCode === undefined || statusCode === 200)) return contentLength;
  return -1;
}

/** Parses the total of "bytes 0-1/12345"; null if absent or "*". */
export function contentRangeTotal(header) {
  const m = /\/\s*(\d+)\s*$/.exec(String(header || ''));
  return m ? Number(m[1]) : null;
}

/**
 * Merges candidates by URL (fragment dropped). Each merged entry:
 * { url, sources: [..in SOURCE_ORDER], frameIds: [..], cls, kind, hints }
 * where hints combine the routes: the server's Content-Type and size win
 * over the page's; the media element contributes tag, size in pixels and
 * EME state.
 */
export function mergeCandidates(candidates) {
  const byKey = new Map();
  for (const c of candidates) {
    const key = urlKey(c.url);
    if (!key) continue;
    let m = byKey.get(key);
    if (!m) {
      m = { url: key, sources: new Set(), frameIds: new Set(), hints: {} };
      byKey.set(key, m);
    }
    m.sources.add(c.source);
    m.frameIds.add(c.frameId ?? 0);
    const h = m.hints;
    const n = c.hints || {};
    if (n.contentType && (c.source === SRC_WEBREQUEST || !h.contentType)) h.contentType = n.contentType;
    if (n.sourceType && !h.sourceType) h.sourceType = n.sourceType;
    if (n.size > 0 && !(h.size > 0)) h.size = n.size;
    if (typeof n.status === 'number') {
      // A success from any route outweighs an error from another.
      if (h.status === undefined || (h.status >= 400 && n.status < 400)) h.status = n.status;
    }
    if (n.tag && (!h.tag || n.isCurrent)) h.tag = n.tag;
    if (n.initiatorType && !h.initiatorType) h.initiatorType = n.initiatorType;
    if (n.resourceType && !h.resourceType) h.resourceType = n.resourceType;
    if (n.mediaKeys) h.mediaKeys = true;
    for (const k of ['videoWidth', 'videoHeight', 'readyState', 'duration']) {
      if (n[k] !== null && n[k] !== undefined && (h[k] === undefined || h[k] === null || n.isCurrent)) h[k] = n[k];
    }
  }
  const out = [];
  for (const m of byKey.values()) {
    const ct = m.hints.contentType || m.hints.sourceType || '';
    const { cls, kind } = classifyUrl(m.url, { ...m.hints, contentType: ct });
    out.push({
      url: m.url,
      sources: SOURCE_ORDER.filter((s) => m.sources.has(s)),
      frameIds: [...m.frameIds].sort((a, b) => a - b),
      cls,
      kind,
      hints: m.hints,
    });
  }
  out.sort((a, b) => (a.url < b.url ? -1 : a.url > b.url ? 1 : 0));
  return out;
}

/**
 * The shape of a URL with its numbers blanked: segments of one stream
 * ("seg_00012.mp4", "?n=12") share it.
 */
export function urlTemplate(url) {
  const u = new URL(url);
  const m = /^(.*?)(\.[A-Za-z0-9]{1,6})?$/.exec(u.pathname);
  const path = m[1].replace(/\d+/g, '#') + (m[2] || ''); // the extension keeps its digits
  const q = [...new URLSearchParams(u.search).keys()].sort().join('&');
  return `${u.host}${path}?${q}`;
}

/**
 * Files that only the player's requests reveal and that come in runs of
 * three or more URLs differing only in numbers are segments of a stream,
 * not tracks. A file a media element points at is never collapsed.
 */
export function collapseSegmentRuns(merged, minRun = 3) {
  const groups = new Map();
  for (const m of merged) {
    if (m.cls !== C_FILE || m.sources.includes(SRC_ELEMENT)) continue;
    const t = urlTemplate(m.url);
    if (!groups.has(t)) groups.set(t, []);
    groups.get(t).push(m);
  }
  const runs = [];
  for (const [template, list] of groups) {
    if (list.length < minRun) continue;
    for (const m of list) m.cls = C_SEGMENT;
    runs.push({ template, count: list.length });
  }
  runs.sort((a, b) => (a.template < b.template ? -1 : 1));
  return runs;
}
