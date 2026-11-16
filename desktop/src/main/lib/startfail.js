// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What the service says about a long or failed start, besides the ready
// line and "ERROR: <reason>" (cmd/lectureflow/startfail.go). Pure functions.
//
//   stdout  "LECTUREFLOW_STARTING <step>"   while a long step runs before the
//           ready line (the backup before an upgrade, the migrations, the
//           sealing of credentials, the import backups' clean-up): once a
//           second into the step, then every 5 s. ServerProcess keeps
//           waiting while they come (test-upgrade F3: a large database's
//           first start took longer than 30 seconds and was killed).
//   stderr  "LECTUREFLOW_FAILURE {"code":"startup.…","params":{…}}"  just
//           before the ERROR line of a failure the user can act on; the
//           error page shows startFailureText in the interface language and
//           the ERROR line (Chinese and English) under "Details".
'use strict';

const { CHAINS } = require('./i18n');

const STARTING_PREFIX = 'LECTUREFLOW_STARTING ';
const FAILURE_PREFIX = 'LECTUREFLOW_FAILURE ';

/** parseStartingLine: the step a "LECTUREFLOW_STARTING <step>" line names, or null. */
function parseStartingLine(line) {
  if (typeof line !== 'string' || !line.startsWith(STARTING_PREFIX)) return null;
  const step = line.slice(STARTING_PREFIX.length).trim();
  return /^[a-z][a-z0-9-]{0,40}$/.test(step) ? step : null;
}

const str = (v) => (typeof v === 'string' && v.length <= 1000 ? v : null);
const num = (v) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : null);

/**
 * extractFailure(lines): { code, params } of the last LECTUREFLOW_FAILURE
 * line of stderr, or null (none, or not valid JSON with a startup.* code).
 */
function extractFailure(lines) {
  for (let i = (lines || []).length - 1; i >= 0; i--) {
    const l = lines[i];
    if (typeof l !== 'string' || !l.startsWith(FAILURE_PREFIX)) continue;
    let v;
    try {
      v = JSON.parse(l.slice(FAILURE_PREFIX.length));
    } catch {
      return null;
    }
    if (!v || typeof v.code !== 'string' || !/^startup\.[a-z_]{1,60}$/.test(v.code)) return null;
    const params = v.params && typeof v.params === 'object' && !Array.isArray(v.params) ? v.params : {};
    return { code: v.code, params };
  }
  return null;
}

/**
 * numberLocale(locale): the first tag of the shell locale's fallback chain
 * (lib/i18n.js CHAINS) that Intl knows ("yue-Hant-HK" may be missing from a
 * small ICU: zh-TW, then en), or "en".
 */
function numberLocale(locale) {
  const chain = [...(CHAINS[locale] || [locale]), 'en'];
  for (const l of chain) {
    try {
      if (typeof l === 'string' && Intl.NumberFormat.supportedLocalesOf([l]).length > 0) return l;
    } catch {
      // not a valid tag: the next one
    }
  }
  return 'en';
}

/**
 * gb(bytes, locale): "3.1" (at least "0.1"), the number the texts put before
 * "GB", with the decimal separator of the interface language ("3,1" in ru).
 */
function gb(bytes, locale = 'en') {
  const n = Math.max(0.1, Math.round((bytes / 2 ** 30) * 10) / 10);
  return new Intl.NumberFormat(numberLocale(locale), { minimumFractionDigits: 1, maximumFractionDigits: 1, useGrouping: false }).format(n);
}

/**
 * startFailureText(failure, t, locale): the error page's text for a known
 * code in the interface language (desktop/locales "startfail.*"; locale
 * formats the numbers), or null (an unknown code, or its parameters
 * missing: the generic text then).
 */
function startFailureText(failure, t, locale = 'en') {
  if (!failure) return null;
  const p = failure.params || {};
  switch (failure.code) {
    case 'startup.data_dir_in_use': {
      const dir = str(p.data_dir);
      if (dir === null) return null;
      const pid = num(p.pid);
      return pid ? t('startfail.data_dir_in_use', { pid: String(pid), path: dir }) : t('startfail.data_dir_in_use_nopid', { path: dir });
    }
    case 'startup.data_dir_newer': {
      const found = num(p.found);
      const known = num(p.known);
      const backups = str(p.backups);
      if (found === null || known === null || backups === null) return null;
      return t('startfail.data_dir_newer', { found: String(found), known: String(known), path: backups });
    }
    case 'startup.upgrade_backup_no_space': {
      const need = num(p.need_bytes);
      const free = num(p.free_bytes);
      const dir = str(p.dir);
      if (need === null || free === null || dir === null) return null;
      return t('startfail.upgrade_backup_no_space', { need: gb(need, locale), free: gb(free, locale), path: dir });
    }
    case 'startup.upgrade_backup_failed': {
      const dir = str(p.dir);
      if (dir === null) return null;
      return t('startfail.upgrade_backup_failed', { path: dir });
    }
    // the database cannot be opened or migrated (test-upgrade F5,
    // cmd/lectureflow/dbfail.go)
    case 'startup.db_busy':
    case 'startup.db_readonly':
    case 'startup.db_cant_open':
    case 'startup.db_disk_full':
    case 'startup.db_io_error': {
      const path = str(p.path);
      if (path === null) return null;
      return t(`startfail.${failure.code.slice('startup.'.length)}`, { path });
    }
    case 'startup.db_not_a_database': {
      const path = str(p.path);
      const backups = str(p.backups);
      if (path === null || backups === null) return null;
      return t('startfail.db_not_a_database', { path, backups });
    }
    default:
      return null;
  }
}

/**
 * ENVIRONMENT_FAILURES: failures whose cause lies outside this version:
 * another LectureFlow holds the data folder; the database is locked by
 * another program, read-only, cannot be opened (a disconnected disk, no
 * permission), the disk is full or failing (cmd/lectureflow/dbfail.go).
 * The previous version would meet them as well, and a rollback writes to
 * the database files (security review SEC3, section 4). Not here:
 * startup.upgrade_backup_no_space (the backup comes before the migrations,
 * so after a rollback the older version starts on an unchanged database);
 * startup.db_not_a_database, which the new version itself may have caused
 * and which the database snapshot a rollback restores can repair;
 * "database table is locked", always this program's own fault, has
 * no code (internal/db/openerr.go). A defect of the new version can still
 * look like one of these: the shell gives back at most
 * selfupdate.js MAX_REFUNDS starts per installation.
 */
const ENVIRONMENT_FAILURES = Object.freeze([
  'startup.data_dir_in_use',
  'startup.db_busy',
  'startup.db_readonly',
  'startup.db_cant_open',
  'startup.db_disk_full',
  'startup.db_io_error',
]);

/**
 * isEnvironmentFailure: the start failed because of something outside this
 * version (ENVIRONMENT_FAILURES): a freshly installed version that meets it
 * has not failed its probation (no rollback, and the start does not count
 * as a try: shell.js refunds it).
 */
function isEnvironmentFailure(failure) {
  return !!failure && ENVIRONMENT_FAILURES.includes(failure.code);
}

module.exports = {
  STARTING_PREFIX,
  FAILURE_PREFIX,
  parseStartingLine,
  extractFailure,
  startFailureText,
  ENVIRONMENT_FAILURES,
  isEnvironmentFailure,
  gb,
  numberLocale,
};
