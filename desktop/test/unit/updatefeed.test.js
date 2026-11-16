// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The update feed of a packaged app (SPEC-v3 V8, gate fix 1): without
// resources/app-update.yml the main process never loads electron-updater.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const { parseRepoSlug, parseAppUpdateYml, updateFeedFrom, updateFeedNote } = require('../../src/main/lib/updatefeed');

function reader(files) {
  return (p) => {
    const name = path.basename(p);
    if (!(name in files)) throw Object.assign(new Error('ENOENT'), { code: 'ENOENT' });
    if (files[name] instanceof Error) throw files[name];
    return files[name];
  };
}

test('repository names: strict owner/name', () => {
  assert.deepEqual(parseRepoSlug('example-owner/repo.name_1'), { owner: 'example-owner', repo: 'repo.name_1' });
  assert.deepEqual(parseRepoSlug('A1/b'), { owner: 'A1', repo: 'b' });
  const bad = [undefined, null, 1, '', 'x', 'a/b/c', 'a /b', 'a/b ', 'a/b.git', 'a/.', 'a/..', '-a/b', 'a-/b', 'a--b/c', 'a_b/c', 'a/b c', 'a/b?x', `${'a'.repeat(40)}/b`, `a/${'b'.repeat(101)}`];
  for (const s of bad) assert.equal(parseRepoSlug(s), null, JSON.stringify(s));
  assert.ok(parseRepoSlug(`${'a'.repeat(39)}/${'b'.repeat(100)}`));
});

test('app-update.yml as electron-builder writes it', () => {
  const y = parseAppUpdateYml('owner: o\r\nrepo: "r"\nprovider: github\npublisherName:\n  - CN=Example\n  - \'Other\'\nupdaterCacheDirName: x\n');
  assert.deepEqual(y, { owner: 'o', repo: 'r', provider: 'github', publisherName: ['CN=Example', 'Other'], updaterCacheDirName: 'x' });
});

test('no app-update.yml: updates off, updater not loaded', () => {
  const feed = updateFeedFrom({ resourcesPath: 'R', readFile: reader({}) });
  assert.deepEqual(feed, { enabled: false, reason: 'not_configured' });
  assert.match(updateFeedNote(feed), /no update repository was configured for this build; automatic updates are off/);
});

test('a GitHub feed with a valid repository: updates on', () => {
  const feed = updateFeedFrom({ resourcesPath: 'R', readFile: reader({ 'app-update.yml': 'owner: o\nrepo: r\nprovider: github\n' }) });
  assert.deepEqual(feed, { enabled: true, owner: 'o', repo: 'r' });
  assert.match(updateFeedNote(feed), /o\/r/);
});

test('an unusable app-update.yml keeps updates off', () => {
  const texts = ['provider: generic\nurl: https://example.org/\n', 'provider: github\nowner: o\n', 'provider: github\nowner: o\nrepo: r/x\n', ''];
  for (const text of texts) {
    const feed = updateFeedFrom({ resourcesPath: 'R', readFile: reader({ 'app-update.yml': text }) });
    assert.equal(feed.enabled, false, text);
    assert.equal(feed.reason, 'invalid');
    assert.match(updateFeedNote(feed), /automatic updates are off/);
  }
  const denied = Object.assign(new Error('EACCES'), { code: 'EACCES' });
  const unreadable = updateFeedFrom({ resourcesPath: 'R', readFile: reader({ 'app-update.yml': denied }) });
  assert.equal(unreadable.enabled, false);
  assert.match(unreadable.detail, /EACCES/);
});

test('the shell never loads electron-updater (security audit FB1 L-8)', () => {
  const dir = path.join(__dirname, '..', '..', 'src');
  const files = [];
  const walk = (d, depth) => {
    if (depth > 5) throw new Error(`too deep: ${d}`);
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      if (e.isDirectory()) walk(path.join(d, e.name), depth + 1);
      else if (/\.(c|m)?js$/.test(e.name)) files.push(path.join(d, e.name));
    }
  };
  walk(dir, 0);
  assert.ok(files.length > 10);
  for (const f of files) {
    const src = fs.readFileSync(f, 'utf8');
    assert.doesNotMatch(src, /require\(\s*['"`]electron-updater['"`]\s*\)|import\(\s*['"`]electron-updater['"`]\s*\)|from\s+['"`]electron-updater['"`]/, path.relative(dir, f));
  }
  // packaged: no updater whatever app-update.yml says; development: only the tests' stand-in
  const src = fs.readFileSync(path.join(dir, 'main', 'main.js'), 'utf8');
  const body = src.slice(src.indexOf('function loadUpdater'), src.indexOf('function refuseDebugSwitches'));
  assert.match(body, /if \(app\.isPackaged\) return \{ updater: null, note: PACKAGED_UPDATER_NOTE \};/);
  assert.doesNotMatch(body, /updateFeedFrom|resourcesPath/);
});
