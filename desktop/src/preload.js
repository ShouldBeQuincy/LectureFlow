// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Preload (sandboxed, context-isolated). Exposes exactly the whitelist of
// v3-brief §3 as window.lectureflow on the app page, and two buttons'
// worth of API (window.lectureflowShell) on the shell's own start/error
// page. ipcRenderer itself is never exposed; the main process validates
// every argument and the sender of every message again.
'use strict';

const { contextBridge, ipcRenderer, webUtils } = require('electron');

const MAX_DROPPED = 1000;
const loc = window.location;

// A FileList crosses the context bridge as a plain object with the keys
// "0".."n-1" (no length); an array of File arrives as an array.
function toFileArray(files) {
  if (Array.isArray(files)) return files;
  if (files !== null && typeof files === 'object') {
    if (Number.isInteger(files.length)) return Array.from({ length: files.length }, (_, i) => files[i]);
    const keys = Object.keys(files);
    if (keys.every((k, i) => k === String(i))) return keys.map((k) => files[k]);
  }
  throw new TypeError('pathsForDroppedFiles: expected a FileList or an array of File');
}

function onlyFirst(arg) {
  // One argument at most crosses to the main process.
  return arg;
}

// The page's transient user activation (a click or key press within the
// last few seconds), as Chromium keeps it; false when it cannot be read.
function userActive() {
  const ua = typeof navigator === 'object' && navigator ? navigator.userActivation : null;
  return !!ua && ua.isActive === true;
}

// The page may send the paths of a drop back for a copy by path; the
// service takes only paths the main process granted (security audit FB1
// L-7, T-SEC-08). Only paths webUtils found for real dropped files are
// granted: page script reaches this world through the bridge alone. The
// page asks for one file at a time (web/api.js attachDroppedPaths), so the
// paths of one drop are collected and sent once, as one batch of grants.
let droppedPending = [];
function grantDropped(paths) {
  if (!paths.length) return;
  const first = droppedPending.length === 0;
  droppedPending.push(...paths);
  if (!first) return;
  Promise.resolve().then(() => {
    const all = droppedPending;
    droppedPending = [];
    for (let i = 0; i < all.length; i += MAX_DROPPED) {
      ipcRenderer.invoke('lf:grant-dropped', all.slice(i, i + MAX_DROPPED)).catch(() => {});
    }
  });
}

if (loc.protocol === 'http:' && loc.hostname === '127.0.0.1') {
  const api = {
    pickFiles: (opts) => ipcRenderer.invoke('lf:pick-files', onlyFirst(opts)),
    pathsForDroppedFiles: (files) => {
      // FileList or array of File from a drop event; the path comes from
      // webUtils (File.path no longer exists). Files without a path on disk
      // (made in the page) are left out.
      const list = toFileArray(files);
      if (list.length > MAX_DROPPED) throw new TypeError(`pathsForDroppedFiles: at most ${MAX_DROPPED} files`);
      const out = [];
      for (let i = 0; i < list.length; i++) {
        const f = list[i];
        if (!(f instanceof File)) throw new TypeError(`pathsForDroppedFiles: item ${i} is not a File`);
        const p = webUtils.getPathForFile(f);
        if (typeof p === 'string' && p !== '') out.push(p);
      }
      grantDropped(out);
      return out;
    },
    pickFolder: () => ipcRenderer.invoke('lf:pick-folder'),
    notify: (n) => ipcRenderer.invoke('lf:notify', onlyFirst(n)),
    getPowerState: () => ipcRenderer.invoke('lf:power-state'),
    openExternal: (url) => {
      // Only right after a click or key press of the person (security
      // audit FB1 L-6). navigator.userActivation is read here, in the
      // preload's isolated world: script in the page can neither replace
      // it for this world nor create an activation without real input. The
      // main process limits how often it opens links (lib/ratelimit.js).
      if (!userActive()) return Promise.reject(new Error('openExternal: only from a click or key press'));
      return ipcRenderer.invoke('lf:open-external', onlyFirst(url));
    },
    getAppVersion: () => ipcRenderer.invoke('lf:app-version'),
    components: Object.freeze({
      importFile: () => ipcRenderer.invoke('lf:components-import-file'),
    }),
  };
  contextBridge.exposeInMainWorld('lectureflow', Object.freeze(api));
} else if (loc.protocol === 'file:') {
  contextBridge.exposeInMainWorld('lectureflowShell', Object.freeze({
    retry: () => ipcRenderer.send('lf-shell:retry'),
    openLogs: () => ipcRenderer.send('lf-shell:open-logs'),
    restoreDefault: () => ipcRenderer.send('lf-shell:restore-default'),
  }));
}
