// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Functions the background injects into a page with
// scripting.executeScript({ func }). The browser serialises each function
// on its own, so each must be self-contained: no imports, no outside
// variables. They only read: media elements, resource timing, the nodes a
// profile's selectors hit, the frame's own title (frameTitle, generic
// detection only); and pageFetch requests the manifest URLs the
// player itself requested, with the page's own credentials. Interpretation
// happens in src/lib (pure, tested).

/**
 * Scans this frame and its same-origin child frames.
 * reads: [{ key, selector, attr }] from the profile (empty without one).
 *
 * For the do-not-handle list (src/lib/blockedframes.js, which also removes
 * these fields again before anything else sees the scan): `docs` lists the
 * documents scanned (this frame's first) with their parent's index, their
 * address, and the addresses they showed — the one they were loaded with
 * and the same-document history entries (Navigation API, where the
 * browser has it); `ancestors` is location.ancestorOrigins; every element
 * and resource names its document (`doc`).
 */
export function scanFrame(reads) {
  const MAX_RESOURCES = 2000;
  const MAX_TEXT = 1000;
  const MAX_HISTORY = 50;
  const out = { url: location.href, elements: [], resources: [], reads: [], encryptedEvents: 0, observed: [], opaque: [], docs: [], ancestors: [] };
  // A mark of this document in the extension's isolated world (the page
  // cannot see or set it; it goes with the document): pageFetch checks it
  // to tell this document from the next one in the frame (SEC36 低-9).
  try {
    if (typeof globalThis.__lectureflowScan !== 'string') {
      globalThis.__lectureflowScan = Array.from(crypto.getRandomValues(new Uint32Array(4)), (n) => n.toString(36)).join('-');
    }
    out.token = globalThis.__lectureflowScan;
  } catch {
    // no mark: the address is checked instead
  }
  const docs = [];
  const seen = new Set();

  const historyOf = (win) => {
    const list = [];
    try {
      const nav = win.performance.getEntriesByType('navigation')[0];
      if (nav && typeof nav.name === 'string') list.push(nav.name);
    } catch {
      // no navigation timing
    }
    try {
      const api = win.navigation;
      if (api && typeof api.entries === 'function') {
        for (const e of api.entries()) {
          if (list.length >= MAX_HISTORY) break;
          if (e && e.sameDocument && typeof e.url === 'string' && !list.includes(e.url)) list.push(e.url);
        }
      }
    } catch {
      // no Navigation API
    }
    return list;
  };

  const visit = (win, depth, parent) => {
    let doc;
    try {
      doc = win.document;
      if (!doc || !doc.documentElement) return;
    } catch {
      return; // cross-origin
    }
    if (seen.has(doc)) return;
    seen.add(doc);
    const index = docs.length;
    docs.push({ win, doc });
    let url = '';
    try {
      url = String(win.location.href);
    } catch {
      url = '';
    }
    out.docs.push({ url, parent, history: historyOf(win) });
    if (depth >= 4) return;
    for (const f of doc.querySelectorAll('iframe, frame')) {
      let readable = false;
      try {
        readable = !!(f.contentWindow && f.contentWindow.document);
      } catch {
        readable = false;
      }
      if (readable) visit(f.contentWindow, depth + 1, index);
      // Another origin: only its address, so the popup can offer that
      // site's access (an LMS page embedding a recording platform).
      else if (f.src && /^https?:/i.test(f.src) && out.opaque.length < 20) out.opaque.push(f.src);
    }
  };
  visit(window, 0, -1);
  try {
    const list = location.ancestorOrigins;
    if (list) out.ancestors = [...list].slice(0, 64).map(String);
  } catch {
    // none
  }

  const shadowOf = (el) => {
    if (el.shadowRoot) return el.shadowRoot;
    try {
      if (globalThis.chrome && chrome.dom && chrome.dom.openOrClosedShadowRoot) return chrome.dom.openOrClosedShadowRoot(el);
    } catch {
      // not available
    }
    try {
      if ('openOrClosedShadowRoot' in el) return el.openOrClosedShadowRoot;
    } catch {
      // not available
    }
    return null;
  };

  const mediaIn = (root, acc, depth) => {
    for (const el of root.querySelectorAll('video, audio')) acc.add(el);
    if (depth > 3) return;
    for (const host of root.querySelectorAll('*')) {
      const sr = shadowOf(host);
      if (sr) mediaIn(sr, acc, depth + 1);
    }
  };

  const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : null);

  for (const [index, { win, doc }] of docs.entries()) {
    const media = new Set();
    mediaIn(doc, media, 0);
    for (const el of media) {
      const tag = el.tagName.toLowerCase();
      out.elements.push({
        tag,
        src: el.getAttribute('src') ? el.src : '',
        currentSrc: el.currentSrc || '',
        sources: [...el.querySelectorAll('source')].map((s) => ({ src: s.src || '', type: s.type || '' })).filter((s) => s.src),
        mediaKeys: !!el.mediaKeys,
        videoWidth: tag === 'video' ? num(el.videoWidth) : null,
        videoHeight: tag === 'video' ? num(el.videoHeight) : null,
        duration: num(el.duration),
        readyState: num(el.readyState),
        doc: index,
      });
    }
    try {
      const entries = win.performance.getEntriesByType('resource');
      for (const e of entries) {
        if (out.resources.length >= MAX_RESOURCES) break;
        out.resources.push({ url: e.name, initiatorType: e.initiatorType || '', responseStatus: num(e.responseStatus), doc: index });
      }
    } catch {
      // no resource timing
    }
  }

  // What the observer content script (authorized sites only) collected
  // beyond the resource-timing buffer, and whether media was encrypted.
  const obs = globalThis.__lectureflowObserver;
  if (obs) {
    for (const r of obs.resources || []) {
      if (out.resources.length >= MAX_RESOURCES) break;
      out.resources.push({ url: r.url, initiatorType: r.initiatorType || '', responseStatus: num(r.responseStatus), doc: 0 });
    }
    out.encryptedEvents = obs.eme ? obs.eme.encrypted || 0 : 0;
    // Values the observer took from the page's own responses (declared
    // pointers only; src/content/observe.js checked them).
    for (const rec of obs.observed || []) {
      if (rec) out.observed.push({ key: rec.key, index: rec.index, url: rec.url, values: JSON.parse(JSON.stringify(rec.values || {})) });
    }
    // The profile whose regexes the observer stopped as too slow on this
    // page, if any (src/lib/observe.js: combineObserved).
    if (typeof obs.stopped === 'string') out.observeStopped = obs.stopped.slice(0, 1000);
  }

  const clean = (s) => String(s == null ? '' : s).replace(/\s+/g, ' ').trim().slice(0, MAX_TEXT);
  const nodePath = (node) => {
    const path = [];
    for (let n = node; n && n.nodeType === 1 && path.length < 8; n = n.parentElement) {
      path.unshift({ tag: n.tagName.toLowerCase(), id: n.id || '', classes: [...n.classList].slice(0, 8) });
    }
    return path;
  };
  for (const rd of reads || []) {
    const res = { key: rd.key, selector: rd.selector, attr: rd.attr || '', count: 0, value: '', nodes: [] };
    for (const { doc } of docs) {
      let list;
      try {
        list = doc.querySelectorAll(rd.selector);
      } catch {
        res.count = -1; // invalid selector
        break;
      }
      if (list.length === 0) continue;
      res.count = list.length;
      const first = list[0];
      res.value = clean(rd.attr ? first.getAttribute(rd.attr) : first.textContent);
      res.nodes = [...list].slice(0, 3).map((n) => ({ path: nodePath(n), attrNames: n.getAttributeNames() }));
      break;
    }
    out.reads.push(res);
  }
  return out;
}

/**
 * This frame's own title (document.title: the line the browser shows on
 * the tab), for generic detection to name the lecture with
 * (src/lib/profile.js pageTitleFor). The background injects it only after
 * the scan was screened for the do-not-handle list, only into the frame
 * detection runs in, and only when no profile matched
 * (src/background/detector.js). The frame's address, the addresses its
 * document showed and location.ancestorOrigins come with it so that the
 * background can check again that this is still the frame it screened and
 * that it is not, and is not inside, a blocked site
 * (src/lib/blockedframes.js titleOfFrame). Never part of a report.
 *
 * expectedUrl: the address the screened scan saw. Unless the frame still
 * shows it (compared without the fragment, in the same synchronous run as
 * the read), the title is not read at all: a page that moved on to a
 * blocked site in between is never read (SEC15 L3).
 */
export function frameTitle(expectedUrl) {
  const MAX_TEXT = 1000;
  const MAX_HISTORY = 50;
  const out = { url: '', title: '', history: [], ancestors: [] };
  const bare = (u) => String(u).split('#')[0];
  try {
    out.url = String(location.href);
  } catch {
    return out;
  }
  if (typeof expectedUrl !== 'string' || bare(out.url) !== bare(expectedUrl)) return out;
  try {
    const nav = performance.getEntriesByType('navigation')[0];
    if (nav && typeof nav.name === 'string') out.history.push(nav.name);
  } catch {
    // no navigation timing
  }
  try {
    const api = globalThis.navigation;
    if (api && typeof api.entries === 'function') {
      for (const e of api.entries()) {
        if (out.history.length >= MAX_HISTORY) break;
        if (e && e.sameDocument && typeof e.url === 'string' && !out.history.includes(e.url)) out.history.push(e.url);
      }
    }
  } catch {
    // no Navigation API
  }
  try {
    const list = location.ancestorOrigins;
    if (list) out.ancestors = [...list].slice(0, 64).map(String);
  } catch {
    // none
  }
  try {
    out.title = String(document.title || '').replace(/\s+/g, ' ').trim().slice(0, MAX_TEXT);
  } catch {
    out.title = '';
  }
  return out;
}

/**
 * Fetches URLs from the page, with the page's credentials (cookies go only
 * where the browser would send them for the page anyway).
 * reqs: [{ url, mode: 'text' | 'probe', maxBytes }]
 * Returns per request { ok, status, contentType, text?, size?, error? }.
 * One timeout (timeoutMs) covers each request from the first byte sent to
 * the last byte read; a playlist is read as a stream and abandoned past
 * maxBytes (security review L-4).
 *
 * expected: the document the frame was scanned in — { token }, the mark
 * scanFrame left in this extension's isolated world of that document
 * (gone with the document, untouched by pushState / replaceState), or,
 * from a scan without one, { url }, the address it was scanned at
 * (compared without the fragment). Unless the frame still shows that
 * document (checked before anything is sent), nothing is fetched and every
 * request answers error 'moved': the frame went on to another page while
 * detection ran, and the addresses the page before gave are never fetched
 * from this one (SEC33 信-1, SEC36 低-9; Chrome also pins the injection to
 * the scanned document). timeoutMs other than a positive number is the
 * default.
 */
export async function pageFetch(reqs, timeoutMs = 10000, expected = null) {
  const TIMEOUT = typeof timeoutMs === 'number' && timeoutMs > 0 ? timeoutMs : 10000;
  if (expected && typeof expected === 'object') {
    let same = true;
    if (typeof expected.token === 'string' && expected.token !== '') {
      let mark;
      try {
        mark = globalThis.__lectureflowScan;
      } catch {
        mark = undefined;
      }
      same = mark === expected.token;
    } else if (typeof expected.url === 'string') {
      const bare = (u) => String(u).split('#')[0];
      let here = '';
      try {
        here = String(location.href);
      } catch {
        here = '';
      }
      same = bare(here) === bare(expected.url);
    }
    if (!same) return reqs.map(() => ({ ok: false, status: 0, contentType: '', text: '', size: -1, error: 'moved' }));
  }
  const tooLarge = () => Object.assign(new Error('too large'), { name: 'TooLargeError' });
  // The request's body as text, at most max bytes; stops when signal aborts.
  const readCapped = async (res, max, signal) => {
    if (!res.body || typeof res.body.getReader !== 'function') {
      const t = await res.text();
      if (new TextEncoder().encode(t).length > max) throw tooLarge();
      return t;
    }
    const reader = res.body.getReader();
    const stop = new Promise((_, reject) => {
      const fail = () => reject(Object.assign(new Error('aborted'), { name: 'AbortError' }));
      if (signal.aborted) fail();
      else signal.addEventListener('abort', fail, { once: true });
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
        if (total > max) throw tooLarge();
        text += dec.decode(value, { stream: true });
      }
    } catch (e) {
      reader.cancel().catch(() => {});
      throw e;
    }
  };
  const total = (h) => {
    const m = /\/\s*(\d+)\s*$/.exec(h || '');
    return m ? Number(m[1]) : -1;
  };
  const out = [];
  for (const r of reqs) {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), TIMEOUT);
    const run = (url, init) => fetch(url, { ...init, signal: ctl.signal, redirect: 'follow', cache: 'no-store' });
    // First as the page's own same-origin request would go (cookies only to
    // the page's origin; signed CDN URLs need none). A cross-origin server
    // that refuses that (401/403, or CORS) is asked once more with the
    // browser's cookies for it, as a player using credentials would.
    const withCreds = async (url, init) => {
      let first;
      try {
        first = await run(url, { ...init, credentials: 'same-origin' });
        if (first.status !== 401 && first.status !== 403) return first;
      } catch (e) {
        if (e && e.name === 'AbortError') throw e;
      }
      if (new URL(url, location.href).origin === location.origin) {
        if (first) return first;
        throw new TypeError('failed');
      }
      try {
        return await run(url, { ...init, credentials: 'include' });
      } catch (e) {
        if (first) return first;
        throw e;
      }
    };
    try {
      if (r.mode === 'probe') {
        let size = -1;
        let status = 0;
        let type = '';
        let ok = false;
        try {
          const res = await withCreds(r.url, { method: 'HEAD' });
          status = res.status;
          type = res.headers.get('content-type') || '';
          if (res.ok) {
            ok = true;
            const len = Number(res.headers.get('content-length'));
            if (Number.isFinite(len) && len > 0) size = len;
          }
        } catch (e) {
          if (e && e.name === 'AbortError') throw e;
          // HEAD refused or blocked; try a one-byte range below
        }
        if (size < 0) {
          const res = await withCreds(r.url, { method: 'GET', headers: { Range: 'bytes=0-0' } });
          status = res.status;
          type = res.headers.get('content-type') || type;
          if (res.status === 206) {
            ok = true;
            size = total(res.headers.get('content-range'));
          } else if (res.ok) {
            ok = true;
            const len = Number(res.headers.get('content-length'));
            if (Number.isFinite(len) && len > 0) size = len;
          }
          // never read a whole file: the finally below aborts the body
        }
        out.push({ ok, status, contentType: type, size });
      } else {
        const res = await withCreds(r.url, { method: 'GET' });
        const type = res.headers.get('content-type') || '';
        if (!res.ok) {
          out.push({ ok: false, status: res.status, contentType: type, text: '' });
          continue;
        }
        const len = Number(res.headers.get('content-length'));
        if (Number.isFinite(len) && len > r.maxBytes) {
          out.push({ ok: false, status: res.status, contentType: type, text: '', error: 'too large' });
          continue;
        }
        try {
          out.push({ ok: true, status: res.status, contentType: type, text: await readCapped(res, r.maxBytes, ctl.signal) });
        } catch (e) {
          if (!e || e.name !== 'TooLargeError') throw e;
          out.push({ ok: false, status: res.status, contentType: type, text: '', error: 'too large' });
        }
      }
    } catch (e) {
      out.push({ ok: false, status: 0, contentType: '', text: '', size: -1, error: String((e && e.name) || 'error') });
    } finally {
      clearTimeout(timer);
      ctl.abort(); // whatever was not read (a refused first try, a range body) is dropped
    }
  }
  return out;
}
