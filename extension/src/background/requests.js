// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The third detection route: a read-only webRequest listener. The browser
// only reports requests to hosts the user has granted, so this sees
// nothing on other sites. Per tab it keeps the media and manifest
// responses (URL, type, status, size) since the last navigation, in memory
// and in storage.session (memory only, survives a service-worker restart,
// gone when the browser closes).
//
// Navigation: the buffer is cleared on the main-frame request, which the
// browser reports in order with the page's own requests (tabs.onUpdated
// may arrive after them). Detection keeps only the requests of the
// documents it scanned: by document id where the browser gives one
// (Chrome), else by the origin a request came from (Firefox: documentUrl),
// which must be that of the document scanned in its frame (requestsFor).
// Requests the extension makes itself (manifest fetches and size probes)
// are not counted as the page's.
//
// Do-not-handle sites (src/lib/blocklist.js): a request the browser ties to
// a blocked document or frame (Firefox: documentUrl, originUrl,
// frameAncestors; Chrome: the initiator's origin, for a host blocked as a
// whole) is not recorded at all, whichever host serves it. The initiator
// is kept with the entry, as the browser gave it at request time, so that
// detection can tie the entry to a scanned document (requestsFor) and
// drop the requests of a frame it found blocked (src/lib/blockedframes.js
// removes it again).
//
// Size (security audit AUD3 中-2): an address longer than LIMITS.urlLength
// (the same limit detection applies, src/lib/detect.js) is not recorded at
// all, a Content-Type is kept to CONTENT_TYPE_LENGTH characters, and a
// tab's records hold at most LIMITS.requestChars address characters
// (MAX_PER_TAB entries of at most urlLength each never reach that; it
// stays as a bound in case either limit changes). A granted site that
// answers media requests with megabyte-long addresses so costs neither
// memory nor time: each response looks its URL up in a table, never
// re-reading the ones kept.

import { browser } from './env.js';
import { classifyUrl, C_HLS, C_DASH, C_FILE } from '../lib/classify.js';
import { contentRangeTotal } from '../lib/merge.js';
import { urlKey } from '../lib/url.js';
import { blockedCategory, originCategory } from '../lib/blocklist.js';
import { LIMITS } from '../lib/detect.js';

const MAX_PER_TAB = 300;
const CONTENT_TYPE_LENGTH = 256;
const PERSIST_DELAY = 500;
const BUFFER_LIMITS = Object.freeze({ perTab: MAX_PER_TAB, urlLength: LIMITS.urlLength, tabChars: LIMITS.requestChars });
let limits = BUFFER_LIMITS;

/** Tests only: other buffer limits ({ perTab, urlLength, tabChars }), or the real ones again with none. */
export function setBufferLimits(over = null) {
  limits = over ? Object.freeze({ ...BUFFER_LIMITS, ...over }) : BUFFER_LIMITS;
}

// tabId → { entries: [], index: Map(urlKey → entry index), chars: address
// characters of the entries, restored: bool }
const buffers = new Map();
const persistTimers = new Map();
const own = new Map(); // tabId → Map(urlKey → expiry): requests the extension makes
const OWN_TTL_MS = 30000;
let onCandidate = () => {};

/** Marks a URL the extension is about to fetch in a tab. */
export function expectOwn(tabId, url) {
  const k = urlKey(url);
  if (!k) return;
  if (!own.has(tabId)) own.set(tabId, new Map());
  own.get(tabId).set(k, Date.now() + OWN_TTL_MS);
}

function isOwn(tabId, k) {
  const m = own.get(tabId);
  if (!m) return false;
  const exp = m.get(k);
  if (exp === undefined) return false;
  if (exp < Date.now()) {
    m.delete(k);
    return false;
  }
  return true;
}

const key = (tabId) => `wr:${tabId}`;

function header(headers, name) {
  const h = (headers || []).find((x) => x.name.toLowerCase() === name);
  return h ? h.value || '' : '';
}

const emptyBuffer = (restored) => ({ entries: [], index: new Map(), chars: 0, restored });

function buffer(tabId) {
  let b = buffers.get(tabId);
  if (!b) {
    b = emptyBuffer(false);
    buffers.set(tabId, b);
  }
  return b;
}

/** Whether an address may be recorded at all: a string of at most urlLength characters. */
function recordable(url) {
  return typeof url === 'string' && url.length <= limits.urlLength;
}

/** Whether the tab has room for one more entry of this address. */
function hasRoom(b, url) {
  return b.entries.length < limits.perTab && b.chars + url.length <= limits.tabChars;
}

function append(b, k, entry) {
  // the entry's own string as the key when it is the same text (a request
  // URL has no fragment): the table then holds no second copy of it
  b.index.set(k === entry.url ? entry.url : k, b.entries.length);
  b.entries.push(entry);
  b.chars += entry.url.length;
}

function schedulePersist(tabId) {
  if (persistTimers.has(tabId)) return;
  persistTimers.set(
    tabId,
    setTimeout(() => {
      persistTimers.delete(tabId);
      const b = buffers.get(tabId);
      if (!b) return;
      browser.storage.session.set({ [key(tabId)]: b.entries }).catch(() => {});
    }, PERSIST_DELAY),
  );
}

/** Whether the browser says a request came from a blocked document or frame. */
export function fromBlockedSource(details) {
  if (blockedCategory(details.url)) return true;
  for (const u of [details.documentUrl, details.originUrl]) if (typeof u === 'string' && blockedCategory(u)) return true;
  if (Array.isArray(details.frameAncestors) && details.frameAncestors.some((a) => a && typeof a.url === 'string' && blockedCategory(a.url))) return true;
  return typeof details.initiator === 'string' && originCategory(details.initiator) !== '';
}

/** The origin the request came from: Chrome's initiator, Firefox's document. */
function initiatorOf(details) {
  if (typeof details.initiator === 'string') return details.initiator;
  try {
    return typeof details.documentUrl === 'string' ? new URL(details.documentUrl).origin : '';
  } catch {
    return '';
  }
}

function onHeaders(details) {
  if (details.tabId < 0 || details.method === 'HEAD') return;
  // first of all: an address past the limit costs nothing further
  if (!recordable(details.url)) return;
  if (fromBlockedSource(details)) return;
  const contentType = header(details.responseHeaders, 'content-type').slice(0, CONTENT_TYPE_LENGTH);
  const status = details.statusCode;
  const { cls } = classifyUrl(details.url, { contentType, resourceType: details.type, status });
  if (cls !== C_HLS && cls !== C_DASH && cls !== C_FILE) return;
  const b = buffer(details.tabId);
  const k = urlKey(details.url);
  if (!k) return;
  const i = b.index.has(k) ? b.index.get(k) : -1;
  if (i < 0 && isOwn(details.tabId, k)) return; // our own fetch, not the player's
  const len = Number(header(details.responseHeaders, 'content-length'));
  const entry = {
    url: details.url,
    frameId: details.frameId,
    resourceType: details.type,
    statusCode: status,
    contentType,
    contentLength: Number.isFinite(len) && len > 0 ? len : null,
    contentRangeTotal: contentRangeTotal(header(details.responseHeaders, 'content-range')),
    documentId: details.documentId || null,
    initiator: initiatorOf(details),
  };
  if (i >= 0) {
    // Keep the most informative response (a 200 with a length, or a 206
    // with a total) for a URL the player requests in ranges.
    const old = b.entries[i];
    if (!(old.contentRangeTotal || old.contentLength) || entry.contentRangeTotal || entry.contentLength) {
      b.chars += entry.url.length - old.url.length;
      b.entries[i] = entry;
    }
  } else {
    if (!hasRoom(b, entry.url)) return;
    append(b, k, entry);
  }
  schedulePersist(details.tabId);
  onCandidate(details.tabId);
}

function onMainFrame(details) {
  if (details.tabId >= 0 && details.type === 'main_frame') reset(details.tabId);
}

/** Forgets a tab's requests (navigation, close). */
export function reset(tabId) {
  buffers.set(tabId, emptyBuffer(true));
  browser.storage.session.remove(key(tabId)).catch(() => {});
}

export function forget(tabId) {
  buffers.delete(tabId);
  own.delete(tabId);
  browser.storage.session.remove(key(tabId)).catch(() => {});
}

/** The http(s) origin of an initiator, or ''. */
function originOf(raw) {
  try {
    const u = new URL(String(raw));
    return u.protocol === 'http:' || u.protocol === 'https:' ? u.origin : '';
  } catch {
    return '';
  }
}

/**
 * Whether an entry belongs to a document the scan read. documentIds
 * (Chrome names each scanned document): an entry with an id must carry one
 * of them. frameOrigins (frame id → origin of the document the scan read in
 * that frame): an entry without an id (Firefox gives none) must have come,
 * at request time, from that origin (its initiator) — a frame that first
 * showed another site's document and then moved on to a granted site does
 * not pass the earlier document's requests off as the new one's, and a
 * frame the scan did not read has none (SEC46 低-1). Without frameOrigins
 * such entries are kept (the end-to-end build's look at the records).
 */
function ofScannedDocument(e, docs, frameOrigins) {
  if (e.documentId) return docs.size === 0 || docs.has(e.documentId);
  if (!frameOrigins) return true;
  const o = originOf(e.initiator);
  return o !== '' && frameOrigins.get(e.frameId) === o;
}

/**
 * The requests seen in a tab since its last navigation, of the documents
 * the scan read (ofScannedDocument).
 */
export async function requestsFor(tabId, documentIds = [], { frameOrigins = null } = {}) {
  const b = buffer(tabId);
  if (!b.restored) {
    b.restored = true;
    try {
      const stored = (await browser.storage.session.get(key(tabId)))[key(tabId)];
      // the same limits as a live record; what was kept before a restart
      // came through them, but storage is read as it is
      for (const e of Array.isArray(stored) ? stored : []) {
        if (!e || typeof e !== 'object' || !recordable(e.url)) continue;
        const k = urlKey(e.url);
        if (!k || b.index.has(k) || !hasRoom(b, e.url)) continue;
        append(b, k, typeof e.contentType === 'string' && e.contentType.length > CONTENT_TYPE_LENGTH ? { ...e, contentType: e.contentType.slice(0, CONTENT_TYPE_LENGTH) } : e);
      }
    } catch {
      // nothing stored
    }
  }
  const docs = new Set(documentIds.filter(Boolean));
  // given but not a map: no frame has an origin (nothing without an id passes)
  const origins = frameOrigins == null ? null : frameOrigins instanceof Map ? frameOrigins : new Map();
  return b.entries.filter((e) => ofScannedDocument(e, docs, origins)).map(({ documentId, ...rest }) => rest);
}

/** Registers the listeners; call at the top level of the background. */
export function install(candidateCallback) {
  onCandidate = candidateCallback || (() => {});
  const filter = { urls: ['<all_urls>'], types: ['media', 'xmlhttprequest', 'object', 'other'] };
  browser.webRequest.onHeadersReceived.addListener(onHeaders, filter, ['responseHeaders']);
  browser.webRequest.onBeforeRequest.addListener(onMainFrame, { urls: ['<all_urls>'], types: ['main_frame'] });
}
