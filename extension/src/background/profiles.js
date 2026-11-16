// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Profiles: the reviewed ones packaged with the extension (profiles/index.json,
// written by the build) and the user's local imports (storage.local, written
// by the options page, marked "未审核"). Local ones come first so a
// contributor can try a new version of a packaged profile — but a local
// file with a packaged profile's id is used only when the user confirmed,
// on import, that it replaces the packaged one (the entry says which
// version: `replaces`; security review L7); otherwise the packaged profile
// stays in force. A local profile that no longer passes the schema is
// skipped, never trusted.

import { browser } from './env.js';
import { checkProfile } from '../lib/profile.js';
import { activeLocalEntries } from '../lib/localprofiles.js';
import { HEALTH_KEY, healthKey, isStopped, tripEntry, nextRecord, withRecord, cleanTrip, recordable } from '../lib/health.js';
import { setRegexGuard, resetRegexGuard, PAGE_WINDOW_MS, BACKGROUND_WINDOW_BUDGET_MS } from '../lib/regexguard.js';

const LOCAL_KEY = 'localProfiles';
let bundledCache = null;
let schemaCache = null;

export async function profileSchema() {
  if (!schemaCache) schemaCache = await (await fetch(browser.runtime.getURL('schema/profile.schema.json'))).json();
  return schemaCache;
}

async function bundled() {
  if (bundledCache) return bundledCache;
  const out = [];
  try {
    const index = await (await fetch(browser.runtime.getURL('profiles/index.json'))).json();
    for (const file of index.profiles || []) {
      const p = await (await fetch(browser.runtime.getURL(`profiles/${file}`))).json();
      out.push({ profile: p, reviewed: true, source: 'bundled' });
    }
  } catch {
    // a build without profiles
  }
  bundledCache = out;
  return out;
}

export async function localProfiles() {
  const stored = (await browser.storage.local.get(LOCAL_KEY))[LOCAL_KEY];
  return Array.isArray(stored) ? stored : [];
}

/** All profiles in priority order: [{ profile, reviewed, source }], without those stopped as too slow. */
export async function allProfiles() {
  return (await profilesWithStopped()).filter((e) => !e.stopped).map(({ profile, reviewed, source }) => ({ profile, reviewed, source }));
}

async function profilesWithStopped() {
  const schema = await profileSchema();
  const packaged = await bundled();
  const local = activeLocalEntries(await localProfiles(), packaged.map((e) => e.profile.id))
    .filter((e) => checkProfile(schema, e.profile, { unreviewed: true }).length === 0)
    .map((e) => ({ profile: e.profile, reviewed: false, source: 'local' }));
  const health = await profileHealth();
  const version = browser.runtime.getManifest().version;
  return [...local, ...packaged].map((e) => ({ ...e, stopped: isStopped(health[healthKey(e)], e, version) }));
}

// ---------------------------------------------- slow regexes (SEC18d)

export async function profileHealth() {
  const h = (await browser.storage.local.get(HEALTH_KEY))[HEALTH_KEY];
  return h && typeof h === 'object' ? h : {};
}

let recording = Promise.resolve();

/**
 * Records a trip of the runtime regex guard (src/lib/regexguard.js): from
 * this background (origin 'background'), or as a content script reported
 * it ('content', checked again here). Only a regex of a profile in use
 * counts, and a content script's trip only for a local profile
 * (recordable); the record names the rule's place and the time, never the
 * page (src/lib/health.js).
 */
export function recordTrip(raw, origin = 'content') {
  const trip = cleanTrip(raw);
  if (!trip) return Promise.resolve(false);
  const run = recording.then(async () => {
    const found = tripEntry(await profilesWithStopped(), trip.owner, trip.source);
    if (!found || !recordable(found.entry, origin)) return false;
    const key = healthKey(found.entry);
    const health = await profileHealth();
    const rec = nextRecord(health[key], {
      entry: found.entry,
      rule: found.rule,
      ms: trip.ms,
      reason: trip.reason,
      extVersion: browser.runtime.getManifest().version,
      today: new Date().toISOString().slice(0, 10),
    });
    await browser.storage.local.set({ [HEALTH_KEY]: withRecord(health, key, rec) });
    return true;
  });
  recording = run.catch(() => false);
  return run;
}

/**
 * The background's own regexes report here. Their budget starts again with
 * every detection (detector.js) and, as in a content script, every minute:
 * matches the messages cause between detections do not add up for ever
 * (SEC18f 低-19). Within the minute, a profile may spend at most
 * BACKGROUND_WINDOW_BUDGET_MS however many detections a page starts (SEC22
 * L-E).
 */
export function watchRegexes() {
  setRegexGuard({ windowMs: PAGE_WINDOW_MS, windowBudgetMs: BACKGROUND_WINDOW_BUDGET_MS, onTrip: (t) => recordTrip(t, 'background').catch(() => false) });
}

/** After the settings page cleared a record: this background tries again. */
export function forgetTrips() {
  resetRegexGuard();
}
