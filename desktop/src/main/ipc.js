// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// IPC handlers behind the preload whitelist (v3-brief §3). Every handler
// first checks the sender (top frame of one of our windows, showing the
// local service) and then each argument (lib/validate.js).
'use strict';

const { app, BrowserWindow, dialog, ipcMain, Notification, powerMonitor, shell } = require('electron');

const { isServerUrl, isShellFileUrl } = require('./lib/security');
const { NOTIFY_LIMIT, createRateLimit } = require('./lib/ratelimit');
const {
  validateNoArgs, validatePickFiles, validateNotify, validateOpenExternal, validateDroppedPaths,
} = require('./lib/validate');

// Offline component packages (components.importFile). The archive format is
// the components manager's (internal/components); "All files" is offered too.
const COMPONENT_EXTENSIONS = ['zip'];

class SenderError extends Error {
  constructor() {
    super('IPC message from an untrusted sender');
    this.name = 'SenderError';
  }
}

/**
 * registerIpc(ctx) with ctx:
 *   getPort()            port of the running service (null otherwise)
 *   shellRoot            directory of the shell's own pages
 *   t                    translator
 *   showMainWindow()     bring the window to the front (notification click)
 *   onRetry(), onOpenLogs(), onRestoreDefault()   buttons of the start / error page
 *   openExternalLimit()  the shell's shared link limit (lib/ratelimit.js), also
 *                        applied to navigation and window.open (shell.js)
 *   grantPaths(paths, kind)  registers one-time grants with the service: 'file'
 *                        for paths the page may copy by path, 'folder' for the
 *                        folder dialog's pick (lib/pathgrant.js; never throws)
 *   log
 */
function registerIpc(ctx) {
  const notifications = new Set(); // keep references until closed
  const notifyLimit = createRateLimit(NOTIFY_LIMIT);
  let notifyDropping = false; // logged the current burst of dropped notifications
  const { openExternalLimit } = ctx;
  const { grantPaths } = ctx;
  if (typeof openExternalLimit !== 'function') throw new TypeError('registerIpc: ctx.openExternalLimit is required');
  if (typeof grantPaths !== 'function') throw new TypeError('registerIpc: ctx.grantPaths is required');

  function appWindow(event) {
    const frame = event.senderFrame;
    const win = BrowserWindow.fromWebContents(event.sender);
    if (!win || !frame || frame.parent !== null || !isServerUrl(frame.url, ctx.getPort())) {
      ctx.log.warn(`rejected IPC from ${frame ? frame.url.split(/[?#]/)[0].slice(0, 80) : 'unknown frame'}`);
      throw new SenderError();
    }
    return win;
  }

  function handle(channel, validate, fn) {
    ipcMain.handle(channel, async (event, ...args) => {
      const win = appWindow(event);
      const value = validate(args);
      return fn(value, win);
    });
  }

  handle('lf:pick-files', validatePickFiles, async (opts, win) => {
    const properties = ['openFile'];
    if (opts.multiple) properties.push('multiSelections');
    const r = await dialog.showOpenDialog(win, {
      title: opts.title || ctx.t('dialog.pick_files'),
      properties,
      filters: opts.filters.length ? opts.filters : undefined,
    });
    if (r.canceled) return [];
    // The page sends these paths back for a copy by path; the service takes
    // only granted ones (security audit FB1 L-7). Granted before the page
    // has them, so its upload never waits.
    await grantPaths(r.filePaths);
    return r.filePaths;
  });

  // The paths of a drop (the preload reads them with webUtils from the
  // dropped File objects, which page script cannot forge): granted like the
  // dialog's. The service waits a moment for a grant, since the page may
  // post the upload while this is on its way.
  handle('lf:grant-dropped', validateDroppedPaths, async (paths) => grantPaths(paths));

  handle('lf:pick-folder', validateNoArgs, async (_, win) => {
    const r = await dialog.showOpenDialog(win, { title: ctx.t('dialog.pick_folder'), properties: ['openDirectory', 'createDirectory'] });
    if (r.canceled || r.filePaths.length === 0) return null;
    // The data directory endpoints take a folder, local or on the network,
    // only when this dialog picked it (security review SEC24 R1): every
    // pick is granted.
    await grantPaths([r.filePaths[0]], 'folder');
    return r.filePaths[0];
  });

  // Lengths are checked by handle() (title ≤ 200, body ≤ 1000); here: not
  // more than NOTIFY_LIMIT (final security audit AUD2 L3). A dropped one
  // resolves false, like an unsupported one; one log line per burst.
  handle('lf:notify', validateNotify, async ({ title, body }) => {
    if (!notifyLimit()) {
      if (!notifyDropping) ctx.log.info('notification dropped: too many in a short time');
      notifyDropping = true;
      return false;
    }
    notifyDropping = false;
    if (!Notification.isSupported()) return false;
    const n = new Notification({ title, body });
    notifications.add(n);
    const forget = () => notifications.delete(n);
    n.on('click', () => {
      forget();
      ctx.showMainWindow();
    });
    n.on('close', forget);
    n.on('failed', forget);
    n.show();
    return true;
  });

  handle('lf:power-state', validateNoArgs, async () => ({ onBattery: powerMonitor.isOnBatteryPower() }));

  // The preload sends it only while the page has a user activation (a
  // click or key press); here: not more than the shared limit allows
  // (security audit FB1 L-6). The URL was checked by handle() already.
  handle('lf:open-external', validateOpenExternal, async (url) => {
    if (!openExternalLimit()) {
      ctx.log.warn('openExternal: too many links in a short time; refused');
      throw new Error('openExternal: too many links in a short time');
    }
    await shell.openExternal(url);
    return true;
  });

  handle('lf:app-version', validateNoArgs, async () => app.getVersion());

  handle('lf:components-import-file', validateNoArgs, async (_, win) => {
    const r = await dialog.showOpenDialog(win, {
      title: ctx.t('dialog.import_component'),
      properties: ['openFile'],
      filters: [
        { name: ctx.t('dialog.component_filter'), extensions: COMPONENT_EXTENSIONS },
        { name: ctx.t('dialog.all_files'), extensions: ['*'] },
      ],
    });
    if (r.canceled || r.filePaths.length === 0) return null;
    await grantPaths([r.filePaths[0]]); // POST /api/components/import {path}
    return r.filePaths[0];
  });

  // Start / error page (file://.../splash.html only).
  function fromShellPage(event) {
    const frame = event.senderFrame;
    return !!frame && frame.parent === null && isShellFileUrl(frame.url, ctx.shellRoot) && !!BrowserWindow.fromWebContents(event.sender);
  }
  ipcMain.on('lf-shell:retry', (event, ...args) => {
    if (!fromShellPage(event) || args.length !== 0) return;
    ctx.onRetry();
  });
  ipcMain.on('lf-shell:open-logs', (event, ...args) => {
    if (!fromShellPage(event) || args.length !== 0) return;
    ctx.onOpenLogs();
  });
  ipcMain.on('lf-shell:restore-default', (event, ...args) => {
    if (!fromShellPage(event) || args.length !== 0) return;
    ctx.onRestoreDefault();
  });
}

module.exports = { registerIpc, COMPONENT_EXTENSIONS };
