// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The settings page's "App" section (SPEC-v3 V11, V1.1, V1.3): the data
// folder (where, how big, move it, delete the old one), what closing the
// window does, keeping the computer awake, the update channel, running the
// first-run guide again, opening the logs and exporting diagnostics.
//
// Everything shown is what the back end returned: a change is sent at once
// and the section re-renders from the response (a failed save shows the
// stored value again). Desktop-only items say so in web mode.
//
// appRequest is a small fetch wrapper for the endpoints of this section and
// the About section (they need text and zip bodies, which api.js does not
// return); errors are ApiErrors with the translated text of api.js.

import { h, btn, skeleton, errorBlock, toast, confirmDialog, download } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { api, MOCK, MOCK_SCENARIO, ApiError, errorText } from './api.js';
import { createUpdateHint } from './update_hint.js';
import { createUpdateSection } from './settings_update.js';

function withMock(path) {
  if (!MOCK) return path;
  return path + (path.includes('?') ? '&' : '?') + 'mock=' + encodeURIComponent(MOCK_SCENARIO);
}

// appRequest(method, path, body?, {as: 'json'|'text'|'blob'}) → the body.
export async function appRequest(method, path, body, opts = {}) {
  const init = { method, cache: 'no-store', headers: {} };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(withMock(path), init);
  } catch {
    throw new ApiError(0, t('common.net.unreachable'));
  }
  if (!res.ok) {
    const text = await res.text().catch(() => '');
    let b = null;
    try {
      b = JSON.parse(text);
    } catch {
      b = null;
    }
    throw new ApiError(res.status, errorText(res.status, b), b);
  }
  if (opts.as === 'blob') return { blob: await res.blob(), name: fileNameOf(res.headers.get('Content-Disposition')) };
  const text = await res.text();
  if (opts.as === 'text') return text;
  if (res.status === 204 || !text) return null;
  try {
    return JSON.parse(text);
  } catch {
    throw new ApiError(res.status, t('common.net.bad_response'));
  }
}

function fileNameOf(cd) {
  const m = /filename="?([^";]+)"?/i.exec(cd || '');
  return m ? m[1] : '';
}

/**
 * "Export diagnostics" (POST /api/app/diagnostics): the zip handed to the
 * browser as a download, then a toast (while shown() says the page is
 * still there); a failure is toasted as an error. Settings → App and the
 * diagnostics page (r7, T24 G10) share it.
 */
export async function exportDiagnosticsFile(shown = () => true) {
  try {
    const { blob, name } = await appRequest('POST', '/api/app/diagnostics', undefined, { as: 'blob' });
    const url = URL.createObjectURL(blob);
    download(url, name || 'lectureflow-diagnostics.zip');
    setTimeout(() => URL.revokeObjectURL(url), 30000);
    if (shown()) toast(t('settings.app.diag_done'), 'success');
  } catch (e) {
    if (shown()) toast(e.message, 'error');
  }
}

// GET /api/app, shared by the App and About sections of one page.
let appInfoPromise = null;
export function loadAppInfo(fresh = false) {
  if (fresh || !appInfoPromise) {
    appInfoPromise = appRequest('GET', '/api/app').catch((e) => {
      appInfoPromise = null;
      throw e;
    });
  }
  return appInfoPromise;
}

const CLOSE_BEHAVIORS = ['ask', 'background', 'quit'];
const CHANNELS = ['stable', 'beta'];
const MOVING = new Set(['copying', 'verifying']);

// createAppSection({head, formRow, switchBtn, segmented, rerender}): the
// helpers of view_settings.js; rerender() redraws the block (renderBlock).
export function createAppSection(ui) {
  let alive = true;
  let app = null; // GET /api/app
  let error = '';
  let storage = null; // GET /api/app/storage
  let settings = null; // GET /api/app/settings
  let dd = null; // GET /api/app/data-dir (desktop)
  let old = null; // GET /api/app/old-data-dir (desktop)
  let saving = ''; // field being saved
  let saveError = '';
  let picking = false;
  let opening = false;
  let exporting = false;
  let deleting = false;
  let pollTimer = null;
  let restartTimer = null;
  let restarting = false;
  // SPEC-v3 V8: the update status next to the channel (desktop app only)
  const updateHint = createUpdateHint({ variant: 'settings' });
  // SPEC-ext 4: the in-app update rows after the channel (settings_update.js)
  const updUI = createUpdateSection(ui);

  const desktop = () => !!app && app.mode === 'desktop';
  const redraw = () => alive && ui.rerender();

  async function load() {
    updUI.load();
    try {
      const [a, s] = await Promise.all([loadAppInfo(true), appRequest('GET', '/api/app/settings')]);
      if (!alive) return;
      app = a;
      settings = s;
      error = '';
    } catch (e) {
      if (!alive) return;
      if (!app) error = e.message || t('common.load_failed');
    }
    redraw();
    loadStorage();
    if (desktop()) {
      loadDataDir();
      loadOld();
    }
  }

  async function loadStorage() {
    try {
      const s = await appRequest('GET', '/api/app/storage');
      if (!alive) return;
      storage = s;
      redraw();
    } catch {
      // the size line just stays empty
    }
  }

  async function loadDataDir() {
    try {
      const s = await appRequest('GET', '/api/app/data-dir');
      if (!alive) return;
      dd = s;
      redraw();
      if (dd && MOVING.has(dd.state)) schedulePoll();
      if (dd && dd.state === 'done' && dd.kind !== 'import') waitForRestart();
    } catch {
      // The server went away while copying or verifying: the move finished
      // and the server is restarting (exit code 3), or it stopped. Wait for
      // it to answer again, then reload.
      if (alive && dd && MOVING.has(dd.state)) waitForRestart(true);
    }
  }

  async function loadOld() {
    try {
      const o = await appRequest('GET', '/api/app/old-data-dir');
      if (!alive) return;
      old = o;
      redraw();
    } catch {
      old = null;
    }
  }

  function schedulePoll() {
    clearTimeout(pollTimer);
    pollTimer = setTimeout(loadDataDir, 800);
  }

  // After a finished move the server restarts (exit code 3); the desktop
  // shell starts it again and reloads the window. Reload ourselves when the
  // server answers again after having gone away (or after 60 s).
  function waitForRestart(away = false) {
    if (restarting) return;
    restarting = true;
    redraw();
    let wentAway = away;
    const started = Date.now();
    const tick = async () => {
      try {
        await appRequest('GET', '/api/health');
        if (wentAway || Date.now() - started > 60000) {
          location.reload();
          return;
        }
      } catch {
        wentAway = true;
      }
      restartTimer = setTimeout(tick, 1000);
    };
    restartTimer = setTimeout(tick, 1000);
  }

  // ---- actions ----

  async function patch(field, value) {
    if (saving) return;
    saving = field;
    saveError = '';
    redraw();
    try {
      const s = await appRequest('PATCH', '/api/app/settings', { [field]: value });
      if (!alive) return;
      settings = s;
    } catch (e) {
      if (!alive) return;
      saveError = e.message;
    }
    saving = '';
    redraw();
  }

  async function changeDir() {
    if (picking || !window.lectureflow || typeof window.lectureflow.pickFolder !== 'function') return;
    picking = true;
    redraw();
    let path = null;
    try {
      path = await window.lectureflow.pickFolder();
      if (!path || !alive) return;
      const ok = await confirmDialog({
        title: t('settings.app.move_title'),
        message: t('settings.app.move_intro', { path }),
        body: h(
          'ul',
          { class: 'app-move-points' },
          h('li', null, icon('copy', { size: 14 }), h('span', null, t('settings.app.move_point_copy'))),
          h('li', null, icon('shield-check', { size: 14 }), h('span', null, t('settings.app.move_point_verify'))),
          h('li', null, icon('hard-drive', { size: 14 }), h('span', null, t('settings.app.move_point_keep'))),
          h('li', null, icon('rotate-cw', { size: 14 }), h('span', null, t('settings.app.move_point_restart'))),
        ),
        okText: t('settings.app.move_ok'),
      });
      if (!ok) api.forgetFolderPick(path); // declined: the pick is given up, not left to expire
      if (!ok || !alive) return;
      const s = await appRequest('POST', '/api/app/data-dir', { path });
      if (!alive) return;
      dd = s && s.state ? s : { state: 'copying', copied_bytes: 0, total_bytes: 0, path };
      redraw();
      schedulePoll();
    } catch (e) {
      if (alive) toast(e.message, 'error');
      // The move was refused: the service returned the pick's grant, and
      // trying again asks for the folder again, so the pick is given up.
      if (path) api.forgetFolderPick(path);
    } finally {
      picking = false;
      redraw();
    }
  }

  async function deleteOld() {
    if (!old || !old.path || deleting) return;
    const ok = await confirmDialog({
      title: t('settings.app.delete_old_title'),
      message: t('settings.app.delete_old_confirm', { path: old.path, size: U.fmtBytes(old.bytes) }),
      okText: t('settings.app.delete_old_ok'),
      danger: true,
    });
    if (!ok || !alive) return;
    deleting = true;
    redraw();
    try {
      const o = await appRequest('DELETE', '/api/app/old-data-dir', { path: old.path });
      if (!alive) return;
      old = o;
      toast(t('settings.app.delete_old_done'), 'success');
      loadStorage();
    } catch (e) {
      if (alive) toast(e.message, 'error');
      loadOld();
    }
    deleting = false;
    redraw();
  }

  async function rerunOnboarding() {
    try {
      await appRequest('PUT', '/api/onboarding', { step: 'welcome', done: false, data: {} });
      location.hash = '#/onboarding';
    } catch (e) {
      toast(e.message, 'error');
    }
  }

  async function openLogs() {
    if (opening) return;
    opening = true;
    redraw();
    try {
      await appRequest('POST', '/api/app/open-logs');
    } catch (e) {
      if (alive) toast(e.message, 'error');
    }
    opening = false;
    redraw();
  }

  async function exportDiagnostics() {
    if (exporting) return;
    exporting = true;
    redraw();
    await exportDiagnosticsFile(() => alive);
    exporting = false;
    redraw();
  }

  // ---- render ----

  const note = (ic, text, cls = '') => h('p', { class: 'set-note' + (cls ? ' ' + cls : '') }, icon(ic, { size: 13 }), h('span', null, text));
  const desktopOnly = () => h('span', { class: 'chip chip-quiet app-desktop-only' }, icon('lock', { size: 12 }), t('settings.app.desktop_only'));

  function moveProgress() {
    if (!dd) return null;
    if (restarting || (dd.state === 'done' && dd.kind !== 'import')) {
      return note('loader-circle', t('settings.app.move_restarting'), 'app-move-status');
    }
    if (MOVING.has(dd.state)) {
      const total = dd.total_bytes || 0;
      const frac = total > 0 ? Math.min(1, (dd.copied_bytes || 0) / total) : 0;
      const text =
        dd.state === 'verifying'
          ? t('settings.app.move_verifying')
          : t('settings.app.move_copying', { copied: U.fmtBytes(dd.copied_bytes || 0), total: U.fmtBytes(total), percent: U.fmtPercent(frac, 0) });
      return h(
        'div',
        { class: 'app-move', role: 'status' },
        h('div', { class: 'app-move-text' }, icon('loader-circle', { size: 14, class: 'spin' }), h('span', null, text)),
        h('div', { class: 'meter meter-accent app-move-meter', role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(frac * 100)), 'aria-label': text }, h('div', { class: 'meter-fill', style: { width: `${(dd.state === 'verifying' ? 1 : frac) * 100}%` } })),
        h('p', { class: 'set-note' }, icon('info', { size: 13 }), h('span', null, t('settings.app.move_keep_open'))),
      );
    }
    if (dd.state === 'failed' && dd.kind !== 'import') {
      return note('circle-alert', t('settings.app.move_failed', { error: U.errorInfoText(dd.error, t('common.unknown_error')) }), 'is-warn');
    }
    return null;
  }

  function dataDirRow() {
    const dir = (storage && storage.data_dir) || app.data_dir || '';
    const usage = storage
      ? h(
          'span',
          { class: 'app-usage' },
          t('settings.app.usage', { used: U.fmtBytes(storage.used_bytes) }),
          storage.free_bytes >= 0 ? h('span', { class: 'dim' }, ' · ', t('settings.app.free', { free: U.fmtBytes(storage.free_bytes) })) : null,
        )
      : h('span', { class: 'app-usage dim' }, t('settings.app.usage_loading'));
    const busy = dd && (MOVING.has(dd.state) || dd.state === 'done') && dd.kind !== 'import';
    let action;
    if (!desktop()) action = h('div', { class: 'set-inline' }, desktopOnly(), h('span', { class: 'dim small' }, t('settings.app.data_dir_web')));
    else if (dd && dd.can_move === false) action = note('info', t('errors.data_dir.env_override'));
    else
      action = btn(picking ? t('settings.app.picking') : t('settings.app.change_dir'), {
        size: 'sm',
        icon: 'hard-drive',
        disabled: picking || busy,
        onclick: changeDir,
        fkey: 'set:app:move',
      });
    const oldRow =
      desktop() && old && old.path && old.exists
        ? h(
            'div',
            { class: 'app-old' },
            h('div', { class: 'app-old-text' }, icon('hard-drive', { size: 14 }), h('span', null, t('settings.app.old_dir', { size: U.fmtBytes(old.bytes) })), h('code', { class: 'app-path-sm mono', title: old.path }, old.path)),
            btn(deleting ? t('settings.app.deleting') : t('settings.app.delete_old'), { size: 'sm', kind: 'danger', icon: 'trash-2', disabled: deleting || busy, onclick: deleteOld, fkey: 'set:app:delold' }),
          )
        : null;
    return ui.formRow(
      t('settings.app.data_dir'),
      [h('code', { class: 'app-path mono', title: dir }, dir), usage],
      t('settings.app.data_dir_hint'),
      [action, moveProgress(), oldRow],
    );
  }

  function closeRow() {
    const options = CLOSE_BEHAVIORS.map((v) => [v, t(`settings.app.close.${v}`)]);
    const ctl = desktop()
      ? ui.segmented('set:app:close', options, settings.close_behavior, (v) => patch('close_behavior', v), t('settings.app.close_label'))
      : desktopOnly();
    return ui.formRow(t('settings.app.close_label'), ctl, t('settings.app.close_hint'), saving === 'close_behavior' ? savingNote() : null);
  }

  function sleepRow() {
    return ui.formRow(
      t('settings.app.sleep_label'),
      ui.switchBtn(settings.prevent_sleep, t('settings.app.sleep_label'), () => patch('prevent_sleep', !settings.prevent_sleep), 'set:app:sleep'),
      t('settings.app.sleep_hint'),
      saving === 'prevent_sleep' ? savingNote() : null,
    );
  }

  function channelRow() {
    const options = CHANNELS.map((v) => [v, t(`settings.app.channel.${v}`)]);
    const ctl = desktop()
      ? ui.segmented('set:app:channel', options, settings.update_channel, (v) => patch('update_channel', v), t('settings.app.channel_label'))
      : desktopOnly();
    return ui.formRow(t('settings.app.channel_label'), ctl, t('settings.app.channel_hint'), [saving === 'update_channel' ? savingNote() : null, desktop() ? updateHint.el : null]);
  }

  const savingNote = () => h('p', { class: 'set-note' }, icon('loader-circle', { size: 13, class: 'spin' }), h('span', null, t('settings.saving')));

  function onboardingRow() {
    return ui.formRow(
      t('settings.app.onboarding_label'),
      btn(t('settings.app.onboarding_rerun'), { size: 'sm', icon: 'rotate-cw', onclick: rerunOnboarding, fkey: 'set:app:onboarding' }),
      t('settings.app.onboarding_hint'),
    );
  }

  function diagRow() {
    return ui.formRow(
      t('settings.app.diag_label'),
      h(
        'div',
        { class: 'set-inline app-diag-btns' },
        btn(t('settings.app.open_logs'), { size: 'sm', icon: 'file-text', disabled: opening, onclick: openLogs, fkey: 'set:app:logs' }),
        btn(exporting ? t('settings.app.diag_exporting') : t('settings.app.diag_export'), { size: 'sm', icon: exporting ? 'loader-circle' : 'download', disabled: exporting, onclick: exportDiagnostics, fkey: 'set:app:diag' }),
      ),
      t('settings.app.diag_hint'),
      note('shield-check', t('settings.app.diag_privacy')),
    );
  }

  function render(shell) {
    shell.append(ui.head('settings', t('settings.app.title')));
    if (!app || !settings) {
      shell.append(h('div', { class: 'card-bd' }, error ? errorBlock(t('settings.app.load_error', { error }), retry) : skeleton(4)));
      return;
    }
    shell.append(
      h(
        'div',
        { class: 'card-bd set-form app-form' },
        saveError ? h('p', { class: 'set-note is-warn', role: 'alert' }, icon('circle-alert', { size: 13 }), h('span', null, saveError)) : null,
        dataDirRow(),
        closeRow(),
        sleepRow(),
        channelRow(),
        updUI.rows(),
        onboardingRow(),
        diagRow(),
      ),
    );
  }

  function retry() {
    error = '';
    redraw();
    load();
  }

  return {
    render,
    load,
    // SSE "changed settings": another window or the desktop shell changed them
    async onSettingsChanged() {
      updUI.onSettingsChanged();
      if (saving) return;
      try {
        const s = await appRequest('GET', '/api/app/settings');
        if (!alive) return;
        if (JSON.stringify(s) !== JSON.stringify(settings)) {
          settings = s;
          redraw();
        }
      } catch {
        // keep what is shown
      }
    },
    // SSE "changed update": the desktop main process reported a new update state
    onChanged(ev) {
      updateHint.onChanged(ev);
      updUI.onChanged(ev); // SPEC-ext 4: "changed selfupdate"
    },
    destroy() {
      alive = false;
      updateHint.destroy();
      updUI.destroy();
      clearTimeout(pollTimer);
      clearTimeout(restartTimer);
    },
  };
}
