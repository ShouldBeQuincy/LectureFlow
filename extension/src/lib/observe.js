// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Page-response observation (profile `observe`, lead ruling 1 in NOTES
// "ext 第三波前的裁决"). Some platforms put what the popup needs only in the
// JSON answer to a request the page itself makes (Panopto: the download
// switch and the full MP4 in DeliveryInfo). A profile declares that request
// by a URL pattern and names the few JSON pointers it needs; on authorized
// sites that match the profile, a main-world content script
// (src/content/observe-main.js) wraps fetch and XHR, and when a response's
// URL matches it takes those pointers' values and nothing else. The
// extension sends no request of its own for this and never keeps or
// forwards a whole response.
//
// This module is the pure side: pointer syntax, the configuration handed to
// the content scripts, re-validating what they report, combining it per
// profile, and the host patterns the main-world script is registered on.
// The content scripts are classic scripts and carry their own small copies
// of the extraction rules; test/unit/observe.test.mjs runs them against the
// same cases.

import { hostMatches, parseHttpUrl } from './url.js';
import { MAX_REGEX_INPUT } from './regexcost.js';
import { guardedRegex } from './regexguard.js';

export const MAX_STRING = 2048; // one observed string (a URL) at most
export const MAX_ARRAY = 16; // values a "*" segment yields at most
export const MAX_DEPTH = 8; // segments in one pointer

/**
 * Parses a JSON pointer (RFC 6901) with one extension: a segment that is
 * exactly "*" takes every element of an array. The root pointer ("") is
 * not allowed: it would be the whole response. Returns the segments, or
 * null when the pointer is not valid.
 */
export function parsePointer(pointer) {
  if (typeof pointer !== 'string' || !pointer.startsWith('/')) return null;
  const parts = pointer.slice(1).split('/');
  if (parts.length === 0 || parts.length > MAX_DEPTH) return null;
  const out = [];
  for (const p of parts) {
    if (p === '') return null;
    if (p === '*') {
      out.push({ any: true });
      continue;
    }
    if (/~[^01]|~$/.test(p) || p.includes('*')) return null;
    out.push({ key: p.replace(/~1/g, '/').replace(/~0/g, '~') });
  }
  return out;
}

/** A value an observation may carry: short strings, finite numbers, booleans, null. */
export function isObservedScalar(v) {
  return v === null || typeof v === 'boolean' || (typeof v === 'number' && Number.isFinite(v)) || (typeof v === 'string' && v.length <= MAX_STRING);
}

/**
 * The value at a pointer, reduced to what may leave the page: a scalar, or
 * (after a "*" segment) an array of at most MAX_ARRAY scalars. Objects and
 * arrays found at the end of a pointer are dropped (undefined), so a
 * pointer can never carry a sub-tree of the response. Missing: undefined.
 */
export function readPointer(obj, segments) {
  let many = false;
  let cur = [obj];
  for (const s of segments) {
    const next = [];
    for (const c of cur) {
      if (s.any) {
        many = true;
        if (Array.isArray(c)) next.push(...c.slice(0, MAX_ARRAY));
      } else if (c !== null && typeof c === 'object') {
        if (Array.isArray(c)) {
          if (/^(0|[1-9]\d*)$/.test(s.key) && Number(s.key) < c.length) next.push(c[Number(s.key)]);
        } else if (Object.prototype.hasOwnProperty.call(c, s.key)) next.push(c[s.key]);
      }
    }
    cur = next;
  }
  if (many) return cur.filter(isObservedScalar).slice(0, MAX_ARRAY);
  if (cur.length === 0) return undefined;
  return isObservedScalar(cur[0]) ? cur[0] : undefined;
}

/** Whether a value read through a pointer is "empty": missing, null, "", false, []. */
export function isEmptyObserved(v) {
  if (v === undefined || v === null || v === '' || v === false) return true;
  if (Array.isArray(v)) return v.every(isEmptyObserved);
  return false;
}

/** "id:version" of a profile, so values taken under an older version are not mixed up. */
export function profileKey(profile) {
  return `${profile.id}:${profile.version}`;
}

/**
 * What the content scripts of a frame need for a matched profile, or null
 * when it observes nothing: { key, specs: [{ urlPattern, hosts, pointers }] }.
 * hosts are the profile's own page hosts: only requests to the platform
 * itself are looked at.
 */
export function observeConfig(profile) {
  if (!profile || !Array.isArray(profile.observe) || profile.observe.length === 0) return null;
  const hosts = [...profile.match.hosts];
  return {
    key: profileKey(profile),
    specs: profile.observe.map((o) => ({ urlPattern: o.urlPattern, hosts, pointers: { ...o.pointers } })),
  };
}

/** Whether an observed request URL fits a spec: platform host, pattern on the URL without query (behind the regex guard; owner: the profile's key). */
export function specMatches(spec, rawUrl, owner = '') {
  const u = parseHttpUrl(rawUrl);
  if (!u) return false;
  if (!spec.hosts.some((h) => hostMatches(h, u.hostname))) return false;
  const bare = `${u.protocol}//${u.host}${u.pathname}`;
  if (bare.length > MAX_REGEX_INPUT) return false; // as profileRegex: never on long inputs
  try {
    return guardedRegex(spec.urlPattern, { owner }).test(bare);
  } catch {
    return false;
  }
}

/**
 * Re-validates one record a frame's scan returned ({ key, index, url,
 * values }) against the profile: same profile version, a declared request,
 * declared names only, values of the allowed shapes. Returns the cleaned
 * record or null.
 */
export function cleanObservedRecord(profile, rec) {
  const cfg = observeConfig(profile);
  if (!cfg || !rec || typeof rec !== 'object' || rec.key !== cfg.key) return null;
  const spec = Number.isInteger(rec.index) ? cfg.specs[rec.index] : undefined;
  if (!spec || typeof rec.url !== 'string' || !specMatches(spec, rec.url, cfg.key)) return null;
  const values = {};
  const vals = rec.values && typeof rec.values === 'object' ? rec.values : {};
  for (const name of Object.keys(spec.pointers)) {
    if (!Object.prototype.hasOwnProperty.call(vals, name)) continue;
    const v = vals[name];
    if (Array.isArray(v)) values[name] = v.filter(isObservedScalar).slice(0, MAX_ARRAY);
    else if (isObservedScalar(v)) values[name] = v;
  }
  const u = parseHttpUrl(rec.url);
  return { key: rec.key, index: rec.index, url: `${u.protocol}//${u.host}${u.pathname}`, values };
}

/**
 * Combines the observations of all frames (top frame first) for a profile.
 * Returns { seen: Set of observe indexes answered, values: { name: value },
 * urls: { name: request URL without query }, stopped: whether a frame's
 * observer stopped the profile's regexes as too slow } — urls resolve
 * relative values. A name of an index that was answered but lacks the
 * field is "seen" with value undefined.
 */
export function combineObserved(profile, frames) {
  const out = { seen: new Set(), values: {}, urls: {}, stopped: false };
  const cfg = observeConfig(profile);
  if (!cfg) return out;
  const key = profileKey(profile);
  const ordered = [...(frames || [])].sort((a, b) => (a.frameId === 0 ? -1 : b.frameId === 0 ? 1 : a.frameId - b.frameId));
  for (const f of ordered) {
    // a frame's observer stopped this profile's regexes as too slow: what
    // it did not see yet it never will (SEC18f 低-18)
    if (f.observeStopped === key) out.stopped = true;
    for (const raw of f.observed || []) {
      const rec = cleanObservedRecord(profile, raw);
      if (!rec || out.seen.has(rec.index)) continue;
      out.seen.add(rec.index);
      for (const name of Object.keys(cfg.specs[rec.index].pointers)) {
        out.values[name] = rec.values[name];
        out.urls[name] = rec.url;
      }
    }
  }
  return out;
}

/** The observe index that declares a pointer name, or -1. */
export function observeIndexOf(profile, name) {
  return (profile.observe || []).findIndex((o) => Object.prototype.hasOwnProperty.call(o.pointers || {}, name));
}

// ------------------------------------------------- registration patterns

function splitPattern(p) {
  const m = /^(\*|https?):\/\/([^/]+)\/(.*)$/.exec(String(p));
  return m ? { scheme: m[1], host: m[2].toLowerCase(), path: m[3] } : null;
}

/** Whether host wildcard a covers host wildcard b (a ⊇ b). */
export function hostWildcardCovers(a, b) {
  if (a === '*') return true;
  if (b === '*') return false;
  if (a.startsWith('*.')) {
    const base = a.slice(2);
    const bb = b.startsWith('*.') ? b.slice(2) : b;
    return bb === base || bb.endsWith('.' + base);
  }
  return !b.startsWith('*.') && a === b;
}

/**
 * Granted origin patterns intersected with host wildcards: where a content
 * script for those hosts may be registered. granted: ["https://*.panopto.eu/*", ...].
 */
export function grantedHostPatterns(granted, hostList) {
  const hosts = new Set((hostList || []).map((h) => String(h).toLowerCase()));
  const out = new Set();
  for (const g of granted || []) {
    const gp = splitPattern(g);
    if (!gp) continue;
    for (const h of hosts) {
      if (hostWildcardCovers(gp.host, h)) out.add(`${gp.scheme}://${h}/*`);
      else if (hostWildcardCovers(h, gp.host)) out.add(`${gp.scheme}://${gp.host}/*`);
    }
  }
  return [...out].sort();
}

/**
 * Match patterns for the main-world observer: the granted origins
 * intersected with the hosts of the profiles that observe responses (so the
 * page's fetch and XHR are wrapped only where a profile needs it and the
 * user said yes).
 */
export function observerMatches(granted, profiles) {
  const hosts = [];
  for (const p of profiles || []) if (p && Array.isArray(p.observe) && p.observe.length) hosts.push(...p.match.hosts);
  return grantedHostPatterns(granted, hosts);
}
