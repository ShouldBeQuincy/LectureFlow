// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The shell controller (SPEC-v3 V1.1): one window, one service process.
//
//   start      splash → resolve data dir → start the service → load the app
//   failure    error page with the reason, "Open logs" and "Try again"
//   exit 3     the service asks to be restarted (data dir moved, data
//              imported): resolve the data dir again, start, reload
//   crash      error page (Try again starts it again)
//   close      ask / background / quit by the service's settings
//   quit       graceful stop of the service (≤ 10 s, then forced)
//   background started by the browser extension's host (--background):
//              no window until the user opens it (tray / Dock) or the
//              extension asks to open a page (lib/nmhost.js)
//   updates    before every start of the service, a pending in-app update
//              is installed (the app quits for it) and a freshly installed
//              version is on probation: healthy = service ready and the
//              window loaded the app; a failure is retried once, then the
//              previous version is put back (selfupdate.js). Which update
//              mechanism checks for new versions is decided once the
//              service is ready (updatepath.js).
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const {
  app, BrowserWindow, dialog, nativeImage, nativeTheme, Notification, powerSaveBlocker, screen, session, shell,
} = require('electron');

const { resolveDataDir } = require('./lib/datadir');
const { buildServerArgs, resolveServerCommand, serviceEnv, EXIT_RESTART } = require('./lib/serverutil');
const {
  decideClose, decideQuit, closeDialogKeys, interpretCloseDialog, wantSleepBlock, runningCount, queuedCount,
} = require('./lib/policy');
const { OPEN_EXTERNAL_LIMIT, createRateLimit } = require('./lib/ratelimit');
const { isServerUrl, isExternalWebUrl } = require('./lib/security');
const { iconPng } = require('./lib/pngicon');
const { shortcutFor, nextZoomLevel } = require('./lib/menu');
const { ServerProcess, StartError, pickFreePort, isPortFree } = require('./server');
const { PORT_FILE, readPortMemo, writePortMemo, makePortPicker } = require('./lib/portmemo');
const { setLocationAside } = require('./lib/location');
const { UpdateController } = require('./updates');
const { UpdatePathChooser } = require('./updatepath');
const { startFailureReason } = require('./selfupdate');
const { startFailureText, isEnvironmentFailure } = require('./lib/startfail');
const { createApiClient } = require('./apiclient');
const { SleepGuard } = require('./sleepguard');
const { TrayController } = require('./tray');
const { registerIpc } = require('./ipc');
const { createPathGranter } = require('./lib/pathgrant');
const { writeLocaleMemo } = require('./lib/i18n');
const { hardenSession, hardenWebContents } = require('./websecurity');
const { launcherFor, launcherBlocked, isPortable } = require('./lib/nmhost');
const { shellLogFile, mkdirLeaf } = require('./logger');

const SHELL_ROOT = path.resolve(__dirname, '..');
const REPO_ROOT = path.resolve(__dirname, '..', '..', '..');
const SPLASH = path.join(SHELL_ROOT, 'splash.html');
const PRELOAD = path.join(SHELL_ROOT, 'preload.js');
// Persistent, so the page keeps its preferences (localStorage) across
// launches; it lives in userData ({default data root}/electron). The HTTP
// cache is off (everything is local anyway). The token never reaches this
// storage: it is only added to outgoing request headers.
const PARTITION = 'persist:lectureflow';
const POLL_MS = 5000;
// Closing the window or Quit in the tray while the service does not answer
// (review R69 L-7): poll()'s last reading of the activity stands in when it
// is at most this old; otherwise the activity is unknown.
const ACTIVITY_FALLBACK_MS = 30000;
// What the close and quit decisions and the close dialog assume while the
// activity is unknown: a job and a fetch of a recording may both run. So
// the shell asks (or follows a remembered choice) instead of quitting at
// once, and the dialog's detail is close.detail_both, which also says that
// an unfinished fetch loses its sign-in data (review R84 L-3).
const ACTIVITY_UNKNOWN = Object.freeze({ running: 2, queued: 0, downloads: 1 });
const STOP_TIMEOUT_MS = 10000;
const RESTART_WINDOW_MS = 60000;
const MAX_RESTARTS = 3;
// A freshly installed version counts as healthy this long after the window
// loaded the app (the service must still be running).
const PROBATION_GRACE_MS = 3000;
const BACKGROUND = { light: '#eef0f4', dark: '#0e1013' }; // --bg-app

/** appDataRoot: the platform's appData; tests of the unpackaged app may move it. */
function appDataRoot() {
  if (!app.isPackaged && process.env.LECTUREFLOW_TEST_APPDATA) return path.resolve(process.env.LECTUREFLOW_TEST_APPDATA);
  return app.getPath('appData');
}

function isDir(p) {
  try {
    return fs.statSync(p).isDirectory();
  } catch {
    return false;
  }
}

/**
 * closeFactsNote: the log's account of what a close or quit decision went by
 * (Shell._closeFacts): the counts, how old a stand-in reading was, or that
 * the activity was unknown, so "no tasks" and "no answer" read differently.
 */
function closeFactsNote(facts) {
  if (facts.unknown) return '(activity unknown)';
  const note = `(running ${runningCount(facts.activity)}) (queued ${queuedCount(facts.activity)})`;
  return facts.ageS === undefined ? note : `${note} (activity from ${facts.ageS} s ago)`;
}

class Shell {
  /**
   * translator: createShellTranslator(); its t is handed to the tray and IPC
   * and follows the app's language choice (poll), which is remembered in
   * localeFile; onLocaleChange() rebuilds the application menu.
   * loadUpdater() → { updater, note }: electron-updater (or a test stand-in),
   * loaded only when the service says in-app updates are not configured.
   * selfUpdateFor(dataDir) → a SelfUpdateApplier, or null when this copy is
   * never updated in place (development).
   * installDir: the installation directory the service checks (-install-dir;
   * lib/updatefiles.installDirOf), null when there is none.
   * noSelfUpdate: this copy of the app cannot replace itself
   * (describeInstallation: unsupported); the service is told
   * (-no-self-update, lib/serverutil.js buildServerArgs).
   */
  constructor({ translator, localeFile = null, onLocaleChange = () => {}, log, isDev, loadUpdater = null, selfUpdateFor = null, background = false, installDir = null, noSelfUpdate = false }) {
    this.translator = translator;
    this.t = translator.t;
    this.localeFile = localeFile;
    this.onLocaleChange = onLocaleChange;
    this.log = log;
    this.isDev = isDev;
    this.platform = process.platform;

    this.win = null;
    this.server = null;
    this.phase = 'idle'; // idle | starting | ready | failed | stopping
    this.generation = 0;
    this.dataDir = null;
    this.quitting = false;
    this.closing = false; // a close or quit question is being asked
    this.trayQuestion = null; // AbortController of the tray's quit question while it is open
    this.restarts = [];
    this.pollTimer = null;
    this.polling = false;
    this.lastSettings = null;
    // { activity, at, gen }: poll()'s last answer of /api/app/activity, when
    // (this.now) and from which start of the service (generation).
    this.lastActivity = null;
    this.now = () => performance.now(); // monotonic milliseconds
    this.backgroundNotice = null;
    this.shutdownPromise = null;
    this.lastError = null; // kind of the error page on screen ('location_invalid' offers "use the default location")
    this.locationFile = null;
    this.installingUpdate = false;
    // In-app update: the applier of the current data dir; probation while a
    // freshly installed version starts (see the file header).
    this.selfUpdateFor = selfUpdateFor;
    this.installDir = installDir;
    this.noSelfUpdate = !!noSelfUpdate;
    this.applier = null;
    this.probation = false;
    this.probationFailing = false;
    this.probationTimer = null;
    // Started with --background: the window stays hidden until shown.
    this.startHidden = background;
    // A route the browser extension asked for ("open") before the app page
    // could show it; loadApp goes there.
    this.pendingRoute = null;
    // One limit for every link handed to the system browser: the page's
    // openExternal (ipc.js) and navigation / window.open (openExternal
    // below). Security audit FB1 L-6, review R44.
    this.openExternalLimit = createRateLimit(OPEN_EXTERNAL_LIMIT);

    this.api = createApiClient({ getServer: () => this._serverForApi() });
    this.updatePath = loadUpdater ? new UpdatePathChooser({
      api: this.api,
      loadUpdater,
      createController: (updater) => new UpdateController({ updater, api: this.api, log }),
      log,
    }) : null;
    this.sleepGuard = new SleepGuard({ blocker: powerSaveBlocker, log });
    this.tray = new TrayController({
      t: this.t,
      icon: this._trayIcon(),
      onOpen: () => this.showMainWindow(),
      // Asks first while tasks run or wait (T26 S1); not inside the tray's own click handler.
      onQuit: () => setImmediate(() => this.onQuitRequested()),
    });
  }

  // ---- setup ----

  init() {
    this.ses = session.fromPartition(PARTITION, { cache: false });
    hardenSession(this.ses, {
      getPort: () => this.port(),
      getToken: () => this.token(),
      shellRoot: SHELL_ROOT,
      devTools: this.isDev,
      openExternal: (url) => this.openExternal(url),
      log: this.log,
    });
    registerIpc({
      getPort: () => this.port(),
      shellRoot: SHELL_ROOT,
      t: this.t,
      showMainWindow: () => this.showMainWindow(),
      onRetry: () => this.retry(),
      onOpenLogs: () => this.openLogs(),
      onRestoreDefault: () => this.restoreDefaultLocation(),
      openExternalLimit: this.openExternalLimit,
      grantPaths: createPathGranter({ api: this.api, log: this.log }),
      log: this.log,
    });
    this.createWindow();
    // Without a window, the tray icon is the way in (macOS: the Dock).
    if (this.startHidden && this.platform !== 'darwin') this.tray.show();
    this.startServer('starting');
  }

  /** hardenContents is called for every webContents (app 'web-contents-created'). */
  hardenContents(contents) {
    contents.on('before-input-event', (event, input) => {
      const action = shortcutFor(input, { platform: this.platform, isDev: this.isDev });
      if (!action) return;
      event.preventDefault();
      if (action === 'reload') contents.reload();
      else if (action === 'devTools') contents.toggleDevTools();
      else contents.setZoomLevel(nextZoomLevel(contents.getZoomLevel(), action));
    });
    hardenWebContents(contents, {
      getPort: () => this.port(),
      openExternal: (url) => this.openExternal(url),
      childWindowOptions: () => ({
        autoHideMenuBar: true,
        backgroundColor: this._background(),
        icon: this._windowIcon(),
        webPreferences: this.webPreferences(),
      }),
      log: this.log,
    });
  }

  webPreferences() {
    return {
      preload: PRELOAD,
      session: this.ses,
      contextIsolation: true,
      sandbox: true,
      nodeIntegration: false,
      nodeIntegrationInWorker: false,
      nodeIntegrationInSubFrames: false,
      webSecurity: true,
      allowRunningInsecureContent: false,
      webviewTag: false,
      experimentalFeatures: false,
      navigateOnDragDrop: false,
      spellcheck: false,
      safeDialogs: true,
      devTools: this.isDev,
    };
  }

  /** updates: the electron-updater controller, once that path was chosen. */
  get updates() {
    return this.updatePath ? this.updatePath.controller : null;
  }

  port() {
    return this.server && this.server.ready ? this.server.port : null;
  }

  token() {
    return this.server && this.server.ready ? this.server.token : null;
  }

  _serverForApi() {
    if (this.phase !== 'ready' || !this.server || !this.server.ready) return null;
    return { origin: this.server.origin, token: this.server.token };
  }

  _background() {
    return nativeTheme.shouldUseDarkColors ? BACKGROUND.dark : BACKGROUND.light;
  }

  _windowIcon() {
    if (!this.windowIcon) this.windowIcon = nativeImage.createFromBuffer(iconPng(256));
    return this.windowIcon;
  }

  _trayIcon() {
    const img = nativeImage.createFromBuffer(iconPng(16), { scaleFactor: 1 });
    img.addRepresentation({ scaleFactor: 2, buffer: iconPng(32) });
    return img;
  }

  createWindow() {
    const area = screen.getPrimaryDisplay().workAreaSize;
    const win = new BrowserWindow({
      width: Math.min(1360, area.width),
      height: Math.min(880, area.height),
      minWidth: 880,
      minHeight: 560,
      show: false,
      title: 'LectureFlow',
      backgroundColor: this._background(),
      icon: this._windowIcon(),
      autoHideMenuBar: false,
      webPreferences: this.webPreferences(),
    });
    win.once('ready-to-show', () => {
      if (!this.quitting && !this.startHidden) win.show();
    });
    win.on('close', (event) => {
      if (this.quitting) return;
      event.preventDefault();
      this.onCloseRequested();
    });
    const wc = win.webContents;
    wc.on('did-fail-load', (event, code, description, url, isMainFrame) => {
      if (!isMainFrame || code === -3 /* ERR_ABORTED: replaced by another load */) return;
      if (!isServerUrl(url, this.port())) return;
      if (this.probation) {
        this._probationFailed(`the window did not load the app: ${description} (${code})`);
        return;
      }
      this.showError({ title: 'error.page.title', body: this.t('error.page.body'), detail: `${description} (${code})` });
    });
    wc.on('did-finish-load', () => {
      if (this.probation && this.phase === 'ready' && isServerUrl(wc.getURL(), this.port())) this._probationLoaded();
      this._reportVisibility(win); // a page loaded while the window is hidden (background start)
    });
    // perf-fix: the service samples the GPU only for a window someone can
    // see (web/api.js, internal/api/visibility.go); hidden to the tray or
    // minimized counts as not seen, whatever the page's own visibility says.
    for (const name of ['show', 'hide', 'minimize', 'restore']) win.on(name, () => this._reportVisibility(win));
    wc.on('render-process-gone', (event, details) => {
      if (this.quitting || details.reason === 'clean-exit') return;
      this.log.error(`renderer gone: ${details.reason} (${details.exitCode})`);
      if (this.probation) {
        this._probationFailed(`the window's page process ended (${details.reason})`);
        return;
      }
      this.showError({ title: 'error.page.title', body: this.t('error.page.body'), detail: this.t('reason.renderer', { reason: details.reason }) });
    });
    this.win = win;
    return win;
  }

  _ensureWindow() {
    if (!this.win || this.win.isDestroyed()) this.createWindow();
    return this.win;
  }

  /**
   * _reportVisibility tells the app page whether its window is shown (not
   * hidden, not minimized): a "lectureflow:window-visibility" event with
   * {detail: {visible}}, which web/api.js reports to the service. Nothing
   * for the splash or error page.
   */
  _reportVisibility(win) {
    if (!win || win.isDestroyed()) return;
    const wc = win.webContents;
    if (!isServerUrl(wc.getURL(), this.port())) return;
    const visible = win.isVisible() && !win.isMinimized();
    wc.executeJavaScript(`window.dispatchEvent(new CustomEvent('lectureflow:window-visibility', { detail: { visible: ${visible} } }))`).catch(() => {});
  }

  // ---- pages ----

  _loadShellPage(query) {
    const win = this._ensureWindow();
    win.loadFile(SPLASH, { query }).catch(() => {
      // ERR_ABORTED when the app page replaces it; real failures show in did-fail-load
    });
  }

  showSplash(state) {
    this.splashState = state;
    this._loadShellPage({ state, lang: this.translator.locale, status: this.t(`splash.${state}`) });
  }

  /** showError({ title: key, body: text, detail: text, kind }) */
  showError({ title, body, detail = '', kind = null }) {
    this.log.error(`${title}: ${detail || body}`);
    this.lastError = kind;
    const extra = kind === 'location_invalid' ? { restore: this.t('action.restore_default') } : {};
    this._loadShellPage({
      ...extra,
      state: 'error',
      lang: this.translator.locale,
      title: this.t(title),
      body,
      detail: String(detail).slice(0, 2000),
      detailLabel: this.t('error.details'),
      retry: this.t('action.retry'),
      logs: this.t('action.open_logs'),
    });
  }

  loadApp() {
    const win = this._ensureWindow();
    const route = this.pendingRoute || '';
    this.pendingRoute = null;
    win.loadURL(`${this.server.origin}/${route}`).catch(() => {
      // reported by did-fail-load
    });
  }

  /**
   * openRoute: the browser extension's "在 LectureFlow 中打开" (a
   * "LECTUREFLOW_OPEN #/lecture/ID" line of the service, validated by
   * lib/nmhost.parseOpenLine): show and focus the window on that route.
   */
  openRoute(route) {
    if (this.quitting) return;
    this.log.info(`open requested by the browser extension: ${route}`);
    const win = this.win;
    const onApp = this.phase === 'ready' && win && !win.isDestroyed() && isServerUrl(win.webContents.getURL(), this.port());
    if (!onApp) {
      this.pendingRoute = route; // the next loadApp goes there
      this.showMainWindow();
      return;
    }
    this.showMainWindow();
    win.webContents.executeJavaScript(`location.hash = ${JSON.stringify(route)}`).catch(() => {});
  }

  /** openSettings: "Settings…" (Cmd+,) of the macOS application menu. */
  openSettings() {
    this.showMainWindow();
    const win = this.win;
    if (this.phase !== 'ready' || !win || win.isDestroyed() || !isServerUrl(win.webContents.getURL(), this.port())) return;
    win.webContents.executeJavaScript("location.hash = '#/settings'").catch(() => {});
  }

  // ---- service lifecycle ----

  async startServer(mode) {
    const gen = ++this.generation;
    this.phase = 'starting';
    this.stopPolling();
    this.sleepGuard.release();
    this.showSplash(mode);

    const previous = this.server;
    if (previous && previous.alive) await previous.stop({ timeoutMs: STOP_TIMEOUT_MS }).catch(() => {});
    if (previous) this.log.removeSecret(previous.token);
    if (gen !== this.generation) return;

    // reason: English, for result.json when a freshly installed version fails.
    const fail = (opts, reason = null) => {
      if (gen !== this.generation || this.quitting) return;
      if (this.probation && reason) {
        this._probationFailed(reason);
        return;
      }
      this.phase = 'failed';
      this.showError(opts);
      // A start in the background must not fail unseen.
      if (this.startHidden) this.showMainWindow();
    };

    // The data dir is resolved again on every start: after exit code 3 the
    // service may have written a new location.json.
    const resolved = resolveDataDir({ env: process.env, appData: appDataRoot(), readFile: (p) => fs.readFileSync(p, 'utf8') });
    this.locationFile = resolved.locationFile;
    if (resolved.error) {
      this.log.setFile(path.join(resolved.profileDir, 'logs', 'desktop.log'));
      return fail({
        kind: 'location_invalid',
        title: 'error.location.title',
        body: this.t('error.location.body', { path: resolved.locationFile, default: resolved.defaultRoot }),
        detail: resolved.error.reason,
      });
    }
    const { dataDir } = resolved;
    if (resolved.source === 'location' && !isDir(dataDir)) {
      this.log.setFile(path.join(resolved.profileDir, 'logs', 'desktop.log'));
      return fail({ title: 'error.data_dir.title', body: this.t('error.data_dir.body', { path: dataDir }) });
    }
    // The shell never creates the data dir (sec-update M2, datadir-ui): the
    // service does, private from the start (internal/dirperm); one made here
    // would get the default permissions of where it lies. Until it exists
    // the shell logs into its profile (logger.js shellLogFile).
    const dataDirMissing = !isDir(dataDir);
    this.dataDir = dataDir;
    this.log.setFile(shellLogFile({ dataDir, profileDir: resolved.profileDir, isDir }));
    this.log.info(`data dir ${dataDir} (${resolved.source}${dataDirMissing ? ', not created yet: the service creates it' : ''}); app ${app.getVersion()}`);

    // The service is not running here (nor any task in it): the only moment
    // an update is installed.
    if ((await this._selfUpdateAtStart(dataDir, gen)) === 'stop') return;

    const cmd = resolveServerCommand({
      isPackaged: app.isPackaged,
      resourcesPath: process.resourcesPath,
      env: process.env,
      repoRoot: REPO_ROOT,
      platform: this.platform,
      execPath: process.execPath,
    });
    const program = cmd.prefixArgs[0] || cmd.command;
    if (!fs.existsSync(program)) {
      return fail({ title: 'error.start.title', body: this.t('error.start.body'), detail: this.t('reason.not_found', { path: program }) }, `the service program is missing: ${program}`);
    }

    const portFile = path.join(app.getPath('userData'), PORT_FILE);
    const server = new ServerProcess({
      pickPort: makePortPicker({ preferred: readPortMemo(portFile), isFree: isPortFree, pickFree: pickFreePort }),
      command: cmd.command,
      prefixArgs: cmd.prefixArgs,
      argsFor: (port) => buildServerArgs({
        port,
        dataDir,
        resourcesDir: cmd.resourcesDir,
        appVersion: app.getVersion(),
        configFile: path.join(dataDir, 'config.toml'),
        locationFile: resolved.source === 'env' ? null : resolved.locationFile,
        launcher: launcherFor({ isPackaged: app.isPackaged, platform: this.platform, execPath: process.execPath, env: process.env }),
        launcherBlocked: launcherBlocked({ isPackaged: app.isPackaged, platform: this.platform, execPath: process.execPath }),
        portable: isPortable({ isPackaged: app.isPackaged, platform: this.platform, env: process.env }),
        installDir: this.installDir,
        noSelfUpdate: this.noSelfUpdate,
      }),
      env: serviceEnv(process.env, cmd.extraEnv), // no usage reporting (serverutil NO_TELEMETRY_ENV)
      // (a data dir the service has yet to create cannot be the working directory)
      cwd: [cmd.resourcesDir, dataDir].find(isDir) || path.dirname(program),
      log: this.log,
    });
    this.server = server;
    server.on('exit', (info) => this.onServerExit(server, info));
    server.on('open', (route) => {
      if (server === this.server) this.openRoute(route);
    });
    // a long step before the ready line (the backup and migrations of an
    // upgrade, test-upgrade F3): the splash says the data is being upgraded
    server.on('starting', () => {
      if (server === this.server && gen === this.generation && this.phase === 'starting' && this.splashState !== 'upgrading') this.showSplash('upgrading');
    });

    try {
      await server.start();
    } catch (err) {
      if (gen !== this.generation || this.quitting) return;
      if (err instanceof StartError && err.kind === 'restart_requested') {
        this.restartRequested();
        return;
      }
      const page = this._startErrorPage(err, program);
      // the service could not create the data dir (the shell leaves that to it)
      if (dataDirMissing && !isDir(dataDir)) page.detail = [this.t('reason.unwritable', { path: dataDir }), page.detail].filter(Boolean).join('\n');
      // another LectureFlow on the data folder, a locked, read-only or
      // unreachable database, a full or failing disk: not this version's
      // fault. No probation failure (no rollback), and the start does not
      // count as a try (else "Try again" would use up the tries and roll
      // back all the same, SEC3 T6); the error page and "Try again". At
      // most MAX_REFUNDS times per installation (a defect of the new
      // version can look the same, SEC12 M-1): then refundTry says no and
      // the failure counts like any other.
      const environment = err instanceof StartError && isEnvironmentFailure(err.failure);
      let refunded = false;
      if (environment && this.probation && !this.probationFailing) {
        refunded = await this.applier.refundTry().catch((e) => {
          this.log.warn(`selfupdate: ${e && e.message}`);
          return false;
        });
        if (refunded) this._endProbation();
      }
      // (outside a probation fail() ignores the reason)
      fail(page, refunded ? null : startFailureReason(err));
      return;
    }
    if (gen !== this.generation || this.quitting) {
      server.stop({ timeoutMs: STOP_TIMEOUT_MS }).catch(() => {});
      return;
    }
    // the service has created the data dir: the shell's log moves next to the service's
    if (dataDirMissing) {
      this.log.info(`the service created the data dir; the log continues in ${path.join(dataDir, 'logs')}`);
      this.log.setFile(shellLogFile({ dataDir, profileDir: resolved.profileDir, isDir }));
      this.log.info(`data dir ${dataDir} (${resolved.source}, created by the service); app ${app.getVersion()}`);
    }
    this.log.addSecret(server.token);
    if (readPortMemo(portFile) !== server.port) writePortMemo(portFile, server.port);
    this.phase = 'ready';
    this.lastError = null;
    this.loadApp();
    // The first poll decides the update mechanism (updatepath.js) and starts
    // electron-updater's checks when that is the one; after a restart
    // sync() reports again.
    this.startPolling();
  }

  // ---- in-app update: install at start, probation of a new version ----

  /**
   * _selfUpdateAtStart: install a pending update (→ 'stop': the app quits
   * for it) or begin the probation of a freshly installed version.
   */
  async _selfUpdateAtStart(dataDir, gen) {
    this._endProbation();
    if (!this.selfUpdateFor) return 'go';
    if (!this.applier || this.applier.dataDir !== dataDir) this.applier = this.selfUpdateFor(dataDir);
    const r = await this.applier.atStart({ background: this.startHidden });
    if (gen !== this.generation || this.quitting) return 'stop';
    if (r.action === 'exit') {
      this._quitForUpdate();
      return 'stop';
    }
    this.probation = r.action === 'probation';
    return 'go';
  }

  _endProbation() {
    if (this.probationTimer) clearTimeout(this.probationTimer);
    this.probationTimer = null;
    this.probation = false;
  }

  /** _probationLoaded: the window loaded the app; healthy if the service still runs a moment later. */
  _probationLoaded() {
    if (this.probationTimer) return;
    this.probationTimer = setTimeout(() => {
      this.probationTimer = null;
      if (!this.probation || this.probationFailing || this.quitting) return;
      if (this.phase !== 'ready' || !this.server || !this.server.alive) return;
      this.probation = false;
      this.applier.healthy().catch((err) => this.log.warn(`selfupdate: ${err.message}`));
    }, PROBATION_GRACE_MS);
  }

  /**
   * _probationFailed(reason): the freshly installed version did not start.
   * The service is stopped first (the database may be put back); then the
   * applier retries once, or starts the previous version (the app quits),
   * or gives up (error page).
   */
  async _probationFailed(reason) {
    if (!this.probation || this.probationFailing || this.quitting) return;
    this.probationFailing = true;
    if (this.probationTimer) clearTimeout(this.probationTimer);
    this.probationTimer = null;
    this.generation++; // ends a start in progress
    this.phase = 'failed';
    this.stopPolling();
    this.sleepGuard.release();
    const server = this.server;
    if (server && server.alive) await server.stop({ timeoutMs: STOP_TIMEOUT_MS }).catch(() => {});
    let r;
    try {
      r = await this.applier.failed(reason);
    } catch (err) {
      r = { action: 'giveup', error: String(err && err.message) };
    }
    this.probation = false;
    this.probationFailing = false;
    if (this.quitting) return;
    if (r.action === 'retry') {
      this.startServer('restarting');
      return;
    }
    if (r.action === 'exit') {
      this._quitForUpdate();
      return;
    }
    this.showError({ title: 'error.start.title', body: this.t('error.start.body'), detail: r.error || reason });
    if (this.startHidden) this.showMainWindow();
  }

  /** _quitForUpdate: an installer or the other version takes over. */
  _quitForUpdate() {
    this.log.info('quitting for the application update');
    this.showSplash('restarting');
    setImmediate(() => app.quit());
  }

  /**
   * restoreDefaultLocation: the "use the default location" button of the
   * location.json error page. After confirmation, location.json is renamed
   * (never deleted) and the service starts with the default data dir.
   */
  async restoreDefaultLocation() {
    if (this.lastError !== 'location_invalid' || !this.locationFile || this.quitting) return;
    const defaultRoot = path.dirname(this.locationFile);
    const backup = `${path.basename(this.locationFile)}.bak`;
    const r = await dialog.showMessageBox(this.win, {
      type: 'question',
      title: 'LectureFlow',
      message: this.t('restore.message'),
      detail: this.t('restore.detail', { path: defaultRoot, backup }),
      buttons: [this.t('restore.confirm'), this.t('close.cancel')],
      defaultId: 0,
      cancelId: 1,
      noLink: true,
    });
    if (r.response !== 0 || this.lastError !== 'location_invalid') return;
    try {
      const moved = setLocationAside(this.locationFile);
      this.log.info(`location.json set aside as ${path.basename(moved)}; using the default data dir`);
    } catch (err) {
      if (err && err.code !== 'ENOENT') {
        this.showError({ kind: 'location_invalid', title: 'error.location.title', body: this.t('error.location.body', { path: this.locationFile, default: defaultRoot }), detail: `${err.code || err.message}` });
        return;
      }
    }
    this.restarts = [];
    this.startServer('starting');
  }

  _startErrorPage(err, program) {
    const page = { title: 'error.start.title', body: this.t('error.start.body') };
    if (!(err instanceof StartError)) return { ...page, detail: String(err && err.message) };
    // a failure the service named (lib/startfail.js): what to do, in the
    // interface language; its ERROR line (Chinese and English) as details
    const text = startFailureText(err.failure, this.t, this.translator.locale);
    if (text) return { ...page, body: text, detail: err.reason || '' };
    switch (err.kind) {
      case 'spawn_failed':
        return { ...page, detail: this.t('reason.spawn_failed', { code: err.reason, path: program }) };
      case 'timeout':
        return { ...page, detail: this.t('reason.timeout') };
      case 'port_in_use':
        return { ...page, detail: [this.t('reason.port'), err.reason].filter(Boolean).join('\n') };
      case 'token_rejected':
        return { ...page, detail: this.t('reason.token_rejected') };
      case 'bad_mode':
        return { ...page, detail: this.t('reason.bad_mode') };
      default:
        return { ...page, detail: err.reason || this.t('reason.exit_code', { code: err.code }) };
    }
  }

  onServerExit(server, info) {
    if (server !== this.server || this.quitting || info.expected) return;
    if (this.phase !== 'ready') return; // still starting: start() reports it
    if (this.probation && info.code !== EXIT_RESTART) {
      this._probationFailed(`the service stopped unexpectedly (${info.reason || `exit code ${info.code === null ? info.signal : info.code}`})`);
      return;
    }
    this.phase = 'failed';
    this.stopPolling();
    this.sleepGuard.release();
    this.tray.setRunning(0);
    if (info.code === EXIT_RESTART) {
      this.log.info('service asked for a restart');
      this.restartRequested();
      return;
    }
    this.showError({
      title: 'error.crash.title',
      body: this.t('error.crash.body'),
      detail: info.reason || this.t('reason.exit_code', { code: info.code === null ? info.signal : info.code }),
    });
    this.showMainWindow();
  }

  restartRequested() {
    const now = Date.now();
    this.restarts = this.restarts.filter((t) => now - t < RESTART_WINDOW_MS);
    this.restarts.push(now);
    if (this.restarts.length > MAX_RESTARTS) {
      this.phase = 'failed';
      this.showError({ title: 'error.start.title', body: this.t('error.start.body'), detail: this.t('reason.restart_loop') });
      return;
    }
    this.startServer('restarting');
  }

  /** retry: the error page's button. */
  retry() {
    if (this.quitting) return;
    if (this.phase === 'ready' && this.server && this.server.alive) {
      this.loadApp(); // only the page failed
      return;
    }
    if (this.phase === 'starting') return;
    this.restarts = [];
    this.startServer('starting');
  }

  openLogs() {
    // the folder of the shell's log: the data dir's logs (with the
    // service's), or the profile's while the data dir does not exist —
    // never creating the data dir (sec-update M2: the service does)
    const dir = this.log.file ? path.dirname(this.log.file) : path.join(app.getPath('userData'), 'logs');
    try {
      mkdirLeaf(dir);
    } catch {
      // openPath reports it
    }
    shell.openPath(dir).then((err) => {
      if (err) this.log.warn(`open logs: ${err}`);
    });
  }

  /**
   * openExternal: a link from navigation, window.open or the openExternal
   * permission (websecurity.js) to the system browser. No user gesture can
   * be checked here; the shared limit applies, and a link over it is
   * dropped with a log line, quietly.
   */
  openExternal(url) {
    if (!isExternalWebUrl(url)) return;
    if (!this.openExternalLimit()) {
      this.log.warn('open external link: too many links in a short time; dropped');
      return;
    }
    shell.openExternal(url).catch((err) => this.log.warn(`open external link: ${err.message}`));
  }

  // ---- activity polling: sleep blocker, tray tooltip ----

  startPolling() {
    this.stopPolling();
    this.pollTimer = setInterval(() => this.poll(), POLL_MS);
    this.poll();
  }

  /**
   * followLocale: the app's language choice ("" = none: the system's). The
   * shell's own strings switch at once (the tray menu is built when it is
   * shown); the choice is remembered for the next splash.
   */
  followLocale(uiLocale) {
    if (!this.translator.use(uiLocale)) return;
    this.log.info(`shell language: ${this.translator.locale}`);
    if (this.localeFile) writeLocaleMemo(this.localeFile, uiLocale);
    this.tray.refresh();
    this.onLocaleChange();
  }

  stopPolling() {
    if (this.pollTimer) clearInterval(this.pollTimer);
    this.pollTimer = null;
  }

  async poll() {
    if (this.polling || this.phase !== 'ready') return;
    this.polling = true;
    try {
      if (this.updatePath && !this.updatePath.decided) await this.updatePath.decide().catch(() => {});
      if (this.updates) await this.updates.sync().catch(() => {});
      const gen = this.generation;
      const activity = await this.api.activity().then((a) => {
        if (gen === this.generation) this.lastActivity = { activity: a, at: this.now(), gen };
        return a;
      }, () => null);
      let settings = this.lastSettings;
      if (runningCount(activity) > 0) settings = await this.api.settings().catch(() => this.lastSettings);
      if (settings) this.lastSettings = settings;
      const ui = await this.api.uiSettings().catch(() => null);
      if (ui) this.followLocale(ui.locale);
      if (this.phase !== 'ready') return;
      this.sleepGuard.update(wantSleepBlock(activity, settings));
      this.tray.setRunning(runningCount(activity));
    } finally {
      this.polling = false;
    }
  }

  // ---- closing the window, Quit in the tray ----

  async onCloseRequested() {
    if (this.closing) return;
    this.closing = true;
    try {
      const facts = await this._closeFacts();
      const activity = facts.unknown ? ACTIVITY_UNKNOWN : facts.activity;
      const decision = decideClose({ platform: this.platform, activity, settings: facts.settings });
      this.log.info(`close requested: ${decision} ${closeFactsNote(facts)}`);
      if (decision === 'ask') {
        const action = await this._askClose(activity);
        if (action === 'background') this.hideToBackground();
        else if (action === 'quit') app.quit();
        return;
      }
      if (decision === 'background') this.hideToBackground();
      else if (decision === 'hide') this.win.hide();
      else app.quit();
    } finally {
      this.closing = false;
    }
  }

  /**
   * onQuitRequested: "Quit" in the tray menu (T26 S1). With tasks it asks
   * the close question first (policy.decideQuit), without "remember my
   * choice"; "continue in background" and cancel leave the app as it is,
   * hidden in the tray. The question has
   * no parent window, so it is not modal: when the window is shown
   * meanwhile, showMainWindow withdraws it as cancelled (review R69 L-1),
   * and closing the window asks again on the window.
   */
  async onQuitRequested() {
    if (this.closing || this.quitting) return;
    this.closing = true;
    const question = new AbortController();
    this.trayQuestion = question;
    const withdrawn = () => {
      if (question.signal.aborted) this.log.info('quit question withdrawn: the window was shown');
      return question.signal.aborted;
    };
    try {
      const facts = await this._closeFacts();
      if (withdrawn()) return;
      const activity = facts.unknown ? ACTIVITY_UNKNOWN : facts.activity;
      const decision = decideQuit({ activity, settings: facts.settings });
      this.log.info(`quit requested: ${decision} ${closeFactsNote(facts)}`);
      if (decision === 'ask') {
        const action = await this._askClose(activity, { signal: question.signal, offerRemember: false });
        if (withdrawn() || action !== 'quit') return;
      }
      app.quit();
    } finally {
      if (this.trayQuestion === question) this.trayQuestion = null;
      this.closing = false;
    }
  }

  /**
   * _closeFacts: { activity, settings, ageS?, unknown? } for the close and
   * quit decisions. Before the service is ready both are null (nothing runs).
   * Settings that do not answer are null. When the activity does not answer
   * within 2 s (review R69 L-7), poll()'s last reading stands in if it is
   * from this start of the service and at most ACTIVITY_FALLBACK_MS old
   * (ageS: its age in seconds); otherwise the activity is unknown (unknown:
   * true, activity null) and the caller assumes tasks. In both cases
   * settings that do not answer either fall back on the last ones poll()
   * read, so a remembered "quit" still quits.
   */
  async _closeFacts() {
    if (this.phase !== 'ready') return { activity: null, settings: null };
    const gen = this.generation;
    const [read, settings] = await Promise.all([
      this.api.activity({ timeoutMs: 2000 }).then((activity) => ({ activity }), () => null),
      this.api.settings({ timeoutMs: 2000 }).catch(() => null),
    ]);
    if (read) return { activity: read.activity, settings };
    const known = settings || this.lastSettings;
    const last = this.lastActivity;
    const age = last && last.gen === gen ? this.now() - last.at : NaN;
    if (age >= 0 && age <= ACTIVITY_FALLBACK_MS) return { activity: last.activity, settings: known, ageS: Math.round(age / 1000) };
    return { activity: null, settings: known, unknown: true };
  }

  /**
   * _askClose: the close dialog for these tasks (policy.closeDialogKeys);
   * a remembered answer is saved. Returns 'background' | 'quit' | 'cancel'.
   * Attached to the window while it is shown; from the tray the window is
   * hidden, and a dialog on a hidden window could stay out of sight.
   * signal (an AbortSignal) closes the dialog as cancelled; Electron honours
   * it on Windows and Linux (on macOS only with a parent window, but there
   * is no tray there). offerRemember: the "remember my choice" box, which
   * sets close_behavior — what closing the window does. The tray's quit
   * question leaves it out (review R69 L-2): there a remembered
   * "background" would not apply, and "quit" would silently stop closing
   * the window from asking.
   */
  async _askClose(activity, { signal = null, offerRemember = true } = {}) {
    const text = closeDialogKeys(activity);
    const options = {
      type: 'question',
      title: 'LectureFlow',
      message: this.t(text.message),
      detail: this.t(text.detail),
      buttons: [this.t('close.background'), this.t('close.quit'), this.t('close.cancel')],
      defaultId: 0,
      cancelId: 2,
      noLink: true,
    };
    if (offerRemember) {
      options.checkboxLabel = this.t('close.remember');
      options.checkboxChecked = false;
    }
    if (signal) options.signal = signal;
    const win = this.win && !this.win.isDestroyed() && this.win.isVisible() ? this.win : null;
    const result = await (win ? dialog.showMessageBox(win, options) : dialog.showMessageBox(options));
    const { action, remember } = interpretCloseDialog(result);
    if (remember && offerRemember) {
      await this.api.patchSettings({ close_behavior: remember }).catch((err) => this.log.warn(`save close behaviour: ${err.message}`));
    }
    return action;
  }

  hideToBackground() {
    if (!this.win || this.win.isDestroyed()) return;
    this.win.hide();
    if (this.platform !== 'darwin') this.tray.show();
    if (!this.backgroundNotice && Notification.isSupported()) {
      this.backgroundNotice = new Notification({
        title: this.t('background.title'),
        body: this.t(this.platform === 'darwin' ? 'background.body.darwin' : 'background.body.win32'),
      });
      this.backgroundNotice.on('click', () => this.showMainWindow());
      this.backgroundNotice.show();
    }
  }

  showMainWindow() {
    if (this.quitting) return;
    // The tray's quit question is not modal; with the window back it is
    // withdrawn (review R69 L-1), so closing the window can ask again.
    if (this.trayQuestion) this.trayQuestion.abort();
    this.startHidden = false;
    if (!this.win || this.win.isDestroyed()) {
      this.createWindow();
      if (this.phase === 'ready') this.loadApp();
      else if (this.phase === 'starting') this.showSplash(this.splashState === 'upgrading' ? 'upgrading' : 'starting');
      else this.retry();
    }
    if (this.win.isMinimized()) this.win.restore();
    this.win.show();
    this.win.focus();
    this.tray.destroy();
  }

  // ---- quitting ----

  shutdown() {
    if (this.shutdownPromise) return this.shutdownPromise;
    this.quitting = true;
    this.phase = 'stopping';
    this.generation++;
    this.stopPolling();
    this.sleepGuard.release();
    this.tray.destroy();
    for (const w of BrowserWindow.getAllWindows()) {
      if (!w.isDestroyed()) w.hide();
    }
    const server = this.server;
    this.shutdownPromise = (async () => {
      if (server) {
        try {
          await server.stop({ timeoutMs: STOP_TIMEOUT_MS });
        } catch (err) {
          this.log.error(`stopping the service: ${err.message}`);
        }
      }
      // Updates are installed here or not at all: the service, and every
      // task it ran, has stopped.
      if (this.updates) {
        this.updates.stop();
        this.installingUpdate = this.updates.quitAndInstall({ serviceStopped: !server || !server.alive });
      }
      this.log.info('shell stopped');
    })();
    return this.shutdownPromise;
  }
}

module.exports = { Shell, SHELL_ROOT, REPO_ROOT, PARTITION, appDataRoot };
