// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// URL helpers shared by detection, profiles and the report. Pure: no
// browser APIs beyond URL, URLSearchParams and Web Crypto, which Node has too.

/** Parses an absolute http(s) URL; returns null for anything else. */
export function parseHttpUrl(raw, base) {
  let u;
  try {
    u = base === undefined ? new URL(raw) : new URL(raw, base);
  } catch {
    return null;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
  return u;
}

// Resolving a reference against a page's base without reading more of the
// base than the URL parser does, so that a long signed query (or a long
// token in the path) costs only where it ends up in the result. The forms,
// after the parser's own clean-up (leading and trailing C0 controls and
// spaces dropped, tabs and newlines removed), follow the WHATWG URL
// parser's scheme, no scheme, special relative or authority, relative and
// relative slash states, for an http(s) base:
//   - another scheme, or the base's scheme followed by two slashes or
//     backslashes: absolute, the base is not read;
//   - two slashes or backslashes, no scheme: only the base's scheme;
//   - one slash or backslash (with or without the base's scheme): the
//     base's scheme, user info, host and port ("root");
//   - nothing, or a fragment (with or without the base's scheme): the whole
//     base, query included;
//   - anything else (a path, or a query): the base without its query and
//     fragment ("path").
// The result is byte for byte new URL(ref, base); manifest-work.test.mjs
// checks it against the parser for a few thousand pairs.

const SCHEME_PREFIX = /^([A-Za-z][A-Za-z0-9+.-]*):/;

/** ref as the URL parser sees it: trimmed of C0 and spaces, no tab or newline. */
function cleanRef(ref) {
  const s = String(ref);
  let i = 0;
  let end = s.length;
  while (i < end && s.charCodeAt(i) <= 0x20) i++;
  while (end > i && s.charCodeAt(end - 1) <= 0x20) end--;
  let out = '';
  let from = i;
  for (let k = i; k < end; k++) {
    const c = s.charCodeAt(k);
    if (c === 0x09 || c === 0x0a || c === 0x0d) {
      out += s.slice(from, k);
      from = k + 1;
    }
  }
  return out + s.slice(from, end);
}

function slashesAt(s, i) {
  let n = 0;
  while (i + n < s.length && (s[i + n] === '/' || s[i + n] === '\\')) n++;
  return n;
}

/** 'absolute' | 'scheme' | 'root' | 'path' | 'full' for an http(s) base's scheme. */
function refForm(t, scheme) {
  const m = SCHEME_PREFIX.exec(t);
  let rest = t;
  if (m) {
    if (m[1].toLowerCase() !== scheme) return 'absolute';
    rest = t.slice(m[0].length);
  }
  if (rest === '' || rest[0] === '#') return 'full';
  const n = slashesAt(rest, 0);
  if (n >= 2) return m ? 'absolute' : 'scheme';
  return n === 1 ? 'root' : 'path';
}

let partsKey = null;
let partsVal = null;

/** { scheme, root, path, full } of an http(s) base; null for anything else. */
function baseParts(base) {
  if (base === partsKey) return partsVal;
  const u = parseHttpUrl(String(base));
  let v = null;
  if (u) {
    const scheme = u.protocol.slice(0, -1);
    u.search = '';
    u.hash = '';
    const path = u.href;
    u.pathname = '/';
    v = { scheme, root: u.href, path, full: String(base) };
  }
  partsKey = base;
  partsVal = v;
  return v;
}

/** An absolute http(s) URL without its query and fragment ('' when not one). */
export function basePath(base) {
  const p = baseParts(base);
  return p ? p.path : '';
}

/**
 * The part of base that resolving ref reads, as a string: '' for an
 * absolute ref (or no base), the scheme for a scheme-relative one, the
 * root, the path or the whole base (see above); a base that is not
 * http(s) is read whole.
 */
export function baseRead(ref, base) {
  if (base === undefined || base === null) return '';
  const p = baseParts(base);
  if (!p) return String(base);
  const form = refForm(cleanRef(ref), p.scheme);
  if (form === 'absolute') return '';
  if (form === 'scheme') return `${p.scheme}:`;
  return p[form];
}

/**
 * new URL(ref, base) as an http(s) URL (null otherwise), byte for byte,
 * reading of base only what the parser reads (see above). When that part
 * is longer than maxBase (or base is undefined), only an absolute ref
 * resolves.
 */
export function resolveRef(ref, base, maxBase = Infinity) {
  if (base === undefined || base === null) return parseHttpUrl(ref);
  const p = baseParts(base);
  if (!p) return parseHttpUrl(ref, base);
  const t = cleanRef(ref);
  const form = refForm(t, p.scheme);
  if (form === 'absolute') return parseHttpUrl(t);
  if (form === 'scheme') return parseHttpUrl(`${p.scheme}:${t}`);
  const b = p[form];
  return b.length <= maxBase ? parseHttpUrl(t, b) : parseHttpUrl(t);
}

/**
 * What resolveRef(ref, base, maxBase) costs, in characters: the longest of
 * ref, the part of base it reads (when within maxBase) and the result
 * (its href, or 0 when it does not resolve). An absolute ref costs no base,
 * a root-relative one only the base's origin. Budgets charge every attempt
 * this much, so work stays bounded even when the results are refused.
 */
export function resolveCost(ref, base, maxBase, result) {
  const b = baseRead(ref, base).length;
  return Math.max(String(ref).length, b <= maxBase ? b : 0, result ? result.length : 0);
}

/**
 * The Secure Contexts standard's "potentially trustworthy URL" for an
 * http(s) URL object: https, or a loopback host (127.0.0.0/8, ::1, also
 * IPv4-mapped, localhost and *.localhost). Browsers send a Referer to such
 * URLs from an HTTPS page; LectureFlow's internal/fetch trustworthy() is the
 * same rule.
 */
export function potentiallyTrustworthy(u) {
  if (!u) return false;
  if (u.protocol === 'https:') return true;
  if (u.protocol !== 'http:') return false;
  const h = u.hostname.toLowerCase().replace(/\.$/, '');
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(h)) return h.startsWith('127.');
  if (h.startsWith('[') && h.endsWith(']')) {
    // the URL parser has already put IPv6 in its canonical form
    const v6 = h.slice(1, -1);
    return v6 === '::1' || /^::ffff:7f[0-9a-f]{2}:[0-9a-f]{1,4}$/.test(v6);
  }
  return h === 'localhost' || h.endsWith('.localhost');
}

/** The dedup key of a media URL: the absolute URL without its fragment. */
export function urlKey(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return null;
  u.hash = '';
  return u.href;
}

/** scheme://host/path — no user info, query or fragment. */
export function withoutQuery(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return null;
  return `${u.protocol}//${u.host}${u.pathname}`;
}

/** Lower-cased file extension of the URL path ("" when none). */
export function pathExtension(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return '';
  return extensionOfPath(u.pathname);
}

export function extensionOfPath(pathname) {
  const last = pathname.slice(pathname.lastIndexOf('/') + 1);
  const dot = last.lastIndexOf('.');
  if (dot <= 0 || dot === last.length - 1) return '';
  const ext = last.slice(dot + 1).toLowerCase();
  return /^[a-z0-9]{1,6}$/.test(ext) ? ext : '';
}

/**
 * Host wildcard with the semantics of a WebExtension match pattern: "*"
 * matches every host, "*.example.com" matches example.com and every
 * subdomain, anything else matches exactly. Case-insensitive.
 */
export function hostMatches(pattern, host) {
  const p = String(pattern).toLowerCase();
  const h = String(host).toLowerCase().replace(/\.$/, '');
  if (p === '*') return true;
  if (p.startsWith('*.')) {
    const base = p.slice(2);
    return h === base || h.endsWith('.' + base);
  }
  return h === p;
}

/**
 * The optional-permission origin pattern for a host wildcard. HTTPS only:
 * the manifest offers no plain-HTTP host access.
 */
export function hostPermissionPattern(hostPattern) {
  return `https://${String(hostPattern).toLowerCase()}/*`;
}

/** Origin pattern ("https://host/*") of an absolute URL, for permission checks. */
export function originPattern(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return null;
  return `${u.protocol}//${u.hostname}/*`;
}

/** The pattern the user can be asked to grant for a URL: HTTPS only. */
export function requestableOrigin(raw) {
  const u = parseHttpUrl(raw);
  if (!u || u.protocol !== 'https:') return null;
  return `https://${u.hostname}/*`;
}

/**
 * Page address normalised for the generic session id (ext-brief §3):
 * fragment dropped, query parameters sorted by key (stable for repeated
 * keys), host lower-cased and default port dropped by the URL parser.
 */
export function normalizePageUrl(raw) {
  const u = parseHttpUrl(raw);
  if (!u) return null;
  u.hash = '';
  u.username = '';
  u.password = '';
  const params = new URLSearchParams(u.search);
  params.sort();
  const q = params.toString();
  u.search = q ? '?' + q : '';
  return u.href;
}

/** Hex SHA-256 of a UTF-8 string (Web Crypto). */
export async function sha256Hex(text) {
  const data = new TextEncoder().encode(text);
  const digest = await globalThis.crypto.subtle.digest('SHA-256', data);
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, '0')).join('');
}

/**
 * Session id of a page when no profile matched: the first 32 hex digits of
 * the SHA-256 of the normalised page address. The address itself is never
 * sent to LectureFlow.
 */
export async function genericSessionId(pageUrl) {
  const norm = normalizePageUrl(pageUrl);
  if (!norm) return '';
  return (await sha256Hex(norm)).slice(0, 32);
}
