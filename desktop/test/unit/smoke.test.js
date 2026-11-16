// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// scripts/smoke.js (the packaged build's smoke test, release.yml): which
// update mechanism desktop.log must name, and the app's environment.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { updateMechanism, expectedUpdates, smokeEnv, smokeDirs, removeRoot } = require('../../scripts/smoke');
const { updateFeedNote } = require('../../src/main/lib/updatefeed');

const READY = '2026-10-27T16:00:09.408Z INFO service ready on port 51247 (attempt 1)\n';
// The lines as the shell writes them (updatepath.js; updatefeed.js updateFeedNote).
const SELF_LINE = 'updates: in-app updates (signed update.json) are configured; electron-updater is not loaded';
const SELF = `${READY}2026-10-27T16:03:01.233Z INFO ${SELF_LINE}\n`;
const NONE = `${READY}2026-10-27T16:00:09.431Z INFO ${updateFeedNote({ enabled: false, reason: 'not_configured' })}\n`;
const REPO = `${READY}2026-10-27T16:00:09.431Z INFO ${updateFeedNote({ enabled: true, owner: 'example-owner', repo: 'example-repo' })}\n`;
const UNUSABLE = `${READY}2026-10-27T16:00:09.431Z INFO ${updateFeedNote({ enabled: false, reason: 'invalid', detail: 'provider "s3" is not github' })}\n`;
// A minisign public key line (made up for the test: "Ed", key id, 32 bytes).
const TEST_KEY = Buffer.concat([Buffer.from('Ed'), Buffer.from('0807060504030201', 'hex'), Buffer.alloc(32, 7)]).toString('base64');
const keysGo = (key) => `package selfupdate\n\nconst PublicKey = "${key}"\n`;
const SELF_KEYS = keysGo(TEST_KEY);
const EMPTY_KEYS = keysGo('');

test('smoke: the in-app update line is the one updatepath.js writes', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'updatepath.js'), 'utf8');
  assert.ok(src.includes(`'${SELF_LINE}'`), 'updatepath.js logs a different line: update scripts/smoke.js UPDATE_LINES.selfupdate');
});

test('smoke: the expected mechanism follows keys.go, or --expect-updates', () => {
  assert.equal(expectedUpdates(undefined, '/repo', () => SELF_KEYS), 'selfupdate');
  assert.equal(expectedUpdates(undefined, '/repo', () => EMPTY_KEYS), 'electron-updater');
  assert.equal(expectedUpdates('electron-updater', '/repo', () => SELF_KEYS), 'electron-updater', 'the flag wins');
  assert.equal(expectedUpdates('selfupdate', '/repo', () => { throw new Error('not read'); }), 'selfupdate', 'keys.go not read with the flag');
  for (const bad of ['', 'in-app', 'SELFUPDATE', 'any']) {
    assert.throws(() => expectedUpdates(bad, '/repo', () => SELF_KEYS), /--expect-updates/, JSON.stringify(bad));
  }
  assert.throws(() => expectedUpdates(undefined, '/repo', () => { throw new Error('ENOENT'); }), /ENOENT/, 'an unreadable keys.go fails');
  assert.throws(() => expectedUpdates(undefined, '/repo', () => 'package selfupdate\n'), /no line const PublicKey/);
  // This checkout: the release builds carry the maintainer's key (update-key, 2026-10-27).
  const repoKeys = fs.readFileSync(path.join(__dirname, '..', '..', '..', 'internal', 'selfupdate', 'keys.go'), 'utf8');
  assert.equal(expectedUpdates(undefined), /^const PublicKey = ""/m.test(repoKeys) ? 'electron-updater' : 'selfupdate');
});

test('smoke: no update line yet → keep waiting', () => {
  for (const expect of ['selfupdate', 'electron-updater']) {
    assert.equal(updateMechanism('', { updatesOn: false, expect }), null);
    assert.equal(updateMechanism(READY, { updatesOn: true, expect }), null);
    assert.equal(updateMechanism(null, { updatesOn: false, expect }), null);
  }
  assert.throws(() => updateMechanism(SELF, { updatesOn: false }), /expect must be one of/);
});

test('smoke: a build with the built-in update key passes with or without LECTUREFLOW_UPDATE_REPO', () => {
  // pack-dry 2026-10-27: with the key the shell never loads electron-updater,
  // and the old check (only the electron-updater lines) timed out.
  for (const updatesOn of [false, true]) {
    assert.deepEqual(updateMechanism(SELF, { updatesOn, expect: 'selfupdate' }), { mechanism: 'selfupdate', problem: '' }, `updatesOn ${updatesOn}`);
  }
  assert.match(updateMechanism(SELF + NONE, { updatesOn: false, expect: 'selfupdate' }).problem, /names both the in-app update and electron-updater/);
  // The key went missing from a build that should have it: loud.
  assert.match(updateMechanism(NONE, { updatesOn: false, expect: 'selfupdate' }).problem, /no built-in update key.*expected in-app updates/);
  assert.match(updateMechanism(REPO, { updatesOn: true, expect: 'selfupdate' }).problem, /no built-in update key.*expected in-app updates/);
});

test('smoke: without the key the electron-updater line must match LECTUREFLOW_UPDATE_REPO', () => {
  const expect = 'electron-updater';
  assert.deepEqual(updateMechanism(NONE, { updatesOn: false, expect }), { mechanism: 'electron-updater', problem: '' });
  assert.deepEqual(updateMechanism(REPO, { updatesOn: true, expect }), { mechanism: 'electron-updater', problem: '' });
  assert.match(updateMechanism(NONE, { updatesOn: true, expect }).problem, /no update repository is configured, but LECTUREFLOW_UPDATE_REPO is set/);
  assert.match(updateMechanism(REPO, { updatesOn: false, expect }).problem, /an update repository is configured, but LECTUREFLOW_UPDATE_REPO is not set/);
  assert.match(updateMechanism(NONE + REPO, { updatesOn: true, expect }).problem, /both that an update repository is configured and that none is/);
  assert.match(updateMechanism(UNUSABLE, { updatesOn: true, expect }).problem, /not usable/, 'fails at once instead of timing out');
  assert.match(updateMechanism(SELF, { updatesOn: false, expect }).problem, /has a built-in update key.*expected electron-updater/);
  // Both problems are reported together.
  assert.match(updateMechanism(REPO, { updatesOn: false, expect: 'selfupdate' }).problem, /LECTUREFLOW_UPDATE_REPO is not set; .*expected in-app updates/);
});

test('smoke: the app gets its own data dir and no browser extension endpoint', () => {
  const saved = { ...process.env };
  try {
    Object.assign(process.env, {
      LECTUREFLOW_SERVER: 'x.js', LECTUREFLOW_RESOURCES: 'r', ELECTRON_RUN_AS_NODE: '1', LECTUREFLOW_E2E_ALLOW_DEBUG: '1',
    });
    delete process.env.LECTUREFLOW_NM_ENDPOINT;
    const dir = path.join(path.parse(process.cwd()).root, 'lf-smoke-x');
    const user = path.join(path.parse(process.cwd()).root, 'lf-smoke-x-user');
    const env = smokeEnv(dir, user);
    assert.equal(env.LECTUREFLOW_DATA_DIR, dir);
    // Off: the service would otherwise register the browsers' native
    // messaging host of this temporary build over an installed LectureFlow's.
    assert.equal(env.LECTUREFLOW_NM_ENDPOINT, 'off');
    for (const k of ['LECTUREFLOW_SERVER', 'LECTUREFLOW_RESOURCES', 'ELECTRON_RUN_AS_NODE', 'LECTUREFLOW_E2E_ALLOW_DEBUG']) assert.equal(env[k], undefined, k);
    process.env.LECTUREFLOW_NM_ENDPOINT = '\\\\.\\pipe\\lf-smoke-test';
    assert.equal(smokeEnv(dir, user).LECTUREFLOW_NM_ENDPOINT, '\\\\.\\pipe\\lf-smoke-test', 'an endpoint the caller chose is kept');
  } finally {
    for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
    Object.assign(process.env, saved);
  }
});

test('smoke: the app gets no model service key from the caller\'s environment (T27 P4)', () => {
  const saved = { ...process.env };
  try {
    // made-up values; they replace whatever the caller's shell holds for the test
    const keys = ['DEEPSEEK_API_KEY', 'OPENAI_API_KEY', 'ANTHROPIC_API_KEY', 'GEMINI_API_KEY', 'ZHIPUAI_API_KEY', 'Some_Service_Api_Key'];
    for (const k of keys) process.env[k] = 'sk-smoke-test-not-a-key';
    process.env.LF_SMOKE_API_KEYRING = 'kept';
    process.env.LF_SMOKE_KEEP = 'kept';
    const dir = path.join(path.parse(process.cwd()).root, 'lf-smoke-x');
    const user = path.join(path.parse(process.cwd()).root, 'lf-smoke-x-user');
    const env = smokeEnv(dir, user);
    // presence only: a failing check never prints a value
    for (const k of keys) assert.ok(!Object.prototype.hasOwnProperty.call(env, k), `${k} reaches the app`);
    assert.ok(!Object.keys(env).some((k) => /_api_key$/i.test(k)), 'a variable ending in _API_KEY reaches the app');
    // the rest of the environment is passed on as before
    assert.equal(env.LF_SMOKE_KEEP, 'kept');
    assert.equal(env.LF_SMOKE_API_KEYRING, 'kept', 'only names ending in _API_KEY are dropped');
    assert.equal(env.LECTUREFLOW_DATA_DIR, dir);
  } finally {
    for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
    Object.assign(process.env, saved);
  }
});

test('smoke: the app\'s per-user folders lie in the run\'s temporary root, never the real account\'s (T14)', () => {
  const saved = { ...process.env };
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-smoke-test-'));
  try {
    // the real account's folders (made up): none of them may reach the app
    const real = path.join(path.parse(process.cwd()).root, 'Users', 'someone', 'AppData');
    Object.assign(process.env, {
      LOCALAPPDATA: path.join(real, 'Local'), APPDATA: path.join(real, 'Roaming'), TEMP: path.join(real, 'Local', 'Temp'), TMP: path.join(real, 'Local', 'Temp'),
    });
    const d = smokeDirs('lf-smoke-', tmp);
    assert.equal(path.dirname(d.root), tmp);
    assert.ok(path.basename(d.root).startsWith('lf-smoke-'));
    const env = smokeEnv(d.dataDir, d.userRoot);
    assert.equal(env.LECTUREFLOW_DATA_DIR, d.dataDir);
    for (const k of ['LECTUREFLOW_DATA_DIR', 'LOCALAPPDATA', 'APPDATA', 'TEMP', 'TMP']) {
      const rel = path.relative(d.root, env[k]);
      assert.ok(rel && !rel.startsWith('..') && !path.isAbsolute(rel), `${k} lies below the run's root: ${env[k]}`);
      assert.ok(fs.statSync(env[k]).isDirectory(), `${k} exists`);
      assert.ok(!env[k].startsWith(real), `${k} is not the real account's`);
    }
    assert.notEqual(env.LOCALAPPDATA, env.APPDATA);
    assert.equal(env.TEMP, env.TMP);
    const rel = path.relative(env.LECTUREFLOW_DATA_DIR, env.LOCALAPPDATA);
    assert.ok(rel.startsWith('..'), 'the per-user folders are not in the data folder (the service refuses a trust folder there)');
    assert.throws(() => smokeEnv(d.dataDir), /userRoot/, 'no run without its own per-user folders');
    assert.throws(() => smokeEnv(d.dataDir, 'relative'), /userRoot/);
    // removing the root removes everything
    fs.rmSync(d.root, { recursive: true, force: true });
    assert.deepEqual(fs.readdirSync(tmp), []);
  } finally {
    for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
    Object.assign(process.env, saved);
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

test('smoke: a temporary root that cannot be removed is a warning, not the run\'s result (SEC29 低-2)', () => {
  const warnings = [];
  const busy = () => {
    const err = new Error('EBUSY: resource busy or locked, rmdir');
    err.code = 'EBUSY';
    throw err;
  };
  assert.doesNotThrow(() => removeRoot(path.join(os.tmpdir(), 'lf-smoke-x'), busy, (m) => warnings.push(m)));
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /could not remove the temporary folder .*lf-smoke-x.*EBUSY/);
  // removed: no warning
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-smoke-test-'));
  fs.writeFileSync(path.join(tmp, 'f'), 'x');
  removeRoot(tmp, undefined, (m) => warnings.push(m));
  assert.equal(fs.existsSync(tmp), false);
  assert.equal(warnings.length, 1);
});
