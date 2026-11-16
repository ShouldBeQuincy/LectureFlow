// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Argument validation for every IPC handler (SPEC-v3 V2: "IPC messages are
// checked parameter by parameter"). Each validator takes the raw argument
// list the renderer sent and returns the sanitised value, or throws
// ValidationError. Unknown keys, wrong types and over-long strings are all
// rejected rather than trimmed, so a caller bug is loud.
'use strict';

const { isExternalWebUrl } = require('./security');

class ValidationError extends TypeError {
  constructor(message) {
    super(message);
    this.name = 'ValidationError';
  }
}

const LIMITS = Object.freeze({
  title: 200,
  body: 1000,
  url: 2048,
  filterName: 100,
  filters: 10,
  extensions: 30,
  extension: 16,
  droppedPaths: 1000, // preload.js MAX_DROPPED
  path: 32767, // the longest Windows path
});

function fail(msg) {
  throw new ValidationError(msg);
}

function isPlainObject(v) {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) return false;
  const proto = Object.getPrototypeOf(v);
  return proto === Object.prototype || proto === null;
}

function checkKeys(obj, allowed, what) {
  for (const k of Object.keys(obj)) {
    if (!allowed.includes(k)) fail(`${what}: unknown key ${JSON.stringify(k).slice(0, 40)}`);
  }
}

function checkString(v, what, { min = 0, max }) {
  if (typeof v !== 'string') fail(`${what}: expected a string`);
  if (v.length < min) fail(`${what}: too short`);
  if (v.length > max) fail(`${what}: too long (max ${max})`);
  if (v.includes('\0')) fail(`${what}: contains NUL`);
  return v;
}

function checkArgCount(args, max) {
  if (!Array.isArray(args)) fail('arguments: expected a list');
  // Trailing undefined values come from optional parameters left out.
  let n = args.length;
  while (n > 0 && args[n - 1] === undefined) n--;
  if (n > max) fail(`arguments: expected at most ${max}, got ${n}`);
}

/** noArgs: pickFolder(), getPowerState(), getAppVersion(), components.importFile(). */
function validateNoArgs(args) {
  checkArgCount(args, 0);
  return undefined;
}

/**
 * validatePickFiles: pickFiles(opts?) with
 *   opts = { title?: string, multiple?: boolean,
 *            filters?: [{ name: string, extensions: string[] }] }
 * Extensions are letters and digits only ("mp4", "pdf") or "*".
 */
function validatePickFiles(args) {
  checkArgCount(args, 1);
  const opts = args[0];
  const out = { title: undefined, multiple: false, filters: [] };
  if (opts === undefined || opts === null) return out;
  if (!isPlainObject(opts)) fail('pickFiles: options must be an object');
  checkKeys(opts, ['title', 'multiple', 'filters'], 'pickFiles');
  if (opts.title !== undefined) out.title = checkString(opts.title, 'pickFiles.title', { min: 1, max: LIMITS.title });
  if (opts.multiple !== undefined) {
    if (typeof opts.multiple !== 'boolean') fail('pickFiles.multiple: expected a boolean');
    out.multiple = opts.multiple;
  }
  if (opts.filters !== undefined) {
    if (!Array.isArray(opts.filters)) fail('pickFiles.filters: expected a list');
    if (opts.filters.length > LIMITS.filters) fail(`pickFiles.filters: at most ${LIMITS.filters}`);
    out.filters = opts.filters.map((f, i) => {
      const what = `pickFiles.filters[${i}]`;
      if (!isPlainObject(f)) fail(`${what}: expected an object`);
      checkKeys(f, ['name', 'extensions'], what);
      const name = checkString(f.name, `${what}.name`, { min: 1, max: LIMITS.filterName });
      if (!Array.isArray(f.extensions) || f.extensions.length === 0) fail(`${what}.extensions: expected a non-empty list`);
      if (f.extensions.length > LIMITS.extensions) fail(`${what}.extensions: at most ${LIMITS.extensions}`);
      const extensions = f.extensions.map((e, j) => {
        checkString(e, `${what}.extensions[${j}]`, { min: 1, max: LIMITS.extension });
        if (e !== '*' && !/^[A-Za-z0-9]+$/.test(e)) fail(`${what}.extensions[${j}]: letters and digits only`);
        return e.toLowerCase();
      });
      return { name, extensions };
    });
  }
  return out;
}

/** validateNotify: notify({ title, body }). */
function validateNotify(args) {
  checkArgCount(args, 1);
  const n = args[0];
  if (!isPlainObject(n)) fail('notify: expected { title, body }');
  checkKeys(n, ['title', 'body'], 'notify');
  const title = checkString(n.title, 'notify.title', { min: 1, max: LIMITS.title });
  const body = n.body === undefined ? '' : checkString(n.body, 'notify.body', { max: LIMITS.body });
  return { title, body };
}

/** validateOpenExternal: openExternal(url), http, https and mailto only. */
function validateOpenExternal(args) {
  checkArgCount(args, 1);
  const url = checkString(args[0], 'openExternal.url', { min: 1, max: LIMITS.url });
  if (!isExternalWebUrl(url)) fail('openExternal: only http, https and mailto links can be opened');
  return new URL(url).href;
}

/**
 * validateDroppedPaths: the preload's lf:grant-dropped(paths), the paths
 * webUtils gave for the files of a drop (T-SEC-08): a list of 1..1000
 * non-empty strings.
 */
function validateDroppedPaths(args) {
  checkArgCount(args, 1);
  const paths = args[0];
  if (!Array.isArray(paths) || paths.length === 0) fail('grantDropped: expected a non-empty list');
  if (paths.length > LIMITS.droppedPaths) fail(`grantDropped: at most ${LIMITS.droppedPaths} paths`);
  return paths.map((p, i) => checkString(p, `grantDropped[${i}]`, { min: 1, max: LIMITS.path }));
}

module.exports = {
  ValidationError,
  LIMITS,
  isPlainObject,
  validateNoArgs,
  validatePickFiles,
  validateNotify,
  validateOpenExternal,
  validateDroppedPaths,
};
