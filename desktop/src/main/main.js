// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Entry point of the LectureFlow desktop shell (SPEC-v3 V1, V2).
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { app, dialog, Menu } = require('electron');

const { resolveDataDir } = require('./lib/datadir');
const { devEnvToDrop, startRefusal } = require('./lib/debugflags');
const { LOCALE_FILE, createShellTranslator, loadTranslator, readLocaleMemo } = require('./lib/i18n');
const { buildMenuTemplate } = require('./lib/menu');
const { isBackgroundLaunch } = require('./lib/nmhost');
const { resolveServerCommand } = require('./lib/serverutil');
const { describeInstallation, installDirOf, portableExecutable } = require('./lib/updatefiles');
const { runVerify } = require('./lib/verifyupdate');
const { createLogger } = require('./logger');
const {
  SelfUpdateApplier, electronOps, recordingOps, testInstallFrom, RESTORE_ARG, UPDATED_ARG,
} = require('./selfupdate');
const { Shell, REPO_ROOT, appDataRoot } = require('./shell');
const { noSpellcheckDownloads } = require('./websecurity');

// Windows notifications are grouped under this id; keep it equal to the
// appId of the installer configuration (step 8).
const APP_ID = 'org.lectureflow.desktop';
// A version started by an update (--updated) waits this long for the
// previous one to quit before giving up its start.
const LOCK_RETRY_MS = 500;
const LOCK_RETRIES = 40;

const PACKAGED_UPDATER_NOTE = 'updates: the service did not offer in-app updates (signed update.json); the packaged app does not load electron-updater, so automatic updates are off';

/**
 * loadUpdater: the updater of the electron-updater path (updatepath.js),
 * which the shell takes only when the service says in-app updates
 * (internal/selfupdate) are not configured, or does not know them.
 * The packaged app never loads electron-updater (security audit FB1 L-8:
 * its updates carry no signature of the maintainer's own, and a service
 * that answers 404 / 503 would have switched a build to it), whatever
 * resources/app-update.yml says: no update checks, /api/app/update stays
 * "none". Development has no update feed; tests may point
 * LECTUREFLOW_FAKE_UPDATER at a module that stands in for it (ignored and
 * dropped from the environment when packaged, lib/debugflags.js).
 * Returns { updater, note }: note is logged by the caller.
 */
function loadUpdater() {
  if (app.isPackaged) return { updater: null, note: PACKAGED_UPDATER_NOTE };
  const fake = process.env.LECTUREFLOW_FAKE_UPDATER;
  return { updater: fake ? require(path.resolve(fake)) : null, note: null };
}

/** refuseStart: the message box of a refused start, then exit code 2. Returns true. */
function refuseStart(title, body) {
  try {
    dialog.showErrorBox(title, body);
  } catch {
    // no dialog: still refuse
  }
  app.exit(2);
  return true;
}

/**
 * refuseDebugSwitches: the packaged app does not start with a switch it
 * does not use itself (--remote-debugging-port, --proxy-server and every
 * other one, see lib/debugflags.js), nor with ELECTRON_DISABLE_SANDBOX in
 * its environment (Electron then runs without the sandbox, and together
 * with app.enableSandbox() the app crashes silently as its window opens).
 * LECTUREFLOW_E2E_ALLOW_DEBUG=1 lets the switches through, not the latter.
 * lib/debugflags.js startRefusal decides whether and with which box; this
 * only shows it. Returns true when it is quitting.
 * Runs first, before anything else of the app exists (before
 * app.enableSandbox() and any window).
 */
function refuseDebugSwitches() {
  const refusal = startRefusal({
    isPackaged: app.isPackaged,
    argv: process.argv.slice(1),
    env: process.env,
    hasSwitch: (s) => app.commandLine.hasSwitch(s),
    platform: process.platform,
  });
  if (!refusal) return false;
  // app.getLocale() is not available before "ready" on Windows; ICU's default is.
  const { t } = loadTranslator(Intl.DateTimeFormat().resolvedOptions().locale);
  return refuseStart(t(refusal.title), t(refusal.body, refusal.params));
}

/**
 * dropDevEnvironment: the packaged app removes the development variables of
 * its environment (lib/debugflags.js devEnvToDrop) before anything reads
 * it, so they reach neither this process's code nor the service. It also
 * drops PORTABLE_EXECUTABLE_FILE when this is not the portable build (an
 * uninstaller lies next to the exe, lib/updatefiles.js portableExecutable),
 * so that every reader (updates, the extension's launcher) agrees.
 */
function dropDevEnvironment() {
  for (const k of devEnvToDrop({ isPackaged: app.isPackaged, env: process.env })) delete process.env[k];
  if (app.isPackaged && process.env.PORTABLE_EXECUTABLE_FILE !== undefined
    && !portableExecutable({ isPackaged: app.isPackaged, platform: process.platform, execPath: process.execPath, env: process.env })) {
    delete process.env.PORTABLE_EXECUTABLE_FILE;
  }
}

/**
 * retryLock: take the single-instance lock, retrying while another
 * instance quits (a version started by an update, or the restore program).
 * The other instance is told it is a background start, so it never shows
 * its window for these attempts.
 */
async function retryLock(tries = LOCK_RETRIES) {
  for (let i = 0; i < tries; i++) {
    await new Promise((r) => setTimeout(r, LOCK_RETRY_MS));
    if (app.requestSingleInstanceLock({ background: true })) return true;
  }
  return false;
}

/**
 * updateCheckProgram(): verifyPending for selfupdate.js — the service
 * program this copy starts (packaged: resources/bin/lectureflow(.exe), the
 * installed version with its built-in public key) run with
 * -selfupdate-verify on the updates directory (lib/verifyupdate.js); opts
 * { previous, copyTo } check previous.json's package and make the private
 * copy the shell runs.
 */
function updateCheckProgram() {
  const cmd = resolveServerCommand({
    isPackaged: app.isPackaged,
    resourcesPath: process.resourcesPath,
    env: process.env,
    repoRoot: REPO_ROOT,
    platform: process.platform,
    execPath: process.execPath,
  });
  return (updatesDir, version, opts) => runVerify({ ...cmd, env: process.env }, updatesDir, version, opts);
}

/** installationForUpdates: what an in-app update replaces here (selfupdate.js), and how. */
function installationForUpdates() {
  const verifyPending = updateCheckProgram();
  const hook = testInstallFrom(process.env, app.isPackaged);
  if (hook) return { install: hook.install, ops: recordingOps(hook.mark, { verifyPending }) };
  const install = describeInstallation({
    isPackaged: app.isPackaged, platform: process.platform, arch: process.arch, execPath: process.execPath, env: process.env,
  });
  return { install, ops: electronOps({ app, verifyPending }) };
}

/**
 * restoreHelper (--lf-restore): this process is a copy of a freshly
 * installed version of the Windows installation that failed, which that
 * version put into a private temporary folder next to a checked copy of
 * the backup of the previous installation. It waits for the failed version
 * to quit, puts that checked copy into the installation's place and starts
 * it (SelfUpdateApplier.restoreFromBackup). No window.
 */
async function restoreHelper(resolved, log) {
  if (!app.isPackaged || !resolved.dataDir) {
    app.exit(1);
    return;
  }
  log.setFile(path.join(resolved.dataDir, 'logs', 'desktop.log'));
  log.info(`restore program ${app.getVersion()} started`);
  const { install, ops } = installationForUpdates();
  const applier = new SelfUpdateApplier({ install, currentVersion: app.getVersion(), dataDir: resolved.dataDir, ops, log });
  try {
    if (!app.requestSingleInstanceLock({ background: true }) && !(await retryLock(120))) {
      log.error('restore: the failed version did not quit');
      await applier.abandonRestore('the failed version did not quit');
      app.exit(1);
      return;
    }
    await applier.restoreFromBackup({ execPath: process.execPath });
  } catch (err) {
    log.error(`restore: ${err && err.message}`);
  }
  app.exit(0);
}

function main() {
  if (refuseDebugSwitches()) return;
  dropDevEnvironment();
  app.enableSandbox();
  noSpellcheckDownloads(app); // no dictionary download from Google's CDN (websecurity.js)
  const isDev = !app.isPackaged;
  const log = createLogger({ echo: isDev });

  // Chromium's profile goes to {default data root}/electron, or inside the
  // data dir given by LECTUREFLOW_DATA_DIR, so that tests and acceptance
  // runs with their own data dir also get their own single-instance lock.
  const resolved = resolveDataDir({ env: process.env, appData: appDataRoot(), readFile: (p) => fs.readFileSync(p, 'utf8') });
  app.setPath('userData', resolved.profileDir);

  const argv = process.argv.slice(1);
  if (argv.includes(RESTORE_ARG)) {
    restoreHelper(resolved, log);
    return;
  }

  // --background: started by the browser extension's host (lib/nmhost.js);
  // no window until the user or the extension asks for one.
  const background = isBackgroundLaunch(argv);
  if (app.requestSingleInstanceLock({ background })) {
    run({ log, isDev, background });
    return;
  }
  // --updated: started by an update (installer, portable or macOS swap)
  // while the previous version may still be quitting.
  if (!argv.includes(UPDATED_ARG)) {
    app.quit();
    return;
  }
  retryLock().then((ok) => {
    if (ok) run({ log, isDev, background });
    else app.quit();
  });
}

function run({ log, isDev, background }) {
  if (process.platform === 'win32') app.setAppUserModelId(APP_ID);

  let shellCtl = null;
  let stopped = false;

  app.on('second-instance', (event, argv, workingDirectory, additionalData) => {
    // The extension's host starting an app that already runs: stay hidden.
    if (shellCtl && !isBackgroundLaunch(argv, additionalData)) shellCtl.showMainWindow();
  });
  app.on('web-contents-created', (event, contents) => {
    if (shellCtl) shellCtl.hardenContents(contents);
    else contents.on('will-navigate', (e) => e.preventDefault());
  });
  app.on('activate', () => {
    if (shellCtl) shellCtl.showMainWindow();
  });
  app.on('window-all-closed', () => {
    if (process.platform !== 'darwin') app.quit();
  });
  app.on('before-quit', (event) => {
    if (stopped || !shellCtl) return;
    event.preventDefault();
    shellCtl.shutdown().finally(() => {
      stopped = true;
      app.quit(); // when an update installer was started, it waits for this exit
    });
  });
  for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => app.quit());
  process.on('unhandledRejection', (err) => log.error(`unhandled rejection: ${err && err.stack ? err.stack : err}`));

  app.whenReady().then(() => {
    // The app's last language choice, so the splash already speaks it.
    const localeFile = path.join(app.getPath('userData'), LOCALE_FILE);
    const translator = createShellTranslator({ systemLocale: app.getLocale(), uiLocale: readLocaleMemo(localeFile) });
    const setMenu = () => {
      const menu = buildMenuTemplate({ platform: process.platform, isDev, t: translator.t, onOpenSettings: () => shellCtl && shellCtl.openSettings() });
      Menu.setApplicationMenu(menu ? Menu.buildFromTemplate(menu) : null);
    };
    setMenu();
    const { install, ops } = installationForUpdates();
    shellCtl = new Shell({
      translator,
      localeFile,
      onLocaleChange: setMenu,
      log,
      isDev,
      loadUpdater,
      selfUpdateFor: install ? (dataDir) => new SelfUpdateApplier({ install, currentVersion: app.getVersion(), dataDir, ops, log }) : null,
      background,
      installDir: installDirOf(install), // who else can change it (security audit M-4)
      // a copy that cannot update itself: the service offers no download
      // and leaves a pending update to the copy it belongs to
      noSelfUpdate: !!(install && install.unsupported),
    });
    shellCtl.init();
  });
}

main();
