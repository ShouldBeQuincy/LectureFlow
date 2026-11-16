// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Frames and requests of blocked sites (src/lib/blocklist.js) taken out of
// a tab's scan before detection. Course platforms are often embedded in
// another page (Coursera for Campus or LinkedIn Learning as an LTI tool in
// Canvas or Moodle) and keep their media on general CDNs, so the media
// host alone says nothing: what a blocked frame shows or requests is
// dropped whichever host serves it.
//
// A frame is blocked when
//   - its address is (host or path entry);
//   - its document showed a blocked address earlier: the address it was
//     loaded with, and the same-document history entries (a single-page
//     site that moved from /learning to its feed still has /learning's
//     media in its resource timing);
//   - it sits inside a blocked frame or page: an origin of its
//     location.ancestorOrigins is a host blocked as a whole, or the origin
//     of a blocked frame of this tab (path entries: an origin has no path).
// Inside a frame's own scan, a same-origin child document (scanned with
// its parent, src/content/scan.js) that is blocked, or below one, drops
// out the same way. The webRequest route drops the requests of blocked
// frames, and those whose initiator (the origin of the document that made
// them: Chrome's initiator, Firefox's documentUrl) is a blocked origin;
// src/background/requests.js already skipped those a browser tied to a
// blocked document or ancestor, and those of documents the scan did not
// read.
//
// The extra fields of the scan (docs, ancestors, doc, initiator) are
// removed here (doc, on request, right after boundScan): they never reach
// detection, the report or a fixture. Pure.

import { blockedCategory, originCategory } from './blocklist.js';
import { urlKey } from './url.js';

const MAX_HISTORY = 50;

function originOf(raw) {
  try {
    const u = new URL(String(raw));
    return u.protocol === 'http:' || u.protocol === 'https:' ? u.origin.toLowerCase() : '';
  } catch {
    return '';
  }
}

/** The first blocked address among a document's own and its history, as a category, or ''. */
export function documentCategory(doc) {
  if (!doc) return '';
  const own = blockedCategory(doc.url);
  if (own) return own;
  for (const h of (Array.isArray(doc.history) ? doc.history : []).slice(0, MAX_HISTORY)) {
    const c = blockedCategory(h);
    if (c) return c;
  }
  return '';
}

/**
 * The category of a frame that is, or sits inside, a blocked page or frame
 * (the content scripts' check: in-page buttons, page-response
 * observation): its own address, the tab's top-level address (when the
 * browser gives it), the origins of location.ancestorOrigins. ''.
 */
export function contextCategory({ frameUrl = '', topUrl = '', ancestors = null } = {}) {
  const own = blockedCategory(frameUrl) || blockedCategory(topUrl);
  if (own) return own;
  for (const a of Array.isArray(ancestors) ? ancestors.slice(0, 64) : []) {
    const c = originCategory(a);
    if (c) return c;
  }
  return '';
}

/**
 * Screens a scan. frames: the scan's frames ({ frameId, url, docs?,
 * ancestors?, elements, resources, opaque?, … }); webRequests: the
 * webRequest route's entries ({ url, frameId, initiator?, … }).
 * Returns { frames, webRequests, blockedFrameIds, categories, frameCategory }:
 * the frames and requests left, without the screening fields; the ids of
 * the frames taken out; the categories of what was taken out; and
 * frameCategory(frameId), the category of a frame that was taken out ('').
 * keepDoc: entries keep their `doc` (which document of the frame's scan
 * holds them), for boundScan's turns only; the caller removes it right
 * after (src/background/detector.js).
 */
export function screenScan({ frames = [], webRequests = [] } = {}, { keepDoc = false } = {}) {
  const blockedOrigins = new Map(); // origin → category
  const addOrigin = (raw, category) => {
    const o = originOf(raw);
    if (o && !blockedOrigins.has(o)) blockedOrigins.set(o, category);
  };
  const frameCat = new Map(); // frameId → category
  const categories = new Set();
  const block = (f, category) => {
    if (frameCat.has(f.frameId)) return false;
    frameCat.set(f.frameId, category);
    categories.add(category);
    addOrigin(f.url, category);
    return true;
  };

  // Blocked frames and documents by their own addresses and histories.
  const blockedDocs = new Map(); // frame → Set of doc indices
  for (const f of frames) {
    const docs = Array.isArray(f.docs) && f.docs.length ? f.docs : [{ url: f.url, parent: -1, history: [] }];
    const own = blockedCategory(f.url) || documentCategory(docs[0]);
    if (own) block(f, own);
    const out = new Set();
    docs.forEach((d, i) => {
      if (i === 0) return;
      const c = documentCategory(d);
      const parent = Number.isInteger(d.parent) ? d.parent : 0;
      if (c || out.has(parent)) {
        out.add(i);
        if (c) {
          categories.add(c);
          addOrigin(d.url, c);
        }
      }
    });
    blockedDocs.set(f, out);
    // frames of other origins this one embeds: their address only
    for (const src of Array.isArray(f.opaque) ? f.opaque : []) {
      const c = blockedCategory(src);
      if (c) {
        categories.add(c);
        addOrigin(src, c);
      }
    }
  }
  // Frames inside a blocked page or frame (repeat: a blocked frame adds its origin).
  for (let changed = true; changed; ) {
    changed = false;
    for (const f of frames) {
      if (frameCat.has(f.frameId)) continue;
      for (const a of Array.isArray(f.ancestors) ? f.ancestors : []) {
        const o = originOf(a);
        const c = originCategory(a) || (o && blockedOrigins.get(o)) || '';
        if (c) {
          changed = block(f, c) || changed;
          break;
        }
      }
    }
  }

  const strip = keepDoc ? (x) => x : ({ doc: _d, ...rest }) => rest;
  const kept = [];
  for (const f of frames) {
    if (frameCat.has(f.frameId)) continue;
    const out = blockedDocs.get(f) || new Set();
    const keep = (x) => !out.has(Number.isInteger(x.doc) ? x.doc : 0);
    const { docs: _docs, ancestors: _anc, ...rest } = f;
    const g = { ...rest };
    // and what points at a blocked site itself (an embedded platform's own
    // page load in the parent's resource timing, a YouTube media URL)
    if (Array.isArray(f.elements)) g.elements = f.elements.filter((e) => keep(e) && !blockedCategory(e.src) && !blockedCategory(e.currentSrc)).map(strip);
    if (Array.isArray(f.resources)) g.resources = f.resources.filter((r) => keep(r) && !blockedCategory(r.url)).map(strip);
    if (Array.isArray(f.opaque)) g.opaque = f.opaque.filter((src) => !blockedCategory(src));
    kept.push(g);
  }
  const requests = [];
  for (const w of webRequests) {
    if (frameCat.has(w.frameId)) continue;
    if (w.initiator) {
      const o = originOf(w.initiator);
      if (originCategory(w.initiator) || (o && blockedOrigins.has(o))) continue;
    }
    if (blockedCategory(w.url)) continue;
    const { initiator: _i, ...rest } = w;
    requests.push(rest);
  }
  return {
    frames: kept,
    webRequests: requests,
    blockedFrameIds: new Set(frameCat.keys()),
    categories: [...categories],
    frameCategory: (frameId) => frameCat.get(frameId) || '',
  };
}

/**
 * The title a frame gave (src/content/scan.js frameTitle: { url, title,
 * history, ancestors }) when it may be used: the frame still shows the
 * document the screened scan saw (scannedUrl, compared without the
 * fragment), and neither its address, the addresses its document showed,
 * the tab's top-level address (topUrl, when known) nor an origin it sits
 * inside is a do-not-handle site. '' otherwise: a blocked site's title is
 * never used.
 */
export function titleOfFrame(read, { scannedUrl = '', topUrl = '' } = {}) {
  if (!read || typeof read !== 'object' || typeof read.title !== 'string' || typeof read.url !== 'string') return '';
  const now = urlKey(read.url);
  if (!now || now !== urlKey(scannedUrl)) return '';
  if (documentCategory({ url: read.url, history: read.history })) return '';
  if (contextCategory({ frameUrl: read.url, topUrl, ancestors: Array.isArray(read.ancestors) ? read.ancestors : null })) return '';
  return read.title;
}
