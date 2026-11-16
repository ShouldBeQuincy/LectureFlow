// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The shell's side of the browser extension's Native Messaging (SPEC-ext,
// .claude/ext-brief.md §4): the launcher and portable arguments, the
// --background start, the "LECTUREFLOW_OPEN <route>" line and the NSIS
// uninstall additions.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { once } = require('node:events');

const nm = require('../../src/main/lib/nmhost');
const { buildServerArgs } = require('../../src/main/lib/serverutil');
const { ServerProcess } = require('../../src/main/server');

const DESKTOP = path.join(__dirname, '..', '..');
const FAKE = path.join(__dirname, '..', 'fixtures', 'fake-server.js');

test('isBackgroundLaunch: --background in argv or the single-instance data', () => {
  assert.equal(nm.isBackgroundLaunch(['--background']), true);
  assert.equal(nm.isBackgroundLaunch(['C:\\LF\\LectureFlow.exe', '--background']), true);
  assert.equal(nm.isBackgroundLaunch(['C:\\LF\\LectureFlow.exe']), false);
  assert.equal(nm.isBackgroundLaunch(['--background=1', '-background', '--Background']), false);
  assert.equal(nm.isBackgroundLaunch([], { background: true }), true);
  assert.equal(nm.isBackgroundLaunch(['x'], { background: false }), false);
  assert.equal(nm.isBackgroundLaunch(undefined, undefined), false);
});

test('launcherFor and isPortable', () => {
  const env = { PORTABLE_EXECUTABLE_FILE: 'D:\\Apps\\LectureFlow-3.2.0-x64-portable.exe' };
  // Installed Windows build: the app's own executable.
  assert.equal(nm.launcherFor({ isPackaged: true, platform: 'win32', execPath: 'C:\\Users\\u\\AppData\\Local\\Programs\\LectureFlow\\LectureFlow.exe' }),
    'C:\\Users\\u\\AppData\\Local\\Programs\\LectureFlow\\LectureFlow.exe');
  assert.equal(nm.isPortable({ isPackaged: true, platform: 'win32', env: {} }), false);
  // Portable: the portable exe, not the temporary unpacked copy.
  assert.equal(nm.launcherFor({ isPackaged: true, platform: 'win32', execPath: 'C:\\Temp\\2x\\LectureFlow.exe', env }), env.PORTABLE_EXECUTABLE_FILE);
  assert.equal(nm.isPortable({ isPackaged: true, platform: 'win32', env }), true);
  // macOS: the executable inside the bundle.
  assert.equal(nm.launcherFor({ isPackaged: true, platform: 'darwin', execPath: '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow' }),
    '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow');
  assert.equal(nm.isPortable({ isPackaged: true, platform: 'darwin', env }), false);
  // Development: the bare Electron cannot start the app with --background.
  assert.equal(nm.launcherFor({ isPackaged: false, platform: 'win32', execPath: 'electron.exe', env }), null);
  assert.equal(nm.isPortable({ isPackaged: false, platform: 'win32', env }), false);
});

test('buildServerArgs passes -launcher and -portable only when set', () => {
  const base = { port: 5, dataDir: 'd', resourcesDir: 'r', appVersion: 'v', configFile: 'c', locationFile: null };
  assert.deepEqual(buildServerArgs(base), ['-desktop', '-listen', '127.0.0.1:5', '-data-dir', 'd', '-resources', 'r', '-app-version', 'v', '-config', 'c']);
  const args = buildServerArgs({ ...base, launcher: 'C:\\LF\\LectureFlow.exe', portable: true });
  assert.deepEqual(args.slice(-3), ['-launcher', 'C:\\LF\\LectureFlow.exe', '-portable']);
  // macOS from the disk image: why there is no launcher (mac-fix)
  assert.deepEqual(buildServerArgs({ ...base, launcher: null, launcherBlocked: 'dmg' }).slice(-2), ['-launcher-blocked', 'dmg']);
  // a launcher wins (they never come together)
  assert.ok(!buildServerArgs({ ...base, launcher: '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow', launcherBlocked: 'dmg' }).includes('-launcher-blocked'));
});

test('buildServerArgs passes -install-dir only when there is one (security audit M-4)', () => {
  const base = { port: 5, dataDir: 'd', resourcesDir: 'r', appVersion: 'v', configFile: 'c', locationFile: null };
  assert.ok(!buildServerArgs(base).includes('-install-dir'));
  assert.deepEqual(buildServerArgs({ ...base, installDir: 'D:\\Apps\\LectureFlow' }).slice(-2), ['-install-dir', 'D:\\Apps\\LectureFlow']);
  const shell = fs.readFileSync(path.join(DESKTOP, 'src', 'main', 'shell.js'), 'utf8');
  assert.match(shell, /installDir: this\.installDir,/);
  const main = fs.readFileSync(path.join(DESKTOP, 'src', 'main', 'main.js'), 'utf8');
  assert.match(main, /installDir: installDirOf\(install\)/);
});

// mac-fix: a macOS app that runs from its disk image or translocated has a
// path that disappears; launch.json must not name it.
test('launcherBlocked and launcherFor on macOS: translocated and disk image', () => {
  const mac = (execPath, readOnly = () => false) => ({ isPackaged: true, platform: 'darwin', execPath, readOnly });
  const installed = '/Applications/LectureFlow.app/Contents/MacOS/LectureFlow';
  const translocated = '/private/var/folders/ab/xyz/T/AppTranslocation/6E1F0C2A-1B2C-4D3E-8F90-0A1B2C3D4E5F/d/LectureFlow.app/Contents/MacOS/LectureFlow';
  const dmg = '/Volumes/LectureFlow 3.2.0-arm64/LectureFlow.app/Contents/MacOS/LectureFlow';
  const external = '/Volumes/Work/Apps/LectureFlow.app/Contents/MacOS/LectureFlow';
  const seen = [];
  const roDmg = (dir) => {
    seen.push(dir);
    return dir === '/Volumes/LectureFlow 3.2.0-arm64';
  };

  assert.equal(nm.launcherBlocked(mac(installed, roDmg)), null);
  assert.equal(nm.launcherFor(mac(installed, roDmg)), installed);
  assert.equal(nm.launcherBlocked(mac(translocated, roDmg)), 'translocated');
  assert.equal(nm.launcherFor(mac(translocated, roDmg)), null);
  assert.equal(nm.launcherBlocked(mac(dmg, roDmg)), 'dmg');
  assert.equal(nm.launcherFor(mac(dmg, roDmg)), null);
  // a writable external disk is the user's choice
  assert.equal(nm.launcherBlocked(mac(external, roDmg)), null);
  assert.equal(nm.launcherFor(mac(external, roDmg)), external);
  // only the volume's root is asked, only for /Volumes paths
  assert.deepEqual([...new Set(seen)].sort(), ['/Volumes/LectureFlow 3.2.0-arm64', '/Volumes/Work']);

  // Windows, development: never blocked; Windows keeps its launcher
  assert.equal(nm.launcherBlocked({ isPackaged: true, platform: 'win32', execPath: 'C:\\AppTranslocation\\LectureFlow.exe', readOnly: () => true }), null);
  assert.equal(nm.launcherBlocked({ isPackaged: false, platform: 'darwin', execPath: translocated }), null);
  assert.equal(nm.launcherFor({ isPackaged: true, platform: 'win32', execPath: 'C:\\LF\\LectureFlow.exe', readOnly: () => true }), 'C:\\LF\\LectureFlow.exe');
});

test('isReadOnlyVolume: a writable directory is not read-only', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-ro-'));
  try {
    assert.equal(nm.isReadOnlyVolume(dir), false);
    // a missing path is not a read-only volume either (ENOENT, not EROFS)
    assert.equal(nm.isReadOnlyVolume(path.join(dir, 'missing')), false);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('parseOpenLine accepts the service routes only', () => {
  assert.equal(nm.parseOpenLine('LECTUREFLOW_OPEN #/lecture/01JABCDEFGHJKMNPQRSTVWXYZ0'), '#/lecture/01JABCDEFGHJKMNPQRSTVWXYZ0');
  assert.equal(nm.parseOpenLine('LECTUREFLOW_OPEN #/course/01J0000000000000000000000Z\r'), '#/course/01J0000000000000000000000Z');
  for (const bad of [
    'LECTUREFLOW_OPEN',
    'LECTUREFLOW_OPEN ',
    'LECTUREFLOW_OPEN https://example.com/',
    'LECTUREFLOW_OPEN #/lecture/1\'; alert(1); //',
    'LECTUREFLOW_OPEN #/lecture/<img>',
    'LECTUREFLOW_OPEN #/../../etc',
    'LECTUREFLOW_OPEN javascript:alert(1)',
    `LECTUREFLOW_OPEN #/${'a'.repeat(300)}`,
    'time=2026-10-26 level=INFO msg="LECTUREFLOW_OPEN #/lecture/1"',
    ' LECTUREFLOW_OPEN #/lecture/1',
    null,
  ]) {
    assert.equal(nm.parseOpenLine(bad), null, String(bad));
  }
});

test('ServerProcess emits the service\'s open lines', async () => {
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-nm-'));
  const srv = new ServerProcess({
    command: process.execPath,
    prefixArgs: [FAKE],
    // The real flags reach the fake service like the Go one.
    argsFor: (port) => buildServerArgs({ port, dataDir, resourcesDir: dataDir, appVersion: '9.9.9', configFile: path.join(dataDir, 'config.toml'), locationFile: null, launcher: 'C:\\LF\\LectureFlow.exe', portable: true }),
    env: { ...process.env },
    cwd: dataDir,
  });
  try {
    await srv.start();
    const state = JSON.parse(fs.readFileSync(path.join(dataDir, 'fake-state.json'), 'utf8'));
    assert.deepEqual(state.args[0].slice(-3), ['-launcher', 'C:\\LF\\LectureFlow.exe', '-portable']);
    const opened = once(srv, 'open');
    await fetch(`${srv.origin}/test/open?route=${encodeURIComponent('#/lecture/01JTEST')}`);
    const [route] = await opened;
    assert.equal(route, '#/lecture/01JTEST');
    // A line that is not a valid route is ignored.
    let extra = null;
    srv.on('open', (r) => { extra = r; });
    await fetch(`${srv.origin}/test/open?route=${encodeURIComponent('javascript:alert(1)')}`);
    await new Promise((r) => setTimeout(r, 200));
    assert.equal(extra, null);
  } finally {
    await srv.stop({ timeoutMs: 5000 });
    fs.rmSync(dataDir, { recursive: true, force: true });
  }
});

test('the shell wires background starts, second instances and open routes', () => {
  const main = fs.readFileSync(path.join(DESKTOP, 'src', 'main', 'main.js'), 'utf8');
  assert.match(main, /requestSingleInstanceLock\(\{ background \}\)/);
  assert.match(main, /second-instance', \(event, argv, workingDirectory, additionalData\)/);
  assert.match(main, /!isBackgroundLaunch\(argv, additionalData\)/);
  const shell = fs.readFileSync(path.join(DESKTOP, 'src', 'main', 'shell.js'), 'utf8');
  assert.match(shell, /if \(!this\.quitting && !this\.startHidden\) win\.show\(\)/);
  assert.match(shell, /server\.on\('open'/);
  assert.match(shell, /launcher: launcherFor\(/);
  assert.match(shell, /launcherBlocked: launcherBlocked\(/);
  assert.match(shell, /portable: isPortable\(/);
});

test('NSIS uninstall removes the registration, not during updates', () => {
  const config = require(path.join(DESKTOP, 'electron-builder.config.js'));
  assert.equal(config.nsis.include, 'packaging/installer.nsh');
  const nsh = fs.readFileSync(path.join(DESKTOP, 'packaging', 'installer.nsh'), 'utf8');
  assert.match(nsh, /^# SPDX-License-Identifier: AGPL-3\.0-or-later/);
  assert.match(nsh, /!macro customUnInstall[\s\S]*\$\{ifNot\} \$\{isUpdated\}[\s\S]*\$\{endIf\}[\s\S]*!macroend/);
  for (const key of [
    'Software\\Google\\Chrome\\NativeMessagingHosts\\org.lectureflow.nmhost',
    'Software\\Microsoft\\Edge\\NativeMessagingHosts\\org.lectureflow.nmhost',
    'Software\\Mozilla\\NativeMessagingHosts\\org.lectureflow.nmhost',
  ]) {
    assert.ok(nsh.includes(`DeleteRegKey HKCU "${key}"`), key);
  }
  assert.ok(nsh.includes('RMDir /r "$APPDATA\\LectureFlow\\nmhost"'));
  assert.ok(nsh.includes('RMDir /r "$LOCALAPPDATA\\LectureFlow\\nmhost"'));
  // The in-app update's trust folder (updatefiles.trustDirFor), inside the
  // isUpdated check: an update's own uninstall must keep the record of the
  // update in progress.
  const unInstall = /!macro customUnInstall([\s\S]*?)!macroend/.exec(nsh)[1];
  const guarded = /\$\{ifNot\} \$\{isUpdated\}([\s\S]*?)\$\{endIf\}/.exec(unInstall)[1];
  const trustRm = 'RMDir /r "$LOCALAPPDATA\\LectureFlow\\selfupdate"';
  assert.ok(guarded.includes(trustRm), 'the trust folder is removed only outside updates');
  assert.equal(nsh.split(trustRm).length, 2, 'and nowhere else');
  const F = require(path.join(DESKTOP, 'src', 'main', 'lib', 'updatefiles.js'));
  assert.equal(F.trustDirFor({ platform: 'win32', env: { LOCALAPPDATA: 'C:\\L' } }), 'C:\\L\\LectureFlow\\selfupdate', 'the folder the app uses');
  // Never the data directory itself.
  assert.ok(!/RMDir[^\n]*"\$APPDATA\\LectureFlow"/.test(nsh));
  assert.equal(config.nsis.deleteAppDataOnUninstall, false);
});

// The installation folder's permissions (security audit M-4). The script
// cannot run here (no NSIS): what it does is pinned by its text, and the
// icacls line itself is run on a temporary folder by
// internal/selfupdate/installdir_windows_test.go, which checks the result
// with the app's own check.
test('NSIS install protects an installation folder outside the profile, updates included', () => {
  const config = require(path.join(DESKTOP, 'electron-builder.config.js'));
  // per user, no elevation, the folder may be changed: the case the macro is for
  assert.equal(config.nsis.perMachine, false);
  assert.equal(config.nsis.allowElevation, false);
  assert.equal(config.nsis.allowToChangeInstallationDirectory, true);
  const nsh = fs.readFileSync(path.join(DESKTOP, 'packaging', 'installer.nsh'), 'utf8');
  const install = /!macro customInstall\b([\s\S]*?)!macroend/.exec(nsh);
  assert.ok(install, 'customInstall');
  assert.equal(install[1].trim(), '!insertmacro lfProtectInstallDir', 'every installation, updates too (no isUpdated check)');
  const body = /!macro lfProtectInstallDir\b([\s\S]*?)!macroend/.exec(nsh)[1];
  // exactly one icacls run: the system's icacls, on the installation
  // folder, the private permissions of internal/dirperm, nothing for anyone else
  const runs = body.match(/nsExec::\w+ '[^\n]*'/g);
  assert.deepEqual(runs, [
    'nsExec::ExecToLog \'"$SYSDIR\\icacls.exe" "$R0" /inheritance:r /grant:r "*$R1:(OI)(CI)F" "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" /Q\'',
  ]);
  assert.ok(!/\bExec(Wait|Shell)?\b/.test(body.replace(/nsExec::\w+/g, '')), 'nothing else is run (never a program of the installation folder)');
  const dirperm = fs.readFileSync(path.join(DESKTOP, '..', 'internal', 'dirperm', 'dirperm_windows.go'), 'utf8');
  assert.ok(dirperm.includes('"D:P(A;OICI;FA;;;" + user.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"'), 'the same accounts as a private data folder');
  // $R0 is the full path of $INSTDIR; $R1 the user's SID (TOKEN_USER) or nothing
  assert.match(body, /GetFullPathName \$R0 "\$INSTDIR"/);
  assert.match(body, /advapi32::GetTokenInformation\(p R3, i 1, p R5/);
  assert.match(body, /ConvertSidToStringSidW/);
  assert.match(body, /StrCpy \$R2 \$R1 4\s+\$\{if\} \$R2 != "S-1-"/, 'no SID: nothing is set');
  // left as they are: an installation for all users (Program Files: other
  // accounts must keep reading and running it; SEC20 中-3) before anything
  // else, the profile and %LOCALAPPDATA% (the default), the root of a
  // drive, a network folder
  assert.match(body, /\$\{if\} \$installMode == "all"\s+DetailPrint "[^"\n]*left as they are"\s+\$\{elseIf\} \$R3 <= 3/);
  assert.ok(body.indexOf('$installMode == "all"') < body.indexOf('icacls.exe'), 'skipped before icacls');
  assert.equal(body.split('$installMode').length, 2, 'only that test of the mode');
  assert.match(body, /!insertmacro lfInside \$R0 "\$PROFILE" \$R1/);
  assert.match(body, /!insertmacro lfInside \$R0 "\$LOCALAPPDATA" \$R4/);
  assert.match(body, /\$\{elseIf\} \$R3 <= 3\s+\$\{orIf\} \$R2 == "\\\\"/);
  // "...\Local" does not take in "...\Local2": compared with a backslash after both
  assert.match(nsh, /StrLen \$R6 "\$R5\\"\s+StrCpy \$R6 "\$\{DIR\}\\" \$R6\s+\$\{if\} \$R6 == "\$R5\\"/);
  // the registers it uses are given back; no variables or functions of its
  // own (electron-builder treats NSIS warnings, such as an unused one, as errors)
  const regs = ['R0', 'R1', 'R2', 'R3', 'R4', 'R5', 'R6'];
  assert.ok(body.trimStart().startsWith(regs.map((r) => `Push $${r}`).join('\n  ')), 'saved first');
  assert.ok(body.trimEnd().endsWith([...regs].reverse().map((r) => `Pop $${r}`).join('\n  ')), 'given back last');
  assert.equal(body.match(/\bPush\b/g).length, regs.length, 'nothing else pushed');
  // the two other Pops take System::Alloc's address and icacls's exit code
  assert.equal(body.match(/\bPop\b/g).length, regs.length + 2);
  assert.match(body, /System::Alloc \$R4\s+Pop \$R5/);
  assert.match(body, /\/Q'\s+Pop \$R2/);
  assert.match(body, /System::Free \$R5/);
  assert.ok(!/^\s*(Var|Function)\b/m.test(nsh));
  // the uninstall part is unchanged: the folder's permissions need nothing
  assert.ok(!/icacls/.test(/!macro customUnInstall([\s\S]*?)!macroend/.exec(nsh)[1]));
});
