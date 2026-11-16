// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The extension's own fallback requests during detection
// (src/background/detector.js): when the page could not fetch a playlist
// or tell a media file's size itself (src/content/scan.js pageFetch), the
// background asks the server, only on an origin the user granted.
//
// Security review FB1 M-2 and L-4 (T-SEC-02). The URL comes from the page
// (<video src>, <source>, an HLS master's variants), so:
//   - the request carries the user's cookies only where detection itself
//     is: the origin of the frame detection runs in, as the browser named
//     it (the tab's address, or the sender's frame URL; never what the page
//     says about itself); a host the matching platform profile declares
//     (match.hosts, match.mediaHosts; HTTPS only), but only when the
//     profile matched that same frame; and, when the profile matched an
//     embedded frame instead (a platform player inside another site's
//     page), only a URL on that frame's origin that the frame itself
//     referenced (SEC15 L1: a page that embeds a tenant's player cannot
//     point the extension at other paths of the tenant). Anywhere else it
//     goes without cookies (credentials: 'omit'): a signed CDN URL still
//     answers, and a page cannot make the extension send a signed-in
//     request to another site the user granted (blind cross-site GET /
//     HEAD).
//   - the request with cookies does not follow redirects (a same-origin
//     URL that redirects elsewhere would take the cookies of wherever it
//     points); on a redirect the same URL is asked again without cookies,
//     and that request follows it. So one URL takes two requests at most.
//   - a playlist is read as a stream, at most maxBytes, and one timeout
//     covers the whole request, the body included; past either the
//     request is aborted and counts as failed.
// Pure but for the fetch and the permission check it is given.

import { parseHttpUrl, hostMatches } from './url.js';

export const MAX_MANIFEST_BYTES = 2 * 1024 * 1024;
export const FETCH_TIMEOUT_MS = 10000;

const tooLarge = () => Object.assign(new Error('too large'), { name: 'TooLargeError' });
const aborted = () => Object.assign(new Error('aborted'), { name: 'AbortError' });

/**
 * The credentials mode of a fallback request for url:
 *   frameUrl  the address of the frame detection runs in, from the browser;
 *   profile   the matching profile when it matched that frame itself, else null;
 *   embedded  when the profile matched embedded frames instead:
 *             [{ frameId, origin }] of those frames;
 *   frameIds  the frames that referenced url (scan and webRequest: ids the
 *             browser gave).
 * 'include' for a URL on frameUrl's origin; over HTTPS on a host of
 * profile.match.hosts / mediaHosts; or on the origin of an embedded
 * matched frame that is among frameIds. 'omit' for any other http(s) URL;
 * '' for anything else (no request).
 */
export function fallbackCredentials(url, { frameUrl = '', profile = null, embedded = [], frameIds = [] } = {}) {
  const u = parseHttpUrl(url);
  if (!u) return '';
  const frame = parseHttpUrl(frameUrl);
  if (frame && frame.origin === u.origin) return 'include';
  const m = profile && typeof profile === 'object' ? profile.match : null;
  if (m && u.protocol === 'https:') {
    const hosts = [...(Array.isArray(m.hosts) ? m.hosts : []), ...(Array.isArray(m.mediaHosts) ? m.mediaHosts : [])];
    if (hosts.some((h) => typeof h === 'string' && h !== '*' && hostMatches(h, u.hostname))) return 'include';
  }
  const refs = new Set(Array.isArray(frameIds) ? frameIds : []);
  for (const e of Array.isArray(embedded) ? embedded : []) {
    if (e && refs.has(e.frameId) && e.origin === u.origin) return 'include';
  }
  return 'omit';
}

/**
 * Reads a response body as UTF-8 text, at most maxBytes: rejects with
 * TooLargeError past that (a Content-Length above it is refused before
 * reading) and with AbortError when signal aborts, mid-body too. The body
 * is cancelled whenever reading stops early.
 */
export async function readTextCapped(res, maxBytes, signal = null) {
  const len = Number(res.headers.get('content-length'));
  if (Number.isFinite(len) && len > maxBytes) {
    if (res.body) res.body.cancel().catch(() => {});
    throw tooLarge();
  }
  if (!res.body || typeof res.body.getReader !== 'function') {
    const text = await res.text();
    if (new TextEncoder().encode(text).length > maxBytes) throw tooLarge();
    return text;
  }
  const reader = res.body.getReader();
  const stop = new Promise((_, reject) => {
    if (!signal) return;
    if (signal.aborted) reject(aborted());
    else signal.addEventListener('abort', () => reject(aborted()), { once: true });
  });
  stop.catch(() => {});
  const dec = new TextDecoder();
  let total = 0;
  let text = '';
  try {
    for (;;) {
      const { done, value } = await Promise.race([reader.read(), stop]);
      if (done) return text + dec.decode();
      total += value.byteLength;
      if (total > maxBytes) throw tooLarge();
      text += dec.decode(value, { stream: true });
    }
  } catch (e) {
    reader.cancel().catch(() => {});
    throw e;
  }
}

/**
 * The background's fallback fetcher for one detection.
 *   fetch(url, init): the browser's fetch;
 *   permitted(url) → Promise<boolean>: whether the user granted the URL's origin;
 *   frameUrl: the address of the frame detection runs in, as the browser gave it;
 *   profile: the matching platform profile when it matched that frame, or null;
 *   embedded: [{ frameId, origin }] of the frames the profile matched
 *     instead, when it matched embedded frames (see fallbackCredentials).
 * text(url, { frameIds }) → { ok, status, contentType, text, error? } and
 * probe(url, { frameIds }) → { ok, status, contentType, size, error? }
 * (HEAD), or null when no request may be made (not http(s), origin not
 * granted). frameIds: the frames that referenced the URL, from detection
 * (ids the browser gave); nothing else from the caller changes the mode.
 */
export function backgroundFetcher({ fetch: doFetch, permitted, frameUrl = '', profile = null, embedded = [], maxBytes = MAX_MANIFEST_BYTES, timeoutMs = FETCH_TIMEOUT_MS }) {
  const once = async (url, init, credentials, read) => {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeoutMs);
    try {
      const res = await doFetch(url, { ...init, credentials, redirect: credentials === 'include' ? 'manual' : 'follow', signal: ctl.signal });
      // With cookies, a redirect is not followed: ask again without them.
      if (credentials === 'include' && (res.type === 'opaqueredirect' || (res.status >= 300 && res.status < 400))) return null;
      return await read(res, ctl.signal);
    } finally {
      clearTimeout(timer);
      ctl.abort(); // whatever was not read is dropped
    }
  };
  const request = async (url, frameIds, init, read) => {
    const credentials = fallbackCredentials(url, { frameUrl, profile, embedded, frameIds });
    if (!credentials || !(await permitted(url))) return null;
    if (credentials === 'include') {
      const r = await once(url, init, 'include', read);
      if (r) return r;
    }
    return once(url, init, 'omit', read);
  };
  const failed = (e, extra) => ({ ok: false, status: 0, contentType: '', ...extra, error: String((e && e.name) || 'error') });
  return {
    async text(url, { frameIds = [] } = {}) {
      try {
        return await request(url, frameIds, { method: 'GET' }, async (res, signal) => {
          const base = { status: res.status, contentType: res.headers.get('content-type') || '' };
          if (!res.ok) return { ok: false, ...base, text: '' };
          try {
            return { ok: true, ...base, text: await readTextCapped(res, maxBytes, signal) };
          } catch (e) {
            if (e && e.name === 'TooLargeError') return { ok: false, ...base, text: '', error: 'too large' };
            throw e;
          }
        });
      } catch (e) {
        return failed(e, { text: '' });
      }
    },
    async probe(url, { frameIds = [] } = {}) {
      try {
        return await request(url, frameIds, { method: 'HEAD' }, async (res) => {
          const len = Number(res.headers.get('content-length'));
          return { ok: res.ok, status: res.status, contentType: res.headers.get('content-type') || '', size: Number.isFinite(len) && len > 0 ? len : -1 };
        });
      } catch (e) {
        return failed(e, { size: -1 });
      }
    },
  };
}
