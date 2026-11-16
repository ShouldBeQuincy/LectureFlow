// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The in-app update in the shell (SPEC-ext M5), end to end on the
// unpackaged app with the fake service. LECTUREFLOW_TEST_SELFUPDATE (only
// honoured when unpackaged) describes a pretend installation in a temporary
// folder and a mark file: programs (installer, the restored version) are
// recorded there instead of started. Nothing is installed, no registry is
// touched.
//
//   - a pending update is installed before the service starts; the app quits
//   - ... only when the service program's -selfupdate-verify vouches for it,
//     from the private copy it makes; not while other accounts can change
//     the data folder (sec-update)
//   - an applying.json the shell did not record is ignored
//   - a freshly installed version that becomes healthy: result "installed"
//   - one that fails twice: the database and the previous version are put back
//   - the previous version reports "rolled_back"
//   - electron-updater is loaded only while in-app updates are not configured
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const base = require('@playwright/test');
const { test, expect, waitForApp, fakeState, DESKTOP, REPO, FAKE } = require('./fixtures');

const VERSION = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8')).version;
const FAKE_UPDATER = path.join(DESKTOP, 'test', 'fixtures', 'fake-updater.js');
const sha = (s) => crypto.createHash('sha256').update(s).digest('hex');
const readJ = (p) => JSON.parse(fs.readFileSync(p, 'utf8'));

/** bump(v, n): the version n minor releases after (n < 0: before). */
function bump(v, n) {
  const [a, b] = v.split('.').map(Number);
  return `${a}.${b + n}.0`;
}

/** world(): a data dir with updates/, a database, and a pretend installed copy. */
function world() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-e2e-su-'));
  const dataDir = path.join(root, 'data');
  const updates = path.join(dataDir, 'updates');
  fs.mkdirSync(updates, { recursive: true });
  fs.writeFileSync(path.join(dataDir, 'lectureflow.db'), 'db as it is');
  const target = path.join(root, 'Programs', 'LectureFlow');
  fs.mkdirSync(path.join(target, 'resources'), { recursive: true });
  fs.writeFileSync(path.join(target, 'LectureFlow.exe'), 'electron');
  fs.writeFileSync(path.join(target, 'resources', 'app.asar'), 'asar');
  const mark = path.join(root, 'mark.jsonl');
  const hook = JSON.stringify({ install: { platform: 'windows-x64', kind: 'installer', target, exe: 'LectureFlow.exe' }, mark });
  // recordingOps: the trust file in <root>/private, the private copies in <root>/tmp
  return { root, dataDir, updates, target, mark, hook, private: path.join(root, 'private'), tmp: path.join(root, 'tmp') };
}

/** record(w, from, to): the trust file of an installation from → to this shell started. */
function record(w, from, to) {
  fs.mkdirSync(w.private, { recursive: true });
  fs.writeFileSync(path.join(w.private, 'selfupdate-trust.json'), JSON.stringify({
    schema: 1, install: { version: to, from_version: from, target: w.target, kind: 'installer', started_at: new Date().toISOString() }, backups: {},
  }));
}

/** isCopy(w, file, name): a private copy the service program made of the package name. */
function isCopy(w, file, name) {
  return path.dirname(path.dirname(file)) === w.tmp && /^lectureflow-update-\d+$/.test(path.basename(path.dirname(file))) && path.basename(file) === name;
}

function pkg(w, version, name, content) {
  fs.mkdirSync(path.join(w.updates, version), { recursive: true });
  fs.writeFileSync(path.join(w.updates, version, name), content);
  return { file: `${version}/${name}`, size: Buffer.byteLength(content), sha256: sha(content) };
}

function marks(w) {
  if (!fs.existsSync(w.mark)) return [];
  return fs.readFileSync(w.mark, 'utf8').trim().split('\n').filter(Boolean).map((l) => JSON.parse(l));
}

function cleanEnv() {
  const env = { ...process.env };
  for (const k of Object.keys(env)) {
    if (/^(LECTUREFLOW_|FAKE_|ELECTRON_RUN_AS_NODE$)|_API_KEY$/i.test(k)) delete env[k];
  }
  return env;
}

/**
 * start(w, env) → { app, exited }: the unpackaged shell on w's data dir.
 * exited resolves with the exit code when the app quits by itself.
 */
async function start(w, env = {}) {
  const app = await base._electron.launch({
    args: [DESKTOP, '--lang=en-US'],
    env: {
      ...cleanEnv(),
      LECTUREFLOW_DATA_DIR: w.dataDir,
      LECTUREFLOW_SERVER: FAKE,
      LECTUREFLOW_RESOURCES: REPO,
      LECTUREFLOW_NM_ENDPOINT: 'off',
      LECTUREFLOW_TEST_SELFUPDATE: w.hook,
      ...env,
    },
    cwd: DESKTOP,
    timeout: 60_000,
  });
  const proc = app.process();
  const exited = new Promise((resolve) => {
    if (proc.exitCode !== null) resolve(proc.exitCode);
    else proc.once('exit', (code) => resolve(code));
  });
  return { app, exited };
}

const apps = [];
const worlds = [];
test.afterEach(async () => {
  for (const a of apps.splice(0)) await a.close().catch(() => {});
  for (const w of worlds.splice(0)) fs.rmSync(w.root, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
});

async function launched(w, env) {
  const r = await start(w, env);
  apps.push(r.app);
  return r;
}

function newWorld() {
  const w = world();
  worlds.push(w);
  return w;
}

test('a pending update is installed before the service starts, then the app quits', async () => {
  test.setTimeout(90_000);
  const w = newWorld();
  const next = bump(VERSION, 1);
  const p = pkg(w, next, `LectureFlow-Setup-${next}-x64.exe`, 'installer bytes');
  fs.writeFileSync(path.join(w.updates, 'pending.json'), JSON.stringify({
    schema: 1, version: next, from_version: VERSION, channel: 'stable', platform: 'windows-x64', kind: 'installer', ...p,
    manifest: `${next}/update.json`, signature: `${next}/update.json.minisig`, key_id: '0000000000000000', origin: 'file', verified_at: '2026-11-02T12:00:00Z',
  }));
  const prev = { schema: 1, version: VERSION, platform: 'windows-x64', kind: 'installer', created_at: '2026-11-02T12:00:00Z' };
  fs.writeFileSync(path.join(w.updates, 'previous.json'), JSON.stringify(prev));

  const verifyLog = path.join(w.root, 'verify.jsonl');
  const { exited } = await launched(w, { FAKE_VERIFY_LOG: verifyLog });
  expect(await exited).toBe(0);

  // The service program checked it first (-selfupdate-verify, the running
  // version) and copied it into a private folder; then the installer runs
  // from that copy (sec-update M3), silently for this user, /D= last.
  const m = marks(w);
  expect(m.map((x) => x.op)).toEqual(['verify', 'spawn']);
  expect(m[0]).toMatchObject({ dir: w.updates, version: VERSION, previous: false, copyTo: w.tmp });
  expect(fs.readFileSync(verifyLog, 'utf8').trim().split('\n').map((l) => JSON.parse(l))).toEqual([{ dir: w.updates, app_version: VERSION, previous: false, copy: w.tmp }]);
  expect(isCopy(w, m[1].file, `LectureFlow-Setup-${next}-x64.exe`)).toBe(true);
  expect(fs.readFileSync(m[1].file, 'utf8')).toBe('installer bytes');
  expect(m[1].args).toEqual(['--updated', '/S', '--force-run', `/D=${w.target}`]);
  expect(m[1].verbatim).toBe(true);
  // the shell recorded the installation it started in its profile
  expect(readJ(path.join(w.private, 'selfupdate-trust.json')).install).toMatchObject({ version: next, from_version: VERSION, target: w.target });
  // The service never started (no task can have run).
  expect(fs.existsSync(path.join(w.dataDir, 'fake-state.json'))).toBe(false);
  // Previous version kept, database kept, applying.json in place of pending.json.
  expect(fs.readFileSync(path.join(w.updates, 'backup', VERSION, 'app', 'resources', 'app.asar'), 'utf8')).toBe('asar');
  expect(fs.readFileSync(path.join(w.updates, 'backup', VERSION, 'db', 'lectureflow.db'), 'utf8')).toBe('db as it is');
  expect(readJ(path.join(w.updates, 'previous.json'))).toEqual({ ...prev, backup: `backup/${VERSION}` });
  expect(fs.existsSync(path.join(w.updates, 'pending.json'))).toBe(false);
  expect(readJ(path.join(w.updates, 'applying.json'))).toMatchObject({ version: next, from_version: VERSION, phase: 'installing', tries: 0, target: w.target });
});

for (const [verdict, kept] of [['invalid', false], ['not_configured', true], ['untrusted', true]]) {
  test(`the service program's check says "${verdict}": not installed, the service starts as usual`, async () => {
    test.setTimeout(90_000);
    const w = newWorld();
    const next = bump(VERSION, 1);
    const p = pkg(w, next, `LectureFlow-Setup-${next}-x64.exe`, 'installer bytes');
    fs.writeFileSync(path.join(w.updates, 'pending.json'), JSON.stringify({
      schema: 1, version: next, from_version: VERSION, channel: 'stable', platform: 'windows-x64', kind: 'installer', ...p,
      manifest: `${next}/update.json`, signature: `${next}/update.json.minisig`, key_id: '0000000000000000', origin: 'file', verified_at: '2026-11-02T12:00:00Z',
    }));
    const prev = { schema: 1, version: VERSION, platform: 'windows-x64', kind: 'installer', created_at: '2026-11-02T12:00:00Z' };
    fs.writeFileSync(path.join(w.updates, 'previous.json'), JSON.stringify(prev));

    const { app } = await launched(w, { FAKE_SELFUPDATE_VERIFY: verdict });
    const page = await app.firstWindow();
    await waitForApp(page);

    expect(marks(w).map((x) => x.op)).toEqual(['verify']); // no installer, no restore program
    expect(fs.existsSync(path.join(w.updates, 'pending.json'))).toBe(kept);
    expect(fs.existsSync(path.join(w.updates, 'applying.json'))).toBe(false);
    expect(fs.existsSync(path.join(w.updates, 'backup'))).toBe(false);
    expect(fs.existsSync(path.join(w.updates, p.file))).toBe(true);
    expect(readJ(path.join(w.updates, 'previous.json'))).toEqual(prev);
    const r = readJ(path.join(w.updates, 'result.json'));
    expect(r).toMatchObject({ version: next, from: VERSION, outcome: 'failed' });
    expect(r.error).toMatch({ invalid: /selfupdate\.signature_invalid/, not_configured: /no update public key/, untrusted: /other accounts of this computer can change the data folder/ }[verdict]);
    expect(fs.existsSync(w.tmp) ? fs.readdirSync(w.tmp) : []).toEqual([]); // no copy left
    // Written before the service started, so the service reads it at its start.
    expect(fs.statSync(path.join(w.updates, 'result.json')).mtimeMs).toBeLessThan(fs.statSync(path.join(w.dataDir, 'fake-state.json')).mtimeMs);
  });
}

/** applying(w, fields): the state an installation leaves for the new version. */
function applying(w, fields) {
  fs.writeFileSync(path.join(w.updates, 'applying.json'), JSON.stringify({
    schema: 1, platform: 'windows-x64', kind: 'installer', file: 'x/y.exe', size: 1, sha256: 'a'.repeat(64),
    phase: 'installing', tries: 0, target: w.target, exe: 'LectureFlow.exe', started_at: new Date().toISOString(), ...fields,
  }));
}

test('a freshly installed version that becomes healthy: result "installed"', async () => {
  test.setTimeout(90_000);
  const w = newWorld();
  const old = bump(VERSION, -1);
  fs.mkdirSync(path.join(w.updates, 'backup', old, 'db'), { recursive: true });
  fs.writeFileSync(path.join(w.updates, 'backup', old, 'db', 'lectureflow.db'), 'db of the old version');
  fs.writeFileSync(path.join(w.updates, 'previous.json'), JSON.stringify({ schema: 1, version: old, platform: 'windows-x64', kind: 'installer', backup: `backup/${old}`, created_at: '2026-11-02T12:00:00Z' }));
  applying(w, { version: VERSION, from_version: old, db_snapshot: `backup/${old}/db` });
  record(w, old, VERSION);

  const { app } = await launched(w);
  const page = await app.firstWindow();
  await waitForApp(page);
  await expect.poll(() => fs.existsSync(path.join(w.updates, 'applying.json')), { timeout: 20_000 }).toBe(false);
  expect(readJ(path.join(w.updates, 'result.json'))).toMatchObject({ version: VERSION, from: old, outcome: 'installed' });
  expect(fs.existsSync(path.join(w.updates, 'backup', old, 'db'))).toBe(false);
  expect(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8')).toBe('db as it is');
  expect(marks(w)).toEqual([]);
});

test('a freshly installed version that fails twice: database and previous version put back', async () => {
  test.setTimeout(120_000);
  const w = newWorld();
  const old = bump(VERSION, -1);
  const p = pkg(w, old, `LectureFlow-Setup-${old}-x64.exe`, 'old installer bytes');
  fs.writeFileSync(path.join(w.updates, 'previous.json'), JSON.stringify({ schema: 1, version: old, platform: 'windows-x64', kind: 'installer', ...p, created_at: '2026-11-02T12:00:00Z' }));
  fs.mkdirSync(path.join(w.updates, 'backup', old, 'db'), { recursive: true });
  fs.writeFileSync(path.join(w.updates, 'backup', old, 'db', 'lectureflow.db'), 'db of the old version');
  fs.writeFileSync(path.join(w.dataDir, 'lectureflow.db-wal'), 'wal of the new version');
  applying(w, { version: VERSION, from_version: old, db_snapshot: `backup/${old}/db` });
  record(w, old, VERSION);

  // The fake service fails every start with a database error.
  const { exited } = await launched(w, { FAKE_START_FAIL_TIMES: '10' });
  expect(await exited).toBe(0);
  expect(fakeState(w.dataDir).attempts).toBe(2); // two starts in a row, no third

  // The service program vouches for the previous installer
  // (-selfupdate-previous) and copies it; the rollback runs that copy.
  const m = marks(w);
  expect(m.map((x) => x.op)).toEqual(['verify', 'spawn']);
  expect(m[0]).toMatchObject({ dir: w.updates, version: VERSION, previous: true, copyTo: w.tmp });
  expect(isCopy(w, m[1].file, `LectureFlow-Setup-${old}-x64.exe`)).toBe(true);
  expect(fs.readFileSync(m[1].file, 'utf8')).toBe('old installer bytes');
  expect(m[1].args).toEqual(['--updated', '/S', '--force-run', `/D=${w.target}`]);
  const ap = readJ(path.join(w.updates, 'applying.json'));
  expect(ap).toMatchObject({ phase: 'rolling_back', restore: 'file', tries: 2 });
  expect(ap.error).toMatch(/the service stopped while starting \(exit code 1\): open database: database is locked/);
  expect(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8')).toBe('db of the old version');
  expect(fs.existsSync(path.join(w.dataDir, 'lectureflow.db-wal'))).toBe(false);
});

test('a planted applying.json (no record in the profile) is ignored: nothing is checked or run', async () => {
  test.setTimeout(90_000);
  const w = newWorld();
  const old = bump(VERSION, -1);
  const p = pkg(w, old, `LectureFlow-Setup-${old}-x64.exe`, 'anything');
  fs.writeFileSync(path.join(w.updates, 'previous.json'), JSON.stringify({ schema: 1, version: old, platform: 'windows-x64', kind: 'installer', ...p, created_at: '2026-11-02T12:00:00Z' }));
  applying(w, { version: VERSION, from_version: old, tries: 5 });

  const { app } = await launched(w);
  const page = await app.firstWindow();
  await waitForApp(page);
  expect(marks(w)).toEqual([]); // nothing checked, nothing run
  expect(fs.existsSync(path.join(w.updates, 'applying.json'))).toBe(false);
  expect(fs.existsSync(path.join(w.updates, 'result.json'))).toBe(false);
  expect(fakeState(w.dataDir).attempts).toBe(1);
});

test('the previous version, started again by the rollback, reports "rolled_back"', async () => {
  test.setTimeout(90_000);
  const w = newWorld();
  const next = bump(VERSION, 1);
  fs.writeFileSync(path.join(w.updates, 'previous.json'), JSON.stringify({ schema: 1, version: VERSION, platform: 'windows-x64', kind: 'installer', created_at: '2026-11-02T12:00:00Z' }));
  applying(w, { version: next, from_version: VERSION, phase: 'rolling_back', restore: 'file', tries: 2, error: 'the service did not answer within 30 seconds' });
  record(w, VERSION, next);

  const { app } = await launched(w);
  const page = await app.firstWindow();
  await waitForApp(page);
  const r = readJ(path.join(w.updates, 'result.json'));
  expect(r).toMatchObject({ version: next, from: VERSION, outcome: 'rolled_back', error: 'the service did not answer within 30 seconds' });
  expect(fs.existsSync(path.join(w.updates, 'applying.json'))).toBe(false);
  // Written before the service started, so the service reads it at its start.
  expect(fs.statSync(path.join(w.updates, 'result.json')).mtimeMs).toBeLessThan(fs.statSync(path.join(w.dataDir, 'fake-state.json')).mtimeMs);
});

for (const [name, selfupdate, loaded] of [
  ['a service without the in-app update (404)', undefined, true],
  ['in-app updates not configured (no public key)', 'no_key', true],
  ['in-app updates configured (public key built in)', 'configured', false],
]) {
  test(`update mechanism: ${name} → electron-updater ${loaded ? 'loaded' : 'not loaded'}`, async () => {
    test.setTimeout(90_000);
    const w = newWorld();
    const loadedMark = path.join(w.root, 'updater-loaded');
    const env = { LECTUREFLOW_FAKE_UPDATER: FAKE_UPDATER, LECTUREFLOW_FAKE_UPDATER_LOADED: loadedMark };
    if (selfupdate) env.FAKE_SELFUPDATE = selfupdate;
    const { app } = await launched(w, env);
    const page = await app.firstWindow();
    await waitForApp(page);
    if (loaded) {
      await expect.poll(() => fs.existsSync(loadedMark), { timeout: 15_000 }).toBe(true);
    } else {
      await expect.poll(() => fakeState(w.dataDir).selfupdateCalls || 0, { timeout: 15_000 }).toBeGreaterThan(0);
      await page.waitForTimeout(6000); // one more poll of the main process
      expect(fs.existsSync(loadedMark)).toBe(false);
      expect(fakeState(w.dataDir).selfupdateCalls).toBe(1); // decided once
    }
  });
}
