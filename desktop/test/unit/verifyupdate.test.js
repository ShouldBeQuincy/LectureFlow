// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The service program's last check of a pending update before it is
// installed (lib/verifyupdate.js; contract: internal/selfupdate/README.md
// "What Electron does at start", cmd/lectureflow/selfupdate_verify.go).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const {
  VERIFY_TIMEOUT_MS, verifyArgs, stripQueries, runVerify, interpretVerify, interpretPrivateDir, copyPath,
} = require('../../src/main/lib/verifyupdate');

const SHA = 'ab'.repeat(32);
const PENDING = { version: '3.2.0', kind: 'installer', file: '3.2.0/LectureFlow-Setup-3.2.0-x64.exe', sha256: SHA };
const ok = (over = {}) => {
  const p = { ...PENDING, ...over };
  return { code: 0, stdout: `ok version=${p.version} kind=${p.kind} file=${p.file} sha256=${p.sha256}\n`, stderr: '', timedOut: false, error: null };
};

test('arguments: the updates folder and the running version, as the README says', () => {
  assert.deepEqual(verifyArgs('C:\\Users\\A B\\LectureFlow\\updates', '3.1.0'), ['-selfupdate-verify', 'C:\\Users\\A B\\LectureFlow\\updates', '-app-version', '3.1.0']);
  assert.deepEqual(verifyArgs('/data/updates', ''), ['-selfupdate-verify', '/data/updates']);
  // sec-update: the previous version's package, and the private copy
  assert.deepEqual(verifyArgs('/data/updates', '3.2.0', { previous: true, copyTo: '/tmp/x y' }),
    ['-selfupdate-verify', '/data/updates', '-app-version', '3.2.0', '-selfupdate-previous', '-selfupdate-copy', '/tmp/x y']);
  assert.equal(VERIFY_TIMEOUT_MS, 120000, 'the check copies the package too');
});

test('query strings and fragments never reach the log', () => {
  assert.equal(stripQueries('see https://h.example/a/b.json?sig=SECRET&x=1 and http://x.test/p#frag end'), 'see https://h.example/a/b.json and http://x.test/p end');
  assert.equal(stripQueries('no url? here # fine'), 'no url? here # fine');
  assert.equal(stripQueries(null), '');
});

test('exit 0 installs only the package pending.json names', () => {
  assert.deepEqual(interpretVerify(ok(), PENDING), { install: true, line: ok().stdout.trim(), copy: null, verdict: 'ok' });
  assert.equal(interpretVerify({ ...ok(), stdout: ok().stdout.replace('\n', '\r\n') }, PENDING).install, true, 'CRLF');
  const spaced = { ...PENDING, file: '3.2.0/LectureFlow Setup 3.2.0.exe' };
  assert.equal(interpretVerify(ok(spaced), spaced).install, true, 'a file name with spaces');
  for (const over of [{ version: '3.2.1' }, { kind: 'portable' }, { file: '3.2.0/other.exe' }, { sha256: 'cd'.repeat(32) }]) {
    const v = interpretVerify(ok(over), PENDING);
    assert.equal(v.install, undefined, JSON.stringify(over));
    assert.match(v.drop, /vouched for .* not the pending 3\.2\.0/, JSON.stringify(over));
  }
  for (const stdout of ['', 'ok\n', `ok version=3.2.0 kind=installer file=${PENDING.file} sha256=${SHA.toUpperCase()}\n`, `x\n${ok().stdout}`]) {
    assert.match(interpretVerify({ code: 0, stdout }, PENDING).keep, /no usable answer \(exit code 0\)/, JSON.stringify(stdout));
  }
});

test('exit 1 "invalid": drop; anything else: no verdict', () => {
  assert.deepEqual(interpretVerify({ code: 1, stdout: 'invalid selfupdate.checksum: the package has 5 bytes, SHA-256 00\n' }, PENDING), {
    drop: 'the update did not pass the check with the built-in key (selfupdate.checksum): the package has 5 bytes, SHA-256 00',
    verdict: 'invalid',
    code: 'selfupdate.checksum',
  });
  assert.match(interpretVerify({ code: 1, stdout: 'invalid : something odd\n' }, PENDING).drop, /\(invalid\): something odd/, 'an error without a code');
  assert.equal(interpretVerify({ code: 1, stdout: 'invalid selfupdate.manifest_invalid: bad url https://e.example/x?t=1\n' }, PENDING).drop.includes('?t='), false);
  assert.deepEqual(interpretVerify({ code: 2, stdout: 'not_configured: this build has no update public key\n' }, PENDING), {
    keep: 'this build has no update public key, so the update cannot be verified here',
    verdict: 'not_configured',
  });
  assert.match(interpretVerify({ code: 2, stdout: 'invalid x: y\n' }, PENDING).keep, /no usable answer \(exit code 2\)/);
  assert.match(interpretVerify({ code: 1, stdout: '', stderr: 'boot\nERROR: flag provided but not defined: -selfupdate-verify\n' }, PENDING).keep,
    /no usable answer \(exit code 1\): ERROR: flag provided but not defined: -selfupdate-verify$/);
  assert.match(interpretVerify({ code: 0, timedOut: true }, PENDING).keep, /within 120 seconds/);
  assert.match(interpretVerify({ timedOut: true }, PENDING, { timeoutMs: 5000 }).keep, /within 5 seconds/);
  assert.match(interpretVerify({ code: null, error: 'spawn x ENOENT' }, PENDING).keep, /could not run: spawn x ENOENT/);
  assert.match(interpretVerify(null, PENDING).keep, /no usable answer/);
});

test('exit 3: other accounts can change the data folder: no verdict, nothing installed or rolled back', () => {
  const v = interpretVerify({ code: 3, stdout: 'untrusted_dir: selfupdate.untrusted_dir: other accounts of this computer can change D:\\LF (Authenticated Users)\n' }, PENDING);
  assert.equal(v.verdict, 'untrusted');
  assert.equal(v.install, undefined);
  assert.equal(v.drop, undefined, 'pending.json is kept: it is installed once the permissions are tightened');
  assert.match(v.keep, /other accounts of this computer can change the data folder.*Diagnostics.*Authenticated Users/);
  assert.equal(interpretVerify({ code: 3, stdout: 'invalid x: y\n' }, PENDING).verdict, 'none', 'exit 3 without the untrusted_dir line');
});

// sec-update M3: with copyTo the program copies the package into a new
// private folder there and names it; the shell runs only that copy.
test('the private copy: only one where it was asked for is accepted', () => {
  // A short fixed folder (interpretVerify only compares paths, nothing is
  // created): below a long os.tmpdir() the ok line passes 300 characters and
  // the expected `line` would be the clipped one (pack-dry 2026-10-27).
  const tmp = path.join(path.parse(os.tmpdir()).root, 'lf tmp');
  const name = path.posix.basename(PENDING.file);
  const good = path.join(tmp, 'lectureflow-update-123456', name);
  const withCopy = (copy, over = {}) => ({ ...ok(over), stdout: `${ok(over).stdout.trim()} copy=${copy}\n` });
  assert.deepEqual(interpretVerify(withCopy(good), PENDING, { copyTo: tmp }), {
    install: true, line: withCopy(good).stdout.trim(), copy: path.resolve(good), verdict: 'ok',
  });
  for (const bad of [
    path.join(os.tmpdir(), 'other', 'lectureflow-update-1', name), // another folder
    path.join(tmp, 'lectureflow-update-1', 'evil.exe'), // another name
    path.join(tmp, 'x-1', name), // not a copy folder
    path.join(tmp, name), // not in its own folder
    `lectureflow-update-1/${name}`, // relative
  ]) {
    const v = interpretVerify(withCopy(bad), PENDING, { copyTo: tmp });
    assert.equal(v.install, undefined, bad);
    assert.match(v.keep, /did not copy the package where it was asked to/, bad);
  }
  assert.match(interpretVerify(ok(), PENDING, { copyTo: tmp }).keep, /did not copy the package/, 'no copy at all');
  // vouching for another package: dropped, and the copy is named for removal
  const other = interpretVerify(withCopy(path.join(tmp, 'lectureflow-update-9', name), { sha256: 'cd'.repeat(32) }), PENDING, { copyTo: tmp });
  assert.match(other.drop, /not the pending 3\.2\.0/);
  assert.equal(other.copy, path.resolve(path.join(tmp, 'lectureflow-update-9', name)));
  assert.match(interpretVerify(ok({ version: '3.1.0' }), PENDING, { what: 'previous' }).drop, /not the previous 3\.2\.0/);
  assert.equal(copyPath(good, tmp, PENDING.file), path.resolve(good));
  assert.equal(copyPath(good, '', PENDING.file), null);
});

// ---- running the program ----

function scripts() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-verify-'));
  const write = (name, body) => {
    const f = path.join(dir, name);
    fs.writeFileSync(f, body);
    return f;
  };
  return {
    dir,
    echo: write('echo.js', `process.stdout.write(JSON.stringify({ args: process.argv.slice(2), env: process.env.LF_VERIFY_TEST || '' }) + '\\n');
process.stderr.write('note on stderr\\n');
process.exitCode = Number(process.env.LF_VERIFY_EXIT || 0);
`),
    hang: write('hang.js', 'setInterval(() => {}, 1000);\n'),
  };
}

test('-selfupdate-private: an empty private folder for the shell\'s copy of a backup', () => {
  const tmp = path.join(path.parse(os.tmpdir()).root, 'lf tmp');
  assert.deepEqual(verifyArgs('D:\\LF\\updates', '3.2.0', { privateDir: true, copyTo: tmp }),
    ['-selfupdate-verify', 'D:\\LF\\updates', '-app-version', '3.2.0', '-selfupdate-private', '-selfupdate-copy', tmp]);
  // (no package is verified: -selfupdate-previous means nothing with it)
  assert.deepEqual(verifyArgs('u', '', { privateDir: true, previous: true, copyTo: tmp }), ['-selfupdate-verify', 'u', '-selfupdate-private', '-selfupdate-copy', tmp]);
  const run = (over) => ({ code: 0, stdout: '', stderr: '', timedOut: false, error: null, ...over });
  const good = path.join(tmp, 'lectureflow-update-123456');
  assert.deepEqual(interpretPrivateDir(run({ stdout: `ok private=${good}\n` }), { copyTo: tmp }), { dir: path.resolve(good), verdict: 'ok' });
  for (const bad of [
    path.join(os.tmpdir(), 'other', 'lectureflow-update-1'), // another folder
    path.join(tmp, 'x-1'), // not a private folder's name
    path.join(tmp, 'lectureflow-update-1', 'app'), // inside one
    tmp, // the temporary folder itself
    'lectureflow-update-1', // relative
  ]) {
    const v = interpretPrivateDir(run({ stdout: `ok private=${bad}\n` }), { copyTo: tmp });
    assert.equal(v.dir, undefined, bad);
    assert.equal(v.verdict, 'none', bad);
    assert.match(v.error, /did not make the private folder where it was asked to/, bad);
  }
  assert.equal(interpretPrivateDir(run({ stdout: `ok private=${good}\n` }), {}).dir, undefined, 'no folder was asked for');
  // the answer of a package check is not the answer to this question
  assert.equal(interpretPrivateDir(run({ stdout: `ok version=3.2.0 kind=installer file=3.2.0/x.exe sha256=${SHA} copy=${path.join(good, 'x.exe')}\n` }), { copyTo: tmp }).verdict, 'none');
  assert.equal(interpretPrivateDir(run({ stdout: `ok private=${good}\nok private=${good}\n` }), { copyTo: tmp }).verdict, 'none', 'two lines');
  const untrusted = interpretPrivateDir(run({ code: 3, stdout: 'untrusted_dir: other accounts of this computer can change D:\\LF (Users)\n' }), { copyTo: tmp });
  assert.equal(untrusted.verdict, 'untrusted');
  assert.match(untrusted.error, /other accounts of this computer can change the data folder.*D:\\LF \(Users\)/);
  const invalid = interpretPrivateDir(run({ code: 1, stdout: 'invalid selfupdate.io_failed: mkdir: Access is denied.\n' }), { copyTo: tmp });
  assert.equal(invalid.verdict, 'invalid');
  assert.match(invalid.error, /no private folder could be made \(selfupdate\.io_failed\): mkdir: Access is denied\./);
  assert.equal(interpretPrivateDir(run({ timedOut: true, code: null }), { copyTo: tmp }).verdict, 'none');
  assert.match(interpretPrivateDir(run({ error: 'spawn ENOENT', code: null }), { copyTo: tmp }).error, /could not run: spawn ENOENT/);
  assert.match(interpretPrivateDir(run({ code: 2, stdout: 'flag provided but not defined: -selfupdate-private\n' }), { copyTo: tmp }).error, /gave no usable answer \(exit code 2\)/);
  assert.equal(interpretPrivateDir(null, { copyTo: tmp }).verdict, 'none');
});

test('runVerify runs the program with the arguments (spaces kept), the environment and its prefix', async () => {
  const s = scripts();
  try {
    const updates = path.join(s.dir, 'data dir', 'updates');
    const cmd = { command: process.execPath, prefixArgs: [s.echo], env: { ...process.env, LF_VERIFY_EXIT: '2' }, extraEnv: { LF_VERIFY_TEST: 'extra' } };
    const r = await runVerify(cmd, updates, '3.1.0');
    assert.equal(r.code, 2);
    assert.equal(r.timedOut, false);
    assert.equal(r.error, null);
    assert.deepEqual(JSON.parse(r.stdout), { args: ['-selfupdate-verify', updates, '-app-version', '3.1.0'], env: 'extra' });
    assert.equal(r.stderr.trim(), 'note on stderr');
    const tmp = path.join(s.dir, 'temp dir');
    const p = await runVerify(cmd, updates, '3.2.0', { previous: true, copyTo: tmp });
    assert.deepEqual(JSON.parse(p.stdout).args, ['-selfupdate-verify', updates, '-app-version', '3.2.0', '-selfupdate-previous', '-selfupdate-copy', tmp]);
  } finally {
    fs.rmSync(s.dir, { recursive: true, force: true });
  }
});

test('runVerify: a program that hangs is stopped after the timeout; a missing one is an error', async () => {
  const s = scripts();
  try {
    const t0 = Date.now();
    const r = await runVerify({ command: process.execPath, prefixArgs: [s.hang] }, s.dir, '3.1.0', { timeoutMs: 400 });
    assert.equal(r.timedOut, true);
    assert.equal(r.code, null);
    assert.ok(Date.now() - t0 < 10000, 'did not wait for the program');
    assert.match(interpretVerify(r, PENDING, { timeoutMs: 400 }).keep, /did not finish/);

    const missing = await runVerify({ command: path.join(s.dir, 'no-such-program.exe') }, s.dir, '3.1.0');
    assert.equal(missing.code, null);
    assert.match(missing.error, /ENOENT/);
    assert.match(interpretVerify(missing, PENDING).keep, /could not run/);

    const thrown = await runVerify({ command: 'x' }, s.dir, '3.1.0', { spawn: () => { throw new Error('spawn refused'); } });
    assert.equal(thrown.error, 'spawn refused');
  } finally {
    fs.rmSync(s.dir, { recursive: true, force: true });
  }
});

test('-selfupdate-trust-dir on every check, -selfupdate-trust-check alone (SEC17 中-B)', () => {
  const { interpretTrustCheck } = require('../../src/main/lib/verifyupdate');
  const trust = 'C:\\Users\\alice\\AppData\\Local\\LectureFlow\\selfupdate';
  assert.deepEqual(verifyArgs('D:\\LF\\updates', '3.2.0', { copyTo: 'T', trustDir: trust }),
    ['-selfupdate-verify', 'D:\\LF\\updates', '-app-version', '3.2.0', '-selfupdate-copy', 'T', '-selfupdate-trust-dir', trust]);
  assert.deepEqual(verifyArgs('u', '3.2.0', { previous: true, copyTo: 'T', trustDir: trust }),
    ['-selfupdate-verify', 'u', '-app-version', '3.2.0', '-selfupdate-previous', '-selfupdate-copy', 'T', '-selfupdate-trust-dir', trust]);
  assert.deepEqual(verifyArgs('u', '3.2.0', { privateDir: true, copyTo: 'T', trustDir: trust }),
    ['-selfupdate-verify', 'u', '-app-version', '3.2.0', '-selfupdate-private', '-selfupdate-copy', 'T', '-selfupdate-trust-dir', trust]);
  // the check alone: no package, no copy
  assert.deepEqual(verifyArgs('u', '3.2.0', { trustCheck: true, previous: true, copyTo: 'T', trustDir: trust }),
    ['-selfupdate-verify', 'u', '-app-version', '3.2.0', '-selfupdate-trust-check', '-selfupdate-trust-dir', trust]);
  const run = (over) => ({ code: 0, stdout: '', stderr: '', timedOut: false, error: null, ...over });
  assert.deepEqual(interpretTrustCheck(run({ stdout: 'ok trusted\n' })), { ok: true });
  for (const [what, r] of [
    ['untrusted', run({ code: 3, stdout: 'untrusted_dir: the update\'s trust directory X cannot vouch for the record (Users)\n' })],
    ['another line', run({ stdout: 'ok private=C:\\x\n' })],
    ['two lines', run({ stdout: 'ok trusted\nok trusted\n' })],
    ['exit 1', run({ code: 1, stdout: 'ok trusted\n' })],
    ['timeout', run({ code: null, timedOut: true })],
    ['not run', run({ code: null, error: 'spawn ENOENT' })],
  ]) {
    const v = interpretTrustCheck(r);
    assert.equal(v.ok, undefined, what);
    assert.equal(typeof v.error, 'string', what);
  }
  assert.match(interpretTrustCheck(run({ code: 3, stdout: 'untrusted_dir: x\n' })).error, /trust folder/);
});

test('-selfupdate-install-dir on every check; exit 3 "untrusted_install_dir": no verdict, in its own words (security audit M-4)', async () => {
  const { interpretTrustCheck, INSTALL_DIR_FLAG } = require('../../src/main/lib/verifyupdate');
  const inst = 'D:\\Apps\\LectureFlow';
  assert.equal(INSTALL_DIR_FLAG, '-selfupdate-install-dir');
  assert.deepEqual(verifyArgs('u', '3.2.0', { copyTo: 'T', trustDir: 'X', installDir: inst }),
    ['-selfupdate-verify', 'u', '-app-version', '3.2.0', '-selfupdate-copy', 'T', '-selfupdate-trust-dir', 'X', '-selfupdate-install-dir', inst]);
  for (const o of [{ trustCheck: true }, { privateDir: true, copyTo: 'T' }, { previous: true, copyTo: 'T' }]) {
    assert.deepEqual(verifyArgs('u', '3.2.0', { ...o, installDir: inst }).slice(-2), ['-selfupdate-install-dir', inst], JSON.stringify(o));
    assert.ok(!verifyArgs('u', '3.2.0', o).includes(INSTALL_DIR_FLAG), 'only when there is one');
  }
  // runVerify passes it on
  let seen = null;
  const spawn = (cmd, args) => {
    seen = args;
    throw new Error('not run');
  };
  await runVerify({ command: 'x' }, 'u', '3.2.0', { spawn, installDir: inst });
  assert.deepEqual(seen.slice(-2), ['-selfupdate-install-dir', inst]);

  const run = (over) => ({ code: 3, stdout: '', stderr: '', timedOut: false, error: null, ...over });
  const line = 'untrusted_install_dir: selfupdate.untrusted_install_dir: other accounts of this computer can change LectureFlow\'s installation folder D:\\Apps (NT AUTHORITY\\Authenticated Users)\n';
  const v = interpretVerify(run({ stdout: line }), PENDING);
  assert.equal(v.verdict, 'untrusted');
  assert.equal(v.install, undefined);
  assert.equal(v.drop, undefined, 'pending.json is kept');
  assert.match(v.keep, /installation folder: nothing is installed or rolled back.*by hand.*D:\\Apps \(NT AUTHORITY\\Authenticated Users\)/);
  assert.doesNotMatch(v.keep, /data folder/);
  const p = interpretPrivateDir(run({ stdout: line }), { copyTo: os.tmpdir() });
  assert.equal(p.verdict, 'untrusted');
  assert.match(p.error, /installation folder/);
  assert.match(interpretTrustCheck(run({ stdout: line })).error, /installation folder/);
  // only with exit 3, and only as the one line
  for (const r of [run({ code: 1, stdout: line }), run({ code: 0, stdout: line }), run({ stdout: `${line}${line}` })]) {
    assert.equal(interpretVerify(r, PENDING).verdict, 'none');
  }
});

// SEC17 round 4 (low-I): the trust folder and the data folder overlapping
// is a reason of its own: tightening the permissions does not help, moving
// the data folder does. Only the text changes; the verdicts, the shape of
// the results and what the program prints stay as they were.
const OVERLAP_LINE = 'untrusted_dir: selfupdate.untrusted_dir: the update\'s trust directory C:\\Users\\a\\AppData\\Local\\LectureFlow\\selfupdate cannot vouch for the record (it lies in the data directory, or the data directory lies in it)\n';
const OTHERS_LINE = 'untrusted_dir: selfupdate.untrusted_dir: other accounts of this computer can change D:\\LF (Authenticated Users)\n';

test('untrusted_dir: the verdicts and the shape of the results do not depend on the reason', () => {
  const { interpretTrustCheck } = require('../../src/main/lib/verifyupdate');
  const tmp = path.join(path.parse(os.tmpdir()).root, 'lf tmp');
  const run = (over) => ({ code: 0, stdout: '', stderr: '', timedOut: false, error: null, ...over });
  const install = 'untrusted_install_dir: selfupdate.untrusted_install_dir: other accounts of this computer can change D:\\Apps (Users)\n';
  // [what, run, interpretVerify's verdict and keys, interpretPrivateDir's, interpretTrustCheck's keys]
  for (const [what, r, verify, priv, trust] of [
    ['exit 0 ok', ok(), ['ok', 'copy,install,line,verdict'], ['none', 'error,verdict'], 'error'],
    ['exit 0 private', run({ stdout: `ok private=${path.join(tmp, 'lectureflow-update-1')}\n` }), ['none', 'keep,verdict'], ['ok', 'dir,verdict'], 'error'],
    ['exit 0 trusted', run({ stdout: 'ok trusted\n' }), ['none', 'keep,verdict'], ['none', 'error,verdict'], 'ok'],
    ['exit 1 invalid', run({ code: 1, stdout: 'invalid selfupdate.checksum: bad\n' }), ['invalid', 'code,drop,verdict'], ['invalid', 'error,verdict'], 'error'],
    ['exit 2 not_configured', run({ code: 2, stdout: 'not_configured: no key\n' }), ['not_configured', 'keep,verdict'], ['none', 'error,verdict'], 'error'],
    ['exit 3 others', run({ code: 3, stdout: OTHERS_LINE }), ['untrusted', 'keep,verdict'], ['untrusted', 'error,verdict'], 'error'],
    ['exit 3 overlap', run({ code: 3, stdout: OVERLAP_LINE }), ['untrusted', 'keep,verdict'], ['untrusted', 'error,verdict'], 'error'],
    ['exit 3 install dir', run({ code: 3, stdout: install }), ['untrusted', 'keep,verdict'], ['untrusted', 'error,verdict'], 'error'],
    ['exit 3 other line', run({ code: 3, stdout: 'invalid x: y\n' }), ['none', 'keep,verdict'], ['none', 'error,verdict'], 'error'],
    ['overlap line with exit 1', run({ code: 1, stdout: OVERLAP_LINE }), ['none', 'keep,verdict'], ['none', 'error,verdict'], 'error'],
    ['overlap line with exit 0', run({ stdout: OVERLAP_LINE }), ['none', 'keep,verdict'], ['none', 'error,verdict'], 'error'],
    ['overlap line twice', run({ code: 3, stdout: OVERLAP_LINE + OVERLAP_LINE }), ['none', 'keep,verdict'], ['none', 'error,verdict'], 'error'],
    ['timeout', run({ code: null, timedOut: true }), ['none', 'keep,verdict'], ['none', 'error,verdict'], 'error'],
    ['not run', run({ code: null, error: 'spawn ENOENT' }), ['none', 'keep,verdict'], ['none', 'error,verdict'], 'error'],
  ]) {
    const v = interpretVerify(r, PENDING);
    assert.equal(v.verdict, verify[0], `interpretVerify ${what}`);
    assert.equal(Object.keys(v).sort().join(','), verify[1], `interpretVerify ${what}`);
    const p = interpretPrivateDir(r, { copyTo: tmp });
    assert.equal(p.verdict, priv[0], `interpretPrivateDir ${what}`);
    assert.equal(Object.keys(p).sort().join(','), priv[1], `interpretPrivateDir ${what}`);
    assert.equal(Object.keys(interpretTrustCheck(r)).sort().join(','), trust, `interpretTrustCheck ${what}`);
  }
});

test('untrusted_dir because the trust folder and the data folder overlap: move the data folder, not "tighten its permissions"', () => {
  const { interpretTrustCheck } = require('../../src/main/lib/verifyupdate');
  const r = { code: 3, stdout: OVERLAP_LINE, stderr: '', timedOut: false, error: null };
  // (the folder the program names; the rest of its reason is what the text says)
  const reason = 'C:\\Users\\a\\AppData\\Local\\LectureFlow\\selfupdate';
  const texts = {
    verify: interpretVerify(r, PENDING).keep,
    private: interpretPrivateDir(r, { copyTo: os.tmpdir() }).error,
    trust: interpretTrustCheck(r).error,
  };
  assert.equal(texts.verify, `the data folder overlaps %LOCALAPPDATA%\\LectureFlow, where the update record is kept, so no update is installed from it (move the data folder somewhere else): ${reason}`);
  assert.equal(texts.private, `the data folder overlaps %LOCALAPPDATA%\\LectureFlow, where the update record is kept, so nothing in the data folder is used (move the data folder somewhere else): ${reason}`);
  assert.equal(texts.trust, `the data folder overlaps %LOCALAPPDATA%\\LectureFlow, where the update record is kept (move the data folder somewhere else): ${reason}`);
  for (const [k, text] of Object.entries(texts)) {
    assert.doesNotMatch(text, /tighten|other accounts/, k);
  }
  // a reason in other words that still says so: the whole reason follows
  const odd = { ...r, stdout: 'untrusted_dir: the record cannot be kept (it lies in the data directory, or the data directory lies in it)\n' };
  assert.equal(interpretTrustCheck(odd).error,
    'the data folder overlaps %LOCALAPPDATA%\\LectureFlow, where the update record is kept (move the data folder somewhere else): the record cannot be kept (it lies in the data directory, or the data directory lies in it)');
});

test('untrusted_dir for any other reason: the text it always had', () => {
  const { interpretTrustCheck } = require('../../src/main/lib/verifyupdate');
  // (short: the text is cut at 300 characters)
  const linkLine = 'untrusted_dir: the update\'s trust directory C:\\T cannot vouch for the record (not confirmed: it is a link)\n';
  // a data folder whose path holds the words of the overlap reason, while
  // the reason is another one: it ends with the accounts (review R47 S1)
  const pathLine = 'untrusted_dir: other accounts of this computer can change D:\\x (it lies in the data directory, or the data directory lies in it)\\LF (Authenticated Users)\n';
  for (const line of [OTHERS_LINE, linkLine, pathLine]) {
    const why = line.trim().replace(/^untrusted_dir: /, '');
    const r = { code: 3, stdout: line, stderr: '', timedOut: false, error: null };
    assert.equal(interpretVerify(r, PENDING).keep, `other accounts of this computer can change the data folder, so no update is installed from it (tighten its permissions in Diagnostics): ${why}`);
    assert.equal(interpretPrivateDir(r, { copyTo: os.tmpdir() }).error, `other accounts of this computer can change the data folder, so nothing in it is used (tighten its permissions in Diagnostics): ${why}`);
    assert.equal(interpretTrustCheck(r).error, `other accounts of this computer can change the data folder or the update's trust folder: ${why}`);
  }
});
