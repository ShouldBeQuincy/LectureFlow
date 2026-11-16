// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// PORTABLE_EXECUTABLE_FILE counts only where no installer's uninstaller
// lies next to the executable (security audit FB1 L-13): otherwise the
// installed app could be told by an environment variable that it is the
// portable build and skip what only an installation gets (the installation
// directory's permission check, installDirOf).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const F = require('../../src/main/lib/updatefiles');

const PORTABLE = 'D:\\Apps\\LectureFlow-3.2.0-x64-portable.exe';

function lister(names) {
  return () => names;
}

test('portableExecutable: only the packaged Windows app, only without an uninstaller next to it', () => {
  const execPath = 'C:\\Temp\\2x\\LectureFlow.exe';
  const env = { PORTABLE_EXECUTABLE_FILE: ` ${PORTABLE} ` };
  const base = { isPackaged: true, platform: 'win32', execPath, env };
  assert.equal(F.portableExecutable({ ...base, readdirSync: lister(['LectureFlow.exe', 'resources']) }), PORTABLE, 'the portable build unpacks into a folder without one');
  assert.equal(F.portableExecutable({ ...base, readdirSync: lister(['LectureFlow.exe', 'resources', 'Uninstall LectureFlow.exe']) }), '', 'an installation');
  assert.equal(F.portableExecutable({ ...base, readdirSync: lister(['uninstall lectureflow.EXE']) }), '', 'any case');
  assert.equal(F.portableExecutable({ ...base, readdirSync: () => { throw new Error('EACCES'); } }), '', 'a folder that cannot be read does not count as portable');
  assert.equal(F.portableExecutable({ ...base, isPackaged: false, readdirSync: lister([]) }), '');
  assert.equal(F.portableExecutable({ ...base, platform: 'darwin', readdirSync: lister([]) }), '');
  assert.equal(F.portableExecutable({ ...base, env: {}, readdirSync: lister([]) }), '');
  assert.equal(F.portableExecutable({ ...base, env: { PORTABLE_EXECUTABLE_FILE: '  ' }, readdirSync: lister([]) }), '');
  let looked = '';
  F.portableExecutable({ ...base, readdirSync: (d) => { looked = d; return []; } });
  assert.equal(looked, path.dirname(execPath), 'the folder of the running executable');
});

test('describeInstallation: an installation stays one whatever PORTABLE_EXECUTABLE_FILE says', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-portable-'));
  try {
    const installed = path.join(root, 'Programs', 'LectureFlow');
    fs.mkdirSync(path.join(installed, 'resources'), { recursive: true });
    fs.writeFileSync(path.join(installed, 'Uninstall LectureFlow.exe'), '');
    const unpacked = path.join(root, 'Temp', '2x');
    fs.mkdirSync(path.join(unpacked, 'resources'), { recursive: true });
    const describe = (dir, env) => F.describeInstallation({ isPackaged: true, platform: 'win32', arch: 'x64', execPath: path.join(dir, 'LectureFlow.exe'), env });
    const env = { PORTABLE_EXECUTABLE_FILE: PORTABLE };
    const inst = describe(installed, env);
    assert.equal(inst.kind, 'installer');
    assert.equal(inst.target, installed);
    assert.equal(F.installDirOf(inst), installed, 'the installation directory is still checked');
    const port = describe(unpacked, env);
    assert.equal(port.kind, 'portable');
    assert.equal(port.target, PORTABLE);
    assert.equal(F.installDirOf(port), null);
    assert.match(describe(unpacked, {}).unsupported, /not set up by its installer/);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('main.js drops PORTABLE_EXECUTABLE_FILE of an installation before anything reads it', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'main.js'), 'utf8');
  const fn = src.slice(src.indexOf('function dropDevEnvironment'), src.indexOf('function main() {'));
  assert.match(fn, /portableExecutable\(\{ isPackaged: app\.isPackaged, platform: process\.platform, execPath: process\.execPath, env: process\.env \}\)/);
  assert.match(fn, /delete process\.env\.PORTABLE_EXECUTABLE_FILE/);
});
