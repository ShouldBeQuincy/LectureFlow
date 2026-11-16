// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Redaction for the detection report (SPEC-ext 贡献与合并; ext-brief §5).
// A report keeps the structure of addresses and manifests and nothing that
// identifies a person or a session:
//   - URLs keep scheme, host and path; user info and fragment are dropped.
//   - Long ID-like runs inside path segments (UUIDs, hex, long numbers,
//     long letter-and-digit tokens) are replaced by fakes of the same shape,
//     so profile patterns still match and the same ID always maps to the
//     same fake (a replay can still relate a master to its playlists).
//   - Query parameters keep their names; every value becomes a placeholder
//     such as {uuid-1} or {tok120-3} (class, length, index).
//   - Manifest text is rebuilt from what is understood: URIs as above, known
//     numeric and enumerated attributes kept, free text reduced to its shape.
//   - Page text becomes its shape: letters → x / X, digits → 0.
// While redacting, things that look like personal data are recorded as
// findings; a report with findings is not exported (src/lib/report.js).
// Pure.

import { parseXml, serializeXml, localName } from './xml.js';
import { playlistLines, parseAttributeList } from './hls.js';

// ---------------------------------------------------------------- shapes

/** Shape of free text: letters → x/X (other scripts → 字), digits → 0. */
export function shape(text, max = 120) {
  const s = String(text ?? '').replace(/\s+/g, ' ').trim();
  let out = '';
  for (const ch of s) {
    if (/\p{Lu}/u.test(ch)) out += 'X';
    else if (/\p{Ll}/u.test(ch)) out += 'x';
    else if (/\p{L}/u.test(ch)) out += '字';
    else if (/\p{N}/u.test(ch)) out += '0';
    else if (ch === '@') out += '_'; // never let an address survive as a shape
    else out += ch;
    if (out.length >= max) {
      out += '…';
      break;
    }
  }
  return out;
}

// ------------------------------------------------------- identity fields

const IDENTITY_FIELD = new RegExp(
  '^(e-?mail|mail|mail-?address|user|user-?name|user-?id|login|login-?name|student|student-?(id|no|num|number)|stu-?(id|no)|' +
    'net-?id|first-?name|last-?name|full-?name|sur-?name|given-?name|family-?name|display-?name|real-?name|' +
    'person|person-?id|learner|learner-?id|matric|matriculation|matriculation-?(no|number)|candidate-?id|' +
    'account|account-?(id|name)|author|viewer-?(id|name)|name-?of-?user)$',
  'i',
);

/** Whether a field name (query parameter, attribute) names a person. */
export function isIdentityField(name) {
  const n = String(name || '')
    .replace(/^data-/i, '')
    .replace(/([a-z])([A-Z])/g, '$1-$2')
    .replace(/[_\s.]+/g, '-')
    .toLowerCase();
  return IDENTITY_FIELD.test(n);
}

const EMAIL_LIKE = /[^\s@/?#&="'<>]+(?:@|%40)[^\s@/?#&="'<>]+\.[A-Za-z]{2,}/i;
const KNOWN_EXT = new Set([
  'm3u8', 'm3u', 'mpd', 'mp4', 'm4a', 'm4v', 'mp3', 'aac', 'ogg', 'oga', 'ogv', 'opus', 'wav', 'flac', 'webm', 'weba',
  'mov', 'mkv', 'ts', 'm4s', 'vtt', 'srt', 'ism', 'isml', 'hls', 'dash', 'key', 'aspx', 'asp', 'php', 'jsp', 'html',
  'htm', 'xml', 'json', 'js', 'do', 'action', 'cgi', 'png', 'jpg', 'jpeg', 'svg', 'css', 'ashx', 'axd', 'smil',
]);
const PEOPLE_DIRS = new Set(['user', 'users', 'people', 'person', 'student', 'students', 'staff', 'profile', 'profiles', 'members', 'member', 'u', 'home']);

// ------------------------------------------------------------- redactor

const UUID_RE = '[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}';
// One pass over a segment: a template variable is skipped, an ID-like run
// replaced. Groups: 1 template, 2 base64url-like token (upper case, lower
// case and digits, 32+ characters: Kaltura KS tickets and JWTs can sit in
// paths), 3 uuid, 4 hex, 5 letter-and-digit token, 6 digits, 7 a run of
// 32+ letters (with - or _ at most; see lettersToken): a token with no
// digit in it (security review L6), 8 a shorter letter-and-digit run (10 to
// 19 characters), 9 a run of 10 to 31 letters: both kept only when they
// read as words and numbers (see wordish; FB1 L-3, SEC18 低-4 / 低-6).
const SEGMENT_RE = new RegExp(
  `(\\$[A-Za-z]*(?:%0\\d+d)?\\$)|((?<![A-Za-z0-9_-])(?=[A-Za-z0-9_-]*[a-z])(?=[A-Za-z0-9_-]*[A-Z])(?=[A-Za-z0-9_-]*[0-9])[A-Za-z0-9_-]{32,}(?![A-Za-z0-9_-]))|(${UUID_RE})|((?<![0-9A-Za-z])[0-9a-fA-F]{16,}(?![0-9A-Za-z]))|((?<![0-9A-Za-z])(?=[A-Za-z0-9]*[0-9])(?=[A-Za-z0-9]*[A-Za-z])[A-Za-z0-9]{20,}(?![0-9A-Za-z]))|(\\d{5,})|((?<![A-Za-z0-9_-])[A-Za-z_-]{32,}(?![A-Za-z0-9_-]))|((?<![0-9A-Za-z])(?=[A-Za-z]*[0-9])(?=[0-9]*[A-Za-z])[A-Za-z0-9]{10,19}(?![0-9A-Za-z]))|((?<![0-9A-Za-z])[A-Za-z]{10,31}(?![0-9A-Za-z]))`,
  'g',
);

const isUpper = (c) => c >= 'A' && c <= 'Z';
const isLower = (c) => c >= 'a' && c <= 'z';
const isDigit = (c) => c >= '0' && c <= '9';

/**
 * Whether a run of letters and digits reads as words and numbers rather
 * than a token (SEC18 低-6): it splits whole into
 *   W  a word: [A-Z]?[a-z]{3,}                 ("Week", "lecture", "Audio")
 *   A  an acronym: [A-Z]{2,5}                  ("CS", "AAC")
 *   N  a number, with a unit from UNITS: \d+(unit)?  ("10", "1080p", "128k", "1920x")
 *   S  a short code: [A-Za-z][a-z]?\d+(unit)?      ("Mp4", "H264", "Hz2ch"), at most one
 * with at least one W — or only N ("1920x1080p30"). "Week10Lecture2",
 * "Mp4Audio128k", "DeliveryInfo" read so; "Xk9pQ2mZ7vL4", "QwErTyUiOpAsDf",
 * "ABCDEFGHIJKL", "CSC3631W10L2" do not. A random string is unlikely to
 * split this way; when unsure, the run is masked.
 */
const UNITS = new Set(['x', 'p', 'i', 'k', 'm', 's', 'h', 'ch', 'hz', 'kb', 'mb', 'gb', 'ms', 'st', 'nd', 'rd', 'th']);
export function wordish(run) {
  const s = String(run);
  const n = s.length;
  if (n === 0 || n > 64) return false;
  const span = (i, f) => {
    let j = i;
    while (j < n && f(s[j])) j++;
    return j - i;
  };
  const memo = new Map();
  const go = (i, word, codes, allNumbers) => {
    if (codes > 1) return false;
    if (i === n) return word || allNumbers;
    const key = `${i}|${word}|${codes}|${allNumbers}`;
    if (memo.has(key)) return memo.get(key);
    const tries = [];
    const up = isUpper(s[i]) ? 1 : 0;
    for (let k = 3, L = span(i + up, isLower); k <= L; k++) tries.push([up + k, 'W']);
    for (let k = 2, U = Math.min(5, span(i, isUpper)); k <= U; k++) tries.push([k, 'A']);
    const unit = (j, k) => k === 0 || UNITS.has(s.slice(j, j + k));
    const d = span(i, isDigit);
    if (d) for (let k = 0, L = Math.min(2, span(i + d, isLower)); k <= L; k++) if (unit(i + d, k)) tries.push([d + k, 'N']);
    if (isUpper(s[i]) || isLower(s[i])) {
      for (const pre of [1, 2]) {
        if (pre === 2 && !isLower(s[i + 1])) continue;
        const dd = span(i + pre, isDigit);
        if (!dd) continue;
        for (let k = 0, L = Math.min(2, span(i + pre + dd, isLower)); k <= L; k++) if (unit(i + pre + dd, k)) tries.push([pre + dd + k, 'S']);
      }
    }
    let ok = false;
    for (const [len, kind] of tries) {
      if (go(i + len, word || kind === 'W', codes + (kind === 'S' ? 1 : 0), allNumbers && kind === 'N')) {
        ok = true;
        break;
      }
    }
    memo.set(key, ok);
    return ok;
  };
  return go(0, false, 0, true);
}

// A piece of a run cut by "-", "_", "." or a percent escape that may stand
// in a slug: words and numbers as above, a short word ("One", "of"), a
// short acronym, a number, one or two letters with a number ("v2", "w123456789").
function pieceOk(p) {
  return wordish(p) || /^\d+$/.test(p) || /^[A-Z]?[a-z]{1,3}$/.test(p) || /^[A-Z]{1,5}$/.test(p) || /^[A-Za-z]{1,2}\d+$/.test(p);
}

// Letters and digits joined by "-", "_", "." or percent escapes
// ("aB3dE-5gH7j-K9xQ2", "Xk9p%51Q2mZ7vL4"): a token cut into pieces too
// short for the runs above (SEC18 低-4). Taken whole when it holds 12+
// letters and digits and a piece that is not pieceOk; UUIDs are left to
// SEGMENT_RE, a known file extension at the end stays.
const SEP_RUN_RE = /(?<![A-Za-z0-9._%-])[A-Za-z0-9]+(?:(?:[._-]|%[0-9A-Fa-f]{2})[A-Za-z0-9]+)+(?![A-Za-z0-9._%-])/g;
const SEP_SPLIT = /([._-]|%[0-9A-Fa-f]{2})/;

// A language tag as BCP 47 writes it, in its canonical case: language (2–3
// lower-case letters), optional extended language (3 lower-case), script
// (4 letters, the first upper case) and region (2 upper-case letters or 3
// digits): "en", "yue", "zh-Hans-CN", "es-419", "zh-cmn-Hans-CN". Anything
// else in a language field — a name ("Li-Ming", "Bob"), an id, a tag
// written in another case ("en-gb", "EN") — is reduced to its shape (FB1
// L-3 DASH lang, SEC18 低-5).
const LANG_TAG = /^[a-z]{2,3}(?:-[a-z]{3})?(?:-[A-Z][a-z]{3})?(?:-(?:[A-Z]{2}|\d{3}))?$/;

/** A language tag kept as it is when it is one (LANG_TAG), its shape otherwise. */
export function safeLanguage(tag) {
  const s = String(tag ?? '');
  return LANG_TAG.test(s) ? s : shape(s);
}

// A query parameter without a value ("?embed") stays only when it is one of
// these common switches (compared in lower case); anything else — a token,
// a key, a person's name ("?jsmith") — is replaced like a value (FB1 L-3,
// SEC18 低-4).
const FLAG_WORDS = new Set([
  'embed', 'embedded', 'download', 'dl', 'autoplay', 'autostart', 'nocache', 'no-cache', 'nocookie', 'noredirect', 'raw',
  'play', 'preview', 'mobile', 'desktop', 'html5', 'hls', 'dash', 'mp4', 'audio', 'video', 'live', 'hd', 'sd', 'fullscreen',
  'debug', 'amp', 'print', 'inline', 'attachment', 'refresh', 'reload', 'json', 'xml', 'list', 'popup', 'iframe', 'loop', 'muted',
]);
function flagWord(name) {
  return FLAG_WORDS.has(String(name).toLowerCase());
}

/**
 * Whether a 32+ run of letters (and - or _) is a token rather than words: no
 * separator at all (no word or slug is 32 letters in one piece), or the case
 * changing at least once every four characters (random base64 does, a
 * "Lecture-Week-One" slug does not).
 */
function lettersToken(run) {
  if (!/[-_]/.test(run)) return true;
  let changes = 0;
  for (let i = 1; i < run.length; i++) {
    const a = run[i - 1];
    const b = run[i];
    if (/[a-z]/.test(a) && /[A-Z]/.test(b)) changes++;
    else if (/[A-Z]/.test(a) && /[a-z]/.test(b)) changes++;
  }
  return changes * 4 >= run.length;
}

// A base64 token whose "+", "/" or "=" sit in the path percent-encoded
// (%2B, %2F, %3D) or as a literal "+": the escapes cut it into pieces too
// short for the runs above (security review L6). Taken whole when it holds
// an escape, letters and digits, 16+ letters and digits in all, and
// changes between upper case, lower case and digits at least once every
// four of them, as random base64 does — a "C%2B%2B-Programming-2026" slug
// does not.
const B64_ESC_RE = /(?<![A-Za-z0-9_%+-])(?:[A-Za-z0-9_-]|%2[BbFf]|%3[Dd]|\+){24,}(?![A-Za-z0-9_%+-])/g;
function escapedToken(run) {
  if (!/%2[BbFf]|%3[Dd]|\+/.test(run)) return false;
  const alnum = run.replace(/%2[BbFf]|%3[Dd]/g, '').replace(/[^A-Za-z0-9]/g, '');
  if (alnum.length < 16 || !/[0-9]/.test(alnum) || !/[A-Za-z]/.test(alnum)) return false;
  const cls = (c) => (/[A-Z]/.test(c) ? 'U' : /[a-z]/.test(c) ? 'l' : 'd');
  let changes = 0;
  for (let i = 1; i < alnum.length; i++) if (cls(alnum[i]) !== cls(alnum[i - 1])) changes++;
  return changes * 4 >= alnum.length;
}

/**
 * Host placeholders (lead ruling 8): a school's own subdomain of a hosted
 * platform names the school, so under a profile's wildcard hosts
 * ("*.cloud.panopto.eu") the subdomain becomes "tenant" and the regional
 * suffix stays ("tenant.cloud.panopto.eu"); the longest matching suffix
 * wins. Hosts listed in maskHosts (a school site the platform is embedded
 * in) become "site-N.example". Every other host stays as it is: a report
 * for a new platform needs the host to write its profile.
 */
export function hostRedactor({ tenantBases = [], maskHosts = [] } = {}) {
  const bases = [...new Set(tenantBases.map((b) => String(b).toLowerCase()))].sort((a, b) => b.length - a.length);
  const masked = new Map();
  for (const h of maskHosts) {
    const k = String(h).toLowerCase();
    if (k && !masked.has(k)) masked.set(k, `site-${masked.size + 1}.example`);
  }
  return (hostname) => {
    const h = String(hostname).toLowerCase();
    if (masked.has(h)) return masked.get(h);
    for (const b of bases) if (h.endsWith('.' + b)) return `tenant.${b}`;
    return h;
  };
}

/** The wildcard bases of a profile's page hosts ("*.cloud.panopto.eu" → "cloud.panopto.eu"). */
export function tenantBasesOf(profile) {
  return (profile?.match?.hosts || []).filter((h) => h.startsWith('*.')).map((h) => h.slice(2));
}

export function createRedactor(opts = {}) {
  const maps = { uuid: new Map(), hex: new Map(), tok: new Map(), num: new Map(), qv: new Map() };
  const fakes = new Set();
  const findings = [];
  const mapHost = hostRedactor(opts);
  let where = '';

  const note = (kind, detail) => {
    const key = `${kind}|${where}|${detail}`;
    if (!findings.some((f) => f.key === key)) findings.push({ key, kind, where, detail });
  };

  const counter = (map, original, make) => {
    if (map.has(original)) return map.get(original);
    const fake = make(map.size + 1);
    map.set(original, fake);
    fakes.add(fake);
    return fake;
  };

  const fakeUuid = (s) =>
    counter(maps.uuid, s.toLowerCase(), (n) => {
      const f = `00000000-0000-4000-8000-${n.toString(16).padStart(12, '0')}`;
      return f;
    });
  const fakeHex = (s) =>
    counter(maps.hex, s.toLowerCase(), (n) => n.toString(16).padStart(s.length, '0'));
  const fakeNum = (s) => counter(maps.num, s, (n) => String(n).padStart(s.length, '0'));
  const fakeTok = (s) =>
    counter(maps.tok, s, (n) => {
      const mask = s.replace(/[A-Z]/g, 'A').replace(/[a-z]/g, 'a').replace(/[0-9]/g, '0').replace(/[-_]/g, 'a');
      const id = String(n);
      return id + mask.slice(id.length);
    });

  function upperIf(orig, fake) {
    return orig === orig.toUpperCase() && /[A-F]/.test(orig) ? fake.toUpperCase() : fake;
  }

  // An escaped base64 token: the same shape with its escapes in place.
  const fakeEscTok = (s) =>
    counter(maps.tok, s, (n) => {
      const parts = s.split(/(%2[BbFf]|%3[Dd]|\+)/);
      let id = String(n);
      return parts
        .map((p, i) => {
          if (i % 2 === 1) return p;
          let mask = p.replace(/[A-Z]/g, 'A').replace(/[a-z]/g, 'a').replace(/[0-9]/g, '0').replace(/[-_]/g, 'a');
          if (id && mask.length >= id.length) {
            mask = id + mask.slice(id.length);
            id = '';
          }
          return mask;
        })
        .join('');
    });

  // A token cut by separators: the same shape, separators kept, a percent
  // escape masked as "%00".
  const fakeSepTok = (s) =>
    counter(maps.tok, s, (n) => {
      let id = String(n);
      return s
        .split(SEP_SPLIT)
        .map((p, i) => {
          if (i % 2 === 1) return p.startsWith('%') ? '%00' : p;
          let mask = p.replace(/[A-Z]/g, 'A').replace(/[a-z]/g, 'a').replace(/[0-9]/g, '0');
          if (id && mask.length >= id.length) {
            mask = id + mask.slice(id.length);
            id = '';
          }
          return mask;
        })
        .join('');
    });

  /** A run joined by separators (SEP_RUN_RE): its fake, or '' to leave it to SEGMENT_RE. */
  function sepRun(m) {
    if (new RegExp(UUID_RE).test(m)) return '';
    let body = m;
    let ext = '';
    const dot = m.lastIndexOf('.');
    if (dot > 0 && KNOWN_EXT.has(m.slice(dot + 1).toLowerCase())) {
      body = m.slice(0, dot);
      ext = m.slice(dot);
    }
    const pieces = body.split(SEP_SPLIT).filter((_, i) => i % 2 === 0);
    if (pieces.join('').length < 12 || pieces.every(pieceOk)) return '';
    return fakeSepTok(body) + ext;
  }

  /** One path segment (still percent-encoded). */
  function segment(seg, prev = '') {
    if (EMAIL_LIKE.test(seg) || /@|%40/i.test(seg)) {
      note('email', 'path');
      return '_redacted_';
    }
    // Escaped base64 tokens first, kept aside so the runs below do not see
    // their fakes again.
    const aside = [];
    const work = seg.replace(B64_ESC_RE, (m) => {
      if (!escapedToken(m)) return m;
      aside.push(fakeEscTok(m));
      return `\u0000${aside.length - 1}\u0000`;
    }).replace(SEP_RUN_RE, (m) => {
      const fake = sepRun(m);
      if (!fake) return m;
      aside.push(fake);
      return `\u0000${aside.length - 1}\u0000`;
    });
    const out = work
      .replace(SEGMENT_RE, (m, tpl, b64, uuid, hex, tok, num, letters, short, word) => {
        if (tpl) return tpl;
        if (b64) return fakeTok(b64);
        if (uuid) return upperIf(uuid, fakeUuid(uuid));
        if (hex) return upperIf(hex, fakeHex(hex));
        if (tok) return fakeTok(tok);
        if (num) return fakeNum(num);
        if (letters) return lettersToken(letters) ? fakeTok(letters) : letters;
        // not a token: its long digit runs are still ids ("w123456789")
        if (short) return pieceOk(short) ? short.replace(/\d{5,}/g, fakeNum) : fakeTok(short);
        if (word) return pieceOk(word) ? word : fakeTok(word);
        return m;
      })
      .replace(/\u0000(\d+)\u0000/g, (_, i) => aside[Number(i)]);
    if (out === seg) {
      let decoded = seg;
      try {
        decoded = decodeURIComponent(seg);
      } catch {
        // keep the raw form
      }
      if (decoded.startsWith('~')) note('name', 'path segment starting with ~');
      const two = /^([A-Za-z]{2,})\.([A-Za-z]{2,})$/.exec(decoded);
      if (two && !KNOWN_EXT.has(two[2].toLowerCase())) note('name', 'path segment shaped like first.last');
      if (PEOPLE_DIRS.has(String(prev).toLowerCase()) && /^[A-Za-z][A-Za-z._-]{1,}$/.test(decoded)) {
        note('name', `path segment after /${prev}/`);
      }
    }
    return out;
  }

  function path(p) {
    const parts = p.split('/');
    return parts.map((s, i) => (s === '' || s === '.' || s === '..' ? s : segment(s, parts[i - 1] || ''))).join('/');
  }

  function valuePlaceholder(raw) {
    let v = raw;
    try {
      v = decodeURIComponent(raw.replace(/\+/g, ' '));
    } catch {
      // keep raw
    }
    if (v === '') return '';
    if (EMAIL_LIKE.test(v) || v.includes('@')) note('email', 'query value');
    let cls;
    if (new RegExp(`^${UUID_RE}$`).test(v)) cls = 'uuid';
    else if (/^\d+$/.test(v)) cls = `num${v.length}`;
    else if (/^[0-9a-fA-F]{8,}$/.test(v)) cls = `hex${v.length}`;
    else if (v.length >= 16) cls = `tok${v.length}`;
    else cls = `str${v.length}`;
    return counter(maps.qv, v, (n) => `{${cls}-${n}}`);
  }

  /** "a=1&b=x" → "a={num1-1}&b={str1-2}" (names kept, values replaced). */
  function query(search) {
    const s = search.replace(/^\?/, '');
    if (!s) return '';
    return s
      .split('&')
      .filter((p) => p !== '')
      .map((p) => {
        const eq = p.indexOf('=');
        const rawName = eq < 0 ? p : p.slice(0, eq);
        let name = rawName;
        try {
          name = decodeURIComponent(rawName.replace(/\+/g, ' '));
        } catch {
          // keep raw
        }
        if (isIdentityField(name)) note('identity_field', `query parameter "${shapeName(name)}"`);
        if (/@|%40/.test(rawName)) note('email', 'query parameter name');
        if (eq < 0) return flagWord(name) ? rawName : valuePlaceholder(rawName);
        return `${segment(rawName)}=${valuePlaceholder(p.slice(eq + 1))}`;
      })
      .join('&');
  }

  /** An absolute URL. */
  function url(raw) {
    const s = String(raw ?? '');
    if (/^blob:/i.test(s)) return 'blob:';
    if (/^data:/i.test(s)) return 'data:';
    let u;
    try {
      u = new URL(s);
    } catch {
      return '(invalid)';
    }
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return `${u.protocol}`;
    if (u.username || u.password) note('identity_field', 'user name in URL');
    const mapped = mapHost(u.hostname);
    const host = u.port ? `${mapped}:${u.port}` : mapped;
    if (/\d{7,12}/.test(mapped)) note('student_id', 'host name with a long number');
    const q = query(u.search);
    return `${u.protocol}//${host}${path(u.pathname)}${q ? '?' + q : ''}`;
  }

  /** A URI reference from a manifest: absolute or relative. */
  function uriRef(raw) {
    const s = String(raw ?? '').trim();
    if (/^[a-z][a-z0-9+.-]*:/i.test(s)) return url(s);
    if (s.startsWith('//')) return url('https:' + s).replace(/^https:/, '');
    const hash = s.indexOf('#');
    const noHash = hash < 0 ? s : s.slice(0, hash);
    const qm = noHash.indexOf('?');
    const p = qm < 0 ? noHash : noHash.slice(0, qm);
    const q = qm < 0 ? '' : query(noHash.slice(qm + 1));
    return `${path(p)}${q ? '?' + q : ''}`;
  }

  function shapeName(name) {
    // Field names are short identifiers; keep them unless they are long.
    return String(name).length <= 40 ? String(name) : shape(name, 40);
  }

  return {
    findings,
    fakes,
    setWhere(w) {
      where = w;
    },
    url,
    uriRef,
    query,
    segment,
    shape,
    note,
    hls: (text) => redactHls(text, api()),
    mpd: (text) => redactMpd(text, api()),
  };

  function api() {
    return { url, uriRef, segment, note, shape, fakeHex };
  }
}

// ------------------------------------------------------------- HLS text

// Attribute values kept as they are (numbers, enumerations, codecs, group
// references); every other value is reduced to its shape.
const HLS_SAFE_ATTRS = new Set([
  'BANDWIDTH', 'AVERAGE-BANDWIDTH', 'RESOLUTION', 'CODECS', 'SUPPLEMENTAL-CODECS', 'FRAME-RATE', 'HDCP-LEVEL',
  'VIDEO-RANGE', 'ALLOWED-CPC', 'TYPE', 'GROUP-ID', 'AUDIO', 'VIDEO', 'SUBTITLES', 'CLOSED-CAPTIONS', 'DEFAULT',
  'AUTOSELECT', 'FORCED', 'LANGUAGE', 'ASSOC-LANGUAGE', 'CHANNELS', 'INSTREAM-ID', 'CHARACTERISTICS', 'METHOD',
  'KEYFORMAT', 'KEYFORMATVERSIONS', 'PROGRAM-ID', 'SCORE', 'BYTERANGE', 'DATA-ID', 'TIME-OFFSET', 'PRECISE',
  'CAN-SKIP-UNTIL', 'CAN-SKIP-DATERANGES', 'HOLD-BACK', 'PART-HOLD-BACK', 'CAN-BLOCK-RELOAD', 'PART-TARGET',
  'DURATION', 'INDEPENDENT', 'GAP', 'LAST-MSN', 'LAST-PART', 'BYTERANGE-START', 'BYTERANGE-LENGTH', 'PATHWAY-ID',
  'STABLE-VARIANT-ID', 'STABLE-RENDITION-ID', 'BIT-DEPTH', 'SAMPLE-RATE',
]);
const HLS_URI_ATTRS = new Set(['URI', 'SERVER-URI']);
const HLS_HEX_ATTRS = new Set(['IV']);
const HLS_LANG_ATTRS = new Set(['LANGUAGE', 'ASSOC-LANGUAGE']);
const SAFE_VALUE = /^[A-Za-z0-9 .,:+\-_/=x]*$/;
const HLS_ATTR_TAGS = new Set([
  '#EXT-X-STREAM-INF', '#EXT-X-I-FRAME-STREAM-INF', '#EXT-X-MEDIA', '#EXT-X-KEY', '#EXT-X-SESSION-KEY', '#EXT-X-MAP',
  '#EXT-X-SESSION-DATA', '#EXT-X-DATERANGE', '#EXT-X-PRELOAD-HINT', '#EXT-X-RENDITION-REPORT', '#EXT-X-PART',
  '#EXT-X-CONTENT-STEERING', '#EXT-X-DEFINE', '#EXT-X-SKIP', '#EXT-X-SERVER-CONTROL', '#EXT-X-PART-INF', '#EXT-X-START',
]);
const HLS_SIMPLE_TAGS = new Set([
  '#EXT-X-VERSION', '#EXT-X-TARGETDURATION', '#EXT-X-MEDIA-SEQUENCE', '#EXT-X-DISCONTINUITY-SEQUENCE',
  '#EXT-X-PLAYLIST-TYPE', '#EXT-X-ENDLIST', '#EXT-X-INDEPENDENT-SEGMENTS', '#EXT-X-DISCONTINUITY',
  '#EXT-X-BYTERANGE', '#EXT-X-I-FRAMES-ONLY', '#EXT-X-GAP', '#EXT-X-BITRATE', '#EXT-X-ALLOW-CACHE',
]);

function redactHls(text, r) {
  const out = [];
  for (const raw of playlistLines(text)) {
    const line = raw.trim();
    if (line === '') continue;
    if (line.startsWith('#')) {
      if (line === '#EXTM3U') {
        out.push(line);
        continue;
      }
      if (!line.startsWith('#EXT')) continue; // comments may say anything
      const colon = line.indexOf(':');
      const tag = colon < 0 ? line : line.slice(0, colon);
      const value = colon < 0 ? '' : line.slice(colon + 1);
      if (!/^#EXT[A-Z0-9-]*$/.test(tag)) continue;
      if (tag === '#EXTINF') {
        const comma = value.indexOf(',');
        const dur = comma < 0 ? value : value.slice(0, comma);
        const title = comma < 0 ? '' : value.slice(comma + 1);
        out.push(`#EXTINF:${/^[0-9.]+$/.test(dur) ? dur : '0'},${r.shape(title)}`);
      } else if (HLS_ATTR_TAGS.has(tag)) {
        const attrs = parseAttributeList(value);
        const parts = [];
        for (const [k, { value: v, quoted }] of attrs) {
          let nv;
          if (HLS_URI_ATTRS.has(k)) nv = r.uriRef(v);
          else if (HLS_HEX_ATTRS.has(k)) nv = /^0x[0-9a-f]+$/i.test(v) ? '0x' + r.fakeHex(v.slice(2)) : r.shape(v);
          else if (HLS_LANG_ATTRS.has(k)) nv = safeLanguage(v);
          else if (HLS_SAFE_ATTRS.has(k) && SAFE_VALUE.test(v)) nv = v;
          else {
            if (!HLS_SAFE_ATTRS.has(k) && !['NAME', 'VALUE', 'ID', 'CLASS'].includes(k) && isIdentityField(k)) {
              r.note('identity_field', `manifest attribute "${k}"`);
            }
            nv = r.shape(v);
          }
          parts.push(`${k}=${quoted ? `"${nv}"` : nv}`);
        }
        out.push(`${tag}:${parts.join(',')}`);
      } else if (HLS_SIMPLE_TAGS.has(tag)) {
        out.push(colon < 0 ? tag : `${tag}:${/^[A-Za-z0-9.@,-]*$/.test(value) ? value : r.shape(value)}`);
      } else {
        // Unknown or free-text tags (#EXT-X-PROGRAM-DATE-TIME, vendor tags):
        // keep the tag, reduce the value to its shape.
        out.push(colon < 0 ? tag : `${tag}:${r.shape(value)}`);
      }
      continue;
    }
    out.push(r.uriRef(line));
  }
  return out.join('\n') + '\n';
}

// ------------------------------------------------------------- DASH text

const MPD_URL_ATTRS = new Set(['media', 'initialization', 'sourceURL', 'href', 'index', 'bitstreamSwitching']);
const MPD_SAFE_ATTRS = new Set([
  'profiles', 'type', 'minBufferTime', 'mediaPresentationDuration', 'maxSegmentDuration', 'minimumUpdatePeriod',
  'timeShiftBufferDepth', 'suggestedPresentationDelay', 'start', 'duration', 'contentType', 'mimeType', 'codecs',
  'lang', 'segmentAlignment', 'subsegmentAlignment', 'subsegmentStartsWithSAP', 'startWithSAP',
  'width', 'height', 'frameRate', 'sar', 'par', 'scanType', 'bandwidth', 'audioSamplingRate', 'maxWidth', 'maxHeight',
  'minWidth', 'minHeight', 'maxBandwidth', 'minBandwidth', 'maxFrameRate', 'timescale', 'startNumber', 'endNumber',
  'presentationTimeOffset', 'indexRange', 'indexRangeExact', 'range', 'mediaRange', 't', 'd', 'r', 'n', 'k',
  'schemeIdUri', 'group', 'xmlns', 'xmlns:xsi', 'xmlns:cenc', 'xmlns:xlink', 'xmlns:mspr', 'xmlns:dvb',
  'xmlns:scte35', 'xmlns:dashif', 'xmlns:ms', 'xsi:schemaLocation', 'availabilityTimeOffset',
  'availabilityTimeComplete', 'contentComponent', 'audioChannelConfiguration', 'maximumSAPPeriod', 'qualityRanking',
  'dependencyId', 'codingDependency', 'selectionPriority',
]);
const MPD_ID_ATTRS = new Set(['id', 'cenc:default_KID', 'default_KID', 'serviceLocation', 'dvb:priority', 'dvb:weight']);
const MPD_URL_TEXT = new Set(['BaseURL', 'Location', 'PatchLocation', 'Laurl', 'laurl']);
const MPD_OPAQUE_TEXT = new Set(['pssh', 'pro', 'IsEncrypted', 'IV_size', 'KID']);
const MPD_SAFE_VALUE = /^[A-Za-z0-9 .,:+\-_/=#$%]*$/;

/**
 * A descriptor's value attribute (Role, Accessibility, AudioChannel-
 * Configuration, SupplementalProperty…; security review L6): short
 * enumerations and numbers stay ("main", "2", "F801"); an address is
 * redacted as one; anything longer or ID-like becomes its shape — a
 * vendor's property may carry a token or a person's id.
 */
function mpdValue(v, r) {
  const s = String(v);
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(s)) return r.uriRef(s);
  if (/^[A-Za-z0-9._:-]{0,24}$/.test(s) && !/\d{5,}|[0-9a-fA-F]{16,}/.test(s)) return s;
  return r.shape(s);
}

function redactMpd(text, r) {
  let root;
  try {
    root = parseXml(text);
  } catch {
    return '';
  }
  const walk = (el) => {
    const attrs = [];
    for (const [k, v] of el.attrs) {
      const local = localName(k);
      let nv;
      if (MPD_URL_ATTRS.has(local)) nv = r.uriRef(v);
      else if (local === 'value') nv = mpdValue(v, r);
      else if (local === 'lang') nv = safeLanguage(v);
      else if (MPD_ID_ATTRS.has(k) || MPD_ID_ATTRS.has(local)) nv = r.segment(v);
      else if ((MPD_SAFE_ATTRS.has(k) || MPD_SAFE_ATTRS.has(local)) && MPD_SAFE_VALUE.test(v)) nv = v;
      else if (/^(availabilityStartTime|publishTime|availabilityEndTime)$/.test(local)) nv = r.shape(v);
      else {
        if (isIdentityField(local)) r.note('identity_field', `manifest attribute "${local}"`);
        nv = r.shape(v);
      }
      attrs.push([k, nv]);
    }
    const local = localName(el.name);
    const children = [];
    for (const c of el.children) {
      if (c.name) children.push(walk(c));
      else if (c.text !== undefined && c.text.trim() !== '') {
        if (MPD_URL_TEXT.has(local)) children.push({ text: r.uriRef(c.text.trim()) });
        else if (MPD_OPAQUE_TEXT.has(local)) children.push({ text: '{opaque}' });
        else children.push({ text: r.shape(c.text) });
      }
    }
    return { name: el.name, attrs, children };
  };
  return '<?xml version="1.0" encoding="UTF-8"?>\n' + serializeXml(walk(root)) + '\n';
}
