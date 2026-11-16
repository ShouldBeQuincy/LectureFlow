// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Profiles stopped for slow regexes (SEC18d): the records the background
// keeps in storage.local under HEALTH_KEY when the runtime guard
// (./regexguard.js) trips, and the rules that say which profiles they
// stop. Pure; src/background/health.js does the storage.
//
// A record: { id, version, source: 'local' | 'bundled', rule (the regex's
// place in the file, "/tracks/1/detect/urlPattern"), ms, reason ('slow' |
// 'budget'), trips, disabled, extVersion, at (a date, YYYY-MM-DD) }. No
// address, no page, no input: only where in the profile and how long.
//   - A local (unreviewed) profile is stopped at its first trip, until the
//     user imports a file again (or removes it).
//   - A bundled (reviewed) profile is ours: one slow match may be a busy
//     machine, so it is stopped only after BUNDLED_TRIPS trips, or one match
//     of BUNDLED_SEVERE_MS or more; a record from another extension version
//     or profile version does not count, so an update retries it. The
//     settings page offers to enable it again and asks for a report.

import { profileKey } from './observe.js';
import { profileRegexes } from './profile.js';

export const HEALTH_KEY = 'profileHealth';
export const BUNDLED_TRIPS = 3;
export const BUNDLED_SEVERE_MS = 2000;
export const MAX_RECORDS = 64;

/** The record key of a profile entry ({ profile, source }). */
export function healthKey(entry) {
  const p = entry.profile;
  return entry.source === 'local' ? `local:${p.id}` : `bundled:${p.id}@${p.version}`;
}

/** Whether a record stops this entry now. */
export function isStopped(rec, entry, extVersion) {
  if (!rec || rec.disabled !== true) return false;
  if (entry.source === 'local') return true;
  return rec.extVersion === extVersion && rec.version === entry.profile.version;
}

/**
 * The entry a trip belongs to and the place of its regex, or null: the
 * first entry (priority order) whose key is the trip's owner and which has
 * that regex; without an owner, the first entry that has it.
 */
export function tripEntry(entries, owner, source) {
  for (const e of entries || []) {
    if (owner && profileKey(e.profile) !== owner) continue;
    const hit = profileRegexes(e.profile).find(([, src]) => src === source);
    if (hit) return { entry: e, rule: hit[0] };
  }
  return null;
}

/**
 * Whether a trip may be recorded (SEC18e 中-5). A trip a content script
 * measured may stop only a local profile: a page that can run script on
 * the platform could drive a content script's matches (forged events, many
 * rows, a heap that makes the garbage collector pause) and so stop a
 * reviewed, bundled profile for every tab. A bundled profile is stopped
 * only by what the background measured itself, on inputs it bounds (256
 * addresses a rule, one budget a detection) and in a process the page
 * cannot touch; in a page, the content script stops it for that page only.
 */
export function recordable(entry, origin) {
  return origin === 'background' || entry.source === 'local';
}

/** Whether a message comes from one of the extension's own pages (the settings, open in a tab or not), not a content script. */
export function fromExtensionPage(sender, extensionBase) {
  return !!sender && typeof sender.url === 'string' && typeof extensionBase === 'string' && extensionBase !== '' && sender.url.startsWith(extensionBase);
}

/** Whether a change of the records removed one (the settings enabled a profile again). */
export function recordRemoved(oldValue, newValue) {
  const before = oldValue && typeof oldValue === 'object' ? Object.keys(oldValue) : [];
  const after = newValue && typeof newValue === 'object' ? newValue : {};
  return before.some((k) => !(k in after));
}

/** The record after one more trip. */
export function nextRecord(prev, { entry, rule, ms, reason, extVersion, today }) {
  const bundled = entry.source !== 'local';
  const same = prev && prev.extVersion === extVersion && prev.version === entry.profile.version;
  const trips = (same && Number.isInteger(prev.trips) ? prev.trips : 0) + 1;
  // "one match of BUNDLED_SEVERE_MS": a slow match, not a budget's total
  // (the background's minute adds up to 3 s and more; SEC22b S-1)
  const disabled = bundled ? trips >= BUNDLED_TRIPS || (reason !== 'budget' && ms >= BUNDLED_SEVERE_MS) : true;
  return {
    id: String(entry.profile.id),
    version: String(entry.profile.version),
    source: bundled ? 'bundled' : 'local',
    rule: String(rule),
    ms: Math.max(0, Math.round(ms)),
    reason: reason === 'budget' ? 'budget' : 'slow',
    trips,
    disabled,
    extVersion: String(extVersion),
    at: String(today),
  };
}

/** The records with one set, at most MAX_RECORDS (the oldest dropped). */
export function withRecord(health, key, rec) {
  const out = { ...(health && typeof health === 'object' ? health : {}) };
  delete out[key];
  out[key] = rec;
  const keys = Object.keys(out);
  for (const k of keys.slice(0, Math.max(0, keys.length - MAX_RECORDS))) delete out[k];
  return out;
}

/** A trip as a content script reports it, checked; null when it is not one. */
export function cleanTrip(msg) {
  if (!msg || typeof msg !== 'object') return null;
  const { owner, source, ms, reason } = msg;
  if (typeof owner !== 'string' || owner.length > 200) return null;
  if (typeof source !== 'string' || !source || source.length > 300) return null;
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0 || ms > 1e7) return null;
  if (reason !== 'slow' && reason !== 'budget') return null;
  return { owner, source, ms, reason };
}
