// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Data directory resolution (SPEC-v3 V1.3, v3-brief §3). Pure: the caller
// passes the environment, the platform's appData directory and a file reader,
// so the rules can be unit-tested for Windows and macOS on any machine.
'use strict';

const path = require('node:path');

const APP_DIR_NAME = 'LectureFlow';
const LOCATION_FILE = 'location.json';
// Chromium's own profile (userData: Local State, GPU cache, the single-instance
// lock). Kept in a subfolder so it never mixes with the service's data.
const SHELL_PROFILE_DIR = 'electron';
const MAX_PATH_CHARS = 4096;

/**
 * parseLocation reads the body of location.json ({"data_dir": "..."}).
 * Returns { ok: true, dataDir } or { ok: false, reason }.
 */
function parseLocation(raw, pathMod = path) {
  let v;
  try {
    v = JSON.parse(String(raw).replace(/^\ufeff/, ''));
  } catch {
    return { ok: false, reason: 'not_json' };
  }
  if (!v || typeof v !== 'object' || Array.isArray(v)) return { ok: false, reason: 'not_object' };
  const dir = v.data_dir;
  if (typeof dir !== 'string' || dir.trim() === '') return { ok: false, reason: 'no_data_dir' };
  if (dir.length > MAX_PATH_CHARS || dir.includes('\0')) return { ok: false, reason: 'bad_data_dir' };
  if (!pathMod.isAbsolute(dir.trim())) return { ok: false, reason: 'not_absolute' };
  return { ok: true, dataDir: pathMod.normalize(dir.trim()) };
}

/**
 * resolveDataDir decides where the service keeps its data.
 *
 *   1. LECTUREFLOW_DATA_DIR (acceptance tests, advanced users)
 *   2. {appData}/LectureFlow/location.json → data_dir (written by "change data
 *      directory", step 5)
 *   3. {appData}/LectureFlow  (%APPDATA%\LectureFlow, ~/Library/Application Support/LectureFlow)
 *
 * deps: { env, appData, readFile(path) → string (throws with err.code), pathMod }
 * Returns { dataDir, defaultRoot, profileDir, source: 'env'|'location'|'default',
 *           locationFile, error?: { kind: 'location_invalid', reason } }.
 * A location.json that exists but cannot be used is an error, not a silent
 * fallback: falling back would show an empty library and look like data loss.
 */
function resolveDataDir({ env = {}, appData, readFile, pathMod = path }) {
  if (typeof appData !== 'string' || appData === '') throw new TypeError('appData is required');
  const defaultRoot = pathMod.join(appData, APP_DIR_NAME);
  const locationFile = pathMod.join(defaultRoot, LOCATION_FILE);
  const fromEnv = typeof env.LECTUREFLOW_DATA_DIR === 'string' ? env.LECTUREFLOW_DATA_DIR.trim() : '';
  if (fromEnv) {
    const dataDir = pathMod.resolve(fromEnv);
    return { dataDir, defaultRoot, locationFile, source: 'env', profileDir: pathMod.join(dataDir, SHELL_PROFILE_DIR) };
  }
  const profileDir = pathMod.join(defaultRoot, SHELL_PROFILE_DIR);
  let raw;
  try {
    raw = readFile(locationFile);
  } catch (err) {
    if (err && (err.code === 'ENOENT' || err.code === 'ENOTDIR')) {
      return { dataDir: defaultRoot, defaultRoot, locationFile, source: 'default', profileDir };
    }
    return {
      dataDir: null, defaultRoot, locationFile, source: 'location', profileDir,
      error: { kind: 'location_invalid', reason: (err && err.code) || 'unreadable' },
    };
  }
  const parsed = parseLocation(raw, pathMod);
  if (!parsed.ok) {
    return { dataDir: null, defaultRoot, locationFile, source: 'location', profileDir, error: { kind: 'location_invalid', reason: parsed.reason } };
  }
  return { dataDir: parsed.dataDir, defaultRoot, locationFile, source: 'location', profileDir };
}

module.exports = { resolveDataDir, parseLocation, APP_DIR_NAME, LOCATION_FILE, SHELL_PROFILE_DIR };
