// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// In-page entry points (user request 2026-10-27, relayed by lead; NOTES
// "ext M2 Chrome MVP"): a "发送至 LectureFlow" button on a platform's
// player, and on its folder list one per session row, a check box per row,
// a button for the whole page and a bar for the selection. Only on sites
// the user authorized and only where a profile's `inpage` says so; the
// toolbar popup stays as it is.
//
// A list page does not load its sessions, so a profile may declare one
// request of its own (`sessionInfo`): made only after the user clicks to
// send a session, only for that session, only to the page's own origin,
// and only the declared JSON pointers of its answer are taken (the same as
// `observe`). This module is the pure side: which pages get what, the
// content script's configuration, that request, turning its values into a
// detection-like result, the tracks a bulk send takes, row states. Pure.

import { parseHttpUrl, hostMatches, withoutQuery } from './url.js';
import { profileRegex, regexOwner, profileStopped, profileMatchesUrl, applyProfile, lectureFor, downloadPolicyFor, profilePermissionOrigins } from './profile.js';
import { profileKey, parsePointer, readPointer, cleanObservedRecord, grantedHostPatterns } from './observe.js';
import { isBlockedUrl } from './blocklist.js';
import { groupOf, ACTIVE_STATES, presetSelection } from './send.js';

/** 'player' | 'list' | '' for a frame address. */
export function inpageModeFor(profile, rawUrl) {
  const ip = profile && profile.inpage;
  const u = parseHttpUrl(rawUrl);
  if (!ip || !u || isBlockedUrl(rawUrl)) return '';
  if (!profile.match.hosts.some((h) => hostMatches(h, u.hostname))) return '';
  if (ip.player && profileMatchesUrl(profile, rawUrl)) return 'player';
  if (ip.list) {
    try {
      if (profileRegex(ip.list.pathPattern, regexOwner(profile)).test(u.pathname)) return 'list';
    } catch {
      return '';
    }
  }
  return '';
}

/** The first profile (priority order) with an in-page entry for this address: { profile, reviewed, mode } or null. */
export function findInpage(entries, rawUrl) {
  for (const e of entries || []) {
    try {
      const mode = inpageModeFor(e.profile, rawUrl);
      if (mode) return { profile: e.profile, reviewed: e.reviewed, mode };
    } catch {
      // a broken local profile never stops the page
    }
  }
  return null;
}

/** Where the in-page content script is registered: granted origins ∩ hosts of profiles with `inpage`. */
export function inpageMatches(granted, profiles) {
  const hosts = [];
  for (const p of profiles || []) if (p && p.inpage) hosts.push(...p.match.hosts);
  return grantedHostPatterns(granted, hosts);
}

/**
 * A profile's sessionInfo with what limits it (security review, lead
 * ruling 7): its method and path, plus the urlPattern of the observe entry
 * it names and the profile's page hosts. The request is made only from a
 * frame on one of those hosts, to that frame's own origin, at exactly the
 * declared path, and only when that address matches the pattern. A
 * profile that is not reviewed (a local import) may only GET (FB1 L-2):
 * its spec says getOnly, and sessionInfoRequest refuses anything else.
 */
export function sessionInfoSpec(profile, { reviewed = false } = {}) {
  const info = profile && profile.sessionInfo;
  const obs = info && (profile.observe || [])[info.observe];
  if (!info || !obs) return null;
  return { method: info.method, path: info.path, form: { ...(info.form || {}) }, urlPattern: obs.urlPattern, hosts: [...profile.match.hosts], getOnly: !reviewed, owner: regexOwner(profile) };
}

/** The pointers a sessionInfo answer is read with (those of its observe entry). */
export function sessionInfoPointers(profile) {
  const info = profile && profile.sessionInfo;
  const obs = info && (profile.observe || [])[info.observe];
  return obs ? { ...obs.pointers } : {};
}

/**
 * What the content script of a frame needs: the mode, where buttons go,
 * the list's selectors, the sessionInfo request template and its pointers.
 */
export function inpageConfig(found) {
  const p = found.profile;
  const list = p.inpage.list;
  return {
    key: profileKey(p),
    id: p.id,
    name: p.name,
    version: p.version,
    reviewed: !!found.reviewed,
    mode: found.mode,
    player: p.inpage.player || [],
    list: list
      ? {
          row: list.row,
          idAttr: list.idAttr || 'id',
          idPattern: list.idPattern || '',
          anchor: list.anchor,
          title: list.title || list.anchor,
          header: list.header || [],
        }
      : null,
    sessionInfo: sessionInfoSpec(p, { reviewed: !!found.reviewed }),
    pointers: sessionInfoPointers(p),
    presets: p.presets || null,
  };
}

/** A row's session id from its attribute value ("" when it does not look like one); owner: the profile's key. */
export function rowSessionId(raw, list, owner = '') {
  const v = String(raw ?? '').trim();
  if (!v) return '';
  let id = v;
  if (list.idPattern) {
    const m = profileRegex(list.idPattern, owner).exec(v);
    if (!m) return '';
    id = m.length > 1 ? m[1] || '' : m[0];
  }
  return id && id.length <= 200 && !/[\s{}]/.test(id) ? id : '';
}

// A sessionInfo path as the schema allows it: absolute, plain segments, no
// dot segments, no query or fragment.
const SESSION_PATH = /^(?:\/(?!\.\.?(?:\/|$))[A-Za-z0-9._~!$&'()*+,;=:@-]+)+\/?$/;

/**
 * The sessionInfo request for one session, against the frame's own origin:
 * { url, method, headers, body } or null. info: sessionInfoSpec(profile);
 * a bare profile.sessionInfo is checked for method, origin and path only.
 * {sessionId} is the only placeholder; a GET puts the fields in the query,
 * a POST in a form body.
 */
export function sessionInfoRequest(info, sessionId, frameUrl) {
  const base = parseHttpUrl(frameUrl);
  if (!info || !base || !sessionId || typeof info.path !== 'string' || !SESSION_PATH.test(info.path)) return null;
  if (info.method !== 'GET' && info.method !== 'POST') return null;
  if (info.getOnly && info.method !== 'GET') return null;
  if (Array.isArray(info.hosts) && !info.hosts.some((h) => hostMatches(h, base.hostname))) return null;
  const url = new URL(info.path, base.origin);
  // nothing normalised away: exactly the declared path, on the frame's origin
  if (url.origin !== base.origin || url.pathname !== info.path) return null;
  if (info.urlPattern !== undefined) {
    let ok = false;
    try {
      ok = profileRegex(info.urlPattern, typeof info.owner === 'string' ? info.owner : '').test(withoutQuery(url.href));
    } catch {
      ok = false;
    }
    if (!ok) return null;
  }
  const params = new URLSearchParams();
  for (const [k, tpl] of Object.entries(info.form || {})) params.append(k, String(tpl).split('{sessionId}').join(sessionId));
  if (info.method === 'GET') {
    for (const [k, v] of params) url.searchParams.append(k, v);
    return { url: url.href, method: 'GET', headers: { accept: 'application/json' }, body: null };
  }
  return { url: url.href, method: 'POST', headers: { 'content-type': 'application/x-www-form-urlencoded; charset=UTF-8', accept: 'application/json' }, body: params.toString() };
}

/** The declared values of a parsed answer (scalars only; see readPointer). */
export function extractPointers(obj, pointers) {
  const out = {};
  for (const [name, p] of Object.entries(pointers || {})) {
    const seg = parsePointer(p);
    if (!seg) continue;
    const v = readPointer(obj, seg);
    if (v !== undefined) out[name] = v;
  }
  return out;
}

/**
 * The answer to an in-page card's "session.load" (src/background/main.js):
 * { ok: true, result } or the error the card shows. A profile the runtime
 * guard stopped reads nothing any more: the error then says so
 * (policyStopped), and the bulk card shows why instead of a bare "could not
 * read" (SEC22 L-D). The session is not sent either way.
 */
export function sessionLoadReply(args) {
  const result = sessionResult(args);
  if (result) return { ok: true, result };
  return { ok: false, error: { code: 'ext.info_failed', params: { status: 0 } }, policyStopped: profileStopped(args.found.profile) };
}

/**
 * A detection-like result for one listed session from its sessionInfo
 * values: what the popup and the send path use (src/lib/send.js), with no
 * generic tracks. Returns null when the values do not fit the profile.
 */
export function sessionResult({ found, sessionId, values, frameUrl, locale = 'en' }) {
  const profile = found.profile;
  const info = profile.sessionInfo;
  const req = sessionInfoRequest(sessionInfoSpec(profile, { reviewed: !!found.reviewed }), sessionId, frameUrl);
  if (!req) return null;
  const rec = cleanObservedRecord(profile, { key: profileKey(profile), index: info.observe, url: req.url, values });
  if (!rec) return null;
  const urls = {};
  for (const name of Object.keys(rec.values)) urls[name] = rec.url;
  for (const name of Object.keys(sessionInfoPointers(profile))) urls[name] = rec.url;
  const observed = { seen: new Set([info.observe]), values: rec.values, urls };
  const match = { profile, reviewed: found.reviewed, url: frameUrl };
  const named = applyProfile(profile, [], locale, observed);
  const result = {
    status: 'ok',
    source: 'session',
    at: Date.now(),
    pageUrl: withoutQuery(frameUrl) ? frameUrl : '',
    authorized: true,
    profile: { id: profile.id, version: profile.version, name: profile.name, reviewed: !!found.reviewed, origins: profilePermissionOrigins(profile) },
    request: { ...(profile.request || {}) },
    presets: profile.presets || null,
    observe: { declared: true, seen: true },
    sessionId,
    sessionSource: 'profile',
    lecture: lectureFor(match, {}, 0, observed),
    downloadPolicy: downloadPolicyFor(match, {}, observed),
    policyStopped: profileStopped(profile),
    tracks: [],
    named,
    summary: null,
    raw: null,
  };
  const summary = { total: named.tracks.length, audio: 0, video: 0, unknown: 0, protected: 0 };
  for (const t of named.tracks) {
    summary[t.kind] = (summary[t.kind] || 0) + 1;
    if (t.protected) summary.protected++;
  }
  result.summary = summary;
  return result;
}

/**
 * The tracks a bulk send takes for one session (ids), by the card's
 * choice: main; dual, or main when the session has fewer than two
 * pictures; audio, or nothing when it has no separate audio.
 */
export function bulkTrackIds(tracks, choice, presets) {
  if (choice === 'dual') return presetSelection(tracks, 'dual', presets) || presetSelection(tracks, 'main', presets) || [];
  return presetSelection(tracks, choice === 'audio' ? 'audio' : 'main', presets) || [];
}

/** A row's state from its check item: new | imported | active | refresh | again. */
export function rowState(item) {
  if (!item) return 'new';
  if (item.state === 'imported') return 'imported';
  if (item.state === 'needs_refresh') return 'refresh';
  if (ACTIVE_STATES.includes(item.state)) return 'active';
  if (item.state === 'failed' || item.state === 'cancelled') return 'again';
  return 'new';
}

/**
 * Counts for the bulk card's summary line. A lecture that needs a refresh
 * is not being fetched (T26 G5): refresh counts it apart.
 */
export function bulkCounts(rows, items, picked) {
  const byId = new Map((items || []).map((i) => [i.sessionId, i]));
  let imported = 0;
  let active = 0;
  let refresh = 0;
  for (const r of rows) {
    const s = rowState(byId.get(r.sessionId));
    if (s === 'imported') imported++;
    else if (s === 'active') active++;
    else if (s === 'refresh') refresh++;
  }
  return { total: rows.length, imported, active, refresh, toSend: rows.filter((r) => picked.has(r.sessionId)).length };
}

export { groupOf };
