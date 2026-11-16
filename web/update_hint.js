// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The application update hint (SPEC-v3 V8): a quiet line at the bottom of
// the dashboard and next to the update channel in the settings. Desktop
// app only (the main process runs electron-updater and relays its state
// through GET /api/app/update, internal/api/handlers_update.go).
//
//   available    "有新版本 x.y.z 可用"  [下载]      — nothing downloads before the click
//                (macOS: "到发布页面下载", with a link when GET /api/app has one)
//   downloading  "正在下载新版本 x.y.z" + progress
//   ready        "新版本 x.y.z 已下载"  [退出时安装] — never installed while tasks run:
//                the main process installs only when the app quits, after
//                every task has stopped; with tasks running the line says so
//   error        a failed download offers "retry"; a failed check is shown
//                in the settings only
//
// No dialog, no badge, no repeated reminder. The line re-reads the status
// on SSE "changed update" (onChanged), every 1.5 s while downloading and
// every 10 minutes otherwise; it shows only what the server returned.
//
// SPEC-ext 4 (in-app update without our own server, settings_update.js):
// when GET /api/selfupdate says it is configured, it decides what the
// workbench line says, and the settings variant stays empty (the App
// section shows the update rows); otherwise the electron-updater relay as
// above. With the in-app update the workbench says:
//
//   available    "有新版本 x.y.z 可用"  [下载] (POST /api/selfupdate/download);
//                a version that cannot be downloaded here (too old,
//                no package) points to its release page
//   downloading  "正在下载新版本 x.y.z" + progress
//   pending      "新版本 x.y.z 已下载，下次启动时安装"
//   error        a failed download offers "retry"; a failed check is shown
//                in the settings only (an update found before or downloaded
//                stays available / pending: a failed check never hides it)
//   and, for a week after it happened unless dismissed, a failed or rolled
//   back installation (result.json) with a link to the settings.

import { h, clear, btn } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { api } from './api.js';
import { externalLinkFailed } from './onboarding_api.js';
import { loadStatus as loadSelfUpdate, configured, recentFailure, resultText, downloadError, suRequest } from './settings_update.js';

const IDLE_MS = 10 * 60 * 1000;
const BUSY_MS = 1500;

// GET /api/app once per page load: the mode does not change while it runs.
let appPromise = null;
function appInfo() {
  if (!appPromise) {
    appPromise = api.appInfo({ quiet: true }).then(
      (a) => a || {},
      () => {
        appPromise = null;
        return {};
      },
    );
  }
  return appPromise;
}

// The release page of the app, when GET /api/app names one (the field name
// is not frozen yet: release_url, releases_url or download_url).
function releaseURL(a) {
  const u = a && (a.release_url || a.releases_url || a.download_url);
  return typeof u === 'string' && /^https:\/\/\S+$/i.test(u) ? u : '';
}

function openExternal(url) {
  const lf = window.lectureflow;
  if (lf && typeof lf.openExternal === 'function') Promise.resolve(lf.openExternal(url)).catch(externalLinkFailed);
}

// The failed installation the user dismissed on the workbench (its time);
// a per-window convenience, so storage may be missing.
const SEEN_KEY = 'lf.selfupdate.seen_result';
function seenResult() {
  try {
    return localStorage.getItem(SEEN_KEY) || '';
  } catch {
    return '';
  }
}
function markSeen(at) {
  try {
    localStorage.setItem(SEEN_KEY, at);
  } catch {
    // the line comes back after a reload; nothing else depends on it
  }
}

/**
 * createUpdateHint({variant: 'dashboard'|'settings'}) → {el, refresh(),
 * onChanged(ev), destroy()}. el stays empty (and hidden) in web mode and
 * while there is nothing to say; it can be re-appended after a re-render
 * (the node and its state survive).
 */
export function createUpdateHint({ variant = 'dashboard' } = {}) {
  const settings = variant === 'settings';
  const el = h('div', { class: `upd-hint upd-${variant}`, role: 'status', hidden: true });
  let alive = true;
  let desktop = false;
  let app = {}; // GET /api/app
  let st = null; // UpdateStatus
  // GET /api/selfupdate while the in-app update is configured (SPEC-ext 4);
  // null: the electron-updater relay above
  let su = null;
  let refreshing = false;
  let again = false;
  let running = 0; // jobs running or queued (GET /api/app/activity)
  let acting = false;
  let actError = '';
  let timer = null;

  // One refresh at a time; a request during one runs once more afterwards.
  async function refresh() {
    if (refreshing) {
      again = true;
      return;
    }
    refreshing = true;
    try {
      await refreshNow();
    } finally {
      refreshing = false;
      if (again && alive) {
        again = false;
        refresh();
      }
    }
  }

  async function refreshNow() {
    clearTimeout(timer);
    if (!alive) return;
    app = await appInfo();
    desktop = app.mode === 'desktop';
    if (!alive) return;
    if (!desktop) {
      render();
      return;
    }
    // SPEC-ext 4: the in-app update decides when it is configured
    try {
      const s = await loadSelfUpdate();
      su = configured(s) ? s : null;
    } catch {
      // keep what is shown; try again later
    }
    if (!alive) return;
    if (su) {
      render();
      timer = setTimeout(refresh, ['checking', 'downloading', 'importing'].includes(su.state) ? BUSY_MS : IDLE_MS);
      return;
    }
    try {
      st = await api.appUpdate({ quiet: true });
    } catch {
      // keep what is shown; try again later
    }
    if (alive && st && st.state === 'ready') {
      try {
        const a = await api.appActivity({ quiet: true });
        running = a ? (a.running || 0) + (a.queued || 0) : 0;
      } catch {
        running = 0;
      }
    }
    if (!alive) return;
    render();
    timer = setTimeout(refresh, st && (st.state === 'downloading' || st.download_requested) ? BUSY_MS : IDLE_MS);
  }

  async function act(fn) {
    if (acting) return;
    acting = true;
    actError = '';
    render();
    try {
      const s = await fn();
      if (s && s.state) {
        if (su) su = s;
        else st = s;
      }
    } catch (e) {
      actError = e.message || t('common.unknown_error');
    }
    acting = false;
    if (alive) refresh();
  }

  const download = () => act(() => (su ? suRequest('POST', '/api/selfupdate/download') : api.downloadUpdate({ quiet: true })));
  const installOnQuit = (on) => act(() => api.installUpdateOnQuit(on, { quiet: true }));

  function percent() {
    if (!st) return 0;
    const p = typeof st.percent === 'number' ? st.percent / 100 : typeof st.progress === 'number' ? st.progress : 0;
    return Math.max(0, Math.min(1, p));
  }

  function line(ic, text, tools, extra, spin = false) {
    return [
      h(
        'div',
        { class: 'upd-line' },
        icon(ic, { size: 14, class: spin ? 'spin' : '' }),
        h('span', { class: 'upd-text' }, text),
        tools && tools.length ? h('span', { class: 'upd-tools' }, tools) : null,
      ),
      extra,
      actError ? h('p', { class: 'upd-sub is-warn', role: 'alert' }, actError) : null,
    ];
  }

  // The in-app update (SPEC-ext 4): the workbench line only; the settings
  // page shows the whole status in its App section (settings_update.js).
  function suContent() {
    if (settings) return null;
    const a = su.available;
    let main = null;
    switch (su.state) {
      case 'available':
        if (!a) break;
        if (a.direct && a.file) {
          main = line('download', t('update.available', { version: a.version }), [
            btn(t('update.download'), { size: 'sm', kind: 'ghost', icon: 'download', disabled: acting, onclick: download, fkey: `upd:${variant}:download` }),
          ]);
        } else {
          const url = typeof a.notes_url === 'string' && /^https:\/\/\S+$/i.test(a.notes_url) ? a.notes_url : '';
          main = line('download', t(url ? 'update.available_manual' : 'update.available_manual_nolink', { version: a.version }), url
            ? [btn(t('update.open_release'), { size: 'sm', kind: 'ghost', icon: 'external-link', onclick: () => openExternal(url), fkey: `upd:${variant}:release` })]
            : null);
        }
        break;
      case 'downloading': {
        const p = su.progress;
        const frac = p && p.bytes_total > 0 ? Math.max(0, Math.min(1, p.bytes_done / p.bytes_total)) : 0;
        main = line(
          'loader-circle',
          t('update.downloading', { version: a ? a.version : '', percent: U.fmtPercent(frac, 0) }),
          null,
          h('div', { class: 'meter meter-accent upd-meter', role: 'progressbar', 'aria-label': t('update.progress_label'), 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(frac * 100)) }, h('div', { class: 'meter-fill', style: { width: `${frac * 100}%` } })),
          true,
        );
        break;
      }
      case 'pending':
        if (su.pending) main = line('circle-check', t('update.pending', { version: su.pending.version }), null);
        break;
      case 'error':
        // a failed check is not worth a line on the workbench (and does not
        // hide an update: the server keeps the state available or pending
        // then, with the failure as a note in the settings)
        if (downloadError(su) && a && a.direct && a.file) {
          main = line('circle-alert', t('update.download_failed', { version: a.version }), [
            btn(t('common.retry'), { size: 'sm', kind: 'ghost', icon: 'rotate-cw', disabled: acting, onclick: download, fkey: `upd:${variant}:retry` }),
          ]);
        }
        break;
      default:
        break;
    }
    // a failed or rolled back installation, for a week unless dismissed
    const r = recentFailure(su);
    const failure =
      r && r.at !== seenResult()
        ? h(
            'div',
            { class: 'set-note is-warn upd-failed' },
            icon('triangle-alert', { size: 13 }),
            h('span', { class: 'upd-text' }, resultText(r)),
            h(
              'span',
              { class: 'upd-tools' },
              btn(t('update.view_settings'), { size: 'sm', kind: 'ghost', onclick: () => (location.hash = '#/settings/app'), fkey: `upd:${variant}:result` }),
              btn(t('update.dismiss'), {
                size: 'sm',
                kind: 'quiet',
                onclick: () => {
                  markSeen(r.at);
                  render();
                },
                fkey: `upd:${variant}:dismiss`,
              }),
            ),
          )
        : null;
    if (!main && !failure) return null;
    return [...(main || []), failure];
  }

  function content() {
    if (su) return desktop ? suContent() : null;
    if (!desktop || !st) return null;
    const v = st.version || '';
    switch (st.state) {
      case 'available':
        // macOS: an unsigned build cannot update itself; the new version is
        // downloaded from the release page by hand
        if (app.platform === 'darwin') {
          const url = releaseURL(app);
          return line('download', t(url ? 'update.available_manual' : 'update.available_manual_nolink', { version: v }), url
            ? [btn(t('update.open_release'), { size: 'sm', kind: 'ghost', icon: 'external-link', onclick: () => openExternal(url), fkey: `upd:${variant}:release` })]
            : null);
        }
        if (st.download_requested) return line('loader-circle', t('update.preparing', { version: v }), null, null, true);
        return line('download', t('update.available', { version: v }), [
          btn(t('update.download'), { size: 'sm', kind: 'ghost', icon: 'download', disabled: acting, onclick: download, fkey: `upd:${variant}:download` }),
        ]);
      case 'downloading': {
        const p = percent();
        return line(
          'loader-circle',
          t('update.downloading', { version: v, percent: U.fmtPercent(p, 0) }),
          null,
          h('div', { class: 'meter meter-accent upd-meter', role: 'progressbar', 'aria-label': t('update.progress_label'), 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(p * 100)) }, h('div', { class: 'meter-fill', style: { width: `${p * 100}%` } })),
          true,
        );
      }
      case 'ready': {
        const sub = running > 0 ? h('p', { class: 'upd-sub' }, t('update.tasks_running')) : null;
        if (st.install_on_quit) {
          return line('circle-check', t('update.ready_on_quit', { version: v }), [
            btn(t('update.not_now'), { size: 'sm', kind: 'quiet', disabled: acting, onclick: () => installOnQuit(false), fkey: `upd:${variant}:undo` }),
          ], sub);
        }
        return line('circle-check', t('update.ready', { version: v }), [
          btn(t('update.install_on_quit'), { size: 'sm', kind: 'ghost', icon: 'check', disabled: acting, onclick: () => installOnQuit(true), fkey: `upd:${variant}:quit` }),
        ], sub);
      }
      case 'error': {
        const code = st.error && st.error.code;
        if (code === 'update.download_failed' && v) {
          return line('circle-alert', t('update.download_failed', { version: v }), [
            btn(t('common.retry'), { size: 'sm', kind: 'ghost', icon: 'rotate-cw', disabled: acting, onclick: download, fkey: `upd:${variant}:retry` }),
          ]);
        }
        // a failed check (or install) is not worth a line on the dashboard
        if (!settings) return null;
        return line('circle-alert', U.errorInfoText(st.error, t('update.check_failed')), null);
      }
      default:
        return null; // none
    }
  }

  function render() {
    if (!alive) return;
    const c = content();
    clear(el);
    el.dataset.state = (su ? su.state : st && st.state) || '';
    if (!c) {
      el.hidden = true;
      return;
    }
    el.hidden = false;
    el.append(...c.filter(Boolean));
  }

  refresh();

  return {
    el,
    refresh,
    // SSE "changed update" (the main process reported a new state) and
    // "changed selfupdate" (the in-app update, SPEC-ext 4)
    onChanged(ev) {
      if (ev && (ev.kind === 'update' || ev.kind === 'selfupdate')) refresh();
    },
    destroy() {
      alive = false;
      clearTimeout(timer);
    },
  };
}
