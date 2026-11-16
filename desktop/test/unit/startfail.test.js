// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// lib/startfail.js: the lines of a long or failed start
// (cmd/lectureflow/startfail.go) and the texts of the coded failures.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const sf = require('../../src/main/lib/startfail');
const i18n = require('../../src/main/lib/i18n');

test('LECTUREFLOW_STARTING lines name the step', () => {
  assert.equal(sf.parseStartingLine('LECTUREFLOW_STARTING upgrade-backup'), 'upgrade-backup');
  assert.equal(sf.parseStartingLine('LECTUREFLOW_STARTING seal-credentials\r'), 'seal-credentials');
  for (const bad of ['LECTUREFLOW_STARTING ', 'LECTUREFLOW_STARTING Upgrade', 'LECTUREFLOW_STARTING a b', 'level=INFO LECTUREFLOW_STARTING x', '', null]) {
    assert.equal(sf.parseStartingLine(bad), null, String(bad));
  }
});

test('the failure line before the ERROR line is read; anything else is not', () => {
  const lines = [
    'level=ERROR msg=fatal',
    'LECTUREFLOW_FAILURE {"code":"startup.data_dir_in_use","params":{"data_dir":"C:\\\\Users\\\\x\\\\LectureFlow","pid":4242}}',
    'ERROR: 另一个 LectureFlow（进程 4242）正在使用这个数据目录 / Another LectureFlow (process 4242) is using this data directory',
  ];
  assert.deepEqual(sf.extractFailure(lines), { code: 'startup.data_dir_in_use', params: { data_dir: 'C:\\Users\\x\\LectureFlow', pid: 4242 } });
  assert.equal(sf.extractFailure(['ERROR: open db: database is locked']), null);
  assert.equal(sf.extractFailure(['LECTUREFLOW_FAILURE {not json']), null);
  assert.equal(sf.extractFailure(['LECTUREFLOW_FAILURE {"code":"selfupdate.x","params":{}}']), null);
  assert.deepEqual(sf.extractFailure(['LECTUREFLOW_FAILURE {"code":"startup.upgrade_backup_failed","params":[1]}']), { code: 'startup.upgrade_backup_failed', params: {} });
  assert.equal(sf.extractFailure(undefined), null);
});

const failures = {
  in_use: { code: 'startup.data_dir_in_use', params: { data_dir: 'D:\\LF data', pid: 4242 } },
  in_use_nopid: { code: 'startup.data_dir_in_use', params: { data_dir: 'D:\\LF data' } },
  newer: { code: 'startup.data_dir_newer', params: { found: 15, known: 14, backups: 'D:\\LF data\\backups' } },
  space: { code: 'startup.upgrade_backup_no_space', params: { dir: 'D:\\LF data\\backups', need_bytes: 3.4 * 2 ** 30, free_bytes: 1.04 * 2 ** 30 } },
  failed: { code: 'startup.upgrade_backup_failed', params: { dir: 'D:\\LF data\\backups' } },
  db_busy: { code: 'startup.db_busy', params: { path: 'D:\\LF data\\lectureflow.db' } },
  db_readonly: { code: 'startup.db_readonly', params: { path: 'D:\\LF data\\lectureflow.db' } },
  db_not_a_database: { code: 'startup.db_not_a_database', params: { path: 'D:\\LF data\\lectureflow.db', backups: 'D:\\LF data\\backups' } },
  db_cant_open: { code: 'startup.db_cant_open', params: { path: 'D:\\LF data\\lectureflow.db' } },
  db_disk_full: { code: 'startup.db_disk_full', params: { path: 'D:\\LF data\\lectureflow.db' } },
  db_io_error: { code: 'startup.db_io_error', params: { path: 'D:\\LF data\\lectureflow.db' } },
};

test('every coded failure has its text in all six languages, parameters filled', () => {
  for (const locale of i18n.LOCALES) {
    const { t } = i18n.loadTranslator(locale);
    for (const [name, f] of Object.entries(failures)) {
      const text = sf.startFailureText(f, t);
      assert.ok(text && !text.startsWith('startfail.'), `${locale} ${name}: ${text}`);
      assert.doesNotMatch(text, /\{\w+\}/, `${locale} ${name}: a placeholder left`);
      assert.ok(text.includes('D:\\LF data'), `${locale} ${name}: the folder`);
    }
    assert.ok(sf.startFailureText(failures.in_use, t).includes('4242'), locale);
    const newer = sf.startFailureText(failures.newer, t);
    assert.ok(newer.includes('15') && newer.includes('14') && newer.includes('backups'), `${locale} newer`);
    const space = sf.startFailureText(failures.space, t);
    assert.ok(space.includes('3.4') && space.includes('1.0'), `${locale} space: ${space}`);
    for (const name of ['db_busy', 'db_readonly', 'db_not_a_database', 'db_cant_open', 'db_disk_full', 'db_io_error']) {
      assert.ok(sf.startFailureText(failures[name], t).includes('lectureflow.db'), `${locale} ${name}: the database file`);
    }
    assert.ok(sf.startFailureText(failures.db_not_a_database, t).includes('D:\\LF data\\backups'), `${locale} db_not_a_database: the backups`);
    for (const name of ['newer', 'db_not_a_database']) {
      const text = sf.startFailureText(failures[name], t);
      assert.ok(text.includes('lectureflow.db-wal') && text.includes('lectureflow.db-shm'), `${locale} ${name}: the -wal and -shm files move along`);
    }
    assert.notEqual(t('splash.upgrading'), 'splash.upgrading', locale);
  }
});

test('unknown codes and missing parameters fall back to the generic text', () => {
  const { t } = i18n.loadTranslator('en');
  assert.equal(sf.startFailureText(null, t), null);
  assert.equal(sf.startFailureText({ code: 'startup.something_new', params: {} }, t), null);
  assert.equal(sf.startFailureText({ code: 'startup.data_dir_newer', params: { found: 15 } }, t), null);
  assert.equal(sf.startFailureText({ code: 'startup.data_dir_in_use', params: { data_dir: 7 } }, t), null);
  assert.equal(sf.startFailureText({ code: 'startup.upgrade_backup_no_space', params: { dir: 'x', need_bytes: -1, free_bytes: 0 } }, t), null);
  assert.equal(sf.startFailureText({ code: 'startup.db_busy', params: {} }, t), null);
  assert.equal(sf.startFailureText({ code: 'startup.db_not_a_database', params: { path: 'x' } }, t), null);
});

test('sizes in GB with one decimal, never 0.0', () => {
  assert.equal(sf.gb(3.45 * 2 ** 30), '3.5');
  assert.equal(sf.gb(0), '0.1');
  assert.equal(sf.gb(10 * 2 ** 20), '0.1');
  assert.equal(sf.gb(2 ** 31), '2.0');
});

test('sizes use the decimal separator of the interface language (T079)', () => {
  assert.equal(sf.gb(3.45 * 2 ** 30, 'ru'), '3,5');
  assert.equal(sf.gb(0, 'ru'), '0,1');
  for (const locale of ['en', 'zh-CN', 'zh-TW', 'yue-Hant-HK', 'hi']) assert.equal(sf.gb(3.45 * 2 ** 30, locale), '3.5', locale);
  assert.equal(sf.gb(1234 * 2 ** 30, 'en'), '1234.0', 'no grouping separator');
  assert.equal(sf.gb(2 ** 31, 'not a tag!'), '2.0', 'an unknown tag falls back');
  assert.ok(['yue-Hant-HK', 'zh-TW', 'en'].includes(sf.numberLocale('yue-Hant-HK')));
  for (const locale of ['ru', 'en', 'zh-CN']) {
    const { t } = i18n.loadTranslator(locale);
    const text = sf.startFailureText(failures.space, t, locale);
    if (locale === 'ru') assert.ok(text.includes('3,4') && text.includes('1,0') && !text.includes('3.4'), `${locale}: ${text}`);
    else assert.ok(text.includes('3.4') && text.includes('1.0'), `${locale}: ${text}`);
  }
});

test('"the data folder is in use" is not the new version\'s fault; the upgrade failures are', () => {
  assert.equal(sf.isEnvironmentFailure(failures.in_use), true);
  for (const f of [failures.newer, failures.space, failures.failed, null]) assert.equal(sf.isEnvironmentFailure(f), false);
});

test('database failures of the environment are not the new version\'s fault; "not a database" is (SEC3 section 4)', () => {
  for (const name of ['db_busy', 'db_readonly', 'db_cant_open', 'db_disk_full', 'db_io_error']) {
    assert.equal(sf.isEnvironmentFailure(failures[name]), true, name);
    // the same code with other parameters (or none) is the same failure
    assert.equal(sf.isEnvironmentFailure({ code: failures[name].code, params: {} }), true, name);
  }
  assert.equal(sf.isEnvironmentFailure(failures.db_not_a_database), false, 'a rollback restores the snapshot, which may repair it');
  assert.deepEqual([...sf.ENVIRONMENT_FAILURES].sort(), [
    'startup.data_dir_in_use', 'startup.db_busy', 'startup.db_cant_open', 'startup.db_disk_full', 'startup.db_io_error', 'startup.db_readonly',
  ]);
  for (const code of ['startup.db_busy_x', 'startup.db', 'db_busy', '', undefined]) assert.equal(sf.isEnvironmentFailure({ code, params: {} }), false, String(code));
});

test('every environment failure is a code the service sends (cmd/lectureflow)', () => {
  const fs = require('node:fs');
  const path = require('node:path');
  const dir = path.join(__dirname, '..', '..', '..', 'cmd', 'lectureflow');
  const src = fs.readdirSync(dir).filter((n) => n.endsWith('.go') && !n.endsWith('_test.go')).map((n) => fs.readFileSync(path.join(dir, n), 'utf8')).join('\n');
  for (const code of sf.ENVIRONMENT_FAILURES) assert.ok(src.includes(`"${code}"`), code);
});
