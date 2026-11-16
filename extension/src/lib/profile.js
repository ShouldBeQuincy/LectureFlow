// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The platform profile engine (SPEC-ext 平台对策文件): matching a page,
// the session id, the lecture fields and the download switch read through
// CSS selectors, and naming the generic tracks. A profile is data only:
// selectors read text or an attribute, patterns are regular expressions
// applied to what was read, and tracks[].detect only filters and renames
// tracks generic detection already found — or, with fromResponse, takes
// URLs the page's own response named (`observe`, src/lib/observe.js); the
// extension requests nothing for them. Pure.

import { parseHttpUrl, hostMatches, hostPermissionPattern, withoutQuery, genericSessionId, urlKey } from './url.js';
import { validate } from './jsonschema.js';
import { normalizeRecordedAt, parseDurationSec } from './dates.js';
import { parsePointer, observeIndexOf, isEmptyObserved, profileKey } from './observe.js';
import { classifyUrl, C_HLS, C_DASH, FORMAT_FILE, FORMAT_HLS, FORMAT_DASH } from './classify.js';
import { wildcardCategory, pathEntriesFor, pathPatternMayReach, normalizePath, pathCovered, isBlockedUrl } from './blocklist.js';
import { MAX_REGEX_INPUT, regexComplexity, captureGroupCount } from './regexcost.js';
import { guardedRegex, regexTrip } from './regexguard.js';

export const GENERIC = 'generic';

// MAX_REGEX_INPUT and regexComplexity live in ./regexcost.js (no imports,
// so src/lib/blocklist.js can use them too); exported here as before.
export { MAX_REGEX_INPUT, regexComplexity };

/**
 * Compiles a profile regex: JavaScript syntax, case-insensitive. Returns
 * { source, test(s), exec(s) } that never runs on more than
 * MAX_REGEX_INPUT characters, behind the runtime guard (./regexguard.js):
 * owner is the profile's key (profileKey), so that a match too slow stops
 * every regex of that profile. Every match of a profile regex goes through
 * here (test/unit/regexguard.test.mjs scans the sources for others).
 */
export function profileRegex(source, owner = '') {
  return guardedRegex(source, { owner });
}

/** The owner key of a profile's regexes (profileKey), '' without one. */
export function regexOwner(profile) {
  return profile && profile.id ? profileKey(profile) : '';
}

/** Whether the runtime guard stopped this profile's regexes in this context. */
export function profileStopped(profile) {
  const owner = regexOwner(profile);
  return !!owner && !!regexTrip(owner);
}

/**
 * Every regex of a profile with its place in the file: [[path, source]],
 * the places checkProfile names and the slow-profile records point to.
 */
export function profileRegexes(profile) {
  const regexes = [];
  if (!profile || typeof profile !== 'object') return regexes;
  if (profile.match?.pathPattern !== undefined) regexes.push(['/match/pathPattern', profile.match.pathPattern]);
  const idFrom = profile.session?.idFrom || {};
  if (idFrom.pathPattern !== undefined) regexes.push(['/session/idFrom/pathPattern', idFrom.pathPattern]);
  if (idFrom.pattern !== undefined) regexes.push(['/session/idFrom/pattern', idFrom.pattern]);
  for (const [field, specs] of Object.entries(profile.lecture || {})) {
    asList(specs).forEach((s, i) => s && s.pattern !== undefined && regexes.push([`/lecture/${field}/${i}/pattern`, s.pattern]));
  }
  asList(profile.downloadPolicy?.disabledWhen).forEach((s, i) => s && s.pattern !== undefined && regexes.push([`/downloadPolicy/disabledWhen/${i}/pattern`, s.pattern]));
  (profile.tracks || []).forEach((t, i) => t?.detect?.urlPattern !== undefined && regexes.push([`/tracks/${i}/detect/urlPattern`, t.detect.urlPattern]));
  (profile.observe || []).forEach((o, i) => o && regexes.push([`/observe/${i}/urlPattern`, o.urlPattern]));
  const list = profile.inpage?.list;
  if (list) {
    regexes.push(['/inpage/list/pathPattern', list.pathPattern]);
    if (list.idPattern !== undefined) regexes.push(['/inpage/list/idPattern', list.idPattern]);
  }
  return regexes;
}

// Second-level labels registries put under a country code ("co.uk",
// "com.au", "ac.jp", "edu.cn"…) and shared hosting suffixes where every
// subdomain is someone else's site. A wildcard on one of these would cover
// a whole registry or every customer of a host (security review L7). Not a
// public suffix list: the common cases, written down.
const COUNTRY_SECOND_LEVEL = new Set(['ac', 'co', 'com', 'edu', 'gov', 'net', 'org', 'or', 'ne', 'go', 'gob', 'gen', 'mil', 'nic', 'sch', 'ltd', 'plc', 'nom', 'info', 'biz', 'int', 'k12', 'res', 'gouv', 'govt', 'school', 'uni']);
export const SHARED_SUFFIXES = Object.freeze([
  'github.io', 'gitlab.io', 'bitbucket.io', 'pages.dev', 'workers.dev', 'netlify.app', 'vercel.app', 'web.app', 'firebaseapp.com',
  'appspot.com', 'blogspot.com', 'herokuapp.com', 'onrender.com', 'fly.dev', 'glitch.me', 'repl.co', 'ngrok.io', 'ngrok-free.app',
  'azurewebsites.net', 'azureedge.net', 'blob.core.windows.net', 'cloudapp.net', 'trafficmanager.net',
  'cloudfront.net', 'amazonaws.com', 's3.amazonaws.com', 'elasticbeanstalk.com', 'r2.dev', 'r2.cloudflarestorage.com',
  'googleusercontent.com', 'storage.googleapis.com', 'googleapis.com', 'akamaized.net', 'akamaihd.net', 'edgesuite.net', 'edgekey.net',
  'fastly.net', 'global.ssl.fastly.net', 'b-cdn.net', 'cdn77.org', 'wpengine.com', 'myshopify.com', 'sharepoint.com', 'dropboxusercontent.com',
]);

/**
 * Why a host wildcard is too wide, or '' ("%s" stands for the pattern): "*."
 * followed by a country code's second level ("*.co.uk", "*.edu.au" — a
 * whole public suffix) or by a shared hosting suffix ("*.github.io",
 * "*.cloudfront.net", "*.s3.us-east-1.amazonaws.com"). One label after "*."
 * ("*.com") the schema refuses already.
 */
export function wideWildcard(pattern) {
  const p = String(pattern || '').toLowerCase();
  if (!p.startsWith('*.')) return '';
  const base = p.slice(2);
  const labels = base.split('.');
  if (labels.length === 2 && /^[a-z]{2}$/.test(labels[1]) && COUNTRY_SECOND_LEVEL.has(labels[0])) return '%s covers a whole public suffix (a country registry)';
  if (SHARED_SUFFIXES.includes(base) || /^s3[.-][a-z0-9-]+\.amazonaws\.com$/.test(base) || /^[a-z0-9-]+\.(?:compute|elb)\.amazonaws\.com$/.test(base)) return `%s is a shared service (${base}), not one platform`;
  return '';
}

const BLOCKED_NAMES = { youtube: 'YouTube', courses: 'an online course platform (MOOC or paid courses)' };
const blockedName = (category) => BLOCKED_NAMES[category] || BLOCKED_NAMES.courses;

/**
 * What a profile's match covers of the do-not-handle list
 * (src/lib/blocklist.js): a host or media host on a site blocked as a
 * whole, or page hosts with a blocked path (linkedin.com/learning,
 * bilibili.com/cheese) whose match.pathPattern, inpage.list.pathPattern
 * or sessionInfo.path could reach that path. Runs on whatever the file
 * holds, before the schema, so the reason is said plainly. Returns
 * [{ path, message, blocked: { host, category } }].
 */
export function blockedMatchErrors(profile) {
  const out = [];
  const m = profile && typeof profile === 'object' && profile.match && typeof profile.match === 'object' ? profile.match : null;
  if (!m) return out;
  const strings = (v) => (Array.isArray(v) ? v : []).map((x, i) => [x, i]).filter(([x]) => typeof x === 'string');
  for (const field of ['hosts', 'mediaHosts']) {
    for (const [h, i] of strings(m[field])) {
      const c = wildcardCategory(h);
      if (c) out.push({ path: `/match/${field}/${i}`, message: `"${h}" covers ${blockedName(c)}, which LectureFlow does not handle (extension/src/lib/blocklist.js)`, blocked: { host: h, category: c } });
    }
  }
  const list = profile.inpage && typeof profile.inpage === 'object' && profile.inpage.list && typeof profile.inpage.list === 'object' ? profile.inpage.list : null;
  const info = profile.sessionInfo && typeof profile.sessionInfo === 'object' ? profile.sessionInfo : null;
  const seen = new Set();
  for (const [h] of strings(m.hosts)) {
    for (const e of pathEntriesFor(h)) {
      const where = `${e.host}${e.path}`;
      if (seen.has(where)) continue;
      seen.add(where);
      const blocked = { host: where, category: e.category };
      const said = `"${h}" covers ${where} (${blockedName(e.category)}), which LectureFlow does not handle`;
      const rule = `start with "^/" and a fixed first path segment other than "${e.path.slice(1)}", without a top-level "|"`;
      if (typeof m.pathPattern !== 'string') out.push({ path: '/match/pathPattern', message: `${said}: add a pathPattern that keeps out of ${e.path} (${rule})`, blocked });
      else if (pathPatternMayReach(m.pathPattern, e.path)) out.push({ path: '/match/pathPattern', message: `${said}: the pathPattern must keep out of ${e.path} (${rule})`, blocked });
      if (list && typeof list.pathPattern === 'string' && pathPatternMayReach(list.pathPattern, e.path)) {
        out.push({ path: '/inpage/list/pathPattern', message: `${said}: the list pathPattern must keep out of ${e.path} (${rule})`, blocked });
      }
      if (info && typeof info.path === 'string' && pathCovered(normalizePath(info.path.split('?')[0]), e.path)) {
        out.push({ path: '/sessionInfo/path', message: `${said}: sessionInfo may not request a page under ${e.path}`, blocked });
      }
    }
  }
  return out;
}

/**
 * Schema validation plus what a schema cannot say: every regex compiles,
 * an idFrom.pathPattern has exactly one capture group, the id is not
 * "generic", observed names are declared once and used only where
 * declared, supersedes names other roles, nothing of the do-not-handle
 * list is matched (blockedMatchErrors, reported first; such an error
 * carries `blocked`). Returns a list of { path, message[, blocked] }.
 * `unreviewed`: a profile the user imports (not one packaged after review)
 * may make its sessionInfo request only as a GET (FB1 L-2): the extension's
 * import and its loading of local profiles pass it; the contribution check
 * does not, the profile being reviewed in its pull request.
 */
export function checkProfile(schema, profile, { unreviewed = false } = {}) {
  const blocked = blockedMatchErrors(profile);
  const schemaErrors = validate(schema, profile);
  // the schema's own refusal of the same hosts says less
  if (schemaErrors.length) return [...blocked, ...schemaErrors.filter((e) => !(e.message === 'must not match' && blocked.some((b) => b.path === e.path)))];
  const errors = [...blocked];
  const regexes = profileRegexes(profile);
  const idFrom = profile.session.idFrom;
  const list = profile.inpage?.list;
  // Every regex is checked statically first; one that does not compile or
  // is too costly is never run below (SEC18 中-2).
  const unsafe = new Set();
  for (const [path, src] of regexes) {
    try {
      new RegExp(src, 'i'); // compiles only
    } catch (e) {
      errors.push({ path, message: `invalid regular expression: ${e.message}` });
      unsafe.add(path);
      continue;
    }
    const costly = regexComplexity(src);
    if (costly) {
      errors.push({ path, message: costly });
      unsafe.add(path);
    }
  }
  if (idFrom.pathPattern !== undefined && errors.length === 0) {
    if (captureGroupCount(idFrom.pathPattern) !== 1) errors.push({ path: '/session/idFrom/pathPattern', message: 'needs exactly one capture group' });
  }
  const roles = profile.tracks.map((t) => t.role);
  roles.forEach((r, i) => roles.indexOf(r) !== i && errors.push({ path: `/tracks/${i}/role`, message: 'duplicate role' }));
  // observe: pointer syntax, each name declared once and used where declared
  const declared = new Set();
  (profile.observe || []).forEach((o, i) => {
    for (const [name, pointer] of Object.entries(o.pointers)) {
      if (!parsePointer(pointer)) errors.push({ path: `/observe/${i}/pointers/${name}`, message: 'not a JSON pointer (a "*" segment takes every array element; the root is not allowed)' });
      if (declared.has(name)) errors.push({ path: `/observe/${i}/pointers/${name}`, message: 'name declared twice' });
      declared.add(name);
    }
  });
  const undeclared = (path, name) => !declared.has(name) && errors.push({ path, message: `"${name}" is not declared in observe` });
  profile.tracks.forEach((t, i) => {
    for (const name of asList(t.detect.fromResponse)) undeclared(`/tracks/${i}/detect/fromResponse`, name);
    if (t.detect.responseWhen) {
      undeclared(`/tracks/${i}/detect/responseWhen/observed`, t.detect.responseWhen.observed);
      if (t.detect.fromResponse === undefined) errors.push({ path: `/tracks/${i}/detect/responseWhen`, message: 'only with fromResponse' });
    }
    for (const r of t.supersedes || []) {
      if (r === t.role || !roles.includes(r)) errors.push({ path: `/tracks/${i}/supersedes`, message: `"${r}" is not another role of this profile` });
    }
  });
  for (const [field, specs] of Object.entries(profile.lecture || {})) {
    asList(specs).forEach((s, i) => s.observed !== undefined && undeclared(`/lecture/${field}/${i}/observed`, s.observed));
  }
  asList(profile.downloadPolicy?.disabledWhen).forEach((s, i) => {
    if (s.observed === undefined) return;
    undeclared(`/downloadPolicy/disabledWhen/${i}/observed`, s.observed);
    if ((s.empty === true) === (s.pattern !== undefined)) errors.push({ path: `/downloadPolicy/disabledWhen/${i}`, message: 'needs exactly one of "empty" or "pattern"' });
  });
  // sessionInfo: the request of an observe entry, to the page's own origin
  const info = profile.sessionInfo;
  if (info) {
    if (unreviewed && info.method !== 'GET') errors.push({ path: '/sessionInfo/method', message: `an unreviewed profile may request sessionInfo only with GET, not ${info.method} (a request with a body needs review)` });
    const obs = (profile.observe || [])[info.observe];
    if (!obs) errors.push({ path: '/sessionInfo/observe', message: 'no such observe entry' });
    else if (!unsafe.has(`/observe/${info.observe}/urlPattern`)) {
      try {
        if (!profileRegex(obs.urlPattern).test(`https://example.invalid${info.path}`)) errors.push({ path: '/sessionInfo/path', message: 'does not match the urlPattern of its observe entry' });
      } catch {
        // reported above
      }
    }
  }
  if (list) {
    if (!info) errors.push({ path: '/inpage/list', message: 'a list needs sessionInfo (the list page does not load the sessions)' });
  }
  for (const k of ['full', 'audio']) for (const r of profile.presets?.[k] || []) if (!roles.includes(r)) errors.push({ path: `/presets/${k}`, message: `"${r}" is not a role of this profile` });
  // the same words as the repository's contribution check (scripts/profilecheck)
  for (const field of ['hosts', 'mediaHosts']) {
    (profile.match[field] || []).forEach((h, i) => {
      const wide = wideWildcard(h);
      if (wide) errors.push({ path: `/match/${field}/${i}`, message: `${wide.replace('%s', JSON.stringify(h))}; name the platform's own domains` });
    });
  }
  return errors;
}

export function asList(specs) {
  if (specs === undefined || specs === null) return [];
  return Array.isArray(specs) ? specs : [specs];
}

/** Whether a page URL is one the profile covers. */
export function profileMatchesUrl(profile, rawUrl) {
  const u = parseHttpUrl(rawUrl);
  if (!u) return false;
  if (!profile.match.hosts.some((h) => hostMatches(h, u.hostname))) return false;
  if (profile.match.pathPattern !== undefined && !profileRegex(profile.match.pathPattern, regexOwner(profile)).test(u.pathname)) return false;
  return true;
}

/**
 * The profile for a page. `profiles` are { profile, reviewed } in priority
 * order (local imports before bundled ones, so a contributor can try a new
 * version). The top-level page is tried first, then the frames (a viewer
 * embedded in another site). Returns { profile, reviewed, url } or null.
 */
export function findProfile(profiles, pageUrl, frameUrls = []) {
  for (const url of [pageUrl, ...frameUrls]) {
    for (const p of profiles) {
      try {
        if (profileMatchesUrl(p.profile, url)) return { profile: p.profile, reviewed: p.reviewed, url };
      } catch {
        // a broken local profile never stops detection
      }
    }
  }
  return null;
}

/**
 * The host permission patterns of a profile: its page hosts, plus its media
 * hosts unless media is false (they matter only for reading cookies, which
 * the user is asked about host by host).
 */
export function profilePermissionOrigins(profile, { media = true } = {}) {
  const hosts = [...profile.match.hosts, ...(media ? profile.match.mediaHosts || [] : [])];
  return [...new Set(hosts.map(hostPermissionPattern))];
}

/**
 * Selector reads the page scan must perform for this profile, as plain
 * data passed into the injected function: [{ key, selector, attr }].
 * Patterns are applied afterwards here, not in the page.
 */
export function selectorReads(profile) {
  const reads = [];
  const add = (key, spec) => {
    if (spec && spec.selector) reads.push({ key, selector: spec.selector, attr: spec.attr || '' });
  };
  if (!profile) return reads;
  add('session.idFrom', profile.session.idFrom.selector ? profile.session.idFrom : null);
  for (const [field, specs] of Object.entries(profile.lecture || {})) asList(specs).forEach((s, i) => add(`lecture.${field}.${i}`, s));
  asList(profile.downloadPolicy?.disabledWhen).forEach((s, i) => add(`downloadPolicy.disabledWhen.${i}`, s));
  return reads;
}

/** Collapses whitespace runs and trims. */
export function cleanText(s) {
  return String(s ?? '').replace(/\s+/g, ' ').trim();
}

function applyPattern(value, pattern, owner) {
  const v = cleanText(value);
  if (!v) return '';
  if (pattern === undefined) return v;
  const m = profileRegex(pattern, owner).exec(v);
  if (!m) return '';
  return cleanText(m.length > 1 ? m[1] ?? '' : m[0]);
}

/**
 * First non-empty value of a field from the selector results or the
 * observed values. `results` maps a read key to { count, value } (value:
 * text or attribute of the first matching element, "" when none);
 * `observed` is combineObserved()'s.
 */
function readField(results, keyPrefix, specs, observed, owner) {
  const list = asList(specs);
  for (let i = 0; i < list.length; i++) {
    let raw;
    if (list[i].observed !== undefined) {
      const v = observed ? observed.values[list[i].observed] : undefined;
      raw = Array.isArray(v) ? v.find((x) => x !== null && x !== '') : v;
      if (raw === undefined || raw === null || typeof raw === 'boolean') continue;
    } else {
      const r = results[`${keyPrefix}.${i}`];
      if (!r || !r.count) continue;
      raw = r.value;
    }
    const v = applyPattern(String(raw), list[i].pattern, owner);
    if (v) return v;
  }
  return '';
}

/**
 * Session id: the profile's idFrom, or (no profile, or nothing found) the
 * generic hash of the page address.
 */
export async function sessionIdFor(match, pageUrl, results = {}) {
  if (match) {
    const idFrom = match.profile.session.idFrom;
    const owner = regexOwner(match.profile);
    const u = parseHttpUrl(match.url);
    let id = '';
    if (idFrom.query !== undefined && u) id = u.searchParams.get(idFrom.query) || '';
    else if (idFrom.pathPattern !== undefined && u) {
      const m = profileRegex(idFrom.pathPattern, owner).exec(u.pathname);
      id = m && m[1] ? m[1] : '';
    } else if (idFrom.selector !== undefined) {
      const r = results['session.idFrom'];
      id = r && r.count ? applyPattern(r.value, idFrom.pattern, owner) : '';
    }
    id = cleanText(id);
    if (id && id.length <= 200) return { sessionId: id, source: 'profile' };
  }
  return { sessionId: await genericSessionId(pageUrl), source: GENERIC };
}

// Titles that name no lecture (compared case-insensitively, whole title).
const PLACEHOLDER_TITLES = new Set(['recording', 'video', 'audio', 'untitled', 'media', 'player']);

/** Whether a title is empty or only a placeholder such as "Recording". */
export function isPlaceholderTitle(s) {
  const v = cleanText(s).toLowerCase();
  return !v || PLACEHOLDER_TITLES.has(v);
}

// The site's own name as a label of its host: "archive" for archive.org,
// "example" for www.example.co.uk ('' for an address or a bare name).
function siteLabel(pageUrl) {
  let host = '';
  try {
    host = new URL(pageUrl).hostname.toLowerCase();
  } catch {
    return '';
  }
  if (!host || /^[0-9.]+$/.test(host) || host.includes(':')) return '';
  const labels = host.split('.').filter(Boolean);
  if (labels.length < 2) return '';
  labels.pop();
  if (labels.length > 1 && /^(ac|co|com|edu|gov|net|org)$/.test(labels[labels.length - 1])) labels.pop();
  const label = labels[labels.length - 1];
  return label.length >= 3 ? label : '';
}

/**
 * The page's own title (document.title) as a lecture title, for generic
 * detection: trailing parts that name the site ("… | Example",
 * "… : Internet Archive" on archive.org) are dropped; '' when what is left
 * is only the site's name or a placeholder.
 */
export function pageTitleFor(title, pageUrl = '') {
  let v = cleanText(title);
  const label = siteLabel(pageUrl);
  const flat = (s) => s.toLowerCase().replace(/[^\p{L}\p{N}]+/gu, '');
  if (label) {
    for (;;) {
      const m = /^(.*\S)\s+[|:\-–—·•]\s+(.+)$/u.exec(v);
      if (!m || !flat(m[2]).includes(label)) break;
      v = m[1];
    }
    if (flat(v) === label) v = '';
  }
  return isPlaceholderTitle(v) ? '' : v.slice(0, 300);
}

/**
 * Lecture fields (title, recordedAt, durationSec). Only these three exist;
 * the schema rules out anything else. Duration falls back to the media
 * element's (pageDurationSec) when the profile does not read one. Without
 * a profile the title is the page's own (page: { title, url }, see
 * pageTitleFor); a profile reads its own title selectors only.
 */
export function lectureFor(match, results = {}, pageDurationSec = 0, observed = null, page = null) {
  const lecture = { title: '', recordedAt: '', durationSec: 0 };
  const spec = match?.profile.lecture || {};
  const owner = regexOwner(match?.profile);
  if (spec.title) lecture.title = readField(results, 'lecture.title', spec.title, observed, owner).slice(0, 300);
  if (!match && page) lecture.title = pageTitleFor(page.title, page.url);
  if (spec.recordedAt) lecture.recordedAt = normalizeRecordedAt(readField(results, 'lecture.recordedAt', spec.recordedAt, observed, owner));
  if (spec.durationSec) lecture.durationSec = parseDurationSec(readField(results, 'lecture.durationSec', spec.durationSec, observed, owner));
  if (!lecture.durationSec && pageDurationSec > 0 && Number.isFinite(pageDurationSec)) lecture.durationSec = Math.round(pageDurationSec);
  return lecture;
}

/**
 * Download switch: "disabled" when a disabledWhen condition holds;
 * "enabled" when at least one could be checked and none holds; "unknown"
 * when the profile cannot tell (SPEC-ext: 只在能读到时提示). A selector
 * condition can always be checked (the scan reports a miss as count 0); an
 * observed condition only once the page's response was seen (`observed`
 * from combineObserved).
 */
export function downloadPolicyFor(match, results = {}, observed = null) {
  const policy = readDownloadPolicy(match, results, observed);
  // A stopped regex answers "no match", which would read as "not
  // restricted": when the guard stopped the profile, nothing can be said
  // (SEC18e 低-15; the card says why: policyStopped).
  return match && profileStopped(match.profile) ? 'unknown' : policy;
}

function readDownloadPolicy(match, results, observed) {
  const specs = asList(match?.profile.downloadPolicy?.disabledWhen);
  if (specs.length === 0) return 'unknown';
  const owner = regexOwner(match?.profile);
  let checked = false;
  let unseen = false;
  for (let i = 0; i < specs.length; i++) {
    const spec = specs[i];
    if (spec.observed !== undefined) {
      const idx = observeIndexOf(match.profile, spec.observed);
      if (!observed || idx < 0 || !observed.seen.has(idx)) {
        unseen = true;
        continue;
      }
      checked = true;
      const v = observed.values[spec.observed];
      if (spec.empty === true) {
        if (isEmptyObserved(v)) return 'disabled';
      } else if (spec.pattern !== undefined && !isEmptyObserved(v)) {
        const first = Array.isArray(v) ? v.find((x) => !isEmptyObserved(x)) : v;
        if (applyPattern(String(first), spec.pattern, owner)) return 'disabled';
      }
      continue;
    }
    checked = true;
    const r = results[`downloadPolicy.disabledWhen.${i}`];
    if (!r || !r.count) continue;
    if (spec.pattern === undefined || applyPattern(r.value, spec.pattern, owner)) return 'disabled';
  }
  // the page's observer was stopped as too slow before it saw a response
  // a condition needs: the selector conditions alone cannot say "not
  // restricted" (SEC22 I-4)
  if (checked && unseen && observed && observed.stopped) return 'unknown';
  return checked ? 'enabled' : 'unknown';
}

/**
 * The label of a rule in a UI language, with fallbacks: the language
 * itself; for Cantonese and Hong Kong / Macao Chinese, as in LectureFlow
 * (web/i18n.js), yue-Hant-HK, then Traditional Chinese, then English;
 * otherwise any label of the same language, then English.
 */
export function localizedLabel(label, locale) {
  const keys = Object.keys(label || {});
  if (keys.length === 0) return '';
  const want = String(locale || 'en').replace(/_/g, '-');
  const lower = want.toLowerCase();
  const find = (tag) => keys.find((k) => k.toLowerCase() === tag.toLowerCase());
  const exact = find(want);
  if (exact) return label[exact];
  if (lower.startsWith('yue') || lower === 'zh-hk' || lower === 'zh-mo' || lower.startsWith('zh-hant-hk') || lower.startsWith('zh-hant-mo')) {
    for (const tag of ['yue-Hant-HK', 'zh-HK', 'zh-TW', 'zh-Hant']) if (find(tag)) return label[find(tag)];
    return label.en || label[keys[0]];
  }
  const lang = lower.split('-')[0];
  const sameLang = keys.find((k) => k.split('-')[0].toLowerCase() === lang);
  if (sameLang) return label[sameLang];
  if (label.en) return label.en;
  return label[keys[0]];
}

/** Format of a URL a response named: by its extension, a file otherwise. */
function formatOfUrl(raw) {
  const { cls } = classifyUrl(raw, {});
  return cls === C_HLS ? FORMAT_HLS : cls === C_DASH ? FORMAT_DASH : FORMAT_FILE;
}

/** Whether a rule's responseWhen holds (no condition: yes; unseen value: no). */
function responseWhenHolds(rule, observed) {
  const w = rule.detect.responseWhen;
  if (!w) return true;
  const v = observed.values[w.observed];
  return (Array.isArray(v) ? v[0] : v) === w.equals;
}

/**
 * The URLs a rule's fromResponse names: http(s) values of its names
 * (relative ones resolved against the request they came from), each path
 * once, filtered by its urlPattern and format.
 */
function responseUrls(rule, observed, owner) {
  const out = [];
  if (!observed || rule.detect.fromResponse === undefined || !responseWhenHolds(rule, observed)) return out;
  const seen = new Set();
  const re = rule.detect.urlPattern !== undefined ? profileRegex(rule.detect.urlPattern, owner) : null;
  for (const name of asList(rule.detect.fromResponse)) {
    const v = observed.values[name];
    for (const raw of Array.isArray(v) ? v : [v]) {
      if (typeof raw !== 'string' || !raw.trim()) continue;
      const u = parseHttpUrl(raw.trim(), observed.urls[name]);
      if (!u || isBlockedUrl(u.href)) continue;
      const bare = withoutQuery(u.href);
      if (seen.has(bare)) continue;
      if (re && !re.test(bare)) continue;
      const format = formatOfUrl(u.href);
      if (rule.detect.format && format !== rule.detect.format) continue;
      seen.add(bare);
      out.push({ url: urlKey(u.href), bare, format });
    }
  }
  return out;
}

/**
 * Names tracks with the profile's rules. A rule takes the generic tracks
 * whose URL without query matches detect.urlPattern (and kind and format,
 * when given), in detection order, and the URLs its fromResponse names in
 * the page's own response (`observed`, from combineObserved): a named URL
 * whose path generic detection found is that track; otherwise it becomes a
 * track of the rule's kind with an unknown size (the extension requests
 * nothing for it). A track goes to the first rule that wants it. When a
 * rule takes several tracks they are numbered ("Screen capture 1",
 * "Screen capture 2"). A recommended track of a rule with `supersedes`
 * takes the recommendation away from those roles (Panopto: the full MP4
 * instead of audio and screen).
 * Returns { tracks: [...named, in rule order], others: [...unmatched] };
 * a named track carries role, label, hint, labelIndex and recommended.
 * opts.frameIds: the frames a track only the response named belongs to
 * (the frame whose page observed it: the one the profile matched; [0], the
 * top frame, by default).
 */
// The most addresses one track rule tests in one call: far above a real
// page's tracks (the request log keeps 300), a ceiling on the work.
export const MAX_TESTS_PER_RULE = 256;

export function applyProfile(profile, tracks, locale = 'en', observed = null, { frameIds = [0] } = {}) {
  const taken = new Set();
  const named = [];
  const owner = regexOwner(profile);
  for (const rule of profile.tracks) {
    const hits = [];
    if (rule.detect.urlPattern !== undefined) {
      const re = profileRegex(rule.detect.urlPattern, owner);
      let tested = 0;
      for (const t of tracks) {
        if (taken.has(t.id)) continue;
        if (rule.detect.kind && t.kind !== rule.detect.kind) continue;
        if (rule.detect.format && t.format !== rule.detect.format) continue;
        const bare = withoutQuery(t.url);
        if (bare === null) continue;
        // at most MAX_TESTS_PER_RULE addresses per rule (SEC18d)
        if (++tested > MAX_TESTS_PER_RULE) break;
        if (re.test(bare)) hits.push(t);
      }
    }
    for (const r of responseUrls(rule, observed, owner)) {
      const mine = hits.find((h) => !h.selector && withoutQuery(h.url) === r.bare);
      if (mine) {
        const i = hits.indexOf(mine);
        hits[i] = { ...mine, sources: mine.sources.includes('response') ? mine.sources : [...mine.sources, 'response'] };
        continue;
      }
      const same = tracks.find((t) => !taken.has(t.id) && !t.selector && withoutQuery(t.url) === r.bare && !hits.includes(t));
      // generic detection read that stream: trust its kind over the rule's
      if (same && same.kind !== 'unknown' && rule.detect.kind && same.kind !== rule.detect.kind) continue;
      const generic = same;
      if (generic) {
        hits.push({ ...generic, kind: generic.kind === 'unknown' ? rule.detect.kind : generic.kind, sources: [...generic.sources, 'response'] });
        continue;
      }
      hits.push({ id: r.url, url: r.url, kind: rule.detect.kind, format: r.format, sizeBytes: -1, sizeEstimated: false, sources: ['response'], frameIds: [...frameIds], protected: false });
    }
    const base = localizedLabel(rule.label, locale);
    const hint = rule.hint ? localizedLabel(rule.hint, locale) : '';
    hits.forEach((t, i) => {
      taken.add(t.id);
      named.push({
        ...t,
        role: rule.role,
        label: hits.length > 1 ? `${base} ${i + 1}` : base,
        hint,
        labelIndex: hits.length > 1 ? i + 1 : 0,
        // One enqueue takes one audio track and at most two pictures (双机位
        // is a choice, never the default), so a rule recommends only its
        // first track.
        recommended: !!rule.recommended && i === 0 && !t.protected,
      });
    });
  }
  for (const rule of profile.tracks) {
    if (!rule.supersedes || !named.some((t) => t.role === rule.role && t.recommended)) continue;
    for (const t of named) if (rule.supersedes.includes(t.role)) t.recommended = false;
  }
  return { tracks: named, others: tracks.filter((t) => !taken.has(t.id)) };
}

/**
 * Selector results of a scan, by read key: the first frame (top first)
 * whose selector matched wins. Returns { key: { count, value } }.
 */
export function readResults(frames) {
  const out = {};
  const ordered = [...(frames || [])].sort((a, b) => (a.frameId === 0 ? -1 : b.frameId === 0 ? 1 : a.frameId - b.frameId));
  for (const f of ordered) {
    for (const r of f.reads || []) {
      if (out[r.key] && out[r.key].count > 0) continue;
      out[r.key] = { count: r.count, value: r.value || '' };
    }
  }
  return out;
}
