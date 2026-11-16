// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The in-app update's Electron side (SPEC-ext "应用内更新", M5;
// internal/selfupdate/README.md "The updates directory"): install a pending
// update at start, keep the previous version, probation of the new version,
// rollback. Everything runs in temporary directories with a recording
// stand-in for the programs (installer, open, ditto, codesign …): nothing
// is installed, no registry is touched.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const F = require('../../src/main/lib/updatefiles');
const {
  SelfUpdateApplier, recordingOps, startFailureReason, testInstallFrom, MAX_TRIES, MAX_REFUNDS, RESTORE_ARG, UPDATED_ARG, HELPER_WAIT_MS,
  DB_BEFORE_ROLLBACK_NAME, otherCopysPending,
} = require('../../src/main/selfupdate');

const sha = (b) => crypto.createHash('sha256').update(b).digest('hex');
const readJ = (p) => JSON.parse(fs.readFileSync(p, 'utf8'));
const NOW = new Date('2026-11-03T08:00:00Z');

/**
 * world({ kind, version }) → a data dir with an updates folder and an
 * installation of `version` (3.1.0) of the given kind in a temporary folder.
 */
function world({ kind = 'installer', version = '3.1.0', legacy = false, defaultPlace = false } = {}) {
  // defaultPlace: the data folder where it is by default, <root>\LectureFlow
  // (root standing for %APPDATA%), with the shell's profile in it
  // (<data>\electron); the root then has no link or short name on its way
  // (the real path of the temporary folder: on macOS /var is a link, on
  // some Windows machines TEMP has an 8.3 name).
  const root = fs.mkdtempSync(path.join(defaultPlace ? fs.realpathSync.native(os.tmpdir()) : os.tmpdir(), 'lf-su-'));
  const dataDir = path.join(root, defaultPlace ? 'LectureFlow' : 'data');
  const updates = path.join(dataDir, 'updates');
  fs.mkdirSync(updates, { recursive: true });
  // the shell's profile, the trust folder outside the data folder (Windows;
  // stands for %LOCALAPPDATA%\LectureFlow\selfupdate) and the temporary
  // folder of the copies. legacy: no trust folder, the trust file in the
  // profile (macOS; and every version before the trust folder).
  const profile = defaultPlace ? path.join(dataDir, 'electron') : path.join(root, 'profile');
  const trustDir = legacy ? null : path.join(root, 'trust');
  const trustFile = legacy ? path.join(profile, F.TRUST_FILE) : path.join(trustDir, F.TRUST_DIR_FILE);
  const tmp = path.join(root, 'tmp');
  fs.mkdirSync(tmp);
  fs.writeFileSync(path.join(dataDir, 'lectureflow.db'), 'db before the update');
  let install;
  if (kind === 'installer') {
    const dir = path.join(root, 'Programs', 'LectureFlow');
    fs.mkdirSync(path.join(dir, 'resources', 'bin'), { recursive: true });
    fs.writeFileSync(path.join(dir, 'LectureFlow.exe'), `electron ${version}`);
    fs.writeFileSync(path.join(dir, 'Uninstall LectureFlow.exe'), 'uninstaller');
    fs.writeFileSync(path.join(dir, 'resources', 'app.asar'), `asar ${version}`);
    fs.writeFileSync(path.join(dir, 'resources', 'bin', 'lectureflow.exe'), `service ${version}`);
    install = { platform: 'windows-x64', kind, kinds: ['installer'], target: dir, exe: 'LectureFlow.exe' };
  } else if (kind === 'portable') {
    const exe = path.join(root, 'Apps', `LectureFlow-${version}-x64-portable.exe`);
    fs.mkdirSync(path.dirname(exe), { recursive: true });
    fs.writeFileSync(exe, `portable ${version}`);
    install = { platform: 'windows-x64', kind, kinds: ['portable'], target: exe, exe: 'LectureFlow.exe' };
  } else {
    const app = path.join(root, 'Applications', 'LectureFlow.app');
    makeApp(app, version);
    install = { platform: 'darwin-arm64', kind: 'zip', kinds: ['zip', 'dmg'], target: app, exe: 'LectureFlow' };
  }
  return { root, dataDir, updates, install, version, profile, trustDir, trustFile, tmp };
}

/** trust(w) → w's trust file ({} when there is none). */
function trust(w) {
  return fs.existsSync(w.trustFile) ? readJ(w.trustFile) : {};
}

/** isCopy(w, file, name): file is a private copy of the package name in w's temporary folder. */
function isCopy(w, file, name) {
  return path.dirname(path.dirname(file)) === w.tmp && /^lectureflow-update-\d+$/.test(path.basename(path.dirname(file))) && path.basename(file) === name;
}

const copies = (w) => fs.readdirSync(w.tmp).filter((n) => n.startsWith('lectureflow-update-'));

/** privateDir(w) → the one private folder in w's temporary folder (a rollback from a backup made it). */
function privateDir(w) {
  const names = copies(w);
  assert.equal(names.length, 1, `one private folder: ${names.join(', ')}`);
  return path.join(w.tmp, names[0]);
}

function makeApp(app, marker) {
  fs.mkdirSync(path.join(app, 'Contents', 'MacOS'), { recursive: true });
  fs.writeFileSync(path.join(app, 'Contents', 'Info.plist'), 'plist');
  fs.writeFileSync(path.join(app, 'Contents', 'MacOS', 'LectureFlow'), `mac ${marker}`);
}

function appMarker(app) {
  return fs.readFileSync(path.join(app, 'Contents', 'MacOS', 'LectureFlow'), 'utf8');
}

/** pkg(w, version, name, content) → the fields of an update.json package, stored in updates/<version>/. */
function pkg(w, version, name, content) {
  const dir = path.join(w.updates, version);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, name), content);
  return { file: `${version}/${name}`, size: Buffer.byteLength(content), sha256: sha(Buffer.from(content)) };
}

function writePending(w, { version = '3.2.0', kind = w.install.kind, platform = w.install.platform, content, name, ...over } = {}) {
  const n = name || (kind === 'installer' ? `LectureFlow-Setup-${version}-x64.exe` : kind === 'portable' ? `LectureFlow-${version}-x64-portable.exe` : `LectureFlow-${version}-arm64-mac.${kind === 'dmg' ? 'dmg' : 'zip'}`);
  const p = pkg(w, version, n, content || `${kind} package ${version}`);
  const pending = {
    schema: 1, version, from_version: w.version, channel: 'stable', platform, kind, ...p,
    manifest: `${version}/update.json`, signature: `${version}/update.json.minisig`, key_id: '8F2D0A6B3C1E4D57', origin: 'download', verified_at: '2026-11-02T12:00:00Z',
    ...over,
  };
  fs.writeFileSync(path.join(w.updates, 'pending.json'), JSON.stringify(pending));
  return pending;
}

function writePrevious(w, { withFile = false, ...over } = {}) {
  const prev = { schema: 1, version: w.version, platform: w.install.platform, kind: w.install.kind, created_at: '2026-11-02T12:00:00Z', ...over };
  if (withFile) {
    const name = w.install.kind === 'installer' ? `LectureFlow-Setup-${w.version}-x64.exe` : w.install.kind === 'portable' ? `LectureFlow-${w.version}-x64-portable.exe` : `LectureFlow-${w.version}-arm64-mac.zip`;
    Object.assign(prev, pkg(w, w.version, name, `${w.install.kind} package ${w.version}`));
  }
  fs.writeFileSync(path.join(w.updates, 'previous.json'), JSON.stringify(prev));
  return prev;
}

/**
 * goVerifies(dir, version, { previous, copyTo }): what `lectureflow
 * -selfupdate-verify dir` answers when the Go side accepts the JSON file
 * (runVerify's result): it checks the package's bytes and, with copyTo,
 * copies it into a new folder there (pending.json, or previous.json with
 * previous).
 */
let copyNo = 0;
function goVerifies(dir, version, { previous = false, copyTo = '', privateDir = false, trustCheck = false } = {}) {
  const say = (code, line) => ({ code, stdout: `${line}\n`, stderr: '', timedOut: false, error: null });
  // -selfupdate-trust-check: the data folder and the trust folder are this account's
  if (trustCheck) return say(0, 'ok trusted');
  if (privateDir) {
    // -selfupdate-private: an empty folder for the shell's copy of a backup
    copyNo += 1;
    const d = path.join(copyTo, `lectureflow-update-${process.pid}${copyNo}`);
    fs.mkdirSync(d);
    return say(0, `ok private=${d}`);
  }
  const p = readJ(path.join(dir, previous ? 'previous.json' : 'pending.json'));
  if (!p.file) return say(1, `invalid selfupdate.not_available: previous.json names no package of ${p.version}`);
  const src = path.join(dir, ...p.file.split('/'));
  if (!fs.existsSync(src)) return say(1, `invalid selfupdate.io_failed: open ${src}: no such file`);
  if (sha(fs.readFileSync(src)) !== p.sha256) return say(1, 'invalid selfupdate.checksum: the package has other bytes');
  let line = `ok version=${p.version} kind=${p.kind} file=${p.file} sha256=${p.sha256}`;
  if (copyTo) {
    copyNo += 1;
    const d = path.join(copyTo, `lectureflow-update-${process.pid}${copyNo}`);
    fs.mkdirSync(d);
    fs.copyFileSync(src, path.join(d, path.posix.basename(p.file)));
    line += ` copy=${path.join(d, path.posix.basename(p.file))}`;
  }
  return say(0, line);
}

/**
 * fakeOps: records every program start; the macOS tools are simulated on
 * the file system (a "zip" or "dmg" holds the marker of the app inside).
 * The update check (-selfupdate-verify) is recorded in `verifies`, not in
 * `calls`; verify(dir, version) → its result (default: the Go side agrees).
 */
function fakeOps({ bundleId = 'org.lectureflow.desktop', codesignFails = false, alive = () => false, verify = goVerifies } = {}) {
  const calls = [];
  const verifies = [];
  const trustDirs = []; // the trust folder each check was told about (null: none)
  let pid = 4000;
  return {
    calls,
    verifies,
    trustDirs,
    verifyPending: async (dir, version, opts = {}) => {
      verifies.push({
        dir, version, applying: fs.existsSync(path.join(dir, 'applying.json')), previous: !!opts.previous,
        ...(opts.privateDir ? { private: true } : {}), ...(opts.trustCheck ? { trustCheck: true } : {}),
      });
      trustDirs.push(opts.trustDir || null);
      return verify(dir, version, opts);
    },
    spawnDetached: async (file, args, opts = {}) => {
      calls.push({ op: 'spawn', file, args, verbatim: !!opts.verbatim });
      pid += 1;
      return pid;
    },
    run: async (file, args) => {
      calls.push({ op: 'run', file: path.basename(file), args });
      const tool = path.basename(file);
      if (tool === 'ditto' && args[0] === '-x') {
        makeApp(path.join(args[3], 'LectureFlow.app'), fs.readFileSync(args[2], 'utf8'));
      } else if (tool === 'ditto') {
        fs.cpSync(args[0], args[1], { recursive: true });
      } else if (tool === 'hdiutil' && args[0] === 'attach') {
        makeApp(path.join(args[args.indexOf('-mountpoint') + 1], 'LectureFlow.app'), fs.readFileSync(args[args.length - 1], 'utf8'));
      } else if (tool === 'plutil') {
        return { stdout: args[1] === 'CFBundleIdentifier' ? `${bundleId}\n` : '3.2.0\n' };
      } else if (tool === 'codesign' && codesignFails) {
        throw new Error('codesign failed: code object is not signed at all');
      }
      return { stdout: '' };
    },
    releaseLock: () => calls.push({ op: 'releaseLock' }),
    pidAlive: (p) => alive(p),
  };
}

function applier(w, { version = w.version, ops = fakeOps(), install = w.install, now = () => NOW } = {}) {
  const logs = [];
  const log = { info: (m) => logs.push(m), warn: (m) => logs.push(m), error: (m) => logs.push(m) };
  const a = new SelfUpdateApplier({ install, currentVersion: version, dataDir: w.dataDir, ops, log, now, privateDir: w.profile, trustDir: w.trustDir, tmpDir: w.tmp });
  return { a, ops, logs };
}

const has = (p) => fs.existsSync(p);
const U = (w, ...p) => path.join(w.updates, ...p);
const cleanup = (w) => fs.rmSync(w.root, { recursive: true, force: true });

// ---- helpers ----

test('versions compare like the Go side (semver precedence)', () => {
  assert.equal(F.compareVersions('3.2.0', '3.1.9'), 1);
  assert.equal(F.compareVersions('3.2.0', '3.2.0+build.7'), 0);
  assert.equal(F.compareVersions('3.2.0-beta.1', '3.2.0'), -1, 'a pre-release is older');
  assert.equal(F.compareVersions('3.2.0-beta.2', '3.2.0-beta.10'), -1, 'numeric identifiers compare as numbers');
  assert.equal(F.compareVersions('3.2.0-1', '3.2.0-alpha'), -1, 'numeric before alphanumeric');
  assert.equal(F.compareVersions('3.2.0-beta', '3.2.0-beta.1'), -1);
  assert.equal(F.compareVersions('3.10.0', '3.9.0'), 1);
  assert.equal(F.parseVersion('v3.1.0'), null, 'no "v"');
  assert.equal(F.parseVersion('3.1'), null);
  assert.equal(F.parseVersion('03.1.0'), null);
  assert.equal(F.sameVersion('3.1.0', '3.1.0'), true);
  assert.equal(F.sameVersion('dev', 'dev'), false, 'only valid versions are the same');
});

test('paths in the JSON files stay inside the updates folder', () => {
  const dir = path.join(os.tmpdir(), 'u');
  assert.equal(F.resolveInside(dir, '3.2.0/a.exe'), path.join(dir, '3.2.0', 'a.exe'));
  for (const bad of ['', '/etc/passwd', 'C:/x', 'c:x', '..', '../x', '3.2.0/../../x', '3.2.0\\a.exe', '\\\\server\\x', 'a\0b']) {
    assert.equal(F.resolveInside(dir, bad), null, bad);
  }
});

test('what this installation is', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-inst-'));
  try {
    const dir = path.join(root, 'LectureFlow');
    fs.mkdirSync(path.join(dir, 'resources'), { recursive: true });
    const exe = path.join(dir, 'LectureFlow.exe');
    const win = (env = {}) => F.describeInstallation({ isPackaged: true, platform: 'win32', arch: 'x64', execPath: exe, env });
    assert.equal(F.describeInstallation({ isPackaged: false, platform: 'win32', arch: 'x64', execPath: exe }), null, 'development: nothing');
    assert.match(win().unsupported, /not set up by its installer/, 'an unpacked folder is not an installation');
    // the portable build unpacks into a folder without an uninstaller
    assert.deepEqual(win({ PORTABLE_EXECUTABLE_FILE: 'D:\\Apps\\LF-portable.exe' }), {
      platform: 'windows-x64', kind: 'portable', kinds: ['portable'], target: 'D:\\Apps\\LF-portable.exe', exe: 'LectureFlow.exe',
    });
    assert.ok(win({ PORTABLE_EXECUTABLE_FILE: 'relative.exe' }).unsupported);
    fs.writeFileSync(path.join(dir, 'Uninstall LectureFlow.exe'), '');
    assert.deepEqual(win(), { platform: 'windows-x64', kind: 'installer', kinds: ['installer'], target: dir, exe: 'LectureFlow.exe' });
    // next to the uninstaller the variable does not count (security audit FB1 L-13)
    assert.deepEqual(win({ PORTABLE_EXECUTABLE_FILE: 'D:\\Apps\\LF-portable.exe' }), { platform: 'windows-x64', kind: 'installer', kinds: ['installer'], target: dir, exe: 'LectureFlow.exe' });
    const mac = F.describeInstallation({ isPackaged: true, platform: 'darwin', arch: 'arm64', execPath: '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow' });
    assert.equal(mac.kind, 'zip');
    assert.deepEqual(mac.kinds, ['zip', 'dmg']);
    assert.equal(mac.target, path.resolve('/Applications/LectureFlow.app'));
    assert.ok(F.describeInstallation({ isPackaged: true, platform: 'darwin', arch: 'x64', execPath: '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow' }).unsupported);
    assert.ok(F.describeInstallation({ isPackaged: true, platform: 'linux', arch: 'x64', execPath: '/opt/lf/lectureflow' }).unsupported);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('NSIS arguments: silent, per user, start the app, /D= last and unquoted', () => {
  assert.deepEqual(F.nsisArguments('C:\\Users\\A B\\AppData\\Local\\Programs\\LectureFlow'), [
    '--updated', '/S', '--force-run', '/D=C:\\Users\\A B\\AppData\\Local\\Programs\\LectureFlow',
  ]);
});

test('startFailureReason and the test hook', () => {
  assert.equal(startFailureReason({ kind: 'timeout' }), 'the service did not answer within 30 seconds');
  assert.equal(startFailureReason({ kind: 'exited', code: 1, reason: 'migration 011: no such table' }), 'the service stopped while starting (exit code 1): migration 011: no such table');
  assert.match(startFailureReason(new Error('boom')), /boom/);
  const hook = JSON.stringify({ install: { platform: 'windows-x64', kind: 'portable', target: path.resolve('x.exe') }, mark: path.resolve('mark.jsonl') });
  assert.equal(testInstallFrom({ LECTUREFLOW_TEST_SELFUPDATE: hook }, true), null, 'never in a packaged app');
  assert.equal(testInstallFrom({ LECTUREFLOW_TEST_SELFUPDATE: hook }, false).install.kind, 'portable');
  assert.equal(testInstallFrom({ LECTUREFLOW_TEST_SELFUPDATE: '{"install":{"kind":"evil"},"mark":"/m"}' }, false), null);
});

// ---- nothing to do ----

test('development (no installation) touches nothing', async () => {
  const w = world();
  try {
    const pending = writePending(w);
    const { a, ops } = applier(w, { install: null });
    assert.deepEqual(await a.atStart(), { action: 'continue' });
    assert.ok(has(U(w, 'pending.json')));
    assert.deepEqual(ops.calls, []);
    assert.equal(pending.version, '3.2.0');
  } finally {
    cleanup(w);
  }
});

test('no pending.json: continue', async () => {
  const w = world();
  try {
    const { a, ops } = applier(w);
    assert.deepEqual(await a.atStart(), { action: 'continue' });
    assert.deepEqual(ops.calls, []);
    assert.equal(has(U(w, 'result.json')), false);
  } finally {
    cleanup(w);
  }
});

// ---- pending.json checks ----

test('a pending update that is not newer is dropped silently (it was installed)', async () => {
  const w = world({ version: '3.2.0' });
  try {
    writePending(w, { version: '3.2.0' });
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'continue');
    assert.equal(has(U(w, 'pending.json')), false);
    assert.equal(has(U(w, 'result.json')), false);
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

for (const [name, over, expect] of [
  ['another schema', { schema: 2 }, /schema 2/],
  ['another platform', { platform: 'darwin-arm64' }, /for darwin-arm64/],
  ['a kind that is none of LectureFlow\'s', { kind: 'msi' }, /needs a "installer" package, not "msi"/],
  ['a kind no Windows copy takes (zip)', { kind: 'zip' }, /needs a "installer" package, not "zip"/],
  ['a kind no Windows copy takes (dmg)', { kind: 'dmg' }, /needs a "installer" package, not "dmg"/],
  ['a path outside the updates folder', { file: '../outside.exe' }, /outside the updates folder/],
  ['a damaged package', { sha256: 'a'.repeat(64) }, /damaged \(SHA-256 mismatch\)/],
  ['a package of another size', { size: 5 }, /has \d+ bytes, not 5/],
  ['a missing package', { file: '3.2.0/missing.exe' }, /missing/],
  ['no valid hash', { sha256: 'XYZ' }, /no valid size or SHA-256/],
]) {
  test(`pending.json with ${name} is dropped and reported`, async () => {
    const w = world();
    try {
      writePending(w, over);
      writePrevious(w);
      const { a, ops } = applier(w);
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(has(U(w, 'pending.json')), false);
      const r = readJ(U(w, 'result.json'));
      assert.equal(r.outcome, 'failed');
      assert.equal(r.version, '3.2.0');
      assert.equal(r.from, '3.1.0');
      assert.match(r.error, expect);
      assert.deepEqual(ops.calls, [], 'nothing was started');
      assert.equal(has(U(w, 'applying.json')), false);
    } finally {
      cleanup(w);
    }
  });
}

test('a copy that cannot update itself leaves a pending update alone: not removed, nothing reported', async () => {
  // (r6: it used to remove pending.json and report "install the new version
  // by hand" — also for the update the installed copy had downloaded into
  // the data folder both share)
  for (const version of ['3.2.0', '3.1.0', '3.0.0']) { // newer than this copy, the same, older
    for (const background of [false, true]) {
      const w = world();
      try {
        writePending(w, { version });
        writePrevious(w);
        const before = snapshot(w.updates);
        const { a, ops, logs } = applier(w, { install: { unsupported: 'this copy of LectureFlow was not set up by its installer; install the new version by hand' } });
        assert.equal((await a.atStart({ background })).action, 'continue', version);
        assert.deepEqual(snapshot(w.updates), before, `${version}: pending.json and its package are as they were, no result.json`);
        assert.deepEqual(ops.calls, [], version);
        assert.deepEqual(ops.verifies, [], version);
        assert.ok(logs.some((l) => /pending\.json is left for the copy that can install it.*cannot update itself/.test(l)), logs.join('\n'));
      } finally {
        cleanup(w);
      }
    }
  }
});

test('a pending update of the other kind of copy (installed / portable) is left alone: not removed, not installed, nothing reported', async () => {
  for (const kind of ['installer', 'portable']) {
    const theirs = kind === 'installer' ? 'portable' : 'installer';
    for (const version of ['3.2.0', '3.1.0', '3.0.0']) { // newer than this copy, the same, older
      for (const background of [false, true]) {
        const w = world({ kind });
        try {
          writePending(w, { version, kind: theirs });
          writePrevious(w, { kind: theirs });
          const before = snapshot(w.updates);
          const { a, ops, logs } = applier(w);
          assert.equal((await a.atStart({ background })).action, 'continue', `${kind} ${version}`);
          assert.deepEqual(snapshot(w.updates), before, `${kind} ${version}: pending.json, its package and previous.json are as they were, no result.json`);
          assert.deepEqual(ops.calls, [], `${kind} ${version}: nothing is run`);
          assert.deepEqual(ops.verifies, [], `${kind} ${version}: the service program is not asked`);
          assert.ok(logs.some((l) => l.includes('pending.json is another copy\'s') && l.includes(`"${theirs}" package`) && l.includes('left alone')), logs.join('\n'));
        } finally {
          cleanup(w);
        }
      }
    }
  }
});

// Whose a pending update is: one rule for the shell (otherCopysPending)
// and the service (internal/selfupdate otherCopys). The same table, row
// for row, is in internal/selfupdate/othercopy_test.go
// (TestOtherCopyKindsTable): change both together. A row: this copy
// (what it installs), pending.json's platform and kind, and whether the
// pending update is another copy's.
const OTHER_COPY_TABLE = [
  ['installer', 'windows-x64', 'installer', false],
  ['installer', 'windows-x64', 'portable', true],
  ['installer', 'windows-x64', 'zip', false],
  ['installer', 'windows-x64', 'dmg', false],
  ['installer', 'windows-x64', 'msi', false],
  ['installer', 'windows-x64', '', false],
  ['installer', 'darwin-arm64', 'portable', false],
  ['installer', 'darwin-arm64', 'zip', false],
  ['portable', 'windows-x64', 'installer', true],
  ['portable', 'windows-x64', 'portable', false],
  ['portable', 'windows-x64', 'zip', false],
  ['portable', 'windows-x64', 'dmg', false],
  ['portable', 'windows-x64', 'msi', false],
  ['portable', 'windows-x64', '', false],
  ['portable', 'darwin-arm64', 'installer', false],
  ['zip', 'darwin-arm64', 'zip', false],
  ['zip', 'darwin-arm64', 'dmg', false],
  ['zip', 'darwin-arm64', 'installer', false],
  ['zip', 'darwin-arm64', 'portable', false],
  ['zip', 'windows-x64', 'installer', false],
  ['zip', 'windows-x64', 'portable', false],
  ['none', 'windows-x64', 'installer', true],
  ['none', 'windows-x64', 'portable', true],
  ['none', 'windows-x64', 'zip', true],
  ['none', 'windows-x64', 'msi', true],
  ['none', 'windows-x64', '', true],
  ['none', 'darwin-arm64', 'zip', true],
  ['none', 'darwin-arm64', 'dmg', true],
];

/** copyInstall(copy) → the installation description of a row's copy. */
function copyInstall(copy) {
  if (copy === 'installer') return { platform: 'windows-x64', kind: 'installer', kinds: ['installer'], target: 'C:\\P\\LectureFlow', exe: 'LectureFlow.exe' };
  if (copy === 'portable') return { platform: 'windows-x64', kind: 'portable', kinds: ['portable'], target: 'D:\\Apps\\LF.exe', exe: 'LectureFlow.exe' };
  if (copy === 'zip') return { platform: 'darwin-arm64', kind: 'zip', kinds: ['zip', 'dmg'], target: '/Applications/LectureFlow.app', exe: 'LectureFlow' };
  // a copy that cannot update itself (the shell tells the service: -no-self-update)
  if (copy === 'none') return { unsupported: 'this copy of LectureFlow was not set up by its installer; install the new version by hand' };
  throw new Error(`unknown copy ${copy}`);
}

test('a copy that cannot update itself says so to the service (-no-self-update); the service gets the kind the shell decided', () => {
  const { buildServerArgs } = require('../../src/main/lib/serverutil');
  const { isPortable } = require('../../src/main/lib/nmhost');
  const base = { port: 5, dataDir: 'd', resourcesDir: 'r', appVersion: 'v', configFile: 'c', locationFile: null };
  assert.ok(!buildServerArgs(base).includes('-no-self-update'));
  assert.ok(!buildServerArgs({ ...base, noSelfUpdate: false, portable: true }).includes('-no-self-update'));
  assert.deepEqual(buildServerArgs({ ...base, installDir: 'D:\\Apps\\LectureFlow', noSelfUpdate: true }).slice(-3), ['-install-dir', 'D:\\Apps\\LectureFlow', '-no-self-update']);
  // main.js decides it from the installation's description, shell.js hands
  // it on (neither loads without Electron: their text is looked at)
  const src = (...p) => fs.readFileSync(path.join(__dirname, '..', '..', ...p), 'utf8');
  assert.match(src('src', 'main', 'main.js'), /noSelfUpdate: !!\(install && install\.unsupported\),/);
  assert.match(src('src', 'main', 'shell.js'), /noSelfUpdate: this\.noSelfUpdate,/);
  assert.match(src('src', 'main', 'shell.js'), /this\.noSelfUpdate = !!noSelfUpdate;/);
  // ... and where (SEC40b (e): a line that is commented out, or stands
  // somewhere else, matches the patterns above too). Comments are taken
  // out, then each line must stand inside the call that uses it.
  const code = (...p) => src(...p).replace(/\/\*[\s\S]*?\*\//g, '').split('\n').filter((l) => !/^\s*\/\//.test(l)).join('\n');
  /** inCall(text, open): the text of the call that starts with `open`, up to its closing bracket. */
  const inCall = (text, open) => {
    const at = text.indexOf(open);
    assert.ok(at >= 0, `no ${open}`);
    let depth = 0;
    for (let i = at + open.length - 1; i < text.length; i++) { // (bounded by the text)
      if ('({['.includes(text[i])) depth += 1;
      else if (')}]'.includes(text[i]) && --depth === 0) return text.slice(at, i + 1);
    }
    throw new Error(`${open} is not closed`);
  };
  const mainJs = code('src', 'main', 'main.js');
  const shellJs = code('src', 'main', 'shell.js');
  // main.js: in the options of the one Shell it makes, after the installation was described
  const newShell = inCall(mainJs, 'new Shell(');
  assert.match(newShell, /\n\s*noSelfUpdate: !!\(install && install\.unsupported\),\n/);
  assert.match(newShell, /\n\s*installDir: installDirOf\(install\),/);
  assert.ok(mainJs.indexOf('const { install, ops } = installationForUpdates();') >= 0
    && mainJs.indexOf('const { install, ops } = installationForUpdates();') < mainJs.indexOf('new Shell('), 'install is the installation\'s description');
  assert.equal(mainJs.split('new Shell(').length, 2, 'one Shell');
  // shell.js: taken by the constructor, kept, and passed in the arguments of the service it starts
  assert.match(inCall(shellJs, 'constructor('), /\bnoSelfUpdate = false\b/);
  const serverProcess = inCall(shellJs, 'new ServerProcess(');
  const argsFor = inCall(serverProcess, 'buildServerArgs(');
  assert.match(serverProcess, /argsFor: \(port\) => buildServerArgs\(/);
  assert.match(argsFor, /\n\s*noSelfUpdate: this\.noSelfUpdate,\n/);
  assert.match(argsFor, /\n\s*installDir: this\.installDir,\n/);
  assert.equal(shellJs.split('buildServerArgs(').length, 2, 'the service\'s arguments are built in one place');
  // the stand-in service of the tests takes the flag like the real one
  assert.match(src('test', 'fixtures', 'fake-server.js'), /BOOL_FLAGS = new Set\(\[[^\]]*'no-self-update'/);

  // What the service is told and what the shell decided, for the same
  // program folder and environment: -portable ⇔ the portable build,
  // -no-self-update ⇔ cannot update itself, neither ⇔ the installer's (or
  // the Mac app). main.js drops PORTABLE_EXECUTABLE_FILE where
  // portableExecutable does not accept it (dropDevEnvironment), before
  // isPortable reads it.
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-kind-'));
  try {
    const installed = path.join(root, 'Installed');
    const unpacked = path.join(root, 'Unpacked');
    for (const d of [installed, unpacked]) fs.mkdirSync(path.join(d, 'resources'), { recursive: true });
    fs.writeFileSync(path.join(installed, 'Uninstall LectureFlow.exe'), '');
    const cases = [
      ['win32', 'x64', path.join(installed, 'LectureFlow.exe'), {}, 'installer'],
      ['win32', 'x64', path.join(installed, 'LectureFlow.exe'), { PORTABLE_EXECUTABLE_FILE: 'D:\\Apps\\LF-portable.exe' }, 'installer'],
      ['win32', 'x64', path.join(unpacked, 'LectureFlow.exe'), { PORTABLE_EXECUTABLE_FILE: 'D:\\Apps\\LF-portable.exe' }, 'portable'],
      ['win32', 'x64', path.join(unpacked, 'LectureFlow.exe'), { PORTABLE_EXECUTABLE_FILE: 'relative.exe' }, 'none'],
      ['win32', 'x64', path.join(unpacked, 'LectureFlow.exe'), {}, 'none'],
      ['darwin', 'arm64', '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow', {}, 'zip'],
      ['darwin', 'arm64', '/opt/odd/LectureFlow', {}, 'none'],
      ['linux', 'x64', '/opt/lf/lectureflow', {}, 'none'],
    ];
    for (const [platform, arch, execPath, env0, want] of cases) {
      const name = `${platform} ${path.basename(path.dirname(execPath))} ${JSON.stringify(env0)}`;
      const env = { ...env0 };
      if (env.PORTABLE_EXECUTABLE_FILE !== undefined && !F.portableExecutable({ isPackaged: true, platform, execPath, env })) delete env.PORTABLE_EXECUTABLE_FILE;
      const install = F.describeInstallation({ isPackaged: true, platform, arch, execPath, env });
      const shell = install.unsupported ? 'none' : install.kind;
      assert.equal(shell, want, name);
      const args = buildServerArgs({ ...base, portable: isPortable({ isPackaged: true, platform, env }), noSelfUpdate: !!(install && install.unsupported) });
      // (internal/selfupdate New: -no-self-update first, then -portable, then the platform's kind)
      const service = args.includes('-no-self-update') ? 'none' : args.includes('-portable') ? 'portable' : platform === 'darwin' ? 'zip' : 'installer';
      assert.equal(service, shell, name);
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('whose a pending update is: the table the service shares', () => {
  for (const [copy, platform, kind, other] of OTHER_COPY_TABLE) {
    assert.equal(otherCopysPending(copyInstall(copy), { platform, kind }), other, `${copy} copy, pending ${platform} ${JSON.stringify(kind)}`);
  }
});

test('a pending update of the other kind of copy stays whatever its age (the service stops waiting for it after 14 days; the shell never removes it)', async () => {
  // (also one dated ahead of the clock: within a day the service still
  // waits for it, further ahead it gives it up; the shell has no rule of
  // time and leaves every one of them)
  for (const verified_at of ['2025-11-03T08:00:00Z', '2026-10-01T00:00:00Z', '2026-11-03T20:00:00Z', '2026-11-05T08:00:00Z', '2027-01-01T00:00:00Z', undefined, 'yesterday']) {
    const w = world();
    try {
      writePending(w, { kind: 'portable', verified_at });
      const before = snapshot(w.updates);
      const { a, ops } = applier(w);
      assert.equal((await a.atStart()).action, 'continue', String(verified_at));
      assert.deepEqual(snapshot(w.updates), before, String(verified_at));
      assert.deepEqual(ops.calls, []);
      assert.deepEqual(ops.verifies, []);
    } finally {
      cleanup(w);
    }
  }
});

test('the copy a pending update belongs to installs it after a copy of the other kind started in between', async () => {
  const w = world({ kind: 'portable' });
  try {
    writePending(w);
    writePrevious(w);
    // the installed copy of this account starts (and stops) first
    const dir = path.join(w.root, 'Programs', 'LectureFlow');
    fs.mkdirSync(path.join(dir, 'resources'), { recursive: true });
    fs.writeFileSync(path.join(dir, 'Uninstall LectureFlow.exe'), 'uninstaller');
    const other = applier(w, { install: { platform: 'windows-x64', kind: 'installer', kinds: ['installer'], target: dir, exe: 'LectureFlow.exe' } });
    assert.equal((await other.a.atStart()).action, 'continue');
    // then the portable one: its update is installed as ever
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(fs.readFileSync(w.install.target, 'utf8'), 'portable package 3.2.0');
    assert.equal(readJ(U(w, 'applying.json')).version, '3.2.0');
    assert.equal(has(U(w, 'pending.json')), false);
    assert.equal(ops.calls.filter((c) => c.op === 'spawn').length, 1);
  } finally {
    cleanup(w);
  }
});

test('a start in the background (browser extension) leaves the pending update for later', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    const { a, ops } = applier(w);
    assert.equal((await a.atStart({ background: true })).action, 'continue');
    assert.ok(has(U(w, 'pending.json')));
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('previous.json that does not describe the running version: not installed', async () => {
  for (const prev of [null, { version: '3.0.0' }, { platform: 'darwin-arm64' }]) {
    const w = world();
    try {
      writePending(w);
      if (prev) writePrevious(w, prev);
      const { a, ops } = applier(w);
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(has(U(w, 'pending.json')), false);
      assert.match(readJ(U(w, 'result.json')).error, /previous\.json does not describe the running version/);
      assert.deepEqual(ops.calls, []);
      assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    } finally {
      cleanup(w);
    }
  }
});

// ---- the service program's check (-selfupdate-verify) ----

test('the service program checks the pending update before anything is backed up or started', async () => {
  for (const kind of ['installer', 'portable', 'zip']) {
    const w = world({ kind });
    try {
      writePending(w);
      writePrevious(w);
      const seen = [];
      const ops = fakeOps({
        verify: (dir, version, opts) => {
          seen.push({ backup: has(U(w, 'backup')), pending: has(U(w, 'pending.json')), copyTo: opts.copyTo });
          return goVerifies(dir, version, opts);
        },
      });
      const { a, logs } = applier(w, { ops });
      assert.equal((await a.atStart()).action, 'exit', kind);
      assert.deepEqual(ops.verifies, [{ dir: w.updates, version: '3.1.0', applying: false, previous: false }], `${kind}: once, on the updates folder, with the running version`);
      assert.deepEqual(seen, [{ backup: false, pending: true, copyTo: w.tmp }], `${kind}: before the backup and before pending.json is consumed; a private copy in the temporary folder`);
      assert.ok(ops.calls.length > 0, `${kind}: installed after the check`);
      assert.ok(logs.some((l) => /the service program verified 3\.2\.0/.test(l)));
      // only the installer still runs from its copy (a later start removes it)
      assert.equal(copies(w).length, kind === 'installer' ? 1 : 0, `${kind}: copies left ${copies(w)}`);
    } finally {
      cleanup(w);
    }
  }
});

test('the check says "invalid" (exit 1): not installed, pending.json removed and reported, nothing else touched', async () => {
  const w = world();
  try {
    const pending = writePending(w);
    const prev = writePrevious(w);
    const stdout = 'invalid selfupdate.signature_invalid: update.json.minisig does not verify (https://mirror.example/u.json?token=SECRET#frag)\n';
    const { a, ops, logs } = applier(w, { ops: fakeOps({ verify: () => ({ code: 1, stdout, stderr: '', timedOut: false, error: null }) }) });
    assert.deepEqual(await a.atStart(), { action: 'continue' });
    assert.equal(has(U(w, 'pending.json')), false);
    const r = readJ(U(w, 'result.json'));
    assert.deepEqual({ version: r.version, from: r.from, outcome: r.outcome }, { version: '3.2.0', from: '3.1.0', outcome: 'failed' });
    assert.match(r.error, /selfupdate\.signature_invalid/);
    assert.match(r.error, /https:\/\/mirror\.example\/u\.json/);
    for (const text of [r.error, ...logs]) assert.ok(!/SECRET|token=|#frag/.test(text), `no query string: ${text}`);
    assert.deepEqual(ops.calls, [], 'nothing was started');
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(has(U(w, 'backup')), false, 'nothing backed up');
    assert.ok(has(U(w, pending.file)), 'the package is left to the service');
    assert.deepEqual(readJ(U(w, 'previous.json')), prev);
  } finally {
    cleanup(w);
  }
});

for (const [name, change] of [
  ['another version', (p) => ({ ...p, version: '3.3.0' })],
  ['another kind', (p) => ({ ...p, kind: 'portable' })],
  ['another file', (p) => ({ ...p, file: '3.2.0/other.exe' })],
  ['another SHA-256', (p) => ({ ...p, sha256: 'f'.repeat(64) })],
]) {
  test(`the check vouches for ${name}: the pending update is dropped`, async () => {
    const w = world();
    try {
      writePending(w);
      writePrevious(w);
      const verify = (dir) => {
        const p = change(readJ(path.join(dir, 'pending.json')));
        return { code: 0, stdout: `ok version=${p.version} kind=${p.kind} file=${p.file} sha256=${p.sha256}\n`, stderr: '', timedOut: false, error: null };
      };
      const { a, ops } = applier(w, { ops: fakeOps({ verify }) });
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(has(U(w, 'pending.json')), false);
      assert.match(readJ(U(w, 'result.json')).error, /the update check vouched for .*, not the pending 3\.2\.0/);
      assert.deepEqual(ops.calls, []);
      assert.equal(has(U(w, 'backup')), false);
    } finally {
      cleanup(w);
    }
  });
}

for (const [name, run, expect] of [
  ['a build without a public key (exit 2)', { code: 2, stdout: 'not_configured: this build has no update public key\n' }, /no update public key/],
  ['a timeout', { timedOut: true }, /did not finish within 120 seconds/],
  ['other accounts can change the data folder (exit 3)', { code: 3, stdout: 'untrusted_dir: other accounts of this computer can change D:\\LF (Authenticated Users)\n' },
    /other accounts of this computer can change the data folder.*Authenticated Users/],
  ['exit 0 without the private copy', { code: 0, stdout: `ok version=3.2.0 kind=installer file=3.2.0/LectureFlow-Setup-3.2.0-x64.exe sha256=${sha(Buffer.from('installer package 3.2.0'))}\n` },
    /did not copy the package where it was asked to/],
  ['a program that cannot be run', { error: 'spawn C:\\x\\lectureflow.exe ENOENT' }, /could not run: spawn .*ENOENT/],
  ['an older program without the flag (exit 1, no "invalid" line)', { code: 1, stdout: '', stderr: 'ERROR: flag provided but not defined: -selfupdate-verify\n' }, /no usable answer \(exit code 1\): ERROR: flag provided but not defined/],
  ['exit 0 without the "ok" line', { code: 0, stdout: 'ok\n' }, /no usable answer \(exit code 0\)/],
  ['exit 0 with more than one line', { code: 0, stdout: `hello\nok version=3.2.0 kind=installer file=3.2.0/x.exe sha256=${'a'.repeat(64)}\n` }, /no usable answer/],
  ['another exit code', { code: 3, stdout: 'invalid x: y\n' }, /no usable answer \(exit code 3\)/],
  ['a check that throws', new Error('boom'), /could not run: boom/],
]) {
  test(`no verdict from the check (${name}): not installed, pending.json kept`, async () => {
    const w = world();
    try {
      writePending(w);
      const prev = writePrevious(w);
      const verify = () => {
        if (run instanceof Error) throw run;
        return { code: null, stdout: '', stderr: '', timedOut: false, error: null, ...run };
      };
      const { a, ops, logs } = applier(w, { ops: fakeOps({ verify }) });
      assert.deepEqual(await a.atStart(), { action: 'continue' });
      assert.ok(has(U(w, 'pending.json')), 'kept for a later start');
      const r = readJ(U(w, 'result.json'));
      assert.equal(r.outcome, 'failed');
      assert.match(r.error, expect);
      assert.match(r.error, /not installed at this start$/);
      assert.ok(logs.some((l) => /pending\.json is kept/.test(l)));
      assert.deepEqual(ops.calls, []);
      assert.equal(has(U(w, 'applying.json')), false);
      assert.equal(has(U(w, 'backup')), false);
      assert.deepEqual(readJ(U(w, 'previous.json')), prev);
    } finally {
      cleanup(w);
    }
  });
}

test('without an update check program nothing is installed', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    const ops = fakeOps();
    delete ops.verifyPending;
    const { a } = applier(w, { ops });
    assert.equal((await a.atStart()).action, 'continue');
    assert.ok(has(U(w, 'pending.json')));
    assert.match(readJ(U(w, 'result.json')).error, /no update check program/);
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('no check for a start in the background or a pending update that fails the shell\'s own checks', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    const bg = applier(w);
    await bg.a.atStart({ background: true });
    assert.deepEqual(bg.ops.verifies, []);
    writePending(w, { sha256: 'a'.repeat(64) });
    const bad = applier(w);
    await bad.a.atStart();
    assert.deepEqual(bad.ops.verifies, [], 'a damaged package is dropped before the program is run');
  } finally {
    cleanup(w);
  }
});

test('the shell runs the installed service program for the check (main.js wiring)', async () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'main.js'), 'utf8');
  assert.match(src, /electronOps\(\{ app, verifyPending \}\)/);
  assert.match(src, /recordingOps\(hook\.mark, \{ verifyPending \}\)/);
  assert.match(src, /\(updatesDir, version, opts\) => runVerify\(\{ \.\.\.cmd, env: process\.env \}, updatesDir, version, opts\)/);
  // The test hook records the check, then really runs it.
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-rec-'));
  try {
    const mark = path.join(root, 'mark.jsonl');
    const ops = recordingOps(mark, { verifyPending: async (dir, version) => ({ code: 0, stdout: `${dir} ${version}` }) });
    assert.deepEqual(await ops.verifyPending('D:\\data\\updates', '3.1.0'), { code: 0, stdout: 'D:\\data\\updates 3.1.0' });
    const lines = fs.readFileSync(mark, 'utf8').trim().split('\n').map((l) => JSON.parse(l));
    assert.deepEqual(lines.map(({ op, dir, version }) => ({ op, dir, version })), [{ op: 'verify', dir: 'D:\\data\\updates', version: '3.1.0' }]);
    assert.match((await recordingOps(mark).verifyPending('x', '1.0.0')).error, /no update check program/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

// ---- installing: Windows installer ----

test('installer: backs up the installation and the database, then runs NSIS silently for this user', async () => {
  const w = world();
  try {
    const pending = writePending(w);
    const prev = writePrevious(w);
    const { a, ops } = applier(w);
    const r = await a.atStart();
    assert.deepEqual(r, { action: 'exit', version: '3.2.0' });

    // The backup: a full copy (app.asar copied as a file), marked complete.
    const b = U(w, 'backup', '3.1.0');
    assert.equal(fs.readFileSync(path.join(b, 'app', 'resources', 'app.asar'), 'utf8'), 'asar 3.1.0');
    assert.equal(fs.readFileSync(path.join(b, 'app', 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    assert.deepEqual(readJ(path.join(b, 'backup.json')), { schema: 1, version: '3.1.0', kind: 'installer', item: '.', created_at: NOW.toISOString() });
    assert.equal(fs.readFileSync(path.join(b, 'db', 'lectureflow.db'), 'utf8'), 'db before the update');
    // previous.json: Electron adds "backup" and keeps every other field.
    assert.deepEqual(readJ(U(w, 'previous.json')), { ...prev, backup: 'backup/3.1.0' });

    // pending.json became applying.json with the bookkeeping.
    assert.equal(has(U(w, 'pending.json')), false);
    const ap = readJ(U(w, 'applying.json'));
    assert.deepEqual(ap, {
      ...pending, phase: 'installing', tries: 0, target: w.install.target, exe: 'LectureFlow.exe', started_at: NOW.toISOString(),
      db_snapshot: 'backup/3.1.0/db', pid: 4001, pid_at: NOW.toISOString(),
    });

    // The installer: verbatim arguments, /D= last; it closes this app
    // itself. It runs from the service program's private copy (M3).
    assert.equal(ops.calls.length, 1);
    assert.deepEqual({ ...ops.calls[0], file: undefined }, {
      op: 'spawn', file: undefined, args: ['--updated', '/S', '--force-run', `/D=${w.install.target}`], verbatim: true,
    });
    assert.ok(isCopy(w, ops.calls[0].file, 'LectureFlow-Setup-3.2.0-x64.exe'), ops.calls[0].file);
    assert.equal(fs.readFileSync(ops.calls[0].file, 'utf8'), 'installer package 3.2.0');
    assert.equal(has(U(w, 'result.json')), false, 'no result before the new version has started');

    // The trust file in the profile: the installation this shell started and
    // the backup it made (with the hash of its tree).
    const t = trust(w);
    assert.deepEqual(t.install, { version: '3.2.0', from_version: '3.1.0', target: w.install.target, kind: 'installer', started_at: NOW.toISOString() });
    const tree = await F.treeHash(path.join(b, 'app'));
    assert.deepEqual(t.backups['3.1.0'], {
      dir: 'backup/3.1.0', kind: 'installer', item: '.', tree: tree.sha256, files: tree.files, bytes: tree.bytes, created_at: NOW.toISOString(),
    });
  } finally {
    cleanup(w);
  }
});

test('installer: with the previous installer kept by the updater, no copy of the installation is made', async () => {
  const w = world();
  try {
    writePending(w);
    const prev = writePrevious(w, { withFile: true });
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(has(U(w, 'backup', '3.1.0', 'app')), false);
    assert.deepEqual(readJ(U(w, 'previous.json')), prev, 'previous.json untouched');
    assert.ok(has(U(w, 'backup', '3.1.0', 'db', 'lectureflow.db')), 'the database is still kept');
    assert.equal(ops.calls.length, 1);
  } finally {
    cleanup(w);
  }
});

test('installer: a complete backup of the same version is reused while it is a copy of the installation as it is', async () => {
  const w = world();
  const LATER = new Date(NOW.getTime() + 3600 * 1000);
  const LATEST = new Date(NOW.getTime() + 7200 * 1000);
  try {
    writePending(w);
    writePrevious(w);
    await applier(w).a.atStart();
    // The installer did not run (e.g. the computer was switched off):
    // the old version reports it, then a new pending update comes.
    const old = applier(w);
    await old.a.atStart();
    assert.equal(readJ(U(w, 'result.json')).outcome, 'failed');
    writePending(w);
    assert.equal((await applier(w, { now: () => LATER }).a.atStart()).action, 'exit');
    assert.equal(readJ(U(w, 'backup', '3.1.0', 'backup.json')).created_at, NOW.toISOString(), 'the existing complete backup was kept, not copied again');
    assert.equal(trust(w).backups['3.1.0'].created_at, NOW.toISOString());
    assert.equal(has(U(w, 'applying.json')), true);

    // The installation is no longer what the backup holds (here: a file
    // was added): the backup is made again from the installation.
    await applier(w, { now: () => LATER }).a.atStart();
    fs.writeFileSync(path.join(w.install.target, 'marker'), 'new in the installation');
    writePending(w);
    assert.equal((await applier(w, { now: () => LATEST }).a.atStart()).action, 'exit');
    assert.equal(fs.readFileSync(U(w, 'backup', '3.1.0', 'app', 'marker'), 'utf8'), 'new in the installation');
    assert.equal(readJ(U(w, 'backup', '3.1.0', 'backup.json')).created_at, LATEST.toISOString());
    assert.equal(trust(w).backups['3.1.0'].tree, (await F.treeHash(w.install.target)).sha256);
    assert.equal(readJ(U(w, 'previous.json')).backup, 'backup/3.1.0');
  } finally {
    cleanup(w);
  }
});

/**
 * forgeBackup(w): a backup of the running version with its mark, its record
 * in the trust file and previous.json's "backup", all consistent with each
 * other — but holding another program than the installation (what an
 * account could prepare while it could write to a data folder that holds
 * the shell's profile; security review SEC17, 中-3).
 */
async function forgeBackup(w, prevFields = {}) {
  const b = U(w, 'backup', w.version);
  fs.cpSync(w.install.target, path.join(b, 'app'), { recursive: true });
  fs.writeFileSync(path.join(b, 'app', 'LectureFlow.exe'), 'evil exe 3.1.0');
  fs.writeFileSync(path.join(b, 'backup.json'), JSON.stringify({ schema: 1, version: w.version, kind: 'installer', item: '.', created_at: '2026-10-01T00:00:00.000Z' }));
  const tree = await F.treeHash(path.join(b, 'app'));
  fs.mkdirSync(path.dirname(w.trustFile), { recursive: true });
  fs.writeFileSync(w.trustFile, JSON.stringify({
    schema: 1, backups: { [w.version]: { dir: `backup/${w.version}`, kind: 'installer', item: '.', tree: tree.sha256, files: tree.files, bytes: tree.bytes, created_at: '2026-10-01T00:00:00.000Z' } },
  }));
  return writePrevious(w, { backup: `backup/${w.version}`, ...prevFields });
}

test('installer: a recorded backup that is not a copy of the installation is dropped and made again', async () => {
  const w = world();
  try {
    writePending(w);
    await forgeBackup(w);
    const source = await F.treeHash(w.install.target);
    const { a, ops, logs } = applier(w);
    // (it is complete, marked and has its recorded hash)
    assert.ok(await a._backupItem(readJ(U(w, 'previous.json'))));
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(fs.readFileSync(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0', 'the backup was made again from the installation');
    assert.equal(readJ(U(w, 'backup', '3.1.0', 'backup.json')).created_at, NOW.toISOString());
    assert.deepEqual(trust(w).backups['3.1.0'], {
      dir: 'backup/3.1.0', kind: 'installer', item: '.', tree: source.sha256, files: source.files, bytes: source.bytes, created_at: NOW.toISOString(),
    });
    assert.equal(readJ(U(w, 'previous.json')).backup, 'backup/3.1.0');
    assert.ok(logs.some((l) => /the backup of 3\.1\.0 is dropped: it is not a copy of the installation as it is now/.test(l)), logs.join('\n'));
    assert.equal(ops.calls.length, 1);

    // the new version fails: what comes back is the installation that was replaced
    fs.writeFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'electron 3.2.0');
    const old = new Date(NOW.getTime() - HELPER_WAIT_MS - 60000);
    for (const n of copies(w)) fs.utimesSync(path.join(w.tmp, n), old, old);
    const { r } = await failTwice(w);
    assert.deepEqual(r, { action: 'exit' });
    assert.equal(fs.readFileSync(path.join(privateDir(w), 'app', 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
  } finally {
    cleanup(w);
  }
});

test('installer: with the previous installer kept by the updater, a backup that is not a copy of the installation is dropped all the same', async () => {
  const w = world();
  try {
    writePending(w);
    const prev = await forgeBackup(w, { ...pkg(w, '3.1.0', 'LectureFlow-Setup-3.1.0-x64.exe', 'installer package 3.1.0') });
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(has(U(w, 'backup', '3.1.0', 'app')), false, 'the backup is removed, none is made (the installer is there)');
    assert.equal(has(U(w, 'backup', '3.1.0', 'backup.json')), false);
    assert.deepEqual(trust(w).backups, {});
    const { backup, ...rest } = prev;
    assert.equal(backup, 'backup/3.1.0');
    assert.deepEqual(readJ(U(w, 'previous.json')), rest, 'previous.json names no backup any more; its other fields are kept');
    assert.ok(has(U(w, 'backup', '3.1.0', 'db', 'lectureflow.db')), 'the database is still kept');
    assert.equal(ops.calls.length, 1);

    // the installer of 3.1.0 turns out not to verify at the rollback: there is no backup to fall back to
    fs.writeFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'electron 3.2.0');
    const old = new Date(NOW.getTime() - HELPER_WAIT_MS - 60000);
    for (const n of copies(w)) fs.utimesSync(path.join(w.tmp, n), old, old);
    const verify = (dir, v, o) => (o.previous
      ? { code: 1, stdout: 'invalid selfupdate.signature_invalid: update.json.minisig does not verify\n', stderr: '', timedOut: false, error: null }
      : goVerifies(dir, v, o));
    const { s, r } = await failTwice(w, fakeOps({ verify }));
    assert.equal(r.action, 'giveup');
    assert.match(r.error, /no package or backup of 3\.1\.0 to return to/);
    assert.deepEqual(s.ops.calls, []);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
  } finally {
    cleanup(w);
  }
});

test('installer: the backup is recorded with the hash of the installation; a copy that differs is no backup', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    const source = await F.treeHash(w.install.target);
    const { a, ops } = applier(w);
    // a file of the copy is swapped after it was written, before its hash
    // is recorded (security audit M-3)
    const record = a._recordBackup.bind(a);
    a._recordBackup = async (dir, item, src) => {
      assert.equal(src.sha256, source.sha256, 'hashed where it is installed');
      fs.writeFileSync(path.join(dir, 'app', 'LectureFlow.exe'), 'evil exe');
      return record(dir, item, src);
    };
    assert.equal((await a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'failed');
    assert.match(r.error, /could not back up LectureFlow 3\.1\.0: the backup of LectureFlow 3\.1\.0 differs from the installation it was made from/);
    assert.deepEqual(ops.calls, [], 'nothing is installed without a backup to return to');
    assert.equal(has(U(w, 'backup', '3.1.0', 'app')), false, 'the copy is removed');
    assert.deepEqual(trust(w).backups || {}, {});
    assert.equal(trust(w).install, undefined);
    assert.equal(has(U(w, 'pending.json')), false);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(readJ(U(w, 'previous.json')).backup, undefined);
    assert.deepEqual(copies(w), []);
  } finally {
    cleanup(w);
  }
});

test('installer: a failing backup leaves everything as it was', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w);
    // backup/3.1.0 cannot be created: a file is in the way.
    fs.writeFileSync(U(w, 'backup'), 'not a directory');
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'failed');
    assert.match(r.error, /could not back up LectureFlow 3\.1\.0/);
    assert.equal(has(U(w, 'pending.json')), false);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('installer: the installer that cannot be started changes nothing', async () => {
  const w = world();
  try {
    writePending(w);
    writePrevious(w, { withFile: true });
    const ops = fakeOps();
    ops.spawnDetached = async () => {
      throw Object.assign(new Error('spawn EACCES'), { code: 'EACCES' });
    };
    const { a } = applier(w, { ops });
    assert.equal((await a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'failed');
    assert.match(r.error, /could not install 3\.2\.0: spawn EACCES/);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(has(U(w, 'backup', '3.1.0', 'db')), false, 'the database snapshot is dropped');
  } finally {
    cleanup(w);
  }
});

// ---- installing: portable ----

test('portable: the executable is replaced (the running one renamed aside) and the new one started', async () => {
  const w = world({ kind: 'portable' });
  try {
    writePending(w, { content: 'portable 3.2.0' });
    writePrevious(w);
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(fs.readFileSync(w.install.target, 'utf8'), 'portable 3.2.0');
    assert.equal(fs.readFileSync(`${w.install.target}.lf-old`, 'utf8'), 'portable 3.1.0');
    assert.equal(has(`${w.install.target}.lf-new`), false);
    const item = path.basename(w.install.target);
    assert.equal(fs.readFileSync(U(w, 'backup', '3.1.0', 'app', item), 'utf8'), 'portable 3.1.0');
    assert.equal(readJ(U(w, 'backup', '3.1.0', 'backup.json')).item, item);
    assert.equal(readJ(U(w, 'previous.json')).backup, 'backup/3.1.0');
    // The lock is released before the new version starts.
    assert.deepEqual(ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: w.install.target, args: [UPDATED_ARG], verbatim: false }]);
    assert.equal(readJ(U(w, 'applying.json')).phase, 'installing');
  } finally {
    cleanup(w);
  }
});

test('portable: a replacement that fails leaves the executable as it was', async () => {
  const w = world({ kind: 'portable' });
  try {
    writePending(w);
    writePrevious(w, { withFile: true });
    // Something occupies the name the new executable is copied to.
    fs.mkdirSync(`${w.install.target}.lf-new`);
    fs.writeFileSync(path.join(`${w.install.target}.lf-new`, 'x'), 'x');
    const { a, ops } = applier(w);
    // The leftover cleanup at start removes it; make it come back.
    const orig = a._cleanupLeftovers.bind(a);
    a._cleanupLeftovers = async () => {
      await orig();
      fs.mkdirSync(`${w.install.target}.lf-new`, { recursive: true });
      fs.writeFileSync(path.join(`${w.install.target}.lf-new`, 'x'), 'x');
    };
    assert.equal((await a.atStart()).action, 'continue');
    assert.equal(fs.readFileSync(w.install.target, 'utf8'), 'portable 3.1.0');
    assert.equal(readJ(U(w, 'result.json')).outcome, 'failed');
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('a folder that cannot be written (read-only stick, app started from the dmg) gives a readable reason', async () => {
  const p = world({ kind: 'portable' });
  const m = world({ kind: 'zip' });
  const { copyFile, mkdir } = F.fsp;
  try {
    F.fsp.copyFile = async (src, dst) => {
      if (String(dst).endsWith('.lf-new')) throw Object.assign(new Error('EACCES: permission denied'), { code: 'EACCES' });
      return copyFile(src, dst);
    };
    F.fsp.mkdir = async (dir, opts) => {
      if (/\.lf-new-/.test(path.basename(String(dir)))) throw Object.assign(new Error('EROFS: read-only file system'), { code: 'EROFS' });
      return mkdir(dir, opts);
    };
    writePending(p);
    writePrevious(p, { withFile: true });
    assert.equal((await applier(p).a.atStart()).action, 'continue');
    assert.match(readJ(path.join(p.updates, 'result.json')).error, /the folder of LectureFlow-3\.1\.0-x64-portable\.exe cannot be written \(EACCES\); move LectureFlow/);
    assert.equal(fs.readFileSync(p.install.target, 'utf8'), 'portable 3.1.0');

    writePending(m, { content: '3.2.0' });
    writePrevious(m);
    assert.equal((await applier(m).a.atStart()).action, 'continue');
    assert.match(readJ(path.join(m.updates, 'result.json')).error, /the folder of LectureFlow\.app cannot be written \(EROFS\)/);
    assert.equal(appMarker(m.install.target), 'mac 3.1.0');
  } finally {
    F.fsp.copyFile = copyFile;
    F.fsp.mkdir = mkdir;
    cleanup(p);
    cleanup(m);
  }
});

// ---- installing: macOS ----

test('macOS zip: unpacked next to the app, checked, swapped; the old app becomes the backup', async () => {
  const w = world({ kind: 'zip' });
  try {
    writePending(w, { content: '3.2.0' });
    writePrevious(w);
    const before = await F.treeHash(w.install.target);
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(appMarker(w.install.target), 'mac 3.2.0');
    assert.equal(appMarker(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.app')), 'mac 3.1.0');
    assert.deepEqual(readJ(U(w, 'backup', '3.1.0', 'backup.json')), { schema: 1, version: '3.1.0', kind: 'zip', item: 'LectureFlow.app', created_at: NOW.toISOString() });
    assert.equal(readJ(U(w, 'previous.json')).backup, 'backup/3.1.0');
    const tools = ops.calls.map((c) => (c.op === 'run' ? c.file : c.op));
    assert.deepEqual(tools, ['ditto', 'plutil', 'plutil', 'xattr', 'codesign', 'releaseLock', 'spawn']);
    assert.deepEqual(ops.calls.find((c) => c.file === 'xattr').args.slice(0, 2), ['-dr', 'com.apple.quarantine']);
    assert.deepEqual(ops.calls.at(-1), { op: 'spawn', file: '/usr/bin/open', args: ['-n', w.install.target, '--args', UPDATED_ARG], verbatim: false });
    const left = fs.readdirSync(path.dirname(w.install.target));
    assert.deepEqual(left, ['LectureFlow.app'], 'no staging folder left');
    // the recorded hash is the one the app had where it was installed
    assert.equal(trust(w).backups['3.1.0'].tree, before.sha256);
  } finally {
    cleanup(w);
  }
});

test('macOS: an old app that is not what was installed once it lies in the backup folder is not recorded', async () => {
  const w = world({ kind: 'zip' });
  try {
    writePending(w, { content: '3.2.0' });
    writePrevious(w);
    const { a, logs } = applier(w);
    // something changes the app between the swap and the record
    const swap = a._swapApp.bind(a);
    a._swapApp = async (incoming, keepOld) => {
      const kept = await swap(incoming, keepOld);
      fs.writeFileSync(path.join(keepOld, 'Contents', 'MacOS', 'LectureFlow'), 'evil');
      return kept;
    };
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(appMarker(w.install.target), 'mac 3.2.0');
    assert.deepEqual(trust(w).backups, {}, 'no record: the backup is never used');
    assert.ok(logs.some((l) => /recording the backup: the backup of LectureFlow 3\.1\.0 differs from the installation/.test(l)), logs.join('\n'));
    // the new version fails: nothing to go back to
    const { s, r } = await failTwice(w);
    assert.equal(r.action, 'giveup');
    assert.match(r.error, /no package or backup of 3\.1\.0 to return to/);
    assert.equal(appMarker(w.install.target), 'mac 3.2.0');
    assert.deepEqual(s.ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('macOS dmg (offline update): mounted, copied, detached', async () => {
  const w = world({ kind: 'zip' });
  try {
    writePending(w, { kind: 'dmg', content: '3.2.0' });
    writePrevious(w);
    const { a, ops } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
    assert.equal(appMarker(w.install.target), 'mac 3.2.0');
    const hd = ops.calls.filter((c) => c.file === 'hdiutil').map((c) => c.args[0]);
    assert.deepEqual(hd, ['attach', 'detach']);
    assert.ok(ops.calls.find((c) => c.file === 'hdiutil').args.includes('-readonly'));
  } finally {
    cleanup(w);
  }
});

test('macOS: another app in the package or a bad signature is refused; nothing changes', async () => {
  for (const opts of [{ bundleId: 'com.example.other' }, { codesignFails: true }]) {
    const w = world({ kind: 'zip' });
    try {
      writePending(w, { content: '3.2.0' });
      writePrevious(w);
      const { a, ops } = applier(w, { ops: fakeOps(opts) });
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(appMarker(w.install.target), 'mac 3.1.0');
      const r = readJ(U(w, 'result.json'));
      assert.equal(r.outcome, 'failed');
      assert.match(r.error, opts.bundleId ? /holds com\.example\.other, not LectureFlow/ : /codesign failed/);
      assert.deepEqual(fs.readdirSync(path.dirname(w.install.target)), ['LectureFlow.app']);
      assert.equal(ops.calls.some((c) => c.op === 'spawn'), false);
    } finally {
      cleanup(w);
    }
  }
});

// ---- the new version starts ----

/** installed(w, opts) → a world where 3.1.0 installed 3.2.0 (installer kind unless given). */
async function installed({ kind = 'installer', withFile = false, legacy = false, defaultPlace = false } = {}) {
  const w = world({ kind, legacy, defaultPlace });
  writePending(w, { content: kind === 'zip' ? '3.2.0' : `${kind} package 3.2.0` });
  writePrevious(w, { withFile });
  const first = applier(w);
  assert.equal((await first.a.atStart()).action, 'exit');
  // What the installer does: the new version is in place; it touches the database.
  if (kind === 'installer') fs.writeFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'electron 3.2.0');
  fs.writeFileSync(path.join(w.dataDir, 'lectureflow.db'), 'db migrated by 3.2.0');
  fs.writeFileSync(path.join(w.dataDir, 'lectureflow.db-wal'), 'wal of 3.2.0');
  // The installer ran from its private copy and is done: old by the tests'
  // clock (NOW), whatever the real date, so the next start sweeps it.
  const old = new Date(NOW.getTime() - HELPER_WAIT_MS - 60000);
  for (const n of copies(w)) fs.utimesSync(path.join(w.tmp, n), old, old);
  return w;
}

test('new version: probation, then healthy → installed', async () => {
  const w = await installed();
  try {
    const { a, ops } = applier(w, { version: '3.2.0' });
    assert.deepEqual(await a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 });
    assert.equal(readJ(U(w, 'applying.json')).tries, 1);
    assert.equal(readJ(U(w, 'applying.json')).pid, undefined);
    // Written before the service starts, so the service can show it.
    assert.equal(readJ(U(w, 'result.json')).outcome, 'installed');
    assert.equal(a.inProbation, true);
    await a.healthy();
    assert.equal(a.inProbation, false);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(readJ(U(w, 'result.json')), { version: '3.2.0', from: '3.1.0', outcome: 'installed', at: NOW.toISOString() });
    assert.equal(has(U(w, 'backup', '3.1.0', 'db')), false, 'the database snapshot only serves the first start');
    assert.ok(has(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.exe')), 'the previous version stays');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0');
    assert.deepEqual(ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('new version: one failed start is retried, the second puts 3.1.0 back (installer kept by the updater)', async () => {
  const w = await installed({ withFile: true });
  try {
    let s = applier(w, { version: '3.2.0' });
    assert.equal((await s.a.atStart()).action, 'probation');
    assert.deepEqual(await s.a.failed('the service did not answer within 30 seconds'), { action: 'retry' });
    assert.equal(readJ(U(w, 'applying.json')).error, 'the service did not answer within 30 seconds');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0', 'not yet');

    // The shell starts the service once more.
    assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 2 });
    const r = await s.a.failed('the service stopped while starting (exit code 1): migration 011: boom');
    assert.deepEqual(r, { action: 'exit' });

    // The database as it was before the update; the new WAL is gone.
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    assert.equal(has(path.join(w.dataDir, 'lectureflow.db-wal')), false);
    // The previous installer, silently, into the same folder: vouched for by
    // the service program (-selfupdate-previous) and run from its private
    // copy (sec-update M2).
    assert.equal(s.ops.calls.length, 1);
    assert.deepEqual({ ...s.ops.calls[0], file: undefined }, {
      op: 'spawn', file: undefined, args: ['--updated', '/S', '--force-run', `/D=${w.install.target}`], verbatim: true,
    });
    assert.ok(isCopy(w, s.ops.calls[0].file, 'LectureFlow-Setup-3.1.0-x64.exe'), s.ops.calls[0].file);
    assert.deepEqual(s.ops.verifies.map((x) => x.previous), [true]);
    const ap = readJ(U(w, 'applying.json'));
    assert.equal(ap.phase, 'rolling_back');
    assert.equal(ap.restore, 'file');
    assert.equal(ap.error, 'the service stopped while starting (exit code 1): migration 011: boom');

    // 3.1.0 starts again: rolled_back with the reason.
    s = applier(w, { version: '3.1.0' });
    assert.equal((await s.a.atStart()).action, 'continue');
    assert.deepEqual(readJ(U(w, 'result.json')), {
      version: '3.2.0', from: '3.1.0', outcome: 'rolled_back', at: NOW.toISOString(), error: 'the service stopped while starting (exit code 1): migration 011: boom',
    });
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(has(U(w, 'backup', '3.1.0', 'db')), false);
  } finally {
    cleanup(w);
  }
});

// ---- the database as it was when a rollback puts the snapshot back (r6, SEC29b b-1) ----

/** keptDb(w, sfx) → lectureflow.db.lf-before-rollback<sfx> in w's data folder. */
const keptDb = (w, sfx = '') => path.join(w.dataDir, `${DB_BEFORE_ROLLBACK_NAME}${sfx}`);

/** keptLeftovers(w) → the temporary files of keeping or restoring the database left in w's data folder. */
const keptLeftovers = (w) => fs.readdirSync(w.dataDir).filter((n) => /\.lf-new$|\.lf-restore$|\.lf-aside$/.test(n));

/**
 * faultFs(rules) → restore(): F.fsp functions fail where a rule says so.
 * A rule: { op: 'rename' | 'rm' | 'copyFile', when: (first, second) → boolean
 * on the base names of the first two arguments, code, times (how often it
 * fails; default: always) }. Everything else works as usual.
 */
function faultFs(rules) {
  const saved = {};
  for (const op of new Set(rules.map((r) => r.op))) {
    const real = F.fsp[op];
    saved[op] = real;
    F.fsp[op] = async function faulty(...args) {
      const [a, b] = args.map((x) => (typeof x === 'string' ? path.basename(x) : ''));
      for (const r of rules) {
        if (r.op !== op || !r.when(a, b) || r.times === 0) continue;
        if (r.times !== undefined) r.times -= 1;
        throw Object.assign(new Error(`${r.code}: ${op} '${a}'${b ? ` -> '${b}'` : ''}`), { code: r.code });
      }
      return real.apply(this, args);
    };
  }
  return () => Object.assign(F.fsp, saved);
}

/** dbState(w) → what lies under the database's own names: [lectureflow.db, -wal, -shm] contents (null: no such file). */
function dbState(w) {
  return ['', '-wal', '-shm'].map((sfx) => {
    const p = path.join(w.dataDir, `lectureflow.db${sfx}`);
    return fs.existsSync(p) ? fs.readFileSync(p, 'utf8') : null;
  });
}

const DB_NEW = ['db migrated by 3.2.0', 'wal of 3.2.0', null]; // as the failed version left it
const DB_SNAPSHOT = ['db before the update', null, null]; // the snapshot of the update (it has no -wal)

/** rollBack(w, opts) → the applier, its logs and what the second failed start of 3.2.0 answered. */
async function rollBack(w, opts = {}) {
  const s = applier(w, { version: '3.2.0', ...opts });
  assert.equal((await s.a.atStart()).action, 'probation');
  assert.deepEqual(await s.a.failed('the service did not answer within 30 seconds'), { action: 'retry' });
  assert.equal((await s.a.atStart()).action, 'probation');
  s.result = await s.a.failed('the service stopped while starting (exit code 1): migration 011: boom');
  return s;
}

/**
 * failingRename(match) → restore(): F.fsp.rename fails (EIO, not retried)
 * for a destination whose base name is match; every other rename works.
 */
function failingRename(match) {
  const real = F.fsp.rename;
  F.fsp.rename = async function rename(from, to) {
    if (path.basename(String(to)) === match) throw Object.assign(new Error(`EIO: i/o error, rename '${from}' -> '${to}'`), { code: 'EIO' });
    return real.call(this, from, to);
  };
  return () => { F.fsp.rename = real; };
}

test('going back keeps the database as it was, with its -wal, before the snapshot is put back; only one generation', async () => {
  const w = await installed({ withFile: true });
  try {
    assert.equal(DB_BEFORE_ROLLBACK_NAME, 'lectureflow.db.lf-before-rollback');
    // what an earlier rollback kept: replaced; its -shm belongs to no
    // database any more (there is none now) and goes
    fs.writeFileSync(keptDb(w), 'kept by an earlier rollback');
    fs.writeFileSync(keptDb(w, '-wal'), 'earlier wal');
    fs.writeFileSync(keptDb(w, '-shm'), 'earlier shm');
    const s = await rollBack(w);
    assert.deepEqual(s.result, { action: 'exit' });
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    assert.equal(has(path.join(w.dataDir, 'lectureflow.db-wal')), false);
    assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0');
    assert.equal(fs.readFileSync(keptDb(w, '-wal'), 'utf8'), 'wal of 3.2.0', 'SQLite opens the kept database with its log');
    assert.equal(has(keptDb(w, '-shm')), false, 'no file of the earlier generation stays beside the new one');
    assert.deepEqual(fs.readdirSync(w.dataDir).filter((n) => /\.lf-restore$/.test(n)), []);
    assert.ok(s.logs.some((l) => /kept as lectureflow\.db\.lf-before-rollback/.test(l)), s.logs.join('\n'));
    // the rollback itself is what it was
    assert.equal(readJ(U(w, 'applying.json')).phase, 'rolling_back');
    assert.equal(s.ops.calls.length, 1);
  } finally {
    cleanup(w);
  }
});

test('going back goes on when the database cannot be kept: the snapshot is put back, the earlier kept database is not mixed with this one', async () => {
  const w = await installed({ withFile: true });
  let restore = null;
  try {
    for (const sfx of ['', '-wal', '-shm']) fs.writeFileSync(keptDb(w, sfx), `earlier${sfx}`);
    restore = failingRename(DB_BEFORE_ROLLBACK_NAME);
    const s = await rollBack(w);
    restore();
    assert.deepEqual(s.result, { action: 'exit' }, 'the rollback goes on');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    assert.equal(has(path.join(w.dataDir, 'lectureflow.db-wal')), false);
    // the earlier database stays; its -wal and -shm went before the new
    // database was to take its place (never a log beside another database)
    assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'earlier');
    assert.equal(has(keptDb(w, '-wal')), false);
    assert.equal(has(keptDb(w, '-shm')), false);
    assert.deepEqual(keptLeftovers(w), [], 'no temporary copy stays');
    assert.ok(s.logs.some((l) => /could not be kept.*EIO.*going back continues/.test(l)), s.logs.join('\n'));
    assert.ok(s.logs.some((l) => /database snapshot restored/.test(l)));
    assert.equal(readJ(U(w, 'applying.json')).phase, 'rolling_back');
    assert.equal(readJ(U(w, 'applying.json')).db_kept, false);
    assert.equal(s.ops.calls.length, 1);
  } finally {
    if (restore) restore();
    cleanup(w);
  }
});

test('going back goes on when the database cannot even be copied: nothing kept, the earlier kept copy whole, the snapshot put back', async () => {
  const w = await installed({ withFile: true });
  const real = F.fsp.copyFile;
  try {
    for (const sfx of ['', '-wal', '-shm']) fs.writeFileSync(keptDb(w, sfx), `earlier${sfx}`);
    F.fsp.copyFile = async function copyFile(from, to, ...rest) {
      if (path.basename(String(to)).startsWith(DB_BEFORE_ROLLBACK_NAME)) throw Object.assign(new Error('ENOSPC: no space left on device, copyfile'), { code: 'ENOSPC' });
      return real.call(this, from, to, ...rest);
    };
    const s = await rollBack(w);
    F.fsp.copyFile = real;
    assert.deepEqual(s.result, { action: 'exit' });
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    for (const sfx of ['', '-wal', '-shm']) assert.equal(fs.readFileSync(keptDb(w, sfx), 'utf8'), `earlier${sfx}`, sfx);
    assert.deepEqual(keptLeftovers(w), []);
    assert.ok(s.logs.some((l) => /could not be kept.*ENOSPC.*going back continues/.test(l)), s.logs.join('\n'));
  } finally {
    F.fsp.copyFile = real;
    cleanup(w);
  }
});

// SEC40 PROBE-1: the kept copy is there, the snapshot cannot take the
// database's place (a scanner holds the fresh file longer than the retries
// last, an I/O error). The rollback goes on as it always did — and the
// database must be what it was, under its own name, with its log.
test('when the snapshot cannot be put in place the database stays as it was under its own name; no kept copy of it', async () => {
  const w = await installed({ withFile: true });
  let restore = null;
  try {
    restore = failingRename('lectureflow.db');
    const s = await rollBack(w);
    restore();
    assert.deepEqual(s.result, { action: 'exit' }, 'the rollback goes on as before');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db-wal'), 'utf8'), 'wal of 3.2.0');
    assert.equal(has(keptDb(w)), false, 'the database itself is still there: no second copy');
    assert.equal(has(keptDb(w, '-wal')), false);
    assert.deepEqual(keptLeftovers(w), []);
    assert.equal(readJ(U(w, 'applying.json')).db_kept, undefined);
    assert.ok(s.logs.some((l) => /restoring the database snapshot.*EIO/.test(l)), s.logs.join('\n'));
  } finally {
    if (restore) restore();
    cleanup(w);
  }
});

// ---- the log of one database never beside another (r6; AUD2 L1 = SEC40b PB-2) ----
//
// The failed version's -wal (and -shm) are moved aside before the
// snapshot's database takes the place of lectureflow.db, and whatever
// fails is undone. A database with the log of another one is what SQLite
// would silently turn back into the new version's content, or call
// malformed.

/** withSnapshotWal(w): the update's snapshot has a -wal of its own (the old version had not closed cleanly). */
function withSnapshotWal(w) {
  fs.writeFileSync(U(w, 'backup', '3.1.0', 'db', 'lectureflow.db-wal'), 'wal before the update');
}

// AUD2 PROBE-W: another program holds the failed version's -wal (it can
// be neither removed nor renamed).
test('the failed version\'s -wal is in use: the snapshot is not put in place, the database stays whole, the result says so', async () => {
  const w = await installed({ withFile: true });
  let restore = null;
  try {
    const busy = (a) => a === 'lectureflow.db-wal';
    restore = faultFs([{ op: 'rm', when: busy, code: 'EBUSY' }, { op: 'rename', when: busy, code: 'EBUSY' }]);
    const s = await rollBack(w);
    restore();
    assert.deepEqual(dbState(w), DB_NEW, 'never the snapshot beside the failed version\'s log');
    assert.deepEqual(keptLeftovers(w), []);
    assert.equal(has(keptDb(w)), false, 'the database itself is still there: no second copy');
    // the rollback of the program goes on as it always did when the
    // snapshot could not be put back, and now says so
    assert.deepEqual(s.result, { action: 'exit' });
    const ap = readJ(U(w, 'applying.json'));
    assert.equal(ap.phase, 'rolling_back');
    assert.match(ap.error, /^the service stopped while starting \(exit code 1\): migration 011: boom; the database of before the update was not put back \(.*EBUSY.*\): it is as 3\.2\.0 left it$/);
    assert.ok(s.logs.some((l) => /restoring the database snapshot.*EBUSY/.test(l)), s.logs.join('\n'));
    // 3.1.0 starts again: the result carries the reason
    const old = applier(w, { version: '3.1.0' });
    assert.equal((await old.a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'rolled_back');
    assert.match(r.error, /the database of before the update was not put back/);
    assert.deepEqual(dbState(w), DB_NEW);
  } finally {
    if (restore) restore();
    cleanup(w);
  }
});

test('the snapshot\'s own -wal is put beside it; the failed version\'s goes; a stray aside file that cannot be removed is only logged', async () => {
  for (const stuck of [false, true]) {
    const w = await installed({ withFile: true });
    let restore = null;
    try {
      withSnapshotWal(w);
      fs.writeFileSync(path.join(w.dataDir, 'lectureflow.db-shm'), 'shm of 3.2.0');
      if (stuck) restore = faultFs([{ op: 'rm', when: (a) => a === 'lectureflow.db-wal.lf-aside', code: 'EBUSY' }]);
      const s = await rollBack(w);
      if (restore) restore();
      assert.deepEqual(s.result, { action: 'exit' });
      assert.deepEqual(dbState(w), ['db before the update', 'wal before the update', null]);
      assert.deepEqual(keptLeftovers(w), stuck ? ['lectureflow.db-wal.lf-aside'] : []);
      assert.equal(readJ(U(w, 'applying.json')).error, 'the service stopped while starting (exit code 1): migration 011: boom', 'nothing to report');
      if (stuck) assert.ok(s.logs.some((l) => /lectureflow\.db-wal\.lf-aside.*EBUSY/.test(l)), s.logs.join('\n'));
      assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0');
      assert.equal(fs.readFileSync(keptDb(w, '-wal'), 'utf8'), 'wal of 3.2.0');
      assert.equal(fs.readFileSync(keptDb(w, '-shm'), 'utf8'), 'shm of 3.2.0');
    } finally {
      if (restore) restore();
      cleanup(w);
    }
  }
});

test('at every step of putting the snapshot back there is a database under its name, and never beside the log of another', async () => {
  const w = await installed({ withFile: true });
  const saved = {};
  try {
    withSnapshotWal(w);
    const whole = [
      JSON.stringify(['db migrated by 3.2.0', 'wal of 3.2.0']), JSON.stringify(['db migrated by 3.2.0', null]),
      JSON.stringify(['db before the update', 'wal before the update']), JSON.stringify(['db before the update', null]),
    ];
    const bad = [];
    const look = (when) => {
      const st = JSON.stringify(dbState(w).slice(0, 2));
      if (!whole.includes(st)) bad.push(`${when}: ${st}`);
    };
    for (const k of ['rename', 'rm', 'copyFile', 'link', 'unlink', 'writeFile']) {
      const fn = F.fsp[k];
      saved[k] = fn;
      F.fsp[k] = async function watched(...args) {
        const what = `${k} ${args.slice(0, 2).map((x) => path.basename(String(x))).join(' ')}`;
        look(`before ${what}`);
        const r = await fn.apply(this, args);
        look(`after ${what}`);
        return r;
      };
    }
    const s = await rollBack(w);
    Object.assign(F.fsp, saved);
    assert.deepEqual(s.result, { action: 'exit' });
    assert.deepEqual(bad, []);
    assert.deepEqual(dbState(w), ['db before the update', 'wal before the update', null]);
  } finally {
    Object.assign(F.fsp, saved);
    cleanup(w);
  }
});

// Two faults in a row (the undo fails as well, or the snapshot's own log
// cannot follow its database): the database lacks its last log, which
// lies beside it under another name. The rollback stops there and says
// so; applying.json stays, so the next start goes through the rollback
// again, forward, and ends with the whole snapshot.
test('when the undo fails too the rollback stops, reports and is finished by the next start', async () => {
  const cases = [
    ['the log cannot be moved back', false, [
      { op: 'rename', when: (a, b) => b === 'lectureflow.db', code: 'EIO' },
      { op: 'rename', when: (a, b) => a === 'lectureflow.db-wal.lf-aside' && b === 'lectureflow.db-wal', code: 'EIO' },
    ], ['db migrated by 3.2.0', null, null], /lectureflow\.db-wal\.lf-aside/],
    ['the snapshot\'s log cannot follow', true, [
      { op: 'rename', when: (a, b) => a === 'lectureflow.db-wal.lf-restore' && b === 'lectureflow.db-wal', code: 'EIO' },
    ], ['db before the update', null, null], /lectureflow\.db-wal/],
  ];
  for (const [name, snapWal, rules, state, names] of cases) {
    const w = await installed({ withFile: true });
    let restore = null;
    try {
      if (snapWal) withSnapshotWal(w);
      restore = faultFs(rules);
      const s = await rollBack(w);
      restore();
      assert.equal(s.result.action, 'giveup', name);
      assert.match(s.result.error, /going back to 3\.1\.0 stopped/, name);
      assert.match(s.result.error, names, name);
      assert.deepEqual(dbState(w), state, `${name}: a database without its last log, never with another's`);
      assert.deepEqual(s.ops.calls.filter((c) => c.op === 'spawn'), [], `${name}: the previous version is not started`);
      const ap = readJ(U(w, 'applying.json'));
      assert.equal(ap.phase, 'installing', `${name}: not rolling back yet`);
      assert.equal(ap.db_kept, true, name);
      assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0', `${name}: the database as it was is whole in the kept copy`);
      assert.equal(fs.readFileSync(keptDb(w, '-wal'), 'utf8'), 'wal of 3.2.0', name);
      assert.ok(has(U(w, 'backup', '3.1.0', 'db', 'lectureflow.db')), `${name}: the snapshot stays for the next start`);
      const r = readJ(U(w, 'result.json'));
      assert.equal(r.outcome, 'failed', name);
      assert.match(r.error, /going back to 3\.1\.0 stopped/, name);

      // the next start (nothing fails any more) finishes the rollback
      const again = applier(w, { version: '3.2.0' });
      assert.deepEqual(await again.a.atStart(), { action: 'exit' }, name);
      assert.deepEqual(dbState(w), snapWal ? ['db before the update', 'wal before the update', null] : DB_SNAPSHOT, name);
      assert.deepEqual(keptLeftovers(w), [], name);
      assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0', name);
      assert.equal(fs.readFileSync(keptDb(w, '-wal'), 'utf8'), 'wal of 3.2.0', name);
      assert.equal(readJ(U(w, 'applying.json')).phase, 'rolling_back', name);
    } finally {
      if (restore) restore();
      cleanup(w);
    }
  }
});

// The process is killed between two steps: the next start runs the
// rollback again from the top; every step can be done again.
test('a rollback killed between any two steps of putting the snapshot back is finished by the next start', async () => {
  const live = (w, sfx = '') => path.join(w.dataDir, `lectureflow.db${sfx}`);
  const snap = (w, sfx = '') => U(w, 'backup', '3.1.0', 'db', `lectureflow.db${sfx}`);
  const kills = [
    ['after the snapshot was copied beside the database', (w) => {
      fs.copyFileSync(snap(w), live(w, '.lf-restore'));
    }, false],
    ['after the database was kept', (w) => {
      fs.copyFileSync(snap(w), live(w, '.lf-restore'));
    }, true],
    ['after the log was moved aside', (w) => {
      fs.copyFileSync(snap(w), live(w, '.lf-restore'));
      fs.renameSync(live(w, '-wal'), live(w, '-wal.lf-aside'));
    }, true],
    ['after the snapshot\'s database took its place', (w) => {
      fs.renameSync(live(w, '-wal'), live(w, '-wal.lf-aside'));
      fs.copyFileSync(snap(w), live(w));
    }, true],
    ['after the snapshot\'s log was put beside it', (w) => {
      fs.renameSync(live(w, '-wal'), live(w, '-wal.lf-aside'));
      fs.copyFileSync(snap(w), live(w));
      if (has(snap(w, '-wal'))) fs.copyFileSync(snap(w, '-wal'), live(w, '-wal'));
    }, true],
  ];
  for (const snapWal of [false, true]) {
    for (const [name, leave, kept] of kills) {
      const w = await installed({ withFile: true });
      try {
        if (snapWal) withSnapshotWal(w);
        const s = await twoTries(w);
        if (kept) assert.equal(await s.a._keepDb(s.a.applying), true, name);
        leave(w); // ... and the process is gone
        const again = applier(w, { version: '3.2.0' });
        assert.deepEqual(await again.a.atStart(), { action: 'exit' }, name);
        assert.deepEqual(dbState(w), snapWal ? ['db before the update', 'wal before the update', null] : DB_SNAPSHOT, `${name} (snapshot -wal: ${snapWal})`);
        assert.deepEqual(keptLeftovers(w), [], name);
        assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0', `${name}: the kept copy is the database as it was`);
        assert.equal(fs.readFileSync(keptDb(w, '-wal'), 'utf8'), 'wal of 3.2.0', name);
        assert.equal(readJ(U(w, 'applying.json')).phase, 'rolling_back', name);
      } finally {
        cleanup(w);
      }
    }
  }
});

// Item 3: the disk has room for one more copy of the database only. The
// kept copy is a convenience; the snapshot must get back.
test('a disk that is nearly full: the snapshot is put back, the database is not kept', async () => {
  const w = await installed({ withFile: true });
  const real = F.fsp.copyFile;
  try {
    let first = null; // the family of copies made first gets the room
    F.fsp.copyFile = async function copyFile(from, to, ...rest) {
      const base = path.basename(String(to));
      const family = /\.lf-new$/.test(base) ? 'kept' : /\.lf-restore$/.test(base) ? 'snapshot' : null;
      if (family) {
        if (!first) first = family;
        if (family !== first) throw Object.assign(new Error('ENOSPC: no space left on device, copyfile'), { code: 'ENOSPC' });
      }
      return real.call(this, from, to, ...rest);
    };
    const s = await rollBack(w);
    F.fsp.copyFile = real;
    assert.deepEqual(s.result, { action: 'exit' });
    assert.deepEqual(dbState(w), DB_SNAPSHOT, 'the snapshot is back');
    assert.equal(has(keptDb(w)), false);
    assert.deepEqual(keptLeftovers(w), []);
    assert.equal(readJ(U(w, 'applying.json')).db_kept, false);
    assert.equal(readJ(U(w, 'applying.json')).error, 'the service stopped while starting (exit code 1): migration 011: boom');
    assert.ok(s.logs.some((l) => /could not be kept.*ENOSPC.*going back continues/.test(l)), s.logs.join('\n'));
  } finally {
    F.fsp.copyFile = real;
    cleanup(w);
  }
});

test('the database is under its own name at every step of putting the snapshot back', async () => {
  const w = await installed({ withFile: true });
  const saved = {};
  try {
    const live = path.join(w.dataDir, 'lectureflow.db');
    const missing = [];
    for (const k of ['rename', 'rm', 'copyFile', 'link', 'unlink', 'writeFile']) {
      const fn = F.fsp[k];
      saved[k] = fn;
      F.fsp[k] = async function watched(...args) {
        if (!fs.existsSync(live)) missing.push(`before ${k} ${args.slice(0, 2).map((x) => path.basename(String(x))).join(' ')}`);
        const r = await fn.apply(this, args);
        if (!fs.existsSync(live)) missing.push(`after ${k} ${args.slice(0, 2).map((x) => path.basename(String(x))).join(' ')}`);
        return r;
      };
    }
    const s = await rollBack(w);
    Object.assign(F.fsp, saved);
    assert.deepEqual(s.result, { action: 'exit' });
    assert.deepEqual(missing, []);
    assert.equal(fs.readFileSync(live, 'utf8'), 'db before the update');
    assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0');
  } finally {
    Object.assign(F.fsp, saved);
    cleanup(w);
  }
});

/**
 * twoTries(w) → the applier of 3.2.0 after its second start (tries 2 in
 * applying.json, its probation in progress): what a process is when it
 * begins a rollback.
 */
async function twoTries(w) {
  const s = applier(w, { version: '3.2.0' });
  assert.equal((await s.a.atStart()).action, 'probation');
  assert.deepEqual(await s.a.failed('the service did not answer within 30 seconds'), { action: 'retry' });
  assert.equal((await s.a.atStart()).action, 'probation');
  return s;
}

// SEC40 PROBE-2 / PROBE-3: the process is killed inside a rollback; the
// next start begins the rollback again ("rolling_back" is only written
// after the database). What the first run kept is the database as it
// was; the second run must not replace it with what it finds now.
test('a rollback killed after the database was replaced starts over without touching what it kept', async () => {
  const w = await installed({ withFile: true });
  try {
    const s = await twoTries(w);
    assert.equal(await s.a._keepDb(s.a.applying), true);
    // ... the snapshot took the database's place, then the process died:
    // the new version's -wal still lies beside the old database
    fs.copyFileSync(U(w, 'backup', '3.1.0', 'db', 'lectureflow.db'), path.join(w.dataDir, 'lectureflow.db'));
    assert.equal(readJ(U(w, 'applying.json')).db_kept, true);
    assert.equal(readJ(U(w, 'applying.json')).phase, 'installing');

    const again = applier(w, { version: '3.2.0' });
    assert.deepEqual(await again.a.atStart(), { action: 'exit' }, 'the third start goes back at once');
    assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0', 'not the snapshot the first run had put in place');
    assert.equal(fs.readFileSync(keptDb(w, '-wal'), 'utf8'), 'wal of 3.2.0');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    assert.equal(has(path.join(w.dataDir, 'lectureflow.db-wal')), false);
    assert.equal(readJ(U(w, 'applying.json')).phase, 'rolling_back');
  } finally {
    cleanup(w);
  }
});

test('a rollback killed between keeping the database and replacing it starts over and keeps the same', async () => {
  const w = await installed({ withFile: true });
  try {
    const s = await twoTries(w);
    assert.equal(await s.a._keepDb(s.a.applying), true);
    const again = applier(w, { version: '3.2.0' });
    assert.deepEqual(await again.a.atStart(), { action: 'exit' });
    assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0');
    assert.equal(fs.readFileSync(keptDb(w, '-wal'), 'utf8'), 'wal of 3.2.0', 'its log was not removed as the live database\'s');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    assert.equal(has(path.join(w.dataDir, 'lectureflow.db-wal')), false);
  } finally {
    cleanup(w);
  }
});

test('a rollback killed while it was keeping the database starts over from the database, which was never touched', async () => {
  const w = await installed({ withFile: true });
  try {
    await twoTries(w);
    // what a kill in the middle leaves: temporary copies, the earlier
    // generation's database without its -wal; nothing recorded
    fs.writeFileSync(keptDb(w), 'earlier');
    fs.writeFileSync(keptDb(w, '.lf-new'), 'half a copy');
    fs.writeFileSync(keptDb(w, '-wal.lf-new'), 'half a copy');
    assert.equal(readJ(U(w, 'applying.json')).db_kept, undefined);
    const again = applier(w, { version: '3.2.0' });
    assert.deepEqual(await again.a.atStart(), { action: 'exit' });
    assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0');
    assert.equal(fs.readFileSync(keptDb(w, '-wal'), 'utf8'), 'wal of 3.2.0');
    assert.deepEqual(keptLeftovers(w), []);
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
  } finally {
    cleanup(w);
  }
});

test('a rollback that starts over after the snapshot could not be put in place keeps the database it finds, once', async () => {
  // the first run kept nothing in the end (the database stayed under its
  // name); had the process died right then, the next run keeps it anew
  const w = await installed({ withFile: true });
  let restore = null;
  try {
    const s = await twoTries(w);
    restore = failingRename('lectureflow.db');
    await assert.rejects(s.a._restoreDb(s.a.applying), /EIO/);
    restore();
    assert.equal(has(keptDb(w)), false);
    assert.equal(s.a.applying.db_kept, undefined, 'not recorded as kept');
    fs.writeFileSync(U(w, 'applying.json'), JSON.stringify(s.a.applying));
    const again = applier(w, { version: '3.2.0' });
    assert.deepEqual(await again.a.atStart(), { action: 'exit' });
    assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0');
    assert.equal(fs.readFileSync(keptDb(w, '-wal'), 'utf8'), 'wal of 3.2.0');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
  } finally {
    if (restore) restore();
    cleanup(w);
  }
});

test('going back goes on when only the -wal cannot be kept: the database is kept without it, no earlier -wal beside it', async () => {
  const w = await installed({ withFile: true });
  let restore = null;
  try {
    for (const sfx of ['', '-wal', '-shm']) fs.writeFileSync(keptDb(w, sfx), `earlier${sfx}`);
    restore = failingRename(`${DB_BEFORE_ROLLBACK_NAME}-wal`);
    const s = await rollBack(w);
    restore();
    assert.deepEqual(s.result, { action: 'exit' });
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    assert.equal(has(path.join(w.dataDir, 'lectureflow.db-wal')), false, 'the snapshot has no log: the new one goes as before');
    assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'db migrated by 3.2.0');
    assert.equal(has(keptDb(w, '-wal')), false, 'an earlier log is never left beside another database');
    assert.equal(has(keptDb(w, '-shm')), false);
    assert.ok(s.logs.some((l) => /could not be kept.*lectureflow\.db-wal.*EIO.*going back continues/.test(l)), s.logs.join('\n'));
  } finally {
    if (restore) restore();
    cleanup(w);
  }
});

test('without a database snapshot nothing is put back and nothing is kept aside', async () => {
  const w = await installed({ withFile: true });
  try {
    const ap = readJ(U(w, 'applying.json'));
    delete ap.db_snapshot;
    fs.writeFileSync(U(w, 'applying.json'), JSON.stringify(ap));
    fs.writeFileSync(keptDb(w), 'kept by an earlier rollback');
    const s = await rollBack(w);
    assert.deepEqual(s.result, { action: 'exit' });
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db-wal'), 'utf8'), 'wal of 3.2.0');
    assert.equal(fs.readFileSync(keptDb(w), 'utf8'), 'kept by an earlier rollback');
    assert.equal(has(keptDb(w, '-wal')), false);
  } finally {
    cleanup(w);
  }
});

test('new version: a crash during the start counts as a try; the third start goes back at once', async () => {
  const w = await installed({ withFile: true });
  try {
    await applier(w, { version: '3.2.0' }).a.atStart(); // try 1, then the app crashed
    await applier(w, { version: '3.2.0' }).a.atStart(); // try 2, crashed again
    const s = applier(w, { version: '3.2.0' });
    assert.deepEqual(await s.a.atStart(), { action: 'exit' });
    const ap = readJ(U(w, 'applying.json'));
    assert.equal(ap.phase, 'rolling_back');
    assert.match(ap.error, new RegExp(`did not start ${MAX_TRIES} times in a row`));
    assert.equal(s.ops.calls[0].op, 'spawn');
  } finally {
    cleanup(w);
  }
});

// ---- environment failures during the probation (N21, SEC3 T6) ----

test('new version: environment failures do not use up the tries; "Try again" three times does not roll back', async () => {
  const w = await installed({ withFile: true });
  try {
    const s = applier(w, { version: '3.2.0' });
    for (let i = 0; i < 3; i++) {
      // the shell starts the service ("Try again" on the error page) …
      assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 }, `start ${i + 1}`);
      // … which fails with startup.db_busy: shell.js refunds the try
      assert.equal(await s.a.refundTry(), true);
      assert.equal(s.a.inProbation, false, 'this start\'s probation ends');
      const ap = readJ(U(w, 'applying.json'));
      assert.equal(ap.tries, 0);
      assert.equal(ap.phase, 'installing');
      assert.equal(ap.error, undefined, 'the environment failure is not recorded as the reason');
    }
    assert.deepEqual(s.ops.calls, [], 'nothing was rolled back or started');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0', 'the database is not put back');
    // A launch after the app was closed on the error page: still the first try.
    const next = applier(w, { version: '3.2.0' });
    assert.deepEqual(await next.a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 });
    await next.a.healthy();
    assert.equal(readJ(U(w, 'result.json')).outcome, 'installed');
    assert.equal(has(U(w, 'applying.json')), false);
  } finally {
    cleanup(w);
  }
});

test('new version: real failures still roll back after two starts, environment failures in between or not', async () => {
  const w = await installed({ withFile: true });
  try {
    const s = applier(w, { version: '3.2.0' });
    assert.equal((await s.a.atStart()).action, 'probation');
    assert.deepEqual(await s.a.failed('migration 011: boom'), { action: 'retry' });
    // the retry meets a locked database: not counted
    assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 2 });
    assert.equal(await s.a.refundTry(), true);
    assert.equal(readJ(U(w, 'applying.json')).tries, 1);
    assert.equal(readJ(U(w, 'applying.json')).error, 'migration 011: boom', 'the last real failure stays');
    // "Try again": the second real failure rolls back, with its own reason
    assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 2 });
    assert.deepEqual(await s.a.failed('the service did not answer within 30 seconds'), { action: 'exit' });
    const ap = readJ(U(w, 'applying.json'));
    assert.equal(ap.phase, 'rolling_back');
    assert.equal(ap.error, 'the service did not answer within 30 seconds');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    assert.equal(s.ops.calls.length, 1);
  } finally {
    cleanup(w);
  }
});

test('refundTry: never below 0; outside a probation it touches nothing', async () => {
  const w = await installed({ withFile: true });
  try {
    const s = applier(w, { version: '3.2.0' });
    // before atStart: no probation
    const before = fs.readFileSync(U(w, 'applying.json'), 'utf8');
    assert.equal(await s.a.refundTry(), false);
    assert.equal(fs.readFileSync(U(w, 'applying.json'), 'utf8'), before);
    assert.equal(await s.a.atStart().then((r) => r.action), 'probation');
    s.a.applying.tries = 0; // (cannot happen after _beginProbation; the floor holds anyway)
    assert.equal(await s.a.refundTry(), true);
    assert.equal(readJ(U(w, 'applying.json')).tries, 0);
    // a second refund for the same start: the probation is over, nothing changes
    const after = fs.readFileSync(U(w, 'applying.json'), 'utf8');
    assert.equal(await s.a.refundTry(), false);
    assert.equal(fs.readFileSync(U(w, 'applying.json'), 'utf8'), after);
    // after healthy(): nothing to refund, applying.json is gone and stays gone
    assert.equal((await s.a.atStart()).action, 'probation');
    await s.a.healthy();
    assert.equal(await s.a.refundTry(), false);
    assert.equal(has(U(w, 'applying.json')), false);
    // the old version (no probation at all)
    const old = world({ kind: 'installer' });
    try {
      const o = applier(old);
      assert.equal((await o.a.atStart()).action, 'continue');
      assert.equal(await o.a.refundTry(), false);
      assert.deepEqual(fs.readdirSync(old.updates), []);
    } finally {
      cleanup(old);
    }
  } finally {
    cleanup(w);
  }
});

test('the shell refunds the try of an environment failure before the error page (source order)', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'shell.js'), 'utf8');
  const start = src.slice(src.indexOf('async startServer('), src.indexOf('// ---- in-app update'));
  const env = start.indexOf('isEnvironmentFailure(err.failure)');
  assert.ok(env > 0, 'startServer checks for environment failures');
  const refund = start.indexOf('this.applier.refundTry()', env);
  assert.ok(refund > env, 'and refunds the try');
  assert.ok(start.indexOf('await this.applier.refundTry()', env) === refund - 'await '.length, 'before "Try again" can start the service again');
  assert.ok(refund < start.indexOf('fail(page,', env), 'before the error page');
  // only a refunded start is no probation failure (SEC12 M-1: refundTry says no after MAX_REFUNDS)
  assert.match(start.slice(env, start.indexOf('fail(page,', env) + 80), /fail\(page, refunded \? null : startFailureReason\(err\)\)/, 'no probation failure');
  assert.match(start.slice(env, start.indexOf('fail(page,', env)), /if \(refunded\) this\._endProbation\(\);/, 'the probation ends only when refunded');
});

test('environment failures: at most MAX_REFUNDS starts are given back, then they count and roll back (SEC12 M-1)', async () => {
  assert.equal(MAX_REFUNDS, 3);
  const w = await installed({ withFile: true });
  try {
    const s = applier(w, { version: '3.2.0' });
    for (let i = 1; i <= MAX_REFUNDS; i++) {
      assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 }, `start ${i}`);
      assert.equal(await s.a.refundTry(), true, `refund ${i}`);
      assert.equal(readJ(U(w, 'applying.json')).env_refunds, i);
      assert.equal(readJ(U(w, 'applying.json')).tries, 0);
    }
    // the 4th: not given back; the probation goes on and the shell reports it
    assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 });
    assert.equal(await s.a.refundTry(), false);
    assert.equal(s.a.inProbation, true);
    assert.equal(readJ(U(w, 'applying.json')).tries, 1);
    assert.equal(readJ(U(w, 'applying.json')).env_refunds, MAX_REFUNDS);
    assert.deepEqual(await s.a.failed('the service stopped while starting (exit code 1): database is locked'), { action: 'retry' });
    // the 5th: counted again → rolled back, the database snapshot restored
    assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 2 });
    assert.equal(await s.a.refundTry(), false);
    assert.deepEqual(await s.a.failed('the service stopped while starting (exit code 1): database is locked'), { action: 'exit' });
    const ap = readJ(U(w, 'applying.json'));
    assert.equal(ap.phase, 'rolling_back');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');
    assert.equal(s.ops.calls.length, 1);
  } finally {
    cleanup(w);
  }
});

test('env_refunds that is not a count in 0..MAX_REFUNDS counts as used up', async () => {
  for (const [value, refunds] of [[-1, 0], [1e9, 0], [4, 0], [2.5, 0], ['1', 0], [null, 0], [{}, 0], [2, 1], [0, MAX_REFUNDS]]) {
    const w = await installed({ withFile: true });
    try {
      const ap = readJ(U(w, 'applying.json'));
      ap.env_refunds = value;
      fs.writeFileSync(U(w, 'applying.json'), JSON.stringify(ap));
      const s = applier(w, { version: '3.2.0' });
      let given = 0;
      for (let i = 0; i <= MAX_REFUNDS; i++) { // bounded: at most MAX_REFUNDS + 1 starts
        assert.equal((await s.a.atStart()).action, 'probation', `${JSON.stringify(value)}: start ${i + 1}`);
        if (!(await s.a.refundTry())) break;
        given++;
      }
      assert.equal(given, refunds, JSON.stringify(value));
      assert.equal(s.a.inProbation, true, 'the last one counts');
      assert.equal(readJ(U(w, 'applying.json')).tries, 1);
    } finally {
      cleanup(w);
    }
  }
});

test('installer rollback from the backup: a private copy is checked; a copy of the failed version puts it back', async () => {
  const w = await installed();
  try {
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('first');
    await s.a.atStart();
    assert.deepEqual(await s.a.failed('the window did not load the app: ERR_FAILED (-2)'), { action: 'exit' });
    // One private folder (asked of the service program, -selfupdate-private):
    // the checked copy of the backup and the restore program. Nothing of
    // the data folder's backup is run (security audit M-3).
    const priv = privateDir(w);
    const helper = path.join(priv, 'helper', 'LectureFlow.exe');
    assert.deepEqual(s.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: helper, args: [RESTORE_ARG], verbatim: false }]);
    assert.equal(fs.readFileSync(helper, 'utf8'), 'electron 3.2.0', 'the restore program is a copy of the failed version');
    assert.equal(fs.readFileSync(path.join(priv, 'app', 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    assert.equal(fs.readFileSync(path.join(priv, 'app', 'resources', 'app.asar'), 'utf8'), 'asar 3.1.0');
    assert.deepEqual(s.ops.verifies.filter((x) => x.private), [{ dir: w.updates, version: '3.2.0', applying: true, previous: false, private: true }]);
    const t = trust(w);
    assert.deepEqual(t.install.restore, { dir: priv, tree: t.backups['3.1.0'].tree });
    assert.equal((await F.treeHash(path.join(priv, 'app'))).sha256, t.backups['3.1.0'].tree);
    const ap = readJ(U(w, 'applying.json'));
    assert.equal(ap.restore, 'backup');
    assert.equal(ap.pid, 4001);

    // The restore program runs (it is 3.2.0) and replaces the failed
    // installation with the checked copy.
    const h = applier(w, { version: '3.2.0' });
    assert.equal(await h.a.restoreFromBackup({ execPath: helper }), true);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    assert.equal(fs.readFileSync(path.join(w.install.target, 'resources', 'app.asar'), 'utf8'), 'asar 3.1.0');
    assert.deepEqual(fs.readdirSync(path.dirname(w.install.target)), ['LectureFlow'], 'the failed installation is removed');
    assert.deepEqual(h.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: path.join(w.install.target, 'LectureFlow.exe'), args: [UPDATED_ARG], verbatim: false }]);
    assert.equal(readJ(U(w, 'applying.json')).pid, undefined, 'the restored app does not wait for the restore program');

    const old = applier(w, { version: '3.1.0' });
    assert.equal((await old.a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'rolled_back');
    assert.equal(r.error, 'the window did not load the app: ERR_FAILED (-2)');
    assert.ok(has(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.exe')), 'the backup stays');
    assert.equal(trust(w).install, undefined, 'the record (with the restore folder) goes with applying.json');
  } finally {
    cleanup(w);
  }
});

test('the restore program runs only as the recorded copy in the private folder', async () => {
  const w = await installed();
  try {
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('x');
    await s.a.atStart();
    await s.a.failed('y');
    const priv = privateDir(w);
    // another private folder with the same layout
    const other = path.join(w.tmp, 'lectureflow-update-999');
    fs.cpSync(priv, other, { recursive: true });
    for (const execPath of [
      path.join(w.root, 'elsewhere', 'LectureFlow.exe'),
      path.join(U(w, 'backup', '3.1.0', 'app'), 'LectureFlow.exe'), // the backup in the data folder: never run
      path.join(priv, 'app', 'LectureFlow.exe'), // the checked copy is what is restored, not what restores
      path.join(other, 'helper', 'LectureFlow.exe'),
    ]) {
      const h = applier(w, { version: '3.2.0' });
      assert.equal(await h.a.restoreFromBackup({ execPath }), false, execPath);
      assert.deepEqual(h.ops.calls, [], execPath);
    }
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
    // a record that names a folder outside the temporary folder of the copies
    const t = trust(w);
    const moved = path.join(w.root, 'lectureflow-update-1');
    fs.cpSync(priv, moved, { recursive: true });
    t.install.restore.dir = moved;
    fs.writeFileSync(w.trustFile, JSON.stringify(t));
    const h = applier(w, { version: '3.2.0' });
    assert.equal(await h.a.restoreFromBackup({ execPath: path.join(moved, 'helper', 'LectureFlow.exe') }), false);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
  } finally {
    cleanup(w);
  }
});

test('a restore program that cannot replace the installation: the new version reports it', async () => {
  const w = await installed();
  try {
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('x');
    await s.a.atStart();
    await s.a.failed('the service did not answer within 30 seconds');
    const helper = path.join(privateDir(w), 'helper', 'LectureFlow.exe');
    const h = applier(w, { version: '3.2.0' });
    h.a._replaceDir = async () => {
      throw Object.assign(new Error('EBUSY: resource busy or locked'), { code: 'EBUSY' });
    };
    await h.a.restoreFromBackup({ execPath: helper });
    assert.equal(readJ(U(w, 'applying.json')).restore_error, 'EBUSY: resource busy or locked');
    // The failed version is started again and reports the whole story.
    const again = applier(w, { version: '3.2.0' });
    assert.equal((await again.a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'failed');
    assert.equal(r.error, 'the service did not answer within 30 seconds; going back to 3.1.0 failed: EBUSY: resource busy or locked');
    assert.equal(has(U(w, 'applying.json')), false);
  } finally {
    cleanup(w);
  }
});

test('the restore program does not put back a private copy that changed after it was checked', async () => {
  const w = await installed();
  try {
    await failTwice(w);
    const priv = privateDir(w);
    fs.writeFileSync(path.join(priv, 'app', 'resources', 'app.asar'), 'evil asar');
    const h = applier(w, { version: '3.2.0' });
    assert.equal(await h.a.restoreFromBackup({ execPath: path.join(priv, 'helper', 'LectureFlow.exe') }), true);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0', 'the installation is not replaced');
    assert.equal(fs.readFileSync(path.join(w.install.target, 'resources', 'app.asar'), 'utf8'), 'asar 3.1.0');
    assert.match(readJ(U(w, 'applying.json')).restore_error, /the private copy of LectureFlow 3\.1\.0 changed since it was checked/);
    // it starts the failed version again, which reports it
    assert.deepEqual(h.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: path.join(w.install.target, 'LectureFlow.exe'), args: [UPDATED_ARG], verbatim: false }]);
    const again = applier(w, { version: '3.2.0' });
    assert.equal((await again.a.atStart()).action, 'continue');
    assert.match(readJ(U(w, 'result.json')).error, /going back to 3\.1\.0 failed: the private copy of LectureFlow 3\.1\.0 changed/);
  } finally {
    cleanup(w);
  }
});

test('nothing to go back to: the failure is reported and the app keeps running', async () => {
  const w = await installed({ withFile: true });
  try {
    fs.rmSync(U(w, '3.1.0'), { recursive: true });
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('x');
    await s.a.atStart();
    const r = await s.a.failed('the service did not answer within 30 seconds');
    assert.equal(r.action, 'giveup');
    assert.match(r.error, /no package or backup of 3\.1\.0 to return to/);
    assert.equal(readJ(U(w, 'result.json')).outcome, 'failed');
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(s.ops.calls, []);
  } finally {
    cleanup(w);
  }
});

test('portable rollback: the previous executable comes back and is started', async () => {
  const w = await installed({ kind: 'portable' });
  try {
    assert.equal(fs.readFileSync(w.install.target, 'utf8'), 'portable package 3.2.0');
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    assert.equal(has(`${w.install.target}.lf-old`), false, 'the old executable is removed once it has quit');
    await s.a.failed('x');
    await s.a.atStart();
    assert.deepEqual(await s.a.failed('y'), { action: 'exit' });
    assert.equal(fs.readFileSync(w.install.target, 'utf8'), 'portable 3.1.0');
    assert.deepEqual(s.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: w.install.target, args: [UPDATED_ARG], verbatim: false }]);
    // it came from a private copy of the backup, which is gone again; the backup stays
    assert.equal(s.ops.verifies.filter((x) => x.private).length, 1);
    assert.deepEqual(copies(w), []);
    assert.equal(fs.readFileSync(U(w, 'backup', '3.1.0', 'app', path.basename(w.install.target)), 'utf8'), 'portable 3.1.0');
    const old = applier(w, { version: '3.1.0' });
    await old.a.atStart();
    assert.equal(readJ(U(w, 'result.json')).outcome, 'rolled_back');
    assert.equal(has(`${w.install.target}.lf-old`), false);
  } finally {
    cleanup(w);
  }
});

test('macOS rollback: the backed-up app is swapped back', async () => {
  const w = await installed({ kind: 'zip' });
  try {
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.failed('x');
    await s.a.atStart();
    assert.deepEqual(await s.a.failed('y'), { action: 'exit' });
    assert.equal(appMarker(w.install.target), 'mac 3.1.0');
    assert.deepEqual(fs.readdirSync(path.dirname(w.install.target)), ['LectureFlow.app'], 'the failed app is removed');
    // it came from a private copy of the backup, which is gone again; the backup stays
    assert.equal(s.ops.verifies.filter((x) => x.private).length, 1);
    assert.deepEqual(copies(w), []);
    assert.equal(appMarker(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.app')), 'mac 3.1.0');
    assert.deepEqual(s.ops.calls.slice(-2), [{ op: 'releaseLock' }, { op: 'spawn', file: '/usr/bin/open', args: ['-n', w.install.target, '--args', UPDATED_ARG], verbatim: false }]);
  } finally {
    cleanup(w);
  }
});

// ---- what the old version finds ----

test('the installer did not install the new version: reported as failed', async () => {
  const w = await installed();
  try {
    const { a } = applier(w, { version: '3.1.0', now: () => new Date(NOW.getTime() + HELPER_WAIT_MS + 1000) });
    assert.equal((await a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'failed');
    assert.match(r.error, /the installer did not install 3\.2\.0; 3\.1\.0 is still installed/);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(has(U(w, 'backup', '3.1.0', 'db')), false);
  } finally {
    cleanup(w);
  }
});

test('the installer is still running: this start ends (the installer starts the app)', async () => {
  const w = await installed();
  try {
    const { a } = applier(w, { version: '3.1.0', ops: fakeOps({ alive: (p) => p === 4001 }) });
    assert.deepEqual(await a.atStart(), { action: 'exit', why: 'busy' });
    assert.ok(has(U(w, 'applying.json')), 'nothing decided yet');
    // Long after, the same pid counts as gone.
    const late = applier(w, { version: '3.1.0', ops: fakeOps({ alive: () => true }), now: () => new Date(NOW.getTime() + HELPER_WAIT_MS + 1) });
    assert.equal((await late.a.atStart()).action, 'continue');
  } finally {
    cleanup(w);
  }
});

test('applying.json that cannot be read, or names other versions, is dropped', async () => {
  for (const body of ['{not json', JSON.stringify({ schema: 1, version: 'x' }), JSON.stringify({ schema: 1, version: '3.4.0', from_version: '3.3.0', phase: 'installing' })]) {
    const w = world();
    try {
      fs.writeFileSync(U(w, 'applying.json'), body);
      const { a, ops } = applier(w);
      assert.equal((await a.atStart()).action, 'continue');
      assert.equal(has(U(w, 'applying.json')), false);
      assert.deepEqual(ops.calls, []);
    } finally {
      cleanup(w);
    }
  }
});

test('leftovers of earlier installations are removed; other files are not', async () => {
  const p = world({ kind: 'portable' });
  const m = world({ kind: 'zip' });
  const i = world();
  try {
    const dirP = path.dirname(p.install.target);
    fs.writeFileSync(`${p.install.target}.lf-old`, 'x');
    fs.writeFileSync(`${p.install.target}.lf-old-20261103T080000Z`, 'x');
    fs.writeFileSync(path.join(dirP, 'notes.txt'), 'mine');
    await applier(p).a.atStart();
    assert.deepEqual(fs.readdirSync(dirP).sort(), [path.basename(p.install.target), 'notes.txt'].sort());

    const dirM = path.dirname(m.install.target);
    fs.mkdirSync(path.join(dirM, '.LectureFlow.app.lf-old-20261103T080000Z'));
    fs.mkdirSync(path.join(dirM, '.LectureFlow.app.lf-new-3.2.0'));
    fs.mkdirSync(path.join(dirM, 'Other.app'));
    await applier(m).a.atStart();
    assert.deepEqual(fs.readdirSync(dirM).sort(), ['LectureFlow.app', 'Other.app']);

    const dirI = path.dirname(i.install.target);
    fs.mkdirSync(path.join(dirI, 'LectureFlow.lf-failed-20261103T080000Z'));
    fs.mkdirSync(path.join(dirI, 'LectureFlowData'));
    await applier(i).a.atStart();
    assert.deepEqual(fs.readdirSync(dirI).sort(), ['LectureFlow', 'LectureFlowData']);
  } finally {
    cleanup(p);
    cleanup(m);
    cleanup(i);
  }
});

test('only the backup previous.json names is kept', async () => {
  const w = await installed({ withFile: false });
  try {
    fs.mkdirSync(U(w, 'backup', '3.0.0', 'app'), { recursive: true });
    const s = applier(w, { version: '3.2.0' });
    await s.a.atStart();
    await s.a.healthy();
    assert.deepEqual(fs.readdirSync(U(w, 'backup')), ['3.1.0']);
  } finally {
    cleanup(w);
  }
});

test('JSON files are written atomically (no temporary file left behind)', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-json-'));
  try {
    const f = path.join(dir, 'result.json');
    await F.writeJsonAtomic(f, { a: 1 });
    await F.writeJsonAtomic(f, { a: 2 });
    assert.deepEqual(readJ(f), { a: 2 });
    assert.deepEqual(fs.readdirSync(dir), ['result.json']);
    assert.deepEqual(await F.readJson(path.join(dir, 'missing.json')), { exists: false });
    fs.writeFileSync(path.join(dir, 'bad.json'), '[1]');
    assert.match((await F.readJson(path.join(dir, 'bad.json'))).error, /not an object/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('the shell installs only while the service is stopped (source order)', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'shell.js'), 'utf8');
  const start = src.slice(src.indexOf('async startServer('), src.indexOf('// ---- in-app update'));
  assert.ok(start.indexOf('previous.stop(') < start.indexOf('_selfUpdateAtStart('), 'the previous service is stopped first');
  assert.ok(start.indexOf('_selfUpdateAtStart(') < start.indexOf('new ServerProcess('), 'updates are applied before the service starts');
  const failed = src.slice(src.indexOf('async _probationFailed('));
  assert.ok(failed.indexOf('server.stop(') < failed.indexOf('this.applier.failed('), 'the service is stopped before a rollback');
});

// ---- security review sec-update (M2, M3) ----

test('a planted applying.json (no record in the profile) is ignored: nothing is rolled back or run', async () => {
  for (const background of [false, true]) {
    const w = world();
    try {
      // what another account could leave in a data folder it can write:
      // a "failed" installation, previous.json and a backup with a program
      writePrevious(w, { backup: 'backup/3.0.0', version: '3.0.0' });
      fs.mkdirSync(U(w, 'backup', '3.0.0', 'app'), { recursive: true });
      fs.writeFileSync(U(w, 'backup', '3.0.0', 'app', 'LectureFlow.exe'), 'evil');
      fs.writeFileSync(U(w, 'backup', '3.0.0', 'backup.json'), JSON.stringify({ schema: 1, version: '3.0.0', kind: 'installer', item: '.' }));
      fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({
        schema: 1, version: '3.1.0', from_version: '3.0.0', platform: 'windows-x64', kind: 'installer', phase: 'installing', tries: 5,
        target: w.install.target, exe: 'LectureFlow.exe',
      }));
      const { a, ops, logs } = applier(w);
      assert.deepEqual(await a.atStart({ background }), { action: 'continue' });
      assert.deepEqual(ops.calls, [], 'nothing started');
      assert.deepEqual(ops.verifies, []);
      assert.equal(has(U(w, 'applying.json')), false);
      assert.equal(has(U(w, 'result.json')), false);
      assert.ok(logs.some((l) => /ignoring applying\.json.*did not start an installation/.test(l)), logs.join('\n'));
    } finally {
      cleanup(w);
    }
  }
});

/**
 * prepareRollback(w): everything a rollback believes, prepared as a whole
 * for the running version (w.version) by someone who could write to a data
 * folder that holds the shell's profile (security review SEC17, 中-3): an
 * "installation on probation" of the running version in applying.json and
 * in the trust file, a backup of an "earlier version" with another
 * program, its mark, its hash in the trust file and previous.json naming
 * it. Nothing here went through _install.
 */
async function prepareRollback(w, from = '3.1.0') {
  const b = U(w, 'backup', from);
  fs.cpSync(w.install.target, path.join(b, 'app'), { recursive: true });
  fs.writeFileSync(path.join(b, 'app', 'LectureFlow.exe'), 'EVIL exe');
  fs.writeFileSync(path.join(b, 'app', 'resources', 'app.asar'), 'EVIL asar');
  fs.writeFileSync(path.join(b, 'backup.json'), JSON.stringify({ schema: 1, version: from, kind: 'installer', item: '.', created_at: '2026-10-01T00:00:00.000Z' }));
  const tree = await F.treeHash(path.join(b, 'app'));
  fs.mkdirSync(path.dirname(w.trustFile), { recursive: true });
  fs.writeFileSync(w.trustFile, JSON.stringify({
    schema: 1,
    install: { version: w.version, from_version: from, target: w.install.target, kind: 'installer', started_at: '2026-10-01T00:00:00.000Z' },
    backups: { [from]: { dir: `backup/${from}`, kind: 'installer', item: '.', tree: tree.sha256, files: tree.files, bytes: tree.bytes, created_at: '2026-10-01T00:00:00.000Z' } },
  }));
  fs.writeFileSync(U(w, 'previous.json'), JSON.stringify({ schema: 1, version: from, platform: w.install.platform, kind: 'installer', backup: `backup/${from}`, created_at: '2026-10-01T00:00:00.000Z' }));
  fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({
    schema: 1, version: w.version, from_version: from, channel: 'stable', platform: w.install.platform, kind: 'installer',
    file: `${w.version}/x.exe`, size: 1, sha256: 'a'.repeat(64), phase: 'installing', tries: 0, target: w.install.target, exe: 'LectureFlow.exe',
    started_at: '2026-10-01T00:00:00.000Z',
  }));
}

/**
 * voidRollback(w, { only }): what the service does to the rollback state
 * when "Restrict access" finds that other accounts could change the data
 * folder (internal/selfupdate/void.go VoidRollback): the profile's trust
 * file (where the record is in the legacy layout: macOS, and versions
 * before the trust folder; a trust folder outside the data folder is never
 * touched), then applying.json, updates/backup and previous.json's
 * "backup". only: 'record' stops after the first step. The tests that
 * prepare the record itself (prepareRollback) use the legacy layout: with
 * a trust folder another account cannot write the record at all.
 */
function voidRollback(w, { only } = {}) {
  fs.rmSync(path.join(w.profile, F.TRUST_FILE), { force: true }); // (Go removes the profile's record only)
  if (only === 'record') return;
  fs.rmSync(U(w, 'applying.json'), { force: true });
  fs.rmSync(U(w, 'backup'), { recursive: true, force: true });
  if (has(U(w, 'previous.json'))) {
    const { backup, ...rest } = readJ(U(w, 'previous.json'));
    fs.writeFileSync(U(w, 'previous.json'), JSON.stringify(rest));
  }
}

const installedFiles = (w) => [
  fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'),
  fs.readFileSync(path.join(w.install.target, 'resources', 'app.asar'), 'utf8'),
];

test('a prepared rollback state: once the service has removed it, nothing is rolled back, checked or run', async () => {
  // Why it has to be removed: left as it is (the folder made private by
  // hand, say), the shell believes it — two failed starts put the prepared
  // program into the installation's place.
  const believed = world({ version: '3.2.0', legacy: true });
  try {
    await prepareRollback(believed);
    const { s, r } = await failTwice(believed);
    assert.deepEqual(r, { action: 'exit' });
    const helper = path.join(privateDir(believed), 'helper', 'LectureFlow.exe');
    assert.deepEqual(s.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: helper, args: [RESTORE_ARG], verbatim: false }]);
    assert.equal(await applier(believed, { version: '3.2.0' }).a.restoreFromBackup({ execPath: helper }), true);
    assert.deepEqual(installedFiles(believed), ['EVIL exe', 'EVIL asar']);
  } finally {
    cleanup(believed);
  }

  for (const only of [undefined, 'record']) {
    const w = world({ version: '3.2.0', legacy: true });
    try {
      await prepareRollback(w);
      voidRollback(w, { only });
      // every later start (bounded): no probation, so no failure can count
      for (let i = 0; i < 3; i++) {
        const { a, ops } = applier(w);
        assert.deepEqual(await a.atStart(), { action: 'continue' }, `start ${i} (${only || 'all'})`);
        assert.equal(a.inProbation, false);
        assert.deepEqual(await a.failed('the service stopped while starting (exit code 1): config.toml: invalid'), { action: 'none' });
        assert.equal(await a.refundTry(), false);
        assert.deepEqual(ops.calls, [], 'nothing is run');
        assert.deepEqual(ops.verifies, [], 'nothing is asked of the service program');
      }
      assert.deepEqual(installedFiles(w), ['electron 3.2.0', 'asar 3.2.0']);
      assert.equal(has(U(w, 'applying.json')), false);
      assert.equal(has(U(w, 'result.json')), false, 'no result of an update that never was');
      assert.deepEqual(copies(w), []);
      // the restore program, started by hand, does nothing either
      const h = applier(w);
      assert.equal(await h.a.restoreFromBackup({ execPath: path.join(w.tmp, 'lectureflow-update-1', 'helper', 'LectureFlow.exe') }), false);
      assert.deepEqual(h.ops.calls, []);
    } finally {
      cleanup(w);
    }
  }
});

test('the rollback state removed during a real probation: the update stays, a later failure is not rolled back, the next update backs up again', async () => {
  // healthy after the removal: installed as usual
  const w = await installed({ legacy: true });
  try {
    const s = applier(w, { version: '3.2.0' });
    assert.equal((await s.a.atStart()).action, 'probation');
    voidRollback(w);
    await s.a.healthy();
    assert.equal(readJ(U(w, 'result.json')).outcome, 'installed');
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal((await applier(w, { version: '3.2.0' }).a.atStart()).action, 'continue');
    // the next update: a new backup, made from the installation
    const next = { ...w, version: '3.2.0' };
    writePending(next, { version: '3.3.0' });
    writePrevious(next);
    const { a, ops } = applier(next);
    assert.deepEqual(await a.atStart(), { action: 'exit', version: '3.3.0' });
    assert.equal(ops.calls.length, 1);
    assert.equal(trust(w).backups['3.2.0'].tree, (await F.treeHash(w.install.target)).sha256);
    assert.equal(readJ(U(w, 'previous.json')).backup, 'backup/3.2.0');
    assert.equal(fs.readFileSync(U(w, 'backup', '3.2.0', 'app', 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
  } finally {
    cleanup(w);
  }

  // failing after the removal: no rollback to what can no longer be vouched for
  const f = await installed({ legacy: true });
  try {
    const s = applier(f, { version: '3.2.0' });
    assert.equal((await s.a.atStart()).action, 'probation');
    voidRollback(f);
    assert.deepEqual(await s.a.failed('the service did not answer within 30 seconds'), { action: 'retry' });
    for (let i = 0; i < 3; i++) { // (bounded) the next starts find no record of the installation
      const n = applier(f, { version: '3.2.0' });
      assert.deepEqual(await n.a.atStart(), { action: 'continue' }, `later start ${i}`);
      assert.deepEqual(await n.a.failed('the service did not answer within 30 seconds'), { action: 'none' });
      assert.deepEqual(n.ops.calls, []);
      assert.deepEqual(n.ops.verifies, []);
    }
    assert.equal(fs.readFileSync(path.join(f.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
    assert.equal(fs.readFileSync(path.join(f.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0', 'the database is not put back');
    assert.equal(has(U(f, 'applying.json')), false);
  } finally {
    cleanup(f);
  }
});

test('SEC12 x T-SEC-04: environment failures given back MAX_REFUNDS times, then the data folder is not trusted: no rollback, the error, no loop', async () => {
  const w = await installed({ withFile: true });
  try {
    // -selfupdate-previous is the first thing a rollback asks: it must be
    // the one that says "untrusted" (exit 3), or another path is tested
    const untrusted = { code: 3, stdout: 'untrusted_dir: other accounts of this computer can change D:\\LF\\updates\\backup (Users)\n', stderr: '', timedOut: false, error: null };
    const verify = (dir, v, o) => (o.previous || o.privateDir ? untrusted : goVerifies(dir, v, o));
    const s = applier(w, { version: '3.2.0', ops: fakeOps({ verify }) });
    for (let i = 1; i <= MAX_REFUNDS; i++) {
      assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 }, `start ${i}`);
      assert.equal(await s.a.refundTry(), true, `refund ${i}`);
    }
    assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 });
    assert.equal(await s.a.refundTry(), false, 'the refunds are used up: this start counts');
    assert.deepEqual(await s.a.failed('the service stopped while starting (exit code 1): database is locked'), { action: 'retry' });
    assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 2 });
    assert.equal(await s.a.refundTry(), false);
    const r = await s.a.failed('the service stopped while starting (exit code 1): database is locked');
    assert.equal(r.action, 'giveup');
    assert.match(r.error, /other accounts of this computer can change the data folder/);
    assert.deepEqual(s.ops.calls, [], 'nothing is run');
    assert.deepEqual(s.ops.verifies.filter((x) => x.previous || x.private).map((x) => (x.private ? 'private' : 'previous')), ['previous'], 'no private folder is asked for');
    assert.deepEqual(copies(w), []);
    assert.equal(has(U(w, 'applying.json')), false);
    assert.equal(trust(w).install, undefined);
    const res = readJ(U(w, 'result.json'));
    assert.equal(res.outcome, 'failed');
    assert.match(res.error, /other accounts of this computer can change the data folder/);
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0', 'the database is not put back');
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
    for (let i = 0; i < 3; i++) { // the next starts: no probation, nothing run (bounded)
      const n = applier(w, { version: '3.2.0', ops: fakeOps({ verify }) });
      assert.deepEqual(await n.a.atStart(), { action: 'continue' }, `later start ${i}`);
      assert.equal(await n.a.refundTry(), false);
      assert.deepEqual(await n.a.failed('the service stopped while starting (exit code 1): database is locked'), { action: 'none' });
      assert.deepEqual(n.ops.calls, []);
      assert.deepEqual(n.ops.verifies, []);
    }
  } finally {
    cleanup(w);
  }
});

test('applying.json of another place (target) is ignored, even with a record of the same versions', async () => {
  const w = await installed();
  try {
    const ap = readJ(U(w, 'applying.json'));
    const elsewhere = JSON.stringify({ ...ap, target: path.join(w.root, 'Elsewhere') });
    fs.writeFileSync(U(w, 'applying.json'), elsewhere);
    const s = applier(w, { version: '3.2.0' });
    assert.deepEqual(await s.a.atStart(), { action: 'continue' });
    // (r6: it is another copy's and stays for that copy; it used to be removed)
    assert.equal(fs.readFileSync(U(w, 'applying.json'), 'utf8'), elsewhere);
    assert.equal(s.a.inProbation, false);
    assert.deepEqual(s.ops.calls, []);
    assert.deepEqual(s.ops.verifies, []);
  } finally {
    cleanup(w);
  }
});

/** failTwice(w, ops): the new version 3.2.0 fails its two starts → { s, r: the second failed()'s result }. */
async function failTwice(w, ops) {
  const s = applier(w, { version: '3.2.0', ops });
  assert.equal((await s.a.atStart()).action, 'probation');
  await s.a.failed('x');
  await s.a.atStart();
  return { s, r: await s.a.failed('the service did not answer within 30 seconds') };
}

for (const [name, run, expect] of [
  ['other accounts can change the data folder (exit 3)', { code: 3, stdout: 'untrusted_dir: other accounts of this computer can change D:\\LF (Authenticated Users)\n' },
    /other accounts of this computer can change the data folder/],
  ['the check cannot run', { error: 'spawn lectureflow.exe ENOENT' }, /the previous version could not be checked: the update check could not run/],
]) {
  test(`no rollback when ${name}: reported, nothing run, the database stays`, async () => {
    const w = await installed({ withFile: true });
    try {
      const verify = (dir, v, o) => (o.previous ? { code: null, stdout: '', stderr: '', timedOut: false, error: null, ...run } : goVerifies(dir, v, o));
      const { s, r } = await failTwice(w, fakeOps({ verify }));
      assert.equal(r.action, 'giveup');
      assert.match(r.error, expect);
      assert.deepEqual(s.ops.calls, []);
      assert.deepEqual(s.ops.verifies.map((x) => x.previous), [true]);
      const res = readJ(U(w, 'result.json'));
      assert.equal(res.outcome, 'failed');
      assert.match(res.error, expect);
      assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0', 'the database is not put back');
      assert.equal(has(U(w, 'applying.json')), false);
      assert.equal(trust(w).install, undefined, 'the record goes with applying.json');
      assert.deepEqual(copies(w), []);
    } finally {
      cleanup(w);
    }
  });
}

test('rollback: a previous package the service program refuses is not run; the recorded backup is used', async () => {
  const w = await installed(); // a backup of 3.1.0 was made
  try {
    // previous.json also names a package (it does not verify)
    const prev = readJ(U(w, 'previous.json'));
    fs.writeFileSync(U(w, 'previous.json'), JSON.stringify({ ...prev, ...pkg(w, '3.1.0', 'LectureFlow-Setup-3.1.0-x64.exe', 'tampered installer') }));
    const verify = (dir, v, o) => (o.previous
      ? { code: 1, stdout: 'invalid selfupdate.signature_invalid: update.json.minisig does not verify\n', stderr: '', timedOut: false, error: null }
      : goVerifies(dir, v, o));
    const { s, r } = await failTwice(w, fakeOps({ verify }));
    assert.deepEqual(r, { action: 'exit' });
    const helper = path.join(privateDir(w), 'helper', 'LectureFlow.exe');
    assert.deepEqual(s.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: helper, args: [RESTORE_ARG], verbatim: false }]);
    assert.equal(readJ(U(w, 'applying.json')).restore, 'backup');
    assert.ok(s.logs.some((l) => /the package of 3\.1\.0 cannot be used: .*signature_invalid/.test(l)), s.logs.join('\n'));
  } finally {
    cleanup(w);
  }
});

for (const [name, change, why] of [
  ['a file of the backup changed', (w) => fs.writeFileSync(U(w, 'backup', '3.1.0', 'app', 'resources', 'app.asar'), 'evil asar'), /its backup: it changed since it was made/],
  ['a file of the backup changed keeping its size', (w) => fs.writeFileSync(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.exe'), 'electron 3.1.X'), /its backup: it changed since it was made/],
  ['a file added to the backup', (w) => fs.writeFileSync(U(w, 'backup', '3.1.0', 'app', 'evil.dll'), 'evil'), /its backup: more than 4 files/],
  ['a file removed from the backup', (w) => fs.rmSync(U(w, 'backup', '3.1.0', 'app', 'Uninstall LectureFlow.exe')), /its backup: it changed since it was made/],
  ['an empty folder added to the backup', (w) => fs.mkdirSync(U(w, 'backup', '3.1.0', 'app', 'plugins')), /its backup: it changed since it was made/],
  ['a file of the backup grew', (w) => fs.appendFileSync(U(w, 'backup', '3.1.0', 'app', 'resources', 'app.asar'), 'x'.repeat(4096)), /its backup: more than \d+ bytes/],
  ['no record of the backup in the profile', (w) => {
    const t = trust(w);
    delete t.backups['3.1.0'];
    fs.writeFileSync(w.trustFile, JSON.stringify(t));
  }, /no package or backup of 3\.1\.0 to return to$/],
  ['the recorded hash is of another tree', (w) => {
    const t = trust(w);
    t.backups['3.1.0'].tree = 'f'.repeat(64);
    fs.writeFileSync(w.trustFile, JSON.stringify(t));
  }, /its backup: it changed since it was made/],
]) {
  test(`rollback: a backup is not used when ${name}`, async () => {
    const w = await installed();
    try {
      change(w);
      const { s, r } = await failTwice(w);
      assert.equal(r.action, 'giveup');
      assert.match(r.error, /no package or backup of 3\.1\.0 to return to/);
      assert.match(r.error, why);
      assert.deepEqual(s.ops.calls, [], 'nothing is run');
      assert.deepEqual(copies(w), [], 'no private folder is left');
      assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
      assert.equal(readJ(U(w, 'result.json')).outcome, 'failed');
    } finally {
      cleanup(w);
    }
  });
}

for (const [name, run, why] of [
  ['other accounts can change the data folder (exit 3)', { code: 3, stdout: 'untrusted_dir: other accounts of this computer can change D:\\LF\\updates\\backup (Users)\n' }, /its backup: other accounts of this computer can change the data folder/],
  ['no private folder can be made (exit 1)', { code: 1, stdout: 'invalid selfupdate.io_failed: mkdir: Access is denied.\n' }, /its backup: no private folder could be made \(selfupdate\.io_failed\)/],
  ['the program names a folder elsewhere', (w) => ({ code: 0, stdout: `ok private=${path.join(w.root, 'lectureflow-update-1')}\n` }), /its backup: the update check did not make the private folder where it was asked to/],
  ['the program names the backup itself', (w) => ({ code: 0, stdout: `ok private=${U(w, 'backup', '3.1.0')}\n` }), /its backup: the update check did not make the private folder where it was asked to/],
  ['the program does not answer', { timedOut: true }, /its backup: the update check did not finish/],
]) {
  test(`rollback: a backup is not used when ${name}`, async () => {
    const w = await installed();
    try {
      const answer = typeof run === 'function' ? run(w) : run;
      const verify = (dir, v, o) => (o.privateDir ? { code: null, stdout: '', stderr: '', timedOut: false, error: null, ...answer } : goVerifies(dir, v, o));
      const { s, r } = await failTwice(w, fakeOps({ verify }));
      assert.equal(r.action, 'giveup');
      assert.match(r.error, why);
      assert.deepEqual(s.ops.calls, [], 'nothing is run');
      assert.deepEqual(copies(w), []);
      assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
    } finally {
      cleanup(w);
    }
  });
}

test('rollback in a build without a public key: the previous package is not run; the result says why', async () => {
  const w = await installed({ withFile: true });
  try {
    const verify = (dir, v, o) => (o.previous ? { code: 2, stdout: 'not_configured: this build has no update public key\n', stderr: '', timedOut: false, error: null } : goVerifies(dir, v, o));
    const { s, r } = await failTwice(w, fakeOps({ verify }));
    assert.equal(r.action, 'giveup');
    assert.match(r.error, /no package or backup of 3\.1\.0 to return to \(its package: this build has no update public key/);
    assert.deepEqual(s.ops.calls, []);
    assert.match(readJ(U(w, 'result.json')).error, /no update public key/);
  } finally {
    cleanup(w);
  }
});

test('the restore program refuses a rollback the profile does not record (another target, no record)', async () => {
  const w = await installed();
  try {
    await failTwice(w);
    const helper = path.join(privateDir(w), 'helper', 'LectureFlow.exe');
    const ap = readJ(U(w, 'applying.json'));
    // a target changed in applying.json: refused
    fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({ ...ap, target: path.join(w.root, 'Windows') }));
    const h = applier(w, { version: '3.2.0' });
    assert.equal(await h.a.restoreFromBackup({ execPath: helper }), false);
    assert.deepEqual(h.ops.calls, []);
    fs.writeFileSync(U(w, 'applying.json'), JSON.stringify(ap));
    // the record without the restore folder: refused
    const t = trust(w);
    const full = JSON.stringify(t);
    delete t.install.restore;
    fs.writeFileSync(w.trustFile, JSON.stringify(t));
    const h1 = applier(w, { version: '3.2.0' });
    assert.equal(await h1.a.restoreFromBackup({ execPath: helper }), false);
    assert.deepEqual(h1.ops.calls, []);
    // the record gone: refused
    delete t.install;
    fs.writeFileSync(w.trustFile, JSON.stringify(t));
    const h2 = applier(w, { version: '3.2.0' });
    assert.equal(await h2.a.restoreFromBackup({ execPath: helper }), false);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
    // (with the record as it was written, the same program is accepted)
    fs.writeFileSync(w.trustFile, full);
    const h3 = applier(w, { version: '3.2.0' });
    assert.equal(await h3.a.restoreFromBackup({ execPath: helper }), true);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0', 'it put 3.1.0 back');
    assert.equal(readJ(U(w, 'applying.json')).restore_error, undefined);
    assert.deepEqual(h3.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: path.join(w.install.target, 'LectureFlow.exe'), args: [UPDATED_ARG], verbatim: false }]);
  } finally {
    cleanup(w);
  }
});

test('private copies: removed when not needed; old ones left by an installer go at a later start', async () => {
  const w = world();
  try {
    // a backup that cannot be made: the copy goes with the refusal
    writePending(w);
    writePrevious(w);
    fs.writeFileSync(U(w, 'backup'), 'not a directory');
    assert.equal((await applier(w).a.atStart()).action, 'continue');
    assert.deepEqual(copies(w), []);
    // copies of earlier starts: old ones are removed, a fresh one and other names stay
    const old = path.join(w.tmp, 'lectureflow-update-111');
    const fresh = path.join(w.tmp, 'lectureflow-update-222');
    const other = path.join(w.tmp, 'lectureflow-other-333');
    for (const d of [old, fresh, other]) fs.mkdirSync(d);
    const t0 = fs.statSync(fresh).mtimeMs;
    fs.utimesSync(old, new Date(t0 - HELPER_WAIT_MS - 60000), new Date(t0 - HELPER_WAIT_MS - 60000));
    await applier(w, { now: () => new Date(t0 + 1000) }).a.atStart();
    assert.deepEqual(fs.readdirSync(w.tmp).sort(), ['lectureflow-other-333', 'lectureflow-update-222']);
  } finally {
    cleanup(w);
  }
});

test('tree hashes: the same tree gives the same hash anywhere; any change gives another', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-tree-'));
  try {
    const a = path.join(root, 'a');
    fs.mkdirSync(path.join(a, 'resources'), { recursive: true });
    fs.writeFileSync(path.join(a, 'LectureFlow.exe'), 'exe');
    fs.writeFileSync(path.join(a, 'resources', 'app.asar'), 'asar');
    fs.cpSync(a, path.join(root, 'b'), { recursive: true });
    const ha = await F.treeHash(a);
    assert.deepEqual(await F.treeHash(path.join(root, 'b')), ha);
    assert.equal(ha.files, 2);
    assert.equal(ha.bytes, 7);
    fs.writeFileSync(path.join(root, 'b', 'resources', 'app.asar'), 'asaR');
    assert.notEqual((await F.treeHash(path.join(root, 'b'))).sha256, ha.sha256);
    fs.mkdirSync(path.join(a, 'empty'));
    assert.notEqual((await F.treeHash(a)).sha256, ha.sha256, 'an empty folder counts');
    assert.equal((await F.treeHash(path.join(a, 'LectureFlow.exe'))).files, 1, 'a single file');
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('treeStat counts without reading and stops at its bounds', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-stat-'));
  try {
    const a = path.join(root, 'a');
    fs.mkdirSync(path.join(a, 'resources', 'bin'), { recursive: true });
    fs.writeFileSync(path.join(a, 'LectureFlow.exe'), 'exe');
    fs.writeFileSync(path.join(a, 'resources', 'app.asar'), 'asar');
    assert.deepEqual(await F.treeStat(a), { files: 2, bytes: 7, entries: 5 });
    assert.deepEqual(await F.treeStat(a, { maxFiles: 2, maxBytes: 7, maxEntries: 5, maxDepth: 3 }), { files: 2, bytes: 7, entries: 5 });
    assert.deepEqual(await F.treeStat(path.join(a, 'LectureFlow.exe')), { files: 1, bytes: 3, entries: 1 });
    await assert.rejects(F.treeStat(a, { maxFiles: 1 }), /more than 1 files/);
    await assert.rejects(F.treeStat(a, { maxBytes: 6 }), /more than 6 bytes/);
    await assert.rejects(F.treeStat(a, { maxEntries: 4 }), /more than 4 entries/);
    await assert.rejects(F.treeStat(a, { maxDepth: 2 }), /deeper than 2 levels/); // resources/bin is the third level
    await assert.rejects(F.treeStat(path.join(root, 'missing')), /ENOENT/);
    // a folder with many entries is refused once its names are known,
    // before any of them is looked at
    const wide = path.join(root, 'wide');
    fs.mkdirSync(wide);
    for (let i = 0; i < 50; i++) fs.writeFileSync(path.join(wide, `f${i}`), '');
    await assert.rejects(F.treeStat(wide, { maxEntries: 20 }), /more than 20 entries/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

// ---- security review SEC17, 高-A: links in place of updates/ or backup/ ----

/**
 * linkDir(link, target): a junction on Windows (no privilege needed), a
 * symbolic link elsewhere. unlinkDir removes the link itself, never what
 * it leads to: links are removed before the temporary folders are.
 */
function linkDir(link, target) {
  fs.symlinkSync(target, link, 'junction');
}
function unlinkDir(link) {
  try {
    fs.unlinkSync(link);
  } catch {
    try {
      fs.rmdirSync(link);
    } catch {
      // gone already
    }
  }
}
function lexists(p) {
  try {
    fs.lstatSync(p);
    return true;
  } catch {
    return false;
  }
}

/** victim(w) → a folder of the user's outside the data folder that looks like the updates folder. */
function victim(w) {
  const v = path.join(w.root, 'victim');
  for (const rel of ['backup/photos/important.txt', 'backup/3.0.0/app/LectureFlow.exe', '3.0.0/db/lectureflow.db', 'db/keep.txt']) {
    fs.mkdirSync(path.dirname(path.join(v, rel)), { recursive: true });
    fs.writeFileSync(path.join(v, rel), `the user's ${rel}`);
  }
  return v;
}

/** listTree(dir) → every path below dir ("/"-separated, a link marked "@"), links not followed. */
function listTree(dir) {
  const out = [];
  const walk = (abs, rel, depth) => {
    assert.ok(depth < 10, 'listTree: too deep');
    for (const n of fs.readdirSync(abs).sort()) {
      const r = rel ? `${rel}/${n}` : n;
      const st = fs.lstatSync(path.join(abs, n));
      out.push(st.isSymbolicLink() ? `${r}@` : r);
      if (st.isDirectory()) walk(path.join(abs, n), r, depth + 1);
    }
  };
  walk(dir, '', 0);
  return out;
}

test('old backups are pruned: the one previous.json names stays without its database, the others go', async () => {
  const w = world();
  try {
    for (const v of ['3.0.0', '3.1.0']) {
      fs.mkdirSync(U(w, 'backup', v, 'app'), { recursive: true });
      fs.writeFileSync(U(w, 'backup', v, 'app', 'LectureFlow.exe'), `electron ${v}`);
      fs.mkdirSync(U(w, 'backup', v, 'db'), { recursive: true });
      fs.writeFileSync(U(w, 'backup', v, 'db', 'lectureflow.db'), `db ${v}`);
    }
    writePrevious(w, { backup: 'backup/3.1.0' });
    await applier(w).a._pruneBackups();
    assert.deepEqual(listTree(U(w, 'backup')), ['3.1.0', '3.1.0/app', '3.1.0/app/LectureFlow.exe']);
  } finally {
    cleanup(w);
  }
});

test('pruning backups does not follow a link in place of updates/, backup/ or a backup', async () => {
  const cases = {
    'updates/': (w, v) => {
      fs.rmdirSync(w.updates); // (empty: world made it)
      linkDir(w.updates, v);
      return w.updates;
    },
    'backup/': (w, v) => {
      linkDir(U(w, 'backup'), path.join(v, 'backup'));
      return U(w, 'backup');
    },
    'the kept backup': (w, v) => {
      fs.mkdirSync(U(w, 'backup'));
      linkDir(U(w, 'backup', '3.1.0'), v); // its db/ is the user's
      return U(w, 'backup', '3.1.0');
    },
  };
  for (const [name, plant] of Object.entries(cases)) {
    const w = world();
    let link = null;
    try {
      const v = victim(w);
      const before = listTree(v);
      link = plant(w, v);
      // (written through the link in the first case: previous.json is read, not pruned)
      writePrevious(w, { backup: 'backup/3.1.0' });
      const after = [...before, ...(name === 'updates/' ? ['previous.json'] : [])].sort();
      const { a, logs } = applier(w);
      await a._pruneBackups();
      assert.deepEqual(listTree(v).sort(), after, `${name}: what the link leads to is untouched`);
      if (name !== 'the kept backup') assert.ok(logs.some((l) => /through a link/.test(l)), `${name}: ${logs.join('\n')}`);
    } finally {
      if (link) unlinkDir(link);
      cleanup(w);
    }
  }
  // a link among the backups that is not kept: the link goes, not what it leads to
  const w = world();
  let link = null;
  try {
    const v = victim(w);
    const before = listTree(v);
    fs.mkdirSync(U(w, 'backup'));
    link = U(w, 'backup', '3.0.0');
    linkDir(link, v);
    writePrevious(w, { backup: 'backup/3.1.0' });
    await applier(w).a._pruneBackups();
    assert.deepEqual(listTree(v), before, 'what the link led to is untouched');
    assert.equal(lexists(link), false, 'the link is gone');
  } finally {
    if (link) unlinkDir(link);
    cleanup(w);
  }
});

// ---- security review SEC17 中-B: the trust file outside the data folder ----

test('trustDirFor: %LOCALAPPDATA%\\LectureFlow\\selfupdate on Windows (else <home>\\AppData\\Local), none elsewhere', () => {
  assert.equal(F.trustDirFor({ platform: 'win32', env: { LOCALAPPDATA: 'C:\\Users\\alice\\AppData\\Local' }, home: 'C:\\Users\\alice' }),
    'C:\\Users\\alice\\AppData\\Local\\LectureFlow\\selfupdate');
  assert.equal(F.trustDirFor({ platform: 'win32', env: {}, home: 'C:\\Users\\alice' }), 'C:\\Users\\alice\\AppData\\Local\\LectureFlow\\selfupdate');
  assert.equal(F.trustDirFor({ platform: 'win32', env: { LOCALAPPDATA: 'relative\\dir' }, home: 'C:\\Users\\alice' }), 'C:\\Users\\alice\\AppData\\Local\\LectureFlow\\selfupdate');
  assert.equal(F.trustDirFor({ platform: 'win32', env: {}, home: '' }), null);
  assert.equal(F.trustDirFor({ platform: 'darwin', env: { LOCALAPPDATA: 'C:\\x' }, home: '/Users/alice' }), null, 'macOS keeps the profile (known limitation)');
  assert.equal(F.trustDirFor({ platform: 'linux', env: {}, home: '/home/alice' }), null);
});

test('roamingAppDataFor: %APPDATA% on Windows when it is an absolute path with a drive letter, else none', () => {
  assert.equal(F.roamingAppDataFor({ platform: 'win32', env: { APPDATA: 'C:\\Users\\alice\\AppData\\Roaming' } }), 'C:\\Users\\alice\\AppData\\Roaming');
  assert.equal(F.roamingAppDataFor({ platform: 'win32', env: { APPDATA: ' C:\\Users\\alice\\AppData\\Roaming\\ ' } }), 'C:\\Users\\alice\\AppData\\Roaming\\');
  for (const APPDATA of [undefined, '', '   ', 'AppData\\Roaming', '\\Users\\alice', '\\\\server\\share\\alice', 'C:relative', 'C:\\a\0b', 42]) {
    assert.equal(F.roamingAppDataFor({ platform: 'win32', env: { APPDATA } }), null, String(APPDATA));
  }
  assert.equal(F.roamingAppDataFor({ platform: 'win32' }), null);
  assert.equal(F.roamingAppDataFor({ platform: 'darwin', env: { APPDATA: 'C:\\x' } }), null, 'macOS does not take the record over this way');
});

/**
 * upgraded(w, { profile, ops, version }): the shell of a version with the
 * trust folder (w.root/trust-new) starting where an older one kept its
 * record in profile (default w.profile).
 */
function upgraded(w, { profile = w.profile, ops = fakeOps(), version = '3.2.0', appDataDir } = {}) {
  const logs = [];
  const log = { info: (m) => logs.push(m), warn: (m) => logs.push(m), error: (m) => logs.push(m) };
  const trustDir = path.join(w.root, 'trust-new');
  const a = new SelfUpdateApplier({ install: w.install, currentVersion: version, dataDir: w.dataDir, ops, log, now: () => NOW, privateDir: profile, trustDir, tmpDir: w.tmp, appDataDir });
  return { a, ops, logs, trustDir, trustFile: path.join(trustDir, F.TRUST_DIR_FILE), mark: path.join(trustDir, F.TRUST_MIGRATED_FILE) };
}
const trustChecks = (ops) => ops.verifies.filter((v) => v.trustCheck).length;

test('the record of an older version: the installation on probation is taken over once, with its backup when the profile lies outside the data folder', async () => {
  const w = await installed({ legacy: true }); // the older shell recorded 3.1.0 → 3.2.0 and a backup of 3.1.0
  try {
    const legacyFile = path.join(w.profile, F.TRUST_FILE);
    const old = readJ(legacyFile);
    const u = upgraded(w);
    assert.deepEqual(await u.a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 });
    const t = readJ(u.trustFile);
    assert.deepEqual(t.install, { version: '3.2.0', from_version: '3.1.0', target: w.install.target, kind: 'installer', started_at: old.install.started_at });
    assert.deepEqual(t.backups, { '3.1.0': old.backups['3.1.0'] });
    assert.equal(readJ(u.mark).install, true);
    assert.equal(trustChecks(u.ops), 1, 'the service program vouched for both folders');
    assert.ok(u.ops.trustDirs.every((d) => d === u.trustDir), 'every check names the trust folder');
    assert.ok(has(legacyFile), 'kept while the installation is on probation (a rollback to 3.1.0 reads it)');
    // a second start: the profile is not looked at again
    fs.writeFileSync(legacyFile, JSON.stringify({ ...old, backups: { '3.1.0': { ...old.backups['3.1.0'], tree: 'f'.repeat(64) } } }));
    const again = upgraded(w);
    assert.equal((await again.a.atStart()).action, 'probation');
    assert.equal(trustChecks(again.ops), 0);
    assert.deepEqual(readJ(again.trustFile).backups, { '3.1.0': old.backups['3.1.0'] });
    await again.a.healthy();
    assert.equal(readJ(U(w, 'result.json')).outcome, 'installed');
    assert.equal(readJ(again.trustFile).install, undefined);
    assert.equal(has(legacyFile), false, 'the old record goes once the installation is done');
  } finally {
    cleanup(w);
  }
});

test('the record of an older version is not taken over when it does not match, nothing is on probation, or the folders are not vouched for', async () => {
  const untrusted = { code: 3, stdout: 'untrusted_dir: other accounts of this computer can change D:\\LF (Users)\n', stderr: '', timedOut: false, error: null };
  const cases = [
    ['another installation', (w) => {
      const ap = readJ(U(w, 'applying.json'));
      fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({ ...ap, from_version: '3.0.0' }));
    }, {}],
    ['another target', (w) => {
      const f = path.join(w.profile, F.TRUST_FILE);
      const t = readJ(f);
      fs.writeFileSync(f, JSON.stringify({ ...t, install: { ...t.install, target: path.join(w.root, 'Elsewhere') } }));
    }, {}],
    ['no installation on probation', (w) => fs.rmSync(U(w, 'applying.json')), {}],
    ['the data folder or the trust folder is not vouched for', () => {}, { verify: (dir, v, o) => (o.trustCheck ? untrusted : goVerifies(dir, v, o)) }],
    ['the check cannot run', () => {}, { verify: (dir, v, o) => (o.trustCheck ? { code: null, stdout: '', stderr: '', timedOut: true, error: null } : goVerifies(dir, v, o)) }],
  ];
  for (const [name, change, opsOpts] of cases) {
    const w = await installed({ legacy: true });
    try {
      change(w);
      const u = upgraded(w, { ops: fakeOps(opsOpts) });
      assert.equal((await u.a.atStart()).action, 'continue', name);
      assert.equal(u.a.inProbation, false, name);
      assert.equal(trust({ trustFile: u.trustFile }).install, undefined, name);
      assert.deepEqual(trust({ trustFile: u.trustFile }).backups || {}, {}, name);
      assert.ok(has(u.mark), `${name}: looked at once all the same`);
      assert.equal(has(path.join(w.profile, F.TRUST_FILE)), false, `${name}: the old record goes`);
      assert.deepEqual(u.ops.calls, [], `${name}: nothing is run`);
      // put back afterwards: never looked at again
      fs.writeFileSync(path.join(w.profile, F.TRUST_FILE), JSON.stringify({
        schema: 1, install: { version: '3.2.0', from_version: '3.1.0', target: w.install.target, kind: 'installer' }, backups: {},
      }));
      const again = upgraded(w);
      await again.a.atStart();
      assert.equal(trustChecks(again.ops), 0, name);
      assert.equal(trust({ trustFile: again.trustFile }).install, undefined, name);
    } finally {
      cleanup(w);
    }
  }
});

test('the record of an older version in a profile in the data folder: the installation only, never its backup', async () => {
  const w = await installed({ legacy: true });
  try {
    const inData = path.join(w.dataDir, 'electron');
    fs.mkdirSync(inData);
    fs.renameSync(path.join(w.profile, F.TRUST_FILE), path.join(inData, F.TRUST_FILE));
    const u = upgraded(w, { profile: inData });
    assert.equal((await u.a.atStart()).action, 'probation');
    const t = readJ(u.trustFile);
    assert.equal(t.install.version, '3.2.0');
    assert.deepEqual(t.backups, {});
    assert.equal(readJ(u.mark).backup, false);
  } finally {
    cleanup(w);
  }
});

// T-SEC-04 (security review SEC17 中-H, option b): in the default data
// folder (%APPDATA%\LectureFlow, the profile in it) the backup's record is
// taken over too when the service program vouches for the folders, so the
// first update after the move can still go back.

test('the record of an older version in the default data folder, vouched for: its backup is taken over and two failed starts go back to it', async () => {
  const w = await installed({ legacy: true, defaultPlace: true });
  try {
    const legacyFile = path.join(w.profile, F.TRUST_FILE);
    const old = readJ(legacyFile);
    assert.ok(old.backups['3.1.0'], 'the older shell recorded its backup');
    // %APPDATA% as the system gives it may differ in case and end in a separator
    const appDataDir = process.platform === 'win32' ? `${w.root.toUpperCase()}\\` : `${w.root}/`;
    const u = upgraded(w, { appDataDir });
    assert.deepEqual(await u.a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 });
    const t = readJ(u.trustFile);
    assert.deepEqual(t.install, { version: '3.2.0', from_version: '3.1.0', target: w.install.target, kind: 'installer', started_at: old.install.started_at });
    assert.deepEqual(t.backups, { '3.1.0': old.backups['3.1.0'] });
    assert.deepEqual(readJ(u.mark), { schema: 1, at: NOW.toISOString(), install: true, backup: true });
    assert.equal(trustChecks(u.ops), 1, 'the service program vouched for the data folder and the trust folder');

    // two failed starts: back to 3.1.0 from the backup, through the same
    // checks as a backup this shell recorded itself
    assert.deepEqual(await u.a.failed('the service did not answer within 30 seconds'), { action: 'retry' });
    assert.deepEqual(await u.a.atStart(), { action: 'probation', version: '3.2.0', tries: 2 });
    assert.deepEqual(await u.a.failed('the window did not load the app: ERR_FAILED (-2)'), { action: 'exit' });
    const priv = privateDir(w);
    const helper = path.join(priv, 'helper', 'LectureFlow.exe');
    assert.deepEqual(u.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: helper, args: [RESTORE_ARG], verbatim: false }]);
    assert.equal(u.ops.verifies.filter((x) => x.previous).length, 1, 'the service program was asked about the previous version first');
    assert.equal(u.ops.verifies.filter((x) => x.private).length, 1, 'the copy went into a folder the service program made');
    assert.ok(u.ops.trustDirs.every((d) => d === u.trustDir), 'every check names the trust folder');
    assert.equal((await F.treeHash(path.join(priv, 'app'))).sha256, old.backups['3.1.0'].tree, 'the private copy has the recorded hash');
    assert.deepEqual(readJ(u.trustFile).install.restore, { dir: priv, tree: old.backups['3.1.0'].tree });
    assert.equal(readJ(U(w, 'applying.json')).restore, 'backup');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update');

    // the restore program (a copy of 3.2.0) puts the checked copy in place
    const h = upgraded(w, { appDataDir });
    assert.equal(await h.a.restoreFromBackup({ execPath: helper }), true);
    assert.deepEqual(installedFiles(w), ['electron 3.1.0', 'asar 3.1.0']);
    assert.deepEqual(h.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: path.join(w.install.target, 'LectureFlow.exe'), args: [UPDATED_ARG], verbatim: false }]);
  } finally {
    cleanup(w);
  }
});

test('the record of an older version is taken over only before the new version\'s first start: applying.json with tries other than 0 is not (SEC21 低-A)', async () => {
  // At the move tries is 0 (written by the older shell's _install) or
  // missing (an older applying.json): _migrateTrust runs before
  // _beginProbation counts the start, and only once. A prepared
  // applying.json that says it was started already would roll back at
  // once.
  for (const tries of [2, 1, '0', -1, 0.5, null]) {
    const w = await installed({ legacy: true, defaultPlace: true });
    try {
      const ap = readJ(U(w, 'applying.json'));
      assert.equal(ap.tries, 0, 'the older shell wrote tries 0');
      fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({ ...ap, tries }));
      const before = installedFiles(w);
      const u = upgraded(w, { appDataDir: w.root });
      const r = await u.a.atStart();
      assert.equal(r.action, 'continue', `tries ${JSON.stringify(tries)}: no probation, no rollback`);
      assert.equal(u.a.inProbation, false);
      assert.deepEqual(u.ops.calls, [], `tries ${JSON.stringify(tries)}: nothing is run`);
      assert.deepEqual(u.ops.verifies.filter((x) => x.private || x.previous), [], 'no copy, no previous version asked for');
      assert.deepEqual(copies(w), []);
      assert.deepEqual(installedFiles(w), before, 'the installation is not touched');
      assert.equal(trust({ trustFile: u.trustFile }).install, undefined, `tries ${JSON.stringify(tries)}`);
      assert.deepEqual(trust({ trustFile: u.trustFile }).backups || {}, {}, `tries ${JSON.stringify(tries)}`);
      assert.equal(readJ(u.mark).install, false);
    } finally {
      cleanup(w);
    }
  }
  // tries missing (an applying.json without it): taken over as usual
  const w = await installed({ legacy: true, defaultPlace: true });
  try {
    const { tries, ...ap } = readJ(U(w, 'applying.json'));
    assert.equal(tries, 0);
    fs.writeFileSync(U(w, 'applying.json'), JSON.stringify(ap));
    const u = upgraded(w, { appDataDir: w.root });
    assert.deepEqual(await u.a.atStart(), { action: 'probation', version: '3.2.0', tries: 1 });
    assert.ok(readJ(u.trustFile).backups['3.1.0'], 'with its backup');
  } finally {
    cleanup(w);
  }
});

test('the record of an older version in the default data folder: no backup without the service program\'s word, and the profile is looked at once', async () => {
  const untrusted = { code: 3, stdout: 'untrusted_dir: other accounts of this computer can change C:\\Users\\alice\\AppData\\Roaming\\LectureFlow (Users)\n', stderr: '', timedOut: false, error: null };
  const w = await installed({ legacy: true, defaultPlace: true });
  try {
    const legacyFile = path.join(w.profile, F.TRUST_FILE);
    const old = readJ(legacyFile);
    const u = upgraded(w, { appDataDir: w.root, ops: fakeOps({ verify: (dir, v, o) => (o.trustCheck ? untrusted : goVerifies(dir, v, o)) }) });
    assert.equal((await u.a.atStart()).action, 'continue');
    assert.equal(trust({ trustFile: u.trustFile }).install, undefined);
    assert.deepEqual(trust({ trustFile: u.trustFile }).backups || {}, {});
    assert.deepEqual(readJ(u.mark), { schema: 1, at: NOW.toISOString(), install: false, backup: false });
    assert.deepEqual(u.ops.calls, []);
    assert.deepEqual(u.ops.verifies.filter((x) => x.private), []);
    // the next start, the folders vouched for now: the profile is not read again
    fs.writeFileSync(legacyFile, JSON.stringify(old));
    const again = upgraded(w, { appDataDir: w.root });
    await again.a.atStart();
    assert.equal(trustChecks(again.ops), 0);
    assert.equal(trust({ trustFile: again.trustFile }).install, undefined);
    assert.deepEqual(trust({ trustFile: again.trustFile }).backups || {}, {});
  } finally {
    cleanup(w);
  }
});

test('the record of an older version in a profile in the data folder: no backup unless the data folder is %APPDATA%\\LectureFlow itself, without a link on its way', async () => {
  const onlyInstall = async (w, opts, name) => {
    const u = upgraded(w, opts);
    assert.equal((await u.a.atStart()).action, 'probation', name);
    const t = readJ(u.trustFile);
    assert.equal(t.install.version, '3.2.0', `${name}: the installation is taken over`);
    assert.deepEqual(t.backups, {}, `${name}: its backup is not`);
    assert.equal(readJ(u.mark).backup, false, name);
    assert.equal(trustChecks(u.ops), 1, name);
  };
  // %APPDATA% not known, not absolute, or the data folder not the default one in it
  for (const [name, appData] of [
    ['no APPDATA', () => undefined],
    ['empty APPDATA', () => ''],
    ['relative APPDATA', () => (process.platform === 'win32' ? 'AppData\\Roaming' : 'AppData/Roaming')],
    ['APPDATA elsewhere', (w) => path.join(w.root, 'elsewhere')],
    ['the data folder is APPDATA itself', (w) => w.dataDir],
    ['APPDATA one level up (the data folder is not its LectureFlow)', (w) => path.dirname(w.root)],
  ]) {
    const w = await installed({ legacy: true, defaultPlace: true });
    try {
      await onlyInstall(w, { appDataDir: appData(w) }, name);
    } finally {
      cleanup(w);
    }
  }
  // a data folder moved elsewhere (LECTUREFLOW_DATA_DIR: the profile goes with it), though below %APPDATA%
  {
    const w = await installed({ legacy: true });
    try {
      const inData = path.join(w.dataDir, 'electron');
      fs.mkdirSync(inData);
      fs.renameSync(path.join(w.profile, F.TRUST_FILE), path.join(inData, F.TRUST_FILE));
      await onlyInstall(w, { profile: inData, appDataDir: w.root }, 'a data folder of its own name below APPDATA');
    } finally {
      cleanup(w);
    }
  }
  // a link on the way: %APPDATA% reached through one, or the profile is one
  {
    const w = await installed({ legacy: true, defaultPlace: true });
    const link = `${w.root}-link`;
    try {
      linkDir(link, w.root);
      const via = path.join(link, 'LectureFlow');
      await onlyInstall({ ...w, dataDir: via }, { profile: path.join(via, 'electron'), appDataDir: link }, 'APPDATA through a link');
    } finally {
      unlinkDir(link);
      cleanup(w);
    }
  }
  {
    const w = await installed({ legacy: true, defaultPlace: true });
    try {
      const real = path.join(w.root, 'profile-elsewhere');
      fs.renameSync(w.profile, real);
      linkDir(w.profile, real);
      await onlyInstall(w, { appDataDir: w.root }, 'the profile is a link');
    } finally {
      unlinkDir(w.profile);
      cleanup(w);
    }
  }
});

test('a record of an installation without applying.json is dropped', async () => {
  const w = world();
  try {
    fs.mkdirSync(w.trustDir, { recursive: true });
    fs.writeFileSync(w.trustFile, JSON.stringify({ schema: 1, install: { version: '3.2.0', from_version: '3.1.0', target: w.install.target, kind: 'installer' }, backups: { '3.0.0': { dir: 'backup/3.0.0' } } }));
    const { a } = applier(w);
    assert.deepEqual(await a.atStart(), { action: 'continue' });
    assert.deepEqual(trust(w).install, undefined);
    assert.deepEqual(trust(w).backups, { '3.0.0': { dir: 'backup/3.0.0' } }, 'the backups stay');
  } finally {
    cleanup(w);
  }
});

// ---- what a copy shares with the other copies of this account (T14 / C132) ----

const UNSUPPORTED = { unsupported: 'this copy of LectureFlow was not set up by its installer; install the new version by hand' };

/** within(p, root): p is root or lies below it (case-insensitive on Windows). */
function within(p, root) {
  let rel = path.relative(path.resolve(root), path.resolve(p));
  if (process.platform === 'win32') rel = path.relative(path.resolve(root).toLowerCase(), path.resolve(p).toLowerCase());
  return rel === '' || (!rel.startsWith('..') && !path.isAbsolute(rel));
}

/**
 * watchFs(roots) → { touched, restore }: every call of the file system
 * functions selfupdate.js uses (F.fsp, the same object as fs.promises)
 * whose first or second argument lies in one of roots is recorded in
 * touched ("<function> <path>"), reads included. restore() puts the
 * functions back.
 */
function watchFs(roots) {
  const touched = [];
  const saved = {};
  for (const k of Object.keys(F.fsp)) {
    const fn = F.fsp[k];
    if (typeof fn !== 'function') continue;
    saved[k] = fn;
    F.fsp[k] = function watched(...args) {
      for (const x of args.slice(0, 2)) {
        if (typeof x === 'string' && roots.some((r) => within(x, r))) touched.push(`${k} ${x}`);
      }
      return fn.apply(this, args);
    };
  }
  return { touched, restore: () => Object.assign(F.fsp, saved) };
}

/** snapshot(dir) → every entry below dir with its kind, content and mtime (a sorted list; [] when dir is missing). */
function snapshot(dir) {
  const out = [];
  const walk = (d) => {
    let names;
    try {
      names = fs.readdirSync(d).sort();
    } catch {
      return;
    }
    for (const n of names) {
      const p = path.join(d, n);
      const st = fs.lstatSync(p);
      out.push(`${path.relative(dir, p)} ${st.isDirectory() ? 'dir' : fs.readFileSync(p, 'utf8')} ${st.mtimeMs}`);
      if (st.isDirectory()) walk(p);
    }
  };
  walk(dir);
  return out;
}

/**
 * sharedState(w, { target }): what other copies of LectureFlow of this
 * account keep outside w's data folder: the trust folder with the record
 * of an installation on probation (of target) and no migrated.json yet, a
 * record of an older version in the profile, and an old private copy in
 * the temporary folder. → the old copy's path.
 */
function sharedState(w, { target }) {
  fs.mkdirSync(w.trustDir, { recursive: true });
  fs.writeFileSync(w.trustFile, JSON.stringify({ schema: 1, install: { version: '3.2.0', from_version: '3.1.0', target, kind: 'installer' }, backups: { '3.1.0': { dir: 'backup/3.1.0' } } }));
  fs.mkdirSync(w.profile, { recursive: true });
  fs.writeFileSync(path.join(w.profile, F.TRUST_FILE), JSON.stringify({ schema: 1, install: { version: '3.2.0', from_version: '3.1.0', target, kind: 'installer' }, backups: {} }));
  const old = path.join(w.tmp, 'lectureflow-update-111');
  fs.mkdirSync(old);
  fs.writeFileSync(path.join(old, 'LectureFlow-Setup-3.2.0-x64.exe'), 'an installer running from its copy');
  const long = new Date(NOW.getTime() - HELPER_WAIT_MS - 60000);
  fs.utimesSync(old, long, long);
  return old;
}

test('a copy that cannot update itself leaves the trust folder, the profile\'s old record and the private copies alone at its start', async () => {
  for (const situation of ['nothing', 'pending', 'applying', 'bad applying', 'applying and pending']) {
    const w = world();
    let watch = null;
    try {
      const elsewhere = path.join(w.root, 'Programs', 'LectureFlow'); // the installed copy of this account
      sharedState(w, { target: elsewhere });
      if (situation.includes('pending')) writePending(w);
      if (situation === 'applying' || situation === 'applying and pending') {
        // the installed copy's update on probation in the shared data
        // folder, with the backup and the database snapshot its rollback needs
        fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({
          schema: 1, version: '3.2.0', from_version: '3.1.0', channel: 'stable', platform: 'windows-x64', kind: 'installer',
          file: '3.2.0/x.exe', size: 1, sha256: 'a'.repeat(64), phase: 'installing', tries: 0, target: elsewhere, exe: 'LectureFlow.exe',
          db_snapshot: 'backup/3.1.0/db',
        }));
        fs.mkdirSync(U(w, 'backup', '3.1.0', 'app'), { recursive: true });
        fs.mkdirSync(U(w, 'backup', '3.1.0', 'db'));
        fs.writeFileSync(U(w, 'backup', '3.1.0', 'app', 'LectureFlow.exe'), 'electron 3.1.0');
        fs.writeFileSync(U(w, 'backup', '3.1.0', 'db', 'lectureflow.db'), 'db before the update');
        fs.writeFileSync(U(w, 'backup', '3.0.0'), 'an older backup');
        writePrevious(w, { backup: 'backup/3.1.0' });
      }
      if (situation === 'bad applying') fs.writeFileSync(U(w, 'applying.json'), '{ not json');
      const roots = [w.trustDir, w.tmp, w.profile];
      const before = roots.map(snapshot);
      const updatesBefore = snapshot(w.updates);
      const { a, ops } = applier(w, { install: UNSUPPORTED });
      watch = watchFs(roots);
      assert.deepEqual(await a.atStart(), { action: 'continue' }, situation);
      watch.restore();
      assert.deepEqual(watch.touched, [], `${situation}: nothing shared was read, written or removed`);
      assert.deepEqual(roots.map(snapshot), before, `${situation}: the shared folders are as they were`);
      assert.equal(has(path.join(w.trustDir, F.TRUST_MIGRATED_FILE)), false, `${situation}: no migrated.json`);
      assert.deepEqual(ops.calls, [], situation);
      assert.deepEqual(ops.verifies, [], situation);
      // applying.json is only ever written by a copy that installs (never
      // this one): it, the backups and previous.json are another copy's,
      // left exactly as they were for that copy to finish (SEC29 低-1).
      // A pending update too (r6): this copy can never install it, the
      // copy that can finds it as it was, and no result is written (it
      // used to be removed and reported as "install by hand").
      assert.deepEqual(snapshot(w.updates), updatesBefore, `${situation}: the updates folder is as it was`);
      assert.equal(has(U(w, 'result.json')), false, `${situation}: no result`);
    } finally {
      if (watch) watch.restore();
      cleanup(w);
    }
  }
});

test('the installed and the portable build still look after the trust folder and the private copies at their start', async () => {
  for (const kind of ['installer', 'portable']) {
    const w = world({ kind });
    try {
      const old = sharedState(w, { target: w.install.target });
      const fresh = path.join(w.tmp, 'lectureflow-update-222');
      fs.mkdirSync(fresh);
      const { a } = applier(w);
      assert.deepEqual(await a.atStart(), { action: 'continue' }, kind);
      assert.equal(has(old), false, `${kind}: an old private copy is removed`);
      assert.ok(has(fresh), `${kind}: a fresh one stays`);
      assert.equal(readJ(path.join(w.trustDir, F.TRUST_MIGRATED_FILE)).install, false, `${kind}: the profile was looked at once`);
      assert.equal(has(path.join(w.profile, F.TRUST_FILE)), false, `${kind}: the profile's old record goes`);
      assert.equal(trust(w).install, undefined, `${kind}: its own record of an installation not on probation is dropped`);
    } finally {
      cleanup(w);
    }
  }
});

test('the record of another copy\'s installation (another target) stays when this copy starts without applying.json', async () => {
  // e.g. the installed copy is on probation with applying.json in its data
  // folder while a portable copy (or one with another data folder) starts:
  // without the record the installed copy could not go back.
  for (const kind of ['installer', 'portable']) {
    const w = world({ kind });
    try {
      const other = kind === 'installer' ? path.join(w.root, 'Apps', 'LectureFlow-3.2.0-x64-portable.exe') : path.join(w.root, 'Programs', 'LectureFlow');
      const rec = { version: '3.2.0', from_version: '3.1.0', target: other, kind: kind === 'installer' ? 'portable' : 'installer' };
      fs.mkdirSync(w.trustDir, { recursive: true });
      fs.writeFileSync(w.trustFile, JSON.stringify({ schema: 1, install: rec, backups: {} }));
      const { a, logs } = applier(w);
      assert.deepEqual(await a.atStart(), { action: 'continue' }, kind);
      assert.deepEqual(trust(w).install, rec, `${kind}: the other copy's record stays`);
      assert.ok(!logs.some((m) => /dropped the record/.test(m)), kind);
    } finally {
      cleanup(w);
    }
  }
});

// ---- copies sharing one data folder (r6; SEC29 第六节 #6, S1) ----

/** otherCopy(w) → the installation of the other kind of copy of this account (not w's), its program in place. */
function otherCopy(w) {
  if (w.install.kind === 'installer') {
    const exe = path.join(w.root, 'Apps', 'LectureFlow-x64-portable.exe');
    fs.mkdirSync(path.dirname(exe), { recursive: true });
    fs.writeFileSync(exe, 'portable');
    return { platform: 'windows-x64', kind: 'portable', kinds: ['portable'], target: exe, exe: 'LectureFlow.exe' };
  }
  const dir = path.join(w.root, 'Programs', 'LectureFlow');
  fs.mkdirSync(path.join(dir, 'resources'), { recursive: true });
  fs.writeFileSync(path.join(dir, 'LectureFlow.exe'), 'electron');
  fs.writeFileSync(path.join(dir, 'Uninstall LectureFlow.exe'), 'uninstaller');
  return { platform: 'windows-x64', kind: 'installer', kinds: ['installer'], target: dir, exe: 'LectureFlow.exe' };
}

test('applying.json of another copy (another target) is left alone, with all its rollback needs; that copy still goes back', async () => {
  for (const kind of ['installer', 'portable']) {
    // this copy (kind) installed 3.2.0 and is on probation; the other kind
    // of copy of this account starts in between, sharing the data folder
    const w = await installed({ kind, withFile: true });
    try {
      let s = applier(w, { version: '3.2.0' });
      assert.equal((await s.a.atStart()).action, 'probation', kind);
      assert.deepEqual(await s.a.failed('the service did not answer within 30 seconds'), { action: 'retry' }, kind);

      const before = snapshot(w.updates);
      const record = trust(w).install;
      for (const version of ['3.1.0', '3.2.0', '3.3.0']) {
        for (const background of [false, true]) {
          const o = applier(w, { version, install: otherCopy(w) });
          assert.deepEqual(await o.a.atStart({ background }), { action: 'continue' }, `${kind} ${version}`);
          assert.deepEqual(snapshot(w.updates), before, `${kind} ${version}: applying.json, the backups, the snapshot and previous.json are as they were; no result`);
          assert.deepEqual(trust(w).install, record, `${kind} ${version}: the record stays`);
          assert.deepEqual(o.ops.calls, [], `${kind} ${version}: nothing is run`);
          assert.deepEqual(o.ops.verifies, [], `${kind} ${version}`);
          assert.ok(o.logs.some((l) => /applying\.json is another copy's.*left alone/.test(l)), o.logs.join('\n'));
          assert.ok(!o.logs.some((l) => /ignoring applying\.json/.test(l)), o.logs.join('\n'));
        }
      }

      // this copy's second failed start: it goes back as if nothing had happened
      s = applier(w, { version: '3.2.0' });
      assert.deepEqual(await s.a.atStart(), { action: 'probation', version: '3.2.0', tries: 2 }, kind);
      assert.deepEqual(await s.a.failed('the service stopped while starting (exit code 1): boom'), { action: 'exit' }, kind);
      assert.equal(readJ(U(w, 'applying.json')).phase, 'rolling_back', kind);
      assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db before the update', kind);
    } finally {
      cleanup(w);
    }
  }
});

test('applying.json that names no place, or this place without a record, is still dropped', async () => {
  const cases = [
    ['no target', (w) => ({ target: undefined }), /its target is not where LectureFlow is installed/],
    ['a relative target', (w) => ({ target: 'Apps\\LectureFlow.exe' }), /its target is not where LectureFlow is installed/],
    ['this place, no record', (w) => ({ target: w.install.target }), /did not start an installation/],
  ];
  for (const [name, over, why] of cases) {
    const w = world();
    try {
      fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({
        schema: 1, version: '3.2.0', from_version: '3.1.0', platform: 'windows-x64', kind: 'installer', phase: 'installing', tries: 1,
        exe: 'LectureFlow.exe', ...over(w),
      }));
      const { a, ops, logs } = applier(w);
      assert.deepEqual(await a.atStart(), { action: 'continue' }, name);
      assert.equal(has(U(w, 'applying.json')), false, name);
      assert.equal(has(U(w, 'result.json')), false, name);
      assert.deepEqual(ops.calls, [], name);
      assert.ok(logs.some((l) => /ignoring applying\.json/.test(l) && why.test(l)), `${name}: ${logs.join('\n')}`);
    } finally {
      cleanup(w);
    }
  }
});

/**
 * plantInDataFolder(w): what an account that once could write to the data
 * folder leaves when the shell's profile lies in it (security review SEC17
 * 中-B): an "installation on probation" of the running version in
 * applying.json and in the profile's record, a backup of an "earlier
 * version" with its program, its hash in that record, previous.json naming
 * it — and then it removes every trace of itself (its permissions), so
 * the data folder looks private and the user never restricts it. pkg:
 * previous.json also names a package (unsigned: the service program
 * refuses it) or a verified one ('signed').
 */
async function plantInDataFolder(w, { pkg = null } = {}) {
  const from = '3.1.0';
  const profile = path.join(w.dataDir, 'electron');
  const b = U(w, 'backup', from);
  fs.cpSync(w.install.target, path.join(b, 'app'), { recursive: true });
  fs.writeFileSync(path.join(b, 'app', 'LectureFlow.exe'), 'EVIL exe');
  fs.writeFileSync(path.join(b, 'app', 'resources', 'app.asar'), 'EVIL asar');
  fs.writeFileSync(path.join(b, 'backup.json'), JSON.stringify({ schema: 1, version: from, kind: 'installer', item: '.', created_at: '2026-10-01T00:00:00.000Z' }));
  const tree = await F.treeHash(path.join(b, 'app'));
  fs.mkdirSync(profile, { recursive: true });
  fs.writeFileSync(path.join(profile, F.TRUST_FILE), JSON.stringify({
    schema: 1,
    install: { version: w.version, from_version: from, target: w.install.target, kind: 'installer', started_at: '2026-10-01T00:00:00.000Z' },
    backups: { [from]: { dir: `backup/${from}`, kind: 'installer', item: '.', tree: tree.sha256, files: tree.files, bytes: tree.bytes, created_at: '2026-10-01T00:00:00.000Z' } },
  }));
  const prev = { schema: 1, version: from, platform: w.install.platform, kind: 'installer', backup: `backup/${from}`, created_at: '2026-10-01T00:00:00.000Z' };
  if (pkg) Object.assign(prev, { file: `${from}/LectureFlow-Setup-${from}-x64.exe`, size: 1, sha256: 'b'.repeat(64) });
  fs.writeFileSync(U(w, 'previous.json'), JSON.stringify(prev));
  fs.writeFileSync(U(w, 'applying.json'), JSON.stringify({
    schema: 1, version: w.version, from_version: from, channel: 'stable', platform: w.install.platform, kind: 'installer',
    file: `${w.version}/x.exe`, size: 1, sha256: 'a'.repeat(64), phase: 'installing', tries: 0, target: w.install.target, exe: 'LectureFlow.exe',
    started_at: '2026-10-01T00:00:00.000Z',
  }));
  return profile;
}

test('SEC17 中-B: a rollback state prepared in the data folder, traces removed, never restricted: two failed starts run nothing of it', async () => {
  const w = world({ version: '3.2.0' });
  try {
    const profile = await plantInDataFolder(w);
    // the service program finds the data folder private (the traces are
    // gone) and refuses previous.json's unsigned package
    const verify = (dir, v, o) => {
      if (o.previous) return { code: 1, stdout: 'invalid selfupdate.not_available: previous.json names no package of 3.1.0\n', stderr: '', timedOut: false, error: null };
      return goVerifies(dir, v, o);
    };
    const ops = fakeOps({ verify });
    const s = new SelfUpdateApplier({
      install: w.install, currentVersion: '3.2.0', dataDir: w.dataDir, ops, log: { info() {}, warn() {}, error() {} }, now: () => NOW,
      privateDir: profile, trustDir: w.trustDir, tmpDir: w.tmp,
    });
    let r = { action: 'none' };
    for (let i = 0; i < 2; i++) { // two failed starts (bounded)
      const at = await s.atStart();
      if (at.action !== 'probation') break;
      r = await s.failed('the service stopped while starting (exit code 1): config.toml: invalid');
    }
    assert.notEqual(r.action, 'exit', 'no rollback is started');
    assert.deepEqual(ops.calls, [], 'nothing is run');
    assert.deepEqual(ops.verifies.filter((v) => v.private), [], 'no private copy of the backup is made');
    assert.deepEqual(copies(w), []);
    assert.deepEqual(installedFiles(w), ['electron 3.2.0', 'asar 3.2.0']);
    assert.deepEqual(trust(w).backups || {}, {}, 'the prepared backup record is not taken over');
  } finally {
    cleanup(w);
  }
});

test('SEC17 中-B: at worst a prepared record rolls back to a previous version the service program verified (an older signed one)', async () => {
  const w = world({ version: '3.2.0' });
  try {
    const profile = await plantInDataFolder(w, { pkg: 'signed' });
    // previous.json's package passes the check with the built-in key
    const signed = pkg(w, '3.1.0', 'LectureFlow-Setup-3.1.0-x64.exe', 'installer package 3.1.0 (signed)');
    fs.writeFileSync(U(w, 'previous.json'), JSON.stringify({ ...readJ(U(w, 'previous.json')), ...signed }));
    const ops = fakeOps();
    const s = new SelfUpdateApplier({
      install: w.install, currentVersion: '3.2.0', dataDir: w.dataDir, ops, log: { info() {}, warn() {}, error() {} }, now: () => NOW,
      privateDir: profile, trustDir: w.trustDir, tmpDir: w.tmp,
    });
    let r = { action: 'none' };
    for (let i = 0; i < 2; i++) {
      assert.equal((await s.atStart()).action, 'probation');
      r = await s.failed('the service stopped while starting (exit code 1): config.toml: invalid');
    }
    assert.equal(r.action, 'exit');
    const spawned = ops.calls.filter((c) => c.op === 'spawn');
    assert.equal(spawned.length, 1);
    assert.ok(isCopy(w, spawned[0].file, 'LectureFlow-Setup-3.1.0-x64.exe'), `the verified package's private copy is run: ${spawned[0].file}`);
    assert.deepEqual(ops.verifies.filter((v) => v.private), [], 'the prepared backup is never copied or used');
    assert.equal(readJ(U(w, 'applying.json')).restore, 'file');
  } finally {
    cleanup(w);
  }
});

// ---- the installation folder (security audit M-4) ----

test('installDirOf: only the Windows installation set up by the installer has one', () => {
  const win = { platform: 'windows-x64', kind: 'installer', kinds: ['installer'], target: 'D:\\Apps\\LectureFlow', exe: 'LectureFlow.exe' };
  assert.equal(F.installDirOf(win), 'D:\\Apps\\LectureFlow');
  for (const [what, install] of [
    ['development', null],
    ['a copy that cannot update itself', { unsupported: 'this copy of LectureFlow was not set up by its installer; install the new version by hand' }],
    ['the portable build', { platform: 'windows-x64', kind: 'portable', kinds: ['portable'], target: 'D:\\Tools\\LectureFlow-portable.exe', exe: 'LectureFlow.exe' }],
    ['macOS (a known limitation)', { platform: 'darwin-arm64', kind: 'zip', kinds: ['zip', 'dmg'], target: '/Applications/LectureFlow.app', exe: 'LectureFlow' }],
    ['no target', { ...win, target: '' }],
  ]) {
    assert.equal(F.installDirOf(install), null, what);
  }
  // describeInstallation's Windows installation is one
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-inst-'));
  try {
    fs.writeFileSync(path.join(dir, 'Uninstall LectureFlow.exe'), '');
    fs.mkdirSync(path.join(dir, 'resources'));
    const exe = path.join(dir, 'LectureFlow.exe');
    assert.equal(F.installDirOf(F.describeInstallation({ isPackaged: true, platform: 'win32', arch: 'x64', execPath: exe, env: {} })), dir);
    assert.equal(F.installDirOf(F.describeInstallation({ isPackaged: false, platform: 'win32', arch: 'x64', execPath: exe })), null, 'development');
    assert.equal(F.installDirOf(F.describeInstallation({ isPackaged: true, platform: 'win32', arch: 'x64', execPath: exe, env: { PORTABLE_EXECUTABLE_FILE: 'D:\\LF.exe' } })), dir, 'PORTABLE_EXECUTABLE_FILE next to the uninstaller: still the installation (security audit FB1 L-13)');
    const unpacked = path.join(dir, 'portable-unpacked');
    fs.mkdirSync(unpacked);
    assert.equal(F.installDirOf(F.describeInstallation({ isPackaged: true, platform: 'win32', arch: 'x64', execPath: path.join(unpacked, 'LectureFlow.exe'), env: { PORTABLE_EXECUTABLE_FILE: 'D:\\LF.exe' } })), null, 'portable');
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('every check of an installer installation names its folder; none for the portable build or macOS', async () => {
  for (const kind of ['installer', 'portable', 'zip']) {
    const w = world({ kind });
    try {
      writePending(w, { content: kind === 'zip' ? '3.2.0' : undefined });
      writePrevious(w);
      const told = [];
      const verify = (dir, v, o) => {
        told.push(o.installDir === undefined ? null : o.installDir);
        return goVerifies(dir, v, o);
      };
      const { a } = applier(w, { ops: fakeOps({ verify }) });
      await a.atStart();
      assert.ok(told.length > 0, kind);
      const want = kind === 'installer' ? w.install.target : null;
      assert.ok(told.every((d) => d === want), `${kind}: ${JSON.stringify(told)}`);
    } finally {
      cleanup(w);
    }
  }
});

const UNTRUSTED_INSTALL = {
  code: 3, stdout: 'untrusted_install_dir: selfupdate.untrusted_install_dir: other accounts of this computer can change LectureFlow\'s installation folder D:\\Apps (NT AUTHORITY\\Authenticated Users)\n', stderr: '', timedOut: false, error: null,
};

test('an installation folder others can change: nothing installed, pending.json kept, the same at every start (no loop)', async () => {
  const w = world();
  try {
    const pending = writePending(w);
    const prev = writePrevious(w);
    let checks = 0;
    const verify = (dir, v, o) => {
      checks += 1;
      assert.equal(o.installDir, w.install.target);
      return UNTRUSTED_INSTALL;
    };
    for (let start = 1; start <= 3; start++) {
      const { a, ops } = applier(w, { ops: fakeOps({ verify }) });
      assert.deepEqual(await a.atStart(), { action: 'continue' }, `start ${start}`);
      assert.equal(checks, start, 'one check per start');
      assert.deepEqual(ops.calls, [], 'nothing run');
      assert.deepEqual(readJ(U(w, 'pending.json')), pending, 'kept: installed once the folder is fixed');
      const r = readJ(U(w, 'result.json'));
      assert.equal(r.outcome, 'failed');
      assert.match(r.error, /installation folder: nothing is installed or rolled back.*by hand.*D:\\Apps/);
      assert.match(r.error, /not installed at this start$/);
      assert.equal(has(U(w, 'applying.json')), false);
      assert.equal(has(U(w, 'backup')), false);
      assert.deepEqual(readJ(U(w, 'previous.json')), prev);
      assert.deepEqual(copies(w), []);
      assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    }
    // fixed: the next start installs it
    const { a } = applier(w);
    assert.equal((await a.atStart()).action, 'exit');
  } finally {
    cleanup(w);
  }
});

test('no rollback while others can change the installation folder: reported, nothing run, the database stays', async () => {
  const w = await installed({ withFile: true });
  try {
    const verify = (dir, v, o) => (o.previous || o.privateDir ? UNTRUSTED_INSTALL : goVerifies(dir, v, o));
    const { s, r } = await failTwice(w, fakeOps({ verify }));
    assert.equal(r.action, 'giveup');
    assert.match(r.error, /installation folder/);
    assert.deepEqual(s.ops.calls, []);
    assert.equal(readJ(U(w, 'result.json')).outcome, 'failed');
    assert.equal(fs.readFileSync(path.join(w.dataDir, 'lectureflow.db'), 'utf8'), 'db migrated by 3.2.0', 'the database is not put back');
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.2.0');
    assert.equal(has(U(w, 'applying.json')), false);
    assert.deepEqual(copies(w), []);
  } finally {
    cleanup(w);
  }
});

// ---- after a rollback from a backup, the installation folder is looked at again (security review SEC20 中-1, 低-A) ----

/**
 * restoredBy(w, verify) → the restore program as it really runs: a copy of
 * the failed version in the private folder, which takes itself for an
 * installation there (main.js describeInstallation by its own execPath), not
 * the installation it restores.
 */
async function restoredBy(w, verify) {
  await failTwice(w);
  const helper = path.join(privateDir(w), 'helper', 'LectureFlow.exe');
  const own = F.describeInstallation({ isPackaged: true, platform: 'win32', arch: 'x64', execPath: helper, env: {} });
  assert.equal(own.kind, 'installer');
  assert.ok(own.target !== w.install.target, 'the restore program\'s own folder is not the installation');
  const told = [];
  const h = applier(w, {
    version: '3.2.0',
    install: own,
    ops: fakeOps({
      verify: (dir, v, o) => {
        if (o.trustCheck) told.push(o.installDir);
        return verify(dir, v, o);
      },
    }),
  });
  assert.equal(await h.a.restoreFromBackup({ execPath: helper }), true);
  return { h, told };
}

test('after a rollback from a backup the installation itself is checked, not the restore program\'s folder', async () => {
  const w = await installed();
  try {
    const { h, told } = await restoredBy(w, goVerifies);
    assert.deepEqual(told, [w.install.target]);
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    assert.deepEqual(h.ops.calls.map((c) => c.op), ['releaseLock', 'spawn']);
    const old = applier(w, { version: '3.1.0' });
    assert.equal((await old.a.atStart()).action, 'continue');
    assert.equal(readJ(U(w, 'result.json')).error, 'the service did not answer within 30 seconds', 'nothing added while it is vouched for');
  } finally {
    cleanup(w);
  }
});

test('after a rollback from a backup an installation folder others can change goes with the rollback\'s reason; the app still starts', async () => {
  const w = await installed();
  try {
    const { h, told } = await restoredBy(w, (dir, v, o) => (o.trustCheck ? UNTRUSTED_INSTALL : goVerifies(dir, v, o)));
    assert.deepEqual(told, [w.install.target]);
    assert.equal(readJ(U(w, 'applying.json')).restore_error, undefined, 'the rollback itself succeeded');
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0', 'put back all the same');
    assert.deepEqual(h.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: path.join(w.install.target, 'LectureFlow.exe'), args: [UPDATED_ARG], verbatim: false }]);
    const old = applier(w, { version: '3.1.0' });
    assert.equal((await old.a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'rolled_back');
    assert.match(r.error, /^the service did not answer within 30 seconds; after going back: other accounts can change the installation folder/);
  } finally {
    cleanup(w);
  }
});

// ---- the check after a rollback has a short time limit of its own (security review SEC20, low-D) ----

test('after a rollback from a backup a check that does not answer is cut short: the rollback goes on and the old version starts', { timeout: 20000 }, async () => {
  const w = await installed();
  try {
    await failTwice(w);
    const helper = path.join(privateDir(w), 'helper', 'LectureFlow.exe');
    const own = F.describeInstallation({ isPackaged: true, platform: 'win32', arch: 'x64', execPath: helper, env: {} });
    const asked = [];
    const h = applier(w, {
      version: '3.2.0',
      install: own,
      // a check program that never answers (and is never told to stop)
      ops: fakeOps({ verify: (dir, v, o) => (o.trustCheck ? (asked.push(o.timeoutMs), new Promise(() => {})) : goVerifies(dir, v, o)) }),
    });
    const byDefault = h.a.restoreCheckTimeoutMs;
    h.a.restoreCheckTimeoutMs = 1000;
    const started = Date.now();
    assert.equal(await h.a.restoreFromBackup({ execPath: helper }), true);
    assert.ok(Date.now() - started < 10000, 'it does not wait for the check');
    assert.equal(byDefault, 15000, 'a short limit by default, not the update check\'s 120 seconds');
    assert.deepEqual(asked, [1000], 'the check program is given the same limit');
    assert.equal(readJ(U(w, 'applying.json')).restore_error, undefined, 'the rollback itself succeeded');
    assert.equal(fs.readFileSync(path.join(w.install.target, 'LectureFlow.exe'), 'utf8'), 'electron 3.1.0');
    assert.deepEqual(h.ops.calls, [{ op: 'releaseLock' }, { op: 'spawn', file: path.join(w.install.target, 'LectureFlow.exe'), args: [UPDATED_ARG], verbatim: false }]);
    const old = applier(w, { version: '3.1.0' });
    assert.equal((await old.a.atStart()).action, 'continue');
    const r = readJ(U(w, 'result.json'));
    assert.equal(r.outcome, 'rolled_back');
    assert.equal(r.error, 'the service did not answer within 30 seconds; after going back: the update check did not finish within 1 seconds');
  } finally {
    cleanup(w);
  }
});

// ---- _replaceDir when the copy fails part way (security review SEC20: the error path had no test) ----

/** tree(dir) → { relative path: content } of every file below dir. */
function tree(dir) {
  const out = {};
  const walk = (d, rel) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      const r = rel ? `${rel}/${e.name}` : e.name;
      if (e.isDirectory()) walk(p, r);
      else out[r] = fs.readFileSync(p, 'utf8');
    }
  };
  walk(dir, '');
  return out;
}

/**
 * replaceFailing(w, { keepHalf }) → what _replaceDir leaves when the copy
 * stops after its first file (EIO); keepHalf: the half copy cannot be
 * removed either.
 */
async function replaceFailing(w, { keepHalf = false } = {}) {
  const parent = path.join(w.root, 'programs');
  const target = path.join(parent, 'LectureFlow');
  const source = path.join(w.root, 'restore-from');
  for (const [dir, v] of [[target, '3.2.0'], [source, '3.1.0']]) {
    fs.mkdirSync(path.join(dir, 'resources', 'bin'), { recursive: true });
    fs.writeFileSync(path.join(dir, 'LectureFlow.exe'), `electron ${v}`);
    fs.writeFileSync(path.join(dir, 'resources', 'app.asar'), `asar ${v}`);
    fs.writeFileSync(path.join(dir, 'resources', 'bin', 'lectureflow.exe'), `service ${v}`);
  }
  const failed = tree(target);
  const { a } = applier(w);
  const { fsp } = F;
  const { cp, rm } = fsp;
  fsp.cp = async (from, to) => {
    fs.mkdirSync(to, { recursive: true });
    fs.copyFileSync(path.join(from, 'LectureFlow.exe'), path.join(to, 'LectureFlow.exe'));
    throw Object.assign(new Error('EIO: i/o error, copyfile'), { code: 'EIO' });
  };
  if (keepHalf) {
    fsp.rm = async (p, o) => {
      if (path.resolve(p) === path.resolve(target)) throw Object.assign(new Error('EBUSY: resource busy or locked'), { code: 'EBUSY' });
      return rm.call(fsp, p, o);
    };
  }
  let err = null;
  try {
    await a._replaceDir(target, source);
  } catch (e) {
    err = e;
  } finally {
    fsp.cp = cp;
    fsp.rm = rm;
  }
  return { parent, target, failed, err };
}

test('a rollback whose copy fails part way puts the failed version back as it was', async () => {
  const w = world();
  try {
    const { parent, target, failed, err } = await replaceFailing(w);
    assert.equal(err && err.code, 'EIO', 'the error is passed on');
    assert.deepEqual(fs.readdirSync(parent), ['LectureFlow'], 'nothing is left beside it');
    assert.deepEqual(tree(target), failed, 'every file of the failed version is back, unchanged');
  } finally {
    cleanup(w);
  }
});

test('a rollback whose copy fails part way and whose half copy cannot be removed keeps the failed version whole beside it', async () => {
  const w = world();
  try {
    const { parent, target, failed, err } = await replaceFailing(w, { keepHalf: true });
    assert.equal(err && err.code, 'EIO', 'the copy\'s error is passed on');
    const aside = fs.readdirSync(parent).filter((n) => n.startsWith('LectureFlow.lf-failed-'));
    assert.equal(aside.length, 1, 'the failed version is kept beside the installation');
    assert.deepEqual(tree(path.join(parent, aside[0])), failed, 'no file of the failed version is lost');
    assert.deepEqual(tree(target), { 'LectureFlow.exe': 'electron 3.1.0' }, 'only the half copy is in the installation\'s place');
  } finally {
    cleanup(w);
  }
});
